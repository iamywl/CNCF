package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// =============================================================================
// tart 데이터 모델 시뮬레이션: VMConfig, VMDirectory, OCI Manifest
//
// tart 실제 소스코드 참조:
//   - Sources/tart/VMConfig.swift       : struct VMConfig, VMDisplayConfig
//   - Sources/tart/VMDirectory.swift    : struct VMDirectory, State enum
//   - Sources/tart/OCI/Manifest.swift   : OCIManifest, OCIManifestLayer, OCIManifestConfig
//   - Sources/tart/Platform/OS.swift    : enum OS { darwin, linux }
//   - Sources/tart/Platform/Architecture.swift : enum Architecture { arm64, amd64 }
// =============================================================================

// ---------------------------------------------------------------------------
// OS: 운영체제 타입
//   실제 코드: Sources/tart/Platform/OS.swift — enum OS: String, Codable
// ---------------------------------------------------------------------------
type OS string

const (
	OSDarwin OS = "darwin"
	OSLinux  OS = "linux"
)

// ---------------------------------------------------------------------------
// Architecture: CPU 아키텍처
//   실제 코드: Sources/tart/Platform/Architecture.swift — enum Architecture
// ---------------------------------------------------------------------------
type Architecture string

const (
	ArchArm64 Architecture = "arm64"
	ArchAmd64 Architecture = "amd64"
)

// ---------------------------------------------------------------------------
// DiskImageFormat: 디스크 이미지 포맷
//   실제 코드: Sources/tart/DiskImageFormat.swift — enum DiskImageFormat
// ---------------------------------------------------------------------------
type DiskImageFormat string

const (
	DiskFormatRaw  DiskImageFormat = "raw"
	DiskFormatASIF DiskImageFormat = "asif"
)

// ---------------------------------------------------------------------------
// VMDisplayConfig: VM 디스플레이 설정
//   실제 코드: Sources/tart/VMConfig.swift — struct VMDisplayConfig: Codable, Equatable
//   width, height, unit(pt/px) 필드를 가짐
//   CustomStringConvertible로 "1024x768pt" 형식의 문자열 표현 제공
// ---------------------------------------------------------------------------
type VMDisplayConfig struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Unit   string `json:"unit,omitempty"` // "pt" 또는 "px", 비어있으면 단위 없음
}

func (d VMDisplayConfig) String() string {
	if d.Unit != "" {
		return fmt.Sprintf("%dx%d%s", d.Width, d.Height, d.Unit)
	}
	return fmt.Sprintf("%dx%d", d.Width, d.Height)
}

// ---------------------------------------------------------------------------
// MACAddress: MAC 주소 생성 및 검증
//   실제 코드: VZMACAddress.randomLocallyAdministered()
//   로컬 관리 MAC 주소는 두 번째 니블의 최하위 2비트가 10 (locally administered)
// ---------------------------------------------------------------------------
type MACAddress string

func RandomLocallyAdministered() MACAddress {
	buf := make([]byte, 6)
	rand.Read(buf)
	// locally administered, unicast 비트 설정
	// 실제: VZMACAddress.randomLocallyAdministered()
	buf[0] = (buf[0] | 0x02) & 0xFE // 비트 1 = 1 (local), 비트 0 = 0 (unicast)
	return MACAddress(fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		buf[0], buf[1], buf[2], buf[3], buf[4], buf[5]))
}

// ---------------------------------------------------------------------------
// VMConfig: VM 설정 구조체
//   실제 코드: Sources/tart/VMConfig.swift — struct VMConfig: Codable
//   version, os, arch, platform(Darwin/Linux), cpuCountMin, cpuCount,
//   memorySizeMin, memorySize, macAddress, display, displayRefit, diskFormat
//
//   JSON 직렬화/역직렬화 지원, CPU/메모리 유효성 검증
// ---------------------------------------------------------------------------
type VMConfig struct {
	Version       int             `json:"version"`
	OS            OS              `json:"os"`
	Arch          Architecture    `json:"arch"`
	CPUCountMin   int             `json:"cpuCountMin"`
	CPUCount      int             `json:"cpuCount"`
	MemorySizeMin uint64          `json:"memorySizeMin"`
	MemorySize    uint64          `json:"memorySize"`
	MACAddress    MACAddress      `json:"macAddress"`
	Display       VMDisplayConfig `json:"display"`
	DisplayRefit  *bool           `json:"displayRefit,omitempty"`
	DiskFormat    DiskImageFormat `json:"diskFormat"`

	// macOS 전용 필드 (실제: Darwin struct의 ecid, hardwareModel)
	ECID          string `json:"ecid,omitempty"`
	HardwareModel string `json:"hardwareModel,omitempty"`
}

// NewVMConfig: VMConfig 생성자
//   실제 코드: VMConfig.init(platform:, cpuCountMin:, memorySizeMin:, macAddress:, diskFormat:)
func NewVMConfig(osType OS, cpuCountMin int, memorySizeMin uint64) *VMConfig {
	return &VMConfig{
		Version:       1,
		OS:            osType,
		Arch:          ArchArm64,
		CPUCountMin:   cpuCountMin,
		CPUCount:      cpuCountMin,
		MemorySizeMin: memorySizeMin,
		MemorySize:    memorySizeMin,
		MACAddress:    RandomLocallyAdministered(),
		Display:       VMDisplayConfig{Width: 1024, Height: 768},
		DiskFormat:    DiskFormatRaw,
	}
}

// SetCPU: CPU 코어 수 설정 (유효성 검증 포함)
//   실제 코드: VMConfig.setCPU(cpuCount:) — cpuCountMin 및 시스템 최소값 체크
func (c *VMConfig) SetCPU(cpuCount int) error {
	// 실제 tart: minimumAllowedCPUCount = 1 (VZVirtualMachineConfiguration)
	const minimumAllowedCPUCount = 1

	if c.OS == OSDarwin && cpuCount < c.CPUCountMin {
		return fmt.Errorf("LessThanMinimalResourcesError: VM은 최소 %d개 CPU 코어가 필요합니다 (요청: %d)",
			c.CPUCountMin, cpuCount)
	}
	if cpuCount < minimumAllowedCPUCount {
		return fmt.Errorf("LessThanMinimalResourcesError: VM은 최소 %d개 CPU 코어가 필요합니다 (요청: %d)",
			minimumAllowedCPUCount, cpuCount)
	}
	c.CPUCount = cpuCount
	return nil
}

// SetMemory: 메모리 크기 설정 (유효성 검증 포함)
//   실제 코드: VMConfig.setMemory(memorySize:) — memorySizeMin 및 시스템 최소값 체크
func (c *VMConfig) SetMemory(memorySize uint64) error {
	// 실제 tart: minimumAllowedMemorySize (VZVirtualMachineConfiguration)
	const minimumAllowedMemorySize uint64 = 512 * 1024 * 1024 // 512MB

	if c.OS == OSDarwin && memorySize < c.MemorySizeMin {
		return fmt.Errorf("LessThanMinimalResourcesError: VM은 최소 %d 바이트 메모리가 필요합니다 (요청: %d)",
			c.MemorySizeMin, memorySize)
	}
	if memorySize < minimumAllowedMemorySize {
		return fmt.Errorf("LessThanMinimalResourcesError: VM은 최소 %d 바이트 메모리가 필요합니다 (요청: %d)",
			minimumAllowedMemorySize, memorySize)
	}
	c.MemorySize = memorySize
	return nil
}

// ToJSON: JSON 직렬화
//   실제 코드: VMConfig.toJSON() — Config.jsonEncoder().encode(self)
func (c *VMConfig) ToJSON() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

// FromJSON: JSON 역직렬화
//   실제 코드: VMConfig.init(fromJSON:) — Config.jsonDecoder().decode(Self.self, from:)
func VMConfigFromJSON(data []byte) (*VMConfig, error) {
	var config VMConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("VMConfig 디코딩 실패: %w", err)
	}
	return &config, nil
}

// ---------------------------------------------------------------------------
// VMDirectory.State: VM 상태 판별
//   실제 코드: Sources/tart/VMDirectory.swift — enum State, func state() throws -> State
//   running() → PIDLock.pid() != 0
//   Suspended → stateURL(state.vzvmsave) 파일 존재
//   Stopped → 그 외
// ---------------------------------------------------------------------------
type VMState string

const (
	VMStateRunning   VMState = "running"
	VMStateSuspended VMState = "suspended"
	VMStateStopped   VMState = "stopped"
)

// ---------------------------------------------------------------------------
// VMDirectory: VM 디렉토리 구조 관리
//   실제 코드: Sources/tart/VMDirectory.swift — struct VMDirectory: Prunable
//   baseURL을 기준으로 config.json, disk.img, nvram.bin 등의 경로를 파생
// ---------------------------------------------------------------------------
type VMDirectory struct {
	BaseURL string
}

func NewVMDirectory(baseURL string) *VMDirectory {
	return &VMDirectory{BaseURL: baseURL}
}

// 경로 파생 메서드들 (실제: VMDirectory의 computed property)
func (d *VMDirectory) ConfigURL() string   { return filepath.Join(d.BaseURL, "config.json") }
func (d *VMDirectory) DiskURL() string     { return filepath.Join(d.BaseURL, "disk.img") }
func (d *VMDirectory) NvramURL() string    { return filepath.Join(d.BaseURL, "nvram.bin") }
func (d *VMDirectory) StateURL() string    { return filepath.Join(d.BaseURL, "state.vzvmsave") }
func (d *VMDirectory) ManifestURL() string { return filepath.Join(d.BaseURL, "manifest.json") }
func (d *VMDirectory) ControlSocketURL() string {
	return filepath.Join(d.BaseURL, "control.sock")
}
func (d *VMDirectory) ExplicitlyPulledMark() string {
	return filepath.Join(d.BaseURL, ".explicitly-pulled")
}
func (d *VMDirectory) Name() string { return filepath.Base(d.BaseURL) }

// Initialized: VM 디렉토리가 완전히 초기화되었는지 확인
//   실제 코드: VMDirectory.initialized — config.json + disk.img + nvram.bin 모두 존재
func (d *VMDirectory) Initialized() bool {
	for _, path := range []string{d.ConfigURL(), d.DiskURL(), d.NvramURL()} {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// State: VM 상태 판별
//   실제 코드: VMDirectory.state() — running() → Suspended(state.vzvmsave) → Stopped
func (d *VMDirectory) State(pidLockActive bool) VMState {
	if pidLockActive {
		return VMStateRunning
	}
	if _, err := os.Stat(d.StateURL()); err == nil {
		return VMStateSuspended
	}
	return VMStateStopped
}

// Initialize: VM 디렉토리 초기화 (생성)
//   실제 코드: VMDirectory.initialize(overwrite:) — 디렉토리 생성 + 기존 파일 제거
func (d *VMDirectory) Initialize(overwrite bool) error {
	if !overwrite && d.Initialized() {
		return fmt.Errorf("VM 디렉토리가 이미 초기화되어 있습니다")
	}

	if err := os.MkdirAll(d.BaseURL, 0755); err != nil {
		return err
	}

	// 기존 파일 제거
	for _, path := range []string{d.ConfigURL(), d.DiskURL(), d.NvramURL()} {
		os.Remove(path)
	}
	return nil
}

// Validate: VM 디렉토리 유효성 검증
//   실제 코드: VMDirectory.validate(userFriendlyName:)
func (d *VMDirectory) Validate(name string) error {
	if _, err := os.Stat(d.BaseURL); os.IsNotExist(err) {
		return fmt.Errorf("VM '%s'이(가) 존재하지 않습니다", name)
	}
	if !d.Initialized() {
		return fmt.Errorf("VM에 필수 파일이 누락되었습니다 (config.json, disk.img, nvram.bin)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// OCI Manifest 구조체들
//   실제 코드: Sources/tart/OCI/Manifest.swift
// ---------------------------------------------------------------------------

// 미디어 타입 상수 (실제: Manifest.swift 상단 상수)
const (
	OCIManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	OCIConfigMediaType   = "application/vnd.oci.image.config.v1+json"
	ConfigLayerMediaType = "application/vnd.cirruslabs.tart.config.v1"
	DiskV2MediaType      = "application/vnd.cirruslabs.tart.disk.v2"
	NVRAMMediaType       = "application/vnd.cirruslabs.tart.nvram.v1"
)

// 어노테이션 키 (실제: Manifest.swift의 상수)
const (
	UncompressedDiskSizeAnnotation = "org.cirruslabs.tart.uncompressed-disk-size"
	UploadTimeAnnotation           = "org.cirruslabs.tart.upload-time"
	UncompressedSizeAnnotation     = "org.cirruslabs.tart.uncompressed-size"
	UncompressedDigestAnnotation   = "org.cirruslabs.tart.uncompressed-content-digest"
)

// OCIManifestConfig: OCI 매니페스트의 config 섹션
//   실제 코드: struct OCIManifestConfig: Codable, Equatable
type OCIManifestConfig struct {
	MediaType string `json:"mediaType"`
	Size      int    `json:"size"`
	Digest    string `json:"digest"`
}

// OCIManifestLayer: OCI 매니페스트의 레이어 항목
//   실제 코드: struct OCIManifestLayer: Codable, Equatable, Hashable
//   annotations으로 uncompressed-size, uncompressed-content-digest를 저장
type OCIManifestLayer struct {
	MediaType   string            `json:"mediaType"`
	Size        int               `json:"size"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func NewOCIManifestLayer(mediaType string, size int, digest string, uncompressedSize *uint64) OCIManifestLayer {
	layer := OCIManifestLayer{
		MediaType: mediaType,
		Size:      size,
		Digest:    digest,
	}

	if uncompressedSize != nil {
		layer.Annotations = map[string]string{
			UncompressedSizeAnnotation: fmt.Sprintf("%d", *uncompressedSize),
		}
	}
	return layer
}

func (l *OCIManifestLayer) UncompressedSize() *uint64 {
	if l.Annotations == nil {
		return nil
	}
	valStr, ok := l.Annotations[UncompressedSizeAnnotation]
	if !ok {
		return nil
	}
	var val uint64
	fmt.Sscanf(valStr, "%d", &val)
	return &val
}

// OCIManifest: OCI 이미지 매니페스트
//   실제 코드: struct OCIManifest: Codable, Equatable
//   schemaVersion=2, mediaType, config, layers, annotations
type OCIManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Config        OCIManifestConfig  `json:"config"`
	Layers        []OCIManifestLayer `json:"layers"`
	Annotations   map[string]string  `json:"annotations,omitempty"`
}

func NewOCIManifest(config OCIManifestConfig, layers []OCIManifestLayer, uncompressedDiskSize *uint64) *OCIManifest {
	m := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     OCIManifestMediaType,
		Config:        config,
		Layers:        layers,
	}

	if uncompressedDiskSize != nil {
		m.Annotations = map[string]string{
			UncompressedDiskSizeAnnotation: fmt.Sprintf("%d", *uncompressedDiskSize),
		}
	}
	return m
}

func (m *OCIManifest) ToJSON() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

func OCIManifestFromJSON(data []byte) (*OCIManifest, error) {
	var manifest OCIManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("OCIManifest 디코딩 실패: %w", err)
	}
	return &manifest, nil
}

func (m *OCIManifest) UncompressedDiskSize() *uint64 {
	if m.Annotations == nil {
		return nil
	}
	valStr, ok := m.Annotations[UncompressedDiskSizeAnnotation]
	if !ok {
		return nil
	}
	var val uint64
	fmt.Sscanf(valStr, "%d", &val)
	return &val
}

// ---------------------------------------------------------------------------
// 시뮬레이션 실행
// ---------------------------------------------------------------------------
func main() {
	fmt.Println("=== tart 데이터 모델 시뮬레이션 ===")

	// --- 1. VMConfig 생성 및 직렬화 ---
	fmt.Println("\n--- 1. VMConfig 생성 ---")
	config := NewVMConfig(OSDarwin, 4, 4*1024*1024*1024)
	config.ECID = "base64-encoded-ecid-data"
	config.HardwareModel = "base64-encoded-hardware-model"

	jsonData, err := config.ToJSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "JSON 직렬화 실패: %v\n", err)
		return
	}
	fmt.Printf("VMConfig JSON:\n%s\n", string(jsonData))

	// --- 2. VMConfig 역직렬화 ---
	fmt.Println("\n--- 2. VMConfig 역직렬화 ---")
	restored, err := VMConfigFromJSON(jsonData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "역직렬화 실패: %v\n", err)
		return
	}
	fmt.Printf("복원된 설정: OS=%s, Arch=%s, CPU=%d, Memory=%d, MAC=%s, Display=%s\n",
		restored.OS, restored.Arch, restored.CPUCount, restored.MemorySize,
		restored.MACAddress, restored.Display)

	// --- 3. CPU/메모리 유효성 검증 ---
	fmt.Println("\n--- 3. CPU/메모리 유효성 검증 ---")

	// 정상 케이스
	if err := config.SetCPU(8); err != nil {
		fmt.Printf("CPU 설정 실패: %v\n", err)
	} else {
		fmt.Printf("CPU를 8로 설정 성공 (cpuCount=%d)\n", config.CPUCount)
	}

	// 최소값 미만 — Darwin은 cpuCountMin 체크
	if err := config.SetCPU(2); err != nil {
		fmt.Printf("CPU 설정 실패 (예상된 에러): %v\n", err)
	}

	// 메모리 정상 케이스
	if err := config.SetMemory(8 * 1024 * 1024 * 1024); err != nil {
		fmt.Printf("메모리 설정 실패: %v\n", err)
	} else {
		fmt.Printf("메모리를 8GB로 설정 성공 (memorySize=%d)\n", config.MemorySize)
	}

	// 메모리 최소값 미만
	if err := config.SetMemory(256 * 1024 * 1024); err != nil {
		fmt.Printf("메모리 설정 실패 (예상된 에러): %v\n", err)
	}

	// Linux VM은 cpuCountMin 제약 없음
	linuxConfig := NewVMConfig(OSLinux, 4, 4*1024*1024*1024)
	if err := linuxConfig.SetCPU(2); err != nil {
		fmt.Printf("Linux CPU 설정 실패: %v\n", err)
	} else {
		fmt.Printf("Linux VM: CPU를 2로 설정 성공 (cpuCountMin 제약 없음)\n")
	}

	// --- 4. VMDirectory 구조 ---
	fmt.Println("\n--- 4. VMDirectory 구조 ---")
	tmpDir := filepath.Join(os.TempDir(), "tart-poc-vmdir")
	os.MkdirAll(tmpDir, 0755)
	defer os.RemoveAll(tmpDir)

	vmDir := NewVMDirectory(filepath.Join(tmpDir, "macos-ventura"))
	fmt.Printf("VMDirectory 경로:\n")
	fmt.Printf("  baseURL:        %s\n", vmDir.BaseURL)
	fmt.Printf("  configURL:      %s\n", vmDir.ConfigURL())
	fmt.Printf("  diskURL:        %s\n", vmDir.DiskURL())
	fmt.Printf("  nvramURL:       %s\n", vmDir.NvramURL())
	fmt.Printf("  stateURL:       %s\n", vmDir.StateURL())
	fmt.Printf("  manifestURL:    %s\n", vmDir.ManifestURL())
	fmt.Printf("  controlSocket:  %s\n", vmDir.ControlSocketURL())
	fmt.Printf("  name:           %s\n", vmDir.Name())

	// 초기화 전 상태
	fmt.Printf("  initialized:    %v\n", vmDir.Initialized())

	// 초기화 실행
	vmDir.Initialize(false)

	// 파일 생성
	os.WriteFile(vmDir.ConfigURL(), jsonData, 0644)
	os.WriteFile(vmDir.DiskURL(), []byte("fake-disk"), 0644)
	os.WriteFile(vmDir.NvramURL(), []byte("fake-nvram"), 0644)
	fmt.Printf("  initialized (후): %v\n", vmDir.Initialized())

	// 상태 판별
	fmt.Printf("  state (stopped):   %s\n", vmDir.State(false))
	fmt.Printf("  state (running):   %s\n", vmDir.State(true))

	// Suspended 상태 시뮬레이션
	os.WriteFile(vmDir.StateURL(), []byte("fake-save-state"), 0644)
	fmt.Printf("  state (suspended): %s\n", vmDir.State(false))

	// 유효성 검증
	if err := vmDir.Validate("macos-ventura"); err != nil {
		fmt.Printf("  검증 실패: %v\n", err)
	} else {
		fmt.Printf("  검증 성공\n")
	}

	// 존재하지 않는 VM 검증
	badDir := NewVMDirectory(filepath.Join(tmpDir, "nonexistent"))
	if err := badDir.Validate("nonexistent"); err != nil {
		fmt.Printf("  존재하지 않는 VM 검증: %v\n", err)
	}

	// --- 5. VMDisplayConfig ---
	fmt.Println("\n--- 5. VMDisplayConfig ---")
	displays := []VMDisplayConfig{
		{Width: 1024, Height: 768, Unit: ""},
		{Width: 1920, Height: 1080, Unit: "pt"},
		{Width: 3840, Height: 2160, Unit: "px"},
	}
	for _, d := range displays {
		fmt.Printf("  Display: %s\n", d)
	}

	// --- 6. OCI Manifest 생성 ---
	fmt.Println("\n--- 6. OCI Manifest ---")
	uncompSize := uint64(50 * 1024 * 1024 * 1024) // 50GB
	layerUncompSize := uint64(512 * 1024 * 1024)   // 512MB

	manifest := NewOCIManifest(
		OCIManifestConfig{
			MediaType: OCIConfigMediaType,
			Size:      256,
			Digest:    "sha256:abc123config",
		},
		[]OCIManifestLayer{
			NewOCIManifestLayer(ConfigLayerMediaType, 1024, "sha256:config001", nil),
			NewOCIManifestLayer(DiskV2MediaType, 100*1024*1024, "sha256:disk001", &layerUncompSize),
			NewOCIManifestLayer(DiskV2MediaType, 95*1024*1024, "sha256:disk002", &layerUncompSize),
			NewOCIManifestLayer(NVRAMMediaType, 2048, "sha256:nvram001", nil),
		},
		&uncompSize,
	)

	manifestJSON, _ := manifest.ToJSON()
	fmt.Printf("OCI Manifest JSON:\n%s\n", string(manifestJSON))

	// Manifest 역직렬화
	restored2, _ := OCIManifestFromJSON(manifestJSON)
	fmt.Printf("\n복원된 매니페스트:\n")
	fmt.Printf("  schemaVersion: %d\n", restored2.SchemaVersion)
	fmt.Printf("  mediaType: %s\n", restored2.MediaType)
	fmt.Printf("  config digest: %s\n", restored2.Config.Digest)
	fmt.Printf("  layers: %d개\n", len(restored2.Layers))

	if diskSize := restored2.UncompressedDiskSize(); diskSize != nil {
		fmt.Printf("  uncompressed disk size: %d bytes (%.1f GB)\n",
			*diskSize, float64(*diskSize)/1024/1024/1024)
	}

	// 레이어별 정보 출력
	fmt.Println("\n  레이어 상세:")
	for i, layer := range restored2.Layers {
		mediaShort := layer.MediaType[strings.LastIndex(layer.MediaType, ".")+1:]
		fmt.Printf("  [%d] type=%-10s size=%-12d digest=%s",
			i, mediaShort, layer.Size, layer.Digest)
		if us := layer.UncompressedSize(); us != nil {
			fmt.Printf(" uncompressed=%d", *us)
		}
		fmt.Println()
	}

	// --- 7. MAC 주소 생성 ---
	fmt.Println("\n--- 7. MAC 주소 생성 ---")
	for i := 0; i < 5; i++ {
		mac := RandomLocallyAdministered()
		fmt.Printf("  MAC[%d]: %s\n", i, mac)
	}

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}
