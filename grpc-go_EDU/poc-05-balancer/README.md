# PoC-05: pick_first / round_robin 밸런서

## 개념

gRPC의 클라이언트 사이드 로드 밸런싱을 시뮬레이션한다.

```
리졸버 (DNS 등)              밸런서                    Picker
┌──────────┐   주소 목록    ┌──────────────┐  Pick()  ┌──────────┐
│ Resolve() │──────────────▶│ UpdateState()│─────────▶│ Pick()   │
│           │               │              │          │  ↓       │
│ [addr1]   │               │ SubConn 관리  │          │ SubConn  │──▶ RPC
│ [addr2]   │               │ Picker 생성   │          └──────────┘
│ [addr3]   │               └──────────────┘
└──────────┘

pick_first:  항상 첫 번째 SubConn → [sc1] [sc2] [sc3]
                                      ↑↑↑
round_robin: 순환 선택        → [sc1] [sc2] [sc3] [sc1] [sc2] ...
                                 ↑          ↑          ↑
```

## 시뮬레이션하는 gRPC 구조

| 구조체/함수 | 실제 위치 | 역할 |
|------------|----------|------|
| `Balancer` | `balancer/balancer.go:344` | 밸런싱 정책 인터페이스 |
| `Picker` | `balancer/balancer.go:313` | RPC를 SubConn에 매핑 |
| `SubConn` | `balancer/balancer.go` | 백엔드 연결 추상화 |
| `pick_first` | `balancer/pickfirst/` | 첫 번째 주소만 사용 |
| `round_robin` | `balancer/roundrobin/` | 순환 선택 |
| `ClientConnState` | `balancer/balancer.go` | 리졸버→밸런서 상태 전달 |

## 실행 방법

```bash
cd poc-05-balancer
go run main.go
```

## 예상 출력

```
=== pick_first / round_robin 밸런서 시뮬레이션 ===

── 1. pick_first 밸런서 ──
  [pick_first] 10.0.0.1:8080에 연결 시도...
  [pick_first] 10.0.0.1:8080 연결 성공 → READY

  10번 Pick 결과:
    Pick #1 → 10.0.0.1:8080
    Pick #2 → 10.0.0.1:8080
    ...

── 2. round_robin 밸런서 ──
  [round_robin] 10.0.0.1:8080 → READY
  [round_robin] 10.0.0.2:8080 → READY
  ...

  12번 Pick 결과 (순환 확인):
    Pick #01 → 10.0.0.1:8080
    Pick #02 → 10.0.0.2:8080
    Pick #03 → 10.0.0.3:8080
    Pick #04 → 10.0.0.1:8080
    ...

=== 시뮬레이션 완료 ===
```

## 핵심 포인트

1. **pick_first**: 첫 번째 주소에만 연결, 단순하고 빠름 (gRPC 기본 정책)
2. **round_robin**: 모든 활성 SubConn을 순환하여 균등 분배
3. **Picker 분리**: 밸런서가 Picker를 생성, Picker는 락 없이 빠르게 Pick 수행 (atomic 카운터)
4. **동적 업데이트**: 리졸버가 주소 목록을 변경하면 밸런서가 SubConn을 재구성하고 새 Picker를 생성
