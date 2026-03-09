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
// Istio 리더 선출 + Status 관리 시뮬레이션
//
// 실제 소스 참조:
//   - pilot/pkg/leaderelection/leaderelection.go → LeaderElection, 다중 선출
//   - pilot/pkg/status/state.go                  → StatusController, 배치 쓰기
//   - pilot/pkg/status/distribution.go           → Distribution Reporter
//
// 핵심 알고리즘:
//   1. Kubernetes Lease 기반 리더 선출 (tryAcquireOrRenew)
//   2. 리더만 Status Writer, Webhook Patcher 등 실행
//   3. Status 배치 업데이트: 중복 쓰기 방지를 위한 ProgressReporter
//   4. Distribution Reporting: 설정이 실제로 프록시에 적용되었는지 추적
// =============================================================================

// --- Lease 기반 리더 선출 ---

// Lease는 Kubernetes Lease 오브젝트를 시뮬레이션한다.
// 실제: coordination.k8s.io/v1/Lease
type Lease struct {
	mu              sync.Mutex
	Name            string
	HolderIdentity  string
	AcquireTime     time.Time
	RenewTime       time.Time
	LeaseDurationMs int64
	ResourceVersion int64
}

func NewLease(name string, durationMs int64) *Lease {
	return &Lease{
		Name:            name,
		LeaseDurationMs: durationMs,
	}
}

// TryAcquireOrRenew은 리더 선출의 핵심 루프이다.
// 실제 Istio는 k8s.io/client-go/tools/leaderelection.LeaderElector.tryAcquireOrRenew()을 사용.
// 반환: (획득 성공 여부, 에러)
func (l *Lease) TryAcquireOrRenew(identity string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// Case 1: Lease가 비어있음 → 즉시 획득
	if l.HolderIdentity == "" {
		l.HolderIdentity = identity
		l.AcquireTime = now
		l.RenewTime = now
		l.ResourceVersion++
		return true, nil
	}

	// Case 2: 현재 보유자가 자신 → 갱신
	if l.HolderIdentity == identity {
		l.RenewTime = now
		l.ResourceVersion++
		return true, nil
	}

	// Case 3: 다른 보유자가 있고 아직 만료되지 않음 → 획득 실패
	elapsed := now.Sub(l.RenewTime).Milliseconds()
	if elapsed < l.LeaseDurationMs {
		return false, nil
	}

	// Case 4: 다른 보유자가 있지만 만료됨 → 강제 인수
	l.HolderIdentity = identity
	l.AcquireTime = now
	l.RenewTime = now
	l.ResourceVersion++
	return true, nil
}

// --- LeaderElector: 선출 관리자 ---

type LeaderElector struct {
	Name         string
	Identity     string
	Lease        *Lease
	IsLeader     atomic.Bool
	OnStarted    func() // 리더 획득 시 콜백
	OnStopped    func() // 리더 상실 시 콜백
	RetryPeriod  time.Duration
	RenewPeriod  time.Duration
}

func NewLeaderElector(name, identity string, lease *Lease,
	onStarted, onStopped func()) *LeaderElector {
	return &LeaderElector{
		Name:        name,
		Identity:    identity,
		Lease:       lease,
		OnStarted:   onStarted,
		OnStopped:   onStopped,
		RetryPeriod: 50 * time.Millisecond,
		RenewPeriod: 30 * time.Millisecond,
	}
}

func (le *LeaderElector) Run(stop <-chan struct{}) {
	// 초기 획득 시도
	for {
		select {
		case <-stop:
			return
		default:
		}
		acquired, _ := le.Lease.TryAcquireOrRenew(le.Identity)
		if acquired {
			le.IsLeader.Store(true)
			if le.OnStarted != nil {
				le.OnStarted()
			}
			break
		}
		time.Sleep(le.RetryPeriod + time.Duration(rand.Intn(10))*time.Millisecond)
	}

	// 갱신 루프
	ticker := time.NewTicker(le.RenewPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			le.release()
			return
		case <-ticker.C:
			renewed, _ := le.Lease.TryAcquireOrRenew(le.Identity)
			if !renewed && le.IsLeader.Load() {
				le.IsLeader.Store(false)
				if le.OnStopped != nil {
					le.OnStopped()
				}
				// 재획득 시도
				go le.Run(stop)
				return
			}
		}
	}
}

func (le *LeaderElector) release() {
	if le.IsLeader.Load() {
		le.IsLeader.Store(false)
		le.Lease.mu.Lock()
		if le.Lease.HolderIdentity == le.Identity {
			le.Lease.HolderIdentity = ""
		}
		le.Lease.mu.Unlock()
	}
}

// --- Status Manager: 배치 Status 업데이트 ---

// StatusKey는 상태를 추적할 리소스의 키이다.
type StatusKey struct {
	Kind      string
	Name      string
	Namespace string
}

func (sk StatusKey) String() string {
	return fmt.Sprintf("%s/%s/%s", sk.Kind, sk.Namespace, sk.Name)
}

// ResourceStatus는 리소스의 상태 정보이다.
type ResourceStatus struct {
	Conditions      []Condition
	ObservedGen     int64
	ResourceVersion string
}

type Condition struct {
	Type    string
	Status  string
	Message string
	LastTransitionTime time.Time
}

// StatusController는 Status 배치 업데이트를 관리한다.
// 실제: pilot/pkg/status/state.go의 StatusController
type StatusController struct {
	mu       sync.Mutex
	name     string
	enabled  bool
	pending  map[StatusKey]*ResourceStatus // 업데이트 대기열
	written  map[StatusKey]*ResourceStatus // 마지막 쓴 상태
	interval time.Duration
	writes   int64
	skipped  int64
}

func NewStatusController(name string, interval time.Duration) *StatusController {
	return &StatusController{
		name:     name,
		interval: interval,
		pending:  make(map[StatusKey]*ResourceStatus),
		written:  make(map[StatusKey]*ResourceStatus),
	}
}

func (sc *StatusController) SetEnabled(enabled bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.enabled = enabled
	if enabled {
		fmt.Printf("  [StatusCtrl-%s] 활성화됨 (리더로 선출)\n", sc.name)
	} else {
		fmt.Printf("  [StatusCtrl-%s] 비활성화됨 (리더 상실)\n", sc.name)
		sc.pending = make(map[StatusKey]*ResourceStatus)
	}
}

// EnqueueStatusUpdate는 상태 업데이트를 대기열에 추가한다.
// 중복 업데이트를 방지: 이전에 쓴 것과 동일하면 스킵.
func (sc *StatusController) EnqueueStatusUpdate(key StatusKey, status *ResourceStatus) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if !sc.enabled {
		return
	}

	// 중복 방지: 마지막 기록과 동일하면 스킵
	if prev, ok := sc.written[key]; ok {
		if len(prev.Conditions) == len(status.Conditions) &&
			prev.ObservedGen == status.ObservedGen {
			sc.skipped++
			return
		}
	}

	sc.pending[key] = status
}

// Flush는 대기열의 모든 업데이트를 한 번에 적용한다.
func (sc *StatusController) Flush() int {
	sc.mu.Lock()
	if !sc.enabled || len(sc.pending) == 0 {
		sc.mu.Unlock()
		return 0
	}
	batch := sc.pending
	sc.pending = make(map[StatusKey]*ResourceStatus)
	sc.mu.Unlock()

	count := 0
	for key, status := range batch {
		fmt.Printf("  [StatusCtrl-%s] Status 업데이트: %s (conditions=%d, gen=%d)\n",
			sc.name, key, len(status.Conditions), status.ObservedGen)
		sc.mu.Lock()
		sc.written[key] = status
		sc.writes++
		sc.mu.Unlock()
		count++
	}
	return count
}

func (sc *StatusController) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(sc.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sc.Flush()
		}
	}
}

func (sc *StatusController) Stats() (writes, skipped int64) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.writes, sc.skipped
}

// --- Distribution Reporter: 설정 분산 추적 ---

// DistributionReporter는 설정이 프록시에 실제로 적용되었는지 추적한다.
// 실제: pilot/pkg/status/distribution.go
type DistributionReporter struct {
	mu        sync.Mutex
	resources map[StatusKey]*DistributionState
	statusCtrl *StatusController
}

type DistributionState struct {
	TotalProxies   int
	AckedProxies   int
	NackedProxies  int
	Generation     int64
}

func NewDistributionReporter(ctrl *StatusController) *DistributionReporter {
	return &DistributionReporter{
		resources:  make(map[StatusKey]*DistributionState),
		statusCtrl: ctrl,
	}
}

// RegisterResource는 분산 추적 대상 리소스를 등록한다.
func (dr *DistributionReporter) RegisterResource(key StatusKey, totalProxies int, gen int64) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.resources[key] = &DistributionState{
		TotalProxies: totalProxies,
		Generation:   gen,
	}
}

// ReportAck는 프록시가 설정을 성공적으로 적용했음을 보고한다.
func (dr *DistributionReporter) ReportAck(key StatusKey) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if state, ok := dr.resources[key]; ok {
		state.AckedProxies++
		if state.AckedProxies >= state.TotalProxies {
			// 모든 프록시가 적용 완료 → Reconciled 상태 업데이트
			dr.statusCtrl.EnqueueStatusUpdate(key, &ResourceStatus{
				Conditions: []Condition{{
					Type:    "Reconciled",
					Status:  "True",
					Message: fmt.Sprintf("%d/%d proxies updated", state.AckedProxies, state.TotalProxies),
					LastTransitionTime: time.Now(),
				}},
				ObservedGen: state.Generation,
			})
		}
	}
}

// ReportNack는 프록시가 설정 적용에 실패했음을 보고한다.
func (dr *DistributionReporter) ReportNack(key StatusKey, reason string) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if state, ok := dr.resources[key]; ok {
		state.NackedProxies++
		dr.statusCtrl.EnqueueStatusUpdate(key, &ResourceStatus{
			Conditions: []Condition{{
				Type:    "Reconciled",
				Status:  "False",
				Message: fmt.Sprintf("NACK from proxy: %s (%d/%d)", reason, state.NackedProxies, state.TotalProxies),
				LastTransitionTime: time.Now(),
			}},
			ObservedGen: state.Generation,
		})
	}
}

// --- Istiod 인스턴스 시뮬레이션 ---

type IstiodInstance struct {
	ID            string
	leaderElector *LeaderElector
	statusCtrl    *StatusController
	distReporter  *DistributionReporter
}

func NewIstiodInstance(id string, leases map[string]*Lease) *IstiodInstance {
	statusCtrl := NewStatusController(id, 20*time.Millisecond)
	inst := &IstiodInstance{
		ID:           id,
		statusCtrl:   statusCtrl,
		distReporter: NewDistributionReporter(statusCtrl),
	}

	// 리더 선출: "status-controller" lease
	statusLease := leases["istio-status-controller"]
	inst.leaderElector = NewLeaderElector(
		"status-controller",
		id,
		statusLease,
		func() {
			fmt.Printf("[%s] 리더로 선출됨!\n", id)
			statusCtrl.SetEnabled(true)
		},
		func() {
			fmt.Printf("[%s] 리더 상실.\n", id)
			statusCtrl.SetEnabled(false)
		},
	)

	return inst
}

func (inst *IstiodInstance) Run(stop <-chan struct{}) {
	go inst.leaderElector.Run(stop)
	go inst.statusCtrl.Run(stop)
}

// --- 메인 함수 ---

func main() {
	fmt.Println("=== Istio 리더 선출 + Status 관리 시뮬레이션 ===")
	fmt.Println()

	// 1. 공유 Lease 생성 (Kubernetes Lease 오브젝트 시뮬레이션)
	leases := map[string]*Lease{
		"istio-status-controller":   NewLease("istio-status-controller", 200),
		"istio-webhook-patcher":     NewLease("istio-webhook-patcher", 200),
		"istio-gateway-deployment":  NewLease("istio-gateway-deployment", 200),
	}

	fmt.Println("--- 1단계: 다중 istiod 인스턴스 기동 ---")
	stop := make(chan struct{})

	instances := make([]*IstiodInstance, 3)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("istiod-%d", i)
		instances[i] = NewIstiodInstance(id, leases)
		instances[i].Run(stop)
	}

	time.Sleep(200 * time.Millisecond)

	// 리더 확인
	fmt.Println()
	fmt.Println("--- 2단계: 리더 확인 ---")
	for _, inst := range instances {
		role := "팔로워"
		if inst.leaderElector.IsLeader.Load() {
			role = "★ 리더"
		}
		fmt.Printf("  %s: %s\n", inst.ID, role)
	}

	// 3. Distribution Reporting 시뮬레이션
	fmt.Println()
	fmt.Println("--- 3단계: 설정 분산 추적 (Distribution Reporting) ---")

	// 리더 인스턴스 찾기
	var leader *IstiodInstance
	for _, inst := range instances {
		if inst.leaderElector.IsLeader.Load() {
			leader = inst
			break
		}
	}

	if leader != nil {
		// VirtualService 리소스의 분산 추적 등록
		vsKey := StatusKey{Kind: "VirtualService", Namespace: "default", Name: "reviews"}
		leader.distReporter.RegisterResource(vsKey, 3, 1)

		// 프록시 ACK 시뮬레이션
		fmt.Println("  프록시 ACK 전송 중...")
		leader.distReporter.ReportAck(vsKey)
		leader.distReporter.ReportAck(vsKey)
		leader.distReporter.ReportAck(vsKey) // 3/3 → Reconciled: True

		time.Sleep(50 * time.Millisecond) // flush 대기
	}

	// 4. 중복 쓰기 방지 시뮬레이션
	fmt.Println()
	fmt.Println("--- 4단계: 중복 쓰기 방지 ---")
	if leader != nil {
		drKey := StatusKey{Kind: "DestinationRule", Namespace: "default", Name: "reviews"}
		// 동일한 상태를 여러 번 큐잉
		for i := 0; i < 5; i++ {
			leader.statusCtrl.EnqueueStatusUpdate(drKey, &ResourceStatus{
				Conditions: []Condition{{Type: "Ready", Status: "True"}},
				ObservedGen: 1,
			})
		}
		leader.statusCtrl.Flush()
		time.Sleep(30 * time.Millisecond)

		// 동일한 상태 다시 시도 → 스킵됨
		for i := 0; i < 3; i++ {
			leader.statusCtrl.EnqueueStatusUpdate(drKey, &ResourceStatus{
				Conditions: []Condition{{Type: "Ready", Status: "True"}},
				ObservedGen: 1,
			})
		}

		writes, skipped := leader.statusCtrl.Stats()
		fmt.Printf("  총 쓰기: %d, 스킵됨: %d\n", writes, skipped)
	}

	// 5. 리더 failover 시뮬레이션
	fmt.Println()
	fmt.Println("--- 5단계: 리더 Failover ---")
	if leader != nil {
		fmt.Printf("  현재 리더 %s 종료 중...\n", leader.ID)
		leader.leaderElector.release()
		// Lease 만료 대기
		leases["istio-status-controller"].mu.Lock()
		leases["istio-status-controller"].HolderIdentity = ""
		leases["istio-status-controller"].mu.Unlock()
	}
	time.Sleep(200 * time.Millisecond)

	// 새 리더 확인
	var newLeader string
	for _, inst := range instances {
		if inst.leaderElector.IsLeader.Load() {
			newLeader = inst.ID
		}
	}
	if newLeader != "" {
		fmt.Printf("  새 리더: %s\n", newLeader)
	}

	// 6. NACK 처리 시뮬레이션
	fmt.Println()
	fmt.Println("--- 6단계: NACK 처리 ---")
	for _, inst := range instances {
		if inst.leaderElector.IsLeader.Load() {
			gwKey := StatusKey{Kind: "Gateway", Namespace: "istio-system", Name: "main-gw"}
			inst.distReporter.RegisterResource(gwKey, 3, 2)
			inst.distReporter.ReportAck(gwKey)
			inst.distReporter.ReportNack(gwKey, "invalid config: unknown host")
			time.Sleep(50 * time.Millisecond)
			break
		}
	}

	close(stop)
	time.Sleep(50 * time.Millisecond)

	// 요약
	fmt.Println()
	fmt.Println("=== 요약 ===")
	fmt.Println("  - Kubernetes Lease 기반 리더 선출: 다중 istiod 중 하나만 Status 쓰기")
	fmt.Println("  - 배치 Status 업데이트: 중복 쓰기 방지로 API 서버 부하 감소")
	fmt.Println("  - Distribution Reporting: 설정이 모든 프록시에 적용되었는지 추적")
	fmt.Println("  - Failover: 리더 다운 시 자동 인수")

	_ = strings.Join(nil, "")
}
