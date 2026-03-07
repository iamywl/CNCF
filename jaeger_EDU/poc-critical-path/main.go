package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Jaeger 크리티컬 패스 분석 시뮬레이터
// =============================================================================
//
// Jaeger UI에는 트레이스의 크리티컬 패스를 분석하는 기능이 있다.
// 원래 TypeScript로 구현된 이 알고리즘을 Go로 포팅하여 시뮬레이션한다.
//
// 크리티컬 패스란: 트레이스의 전체 실행 시간에 직접적으로 기여하는
// 스팬들의 경로이다. 이 경로 위의 어떤 스팬이라도 느려지면
// 전체 트레이스의 완료 시간이 늘어난다.
//
// 핵심 알고리즘:
// 1. findLastFinishingChildSpan: 가장 늦게 끝나는 자식 스팬 찾기
// 2. sanitizeOverFlowingChildren: 부모 범위를 넘는 자식 보정
// 3. computeCriticalPath: 재귀적으로 크리티컬 패스 계산
// 4. Self-time 계산: 크리티컬 패스 상 각 스팬의 고유 실행 시간
//
// 참조: jaeger-ui/packages/jaeger-ui/src/utils/TreeNode.tsx
//       jaeger-ui/packages/jaeger-ui/src/TracePage/CriticalPath/
// =============================================================================

// --- 데이터 모델 ---

// Span은 트레이스 내의 단일 스팬을 나타낸다.
type Span struct {
	TraceID   string
	SpanID    string
	ParentID  string
	Service   string
	Operation string
	StartTime time.Time
	Duration  time.Duration
	Children  []*Span // 자식 스팬 목록 (트리 구축 후 채워짐)
}

// EndTime은 스팬의 종료 시간을 반환한다.
func (s *Span) EndTime() time.Time {
	return s.StartTime.Add(s.Duration)
}

// CriticalPathSegment는 크리티컬 패스의 한 구간을 나타낸다.
type CriticalPathSegment struct {
	Span     *Span
	SelfTime time.Duration // 이 스팬이 직접 사용한 시간 (자식 제외)
	Section  string        // "self" 또는 "waiting"
}

// CriticalPathResult는 크리티컬 패스 분석 결과를 담는다.
type CriticalPathResult struct {
	Segments       []CriticalPathSegment
	TotalDuration  time.Duration
	CriticalTime   time.Duration // 크리티컬 패스 상 총 시간
	AllSpans       []*Span       // 트레이스의 모든 스팬
	CriticalSpanIDs map[string]bool // 크리티컬 패스 상의 스팬 ID들
}

// --- 트리 구축 ---

// BuildTraceTree는 플랫 스팬 리스트에서 트리를 구축한다.
// 루트 스팬을 반환한다.
func BuildTraceTree(spans []Span) *Span {
	// SpanID -> Span 맵
	spanMap := make(map[string]*Span)
	for i := range spans {
		spans[i].Children = make([]*Span, 0)
		spanMap[spans[i].SpanID] = &spans[i]
	}

	// 부모-자식 관계 구축
	var root *Span
	for i := range spans {
		span := &spans[i]
		if span.ParentID == "" {
			root = span
		} else {
			if parent, ok := spanMap[span.ParentID]; ok {
				parent.Children = append(parent.Children, span)
			}
		}
	}

	// 자식을 시작 시간 순으로 정렬
	sortChildren(root)

	return root
}

// sortChildren은 재귀적으로 자식 스팬을 시작 시간 순으로 정렬한다.
func sortChildren(span *Span) {
	if span == nil {
		return
	}
	sort.Slice(span.Children, func(i, j int) bool {
		return span.Children[i].StartTime.Before(span.Children[j].StartTime)
	})
	for _, child := range span.Children {
		sortChildren(child)
	}
}

// --- 크리티컬 패스 알고리즘 ---

// findLastFinishingChildSpan은 가장 늦게 끝나는 자식 스팬을 찾는다.
// Jaeger UI의 findLastFinishingChildSpan 함수에 대응한다.
//
// 핵심 아이디어: 부모 스팬의 종료 시점에 가장 가까운 시점까지
// 실행되는 자식이 크리티컬 패스에 속한다.
func findLastFinishingChildSpan(span *Span) *Span {
	if len(span.Children) == 0 {
		return nil
	}

	var lastChild *Span
	var latestEnd time.Time

	for _, child := range span.Children {
		childEnd := child.EndTime()
		if lastChild == nil || childEnd.After(latestEnd) {
			lastChild = child
			latestEnd = childEnd
		}
	}

	return lastChild
}

// sanitizeOverFlowingChildren은 부모 범위를 넘는 자식 스팬을 보정한다.
// Jaeger UI의 sanitizeOverFlowingChildren 함수에 대응한다.
//
// 실제 트레이스에서 클록 스큐나 비동기 작업으로 인해 자식 스팬이
// 부모 스팬의 범위를 벗어나는 경우가 있다. 이 함수는 그런 자식의
// duration을 부모 범위 내로 클리핑한다.
func sanitizeOverFlowingChildren(parent *Span) {
	parentEnd := parent.EndTime()

	for _, child := range parent.Children {
		childEnd := child.EndTime()

		// 자식이 부모보다 늦게 끝나면 클리핑
		if childEnd.After(parentEnd) {
			child.Duration = parentEnd.Sub(child.StartTime)
			if child.Duration < 0 {
				child.Duration = 0
			}
		}

		// 자식이 부모보다 일찍 시작하면 클리핑
		if child.StartTime.Before(parent.StartTime) {
			adjustment := parent.StartTime.Sub(child.StartTime)
			child.StartTime = parent.StartTime
			child.Duration -= adjustment
			if child.Duration < 0 {
				child.Duration = 0
			}
		}

		// 재귀적으로 하위 자식도 보정
		sanitizeOverFlowingChildren(child)
	}
}

// computeCriticalPath는 재귀적으로 크리티컬 패스를 계산한다.
// Jaeger UI의 computeCriticalPath 함수에 대응한다.
//
// 알고리즘:
// 1. 현재 스팬에서 가장 늦게 끝나는 자식을 찾는다 (lastFinishingChild)
// 2. 그 자식에 대해 재귀적으로 크리티컬 패스를 계산한다
// 3. 현재 스팬의 self-time을 계산한다 (자식에게 넘겨주지 않은 시간)
// 4. 크리티컬 패스 세그먼트를 기록한다
func computeCriticalPath(span *Span, result *CriticalPathResult) {
	if span == nil {
		return
	}

	result.CriticalSpanIDs[span.SpanID] = true

	if len(span.Children) == 0 {
		// 리프 노드 — 전체 duration이 self-time
		result.Segments = append(result.Segments, CriticalPathSegment{
			Span:     span,
			SelfTime: span.Duration,
			Section:  "self",
		})
		result.CriticalTime += span.Duration
		return
	}

	// 가장 늦게 끝나는 자식 찾기
	lastChild := findLastFinishingChildSpan(span)
	if lastChild == nil {
		result.Segments = append(result.Segments, CriticalPathSegment{
			Span:     span,
			SelfTime: span.Duration,
			Section:  "self",
		})
		result.CriticalTime += span.Duration
		return
	}

	// Self-time 계산: 현재 스팬의 duration에서 lastChild가 커버하는 시간을 빼기
	// 자식 실행 전/후의 self-time을 계산
	selfTimeBefore := time.Duration(0)
	selfTimeAfter := time.Duration(0)

	// 자식 시작 전의 self-time
	if lastChild.StartTime.After(span.StartTime) {
		selfTimeBefore = lastChild.StartTime.Sub(span.StartTime)
	}

	// 마지막 자식 종료 후의 self-time
	lastChildEnd := lastChild.EndTime()
	spanEnd := span.EndTime()
	if spanEnd.After(lastChildEnd) {
		selfTimeAfter = spanEnd.Sub(lastChildEnd)
	}

	totalSelfTime := selfTimeBefore + selfTimeAfter

	if totalSelfTime > 0 {
		result.Segments = append(result.Segments, CriticalPathSegment{
			Span:     span,
			SelfTime: totalSelfTime,
			Section:  "self",
		})
		result.CriticalTime += totalSelfTime
	}

	// 재귀: lastChild에 대해 크리티컬 패스 계산
	computeCriticalPath(lastChild, result)
}

// AnalyzeCriticalPath는 트레이스의 크리티컬 패스를 분석한다.
func AnalyzeCriticalPath(spans []Span) CriticalPathResult {
	// 1. 트리 구축
	root := BuildTraceTree(spans)

	// 2. 오버플로우 자식 보정
	sanitizeOverFlowingChildren(root)

	// 3. 크리티컬 패스 계산
	result := CriticalPathResult{
		TotalDuration:   root.Duration,
		CriticalSpanIDs: make(map[string]bool),
	}

	// 모든 스팬 수집
	var collectSpans func(s *Span)
	collectSpans = func(s *Span) {
		result.AllSpans = append(result.AllSpans, s)
		for _, child := range s.Children {
			collectSpans(child)
		}
	}
	collectSpans(root)

	computeCriticalPath(root, &result)

	return result
}

// --- 시각화 ---

// VisualizeTrace는 트레이스를 ASCII 타임라인으로 시각화한다.
func VisualizeTrace(result CriticalPathResult) {
	if len(result.AllSpans) == 0 {
		fmt.Println("  (빈 트레이스)")
		return
	}

	// 최소 시작시간 찾기
	minStart := result.AllSpans[0].StartTime
	for _, span := range result.AllSpans {
		if span.StartTime.Before(minStart) {
			minStart = span.StartTime
		}
	}

	// 타임라인 너비
	const timelineWidth = 60
	totalDuration := result.TotalDuration
	if totalDuration == 0 {
		totalDuration = time.Millisecond
	}

	fmt.Println()
	fmt.Printf("  트레이스 총 시간: %v\n", totalDuration)
	fmt.Printf("  크리티컬 패스 시간: %v\n", result.CriticalTime)
	fmt.Println()

	// 타임라인 헤더
	fmt.Printf("  %-20s %-10s ", "스팬", "서비스")
	fmt.Printf("|")
	for i := 0; i < timelineWidth; i++ {
		if i%10 == 0 {
			pct := float64(i) / float64(timelineWidth) * 100
			label := fmt.Sprintf("%.0f%%", pct)
			fmt.Print(label)
			i += len(label) - 1
		} else {
			fmt.Print("-")
		}
	}
	fmt.Println("|")
	fmt.Println("  " + strings.Repeat("-", 20+10+2+timelineWidth+1))

	// 각 스팬의 타임라인
	for _, span := range result.AllSpans {
		relStart := span.StartTime.Sub(minStart)
		startPos := int(float64(relStart) / float64(totalDuration) * float64(timelineWidth))
		endPos := int(float64(relStart+span.Duration) / float64(totalDuration) * float64(timelineWidth))

		if startPos >= timelineWidth {
			startPos = timelineWidth - 1
		}
		if endPos > timelineWidth {
			endPos = timelineWidth
		}
		if endPos <= startPos {
			endPos = startPos + 1
		}

		// 크리티컬 패스 여부에 따라 문자 선택
		isCritical := result.CriticalSpanIDs[span.SpanID]
		fillChar := "."
		if isCritical {
			fillChar = "#"
		}

		// 스팬 이름 (잘라내기)
		name := span.Operation
		if len(name) > 18 {
			name = name[:18]
		}
		svc := span.Service
		if len(svc) > 8 {
			svc = svc[:8]
		}

		marker := " "
		if isCritical {
			marker = "*"
		}

		fmt.Printf(" %s%-19s %-10s|", marker, name, svc)
		for i := 0; i < timelineWidth; i++ {
			if i >= startPos && i < endPos {
				fmt.Print(fillChar)
			} else {
				fmt.Print(" ")
			}
		}
		fmt.Println("|")
	}

	fmt.Println()
	fmt.Println("  범례: # = 크리티컬 패스, . = 비크리티컬, * = 크리티컬 패스 스팬")

	// 크리티컬 패스 세그먼트 상세
	fmt.Println("\n  === 크리티컬 패스 세그먼트 ===")
	fmt.Printf("  %-25s %-15s %-12s %-10s\n", "스팬", "서비스", "Self-Time", "비율")
	fmt.Println("  " + strings.Repeat("-", 65))

	for _, seg := range result.Segments {
		pct := float64(seg.SelfTime) / float64(totalDuration) * 100
		fmt.Printf("  %-25s %-15s %-12v %6.1f%%\n",
			seg.Span.Operation, seg.Span.Service, seg.SelfTime, pct)
	}
}

// =============================================================================
// 테스트 케이스
// =============================================================================

// 테스트 1: 직렬 실행 (A -> B -> C)
// 모든 스팬이 순차적으로 실행됨 — 전체가 크리티컬 패스
func testSerialExecution() {
	fmt.Println("\n########################################################")
	fmt.Println("# 테스트 1: 직렬 실행 (A -> B -> C)")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("구조:")
	fmt.Println("  A [====================] 300ms")
	fmt.Println("    B [============]       200ms")
	fmt.Println("      C [======]           100ms")
	fmt.Println()
	fmt.Println("기대: 모든 스팬이 크리티컬 패스에 포함")
	fmt.Println("  A의 self-time: 300 - 200 = 100ms (B 시작 전/후)")
	fmt.Println("  B의 self-time: 200 - 100 = 100ms (C 시작 전/후)")
	fmt.Println("  C의 self-time: 100ms (리프)")

	now := time.Now()
	spans := []Span{
		{TraceID: "t1", SpanID: "A", ParentID: "", Service: "gateway",
			Operation: "handleRequest", StartTime: now, Duration: 300 * time.Millisecond},
		{TraceID: "t1", SpanID: "B", ParentID: "A", Service: "user-svc",
			Operation: "getUser", StartTime: now.Add(50 * time.Millisecond), Duration: 200 * time.Millisecond},
		{TraceID: "t1", SpanID: "C", ParentID: "B", Service: "database",
			Operation: "SELECT", StartTime: now.Add(100 * time.Millisecond), Duration: 100 * time.Millisecond},
	}

	result := AnalyzeCriticalPath(spans)
	VisualizeTrace(result)
}

// 테스트 2: 병렬 실행 (A -> B|C, B가 더 늦게 끝남)
// B가 C보다 늦게 끝나므로 B가 크리티컬 패스에 속함
func testParallelExecution() {
	fmt.Println("\n\n########################################################")
	fmt.Println("# 테스트 2: 병렬 실행 (A -> B|C, B가 늦게 끝남)")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("구조:")
	fmt.Println("  A [========================] 400ms")
	fmt.Println("    B [==================]     300ms  <-- 크리티컬!")
	fmt.Println("    C [==========]             150ms")
	fmt.Println()
	fmt.Println("기대: B가 크리티컬 패스 (더 늦게 끝남), C는 비크리티컬")

	now := time.Now()
	spans := []Span{
		{TraceID: "t2", SpanID: "A", ParentID: "", Service: "gateway",
			Operation: "handleRequest", StartTime: now, Duration: 400 * time.Millisecond},
		{TraceID: "t2", SpanID: "B", ParentID: "A", Service: "order-svc",
			Operation: "processOrder", StartTime: now.Add(50 * time.Millisecond), Duration: 300 * time.Millisecond},
		{TraceID: "t2", SpanID: "C", ParentID: "A", Service: "inventory-svc",
			Operation: "checkStock", StartTime: now.Add(50 * time.Millisecond), Duration: 150 * time.Millisecond},
	}

	result := AnalyzeCriticalPath(spans)
	VisualizeTrace(result)
}

// 테스트 3: 중첩 병렬 (A -> B -> D|E, A -> C)
// 복잡한 병렬 + 직렬 혼합
func testNestedParallelism() {
	fmt.Println("\n\n########################################################")
	fmt.Println("# 테스트 3: 중첩 병렬")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("구조:")
	fmt.Println("  A [==============================] 500ms")
	fmt.Println("    B [====================]         350ms")
	fmt.Println("      D [==========]                 150ms")
	fmt.Println("      E [================]           250ms  <-- D보다 늦게 끝남")
	fmt.Println("    C [===========]                  200ms")
	fmt.Println()
	fmt.Println("기대: A -> B -> E 가 크리티컬 패스")
	fmt.Println("  (B가 C보다 늦게 끝나고, E가 D보다 늦게 끝남)")

	now := time.Now()
	spans := []Span{
		{TraceID: "t3", SpanID: "A", ParentID: "", Service: "gateway",
			Operation: "handleRequest", StartTime: now, Duration: 500 * time.Millisecond},
		{TraceID: "t3", SpanID: "B", ParentID: "A", Service: "order-svc",
			Operation: "processOrder", StartTime: now.Add(50 * time.Millisecond), Duration: 350 * time.Millisecond},
		{TraceID: "t3", SpanID: "C", ParentID: "A", Service: "notification-svc",
			Operation: "sendNotif", StartTime: now.Add(50 * time.Millisecond), Duration: 200 * time.Millisecond},
		{TraceID: "t3", SpanID: "D", ParentID: "B", Service: "payment-svc",
			Operation: "charge", StartTime: now.Add(80 * time.Millisecond), Duration: 150 * time.Millisecond},
		{TraceID: "t3", SpanID: "E", ParentID: "B", Service: "database",
			Operation: "INSERT", StartTime: now.Add(80 * time.Millisecond), Duration: 250 * time.Millisecond},
	}

	result := AnalyzeCriticalPath(spans)
	VisualizeTrace(result)
}

// 테스트 4: 자식이 부모를 초과하는 경우 (오버플로우 보정)
func testOverflowingChildren() {
	fmt.Println("\n\n########################################################")
	fmt.Println("# 테스트 4: 자식 오버플로우 보정")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("구조 (보정 전):")
	fmt.Println("  A [==============] 200ms")
	fmt.Println("    B [===================] 250ms  <-- 부모보다 길다!")
	fmt.Println()
	fmt.Println("기대: B의 duration이 부모 A의 범위 내로 클리핑됨")
	fmt.Println("  B 보정 후: 200 - 30 = 170ms (A 시작 + 30ms에서 시작)")

	now := time.Now()
	spans := []Span{
		{TraceID: "t4", SpanID: "A", ParentID: "", Service: "gateway",
			Operation: "handleRequest", StartTime: now, Duration: 200 * time.Millisecond},
		{TraceID: "t4", SpanID: "B", ParentID: "A", Service: "slow-service",
			Operation: "slowOperation", StartTime: now.Add(30 * time.Millisecond), Duration: 250 * time.Millisecond},
	}

	result := AnalyzeCriticalPath(spans)
	VisualizeTrace(result)
}

// 테스트 5: 실제적인 마이크로서비스 트레이스
func testRealisticTrace() {
	fmt.Println("\n\n########################################################")
	fmt.Println("# 테스트 5: 실제적인 마이크로서비스 트레이스")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("구조: 전형적인 e-commerce 주문 처리 흐름")
	fmt.Println()
	fmt.Println("  frontend.GET /checkout           [==================================] 800ms")
	fmt.Println("    api.validateSession             [===]                               50ms")
	fmt.Println("    api.processCheckout             [=============================]     600ms")
	fmt.Println("      cart.getItems                  [=====]                            80ms")
	fmt.Println("        db.SELECT_cart                [===]                             40ms")
	fmt.Println("      order.createOrder              [===================]             350ms")
	fmt.Println("        db.INSERT_order               [===]                            50ms")
	fmt.Println("        payment.charge                [============]                   200ms")
	fmt.Println("          bank.processPayment          [=========]                     150ms")
	fmt.Println("        inventory.reserve             [====]                           60ms")
	fmt.Println("      notification.send              [======]                          100ms")
	fmt.Println("        email.deliver                 [====]                           70ms")

	now := time.Now()
	spans := []Span{
		// 루트: frontend
		{TraceID: "t5", SpanID: "s1", ParentID: "", Service: "frontend",
			Operation: "GET /checkout", StartTime: now, Duration: 800 * time.Millisecond},

		// api: 세션 검증 (순차, 빠름)
		{TraceID: "t5", SpanID: "s2", ParentID: "s1", Service: "api",
			Operation: "validateSession", StartTime: now.Add(10 * time.Millisecond), Duration: 50 * time.Millisecond},

		// api: 체크아웃 처리 (주요 처리)
		{TraceID: "t5", SpanID: "s3", ParentID: "s1", Service: "api",
			Operation: "processCheckout", StartTime: now.Add(70 * time.Millisecond), Duration: 600 * time.Millisecond},

		// cart: 장바구니 조회 (순차)
		{TraceID: "t5", SpanID: "s4", ParentID: "s3", Service: "cart",
			Operation: "getItems", StartTime: now.Add(80 * time.Millisecond), Duration: 80 * time.Millisecond},

		// db: 장바구니 쿼리
		{TraceID: "t5", SpanID: "s5", ParentID: "s4", Service: "database",
			Operation: "SELECT cart", StartTime: now.Add(90 * time.Millisecond), Duration: 40 * time.Millisecond},

		// order: 주문 생성 (병렬 작업 포함)
		{TraceID: "t5", SpanID: "s6", ParentID: "s3", Service: "order",
			Operation: "createOrder", StartTime: now.Add(170 * time.Millisecond), Duration: 350 * time.Millisecond},

		// db: 주문 삽입
		{TraceID: "t5", SpanID: "s7", ParentID: "s6", Service: "database",
			Operation: "INSERT order", StartTime: now.Add(180 * time.Millisecond), Duration: 50 * time.Millisecond},

		// payment: 결제 처리 (가장 느림 → 크리티컬!)
		{TraceID: "t5", SpanID: "s8", ParentID: "s6", Service: "payment",
			Operation: "charge", StartTime: now.Add(240 * time.Millisecond), Duration: 200 * time.Millisecond},

		// bank: 은행 API 호출
		{TraceID: "t5", SpanID: "s9", ParentID: "s8", Service: "bank",
			Operation: "processPayment", StartTime: now.Add(260 * time.Millisecond), Duration: 150 * time.Millisecond},

		// inventory: 재고 차감 (payment보다 빨리 끝남)
		{TraceID: "t5", SpanID: "s10", ParentID: "s6", Service: "inventory",
			Operation: "reserveStock", StartTime: now.Add(240 * time.Millisecond), Duration: 60 * time.Millisecond},

		// notification: 알림 전송 (주문 생성 후)
		{TraceID: "t5", SpanID: "s11", ParentID: "s3", Service: "notification",
			Operation: "sendNotif", StartTime: now.Add(530 * time.Millisecond), Duration: 100 * time.Millisecond},

		// email: 이메일 발송
		{TraceID: "t5", SpanID: "s12", ParentID: "s11", Service: "email",
			Operation: "deliver", StartTime: now.Add(540 * time.Millisecond), Duration: 70 * time.Millisecond},
	}

	result := AnalyzeCriticalPath(spans)
	VisualizeTrace(result)

	// 크리티컬 패스 경로 출력
	fmt.Println("\n  === 크리티컬 패스 경로 ===")
	fmt.Print("  ")
	for i, seg := range result.Segments {
		if i > 0 {
			fmt.Print(" -> ")
		}
		fmt.Printf("%s(%s)", seg.Span.Operation, seg.Span.Service)
	}
	fmt.Println()

	fmt.Println("\n  === 최적화 제안 ===")
	fmt.Println("  크리티컬 패스 상에서 self-time이 가장 큰 스팬을 최적화하면")
	fmt.Println("  전체 트레이스 시간을 줄일 수 있다.")
	fmt.Println()

	// self-time 기준 정렬
	sorted := make([]CriticalPathSegment, len(result.Segments))
	copy(sorted, result.Segments)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].SelfTime > sorted[j].SelfTime
	})

	for i, seg := range sorted {
		pct := float64(seg.SelfTime) / float64(result.TotalDuration) * 100
		fmt.Printf("  %d. %s (%s): %v (%.1f%% of total)\n",
			i+1, seg.Span.Operation, seg.Span.Service, seg.SelfTime, pct)
	}
}

// 테스트 6: 깊게 중첩된 직렬 체인
func testDeepSerialChain() {
	fmt.Println("\n\n########################################################")
	fmt.Println("# 테스트 6: 깊게 중첩된 직렬 체인 (7단계)")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("구조: gateway -> auth -> api -> cache -> db -> logger -> audit")
	fmt.Println("  모든 호출이 순차적으로 발생하는 최악의 경우")

	now := time.Now()
	spans := []Span{
		{TraceID: "t6", SpanID: "d1", ParentID: "", Service: "gateway",
			Operation: "handleRequest", StartTime: now, Duration: 700 * time.Millisecond},
		{TraceID: "t6", SpanID: "d2", ParentID: "d1", Service: "auth",
			Operation: "authenticate", StartTime: now.Add(10 * time.Millisecond), Duration: 600 * time.Millisecond},
		{TraceID: "t6", SpanID: "d3", ParentID: "d2", Service: "api",
			Operation: "processAPI", StartTime: now.Add(30 * time.Millisecond), Duration: 500 * time.Millisecond},
		{TraceID: "t6", SpanID: "d4", ParentID: "d3", Service: "cache",
			Operation: "lookupCache", StartTime: now.Add(50 * time.Millisecond), Duration: 400 * time.Millisecond},
		{TraceID: "t6", SpanID: "d5", ParentID: "d4", Service: "database",
			Operation: "queryDB", StartTime: now.Add(80 * time.Millisecond), Duration: 300 * time.Millisecond},
		{TraceID: "t6", SpanID: "d6", ParentID: "d5", Service: "logger",
			Operation: "logAccess", StartTime: now.Add(120 * time.Millisecond), Duration: 200 * time.Millisecond},
		{TraceID: "t6", SpanID: "d7", ParentID: "d6", Service: "audit",
			Operation: "recordAudit", StartTime: now.Add(160 * time.Millisecond), Duration: 100 * time.Millisecond},
	}

	result := AnalyzeCriticalPath(spans)
	VisualizeTrace(result)
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("================================================================")
	fmt.Println(" Jaeger 크리티컬 패스 분석 시뮬레이터")
	fmt.Println("================================================================")
	fmt.Println()
	fmt.Println("크리티컬 패스(Critical Path)란:")
	fmt.Println("  트레이스의 전체 실행 시간에 직접적으로 기여하는 스팬들의 경로.")
	fmt.Println("  이 경로 위의 어떤 스팬이라도 느려지면 전체 트레이스 시간이 늘어난다.")
	fmt.Println("  반대로, 비크리티컬 스팬이 느려져도 전체 시간에 영향이 없다.")
	fmt.Println()
	fmt.Println("알고리즘 핵심:")
	fmt.Println("  1. findLastFinishingChildSpan: 가장 늦게 끝나는 자식 선택")
	fmt.Println("  2. sanitizeOverFlowingChildren: 부모 범위 초과 자식 보정")
	fmt.Println("  3. computeCriticalPath: 재귀적 크리티컬 패스 계산")
	fmt.Println("  4. Self-time: 자식에게 위임하지 않고 직접 사용한 시간")

	// 테스트 실행
	testSerialExecution()
	testParallelExecution()
	testNestedParallelism()
	testOverflowingChildren()
	testRealisticTrace()
	testDeepSerialChain()

	// 전체 요약
	fmt.Println("\n\n================================================================")
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println("================================================================")
	fmt.Println()
	fmt.Println("=== Jaeger 크리티컬 패스 분석 핵심 설계 포인트 ===")
	fmt.Println()
	fmt.Println("1. 가장 늦게 끝나는 자식 선택 (findLastFinishingChildSpan):")
	fmt.Println("   병렬로 실행되는 자식 중 가장 늦게 끝나는 자식이")
	fmt.Println("   부모의 완료 시간을 결정하므로 크리티컬 패스에 속한다.")
	fmt.Println()
	fmt.Println("2. 오버플로우 보정 (sanitizeOverFlowingChildren):")
	fmt.Println("   실제 환경에서 클록 스큐 등으로 자식이 부모 범위를")
	fmt.Println("   벗어나는 경우가 있다. 이를 보정해야 정확한 분석이 가능하다.")
	fmt.Println()
	fmt.Println("3. Self-time 계산:")
	fmt.Println("   스팬의 총 duration에서 크리티컬 자식이 커버하는 시간을 빼면")
	fmt.Println("   해당 스팬이 직접 실행한 시간(self-time)이 나온다.")
	fmt.Println("   이 값이 큰 스팬을 최적화해야 전체 지연시간이 줄어든다.")
	fmt.Println()
	fmt.Println("4. 활용:")
	fmt.Println("   - 성능 병목 식별: self-time이 큰 스팬이 진짜 병목")
	fmt.Println("   - 최적화 우선순위: 비크리티컬 스팬 최적화는 효과 없음")
	fmt.Println("   - 병렬화 기회: 크리티컬 패스를 분할/병렬화할 수 있는지 검토")
}
