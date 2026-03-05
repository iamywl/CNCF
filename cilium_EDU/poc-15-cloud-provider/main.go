package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
)

// =============================================================================
// Cilium AWS ENI IPAM 시뮬레이션
// =============================================================================
// 실제 소스: pkg/aws/eni/node.go, pkg/aws/eni/instances.go, pkg/aws/eni/limits/limits.go
//
// Cilium의 AWS ENI IPAM 모드 핵심 아키텍처:
//
// 1. LimitsGetter: EC2 API에서 인스턴스 타입별 리밋을 가져와 캐시
//    (실제: limits.LimitsGetter - Trigger 패턴으로 최소 1분 간격 갱신)
//
// 2. InstancesManager: EC2 인스턴스/ENI/서브넷/VPC/보안그룹 전체 상태를 관리
//    (실제: instances.go - Resync/InstanceSync로 주기적 동기화)
//
// 3. Node(NodeOperations): 각 K8s 노드의 ENI 할당/해제를 구현
//    (실제: node.go - PrepareIPAllocation, AllocateIPs, CreateInterface 등)
//
// 4. EC2API 인터페이스: AWS EC2 API 호출 추상화
//    (실제: instances.go - CreateNetworkInterface, AttachNetworkInterface 등)
//
// 실행: go run main.go
// =============================================================================

// ============================================================================
// Limits (실제: pkg/aws/eni/limits/limits.go, pkg/ipam/types/types.go)
// ============================================================================

// Limits는 EC2 인스턴스 타입별 네트워크 리밋이다.
// 실제 Cilium에서는 ipamTypes.Limits 구조체를 사용하며,
// EC2 DescribeInstanceTypes API로 동적으로 조회한다.
type Limits struct {
	Adapters       int    // 최대 ENI(네트워크 어댑터) 수
	IPv4           int    // ENI당 최대 IPv4 주소 수 (primary IP 포함)
	HypervisorType string // "nitro" 또는 "xen" (prefix delegation은 nitro만 지원)
}

// LimitsGetter는 인스턴스 타입별 리밋을 캐시하고 제공한다.
// 실제 구현(limits.LimitsGetter)에서는:
// - EC2 API를 Trigger 패턴으로 호출하여 최소 1분 간격 갱신
// - 캐시 미스 시 EC2 API 트리거 후 타임아웃(5초+10% jitter) 대기
// - triggerDone 채널로 동시 Get() 호출자들이 업데이트를 공유
type LimitsGetter struct {
	mu     sync.RWMutex
	limits map[string]Limits
}

func NewLimitsGetter() *LimitsGetter {
	return &LimitsGetter{
		limits: map[string]Limits{
			// AWS 공식 문서 기반 리밋 예시 (MaxENI, MaxIPv4PerENI including primary)
			"t3.micro":   {Adapters: 2, IPv4: 2, HypervisorType: "nitro"},
			"t3.small":   {Adapters: 3, IPv4: 4, HypervisorType: "nitro"},
			"t3.medium":  {Adapters: 3, IPv4: 6, HypervisorType: "nitro"},
			"m5.large":   {Adapters: 3, IPv4: 10, HypervisorType: "nitro"},
			"m5.xlarge":  {Adapters: 4, IPv4: 15, HypervisorType: "nitro"},
			"c5.2xlarge": {Adapters: 4, IPv4: 15, HypervisorType: "nitro"},
			"r5.4xlarge": {Adapters: 8, IPv4: 30, HypervisorType: "nitro"},
			"m4.large":   {Adapters: 2, IPv4: 10, HypervisorType: "xen"},
		},
	}
}

// Get은 인스턴스 타입의 리밋을 반환한다. (실제: LimitsGetter.Get)
func (l *LimitsGetter) Get(instanceType string) (Limits, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	limit, ok := l.limits[instanceType]
	return limit, ok
}

// ============================================================================
// ENI / Subnet 데이터 모델 (실제: pkg/aws/eni/types/types.go, pkg/ipam/types/)
// ============================================================================

// ENI는 AWS Elastic Network Interface를 나타낸다.
// 실제 구현: pkg/aws/eni/types/types.go - ENI struct
type ENI struct {
	ID             string   // ENI ID (eni-xxxxxxxx)
	IP             string   // Primary private IP
	Addresses      []string // Secondary IP 주소 목록
	SecurityGroups []string // 연결된 보안 그룹 ID 목록
	SubnetID       string   // 소속 서브넷 ID
	Number         int      // 인스턴스 내 인터페이스 인덱스 (0 = eth0/primary)
	VpcID          string   // 소속 VPC ID
}

// Subnet은 AWS VPC 서브넷을 나타낸다.
// 실제 구현: pkg/ipam/types/types.go - Subnet struct
type Subnet struct {
	ID                 string
	CIDR               string
	AvailableAddresses int    // 사용 가능한 IP 수
	VirtualNetworkID   string // VPC ID
	AvailabilityZone   string // 가용 영역
}

// ============================================================================
// EC2API 인터페이스 (실제: pkg/aws/eni/instances.go - EC2API interface)
// ============================================================================

// EC2API는 Cilium이 사용하는 AWS EC2 API를 추상화한다.
// 실제 인터페이스(instances.go)에는 CreateNetworkInterface,
// AttachNetworkInterface, AssignPrivateIpAddresses, DeleteNetworkInterface,
// ModifyNetworkInterface, UnassignPrivateIpAddresses 등이 포함된다.
type EC2API interface {
	CreateNetworkInterface(toAllocate int, subnetID, desc string, groups []string) (string, *ENI, error)
	AttachNetworkInterface(index int, instanceID, eniID string) (string, error)
	AssignPrivateIpAddresses(eniID string, count int) ([]string, error)
	UnassignPrivateIpAddresses(eniID string, addresses []string) error
	DeleteNetworkInterface(eniID string) error
}

// mockEC2API는 EC2 API의 시뮬레이션 구현이다.
type mockEC2API struct {
	mu        sync.Mutex
	eniSeq    int
	attachSeq int
	subnets   map[string]*Subnet
	ipCounter map[string]int // subnetID -> 다음 IP 오프셋
}

func newMockEC2API(subnets map[string]*Subnet) *mockEC2API {
	return &mockEC2API{
		subnets:   subnets,
		ipCounter: make(map[string]int),
	}
}

func (m *mockEC2API) CreateNetworkInterface(toAllocate int, subnetID, desc string, groups []string) (string, *ENI, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	subnet, ok := m.subnets[subnetID]
	if !ok {
		return "", nil, fmt.Errorf("서브넷 %s 없음", subnetID)
	}
	if subnet.AvailableAddresses < toAllocate+1 {
		return "", nil, fmt.Errorf("서브넷 %s IP 부족 (필요=%d, 가용=%d)", subnetID, toAllocate+1, subnet.AvailableAddresses)
	}

	m.eniSeq++
	eniID := fmt.Sprintf("eni-%08d", m.eniSeq)

	// IP 생성 (서브넷 CIDR 기반 시뮬레이션)
	counter := m.ipCounter[subnetID]
	primaryIP := subnetIP(subnet.CIDR, counter)
	counter++

	addresses := make([]string, toAllocate)
	for i := range addresses {
		addresses[i] = subnetIP(subnet.CIDR, counter)
		counter++
	}
	m.ipCounter[subnetID] = counter

	subnet.AvailableAddresses -= toAllocate + 1

	eni := &ENI{
		ID:             eniID,
		IP:             primaryIP,
		Addresses:      addresses,
		SecurityGroups: groups,
		SubnetID:       subnetID,
		VpcID:          subnet.VirtualNetworkID,
	}
	return eniID, eni, nil
}

func (m *mockEC2API) AttachNetworkInterface(index int, instanceID, eniID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attachSeq++
	return fmt.Sprintf("eni-attach-%08d", m.attachSeq), nil
}

func (m *mockEC2API) AssignPrivateIpAddresses(eniID string, count int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ips := make([]string, count)
	for i := range ips {
		ips[i] = fmt.Sprintf("10.0.%d.%d", rand.Intn(250)+1, rand.Intn(254)+1)
	}
	return ips, nil
}

func (m *mockEC2API) UnassignPrivateIpAddresses(eniID string, addresses []string) error {
	return nil
}

func (m *mockEC2API) DeleteNetworkInterface(eniID string) error {
	return nil
}

// subnetIP는 CIDR에서 오프셋으로 IP를 생성한다 (시뮬레이션).
func subnetIP(cidr string, offset int) string {
	parts := strings.Split(strings.Split(cidr, "/")[0], ".")
	if len(parts) < 3 {
		return fmt.Sprintf("10.0.0.%d", offset+1)
	}
	third := offset / 254
	fourth := offset%254 + 1
	return fmt.Sprintf("%s.%s.%d.%d", parts[0], parts[1], third, fourth)
}

// ============================================================================
// InstancesManager (실제: pkg/aws/eni/instances.go)
// ============================================================================

// InstancesManager는 EC2 인스턴스/ENI/서브넷 전체를 관리한다.
// 실제 구현에서는 EC2 API(GetInstances, GetSubnets, GetVpcs, GetSecurityGroups,
// GetRouteTables)를 호출하여 주기적으로 인프라 상태를 동기화한다.
//
// Resync()는 전체 동기화(resyncLock.Lock), InstanceSync()는 인스턴스 단위
// 증분 동기화(resyncLock.RLock)로, 전체 동기화가 증분 동기화를 블록한다.
type InstancesManager struct {
	mu           sync.RWMutex
	ec2api       EC2API
	limitsGetter *LimitsGetter
	subnets      map[string]*Subnet
	instances    map[string]map[string]*ENI // instanceID -> eniID -> ENI
	vpcID        string
}

func NewInstancesManager(ec2api EC2API, lg *LimitsGetter, subnets map[string]*Subnet, vpcID string) *InstancesManager {
	return &InstancesManager{
		ec2api:       ec2api,
		limitsGetter: lg,
		subnets:      subnets,
		instances:    make(map[string]map[string]*ENI),
		vpcID:        vpcID,
	}
}

// GetSubnet은 서브넷 ID로 서브넷을 반환한다. (실제: InstancesManager.GetSubnet)
func (m *InstancesManager) GetSubnet(subnetID string) *Subnet {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.subnets[subnetID]
}

// FindBestSubnet은 VPC/AZ에 맞는 서브넷 중 가용 주소가 가장 많은 것을 반환.
// 실제 구현(FindSubnetByTags, FindSubnetByIDs)에서는 태그/ID 매칭 후
// AvailableAddresses가 가장 큰 서브넷을 선택한다.
func (m *InstancesManager) FindBestSubnet(vpcID, az string) *Subnet {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *Subnet
	for _, s := range m.subnets {
		if s.VirtualNetworkID == vpcID && s.AvailabilityZone == az {
			if best == nil || s.AvailableAddresses > best.AvailableAddresses {
				best = s
			}
		}
	}
	return best
}

// UpdateENI는 인스턴스의 ENI를 업데이트한다. (실제: InstancesManager.UpdateENI)
func (m *InstancesManager) UpdateENI(instanceID string, eni *ENI) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instances[instanceID] == nil {
		m.instances[instanceID] = make(map[string]*ENI)
	}
	m.instances[instanceID][eni.ID] = eni
}

// ForeachInstance는 인스턴스의 모든 ENI에 대해 함수를 실행한다.
// 실제 구현에서는 ipamTypes.InterfaceRevision과 InterfaceIterator를 사용.
func (m *InstancesManager) ForeachInstance(instanceID string, fn func(eniID string, eni *ENI) error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if enis, ok := m.instances[instanceID]; ok {
		for id, eni := range enis {
			if err := fn(id, eni); err != nil {
				return
			}
		}
	}
}

// ============================================================================
// AllocationAction / ReleaseAction (실제: pkg/ipam/types.go)
// ============================================================================

// AllocationAction은 IP 할당 계획을 나타낸다.
type AllocationAction struct {
	InterfaceID            string // 할당 대상 ENI ID
	PoolID                 string // 서브넷 ID (= ipamTypes.PoolID)
	AvailableForAllocation int    // 할당 가능한 IP 수
	InterfaceCandidates    int    // IP 추가 가능한 ENI 수
	EmptyInterfaceSlots    int    // 새 ENI 생성 가능 슬롯 수
	MaxIPsToAllocate       int    // 이번에 할당할 최대 IP 수
}

// ReleaseAction은 IP 해제 계획을 나타낸다.
type ReleaseAction struct {
	InterfaceID  string   // 해제 대상 ENI ID
	PoolID       string   // 서브넷 ID
	IPsToRelease []string // 해제할 IP 목록
}

// ============================================================================
// Node (실제: pkg/aws/eni/node.go)
// ============================================================================

// Node는 Cilium ENI IPAM에서 하나의 K8s 노드를 나타낸다.
// 실제 구현에서 Node는 ipam.NodeOperations 인터페이스를 구현하며,
// ipamNodeActions 인터페이스를 통해 상위 IPAM Node와 통신한다.
type Node struct {
	mu               sync.RWMutex
	instanceID       string
	instanceType     string
	vpcID            string
	availabilityZone string
	securityGroups   []string
	firstIfaceIndex  int // Cilium이 관리 시작하는 인터페이스 인덱스 (보통 1)

	enis    map[string]*ENI // eniID -> ENI
	usedIPs map[string]bool // 사용 중인 IP (Pod에 할당됨)

	manager *InstancesManager
}

func NewNode(instanceID, instanceType, vpcID, az string, sgs []string, firstIdx int, mgr *InstancesManager) *Node {
	return &Node{
		instanceID:       instanceID,
		instanceType:     instanceType,
		vpcID:            vpcID,
		availabilityZone: az,
		securityGroups:   sgs,
		firstIfaceIndex:  firstIdx,
		enis:             make(map[string]*ENI),
		usedIPs:          make(map[string]bool),
		manager:          mgr,
	}
}

// getLimits는 인스턴스 타입의 리밋을 반환한다. (실제: Node.getLimits)
func (n *Node) getLimits() (Limits, bool) {
	return n.manager.limitsGetter.Get(n.instanceType)
}

// PrepareIPAllocation은 기존 ENI에서 할당 가능한 IP를 계산한다.
// 실제 구현(node.go:PrepareIPAllocation)에서는:
// - 모든 ENI를 순회하며 각 ENI의 effectiveLimits(= limits.IPv4 - 1)와
//   현재 주소 수의 차이를 계산
// - IsExcludedBySpec으로 제외된 ENI 건너뜀
// - 서브넷 가용 주소와 ENI 가용 슬롯 중 작은 값을 할당 가능량으로 설정
// - EmptyInterfaceSlots = limits.Adapters - len(enis)
func (n *Node) PrepareIPAllocation() (*AllocationAction, error) {
	limits, ok := n.getLimits()
	if !ok {
		return nil, fmt.Errorf("인스턴스 타입 %s 리밋 없음", n.instanceType)
	}

	a := &AllocationAction{}
	n.mu.RLock()
	defer n.mu.RUnlock()

	for eniID, e := range n.enis {
		if e.Number < n.firstIfaceIndex {
			continue
		}
		// effectiveLimits = limits.IPv4 - 1 (primary IP 제외)
		effectiveLimits := limits.IPv4 - 1
		availableOnENI := effectiveLimits - len(e.Addresses)
		if availableOnENI <= 0 {
			continue
		}
		a.InterfaceCandidates++

		subnet := n.manager.GetSubnet(e.SubnetID)
		if subnet != nil && subnet.AvailableAddresses > 0 && a.InterfaceID == "" {
			a.InterfaceID = eniID
			a.PoolID = e.SubnetID
			a.AvailableForAllocation = min(subnet.AvailableAddresses, availableOnENI)
		}
	}
	a.EmptyInterfaceSlots = limits.Adapters - len(n.enis)
	return a, nil
}

// AllocateIPs는 기존 ENI에 IP를 추가한다. (실제: Node.AllocateIPs)
func (n *Node) AllocateIPs(action *AllocationAction) error {
	ips, err := n.manager.ec2api.AssignPrivateIpAddresses(action.InterfaceID, action.AvailableForAllocation)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if eni, ok := n.enis[action.InterfaceID]; ok {
		eni.Addresses = append(eni.Addresses, ips...)
	}
	return nil
}

// CreateInterface는 새 ENI를 생성하고 인스턴스에 연결한다.
// 실제 구현(node.go:CreateInterface)의 전체 흐름:
// 1. getLimits()로 인스턴스 리밋 확인
// 2. findSuitableSubnet()으로 서브넷 선택
//    - SubnetIDs 직접 지정 > SubnetTags 매칭 > NodeSubnetID > 같은 라우트테이블 > 폴백
// 3. getSecurityGroupIDs()로 보안그룹 결정
//    - SecurityGroups 직접 > SecurityGroupTags > eth0 보안그룹 상속
// 4. toAllocate = min(MaxIPsToAllocate, limits.IPv4-1)
// 5. findNextIndex()로 빈 인덱스 찾기
// 6. ec2api.CreateNetworkInterface()로 ENI 생성
// 7. ec2api.AttachNetworkInterface()로 연결 (최대 maxAttachRetries=5 재시도)
// 8. ec2api.ModifyNetworkInterface()로 DeleteOnTermination 설정
// 9. 실패 시 ec2api.DeleteNetworkInterface()로 정리(cleanup)
func (n *Node) CreateInterface(allocation *AllocationAction) (int, string, error) {
	limits, ok := n.getLimits()
	if !ok {
		return 0, "리밋없음", fmt.Errorf("인스턴스 타입 %s 리밋 없음", n.instanceType)
	}

	// 1. 적절한 서브넷 찾기
	subnet := n.manager.FindBestSubnet(n.vpcID, n.availabilityZone)
	if subnet == nil {
		return 0, "서브넷없음", fmt.Errorf("적절한 서브넷 없음 (VPC=%s, AZ=%s)", n.vpcID, n.availabilityZone)
	}
	allocation.PoolID = subnet.ID

	// 2. 할당할 IP 수 계산
	toAllocate := min(allocation.MaxIPsToAllocate, limits.IPv4-1)
	if toAllocate == 0 {
		return 0, "", nil
	}

	// 3. 다음 인터페이스 인덱스 찾기 (실제: findNextIndex)
	index := n.findNextIndex(n.firstIfaceIndex)

	desc := fmt.Sprintf("Cilium-CNI (%s)", n.instanceID)

	// 4. ENI 생성 (실제: ec2api.CreateNetworkInterface)
	eniID, eni, err := n.manager.ec2api.CreateNetworkInterface(toAllocate, subnet.ID, desc, n.securityGroups)
	if err != nil {
		return 0, "ENI생성실패", fmt.Errorf("ENI 생성 실패: %w", err)
	}

	// 5. ENI 연결 (실제: 최대 maxAttachRetries=5회 재시도, isAttachmentIndexConflict 확인)
	const maxAttachRetries = 5
	var attachmentID string
	for i := 0; i < maxAttachRetries; i++ {
		attachmentID, err = n.manager.ec2api.AttachNetworkInterface(index, n.instanceID, eniID)
		if err == nil {
			break
		}
		// 인덱스 충돌 시 다음 인덱스 시도
		index = n.findNextIndex(index + 1)
	}
	if err != nil {
		// 연결 실패 시 생성한 ENI 삭제 (cleanup)
		_ = n.manager.ec2api.DeleteNetworkInterface(eniID)
		return 0, "ENI연결실패", fmt.Errorf("ENI 연결 실패: %w", err)
	}

	eni.Number = index

	// 6. 매니저와 노드에 ENI 등록
	n.mu.Lock()
	n.enis[eniID] = eni
	n.mu.Unlock()
	n.manager.UpdateENI(n.instanceID, eni)

	fmt.Printf("    ENI 생성: %s (index=%d, attach=%s, IPs=%d개)\n",
		eniID, index, attachmentID, len(eni.Addresses))

	return toAllocate, "", nil
}

// findNextIndex는 빈 인터페이스 인덱스를 찾는다. (실제: Node.findNextIndex)
// 기존 ENI의 Number를 확인하여 충돌을 피한다.
func (n *Node) findNextIndex(start int) int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	idx := start
	for {
		exists := false
		for _, e := range n.enis {
			if e.Number == idx {
				exists = true
				break
			}
		}
		if !exists {
			return idx
		}
		idx++
	}
}

// PrepareIPRelease는 초과 IP를 해제할 ENI를 선택한다.
// 실제 구현(node.go:PrepareIPRelease)에서는:
// - 정렬된 ENI ID를 순회 (slices.Sorted(maps.Keys(n.enis)))
// - IsExcludedBySpec으로 제외된 ENI 건너뜀
// - prefix delegation 모드에서는 /28 접두사 단위 해제
// - secondary IP 모드에서는 사용되지 않는 IP가 가장 많은 ENI 선택
func (n *Node) PrepareIPRelease(excessIPs int) *ReleaseAction {
	r := &ReleaseAction{}
	n.mu.RLock()
	defer n.mu.RUnlock()

	// 정렬된 ENI ID 순서로 순회 (실제: slices.Sorted(maps.Keys(n.enis)))
	eniIDs := make([]string, 0, len(n.enis))
	for id := range n.enis {
		eniIDs = append(eniIDs, id)
	}
	sort.Strings(eniIDs)

	for _, eniID := range eniIDs {
		e := n.enis[eniID]
		if e.Number < n.firstIfaceIndex {
			continue
		}
		// 사용되지 않는 IP 수집 (실제: getUnusedIPs)
		var freeIPs []string
		for _, ip := range e.Addresses {
			if !n.usedIPs[ip] && ip != e.IP {
				freeIPs = append(freeIPs, ip)
			}
		}
		if len(freeIPs) == 0 {
			continue
		}
		maxRelease := min(len(freeIPs), excessIPs)
		// 미사용 IP가 더 많은 ENI를 선택 (실제: firstENIWithFreeIPFound || eniWithMoreFreeIPsFound)
		if r.IPsToRelease == nil || maxRelease > len(r.IPsToRelease) {
			r.InterfaceID = eniID
			r.PoolID = e.SubnetID
			r.IPsToRelease = freeIPs[:maxRelease]
		}
	}
	return r
}

// ReleaseIPs는 EC2 API로 IP를 해제한다. (실제: Node.ReleaseIPs)
func (n *Node) ReleaseIPs(action *ReleaseAction) error {
	if len(action.IPsToRelease) == 0 {
		return nil
	}
	return n.manager.ec2api.UnassignPrivateIpAddresses(action.InterfaceID, action.IPsToRelease)
}

// GetMaximumAllocatableIPv4는 이 인스턴스의 최대 IPv4 할당량을 반환한다.
// 실제 구현(node.go:GetMaximumAllocatableIPv4):
// maxPerInterface = max(limits.IPv4 - 1, 0)
// return (limits.Adapters - firstInterfaceIndex) * maxPerInterface
func (n *Node) GetMaximumAllocatableIPv4() int {
	limits, ok := n.getLimits()
	if !ok {
		return 0
	}
	maxPerIface := limits.IPv4 - 1
	if maxPerIface <= 0 {
		return 0
	}
	return (limits.Adapters - n.firstIfaceIndex) * maxPerIface
}

// ResyncInterfacesAndIPs는 로컬 ENI 캐시를 동기화하고 가용 IP를 반환한다.
// 실제 구현(node.go:ResyncInterfacesAndIPs):
// - InstancesManager.ForeachInstance로 모든 ENI 순회
// - effectiveIPLimits 계산하여 NodeCapacity 산출
// - 제외된 ENI는 NodeCapacity에서 차감
func (n *Node) ResyncInterfacesAndIPs() (map[string]string, int, error) {
	limits, ok := n.getLimits()
	if !ok {
		return nil, 0, fmt.Errorf("리밋 없음")
	}

	available := make(map[string]string) // ip -> eniID
	n.mu.Lock()
	n.enis = make(map[string]*ENI)

	effectiveLimits := limits.IPv4 - 1
	nodeCapacity := effectiveLimits * limits.Adapters

	n.manager.ForeachInstance(n.instanceID, func(eniID string, eni *ENI) error {
		n.enis[eniID] = eni
		for _, ip := range eni.Addresses {
			available[ip] = eniID
		}
		return nil
	})
	eniCount := len(n.enis)
	n.mu.Unlock()

	if eniCount == 0 {
		return nil, 0, fmt.Errorf("ENI를 찾을 수 없음")
	}

	return available, nodeCapacity, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ============================================================================
// 시뮬레이션 실행
// ============================================================================

func main() {
	fmt.Println("=== Cilium AWS ENI IPAM 시뮬레이션 ===")
	fmt.Println("소스: pkg/aws/eni/node.go, instances.go, limits/limits.go")
	fmt.Println()

	// ─── 1. 인프라 설정 ─────────────────────────────────────────
	fmt.Println("[1] 인프라 설정 (서브넷, EC2 API, LimitsGetter)")

	subnets := map[string]*Subnet{
		"subnet-001": {ID: "subnet-001", CIDR: "10.0.1.0/24", AvailableAddresses: 250, VirtualNetworkID: "vpc-001", AvailabilityZone: "us-west-2a"},
		"subnet-002": {ID: "subnet-002", CIDR: "10.0.2.0/24", AvailableAddresses: 200, VirtualNetworkID: "vpc-001", AvailabilityZone: "us-west-2a"},
		"subnet-003": {ID: "subnet-003", CIDR: "10.0.3.0/24", AvailableAddresses: 100, VirtualNetworkID: "vpc-001", AvailabilityZone: "us-west-2b"},
	}
	for id, s := range subnets {
		fmt.Printf("  서브넷 %s: CIDR=%s, AZ=%s, 가용=%d\n", id, s.CIDR, s.AvailabilityZone, s.AvailableAddresses)
	}

	ec2api := newMockEC2API(subnets)
	lg := NewLimitsGetter()
	mgr := NewInstancesManager(ec2api, lg, subnets, "vpc-001")

	// ─── 2. 노드 생성 & 리밋 확인 ──────────────────────────────
	fmt.Println()
	fmt.Println("[2] 노드 생성 및 인스턴스 타입별 리밋")

	type nodeInfo struct {
		id   string
		typ  string
		az   string
		node *Node
	}

	nodeList := []nodeInfo{
		{id: "i-node001", typ: "m5.large", az: "us-west-2a"},
		{id: "i-node002", typ: "t3.small", az: "us-west-2a"},
		{id: "i-node003", typ: "r5.4xlarge", az: "us-west-2a"},
	}

	fmt.Printf("  %-12s %-12s %6s %8s %10s\n", "인스턴스", "타입", "MaxENI", "IPv4/ENI", "최대할당가능")
	fmt.Println("  " + strings.Repeat("-", 55))

	for i := range nodeList {
		n := &nodeList[i]
		n.node = NewNode(n.id, n.typ, "vpc-001", n.az, []string{"sg-001", "sg-002"}, 1, mgr)

		limits, _ := lg.Get(n.typ)
		maxIPs := n.node.GetMaximumAllocatableIPv4()
		fmt.Printf("  %-12s %-12s %6d %8d %10d\n", n.id, n.typ, limits.Adapters, limits.IPv4, maxIPs)
	}

	// ─── 3. Primary ENI (eth0) 설정 ─────────────────────────────
	fmt.Println()
	fmt.Println("[3] Primary ENI 설정 (eth0 = index 0, Cilium은 firstIfaceIndex=1부터 관리)")

	for i := range nodeList {
		n := &nodeList[i]
		primary := &ENI{
			ID:             fmt.Sprintf("eni-primary-%03d", i+1),
			IP:             fmt.Sprintf("10.0.1.%d", i+10),
			Number:         0, // eth0
			SubnetID:       "subnet-001",
			VpcID:          "vpc-001",
			SecurityGroups: []string{"sg-001"},
		}
		n.node.mu.Lock()
		n.node.enis[primary.ID] = primary
		n.node.mu.Unlock()
		mgr.UpdateENI(n.id, primary)
		fmt.Printf("  %s: eth0=%s, primary_ip=%s\n", n.id, primary.ID, primary.IP)
	}

	// ─── 4. ENI 생성 & IP 할당 ──────────────────────────────────
	fmt.Println()
	fmt.Println("[4] ENI 생성 및 IP 할당 (PrepareIPAllocation -> CreateInterface)")

	for i := range nodeList {
		n := &nodeList[i]
		fmt.Printf("\n  --- %s (%s) ---\n", n.id, n.typ)

		action, err := n.node.PrepareIPAllocation()
		if err != nil {
			fmt.Printf("    PrepareIPAllocation 실패: %v\n", err)
			continue
		}
		fmt.Printf("    PrepareIPAllocation: 기존ENI할당가능=%d, 후보ENI=%d, 빈슬롯=%d\n",
			action.AvailableForAllocation, action.InterfaceCandidates, action.EmptyInterfaceSlots)

		if action.EmptyInterfaceSlots > 0 {
			limits, _ := lg.Get(n.typ)
			action.MaxIPsToAllocate = limits.IPv4 - 1

			allocated, errStr, err := n.node.CreateInterface(action)
			if err != nil {
				fmt.Printf("    CreateInterface 실패 (%s): %v\n", errStr, err)
			} else {
				fmt.Printf("    CreateInterface 완료: %d개 IP 할당\n", allocated)
			}
		}
	}

	// ─── 5. 할당 현황 ──────────────────────────────────────────
	fmt.Println()
	fmt.Println("[5] 노드별 ENI/IP 할당 현황")

	for i := range nodeList {
		n := &nodeList[i]
		fmt.Printf("\n  --- %s (%s) ---\n", n.id, n.typ)
		n.node.mu.RLock()
		totalIPs := 0
		for eniID, eni := range n.node.enis {
			fmt.Printf("    %s (index=%d, subnet=%s): primary=%s, secondary=%d개\n",
				eniID, eni.Number, eni.SubnetID, eni.IP, len(eni.Addresses))
			if len(eni.Addresses) > 0 {
				show := eni.Addresses
				if len(show) > 3 {
					show = show[:3]
				}
				fmt.Printf("      IPs: [%s", strings.Join(show, ", "))
				if len(eni.Addresses) > 3 {
					fmt.Printf(", ... +%d", len(eni.Addresses)-3)
				}
				fmt.Println("]")
			}
			totalIPs += len(eni.Addresses)
		}
		maxIPs := n.node.GetMaximumAllocatableIPv4()
		fmt.Printf("    총 secondary IPs: %d / 최대 %d\n", totalIPs, maxIPs)
		n.node.mu.RUnlock()
	}

	// ─── 6. IP 해제 시뮬레이션 ──────────────────────────────────
	fmt.Println()
	fmt.Println("[6] IP 해제 (PrepareIPRelease -> ReleaseIPs)")

	node0 := nodeList[0].node
	release := node0.PrepareIPRelease(3)
	if len(release.IPsToRelease) > 0 {
		fmt.Printf("  %s: ENI=%s에서 %d개 IP 해제 대상\n",
			nodeList[0].id, release.InterfaceID, len(release.IPsToRelease))
		fmt.Printf("    해제 IPs: %v\n", release.IPsToRelease)
		if err := node0.ReleaseIPs(release); err != nil {
			fmt.Printf("    해제 실패: %v\n", err)
		} else {
			fmt.Println("    해제 완료 (ec2api.UnassignPrivateIpAddresses 호출)")
		}
	} else {
		fmt.Println("  해제할 미사용 IP 없음")
	}

	// ─── 7. 서브넷 가용 상태 ────────────────────────────────────
	fmt.Println()
	fmt.Println("[7] 서브넷 가용 주소 최종 상태")
	for id, s := range subnets {
		fmt.Printf("  %s (CIDR=%s, AZ=%s): 가용 IP=%d\n", id, s.CIDR, s.AvailabilityZone, s.AvailableAddresses)
	}

	// ─── 8. ResyncInterfacesAndIPs ──────────────────────────────
	fmt.Println()
	fmt.Println("[8] ResyncInterfacesAndIPs (EC2 API 동기화 시뮬레이션)")

	node0avail, nodeCap, err := node0.ResyncInterfacesAndIPs()
	if err != nil {
		fmt.Printf("  동기화 실패: %v\n", err)
	} else {
		fmt.Printf("  %s: 가용IP=%d개, 노드용량=%d\n",
			nodeList[0].id, len(node0avail), nodeCap)
	}

	// ─── 구조 요약 ─────────────────────────────────────────────
	fmt.Println()
	fmt.Println("=== ENI IPAM 아키텍처 요약 ===")
	fmt.Println()
	fmt.Println("  InstancesManager (싱글턴)")
	fmt.Println("  ├── Resync():      EC2 API 전체 동기화 (VPC/서브넷/인스턴스/보안그룹)")
	fmt.Println("  ├── InstanceSync(): 인스턴스 단위 증분 동기화")
	fmt.Println("  ├── GetSubnet():    서브넷 조회")
	fmt.Println("  └── FindSubnetByTags/IDs(): 서브넷 선택 (가용 주소 최대)")
	fmt.Println()
	fmt.Println("  Node (NodeOperations 구현)")
	fmt.Println("  ├── PrepareIPAllocation(): 기존 ENI 가용 IP 계산")
	fmt.Println("  ├── AllocateIPs():         기존 ENI에 IP 추가")
	fmt.Println("  ├── CreateInterface():     새 ENI 생성 + 연결 + IP 할당")
	fmt.Println("  │   └── findSuitableSubnet() → getSecurityGroupIDs()")
	fmt.Println("  │       → CreateNetworkInterface() → AttachNetworkInterface()")
	fmt.Println("  ├── PrepareIPRelease():    초과 IP 해제 대상 선택")
	fmt.Println("  ├── ReleaseIPs():          IP 해제")
	fmt.Println("  └── GetMaximumAllocatableIPv4(): 최대 할당 가능 IP 계산")
	fmt.Println()
	fmt.Println("  핵심 제약: limits.Adapters (최대 ENI), limits.IPv4 (ENI당 최대 IP)")
	fmt.Println("  firstInterfaceIndex (보통 1): 0번 ENI(eth0)는 노드용, Cilium은 1번부터 관리")
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
