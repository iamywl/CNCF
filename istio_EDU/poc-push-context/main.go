package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Istio PushContext 스냅샷 및 인덱싱 시뮬레이션
// =============================================================================
//
// 이 PoC는 Istio Pilot(istiod)의 PushContext가 설정 변경 시 불변 스냅샷을
// 생성하고, 서비스/VirtualService/DestinationRule을 인덱싱하는 메커니즘을 재현한다.
//
// 실제 소스 참조:
//   - pilot/pkg/model/push_context.go (PushContext, serviceIndex, virtualServiceIndex, destinationRuleIndex)
//   - pilot/pkg/model/push_context.go (serviceIndex: privateByNamespace, public, exportedToNamespace)
//   - pilot/pkg/model/push_context.go (exportToDefaults, visibility 필터링)
//   - pilot/pkg/model/sidecar.go (SidecarScope)
//
// 핵심 원리:
//   1. 설정 변경 시 새 PushContext를 생성하고 모든 인덱스를 재구축
//   2. 인덱스가 완성되면 불변 스냅샷으로 취급 → 동시 읽기 안전
//   3. 서비스 가시성(exportTo)에 따라 네임스페이스별 필터링
//   4. SidecarScope로 프록시에 필요한 서비스만 선택
// =============================================================================

// --- Visibility (가시성) 모델 ---

// Visibility는 서비스/설정의 가시성 범위를 나타낸다.
// 실제 코드: pkg/config/visibility/visibility.go
type Visibility string

const (
	VisibilityPublic  Visibility = "*" // 전체 메시에서 접근 가능
	VisibilityPrivate Visibility = "." // 같은 네임스페이스에서만 접근 가능
	VisibilityNone    Visibility = "~" // 누구에게도 노출하지 않음
)

// --- Service 모델 ---

// Service는 Istio 서비스를 나타낸다.
// 실제 코드: pilot/pkg/model/service.go의 Service 구조체
type Service struct {
	Hostname  string            // 서비스 호스트명 (e.g., "reviews.default.svc.cluster.local")
	Namespace string            // 네임스페이스
	Ports     []ServicePort     // 서비스 포트
	ExportTo  map[Visibility]bool // exportTo 설정 ("*", ".", "namespace-name")
}

// ServicePort는 서비스 포트를 나타낸다.
type ServicePort struct {
	Name     string
	Port     int
	Protocol string
}

// --- VirtualService 모델 ---

// VirtualService는 Istio VirtualService 설정을 나타낸다.
// 실제 코드: pilot/pkg/model/push_context.go의 virtualServiceIndex
type VirtualService struct {
	Name      string
	Namespace string
	Hosts     []string          // 적용 대상 호스트
	Gateways  []string          // 적용 게이트웨이 ("mesh" = 메시 내부)
	ExportTo  map[Visibility]bool
	HTTPRoutes []HTTPRoute      // HTTP 라우팅 규칙
}

// HTTPRoute는 HTTP 라우팅 규칙이다.
type HTTPRoute struct {
	Match []HTTPMatchRequest
	Route []HTTPRouteDestination
}

type HTTPMatchRequest struct {
	URI string
}

type HTTPRouteDestination struct {
	Host   string
	Subset string
	Weight int
}

// --- DestinationRule 모델 ---

// DestinationRule은 Istio DestinationRule 설정을 나타낸다.
type DestinationRule struct {
	Name      string
	Namespace string
	Host      string                  // 적용 대상 호스트
	ExportTo  map[Visibility]bool
	Subsets   []Subset                // 서브셋 정의
	TrafficPolicy *TrafficPolicy     // 트래픽 정책
}

type Subset struct {
	Name   string
	Labels map[string]string
}

type TrafficPolicy struct {
	ConnectionPool *ConnectionPool
	LoadBalancer   string
}

type ConnectionPool struct {
	MaxConnections int
}

// --- ConfigKey (설정 키) ---

// ConfigKey는 설정 리소스를 고유하게 식별한다.
// 실제 코드: pilot/pkg/model/push_context.go의 ConfigKey
type ConfigKey struct {
	Kind      string
	Name      string
	Namespace string
}

func (ck ConfigKey) String() string {
	return fmt.Sprintf("%s/%s/%s", ck.Kind, ck.Namespace, ck.Name)
}

// --- serviceIndex (서비스 인덱스) ---

// serviceIndex는 서비스를 다양한 기준으로 인덱싱한다.
// 실제 코드: pilot/pkg/model/push_context.go의 serviceIndex 구조체
//
// 핵심 필드:
//   - privateByNamespace: exportTo "."인 서비스 (같은 네임스페이스에서만 접근)
//   - public: exportTo "*"인 서비스 (전체 메시에서 접근)
//   - exportedToNamespace: 특정 네임스페이스에 명시적으로 export된 서비스
//   - HostnameAndNamespace: 호스트명+네임스페이스 기반 룩업
type serviceIndex struct {
	privateByNamespace   map[string][]*Service // ns -> services (exportTo ".")
	public               []*Service            // exportTo "*"
	exportedToNamespace  map[string][]*Service // targetNs -> services
	HostnameAndNamespace map[string]map[string]*Service // hostname -> ns -> service
}

func newServiceIndex() serviceIndex {
	return serviceIndex{
		privateByNamespace:   make(map[string][]*Service),
		public:               []*Service{},
		exportedToNamespace:  make(map[string][]*Service),
		HostnameAndNamespace: make(map[string]map[string]*Service),
	}
}

// --- virtualServiceIndex ---

// virtualServiceIndex는 VirtualService를 게이트웨이/네임스페이스별로 인덱싱한다.
// 실제 코드: pilot/pkg/model/push_context.go의 virtualServiceIndex
type virtualServiceIndex struct {
	publicByGateway              map[string][]*VirtualService          // gateway -> vs list
	privateByNamespaceAndGateway map[string][]*VirtualService          // ns:gateway -> vs list
	exportedToNamespaceByGateway map[string][]*VirtualService          // ns:gateway -> vs list
	referencedDestinations       map[string]map[string]bool            // vs host -> dest hosts
}

func newVirtualServiceIndex() virtualServiceIndex {
	return virtualServiceIndex{
		publicByGateway:              make(map[string][]*VirtualService),
		privateByNamespaceAndGateway: make(map[string][]*VirtualService),
		exportedToNamespaceByGateway: make(map[string][]*VirtualService),
		referencedDestinations:       make(map[string]map[string]bool),
	}
}

// --- destinationRuleIndex ---

// destinationRuleIndex는 DestinationRule을 네임스페이스/호스트별로 인덱싱한다.
// 실제 코드: pilot/pkg/model/push_context.go의 destinationRuleIndex
type destinationRuleIndex struct {
	namespaceLocal      map[string]map[string]*DestinationRule // ns -> host -> DR
	exportedByNamespace map[string]map[string]*DestinationRule // ns -> host -> DR
}

func newDestinationRuleIndex() destinationRuleIndex {
	return destinationRuleIndex{
		namespaceLocal:      make(map[string]map[string]*DestinationRule),
		exportedByNamespace: make(map[string]map[string]*DestinationRule),
	}
}

// --- SidecarScope ---

// SidecarScope는 특정 네임스페이스의 사이드카가 볼 수 있는 서비스를 제한한다.
// 실제 코드: pilot/pkg/model/sidecar.go
// 핵심 목적: 프록시에 필요한 설정만 전달하여 메모리와 네트워크 사용량 최적화
type SidecarScope struct {
	Name       string
	Namespace  string
	EgressHosts map[string]map[string]bool // ns -> hosts (허용 호스트)
}

// AllowsService는 이 SidecarScope가 특정 서비스를 허용하는지 확인한다.
func (ss *SidecarScope) AllowsService(svc *Service) bool {
	if ss == nil {
		return true // SidecarScope 없으면 모든 서비스 허용 (기본값)
	}

	// 같은 네임스페이스의 모든 서비스 허용하는지 체크
	if hosts, ok := ss.EgressHosts[svc.Namespace]; ok {
		if hosts["*"] || hosts[svc.Hostname] {
			return true
		}
	}
	// "*" 네임스페이스(전체)에서 허용하는지 체크
	if hosts, ok := ss.EgressHosts["*"]; ok {
		if hosts["*"] || hosts[svc.Hostname] {
			return true
		}
	}
	return false
}

// --- PushContext ---

// PushContext는 설정 변경 시 생성되는 불변 스냅샷이다.
// 프록시에 push할 때 이 스냅샷을 기준으로 설정을 생성한다.
//
// 실제 코드: pilot/pkg/model/push_context.go
// 핵심 특성:
//   1. 불변(immutable): 한번 생성되면 변경되지 않음
//   2. 인덱싱: 다양한 기준으로 빠른 룩업 제공
//   3. 스레드 안전: 동시 읽기 가능 (불변이므로)
type PushContext struct {
	mu sync.RWMutex

	// 버전 정보
	PushVersion string
	CreateTime  time.Time

	// 인덱스
	ServiceIndex         serviceIndex
	VirtualServiceIndex  virtualServiceIndex
	DestinationRuleIndex destinationRuleIndex

	// SidecarScope 캐시
	sidecarsByNamespace map[string]*SidecarScope

	// 불변 플래그
	initialized bool
}

// NewPushContext는 새 PushContext를 생성한다.
func NewPushContext() *PushContext {
	return &PushContext{
		ServiceIndex:         newServiceIndex(),
		VirtualServiceIndex:  newVirtualServiceIndex(),
		DestinationRuleIndex: newDestinationRuleIndex(),
		sidecarsByNamespace:  make(map[string]*SidecarScope),
		CreateTime:           time.Now(),
	}
}

// InitContext는 모든 설정을 로드하고 인덱스를 구축한다.
// 이 메서드가 완료되면 PushContext는 불변이 된다.
// 실제 코드: pilot/pkg/model/push_context.go의 InitContext
func (pc *PushContext) InitContext(
	services []*Service,
	virtualServices []*VirtualService,
	destinationRules []*DestinationRule,
	sidecars []*SidecarScope,
	version string,
) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.PushVersion = version

	// 1. 서비스 인덱싱
	pc.initServiceIndex(services)

	// 2. VirtualService 인덱싱
	pc.initVirtualServiceIndex(virtualServices)

	// 3. DestinationRule 인덱싱
	pc.initDestinationRuleIndex(destinationRules)

	// 4. SidecarScope 등록
	for _, sc := range sidecars {
		pc.sidecarsByNamespace[sc.Namespace] = sc
	}

	pc.initialized = true
}

// initServiceIndex는 서비스를 가시성(exportTo)에 따라 인덱싱한다.
// 실제 코드: pilot/pkg/model/push_context.go line ~1573
func (pc *PushContext) initServiceIndex(services []*Service) {
	for _, svc := range services {
		// HostnameAndNamespace 인덱스 항상 업데이트
		if pc.ServiceIndex.HostnameAndNamespace[svc.Hostname] == nil {
			pc.ServiceIndex.HostnameAndNamespace[svc.Hostname] = make(map[string]*Service)
		}
		pc.ServiceIndex.HostnameAndNamespace[svc.Hostname][svc.Namespace] = svc

		// exportTo에 따른 분류
		if len(svc.ExportTo) == 0 {
			// exportTo 미설정 → 기본값(public)
			pc.ServiceIndex.public = append(pc.ServiceIndex.public, svc)
		} else if svc.ExportTo[VisibilityPublic] {
			// exportTo "*" → public
			pc.ServiceIndex.public = append(pc.ServiceIndex.public, svc)
		} else if svc.ExportTo[VisibilityNone] {
			// exportTo "~" → 누구에게도 노출하지 않음
			// 아무 인덱스에도 추가하지 않음
		} else if svc.ExportTo[VisibilityPrivate] {
			// exportTo "." → 같은 네임스페이스에서만 접근
			pc.ServiceIndex.privateByNamespace[svc.Namespace] = append(
				pc.ServiceIndex.privateByNamespace[svc.Namespace], svc)
		} else {
			// 특정 네임스페이스에 export
			for vis := range svc.ExportTo {
				ns := string(vis)
				if ns != string(VisibilityPublic) && ns != string(VisibilityPrivate) && ns != string(VisibilityNone) {
					pc.ServiceIndex.exportedToNamespace[ns] = append(
						pc.ServiceIndex.exportedToNamespace[ns], svc)
				}
			}
		}
	}
}

// initVirtualServiceIndex는 VirtualService를 게이트웨이/가시성별로 인덱싱한다.
func (pc *PushContext) initVirtualServiceIndex(virtualServices []*VirtualService) {
	for _, vs := range virtualServices {
		for _, gw := range vs.Gateways {
			if len(vs.ExportTo) == 0 || vs.ExportTo[VisibilityPublic] {
				pc.VirtualServiceIndex.publicByGateway[gw] = append(
					pc.VirtualServiceIndex.publicByGateway[gw], vs)
			} else if vs.ExportTo[VisibilityPrivate] {
				key := vs.Namespace + ":" + gw
				pc.VirtualServiceIndex.privateByNamespaceAndGateway[key] = append(
					pc.VirtualServiceIndex.privateByNamespaceAndGateway[key], vs)
			}
		}

		// 참조된 목적지 호스트 인덱싱
		for _, host := range vs.Hosts {
			dests := make(map[string]bool)
			for _, route := range vs.HTTPRoutes {
				for _, dest := range route.Route {
					dests[dest.Host] = true
				}
			}
			pc.VirtualServiceIndex.referencedDestinations[host] = dests
		}
	}
}

// initDestinationRuleIndex는 DestinationRule을 네임스페이스/호스트별로 인덱싱한다.
func (pc *PushContext) initDestinationRuleIndex(destinationRules []*DestinationRule) {
	for _, dr := range destinationRules {
		// 네임스페이스 로컬 인덱스
		if pc.DestinationRuleIndex.namespaceLocal[dr.Namespace] == nil {
			pc.DestinationRuleIndex.namespaceLocal[dr.Namespace] = make(map[string]*DestinationRule)
		}
		pc.DestinationRuleIndex.namespaceLocal[dr.Namespace][dr.Host] = dr

		// 다른 네임스페이스에 export
		if dr.ExportTo[VisibilityPublic] || len(dr.ExportTo) == 0 {
			if pc.DestinationRuleIndex.exportedByNamespace["*"] == nil {
				pc.DestinationRuleIndex.exportedByNamespace["*"] = make(map[string]*DestinationRule)
			}
			pc.DestinationRuleIndex.exportedByNamespace["*"][dr.Host] = dr
		}
	}
}

// --- 조회 메서드 ---

// ServicesForNamespace는 특정 네임스페이스에서 볼 수 있는 서비스 목록을 반환한다.
// 실제 코드의 가시성 필터링 로직을 재현한다.
func (pc *PushContext) ServicesForNamespace(namespace string) []*Service {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	var services []*Service

	// 1. public 서비스 (exportTo "*")
	services = append(services, pc.ServiceIndex.public...)

	// 2. private 서비스 (같은 네임스페이스)
	if private, ok := pc.ServiceIndex.privateByNamespace[namespace]; ok {
		services = append(services, private...)
	}

	// 3. 이 네임스페이스에 명시적으로 export된 서비스
	if exported, ok := pc.ServiceIndex.exportedToNamespace[namespace]; ok {
		services = append(services, exported...)
	}

	return services
}

// ServicesForSidecar는 SidecarScope를 적용하여 필터링된 서비스 목록을 반환한다.
func (pc *PushContext) ServicesForSidecar(namespace string) []*Service {
	allServices := pc.ServicesForNamespace(namespace)

	scope := pc.sidecarsByNamespace[namespace]
	if scope == nil {
		return allServices // SidecarScope 없으면 모든 서비스 반환
	}

	var filtered []*Service
	for _, svc := range allServices {
		if scope.AllowsService(svc) {
			filtered = append(filtered, svc)
		}
	}
	return filtered
}

// VirtualServicesForHost는 특정 호스트에 대한 VirtualService를 찾는다.
func (pc *PushContext) VirtualServicesForHost(host, gateway, namespace string) []*VirtualService {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	var result []*VirtualService

	// public VS 검색
	for _, vs := range pc.VirtualServiceIndex.publicByGateway[gateway] {
		for _, h := range vs.Hosts {
			if matchHost(h, host) {
				result = append(result, vs)
				break
			}
		}
	}

	// private VS 검색 (같은 네임스페이스)
	key := namespace + ":" + gateway
	for _, vs := range pc.VirtualServiceIndex.privateByNamespaceAndGateway[key] {
		for _, h := range vs.Hosts {
			if matchHost(h, host) {
				result = append(result, vs)
				break
			}
		}
	}

	return result
}

// DestinationRuleForHost는 특정 호스트에 대한 DestinationRule을 찾는다.
func (pc *PushContext) DestinationRuleForHost(host, clientNamespace string) *DestinationRule {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	// 1. 클라이언트 네임스페이스에서 먼저 찾기
	if nsRules, ok := pc.DestinationRuleIndex.namespaceLocal[clientNamespace]; ok {
		if dr, ok := nsRules[host]; ok {
			return dr
		}
	}

	// 2. public export된 DR에서 찾기
	if globalRules, ok := pc.DestinationRuleIndex.exportedByNamespace["*"]; ok {
		if dr, ok := globalRules[host]; ok {
			return dr
		}
	}

	return nil
}

// SubsetForHost는 호스트+서브셋 조합으로 서브셋을 찾는다.
func (pc *PushContext) SubsetForHost(host, subsetName, clientNamespace string) *Subset {
	dr := pc.DestinationRuleForHost(host, clientNamespace)
	if dr == nil {
		return nil
	}
	for i := range dr.Subsets {
		if dr.Subsets[i].Name == subsetName {
			return &dr.Subsets[i]
		}
	}
	return nil
}

// --- 호스트 매칭 ---

// matchHost는 VirtualService 호스트 패턴과 실제 호스트를 매칭한다.
// "*" = 전체 매치, "*.example.com" = suffix 매치
func matchHost(pattern, host string) bool {
	if pattern == "*" {
		return true
	}
	if pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix)
	}
	return false
}

// --- Environment (환경) ---

// Environment는 PushContext를 관리하는 상위 구조체이다.
// 실제 코드: pilot/pkg/model/environment.go
type Environment struct {
	mu          sync.RWMutex
	pushContext *PushContext
}

// SetPushContext는 새 PushContext를 설정한다.
// 원자적으로 교체하여 읽기 중인 이전 PushContext에 영향을 주지 않는다.
func (env *Environment) SetPushContext(pc *PushContext) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.pushContext = pc
}

// PushContext는 현재 활성 PushContext를 반환한다.
func (env *Environment) PushContext() *PushContext {
	env.mu.RLock()
	defer env.mu.RUnlock()
	return env.pushContext
}

// --- 유틸리티 ---

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println()
}

// --- main ---

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║         Istio PushContext 스냅샷 및 인덱싱 시뮬레이션               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// ==========================================================================
	// 테스트 데이터 생성
	// ==========================================================================

	services := []*Service{
		{
			Hostname:  "productpage.default.svc.cluster.local",
			Namespace: "default",
			Ports:     []ServicePort{{Name: "http", Port: 9080, Protocol: "HTTP"}},
			ExportTo:  map[Visibility]bool{VisibilityPublic: true}, // 전체 공개
		},
		{
			Hostname:  "reviews.default.svc.cluster.local",
			Namespace: "default",
			Ports:     []ServicePort{{Name: "http", Port: 9080, Protocol: "HTTP"}},
			ExportTo:  map[Visibility]bool{VisibilityPublic: true},
		},
		{
			Hostname:  "ratings.default.svc.cluster.local",
			Namespace: "default",
			Ports:     []ServicePort{{Name: "http", Port: 9080, Protocol: "HTTP"}},
			ExportTo:  map[Visibility]bool{VisibilityPrivate: true}, // 같은 NS에서만
		},
		{
			Hostname:  "details.default.svc.cluster.local",
			Namespace: "default",
			Ports:     []ServicePort{{Name: "http", Port: 9080, Protocol: "HTTP"}},
			ExportTo:  map[Visibility]bool{Visibility("istio-system"): true}, // istio-system에만 export
		},
		{
			Hostname:  "mysql.database.svc.cluster.local",
			Namespace: "database",
			Ports:     []ServicePort{{Name: "tcp", Port: 3306, Protocol: "TCP"}},
			ExportTo:  map[Visibility]bool{Visibility("default"): true}, // default에만 export
		},
		{
			Hostname:  "redis.cache.svc.cluster.local",
			Namespace: "cache",
			Ports:     []ServicePort{{Name: "tcp", Port: 6379, Protocol: "TCP"}},
			ExportTo:  map[Visibility]bool{VisibilityPublic: true},
		},
		{
			Hostname:  "internal-api.backend.svc.cluster.local",
			Namespace: "backend",
			Ports:     []ServicePort{{Name: "grpc", Port: 8080, Protocol: "gRPC"}},
			ExportTo:  map[Visibility]bool{VisibilityNone: true}, // 아무에게도 공개하지 않음
		},
	}

	virtualServices := []*VirtualService{
		{
			Name:      "reviews-route",
			Namespace: "default",
			Hosts:     []string{"reviews.default.svc.cluster.local"},
			Gateways:  []string{"mesh"},
			ExportTo:  map[Visibility]bool{VisibilityPublic: true},
			HTTPRoutes: []HTTPRoute{
				{
					Match: []HTTPMatchRequest{{URI: "/reviews"}},
					Route: []HTTPRouteDestination{
						{Host: "reviews.default.svc.cluster.local", Subset: "v1", Weight: 80},
						{Host: "reviews.default.svc.cluster.local", Subset: "v2", Weight: 20},
					},
				},
			},
		},
		{
			Name:      "ratings-canary",
			Namespace: "default",
			Hosts:     []string{"ratings.default.svc.cluster.local"},
			Gateways:  []string{"mesh"},
			ExportTo:  map[Visibility]bool{VisibilityPrivate: true}, // default NS에서만 보임
			HTTPRoutes: []HTTPRoute{
				{
					Route: []HTTPRouteDestination{
						{Host: "ratings.default.svc.cluster.local", Subset: "v1", Weight: 90},
						{Host: "ratings.default.svc.cluster.local", Subset: "v2", Weight: 10},
					},
				},
			},
		},
		{
			Name:      "bookinfo-gateway",
			Namespace: "default",
			Hosts:     []string{"*.bookinfo.com"},
			Gateways:  []string{"bookinfo-gateway"},
			ExportTo:  map[Visibility]bool{VisibilityPublic: true},
			HTTPRoutes: []HTTPRoute{
				{
					Route: []HTTPRouteDestination{
						{Host: "productpage.default.svc.cluster.local", Weight: 100},
					},
				},
			},
		},
	}

	destinationRules := []*DestinationRule{
		{
			Name:      "reviews-dr",
			Namespace: "default",
			Host:      "reviews.default.svc.cluster.local",
			ExportTo:  map[Visibility]bool{VisibilityPublic: true},
			Subsets: []Subset{
				{Name: "v1", Labels: map[string]string{"version": "v1"}},
				{Name: "v2", Labels: map[string]string{"version": "v2"}},
				{Name: "v3", Labels: map[string]string{"version": "v3"}},
			},
			TrafficPolicy: &TrafficPolicy{
				LoadBalancer: "ROUND_ROBIN",
				ConnectionPool: &ConnectionPool{MaxConnections: 100},
			},
		},
		{
			Name:      "ratings-dr",
			Namespace: "default",
			Host:      "ratings.default.svc.cluster.local",
			Subsets: []Subset{
				{Name: "v1", Labels: map[string]string{"version": "v1"}},
				{Name: "v2", Labels: map[string]string{"version": "v2-mysql"}},
			},
		},
		{
			Name:      "redis-dr",
			Namespace: "cache",
			Host:      "redis.cache.svc.cluster.local",
			ExportTo:  map[Visibility]bool{VisibilityPublic: true},
			TrafficPolicy: &TrafficPolicy{
				LoadBalancer: "LEAST_CONN",
			},
		},
	}

	sidecars := []*SidecarScope{
		{
			Name:      "default-sidecar",
			Namespace: "default",
			EgressHosts: map[string]map[string]bool{
				"default": {"*": true},                           // default NS 내 모든 서비스
				"cache":   {"redis.cache.svc.cluster.local": true}, // cache NS의 redis만
			},
		},
	}

	// ==========================================================================
	// 1. PushContext 생성 및 초기화
	// ==========================================================================
	printSeparator("1. PushContext 생성 및 인덱스 구축")

	env := &Environment{}
	pc := NewPushContext()
	pc.InitContext(services, virtualServices, destinationRules, sidecars, "v1-2024-01-01/1")
	env.SetPushContext(pc)

	fmt.Printf("  PushContext 버전: %s\n", pc.PushVersion)
	fmt.Printf("  생성 시각: %s\n", pc.CreateTime.Format(time.RFC3339))
	fmt.Printf("  초기화 완료: %v\n", pc.initialized)

	fmt.Printf("\n  서비스 인덱스:\n")
	fmt.Printf("    public 서비스: %d개\n", len(pc.ServiceIndex.public))
	fmt.Printf("    private 네임스페이스: %d개\n", len(pc.ServiceIndex.privateByNamespace))
	fmt.Printf("    명시적 export 대상 네임스페이스: %d개\n", len(pc.ServiceIndex.exportedToNamespace))
	fmt.Printf("    호스트명 인덱스 항목: %d개\n", len(pc.ServiceIndex.HostnameAndNamespace))

	// ==========================================================================
	// 2. ServiceIndex - 가시성(Visibility) 필터링
	// ==========================================================================
	printSeparator("2. ServiceIndex - 네임스페이스별 서비스 가시성")

	namespaces := []string{"default", "istio-system", "cache", "backend", "other"}
	for _, ns := range namespaces {
		svcs := pc.ServicesForNamespace(ns)
		fmt.Printf("  네임스페이스 '%s'에서 보이는 서비스 (%d개):\n", ns, len(svcs))
		for _, svc := range svcs {
			fmt.Printf("    - %s (from: %s)\n", svc.Hostname, svc.Namespace)
		}
		fmt.Println()
	}

	// ==========================================================================
	// 3. VirtualService 인덱스 - 호스트 매칭
	// ==========================================================================
	printSeparator("3. VirtualService 인덱스 - 호스트 매칭 테스트")

	type vsTest struct {
		host    string
		gateway string
		ns      string
	}
	vsTests := []vsTest{
		{"reviews.default.svc.cluster.local", "mesh", "default"},
		{"ratings.default.svc.cluster.local", "mesh", "default"},
		{"ratings.default.svc.cluster.local", "mesh", "istio-system"}, // private이므로 안 보임
		{"app.bookinfo.com", "bookinfo-gateway", "default"},
		{"unknown.host.com", "mesh", "default"},
	}

	for _, t := range vsTests {
		vss := pc.VirtualServicesForHost(t.host, t.gateway, t.ns)
		if len(vss) > 0 {
			for _, vs := range vss {
				fmt.Printf("  호스트='%s' 게이트웨이='%s' NS='%s'\n", t.host, t.gateway, t.ns)
				fmt.Printf("    -> VS '%s/%s' 매치!\n", vs.Namespace, vs.Name)
				for _, route := range vs.HTTPRoutes {
					for _, dest := range route.Route {
						fmt.Printf("       라우트: %s (subset=%s, weight=%d%%)\n",
							dest.Host, dest.Subset, dest.Weight)
					}
				}
			}
		} else {
			fmt.Printf("  호스트='%s' 게이트웨이='%s' NS='%s'\n", t.host, t.gateway, t.ns)
			fmt.Printf("    -> 매칭 VS 없음\n")
		}
		fmt.Println()
	}

	// ==========================================================================
	// 4. DestinationRule 인덱스 - 호스트+서브셋 룩업
	// ==========================================================================
	printSeparator("4. DestinationRule 인덱스 - 호스트+서브셋 룩업")

	type drTest struct {
		host    string
		subset  string
		ns      string
	}
	drTests := []drTest{
		{"reviews.default.svc.cluster.local", "v1", "default"},
		{"reviews.default.svc.cluster.local", "v2", "default"},
		{"reviews.default.svc.cluster.local", "v3", "istio-system"}, // public이므로 보임
		{"redis.cache.svc.cluster.local", "", "default"},
		{"unknown.host", "", "default"},
	}

	for _, t := range drTests {
		dr := pc.DestinationRuleForHost(t.host, t.ns)
		if dr != nil {
			fmt.Printf("  호스트='%s' NS='%s'\n", t.host, t.ns)
			fmt.Printf("    -> DR '%s/%s' (LB: %s)\n", dr.Namespace, dr.Name,
				func() string {
					if dr.TrafficPolicy != nil {
						return dr.TrafficPolicy.LoadBalancer
					}
					return "기본값"
				}())
			if t.subset != "" {
				sub := pc.SubsetForHost(t.host, t.subset, t.ns)
				if sub != nil {
					fmt.Printf("    -> Subset '%s': labels=%v\n", sub.Name, sub.Labels)
				} else {
					fmt.Printf("    -> Subset '%s': 없음\n", t.subset)
				}
			}
			fmt.Printf("    -> 전체 서브셋: ")
			names := make([]string, len(dr.Subsets))
			for i, s := range dr.Subsets {
				names[i] = s.Name
			}
			fmt.Printf("[%s]\n", strings.Join(names, ", "))
		} else {
			fmt.Printf("  호스트='%s' NS='%s' -> DR 없음\n", t.host, t.ns)
		}
		fmt.Println()
	}

	// ==========================================================================
	// 5. SidecarScope - 네임스페이스 레벨 서비스 필터링
	// ==========================================================================
	printSeparator("5. SidecarScope - 네임스페이스 레벨 서비스 필터링")

	fmt.Println("  default 네임스페이스 SidecarScope 설정:")
	fmt.Println("    egress:")
	fmt.Println("      - default/*            (default NS 내 모든 서비스)")
	fmt.Println("      - cache/redis.cache... (cache NS의 redis만)")
	fmt.Println()

	allSvcs := pc.ServicesForNamespace("default")
	filteredSvcs := pc.ServicesForSidecar("default")

	fmt.Printf("  SidecarScope 적용 전 (가시성 필터링만): %d개\n", len(allSvcs))
	for _, svc := range allSvcs {
		fmt.Printf("    - %s\n", svc.Hostname)
	}
	fmt.Println()
	fmt.Printf("  SidecarScope 적용 후: %d개\n", len(filteredSvcs))
	for _, svc := range filteredSvcs {
		fmt.Printf("    - %s\n", svc.Hostname)
	}

	// ==========================================================================
	// 6. 불변 스냅샷 - 설정 변경 시 새 PushContext 생성
	// ==========================================================================
	printSeparator("6. 불변 스냅샷 - 설정 변경 시 새 PushContext 생성")

	fmt.Println("  설정 변경 시나리오: reviews v2 가중치 80%로 변경")
	fmt.Println()

	// 이전 PushContext (불변, 계속 사용 가능)
	oldPC := env.PushContext()
	fmt.Printf("  이전 PushContext 버전: %s\n", oldPC.PushVersion)

	// 변경된 VirtualService
	updatedVS := make([]*VirtualService, len(virtualServices))
	copy(updatedVS, virtualServices)
	updatedVS[0] = &VirtualService{
		Name:      "reviews-route",
		Namespace: "default",
		Hosts:     []string{"reviews.default.svc.cluster.local"},
		Gateways:  []string{"mesh"},
		ExportTo:  map[Visibility]bool{VisibilityPublic: true},
		HTTPRoutes: []HTTPRoute{
			{
				Match: []HTTPMatchRequest{{URI: "/reviews"}},
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v1", Weight: 20},
					{Host: "reviews.default.svc.cluster.local", Subset: "v2", Weight: 80}, // 변경!
				},
			},
		},
	}

	// 새 PushContext 생성
	newPC := NewPushContext()
	newPC.InitContext(services, updatedVS, destinationRules, sidecars, "v2-2024-01-01/2")
	env.SetPushContext(newPC)

	fmt.Printf("  새 PushContext 버전: %s\n", newPC.PushVersion)
	fmt.Printf("  현재 활성 PushContext 버전: %s\n", env.PushContext().PushVersion)
	fmt.Println()

	// 이전 vs 새 라우팅 비교
	fmt.Println("  라우팅 비교 (reviews.default.svc.cluster.local):")
	fmt.Println()
	oldVSS := oldPC.VirtualServicesForHost("reviews.default.svc.cluster.local", "mesh", "default")
	newVSS := newPC.VirtualServicesForHost("reviews.default.svc.cluster.local", "mesh", "default")

	fmt.Println("    이전 (oldPC):")
	for _, vs := range oldVSS {
		for _, route := range vs.HTTPRoutes {
			for _, dest := range route.Route {
				fmt.Printf("      %s (subset=%s): %d%%\n", dest.Host, dest.Subset, dest.Weight)
			}
		}
	}
	fmt.Println()
	fmt.Println("    현재 (newPC):")
	for _, vs := range newVSS {
		for _, route := range vs.HTTPRoutes {
			for _, dest := range route.Route {
				fmt.Printf("      %s (subset=%s): %d%%\n", dest.Host, dest.Subset, dest.Weight)
			}
		}
	}

	// ==========================================================================
	// 7. 동시 읽기 안전성 데모
	// ==========================================================================
	printSeparator("7. 동시 읽기 안전성 (불변 스냅샷)")

	fmt.Println("  10개의 고루틴이 동시에 PushContext를 읽는 시나리오:")
	fmt.Println()

	currentPC := env.PushContext()
	var wg sync.WaitGroup
	results := make([]string, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			svcs := currentPC.ServicesForNamespace("default")
			svcNames := make([]string, len(svcs))
			for j, s := range svcs {
				svcNames[j] = s.Hostname
			}
			sort.Strings(svcNames)
			results[id] = fmt.Sprintf("고루틴 %02d: %d개 서비스 조회 완료", id, len(svcs))
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		fmt.Printf("    %s\n", r)
	}
	fmt.Println()
	fmt.Println("  -> 모든 고루틴이 동일한 불변 스냅샷을 안전하게 읽기 완료")

	// ==========================================================================
	// 8. 아키텍처 다이어그램
	// ==========================================================================
	printSeparator("8. PushContext 아키텍처 요약")

	fmt.Println("  설정 변경 → PushContext 생성 흐름:")
	fmt.Println()
	fmt.Println("  ConfigUpdate()")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  pushChannel (디바운스)")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  initPushContext()")
	fmt.Println("       │")
	fmt.Println("       ├── NewPushContext()")
	fmt.Println("       │")
	fmt.Println("       ├── InitContext()")
	fmt.Println("       │       ├── initServiceIndex()     ─── serviceIndex")
	fmt.Println("       │       │     ├── public           [exportTo=*]")
	fmt.Println("       │       │     ├── privateByNS      [exportTo=.]")
	fmt.Println("       │       │     └── exportedToNS     [exportTo=ns]")
	fmt.Println("       │       │")
	fmt.Println("       │       ├── initVSIndex()          ─── virtualServiceIndex")
	fmt.Println("       │       │     ├── publicByGateway")
	fmt.Println("       │       │     └── privateByNS+GW")
	fmt.Println("       │       │")
	fmt.Println("       │       └── initDRIndex()          ─── destinationRuleIndex")
	fmt.Println("       │             ├── namespaceLocal")
	fmt.Println("       │             └── exportedByNS")
	fmt.Println("       │")
	fmt.Println("       ├── SetPushContext()  (원자적 교체)")
	fmt.Println("       │")
	fmt.Println("       └── AdsPushAll()      (프록시에 push)")
	fmt.Println("             │")
	fmt.Println("             ▼")
	fmt.Println("       각 프록시별 SidecarScope 적용 → 필요한 설정만 전달")

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
