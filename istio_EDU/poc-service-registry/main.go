package main

import (
	"fmt"
	"strings"
	"sync"
)

// =============================================================================
// Istio Service Registry Aggregate 패턴 시뮬레이션
//
// 실제 소스 참조:
//   - pilot/pkg/serviceregistry/instance.go              → Instance 인터페이스
//   - pilot/pkg/serviceregistry/aggregate/controller.go  → AggregateController
//   - pilot/pkg/model/service.go                         → Service, ServiceDiscovery
//   - pilot/pkg/model/controller.go                      → Controller, ServiceHandler
//
// Istio Pilot은 여러 소스(Kubernetes, ServiceEntry 등)에서 서비스 정보를 수집하여
// 단일 뷰로 통합한다. AggregateController가 이 역할을 담당한다.
//
// 핵심 설계:
//   1. ServiceDiscovery 인터페이스로 레지스트리를 추상화
//   2. KubernetesRegistry: K8s Service/Endpoint를 읽어 서비스 제공
//   3. ServiceEntryRegistry: Istio ServiceEntry CRD로 외부 서비스 등록
//   4. AggregateController: 여러 레지스트리를 합쳐 통합 뷰 제공
//      - 같은 hostname의 서비스가 여러 클러스터에 있으면 ClusterVIP를 병합
//      - 이벤트 핸들러로 서비스 변경을 상위에 전파
// =============================================================================

// --- 이벤트 모델 ---

// Event는 서비스 변경 이벤트 타입이다.
type Event int

const (
	EventAdd    Event = iota // 서비스 추가
	EventUpdate              // 서비스 업데이트
	EventDelete              // 서비스 삭제
)

func (e Event) String() string {
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

// ServiceHandler는 서비스 변경 시 호출되는 콜백이다.
// 실제 Istio: model.ServiceHandler = func(prev, curr *Service, event Event)
type ServiceHandler func(prev, curr *Service, event Event)

// --- 서비스 모델 ---

// Port는 서비스 포트이다.
type Port struct {
	Name     string
	Port     int
	Protocol string
}

// Service는 메시 내의 서비스를 나타낸다.
// 실제 Istio의 model.Service 구조체의 핵심 필드를 반영한다.
type Service struct {
	Hostname        string            // FQDN
	Name            string            // 서비스 이름
	Namespace       string            // 네임스페이스
	Ports           []*Port           // 포트 목록
	ClusterVIPs     map[string][]string // 클러스터별 VIP 주소 (멀티클러스터 지원)
	ServiceAccounts []string          // 서비스 계정 (mTLS 인증에 사용)
	MeshExternal    bool              // 외부 서비스 여부 (ServiceEntry)
	Labels          map[string]string // 레이블
}

// ShallowCopy는 서비스의 얕은 복사본을 반환한다.
// 실제 Istio: model.Service.ShallowCopy()
// ClusterVIPs 병합 시 원본을 보호하기 위해 사용한다.
func (s *Service) ShallowCopy() *Service {
	cp := *s
	cp.ClusterVIPs = make(map[string][]string)
	for k, v := range s.ClusterVIPs {
		addrs := make([]string, len(v))
		copy(addrs, v)
		cp.ClusterVIPs[k] = addrs
	}
	if len(s.ServiceAccounts) > 0 {
		sa := make([]string, len(s.ServiceAccounts))
		copy(sa, s.ServiceAccounts)
		cp.ServiceAccounts = sa
	}
	return &cp
}

// --- 레지스트리 인터페이스 ---

// ServiceDiscovery는 서비스 검색 인터페이스이다.
// 실제 Istio: model.ServiceDiscovery 인터페이스
type ServiceDiscovery interface {
	Services() []*Service
	GetService(hostname string) *Service
}

// Controller는 이벤트 핸들러 등록 인터페이스이다.
// 실제 Istio: model.Controller 인터페이스
type Controller interface {
	AppendServiceHandler(f ServiceHandler)
	Run(stop <-chan struct{})
	HasSynced() bool
}

// ProviderID는 레지스트리 제공자를 식별한다.
type ProviderID string

const (
	ProviderKubernetes  ProviderID = "Kubernetes"
	ProviderServiceEntry ProviderID = "ServiceEntry"
)

// RegistryInstance는 레지스트리 인스턴스 인터페이스이다.
// ServiceDiscovery + Controller를 결합한다.
// 실제 Istio: serviceregistry.Instance 인터페이스
type RegistryInstance interface {
	ServiceDiscovery
	Controller
	Provider() ProviderID
	Cluster() string
}

// --- KubernetesRegistry: Kubernetes 서비스 레지스트리 ---

// KubernetesRegistry는 Kubernetes의 Service/Endpoint를 읽어
// Istio 서비스 모델로 변환하는 레지스트리이다.
// 실제로는 K8s Informer를 사용하여 Service, Endpoint, Pod를 감시한다.
type KubernetesRegistry struct {
	mu        sync.RWMutex
	clusterID string
	services  map[string]*Service // hostname → Service
	handlers  []ServiceHandler
	synced    bool
}

func NewKubernetesRegistry(clusterID string) *KubernetesRegistry {
	return &KubernetesRegistry{
		clusterID: clusterID,
		services:  make(map[string]*Service),
	}
}

func (r *KubernetesRegistry) Provider() ProviderID { return ProviderKubernetes }
func (r *KubernetesRegistry) Cluster() string      { return r.clusterID }

func (r *KubernetesRegistry) Services() []*Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Service, 0, len(r.services))
	for _, svc := range r.services {
		result = append(result, svc)
	}
	return result
}

func (r *KubernetesRegistry) GetService(hostname string) *Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.services[hostname]
}

func (r *KubernetesRegistry) AppendServiceHandler(f ServiceHandler) {
	r.handlers = append(r.handlers, f)
}

func (r *KubernetesRegistry) Run(stop <-chan struct{}) {
	r.synced = true
	<-stop
}

func (r *KubernetesRegistry) HasSynced() bool {
	return r.synced
}

// AddService는 Kubernetes 서비스를 등록한다 (K8s Informer 이벤트 시뮬레이션).
func (r *KubernetesRegistry) AddService(svc *Service) {
	r.mu.Lock()
	r.services[svc.Hostname] = svc
	r.mu.Unlock()

	// 서비스 핸들러에 이벤트 전파
	for _, h := range r.handlers {
		h(nil, svc, EventAdd)
	}
}

// UpdateService는 Kubernetes 서비스를 업데이트한다.
func (r *KubernetesRegistry) UpdateService(svc *Service) {
	r.mu.Lock()
	prev := r.services[svc.Hostname]
	r.services[svc.Hostname] = svc
	r.mu.Unlock()

	for _, h := range r.handlers {
		h(prev, svc, EventUpdate)
	}
}

// DeleteService는 Kubernetes 서비스를 삭제한다.
func (r *KubernetesRegistry) DeleteService(hostname string) {
	r.mu.Lock()
	prev := r.services[hostname]
	delete(r.services, hostname)
	r.mu.Unlock()

	if prev != nil {
		for _, h := range r.handlers {
			h(prev, nil, EventDelete)
		}
	}
}

// --- ServiceEntryRegistry: ServiceEntry 레지스트리 ---

// ServiceEntryRegistry는 Istio ServiceEntry CRD로 정의된 외부 서비스를 관리한다.
// ServiceEntry를 통해 메시 외부의 서비스(외부 API, 레거시 시스템 등)를
// Istio의 서비스 모델에 포함시킬 수 있다.
type ServiceEntryRegistry struct {
	mu       sync.RWMutex
	services map[string]*Service
	handlers []ServiceHandler
	synced   bool
}

func NewServiceEntryRegistry() *ServiceEntryRegistry {
	return &ServiceEntryRegistry{
		services: make(map[string]*Service),
	}
}

func (r *ServiceEntryRegistry) Provider() ProviderID { return ProviderServiceEntry }
func (r *ServiceEntryRegistry) Cluster() string      { return "" }

func (r *ServiceEntryRegistry) Services() []*Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Service, 0, len(r.services))
	for _, svc := range r.services {
		result = append(result, svc)
	}
	return result
}

func (r *ServiceEntryRegistry) GetService(hostname string) *Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.services[hostname]
}

func (r *ServiceEntryRegistry) AppendServiceHandler(f ServiceHandler) {
	r.handlers = append(r.handlers, f)
}

func (r *ServiceEntryRegistry) Run(stop <-chan struct{}) {
	r.synced = true
	<-stop
}

func (r *ServiceEntryRegistry) HasSynced() bool {
	return r.synced
}

// AddServiceEntry는 ServiceEntry CRD를 등록한다.
func (r *ServiceEntryRegistry) AddServiceEntry(svc *Service) {
	r.mu.Lock()
	svc.MeshExternal = true
	r.services[svc.Hostname] = svc
	r.mu.Unlock()

	for _, h := range r.handlers {
		h(nil, svc, EventAdd)
	}
}

// DeleteServiceEntry는 ServiceEntry를 삭제한다.
func (r *ServiceEntryRegistry) DeleteServiceEntry(hostname string) {
	r.mu.Lock()
	prev := r.services[hostname]
	delete(r.services, hostname)
	r.mu.Unlock()

	if prev != nil {
		for _, h := range r.handlers {
			h(prev, nil, EventDelete)
		}
	}
}

// --- AggregateController: 레지스트리 통합 컨트롤러 ---

// AggregateController는 여러 레지스트리를 통합하여 단일 ServiceDiscovery를 제공한다.
// 실제 소스: pilot/pkg/serviceregistry/aggregate/controller.go
//
// 핵심 설계:
//   1. registries 슬라이스에 Kubernetes > ServiceEntry 순서로 레지스트리 저장
//   2. Services() 호출 시 모든 레지스트리의 서비스를 순회하며 합침
//   3. 같은 hostname의 K8s 서비스가 여러 클러스터에 있으면 ClusterVIP 병합
//   4. 레지스트리에 핸들러를 등록하여 이벤트를 상위로 전파
type AggregateController struct {
	mu         sync.RWMutex
	registries []RegistryInstance
	handlers   []ServiceHandler
	running    bool
}

func NewAggregateController() *AggregateController {
	return &AggregateController{
		registries: make([]RegistryInstance, 0),
	}
}

// AddRegistry는 레지스트리를 추가한다.
// Kubernetes 레지스트리는 앞에, 나머지는 뒤에 추가한다.
// 실제 Istio: aggregate/controller.go의 addRegistry 함수
func (c *AggregateController) AddRegistry(registry RegistryInstance) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Kubernetes 레지스트리를 앞에 배치하는 로직
	// 실제 Istio에서도 동일한 패턴으로 K8s 레지스트리에 우선순위를 부여한다
	added := false
	if registry.Provider() == ProviderKubernetes {
		for i, r := range c.registries {
			if r.Provider() != ProviderKubernetes {
				// 첫 번째 non-K8s 레지스트리 위치에 삽입
				c.registries = append(c.registries[:i+1], c.registries[i:]...)
				c.registries[i] = registry
				added = true
				break
			}
		}
	}
	if !added {
		c.registries = append(c.registries, registry)
	}

	// 레지스트리의 이벤트를 AggregateController의 핸들러로 전파
	registry.AppendServiceHandler(func(prev, curr *Service, event Event) {
		c.notifyHandlers(prev, curr, event)
	})
}

func (c *AggregateController) notifyHandlers(prev, curr *Service, event Event) {
	for _, h := range c.handlers {
		h(prev, curr, event)
	}
}

// AppendServiceHandler는 서비스 변경 핸들러를 등록한다.
func (c *AggregateController) AppendServiceHandler(f ServiceHandler) {
	c.handlers = append(c.handlers, f)
}

// Services는 모든 레지스트리의 서비스를 통합 반환한다.
// 실제 소스: aggregate/controller.go의 Services() 메서드
//
// 핵심 알고리즘:
//   1. 각 레지스트리의 서비스를 순회
//   2. K8s 레지스트리: hostname을 키로 중복 체크
//      - 첫 등장: 그대로 추가
//      - 재등장(다른 클러스터): ClusterVIP 병합 + ServiceAccount 병합
//   3. ServiceEntry 레지스트리: 그대로 추가 (중복 체크 없음)
func (c *AggregateController) Services() []*Service {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// smap: hostname → 결과 슬라이스의 인덱스
	smap := make(map[string]int)
	index := 0
	services := make([]*Service, 0)

	for _, r := range c.registries {
		svcs := r.Services()
		if r.Provider() != ProviderKubernetes {
			// ServiceEntry 등 non-K8s 레지스트리: 그대로 추가
			index += len(svcs)
			services = append(services, svcs...)
		} else {
			// Kubernetes 레지스트리: hostname 기반 중복 체크
			for _, s := range svcs {
				previous, ok := smap[s.Hostname]
				if !ok {
					// 첫 등장
					smap[s.Hostname] = index
					index++
					services = append(services, s)
				} else {
					// 재등장 (다른 K8s 클러스터)
					// 깊은 복사 후 병합 (원본 보호)
					if len(services[previous].ClusterVIPs) < 2 {
						services[previous] = services[previous].ShallowCopy()
					}
					mergeService(services[previous], s, r)
				}
			}
		}
	}

	return services
}

// GetService는 hostname으로 서비스를 검색한다.
// 실제 소스: aggregate/controller.go의 GetService() 메서드
func (c *AggregateController) GetService(hostname string) *Service {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out *Service
	for _, r := range c.registries {
		service := r.GetService(hostname)
		if service == nil {
			continue
		}
		// non-K8s 레지스트리의 결과는 바로 반환
		if r.Provider() != ProviderKubernetes {
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

// mergeService는 두 K8s 클러스터의 서비스를 병합한다.
// 실제 소스: aggregate/controller.go의 mergeService 함수
func mergeService(dst, src *Service, srcRegistry RegistryInstance) {
	clusterID := srcRegistry.Cluster()

	// ClusterVIP 병합: 원본 클러스터의 VIP 주소를 추가
	if _, exists := dst.ClusterVIPs[clusterID]; !exists {
		if srcVIPs, ok := src.ClusterVIPs[clusterID]; ok {
			dst.ClusterVIPs[clusterID] = srcVIPs
		}
	}

	// ServiceAccount 병합: 중복 제거
	if len(src.ServiceAccounts) > 0 {
		saSet := make(map[string]bool)
		for _, sa := range dst.ServiceAccounts {
			saSet[sa] = true
		}
		for _, sa := range src.ServiceAccounts {
			if !saSet[sa] {
				dst.ServiceAccounts = append(dst.ServiceAccounts, sa)
			}
		}
	}
}

// Run은 모든 레지스트리를 시작한다.
func (c *AggregateController) Run(stop <-chan struct{}) {
	c.mu.Lock()
	for _, r := range c.registries {
		go r.Run(stop)
	}
	c.running = true
	c.mu.Unlock()
	<-stop
}

// HasSynced는 모든 레지스트리가 동기화되었는지 확인한다.
func (c *AggregateController) HasSynced() bool {
	for _, r := range c.registries {
		if !r.HasSynced() {
			return false
		}
	}
	return true
}

// --- 출력 헬퍼 ---

func printService(svc *Service) {
	external := ""
	if svc.MeshExternal {
		external = " [외부]"
	}
	fmt.Printf("    호스트: %s%s\n", svc.Hostname, external)
	fmt.Printf("    이름/네임스페이스: %s/%s\n", svc.Name, svc.Namespace)
	if len(svc.Ports) > 0 {
		ports := make([]string, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			ports = append(ports, fmt.Sprintf("%s(%d/%s)", p.Name, p.Port, p.Protocol))
		}
		fmt.Printf("    포트: %s\n", strings.Join(ports, ", "))
	}
	if len(svc.ClusterVIPs) > 0 {
		for cluster, vips := range svc.ClusterVIPs {
			fmt.Printf("    클러스터 VIP [%s]: %v\n", cluster, vips)
		}
	}
	if len(svc.ServiceAccounts) > 0 {
		fmt.Printf("    서비스 계정: %v\n", svc.ServiceAccounts)
	}
}

func printAllServices(services []*Service) {
	fmt.Printf("  총 서비스 수: %d\n", len(services))
	for i, svc := range services {
		fmt.Printf("\n  [%d]\n", i+1)
		printService(svc)
	}
}

// =============================================================================
// main: 시나리오 실행
// =============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("Istio Service Registry Aggregate 패턴 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// --- 레지스트리 생성 ---
	k8sCluster1 := NewKubernetesRegistry("cluster-1")
	k8sCluster2 := NewKubernetesRegistry("cluster-2")
	seRegistry := NewServiceEntryRegistry()

	// --- AggregateController 생성 ---
	aggregate := NewAggregateController()

	// 이벤트 핸들러 등록 (xDS push 트리거 시뮬레이션)
	eventLog := make([]string, 0)
	aggregate.AppendServiceHandler(func(prev, curr *Service, event Event) {
		hostname := ""
		if curr != nil {
			hostname = curr.Hostname
		} else if prev != nil {
			hostname = prev.Hostname
		}
		msg := fmt.Sprintf("  [이벤트] %s: %s", event, hostname)
		eventLog = append(eventLog, msg)
		fmt.Println(msg)
	})

	// 레지스트리 등록
	aggregate.AddRegistry(k8sCluster1)
	aggregate.AddRegistry(k8sCluster2)
	aggregate.AddRegistry(seRegistry)

	// 레지스트리 시작
	stopCh := make(chan struct{})
	go aggregate.Run(stopCh)

	fmt.Println("\n--- 시나리오 1: Kubernetes 서비스 등록 (클러스터 1) ---")
	k8sCluster1.AddService(&Service{
		Hostname:  "reviews.default.svc.cluster.local",
		Name:      "reviews",
		Namespace: "default",
		Ports:     []*Port{{Name: "http", Port: 9080, Protocol: "HTTP"}},
		ClusterVIPs: map[string][]string{
			"cluster-1": {"10.96.0.10"},
		},
		ServiceAccounts: []string{"reviews-sa@cluster-1.iam"},
	})

	k8sCluster1.AddService(&Service{
		Hostname:  "ratings.default.svc.cluster.local",
		Name:      "ratings",
		Namespace: "default",
		Ports:     []*Port{{Name: "http", Port: 9080, Protocol: "HTTP"}},
		ClusterVIPs: map[string][]string{
			"cluster-1": {"10.96.0.11"},
		},
		ServiceAccounts: []string{"ratings-sa@cluster-1.iam"},
	})

	fmt.Println("\n  통합 서비스 목록:")
	printAllServices(aggregate.Services())

	// --- 시나리오 2: 멀티클러스터 서비스 병합 ---
	fmt.Println("\n--- 시나리오 2: 멀티클러스터 서비스 병합 (클러스터 2에 동일 서비스) ---")

	k8sCluster2.AddService(&Service{
		Hostname:  "reviews.default.svc.cluster.local",
		Name:      "reviews",
		Namespace: "default",
		Ports:     []*Port{{Name: "http", Port: 9080, Protocol: "HTTP"}},
		ClusterVIPs: map[string][]string{
			"cluster-2": {"10.100.0.10"},
		},
		ServiceAccounts: []string{"reviews-sa@cluster-2.iam"},
	})

	fmt.Println("\n  통합 서비스 목록 (ClusterVIP 병합 확인):")
	printAllServices(aggregate.Services())

	// reviews 서비스 검색
	fmt.Println("\n  GetService('reviews.default.svc.cluster.local'):")
	reviewsSvc := aggregate.GetService("reviews.default.svc.cluster.local")
	if reviewsSvc != nil {
		printService(reviewsSvc)
		fmt.Println("    → 두 클러스터의 VIP와 ServiceAccount가 병합됨")
	}

	// --- 시나리오 3: ServiceEntry 등록 (외부 서비스) ---
	fmt.Println("\n--- 시나리오 3: ServiceEntry 등록 (외부 API) ---")

	seRegistry.AddServiceEntry(&Service{
		Hostname:  "api.external-service.com",
		Name:      "external-api",
		Namespace: "default",
		Ports: []*Port{
			{Name: "https", Port: 443, Protocol: "HTTPS"},
		},
		ClusterVIPs: map[string][]string{},
		Labels: map[string]string{
			"app": "external-api",
		},
	})

	seRegistry.AddServiceEntry(&Service{
		Hostname:  "database.legacy.corp",
		Name:      "legacy-db",
		Namespace: "default",
		Ports: []*Port{
			{Name: "tcp", Port: 3306, Protocol: "TCP"},
		},
		ClusterVIPs: map[string][]string{},
	})

	fmt.Println("\n  통합 서비스 목록 (K8s + ServiceEntry):")
	printAllServices(aggregate.Services())

	// --- 시나리오 4: 서비스 업데이트 ---
	fmt.Println("\n--- 시나리오 4: 서비스 업데이트 (포트 추가) ---")

	k8sCluster1.UpdateService(&Service{
		Hostname:  "reviews.default.svc.cluster.local",
		Name:      "reviews",
		Namespace: "default",
		Ports: []*Port{
			{Name: "http", Port: 9080, Protocol: "HTTP"},
			{Name: "grpc", Port: 9090, Protocol: "gRPC"},
		},
		ClusterVIPs: map[string][]string{
			"cluster-1": {"10.96.0.10"},
		},
		ServiceAccounts: []string{"reviews-sa@cluster-1.iam"},
	})

	reviewsSvc = aggregate.GetService("reviews.default.svc.cluster.local")
	if reviewsSvc != nil {
		fmt.Println("\n  업데이트된 reviews 서비스:")
		printService(reviewsSvc)
	}

	// --- 시나리오 5: 서비스 삭제 ---
	fmt.Println("\n--- 시나리오 5: 서비스 삭제 ---")

	// ServiceEntry 삭제
	seRegistry.DeleteServiceEntry("database.legacy.corp")

	fmt.Println("\n  삭제 후 통합 서비스 목록:")
	printAllServices(aggregate.Services())

	// --- 시나리오 6: Kubernetes 서비스 삭제 (멀티클러스터) ---
	fmt.Println("\n--- 시나리오 6: 클러스터 2에서 reviews 삭제 ---")
	k8sCluster2.DeleteService("reviews.default.svc.cluster.local")

	reviewsSvc = aggregate.GetService("reviews.default.svc.cluster.local")
	if reviewsSvc != nil {
		fmt.Println("\n  클러스터 2 삭제 후 reviews 서비스:")
		printService(reviewsSvc)
		fmt.Println("    → 클러스터 1의 정보만 남음")
	}

	// --- 이벤트 로그 출력 ---
	fmt.Println("\n--- 이벤트 로그 (총 " + fmt.Sprintf("%d", len(eventLog)) + "개) ---")
	for _, e := range eventLog {
		fmt.Println(e)
	}

	// --- 레지스트리 구조 시각화 ---
	fmt.Println("\n--- Aggregate Registry 구조 ---")
	fmt.Println()
	fmt.Println("  AggregateController")
	fmt.Println("  ├── KubernetesRegistry [cluster-1]")
	fmt.Println("  │   ├── reviews.default.svc.cluster.local")
	fmt.Println("  │   └── ratings.default.svc.cluster.local")
	fmt.Println("  ├── KubernetesRegistry [cluster-2]")
	fmt.Println("  │   └── (삭제됨)")
	fmt.Println("  └── ServiceEntryRegistry")
	fmt.Println("      └── api.external-service.com [외부]")
	fmt.Println()
	fmt.Println("  Services() 호출 시:")
	fmt.Println("  1. 각 레지스트리 순회 (K8s 우선)")
	fmt.Println("  2. K8s 서비스: hostname으로 중복 체크, ClusterVIP 병합")
	fmt.Println("  3. ServiceEntry: 그대로 추가 (MeshExternal=true)")
	fmt.Println("  4. 핸들러로 xDS push 트리거")

	close(stopCh)
	fmt.Println("\n시뮬레이션 완료.")
}
