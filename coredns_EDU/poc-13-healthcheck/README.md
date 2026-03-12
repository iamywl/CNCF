# PoC 13: 업스트림 헬스체크

## 개요

CoreDNS forward 플러그인의 업스트림 헬스체크 메커니즘을 시뮬레이션한다.
주기적으로 업스트림 서버의 상태를 확인하고, 실패 횟수 기반으로 다운된 서버를 자동 제외하며, 복구 시 재포함한다.

## 실제 CoreDNS 코드 참조

| 파일 | 역할 |
|------|------|
| `plugin/pkg/proxy/proxy.go` | Proxy 구조체, `Down()`, `incrementFails()` |
| `plugin/pkg/proxy/health.go` | HealthChecker 인터페이스, `Check()` 메서드 |
| `plugin/forward/forward.go` | `ServeDNS()`에서 `proxy.Down(maxfails)` 호출 |

## 핵심 동작

1. **주기적 헬스체크**: 각 업스트림에 `hcInterval`(기본 500ms) 간격으로 `. IN NS` 쿼리 전송
2. **실패 카운터**: 헬스체크 실패 시 `fails` atomic 카운터 증가
3. **다운 판정**: `fails > maxfails`(기본 2)이면 `Down()` → `true`
4. **자동 복구**: 헬스체크 성공 시 `fails = 0`으로 리셋
5. **SERVFAIL**: 모든 업스트림 다운 시 클라이언트에 SERVFAIL 반환

## 실행

```bash
go run main.go
```

## 데모 시나리오

1. **Phase 1**: 모든 서버 정상 → 3개 서버에 균등 분배
2. **Phase 2**: 1개 서버 다운 → 자동 제외, 나머지 2개에만 쿼리
3. **Phase 3**: 다운된 서버 복구 → fails 리셋, 다시 3개 서버 사용
4. **Phase 4**: 모든 서버 다운 → SERVFAIL 반환
