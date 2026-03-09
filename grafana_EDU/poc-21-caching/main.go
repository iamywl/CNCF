package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// =============================================================================
// Grafana 캐싱 시스템 PoC
//
// 이 PoC는 Grafana의 캐싱 시스템 핵심 개념을 시뮬레이션한다:
//   1. 멀티 레이어 캐시 (LocalCache, RemoteCache)
//   2. 쿼리 결과 캐싱 (CachingService 패턴)
//   3. SHA256 기반 캐시 키 생성
//   4. 캐시 데코레이터 (접두사, 암호화)
//   5. Exclusive Lock으로 동시 쓰기 방지
//   6. TTL 기반 만료 및 캐시 메트릭
//
// 실제 소스 참조:
//   - pkg/services/caching/service.go         (CachingService, CachingServiceClient)
//   - pkg/infra/localcache/cache.go           (LocalCache)
//   - pkg/infra/remotecache/remotecache.go    (RemoteCache, CacheStorage)
// =============================================================================

// --- 캐시 상태 (pkg/services/caching/service.go 참조) ---

type CacheStatus string

const (
	StatusHit      CacheStatus = "HIT"
	StatusMiss     CacheStatus = "MISS"
	StatusBypass   CacheStatus = "BYPASS"
	StatusError    CacheStatus = "ERROR"
	StatusDisabled CacheStatus = "DISABLED"
)

// --- LocalCache 구현 (pkg/infra/localcache/cache.go 참조) ---

type cacheItem struct {
	value     []byte
	expiresAt time.Time
}

// LocalCache는 프로세스 내 메모리 캐시를 시뮬레이션한다.
// 실제 Grafana는 go-cache 라이브러리를 사용하지만, 핵심 개념은 동일하다.
type LocalCache struct {
	mu             sync.RWMutex
	items          map[string]*cacheItem
	defaultExpiry  time.Duration
	cleanupTicker  *time.Ticker

	// Exclusive lock 지원 (pkg/infra/localcache/cache.go의 lockableEntry 참조)
	lockMu sync.Mutex
	locks  map[string]*lockableEntry
}

type lockableEntry struct {
	sync.Mutex
	holds int
}

func NewLocalCache(defaultExpiry, cleanupInterval time.Duration) *LocalCache {
	c := &LocalCache{
		items:         make(map[string]*cacheItem),
		defaultExpiry: defaultExpiry,
		locks:         make(map[string]*lockableEntry),
	}

	// 백그라운드 정리는 시뮬레이션에서 생략 (실제는 ticker로 주기적 정리)
	return c
}

func (c *LocalCache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, ok := c.items[key]
	if !ok {
		return nil, false
	}

	// TTL 만료 확인
	if !item.expiresAt.IsZero() && time.Now().After(item.expiresAt) {
		return nil, false
	}

	return item.value, true
}

func (c *LocalCache) Set(key string, value []byte, duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if duration == 0 {
		duration = c.defaultExpiry
	}

	c.items[key] = &cacheItem{
		value:     value,
		expiresAt: time.Now().Add(duration),
	}
}

func (c *LocalCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// Lock은 키별 배타적 잠금을 획득한다.
// 실제 Grafana의 ExclusiveSet 패턴을 구현한다.
func (c *LocalCache) Lock(key string) func() {
	c.lockMu.Lock()
	entry, ok := c.locks[key]
	if !ok {
		entry = &lockableEntry{}
		c.locks[key] = entry
	}
	entry.holds++
	c.lockMu.Unlock()

	entry.Lock()

	return func() {
		entry.Unlock()
		c.lockMu.Lock()
		entry.holds--
		if entry.holds == 0 {
			delete(c.locks, key)
		}
		c.lockMu.Unlock()
	}
}

// ExclusiveSet은 Lock을 획득한 후 값을 계산하여 저장한다.
func (c *LocalCache) ExclusiveSet(key string, getValue func() ([]byte, error), dur time.Duration) error {
	unlock := c.Lock(key)
	defer unlock()

	v, err := getValue()
	if err != nil {
		return err
	}

	c.Set(key, v, dur)
	return nil
}

// Count는 현재 캐시 항목 수를 반환한다.
func (c *LocalCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// --- RemoteCache 구현 (pkg/infra/remotecache/remotecache.go 참조) ---

// CacheStorage는 Grafana의 RemoteCache 백엔드 인터페이스이다.
type CacheStorage interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte, expire time.Duration) error
	Delete(key string) error
}

// InMemoryRemoteCache는 Redis/Memcached를 인메모리로 시뮬레이션한다.
type InMemoryRemoteCache struct {
	mu    sync.RWMutex
	items map[string]*cacheItem
}

func NewInMemoryRemoteCache() *InMemoryRemoteCache {
	return &InMemoryRemoteCache{
		items: make(map[string]*cacheItem),
	}
}

func (c *InMemoryRemoteCache) Get(key string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, ok := c.items[key]
	if !ok {
		return nil, fmt.Errorf("cache item not found")
	}

	if !item.expiresAt.IsZero() && time.Now().After(item.expiresAt) {
		return nil, fmt.Errorf("cache item expired")
	}

	return item.value, nil
}

func (c *InMemoryRemoteCache) Set(key string, value []byte, expire time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items[key] = &cacheItem{
		value:     value,
		expiresAt: time.Now().Add(expire),
	}
	return nil
}

func (c *InMemoryRemoteCache) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	return nil
}

// --- 캐시 데코레이터 (pkg/infra/remotecache/remotecache.go 참조) ---

// PrefixCache는 모든 키에 접두사를 추가하는 데코레이터이다.
type PrefixCache struct {
	inner  CacheStorage
	prefix string
}

func (c *PrefixCache) Get(key string) ([]byte, error) {
	return c.inner.Get(c.prefix + key)
}

func (c *PrefixCache) Set(key string, value []byte, expire time.Duration) error {
	return c.inner.Set(c.prefix+key, value, expire)
}

func (c *PrefixCache) Delete(key string) error {
	return c.inner.Delete(c.prefix + key)
}

// XOREncryptedCache는 암호화 데코레이터를 시뮬레이션한다.
// 실제 Grafana는 AES를 사용하지만, 여기서는 XOR로 간단히 시뮬레이션.
type XOREncryptedCache struct {
	inner CacheStorage
	key   byte
}

func (c *XOREncryptedCache) encrypt(data []byte) []byte {
	result := make([]byte, len(data))
	for i, b := range data {
		result[i] = b ^ c.key
	}
	return result
}

func (c *XOREncryptedCache) decrypt(data []byte) []byte {
	return c.encrypt(data) // XOR은 자기 역원
}

func (c *XOREncryptedCache) Get(key string) ([]byte, error) {
	data, err := c.inner.Get(key)
	if err != nil {
		return nil, err
	}
	return c.decrypt(data), nil
}

func (c *XOREncryptedCache) Set(key string, value []byte, expire time.Duration) error {
	return c.inner.Set(key, c.encrypt(value), expire)
}

func (c *XOREncryptedCache) Delete(key string) error {
	return c.inner.Delete(key)
}

// --- 캐시 키 생성 (pkg/services/caching/service.go 참조) ---

// GenerateCacheKey는 쿼리 객체에서 SHA256 기반 캐시 키를 생성한다.
func GenerateCacheKey(namespace, prefix string, query interface{}) (string, error) {
	data, err := json.Marshal(query)
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256(data)
	hexHash := hex.EncodeToString(hash[:])

	if namespace != "" {
		return fmt.Sprintf("%s:%s:%s", namespace, prefix, hexHash), nil
	}
	return fmt.Sprintf("%s:%s", prefix, hexHash), nil
}

// --- 쿼리 캐싱 서비스 (pkg/services/caching/service.go 참조) ---

// QueryDataRequest는 데이터소스 쿼리 요청을 시뮬레이션한다.
type QueryDataRequest struct {
	DatasourceType string
	Expression     string
	TimeRange      struct {
		From time.Time
		To   time.Time
	}
}

// QueryDataResponse는 쿼리 결과를 시뮬레이션한다.
type QueryDataResponse struct {
	Data []float64
}

// CacheMetrics는 캐시 성능 메트릭을 추적한다.
type CacheMetrics struct {
	mu        sync.Mutex
	hits      int
	misses    int
	bypasses  int
	errors    int
	durations []time.Duration
}

func (m *CacheMetrics) Record(status CacheStatus, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch status {
	case StatusHit:
		m.hits++
	case StatusMiss:
		m.misses++
	case StatusBypass:
		m.bypasses++
	case StatusError:
		m.errors++
	}
	m.durations = append(m.durations, duration)
}

func (m *CacheMetrics) Print() {
	m.mu.Lock()
	defer m.mu.Unlock()

	total := m.hits + m.misses + m.bypasses + m.errors
	hitRate := float64(0)
	if total > 0 {
		hitRate = float64(m.hits) / float64(total) * 100
	}

	var avgDuration time.Duration
	if len(m.durations) > 0 {
		var sum time.Duration
		for _, d := range m.durations {
			sum += d
		}
		avgDuration = sum / time.Duration(len(m.durations))
	}

	fmt.Printf("  캐시 메트릭:\n")
	fmt.Printf("    총 요청: %d\n", total)
	fmt.Printf("    HIT: %d, MISS: %d, BYPASS: %d, ERROR: %d\n", m.hits, m.misses, m.bypasses, m.errors)
	fmt.Printf("    히트율: %.1f%%\n", hitRate)
	fmt.Printf("    평균 응답시간: %v\n", avgDuration)
}

// QueryCachingService는 CachingServiceClient의 WithQueryDataCaching 패턴을 구현한다.
type QueryCachingService struct {
	cache     CacheStorage
	metrics   *CacheMetrics
	ttl       time.Duration
	namespace string
}

func NewQueryCachingService(cache CacheStorage, namespace string, ttl time.Duration) *QueryCachingService {
	return &QueryCachingService{
		cache:     cache,
		metrics:   &CacheMetrics{},
		ttl:       ttl,
		namespace: namespace,
	}
}

// WithQueryDataCaching은 캐시를 확인하고 미스 시 queryFn을 실행하여 결과를 캐시한다.
// 실제 Grafana의 CachingServiceClient.WithQueryDataCaching() 패턴을 구현한다.
func (s *QueryCachingService) WithQueryDataCaching(
	req *QueryDataRequest,
	queryFn func() (*QueryDataResponse, error),
) (*QueryDataResponse, CacheStatus, error) {
	start := time.Now()

	// 1. 캐시 키 생성
	cacheKey, err := GenerateCacheKey(s.namespace, "query", req)
	if err != nil {
		s.metrics.Record(StatusError, time.Since(start))
		return nil, StatusError, err
	}

	// 2. 캐시 확인
	if cached, err := s.cache.Get(cacheKey); err == nil {
		var resp QueryDataResponse
		if err := json.Unmarshal(cached, &resp); err == nil {
			duration := time.Since(start)
			s.metrics.Record(StatusHit, duration)
			return &resp, StatusHit, nil
		}
	}

	// 3. 캐시 미스 → 실제 쿼리 실행
	resp, err := queryFn()
	if err != nil {
		s.metrics.Record(StatusError, time.Since(start))
		return nil, StatusError, err
	}

	// 4. 결과 캐싱
	if data, err := json.Marshal(resp); err == nil {
		s.cache.Set(cacheKey, data, s.ttl)
	}

	duration := time.Since(start)
	s.metrics.Record(StatusMiss, duration)
	return resp, StatusMiss, nil
}

// --- 메인 실행 ---

func main() {
	fmt.Println("=== Grafana 캐싱 시스템 PoC ===")
	fmt.Println()

	// -------------------------------------------------------
	// 1. LocalCache 기본 동작
	// -------------------------------------------------------
	fmt.Println("--- [1] LocalCache (인메모리 캐시) ---")

	localCache := NewLocalCache(5*time.Minute, 10*time.Minute)

	localCache.Set("user:42", []byte(`{"name":"Alice","role":"Admin"}`), 1*time.Minute)
	localCache.Set("user:43", []byte(`{"name":"Bob","role":"Editor"}`), 1*time.Minute)

	if val, ok := localCache.Get("user:42"); ok {
		fmt.Printf("  캐시 HIT: user:42 = %s\n", string(val))
	}

	if _, ok := localCache.Get("user:999"); !ok {
		fmt.Printf("  캐시 MISS: user:999 (없는 키)\n")
	}

	fmt.Printf("  캐시 항목 수: %d\n", localCache.Count())
	fmt.Println()

	// -------------------------------------------------------
	// 2. Exclusive Lock 패턴
	// -------------------------------------------------------
	fmt.Println("--- [2] Exclusive Lock 패턴 ---")

	var wg sync.WaitGroup
	computeCount := 0

	// 동일한 키에 대해 여러 고루틴이 동시에 값을 계산하려 할 때,
	// ExclusiveSet은 한 번만 계산하도록 보장한다.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			localCache.ExclusiveSet("expensive_key", func() ([]byte, error) {
				computeCount++
				fmt.Printf("  고루틴 %d: 값 계산 실행 (비용이 큰 연산)\n", id)
				time.Sleep(10 * time.Millisecond) // 연산 시뮬레이션
				return []byte(`{"computed":"result"}`), nil
			}, 5*time.Minute)
		}(i)
	}
	wg.Wait()

	if val, ok := localCache.Get("expensive_key"); ok {
		fmt.Printf("  최종 캐시 값: %s\n", string(val))
	}
	fmt.Println()

	// -------------------------------------------------------
	// 3. RemoteCache + 데코레이터 체인
	// -------------------------------------------------------
	fmt.Println("--- [3] RemoteCache + 데코레이터 ---")

	// 기본 스토리지 생성
	baseStorage := NewInMemoryRemoteCache()

	// 데코레이터 체인: 기본 → 접두사 → 암호화
	prefixedCache := &PrefixCache{inner: baseStorage, prefix: "grafana:"}
	encryptedCache := &XOREncryptedCache{inner: prefixedCache, key: 0x42}

	// 암호화된 캐시에 데이터 저장
	testData := []byte(`{"secret":"sensitive_data","metrics":[1,2,3]}`)
	encryptedCache.Set("session:abc", testData, 24*time.Hour)
	fmt.Printf("  원본 데이터: %s\n", string(testData))

	// 직접 기본 스토리지에서 읽으면 암호화된 데이터가 보인다
	if raw, err := baseStorage.Get("grafana:session:abc"); err == nil {
		fmt.Printf("  기본 스토리지(암호화됨): %x...\n", raw[:20])
	}

	// 암호화된 캐시를 통해 읽으면 원본이 복원된다
	if decrypted, err := encryptedCache.Get("session:abc"); err == nil {
		fmt.Printf("  복호화된 데이터: %s\n", string(decrypted))
	}
	fmt.Println()

	// -------------------------------------------------------
	// 4. SHA256 캐시 키 생성
	// -------------------------------------------------------
	fmt.Println("--- [4] SHA256 캐시 키 생성 ---")

	query1 := map[string]string{"expr": `rate(http_requests_total[5m])`, "ds": "prometheus"}
	query2 := map[string]string{"expr": `rate(http_requests_total[1m])`, "ds": "prometheus"}
	query3 := map[string]string{"expr": `rate(http_requests_total[5m])`, "ds": "prometheus"}

	key1, _ := GenerateCacheKey("org1", "query", query1)
	key2, _ := GenerateCacheKey("org1", "query", query2)
	key3, _ := GenerateCacheKey("org1", "query", query3)

	// 다른 Org의 동일 쿼리
	key4, _ := GenerateCacheKey("org2", "query", query1)

	fmt.Printf("  쿼리1 키: %s\n", key1[:50]+"...")
	fmt.Printf("  쿼리2 키: %s\n", key2[:50]+"...")
	fmt.Printf("  쿼리3 키: %s\n", key3[:50]+"...")
	fmt.Printf("  쿼리1 (org2) 키: %s\n", key4[:50]+"...")
	fmt.Printf("  쿼리1 == 쿼리3 (동일 쿼리): %v\n", key1 == key3)
	fmt.Printf("  쿼리1 != 쿼리2 (다른 쿼리): %v\n", key1 != key2)
	fmt.Printf("  org1 != org2 (다른 조직): %v\n", key1 != key4)
	fmt.Println()

	// -------------------------------------------------------
	// 5. 쿼리 캐싱 서비스 (WithQueryDataCaching 패턴)
	// -------------------------------------------------------
	fmt.Println("--- [5] 쿼리 캐싱 서비스 ---")

	queryCache := NewQueryCachingService(
		NewInMemoryRemoteCache(),
		"org1",
		1*time.Minute,
	)

	// 시뮬레이션 쿼리 요청
	req := &QueryDataRequest{
		DatasourceType: "prometheus",
		Expression:     `rate(http_requests_total{status="500"}[5m])`,
	}

	queryCount := 0
	queryFn := func() (*QueryDataResponse, error) {
		queryCount++
		time.Sleep(50 * time.Millisecond) // 데이터소스 쿼리 시뮬레이션
		return &QueryDataResponse{
			Data: []float64{100, 120, 95, 110, 105},
		}, nil
	}

	// 첫 번째 요청: MISS (캐시에 없으므로 실제 쿼리 실행)
	resp, status, _ := queryCache.WithQueryDataCaching(req, queryFn)
	fmt.Printf("  요청 1: status=%s, data=%v, 쿼리 실행 횟수=%d\n", status, resp.Data, queryCount)

	// 두 번째 요청: HIT (캐시에서 반환)
	resp, status, _ = queryCache.WithQueryDataCaching(req, queryFn)
	fmt.Printf("  요청 2: status=%s, data=%v, 쿼리 실행 횟수=%d\n", status, resp.Data, queryCount)

	// 세 번째 요청: HIT (캐시에서 반환)
	resp, status, _ = queryCache.WithQueryDataCaching(req, queryFn)
	fmt.Printf("  요청 3: status=%s, data=%v, 쿼리 실행 횟수=%d\n", status, resp.Data, queryCount)

	// 다른 쿼리: MISS
	req2 := &QueryDataRequest{
		DatasourceType: "prometheus",
		Expression:     `sum(rate(http_requests_total[5m])) by (method)`,
	}
	resp, status, _ = queryCache.WithQueryDataCaching(req2, queryFn)
	fmt.Printf("  요청 4 (다른 쿼리): status=%s, 쿼리 실행 횟수=%d\n", status, queryCount)

	fmt.Println()
	queryCache.metrics.Print()
	fmt.Println()

	// -------------------------------------------------------
	// 6. TTL 만료 시뮬레이션
	// -------------------------------------------------------
	fmt.Println("--- [6] TTL 만료 ---")

	shortTTLCache := NewLocalCache(100*time.Millisecond, 1*time.Second)

	shortTTLCache.Set("temp_key", []byte("temporary_value"), 200*time.Millisecond)

	if val, ok := shortTTLCache.Get("temp_key"); ok {
		fmt.Printf("  즉시 조회: %s (HIT)\n", string(val))
	}

	time.Sleep(250 * time.Millisecond)

	if _, ok := shortTTLCache.Get("temp_key"); !ok {
		fmt.Printf("  250ms 후 조회: MISS (TTL 만료)\n")
	}
	fmt.Println()

	// -------------------------------------------------------
	// 7. X-Cache 헤더 시뮬레이션
	// -------------------------------------------------------
	fmt.Println("--- [7] X-Cache 헤더 ---")

	headers := map[string]string{}

	simulateHTTPResponse := func(status CacheStatus) {
		headers["X-Cache"] = string(status)
		fmt.Printf("  HTTP 응답 헤더: X-Cache: %s\n", headers["X-Cache"])
	}

	simulateHTTPResponse(StatusMiss)
	simulateHTTPResponse(StatusHit)
	simulateHTTPResponse(StatusBypass)
	fmt.Println()

	fmt.Println("=== 캐싱 시스템 PoC 완료 ===")
}
