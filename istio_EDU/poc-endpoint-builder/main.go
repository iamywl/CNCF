package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Istio 엔드포인트 빌더 및 Locality-aware 로드밸런싱 시뮬레이션
//
// 실제 Istio 소스 참조:
//   - pilot/pkg/xds/endpoints/endpoint_builder.go: EndpointBuilder 구조체, generate(), filterIstioEndpoint()
//   - pilot/pkg/model/endpointshards.go: EndpointShards, ShardKey, EndpointIndex
//   - pilot/pkg/networking/core/loadbalancer/loadbalancer.go: ApplyLocalityLoadBalancer(), applyLocalityFailover()
//   - pilot/pkg/networking/util/util.go: LbPriority() 함수
//   - pilot/pkg/serviceregistry/kube/controller/endpoint_builder.go: buildIstioEndpoint()
//
// 핵심 알고리즘:
// 1. EndpointBuilder가 서비스 엔드포인트를 수집하고 locality별로 그룹화
// 2. 건강 상태(Healthy/Unhealthy/Draining/Terminating)에 따른 필터링
// 3. LbPriority 함수로 locality 우선순위 계산 (region/zone/subzone 매칭)
//    - region+zone+subzone 일치: priority 0
//    - region+zone 일치: priority 1
//    - region만 일치: priority 2
//    - 불일치: priority 3
// 4. EndpointShards를 통한 멀티클러스터 엔드포인트 샤딩
// =============================================================================

// --- 건강 상태 (실제 Istio의 model.HealthStatus) ---
type HealthStatus int

const (
	Healthy     HealthStatus = 0
	UnHealthy   HealthStatus = 1
	Draining    HealthStatus = 2
	Terminating HealthStatus = 3
)

func (h HealthStatus) String() string {
	switch h {
	case Healthy:
		return "Healthy"
	case UnHealthy:
		return "Unhealthy"
	case Draining:
		return "Draining"
	case Terminating:
		return "Terminating"
	}
	return "Unknown"
}

// --- Locality (실제 Istio의 core.Locality) ---
// region/zone/subzone 3단계 계층 구조
type Locality struct {
	Region  string
	Zone    string
	SubZone string
}

func (l Locality) Label() string {
	parts := []string{l.Region}
	if l.Zone != "" {
		parts = append(parts, l.Zone)
	}
	if l.SubZone != "" {
		parts = append(parts, l.SubZone)
	}
	return strings.Join(parts, "/")
}

// --- IstioEndpoint (실제 Istio의 model.IstioEndpoint) ---
// 서비스 메시 내의 개별 엔드포인트를 나타냄
type IstioEndpoint struct {
	Address         string
	Port            uint32
	ServicePortName string
	Labels          map[string]string
	ServiceAccount  string
	Locality        EndpointLocality
	Network         string
	HealthStatus    HealthStatus
	Weight          uint32
	ClusterID       string
	NodeName        string
}

type EndpointLocality struct {
	Label     string // region/zone/subzone 형식
	ClusterID string
}

func (ep *IstioEndpoint) GetLoadBalancingWeight() uint32 {
	if ep.Weight == 0 {
		return 1
	}
	return ep.Weight
}

// --- ShardKey (실제 Istio의 model.ShardKey) ---
// 엔드포인트 샤드의 키: provider/cluster 형식
type ShardKey struct {
	Cluster  string
	Provider string
}

func (sk ShardKey) String() string {
	return sk.Provider + "/" + sk.Cluster
}

// --- EndpointShards (실제 Istio의 model.EndpointShards) ---
// 서비스별 엔드포인트 샤드 세트를 보관
// 레지스트리(클러스터)별로 개별 샤드를 업데이트
type EndpointShards struct {
	sync.RWMutex
	Shards          map[ShardKey][]*IstioEndpoint
	ServiceAccounts map[string]bool
}

func NewEndpointShards() *EndpointShards {
	return &EndpointShards{
		Shards:          make(map[ShardKey][]*IstioEndpoint),
		ServiceAccounts: make(map[string]bool),
	}
}

// Keys는 정렬된 샤드 키 목록을 반환 (안정적인 EDS 출력 보장)
// 실제 코드: endpointshards.go의 Keys() 함수
func (es *EndpointShards) Keys() []ShardKey {
	keys := make([]ShardKey, 0, len(es.Shards))
	for k := range es.Shards {
		keys = append(keys, k)
	}
	if len(keys) >= 2 {
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Provider == keys[j].Provider {
				return keys[i].Cluster < keys[j].Cluster
			}
			return keys[i].Provider < keys[j].Provider
		})
	}
	return keys
}

// --- EndpointIndex (실제 Istio의 model.EndpointIndex) ---
// 서비스+네임스페이스 키로 EndpointShards를 관리하는 인덱스
type EndpointIndex struct {
	mu          sync.RWMutex
	shardsBySvc map[string]map[string]*EndpointShards
}

func NewEndpointIndex() *EndpointIndex {
	return &EndpointIndex{
		shardsBySvc: make(map[string]map[string]*EndpointShards),
	}
}

func (e *EndpointIndex) GetOrCreateEndpointShard(serviceName, namespace string) (*EndpointShards, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.shardsBySvc[serviceName]; !exists {
		e.shardsBySvc[serviceName] = map[string]*EndpointShards{}
	}
	if ep, exists := e.shardsBySvc[serviceName][namespace]; exists {
		return ep, false
	}
	ep := NewEndpointShards()
	e.shardsBySvc[serviceName][namespace] = ep
	return ep, true
}

func (e *EndpointIndex) ShardsForService(serviceName, namespace string) (*EndpointShards, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	byNs, ok := e.shardsBySvc[serviceName]
	if !ok {
		return nil, false
	}
	shards, ok := byNs[namespace]
	return shards, ok
}

// --- LocalityLbEndpoints ---
// 특정 locality의 엔드포인트 그룹
// 실제 Istio의 endpoint_builder.go의 LocalityEndpoints 구조체에 대응
type LocalityLbEndpoints struct {
	Locality            Locality
	Endpoints           []*IstioEndpoint
	LoadBalancingWeight uint32
	Priority            uint32
}

func (l *LocalityLbEndpoints) refreshWeight() {
	var weight uint32
	for _, ep := range l.Endpoints {
		weight += ep.GetLoadBalancingWeight()
	}
	l.LoadBalancingWeight = weight
}

// --- EndpointBuilder (실제 Istio의 endpoints.EndpointBuilder) ---
// EDS 캐시 키를 정의하고 엔드포인트를 생성하는 빌더
type EndpointBuilder struct {
	ClusterName  string
	Network      string
	ClusterID    string
	Locality     Locality
	ClusterLocal bool
	Hostname     string
	Port         int
	SubsetLabels map[string]string
}

// filterIstioEndpoint는 엔드포인트 필터링 로직을 구현
// 실제 코드: endpoint_builder.go의 filterIstioEndpoint() 함수
func (b *EndpointBuilder) filterIstioEndpoint(ep *IstioEndpoint) bool {
	// 클러스터-로컬 서비스는 같은 클러스터의 엔드포인트만 포함
	if b.ClusterLocal && b.ClusterID != ep.Locality.ClusterID {
		return false
	}

	// Unhealthy 엔드포인트 필터링
	if ep.HealthStatus == UnHealthy {
		return false
	}

	// Terminating 엔드포인트는 항상 필터링
	if ep.HealthStatus == Terminating {
		return false
	}

	// Draining 엔드포인트는 persistent session이 아니면 필터링
	if ep.HealthStatus == Draining {
		return false
	}

	// 서브셋 레이블 매칭
	if len(b.SubsetLabels) > 0 {
		for k, v := range b.SubsetLabels {
			if ep.Labels[k] != v {
				return false
			}
		}
	}

	return true
}

// generate는 엔드포인트를 locality별로 그룹화
// 실제 코드: endpoint_builder.go의 generate() 함수
func (b *EndpointBuilder) generate(eps []*IstioEndpoint) []*LocalityLbEndpoints {
	// 1. 필터링
	var filtered []*IstioEndpoint
	for _, ep := range eps {
		if b.filterIstioEndpoint(ep) {
			filtered = append(filtered, ep)
		}
	}

	// 2. locality별 그룹화
	localityEpMap := make(map[string]*LocalityLbEndpoints)
	for _, ep := range filtered {
		locLabel := ep.Locality.Label
		locLbEps, found := localityEpMap[locLabel]
		if !found {
			parts := strings.Split(locLabel, "/")
			loc := Locality{Region: parts[0]}
			if len(parts) > 1 {
				loc.Zone = parts[1]
			}
			if len(parts) > 2 {
				loc.SubZone = parts[2]
			}
			locLbEps = &LocalityLbEndpoints{
				Locality:  loc,
				Endpoints: make([]*IstioEndpoint, 0),
			}
			localityEpMap[locLabel] = locLbEps
		}
		locLbEps.Endpoints = append(locLbEps.Endpoints, ep)
	}

	// 3. 정렬된 순서로 결과 반환 (안정적인 출력)
	locs := make([]string, 0, len(localityEpMap))
	for k := range localityEpMap {
		locs = append(locs, k)
	}
	sort.Strings(locs)

	locEps := make([]*LocalityLbEndpoints, 0, len(localityEpMap))
	for _, locality := range locs {
		locLbEps := localityEpMap[locality]
		locLbEps.refreshWeight()
		locEps = append(locEps, locLbEps)
	}

	return locEps
}

// --- LbPriority (실제 Istio의 util.LbPriority 함수) ---
// 프록시 locality와 엔드포인트 locality를 비교하여 우선순위 반환
// region+zone+subzone 모두 일치: 0
// region+zone 일치: 1
// region만 일치: 2
// 불일치: 3
func LbPriority(proxyLocality, endpointLocality Locality) int {
	if proxyLocality.Region == endpointLocality.Region {
		if proxyLocality.Zone == endpointLocality.Zone {
			if proxyLocality.SubZone == endpointLocality.SubZone {
				return 0
			}
			return 1
		}
		return 2
	}
	return 3
}

// --- applyLocalityFailover (실제 loadbalancer.go의 applyLocalityFailover 함수) ---
// locality 기반 failover 우선순위를 적용
func applyLocalityFailover(proxyLocality Locality, endpoints []*LocalityLbEndpoints) {
	// 우선순위 맵: key=priority, value=인덱스 목록
	priorityMap := map[int][]int{}

	for i, locEp := range endpoints {
		priority := LbPriority(proxyLocality, locEp.Locality)
		// 기존 priority에 곱하기 5를 하여 failover priority와 결합
		// 실제 코드에서는 failoverPriority와 locality priority를 곱셈으로 결합
		priorityInt := int(endpoints[i].Priority*5) + priority
		endpoints[i].Priority = uint32(priorityInt)
		priorityMap[priorityInt] = append(priorityMap[priorityInt], i)
	}

	// 우선순위를 0부터 N까지 연속으로 재배치
	priorities := make([]int, 0, len(priorityMap))
	for p := range priorityMap {
		priorities = append(priorities, p)
	}
	sort.Ints(priorities)

	for i, priority := range priorities {
		if i != priority {
			for _, index := range priorityMap[priority] {
				endpoints[index].Priority = uint32(i)
			}
		}
	}
}

// --- 로드밸런서 시뮬레이션 ---
// 우선순위별 가중치 기반 로드밸런싱
type LocalityLoadBalancer struct {
	proxyLocality Locality
	endpoints     []*LocalityLbEndpoints
	rng           *rand.Rand
}

func NewLocalityLoadBalancer(proxyLocality Locality, endpoints []*LocalityLbEndpoints) *LocalityLoadBalancer {
	return &LocalityLoadBalancer{
		proxyLocality: proxyLocality,
		endpoints:     endpoints,
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// PickEndpoint는 우선순위별로 엔드포인트를 선택
// 가장 높은 우선순위(낮은 번호)의 healthy 엔드포인트 중 가중치 기반 선택
func (lb *LocalityLoadBalancer) PickEndpoint() *IstioEndpoint {
	if len(lb.endpoints) == 0 {
		return nil
	}

	// 가장 높은 우선순위(최소값) 찾기
	minPriority := uint32(999)
	for _, locEp := range lb.endpoints {
		if locEp.Priority < minPriority && len(locEp.Endpoints) > 0 {
			minPriority = locEp.Priority
		}
	}

	// 해당 우선순위의 엔드포인트에서 가중치 기반 선택
	var candidates []*IstioEndpoint
	var weights []uint32
	var totalWeight uint32

	for _, locEp := range lb.endpoints {
		if locEp.Priority == minPriority {
			for _, ep := range locEp.Endpoints {
				candidates = append(candidates, ep)
				w := ep.GetLoadBalancingWeight()
				weights = append(weights, w)
				totalWeight += w
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// 가중치 기반 랜덤 선택
	r := lb.rng.Uint32() % totalWeight
	var cumulative uint32
	for i, w := range weights {
		cumulative += w
		if r < cumulative {
			return candidates[i]
		}
	}
	return candidates[len(candidates)-1]
}

// =============================================================================
// 시뮬레이션 실행
// =============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("Istio EndpointBuilder & Locality-Aware 로드밸런싱 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// -----------------------------------------------------------------------
	// 1단계: 멀티클러스터 엔드포인트 샤딩
	// -----------------------------------------------------------------------
	fmt.Println("\n[1] 멀티클러스터 엔드포인트 샤딩 (EndpointShards)")
	fmt.Println(strings.Repeat("-", 60))

	endpointIndex := NewEndpointIndex()
	serviceName := "reviews.default.svc.cluster.local"
	namespace := "default"

	// cluster-1 샤드 (us-west-1 리전)
	shard1 := ShardKey{Cluster: "cluster-1", Provider: "Kubernetes"}
	eps1 := []*IstioEndpoint{
		{Address: "10.1.1.1", Port: 9080, HealthStatus: Healthy,
			Locality: EndpointLocality{Label: "us-west-1/zone-a/sub-1", ClusterID: "cluster-1"},
			Labels: map[string]string{"version": "v1"}, Weight: 1},
		{Address: "10.1.1.2", Port: 9080, HealthStatus: Healthy,
			Locality: EndpointLocality{Label: "us-west-1/zone-a/sub-2", ClusterID: "cluster-1"},
			Labels: map[string]string{"version": "v1"}, Weight: 1},
		{Address: "10.1.1.3", Port: 9080, HealthStatus: Healthy,
			Locality: EndpointLocality{Label: "us-west-1/zone-b/sub-1", ClusterID: "cluster-1"},
			Labels: map[string]string{"version": "v2"}, Weight: 2},
		{Address: "10.1.1.4", Port: 9080, HealthStatus: UnHealthy,
			Locality: EndpointLocality{Label: "us-west-1/zone-a/sub-1", ClusterID: "cluster-1"},
			Labels: map[string]string{"version": "v1"}, Weight: 1},
		{Address: "10.1.1.5", Port: 9080, HealthStatus: Terminating,
			Locality: EndpointLocality{Label: "us-west-1/zone-b/sub-1", ClusterID: "cluster-1"},
			Labels: map[string]string{"version": "v1"}, Weight: 1},
	}

	// cluster-2 샤드 (us-east-1 리전)
	shard2 := ShardKey{Cluster: "cluster-2", Provider: "Kubernetes"}
	eps2 := []*IstioEndpoint{
		{Address: "10.2.1.1", Port: 9080, HealthStatus: Healthy,
			Locality: EndpointLocality{Label: "us-east-1/zone-c/sub-1", ClusterID: "cluster-2"},
			Labels: map[string]string{"version": "v1"}, Weight: 1},
		{Address: "10.2.1.2", Port: 9080, HealthStatus: Healthy,
			Locality: EndpointLocality{Label: "us-east-1/zone-c/sub-1", ClusterID: "cluster-2"},
			Labels: map[string]string{"version": "v2"}, Weight: 1},
		{Address: "10.2.1.3", Port: 9080, HealthStatus: Draining,
			Locality: EndpointLocality{Label: "us-east-1/zone-d/sub-1", ClusterID: "cluster-2"},
			Labels: map[string]string{"version": "v1"}, Weight: 1},
	}

	// cluster-3 샤드 (eu-west-1 리전)
	shard3 := ShardKey{Cluster: "cluster-3", Provider: "Kubernetes"}
	eps3 := []*IstioEndpoint{
		{Address: "10.3.1.1", Port: 9080, HealthStatus: Healthy,
			Locality: EndpointLocality{Label: "eu-west-1/zone-e/sub-1", ClusterID: "cluster-3"},
			Labels: map[string]string{"version": "v1"}, Weight: 1},
		{Address: "10.3.1.2", Port: 9080, HealthStatus: Healthy,
			Locality: EndpointLocality{Label: "eu-west-1/zone-f/sub-1", ClusterID: "cluster-3"},
			Labels: map[string]string{"version": "v1"}, Weight: 1},
	}

	// EndpointIndex에 샤드 등록
	epShards, _ := endpointIndex.GetOrCreateEndpointShard(serviceName, namespace)
	epShards.Lock()
	epShards.Shards[shard1] = eps1
	epShards.Shards[shard2] = eps2
	epShards.Shards[shard3] = eps3
	epShards.Unlock()

	// 샤드 내용 출력
	epShards.RLock()
	for _, key := range epShards.Keys() {
		eps := epShards.Shards[key]
		fmt.Printf("  샤드 [%s]: %d개 엔드포인트\n", key.String(), len(eps))
		for _, ep := range eps {
			fmt.Printf("    %s:%d  locality=%s  health=%s  weight=%d\n",
				ep.Address, ep.Port, ep.Locality.Label, ep.HealthStatus, ep.GetLoadBalancingWeight())
		}
	}
	epShards.RUnlock()

	// -----------------------------------------------------------------------
	// 2단계: snapshotShards - 클러스터간 엔드포인트 수집
	// -----------------------------------------------------------------------
	fmt.Println("\n[2] EndpointBuilder의 snapshotShards (모든 클러스터 엔드포인트 수집)")
	fmt.Println(strings.Repeat("-", 60))

	// 모든 샤드에서 엔드포인트 수집 (실제 snapshotShards 동작)
	var allEps []*IstioEndpoint
	epShards.RLock()
	for _, key := range epShards.Keys() {
		allEps = append(allEps, epShards.Shards[key]...)
	}
	epShards.RUnlock()

	fmt.Printf("  전체 수집된 엔드포인트: %d개\n", len(allEps))

	// -----------------------------------------------------------------------
	// 3단계: 건강 상태 필터링 + locality 그룹화
	// -----------------------------------------------------------------------
	fmt.Println("\n[3] 건강 상태 필터링 및 Locality 그룹화")
	fmt.Println(strings.Repeat("-", 60))

	builder := &EndpointBuilder{
		ClusterName:  "outbound|9080||reviews.default.svc.cluster.local",
		ClusterID:    "cluster-1",
		ClusterLocal: false,
		Hostname:     serviceName,
		Port:         9080,
	}

	localityEps := builder.generate(allEps)

	fmt.Printf("  필터링 전 엔드포인트: %d개\n", len(allEps))
	totalFiltered := 0
	for _, loc := range localityEps {
		totalFiltered += len(loc.Endpoints)
	}
	fmt.Printf("  필터링 후 엔드포인트: %d개 (Unhealthy/Terminating/Draining 제거)\n", totalFiltered)
	fmt.Printf("  Locality 그룹 수: %d개\n\n", len(localityEps))

	for _, locEp := range localityEps {
		fmt.Printf("  Locality [%s] (weight=%d):\n", locEp.Locality.Label(), locEp.LoadBalancingWeight)
		for _, ep := range locEp.Endpoints {
			fmt.Printf("    %s:%d  health=%s  labels=%v  weight=%d\n",
				ep.Address, ep.Port, ep.HealthStatus, ep.Labels, ep.GetLoadBalancingWeight())
		}
	}

	// -----------------------------------------------------------------------
	// 4단계: Locality-aware Failover 우선순위 계산
	// -----------------------------------------------------------------------
	fmt.Println("\n[4] Locality Failover 우선순위 계산 (LbPriority)")
	fmt.Println(strings.Repeat("-", 60))

	// 프록시는 us-west-1/zone-a/sub-1에 위치
	proxyLocality := Locality{Region: "us-west-1", Zone: "zone-a", SubZone: "sub-1"}
	fmt.Printf("  프록시 Locality: %s\n\n", proxyLocality.Label())

	fmt.Println("  우선순위 계산 (region/zone/subzone 매칭):")
	for _, locEp := range localityEps {
		priority := LbPriority(proxyLocality, locEp.Locality)
		reason := ""
		switch priority {
		case 0:
			reason = "region+zone+subzone 모두 일치"
		case 1:
			reason = "region+zone 일치"
		case 2:
			reason = "region만 일치"
		case 3:
			reason = "locality 불일치"
		}
		fmt.Printf("    %s -> priority=%d (%s)\n", locEp.Locality.Label(), priority, reason)
	}

	// failover 적용
	applyLocalityFailover(proxyLocality, localityEps)

	fmt.Println("\n  Failover 적용 후 우선순위:")
	for _, locEp := range localityEps {
		fmt.Printf("    Priority %d: %s (%d개 엔드포인트)\n",
			locEp.Priority, locEp.Locality.Label(), len(locEp.Endpoints))
	}

	// -----------------------------------------------------------------------
	// 5단계: 요청 분산 시뮬레이션
	// -----------------------------------------------------------------------
	fmt.Println("\n[5] Locality-aware 로드밸런싱 요청 분산 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	lb := NewLocalityLoadBalancer(proxyLocality, localityEps)
	requestCount := 1000
	distribution := make(map[string]int)

	for i := 0; i < requestCount; i++ {
		ep := lb.PickEndpoint()
		if ep != nil {
			key := fmt.Sprintf("%s (%s)", ep.Address, ep.Locality.Label)
			distribution[key]++
		}
	}

	fmt.Printf("  총 %d개 요청 분산 결과:\n\n", requestCount)

	// 정렬된 순서로 출력
	var keys []string
	for k := range distribution {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		count := distribution[key]
		pct := float64(count) / float64(requestCount) * 100
		bar := strings.Repeat("#", int(pct/2))
		fmt.Printf("    %-45s %4d (%5.1f%%) %s\n", key, count, pct, bar)
	}

	fmt.Println("\n  => 같은 zone+subzone(priority 0)의 엔드포인트가 모든 요청을 처리")

	// -----------------------------------------------------------------------
	// 6단계: Failover 시뮬레이션 (같은 zone 엔드포인트 제거)
	// -----------------------------------------------------------------------
	fmt.Println("\n[6] Failover 시뮬레이션 (같은 subzone 장애 시)")
	fmt.Println(strings.Repeat("-", 60))

	// priority 0 엔드포인트 제거 (같은 zone+subzone 장애)
	failoverEps := make([]*LocalityLbEndpoints, 0)
	for _, locEp := range localityEps {
		if locEp.Priority != 0 {
			failoverEps = append(failoverEps, locEp)
		} else {
			fmt.Printf("  장애 발생: %s (priority 0) 엔드포인트 제거\n", locEp.Locality.Label())
		}
	}

	lb2 := NewLocalityLoadBalancer(proxyLocality, failoverEps)
	distribution2 := make(map[string]int)

	for i := 0; i < requestCount; i++ {
		ep := lb2.PickEndpoint()
		if ep != nil {
			key := fmt.Sprintf("%s (%s) [p=%d]", ep.Address, ep.Locality.Label, findPriority(failoverEps, ep))
			distribution2[key]++
		}
	}

	fmt.Printf("\n  Failover 후 %d개 요청 분산:\n\n", requestCount)

	var keys2 []string
	for k := range distribution2 {
		keys2 = append(keys2, k)
	}
	sort.Strings(keys2)

	for _, key := range keys2 {
		count := distribution2[key]
		pct := float64(count) / float64(requestCount) * 100
		bar := strings.Repeat("#", int(pct/2))
		fmt.Printf("    %-55s %4d (%5.1f%%) %s\n", key, count, pct, bar)
	}

	fmt.Println("\n  => 다음 우선순위(같은 zone, 다른 subzone)로 failover 됨")

	// -----------------------------------------------------------------------
	// 7단계: 서브셋 필터링
	// -----------------------------------------------------------------------
	fmt.Println("\n[7] 서브셋 레이블 필터링 (version=v1)")
	fmt.Println(strings.Repeat("-", 60))

	subsetBuilder := &EndpointBuilder{
		ClusterName:  "outbound|9080|v1|reviews.default.svc.cluster.local",
		ClusterID:    "cluster-1",
		ClusterLocal: false,
		Hostname:     serviceName,
		Port:         9080,
		SubsetLabels: map[string]string{"version": "v1"},
	}

	subsetEps := subsetBuilder.generate(allEps)
	fmt.Printf("  서브셋 'v1' 필터링 결과:\n")
	for _, locEp := range subsetEps {
		fmt.Printf("    Locality [%s]: %d개 엔드포인트\n", locEp.Locality.Label(), len(locEp.Endpoints))
		for _, ep := range locEp.Endpoints {
			fmt.Printf("      %s:%d  labels=%v\n", ep.Address, ep.Port, ep.Labels)
		}
	}

	// -----------------------------------------------------------------------
	// 8단계: 클러스터-로컬 서비스
	// -----------------------------------------------------------------------
	fmt.Println("\n[8] 클러스터-로컬 서비스 (같은 클러스터만 허용)")
	fmt.Println(strings.Repeat("-", 60))

	localBuilder := &EndpointBuilder{
		ClusterName:  "outbound|9080||reviews.default.svc.cluster.local",
		ClusterID:    "cluster-1",
		ClusterLocal: true, // 클러스터-로컬 활성화
		Hostname:     serviceName,
		Port:         9080,
	}

	localEps := localBuilder.generate(allEps)
	totalLocal := 0
	for _, loc := range localEps {
		totalLocal += len(loc.Endpoints)
	}
	fmt.Printf("  클러스터-로컬 모드 (cluster-1만 허용):\n")
	fmt.Printf("  필터링 전: %d개 -> 필터링 후: %d개\n", len(allEps), totalLocal)
	for _, locEp := range localEps {
		fmt.Printf("    Locality [%s]: %d개 (클러스터: %s)\n",
			locEp.Locality.Label(), len(locEp.Endpoints), locEp.Endpoints[0].Locality.ClusterID)
	}

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("시뮬레이션 완료!")
	fmt.Println(strings.Repeat("=", 80))
}

func findPriority(eps []*LocalityLbEndpoints, target *IstioEndpoint) uint32 {
	for _, locEp := range eps {
		for _, ep := range locEp.Endpoints {
			if ep.Address == target.Address {
				return locEp.Priority
			}
		}
	}
	return 999
}
