package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium 서비스 메시 시뮬레이션 (L7 프록시 + xDS)
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/envoy/resources.go       : ListenerTypeURL, RouteTypeURL, ClusterTypeURL 등 리소스 타입
//   - pkg/envoy/xds/cache.go       : Cache (xDS 리소스 캐시, version + TX)
//   - pkg/envoy/xds/server.go      : Server (xDS gRPC 스트림 핸들러)
//   - pkg/envoy/xds/watcher.go     : ResourceWatcher (리소스 변경 감시)
//   - pkg/envoy/xds_server.go      : XDSServer 인터페이스 (AddListener, UpdateNetworkPolicy 등)
//   - pkg/envoy/embedded_envoy.go  : Envoy 프로세스 관리
//   - pkg/proxy/                   : L7 프록시 리다이렉트 관리
//
// xDS 프로토콜:
//   Envoy Discovery Service — 동적 설정 업데이트 프로토콜
//   리소스 타입: Listener(LDS), Route(RDS), Cluster(CDS), Endpoint(EDS)
//   흐름: Cilium → xDS gRPC → Envoy → ACK/NACK

// =============================================================================
// 1. xDS 리소스 타입 — pkg/envoy/resources.go 재현
// =============================================================================

// 리소스 타입 URL
// 실제: pkg/envoy/resources.go에 정의된 상수들
const (
	ListenerTypeURL    = "type.googleapis.com/envoy.config.listener.v3.Listener"
	RouteTypeURL       = "type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
	ClusterTypeURL     = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
	EndpointTypeURL    = "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
	NetworkPolicyURL   = "type.googleapis.com/cilium.NetworkPolicy"
)

// ResourceType은 짧은 이름으로 변환
func ResourceTypeName(typeURL string) string {
	switch typeURL {
	case ListenerTypeURL:
		return "LDS"
	case RouteTypeURL:
		return "RDS"
	case ClusterTypeURL:
		return "CDS"
	case EndpointTypeURL:
		return "EDS"
	case NetworkPolicyURL:
		return "NPol"
	default:
		return "Unknown"
	}
}

// =============================================================================
// 2. xDS 리소스 모델 — Envoy 설정 리소스
// =============================================================================

// Listener는 Envoy 리스너 설정
// 실제: envoy_config_listener.Listener (protobuf)
// Cilium은 pkg/envoy/xds_server.go의 AddListener()에서 리스너를 생성하고
// xDS 캐시를 통해 Envoy에 전달
type Listener struct {
	Name           string
	Address        string
	Port           uint16
	FilterChains   []FilterChain
	IsIngress      bool
}

// FilterChain은 리스너의 필터 체인
type FilterChain struct {
	Filters []Filter
}

// Filter는 네트워크 필터 (TCP proxy, HTTP connection manager 등)
type Filter struct {
	Name   string
	Config map[string]string
}

// Route는 HTTP 라우트 설정
// 실제: envoy_config_route.RouteConfiguration (protobuf)
type Route struct {
	Name         string
	VirtualHosts []VirtualHost
}

// VirtualHost는 가상 호스트 설정
type VirtualHost struct {
	Name    string
	Domains []string
	Routes  []RouteEntry
}

// RouteEntry는 라우트 엔트리
type RouteEntry struct {
	Match   string // prefix, path, regex
	Cluster string // 대상 클러스터
}

// Cluster는 업스트림 클러스터 설정
// 실제: envoy_config_cluster.Cluster (protobuf)
type Cluster struct {
	Name           string
	LBPolicy       string // ROUND_ROBIN, RING_HASH 등
	ConnectTimeout time.Duration
}

// Endpoint는 클러스터의 엔드포인트
// 실제: envoy_config_endpoint.ClusterLoadAssignment (protobuf)
type Endpoint struct {
	ClusterName string
	Addresses   []EndpointAddress
}

// EndpointAddress는 엔드포인트 주소
type EndpointAddress struct {
	Address string
	Port    uint16
	Weight  uint32
	Healthy bool
}

// =============================================================================
// 3. xDS Cache — pkg/envoy/xds/cache.go 재현
// =============================================================================
//
// Cache는 리소스를 저장하고 버전 관리하는 구조체
// 실제: pkg/envoy/xds/cache.go의 Cache
//   - 리소스는 (typeURL, name) 쌍으로 식별
//   - TX() 메서드로 원자적 업데이트 (upsert + delete → version bump)
//   - 변경 시 옵저버에게 통지

// Resource는 xDS 리소스의 일반 표현
type Resource struct {
	TypeURL  string
	Name     string
	Value    interface{} // 실제: proto.Message
	Version  uint64
}

// cacheKey는 리소스의 고유 키
// 실제: pkg/envoy/xds/cache.go의 cacheKey
type cacheKey struct {
	typeURL      string
	resourceName string
}

// Cache는 xDS 리소스 캐시
// 실제: pkg/envoy/xds/cache.go의 Cache
type Cache struct {
	mu        sync.RWMutex
	resources map[cacheKey]Resource
	version   uint64
	observers []ResourceObserver
}

// ResourceObserver는 리소스 변경을 감시하는 인터페이스
// 실제: pkg/envoy/xds/watcher.go의 ResourceVersionObserver
type ResourceObserver interface {
	OnResourceUpdate(typeURL string, version uint64)
}

func NewCache() *Cache {
	return &Cache{
		resources: make(map[cacheKey]Resource),
		version:   1,
	}
}

// TX는 원자적으로 리소스를 업데이트
// 실제: pkg/envoy/xds/cache.go의 TX()
// upsert + delete → 변경이 있으면 version bump → 옵저버 통지
func (c *Cache) TX(typeURL string, upserted map[string]interface{}, deleted []string) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	updated := false
	newVersion := c.version + 1

	// Upsert
	for name, value := range upserted {
		key := cacheKey{typeURL: typeURL, resourceName: name}
		c.resources[key] = Resource{
			TypeURL: typeURL,
			Name:    name,
			Value:   value,
			Version: newVersion,
		}
		updated = true
	}

	// Delete
	for _, name := range deleted {
		key := cacheKey{typeURL: typeURL, resourceName: name}
		if _, exists := c.resources[key]; exists {
			delete(c.resources, key)
			updated = true
		}
	}

	if updated {
		c.version = newVersion
		// 옵저버 통지
		for _, obs := range c.observers {
			obs.OnResourceUpdate(typeURL, newVersion)
		}
	}

	return c.version, updated
}

// GetResources는 특정 타입의 모든 리소스를 반환
func (c *Cache) GetResources(typeURL string) []Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var resources []Resource
	for key, res := range c.resources {
		if key.typeURL == typeURL {
			resources = append(resources, res)
		}
	}
	return resources
}

// GetResource는 특정 리소스를 반환
func (c *Cache) GetResource(typeURL, name string) (Resource, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res, exists := c.resources[cacheKey{typeURL: typeURL, resourceName: name}]
	return res, exists
}

// Version은 현재 캐시 버전을 반환
func (c *Cache) Version() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

// AddObserver는 옵저버를 등록
func (c *Cache) AddObserver(obs ResourceObserver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observers = append(c.observers, obs)
}

// =============================================================================
// 4. xDS Server — pkg/envoy/xds/server.go 재현
// =============================================================================

// DiscoveryRequest는 Envoy의 리소스 요청
// 실제: envoy_service_discovery.DiscoveryRequest (protobuf)
type DiscoveryRequest struct {
	TypeURL       string
	VersionInfo   uint64   // 마지막으로 받은 버전
	ResourceNames []string // 요청하는 리소스 이름
	ResponseNonce uint64
	Node          string   // 요청 노드 식별자
	ErrorDetail   string   // NACK인 경우 에러 상세
}

// DiscoveryResponse는 xDS 서버의 응답
// 실제: envoy_service_discovery.DiscoveryResponse (protobuf)
type DiscoveryResponse struct {
	TypeURL     string
	VersionInfo uint64
	Resources   []Resource
	Nonce       uint64
}

// XDSServer는 xDS gRPC 서버
// 실제: pkg/envoy/xds/server.go의 Server
type XDSServer struct {
	cache    *Cache
	lastNonce uint64
	mu       sync.Mutex
	streams  map[string]*xdsStream // 연결된 Envoy 스트림
}

// xdsStream은 하나의 Envoy 연결을 나타냄
type xdsStream struct {
	node       string
	lastNonce  uint64
	ackedVer   map[string]uint64 // typeURL → acked version
}

func NewXDSServer(cache *Cache) *XDSServer {
	return &XDSServer{
		cache:   cache,
		streams: make(map[string]*xdsStream),
	}
}

// HandleRequest는 DiscoveryRequest를 처리하고 응답을 반환
// 실제: pkg/envoy/xds/server.go의 HandleRequestStream()
// ACK/NACK 판별: ResponseNonce가 유효하고 ErrorDetail이 비어있으면 ACK
func (s *XDSServer) HandleRequest(req DiscoveryRequest) (*DiscoveryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 스트림 등록/업데이트
	stream, exists := s.streams[req.Node]
	if !exists {
		stream = &xdsStream{
			node:     req.Node,
			ackedVer: make(map[string]uint64),
		}
		s.streams[req.Node] = stream
	}

	// ACK 처리
	if req.ResponseNonce > 0 {
		if req.ErrorDetail == "" {
			// ACK — 이전 응답이 성공적으로 적용됨
			stream.ackedVer[req.TypeURL] = req.VersionInfo
		}
		// NACK — 이전 응답 적용 실패 (ErrorDetail에 이유 포함)
	}

	// 응답 생성
	currentVersion := s.cache.Version()
	if req.VersionInfo >= currentVersion {
		// 이미 최신 버전 — 새 버전이 나올 때까지 대기 (여기서는 nil 반환)
		return nil, nil
	}

	resources := s.cache.GetResources(req.TypeURL)
	s.lastNonce++

	return &DiscoveryResponse{
		TypeURL:     req.TypeURL,
		VersionInfo: currentVersion,
		Resources:   resources,
		Nonce:       s.lastNonce,
	}, nil
}

// =============================================================================
// 5. L7 프록시 리다이렉트 — tc→proxy→tc 패턴
// =============================================================================
//
// 실제 흐름:
//   1. BPF (tc ingress) → 패킷이 L7 정책에 해당하면 프록시 포트로 리다이렉트
//   2. Envoy (L7 proxy) → HTTP 파싱, 정책 검사, 요청 수정
//   3. BPF (tc egress) → 프록시에서 나온 패킷을 원래 목적지로 전달
//
// 프록시 포트 할당:
//   pkg/proxy/ 패키지가 Envoy 리스너에 포트를 할당
//   BPF 맵에 (identity, port) → proxyPort 매핑 저장

// ProxyRedirect는 L7 프록시 리다이렉트 설정
// 실제: pkg/loadbalancer/service.go의 ProxyRedirect
type ProxyRedirect struct {
	ListenerName string
	ProxyPort    uint16
	Direction    string // "ingress" 또는 "egress"
}

// Packet은 네트워크 패킷을 시뮬레이션
type Packet struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol string
	Method   string // HTTP 메서드
	Path     string // HTTP 경로
	Host     string // HTTP Host 헤더
}

// ProxyEngine은 L7 프록시 엔진을 시뮬레이션
type ProxyEngine struct {
	redirects map[uint16]*ProxyRedirect // proxyPort → redirect 설정
	xdsServer *XDSServer
	listeners map[string]*Listener
}

func NewProxyEngine(xds *XDSServer) *ProxyEngine {
	return &ProxyEngine{
		redirects: make(map[uint16]*ProxyRedirect),
		xdsServer: xds,
		listeners: make(map[string]*Listener),
	}
}

// AddRedirect는 프록시 리다이렉트를 추가
func (pe *ProxyEngine) AddRedirect(redirect *ProxyRedirect) {
	pe.redirects[redirect.ProxyPort] = redirect
}

// ProcessPacket은 패킷을 L7에서 처리
// 실제 흐름: tc ingress BPF → TPROXY → Envoy → tc egress BPF
func (pe *ProxyEngine) ProcessPacket(pkt Packet) (string, bool) {
	// L7 정책 검사 시뮬레이션
	// 실제: Envoy가 NetworkPolicy를 기반으로 판정

	// 라우트 매칭
	routes := pe.xdsServer.cache.GetResources(RouteTypeURL)
	for _, res := range routes {
		route := res.Value.(*Route)
		for _, vh := range route.VirtualHosts {
			for _, domain := range vh.Domains {
				if domain == "*" || domain == pkt.Host {
					for _, re := range vh.Routes {
						if strings.HasPrefix(pkt.Path, re.Match) {
							return fmt.Sprintf("→ Route match: %s → cluster=%s", re.Match, re.Cluster), true
						}
					}
				}
			}
		}
	}
	return "→ No route match", false
}

// =============================================================================
// 6. XDSServer 고수준 인터페이스 — pkg/envoy/xds_server.go 재현
// =============================================================================

// CiliumXDSServer는 Cilium의 고수준 xDS 서버
// 실제: pkg/envoy/xds_server.go의 XDSServer 인터페이스 + xdsServer 구현
type CiliumXDSServer struct {
	cache       *Cache
	server      *XDSServer
	proxyEngine *ProxyEngine
	nextPort    uint16
}

func NewCiliumXDSServer() *CiliumXDSServer {
	cache := NewCache()
	server := NewXDSServer(cache)
	return &CiliumXDSServer{
		cache:       cache,
		server:      server,
		proxyEngine: NewProxyEngine(server),
		nextPort:    10000,
	}
}

// AddListener는 Envoy 리스너를 추가
// 실제: pkg/envoy/xds_server.go의 AddListener()
func (s *CiliumXDSServer) AddListener(name string, port uint16, isIngress bool) uint16 {
	listener := &Listener{
		Name:      name,
		Address:   "0.0.0.0",
		Port:      port,
		IsIngress: isIngress,
		FilterChains: []FilterChain{
			{Filters: []Filter{
				{Name: "envoy.filters.network.http_connection_manager",
					Config: map[string]string{"route_config": name + "-route"}},
			}},
		},
	}

	// xDS 캐시에 리스너 추가
	s.cache.TX(ListenerTypeURL, map[string]interface{}{name: listener}, nil)

	// 프록시 리다이렉트 등록
	proxyPort := s.nextPort
	s.nextPort++
	s.proxyEngine.AddRedirect(&ProxyRedirect{
		ListenerName: name,
		ProxyPort:    proxyPort,
		Direction: func() string {
			if isIngress {
				return "ingress"
			}
			return "egress"
		}(),
	})

	return proxyPort
}

// AddRoute는 라우트 설정을 추가
func (s *CiliumXDSServer) AddRoute(name string, route *Route) {
	s.cache.TX(RouteTypeURL, map[string]interface{}{name: route}, nil)
}

// AddCluster는 클러스터 설정을 추가
func (s *CiliumXDSServer) AddCluster(name string, cluster *Cluster) {
	s.cache.TX(ClusterTypeURL, map[string]interface{}{name: cluster}, nil)
}

// AddEndpoint는 엔드포인트를 추가
func (s *CiliumXDSServer) AddEndpoint(name string, endpoint *Endpoint) {
	s.cache.TX(EndpointTypeURL, map[string]interface{}{name: endpoint}, nil)
}

// UpdateEndpoint는 엔드포인트를 업데이트 (핫 리로드)
func (s *CiliumXDSServer) UpdateEndpoint(name string, endpoint *Endpoint) {
	s.cache.TX(EndpointTypeURL, map[string]interface{}{name: endpoint}, nil)
}

// RemoveListener는 리스너를 제거
func (s *CiliumXDSServer) RemoveListener(name string) {
	s.cache.TX(ListenerTypeURL, nil, []string{name})
}

// =============================================================================
// 7. 데모 실행
// =============================================================================

func printSep(title string) {
	fmt.Printf("\n━━━ %s ━━━\n\n", title)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Cilium 서비스 메시 시뮬레이션 (L7 프록시 + xDS)            ║")
	fmt.Println("║  소스: pkg/envoy/, pkg/envoy/xds/, pkg/proxy/              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	xds := NewCiliumXDSServer()

	// =========================================================================
	// 데모 1: xDS 리소스 디스커버리 프로토콜
	// =========================================================================
	printSep("데모 1: xDS 리소스 디스커버리 프로토콜 (LDS/RDS/CDS/EDS)")

	fmt.Println("  xDS 리소스 타입 (실제: pkg/envoy/resources.go):")
	typeURLs := []string{ListenerTypeURL, RouteTypeURL, ClusterTypeURL, EndpointTypeURL, NetworkPolicyURL}
	for _, url := range typeURLs {
		fmt.Printf("    %s ← %s\n", ResourceTypeName(url), url)
	}

	fmt.Println("\n  xDS 프로토콜 흐름:")
	fmt.Println("    Envoy → DiscoveryRequest(typeURL, version=0)")
	fmt.Println("    Cilium → DiscoveryResponse(resources, version=1, nonce=1)")
	fmt.Println("    Envoy → DiscoveryRequest(version=1, nonce=1)  ← ACK")
	fmt.Println("    Envoy → DiscoveryRequest(version=0, nonce=1, error='...')  ← NACK")

	// =========================================================================
	// 데모 2: 리스너, 라우트, 클러스터, 엔드포인트 설정
	// =========================================================================
	printSep("데모 2: 리스너, 라우트, 클러스터, 엔드포인트 설정")

	// 리스너 추가
	proxyPort := xds.AddListener("ingress-http", 80, true)
	fmt.Printf("  [LDS] 리스너 추가: ingress-http:80 (proxyPort=%d)\n", proxyPort)

	// 라우트 추가
	xds.AddRoute("ingress-http-route", &Route{
		Name: "ingress-http-route",
		VirtualHosts: []VirtualHost{
			{
				Name:    "web-service",
				Domains: []string{"web.default.svc.cluster.local", "*"},
				Routes: []RouteEntry{
					{Match: "/api/", Cluster: "api-cluster"},
					{Match: "/static/", Cluster: "static-cluster"},
					{Match: "/", Cluster: "default-cluster"},
				},
			},
		},
	})
	fmt.Println("  [RDS] 라우트 추가: /api/ → api-cluster, /static/ → static-cluster")

	// 클러스터 추가
	xds.AddCluster("api-cluster", &Cluster{
		Name:           "api-cluster",
		LBPolicy:       "ROUND_ROBIN",
		ConnectTimeout: 5 * time.Second,
	})
	xds.AddCluster("static-cluster", &Cluster{
		Name:           "static-cluster",
		LBPolicy:       "ROUND_ROBIN",
		ConnectTimeout: 3 * time.Second,
	})
	xds.AddCluster("default-cluster", &Cluster{
		Name:           "default-cluster",
		LBPolicy:       "ROUND_ROBIN",
		ConnectTimeout: 5 * time.Second,
	})
	fmt.Println("  [CDS] 클러스터 추가: api-cluster, static-cluster, default-cluster")

	// 엔드포인트 추가
	xds.AddEndpoint("api-cluster", &Endpoint{
		ClusterName: "api-cluster",
		Addresses: []EndpointAddress{
			{Address: "10.0.1.1", Port: 8080, Weight: 100, Healthy: true},
			{Address: "10.0.1.2", Port: 8080, Weight: 100, Healthy: true},
		},
	})
	xds.AddEndpoint("static-cluster", &Endpoint{
		ClusterName: "static-cluster",
		Addresses: []EndpointAddress{
			{Address: "10.0.2.1", Port: 8081, Weight: 100, Healthy: true},
		},
	})
	fmt.Println("  [EDS] 엔드포인트 추가: api-cluster(2개), static-cluster(1개)")
	fmt.Printf("\n  캐시 버전: %d\n", xds.cache.Version())

	// =========================================================================
	// 데모 3: xDS 스트리밍 — 요청/응답/ACK 흐름
	// =========================================================================
	printSep("데모 3: xDS 스트리밍 — 요청/응답/ACK 흐름")

	fmt.Println("  [Envoy] 초기 LDS 요청 (version=0)")
	resp, _ := xds.server.HandleRequest(DiscoveryRequest{
		TypeURL:     ListenerTypeURL,
		VersionInfo: 0,
		Node:        "envoy-sidecar-1",
	})
	if resp != nil {
		fmt.Printf("  [Cilium] LDS 응답: version=%d, nonce=%d, resources=%d\n",
			resp.VersionInfo, resp.Nonce, len(resp.Resources))
	}

	fmt.Println("\n  [Envoy] ACK (version=응답버전, nonce=응답nonce)")
	resp2, _ := xds.server.HandleRequest(DiscoveryRequest{
		TypeURL:       ListenerTypeURL,
		VersionInfo:   resp.VersionInfo,
		ResponseNonce: resp.Nonce,
		Node:          "envoy-sidecar-1",
	})
	if resp2 == nil {
		fmt.Println("  [Cilium] 이미 최신 — 대기 (long-poll)")
	}

	fmt.Println("\n  [Envoy] RDS 요청 (version=0)")
	rdsResp, _ := xds.server.HandleRequest(DiscoveryRequest{
		TypeURL:     RouteTypeURL,
		VersionInfo: 0,
		Node:        "envoy-sidecar-1",
	})
	if rdsResp != nil {
		fmt.Printf("  [Cilium] RDS 응답: version=%d, resources=%d\n",
			rdsResp.VersionInfo, len(rdsResp.Resources))
	}

	// NACK 시뮬레이션
	fmt.Println("\n  [Envoy] NACK 시뮬레이션 (잘못된 설정 적용 실패)")
	xds.server.HandleRequest(DiscoveryRequest{
		TypeURL:       RouteTypeURL,
		VersionInfo:   0, // 이전 버전 유지
		ResponseNonce: rdsResp.Nonce,
		Node:          "envoy-sidecar-1",
		ErrorDetail:   "invalid route configuration: unknown cluster 'missing-cluster'",
	})
	fmt.Println("  [Cilium] NACK 수신 — 이전 설정 유지, 에러 기록")

	// =========================================================================
	// 데모 4: L7 프록시 리다이렉트 (tc→proxy→tc 패턴)
	// =========================================================================
	printSep("데모 4: L7 프록시 리다이렉트 (tc→proxy→tc 패턴)")

	fmt.Println("  실제 패킷 흐름:")
	fmt.Println("    ┌─────────┐     ┌──────────┐     ┌─────────┐")
	fmt.Println("    │tc ingress│ ──→ │Envoy L7  │ ──→ │tc egress│ ──→ Backend")
	fmt.Println("    │  (BPF)  │     │  Proxy   │     │  (BPF)  │")
	fmt.Println("    └─────────┘     └──────────┘     └─────────┘")
	fmt.Println("         │              │                  │")
	fmt.Println("    identity+port  HTTP parse+policy   DNAT to backend")
	fmt.Println("    → proxy port   → allow/deny")
	fmt.Println()

	packets := []Packet{
		{SrcIP: "10.0.0.100", DstIP: "10.96.0.1", DstPort: 80, Protocol: "TCP",
			Method: "GET", Path: "/api/users", Host: "web.default.svc.cluster.local"},
		{SrcIP: "10.0.0.101", DstIP: "10.96.0.1", DstPort: 80, Protocol: "TCP",
			Method: "GET", Path: "/static/style.css", Host: "web.default.svc.cluster.local"},
		{SrcIP: "10.0.0.102", DstIP: "10.96.0.1", DstPort: 80, Protocol: "TCP",
			Method: "GET", Path: "/health", Host: "web.default.svc.cluster.local"},
		{SrcIP: "10.0.0.103", DstIP: "10.96.0.1", DstPort: 80, Protocol: "TCP",
			Method: "DELETE", Path: "/api/admin", Host: "web.default.svc.cluster.local"},
	}

	fmt.Println("  L7 패킷 처리:")
	for _, pkt := range packets {
		result, matched := xds.proxyEngine.ProcessPacket(pkt)
		status := "ALLOW"
		if !matched {
			status = "NO MATCH"
		}
		fmt.Printf("    %s %s %-20s → %s [%s]\n",
			pkt.Method, pkt.Host, pkt.Path, result, status)
	}

	// =========================================================================
	// 데모 5: 핫 리로드 — 엔드포인트 업데이트
	// =========================================================================
	printSep("데모 5: 핫 리로드 — 엔드포인트 동적 업데이트")

	fmt.Println("  [이전] api-cluster 엔드포인트:")
	for _, res := range xds.cache.GetResources(EndpointTypeURL) {
		ep := res.Value.(*Endpoint)
		if ep.ClusterName == "api-cluster" {
			for _, addr := range ep.Addresses {
				fmt.Printf("    %s:%d (weight=%d, healthy=%v)\n",
					addr.Address, addr.Port, addr.Weight, addr.Healthy)
			}
		}
	}

	// 엔드포인트 업데이트 (새 포드 추가 + 기존 포드 unhealthy)
	versionBefore := xds.cache.Version()
	xds.UpdateEndpoint("api-cluster", &Endpoint{
		ClusterName: "api-cluster",
		Addresses: []EndpointAddress{
			{Address: "10.0.1.1", Port: 8080, Weight: 100, Healthy: true},
			{Address: "10.0.1.2", Port: 8080, Weight: 100, Healthy: false}, // unhealthy
			{Address: "10.0.1.3", Port: 8080, Weight: 100, Healthy: true},  // 새 포드
		},
	})
	versionAfter := xds.cache.Version()

	fmt.Println("\n  [이후] api-cluster 엔드포인트:")
	for _, res := range xds.cache.GetResources(EndpointTypeURL) {
		ep := res.Value.(*Endpoint)
		if ep.ClusterName == "api-cluster" {
			for _, addr := range ep.Addresses {
				status := ""
				if !addr.Healthy {
					status = " ← UNHEALTHY"
				}
				if addr.Address == "10.0.1.3" {
					status = " ← NEW"
				}
				fmt.Printf("    %s:%d (weight=%d, healthy=%v)%s\n",
					addr.Address, addr.Port, addr.Weight, addr.Healthy, status)
			}
		}
	}
	fmt.Printf("\n  캐시 버전: %d → %d (자동 bump)\n", versionBefore, versionAfter)

	// Envoy가 변경을 감지하고 요청
	fmt.Println("\n  [Envoy] EDS 업데이트 감지 (long-poll 응답)")
	edsResp, _ := xds.server.HandleRequest(DiscoveryRequest{
		TypeURL:     EndpointTypeURL,
		VersionInfo: versionBefore,
		Node:        "envoy-sidecar-1",
	})
	if edsResp != nil {
		fmt.Printf("  [Cilium] EDS 응답: version=%d, nonce=%d\n", edsResp.VersionInfo, edsResp.Nonce)
		fmt.Println("  [Envoy] ACK — 새 엔드포인트 적용 완료")
	}

	// =========================================================================
	// 데모 6: 설정 업데이트 시뮬레이션 — 리스너 추가/제거
	// =========================================================================
	printSep("데모 6: 설정 업데이트 — 리스너 추가/제거")

	// 새 리스너 추가
	proxyPort2 := xds.AddListener("egress-http", 15001, false)
	fmt.Printf("  [추가] egress-http:15001 (proxyPort=%d)\n", proxyPort2)

	listeners := xds.cache.GetResources(ListenerTypeURL)
	fmt.Printf("  현재 리스너 수: %d\n", len(listeners))
	for _, res := range listeners {
		l := res.Value.(*Listener)
		dir := "ingress"
		if !l.IsIngress {
			dir = "egress"
		}
		fmt.Printf("    %s (%s, port=%d)\n", l.Name, dir, l.Port)
	}

	// 리스너 제거
	xds.RemoveListener("egress-http")
	fmt.Println("\n  [제거] egress-http")
	listeners = xds.cache.GetResources(ListenerTypeURL)
	fmt.Printf("  현재 리스너 수: %d\n", len(listeners))

	// =========================================================================
	// 데모 7: 부하 시뮬레이션
	// =========================================================================
	printSep("데모 7: L7 라우팅 부하 시뮬레이션")

	paths := []string{"/api/users", "/api/products", "/static/img.png", "/health", "/api/orders", "/about"}
	routeDistribution := make(map[string]int)

	for i := 0; i < 1000; i++ {
		p := paths[rand.Intn(len(paths))]
		pkt := Packet{
			Method: "GET",
			Path:   p,
			Host:   "web.default.svc.cluster.local",
		}
		result, _ := xds.proxyEngine.ProcessPacket(pkt)
		routeDistribution[result]++
	}

	fmt.Println("  1000개 요청 라우팅 분포:")
	for route, count := range routeDistribution {
		bar := strings.Repeat("█", count/20)
		fmt.Printf("    %-50s: %4d %s\n", route, count, bar)
	}

	// =========================================================================
	// 요약
	// =========================================================================
	printSep("요약")

	fmt.Println("  핵심 흐름:")
	fmt.Println("    1. Cilium이 xDS Cache에 리소스(LDS/RDS/CDS/EDS) 저장")
	fmt.Println("    2. Envoy가 xDS gRPC 스트림으로 리소스 구독")
	fmt.Println("    3. Cache 변경 → version bump → Envoy에 push")
	fmt.Println("    4. Envoy가 ACK/NACK으로 적용 확인")
	fmt.Println("    5. BPF tc가 L7 트래픽을 Envoy 프록시 포트로 리다이렉트")
	fmt.Println()
	fmt.Println("  설계 포인트:")
	fmt.Println("    - Cache.TX()로 원자적 업데이트 (upsert+delete 한 번에)")
	fmt.Println("    - 리소스 값이 동일하면 version bump 안 함 (불필요한 push 방지)")
	fmt.Println("    - Nonce 기반 ACK/NACK으로 동시 업데이트 추적")
	fmt.Println("    - 프록시 리다이렉트는 BPF 맵으로 O(1) 판정")
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════")
}
