package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes Cloud Controller Manager (CCM) 시뮬레이션
// 참조:
//   - Interface: staging/src/k8s.io/cloud-provider/cloud.go
//   - Node Controller: staging/src/k8s.io/cloud-provider/controllers/node/node_controller.go
//   - Service Controller: staging/src/k8s.io/cloud-provider/controllers/service/controller.go
//   - Route Controller: staging/src/k8s.io/cloud-provider/controllers/route/route_controller.go
//   - Plugin: staging/src/k8s.io/cloud-provider/plugins.go
// =============================================================================

// --- Cloud Provider Interface ---
// 실제: staging/src/k8s.io/cloud-provider/cloud.go:42-69

type CloudProvider interface {
	ProviderName() string
	LoadBalancer() LoadBalancerInterface
	Instances() InstancesInterface
	Routes() RoutesInterface
}

type LoadBalancerInterface interface {
	EnsureLoadBalancer(service string, nodes []string) (string, error)
	UpdateLoadBalancer(service string, nodes []string) error
	EnsureLoadBalancerDeleted(service string) error
}

type InstancesInterface interface {
	InstanceExists(nodeName string) bool
	InstanceShutdown(nodeName string) bool
	InstanceMetadata(nodeName string) InstanceMetadata
}

type InstanceMetadata struct {
	ProviderID   string
	InstanceType string
	Zone         string
	Region       string
	Addresses    []string
}

type RoutesInterface interface {
	ListRoutes() []Route
	CreateRoute(nodeName, cidr string) error
	DeleteRoute(nodeName string) error
}

type Route struct {
	NodeName string
	CIDR     string
}

// --- Fake Cloud Provider ---
// 실제: staging/src/k8s.io/cloud-provider/fake/fake.go

type FakeCloud struct {
	mu           sync.Mutex
	name         string
	instances    map[string]*FakeInstance
	loadBalancers map[string]*FakeLB
	routes       map[string]Route
	calls        []string
}

type FakeInstance struct {
	Exists   bool
	Shutdown bool
	Metadata InstanceMetadata
}

type FakeLB struct {
	Service string
	IP      string
	Nodes   []string
}

func NewFakeCloud(name string) *FakeCloud {
	return &FakeCloud{
		name:         name,
		instances:    make(map[string]*FakeInstance),
		loadBalancers: make(map[string]*FakeLB),
		routes:       make(map[string]Route),
	}
}

func (c *FakeCloud) ProviderName() string            { return c.name }
func (c *FakeCloud) LoadBalancer() LoadBalancerInterface { return c }
func (c *FakeCloud) Instances() InstancesInterface       { return c }
func (c *FakeCloud) Routes() RoutesInterface             { return c }

func (c *FakeCloud) InstanceExists(nodeName string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "InstanceExists:"+nodeName)
	inst, ok := c.instances[nodeName]
	return ok && inst.Exists
}

func (c *FakeCloud) InstanceShutdown(nodeName string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	inst, ok := c.instances[nodeName]
	return ok && inst.Shutdown
}

func (c *FakeCloud) InstanceMetadata(nodeName string) InstanceMetadata {
	c.mu.Lock()
	defer c.mu.Unlock()
	if inst, ok := c.instances[nodeName]; ok {
		return inst.Metadata
	}
	return InstanceMetadata{}
}

func (c *FakeCloud) EnsureLoadBalancer(service string, nodes []string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "EnsureLoadBalancer:"+service)

	ip := fmt.Sprintf("203.0.113.%d", len(c.loadBalancers)+1)
	c.loadBalancers[service] = &FakeLB{Service: service, IP: ip, Nodes: nodes}
	return ip, nil
}

func (c *FakeCloud) UpdateLoadBalancer(service string, nodes []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "UpdateLoadBalancer:"+service)
	if lb, ok := c.loadBalancers[service]; ok {
		lb.Nodes = nodes
	}
	return nil
}

func (c *FakeCloud) EnsureLoadBalancerDeleted(service string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "EnsureLoadBalancerDeleted:"+service)
	delete(c.loadBalancers, service)
	return nil
}

func (c *FakeCloud) ListRoutes() []Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	var routes []Route
	for _, r := range c.routes {
		routes = append(routes, r)
	}
	return routes
}

func (c *FakeCloud) CreateRoute(nodeName, cidr string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "CreateRoute:"+nodeName)
	c.routes[nodeName] = Route{NodeName: nodeName, CIDR: cidr}
	return nil
}

func (c *FakeCloud) DeleteRoute(nodeName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "DeleteRoute:"+nodeName)
	delete(c.routes, nodeName)
	return nil
}

// --- Plugin Registry ---
// 실제: staging/src/k8s.io/cloud-provider/plugins.go

var (
	providersMu sync.Mutex
	providers   = make(map[string]func() CloudProvider)
)

func RegisterCloudProvider(name string, factory func() CloudProvider) {
	providersMu.Lock()
	defer providersMu.Unlock()
	providers[name] = factory
	fmt.Printf("  [Plugin] Cloud Provider '%s' 등록됨\n", name)
}

func InitCloudProvider(name string) CloudProvider {
	providersMu.Lock()
	defer providersMu.Unlock()

	if name == "external" {
		fmt.Printf("  [Plugin] 외부 Cloud Provider → nil 반환\n")
		return nil
	}

	factory, ok := providers[name]
	if !ok {
		fmt.Printf("  [Plugin] '%s' 미등록\n", name)
		return nil
	}
	return factory()
}

// --- Cloud Node Controller ---
// 실제: staging/src/k8s.io/cloud-provider/controllers/node/node_controller.go

type CloudNodeController struct {
	cloud CloudProvider
	nodes map[string]*NodeInfo
}

type NodeInfo struct {
	Name       string
	ProviderID string
	Zone       string
	Region     string
	Labels     map[string]string
	Ready      bool
	Tainted    bool // TaintExternalCloudProvider
}

func NewCloudNodeController(cloud CloudProvider) *CloudNodeController {
	return &CloudNodeController{
		cloud: cloud,
		nodes: make(map[string]*NodeInfo),
	}
}

// syncNode: 노드 초기화 (ProviderID, Zone/Region 라벨 설정)
// 실제: node_controller.go:414-487
func (c *CloudNodeController) syncNode(nodeName string) {
	meta := c.cloud.Instances().InstanceMetadata(nodeName)

	node := &NodeInfo{
		Name:       nodeName,
		ProviderID: meta.ProviderID,
		Zone:       meta.Zone,
		Region:     meta.Region,
		Labels: map[string]string{
			"node.kubernetes.io/instance-type":  meta.InstanceType,
			"topology.kubernetes.io/zone":       meta.Zone,
			"topology.kubernetes.io/region":     meta.Region,
		},
		Ready:   true,
		Tainted: false, // Taint 제거
	}
	c.nodes[nodeName] = node

	fmt.Printf("  [NodeController] syncNode '%s':\n", nodeName)
	fmt.Printf("    ProviderID: %s\n", node.ProviderID)
	fmt.Printf("    Zone: %s, Region: %s\n", node.Zone, node.Region)
	fmt.Printf("    Labels: %v\n", node.Labels)
	fmt.Printf("    TaintExternalCloudProvider 제거됨\n")
}

// --- Node Lifecycle Controller ---
// 실제: staging/src/k8s.io/cloud-provider/controllers/nodelifecycle/node_lifecycle_controller.go

func (c *CloudNodeController) MonitorNodes() {
	fmt.Printf("  [NodeLifecycle] MonitorNodes 시작\n")

	for name, node := range c.nodes {
		if node.Ready {
			continue // Ready 노드는 스킵
		}

		exists := c.cloud.Instances().InstanceExists(name)
		if !exists {
			fmt.Printf("    노드 '%s': 클라우드에서 사라짐 → 삭제\n", name)
			delete(c.nodes, name)
			continue
		}

		shutdown := c.cloud.Instances().InstanceShutdown(name)
		if shutdown {
			fmt.Printf("    노드 '%s': 종료 상태 → ShutdownTaint 추가\n", name)
			node.Tainted = true
		}
	}
}

// --- Service Controller ---
// 실제: staging/src/k8s.io/cloud-provider/controllers/service/controller.go

type ServiceController struct {
	cloud    CloudProvider
	services map[string]*LBService
}

type LBService struct {
	Name     string
	Type     string // "LoadBalancer"
	ExternalIP string
	Nodes    []string
}

func NewServiceController(cloud CloudProvider) *ServiceController {
	return &ServiceController{
		cloud:    cloud,
		services: make(map[string]*LBService),
	}
}

// syncLoadBalancerIfNeeded
// 실제: controller.go:364-438
func (sc *ServiceController) SyncService(name, svcType string, nodes []string) {
	if svcType != "LoadBalancer" {
		fmt.Printf("  [ServiceController] '%s': Type=%s → LB 불필요\n", name, svcType)
		return
	}

	ip, err := sc.cloud.LoadBalancer().EnsureLoadBalancer(name, nodes)
	if err != nil {
		fmt.Printf("  [ServiceController] '%s': LB 생성 실패: %v\n", name, err)
		return
	}

	sc.services[name] = &LBService{
		Name:       name,
		Type:       svcType,
		ExternalIP: ip,
		Nodes:      nodes,
	}
	fmt.Printf("  [ServiceController] '%s': LB 생성 → ExternalIP=%s\n", name, ip)
}

func (sc *ServiceController) DeleteService(name string) {
	if err := sc.cloud.LoadBalancer().EnsureLoadBalancerDeleted(name); err != nil {
		fmt.Printf("  [ServiceController] '%s': LB 삭제 실패: %v\n", name, err)
		return
	}
	delete(sc.services, name)
	fmt.Printf("  [ServiceController] '%s': LB 삭제됨\n", name)
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	fmt.Println("=== Kubernetes Cloud Controller Manager 시뮬레이션 ===")
	fmt.Println()

	// 1. Plugin 등록
	demo1_PluginRegistry()

	// 2. Cloud Node Controller
	demo2_NodeController()

	// 3. Service Controller (LoadBalancer)
	demo3_ServiceController()

	// 4. Route Controller
	demo4_RouteController()

	// 5. Node Lifecycle Controller
	demo5_NodeLifecycle()

	printSummary()
}

func demo1_PluginRegistry() {
	fmt.Println("--- 1. Cloud Provider Plugin 등록 ---")

	RegisterCloudProvider("fake-aws", func() CloudProvider {
		cloud := NewFakeCloud("fake-aws")
		cloud.instances["node-1"] = &FakeInstance{
			Exists: true,
			Metadata: InstanceMetadata{
				ProviderID:   "aws://i-1234567890abcdef0",
				InstanceType: "m5.xlarge",
				Zone:         "us-east-1a",
				Region:       "us-east-1",
				Addresses:    []string{"10.0.1.10"},
			},
		}
		cloud.instances["node-2"] = &FakeInstance{
			Exists: true,
			Metadata: InstanceMetadata{
				ProviderID:   "aws://i-0987654321fedcba0",
				InstanceType: "m5.xlarge",
				Zone:         "us-east-1b",
				Region:       "us-east-1",
				Addresses:    []string{"10.0.2.20"},
			},
		}
		return cloud
	})

	// 초기화
	cloud := InitCloudProvider("fake-aws")
	fmt.Printf("  Provider: %s\n", cloud.ProviderName())

	// 외부 CP
	extCloud := InitCloudProvider("external")
	fmt.Printf("  External: %v\n", extCloud)
	fmt.Println()
}

func demo2_NodeController() {
	fmt.Println("--- 2. Cloud Node Controller (노드 초기화) ---")

	cloud := NewFakeCloud("demo")
	cloud.instances["node-1"] = &FakeInstance{
		Exists: true,
		Metadata: InstanceMetadata{
			ProviderID:   "demo://instance-001",
			InstanceType: "standard-4",
			Zone:         "zone-a",
			Region:       "region-1",
		},
	}

	ctrl := NewCloudNodeController(cloud)
	ctrl.syncNode("node-1")
	fmt.Println()
}

func demo3_ServiceController() {
	fmt.Println("--- 3. Service Controller (LoadBalancer) ---")

	cloud := NewFakeCloud("demo")
	sc := NewServiceController(cloud)

	// LoadBalancer Service 생성
	sc.SyncService("web-svc", "LoadBalancer", []string{"node-1", "node-2"})

	// ClusterIP Service (LB 불필요)
	sc.SyncService("internal-svc", "ClusterIP", nil)

	// Service 삭제
	sc.DeleteService("web-svc")
	fmt.Println()
}

func demo4_RouteController() {
	fmt.Println("--- 4. Route Controller (Pod CIDR) ---")

	cloud := NewFakeCloud("demo")

	// 노드별 Pod CIDR 라우트 생성
	nodes := map[string]string{
		"node-1": "10.244.0.0/24",
		"node-2": "10.244.1.0/24",
		"node-3": "10.244.2.0/24",
	}

	for name, cidr := range nodes {
		cloud.Routes().CreateRoute(name, cidr)
		fmt.Printf("  라우트 생성: %s → %s\n", name, cidr)
	}

	// 노드 삭제 시 라우트 정리
	cloud.Routes().DeleteRoute("node-3")
	fmt.Printf("  라우트 삭제: node-3\n")

	routes := cloud.Routes().ListRoutes()
	fmt.Printf("  현재 라우트: %d개\n", len(routes))
	fmt.Println()
}

func demo5_NodeLifecycle() {
	fmt.Println("--- 5. Node Lifecycle Controller ---")

	cloud := NewFakeCloud("demo")
	cloud.instances["healthy-node"] = &FakeInstance{Exists: true}
	cloud.instances["shutdown-node"] = &FakeInstance{Exists: true, Shutdown: true}
	// "deleted-node"는 instances에 없음 → InstanceExists=false

	ctrl := NewCloudNodeController(cloud)
	ctrl.nodes["healthy-node"] = &NodeInfo{Name: "healthy-node", Ready: false}
	ctrl.nodes["shutdown-node"] = &NodeInfo{Name: "shutdown-node", Ready: false}
	ctrl.nodes["deleted-node"] = &NodeInfo{Name: "deleted-node", Ready: false}

	ctrl.MonitorNodes()
	fmt.Println()
}

func printSummary() {
	_ = time.Now
	_ = strings.Join
	fmt.Println("=== 핵심 정리 ===")
	items := []string{
		"1. CCM은 클라우드 종속 로직을 kube-controller-manager에서 분리한 바이너리다",
		"2. Cloud Provider Interface: LoadBalancer, Instances, InstancesV2, Routes",
		"3. Node Controller: 신규 노드에 ProviderID, Zone/Region 라벨 설정",
		"4. Node Lifecycle: NotReady 노드의 클라우드 존재/종료 상태 확인",
		"5. Service Controller: LoadBalancer 타입 Service의 외부 LB 프로비저닝",
		"6. Route Controller: 노드의 PodCIDR 라우트를 클라우드에 동기화",
	}
	for _, item := range items {
		fmt.Printf("  %s\n", item)
	}
}
