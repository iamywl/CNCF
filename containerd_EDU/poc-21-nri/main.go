package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// containerd NRI (Node Resource Interface) 플러그인 시뮬레이션
// =============================================================================
//
// NRI는 containerd/CRI-O에서 플러그인이 컨테이너 라이프사이클에 개입할 수 있는
// 표준 인터페이스이다. 플러그인은 컨테이너 생성/업데이트/삭제 시점에 훅을 실행한다.
//
// 핵심 개념:
//   - NRI Plugin: 외부 프로세스로 실행되는 플러그인
//   - Hook Points: RunPodSandbox, CreateContainer, UpdateContainer, StopContainer 등
//   - Container Adjustment: 플러그인이 컨테이너 스펙을 수정
//   - Chaining: 여러 플러그인이 순서대로 실행
//
// 실제 코드 참조:
//   - pkg/nri/: NRI 통합 코드
//   - vendor/github.com/containerd/nri/: NRI 라이브러리
// =============================================================================

// --- 컨테이너/팟 모델 ---

type PodSandbox struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

type Container struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	PodID       string            `json:"pod_id"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Resources   ContainerResources `json:"resources"`
	Env         []string          `json:"env"`
	Devices     []string          `json:"devices"`
}

type ContainerResources struct {
	CPUShares  uint64 `json:"cpu_shares"`
	CPUQuota   int64  `json:"cpu_quota"`
	MemoryLimit int64 `json:"memory_limit"`
	CPUSetCPUs string `json:"cpuset_cpus"`
}

// --- Container Adjustment ---

// ContainerAdjustment는 NRI 플러그인이 컨테이너 스펙을 수정하는 데 사용하는 구조체이다.
type ContainerAdjustment struct {
	AddEnv       []string           `json:"add_env,omitempty"`
	RemoveEnv    []string           `json:"remove_env,omitempty"`
	AddDevices   []string           `json:"add_devices,omitempty"`
	Resources    *ContainerResources `json:"resources,omitempty"`
	AddLabels    map[string]string  `json:"add_labels,omitempty"`
	AddAnnotations map[string]string `json:"add_annotations,omitempty"`
}

func (adj *ContainerAdjustment) Apply(c *Container) {
	// 환경변수 추가
	c.Env = append(c.Env, adj.AddEnv...)

	// 환경변수 제거
	for _, remove := range adj.RemoveEnv {
		for i, env := range c.Env {
			if strings.HasPrefix(env, remove+"=") {
				c.Env = append(c.Env[:i], c.Env[i+1:]...)
				break
			}
		}
	}

	// 디바이스 추가
	c.Devices = append(c.Devices, adj.AddDevices...)

	// 리소스 수정
	if adj.Resources != nil {
		if adj.Resources.CPUShares > 0 {
			c.Resources.CPUShares = adj.Resources.CPUShares
		}
		if adj.Resources.CPUQuota > 0 {
			c.Resources.CPUQuota = adj.Resources.CPUQuota
		}
		if adj.Resources.MemoryLimit > 0 {
			c.Resources.MemoryLimit = adj.Resources.MemoryLimit
		}
		if adj.Resources.CPUSetCPUs != "" {
			c.Resources.CPUSetCPUs = adj.Resources.CPUSetCPUs
		}
	}

	// 레이블/어노테이션 추가
	if adj.AddLabels != nil {
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}
		for k, v := range adj.AddLabels {
			c.Labels[k] = v
		}
	}
	if adj.AddAnnotations != nil {
		if c.Annotations == nil {
			c.Annotations = make(map[string]string)
		}
		for k, v := range adj.AddAnnotations {
			c.Annotations[k] = v
		}
	}
}

// --- NRI Plugin 인터페이스 ---

type NRIPlugin interface {
	Name() string
	Index() int // 실행 순서 (낮을수록 먼저)
	RunPodSandbox(pod *PodSandbox) error
	CreateContainer(pod *PodSandbox, ctr *Container) (*ContainerAdjustment, error)
	UpdateContainer(pod *PodSandbox, ctr *Container) (*ContainerAdjustment, error)
	StopContainer(pod *PodSandbox, ctr *Container) error
}

// --- 플러그인 구현: CPU Pinning ---

type CPUPinningPlugin struct {
	cpuPool    map[string]string // podID -> assigned CPUs
	nextCPUSet int
}

func NewCPUPinningPlugin() *CPUPinningPlugin {
	return &CPUPinningPlugin{
		cpuPool: make(map[string]string),
	}
}

func (p *CPUPinningPlugin) Name() string  { return "cpu-pinning" }
func (p *CPUPinningPlugin) Index() int    { return 10 }

func (p *CPUPinningPlugin) RunPodSandbox(pod *PodSandbox) error {
	fmt.Printf("    [%s] Pod %s 시작 확인\n", p.Name(), pod.Name)
	return nil
}

func (p *CPUPinningPlugin) CreateContainer(pod *PodSandbox, ctr *Container) (*ContainerAdjustment, error) {
	// QoS가 guaranteed인 경우만 CPU pinning
	if ctr.Labels["qos"] != "guaranteed" {
		fmt.Printf("    [%s] Container %s: QoS가 guaranteed가 아님, skip\n", p.Name(), ctr.Name)
		return nil, nil
	}

	cpuSet := fmt.Sprintf("%d-%d", p.nextCPUSet, p.nextCPUSet+1)
	p.nextCPUSet += 2
	p.cpuPool[ctr.ID] = cpuSet

	fmt.Printf("    [%s] Container %s: CPU pinning -> %s\n", p.Name(), ctr.Name, cpuSet)
	return &ContainerAdjustment{
		Resources: &ContainerResources{CPUSetCPUs: cpuSet},
		AddAnnotations: map[string]string{
			"nri.cpu-pinning/cpuset": cpuSet,
		},
	}, nil
}

func (p *CPUPinningPlugin) UpdateContainer(pod *PodSandbox, ctr *Container) (*ContainerAdjustment, error) {
	return nil, nil
}

func (p *CPUPinningPlugin) StopContainer(pod *PodSandbox, ctr *Container) error {
	if cpuSet, ok := p.cpuPool[ctr.ID]; ok {
		fmt.Printf("    [%s] Container %s: CPU %s 반환\n", p.Name(), ctr.Name, cpuSet)
		delete(p.cpuPool, ctr.ID)
	}
	return nil
}

// --- 플러그인 구현: Device Injector ---

type DeviceInjectorPlugin struct{}

func (p *DeviceInjectorPlugin) Name() string { return "device-injector" }
func (p *DeviceInjectorPlugin) Index() int   { return 20 }

func (p *DeviceInjectorPlugin) RunPodSandbox(pod *PodSandbox) error { return nil }

func (p *DeviceInjectorPlugin) CreateContainer(pod *PodSandbox, ctr *Container) (*ContainerAdjustment, error) {
	adj := &ContainerAdjustment{}

	// GPU 요청이 있으면 디바이스 주입
	if gpuCount, ok := ctr.Labels["nvidia.com/gpu"]; ok {
		fmt.Printf("    [%s] Container %s: GPU %s개 주입\n", p.Name(), ctr.Name, gpuCount)
		adj.AddDevices = append(adj.AddDevices, "/dev/nvidia0", "/dev/nvidiactl", "/dev/nvidia-uvm")
		adj.AddEnv = append(adj.AddEnv, "NVIDIA_VISIBLE_DEVICES=0", "NVIDIA_DRIVER_CAPABILITIES=compute,utility")
	}

	// RDMA 요청
	if _, ok := ctr.Labels["rdma/device"]; ok {
		fmt.Printf("    [%s] Container %s: RDMA 디바이스 주입\n", p.Name(), ctr.Name)
		adj.AddDevices = append(adj.AddDevices, "/dev/infiniband/uverbs0")
	}

	if len(adj.AddDevices) == 0 {
		return nil, nil
	}
	return adj, nil
}

func (p *DeviceInjectorPlugin) UpdateContainer(pod *PodSandbox, ctr *Container) (*ContainerAdjustment, error) {
	return nil, nil
}

func (p *DeviceInjectorPlugin) StopContainer(pod *PodSandbox, ctr *Container) error { return nil }

// --- 플러그인 구현: Resource Limiter ---

type ResourceLimiterPlugin struct {
	maxMemory int64
}

func (p *ResourceLimiterPlugin) Name() string { return "resource-limiter" }
func (p *ResourceLimiterPlugin) Index() int   { return 30 }

func (p *ResourceLimiterPlugin) RunPodSandbox(pod *PodSandbox) error { return nil }

func (p *ResourceLimiterPlugin) CreateContainer(pod *PodSandbox, ctr *Container) (*ContainerAdjustment, error) {
	if ctr.Resources.MemoryLimit > p.maxMemory {
		fmt.Printf("    [%s] Container %s: 메모리 제한 %dMB -> %dMB (클램핑)\n",
			p.Name(), ctr.Name, ctr.Resources.MemoryLimit/(1024*1024), p.maxMemory/(1024*1024))
		return &ContainerAdjustment{
			Resources: &ContainerResources{MemoryLimit: p.maxMemory},
		}, nil
	}
	return nil, nil
}

func (p *ResourceLimiterPlugin) UpdateContainer(pod *PodSandbox, ctr *Container) (*ContainerAdjustment, error) {
	return p.CreateContainer(nil, ctr)
}

func (p *ResourceLimiterPlugin) StopContainer(pod *PodSandbox, ctr *Container) error { return nil }

// --- NRI Runtime ---

type NRIRuntime struct {
	plugins []NRIPlugin
}

func NewNRIRuntime() *NRIRuntime {
	return &NRIRuntime{}
}

func (r *NRIRuntime) RegisterPlugin(p NRIPlugin) {
	// 인덱스 순서로 삽입
	inserted := false
	for i, existing := range r.plugins {
		if p.Index() < existing.Index() {
			r.plugins = append(r.plugins[:i+1], r.plugins[i:]...)
			r.plugins[i] = p
			inserted = true
			break
		}
	}
	if !inserted {
		r.plugins = append(r.plugins, p)
	}
	fmt.Printf("  [NRI] Plugin registered: %s (index: %d)\n", p.Name(), p.Index())
}

func (r *NRIRuntime) RunPodSandbox(pod *PodSandbox) {
	fmt.Printf("\n  >> RunPodSandbox: %s/%s\n", pod.Namespace, pod.Name)
	for _, p := range r.plugins {
		if err := p.RunPodSandbox(pod); err != nil {
			fmt.Printf("    [ERROR] %s: %v\n", p.Name(), err)
		}
	}
}

func (r *NRIRuntime) CreateContainer(pod *PodSandbox, ctr *Container) {
	fmt.Printf("\n  >> CreateContainer: %s (pod: %s)\n", ctr.Name, pod.Name)
	for _, p := range r.plugins {
		adj, err := p.CreateContainer(pod, ctr)
		if err != nil {
			fmt.Printf("    [ERROR] %s: %v\n", p.Name(), err)
			continue
		}
		if adj != nil {
			adj.Apply(ctr)
		}
	}
}

func (r *NRIRuntime) UpdateContainer(pod *PodSandbox, ctr *Container) {
	fmt.Printf("\n  >> UpdateContainer: %s\n", ctr.Name)
	for _, p := range r.plugins {
		adj, err := p.UpdateContainer(pod, ctr)
		if err != nil {
			fmt.Printf("    [ERROR] %s: %v\n", p.Name(), err)
			continue
		}
		if adj != nil {
			adj.Apply(ctr)
		}
	}
}

func (r *NRIRuntime) StopContainer(pod *PodSandbox, ctr *Container) {
	fmt.Printf("\n  >> StopContainer: %s\n", ctr.Name)
	// 역순으로 정리
	for i := len(r.plugins) - 1; i >= 0; i-- {
		if err := r.plugins[i].StopContainer(pod, ctr); err != nil {
			fmt.Printf("    [ERROR] %s: %v\n", r.plugins[i].Name(), err)
		}
	}
}

func prettyJSON(v interface{}) string {
	data, _ := json.MarshalIndent(v, "    ", "  ")
	return string(data)
}

func main() {
	fmt.Println("=== containerd NRI 플러그인 시뮬레이션 ===")
	fmt.Println()

	// --- NRI 런타임 초기화 ---
	fmt.Println("[1] NRI 플러그인 등록")
	fmt.Println(strings.Repeat("-", 60))

	runtime := NewNRIRuntime()
	runtime.RegisterPlugin(NewCPUPinningPlugin())
	runtime.RegisterPlugin(&DeviceInjectorPlugin{})
	runtime.RegisterPlugin(&ResourceLimiterPlugin{maxMemory: 2 * 1024 * 1024 * 1024}) // 2GB max
	fmt.Println()

	// --- 시나리오 1: GPU 워크로드 ---
	fmt.Println("[2] 시나리오: GPU 워크로드 배포")
	fmt.Println(strings.Repeat("-", 60))

	pod1 := &PodSandbox{
		ID: "pod-001", Name: "ml-training", Namespace: "ai",
		Labels: map[string]string{"app": "training"},
	}
	runtime.RunPodSandbox(pod1)

	gpuContainer := &Container{
		ID: "ctr-001", Name: "trainer", PodID: "pod-001",
		Labels: map[string]string{
			"qos": "guaranteed", "nvidia.com/gpu": "1",
		},
		Env:       []string{"CUDA_VERSION=12.0"},
		Resources: ContainerResources{CPUShares: 1024, MemoryLimit: 4 * 1024 * 1024 * 1024},
	}
	runtime.CreateContainer(pod1, gpuContainer)
	fmt.Printf("\n    결과 컨테이너:\n    %s\n", prettyJSON(gpuContainer))
	fmt.Println()

	// --- 시나리오 2: 일반 웹 서버 ---
	fmt.Println("[3] 시나리오: 일반 웹 서버 배포")
	fmt.Println(strings.Repeat("-", 60))

	pod2 := &PodSandbox{
		ID: "pod-002", Name: "web-server", Namespace: "default",
		Labels: map[string]string{"app": "nginx"},
	}
	runtime.RunPodSandbox(pod2)

	webContainer := &Container{
		ID: "ctr-002", Name: "nginx", PodID: "pod-002",
		Labels: map[string]string{"qos": "burstable"},
		Env:     []string{"NGINX_PORT=8080"},
		Resources: ContainerResources{CPUShares: 512, MemoryLimit: 512 * 1024 * 1024},
	}
	runtime.CreateContainer(pod2, webContainer)
	fmt.Printf("\n    결과 컨테이너:\n    %s\n", prettyJSON(webContainer))
	fmt.Println()

	// --- 시나리오 3: 리소스 초과 컨테이너 ---
	fmt.Println("[4] 시나리오: 리소스 초과 컨테이너 (클램핑)")
	fmt.Println(strings.Repeat("-", 60))

	bigContainer := &Container{
		ID: "ctr-003", Name: "memory-hog", PodID: "pod-002",
		Labels: map[string]string{"qos": "guaranteed"},
		Resources: ContainerResources{CPUShares: 2048, MemoryLimit: 8 * 1024 * 1024 * 1024}, // 8GB
	}
	runtime.CreateContainer(pod2, bigContainer)
	fmt.Printf("\n    결과: 메모리 = %dMB (클램핑됨)\n",
		bigContainer.Resources.MemoryLimit/(1024*1024))
	fmt.Println()

	// --- 컨테이너 업데이트 ---
	fmt.Println("[5] 컨테이너 업데이트")
	fmt.Println(strings.Repeat("-", 60))
	runtime.UpdateContainer(pod2, bigContainer)
	fmt.Println()

	// --- 컨테이너 정지 ---
	fmt.Println("[6] 컨테이너 정지 (역순 정리)")
	fmt.Println(strings.Repeat("-", 60))
	runtime.StopContainer(pod1, gpuContainer)
	runtime.StopContainer(pod2, webContainer)
	runtime.StopContainer(pod2, bigContainer)
	fmt.Println()

	_ = time.Now() // 타임스탬프용
	fmt.Println("=== 시뮬레이션 완료 ===")
}
