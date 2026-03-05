package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Shared/Bridged/Softnet 네트워크 추상화 시뮬레이션
//
// tart의 Network/ 디렉토리 구현을 Go로 재현한다.
//
// 핵심 개념:
//   - Network 프로토콜: Attachments(), Run(), Stop() 인터페이스
//   - NetworkShared: NAT 기반 네트워크 (VZNATNetworkDeviceAttachment)
//   - NetworkBridged: 브리지 인터페이스 기반 네트워크
//   - Softnet: socketpair + 외부 프로세스를 통한 네트워킹 (net.Pipe 시뮬레이션)
//
// 참조: tart/Sources/tart/Network/Network.swift
//       tart/Sources/tart/Network/NetworkShared.swift
//       tart/Sources/tart/Network/NetworkBridged.swift
//       tart/Sources/tart/Network/Softnet.swift
// =============================================================================

// --- 이더넷 프레임 시뮬레이션 ---

// EthernetFrame은 간소화된 이더넷 프레임이다.
// 실제 이더넷: Dst MAC(6) + Src MAC(6) + EtherType(2) + Payload + FCS(4)
type EthernetFrame struct {
	DstMAC    string // 목적지 MAC 주소
	SrcMAC    string // 출발지 MAC 주소
	EtherType uint16 // 0x0800=IPv4, 0x0806=ARP
	Payload   []byte // 패킷 데이터
}

// Serialize는 프레임을 바이트 슬라이스로 직렬화한다.
func (f *EthernetFrame) Serialize() []byte {
	var data []byte
	data = append(data, []byte(f.DstMAC)...)
	data = append(data, []byte(f.SrcMAC)...)
	etherType := make([]byte, 2)
	binary.BigEndian.PutUint16(etherType, f.EtherType)
	data = append(data, etherType...)
	data = append(data, f.Payload...)
	return data
}

func (f *EthernetFrame) String() string {
	typeStr := "Unknown"
	switch f.EtherType {
	case 0x0800:
		typeStr = "IPv4"
	case 0x0806:
		typeStr = "ARP"
	}
	return fmt.Sprintf("[%s->%s type=%s payload=%dB]",
		f.SrcMAC, f.DstMAC, typeStr, len(f.Payload))
}

// --- Network 인터페이스 (tart Network.swift 참조) ---

// NetworkAttachment는 VM에 연결되는 네트워크 디바이스 추상화이다.
// tart의 VZNetworkDeviceAttachment에 대응.
type NetworkAttachment struct {
	Name string
	Type string // "NAT", "Bridged", "FileHandle"
}

// Network는 tart의 Network 프로토콜을 Go 인터페이스로 재현한다.
// tart: protocol Network { func attachments(); func run(); func stop() }
type Network interface {
	Attachments() []NetworkAttachment
	Run(done chan struct{}) error
	Stop() error
	SendPacket(frame *EthernetFrame) error
	ReceivePacket() (*EthernetFrame, error)
}

// --- NetworkShared: NAT 시뮬레이션 (tart NetworkShared.swift 참조) ---

// NetworkShared는 NAT(Network Address Translation) 기반 공유 네트워크이다.
// tart의 NetworkShared 클래스: VZNATNetworkDeviceAttachment 사용.
// Run()과 Stop()은 no-op (Softnet에서만 사용).
//
// 시뮬레이션:
//   - VM에게 내부 IP(192.168.64.x) 할당
//   - 외부로 나갈 때 호스트 IP(10.0.0.1)로 NAT 변환
//   - 외부에서 들어올 때 역변환
type NetworkShared struct {
	mu      sync.Mutex
	vmMAC   string
	vmIP    string // 내부 IP (NAT 뒤)
	hostIP  string // 호스트/외부 IP
	packets []*EthernetFrame
	running bool
}

// NewNetworkShared는 NAT 네트워크를 생성한다.
func NewNetworkShared(vmMAC string) *NetworkShared {
	return &NetworkShared{
		vmMAC:  vmMAC,
		vmIP:   fmt.Sprintf("192.168.64.%d", rand.Intn(200)+2),
		hostIP: "10.0.0.1",
	}
}

// Attachments는 NAT 네트워크 어태치먼트를 반환한다.
// tart: func attachments() -> [VZNetworkDeviceAttachment] { [VZNATNetworkDeviceAttachment()] }
func (n *NetworkShared) Attachments() []NetworkAttachment {
	return []NetworkAttachment{
		{Name: "shared-nat", Type: "NAT"},
	}
}

// Run은 no-op이다.
// tart: func run(_ sema: AsyncSemaphore) throws { /* no-op */ }
func (n *NetworkShared) Run(done chan struct{}) error {
	n.mu.Lock()
	n.running = true
	n.mu.Unlock()
	// SharedNetwork에서는 별도 goroutine 불필요 (Softnet에서만 사용)
	return nil
}

// Stop은 no-op이다.
// tart: func stop() async throws { /* no-op */ }
func (n *NetworkShared) Stop() error {
	n.mu.Lock()
	n.running = false
	n.mu.Unlock()
	return nil
}

// SendPacket은 VM에서 외부로 패킷을 전송한다 (NAT 변환 포함).
func (n *NetworkShared) SendPacket(frame *EthernetFrame) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	// NAT 변환: 내부 IP -> 호스트 IP (실제로는 소스 IP 변경)
	natFrame := &EthernetFrame{
		DstMAC:    frame.DstMAC,
		SrcMAC:    n.vmMAC,
		EtherType: frame.EtherType,
		Payload:   append([]byte(fmt.Sprintf("[NAT %s->%s] ", n.vmIP, n.hostIP)), frame.Payload...),
	}
	n.packets = append(n.packets, natFrame)
	return nil
}

// ReceivePacket은 외부에서 VM으로 패킷을 수신한다.
func (n *NetworkShared) ReceivePacket() (*EthernetFrame, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.packets) == 0 {
		return nil, fmt.Errorf("수신할 패킷 없음")
	}
	frame := n.packets[0]
	n.packets = n.packets[1:]
	return frame, nil
}

// --- NetworkBridged: 브리지 시뮬레이션 (tart NetworkBridged.swift 참조) ---

// NetworkBridged는 호스트의 물리적 네트워크 인터페이스에 브리지 연결한다.
// tart의 NetworkBridged 클래스: VZBridgedNetworkDeviceAttachment 사용.
// VM이 호스트와 동일한 L2 네트워크에 직접 참여.
type NetworkBridged struct {
	mu         sync.Mutex
	interfaces []string // 브리지할 인터페이스 이름 목록
	vmMAC      string
	packets    []*EthernetFrame
	running    bool
}

// NewNetworkBridged는 브리지 네트워크를 생성한다.
// tart: init(interfaces: [VZBridgedNetworkInterface])
func NewNetworkBridged(vmMAC string, interfaces []string) *NetworkBridged {
	return &NetworkBridged{
		interfaces: interfaces,
		vmMAC:      vmMAC,
	}
}

// Attachments는 각 인터페이스에 대한 브리지 어태치먼트를 반환한다.
// tart: interfaces.map { VZBridgedNetworkDeviceAttachment(interface: $0) }
func (n *NetworkBridged) Attachments() []NetworkAttachment {
	var attachments []NetworkAttachment
	for _, iface := range n.interfaces {
		attachments = append(attachments, NetworkAttachment{
			Name: iface,
			Type: "Bridged",
		})
	}
	return attachments
}

// Run은 no-op이다 (tart: Softnet에서만 사용).
func (n *NetworkBridged) Run(done chan struct{}) error {
	n.mu.Lock()
	n.running = true
	n.mu.Unlock()
	return nil
}

// Stop은 no-op이다.
func (n *NetworkBridged) Stop() error {
	n.mu.Lock()
	n.running = false
	n.mu.Unlock()
	return nil
}

// SendPacket은 브리지를 통해 패킷을 전송한다 (L2 직접 전달).
func (n *NetworkBridged) SendPacket(frame *EthernetFrame) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	// 브리지: NAT 변환 없이 원본 프레임 그대로 전달
	bridgedFrame := &EthernetFrame{
		DstMAC:    frame.DstMAC,
		SrcMAC:    n.vmMAC,
		EtherType: frame.EtherType,
		Payload:   append([]byte("[Bridged] "), frame.Payload...),
	}
	n.packets = append(n.packets, bridgedFrame)
	return nil
}

// ReceivePacket은 브리지를 통해 패킷을 수신한다.
func (n *NetworkBridged) ReceivePacket() (*EthernetFrame, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.packets) == 0 {
		return nil, fmt.Errorf("수신할 패킷 없음")
	}
	frame := n.packets[0]
	n.packets = n.packets[1:]
	return frame, nil
}

// --- Softnet: 소켓 페어 기반 네트워킹 (tart Softnet.swift 참조) ---

// Softnet은 socketpair(AF_UNIX, SOCK_DGRAM)를 사용하는 네트워크이다.
// tart의 Softnet 클래스를 재현:
//   - socketpair 대신 net.Pipe() 사용 (Go에서 유닉스 소켓 페어 대용)
//   - VM 측 FD와 Softnet 프로세스 측 FD
//   - goroutine으로 패킷 포워딩 (tart: Process + monitorTask)
//   - 버퍼 크기 설정 (tart: setSocketBuffers SO_RCVBUF/SO_SNDBUF)
//
// 실제 tart에서는 외부 'softnet' 바이너리를 Process로 실행하지만,
// 여기서는 goroutine으로 시뮬레이션한다.
type Softnet struct {
	mu             sync.Mutex
	vmConn         net.Conn     // VM 측 소켓 (tart: vmFD)
	softnetConn    net.Conn     // Softnet 프로세스 측 소켓 (tart: softnetFD)
	vmMAC          string
	running        bool
	monitorDone    chan struct{} // tart: monitorTask 완료 시그널
	stopCh         chan struct{} // 종료 시그널
	receivedFrames []*EthernetFrame
}

// NewSoftnet은 Softnet 네트워크를 생성한다.
// tart: init(vmMACAddress:extraArguments:)
//   - socketpair(AF_UNIX, SOCK_DGRAM, 0, fds)
//   - vmFD = fds[0], softnetFD = fds[1]
//   - setSocketBuffers(vmFD, 1MB), setSocketBuffers(softnetFD, 1MB)
func NewSoftnet(vmMAC string) (*Softnet, error) {
	// net.Pipe(): tart의 socketpair(AF_UNIX, SOCK_DGRAM) 대용
	// 양방향 동기 인메모리 연결
	vmConn, softnetConn := net.Pipe()

	return &Softnet{
		vmConn:      vmConn,
		softnetConn: softnetConn,
		vmMAC:       vmMAC,
		monitorDone: make(chan struct{}),
		stopCh:      make(chan struct{}),
	}, nil
}

// Attachments는 FileHandle 기반 어태치먼트를 반환한다.
// tart: VZFileHandleNetworkDeviceAttachment(fileHandle: FileHandle(fileDescriptor: vmFD))
func (s *Softnet) Attachments() []NetworkAttachment {
	return []NetworkAttachment{
		{Name: "softnet-pipe", Type: "FileHandle"},
	}
}

// Run은 Softnet 프로세스(goroutine)를 시작한다.
// tart: try process.run() -> monitorTask = Task { process.waitUntilExit(); sema.signal() }
//
// 실제 tart에서는 외부 softnet 바이너리가 vmFD에서 패킷을 읽어
// 호스트 네트워크로 전달하지만, 여기서는 goroutine이 softnetConn에서
// 읽어 내부 버퍼에 저장하는 방식으로 시뮬레이션.
func (s *Softnet) Run(done chan struct{}) error {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	// Softnet 프로세스 시뮬레이션 (패킷 포워딩 goroutine)
	go func() {
		defer close(s.monitorDone)

		buf := make([]byte, 65536) // 최대 프레임 크기
		for {
			select {
			case <-s.stopCh:
				return
			default:
			}

			// softnetConn에서 패킷 읽기 (VM -> Softnet)
			s.softnetConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, err := s.softnetConn.Read(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue // 타임아웃은 정상 (non-blocking 폴링)
				}
				return
			}

			// 패킷 수신 및 저장
			frame := &EthernetFrame{
				DstMAC:    "host",
				SrcMAC:    s.vmMAC,
				EtherType: 0x0800,
				Payload:   make([]byte, n),
			}
			copy(frame.Payload, buf[:n])

			s.mu.Lock()
			s.receivedFrames = append(s.receivedFrames, frame)
			s.mu.Unlock()
		}
	}()

	return nil
}

// Stop은 Softnet 프로세스를 종료한다.
// tart: process.interrupt() -> monitorTask?.value
func (s *Softnet) Stop() error {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	close(s.stopCh)

	// monitorTask 완료 대기 (tart: _ = try await monitorTask?.value)
	<-s.monitorDone

	s.vmConn.Close()
	s.softnetConn.Close()

	return nil
}

// SendPacket은 VM에서 Softnet 프로세스로 패킷을 전송한다.
// tart에서는 VZFileHandleNetworkDeviceAttachment가 vmFD를 통해 자동 전송.
func (s *Softnet) SendPacket(frame *EthernetFrame) error {
	data := frame.Serialize()
	_, err := s.vmConn.Write(data)
	return err
}

// ReceivePacket은 Softnet 프로세스가 수신한 패킷을 반환한다.
func (s *Softnet) ReceivePacket() (*EthernetFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.receivedFrames) == 0 {
		return nil, fmt.Errorf("수신할 패킷 없음")
	}
	frame := s.receivedFrames[0]
	s.receivedFrames = s.receivedFrames[1:]
	return frame, nil
}

// --- 패킷 라우팅 데모 ---

// simulatePacketFlow는 네트워크를 통한 패킷 전송/수신을 시연한다.
func simulatePacketFlow(name string, nw Network) {
	fmt.Printf("--- %s 패킷 전달 데모 ---\n", name)

	// 네트워크 시작
	done := make(chan struct{})
	if err := nw.Run(done); err != nil {
		fmt.Printf("  네트워크 시작 실패: %v\n", err)
		return
	}

	// VM -> 호스트 패킷 전송
	frame1 := &EthernetFrame{
		DstMAC:    "AA:BB:CC:DD:EE:01",
		SrcMAC:    "AA:BB:CC:DD:EE:02",
		EtherType: 0x0800, // IPv4
		Payload:   []byte("Hello from VM!"),
	}
	fmt.Printf("  [VM->Host] 전송: %s\n", frame1)
	if err := nw.SendPacket(frame1); err != nil {
		fmt.Printf("  전송 실패: %v\n", err)
	}

	// ARP 패킷 전송
	frame2 := &EthernetFrame{
		DstMAC:    "FF:FF:FF:FF:FF:FF",
		SrcMAC:    "AA:BB:CC:DD:EE:02",
		EtherType: 0x0806, // ARP
		Payload:   []byte("Who has 192.168.64.1?"),
	}
	fmt.Printf("  [VM->Host] ARP 전송: %s\n", frame2)
	nw.SendPacket(frame2)

	// Softnet은 goroutine으로 비동기 수신하므로 잠시 대기
	time.Sleep(200 * time.Millisecond)

	// 수신된 패킷 확인
	for i := 0; i < 2; i++ {
		frame, err := nw.ReceivePacket()
		if err != nil {
			fmt.Printf("  [Host<-VM] 수신 대기 중: %v\n", err)
			break
		}
		fmt.Printf("  [Host<-VM] 수신: %s payload=%q\n", frame, string(frame.Payload))
	}

	// 네트워크 종료
	if err := nw.Stop(); err != nil {
		fmt.Printf("  네트워크 종료 실패: %v\n", err)
	}
	fmt.Println()
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== Shared/Bridged/Softnet 네트워크 추상화 시뮬레이션 ===")
	fmt.Println("(tart Network/Network.swift, NetworkShared.swift, NetworkBridged.swift, Softnet.swift 기반)")
	fmt.Println()

	// --- 1. Network 인터페이스 소개 ---
	fmt.Println("========== 1. Network 인터페이스 ==========")
	fmt.Println()
	fmt.Println("tart의 Network 프로토콜 (Network.swift):")
	fmt.Println("  protocol Network {")
	fmt.Println("    func attachments() -> [VZNetworkDeviceAttachment]")
	fmt.Println("    func run(_ sema: AsyncSemaphore) throws")
	fmt.Println("    func stop() async throws")
	fmt.Println("  }")
	fmt.Println()
	fmt.Println("Go 인터페이스로 재현:")
	fmt.Println("  type Network interface {")
	fmt.Println("    Attachments() []NetworkAttachment")
	fmt.Println("    Run(done chan struct{}) error")
	fmt.Println("    Stop() error")
	fmt.Println("    SendPacket(frame *EthernetFrame) error")
	fmt.Println("    ReceivePacket() (*EthernetFrame, error)")
	fmt.Println("  }")
	fmt.Println()

	// --- 2. NetworkShared (NAT) ---
	fmt.Println("========== 2. NetworkShared (NAT 네트워크) ==========")
	fmt.Println()
	fmt.Println("tart: VZNATNetworkDeviceAttachment() -- macOS 내장 NAT")
	fmt.Println("VM은 192.168.64.x 내부 IP를 받고, 외부 통신 시 호스트 IP로 변환됨")
	fmt.Println()

	sharedNet := NewNetworkShared("AA:BB:CC:DD:EE:10")
	fmt.Println("[Shared] 어태치먼트:")
	for _, a := range sharedNet.Attachments() {
		fmt.Printf("  - Name: %s, Type: %s\n", a.Name, a.Type)
	}
	fmt.Printf("[Shared] VM MAC: %s\n", sharedNet.vmMAC)
	fmt.Printf("[Shared] VM 내부 IP: %s (NAT)\n", sharedNet.vmIP)
	fmt.Printf("[Shared] 호스트 IP: %s\n\n", sharedNet.hostIP)

	simulatePacketFlow("SharedNetwork", sharedNet)

	// --- 3. NetworkBridged ---
	fmt.Println("========== 3. NetworkBridged (브리지 네트워크) ==========")
	fmt.Println()
	fmt.Println("tart: VZBridgedNetworkDeviceAttachment -- 호스트 인터페이스에 브리지")
	fmt.Println("VM이 호스트와 동일한 L2 네트워크에 직접 참여 (NAT 없음)")
	fmt.Println()

	bridgedNet := NewNetworkBridged("AA:BB:CC:DD:EE:20", []string{"en0", "en1"})
	fmt.Println("[Bridged] 어태치먼트:")
	for _, a := range bridgedNet.Attachments() {
		fmt.Printf("  - Name: %s, Type: %s\n", a.Name, a.Type)
	}
	fmt.Println()

	simulatePacketFlow("BridgedNetwork", bridgedNet)

	// --- 4. Softnet ---
	fmt.Println("========== 4. Softnet (소켓 페어 네트워크) ==========")
	fmt.Println()
	fmt.Println("tart: socketpair(AF_UNIX, SOCK_DGRAM) + 외부 softnet 프로세스")
	fmt.Println("시뮬레이션: net.Pipe() + goroutine으로 패킷 포워딩")
	fmt.Println()
	fmt.Println("tart Softnet 초기화 흐름:")
	fmt.Println("  1) socketpair(AF_UNIX, SOCK_DGRAM, 0, fds)")
	fmt.Println("  2) vmFD = fds[0], softnetFD = fds[1]")
	fmt.Println("  3) setSocketBuffers(vmFD, 1MB)  -- SO_RCVBUF = 4MB, SO_SNDBUF = 1MB")
	fmt.Println("  4) setSocketBuffers(softnetFD, 1MB)")
	fmt.Println("  5) Process.run() -- softnet --vm-fd STDIN --vm-mac-address ...")
	fmt.Println("  6) monitorTask: process.waitUntilExit() -> sema.signal()")
	fmt.Println()

	softnet, err := NewSoftnet("AA:BB:CC:DD:EE:30")
	if err != nil {
		fmt.Printf("Softnet 생성 실패: %v\n", err)
		return
	}
	fmt.Println("[Softnet] 어태치먼트:")
	for _, a := range softnet.Attachments() {
		fmt.Printf("  - Name: %s, Type: %s\n", a.Name, a.Type)
	}
	fmt.Println()

	simulatePacketFlow("Softnet", softnet)

	// --- 5. 네트워크 타입 비교 ---
	fmt.Println("========== 5. 네트워크 타입 비교 ==========")
	fmt.Println()
	fmt.Println("+----------------+-------------------+--------------------+--------------------+")
	fmt.Println("|                | SharedNetwork     | BridgedNetwork     | Softnet            |")
	fmt.Println("+----------------+-------------------+--------------------+--------------------+")
	fmt.Println("| tart 클래스    | NetworkShared     | NetworkBridged     | Softnet            |")
	fmt.Println("| Attachment     | VZNAT...          | VZBridged...       | VZFileHandle...    |")
	fmt.Println("| IP 할당        | NAT(192.168.64.x) | DHCP(물리 네트워크)| Softnet 관리       |")
	fmt.Println("| L2 접근        | 격리됨            | 호스트와 동일      | 소켓 페어          |")
	fmt.Println("| Run/Stop       | no-op             | no-op              | Process 관리       |")
	fmt.Println("| 권한           | 불필요            | 불필요             | SUID 필요          |")
	fmt.Println("| 사용 사례      | 기본(인터넷 접근) | 동일 서브넷 필요   | 고급 네트워킹      |")
	fmt.Println("+----------------+-------------------+--------------------+--------------------+")
	fmt.Println()

	// --- 6. 다형성 데모 ---
	fmt.Println("========== 6. 다형성 데모 (Network 인터페이스) ==========")
	fmt.Println()

	networks := map[string]Network{
		"shared":  NewNetworkShared("AA:BB:CC:00:00:01"),
		"bridged": NewNetworkBridged("AA:BB:CC:00:00:02", []string{"en0"}),
	}
	softnet2, _ := NewSoftnet("AA:BB:CC:00:00:03")
	networks["softnet"] = softnet2

	for name, nw := range networks {
		fmt.Printf("[%s] 어태치먼트: ", name)
		attachments := nw.Attachments()
		names := make([]string, len(attachments))
		for i, a := range attachments {
			names[i] = fmt.Sprintf("%s(%s)", a.Name, a.Type)
		}
		fmt.Printf("%s\n", strings.Join(names, ", "))
	}
	fmt.Println()

	// Softnet 정리
	softnet2.Run(make(chan struct{}))
	softnet2.Stop()

	// --- 7. Softnet SUID 설정 흐름 ---
	fmt.Println("========== 7. Softnet SUID 설정 흐름 ==========")
	fmt.Println()
	fmt.Println("tart Softnet.configureSUIDBitIfNeeded() 흐름:")
	fmt.Println("  1) softnet 바이너리 경로 resolve (symlink 해제)")
	fmt.Println("     예: /opt/homebrew/bin/softnet -> /opt/homebrew/Cellar/softnet/0.6.2/bin/softnet")
	fmt.Println("  2) 이미 SUID 설정 확인: owner=root && S_ISUID 비트")
	fmt.Println("     -> 설정됨: 종료")
	fmt.Println("  3) passwordless sudo 확인: sudo --non-interactive softnet --help")
	fmt.Println("     -> 성공: 종료")
	fmt.Println("  4) 사용자에게 sudo 비밀번호 요청")
	fmt.Println("     sudo sh -c 'chown root {path} && chmod u+s {path}'")
	fmt.Println("  5) tcsetpgrp(STDIN, process.pid) -- TTY 포그라운드 그룹 설정")
	fmt.Println()

	// --- 요약 ---
	fmt.Println("========== 요약 ==========")
	fmt.Println()
	fmt.Println("tart의 Network 추상화 계층:")
	fmt.Println("  - Network 프로토콜: 3가지 구현체가 동일 인터페이스 제공")
	fmt.Println("  - SharedNetwork: macOS 내장 NAT (기본값, 설정 불필요)")
	fmt.Println("  - BridgedNetwork: 물리 인터페이스 브리지 (동일 서브넷)")
	fmt.Println("  - Softnet: socketpair + 외부 프로세스 (고급 네트워킹)")
	fmt.Println()
	fmt.Println("핵심 설계 패턴:")
	fmt.Println("  - 전략 패턴: Network 프로토콜 -> 런타임에 구현체 교체")
	fmt.Println("  - Run/Stop은 Softnet에서만 실제 동작 (SharedNetwork/BridgedNetwork은 no-op)")
	fmt.Println("  - Softnet은 socketpair로 VM<->프로세스 간 제로카피 통신")
	fmt.Println()
	fmt.Println("[완료] 네트워크 추상화 시뮬레이션 성공")
}
