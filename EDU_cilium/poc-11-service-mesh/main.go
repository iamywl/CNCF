// SPDX-License-Identifier: Apache-2.0
// Cilium Service Mesh & Proxy Subsystem PoC
//
// 이 PoC는 Cilium의 서비스 메시 핵심 메커니즘을 시뮬레이션한다:
// 1. xDS 프로토콜: 컨트롤 플레인이 Listener/Route/Cluster/Endpoint 설정을 푸시
// 2. DNS 프록시: DNS 쿼리 가로채기 → 해석 → 캐시 → FQDN Identity 생성
// 3. L7 프록시 흐름: 패킷 → BPF 리다이렉트 → 프록시 → HTTP 경로 매칭 → 포워드/블록
// 4. Gateway API: HTTPRoute path/header 매칭 기반 라우팅
// 5. Per-node 프록시 vs 사이드카 모델 비교
//
// 실행: go run main.go
// 외부 의존성 없음 (순수 Go 표준 라이브러리)

package main

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. xDS Protocol Simulation
// =============================================================================

// XDSResourceType는 xDS 리소스 타입을 나타낸다.
// Cilium 실제 코드: pkg/envoy/resources.go
type XDSResourceType string

const (
	ListenerType XDSResourceType = "type.googleapis.com/envoy.config.listener.v3.Listener"
	RouteType    XDSResourceType = "type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
	ClusterType  XDSResourceType = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
	EndpointType XDSResourceType = "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
	SecretType   XDSResourceType = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"
)

// XDSResource는 xDS 리소스를 나타낸다.
type XDSResource struct {
	TypeURL  XDSResourceType
	Name     string
	Version  uint64
	Resource interface{}
}

// ListenerConfig는 Envoy Listener 설정을 시뮬레이션한다.
// Cilium 실제 코드: pkg/envoy/xds_server.go AddListener()
type ListenerConfig struct {
	Name      string
	Port      uint16
	Protocol  string // "HTTP" or "TCP"
	IsIngress bool
	Filters   []string // Filter chain names
}

// RouteConfig는 Envoy Route 설정을 시뮬레이션한다.
type RouteConfig struct {
	Name         string
	VirtualHosts []VirtualHost
}

// VirtualHost는 Envoy VirtualHost를 시뮬레이션한다.
type VirtualHost struct {
	Name    string
	Domains []string
	Routes  []Route
}

// Route는 Envoy Route를 시뮬레이션한다.
type Route struct {
	PathPrefix  string
	PathExact   string
	Headers     map[string]string // header name → value match
	ClusterName string
	Action      string // "forward" or "block"
}

// ClusterConfig는 Envoy Cluster를 시뮬레이션한다.
type ClusterConfig struct {
	Name           string
	ConnectTimeout time.Duration
	LbPolicy       string // "ROUND_ROBIN", "LEAST_REQUEST"
	UseTLS         bool
}

// EndpointConfig는 Envoy Endpoint를 시뮬레이션한다.
type EndpointConfig struct {
	ClusterName string
	Endpoints   []Endpoint
}

// Endpoint는 백엔드 엔드포인트이다.
type Endpoint struct {
	Address string
	Port    uint16
	Weight  uint32
	Healthy bool
}

// SecretConfig는 TLS Secret을 시뮬레이션한다.
type SecretConfig struct {
	Name        string
	Certificate string // PEM cert (시뮬레이션)
	PrivateKey  string // PEM key (시뮬레이션)
}

// XDSCache는 xDS 리소스 캐시를 시뮬레이션한다.
// Cilium 실제 코드: pkg/envoy/xds/cache.go
type XDSCache struct {
	mu        sync.RWMutex
	resources map[XDSResourceType]map[string]XDSResource
	version   uint64
	watchers  []chan XDSResource // ACK/NACK를 기다리는 watcher
}

func NewXDSCache() *XDSCache {
	return &XDSCache{
		resources: make(map[XDSResourceType]map[string]XDSResource),
		version:   1,
	}
}

// Upsert는 리소스를 캐시에 삽입/업데이트한다.
func (c *XDSCache) Upsert(typeURL XDSResourceType, name string, resource interface{}) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.resources[typeURL]; !ok {
		c.resources[typeURL] = make(map[string]XDSResource)
	}

	c.version++
	r := XDSResource{
		TypeURL:  typeURL,
		Name:     name,
		Version:  c.version,
		Resource: resource,
	}
	c.resources[typeURL][name] = r

	// 모든 watcher에게 변경 통지 (비동기)
	for _, w := range c.watchers {
		select {
		case w <- r:
		default:
		}
	}

	return c.version
}

// Delete는 리소스를 캐시에서 삭제한다.
func (c *XDSCache) Delete(typeURL XDSResourceType, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if m, ok := c.resources[typeURL]; ok {
		delete(m, name)
		c.version++
	}
}

// Lookup은 리소스를 조회한다.
func (c *XDSCache) Lookup(typeURL XDSResourceType, name string) (XDSResource, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if m, ok := c.resources[typeURL]; ok {
		if r, ok := m[name]; ok {
			return r, true
		}
	}
	return XDSResource{}, false
}

// GetAll은 특정 타입의 모든 리소스를 반환한다.
func (c *XDSCache) GetAll(typeURL XDSResourceType) []XDSResource {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var results []XDSResource
	if m, ok := c.resources[typeURL]; ok {
		for _, r := range m {
			results = append(results, r)
		}
	}
	return results
}

// XDSServer는 Cilium의 xDS 서버를 시뮬레이션한다.
// Cilium 실제 코드: pkg/envoy/xds_server.go, pkg/envoy/grpc.go
type XDSServer struct {
	listenerCache *XDSCache
	routeCache    *XDSCache
	clusterCache  *XDSCache
	endpointCache *XDSCache
	secretCache   *XDSCache
}

func NewXDSServer() *XDSServer {
	return &XDSServer{
		listenerCache: NewXDSCache(),
		routeCache:    NewXDSCache(),
		clusterCache:  NewXDSCache(),
		endpointCache: NewXDSCache(),
		secretCache:   NewXDSCache(),
	}
}

// AddListener는 Envoy Listener를 추가한다.
func (s *XDSServer) AddListener(cfg ListenerConfig) uint64 {
	return s.listenerCache.Upsert(ListenerType, cfg.Name, cfg)
}

// AddRoute는 Route를 추가한다.
func (s *XDSServer) AddRoute(cfg RouteConfig) uint64 {
	return s.routeCache.Upsert(RouteType, cfg.Name, cfg)
}

// AddCluster는 Cluster를 추가한다.
func (s *XDSServer) AddCluster(cfg ClusterConfig) uint64 {
	return s.clusterCache.Upsert(ClusterType, cfg.Name, cfg)
}

// AddEndpoint는 Endpoint를 추가한다.
func (s *XDSServer) AddEndpoint(cfg EndpointConfig) uint64 {
	return s.endpointCache.Upsert(EndpointType, cfg.ClusterName, cfg)
}

// AddSecret은 TLS Secret을 추가한다.
func (s *XDSServer) AddSecret(cfg SecretConfig) uint64 {
	return s.secretCache.Upsert(SecretType, cfg.Name, cfg)
}

// =============================================================================
// 2. DNS Proxy Simulation
// =============================================================================

// DNSCacheEntry는 DNS 캐시 항목을 시뮬레이션한다.
// Cilium 실제 코드: pkg/fqdn/cache.go cacheEntry
type DNSCacheEntry struct {
	Name           string
	IPs            []net.IP
	TTL            int
	LookupTime     time.Time
	ExpirationTime time.Time
}

// FQDNIdentity는 FQDN 기반 Identity를 시뮬레이션한다.
type FQDNIdentity struct {
	FQDN       string
	IPs        []net.IP
	IdentityID uint32
}

// DNSProxy는 Cilium의 DNS 프록시를 시뮬레이션한다.
// Cilium 실제 코드: pkg/fqdn/cache.go, pkg/proxy/dns.go
type DNSProxy struct {
	mu            sync.RWMutex
	cache         map[string]*DNSCacheEntry   // FQDN → cache entry
	reverseCache  map[string]string           // IP → FQDN (역방향 조회)
	identities    map[string]*FQDNIdentity    // FQDN → identity
	allowPatterns []string                    // 허용된 DNS 패턴
	nextIdentity  uint32
	queryLog      []DNSQueryLog
}

// DNSQueryLog는 DNS 쿼리 로그이다.
type DNSQueryLog struct {
	Timestamp  time.Time
	Query      string
	Response   []net.IP
	Allowed    bool
	EndpointID uint16
}

func NewDNSProxy() *DNSProxy {
	return &DNSProxy{
		cache:        make(map[string]*DNSCacheEntry),
		reverseCache: make(map[string]string),
		identities:   make(map[string]*FQDNIdentity),
		nextIdentity: 16384, // FQDN identity 시작 번호
	}
}

// SetAllowedPatterns는 DNS 프록시가 허용할 FQDN 패턴을 설정한다.
func (d *DNSProxy) SetAllowedPatterns(patterns []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allowPatterns = patterns
}

// matchPattern은 FQDN이 패턴과 매칭되는지 확인한다.
func matchPattern(fqdn, pattern string) bool {
	if pattern == "*" {
		return true
	}
	// *.example.com 패턴 처리
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // .example.com
		return strings.HasSuffix(fqdn, suffix) || fqdn == pattern[2:]
	}
	return fqdn == pattern
}

// Resolve는 DNS 쿼리를 시뮬레이션한다.
// Cilium에서는 BPF 데이터패스가 DNS 쿼리를 프록시로 리다이렉트한다.
func (d *DNSProxy) Resolve(endpointID uint16, fqdn string) ([]net.IP, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 1. 패턴 매칭으로 허용 여부 확인
	allowed := false
	for _, pattern := range d.allowPatterns {
		if matchPattern(fqdn, pattern) {
			allowed = true
			break
		}
	}

	if !allowed {
		d.queryLog = append(d.queryLog, DNSQueryLog{
			Timestamp:  time.Now(),
			Query:      fqdn,
			Allowed:    false,
			EndpointID: endpointID,
		})
		return nil, false
	}

	// 2. 캐시 확인
	if entry, ok := d.cache[fqdn]; ok {
		if time.Now().Before(entry.ExpirationTime) {
			d.queryLog = append(d.queryLog, DNSQueryLog{
				Timestamp:  time.Now(),
				Query:      fqdn,
				Response:   entry.IPs,
				Allowed:    true,
				EndpointID: endpointID,
			})
			return entry.IPs, true
		}
		// TTL 만료 - 재해석 필요
		delete(d.cache, fqdn)
	}

	// 3. DNS 해석 시뮬레이션 (실제로는 upstream DNS에 쿼리)
	ips := simulateDNSResolve(fqdn)

	// 4. 캐시에 저장
	ttl := 300 // 5분 TTL
	entry := &DNSCacheEntry{
		Name:           fqdn,
		IPs:            ips,
		TTL:            ttl,
		LookupTime:     time.Now(),
		ExpirationTime: time.Now().Add(time.Duration(ttl) * time.Second),
	}
	d.cache[fqdn] = entry

	// 5. 역방향 캐시 업데이트
	for _, ip := range ips {
		d.reverseCache[ip.String()] = fqdn
	}

	// 6. FQDN Identity 생성/업데이트
	// Cilium에서는 DNS 응답의 IP에 대해 CIDR Identity를 생성한다.
	if _, ok := d.identities[fqdn]; !ok {
		d.identities[fqdn] = &FQDNIdentity{
			FQDN:       fqdn,
			IPs:        ips,
			IdentityID: d.nextIdentity,
		}
		d.nextIdentity++
	} else {
		d.identities[fqdn].IPs = ips
	}

	d.queryLog = append(d.queryLog, DNSQueryLog{
		Timestamp:  time.Now(),
		Query:      fqdn,
		Response:   ips,
		Allowed:    true,
		EndpointID: endpointID,
	})

	return ips, true
}

// simulateDNSResolve는 DNS 해석을 시뮬레이션한다.
func simulateDNSResolve(fqdn string) []net.IP {
	// 시뮬레이션: FQDN에 따라 고정 IP 반환
	dnsDB := map[string][]string{
		"api.example.com":     {"10.0.1.10", "10.0.1.11"},
		"web.example.com":     {"10.0.2.20", "10.0.2.21"},
		"db.internal.io":      {"10.0.3.30"},
		"s3.amazonaws.com":    {"52.216.1.1", "52.216.1.2", "52.216.1.3"},
		"blocked.malware.com": {"192.168.99.99"},
	}

	var ips []net.IP
	if addrs, ok := dnsDB[fqdn]; ok {
		for _, addr := range addrs {
			ips = append(ips, net.ParseIP(addr))
		}
	} else {
		// 알 수 없는 도메인은 랜덤 IP
		ips = append(ips, net.IPv4(10, byte(rand.Intn(255)), byte(rand.Intn(255)), byte(rand.Intn(255))))
	}
	return ips
}

// =============================================================================
// 3. L7 Proxy Flow Simulation
// =============================================================================

// ProxyDecision은 프록시 결정 결과이다.
type ProxyDecision struct {
	Allowed       bool
	Reason        string
	MatchedRoute  string
	TargetCluster string
	TargetBackend string
}

// L7Proxy는 Cilium의 L7 프록시 흐름을 시뮬레이션한다.
// BPF 리다이렉트 → Envoy 처리 → 정책 적용 → 포워딩/블로킹
type L7Proxy struct {
	xdsServer *XDSServer
	dnsProxy  *DNSProxy
	stats     ProxyStats
	mu        sync.Mutex
}

// ProxyStats는 프록시 통계이다.
type ProxyStats struct {
	TotalRequests    int
	AllowedRequests  int
	BlockedRequests  int
	L7Redirects      int
	DirectForwards   int
}

func NewL7Proxy(xds *XDSServer, dns *DNSProxy) *L7Proxy {
	return &L7Proxy{
		xdsServer: xds,
		dnsProxy:  dns,
	}
}

// ProcessPacket은 패킷 처리를 시뮬레이션한다.
// Cilium의 BPF → Envoy → BPF 흐름을 보여준다.
func (p *L7Proxy) ProcessPacket(srcIP, dstIP string, dstPort uint16, httpMethod, httpPath, httpHost string, headers map[string]string) ProxyDecision {
	p.mu.Lock()
	p.stats.TotalRequests++
	p.mu.Unlock()

	// === Phase 1: BPF Datapath (시뮬레이션) ===
	// 실제 Cilium에서는 tc BPF 프로그램이 L7 정책이 필요한 패킷을 감지하여
	// Envoy 프록시 포트로 리다이렉트한다.
	needsL7 := p.checkL7PolicyNeeded(dstPort)

	if !needsL7 {
		// L7 정책 불필요 → BPF에서 직접 포워딩
		p.mu.Lock()
		p.stats.DirectForwards++
		p.mu.Unlock()
		return ProxyDecision{
			Allowed: true,
			Reason:  "BPF direct forward (no L7 policy)",
		}
	}

	p.mu.Lock()
	p.stats.L7Redirects++
	p.mu.Unlock()

	// === Phase 2: Envoy Proxy (시뮬레이션) ===
	// BPF가 패킷을 Envoy로 리다이렉트한 후, Envoy가 L7 처리를 수행한다.

	// 2a. Listener 매칭
	listener := p.findListener(dstPort)
	if listener == nil {
		return ProxyDecision{
			Allowed: false,
			Reason:  fmt.Sprintf("No listener found for port %d", dstPort),
		}
	}

	// 2b. Route 매칭 (HTTP 경로, 헤더)
	route := p.matchRoute(httpHost, httpPath, headers)
	if route == nil {
		p.mu.Lock()
		p.stats.BlockedRequests++
		p.mu.Unlock()
		return ProxyDecision{
			Allowed: false,
			Reason:  fmt.Sprintf("No matching route for %s %s (Host: %s)", httpMethod, httpPath, httpHost),
		}
	}

	// 2c. 정책 결정 (Cilium L7 Policy Filter)
	if route.Action == "block" {
		p.mu.Lock()
		p.stats.BlockedRequests++
		p.mu.Unlock()
		return ProxyDecision{
			Allowed:      false,
			Reason:       fmt.Sprintf("Blocked by L7 policy: route %s", route.PathPrefix+route.PathExact),
			MatchedRoute: route.PathPrefix + route.PathExact,
		}
	}

	// 2d. Cluster 및 Endpoint 선택 (로드밸런싱)
	backend := p.selectBackend(route.ClusterName)
	if backend == "" {
		p.mu.Lock()
		p.stats.BlockedRequests++
		p.mu.Unlock()
		return ProxyDecision{
			Allowed:       false,
			Reason:        fmt.Sprintf("No healthy backend for cluster %s", route.ClusterName),
			MatchedRoute:  route.PathPrefix + route.PathExact,
			TargetCluster: route.ClusterName,
		}
	}

	// === Phase 3: BPF Re-injection (시뮬레이션) ===
	// Envoy가 처리를 완료한 패킷을 다시 BPF 데이터패스에 주입한다.
	p.mu.Lock()
	p.stats.AllowedRequests++
	p.mu.Unlock()

	return ProxyDecision{
		Allowed:       true,
		Reason:        "Allowed by L7 policy and routed to backend",
		MatchedRoute:  route.PathPrefix + route.PathExact,
		TargetCluster: route.ClusterName,
		TargetBackend: backend,
	}
}

func (p *L7Proxy) checkL7PolicyNeeded(dstPort uint16) bool {
	// L7 정책이 있는 포트만 프록시로 리다이렉트
	listeners := p.xdsServer.listenerCache.GetAll(ListenerType)
	for _, l := range listeners {
		cfg := l.Resource.(ListenerConfig)
		if cfg.Port == dstPort {
			return true
		}
	}
	return false
}

func (p *L7Proxy) findListener(port uint16) *ListenerConfig {
	listeners := p.xdsServer.listenerCache.GetAll(ListenerType)
	for _, l := range listeners {
		cfg := l.Resource.(ListenerConfig)
		if cfg.Port == port {
			return &cfg
		}
	}
	return nil
}

func (p *L7Proxy) matchRoute(host, path string, headers map[string]string) *Route {
	routes := p.xdsServer.routeCache.GetAll(RouteType)
	for _, r := range routes {
		cfg := r.Resource.(RouteConfig)
		for _, vh := range cfg.VirtualHosts {
			// 도메인 매칭
			domainMatch := false
			for _, d := range vh.Domains {
				if d == "*" || d == host {
					domainMatch = true
					break
				}
			}
			if !domainMatch {
				continue
			}

			// 경로 매칭
			for _, route := range vh.Routes {
				pathMatch := false
				if route.PathPrefix != "" && strings.HasPrefix(path, route.PathPrefix) {
					pathMatch = true
				}
				if route.PathExact != "" && path == route.PathExact {
					pathMatch = true
				}
				if !pathMatch {
					continue
				}

				// 헤더 매칭
				headerMatch := true
				for hk, hv := range route.Headers {
					if headers == nil || headers[hk] != hv {
						headerMatch = false
						break
					}
				}
				if !headerMatch {
					continue
				}

				return &route
			}
		}
	}
	return nil
}

func (p *L7Proxy) selectBackend(clusterName string) string {
	endpoints := p.xdsServer.endpointCache.GetAll(EndpointType)
	for _, e := range endpoints {
		cfg := e.Resource.(EndpointConfig)
		if cfg.ClusterName == clusterName {
			// 간단한 라운드 로빈 (건강한 엔드포인트만)
			var healthy []Endpoint
			for _, ep := range cfg.Endpoints {
				if ep.Healthy {
					healthy = append(healthy, ep)
				}
			}
			if len(healthy) == 0 {
				return ""
			}
			selected := healthy[rand.Intn(len(healthy))]
			return fmt.Sprintf("%s:%d", selected.Address, selected.Port)
		}
	}
	return ""
}

// =============================================================================
// 4. Gateway API Simulation
// =============================================================================

// GatewayClass는 Gateway API GatewayClass를 시뮬레이션한다.
type GatewayClass struct {
	Name           string
	ControllerName string
}

// Gateway는 Gateway API Gateway를 시뮬레이션한다.
type Gateway struct {
	Name      string
	Namespace string
	ClassName string
	Listeners []GatewayListener
}

// GatewayListener는 Gateway Listener를 시뮬레이션한다.
type GatewayListener struct {
	Name     string
	Port     uint16
	Protocol string
	Hostname string
	TLS      *GatewayTLS
}

// GatewayTLS는 TLS 설정이다.
type GatewayTLS struct {
	Mode       string // "Terminate" or "Passthrough"
	SecretName string
}

// HTTPRoute는 Gateway API HTTPRoute를 시뮬레이션한다.
type HTTPRoute struct {
	Name       string
	Namespace  string
	ParentRefs []ParentRef
	Hostnames  []string
	Rules      []HTTPRouteRule
}

// ParentRef는 부모 Gateway 참조이다.
type ParentRef struct {
	Name      string
	Namespace string
}

// HTTPRouteRule은 HTTPRoute 규칙이다.
type HTTPRouteRule struct {
	Matches    []HTTPRouteMatch
	BackendRef BackendRef
	Filters    []HTTPRouteFilter
}

// HTTPRouteMatch는 매칭 조건이다.
type HTTPRouteMatch struct {
	PathPrefix string
	PathExact  string
	Headers    map[string]string
	Method     string
}

// BackendRef는 백엔드 서비스 참조이다.
type BackendRef struct {
	ServiceName string
	Port        uint16
}

// HTTPRouteFilter는 요청/응답 필터이다.
type HTTPRouteFilter struct {
	Type               string // "RequestHeaderModifier"
	HeadersToAdd       map[string]string
	HeadersToRemove    []string
}

// GatewayAPIReconciler는 Gateway API 리소스를 xDS 리소스로 변환하는 reconciler이다.
// Cilium 실제 코드: operator/pkg/gateway-api/gateway_reconcile.go
//                   operator/pkg/model/translation/types.go Translator 인터페이스
type GatewayAPIReconciler struct {
	xdsServer *XDSServer
}

func NewGatewayAPIReconciler(xds *XDSServer) *GatewayAPIReconciler {
	return &GatewayAPIReconciler{xdsServer: xds}
}

// Reconcile은 Gateway + HTTPRoute를 CiliumEnvoyConfig(xDS 리소스)로 변환한다.
// 이것이 Cilium operator의 핵심 로직이다:
// Gateway API CRDs → Model → Translator → CiliumEnvoyConfig → xDS Cache → Envoy
func (r *GatewayAPIReconciler) Reconcile(gw Gateway, routes []HTTPRoute) {
	fmt.Printf("\n  [Gateway API Reconciler] Reconciling Gateway '%s/%s'\n", gw.Namespace, gw.Name)

	for _, listener := range gw.Listeners {
		// 1. Listener 생성
		listenerName := fmt.Sprintf("%s/%s/%s", gw.Namespace, gw.Name, listener.Name)
		listenerCfg := ListenerConfig{
			Name:      listenerName,
			Port:      listener.Port,
			Protocol:  listener.Protocol,
			IsIngress: true,
		}
		if listener.TLS != nil {
			listenerCfg.Filters = append(listenerCfg.Filters, "tls_inspector")
			// TLS Secret 추가
			r.xdsServer.AddSecret(SecretConfig{
				Name:        listener.TLS.SecretName,
				Certificate: "-----BEGIN CERTIFICATE-----\n(simulated)\n-----END CERTIFICATE-----",
				PrivateKey:  "-----BEGIN PRIVATE KEY-----\n(simulated)\n-----END PRIVATE KEY-----",
			})
			fmt.Printf("    - Added TLS Secret: %s\n", listener.TLS.SecretName)
		}
		ver := r.xdsServer.AddListener(listenerCfg)
		fmt.Printf("    - Added Listener: %s (port=%d, version=%d)\n", listenerName, listener.Port, ver)

		// 2. 매칭되는 HTTPRoute를 찾아서 Route/Cluster/Endpoint 생성
		for _, route := range routes {
			// parentRef 매칭 확인
			matched := false
			for _, ref := range route.ParentRefs {
				if ref.Name == gw.Name {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}

			var virtualHosts []VirtualHost
			domains := route.Hostnames
			if len(domains) == 0 {
				domains = []string{"*"}
			}

			var envoyRoutes []Route
			for _, rule := range route.Rules {
				clusterName := fmt.Sprintf("%s/%s:%d", route.Namespace, rule.BackendRef.ServiceName, rule.BackendRef.Port)

				// Cluster 생성
				r.xdsServer.AddCluster(ClusterConfig{
					Name:           clusterName,
					ConnectTimeout: 5 * time.Second,
					LbPolicy:       "ROUND_ROBIN",
				})

				// Endpoint 생성 (시뮬레이션)
				r.xdsServer.AddEndpoint(EndpointConfig{
					ClusterName: clusterName,
					Endpoints: []Endpoint{
						{Address: "10.0.1.10", Port: rule.BackendRef.Port, Weight: 1, Healthy: true},
						{Address: "10.0.1.11", Port: rule.BackendRef.Port, Weight: 1, Healthy: true},
					},
				})

				for _, match := range rule.Matches {
					envoyRoute := Route{
						PathPrefix:  match.PathPrefix,
						PathExact:   match.PathExact,
						Headers:     match.Headers,
						ClusterName: clusterName,
						Action:      "forward",
					}
					envoyRoutes = append(envoyRoutes, envoyRoute)
				}
			}

			virtualHosts = append(virtualHosts, VirtualHost{
				Name:    fmt.Sprintf("%s/%s", route.Namespace, route.Name),
				Domains: domains,
				Routes:  envoyRoutes,
			})

			routeCfg := RouteConfig{
				Name:         fmt.Sprintf("%s/%s", route.Namespace, route.Name),
				VirtualHosts: virtualHosts,
			}
			ver := r.xdsServer.AddRoute(routeCfg)
			fmt.Printf("    - Added Route: %s (domains=%v, rules=%d, version=%d)\n",
				routeCfg.Name, domains, len(envoyRoutes), ver)
		}
	}
}

// =============================================================================
// 5. Per-Node Proxy vs Sidecar Model Comparison
// =============================================================================

// NodeProxyModel은 per-node 프록시 모델을 시뮬레이션한다.
type NodeProxyModel struct {
	NodeName    string
	PodCount    int
	ProxyCount  int // per-node = 1, sidecar = PodCount
	MemoryPerProxy int // MB
	TotalMemory    int // MB
}

func simulatePerNodeModel(nodeName string, podCount int) NodeProxyModel {
	proxyCount := 1 // 노드당 1개 Envoy
	memPerProxy := 50 // Envoy 50MB (공유 모드에서는 더 큼)
	return NodeProxyModel{
		NodeName:       nodeName,
		PodCount:       podCount,
		ProxyCount:     proxyCount,
		MemoryPerProxy: memPerProxy,
		TotalMemory:    proxyCount * memPerProxy,
	}
}

func simulateSidecarModel(nodeName string, podCount int) NodeProxyModel {
	proxyCount := podCount // Pod당 1개 사이드카
	memPerProxy := 40      // 사이드카 Envoy 40MB
	return NodeProxyModel{
		NodeName:       nodeName,
		PodCount:       podCount,
		ProxyCount:     proxyCount,
		MemoryPerProxy: memPerProxy,
		TotalMemory:    proxyCount * memPerProxy,
	}
}

// =============================================================================
// Demo 실행
// =============================================================================

func main() {
	fmt.Println("=== Cilium Service Mesh & Proxy Subsystem PoC ===")
	fmt.Println()

	// -------------------------------------------------------------------------
	// Demo 1: xDS Protocol Simulation
	// -------------------------------------------------------------------------
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Demo 1: xDS Protocol - 컨트롤 플레인이 Envoy에 설정 푸시")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("Cilium agent는 xDS gRPC 서버를 운영하며,")
	fmt.Println("Envoy는 이 서버에 연결하여 설정을 동적으로 수신한다.")
	fmt.Println("(실제 코드: pkg/envoy/grpc.go, pkg/envoy/xds/server.go)")
	fmt.Println()

	xds := NewXDSServer()

	// Listener 추가 (LDS)
	ver := xds.AddListener(ListenerConfig{
		Name:      "ingress-http",
		Port:      8080,
		Protocol:  "HTTP",
		IsIngress: true,
		Filters:   []string{"cilium.l7policy", "envoy.filters.http.router"},
	})
	fmt.Printf("  [LDS] Added Listener 'ingress-http' (port=8080, version=%d)\n", ver)

	ver = xds.AddListener(ListenerConfig{
		Name:      "ingress-https",
		Port:      8443,
		Protocol:  "HTTPS",
		IsIngress: true,
		Filters:   []string{"tls_inspector", "cilium.l7policy", "envoy.filters.http.router"},
	})
	fmt.Printf("  [LDS] Added Listener 'ingress-https' (port=8443, version=%d)\n", ver)

	// Route 추가 (RDS)
	ver = xds.AddRoute(RouteConfig{
		Name: "main-routes",
		VirtualHosts: []VirtualHost{
			{
				Name:    "api-host",
				Domains: []string{"api.example.com"},
				Routes: []Route{
					{PathPrefix: "/v1/users", ClusterName: "default/user-service:8080", Action: "forward"},
					{PathPrefix: "/v1/orders", ClusterName: "default/order-service:8080", Action: "forward"},
					{PathPrefix: "/admin", ClusterName: "default/admin-service:8080", Action: "block"},
					{PathPrefix: "/", ClusterName: "default/frontend:8080", Action: "forward"},
				},
			},
			{
				Name:    "web-host",
				Domains: []string{"web.example.com", "*"},
				Routes: []Route{
					{PathPrefix: "/api", ClusterName: "default/api-backend:8080", Action: "forward",
						Headers: map[string]string{"x-api-version": "v2"}},
					{PathPrefix: "/api", ClusterName: "default/api-backend-v1:8080", Action: "forward"},
					{PathPrefix: "/", ClusterName: "default/web-frontend:3000", Action: "forward"},
				},
			},
		},
	})
	fmt.Printf("  [RDS] Added Route 'main-routes' (2 virtual hosts, version=%d)\n", ver)

	// Cluster 추가 (CDS)
	for _, name := range []string{
		"default/user-service:8080",
		"default/order-service:8080",
		"default/admin-service:8080",
		"default/frontend:8080",
		"default/api-backend:8080",
		"default/api-backend-v1:8080",
		"default/web-frontend:3000",
	} {
		ver = xds.AddCluster(ClusterConfig{
			Name:           name,
			ConnectTimeout: 5 * time.Second,
			LbPolicy:       "ROUND_ROBIN",
		})
	}
	fmt.Printf("  [CDS] Added 7 Clusters (version=%d)\n", ver)

	// Endpoint 추가 (EDS)
	ver = xds.AddEndpoint(EndpointConfig{
		ClusterName: "default/user-service:8080",
		Endpoints: []Endpoint{
			{Address: "10.0.1.10", Port: 8080, Weight: 1, Healthy: true},
			{Address: "10.0.1.11", Port: 8080, Weight: 1, Healthy: true},
			{Address: "10.0.1.12", Port: 8080, Weight: 1, Healthy: false}, // unhealthy
		},
	})
	fmt.Printf("  [EDS] Added Endpoints for user-service (3 endpoints, 2 healthy, version=%d)\n", ver)

	ver = xds.AddEndpoint(EndpointConfig{
		ClusterName: "default/order-service:8080",
		Endpoints: []Endpoint{
			{Address: "10.0.2.20", Port: 8080, Weight: 1, Healthy: true},
			{Address: "10.0.2.21", Port: 8080, Weight: 1, Healthy: true},
		},
	})
	fmt.Printf("  [EDS] Added Endpoints for order-service (2 endpoints, version=%d)\n", ver)

	ver = xds.AddEndpoint(EndpointConfig{
		ClusterName: "default/api-backend:8080",
		Endpoints: []Endpoint{
			{Address: "10.0.4.40", Port: 8080, Weight: 1, Healthy: true},
		},
	})

	ver = xds.AddEndpoint(EndpointConfig{
		ClusterName: "default/api-backend-v1:8080",
		Endpoints: []Endpoint{
			{Address: "10.0.4.50", Port: 8080, Weight: 1, Healthy: true},
		},
	})

	ver = xds.AddEndpoint(EndpointConfig{
		ClusterName: "default/web-frontend:3000",
		Endpoints: []Endpoint{
			{Address: "10.0.5.60", Port: 3000, Weight: 1, Healthy: true},
		},
	})
	fmt.Printf("  [EDS] Added Endpoints for remaining clusters (version=%d)\n", ver)

	// Secret 추가 (SDS)
	ver = xds.AddSecret(SecretConfig{
		Name:        "tls-cert-example",
		Certificate: "-----BEGIN CERTIFICATE-----\n(simulated TLS cert for example.com)\n-----END CERTIFICATE-----",
		PrivateKey:  "-----BEGIN PRIVATE KEY-----\n(simulated private key)\n-----END PRIVATE KEY-----",
	})
	fmt.Printf("  [SDS] Added TLS Secret 'tls-cert-example' (version=%d)\n", ver)

	// xDS 캐시 조회 시뮬레이션
	fmt.Println()
	fmt.Println("  [xDS Cache Summary]")
	fmt.Printf("    Listeners:  %d\n", len(xds.listenerCache.GetAll(ListenerType)))
	fmt.Printf("    Routes:     %d\n", len(xds.routeCache.GetAll(RouteType)))
	fmt.Printf("    Clusters:   %d\n", len(xds.clusterCache.GetAll(ClusterType)))
	fmt.Printf("    Endpoints:  %d\n", len(xds.endpointCache.GetAll(EndpointType)))
	fmt.Printf("    Secrets:    %d\n", len(xds.secretCache.GetAll(SecretType)))

	// -------------------------------------------------------------------------
	// Demo 2: DNS Proxy Simulation
	// -------------------------------------------------------------------------
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Demo 2: DNS Proxy - FQDN 기반 정책 및 DNS 캐싱")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("BPF 데이터패스가 DNS 쿼리를 가로채어 DNS 프록시로 리다이렉트한다.")
	fmt.Println("프록시는 허용된 패턴만 통과시키고, 응답 IP를 캐싱하여 FQDN Identity를 생성한다.")
	fmt.Println("(실제 코드: pkg/fqdn/doc.go, pkg/fqdn/cache.go, pkg/proxy/dns.go)")
	fmt.Println()

	dnsProxy := NewDNSProxy()

	// DNS 정책 설정: *.example.com과 s3.amazonaws.com만 허용
	dnsProxy.SetAllowedPatterns([]string{
		"*.example.com",
		"s3.amazonaws.com",
	})
	fmt.Println("  [DNS Policy] Allowed patterns: *.example.com, s3.amazonaws.com")
	fmt.Println()

	// DNS 쿼리 시뮬레이션
	dnsQueries := []struct {
		epID uint16
		fqdn string
	}{
		{1001, "api.example.com"},
		{1001, "web.example.com"},
		{1002, "s3.amazonaws.com"},
		{1001, "blocked.malware.com"}, // 차단 대상
		{1003, "db.internal.io"},       // 차단 대상 (패턴 불일치)
		{1001, "api.example.com"},      // 캐시 히트!
	}

	for _, q := range dnsQueries {
		ips, allowed := dnsProxy.Resolve(q.epID, q.fqdn)
		status := "ALLOWED"
		if !allowed {
			status = "BLOCKED"
		}

		ipStrs := make([]string, len(ips))
		for i, ip := range ips {
			ipStrs[i] = ip.String()
		}

		if allowed {
			fmt.Printf("  [DNS] ep=%d query=%s → %s IPs=%v\n", q.epID, q.fqdn, status, ipStrs)
		} else {
			fmt.Printf("  [DNS] ep=%d query=%s → %s (policy violation)\n", q.epID, q.fqdn, status)
		}
	}

	// FQDN Identity 출력
	fmt.Println()
	fmt.Println("  [FQDN Identities Created]")
	dnsProxy.mu.RLock()
	for fqdn, id := range dnsProxy.identities {
		ipStrs := make([]string, len(id.IPs))
		for i, ip := range id.IPs {
			ipStrs[i] = ip.String()
		}
		fmt.Printf("    FQDN=%s → IdentityID=%d IPs=%v\n", fqdn, id.IdentityID, ipStrs)
	}
	dnsProxy.mu.RUnlock()

	// -------------------------------------------------------------------------
	// Demo 3: L7 Proxy Flow Simulation
	// -------------------------------------------------------------------------
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Demo 3: L7 Proxy Flow - BPF → Envoy → 정책 적용 → 포워딩")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("패킷 흐름: Pod → BPF(L3/L4) → Envoy(L7) → BPF(재삽입) → Backend")
	fmt.Println("(실제 코드: pkg/proxy/proxy.go, pkg/proxy/envoyproxy.go)")
	fmt.Println()

	proxy := NewL7Proxy(xds, dnsProxy)

	testCases := []struct {
		desc       string
		srcIP      string
		dstIP      string
		dstPort    uint16
		method     string
		path       string
		host       string
		headers    map[string]string
	}{
		{
			desc: "허용: /v1/users API 호출",
			srcIP: "10.0.0.1", dstIP: "10.0.1.10", dstPort: 8080,
			method: "GET", path: "/v1/users/123", host: "api.example.com",
		},
		{
			desc: "허용: /v1/orders API 호출",
			srcIP: "10.0.0.2", dstIP: "10.0.2.20", dstPort: 8080,
			method: "POST", path: "/v1/orders", host: "api.example.com",
		},
		{
			desc: "차단: /admin 경로 (L7 정책에 의해 block)",
			srcIP: "10.0.0.3", dstIP: "10.0.3.30", dstPort: 8080,
			method: "GET", path: "/admin/dashboard", host: "api.example.com",
		},
		{
			desc: "허용: 헤더 기반 라우팅 (x-api-version: v2)",
			srcIP: "10.0.0.4", dstIP: "10.0.4.40", dstPort: 8080,
			method: "GET", path: "/api/data", host: "web.example.com",
			headers: map[string]string{"x-api-version": "v2"},
		},
		{
			desc: "허용: 기본 API 라우팅 (헤더 없음 → v1)",
			srcIP: "10.0.0.5", dstIP: "10.0.4.50", dstPort: 8080,
			method: "GET", path: "/api/data", host: "web.example.com",
		},
		{
			desc: "BPF 직접 포워딩: L7 정책 없는 포트",
			srcIP: "10.0.0.6", dstIP: "10.0.5.60", dstPort: 3306,
			method: "", path: "", host: "",
		},
		{
			desc: "차단: 존재하지 않는 경로",
			srcIP: "10.0.0.7", dstIP: "10.0.1.10", dstPort: 8080,
			method: "GET", path: "/nonexistent/path", host: "unknown.host.com",
		},
	}

	for i, tc := range testCases {
		decision := proxy.ProcessPacket(tc.srcIP, tc.dstIP, tc.dstPort, tc.method, tc.path, tc.host, tc.headers)
		icon := "[PASS]"
		if !decision.Allowed {
			icon = "[DENY]"
		}
		fmt.Printf("  %d. %s %s\n", i+1, icon, tc.desc)
		fmt.Printf("     %s %s (Host: %s)\n", tc.method, tc.path, tc.host)
		fmt.Printf("     → %s\n", decision.Reason)
		if decision.TargetBackend != "" {
			fmt.Printf("     → Backend: %s (cluster: %s)\n", decision.TargetBackend, decision.TargetCluster)
		}
		fmt.Println()
	}

	// 프록시 통계
	fmt.Println("  [Proxy Stats]")
	fmt.Printf("    Total Requests:    %d\n", proxy.stats.TotalRequests)
	fmt.Printf("    Allowed:           %d\n", proxy.stats.AllowedRequests)
	fmt.Printf("    Blocked:           %d\n", proxy.stats.BlockedRequests)
	fmt.Printf("    L7 Redirects:      %d\n", proxy.stats.L7Redirects)
	fmt.Printf("    BPF Direct:        %d\n", proxy.stats.DirectForwards)

	// -------------------------------------------------------------------------
	// Demo 4: Gateway API Simulation
	// -------------------------------------------------------------------------
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Demo 4: Gateway API → CiliumEnvoyConfig 변환")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("Gateway API 리소스를 CiliumEnvoyConfig(xDS)로 변환하는 과정:")
	fmt.Println("GatewayClass + Gateway + HTTPRoute → Translator → CEC → xDS → Envoy")
	fmt.Println("(실제 코드: operator/pkg/gateway-api/gateway_reconcile.go)")
	fmt.Println("(실제 코드: operator/pkg/model/translation/types.go)")
	fmt.Println()

	gwXDS := NewXDSServer()
	reconciler := NewGatewayAPIReconciler(gwXDS)

	// GatewayClass (Cilium 컨트롤러)
	gwClass := GatewayClass{
		Name:           "cilium",
		ControllerName: "io.cilium/gateway-controller",
	}
	fmt.Printf("  [GatewayClass] name=%s controller=%s\n", gwClass.Name, gwClass.ControllerName)

	// Gateway 정의
	gateway := Gateway{
		Name:      "my-gateway",
		Namespace: "default",
		ClassName: "cilium",
		Listeners: []GatewayListener{
			{
				Name:     "http",
				Port:     80,
				Protocol: "HTTP",
				Hostname: "*.example.com",
			},
			{
				Name:     "https",
				Port:     443,
				Protocol: "HTTPS",
				Hostname: "*.example.com",
				TLS: &GatewayTLS{
					Mode:       "Terminate",
					SecretName: "example-com-tls",
				},
			},
		},
	}
	fmt.Printf("  [Gateway] name=%s/%s listeners=[http:80, https:443]\n", gateway.Namespace, gateway.Name)

	// HTTPRoute 정의
	httpRoutes := []HTTPRoute{
		{
			Name:       "api-routes",
			Namespace:  "default",
			ParentRefs: []ParentRef{{Name: "my-gateway", Namespace: "default"}},
			Hostnames:  []string{"api.example.com"},
			Rules: []HTTPRouteRule{
				{
					Matches: []HTTPRouteMatch{
						{PathPrefix: "/v1/users"},
					},
					BackendRef: BackendRef{ServiceName: "user-svc", Port: 8080},
				},
				{
					Matches: []HTTPRouteMatch{
						{PathPrefix: "/v1/orders"},
					},
					BackendRef: BackendRef{ServiceName: "order-svc", Port: 8080},
				},
				{
					Matches: []HTTPRouteMatch{
						{PathPrefix: "/v2", Headers: map[string]string{"x-canary": "true"}},
					},
					BackendRef: BackendRef{ServiceName: "api-canary", Port: 8080},
				},
			},
		},
		{
			Name:       "web-routes",
			Namespace:  "default",
			ParentRefs: []ParentRef{{Name: "my-gateway", Namespace: "default"}},
			Hostnames:  []string{"web.example.com"},
			Rules: []HTTPRouteRule{
				{
					Matches: []HTTPRouteMatch{
						{PathPrefix: "/"},
					},
					BackendRef: BackendRef{ServiceName: "web-frontend", Port: 3000},
				},
			},
		},
	}

	for _, r := range httpRoutes {
		fmt.Printf("  [HTTPRoute] name=%s/%s hostnames=%v rules=%d\n",
			r.Namespace, r.Name, r.Hostnames, len(r.Rules))
	}

	// Reconcile 실행: Gateway API → xDS 리소스 변환
	fmt.Println()
	fmt.Println("  --- Reconcile 시작 ---")
	reconciler.Reconcile(gateway, httpRoutes)

	// 변환 결과 확인
	fmt.Println()
	fmt.Println("  [Generated xDS Resources]")
	fmt.Printf("    Listeners:  %d\n", len(gwXDS.listenerCache.GetAll(ListenerType)))
	fmt.Printf("    Routes:     %d\n", len(gwXDS.routeCache.GetAll(RouteType)))
	fmt.Printf("    Clusters:   %d\n", len(gwXDS.clusterCache.GetAll(ClusterType)))
	fmt.Printf("    Endpoints:  %d\n", len(gwXDS.endpointCache.GetAll(EndpointType)))
	fmt.Printf("    Secrets:    %d\n", len(gwXDS.secretCache.GetAll(SecretType)))

	// 생성된 Route로 L7 프록시 테스트
	fmt.Println()
	fmt.Println("  --- Gateway API HTTPRoute를 통한 요청 처리 테스트 ---")
	gwProxy := NewL7Proxy(gwXDS, dnsProxy)

	gwTestCases := []struct {
		desc    string
		method  string
		path    string
		host    string
		headers map[string]string
	}{
		{"일반 사용자 API", "GET", "/v1/users/42", "api.example.com", nil},
		{"주문 API", "POST", "/v1/orders", "api.example.com", nil},
		{"카나리 라우팅 (x-canary: true)", "GET", "/v2/new-feature", "api.example.com",
			map[string]string{"x-canary": "true"}},
		{"웹 프론트엔드", "GET", "/", "web.example.com", nil},
	}

	for _, tc := range gwTestCases {
		decision := gwProxy.ProcessPacket("10.0.0.1", "10.0.1.1", 80, tc.method, tc.path, tc.host, tc.headers)
		icon := "[PASS]"
		if !decision.Allowed {
			icon = "[DENY]"
		}
		fmt.Printf("  %s %s %s (Host: %s)\n", icon, tc.method, tc.path, tc.host)
		if tc.headers != nil {
			fmt.Printf("       Headers: %v\n", tc.headers)
		}
		fmt.Printf("       → %s", decision.Reason)
		if decision.TargetBackend != "" {
			fmt.Printf(" [backend=%s]", decision.TargetBackend)
		}
		fmt.Println()
	}

	// -------------------------------------------------------------------------
	// Demo 5: Per-Node Proxy vs Sidecar Model
	// -------------------------------------------------------------------------
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Demo 5: Per-Node 프록시 vs 사이드카 모델 비교")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("Cilium은 노드당 1개의 공유 Envoy 프록시를 사용한다.")
	fmt.Println("전통적인 사이드카 모델은 Pod당 1개의 프록시를 배치한다.")
	fmt.Println()

	// 클러스터 시뮬레이션: 10개 노드, 각 50개 Pod
	nodes := 10
	podsPerNode := 50

	fmt.Printf("  클러스터 규모: %d 노드 × %d Pod/노드 = %d Pod\n\n", nodes, podsPerNode, nodes*podsPerNode)

	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                  Per-Node 모델 (Cilium)                       │")
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")

	totalPerNode := 0
	for i := 0; i < nodes; i++ {
		model := simulatePerNodeModel(fmt.Sprintf("node-%02d", i), podsPerNode)
		totalPerNode += model.TotalMemory
		if i < 3 { // 처음 3개만 출력
			fmt.Printf("  │ %s: %d Pods, %d Envoy, Memory=%dMB                     │\n",
				model.NodeName, model.PodCount, model.ProxyCount, model.TotalMemory)
		}
	}
	fmt.Printf("  │ ... (%d개 노드 더)                                           │\n", nodes-3)
	fmt.Printf("  │ 총 프록시 수: %d, 총 메모리: %dMB                             │\n", nodes, totalPerNode)
	fmt.Println("  └──────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                  사이드카 모델 (전통적 서비스 메시)             │")
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")

	totalSidecar := 0
	for i := 0; i < nodes; i++ {
		model := simulateSidecarModel(fmt.Sprintf("node-%02d", i), podsPerNode)
		totalSidecar += model.TotalMemory
		if i < 3 {
			fmt.Printf("  │ %s: %d Pods, %d Envoy sidecars, Memory=%dMB          │\n",
				model.NodeName, model.PodCount, model.ProxyCount, model.TotalMemory)
		}
	}
	fmt.Printf("  │ ... (%d개 노드 더)                                           │\n", nodes-3)
	fmt.Printf("  │ 총 프록시 수: %d, 총 메모리: %dMB                         │\n", nodes*podsPerNode, totalSidecar)
	fmt.Println("  └──────────────────────────────────────────────────────────────┘")

	fmt.Println()
	savings := float64(totalSidecar-totalPerNode) / float64(totalSidecar) * 100
	fmt.Printf("  메모리 절약: %dMB → %dMB (%.1f%% 절약)\n", totalSidecar, totalPerNode, savings)
	fmt.Printf("  프록시 수 절감: %d → %d (%.0fx 감소)\n", nodes*podsPerNode, nodes, float64(nodes*podsPerNode)/float64(nodes))

	fmt.Println()
	fmt.Println("  추가 장점 (Per-Node 모델):")
	fmt.Println("    - 사이드카 주입 불필요 → 배포 단순화")
	fmt.Println("    - Pod 재시작 없이 프록시 업그레이드 가능")
	fmt.Println("    - BPF 기반 리다이렉트 → iptables 불필요")
	fmt.Println("    - L7 정책이 필요한 트래픽만 선택적 프록시 경유")

	// -------------------------------------------------------------------------
	// Demo 6: HTTP 서버 통합 데모 (선택적)
	// -------------------------------------------------------------------------
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Demo 6: 실제 HTTP 서버를 사용한 L7 프록시 시뮬레이션")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  HTTP 프록시 서버를 시작합니다 (localhost:19080)")
	fmt.Println("  이 서버는 Cilium의 per-node Envoy 프록시 동작을 시뮬레이션합니다.")
	fmt.Println()

	// 간단한 HTTP 프록시 서버 시작
	mux := http.NewServeMux()

	// /proxy 엔드포인트: L7 정책 적용 시뮬레이션
	mux.HandleFunc("/proxy", func(w http.ResponseWriter, r *http.Request) {
		targetHost := r.Header.Get("X-Target-Host")
		targetPath := r.Header.Get("X-Target-Path")
		if targetHost == "" {
			targetHost = "api.example.com"
		}
		if targetPath == "" {
			targetPath = "/"
		}

		decision := proxy.ProcessPacket(
			r.RemoteAddr, "10.0.1.10", 8080,
			r.Method, targetPath, targetHost, nil,
		)

		w.Header().Set("Content-Type", "application/json")
		if decision.Allowed {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"allowed":true,"reason":%q,"backend":%q,"route":%q}`+"\n",
				decision.Reason, decision.TargetBackend, decision.MatchedRoute)
		} else {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"allowed":false,"reason":%q}`+"\n", decision.Reason)
		}
	})

	// /dns 엔드포인트: DNS 프록시 시뮬레이션
	mux.HandleFunc("/dns", func(w http.ResponseWriter, r *http.Request) {
		fqdn := r.URL.Query().Get("fqdn")
		if fqdn == "" {
			http.Error(w, "missing 'fqdn' query parameter", http.StatusBadRequest)
			return
		}

		ips, allowed := dnsProxy.Resolve(1000, fqdn)

		w.Header().Set("Content-Type", "application/json")
		ipStrs := make([]string, len(ips))
		for i, ip := range ips {
			ipStrs[i] = ip.String()
		}
		fmt.Fprintf(w, `{"fqdn":%q,"allowed":%v,"ips":%q}`+"\n", fqdn, allowed, ipStrs)
	})

	// /stats 엔드포인트: 프록시 통계
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		proxy.mu.Lock()
		fmt.Fprintf(w, `{"total":%d,"allowed":%d,"blocked":%d,"l7_redirects":%d,"bpf_direct":%d}`+"\n",
			proxy.stats.TotalRequests, proxy.stats.AllowedRequests,
			proxy.stats.BlockedRequests, proxy.stats.L7Redirects, proxy.stats.DirectForwards)
		proxy.mu.Unlock()
	})

	// /xds 엔드포인트: xDS 캐시 요약
	mux.HandleFunc("/xds", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"listeners":%d,"routes":%d,"clusters":%d,"endpoints":%d,"secrets":%d}`+"\n",
			len(xds.listenerCache.GetAll(ListenerType)),
			len(xds.routeCache.GetAll(RouteType)),
			len(xds.clusterCache.GetAll(ClusterType)),
			len(xds.endpointCache.GetAll(EndpointType)),
			len(xds.secretCache.GetAll(SecretType)))
	})

	server := &http.Server{
		Addr:    ":19080",
		Handler: mux,
	}

	fmt.Println("  사용 예시:")
	fmt.Println("    curl http://localhost:19080/proxy -H 'X-Target-Host: api.example.com' -H 'X-Target-Path: /v1/users/1'")
	fmt.Println("    curl http://localhost:19080/proxy -H 'X-Target-Host: api.example.com' -H 'X-Target-Path: /admin'")
	fmt.Println("    curl http://localhost:19080/dns?fqdn=api.example.com")
	fmt.Println("    curl http://localhost:19080/dns?fqdn=blocked.malware.com")
	fmt.Println("    curl http://localhost:19080/stats")
	fmt.Println("    curl http://localhost:19080/xds")
	fmt.Println()
	fmt.Println("  Ctrl+C로 종료")

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTP server error: %v", err)
	}
}
