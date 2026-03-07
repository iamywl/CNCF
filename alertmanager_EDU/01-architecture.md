# Alertmanager 아키텍처

## 1. 개요

Prometheus Alertmanager는 Prometheus 서버 등 클라이언트로부터 알림(Alert)을 수신하여 **중복 제거**, **그룹핑**, **라우팅**, **전송**을 담당하는 독립 서비스이다. 핵심 설계 원칙은 다음과 같다:

- **선언적 설정**: YAML 기반 라우팅 트리로 알림 흐름 정의
- **파이프라인 아키텍처**: Stage 체인으로 알림 필터링/전송 처리
- **고가용성**: Gossip 프로토콜로 다중 인스턴스 간 상태 동기화
- **플러그인 확장**: Receiver 인터페이스로 다양한 알림 채널 지원

## 2. 전체 아키텍처

```
                    ┌─────────────────┐
                    │   Prometheus    │
                    │   (or client)   │
                    └────────┬────────┘
                             │ POST /api/v2/alerts
                             ▼
┌─────────────────────────────────────────────────────────────┐
│                      Alertmanager                           │
│                                                             │
│  ┌──────────┐    ┌──────────────┐    ┌──────────────────┐  │
│  │  API v2  │───→│   Provider   │───→│   Dispatcher     │  │
│  │(go-swagger)│  │  (mem.Alerts)│    │                  │  │
│  └──────────┘    └──────────────┘    │  ┌────────────┐  │  │
│       │                              │  │ Route 트리 │  │  │
│       │          ┌──────────────┐    │  │  매칭       │  │  │
│       ├─────────→│   Silences   │    │  └─────┬──────┘  │  │
│       │          └──────────────┘    │        │         │  │
│       │          ┌──────────────┐    │  ┌─────▼──────┐  │  │
│       ├─────────→│  Inhibitor   │    │  │ Aggregation│  │  │
│       │          └──────────────┘    │  │   Group    │  │  │
│       │                              │  └─────┬──────┘  │  │
│       │                              └────────┼─────────┘  │
│       │                                       │            │
│       │                              ┌────────▼─────────┐  │
│       │                              │  Notification    │  │
│       │                              │  Pipeline        │  │
│       │                              │                  │  │
│       │                              │ ┌──────────────┐ │  │
│       │                              │ │ MuteStage    │ │  │
│       │                              │ │ (Silence,    │ │  │
│       │                              │ │  Inhibition) │ │  │
│       │                              │ └──────┬───────┘ │  │
│       │                              │ ┌──────▼───────┐ │  │
│       │                              │ │ WaitStage    │ │  │
│       │                              │ └──────┬───────┘ │  │
│       │                              │ ┌──────▼───────┐ │  │
│       │                              │ │ DedupStage   │ │  │
│       │                              │ │ (nflog 참조) │ │  │
│       │                              │ └──────┬───────┘ │  │
│       │                              │ ┌──────▼───────┐ │  │
│       │                              │ │ RetryStage   │ │  │
│       │                              │ │→ Integration │ │  │
│       │                              │ │ (Slack,Email)│ │  │
│       │                              │ └──────────────┘ │  │
│       │                              └──────────────────┘  │
│       │                                                     │
│  ┌────▼─────┐     ┌─────────┐     ┌──────────┐            │
│  │  Marker   │     │  nflog  │     │ Cluster  │            │
│  │(상태추적) │     │(알림로그)│←───→│(Gossip)  │            │
│  └──────────┘     └─────────┘     └──────────┘            │
│                                        ↕                   │
│                               ┌──────────────┐            │
│                               │ 다른 AM 인스턴스│           │
│                               └──────────────┘            │
└─────────────────────────────────────────────────────────────┘
```

## 3. 핵심 컴포넌트

### 3.1 API 계층

| 컴포넌트 | 파일 | 역할 |
|----------|------|------|
| API | `api/api.go` | HTTP 라우터 통합, 동시성 제한 |
| API v2 | `api/v2/api.go` | OpenAPI/go-swagger 기반 REST 핸들러 |
| V1 Deprecation | `api/v1_deprecation_router.go` | v1 API 호환성 (deprecated) |

API v2는 다음 엔드포인트를 제공한다:
- `POST /api/v2/alerts` — 알림 수신
- `GET /api/v2/alerts` — 알림 조회
- `GET /api/v2/alerts/groups` — 알림 그룹 조회
- `POST /api/v2/silences` — Silence 생성
- `GET /api/v2/silences` — Silence 조회
- `DELETE /api/v2/silence/{silenceID}` — Silence 삭제
- `GET /api/v2/status` — 상태 조회
- `GET /api/v2/receivers` — Receiver 목록

### 3.2 Alert 저장소

| 컴포넌트 | 파일 | 역할 |
|----------|------|------|
| Provider 인터페이스 | `provider/provider.go` | Alert 저장/조회/구독 인터페이스 |
| 메모리 구현 | `provider/mem/mem.go` | 인메모리 Alert 저장소 |
| 내부 저장소 | `store/store.go` | fingerprint → Alert 맵 |

### 3.3 라우팅 및 디스패치

| 컴포넌트 | 파일 | 역할 |
|----------|------|------|
| Dispatcher | `dispatch/dispatch.go` | Alert 수집, 라우팅, 그룹핑 |
| Route | `dispatch/route.go` | 라우팅 트리, 매칭 로직 |

### 3.4 알림 파이프라인

| 컴포넌트 | 파일 | 역할 |
|----------|------|------|
| Stage 인터페이스 | `notify/notify.go` | 알림 처리 단계 체인 |
| MuteStage | `notify/mute.go` | Silence/Inhibition 필터링 |
| Integrations | `notify/impl/` | Slack, Email, PagerDuty 등 구현 |

### 3.5 알림 억제

| 컴포넌트 | 파일 | 역할 |
|----------|------|------|
| Silencer | `silence/silence.go` | 레이블 매칭 기반 침묵 |
| Inhibitor | `inhibit/inhibit.go` | Source→Target 규칙 기반 억제 |

### 3.6 클러스터링

| 컴포넌트 | 파일 | 역할 |
|----------|------|------|
| Peer | `cluster/cluster.go` | Gossip 프로토콜, 피어 관리 |
| Channel | `cluster/channel.go` | 상태 브로드캐스트 채널 |

### 3.7 알림 로그

| 컴포넌트 | 파일 | 역할 |
|----------|------|------|
| Log | `nflog/nflog.go` | 알림 발송 기록, 중복 방지 |

## 4. 진입점 및 초기화 흐름

### 4.1 main() → run()

진입점은 `cmd/alertmanager/main.go`이다. `main()`이 `run()`을 호출하며, `run()` 함수가 모든 초기화를 수행한다.

```
main()
  └→ run()
      ├→ 1. 플래그 파싱 (kingpin)
      │    - --config.file, --storage.path
      │    - --cluster.*, --web.*
      │    - --log.level, --log.format
      │
      ├→ 2. Logger 생성
      │
      ├→ 3. Feature Flags 초기화
      │    - featurecontrol.NewFlags()
      │
      ├→ 4. Cluster Peer 생성 (선택)
      │    - cluster.Create()
      │    - 피어 목록 설정, 바인드 주소
      │
      ├→ 5. Notification Log (nflog) 생성
      │    - nflog.New()
      │    - 스냅샷 파일 로드
      │    - cluster.AddState("nfl", nlog)
      │
      ├→ 6. AlertMarker, GroupMarker 생성
      │    - types.NewMarker()
      │
      ├→ 7. Silences 생성
      │    - silence.New()
      │    - 스냅샷 파일 로드
      │    - cluster.AddState("sil", silences)
      │
      ├→ 8. Alert Provider 생성
      │    - mem.NewAlerts()
      │
      ├→ 9. Inhibitor 생성
      │    - inhibit.NewInhibitor()
      │
      ├→ 10. TimeInterval Intervener 생성
      │    - timeinterval.NewIntervener()
      │
      ├→ 11. Notification Pipeline 구축
      │    - RoutingStage 빌드
      │    - MuteStage, WaitStage, DedupStage
      │    - RetryStage, SetNotifiesStage
      │
      ├→ 12. Dispatcher 생성 및 시작
      │    - dispatch.NewDispatcher()
      │    - dispatcher.Run()
      │
      ├→ 13. API 생성 및 HTTP 서버 시작
      │    - api.New()
      │    - api.Register()
      │
      ├→ 14. Config Coordinator 설정
      │    - config.NewCoordinator()
      │    - coord.Subscribe(disp, inhibitor, api, ...)
      │    - coord.Reload()
      │
      ├→ 15. Cluster Join
      │    - peer.Join()
      │    - peer.Settle()
      │
      └→ 16. 시그널 핸들러 (SIGHUP → Reload, SIGTERM → Shutdown)
```

### 4.2 핵심 goroutine

```
┌─────────────────────────────────────────┐
│           main goroutine                 │
│  - HTTP 서버                             │
│  - 시그널 핸들러                          │
├─────────────────────────────────────────┤
│           Dispatcher goroutine           │
│  - Alert 수집 워커 (concurrency 설정)    │
│  - Aggregation Group 유지보수            │
├─────────────────────────────────────────┤
│           Inhibitor goroutine            │
│  - Alert 변경 감시                       │
│  - Source alert 캐시 업데이트            │
├─────────────────────────────────────────┤
│           nflog Maintenance goroutine    │
│  - GC (만료 엔트리 삭제)                 │
│  - 스냅샷 저장                           │
├─────────────────────────────────────────┤
│           Silences Maintenance goroutine │
│  - GC (만료 Silence 삭제)               │
│  - 스냅샷 저장                           │
├─────────────────────────────────────────┤
│           Provider GC goroutine          │
│  - 만료 Alert 삭제                       │
│  - Marker 정리                           │
├─────────────────────────────────────────┤
│           Cluster goroutine (선택)       │
│  - Gossip 프로토콜                       │
│  - 피어 상태 관리                        │
│  - State 동기화 (nflog, silences)        │
└─────────────────────────────────────────┘
```

## 5. 설정 리로드 메커니즘

Alertmanager는 **SIGHUP** 시그널 또는 **`/-/reload` 엔드포인트**로 설정을 동적 리로드한다.

```
SIGHUP / /-/reload
        │
        ▼
  Coordinator.Reload()
        │
        ├→ config.LoadFile()     // YAML 파싱
        │
        ├→ 유효성 검증
        │   - Route 트리 검증
        │   - Receiver 중복 확인
        │   - Template 파일 존재 확인
        │
        └→ 구독자 콜백 호출 (순서대로)
            ├→ 1. Template 재로드
            ├→ 2. Inhibitor 재구성
            │     - 기존 Inhibitor 중지
            │     - 새 InhibitRule로 Inhibitor 생성
            │     - inhibitor.Run()
            ├→ 3. TimeInterval Intervener 재구성
            ├→ 4. Silencer 재구성
            ├→ 5. Notification Pipeline 재구축
            │     - RoutingStage 재생성
            │     - 각 Receiver별 Stage 체인
            ├→ 6. Dispatcher 재시작
            │     - 기존 Dispatcher 중지
            │     - 새 Route 트리로 Dispatcher 생성
            │     - dispatcher.Run()
            └→ 7. API 업데이트
                  - api.Update(config)
```

`config/coordinator.go`의 `Coordinator`가 이 프로세스를 관리한다:

```go
type Coordinator struct {
    configFilePath string
    mutex          sync.Mutex
    config         *Config
    subscribers    []func(*Config) error
}
```

## 6. 데이터 흐름 요약

```
[Alert 수신]
    │
    ▼
Provider.Put()          // 메모리에 저장, fingerprint 키
    │
    ├→ PreStore 콜백      // 검증, 제한 확인
    ├→ Set (store.Alerts) // fingerprint → Alert 맵
    └→ 리스너 브로드캐스트  // Dispatcher에게 알림

[Dispatcher 수신]
    │
    ▼
routeAlert()            // Route 트리 DFS 매칭
    │
    ├→ 매칭 Route 발견
    │   └→ groupAlert() // Aggregation Group에 추가
    │       └→ group_by 레이블로 그룹 키 생성
    │
    └→ 매칭 실패 → 루트 Route의 Receiver로 전달

[Aggregation Group]
    │
    ├→ GroupWait 타이머   // 첫 알림 대기
    ├→ GroupInterval 타이머 // 후속 알림 간격
    └→ flush()
        │
        ▼
    Notification Pipeline
        │
        ├→ GossipSettleStage  // 클러스터 안정화 대기
        ├→ MuteStage          // Silence/Inhibition 체크
        ├→ WaitStage          // RepeatInterval 대기
        ├→ DedupStage         // nflog 참조, 중복 방지
        ├→ RetryStage         // Integration 호출 + 재시도
        └→ SetNotifiesStage   // nflog 기록
```

## 7. 고가용성 아키텍처

```
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│ Alertmanager │  │ Alertmanager │  │ Alertmanager │
│  Instance 1  │  │  Instance 2  │  │  Instance 3  │
│              │  │              │  │              │
│  ┌────────┐  │  │  ┌────────┐  │  │  ┌────────┐  │
│  │ nflog  │←─┼──┼─→│ nflog  │←─┼──┼─→│ nflog  │  │
│  └────────┘  │  │  └────────┘  │  │  └────────┘  │
│  ┌────────┐  │  │  ┌────────┐  │  │  ┌────────┐  │
│  │Silences│←─┼──┼─→│Silences│←─┼──┼─→│Silences│  │
│  └────────┘  │  │  └────────┘  │  │  └────────┘  │
└──────────────┘  └──────────────┘  └──────────────┘
      ↑                ↑                 ↑
      │                │                 │
      └────────────────┼─────────────────┘
                       │
              Gossip Protocol
           (hashicorp/memberlist)
              UDP + TCP :9094
```

**동기화 대상**:
- **nflog**: 알림 발송 기록 → 중복 전송 방지
- **Silences**: 침묵 상태 → 모든 인스턴스에서 동일한 침묵 적용

**중요**: Prometheus는 **모든** Alertmanager 인스턴스에 알림을 전송해야 한다. 로드밸런서를 사용하면 안 된다.

## 8. 바이너리 구성

| 바이너리 | 진입점 | 역할 |
|----------|--------|------|
| `alertmanager` | `cmd/alertmanager/main.go` | 메인 서버 (682줄) |
| `amtool` | `cmd/amtool/main.go` | CLI 관리 도구 |

`amtool`은 Alertmanager API와 상호작용하는 CLI 도구로, 알림 조회, Silence 관리, 라우팅 테스트 등을 지원한다.
