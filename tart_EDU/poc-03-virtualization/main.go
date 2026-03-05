package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// tart 가상 머신 구성 빌더 패턴 시뮬레이션
//
// tart 실제 소스코드 참조:
//   - Sources/tart/VM.swift             : VM.craftConfiguration() — 전체 구성 조립
//   - Sources/tart/Platform/Platform.swift : Platform 프로토콜
//   - Sources/tart/Platform/Darwin.swift : Darwin 플랫폼 (macOS)
//   - Sources/tart/Platform/Linux.swift  : Linux 플랫폼 (EFI)
//   - Sources/tart/VMConfig.swift        : VMConfig 데이터
// =============================================================================

// ---------------------------------------------------------------------------
// OS, Architecture 타입
// ---------------------------------------------------------------------------
type OS string
type Architecture string

const (
	OSDarwin OS = "darwin"
	OSLinux  OS = "linux"
)

const (
	ArchArm64 Architecture = "arm64"
)

// ---------------------------------------------------------------------------
// BootLoaderType: 부트로더 타입
//   실제 코드: Platform.bootLoader(nvramURL:) 메서드
//   Darwin → VZMacOSBootLoader, Linux → VZEFIBootLoader
// ---------------------------------------------------------------------------
type BootLoaderType string

const (
	BootMacOS BootLoaderType = "MacOSBootLoader"
	BootEFI   BootLoaderType = "EFIBootLoader"
)

// ---------------------------------------------------------------------------
// BootLoader: 부트로더 구성
//   실제 코드:
//   - Darwin.bootLoader() → VZMacOSBootLoader()
//   - Linux.bootLoader() → VZEFIBootLoader() + VZEFIVariableStore(url: nvramURL)
// ---------------------------------------------------------------------------
type BootLoader struct {
	Type         BootLoaderType
	NvramPath    string
	VariableStore string // EFI에서만 사용
}

// ---------------------------------------------------------------------------
// PlatformConfig: 플랫폼 구성
//   실제 코드:
//   - Darwin.platform() → VZMacPlatformConfiguration (ecid, auxiliaryStorage, hardwareModel)
//   - Linux.platform() → VZGenericPlatformConfiguration (nestedVirtualization)
// ---------------------------------------------------------------------------
type PlatformConfig struct {
	Type                    string
	ECID                    string // Darwin 전용
	HardwareModel           string // Darwin 전용
	NestedVirtualization    bool   // Linux 전용 (macOS 15+)
}

// ---------------------------------------------------------------------------
// GraphicsDevice: 그래픽 디바이스 구성
//   실제 코드:
//   - Darwin.graphicsDevice() → VZMacGraphicsDeviceConfiguration
//     - unit == .point → VZMacGraphicsDisplayConfiguration(for: hostMainScreen, sizeInPoints:)
//     - unit == .pixel → VZMacGraphicsDisplayConfiguration(widthInPixels:, heightInPixels:, ppi: 72)
//   - Linux.graphicsDevice() → VZVirtioGraphicsDeviceConfiguration + Scanout
// ---------------------------------------------------------------------------
type GraphicsDevice struct {
	Type         string
	Width        int
	Height       int
	PixelsPerInch int
}

// ---------------------------------------------------------------------------
// AudioDevice: 오디오 디바이스 구성
//   실제 코드: VM.craftConfiguration()에서 VZVirtioSoundDeviceConfiguration 구성
//   - audio && !suspendable → 입력/출력 스트림 모두 설정
//   - 그 외 → null speaker (출력 스트림만)
// ---------------------------------------------------------------------------
type AudioDevice struct {
	Type       string
	HasInput   bool
	HasOutput  bool
}

// ---------------------------------------------------------------------------
// KeyboardDevice, PointingDevice: 입력 디바이스
//   실제 코드:
//   - Darwin.keyboards() → [VZUSBKeyboardConfiguration, VZMacKeyboardConfiguration]
//   - Darwin.pointingDevices() → [VZUSBScreenCoordinatePointing, VZMacTrackpadConfiguration]
//   - Linux.keyboards() → [VZUSBKeyboardConfiguration]
//   - Linux.pointingDevices() → [VZUSBScreenCoordinatePointingDeviceConfiguration]
//   - suspendable 모드:
//     - keyboardsSuspendable() → [VZMacKeyboardConfiguration] (Mac 키보드만)
//     - pointingDevicesSuspendable() → [VZMacTrackpadConfiguration] (트랙패드만)
// ---------------------------------------------------------------------------
type KeyboardDevice struct{ Type string }
type PointingDevice struct{ Type string }

// ---------------------------------------------------------------------------
// NetworkAttachment: 네트워크 어태치먼트
//   실제 코드: network.attachments().map { VZVirtioNetworkDeviceConfiguration ... }
// ---------------------------------------------------------------------------
type NetworkAttachment struct {
	Type       string
	MACAddress string
}

// ---------------------------------------------------------------------------
// StorageDevice: 스토리지 디바이스
//   실제 코드: VZDiskImageStorageDeviceAttachment → VZVirtioBlockDeviceConfiguration
//   cachingMode: Linux → .cached, Darwin → .automatic
//   synchronizationMode: .full (기본값)
// ---------------------------------------------------------------------------
type StorageDevice struct {
	Type      string
	DiskPath  string
	ReadOnly  bool
	Caching   string // "automatic" | "cached" | "uncached"
	Sync      string // "none" | "fsync" | "full"
}

// ---------------------------------------------------------------------------
// Platform 인터페이스: 플랫폼별 구성 생성
//   실제 코드: Sources/tart/Platform/Platform.swift — protocol Platform: Codable
// ---------------------------------------------------------------------------
type Platform interface {
	GetOS() OS
	BootLoader(nvramPath string) BootLoader
	PlatformConfig(nvramPath string, nested bool) PlatformConfig
	GraphicsDevice(width, height int, unit string) GraphicsDevice
	Keyboards() []KeyboardDevice
	PointingDevices() []PointingDevice
	PointingDevicesSimplified() []PointingDevice
}

// ---------------------------------------------------------------------------
// PlatformSuspendable: 일시중지 가능한 플랫폼 확장
//   실제 코드: protocol PlatformSuspendable: Platform
//   pointingDevicesSuspendable(), keyboardsSuspendable()
// ---------------------------------------------------------------------------
type PlatformSuspendable interface {
	Platform
	KeyboardsSuspendable() []KeyboardDevice
	PointingDevicesSuspendable() []PointingDevice
}

// ---------------------------------------------------------------------------
// DarwinPlatform: macOS 플랫폼 구현
//   실제 코드: Sources/tart/Platform/Darwin.swift — struct Darwin: PlatformSuspendable
// ---------------------------------------------------------------------------
type DarwinPlatform struct {
	ECID          string
	HardwareModel string
}

func (d *DarwinPlatform) GetOS() OS { return OSDarwin }

func (d *DarwinPlatform) BootLoader(nvramPath string) BootLoader {
	// 실제: VZMacOSBootLoader() — NVRAM은 platform에서 설정
	return BootLoader{Type: BootMacOS, NvramPath: nvramPath}
}

func (d *DarwinPlatform) PlatformConfig(nvramPath string, nested bool) PlatformConfig {
	if nested {
		// 실제: "macOS virtual machines do not support nested virtualization" 에러
		fmt.Println("  [경고] macOS VM은 중첩 가상화를 지원하지 않습니다")
	}
	return PlatformConfig{
		Type:          "MacPlatformConfiguration",
		ECID:          d.ECID,
		HardwareModel: d.HardwareModel,
	}
}

func (d *DarwinPlatform) GraphicsDevice(width, height int, unit string) GraphicsDevice {
	gd := GraphicsDevice{Type: "MacGraphicsDevice", Width: width, Height: height}
	if unit == "pt" || unit == "" {
		// 실제: VZMacGraphicsDisplayConfiguration(for: hostMainScreen, sizeInPoints:)
		gd.PixelsPerInch = 144 // Retina
	} else {
		// 실제: VZMacGraphicsDisplayConfiguration(widthInPixels:, heightInPixels:, pixelsPerInch: 72)
		gd.PixelsPerInch = 72
	}
	return gd
}

func (d *DarwinPlatform) Keyboards() []KeyboardDevice {
	// 실제 (macOS 14+): [VZUSBKeyboardConfiguration, VZMacKeyboardConfiguration]
	return []KeyboardDevice{
		{Type: "USBKeyboard"},
		{Type: "MacKeyboard"},
	}
}

func (d *DarwinPlatform) PointingDevices() []PointingDevice {
	// 실제: [VZUSBScreenCoordinatePointingDevice, VZMacTrackpad]
	return []PointingDevice{
		{Type: "USBScreenCoordinatePointing"},
		{Type: "MacTrackpad"},
	}
}

func (d *DarwinPlatform) PointingDevicesSimplified() []PointingDevice {
	// 실제: noTrackpad 옵션 — 트랙패드 없이 USB 포인팅만
	return []PointingDevice{{Type: "USBScreenCoordinatePointing"}}
}

func (d *DarwinPlatform) KeyboardsSuspendable() []KeyboardDevice {
	// 실제: [VZMacKeyboardConfiguration] — USB 키보드 없이 Mac 키보드만
	return []KeyboardDevice{{Type: "MacKeyboard"}}
}

func (d *DarwinPlatform) PointingDevicesSuspendable() []PointingDevice {
	// 실제: [VZMacTrackpadConfiguration] — USB 포인팅 없이 트랙패드만
	return []PointingDevice{{Type: "MacTrackpad"}}
}

// ---------------------------------------------------------------------------
// LinuxPlatform: Linux 플랫폼 구현
//   실제 코드: Sources/tart/Platform/Linux.swift — struct Linux: Platform
// ---------------------------------------------------------------------------
type LinuxPlatform struct{}

func (l *LinuxPlatform) GetOS() OS { return OSLinux }

func (l *LinuxPlatform) BootLoader(nvramPath string) BootLoader {
	// 실제: VZEFIBootLoader() + VZEFIVariableStore(url: nvramURL)
	return BootLoader{
		Type:          BootEFI,
		NvramPath:     nvramPath,
		VariableStore: nvramPath,
	}
}

func (l *LinuxPlatform) PlatformConfig(nvramPath string, nested bool) PlatformConfig {
	return PlatformConfig{
		Type:                 "GenericPlatformConfiguration",
		NestedVirtualization: nested,
	}
}

func (l *LinuxPlatform) GraphicsDevice(width, height int, unit string) GraphicsDevice {
	// 실제: VZVirtioGraphicsDeviceConfiguration + VZVirtioGraphicsScanoutConfiguration
	return GraphicsDevice{
		Type:          "VirtioGraphicsDevice",
		Width:         width,
		Height:        height,
		PixelsPerInch: 0, // Virtio는 PPI 개념 없음
	}
}

func (l *LinuxPlatform) Keyboards() []KeyboardDevice {
	return []KeyboardDevice{{Type: "USBKeyboard"}}
}

func (l *LinuxPlatform) PointingDevices() []PointingDevice {
	return []PointingDevice{{Type: "USBScreenCoordinatePointing"}}
}

func (l *LinuxPlatform) PointingDevicesSimplified() []PointingDevice {
	return l.PointingDevices() // Linux는 트랙패드 미지원
}

// ---------------------------------------------------------------------------
// VirtualMachineConfiguration: 최종 구성 결과물
//   실제 코드: VZVirtualMachineConfiguration — 모든 디바이스가 조합된 최종 구성
// ---------------------------------------------------------------------------
type VirtualMachineConfiguration struct {
	BootLoader       BootLoader
	CPUCount         int
	MemorySize       uint64
	Platform         PlatformConfig
	Graphics         []GraphicsDevice
	Audio            []AudioDevice
	Keyboards        []KeyboardDevice
	PointingDevices  []PointingDevice
	Networks         []NetworkAttachment
	Storage          []StorageDevice
	Entropy          bool
	Clipboard        bool
	ConsoleDevices   []string
	SocketDevices    []string
	SerialPorts      []string
	DirSharing       []string
}

// Validate: 구성 유효성 검증
//   실제 코드: try configuration.validate() (VZVirtualMachineConfiguration)
func (c *VirtualMachineConfiguration) Validate() error {
	var errors []string

	if c.CPUCount < 1 {
		errors = append(errors, "CPU 코어가 최소 1개 필요합니다")
	}
	if c.MemorySize < 512*1024*1024 {
		errors = append(errors, "메모리가 최소 512MB 필요합니다")
	}
	if len(c.Graphics) == 0 {
		errors = append(errors, "그래픽 디바이스가 필요합니다")
	}
	if len(c.Storage) == 0 {
		errors = append(errors, "스토리지 디바이스가 필요합니다")
	}

	if len(errors) > 0 {
		return fmt.Errorf("구성 검증 실패:\n  - %s", strings.Join(errors, "\n  - "))
	}
	return nil
}

// Print: 구성 정보 출력
func (c *VirtualMachineConfiguration) Print() {
	fmt.Println("  +--------------------------------------------+")
	fmt.Printf("  | Boot Loader:  %-29s|\n", c.BootLoader.Type)
	fmt.Printf("  | CPU Count:    %-29d|\n", c.CPUCount)
	fmt.Printf("  | Memory:       %-29s|\n", formatMemory(c.MemorySize))
	fmt.Printf("  | Platform:     %-29s|\n", c.Platform.Type)
	fmt.Println("  +--------------------------------------------+")

	fmt.Printf("  | Graphics:     ")
	for i, g := range c.Graphics {
		if i > 0 {
			fmt.Printf("  |               ")
		}
		fmt.Printf("%-29s|\n", fmt.Sprintf("%s %dx%d", g.Type, g.Width, g.Height))
	}

	fmt.Printf("  | Audio:        ")
	for i, a := range c.Audio {
		if i > 0 {
			fmt.Printf("  |               ")
		}
		mode := "null-speaker"
		if a.HasInput && a.HasOutput {
			mode = "input+output"
		}
		fmt.Printf("%-29s|\n", fmt.Sprintf("%s (%s)", a.Type, mode))
	}

	fmt.Printf("  | Keyboards:    ")
	names := make([]string, len(c.Keyboards))
	for i, k := range c.Keyboards {
		names[i] = k.Type
	}
	fmt.Printf("%-29s|\n", strings.Join(names, ", "))

	fmt.Printf("  | Pointing:     ")
	pnames := make([]string, len(c.PointingDevices))
	for i, p := range c.PointingDevices {
		pnames[i] = p.Type
	}
	fmt.Printf("%-29s|\n", strings.Join(pnames, ", "))

	fmt.Printf("  | Networks:     ")
	for i, n := range c.Networks {
		if i > 0 {
			fmt.Printf("  |               ")
		}
		fmt.Printf("%-29s|\n", fmt.Sprintf("%s (MAC: %s)", n.Type, n.MACAddress))
	}

	fmt.Printf("  | Storage:      ")
	for i, s := range c.Storage {
		if i > 0 {
			fmt.Printf("  |               ")
		}
		fmt.Printf("%-29s|\n", fmt.Sprintf("%s (cache=%s, sync=%s)", s.Type, s.Caching, s.Sync))
	}

	fmt.Printf("  | Entropy:      %-29v|\n", c.Entropy)
	fmt.Printf("  | Clipboard:    %-29v|\n", c.Clipboard)
	fmt.Printf("  | Console:      %-29s|\n", strings.Join(c.ConsoleDevices, ", "))
	fmt.Printf("  | Socket:       %-29s|\n", strings.Join(c.SocketDevices, ", "))
	fmt.Println("  +--------------------------------------------+")
}

func formatMemory(bytes uint64) string {
	gb := float64(bytes) / 1024 / 1024 / 1024
	if gb >= 1 {
		return fmt.Sprintf("%.1f GB", gb)
	}
	mb := float64(bytes) / 1024 / 1024
	return fmt.Sprintf("%.0f MB", mb)
}

// ---------------------------------------------------------------------------
// craftConfiguration: VM 구성 빌더
//   실제 코드: Sources/tart/VM.swift — static func craftConfiguration(...)
//   디스크 URL, NVRAM URL, VMConfig, Network, 추가 옵션을 받아
//   VZVirtualMachineConfiguration을 조립하고 validate() 호출
// ---------------------------------------------------------------------------
func craftConfiguration(
	diskPath string,
	nvramPath string,
	platform Platform,
	cpuCount int,
	memorySize uint64,
	macAddress string,
	displayWidth int,
	displayHeight int,
	displayUnit string,
	networkType string,
	suspendable bool,
	nested bool,
	audio bool,
	clipboard bool,
	syncMode string,
	cachingMode string,
	noTrackpad bool,
	noPointer bool,
	noKeyboard bool,
) (*VirtualMachineConfiguration, error) {

	config := &VirtualMachineConfiguration{}

	// 1. 부트로더 설정
	//    실제: configuration.bootLoader = try vmConfig.platform.bootLoader(nvramURL:)
	config.BootLoader = platform.BootLoader(nvramPath)

	// 2. CPU/메모리 설정
	//    실제: configuration.cpuCount = vmConfig.cpuCount
	//          configuration.memorySize = vmConfig.memorySize
	config.CPUCount = cpuCount
	config.MemorySize = memorySize

	// 3. 플랫폼 설정
	//    실제: configuration.platform = try vmConfig.platform.platform(nvramURL:, needsNestedVirtualization:)
	config.Platform = platform.PlatformConfig(nvramPath, nested)

	// 4. 그래픽 디바이스
	//    실제: configuration.graphicsDevices = [vmConfig.platform.graphicsDevice(vmConfig:)]
	config.Graphics = []GraphicsDevice{
		platform.GraphicsDevice(displayWidth, displayHeight, displayUnit),
	}

	// 5. 오디오 디바이스
	//    실제: VZVirtioSoundDeviceConfiguration
	//    audio && !suspendable → 입력+출력, 그 외 → null speaker
	audioDevice := AudioDevice{Type: "VirtioSoundDevice"}
	if audio && !suspendable {
		audioDevice.HasInput = true
		audioDevice.HasOutput = true
	}
	config.Audio = []AudioDevice{audioDevice}

	// 6. 키보드 & 포인팅 디바이스
	//    실제: suspendable + PlatformSuspendable인 경우 제한된 디바이스 사용
	if suspendable {
		if sp, ok := platform.(PlatformSuspendable); ok {
			config.Keyboards = sp.KeyboardsSuspendable()
			config.PointingDevices = sp.PointingDevicesSuspendable()
		} else {
			config.Keyboards = platform.Keyboards()
			config.PointingDevices = platform.PointingDevices()
		}
	} else {
		if noKeyboard {
			config.Keyboards = nil
		} else {
			config.Keyboards = platform.Keyboards()
		}

		if noPointer {
			config.PointingDevices = nil
		} else if noTrackpad {
			config.PointingDevices = platform.PointingDevicesSimplified()
		} else {
			config.PointingDevices = platform.PointingDevices()
		}
	}

	// 7. 네트워크
	//    실제: configuration.networkDevices = network.attachments().map { ... }
	config.Networks = []NetworkAttachment{
		{Type: networkType, MACAddress: macAddress},
	}

	// 8. 클립보드 공유 (Spice Agent)
	//    실제: VZVirtioConsoleDeviceConfiguration + VZSpiceAgentPortAttachment
	config.Clipboard = clipboard
	if clipboard {
		config.ConsoleDevices = append(config.ConsoleDevices, "SpiceAgent")
	}

	// 9. 스토리지
	//    실제: VZDiskImageStorageDeviceAttachment(url:, readOnly:, cachingMode:, syncMode:)
	//    Linux → .cached (기본값), Darwin → .automatic
	caching := cachingMode
	if caching == "" {
		if platform.GetOS() == OSLinux {
			caching = "cached"
		} else {
			caching = "automatic"
		}
	}
	sync := syncMode
	if sync == "" {
		sync = "full"
	}
	config.Storage = []StorageDevice{
		{Type: "VirtioBlockDevice", DiskPath: diskPath, Caching: caching, Sync: sync},
	}

	// 10. 엔트로피 디바이스
	//     실제: if !suspendable { configuration.entropyDevices = [VZVirtioEntropyDeviceConfiguration()] }
	config.Entropy = !suspendable

	// 11. 버전 콘솔 디바이스
	//     실제: "tart-version-{CI.version}" 포트가 있는 콘솔
	config.ConsoleDevices = append(config.ConsoleDevices, "tart-version-1.0.0")

	// 12. 소켓 디바이스
	//     실제: configuration.socketDevices = [VZVirtioSocketDeviceConfiguration()]
	config.SocketDevices = []string{"VirtioSocketDevice"}

	// 13. validate()
	//     실제: try configuration.validate()
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}

// ---------------------------------------------------------------------------
// main: 시뮬레이션 실행
// ---------------------------------------------------------------------------
func main() {
	fmt.Println("=== tart 가상 머신 구성 빌더 패턴 시뮬레이션 ===")

	// --- 시나리오 1: macOS VM (기본 구성) ---
	fmt.Println("\n--- 시나리오 1: macOS VM 기본 구성 ---")
	darwin := &DarwinPlatform{
		ECID:          "sample-ecid-base64",
		HardwareModel: "sample-hw-model-base64",
	}
	cfg1, err := craftConfiguration(
		"/path/to/disk.img", "/path/to/nvram.bin",
		darwin, 4, 4*1024*1024*1024,
		"a2:b4:c6:d8:e0:f2",
		1024, 768, "pt",
		"NAT(Shared)", false, false, true, true, "", "", false, false, false,
	)
	if err != nil {
		fmt.Printf("구성 실패: %v\n", err)
		return
	}
	cfg1.Print()

	// --- 시나리오 2: Linux VM ---
	fmt.Println("\n--- 시나리오 2: Linux VM 구성 ---")
	linux := &LinuxPlatform{}
	cfg2, err := craftConfiguration(
		"/path/to/disk.img", "/path/to/nvram.bin",
		linux, 2, 2*1024*1024*1024,
		"b2:c4:d6:e8:f0:a2",
		1920, 1080, "px",
		"NAT(Shared)", false, false, true, true, "", "", false, false, false,
	)
	if err != nil {
		fmt.Printf("구성 실패: %v\n", err)
		return
	}
	cfg2.Print()

	// --- 시나리오 3: Suspendable 모드 (macOS) ---
	fmt.Println("\n--- 시나리오 3: macOS VM Suspendable 모드 ---")
	fmt.Println("  [Suspendable] 일부 디바이스 비활성화:")
	fmt.Println("  - 오디오: null speaker (입력/출력 스트림 없음)")
	fmt.Println("  - 키보드: MacKeyboard만 (USB 키보드 없음)")
	fmt.Println("  - 포인팅: MacTrackpad만 (USB 포인팅 없음)")
	fmt.Println("  - 엔트로피: 비활성화")

	cfg3, err := craftConfiguration(
		"/path/to/disk.img", "/path/to/nvram.bin",
		darwin, 4, 4*1024*1024*1024,
		"c2:d4:e6:f8:a0:b2",
		1024, 768, "pt",
		"NAT(Shared)", true, false, true, true, "", "", false, false, false,
	)
	if err != nil {
		fmt.Printf("구성 실패: %v\n", err)
		return
	}
	cfg3.Print()

	// --- 시나리오 4: noTrackpad, noKeyboard 옵션 ---
	fmt.Println("\n--- 시나리오 4: noTrackpad + noKeyboard 옵션 ---")
	cfg4, err := craftConfiguration(
		"/path/to/disk.img", "/path/to/nvram.bin",
		darwin, 4, 4*1024*1024*1024,
		"d2:e4:f6:a8:b0:c2",
		1024, 768, "pt",
		"NAT(Shared)", false, false, true, false, "", "", true, false, true,
	)
	if err != nil {
		fmt.Printf("구성 실패: %v\n", err)
		return
	}
	cfg4.Print()

	// --- 시나리오 5: Linux VM + 중첩 가상화 ---
	fmt.Println("\n--- 시나리오 5: Linux VM + 중첩 가상화 ---")
	cfg5, err := craftConfiguration(
		"/path/to/disk.img", "/path/to/nvram.bin",
		linux, 8, 16*1024*1024*1024,
		"e2:f4:a6:b8:c0:d2",
		3840, 2160, "px",
		"Bridged(en0)", false, true, true, true, "full", "cached", false, false, false,
	)
	if err != nil {
		fmt.Printf("구성 실패: %v\n", err)
		return
	}
	cfg5.Print()
	fmt.Printf("  중첩 가상화: %v\n", cfg5.Platform.NestedVirtualization)

	// --- 시나리오 6: 유효성 검증 실패 ---
	fmt.Println("\n--- 시나리오 6: 유효성 검증 실패 케이스 ---")
	badConfig := &VirtualMachineConfiguration{
		CPUCount:   0,
		MemorySize: 256 * 1024 * 1024,
	}
	if err := badConfig.Validate(); err != nil {
		fmt.Printf("  검증 실패 (예상됨): %v\n", err)
	}

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}
