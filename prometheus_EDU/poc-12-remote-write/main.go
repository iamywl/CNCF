package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// 데이터 모델 (Prometheus prompb.TimeSeries 단순화)
// =============================================================================

// Label은 Prometheus의 labels.Label과 동일한 구조
type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Sample은 prompb.Sample과 동일 — 타임스탬프 + 값
type Sample struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

// TimeSeries는 prompb.TimeSeries의 단순화 버전
type TimeSeries struct {
	Labels  []Label  `json:"labels"`
	Samples []Sample `json:"samples"`
}

// WriteRequest는 Remote Write 프로토콜의 요청 본문
// 실제 Prometheus는 protobuf + snappy 압축을 사용하지만, PoC에서는 JSON 사용
type WriteRequest struct {
	TimeSeries []TimeSeries `json:"timeseries"`
}

// labelsKey는 레이블 세트를 문자열 키로 변환 (해싱용)
func labelsKey(labels []Label) string {
	sorted := make([]Label, len(labels))
	copy(sorted, labels)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})
	parts := make([]string, len(sorted))
	for i, l := range sorted {
		parts[i] = l.Name + "=" + l.Value
	}
	return strings.Join(parts, ",")
}

// hashLabels는 레이블 세트의 해시를 계산 — 샤드 할당에 사용
// 실제 Prometheus: shards.enqueue()에서 ref(HeadSeriesRef) % numShards
func hashLabels(labels []Label) uint64 {
	h := fnv.New64a()
	h.Write([]byte(labelsKey(labels)))
	return h.Sum64()
}

// =============================================================================
// EWMA (Exponentially Weighted Moving Average) Rate
// storage/remote/queue_manager.go의 ewmaRate 구현
// =============================================================================

const ewmaWeight = 0.2

type ewmaRate struct {
	mu       sync.Mutex
	newCount int64
	rate     float64
	lastTick time.Time
}

func newEWMARate() *ewmaRate {
	return &ewmaRate{lastTick: time.Now()}
}

func (r *ewmaRate) incr(count int64) {
	r.mu.Lock()
	r.newCount += count
	r.mu.Unlock()
}

func (r *ewmaRate) tick() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	elapsed := time.Since(r.lastTick).Seconds()
	if elapsed == 0 {
		elapsed = 1
	}
	instantRate := float64(r.newCount) / elapsed
	r.rate = ewmaWeight*instantRate + (1-ewmaWeight)*r.rate
	r.newCount = 0
	r.lastTick = time.Now()
	return r.rate
}

func (r *ewmaRate) getRate() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rate
}

// =============================================================================
// Remote Write Receiver (수신 서버)
// =============================================================================

type RemoteWriteServer struct {
	mu             sync.Mutex
	receivedSeries map[string][]Sample // labelsKey → samples
	totalReceived  int64
	failUntil      time.Time // 이 시간까지 503 응답 (장애 시뮬레이션)
	listenAddr     string
	server         *http.Server
}

func NewRemoteWriteServer(addr string) *RemoteWriteServer {
	s := &RemoteWriteServer{
		receivedSeries: make(map[string][]Sample),
		listenAddr:     addr,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/write", s.handleWrite)
	s.server = &http.Server{Addr: addr, Handler: mux}
	return s
}

func (s *RemoteWriteServer) handleWrite(w http.ResponseWriter, r *http.Request) {
	// 장애 시뮬레이션: failUntil 이전이면 503 응답
	s.mu.Lock()
	if time.Now().Before(s.failUntil) {
		s.mu.Unlock()
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	s.mu.Unlock()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req WriteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "unmarshal error", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	for _, ts := range req.TimeSeries {
		key := labelsKey(ts.Labels)
		s.receivedSeries[key] = append(s.receivedSeries[key], ts.Samples...)
		s.totalReceived += int64(len(ts.Samples))
	}
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (s *RemoteWriteServer) SetFailUntil(d time.Duration) {
	s.mu.Lock()
	s.failUntil = time.Now().Add(d)
	s.mu.Unlock()
}

func (s *RemoteWriteServer) TotalReceived() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalReceived
}

func (s *RemoteWriteServer) SeriesCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.receivedSeries)
}

func (s *RemoteWriteServer) Start() {
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Server] ListenAndServe error: %v", err)
		}
	}()
}

// =============================================================================
// Queue — 각 샤드별 채널 기반 큐
// storage/remote/queue_manager.go의 queue 구조체 단순화
// =============================================================================

type shardQueue struct {
	ch       chan TimeSeries
	capacity int
}

func newShardQueue(capacity int) *shardQueue {
	return &shardQueue{
		ch:       make(chan TimeSeries, capacity),
		capacity: capacity,
	}
}

func (q *shardQueue) Append(ts TimeSeries) bool {
	select {
	case q.ch <- ts:
		return true
	default:
		return false // 큐가 가득 참
	}
}

func (q *shardQueue) Len() int {
	return len(q.ch)
}

// =============================================================================
// QueueManager — Remote Write 파이프라인의 핵심
// storage/remote/queue_manager.go의 QueueManager 단순화
// =============================================================================

const (
	shardToleranceFraction = 0.3
	shardUpdateDuration    = 2 * time.Second // PoC에서는 짧게 설정
)

type QueueManagerConfig struct {
	NumShards         int
	MaxShards         int
	MinShards         int
	MaxSamplesPerSend int
	QueueCapacity     int
	BatchSendDeadline time.Duration
	MinBackoff        time.Duration
	MaxBackoff        time.Duration
	Endpoint          string
	AutoReshard       bool // 자동 resharding 활성화 여부
}

func DefaultConfig(endpoint string) QueueManagerConfig {
	return QueueManagerConfig{
		NumShards:         4,
		MaxShards:         16,
		MinShards:         1,
		MaxSamplesPerSend: 100,
		QueueCapacity:     500,
		BatchSendDeadline: 500 * time.Millisecond,
		MinBackoff:        30 * time.Millisecond,
		MaxBackoff:        500 * time.Millisecond,
		Endpoint:          endpoint,
		AutoReshard:       false,
	}
}

type QueueManager struct {
	cfg QueueManagerConfig

	mu     sync.RWMutex
	queues []*shardQueue

	// 메트릭 (EWMA 기반 dynamic sharding)
	dataIn          *ewmaRate
	dataOut         *ewmaRate
	dataOutDuration *ewmaRate

	// 통계
	totalEnqueued atomic.Int64
	totalSent     atomic.Int64
	totalRetries  atomic.Int64

	// 샤드별 할당 카운터 — reshard와 독립적으로 누적
	shardDistMu   sync.Mutex
	shardDistMap  map[int]int64

	reshardChan chan int
	quit        chan struct{}
	wg          sync.WaitGroup
	numShards   int

	client *http.Client
}

func NewQueueManager(cfg QueueManagerConfig) *QueueManager {
	qm := &QueueManager{
		cfg:          cfg,
		dataIn:       newEWMARate(),
		dataOut:      newEWMARate(),
		dataOutDuration: newEWMARate(),
		reshardChan:  make(chan int),
		quit:         make(chan struct{}),
		numShards:    cfg.NumShards,
		shardDistMap: make(map[int]int64),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
	qm.createShards(cfg.NumShards)
	return qm
}

func (qm *QueueManager) createShards(n int) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	qm.queues = make([]*shardQueue, n)
	for i := 0; i < n; i++ {
		qm.queues[i] = newShardQueue(qm.cfg.QueueCapacity)
	}
	qm.numShards = n
}

// Start는 모든 샤드 루프와 reshard 루프를 시작
func (qm *QueueManager) Start() {
	qm.mu.RLock()
	n := len(qm.queues)
	qm.mu.RUnlock()

	for i := 0; i < n; i++ {
		qm.wg.Add(1)
		go qm.shardLoop(i)
	}

	qm.wg.Add(1)
	go qm.reshardLoop()

	if qm.cfg.AutoReshard {
		qm.wg.Add(1)
		go qm.updateShardsLoop()
	}
}

// Append는 시리즈를 해싱하여 적절한 샤드에 할당
// 실제: QueueManager.Append() → shards.enqueue()에서 ref % numShards
func (qm *QueueManager) Append(ts TimeSeries) bool {
	hash := hashLabels(ts.Labels)

	qm.mu.RLock()
	n := len(qm.queues)
	shard := int(hash % uint64(n))
	q := qm.queues[shard]
	qm.mu.RUnlock()

	// 실제 Prometheus: enqueue 실패 시 backoff 후 재시도
	backoff := qm.cfg.MinBackoff
	for i := 0; i < 10; i++ {
		if q.Append(ts) {
			qm.totalEnqueued.Add(1)
			qm.dataIn.incr(int64(len(ts.Samples)))

			// 샤드 분포 기록
			qm.shardDistMu.Lock()
			qm.shardDistMap[shard]++
			qm.shardDistMu.Unlock()
			return true
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, qm.cfg.MaxBackoff)
	}
	return false
}

// GetShardDistribution은 현재까지의 샤드 분포를 반환
func (qm *QueueManager) GetShardDistribution() map[int]int64 {
	qm.shardDistMu.Lock()
	defer qm.shardDistMu.Unlock()
	result := make(map[int]int64, len(qm.shardDistMap))
	for k, v := range qm.shardDistMap {
		result[k] = v
	}
	return result
}

// ResetShardDistribution은 분포 카운터를 초기화
func (qm *QueueManager) ResetShardDistribution() {
	qm.shardDistMu.Lock()
	qm.shardDistMap = make(map[int]int64)
	qm.shardDistMu.Unlock()
}

// shardLoop — 각 샤드에서 배치를 수집하여 전송
// 실제: shards.runShard() — batchQueue에서 읽어 sendSamples() 호출
func (qm *QueueManager) shardLoop(shardID int) {
	defer qm.wg.Done()

	ticker := time.NewTicker(qm.cfg.BatchSendDeadline)
	defer ticker.Stop()

	batch := make([]TimeSeries, 0, qm.cfg.MaxSamplesPerSend)

	sendBatch := func() {
		if len(batch) == 0 {
			return
		}
		toSend := make([]TimeSeries, len(batch))
		copy(toSend, batch)
		batch = batch[:0]

		qm.sendWithBackoff(shardID, toSend)
	}

	for {
		qm.mu.RLock()
		if shardID >= len(qm.queues) {
			qm.mu.RUnlock()
			return
		}
		q := qm.queues[shardID]
		qm.mu.RUnlock()

		select {
		case <-qm.quit:
			sendBatch() // 종료 전 남은 배치 전송
			return
		case ts, ok := <-q.ch:
			if !ok {
				sendBatch()
				return
			}
			batch = append(batch, ts)
			// MaxSamplesPerSend에 도달하면 즉시 전송
			if len(batch) >= qm.cfg.MaxSamplesPerSend {
				sendBatch()
				ticker.Reset(qm.cfg.BatchSendDeadline)
			}
		case <-ticker.C:
			// BatchSendDeadline 경과 — 모인 것만이라도 전송
			sendBatch()
		}
	}
}

// sendWithBackoff — 지수 백오프 재시도 포함 HTTP POST
// 실제: sendSamplesWithBackoff() → sendWriteRequestWithBackoff()
func (qm *QueueManager) sendWithBackoff(shardID int, batch []TimeSeries) {
	req := WriteRequest{TimeSeries: batch}
	data, err := json.Marshal(req)
	if err != nil {
		log.Printf("[Shard %d] marshal error: %v", shardID, err)
		return
	}

	sampleCount := 0
	for _, ts := range batch {
		sampleCount += len(ts.Samples)
	}

	backoff := qm.cfg.MinBackoff
	maxRetries := 10

	for try := 0; try <= maxRetries; try++ {
		select {
		case <-qm.quit:
			return
		default:
		}

		start := time.Now()
		resp, err := qm.client.Post(
			qm.cfg.Endpoint+"/api/v1/write",
			"application/json",
			strings.NewReader(string(data)),
		)
		duration := time.Since(start)

		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				qm.totalSent.Add(int64(sampleCount))
				qm.dataOut.incr(int64(sampleCount))
				qm.dataOutDuration.incr(int64(duration))
				return
			}
			// 5xx는 recoverable — 재시도
			if resp.StatusCode >= 500 {
				qm.totalRetries.Add(1)
				if try < maxRetries {
					log.Printf("[Shard %d] 서버 오류 %d, %v 후 재시도 (try %d)",
						shardID, resp.StatusCode, backoff, try+1)
					time.Sleep(backoff)
					backoff = min(backoff*2, qm.cfg.MaxBackoff)
					continue
				}
			}
			// 4xx는 non-recoverable
			log.Printf("[Shard %d] non-recoverable error: %d", shardID, resp.StatusCode)
			return
		}

		// 네트워크 오류 — 재시도
		qm.totalRetries.Add(1)
		if try < maxRetries {
			log.Printf("[Shard %d] 네트워크 오류, %v 후 재시도 (try %d): %v",
				shardID, backoff, try+1, err)
			time.Sleep(backoff)
			backoff = min(backoff*2, qm.cfg.MaxBackoff)
			continue
		}
		log.Printf("[Shard %d] 최대 재시도 횟수 초과, 배치 폐기", shardID)
	}
}

// reshardLoop — reshardChan에서 새로운 샤드 수를 받아 resharding 수행
// 실제: QueueManager.reshardLoop()
func (qm *QueueManager) reshardLoop() {
	defer qm.wg.Done()

	for {
		select {
		case <-qm.quit:
			return
		case newShards := <-qm.reshardChan:
			qm.reshard(newShards)
		}
	}
}

// reshard — 새로운 샤드 수로 큐를 재구성
// 기존 샤드의 잔여 데이터는 새 샤드로 재분배
func (qm *QueueManager) reshard(newShards int) {
	qm.mu.Lock()
	oldQueues := qm.queues
	oldNum := len(oldQueues)

	// 새 큐 생성
	qm.queues = make([]*shardQueue, newShards)
	for i := 0; i < newShards; i++ {
		qm.queues[i] = newShardQueue(qm.cfg.QueueCapacity)
	}
	qm.numShards = newShards
	qm.mu.Unlock()

	// 기존 큐에서 남은 데이터를 새 큐로 재분배
	redistributed := 0
	for i := 0; i < oldNum; i++ {
		for {
			select {
			case ts := <-oldQueues[i].ch:
				hash := hashLabels(ts.Labels)
				shard := hash % uint64(newShards)
				qm.queues[shard].Append(ts)
				redistributed++
			default:
				goto nextQueue
			}
		}
	nextQueue:
	}

	// 새 샤드 루프 시작
	for i := 0; i < newShards; i++ {
		qm.wg.Add(1)
		go qm.shardLoop(i)
	}

	log.Printf("[QueueManager] Resharded: %d -> %d 샤드 (재분배된 샘플: %d)",
		oldNum, newShards, redistributed)
}

// updateShardsLoop — 주기적으로 desiredShards를 계산하여 resharding 트리거
// 실제: QueueManager.updateShardsLoop() → calculateDesiredShards()
func (qm *QueueManager) updateShardsLoop() {
	defer qm.wg.Done()

	ticker := time.NewTicker(shardUpdateDuration)
	defer ticker.Stop()

	for {
		select {
		case <-qm.quit:
			return
		case <-ticker.C:
			desired := qm.calculateDesiredShards()
			if desired != qm.numShards {
				select {
				case qm.reshardChan <- desired:
				default:
				}
			}
		}
	}
}

// calculateDesiredShards — EWMA 기반 동적 샤드 수 계산
// 실제 알고리즘:
//
//	dataInRate = 초당 수집 속도 (EWMA)
//	dataOutRate = 초당 전송 속도 (EWMA)
//	timePerSample = dataOutDuration / dataOutRate
//	desiredShards = timePerSample * (dataInRate + 0.05 * dataPending)
//
// 현재 샤드 수 대비 +-30% tolerance 적용
func (qm *QueueManager) calculateDesiredShards() int {
	inRate := qm.dataIn.tick()
	outRate := qm.dataOut.tick()
	outDuration := qm.dataOutDuration.tick()

	if outRate <= 0 {
		return qm.numShards
	}

	timePerSample := (outDuration / float64(time.Second)) / outRate
	pending := float64(qm.pendingSamples())
	backlogCatchup := 0.05 * pending

	desiredShards := timePerSample * (inRate + backlogCatchup)

	// tolerance: 현재 샤드 수의 +-30% 이내면 변경하지 않음
	lowerBound := float64(qm.numShards) * (1.0 - shardToleranceFraction)
	upperBound := float64(qm.numShards) * (1.0 + shardToleranceFraction)

	desiredShards = math.Ceil(desiredShards)
	if lowerBound <= desiredShards && desiredShards <= upperBound {
		return qm.numShards
	}

	n := int(desiredShards)
	if n < qm.cfg.MinShards {
		n = qm.cfg.MinShards
	}
	if n > qm.cfg.MaxShards {
		n = qm.cfg.MaxShards
	}
	return n
}

// pendingSamples — 모든 샤드의 대기 중인 샘플 수 합산
func (qm *QueueManager) pendingSamples() int {
	qm.mu.RLock()
	defer qm.mu.RUnlock()
	total := 0
	for _, q := range qm.queues {
		total += q.Len()
	}
	return total
}

// Stop — QueueManager 종료
func (qm *QueueManager) Stop() {
	close(qm.quit)
	qm.wg.Wait()
}

// =============================================================================
// 유틸리티
// =============================================================================

func printShardDistribution(dist map[int]int64, numShards int) {
	maxCount := int64(0)
	for _, v := range dist {
		if v > maxCount {
			maxCount = v
		}
	}
	for i := 0; i < numShards; i++ {
		count := dist[i]
		barLen := 0
		if maxCount > 0 {
			barLen = int(count * 40 / maxCount)
		}
		bar := strings.Repeat("█", barLen)
		fmt.Printf("  Shard %2d: %4d 시리즈 %s\n", i, count, bar)
	}
}

// =============================================================================
// 메인 데모
// =============================================================================

func main() {
	fmt.Println("=== Prometheus Remote Write Pipeline PoC ===")
	fmt.Println()
	fmt.Println("이 PoC는 Prometheus의 Remote Write 파이프라인을 시뮬레이션합니다.")
	fmt.Println("참조: storage/remote/queue_manager.go")
	fmt.Println()

	// -----------------------------------------------------------------------
	// 1. Remote Write 수신 서버 시작
	// -----------------------------------------------------------------------
	fmt.Println("--- 1단계: Remote Write 수신 서버 시작 ---")
	server := NewRemoteWriteServer(":19291")
	server.Start()
	time.Sleep(100 * time.Millisecond)
	fmt.Println("  수신 서버 시작: http://localhost:19291/api/v1/write")
	fmt.Println()

	// -----------------------------------------------------------------------
	// 2. QueueManager 시작 (4 샤드, 자동 resharding 비활성화)
	// -----------------------------------------------------------------------
	fmt.Println("--- 2단계: QueueManager 시작 (4 샤드) ---")
	cfg := DefaultConfig("http://localhost:19291")
	cfg.NumShards = 4
	cfg.MaxShards = 16
	cfg.MinShards = 1
	cfg.MaxSamplesPerSend = 50
	cfg.BatchSendDeadline = 200 * time.Millisecond
	cfg.AutoReshard = false // 데모 흐름 제어를 위해 수동 resharding

	qm := NewQueueManager(cfg)
	qm.Start()
	fmt.Printf("  설정: NumShards=%d, MaxSamplesPerSend=%d, BatchSendDeadline=%v\n",
		cfg.NumShards, cfg.MaxSamplesPerSend, cfg.BatchSendDeadline)
	fmt.Println()

	// -----------------------------------------------------------------------
	// 3. 1000개 샘플을 50개 시리즈에 걸쳐 전송
	// -----------------------------------------------------------------------
	fmt.Println("--- 3단계: 1000개 샘플 전송 (50개 시리즈) ---")
	numSeries := 50
	samplesPerSeries := 20
	baseTime := time.Now().UnixMilli()

	for i := 0; i < numSeries; i++ {
		samples := make([]Sample, samplesPerSeries)
		for j := 0; j < samplesPerSeries; j++ {
			samples[j] = Sample{
				Timestamp: baseTime + int64(j)*1000,
				Value:     rand.Float64() * 100,
			}
		}
		ts := TimeSeries{
			Labels: []Label{
				{Name: "__name__", Value: fmt.Sprintf("http_requests_total_%d", i)},
				{Name: "instance", Value: fmt.Sprintf("instance-%d", i%5)},
				{Name: "job", Value: "webserver"},
			},
			Samples: samples,
		}
		if !qm.Append(ts) {
			log.Printf("  경고: 시리즈 %d enqueue 실패", i)
		}
	}

	totalSamples := numSeries * samplesPerSeries
	fmt.Printf("  총 %d 샘플 enqueued (%d 시리즈 x %d 샘플/시리즈)\n",
		totalSamples, numSeries, samplesPerSeries)

	// 전송 완료 대기
	time.Sleep(2 * time.Second)

	received := server.TotalReceived()
	fmt.Printf("  수신 서버 수신 완료: %d / %d 샘플\n", received, totalSamples)
	fmt.Println()

	// -----------------------------------------------------------------------
	// 4. 샤드 분포 표시
	// -----------------------------------------------------------------------
	fmt.Println("--- 4단계: 샤드 분포 ---")
	fmt.Println("  실제 Prometheus: shards.enqueue()에서 ref %% numShards로 할당")
	fmt.Println("  PoC: fnv64a(labelsKey) %% numShards로 할당")
	fmt.Println()

	dist := qm.GetShardDistribution()
	printShardDistribution(dist, 4)
	fmt.Println()

	// -----------------------------------------------------------------------
	// 5. 수신 확인
	// -----------------------------------------------------------------------
	fmt.Println("--- 5단계: 수신 확인 ---")
	fmt.Printf("  전송 요청: %d 샘플\n", totalSamples)
	fmt.Printf("  수신 완료: %d 샘플\n", server.TotalReceived())
	fmt.Printf("  고유 시리즈: %d\n", server.SeriesCount())
	fmt.Printf("  재시도 횟수: %d\n", qm.totalRetries.Load())

	if server.TotalReceived() >= int64(totalSamples) {
		fmt.Println("  결과: 모든 샘플 성공적으로 수신됨")
	} else {
		fmt.Println("  결과: 일부 샘플 누락")
	}
	fmt.Println()

	// -----------------------------------------------------------------------
	// 6. 장애 시뮬레이션 → 재시도 → 복구
	// -----------------------------------------------------------------------
	fmt.Println("--- 6단계: 서버 장애 시뮬레이션 ---")
	fmt.Println("  서버를 1.5초간 503 응답으로 설정...")
	server.SetFailUntil(1500 * time.Millisecond)

	beforeRetries := qm.totalRetries.Load()
	beforeReceived := server.TotalReceived()

	// 장애 중 200개 샘플 추가 전송
	failureSamples := 200
	for i := 0; i < 10; i++ {
		samples := make([]Sample, 20)
		for j := 0; j < 20; j++ {
			samples[j] = Sample{
				Timestamp: baseTime + int64(1000+j)*1000,
				Value:     rand.Float64() * 100,
			}
		}
		ts := TimeSeries{
			Labels: []Label{
				{Name: "__name__", Value: fmt.Sprintf("error_test_%d", i)},
				{Name: "job", Value: "test"},
			},
			Samples: samples,
		}
		qm.Append(ts)
	}
	fmt.Printf("  장애 중 %d 샘플 전송 시도\n", failureSamples)

	// 복구 대기
	time.Sleep(3 * time.Second)

	afterRetries := qm.totalRetries.Load()
	afterReceived := server.TotalReceived()
	fmt.Printf("  재시도 횟수: %d (이전: %d, 신규: %d)\n",
		afterRetries, beforeRetries, afterRetries-beforeRetries)
	fmt.Printf("  추가 수신: %d 샘플 (장애 후 복구로 수신)\n",
		afterReceived-beforeReceived)

	fmt.Println()
	fmt.Println("  지수 백오프 전략 (실제 Prometheus sendWriteRequestWithBackoff):")
	fmt.Println("    - 초기 백오프: MinBackoff (30ms)")
	fmt.Println("    - 실패 시: backoff *= 2 (최대 MaxBackoff)")
	fmt.Println("    - Retry-After 헤더 지원")
	fmt.Println("    - 5xx: recoverable (재시도)")
	fmt.Println("    - 4xx: non-recoverable (폐기)")
	fmt.Println()

	// -----------------------------------------------------------------------
	// 7. 동적 Resharding 시뮬레이션
	// -----------------------------------------------------------------------
	fmt.Println("--- 7단계: 동적 Resharding ---")
	fmt.Println("  실제 Prometheus calculateDesiredShards() 알고리즘:")
	fmt.Println("    desiredShards = timePerSample * (dataInRate + 0.05 * dataPending)")
	fmt.Println("    - timePerSample = 전송 소요시간 / 전송 속도 (EWMA)")
	fmt.Println("    - dataInRate   = 수집 속도 (EWMA)")
	fmt.Println("    - dataPending  = 지연 * 수집속도 (밀린 양)")
	fmt.Println("    - +-30% tolerance 이내면 변경하지 않음")
	fmt.Println()

	oldShards := qm.numShards
	fmt.Printf("  현재 샤드 수: %d\n", oldShards)

	// 분포 카운터 리셋 (resharding 후 분포 확인용)
	qm.ResetShardDistribution()

	// 수동으로 resharding 트리거
	fmt.Printf("  수동 resharding 트리거: %d -> 8 샤드\n", oldShards)
	qm.reshardChan <- 8
	time.Sleep(500 * time.Millisecond)
	fmt.Printf("  resharding 후 샤드 수: %d\n", qm.numShards)

	// resharding 후 추가 샘플 전송
	fmt.Println("  resharding 후 100 샘플 추가 전송...")
	for i := 0; i < 5; i++ {
		samples := make([]Sample, 20)
		for j := 0; j < 20; j++ {
			samples[j] = Sample{
				Timestamp: baseTime + int64(2000+j)*1000,
				Value:     rand.Float64() * 100,
			}
		}
		ts := TimeSeries{
			Labels: []Label{
				{Name: "__name__", Value: fmt.Sprintf("reshard_test_%d", i)},
				{Name: "job", Value: "reshard"},
			},
			Samples: samples,
		}
		qm.Append(ts)
	}

	time.Sleep(2 * time.Second)

	fmt.Printf("  resharding 후 수신 서버 총 수신: %d 샘플\n", server.TotalReceived())
	fmt.Println()

	// 샤드 분포 (resharding 후)
	fmt.Println("  Resharding 후 샤드 분포 (8 샤드):")
	dist = qm.GetShardDistribution()
	printShardDistribution(dist, 8)
	fmt.Println()

	// -----------------------------------------------------------------------
	// 종합 요약
	// -----------------------------------------------------------------------
	fmt.Println("=== 종합 요약 ===")
	fmt.Printf("  총 enqueued:    %d 시리즈\n", qm.totalEnqueued.Load())
	fmt.Printf("  총 sent:        %d 샘플\n", qm.totalSent.Load())
	fmt.Printf("  총 received:    %d 샘플\n", server.TotalReceived())
	fmt.Printf("  총 retries:     %d\n", qm.totalRetries.Load())
	fmt.Printf("  최종 샤드 수:    %d\n", qm.numShards)
	fmt.Println()

	fmt.Println("=== Remote Write 파이프라인 핵심 구조 ===")
	fmt.Println()
	fmt.Println("  ┌─────────────┐")
	fmt.Println("  │  Scrape     │  WAL에서 읽어 Append() 호출")
	fmt.Println("  │  Engine     │")
	fmt.Println("  └──────┬──────┘")
	fmt.Println("         │")
	fmt.Println("         ▼")
	fmt.Println("  ┌─────────────┐  hash(labels) %% numShards")
	fmt.Println("  │  Queue      │──────────────────────────┐")
	fmt.Println("  │  Manager    │                          │")
	fmt.Println("  └──────┬──────┘                          │")
	fmt.Println("         │                                 │")
	fmt.Println("    ┌────┼────┬────┬─── ··· ───┐          │")
	fmt.Println("    ▼    ▼    ▼    ▼            ▼          │")
	fmt.Println("  ┌───┐┌───┐┌───┐┌───┐      ┌───┐        │")
	fmt.Println("  │ 0 ││ 1 ││ 2 ││ 3 │ ···  │ N │  shards│")
	fmt.Println("  └─┬─┘└─┬─┘└─┬─┘└─┬─┘      └─┬─┘        │")
	fmt.Println("    │    │    │    │            │          │")
	fmt.Println("    └────┴────┴────┴────── ····┘          │")
	fmt.Println("         │  batch + HTTP POST             │")
	fmt.Println("         ▼                                │")
	fmt.Println("  ┌─────────────┐  sendSamplesWithBackoff │")
	fmt.Println("  │  Remote     │  - 지수 백오프 재시도      │")
	fmt.Println("  │  Endpoint   │  - 5xx: recoverable     │")
	fmt.Println("  └─────────────┘  - 4xx: drop            │")
	fmt.Println("                                          │")
	fmt.Println("  ┌─────────────┐  calculateDesiredShards │")
	fmt.Println("  │  Reshard    │◄─────────────────────────┘")
	fmt.Println("  │  Loop       │  EWMA 기반 동적 샤드 조정")
	fmt.Println("  └─────────────┘")
	fmt.Println()

	// 정리
	qm.Stop()
	fmt.Println("완료.")
}
