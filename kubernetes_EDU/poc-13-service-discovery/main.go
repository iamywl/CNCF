package main

import (
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Kubernetes Service Discovery & 로드밸런싱 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/apis/core/types.go           : Service, ServiceSpec, ServiceType, Endpoints, EndpointSubset
//   - pkg/registry/core/service/       : Service REST 저장소, ClusterIP 할당
//   - pkg/proxy/                       : kube-proxy (iptables/ipvs 기반 로드밸런싱)
//   - pkg/controller/endpoint/         : EndpointController (Service→Pod 매핑)
//   - plugin/pkg/admission/serviceaccount/ : ServiceAccount 자동 주입
//
// Kubernetes Service는 Pod 집합에 대한 안정적인 네트워크 추상화를 제공한다:
//   1. ClusterIP: 클러스터 내부 가상 IP 할당 (기본값)
//   2. NodePort: 모든 노드에서 특정 포트로 접근 가능 (ClusterIP 포함)
//   3. LoadBalancer: 외부 로드밸런서 연동 (NodePort + ClusterIP 포함)
//   4. ExternalName: DNS CNAME 레코드 (프록시 없음)
//
// DNS 해석 규칙:
//   <service>.<namespace>.svc.cluster.local → ClusterIP
//   단축형: <service> (같은 namespace), <service>.<namespace> (다른 namespace)

// =============================================================================
// 1. 데이터 모델 — pkg/apis/core/types.go 재현
// =============================================================================

// ServiceType은 Service의 유형
// 실제: pkg/apis/core/types.go의 ServiceType
type ServiceType string

const (
	ServiceTypeClusterIP    ServiceType = "ClusterIP"
	ServiceTypeNodePort     ServiceType = "NodePort"
	ServiceTypeLoadBalancer ServiceType = "LoadBalancer"
	ServiceTypeExternalName ServiceType = "ExternalName"
)

// Protocol은 네트워크 프로토콜
type Protocol string

const (
	ProtocolTCP Protocol = "TCP"
	ProtocolUDP Protocol = "UDP"
)

// ServicePort는 Service가 노출하는 포트 정의
// 실제: pkg/apis/core/types.go의 ServicePort
type ServicePort struct {
	Name       string   // 포트 이름 (예: "http", "grpc")
	Protocol   Protocol // TCP/UDP
	Port       int32    // Service 포트 (ClusterIP에서 수신)
	TargetPort int32    // Pod가 실제 수신하는 포트
	NodePort   int32    // NodePort 타입에서 노드에 할당되는 포트 (30000-32767)
}

// Service는 Kubernetes Service 오브젝트
// 실제: pkg/apis/core/types.go의 Service, ServiceSpec
type Service struct {
	Name        string
	Namespace   string
	Type        ServiceType
	ClusterIP   string            // 할당된 클러스터 IP
	Selector    map[string]string // Pod 선택 레이블
	Ports       []ServicePort
	ExternalIPs []string // 외부 IP (LoadBalancer)
}

// EndpointAddress는 실제 Pod의 네트워크 주소
// 실제: pkg/apis/core/types.go의 EndpointAddress
type EndpointAddress struct {
	IP       string
	NodeName string
	PodName  string
	Ready    bool // readiness probe 결과
}

// EndpointSubset은 동일 포트를 공유하는 주소 그룹
// 실제: pkg/apis/core/types.go의 EndpointSubset
// Endpoints = union of all Subsets
// 각 Subset의 확장된 endpoint 집합 = Addresses x Ports (데카르트 곱)
type EndpointSubset struct {
	Addresses         []EndpointAddress // Ready 상태인 주소
	NotReadyAddresses []EndpointAddress // Not Ready 상태인 주소
	Ports             []ServicePort
}

// Endpoints는 Service에 대응하는 실제 Pod IP 목록
// 실제: pkg/apis/core/types.go의 Endpoints
type Endpoints struct {
	Name      string // Service와 동일한 이름
	Namespace string
	Subsets   []EndpointSubset
}

// Pod는 간단한 Pod 모델
type Pod struct {
	Name      string
	Namespace string
	IP        string
	Labels    map[string]string
	Ready     bool
	NodeName  string
	Ports     []int32 // 컨테이너 포트
}

// Node는 간단한 Node 모델
type Node struct {
	Name string
	IP   string // 노드 IP (NodePort 접근용)
}

// =============================================================================
// 2. ClusterIP 할당기 — pkg/registry/core/service/ipallocator 재현
// =============================================================================

// ClusterIPAllocator는 Service에 ClusterIP를 할당하는 구조체
// 실제: pkg/registry/core/service/ipallocator/range_alloc.go
// CIDR 범위에서 비트맵으로 IP 할당을 추적한다.
type ClusterIPAllocator struct {
	mu        sync.Mutex
	cidr      string    // 서비스 CIDR (예: 10.96.0.0/12)
	baseIP    net.IP    // CIDR 시작 IP
	allocated map[string]bool // 할당된 IP 추적
	nextOctet int       // 다음 할당할 마지막 옥텟
}

// NewClusterIPAllocator는 새 ClusterIP 할당기를 생성한다
func NewClusterIPAllocator(cidr string) *ClusterIPAllocator {
	// 간단히 10.96.0.x 범위 시뮬레이션
	return &ClusterIPAllocator{
		cidr:      cidr,
		baseIP:    net.ParseIP("10.96.0.0"),
		allocated: make(map[string]bool),
		nextOctet: 1, // .0은 네트워크, .1부터 시작
	}
}

// Allocate는 새 ClusterIP를 할당한다
// 실제: pkg/registry/core/service/ipallocator/range_alloc.go의 AllocateNext()
// 비트맵에서 빈 슬롯을 찾아 할당
func (a *ClusterIPAllocator) Allocate() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for i := 0; i < 254; i++ {
		ip := fmt.Sprintf("10.96.0.%d", a.nextOctet)
		a.nextOctet++
		if a.nextOctet > 254 {
			a.nextOctet = 1
		}
		if !a.allocated[ip] {
			a.allocated[ip] = true
			return ip, nil
		}
	}
	return "", fmt.Errorf("ClusterIP 범위 소진")
}

// Release는 ClusterIP를 반환한다
func (a *ClusterIPAllocator) Release(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allocated, ip)
}

// NodePortAllocator는 NodePort 할당기
// 실제: pkg/registry/core/service/portallocator/portallocator.go
// 범위: 30000-32767 (기본값)
type NodePortAllocator struct {
	mu        sync.Mutex
	allocated map[int32]bool
	nextPort  int32
}

func NewNodePortAllocator() *NodePortAllocator {
	return &NodePortAllocator{
		allocated: make(map[int32]bool),
		nextPort:  30000,
	}
}

func (a *NodePortAllocator) Allocate() (int32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for port := a.nextPort; port <= 32767; port++ {
		if !a.allocated[port] {
			a.allocated[port] = true
			a.nextPort = port + 1
			return port, nil
		}
	}
	return 0, fmt.Errorf("NodePort 범위 소진 (30000-32767)")
}

// =============================================================================
// 3. DNS 해석기 — CoreDNS / kube-dns 동작 시뮬레이션
// =============================================================================

// DNSRecord는 DNS 레코드
type DNSRecord struct {
	FQDN      string // 예: my-svc.default.svc.cluster.local
	ClusterIP string
	Type      string // A, CNAME, SRV
	Port      int32  // SRV 레코드용
}

// ClusterDNS는 클러스터 내부 DNS 해석기
// Kubernetes에서 CoreDNS가 Service 이름 → ClusterIP 매핑을 제공한다.
// DNS 쿼리 형식:
//   <service>.<namespace>.svc.<cluster-domain>  → A 레코드 (ClusterIP)
//   _<port>._<protocol>.<service>.<namespace>.svc.<cluster-domain> → SRV 레코드
type ClusterDNS struct {
	mu            sync.RWMutex
	clusterDomain string // 기본: cluster.local
	records       map[string]*DNSRecord
}

func NewClusterDNS(domain string) *ClusterDNS {
	return &ClusterDNS{
		clusterDomain: domain,
		records:       make(map[string]*DNSRecord),
	}
}

// RegisterService는 Service를 DNS에 등록한다
func (d *ClusterDNS) RegisterService(svc *Service) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// A 레코드: <service>.<namespace>.svc.<domain>
	fqdn := fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, d.clusterDomain)
	d.records[fqdn] = &DNSRecord{
		FQDN:      fqdn,
		ClusterIP: svc.ClusterIP,
		Type:      "A",
	}

	// 단축형도 등록 (실제로는 search domain으로 처리)
	d.records[fmt.Sprintf("%s.%s", svc.Name, svc.Namespace)] = d.records[fqdn]
	d.records[svc.Name] = d.records[fqdn]

	// SRV 레코드: _<port-name>._<proto>.<service>.<namespace>.svc.<domain>
	for _, port := range svc.Ports {
		if port.Name != "" {
			srvName := fmt.Sprintf("_%s._%s.%s",
				port.Name, strings.ToLower(string(port.Protocol)), fqdn)
			d.records[srvName] = &DNSRecord{
				FQDN:      srvName,
				ClusterIP: svc.ClusterIP,
				Type:      "SRV",
				Port:      port.Port,
			}
		}
	}
}

// Resolve는 DNS 이름을 해석한다
func (d *ClusterDNS) Resolve(name string) (*DNSRecord, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if record, ok := d.records[name]; ok {
		return record, nil
	}
	return nil, fmt.Errorf("NXDOMAIN: %s", name)
}

// UnregisterService는 Service를 DNS에서 제거한다
func (d *ClusterDNS) UnregisterService(svc *Service) {
	d.mu.Lock()
	defer d.mu.Unlock()

	fqdn := fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, d.clusterDomain)
	delete(d.records, fqdn)
	delete(d.records, fmt.Sprintf("%s.%s", svc.Name, svc.Namespace))
	delete(d.records, svc.Name)

	for _, port := range svc.Ports {
		if port.Name != "" {
			srvName := fmt.Sprintf("_%s._%s.%s",
				port.Name, strings.ToLower(string(port.Protocol)), fqdn)
			delete(d.records, srvName)
		}
	}
}

// =============================================================================
// 4. Endpoint 컨트롤러 — pkg/controller/endpoint 재현
// =============================================================================

// EndpointController는 Service의 selector와 매칭하는 Pod를 찾아
// Endpoints 오브젝트를 자동으로 생성/갱신한다.
// 실제: pkg/controller/endpoint/endpoints_controller.go의 Controller
type EndpointController struct {
	mu        sync.RWMutex
	pods      map[string]*Pod               // namespace/name → Pod
	services  map[string]*Service           // namespace/name → Service
	endpoints map[string]*Endpoints         // namespace/name → Endpoints
}

func NewEndpointController() *EndpointController {
	return &EndpointController{
		pods:      make(map[string]*Pod),
		services:  make(map[string]*Service),
		endpoints: make(map[string]*Endpoints),
	}
}

// matchesSelector는 Pod 레이블이 Service selector와 매칭하는지 확인
// 실제: k8s.io/apimachinery/pkg/labels/selector.go의 Matches()
func matchesSelector(podLabels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

// SyncEndpoints는 Service에 매칭하는 Pod를 찾아 Endpoints를 갱신한다
// 실제: pkg/controller/endpoint/endpoints_controller.go의 syncService()
// 핵심 로직:
//   1. Service의 Selector로 Pod 필터링
//   2. Ready Pod → addresses, Not Ready Pod → notReadyAddresses
//   3. Endpoints 오브젝트 생성/갱신
func (ec *EndpointController) SyncEndpoints(svc *Service) *Endpoints {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	key := svc.Namespace + "/" + svc.Name
	var readyAddrs []EndpointAddress
	var notReadyAddrs []EndpointAddress

	for _, pod := range ec.pods {
		if pod.Namespace != svc.Namespace {
			continue
		}
		if !matchesSelector(pod.Labels, svc.Selector) {
			continue
		}

		addr := EndpointAddress{
			IP:       pod.IP,
			NodeName: pod.NodeName,
			PodName:  pod.Name,
			Ready:    pod.Ready,
		}

		if pod.Ready {
			readyAddrs = append(readyAddrs, addr)
		} else {
			notReadyAddrs = append(notReadyAddrs, addr)
		}
	}

	ep := &Endpoints{
		Name:      svc.Name,
		Namespace: svc.Namespace,
		Subsets: []EndpointSubset{
			{
				Addresses:         readyAddrs,
				NotReadyAddresses: notReadyAddrs,
				Ports:             svc.Ports,
			},
		},
	}

	ec.endpoints[key] = ep
	return ep
}

// AddPod는 Pod를 등록한다
func (ec *EndpointController) AddPod(pod *Pod) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	ec.pods[pod.Namespace+"/"+pod.Name] = pod
}

// RemovePod는 Pod를 제거한다
func (ec *EndpointController) RemovePod(pod *Pod) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	delete(ec.pods, pod.Namespace+"/"+pod.Name)
}

// GetEndpoints는 Service의 Endpoints를 반환한다
func (ec *EndpointController) GetEndpoints(namespace, name string) *Endpoints {
	ec.mu.RLock()
	defer ec.mu.RUnlock()
	return ec.endpoints[namespace+"/"+name]
}

// =============================================================================
// 5. 라운드 로빈 로드밸런서 — kube-proxy 동작 시뮬레이션
// =============================================================================

// LoadBalancer는 Service 엔드포인트에 대한 라운드 로빈 로드밸런서
// 실제: pkg/proxy/endpoints.go, pkg/proxy/iptables/proxier.go
// kube-proxy는 iptables/ipvs 규칙으로 패킷을 라운드 로빈 분배한다.
// iptables 모드: statistic --mode random --probability (1/N, 1/(N-1), ..., 1/1) 체인
// ipvs 모드: rr(라운드로빈), lc(최소연결), sh(소스해시) 등 지원
type LoadBalancer struct {
	mu          sync.RWMutex
	counters    map[string]*atomic.Int64 // service key → 라운드 로빈 카운터
	endpointCtl *EndpointController
}

func NewLoadBalancer(ectl *EndpointController) *LoadBalancer {
	return &LoadBalancer{
		counters:    make(map[string]*atomic.Int64),
		endpointCtl: ectl,
	}
}

// NextEndpoint는 라운드 로빈으로 다음 엔드포인트를 선택한다
// 실제 kube-proxy iptables 모드에서는 iptables -m statistic 체인으로 구현:
//   -A KUBE-SVC-XXX -m statistic --mode random --probability 0.33 -j KUBE-SEP-AAA
//   -A KUBE-SVC-XXX -m statistic --mode random --probability 0.50 -j KUBE-SEP-BBB
//   -A KUBE-SVC-XXX -j KUBE-SEP-CCC
func (lb *LoadBalancer) NextEndpoint(namespace, serviceName string) (*EndpointAddress, error) {
	ep := lb.endpointCtl.GetEndpoints(namespace, serviceName)
	if ep == nil || len(ep.Subsets) == 0 {
		return nil, fmt.Errorf("Service %s/%s에 엔드포인트 없음", namespace, serviceName)
	}

	addrs := ep.Subsets[0].Addresses // Ready 주소만 사용
	if len(addrs) == 0 {
		return nil, fmt.Errorf("Service %s/%s에 Ready 엔드포인트 없음", namespace, serviceName)
	}

	key := namespace + "/" + serviceName
	lb.mu.Lock()
	if _, ok := lb.counters[key]; !ok {
		lb.counters[key] = &atomic.Int64{}
	}
	counter := lb.counters[key]
	lb.mu.Unlock()

	idx := counter.Add(1) - 1
	selected := addrs[idx%int64(len(addrs))]
	return &selected, nil
}

// =============================================================================
// 6. ServiceRegistry — 전체 Service 관리 통합
// =============================================================================

// ServiceRegistry는 Service 생성/삭제를 관리하는 상위 구조체
type ServiceRegistry struct {
	mu            sync.RWMutex
	services      map[string]*Service // namespace/name → Service
	clusterIPAlloc *ClusterIPAllocator
	nodePortAlloc  *NodePortAllocator
	dns           *ClusterDNS
	endpointCtl   *EndpointController
	loadBalancer  *LoadBalancer
	nodes         []*Node
}

func NewServiceRegistry(nodes []*Node) *ServiceRegistry {
	ectl := NewEndpointController()
	return &ServiceRegistry{
		services:       make(map[string]*Service),
		clusterIPAlloc: NewClusterIPAllocator("10.96.0.0/12"),
		nodePortAlloc:  NewNodePortAllocator(),
		dns:            NewClusterDNS("cluster.local"),
		endpointCtl:    ectl,
		loadBalancer:   NewLoadBalancer(ectl),
		nodes:          nodes,
	}
}

// CreateService는 Service를 생성하고 ClusterIP, NodePort 등을 할당한다
// 실제: pkg/registry/core/service/strategy.go의 PrepareForCreate → ClusterIP 할당
func (sr *ServiceRegistry) CreateService(name, namespace string, svcType ServiceType, selector map[string]string, ports []ServicePort) (*Service, error) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	key := namespace + "/" + name

	svc := &Service{
		Name:      name,
		Namespace: namespace,
		Type:      svcType,
		Selector:  selector,
		Ports:     ports,
	}

	// ClusterIP 할당 (ExternalName 제외)
	if svcType != ServiceTypeExternalName {
		ip, err := sr.clusterIPAlloc.Allocate()
		if err != nil {
			return nil, fmt.Errorf("ClusterIP 할당 실패: %v", err)
		}
		svc.ClusterIP = ip
	}

	// NodePort 할당 (NodePort, LoadBalancer)
	if svcType == ServiceTypeNodePort || svcType == ServiceTypeLoadBalancer {
		for i := range svc.Ports {
			if svc.Ports[i].NodePort == 0 {
				port, err := sr.nodePortAlloc.Allocate()
				if err != nil {
					return nil, fmt.Errorf("NodePort 할당 실패: %v", err)
				}
				svc.Ports[i].NodePort = port
			}
		}
	}

	// LoadBalancer 외부 IP 시뮬레이션
	if svcType == ServiceTypeLoadBalancer {
		svc.ExternalIPs = []string{
			fmt.Sprintf("203.0.113.%d", rand.Intn(254)+1),
		}
	}

	sr.services[key] = svc

	// DNS 등록
	sr.dns.RegisterService(svc)

	return svc, nil
}

// DeleteService는 Service를 삭제한다
func (sr *ServiceRegistry) DeleteService(namespace, name string) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	key := namespace + "/" + name
	svc, ok := sr.services[key]
	if !ok {
		return
	}

	// ClusterIP 반환
	if svc.ClusterIP != "" {
		sr.clusterIPAlloc.Release(svc.ClusterIP)
	}

	// DNS 제거
	sr.dns.UnregisterService(svc)

	delete(sr.services, key)
}

// AddPod는 Pod를 등록하고 관련 Service의 Endpoints를 갱신한다
func (sr *ServiceRegistry) AddPod(pod *Pod) {
	sr.endpointCtl.AddPod(pod)

	// 매칭하는 모든 Service에 대해 Endpoints 갱신
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	for _, svc := range sr.services {
		if svc.Namespace == pod.Namespace && matchesSelector(pod.Labels, svc.Selector) {
			sr.endpointCtl.SyncEndpoints(svc)
		}
	}
}

// RemovePod는 Pod를 제거하고 Endpoints를 갱신한다
func (sr *ServiceRegistry) RemovePod(pod *Pod) {
	sr.endpointCtl.RemovePod(pod)

	sr.mu.RLock()
	defer sr.mu.RUnlock()
	for _, svc := range sr.services {
		if svc.Namespace == pod.Namespace && matchesSelector(pod.Labels, svc.Selector) {
			sr.endpointCtl.SyncEndpoints(svc)
		}
	}
}

// =============================================================================
// 7. 데모
// =============================================================================

func printHeader(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printSubHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

func main() {
	rand.Seed(time.Now().UnixNano())

	// 클러스터 노드 구성
	nodes := []*Node{
		{Name: "node-1", IP: "192.168.1.10"},
		{Name: "node-2", IP: "192.168.1.11"},
		{Name: "node-3", IP: "192.168.1.12"},
	}

	registry := NewServiceRegistry(nodes)

	// =====================================================================
	// 데모 1: ClusterIP Service 생성 및 DNS 해석
	// =====================================================================
	printHeader("데모 1: ClusterIP Service 생성 및 DNS 해석")

	svc, err := registry.CreateService(
		"my-web", "default",
		ServiceTypeClusterIP,
		map[string]string{"app": "web", "version": "v1"},
		[]ServicePort{
			{Name: "http", Protocol: ProtocolTCP, Port: 80, TargetPort: 8080},
			{Name: "grpc", Protocol: ProtocolTCP, Port: 9090, TargetPort: 9090},
		},
	)
	if err != nil {
		fmt.Printf("Service 생성 실패: %v\n", err)
		return
	}
	fmt.Printf("Service 생성됨: %s/%s\n", svc.Namespace, svc.Name)
	fmt.Printf("  Type:      %s\n", svc.Type)
	fmt.Printf("  ClusterIP: %s\n", svc.ClusterIP)
	fmt.Printf("  Ports:     ")
	for _, p := range svc.Ports {
		fmt.Printf("%s %d→%d  ", p.Name, p.Port, p.TargetPort)
	}
	fmt.Println()

	// DNS 해석 테스트
	printSubHeader("DNS 해석")
	testNames := []string{
		"my-web",                                       // 단축형 (같은 namespace)
		"my-web.default",                               // namespace 포함
		"my-web.default.svc.cluster.local",             // FQDN
		"_http._tcp.my-web.default.svc.cluster.local",  // SRV 레코드
		"_grpc._tcp.my-web.default.svc.cluster.local",  // SRV 레코드
		"nonexistent.default.svc.cluster.local",        // 존재하지 않는 Service
	}

	for _, name := range testNames {
		record, err := registry.dns.Resolve(name)
		if err != nil {
			fmt.Printf("  %-50s → %v\n", name, err)
		} else {
			if record.Type == "SRV" {
				fmt.Printf("  %-50s → [SRV] %s:%d\n", name, record.ClusterIP, record.Port)
			} else {
				fmt.Printf("  %-50s → [A]   %s\n", name, record.ClusterIP)
			}
		}
	}

	// =====================================================================
	// 데모 2: Pod 등록 및 Endpoint 자동 갱신
	// =====================================================================
	printHeader("데모 2: Pod 등록 및 Endpoint 자동 동기화")

	pods := []*Pod{
		{Name: "web-pod-1", Namespace: "default", IP: "10.244.1.5",
			Labels: map[string]string{"app": "web", "version": "v1"},
			Ready: true, NodeName: "node-1", Ports: []int32{8080}},
		{Name: "web-pod-2", Namespace: "default", IP: "10.244.2.8",
			Labels: map[string]string{"app": "web", "version": "v1"},
			Ready: true, NodeName: "node-2", Ports: []int32{8080}},
		{Name: "web-pod-3", Namespace: "default", IP: "10.244.3.12",
			Labels: map[string]string{"app": "web", "version": "v1"},
			Ready: false, NodeName: "node-3", Ports: []int32{8080}}, // Not Ready
		{Name: "db-pod-1", Namespace: "default", IP: "10.244.1.20",
			Labels: map[string]string{"app": "db"},
			Ready: true, NodeName: "node-1", Ports: []int32{5432}}, // 다른 앱
	}

	for _, pod := range pods {
		registry.AddPod(pod)
	}

	ep := registry.endpointCtl.GetEndpoints("default", "my-web")
	if ep != nil && len(ep.Subsets) > 0 {
		fmt.Printf("Endpoints for %s/%s:\n", ep.Namespace, ep.Name)
		fmt.Printf("  Ready addresses (%d개):\n", len(ep.Subsets[0].Addresses))
		for _, addr := range ep.Subsets[0].Addresses {
			fmt.Printf("    - %s (Pod: %s, Node: %s)\n", addr.IP, addr.PodName, addr.NodeName)
		}
		fmt.Printf("  NotReady addresses (%d개):\n", len(ep.Subsets[0].NotReadyAddresses))
		for _, addr := range ep.Subsets[0].NotReadyAddresses {
			fmt.Printf("    - %s (Pod: %s, Node: %s)\n", addr.IP, addr.PodName, addr.NodeName)
		}
		fmt.Printf("  (db-pod-1은 selector 불일치로 제외됨)\n")
	}

	// =====================================================================
	// 데모 3: 라운드 로빈 로드밸런싱
	// =====================================================================
	printHeader("데모 3: 라운드 로빈 로드밸런싱")

	fmt.Println("ClusterIP로 10번 요청 분배 (Ready 엔드포인트만 사용):")
	distribution := make(map[string]int)
	for i := 0; i < 10; i++ {
		addr, err := registry.loadBalancer.NextEndpoint("default", "my-web")
		if err != nil {
			fmt.Printf("  요청 %d: %v\n", i+1, err)
			continue
		}
		distribution[addr.PodName]++
		fmt.Printf("  요청 %2d → %s (%s)\n", i+1, addr.IP, addr.PodName)
	}
	fmt.Println("\n분배 통계:")
	for pod, count := range distribution {
		fmt.Printf("  %s: %d회 (%d%%)\n", pod, count, count*100/10)
	}

	// =====================================================================
	// 데모 4: NodePort Service
	// =====================================================================
	printHeader("데모 4: NodePort Service")

	npSvc, err := registry.CreateService(
		"api-gateway", "default",
		ServiceTypeNodePort,
		map[string]string{"app": "api"},
		[]ServicePort{
			{Name: "http", Protocol: ProtocolTCP, Port: 80, TargetPort: 3000},
		},
	)
	if err != nil {
		fmt.Printf("NodePort Service 생성 실패: %v\n", err)
		return
	}

	fmt.Printf("NodePort Service 생성됨: %s/%s\n", npSvc.Namespace, npSvc.Name)
	fmt.Printf("  ClusterIP: %s\n", npSvc.ClusterIP)
	fmt.Printf("  NodePort:  %d\n", npSvc.Ports[0].NodePort)
	fmt.Println("\n접근 방법:")
	fmt.Printf("  클러스터 내부: %s:%d\n", npSvc.ClusterIP, npSvc.Ports[0].Port)
	for _, node := range nodes {
		fmt.Printf("  노드 %s: %s:%d\n", node.Name, node.IP, npSvc.Ports[0].NodePort)
	}

	// =====================================================================
	// 데모 5: LoadBalancer Service
	// =====================================================================
	printHeader("데모 5: LoadBalancer Service")

	lbSvc, err := registry.CreateService(
		"public-web", "production",
		ServiceTypeLoadBalancer,
		map[string]string{"app": "frontend"},
		[]ServicePort{
			{Name: "https", Protocol: ProtocolTCP, Port: 443, TargetPort: 8443},
		},
	)
	if err != nil {
		fmt.Printf("LoadBalancer Service 생성 실패: %v\n", err)
		return
	}

	fmt.Printf("LoadBalancer Service 생성됨: %s/%s\n", lbSvc.Namespace, lbSvc.Name)
	fmt.Printf("  ClusterIP:   %s\n", lbSvc.ClusterIP)
	fmt.Printf("  NodePort:    %d\n", lbSvc.Ports[0].NodePort)
	fmt.Printf("  External IP: %s\n", lbSvc.ExternalIPs[0])
	fmt.Println("\n접근 계층 (LoadBalancer ⊃ NodePort ⊃ ClusterIP):")
	fmt.Printf("  L1. 외부: %s:%d (LoadBalancer)\n", lbSvc.ExternalIPs[0], lbSvc.Ports[0].Port)
	for _, node := range nodes {
		fmt.Printf("  L2. 노드: %s:%d (NodePort)\n", node.IP, lbSvc.Ports[0].NodePort)
	}
	fmt.Printf("  L3. 내부: %s:%d (ClusterIP)\n", lbSvc.ClusterIP, lbSvc.Ports[0].Port)

	// =====================================================================
	// 데모 6: Pod 변경 시 Endpoints 자동 갱신
	// =====================================================================
	printHeader("데모 6: Pod 변경 시 Endpoints 동적 갱신")

	printSubHeader("초기 상태")
	ep = registry.endpointCtl.GetEndpoints("default", "my-web")
	fmt.Printf("Ready 엔드포인트 수: %d\n", len(ep.Subsets[0].Addresses))

	// Pod 제거 (스케일다운)
	printSubHeader("web-pod-2 제거 (스케일다운 시뮬레이션)")
	registry.RemovePod(pods[1]) // web-pod-2 제거
	ep = registry.endpointCtl.GetEndpoints("default", "my-web")
	fmt.Printf("Ready 엔드포인트 수: %d\n", len(ep.Subsets[0].Addresses))
	for _, addr := range ep.Subsets[0].Addresses {
		fmt.Printf("  - %s (%s)\n", addr.IP, addr.PodName)
	}

	// NotReady Pod가 Ready 상태로 변경
	printSubHeader("web-pod-3 Ready 상태로 전환")
	pods[2].Ready = true
	registry.endpointCtl.SyncEndpoints(svc)
	ep = registry.endpointCtl.GetEndpoints("default", "my-web")
	fmt.Printf("Ready 엔드포인트 수: %d\n", len(ep.Subsets[0].Addresses))
	for _, addr := range ep.Subsets[0].Addresses {
		fmt.Printf("  - %s (%s)\n", addr.IP, addr.PodName)
	}

	// 새 Pod 추가 (스케일업)
	printSubHeader("web-pod-4 추가 (스케일업 시뮬레이션)")
	newPod := &Pod{
		Name: "web-pod-4", Namespace: "default", IP: "10.244.2.15",
		Labels: map[string]string{"app": "web", "version": "v1"},
		Ready: true, NodeName: "node-2", Ports: []int32{8080},
	}
	registry.AddPod(newPod)
	ep = registry.endpointCtl.GetEndpoints("default", "my-web")
	fmt.Printf("Ready 엔드포인트 수: %d\n", len(ep.Subsets[0].Addresses))
	for _, addr := range ep.Subsets[0].Addresses {
		fmt.Printf("  - %s (%s, %s)\n", addr.IP, addr.PodName, addr.NodeName)
	}

	// =====================================================================
	// 데모 7: 다중 네임스페이스 격리
	// =====================================================================
	printHeader("데모 7: 네임스페이스 격리")

	// 같은 이름의 Service를 다른 namespace에 생성
	svc2, _ := registry.CreateService(
		"my-web", "staging",
		ServiceTypeClusterIP,
		map[string]string{"app": "web"},
		[]ServicePort{
			{Name: "http", Protocol: ProtocolTCP, Port: 80, TargetPort: 8080},
		},
	)

	fmt.Printf("default/my-web  ClusterIP: %s\n", svc.ClusterIP)
	fmt.Printf("staging/my-web  ClusterIP: %s\n", svc2.ClusterIP)
	fmt.Println("\n같은 이름이지만 다른 namespace이므로 서로 다른 ClusterIP가 할당된다.")

	r1, _ := registry.dns.Resolve("my-web.default.svc.cluster.local")
	r2, _ := registry.dns.Resolve("my-web.staging.svc.cluster.local")
	fmt.Printf("\nDNS 해석:\n")
	fmt.Printf("  my-web.default.svc.cluster.local → %s\n", r1.ClusterIP)
	fmt.Printf("  my-web.staging.svc.cluster.local → %s\n", r2.ClusterIP)

	// =====================================================================
	// 요약
	// =====================================================================
	printHeader("요약: Kubernetes Service Discovery 핵심 동작")
	fmt.Println(`
  1. Service 생성 시 ClusterIP(가상 IP)가 자동 할당된다
  2. CoreDNS가 <service>.<namespace>.svc.cluster.local → ClusterIP 매핑을 제공한다
  3. EndpointController가 selector 매칭 Pod를 찾아 Endpoints를 자동 갱신한다
  4. kube-proxy가 ClusterIP 트래픽을 Ready 엔드포인트로 라운드 로빈 분배한다
  5. NodePort = ClusterIP + 모든 노드의 고정 포트 (30000-32767)
  6. LoadBalancer = NodePort + 외부 로드밸런서 (클라우드 제공자)
  7. Pod 추가/삭제/Ready 변경 시 Endpoints가 실시간 갱신된다

  실제 소스 경로:
  - Service 타입/스펙: pkg/apis/core/types.go (ServiceSpec, ServiceType)
  - ClusterIP 할당:   pkg/registry/core/service/ipallocator/
  - Endpoint 동기화:   pkg/controller/endpoint/endpoints_controller.go
  - kube-proxy:       pkg/proxy/ (iptables, ipvs, nftables 모드)
  - DNS (CoreDNS):    클러스터 애드온으로 별도 배포`)
}
