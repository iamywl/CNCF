package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kafka 쿼터 관리 시스템 시뮬레이션
//
// 이 PoC는 Kafka의 ClientQuotaManager가 구현하는 핵심 개념을 시뮬레이션한다:
//   1. 시간 윈도우 기반 Rate 계산 (SampledStat/Rate)
//   2. 계층적 쿼터 매칭 (user > client-id > default)
//   3. 쓰로틀 시간 계산 (QuotaUtils.throttleTime)
//   4. ThrottledChannel과 DelayQueue를 통한 채널 뮤트/언뮤트
//   5. TokenBucket 기반 ControllerMutation 쿼터
//
// 참조 소스:
//   server/src/main/java/org/apache/kafka/server/quota/ClientQuotaManager.java
//   server-common/src/main/java/org/apache/kafka/server/quota/QuotaUtils.java
//   server/src/main/java/org/apache/kafka/server/quota/ThrottledChannel.java
//   clients/src/main/java/org/apache/kafka/common/metrics/stats/TokenBucket.java
// =============================================================================

// --- 시간 윈도우 기반 Rate 계산 ---

// Sample은 하나의 시간 윈도우 내 데이터를 나타낸다.
// Kafka의 SampledStat.Sample에 대응한다.
type Sample struct {
	StartTimeMs int64   // 이 샘플의 시작 시각
	Value       float64 // 이 윈도우 내 누적 값
	Count       int64   // 이 윈도우 내 기록 횟수
}

// RateStat은 시간 윈도우 기반 Rate를 계산하는 통계 구현이다.
// Kafka의 Rate (SampledStat 기반)에 대응한다.
// 여러 시간 윈도우의 합계를 전체 시간으로 나누어 초당 속도를 계산한다.
type RateStat struct {
	mu               sync.Mutex
	samples          []Sample // 시간 윈도우 별 샘플
	numSamples       int      // 유지할 총 샘플 수
	windowSizeMs     int64    // 각 샘플의 시간 간격 (밀리초)
	currentSampleIdx int      // 현재 활성 샘플 인덱스
}

// NewRateStat은 새 RateStat을 생성한다.
// Kafka의 quota.window.num(기본 11)과 quota.window.size.seconds(기본 1)에 대응한다.
func NewRateStat(numSamples int, windowSizeMs int64) *RateStat {
	samples := make([]Sample, numSamples)
	now := time.Now().UnixMilli()
	for i := range samples {
		samples[i].StartTimeMs = now
	}
	return &RateStat{
		samples:      samples,
		numSamples:   numSamples,
		windowSizeMs: windowSizeMs,
	}
}

// Record는 값을 기록한다. 시간 윈도우가 만료되면 다음 샘플로 이동한다.
func (r *RateStat) Record(value float64, timeMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := &r.samples[r.currentSampleIdx]

	// 현재 윈도우가 만료되었으면 다음 윈도우로 이동
	if timeMs-current.StartTimeMs >= r.windowSizeMs {
		r.advanceWindow(timeMs)
		current = &r.samples[r.currentSampleIdx]
	}

	current.Value += value
	current.Count++
}

// advanceWindow는 현재 윈도우를 닫고 새 윈도우를 시작한다.
func (r *RateStat) advanceWindow(timeMs int64) {
	r.currentSampleIdx = (r.currentSampleIdx + 1) % r.numSamples
	r.samples[r.currentSampleIdx] = Sample{
		StartTimeMs: timeMs,
		Value:       0,
		Count:       0,
	}
}

// Rate는 현재 초당 속도를 계산한다.
// 계산식: 전체 샘플 합 / (현재시간 - 가장 오래된 샘플 시작시간) * 1000
func (r *RateStat) Rate(timeMs int64) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 가장 오래된 샘플의 시작 시간 찾기
	oldestIdx := (r.currentSampleIdx + 1) % r.numSamples
	oldestStartMs := r.samples[oldestIdx].StartTimeMs

	// 전체 시간 범위
	elapsedMs := float64(timeMs - oldestStartMs)
	if elapsedMs <= 0 {
		return 0
	}

	// 전체 샘플 합계 (현재 샘플 포함)
	var totalValue float64
	for i, s := range r.samples {
		if i != oldestIdx { // 가장 오래된 샘플은 부분적일 수 있으므로 포함
			totalValue += s.Value
		}
	}

	// 초당 속도 계산
	return totalValue / (elapsedMs / 1000.0)
}

// WindowSize는 현재 윈도우 크기를 반환한다 (쓰로틀 시간 계산에 사용).
func (r *RateStat) WindowSize(timeMs int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	oldestIdx := (r.currentSampleIdx + 1) % r.numSamples
	return timeMs - r.samples[oldestIdx].StartTimeMs
}

// --- 쿼터 엔티티 ---

// QuotaEntity는 쿼터 적용 대상을 나타낸다.
// Kafka의 KafkaQuotaEntity(UserEntity, ClientIdEntity)에 대응한다.
type QuotaEntity struct {
	User     string // 빈 문자열이면 user 쿼터 미적용
	ClientID string // 빈 문자열이면 client-id 쿼터 미적용
}

func (e QuotaEntity) String() string {
	parts := []string{}
	if e.User != "" {
		parts = append(parts, "user="+e.User)
	}
	if e.ClientID != "" {
		parts = append(parts, "client-id="+e.ClientID)
	}
	if len(parts) == 0 {
		return "<default>"
	}
	return strings.Join(parts, ", ")
}

// --- 쿼터 매니저 ---

// QuotaManager는 ClientQuotaManager의 핵심 동작을 시뮬레이션한다.
type QuotaManager struct {
	mu sync.RWMutex

	quotaType string // "Produce", "Fetch", "Request"

	// 계층적 쿼터 저장소
	// Kafka의 DefaultQuotaCallback.overriddenQuotas에 대응
	quotas map[QuotaEntity]float64

	// 클라이언트별 Rate 센서
	sensors map[QuotaEntity]*RateStat

	// 시간 윈도우 설정
	numSamples   int
	windowSizeMs int64

	// 쓰로틀된 채널 관리
	throttledChannels chan *ThrottledChannel
}

// ThrottledChannel은 쿼터 위반으로 뮤트된 채널을 나타낸다.
// Kafka의 ThrottledChannel(Delayed 인터페이스)에 대응한다.
type ThrottledChannel struct {
	Entity       QuotaEntity
	ThrottleTime time.Duration
	EndTime      time.Time
}

// NewQuotaManager는 새 쿼터 매니저를 생성한다.
func NewQuotaManager(quotaType string, numSamples int, windowSizeSec int) *QuotaManager {
	qm := &QuotaManager{
		quotaType:         quotaType,
		quotas:            make(map[QuotaEntity]float64),
		sensors:           make(map[QuotaEntity]*RateStat),
		numSamples:        numSamples,
		windowSizeMs:      int64(windowSizeSec * 1000),
		throttledChannels: make(chan *ThrottledChannel, 100),
	}

	// ThrottledChannelReaper 시작
	go qm.reaperLoop()

	return qm
}

// reaperLoop는 만료된 ThrottledChannel을 처리한다.
// Kafka의 ThrottledChannelReaper.doWork()에 대응한다.
func (qm *QuotaManager) reaperLoop() {
	for tc := range qm.throttledChannels {
		remaining := time.Until(tc.EndTime)
		if remaining > 0 {
			time.Sleep(remaining)
		}
		fmt.Printf("    [Reaper] 채널 언뮤트: %s (쓰로틀 %v 완료)\n", tc.Entity, tc.ThrottleTime)
	}
}

// SetQuota는 특정 엔티티의 쿼터를 설정한다.
// Kafka의 ClientQuotaManager.updateQuota()에 대응한다.
func (qm *QuotaManager) SetQuota(entity QuotaEntity, bytesPerSec float64) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	qm.quotas[entity] = bytesPerSec
	fmt.Printf("  쿼터 설정: [%s] %s = %.0f bytes/sec\n", qm.quotaType, entity, bytesPerSec)
}

// findQuota는 계층적 쿼터 매칭을 수행한다.
// Kafka의 DefaultQuotaCallback.findQuota()에 대응한다.
// 우선순위: user+client-id > user+default > default+client-id > user > default_user > client-id > default_client
func (qm *QuotaManager) findQuota(user, clientID string) (float64, bool) {
	// 1단계: /config/users/<user>/clients/<client-id>
	if q, ok := qm.quotas[QuotaEntity{User: user, ClientID: clientID}]; ok {
		return q, true
	}
	// 2단계: /config/users/<user>/clients/<default>
	if q, ok := qm.quotas[QuotaEntity{User: user, ClientID: "<default>"}]; ok {
		return q, true
	}
	// 3단계: /config/users/<user>
	if q, ok := qm.quotas[QuotaEntity{User: user}]; ok {
		return q, true
	}
	// 4단계: /config/users/<default>/clients/<client-id>
	if q, ok := qm.quotas[QuotaEntity{User: "<default>", ClientID: clientID}]; ok {
		return q, true
	}
	// 5단계: /config/users/<default>/clients/<default>
	if q, ok := qm.quotas[QuotaEntity{User: "<default>", ClientID: "<default>"}]; ok {
		return q, true
	}
	// 6단계: /config/users/<default>
	if q, ok := qm.quotas[QuotaEntity{User: "<default>"}]; ok {
		return q, true
	}
	// 7단계: /config/clients/<client-id>
	if q, ok := qm.quotas[QuotaEntity{ClientID: clientID}]; ok {
		return q, true
	}
	// 8단계: /config/clients/<default>
	if q, ok := qm.quotas[QuotaEntity{ClientID: "<default>"}]; ok {
		return q, true
	}
	return 0, false
}

// getOrCreateSensor는 엔티티에 대한 Rate 센서를 조회하거나 생성한다.
// Kafka의 ClientQuotaManager.getOrCreateQuotaSensors()에 대응한다.
func (qm *QuotaManager) getOrCreateSensor(entity QuotaEntity) *RateStat {
	if sensor, ok := qm.sensors[entity]; ok {
		return sensor
	}
	sensor := NewRateStat(qm.numSamples, qm.windowSizeMs)
	qm.sensors[entity] = sensor
	return sensor
}

// RecordAndGetThrottleTimeMs는 값을 기록하고 쓰로틀 시간을 반환한다.
// Kafka의 ClientQuotaManager.recordAndGetThrottleTimeMs()에 대응한다.
func (qm *QuotaManager) RecordAndGetThrottleTimeMs(user, clientID string, value float64) int64 {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	entity := QuotaEntity{User: user, ClientID: clientID}
	quotaLimit, hasQuota := qm.findQuota(user, clientID)
	if !hasQuota {
		return 0 // 쿼터 없음
	}

	sensor := qm.getOrCreateSensor(entity)
	now := time.Now().UnixMilli()
	sensor.Record(value, now)

	// 현재 Rate 확인
	currentRate := sensor.Rate(now)
	if currentRate <= quotaLimit {
		return 0 // 쿼터 이내
	}

	// 쿼터 위반 → 쓰로틀 시간 계산
	// 공식: X = (O - T) / T * W
	// O: 관측된 Rate, T: 목표 Rate, W: 윈도우 크기
	diff := currentRate - quotaLimit
	windowMs := float64(sensor.WindowSize(now))
	throttleTimeMs := int64(math.Round(diff / quotaLimit * windowMs))
	if throttleTimeMs < 0 {
		throttleTimeMs = 0
	}

	return throttleTimeMs
}

// Throttle은 채널을 뮤트한다.
// Kafka의 ClientQuotaManager.throttle()에 대응한다.
func (qm *QuotaManager) Throttle(user, clientID string, throttleTimeMs int64) {
	if throttleTimeMs > 0 {
		entity := QuotaEntity{User: user, ClientID: clientID}
		tc := &ThrottledChannel{
			Entity:       entity,
			ThrottleTime: time.Duration(throttleTimeMs) * time.Millisecond,
			EndTime:      time.Now().Add(time.Duration(throttleTimeMs) * time.Millisecond),
		}
		fmt.Printf("    [Throttle] 채널 뮤트: %s, 쓰로틀 시간: %dms\n", entity, throttleTimeMs)
		qm.throttledChannels <- tc
	}
}

// --- TokenBucket (ControllerMutation 쿼터) ---

// TokenBucket은 토큰 버킷 알고리즘을 구현한다.
// Kafka의 TokenBucket (stats/TokenBucket.java)에 대응한다.
type TokenBucket struct {
	mu         sync.Mutex
	tokens     float64 // 현재 토큰 수
	rate       float64 // 초당 토큰 충전 속도
	maxTokens  float64 // 최대 토큰 수
	lastTimeMs int64   // 마지막 토큰 충전 시각
}

// NewTokenBucket은 새 토큰 버킷을 생성한다.
func NewTokenBucket(rate float64) *TokenBucket {
	return &TokenBucket{
		tokens:     rate, // 초기 토큰 = 1초분
		rate:       rate,
		maxTokens:  rate * 10, // 최대 10초분 버스트 허용
		lastTimeMs: time.Now().UnixMilli(),
	}
}

// TryConsume은 토큰을 소비하고 쓰로틀 시간을 반환한다.
// Kafka의 ControllerMutationQuotaManager.recordAndGetThrottleTimeMs()에 대응한다.
func (tb *TokenBucket) TryConsume(permits float64) int64 {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now().UnixMilli()

	// 경과 시간만큼 토큰 충전
	elapsed := float64(now-tb.lastTimeMs) / 1000.0
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastTimeMs = now

	// 쿼터 검사: 현재 토큰이 양수인지 확인 (Kafka는 소비 전에 검사)
	if tb.tokens <= 0 {
		// 토큰 부족 → 쓰로틀 시간 계산
		// 공식: -tokens / rate * 1000
		throttleTimeMs := int64(math.Round(-tb.tokens / tb.rate * 1000))
		return throttleTimeMs
	}

	// 토큰 소비
	tb.tokens -= permits

	return 0 // 쿼터 이내
}

// =============================================================================
// 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Kafka 쿼터 관리 시스템 시뮬레이션 ===")
	fmt.Println()

	// --- 데모 1: 계층적 쿼터 매칭 ---
	demo1HierarchicalQuotaMatching()

	// --- 데모 2: Rate 기반 쓰로틀링 ---
	demo2RateBasedThrottling()

	// --- 데모 3: TokenBucket (ControllerMutation 쿼터) ---
	demo3TokenBucketQuota()

	// --- 데모 4: 쓰로틀 시간 계산 공식 ---
	demo4ThrottleTimeCalculation()

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}

func demo1HierarchicalQuotaMatching() {
	fmt.Println("--- 데모 1: 계층적 쿼터 매칭 ---")
	fmt.Println()

	qm := NewQuotaManager("Produce", 11, 1)

	// 계층적 쿼터 설정
	qm.SetQuota(QuotaEntity{ClientID: "<default>"}, 1000000)                            // 8순위: 모든 클라이언트 기본
	qm.SetQuota(QuotaEntity{User: "<default>"}, 2000000)                                // 6순위: 모든 사용자 기본
	qm.SetQuota(QuotaEntity{User: "alice"}, 5000000)                                    // 3순위: alice 전용
	qm.SetQuota(QuotaEntity{User: "alice", ClientID: "producer-1"}, 10000000)           // 1순위: alice+producer-1
	qm.SetQuota(QuotaEntity{User: "<default>", ClientID: "<default>"}, 1500000)         // 5순위: 기본 user+client

	fmt.Println()

	// 매칭 테스트
	testCases := []struct {
		user     string
		clientID string
		expected string
	}{
		{"alice", "producer-1", "10MB/s (1순위: user+client-id 매칭)"},
		{"alice", "producer-2", "5MB/s (3순위: user 매칭)"},
		{"bob", "producer-1", "1.5MB/s (5순위: default user+default client 매칭)"},
		{"bob", "consumer-1", "1.5MB/s (5순위: default user+default client 매칭)"},
	}

	for _, tc := range testCases {
		quota, found := qm.findQuota(tc.user, tc.clientID)
		status := "없음"
		if found {
			status = fmt.Sprintf("%.0f bytes/sec", quota)
		}
		fmt.Printf("  매칭 결과: user=%s, client-id=%s → %s (%s)\n",
			tc.user, tc.clientID, status, tc.expected)
	}
	fmt.Println()
}

func demo2RateBasedThrottling() {
	fmt.Println("--- 데모 2: Rate 기반 쓰로틀링 ---")
	fmt.Println()

	qm := NewQuotaManager("Produce", 11, 1)

	// 쿼터 설정: alice는 초당 100 bytes
	qm.SetQuota(QuotaEntity{User: "alice"}, 100)
	fmt.Println()

	// 연속 요청 시뮬레이션
	fmt.Println("  alice가 연속으로 데이터를 전송합니다:")
	for i := 0; i < 5; i++ {
		value := float64(50) // 50 bytes씩 전송
		throttleMs := qm.RecordAndGetThrottleTimeMs("alice", "prod-1", value)

		if throttleMs > 0 {
			fmt.Printf("    요청 %d: %.0f bytes 전송 → 쿼터 초과! 쓰로틀 %dms\n", i+1, value, throttleMs)
			qm.Throttle("alice", "prod-1", throttleMs)
		} else {
			fmt.Printf("    요청 %d: %.0f bytes 전송 → OK (쿼터 이내)\n", i+1, value)
		}
		time.Sleep(100 * time.Millisecond) // 100ms 간격
	}
	fmt.Println()

	// Reaper가 처리할 시간을 약간 대기
	time.Sleep(200 * time.Millisecond)
}

func demo3TokenBucketQuota() {
	fmt.Println("--- 데모 3: TokenBucket (ControllerMutation 쿼터) ---")
	fmt.Println()

	// 초당 5개 변경(mutation) 허용
	tb := NewTokenBucket(5.0)
	fmt.Printf("  TokenBucket 생성: rate=5 mutations/sec, 초기 토큰=5\n\n")

	// 관리 작업 시뮬레이션
	operations := []struct {
		name    string
		permits float64
	}{
		{"토픽 생성 (3파티션)", 3},
		{"토픽 생성 (5파티션)", 5},
		{"토픽 삭제", 1},
		{"토픽 생성 (10파티션)", 10}, // 이 요청은 쿼터 초과 예상
	}

	for _, op := range operations {
		throttleMs := tb.TryConsume(op.permits)
		if throttleMs > 0 {
			fmt.Printf("  %s (permits=%.0f) → 쿼터 초과! Strict 모드에서는 THROTTLING_QUOTA_EXCEEDED 반환\n",
				op.name, op.permits)
			fmt.Printf("    쓰로틀 시간: %dms (토큰 복구 대기)\n", throttleMs)
		} else {
			fmt.Printf("  %s (permits=%.0f) → OK\n", op.name, op.permits)
		}
	}
	fmt.Println()
}

func demo4ThrottleTimeCalculation() {
	fmt.Println("--- 데모 4: 쓰로틀 시간 계산 공식 ---")
	fmt.Println()

	// Kafka의 QuotaUtils.throttleTime() 공식:
	// X = (O - T) / T * W
	// O: 관측된 Rate, T: 목표 Rate (quota bound), W: 윈도우 크기

	testCases := []struct {
		observedRate float64
		quotaBound   float64
		windowMs     float64
	}{
		{150, 100, 10000},  // 150 bytes/sec, 쿼터 100, 윈도우 10초
		{200, 100, 10000},  // 200 bytes/sec, 쿼터 100, 윈도우 10초
		{1000, 100, 10000}, // 1000 bytes/sec, 쿼터 100, 윈도우 10초
		{110, 100, 10000},  // 약간 초과
	}

	fmt.Println("  공식: throttleTime = (observedRate - quotaBound) / quotaBound * windowSize")
	fmt.Println()
	fmt.Printf("  %-15s %-12s %-12s %s\n", "관측 Rate", "쿼터 한계", "윈도우(ms)", "쓰로틀 시간(ms)")
	fmt.Println("  " + strings.Repeat("-", 60))

	for _, tc := range testCases {
		diff := tc.observedRate - tc.quotaBound
		throttleMs := math.Round(diff / tc.quotaBound * tc.windowMs)
		fmt.Printf("  %-15.0f %-12.0f %-12.0f %.0f\n",
			tc.observedRate, tc.quotaBound, tc.windowMs, throttleMs)
	}

	fmt.Println()
	fmt.Println("  해석: Rate가 쿼터의 2배이면 윈도우 크기만큼 대기해야 함")
	fmt.Println("        Rate가 쿼터의 1.5배이면 윈도우의 50%만큼 대기")
	fmt.Println()
}
