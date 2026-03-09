package main

import (
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium Socket 기반 로드밸런싱 시뮬레이션
//
// 실제 소스: pkg/socketlb/socketlb.go, pkg/socketlb/cgroup.go
//
// 핵심 개념:
// 1. cgroup BPF 훅 (connect, sendmsg, recvmsg, getpeername)
// 2. 소켓 레벨 DNAT (패킷 전송 전 주소 변환)
// 3. UDP/TCP 처리 차이
// 4. NodePort 바인드 보호
// 5. bpf_link vs PROG_ATTACH 부착 방식
// =============================================================================

// --- 서비스/백엔드 정의 ---

type ServiceKey struct {
	IP   net.IP
	Port uint16
}

func (k ServiceKey) String() string {
	return fmt.Sprintf("%s:%d", k.IP, k.Port)
}

type Backend struct {
	IP   net.IP
	Port uint16
}

func (b Backend) String() string {
	return fmt.Sprintf("%s:%d", b.IP, b.Port)
}

type Service struct {
	Key      ServiceKey
	Backends []Backend
}

// --- 서비스 맵 ---

type ServiceMap struct {
	mu       sync.RWMutex
	services map[string]*Service
}

func NewServiceMap() *ServiceMap {
	return &ServiceMap{services: make(map[string]*Service)}
}

func (m *ServiceMap) Add(svc *Service) {
	m.mu.Lock()
	m.services[svc.Key.String()] = svc
	m.mu.Unlock()
}

func (m *ServiceMap) Lookup(key ServiceKey) (*Service, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	svc, ok := m.services[key.String()]
	return svc, ok
}

// --- Socket LB 상태 ---

// SockLBState는 소켓별 LB 상태 (원래 서비스 주소를 기억)
type SockLBState struct {
	OrigService ServiceKey
	Backend     Backend
}

// --- cgroup BPF 프로그램 시뮬레이션 ---

// CgroupProgram은 cgroup BPF 프로그램
type CgroupProgram struct {
	Name     string
	Enabled  bool
	Attached bool
	Mode     string // "bpf_link" or "prog_attach"
}

// SocketLB는 Socket LB 시뮬레이션 (Cilium: pkg/socketlb)
type SocketLB struct {
	mu         sync.Mutex
	services   *ServiceMap
	sockStates map[int]*SockLBState // fd → LB 상태
	programs   map[string]*CgroupProgram

	// 통계
	connects   int
	sendmsgs   int
	recvmsgs   int
	peernames  int
	bindBlocks int

	// 설정
	enableIPv4          bool
	enableIPv6          bool
	enablePeer          bool
	enableBindProtect   bool
	nodePortMin         uint16
	nodePortMax         uint16
}

func NewSocketLB(services *ServiceMap) *SocketLB {
	slb := &SocketLB{
		services:          services,
		sockStates:        make(map[int]*SockLBState),
		programs:          make(map[string]*CgroupProgram),
		enableIPv4:        true,
		enableIPv6:        true,
		enablePeer:        true,
		enableBindProtect: true,
		nodePortMin:       30000,
		nodePortMax:       32767,
	}

	// 13개 cgroup BPF 프로그램 정의 (Cilium: socketlb.go:27-39)
	progNames := []string{
		"cil_sock4_connect", "cil_sock4_sendmsg", "cil_sock4_recvmsg",
		"cil_sock4_getpeername", "cil_sock4_post_bind", "cil_sock4_pre_bind",
		"cil_sock6_connect", "cil_sock6_sendmsg", "cil_sock6_recvmsg",
		"cil_sock6_getpeername", "cil_sock6_post_bind", "cil_sock6_pre_bind",
		"cil_sock_release",
	}

	for _, name := range progNames {
		slb.programs[name] = &CgroupProgram{Name: name, Enabled: false}
	}

	return slb
}

// Enable은 설정에 따라 프로그램 활성화 (Cilium: socketlb.Enable)
func (slb *SocketLB) Enable() {
	if slb.enableIPv4 {
		slb.programs["cil_sock4_connect"].Enabled = true
		slb.programs["cil_sock4_sendmsg"].Enabled = true
		slb.programs["cil_sock4_recvmsg"].Enabled = true

		if slb.enablePeer {
			slb.programs["cil_sock4_getpeername"].Enabled = true
		}
		if slb.enableBindProtect {
			slb.programs["cil_sock4_post_bind"].Enabled = true
		}
	}

	if slb.enableIPv6 {
		slb.programs["cil_sock6_connect"].Enabled = true
		slb.programs["cil_sock6_sendmsg"].Enabled = true
		slb.programs["cil_sock6_recvmsg"].Enabled = true

		if slb.enablePeer {
			slb.programs["cil_sock6_getpeername"].Enabled = true
		}
		if slb.enableBindProtect {
			slb.programs["cil_sock6_post_bind"].Enabled = true
		}
	}

	if slb.enableIPv4 || slb.enableIPv6 {
		slb.programs["cil_sock_release"].Enabled = true
	}

	// cgroup에 부착 시뮬레이션
	for _, prog := range slb.programs {
		if prog.Enabled {
			prog.Attached = true
			prog.Mode = "bpf_link" // 최신 커널 가정
		}
	}
}

// Connect는 connect() 시스템 콜 훅 (cil_sock4_connect)
func (slb *SocketLB) Connect(fd int, dstIP net.IP, dstPort uint16) (net.IP, uint16, error) {
	slb.mu.Lock()
	defer slb.mu.Unlock()
	slb.connects++

	key := ServiceKey{IP: dstIP, Port: dstPort}
	svc, found := slb.services.Lookup(key)
	if !found {
		// 서비스가 아님 → 원래 주소로 연결
		return dstIP, dstPort, nil
	}

	if len(svc.Backends) == 0 {
		return nil, 0, fmt.Errorf("no backends for service %s", key)
	}

	// 백엔드 선택 (실제: Maglev 해시)
	backend := svc.Backends[rand.Intn(len(svc.Backends))]

	// LB 상태 저장 (getpeername 역변환용)
	slb.sockStates[fd] = &SockLBState{
		OrigService: key,
		Backend:     backend,
	}

	return backend.IP, backend.Port, nil
}

// SendMsg는 sendmsg() 시스템 콜 훅 (cil_sock4_sendmsg) - UDP
func (slb *SocketLB) SendMsg(fd int, dstIP net.IP, dstPort uint16) (net.IP, uint16, error) {
	slb.mu.Lock()
	defer slb.mu.Unlock()
	slb.sendmsgs++

	key := ServiceKey{IP: dstIP, Port: dstPort}
	svc, found := slb.services.Lookup(key)
	if !found {
		return dstIP, dstPort, nil
	}

	if len(svc.Backends) == 0 {
		return nil, 0, fmt.Errorf("no backends for service %s", key)
	}

	backend := svc.Backends[rand.Intn(len(svc.Backends))]

	// UDP는 매 패킷마다 변환 (연결 상태 없음)
	return backend.IP, backend.Port, nil
}

// RecvMsg는 recvmsg() 시스템 콜 훅 (cil_sock4_recvmsg) - UDP 역변환
func (slb *SocketLB) RecvMsg(srcIP net.IP, srcPort uint16, origService *ServiceKey) (net.IP, uint16) {
	slb.mu.Lock()
	slb.recvmsgs++
	slb.mu.Unlock()

	if origService != nil {
		// 백엔드 IP → 서비스 VIP로 역변환
		return origService.IP, origService.Port
	}
	return srcIP, srcPort
}

// GetPeerName은 getpeername() 시스템 콜 훅 (cil_sock4_getpeername)
func (slb *SocketLB) GetPeerName(fd int) (net.IP, uint16) {
	slb.mu.Lock()
	defer slb.mu.Unlock()
	slb.peernames++

	state, ok := slb.sockStates[fd]
	if !ok {
		return nil, 0
	}

	// 실제 백엔드 대신 원래 서비스 VIP 반환
	return state.OrigService.IP, state.OrigService.Port
}

// PostBind는 bind() 후 훅 (cil_sock4_post_bind) - NodePort 보호
func (slb *SocketLB) PostBind(port uint16) error {
	slb.mu.Lock()
	defer slb.mu.Unlock()

	if port >= slb.nodePortMin && port <= slb.nodePortMax {
		slb.bindBlocks++
		return fmt.Errorf("port %d is in NodePort range [%d-%d], bind blocked",
			port, slb.nodePortMin, slb.nodePortMax)
	}
	return nil
}

// Release는 소켓 해제 훅 (cil_sock_release)
func (slb *SocketLB) Release(fd int) {
	slb.mu.Lock()
	delete(slb.sockStates, fd)
	slb.mu.Unlock()
}

func main() {
	fmt.Println("=" + strings.Repeat("=", 70))
	fmt.Println(" Cilium Socket 기반 로드밸런싱 시뮬레이션")
	fmt.Println(" 소스: pkg/socketlb/socketlb.go, pkg/socketlb/cgroup.go")
	fmt.Println("=" + strings.Repeat("=", 70))

	// 서비스 맵 설정
	svcMap := NewServiceMap()
	svcMap.Add(&Service{
		Key: ServiceKey{IP: net.ParseIP("10.96.0.1"), Port: 80},
		Backends: []Backend{
			{IP: net.ParseIP("10.244.1.5"), Port: 8080},
			{IP: net.ParseIP("10.244.2.10"), Port: 8080},
			{IP: net.ParseIP("10.244.3.15"), Port: 8080},
		},
	})
	svcMap.Add(&Service{
		Key: ServiceKey{IP: net.ParseIP("10.96.0.10"), Port: 53},
		Backends: []Backend{
			{IP: net.ParseIP("10.244.0.100"), Port: 53},
			{IP: net.ParseIP("10.244.0.101"), Port: 53},
		},
	})

	slb := NewSocketLB(svcMap)

	// --- 1. 프로그램 활성화 ---
	fmt.Println("\n[1] cgroup BPF 프로그램 활성화")
	fmt.Println(strings.Repeat("-", 50))

	slb.Enable()

	for _, prog := range slb.programs {
		status := "disabled"
		if prog.Enabled {
			status = fmt.Sprintf("enabled (%s)", prog.Mode)
		}
		fmt.Printf("  %-30s %s\n", prog.Name, status)
	}

	// --- 2. TCP connect() 변환 ---
	fmt.Println("\n[2] TCP connect() - 서비스 VIP → 백엔드 변환")
	fmt.Println(strings.Repeat("-", 50))

	for fd := 100; fd < 105; fd++ {
		backendIP, backendPort, err := slb.Connect(fd, net.ParseIP("10.96.0.1"), 80)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			continue
		}
		fmt.Printf("  fd=%d: connect(10.96.0.1:80) → connect(%s:%d)\n",
			fd, backendIP, backendPort)
	}

	// 서비스가 아닌 주소로 연결
	backendIP, backendPort, _ := slb.Connect(200, net.ParseIP("8.8.8.8"), 443)
	fmt.Printf("  fd=200: connect(8.8.8.8:443) → connect(%s:%d) (서비스 아님, 변환 없음)\n",
		backendIP, backendPort)

	// --- 3. getpeername() 역변환 ---
	fmt.Println("\n[3] getpeername() - 백엔드 IP → 서비스 VIP 역변환")
	fmt.Println(strings.Repeat("-", 50))

	for fd := 100; fd < 105; fd++ {
		peerIP, peerPort := slb.GetPeerName(fd)
		fmt.Printf("  fd=%d: getpeername() → %s:%d (원래 서비스 VIP)\n",
			fd, peerIP, peerPort)
	}

	// --- 4. UDP sendmsg/recvmsg ---
	fmt.Println("\n[4] UDP sendmsg/recvmsg - 매 패킷 변환")
	fmt.Println(strings.Repeat("-", 50))

	dnsService := ServiceKey{IP: net.ParseIP("10.96.0.10"), Port: 53}

	for i := 0; i < 3; i++ {
		// sendmsg: 서비스 VIP → 백엔드
		backendIP, backendPort, _ := slb.SendMsg(300+i, net.ParseIP("10.96.0.10"), 53)
		fmt.Printf("  sendmsg #%d: 10.96.0.10:53 → %s:%d\n", i+1, backendIP, backendPort)

		// recvmsg: 백엔드 → 서비스 VIP (역변환)
		srcIP, srcPort := slb.RecvMsg(backendIP, backendPort, &dnsService)
		fmt.Printf("  recvmsg #%d: %s:%d → %s:%d (앱이 보는 소스)\n",
			i+1, backendIP, backendPort, srcIP, srcPort)
	}

	// --- 5. NodePort 바인드 보호 ---
	fmt.Println("\n[5] NodePort 바인드 보호 (post_bind)")
	fmt.Println(strings.Repeat("-", 50))

	testPorts := []uint16{8080, 30080, 31000, 32767, 9090, 30000}
	for _, port := range testPorts {
		err := slb.PostBind(port)
		if err != nil {
			fmt.Printf("  bind(:%d) → BLOCKED: %v\n", port, err)
		} else {
			fmt.Printf("  bind(:%d) → ALLOWED\n", port)
		}
	}

	// --- 6. 소켓 해제 ---
	fmt.Println("\n[6] 소켓 해제 (sock_release)")
	fmt.Println(strings.Repeat("-", 50))

	fmt.Printf("  해제 전 LB 상태 수: %d\n", len(slb.sockStates))
	for fd := 100; fd < 105; fd++ {
		slb.Release(fd)
	}
	fmt.Printf("  해제 후 LB 상태 수: %d\n", len(slb.sockStates))

	// --- 7. 성능 비교 ---
	fmt.Println("\n[7] 성능 비교: Socket LB vs iptables 시뮬레이션")
	fmt.Println(strings.Repeat("-", 50))

	iterations := 500000

	// Socket LB
	start := time.Now()
	for i := 0; i < iterations; i++ {
		slb.Connect(i+1000, net.ParseIP("10.96.0.1"), 80)
	}
	socketDuration := time.Since(start)

	// iptables 시뮬레이션 (추가 오버헤드: netfilter 통과 + CT 생성)
	start = time.Now()
	for i := 0; i < iterations; i++ {
		slb.Connect(i+1000, net.ParseIP("10.96.0.1"), 80)
		// iptables는 추가로: CT lookup + DNAT + CT create
		_ = i * 3 // 의미 없는 연산으로 추가 비용 시뮬레이션
	}
	iptablesDuration := time.Since(start)

	fmt.Printf("  Socket LB:  %d ops / %v\n", iterations, socketDuration)
	fmt.Printf("  iptables:   %d ops / %v (CT + DNAT 오버헤드)\n", iterations, iptablesDuration)

	// --- 통계 ---
	fmt.Println("\n[통계]")
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("  connect() 호출:     %d\n", slb.connects)
	fmt.Printf("  sendmsg() 호출:     %d\n", slb.sendmsgs)
	fmt.Printf("  recvmsg() 호출:     %d\n", slb.recvmsgs)
	fmt.Printf("  getpeername() 호출: %d\n", slb.peernames)
	fmt.Printf("  bind() 차단:        %d\n", slb.bindBlocks)

	// --- 요약 ---
	fmt.Println("\n" + strings.Repeat("=", 71))
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println()
	fmt.Println(" Socket LB 핵심 동작:")
	fmt.Println("   1. cgroup BPF로 소켓 시스템 콜을 훅")
	fmt.Println("   2. connect()에서 서비스 VIP → 백엔드 IP 변환")
	fmt.Println("   3. UDP: sendmsg()/recvmsg()에서 매 패킷 변환")
	fmt.Println("   4. getpeername()으로 원래 서비스 주소 복원")
	fmt.Println("   5. NodePort 포트 범위 바인드 보호")
	fmt.Println("   6. ConnTrack/iptables DNAT 불필요 → 성능 향상")
	fmt.Println(strings.Repeat("=", 71))
}
