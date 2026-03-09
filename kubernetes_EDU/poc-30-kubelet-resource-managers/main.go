// Kubelet Resource Manager PoC
//
// Kubernetes Kubelet의 Resource Manager 서브시스템을 시뮬레이션한다:
// 1. NUMA 토폴로지 표현
// 2. CPU 할당 (Static Policy, Packed vs Spread)
// 3. Topology Hint 생성 및 병합 (mergePermutation, HintMerger)
// 4. 정책 비교 (none / best-effort / restricted / single-numa-node)
// 5. Resource Manager 조율 흐름 (CPU + Memory + Device 통합)
//
// 실행: go run main.go
// 외부 의존성 없음 (Go 표준 라이브러리만 사용)

package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// =============================================================================
// 1. NUMA 토폴로지 표현
// =============================================================================

// BitMask는 NUMA 노드 비트마스크를 표현한다.
// 실제 Kubernetes: k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask
type BitMask uint64

func NewBitMask(bits ...int) BitMask {
	var mask BitMask
	for _, b := range bits {
		mask |= 1 << b
	}
	return mask
}

func (bm BitMask) Count() int {
	count := 0
	for v := bm; v != 0; v &= v - 1 {
		count++
	}
	return count
}

func (bm BitMask) GetBits() []int {
	var bits []int
	for i := 0; i < 64; i++ {
		if bm&(1<<i) != 0 {
			bits = append(bits, i)
		}
	}
	return bits
}

func (bm BitMask) IsSet(bit int) bool {
	return bm&(1<<bit) != 0
}

func (bm BitMask) And(other BitMask) BitMask {
	return bm & other
}

func (bm BitMask) Or(other BitMask) BitMask {
	return bm | other
}

func (bm BitMask) IsNarrowerThan(other BitMask) bool {
	return bm.Count() < other.Count()
}

func (bm BitMask) IsEqual(other BitMask) bool {
	return bm == other
}

func (bm BitMask) String() string {
	bits := bm.GetBits()
	strs := make([]string, len(bits))
	for i, b := range bits {
		strs[i] = fmt.Sprintf("%d", b)
	}
	return fmt.Sprintf("[%s]", strings.Join(strs, ","))
}

// NUMADistances는 NUMA 노드 간 거리 행렬이다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/numa_info.go:26
type NUMADistances map[int][]uint64

func (d NUMADistances) CalculateAverageFor(bm BitMask) float64 {
	if bm.Count() == 0 {
		return 0
	}
	var count, sum float64
	for _, n1 := range bm.GetBits() {
		for _, n2 := range bm.GetBits() {
			sum += float64(d[n1][n2])
			count++
		}
	}
	return sum / count
}

// NUMAInfo는 NUMA 노드 목록과 거리 정보를 담는다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/numa_info.go:28-31
type NUMAInfo struct {
	Nodes         []int
	NUMADistances NUMADistances
}

func (n *NUMAInfo) DefaultAffinityMask() BitMask {
	return NewBitMask(n.Nodes...)
}

func (n *NUMAInfo) Narrowest(m1, m2 BitMask) BitMask {
	if m1.IsNarrowerThan(m2) {
		return m1
	}
	return m2
}

func (n *NUMAInfo) Closest(m1, m2 BitMask) BitMask {
	if m1.Count() != m2.Count() {
		return n.Narrowest(m1, m2)
	}
	d1 := n.NUMADistances.CalculateAverageFor(m1)
	d2 := n.NUMADistances.CalculateAverageFor(m2)
	if d1 == d2 {
		if m1 < m2 {
			return m1
		}
		return m2
	}
	if d1 < d2 {
		return m1
	}
	return m2
}

// CPUInfo는 하나의 논리 CPU 정보이다.
type CPUInfo struct {
	CPUID    int
	CoreID   int
	SocketID int
	NUMAID   int
}

// CPUTopology는 전체 CPU 토폴로지를 표현한다.
// 실제 Kubernetes: pkg/kubelet/cm/cpumanager/topology/
type CPUTopology struct {
	CPUs       []CPUInfo
	NumSockets int
	NumNUMA    int
	NumCores   int
}

func NewCPUTopology(numSockets, coresPerSocket, threadsPerCore int) *CPUTopology {
	topo := &CPUTopology{
		NumSockets: numSockets,
		NumNUMA:    numSockets, // 1 socket = 1 NUMA (일반적)
		NumCores:   numSockets * coresPerSocket,
	}
	cpuID := 0
	for s := 0; s < numSockets; s++ {
		for c := 0; c < coresPerSocket; c++ {
			coreID := s*coresPerSocket + c
			for t := 0; t < threadsPerCore; t++ {
				topo.CPUs = append(topo.CPUs, CPUInfo{
					CPUID:    cpuID,
					CoreID:   coreID,
					SocketID: s,
					NUMAID:   s,
				})
				cpuID++
			}
		}
	}
	return topo
}

func (t *CPUTopology) CPUsInNUMA(numaID int) []int {
	var cpus []int
	for _, c := range t.CPUs {
		if c.NUMAID == numaID {
			cpus = append(cpus, c.CPUID)
		}
	}
	return cpus
}

func (t *CPUTopology) CPUsPerCore() int {
	if t.NumCores == 0 {
		return 1
	}
	return len(t.CPUs) / t.NumCores
}

// =============================================================================
// 2. CPU 할당 시뮬레이션 (Static Policy, Packed vs Spread)
// =============================================================================

// CPUSet은 CPU ID 집합이다.
// 실제 Kubernetes: k8s.io/utils/cpuset
type CPUSet map[int]struct{}

func NewCPUSet(cpus ...int) CPUSet {
	s := make(CPUSet)
	for _, c := range cpus {
		s[c] = struct{}{}
	}
	return s
}

func (s CPUSet) Size() int { return len(s) }

func (s CPUSet) Contains(cpu int) bool {
	_, ok := s[cpu]
	return ok
}

func (s CPUSet) Difference(other CPUSet) CPUSet {
	result := NewCPUSet()
	for cpu := range s {
		if !other.Contains(cpu) {
			result[cpu] = struct{}{}
		}
	}
	return result
}

func (s CPUSet) Union(other CPUSet) CPUSet {
	result := NewCPUSet()
	for cpu := range s {
		result[cpu] = struct{}{}
	}
	for cpu := range other {
		result[cpu] = struct{}{}
	}
	return result
}

func (s CPUSet) ToSlice() []int {
	var result []int
	for cpu := range s {
		result = append(result, cpu)
	}
	sort.Ints(result)
	return result
}

func (s CPUSet) String() string {
	return fmt.Sprintf("%v", s.ToSlice())
}

// cpuAccumulator는 CPU 선택 알고리즘을 구현한다.
// 실제 Kubernetes: pkg/kubelet/cm/cpumanager/cpu_assignment.go:259-299
type cpuAccumulator struct {
	topo          *CPUTopology
	available     CPUSet // 사용 가능한 CPU
	numCPUsNeeded int    // 아직 필요한 CPU 수
	result        CPUSet // 축적한 CPU
	packed        bool   // true: packed, false: spread
}

func newCPUAccumulator(topo *CPUTopology, available CPUSet, numCPUs int, packed bool) *cpuAccumulator {
	// available를 복사하여 원본 보존
	avail := NewCPUSet()
	for cpu := range available {
		avail[cpu] = struct{}{}
	}
	return &cpuAccumulator{
		topo:          topo,
		available:     avail,
		numCPUsNeeded: numCPUs,
		result:        NewCPUSet(),
		packed:        packed,
	}
}

func (a *cpuAccumulator) isSatisfied() bool {
	return a.numCPUsNeeded <= 0
}

func (a *cpuAccumulator) take(cpuID int) {
	a.result[cpuID] = struct{}{}
	delete(a.available, cpuID)
	a.numCPUsNeeded--
}

// sortCPUs는 Packed 또는 Spread 전략에 따라 CPU를 정렬한다.
// 실제 Kubernetes: cpu_assignment.go의 sortAvailableCPUsPacked/Spread
func (a *cpuAccumulator) sortCPUs(numaID int) []int {
	var cpus []int
	for _, cpu := range a.topo.CPUsInNUMA(numaID) {
		if a.available.Contains(cpu) {
			cpus = append(cpus, cpu)
		}
	}

	if a.packed {
		// Packed: 코어 ID 순서로 정렬 (같은 코어의 HT를 연속으로)
		sort.Slice(cpus, func(i, j int) bool {
			ci := a.topo.CPUs[cpus[i]]
			cj := a.topo.CPUs[cpus[j]]
			if ci.CoreID != cj.CoreID {
				return ci.CoreID < cj.CoreID
			}
			return ci.CPUID < cj.CPUID
		})
	} else {
		// Spread: 코어 간에 분산 (각 코어에서 하나씩)
		sort.Slice(cpus, func(i, j int) bool {
			ci := a.topo.CPUs[cpus[i]]
			cj := a.topo.CPUs[cpus[j]]
			// 스레드 인덱스 우선, 그 다음 코어 ID
			ti := ci.CPUID - ci.CoreID*a.topo.CPUsPerCore()
			tj := cj.CPUID - cj.CoreID*a.topo.CPUsPerCore()
			if ti != tj {
				return ti < tj
			}
			return ci.CoreID < cj.CoreID
		})
	}
	return cpus
}

// allocateCPUs는 NUMA affinity에 따라 CPU를 할당한다.
func (a *cpuAccumulator) allocateCPUs(numaAffinity BitMask) CPUSet {
	// NUMA affinity에 속하는 노드부터 할당
	affinityNodes := numaAffinity.GetBits()

	// 1단계: affinity NUMA 노드에서 CPU 할당
	for _, numaID := range affinityNodes {
		if a.isSatisfied() {
			break
		}
		sorted := a.sortCPUs(numaID)
		for _, cpu := range sorted {
			if a.isSatisfied() {
				break
			}
			a.take(cpu)
		}
	}

	// 2단계: affinity 외 NUMA 노드에서 부족분 보충 (fallback)
	if !a.isSatisfied() {
		for numaID := 0; numaID < a.topo.NumNUMA; numaID++ {
			if numaAffinity.IsSet(numaID) {
				continue
			}
			sorted := a.sortCPUs(numaID)
			for _, cpu := range sorted {
				if a.isSatisfied() {
					break
				}
				a.take(cpu)
			}
		}
	}

	return a.result
}

// =============================================================================
// 3. Topology Hint 생성 및 병합
// =============================================================================

// TopologyHint는 NUMA 배치 힌트이다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/topology_manager.go:105-110
type TopologyHint struct {
	NUMANodeAffinity BitMask
	Preferred        bool
}

func (h TopologyHint) String() string {
	return fmt.Sprintf("{NUMA: %s, Preferred: %v}", h.NUMANodeAffinity, h.Preferred)
}

// HintProvider는 TopologyHint를 제공하는 인터페이스이다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/topology_manager.go:80-96
type HintProvider interface {
	Name() string
	GetTopologyHints(resourceName string, request int) map[string][]TopologyHint
}

// generateAllBitMasks는 주어진 NUMA 노드의 모든 비트마스크 조합을 생성한다.
// 실제 Kubernetes: bitmask.IterateBitMasks
func generateAllBitMasks(nodes []int) []BitMask {
	n := len(nodes)
	total := (1 << n) - 1 // 2^n - 1 (빈 집합 제외)
	masks := make([]BitMask, 0, total)
	for i := 1; i <= total; i++ {
		var mask BitMask
		for bit := 0; bit < n; bit++ {
			if i&(1<<bit) != 0 {
				mask = mask.Or(NewBitMask(nodes[bit]))
			}
		}
		masks = append(masks, mask)
	}
	// 비트 수 오름차순 정렬
	sort.Slice(masks, func(i, j int) bool {
		if masks[i].Count() != masks[j].Count() {
			return masks[i].Count() < masks[j].Count()
		}
		return masks[i] < masks[j]
	})
	return masks
}

// mergePermutation은 순열의 힌트를 비트와이즈 AND로 병합한다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy.go:44-69
func mergePermutation(defaultAffinity BitMask, permutation []TopologyHint) TopologyHint {
	preferred := true
	merged := defaultAffinity
	var firstAffinity BitMask
	hasFirst := false

	for _, hint := range permutation {
		if hint.NUMANodeAffinity != 0 {
			merged = merged.And(hint.NUMANodeAffinity)
			if !hasFirst {
				firstAffinity = hint.NUMANodeAffinity
				hasFirst = true
			} else if !hint.NUMANodeAffinity.IsEqual(firstAffinity) {
				preferred = false
			}
		}
		if !hint.Preferred {
			preferred = false
		}
	}

	return TopologyHint{
		NUMANodeAffinity: merged,
		Preferred:        preferred,
	}
}

// maxOfMinAffinityCounts는 모든 리소스 힌트의 최소 NUMA 수 중 최대값을 계산한다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy.go:123-135
func maxOfMinAffinityCounts(allHints [][]TopologyHint) int {
	maxOfMin := 0
	for _, hints := range allHints {
		minCount := math.MaxInt32
		for _, h := range hints {
			if h.NUMANodeAffinity != 0 && h.NUMANodeAffinity.Count() < minCount {
				minCount = h.NUMANodeAffinity.Count()
			}
		}
		if minCount != math.MaxInt32 && minCount > maxOfMin {
			maxOfMin = minCount
		}
	}
	return maxOfMin
}

// HintMerger는 여러 리소스의 힌트를 병합한다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy.go:137-322
type HintMerger struct {
	NUMAInfo                      *NUMAInfo
	Hints                         [][]TopologyHint
	BestNonPreferredAffinityCount int
	PolicyName                    string
	PreferClosestNUMA             bool
}

func NewHintMerger(numaInfo *NUMAInfo, hints [][]TopologyHint, policyName string, preferClosest bool) *HintMerger {
	return &HintMerger{
		NUMAInfo:                      numaInfo,
		Hints:                         hints,
		BestNonPreferredAffinityCount: maxOfMinAffinityCounts(hints),
		PolicyName:                    policyName,
		PreferClosestNUMA:             preferClosest,
	}
}

// compareNUMAAffinityMasks는 두 힌트의 NUMA affinity를 비교한다.
func (m *HintMerger) compareNUMAAffinityMasks(current, candidate *TopologyHint) *TopologyHint {
	if candidate.NUMANodeAffinity.IsEqual(current.NUMANodeAffinity) {
		return current
	}
	var best BitMask
	if m.PolicyName != "single-numa-node" && m.PreferClosestNUMA {
		best = m.NUMAInfo.Closest(current.NUMANodeAffinity, candidate.NUMANodeAffinity)
	} else {
		best = m.NUMAInfo.Narrowest(current.NUMANodeAffinity, candidate.NUMANodeAffinity)
	}
	if best.IsEqual(current.NUMANodeAffinity) {
		return current
	}
	return candidate
}

// compare는 현재 최적 힌트와 후보를 비교한다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy.go:180-300
func (m *HintMerger) compare(current, candidate *TopologyHint) *TopologyHint {
	if candidate.NUMANodeAffinity.Count() == 0 {
		return current
	}
	if current == nil {
		return candidate
	}
	if !current.Preferred && candidate.Preferred {
		return candidate
	}
	if current.Preferred && !candidate.Preferred {
		return current
	}
	if current.Preferred && candidate.Preferred {
		return m.compareNUMAAffinityMasks(current, candidate)
	}

	// 둘 다 non-preferred
	bestCount := m.BestNonPreferredAffinityCount
	curCount := current.NUMANodeAffinity.Count()
	canCount := candidate.NUMANodeAffinity.Count()

	if curCount > bestCount {
		return m.compareNUMAAffinityMasks(current, candidate)
	}
	if curCount == bestCount {
		if canCount != bestCount {
			return current
		}
		return m.compareNUMAAffinityMasks(current, candidate)
	}
	// curCount < bestCount
	if canCount > bestCount {
		return current
	}
	if canCount == bestCount {
		return candidate
	}
	if canCount > curCount {
		return candidate
	}
	if canCount < curCount {
		return current
	}
	return m.compareNUMAAffinityMasks(current, candidate)
}

// Merge는 모든 순열을 순회하여 최적 힌트를 찾는다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy.go:303-322
func (m *HintMerger) Merge() TopologyHint {
	defaultAffinity := m.NUMAInfo.DefaultAffinityMask()

	var bestHint *TopologyHint
	m.iteratePermutations(m.Hints, 0, nil, func(perm []TopologyHint) {
		merged := mergePermutation(defaultAffinity, perm)
		bestHint = m.compare(bestHint, &merged)
	})

	if bestHint == nil {
		return TopologyHint{defaultAffinity, false}
	}
	return *bestHint
}

// iteratePermutations는 모든 힌트 순열을 재귀적으로 순회한다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy.go:324-360
func (m *HintMerger) iteratePermutations(allHints [][]TopologyHint, depth int, current []TopologyHint, callback func([]TopologyHint)) {
	if depth == len(allHints) {
		callback(current)
		return
	}
	for _, hint := range allHints[depth] {
		m.iteratePermutations(allHints, depth+1, append(current, hint), callback)
	}
}

// =============================================================================
// 4. 정책 구현 (none / best-effort / restricted / single-numa-node)
// =============================================================================

// Policy는 Topology Manager 정책 인터페이스이다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy.go:24-31
type Policy interface {
	Name() string
	Merge(providersHints []map[string][]TopologyHint, numaInfo *NUMAInfo) (TopologyHint, bool)
}

// --- none 정책 ---
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy_none.go

type nonePolicy struct{}

func (p *nonePolicy) Name() string { return "none" }

func (p *nonePolicy) Merge(providersHints []map[string][]TopologyHint, numaInfo *NUMAInfo) (TopologyHint, bool) {
	return TopologyHint{}, true // 항상 admit, 힌트 무시
}

// --- best-effort 정책 ---
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy_best_effort.go

type bestEffortPolicy struct{}

func (p *bestEffortPolicy) Name() string { return "best-effort" }

func (p *bestEffortPolicy) Merge(providersHints []map[string][]TopologyHint, numaInfo *NUMAInfo) (TopologyHint, bool) {
	filtered := filterProvidersHints(providersHints)
	merger := NewHintMerger(numaInfo, filtered, p.Name(), false)
	bestHint := merger.Merge()
	return bestHint, true // 항상 admit
}

// --- restricted 정책 ---
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy_restricted.go

type restrictedPolicy struct{}

func (p *restrictedPolicy) Name() string { return "restricted" }

func (p *restrictedPolicy) Merge(providersHints []map[string][]TopologyHint, numaInfo *NUMAInfo) (TopologyHint, bool) {
	filtered := filterProvidersHints(providersHints)
	merger := NewHintMerger(numaInfo, filtered, p.Name(), false)
	bestHint := merger.Merge()
	return bestHint, bestHint.Preferred // Preferred 아니면 거부
}

// --- single-numa-node 정책 ---
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy_single_numa_node.go

type singleNumaNodePolicy struct{}

func (p *singleNumaNodePolicy) Name() string { return "single-numa-node" }

func (p *singleNumaNodePolicy) Merge(providersHints []map[string][]TopologyHint, numaInfo *NUMAInfo) (TopologyHint, bool) {
	filtered := filterProvidersHints(providersHints)
	singleNuma := filterSingleNumaHints(filtered)
	merger := NewHintMerger(numaInfo, singleNuma, p.Name(), false)
	bestHint := merger.Merge()
	if bestHint.NUMANodeAffinity.IsEqual(numaInfo.DefaultAffinityMask()) {
		bestHint = TopologyHint{0, bestHint.Preferred}
	}
	return bestHint, bestHint.Preferred
}

// filterProvidersHints는 Provider 힌트를 전처리한다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy.go:71-101
func filterProvidersHints(providersHints []map[string][]TopologyHint) [][]TopologyHint {
	var allHints [][]TopologyHint
	for _, hints := range providersHints {
		if len(hints) == 0 {
			allHints = append(allHints, []TopologyHint{{0, true}})
			continue
		}
		for _, resourceHints := range hints {
			if resourceHints == nil {
				allHints = append(allHints, []TopologyHint{{0, true}})
			} else if len(resourceHints) == 0 {
				allHints = append(allHints, []TopologyHint{{0, false}})
			} else {
				allHints = append(allHints, resourceHints)
			}
		}
	}
	return allHints
}

// filterSingleNumaHints는 단일 NUMA preferred 힌트만 유지한다.
// 실제 Kubernetes: pkg/kubelet/cm/topologymanager/policy_single_numa_node.go:46-61
func filterSingleNumaHints(allHints [][]TopologyHint) [][]TopologyHint {
	var filtered [][]TopologyHint
	for _, hints := range allHints {
		var singleNuma []TopologyHint
		for _, h := range hints {
			if h.NUMANodeAffinity == 0 && h.Preferred {
				singleNuma = append(singleNuma, h) // "아무 NUMA나 OK" 유지
			}
			if h.NUMANodeAffinity != 0 && h.NUMANodeAffinity.Count() == 1 && h.Preferred {
				singleNuma = append(singleNuma, h) // 단일 NUMA + preferred만
			}
		}
		filtered = append(filtered, singleNuma)
	}
	return filtered
}

// =============================================================================
// 5. Resource Manager 시뮬레이션
// =============================================================================

// MemoryState는 NUMA 노드별 메모리 상태이다.
// 실제 Kubernetes: pkg/kubelet/cm/memorymanager/state/state.go:24-30
type MemoryState struct {
	TotalMB     uint64
	ReservedMB  uint64
	AllocatedMB uint64
	FreeMB      uint64
}

// DeviceInfo는 디바이스 정보이다.
type DeviceInfo struct {
	ID     string
	NUMAID int
}

// CPUResourceManager는 CPU Manager를 시뮬레이션한다.
type CPUResourceManager struct {
	topology    *CPUTopology
	available   CPUSet
	assignments map[string]CPUSet // containerName -> assigned CPUs
	reserved    CPUSet
}

func NewCPUResourceManager(topo *CPUTopology, reservedCount int) *CPUResourceManager {
	available := NewCPUSet()
	for _, cpu := range topo.CPUs {
		available[cpu.CPUID] = struct{}{}
	}
	reserved := NewCPUSet()
	// 가장 낮은 ID부터 예약
	for i := 0; i < reservedCount && i < len(topo.CPUs); i++ {
		reserved[topo.CPUs[i].CPUID] = struct{}{}
	}

	return &CPUResourceManager{
		topology:    topo,
		available:   available.Difference(reserved), // ASSIGNABLE = 전체 - RESERVED
		assignments: make(map[string]CPUSet),
		reserved:    reserved,
	}
}

func (m *CPUResourceManager) Name() string { return "CPUManager" }

func (m *CPUResourceManager) GetTopologyHints(resourceName string, request int) map[string][]TopologyHint {
	if request == 0 {
		return nil
	}
	numaNodes := make([]int, m.topology.NumNUMA)
	for i := range numaNodes {
		numaNodes[i] = i
	}

	masks := generateAllBitMasks(numaNodes)
	minAffinitySize := len(numaNodes)
	var hints []TopologyHint

	for _, mask := range masks {
		count := 0
		for cpuID := range m.available {
			cpu := m.topology.CPUs[cpuID]
			if mask.IsSet(cpu.NUMAID) {
				count++
			}
		}
		if count >= request {
			if mask.Count() < minAffinitySize {
				minAffinitySize = mask.Count()
			}
			hints = append(hints, TopologyHint{
				NUMANodeAffinity: mask,
				Preferred:        false,
			})
		}
	}

	for i := range hints {
		if hints[i].NUMANodeAffinity.Count() == minAffinitySize {
			hints[i].Preferred = true
		}
	}

	return map[string][]TopologyHint{resourceName: hints}
}

func (m *CPUResourceManager) Allocate(containerName string, numCPUs int, numaAffinity BitMask, packed bool) CPUSet {
	acc := newCPUAccumulator(m.topology, m.available, numCPUs, packed)
	result := acc.allocateCPUs(numaAffinity)
	m.assignments[containerName] = result
	m.available = m.available.Difference(result)
	return result
}

// MemoryResourceManager는 Memory Manager를 시뮬레이션한다.
type MemoryResourceManager struct {
	numaStates map[int]*MemoryState
}

func NewMemoryResourceManager(numaCount int, memPerNUMA uint64, reservedPerNUMA uint64) *MemoryResourceManager {
	states := make(map[int]*MemoryState)
	for i := 0; i < numaCount; i++ {
		allocatable := memPerNUMA - reservedPerNUMA
		states[i] = &MemoryState{
			TotalMB:     memPerNUMA,
			ReservedMB:  reservedPerNUMA,
			AllocatedMB: 0,
			FreeMB:      allocatable,
		}
	}
	return &MemoryResourceManager{numaStates: states}
}

func (m *MemoryResourceManager) Name() string { return "MemoryManager" }

func (m *MemoryResourceManager) GetTopologyHints(resourceName string, requestMB int) map[string][]TopologyHint {
	if requestMB == 0 {
		return nil
	}
	numaNodes := make([]int, len(m.numaStates))
	for i := range numaNodes {
		numaNodes[i] = i
	}

	masks := generateAllBitMasks(numaNodes)
	minAffinitySize := len(numaNodes)
	var hints []TopologyHint

	for _, mask := range masks {
		totalFree := uint64(0)
		for _, numaID := range mask.GetBits() {
			if state, ok := m.numaStates[numaID]; ok {
				totalFree += state.FreeMB
			}
		}
		if totalFree >= uint64(requestMB) {
			if mask.Count() < minAffinitySize {
				minAffinitySize = mask.Count()
			}
			hints = append(hints, TopologyHint{
				NUMANodeAffinity: mask,
				Preferred:        false,
			})
		}
	}

	for i := range hints {
		if hints[i].NUMANodeAffinity.Count() == minAffinitySize {
			hints[i].Preferred = true
		}
	}

	return map[string][]TopologyHint{resourceName: hints}
}

func (m *MemoryResourceManager) Allocate(requestMB uint64, numaAffinity BitMask) {
	remaining := requestMB
	for _, numaID := range numaAffinity.GetBits() {
		if remaining == 0 {
			break
		}
		state := m.numaStates[numaID]
		alloc := remaining
		if alloc > state.FreeMB {
			alloc = state.FreeMB
		}
		state.AllocatedMB += alloc
		state.FreeMB -= alloc
		remaining -= alloc
	}
}

// DeviceResourceManager는 Device Manager를 시뮬레이션한다.
type DeviceResourceManager struct {
	devices   map[string][]DeviceInfo        // resourceName -> devices
	allocated map[string]map[string]bool     // resourceName -> deviceID -> allocated
	numaNodes []int
}

func NewDeviceResourceManager(numaNodes []int) *DeviceResourceManager {
	return &DeviceResourceManager{
		devices:   make(map[string][]DeviceInfo),
		allocated: make(map[string]map[string]bool),
		numaNodes: numaNodes,
	}
}

func (m *DeviceResourceManager) AddDevices(resourceName string, devices []DeviceInfo) {
	m.devices[resourceName] = devices
	m.allocated[resourceName] = make(map[string]bool)
}

func (m *DeviceResourceManager) Name() string { return "DeviceManager" }

// GetTopologyHints는 디바이스 토폴로지 힌트를 생성한다.
// 실제 Kubernetes: pkg/kubelet/cm/devicemanager/topology_hints.go:154-219
func (m *DeviceResourceManager) GetTopologyHints(resourceName string, request int) map[string][]TopologyHint {
	devices, ok := m.devices[resourceName]
	if !ok || request == 0 {
		return nil
	}

	// 가용 디바이스 확인
	var available []DeviceInfo
	for _, d := range devices {
		if !m.allocated[resourceName][d.ID] {
			available = append(available, d)
		}
	}

	if len(available) < request {
		return map[string][]TopologyHint{resourceName: {}}
	}

	masks := generateAllBitMasks(m.numaNodes)
	minAffinitySize := len(m.numaNodes)
	var hints []TopologyHint

	for _, mask := range masks {
		count := 0
		for _, d := range available {
			if mask.IsSet(d.NUMAID) {
				count++
			}
		}
		if count >= request {
			if mask.Count() < minAffinitySize {
				minAffinitySize = mask.Count()
			}
			hints = append(hints, TopologyHint{
				NUMANodeAffinity: mask,
				Preferred:        false,
			})
		}
	}

	for i := range hints {
		if hints[i].NUMANodeAffinity.Count() == minAffinitySize {
			hints[i].Preferred = true
		}
	}

	return map[string][]TopologyHint{resourceName: hints}
}

// =============================================================================
// 데모 실행
// =============================================================================

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printSubSeparator(title string) {
	fmt.Println()
	fmt.Printf("--- %s ---\n", title)
}

func main() {
	// =========================================================================
	// 데모 1: NUMA 토폴로지 표현
	// =========================================================================
	printSeparator("1. NUMA 토폴로지 표현")

	numaInfo := &NUMAInfo{
		Nodes: []int{0, 1},
		NUMADistances: NUMADistances{
			0: {10, 20},
			1: {20, 10},
		},
	}

	fmt.Printf("NUMA 노드: %v\n", numaInfo.Nodes)
	fmt.Printf("기본 Affinity Mask: %s\n", numaInfo.DefaultAffinityMask())
	fmt.Printf("NUMA 거리 행렬:\n")
	for _, n := range numaInfo.Nodes {
		fmt.Printf("  NUMA %d: %v\n", n, numaInfo.NUMADistances[n])
	}

	// CPU 토폴로지: 2소켓, 소켓당 4코어, HT=2
	topo := NewCPUTopology(2, 4, 2)
	fmt.Printf("\nCPU 토폴로지: %d 소켓, %d NUMA, %d 코어, %d CPU\n",
		topo.NumSockets, topo.NumNUMA, topo.NumCores, len(topo.CPUs))
	fmt.Printf("코어당 스레드: %d\n", topo.CPUsPerCore())

	for numaID := 0; numaID < topo.NumNUMA; numaID++ {
		cpus := topo.CPUsInNUMA(numaID)
		fmt.Printf("  NUMA %d: CPU %v\n", numaID, cpus)
	}

	// =========================================================================
	// 데모 2: CPU 할당 (Packed vs Spread)
	// =========================================================================
	printSeparator("2. CPU 할당: Packed vs Spread 전략")

	printSubSeparator("Packed 전략 (기본값)")
	available := NewCPUSet()
	for _, cpu := range topo.CPUs {
		available[cpu.CPUID] = struct{}{}
	}
	// CPU 0, 1 예약
	reserved := NewCPUSet(0, 1)
	assignable := available.Difference(reserved)
	fmt.Printf("전체 CPU: %s\n", available)
	fmt.Printf("예약 CPU: %s\n", reserved)
	fmt.Printf("할당가능 CPU: %s\n", assignable)

	accPacked := newCPUAccumulator(topo, assignable, 4, true)
	resultPacked := accPacked.allocateCPUs(NewBitMask(0)) // NUMA 0 선호
	fmt.Printf("4 CPU 요청 (NUMA 0 선호, Packed): %s\n", resultPacked)

	printSubSeparator("Spread 전략")
	accSpread := newCPUAccumulator(topo, assignable, 4, false)
	resultSpread := accSpread.allocateCPUs(NewBitMask(0)) // NUMA 0 선호
	fmt.Printf("4 CPU 요청 (NUMA 0 선호, Spread): %s\n", resultSpread)

	// =========================================================================
	// 데모 3: Topology Hint 생성 및 병합
	// =========================================================================
	printSeparator("3. Topology Hint 생성 및 병합")

	// CPU Manager: 6 CPU 가용 (NUMA 0: 6, NUMA 1: 8)
	cpuMgr := NewCPUResourceManager(topo, 2)
	cpuHints := cpuMgr.GetTopologyHints("cpu", 4)

	fmt.Println("CPU Manager 힌트 (CPU 4개 요청):")
	for _, h := range cpuHints["cpu"] {
		fmt.Printf("  %s\n", h)
	}

	// Memory Manager: NUMA 0=15GB, NUMA 1=15GB
	memMgr := NewMemoryResourceManager(2, 16384, 1024) // 16GB, 1GB 예약
	memHints := memMgr.GetTopologyHints("memory", 8192) // 8GB 요청

	fmt.Println("\nMemory Manager 힌트 (8GB 요청):")
	for _, h := range memHints["memory"] {
		fmt.Printf("  %s\n", h)
	}

	// Device Manager: GPU 2개 (NUMA 0에 1개, NUMA 1에 1개)
	devMgr := NewDeviceResourceManager([]int{0, 1})
	devMgr.AddDevices("nvidia.com/gpu", []DeviceInfo{
		{ID: "gpu-0", NUMAID: 0},
		{ID: "gpu-1", NUMAID: 1},
	})
	devHints := devMgr.GetTopologyHints("nvidia.com/gpu", 1)

	fmt.Println("\nDevice Manager 힌트 (GPU 1개 요청):")
	for _, h := range devHints["nvidia.com/gpu"] {
		fmt.Printf("  %s\n", h)
	}

	// Hint 병합
	printSubSeparator("Hint 병합 (mergePermutation)")

	allProviderHints := []map[string][]TopologyHint{
		cpuHints,
		memHints,
		devHints,
	}
	filtered := filterProvidersHints(allProviderHints)
	merger := NewHintMerger(numaInfo, filtered, "best-effort", false)
	mergedHint := merger.Merge()
	fmt.Printf("병합 결과: %s\n", mergedHint)

	// =========================================================================
	// 데모 4: 정책 비교
	// =========================================================================
	printSeparator("4. Topology Manager 정책 비교")

	policies := []Policy{
		&nonePolicy{},
		&bestEffortPolicy{},
		&restrictedPolicy{},
		&singleNumaNodePolicy{},
	}

	fmt.Println("시나리오 1: CPU=4, Memory=8GB, GPU=1 요청 (2-NUMA 시스템)")
	fmt.Println("  각 NUMA에 충분한 CPU/메모리 가용, GPU는 양쪽에 1개씩")
	fmt.Println()
	fmt.Printf("%-20s | %-30s | %-8s\n", "정책", "NUMA Affinity", "Admit")
	fmt.Println(strings.Repeat("-", 65))

	for _, p := range policies {
		hint, admit := p.Merge(allProviderHints, numaInfo)
		affinityStr := "없음"
		if hint.NUMANodeAffinity != 0 {
			affinityStr = fmt.Sprintf("%s (preferred=%v)", hint.NUMANodeAffinity, hint.Preferred)
		}
		admitStr := "허용"
		if !admit {
			admitStr = "거부"
		}
		fmt.Printf("%-20s | %-30s | %-8s\n", p.Name(), affinityStr, admitStr)
	}

	// 시나리오 2: 단일 NUMA 불가능한 경우
	printSubSeparator("시나리오 2: 단일 NUMA로 충족 불가능한 경우")

	topo2 := NewCPUTopology(2, 4, 1) // 2소켓, 소켓당 4코어, HT 없음
	numaInfo2 := &NUMAInfo{
		Nodes: []int{0, 1},
		NUMADistances: NUMADistances{
			0: {10, 20},
			1: {20, 10},
		},
	}

	cpuMgr2 := NewCPUResourceManager(topo2, 0)
	cpuHints2 := cpuMgr2.GetTopologyHints("cpu", 6) // 6 CPU 요청 (각 NUMA에 4개만)

	fmt.Println("CPU 토폴로지: NUMA 0에 4 CPU, NUMA 1에 4 CPU")
	fmt.Println("요청: CPU=6 (단일 NUMA로 불가능)")
	fmt.Println("\nCPU Manager 힌트:")
	for _, h := range cpuHints2["cpu"] {
		fmt.Printf("  %s\n", h)
	}

	allProviderHints2 := []map[string][]TopologyHint{cpuHints2}

	fmt.Printf("\n%-20s | %-30s | %-8s\n", "정책", "NUMA Affinity", "Admit")
	fmt.Println(strings.Repeat("-", 65))

	for _, p := range policies {
		hint, admit := p.Merge(allProviderHints2, numaInfo2)
		affinityStr := "없음"
		if hint.NUMANodeAffinity != 0 {
			affinityStr = fmt.Sprintf("%s (preferred=%v)", hint.NUMANodeAffinity, hint.Preferred)
		}
		admitStr := "허용"
		if !admit {
			admitStr = "거부"
		}
		fmt.Printf("%-20s | %-30s | %-8s\n", p.Name(), affinityStr, admitStr)
	}

	// =========================================================================
	// 데모 5: Resource Manager 조율 흐름
	// =========================================================================
	printSeparator("5. Resource Manager 조율 흐름 (전체 통합)")

	fmt.Println("시뮬레이션: Pod Admit 흐름 (restricted 정책)")
	fmt.Println()

	// 토폴로지 설정
	topo3 := NewCPUTopology(2, 4, 2) // 2소켓, 4코어, HT
	numaInfo3 := &NUMAInfo{
		Nodes: []int{0, 1},
		NUMADistances: NUMADistances{
			0: {10, 20},
			1: {20, 10},
		},
	}

	// Resource Manager 초기화
	cpuMgr3 := NewCPUResourceManager(topo3, 2)
	memMgr3 := NewMemoryResourceManager(2, 16384, 1024)
	devMgr3 := NewDeviceResourceManager([]int{0, 1})
	devMgr3.AddDevices("nvidia.com/gpu", []DeviceInfo{
		{ID: "gpu-0", NUMAID: 0},
		{ID: "gpu-1", NUMAID: 1},
	})

	fmt.Println("[단계 1] Resource Manager 초기화")
	fmt.Printf("  CPU Manager: %d CPU 할당 가능 (예약=%s)\n",
		cpuMgr3.available.Size(), cpuMgr3.reserved)
	fmt.Printf("  Memory Manager: NUMA 0=%dMB, NUMA 1=%dMB 여유\n",
		memMgr3.numaStates[0].FreeMB, memMgr3.numaStates[1].FreeMB)
	fmt.Printf("  Device Manager: GPU 2개 (NUMA 0: gpu-0, NUMA 1: gpu-1)\n")

	// Pod 요청
	fmt.Println("\n[단계 2] Pod 요청: CPU=4, Memory=8GB, GPU=1")

	// 힌트 수집
	fmt.Println("\n[단계 3] HintProvider에서 TopologyHint 수집")
	cpuH := cpuMgr3.GetTopologyHints("cpu", 4)
	memH := memMgr3.GetTopologyHints("memory", 8192)
	devH := devMgr3.GetTopologyHints("nvidia.com/gpu", 1)

	fmt.Printf("  CPU Manager: %d개 힌트\n", len(cpuH["cpu"]))
	for _, h := range cpuH["cpu"] {
		fmt.Printf("    %s\n", h)
	}
	fmt.Printf("  Memory Manager: %d개 힌트\n", len(memH["memory"]))
	for _, h := range memH["memory"] {
		fmt.Printf("    %s\n", h)
	}
	fmt.Printf("  Device Manager: %d개 힌트\n", len(devH["nvidia.com/gpu"]))
	for _, h := range devH["nvidia.com/gpu"] {
		fmt.Printf("    %s\n", h)
	}

	// 정책으로 병합
	fmt.Println("\n[단계 4] restricted 정책으로 힌트 병합")
	policy := &restrictedPolicy{}
	allH := []map[string][]TopologyHint{cpuH, memH, devH}
	bestHint, admit := policy.Merge(allH, numaInfo3)
	fmt.Printf("  최적 힌트: %s\n", bestHint)
	fmt.Printf("  Admit 결과: %v\n", admit)

	if admit {
		fmt.Println("\n[단계 5] Admit 성공 -> 각 Manager에 Allocate() 호출")

		affinity := bestHint.NUMANodeAffinity
		if affinity == 0 {
			affinity = numaInfo3.DefaultAffinityMask()
		}

		// CPU 할당
		allocatedCPUs := cpuMgr3.Allocate("container-0", 4, affinity, true)
		fmt.Printf("  CPU 할당: %s (NUMA %s)\n", allocatedCPUs, affinity)

		// Memory 할당
		memMgr3.Allocate(8192, affinity)
		fmt.Printf("  Memory 할당: 8192MB @ NUMA %s\n", affinity)
		for _, numaID := range affinity.GetBits() {
			s := memMgr3.numaStates[numaID]
			fmt.Printf("    NUMA %d: allocated=%dMB, free=%dMB\n",
				numaID, s.AllocatedMB, s.FreeMB)
		}

		// 최종 상태
		fmt.Println("\n[단계 6] 최종 상태")
		fmt.Printf("  CPU 할당: container-0 -> %s\n", cpuMgr3.assignments["container-0"])
		fmt.Printf("  CPU 남은 가용: %s (%d개)\n", cpuMgr3.available, cpuMgr3.available.Size())
		fmt.Printf("  Memory NUMA 0: allocated=%dMB, free=%dMB\n",
			memMgr3.numaStates[0].AllocatedMB, memMgr3.numaStates[0].FreeMB)
		fmt.Printf("  Memory NUMA 1: allocated=%dMB, free=%dMB\n",
			memMgr3.numaStates[1].AllocatedMB, memMgr3.numaStates[1].FreeMB)
	} else {
		fmt.Println("\n[단계 5] Admit 거부! Pod는 이 노드에서 실행 불가")
	}

	// =========================================================================
	// 데모 6: 4-NUMA 시스템에서 PreferClosestNUMA
	// =========================================================================
	printSeparator("6. 4-NUMA 시스템: PreferClosestNUMA 비교")

	numaInfo4 := &NUMAInfo{
		Nodes: []int{0, 1, 2, 3},
		NUMADistances: NUMADistances{
			0: {10, 12, 20, 22},
			1: {12, 10, 22, 20},
			2: {20, 22, 10, 12},
			3: {22, 20, 12, 10},
		},
	}

	fmt.Println("NUMA 거리 행렬:")
	fmt.Printf("         ")
	for _, n := range numaInfo4.Nodes {
		fmt.Printf("NUMA %d  ", n)
	}
	fmt.Println()
	for _, n1 := range numaInfo4.Nodes {
		fmt.Printf("NUMA %d  ", n1)
		for _, n2 := range numaInfo4.Nodes {
			fmt.Printf("  %3d   ", numaInfo4.NUMADistances[n1][n2])
		}
		fmt.Println()
	}

	// 비교: [0,1] vs [0,2]
	m01 := NewBitMask(0, 1)
	m02 := NewBitMask(0, 2)

	fmt.Printf("\n비교: NUMA %s vs NUMA %s\n", m01, m02)
	fmt.Printf("  평균 거리 %s: %.1f\n", m01, numaInfo4.NUMADistances.CalculateAverageFor(m01))
	fmt.Printf("  평균 거리 %s: %.1f\n", m02, numaInfo4.NUMADistances.CalculateAverageFor(m02))

	narrowest := numaInfo4.Narrowest(m01, m02)
	closest := numaInfo4.Closest(m01, m02)
	fmt.Printf("  Narrowest 선택: %s (비트 수 동일 -> 더 작은 값)\n", narrowest)
	fmt.Printf("  Closest 선택:   %s (평균 거리가 짧은 것)\n", closest)

	// Hint 병합에서의 차이
	fmt.Println("\nHint 병합에서의 차이:")
	hints4 := [][]TopologyHint{
		{{NewBitMask(0, 1), true}, {NewBitMask(0, 2), true}},
		{{NewBitMask(0, 1), true}, {NewBitMask(0, 2), true}},
	}

	mergerNarrow := NewHintMerger(numaInfo4, hints4, "best-effort", false)
	resultNarrow := mergerNarrow.Merge()
	fmt.Printf("  Narrowest (기본):            %s\n", resultNarrow)

	mergerClose := NewHintMerger(numaInfo4, hints4, "best-effort", true)
	resultClose := mergerClose.Merge()
	fmt.Printf("  Closest (PreferClosestNUMA): %s\n", resultClose)

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("요약")
	fmt.Println(`
Kubelet Resource Manager 핵심 정리:

1. CPU Manager: Guaranteed QoS + 정수 CPU 요청 시 전용 CPU 할당
   - Static Policy: SHARED/RESERVED/ASSIGNABLE/EXCLUSIVE 4개 풀
   - cpuAccumulator: Packed(지역성) vs Spread(분산) 전략

2. Memory Manager: NUMA 노드별 메모리를 Block 단위로 할당
   - 힌트 확장(extend): Topology Manager 결과가 메모리 부족하면 확대
   - Single vs Cross NUMA 할당 규칙으로 혼재 방지

3. Device Manager: Device Plugin 기반 NUMA 위치 인식 할당
   - generateDeviceTopologyHints(): 모든 NUMA 비트마스크 조합 순회
   - Preferred = 최소 NUMA 수로 요청 충족 가능한 조합

4. Topology Manager: HintProvider 패턴으로 리소스 조율
   - none: 힌트 무시, 항상 admit
   - best-effort: 최적 힌트 사용, 항상 admit
   - restricted: 최적 힌트 사용, Preferred 아니면 reject
   - single-numa-node: 단일 NUMA 힌트만, Preferred 아니면 reject

5. Hint Merging: 순열 기반 비트와이즈 AND
   - 지수적 복잡도 -> NUMA 노드 최대 8개 제한
   - CompareNUMAAffinityMasks: Narrowest vs Closest`)
}
