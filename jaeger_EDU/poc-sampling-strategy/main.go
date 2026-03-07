// Jaeger PoC: 샘플링 전략(Sampling Strategy) 시뮬레이션
//
// Jaeger는 모든 트레이스를 저장하면 비용이 과다해지므로,
// 다양한 샘플링 전략을 통해 저장할 트레이스를 선택한다.
//
// 실제 Jaeger 소스 참조:
//   - internal/sampling/samplingstrategy/file/strategy.go:
//     strategy, operationStrategy, serviceStrategy 구조체
//   - internal/sampling/samplingstrategy/file/provider.go:
//     "probabilistic", "ratelimiting" 타입 정의
//   - internal/sampling/samplingstrategy/adaptive/: 적응형 샘플링
//
// 세 가지 샘플링 전략:
//   1. Always-on: 모든 트레이스 샘플링
//   2. Probabilistic: 확률적 샘플링 (X% 선택)
//   3. Rate-limiting: 초당 최대 N개 (토큰 버킷 알고리즘)

package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ============================================================
// 샘플러 인터페이스 (Jaeger 클라이언트 SDK 패턴)
// ============================================================

// SamplingDecision은 샘플링 결정 결과
type SamplingDecision struct {
	Sample bool
	Tags   map[string]string
}

// Sampler는 샘플링 전략 인터페이스
type Sampler interface {
	// IsSampled는 주어진 트레이스ID와 오퍼레이션에 대해 샘플링 여부를 결정
	IsSampled(traceID uint64, operation string) SamplingDecision
	// Name은 샘플러의 이름을 반환
	Name() string
}

// ============================================================
// 1. Always-on 샘플러
// ============================================================

// AlwaysOnSampler는 모든 트레이스를 샘플링한다
type AlwaysOnSampler struct{}

func (s *AlwaysOnSampler) IsSampled(_ uint64, _ string) SamplingDecision {
	return SamplingDecision{
		Sample: true,
		Tags: map[string]string{
			"sampler.type":  "const",
			"sampler.param": "1",
		},
	}
}

func (s *AlwaysOnSampler) Name() string { return "Always-On (const=1)" }

// ============================================================
// 2. 확률적 샘플러 (Probabilistic Sampler)
// ============================================================

// ProbabilisticSampler는 설정된 확률로 트레이스를 샘플링한다.
// 실제 Jaeger에서 strategy.Type == "probabilistic" 일 때 사용.
// TraceID의 해시값을 경계값과 비교하여 결정론적으로 샘플링한다.
type ProbabilisticSampler struct {
	samplingRate    float64
	samplingBoundary uint64 // TraceID 비교 경계값
}

func NewProbabilisticSampler(rate float64) *ProbabilisticSampler {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	return &ProbabilisticSampler{
		samplingRate:    rate,
		samplingBoundary: uint64(float64(math.MaxUint64) * rate),
	}
}

// IsSampled는 traceID를 경계값과 비교하여 샘플링 여부를 결정한다.
// 이 방식은 결정론적(deterministic)이라서 동일한 traceID는 항상 같은 결과를 반환한다.
// 분산 시스템에서 같은 트레이스의 모든 span이 동일하게 샘플링되도록 보장한다.
func (s *ProbabilisticSampler) IsSampled(traceID uint64, _ string) SamplingDecision {
	return SamplingDecision{
		Sample: traceID <= s.samplingBoundary,
		Tags: map[string]string{
			"sampler.type":  "probabilistic",
			"sampler.param": fmt.Sprintf("%.4f", s.samplingRate),
		},
	}
}

func (s *ProbabilisticSampler) Name() string {
	return fmt.Sprintf("Probabilistic (rate=%.2f%%)", s.samplingRate*100)
}

// ============================================================
// 3. Rate-Limiting 샘플러 (토큰 버킷 알고리즘)
// ============================================================

// RateLimitingSampler는 초당 최대 N개의 트레이스만 샘플링한다.
// 실제 Jaeger에서 strategy.Type == "ratelimiting" 일 때 사용.
// 토큰 버킷(Token Bucket) 알고리즘을 사용한다.
type RateLimitingSampler struct {
	mu               sync.Mutex
	maxTracesPerSec  float64
	balance          float64   // 현재 토큰 잔액
	maxBalance       float64   // 최대 토큰 수
	lastTick         time.Time // 마지막 토큰 보충 시간
	creditsPerSecond float64   // 초당 토큰 보충 수
}

func NewRateLimitingSampler(maxTracesPerSec float64) *RateLimitingSampler {
	return &RateLimitingSampler{
		maxTracesPerSec:  maxTracesPerSec,
		balance:          maxTracesPerSec, // 초기에 최대 토큰으로 시작
		maxBalance:       maxTracesPerSec, // 버스트 허용량 = 초당 허용량
		lastTick:         time.Now(),
		creditsPerSecond: maxTracesPerSec,
	}
}

// IsSampled는 토큰 버킷에서 토큰을 소비하여 샘플링 여부를 결정한다.
//
// 토큰 버킷 알고리즘:
//   1. 경과 시간에 비례하여 토큰을 보충
//   2. 토큰이 있으면 1개 소비하고 샘플링
//   3. 토큰이 없으면 샘플링하지 않음
func (s *RateLimitingSampler) IsSampled(_ uint64, _ string) SamplingDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(s.lastTick).Seconds()
	s.lastTick = now

	// 토큰 보충: 경과 시간 * 초당 보충률
	s.balance += elapsed * s.creditsPerSecond
	if s.balance > s.maxBalance {
		s.balance = s.maxBalance
	}

	// 토큰 소비
	if s.balance >= 1.0 {
		s.balance -= 1.0
		return SamplingDecision{
			Sample: true,
			Tags: map[string]string{
				"sampler.type":  "ratelimiting",
				"sampler.param": fmt.Sprintf("%.1f", s.maxTracesPerSec),
			},
		}
	}

	return SamplingDecision{
		Sample: false,
		Tags: map[string]string{
			"sampler.type":  "ratelimiting",
			"sampler.param": fmt.Sprintf("%.1f", s.maxTracesPerSec),
		},
	}
}

func (s *RateLimitingSampler) Name() string {
	return fmt.Sprintf("Rate-Limiting (max=%.0f/sec)", s.maxTracesPerSec)
}

// ============================================================
// 4. Per-Operation 샘플러
// ============================================================

// PerOperationSampler는 오퍼레이션별로 다른 샘플링 전략을 적용한다.
// 실제 Jaeger의 serviceStrategy.OperationStrategies 기반.
//
// 소스: file/strategy.go
//   type serviceStrategy struct {
//       Service             string
//       OperationStrategies []*operationStrategy
//       strategy            // 기본 전략
//   }
type PerOperationSampler struct {
	defaultSampler    Sampler
	operationSamplers map[string]Sampler
}

func NewPerOperationSampler(defaultSampler Sampler) *PerOperationSampler {
	return &PerOperationSampler{
		defaultSampler:    defaultSampler,
		operationSamplers: make(map[string]Sampler),
	}
}

func (s *PerOperationSampler) SetOperationSampler(operation string, sampler Sampler) {
	s.operationSamplers[operation] = sampler
}

func (s *PerOperationSampler) IsSampled(traceID uint64, operation string) SamplingDecision {
	if sampler, ok := s.operationSamplers[operation]; ok {
		return sampler.IsSampled(traceID, operation)
	}
	return s.defaultSampler.IsSampled(traceID, operation)
}

func (s *PerOperationSampler) Name() string { return "Per-Operation" }

// ============================================================
// 트래픽 시뮬레이터
// ============================================================

// TrafficResult는 트래픽 시뮬레이션 결과
type TrafficResult struct {
	SamplerName    string
	TotalGenerated int
	TotalSampled   int
	SamplingRate   float64
	PerOperation   map[string]*OperationResult
}

// OperationResult는 오퍼레이션별 결과
type OperationResult struct {
	Generated int
	Sampled   int
	Rate      float64
}

func simulateTraffic(sampler Sampler, operations []string, totalTraces int, rng *rand.Rand) TrafficResult {
	result := TrafficResult{
		SamplerName:  sampler.Name(),
		PerOperation: make(map[string]*OperationResult),
	}

	for _, op := range operations {
		result.PerOperation[op] = &OperationResult{}
	}

	for i := 0; i < totalTraces; i++ {
		traceID := rng.Uint64()
		op := operations[rng.Intn(len(operations))]

		result.TotalGenerated++
		result.PerOperation[op].Generated++

		decision := sampler.IsSampled(traceID, op)
		if decision.Sample {
			result.TotalSampled++
			result.PerOperation[op].Sampled++
		}
	}

	result.SamplingRate = float64(result.TotalSampled) / float64(result.TotalGenerated) * 100

	for _, opResult := range result.PerOperation {
		if opResult.Generated > 0 {
			opResult.Rate = float64(opResult.Sampled) / float64(opResult.Generated) * 100
		}
	}

	return result
}

// simulateTimedTraffic는 시간 기반 트래픽 시뮬레이션 (Rate-Limiting 테스트용)
func simulateTimedTraffic(sampler Sampler, operations []string, durationSec int, requestsPerSec int, rng *rand.Rand) TrafficResult {
	result := TrafficResult{
		SamplerName:  sampler.Name(),
		PerOperation: make(map[string]*OperationResult),
	}

	for _, op := range operations {
		result.PerOperation[op] = &OperationResult{}
	}

	interval := time.Second / time.Duration(requestsPerSec)

	for sec := 0; sec < durationSec; sec++ {
		for req := 0; req < requestsPerSec; req++ {
			traceID := rng.Uint64()
			op := operations[rng.Intn(len(operations))]

			result.TotalGenerated++
			result.PerOperation[op].Generated++

			decision := sampler.IsSampled(traceID, op)
			if decision.Sample {
				result.TotalSampled++
				result.PerOperation[op].Sampled++
			}

			// Rate-limiting 샘플러의 토큰 보충을 위해 시간 경과 시뮬레이션
			time.Sleep(interval / time.Duration(durationSec*2)) // 축약된 시간
		}
	}

	result.SamplingRate = float64(result.TotalSampled) / float64(result.TotalGenerated) * 100

	for _, opResult := range result.PerOperation {
		if opResult.Generated > 0 {
			opResult.Rate = float64(opResult.Sampled) / float64(opResult.Generated) * 100
		}
	}

	return result
}

func printResult(result TrafficResult, operations []string) {
	fmt.Printf("  %-35s\n", result.SamplerName)
	fmt.Printf("    생성: %6d  |  샘플링: %6d  |  샘플링률: %6.2f%%\n",
		result.TotalGenerated, result.TotalSampled, result.SamplingRate)
	fmt.Println("    오퍼레이션별:")
	for _, op := range operations {
		opResult := result.PerOperation[op]
		bar := strings.Repeat("█", int(opResult.Rate/2))
		fmt.Printf("      %-25s %5d/%5d (%5.1f%%) %s\n",
			op, opResult.Sampled, opResult.Generated, opResult.Rate, bar)
	}
}

// ============================================================
// 메인
// ============================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  Jaeger PoC: 샘플링 전략(Sampling Strategy) 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	rng := rand.New(rand.NewSource(42))
	operations := []string{"GET /users", "POST /orders", "GET /products", "DELETE /users", "PUT /orders"}
	totalTraces := 100000

	// ----------------------------------------------------------
	// 1. 세 가지 기본 샘플링 전략 비교
	// ----------------------------------------------------------
	fmt.Println("\n[1단계] 세 가지 기본 샘플링 전략 비교")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  Jaeger는 세 가지 기본 샘플링 전략을 제공한다:

  ┌─────────────────────────────────────────────────────┐
  │  전략 (strategy.Type)    │  매개변수 (strategy.Param)│
  ├─────────────────────────────────────────────────────┤
  │  "const" (상수)          │  0 = off, 1 = on         │
  │  "probabilistic" (확률)  │  0.0 ~ 1.0 (확률)        │
  │  "ratelimiting" (제한)   │  초당 최대 트레이스 수     │
  └─────────────────────────────────────────────────────┘

  소스: internal/sampling/samplingstrategy/file/strategy.go`)

	fmt.Printf("\n  시뮬레이션: %d개 트레이스, %d개 오퍼레이션\n\n", totalTraces, len(operations))

	// 1-1. Always-on
	fmt.Println("  ─── 1. Always-On (const=1) ───")
	alwaysOn := &AlwaysOnSampler{}
	r1 := simulateTraffic(alwaysOn, operations, totalTraces, rng)
	printResult(r1, operations)

	// 1-2. Probabilistic (10%)
	fmt.Println("\n  ─── 2. Probabilistic (10%) ───")
	prob10 := NewProbabilisticSampler(0.10)
	r2 := simulateTraffic(prob10, operations, totalTraces, rng)
	printResult(r2, operations)

	// 1-3. Probabilistic (1%)
	fmt.Println("\n  ─── 3. Probabilistic (1%) ───")
	prob1 := NewProbabilisticSampler(0.01)
	r3 := simulateTraffic(prob1, operations, totalTraces, rng)
	printResult(r3, operations)

	// ----------------------------------------------------------
	// 2. Rate-Limiting 샘플러 (토큰 버킷)
	// ----------------------------------------------------------
	fmt.Println("\n[2단계] Rate-Limiting 샘플러 (토큰 버킷 알고리즘)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  토큰 버킷(Token Bucket) 알고리즘:

  ┌─────────────────────────────────┐
  │          토큰 버킷               │
  │                                  │
  │   [토큰]  ← 초당 N개 보충        │
  │   [토큰]                         │
  │   [토큰]  ← 최대 N개 저장 가능    │
  │   [    ]                         │
  │   [    ]                         │
  │                                  │
  │   토큰 있음 → 1개 소비 → 샘플링   │
  │   토큰 없음 → 드롭               │
  └─────────────────────────────────┘

  장점: 버스트 트래픽에 유연하게 대응
  단점: 높은 QPS에서 샘플링률이 극히 낮아질 수 있음`)

	// Rate-Limiting: 초당 5개
	fmt.Println("\n  ─── Rate-Limiting (5/sec), 실시간 토큰 소비 시뮬레이션 ───")
	rateLimiter := NewRateLimitingSampler(5.0)
	fmt.Printf("    초기 토큰: %.0f\n\n", rateLimiter.balance)

	// 빠른 연속 요청 10개 테스트
	fmt.Println("    연속 10회 즉시 요청 (버스트 테스트):")
	sampled := 0
	for i := 0; i < 10; i++ {
		decision := rateLimiter.IsSampled(uint64(i), "test")
		marker := "X"
		if decision.Sample {
			marker = "O"
			sampled++
		}
		fmt.Printf("      요청 %2d: [%s] 잔여 토큰: %.2f\n", i+1, marker, rateLimiter.balance)
	}
	fmt.Printf("    결과: 10회 중 %d회 샘플링 (토큰 소진 후 드롭됨)\n", sampled)

	// 토큰 보충 대기 후 재시도
	fmt.Println("\n    1초 대기 후 토큰 보충...")
	time.Sleep(1 * time.Second)
	sampled2 := 0
	for i := 0; i < 5; i++ {
		decision := rateLimiter.IsSampled(uint64(100+i), "test")
		marker := "X"
		if decision.Sample {
			marker = "O"
			sampled2++
		}
		fmt.Printf("      요청 %2d: [%s] 잔여 토큰: %.2f\n", i+1, marker, rateLimiter.balance)
	}
	fmt.Printf("    결과: 5회 중 %d회 샘플링 (토큰 보충됨)\n", sampled2)

	// ----------------------------------------------------------
	// 3. 확률적 샘플러의 결정론적 특성
	// ----------------------------------------------------------
	fmt.Println("\n[3단계] 확률적 샘플러의 결정론적(Deterministic) 특성")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  확률적 샘플러는 traceID를 경계값과 비교하여 결정한다:

    samplingBoundary = MaxUint64 × samplingRate
    sampled = (traceID <= samplingBoundary)

  이 방식의 장점:
  1. 동일한 traceID는 항상 같은 결과 → 분산 환경에서 일관성
  2. 모든 서비스가 같은 결과를 독립적으로 계산 가능
  3. 난수 생성기(RNG) 불필요 → 재현 가능`)

	prob50 := NewProbabilisticSampler(0.5)
	fmt.Printf("\n  Probabilistic(50%%) 경계값: %d (MaxUint64의 50%%)\n", prob50.samplingBoundary)
	fmt.Println("\n  동일 traceID로 5회 반복 테스트:")
	testTraceID := uint64(12345678)
	for i := 0; i < 5; i++ {
		decision := prob50.IsSampled(testTraceID, "test")
		fmt.Printf("    시도 %d: traceID=%d → sampled=%v (항상 동일)\n", i+1, testTraceID, decision.Sample)
	}

	// 경계값 근처 시각화
	fmt.Println("\n  traceID 분포 vs 경계값 (10% 샘플링):")
	prob10_2 := NewProbabilisticSampler(0.10)
	boundary := prob10_2.samplingBoundary
	fmt.Printf("    경계값: %d\n", boundary)
	fmt.Printf("    MaxUint64: %d\n", uint64(math.MaxUint64))
	fmt.Println("    ├─ 0 ─────────────────────── boundary ─────────── MaxUint64 ──┤")
	fmt.Println("    │  ████████                                                    │")
	fmt.Println("    │  샘플링됨(10%)                 드롭됨(90%)                   │")
	fmt.Println("    └──────────────────────────────────────────────────────────────┘")

	// ----------------------------------------------------------
	// 4. Per-Operation 샘플링 전략
	// ----------------------------------------------------------
	fmt.Println("\n[4단계] Per-Operation 샘플링 전략")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  Jaeger는 서비스별, 오퍼레이션별로 다른 샘플링 전략을 적용할 수 있다.

  소스: file/strategy.go
    type serviceStrategy struct {
        Service             string
        OperationStrategies []*operationStrategy  ← 오퍼레이션별 전략
        strategy                                   ← 기본 전략
    }

  예시 설정:
    service: "api-gateway"
    default: probabilistic(0.01)  ← 기본 1%
    operations:
      - "GET /health":   probabilistic(0.001)  ← 헬스체크는 0.1%
      - "POST /orders":  probabilistic(0.50)   ← 주문은 50%
      - "DELETE /users": probabilistic(1.00)   ← 삭제는 100%`)

	perOp := NewPerOperationSampler(NewProbabilisticSampler(0.01)) // 기본 1%
	perOp.SetOperationSampler("GET /health", NewProbabilisticSampler(0.001))
	perOp.SetOperationSampler("POST /orders", NewProbabilisticSampler(0.50))
	perOp.SetOperationSampler("DELETE /users", NewProbabilisticSampler(1.0))
	perOp.SetOperationSampler("GET /users", NewProbabilisticSampler(0.05))

	perOpOps := []string{"GET /health", "GET /users", "POST /orders", "DELETE /users", "PUT /orders"}
	r4 := simulateTraffic(perOp, perOpOps, totalTraces, rng)

	fmt.Printf("\n  시뮬레이션: %d개 트레이스\n\n", totalTraces)
	printResult(r4, perOpOps)

	// ----------------------------------------------------------
	// 5. 전략 비교 종합
	// ----------------------------------------------------------
	fmt.Println("\n[5단계] 샘플링 전략 종합 비교")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println(`
  ┌────────────────────┬──────────┬──────────┬─────────────────────────────┐
  │  전략              │ 샘플링률  │ 예측가능 │ 적합한 상황                  │
  ├────────────────────┼──────────┼──────────┼─────────────────────────────┤
  │ Always-On          │ 100%     │  O       │ 개발/스테이징, 낮은 QPS      │
  │ Probabilistic      │ 고정%    │  O       │ 균일한 트래픽, 간단한 설정    │
  │ Rate-Limiting      │ 변동     │  X       │ 비용 제한, 버스트 트래픽      │
  │ Per-Operation      │ 오퍼별   │  O       │ 중요 오퍼레이션 우선 샘플링   │
  │ Adaptive (PoC 8)   │ 자동 조정│  X       │ 대규모 프로덕션 환경          │
  └────────────────────┴──────────┴──────────┴─────────────────────────────┘`)

	// 같은 트래픽으로 모든 전략 비교
	fmt.Println("\n  동일 트래픽(100,000개)으로 전략별 비교:")
	fmt.Println()

	allSamplers := []Sampler{
		&AlwaysOnSampler{},
		NewProbabilisticSampler(0.10),
		NewProbabilisticSampler(0.01),
		NewProbabilisticSampler(0.001),
	}

	fmt.Printf("  %-40s %8s %8s %8s\n", "전략", "생성", "샘플링", "비율")
	fmt.Println("  " + strings.Repeat("-", 70))

	for _, s := range allSamplers {
		r := simulateTraffic(s, operations, totalTraces, rng)
		fmt.Printf("  %-40s %8d %8d %7.2f%%\n", s.Name(), r.TotalGenerated, r.TotalSampled, r.SamplingRate)
	}

	// ----------------------------------------------------------
	// 6. 비용 절감 시각화
	// ----------------------------------------------------------
	fmt.Println("\n[6단계] 샘플링에 따른 비용 절감 효과")
	fmt.Println(strings.Repeat("-", 60))

	dailyTraces := 10000000 // 일 1000만 트레이스
	costPerTrace := 0.000005 // 트레이스당 저장 비용 ($)

	fmt.Printf("\n  일일 트래픽: %d 트레이스\n", dailyTraces)
	fmt.Printf("  트레이스당 저장 비용: $%.6f\n\n", costPerTrace)

	rates := []struct {
		name string
		rate float64
	}{
		{"100% (Always-on)", 1.0},
		{"10% (Probabilistic)", 0.10},
		{"1% (Probabilistic)", 0.01},
		{"0.1% (Probabilistic)", 0.001},
	}

	fmt.Printf("  %-25s %12s %12s %10s\n", "전략", "저장 트레이스", "일일 비용", "월간 비용")
	fmt.Println("  " + strings.Repeat("-", 65))

	for _, r := range rates {
		stored := int(float64(dailyTraces) * r.rate)
		dailyCost := float64(stored) * costPerTrace
		monthlyCost := dailyCost * 30
		fmt.Printf("  %-25s %12d $%10.2f $%9.2f\n", r.name, stored, dailyCost, monthlyCost)
	}

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("  시뮬레이션 완료")
	fmt.Println(strings.Repeat("=", 80))
}
