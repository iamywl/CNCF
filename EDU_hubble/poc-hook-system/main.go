// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Hook/Plugin 패턴
//
// Hubble Observer는 7개의 확장 포인트(Hook)을 제공합니다.
// 각 Hook은 체인으로 실행되며, stop=true를 반환하면 체인이 중단됩니다.
//
// 이 패턴 덕분에 Observer 코어 코드를 수정하지 않고도
// 메트릭, 내보내기, 속도 제한 등의 기능을 추가할 수 있습니다.
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ========================================
// 1. 데이터 타입
// ========================================

type Flow struct {
	Source      string
	Destination string
	Verdict     string
	Protocol    string
	Timestamp   time.Time
}

// ========================================
// 2. Hook 인터페이스 (Hubble의 observeroption 패턴)
// ========================================

// OnDecodedFlow는 Flow가 디코딩된 후 호출되는 Hook입니다.
//
// 실제 Hubble에서는:
//   type OnDecodedFlow interface {
//       OnDecodedFlow(ctx context.Context, flow *pb.Flow) (stop bool, err error)
//   }
//
// stop=true: 이후 Hook과 처리를 모두 중단
// stop=false: 다음 Hook으로 계속 진행
type OnDecodedFlow interface {
	OnDecodedFlow(ctx context.Context, flow *Flow) (stop bool, err error)
	Name() string
}

// OnDecodedFlowFunc는 함수를 Hook 인터페이스로 감싸는 어댑터입니다.
// 간단한 Hook을 구조체 없이 함수로 정의할 수 있습니다.
type OnDecodedFlowFunc struct {
	name string
	fn   func(ctx context.Context, flow *Flow) (bool, error)
}

func (f OnDecodedFlowFunc) OnDecodedFlow(ctx context.Context, flow *Flow) (bool, error) {
	return f.fn(ctx, flow)
}
func (f OnDecodedFlowFunc) Name() string { return f.name }

// ========================================
// 3. 구체적인 Hook 구현들
// ========================================

// MetricsHook은 모든 Flow를 카운트하는 메트릭 Hook입니다.
// 실제 Hubble의 pkg/hubble/metrics/ 하위 핸들러들이 이 패턴을 사용합니다.
type MetricsHook struct {
	flowCount    int
	dropCount    int
	forwardCount int
}

func (m *MetricsHook) Name() string { return "Metrics" }

func (m *MetricsHook) OnDecodedFlow(ctx context.Context, flow *Flow) (bool, error) {
	m.flowCount++
	switch flow.Verdict {
	case "DROPPED":
		m.dropCount++
	case "FORWARDED":
		m.forwardCount++
	}
	// 메트릭은 체인을 중단하지 않음 (항상 false 반환)
	return false, nil
}

func (m *MetricsHook) Report() {
	fmt.Printf("    총 플로우: %d, FORWARDED: %d, DROPPED: %d\n",
		m.flowCount, m.forwardCount, m.dropCount)
}

// RateLimiterHook은 초당 N개 이상의 Flow가 오면 처리를 중단합니다.
type RateLimiterHook struct {
	maxPerSecond int
	count        int
	windowStart  time.Time
	blocked      int
}

func (r *RateLimiterHook) Name() string { return "RateLimiter" }

func (r *RateLimiterHook) OnDecodedFlow(ctx context.Context, flow *Flow) (bool, error) {
	now := time.Now()
	if now.Sub(r.windowStart) > time.Second {
		r.count = 0
		r.windowStart = now
	}

	r.count++
	if r.count > r.maxPerSecond {
		r.blocked++
		// stop=true: 이 Flow의 이후 처리를 모두 중단!
		return true, nil
	}
	return false, nil
}

// AuditLogHook은 DROPPED 이벤트를 감사 로그로 기록합니다.
type AuditLogHook struct {
	logs []string
}

func (a *AuditLogHook) Name() string { return "AuditLog" }

func (a *AuditLogHook) OnDecodedFlow(ctx context.Context, flow *Flow) (bool, error) {
	if flow.Verdict == "DROPPED" {
		log := fmt.Sprintf("[AUDIT] %s → %s DROPPED at %s",
			flow.Source, flow.Destination, flow.Timestamp.Format("15:04:05"))
		a.logs = append(a.logs, log)
	}
	return false, nil
}

// ========================================
// 4. Hook Chain 실행 엔진
// ========================================

// RunHookChain은 모든 Hook을 순서대로 실행합니다.
// Hubble Observer의 hook 실행 로직과 동일한 패턴입니다.
//
// 핵심: stop=true를 반환한 Hook이 있으면 이후 Hook은 실행되지 않음
func RunHookChain(ctx context.Context, hooks []OnDecodedFlow, flow *Flow) (stopped bool) {
	for _, hook := range hooks {
		stop, err := hook.OnDecodedFlow(ctx, flow)
		if err != nil {
			fmt.Printf("      ✗ [%s] 에러: %v\n", hook.Name(), err)
			return true
		}
		if stop {
			fmt.Printf("      ■ [%s] 체인 중단 (stop=true)\n", hook.Name())
			return true
		}
		fmt.Printf("      ✓ [%s] 통과 (stop=false)\n", hook.Name())
	}
	return false
}

// ========================================
// 5. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Hook/Plugin 패턴 ===")
	fmt.Println()
	fmt.Println("Observer의 Hook 체인:")
	fmt.Println("  Flow → [Metrics] → [RateLimiter] → [AuditLog] → 전달")
	fmt.Println()
	fmt.Println("각 Hook은 (stop bool, error) 반환:")
	fmt.Println("  stop=false → 다음 Hook으로 계속")
	fmt.Println("  stop=true  → 체인 즉시 중단, 이후 Hook 실행 안 함")
	fmt.Println()
	fmt.Println("-------------------------------------------")

	ctx := context.Background()

	// Hook 인스턴스 생성
	metrics := &MetricsHook{}
	rateLimiter := &RateLimiterHook{maxPerSecond: 3, windowStart: time.Now()}
	auditLog := &AuditLogHook{}

	// Hook 체인 등록 (순서가 중요!)
	// 실제 Hubble: Options.OnDecodedFlow = []OnDecodedFlow{metrics, rateLimiter, auditLog}
	hooks := []OnDecodedFlow{metrics, rateLimiter, auditLog}

	fmt.Println()
	fmt.Printf("  등록된 Hook 체인: %s\n",
		strings.Join([]string{metrics.Name(), rateLimiter.Name(), auditLog.Name()}, " → "))
	fmt.Printf("  속도 제한: %d flows/sec\n", rateLimiter.maxPerSecond)
	fmt.Println()

	// 테스트 Flow들
	flows := []Flow{
		{Source: "frontend", Destination: "backend:8080", Verdict: "FORWARDED", Protocol: "TCP", Timestamp: time.Now()},
		{Source: "frontend", Destination: "coredns:53", Verdict: "FORWARDED", Protocol: "DNS", Timestamp: time.Now()},
		{Source: "scanner", Destination: "database:3306", Verdict: "DROPPED", Protocol: "TCP", Timestamp: time.Now()},
		{Source: "frontend", Destination: "cache:6379", Verdict: "FORWARDED", Protocol: "TCP", Timestamp: time.Now()},   // 속도 제한에 걸림
		{Source: "backend", Destination: "external:443", Verdict: "FORWARDED", Protocol: "TCP", Timestamp: time.Now()},   // 속도 제한에 걸림
	}

	delivered := 0
	blocked := 0

	for i, flow := range flows {
		fmt.Printf("  ── Flow %d: %s → %s [%s] ──\n", i+1, flow.Source, flow.Destination, flow.Verdict)

		stopped := RunHookChain(ctx, hooks, &flow)

		if stopped {
			blocked++
			fmt.Printf("      → 전달 안 됨 (Hook이 중단)\n")
		} else {
			delivered++
			fmt.Printf("      → ✓ 클라이언트에 전달!\n")
		}
		fmt.Println()
	}

	// 결과 요약
	fmt.Println("-------------------------------------------")
	fmt.Println()
	fmt.Println("[결과 요약]")
	fmt.Println()
	fmt.Printf("  전달: %d, 차단: %d (속도 제한)\n", delivered, blocked)
	fmt.Println()

	fmt.Println("  [Metrics Hook 리포트]")
	metrics.Report()

	fmt.Println()
	fmt.Printf("  [RateLimiter Hook 리포트]\n")
	fmt.Printf("    차단된 Flow: %d (초당 %d개 초과)\n", rateLimiter.blocked, rateLimiter.maxPerSecond)

	fmt.Println()
	fmt.Println("  [AuditLog Hook 리포트]")
	for _, log := range auditLog.logs {
		fmt.Printf("    %s\n", log)
	}
	if len(auditLog.logs) == 0 {
		fmt.Println("    (감사 로그 없음 - DROPPED가 속도 제한 전에 통과한 것만)")
	}

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - Hook 순서가 중요: Metrics를 먼저 두면 모든 Flow를 카운트")
	fmt.Println("  - RateLimiter가 stop=true 반환 → AuditLog는 실행 안 됨")
	fmt.Println("  - 인터페이스 기반: 새 Hook 추가 시 Observer 코드 수정 불필요")
	fmt.Println("  - 함수 어댑터: 간단한 Hook은 구조체 없이 함수로 정의 가능")
}
