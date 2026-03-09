// Package main은 Tart의 Shell Completions, VNC 원격 접근, Serial 콘솔,
// PassphraseGenerator, 터미널 제어, 유틸리티 함수를
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Shell Completions: VM 목록 기반 자동완성 (completeMachines, completeLocal, completeRunning)
// 2. VNC Protocol 추상화: ScreenSharingVNC vs FullFledgedVNC
// 3. ScreenSharingVNC: MAC → IP 해석, vnc://IP URL 생성
// 4. FullFledgedVNC: 패스프레이즈 인증, 임의 포트 할당, 폴링
// 5. Serial 콘솔: PTY 생성, non-blocking, baud rate 설정
// 6. 터미널 제어 (Term): IsTerminal, MakeRaw, Restore, GetSize
// 7. PassphraseGenerator: BIP-39 단어 목록, Sequence 패턴
// 8. Utils: 안전한 인덱스 접근, PATH 바이너리 검색
// 9. Run.swift 통합: VNC/Serial/Graphics 조합 결정
//
// 실제 소스 참조:
//   - Sources/tart/ShellCompletions/ShellCompletions.swift
//   - Sources/tart/VNC/VNC.swift
//   - Sources/tart/VNC/ScreenSharingVNC.swift
//   - Sources/tart/VNC/FullFledgedVNC.swift
//   - Sources/tart/Serial.swift
//   - Sources/tart/Term.swift
//   - Sources/tart/Passphrase/PassphraseGenerator.swift
//   - Sources/tart/Passphrase/Words.swift
//   - Sources/tart/Utils.swift
//   - Sources/tart/Commands/Run.swift
package main

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"time"
)

// ============================================================================
// 1. VM 스토리지 및 Shell Completions
// ============================================================================

// VMState는 VM의 실행 상태를 나타낸다.
// 실제 소스: VMStorageLocal에서 vmDir.state()로 조회
type VMState int

const (
	StateStopped VMState = iota
	StateRunning
	StateSuspended
)

func (s VMState) String() string {
	switch s {
	case StateStopped:
		return "Stopped"
	case StateRunning:
		return "Running"
	case StateSuspended:
		return "Suspended"
	default:
		return "Unknown"
	}
}

// VMEntry는 스토리지에 저장된 VM 항목이다.
type VMEntry struct {
	Name  string
	State VMState
	Tag   string // OCI 태그 (OCI 스토리지만)
}

// VMStorageLocal은 로컬 VM 스토리지를 시뮬레이션한다.
// 실제 소스: Sources/tart/VMStorageLocal.swift
type VMStorageLocal struct {
	vms []VMEntry
}

// VMStorageOCI는 OCI 캐시 VM 스토리지를 시뮬레이션한다.
// 실제 소스: Sources/tart/VMStorageOCI.swift
type VMStorageOCI struct {
	vms []VMEntry
}

// normalizeName은 Zsh 자동완성에서 콜론을 이스케이프한다.
// 실제 소스: ShellCompletions.swift 3~6행
// Zsh에서 콜론은 completion:description 구분자로 해석되므로 \:로 이스케이프
func normalizeName(name string) string {
	return strings.ReplaceAll(name, ":", "\\:")
}

// completeMachines는 모든 VM(Local + OCI) 목록을 반환한다.
// 실제 소스: ShellCompletions.swift 8~16행
func completeMachines(local *VMStorageLocal, oci *VMStorageOCI) []string {
	var result []string
	for _, vm := range local.vms {
		result = append(result, normalizeName(vm.Name))
	}
	for _, vm := range oci.vms {
		result = append(result, normalizeName(vm.Name))
	}
	return result
}

// completeLocalMachines는 로컬 VM만 반환한다.
// 실제 소스: ShellCompletions.swift 18~21행
// tart run, tart set, tart delete 등에서 사용
func completeLocalMachines(local *VMStorageLocal) []string {
	var result []string
	for _, vm := range local.vms {
		result = append(result, normalizeName(vm.Name))
	}
	return result
}

// completeRunningMachines는 실행 중인 VM만 반환한다.
// 실제 소스: ShellCompletions.swift 23~28행
// tart stop, tart suspend, tart ip 등에서 사용
func completeRunningMachines(local *VMStorageLocal) []string {
	var result []string
	for _, vm := range local.vms {
		if vm.State == StateRunning {
			result = append(result, normalizeName(vm.Name))
		}
	}
	return result
}

// ============================================================================
// 2. VNC Protocol 추상화
// ============================================================================

// VNC는 VNC 서버의 공통 인터페이스이다.
// 실제 소스: Sources/tart/VNC/VNC.swift
// waitForURL과 stop 두 메서드만 정의
type VNC interface {
	WaitForURL(netBridged bool) (string, error)
	Stop() error
}

// ============================================================================
// 3. ScreenSharingVNC
// ============================================================================

// IPResolutionStrategy는 IP 해석 전략을 나타낸다.
type IPResolutionStrategy int

const (
	StrategyDHCP IPResolutionStrategy = iota // Shared 네트워크: DHCP 리스 조회
	StrategyARP                              // Bridged 네트워크: ARP 테이블 조회
)

func (s IPResolutionStrategy) String() string {
	if s == StrategyDHCP {
		return "DHCP"
	}
	return "ARP"
}

// ScreenSharingVNC는 macOS 내장 화면 공유를 통한 VNC를 시뮬레이션한다.
// 실제 소스: Sources/tart/VNC/ScreenSharingVNC.swift
// VM 내부의 화면 공유 서비스에 연결하며, VM의 MAC 주소로 IP를 해석
type ScreenSharingVNC struct {
	macAddress string
	// 시뮬레이션용 IP 테이블 (MAC → IP)
	dhcpLeases map[string]string
	arpTable   map[string]string
}

// resolveIP는 MAC 주소로 IP를 해석한다.
// 실제 소스: IP.resolveIP(vmMACAddress, resolutionStrategy:, secondsToWait:)
func (s *ScreenSharingVNC) resolveIP(strategy IPResolutionStrategy) (string, error) {
	var table map[string]string
	if strategy == StrategyDHCP {
		table = s.dhcpLeases
	} else {
		table = s.arpTable
	}

	if ip, ok := table[s.macAddress]; ok {
		return ip, nil
	}
	return "", fmt.Errorf("IPNotFound: could not resolve IP for MAC %s", s.macAddress)
}

// WaitForURL은 VM의 IP를 해석하여 VNC URL을 반환한다.
// 실제 소스: ScreenSharingVNC.swift 12~21행
func (s *ScreenSharingVNC) WaitForURL(netBridged bool) (string, error) {
	strategy := StrategyDHCP
	if netBridged {
		strategy = StrategyARP
	}

	ip, err := s.resolveIP(strategy)
	if err != nil {
		return "", err
	}

	// vnc://IP 형식 (포트 미지정 → 기본 5900)
	return fmt.Sprintf("vnc://%s", ip), nil
}

// Stop은 ScreenSharingVNC에서는 아무것도 하지 않는다.
// VM 내부 서비스에 연결만 하므로 호스트에서 정리할 리소스가 없다.
func (s *ScreenSharingVNC) Stop() error {
	return nil
}

// ============================================================================
// 4. FullFledgedVNC
// ============================================================================

// FullFledgedVNC는 Virtualization.Framework의 VNC 서버를 시뮬레이션한다.
// 실제 소스: Sources/tart/VNC/FullFledgedVNC.swift
// _VZVNCServer (비공개 API)를 Dynamic 라이브러리로 호출
type FullFledgedVNC struct {
	password string
	port     uint16
	running  bool
}

// NewFullFledgedVNC는 패스프레이즈를 생성하고 VNC 서버를 시작한다.
// 실제 소스: FullFledgedVNC.swift init 9~16행
func NewFullFledgedVNC() *FullFledgedVNC {
	// 4단어 패스프레이즈 생성
	password := generatePassphrase(4)

	v := &FullFledgedVNC{
		password: password,
		port:     0, // OS가 임의 포트 할당 (시뮬레이션)
		running:  true,
	}

	// 시뮬레이션: 약간의 지연 후 포트 할당
	// 실제: vnc.start() 호출 후 비동기적으로 포트 바인딩
	go func() {
		time.Sleep(10 * time.Millisecond)
		// 임의의 높은 포트 번호 할당
		v.port = uint16(49152 + rand.Intn(16383))
	}()

	return v
}

// WaitForURL은 VNC 서버가 준비될 때까지 폴링하고 URL을 반환한다.
// 실제 소스: FullFledgedVNC.swift 18~28행
// port가 0이 아닐 때까지 50ms 간격으로 폴링
func (v *FullFledgedVNC) WaitForURL(netBridged bool) (string, error) {
	for i := 0; i < 100; i++ { // 최대 5초 (100 * 50ms)
		if v.port != 0 {
			// vnc://:password@127.0.0.1:port 형식
			// 사용자명 없음 (: 앞이 비어있음)
			return fmt.Sprintf("vnc://:%s@127.0.0.1:%d", v.password, v.port), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", fmt.Errorf("VNC server failed to start within timeout")
}

// Stop은 VNC 서버를 종료한다.
// 실제 소스: FullFledgedVNC.swift stop() + deinit
func (v *FullFledgedVNC) Stop() error {
	v.running = false
	v.port = 0
	return nil
}

// ============================================================================
// 5. PassphraseGenerator (BIP-39)
// ============================================================================

// bip39Words는 BIP-39 표준 영어 단어 목록의 일부를 시뮬레이션한다.
// 실제 소스: Sources/tart/Passphrase/Words.swift
// 실제 목록은 2048개이며, 여기서는 시뮬레이션용으로 64개만 사용
var bip39Words = []string{
	"abandon", "ability", "able", "about", "above", "absent", "absorb", "abstract",
	"absurd", "abuse", "access", "accident", "account", "accuse", "achieve", "acid",
	"acoustic", "acquire", "across", "act", "action", "actor", "actress", "actual",
	"adapt", "add", "addict", "address", "adjust", "admit", "adult", "advance",
	"advice", "aerobic", "affair", "afford", "afraid", "again", "age", "agent",
	"agree", "ahead", "aim", "air", "airport", "aisle", "alarm", "album",
	"alcohol", "alert", "alien", "all", "alley", "allow", "almost", "alone",
	"alpha", "already", "also", "alter", "always", "amateur", "amazing", "among",
}

// PassphraseGenerator는 무한 단어 스트림을 생성한다.
// 실제 소스: Sources/tart/Passphrase/PassphraseGenerator.swift
// Swift Sequence 프로토콜을 구현하여 .prefix(4)로 4개 추출
type PassphraseGenerator struct {
	words []string
}

// Next는 랜덤 단어 하나를 반환한다.
// 실제 소스: PassphraseIterator.next()
// arc4random_uniform 대신 Go의 rand 사용 (시뮬레이션)
func (g *PassphraseGenerator) Next() string {
	return g.words[rand.Intn(len(g.words))]
}

// Prefix는 n개의 랜덤 단어를 반환한다.
// Swift의 .prefix(n) 시뮬레이션
func (g *PassphraseGenerator) Prefix(n int) []string {
	result := make([]string, n)
	for i := 0; i < n; i++ {
		result[i] = g.Next()
	}
	return result
}

// generatePassphrase는 n개 단어를 하이픈으로 결합한 패스프레이즈를 생성한다.
// 실제 사용: FullFledgedVNC.swift 10행
//
//	Array(PassphraseGenerator().prefix(4)).joined(separator: "-")
func generatePassphrase(n int) string {
	gen := &PassphraseGenerator{words: bip39Words}
	words := gen.Prefix(n)
	return strings.Join(words, "-")
}

// ============================================================================
// 6. Serial 콘솔 (PTY 시뮬레이션)
// ============================================================================

// PTYConfig는 PTY 설정을 나타낸다.
type PTYConfig struct {
	Path       string
	BaudRate   int
	NonBlock   bool
	MasterFD   int
	SlaveFD    int
	SlavePath  string
}

// SerialPort는 가상 시리얼 포트를 시뮬레이션한다.
// 실제 소스: Sources/tart/Serial.swift (createPTY 함수)
type SerialPort struct {
	config PTYConfig
	buffer []byte
}

// CreatePTY는 PTY를 생성한다.
// 실제 소스: Serial.swift createPTY() 전체
// 단계: openpty → close slave → fcntl O_NONBLOCK → tcsetattr 115200
func CreatePTY() (*SerialPort, error) {
	// 시뮬레이션: 실제로는 openpty() 시스템 콜
	masterFD := 3 + rand.Intn(100) // 시뮬레이션용 FD
	slavePath := fmt.Sprintf("/dev/ttys%03d", rand.Intn(999))

	config := PTYConfig{
		Path:      slavePath,
		BaudRate:  115200, // 가상 시리얼 표준 속도
		NonBlock:  true,
		MasterFD:  masterFD,
		SlaveFD:   -1, // close(sfd) — slave는 즉시 닫음
		SlavePath: slavePath,
	}

	return &SerialPort{
		config: config,
		buffer: make([]byte, 0),
	}, nil
}

// Write는 시리얼 포트에 데이터를 쓴다.
func (s *SerialPort) Write(data []byte) {
	s.buffer = append(s.buffer, data...)
}

// Read는 시리얼 포트에서 데이터를 읽는다 (non-blocking).
func (s *SerialPort) Read() []byte {
	if len(s.buffer) == 0 {
		return nil // non-blocking: 데이터 없으면 nil 반환
	}
	data := s.buffer
	s.buffer = nil
	return data
}

// ============================================================================
// 7. 터미널 제어 (Term)
// ============================================================================

// TermState는 터미널의 원래 상태를 저장한다.
// 실제 소스: Sources/tart/Term.swift — State 구조체
// fileprivate let termios로 외부 수정 방지
type TermState struct {
	echo    bool
	icanon  bool
	isig    bool
	rawMode bool
}

// Term은 터미널 제어 유틸리티이다.
// 실제 소스: Sources/tart/Term.swift
// 모든 메서드가 static — 터미널은 프로세스당 하나이므로 인스턴스 불필요
type Term struct{}

// IsTerminal은 표준 입력이 터미널인지 확인한다.
// 실제 소스: Term.swift IsTerminal()
// tcgetattr 성공 여부로 판단
func (Term) IsTerminal() bool {
	// 시뮬레이션: os.Stdin이 터미널인지 확인
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// MakeRaw는 터미널을 Raw 모드로 전환하고 원래 상태를 반환한다.
// 실제 소스: Term.swift MakeRaw()
// cfmakeraw: ECHO, ICANON, ISIG, IEXTEN, IXON, ICRNL, OPOST 비활성화
func (Term) MakeRaw() TermState {
	original := TermState{
		echo:    true,
		icanon:  true,
		isig:    true,
		rawMode: false,
	}
	// 실제: cfmakeraw(&termiosRaw) + tcsetattr(TCSANOW)
	// Raw 모드에서 비활성화되는 기능:
	//   ECHO   — 입력 에코
	//   ECHONL — 개행 에코
	//   ICANON — 정규 모드 (줄 버퍼링)
	//   ISIG   — INTR/QUIT/SUSP 시그널
	//   IEXTEN — 확장 입력 처리
	//   IXON   — XON/XOFF 흐름 제어
	//   ICRNL  — CR→NL 변환
	//   OPOST  — 출력 후처리
	return original
}

// Restore는 터미널을 원래 상태로 복원한다.
// 실제 소스: Term.swift Restore(_ state: State)
func (Term) Restore(state TermState) {
	// 실제: tcsetattr(TCSANOW, &state.termios)
	_ = state // 원래 상태로 복원
}

// GetSize는 터미널 창 크기를 반환한다.
// 실제 소스: Term.swift GetSize()
// ioctl(STDOUT_FILENO, TIOCGWINSZ, &winsize)
func (Term) GetSize() (width, height uint16) {
	// 시뮬레이션: 기본 터미널 크기 반환
	return 80, 24
}

// ============================================================================
// 8. Utils
// ============================================================================

// SafeIndex는 배열의 안전한 인덱스 접근을 제공한다.
// 실제 소스: Utils.swift — Collection subscript(safe:)
// Swift 배열의 범위 밖 접근 시 크래시 방지
func SafeIndex(slice []string, index int) (string, bool) {
	if index >= 0 && index < len(slice) {
		return slice[index], true
	}
	return "", false
}

// ResolveBinaryPath는 PATH 환경변수에서 바이너리를 검색한다.
// 실제 소스: Utils.swift resolveBinaryPath(_ name: String)
// 쉘의 which 명령과 동일한 기능
func ResolveBinaryPath(name string) (string, bool) {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return "", false
	}

	for _, dir := range strings.Split(pathEnv, ":") {
		fullPath := dir + "/" + name
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath, true
		}
	}
	return "", false
}

// ============================================================================
// 9. Run.swift 통합: VNC/Serial/Graphics 조합
// ============================================================================

// RunOptions는 tart run 명령의 옵션을 나타낸다.
// 실제 소스: Commands/Run.swift의 @Flag/@Option 매개변수들
type RunOptions struct {
	NoGraphics      bool
	VNCEnabled      bool   // --vnc
	VNCExperimental bool   // --vnc-experimental
	Serial          bool   // --serial
	SerialPath      string // --serial-path
}

// RunMode는 실행 모드를 결정한다.
type RunMode struct {
	DisplayMode string // "gui", "headless", "vnc-screen", "vnc-experimental"
	SerialMode  string // "none", "auto-pty", "external-pty"
}

// DetermineRunMode는 옵션 조합에 따라 실행 모드를 결정한다.
// 실제 소스: Commands/Run.swift validate() + run()
func DetermineRunMode(opts RunOptions) (RunMode, error) {
	// 상호 배타성 검증
	if opts.VNCEnabled && opts.VNCExperimental {
		return RunMode{}, fmt.Errorf("--vnc and --vnc-experimental are mutually exclusive")
	}

	mode := RunMode{SerialMode: "none"}

	// 디스플레이 모드 결정
	if opts.VNCExperimental {
		mode.DisplayMode = "vnc-experimental"
	} else if opts.VNCEnabled {
		mode.DisplayMode = "vnc-screen"
	} else if opts.NoGraphics {
		mode.DisplayMode = "headless"
	} else {
		mode.DisplayMode = "gui"
	}

	// VNC + no-graphics → headless + VNC URL만 출력
	if opts.NoGraphics && (opts.VNCEnabled || opts.VNCExperimental) {
		// headless + VNC 조합: URL만 출력
		if opts.VNCExperimental {
			mode.DisplayMode = "headless-vnc-experimental"
		} else {
			mode.DisplayMode = "headless-vnc-screen"
		}
	}

	// 시리얼 모드 결정
	if opts.Serial {
		mode.SerialMode = "auto-pty"
	} else if opts.SerialPath != "" {
		mode.SerialMode = "external-pty"
	}

	return mode, nil
}

// ============================================================================
// 메인 시뮬레이션
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Tart Shell/VNC/Utilities 시뮬레이션 PoC                     ║")
	fmt.Println("║  실제 소스: ShellCompletions, VNC/, Serial, Term, Utils      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	// === 1. Shell Completions ===
	fmt.Println("\n=== 1. Shell Completions ===")

	local := &VMStorageLocal{
		vms: []VMEntry{
			{Name: "macos-sonoma", State: StateRunning},
			{Name: "macos-ventura", State: StateStopped},
			{Name: "ubuntu-22.04", State: StateRunning},
			{Name: "debian-12", State: StateSuspended},
		},
	}
	oci := &VMStorageOCI{
		vms: []VMEntry{
			{Name: "ghcr.io/cirruslabs/macos-sonoma-base:latest", Tag: "latest"},
			{Name: "ghcr.io/cirruslabs/macos-ventura-xcode:15.2", Tag: "15.2"},
		},
	}

	all := completeMachines(local, oci)
	fmt.Printf("  completeMachines (%d개):\n", len(all))
	for _, name := range all {
		fmt.Printf("    %s\n", name)
	}

	localOnly := completeLocalMachines(local)
	fmt.Printf("\n  completeLocalMachines (%d개):\n", len(localOnly))
	for _, name := range localOnly {
		fmt.Printf("    %s\n", name)
	}

	running := completeRunningMachines(local)
	fmt.Printf("\n  completeRunningMachines (%d개):\n", len(running))
	for _, name := range running {
		fmt.Printf("    %s\n", name)
	}

	// normalizeName 데모
	fmt.Println("\n  normalizeName 콜론 이스케이프:")
	fmt.Printf("    원본: ghcr.io/cirruslabs/macos-sonoma-base:latest\n")
	fmt.Printf("    결과: %s\n", normalizeName("ghcr.io/cirruslabs/macos-sonoma-base:latest"))
	fmt.Println("    (Zsh에서 콜론은 completion:description 구분자)")

	// === 2. VNC Protocol 비교 ===
	fmt.Println("\n=== 2. VNC Protocol 비교 ===")
	fmt.Println("  ┌────────────────────┬─────────────────────┬──────────────────────┐")
	fmt.Println("  │ 항목               │ ScreenSharingVNC    │ FullFledgedVNC       │")
	fmt.Println("  ├────────────────────┼─────────────────────┼──────────────────────┤")
	fmt.Println("  │ 연결 대상          │ VM 내부 서비스      │ 호스트 VZ VNC 서버   │")
	fmt.Println("  │ CLI 플래그         │ --vnc               │ --vnc-experimental   │")
	fmt.Println("  │ IP                 │ VM IP (MAC 해석)    │ 127.0.0.1            │")
	fmt.Println("  │ 인증               │ VM 내부 설정        │ 자동 패스프레이즈    │")
	fmt.Println("  │ macOS 설치         │ 불가                │ 가능                 │")
	fmt.Println("  │ 복구 모드          │ 불가                │ 가능                 │")
	fmt.Println("  │ 안정성             │ 안정                │ 실험적               │")
	fmt.Println("  └────────────────────┴─────────────────────┴──────────────────────┘")

	// === 3. ScreenSharingVNC 시뮬레이션 ===
	fmt.Println("\n=== 3. ScreenSharingVNC ===")

	macAddr := "AA:BB:CC:DD:EE:FF"
	screenVNC := &ScreenSharingVNC{
		macAddress: macAddr,
		dhcpLeases: map[string]string{
			macAddr: "192.168.64.5",
		},
		arpTable: map[string]string{
			macAddr: "10.0.1.50",
		},
	}

	// Shared 네트워크 (DHCP)
	url, err := screenVNC.WaitForURL(false)
	if err != nil {
		fmt.Printf("  [에러] %v\n", err)
	} else {
		fmt.Printf("  Shared 네트워크 (DHCP): %s\n", url)
	}

	// Bridged 네트워크 (ARP)
	url, err = screenVNC.WaitForURL(true)
	if err != nil {
		fmt.Printf("  [에러] %v\n", err)
	} else {
		fmt.Printf("  Bridged 네트워크 (ARP): %s\n", url)
	}

	fmt.Println("  stop() → 아무것도 하지 않음 (VM 내부 서비스에 연결만)")

	// === 4. FullFledgedVNC 시뮬레이션 ===
	fmt.Println("\n=== 4. FullFledgedVNC ===")

	fullVNC := NewFullFledgedVNC()
	fmt.Printf("  생성된 패스프레이즈: %s\n", fullVNC.password)

	url, err = fullVNC.WaitForURL(false)
	if err != nil {
		fmt.Printf("  [에러] %v\n", err)
	} else {
		fmt.Printf("  VNC URL: %s\n", url)
	}

	err = fullVNC.Stop()
	fmt.Printf("  stop() → running=%v, port=%d\n", fullVNC.running, fullVNC.port)

	// === 5. PassphraseGenerator ===
	fmt.Println("\n=== 5. PassphraseGenerator (BIP-39) ===")

	gen := &PassphraseGenerator{words: bip39Words}

	fmt.Printf("  단어 목록 크기: %d (실제: 2048)\n", len(bip39Words))
	fmt.Printf("  보안 강도: 2048^4 = 2^44 ≈ 17.6조 조합 (4단어)\n")

	for i := 0; i < 3; i++ {
		passphrase := strings.Join(gen.Prefix(4), "-")
		fmt.Printf("  패스프레이즈 %d: %s\n", i+1, passphrase)
	}

	// === 6. Serial 콘솔 ===
	fmt.Println("\n=== 6. Serial 콘솔 (PTY) ===")

	pty, err := CreatePTY()
	if err != nil {
		fmt.Printf("  [에러] %v\n", err)
	} else {
		fmt.Printf("  PTY 경로: %s\n", pty.config.SlavePath)
		fmt.Printf("  Master FD: %d\n", pty.config.MasterFD)
		fmt.Printf("  Slave FD: %d (닫힘)\n", pty.config.SlaveFD)
		fmt.Printf("  Baud Rate: %d\n", pty.config.BaudRate)
		fmt.Printf("  Non-blocking: %v\n", pty.config.NonBlock)

		fmt.Println("\n  PTY 생성 단계:")
		fmt.Println("    1. openpty(&tty_fd, &sfd, tty_path, nil, nil)")
		fmt.Println("    2. close(sfd) — Slave FD 닫기")
		fmt.Println("    3. fcntl(tty_fd, F_SETFL, O_NONBLOCK)")
		fmt.Println("    4. cfsetispeed(&termios, B115200)")
		fmt.Println("    5. cfsetospeed(&termios, B115200)")
		fmt.Println("    6. tcsetattr(tty_fd, TCSANOW, &termios)")

		// 데이터 읽기/쓰기 시뮬레이션
		pty.Write([]byte("Hello from VM serial console\n"))
		data := pty.Read()
		fmt.Printf("\n  시리얼 데이터 수신: %s", string(data))

		// non-blocking 읽기 (데이터 없음)
		data = pty.Read()
		fmt.Printf("  non-blocking 읽기 (데이터 없음): %v\n", data)
	}

	// === 7. 터미널 제어 (Term) ===
	fmt.Println("\n=== 7. 터미널 제어 (Term) ===")

	term := Term{}

	fmt.Printf("  IsTerminal(): %v\n", term.IsTerminal())

	state := term.MakeRaw()
	fmt.Println("  MakeRaw() 호출 → Raw 모드 전환")
	fmt.Println("    비활성화된 플래그:")
	fmt.Println("      ECHO   — 입력 에코 끔")
	fmt.Println("      ICANON — 정규 모드 끔 (줄 버퍼링 해제)")
	fmt.Println("      ISIG   — INTR/QUIT/SUSP 시그널 끔")
	fmt.Println("      IEXTEN — 확장 입력 처리 끔")
	fmt.Println("      IXON   — XON/XOFF 흐름 제어 끔")
	fmt.Println("      ICRNL  — CR→NL 변환 끔")
	fmt.Println("      OPOST  — 출력 후처리 끔")

	term.Restore(state)
	fmt.Println("  Restore() 호출 → 원래 상태 복원")

	w, h := term.GetSize()
	fmt.Printf("  GetSize(): %d x %d (cols x rows)\n", w, h)

	// === 8. Utils ===
	fmt.Println("\n=== 8. Utils ===")

	// SafeIndex
	arr := []string{"alpha", "beta", "gamma"}
	if val, ok := SafeIndex(arr, 1); ok {
		fmt.Printf("  SafeIndex([alpha,beta,gamma], 1) = %s\n", val)
	}
	if _, ok := SafeIndex(arr, 5); !ok {
		fmt.Printf("  SafeIndex([alpha,beta,gamma], 5) = nil (범위 밖)\n")
	}

	// ResolveBinaryPath
	for _, bin := range []string{"ls", "go", "nonexistent-binary-xyz"} {
		if path, found := ResolveBinaryPath(bin); found {
			fmt.Printf("  ResolveBinaryPath(%q) = %s\n", bin, path)
		} else {
			fmt.Printf("  ResolveBinaryPath(%q) = (없음)\n", bin)
		}
	}

	// === 9. Run.swift VNC/Serial/Graphics 조합 ===
	fmt.Println("\n=== 9. Run 옵션 조합 ===")

	testCases := []RunOptions{
		{NoGraphics: false, VNCEnabled: false},
		{NoGraphics: true, VNCEnabled: false},
		{NoGraphics: false, VNCEnabled: true},
		{NoGraphics: true, VNCEnabled: true},
		{NoGraphics: false, VNCExperimental: true},
		{NoGraphics: false, VNCEnabled: false, Serial: true},
		{NoGraphics: true, VNCExperimental: true, Serial: true},
	}

	fmt.Println("  ┌──────────┬──────┬──────────┬──────────┬──────────────────────────┬────────────┐")
	fmt.Println("  │no-graph  │ vnc  │ vnc-exp  │ serial   │ 디스플레이               │ 시리얼     │")
	fmt.Println("  ├──────────┼──────┼──────────┼──────────┼──────────────────────────┼────────────┤")

	for _, tc := range testCases {
		mode, err := DetermineRunMode(tc)
		if err != nil {
			fmt.Printf("  │ %-8v │ %-4v │ %-8v │ %-8v │ %-24s │ %-10s │\n",
				tc.NoGraphics, tc.VNCEnabled, tc.VNCExperimental, tc.Serial,
				"ERROR: "+err.Error(), "")
		} else {
			fmt.Printf("  │ %-8v │ %-4v │ %-8v │ %-8v │ %-24s │ %-10s │\n",
				tc.NoGraphics, tc.VNCEnabled, tc.VNCExperimental, tc.Serial,
				mode.DisplayMode, mode.SerialMode)
		}
	}
	fmt.Println("  └──────────┴──────┴──────────┴──────────┴──────────────────────────┴────────────┘")

	// 상호 배타성 검증
	_, err = DetermineRunMode(RunOptions{VNCEnabled: true, VNCExperimental: true})
	if err != nil {
		fmt.Printf("\n  상호 배타성 검증: %v\n", err)
	}

	// === 10. 전체 시나리오 시뮬레이션 ===
	fmt.Println("\n=== 10. 전체 시나리오: tart run vm --vnc-experimental --serial ===")

	fmt.Println("  1. Run 옵션 파싱")
	opts := RunOptions{VNCExperimental: true, Serial: true}
	mode, _ := DetermineRunMode(opts)
	fmt.Printf("     디스플레이: %s, 시리얼: %s\n", mode.DisplayMode, mode.SerialMode)

	fmt.Println("  2. PTY 생성")
	serialPort, _ := CreatePTY()
	fmt.Printf("     PTY: %s (baud: %d)\n", serialPort.config.SlavePath, serialPort.config.BaudRate)

	fmt.Println("  3. FullFledgedVNC 시작")
	vncServer := NewFullFledgedVNC()
	fmt.Printf("     패스프레이즈: %s\n", vncServer.password)

	vncURL, _ := vncServer.WaitForURL(false)
	fmt.Printf("     VNC URL: %s\n", vncURL)

	fmt.Println("  4. VM 실행 중...")
	fmt.Println("     (시뮬레이션: 즉시 종료)")

	fmt.Println("  5. 정리")
	_ = vncServer.Stop()
	fmt.Println("     VNC 서버 종료")
	fmt.Println("     시리얼 포트 해제")

	// === IP 해석 전략 비교 ===
	fmt.Println("\n=== IP 해석 전략 비교 ===")
	fmt.Println("  ┌──────────────────┬──────────────────────────────────────┐")
	fmt.Println("  │ 전략             │ 설명                                 │")
	fmt.Println("  ├──────────────────┼──────────────────────────────────────┤")
	fmt.Println("  │ DHCP             │ /var/db/dhcpd_leases에서 MAC→IP     │")
	fmt.Println("  │                  │ Shared(NAT) 네트워크에서 사용         │")
	fmt.Println("  ├──────────────────┼──────────────────────────────────────┤")
	fmt.Println("  │ ARP              │ ARP 캐시에서 MAC→IP 조회             │")
	fmt.Println("  │                  │ Bridged 네트워크에서 사용             │")
	fmt.Println("  └──────────────────┴──────────────────────────────────────┘")

	// 미사용 import 방지
	_ = net.IPv4(0, 0, 0, 0)
}
