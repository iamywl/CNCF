package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// =============================================================================
// containerd OCI Spec 생성 + CDI 디바이스 주입 시뮬레이션
// =============================================================================
//
// containerd는 컨테이너 실행 시 OCI 런타임 스펙을 생성하고,
// CDI(Container Device Interface)를 통해 GPU 등 디바이스를 주입한다.
//
// 핵심 동작:
//   - OCI Spec Generation: 컨테이너 설정을 OCI 런타임 스펙으로 변환
//   - CDI: 벤더 독립적 디바이스 주입 표준
//   - Spec Opts: 함수형 옵션 패턴으로 스펙 수정
//
// 실제 코드 참조:
//   - oci/spec.go: OCI 스펙 생성
//   - pkg/cdi/: CDI 통합
//   - oci/spec_opts.go: 스펙 옵션 함수들
// =============================================================================

// --- OCI 런타임 스펙 구조체 ---

type OCISpec struct {
	OCIVersion string      `json:"ociVersion"`
	Process    OCIProcess  `json:"process"`
	Root       OCIRoot     `json:"root"`
	Hostname   string      `json:"hostname"`
	Mounts     []OCIMount  `json:"mounts"`
	Linux      *LinuxSpec  `json:"linux,omitempty"`
	Hooks      *OCIHooks   `json:"hooks,omitempty"`
	Env        []string    `json:"-"`
}

type OCIProcess struct {
	Terminal bool     `json:"terminal"`
	User     OCIUser  `json:"user"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
}

type OCIUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type OCIRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type OCIMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

type LinuxSpec struct {
	Resources    *LinuxResources  `json:"resources,omitempty"`
	Namespaces   []LinuxNamespace `json:"namespaces"`
	Devices      []LinuxDevice    `json:"devices,omitempty"`
	CgroupsPath  string           `json:"cgroupsPath"`
}

type LinuxResources struct {
	Memory  *MemoryResources  `json:"memory,omitempty"`
	CPU     *CPUResources     `json:"cpu,omitempty"`
	Devices []DeviceCgroup    `json:"devices,omitempty"`
}

type MemoryResources struct {
	Limit int64 `json:"limit"`
}

type CPUResources struct {
	Shares uint64 `json:"shares"`
	Quota  int64  `json:"quota"`
	Period uint64 `json:"period"`
}

type DeviceCgroup struct {
	Allow  bool   `json:"allow"`
	Type   string `json:"type,omitempty"`
	Major  *int64 `json:"major,omitempty"`
	Minor  *int64 `json:"minor,omitempty"`
	Access string `json:"access"`
}

type LinuxNamespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

type LinuxDevice struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Major int64  `json:"major"`
	Minor int64  `json:"minor"`
	UID   uint32 `json:"uid"`
	GID   uint32 `json:"gid"`
}

type OCIHooks struct {
	Prestart  []OCIHook `json:"prestart,omitempty"`
	Poststart []OCIHook `json:"poststart,omitempty"`
	Poststop  []OCIHook `json:"poststop,omitempty"`
}

type OCIHook struct {
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
}

// --- Spec Option 패턴 ---

// SpecOpt는 OCI 스펙을 수정하는 함수형 옵션이다.
// containerd의 oci.SpecOpts 패턴을 재현한다.
type SpecOpt func(*OCISpec) error

func WithHostname(hostname string) SpecOpt {
	return func(s *OCISpec) error {
		s.Hostname = hostname
		return nil
	}
}

func WithArgs(args ...string) SpecOpt {
	return func(s *OCISpec) error {
		s.Process.Args = args
		return nil
	}
}

func WithEnv(env []string) SpecOpt {
	return func(s *OCISpec) error {
		s.Process.Env = append(s.Process.Env, env...)
		return nil
	}
}

func WithMemoryLimit(limit int64) SpecOpt {
	return func(s *OCISpec) error {
		if s.Linux == nil {
			s.Linux = &LinuxSpec{}
		}
		if s.Linux.Resources == nil {
			s.Linux.Resources = &LinuxResources{}
		}
		s.Linux.Resources.Memory = &MemoryResources{Limit: limit}
		return nil
	}
}

func WithCPUShares(shares uint64) SpecOpt {
	return func(s *OCISpec) error {
		if s.Linux == nil {
			s.Linux = &LinuxSpec{}
		}
		if s.Linux.Resources == nil {
			s.Linux.Resources = &LinuxResources{}
		}
		s.Linux.Resources.CPU = &CPUResources{
			Shares: shares,
			Quota:  100000,
			Period: 100000,
		}
		return nil
	}
}

func WithNamespaces(ns ...string) SpecOpt {
	return func(s *OCISpec) error {
		if s.Linux == nil {
			s.Linux = &LinuxSpec{}
		}
		for _, n := range ns {
			s.Linux.Namespaces = append(s.Linux.Namespaces, LinuxNamespace{Type: n})
		}
		return nil
	}
}

func WithRootfs(path string, readonly bool) SpecOpt {
	return func(s *OCISpec) error {
		s.Root = OCIRoot{Path: path, Readonly: readonly}
		return nil
	}
}

func WithMount(dst, src, typ string, opts ...string) SpecOpt {
	return func(s *OCISpec) error {
		s.Mounts = append(s.Mounts, OCIMount{
			Destination: dst, Source: src, Type: typ, Options: opts,
		})
		return nil
	}
}

// --- CDI (Container Device Interface) ---

// CDIDevice는 CDI 스펙의 디바이스 정의이다.
type CDIDevice struct {
	Name           string
	ContainerEdits CDIContainerEdits
}

type CDIContainerEdits struct {
	Env     []string
	Devices []LinuxDevice
	Mounts  []OCIMount
	Hooks   []OCIHook
}

// CDISpec은 CDI 벤더 스펙이다 (예: nvidia.com/gpu).
type CDISpec struct {
	CDIVersion string      `json:"cdiVersion"`
	Kind       string      `json:"kind"`
	Devices    []CDIDevice `json:"devices"`
	// 전역 수정 사항
	ContainerEdits CDIContainerEdits `json:"containerEdits"`
}

// CDIRegistry는 CDI 스펙을 관리하는 레지스트리이다.
type CDIRegistry struct {
	specs map[string]*CDISpec
}

func NewCDIRegistry() *CDIRegistry {
	return &CDIRegistry{specs: make(map[string]*CDISpec)}
}

func (r *CDIRegistry) Register(spec *CDISpec) {
	r.specs[spec.Kind] = spec
}

// InjectDevice는 CDI 디바이스를 OCI 스펙에 주입한다.
// qualified name: "vendor.com/kind=device_name"
func (r *CDIRegistry) InjectDevice(spec *OCISpec, qualifiedName string) error {
	parts := strings.SplitN(qualifiedName, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid CDI device name: %s", qualifiedName)
	}
	kind, deviceName := parts[0], parts[1]

	cdiSpec, ok := r.specs[kind]
	if !ok {
		return fmt.Errorf("CDI spec not found for kind: %s", kind)
	}

	// 전역 수정 적용
	applyEdits(spec, cdiSpec.ContainerEdits)

	// 디바이스별 수정 적용
	for _, dev := range cdiSpec.Devices {
		if dev.Name == deviceName {
			applyEdits(spec, dev.ContainerEdits)
			fmt.Printf("  [CDI] Injected device: %s (kind: %s)\n", deviceName, kind)
			return nil
		}
	}
	return fmt.Errorf("device %s not found in spec %s", deviceName, kind)
}

func applyEdits(spec *OCISpec, edits CDIContainerEdits) {
	spec.Process.Env = append(spec.Process.Env, edits.Env...)
	for _, dev := range edits.Devices {
		if spec.Linux == nil {
			spec.Linux = &LinuxSpec{}
		}
		spec.Linux.Devices = append(spec.Linux.Devices, dev)
	}
	spec.Mounts = append(spec.Mounts, edits.Mounts...)
	if len(edits.Hooks) > 0 {
		if spec.Hooks == nil {
			spec.Hooks = &OCIHooks{}
		}
		spec.Hooks.Prestart = append(spec.Hooks.Prestart, edits.Hooks...)
	}
}

// --- 기본 OCI 스펙 생성 ---

func GenerateDefaultSpec(opts ...SpecOpt) (*OCISpec, error) {
	spec := &OCISpec{
		OCIVersion: "1.1.0",
		Process: OCIProcess{
			User: OCIUser{UID: 0, GID: 0},
			Cwd:  "/",
			Env: []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"TERM=xterm",
			},
		},
		Root: OCIRoot{Path: "rootfs", Readonly: false},
		Mounts: []OCIMount{
			{"/proc", "proc", "proc", []string{"nosuid", "noexec", "nodev"}},
			{"/dev", "tmpfs", "tmpfs", []string{"nosuid", "strictatime", "mode=755"}},
			{"/sys", "sysfs", "sysfs", []string{"nosuid", "noexec", "nodev", "ro"}},
		},
	}

	for _, opt := range opts {
		if err := opt(spec); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

func prettyJSON(v interface{}) string {
	data, _ := json.MarshalIndent(v, "  ", "  ")
	return string(data)
}

func main() {
	fmt.Println("=== containerd OCI Spec + CDI 시뮬레이션 ===")
	fmt.Println()

	// --- 기본 OCI 스펙 생성 ---
	fmt.Println("[1] 기본 OCI 런타임 스펙 생성")
	fmt.Println(strings.Repeat("-", 60))

	spec, _ := GenerateDefaultSpec(
		WithHostname("my-container"),
		WithArgs("/bin/sh", "-c", "echo hello && sleep 3600"),
		WithEnv([]string{"APP_ENV=production", "LOG_LEVEL=info"}),
		WithMemoryLimit(256*1024*1024), // 256MB
		WithCPUShares(512),
		WithNamespaces("pid", "network", "mount", "ipc", "uts"),
		WithRootfs("/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/1/fs", true),
		WithMount("/data", "/mnt/host-data", "bind", "rbind", "rw"),
	)

	fmt.Printf("  %s\n", prettyJSON(spec))
	fmt.Println()

	// --- CDI 스펙 등록 ---
	fmt.Println("[2] CDI 스펙 등록")
	fmt.Println(strings.Repeat("-", 60))

	cdiReg := NewCDIRegistry()

	// NVIDIA GPU CDI 스펙
	nvidiaCDI := &CDISpec{
		CDIVersion: "0.6.0",
		Kind:       "nvidia.com/gpu",
		ContainerEdits: CDIContainerEdits{
			Env: []string{
				"NVIDIA_VISIBLE_DEVICES=all",
				"NVIDIA_DRIVER_CAPABILITIES=compute,utility",
			},
			Hooks: []OCIHook{
				{Path: "/usr/bin/nvidia-container-runtime-hook", Args: []string{"prestart"}},
			},
		},
		Devices: []CDIDevice{
			{
				Name: "0",
				ContainerEdits: CDIContainerEdits{
					Devices: []LinuxDevice{
						{Path: "/dev/nvidia0", Type: "c", Major: 195, Minor: 0},
						{Path: "/dev/nvidiactl", Type: "c", Major: 195, Minor: 255},
						{Path: "/dev/nvidia-uvm", Type: "c", Major: 510, Minor: 0},
					},
					Mounts: []OCIMount{
						{"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so", "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so", "bind", []string{"ro", "bind"}},
					},
				},
			},
			{
				Name: "1",
				ContainerEdits: CDIContainerEdits{
					Devices: []LinuxDevice{
						{Path: "/dev/nvidia1", Type: "c", Major: 195, Minor: 1},
					},
				},
			},
		},
	}
	cdiReg.Register(nvidiaCDI)
	fmt.Printf("  Registered CDI spec: %s (devices: %d)\n", nvidiaCDI.Kind, len(nvidiaCDI.Devices))

	// FPGA CDI 스펙
	fpgaCDI := &CDISpec{
		CDIVersion: "0.6.0",
		Kind:       "intel.com/fpga",
		Devices: []CDIDevice{
			{
				Name: "region0",
				ContainerEdits: CDIContainerEdits{
					Devices: []LinuxDevice{
						{Path: "/dev/intel-fpga-port.0", Type: "c", Major: 241, Minor: 0},
					},
					Env: []string{"FPGA_REGION=0"},
				},
			},
		},
	}
	cdiReg.Register(fpgaCDI)
	fmt.Printf("  Registered CDI spec: %s (devices: %d)\n", fpgaCDI.Kind, len(fpgaCDI.Devices))
	fmt.Println()

	// --- CDI 디바이스 주입 ---
	fmt.Println("[3] CDI 디바이스 주입 (GPU 컨테이너)")
	fmt.Println(strings.Repeat("-", 60))

	gpuSpec, _ := GenerateDefaultSpec(
		WithHostname("gpu-worker"),
		WithArgs("python3", "train.py"),
		WithEnv([]string{"CUDA_VERSION=12.0"}),
		WithMemoryLimit(4*1024*1024*1024), // 4GB
		WithNamespaces("pid", "network", "mount"),
	)

	if err := cdiReg.InjectDevice(gpuSpec, "nvidia.com/gpu=0"); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}

	fmt.Printf("\n  GPU 컨테이너 스펙:\n")
	fmt.Printf("  %s\n", prettyJSON(gpuSpec))
	fmt.Println()

	// --- FPGA 주입 ---
	fmt.Println("[4] CDI 디바이스 주입 (FPGA 컨테이너)")
	fmt.Println(strings.Repeat("-", 60))

	fpgaSpec, _ := GenerateDefaultSpec(
		WithHostname("fpga-worker"),
		WithArgs("./accelerator"),
		WithNamespaces("pid", "network", "mount"),
	)

	if err := cdiReg.InjectDevice(fpgaSpec, "intel.com/fpga=region0"); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}

	fmt.Printf("\n  FPGA 컨테이너의 디바이스:\n")
	if fpgaSpec.Linux != nil {
		for _, dev := range fpgaSpec.Linux.Devices {
			fmt.Printf("    %s (major=%d, minor=%d)\n", dev.Path, dev.Major, dev.Minor)
		}
	}
	fmt.Printf("  FPGA 컨테이너의 환경변수:\n")
	for _, env := range fpgaSpec.Process.Env {
		fmt.Printf("    %s\n", env)
	}
	fmt.Println()

	// --- 에러 케이스 ---
	fmt.Println("[5] CDI 에러 케이스")
	fmt.Println(strings.Repeat("-", 60))
	errSpec, _ := GenerateDefaultSpec()
	err := cdiReg.InjectDevice(errSpec, "unknown.com/device=0")
	fmt.Printf("  Unknown kind: %v\n", err)
	err = cdiReg.InjectDevice(errSpec, "nvidia.com/gpu=99")
	fmt.Printf("  Unknown device: %v\n", err)
	err = cdiReg.InjectDevice(errSpec, "bad-format")
	fmt.Printf("  Bad format: %v\n", err)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
