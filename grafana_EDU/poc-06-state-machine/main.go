package main

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 1. 상태 정의
// ---------------------------------------------------------------------------

// AlertState — 알림 상태
type AlertState int

const (
	StateNormal AlertState = iota
	StatePending
	StateAlerting
	StateNoData
	StateError
)

func (s AlertState) String() string {
	switch s {
	case StateNormal:
		return "Normal"
	case StatePending:
		return "Pending"
	case StateAlerting:
		return "Alerting"
	case StateNoData:
		return "NoData"
	case StateError:
		return "Error"
	default:
		return "Unknown"
	}
}

// EvalResult — 평가 결과 입력
type EvalResult int

const (
	ResultNormal EvalResult = iota
	ResultAlerting
	ResultNoData
	ResultError
)

func (r EvalResult) String() string {
	switch r {
	case ResultNormal:
		return "Normal"
	case ResultAlerting:
		return "Alerting"
	case ResultNoData:
		return "NoData"
	case ResultError:
		return "Error"
	default:
		return "Unknown"
	}
}

// ---------------------------------------------------------------------------
// 2. 알림 상태 구조체
// ---------------------------------------------------------------------------

// AlertInstanceState — 단일 알림 인스턴스의 상태
type AlertInstanceState struct {
	CurrentState AlertState
	StartsAt     time.Time     // 현재 상태 시작 시간
	EndsAt       time.Time     // 상태 종료 예정 시간
	FiredAt      time.Time     // Alerting 전환 시간 (Alertmanager 전송)
	ResolvedAt   time.Time     // Normal 전환 시간
	LastSentAt   time.Time     // 마지막 Alertmanager 전송 시간
	LastEvalAt   time.Time     // 마지막 평가 시간

	// For: Pending → Alerting 전환 대기 시간
	ForDuration time.Duration
	// KeepFiringFor: 조건 해소 후에도 Alerting 유지 시간
	KeepFiringFor time.Duration
	// ResendDelay: Alertmanager 재전송 최소 간격
	ResendDelay time.Duration

	// Stale resolution: 시리즈 사라짐 감지
	MissingSeriesCount int
	StaleThreshold     int // 이 횟수만큼 missing이면 자동 해소
}

// NewAlertInstanceState — 새 알림 인스턴스 상태 생성
func NewAlertInstanceState(forDuration, keepFiringFor, resendDelay time.Duration) *AlertInstanceState {
	return &AlertInstanceState{
		CurrentState:  StateNormal,
		ForDuration:   forDuration,
		KeepFiringFor: keepFiringFor,
		ResendDelay:   resendDelay,
		StaleThreshold: 3,
	}
}

// ---------------------------------------------------------------------------
// 3. 상태 전이 로그
// ---------------------------------------------------------------------------

// TransitionLog — 상태 전이 기록
type TransitionLog struct {
	Cycle     int
	Time      time.Time
	From      AlertState
	To        AlertState
	Input     EvalResult
	Reason    string
	SentToAM  bool   // Alertmanager로 전송 여부
}

func (t TransitionLog) String() string {
	sent := " "
	if t.SentToAM {
		sent = ">>AM"
	}
	arrow := fmt.Sprintf("%s -> %s", t.From, t.To)
	if t.From == t.To {
		arrow = fmt.Sprintf("%s (유지)", t.From)
	}
	return fmt.Sprintf("  [Cycle %2d] %-27s | input=%-8s | %s %s",
		t.Cycle, arrow, t.Input, t.Reason, sent)
}

// ---------------------------------------------------------------------------
// 4. 상태 머신
// ---------------------------------------------------------------------------

// StateMachine — 알림 상태 머신
type StateMachine struct {
	state      *AlertInstanceState
	transitions []TransitionLog
	evalInterval time.Duration
}

// NewStateMachine — 상태 머신 생성
func NewStateMachine(forDuration, keepFiringFor, resendDelay, evalInterval time.Duration) *StateMachine {
	return &StateMachine{
		state:        NewAlertInstanceState(forDuration, keepFiringFor, resendDelay),
		transitions:  make([]TransitionLog, 0),
		evalInterval: evalInterval,
	}
}

// ProcessEvaluation — 평가 결과를 받아 상태 전이 처리
func (sm *StateMachine) ProcessEvaluation(cycle int, evalTime time.Time, result EvalResult) {
	prev := sm.state.CurrentState
	var reason string
	sentToAM := false

	switch result {
	case ResultAlerting:
		reason, sentToAM = sm.handleAlerting(evalTime)
	case ResultNormal:
		reason, sentToAM = sm.handleNormal(evalTime)
	case ResultNoData:
		reason, sentToAM = sm.handleNoData(evalTime)
	case ResultError:
		reason, sentToAM = sm.handleError(evalTime)
	}

	sm.state.LastEvalAt = evalTime

	sm.transitions = append(sm.transitions, TransitionLog{
		Cycle:    cycle,
		Time:     evalTime,
		From:     prev,
		To:       sm.state.CurrentState,
		Input:    result,
		Reason:   reason,
		SentToAM: sentToAM,
	})
}

// handleAlerting — 조건 충족 시 처리
func (sm *StateMachine) handleAlerting(evalTime time.Time) (string, bool) {
	sm.state.MissingSeriesCount = 0 // 시리즈 존재 확인

	switch sm.state.CurrentState {
	case StateNormal:
		if sm.state.ForDuration == 0 {
			// For 없으면 바로 Alerting
			sm.state.CurrentState = StateAlerting
			sm.state.StartsAt = evalTime
			sm.state.FiredAt = evalTime
			sm.state.LastSentAt = evalTime
			return "For=0, 즉시 Alerting 전환", true
		}
		// For 있으면 Pending
		sm.state.CurrentState = StatePending
		sm.state.StartsAt = evalTime
		return fmt.Sprintf("Pending 시작, For=%v 대기", sm.state.ForDuration), false

	case StatePending:
		elapsed := evalTime.Sub(sm.state.StartsAt)
		if elapsed >= sm.state.ForDuration {
			// For 기간 경과 → Alerting
			sm.state.CurrentState = StateAlerting
			sm.state.FiredAt = evalTime
			sm.state.LastSentAt = evalTime
			return fmt.Sprintf("For 기간(%v) 경과, Alerting 전환", sm.state.ForDuration), true
		}
		return fmt.Sprintf("Pending 유지 (경과: %v/%v)", elapsed.Round(time.Second), sm.state.ForDuration), false

	case StateAlerting:
		// 이미 Alerting — ResendDelay 확인
		sentToAM := false
		if !sm.state.LastSentAt.IsZero() && evalTime.Sub(sm.state.LastSentAt) >= sm.state.ResendDelay {
			sm.state.LastSentAt = evalTime
			sentToAM = true
		}
		return "Alerting 유지 (조건 지속)", sentToAM

	case StateNoData, StateError:
		// NoData/Error에서 복구 → 조건 충족이므로 Pending/Alerting
		if sm.state.ForDuration == 0 {
			sm.state.CurrentState = StateAlerting
			sm.state.StartsAt = evalTime
			sm.state.FiredAt = evalTime
			sm.state.LastSentAt = evalTime
			return "NoData/Error에서 복구, 즉시 Alerting", true
		}
		sm.state.CurrentState = StatePending
		sm.state.StartsAt = evalTime
		return "NoData/Error에서 복구, Pending 시작", false
	}

	return "알 수 없는 상태", false
}

// handleNormal — 조건 미충족 시 처리
func (sm *StateMachine) handleNormal(evalTime time.Time) (string, bool) {
	sm.state.MissingSeriesCount = 0

	switch sm.state.CurrentState {
	case StateNormal:
		return "Normal 유지", false

	case StatePending:
		// For 기간 내에 조건 해소 → Normal
		sm.state.CurrentState = StateNormal
		sm.state.StartsAt = evalTime
		return "For 기간 내 조건 해소, Normal 복귀", false

	case StateAlerting:
		// KeepFiringFor 확인
		if sm.state.KeepFiringFor > 0 {
			elapsed := evalTime.Sub(sm.state.FiredAt)
			// KeepFiringFor는 조건이 처음 해소된 시점부터 계산해야 하지만
			// 간단히 FiredAt 기준으로 시뮬레이션
			if sm.state.ResolvedAt.IsZero() {
				// 처음 조건 해소 — ResolvedAt 기록, Alerting 유지
				sm.state.ResolvedAt = evalTime
				return fmt.Sprintf("조건 해소, KeepFiringFor=%v 유지 시작", sm.state.KeepFiringFor), false
			}
			keepElapsed := evalTime.Sub(sm.state.ResolvedAt)
			if keepElapsed < sm.state.KeepFiringFor {
				return fmt.Sprintf("KeepFiringFor 유지 (%v/%v)", keepElapsed.Round(time.Second), sm.state.KeepFiringFor), false
			}
			_ = elapsed
		}
		// Normal 전환
		sm.state.CurrentState = StateNormal
		sm.state.StartsAt = evalTime
		sm.state.ResolvedAt = evalTime
		sm.state.LastSentAt = evalTime
		return "조건 해소, Normal 전환 (Resolved 전송)", true

	case StateNoData, StateError:
		sm.state.CurrentState = StateNormal
		sm.state.StartsAt = evalTime
		return "NoData/Error에서 Normal 복구", false
	}

	return "알 수 없는 상태", false
}

// handleNoData — 데이터 없음 처리
func (sm *StateMachine) handleNoData(evalTime time.Time) (string, bool) {
	sm.state.MissingSeriesCount++

	// Stale resolution: N회 연속 missing이면 자동 해소
	if sm.state.CurrentState == StateAlerting && sm.state.MissingSeriesCount >= sm.state.StaleThreshold {
		sm.state.CurrentState = StateNormal
		sm.state.StartsAt = evalTime
		sm.state.ResolvedAt = evalTime
		sm.state.LastSentAt = evalTime
		return fmt.Sprintf("Stale 해소: %d회 연속 NoData, 자동 Normal 전환", sm.state.StaleThreshold), true
	}

	prev := sm.state.CurrentState
	sm.state.CurrentState = StateNoData
	if prev != StateNoData {
		sm.state.StartsAt = evalTime
	}
	return fmt.Sprintf("NoData 전환 (missing count: %d/%d)",
		sm.state.MissingSeriesCount, sm.state.StaleThreshold), false
}

// handleError — 평가 에러 처리
func (sm *StateMachine) handleError(evalTime time.Time) (string, bool) {
	prev := sm.state.CurrentState
	sm.state.CurrentState = StateError
	if prev != StateError {
		sm.state.StartsAt = evalTime
	}
	return "평가 에러 발생", false
}

// ---------------------------------------------------------------------------
// 5. 시나리오 생성
// ---------------------------------------------------------------------------

// generateScenario — 20 사이클 시나리오 생성
func generateScenario() []EvalResult {
	// 시나리오:
	// Cycle 1-3: Normal
	// Cycle 4-8: Alerting (Pending → Alerting 전환)
	// Cycle 9-10: Normal (KeepFiringFor 유지)
	// Cycle 11: Normal (실제 해소)
	// Cycle 12-13: Alerting (재발)
	// Cycle 14-16: NoData
	// Cycle 17: Error
	// Cycle 18-20: Normal (복구)
	scenario := []EvalResult{
		ResultNormal,   // 1
		ResultNormal,   // 2
		ResultNormal,   // 3
		ResultAlerting, // 4: Pending 시작
		ResultAlerting, // 5: Pending 유지
		ResultAlerting, // 6: For 경과 → Alerting
		ResultAlerting, // 7: Alerting 유지
		ResultAlerting, // 8: Alerting 유지
		ResultNormal,   // 9: 조건 해소 → KeepFiringFor 시작
		ResultNormal,   // 10: KeepFiringFor 유지
		ResultNormal,   // 11: KeepFiringFor 경과 → Normal
		ResultAlerting, // 12: 재발 → Pending
		ResultAlerting, // 13: Pending → Alerting
		ResultNoData,   // 14: NoData 1
		ResultNoData,   // 15: NoData 2
		ResultNoData,   // 16: NoData 3 (Stale threshold)
		ResultError,    // 17: Error
		ResultNormal,   // 18: 복구
		ResultNormal,   // 19: Normal 유지
		ResultNormal,   // 20: Normal 유지
	}
	return scenario
}

// ---------------------------------------------------------------------------
// 6. ASCII 상태 다이어그램
// ---------------------------------------------------------------------------

func printStateDiagram() {
	fmt.Println(`
  ┌───────────────────────────────────────────────────────────────────────┐
  │                    Alert State Machine                                │
  │                                                                       │
  │                        condition=true                                 │
  │              ┌────────────────────────────────┐                       │
  │              │                                │                       │
  │              │                                ▼                       │
  │         ┌─────────┐   condition=true    ┌──────────┐                  │
  │   ┌────▶│ Normal  │───────────────────▶│ Pending  │──┐               │
  │   │     └─────────┘                    └──────────┘  │               │
  │   │          ▲                              │         │ For elapsed  │
  │   │          │   condition=false            │         │               │
  │   │          └──────────────────────────────┘         │               │
  │   │          │                                        ▼               │
  │   │          │   condition=false            ┌───────────┐             │
  │   │          │   (+ KeepFiringFor           │ Alerting  │             │
  │   │          │     elapsed)                 │           │             │
  │   │          └──────────────────────────────┤           │             │
  │   │                                         └───────────┘             │
  │   │                                                                   │
  │   │  stale / recover                                                  │
  │   │                                                                   │
  │   │     ┌──────────┐                    ┌──────────┐                  │
  │   ├─────│  NoData   │                    │  Error   │                  │
  │   │     └──────────┘                    └──────────┘                  │
  │   │          ▲                               ▲                        │
  │   │          │  no data received             │  evaluation error      │
  │   │          └───────── Any ─────────────────┘                        │
  │   │                                                                   │
  │   │  KeepFiringFor:                                                   │
  │   │    Alerting + condition=false → 일정 시간 Alerting 유지            │
  │   │                                                                   │
  │   │  Stale Resolution:                                                │
  │   └── N번 연속 NoData → 자동 Normal 전환 (Resolved 전송)               │
  │                                                                       │
  │   ResendDelay: Alertmanager 재전송 최소 간격 (기본 30초)               │
  └───────────────────────────────────────────────────────────────────────┘
`)
}

// ---------------------------------------------------------------------------
// 7. 메인
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("==================================================")
	fmt.Println("  Grafana Alert State Machine Simulation")
	fmt.Println("==================================================")

	rand.Seed(time.Now().UnixNano())

	// 상태 머신 설정
	forDuration := 10 * time.Second      // Pending → Alerting 대기 시간
	keepFiringFor := 10 * time.Second    // 조건 해소 후 Alerting 유지 시간
	resendDelay := 30 * time.Second      // Alertmanager 재전송 간격
	evalInterval := 5 * time.Second      // 평가 간격

	fmt.Println("\n--- 설정 ---")
	fmt.Printf("  For:            %v (Pending 최소 유지 시간)\n", forDuration)
	fmt.Printf("  KeepFiringFor:  %v (조건 해소 후 Alerting 유지)\n", keepFiringFor)
	fmt.Printf("  ResendDelay:    %v (Alertmanager 재전송 간격)\n", resendDelay)
	fmt.Printf("  EvalInterval:   %v (평가 간격)\n", evalInterval)

	sm := NewStateMachine(forDuration, keepFiringFor, resendDelay, evalInterval)

	// 시나리오 생성 및 실행
	scenario := generateScenario()

	fmt.Println("\n--- 시나리오 (20 사이클) ---")
	fmt.Print("  입력: ")
	for i, r := range scenario {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(r)
	}
	fmt.Println()

	fmt.Println("\n--- 상태 전이 실행 ---")
	baseTime := time.Now()

	for i, result := range scenario {
		cycle := i + 1
		evalTime := baseTime.Add(time.Duration(i) * evalInterval)
		sm.ProcessEvaluation(cycle, evalTime, result)
	}

	// 전이 로그 출력
	for _, t := range sm.transitions {
		fmt.Println(t)
	}

	// 상태 타임라인 시각화
	fmt.Println("\n--- 상태 타임라인 ---")
	fmt.Print("  Cycle: ")
	for i := 1; i <= 20; i++ {
		fmt.Printf("%2d ", i)
	}
	fmt.Println()

	fmt.Print("  Input: ")
	for _, r := range scenario {
		switch r {
		case ResultNormal:
			fmt.Print(" N ")
		case ResultAlerting:
			fmt.Print(" A ")
		case ResultNoData:
			fmt.Print(" ? ")
		case ResultError:
			fmt.Print(" E ")
		}
	}
	fmt.Println()

	fmt.Print("  State: ")
	for _, t := range sm.transitions {
		switch t.To {
		case StateNormal:
			fmt.Print(" N ")
		case StatePending:
			fmt.Print(" P ")
		case StateAlerting:
			fmt.Print("[A]")
		case StateNoData:
			fmt.Print(" ? ")
		case StateError:
			fmt.Print(" ! ")
		}
	}
	fmt.Println()

	fmt.Print("  AM:    ")
	for _, t := range sm.transitions {
		if t.SentToAM {
			fmt.Print(">>>")
		} else {
			fmt.Print(" . ")
		}
	}
	fmt.Println()

	// 통계
	fmt.Println("\n--- 상태 전이 통계 ---")
	stateCounts := make(map[AlertState]int)
	transitionCount := 0
	amSendCount := 0
	for i, t := range sm.transitions {
		stateCounts[t.To]++
		if t.SentToAM {
			amSendCount++
		}
		if i > 0 && t.From != t.To {
			transitionCount++
		}
	}

	fmt.Printf("  총 평가 횟수: %d\n", len(sm.transitions))
	fmt.Printf("  상태 전이 횟수: %d\n", transitionCount)
	fmt.Printf("  Alertmanager 전송 횟수: %d\n", amSendCount)
	fmt.Println()
	fmt.Printf("  %-12s %s\n", "상태", "횟수")
	fmt.Println("  " + strings.Repeat("-", 20))
	for _, state := range []AlertState{StateNormal, StatePending, StateAlerting, StateNoData, StateError} {
		bar := strings.Repeat("#", stateCounts[state])
		fmt.Printf("  %-12s %d  %s\n", state, stateCounts[state], bar)
	}

	// ASCII 상태 다이어그램
	fmt.Println("\n--- 알림 상태 전이 다이어그램 ---")
	printStateDiagram()

	// Grafana 실제 상태 관리 설명
	fmt.Println("--- Grafana 상태 관리 구현 ---")
	fmt.Println(`
  파일: pkg/services/ngalert/state/manager.go

  StateManager 핵심 메서드:

  1. ProcessEvalResults(ctx, evalTime, rule, results, extraLabels)
     - 평가 결과를 받아 각 인스턴스의 상태를 업데이트
     - 인스턴스 키: rule UID + labels 조합

  2. setNextState(ctx, rule, result, instance)
     - 상태 전이 규칙 적용
     - For 타이머 확인
     - KeepFiringFor 확인

  3. staleResultsHandler(ctx, rule, states)
     - 이전에 존재했으나 현재 없는 시리즈 감지
     - StaleThreshold 초과 시 자동 Resolved

  4. sendToAlertmanager(ctx, instance)
     - ResendDelay 확인
     - Alertmanager API로 POST
     - EndsAt 계산: now + 4 * evalInterval (keep-alive)
`)
}
