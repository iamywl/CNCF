# PoC 16: 동시 DNS 리졸버 (경쟁 패턴)

## 개요

CoreDNS forward 플러그인을 기반으로, 여러 업스트림에 동시 쿼리를 보내고
가장 빠른 응답을 채택하는 경쟁(race) 패턴을 시뮬레이션한다.
Go의 goroutine, channel, context를 활용한 동시성 패턴을 구현한다.

## 실제 CoreDNS 코드 참조

| 파일 | 역할 |
|------|------|
| `plugin/forward/forward.go` | `ServeDNS()`에서 프록시 순회, 타임아웃 처리 |
| `plugin/forward/policy.go` | random, round_robin, sequential 정책 |
| `plugin/pkg/proxy/proxy.go` | `Connect()`를 통한 업스트림 연결 |

## 핵심 동작

1. **동시 쿼리**: 모든 업스트림에 goroutine으로 동시 쿼리 전송
2. **경쟁 패턴**: 가장 빠른 성공 응답을 채택
3. **Context 취소**: 첫 응답 수신 시 `cancel()` → 나머지 goroutine 조기 종료
4. **타임아웃**: `context.WithTimeout`으로 전체 쿼리 시간 제한
5. **실패 내성**: 일부 서버 실패해도 다른 서버 응답으로 정상 처리

## 실행

```bash
go run main.go
```

## 데모 시나리오

1. **동시 쿼리**: 3개 서버에 동시 쿼리, 가장 빠른 응답 채택
2. **성능 비교**: 순차 vs 동시 쿼리 평균 응답시간 비교
3. **부분 실패**: 1개 서버 항상 실패해도 나머지로 정상 응답
4. **타임아웃**: 모든 서버가 느릴 때 타임아웃 동작
5. **Context 취소 전파**: 승자 결정 후 나머지 goroutine 취소 확인
