package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Istio Envoy Config Generation 시뮬레이션
//
// 실제 소스 참조:
//   - pilot/pkg/networking/core/configgen.go     → ConfigGenerator 인터페이스
//   - pilot/pkg/networking/core/cluster.go       → BuildClusters, BuildSubsetKey
//   - pilot/pkg/networking/core/httproute.go     → BuildHTTPRoutes
//   - pilot/pkg/networking/core/listener.go      → BuildListeners
//   - pilot/pkg/model/service.go                 → BuildSubsetKey, ParseSubsetKey
//
// Istio는 Kubernetes Service, VirtualService, DestinationRule 등의
// 사용자 설정을 Envoy가 이해할 수 있는 xDS 리소스(CDS, RDS, LDS)로 변환한다.
// =============================================================================

// --- Istio 데이터 모델 ---

// TrafficDirection은 트래픽 방향을 나타낸다.
type TrafficDirection string

const (
	TrafficDirectionOutbound TrafficDirection = "outbound"
	TrafficDirectionInbound  TrafficDirection = "inbound"
)

// Service는 Kubernetes Service를 나타낸다.
// 실제 Istio의 model.Service 구조체의 핵심 필드를 반영한다.
type Service struct {
	Hostname   string            // FQDN (예: "reviews.default.svc.cluster.local")
	Name       string            // 서비스 이름
	Namespace  string            // 네임스페이스
	Ports      []*Port           // 서비스 포트 목록
	Resolution Resolution        // 엔드포인트 해석 방식
	Labels     map[string]string // 서비스 레이블
}

// Port는 서비스 포트를 나타낸다.
type Port struct {
	Name     string // 포트 이름
	Port     int    // 서비스 포트 번호
	Protocol string // 프로토콜 (HTTP, TCP, gRPC 등)
}

// Resolution은 서비스의 엔드포인트 해석 방식을 정의한다.
type Resolution int

const (
	ClientSideLB    Resolution = iota // EDS: Pilot이 엔드포인트를 관리
	DNSLB                            // STRICT_DNS: DNS로 해석
	DNSRoundRobinLB                  // LOGICAL_DNS
	Passthrough                      // ORIGINAL_DST: 원본 목적지 사용
)

// VirtualService는 Istio의 트래픽 라우팅 규칙이다.
// HTTP 요청을 조건에 따라 다른 destination으로 라우팅한다.
type VirtualService struct {
	Name      string
	Namespace string
	Hosts     []string         // 적용 대상 호스트
	HTTP      []*HTTPRoute     // HTTP 라우팅 규칙
	Gateways  []string         // 적용 대상 Gateway (mesh = 사이드카)
}

// HTTPRoute는 HTTP 라우팅 규칙 하나를 나타낸다.
type HTTPRoute struct {
	Name    string              // 라우트 이름
	Match   []*HTTPMatchRequest // 매칭 조건
	Route   []*HTTPRouteDestination // 목적지와 가중치
	Timeout string              // 타임아웃
	Retries *HTTPRetry          // 재시도 정책
}

// HTTPMatchRequest는 HTTP 요청 매칭 조건이다.
type HTTPMatchRequest struct {
	URI     *StringMatch       // URI 매칭
	Headers map[string]*StringMatch // 헤더 매칭
}

// StringMatch는 문자열 매칭 조건이다.
type StringMatch struct {
	MatchType string // "exact", "prefix", "regex"
	Value     string
}

// HTTPRouteDestination은 라우팅 목적지와 가중치이다.
type HTTPRouteDestination struct {
	Destination *Destination
	Weight      int // 가중치 (퍼센트)
}

// Destination은 트래픽 목적지이다.
type Destination struct {
	Host   string // 서비스 호스트명
	Subset string // DestinationRule의 subset 이름
	Port   int    // 포트 번호
}

// HTTPRetry는 재시도 정책이다.
type HTTPRetry struct {
	Attempts int
	PerTryTimeout string
}

// DestinationRule은 서비스에 대한 트래픽 정책과 subset을 정의한다.
type DestinationRule struct {
	Name          string
	Namespace     string
	Host          string             // 적용 대상 호스트
	TrafficPolicy *TrafficPolicy     // 기본 트래픽 정책
	Subsets       []*Subset          // 서비스의 부분 집합
}

// TrafficPolicy는 트래픽 정책이다.
type TrafficPolicy struct {
	ConnectionPool *ConnectionPool
	LoadBalancer   string // ROUND_ROBIN, LEAST_CONN, RANDOM
}

// ConnectionPool은 연결 풀 설정이다.
type ConnectionPool struct {
	MaxConnections int
}

// Subset은 DestinationRule 내의 서비스 부분 집합이다.
// 레이블 셀렉터로 엔드포인트를 분류한다.
type Subset struct {
	Name          string
	Labels        map[string]string
	TrafficPolicy *TrafficPolicy
}

// --- Envoy xDS 리소스 모델 ---

// Cluster는 Envoy의 CDS 리소스이다.
// 실제로는 envoy.config.cluster.v3.Cluster protobuf 메시지이다.
type Cluster struct {
	Name           string   // 클러스터 이름 (예: "outbound|8080|v1|reviews.default.svc.cluster.local")
	DiscoveryType  string   // EDS, STRICT_DNS, LOGICAL_DNS, ORIGINAL_DST
	LoadBalancer   string   // 로드밸런서 알고리즘
	MaxConnections int      // 최대 연결 수
	Endpoints      []string // DNS 클러스터의 경우 엔드포인트 주소
}

// Route는 Envoy의 RDS 리소스(RouteConfiguration) 내의 라우트이다.
type Route struct {
	Name         string         // 라우트 이름
	Match        *RouteMatch    // 매칭 조건
	ClusterName  string         // 단일 목적지 클러스터
	WeightedClusters []*WeightedCluster // 가중치 기반 다중 목적지
}

// RouteMatch는 라우트 매칭 조건이다.
type RouteMatch struct {
	Prefix  string
	Path    string
	Headers map[string]string
}

// WeightedCluster는 가중치 기반 클러스터 선택이다.
type WeightedCluster struct {
	ClusterName string
	Weight      int
}

// RouteConfiguration은 Envoy RDS 응답이다.
type RouteConfiguration struct {
	Name         string    // 라우트 설정 이름
	VirtualHosts []*VirtualHost
}

// VirtualHost는 Envoy의 가상 호스트이다.
type VirtualHost struct {
	Name    string
	Domains []string
	Routes  []*Route
}

// Listener는 Envoy의 LDS 리소스이다.
type Listener struct {
	Name           string
	Address        string
	Port           int
	FilterChains   []*FilterChain
	RouteConfigName string // RDS 참조
}

// FilterChain은 Envoy의 필터 체인이다.
type FilterChain struct {
	Filters []string // 적용되는 필터 목록
}

// --- BuildSubsetKey: 클러스터 이름 생성 ---

// BuildSubsetKey는 Envoy 클러스터 이름을 생성한다.
// 형식: "direction|port|subset|hostname"
// 예: "outbound|8080|v1|reviews.default.svc.cluster.local"
//
// 실제 소스: pilot/pkg/model/service.go의 BuildSubsetKey 함수
func BuildSubsetKey(direction TrafficDirection, subsetName string, hostname string, port int) string {
	return fmt.Sprintf("%s|%d|%s|%s", direction, port, subsetName, hostname)
}

// ParseSubsetKey는 클러스터 이름을 파싱한다.
// 실제 소스: pilot/pkg/model/service.go의 ParseSubsetKey 함수
func ParseSubsetKey(key string) (direction, subset, hostname string, port int) {
	parts := strings.Split(key, "|")
	if len(parts) != 4 {
		return
	}
	direction = parts[0]
	fmt.Sscanf(parts[1], "%d", &port)
	subset = parts[2]
	hostname = parts[3]
	return
}

// --- ConfigGenerator: Envoy 설정 생성기 ---

// ConfigGenerator는 Istio의 ConfigGenerator 인터페이스를 시뮬레이션한다.
// 실제 소스: pilot/pkg/networking/core/configgen.go
type ConfigGenerator struct {
	services         []*Service
	virtualServices  []*VirtualService
	destinationRules []*DestinationRule
}

// NewConfigGenerator는 설정 생성기를 초기화한다.
func NewConfigGenerator(services []*Service, vs []*VirtualService, dr []*DestinationRule) *ConfigGenerator {
	return &ConfigGenerator{
		services:         services,
		virtualServices:  vs,
		destinationRules: dr,
	}
}

// BuildClusters는 CDS 리소스를 생성한다.
// 실제 소스: pilot/pkg/networking/core/cluster.go의 BuildClusters 함수 참조
//
// 처리 흐름:
//   1. 각 서비스의 각 포트에 대해 기본 클러스터 생성
//   2. DestinationRule이 있으면 subset별 추가 클러스터 생성
//   3. 클러스터 이름 = BuildSubsetKey(direction, subset, hostname, port)
func (cg *ConfigGenerator) BuildClusters() []*Cluster {
	clusters := make([]*Cluster, 0)

	for _, svc := range cg.services {
		// 해당 서비스의 DestinationRule 찾기
		var dr *DestinationRule
		for _, d := range cg.destinationRules {
			if d.Host == svc.Hostname {
				dr = d
				break
			}
		}

		for _, port := range svc.Ports {
			// 기본 클러스터 생성 (subset 없음)
			clusterName := BuildSubsetKey(TrafficDirectionOutbound, "", svc.Hostname, port.Port)
			discoveryType := convertResolution(svc.Resolution)

			defaultCluster := &Cluster{
				Name:          clusterName,
				DiscoveryType: discoveryType,
				LoadBalancer:  "ROUND_ROBIN",
			}

			// DestinationRule의 기본 TrafficPolicy 적용
			if dr != nil && dr.TrafficPolicy != nil {
				if dr.TrafficPolicy.LoadBalancer != "" {
					defaultCluster.LoadBalancer = dr.TrafficPolicy.LoadBalancer
				}
				if dr.TrafficPolicy.ConnectionPool != nil {
					defaultCluster.MaxConnections = dr.TrafficPolicy.ConnectionPool.MaxConnections
				}
			}

			clusters = append(clusters, defaultCluster)

			// DestinationRule의 subset별 클러스터 생성
			// 실제 Istio: cluster.go의 applyDestinationRule 함수에서
			// subset마다 별도의 Envoy 클러스터를 생성한다.
			if dr != nil {
				for _, subset := range dr.Subsets {
					subsetClusterName := BuildSubsetKey(
						TrafficDirectionOutbound, subset.Name, svc.Hostname, port.Port)

					subsetCluster := &Cluster{
						Name:          subsetClusterName,
						DiscoveryType: discoveryType,
						LoadBalancer:  defaultCluster.LoadBalancer,
						MaxConnections: defaultCluster.MaxConnections,
					}

					// subset별 TrafficPolicy 오버라이드
					if subset.TrafficPolicy != nil {
						if subset.TrafficPolicy.LoadBalancer != "" {
							subsetCluster.LoadBalancer = subset.TrafficPolicy.LoadBalancer
						}
						if subset.TrafficPolicy.ConnectionPool != nil {
							subsetCluster.MaxConnections = subset.TrafficPolicy.ConnectionPool.MaxConnections
						}
					}

					clusters = append(clusters, subsetCluster)
				}
			}
		}
	}

	return clusters
}

// BuildHTTPRoutes는 RDS 리소스를 생성한다.
// 실제 소스: pilot/pkg/networking/core/httproute.go의 BuildHTTPRoutes 함수 참조
//
// VirtualService의 HTTP 라우팅 규칙을 Envoy RouteConfiguration으로 변환한다.
// 각 VirtualService의 각 HTTP route는 Envoy의 Route로 매핑된다.
func (cg *ConfigGenerator) BuildHTTPRoutes() []*RouteConfiguration {
	routeConfigs := make([]*RouteConfiguration, 0)

	for _, vs := range cg.virtualServices {
		for _, host := range vs.Hosts {
			rc := &RouteConfiguration{
				Name: fmt.Sprintf("%s:%s", host, vs.Name),
			}

			vh := &VirtualHost{
				Name:    fmt.Sprintf("%s|%s", host, vs.Name),
				Domains: []string{host, host + ":*"},
			}

			for _, httpRoute := range vs.HTTP {
				route := cg.translateHTTPRoute(httpRoute, host)
				vh.Routes = append(vh.Routes, route)
			}

			rc.VirtualHosts = append(rc.VirtualHosts, vh)
			routeConfigs = append(routeConfigs, rc)
		}
	}

	return routeConfigs
}

// translateHTTPRoute는 VirtualService의 HTTPRoute를 Envoy Route로 변환한다.
func (cg *ConfigGenerator) translateHTTPRoute(httpRoute *HTTPRoute, host string) *Route {
	route := &Route{
		Name: httpRoute.Name,
	}

	// 매칭 조건 변환
	if len(httpRoute.Match) > 0 {
		m := httpRoute.Match[0]
		rm := &RouteMatch{
			Headers: make(map[string]string),
		}
		if m.URI != nil {
			switch m.URI.MatchType {
			case "prefix":
				rm.Prefix = m.URI.Value
			case "exact":
				rm.Path = m.URI.Value
			}
		}
		if m.Headers != nil {
			for k, v := range m.Headers {
				rm.Headers[k] = fmt.Sprintf("%s:%s", v.MatchType, v.Value)
			}
		}
		route.Match = rm
	} else {
		// 매칭 조건이 없으면 prefix "/"로 기본 매칭
		route.Match = &RouteMatch{Prefix: "/"}
	}

	// 목적지 변환
	if len(httpRoute.Route) == 1 {
		// 단일 목적지 → direct cluster
		dest := httpRoute.Route[0].Destination
		clusterName := BuildSubsetKey(
			TrafficDirectionOutbound, dest.Subset, dest.Host, dest.Port)
		route.ClusterName = clusterName
	} else if len(httpRoute.Route) > 1 {
		// 다중 목적지 → weighted clusters (카나리 배포 등)
		for _, rd := range httpRoute.Route {
			dest := rd.Destination
			clusterName := BuildSubsetKey(
				TrafficDirectionOutbound, dest.Subset, dest.Host, dest.Port)
			route.WeightedClusters = append(route.WeightedClusters, &WeightedCluster{
				ClusterName: clusterName,
				Weight:      rd.Weight,
			})
		}
	}

	return route
}

// BuildListeners는 LDS 리소스를 생성한다.
// 실제 소스: pilot/pkg/networking/core/listener.go의 BuildListeners 함수 참조
//
// Sidecar 프록시의 경우:
//   - Outbound: 각 서비스 포트별 Listener (0.0.0.0:port)
//   - Inbound: 워크로드가 수신하는 포트별 Listener
func (cg *ConfigGenerator) BuildListeners() []*Listener {
	listeners := make([]*Listener, 0)

	// Outbound listeners: 각 서비스의 각 포트에 대해 리스너 생성
	// 실제 Istio는 같은 포트를 공유하는 서비스들을 하나의 리스너로 병합한다
	portListeners := make(map[int]*Listener)
	for _, svc := range cg.services {
		for _, port := range svc.Ports {
			if _, exists := portListeners[port.Port]; !exists {
				listener := &Listener{
					Name:    fmt.Sprintf("0.0.0.0_%d", port.Port),
					Address: "0.0.0.0",
					Port:    port.Port,
				}

				// HTTP 포트면 HCM(HttpConnectionManager) 필터 + RDS 참조
				if port.Protocol == "HTTP" || port.Protocol == "gRPC" {
					listener.FilterChains = []*FilterChain{{
						Filters: []string{
							"envoy.filters.network.http_connection_manager",
						},
					}}
					// RDS 참조: VirtualService에서 해당 호스트의 라우트를 찾음
					for _, vs := range cg.virtualServices {
						for _, h := range vs.Hosts {
							if h == svc.Hostname {
								listener.RouteConfigName = fmt.Sprintf("%s:%s", h, vs.Name)
							}
						}
					}
				} else {
					// TCP 포트면 tcp_proxy 필터
					listener.FilterChains = []*FilterChain{{
						Filters: []string{
							"envoy.filters.network.tcp_proxy",
						},
					}}
				}
				portListeners[port.Port] = listener
				listeners = append(listeners, listener)
			}
		}
	}

	// virtualOutbound listener (15001): iptables로 리다이렉트된 모든 outbound 트래픽 수신
	listeners = append(listeners, &Listener{
		Name:    "virtualOutbound",
		Address: "0.0.0.0",
		Port:    15001,
		FilterChains: []*FilterChain{{
			Filters: []string{"envoy.filters.network.tcp_proxy"},
		}},
	})

	// virtualInbound listener (15006): iptables로 리다이렉트된 모든 inbound 트래픽 수신
	listeners = append(listeners, &Listener{
		Name:    "virtualInbound",
		Address: "0.0.0.0",
		Port:    15006,
		FilterChains: []*FilterChain{{
			Filters: []string{"envoy.filters.network.http_connection_manager"},
		}},
	})

	return listeners
}

// convertResolution은 Istio Resolution을 Envoy DiscoveryType으로 변환한다.
// 실제 소스: pilot/pkg/networking/core/cluster.go의 convertResolution 함수
func convertResolution(res Resolution) string {
	switch res {
	case ClientSideLB:
		return "EDS"
	case DNSLB:
		return "STRICT_DNS"
	case DNSRoundRobinLB:
		return "LOGICAL_DNS"
	case Passthrough:
		return "ORIGINAL_DST"
	default:
		return "EDS"
	}
}

// --- 출력 헬퍼 ---

func printClusters(clusters []*Cluster) {
	fmt.Printf("\n  생성된 Cluster 수: %d\n", len(clusters))
	fmt.Println("  " + strings.Repeat("-", 90))
	fmt.Printf("  %-55s %-12s %-12s %s\n", "이름", "타입", "LB", "MaxConn")
	fmt.Println("  " + strings.Repeat("-", 90))
	for _, c := range clusters {
		maxConn := "-"
		if c.MaxConnections > 0 {
			maxConn = fmt.Sprintf("%d", c.MaxConnections)
		}
		fmt.Printf("  %-55s %-12s %-12s %s\n", c.Name, c.DiscoveryType, c.LoadBalancer, maxConn)
	}
}

func printRoutes(routeConfigs []*RouteConfiguration) {
	fmt.Printf("\n  생성된 RouteConfiguration 수: %d\n", len(routeConfigs))
	for _, rc := range routeConfigs {
		fmt.Printf("\n  RouteConfig: %s\n", rc.Name)
		for _, vh := range rc.VirtualHosts {
			fmt.Printf("    VirtualHost: %s (domains: %v)\n", vh.Name, vh.Domains)
			for _, r := range vh.Routes {
				if r.Match != nil {
					matchStr := ""
					if r.Match.Prefix != "" {
						matchStr = fmt.Sprintf("prefix=%s", r.Match.Prefix)
					} else if r.Match.Path != "" {
						matchStr = fmt.Sprintf("path=%s", r.Match.Path)
					}
					if len(r.Match.Headers) > 0 {
						for k, v := range r.Match.Headers {
							matchStr += fmt.Sprintf(" header[%s]=%s", k, v)
						}
					}
					fmt.Printf("      Route[%s]: match(%s)\n", r.Name, matchStr)
				}

				if r.ClusterName != "" {
					fmt.Printf("        → cluster: %s\n", r.ClusterName)
				}
				for _, wc := range r.WeightedClusters {
					fmt.Printf("        → weighted: %s (weight=%d%%)\n", wc.ClusterName, wc.Weight)
				}
			}
		}
	}
}

func printListeners(listeners []*Listener) {
	fmt.Printf("\n  생성된 Listener 수: %d\n", len(listeners))
	fmt.Println("  " + strings.Repeat("-", 90))
	fmt.Printf("  %-25s %-15s %-6s %-35s %s\n", "이름", "주소", "포트", "필터", "RDS")
	fmt.Println("  " + strings.Repeat("-", 90))
	for _, l := range listeners {
		filters := ""
		if len(l.FilterChains) > 0 && len(l.FilterChains[0].Filters) > 0 {
			f := l.FilterChains[0].Filters[0]
			// 축약 표시
			f = strings.TrimPrefix(f, "envoy.filters.network.")
			filters = f
		}
		rds := "-"
		if l.RouteConfigName != "" {
			rds = l.RouteConfigName
		}
		fmt.Printf("  %-25s %-15s %-6d %-35s %s\n", l.Name, l.Address, l.Port, filters, rds)
	}
}

// =============================================================================
// main: 시나리오 실행
// =============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("Istio Envoy Config Generation 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// --- 서비스 정의 ---
	services := []*Service{
		{
			Hostname:   "reviews.default.svc.cluster.local",
			Name:       "reviews",
			Namespace:  "default",
			Resolution: ClientSideLB,
			Ports: []*Port{
				{Name: "http", Port: 9080, Protocol: "HTTP"},
			},
		},
		{
			Hostname:   "ratings.default.svc.cluster.local",
			Name:       "ratings",
			Namespace:  "default",
			Resolution: ClientSideLB,
			Ports: []*Port{
				{Name: "http", Port: 9080, Protocol: "HTTP"},
			},
		},
		{
			Hostname:   "productpage.default.svc.cluster.local",
			Name:       "productpage",
			Namespace:  "default",
			Resolution: ClientSideLB,
			Ports: []*Port{
				{Name: "http", Port: 9080, Protocol: "HTTP"},
			},
		},
		{
			Hostname:   "mongodb.default.svc.cluster.local",
			Name:       "mongodb",
			Namespace:  "default",
			Resolution: ClientSideLB,
			Ports: []*Port{
				{Name: "tcp", Port: 27017, Protocol: "TCP"},
			},
		},
	}

	// --- DestinationRule 정의 ---
	// reviews 서비스에 v1, v2, v3 세 개의 subset 정의
	destinationRules := []*DestinationRule{
		{
			Name:      "reviews-dr",
			Namespace: "default",
			Host:      "reviews.default.svc.cluster.local",
			TrafficPolicy: &TrafficPolicy{
				LoadBalancer: "ROUND_ROBIN",
				ConnectionPool: &ConnectionPool{
					MaxConnections: 100,
				},
			},
			Subsets: []*Subset{
				{
					Name:   "v1",
					Labels: map[string]string{"version": "v1"},
				},
				{
					Name:   "v2",
					Labels: map[string]string{"version": "v2"},
					TrafficPolicy: &TrafficPolicy{
						LoadBalancer: "LEAST_CONN",
					},
				},
				{
					Name:   "v3",
					Labels: map[string]string{"version": "v3"},
					TrafficPolicy: &TrafficPolicy{
						ConnectionPool: &ConnectionPool{
							MaxConnections: 50,
						},
					},
				},
			},
		},
		{
			Name:      "ratings-dr",
			Namespace: "default",
			Host:      "ratings.default.svc.cluster.local",
			TrafficPolicy: &TrafficPolicy{
				LoadBalancer: "RANDOM",
			},
			Subsets: []*Subset{
				{
					Name:   "v1",
					Labels: map[string]string{"version": "v1"},
				},
			},
		},
	}

	// --- VirtualService 정의 ---
	virtualServices := []*VirtualService{
		{
			Name:      "reviews-vs",
			Namespace: "default",
			Hosts:     []string{"reviews.default.svc.cluster.local"},
			Gateways:  []string{"mesh"},
			HTTP: []*HTTPRoute{
				{
					// 카나리 배포: v1에 80%, v2에 20%
					Name: "canary-split",
					Match: []*HTTPMatchRequest{
						{URI: &StringMatch{MatchType: "prefix", Value: "/api/v1"}},
					},
					Route: []*HTTPRouteDestination{
						{
							Destination: &Destination{
								Host:   "reviews.default.svc.cluster.local",
								Subset: "v1",
								Port:   9080,
							},
							Weight: 80,
						},
						{
							Destination: &Destination{
								Host:   "reviews.default.svc.cluster.local",
								Subset: "v2",
								Port:   9080,
							},
							Weight: 20,
						},
					},
				},
				{
					// 헤더 기반 라우팅: test 사용자 → v3
					Name: "header-routing",
					Match: []*HTTPMatchRequest{
						{
							Headers: map[string]*StringMatch{
								"end-user": {MatchType: "exact", Value: "jason"},
							},
						},
					},
					Route: []*HTTPRouteDestination{
						{
							Destination: &Destination{
								Host:   "reviews.default.svc.cluster.local",
								Subset: "v3",
								Port:   9080,
							},
							Weight: 100,
						},
					},
				},
				{
					// 기본 라우트 → v1
					Name: "default",
					Route: []*HTTPRouteDestination{
						{
							Destination: &Destination{
								Host:   "reviews.default.svc.cluster.local",
								Subset: "v1",
								Port:   9080,
							},
							Weight: 100,
						},
					},
				},
			},
		},
	}

	cg := NewConfigGenerator(services, virtualServices, destinationRules)

	// --- 시나리오 1: CDS (Cluster) 생성 ---
	fmt.Println("\n--- 시나리오 1: CDS (Cluster Discovery Service) ---")
	fmt.Println("  DestinationRule의 subset이 별도 Envoy 클러스터를 생성한다.")
	fmt.Println("  클러스터 이름 형식: outbound|port|subset|hostname")

	clusters := cg.BuildClusters()
	printClusters(clusters)

	// --- 시나리오 2: 클러스터 이름 파싱 ---
	fmt.Println("\n--- 시나리오 2: 클러스터 이름 파싱 (ParseSubsetKey) ---")
	testNames := []string{
		"outbound|9080|v1|reviews.default.svc.cluster.local",
		"outbound|9080||ratings.default.svc.cluster.local",
		"outbound|27017||mongodb.default.svc.cluster.local",
	}
	for _, name := range testNames {
		dir, subset, host, port := ParseSubsetKey(name)
		subsetStr := "(default)"
		if subset != "" {
			subsetStr = subset
		}
		fmt.Printf("  %s\n    → direction=%s, port=%d, subset=%s, host=%s\n",
			name, dir, port, subsetStr, host)
	}

	// --- 시나리오 3: RDS (Route) 생성 ---
	fmt.Println("\n--- 시나리오 3: RDS (Route Discovery Service) ---")
	fmt.Println("  VirtualService의 HTTP 규칙이 Envoy Route로 변환된다.")
	fmt.Println("  - match 조건 → RouteMatch")
	fmt.Println("  - 단일 destination → cluster 직접 참조")
	fmt.Println("  - 다중 destination → weighted_clusters")

	routeConfigs := cg.BuildHTTPRoutes()
	printRoutes(routeConfigs)

	// --- 시나리오 4: LDS (Listener) 생성 ---
	fmt.Println("\n--- 시나리오 4: LDS (Listener Discovery Service) ---")
	fmt.Println("  서비스 포트별 Listener + virtualOutbound(15001) + virtualInbound(15006)")

	listeners := cg.BuildListeners()
	printListeners(listeners)

	// --- 시나리오 5: 설정 간 참조 관계 ---
	fmt.Println("\n--- 시나리오 5: CDS → RDS → LDS 참조 관계 ---")
	fmt.Println()
	fmt.Println("  LDS (Listener)          →  RDS (Route)              →  CDS (Cluster)")
	fmt.Println("  ─────────────────────       ──────────────────────       ─────────────────────")
	for _, l := range listeners {
		if l.RouteConfigName != "" {
			fmt.Printf("  %-25s → ", l.Name)
			for _, rc := range routeConfigs {
				if rc.Name == l.RouteConfigName {
					fmt.Printf("%-25s →  ", rc.Name)
					for _, vh := range rc.VirtualHosts {
						for _, r := range vh.Routes {
							if r.ClusterName != "" {
								fmt.Printf("%s", r.ClusterName)
							} else if len(r.WeightedClusters) > 0 {
								names := make([]string, 0)
								for _, wc := range r.WeightedClusters {
									names = append(names, fmt.Sprintf("%s(%d%%)", wc.ClusterName, wc.Weight))
								}
								fmt.Printf("[%s]", strings.Join(names, ", "))
							}
							fmt.Println()
							fmt.Printf("  %-25s   %-25s    ", "", "")
						}
					}
				}
			}
			fmt.Println()
		}
	}

	fmt.Println("\n시뮬레이션 완료.")
}
