package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// =============================================================================
// Loki PoC #11: 쿼리 프론트엔드 - 쿼리 분할, 큐잉, 캐싱
// =============================================================================
//
// Loki의 쿼리 프론트엔드는 대규모 쿼리를 효율적으로 처리하기 위한
// 프록시 계층이다. 핵심 기능:
//   1. 쿼리 분할(Splitting): 큰 시간 범위를 작은 간격으로 분할
//   2. 테넌트별 큐잉(Queuing): 테넌트별 공정한 스케줄링
//   3. 결과 캐싱(Caching): 동일 쿼리 결과 재사용
//   4. 쿼리 중복 제거(Dedup): 동시에 들어온 동일 쿼리 합치기
//
// 실제 Loki 코드: pkg/lokifrontend/, pkg/querier/queryrange/
//
// 실행: go run main.go

// =============================================================================
// 1. 쿼리 요청/응답 정의
// =============================================================================

// QueryRequest는 로그 쿼리 요청이다.
type QueryRequest struct {
	ID       string    // 요청 고유 ID
	Tenant   string    // 테넌트 ID
	Query    string    // LogQL 쿼리
	Start    time.Time // 시작 시간
	End      time.Time // 종료 시간
	Limit    int       // 최대 결과 수
	Priority int       // 우선순위 (낮을수록 높은 우선순위)
}

// Hash는 요청의 고유 해시를 생성한다 (캐싱/중복 제거용).
func (r *QueryRequest) Hash() string {
	data := fmt.Sprintf("%s|%s|%d|%d",
		r.Tenant, r.Query, r.Start.UnixNano(), r.End.UnixNano())
	hash := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", hash[:8])
}

func (r *QueryRequest) String() string {
	duration := r.End.Sub(r.Start)
	return fmt.Sprintf("[%s] tenant=%s query=%q range=%v",
		r.ID, r.Tenant, r.Query, duration.Round(time.Second))
}

// QueryResponse는 쿼리 응답이다.
type QueryResponse struct {
	RequestID string
	Entries   []LogEntry
	FromCache bool   // 캐시에서 가져온 결과인지
	Duration  time.Duration
	Error     string
}

// LogEntry는 하나의 로그 엔트리이다.
type LogEntry struct {
	Timestamp time.Time
	Line      string
	Labels    string
}

// =============================================================================
// 2. 쿼리 분할 (Query Splitting)
// =============================================================================
// 큰 시간 범위의 쿼리를 작은 간격(기본 1시간)으로 분할한다.
// 각 분할된 쿼리는 병렬로 실행되어 성능이 향상된다.
//
// 예: 24시간 범위 → 24개의 1시간 쿼리로 분할
//
// Loki 실제 코드: pkg/querier/queryrange/split_by_interval.go

// SplitByInterval은 쿼리를 시간 간격으로 분할한다.
func SplitByInterval(req *QueryRequest, interval time.Duration) []*QueryRequest {
	var splits []*QueryRequest

	start := req.Start
	splitIdx := 0
	for start.Before(req.End) {
		end := start.Add(interval)
		if end.After(req.End) {
			end = req.End
		}

		splits = append(splits, &QueryRequest{
			ID:       fmt.Sprintf("%s-split-%d", req.ID, splitIdx),
			Tenant:   req.Tenant,
			Query:    req.Query,
			Start:    start,
			End:      end,
			Limit:    req.Limit,
			Priority: req.Priority,
		})

		start = end
		splitIdx++
	}

	return splits
}

// MergeResponses는 분할된 쿼리의 응답을 시간순으로 병합한다.
func MergeResponses(responses []*QueryResponse, limit int) *QueryResponse {
	merged := &QueryResponse{}

	// 모든 엔트리 수집
	for _, resp := range responses {
		if resp.Error != "" {
			merged.Error = resp.Error
			continue
		}
		merged.Entries = append(merged.Entries, resp.Entries...)
	}

	// 시간순 정렬
	sort.Slice(merged.Entries, func(i, j int) bool {
		return merged.Entries[i].Timestamp.Before(merged.Entries[j].Timestamp)
	})

	// Limit 적용
	if limit > 0 && len(merged.Entries) > limit {
		merged.Entries = merged.Entries[:limit]
	}

	return merged
}

// =============================================================================
// 3. 테넌트별 큐 (Per-Tenant Queue)
// =============================================================================
// 각 테넌트에 별도의 큐를 두어 공정한 스케줄링을 보장한다.
// 하나의 테넌트가 대량의 쿼리를 보내도 다른 테넌트에 영향을 주지 않는다.
//
// Loki 실제 코드: pkg/scheduler/queue/

// RequestItem은 큐에 들어가는 요청 아이템이다.
type RequestItem struct {
	Request  *QueryRequest
	ResultCh chan *QueryResponse // 결과를 돌려줄 채널
}

// TenantQueue는 테넌트별 요청 큐이다.
type TenantQueue struct {
	mu             sync.Mutex
	queues         map[string][]*RequestItem // 테넌트별 큐
	tenantOrder    []string                  // 라운드 로빈용 테넌트 순서
	currentIdx     int                       // 현재 라운드 로빈 인덱스
	maxOutstanding int                       // 테넌트당 최대 대기 요청 수
	notify         chan struct{}             // 새 요청 알림
	closed         bool
}

// NewTenantQueue는 새로운 테넌트별 큐를 생성한다.
func NewTenantQueue(maxOutstanding int) *TenantQueue {
	return &TenantQueue{
		queues:         make(map[string][]*RequestItem),
		maxOutstanding: maxOutstanding,
		notify:         make(chan struct{}, 1),
	}
}

// Enqueue는 요청을 테넌트 큐에 추가한다.
func (tq *TenantQueue) Enqueue(item *RequestItem) error {
	tq.mu.Lock()
	defer tq.mu.Unlock()

	if tq.closed {
		return fmt.Errorf("큐가 닫혔습니다")
	}

	tenant := item.Request.Tenant

	// 최대 대기 요청 수 확인
	if len(tq.queues[tenant]) >= tq.maxOutstanding {
		return fmt.Errorf("테넌트 %s의 큐가 가득 참 (%d/%d)",
			tenant, len(tq.queues[tenant]), tq.maxOutstanding)
	}

	// 새 테넌트면 순서에 추가
	if _, exists := tq.queues[tenant]; !exists {
		tq.tenantOrder = append(tq.tenantOrder, tenant)
	}

	tq.queues[tenant] = append(tq.queues[tenant], item)

	// 워커에 알림
	select {
	case tq.notify <- struct{}{}:
	default:
	}

	return nil
}

// Dequeue는 라운드 로빈 방식으로 다음 요청을 가져온다.
func (tq *TenantQueue) Dequeue() *RequestItem {
	tq.mu.Lock()
	defer tq.mu.Unlock()

	if len(tq.tenantOrder) == 0 {
		return nil
	}

	// 라운드 로빈: 각 테넌트를 순환하며 요청 가져오기
	for attempts := 0; attempts < len(tq.tenantOrder); attempts++ {
		idx := tq.currentIdx % len(tq.tenantOrder)
		tenant := tq.tenantOrder[idx]

		if queue, exists := tq.queues[tenant]; exists && len(queue) > 0 {
			item := queue[0]
			tq.queues[tenant] = queue[1:]

			// 빈 큐 정리
			if len(tq.queues[tenant]) == 0 {
				delete(tq.queues, tenant)
				tq.tenantOrder = removeFromSlice(tq.tenantOrder, idx)
				// 인덱스 조정 불필요 (다음 순환에서 자동 조정)
			} else {
				tq.currentIdx = idx + 1
			}

			return item
		}

		tq.currentIdx = idx + 1
	}

	return nil
}

// Close는 큐를 닫는다.
func (tq *TenantQueue) Close() {
	tq.mu.Lock()
	tq.closed = true
	tq.mu.Unlock()
	close(tq.notify)
}

// QueueSizes는 테넌트별 큐 크기를 반환한다.
func (tq *TenantQueue) QueueSizes() map[string]int {
	tq.mu.Lock()
	defer tq.mu.Unlock()
	sizes := make(map[string]int)
	for tenant, queue := range tq.queues {
		sizes[tenant] = len(queue)
	}
	return sizes
}

func removeFromSlice(s []string, idx int) []string {
	return append(s[:idx], s[idx+1:]...)
}

// =============================================================================
// 4. 결과 캐시 (Result Cache)
// =============================================================================
// 동일한 쿼리의 결과를 캐시하여 재실행을 방지한다.
// TTL 기반으로 캐시 항목이 만료된다.
//
// Loki 실제 코드: pkg/querier/queryrange/results_cache.go

// CacheEntry는 캐시에 저장되는 항목이다.
type CacheEntry struct {
	Response *QueryResponse
	CachedAt time.Time
	TTL      time.Duration
}

// IsExpired는 캐시 항목이 만료되었는지 확인한다.
func (e *CacheEntry) IsExpired() bool {
	return time.Since(e.CachedAt) > e.TTL
}

// ResultCache는 쿼리 결과 캐시이다.
type ResultCache struct {
	mu      sync.RWMutex
	entries map[string]*CacheEntry
	ttl     time.Duration
	hits    int
	misses  int
}

// NewResultCache는 새로운 결과 캐시를 생성한다.
func NewResultCache(ttl time.Duration) *ResultCache {
	return &ResultCache{
		entries: make(map[string]*CacheEntry),
		ttl:     ttl,
	}
}

// Get은 캐시에서 결과를 가져온다.
func (c *ResultCache) Get(key string) (*QueryResponse, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.entries[key]
	if !exists || entry.IsExpired() {
		c.mu.RUnlock()
		c.mu.Lock()
		c.misses++
		// 만료된 항목 삭제
		if exists {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		c.mu.RLock()
		return nil, false
	}

	c.mu.RUnlock()
	c.mu.Lock()
	c.hits++
	c.mu.Unlock()
	c.mu.RLock()

	resp := &QueryResponse{
		Entries:   entry.Response.Entries,
		FromCache: true,
	}
	return resp, true
}

// Put은 결과를 캐시에 저장한다.
func (c *ResultCache) Put(key string, resp *QueryResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = &CacheEntry{
		Response: resp,
		CachedAt: time.Now(),
		TTL:      c.ttl,
	}
}

// Stats는 캐시 통계를 반환한다.
func (c *ResultCache) Stats() (size, hits, misses int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries), c.hits, c.misses
}

// =============================================================================
// 5. 쿼리 중복 제거 (Query Dedup)
// =============================================================================
// 동시에 들어온 동일한 쿼리를 하나만 실행하고, 결과를 공유한다.
// "Single Flight" 패턴이라고도 한다.

// QueryDedup은 동일 쿼리 중복 제거기이다.
type QueryDedup struct {
	mu       sync.Mutex
	inflight map[string]*inflightQuery
	deduped  int
}

type inflightQuery struct {
	done     chan struct{}
	response *QueryResponse
	waiters  int // 대기 중인 요청 수
}

// NewQueryDedup은 새로운 중복 제거기를 생성한다.
func NewQueryDedup() *QueryDedup {
	return &QueryDedup{
		inflight: make(map[string]*inflightQuery),
	}
}

// Do는 쿼리를 실행하되, 동일 쿼리가 이미 실행 중이면 결과를 기다린다.
func (d *QueryDedup) Do(key string, fn func() *QueryResponse) *QueryResponse {
	d.mu.Lock()

	// 이미 실행 중인 동일 쿼리가 있는지 확인
	if flight, exists := d.inflight[key]; exists {
		flight.waiters++
		d.deduped++
		d.mu.Unlock()

		// 결과 대기
		<-flight.done
		return flight.response
	}

	// 새로운 쿼리 실행
	flight := &inflightQuery{
		done:    make(chan struct{}),
		waiters: 1,
	}
	d.inflight[key] = flight
	d.mu.Unlock()

	// 실제 쿼리 실행
	resp := fn()
	flight.response = resp

	// 대기 중인 모든 요청에 결과 전달
	close(flight.done)

	// 정리
	d.mu.Lock()
	delete(d.inflight, key)
	d.mu.Unlock()

	return resp
}

// DedupCount는 중복 제거된 쿼리 수를 반환한다.
func (d *QueryDedup) DedupCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deduped
}

// =============================================================================
// 6. 워커 풀 (Querier Workers)
// =============================================================================
// 큐에서 요청을 가져와 실행하는 워커 풀이다.
// 실제 Loki에서는 Querier가 이 역할을 한다.

// WorkerPool은 쿼리 워커 풀이다.
type WorkerPool struct {
	queue    *TenantQueue
	cache    *ResultCache
	dedup    *QueryDedup
	workers  int
	wg       sync.WaitGroup
	executed int
	mu       sync.Mutex
}

// NewWorkerPool은 새로운 워커 풀을 생성한다.
func NewWorkerPool(queue *TenantQueue, cache *ResultCache, dedup *QueryDedup, workers int) *WorkerPool {
	return &WorkerPool{
		queue:   queue,
		cache:   cache,
		dedup:   dedup,
		workers: workers,
	}
}

// Start는 워커 풀을 시작한다.
func (wp *WorkerPool) Start(ctx context.Context) {
	for i := 0; i < wp.workers; i++ {
		wp.wg.Add(1)
		go wp.worker(ctx, i)
	}
}

// Wait는 모든 워커가 종료될 때까지 기다린다.
func (wp *WorkerPool) Wait() {
	wp.wg.Wait()
}

// worker는 하나의 쿼리 워커이다.
func (wp *WorkerPool) worker(ctx context.Context, id int) {
	defer wp.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-wp.queue.notify:
			if !ok {
				return
			}
			// 큐에서 요청 처리
			for {
				item := wp.queue.Dequeue()
				if item == nil {
					break
				}
				wp.processRequest(item, id)
			}
		}
	}
}

// processRequest는 하나의 쿼리 요청을 처리한다.
func (wp *WorkerPool) processRequest(item *RequestItem, workerID int) {
	req := item.Request
	hash := req.Hash()

	// 1. 캐시 확인
	if cached, ok := wp.cache.Get(hash); ok {
		cached.RequestID = req.ID
		item.ResultCh <- cached
		return
	}

	// 2. 중복 제거 (동일 쿼리가 실행 중이면 결과 공유)
	resp := wp.dedup.Do(hash, func() *QueryResponse {
		return wp.executeQuery(req, workerID)
	})

	// 3. 캐시에 저장
	wp.cache.Put(hash, resp)

	resp.RequestID = req.ID
	item.ResultCh <- resp
}

// executeQuery는 실제 쿼리를 실행한다 (시뮬레이션).
func (wp *WorkerPool) executeQuery(req *QueryRequest, workerID int) *QueryResponse {
	start := time.Now()

	// 쿼리 실행 시뮬레이션 (10~50ms 랜덤 지연)
	r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
	delay := time.Duration(10+r.Intn(40)) * time.Millisecond
	time.Sleep(delay)

	wp.mu.Lock()
	wp.executed++
	wp.mu.Unlock()

	// 가짜 결과 생성
	numEntries := 2 + r.Intn(5)
	entries := make([]LogEntry, numEntries)
	for i := 0; i < numEntries; i++ {
		offset := time.Duration(r.Intn(int(req.End.Sub(req.Start).Seconds()))) * time.Second
		entries[i] = LogEntry{
			Timestamp: req.Start.Add(offset),
			Line:      fmt.Sprintf("Log line from worker-%d for %s", workerID, req.Query),
			Labels:    fmt.Sprintf("{tenant=%q}", req.Tenant),
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})

	return &QueryResponse{
		RequestID: req.ID,
		Entries:   entries,
		Duration:  time.Since(start),
	}
}

// ExecutedCount는 실제 실행된 쿼리 수를 반환한다.
func (wp *WorkerPool) ExecutedCount() int {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return wp.executed
}

// =============================================================================
// 7. 쿼리 프론트엔드 (통합)
// =============================================================================

// QueryFrontend는 쿼리 프론트엔드이다.
type QueryFrontend struct {
	queue       *TenantQueue
	cache       *ResultCache
	dedup       *QueryDedup
	pool        *WorkerPool
	splitInterval time.Duration
}

// NewQueryFrontend는 새로운 쿼리 프론트엔드를 생성한다.
func NewQueryFrontend(splitInterval time.Duration, numWorkers, maxOutstanding int, cacheTTL time.Duration) *QueryFrontend {
	queue := NewTenantQueue(maxOutstanding)
	cache := NewResultCache(cacheTTL)
	dedup := NewQueryDedup()
	pool := NewWorkerPool(queue, cache, dedup, numWorkers)

	return &QueryFrontend{
		queue:         queue,
		cache:         cache,
		dedup:         dedup,
		pool:          pool,
		splitInterval: splitInterval,
	}
}

// Start는 프론트엔드를 시작한다.
func (qf *QueryFrontend) Start(ctx context.Context) {
	qf.pool.Start(ctx)
}

// Execute는 쿼리를 실행한다.
func (qf *QueryFrontend) Execute(req *QueryRequest) *QueryResponse {
	// 1. 쿼리 분할
	splits := SplitByInterval(req, qf.splitInterval)

	// 2. 각 분할 쿼리를 큐에 넣고 결과 수집
	resultChs := make([]chan *QueryResponse, len(splits))
	for i, split := range splits {
		resultChs[i] = make(chan *QueryResponse, 1)
		item := &RequestItem{
			Request:  split,
			ResultCh: resultChs[i],
		}
		if err := qf.queue.Enqueue(item); err != nil {
			return &QueryResponse{
				RequestID: req.ID,
				Error:     err.Error(),
			}
		}

		// 큐에 알림
		select {
		case qf.queue.notify <- struct{}{}:
		default:
		}
	}

	// 3. 모든 분할 결과 수집
	responses := make([]*QueryResponse, len(splits))
	for i, ch := range resultChs {
		responses[i] = <-ch
	}

	// 4. 결과 병합
	merged := MergeResponses(responses, req.Limit)
	merged.RequestID = req.ID

	return merged
}

// Stats는 프론트엔드 통계를 반환한다.
func (qf *QueryFrontend) Stats() {
	cacheSize, cacheHits, cacheMisses := qf.cache.Stats()
	fmt.Printf("    캐시: size=%d, hits=%d, misses=%d", cacheSize, cacheHits, cacheMisses)
	if cacheHits+cacheMisses > 0 {
		hitRate := float64(cacheHits) / float64(cacheHits+cacheMisses) * 100
		fmt.Printf(" (히트율: %.1f%%)", hitRate)
	}
	fmt.Println()
	fmt.Printf("    중복 제거: %d건\n", qf.dedup.DedupCount())
	fmt.Printf("    실제 실행: %d건\n", qf.pool.ExecutedCount())
}

// Close는 프론트엔드를 종료한다.
func (qf *QueryFrontend) Close() {
	qf.queue.Close()
}

// =============================================================================
// 8. 메인 함수 - 쿼리 프론트엔드 시연
// =============================================================================

func main() {
	fmt.Println("=== Loki PoC #11: 쿼리 프론트엔드 - 쿼리 분할, 큐잉, 캐싱 ===")
	fmt.Println()

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	// =========================================================================
	// 시연 1: 쿼리 분할 (Time-based Splitting)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 1] 쿼리 분할 (Time-based Splitting)")
	fmt.Println()

	// 24시간 범위 쿼리 → 6시간 간격으로 분할
	originalReq := &QueryRequest{
		ID:     "query-001",
		Tenant: "tenant-A",
		Query:  `{app="api"} |= "error"`,
		Start:  baseTime,
		End:    baseTime.Add(24 * time.Hour),
		Limit:  100,
	}

	fmt.Printf("  원본 쿼리: %s\n", originalReq)
	fmt.Printf("  시간 범위: %s ~ %s (24시간)\n",
		originalReq.Start.Format("15:04"),
		originalReq.End.Format("2006-01-02 15:04"))
	fmt.Println()

	// 6시간 간격으로 분할
	splits := SplitByInterval(originalReq, 6*time.Hour)
	fmt.Printf("  6시간 간격으로 분할 → %d개 서브쿼리:\n", len(splits))
	for _, split := range splits {
		fmt.Printf("    %s: %s ~ %s\n",
			split.ID,
			split.Start.Format("15:04"),
			split.End.Format("2006-01-02 15:04"))
	}
	fmt.Println()

	// 1시간 간격으로 분할
	splits1h := SplitByInterval(originalReq, 1*time.Hour)
	fmt.Printf("  1시간 간격으로 분할 → %d개 서브쿼리\n", len(splits1h))
	fmt.Println()

	// =========================================================================
	// 시연 2: 테넌트별 큐잉 (Fair Scheduling)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 2] 테넌트별 큐잉 (Round-Robin Fair Scheduling)")
	fmt.Println()

	queue := NewTenantQueue(10)

	// 3개 테넌트가 각각 다른 수의 요청을 보냄
	tenantRequests := map[string]int{
		"tenant-A": 5,
		"tenant-B": 2,
		"tenant-C": 3,
	}

	fmt.Println("  요청 등록:")
	for tenant, count := range tenantRequests {
		for i := 0; i < count; i++ {
			item := &RequestItem{
				Request: &QueryRequest{
					ID:     fmt.Sprintf("%s-q%d", tenant, i+1),
					Tenant: tenant,
					Query:  fmt.Sprintf("query-%d", i+1),
				},
				ResultCh: make(chan *QueryResponse, 1),
			}
			queue.Enqueue(item)
		}
		fmt.Printf("    %s: %d개 요청\n", tenant, count)
	}

	// 라운드 로빈 순서로 Dequeue
	fmt.Println()
	fmt.Println("  Dequeue 순서 (Round-Robin):")
	for i := 0; ; i++ {
		item := queue.Dequeue()
		if item == nil {
			break
		}
		fmt.Printf("    [%2d] %s (tenant: %s)\n",
			i+1, item.Request.ID, item.Request.Tenant)
	}
	fmt.Println()

	// =========================================================================
	// 시연 3: 최대 대기 요청 수 제한
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 3] 최대 대기 요청 수 제한 (Max Outstanding)")
	fmt.Println()

	limitedQueue := NewTenantQueue(3) // 테넌트당 최대 3개

	fmt.Println("  테넌트당 최대 대기 요청 수: 3")
	fmt.Println()

	for i := 0; i < 5; i++ {
		item := &RequestItem{
			Request: &QueryRequest{
				ID:     fmt.Sprintf("q-%d", i+1),
				Tenant: "tenant-heavy",
				Query:  fmt.Sprintf("query-%d", i+1),
			},
			ResultCh: make(chan *QueryResponse, 1),
		}
		err := limitedQueue.Enqueue(item)
		if err != nil {
			fmt.Printf("    요청 %d: 거부 → %v\n", i+1, err)
		} else {
			fmt.Printf("    요청 %d: 수락 (큐 크기: %d)\n", i+1,
				limitedQueue.QueueSizes()["tenant-heavy"])
		}
	}
	fmt.Println()

	// =========================================================================
	// 시연 4: 결과 캐시
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 4] 결과 캐시")
	fmt.Println()

	cache := NewResultCache(5 * time.Second)

	testReq := &QueryRequest{
		Tenant: "tenant-A",
		Query:  `{app="api"}`,
		Start:  baseTime,
		End:    baseTime.Add(1 * time.Hour),
	}
	hash := testReq.Hash()

	// 캐시 미스
	_, found := cache.Get(hash)
	fmt.Printf("  1차 조회 (hash=%s...): %v (캐시 미스)\n", hash[:8], found)

	// 캐시에 저장
	cache.Put(hash, &QueryResponse{
		Entries: []LogEntry{
			{Timestamp: baseTime, Line: "cached result", Labels: "{app=api}"},
		},
	})
	fmt.Println("  결과 캐시에 저장")

	// 캐시 히트
	resp, found := cache.Get(hash)
	fmt.Printf("  2차 조회 (hash=%s...): %v (캐시 히트, from_cache=%v)\n",
		hash[:8], found, resp.FromCache)

	size, hits, misses := cache.Stats()
	fmt.Printf("  캐시 통계: size=%d, hits=%d, misses=%d\n", size, hits, misses)
	fmt.Println()

	// =========================================================================
	// 시연 5: 쿼리 중복 제거 (Single Flight)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 5] 쿼리 중복 제거 (Single Flight)")
	fmt.Println()

	dedup := NewQueryDedup()
	var wg sync.WaitGroup
	execCount := 0
	var execMu sync.Mutex

	// 동일한 쿼리를 5개의 고루틴에서 동시에 실행
	fmt.Println("  5개의 동시 요청 (동일 쿼리):")
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp := dedup.Do("same-query-key", func() *QueryResponse {
				execMu.Lock()
				execCount++
				execMu.Unlock()

				// 쿼리 실행 시뮬레이션
				time.Sleep(50 * time.Millisecond)
				return &QueryResponse{
					Entries: []LogEntry{
						{Line: "result from actual execution"},
					},
				}
			})
			fmt.Printf("    고루틴 %d: 결과 수신 (엔트리 %d개)\n",
				idx+1, len(resp.Entries))
		}(i)
	}

	wg.Wait()
	fmt.Printf("  실제 실행 횟수: %d (5개 요청 중 1개만 실행)\n", execCount)
	fmt.Printf("  중복 제거: %d건\n", dedup.DedupCount())
	fmt.Println()

	// =========================================================================
	// 시연 6: 전체 통합 시연
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 6] 전체 통합 시연 (분할 + 큐잉 + 캐싱 + 중복제거)")
	fmt.Println()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 쿼리 프론트엔드 생성
	// 3시간 간격 분할, 워커 3개, 테넌트당 최대 20개 대기, 캐시 TTL 10초
	frontend := NewQueryFrontend(3*time.Hour, 3, 20, 10*time.Second)
	frontend.Start(ctx)

	// 여러 테넌트의 쿼리를 동시에 실행
	queries := []*QueryRequest{
		{ID: "q1", Tenant: "team-backend", Query: `{app="api"} |= "error"`,
			Start: baseTime, End: baseTime.Add(12 * time.Hour), Limit: 50},
		{ID: "q2", Tenant: "team-frontend", Query: `{app="web"} |= "404"`,
			Start: baseTime, End: baseTime.Add(6 * time.Hour), Limit: 30},
		{ID: "q3", Tenant: "team-backend", Query: `{app="api"} |= "error"`,
			Start: baseTime, End: baseTime.Add(12 * time.Hour), Limit: 50}, // q1과 동일
		{ID: "q4", Tenant: "team-data", Query: `{app="pipeline"} |= "failed"`,
			Start: baseTime, End: baseTime.Add(24 * time.Hour), Limit: 100},
	}

	fmt.Println("  쿼리 요청:")
	for _, q := range queries {
		splits := SplitByInterval(q, 3*time.Hour)
		fmt.Printf("    %s: %s → %d개 서브쿼리로 분할\n", q.ID, q, len(splits))
	}
	fmt.Println()

	// 동시 실행
	var queryWg sync.WaitGroup
	results := make([]*QueryResponse, len(queries))
	startTime := time.Now()

	for i, q := range queries {
		queryWg.Add(1)
		go func(idx int, req *QueryRequest) {
			defer queryWg.Done()
			results[idx] = frontend.Execute(req)
		}(i, q)
	}

	queryWg.Wait()
	totalDuration := time.Since(startTime)

	fmt.Println("  실행 결과:")
	for i, resp := range results {
		if resp.Error != "" {
			fmt.Printf("    %s: 에러 → %s\n", queries[i].ID, resp.Error)
		} else {
			fmt.Printf("    %s: %d개 엔트리 반환 (cache=%v)\n",
				queries[i].ID, len(resp.Entries), resp.FromCache)
		}
	}
	fmt.Printf("  총 실행 시간: %v\n", totalDuration)
	fmt.Println()
	fmt.Println("  통계:")
	frontend.Stats()

	frontend.Close()
	cancel()

	// =========================================================================
	// 구조 요약
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("=== 쿼리 프론트엔드 구조 요약 ===")
	fmt.Println()
	fmt.Println("  Loki 쿼리 처리 파이프라인:")
	fmt.Println()
	fmt.Println("  클라이언트 쿼리")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  ┌────────────────────────────────────────┐")
	fmt.Println("  │ Query Frontend                         │")
	fmt.Println("  │                                        │")
	fmt.Println("  │  [1] 결과 캐시 확인 ← 히트 시 즉시 반환│")
	fmt.Println("  │       │                                │")
	fmt.Println("  │  [2] 쿼리 분할 (24h → 1h × 24)        │")
	fmt.Println("  │       │                                │")
	fmt.Println("  │  [3] 중복 제거 (동일 쿼리 합치기)       │")
	fmt.Println("  │       │                                │")
	fmt.Println("  │  [4] 테넌트별 큐 (Fair Scheduling)      │")
	fmt.Println("  │       │    Round-Robin                  │")
	fmt.Println("  └───────┼────────────────────────────────┘")
	fmt.Println("          │")
	fmt.Println("          ▼")
	fmt.Println("  ┌────────────────┐")
	fmt.Println("  │ Worker Pool    │   Querier 인스턴스들")
	fmt.Println("  │ (Querier ×N)   │")
	fmt.Println("  └────────────────┘")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("    - 쿼리 분할: 큰 쿼리를 작은 간격으로 나눠 병렬 처리")
	fmt.Println("    - 테넌트 격리: 라운드 로빈으로 공정한 스케줄링")
	fmt.Println("    - 캐시: 동일 쿼리 결과 재사용 (히트율 향상)")
	fmt.Println("    - 중복 제거: Single Flight 패턴으로 중복 실행 방지")
	fmt.Println("    - 백프레셔: 테넌트당 max_outstanding으로 큐 오버플로 방지")

	// 워커 종료를 위해 잠시 대기
	time.Sleep(100 * time.Millisecond)
}

