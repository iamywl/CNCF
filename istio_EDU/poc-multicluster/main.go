// poc-multicluster: Istio 멀티클러스터 서비스 디스커버리 시뮬레이션
//
// 이 PoC는 Istio의 멀티클러스터 서비스 디스커버리 핵심 메커니즘을 시뮬레이션한다:
// 1. 다중 클러스터 레지스트리 (cluster-1, cluster-2, cluster-3)
// 2. Aggregate Controller가 모든 클러스터의 서비스를 병합
// 3. 크로스 클러스터 엔드포인트 디스커버리
// 4. Locality 기반 로드 밸런싱 (Region/Zone/Subzone 선호도)
// 5. 서비스 병합 (동일 hostname → 엔드포인트 통합, ClusterVIPs 병합)
// 6. Network Gateway를 통한 크로스 네트워크 트래픽
//
// Istio 소스 참조:
// - pilot/pkg/serviceregistry/aggregate/controller.go: Aggregate Controller
// - pilot/pkg/serviceregistry/kube/controller/multicluster.go: Multicluster Controller
// - pilot/pkg/model/service.go: Service, ClusterVIPs, AddressMap

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. 데이터 모델 — Istio model/service.go 기반
// ============================================================================

// ClusterID는 클러스터의 고유 식별자이다.
// 실제 소스: pkg/cluster/id.go
type ClusterID string

// NetworkID는 네트워크의 고유 식별자이다.
// 실제 소스: pkg/network/id.go
type NetworkID string

// Locality는 워크로드의 지리적 위치를 나타낸다.
// 실제 소스: pkg/workloadapi/workload.proto의 Locality message
type Locality struct {
	Region  string
	Zone    string
	Subzone string
}

func (l Locality) String() string {
	parts := []string{l.Region}
	if l.Zone != "" {
		parts = append(parts, l.Zone)
	}
	if l.Subzone != "" {
		parts = append(parts, l.Subzone)
	}
	return strings.Join(parts, "/")
}

// HostName은 서비스의 FQDN이다.
// 실제 소스: pkg/config/host/name.go
type HostName string

// Port는 서비스 포트 정보이다.
// 실제 소스: pilot/pkg/model/service.go의 Port struct
type Port struct {
	Name     string
	Port     int
	Protocol string
}

func (p Port) String() string {
	return fmt.Sprintf("%s/%d/%s", p.Name, p.Port, p.Protocol)
}

// AddressMap은 클러스터별 서비스 VIP 매핑이다.
// 실제 소스: pilot/pkg/model/addressmap.go
// 핵심: 동일 서비스가 여러 클러스터에 존재할 때 각 클러스터의 ClusterIP를 보관한다.
type AddressMap struct {
	mu        sync.RWMutex
	Addresses map[ClusterID][]string
}

func NewAddressMap() *AddressMap {
	return &AddressMap{
		Addresses: make(map[ClusterID][]string),
	}
}

func (m *AddressMap) SetAddressesFor(cluster ClusterID, addrs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Addresses[cluster] = addrs
}

func (m *AddressMap) GetAddressesFor(cluster ClusterID) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Addresses[cluster]
}

func (m *AddressMap) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Addresses)
}

// DeepCopy는 AddressMap의 깊은 복사본을 만든다.
// 실제 소스: pilot/pkg/model/addressmap.go의 DeepCopy
func (m *AddressMap) DeepCopy() *AddressMap {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := NewAddressMap()
	for k, v := range m.Addresses {
		cp := make([]string, len(v))
		copy(cp, v)
		out.Addresses[k] = cp
	}
	return out
}

// Service는 Istio의 서비스 모델이다.
// 실제 소스: pilot/pkg/model/service.go의 Service struct
// 핵심 필드:
// - Hostname: 서비스의 FQDN (예: reviews.default.svc.cluster.local)
// - ClusterVIPs: 클러스터별 서비스 IP (ClusterID → []IP)
// - ServiceAccounts: 서비스를 실행하는 서비스 어카운트 목록
type Service struct {
	Hostname        HostName
	Namespace       string
	Ports           []Port
	ClusterVIPs     *AddressMap
	DefaultAddress  string
	ServiceAccounts []string
	MeshExternal    bool
}

// ShallowCopy는 서비스의 얕은 복사본을 만든다.
// Aggregate Controller의 Services() 메서드에서 병합 전에 호출된다.
// 실제 소스: pilot/pkg/model/service.go의 ShallowCopy
func (s *Service) ShallowCopy() *Service {
	out := *s
	out.ClusterVIPs = s.ClusterVIPs.DeepCopy()
	out.ServiceAccounts = make([]string, len(s.ServiceAccounts))
	copy(out.ServiceAccounts, s.ServiceAccounts)
	return &out
}

// Endpoint는 서비스의 개별 엔드포인트(Pod)를 나타낸다.
// 실제 소스: pilot/pkg/model/service.go의 ServiceTarget
type Endpoint struct {
	Address         string
	Port            int
	ServicePort     int
	ClusterID       ClusterID
	Network         NetworkID
	Locality        Locality
	ServiceAccount  string
	Weight          int
	HealthStatus    string // HEALTHY, UNHEALTHY
}

func (e *Endpoint) String() string {
	return fmt.Sprintf("%s:%d (cluster=%s, network=%s, locality=%s, health=%s, weight=%d)",
		e.Address, e.Port, e.ClusterID, e.Network, e.Locality, e.HealthStatus, e.Weight)
}

// NetworkGateway는 크로스 네트워크 트래픽을 위한 게이트웨이이다.
// 실제 소스: pilot/pkg/model/network.go의 NetworkGateway struct
// 서로 다른 네트워크의 클러스터 간 트래픽은 이 게이트웨이를 통해 라우팅된다.
type NetworkGateway struct {
	Network        NetworkID
	Cluster        ClusterID
	Addr           string
	Port           int
	HBONEPort      int
	ServiceAccount string
}

func (gw NetworkGateway) String() string {
	return fmt.Sprintf("%s:%d (network=%s, cluster=%s)", gw.Addr, gw.Port, gw.Network, gw.Cluster)
}

// ============================================================================
// 2. ServiceRegistry — 개별 클러스터 레지스트리
// ============================================================================

// ProviderID는 레지스트리 제공자 유형이다.
// 실제 소스: pilot/pkg/serviceregistry/provider/id.go
type ProviderID string

const (
	Kubernetes ProviderID = "Kubernetes"
	External   ProviderID = "External" // ServiceEntry 등
)

// ServiceRegistry는 개별 클러스터의 서비스 레지스트리 인터페이스이다.
// 실제 소스: pilot/pkg/serviceregistry/instance.go의 Instance 인터페이스
type ServiceRegistry interface {
	Provider() ProviderID
	Cluster() ClusterID
	Services() []*Service
	GetService(hostname HostName) *Service
	GetEndpoints(hostname HostName) []*Endpoint
	NetworkGateways() []NetworkGateway
	HasSynced() bool
	Run(stop <-chan struct{})
}

// KubeRegistry는 Kubernetes 클러스터의 서비스 레지스트리 구현이다.
// 실제 소스: pilot/pkg/serviceregistry/kube/controller/controller.go
type KubeRegistry struct {
	clusterID  ClusterID
	network    NetworkID
	services   map[HostName]*Service
	endpoints  map[HostName][]*Endpoint
	gateways   []NetworkGateway
	synced     bool
	mu         sync.RWMutex
	handlers   []ServiceHandler
}

type ServiceHandler func(prev, curr *Service, event EventType)
type EventType int

const (
	EventAdd EventType = iota
	EventUpdate
	EventDelete
)

func (e EventType) String() string {
	switch e {
	case EventAdd:
		return "ADD"
	case EventUpdate:
		return "UPDATE"
	case EventDelete:
		return "DELETE"
	default:
		return "UNKNOWN"
	}
}

func NewKubeRegistry(clusterID ClusterID, network NetworkID) *KubeRegistry {
	return &KubeRegistry{
		clusterID: clusterID,
		network:   network,
		services:  make(map[HostName]*Service),
		endpoints: make(map[HostName][]*Endpoint),
	}
}

func (r *KubeRegistry) Provider() ProviderID { return Kubernetes }
func (r *KubeRegistry) Cluster() ClusterID   { return r.clusterID }
func (r *KubeRegistry) HasSynced() bool       { return r.synced }

func (r *KubeRegistry) Run(stop <-chan struct{}) {
	r.synced = true
	<-stop
}

func (r *KubeRegistry) Services() []*Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Service, 0, len(r.services))
	for _, s := range r.services {
		out = append(out, s)
	}
	return out
}

func (r *KubeRegistry) GetService(hostname HostName) *Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.services[hostname]
}

func (r *KubeRegistry) GetEndpoints(hostname HostName) []*Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.endpoints[hostname]
}

func (r *KubeRegistry) NetworkGateways() []NetworkGateway {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gateways
}

// AddService는 레지스트리에 서비스를 추가한다.
func (r *KubeRegistry) AddService(svc *Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[svc.Hostname] = svc
	for _, h := range r.handlers {
		h(nil, svc, EventAdd)
	}
}

// AddEndpoints는 서비스에 엔드포인트를 추가한다.
func (r *KubeRegistry) AddEndpoints(hostname HostName, eps []*Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints[hostname] = append(r.endpoints[hostname], eps...)
}

// SetNetworkGateway는 네트워크 게이트웨이를 설정한다.
func (r *KubeRegistry) SetNetworkGateway(gw NetworkGateway) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gateways = append(r.gateways, gw)
}

// AppendServiceHandler는 서비스 변경 이벤트 핸들러를 등록한다.
func (r *KubeRegistry) AppendServiceHandler(f ServiceHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers = append(r.handlers, f)
}

// ============================================================================
// 3. Aggregate Controller — 멀티클러스터 서비스 통합
// ============================================================================

// AggregateController는 여러 클러스터 레지스트리를 하나로 통합한다.
// 실제 소스: pilot/pkg/serviceregistry/aggregate/controller.go
//
// 핵심 동작:
// 1. Services() — 모든 레지스트리에서 서비스 조회 후 hostname 기준으로 병합
// 2. mergeService() — 동일 hostname의 서비스가 여러 클러스터에 존재하면 ClusterVIPs 병합
// 3. NetworkGateways() — 모든 레지스트리에서 네트워크 게이트웨이 수집
type AggregateController struct {
	mu              sync.RWMutex
	registries      []ServiceRegistry
	configClusterID ClusterID
	handlers        []ServiceHandler
}

func NewAggregateController(configClusterID ClusterID) *AggregateController {
	return &AggregateController{
		configClusterID: configClusterID,
	}
}

// AddRegistry는 레지스트리를 추가한다.
// 실제 소스: aggregate/controller.go의 AddRegistry
// Kubernetes 레지스트리는 앞에, 나머지(ServiceEntry 등)는 뒤에 배치한다.
func (c *AggregateController) AddRegistry(registry ServiceRegistry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Kubernetes 레지스트리를 앞에 배치하는 로직
	// 실제 소스의 addRegistry()와 동일한 패턴
	if registry.Provider() == Kubernetes {
		inserted := false
		for i, r := range c.registries {
			if r.Provider() != Kubernetes {
				// 첫 번째 non-Kubernetes 레지스트리 위치에 삽입
				newRegs := make([]ServiceRegistry, 0, len(c.registries)+1)
				newRegs = append(newRegs, c.registries[:i]...)
				newRegs = append(newRegs, registry)
				newRegs = append(newRegs, c.registries[i:]...)
				c.registries = newRegs
				inserted = true
				break
			}
		}
		if !inserted {
			c.registries = append(c.registries, registry)
		}
	} else {
		c.registries = append(c.registries, registry)
	}
}

// DeleteRegistry는 클러스터 ID로 레지스트리를 삭제한다.
// 실제 소스: aggregate/controller.go의 DeleteRegistry
func (c *AggregateController) DeleteRegistry(clusterID ClusterID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, r := range c.registries {
		if r.Cluster() == clusterID {
			c.registries = append(c.registries[:i], c.registries[i+1:]...)
			fmt.Printf("  [Aggregate] 레지스트리 삭제: cluster=%s\n", clusterID)
			return
		}
	}
}

// GetRegistries는 모든 레지스트리의 복사본을 반환한다.
// 실제 소스: aggregate/controller.go의 GetRegistries
func (c *AggregateController) GetRegistries() []ServiceRegistry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ServiceRegistry, len(c.registries))
	copy(out, c.registries)
	return out
}

// Services는 모든 레지스트리에서 서비스를 조회하고 hostname 기준으로 병합한다.
// 이것이 멀티클러스터 서비스 디스커버리의 핵심 메서드이다.
//
// 실제 소스: aggregate/controller.go의 Services()
// - smap (hostname → index)으로 중복 서비스 탐지
// - 첫 번째 발견 시 결과에 추가
// - 두 번째 이후 발견 시 mergeService()로 ClusterVIPs 병합
func (c *AggregateController) Services() []*Service {
	smap := make(map[HostName]int)   // hostname → 서비스 인덱스
	index := 0
	services := make([]*Service, 0)

	for _, r := range c.GetRegistries() {
		svcs := r.Services()
		if r.Provider() != Kubernetes {
			// non-Kubernetes 레지스트리는 병합 없이 추가
			index += len(svcs)
			services = append(services, svcs...)
		} else {
			for _, s := range svcs {
				previous, ok := smap[s.Hostname]
				if !ok {
					// 첫 번째 발견: 결과에 추가
					smap[s.Hostname] = index
					index++
					services = append(services, s)
				} else {
					// 두 번째 이후 발견: ClusterVIPs 병합
					// 병합 전에 DeepCopy (원본 보호)
					if services[previous].ClusterVIPs.Len() < 2 {
						services[previous] = services[previous].ShallowCopy()
					}
					mergeService(services[previous], s, r)
				}
			}
		}
	}
	return services
}

// mergeService는 동일 hostname의 서비스를 병합한다.
// 실제 소스: aggregate/controller.go의 mergeService()
// - ClusterVIPs 병합: 각 클러스터의 ClusterIP 보존
// - ServiceAccounts 병합: 중복 제거
func mergeService(dst, src *Service, srcRegistry ServiceRegistry) {
	clusterID := srcRegistry.Cluster()
	if len(dst.ClusterVIPs.GetAddressesFor(clusterID)) == 0 {
		newAddresses := src.ClusterVIPs.GetAddressesFor(clusterID)
		dst.ClusterVIPs.SetAddressesFor(clusterID, newAddresses)
	}
	// 서비스 어카운트 병합 (중복 제거)
	if len(src.ServiceAccounts) > 0 {
		seen := make(map[string]bool)
		for _, sa := range dst.ServiceAccounts {
			seen[sa] = true
		}
		for _, sa := range src.ServiceAccounts {
			if !seen[sa] {
				dst.ServiceAccounts = append(dst.ServiceAccounts, sa)
				seen[sa] = true
			}
		}
	}
}

// GetService는 hostname으로 서비스를 조회하고 여러 클러스터의 정보를 병합한다.
// 실제 소스: aggregate/controller.go의 GetService()
func (c *AggregateController) GetService(hostname HostName) *Service {
	var out *Service
	for _, r := range c.GetRegistries() {
		service := r.GetService(hostname)
		if service == nil {
			continue
		}
		if r.Provider() != Kubernetes {
			return service
		}
		if out == nil {
			out = service.ShallowCopy()
		} else {
			mergeService(out, service, r)
		}
	}
	return out
}

// GetAllEndpoints는 모든 클러스터에서 특정 서비스의 엔드포인트를 수집한다.
func (c *AggregateController) GetAllEndpoints(hostname HostName) []*Endpoint {
	var out []*Endpoint
	for _, r := range c.GetRegistries() {
		eps := r.GetEndpoints(hostname)
		out = append(out, eps...)
	}
	return out
}

// NetworkGateways는 모든 레지스트리에서 네트워크 게이트웨이를 수집한다.
// 실제 소스: aggregate/controller.go의 NetworkGateways()
func (c *AggregateController) NetworkGateways() []NetworkGateway {
	var gws []NetworkGateway
	for _, r := range c.GetRegistries() {
		gws = append(gws, r.NetworkGateways()...)
	}
	return gws
}

// HasSynced는 모든 레지스트리가 동기화되었는지 확인한다.
// 실제 소스: aggregate/controller.go의 HasSynced()
func (c *AggregateController) HasSynced() bool {
	for _, r := range c.GetRegistries() {
		if !r.HasSynced() {
			return false
		}
	}
	return true
}

// ============================================================================
// 4. Locality-Aware 로드 밸런서
// ============================================================================

// LocalityLoadBalancer는 Istio의 locality 기반 로드 밸런싱을 시뮬레이션한다.
// 실제 Istio에서는 Envoy의 locality weighted load balancing을 사용한다.
//
// LoadBalancing.Scope 우선순위 (workload.proto):
// - NETWORK > REGION > ZONE > SUBZONE > NODE > CLUSTER
//
// Mode:
// - FAILOVER: 선호도 순으로 시도, 매칭 없으면 다음 단계로 폴백
// - STRICT: 선호도 매칭 엔드포인트만 사용 (매칭 없으면 드롭)
type LocalityLoadBalancer struct {
	proxyLocality  Locality
	proxyCluster   ClusterID
	proxyNetwork   NetworkID
	networkGateways map[NetworkID][]NetworkGateway
}

func NewLocalityLoadBalancer(locality Locality, cluster ClusterID, network NetworkID) *LocalityLoadBalancer {
	return &LocalityLoadBalancer{
		proxyLocality:   locality,
		proxyCluster:    cluster,
		proxyNetwork:    network,
		networkGateways: make(map[NetworkID][]NetworkGateway),
	}
}

func (lb *LocalityLoadBalancer) SetNetworkGateways(gateways []NetworkGateway) {
	for _, gw := range gateways {
		lb.networkGateways[gw.Network] = append(lb.networkGateways[gw.Network], gw)
	}
}

// LocalityPriority는 엔드포인트의 locality 매칭 우선순위를 계산한다.
// 값이 낮을수록 높은 우선순위 (더 가까움)
func (lb *LocalityLoadBalancer) LocalityPriority(ep *Endpoint) int {
	// 같은 네트워크 + 같은 리전 + 같은 존 + 같은 서브존
	if ep.Network == lb.proxyNetwork &&
		ep.Locality.Region == lb.proxyLocality.Region &&
		ep.Locality.Zone == lb.proxyLocality.Zone &&
		ep.Locality.Subzone == lb.proxyLocality.Subzone {
		return 0 // 최우선: 동일 서브존
	}
	// 같은 네트워크 + 같은 리전 + 같은 존
	if ep.Network == lb.proxyNetwork &&
		ep.Locality.Region == lb.proxyLocality.Region &&
		ep.Locality.Zone == lb.proxyLocality.Zone {
		return 1 // 동일 존
	}
	// 같은 네트워크 + 같은 리전
	if ep.Network == lb.proxyNetwork &&
		ep.Locality.Region == lb.proxyLocality.Region {
		return 2 // 동일 리전
	}
	// 같은 네트워크, 다른 리전
	if ep.Network == lb.proxyNetwork {
		return 3 // 동일 네트워크
	}
	// 다른 네트워크 (크로스 네트워크 게이트웨이 필요)
	return 4 // 원격 네트워크
}

// SelectEndpoints는 locality 선호도에 따라 엔드포인트를 선택한다.
// FAILOVER 모드를 시뮬레이션한다.
func (lb *LocalityLoadBalancer) SelectEndpoints(endpoints []*Endpoint) []*Endpoint {
	if len(endpoints) == 0 {
		return nil
	}

	// 우선순위별로 엔드포인트 그룹화
	groups := make(map[int][]*Endpoint)
	for _, ep := range endpoints {
		if ep.HealthStatus != "HEALTHY" {
			continue
		}
		prio := lb.LocalityPriority(ep)
		groups[prio] = append(groups[prio], ep)
	}

	// FAILOVER 모드: 가장 높은 우선순위(낮은 값) 그룹부터 반환
	// 해당 그룹에 엔드포인트가 있으면 반환, 없으면 다음 그룹으로 폴백
	for prio := 0; prio <= 4; prio++ {
		if eps, ok := groups[prio]; ok && len(eps) > 0 {
			return eps
		}
	}

	return nil
}

// ResolveEndpointAddress는 크로스 네트워크 엔드포인트의 주소를 게이트웨이로 해석한다.
// 실제 Istio에서 다른 네트워크의 엔드포인트에 접근할 때,
// 직접 Pod IP가 아닌 네트워크 게이트웨이를 통해 라우팅한다.
func (lb *LocalityLoadBalancer) ResolveEndpointAddress(ep *Endpoint) (string, int) {
	if ep.Network == lb.proxyNetwork {
		// 같은 네트워크: 직접 접근
		return ep.Address, ep.Port
	}
	// 다른 네트워크: 게이트웨이를 통해 접근
	gateways := lb.networkGateways[ep.Network]
	if len(gateways) > 0 {
		// 라운드로빈으로 게이트웨이 선택
		gw := gateways[rand.Intn(len(gateways))]
		return gw.Addr, gw.Port
	}
	// 게이트웨이 없으면 직접 접근 시도 (실패할 수 있음)
	return ep.Address, ep.Port
}

// ============================================================================
// 5. 시뮬레이션 헬퍼
// ============================================================================

func printSeparator(title string) {
	fmt.Println()
	fmt.Println("============================================================")
	fmt.Printf("  %s\n", title)
	fmt.Println("============================================================")
	fmt.Println()
}

func printServiceTable(services []*Service) {
	fmt.Println("  ┌──────────────────────────────────────────┬────────────┬───────────────────────────────────┬────────────────────────┐")
	fmt.Println("  │ Hostname                                 │ Namespace  │ ClusterVIPs                       │ ServiceAccounts        │")
	fmt.Println("  ├──────────────────────────────────────────┼────────────┼───────────────────────────────────┼────────────────────────┤")
	for _, svc := range services {
		vips := formatClusterVIPs(svc.ClusterVIPs)
		sas := strings.Join(svc.ServiceAccounts, ", ")
		fmt.Printf("  │ %-40s │ %-10s │ %-33s │ %-22s │\n",
			svc.Hostname, svc.Namespace, vips, sas)
	}
	fmt.Println("  └──────────────────────────────────────────┴────────────┴───────────────────────────────────┴────────────────────────┘")
}

func formatClusterVIPs(am *AddressMap) string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	parts := make([]string, 0, len(am.Addresses))
	// 정렬된 순서로 출력
	keys := make([]string, 0, len(am.Addresses))
	for k := range am.Addresses {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		addrs := am.Addresses[ClusterID(k)]
		parts = append(parts, fmt.Sprintf("%s:%s", k, strings.Join(addrs, ",")))
	}
	return strings.Join(parts, " | ")
}

func printEndpointTable(endpoints []*Endpoint) {
	fmt.Println("  ┌────────────────┬───────┬────────────┬────────────┬──────────────────────┬──────────┐")
	fmt.Println("  │ Address        │ Port  │ Cluster    │ Network    │ Locality             │ Health   │")
	fmt.Println("  ├────────────────┼───────┼────────────┼────────────┼──────────────────────┼──────────┤")
	for _, ep := range endpoints {
		fmt.Printf("  │ %-14s │ %-5d │ %-10s │ %-10s │ %-20s │ %-8s │\n",
			ep.Address, ep.Port, ep.ClusterID, ep.Network, ep.Locality, ep.HealthStatus)
	}
	fmt.Println("  └────────────────┴───────┴────────────┴────────────┴──────────────────────┴──────────┘")
}

// ============================================================================
// 6. 메인 시뮬레이션
// ============================================================================

func main() {
	printSeparator("Istio 멀티클러스터 서비스 디스커버리 시뮬레이션")

	// --- 1단계: 클러스터 레지스트리 생성 ---
	fmt.Println("[1단계] 멀티클러스터 환경 구성")
	fmt.Println("  실제 Istio: pilot/pkg/serviceregistry/kube/controller/multicluster.go")
	fmt.Println("  NewMulticluster()가 각 원격 클러스터 발견 시 KubeRegistry를 생성한다.")
	fmt.Println()
	fmt.Println("  클러스터 구성:")
	fmt.Println("    cluster-1: Primary (config cluster), network=net-west, region=us-west")
	fmt.Println("    cluster-2: Remote, network=net-west, region=us-west (동일 네트워크)")
	fmt.Println("    cluster-3: Remote, network=net-east, region=us-east (다른 네트워크)")
	fmt.Println()

	// 3개의 클러스터 레지스트리 생성
	reg1 := NewKubeRegistry("cluster-1", "net-west")
	reg2 := NewKubeRegistry("cluster-2", "net-west")
	reg3 := NewKubeRegistry("cluster-3", "net-east")

	// 동기화 완료 시뮬레이션
	reg1.synced = true
	reg2.synced = true
	reg3.synced = true

	// --- 2단계: 서비스 등록 ---
	fmt.Println("[2단계] 각 클러스터에 서비스 등록")
	fmt.Println("  동일 hostname의 서비스가 여러 클러스터에 존재할 수 있다.")
	fmt.Println("  각 클러스터에서 서비스는 서로 다른 ClusterIP를 가진다.")
	fmt.Println()

	// reviews 서비스 — 3개 클러스터 모두에 존재
	reviewsHostname := HostName("reviews.default.svc.cluster.local")
	reviewsPorts := []Port{{Name: "http", Port: 9080, Protocol: "HTTP"}}

	reviewsSvc1 := &Service{
		Hostname:        reviewsHostname,
		Namespace:       "default",
		Ports:           reviewsPorts,
		ClusterVIPs:     NewAddressMap(),
		DefaultAddress:  "10.0.1.100",
		ServiceAccounts: []string{"reviews-sa"},
	}
	reviewsSvc1.ClusterVIPs.SetAddressesFor("cluster-1", []string{"10.0.1.100"})

	reviewsSvc2 := &Service{
		Hostname:        reviewsHostname,
		Namespace:       "default",
		Ports:           reviewsPorts,
		ClusterVIPs:     NewAddressMap(),
		DefaultAddress:  "10.0.2.100",
		ServiceAccounts: []string{"reviews-sa"},
	}
	reviewsSvc2.ClusterVIPs.SetAddressesFor("cluster-2", []string{"10.0.2.100"})

	reviewsSvc3 := &Service{
		Hostname:        reviewsHostname,
		Namespace:       "default",
		Ports:           reviewsPorts,
		ClusterVIPs:     NewAddressMap(),
		DefaultAddress:  "10.0.3.100",
		ServiceAccounts: []string{"reviews-sa-east"}, // 다른 trust domain 가능
	}
	reviewsSvc3.ClusterVIPs.SetAddressesFor("cluster-3", []string{"10.0.3.100"})

	// productpage 서비스 — cluster-1에만 존재
	productpageHostname := HostName("productpage.default.svc.cluster.local")
	productpageSvc := &Service{
		Hostname:        productpageHostname,
		Namespace:       "default",
		Ports:           []Port{{Name: "http", Port: 9080, Protocol: "HTTP"}},
		ClusterVIPs:     NewAddressMap(),
		DefaultAddress:  "10.0.1.101",
		ServiceAccounts: []string{"productpage-sa"},
	}
	productpageSvc.ClusterVIPs.SetAddressesFor("cluster-1", []string{"10.0.1.101"})

	// ratings 서비스 — cluster-1, cluster-3에 존재
	ratingsHostname := HostName("ratings.default.svc.cluster.local")
	ratingsSvc1 := &Service{
		Hostname:        ratingsHostname,
		Namespace:       "default",
		Ports:           []Port{{Name: "http", Port: 9080, Protocol: "HTTP"}},
		ClusterVIPs:     NewAddressMap(),
		DefaultAddress:  "10.0.1.102",
		ServiceAccounts: []string{"ratings-sa"},
	}
	ratingsSvc1.ClusterVIPs.SetAddressesFor("cluster-1", []string{"10.0.1.102"})

	ratingsSvc3 := &Service{
		Hostname:        ratingsHostname,
		Namespace:       "default",
		Ports:           []Port{{Name: "http", Port: 9080, Protocol: "HTTP"}},
		ClusterVIPs:     NewAddressMap(),
		DefaultAddress:  "10.0.3.102",
		ServiceAccounts: []string{"ratings-sa"},
	}
	ratingsSvc3.ClusterVIPs.SetAddressesFor("cluster-3", []string{"10.0.3.102"})

	// 레지스트리에 서비스 추가
	reg1.AddService(reviewsSvc1)
	reg1.AddService(productpageSvc)
	reg1.AddService(ratingsSvc1)

	reg2.AddService(reviewsSvc2)

	reg3.AddService(reviewsSvc3)
	reg3.AddService(ratingsSvc3)

	fmt.Println("  cluster-1: reviews, productpage, ratings")
	fmt.Println("  cluster-2: reviews")
	fmt.Println("  cluster-3: reviews, ratings")
	fmt.Println()

	// --- 3단계: 엔드포인트 등록 ---
	fmt.Println("[3단계] 엔드포인트(Pod) 등록 및 locality 설정")
	fmt.Println()

	// reviews 엔드포인트
	reg1.AddEndpoints(reviewsHostname, []*Endpoint{
		{Address: "10.0.1.10", Port: 9080, ServicePort: 9080, ClusterID: "cluster-1", Network: "net-west",
			Locality: Locality{Region: "us-west", Zone: "us-west-1a", Subzone: "rack-1"}, ServiceAccount: "reviews-sa", Weight: 100, HealthStatus: "HEALTHY"},
		{Address: "10.0.1.11", Port: 9080, ServicePort: 9080, ClusterID: "cluster-1", Network: "net-west",
			Locality: Locality{Region: "us-west", Zone: "us-west-1a", Subzone: "rack-2"}, ServiceAccount: "reviews-sa", Weight: 100, HealthStatus: "HEALTHY"},
	})

	reg2.AddEndpoints(reviewsHostname, []*Endpoint{
		{Address: "10.0.2.10", Port: 9080, ServicePort: 9080, ClusterID: "cluster-2", Network: "net-west",
			Locality: Locality{Region: "us-west", Zone: "us-west-1b"}, ServiceAccount: "reviews-sa", Weight: 100, HealthStatus: "HEALTHY"},
		{Address: "10.0.2.11", Port: 9080, ServicePort: 9080, ClusterID: "cluster-2", Network: "net-west",
			Locality: Locality{Region: "us-west", Zone: "us-west-1b"}, ServiceAccount: "reviews-sa", Weight: 100, HealthStatus: "UNHEALTHY"},
	})

	reg3.AddEndpoints(reviewsHostname, []*Endpoint{
		{Address: "10.0.3.10", Port: 9080, ServicePort: 9080, ClusterID: "cluster-3", Network: "net-east",
			Locality: Locality{Region: "us-east", Zone: "us-east-1a"}, ServiceAccount: "reviews-sa-east", Weight: 100, HealthStatus: "HEALTHY"},
		{Address: "10.0.3.11", Port: 9080, ServicePort: 9080, ClusterID: "cluster-3", Network: "net-east",
			Locality: Locality{Region: "us-east", Zone: "us-east-1a"}, ServiceAccount: "reviews-sa-east", Weight: 100, HealthStatus: "HEALTHY"},
	})

	// ratings 엔드포인트
	reg1.AddEndpoints(ratingsHostname, []*Endpoint{
		{Address: "10.0.1.20", Port: 9080, ServicePort: 9080, ClusterID: "cluster-1", Network: "net-west",
			Locality: Locality{Region: "us-west", Zone: "us-west-1a"}, ServiceAccount: "ratings-sa", Weight: 100, HealthStatus: "HEALTHY"},
	})

	reg3.AddEndpoints(ratingsHostname, []*Endpoint{
		{Address: "10.0.3.20", Port: 9080, ServicePort: 9080, ClusterID: "cluster-3", Network: "net-east",
			Locality: Locality{Region: "us-east", Zone: "us-east-1a"}, ServiceAccount: "ratings-sa", Weight: 100, HealthStatus: "HEALTHY"},
	})

	// productpage 엔드포인트
	reg1.AddEndpoints(productpageHostname, []*Endpoint{
		{Address: "10.0.1.30", Port: 9080, ServicePort: 9080, ClusterID: "cluster-1", Network: "net-west",
			Locality: Locality{Region: "us-west", Zone: "us-west-1a"}, ServiceAccount: "productpage-sa", Weight: 100, HealthStatus: "HEALTHY"},
	})

	// --- 4단계: 네트워크 게이트웨이 설정 ---
	fmt.Println("[4단계] 네트워크 게이트웨이 설정")
	fmt.Println("  실제 Istio: pilot/pkg/model/network.go의 NetworkGateway")
	fmt.Println("  서로 다른 네트워크의 클러스터 간 트래픽은 게이트웨이를 경유한다.")
	fmt.Println()

	// net-west 게이트웨이 (cluster-1이 호스팅)
	gw1 := NetworkGateway{
		Network:        "net-west",
		Cluster:        "cluster-1",
		Addr:           "35.192.0.1",
		Port:           15443,
		HBONEPort:      15008,
		ServiceAccount: "istio-eastwestgateway",
	}
	reg1.SetNetworkGateway(gw1)

	// net-east 게이트웨이 (cluster-3이 호스팅)
	gw3 := NetworkGateway{
		Network:        "net-east",
		Cluster:        "cluster-3",
		Addr:           "35.194.0.1",
		Port:           15443,
		HBONEPort:      15008,
		ServiceAccount: "istio-eastwestgateway",
	}
	reg3.SetNetworkGateway(gw3)

	fmt.Printf("  net-west 게이트웨이: %s (cluster-1)\n", gw1.String())
	fmt.Printf("  net-east 게이트웨이: %s (cluster-3)\n", gw3.String())
	fmt.Println()

	// --- 5단계: Aggregate Controller 구성 ---
	printSeparator("Aggregate Controller 서비스 병합")

	fmt.Println("[5단계] Aggregate Controller로 멀티클러스터 서비스 통합")
	fmt.Println("  실제 소스: pilot/pkg/serviceregistry/aggregate/controller.go")
	fmt.Println("  핵심 메서드: Services() — hostname 기준 서비스 병합")
	fmt.Println("  핵심 함수: mergeService() — ClusterVIPs, ServiceAccounts 병합")
	fmt.Println()

	agg := NewAggregateController("cluster-1")
	agg.AddRegistry(reg1)
	agg.AddRegistry(reg2)
	agg.AddRegistry(reg3)

	fmt.Printf("  등록된 레지스트리: %d개\n", len(agg.GetRegistries()))
	for _, r := range agg.GetRegistries() {
		fmt.Printf("    - %s (provider=%s)\n", r.Cluster(), r.Provider())
	}
	fmt.Println()

	// 통합 서비스 목록
	fmt.Println("  [병합된 서비스 목록]")
	fmt.Println("  동일 hostname의 서비스가 여러 클러스터에 존재하면 ClusterVIPs가 병합된다:")
	fmt.Println()

	allServices := agg.Services()
	printServiceTable(allServices)
	fmt.Println()

	// 개별 서비스 조회
	fmt.Println("  [개별 서비스 조회: reviews.default.svc.cluster.local]")
	reviews := agg.GetService(reviewsHostname)
	if reviews != nil {
		fmt.Printf("    Hostname:  %s\n", reviews.Hostname)
		fmt.Printf("    ClusterVIPs:\n")
		reviews.ClusterVIPs.mu.RLock()
		for clusterID, addrs := range reviews.ClusterVIPs.Addresses {
			fmt.Printf("      %s → %s\n", clusterID, strings.Join(addrs, ", "))
		}
		reviews.ClusterVIPs.mu.RUnlock()
		fmt.Printf("    ServiceAccounts: %s\n", strings.Join(reviews.ServiceAccounts, ", "))
		fmt.Println()
		fmt.Println("    → 3개 클러스터의 ClusterIP가 하나의 서비스에 병합됨")
		fmt.Println("    → ServiceAccount도 병합됨 (reviews-sa + reviews-sa-east)")
	}
	fmt.Println()

	// --- 6단계: 크로스 클러스터 엔드포인트 디스커버리 ---
	printSeparator("크로스 클러스터 엔드포인트 디스커버리")

	fmt.Println("[6단계] 모든 클러스터에서 엔드포인트 수집")
	fmt.Println("  Aggregate Controller는 모든 레지스트리에서 엔드포인트를 수집한다.")
	fmt.Println("  프록시는 모든 클러스터의 엔드포인트에 대해 알게 된다.")
	fmt.Println()

	// reviews 서비스의 모든 엔드포인트
	fmt.Println("  [reviews 서비스 전체 엔드포인트]")
	allReviewsEps := agg.GetAllEndpoints(reviewsHostname)
	printEndpointTable(allReviewsEps)
	fmt.Println()

	// ratings 서비스의 모든 엔드포인트
	fmt.Println("  [ratings 서비스 전체 엔드포인트]")
	allRatingsEps := agg.GetAllEndpoints(ratingsHostname)
	printEndpointTable(allRatingsEps)
	fmt.Println()

	// --- 7단계: Locality 기반 로드 밸런싱 ---
	printSeparator("Locality 기반 로드 밸런싱")

	fmt.Println("[7단계] Locality-aware 엔드포인트 선택")
	fmt.Println("  실제 Istio: pkg/workloadapi/workload.proto의 LoadBalancing message")
	fmt.Println("  FAILOVER 모드: 가까운 locality부터 시도, 없으면 원격으로 폴백")
	fmt.Println()

	// 시나리오 1: cluster-1 us-west-1a에서 reviews 접근
	fmt.Println("  --- 시나리오 1: cluster-1/us-west/us-west-1a에서 reviews 접근 ---")
	lb1 := NewLocalityLoadBalancer(
		Locality{Region: "us-west", Zone: "us-west-1a", Subzone: "rack-1"},
		"cluster-1", "net-west",
	)
	lb1.SetNetworkGateways(agg.NetworkGateways())

	fmt.Println("  모든 엔드포인트의 우선순위:")
	for _, ep := range allReviewsEps {
		prio := lb1.LocalityPriority(ep)
		prioLabel := []string{"동일 서브존", "동일 존", "동일 리전", "동일 네트워크", "원격 네트워크"}[prio]
		fmt.Printf("    %s:%d (cluster=%s, locality=%s) → 우선순위 %d (%s)\n",
			ep.Address, ep.Port, ep.ClusterID, ep.Locality, prio, prioLabel)
	}
	fmt.Println()

	selected1 := lb1.SelectEndpoints(allReviewsEps)
	fmt.Println("  FAILOVER 선택 결과 (건강한 엔드포인트만):")
	for _, ep := range selected1 {
		addr, port := lb1.ResolveEndpointAddress(ep)
		fmt.Printf("    → %s:%d (원본: %s:%d, cluster=%s)\n",
			addr, port, ep.Address, ep.Port, ep.ClusterID)
	}
	fmt.Println()

	// 시나리오 2: cluster-3 us-east-1a에서 reviews 접근
	fmt.Println("  --- 시나리오 2: cluster-3/us-east/us-east-1a에서 reviews 접근 ---")
	lb3 := NewLocalityLoadBalancer(
		Locality{Region: "us-east", Zone: "us-east-1a"},
		"cluster-3", "net-east",
	)
	lb3.SetNetworkGateways(agg.NetworkGateways())

	fmt.Println("  모든 엔드포인트의 우선순위:")
	for _, ep := range allReviewsEps {
		prio := lb3.LocalityPriority(ep)
		prioLabel := []string{"동일 서브존", "동일 존", "동일 리전", "동일 네트워크", "원격 네트워크"}[prio]
		fmt.Printf("    %s:%d (cluster=%s, locality=%s) → 우선순위 %d (%s)\n",
			ep.Address, ep.Port, ep.ClusterID, ep.Locality, prio, prioLabel)
	}
	fmt.Println()

	selected3 := lb3.SelectEndpoints(allReviewsEps)
	fmt.Println("  FAILOVER 선택 결과 (건강한 엔드포인트만):")
	for _, ep := range selected3 {
		addr, port := lb3.ResolveEndpointAddress(ep)
		fmt.Printf("    → %s:%d (원본: %s:%d, cluster=%s)\n",
			addr, port, ep.Address, ep.Port, ep.ClusterID)
	}
	fmt.Println()

	// 시나리오 3: 로컬 엔드포인트 없을 때 폴백
	fmt.Println("  --- 시나리오 3: cluster-2에서 ratings 접근 (로컬 없음 → 폴백) ---")
	lb2 := NewLocalityLoadBalancer(
		Locality{Region: "us-west", Zone: "us-west-1b"},
		"cluster-2", "net-west",
	)
	lb2.SetNetworkGateways(agg.NetworkGateways())

	fmt.Println("  모든 ratings 엔드포인트의 우선순위:")
	for _, ep := range allRatingsEps {
		prio := lb2.LocalityPriority(ep)
		prioLabel := []string{"동일 서브존", "동일 존", "동일 리전", "동일 네트워크", "원격 네트워크"}[prio]
		fmt.Printf("    %s:%d (cluster=%s, locality=%s) → 우선순위 %d (%s)\n",
			ep.Address, ep.Port, ep.ClusterID, ep.Locality, prio, prioLabel)
	}
	fmt.Println()

	selectedRatings := lb2.SelectEndpoints(allRatingsEps)
	fmt.Println("  FAILOVER 선택 결과:")
	fmt.Println("  (cluster-2에는 ratings가 없으므로 같은 리전/네트워크의 cluster-1 엔드포인트로 폴백)")
	for _, ep := range selectedRatings {
		addr, port := lb2.ResolveEndpointAddress(ep)
		fmt.Printf("    → %s:%d (원본: %s:%d, cluster=%s)\n",
			addr, port, ep.Address, ep.Port, ep.ClusterID)
	}
	fmt.Println()

	// --- 8단계: 크로스 네트워크 게이트웨이 라우팅 ---
	printSeparator("크로스 네트워크 게이트웨이 라우팅")

	fmt.Println("[8단계] 네트워크 게이트웨이를 통한 크로스 네트워크 트래픽")
	fmt.Println("  실제 Istio: East-West Gateway (istio-eastwestgateway)")
	fmt.Println("  서로 다른 네트워크의 Pod은 직접 통신 불가 → 게이트웨이 경유")
	fmt.Println()

	fmt.Println("  [네트워크 게이트웨이 목록]")
	allGateways := agg.NetworkGateways()
	for _, gw := range allGateways {
		fmt.Printf("    network=%s → %s:%d (HBONE:%d, cluster=%s)\n",
			gw.Network, gw.Addr, gw.Port, gw.HBONEPort, gw.Cluster)
	}
	fmt.Println()

	fmt.Println("  [크로스 네트워크 엔드포인트 해석]")
	fmt.Println("  cluster-1(net-west)에서 cluster-3(net-east)의 reviews 엔드포인트에 접근:")
	for _, ep := range allReviewsEps {
		if ep.Network == "net-east" {
			addr, port := lb1.ResolveEndpointAddress(ep)
			fmt.Printf("    원본: %s:%d (cluster=%s, network=%s)\n", ep.Address, ep.Port, ep.ClusterID, ep.Network)
			fmt.Printf("    해석: %s:%d (East-West 게이트웨이 경유)\n", addr, port)
		}
	}
	fmt.Println()

	fmt.Println("  cluster-3(net-east)에서 cluster-1(net-west)의 reviews 엔드포인트에 접근:")
	for _, ep := range allReviewsEps {
		if ep.Network == "net-west" && ep.HealthStatus == "HEALTHY" {
			addr, port := lb3.ResolveEndpointAddress(ep)
			fmt.Printf("    원본: %s:%d (cluster=%s, network=%s)\n", ep.Address, ep.Port, ep.ClusterID, ep.Network)
			fmt.Printf("    해석: %s:%d (East-West 게이트웨이 경유)\n", addr, port)
		}
	}
	fmt.Println()

	// --- 9단계: 동적 클러스터 추가/삭제 ---
	printSeparator("동적 클러스터 관리")

	fmt.Println("[9단계] 런타임 클러스터 추가/삭제")
	fmt.Println("  실제 Istio: multicluster.go의 BuildMultiClusterComponent()")
	fmt.Println("  원격 클러스터의 kubeconfig secret이 추가/삭제되면 레지스트리를 동적 관리한다.")
	fmt.Println()

	// 새 클러스터 추가
	fmt.Println("  [cluster-4 추가 (network=net-east, region=us-east)]")
	reg4 := NewKubeRegistry("cluster-4", "net-east")
	reg4.synced = true

	reviewsSvc4 := &Service{
		Hostname:        reviewsHostname,
		Namespace:       "default",
		Ports:           reviewsPorts,
		ClusterVIPs:     NewAddressMap(),
		DefaultAddress:  "10.0.4.100",
		ServiceAccounts: []string{"reviews-sa-east"},
	}
	reviewsSvc4.ClusterVIPs.SetAddressesFor("cluster-4", []string{"10.0.4.100"})
	reg4.AddService(reviewsSvc4)
	reg4.AddEndpoints(reviewsHostname, []*Endpoint{
		{Address: "10.0.4.10", Port: 9080, ServicePort: 9080, ClusterID: "cluster-4", Network: "net-east",
			Locality: Locality{Region: "us-east", Zone: "us-east-1b"}, ServiceAccount: "reviews-sa-east", Weight: 100, HealthStatus: "HEALTHY"},
	})

	agg.AddRegistry(reg4)
	fmt.Printf("  레지스트리 수: %d\n", len(agg.GetRegistries()))
	fmt.Println()

	// 추가 후 서비스 확인
	fmt.Println("  [cluster-4 추가 후 reviews 서비스]")
	reviewsAfterAdd := agg.GetService(reviewsHostname)
	if reviewsAfterAdd != nil {
		fmt.Printf("    ClusterVIPs: %d개 클러스터\n", reviewsAfterAdd.ClusterVIPs.Len())
		reviewsAfterAdd.ClusterVIPs.mu.RLock()
		for clusterID, addrs := range reviewsAfterAdd.ClusterVIPs.Addresses {
			fmt.Printf("      %s → %s\n", clusterID, strings.Join(addrs, ", "))
		}
		reviewsAfterAdd.ClusterVIPs.mu.RUnlock()
	}
	fmt.Println()

	// 엔드포인트 확인
	fmt.Println("  [cluster-4 추가 후 reviews 엔드포인트]")
	epsAfterAdd := agg.GetAllEndpoints(reviewsHostname)
	printEndpointTable(epsAfterAdd)
	fmt.Println()

	// 클러스터 삭제
	fmt.Println("  [cluster-2 삭제 시뮬레이션]")
	agg.DeleteRegistry("cluster-2")
	fmt.Printf("  레지스트리 수: %d\n", len(agg.GetRegistries()))
	fmt.Println()

	fmt.Println("  [cluster-2 삭제 후 reviews 엔드포인트]")
	epsAfterDel := agg.GetAllEndpoints(reviewsHostname)
	printEndpointTable(epsAfterDel)
	fmt.Println()

	// --- 10단계: 아키텍처 요약 ---
	printSeparator("멀티클러스터 아키텍처 요약")

	fmt.Println("  ┌──────────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                         Istiod (Primary)                         │")
	fmt.Println("  │                                                                  │")
	fmt.Println("  │  ┌──────────────────────────────────────────────────────────┐    │")
	fmt.Println("  │  │              Aggregate Controller                        │    │")
	fmt.Println("  │  │                                                          │    │")
	fmt.Println("  │  │  ┌────────────┐  ┌────────────┐  ┌────────────┐         │    │")
	fmt.Println("  │  │  │ Registry-1 │  │ Registry-2 │  │ Registry-3 │  ...    │    │")
	fmt.Println("  │  │  │ cluster-1  │  │ cluster-2  │  │ cluster-3  │         │    │")
	fmt.Println("  │  │  └─────┬──────┘  └─────┬──────┘  └─────┬──────┘         │    │")
	fmt.Println("  │  │        │               │               │                │    │")
	fmt.Println("  │  │     Services()       Services()      Services()         │    │")
	fmt.Println("  │  │        │               │               │                │    │")
	fmt.Println("  │  │        └───────┬───────┴───────────────┘                │    │")
	fmt.Println("  │  │                │                                         │    │")
	fmt.Println("  │  │         mergeService()                                  │    │")
	fmt.Println("  │  │     (hostname 기준 병합)                                  │    │")
	fmt.Println("  │  │     (ClusterVIPs 통합)                                   │    │")
	fmt.Println("  │  │     (ServiceAccounts 병합)                               │    │")
	fmt.Println("  │  └──────────────────────────────────────────────────────────┘    │")
	fmt.Println("  └──────────────────────────────────────────────────────────────────┘")
	fmt.Println("                    │ xDS push (EDS, CDS)")
	fmt.Println("                    ▼")
	fmt.Println("  ┌──────────┐  ┌──────────┐  ┌──────────┐")
	fmt.Println("  │ Envoy-1  │  │ Envoy-2  │  │ Envoy-3  │")
	fmt.Println("  │(cluster1)│  │(cluster2)│  │(cluster3)│")
	fmt.Println("  └──────────┘  └──────────┘  └──────────┘")
	fmt.Println()

	fmt.Println("  [크로스 네트워크 트래픽 흐름]")
	fmt.Println()
	fmt.Println("  cluster-1 (net-west)           cluster-3 (net-east)")
	fmt.Println("  ┌──────────────────┐           ┌──────────────────┐")
	fmt.Println("  │ ┌──────┐         │           │         ┌──────┐ │")
	fmt.Println("  │ │ Pod  │─→ Envoy │           │ Envoy ←─│ Pod  │ │")
	fmt.Println("  │ └──────┘    │    │           │   │     └──────┘ │")
	fmt.Println("  │             ▼    │           │   ▼              │")
	fmt.Println("  │    ┌────────────┐│           │┌────────────┐    │")
	fmt.Println("  │    │ EW Gateway ├┼───mTLS───→┼┤ EW Gateway │    │")
	fmt.Println("  │    │ 35.192.0.1 ││  :15443   ││ 35.194.0.1 │    │")
	fmt.Println("  │    └────────────┘│           │└────────────┘    │")
	fmt.Println("  └──────────────────┘           └──────────────────┘")
	fmt.Println()

	fmt.Println("  핵심 개념:")
	fmt.Println("  1. Aggregate Controller: 모든 클러스터 레지스트리를 통합하는 단일 뷰")
	fmt.Println("  2. 서비스 병합: 동일 hostname → ClusterVIPs 통합 + ServiceAccounts 병합")
	fmt.Println("  3. Locality-aware LB: Region/Zone/Subzone 근접도 기반 엔드포인트 선택")
	fmt.Println("  4. Network Gateway: 다른 네트워크의 클러스터는 East-West Gateway 경유")
	fmt.Println("  5. 동적 관리: kubeconfig secret 감시로 클러스터 자동 추가/삭제")
	fmt.Println()

	fmt.Println("  소스 참조:")
	fmt.Println("  - pilot/pkg/serviceregistry/aggregate/controller.go: Services(), mergeService()")
	fmt.Println("  - pilot/pkg/serviceregistry/kube/controller/multicluster.go: Multicluster 구조체")
	fmt.Println("  - pilot/pkg/model/service.go: Service, ClusterVIPs, AddressMap")
	fmt.Println("  - pilot/pkg/model/network.go: NetworkGateway")
	fmt.Println("  - pkg/workloadapi/workload.proto: LoadBalancing, Locality")
	fmt.Println()

	// 시뮬레이션 시간 표기
	_ = time.Now()
	fmt.Println("============================================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println("============================================================")
}
