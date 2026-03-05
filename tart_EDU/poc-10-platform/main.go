package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// =============================================================================
// tart Platform 서브시스템 시뮬레이션
// =============================================================================
//
// tart는 Protocol(인터페이스) 기반으로 Darwin/Linux 플랫폼을 추상화한다.
// 실제 소스: Sources/tart/Platform/Platform.swift, Darwin.swift, Linux.swift
//
// 핵심 설계:
// 1. Platform 프로토콜 — OS(), BootLoader(), GraphicsDevice(), Keyboards() 등
// 2. PlatformSuspendable 프로토콜 — 일시정지 모드에서 제한된 디바이스
// 3. VMConfig.init(from:) — JSON의 "os" 필드로 다형성 디코딩
// 4. Darwin: macOS 부트로더, Mac 그래픽(point/pixel), Mac+USB 키보드, 트랙패드
// 5. Linux: EFI 부트로더, Virtio 그래픽, USB 키보드만

// =============================================================================
// OS 열거형 — Sources/tart/Platform/OS.swift 참조
// =============================================================================

// OS는 가상머신의 운영체제 유형을 나타낸다.
// tart에서는 enum OS: String, Codable { case darwin; case linux } 로 정의됨
type OS string

const (
	OSDarwin OS = "darwin"
	OSLinux  OS = "linux"
)

// =============================================================================
// 디스플레이 단위 — Sources/tart/VMConfig.swift의 VMDisplayConfig.Unit 참조
// =============================================================================

// DisplayUnit은 디스플레이 해상도의 단위를 나타낸다.
// tart에서 Darwin 플랫폼은 point(pt)와 pixel(px) 두 가지 모드를 지원한다.
type DisplayUnit string

const (
	UnitPoint DisplayUnit = "pt" // macOS 논리적 포인트 단위 (Retina 스케일링 적용)
	UnitPixel DisplayUnit = "px" // 물리적 픽셀 단위
)

// =============================================================================
// 디바이스 타입 정의
// =============================================================================

// BootLoader는 부트로더 유형을 나타낸다.
// Darwin: VZMacOSBootLoader (macOS 부트로더)
// Linux: VZEFIBootLoader (EFI 부트로더 + 변수 저장소)
type BootLoader struct {
	Type        string `json:"type"`        // "macos" 또는 "efi"
	NVRAMPath   string `json:"nvram_path"`  // NVRAM 파일 경로
	HasVarStore bool   `json:"has_var_store"` // EFI 변수 저장소 유무
}

// GraphicsDevice는 그래픽 장치 설정을 나타낸다.
// Darwin: VZMacGraphicsDeviceConfiguration (point/pixel 모드)
// Linux: VZVirtioGraphicsDeviceConfiguration (pixel 전용)
type GraphicsDevice struct {
	Type         string      `json:"type"`          // "mac_graphics" 또는 "virtio_graphics"
	Width        int         `json:"width"`
	Height       int         `json:"height"`
	Unit         DisplayUnit `json:"unit,omitempty"` // Darwin만 point/pixel 구분
	PixelsPerInch int        `json:"ppi,omitempty"`  // Darwin pixel 모드 시 72 고정
}

// Keyboard는 키보드 유형을 나타낸다.
type Keyboard struct {
	Type string `json:"type"` // "usb", "mac"
}

// PointingDevice는 포인팅 장치 유형을 나타낸다.
type PointingDevice struct {
	Type string `json:"type"` // "usb_screen_coordinate", "mac_trackpad"
}

// =============================================================================
// Platform 인터페이스 — Sources/tart/Platform/Platform.swift 참조
// =============================================================================

// Platform은 tart의 핵심 프로토콜로, 가상머신의 플랫폼별 설정을 추상화한다.
// protocol Platform: Codable {
//   func os() -> OS
//   func bootLoader(nvramURL: URL) throws -> VZBootLoader
//   func graphicsDevice(vmConfig: VMConfig) -> VZGraphicsDeviceConfiguration
//   func keyboards() -> [VZKeyboardConfiguration]
//   func pointingDevices() -> [VZPointingDeviceConfiguration]
//   func pointingDevicesSimplified() -> [VZPointingDeviceConfiguration]
// }
type Platform interface {
	OS() OS
	BootLoader(nvramPath string) BootLoader
	PlatformConfig(nvramPath string, nestedVirt bool) (map[string]interface{}, error)
	GraphicsDevice(width, height int, unit DisplayUnit) GraphicsDevice
	Keyboards() []Keyboard
	PointingDevices() []PointingDevice
	PointingDevicesSimplified() []PointingDevice
}

// PlatformSuspendable은 일시정지(Suspendable) 모드를 지원하는 플랫폼 인터페이스이다.
// tart에서 Darwin만 이 프로토콜을 구현한다.
// 일시정지 모드에서는 디바이스 수를 제한하여 상태 저장 호환성을 보장한다.
// protocol PlatformSuspendable: Platform {
//   func pointingDevicesSuspendable() -> [VZPointingDeviceConfiguration]
//   func keyboardsSuspendable() -> [VZKeyboardConfiguration]
// }
type PlatformSuspendable interface {
	Platform
	KeyboardsSuspendable() []Keyboard
	PointingDevicesSuspendable() []PointingDevice
}

// =============================================================================
// DarwinPlatform — Sources/tart/Platform/Darwin.swift 참조
// =============================================================================

// DarwinPlatform은 macOS 가상머신 플랫폼을 나타낸다.
// tart의 Darwin 구조체는 ecid(VZMacMachineIdentifier)와
// hardwareModel(VZMacHardwareModel)을 base64로 직렬화한다.
type DarwinPlatform struct {
	ECID          string `json:"ecid"`           // Mac 고유 식별자 (base64)
	HardwareModel string `json:"hardwareModel"`  // Mac 하드웨어 모델 (base64)
}

func (d *DarwinPlatform) OS() OS {
	return OSDarwin
}

// BootLoader: macOS는 VZMacOSBootLoader를 사용한다.
// NVRAM은 Mac 보조 저장소(VZMacAuxiliaryStorage)로 사용됨
func (d *DarwinPlatform) BootLoader(nvramPath string) BootLoader {
	return BootLoader{
		Type:        "macos",
		NVRAMPath:   nvramPath,
		HasVarStore: false, // macOS 부트로더는 EFI 변수 저장소가 없음
	}
}

// PlatformConfig: VZMacPlatformConfiguration 생성
// - machineIdentifier = ecid
// - auxiliaryStorage = nvramURL
// - hardwareModel 지원 여부 확인
// - macOS는 중첩 가상화(nested virtualization)를 지원하지 않음
func (d *DarwinPlatform) PlatformConfig(nvramPath string, nestedVirt bool) (map[string]interface{}, error) {
	if nestedVirt {
		// 실제: throw RuntimeError.VMConfigurationError("macOS virtual machines do not support nested virtualization")
		return nil, fmt.Errorf("macOS 가상머신은 중첩 가상화를 지원하지 않습니다")
	}
	return map[string]interface{}{
		"type":               "mac_platform",
		"machine_identifier": d.ECID,
		"hardware_model":     d.HardwareModel,
		"auxiliary_storage":  nvramPath,
	}, nil
}

// GraphicsDevice: VZMacGraphicsDeviceConfiguration 생성
// - point 모드: 호스트 화면 기반 스케일링 (NSScreen.main 참조)
// - pixel 모드: 직접 픽셀 지정, PPI=72 고정
//   Apple 문서: https://developer.apple.com/documentation/coregraphics/1456599-cgdisplayscreensize
func (d *DarwinPlatform) GraphicsDevice(width, height int, unit DisplayUnit) GraphicsDevice {
	if unit == UnitPoint {
		return GraphicsDevice{
			Type:   "mac_graphics",
			Width:  width,
			Height: height,
			Unit:   UnitPoint,
		}
	}
	// pixel 모드 — PPI 72 고정 (Apple 문서 권장값)
	return GraphicsDevice{
		Type:          "mac_graphics",
		Width:         width,
		Height:        height,
		Unit:          UnitPixel,
		PixelsPerInch: 72,
	}
}

// Keyboards: macOS는 USB + Mac 키보드 모두 지원
// tart: [VZUSBKeyboardConfiguration(), VZMacKeyboardConfiguration()]
// Mac 키보드는 macOS 14+ (Ventura 게스트)부터 지원
func (d *DarwinPlatform) Keyboards() []Keyboard {
	return []Keyboard{
		{Type: "usb"},
		{Type: "mac"}, // macOS 14+ 에서만 사용 가능
	}
}

// KeyboardsSuspendable: 일시정지 모드에서는 Mac 키보드만 사용
// tart: [VZMacKeyboardConfiguration()] (macOS 14+)
func (d *DarwinPlatform) KeyboardsSuspendable() []Keyboard {
	return []Keyboard{
		{Type: "mac"},
	}
}

// PointingDevices: USB 포인팅 + Mac 트랙패드
// tart: [VZUSBScreenCoordinatePointingDeviceConfiguration(), VZMacTrackpadConfiguration()]
func (d *DarwinPlatform) PointingDevices() []PointingDevice {
	return []PointingDevice{
		{Type: "usb_screen_coordinate"},
		{Type: "mac_trackpad"},
	}
}

// PointingDevicesSimplified: 트랙패드 제외, USB만
// tart: [VZUSBScreenCoordinatePointingDeviceConfiguration()]
func (d *DarwinPlatform) PointingDevicesSimplified() []PointingDevice {
	return []PointingDevice{
		{Type: "usb_screen_coordinate"},
	}
}

// PointingDevicesSuspendable: 일시정지 모드에서는 트랙패드만
// tart: [VZMacTrackpadConfiguration()] (macOS 14+)
func (d *DarwinPlatform) PointingDevicesSuspendable() []PointingDevice {
	return []PointingDevice{
		{Type: "mac_trackpad"},
	}
}

// =============================================================================
// LinuxPlatform — Sources/tart/Platform/Linux.swift 참조
// =============================================================================

// LinuxPlatform은 Linux 가상머신 플랫폼을 나타낸다.
// tart의 Linux 구조체는 별도의 필드 없이 Platform 프로토콜만 구현한다.
type LinuxPlatform struct{}

func (l *LinuxPlatform) OS() OS {
	return OSLinux
}

// BootLoader: Linux는 VZEFIBootLoader + VZEFIVariableStore를 사용
func (l *LinuxPlatform) BootLoader(nvramPath string) BootLoader {
	return BootLoader{
		Type:        "efi",
		NVRAMPath:   nvramPath,
		HasVarStore: true, // EFI 변수 저장소 포함
	}
}

// PlatformConfig: VZGenericPlatformConfiguration 생성
// Linux는 macOS 15+에서 중첩 가상화를 지원한다.
func (l *LinuxPlatform) PlatformConfig(nvramPath string, nestedVirt bool) (map[string]interface{}, error) {
	config := map[string]interface{}{
		"type": "generic_platform",
	}
	if nestedVirt {
		config["nested_virtualization"] = true
	}
	return config, nil
}

// GraphicsDevice: VZVirtioGraphicsDeviceConfiguration (Virtio GPU)
// Linux는 항상 pixel 단위만 사용하고, point 모드는 없다.
func (l *LinuxPlatform) GraphicsDevice(width, height int, unit DisplayUnit) GraphicsDevice {
	return GraphicsDevice{
		Type:   "virtio_graphics",
		Width:  width,
		Height: height,
		// Linux는 unit 구분 없음 — 항상 pixel
	}
}

// Keyboards: Linux는 USB 키보드만 지원
// tart: [VZUSBKeyboardConfiguration()]
func (l *LinuxPlatform) Keyboards() []Keyboard {
	return []Keyboard{
		{Type: "usb"},
	}
}

// PointingDevices: Linux는 USB 포인팅만 지원 (트랙패드 없음)
// tart: [VZUSBScreenCoordinatePointingDeviceConfiguration()]
func (l *LinuxPlatform) PointingDevices() []PointingDevice {
	return []PointingDevice{
		{Type: "usb_screen_coordinate"},
	}
}

// PointingDevicesSimplified: Linux는 트랙패드가 없으므로 동일
func (l *LinuxPlatform) PointingDevicesSimplified() []PointingDevice {
	return l.PointingDevices()
}

// =============================================================================
// VMConfig — Sources/tart/VMConfig.swift 참조 (다형성 디코딩)
// =============================================================================

// VMDisplayConfig는 디스플레이 설정을 나타낸다.
type VMDisplayConfig struct {
	Width  int         `json:"width"`
	Height int         `json:"height"`
	Unit   DisplayUnit `json:"unit,omitempty"`
}

// VMConfig는 가상머신 설정을 나타낸다.
// tart의 VMConfig는 JSON 디코딩 시 "os" 필드를 먼저 읽어서
// darwin이면 Darwin을, linux면 Linux를 생성하는 다형성 패턴을 사용한다.
type VMConfig struct {
	Version    int             `json:"version"`
	OSType     OS              `json:"os"`
	Arch       string          `json:"arch"`
	CPUCount   int             `json:"cpuCount"`
	MemorySize uint64          `json:"memorySize"`
	MACAddress string          `json:"macAddress"`
	Display    VMDisplayConfig `json:"display"`

	// Platform은 OS 타입에 따라 다형적으로 결정됨
	Platform Platform `json:"-"`

	// Darwin 전용 필드 (JSON에 포함될 수 있음)
	ECID          string `json:"ecid,omitempty"`
	HardwareModel string `json:"hardwareModel,omitempty"`
}

// DecodeVMConfig는 JSON에서 VMConfig를 디코딩한다.
// tart VMConfig.init(from decoder:) 의 다형성 디코딩을 시뮬레이션:
//
//	switch os {
//	case .darwin: platform = try Darwin(from: decoder)
//	case .linux:  platform = try Linux(from: decoder)
//	}
func DecodeVMConfig(data []byte) (*VMConfig, error) {
	var config VMConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("JSON 디코딩 실패: %w", err)
	}

	// os 필드 기반 다형성 플랫폼 생성
	switch config.OSType {
	case OSDarwin:
		config.Platform = &DarwinPlatform{
			ECID:          config.ECID,
			HardwareModel: config.HardwareModel,
		}
	case OSLinux:
		config.Platform = &LinuxPlatform{}
	default:
		return nil, fmt.Errorf("알 수 없는 OS 유형: %s", config.OSType)
	}

	return &config, nil
}

// =============================================================================
// 출력 헬퍼
// =============================================================================

func printSeparator(title string) {
	fmt.Printf("\n%s\n", strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Printf("%s\n\n", strings.Repeat("=", 70))
}

func printDeviceList(label string, items []string) {
	fmt.Printf("  %s:\n", label)
	for i, item := range items {
		fmt.Printf("    [%d] %s\n", i, item)
	}
}

func printPlatformInfo(name string, p Platform, width, height int, unit DisplayUnit) {
	fmt.Printf("--- %s 플랫폼 ---\n", name)
	fmt.Printf("  OS: %s\n", p.OS())

	// 부트로더
	bl := p.BootLoader("/path/to/nvram.bin")
	fmt.Printf("  부트로더: type=%s, nvram=%s, efi_var_store=%v\n",
		bl.Type, bl.NVRAMPath, bl.HasVarStore)

	// 그래픽
	gd := p.GraphicsDevice(width, height, unit)
	fmt.Printf("  그래픽: type=%s, %dx%d", gd.Type, gd.Width, gd.Height)
	if gd.Unit != "" {
		fmt.Printf(" (%s)", gd.Unit)
	}
	if gd.PixelsPerInch > 0 {
		fmt.Printf(", ppi=%d", gd.PixelsPerInch)
	}
	fmt.Println()

	// 키보드
	keyboards := p.Keyboards()
	kbNames := make([]string, len(keyboards))
	for i, kb := range keyboards {
		kbNames[i] = kb.Type
	}
	printDeviceList("키보드", kbNames)

	// 포인팅 디바이스
	pointing := p.PointingDevices()
	pdNames := make([]string, len(pointing))
	for i, pd := range pointing {
		pdNames[i] = pd.Type
	}
	printDeviceList("포인팅 디바이스", pdNames)

	// Simplified 포인팅
	simplified := p.PointingDevicesSimplified()
	spNames := make([]string, len(simplified))
	for i, sp := range simplified {
		spNames[i] = sp.Type
	}
	printDeviceList("포인팅(Simplified)", spNames)
	fmt.Println()
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("tart Platform 서브시스템 시뮬레이션")
	fmt.Println("실제 소스: Sources/tart/Platform/{Platform,Darwin,Linux,OS}.swift")

	// =========================================================================
	// 1. 플랫폼 인터페이스 데모: Darwin vs Linux
	// =========================================================================
	printSeparator("1. Darwin vs Linux 플랫폼 비교")

	darwin := &DarwinPlatform{
		ECID:          "QUJDREVGR0hJSktMTU5PUA==",
		HardwareModel: "SGFyZHdhcmVNb2RlbERhdGE=",
	}
	linux := &LinuxPlatform{}

	printPlatformInfo("Darwin", darwin, 1920, 1200, UnitPoint)
	printPlatformInfo("Linux", linux, 1920, 1080, UnitPixel)

	// =========================================================================
	// 2. Darwin 그래픽 모드: point vs pixel
	// =========================================================================
	printSeparator("2. Darwin 그래픽 모드 비교 (point vs pixel)")

	gdPoint := darwin.GraphicsDevice(1920, 1200, UnitPoint)
	fmt.Printf("  Point 모드: type=%s, %dx%d%s\n",
		gdPoint.Type, gdPoint.Width, gdPoint.Height, gdPoint.Unit)
	fmt.Println("    -> 호스트 NSScreen.main 기반 Retina 스케일링 적용")

	gdPixel := darwin.GraphicsDevice(3840, 2400, UnitPixel)
	fmt.Printf("  Pixel 모드: type=%s, %dx%d%s, ppi=%d\n",
		gdPixel.Type, gdPixel.Width, gdPixel.Height, gdPixel.Unit, gdPixel.PixelsPerInch)
	fmt.Println("    -> 직접 픽셀 지정, PPI 72 고정 (Apple 문서 권장)")

	// =========================================================================
	// 3. Suspendable 모드: 제한된 디바이스
	// =========================================================================
	printSeparator("3. PlatformSuspendable — 일시정지 모드 디바이스 비교")

	// Darwin은 PlatformSuspendable 구현
	var suspendable PlatformSuspendable = darwin

	fmt.Println("--- 일반 모드 ---")
	normalKB := suspendable.Keyboards()
	fmt.Printf("  키보드: %d개 →", len(normalKB))
	for _, kb := range normalKB {
		fmt.Printf(" [%s]", kb.Type)
	}
	fmt.Println()
	normalPD := suspendable.PointingDevices()
	fmt.Printf("  포인팅: %d개 →", len(normalPD))
	for _, pd := range normalPD {
		fmt.Printf(" [%s]", pd.Type)
	}
	fmt.Println()

	fmt.Println("\n--- 일시정지(Suspendable) 모드 ---")
	suspKB := suspendable.KeyboardsSuspendable()
	fmt.Printf("  키보드: %d개 →", len(suspKB))
	for _, kb := range suspKB {
		fmt.Printf(" [%s]", kb.Type)
	}
	fmt.Println()
	suspPD := suspendable.PointingDevicesSuspendable()
	fmt.Printf("  포인팅: %d개 →", len(suspPD))
	for _, pd := range suspPD {
		fmt.Printf(" [%s]", pd.Type)
	}
	fmt.Println()

	fmt.Println("\n  [설명] 일시정지 모드에서는 상태 저장 호환성을 위해")
	fmt.Println("  디바이스 수를 최소화한다 (Mac 키보드/트랙패드만 유지)")

	// Linux는 PlatformSuspendable을 구현하지 않음
	fmt.Println("\n  [참고] Linux는 PlatformSuspendable 미구현 — 일시정지 모드 미지원")

	// =========================================================================
	// 4. 중첩 가상화(Nested Virtualization) 지원 차이
	// =========================================================================
	printSeparator("4. 중첩 가상화 지원 차이")

	_, err := darwin.PlatformConfig("/nvram.bin", true)
	if err != nil {
		fmt.Printf("  Darwin + 중첩 가상화: 오류 — %s\n", err)
	}

	linuxConfig, err := linux.PlatformConfig("/nvram.bin", true)
	if err == nil {
		fmt.Printf("  Linux + 중첩 가상화: 성공 — %v\n", linuxConfig)
	}

	_, err = darwin.PlatformConfig("/nvram.bin", false)
	if err == nil {
		fmt.Println("  Darwin + 중첩 가상화 없음: 성공")
	}

	// =========================================================================
	// 5. JSON 다형성 디코딩 — VMConfig.init(from:)
	// =========================================================================
	printSeparator("5. JSON 다형성 디코딩 (os 필드 기반)")

	// Darwin VM 설정 JSON
	darwinJSON := `{
  "version": 1,
  "os": "darwin",
  "arch": "arm64",
  "ecid": "QUJDREVGR0hJSktMTU5PUA==",
  "hardwareModel": "SGFyZHdhcmVNb2RlbERhdGE=",
  "cpuCount": 4,
  "memorySize": 8589934592,
  "macAddress": "7a:65:e4:3f:b2:01",
  "display": {"width": 1920, "height": 1200, "unit": "pt"}
}`

	fmt.Println("  Darwin VM JSON 디코딩:")
	darwinConfig, err := DecodeVMConfig([]byte(darwinJSON))
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		fmt.Printf("    os=%s, platform=%T\n", darwinConfig.OSType, darwinConfig.Platform)
		fmt.Printf("    cpu=%d, memory=%d bytes\n", darwinConfig.CPUCount, darwinConfig.MemorySize)
		fmt.Printf("    display=%dx%d (%s)\n",
			darwinConfig.Display.Width, darwinConfig.Display.Height, darwinConfig.Display.Unit)
		bl := darwinConfig.Platform.BootLoader("/nvram.bin")
		fmt.Printf("    부트로더=%s\n", bl.Type)
	}

	// Linux VM 설정 JSON
	linuxJSON := `{
  "version": 1,
  "os": "linux",
  "arch": "arm64",
  "cpuCount": 2,
  "memorySize": 4294967296,
  "macAddress": "52:54:00:ab:cd:ef",
  "display": {"width": 1024, "height": 768}
}`

	fmt.Println("\n  Linux VM JSON 디코딩:")
	linuxConfig2, err := DecodeVMConfig([]byte(linuxJSON))
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		fmt.Printf("    os=%s, platform=%T\n", linuxConfig2.OSType, linuxConfig2.Platform)
		fmt.Printf("    cpu=%d, memory=%d bytes\n", linuxConfig2.CPUCount, linuxConfig2.MemorySize)
		bl := linuxConfig2.Platform.BootLoader("/nvram.bin")
		fmt.Printf("    부트로더=%s (efi_var_store=%v)\n", bl.Type, bl.HasVarStore)
	}

	// 잘못된 OS
	invalidJSON := `{"version": 1, "os": "windows", "arch": "x86_64", "cpuCount": 2, "memorySize": 4294967296, "macAddress": "aa:bb:cc:dd:ee:ff"}`
	fmt.Println("\n  잘못된 OS JSON 디코딩:")
	_, err = DecodeVMConfig([]byte(invalidJSON))
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	}

	// =========================================================================
	// 6. 인터페이스 타입 확인 (다형성 검증)
	// =========================================================================
	printSeparator("6. 인터페이스 타입 확인 (다형성 검증)")

	platforms := []Platform{darwin, linux}
	for _, p := range platforms {
		fmt.Printf("  %T:\n", p)
		fmt.Printf("    Platform 구현: O\n")
		if sp, ok := p.(PlatformSuspendable); ok {
			fmt.Printf("    PlatformSuspendable 구현: O\n")
			suspKBs := sp.KeyboardsSuspendable()
			fmt.Printf("    Suspendable 키보드: %d개\n", len(suspKBs))
		} else {
			fmt.Printf("    PlatformSuspendable 구현: X (일시정지 미지원)\n")
		}
	}

	fmt.Println("\n[완료] tart Platform 시뮬레이션 종료")
}
