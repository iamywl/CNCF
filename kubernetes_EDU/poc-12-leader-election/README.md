# PoC-12: Lease 기반 리더 선출

## 개요

Kubernetes의 Lease 기반 리더 선출(Leader Election) 메커니즘을 시뮬레이션한다.

Kubernetes에서 컨트롤러 매니저, 스케줄러 등 고가용성 컴포넌트는 여러 인스턴스가 실행되지만, 실제로 작업을 수행하는 것은 리더 하나뿐이다. Lease 리소스를 이용한 리더 선출로 active-standby 패턴을 구현한다.

## 실제 코드 참조

| 파일 | 역할 |
|------|------|
| `staging/src/k8s.io/client-go/tools/leaderelection/leaderelection.go` | LeaderElector, acquire/renew 루프 |
| `staging/src/k8s.io/client-go/tools/leaderelection/resourcelock/` | LeaseLock 구현 |

## 시뮬레이션하는 개념

### 1. Lease 리소스
- HolderIdentity: 현재 리더의 ID
- LeaseDurationSeconds: Lease 유효 기간
- RenewTime: 마지막 갱신 시간
- LeaderTransitions: 리더 교체 횟수

### 2. 리더 선출 흐름
1. `acquire()`: Lease를 획득할 때까지 RetryPeriod 간격으로 시도
2. 획득 성공 시 `OnStartedLeading` 콜백 호출 (별도 goroutine)
3. `renew()`: RetryPeriod 간격으로 Lease 갱신
4. 갱신 실패 (RenewDeadline 초과) 시 리더십 포기
5. `OnStoppedLeading` 콜백 호출

### 3. Failover
- 리더가 갱신을 멈추면 LeaseDuration 후 Lease 만료
- 대기 중인 다른 후보가 만료된 Lease를 획득
- LeaderTransitions 카운터 증가

### 4. 시간 제약 조건
```
LeaseDuration > RenewDeadline > RetryPeriod * 1.2
기본값: 15s > 10s > 2s * 1.2 = 2.4s
```

## 실행

```bash
go run main.go
```

## 시뮬레이션 시나리오

1. 3개의 후보자가 동시에 리더 선출 시작
2. 한 후보가 리더가 되어 작업을 수행
3. 1.5초 후 리더를 강제 중단 (장애 시뮬레이션)
4. 다른 후보가 failover로 새 리더가 됨
5. 3초 후 전체 종료
