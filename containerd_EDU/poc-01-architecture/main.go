// containerd PoC-01: gRPC 서버/클라이언트 + 플러그인 등록 아키텍처
//
// 실제 소스 참조:
//   - cmd/containerd/server/server.go         : Server 구조체, New(), LoadPlugins(), ServeGRPC/TTRPC/Metrics/Debug
//   - vendor/github.com/containerd/plugin/plugin.go : Registration, Type, Graph(), children() DFS
//   - plugins/types.go                        : GRPCPlugin, ServicePlugin, ContentPlugin, SnapshotPlugin 등 타입 정의
//   - vendor/github.com/containerd/plugin/registry/registry.go : 전역 레지스트리, Register(), Graph()
//
// 핵심 개념:
//   1. containerd는 플러그인 기반 아키텍처 — 모든 기능이 플러그인으로 등록됨
//   2. Registration{Type, ID, Requires, InitFn}으로 플러그인을 정의
//   3. Graph() 함수가 Requires 의존성을 DFS로 순회하여 초기화 순서를 결정
//   4. 서버는 4개의 리스너를 병렬 운영: gRPC, TTRPC, Metrics(HTTP), Debug(HTTP/pprof)
//   5. 각 플러그인의 InitFn이 순서대로 호출되어 서비스 인스턴스를 생성
//
// 실행: go run main.go

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. 플러그인 타입 정의 (plugins/types.go 참조)
// =============================================================================

// Type은 플러그인의 종류를 나타내는 문자열 타입
// 실제: plugin.Type = string, "io.containerd.grpc.v1" 등의 형식
type Type string

const (
	// 실제 containerd 플러그인 타입들 (plugins/types.go 참조)
	InternalPlugin Type = "io.containerd.internal.v1"
	RuntimePlugin  Type = "io.containerd.runtime.v2"
	ServicePlugin  Type = "io.containerd.service.v1"
	GRPCPlugin     Type = "io.containerd.grpc.v1"
	TTRPCPlugin    Type = "io.containerd.ttrpc.v1"
	SnapshotPlugin Type = "io.containerd.snapshotter.v1"
	ContentPlugin  Type = "io.containerd.content.v1"
	MetadataPlugin Type = "io.containerd.metadata.v1"
	DiffPlugin     Type = "io.containerd.differ.v1"
	EventPlugin    Type = "io.containerd.event.v1"
	GCPlugin       Type = "io.containerd.gc.v1"
	LeasePlugin    Type = "io.containerd.lease.v1"
)

func (t Type) String() string { return string(t) }

// =============================================================================
// 2. 플러그인 등록 (plugin.go의 Registration 구조체 참조)
// =============================================================================

// InitContext는 플러그인 초기화 시 전달되는 컨텍스트
// 실제: plugin.InitContext — 이미 초기화된 플러그인셋 + 속성맵을 포함
type InitContext struct {
	Context     context.Context
	Initialized *PluginSet
	Properties  map[string]string
	Config      interface{}
}

// Registration은 플러그인 등록 정보
// 실제: plugin.Registration{Type, ID, Config, Requires, InitFn, ConfigMigration}
type Registration struct {
	Type     Type
	ID       string
	Requires []Type
	InitFn   func(*InitContext) (interface{}, error)
}

// URI는 "Type.ID" 형식의 고유 식별자를 반환
// 실제: plugin.go의 Registration.URI() — Type.String() + "." + ID
func (r *Registration) URI() string {
	return r.Type.String() + "." + r.ID
}

// =============================================================================
// 3. 플러그인 레지스트리 (plugin.go의 Registry, Graph, children 참조)
// =============================================================================

// Registry는 등록된 플러그인 목록
// 실제: plugin.Registry = []*Registration
type Registry []*Registration

// 전역 레지스트리
// 실제: registry/registry.go의 전역 변수 + Register()/Graph() 함수
var globalRegistry Registry

// Register는 플러그인을 전역 레지스트리에 등록
// 실제: registry.Register() — Type과 ID 필수 검증, URI 중복 시 panic
func Register(r *Registration) {
	if r.Type == "" {
		panic("plugin: no type")
	}
	if r.ID == "" {
		panic("plugin: no id")
	}
	// 중복 URI 검사
	for _, existing := range globalRegistry {
		if r.URI() == existing.URI() {
			panic(fmt.Sprintf("%s: plugin id already registered", r.URI()))
		}
	}
	// Requires에 "*"가 포함되면 단독이어야 함
	for _, req := range r.Requires {
		if req == "*" && len(r.Requires) != 1 {
			panic("invalid requires: wildcard must be sole requirement")
		}
	}
	globalRegistry = append(globalRegistry, r)
}

// Graph는 의존성 기반 토폴로지 정렬된 플러그인 목록을 반환
// 실제: plugin.go의 Registry.Graph(filter DisableFilter) []Registration
//
// 알고리즘:
//   1. disabled 필터 적용
//   2. 각 플러그인에 대해 children() DFS 재귀 호출
//   3. 의존 플러그인이 먼저 ordered에 추가됨 → 초기화 순서 보장
func Graph(disabled map[string]bool) []Registration {
	ordered := make([]Registration, 0, len(globalRegistry))
	added := map[*Registration]bool{}

	for _, r := range globalRegistry {
		if disabled[r.URI()] {
			continue
		}
		// DFS로 의존성 먼저 추가
		children(r, globalRegistry, added, disabled, &ordered)
		if !added[r] {
			ordered = append(ordered, *r)
			added[r] = true
		}
	}
	return ordered
}

// children는 재귀적으로 의존성을 탐색하여 ordered에 추가
// 실제: plugin.go의 children() 함수 — Requires 타입 매칭으로 의존 플러그인을 찾음
//
// 핵심 로직:
//   - reg.Requires에 있는 각 Type에 대해
//   - 레지스트리에서 해당 Type의 플러그인을 찾아
//   - 재귀적으로 그 플러그인의 의존성을 먼저 처리
//   - Type == "*" 이면 모든 플러그인에 의존 (와일드카드)
func children(reg *Registration, registry []*Registration,
	added map[*Registration]bool, disabled map[string]bool, ordered *[]Registration) {
	for _, t := range reg.Requires {
		for _, r := range registry {
			if disabled[r.URI()] {
				continue
			}
			// 자기 자신은 제외, 타입 매칭 또는 와일드카드
			if r.URI() != reg.URI() && (t == "*" || r.Type == t) {
				children(r, registry, added, disabled, ordered)
				if !added[r] {
					*ordered = append(*ordered, *r)
					added[r] = true
				}
			}
		}
	}
}

// =============================================================================
// 4. 플러그인 인스턴스 (plugin.go의 Plugin, PluginSet 참조)
// =============================================================================

// Plugin은 초기화된 플러그인 인스턴스
// 실제: plugin.Plugin{Registration, Config, Meta, instance, err}
type Plugin struct {
	Registration Registration
	instance     interface{}
	err          error
}

func (p *Plugin) Instance() (interface{}, error) {
	return p.instance, p.err
}

// PluginSet은 초기화된 플러그인들의 집합
// 실제: plugin.Set — sync.Mutex로 보호되는 ordered + byTypeAndID 맵
type PluginSet struct {
	mu      sync.Mutex
	plugins []*Plugin
}

func NewPluginSet() *PluginSet {
	return &PluginSet{}
}

func (ps *PluginSet) Add(p *Plugin) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.plugins = append(ps.plugins, p)
}

// =============================================================================
// 5. gRPC/TTRPC 서비스 인터페이스 (server.go 참조)
// =============================================================================

// grpcService는 gRPC 서버에 등록 가능한 서비스
// 실제: server.go 내부 인터페이스 — Register(*grpc.Server) error
type grpcService interface {
	RegisterGRPC(s *SimpleGRPCServer) error
}

// ttrpcService는 TTRPC 서버에 등록 가능한 서비스
// 실제: server.go 내부 인터페이스 — RegisterTTRPC(*ttrpc.Server) error
type ttrpcService interface {
	RegisterTTRPC(s *SimpleTTRPCServer) error
}

// =============================================================================
// 6. 서버 시뮬레이션 (server.go의 Server 참조)
// =============================================================================

// SimpleGRPCServer는 gRPC 서버를 시뮬레이션
type SimpleGRPCServer struct {
	services []string
	listener net.Listener
}

func (s *SimpleGRPCServer) RegisterService(name string) {
	s.services = append(s.services, name)
}

func (s *SimpleGRPCServer) Serve(l net.Listener) error {
	s.listener = l
	fmt.Printf("  [gRPC Server] 등록된 서비스: %v\n", s.services)
	// 간단한 연결 수락 시뮬레이션
	for {
		conn, err := l.Accept()
		if err != nil {
			return nil // 리스너 닫힘
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 1024)
			n, _ := c.Read(buf)
			if n > 0 {
				response := fmt.Sprintf("gRPC response: services=%v", s.services)
				c.Write([]byte(response))
			}
		}(conn)
	}
}

// SimpleTTRPCServer는 TTRPC 서버를 시뮬레이션
// 실제: containerd는 낮은 오버헤드의 TTRPC(tiny-TTRPC)를 shim 통신에 사용
type SimpleTTRPCServer struct {
	services []string
}

func (s *SimpleTTRPCServer) RegisterService(name string) {
	s.services = append(s.services, name)
}

// =============================================================================
// 7. Server 구조체 (server.go의 Server 참조)
// =============================================================================

// Server는 containerd 메인 데몬
// 실제 필드: prometheusServerMetrics, grpcServer, ttrpcServer, tcpServer, config, plugins, ready
type Server struct {
	grpcServer  *SimpleGRPCServer
	ttrpcServer *SimpleTTRPCServer
	plugins     []*Plugin
	ready       sync.WaitGroup
}

// =============================================================================
// 8. 구체적인 플러그인 구현체들 (시뮬레이션)
// =============================================================================

// ContentStoreService — Content Store 서비스
type ContentStoreService struct{ name string }

func (c *ContentStoreService) RegisterGRPC(s *SimpleGRPCServer) error {
	s.RegisterService("containerd.services.content.v1.Content")
	return nil
}

// SnapshotService — Snapshot 서비스
type SnapshotService struct{ name string }

func (s *SnapshotService) RegisterGRPC(srv *SimpleGRPCServer) error {
	srv.RegisterService("containerd.services.snapshots.v1.Snapshots")
	return nil
}

// ImageService — Image 서비스
type ImageService struct{ name string }

func (i *ImageService) RegisterGRPC(s *SimpleGRPCServer) error {
	s.RegisterService("containerd.services.images.v1.Images")
	return nil
}
func (i *ImageService) RegisterTTRPC(s *SimpleTTRPCServer) error {
	s.RegisterService("containerd.services.images.v1.Images")
	return nil
}

// ContainerService — Container 서비스
type ContainerService struct{ name string }

func (c *ContainerService) RegisterGRPC(s *SimpleGRPCServer) error {
	s.RegisterService("containerd.services.containers.v1.Containers")
	return nil
}
func (c *ContainerService) RegisterTTRPC(s *SimpleTTRPCServer) error {
	s.RegisterService("containerd.services.containers.v1.Containers")
	return nil
}

// TaskService — Task 서비스
type TaskService struct{ name string }

func (t *TaskService) RegisterGRPC(s *SimpleGRPCServer) error {
	s.RegisterService("containerd.services.tasks.v1.Tasks")
	return nil
}
func (t *TaskService) RegisterTTRPC(s *SimpleTTRPCServer) error {
	s.RegisterService("containerd.services.tasks.v1.Tasks")
	return nil
}

// MetadataService — Metadata 서비스 (boltdb)
type MetadataService struct{ name string }

// EventService — 이벤트 서비스
type EventService struct{ name string }

// =============================================================================
// 9. 플러그인 등록 (init 패턴 시뮬레이션)
// =============================================================================

func registerAllPlugins() {
	// 실제 containerd에서는 각 패키지의 init()에서 registry.Register() 호출
	// 여기서는 초기화 순서를 명확히 보여주기 위해 한 곳에서 등록

	// 1) 이벤트 플러그인 — 다른 의존성 없음
	Register(&Registration{
		Type: EventPlugin,
		ID:   "exchange",
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &EventService{name: "exchange"}, nil
		},
	})

	// 2) Content Store — 이벤트에 의존
	Register(&Registration{
		Type:     ContentPlugin,
		ID:       "content",
		Requires: []Type{EventPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &ContentStoreService{name: "content"}, nil
		},
	})

	// 3) Snapshot — Content에 의존
	Register(&Registration{
		Type:     SnapshotPlugin,
		ID:       "overlayfs",
		Requires: []Type{ContentPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &SnapshotService{name: "overlayfs"}, nil
		},
	})

	// 4) Metadata — Content + Snapshot에 의존
	Register(&Registration{
		Type:     MetadataPlugin,
		ID:       "bolt",
		Requires: []Type{ContentPlugin, SnapshotPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &MetadataService{name: "bolt"}, nil
		},
	})

	// 5) GC — Metadata에 의존
	Register(&Registration{
		Type:     GCPlugin,
		ID:       "scheduler",
		Requires: []Type{MetadataPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return struct{}{}, nil
		},
	})

	// 6) Image 서비스 (gRPC) — Metadata에 의존
	Register(&Registration{
		Type:     GRPCPlugin,
		ID:       "images",
		Requires: []Type{MetadataPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &ImageService{name: "images"}, nil
		},
	})

	// 7) Container 서비스 (gRPC) — Metadata + Runtime에 의존
	Register(&Registration{
		Type:     GRPCPlugin,
		ID:       "containers",
		Requires: []Type{MetadataPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &ContainerService{name: "containers"}, nil
		},
	})

	// 8) Task 서비스 (gRPC) — 거의 모든 것에 의존
	Register(&Registration{
		Type:     GRPCPlugin,
		ID:       "tasks",
		Requires: []Type{MetadataPlugin, RuntimePlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &TaskService{name: "tasks"}, nil
		},
	})

	// 9) Runtime v2
	Register(&Registration{
		Type:     RuntimePlugin,
		ID:       "task",
		Requires: []Type{MetadataPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return struct{}{}, nil
		},
	})
}

// =============================================================================
// 10. 서버 초기화 (server.go의 New() 함수 참조)
// =============================================================================

// NewServer는 containerd 서버를 생성하고 플러그인을 초기화
// 실제 흐름:
//   1. config 로드 → 플러그인 로드 (LoadPlugins)
//   2. Graph()로 의존성 정렬
//   3. 순서대로 InitFn 호출
//   4. grpcService/ttrpcService 인터페이스 구현 시 해당 서버에 등록
func NewServer() (*Server, error) {
	fmt.Println("=" + strings.Repeat("=", 69))
	fmt.Println("containerd 서버 초기화 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 69))

	// 1) 의존성 정렬
	fmt.Println("\n[1단계] Graph() — DFS 의존성 정렬")
	disabled := map[string]bool{} // 비활성 플러그인 없음
	loaded := Graph(disabled)

	fmt.Printf("  등록된 플러그인 수: %d\n", len(globalRegistry))
	fmt.Printf("  정렬된 초기화 순서:\n")
	for i, r := range loaded {
		deps := "없음"
		if len(r.Requires) > 0 {
			depStrs := make([]string, len(r.Requires))
			for j, t := range r.Requires {
				depStrs[j] = string(t)
			}
			deps = strings.Join(depStrs, ", ")
		}
		fmt.Printf("    %d. %-50s (의존: %s)\n", i+1, r.URI(), deps)
	}

	// 2) 플러그인 초기화
	fmt.Println("\n[2단계] 플러그인 순차 초기화 (InitFn 호출)")
	grpcServer := &SimpleGRPCServer{}
	ttrpcServer := &SimpleTTRPCServer{}
	initialized := NewPluginSet()

	var (
		grpcServices  []grpcService
		ttrpcServices []ttrpcService
		plugins       []*Plugin
	)

	for _, r := range loaded {
		fmt.Printf("  Loading: %-50s", r.URI())

		// InitContext 생성 — 실제로는 rootDir, stateDir, grpcAddress 등 포함
		ic := &InitContext{
			Context:     context.Background(),
			Initialized: initialized,
			Properties: map[string]string{
				"io.containerd.plugin.root":  "/var/lib/containerd/" + r.URI(),
				"io.containerd.plugin.state": "/run/containerd/" + r.URI(),
			},
		}

		// InitFn 호출
		instance, err := r.InitFn(ic)
		p := &Plugin{
			Registration: r,
			instance:     instance,
			err:          err,
		}
		initialized.Add(p)

		if err != nil {
			fmt.Printf(" [SKIP: %v]\n", err)
			continue
		}
		fmt.Printf(" [OK]\n")

		// gRPC/TTRPC 서비스 인터페이스 타입 체크
		// 실제: server.go에서 instance.(grpcService), instance.(ttrpcService) 형변환
		if svc, ok := instance.(grpcService); ok {
			grpcServices = append(grpcServices, svc)
		}
		if svc, ok := instance.(ttrpcService); ok {
			ttrpcServices = append(ttrpcServices, svc)
		}
		plugins = append(plugins, p)
	}

	// 3) 서비스 등록
	fmt.Println("\n[3단계] gRPC/TTRPC 서비스 등록")
	for _, svc := range grpcServices {
		svc.RegisterGRPC(grpcServer)
	}
	for _, svc := range ttrpcServices {
		svc.RegisterTTRPC(ttrpcServer)
	}
	fmt.Printf("  gRPC 서비스: %v\n", grpcServer.services)
	fmt.Printf("  TTRPC 서비스: %v\n", ttrpcServer.services)

	return &Server{
		grpcServer:  grpcServer,
		ttrpcServer: ttrpcServer,
		plugins:     plugins,
	}, nil
}

// =============================================================================
// 11. 4개 리스너 시뮬레이션 (server.go의 ServeGRPC/TTRPC/Metrics/Debug 참조)
// =============================================================================

func (s *Server) Serve(ctx context.Context) error {
	fmt.Println("\n[4단계] 4개 리스너 시작 (실제: goroutine으로 병렬 실행)")
	fmt.Println("  실제 containerd 리스너 구성:")
	fmt.Println("  ┌─────────────┬──────────────────────────────────┬──────────┐")
	fmt.Println("  │ 리스너      │ 주소                             │ 프로토콜 │")
	fmt.Println("  ├─────────────┼──────────────────────────────────┼──────────┤")
	fmt.Println("  │ gRPC        │ /run/containerd/containerd.sock  │ Unix     │")
	fmt.Println("  │ TTRPC       │ /run/containerd/containerd.sock.ttrpc │ Unix │")
	fmt.Println("  │ Metrics     │ 0.0.0.0:1338 (/v1/metrics)      │ TCP/HTTP │")
	fmt.Println("  │ Debug       │ /run/containerd/debug.sock       │ Unix     │")
	fmt.Println("  └─────────────┴──────────────────────────────────┴──────────┘")

	var wg sync.WaitGroup

	// 1) gRPC 리스너 시뮬레이션
	// 실제: s.ServeGRPC(l net.Listener) → s.grpcServer.Serve(l)
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("gRPC listen: %w", err)
	}
	grpcAddr := grpcLn.Addr().String()
	fmt.Printf("\n  [gRPC]    리스닝: %s (실제: Unix 소켓)\n", grpcAddr)

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.grpcServer.Serve(grpcLn)
	}()

	// 2) TTRPC 리스너 시뮬레이션 (로그만)
	// 실제: s.ServeTTRPC(l) → s.ttrpcServer.Serve(ctx, l)
	fmt.Printf("  [TTRPC]   시뮬레이션 (실제: containerd.sock.ttrpc)\n")

	// 3) Metrics 리스너 시뮬레이션
	// 실제: s.ServeMetrics(l) → http.NewServeMux() + /v1/metrics 핸들러
	metricsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		grpcLn.Close()
		return fmt.Errorf("metrics listen: %w", err)
	}
	metricsAddr := metricsLn.Addr().String()
	fmt.Printf("  [Metrics] 리스닝: %s/v1/metrics (Prometheus 엔드포인트)\n", metricsAddr)

	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		// 실제: metrics.Handler() — Prometheus 핸들러
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "# containerd metrics simulation\n")
		fmt.Fprintf(w, "containerd_plugin_count %d\n", len(s.plugins))
		fmt.Fprintf(w, "containerd_grpc_services_count %d\n", len(s.grpcServer.services))
	})
	metricsSrv := &http.Server{Handler: metricsMux, ReadHeaderTimeout: 5 * time.Minute}

	wg.Add(1)
	go func() {
		defer wg.Done()
		metricsSrv.Serve(metricsLn)
	}()

	// 4) Debug 리스너 시뮬레이션
	// 실제: s.ServeDebug(l) → /debug/vars, /debug/pprof/* 핸들러
	debugLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		grpcLn.Close()
		metricsLn.Close()
		return fmt.Errorf("debug listen: %w", err)
	}
	debugAddr := debugLn.Addr().String()
	fmt.Printf("  [Debug]   리스닝: %s/debug/pprof (pprof 엔드포인트)\n", debugAddr)

	debugMux := http.NewServeMux()
	debugMux.HandleFunc("/debug/pprof/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "pprof simulation - endpoint: %s\n", r.URL.Path)
	})
	debugMux.HandleFunc("/debug/vars", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"plugins": len(s.plugins),
		})
	})
	debugSrv := &http.Server{Handler: debugMux, ReadHeaderTimeout: 5 * time.Minute}

	wg.Add(1)
	go func() {
		defer wg.Done()
		debugSrv.Serve(debugLn)
	}()

	// 클라이언트 테스트
	fmt.Println("\n[5단계] 클라이언트 요청 시뮬레이션")
	time.Sleep(100 * time.Millisecond)

	// gRPC 클라이언트 테스트
	fmt.Println("\n  --- gRPC 클라이언트 ---")
	conn, err := net.Dial("tcp", grpcAddr)
	if err == nil {
		conn.Write([]byte("list-services"))
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _ := conn.Read(buf)
		if n > 0 {
			fmt.Printf("  응답: %s\n", string(buf[:n]))
		}
		conn.Close()
	}

	// Metrics 클라이언트 테스트
	fmt.Println("\n  --- Metrics 클라이언트 (/v1/metrics) ---")
	resp, err := http.Get(fmt.Sprintf("http://%s/v1/metrics", metricsAddr))
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("  응답:\n")
		for _, line := range strings.Split(string(body), "\n") {
			if line != "" {
				fmt.Printf("    %s\n", line)
			}
		}
	}

	// Debug 클라이언트 테스트
	fmt.Println("\n  --- Debug 클라이언트 (/debug/vars) ---")
	resp, err = http.Get(fmt.Sprintf("http://%s/debug/vars", debugAddr))
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("  응답: %s\n", strings.TrimSpace(string(body)))
	}

	// 서버 종료
	fmt.Println("\n[6단계] 서버 종료")
	// 실제: Server.Stop() — grpcServer.Stop() 후 플러그인 역순 Close()
	grpcLn.Close()
	metricsLn.Close()
	debugLn.Close()
	metricsSrv.Shutdown(ctx)
	debugSrv.Shutdown(ctx)

	// 플러그인 역순 정리
	// 실제: server.go Stop() — for i := len(s.plugins) - 1; i >= 0; i-- { closer.Close() }
	fmt.Println("  플러그인 역순 종료:")
	for i := len(s.plugins) - 1; i >= 0; i-- {
		fmt.Printf("    Closing: %s\n", s.plugins[i].Registration.URI())
	}

	wg.Wait()
	return nil
}

// =============================================================================
// 12. 의존성 비활성화 데모
// =============================================================================

func demonstrateDisabledFilter() {
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("Graph() 필터링 데모: GC 플러그인 비활성화")
	fmt.Println(strings.Repeat("=", 70))

	// 실제: config.DisabledPlugins에 해당하는 URI를 필터링
	disabled := map[string]bool{
		"io.containerd.gc.v1.scheduler": true,
	}
	filtered := Graph(disabled)
	fmt.Printf("\n  비활성화: %v\n", disabled)
	fmt.Printf("  필터링 후 플러그인 수: %d (원본: %d)\n", len(filtered), len(globalRegistry))
	for i, r := range filtered {
		fmt.Printf("    %d. %s\n", i+1, r.URI())
	}
}

// =============================================================================
// main
// =============================================================================

func main() {
	// 플러그인 등록
	registerAllPlugins()

	// 서버 생성 및 초기화
	srv, err := NewServer()
	if err != nil {
		fmt.Printf("서버 생성 실패: %v\n", err)
		return
	}

	// 서버 실행
	ctx := context.Background()
	if err := srv.Serve(ctx); err != nil {
		fmt.Printf("서버 실행 오류: %v\n", err)
	}

	// 필터 데모
	demonstrateDisabledFilter()

	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("containerd 아키텍처 PoC 완료")
	fmt.Println(strings.Repeat("=", 70))
}
