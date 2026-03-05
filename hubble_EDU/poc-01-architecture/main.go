package main

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// =============================================================================
// Hubble 서버 아키텍처 시뮬레이션
//
// 실제 구현 참조:
//   - pkg/hubble/server/server.go: Server 구조체, NewServer(), initGRPCServer()
//   - pkg/hubble/server/serveroption/option.go: Options, WithTCPListener 등
//
// Hubble 서버는 gRPC 서버를 중심으로 3개의 핵심 서비스를 등록한다:
//   1. Observer 서비스: 플로우 이벤트 조회 (GetFlows, ServerStatus)
//   2. Peer 서비스: 피어 노드 발견/변경 알림 (Notify)
//   3. Health 서비스: 헬스 체크 (Check, Watch)
//
// 서버 초기화 흐름:
//   옵션 적용 → Listener 검증 → TLS 설정 → gRPC 서버 생성
//   → 서비스 등록 → 메트릭 초기화 → Serve() 시작
// =============================================================================

// -----------------------------------------------------------------------------
// 서비스 인터페이스 (실제: observerpb.ObserverServer, peerpb.PeerServer, healthpb.HealthServer)
// -----------------------------------------------------------------------------

// Service는 gRPC 서비스의 공통 인터페이스
type Service interface {
	Name() string
	Start() error
	Stop()
	HealthCheck() string
}

// ObserverService는 플로우 관찰 서비스를 시뮬레이션
// 실제: pkg/hubble/observer/local_observer.go의 LocalObserverServer
type ObserverService struct {
	mu       sync.Mutex
	running  bool
	flowChan chan string
}

func NewObserverService() *ObserverService {
	return &ObserverService{
		flowChan: make(chan string, 100),
	}
}

func (o *ObserverService) Name() string { return "hubble.observer.Observer" }

func (o *ObserverService) Start() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.running = true
	fmt.Println("  [Observer] 플로우 관찰 서비스 시작")
	fmt.Println("  [Observer] GetFlows, GetAgentEvents, GetDebugEvents, ServerStatus RPC 등록")
	return nil
}

func (o *ObserverService) Stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.running = false
	close(o.flowChan)
	fmt.Println("  [Observer] 서비스 중지")
}

func (o *ObserverService) HealthCheck() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running {
		return "SERVING"
	}
	return "NOT_SERVING"
}

// PeerService는 피어 발견 서비스를 시뮬레이션
// 실제: pkg/hubble/peer/service.go의 Service
type PeerService struct {
	mu      sync.Mutex
	running bool
	peers   map[string]string
}

func NewPeerService() *PeerService {
	return &PeerService{
		peers: make(map[string]string),
	}
}

func (p *PeerService) Name() string { return "hubble.peer.Peer" }

func (p *PeerService) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	fmt.Println("  [Peer] 피어 디스커버리 서비스 시작")
	fmt.Println("  [Peer] Notify RPC 등록")
	return nil
}

func (p *PeerService) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	fmt.Println("  [Peer] 서비스 중지")
}

func (p *PeerService) HealthCheck() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return "SERVING"
	}
	return "NOT_SERVING"
}

// HealthService는 gRPC 헬스 체크 서비스를 시뮬레이션
// 실제: google.golang.org/grpc/health의 Server
type HealthService struct {
	mu       sync.RWMutex
	statuses map[string]string
}

func NewHealthService() *HealthService {
	return &HealthService{
		statuses: make(map[string]string),
	}
}

func (h *HealthService) Name() string { return "grpc.health.v1.Health" }

func (h *HealthService) Start() error {
	fmt.Println("  [Health] 헬스 체크 서비스 시작")
	fmt.Println("  [Health] Check, Watch RPC 등록")
	return nil
}

func (h *HealthService) Stop() {
	fmt.Println("  [Health] 서비스 중지")
}

func (h *HealthService) HealthCheck() string {
	return "SERVING"
}

func (h *HealthService) SetServingStatus(service, status string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statuses[service] = status
	fmt.Printf("  [Health] 서비스 상태 설정: %s = %s\n", service, status)
}

func (h *HealthService) Check(service string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if s, ok := h.statuses[service]; ok {
		return s
	}
	return "UNKNOWN"
}

// -----------------------------------------------------------------------------
// Options 패턴 (실제: serveroption.Options + Option 함수 타입)
// -----------------------------------------------------------------------------

// Options는 서버 설정을 담는 구조체
// 실제: pkg/hubble/server/serveroption/option.go의 Options
type Options struct {
	Listener        net.Listener
	ObserverService Service
	PeerService     Service
	HealthService   *HealthService
	Insecure        bool
	TLSEnabled      bool
}

// Option은 서버 옵션을 설정하는 함수 타입
// 실제: type Option func(o *Options) error
type Option func(o *Options) error

// WithTCPListener는 TCP 리스너를 설정
// 실제: serveroption.WithTCPListener
func WithTCPListener(address string) Option {
	return func(o *Options) error {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("리스너 생성 실패: %w", err)
		}
		if o.Listener != nil {
			listener.Close()
			return fmt.Errorf("리스너가 이미 설정됨: %s", address)
		}
		o.Listener = listener
		fmt.Printf("  [옵션] TCP 리스너 설정: %s\n", address)
		return nil
	}
}

// WithObserverService는 Observer 서비스를 설정
// 실제: serveroption.WithObserverService
func WithObserverService(svc Service) Option {
	return func(o *Options) error {
		o.ObserverService = svc
		fmt.Printf("  [옵션] Observer 서비스 등록: %s\n", svc.Name())
		return nil
	}
}

// WithPeerService는 Peer 서비스를 설정
// 실제: serveroption.WithPeerService
func WithPeerService(svc Service) Option {
	return func(o *Options) error {
		o.PeerService = svc
		fmt.Printf("  [옵션] Peer 서비스 등록: %s\n", svc.Name())
		return nil
	}
}

// WithHealthService는 Health 서비스를 설정
// 실제: serveroption.WithHealthService
func WithHealthService() Option {
	return func(o *Options) error {
		healthSvc := NewHealthService()
		healthSvc.SetServingStatus("hubble.observer.Observer", "SERVING")
		o.HealthService = healthSvc
		return nil
	}
}

// WithInsecure는 TLS 없이 서버를 설정
// 실제: serveroption.WithInsecure
func WithInsecure() Option {
	return func(o *Options) error {
		o.Insecure = true
		fmt.Println("  [옵션] Insecure 모드 설정 (TLS 비활성화)")
		return nil
	}
}

// -----------------------------------------------------------------------------
// Server 구조체 (실제: pkg/hubble/server/server.go의 Server)
// -----------------------------------------------------------------------------

// Server는 Hubble gRPC 서버를 시뮬레이션
// 실제 Server는 *grpc.Server, serveroption.Options, *slog.Logger를 포함
type Server struct {
	opts     Options
	services []Service
	stopCh   chan struct{}
}

// NewServer는 새 Hubble 서버를 생성
// 실제: server.NewServer(log, options...)
// 흐름: 옵션 적용 → Listener 검증 → TLS 검증 → initGRPCServer()
func NewServer(options ...Option) (*Server, error) {
	fmt.Println("\n=== Hubble 서버 생성 시작 ===")
	fmt.Println("[1단계] 옵션 적용")

	opts := Options{}
	for _, opt := range options {
		if err := opt(&opts); err != nil {
			return nil, fmt.Errorf("옵션 적용 실패: %w", err)
		}
	}

	// Listener 검증 (실제: errNoListener)
	fmt.Println("\n[2단계] 설정 검증")
	if opts.Listener == nil {
		return nil, fmt.Errorf("리스너가 설정되지 않음")
	}
	fmt.Println("  리스너 검증 통과")

	// TLS 검증 (실제: errNoServerTLSConfig)
	if !opts.Insecure && !opts.TLSEnabled {
		return nil, fmt.Errorf("TLS 설정이 필요합니다 (또는 WithInsecure 사용)")
	}
	if opts.Insecure {
		fmt.Println("  Insecure 모드로 동작 (TLS 없음)")
	} else {
		fmt.Println("  TLS 설정 검증 통과 (MinVersion: TLS 1.3)")
	}

	s := &Server{
		opts:   opts,
		stopCh: make(chan struct{}),
	}

	// initGRPCServer 시뮬레이션
	// 실제: s.initGRPCServer()에서 gRPC 서버 생성 후 서비스 등록
	fmt.Println("\n[3단계] gRPC 서버 초기화 (initGRPCServer)")
	s.initGRPCServer()

	return s, nil
}

// initGRPCServer는 gRPC 서버를 초기화하고 서비스를 등록
// 실제 순서:
//  1. grpc.NewServer(opts...) - 인터셉터, TLS 등 설정
//  2. healthpb.RegisterHealthServer - 헬스 서비스 등록
//  3. observerpb.RegisterObserverServer - Observer 서비스 등록
//  4. peerpb.RegisterPeerServer - Peer 서비스 등록
//  5. reflection.Register - gRPC reflection 등록
//  6. GRPCMetrics.InitializeMetrics - 메트릭 초기화
func (s *Server) initGRPCServer() {
	fmt.Println("  gRPC 서버 인스턴스 생성")

	// 서비스 등록 순서는 실제 코드와 동일
	if s.opts.HealthService != nil {
		fmt.Printf("  서비스 등록: %s\n", s.opts.HealthService.Name())
		s.services = append(s.services, s.opts.HealthService)
	}
	if s.opts.ObserverService != nil {
		fmt.Printf("  서비스 등록: %s\n", s.opts.ObserverService.Name())
		s.services = append(s.services, s.opts.ObserverService)
	}
	if s.opts.PeerService != nil {
		fmt.Printf("  서비스 등록: %s\n", s.opts.PeerService.Name())
		s.services = append(s.services, s.opts.PeerService)
	}

	fmt.Println("  gRPC reflection 등록")
	fmt.Println("  gRPC 서버 초기화 완료")
}

// Serve는 서버를 시작하고 연결을 수락
// 실제: s.srv.Serve(s.opts.Listener)
func (s *Server) Serve() error {
	fmt.Printf("\n[4단계] 서버 시작: %s\n", s.opts.Listener.Addr().String())

	// 모든 서비스 시작
	for _, svc := range s.services {
		if err := svc.Start(); err != nil {
			return fmt.Errorf("서비스 시작 실패 (%s): %w", svc.Name(), err)
		}
	}

	fmt.Println("\n=== Hubble 서버가 연결을 수락합니다 ===")

	// 시뮬레이션: 클라이언트 연결 처리
	go func() {
		for i := 1; i <= 3; i++ {
			time.Sleep(300 * time.Millisecond)
			fmt.Printf("\n  [연결] 클라이언트 #%d 연결 수락\n", i)

			// 헬스 체크 시뮬레이션
			if s.opts.HealthService != nil {
				status := s.opts.HealthService.Check("hubble.observer.Observer")
				fmt.Printf("  [헬스 체크] hubble.observer.Observer = %s\n", status)
			}

			// 각 서비스 상태 보고
			for _, svc := range s.services {
				fmt.Printf("  [상태] %s: %s\n", svc.Name(), svc.HealthCheck())
			}
		}

		// 서버 종료 신호
		close(s.stopCh)
	}()

	<-s.stopCh
	return nil
}

// Stop은 서버를 중지
// 실제: s.srv.Stop()
func (s *Server) Stop() {
	fmt.Println("\n=== Hubble 서버 종료 ===")
	for _, svc := range s.services {
		svc.Stop()
	}
	if s.opts.Listener != nil {
		s.opts.Listener.Close()
	}
	fmt.Println("서버 종료 완료")
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     Hubble 서버 아키텍처 시뮬레이션                          ║")
	fmt.Println("║     참조: pkg/hubble/server/server.go                       ║")
	fmt.Println("║           pkg/hubble/server/serveroption/option.go          ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	// 서비스 인스턴스 생성
	observerSvc := NewObserverService()
	peerSvc := NewPeerService()

	// 실제 Hubble 서버 생성과 동일한 옵션 패턴 사용
	// 실제 코드: server.NewServer(log, WithTCPListener(addr), WithObserverService(obs), ...)
	srv, err := NewServer(
		WithTCPListener("127.0.0.1:0"), // :0은 OS가 포트를 자동 할당
		WithInsecure(),
		WithHealthService(),
		WithObserverService(observerSvc),
		WithPeerService(peerSvc),
	)
	if err != nil {
		log.Fatalf("서버 생성 실패: %v", err)
	}

	// 서버 실행
	if err := srv.Serve(); err != nil {
		log.Fatalf("서버 실행 오류: %v", err)
	}

	// 서버 종료
	srv.Stop()

	// 아키텍처 요약 출력
	fmt.Println("\n" + `
┌──────────────────────────────────────────────────────────────────┐
│                    Hubble 서버 아키텍처                           │
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│  NewServer(options...)                                           │
│      │                                                           │
│      ├─ 옵션 적용: Listener, TLS, Services                       │
│      ├─ 설정 검증: Listener != nil, TLS/Insecure                 │
│      └─ initGRPCServer()                                         │
│            │                                                     │
│            ├─ grpc.NewServer(interceptors, TLS)                  │
│            ├─ RegisterHealthServer(srv, healthSvc)               │
│            ├─ RegisterObserverServer(srv, observerSvc)           │
│            ├─ RegisterPeerServer(srv, peerSvc)                   │
│            ├─ reflection.Register(srv)                           │
│            └─ GRPCMetrics.InitializeMetrics(srv)                 │
│                                                                  │
│  Server.Serve()                                                  │
│      └─ srv.Serve(listener) ← 클라이언트 연결 수락               │
│                                                                  │
│  Server.Stop()                                                   │
│      └─ srv.Stop() ← graceful shutdown                           │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘`)
}
