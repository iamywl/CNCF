// Package main은 gRPC-Go의 Admin API(Channelz + CSDS 통합) 서비스를
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Admin API 서비스 등록 패턴 (AddService + Register)
// 2. Channelz 서비스 (채널/서버/소켓 계측)
// 3. CSDS (Client Status Discovery Service) 통합
// 4. 순환 의존 회피를 위한 플러그인 패턴
// 5. 채널 계층 구조 (TopChannel → SubChannel → Socket)
// 6. 서버 정보 (리스너, 콜 수)
// 7. Cleanup 함수 패턴
// 8. ServiceRegistrar 인터페이스
// 9. Channelz 메트릭 (calls started/succeeded/failed)
// 10. 소켓 상세 (로컬/원격 주소, 스트림 수)
//
// 실제 소스 참조:
//   - admin/admin.go               (Register, AddService)
//   - internal/admin/admin.go      (서비스 등록 레지스트리)
//   - channelz/service/service.go  (Channelz gRPC 서비스)
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 1. ServiceRegistrar 인터페이스 (grpc.ServiceRegistrar 시뮬레이션)
// ============================================================================

// ServiceRegistrar는 gRPC 서비스를 등록하는 인터페이스다.
type ServiceRegistrar interface {
	RegisterService(name string, handler interface{})
}

// SimpleServer는 간단한 gRPC 서버 시뮬레이션이다.
type SimpleServer struct {
	mu       sync.Mutex
	services map[string]interface{}
}

// NewSimpleServer는 새 서버를 생성한다.
func NewSimpleServer() *SimpleServer {
	return &SimpleServer{services: make(map[string]interface{})}
}

func (s *SimpleServer) RegisterService(name string, handler interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[name] = handler
	fmt.Printf("    서비스 등록: %s\n", name)
}

func (s *SimpleServer) ListServices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for k := range s.services {
		names = append(names, k)
	}
	return names
}

// ============================================================================
// 2. Admin API 서비스 등록 (admin/admin.go + internal/admin/ 시뮬레이션)
// ============================================================================

// 전역 서비스 레지스트리 (internal/admin/admin.go)
var (
	adminMu      sync.Mutex
	adminServices []func(ServiceRegistrar) (func(), error)
)

// AddService는 Admin 서비스를 등록한다.
// xDS 등 외부 패키지가 init()에서 호출하여 순환 의존을 회피한다.
func AddService(f func(ServiceRegistrar) (func(), error)) {
	adminMu.Lock()
	defer adminMu.Unlock()
	adminServices = append(adminServices, f)
}

// Register는 모든 등록된 Admin 서비스를 서버에 등록한다.
// admin/admin.go의 Register 함수를 시뮬레이션한다.
func Register(s ServiceRegistrar) (func(), error) {
	adminMu.Lock()
	defer adminMu.Unlock()

	var cleanups []func()
	for _, svc := range adminServices {
		cleanup, err := svc(s)
		if err != nil {
			return nil, err
		}
		if cleanup != nil {
			cleanups = append(cleanups, cleanup)
		}
	}

	return func() {
		for _, c := range cleanups {
			c()
		}
	}, nil
}

// ============================================================================
// 3. Channelz 데이터 모델 (channelz 시뮬레이션)
// ============================================================================

// ChannelState는 채널 상태다.
type ChannelState int

const (
	ChannelIdle ChannelState = iota
	ChannelConnecting
	ChannelReady
	ChannelTransientFailure
	ChannelShutdown
)

func (s ChannelState) String() string {
	switch s {
	case ChannelIdle:
		return "IDLE"
	case ChannelConnecting:
		return "CONNECTING"
	case ChannelReady:
		return "READY"
	case ChannelTransientFailure:
		return "TRANSIENT_FAILURE"
	case ChannelShutdown:
		return "SHUTDOWN"
	default:
		return "UNKNOWN"
	}
}

// ChannelMetrics는 채널의 RPC 메트릭이다.
type ChannelMetrics struct {
	CallsStarted   atomic.Int64
	CallsSucceeded atomic.Int64
	CallsFailed    atomic.Int64
	LastCallStarted time.Time
}

// Channel은 Channelz의 Top Channel(ClientConn)을 나타낸다.
type Channel struct {
	ID          int64
	RefName     string // 채널 참조 이름 (대상 주소)
	State       ChannelState
	Target      string
	SubChannels []*SubChannel
	Metrics     ChannelMetrics
	CreatedAt   time.Time
}

// SubChannel은 서브 채널을 나타낸다.
type SubChannel struct {
	ID      int64
	RefName string
	State   ChannelState
	Sockets []*Socket
	Metrics ChannelMetrics
}

// Socket은 네트워크 소켓을 나타낸다.
type Socket struct {
	ID           int64
	RefName      string
	LocalAddr    string
	RemoteAddr   string
	StreamsStarted  int64
	MessagesSent    int64
	MessagesRecv    int64
	KeepAliveSent   int64
	LastMsgSentTime time.Time
}

// ServerInfo는 Channelz의 서버 정보다.
type ServerInfo struct {
	ID          int64
	RefName     string
	ListenAddrs []string
	Metrics     ChannelMetrics
}

// ============================================================================
// 4. Channelz 서비스 (channelz/service/service.go 시뮬레이션)
// ============================================================================

// ChannelzService는 Channelz gRPC 서비스다.
type ChannelzService struct {
	mu       sync.RWMutex
	channels map[int64]*Channel
	servers  map[int64]*ServerInfo
	nextID   int64
}

// NewChannelzService는 새 Channelz 서비스를 생성한다.
func NewChannelzService() *ChannelzService {
	return &ChannelzService{
		channels: make(map[int64]*Channel),
		servers:  make(map[int64]*ServerInfo),
		nextID:   1,
	}
}

// RegisterChannel은 채널을 등록한다.
func (s *ChannelzService) RegisterChannel(target string) *Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := &Channel{
		ID:        s.nextID,
		RefName:   target,
		State:     ChannelReady,
		Target:    target,
		CreatedAt: time.Now(),
	}
	s.channels[s.nextID] = ch
	s.nextID++
	return ch
}

// RegisterServer는 서버를 등록한다.
func (s *ChannelzService) RegisterServer(addrs []string) *ServerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv := &ServerInfo{
		ID:          s.nextID,
		ListenAddrs: addrs,
	}
	s.servers[s.nextID] = srv
	s.nextID++
	return srv
}

// GetTopChannels는 모든 최상위 채널을 반환한다.
func (s *ChannelzService) GetTopChannels() []*Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Channel
	for _, ch := range s.channels {
		result = append(result, ch)
	}
	return result
}

// GetServers는 모든 서버를 반환한다.
func (s *ChannelzService) GetServers() []*ServerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*ServerInfo
	for _, srv := range s.servers {
		result = append(result, srv)
	}
	return result
}

// GetChannel은 특정 채널의 상세 정보를 반환한다.
func (s *ChannelzService) GetChannel(id int64) (*Channel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch, ok := s.channels[id]
	return ch, ok
}

// ============================================================================
// 5. CSDS (Client Status Discovery Service) 시뮬레이션
// ============================================================================

// CSDSConfig는 xDS 설정 상태를 나타낸다.
type CSDSConfig struct {
	TypeURL     string
	Name        string
	VersionInfo string
	Status      string // ACK, NACK, REQUESTED, DOES_NOT_EXIST
}

// CSDSService는 CSDS 서비스다.
type CSDSService struct {
	configs []CSDSConfig
}

// NewCSDSService는 새 CSDS 서비스를 생성한다.
func NewCSDSService() *CSDSService {
	return &CSDSService{}
}

// AddConfig는 xDS 설정 상태를 추가한다.
func (s *CSDSService) AddConfig(config CSDSConfig) {
	s.configs = append(s.configs, config)
}

// GetClientConfig는 클라이언트의 xDS 설정 덤프를 반환한다.
func (s *CSDSService) GetClientConfig() []CSDSConfig {
	return s.configs
}

// ============================================================================
// init - Channelz 서비스를 Admin에 등록 (admin/admin.go의 init 시뮬레이션)
// ============================================================================

var globalChannelz = NewChannelzService()

func init() {
	// admin/admin.go의 init()에서 Channelz 서비스를 등록한다
	AddService(func(s ServiceRegistrar) (func(), error) {
		s.RegisterService("grpc.channelz.v1.Channelz", globalChannelz)
		return func() {
			fmt.Println("    Channelz 서비스 정리 완료")
		}, nil
	})

	// xDS 패키지의 init()에서 CSDS를 등록하는 패턴 시뮬레이션
	csds := NewCSDSService()
	csds.AddConfig(CSDSConfig{
		TypeURL:     "type.googleapis.com/envoy.config.listener.v3.Listener",
		Name:        "inbound",
		VersionInfo: "v42",
		Status:      "ACK",
	})
	csds.AddConfig(CSDSConfig{
		TypeURL:     "type.googleapis.com/envoy.config.cluster.v3.Cluster",
		Name:        "backend-cluster",
		VersionInfo: "v15",
		Status:      "ACK",
	})

	AddService(func(s ServiceRegistrar) (func(), error) {
		s.RegisterService("envoy.service.status.v3.ClientStatusDiscoveryService", csds)
		return nil, nil
	})
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  gRPC-Go Admin API (Channelz + CSDS) 시뮬레이션 PoC        ║")
	fmt.Println("║  실제 소스: admin/admin.go, channelz/service/               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. Admin 서비스 등록 ===
	fmt.Println("=== 1. Admin 서비스 등록 (admin.Register) ===")
	server := NewSimpleServer()
	cleanup, err := Register(server)
	if err != nil {
		fmt.Printf("  등록 오류: %v\n", err)
		return
	}
	fmt.Printf("  등록된 서비스: %v\n\n", server.ListServices())

	// === 2. Channelz 채널 등록 ===
	fmt.Println("=== 2. Channelz 채널 등록 ===")

	ch1 := globalChannelz.RegisterChannel("dns:///backend.example.com:443")
	ch1.SubChannels = []*SubChannel{
		{
			ID:      100,
			RefName: "backend.example.com:443",
			State:   ChannelReady,
			Sockets: []*Socket{
				{
					ID:           200,
					LocalAddr:    "192.168.1.10:50051",
					RemoteAddr:   "10.0.0.1:443",
					StreamsStarted: 150,
					MessagesSent:   500,
					MessagesRecv:   480,
					KeepAliveSent:  25,
					LastMsgSentTime: time.Now(),
				},
			},
		},
		{
			ID:      101,
			RefName: "backend.example.com:443",
			State:   ChannelReady,
			Sockets: []*Socket{
				{
					ID:           201,
					LocalAddr:    "192.168.1.10:50052",
					RemoteAddr:   "10.0.0.2:443",
					StreamsStarted: 120,
					MessagesSent:   380,
					MessagesRecv:   375,
					KeepAliveSent:  20,
				},
			},
		},
	}
	ch1.Metrics.CallsStarted.Store(270)
	ch1.Metrics.CallsSucceeded.Store(265)
	ch1.Metrics.CallsFailed.Store(5)

	ch2 := globalChannelz.RegisterChannel("dns:///auth.example.com:443")
	ch2.Metrics.CallsStarted.Store(50)
	ch2.Metrics.CallsSucceeded.Store(50)

	srv := globalChannelz.RegisterServer([]string{"0.0.0.0:8080", "[::]:8080"})
	srv.Metrics.CallsStarted.Store(1000)
	srv.Metrics.CallsSucceeded.Store(995)
	srv.Metrics.CallsFailed.Store(5)

	// === 3. GetTopChannels ===
	fmt.Println("=== 3. GetTopChannels ===")
	channels := globalChannelz.GetTopChannels()
	for _, ch := range channels {
		fmt.Printf("  Channel[%d] target=%s state=%s\n", ch.ID, ch.Target, ch.State)
		fmt.Printf("    calls: started=%d, succeeded=%d, failed=%d\n",
			ch.Metrics.CallsStarted.Load(),
			ch.Metrics.CallsSucceeded.Load(),
			ch.Metrics.CallsFailed.Load())
		for _, sub := range ch.SubChannels {
			fmt.Printf("    SubChannel[%d] state=%s\n", sub.ID, sub.State)
			for _, sock := range sub.Sockets {
				fmt.Printf("      Socket[%d] %s → %s (streams=%d, sent=%d, recv=%d)\n",
					sock.ID, sock.LocalAddr, sock.RemoteAddr,
					sock.StreamsStarted, sock.MessagesSent, sock.MessagesRecv)
			}
		}
	}
	fmt.Println()

	// === 4. GetServers ===
	fmt.Println("=== 4. GetServers ===")
	servers := globalChannelz.GetServers()
	for _, srv := range servers {
		fmt.Printf("  Server[%d] listen=%v\n", srv.ID, srv.ListenAddrs)
		fmt.Printf("    calls: started=%d, succeeded=%d, failed=%d\n",
			srv.Metrics.CallsStarted.Load(),
			srv.Metrics.CallsSucceeded.Load(),
			srv.Metrics.CallsFailed.Load())
	}
	fmt.Println()

	// === 5. GetChannel (상세) ===
	fmt.Println("=== 5. GetChannel (상세 조회) ===")
	ch, ok := globalChannelz.GetChannel(ch1.ID)
	if ok {
		fmt.Printf("  Channel[%d] 상세:\n", ch.ID)
		fmt.Printf("    Target: %s\n", ch.Target)
		fmt.Printf("    State: %s\n", ch.State)
		fmt.Printf("    서브채널 수: %d\n", len(ch.SubChannels))
		totalSockets := 0
		for _, sub := range ch.SubChannels {
			totalSockets += len(sub.Sockets)
		}
		fmt.Printf("    총 소켓 수: %d\n", totalSockets)
	}
	fmt.Println()

	// === 6. CSDS ===
	fmt.Println("=== 6. CSDS (Client Status Discovery Service) ===")
	// CSDS 서비스는 init()에서 등록됨
	// server에서 가져오기
	if csds, ok := server.services["envoy.service.status.v3.ClientStatusDiscoveryService"]; ok {
		configs := csds.(*CSDSService).GetClientConfig()
		for _, cfg := range configs {
			fmt.Printf("  %s\n", cfg.TypeURL)
			fmt.Printf("    name=%s, version=%s, status=%s\n", cfg.Name, cfg.VersionInfo, cfg.Status)
		}
	}
	fmt.Println()

	// === 7. 플러그인 패턴 설명 ===
	fmt.Println("=== 7. 플러그인 패턴 (순환 의존 회피) ===")
	fmt.Println("  admin 패키지는 channelz를 직접 import")
	fmt.Println("  xDS 패키지는 init()에서 AddService()로 CSDS를 admin에 등록")
	fmt.Println("  → admin이 xDS를 import하지 않아 순환 의존 회피")
	fmt.Println()

	// === 8. 채널 계층 구조 ===
	fmt.Println("=== 8. 채널 계층 구조 ===")
	for _, ch := range channels {
		fmt.Printf("  TopChannel[%d] %s (%s)\n", ch.ID, ch.Target, ch.State)
		for _, sub := range ch.SubChannels {
			fmt.Printf("  %s SubChannel[%d] %s (%s)\n", "├──", sub.ID, sub.RefName, sub.State)
			for j, sock := range sub.Sockets {
				connector := "│   ├──"
				if j == len(sub.Sockets)-1 {
					connector = "│   └──"
				}
				fmt.Printf("  %s Socket[%d] %s → %s\n", connector, sock.ID, sock.LocalAddr, sock.RemoteAddr)
			}
		}
	}
	fmt.Println()

	// Cleanup
	fmt.Println("=== 9. Cleanup ===")
	cleanup()
}
