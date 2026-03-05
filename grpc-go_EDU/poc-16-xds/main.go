// poc-16-xds: xDS 프로토콜 시뮬레이션
//
// gRPC xDS(Envoy Discovery Service) 프로토콜의 핵심 개념을 시뮬레이션한다.
// - xDS 서버: LDS, RDS, CDS, EDS 리소스 제공
// - xDS 클라이언트: 리소스 구독, 업데이트 수신
// - ADS 스트림: 단일 스트림으로 모든 리소스 타입 전달
// - ACK/NACK 메커니즘: 리소스 수락/거부
// - 리소스 타입별 처리 체인
//
// 실제 grpc-go 소스: xds/ 디렉토리
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ========== xDS 리소스 타입 ==========
// Envoy xDS API에서 정의하는 4가지 핵심 리소스 타입.
// 리소스 간에는 의존 관계가 있다: LDS → RDS → CDS → EDS
type ResourceType string

const (
	ListenerResource ResourceType = "LDS" // Listener Discovery Service
	RouteResource    ResourceType = "RDS" // Route Discovery Service
	ClusterResource  ResourceType = "CDS" // Cluster Discovery Service
	EndpointResource ResourceType = "EDS" // Endpoint Discovery Service
)

func (r ResourceType) FullName() string {
	switch r {
	case ListenerResource:
		return "type.googleapis.com/envoy.config.listener.v3.Listener"
	case RouteResource:
		return "type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
	case ClusterResource:
		return "type.googleapis.com/envoy.config.cluster.v3.Cluster"
	case EndpointResource:
		return "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
	default:
		return "unknown"
	}
}

// ========== xDS 리소스 ==========
type Resource struct {
	Name    string
	Type    ResourceType
	Version string
	Data    map[string]interface{} // 리소스 내용 (protobuf 대신 map 사용)
}

func (r *Resource) String() string {
	return fmt.Sprintf("{name=%s, type=%s, version=%s}", r.Name, r.Type, r.Version)
}

// ========== DiscoveryRequest ==========
// xDS 클라이언트 → 서버 요청 (구독/ACK/NACK)
type DiscoveryRequest struct {
	TypeURL       string       // 요청하는 리소스 타입
	ResourceNames []string     // 구독할 리소스 이름
	VersionInfo   string       // 마지막 수락한 버전 (ACK용)
	Nonce         string       // 응답 식별자
	ErrorDetail   string       // NACK 시 에러 메시지
	Node          string       // 클라이언트 식별자
}

func (r *DiscoveryRequest) IsACK() bool {
	return r.VersionInfo != "" && r.ErrorDetail == ""
}

func (r *DiscoveryRequest) IsNACK() bool {
	return r.ErrorDetail != ""
}

// ========== DiscoveryResponse ==========
// xDS 서버 → 클라이언트 응답
type DiscoveryResponse struct {
	TypeURL     string
	VersionInfo string
	Nonce       string
	Resources   []*Resource
}

// ========== xDS 서버 ==========
// 리소스를 관리하고, 클라이언트의 구독에 따라 업데이트를 전달한다.
type XDSServer struct {
	mu           sync.RWMutex
	resources    map[ResourceType]map[string]*Resource // type → name → resource
	versions     map[ResourceType]int                  // type별 현재 버전
	subscribers  map[ResourceType][]chan *DiscoveryResponse
	log          []string
}

func NewXDSServer() *XDSServer {
	s := &XDSServer{
		resources:   make(map[ResourceType]map[string]*Resource),
		versions:    make(map[ResourceType]int),
		subscribers: make(map[ResourceType][]chan *DiscoveryResponse),
	}
	// 리소스 맵 초기화
	for _, rt := range []ResourceType{ListenerResource, RouteResource, ClusterResource, EndpointResource} {
		s.resources[rt] = make(map[string]*Resource)
		s.versions[rt] = 0
	}
	return s
}

// logEvent는 뮤텍스가 이미 잡힌 상태에서 호출된다.
func (s *XDSServer) logEvent(msg string) {
	s.log = append(s.log, fmt.Sprintf("  [서버] %s", msg))
}

// SetResource는 리소스를 추가/업데이트하고 구독자에게 알린다.
func (s *XDSServer) SetResource(rt ResourceType, name string, data map[string]interface{}) {
	s.mu.Lock()

	s.versions[rt]++
	version := fmt.Sprintf("v%d", s.versions[rt])

	resource := &Resource{
		Name:    name,
		Type:    rt,
		Version: version,
		Data:    data,
	}
	s.resources[rt][name] = resource

	// 구독자들에게 알림
	subs := s.subscribers[rt]
	s.logEvent(fmt.Sprintf("리소스 업데이트: %s/%s → %s", rt, name, version))
	s.mu.Unlock()

	// 구독자에게 응답 전송
	resp := &DiscoveryResponse{
		TypeURL:     string(rt),
		VersionInfo: version,
		Nonce:       fmt.Sprintf("nonce-%s-%s", rt, version),
		Resources:   []*Resource{resource},
	}

	for _, ch := range subs {
		select {
		case ch <- resp:
		default:
			// 채널이 꽉 찬 경우 스킵
		}
	}
}

// Subscribe는 리소스 타입에 대한 구독을 시작한다.
func (s *XDSServer) Subscribe(rt ResourceType) chan *DiscoveryResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch := make(chan *DiscoveryResponse, 10)
	s.subscribers[rt] = append(s.subscribers[rt], ch)

	s.logEvent(fmt.Sprintf("구독 추가: %s (총 %d 구독자)", rt, len(s.subscribers[rt])))

	// 기존 리소스가 있으면 즉시 전송
	if len(s.resources[rt]) > 0 {
		resources := make([]*Resource, 0)
		for _, r := range s.resources[rt] {
			resources = append(resources, r)
		}
		version := fmt.Sprintf("v%d", s.versions[rt])
		go func() {
			ch <- &DiscoveryResponse{
				TypeURL:     string(rt),
				VersionInfo: version,
				Nonce:       fmt.Sprintf("nonce-%s-%s", rt, version),
				Resources:   resources,
			}
		}()
	}

	return ch
}

// HandleRequest는 클라이언트 요청을 처리한다 (ACK/NACK).
func (s *XDSServer) HandleRequest(req *DiscoveryRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.IsACK() {
		s.logEvent(fmt.Sprintf("ACK 수신: type=%s, version=%s, nonce=%s",
			req.TypeURL, req.VersionInfo, req.Nonce))
	} else if req.IsNACK() {
		s.logEvent(fmt.Sprintf("NACK 수신: type=%s, error=%s, nonce=%s",
			req.TypeURL, req.ErrorDetail, req.Nonce))
	} else {
		s.logEvent(fmt.Sprintf("초기 구독: type=%s, resources=%v, node=%s",
			req.TypeURL, req.ResourceNames, req.Node))
	}
}

func (s *XDSServer) PrintLog() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.log {
		fmt.Println(entry)
	}
}

// ========== xDS 클라이언트 ==========
type XDSClient struct {
	mu              sync.Mutex
	nodeID          string
	ackedVersions   map[ResourceType]string // 마지막 ACK한 버전
	currentConfigs  map[ResourceType]map[string]*Resource
	log             []string
}

func NewXDSClient(nodeID string) *XDSClient {
	return &XDSClient{
		nodeID:         nodeID,
		ackedVersions:  make(map[ResourceType]string),
		currentConfigs: make(map[ResourceType]map[string]*Resource),
	}
}

func (c *XDSClient) logEvent(msg string) {
	c.mu.Lock()
	c.log = append(c.log, fmt.Sprintf("  [클라이언트:%s] %s", c.nodeID, msg))
	c.mu.Unlock()
}

// WatchResource는 ADS 스트림에서 리소스 업데이트를 수신한다.
func (c *XDSClient) WatchResource(server *XDSServer, rt ResourceType, names []string) {
	// 초기 구독 요청
	server.HandleRequest(&DiscoveryRequest{
		TypeURL:       string(rt),
		ResourceNames: names,
		Node:          c.nodeID,
	})

	ch := server.Subscribe(rt)

	go func() {
		for resp := range ch {
			c.handleResponse(server, resp)
		}
	}()
}

func (c *XDSClient) handleResponse(server *XDSServer, resp *DiscoveryResponse) {
	rt := ResourceType(resp.TypeURL)
	c.logEvent(fmt.Sprintf("응답 수신: type=%s, version=%s, resources=%d개",
		rt, resp.VersionInfo, len(resp.Resources)))

	// 리소스 검증 시뮬레이션
	valid := true
	for _, r := range resp.Resources {
		if err := c.validateResource(r); err != "" {
			c.logEvent(fmt.Sprintf("리소스 검증 실패: %s — %s", r.Name, err))
			valid = false

			// NACK 전송
			server.HandleRequest(&DiscoveryRequest{
				TypeURL:     string(rt),
				VersionInfo: c.ackedVersions[rt], // 이전 버전 유지
				Nonce:       resp.Nonce,
				ErrorDetail: err,
				Node:        c.nodeID,
			})
			return
		}
	}

	if valid {
		// 리소스 적용
		c.mu.Lock()
		if c.currentConfigs[rt] == nil {
			c.currentConfigs[rt] = make(map[string]*Resource)
		}
		for _, r := range resp.Resources {
			c.currentConfigs[rt][r.Name] = r
		}
		c.ackedVersions[rt] = resp.VersionInfo
		c.mu.Unlock()

		c.logEvent(fmt.Sprintf("리소스 적용 완료: type=%s, version=%s", rt, resp.VersionInfo))

		// ACK 전송
		server.HandleRequest(&DiscoveryRequest{
			TypeURL:     string(rt),
			VersionInfo: resp.VersionInfo,
			Nonce:       resp.Nonce,
			Node:        c.nodeID,
		})
	}
}

func (c *XDSClient) validateResource(r *Resource) string {
	// 간단한 검증: "invalid" 키가 있으면 실패
	if _, ok := r.Data["invalid"]; ok {
		return fmt.Sprintf("invalid configuration in %s", r.Name)
	}
	return ""
}

func (c *XDSClient) PrintConfig() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for rt, resources := range c.currentConfigs {
		fmt.Printf("    [%s]\n", rt)
		for name, r := range resources {
			fmt.Printf("      %s (version=%s): %v\n", name, r.Version, r.Data)
		}
	}
}

func (c *XDSClient) PrintLog() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.log {
		fmt.Println(entry)
	}
}

func main() {
	fmt.Println("========================================")
	fmt.Println("xDS 프로토콜 시뮬레이션")
	fmt.Println("========================================")

	// 1. xDS 리소스 계층 구조
	fmt.Println("\n[1] xDS 리소스 계층 구조")
	fmt.Println("─────────────────────────")
	fmt.Println("  LDS (Listener) → RDS (Route) → CDS (Cluster) → EDS (Endpoint)")
	fmt.Println()
	fmt.Println("  LDS: 리스너 설정 (포트, 프로토콜, 필터 체인)")
	fmt.Println("    → 어떤 RouteConfiguration을 사용할지 지정")
	fmt.Println("  RDS: 라우팅 규칙 (호스트 → 클러스터 매핑)")
	fmt.Println("    → 트래픽을 어떤 Cluster로 보낼지 결정")
	fmt.Println("  CDS: 클러스터 설정 (로드밸런싱, 헬스체크)")
	fmt.Println("    → 클러스터의 엔드포인트 목록을 EDS로 위임")
	fmt.Println("  EDS: 엔드포인트 목록 (IP:port, 가중치, 상태)")
	fmt.Println()
	for _, rt := range []ResourceType{ListenerResource, RouteResource, ClusterResource, EndpointResource} {
		fmt.Printf("  %s: %s\n", rt, rt.FullName())
	}

	// 2. 서버/클라이언트 생성
	fmt.Println("\n[2] ADS 스트림 시뮬레이션")
	fmt.Println("──────────────────────────")
	fmt.Println("  ADS(Aggregated Discovery Service):")
	fmt.Println("  단일 gRPC 스트림으로 모든 리소스 타입을 전달한다.")
	fmt.Println("  리소스 간 순서 보장: LDS → RDS → CDS → EDS")

	server := NewXDSServer()
	client := NewXDSClient("node-1")

	// 3. 리소스 구독 및 전달
	fmt.Println("\n[3] 리소스 구독 및 전달")
	fmt.Println("────────────────────────")

	// 초기 리소스 설정
	server.SetResource(ListenerResource, "listener-80", map[string]interface{}{
		"address":             "0.0.0.0:80",
		"route_config_name":   "route-config-1",
		"filter_chain":        []string{"http_connection_manager"},
	})

	server.SetResource(RouteResource, "route-config-1", map[string]interface{}{
		"virtual_hosts": []map[string]interface{}{
			{"domains": []string{"myservice.example.com"}, "cluster": "cluster-1"},
			{"domains": []string{"api.example.com"}, "cluster": "cluster-2"},
		},
	})

	server.SetResource(ClusterResource, "cluster-1", map[string]interface{}{
		"lb_policy":         "ROUND_ROBIN",
		"health_check":      "HTTP /healthz",
		"connect_timeout":   "5s",
	})

	server.SetResource(EndpointResource, "cluster-1", map[string]interface{}{
		"endpoints": []map[string]interface{}{
			{"address": "10.0.0.1:8080", "weight": 3, "health": "HEALTHY"},
			{"address": "10.0.0.2:8080", "weight": 2, "health": "HEALTHY"},
			{"address": "10.0.0.3:8080", "weight": 1, "health": "UNHEALTHY"},
		},
	})

	// 클라이언트 구독 시작
	client.WatchResource(server, ListenerResource, []string{"listener-80"})
	client.WatchResource(server, RouteResource, []string{"route-config-1"})
	client.WatchResource(server, ClusterResource, []string{"cluster-1"})
	client.WatchResource(server, EndpointResource, []string{"cluster-1"})

	time.Sleep(100 * time.Millisecond) // 비동기 처리 대기

	// 4. 이벤트 로그
	fmt.Println("\n[4] 서버 이벤트 로그")
	fmt.Println("─────────────────────")
	server.PrintLog()

	fmt.Println("\n[5] 클라이언트 이벤트 로그")
	fmt.Println("──────────────────────────")
	client.PrintLog()

	// 5. 현재 클라이언트 설정
	fmt.Println("\n[6] 클라이언트 현재 설정")
	fmt.Println("─────────────────────────")
	client.PrintConfig()

	// 6. 리소스 업데이트 (스케일 아웃)
	fmt.Println("\n[7] 리소스 업데이트 — 엔드포인트 스케일 아웃")
	fmt.Println("───────────────────────────────────────────")
	server.SetResource(EndpointResource, "cluster-1", map[string]interface{}{
		"endpoints": []map[string]interface{}{
			{"address": "10.0.0.1:8080", "weight": 3, "health": "HEALTHY"},
			{"address": "10.0.0.2:8080", "weight": 2, "health": "HEALTHY"},
			{"address": "10.0.0.3:8080", "weight": 1, "health": "HEALTHY"}, // 복구됨
			{"address": "10.0.0.4:8080", "weight": 2, "health": "HEALTHY"}, // 새 엔드포인트
		},
	})

	time.Sleep(50 * time.Millisecond)

	fmt.Println("\n  업데이트 후 서버 로그:")
	server.PrintLog()
	fmt.Println("\n  업데이트 후 클라이언트 로그:")
	client.PrintLog()

	// 7. NACK 시뮬레이션 — 잘못된 리소스
	fmt.Println("\n[8] NACK 시뮬레이션 — 잘못된 리소스")
	fmt.Println("─────────────────────────────────────")

	server2 := NewXDSServer()
	client2 := NewXDSClient("node-2")

	// 정상 리소스 먼저 설정
	server2.SetResource(ClusterResource, "bad-cluster", map[string]interface{}{
		"lb_policy": "ROUND_ROBIN",
	})

	client2.WatchResource(server2, ClusterResource, []string{"bad-cluster"})
	time.Sleep(50 * time.Millisecond)

	// 잘못된 리소스로 업데이트
	server2.SetResource(ClusterResource, "bad-cluster", map[string]interface{}{
		"lb_policy": "ROUND_ROBIN",
		"invalid":   true, // 검증 실패를 유발하는 필드
	})

	time.Sleep(50 * time.Millisecond)

	fmt.Println("  서버 로그:")
	server2.PrintLog()
	fmt.Println("\n  클라이언트 로그:")
	client2.PrintLog()

	// 8. ACK/NACK 메커니즘 요약
	fmt.Println("\n[9] ACK/NACK 메커니즘")
	fmt.Println("──────────────────────")
	printACKFlow()

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}

func printACKFlow() {
	lines := []string{
		"  xDS 클라이언트                    xDS 서버 (컨트롤 플레인)",
		"      │                                │",
		"      │── DiscoveryRequest ───────────→│  초기 구독",
		"      │   (type=CDS, names=[cluster-1])│  (version=\"\", nonce=\"\")",
		"      │                                │",
		"      │←── DiscoveryResponse ─────────│  리소스 전달",
		"      │   (version=v1, nonce=abc)      │",
		"      │   resources: [cluster-1 설정]   │",
		"      │                                │",
		"      │── DiscoveryRequest ───────────→│  ACK",
		"      │   (version=v1, nonce=abc)      │  (이전 version 포함 = ACK)",
		"      │                                │",
		"      │←── DiscoveryResponse ─────────│  업데이트",
		"      │   (version=v2, nonce=def)      │",
		"      │   resources: [cluster-1 변경]   │",
		"      │                                │",
		"      │── DiscoveryRequest ───────────→│  NACK",
		"      │   (version=v1, nonce=def,      │  (이전 version + error = NACK)",
		"      │    error=\"invalid config\")     │",
		"      │                                │",
		"      │   ※ NACK 시 서버는 v1을 유지   │",
		"      │   ※ 서버가 수정 후 v3을 전송   │",
	}
	fmt.Println(strings.Join(lines, "\n"))
}
