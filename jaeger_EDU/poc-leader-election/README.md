# PoC 16: 분산 리더 선출 시뮬레이션

## 개요

Jaeger의 적응형 샘플링(Adaptive Sampling)에서 사용하는 분산 리더 선출 메커니즘을 시뮬레이션합니다.
여러 Jaeger Collector 인스턴스 중 하나만 샘플링 확률을 계산하도록 리더를 선출하고,
리더 장애 시 자동으로 다른 인스턴스가 리더를 인계받는 과정을 구현합니다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `internal/distributedlock/interface.go` | Lock 인터페이스 (Acquire, Forfeit) |
| `internal/leaderelection/leader_election.go` | DistributedElectionParticipant |
| `internal/sampling/samplingstrategy/adaptive/` | 적응형 샘플링 (리더가 수행) |

## 핵심 설계 원리

### DistributedLock 인터페이스
```go
type Lock interface {
    Acquire(resource string, ttl time.Duration) (acquired bool, err error)
    Forfeit(resource string) (forfeited bool, err error)
}
```

### 두 가지 리프레시 인터벌
```go
type ElectionParticipantOptions struct {
    LeaderLeaseRefreshInterval   time.Duration  // 리더: 5초 (빠른 갱신)
    FollowerLeaseRefreshInterval time.Duration  // 팔로워: 60초 (느린 재시도)
}
```

### 핵심 루프
```go
func (p *DistributedElectionParticipant) acquireLock() time.Duration {
    if acquired, err := p.lock.Acquire(p.resourceName, p.FollowerLeaseRefreshInterval); err == nil {
        p.setLeader(acquired)
    }
    if p.IsLeader() {
        return p.LeaderLeaseRefreshInterval   // 빠른 갱신
    }
    return p.FollowerLeaseRefreshInterval      // 느린 재시도
}
```

### atomic.Bool 상태 관리
```go
func (p *DistributedElectionParticipant) IsLeader() bool {
    return p.isLeader.Load()  // 락 없이 안전한 읽기
}
```

## 시뮬레이션 내용

1. **분산 잠금 기본 동작**: Acquire/Forfeit 시뮬레이션
2. **3개 인스턴스 경쟁**: 동시에 리더 선출 시도
3. **리더 장애 페일오버**: 리더에 장애를 주입하고 새 리더 선출 확인
4. **리더만 작업 수행**: 적응형 샘플링 확률 계산은 리더만 실행
5. **Graceful Shutdown**: closeChan + WaitGroup 기반 안전한 종료
6. **인터벌 전략 설명**: ASCII 타임라인 다이어그램

## 실행 방법

```bash
go run main.go
```

## 주요 출력

- 분산 잠금 획득/포기 시뮬레이션
- 3개 인스턴스의 리더/팔로워 상태
- 장애 주입 후 페일오버 이벤트 로그
- 샘플링 확률 계산 횟수 (리더만 수행 확인)
- Graceful Shutdown 소요 시간

## 핵심 인사이트

- 잠금 TTL은 `FollowerLeaseRefreshInterval`과 동일 → 팔로워의 재시도 주기가 곧 잠금 만료 시간
- 리더는 TTL보다 훨씬 짧은 주기로 갱신 → 네트워크 지연이 있어도 잠금 유지
- `atomic.Bool`로 상태 전환이 즉시 반영 → 리더 작업의 중단/시작이 빠름
- 장애 복구 후 기존 리더가 반드시 리더를 되찾는 것은 아님 → 먼저 획득한 인스턴스가 리더
