// poc-05-cache-server: CoreDNS 캐시 플러그인의 핵심 알고리즘 시뮬레이션
//
// CoreDNS cache 플러그인(plugin/cache/)의 이중 캐시(pcache/ncache),
// FNV 해시 키 생성, TTL 기반 만료, 프리페치 메커니즘을 재현한다.
//
// 실제 소스 참조:
//   - plugin/cache/cache.go: Cache 구조체, hash(), key(), computeTTL()
//   - plugin/cache/handler.go: ServeDNS(), getIfNotStale(), shouldPrefetch()
//   - plugin/cache/item.go: item 구조체, ttl(), toMsg()
//   - plugin/pkg/cache/cache.go: 256-shard 캐시 구현
//
// 실행: go run main.go

package main

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 1. 캐시 아이템 - plugin/cache/item.go의 item 구조체 재현
// ============================================================================

// DNSRecord는 캐시에 저장되는 DNS 레코드를 나타낸다
type DNSRecord struct {
	Name  string
	Type  uint16 // 1=A, 28=AAAA, 5=CNAME, 등
	Value string
	TTL   uint32
}

// CacheItem은 CoreDNS의 item 구조체를 재현한다
// 실제 소스: plugin/cache/item.go
type CacheItem struct {
	Name    string       // 쿼리 이름
	QType   uint16       // 쿼리 타입
	Rcode   int          // 응답 코드 (0=NoError, 3=NXDomain)
	Records []DNSRecord  // 응답 레코드들
	OrigTTL uint32       // 원본 TTL (초)
	Stored  time.Time    // 저장 시각
	Hits    atomic.Int64 // 조회 횟수 (프리페치 판단용)

	// 프리페치를 위한 빈도 추적
	// 실제 소스: plugin/cache/freq/freq.go
	lastAccess time.Time
}

// ttl은 현재 시각 기준으로 남은 TTL을 계산한다
// 실제 소스: plugin/cache/item.go의 func (i *item) ttl(now time.Time) int
func (i *CacheItem) ttl(now time.Time) int {
	return int(i.OrigTTL) - int(now.UTC().Sub(i.Stored).Seconds())
}

// updateHits는 접근 빈도를 갱신한다
func (i *CacheItem) updateHits(duration time.Duration, now time.Time) {
	i.Hits.Add(1)
	i.lastAccess = now
}

// ============================================================================
// 2. 샤드 캐시 - plugin/pkg/cache/cache.go의 256-shard 구현 재현
// ============================================================================

const shardSize = 256

// Shard는 뮤텍스로 보호되는 개별 캐시 파티션
// 실제 소스: plugin/pkg/cache/cache.go의 shard 구조체
type Shard struct {
	items map[uint64]*CacheItem
	size  int
	mu    sync.RWMutex
}

// Add는 샤드에 아이템을 추가한다. 가득 찼으면 랜덤 축출(eviction)한다.
// 실제 소스: plugin/pkg/cache/cache.go의 func (s *shard[T]) Add()
// CoreDNS는 가득 찼을 때 map 순회에서 첫 번째 항목을 삭제한다 (랜덤 축출)
func (s *Shard) Add(key uint64, item *CacheItem) bool {
	eviction := false
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) >= s.size {
		if _, ok := s.items[key]; !ok {
			// 기존 키가 아니면 하나를 축출
			for k := range s.items {
				delete(s.items, k)
				eviction = true
				break
			}
		}
	}
	s.items[key] = item
	return eviction
}

// Get은 키로 아이템을 조회한다
func (s *Shard) Get(key uint64) (*CacheItem, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[key]
	return item, ok
}

// Remove는 키에 해당하는 아이템을 제거한다
func (s *Shard) Remove(key uint64) {
	s.mu.Lock()
	delete(s.items, key)
	s.mu.Unlock()
}

// Len은 샤드의 현재 아이템 수를 반환한다
func (s *Shard) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// ShardedCache는 256개 샤드로 분할된 캐시
// 실제 소스: plugin/pkg/cache/cache.go의 Cache 구조체
type ShardedCache struct {
	shards [shardSize]*Shard
}

// NewShardedCache는 새 샤드 캐시를 생성한다
func NewShardedCache(totalSize int) *ShardedCache {
	shardCap := totalSize / shardSize
	if shardCap < 4 {
		shardCap = 4
	}
	c := &ShardedCache{}
	for i := range shardSize {
		c.shards[i] = &Shard{
			items: make(map[uint64]*CacheItem),
			size:  shardCap,
		}
	}
	return c
}

// Add는 키 해시의 하위 8비트로 샤드를 결정하고 아이템을 추가한다
func (c *ShardedCache) Add(key uint64, item *CacheItem) bool {
	shard := key & (shardSize - 1)
	return c.shards[shard].Add(key, item)
}

// Get은 키에 해당하는 아이템을 조회한다
func (c *ShardedCache) Get(key uint64) (*CacheItem, bool) {
	shard := key & (shardSize - 1)
	return c.shards[shard].Get(key)
}

// Remove는 키에 해당하는 아이템을 제거한다
func (c *ShardedCache) Remove(key uint64) {
	shard := key & (shardSize - 1)
	c.shards[shard].Remove(key)
}

// Len은 전체 캐시의 아이템 수를 반환한다
func (c *ShardedCache) Len() int {
	total := 0
	for _, s := range c.shards {
		total += s.Len()
	}
	return total
}

// ============================================================================
// 3. DNS 캐시 서버 - plugin/cache/cache.go의 Cache 구조체 재현
// ============================================================================

// 응답 분류 - plugin/pkg/response/typify.go 참조
const (
	ResponseNoError   = 0 // 성공 응답 → pcache
	ResponseNameError = 3 // NXDOMAIN → ncache
	ResponseNoData    = 4 // 데이터 없음 → ncache
)

// DNSCacheServer는 CoreDNS의 Cache 구조체를 재현한다
// 실제 소스: plugin/cache/cache.go
type DNSCacheServer struct {
	// 이중 캐시: 양성(positive) + 음성(negative)
	// 실제 소스에서 pcache는 성공 응답, ncache는 NXDOMAIN/NoData 응답을 저장
	pcache *ShardedCache // 양성 캐시 (성공 응답)
	ncache *ShardedCache // 음성 캐시 (NXDOMAIN, NoData)

	// TTL 설정
	pttl    time.Duration // 양성 캐시 최대 TTL (기본 3600초)
	minpttl time.Duration // 양성 캐시 최소 TTL (기본 5초)
	nttl    time.Duration // 음성 캐시 최대 TTL (기본 1800초)
	minnttl time.Duration // 음성 캐시 최소 TTL (기본 5초)

	// 프리페치 설정
	// 실제 소스: plugin/cache/handler.go의 shouldPrefetch()
	prefetchThreshold int           // 프리페치 시작 조건 (최소 히트 수)
	prefetchDuration  time.Duration // 빈도 측정 윈도우
	prefetchPercent   int           // TTL 대비 남은 비율 (%)

	// 통계
	stats CacheStats

	// 백엔드 (원본 DNS 조회)
	backend func(name string, qtype uint16) ([]DNSRecord, int)

	now func() time.Time
}

// CacheStats는 캐시 히트/미스 통계를 추적한다
type CacheStats struct {
	Requests   atomic.Int64
	Hits       atomic.Int64
	Misses     atomic.Int64
	PHits      atomic.Int64 // 양성 캐시 히트
	NHits      atomic.Int64 // 음성 캐시 히트
	Prefetches atomic.Int64
	Evictions  atomic.Int64
}

// NewDNSCacheServer는 기본 설정으로 캐시 서버를 생성한다
func NewDNSCacheServer(backend func(string, uint16) ([]DNSRecord, int)) *DNSCacheServer {
	return &DNSCacheServer{
		pcache:            NewShardedCache(10000),
		ncache:            NewShardedCache(10000),
		pttl:              3600 * time.Second,
		minpttl:           5 * time.Second,
		nttl:              1800 * time.Second,
		minnttl:           5 * time.Second,
		prefetchThreshold: 2,
		prefetchDuration:  1 * time.Minute,
		prefetchPercent:   10,
		backend:           backend,
		now:               time.Now,
	}
}

// hash는 qname + qtype + DO + CD 비트로 캐시 키를 생성한다
// 실제 소스: plugin/cache/cache.go의 func hash()
// CoreDNS는 FNV-64 해시를 사용한다
func hash(qname string, qtype uint16, do, cd bool) uint64 {
	h := fnv.New64()

	// DO 비트
	if do {
		h.Write([]byte("1"))
	} else {
		h.Write([]byte("0"))
	}

	// CD 비트
	if cd {
		h.Write([]byte("1"))
	} else {
		h.Write([]byte("0"))
	}

	// qtype (빅엔디안 2바이트)
	var qtypeBytes [2]byte
	binary.BigEndian.PutUint16(qtypeBytes[:], qtype)
	h.Write(qtypeBytes[:])

	// qname
	h.Write([]byte(strings.ToLower(qname)))

	return h.Sum64()
}

// computeTTL은 메시지 TTL을 최소/최대 범위로 클램프한다
// 실제 소스: plugin/cache/cache.go의 func computeTTL()
func computeTTL(msgTTL, minTTL, maxTTL time.Duration) time.Duration {
	if msgTTL < minTTL {
		return minTTL
	}
	if msgTTL > maxTTL {
		return maxTTL
	}
	return msgTTL
}

// Query는 DNS 쿼리를 처리한다 (캐시 조회 → 미스 시 백엔드 조회 → 캐시 저장)
// 실제 소스: plugin/cache/handler.go의 func (c *Cache) ServeDNS()
func (s *DNSCacheServer) Query(name string, qtype uint16) ([]DNSRecord, int, string) {
	s.stats.Requests.Add(1)
	now := s.now().UTC()
	name = strings.ToLower(name)

	k := hash(name, qtype, false, false)

	// 1. 음성 캐시 먼저 확인 (NXDOMAIN 캐싱)
	// 실제 소스: getIfNotStale()에서 ncache를 먼저 확인
	if item, ok := s.ncache.Get(k); ok {
		ttl := item.ttl(now)
		if ttl > 0 && strings.EqualFold(item.Name, name) && item.QType == qtype {
			s.stats.Hits.Add(1)
			s.stats.NHits.Add(1)
			item.updateHits(s.prefetchDuration, now)
			return nil, item.Rcode, "NCACHE_HIT"
		}
	}

	// 2. 양성 캐시 확인
	if item, ok := s.pcache.Get(k); ok {
		ttl := item.ttl(now)
		if ttl > 0 && strings.EqualFold(item.Name, name) && item.QType == qtype {
			s.stats.Hits.Add(1)
			s.stats.PHits.Add(1)
			item.updateHits(s.prefetchDuration, now)

			// 프리페치 판단
			// 실제 소스: handler.go의 shouldPrefetch()
			if s.shouldPrefetch(item, now) {
				go s.doPrefetch(name, qtype, item, now)
			}

			// TTL 조정된 레코드 반환
			result := make([]DNSRecord, len(item.Records))
			for i, r := range item.Records {
				result[i] = r
				result[i].TTL = uint32(ttl)
			}
			return result, item.Rcode, "PCACHE_HIT"
		}
	}

	// 3. 캐시 미스 → 백엔드 조회
	s.stats.Misses.Add(1)
	records, rcode := s.backend(name, qtype)

	// 4. 응답 캐싱
	s.cacheResponse(name, qtype, records, rcode, k, now)

	return records, rcode, "MISS"
}

// cacheResponse는 응답을 적절한 캐시에 저장한다
// 실제 소스: plugin/cache/cache.go의 func (w *ResponseWriter) set()
func (s *DNSCacheServer) cacheResponse(name string, qtype uint16, records []DNSRecord, rcode int, key uint64, now time.Time) {
	var duration time.Duration
	var msgTTL time.Duration

	if len(records) > 0 {
		msgTTL = time.Duration(records[0].TTL) * time.Second
	} else {
		msgTTL = 30 * time.Second // SOA minimum TTL 기본값
	}

	item := &CacheItem{
		Name:    name,
		QType:   qtype,
		Rcode:   rcode,
		Records: records,
		Stored:  now,
	}

	switch rcode {
	case ResponseNameError, ResponseNoData:
		// 음성 캐시에 저장
		duration = computeTTL(msgTTL, s.minnttl, s.nttl)
		item.OrigTTL = uint32(duration.Seconds())
		if s.ncache.Add(key, item) {
			s.stats.Evictions.Add(1)
		}

	case ResponseNoError:
		// 양성 캐시에 저장
		duration = computeTTL(msgTTL, s.minpttl, s.pttl)
		item.OrigTTL = uint32(duration.Seconds())
		if s.pcache.Add(key, item) {
			s.stats.Evictions.Add(1)
		}
	}
}

// shouldPrefetch는 프리페치 조건을 판단한다
// 실제 소스: plugin/cache/handler.go의 func (c *Cache) shouldPrefetch()
// 조건: 히트 수 >= 임계값 AND 남은 TTL <= origTTL의 percentage%
func (s *DNSCacheServer) shouldPrefetch(item *CacheItem, now time.Time) bool {
	if s.prefetchThreshold <= 0 {
		return false
	}
	threshold := int(math.Ceil(float64(s.prefetchPercent) / 100 * float64(item.OrigTTL)))
	return int(item.Hits.Load()) >= s.prefetchThreshold && item.ttl(now) <= threshold
}

// doPrefetch는 백그라운드에서 캐시 항목을 갱신한다
// 실제 소스: plugin/cache/handler.go의 func (c *Cache) doPrefetch()
func (s *DNSCacheServer) doPrefetch(name string, qtype uint16, oldItem *CacheItem, now time.Time) {
	s.stats.Prefetches.Add(1)
	records, rcode := s.backend(name, qtype)

	k := hash(name, qtype, false, false)
	s.cacheResponse(name, qtype, records, rcode, k, now)

	// 빈도 정보를 새 아이템에 복사
	// 실제 소스: handler.go doPrefetch()에서 i1.Reset(now, i.Hits())
	if newItem, ok := s.pcache.Get(k); ok {
		newItem.Hits.Store(oldItem.Hits.Load())
	}
}

// ============================================================================
// 4. 데모 실행
// ============================================================================

func main() {
	fmt.Println("=== CoreDNS 캐시 서버 PoC ===")
	fmt.Println()
	fmt.Println("CoreDNS 캐시 플러그인의 핵심 알고리즘을 시뮬레이션합니다.")
	fmt.Println("참조: plugin/cache/cache.go, handler.go, item.go, plugin/pkg/cache/cache.go")
	fmt.Println()

	// 백엔드 DNS 조회 함수 (지연 시뮬레이션)
	backendCalls := 0
	backend := func(name string, qtype uint16) ([]DNSRecord, int) {
		backendCalls++
		time.Sleep(10 * time.Millisecond) // 네트워크 지연 시뮬레이션

		// 알려진 도메인
		switch {
		case name == "example.com." && qtype == 1: // A 레코드
			return []DNSRecord{
				{Name: "example.com.", Type: 1, Value: "93.184.216.34", TTL: 300},
			}, ResponseNoError
		case name == "api.example.com." && qtype == 1:
			return []DNSRecord{
				{Name: "api.example.com.", Type: 1, Value: "10.0.1.1", TTL: 60},
				{Name: "api.example.com.", Type: 1, Value: "10.0.1.2", TTL: 60},
			}, ResponseNoError
		case name == "short-ttl.example.com." && qtype == 1:
			return []DNSRecord{
				{Name: "short-ttl.example.com.", Type: 1, Value: "10.0.2.1", TTL: 10},
			}, ResponseNoError
		case name == "nonexistent.example.com." && qtype == 1:
			return nil, ResponseNameError // NXDOMAIN
		default:
			return nil, ResponseNameError
		}
	}

	server := NewDNSCacheServer(backend)

	// ── 데모 1: 기본 캐시 동작 (히트/미스) ──
	fmt.Println("── 1. 기본 캐시 히트/미스 ──")
	fmt.Println()

	for i := 0; i < 5; i++ {
		records, rcode, status := server.Query("example.com.", 1)
		if i < 3 {
			if rcode == ResponseNoError && len(records) > 0 {
				fmt.Printf("  쿼리 %d: example.com A → %s (rcode=%d, %s)\n",
					i+1, records[0].Value, rcode, status)
			}
		}
	}
	fmt.Printf("  ... (총 5회 쿼리)\n")
	fmt.Printf("  백엔드 호출 횟수: %d (나머지는 캐시 히트)\n", backendCalls)
	fmt.Println()

	// ── 데모 2: 음성 캐시 (NXDOMAIN) ──
	fmt.Println("── 2. 음성 캐시 (NXDOMAIN 캐싱) ──")
	fmt.Println()

	prevBackendCalls := backendCalls
	for i := 0; i < 3; i++ {
		_, rcode, status := server.Query("nonexistent.example.com.", 1)
		fmt.Printf("  쿼리 %d: nonexistent.example.com A → rcode=%d (%s)\n",
			i+1, rcode, status)
	}
	fmt.Printf("  백엔드 호출: %d회 (음성 캐시가 반복 조회 방지)\n", backendCalls-prevBackendCalls)
	fmt.Println()

	// ── 데모 3: 캐시 키 해시 ──
	fmt.Println("── 3. FNV-64 캐시 키 해시 ──")
	fmt.Println()
	fmt.Println("  CoreDNS는 qname + qtype + DO + CD 비트를 FNV-64로 해시하여 캐시 키를 생성한다.")
	fmt.Println("  동일 도메인이라도 레코드 타입이나 DNSSEC 플래그가 다르면 별도 캐시 항목이 된다.")
	fmt.Println()

	testCases := []struct {
		name  string
		qtype uint16
		do    bool
		cd    bool
		label string
	}{
		{"example.com.", 1, false, false, "A, DO=false"},
		{"example.com.", 1, true, false, "A, DO=true"},
		{"example.com.", 28, false, false, "AAAA, DO=false"},
		{"EXAMPLE.COM.", 1, false, false, "A (대문자, DO=false)"},
	}

	for _, tc := range testCases {
		k := hash(tc.name, tc.qtype, tc.do, tc.cd)
		shard := k & (shardSize - 1)
		fmt.Printf("  %-30s → key=0x%016x, shard=%d\n", tc.label, k, shard)
	}
	// 대소문자 무시 확인
	k1 := hash("example.com.", 1, false, false)
	k2 := hash("EXAMPLE.COM.", 1, false, false)
	fmt.Printf("\n  대소문자 무시: %v (CoreDNS는 qname을 소문자로 정규화)\n", k1 == k2)
	fmt.Println()

	// ── 데모 4: TTL 감소 관찰 ──
	fmt.Println("── 4. TTL 감소 관찰 ──")
	fmt.Println()

	// 시간 제어를 위한 서버
	currentTime := time.Now()
	timedServer := NewDNSCacheServer(backend)
	timedServer.now = func() time.Time { return currentTime }

	timedServer.Query("api.example.com.", 1)
	fmt.Println("  api.example.com (원본 TTL=60초):")
	for _, offset := range []int{0, 15, 30, 45, 55, 60} {
		timedServer.now = func() time.Time { return currentTime.Add(time.Duration(offset) * time.Second) }
		records, _, status := timedServer.Query("api.example.com.", 1)
		if len(records) > 0 {
			fmt.Printf("  +%2d초: TTL=%d초 (%s)\n", offset, records[0].TTL, status)
		} else {
			fmt.Printf("  +%2d초: 만료됨 (%s)\n", offset, status)
		}
	}
	fmt.Println()

	// ── 데모 5: 프리페치 ──
	fmt.Println("── 5. 프리페치 메커니즘 ──")
	fmt.Println()
	fmt.Println("  CoreDNS 프리페치 조건 (plugin/cache/handler.go shouldPrefetch()):")
	fmt.Printf("  - 히트 수 >= %d (prefetch threshold)\n", timedServer.prefetchThreshold)
	fmt.Printf("  - 남은 TTL <= origTTL의 %d%%\n", timedServer.prefetchPercent)
	fmt.Println()

	currentTime = time.Now()
	prefetchServer := NewDNSCacheServer(backend)
	prefetchServer.now = func() time.Time { return currentTime }
	prefetchServer.prefetchThreshold = 2
	prefetchServer.prefetchPercent = 10

	// short-ttl.example.com: TTL=10초, 10%=1초 → 남은 TTL ≤ 1초일 때 프리페치
	prefetchServer.Query("short-ttl.example.com.", 1)  // 미스 → 캐시
	prefetchServer.Query("short-ttl.example.com.", 1)   // 히트 (hits=1)
	prefetchServer.Query("short-ttl.example.com.", 1)   // 히트 (hits=2)

	// TTL 10초에서 10%는 1초 → 남은 TTL ≤ 1초일 때 프리페치
	for _, offset := range []int{0, 5, 8, 9} {
		prefetchServer.now = func() time.Time { return currentTime.Add(time.Duration(offset) * time.Second) }
		records, _, status := prefetchServer.Query("short-ttl.example.com.", 1)
		shouldPrefetch := "아니오"
		// 프리페치 판단 재현
		k := hash("short-ttl.example.com.", 1, false, false)
		if item, ok := prefetchServer.pcache.Get(k); ok {
			if prefetchServer.shouldPrefetch(item, prefetchServer.now()) {
				shouldPrefetch = "예 (백그라운드 갱신)"
			}
		}
		if len(records) > 0 {
			fmt.Printf("  +%d초: TTL=%d초, 프리페치=%s (%s)\n",
				offset, records[0].TTL, shouldPrefetch, status)
		}
	}
	time.Sleep(50 * time.Millisecond) // 프리페치 고루틴 완료 대기
	fmt.Println()

	// ── 데모 6: 캐시 히트율 통계 ──
	fmt.Println("── 6. 캐시 히트율 통계 ──")
	fmt.Println()

	statsServer := NewDNSCacheServer(backend)
	domains := []string{
		"example.com.", "example.com.", "example.com.",
		"api.example.com.", "api.example.com.",
		"nonexistent.example.com.", "nonexistent.example.com.", "nonexistent.example.com.",
		"example.com.", "api.example.com.",
	}

	for _, d := range domains {
		statsServer.Query(d, 1)
	}

	requests := statsServer.stats.Requests.Load()
	hits := statsServer.stats.Hits.Load()
	misses := statsServer.stats.Misses.Load()
	phits := statsServer.stats.PHits.Load()
	nhits := statsServer.stats.NHits.Load()

	fmt.Printf("  총 요청:      %d\n", requests)
	fmt.Printf("  캐시 히트:    %d (양성=%d, 음성=%d)\n", hits, phits, nhits)
	fmt.Printf("  캐시 미스:    %d\n", misses)
	if requests > 0 {
		fmt.Printf("  히트율:       %.1f%%\n", float64(hits)/float64(requests)*100)
	}
	fmt.Printf("  양성 캐시:    %d 항목\n", statsServer.pcache.Len())
	fmt.Printf("  음성 캐시:    %d 항목\n", statsServer.ncache.Len())
	fmt.Println()

	// ── 데모 7: 샤드 분포 ──
	fmt.Println("── 7. 256-샤드 캐시 분포 ──")
	fmt.Println()
	fmt.Println("  CoreDNS는 캐시를 256개 샤드로 분할하여 락 경합을 줄인다.")
	fmt.Println("  샤드 선택: key & (256 - 1), 즉 키의 하위 8비트로 결정")
	fmt.Println()

	shardDist := make(map[uint64]int)
	testDomains := []string{
		"a.example.com.", "b.example.com.", "c.example.com.",
		"d.example.com.", "e.example.com.", "api.internal.com.",
		"web.internal.com.", "db.internal.com.", "cache.internal.com.",
	}
	for _, d := range testDomains {
		k := hash(d, 1, false, false)
		shard := k & (shardSize - 1)
		shardDist[shard]++
		fmt.Printf("  %-25s → shard %3d\n", d, shard)
	}
	fmt.Printf("\n  %d개 도메인이 %d개 샤드에 분산됨\n", len(testDomains), len(shardDist))
	fmt.Println()

	// ── 데모 8: computeTTL 클램프 ──
	fmt.Println("── 8. TTL 클램프 (computeTTL) ──")
	fmt.Println()
	fmt.Println("  CoreDNS는 응답의 TTL을 min/max 범위로 제한한다:")
	fmt.Printf("  양성 캐시: min=%v, max=%v\n", 5*time.Second, 3600*time.Second)
	fmt.Printf("  음성 캐시: min=%v, max=%v\n", 5*time.Second, 1800*time.Second)
	fmt.Println()

	ttlCases := []struct {
		msgTTL time.Duration
		minTTL time.Duration
		maxTTL time.Duration
		label  string
	}{
		{2 * time.Second, 5 * time.Second, 3600 * time.Second, "2초 (양성, min보다 작음)"},
		{300 * time.Second, 5 * time.Second, 3600 * time.Second, "300초 (양성, 범위 내)"},
		{86400 * time.Second, 5 * time.Second, 3600 * time.Second, "86400초 (양성, max 초과)"},
		{1 * time.Second, 5 * time.Second, 1800 * time.Second, "1초 (음성, min보다 작음)"},
	}
	for _, tc := range ttlCases {
		result := computeTTL(tc.msgTTL, tc.minTTL, tc.maxTTL)
		fmt.Printf("  %-35s → %v\n", tc.label, result)
	}
	fmt.Println()

	fmt.Println("=== PoC 완료 ===")
}
