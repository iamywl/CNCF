package main

import (
	"fmt"
	"math/bits"
	"net"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium IPAM PoC
// =============================================================================
// Cilium의 IPAM(IP Address Management)을 시뮬레이션한다.
// 실제 Cilium에서는 pkg/ipam/ 아래에 다양한 IPAM 모드가 구현되어 있다:
//   - cluster-pool: 클러스터 범위 CIDR에서 노드별 PodCIDR 할당
//   - multi-pool: 여러 IP 풀에서 용도별로 할당
//   - eni: AWS ENI 기반 할당 (poc-15에서 다룸)
//
// 핵심 구현:
//   1. CIDR 기반 IP 할당 (비트맵 추적)
//   2. 다중 풀 지원 (Pool Manager)
//   3. Pre-allocation (사전 할당)
//   4. IP 해제 및 가비지 컬렉션
//   5. 할당 통계 (used/available/capacity)
// =============================================================================

// --- CIDR Pool (비트맵 기반 IP 할당) ---

// CIDRPool은 하나의 CIDR 범위에서 개별 IP를 관리한다.
// 비트맵으로 할당 상태를 추적하며, 이는 실제 Cilium의
// pkg/ipam/cidrset/cidr_set.go 구현을 단순화한 것이다.
type CIDRPool struct {
	cidr       *net.IPNet   // CIDR 범위
	baseIP     net.IP       // 네트워크 시작 주소
	totalIPs   int          // 사용 가능한 총 IP 수
	bitmap     []uint64     // 비트맵 (각 비트 = 하나의 IP 할당 상태)
	allocated  map[string]time.Time // 할당된 IP → 할당 시각
	released   map[string]time.Time // 해제된 IP → 해제 시각 (GC 대기)
	mu         sync.Mutex
	gcGrace    time.Duration // GC 유예 기간
}

// NewCIDRPool은 주어진 CIDR에서 IP 풀을 생성한다.
func NewCIDRPool(cidrStr string, gcGrace time.Duration) (*CIDRPool, error) {
	_, ipNet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return nil, fmt.Errorf("CIDR 파싱 실패: %w", err)
	}

	// 호스트 비트 수 계산 → 사용 가능한 IP 수
	ones, totalBits := ipNet.Mask.Size()
	hostBits := totalBits - ones
	totalIPs := (1 << hostBits) - 2 // 네트워크 주소와 브로드캐스트 제외

	if totalIPs <= 0 {
		return nil, fmt.Errorf("CIDR이 너무 작음: %s", cidrStr)
	}

	// 비트맵 크기 계산 (64비트 단위)
	bitmapSize := (totalIPs + 63) / 64

	return &CIDRPool{
		cidr:      ipNet,
		baseIP:    ipNet.IP.To4(),
		totalIPs:  totalIPs,
		bitmap:    make([]uint64, bitmapSize),
		allocated: make(map[string]time.Time),
		released:  make(map[string]time.Time),
		gcGrace:   gcGrace,
	}, nil
}

// ipAtOffset은 기본 주소에서 offset만큼 떨어진 IP를 반환한다.
func (p *CIDRPool) ipAtOffset(offset int) net.IP {
	// 네트워크 주소 + 1부터 시작 (네트워크 주소 자체는 사용 불가)
	ip := make(net.IP, 4)
	copy(ip, p.baseIP)
	val := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	val += uint32(offset + 1) // +1: 네트워크 주소 건너뛰기
	ip[0] = byte(val >> 24)
	ip[1] = byte(val >> 16)
	ip[2] = byte(val >> 8)
	ip[3] = byte(val)
	return ip
}

// Allocate는 사용 가능한 IP 하나를 할당한다.
// 비트맵에서 첫 번째 빈 비트를 찾아 설정한다.
func (p *CIDRPool) Allocate() (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 비트맵에서 빈 슬롯 찾기 (First Fit)
	for i := 0; i < len(p.bitmap); i++ {
		if p.bitmap[i] == ^uint64(0) {
			// 이 워드의 모든 비트가 사용 중
			continue
		}
		// 첫 번째 빈 비트 찾기
		// ^bitmap[i]의 trailing zeros = 첫 번째 0 비트 위치
		bit := bits.TrailingZeros64(^p.bitmap[i])
		offset := i*64 + bit

		if offset >= p.totalIPs {
			break // 범위 초과
		}

		// 비트 설정 (할당)
		p.bitmap[i] |= 1 << uint(bit)
		ip := p.ipAtOffset(offset)
		p.allocated[ip.String()] = time.Now()
		return ip, nil
	}

	return nil, fmt.Errorf("IP 풀 소진: %s", p.cidr)
}

// Release는 할당된 IP를 해제 대기 상태로 전환한다.
// 즉시 삭제하지 않고 GC 유예 기간 후 실제 해제된다.
func (p *CIDRPool) Release(ip net.IP) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	ipStr := ip.String()
	if _, exists := p.allocated[ipStr]; !exists {
		return fmt.Errorf("할당되지 않은 IP: %s", ipStr)
	}

	// 할당 맵에서 제거하고 해제 대기 맵으로 이동
	delete(p.allocated, ipStr)
	p.released[ipStr] = time.Now()
	return nil
}

// GarbageCollect는 유예 기간이 지난 해제 IP를 실제로 반환한다.
// 이는 Cilium에서 IP가 즉시 재사용되지 않도록 하는 안전장치이다.
// 다른 Pod에 같은 IP가 너무 빨리 할당되면 conntrack 충돌이 발생할 수 있다.
func (p *CIDRPool) GarbageCollect() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	collected := 0

	for ipStr, releasedAt := range p.released {
		if now.Sub(releasedAt) >= p.gcGrace {
			// 비트맵에서 비트 해제
			ip := net.ParseIP(ipStr).To4()
			offset := p.ipToOffset(ip)
			if offset >= 0 && offset < p.totalIPs {
				wordIdx := offset / 64
				bitIdx := uint(offset % 64)
				p.bitmap[wordIdx] &^= 1 << bitIdx
			}
			delete(p.released, ipStr)
			collected++
		}
	}
	return collected
}

// ipToOffset은 IP 주소를 비트맵 오프셋으로 변환한다.
func (p *CIDRPool) ipToOffset(ip net.IP) int {
	ip = ip.To4()
	base := uint32(p.baseIP[0])<<24 | uint32(p.baseIP[1])<<16 |
		uint32(p.baseIP[2])<<8 | uint32(p.baseIP[3])
	target := uint32(ip[0])<<24 | uint32(ip[1])<<16 |
		uint32(ip[2])<<8 | uint32(ip[3])
	return int(target-base) - 1 // -1: 네트워크 주소 보정
}

// Stats는 풀의 할당 통계를 반환한다.
func (p *CIDRPool) Stats() (used, releasing, available, capacity int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	used = len(p.allocated)
	releasing = len(p.released)
	available = p.totalIPs - used - releasing
	capacity = p.totalIPs
	return
}

// String은 풀 정보를 문자열로 반환한다.
func (p *CIDRPool) String() string {
	used, releasing, avail, cap := p.Stats()
	return fmt.Sprintf("CIDR=%s used=%d releasing=%d available=%d capacity=%d",
		p.cidr, used, releasing, avail, cap)
}

// --- Pool Manager (다중 풀 관리) ---

// PoolType은 IP 풀의 용도를 나타낸다.
type PoolType string

const (
	PoolDefault  PoolType = "default"      // 기본 Pod IP
	PoolExternal PoolType = "external"     // 외부 통신용
	PoolInternal PoolType = "internal-svc" // 내부 서비스용
)

// PoolManager는 여러 IP 풀을 관리한다.
// multi-pool IPAM 모드를 시뮬레이션한다.
type PoolManager struct {
	pools       map[PoolType]*CIDRPool
	preAllocate int // 사전 할당 수 (워밍업)
	mu          sync.Mutex
}

// NewPoolManager는 풀 매니저를 생성한다.
func NewPoolManager(preAllocate int) *PoolManager {
	return &PoolManager{
		pools:       make(map[PoolType]*CIDRPool),
		preAllocate: preAllocate,
	}
}

// AddPool은 새 풀을 추가한다.
func (pm *PoolManager) AddPool(poolType PoolType, cidr string, gcGrace time.Duration) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pool, err := NewCIDRPool(cidr, gcGrace)
	if err != nil {
		return err
	}
	pm.pools[poolType] = pool
	return nil
}

// AllocateFromPool은 지정된 풀에서 IP를 할당한다.
func (pm *PoolManager) AllocateFromPool(poolType PoolType) (net.IP, error) {
	pm.mu.Lock()
	pool, exists := pm.pools[poolType]
	pm.mu.Unlock()

	if !exists {
		return nil, fmt.Errorf("풀 없음: %s", poolType)
	}
	return pool.Allocate()
}

// ReleaseToPool은 지정된 풀로 IP를 해제한다.
func (pm *PoolManager) ReleaseToPool(poolType PoolType, ip net.IP) error {
	pm.mu.Lock()
	pool, exists := pm.pools[poolType]
	pm.mu.Unlock()

	if !exists {
		return fmt.Errorf("풀 없음: %s", poolType)
	}
	return pool.Release(ip)
}

// PreAllocate는 각 풀에서 IP를 사전 할당한다.
// 이는 ENI 모드에서 인터페이스를 미리 준비하는 것과 유사하다.
// 실제 필요할 때 API 지연 없이 즉시 할당할 수 있도록 한다.
func (pm *PoolManager) PreAllocate() map[PoolType][]net.IP {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	result := make(map[PoolType][]net.IP)
	for poolType, pool := range pm.pools {
		var ips []net.IP
		for i := 0; i < pm.preAllocate; i++ {
			ip, err := pool.Allocate()
			if err != nil {
				break
			}
			ips = append(ips, ip)
		}
		result[poolType] = ips
	}
	return result
}

// RunGC는 모든 풀에서 가비지 컬렉션을 실행한다.
func (pm *PoolManager) RunGC() map[PoolType]int {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	result := make(map[PoolType]int)
	for poolType, pool := range pm.pools {
		result[poolType] = pool.GarbageCollect()
	}
	return result
}

// DumpStats는 모든 풀의 통계를 출력한다.
func (pm *PoolManager) DumpStats() string {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-15s %-20s %6s %6s %6s %6s\n",
		"Pool", "CIDR", "Used", "Rlsing", "Avail", "Cap"))
	sb.WriteString(strings.Repeat("-", 75) + "\n")

	for poolType, pool := range pm.pools {
		used, releasing, avail, cap := pool.Stats()
		sb.WriteString(fmt.Sprintf("%-15s %-20s %6d %6d %6d %6d\n",
			poolType, pool.cidr, used, releasing, avail, cap))
	}
	return sb.String()
}

// =============================================================================
// main: IPAM 시뮬레이션
// =============================================================================

func main() {
	fmt.Println("=== Cilium IPAM PoC ===")
	fmt.Println("CIDR 기반 IP 할당, 다중 풀 관리, 가비지 컬렉션 시뮬레이션")
	fmt.Println()

	// --- 1. 단일 CIDR Pool 테스트 ---
	fmt.Println("[1] 단일 CIDR Pool 테스트 (10.0.1.0/28 = 14 IPs)")
	pool, err := NewCIDRPool("10.0.1.0/28", 0)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  생성: %s\n", pool)

	// IP 5개 할당
	var allocatedIPs []net.IP
	for i := 0; i < 5; i++ {
		ip, err := pool.Allocate()
		if err != nil {
			fmt.Printf("  할당 실패: %v\n", err)
			break
		}
		allocatedIPs = append(allocatedIPs, ip)
		fmt.Printf("  할당: %s\n", ip)
	}
	fmt.Printf("  상태: %s\n", pool)
	fmt.Println()

	// IP 2개 해제
	fmt.Println("  IP 해제 (GC 유예기간 없음):")
	for i := 0; i < 2; i++ {
		err := pool.Release(allocatedIPs[i])
		if err != nil {
			fmt.Printf("  해제 실패: %v\n", err)
		} else {
			fmt.Printf("  해제 요청: %s\n", allocatedIPs[i])
		}
	}
	fmt.Printf("  상태: %s\n", pool)

	// GC 실행
	collected := pool.GarbageCollect()
	fmt.Printf("  GC 수행: %d개 IP 회수\n", collected)
	fmt.Printf("  상태: %s\n", pool)
	fmt.Println()

	// 풀 소진 테스트
	fmt.Println("  풀 소진 테스트:")
	for i := 0; i < 15; i++ {
		ip, err := pool.Allocate()
		if err != nil {
			fmt.Printf("  할당 실패 (IP #%d): %v\n", i+6, err)
			break
		}
		fmt.Printf("  할당: %s\n", ip)
	}
	fmt.Println()

	// --- 2. Multi-Pool 모드 ---
	fmt.Println("[2] Multi-Pool 모드 시뮬레이션")
	fmt.Println("  (여러 풀을 용도별로 분리하여 관리)")
	fmt.Println()

	pm := NewPoolManager(3) // 풀당 3개 사전 할당

	// 풀 추가
	pm.AddPool(PoolDefault, "10.10.0.0/24", 50*time.Millisecond)
	pm.AddPool(PoolExternal, "10.20.0.0/24", 50*time.Millisecond)
	pm.AddPool(PoolInternal, "10.30.0.0/24", 50*time.Millisecond)

	fmt.Println("  풀 초기 상태:")
	fmt.Print(indent(pm.DumpStats(), "  "))
	fmt.Println()

	// --- 3. Pre-Allocation ---
	fmt.Println("[3] Pre-Allocation (사전 할당)")
	fmt.Println("  ENI 모드에서 인터페이스를 미리 준비하는 것과 유사.")
	fmt.Println("  실제 Pod 생성 요청 시 API 지연 없이 즉시 할당 가능.")
	fmt.Println()

	preAllocated := pm.PreAllocate()
	for poolType, ips := range preAllocated {
		ipStrs := make([]string, len(ips))
		for i, ip := range ips {
			ipStrs[i] = ip.String()
		}
		fmt.Printf("  [%s] 사전 할당: %s\n", poolType, strings.Join(ipStrs, ", "))
	}
	fmt.Println()

	fmt.Println("  사전 할당 후 상태:")
	fmt.Print(indent(pm.DumpStats(), "  "))
	fmt.Println()

	// --- 4. 동적 할당 및 해제 ---
	fmt.Println("[4] 동적 IP 할당 및 해제 시뮬레이션")

	// default 풀에서 10개 할당
	var defaultIPs []net.IP
	for i := 0; i < 10; i++ {
		ip, err := pm.AllocateFromPool(PoolDefault)
		if err != nil {
			fmt.Printf("  할당 실패: %v\n", err)
			break
		}
		defaultIPs = append(defaultIPs, ip)
	}
	fmt.Printf("  default 풀에서 %d개 추가 할당\n", len(defaultIPs))

	// external 풀에서 5개 할당
	var externalIPs []net.IP
	for i := 0; i < 5; i++ {
		ip, err := pm.AllocateFromPool(PoolExternal)
		if err != nil {
			break
		}
		externalIPs = append(externalIPs, ip)
	}
	fmt.Printf("  external 풀에서 %d개 추가 할당\n", len(externalIPs))

	fmt.Println()
	fmt.Println("  할당 후 상태:")
	fmt.Print(indent(pm.DumpStats(), "  "))
	fmt.Println()

	// 일부 해제
	fmt.Println("  default 풀에서 5개 해제 요청:")
	for i := 0; i < 5 && i < len(defaultIPs); i++ {
		pm.ReleaseToPool(PoolDefault, defaultIPs[i])
		fmt.Printf("    해제: %s\n", defaultIPs[i])
	}
	fmt.Println()
	fmt.Println("  해제 후 상태 (아직 GC 전):")
	fmt.Print(indent(pm.DumpStats(), "  "))
	fmt.Println()

	// --- 5. Garbage Collection ---
	fmt.Println("[5] Garbage Collection")
	fmt.Println("  유예 기간(50ms) 경과 대기...")
	time.Sleep(100 * time.Millisecond) // GC 유예기간 초과 대기

	gcResult := pm.RunGC()
	for poolType, count := range gcResult {
		if count > 0 {
			fmt.Printf("  [%s] GC 회수: %d개 IP\n", poolType, count)
		}
	}
	fmt.Println()
	fmt.Println("  GC 후 상태:")
	fmt.Print(indent(pm.DumpStats(), "  "))
	fmt.Println()

	// --- 6. 비트맵 시각화 ---
	fmt.Println("[6] 비트맵 할당 상태 시각화 (10.0.1.0/28)")
	vizPool, _ := NewCIDRPool("10.0.1.0/28", 0)

	// 특정 패턴으로 할당
	for i := 0; i < 5; i++ {
		vizPool.Allocate()
	}
	// 3번째 IP 해제하고 즉시 GC
	thirdIP := vizPool.ipAtOffset(2)
	vizPool.Release(thirdIP)
	vizPool.GarbageCollect()

	// 비트맵 출력
	fmt.Print("  비트맵: [")
	for i := 0; i < vizPool.totalIPs; i++ {
		wordIdx := i / 64
		bitIdx := uint(i % 64)
		if vizPool.bitmap[wordIdx]&(1<<bitIdx) != 0 {
			fmt.Print("1")
		} else {
			fmt.Print("0")
		}
	}
	fmt.Println("]")
	fmt.Println("  (1=할당, 0=미할당, 3번째 IP 해제 확인)")
	fmt.Println()

	// --- 구조 요약 ---
	fmt.Println("=== IPAM 구조 ===")
	fmt.Println()
	fmt.Println("  Pool Manager")
	fmt.Println("       │")
	fmt.Println("       ├── default    (10.10.0.0/24) ── CIDR Pool")
	fmt.Println("       │                                   ├── Bitmap [uint64 배열]")
	fmt.Println("       │                                   ├── Allocate() → First Fit")
	fmt.Println("       │                                   ├── Release() → GC 대기열")
	fmt.Println("       │                                   └── GarbageCollect() → 비트 해제")
	fmt.Println("       │")
	fmt.Println("       ├── external   (10.20.0.0/24) ── CIDR Pool")
	fmt.Println("       │")
	fmt.Println("       └── internal   (10.30.0.0/24) ── CIDR Pool")
	fmt.Println()
	fmt.Println("  Pre-Allocation: Pod 생성 전 미리 IP 확보 → 지연 최소화")
	fmt.Println("  GC Grace Period: conntrack 충돌 방지를 위한 IP 재사용 유예")
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}

// indent는 여러 줄 문자열에 접두사를 추가한다.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
