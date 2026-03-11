package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 1. 알림 규칙 모델
// ---------------------------------------------------------------------------

// EvalState — 평가 결과 상태
type EvalState string

const (
	EvalNormal   EvalState = "Normal"
	EvalAlerting EvalState = "Alerting"
	EvalPending  EvalState = "Pending"
	EvalNoData   EvalState = "NoData"
	EvalError    EvalState = "Error"
)

// NoDataState — 데이터 없을 때 동작
type NoDataState string

const (
	NoDataAlerting NoDataState = "Alerting"
	NoDataOK       NoDataState = "OK"
	NoDataNoData   NoDataState = "NoData"
)

// AlertRule — 알림 규칙
type AlertRule struct {
	UID             string
	Title           string
	Condition       string  // 조건 표현식 (예: "> 80")
	ThresholdValue  float64 // 임계값
	IntervalSeconds int     // 평가 간격 (초)
	For             time.Duration // Pending → Alerting 전환 대기 시간
	NoDataState     NoDataState
	Labels          map[string]string
}

// EvalResult — 평가 결과
type EvalResult struct {
	RuleUID    string
	State      EvalState
	Value      float64
	EvaluatedAt time.Time
	Error      error
	Attempt    int
}

func (r EvalResult) String() string {
	if r.Error != nil {
		return fmt.Sprintf("[%s] %s state=%-9s value=%.2f error=%v (attempt %d)",
			r.EvaluatedAt.Format("15:04:05"), r.RuleUID, r.State, r.Value, r.Error, r.Attempt)
	}
	return fmt.Sprintf("[%s] %s state=%-9s value=%.2f (attempt %d)",
		r.EvaluatedAt.Format("15:04:05"), r.RuleUID, r.State, r.Value, r.Attempt)
}

// ---------------------------------------------------------------------------
// 2. 메트릭 시뮬레이터
// ---------------------------------------------------------------------------

// MetricSimulator — 메트릭 값을 시뮬레이션
type MetricSimulator struct {
	mu       sync.Mutex
	metrics  map[string]float64
	noData   map[string]bool
}

func NewMetricSimulator() *MetricSimulator {
	return &MetricSimulator{
		metrics: map[string]float64{
			"cpu_usage":    45.0,
			"memory_usage": 60.0,
			"error_rate":   0.5,
		},
		noData: make(map[string]bool),
	}
}

// GetMetric — 메트릭 값 조회 (랜덤 변동 포함)
func (m *MetricSimulator) GetMetric(name string) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.noData[name] {
		return 0, false
	}

	base, ok := m.metrics[name]
	if !ok {
		return 0, false
	}

	// 랜덤 변동 추가 (±20%)
	variation := (rand.Float64() - 0.5) * base * 0.4
	current := base + variation

	// 트렌드 시뮬레이션: 점진적 증가
	m.metrics[name] = base + rand.Float64()*2 - 0.5

	return current, true
}

// SetNoData — 특정 메트릭을 NoData 상태로 설정
func (m *MetricSimulator) SetNoData(name string, noData bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noData[name] = noData
}

// ---------------------------------------------------------------------------
// 3. 알림 스케줄러
// ---------------------------------------------------------------------------

// Scheduler — Grafana 알림 스케줄러
type Scheduler struct {
	baseInterval time.Duration         // 기본 틱 간격
	rules        map[string]*AlertRule // UID → AlertRule
	results      []EvalResult          // 평가 결과 기록
	metrics      *MetricSimulator
	startTime    time.Time
	mu           sync.Mutex
	maxRetries   int
}

// NewScheduler — 스케줄러 생성
func NewScheduler(baseInterval time.Duration, metrics *MetricSimulator) *Scheduler {
	return &Scheduler{
		baseInterval: baseInterval,
		rules:        make(map[string]*AlertRule),
		results:      make([]EvalResult, 0),
		metrics:      metrics,
		maxRetries:   3,
	}
}

// AddRule — 알림 규칙 등록
func (s *Scheduler) AddRule(rule *AlertRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[rule.UID] = rule
	freq := s.itemFrequency(rule)
	fmt.Printf("  [규칙 등록] %s: 간격=%ds, frequency=%d (매 %d번째 틱마다 실행)\n",
		rule.Title, rule.IntervalSeconds, freq, freq)
}

// itemFrequency — 규칙의 실행 빈도 계산
// Grafana: rule.IntervalSeconds / baseInterval
func (s *Scheduler) itemFrequency(rule *AlertRule) int {
	freq := rule.IntervalSeconds / int(s.baseInterval.Seconds())
	if freq < 1 {
		freq = 1
	}
	return freq
}

// jitter — 규칙별 Jitter 계산 (해시 기반)
func (s *Scheduler) jitter(ruleUID string) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(ruleUID))
	jitterMs := h.Sum32() % uint32(s.baseInterval.Milliseconds())
	return time.Duration(jitterMs) * time.Millisecond
}

// shouldEvaluate — 현재 틱에서 규칙을 평가해야 하는지 판단
func (s *Scheduler) shouldEvaluate(tickNum int, rule *AlertRule) bool {
	freq := s.itemFrequency(rule)
	return tickNum%freq == 0
}

// evaluateRule — 규칙 평가 (재시도 포함)
func (s *Scheduler) evaluateRule(rule *AlertRule, tickTime time.Time) EvalResult {
	var lastResult EvalResult

	for attempt := 1; attempt <= s.maxRetries; attempt++ {
		result := s.doEvaluate(rule, tickTime, attempt)

		if result.State != EvalError {
			s.mu.Lock()
			s.results = append(s.results, result)
			s.mu.Unlock()
			return result
		}

		lastResult = result

		if attempt < s.maxRetries {
			// 지수 백오프: 100ms, 200ms, 400ms...
			backoff := time.Duration(1<<uint(attempt-1)) * 100 * time.Millisecond
			fmt.Printf("      [재시도] %s: attempt %d/%d, 대기 %v\n",
				rule.Title, attempt, s.maxRetries, backoff)
			time.Sleep(backoff)
		}
	}

	// 최대 재시도 초과
	s.mu.Lock()
	s.results = append(s.results, lastResult)
	s.mu.Unlock()
	return lastResult
}

// doEvaluate — 단일 평가 실행
func (s *Scheduler) doEvaluate(rule *AlertRule, tickTime time.Time, attempt int) EvalResult {
	// 메트릭 이름 추출 (간단히 라벨에서 가져옴)
	metricName := rule.Labels["metric"]

	value, hasData := s.metrics.GetMetric(metricName)

	// NoData 처리
	if !hasData {
		state := EvalNoData
		if rule.NoDataState == NoDataAlerting {
			state = EvalAlerting
		} else if rule.NoDataState == NoDataOK {
			state = EvalNormal
		}
		return EvalResult{
			RuleUID:     rule.UID,
			State:       state,
			Value:       0,
			EvaluatedAt: tickTime,
			Attempt:     attempt,
		}
	}

	// 에러 시뮬레이션 (5% 확률)
	if rand.Float64() < 0.05 {
		return EvalResult{
			RuleUID:     rule.UID,
			State:       EvalError,
			Value:       value,
			EvaluatedAt: tickTime,
			Error:       fmt.Errorf("datasource timeout"),
			Attempt:     attempt,
		}
	}

	// 조건 평가
	state := EvalNormal
	if value > rule.ThresholdValue {
		state = EvalAlerting
	}

	return EvalResult{
		RuleUID:     rule.UID,
		State:       state,
		Value:       value,
		EvaluatedAt: tickTime,
		Attempt:     attempt,
	}
}

// Run — 스케줄러 실행
func (s *Scheduler) Run(ctx context.Context) {
	s.startTime = time.Now()
	ticker := time.NewTicker(s.baseInterval)
	defer ticker.Stop()

	tickNum := 0
	fmt.Printf("\n  [스케줄러] 시작 (baseInterval=%v)\n", s.baseInterval)

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n  [스케줄러] 종료됨")
			return
		case t := <-ticker.C:
			tickNum++
			s.processTick(tickNum, t)
		}
	}
}

// processTick — 틱 처리: 실행 대상 규칙 선택 및 실행
func (s *Scheduler) processTick(tickNum int, tickTime time.Time) {
	s.mu.Lock()
	rules := make([]*AlertRule, 0)
	for _, r := range s.rules {
		if s.shouldEvaluate(tickNum, r) {
			rules = append(rules, r)
		}
	}
	s.mu.Unlock()

	if len(rules) == 0 {
		return
	}

	fmt.Printf("\n  [Tick #%d] %s — %d개 규칙 평가\n",
		tickNum, tickTime.Format("15:04:05"), len(rules))

	// Staggered execution: 각 규칙에 jitter를 적용하여 분산 실행
	wg := sync.WaitGroup{}
	for _, rule := range rules {
		wg.Add(1)
		j := s.jitter(rule.UID)

		go func(r *AlertRule, jitter time.Duration) {
			defer wg.Done()
			// Jitter 적용 (부하 분산)
			time.Sleep(jitter)

			result := s.evaluateRule(r, tickTime)
			stateIndicator := "  "
			switch result.State {
			case EvalAlerting:
				stateIndicator = ">>"
			case EvalPending:
				stateIndicator = ".."
			case EvalNoData:
				stateIndicator = "??"
			case EvalError:
				stateIndicator = "!!"
			}
			fmt.Printf("    %s %s\n", stateIndicator, result)
		}(rule, j)
	}
	wg.Wait()
}

// Summary — 평가 결과 요약
func (s *Scheduler) Summary() {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Println("\n=== 평가 결과 요약 ===")

	// 규칙별 통계
	stats := make(map[string]map[EvalState]int)
	for _, r := range s.results {
		if _, ok := stats[r.RuleUID]; !ok {
			stats[r.RuleUID] = make(map[EvalState]int)
		}
		stats[r.RuleUID][r.State]++
	}

	fmt.Printf("\n  %-15s %-8s %-8s %-8s %-8s %-8s %-8s\n",
		"Rule", "Total", "Normal", "Alert", "Pending", "NoData", "Error")
	fmt.Println("  " + strings.Repeat("-", 65))

	for uid, counts := range stats {
		total := 0
		for _, c := range counts {
			total += c
		}
		rule := s.rules[uid]
		name := uid
		if rule != nil {
			name = rule.Title
		}
		fmt.Printf("  %-15s %-8d %-8d %-8d %-8d %-8d %-8d\n",
			name, total,
			counts[EvalNormal], counts[EvalAlerting],
			counts[EvalPending], counts[EvalNoData], counts[EvalError])
	}
}

// ---------------------------------------------------------------------------
// 4. 메인
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("==================================================")
	fmt.Println("  Grafana Alert Scheduler Simulation")
	fmt.Println("==================================================")

	// 시드 초기화
	rand.Seed(time.Now().UnixNano())

	// 메트릭 시뮬레이터
	metrics := NewMetricSimulator()

	// 스케줄러 생성 (baseInterval = 2초로 시뮬레이션, 실제 Grafana는 10초)
	baseInterval := 2 * time.Second
	scheduler := NewScheduler(baseInterval, metrics)

	// 알림 규칙 등록
	fmt.Println("\n--- 알림 규칙 등록 ---")

	scheduler.AddRule(&AlertRule{
		UID:             "rule-cpu",
		Title:           "HighCPU",
		Condition:       "> 80",
		ThresholdValue:  80.0,
		IntervalSeconds: 2, // 매 틱마다 (2초)
		For:             10 * time.Second,
		NoDataState:     NoDataAlerting,
		Labels:          map[string]string{"metric": "cpu_usage", "severity": "critical"},
	})

	scheduler.AddRule(&AlertRule{
		UID:             "rule-mem",
		Title:           "HighMemory",
		Condition:       "> 70",
		ThresholdValue:  70.0,
		IntervalSeconds: 4, // 매 2번째 틱마다 (4초)
		For:             20 * time.Second,
		NoDataState:     NoDataOK,
		Labels:          map[string]string{"metric": "memory_usage", "severity": "warning"},
	})

	scheduler.AddRule(&AlertRule{
		UID:             "rule-err",
		Title:           "HighErrors",
		Condition:       "> 5",
		ThresholdValue:  5.0,
		IntervalSeconds: 6, // 매 3번째 틱마다 (6초)
		For:             30 * time.Second,
		NoDataState:     NoDataNoData,
		Labels:          map[string]string{"metric": "error_rate", "severity": "critical"},
	})

	// 스케줄링 공식 설명
	fmt.Println("\n--- 스케줄링 공식 ---")
	fmt.Printf("  baseInterval = %v\n", baseInterval)
	fmt.Println("  itemFrequency = rule.IntervalSeconds / baseInterval")
	fmt.Println("  shouldEvaluate = tickNum %% itemFrequency == 0")
	fmt.Println("  jitter = hash(ruleUID) %% baseInterval")

	fmt.Println("\n  규칙별 스케줄:")
	fmt.Println("  Tick#:  1  2  3  4  5  6  7  8  9  10 11 12 13 14 15")
	fmt.Println("  CPU:    x  x  x  x  x  x  x  x  x  x  x  x  x  x  x  (매 틱)")
	fmt.Println("  Memory: .  x  .  x  .  x  .  x  .  x  .  x  .  x  .  (2틱마다)")
	fmt.Println("  Error:  .  .  x  .  .  x  .  .  x  .  .  x  .  .  x  (3틱마다)")

	// 5초 후 error_rate 메트릭을 NoData로 전환
	go func() {
		time.Sleep(5 * time.Second)
		fmt.Println("\n  *** [이벤트] error_rate 메트릭 NoData 전환 ***")
		metrics.SetNoData("error_rate", true)
	}()

	// 스케줄러 실행 (8초)
	fmt.Println("\n--- 스케줄러 실행 (8초) ---")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	scheduler.Run(ctx)

	// 결과 요약
	scheduler.Summary()

	// 아키텍처 설명
	fmt.Println("\n--- Grafana Alert Scheduler 아키텍처 ---")
	fmt.Print(`
  ┌──────────────────────────────────────────────────────┐
  │                  Alert Scheduler                      │
  │                                                       │
  │  ┌──────────┐     baseInterval = 10s                 │
  │  │  Ticker   │────┐                                   │
  │  └──────────┘    │  tickNum++                         │
  │                   ▼                                    │
  │  ┌──────────────────────────────┐                     │
  │  │ for each rule:               │                     │
  │  │   freq = interval / base     │                     │
  │  │   if tickNum % freq == 0:    │                     │
  │  │     schedule(rule, jitter)   │                     │
  │  └──────────┬───────────────────┘                     │
  │             │                                          │
  │     time.AfterFunc(jitter)                             │
  │             │                                          │
  │             ▼                                          │
  │  ┌──────────────────────────────┐                     │
  │  │ evaluateRule()               │                     │
  │  │   1. 데이터소스 쿼리          │                     │
  │  │   2. 조건 평가                │                     │
  │  │   3. 상태 전이 결정           │                     │
  │  │   4. 실패 시 재시도 (3회)     │                     │
  │  └──────────┬───────────────────┘                     │
  │             │                                          │
  │             ▼                                          │
  │  ┌──────────────────────────────┐                     │
  │  │ State Manager                │                     │
  │  │   Normal/Pending/Alerting    │                     │
  │  └──────────┬───────────────────┘                     │
  │             │                                          │
  │             ▼                                          │
  │  ┌──────────────────────────────┐                     │
  │  │ Alertmanager로 전송           │                     │
  │  └──────────────────────────────┘                     │
  └──────────────────────────────────────────────────────┘
`)
}
