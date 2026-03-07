package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Jaeger 분산 리더 선출 시뮬레이션
//
// 실제 소스코드 참조:
//   - internal/distributedlock/interface.go: Lock 인터페이스 (Acquire, Forfeit)
//   - internal/leaderelection/leader_election.go: ElectionParticipant, DistributedElectionParticipant
//
// 핵심 설계:
//   1. DistributedLock 인터페이스: Acquire(resource, ttl) / Forfeit(resource)
//   2. ElectionParticipant: 백그라운드 고루틴으로 잠금 획득 시도
//   3. 리더: LeaderLeaseRefreshInterval (빠른 주기)로 갱신
//   4. 팔로워: FollowerLeaseRefreshInterval (느린 주기)로 재시도
//   5. atomic.Bool로 IsLeader() 상태 관리
//   6. 적응형 샘플링 계산을 리더만 수행
// =============================================================================

// ---------------------------------------------------------------------------
// DistributedLock 인터페이스
// 실제 코드: internal/distributedlock/interface.go L12-L20
// ---------------------------------------------------------------------------

// Lock은 분산 잠금 인터페이스
// 실제 Jaeger에서는 Cassandra 기반 잠금(cassandra/lock.go)을 사용
type Lock interface {
	// Acquire는 주어진 리소스에 대해 ttl 기간의 잠금을 획득 시도
	// 반환값: (획득 성공 여부, 오류)
	Acquire(resource string, ttl time.Duration) (acquired bool, err error)

	// Forfeit은 주어진 리소스의 잠금을 포기
	Forfeit(resource string) (forfeited bool, err error)
}

// ---------------------------------------------------------------------------
// 인메모리 분산 잠금 구현 (Cassandra 잠금 시뮬레이션)
// ---------------------------------------------------------------------------

// lockEntry는 잠금 상태
type lockEntry struct {
	holder    string    // 잠금 보유자 ID
	expiresAt time.Time // 만료 시간
}

// InMemoryLock은 인메모리 분산 잠금 (테스트/시뮬레이션용)
type InMemoryLock struct {
	mu       sync.Mutex
	locks    map[string]*lockEntry
	ownerID  string
	failRate float64   // 장애 시뮬레이션 확률
	rng      *rand.Rand
}

func NewInMemoryLock(ownerID string, failRate float64) *InMemoryLock {
	return &InMemoryLock{
		locks:    make(map[string]*lockEntry),
		ownerID:  ownerID,
		failRate: failRate,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (l *InMemoryLock) Acquire(resource string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 장애 시뮬레이션
	if l.rng.Float64() < l.failRate {
		return false, fmt.Errorf("lock acquisition failed: simulated error")
	}

	now := time.Now()
	entry, exists := l.locks[resource]

	// 잠금이 없거나 만료된 경우 → 획득 가능
	if !exists || now.After(entry.expiresAt) {
		l.locks[resource] = &lockEntry{
			holder:    l.ownerID,
			expiresAt: now.Add(ttl),
		}
		return true, nil
	}

	// 현재 보유자가 자신인 경우 → 갱신
	if entry.holder == l.ownerID {
		entry.expiresAt = now.Add(ttl)
		return true, nil
	}

	// 다른 보유자의 잠금이 아직 유효 → 획득 실패
	return false, nil
}

func (l *InMemoryLock) Forfeit(resource string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.locks[resource]
	if !exists {
		return false, nil
	}
	if entry.holder == l.ownerID {
		delete(l.locks, resource)
		return true, nil
	}
	return false, nil
}

// SharedLockStore는 여러 인스턴스가 공유하는 잠금 저장소
// (실제로는 Cassandra 클러스터가 이 역할을 함)
type SharedLockStore struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

func NewSharedLockStore() *SharedLockStore {
	return &SharedLockStore{
		locks: make(map[string]*lockEntry),
	}
}

// SharedLock은 SharedLockStore를 사용하는 잠금 구현
type SharedLock struct {
	store   *SharedLockStore
	ownerID string
	mu      sync.Mutex
	rng     *rand.Rand
	failed  bool // 장애 상태 시뮬레이션
}

func NewSharedLock(store *SharedLockStore, ownerID string) *SharedLock {
	return &SharedLock{
		store:   store,
		ownerID: ownerID,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (l *SharedLock) SetFailed(failed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed = failed
}

func (l *SharedLock) isFailed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failed
}

func (l *SharedLock) Acquire(resource string, ttl time.Duration) (bool, error) {
	if l.isFailed() {
		return false, fmt.Errorf("instance %s is down", l.ownerID)
	}

	l.store.mu.Lock()
	defer l.store.mu.Unlock()

	now := time.Now()
	entry, exists := l.store.locks[resource]

	if !exists || now.After(entry.expiresAt) {
		l.store.locks[resource] = &lockEntry{
			holder:    l.ownerID,
			expiresAt: now.Add(ttl),
		}
		return true, nil
	}

	if entry.holder == l.ownerID {
		entry.expiresAt = now.Add(ttl)
		return true, nil
	}

	return false, nil
}

func (l *SharedLock) Forfeit(resource string) (bool, error) {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()

	entry, exists := l.store.locks[resource]
	if !exists {
		return false, nil
	}
	if entry.holder == l.ownerID {
		delete(l.store.locks, resource)
		return true, nil
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// ElectionParticipant: 리더 선출 참가자
// 실제 코드: internal/leaderelection/leader_election.go L22-L37
// ---------------------------------------------------------------------------

// ElectionParticipant는 리더 선출에 참여하는 인터페이스
type ElectionParticipant interface {
	IsLeader() bool
	Start() error
	Close() error
}

// ElectionParticipantOptions는 리더 선출 옵션
// 실제 코드: leader_election.go L40-L44
type ElectionParticipantOptions struct {
	LeaderLeaseRefreshInterval   time.Duration // 리더의 갱신 주기 (빠름)
	FollowerLeaseRefreshInterval time.Duration // 팔로워의 재시도 주기 (느림)
}

// DistributedElectionParticipant는 분산 잠금 기반 리더 선출 참가자
// 실제 코드: leader_election.go L30-L37
type DistributedElectionParticipant struct {
	options      ElectionParticipantOptions
	lock         Lock
	isLeader     atomic.Bool
	resourceName string
	instanceID   string
	closeChan    chan struct{}
	wg           sync.WaitGroup

	// 시뮬레이션 이벤트 로깅
	eventChan chan string
}

// NewElectionParticipant는 선출 참가자를 생성
// 실제 코드: leader_election.go L47-L54
func NewElectionParticipant(
	lock Lock,
	resourceName string,
	instanceID string,
	options ElectionParticipantOptions,
	eventChan chan string,
) *DistributedElectionParticipant {
	return &DistributedElectionParticipant{
		options:      options,
		lock:         lock,
		resourceName: resourceName,
		instanceID:   instanceID,
		closeChan:    make(chan struct{}),
		eventChan:    eventChan,
	}
}

// Start는 백그라운드 잠금 획득 루프를 시작
// 실제 코드: leader_election.go L57-L61
func (p *DistributedElectionParticipant) Start() error {
	p.wg.Add(1)
	go p.runAcquireLockLoop()
	return nil
}

// Close는 참가자를 종료
// 실제 코드: leader_election.go L64-L68
func (p *DistributedElectionParticipant) Close() error {
	close(p.closeChan)
	p.wg.Wait()
	return nil
}

// IsLeader는 현재 리더 여부를 반환
// 실제 코드: leader_election.go L71-L73
func (p *DistributedElectionParticipant) IsLeader() bool {
	return p.isLeader.Load()
}

// runAcquireLockLoop는 잠금 획득 루프
// 실제 코드: leader_election.go L77-L90
func (p *DistributedElectionParticipant) runAcquireLockLoop() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.acquireLock())
	for {
		select {
		case <-ticker.C:
			ticker.Stop()
			ticker = time.NewTicker(p.acquireLock())
		case <-p.closeChan:
			ticker.Stop()
			return
		}
	}
}

// acquireLock은 잠금 획득을 시도하고 다음 재시도 간격을 반환
// 실제 코드: leader_election.go L93-L106
func (p *DistributedElectionParticipant) acquireLock() time.Duration {
	// 잠금 획득 시도 (ttl = FollowerLeaseRefreshInterval)
	if acquired, err := p.lock.Acquire(p.resourceName, p.options.FollowerLeaseRefreshInterval); err == nil {
		wasLeader := p.isLeader.Load()
		p.setLeader(acquired)

		// 상태 변경 이벤트 로깅
		if acquired && !wasLeader {
			p.logEvent("LEADER_ACQUIRED")
		} else if !acquired && wasLeader {
			p.logEvent("LEADER_LOST")
		}
	} else {
		wasLeader := p.isLeader.Load()
		p.setLeader(false)
		if wasLeader {
			p.logEvent(fmt.Sprintf("LEADER_LOST (error: %v)", err))
		}
		p.logEvent(fmt.Sprintf("ACQUIRE_ERROR: %v", err))
	}

	// 리더면 빠른 주기, 팔로워면 느린 주기
	// 실제 코드: leader_election.go L99-L105
	if p.IsLeader() {
		return p.options.LeaderLeaseRefreshInterval
	}
	return p.options.FollowerLeaseRefreshInterval
}

// setLeader는 리더 상태를 설정
// 실제 코드: leader_election.go L108-L110
func (p *DistributedElectionParticipant) setLeader(isLeader bool) {
	p.isLeader.Store(isLeader)
}

func (p *DistributedElectionParticipant) logEvent(event string) {
	select {
	case p.eventChan <- fmt.Sprintf("[%s] %s: %s", time.Now().Format("15:04:05.000"), p.instanceID, event):
	default:
	}
}

// ---------------------------------------------------------------------------
// 적응형 샘플링 작업 시뮬레이션
// ---------------------------------------------------------------------------

// SamplingCalculator는 리더만 수행하는 샘플링 확률 계산기
type SamplingCalculator struct {
	mu            sync.Mutex
	probabilities map[string]float64
	calculations  int
}

func NewSamplingCalculator() *SamplingCalculator {
	return &SamplingCalculator{
		probabilities: make(map[string]float64),
	}
}

func (sc *SamplingCalculator) Calculate(leaderID string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	// 서비스별 샘플링 확률 계산 (시뮬레이션)
	services := []string{"api-gateway", "user-service", "order-service", "payment-service"}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for _, svc := range services {
		sc.probabilities[svc] = 0.001 + rng.Float64()*0.1
	}
	sc.calculations++
}

func (sc *SamplingCalculator) GetCalculationCount() int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.calculations
}

func (sc *SamplingCalculator) GetProbabilities() map[string]float64 {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	result := make(map[string]float64)
	for k, v := range sc.probabilities {
		result[k] = v
	}
	return result
}

// ---------------------------------------------------------------------------
// 시각화 헬퍼
// ---------------------------------------------------------------------------

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 80))
}

func printSubSeparator(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// ---------------------------------------------------------------------------
// 메인: 시뮬레이션 실행
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("Jaeger 분산 리더 선출 시뮬레이션")
	fmt.Println("참조: internal/leaderelection/leader_election.go")
	fmt.Println("참조: internal/distributedlock/interface.go")

	// =========================================================================
	// 1단계: 분산 잠금 기본 동작
	// =========================================================================
	printSeparator("1단계: 분산 잠금 기본 동작")

	fmt.Println()
	fmt.Println("Lock 인터페이스 (internal/distributedlock/interface.go):")
	fmt.Println("  Acquire(resource string, ttl time.Duration) (acquired bool, err error)")
	fmt.Println("  Forfeit(resource string) (forfeited bool, err error)")
	fmt.Println()

	lock1 := NewInMemoryLock("instance-1", 0)
	lock2 := NewInMemoryLock("instance-2", 0)

	// 공유 잠금 스토어 사용 (단독 잠금은 서로 상태를 공유하지 않으므로)
	store := NewSharedLockStore()
	slock1 := NewSharedLock(store, "instance-1")
	slock2 := NewSharedLock(store, "instance-2")

	// 잠금 획득 시뮬레이션
	acquired1, _ := slock1.Acquire("sampling-lock", 5*time.Second)
	fmt.Printf("instance-1 잠금 획득: %v\n", acquired1)

	acquired2, _ := slock2.Acquire("sampling-lock", 5*time.Second)
	fmt.Printf("instance-2 잠금 획득: %v (instance-1이 보유 중)\n", acquired2)

	// instance-1이 잠금을 포기
	forfeited, _ := slock1.Forfeit("sampling-lock")
	fmt.Printf("instance-1 잠금 포기: %v\n", forfeited)

	acquired2Again, _ := slock2.Acquire("sampling-lock", 5*time.Second)
	fmt.Printf("instance-2 재시도: %v (이제 획득 가능)\n", acquired2Again)

	_ = lock1
	_ = lock2

	// =========================================================================
	// 2단계: 3개 인스턴스 리더 선출 시뮬레이션
	// =========================================================================
	printSeparator("2단계: 3개 인스턴스 리더 선출 경쟁")

	fmt.Println()
	fmt.Println("설정:")
	fmt.Println("  - LeaderLeaseRefreshInterval:   5ms  (리더는 빠르게 갱신)")
	fmt.Println("  - FollowerLeaseRefreshInterval: 15ms (팔로워는 느리게 재시도)")
	fmt.Println("  - 리소스: 'adaptive-sampling-lock'")
	fmt.Println()
	fmt.Println("실제 Jaeger에서는:")
	fmt.Println("  - LeaderLeaseRefreshInterval:   5초")
	fmt.Println("  - FollowerLeaseRefreshInterval: 60초")
	fmt.Println("  - 시뮬레이션을 위해 밀리초 단위로 축소")
	fmt.Println()

	sharedStore := NewSharedLockStore()
	eventChan := make(chan string, 1000)

	instances := make([]*DistributedElectionParticipant, 3)
	locks := make([]*SharedLock, 3)

	for i := 0; i < 3; i++ {
		instanceID := fmt.Sprintf("collector-%d", i+1)
		locks[i] = NewSharedLock(sharedStore, instanceID)
		instances[i] = NewElectionParticipant(
			locks[i],
			"adaptive-sampling-lock",
			instanceID,
			ElectionParticipantOptions{
				LeaderLeaseRefreshInterval:   5 * time.Millisecond,
				FollowerLeaseRefreshInterval: 15 * time.Millisecond,
			},
			eventChan,
		)
	}

	// 모든 인스턴스 시작
	for _, inst := range instances {
		inst.Start()
	}

	// 잠깐 대기하여 리더 선출
	time.Sleep(50 * time.Millisecond)

	printSubSeparator("초기 리더 선출 결과")
	for i, inst := range instances {
		role := "팔로워"
		if inst.IsLeader() {
			role = "리더"
		}
		fmt.Printf("  collector-%d: %s\n", i+1, role)
	}

	// 이벤트 출력
	printSubSeparator("선출 이벤트 로그")
	drainEvents(eventChan, 10)

	// =========================================================================
	// 3단계: 리더 장애 시 페일오버
	// =========================================================================
	printSeparator("3단계: 리더 장애 시 페일오버")

	// 현재 리더 찾기
	leaderIdx := -1
	for i, inst := range instances {
		if inst.IsLeader() {
			leaderIdx = i
			break
		}
	}

	if leaderIdx >= 0 {
		fmt.Printf("\n현재 리더: collector-%d\n", leaderIdx+1)
		fmt.Printf("collector-%d에 장애 주입...\n", leaderIdx+1)

		// 리더 인스턴스의 잠금에 장애 주입
		locks[leaderIdx].SetFailed(true)

		// 잠금 TTL 만료 대기 + 팔로워의 재시도 대기
		time.Sleep(80 * time.Millisecond)

		printSubSeparator("페일오버 후 상태")
		newLeaderFound := false
		for i, inst := range instances {
			role := "팔로워"
			if inst.IsLeader() {
				role = "리더"
				if i != leaderIdx {
					newLeaderFound = true
				}
			}
			if i == leaderIdx {
				role += " (장애 상태)"
			}
			fmt.Printf("  collector-%d: %s\n", i+1, role)
		}

		if newLeaderFound {
			fmt.Println("\n새로운 리더가 선출되었습니다!")
		}

		printSubSeparator("페일오버 이벤트 로그")
		drainEvents(eventChan, 15)

		// 장애 복구
		locks[leaderIdx].SetFailed(false)
		time.Sleep(50 * time.Millisecond)

		printSubSeparator("장애 복구 후 상태")
		for i, inst := range instances {
			role := "팔로워"
			if inst.IsLeader() {
				role = "리더"
			}
			fmt.Printf("  collector-%d: %s\n", i+1, role)
		}
	}

	// =========================================================================
	// 4단계: 리더만 작업 수행
	// =========================================================================
	printSeparator("4단계: 리더만 적응형 샘플링 확률 계산")

	fmt.Println()
	fmt.Println("적응형 샘플링에서 리더만 샘플링 확률을 계산합니다.")
	fmt.Println("팔로워는 리더가 계산한 결과를 읽기만 합니다.")
	fmt.Println()

	calculator := NewSamplingCalculator()

	// 시뮬레이션: 200ms 동안 리더만 계산 수행
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		timeout := time.After(200 * time.Millisecond)
		for {
			select {
			case <-ticker.C:
				for i, inst := range instances {
					if inst.IsLeader() {
						calculator.Calculate(fmt.Sprintf("collector-%d", i+1))
					}
				}
			case <-timeout:
				close(done)
				return
			}
		}
	}()
	<-done

	fmt.Printf("총 샘플링 계산 횟수: %d회 (리더만 수행)\n", calculator.GetCalculationCount())
	fmt.Println()
	fmt.Println("계산된 샘플링 확률:")
	probs := calculator.GetProbabilities()
	for svc, prob := range probs {
		fmt.Printf("  %-20s: %.4f\n", svc, prob)
	}

	// =========================================================================
	// 5단계: Graceful Shutdown
	// =========================================================================
	printSeparator("5단계: Graceful Shutdown")

	fmt.Println()
	fmt.Println("모든 인스턴스를 순차적으로 종료합니다.")
	fmt.Println("Close()는 closeChan을 닫고 wg.Wait()로 고루틴 종료를 기다립니다.")
	fmt.Println()

	for i, inst := range instances {
		start := time.Now()
		inst.Close()
		elapsed := time.Since(start)
		fmt.Printf("  collector-%d 종료 완료 (소요: %v)\n", i+1, elapsed.Round(time.Microsecond))
	}

	printSubSeparator("종료 후 잔여 이벤트")
	drainEvents(eventChan, 20)

	// =========================================================================
	// 6단계: 리프레시 인터벌 전략 설명
	// =========================================================================
	printSeparator("6단계: 두 가지 리프레시 인터벌 전략")

	fmt.Println(`
    리더 선출의 핵심은 두 가지 서로 다른 리프레시 인터벌입니다:

    ┌──────────────────────────────────────────────┐
    │  LeaderLeaseRefreshInterval (짧은 주기)      │
    │  실제: 5초, 시뮬레이션: 5ms                   │
    │                                              │
    │  리더가 잠금을 갱신하는 주기입니다.           │
    │  짧은 주기로 갱신하여 잠금 만료를 방지하고    │
    │  리더십을 유지합니다.                         │
    └──────────────────────────────────────────────┘

    ┌──────────────────────────────────────────────┐
    │  FollowerLeaseRefreshInterval (긴 주기)      │
    │  실제: 60초, 시뮬레이션: 15ms                 │
    │                                              │
    │  팔로워가 잠금 획득을 재시도하는 주기입니다.  │
    │  리더가 활성 상태일 때 불필요한 잠금 경쟁을   │
    │  줄여 시스템 부하를 낮춥니다.                 │
    │                                              │
    │  이 값은 동시에 잠금 TTL로도 사용됩니다.     │
    │  → Acquire(resource, FollowerLeaseRefreshInterval)│
    │  → 잠금이 이 시간 후에 자동 만료              │
    └──────────────────────────────────────────────┘

    시간축 시뮬레이션:

    collector-1 (리더):
    ├─Acquire─┤  ├─Refresh─┤  ├─Refresh─┤  ├─FAIL──┤
    0ms       5ms          10ms         15ms        20ms

    collector-2 (팔로워):
    ├───────────────Acquire(실패)──────────────────┤  ├─Acquire(성공!)─┤
    0ms                                          15ms                30ms

    collector-3 (팔로워):
    ├───────────────Acquire(실패)──────────────────┤  ├─Acquire(실패)──┤
    0ms                                          15ms                30ms

    핵심:
    1. 리더는 5ms마다 갱신 → 잠금 유지 (TTL=15ms인 잠금을 5ms마다 갱신)
    2. 팔로워는 15ms마다 시도 → 리더가 활성이면 실패
    3. 리더 장애 시 TTL(15ms) 경과 후 팔로워가 획득 가능
    4. atomic.Bool로 상태 전환이 즉시 반영됨
`)

	// =========================================================================
	// 아키텍처 다이어그램
	// =========================================================================
	printSeparator("아키텍처: 적응형 샘플링에서의 리더 선출")

	fmt.Println(`
    ┌──────────────────────────────────────────────────────────────┐
    │                    Jaeger Collector 클러스터                 │
    │                                                              │
    │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐         │
    │  │ Collector-1  │  │ Collector-2  │  │ Collector-3  │         │
    │  │             │  │             │  │             │         │
    │  │ Election    │  │ Election    │  │ Election    │         │
    │  │ Participant │  │ Participant │  │ Participant │         │
    │  │ isLeader:   │  │ isLeader:   │  │ isLeader:   │         │
    │  │   true      │  │   false     │  │   false     │         │
    │  │             │  │             │  │             │         │
    │  │ [계산 수행] │  │ [대기]      │  │ [대기]      │         │
    │  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘         │
    │         │                │                │                 │
    │         │  Acquire(5ms)  │ Acquire(15ms)  │ Acquire(15ms)  │
    │         │                │                │                 │
    │         └────────────────┼────────────────┘                 │
    │                          │                                   │
    │                          v                                   │
    │              ┌───────────────────┐                           │
    │              │  Distributed Lock │                           │
    │              │  (Cassandra)      │                           │
    │              │                   │                           │
    │              │  resource:        │                           │
    │              │   adaptive-       │                           │
    │              │   sampling-lock   │                           │
    │              │  holder:          │                           │
    │              │   collector-1     │                           │
    │              │  expires_at:      │                           │
    │              │   now + 15ms      │                           │
    │              └───────────────────┘                           │
    └──────────────────────────────────────────────────────────────┘
`)
}

// drainEvents는 이벤트 채널에서 이벤트를 읽어 출력
func drainEvents(ch chan string, maxEvents int) {
	count := 0
	for {
		select {
		case event := <-ch:
			fmt.Printf("  %s\n", event)
			count++
			if count >= maxEvents {
				// 남은 이벤트 수 확인
				remaining := len(ch)
				if remaining > 0 {
					fmt.Printf("  ... 외 %d개 이벤트\n", remaining)
					// 남은 이벤트 드레인
					for range remaining {
						<-ch
					}
				}
				return
			}
		default:
			if count == 0 {
				fmt.Println("  (이벤트 없음)")
			}
			return
		}
	}
}
