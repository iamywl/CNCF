// containerd CRI RunPodSandbox / CreateContainer 시뮬레이션
//
// containerd는 Kubernetes CRI(Container Runtime Interface)를 내장 구현하며,
// kubelet의 RunPodSandbox, CreateContainer, StartContainer 등의 호출을
// 내부 sandbox/container 관리 로직으로 변환한다.
//
// 참조 소스코드:
//   - internal/cri/server/sandbox_run.go       (RunPodSandbox)
//   - internal/cri/server/container_create.go  (CreateContainer, createContainer)
//   - internal/cri/server/container_start.go   (StartContainer)
//   - internal/cri/server/container_stop.go    (StopContainer)
//   - internal/cri/server/container_remove.go  (RemoveContainer)

package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. CRI 타입 정의
// ============================================================================

// SandboxState는 Pod Sandbox의 상태.
// 참조: internal/cri/store/sandbox/ - State 상수
type SandboxState int

const (
	SandboxStateUnknown SandboxState = iota
	SandboxStateReady
	SandboxStateNotReady
)

func (s SandboxState) String() string {
	switch s {
	case SandboxStateReady:
		return "SANDBOX_READY"
	case SandboxStateNotReady:
		return "SANDBOX_NOT_READY"
	default:
		return "SANDBOX_UNKNOWN"
	}
}

// ContainerState는 Container의 상태.
type ContainerState int

const (
	ContainerStateCreated ContainerState = iota
	ContainerStateRunning
	ContainerStateStopped
	ContainerStateRemoved
)

func (s ContainerState) String() string {
	switch s {
	case ContainerStateCreated:
		return "CONTAINER_CREATED"
	case ContainerStateRunning:
		return "CONTAINER_RUNNING"
	case ContainerStateStopped:
		return "CONTAINER_STOPPED"
	case ContainerStateRemoved:
		return "CONTAINER_REMOVED"
	default:
		return "CONTAINER_UNKNOWN"
	}
}

// PodSandboxConfig는 Kubernetes에서 전달하는 Sandbox 설정.
// 참조: k8s.io/cri-api/pkg/apis/runtime/v1 - PodSandboxConfig
type PodSandboxConfig struct {
	Metadata     *PodSandboxMetadata
	Hostname     string
	LogDirectory string
	DNSConfig    *DNSConfig
	Labels       map[string]string
	Annotations  map[string]string
	Linux        *LinuxPodSandboxConfig
}

type PodSandboxMetadata struct {
	Name      string
	UID       string
	Namespace string
	Attempt   uint32
}

type DNSConfig struct {
	Servers  []string
	Searches []string
}

type LinuxPodSandboxConfig struct {
	SecurityContext *LinuxSandboxSecurityContext
}

type LinuxSandboxSecurityContext struct {
	NamespaceOptions *NamespaceOption
}

type NamespaceOption struct {
	Network string // "POD" 또는 "NODE"
}

// ContainerConfig는 컨테이너 생성 설정.
// 참조: k8s.io/cri-api/pkg/apis/runtime/v1 - ContainerConfig
type ContainerConfig struct {
	Metadata *ContainerMetadata
	Image    *ImageSpec
	Command  []string
	Args     []string
	Envs     []KeyValue
	Labels   map[string]string
	LogPath  string
}

type ContainerMetadata struct {
	Name    string
	Attempt uint32
}

type ImageSpec struct {
	Image string
}

type KeyValue struct {
	Key   string
	Value string
}

// ============================================================================
// 2. 내부 저장소: Sandbox, Container
// ============================================================================

// Sandbox는 CRI의 PodSandbox 내부 표현.
// 참조: internal/cri/store/sandbox/sandbox.go - Sandbox, Metadata
type Sandbox struct {
	ID          string
	Name        string
	Config      *PodSandboxConfig
	State       SandboxState
	Pid         int
	CreatedAt   time.Time
	NetNSPath   string
	IP          string
	Containers  map[string]*Container // sandbox에 속한 컨테이너들
	RuntimeName string
	LeaseID     string
}

// Container는 CRI의 Container 내부 표현.
// 참조: internal/cri/store/container/container.go - Container, Metadata
type Container struct {
	ID         string
	Name       string
	SandboxID  string
	ImageRef   string
	Config     *ContainerConfig
	State      ContainerState
	Pid        int
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	SnapshotID string
}

// ============================================================================
// 3. CRI Service 시뮬레이션
// 참조: internal/cri/server/sandbox_run.go   - criService.RunPodSandbox()
// 참조: internal/cri/server/container_create.go - criService.CreateContainer()
// ============================================================================

// CRIService는 containerd의 CRI 구현체를 시뮬레이션한다.
type CRIService struct {
	mu             sync.Mutex
	sandboxes      map[string]*Sandbox
	containers     map[string]*Container
	nameIndex      map[string]string // name → ID (중복 방지)
	leases         map[string]string // leaseID → sandboxID
}

func NewCRIService() *CRIService {
	return &CRIService{
		sandboxes:  make(map[string]*Sandbox),
		containers: make(map[string]*Container),
		nameIndex:  make(map[string]string),
		leases:     make(map[string]string),
	}
}

// generateID는 고유 ID를 생성한다.
// 참조: internal/cri/util/id.go - GenerateID()
func generateID() string {
	const chars = "abcdef0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// makeSandboxName은 sandbox의 고유 이름을 생성한다.
// 참조: internal/cri/server/helpers.go - makeSandboxName()
func makeSandboxName(meta *PodSandboxMetadata) string {
	return fmt.Sprintf("%s_%s_%s_%d", meta.Name, meta.Namespace, meta.UID, meta.Attempt)
}

// makeContainerName은 container의 고유 이름을 생성한다.
func makeContainerName(cMeta *ContainerMetadata, pMeta *PodSandboxMetadata) string {
	return fmt.Sprintf("%s_%s_%s_%s_%d",
		cMeta.Name, pMeta.Name, pMeta.Namespace, pMeta.UID, cMeta.Attempt)
}

// ============================================================================
// 4. RunPodSandbox: Pod Sandbox 생성 및 시작
// 참조: internal/cri/server/sandbox_run.go - func (c *criService) RunPodSandbox(...)
// 흐름: ID생성 → 이름예약 → Lease → 네트워크 → SandboxCreate → SandboxStart
// ============================================================================

func (s *CRIService) RunPodSandbox(config *PodSandboxConfig, runtimeHandler string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. 고유 ID 생성
	// 참조: sandbox_run.go - id := util.GenerateID()
	id := generateID()
	metadata := config.Metadata
	if metadata == nil {
		return "", fmt.Errorf("sandbox config must include metadata")
	}
	name := makeSandboxName(metadata)
	fmt.Printf("    [1] ID 생성: id=%s, name=%s\n", id, name)

	// 2. 이름 예약 (중복 RunPodSandbox 방지)
	// 참조: sandbox_run.go - c.sandboxNameIndex.Reserve(name, id)
	if _, exists := s.nameIndex[name]; exists {
		return "", fmt.Errorf("sandbox name %q already reserved", name)
	}
	s.nameIndex[name] = id
	fmt.Printf("    [2] 이름 예약: %s → %s\n", name, id)

	// 3. Lease 생성 (리소스 보호)
	// 참조: sandbox_run.go - leaseSvc.Create(ctx, leases.WithID(id))
	leaseID := "lease-" + id
	s.leases[leaseID] = id
	fmt.Printf("    [3] Lease 생성: %s\n", leaseID)

	// 4. 네트워크 네임스페이스 설정 (host network가 아닌 경우)
	// 참조: sandbox_run.go - sandbox.NetNS, err = netns.NewNetNS(netnsMountDir)
	netNSPath := ""
	ip := ""
	hostNetwork := false
	if config.Linux != nil && config.Linux.SecurityContext != nil &&
		config.Linux.SecurityContext.NamespaceOptions != nil {
		if config.Linux.SecurityContext.NamespaceOptions.Network == "NODE" {
			hostNetwork = true
		}
	}

	if !hostNetwork {
		netNSPath = fmt.Sprintf("/var/run/netns/cni-%s", id[:8])
		ip = fmt.Sprintf("10.244.%d.%d", rand.Intn(255), rand.Intn(254)+1)
		fmt.Printf("    [4] 네트워크 설정: netns=%s, ip=%s\n", netNSPath, ip)

		// CNI 플러그인을 통한 네트워크 설정
		// 참조: sandbox_run.go - c.setupPodNetwork(ctx, &sandbox)
		fmt.Printf("        CNI Setup: veth 생성, IP 할당, 라우팅 설정\n")
	} else {
		fmt.Printf("    [4] 네트워크 설정: Host Network (CNI 스킵)\n")
	}

	// 5. Sandbox 생성 (sandboxService.CreateSandbox)
	// 참조: sandbox_run.go - c.sandboxService.CreateSandbox(ctx, sandboxInfo, ...)
	fmt.Printf("    [5] Sandbox 생성: runtime=%s\n", runtimeHandler)

	// 6. Sandbox 시작 (sandboxService.StartSandbox)
	// 참조: sandbox_run.go - ctrl, err := c.sandboxService.StartSandbox(ctx, sandbox.Sandboxer, id)
	pid := rand.Intn(90000) + 10000
	fmt.Printf("    [6] Sandbox 시작: pid=%d\n", pid)

	sandbox := &Sandbox{
		ID:          id,
		Name:        name,
		Config:      config,
		State:       SandboxStateReady,
		Pid:         pid,
		CreatedAt:   time.Now(),
		NetNSPath:   netNSPath,
		IP:          ip,
		Containers:  make(map[string]*Container),
		RuntimeName: runtimeHandler,
		LeaseID:     leaseID,
	}
	s.sandboxes[id] = sandbox

	// 7. 상태 업데이트: Ready
	// 참조: sandbox_run.go - status.State = sandboxstore.StateReady
	fmt.Printf("    [7] 상태 업데이트: %s\n", sandbox.State)

	// 8. Exit 모니터 시작
	// 참조: sandbox_run.go - c.startSandboxExitMonitor(...)
	fmt.Printf("    [8] Exit 모니터 시작\n")

	return id, nil
}

// ============================================================================
// 5. CreateContainer: Pod 내 컨테이너 생성
// 참조: internal/cri/server/container_create.go - func (c *criService) CreateContainer(...)
// 흐름: Sandbox조회 → ID생성 → 이미지확인 → Spec생성 → Snapshot → Container생성
// ============================================================================

func (s *CRIService) CreateContainer(podSandboxID string, config *ContainerConfig, sandboxConfig *PodSandboxConfig) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Sandbox 조회
	// 참조: container_create.go - sandbox, err := c.sandboxStore.Get(r.GetPodSandboxId())
	sandbox, ok := s.sandboxes[podSandboxID]
	if !ok {
		return "", fmt.Errorf("sandbox %s not found", podSandboxID)
	}
	if sandbox.State != SandboxStateReady {
		return "", fmt.Errorf("sandbox %s is not ready", podSandboxID)
	}
	fmt.Printf("    [1] Sandbox 조회: id=%s, state=%s\n", podSandboxID, sandbox.State)

	// 2. 고유 ID 및 이름 생성
	// 참조: container_create.go - id := util.GenerateID()
	id := generateID()
	name := makeContainerName(config.Metadata, sandboxConfig.Metadata)
	if _, exists := s.nameIndex[name]; exists {
		return "", fmt.Errorf("container name %q already reserved", name)
	}
	s.nameIndex[name] = id
	fmt.Printf("    [2] 컨테이너 ID/이름: id=%s, name=%s\n", id, name)

	// 3. 이미지 확인
	// 참조: container_create.go - image, err := c.LocalResolve(config.GetImage().GetImage())
	imageRef := config.Image.Image
	fmt.Printf("    [3] 이미지 확인: %s\n", imageRef)

	// 4. OCI Spec 생성
	// 참조: container_create.go - spec, err := c.buildContainerSpec(...)
	fmt.Printf("    [4] OCI Spec 생성: command=%v, envs=%d개\n", config.Command, len(config.Envs))

	// 5. Snapshot 준비 (이미지 레이어 → writeable 스냅샷)
	// 참조: container_create.go - customopts.WithNewSnapshot(...)
	snapshotID := fmt.Sprintf("snapshot-%s", id[:8])
	fmt.Printf("    [5] Snapshot 준비: %s (overlayfs)\n", snapshotID)

	// 6. containerd Container 메타데이터 생성
	// 참조: container_create.go - container, err := c.client.NewContainer(...)
	fmt.Printf("    [6] Container 메타데이터 저장\n")

	container := &Container{
		ID:         id,
		Name:       name,
		SandboxID:  podSandboxID,
		ImageRef:   imageRef,
		Config:     config,
		State:      ContainerStateCreated,
		CreatedAt:  time.Now(),
		SnapshotID: snapshotID,
	}
	s.containers[id] = container
	sandbox.Containers[id] = container

	fmt.Printf("    [7] 컨테이너 생성 완료: state=%s\n", container.State)

	return id, nil
}

// ============================================================================
// 6. StartContainer: 컨테이너 시작
// 참조: internal/cri/server/container_start.go - func (c *criService) StartContainer(...)
// ============================================================================

func (s *CRIService) StartContainer(containerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	container, ok := s.containers[containerID]
	if !ok {
		return fmt.Errorf("container %s not found", containerID)
	}
	if container.State != ContainerStateCreated {
		return fmt.Errorf("container %s is not in created state (current: %s)", containerID, container.State)
	}

	// 1. Task 생성 및 시작
	// 참조: container_start.go에서 Task Create → Start 호출
	pid := rand.Intn(90000) + 10000
	fmt.Printf("    [1] Task 생성: containerID=%s\n", containerID)
	fmt.Printf("    [2] Task 시작: pid=%d\n", pid)

	container.Pid = pid
	container.State = ContainerStateRunning
	container.StartedAt = time.Now()

	fmt.Printf("    [3] 상태 업데이트: %s\n", container.State)
	return nil
}

// ============================================================================
// 7. StopContainer: 컨테이너 중지
// 참조: internal/cri/server/container_stop.go - func (c *criService) StopContainer(...)
// ============================================================================

func (s *CRIService) StopContainer(containerID string, timeout int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	container, ok := s.containers[containerID]
	if !ok {
		return fmt.Errorf("container %s not found", containerID)
	}
	if container.State != ContainerStateRunning {
		fmt.Printf("    컨테이너 %s은 이미 %s 상태\n", containerID, container.State)
		return nil
	}

	// 1. SIGTERM 전송
	fmt.Printf("    [1] SIGTERM 전송: pid=%d, timeout=%ds\n", container.Pid, timeout)

	// 2. 타임아웃 후 SIGKILL (시뮬레이션에서는 즉시 처리)
	fmt.Printf("    [2] 프로세스 종료 대기\n")

	container.State = ContainerStateStopped
	container.FinishedAt = time.Now()
	container.ExitCode = 0

	fmt.Printf("    [3] 상태 업데이트: %s, exitCode=%d\n", container.State, container.ExitCode)
	return nil
}

// ============================================================================
// 8. RemoveContainer: 컨테이너 제거
// 참조: internal/cri/server/container_remove.go - func (c *criService) RemoveContainer(...)
// ============================================================================

func (s *CRIService) RemoveContainer(containerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	container, ok := s.containers[containerID]
	if !ok {
		return fmt.Errorf("container %s not found", containerID)
	}
	if container.State == ContainerStateRunning {
		return fmt.Errorf("container %s is still running", containerID)
	}

	// 1. Snapshot 삭제
	fmt.Printf("    [1] Snapshot 삭제: %s\n", container.SnapshotID)

	// 2. Container 메타데이터 삭제
	fmt.Printf("    [2] Container 메타데이터 삭제\n")

	// 3. 이름 인덱스 해제
	for name, id := range s.nameIndex {
		if id == containerID {
			delete(s.nameIndex, name)
			break
		}
	}

	// 4. Sandbox에서 제거
	if sandbox, ok := s.sandboxes[container.SandboxID]; ok {
		delete(sandbox.Containers, containerID)
	}

	delete(s.containers, containerID)
	fmt.Printf("    [3] 컨테이너 제거 완료: %s\n", containerID)

	return nil
}

// ============================================================================
// 9. StopPodSandbox / RemovePodSandbox
// ============================================================================

func (s *CRIService) StopPodSandbox(sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, ok := s.sandboxes[sandboxID]
	if !ok {
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}

	// 1. 모든 컨테이너 중지
	for _, c := range sandbox.Containers {
		if c.State == ContainerStateRunning {
			fmt.Printf("    컨테이너 %s 중지\n", c.ID[:8])
			c.State = ContainerStateStopped
			c.FinishedAt = time.Now()
		}
	}

	// 2. 네트워크 해제 (CNI teardown)
	if sandbox.NetNSPath != "" {
		fmt.Printf("    CNI Teardown: %s\n", sandbox.NetNSPath)
	}

	// 3. Sandbox 중지
	sandbox.State = SandboxStateNotReady
	fmt.Printf("    Sandbox 상태: %s\n", sandbox.State)

	return nil
}

func (s *CRIService) RemovePodSandbox(sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, ok := s.sandboxes[sandboxID]
	if !ok {
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}

	// 1. 모든 컨테이너 제거
	for id := range sandbox.Containers {
		delete(s.containers, id)
	}

	// 2. Lease 삭제
	delete(s.leases, sandbox.LeaseID)

	// 3. 이름 인덱스 해제
	for name, id := range s.nameIndex {
		if id == sandboxID {
			delete(s.nameIndex, name)
			break
		}
	}

	// 4. Sandbox 삭제
	delete(s.sandboxes, sandboxID)
	fmt.Printf("    Sandbox 제거 완료: %s\n", sandboxID)

	return nil
}

// ============================================================================
// 10. 상태 출력 헬퍼
// ============================================================================

func (s *CRIService) printStatus() {
	fmt.Println()
	fmt.Println("  현재 상태:")
	fmt.Printf("    Sandbox 수: %d\n", len(s.sandboxes))
	for _, sb := range s.sandboxes {
		fmt.Printf("      Sandbox %-16s  State=%-18s  IP=%-15s  Containers=%d\n",
			sb.ID[:12]+"...", sb.State, sb.IP, len(sb.Containers))
		for _, c := range sb.Containers {
			fmt.Printf("        Container %-16s  State=%-20s  Image=%s\n",
				c.ID[:12]+"...", c.State, c.ImageRef)
		}
	}
}

// ============================================================================
// 11. main: 전체 CRI 시나리오 실행
// ============================================================================

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Println("========================================")
	fmt.Println("containerd CRI 시뮬레이션")
	fmt.Println("(RunPodSandbox / CreateContainer)")
	fmt.Println("========================================")
	fmt.Println()

	svc := NewCRIService()

	// ----- 시나리오 1: RunPodSandbox -----
	fmt.Println("--- 시나리오 1: RunPodSandbox ---")
	fmt.Println("    kubelet → CRI RunPodSandbox 호출")
	fmt.Println()

	sandboxConfig := &PodSandboxConfig{
		Metadata: &PodSandboxMetadata{
			Name:      "nginx-pod",
			UID:       "uid-abc-123",
			Namespace: "default",
			Attempt:   0,
		},
		Hostname:     "nginx-pod",
		LogDirectory: "/var/log/pods/default_nginx-pod_uid-abc-123",
		DNSConfig: &DNSConfig{
			Servers:  []string{"10.96.0.10"},
			Searches: []string{"default.svc.cluster.local"},
		},
		Labels: map[string]string{
			"app": "nginx",
		},
		Linux: &LinuxPodSandboxConfig{
			SecurityContext: &LinuxSandboxSecurityContext{
				NamespaceOptions: &NamespaceOption{
					Network: "POD",
				},
			},
		},
	}

	sandboxID, err := svc.RunPodSandbox(sandboxConfig, "io.containerd.runc.v2")
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	fmt.Printf("\n  RunPodSandbox 성공: sandboxID=%s\n", sandboxID)

	// ----- 시나리오 2: CreateContainer -----
	fmt.Println()
	fmt.Println("--- 시나리오 2: CreateContainer ---")
	fmt.Println("    kubelet → CRI CreateContainer 호출 (nginx 컨테이너)")
	fmt.Println()

	containerID, err := svc.CreateContainer(sandboxID, &ContainerConfig{
		Metadata: &ContainerMetadata{Name: "nginx", Attempt: 0},
		Image:    &ImageSpec{Image: "docker.io/library/nginx:1.25"},
		Command:  []string{"nginx"},
		Args:     []string{"-g", "daemon off;"},
		Envs: []KeyValue{
			{Key: "NGINX_PORT", Value: "80"},
		},
		Labels:  map[string]string{"app": "nginx"},
		LogPath: "nginx/0.log",
	}, sandboxConfig)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	fmt.Printf("\n  CreateContainer 성공: containerID=%s\n", containerID)

	// sidecar 컨테이너 추가
	fmt.Println()
	fmt.Println("    sidecar 컨테이너 (istio-proxy) 추가:")
	fmt.Println()
	sidecarID, err := svc.CreateContainer(sandboxID, &ContainerConfig{
		Metadata: &ContainerMetadata{Name: "istio-proxy", Attempt: 0},
		Image:    &ImageSpec{Image: "docker.io/istio/proxyv2:1.20"},
		Command:  []string{"/usr/local/bin/pilot-agent", "proxy", "sidecar"},
		Labels:   map[string]string{"app": "istio-proxy"},
		LogPath:  "istio-proxy/0.log",
	}, sandboxConfig)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	fmt.Printf("\n  CreateContainer (sidecar) 성공: containerID=%s\n", sidecarID)

	// ----- 시나리오 3: StartContainer -----
	fmt.Println()
	fmt.Println("--- 시나리오 3: StartContainer ---")
	fmt.Println()

	fmt.Println("  nginx 컨테이너 시작:")
	if err := svc.StartContainer(containerID); err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("  istio-proxy 컨테이너 시작:")
	if err := svc.StartContainer(sidecarID); err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}

	svc.printStatus()

	// ----- 시나리오 4: StopContainer -----
	fmt.Println()
	fmt.Println("--- 시나리오 4: StopContainer ---")
	fmt.Println()

	fmt.Println("  nginx 컨테이너 중지 (gracePeriod=30s):")
	if err := svc.StopContainer(containerID, 30); err != nil {
		fmt.Printf("  에러: %v\n", err)
	}

	// ----- 시나리오 5: RemoveContainer -----
	fmt.Println()
	fmt.Println("--- 시나리오 5: RemoveContainer ---")
	fmt.Println()

	fmt.Println("  nginx 컨테이너 제거:")
	if err := svc.RemoveContainer(containerID); err != nil {
		fmt.Printf("  에러: %v\n", err)
	}

	svc.printStatus()

	// ----- 시나리오 6: Pod 전체 중지 및 제거 -----
	fmt.Println()
	fmt.Println("--- 시나리오 6: StopPodSandbox / RemovePodSandbox ---")
	fmt.Println()

	fmt.Println("  Pod Sandbox 중지:")
	if err := svc.StopPodSandbox(sandboxID); err != nil {
		fmt.Printf("  에러: %v\n", err)
	}

	fmt.Println()
	fmt.Println("  Pod Sandbox 제거:")
	if err := svc.RemovePodSandbox(sandboxID); err != nil {
		fmt.Printf("  에러: %v\n", err)
	}

	svc.printStatus()

	// ----- CRI 호출 흐름 다이어그램 -----
	fmt.Println()
	fmt.Println("--- CRI 호출 흐름 ---")
	fmt.Println()
	fmt.Println("  kubelet                    containerd (CRI)                  shim/runc")
	fmt.Println("  " + strings.Repeat("-", 72))
	steps := []struct {
		kubelet, cri, runtime string
	}{
		{"RunPodSandbox()", "ID생성 → Lease → NetNS → CNI → Sandbox Create/Start", "pause container"},
		{"CreateContainer()", "Sandbox조회 → Spec생성 → Snapshot.Prepare → Store", "(메타데이터만)"},
		{"StartContainer()", "Container조회 → Task.Create → Task.Start", "runc create+start"},
		{"StopContainer()", "SIGTERM → (timeout) → SIGKILL → Wait", "runc kill"},
		{"RemoveContainer()", "Task.Delete → Snapshot.Remove → Store삭제", "runc delete"},
		{"StopPodSandbox()", "컨테이너 중지 → CNI Teardown → Sandbox Stop", "pause 종료"},
		{"RemovePodSandbox()", "Lease삭제 → NetNS삭제 → Store삭제", "정리 완료"},
	}
	for _, s := range steps {
		fmt.Printf("  %-24s → %-48s → %s\n", s.kubelet, s.cri, s.runtime)
	}

	// ----- Pod와 Container 관계 -----
	fmt.Println()
	fmt.Println("--- Pod와 Container 관계 ---")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │ Pod Sandbox (NetNS, Cgroup)                            │")
	fmt.Println("  │                                                        │")
	fmt.Println("  │  ┌──────────────┐  ┌──────────────┐  ┌────────────┐  │")
	fmt.Println("  │  │   pause      │  │   nginx      │  │ istio-proxy│  │")
	fmt.Println("  │  │  (infra)     │  │  (app)       │  │ (sidecar)  │  │")
	fmt.Println("  │  └──────────────┘  └──────────────┘  └────────────┘  │")
	fmt.Println("  │                                                        │")
	fmt.Println("  │  Network: 10.244.x.x    DNS: 10.96.0.10              │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
