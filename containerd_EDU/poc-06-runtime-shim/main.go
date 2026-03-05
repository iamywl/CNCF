// containerd Shim 프로세스 생명주기 시뮬레이션
//
// containerd는 각 컨테이너마다 독립적인 shim 프로세스를 실행하여
// 컨테이너 런타임(runc)과 통신한다. shim은 containerd 재시작과 독립적으로
// 컨테이너를 관리할 수 있는 핵심 아키텍처 컴포넌트이다.
//
// 참조 소스코드:
//   - pkg/shim/shim.go                              (Manager, BootstrapParams, Run, StartOpts)
//   - cmd/containerd-shim-runc-v2/task/service.go   (TaskService, Create, Start, Kill, Delete, forward)

package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. BootstrapParams: Shim 시작 시 반환하는 연결 정보
// 참조: pkg/shim/shim.go - BootstrapParams struct
// ============================================================================

// BootstrapParams는 shim.Start() 호출 시 JSON으로 stdout에 출력되어
// containerd가 shim에 연결할 수 있는 정보를 제공한다.
type BootstrapParams struct {
	Version  int    // shim 프로토콜 버전 (v2 shim은 2)
	Address  string // containerd가 연결할 주소 (unix socket 경로)
	Protocol string // 통신 프로토콜 ("ttrpc" 또는 "grpc")
}

// ============================================================================
// 2. StartOpts: Shim 시작 옵션
// 참조: pkg/shim/shim.go - StartOpts struct
// ============================================================================

type StartOpts struct {
	Address      string // containerd 의 gRPC 주소
	TTRPCAddress string // containerd 의 TTRPC 주소
	Debug        bool   // 디버그 모드
}

// ============================================================================
// 3. StopStatus: Shim 종료 상태
// 참조: pkg/shim/shim.go - StopStatus struct
// ============================================================================

type StopStatus struct {
	Pid        int
	ExitStatus int
	ExitedAt   time.Time
}

// ============================================================================
// 4. 이벤트 시스템: Shim → containerd 이벤트 포워딩
// 참조: cmd/containerd-shim-runc-v2/task/service.go - forward(), send()
// containerd의 shim은 이벤트 채널을 통해 비동기적으로 이벤트를 containerd에 전달한다.
// ============================================================================

// Event는 shim이 containerd로 전달하는 이벤트.
type Event struct {
	Topic       string // 이벤트 토픽 (예: "/tasks/create", "/tasks/start")
	ContainerID string
	Pid         int
	ExitStatus  int
	Timestamp   time.Time
}

// Publisher는 이벤트를 containerd로 전달하는 인터페이스.
// 참조: pkg/shim/shim.go - Publisher interface
type Publisher interface {
	Publish(topic string, event Event) error
	Close() error
}

// simPublisher는 TTRPC 기반 이벤트 퍼블리셔를 시뮬레이션한다.
// 실제로는 TTRPC 연결을 통해 containerd에 이벤트를 전달하지만,
// 여기서는 로그로 출력한다.
type simPublisher struct {
	address string
	mu      sync.Mutex
	closed  bool
}

func (p *simPublisher) Publish(topic string, event Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("publisher closed")
	}
	fmt.Printf("    [이벤트 전달] %s → containerd(%s): container=%s, pid=%d\n",
		topic, p.address, event.ContainerID, event.Pid)
	return nil
}

func (p *simPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	fmt.Printf("    [Publisher] TTRPC 연결 닫힘 (address=%s)\n", p.address)
	return nil
}

// ============================================================================
// 5. TaskService: Shim이 제공하는 TTRPC 서비스
// 참조: cmd/containerd-shim-runc-v2/task/service.go - service struct
// ============================================================================

// 프로세스 상태
type ProcessStatus string

const (
	StatusCreated ProcessStatus = "created"
	StatusRunning ProcessStatus = "running"
	StatusStopped ProcessStatus = "stopped"
)

// container는 shim이 관리하는 컨테이너 정보.
type container struct {
	id     string
	pid    int
	status ProcessStatus
	bundle string
	execs  map[string]*execProcess // exec 프로세스들
}

type execProcess struct {
	id     string
	pid    int
	status ProcessStatus
}

// TaskService는 containerd-shim-runc-v2가 제공하는 Task API 서비스.
// 참조: cmd/containerd-shim-runc-v2/task/service.go - service struct
type TaskService struct {
	mu         sync.Mutex
	containers map[string]*container
	events     chan Event      // 이벤트 버퍼 채널 (128 크기)
	publisher  Publisher       // TTRPC 퍼블리셔
	ec         chan reapEvent  // 프로세스 reaping 채널
	running    map[int]string  // pid → container ID 매핑
}

// reapEvent는 프로세스 종료를 알리는 이벤트.
// 참조: cmd/containerd-shim-runc-v2/task/service.go - processExits()
type reapEvent struct {
	pid    int
	status int
}

func newTaskService(publisher Publisher) *TaskService {
	s := &TaskService{
		containers: make(map[string]*container),
		events:     make(chan Event, 128), // 참조: service.go line 78 - make(chan interface{}, 128)
		publisher:  publisher,
		ec:         make(chan reapEvent, 32),
		running:    make(map[int]string),
	}
	// 이벤트 포워딩 goroutine 시작
	// 참조: service.go - go s.forward(ctx, publisher)
	go s.forward()
	// 프로세스 reaping goroutine 시작
	// 참조: service.go - go s.processExits()
	go s.processExits()
	return s
}

// forward는 이벤트 채널에서 이벤트를 읽어 publisher를 통해 containerd로 전달한다.
// 참조: cmd/containerd-shim-runc-v2/task/service.go - func (s *service) forward(...)
func (s *TaskService) forward() {
	for e := range s.events {
		if err := s.publisher.Publish(e.Topic, e); err != nil {
			fmt.Printf("    [forward 에러] %v\n", err)
		}
	}
	s.publisher.Close()
}

// processExits는 프로세스 종료 이벤트를 처리한다.
// 참조: cmd/containerd-shim-runc-v2/task/service.go - func (s *service) processExits()
func (s *TaskService) processExits() {
	for e := range s.ec {
		s.mu.Lock()
		containerID, ok := s.running[e.pid]
		if ok {
			delete(s.running, e.pid)
			if c, exists := s.containers[containerID]; exists {
				c.status = StatusStopped
				s.events <- Event{
					Topic:       "/tasks/exit",
					ContainerID: containerID,
					Pid:         e.pid,
					ExitStatus:  e.status,
					Timestamp:   time.Now(),
				}
			}
		}
		s.mu.Unlock()
	}
}

// send는 이벤트를 events 채널에 보낸다.
// 참조: service.go - func (s *service) send(evt interface{})
func (s *TaskService) send(evt Event) {
	s.events <- evt
}

// Create는 새 컨테이너를 생성한다.
// 참조: service.go - func (s *service) Create(ctx, r) (*CreateTaskResponse, error)
func (s *TaskService) Create(id, bundle string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.containers[id]; exists {
		return 0, fmt.Errorf("container %s already exists", id)
	}

	pid := rand.Intn(90000) + 10000 // 시뮬레이션 PID
	c := &container{
		id:     id,
		pid:    pid,
		status: StatusCreated,
		bundle: bundle,
		execs:  make(map[string]*execProcess),
	}
	s.containers[id] = c

	s.send(Event{
		Topic:       "/tasks/create",
		ContainerID: id,
		Pid:         pid,
		Timestamp:   time.Now(),
	})

	return pid, nil
}

// Start는 생성된 컨테이너의 init 프로세스를 시작한다.
// 참조: service.go - func (s *service) Start(ctx, r) (*StartResponse, error)
func (s *TaskService) Start(id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers[id]
	if !ok {
		return 0, fmt.Errorf("container not created: %s", id)
	}
	if c.status != StatusCreated {
		return 0, fmt.Errorf("container %s is not in created state", id)
	}

	c.status = StatusRunning
	s.running[c.pid] = id

	s.send(Event{
		Topic:       "/tasks/start",
		ContainerID: id,
		Pid:         c.pid,
		Timestamp:   time.Now(),
	})

	return c.pid, nil
}

// Kill은 컨테이너 프로세스에 시그널을 보낸다.
// 참조: service.go - func (s *service) Kill(ctx, r) (*Empty, error)
func (s *TaskService) Kill(id string, signal int) error {
	s.mu.Lock()
	c, ok := s.containers[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("container not created: %s", id)
	}
	pid := c.pid
	s.mu.Unlock()

	fmt.Printf("    [Kill] container=%s, pid=%d, signal=%d\n", id, pid, signal)

	// 프로세스 reaping 시뮬레이션: Kill 후 비동기적으로 종료 이벤트 발생
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.ec <- reapEvent{pid: pid, status: 0}
	}()

	return nil
}

// Delete는 컨테이너를 삭제한다.
// 참조: service.go - func (s *service) Delete(ctx, r) (*DeleteResponse, error)
func (s *TaskService) Delete(id string) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers[id]
	if !ok {
		return 0, 0, fmt.Errorf("container not created: %s", id)
	}
	if c.status == StatusRunning {
		return 0, 0, fmt.Errorf("container %s is still running", id)
	}

	pid := c.pid
	delete(s.containers, id)

	s.send(Event{
		Topic:       "/tasks/delete",
		ContainerID: id,
		Pid:         pid,
		ExitStatus:  0,
		Timestamp:   time.Now(),
	})

	return pid, 0, nil
}

// Exec는 실행 중인 컨테이너에 추가 프로세스를 생성한다.
// 참조: service.go - func (s *service) Exec(ctx, r) (*Empty, error)
func (s *TaskService) Exec(containerID, execID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers[containerID]
	if !ok {
		return 0, fmt.Errorf("container not created: %s", containerID)
	}
	if c.status != StatusRunning {
		return 0, fmt.Errorf("container %s is not running", containerID)
	}
	if _, exists := c.execs[execID]; exists {
		return 0, fmt.Errorf("exec %s already exists", execID)
	}

	execPid := rand.Intn(90000) + 10000
	c.execs[execID] = &execProcess{
		id:     execID,
		pid:    execPid,
		status: StatusCreated,
	}

	s.send(Event{
		Topic:       "/tasks/exec-added",
		ContainerID: containerID,
		Pid:         execPid,
		Timestamp:   time.Now(),
	})

	return execPid, nil
}

// ============================================================================
// 6. ShimManager: Shim 프로세스 관리자
// 참조: pkg/shim/shim.go - Manager interface
// ============================================================================

// ShimManager는 shim 프로세스의 시작/종료를 관리한다.
// 실제로는 exec.Command로 shim 바이너리를 실행하지만,
// 여기서는 TaskService를 직접 생성하여 시뮬레이션한다.
type ShimManager struct {
	mu     sync.Mutex
	shims  map[string]*ShimProcess
	name   string
}

type ShimProcess struct {
	ID          string
	Pid         int
	Service     *TaskService
	Publisher   Publisher
	BootParams  BootstrapParams
	StartedAt   time.Time
}

func NewShimManager(name string) *ShimManager {
	return &ShimManager{
		shims: make(map[string]*ShimProcess),
		name:  name,
	}
}

// Name은 shim 런타임 이름을 반환한다.
// 참조: pkg/shim/shim.go - Manager.Name()
func (m *ShimManager) Name() string {
	return m.name
}

// Start는 새 shim 프로세스를 시작한다.
// 실제 containerd에서는 exec.Command로 shim 바이너리를 실행하고,
// stdout에서 BootstrapParams JSON을 읽어 TTRPC 연결을 설정한다.
// 참조: pkg/shim/shim.go - Manager.Start(), Run() 함수의 "start" 액션 처리
func (m *ShimManager) Start(id string, opts StartOpts) (BootstrapParams, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.shims[id]; exists {
		return BootstrapParams{}, fmt.Errorf("shim already exists for %s", id)
	}

	shimPid := rand.Intn(90000) + 10000
	socketAddr := fmt.Sprintf("unix:///run/containerd/s/%x", rand.Int63())

	// TTRPC publisher 생성 (shim → containerd 이벤트 전달)
	publisher := &simPublisher{address: opts.TTRPCAddress}

	// TaskService 생성 (shim이 제공하는 TTRPC 서비스)
	service := newTaskService(publisher)

	params := BootstrapParams{
		Version:  2,
		Address:  socketAddr,
		Protocol: "ttrpc",
	}

	m.shims[id] = &ShimProcess{
		ID:         id,
		Pid:        shimPid,
		Service:    service,
		Publisher:  publisher,
		BootParams: params,
		StartedAt:  time.Now(),
	}

	fmt.Printf("  [ShimManager] Shim 시작: id=%s, pid=%d, address=%s, protocol=%s\n",
		id, shimPid, socketAddr, params.Protocol)

	return params, nil
}

// Stop은 shim 프로세스를 종료한다.
// 참조: pkg/shim/shim.go - Manager.Stop()
func (m *ShimManager) Stop(id string) (StopStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	shim, ok := m.shims[id]
	if !ok {
		return StopStatus{}, fmt.Errorf("shim not found for %s", id)
	}

	// 이벤트 채널 닫기 → forward goroutine 종료 → publisher.Close() 호출
	close(shim.Service.events)
	close(shim.Service.ec)

	status := StopStatus{
		Pid:        shim.Pid,
		ExitStatus: 0,
		ExitedAt:   time.Now(),
	}
	delete(m.shims, id)

	fmt.Printf("  [ShimManager] Shim 종료: id=%s, pid=%d\n", id, shim.Pid)
	return status, nil
}

// GetService는 shim의 TaskService를 반환한다.
// 실제로는 TTRPC 클라이언트를 통해 접근하지만 여기서는 직접 참조.
func (m *ShimManager) GetService(id string) (*TaskService, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	shim, ok := m.shims[id]
	if !ok {
		return nil, fmt.Errorf("shim not found for %s", id)
	}
	return shim.Service, nil
}

// ============================================================================
// 7. main: 전체 Shim 생명주기 시뮬레이션
// ============================================================================

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Println("========================================")
	fmt.Println("containerd Shim 프로세스 생명주기 시뮬레이션")
	fmt.Println("========================================")
	fmt.Println()

	// ----- 시나리오 1: Shim 시작 및 BootstrapParams -----
	fmt.Println("--- 시나리오 1: Shim Manager를 통한 Shim 프로세스 시작 ---")
	fmt.Println()

	manager := NewShimManager("io.containerd.runc.v2")
	fmt.Printf("  Runtime: %s\n\n", manager.Name())

	// containerd가 shim을 시작
	// 실제: exec.Command("containerd-shim-runc-v2", "-id", id, "start")
	params, err := manager.Start("container-abc", StartOpts{
		Address:      "unix:///run/containerd/containerd.sock",
		TTRPCAddress: "unix:///run/containerd/containerd.sock.ttrpc",
		Debug:        false,
	})
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}

	fmt.Printf("\n  BootstrapParams (shim stdout → containerd):\n")
	fmt.Printf("    Version:  %d\n", params.Version)
	fmt.Printf("    Address:  %s\n", params.Address)
	fmt.Printf("    Protocol: %s\n", params.Protocol)

	// ----- 시나리오 2: TTRPC TaskService를 통한 컨테이너 관리 -----
	fmt.Println()
	fmt.Println("--- 시나리오 2: TTRPC TaskService 호출 (Create → Start → Exec → Kill → Delete) ---")
	fmt.Println()

	svc, _ := manager.GetService("container-abc")

	// Create: 컨테이너 생성
	fmt.Println("  [1] Create:")
	pid, err := svc.Create("container-abc", "/run/containerd/io.containerd.runtime.v2.task/default/container-abc")
	if err != nil {
		fmt.Printf("      에러: %v\n", err)
		return
	}
	fmt.Printf("      컨테이너 생성 완료: pid=%d, status=created\n", pid)
	time.Sleep(100 * time.Millisecond) // 이벤트 전달 대기

	// Start: init 프로세스 시작
	fmt.Println()
	fmt.Println("  [2] Start:")
	startPid, err := svc.Start("container-abc")
	if err != nil {
		fmt.Printf("      에러: %v\n", err)
		return
	}
	fmt.Printf("      프로세스 시작: pid=%d, status=running\n", startPid)
	time.Sleep(100 * time.Millisecond)

	// Exec: 추가 프로세스 실행
	fmt.Println()
	fmt.Println("  [3] Exec (추가 프로세스):")
	execPid, err := svc.Exec("container-abc", "exec-shell")
	if err != nil {
		fmt.Printf("      에러: %v\n", err)
		return
	}
	fmt.Printf("      exec 프로세스 추가: execID=exec-shell, pid=%d\n", execPid)
	time.Sleep(100 * time.Millisecond)

	// Kill: 시그널 전송 (SIGTERM=15)
	fmt.Println()
	fmt.Println("  [4] Kill (SIGTERM):")
	err = svc.Kill("container-abc", 15)
	if err != nil {
		fmt.Printf("      에러: %v\n", err)
		return
	}
	// processExits 처리 대기
	time.Sleep(200 * time.Millisecond)

	// Delete: 컨테이너 삭제
	fmt.Println()
	fmt.Println("  [5] Delete:")
	delPid, exitCode, err := svc.Delete("container-abc")
	if err != nil {
		fmt.Printf("      에러: %v\n", err)
		return
	}
	fmt.Printf("      컨테이너 삭제 완료: pid=%d, exitStatus=%d\n", delPid, exitCode)
	time.Sleep(100 * time.Millisecond)

	// ----- 시나리오 3: 여러 Shim 프로세스 관리 -----
	fmt.Println()
	fmt.Println("--- 시나리오 3: 여러 컨테이너에 대한 독립 Shim 프로세스 ---")
	fmt.Println()

	containers := []string{"web-server", "api-server", "worker"}
	for _, cid := range containers {
		p, err := manager.Start(cid, StartOpts{
			Address:      "unix:///run/containerd/containerd.sock",
			TTRPCAddress: "unix:///run/containerd/containerd.sock.ttrpc",
		})
		if err != nil {
			fmt.Printf("  에러: %v\n", err)
			continue
		}
		fmt.Printf("  Shim: id=%-15s address=%s\n", cid, p.Address)
	}

	fmt.Println()
	fmt.Println("  각 컨테이너의 Shim은 독립 프로세스로 실행됨:")
	fmt.Println("  containerd 재시작 시에도 Shim은 계속 동작")
	fmt.Println()
	fmt.Println("  ┌───────────┐     TTRPC      ┌──────────┐     runc      ┌───────────┐")
	fmt.Println("  │ containerd│ ◄──────────────►│  shim    │ ──────────── │ container │")
	fmt.Println("  │           │                 │ (per-ctr)│              │ process   │")
	fmt.Println("  └───────────┘                 └──────────┘              └───────────┘")
	fmt.Println("        │                             │")
	fmt.Println("        │  BootstrapParams (start)    │  events (forward)")
	fmt.Println("        │  StopStatus (delete)        │  reaping (processExits)")

	// ----- 시나리오 4: Shim 종료 -----
	fmt.Println()
	fmt.Println("--- 시나리오 4: Shim 종료 및 정리 ---")
	fmt.Println()

	for _, cid := range containers {
		status, err := manager.Stop(cid)
		if err != nil {
			fmt.Printf("  에러: %v\n", err)
			continue
		}
		fmt.Printf("  종료: id=%-15s pid=%d, exitStatus=%d\n", cid, status.Pid, status.ExitStatus)
	}

	// ----- 시나리오 5: 이벤트 흐름 요약 -----
	fmt.Println()
	fmt.Println("--- 이벤트 흐름 요약 ---")
	fmt.Println()
	events := []struct {
		action string
		topic  string
	}{
		{"Create", "/tasks/create"},
		{"Start", "/tasks/start"},
		{"Exec", "/tasks/exec-added"},
		{"ExecStart", "/tasks/exec-started"},
		{"Kill→exit", "/tasks/exit"},
		{"Delete", "/tasks/delete"},
	}
	fmt.Println("  Action         Topic                 흐름")
	fmt.Println("  " + strings.Repeat("-", 60))
	for _, e := range events {
		fmt.Printf("  %-14s %-22s shim.events → forward() → publisher.Publish()\n", e.action, e.topic)
	}

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
