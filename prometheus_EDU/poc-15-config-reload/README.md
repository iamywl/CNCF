# PoC-15: Config Reload (설정 리로드 메커니즘)

## 개요

Prometheus의 설정 리로드 메커니즘을 시뮬레이션한다. Prometheus는 무중단으로 설정을 변경할 수 있으며, 3가지 트리거와 순서화된 reloader 체인을 통해 안전한 설정 반영을 보장한다.

## 실제 소스코드 참조

| 구성 요소 | 파일 위치 |
|----------|----------|
| reloadConfig() 함수 | `cmd/prometheus/main.go:1604` |
| reloader 구조체 및 목록 | `cmd/prometheus/main.go:1028-1107` |
| SIGHUP/HTTP/Auto 트리거 루프 | `cmd/prometheus/main.go:1264-1347` |
| GenerateChecksum() | `config/reload.go:33` |
| configSuccess 메트릭 | `cmd/prometheus/main.go` (configSuccess, configSuccessTime 게이지) |

## 핵심 개념

### 1. 3가지 리로드 트리거

Prometheus는 세 가지 방법으로 설정 리로드를 트리거할 수 있다:

```
┌─────────────┐     ┌──────────────┐     ┌───────────────────┐
│   SIGHUP    │     │ HTTP POST    │     │ Auto-Reload       │
│ (kill -HUP) │     │ /-/reload    │     │ (체크섬 폴링)      │
└──────┬──────┘     └──────┬───────┘     └────────┬──────────┘
       │                   │                      │
       └───────────────────┼──────────────────────┘
                           │
                    ┌──────▼──────┐
                    │ reloadConfig│  ← 모두 같은 함수 호출
                    └──────┬──────┘
                           │
              ┌────────────▼────────────┐
              │ reloader 체인 순서 실행   │
              └─────────────────────────┘
```

- **SIGHUP**: `kill -HUP <pid>` 또는 `systemctl reload prometheus`
- **HTTP POST /-/reload**: `--web.enable-lifecycle` 플래그 필요
- **자동 리로드**: `--config.auto-reload-interval` 설정 시 활성화, SHA256 체크섬으로 변경 감지

### 2. Reloader 순서

설정 변경 시 서브시스템에 적용하는 순서가 중요하다:

```
1. db_storage       ← 스토리지 엔진 설정 (retention 등)
2. remote_storage   ← remote_write/read 엔드포인트
3. web_handler      ← 외부 URL, CORS 설정
4. query_engine     ← 쿼리 로그 설정
5. scrape           ← 스크레이프 매니저 설정
6. scrape_sd        ← 스크레이프 서비스 디스커버리
7. notify           ← Alertmanager 알림 설정
8. notify_sd        ← 알림 서비스 디스커버리
9. rules            ← 룰 매니저 (룰 파일 로드 및 평가 주기)
```

**순서가 중요한 이유**: scrape/notify 매니저가 discovery 매니저보다 먼저 설정을 받아야 한다. 새로운 타겟 목록이 도착했을 때, 매니저가 이미 최신 설정을 갖고 있어야 올바르게 처리할 수 있기 때문이다.

### 3. Partial Failure 처리

```go
// cmd/prometheus/main.go:1631-1641
failed := false
for _, rl := range rls {
    if err := rl.reloader(conf); err != nil {
        logger.Error("Failed to apply configuration", "err", err)
        failed = true  // 실패 마킹하지만 계속 진행
    }
}
```

하나의 reloader가 실패해도 나머지 reloader는 계속 실행된다. 단, 전체 리로드 결과는 실패로 마킹되어 `configSuccess` 메트릭이 0이 된다.

### 4. 체크섬 기반 변경 감지

`config/reload.go`의 `GenerateChecksum()`은 단순히 설정 파일만 해싱하는 것이 아니라, 참조하는 rule_files와 scrape_config_files까지 포함하여 SHA256 해시를 생성한다. 체크섬 업데이트는 리로드 성공 시에만 수행되므로, 실패하면 다음 폴링 주기에서 다시 시도한다.

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. **초기 설정 로드**: JSON 설정 파일을 생성하고 reloader 체인을 통해 적용
2. **자동 리로드**: 설정 파일 변경 → 체크섬 비교 → 변경 감지 시 자동 리로드
3. **HTTP 리로드**: `/-/reload` 엔드포인트를 통한 리로드 (채널 기반 비동기 처리)
4. **Reloader 실패**: rule_manager 실패 시뮬레이션 → partial failure 동작 확인
5. **SIGHUP 리로드**: 프로세스에 SIGHUP 신호 전송 → 리로드 트리거
6. **통합 루프**: 3가지 트리거가 하나의 select 루프에서 동작하는 구조 재현
7. **실제 HTTP 서버**: `localhost:9191/-/reload`에서 POST 요청 처리

## 설계 포인트

| 포인트 | 설명 |
|--------|------|
| 단일 함수 | 3가지 트리거 모두 `reloadConfig()` 하나를 호출하여 일관성 보장 |
| 순서 보장 | reloader 슬라이스의 순서대로 실행, 서브시스템 간 의존성 반영 |
| Partial failure | 하나가 실패해도 나머지 계속 실행, 전체 결과만 실패로 마킹 |
| 체크섬 타이밍 | 성공 시에만 업데이트 → 실패하면 다음 주기에 재시도 |
| 메트릭 | `config_last_reload_successful` (0/1), `config_last_reload_success_timestamp_seconds` |
