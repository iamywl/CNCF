package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
)

// =============================================================================
// Cilium 핵심 데이터 모델 시뮬레이션
// =============================================================================
//
// Cilium의 핵심 데이터 구조:
//   - Endpoint: 네트워크 엔드포인트 (Pod의 veth 인터페이스에 해당)
//   - Identity: 보안 식별자 (레이블 기반, 클러스터 전역)
//   - Node: 클러스터 노드 정보
//   - IPCache: IP → Identity 매핑 (BPF 맵으로 동기화됨)
//
// 실제 코드 위치:
//   - pkg/endpoint/endpoint.go
//   - pkg/identity/numeric_identity.go
//   - pkg/node/types/node.go
//   - pkg/ipcache/ipcache.go
// =============================================================================

// --- 보안 Identity ---

// NumericIdentity는 Cilium의 숫자형 보안 식별자이다.
// 실제 코드: pkg/identity/numeric_identity.go
// 범위:
//   1-255:        Reserved (host, world, health 등)
//   256-65535:    Cluster-local identities
//   16777216+:    CIDR-based identities
type NumericIdentity uint32

const (
	// 예약된 Identity — 실제 Cilium의 reserved identity 값
	IdentityUnknown     NumericIdentity = 0
	IdentityHost        NumericIdentity = 1  // 호스트 자체
	IdentityWorld       NumericIdentity = 2  // 외부 트래픽
	IdentityUnmanaged   NumericIdentity = 3  // Cilium이 관리하지 않는 엔드포인트
	IdentityHealth      NumericIdentity = 4  // 헬스 체크 엔드포인트
	IdentityInit        NumericIdentity = 5  // 초기화 중인 엔드포인트
	IdentityLocalNode   NumericIdentity = 6  // 로컬 노드
	IdentityKubeAPIServer NumericIdentity = 7 // kube-apiserver
)

func (n NumericIdentity) String() string {
	switch n {
	case IdentityHost:
		return "reserved:host"
	case IdentityWorld:
		return "reserved:world"
	case IdentityHealth:
		return "reserved:health"
	case IdentityInit:
		return "reserved:init"
	default:
		return fmt.Sprintf("identity:%d", n)
	}
}

// Identity는 레이블 기반 보안 식별자이다.
// 실제 코드: pkg/identity/identity.go
// 동일한 레이블 조합을 가진 모든 Pod는 같은 Identity를 공유한다.
type Identity struct {
	ID        NumericIdentity
	Labels    map[string]string // k8s:io.kubernetes.pod.namespace=default 등
	Namespace string
	Source    string // "k8s", "kvstore", "local" 등
}

func (id *Identity) String() string {
	labelStrs := make([]string, 0, len(id.Labels))
	for k, v := range id.Labels {
		labelStrs = append(labelStrs, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(labelStrs)
	return fmt.Sprintf("Identity{ID:%d, Labels:[%s]}", id.ID, strings.Join(labelStrs, ", "))
}

// --- Endpoint ---

// EndpointState는 엔드포인트의 상태를 나타낸다.
// 실제 코드: pkg/endpoint/endpoint.go
type EndpointState int

const (
	StateCreating       EndpointState = iota // 생성 중
	StateWaitingForIdentity                  // Identity 할당 대기
	StateWaitingToRegenerate                 // BPF 재생성 대기
	StateRegenerating                        // BPF 프로그램 재생성 중
	StateDisconnecting                       // 연결 해제 중
	StateDisconnected                        // 연결 해제됨
	StateReady                               // 정상 동작 중
)

func (s EndpointState) String() string {
	names := []string{
		"creating", "waiting-for-identity", "waiting-to-regenerate",
		"regenerating", "disconnecting", "disconnected", "ready",
	}
	if int(s) < len(names) {
		return names[s]
	}
	return "unknown"
}

// Endpoint는 네트워크 엔드포인트를 표현한다.
// 실제 코드: pkg/endpoint/endpoint.go
// 각 Pod의 veth 인터페이스에 해당하며, BPF 프로그램이 연결된다.
type Endpoint struct {
	ID               uint16          // 클러스터 내 고유 ID (BPF 맵 인덱스)
	ContainerName    string          // 컨테이너 이름
	ContainerID      string          // 컨테이너 런타임 ID
	PodName          string          // K8s Pod 이름
	Namespace        string          // K8s 네임스페이스
	IPv4             net.IP          // IPv4 주소
	IPv6             net.IP          // IPv6 주소
	IfName           string          // 인터페이스 이름 (lxc...)
	IfIndex          int             // 인터페이스 인덱스
	SecurityIdentity *Identity       // 보안 Identity
	State            EndpointState   // 현재 상태
	Labels           map[string]string // Pod 레이블
	PolicyRevision   uint64          // 마지막 적용된 정책 리비전
}

func (e *Endpoint) String() string {
	idStr := "nil"
	if e.SecurityIdentity != nil {
		idStr = fmt.Sprintf("%d", e.SecurityIdentity.ID)
	}
	return fmt.Sprintf("Endpoint{ID:%d, Pod:%s/%s, IPv4:%s, Identity:%s, State:%s}",
		e.ID, e.Namespace, e.PodName, e.IPv4, idStr, e.State)
}

// --- Node ---

// Node는 클러스터 노드를 표현한다.
// 실제 코드: pkg/node/types/node.go
type Node struct {
	Name          string
	Cluster       string
	IPv4          net.IP   // 노드 내부 IP
	IPv6          net.IP   // 노드 IPv6
	IPv4HealthIP  net.IP   // 헬스 체크 IP
	IPv4InternalIP net.IP  // 내부 통신 IP
	CiliumHostIP  net.IP   // cilium_host 인터페이스 IP
	PodCIDRs      []string // 할당된 Pod CIDR 범위
	Source        string   // "k8s", "kvstore", "local"
}

func (n *Node) String() string {
	return fmt.Sprintf("Node{Name:%s, IPv4:%s, PodCIDRs:%v}", n.Name, n.IPv4, n.PodCIDRs)
}

// --- IPCache ---

// IPCacheEntry는 IP → Identity 매핑 엔트리이다.
// 실제 코드: pkg/ipcache/ipcache.go
// 이 데이터는 BPF 맵(cilium_ipcache)으로 동기화되어
// 데이터패스에서 패킷의 소스/목적지 Identity를 조회하는 데 사용된다.
type IPCacheEntry struct {
	IP       string          // CIDR 형태 (10.0.1.5/32)
	Identity NumericIdentity
	HostIP   net.IP          // 해당 IP가 위치한 노드
	Source   string          // "k8s", "kvstore", "custom-resource"
}

// IPCache는 IP → Identity 매핑 캐시이다.
// 실제 코드: pkg/ipcache/ipcache.go
type IPCache struct {
	mu      sync.RWMutex
	entries map[string]*IPCacheEntry // CIDR → Entry
}

func NewIPCache() *IPCache {
	return &IPCache{entries: make(map[string]*IPCacheEntry)}
}

// Upsert는 IPCache 엔트리를 추가/업데이트한다.
func (c *IPCache) Upsert(cidr string, identity NumericIdentity, hostIP net.IP, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[cidr] = &IPCacheEntry{
		IP:       cidr,
		Identity: identity,
		HostIP:   hostIP,
		Source:   source,
	}
}

// Lookup은 IP에 대한 Identity를 조회한다.
func (c *IPCache) Lookup(ip string) (*IPCacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// 정확한 /32 매칭 먼저 시도
	if entry, ok := c.entries[ip+"/32"]; ok {
		return entry, true
	}
	// CIDR 프리픽스 매칭 시도
	targetIP := net.ParseIP(ip)
	if targetIP == nil {
		return nil, false
	}
	var bestEntry *IPCacheEntry
	bestPrefixLen := -1
	for cidr, entry := range c.entries {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(targetIP) {
			ones, _ := network.Mask.Size()
			if ones > bestPrefixLen {
				bestPrefixLen = ones
				bestEntry = entry
			}
		}
	}
	if bestEntry != nil {
		return bestEntry, true
	}
	return nil, false
}

// Delete는 IPCache 엔트리를 삭제한다.
func (c *IPCache) Delete(cidr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, cidr)
}

// Dump는 IPCache 전체 내용을 출력한다.
func (c *IPCache) Dump() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fmt.Printf("  %-20s → %-15s %-16s %s\n", "CIDR", "Identity", "HostIP", "Source")
	fmt.Printf("  %s\n", strings.Repeat("-", 70))
	for cidr, entry := range c.entries {
		fmt.Printf("  %-20s → %-15s %-16s %s\n",
			cidr, entry.Identity, entry.HostIP, entry.Source)
	}
}

// --- IdentityAllocator ---

// IdentityAllocator는 레이블 기반 Identity 할당기이다.
// 실제 코드: pkg/identity/cache/allocator.go
// 동일한 레이블 조합은 같은 Identity ID를 받는다.
type IdentityAllocator struct {
	mu         sync.Mutex
	nextID     NumericIdentity
	identities map[NumericIdentity]*Identity
	labelIndex map[string]NumericIdentity // 레이블 해시 → ID
}

func NewIdentityAllocator() *IdentityAllocator {
	return &IdentityAllocator{
		nextID:     256, // 256부터 시작 (0-255는 예약)
		identities: make(map[NumericIdentity]*Identity),
		labelIndex: make(map[string]NumericIdentity),
	}
}

// labelsKey는 레이블 조합의 정규화된 키를 생성한다.
func labelsKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return strings.Join(parts, ";")
}

// AllocateIdentity는 레이블 기반으로 Identity를 할당한다.
// 같은 레이블 조합이면 기존 Identity를 반환한다.
func (a *IdentityAllocator) AllocateIdentity(labels map[string]string, namespace string) *Identity {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := labelsKey(labels)

	// 이미 할당된 Identity가 있으면 재사용
	if id, ok := a.labelIndex[key]; ok {
		fmt.Printf("  [allocator] 기존 Identity 재사용: %d (레이블: %s)\n", id, key)
		return a.identities[id]
	}

	// 새 Identity 할당
	id := a.nextID
	a.nextID++

	identity := &Identity{
		ID:        id,
		Labels:    labels,
		Namespace: namespace,
		Source:    "k8s",
	}

	a.identities[id] = identity
	a.labelIndex[key] = id
	fmt.Printf("  [allocator] 새 Identity 할당: %d (레이블: %s)\n", id, key)
	return identity
}

// --- EndpointManager ---

// EndpointManager는 엔드포인트의 CRUD를 관리한다.
// 실제 코드: pkg/endpoint/manager.go
type EndpointManager struct {
	mu        sync.RWMutex
	endpoints map[uint16]*Endpoint
	nextID    uint16
	allocator *IdentityAllocator
	ipcache   *IPCache
}

func NewEndpointManager(alloc *IdentityAllocator, ipc *IPCache) *EndpointManager {
	return &EndpointManager{
		endpoints: make(map[uint16]*Endpoint),
		nextID:    1001,
		allocator: alloc,
		ipcache:   ipc,
	}
}

// CreateEndpoint는 새 엔드포인트를 생성한다.
// 실제 흐름: Pod 생성 → CNI → Cilium Agent → Endpoint 생성 → Identity 할당 → BPF 재생성
func (m *EndpointManager) CreateEndpoint(podName, namespace string, labels map[string]string, ipv4 net.IP) *Endpoint {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := m.nextID
	m.nextID++

	ep := &Endpoint{
		ID:        id,
		PodName:   podName,
		Namespace: namespace,
		IPv4:      ipv4,
		IfName:    fmt.Sprintf("lxc%04x", id),
		IfIndex:   int(id) + 100,
		State:     StateCreating,
		Labels:    labels,
	}

	fmt.Printf("\n  [endpoint-manager] 엔드포인트 생성 시작: %s/%s (ID: %d)\n", namespace, podName, id)

	// 상태 전이: Creating → WaitingForIdentity
	ep.State = StateWaitingForIdentity
	fmt.Printf("  [endpoint-manager] 상태 전이: %s\n", ep.State)

	// Identity 할당
	identity := m.allocator.AllocateIdentity(labels, namespace)
	ep.SecurityIdentity = identity
	fmt.Printf("  [endpoint-manager] Identity 할당됨: %d\n", identity.ID)

	// IPCache 업데이트
	m.ipcache.Upsert(
		ipv4.String()+"/32",
		identity.ID,
		net.ParseIP("10.0.0.1"), // 노드 IP
		"k8s",
	)

	// 상태 전이: WaitingForIdentity → Regenerating → Ready
	ep.State = StateRegenerating
	fmt.Printf("  [endpoint-manager] 상태 전이: %s (BPF 프로그램 재생성 중)\n", ep.State)

	ep.State = StateReady
	ep.PolicyRevision = 1
	fmt.Printf("  [endpoint-manager] 상태 전이: %s\n", ep.State)

	m.endpoints[id] = ep
	return ep
}

// DeleteEndpoint는 엔드포인트를 삭제한다.
func (m *EndpointManager) DeleteEndpoint(id uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ep, ok := m.endpoints[id]
	if !ok {
		return fmt.Errorf("엔드포인트 %d를 찾을 수 없음", id)
	}

	fmt.Printf("\n  [endpoint-manager] 엔드포인트 삭제: %d (%s/%s)\n", id, ep.Namespace, ep.PodName)

	// 상태 전이
	ep.State = StateDisconnecting
	fmt.Printf("  [endpoint-manager] 상태 전이: %s\n", ep.State)

	// IPCache에서 제거
	m.ipcache.Delete(ep.IPv4.String() + "/32")
	fmt.Printf("  [endpoint-manager] IPCache에서 제거됨: %s\n", ep.IPv4)

	ep.State = StateDisconnected
	delete(m.endpoints, id)
	fmt.Printf("  [endpoint-manager] 엔드포인트 %d 삭제 완료\n", id)
	return nil
}

// ListEndpoints는 모든 엔드포인트를 출력한다.
func (m *EndpointManager) ListEndpoints() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	fmt.Printf("\n  %-6s %-12s %-20s %-15s %-10s %s\n",
		"ID", "Interface", "Pod", "IPv4", "Identity", "State")
	fmt.Printf("  %s\n", strings.Repeat("-", 85))
	for _, ep := range m.endpoints {
		idStr := "N/A"
		if ep.SecurityIdentity != nil {
			idStr = fmt.Sprintf("%d", ep.SecurityIdentity.ID)
		}
		fmt.Printf("  %-6d %-12s %-20s %-15s %-10s %s\n",
			ep.ID, ep.IfName, ep.Namespace+"/"+ep.PodName, ep.IPv4, idStr, ep.State)
	}
}

// --- 메인 ---

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════╗")
	fmt.Println("║  Cilium 핵심 데이터 모델 시뮬레이션                 ║")
	fmt.Println("║  Endpoint, Identity, Node, IPCache                  ║")
	fmt.Println("╚══════════════════════════════════════════════════════╝")

	// 초기화
	allocator := NewIdentityAllocator()
	ipcache := NewIPCache()
	epManager := NewEndpointManager(allocator, ipcache)

	// === 1. 노드 정보 ===
	fmt.Println("\n=== 1. 노드 정보 ===")
	node := &Node{
		Name:           "worker-1",
		Cluster:        "default",
		IPv4:           net.ParseIP("10.0.0.1"),
		IPv4HealthIP:   net.ParseIP("10.0.0.1"),
		CiliumHostIP:   net.ParseIP("10.244.0.1"),
		PodCIDRs:       []string{"10.244.0.0/24"},
		Source:         "k8s",
	}
	fmt.Println("  " + node.String())

	// === 2. 예약된 Identity ===
	fmt.Println("\n=== 2. 예약된 Identity ===")
	reservedIDs := []NumericIdentity{IdentityHost, IdentityWorld, IdentityHealth, IdentityInit}
	for _, id := range reservedIDs {
		fmt.Printf("  ID: %-3d → %s\n", id, id)
	}

	// IPCache에 예약 엔트리 추가
	ipcache.Upsert("0.0.0.0/0", IdentityWorld, nil, "reserved")
	ipcache.Upsert("10.0.0.1/32", IdentityHost, net.ParseIP("10.0.0.1"), "reserved")

	// === 3. 엔드포인트 생성 ===
	fmt.Println("\n=== 3. 엔드포인트 생성 (Pod 배포 시뮬레이션) ===")

	// 같은 레이블을 가진 Pod들은 같은 Identity를 공유한다
	frontendLabels := map[string]string{
		"k8s:app":                         "frontend",
		"k8s:io.kubernetes.pod.namespace": "default",
	}
	backendLabels := map[string]string{
		"k8s:app":                         "backend",
		"k8s:io.kubernetes.pod.namespace": "default",
	}

	ep1 := epManager.CreateEndpoint("frontend-abc", "default", frontendLabels, net.ParseIP("10.244.0.10"))
	ep2 := epManager.CreateEndpoint("frontend-def", "default", frontendLabels, net.ParseIP("10.244.0.11"))
	ep3 := epManager.CreateEndpoint("backend-xyz", "default", backendLabels, net.ParseIP("10.244.0.20"))

	_ = ep1
	_ = ep2
	_ = ep3

	// === 4. Identity 공유 확인 ===
	fmt.Println("\n=== 4. Identity 공유 확인 ===")
	fmt.Println("  같은 레이블을 가진 Pod는 동일한 Identity를 공유한다:")
	fmt.Printf("  frontend-abc Identity: %d\n", ep1.SecurityIdentity.ID)
	fmt.Printf("  frontend-def Identity: %d\n", ep2.SecurityIdentity.ID)
	fmt.Printf("  backend-xyz  Identity: %d\n", ep3.SecurityIdentity.ID)
	fmt.Printf("  frontend 공유 여부: %v (동일한 ID여야 함)\n",
		ep1.SecurityIdentity.ID == ep2.SecurityIdentity.ID)

	// === 5. 엔드포인트 목록 ===
	fmt.Println("\n=== 5. 엔드포인트 목록 ===")
	epManager.ListEndpoints()

	// === 6. IPCache 조회 ===
	fmt.Println("\n=== 6. IPCache (IP → Identity 매핑) ===")
	ipcache.Dump()

	// === 7. IPCache Lookup ===
	fmt.Println("\n=== 7. IPCache Lookup 테스트 ===")
	testIPs := []string{"10.244.0.10", "10.244.0.20", "8.8.8.8", "10.0.0.1"}
	for _, ip := range testIPs {
		entry, found := ipcache.Lookup(ip)
		if found {
			fmt.Printf("  %s → Identity %s (source: %s)\n", ip, entry.Identity, entry.Source)
		} else {
			fmt.Printf("  %s → (not found)\n", ip)
		}
	}

	// === 8. 엔드포인트 삭제 ===
	fmt.Println("\n=== 8. 엔드포인트 삭제 (Pod 삭제 시뮬레이션) ===")
	if err := epManager.DeleteEndpoint(ep1.ID); err != nil {
		fmt.Printf("  오류: %v\n", err)
	}

	// === 9. 삭제 후 상태 확인 ===
	fmt.Println("\n=== 9. 삭제 후 엔드포인트 목록 ===")
	epManager.ListEndpoints()

	fmt.Println("\n=== 10. 삭제 후 IPCache ===")
	ipcache.Dump()

	// === 10. Lookup 재확인 ===
	fmt.Println("\n=== 11. 삭제 후 IPCache Lookup ===")
	entry, found := ipcache.Lookup("10.244.0.10")
	if found {
		fmt.Printf("  10.244.0.10 → Identity %s\n", entry.Identity)
	} else {
		fmt.Printf("  10.244.0.10 → (삭제됨, 0.0.0.0/0 fallback 시도)\n")
		// /0 프리픽스로 fallback
		entry, found = ipcache.Lookup("10.244.0.10")
		if found {
			fmt.Printf("  10.244.0.10 → Identity %s (CIDR 매칭)\n", entry.Identity)
		}
	}

	fmt.Println("\n데이터 모델 시뮬레이션 완료.")
}
