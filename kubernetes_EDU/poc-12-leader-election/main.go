package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes Lease 기반 리더 선출(Leader Election) 시뮬레이션
//
// 실제 구현 참조:
//   - staging/src/k8s.io/client-go/tools/leaderelection/leaderelection.go
//   - staging/src/k8s.io/client-go/tools/leaderelection/resourcelock/
//
// 핵심 개념:
//   1. Lease 리소스: 리더 정보를 저장하는 공유 리소스 (holderIdentity, leaseDuration, renewTime)
//   2. 후보자(candidate)가 Lease를 획득하면 리더가 된다
//   3. 리더는 주기적으로 Lease를 갱신(renew)해야 한다
//   4. 갱신 실패 시 Lease가 만료되고, 다른 후보가 리더가 된다
//   5. OnStartedLeading / OnStoppedLeading 콜백으로 리더십 전환을 처리한다
//
// 실제 시간 관계:
//   LeaseDuration > RenewDeadline > RetryPeriod * JitterFactor(1.2)
//   기본값: LeaseDuration=15s, RenewDeadline=10s, RetryPeriod=2s
// =============================================================================

// --- LeaseRecord ---

// LeaseRecord는 리더 정보를 저장하는 공유 레코드이다.
// 실제 resourcelock.LeaderElectionRecord에 대응한다.
type LeaseRecord struct {
	HolderIdentity       string    // 현재 리더의 ID
	LeaseDurationSeconds int       // Lease 유효 기간 (초)
	AcquireTime          time.Time // Lease 획득 시간
	RenewTime            time.Time // 마지막 갱신 시간
	LeaderTransitions    int       // 리더 교체 횟수
}

// --- LeaseLock (공유 리소스) ---

// LeaseLock은 etcd에 저장되는 Lease 리소스를 시뮬레이션한다.
// 실제 resourcelock.LeaseLock에 대응한다.
// 모든 후보자가 이 하나의 Lock을 경쟁한다.
type LeaseLock struct {
	mu     sync.Mutex
	record *LeaseRecord
}

func NewLeaseLock() *LeaseLock {
	return &LeaseLock{}
}

// Get은 현재 Lease 레코드를 반환한다.
func (l *LeaseLock) Get() (*LeaseRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.record == nil {
		return nil, fmt.Errorf("lease not found")
	}

	// 복사본 반환
	cp := *l.record
	return &cp, nil
}

// Create는 새 Lease를 생성한다 (Lease가 없을 때만).
func (l *LeaseLock) Create(record LeaseRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.record != nil {
		return fmt.Errorf("lease already exists")
	}

	cp := record
	l.record = &cp
	return nil
}

// Update는 기존 Lease를 갱신한다.
func (l *LeaseLock) Update(record LeaseRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.record == nil {
		return fmt.Errorf("lease not found")
	}

	cp := record
	l.record = &cp
	return nil
}

// --- LeaderCallbacks ---

// LeaderCallbacks는 리더십 전환 시 호출되는 콜백이다.
// 실제 leaderelection.LeaderCallbacks에 대응한다.
type LeaderCallbacks struct {
	// OnStartedLeading은 리더가 되었을 때 호출된다 (별도 goroutine에서 실행)
	OnStartedLeading func(ctx context.Context)
	// OnStoppedLeading은 리더십을 잃었을 때 호출된다
	OnStoppedLeading func()
	// OnNewLeader는 새 리더를 관찰했을 때 호출된다
	OnNewLeader func(identity string)
}

// --- LeaderElectionConfig ---

// LeaderElectionConfig는 리더 선출 설정이다.
// 실제 leaderelection.LeaderElectionConfig에 대응한다.
//
// 시간 제약 조건:
//   LeaseDuration > RenewDeadline > RetryPeriod * 1.2
type LeaderElectionConfig struct {
	Lock            *LeaseLock
	LeaseDuration   time.Duration // Lease 만료 시간
	RenewDeadline   time.Duration // 갱신 타임아웃
	RetryPeriod     time.Duration // 재시도 간격
	Callbacks       LeaderCallbacks
	ReleaseOnCancel bool   // ctx 취소 시 Lease 해제 여부
	Name            string // 디버깅용 이름
	Identity        string // 이 후보자의 고유 ID
}

// --- LeaderElector ---

// LeaderElector는 리더 선출 클라이언트이다.
// 실제 leaderelection.LeaderElector에 대응한다.
type LeaderElector struct {
	config LeaderElectionConfig

	// 관찰된 레코드 (다른 후보의 변경 감지용)
	observedRecord     LeaseRecord
	observedTime       time.Time
	observedRecordLock sync.RWMutex

	// 보고된 리더 (OnNewLeader 콜백 중복 방지)
	reportedLeader string
}

// NewLeaderElector는 새 LeaderElector를 생성한다.
// 실제 leaderelection.NewLeaderElector에 대응한다 (유효성 검사 포함).
func NewLeaderElector(config LeaderElectionConfig) (*LeaderElector, error) {
	if config.LeaseDuration <= config.RenewDeadline {
		return nil, fmt.Errorf("leaseDuration(%v)는 renewDeadline(%v)보다 커야 합니다",
			config.LeaseDuration, config.RenewDeadline)
	}
	if config.RenewDeadline <= time.Duration(1.2*float64(config.RetryPeriod)) {
		return nil, fmt.Errorf("renewDeadline(%v)는 retryPeriod*1.2(%v)보다 커야 합니다",
			config.RenewDeadline, time.Duration(1.2*float64(config.RetryPeriod)))
	}

	return &LeaderElector{
		config: config,
	}, nil
}

// Run은 리더 선출 루프를 시작한다.
// 실제 leaderelection.go:211의 Run에 대응한다:
//   if !le.acquire(ctx) { return }
//   go le.config.Callbacks.OnStartedLeading(ctx)
//   le.renew(ctx)
func (le *LeaderElector) Run(ctx context.Context) {
	defer le.config.Callbacks.OnStoppedLeading()

	if !le.acquire(ctx) {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go le.config.Callbacks.OnStartedLeading(ctx)
	le.renew(ctx)
}

// acquire는 리더 Lease를 획득할 때까지 반복 시도한다.
// 실제 leaderelection.go:252의 acquire에 대응한다.
func (le *LeaderElector) acquire(ctx context.Context) bool {
	fmt.Printf("  [%s] Lease 획득 시도 중...\n", le.config.Identity)

	ticker := time.NewTicker(le.config.RetryPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		succeeded := le.tryAcquireOrRenew()
		le.maybeReportTransition()

		if succeeded {
			fmt.Printf("  [%s] Lease 획득 성공!\n", le.config.Identity)
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			// jitter 추가 (실제: JitterFactor=1.2)
			jitter := time.Duration(rand.Int63n(int64(float64(le.config.RetryPeriod) * 0.2)))
			time.Sleep(jitter)
		}
	}
}

// renew는 리더 Lease를 주기적으로 갱신한다.
// 갱신 실패 시 리더십을 포기한다.
// 실제 leaderelection.go:279의 renew에 대응한다.
func (le *LeaderElector) renew(ctx context.Context) {
	ticker := time.NewTicker(le.config.RetryPeriod)
	defer ticker.Stop()

	renewDeadline := time.NewTimer(le.config.RenewDeadline)
	defer renewDeadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			succeeded := le.tryAcquireOrRenew()
			le.maybeReportTransition()

			if succeeded {
				// 갱신 성공 → deadline 리셋
				renewDeadline.Reset(le.config.RenewDeadline)
				continue
			}

			fmt.Printf("  [%s] Lease 갱신 실패\n", le.config.Identity)
		case <-renewDeadline.C:
			fmt.Printf("  [%s] 갱신 deadline 초과 - 리더십 포기\n", le.config.Identity)
			return
		}
	}
}

// tryAcquireOrRenew는 Lease 획득 또는 갱신을 시도한다.
// 실제 leaderelection.go:432의 tryAcquireOrRenew에 대응한다.
//
// 동작:
//   1. 기존 Lease가 없으면 → Create로 획득
//   2. 기존 Lease가 있고 만료되지 않았으며 내가 리더가 아니면 → 실패
//   3. 내가 리더면 → 갱신 (RenewTime 업데이트)
//   4. Lease가 만료되었으면 → 새로 획득 (LeaderTransitions 증가)
func (le *LeaderElector) tryAcquireOrRenew() bool {
	now := time.Now()
	leaderElectionRecord := LeaseRecord{
		HolderIdentity:       le.config.Identity,
		LeaseDurationSeconds: int(le.config.LeaseDuration / time.Second),
		RenewTime:            now,
		AcquireTime:          now,
	}

	// 1단계: 기존 Lease 조회 또는 새로 생성
	oldRecord, err := le.config.Lock.Get()
	if err != nil {
		// Lease가 없음 → 새로 생성
		if err := le.config.Lock.Create(leaderElectionRecord); err != nil {
			return false
		}
		le.setObservedRecord(&leaderElectionRecord)
		return true
	}

	// 2단계: 관찰 레코드 업데이트
	if oldRecord.HolderIdentity != le.getObservedRecord().HolderIdentity ||
		oldRecord.RenewTime != le.getObservedRecord().RenewTime {
		le.setObservedRecord(oldRecord)
	}

	// 3단계: Lease 만료 여부 확인
	isExpired := le.observedTime.Add(
		time.Duration(oldRecord.LeaseDurationSeconds) * time.Second,
	).Before(now)

	// Lease가 유효하고 내가 리더가 아니면 → 실패
	if !isExpired && oldRecord.HolderIdentity != le.config.Identity {
		return false
	}

	// 4단계: Lease 업데이트
	if le.IsLeader() {
		// 리더 갱신: AcquireTime과 Transitions 유지
		leaderElectionRecord.AcquireTime = oldRecord.AcquireTime
		leaderElectionRecord.LeaderTransitions = oldRecord.LeaderTransitions
	} else {
		// 새 리더: Transitions 증가
		leaderElectionRecord.LeaderTransitions = oldRecord.LeaderTransitions + 1
	}

	if err := le.config.Lock.Update(leaderElectionRecord); err != nil {
		return false
	}

	le.setObservedRecord(&leaderElectionRecord)
	return true
}

// IsLeader는 현재 이 인스턴스가 리더인지 반환한다.
func (le *LeaderElector) IsLeader() bool {
	return le.getObservedRecord().HolderIdentity == le.config.Identity
}

// GetLeader는 현재 리더의 ID를 반환한다.
func (le *LeaderElector) GetLeader() string {
	return le.getObservedRecord().HolderIdentity
}

// setObservedRecord는 관찰된 레코드와 시간을 업데이트한다.
func (le *LeaderElector) setObservedRecord(record *LeaseRecord) {
	le.observedRecordLock.Lock()
	defer le.observedRecordLock.Unlock()
	le.observedRecord = *record
	le.observedTime = time.Now()
}

// getObservedRecord는 관찰된 레코드를 반환한다.
func (le *LeaderElector) getObservedRecord() LeaseRecord {
	le.observedRecordLock.RLock()
	defer le.observedRecordLock.RUnlock()
	return le.observedRecord
}

// maybeReportTransition은 리더가 변경되었을 때 OnNewLeader를 호출한다.
// 실제 leaderelection.go:505의 maybeReportTransition에 대응한다.
func (le *LeaderElector) maybeReportTransition() {
	record := le.getObservedRecord()
	if record.HolderIdentity == le.reportedLeader {
		return
	}
	le.reportedLeader = record.HolderIdentity
	if le.config.Callbacks.OnNewLeader != nil {
		go le.config.Callbacks.OnNewLeader(le.reportedLeader)
	}
}

// --- 데모 실행 ---

func main() {
	fmt.Println("=== Kubernetes Lease 기반 리더 선출 시뮬레이션 ===")
	fmt.Println()

	// 데모에서는 짧은 시간 사용
	leaseDuration := 800 * time.Millisecond
	renewDeadline := 500 * time.Millisecond
	retryPeriod := 200 * time.Millisecond

	// 공유 Lease 리소스
	lock := NewLeaseLock()

	// 이벤트 로그 (데모 출력용)
	var logMu sync.Mutex
	logEvent := func(format string, args ...interface{}) {
		logMu.Lock()
		defer logMu.Unlock()
		timestamp := time.Now().Format("15:04:05.000")
		fmt.Printf("  [%s] %s\n", timestamp, fmt.Sprintf(format, args...))
	}

	// -----------------------------------------------
	// 시나리오: 3개 후보자가 리더를 경쟁, 리더가 중단되면 failover
	// -----------------------------------------------

	fmt.Println("시나리오:")
	fmt.Println("  1. candidate-1, candidate-2, candidate-3가 동시에 리더 선출 시작")
	fmt.Println("  2. 한 후보가 리더가 되어 작업 수행")
	fmt.Println("  3. 1.5초 후 리더를 강제 중단")
	fmt.Println("  4. 다른 후보가 failover로 새 리더가 됨")
	fmt.Println("  5. 3초 후 전체 종료")
	fmt.Printf("  설정: leaseDuration=%v, renewDeadline=%v, retryPeriod=%v\n",
		leaseDuration, renewDeadline, retryPeriod)
	fmt.Println()
	fmt.Println("--- 실행 로그 ---")

	// 전체 타임아웃
	globalCtx, globalCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer globalCancel()

	// 리더 cancel 함수 저장 (1.5초 후 리더 중단용)
	var leaderCancelOnce sync.Once
	var leaderCancel context.CancelFunc
	var leaderCancelMu sync.Mutex
	var leaderIdentity string

	var wg sync.WaitGroup

	for i := 1; i <= 3; i++ {
		candidateID := fmt.Sprintf("candidate-%d", i)
		candidateCtx, candidateCancel := context.WithCancel(globalCtx)

		wg.Add(1)
		go func(id string, ctx context.Context, cancel context.CancelFunc) {
			defer wg.Done()

			le, err := NewLeaderElector(LeaderElectionConfig{
				Lock:          lock,
				LeaseDuration: leaseDuration,
				RenewDeadline: renewDeadline,
				RetryPeriod:   retryPeriod,
				Identity:      id,
				Callbacks: LeaderCallbacks{
					OnStartedLeading: func(ctx context.Context) {
						logEvent("%s: 리더 시작! 작업 수행 중...", id)

						// 리더 cancel 함수 저장
						leaderCancelMu.Lock()
						leaderCancel = cancel
						leaderIdentity = id
						leaderCancelMu.Unlock()

						// 리더로서 작업 수행 (ctx가 취소될 때까지)
						ticker := time.NewTicker(300 * time.Millisecond)
						defer ticker.Stop()
						workCount := 0
						for {
							select {
							case <-ctx.Done():
								logEvent("%s: 리더 작업 중단 (ctx 취소)", id)
								return
							case <-ticker.C:
								workCount++
								logEvent("%s: [리더 작업 #%d] 처리 중...", id, workCount)
							}
						}
					},
					OnStoppedLeading: func() {
						logEvent("%s: 리더십 상실 (OnStoppedLeading)", id)
					},
					OnNewLeader: func(identity string) {
						if identity == id {
							logEvent("%s: 새 리더 관찰 → 나 자신!", id)
						} else {
							logEvent("%s: 새 리더 관찰 → %s", id, identity)
						}
					},
				},
			})
			if err != nil {
				logEvent("%s: LeaderElector 생성 실패: %v", id, err)
				return
			}

			le.Run(ctx)
		}(candidateID, candidateCtx, candidateCancel)
	}

	// 1.5초 후 현재 리더를 중단 (failover 테스트)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		leaderCancelMu.Lock()
		cancelFn := leaderCancel
		id := leaderIdentity
		leaderCancelMu.Unlock()

		if cancelFn != nil {
			logEvent(">>> %s 강제 중단 (failover 테스트) <<<", id)
			leaderCancelOnce.Do(func() {
				cancelFn()
			})
		}
	}()

	wg.Wait()

	fmt.Println()
	fmt.Println("--- 최종 Lease 상태 ---")
	if record, err := lock.Get(); err == nil {
		fmt.Printf("  HolderIdentity: %s\n", record.HolderIdentity)
		fmt.Printf("  LeaseDurationSeconds: %d\n", record.LeaseDurationSeconds)
		fmt.Printf("  AcquireTime: %s\n", record.AcquireTime.Format("15:04:05.000"))
		fmt.Printf("  RenewTime: %s\n", record.RenewTime.Format("15:04:05.000"))
		fmt.Printf("  LeaderTransitions: %d\n", record.LeaderTransitions)
	}

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 요약:")
	fmt.Println("  1. Lease는 공유 리소스(etcd)에 저장되는 리더 정보이다")
	fmt.Println("  2. 리더는 RetryPeriod 간격으로 Lease를 갱신한다")
	fmt.Println("  3. LeaseDuration 동안 갱신이 없으면 Lease가 만료된다")
	fmt.Println("  4. 만료된 Lease는 다른 후보가 획득하여 새 리더가 된다 (failover)")
	fmt.Println("  5. OnStartedLeading/OnStoppedLeading으로 리더십 전환을 처리한다")
	fmt.Println("  6. 시간 관계: LeaseDuration > RenewDeadline > RetryPeriod * 1.2")
}
