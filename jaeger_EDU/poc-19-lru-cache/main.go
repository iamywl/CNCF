package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Jaeger LRU Cache with TTL 시뮬레이션
// =============================================================================
//
// Jaeger는 LRU(Least Recently Used) 캐시를 TTL과 함께 사용하여
// 서비스/오퍼레이션 이름, adaptive sampling 확률 등을 캐싱한다.
//
// 핵심:
//   - Double-linked list + HashMap = O(1) get/put
//   - TTL: 엔트리별 만료 시간
//   - Eviction: 용량 초과 시 LRU 항목 제거
//   - Thread-safe: 동시 접근 안전
//
// 실제 코드 참조:
//   - pkg/cache/lru.go (또는 유사 유틸리티)
// =============================================================================

// --- Double-Linked List Node ---

type node struct {
	key       string
	value     interface{}
	expiresAt time.Time
	prev      *node
	next      *node
}

// --- LRU Cache ---

type LRUCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	items    map[string]*node
	head     *node // most recently used
	tail     *node // least recently used
	stats    CacheStats
}

type CacheStats struct {
	Hits       int
	Misses     int
	Evictions  int
	Expired    int
	Insertions int
}

func NewLRUCache(capacity int, ttl time.Duration) *LRUCache {
	return &LRUCache{
		capacity: capacity,
		ttl:      ttl,
		items:    make(map[string]*node),
	}
}

// moveToFront는 노드를 리스트 앞(MRU)으로 이동한다.
func (c *LRUCache) moveToFront(n *node) {
	if c.head == n {
		return
	}
	c.removeNode(n)
	c.addToFront(n)
}

func (c *LRUCache) addToFront(n *node) {
	n.prev = nil
	n.next = c.head
	if c.head != nil {
		c.head.prev = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
}

func (c *LRUCache) removeNode(n *node) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		c.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		c.tail = n.prev
	}
}

func (c *LRUCache) removeLRU() *node {
	if c.tail == nil {
		return nil
	}
	lru := c.tail
	c.removeNode(lru)
	delete(c.items, lru.key)
	return lru
}

// Get은 캐시에서 값을 조회한다.
func (c *LRUCache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, ok := c.items[key]
	if !ok {
		c.stats.Misses++
		return nil, false
	}

	// TTL 검사
	if time.Now().After(n.expiresAt) {
		c.removeNode(n)
		delete(c.items, key)
		c.stats.Expired++
		c.stats.Misses++
		return nil, false
	}

	c.moveToFront(n)
	c.stats.Hits++
	return n.value, true
}

// Put은 캐시에 값을 저장한다.
func (c *LRUCache) Put(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if n, ok := c.items[key]; ok {
		n.value = value
		n.expiresAt = time.Now().Add(c.ttl)
		c.moveToFront(n)
		return
	}

	// 용량 초과 시 LRU 제거
	if len(c.items) >= c.capacity {
		evicted := c.removeLRU()
		if evicted != nil {
			c.stats.Evictions++
			fmt.Printf("    [EVICT] key=%s\n", evicted.key)
		}
	}

	n := &node{
		key:       key,
		value:     value,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.addToFront(n)
	c.items[key] = n
	c.stats.Insertions++
}

// Delete는 캐시에서 항목을 삭제한다.
func (c *LRUCache) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.items[key]
	if !ok {
		return false
	}
	c.removeNode(n)
	delete(c.items, key)
	return true
}

// Len은 현재 캐시 크기를 반환한다.
func (c *LRUCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// PurgeExpired는 만료된 항목을 모두 제거한다.
func (c *LRUCache) PurgeExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	purged := 0
	now := time.Now()
	for key, n := range c.items {
		if now.After(n.expiresAt) {
			c.removeNode(n)
			delete(c.items, key)
			purged++
			c.stats.Expired++
		}
	}
	return purged
}

// Keys는 MRU → LRU 순서로 키를 반환한다.
func (c *LRUCache) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var keys []string
	for n := c.head; n != nil; n = n.next {
		keys = append(keys, n.key)
	}
	return keys
}

func (c *LRUCache) PrintStats() {
	fmt.Printf("    Hits: %d, Misses: %d, Evictions: %d, Expired: %d, Insertions: %d\n",
		c.stats.Hits, c.stats.Misses, c.stats.Evictions, c.stats.Expired, c.stats.Insertions)
	hitRate := float64(0)
	total := c.stats.Hits + c.stats.Misses
	if total > 0 {
		hitRate = float64(c.stats.Hits) / float64(total) * 100
	}
	fmt.Printf("    Hit Rate: %.1f%%, Size: %d/%d\n", hitRate, c.Len(), c.capacity)
}

func main() {
	fmt.Println("=== Jaeger LRU Cache with TTL 시뮬레이션 ===")
	fmt.Println()

	// --- 기본 동작 ---
	fmt.Println("[1] 기본 LRU 동작 (capacity=5, TTL=1s)")
	fmt.Println(strings.Repeat("-", 60))

	cache := NewLRUCache(5, 1*time.Second)

	// 삽입
	services := []string{"frontend", "backend", "auth", "payment", "notification"}
	for _, svc := range services {
		cache.Put(svc, fmt.Sprintf("%s-data", svc))
		fmt.Printf("  PUT %s\n", svc)
	}
	fmt.Printf("  Keys (MRU->LRU): %v\n", cache.Keys())
	fmt.Println()

	// 조회 (MRU 업데이트)
	fmt.Println("[2] 조회로 MRU 업데이트")
	fmt.Println(strings.Repeat("-", 60))

	for _, key := range []string{"frontend", "auth"} {
		val, ok := cache.Get(key)
		fmt.Printf("  GET %s -> %v (found=%v)\n", key, val, ok)
	}
	fmt.Printf("  Keys (MRU->LRU): %v\n", cache.Keys())
	fmt.Println()

	// LRU 제거
	fmt.Println("[3] 용량 초과 → LRU 제거")
	fmt.Println(strings.Repeat("-", 60))

	for _, svc := range []string{"cache", "database", "gateway"} {
		cache.Put(svc, fmt.Sprintf("%s-data", svc))
		fmt.Printf("  PUT %s -> Keys: %v\n", svc, cache.Keys())
	}
	fmt.Println()

	// --- TTL 만료 ---
	fmt.Println("[4] TTL 만료 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	shortCache := NewLRUCache(10, 100*time.Millisecond)
	shortCache.Put("key-1", "val-1")
	shortCache.Put("key-2", "val-2")
	shortCache.Put("key-3", "val-3")

	val, ok := shortCache.Get("key-1")
	fmt.Printf("  즉시 조회: key-1=%v (found=%v)\n", val, ok)

	time.Sleep(150 * time.Millisecond) // TTL 만료 대기

	val, ok = shortCache.Get("key-1")
	fmt.Printf("  만료 후 조회: key-1=%v (found=%v)\n", val, ok)

	purged := shortCache.PurgeExpired()
	fmt.Printf("  PurgeExpired: %d items removed\n", purged)
	fmt.Println()

	// --- Jaeger 사용 시나리오: 서비스/오퍼레이션 캐시 ---
	fmt.Println("[5] Jaeger 시나리오: 서비스-오퍼레이션 캐시")
	fmt.Println(strings.Repeat("-", 60))

	opCache := NewLRUCache(20, 5*time.Second)

	// 서비스별 오퍼레이션 캐싱
	operations := map[string][]string{
		"frontend": {"HTTP GET /", "HTTP GET /api/v1/products", "HTTP POST /api/v1/order"},
		"backend":  {"ProcessOrder", "ValidatePayment", "SendNotification"},
		"auth":     {"ValidateToken", "RefreshToken", "RevokeToken"},
		"payment":  {"ChargeCard", "Refund", "GetBalance"},
	}

	for svc, ops := range operations {
		for _, op := range ops {
			key := svc + "::" + op
			opCache.Put(key, map[string]interface{}{
				"service":   svc,
				"operation": op,
				"count":     1,
			})
		}
	}

	fmt.Printf("  캐시 크기: %d/%d\n", opCache.Len(), 20)
	fmt.Println()

	// 핫 오퍼레이션 접근 패턴
	fmt.Println("[6] 핫 오퍼레이션 접근 패턴")
	fmt.Println(strings.Repeat("-", 60))

	hotOps := []string{
		"frontend::HTTP GET /",
		"frontend::HTTP GET /api/v1/products",
		"backend::ProcessOrder",
		"auth::ValidateToken",
	}
	coldOps := []string{
		"payment::Refund",
		"auth::RevokeToken",
	}

	// 핫 오퍼레이션 반복 접근
	for i := 0; i < 50; i++ {
		op := hotOps[i%len(hotOps)]
		opCache.Get(op)
	}
	// 콜드 오퍼레이션 가끔 접근
	for _, op := range coldOps {
		opCache.Get(op)
	}
	// 존재하지 않는 키
	for i := 0; i < 10; i++ {
		opCache.Get(fmt.Sprintf("unknown::op-%d", i))
	}

	fmt.Println("  통계:")
	opCache.PrintStats()
	fmt.Println()

	// --- Adaptive Sampling 확률 캐시 ---
	fmt.Println("[7] Adaptive Sampling 확률 캐시")
	fmt.Println(strings.Repeat("-", 60))

	samplingCache := NewLRUCache(100, 10*time.Second)
	probabilities := map[string]float64{
		"frontend/HTTP GET /":         0.01,
		"frontend/HTTP POST /order":   0.1,
		"backend/ProcessOrder":        0.5,
		"auth/ValidateToken":          0.001,
		"payment/ChargeCard":          1.0,
	}

	for key, prob := range probabilities {
		samplingCache.Put(key, prob)
	}

	for key := range probabilities {
		val, _ := samplingCache.Get(key)
		fmt.Printf("  %-35s -> probability=%.3f\n", key, val.(float64))
	}
	fmt.Println()

	// --- 최종 통계 ---
	fmt.Println("[8] 최종 캐시 통계")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  기본 캐시:")
	cache.PrintStats()
	fmt.Println("  오퍼레이션 캐시:")
	opCache.PrintStats()
	fmt.Println("  샘플링 캐시:")
	samplingCache.PrintStats()
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
