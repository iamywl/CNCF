package main

import (
	"container/list"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium BPF 맵 시뮬레이션 — LRU Hash, Hash, PerCPU Hash
// =============================================================================
//
// Cilium은 eBPF 맵을 사용하여 커널 내에서 고성능 데이터 구조를 유지한다.
// 이 PoC는 세 가지 핵심 맵 타입의 동작 원리를 표준 라이브러리만으로 재현한다.
//
// 실제 소스 코드 참조:
//
// 1. Connection Tracking Map (LRU Hash)
//   - pkg/maps/ctmap/ctmap.go      → Map 구조체, CtMap 인터페이스
//   - pkg/maps/ctmap/types.go      → CtEntry (Packets, Bytes, Lifetime, Flags)
//   - pkg/maps/ctmap/gc/gc.go      → GC 구조체, ConntrackGCInterval, 만료 엔트리 제거
//   - pkg/maps/ctmap/ctmap.go:164  → GCFilter{RemoveExpired, Time, MatchIPs}
//   커널 맵 타입: BPF_MAP_TYPE_LRU_HASH (용량 초과 시 LRU 퇴출)
//
// 2. Policy Map (Hash)
//   - pkg/maps/policymap/policymap.go → PolicyMap, PolicyKey, PolicyEntry
//   - PolicyKey: {Prefixlen, Identity, TrafficDirection, Nexthdr, DestPort}
//   - policyEntryFlags: Deny(0x01), LPM prefix 정보
//   커널 맵 타입: BPF_MAP_TYPE_HASH (LPM trie 포함)
//
// 3. Metrics Map (Per-CPU Hash)
//   - pkg/maps/metricsmap/metricsmap.go → Key{Reason, Dir, Line, File}, Value{Count, Bytes}
//   - 방향: INGRESS(1), EGRESS(2), SERVICE(3)
//   - metricsmapCollector → Prometheus Collector 구현
//   커널 맵 타입: BPF_MAP_TYPE_PERCPU_HASH
//
// 4. BPF Map 공통 인프라
//   - pkg/bpf/map_linux.go → Map 구조체 (ebpf.Map 래핑, 캐시, sync 컨트롤러)
//   - MapKey/MapValue 인터페이스: New() + String()
//   - 캐시 기반 비동기 동기화 (커널 맵과 유저스페이스 캐시)
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// 1. BPF 맵 공통 인터페이스 (pkg/bpf/map_linux.go)
// ─────────────────────────────────────────────────────────────────────────────

// BPFMapType은 eBPF 맵의 종류를 나타낸다.
// 실제 커널: enum bpf_map_type (include/uapi/linux/bpf.h)
type BPFMapType int

const (
	BPF_MAP_TYPE_HASH      BPFMapType = iota // 일반 해시맵
	BPF_MAP_TYPE_ARRAY                       // 고정 크기 배열
	BPF_MAP_TYPE_LRU_HASH                    // LRU 퇴출 해시맵
	BPF_MAP_TYPE_PERCPU_HASH                 // Per-CPU 해시맵
)

func (t BPFMapType) String() string {
	switch t {
	case BPF_MAP_TYPE_HASH:
		return "Hash"
	case BPF_MAP_TYPE_ARRAY:
		return "Array"
	case BPF_MAP_TYPE_LRU_HASH:
		return "LRU_Hash"
	case BPF_MAP_TYPE_PERCPU_HASH:
		return "PerCPU_Hash"
	default:
		return "Unknown"
	}
}

// BPFMap은 BPF 맵의 공통 인터페이스이다.
// 실제 코드: pkg/bpf/map_linux.go의 Map 구조체 메서드들
type BPFMap interface {
	Name() string
	Type() BPFMapType
	MaxEntries() int
	Count() int
	Dump() string
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. LRU Hash Map — Connection Tracking Map 시뮬레이션
// ─────────────────────────────────────────────────────────────────────────────
//
// 실제 코드: pkg/maps/ctmap/ctmap.go
// CT 맵은 BPF_MAP_TYPE_LRU_HASH를 사용한다.
// 용량 초과 시 가장 오래 사용되지 않은 엔트리를 자동 퇴출한다.
// GC가 주기적으로 만료된 엔트리를 제거한다 (gc/gc.go).

// CTTupleKey는 연결 추적 키이다.
// 실제 코드: pkg/maps/ctmap/types.go — CtKey4Global
// {SourceAddr, SourcePort, DestAddr, DestPort, NextHeader, Flags}
type CTTupleKey struct {
	SrcIP   string
	DstIP   string
	SrcPort uint16
	DstPort uint16
	Proto   uint8 // 6=TCP, 17=UDP
	Flags   uint8 // TUPLE_F_OUT(0), TUPLE_F_IN(1), TUPLE_F_RELATED(2), TUPLE_F_SERVICE(4)
}

func (k CTTupleKey) String() string {
	protoStr := "TCP"
	if k.Proto == 17 {
		protoStr = "UDP"
	}
	dirStr := "OUT"
	if k.Flags&1 != 0 {
		dirStr = "IN"
	}
	if k.Flags&4 != 0 {
		dirStr = "SVC"
	}
	return fmt.Sprintf("%s %s %s:%d -> %s:%d", protoStr, dirStr, k.SrcIP, k.SrcPort, k.DstIP, k.DstPort)
}

// CTEntry는 연결 추적 값이다.
// 실제 코드: pkg/maps/ctmap/types.go — CtEntry
// {Packets, Bytes, Lifetime, Flags, RevNAT, SourceSecurityID, ...}
type CTEntry struct {
	Packets          uint64
	Bytes            uint64
	Lifetime         uint32 // 만료 시간 (초 단위 타임스탬프)
	Flags            uint16 // RxClosing, TxClosing, Nat64, LBLoopback, ...
	RevNAT           uint16 // 역방향 NAT 인덱스
	SourceSecurityID uint32 // 소스 보안 ID (Identity)
}

// CT 엔트리 플래그 상수 (실제 코드: types.go:330-343)
const (
	CTFlagRxClosing = 1 << iota // 수신측 연결 종료 중
	CTFlagTxClosing             // 송신측 연결 종료 중
	CTFlagNat64                 // NAT64 변환 적용
	CTFlagLBLoopback            // LB 루프백
	CTFlagSeenNonSyn            // SYN 외의 패킷 확인됨
	CTFlagNodePort              // NodePort 서비스
	CTFlagProxyRedirect         // 프록시 리다이렉트
	CTFlagDSRInternal           // DSR 내부 엔트리
)

func (e CTEntry) String() string {
	var flags []string
	if e.Flags&CTFlagRxClosing != 0 {
		flags = append(flags, "RxClosing")
	}
	if e.Flags&CTFlagTxClosing != 0 {
		flags = append(flags, "TxClosing")
	}
	if e.Flags&CTFlagNodePort != 0 {
		flags = append(flags, "NodePort")
	}
	flagStr := strings.Join(flags, "|")
	if flagStr == "" {
		flagStr = "-"
	}
	return fmt.Sprintf("pkts=%d bytes=%d lifetime=%d flags=[%s] secID=%d",
		e.Packets, e.Bytes, e.Lifetime, flagStr, e.SourceSecurityID)
}

// LRUHashMap은 LRU 퇴출 기능이 있는 해시맵이다.
// 실제 커널: BPF_MAP_TYPE_LRU_HASH — 용량 초과 시 LRU 퇴출
// CT 맵의 기본 maxEntries: TCP=524288, Any=262144 (option.CTMapEntriesGlobalTCPDefault)
type LRUHashMap struct {
	mu         sync.RWMutex
	name       string
	maxEntries int
	data       map[string]*lruEntry   // key string -> entry
	order      *list.List             // LRU 순서 (front=최신, back=최구)
	elements   map[string]*list.Element // key string -> list element

	// 통계
	lookups   uint64
	updates   uint64
	deletes   uint64
	evictions uint64
}

type lruEntry struct {
	key   CTTupleKey
	value CTEntry
}

func NewLRUHashMap(name string, maxEntries int) *LRUHashMap {
	return &LRUHashMap{
		name:       name,
		maxEntries: maxEntries,
		data:       make(map[string]*lruEntry),
		order:      list.New(),
		elements:   make(map[string]*list.Element),
	}
}

func (m *LRUHashMap) Name() string     { return m.name }
func (m *LRUHashMap) Type() BPFMapType { return BPF_MAP_TYPE_LRU_HASH }
func (m *LRUHashMap) MaxEntries() int  { return m.maxEntries }

func (m *LRUHashMap) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}

// Update는 엔트리를 추가하거나 갱신한다.
// 용량 초과 시 LRU 퇴출이 발생한다.
// 실제 커널에서는 bpf_map_update_elem()이 이 역할을 한다.
func (m *LRUHashMap) Update(key CTTupleKey, value CTEntry) (evicted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	keyStr := key.String()
	m.updates++

	// 이미 존재하면 값 갱신 + LRU 순서 갱신
	if entry, ok := m.data[keyStr]; ok {
		entry.value = value
		m.order.MoveToFront(m.elements[keyStr])
		return false
	}

	// 용량 초과 시 LRU 퇴출
	if len(m.data) >= m.maxEntries {
		m.evictLRU()
		evicted = true
	}

	// 새 엔트리 추가
	entry := &lruEntry{key: key, value: value}
	m.data[keyStr] = entry
	elem := m.order.PushFront(entry)
	m.elements[keyStr] = elem
	return
}

// evictLRU는 가장 오래 접근되지 않은 엔트리를 제거한다.
func (m *LRUHashMap) evictLRU() {
	back := m.order.Back()
	if back == nil {
		return
	}
	entry := back.Value.(*lruEntry)
	keyStr := entry.key.String()
	delete(m.data, keyStr)
	delete(m.elements, keyStr)
	m.order.Remove(back)
	m.evictions++
}

// Lookup은 키로 엔트리를 조회한다. 조회 시 LRU 순서가 갱신된다.
func (m *LRUHashMap) Lookup(key CTTupleKey) (*CTEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	keyStr := key.String()
	m.lookups++

	entry, ok := m.data[keyStr]
	if !ok {
		return nil, false
	}

	// LRU 순서 갱신 — 접근된 엔트리를 front로 이동
	m.order.MoveToFront(m.elements[keyStr])
	return &entry.value, true
}

// Delete는 엔트리를 삭제한다.
func (m *LRUHashMap) Delete(key CTTupleKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	keyStr := key.String()
	if _, ok := m.data[keyStr]; !ok {
		return false
	}

	m.order.Remove(m.elements[keyStr])
	delete(m.data, keyStr)
	delete(m.elements, keyStr)
	m.deletes++
	return true
}

// GCFilter는 CT 맵 GC 필터이다.
// 실제 코드: pkg/maps/ctmap/ctmap.go:164 — GCFilter
type GCFilter struct {
	RemoveExpired bool            // 만료 엔트리 제거 활성화
	Time          uint32          // 현재 시간 (만료 비교 기준)
	MatchIPs      map[string]bool // 특정 IP에 매칭되는 엔트리만 제거
}

// RunGC는 GC를 실행하여 만료/매칭된 엔트리를 제거한다.
// 실제 코드: pkg/maps/ctmap/gc/gc.go — GC 구조체의 doGC 메서드
func (m *LRUHashMap) RunGC(filter GCFilter) (alive, deleted int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var toDelete []string

	for keyStr, entry := range m.data {
		shouldDelete := false

		// 만료 체크
		if filter.RemoveExpired && entry.value.Lifetime < filter.Time {
			shouldDelete = true
		}

		// IP 매칭 체크
		if len(filter.MatchIPs) > 0 {
			if filter.MatchIPs[entry.key.SrcIP] || filter.MatchIPs[entry.key.DstIP] {
				shouldDelete = true
			}
		}

		if shouldDelete {
			toDelete = append(toDelete, keyStr)
		}
	}

	for _, keyStr := range toDelete {
		if elem, ok := m.elements[keyStr]; ok {
			m.order.Remove(elem)
		}
		delete(m.data, keyStr)
		delete(m.elements, keyStr)
	}

	deleted = len(toDelete)
	alive = len(m.data)
	return
}

// Dump는 맵 내용을 문자열로 반환한다.
func (m *LRUHashMap) Dump() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== %s (LRU Hash, max=%d, count=%d) ===\n",
		m.name, m.maxEntries, len(m.data)))
	sb.WriteString(fmt.Sprintf("통계: lookups=%d updates=%d deletes=%d evictions=%d\n",
		m.lookups, m.updates, m.deletes, m.evictions))

	i := 0
	for elem := m.order.Front(); elem != nil && i < 10; elem = elem.Next() {
		entry := elem.Value.(*lruEntry)
		sb.WriteString(fmt.Sprintf("  [%d] %s -> %s\n", i, entry.key.String(), entry.value.String()))
		i++
	}
	if len(m.data) > 10 {
		sb.WriteString(fmt.Sprintf("  ... (%d개 더)\n", len(m.data)-10))
	}
	return sb.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Hash Map — Policy Map 시뮬레이션
// ─────────────────────────────────────────────────────────────────────────────
//
// 실제 코드: pkg/maps/policymap/policymap.go
// 정책 맵은 엔드포인트별로 존재하며, Identity+Port+Direction으로 정책을 조회한다.
// BPF_MAP_TYPE_HASH (LPM trie 포함)를 사용한다.

// TrafficDirection은 트래픽 방향이다.
// 실제 코드: pkg/policy/trafficdirection/
type TrafficDirection uint8

const (
	Ingress TrafficDirection = 0
	Egress  TrafficDirection = 1
)

func (d TrafficDirection) String() string {
	if d == Ingress {
		return "Ingress"
	}
	return "Egress"
}

// PolicyKey는 정책 맵의 키이다.
// 실제 코드: pkg/maps/policymap/policymap.go:107 — PolicyKey
// {Prefixlen, Identity, TrafficDirection, Nexthdr, DestPortNetwork}
type PolicyKey struct {
	Identity         uint32           // 소스/목적지 보안 Identity
	TrafficDirection TrafficDirection // Ingress/Egress
	Protocol         uint8            // 0=Any, 6=TCP, 17=UDP
	DestPort         uint16           // 0=AllPorts, 그 외=특정 포트
}

func (k PolicyKey) String() string {
	protoStr := "Any"
	if k.Protocol == 6 {
		protoStr = "TCP"
	} else if k.Protocol == 17 {
		protoStr = "UDP"
	}
	portStr := "*"
	if k.DestPort != 0 {
		portStr = fmt.Sprintf("%d", k.DestPort)
	}
	return fmt.Sprintf("id=%d dir=%s proto=%s port=%s", k.Identity, k.TrafficDirection, protoStr, portStr)
}

// PolicyEntry는 정책 맵의 값이다.
// 실제 코드: pkg/maps/policymap/policymap.go — PolicyEntry
type PolicyEntry struct {
	ProxyPort uint16           // 프록시 리다이렉트 포트 (0이면 직접 전달)
	Flags     PolicyEntryFlags // Allow/Deny, LPM prefix length
}

// PolicyEntryFlags는 정책 엔트리 플래그이다.
// 실제 코드: pkg/maps/policymap/policymap.go:58
type PolicyEntryFlags uint8

const (
	PolicyFlagDeny PolicyEntryFlags = 1 << iota // 거부 정책
)

func (e PolicyEntry) String() string {
	action := "ALLOW"
	if e.Flags&PolicyFlagDeny != 0 {
		action = "DENY"
	}
	proxyStr := ""
	if e.ProxyPort != 0 {
		proxyStr = fmt.Sprintf(" proxy=%d", e.ProxyPort)
	}
	return fmt.Sprintf("%s%s", action, proxyStr)
}

// PolicyHashMap은 정책 맵을 시뮬레이션한다.
// 실제 코드: pkg/maps/policymap/policymap.go — PolicyMap
// 이름 패턴: "cilium_policy_v2_{endpoint_id}"
type PolicyHashMap struct {
	mu         sync.RWMutex
	name       string
	epID       uint16 // 엔드포인트 ID
	maxEntries int
	data       map[PolicyKey]PolicyEntry
}

func NewPolicyHashMap(epID uint16, maxEntries int) *PolicyHashMap {
	// 실제 맵 이름 패턴: MapName = "cilium_policy_v2_" (policymap.go:37)
	return &PolicyHashMap{
		name:       fmt.Sprintf("cilium_policy_v2_%d", epID),
		epID:       epID,
		maxEntries: maxEntries,
		data:       make(map[PolicyKey]PolicyEntry),
	}
}

func (m *PolicyHashMap) Name() string     { return m.name }
func (m *PolicyHashMap) Type() BPFMapType { return BPF_MAP_TYPE_HASH }
func (m *PolicyHashMap) MaxEntries() int  { return m.maxEntries }

func (m *PolicyHashMap) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}

// Allow는 허용 정책을 추가한다.
func (m *PolicyHashMap) Allow(identity uint32, dir TrafficDirection, proto uint8, port uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := PolicyKey{Identity: identity, TrafficDirection: dir, Protocol: proto, DestPort: port}
	m.data[key] = PolicyEntry{Flags: 0} // Allow (Deny 플래그 없음)
}

// Deny는 거부 정책을 추가한다.
func (m *PolicyHashMap) Deny(identity uint32, dir TrafficDirection, proto uint8, port uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := PolicyKey{Identity: identity, TrafficDirection: dir, Protocol: proto, DestPort: port}
	m.data[key] = PolicyEntry{Flags: PolicyFlagDeny}
}

// AllowWithProxy는 프록시를 통한 허용 정책을 추가한다.
func (m *PolicyHashMap) AllowWithProxy(identity uint32, dir TrafficDirection, proto uint8, port uint16, proxyPort uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := PolicyKey{Identity: identity, TrafficDirection: dir, Protocol: proto, DestPort: port}
	m.data[key] = PolicyEntry{Flags: 0, ProxyPort: proxyPort}
}

// Lookup은 정책을 조회한다.
// 실제 조회 순서: 정확한 키 -> 포트 와일드카드(AllPorts=0) -> 프로토콜 와일드카드
// 이는 LPM(Longest Prefix Match) trie의 동작을 시뮬레이션한다.
func (m *PolicyHashMap) Lookup(identity uint32, dir TrafficDirection, proto uint8, port uint16) (PolicyEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 1. 정확한 키 매칭
	key := PolicyKey{Identity: identity, TrafficDirection: dir, Protocol: proto, DestPort: port}
	if entry, ok := m.data[key]; ok {
		return entry, true
	}

	// 2. 포트 와일드카드 매칭 (AllPorts=0)
	// 실제 코드: AllPorts = uint16(0) (policymap.go:47)
	key.DestPort = 0
	if entry, ok := m.data[key]; ok {
		return entry, true
	}

	// 3. 프로토콜 와일드카드 매칭
	key.Protocol = 0
	if entry, ok := m.data[key]; ok {
		return entry, true
	}

	return PolicyEntry{}, false
}

// Delete는 정책을 삭제한다.
func (m *PolicyHashMap) Delete(key PolicyKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[key]; !ok {
		return false
	}
	delete(m.data, key)
	return true
}

func (m *PolicyHashMap) Dump() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== %s (Hash, ep=%d, count=%d) ===\n",
		m.name, m.epID, len(m.data)))
	for key, entry := range m.data {
		sb.WriteString(fmt.Sprintf("  %s -> %s\n", key.String(), entry.String()))
	}
	return sb.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Per-CPU Hash — Metrics Map 시뮬레이션
// ─────────────────────────────────────────────────────────────────────────────
//
// 실제 코드: pkg/maps/metricsmap/metricsmap.go
// 메트릭 맵은 BPF_MAP_TYPE_PERCPU_HASH를 사용한다.
// 각 CPU가 독립적으로 카운터를 갱신하므로 lock-free로 고성능이다.
// 유저스페이스에서 읽을 때 모든 CPU의 값을 합산한다.

// MetricsKey는 메트릭 맵의 키이다.
// 실제 코드: pkg/maps/metricsmap/metricsmap.go:135 — Key
type MetricsKey struct {
	Reason uint8 // 드롭/포워드 사유
	Dir    uint8 // 0=Unknown, 1=Ingress, 2=Egress, 3=Service
}

// 메트릭 방향 상수 (실제 코드: metricsmap.go:113-117)
const (
	MetricDirIngress uint8 = 1
	MetricDirEgress  uint8 = 2
	MetricDirService uint8 = 3
)

// 메트릭 사유 상수
const (
	MetricReasonForwarded  uint8 = 0   // 정상 전달
	MetricReasonDropPolicy uint8 = 181 // 정책에 의한 드롭
)

func (k MetricsKey) String() string {
	dirMap := map[uint8]string{0: "UNKNOWN", 1: "INGRESS", 2: "EGRESS", 3: "SERVICE"}
	dir := dirMap[k.Dir]
	reason := "FORWARDED"
	if k.Reason == MetricReasonDropPolicy {
		reason = "DROP_POLICY"
	}
	return fmt.Sprintf("dir=%s reason=%s", dir, reason)
}

// MetricsValue는 메트릭 맵의 값이다.
// 실제 코드: pkg/maps/metricsmap/metricsmap.go:146 — Value
type MetricsValue struct {
	Count uint64 // 패킷 수
	Bytes uint64 // 바이트 수
}

// PerCPUMetricsMap은 Per-CPU 메트릭 맵을 시뮬레이션한다.
// 실제 커널에서는 각 CPU가 독립적인 메모리에 값을 갱신한다.
// 유저스페이스에서 읽으면 모든 CPU 값의 배열이 반환되고, 합산하여 사용한다.
type PerCPUMetricsMap struct {
	mu         sync.RWMutex
	name       string
	numCPUs    int
	maxEntries int
	// data[key][cpu_id] = MetricsValue — Per-CPU 데이터 구조
	data map[MetricsKey][]MetricsValue
}

func NewPerCPUMetricsMap(name string, numCPUs, maxEntries int) *PerCPUMetricsMap {
	return &PerCPUMetricsMap{
		name:       name,
		numCPUs:    numCPUs,
		maxEntries: maxEntries,
		data:       make(map[MetricsKey][]MetricsValue),
	}
}

func (m *PerCPUMetricsMap) Name() string     { return m.name }
func (m *PerCPUMetricsMap) Type() BPFMapType { return BPF_MAP_TYPE_PERCPU_HASH }
func (m *PerCPUMetricsMap) MaxEntries() int  { return m.maxEntries }

func (m *PerCPUMetricsMap) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}

// Increment는 특정 CPU에서 카운터를 증가시킨다.
// 실제 커널에서는 BPF 프로그램이 자동으로 현재 CPU의 값을 갱신한다.
func (m *PerCPUMetricsMap) Increment(key MetricsKey, cpuID int, packets uint64, bytes uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.data[key]; !ok {
		m.data[key] = make([]MetricsValue, m.numCPUs)
	}

	if cpuID < m.numCPUs {
		m.data[key][cpuID].Count += packets
		m.data[key][cpuID].Bytes += bytes
	}
}

// ReadAll은 모든 CPU의 값을 합산하여 반환한다.
// 실제 코드: metricsmap.go:208 — Values.Count(), Values.Bytes()
func (m *PerCPUMetricsMap) ReadAll() map[MetricsKey]MetricsValue {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[MetricsKey]MetricsValue)
	for key, perCPU := range m.data {
		var total MetricsValue
		for _, v := range perCPU {
			total.Count += v.Count
			total.Bytes += v.Bytes
		}
		result[key] = total
	}
	return result
}

// IterateWithCallback은 콜백으로 모든 엔트리를 순회한다.
// 실제 코드: metricsmap.go:156 — IterateWithCallback
func (m *PerCPUMetricsMap) IterateWithCallback(cb func(MetricsKey, []MetricsValue)) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for key, values := range m.data {
		cb(key, values)
	}
}

func (m *PerCPUMetricsMap) Dump() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== %s (PerCPU Hash, CPUs=%d, count=%d) ===\n",
		m.name, m.numCPUs, len(m.data)))

	for key, perCPU := range m.data {
		var totalCount, totalBytes uint64
		for _, v := range perCPU {
			totalCount += v.Count
			totalBytes += v.Bytes
		}
		sb.WriteString(fmt.Sprintf("  %s -> total_pkts=%d total_bytes=%d\n",
			key.String(), totalCount, totalBytes))
		for cpu, v := range perCPU {
			if v.Count > 0 {
				sb.WriteString(fmt.Sprintf("    CPU%d: pkts=%d bytes=%d\n", cpu, v.Count, v.Bytes))
			}
		}
	}
	return sb.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. 유틸리티
// ─────────────────────────────────────────────────────────────────────────────

func randomIP() string {
	return fmt.Sprintf("10.%d.%d.%d", rand.Intn(256), rand.Intn(256), rand.Intn(256))
}

func randomPort() uint16 {
	return uint16(1024 + rand.Intn(64512))
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. main — 시나리오 실행
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Println("================================================================")
	fmt.Println("  Cilium BPF 맵 시뮬레이션")
	fmt.Println("  LRU Hash (CT), Hash (Policy), PerCPU Hash (Metrics)")
	fmt.Println("================================================================")

	// ==========================================
	// 시나리오 1: CT Map — LRU Hash + GC
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 1: Connection Tracking Map (LRU Hash)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  CT 맵은 모든 네트워크 연결의 상태를 추적한다.")
	fmt.Println("  - 맵 이름: cilium_ct4_global (IPv4 TCP)")
	fmt.Println("  - 커널 타입: BPF_MAP_TYPE_LRU_HASH")
	fmt.Println("  - 기본 크기: TCP=524288, Any=262144")
	fmt.Println("  - GC: 주기적으로 만료된 엔트리 제거 (기본 30초 간격)")
	fmt.Println()

	// 작은 크기의 CT 맵 생성 (LRU 퇴출 시연을 위해)
	ctMap := NewLRUHashMap("cilium_ct4_global", 8)

	// 연결 추가
	currentTime := uint32(1000)
	connections := []struct {
		src, dst     string
		sport, dport uint16
		proto        uint8
		lifetime     uint32
	}{
		{"10.0.1.1", "10.0.2.1", 45678, 80, 6, currentTime + 300},   // TCP HTTP, 5분 후 만료
		{"10.0.1.2", "10.0.2.2", 45679, 443, 6, currentTime + 600},  // TCP HTTPS, 10분 후 만료
		{"10.0.1.3", "10.0.2.3", 12345, 53, 17, currentTime + 30},   // UDP DNS, 30초 후 만료
		{"10.0.1.4", "10.0.2.4", 45680, 8080, 6, currentTime + 900}, // TCP API, 15분 후 만료
		{"10.0.1.5", "10.0.2.5", 45681, 3306, 6, currentTime - 100}, // TCP MySQL, 이미 만료!
		{"10.0.1.6", "10.0.2.6", 45682, 6379, 6, currentTime + 1200},// TCP Redis, 20분 후 만료
	}

	fmt.Println("  [CT] 연결 추가:")
	for _, conn := range connections {
		key := CTTupleKey{
			SrcIP: conn.src, DstIP: conn.dst,
			SrcPort: conn.sport, DstPort: conn.dport,
			Proto: conn.proto, Flags: 0, // TUPLE_F_OUT
		}
		entry := CTEntry{
			Packets: uint64(rand.Intn(1000) + 1), Bytes: uint64(rand.Intn(100000) + 100),
			Lifetime: conn.lifetime, SourceSecurityID: uint32(rand.Intn(100) + 1),
		}
		ctMap.Update(key, entry)
		fmt.Printf("    + %s  lifetime=%d\n", key.String(), conn.lifetime)
	}

	fmt.Printf("\n  [CT] 맵 상태 (GC 전): %d/%d 엔트리\n", ctMap.Count(), ctMap.MaxEntries())

	// GC 실행 — 만료된 엔트리 제거
	fmt.Printf("\n  [CT] GC 실행 (현재 시간=%d):\n", currentTime)
	alive, deleted := ctMap.RunGC(GCFilter{
		RemoveExpired: true,
		Time:          currentTime,
	})
	fmt.Printf("    결과: alive=%d, deleted=%d\n", alive, deleted)

	// 조회
	fmt.Println("\n  [CT] 연결 조회:")
	lookupKey := CTTupleKey{SrcIP: "10.0.1.1", DstIP: "10.0.2.1", SrcPort: 45678, DstPort: 80, Proto: 6}
	if entry, ok := ctMap.Lookup(lookupKey); ok {
		fmt.Printf("    찾음: %s -> %s\n", lookupKey.String(), entry.String())
	}

	lookupKey2 := CTTupleKey{SrcIP: "10.0.1.5", DstIP: "10.0.2.5", SrcPort: 45681, DstPort: 3306, Proto: 6}
	if _, ok := ctMap.Lookup(lookupKey2); !ok {
		fmt.Printf("    못 찾음: %s (GC에 의해 제거됨)\n", lookupKey2.String())
	}

	// LRU 퇴출 시연
	fmt.Println("\n  [CT] LRU 퇴출 시연 (max=8, 추가 연결 삽입):")
	for i := 0; i < 5; i++ {
		key := CTTupleKey{
			SrcIP: randomIP(), DstIP: randomIP(),
			SrcPort: randomPort(), DstPort: 80,
			Proto: 6, Flags: 0,
		}
		entry := CTEntry{Packets: 1, Bytes: 64, Lifetime: currentTime + 300}
		evicted := ctMap.Update(key, entry)
		if evicted {
			fmt.Printf("    + %s (LRU 퇴출 발생!)\n", key.String())
		} else {
			fmt.Printf("    + %s\n", key.String())
		}
	}
	fmt.Printf("  [CT] 최종 맵 상태: %d/%d 엔트리\n", ctMap.Count(), ctMap.MaxEntries())

	// IP 기반 GC 시연
	fmt.Println("\n  [CT] IP 기반 GC (특정 IP의 연결 제거):")
	alive2, deleted2 := ctMap.RunGC(GCFilter{
		MatchIPs: map[string]bool{"10.0.1.1": true},
	})
	fmt.Printf("    10.0.1.1 관련 엔트리 제거: alive=%d, deleted=%d\n", alive2, deleted2)

	// ==========================================
	// 시나리오 2: Policy Map — Hash + LPM 조회
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 2: Policy Map (Hash + LPM 매칭)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  정책 맵은 엔드포인트별로 존재하며, 트래픽 허용/거부를 결정한다.")
	fmt.Println("  - 맵 이름: cilium_policy_v2_{ep_id}")
	fmt.Println("  - 키: {Identity, Direction, Protocol, Port}")
	fmt.Println("  - 조회: 정확한 매칭 -> 포트 와일드카드 -> 프로토콜 와일드카드")
	fmt.Println()

	policyMap := NewPolicyHashMap(1234, 16384)

	// 정책 추가
	fmt.Println("  [Policy] 정책 추가:")

	// Identity 100: Ingress TCP 80 허용 (L7 프록시 경유)
	policyMap.AllowWithProxy(100, Ingress, 6, 80, 15001)
	fmt.Println("    + id=100 Ingress TCP:80 ALLOW (proxy=15001)")

	// Identity 100: Ingress TCP 443 허용
	policyMap.Allow(100, Ingress, 6, 443)
	fmt.Println("    + id=100 Ingress TCP:443 ALLOW")

	// Identity 200: Egress 모든 프로토콜 모든 포트 허용
	policyMap.Allow(200, Egress, 0, 0)
	fmt.Println("    + id=200 Egress Any:* ALLOW (와일드카드)")

	// Identity 300: Ingress TCP 22 거부
	policyMap.Deny(300, Ingress, 6, 22)
	fmt.Println("    + id=300 Ingress TCP:22 DENY")

	// Identity 300: Ingress TCP 모든 포트 허용
	policyMap.Allow(300, Ingress, 6, 0)
	fmt.Println("    + id=300 Ingress TCP:* ALLOW (포트 와일드카드)")

	fmt.Println("\n  [Policy] 정책 조회 (LPM 매칭 시뮬레이션):")

	testCases := []struct {
		id    uint32
		dir   TrafficDirection
		proto uint8
		port  uint16
		desc  string
	}{
		{100, Ingress, 6, 80, "id=100 Ingress TCP:80"},
		{100, Ingress, 6, 443, "id=100 Ingress TCP:443"},
		{100, Ingress, 6, 8080, "id=100 Ingress TCP:8080 (정책 없음)"},
		{200, Egress, 6, 443, "id=200 Egress TCP:443 (와일드카드 매칭)"},
		{200, Egress, 17, 53, "id=200 Egress UDP:53 (와일드카드 매칭)"},
		{300, Ingress, 6, 22, "id=300 Ingress TCP:22 (DENY 정확히 매칭)"},
		{300, Ingress, 6, 80, "id=300 Ingress TCP:80 (포트 와일드카드 매칭)"},
		{999, Ingress, 6, 80, "id=999 Ingress TCP:80 (정책 없음)"},
	}

	for _, tc := range testCases {
		entry, found := policyMap.Lookup(tc.id, tc.dir, tc.proto, tc.port)
		if found {
			fmt.Printf("    %s -> %s\n", tc.desc, entry.String())
		} else {
			fmt.Printf("    %s -> DROP (기본 거부)\n", tc.desc)
		}
	}

	fmt.Println("\n  [Policy] 맵 덤프:")
	fmt.Print(policyMap.Dump())

	// ==========================================
	// 시나리오 3: Metrics Map — Per-CPU 카운터
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 3: Metrics Map (Per-CPU 카운터)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  메트릭 맵은 패킷 포워딩/드롭 통계를 수집한다.")
	fmt.Println("  - 맵 이름: cilium_metrics")
	fmt.Println("  - Per-CPU: 각 CPU가 독립적으로 카운터 갱신 (lock-free)")
	fmt.Println("  - 유저스페이스에서 읽을 때 모든 CPU 값을 합산")
	fmt.Println("  - Prometheus Collector(metricsmapCollector)로 노출")
	fmt.Println()

	numCPUs := 4
	metricsMap := NewPerCPUMetricsMap("cilium_metrics", numCPUs, 1024)

	// 패킷 처리 시뮬레이션 — 각 CPU가 독립적으로 카운터 갱신
	fmt.Println("  [Metrics] 패킷 처리 시뮬레이션 (4 CPU):")

	// Ingress 포워딩 (4개 CPU에서 병렬 처리)
	fwdIngress := MetricsKey{Reason: MetricReasonForwarded, Dir: MetricDirIngress}
	for cpu := 0; cpu < numCPUs; cpu++ {
		pkts := uint64(rand.Intn(10000) + 1000)
		bytes := pkts * uint64(rand.Intn(1400)+64)
		metricsMap.Increment(fwdIngress, cpu, pkts, bytes)
		fmt.Printf("    CPU%d: Ingress 포워딩 %d pkts, %d bytes\n", cpu, pkts, bytes)
	}

	// Egress 포워딩
	fwdEgress := MetricsKey{Reason: MetricReasonForwarded, Dir: MetricDirEgress}
	for cpu := 0; cpu < numCPUs; cpu++ {
		pkts := uint64(rand.Intn(8000) + 500)
		bytes := pkts * uint64(rand.Intn(1200)+100)
		metricsMap.Increment(fwdEgress, cpu, pkts, bytes)
	}

	// 정책 드롭
	dropPolicy := MetricsKey{Reason: MetricReasonDropPolicy, Dir: MetricDirIngress}
	for cpu := 0; cpu < numCPUs; cpu++ {
		pkts := uint64(rand.Intn(100) + 1)
		bytes := pkts * uint64(rand.Intn(200)+64)
		metricsMap.Increment(dropPolicy, cpu, pkts, bytes)
	}

	// Per-CPU 값 합산 및 출력
	fmt.Println("\n  [Metrics] Per-CPU 합산 결과:")
	totals := metricsMap.ReadAll()
	for key, total := range totals {
		fmt.Printf("    %s -> total_pkts=%d total_bytes=%d\n",
			key.String(), total.Count, total.Bytes)
	}

	// Prometheus Collector 스타일 출력
	fmt.Println("\n  [Metrics] Prometheus 메트릭 형식 (metricsmapCollector 출력):")
	metricsMap.IterateWithCallback(func(key MetricsKey, values []MetricsValue) {
		var totalCount, totalBytes uint64
		for _, v := range values {
			totalCount += v.Count
			totalBytes += v.Bytes
		}

		dirMap := map[uint8]string{1: "ingress", 2: "egress", 3: "service"}
		dir := dirMap[key.Dir]
		if dir == "" {
			dir = "unknown"
		}

		if key.Reason == MetricReasonForwarded {
			fmt.Printf("    cilium_forward_count_total{direction=\"%s\"} %d\n", dir, totalCount)
			fmt.Printf("    cilium_forward_bytes_total{direction=\"%s\"} %d\n", dir, totalBytes)
		} else {
			fmt.Printf("    cilium_drop_count_total{reason=\"POLICY_DENIED\",direction=\"%s\"} %d\n",
				dir, totalCount)
			fmt.Printf("    cilium_drop_bytes_total{reason=\"POLICY_DENIED\",direction=\"%s\"} %d\n",
				dir, totalBytes)
		}
	})

	// ==========================================
	// 시나리오 4: 맵 타입 비교 요약
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 4: Cilium BPF 맵 타입 비교")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println(`
  +-------------------+----------------+--------------------+----------------------+
  | 맵 종류           | BPF 타입       | 맵 이름            | 용도                 |
  +-------------------+----------------+--------------------+----------------------+
  | CT Map            | LRU_HASH       | cilium_ct4_global  | 연결 추적, NAT       |
  | Policy Map        | HASH (LPM)     | cilium_policy_v2_  | 패킷 허용/거부 결정  |
  | Metrics Map       | PERCPU_HASH    | cilium_metrics     | 포워딩/드롭 통계     |
  | NAT Map           | LRU_HASH       | cilium_snat_v4     | SNAT/DNAT 변환       |
  | LB Service Map    | HASH           | cilium_lb4_svcs    | 서비스->백엔드 매핑  |
  | IPCACHE Map       | LPM_TRIE       | cilium_ipcache     | IP->Identity 매핑    |
  | Events Map        | PERF_EVENT     | cilium_events      | 모니터 이벤트 전달   |
  +-------------------+----------------+--------------------+----------------------+

  CT Map GC 흐름 (gc/gc.go):
  +---------------+     +---------------+     +----------------+
  | GC Timer      |---->| CT Map 순회   |---->| 만료 엔트리    |
  | (30초 주기)   |     | (전체 스캔)   |     | 삭제           |
  +---------------+     +---------------+     +-------+--------+
                                                      |
                                              +-------v--------+
                                              | NAT Map 정리   |
                                              | (연관 엔트리)  |
                                              +----------------+

  Per-CPU 카운터 동작:
  +-------+  +-------+  +-------+  +-------+
  | CPU 0 |  | CPU 1 |  | CPU 2 |  | CPU 3 |    커널 공간
  | +1    |  | +1    |  | +1    |  | +1    |    (lock-free)
  +---+---+  +---+---+  +---+---+  +---+---+
      |          |          |          |
      +----------+----------+----------+
                     |
               +-----v-----+
               |  합산     |    유저스페이스
               |  total=4  |    (읽기 시 합산)
               +-----------+`)

	fmt.Println("\n\n  BPF 맵 시뮬레이션 완료.")
}
