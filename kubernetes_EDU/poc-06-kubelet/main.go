// poc-06-kubelet: 쿠버네티스 Kubelet Sync Loop 시뮬레이션
//
// Kubelet의 핵심 동작인 syncLoop를 구현한다.
// syncLoop는 여러 소스(채널)로부터 이벤트를 받아 파드를 관리한다:
//
//   configCh: API Server로부터 파드 스펙 변경사항
//   plegCh:   PLEG(Pod Lifecycle Event Generator)의 컨테이너 상태 변경
//   syncCh:   주기적 동기화 타이머
//   housekeepingCh: GC, 로그 정리 등 정비 작업
//
// 참조 소스:
//   - pkg/kubelet/kubelet.go (syncLoop, syncLoopIteration)
//   - pkg/kubelet/pleg/pleg.go (PodLifecycleEvent)
//   - pkg/kubelet/pod_workers.go (PodWorkers)
//   - pkg/kubelet/status/status_manager.go (StatusManager)
//
// 실행: go run main.go
package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 데이터 모델
// ============================================================================

// PodPhase는 파드의 생명주기 단계이다.
type PodPhase string

const (
	PodPending    PodPhase = "Pending"
	PodRunning    PodPhase = "Running"
	PodSucceeded  PodPhase = "Succeeded"
	PodFailed     PodPhase = "Failed"
	PodTerminated PodPhase = "Terminated"
)

// ContainerState는 컨테이너의 상태이다.
type ContainerState string

const (
	ContainerCreated    ContainerState = "Created"
	ContainerRunning    ContainerState = "Running"
	ContainerExited     ContainerState = "Exited"
	ContainerUnknown    ContainerState = "Unknown"
)

// Container는 파드 내의 컨테이너이다.
type Container struct {
	Name   string
	Image  string
	State  ContainerState
	ExitCode int
}

// Pod는 kubelet이 관리하는 파드이다.
type Pod struct {
	Name       string
	Namespace  string
	Containers []Container
	Phase      PodPhase
	NodeName   string
}

// ============================================================================
// Pod Worker 상태 머신
// 실제 소스: pkg/kubelet/pod_workers.go
//
// Pod Worker의 상태 전이:
//   (없음) → SyncPod → Running
//   Running → TerminatingPod → TerminatedPod → Cleanup
// ============================================================================

// WorkerState는 파드 워커의 상태이다.
type WorkerState string

const (
	WorkerIdle         WorkerState = "Idle"
	WorkerSyncing      WorkerState = "Syncing"
	WorkerRunning      WorkerState = "Running"
	WorkerTerminating  WorkerState = "Terminating"
	WorkerTerminated   WorkerState = "Terminated"
)

// PodWorker는 개별 파드의 생명주기를 관리하는 워커이다.
type PodWorker struct {
	podName string
	state   WorkerState
	pod     *Pod
}

// PodWorkers는 모든 파드 워커를 관리한다.
type PodWorkers struct {
	mu      sync.Mutex
	workers map[string]*PodWorker // podKey → PodWorker
	cri     *FakeCRI
}

func NewPodWorkers(cri *FakeCRI) *PodWorkers {
	return &PodWorkers{
		workers: make(map[string]*PodWorker),
		cri:     cri,
	}
}

// SyncPod는 파드를 동기화(시작)한다.
// 실제 소스: pkg/kubelet/pod_workers.go의 UpdatePod → managePodLoop → syncPod
func (pw *PodWorkers) SyncPod(pod *Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	key := pod.Namespace + "/" + pod.Name
	worker, exists := pw.workers[key]

	if !exists {
		// 새 파드 → 워커 생성 + SyncPod 실행
		worker = &PodWorker{podName: key, state: WorkerSyncing, pod: pod}
		pw.workers[key] = worker
		fmt.Printf("    [PodWorker] %s: Idle → Syncing (새 파드 시작)\n", key)

		// CRI를 통해 컨테이너 생성 + 시작
		go func() {
			for i := range pod.Containers {
				pw.cri.CreateContainer(pod.Name, &pod.Containers[i])
				pw.cri.StartContainer(pod.Name, &pod.Containers[i])
			}
			pw.mu.Lock()
			worker.state = WorkerRunning
			pod.Phase = PodRunning
			fmt.Printf("    [PodWorker] %s: Syncing → Running\n", key)
			pw.mu.Unlock()
		}()
	} else if worker.state == WorkerRunning {
		// 이미 실행 중 → 업데이트 동기화
		fmt.Printf("    [PodWorker] %s: Running (재동기화)\n", key)
	}
}

// TerminatePod는 파드를 종료한다.
func (pw *PodWorkers) TerminatePod(pod *Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	key := pod.Namespace + "/" + pod.Name
	worker, exists := pw.workers[key]
	if !exists {
		return
	}

	if worker.state == WorkerRunning || worker.state == WorkerSyncing {
		worker.state = WorkerTerminating
		fmt.Printf("    [PodWorker] %s: → Terminating\n", key)

		go func() {
			// CRI를 통해 컨테이너 정지
			for i := range pod.Containers {
				pw.cri.StopContainer(pod.Name, &pod.Containers[i])
			}
			pw.mu.Lock()
			worker.state = WorkerTerminated
			pod.Phase = PodTerminated
			fmt.Printf("    [PodWorker] %s: Terminating → Terminated\n", key)
			pw.mu.Unlock()
		}()
	}
}

// GetState는 파드 워커의 상태를 반환한다.
func (pw *PodWorkers) GetState(podKey string) WorkerState {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if w, ok := pw.workers[podKey]; ok {
		return w.state
	}
	return WorkerIdle
}

// ============================================================================
// Fake CRI (Container Runtime Interface)
// 실제 소스: staging/src/k8s.io/cri-api/pkg/apis/runtime/v1/api.proto
//
// CRI는 kubelet과 컨테이너 런타임(containerd, CRI-O) 사이의 gRPC 인터페이스이다.
// ============================================================================

// CRIInterface는 CRI의 핵심 인터페이스이다.
type CRIInterface interface {
	CreateContainer(podName string, container *Container)
	StartContainer(podName string, container *Container)
	StopContainer(podName string, container *Container)
	ListContainers(podName string) []Container
}

// FakeCRI는 CRI를 시뮬레이션한다.
type FakeCRI struct {
	mu         sync.Mutex
	containers map[string][]Container // podName → containers
}

func NewFakeCRI() *FakeCRI {
	return &FakeCRI{
		containers: make(map[string][]Container),
	}
}

func (cri *FakeCRI) CreateContainer(podName string, container *Container) {
	cri.mu.Lock()
	defer cri.mu.Unlock()
	container.State = ContainerCreated
	cri.containers[podName] = append(cri.containers[podName], *container)
	fmt.Printf("      [CRI] 컨테이너 생성: %s/%s (이미지=%s)\n", podName, container.Name, container.Image)
}

func (cri *FakeCRI) StartContainer(podName string, container *Container) {
	cri.mu.Lock()
	defer cri.mu.Unlock()
	container.State = ContainerRunning
	// containers 맵에서도 업데이트
	for i := range cri.containers[podName] {
		if cri.containers[podName][i].Name == container.Name {
			cri.containers[podName][i].State = ContainerRunning
			break
		}
	}
	fmt.Printf("      [CRI] 컨테이너 시작: %s/%s\n", podName, container.Name)
	time.Sleep(50 * time.Millisecond) // 시작 지연 시뮬레이션
}

func (cri *FakeCRI) StopContainer(podName string, container *Container) {
	cri.mu.Lock()
	defer cri.mu.Unlock()
	container.State = ContainerExited
	for i := range cri.containers[podName] {
		if cri.containers[podName][i].Name == container.Name {
			cri.containers[podName][i].State = ContainerExited
			break
		}
	}
	fmt.Printf("      [CRI] 컨테이너 정지: %s/%s\n", podName, container.Name)
	time.Sleep(30 * time.Millisecond) // 정지 지연 시뮬레이션
}

func (cri *FakeCRI) ListContainers(podName string) []Container {
	cri.mu.Lock()
	defer cri.mu.Unlock()
	return cri.containers[podName]
}

// ============================================================================
// PLEG (Pod Lifecycle Event Generator)
// 실제 소스: pkg/kubelet/pleg/pleg.go, pkg/kubelet/pleg/generic.go
//
// PLEG는 주기적으로 컨테이너 런타임에 컨테이너 상태를 질의하고,
// 이전 상태와 비교하여 변경사항을 이벤트로 발생시킨다.
// 기본 주기: 1초 (relistPeriod)
// ============================================================================

// PodLifecycleEventType은 PLEG 이벤트 종류이다.
type PodLifecycleEventType string

const (
	ContainerStarted PodLifecycleEventType = "ContainerStarted"
	ContainerDied    PodLifecycleEventType = "ContainerDied"
	ContainerChanged PodLifecycleEventType = "ContainerChanged"
)

// PodLifecycleEvent는 PLEG 이벤트이다.
type PodLifecycleEvent struct {
	PodName       string
	ContainerName string
	Type          PodLifecycleEventType
}

// PLEG는 파드 생명주기 이벤트 생성기이다.
type PLEG struct {
	cri            *FakeCRI
	eventCh        chan PodLifecycleEvent
	previousStates map[string]ContainerState // podName/containerName → 이전 상태
	mu             sync.Mutex
}

func NewPLEG(cri *FakeCRI) *PLEG {
	return &PLEG{
		cri:            cri,
		eventCh:        make(chan PodLifecycleEvent, 50),
		previousStates: make(map[string]ContainerState),
	}
}

// Watch는 PLEG 이벤트 채널을 반환한다.
func (p *PLEG) Watch() <-chan PodLifecycleEvent {
	return p.eventCh
}

// relist는 컨테이너 런타임에서 현재 상태를 가져와 이전 상태와 비교한다.
// 실제 소스: pkg/kubelet/pleg/generic.go의 relist 함수
func (p *PLEG) relist(podNames []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, podName := range podNames {
		containers := p.cri.ListContainers(podName)
		for _, c := range containers {
			key := podName + "/" + c.Name
			prevState, existed := p.previousStates[key]

			if !existed && c.State == ContainerRunning {
				// 새로 시작된 컨테이너
				p.previousStates[key] = c.State
				p.eventCh <- PodLifecycleEvent{
					PodName:       podName,
					ContainerName: c.Name,
					Type:          ContainerStarted,
				}
			} else if existed && prevState != c.State {
				// 상태 변경
				p.previousStates[key] = c.State
				if c.State == ContainerExited {
					p.eventCh <- PodLifecycleEvent{
						PodName:       podName,
						ContainerName: c.Name,
						Type:          ContainerDied,
					}
				} else {
					p.eventCh <- PodLifecycleEvent{
						PodName:       podName,
						ContainerName: c.Name,
						Type:          ContainerChanged,
					}
				}
			}
		}
	}
}

// Run은 PLEG를 주기적으로 실행한다.
func (p *PLEG) Run(podNames func() []string, stopCh <-chan struct{}) {
	ticker := time.NewTicker(200 * time.Millisecond) // 실제: 1초
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.relist(podNames())
		case <-stopCh:
			return
		}
	}
}

// ============================================================================
// Status Manager — API Server에 파드 상태를 보고한다.
// 실제 소스: pkg/kubelet/status/status_manager.go
// ============================================================================

// StatusManager는 파드 상태를 API Server에 보고한다.
type StatusManager struct {
	mu       sync.Mutex
	statuses map[string]PodPhase // podKey → phase
}

func NewStatusManager() *StatusManager {
	return &StatusManager{
		statuses: make(map[string]PodPhase),
	}
}

// UpdateStatus는 파드 상태를 업데이트한다.
func (sm *StatusManager) UpdateStatus(podKey string, phase PodPhase) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	old := sm.statuses[podKey]
	if old != phase {
		sm.statuses[podKey] = phase
		fmt.Printf("    [StatusManager] %s: %s → %s (API Server에 보고)\n", podKey, old, phase)
	}
}

// GetStatuses는 모든 파드 상태를 반환한다.
func (sm *StatusManager) GetStatuses() map[string]PodPhase {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cp := make(map[string]PodPhase)
	for k, v := range sm.statuses {
		cp[k] = v
	}
	return cp
}

// ============================================================================
// PodUpdate — API Server로부터 받는 파드 설정 변경사항
// 실제 소스: pkg/kubelet/types/pod_update.go
// ============================================================================

// PodOperation은 파드 조작 종류이다.
type PodOperation string

const (
	PodOpAdd    PodOperation = "ADD"
	PodOpUpdate PodOperation = "UPDATE"
	PodOpDelete PodOperation = "DELETE"
)

// PodUpdate는 API Server가 보내는 파드 변경사항이다.
type PodUpdate struct {
	Op   PodOperation
	Pod  Pod
}

// ============================================================================
// Kubelet — syncLoop의 핵심 구현
// 실제 소스: pkg/kubelet/kubelet.go
// ============================================================================

// Kubelet은 노드의 파드 관리자이다.
type Kubelet struct {
	nodeName      string
	pods          map[string]*Pod // 관리 중인 파드
	podsMu        sync.RWMutex
	podWorkers    *PodWorkers
	cri           *FakeCRI
	pleg          *PLEG
	statusManager *StatusManager
}

func NewKubelet(nodeName string) *Kubelet {
	cri := NewFakeCRI()
	return &Kubelet{
		nodeName:      nodeName,
		pods:          make(map[string]*Pod),
		podWorkers:    NewPodWorkers(cri),
		cri:           cri,
		pleg:          NewPLEG(cri),
		statusManager: NewStatusManager(),
	}
}

// syncLoop는 kubelet의 메인 이벤트 루프이다.
// 실제 소스: pkg/kubelet/kubelet.go의 syncLoop 함수
//
// 여러 채널에서 이벤트를 받아 처리한다:
// - configCh: API Server로부터의 파드 스펙 변경
// - plegCh: 컨테이너 상태 변경 감지
// - syncCh: 주기적 동기화 (기본 10초)
// - housekeepingCh: 정비 작업 (기본 2초)
func (kl *Kubelet) syncLoop(configCh <-chan PodUpdate, stopCh <-chan struct{}) {
	fmt.Printf("[Kubelet-%s] syncLoop 시작\n", kl.nodeName)

	// 타이머 설정
	syncTicker := time.NewTicker(500 * time.Millisecond) // 실제: 1초
	defer syncTicker.Stop()
	housekeepingTicker := time.NewTicker(800 * time.Millisecond) // 실제: 2초
	defer housekeepingTicker.Stop()

	// PLEG 시작
	plegCh := kl.pleg.Watch()
	go kl.pleg.Run(kl.getPodNames, stopCh)

	for {
		kl.syncLoopIteration(configCh, plegCh, syncTicker.C, housekeepingTicker.C, stopCh)

		select {
		case <-stopCh:
			fmt.Printf("[Kubelet-%s] syncLoop 종료\n", kl.nodeName)
			return
		default:
		}
	}
}

// syncLoopIteration는 syncLoop의 한 번 반복이다.
// 실제 소스: pkg/kubelet/kubelet.go의 syncLoopIteration 함수
func (kl *Kubelet) syncLoopIteration(
	configCh <-chan PodUpdate,
	plegCh <-chan PodLifecycleEvent,
	syncCh <-chan time.Time,
	housekeepingCh <-chan time.Time,
	stopCh <-chan struct{},
) {
	select {
	case update := <-configCh:
		// API Server로부터 파드 변경사항 수신
		kl.handlePodUpdate(update)

	case event := <-plegCh:
		// PLEG: 컨테이너 상태 변경 감지
		kl.handlePLEGEvent(event)

	case <-syncCh:
		// 주기적 동기화: 모든 파드의 상태를 확인
		kl.handleSyncTick()

	case <-housekeepingCh:
		// 정비 작업: 고아 컨테이너 정리, 로그 정리 등
		kl.handleHousekeeping()

	case <-stopCh:
		return
	}
}

// handlePodUpdate는 API Server로부터의 파드 변경사항을 처리한다.
func (kl *Kubelet) handlePodUpdate(update PodUpdate) {
	key := update.Pod.Namespace + "/" + update.Pod.Name
	fmt.Printf("\n  [SyncLoop-Config] %s 파드 %s\n", update.Op, key)

	kl.podsMu.Lock()
	defer kl.podsMu.Unlock()

	switch update.Op {
	case PodOpAdd:
		pod := update.Pod
		pod.NodeName = kl.nodeName
		kl.pods[key] = &pod
		kl.podWorkers.SyncPod(&pod)
		kl.statusManager.UpdateStatus(key, PodPending)

	case PodOpUpdate:
		if existing, ok := kl.pods[key]; ok {
			existing.Containers = update.Pod.Containers
			kl.podWorkers.SyncPod(existing)
		}

	case PodOpDelete:
		if existing, ok := kl.pods[key]; ok {
			kl.podWorkers.TerminatePod(existing)
			kl.statusManager.UpdateStatus(key, PodTerminated)
		}
	}
}

// handlePLEGEvent는 PLEG 이벤트를 처리한다.
func (kl *Kubelet) handlePLEGEvent(event PodLifecycleEvent) {
	fmt.Printf("\n  [SyncLoop-PLEG] 이벤트: %s — 파드=%s, 컨테이너=%s\n",
		event.Type, event.PodName, event.ContainerName)

	kl.podsMu.RLock()
	defer kl.podsMu.RUnlock()

	// 파드 키 찾기
	for key, pod := range kl.pods {
		if pod.Name == event.PodName {
			switch event.Type {
			case ContainerStarted:
				kl.statusManager.UpdateStatus(key, PodRunning)
			case ContainerDied:
				// 컨테이너가 죽었으면 재시작 시도 (RestartPolicy에 따라)
				fmt.Printf("    [SyncLoop-PLEG] 컨테이너 %s 종료 감지 → 재시작 검토\n", event.ContainerName)
				kl.statusManager.UpdateStatus(key, PodFailed)
			}
			break
		}
	}
}

// handleSyncTick은 주기적 동기화를 수행한다.
func (kl *Kubelet) handleSyncTick() {
	kl.podsMu.RLock()
	defer kl.podsMu.RUnlock()

	if len(kl.pods) == 0 {
		return
	}

	fmt.Printf("\n  [SyncLoop-Sync] 주기적 동기화 — %d개 파드 확인\n", len(kl.pods))
	for key, pod := range kl.pods {
		state := kl.podWorkers.GetState(key)
		fmt.Printf("    %s: Phase=%s, WorkerState=%s\n", key, pod.Phase, state)
	}
}

// handleHousekeeping은 정비 작업을 수행한다.
func (kl *Kubelet) handleHousekeeping() {
	kl.podsMu.RLock()
	count := len(kl.pods)
	kl.podsMu.RUnlock()

	if count > 0 {
		fmt.Printf("\n  [SyncLoop-Housekeeping] 정비 — 고아 컨테이너 확인, 이미지 GC, 로그 정리\n")
	}
}

// getPodNames는 관리 중인 파드 이름 목록을 반환한다.
func (kl *Kubelet) getPodNames() []string {
	kl.podsMu.RLock()
	defer kl.podsMu.RUnlock()
	names := make([]string, 0, len(kl.pods))
	for _, pod := range kl.pods {
		names = append(names, pod.Name)
	}
	return names
}

// ============================================================================
// 메인
// ============================================================================

func main() {
	fmt.Println("========================================")
	fmt.Println("Kubernetes Kubelet Sync Loop 시뮬레이션")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Println("syncLoop 채널 구조:")
	fmt.Println("  ┌─ configCh (API Server → 파드 스펙 변경)")
	fmt.Println("  ├─ plegCh   (PLEG → 컨테이너 상태 변경 감지)")
	fmt.Println("  ├─ syncCh   (타이머 → 주기적 동기화)")
	fmt.Println("  └─ housekeepingCh (타이머 → 정비 작업)")
	fmt.Println()
	fmt.Println("파드 워커 상태 머신:")
	fmt.Println("  Idle → Syncing → Running → Terminating → Terminated")
	fmt.Println()

	kubelet := NewKubelet("node-1")
	configCh := make(chan PodUpdate, 10)
	stopCh := make(chan struct{})

	// syncLoop 시작
	go kubelet.syncLoop(configCh, stopCh)
	time.Sleep(100 * time.Millisecond)

	// ── 1단계: 파드 추가 (API Server → configCh) ──
	fmt.Println("── 1단계: 파드 추가 (API Server → configCh) ──")
	configCh <- PodUpdate{
		Op: PodOpAdd,
		Pod: Pod{
			Name:      "nginx",
			Namespace: "default",
			Containers: []Container{
				{Name: "nginx", Image: "nginx:1.25"},
				{Name: "sidecar", Image: "envoy:1.28"},
			},
			Phase: PodPending,
		},
	}
	time.Sleep(500 * time.Millisecond)

	// ── 2단계: 두 번째 파드 추가 ──
	fmt.Println()
	fmt.Println("── 2단계: 두 번째 파드 추가 ──")
	configCh <- PodUpdate{
		Op: PodOpAdd,
		Pod: Pod{
			Name:      "redis",
			Namespace: "default",
			Containers: []Container{
				{Name: "redis", Image: "redis:7"},
			},
			Phase: PodPending,
		},
	}
	time.Sleep(600 * time.Millisecond)

	// ── 3단계: PLEG 이벤트 감지 대기 (자동 발생) ──
	fmt.Println()
	fmt.Println("── 3단계: PLEG가 컨테이너 상태 변경 감지 (자동) ──")
	time.Sleep(500 * time.Millisecond)

	// ── 4단계: 파드 삭제 ──
	fmt.Println()
	fmt.Println("── 4단계: 파드 삭제 (API Server → configCh → TerminatePod) ──")
	configCh <- PodUpdate{
		Op: PodOpDelete,
		Pod: Pod{Name: "redis", Namespace: "default"},
	}
	time.Sleep(500 * time.Millisecond)

	// ── 5단계: 컨테이너 비정상 종료 시뮬레이션 ──
	fmt.Println()
	fmt.Println("── 5단계: 컨테이너 비정상 종료 시뮬레이션 ──")
	// CRI에서 직접 컨테이너 정지 (실제로는 OOMKill, 프로세스 크래시 등)
	sidecar := &Container{Name: "sidecar", Image: "envoy:1.28"}
	kubelet.cri.StopContainer("nginx", sidecar)
	time.Sleep(500 * time.Millisecond)

	// ── 최종 상태 ──
	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("최종 상태 보고 (StatusManager)")
	fmt.Println("========================================")
	statuses := kubelet.statusManager.GetStatuses()
	for key, phase := range statuses {
		fmt.Printf("  %s: %s\n", key, phase)
	}

	close(stopCh)
	time.Sleep(100 * time.Millisecond)

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("핵심 포인트:")
	fmt.Println("========================================")
	fmt.Println("1. syncLoop: select로 4개 채널을 동시에 감시하는 이벤트 루프")
	fmt.Println("2. configCh: API Server로부터 파드 스펙 변경 수신 (ADD/UPDATE/DELETE)")
	fmt.Println("3. PLEG: 주기적으로 CRI에 컨테이너 상태 질의 → 변경사항 이벤트 발생")
	fmt.Println("4. PodWorker: 파드별 독립 goroutine으로 상태 머신 관리")
	fmt.Println("5. CRI: 컨테이너 런타임(containerd)과의 인터페이스")
	fmt.Println("6. StatusManager: 파드 상태를 API Server에 주기적 보고")

	_ = rand.Int()
	_ = strings.TrimSpace("")
}
