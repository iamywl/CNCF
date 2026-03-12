package main

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Prometheus Alerting Rule 상태 머신 시뮬레이션
// 원본: prometheus/rules/alerting.go
//
// 핵심 개념:
// 1. AlertState: Inactive → Pending → Firing 상태 전이
// 2. holdDuration (for): Pending에서 Firing으로 전환하기까지 대기 시간
// 3. keepFiringFor: 조건이 해소된 후에도 Firing 상태를 유지하는 시간
// 4. needsSending: resendDelay 기반 알림 전송 판단
// 5. resolvedRetention: Resolved 알림을 메모리에 유지하는 시간 (15분)
// =============================================================================

// ---------------------------------------------------------------------------
// AlertState - 원본: rules/alerting.go:54-67
// ---------------------------------------------------------------------------

type AlertState int

const (
	StateInactive AlertState = iota
	StatePending
	StateFiring
)

func (s AlertState) String() string {
	switch s {
	case StateInactive:
		return "inactive"
	case StatePending:
		return "pending"
	case StateFiring:
		return "firing"
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// Labels & Fingerprint - 라벨 기반 알림 식별
// 원본에서는 labels.Labels.Hash()로 uint64 fingerprint 생성
// ---------------------------------------------------------------------------

type Labels map[string]string

func (l Labels) String() string {
	if len(l) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, l[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Fingerprint 생성 - 원본: labels.Labels.Hash()
// 동일 라벨셋은 항상 같은 fingerprint를 반환해야 함
func (l Labels) Fingerprint() uint64 {
	h := fnv.New64a()
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0xFF})
		h.Write([]byte(l[k]))
		h.Write([]byte{0xFF})
	}
	return h.Sum64()
}

// ---------------------------------------------------------------------------
// Alert - 개별 알림 인스턴스
// 원본: rules/alerting.go:84-100
// ---------------------------------------------------------------------------

type Alert struct {
	State       AlertState
	Labels      Labels
	Annotations Labels
	Value       float64

	ActiveAt        time.Time // 조건이 처음 참이 된 시점
	FiredAt         time.Time // Firing 상태로 전환된 시점
	ResolvedAt      time.Time // 조건이 해소된 시점 (zero = 아직 활성)
	LastSentAt      time.Time // 마지막으로 Alertmanager에 전송된 시점
	ValidUntil      time.Time // 알림 유효 기간
	KeepFiringSince time.Time // keepFiringFor 카운트 시작 시점
}

// needsSending - 원본: rules/alerting.go:102-113
// Pending 상태는 전송하지 않음
// Resolved 후 아직 전송 안 했으면 전송
// resendDelay 경과 후 재전송
func (a *Alert) needsSending(ts time.Time, resendDelay time.Duration) bool {
	if a.State == StatePending {
		return false
	}
	// Resolved 이후 아직 전송 안 했으면 즉시 전송
	if a.ResolvedAt.After(a.LastSentAt) {
		return true
	}
	return a.LastSentAt.Add(resendDelay).Before(ts)
}

// ---------------------------------------------------------------------------
// Sample - 쿼리 결과 (원본: promql.Sample 단순화)
// ---------------------------------------------------------------------------

type Sample struct {
	Labels Labels
	Value  float64
}

// ---------------------------------------------------------------------------
// AlertingRule - 알림 규칙
// 원본: rules/alerting.go:116-157
// ---------------------------------------------------------------------------

const resolvedRetention = 15 * time.Minute // 원본: rules/alerting.go:378

type AlertingRule struct {
	name          string
	expr          string        // 실제로는 parser.Expr; 여기서는 설명용 문자열
	holdDuration  time.Duration // `for` 필드: Pending → Firing 대기 시간
	keepFiringFor time.Duration // 조건 해소 후에도 Firing 유지 시간
	labels        Labels        // 규칙에 추가되는 라벨
	annotations   Labels        // 알림 어노테이션

	active map[uint64]*Alert // fingerprint → Alert (Pending 또는 Firing)
}

func NewAlertingRule(name, expr string, holdDuration, keepFiringFor time.Duration, labels, annotations Labels) *AlertingRule {
	return &AlertingRule{
		name:          name,
		expr:          expr,
		holdDuration:  holdDuration,
		keepFiringFor: keepFiringFor,
		labels:        labels,
		annotations:   annotations,
		active:        make(map[uint64]*Alert),
	}
}

// Eval - 핵심 평가 로직
// 원본: rules/alerting.go:382-546
//
// 알고리즘:
// 1. 쿼리 결과의 각 Sample을 라벨 fingerprint로 매핑
// 2. 새로운 결과 → Pending 알림 생성 (ActiveAt = 현재 시각)
// 3. 기존 알림이 있으면 Value만 업데이트
// 4. 결과에 없는 기존 알림:
//    - keepFiringFor 설정이 있고 Firing 상태면 유지
//    - 아니면 Inactive로 전환 (ResolvedAt 설정)
//    - Pending 상태이거나 resolvedRetention 초과 → 맵에서 삭제
// 5. Pending + holdDuration 경과 → Firing 전환 (FiredAt 설정)
func (r *AlertingRule) Eval(ts time.Time, queryResults []Sample) {
	// 1단계: 쿼리 결과를 fingerprint로 매핑
	resultFPs := make(map[uint64]struct{})
	newAlerts := make(map[uint64]*Alert)

	for _, smpl := range queryResults {
		// 원본: 라벨에서 __name__ 제거 후 규칙 라벨 추가, alertname 설정
		alertLabels := make(Labels)
		for k, v := range smpl.Labels {
			if k == "__name__" {
				continue
			}
			alertLabels[k] = v
		}
		for k, v := range r.labels {
			alertLabels[k] = v
		}
		alertLabels["alertname"] = r.name

		h := alertLabels.Fingerprint()
		resultFPs[h] = struct{}{}

		newAlerts[h] = &Alert{
			Labels:      alertLabels,
			Annotations: r.annotations,
			ActiveAt:    ts,
			State:       StatePending,
			Value:       smpl.Value,
		}
	}

	// 2단계: 기존 활성 알림과 병합
	// 원본: alerting.go:463-473
	for h, a := range newAlerts {
		if existing, ok := r.active[h]; ok && existing.State != StateInactive {
			// 이미 활성 알림이 있으면 Value와 Annotations만 업데이트
			existing.Value = a.Value
			existing.Annotations = a.Annotations
			continue
		}
		// 새로운 알림 또는 Inactive였던 것 → 새로 생성
		r.active[h] = a
	}

	// 3단계: 결과에 없는 기존 알림 처리 + 상태 전이
	// 원본: alerting.go:477-538
	for fp, a := range r.active {
		if _, ok := resultFPs[fp]; !ok {
			// 쿼리 결과에 없음 → 조건 해소

			// keepFiringFor 처리 (원본: alerting.go:484-493)
			var keepFiring bool
			if a.State == StateFiring && r.keepFiringFor > 0 {
				if a.KeepFiringSince.IsZero() {
					a.KeepFiringSince = ts
				}
				if ts.Sub(a.KeepFiringSince) < r.keepFiringFor {
					keepFiring = true
				}
			}

			// Pending이면 즉시 삭제, Resolved 후 retention 초과해도 삭제
			// 원본: alerting.go:505-507
			if a.State == StatePending || (!a.ResolvedAt.IsZero() && ts.Sub(a.ResolvedAt) > resolvedRetention) {
				delete(r.active, fp)
			}

			// Inactive 전환 (원본: alerting.go:508-511)
			if a.State != StateInactive && !keepFiring {
				a.State = StateInactive
				a.ResolvedAt = ts
			}
			if !keepFiring {
				continue
			}
		} else {
			// 조건이 다시 참 → keepFiringSince 리셋 (원본: alerting.go:516)
			a.KeepFiringSince = time.Time{}
		}

		// Pending → Firing 전이 (원본: alerting.go:521-524)
		if a.State == StatePending && ts.Sub(a.ActiveAt) >= r.holdDuration {
			a.State = StateFiring
			a.FiredAt = ts
		}
	}
}

// sendAlerts - 전송이 필요한 알림을 수집
// 원본: rules/alerting.go:613-628
func (r *AlertingRule) sendAlerts(ts time.Time, resendDelay, evalInterval time.Duration) []*Alert {
	var alerts []*Alert
	for _, a := range r.active {
		if a.needsSending(ts, resendDelay) {
			a.LastSentAt = ts
			delta := resendDelay
			if evalInterval > delta {
				delta = evalInterval
			}
			a.ValidUntil = ts.Add(4 * delta)
			copied := *a
			alerts = append(alerts, &copied)
		}
	}
	return alerts
}

// State - 규칙의 최대 상태 반환 (원본: alerting.go:550-565)
func (r *AlertingRule) State() AlertState {
	maxState := StateInactive
	for _, a := range r.active {
		if a.State > maxState {
			maxState = a.State
		}
	}
	return maxState
}

// ActiveAlerts - 활성 알림 목록 (ResolvedAt이 zero인 것)
func (r *AlertingRule) ActiveAlerts() []*Alert {
	var res []*Alert
	for _, a := range r.active {
		if a.ResolvedAt.IsZero() {
			res = append(res, a)
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// 시뮬레이션 유틸리티
// ---------------------------------------------------------------------------

func printHeader(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 80))
}

func printEval(cycle int, ts time.Time, rule *AlertingRule, results []Sample) {
	fmt.Printf("\n--- Eval #%02d | ts=%s ---\n", cycle, ts.Format("15:04:05"))
	if len(results) > 0 {
		fmt.Printf("  쿼리 결과: %d개 시리즈 매칭\n", len(results))
		for _, s := range results {
			fmt.Printf("    %s value=%.1f\n", s.Labels, s.Value)
		}
	} else {
		fmt.Println("  쿼리 결과: 없음 (조건 미충족)")
	}

	// 규칙 상태 출력
	fmt.Printf("  규칙 상태: %s\n", rule.State())
	for _, a := range rule.active {
		extra := ""
		if !a.KeepFiringSince.IsZero() {
			extra = fmt.Sprintf(" (keepFiringSince=%s)", a.KeepFiringSince.Format("15:04:05"))
		}
		if !a.ResolvedAt.IsZero() {
			extra += fmt.Sprintf(" (resolvedAt=%s)", a.ResolvedAt.Format("15:04:05"))
		}
		fmt.Printf("    알림 %s → state=%s, value=%.1f, activeAt=%s%s\n",
			a.Labels, a.State, a.Value, a.ActiveAt.Format("15:04:05"), extra)
	}
}

// ---------------------------------------------------------------------------
// main - 데모 시나리오
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Prometheus Alerting Rule 상태 머신 시뮬레이션                              ║")
	fmt.Println("║  원본: prometheus/rules/alerting.go                                         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")

	// =========================================================================
	// 시나리오 1: 기본 상태 전이 (Inactive → Pending → Firing → Resolved)
	// =========================================================================
	printHeader("시나리오 1: 기본 상태 전이 (for: 30s)")

	fmt.Println(`
  상태 머신:
                                  ┌─────────────────────┐
                                  │                     │
                   조건 참         │  조건 참 지속        │  조건 해소
  ┌──────────┐  ──────────►  ┌────┴─────┐  ──────────►  ┌──────────┐
  │ Inactive │               │ Pending  │   for 경과    │ Firing   │
  └──────────┘  ◄──────────  └──────────┘               └──────────┘
                  조건 해소                                    │
                  (즉시 삭제)                     조건 해소     │
                                              ──────────►     │
                                  ┌──────────┐                │
                                  │ Inactive │ ◄──────────────┘
                                  │(Resolved)│   ResolvedAt 설정
                                  └──────────┘
`)

	evalInterval := 10 * time.Second
	rule1 := NewAlertingRule(
		"HighErrorRate",
		"rate(http_errors_total[5m]) > 0.05",
		30*time.Second, // for: 30s (3번 평가 후 Firing)
		0,              // keepFiringFor 없음
		Labels{"severity": "critical"},
		Labels{"summary": "높은 에러율 감지"},
	)

	baseTime := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	// 20 사이클 시뮬레이션
	for cycle := 1; cycle <= 20; cycle++ {
		ts := baseTime.Add(time.Duration(cycle-1) * evalInterval)
		var results []Sample

		// 사이클 1~3: 정상 (결과 없음)
		// 사이클 4~12: 조건 발생 (결과 있음)
		// 사이클 13~20: 조건 해소 (결과 없음)
		if cycle >= 4 && cycle <= 12 {
			results = []Sample{
				{Labels: Labels{"instance": "web-1", "job": "api"}, Value: 0.08},
			}
		}

		rule1.Eval(ts, results)
		printEval(cycle, ts, rule1, results)

		// 전송 체크
		sent := rule1.sendAlerts(ts, 1*time.Minute, evalInterval)
		if len(sent) > 0 {
			for _, a := range sent {
				fmt.Printf("  >> Alertmanager 전송: state=%s, labels=%s\n", a.State, a.Labels)
			}
		}
	}

	// =========================================================================
	// 시나리오 2: keepFiringFor - 조건 해소 후에도 Firing 유지
	// =========================================================================
	printHeader("시나리오 2: keepFiringFor (for: 10s, keepFiringFor: 30s)")

	fmt.Println(`
  keepFiringFor 동작:

  조건 참  ████████████░░░░░░░░░░░░░░░░░  조건 해소
  상태     Pending→Firing████████████████  keepFiringFor 동안 Firing 유지
                                     ▲
                                     │
                              keepFiringSince 설정
                                     │
                              30초 경과 후 Inactive 전환
`)

	rule2 := NewAlertingRule(
		"DiskAlmostFull",
		"disk_usage_percent > 90",
		10*time.Second, // for: 10s (1번 평가 후 Firing)
		30*time.Second, // keepFiringFor: 30s
		Labels{"severity": "warning"},
		Labels{"summary": "디스크 사용량 90% 초과"},
	)

	for cycle := 1; cycle <= 15; cycle++ {
		ts := baseTime.Add(time.Duration(cycle-1) * evalInterval)
		var results []Sample

		// 사이클 1~4: 조건 발생
		// 사이클 5~15: 조건 해소 (keepFiringFor 테스트)
		if cycle >= 1 && cycle <= 4 {
			results = []Sample{
				{Labels: Labels{"instance": "db-1", "mountpoint": "/data"}, Value: 95.2},
			}
		}

		rule2.Eval(ts, results)
		printEval(cycle, ts, rule2, results)
	}

	// =========================================================================
	// 시나리오 3: 다중 알림 인스턴스 (서로 다른 라벨셋)
	// =========================================================================
	printHeader("시나리오 3: 다중 알림 인스턴스")

	fmt.Println(`
  동일 규칙에서 서로 다른 라벨셋의 알림이 독립적으로 관리됨:

  규칙: rate(http_errors_total[5m]) > 0.05

  인스턴스별 타임라인:
  web-1:  ──Pending──Firing────────────Resolved──
  web-2:  ────────Pending──Firing──────Resolved──
  web-3:  ──────────────Pending──Firing─────────  (계속 유지)
`)

	rule3 := NewAlertingRule(
		"HighLatency",
		"http_request_duration_seconds > 1.0",
		20*time.Second, // for: 20s
		0,
		Labels{"severity": "warning"},
		Labels{"summary": "높은 지연시간"},
	)

	for cycle := 1; cycle <= 12; cycle++ {
		ts := baseTime.Add(time.Duration(cycle-1) * evalInterval)
		var results []Sample

		// web-1: 사이클 1~7 활성
		if cycle >= 1 && cycle <= 7 {
			results = append(results, Sample{
				Labels: Labels{"instance": "web-1", "job": "api"},
				Value:  1.5 + float64(cycle)*0.1,
			})
		}
		// web-2: 사이클 3~9 활성
		if cycle >= 3 && cycle <= 9 {
			results = append(results, Sample{
				Labels: Labels{"instance": "web-2", "job": "api"},
				Value:  2.0 + float64(cycle)*0.05,
			})
		}
		// web-3: 사이클 5~12 활성 (끝까지)
		if cycle >= 5 && cycle <= 12 {
			results = append(results, Sample{
				Labels: Labels{"instance": "web-3", "job": "api"},
				Value:  1.2,
			})
		}

		rule3.Eval(ts, results)
		printEval(cycle, ts, rule3, results)
	}

	// =========================================================================
	// 시나리오 4: needsSending & resendDelay 메커니즘
	// =========================================================================
	printHeader("시나리오 4: needsSending & resendDelay 메커니즘")

	fmt.Println(`
  전송 규칙 (원본: alerting.go:102-113):

  1. Pending 상태 → 전송하지 않음
  2. Firing 상태 + 처음 → 즉시 전송 (LastSentAt이 zero)
  3. Firing 상태 + resendDelay 경과 → 재전송
  4. Resolved 후 LastSentAt 이전에 ResolvedAt → 즉시 전송

  타임라인:
  t=0s   Pending     → 전송 안 함
  t=10s  Firing      → 전송 (최초)
  t=20s  Firing      → 전송 안 함 (resendDelay=60s 미경과)
  t=70s  Firing      → 재전송 (60s 경과)
  t=80s  Resolved    → 전송 (resolved 알림)
`)

	resendDelay := 60 * time.Second
	rule4 := NewAlertingRule(
		"CPUThrottling",
		"container_cpu_throttled_seconds_total > 100",
		10*time.Second,
		0,
		Labels{"severity": "info"},
		Labels{"summary": "CPU 쓰로틀링 감지"},
	)

	for cycle := 1; cycle <= 12; cycle++ {
		ts := baseTime.Add(time.Duration(cycle-1) * evalInterval)
		var results []Sample

		if cycle >= 1 && cycle <= 9 {
			results = []Sample{
				{Labels: Labels{"container": "app", "pod": "app-pod-1"}, Value: 150.0},
			}
		}

		rule4.Eval(ts, results)
		fmt.Printf("\n--- Eval #%02d | ts=%s ---\n", cycle, ts.Format("15:04:05"))
		fmt.Printf("  규칙 상태: %s\n", rule4.State())

		sent := rule4.sendAlerts(ts, resendDelay, evalInterval)
		if len(sent) > 0 {
			for _, a := range sent {
				fmt.Printf("  >> 전송: state=%s (lastSent=%s, validUntil=%s)\n",
					a.State, a.LastSentAt.Format("15:04:05"), a.ValidUntil.Format("15:04:05"))
			}
		} else {
			reason := ""
			for _, a := range rule4.active {
				if a.State == StatePending {
					reason = "Pending 상태는 전송 안 함"
				} else if a.State == StateFiring && !a.LastSentAt.IsZero() {
					remaining := a.LastSentAt.Add(resendDelay).Sub(ts)
					if remaining > 0 {
						reason = fmt.Sprintf("resendDelay 미경과 (남은 시간: %s)", remaining)
					}
				}
			}
			if reason != "" {
				fmt.Printf("  -- 전송 건너뜀: %s\n", reason)
			}
		}
	}

	// =========================================================================
	// 요약
	// =========================================================================
	printHeader("상태 전이 요약")

	fmt.Println(`
  ┌─────────────────────────────────────────────────────────────────────────┐
  │                    Prometheus Alert 상태 전이 규칙                      │
  ├─────────────────────────────────────────────────────────────────────────┤
  │                                                                        │
  │  [조건 참] ──► Pending 생성 (ActiveAt = 현재 시각)                     │
  │                                                                        │
  │  [Pending + for 경과] ──► Firing 전환 (FiredAt = 현재 시각)            │
  │                                                                        │
  │  [Pending + 조건 해소] ──► 즉시 삭제 (active 맵에서 제거)              │
  │                                                                        │
  │  [Firing + 조건 해소] ──► Inactive (ResolvedAt = 현재 시각)            │
  │    └─ keepFiringFor 설정 시: KeepFiringSince부터 duration만큼 유지     │
  │                                                                        │
  │  [Inactive + resolvedRetention(15m) 초과] ──► active 맵에서 삭제       │
  │                                                                        │
  │  전송 조건:                                                            │
  │    - Pending: 전송 안 함                                               │
  │    - Firing 최초: 즉시 전송                                            │
  │    - Firing 재전송: resendDelay 경과 후                                │
  │    - Resolved: ResolvedAt > LastSentAt이면 즉시 전송                   │
  │                                                                        │
  │  원본 소스:                                                            │
  │    AlertState      → rules/alerting.go:54-67                           │
  │    Alert struct    → rules/alerting.go:84-100                          │
  │    needsSending    → rules/alerting.go:102-113                         │
  │    AlertingRule    → rules/alerting.go:116-157                         │
  │    Eval()          → rules/alerting.go:382-546                         │
  │    sendAlerts()    → rules/alerting.go:613-628                         │
  │                                                                        │
  └─────────────────────────────────────────────────────────────────────────┘
`)
}
