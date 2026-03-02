// Cilium IPAM (IP Address Management) PoC
//
// 이 프로그램은 Cilium의 IPAM 서브시스템 핵심 메커니즘을 시뮬레이션합니다.
// 실제 Cilium 코드 구조를 참고하되, 순수 Go 표준 라이브러리만 사용합니다.
//
// 시뮬레이션 내용:
//   1. Cluster-Pool IPAM: CIDR 블록에서 IP 할당/해제
//   2. Multi-Pool IPAM: 여러 풀에서 annotation 기반 할당
//   3. ENI-Style IPAM: 인터페이스 추가 → 보조 IP 할당
//   4. Pre-allocation: 워터마크 기반 자동 보충
//   5. IP 고갈 및 복구
//   6. 듀얼스택 (IPv4 + IPv6) 동시 할당
//
// 참조 소스:
//   - pkg/ipam/ipam.go          (IPAM 초기화, ConfigureAllocator)
//   - pkg/ipam/allocator.go     (AllocateNext, ReleaseIP)
//   - pkg/ipam/types.go         (Allocator 인터페이스, AllocationResult)
//   - pkg/ipam/hostscope.go     (hostScopeAllocator)
//   - pkg/ipam/pool.go          (cidrPool)
//   - pkg/ipam/multipool_manager.go (multiPoolManager, neededIPCeil)
//   - pkg/ipam/node.go          (calculateNeededIPs, calculateExcessIPs)
//   - pkg/ipam/crd.go           (crdAllocator, nodeStore)
//   - pkg/ipam/node_manager.go  (NodeManager, Resync)

package main

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
)

// =============================================================================
// 1. 공통 타입 정의
//    참조: pkg/ipam/types.go, pkg/ipam/ipam.go
// =============================================================================

// Family는 주소 패밀리 (IPv4 또는 IPv6)
type Family string

const (
	IPv4 Family = "ipv4"
	IPv6 Family = "ipv6"
)

// Pool은 IP 풀 이름
type Pool string

const DefaultPool Pool = "default"

// DeriveFamily는 IP 주소에서 패밀리를 유도한다.
// 참조: pkg/ipam/ipam.go - DeriveFamily()
func DeriveFamily(ip net.IP) Family {
	if ip.To4() == nil {
		return IPv6
	}
	return IPv4
}

// AllocationResult는 IP 할당 결과를 나타낸다.
// 참조: pkg/ipam/types.go - AllocationResult
type AllocationResult struct {
	IP             net.IP
	IPPoolName     Pool
	CIDRs          []string // VPC 라우팅 가능 CIDR (ENI 모드)
	PrimaryMAC     string   // 주 인터페이스 MAC (ENI 모드)
	GatewayIP      string   // 게이트웨이 (ENI 모드)
	InterfaceNumber string  // 인터페이스 번호 (ENI 모드)
	SkipMasquerade bool     // 마스커레이드 건너뛰기 (multi-pool)
}

// Allocator 인터페이스 - 모든 IPAM 모드가 구현해야 하는 인터페이스
// 참조: pkg/ipam/types.go - Allocator
type Allocator interface {
	Allocate(ip net.IP, owner string, pool Pool) (*AllocationResult, error)
	Release(ip net.IP, pool Pool) error
	AllocateNext(owner string, pool Pool) (*AllocationResult, error)
	Dump() (map[Pool]map[string]string, string)
	Capacity() uint64
}

// =============================================================================
// 2. CIDR 유틸리티
//    참조: pkg/cidr/cidr.go, pkg/ipam/service/ipallocator/
// =============================================================================

// CIDRAllocator는 단일 CIDR 블록에서 IP를 할당하는 비트맵 기반 할당기이다.
// 참조: pkg/ipam/service/ipallocator/ (ipallocator.Range)
type CIDRAllocator struct {
	cidr      *net.IPNet
	allocated map[string]string // IP -> owner
	baseIP    net.IP
	size      int
	mu        sync.Mutex
}

// NewCIDRAllocator는 CIDR 문자열에서 할당기를 생성한다.
func NewCIDRAllocator(cidrStr string) (*CIDRAllocator, error) {
	_, ipNet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return nil, fmt.Errorf("CIDR 파싱 실패: %w", err)
	}

	ones, bits := ipNet.Mask.Size()
	size := 1 << uint(bits-ones)
	// 네트워크 주소와 브로드캐스트 주소 제외
	if size > 2 {
		size -= 2
	}

	return &CIDRAllocator{
		cidr:      ipNet,
		allocated: make(map[string]string),
		baseIP:    ipNet.IP,
		size:      size,
	}, nil
}

// ipAtIndex는 baseIP에서 index만큼 떨어진 IP를 반환한다.
func ipAtIndex(base net.IP, index int) net.IP {
	ip := make(net.IP, len(base))
	copy(ip, base)

	bigIP := new(big.Int)
	if len(base) == net.IPv4len || base.To4() != nil {
		bigIP.SetBytes(base.To4())
	} else {
		bigIP.SetBytes(base.To16())
	}
	bigIP.Add(bigIP, big.NewInt(int64(index+1))) // +1 to skip network address

	b := bigIP.Bytes()
	if len(base.To4()) == net.IPv4len || base.To4() != nil {
		result := make(net.IP, 4)
		// 왼쪽에 0 패딩
		offset := 4 - len(b)
		for i := 0; i < len(b) && offset+i < 4; i++ {
			result[offset+i] = b[i]
		}
		return result
	}
	result := make(net.IP, 16)
	offset := 16 - len(b)
	for i := 0; i < len(b) && offset+i < 16; i++ {
		result[offset+i] = b[i]
	}
	return result
}

// Allocate는 특정 IP를 할당한다.
func (c *CIDRAllocator) Allocate(ip net.IP, owner string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ipStr := ip.String()
	if !c.cidr.Contains(ip) {
		return fmt.Errorf("IP %s가 CIDR %s 범위 밖", ipStr, c.cidr.String())
	}
	if _, ok := c.allocated[ipStr]; ok {
		return fmt.Errorf("IP %s 이미 할당됨", ipStr)
	}
	c.allocated[ipStr] = owner
	return nil
}

// AllocateNext는 다음 사용 가능한 IP를 할당한다.
func (c *CIDRAllocator) AllocateNext(owner string) (net.IP, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := 0; i < c.size; i++ {
		ip := ipAtIndex(c.baseIP, i)
		ipStr := ip.String()
		if _, ok := c.allocated[ipStr]; !ok {
			c.allocated[ipStr] = owner
			return ip, nil
		}
	}
	return nil, fmt.Errorf("CIDR %s 범위의 모든 IP가 소진됨", c.cidr.String())
}

// Release는 IP를 해제한다.
func (c *CIDRAllocator) Release(ip net.IP) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.allocated, ip.String())
}

// Free는 사용 가능한 IP 수를 반환한다.
func (c *CIDRAllocator) Free() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size - len(c.allocated)
}

// Used는 사용 중인 IP 수를 반환한다.
func (c *CIDRAllocator) Used() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.allocated)
}

// Size는 총 할당 가능한 IP 수를 반환한다.
func (c *CIDRAllocator) Size() int {
	return c.size
}

// CIDR은 해당 할당기의 CIDR을 반환한다.
func (c *CIDRAllocator) CIDR() *net.IPNet {
	return c.cidr
}

// Contains는 IP가 CIDR 내에 있는지 확인한다.
func (c *CIDRAllocator) Contains(ip net.IP) bool {
	return c.cidr.Contains(ip)
}

// =============================================================================
// 3. Cluster-Pool IPAM (hostScopeAllocator)
//    참조: pkg/ipam/hostscope.go
// =============================================================================

// HostScopeAllocator는 단일 CIDR에서 IP를 할당한다.
// Cilium의 cluster-pool 및 kubernetes 모드에서 사용된다.
type HostScopeAllocator struct {
	cidrAlloc *CIDRAllocator
}

func NewHostScopeAllocator(cidrStr string) (*HostScopeAllocator, error) {
	alloc, err := NewCIDRAllocator(cidrStr)
	if err != nil {
		return nil, err
	}
	return &HostScopeAllocator{cidrAlloc: alloc}, nil
}

func (h *HostScopeAllocator) Allocate(ip net.IP, owner string, pool Pool) (*AllocationResult, error) {
	if err := h.cidrAlloc.Allocate(ip, owner); err != nil {
		return nil, err
	}
	return &AllocationResult{IP: ip, IPPoolName: DefaultPool}, nil
}

func (h *HostScopeAllocator) Release(ip net.IP, pool Pool) error {
	h.cidrAlloc.Release(ip)
	return nil
}

func (h *HostScopeAllocator) AllocateNext(owner string, pool Pool) (*AllocationResult, error) {
	ip, err := h.cidrAlloc.AllocateNext(owner)
	if err != nil {
		return nil, err
	}
	return &AllocationResult{IP: ip, IPPoolName: DefaultPool}, nil
}

func (h *HostScopeAllocator) Dump() (map[Pool]map[string]string, string) {
	h.cidrAlloc.mu.Lock()
	defer h.cidrAlloc.mu.Unlock()

	alloc := make(map[string]string)
	for ip, owner := range h.cidrAlloc.allocated {
		alloc[ip] = owner
	}
	status := fmt.Sprintf("%d/%d 할당됨 from %s",
		len(alloc), h.cidrAlloc.size, h.cidrAlloc.cidr.String())
	return map[Pool]map[string]string{DefaultPool: alloc}, status
}

func (h *HostScopeAllocator) Capacity() uint64 {
	return uint64(h.cidrAlloc.Size())
}

// =============================================================================
// 4. Multi-Pool IPAM
//    참조: pkg/ipam/multipool.go, pkg/ipam/multipool_manager.go, pkg/ipam/pool.go
// =============================================================================

// CIDRPool은 다중 CIDR을 관리하는 풀이다.
// 참조: pkg/ipam/pool.go - cidrPool
type CIDRPool struct {
	mu           sync.Mutex
	allocators   []*CIDRAllocator
	released     map[string]struct{}
	removed      map[string]struct{}
}

func NewCIDRPool() *CIDRPool {
	return &CIDRPool{
		released: make(map[string]struct{}),
		removed:  make(map[string]struct{}),
	}
}

// AddCIDR은 풀에 CIDR을 추가한다.
func (p *CIDRPool) AddCIDR(cidrStr string) error {
	alloc, err := NewCIDRAllocator(cidrStr)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allocators = append(p.allocators, alloc)
	return nil
}

// AllocateNext는 풀에서 다음 IP를 할당한다.
// 참조: pkg/ipam/pool.go - cidrPool.allocateNext()
func (p *CIDRPool) AllocateNext(owner string) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, alloc := range p.allocators {
		cidrStr := alloc.CIDR().String()
		if _, removed := p.removed[cidrStr]; removed {
			continue
		}
		if alloc.Free() == 0 {
			continue
		}
		return alloc.AllocateNext(owner)
	}
	return nil, fmt.Errorf("모든 CIDR 범위가 소진됨")
}

// Allocate는 특정 IP를 할당한다.
func (p *CIDRPool) Allocate(ip net.IP, owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, alloc := range p.allocators {
		if alloc.Contains(ip) {
			return alloc.Allocate(ip, owner)
		}
	}
	return fmt.Errorf("IP %s가 어떤 CIDR 범위에도 속하지 않음", ip.String())
}

// Release는 IP를 해제한다.
func (p *CIDRPool) Release(ip net.IP) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, alloc := range p.allocators {
		if alloc.Contains(ip) {
			alloc.Release(ip)
			return
		}
	}
}

// InUseCount는 사용 중인 IP 수를 반환한다.
func (p *CIDRPool) InUseCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, alloc := range p.allocators {
		count += alloc.Used()
	}
	return count
}

// FreeCount는 사용 가능한 IP 수를 반환한다.
func (p *CIDRPool) FreeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, alloc := range p.allocators {
		cidrStr := alloc.CIDR().String()
		if _, removed := p.removed[cidrStr]; !removed {
			count += alloc.Free()
		}
	}
	return count
}

// PoolPair는 IPv4/IPv6 CIDR 풀 쌍이다.
// 참조: pkg/ipam/multipool_manager.go - poolPair
type PoolPair struct {
	V4 *CIDRPool
	V6 *CIDRPool
}

// MultiPoolManager는 여러 IP 풀을 관리한다.
// 참조: pkg/ipam/multipool_manager.go - multiPoolManager
type MultiPoolManager struct {
	mu                 sync.Mutex
	pools              map[Pool]*PoolPair
	preallocPerPool    map[Pool]int
	ipv4Enabled        bool
	ipv6Enabled        bool
}

func NewMultiPoolManager(ipv4, ipv6 bool) *MultiPoolManager {
	return &MultiPoolManager{
		pools:           make(map[Pool]*PoolPair),
		preallocPerPool: make(map[Pool]int),
		ipv4Enabled:     ipv4,
		ipv6Enabled:     ipv6,
	}
}

// CreatePool은 새 IP 풀을 생성한다.
func (m *MultiPoolManager) CreatePool(name Pool, v4CIDRs []string, v6CIDRs []string, prealloc int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pair := &PoolPair{}
	if m.ipv4Enabled {
		pair.V4 = NewCIDRPool()
		for _, cidr := range v4CIDRs {
			if err := pair.V4.AddCIDR(cidr); err != nil {
				return fmt.Errorf("IPv4 CIDR 추가 실패: %w", err)
			}
		}
	}
	if m.ipv6Enabled {
		pair.V6 = NewCIDRPool()
		for _, cidr := range v6CIDRs {
			if err := pair.V6.AddCIDR(cidr); err != nil {
				return fmt.Errorf("IPv6 CIDR 추가 실패: %w", err)
			}
		}
	}

	m.pools[name] = pair
	m.preallocPerPool[name] = prealloc
	return nil
}

// AllocateNext는 지정된 풀과 패밀리에서 다음 IP를 할당한다.
func (m *MultiPoolManager) AllocateNext(owner string, poolName Pool, family Family) (*AllocationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pair, ok := m.pools[poolName]
	if !ok {
		return nil, fmt.Errorf("풀 %q을 찾을 수 없음", poolName)
	}

	var pool *CIDRPool
	switch family {
	case IPv4:
		pool = pair.V4
	case IPv6:
		pool = pair.V6
	}
	if pool == nil {
		return nil, fmt.Errorf("풀 %q에 %s 할당기 없음", poolName, family)
	}

	ip, err := pool.AllocateNext(owner)
	if err != nil {
		return nil, fmt.Errorf("풀 %q에서 %s 할당 실패: %w", poolName, family, err)
	}

	return &AllocationResult{
		IP:         ip,
		IPPoolName: poolName,
	}, nil
}

// Release는 IP를 해제한다.
func (m *MultiPoolManager) Release(ip net.IP, poolName Pool, family Family) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pair, ok := m.pools[poolName]
	if !ok {
		return fmt.Errorf("풀 %q을 찾을 수 없음", poolName)
	}

	var pool *CIDRPool
	switch family {
	case IPv4:
		pool = pair.V4
	case IPv6:
		pool = pair.V6
	}
	if pool == nil {
		return fmt.Errorf("풀 %q에 %s 할당기 없음", poolName, family)
	}

	pool.Release(ip)
	return nil
}

// neededIPCeil은 필요한 IP 수를 preAlloc 단위로 올림한다.
// 참조: pkg/ipam/multipool_manager.go - neededIPCeil()
//
//	numIP  0, preAlloc=16 -> 16
//	numIP  1, preAlloc=16 -> 32
//	numIP 16, preAlloc=16 -> 32
//	numIP 17, preAlloc=16 -> 48
func neededIPCeil(numIP int, preAlloc int) int {
	if preAlloc == 0 {
		return numIP
	}
	quotient := numIP / preAlloc
	rem := numIP % preAlloc
	if rem > 0 {
		return (quotient + 2) * preAlloc
	}
	return (quotient + 1) * preAlloc
}

// ComputeNeededIPsPerPool은 풀별 필요 IP 수를 계산한다.
// 참조: pkg/ipam/multipool_manager.go - computeNeededIPsPerPoolLocked()
func (m *MultiPoolManager) ComputeNeededIPsPerPool() map[Pool]map[Family]int {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[Pool]map[Family]int)
	for poolName, pair := range m.pools {
		demand := make(map[Family]int)
		prealloc := m.preallocPerPool[poolName]

		if pair.V4 != nil && m.ipv4Enabled {
			inUse := pair.V4.InUseCount()
			demand[IPv4] = neededIPCeil(inUse, prealloc)
		}
		if pair.V6 != nil && m.ipv6Enabled {
			inUse := pair.V6.InUseCount()
			demand[IPv6] = neededIPCeil(inUse, prealloc)
		}
		result[poolName] = demand
	}
	return result
}

// GetPoolStatus는 풀의 상태를 반환한다.
func (m *MultiPoolManager) GetPoolStatus(poolName Pool) (v4InUse, v4Free, v6InUse, v6Free int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pair, ok := m.pools[poolName]
	if !ok {
		return
	}
	if pair.V4 != nil {
		v4InUse = pair.V4.InUseCount()
		v4Free = pair.V4.FreeCount()
	}
	if pair.V6 != nil {
		v6InUse = pair.V6.InUseCount()
		v6Free = pair.V6.FreeCount()
	}
	return
}

// =============================================================================
// 5. ENI-Style IPAM (인터페이스 기반)
//    참조: pkg/ipam/crd.go, pkg/ipam/node_manager.go, pkg/ipam/node.go
// =============================================================================

// ENIInterface는 Elastic Network Interface를 시뮬레이션한다.
type ENIInterface struct {
	ID           string
	SubnetCIDR   string
	VPCCIDR      string
	MAC          string
	SecondaryIPs []string
	MaxIPs       int
}

// ENINode는 ENI 모드에서의 노드를 시뮬레이션한다.
// 참조: pkg/ipam/node.go - Node, ipAllocAttrs
type ENINode struct {
	mu                  sync.Mutex
	Name                string
	Interfaces          []*ENIInterface
	MaxInterfaces       int
	MaxIPsPerInterface  int
	UsedIPs             map[string]string // IP -> owner
	PreAllocate         int
	MinAllocate         int
	MaxAllocate         int
	MaxAboveWatermark   int
}

func NewENINode(name string, maxIfaces, maxIPsPerIface, preAlloc, minAlloc, maxAboveWatermark int) *ENINode {
	return &ENINode{
		Name:               name,
		MaxInterfaces:      maxIfaces,
		MaxIPsPerInterface: maxIPsPerIface,
		UsedIPs:            make(map[string]string),
		PreAllocate:        preAlloc,
		MinAllocate:        minAlloc,
		MaxAboveWatermark:  maxAboveWatermark,
	}
}

// AvailableIPs는 할당 가능한 총 보조 IP 수를 반환한다.
func (n *ENINode) AvailableIPs() int {
	count := 0
	for _, iface := range n.Interfaces {
		count += len(iface.SecondaryIPs)
	}
	return count
}

// UsedIPCount는 사용 중인 IP 수를 반환한다.
func (n *ENINode) UsedIPCount() int {
	return len(n.UsedIPs)
}

// calculateNeededIPs는 필요한 IP 수를 계산한다.
// 참조: pkg/ipam/node.go - calculateNeededIPs()
func calculateNeededIPs(availableIPs, usedIPs, preAllocate, minAllocate, maxAllocate int) int {
	neededIPs := preAllocate - (availableIPs - usedIPs)

	if minAllocate > 0 {
		if minAllocate-availableIPs > neededIPs {
			neededIPs = minAllocate - availableIPs
		}
	}

	if maxAllocate > 0 && (availableIPs+neededIPs) > maxAllocate {
		neededIPs = maxAllocate - availableIPs
	}

	if neededIPs < 0 {
		neededIPs = 0
	}
	return neededIPs
}

// calculateExcessIPs는 초과 IP 수를 계산한다.
// 참조: pkg/ipam/node.go - calculateExcessIPs()
func calculateExcessIPs(availableIPs, usedIPs, preAllocate, minAllocate, maxAboveWatermark int) int {
	if usedIPs <= (minAllocate + maxAboveWatermark) {
		if availableIPs <= (minAllocate + maxAboveWatermark) {
			return 0
		}
		if (usedIPs + preAllocate) <= (minAllocate + maxAboveWatermark) {
			return availableIPs - minAllocate - maxAboveWatermark
		}
	}
	excess := availableIPs - usedIPs - preAllocate - maxAboveWatermark
	if excess < 0 {
		return 0
	}
	return excess
}

// CreateInterface는 새 ENI를 생성하고 보조 IP를 할당한다.
// 참조: pkg/ipam/node.go - createInterface()
func (n *ENINode) CreateInterface(subnetCIDR, vpcCIDR string, numIPs int) (*ENIInterface, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.Interfaces) >= n.MaxInterfaces {
		return nil, fmt.Errorf("최대 인터페이스 수 %d 도달", n.MaxInterfaces)
	}

	iface := &ENIInterface{
		ID:         fmt.Sprintf("eni-%d", len(n.Interfaces)),
		SubnetCIDR: subnetCIDR,
		VPCCIDR:    vpcCIDR,
		MAC:        fmt.Sprintf("02:00:00:00:%02x:00", len(n.Interfaces)),
		MaxIPs:     n.MaxIPsPerInterface,
	}

	// 서브넷에서 보조 IP 할당 시뮬레이션
	_, subnet, _ := net.ParseCIDR(subnetCIDR)
	baseIP := binary.BigEndian.Uint32(subnet.IP.To4())
	startOffset := len(n.Interfaces)*n.MaxIPsPerInterface + 10 // 오프셋 시뮬레이션

	allocated := 0
	for i := 0; i < numIPs && i < n.MaxIPsPerInterface; i++ {
		ipInt := baseIP + uint32(startOffset+i)
		ipBytes := make(net.IP, 4)
		binary.BigEndian.PutUint32(ipBytes, ipInt)
		if subnet.Contains(ipBytes) {
			iface.SecondaryIPs = append(iface.SecondaryIPs, ipBytes.String())
			allocated++
		}
	}

	n.Interfaces = append(n.Interfaces, iface)
	return iface, nil
}

// AllocateIP는 가용 보조 IP 중 하나를 파드에 할당한다.
// 참조: pkg/ipam/crd.go - crdAllocator.AllocateNext()
func (n *ENINode) AllocateIP(owner string) (*AllocationResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for _, iface := range n.Interfaces {
		for _, ip := range iface.SecondaryIPs {
			if _, used := n.UsedIPs[ip]; !used {
				n.UsedIPs[ip] = owner
				return &AllocationResult{
					IP:              net.ParseIP(ip),
					IPPoolName:      DefaultPool,
					PrimaryMAC:      iface.MAC,
					CIDRs:           []string{iface.VPCCIDR},
					GatewayIP:       gatewayIP(iface.SubnetCIDR),
					InterfaceNumber: iface.ID,
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("사용 가능한 IP가 없음 (Operator가 IP를 할당하면 재시도)")
}

// ReleaseIP는 파드의 IP를 해제한다.
func (n *ENINode) ReleaseIP(ip string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.UsedIPs, ip)
}

// MaintainIPPool은 워터마크 기반으로 IP 풀을 유지한다.
// 참조: pkg/ipam/node.go - MaintainIPPool(), determineMaintenanceAction()
func (n *ENINode) MaintainIPPool() (action string) {
	n.mu.Lock()
	availableIPs := n.AvailableIPs()
	usedIPs := n.UsedIPCount()
	preAlloc := n.PreAllocate
	minAlloc := n.MinAllocate
	maxAbove := n.MaxAboveWatermark
	maxAlloc := n.MaxAllocate
	n.mu.Unlock()

	needed := calculateNeededIPs(availableIPs, usedIPs, preAlloc, minAlloc, maxAlloc)
	excess := calculateExcessIPs(availableIPs, usedIPs, preAlloc, minAlloc, maxAbove)

	if needed > 0 {
		// 기존 인터페이스에 IP 추가 시도 시뮬레이션
		n.mu.Lock()
		for _, iface := range n.Interfaces {
			if len(iface.SecondaryIPs) < iface.MaxIPs && needed > 0 {
				_, subnet, _ := net.ParseCIDR(iface.SubnetCIDR)
				baseIP := binary.BigEndian.Uint32(subnet.IP.To4())
				offset := len(iface.SecondaryIPs) + 100 + len(n.Interfaces)*50

				toAdd := needed
				if toAdd > iface.MaxIPs-len(iface.SecondaryIPs) {
					toAdd = iface.MaxIPs - len(iface.SecondaryIPs)
				}

				for i := 0; i < toAdd; i++ {
					ipInt := baseIP + uint32(offset+i)
					ipBytes := make(net.IP, 4)
					binary.BigEndian.PutUint32(ipBytes, ipInt)
					iface.SecondaryIPs = append(iface.SecondaryIPs, ipBytes.String())
					needed--
				}
			}
		}
		n.mu.Unlock()

		if needed > 0 {
			// 새 인터페이스 필요
			iface, err := n.CreateInterface("10.0.0.0/16", "10.0.0.0/8", needed)
			if err != nil {
				return fmt.Sprintf("인터페이스 생성 실패: %v", err)
			}
			return fmt.Sprintf("새 인터페이스 %s 생성, %d개 IP 할당", iface.ID, len(iface.SecondaryIPs))
		}
		return fmt.Sprintf("%d개 IP 기존 인터페이스에 추가됨", needed)
	}

	if excess > 0 {
		return fmt.Sprintf("%d개 초과 IP 감지 (해제 대기)", excess)
	}

	return "풀 유지 완료 - 변경 없음"
}

func gatewayIP(subnetCIDR string) string {
	_, subnet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return ""
	}
	gw := make(net.IP, len(subnet.IP))
	copy(gw, subnet.IP)
	gw[len(gw)-1] = 1
	return gw.String()
}

// =============================================================================
// 6. 듀얼스택 IPAM
//    참조: pkg/ipam/allocator.go - AllocateNext()
// =============================================================================

// DualStackIPAM은 IPv4와 IPv6를 동시에 관리하는 IPAM이다.
type DualStackIPAM struct {
	mu            sync.Mutex
	ipv4Allocator Allocator
	ipv6Allocator Allocator
	owners        map[Pool]map[string]string // pool -> ip -> owner
	excludedIPs   map[string]string
}

func NewDualStackIPAM(ipv4Alloc, ipv6Alloc Allocator) *DualStackIPAM {
	return &DualStackIPAM{
		ipv4Allocator: ipv4Alloc,
		ipv6Allocator: ipv6Alloc,
		owners:        make(map[Pool]map[string]string),
		excludedIPs:   make(map[string]string),
	}
}

// AllocateNext는 IPv4와 IPv6 주소를 동시에 할당한다.
// 참조: pkg/ipam/allocator.go - AllocateNext()
func (d *DualStackIPAM) AllocateNext(family string, owner string, pool Pool) (ipv4Result, ipv6Result *AllocationResult, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if (family == "ipv6" || family == "") && d.ipv6Allocator != nil {
		ipv6Result, err = d.ipv6Allocator.AllocateNext(owner, pool)
		if err != nil {
			return nil, nil, fmt.Errorf("IPv6 할당 실패: %w", err)
		}
		d.registerOwner(ipv6Result.IP, owner, pool)
	}

	if (family == "ipv4" || family == "") && d.ipv4Allocator != nil {
		ipv4Result, err = d.ipv4Allocator.AllocateNext(owner, pool)
		if err != nil {
			// IPv6가 할당되었으면 롤백
			if ipv6Result != nil {
				d.ipv6Allocator.Release(ipv6Result.IP, pool)
				d.releaseOwner(ipv6Result.IP, pool)
			}
			return nil, nil, fmt.Errorf("IPv4 할당 실패 (IPv6 롤백됨): %w", err)
		}
		d.registerOwner(ipv4Result.IP, owner, pool)
	}

	return
}

// ReleaseIP는 IP를 해제한다.
func (d *DualStackIPAM) ReleaseIP(ip net.IP, pool Pool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	family := DeriveFamily(ip)
	if family == IPv4 && d.ipv4Allocator != nil {
		d.ipv4Allocator.Release(ip, pool)
	} else if family == IPv6 && d.ipv6Allocator != nil {
		d.ipv6Allocator.Release(ip, pool)
	}
	d.releaseOwner(ip, pool)
	return nil
}

func (d *DualStackIPAM) registerOwner(ip net.IP, owner string, pool Pool) {
	if _, ok := d.owners[pool]; !ok {
		d.owners[pool] = make(map[string]string)
	}
	d.owners[pool][ip.String()] = owner
}

func (d *DualStackIPAM) releaseOwner(ip net.IP, pool Pool) {
	if m, ok := d.owners[pool]; ok {
		delete(m, ip.String())
	}
}

// Dump는 할당된 모든 IP를 덤프한다.
func (d *DualStackIPAM) Dump() (v4 map[string]string, v6 map[string]string) {
	v4 = make(map[string]string)
	v6 = make(map[string]string)

	if d.ipv4Allocator != nil {
		pools, _ := d.ipv4Allocator.Dump()
		for _, alloc := range pools {
			for ip, owner := range alloc {
				v4[ip] = owner
			}
		}
	}
	if d.ipv6Allocator != nil {
		pools, _ := d.ipv6Allocator.Dump()
		for _, alloc := range pools {
			for ip, owner := range alloc {
				v6[ip] = owner
			}
		}
	}
	return
}

// =============================================================================
// 7. Pre-allocation Manager
//    참조: pkg/ipam/node.go - calculateNeededIPs, calculateExcessIPs
//    참조: pkg/ipam/multipool_manager.go - computeNeededIPsPerPoolLocked
// =============================================================================

// PreAllocManager는 워터마크 기반 사전 할당 관리자이다.
type PreAllocManager struct {
	mu               sync.Mutex
	pool             *CIDRPool
	preAllocate      int
	minAllocate      int
	maxAboveWatermark int
	usedIPs          map[string]string
}

func NewPreAllocManager(preAlloc, minAlloc, maxAbove int) *PreAllocManager {
	return &PreAllocManager{
		pool:              NewCIDRPool(),
		preAllocate:       preAlloc,
		minAllocate:       minAlloc,
		maxAboveWatermark: maxAbove,
		usedIPs:           make(map[string]string),
	}
}

// AddCIDR은 풀에 CIDR을 추가한다.
func (p *PreAllocManager) AddCIDR(cidr string) error {
	return p.pool.AddCIDR(cidr)
}

// AllocateNext는 다음 IP를 할당한다.
func (p *PreAllocManager) AllocateNext(owner string) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ip, err := p.pool.AllocateNext(owner)
	if err != nil {
		return nil, err
	}
	p.usedIPs[ip.String()] = owner
	return ip, nil
}

// Release는 IP를 해제한다.
func (p *PreAllocManager) Release(ip net.IP) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.usedIPs, ip.String())
	p.pool.Release(ip)
}

// GetStats는 현재 통계를 반환한다.
func (p *PreAllocManager) GetStats() (available, used, needed, excess int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	available = p.pool.InUseCount() + p.pool.FreeCount()
	used = len(p.usedIPs)
	needed = calculateNeededIPs(available, used, p.preAllocate, p.minAllocate, 0)
	excess = calculateExcessIPs(available, used, p.preAllocate, p.minAllocate, p.maxAboveWatermark)
	return
}

// =============================================================================
// 메인 - 데모 실행
// =============================================================================

func main() {
	printHeader("Cilium IPAM PoC - IP Address Management 시뮬레이션")

	demoClusterPool()
	demoMultiPool()
	demoENIStyle()
	demoPreAllocation()
	demoIPExhaustion()
	demoDualStack()
}

// -----------------------------------------------------------------------------
// 데모 1: Cluster-Pool IPAM
// -----------------------------------------------------------------------------

func demoClusterPool() {
	printHeader("데모 1: Cluster-Pool IPAM (hostScopeAllocator)")
	fmt.Println("참조: pkg/ipam/hostscope.go")
	fmt.Println("설명: Operator가 노드에 할당한 CIDR에서 순차적으로 IP를 할당합니다.")
	fmt.Println()

	// 노드에 할당된 PodCIDR 시뮬레이션 (10.244.1.0/28 = 14개 IP)
	allocator, err := NewHostScopeAllocator("10.244.1.0/28")
	if err != nil {
		fmt.Printf("  [오류] 할당기 생성 실패: %v\n", err)
		return
	}
	fmt.Printf("  CIDR: 10.244.1.0/28 (총 용량: %d IPs)\n\n", allocator.Capacity())

	// 파드 5개에 IP 할당
	fmt.Println("  --- IP 할당 ---")
	allocatedIPs := make([]*AllocationResult, 0)
	for i := 1; i <= 5; i++ {
		owner := fmt.Sprintf("pod-%d", i)
		result, err := allocator.AllocateNext(owner, DefaultPool)
		if err != nil {
			fmt.Printf("  [실패] %s: %v\n", owner, err)
			continue
		}
		allocatedIPs = append(allocatedIPs, result)
		fmt.Printf("  [할당] %s -> %s (pool: %s)\n", owner, result.IP, result.IPPoolName)
	}

	// 상태 덤프
	fmt.Println()
	pools, status := allocator.Dump()
	fmt.Printf("  상태: %s\n", status)
	fmt.Printf("  할당된 IP 수: %d\n", len(pools[DefaultPool]))

	// IP 해제
	fmt.Println("\n  --- IP 해제 ---")
	if len(allocatedIPs) >= 2 {
		ip := allocatedIPs[1].IP
		allocator.Release(ip, DefaultPool)
		fmt.Printf("  [해제] %s\n", ip)
	}

	// 해제 후 재할당
	result, _ := allocator.AllocateNext("pod-new", DefaultPool)
	if result != nil {
		fmt.Printf("  [재할당] pod-new -> %s (해제된 IP 재사용)\n", result.IP)
	}

	_, status = allocator.Dump()
	fmt.Printf("\n  최종 상태: %s\n", status)
}

// -----------------------------------------------------------------------------
// 데모 2: Multi-Pool IPAM
// -----------------------------------------------------------------------------

func demoMultiPool() {
	printHeader("데모 2: Multi-Pool IPAM")
	fmt.Println("참조: pkg/ipam/multipool.go, pkg/ipam/multipool_manager.go")
	fmt.Println("설명: CiliumPodIPPool CRD로 여러 풀을 정의하고 annotation 기반으로 할당합니다.")
	fmt.Println()

	mgr := NewMultiPoolManager(true, true)

	// 풀 생성 (CiliumPodIPPool CRD 시뮬레이션)
	mgr.CreatePool("production", []string{"10.10.0.0/24"}, []string{"fd00:10::/120"}, 4)
	mgr.CreatePool("staging", []string{"10.20.0.0/24"}, []string{"fd00:20::/120"}, 2)
	mgr.CreatePool("monitoring", []string{"10.30.0.0/28"}, nil, 2) // IPv4만

	fmt.Println("  생성된 풀:")
	fmt.Println("    production  - IPv4: 10.10.0.0/24, IPv6: fd00:10::/120, preAlloc: 4")
	fmt.Println("    staging     - IPv4: 10.20.0.0/24, IPv6: fd00:20::/120, preAlloc: 2")
	fmt.Println("    monitoring  - IPv4: 10.30.0.0/28, IPv6: 없음, preAlloc: 2")

	// annotation 기반 풀 선택 시뮬레이션
	fmt.Println("\n  --- Annotation 기반 IP 할당 ---")

	// 시뮬레이션: Pod annotation에서 풀 결정
	podAnnotations := map[string]Pool{
		"web-app-1":    "production",
		"web-app-2":    "production",
		"test-pod-1":   "staging",
		"monitor-pod":  "monitoring",
	}

	for pod, pool := range podAnnotations {
		v4Result, err := mgr.AllocateNext(pod, pool, IPv4)
		if err != nil {
			fmt.Printf("  [실패] %s (pool=%s, IPv4): %v\n", pod, pool, err)
			continue
		}
		fmt.Printf("  [할당] %s -> IPv4: %s (pool: %s)\n", pod, v4Result.IP, pool)

		// IPv6가 있는 풀만 IPv6 할당
		v6Result, err := mgr.AllocateNext(pod, pool, IPv6)
		if err != nil {
			fmt.Printf("           %s -> IPv6: 없음 (%v)\n", pod, err)
		} else {
			fmt.Printf("           %s -> IPv6: %s (pool: %s)\n", pod, v6Result.IP, pool)
		}
	}

	// 풀별 수요 계산 (neededIPCeil 사용)
	fmt.Println("\n  --- 풀별 IP 수요 (neededIPCeil 적용) ---")
	demands := mgr.ComputeNeededIPsPerPool()
	for poolName, demand := range demands {
		v4InUse, v4Free, v6InUse, v6Free := mgr.GetPoolStatus(poolName)
		fmt.Printf("  풀 %s:\n", poolName)
		if d, ok := demand[IPv4]; ok {
			fmt.Printf("    IPv4: 사용중=%d, 여유=%d, 목표수요=%d\n", v4InUse, v4Free, d)
		}
		if d, ok := demand[IPv6]; ok {
			fmt.Printf("    IPv6: 사용중=%d, 여유=%d, 목표수요=%d\n", v6InUse, v6Free, d)
		}
	}

	// neededIPCeil 동작 시연
	fmt.Println("\n  --- neededIPCeil 계산 예시 (preAlloc=16) ---")
	for _, numIP := range []int{0, 1, 15, 16, 17, 32, 33} {
		fmt.Printf("    neededIPCeil(%d, 16) = %d\n", numIP, neededIPCeil(numIP, 16))
	}
}

// -----------------------------------------------------------------------------
// 데모 3: ENI-Style IPAM
// -----------------------------------------------------------------------------

func demoENIStyle() {
	printHeader("데모 3: ENI-Style IPAM (인터페이스 기반)")
	fmt.Println("참조: pkg/ipam/crd.go, pkg/ipam/node_manager.go, pkg/ipam/node.go")
	fmt.Println("설명: AWS ENI처럼 인터페이스에 보조 IP를 할당한 후 파드에 배분합니다.")
	fmt.Println()

	// m5.large 인스턴스 시뮬레이션: 최대 3 ENI, ENI당 10 IP
	node := NewENINode("worker-1", 3, 10, 8, 4, 2)
	fmt.Printf("  노드: %s (최대 인터페이스: %d, 인터페이스당 최대 IP: %d)\n", node.Name, node.MaxInterfaces, node.MaxIPsPerInterface)
	fmt.Printf("  설정: PreAllocate=%d, MinAllocate=%d, MaxAboveWatermark=%d\n\n", node.PreAllocate, node.MinAllocate, node.MaxAboveWatermark)

	// 1단계: 첫 번째 ENI 생성
	fmt.Println("  --- 1단계: 첫 번째 ENI 생성 ---")
	iface1, err := node.CreateInterface("10.0.0.0/16", "10.0.0.0/8", 10)
	if err != nil {
		fmt.Printf("  [오류] %v\n", err)
		return
	}
	fmt.Printf("  [인터페이스] %s 생성: MAC=%s, 보조IP=%d개\n",
		iface1.ID, iface1.MAC, len(iface1.SecondaryIPs))
	for i, ip := range iface1.SecondaryIPs {
		if i < 3 {
			fmt.Printf("    - %s\n", ip)
		}
	}
	if len(iface1.SecondaryIPs) > 3 {
		fmt.Printf("    ... (총 %d개)\n", len(iface1.SecondaryIPs))
	}

	// 2단계: 파드에 IP 할당
	fmt.Println("\n  --- 2단계: 파드에 IP 할당 ---")
	for i := 1; i <= 5; i++ {
		owner := fmt.Sprintf("pod-%d", i)
		result, err := node.AllocateIP(owner)
		if err != nil {
			fmt.Printf("  [실패] %s: %v\n", owner, err)
			continue
		}
		fmt.Printf("  [할당] %s -> IP=%s, MAC=%s, GW=%s, ENI=%s\n",
			owner, result.IP, result.PrimaryMAC, result.GatewayIP, result.InterfaceNumber)
	}

	// 3단계: 워터마크 확인
	fmt.Println("\n  --- 3단계: 워터마크 확인 ---")
	available := node.AvailableIPs()
	used := node.UsedIPCount()
	needed := calculateNeededIPs(available, used, node.PreAllocate, node.MinAllocate, 0)
	excess := calculateExcessIPs(available, used, node.PreAllocate, node.MinAllocate, node.MaxAboveWatermark)
	fmt.Printf("  가용IP: %d, 사용중: %d, 필요: %d, 초과: %d\n", available, used, needed, excess)

	// 4단계: IP 부족 시 새 ENI 생성 시뮬레이션
	fmt.Println("\n  --- 4단계: IP를 거의 소진 후 새 인터페이스 생성 ---")
	for i := 6; i <= 10; i++ {
		node.AllocateIP(fmt.Sprintf("pod-%d", i))
	}
	fmt.Printf("  현재: 가용=%d, 사용=%d\n", node.AvailableIPs(), node.UsedIPCount())
	action := node.MaintainIPPool()
	fmt.Printf("  풀 유지보수 결과: %s\n", action)
	fmt.Printf("  유지보수 후: 가용=%d, 사용=%d, 인터페이스=%d\n",
		node.AvailableIPs(), node.UsedIPCount(), len(node.Interfaces))
}

// -----------------------------------------------------------------------------
// 데모 4: Pre-allocation 메커니즘
// -----------------------------------------------------------------------------

func demoPreAllocation() {
	printHeader("데모 4: Pre-allocation (워터마크 기반 자동 보충)")
	fmt.Println("참조: pkg/ipam/node.go - calculateNeededIPs(), calculateExcessIPs()")
	fmt.Println("설명: 사용중 IP가 임계치에 도달하면 자동으로 추가 IP를 확보합니다.")
	fmt.Println()

	// 시나리오: preAllocate=8, minAllocate=4, maxAboveWatermark=2
	fmt.Println("  설정: preAllocate=8, minAllocate=4, maxAboveWatermark=2")
	fmt.Println()

	fmt.Println("  --- calculateNeededIPs 워터마크 동작 ---")
	fmt.Printf("  %-12s %-8s %-12s %-10s %-12s\n", "Available", "Used", "PreAllocate", "MinAlloc", "Needed")
	fmt.Println("  " + strings.Repeat("-", 58))

	testCases := []struct{ available, used, preAlloc, minAlloc, maxAlloc int }{
		{0, 0, 8, 4, 0},   // 초기 상태: 8개 필요
		{8, 0, 8, 4, 0},   // 8개 확보 후: 0개 필요
		{8, 3, 8, 4, 0},   // 3개 사용: 3개 필요 (여유 5 < preAlloc 8)
		{11, 3, 8, 4, 0},  // 보충 후: 0개 필요 (여유 8 = preAlloc)
		{11, 8, 8, 4, 0},  // 8개 사용: 5개 필요
		{16, 8, 8, 4, 0},  // 보충 후: 0개 필요
		{16, 14, 8, 4, 0}, // 14개 사용: 6개 필요
		{16, 8, 8, 4, 20}, // maxAllocate=20: 0개 필요
		{16, 15, 8, 4, 20},// maxAllocate=20: 4개 필요 (20 - 16 = 4로 제한)
	}

	for _, tc := range testCases {
		needed := calculateNeededIPs(tc.available, tc.used, tc.preAlloc, tc.minAlloc, tc.maxAlloc)
		maxStr := "무제한"
		if tc.maxAlloc > 0 {
			maxStr = fmt.Sprintf("%d", tc.maxAlloc)
		}
		fmt.Printf("  %-12d %-8d %-12d %-10d %-12s -> needed=%d\n",
			tc.available, tc.used, tc.preAlloc, tc.minAlloc, maxStr, needed)
	}

	fmt.Println()
	fmt.Println("  --- calculateExcessIPs 초과 감지 ---")
	fmt.Printf("  %-12s %-8s %-12s %-10s %-16s %-10s\n",
		"Available", "Used", "PreAllocate", "MinAlloc", "MaxAboveWater", "Excess")
	fmt.Println("  " + strings.Repeat("-", 72))

	excessCases := []struct{ available, used, preAlloc, minAlloc, maxAbove int }{
		{16, 8, 8, 4, 2},  // 여유 8: 초과 없음 (8 - 8 - 2 = -2 -> 0)
		{20, 8, 8, 4, 2},  // 여유 12: 2개 초과 (12 - 8 - 2 = 2)
		{30, 8, 8, 4, 2},  // 여유 22: 12개 초과
		{10, 2, 8, 4, 2},  // minAlloc+maxAbove=6 이하: 4개 초과 (10 - 6 = 4)
		{6, 2, 8, 4, 2},   // 딱 맞음: 초과 없음
	}

	for _, tc := range excessCases {
		excess := calculateExcessIPs(tc.available, tc.used, tc.preAlloc, tc.minAlloc, tc.maxAbove)
		fmt.Printf("  %-12d %-8d %-12d %-10d %-16d -> excess=%d\n",
			tc.available, tc.used, tc.preAlloc, tc.minAlloc, tc.maxAbove, excess)
	}
}

// -----------------------------------------------------------------------------
// 데모 5: IP 고갈 및 복구
// -----------------------------------------------------------------------------

func demoIPExhaustion() {
	printHeader("데모 5: IP 풀 고갈 및 복구")
	fmt.Println("참조: pkg/ipam/pool.go - cidrPool.allocateNext()")
	fmt.Println("설명: 작은 CIDR에서 IP를 모두 소진시킨 후, 새 CIDR 추가로 복구합니다.")
	fmt.Println()

	pool := NewCIDRPool()
	pool.AddCIDR("10.99.0.0/30") // 2개 IP만 사용 가능

	fmt.Println("  초기 풀: 10.99.0.0/30 (가용 IP: 2개)")
	fmt.Println()

	// 할당 시도
	fmt.Println("  --- 할당 시도 ---")
	for i := 1; i <= 4; i++ {
		owner := fmt.Sprintf("pod-%d", i)
		ip, err := pool.AllocateNext(owner)
		if err != nil {
			fmt.Printf("  [실패] %s: %v\n", owner, err)
		} else {
			fmt.Printf("  [성공] %s -> %s\n", owner, ip)
		}
	}

	fmt.Printf("\n  풀 상태: 사용중=%d, 여유=%d\n", pool.InUseCount(), pool.FreeCount())

	// 복구: 새 CIDR 추가 (Operator가 CiliumNode CRD를 통해 CIDR 할당)
	fmt.Println("\n  --- 복구: Operator가 새 CIDR 할당 ---")
	pool.AddCIDR("10.99.1.0/28") // 14개 IP 추가
	fmt.Println("  새 CIDR 추가: 10.99.1.0/28 (14개 IP)")
	fmt.Printf("  복구 후 풀 상태: 사용중=%d, 여유=%d\n", pool.InUseCount(), pool.FreeCount())

	// 추가 할당
	fmt.Println("\n  --- 복구 후 할당 ---")
	for i := 5; i <= 8; i++ {
		owner := fmt.Sprintf("pod-%d", i)
		ip, err := pool.AllocateNext(owner)
		if err != nil {
			fmt.Printf("  [실패] %s: %v\n", owner, err)
		} else {
			fmt.Printf("  [성공] %s -> %s\n", owner, ip)
		}
	}
	fmt.Printf("\n  최종 풀 상태: 사용중=%d, 여유=%d\n", pool.InUseCount(), pool.FreeCount())

	// IP 해제 후 재활용
	fmt.Println("\n  --- IP 해제 및 재활용 ---")
	pool.Release(net.ParseIP("10.99.0.1"))
	fmt.Println("  [해제] 10.99.0.1")
	ip, _ := pool.AllocateNext("pod-recycled")
	fmt.Printf("  [재활용] pod-recycled -> %s\n", ip)
}

// -----------------------------------------------------------------------------
// 데모 6: 듀얼스택 (IPv4 + IPv6)
// -----------------------------------------------------------------------------

func demoDualStack() {
	printHeader("데모 6: 듀얼스택 (IPv4 + IPv6 동시 할당)")
	fmt.Println("참조: pkg/ipam/allocator.go - AllocateNext()")
	fmt.Println("설명: 하나의 파드에 IPv4와 IPv6 주소를 동시에 할당합니다.")
	fmt.Println()

	// 듀얼스택 할당기 생성
	v4Alloc, _ := NewHostScopeAllocator("10.200.0.0/28")
	v6Alloc, _ := NewHostScopeAllocator("fd00:200::/124")

	ds := NewDualStackIPAM(v4Alloc, v6Alloc)

	fmt.Println("  IPv4 풀: 10.200.0.0/28")
	fmt.Println("  IPv6 풀: fd00:200::/124")
	fmt.Println()

	// 듀얼스택 할당
	fmt.Println("  --- 듀얼스택 할당 (family=\"\") ---")
	type podAlloc struct {
		v4, v6 net.IP
	}
	pods := make(map[string]podAlloc)

	for i := 1; i <= 4; i++ {
		owner := fmt.Sprintf("pod-%d", i)
		v4r, v6r, err := ds.AllocateNext("", owner, DefaultPool)
		if err != nil {
			fmt.Printf("  [실패] %s: %v\n", owner, err)
			continue
		}
		pods[owner] = podAlloc{v4: v4r.IP, v6: v6r.IP}
		fmt.Printf("  [할당] %s -> IPv4: %-16s IPv6: %s\n", owner, v4r.IP, v6r.IP)
	}

	// IPv4만 할당
	fmt.Println("\n  --- IPv4만 할당 (family=\"ipv4\") ---")
	v4r, _, err := ds.AllocateNext("ipv4", "pod-v4only", DefaultPool)
	if err != nil {
		fmt.Printf("  [실패] pod-v4only: %v\n", err)
	} else {
		fmt.Printf("  [할당] pod-v4only -> IPv4: %s\n", v4r.IP)
	}

	// IPv6만 할당
	fmt.Println("\n  --- IPv6만 할당 (family=\"ipv6\") ---")
	_, v6r, err := ds.AllocateNext("ipv6", "pod-v6only", DefaultPool)
	if err != nil {
		fmt.Printf("  [실패] pod-v6only: %v\n", err)
	} else {
		fmt.Printf("  [할당] pod-v6only -> IPv6: %s\n", v6r.IP)
	}

	// 상태 덤프
	v4Map, v6Map := ds.Dump()
	fmt.Printf("\n  할당 현황: IPv4=%d개, IPv6=%d개\n", len(v4Map), len(v6Map))

	// 해제 시 양쪽 모두 해제
	fmt.Println("\n  --- IP 해제 ---")
	if pa, ok := pods["pod-2"]; ok {
		ds.ReleaseIP(pa.v4, DefaultPool)
		ds.ReleaseIP(pa.v6, DefaultPool)
		fmt.Printf("  [해제] pod-2 -> IPv4: %s, IPv6: %s\n", pa.v4, pa.v6)
	}

	v4Map, v6Map = ds.Dump()
	fmt.Printf("  해제 후: IPv4=%d개, IPv6=%d개\n", len(v4Map), len(v6Map))

	// 롤백 시연: IPv6 고갈 시 IPv4도 롤백
	fmt.Println("\n  --- 롤백 시연: IPv6 풀 고갈 ---")
	// IPv6 풀을 소진시키기
	for i := 0; i < 20; i++ {
		_, _, err := ds.AllocateNext("ipv6", fmt.Sprintf("exhaust-%d", i), DefaultPool)
		if err != nil {
			break
		}
	}
	// 듀얼스택 할당 시도 -> IPv6 실패 시 IPv4 롤백
	_, _, err = ds.AllocateNext("", "pod-rollback", DefaultPool)
	if err != nil {
		fmt.Printf("  [롤백 발생] pod-rollback: %v\n", err)
	}
	v4Map, _ = ds.Dump()
	fmt.Printf("  롤백 확인: IPv4 할당수 변경 없음 = %d개\n", len(v4Map))
}

// =============================================================================
// 유틸리티 함수
// =============================================================================

func printHeader(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 78))
	fmt.Println()
}
