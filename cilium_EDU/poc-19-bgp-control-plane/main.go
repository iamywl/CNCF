// BGP 제어플레인 시뮬레이션
//
// 이 PoC는 Cilium BGP 제어플레인의 핵심 개념을 시뮬레이션한다:
// 1. BGP 라우터 인스턴스 관리 (BGPRouterManager)
// 2. ReconcileDiff를 통한 인스턴스 생성/삭제/업데이트 결정
// 3. ConfigReconciler 파이프라인 (우선순위 기반)
// 4. BGP 피어 관리 및 경로 광고/철회
// 5. 이벤트 기반 리컨실레이션 루프
//
// 실제 Cilium 구현을 참고:
// - pkg/bgp/manager/manager.go (BGPRouterManager)
// - pkg/bgp/agent/controller.go (Controller)
// - pkg/bgp/types/bgp.go (Router 인터페이스)
// - pkg/bgp/manager/reconciler/reconcilers.go (ConfigReconciler)
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─── BGP 타입 정의 ───

// Family는 BGP 주소 패밀리를 나타낸다 (Cilium types.Family 참고)
type Family struct {
	AFI  string // ipv4, ipv6
	SAFI string // unicast, multicast
}

func (f Family) String() string {
	return f.AFI + "/" + f.SAFI
}

// Path는 BGP 경로를 나타낸다 (Cilium types.Path 참고)
type Path struct {
	Prefix     netip.Prefix
	NextHop    netip.Addr
	Family     Family
	LocalPref  int
	ASPath     []uint32
	Communities []string
}

func (p Path) String() string {
	return fmt.Sprintf("%s via %s (LP:%d, AS-Path:%v)", p.Prefix, p.NextHop, p.LocalPref, p.ASPath)
}

// Neighbor는 BGP 피어를 나타낸다 (Cilium types.Neighbor 참고)
type Neighbor struct {
	Name    string
	Address netip.Addr
	ASN     uint32
	State   string // Idle, Connect, Active, OpenSent, OpenConfirm, Established
}

// RoutePolicy는 라우트 정책을 나타낸다 (Cilium types.RoutePolicy 참고)
type RoutePolicy struct {
	Name       string
	Type       string // export, import
	Action     string // accept, reject
	MatchPrefix []netip.Prefix
}

// ─── Router 인터페이스 (Cilium types.Router 참고) ───

type Router interface {
	AddNeighbor(n *Neighbor) error
	RemoveNeighbor(n *Neighbor) error
	AdvertisePath(p *Path) error
	WithdrawPath(p *Path) error
	AddRoutePolicy(rp *RoutePolicy) error
	GetPeerState() []*Neighbor
	GetRoutes() []*Path
	Stop()
}

// ─── 시뮬레이션 BGP 라우터 (GoBGPServer 모방) ───

type SimBGPRouter struct {
	mu        sync.RWMutex
	asn       uint32
	routerID  string
	port      int32
	neighbors map[string]*Neighbor
	routes    map[string]*Path
	policies  map[string]*RoutePolicy
	running   bool
}

func NewSimBGPRouter(asn uint32, routerID string, port int32) *SimBGPRouter {
	return &SimBGPRouter{
		asn:       asn,
		routerID:  routerID,
		port:      port,
		neighbors: make(map[string]*Neighbor),
		routes:    make(map[string]*Path),
		policies:  make(map[string]*RoutePolicy),
		running:   true,
	}
}

func (r *SimBGPRouter) AddNeighbor(n *Neighbor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return fmt.Errorf("라우터가 중지됨")
	}
	n.State = "Established" // 시뮬레이션에서는 즉시 Established
	r.neighbors[n.Address.String()] = n
	fmt.Printf("  [Router ASN=%d] 피어 추가: %s (ASN=%d) → %s\n", r.asn, n.Address, n.ASN, n.State)
	return nil
}

func (r *SimBGPRouter) RemoveNeighbor(n *Neighbor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.neighbors, n.Address.String())
	fmt.Printf("  [Router ASN=%d] 피어 제거: %s\n", r.asn, n.Address)
	return nil
}

func (r *SimBGPRouter) AdvertisePath(p *Path) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return fmt.Errorf("라우터가 중지됨")
	}
	r.routes[p.Prefix.String()] = p
	fmt.Printf("  [Router ASN=%d] 경로 광고: %s\n", r.asn, p)
	return nil
}

func (r *SimBGPRouter) WithdrawPath(p *Path) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, p.Prefix.String())
	fmt.Printf("  [Router ASN=%d] 경로 철회: %s\n", r.asn, p.Prefix)
	return nil
}

func (r *SimBGPRouter) AddRoutePolicy(rp *RoutePolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[rp.Name] = rp
	fmt.Printf("  [Router ASN=%d] 정책 추가: %s (type=%s, action=%s)\n", r.asn, rp.Name, rp.Type, rp.Action)
	return nil
}

func (r *SimBGPRouter) GetPeerState() []*Neighbor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var peers []*Neighbor
	for _, n := range r.neighbors {
		peers = append(peers, n)
	}
	return peers
}

func (r *SimBGPRouter) GetRoutes() []*Path {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var routes []*Path
	for _, p := range r.routes {
		routes = append(routes, p)
	}
	return routes
}

func (r *SimBGPRouter) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
	fmt.Printf("  [Router ASN=%d] 라우터 중지\n", r.asn)
}

// ─── BGP Instance (Cilium instance.BGPInstance 참고) ───

type BGPInstance struct {
	Name     string
	Router   Router
	Config   *BGPNodeInstance
	LocalASN uint32
}

// ─── 설정 구조체 ───

// BGPNodeInstance는 CiliumBGPNodeInstance를 모방
type BGPNodeInstance struct {
	Name     string
	LocalASN uint32
	RouterID string
	Port     int32
	Peers    []BGPNodePeer
}

type BGPNodePeer struct {
	Name    string
	Address string
	ASN     uint32
}

// BGPNodeConfig는 CiliumBGPNodeConfig를 모방
type BGPNodeConfig struct {
	NodeName  string
	Instances []BGPNodeInstance
}

// ─── ConfigReconciler 인터페이스 (Cilium reconciler.ConfigReconciler 참고) ───

type ConfigReconciler interface {
	Name() string
	Priority() int
	Init(i *BGPInstance) error
	Cleanup(i *BGPInstance)
	Reconcile(ctx context.Context, instance *BGPInstance, config *BGPNodeInstance) error
}

// ─── NeighborReconciler (Cilium reconciler.NeighborReconciler 참고) ───

type NeighborReconciler struct {
	currentPeers map[string]map[string]*Neighbor // instance → peer_addr → Neighbor
}

func NewNeighborReconciler() *NeighborReconciler {
	return &NeighborReconciler{
		currentPeers: make(map[string]map[string]*Neighbor),
	}
}

func (r *NeighborReconciler) Name() string    { return "Neighbor" }
func (r *NeighborReconciler) Priority() int   { return 60 }

func (r *NeighborReconciler) Init(i *BGPInstance) error {
	r.currentPeers[i.Name] = make(map[string]*Neighbor)
	return nil
}

func (r *NeighborReconciler) Cleanup(i *BGPInstance) {
	delete(r.currentPeers, i.Name)
}

func (r *NeighborReconciler) Reconcile(ctx context.Context, instance *BGPInstance, config *BGPNodeInstance) error {
	current := r.currentPeers[instance.Name]
	desired := make(map[string]*Neighbor)

	// 원하는 피어 구성
	for _, p := range config.Peers {
		addr, err := netip.ParseAddr(p.Address)
		if err != nil {
			continue
		}
		desired[p.Address] = &Neighbor{
			Name:    p.Name,
			Address: addr,
			ASN:     p.ASN,
		}
	}

	// 추가할 피어
	for addr, n := range desired {
		if _, exists := current[addr]; !exists {
			if err := instance.Router.AddNeighbor(n); err != nil {
				return err
			}
			current[addr] = n
		}
	}

	// 제거할 피어
	for addr, n := range current {
		if _, exists := desired[addr]; !exists {
			if err := instance.Router.RemoveNeighbor(n); err != nil {
				return err
			}
			delete(current, addr)
		}
	}

	r.currentPeers[instance.Name] = current
	return nil
}

// ─── PodCIDRReconciler (Cilium reconciler.PodCIDRReconciler 참고) ───

type PodCIDRReconciler struct {
	podCIDRs      []netip.Prefix
	currentPaths  map[string]map[string]*Path // instance → prefix → Path
}

func NewPodCIDRReconciler(podCIDRs []netip.Prefix) *PodCIDRReconciler {
	return &PodCIDRReconciler{
		podCIDRs:     podCIDRs,
		currentPaths: make(map[string]map[string]*Path),
	}
}

func (r *PodCIDRReconciler) Name() string    { return "PodCIDR" }
func (r *PodCIDRReconciler) Priority() int   { return 30 }

func (r *PodCIDRReconciler) Init(i *BGPInstance) error {
	r.currentPaths[i.Name] = make(map[string]*Path)
	return nil
}

func (r *PodCIDRReconciler) Cleanup(i *BGPInstance) {
	delete(r.currentPaths, i.Name)
}

func (r *PodCIDRReconciler) Reconcile(ctx context.Context, instance *BGPInstance, config *BGPNodeInstance) error {
	current := r.currentPaths[instance.Name]
	desired := make(map[string]*Path)

	nodeIP := netip.MustParseAddr("10.0.0.1") // 시뮬레이션 노드 IP
	for _, cidr := range r.podCIDRs {
		path := &Path{
			Prefix:  cidr,
			NextHop: nodeIP,
			Family:  Family{AFI: "ipv4", SAFI: "unicast"},
			ASPath:  []uint32{config.LocalASN},
		}
		desired[cidr.String()] = path
	}

	// 추가
	for prefix, path := range desired {
		if _, exists := current[prefix]; !exists {
			if err := instance.Router.AdvertisePath(path); err != nil {
				return err
			}
			current[prefix] = path
		}
	}

	// 제거
	for prefix, path := range current {
		if _, exists := desired[prefix]; !exists {
			if err := instance.Router.WithdrawPath(path); err != nil {
				return err
			}
			delete(current, prefix)
		}
	}

	r.currentPaths[instance.Name] = current
	return nil
}

// ─── ServiceReconciler (Cilium reconciler.ServiceReconciler 참고) ───

type ServiceReconciler struct {
	serviceVIPs  []netip.Addr
	currentPaths map[string]map[string]*Path
}

func NewServiceReconciler(vips []netip.Addr) *ServiceReconciler {
	return &ServiceReconciler{
		serviceVIPs:  vips,
		currentPaths: make(map[string]map[string]*Path),
	}
}

func (r *ServiceReconciler) Name() string    { return "Service" }
func (r *ServiceReconciler) Priority() int   { return 40 }

func (r *ServiceReconciler) Init(i *BGPInstance) error {
	r.currentPaths[i.Name] = make(map[string]*Path)
	return nil
}

func (r *ServiceReconciler) Cleanup(i *BGPInstance) {
	delete(r.currentPaths, i.Name)
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, instance *BGPInstance, config *BGPNodeInstance) error {
	current := r.currentPaths[instance.Name]
	desired := make(map[string]*Path)

	nodeIP := netip.MustParseAddr("10.0.0.1")
	for _, vip := range r.serviceVIPs {
		prefix := netip.PrefixFrom(vip, 32)
		path := &Path{
			Prefix:  prefix,
			NextHop: nodeIP,
			Family:  Family{AFI: "ipv4", SAFI: "unicast"},
			ASPath:  []uint32{config.LocalASN},
		}
		desired[prefix.String()] = path
	}

	for prefix, path := range desired {
		if _, exists := current[prefix]; !exists {
			if err := instance.Router.AdvertisePath(path); err != nil {
				return err
			}
			current[prefix] = path
		}
	}

	for prefix, path := range current {
		if _, exists := desired[prefix]; !exists {
			if err := instance.Router.WithdrawPath(path); err != nil {
				return err
			}
			delete(current, prefix)
		}
	}

	r.currentPaths[instance.Name] = current
	return nil
}

// ─── ReconcileDiff (Cilium manager.reconcileDiff 참고) ───

type ReconcileDiff struct {
	register  []string                        // 새로 생성할 인스턴스
	withdraw  []string                        // 제거할 인스턴스
	reconcile []string                        // 업데이트할 인스턴스
	seen      map[string]*BGPNodeInstance     // 원하는 설정 맵
}

func NewReconcileDiff() *ReconcileDiff {
	return &ReconcileDiff{
		seen: make(map[string]*BGPNodeInstance),
	}
}

func (rd *ReconcileDiff) Diff(current map[string]*BGPInstance, desired *BGPNodeConfig) {
	if desired == nil {
		// 모든 인스턴스 삭제
		for name := range current {
			rd.withdraw = append(rd.withdraw, name)
		}
		return
	}

	// desired 인스턴스를 seen에 등록
	desiredSet := make(map[string]bool)
	for i := range desired.Instances {
		inst := &desired.Instances[i]
		rd.seen[inst.Name] = inst
		desiredSet[inst.Name] = true
	}

	// 현재 실행 중이지만 desired에 없는 → withdraw
	for name := range current {
		if !desiredSet[name] {
			rd.withdraw = append(rd.withdraw, name)
		}
	}

	// desired에 있지만 현재 실행 중이 아닌 → register
	for name := range desiredSet {
		if _, exists := current[name]; !exists {
			rd.register = append(rd.register, name)
		} else {
			rd.reconcile = append(rd.reconcile, name)
		}
	}
}

func (rd *ReconcileDiff) Empty() bool {
	return len(rd.register) == 0 && len(rd.withdraw) == 0 && len(rd.reconcile) == 0
}

// ─── BGPRouterManager (Cilium manager.BGPRouterManager 참고) ───

type BGPRouterManager struct {
	mu          sync.RWMutex
	instances   map[string]*BGPInstance
	reconcilers []ConfigReconciler
	running     bool
}

func NewBGPRouterManager(reconcilers []ConfigReconciler) *BGPRouterManager {
	// 우선순위 순으로 정렬 (Cilium의 GetActiveReconcilers 참고)
	sort.Slice(reconcilers, func(i, j int) bool {
		return reconcilers[i].Priority() < reconcilers[j].Priority()
	})

	return &BGPRouterManager{
		instances:   make(map[string]*BGPInstance),
		reconcilers: reconcilers,
		running:     true,
	}
}

func (m *BGPRouterManager) ReconcileInstances(ctx context.Context, nodeConfig *BGPNodeConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rd := NewReconcileDiff()
	rd.Diff(m.instances, nodeConfig)

	if rd.Empty() {
		fmt.Println("  인스턴스가 최신 상태입니다")
		return nil
	}

	// Withdraw
	for _, name := range rd.withdraw {
		m.withdrawInstance(ctx, name)
	}

	// Register
	for _, name := range rd.register {
		config := rd.seen[name]
		if err := m.registerInstance(ctx, config); err != nil {
			return err
		}
	}

	// Reconcile
	for _, name := range rd.reconcile {
		config := rd.seen[name]
		instance := m.instances[name]
		if err := m.reconcileBGPConfig(ctx, instance, config); err != nil {
			return err
		}
	}

	return nil
}

func (m *BGPRouterManager) registerInstance(ctx context.Context, config *BGPNodeInstance) error {
	fmt.Printf("\n▶ BGP 인스턴스 등록: %s (ASN=%d, RouterID=%s)\n", config.Name, config.LocalASN, config.RouterID)

	router := NewSimBGPRouter(config.LocalASN, config.RouterID, config.Port)
	instance := &BGPInstance{
		Name:     config.Name,
		Router:   router,
		Config:   config,
		LocalASN: config.LocalASN,
	}

	m.instances[config.Name] = instance

	// 각 리컨실러 초기화
	for _, r := range m.reconcilers {
		if err := r.Init(instance); err != nil {
			return fmt.Errorf("[%s] 초기화 실패: %w", r.Name(), err)
		}
	}

	// 초기 리컨실레이션
	return m.reconcileBGPConfig(ctx, instance, config)
}

func (m *BGPRouterManager) withdrawInstance(ctx context.Context, name string) {
	instance, ok := m.instances[name]
	if !ok {
		return
	}

	fmt.Printf("\n▶ BGP 인스턴스 제거: %s\n", name)

	for _, r := range m.reconcilers {
		r.Cleanup(instance)
	}
	instance.Router.Stop()
	delete(m.instances, name)
}

func (m *BGPRouterManager) reconcileBGPConfig(ctx context.Context, instance *BGPInstance, config *BGPNodeInstance) error {
	fmt.Printf("\n  리컨실레이션 시작: %s (리컨실러 %d개)\n", config.Name, len(m.reconcilers))

	for _, r := range m.reconcilers {
		fmt.Printf("  ├─ [%s] (우선순위=%d) 실행\n", r.Name(), r.Priority())
		if err := r.Reconcile(ctx, instance, config); err != nil {
			fmt.Printf("  └─ [%s] 에러: %v\n", r.Name(), err)
			return err
		}
	}
	fmt.Printf("  └─ 리컨실레이션 완료\n")

	instance.Config = config
	return nil
}

func (m *BGPRouterManager) GetStatus() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var sb strings.Builder
	for name, inst := range m.instances {
		sb.WriteString(fmt.Sprintf("  인스턴스: %s (ASN=%d)\n", name, inst.LocalASN))
		peers := inst.Router.GetPeerState()
		for _, p := range peers {
			sb.WriteString(fmt.Sprintf("    피어: %s (ASN=%d, State=%s)\n", p.Address, p.ASN, p.State))
		}
		routes := inst.Router.GetRoutes()
		for _, r := range routes {
			sb.WriteString(fmt.Sprintf("    경로: %s\n", r))
		}
	}
	return sb.String()
}

// ─── Controller (Cilium agent.Controller 참고) ───

type Controller struct {
	manager     *BGPRouterManager
	signalCh    chan struct{}
	nodeConfig  *BGPNodeConfig
	mu          sync.Mutex
}

func NewController(manager *BGPRouterManager) *Controller {
	return &Controller{
		manager:  manager,
		signalCh: make(chan struct{}, 1),
	}
}

func (c *Controller) Signal() {
	select {
	case c.signalCh <- struct{}{}:
	default:
	}
}

func (c *Controller) SetNodeConfig(config *BGPNodeConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodeConfig = config
	c.Signal()
}

func (c *Controller) Run(ctx context.Context) {
	fmt.Println("\n=== BGP Controller 시작 ===")
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n=== BGP Controller 종료 ===")
			return
		case <-c.signalCh:
			c.mu.Lock()
			config := c.nodeConfig
			c.mu.Unlock()

			if err := c.manager.ReconcileInstances(ctx, config); err != nil {
				fmt.Printf("  리컨실레이션 에러: %v\n", err)
			}
		}
	}
}

// ─── 메인 ───

func main() {
	fmt.Println("╔════════════════════════════════════════════════════════╗")
	fmt.Println("║     Cilium BGP 제어플레인 시뮬레이션                    ║")
	fmt.Println("╚════════════════════════════════════════════════════════╝")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pod CIDR과 Service VIP 설정
	podCIDRs := []netip.Prefix{
		netip.MustParsePrefix("10.244.1.0/24"),
		netip.MustParsePrefix("10.244.2.0/24"),
	}
	serviceVIPs := []netip.Addr{
		netip.MustParseAddr("192.168.100.10"),
		netip.MustParseAddr("192.168.100.20"),
	}

	// 리컨실러 생성 (Cilium의 ConfigReconcilers 참고)
	reconcilers := []ConfigReconciler{
		NewPodCIDRReconciler(podCIDRs),
		NewServiceReconciler(serviceVIPs),
		NewNeighborReconciler(),
	}

	// BGPRouterManager 생성
	manager := NewBGPRouterManager(reconcilers)

	// Controller 생성 및 실행
	controller := NewController(manager)
	go controller.Run(ctx)

	// ─── 시나리오 1: 초기 BGP 설정 적용 ───
	fmt.Println("\n━━━ 시나리오 1: 초기 BGP 인스턴스 생성 ━━━")
	controller.SetNodeConfig(&BGPNodeConfig{
		NodeName: "worker-1",
		Instances: []BGPNodeInstance{
			{
				Name:     "instance-65001",
				LocalASN: 65001,
				RouterID: "10.0.0.1",
				Port:     179,
				Peers: []BGPNodePeer{
					{Name: "tor-switch-1", Address: "10.0.0.254", ASN: 65000},
					{Name: "tor-switch-2", Address: "10.0.0.253", ASN: 65000},
				},
			},
		},
	})
	time.Sleep(200 * time.Millisecond)

	// 상태 출력
	fmt.Println("\n━━━ 현재 BGP 상태 ━━━")
	fmt.Print(manager.GetStatus())

	// ─── 시나리오 2: 두 번째 인스턴스 추가 ───
	fmt.Println("\n━━━ 시나리오 2: 두 번째 BGP 인스턴스 추가 ━━━")
	controller.SetNodeConfig(&BGPNodeConfig{
		NodeName: "worker-1",
		Instances: []BGPNodeInstance{
			{
				Name:     "instance-65001",
				LocalASN: 65001,
				RouterID: "10.0.0.1",
				Port:     179,
				Peers: []BGPNodePeer{
					{Name: "tor-switch-1", Address: "10.0.0.254", ASN: 65000},
					{Name: "tor-switch-2", Address: "10.0.0.253", ASN: 65000},
				},
			},
			{
				Name:     "instance-65002",
				LocalASN: 65002,
				RouterID: "10.0.0.1",
				Port:     1790,
				Peers: []BGPNodePeer{
					{Name: "spine-1", Address: "10.1.0.1", ASN: 65100},
				},
			},
		},
	})
	time.Sleep(200 * time.Millisecond)

	fmt.Println("\n━━━ 현재 BGP 상태 ━━━")
	fmt.Print(manager.GetStatus())

	// ─── 시나리오 3: 첫 번째 인스턴스 제거 ───
	fmt.Println("\n━━━ 시나리오 3: 첫 번째 인스턴스 제거 ━━━")
	controller.SetNodeConfig(&BGPNodeConfig{
		NodeName: "worker-1",
		Instances: []BGPNodeInstance{
			{
				Name:     "instance-65002",
				LocalASN: 65002,
				RouterID: "10.0.0.1",
				Port:     1790,
				Peers: []BGPNodePeer{
					{Name: "spine-1", Address: "10.1.0.1", ASN: 65100},
				},
			},
		},
	})
	time.Sleep(200 * time.Millisecond)

	fmt.Println("\n━━━ 현재 BGP 상태 ━━━")
	fmt.Print(manager.GetStatus())

	// ─── 시나리오 4: 모든 인스턴스 제거 (NodeConfig nil) ───
	fmt.Println("\n━━━ 시나리오 4: 모든 인스턴스 제거 ━━━")
	controller.SetNodeConfig(nil)
	time.Sleep(200 * time.Millisecond)

	fmt.Println("\n━━━ 현재 BGP 상태 ━━━")
	status := manager.GetStatus()
	if status == "" {
		fmt.Println("  (인스턴스 없음)")
	} else {
		fmt.Print(status)
	}

	cancel()
	time.Sleep(100 * time.Millisecond)

	fmt.Println("\n╔════════════════════════════════════════════════════════╗")
	fmt.Println("║     시뮬레이션 완료                                     ║")
	fmt.Println("╚════════════════════════════════════════════════════════╝")
}
