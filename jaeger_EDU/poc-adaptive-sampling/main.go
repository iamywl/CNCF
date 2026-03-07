// Jaeger PoC: 적응형 샘플링(Adaptive Sampling) 시뮬레이션
//
// Jaeger의 적응형 샘플링은 각 서비스/오퍼레이션의 트래픽 패턴에 따라
// 샘플링 확률을 자동으로 조정하여 targetSamplesPerSecond를 유지한다.
//
// 실제 Jaeger 소스 참조:
//   - internal/sampling/samplingstrategy/adaptive/post_aggregator.go:
//     PostAggregator, calculateProbability(), withinTolerance()
//   - internal/sampling/samplingstrategy/adaptive/options.go:
//     Options (TargetSamplesPerSecond, DeltaTolerance, MinSamplingProbability 등)
//   - internal/sampling/samplingstrategy/adaptive/calculationstrategy/
//     percentage_increase_capped_calculator.go:
//     PercentageIncreaseCappedCalculator.Calculate()
//   - internal/sampling/samplingstrategy/adaptive/weightvectorcache.go:
//     WeightVectorCache.GetWeights() — w(i) = i^4, 정규화
//
// 핵심 알고리즘:
//   newProb = prevProb × (targetQPS / curQPS)
//   단, 증가율은 50%로 제한 (PercentageIncreaseCappedCalculator)
//   DeltaTolerance(±30%) 이내면 조정하지 않음
//   MinSamplingProbability 이하로 내려가지 않음

package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ============================================================
// 설정 (실제 Jaeger adaptive/options.go DefaultOptions() 기반)
// ============================================================

// AdaptiveOptions는 적응형 샘플링 설정
// 실제 소스: adaptive/options.go Options 구조체
type AdaptiveOptions struct {
	// TargetSamplesPerSecond: 오퍼레이션당 목표 초당 샘플 수
	TargetSamplesPerSecond float64

	// DeltaTolerance: 실제 QPS가 목표 대비 이 비율 이내면 조정하지 않음 (기본 0.3 = 30%)
	DeltaTolerance float64

	// CalculationInterval: 확률 재계산 주기
	CalculationInterval time.Duration

	// AggregationBuckets: 메모리에 유지하는 집계 버킷 수
	AggregationBuckets int

	// BucketsForCalculation: 가중 QPS 계산에 사용하는 버킷 수
	BucketsForCalculation int

	// InitialSamplingProbability: 새 오퍼레이션의 초기 샘플링 확률
	InitialSamplingProbability float64

	// MinSamplingProbability: 최소 샘플링 확률 (이 아래로 내려가지 않음)
	MinSamplingProbability float64

	// PercentageIncreaseCap: 확률 증가 시 최대 증가율 제한 (기본 0.5 = 50%)
	PercentageIncreaseCap float64
}

// DefaultOptions는 Jaeger의 기본 설정을 반환한다
// 실제 소스: adaptive/options.go DefaultOptions()
func DefaultOptions() AdaptiveOptions {
	return AdaptiveOptions{
		TargetSamplesPerSecond:     1.0,
		DeltaTolerance:            0.3,    // ±30%
		CalculationInterval:       time.Minute,
		AggregationBuckets:        10,
		BucketsForCalculation:     1,
		InitialSamplingProbability: 0.001,
		MinSamplingProbability:    1e-5,   // 1/100,000
		PercentageIncreaseCap:     0.5,    // 50%
	}
}

// ============================================================
// PercentageIncreaseCappedCalculator
// 실제 소스: calculationstrategy/percentage_increase_capped_calculator.go
// ============================================================

// ProbabilityCalculator는 새 확률을 계산하는 인터페이스
// 실제 소스: calculationstrategy/interface.go
type ProbabilityCalculator interface {
	Calculate(targetQPS, curQPS, prevProbability float64) float64
}

// PercentageIncreaseCappedCalculator는 확률 증가를 일정 비율로 제한한다.
//
// 실제 소스: calculationstrategy/percentage_increase_capped_calculator.go
//
// 핵심 로직:
//   factor = targetQPS / curQPS
//   newProb = prevProb × factor
//   if factor > 1.0:  // 증가하는 경우
//     percentIncrease = (newProb - prevProb) / prevProb
//     if percentIncrease > cap:
//       newProb = prevProb + (prevProb × cap)
//   // 감소하는 경우는 즉시 적용 (오버샘플링 방어)
type PercentageIncreaseCappedCalculator struct {
	percentageIncreaseCap float64
}

func NewPercentageIncreaseCappedCalculator(cap float64) *PercentageIncreaseCappedCalculator {
	if cap == 0 {
		cap = 0.5 // defaultPercentageIncreaseCap
	}
	return &PercentageIncreaseCappedCalculator{
		percentageIncreaseCap: cap,
	}
}

// Calculate는 실제 Jaeger의 PercentageIncreaseCappedCalculator.Calculate()를 그대로 재현한다.
func (c *PercentageIncreaseCappedCalculator) Calculate(targetQPS, curQPS, prevProbability float64) float64 {
	factor := targetQPS / curQPS
	newProbability := prevProbability * factor

	// 확률이 증가하는 경우에만 증가율을 제한한다.
	// 감소하는 경우(오버샘플링)에는 즉시 newProbability를 적용한다.
	if factor > 1.0 {
		percentIncrease := (newProbability - prevProbability) / prevProbability
		if percentIncrease > c.percentageIncreaseCap {
			newProbability = prevProbability + (prevProbability * c.percentageIncreaseCap)
		}
	}
	return newProbability
}

// ============================================================
// WeightVectorCache
// 실제 소스: adaptive/weightvectorcache.go
// ============================================================

// WeightVectorCache는 가중치 벡터를 캐싱한다.
// 가중치: w(i) = i^4 (최근 버킷에 높은 가중치)
// 실제 소스: adaptive/weightvectorcache.go
type WeightVectorCache struct {
	cache map[int][]float64
}

func NewWeightVectorCache() *WeightVectorCache {
	return &WeightVectorCache{
		cache: make(map[int][]float64),
	}
}

// GetWeights는 주어진 길이의 정규화된 가중치 벡터를 반환한다.
// 가중치: w(i) = i^4, i=length..1 (인덱스 0이 가장 최근, 가장 큰 가중치)
// 실제 소스: weightvectorcache.go GetWeights()
func (c *WeightVectorCache) GetWeights(length int) []float64 {
	if weights, ok := c.cache[length]; ok {
		return weights
	}

	weights := make([]float64, 0, length)
	var sum float64
	for i := length; i > 0; i-- {
		w := math.Pow(float64(i), 4)
		weights = append(weights, w)
		sum += w
	}
	// 정규화
	for i := range weights {
		weights[i] /= sum
	}

	c.cache[length] = weights
	return weights
}

// ============================================================
// ThroughputBucket: 처리량 버킷
// 실제 소스: adaptive/post_aggregator.go throughputBucket
// ============================================================

// ThroughputBucket은 일정 시간 동안의 처리량 데이터
type ThroughputBucket struct {
	// service → operation → count
	Throughput map[string]map[string]int64
	Interval   time.Duration
	EndTime    time.Time
}

// ============================================================
// AdaptiveSamplingProcessor: 적응형 샘플링 처리기
// ============================================================

// OperationState는 오퍼레이션별 상태
type OperationState struct {
	CurrentProbability float64
	CurrentQPS         float64
	TargetQPS          float64
	LastAdjustment     string // 조정 사유
}

// AdaptiveSamplingProcessor는 적응형 샘플링 확률을 계산한다.
// 실제 소스: adaptive/post_aggregator.go PostAggregator
type AdaptiveSamplingProcessor struct {
	opts AdaptiveOptions

	// service → operation → probability
	probabilities map[string]map[string]float64

	// service → operation → QPS
	qps map[string]map[string]float64

	// 처리량 버킷 (최신이 인덱스 0)
	throughputs []*ThroughputBucket

	calculator  ProbabilityCalculator
	weightCache *WeightVectorCache

	// 시뮬레이션용 상태 추적
	history map[string]map[string][]OperationState
}

func NewAdaptiveSamplingProcessor(opts AdaptiveOptions) *AdaptiveSamplingProcessor {
	return &AdaptiveSamplingProcessor{
		opts:          opts,
		probabilities: make(map[string]map[string]float64),
		qps:           make(map[string]map[string]float64),
		throughputs:   make([]*ThroughputBucket, 0),
		calculator:    NewPercentageIncreaseCappedCalculator(opts.PercentageIncreaseCap),
		weightCache:   NewWeightVectorCache(),
		history:       make(map[string]map[string][]OperationState),
	}
}

// AddThroughputBucket은 새 처리량 버킷을 추가한다
// 실제 소스: post_aggregator.go prependThroughputBucket()
func (p *AdaptiveSamplingProcessor) AddThroughputBucket(bucket *ThroughputBucket) {
	p.throughputs = append([]*ThroughputBucket{bucket}, p.throughputs...)
	if len(p.throughputs) > p.opts.AggregationBuckets {
		p.throughputs = p.throughputs[:p.opts.AggregationBuckets]
	}
}

// throughputToQPS는 처리량을 QPS로 변환한다
// 실제 소스: post_aggregator.go throughputToQPS()
func (p *AdaptiveSamplingProcessor) throughputToQPS() map[string]map[string][]float64 {
	qps := make(map[string]map[string][]float64)

	for _, bucket := range p.throughputs {
		for svc, operations := range bucket.Throughput {
			if _, ok := qps[svc]; !ok {
				qps[svc] = make(map[string][]float64)
			}
			for op, count := range operations {
				if len(qps[svc][op]) >= p.opts.BucketsForCalculation {
					continue
				}
				seconds := float64(bucket.Interval) / float64(time.Second)
				qps[svc][op] = append(qps[svc][op], float64(count)/seconds)
			}
		}
	}
	return qps
}

// calculateWeightedQPS는 가중 평균 QPS를 계산한다
// 실제 소스: post_aggregator.go calculateWeightedQPS()
func (p *AdaptiveSamplingProcessor) calculateWeightedQPS(allQPS []float64) float64 {
	if len(allQPS) == 0 {
		return 0
	}
	weights := p.weightCache.GetWeights(len(allQPS))
	var qps float64
	for i := range allQPS {
		qps += allQPS[i] * weights[i]
	}
	return qps
}

// withinTolerance는 실제 QPS가 목표 대비 허용 범위 내인지 확인한다
// 실제 소스: post_aggregator.go withinTolerance()
func (p *AdaptiveSamplingProcessor) withinTolerance(actual, expected float64) bool {
	if expected == 0 {
		return false
	}
	return math.Abs(actual-expected)/expected < p.opts.DeltaTolerance
}

// calculateProbability는 단일 오퍼레이션의 새 확률을 계산한다
// 실제 소스: post_aggregator.go calculateProbability()
func (p *AdaptiveSamplingProcessor) calculateProbability(service, operation string, curQPS float64) (float64, string) {
	oldProbability := p.opts.InitialSamplingProbability

	if opProbs, ok := p.probabilities[service]; ok {
		if prob, ok := opProbs[operation]; ok {
			oldProbability = prob
		}
	}

	// DeltaTolerance 체크: 목표 범위 내면 조정하지 않음
	if p.withinTolerance(curQPS, p.opts.TargetSamplesPerSecond) {
		return oldProbability, "DeltaTolerance 이내 - 조정 불필요"
	}

	var newProbability float64
	var reason string

	if floatEquals(curQPS, 0) {
		// QPS가 0이면 확률을 2배로 증가 (최소 하나의 span이라도 샘플링되도록)
		newProbability = oldProbability * 2.0
		reason = "QPS=0, 확률 2배 증가"
	} else {
		newProbability = p.calculator.Calculate(p.opts.TargetSamplesPerSecond, curQPS, oldProbability)
		factor := p.opts.TargetSamplesPerSecond / curQPS
		if factor > 1.0 {
			// 증가하는 경우
			percentIncrease := (newProbability - oldProbability) / oldProbability * 100
			if percentIncrease > p.opts.PercentageIncreaseCap*100 {
				reason = fmt.Sprintf("증가 제한: %.1f%% → %.1f%% (cap %.0f%%)", percentIncrease, p.opts.PercentageIncreaseCap*100, p.opts.PercentageIncreaseCap*100)
			} else {
				reason = fmt.Sprintf("증가: factor=%.3f", factor)
			}
		} else {
			reason = fmt.Sprintf("감소: factor=%.3f (즉시 적용)", factor)
		}
	}

	// 범위 제한: [MinSamplingProbability, 1.0]
	newProbability = math.Min(1.0, math.Max(p.opts.MinSamplingProbability, newProbability))

	return newProbability, reason
}

// RunCalculation은 전체 확률 재계산을 수행한다
// 실제 소스: post_aggregator.go calculateProbabilitiesAndQPS()
func (p *AdaptiveSamplingProcessor) RunCalculation() {
	svcOpQPS := p.throughputToQPS()

	for svc, opQPS := range svcOpQPS {
		if _, ok := p.probabilities[svc]; !ok {
			p.probabilities[svc] = make(map[string]float64)
		}
		if _, ok := p.qps[svc]; !ok {
			p.qps[svc] = make(map[string]float64)
		}
		if _, ok := p.history[svc]; !ok {
			p.history[svc] = make(map[string][]OperationState)
		}

		for op, qpsBuckets := range opQPS {
			avgQPS := p.calculateWeightedQPS(qpsBuckets)
			p.qps[svc][op] = avgQPS

			newProb, reason := p.calculateProbability(svc, op, avgQPS)
			p.probabilities[svc][op] = newProb

			// 히스토리 기록
			p.history[svc][op] = append(p.history[svc][op], OperationState{
				CurrentProbability: newProb,
				CurrentQPS:         avgQPS,
				TargetQPS:          p.opts.TargetSamplesPerSecond,
				LastAdjustment:     reason,
			})
		}
	}
}

func floatEquals(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// ============================================================
// 트래픽 시뮬레이터
// ============================================================

// TrafficPattern은 트래픽 패턴을 정의
type TrafficPattern struct {
	Name     string
	Service  string
	// operation → 반복(iteration)별 초당 요청 수
	OperationQPS map[string][]float64
}

func generateThroughputBucket(pattern TrafficPattern, iteration int, interval time.Duration) *ThroughputBucket {
	throughput := make(map[string]map[string]int64)
	throughput[pattern.Service] = make(map[string]int64)

	for op, qpsPattern := range pattern.OperationQPS {
		idx := iteration % len(qpsPattern)
		qps := qpsPattern[idx]
		// QPS * interval(초) = 해당 구간의 총 요청 수
		count := int64(qps * interval.Seconds())
		throughput[pattern.Service][op] = count
	}

	return &ThroughputBucket{
		Throughput: throughput,
		Interval:   interval,
		EndTime:    time.Now(),
	}
}

// ============================================================
// 시각화 도구
// ============================================================

func printProbabilityGraph(history []OperationState, operation string, width int) {
	if len(history) == 0 {
		return
	}

	// 확률 값을 로그 스케일로 시각화 (매우 작은 값도 보이도록)
	fmt.Printf("    %s:\n", operation)
	for i, state := range history {
		logProb := -math.Log10(state.CurrentProbability)
		if logProb < 0 {
			logProb = 0
		}
		// 0~5 범위를 width 칸에 매핑 (1.0 ~ 1e-5)
		barLen := int(float64(width) * (1.0 - logProb/5.0))
		if barLen < 0 {
			barLen = 0
		}
		if barLen > width {
			barLen = width
		}

		bar := strings.Repeat("█", barLen) + strings.Repeat("░", width-barLen)
		fmt.Printf("      [%2d] %s prob=%.6f qps=%.1f %s\n",
			i, bar, state.CurrentProbability, state.CurrentQPS, state.LastAdjustment)
	}
}

// ============================================================
// 메인
// ============================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  Jaeger PoC: 적응형 샘플링(Adaptive Sampling) 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// ----------------------------------------------------------
	// 1. 적응형 샘플링 알고리즘 설명
	// ----------------------------------------------------------
	fmt.Println("\n[1단계] 적응형 샘플링 알고리즘 개요")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  적응형 샘플링은 각 서비스/오퍼레이션의 트래픽에 맞춰
  샘플링 확률을 자동으로 조정하여 목표 QPS를 유지한다.

  실제 소스: adaptive/post_aggregator.go

  핵심 공식:
    newProb = prevProb × (targetQPS / curQPS)

  제약 조건:
    1. 증가율 제한 (PercentageIncreaseCap = 50%)
       → 급격한 확률 증가 방지 (오버샘플링 방어)
    2. 감소는 즉시 적용
       → 오버샘플링을 빠르게 교정
    3. DeltaTolerance (±30%)
       → 목표 근처면 불필요한 변동 방지
    4. MinSamplingProbability (1e-5)
       → 확률이 0에 수렴하는 것 방지

  ┌──────────────────────────────────────────────────────┐
  │  반복 계산 루프 (calculateProbability)                │
  │                                                       │
  │  curQPS ←────── throughputToQPS()                     │
  │     │                                                 │
  │     ▼                                                 │
  │  withinTolerance(curQPS, targetQPS)?                  │
  │     │ Yes → 변경 없음                                  │
  │     │ No  ↓                                           │
  │     ▼                                                 │
  │  curQPS == 0?                                         │
  │     │ Yes → newProb = prevProb × 2                    │
  │     │ No  ↓                                           │
  │     ▼                                                 │
  │  calculator.Calculate(targetQPS, curQPS, prevProb)    │
  │     │                                                 │
  │     ▼                                                 │
  │  clamp(MinSamplingProbability, 1.0)                   │
  └──────────────────────────────────────────────────────┘`)

	// ----------------------------------------------------------
	// 2. PercentageIncreaseCappedCalculator 동작 확인
	// ----------------------------------------------------------
	fmt.Println("\n[2단계] PercentageIncreaseCappedCalculator 동작 확인")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  실제 소스: calculationstrategy/percentage_increase_capped_calculator.go

  규칙:
    - factor > 1.0 (증가): percentIncrease가 cap(50%)을 초과하면 제한
    - factor < 1.0 (감소): 즉시 newProbability 적용`)

	calc := NewPercentageIncreaseCappedCalculator(0.5)

	testCases := []struct {
		targetQPS float64
		curQPS    float64
		prevProb  float64
		desc      string
	}{
		{1.0, 100.0, 0.1, "높은 QPS → 확률 대폭 감소"},
		{1.0, 0.5, 0.1, "낮은 QPS → 확률 증가 (cap 적용)"},
		{1.0, 0.9, 0.1, "목표 근처 → 소폭 증가"},
		{1.0, 2.0, 0.1, "2배 초과 → 50% 감소"},
		{1.0, 0.01, 0.001, "매우 낮은 QPS → 증가 cap 적용"},
	}

	fmt.Printf("\n  %-30s %10s %10s %10s %10s %10s\n",
		"설명", "targetQPS", "curQPS", "prevProb", "newProb", "변화율")
	fmt.Println("  " + strings.Repeat("-", 85))

	for _, tc := range testCases {
		newProb := calc.Calculate(tc.targetQPS, tc.curQPS, tc.prevProb)
		change := (newProb - tc.prevProb) / tc.prevProb * 100
		fmt.Printf("  %-30s %10.1f %10.2f %10.6f %10.6f %+9.1f%%\n",
			tc.desc, tc.targetQPS, tc.curQPS, tc.prevProb, newProb, change)
	}

	// ----------------------------------------------------------
	// 3. WeightVectorCache 가중치 시각화
	// ----------------------------------------------------------
	fmt.Println("\n[3단계] WeightVectorCache: 가중 QPS 계산")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  실제 소스: adaptive/weightvectorcache.go

  가중치: w(i) = i^4, i = length..1 (최근 버킷이 가장 큰 가중치)
  정규화: 합이 1.0이 되도록`)

	wvc := NewWeightVectorCache()

	for _, n := range []int{1, 3, 5, 10} {
		weights := wvc.GetWeights(n)
		fmt.Printf("\n  버킷 %d개 가중치:\n    ", n)
		var sum float64
		for i, w := range weights {
			sum += w
			label := "최신"
			if i == len(weights)-1 {
				label = "가장 오래됨"
			} else if i > 0 {
				label = fmt.Sprintf("-%d", i)
			}
			bar := strings.Repeat("█", int(w*50))
			fmt.Printf("[%s] %.4f %s\n    ", label, w, bar)
		}
		fmt.Printf("합계: %.4f\n", sum)
	}

	// ----------------------------------------------------------
	// 4. 시나리오 1: 일정한 고트래픽 → 확률 수렴
	// ----------------------------------------------------------
	fmt.Println("\n[4단계] 시나리오 1: 일정한 고트래픽에서 확률 수렴")
	fmt.Println(strings.Repeat("-", 60))

	opts := DefaultOptions()
	opts.TargetSamplesPerSecond = 1.0
	opts.BucketsForCalculation = 3
	opts.AggregationBuckets = 10
	opts.CalculationInterval = 60 * time.Second // 1분

	processor1 := NewAdaptiveSamplingProcessor(opts)

	// 일정한 QPS: order-service의 CreateOrder가 초당 500 요청
	pattern1 := TrafficPattern{
		Name:    "일정한 고트래픽",
		Service: "order-service",
		OperationQPS: map[string][]float64{
			"CreateOrder": {500, 500, 500, 500, 500, 500, 500, 500, 500, 500},
			"GetOrder":    {100, 100, 100, 100, 100, 100, 100, 100, 100, 100},
		},
	}

	fmt.Printf("  트래픽 패턴: %s\n", pattern1.Name)
	fmt.Printf("  서비스: %s\n", pattern1.Service)
	fmt.Printf("  목표 QPS: %.1f/sec\n", opts.TargetSamplesPerSecond)
	fmt.Printf("  초기 확률: %.6f\n\n", opts.InitialSamplingProbability)

	for i := 0; i < 20; i++ {
		bucket := generateThroughputBucket(pattern1, i, opts.CalculationInterval)
		processor1.AddThroughputBucket(bucket)
		processor1.RunCalculation()
	}

	// 수렴 결과 출력
	for op := range pattern1.OperationQPS {
		history := processor1.history[pattern1.Service][op]
		printProbabilityGraph(history, op, 40)

		if len(history) > 0 {
			final := history[len(history)-1]
			expectedProb := opts.TargetSamplesPerSecond / final.CurrentQPS
			fmt.Printf("      이론적 수렴값: %.6f (target/QPS = %.1f/%.1f)\n",
				expectedProb, opts.TargetSamplesPerSecond, final.CurrentQPS)
		}
	}

	// ----------------------------------------------------------
	// 5. 시나리오 2: 트래픽 급증 (스파이크)
	// ----------------------------------------------------------
	fmt.Println("\n[5단계] 시나리오 2: 트래픽 급증 (스파이크)")
	fmt.Println(strings.Repeat("-", 60))

	processor2 := NewAdaptiveSamplingProcessor(opts)

	// QPS: 10 → 10 → 10 → 1000 → 1000 → 1000 → 10 → 10 → 10 → 10
	pattern2 := TrafficPattern{
		Name:    "트래픽 스파이크",
		Service: "payment-service",
		OperationQPS: map[string][]float64{
			"ProcessPayment": {10, 10, 10, 1000, 1000, 1000, 10, 10, 10, 10},
		},
	}

	fmt.Printf("  트래픽 패턴: %s\n", pattern2.Name)
	fmt.Println("  QPS 변화: 10→10→10→1000→1000→1000→10→10→10→10")
	fmt.Printf("  목표 QPS: %.1f/sec\n\n", opts.TargetSamplesPerSecond)

	for i := 0; i < 20; i++ {
		bucket := generateThroughputBucket(pattern2, i, opts.CalculationInterval)
		processor2.AddThroughputBucket(bucket)
		processor2.RunCalculation()
	}

	printProbabilityGraph(processor2.history["payment-service"]["ProcessPayment"], "ProcessPayment", 40)

	fmt.Println(`
  관찰:
  - 트래픽 급증 시 확률이 즉시 감소 (감소는 cap 없이 즉시 적용)
  - 트래픽 감소 시 확률이 천천히 증가 (50% cap으로 제한)
  - 이 비대칭 설계가 오버샘플링을 방어한다`)

	// ----------------------------------------------------------
	// 6. 시나리오 3: 점진적 트래픽 증가
	// ----------------------------------------------------------
	fmt.Println("\n[6단계] 시나리오 3: 점진적 트래픽 증가")
	fmt.Println(strings.Repeat("-", 60))

	processor3 := NewAdaptiveSamplingProcessor(opts)

	// QPS: 50 → 100 → 200 → 400 → 800 → 1600 → 3200 → 3200 → 3200 → 3200
	pattern3 := TrafficPattern{
		Name:    "점진적 증가",
		Service: "api-gateway",
		OperationQPS: map[string][]float64{
			"GET /users": {50, 100, 200, 400, 800, 1600, 3200, 3200, 3200, 3200},
		},
	}

	fmt.Printf("  트래픽 패턴: %s\n", pattern3.Name)
	fmt.Println("  QPS 변화: 50→100→200→400→800→1600→3200→3200→3200→3200")
	fmt.Printf("  목표 QPS: %.1f/sec\n\n", opts.TargetSamplesPerSecond)

	for i := 0; i < 20; i++ {
		bucket := generateThroughputBucket(pattern3, i, opts.CalculationInterval)
		processor3.AddThroughputBucket(bucket)
		processor3.RunCalculation()
	}

	printProbabilityGraph(processor3.history["api-gateway"]["GET /users"], "GET /users", 40)

	// ----------------------------------------------------------
	// 7. 시나리오 4: 간헐적 트래픽 (낮은 QPS)
	// ----------------------------------------------------------
	fmt.Println("\n[7단계] 시나리오 4: 간헐적 트래픽 (매우 낮은 QPS)")
	fmt.Println(strings.Repeat("-", 60))

	processor4 := NewAdaptiveSamplingProcessor(opts)

	// QPS: 0 → 0.5 → 0 → 0 → 1 → 0 → 0.5 → 0 → 0 → 0.5
	pattern4 := TrafficPattern{
		Name:    "간헐적 트래픽",
		Service: "batch-service",
		OperationQPS: map[string][]float64{
			"RunBatch": {0, 0.5, 0, 0, 1, 0, 0.5, 0, 0, 0.5},
		},
	}

	fmt.Printf("  트래픽 패턴: %s\n", pattern4.Name)
	fmt.Println("  QPS 변화: 0→0.5→0→0→1→0→0.5→0→0→0.5")
	fmt.Printf("  목표 QPS: %.1f/sec\n\n", opts.TargetSamplesPerSecond)

	for i := 0; i < 20; i++ {
		bucket := generateThroughputBucket(pattern4, i, opts.CalculationInterval)
		processor4.AddThroughputBucket(bucket)
		processor4.RunCalculation()
	}

	printProbabilityGraph(processor4.history["batch-service"]["RunBatch"], "RunBatch", 40)

	fmt.Println(`
  관찰:
  - QPS=0일 때 확률이 2배씩 증가 (최소 1개 span 샘플링 시도)
  - MinSamplingProbability로 하한이 보장됨
  - 간헐적 트래픽에서도 적응적으로 확률 조정`)

	// ----------------------------------------------------------
	// 8. DeltaTolerance 효과 시각화
	// ----------------------------------------------------------
	fmt.Println("\n[8단계] DeltaTolerance 효과 비교")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  DeltaTolerance = 0.3 (±30%):
  실제 QPS가 목표 대비 30% 이내면 확률을 조정하지 않는다.

  소스: post_aggregator.go
    func (p *PostAggregator) withinTolerance(actual, expected float64) bool {
        return math.Abs(actual-expected)/expected < p.DeltaTolerance
    }`)

	// DeltaTolerance=0 vs 0.3 비교
	optsNoDelta := opts
	optsNoDelta.DeltaTolerance = 0.0

	procNoDelta := NewAdaptiveSamplingProcessor(optsNoDelta)
	procWithDelta := NewAdaptiveSamplingProcessor(opts)

	// 약간 변동하는 트래픽: 목표 근처
	fluctuatingPattern := TrafficPattern{
		Name:    "변동 트래픽",
		Service: "test-service",
		OperationQPS: map[string][]float64{
			"TestOp": {100, 95, 105, 98, 102, 110, 90, 100, 95, 105},
		},
	}

	for i := 0; i < 20; i++ {
		bucket := generateThroughputBucket(fluctuatingPattern, i, opts.CalculationInterval)

		procNoDelta.AddThroughputBucket(bucket)
		procNoDelta.RunCalculation()

		bucketCopy := generateThroughputBucket(fluctuatingPattern, i, opts.CalculationInterval)
		procWithDelta.AddThroughputBucket(bucketCopy)
		procWithDelta.RunCalculation()
	}

	fmt.Println("\n  DeltaTolerance = 0 (매번 조정):")
	histNoDelta := procNoDelta.history["test-service"]["TestOp"]
	for i, state := range histNoDelta {
		if i >= 10 {
			break
		}
		fmt.Printf("    [%2d] prob=%.8f qps=%.1f %s\n",
			i, state.CurrentProbability, state.CurrentQPS, state.LastAdjustment)
	}

	fmt.Println("\n  DeltaTolerance = 0.3 (±30% 이내 무시):")
	histWithDelta := procWithDelta.history["test-service"]["TestOp"]
	for i, state := range histWithDelta {
		if i >= 10 {
			break
		}
		fmt.Printf("    [%2d] prob=%.8f qps=%.1f %s\n",
			i, state.CurrentProbability, state.CurrentQPS, state.LastAdjustment)
	}

	toleranceCount := 0
	for _, state := range histWithDelta {
		if strings.Contains(state.LastAdjustment, "DeltaTolerance") {
			toleranceCount++
		}
	}
	fmt.Printf("\n  DeltaTolerance 적용으로 %d/%d 반복에서 불필요한 조정 방지\n",
		toleranceCount, len(histWithDelta))

	// ----------------------------------------------------------
	// 9. 종합 정리
	// ----------------------------------------------------------
	fmt.Println("\n[9단계] 적응형 샘플링 종합 정리")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  Jaeger 적응형 샘플링의 핵심 설계 원칙:

  1. 비대칭 조정 (Asymmetric Adjustment)
     - 감소: 즉시 적용 → 오버샘플링 빠르게 교정
     - 증가: 50% cap → 과도한 샘플링 방지

  2. 안정성 (Stability)
     - DeltaTolerance(±30%): 목표 근처에서 불필요한 진동 방지
     - WeightedQPS: 최근 버킷에 높은 가중치로 순간 변동 완화

  3. 안전장치 (Safety)
     - MinSamplingProbability(1e-5): 확률이 0에 수렴하는 것 방지
     - QPS=0 시 확률 2배: 최소 샘플 보장
     - maxSamplingProbability(1.0): 상한 제한

  4. 실행 구조
     ┌──────────┐  throughput   ┌──────────────┐
     │Collector │─────────────>│  Storage      │
     │(Aggregator)│             │  (buckets)    │
     └──────────┘              └──────┬───────┘
                                      │
                              ┌───────▼───────┐
                              │PostAggregator │
                              │ (Leader only) │
                              └───────┬───────┘
                                      │
                              ┌───────▼───────┐
                              │  Probabilities │
                              │  (per svc/op) │
                              └───────┬───────┘
                                      │
                              ┌───────▼───────┐
                              │ Client SDKs   │
                              │ (폴링으로 수신)│
                              └───────────────┘`)

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("  시뮬레이션 완료")
	fmt.Println(strings.Repeat("=", 80))
}
