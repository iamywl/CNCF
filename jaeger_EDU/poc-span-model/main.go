package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Jaeger Span/Trace 데이터 모델 시뮬레이션
// =============================================================================
// Jaeger의 UI 모델(internal/uimodel/model.go)을 기반으로 핵심 데이터 구조를 재현한다.
// 실제 Jaeger에서 Span은 분산 시스템의 단일 작업 단위를 표현하며,
// Trace는 하나의 요청이 여러 서비스를 거치면서 생성하는 Span들의 집합이다.
//
// 핵심 개념:
// - TraceID: 전체 요청 흐름을 식별하는 고유 ID (128비트)
// - SpanID:  개별 작업 단위를 식별하는 고유 ID (64비트)
// - Reference: Span 간의 관계 (CHILD_OF, FOLLOWS_FROM)
// - Process: Span을 생성한 서비스 정보
// =============================================================================

// ReferenceType은 Span 간의 참조 관계 유형이다.
// Jaeger는 OpenTracing 명세의 두 가지 참조 유형을 지원한다.
type ReferenceType string

const (
	// ChildOf는 부모 Span이 자식 Span의 완료에 의존하는 관계이다.
	// 예: HTTP 핸들러가 DB 쿼리를 호출하는 경우
	ChildOf ReferenceType = "CHILD_OF"

	// FollowsFrom은 부모 Span이 자식 Span의 완료에 의존하지 않는 관계이다.
	// 예: 메시지 큐에 메시지를 발행한 후, 소비자가 비동기로 처리하는 경우
	FollowsFrom ReferenceType = "FOLLOWS_FROM"
)

// ValueType은 KeyValue에 저장되는 값의 타입이다.
// Jaeger는 5가지 타입을 지원한다: string, bool, int64, float64, binary
type ValueType string

const (
	StringType  ValueType = "string"
	BoolType    ValueType = "bool"
	Int64Type   ValueType = "int64"
	Float64Type ValueType = "float64"
	BinaryType  ValueType = "binary"
)

// KeyValue는 태그나 로그 필드를 표현하는 키-값 쌍이다.
// Jaeger UI 모델의 KeyValue 구조체를 재현한다.
type KeyValue struct {
	Key   string    `json:"key"`
	Type  ValueType `json:"type,omitempty"`
	Value any       `json:"value"`
}

// Reference는 한 Span에서 다른 Span으로의 참조를 표현한다.
// 실제 Jaeger 소스: internal/uimodel/model.go의 Reference 구조체
type Reference struct {
	RefType ReferenceType `json:"refType"`
	TraceID string        `json:"traceID"`
	SpanID  string        `json:"spanID"`
}

// Log는 Span 실행 중 특정 시점에 기록되는 이벤트이다.
// 타임스탬프와 여러 키-값 필드로 구성된다.
type Log struct {
	Timestamp time.Time  `json:"timestamp"`
	Fields    []KeyValue `json:"fields"`
}

// Process는 Span을 생성한 서비스(프로세스) 정보이다.
// 서비스 이름과 메타데이터 태그를 포함한다.
type Process struct {
	ServiceName string     `json:"serviceName"`
	Tags        []KeyValue `json:"tags"`
}

// Span은 분산 시스템에서 단일 작업 단위를 표현하는 핵심 데이터 구조이다.
// Jaeger UI 모델의 Span 구조체를 충실히 재현한다.
//
// 실제 Jaeger에서 Span은 다음 정보를 포함한다:
// - 식별 정보: TraceID, SpanID, ParentSpanID
// - 작업 정보: OperationName, StartTime, Duration
// - 관계 정보: References (다른 Span과의 관계)
// - 메타데이터: Tags (키-값 쌍), Logs (시간 기반 이벤트)
// - 프로세스: 이 Span을 생성한 서비스 정보
type Span struct {
	TraceID       string      `json:"traceID"`
	SpanID        string      `json:"spanID"`
	ParentSpanID  string      `json:"parentSpanID,omitempty"`
	OperationName string      `json:"operationName"`
	References    []Reference `json:"references"`
	StartTime     time.Time   `json:"startTime"`
	Duration      time.Duration `json:"duration"`
	Tags          []KeyValue  `json:"tags"`
	Logs          []Log       `json:"logs"`
	Process       *Process    `json:"process"`
	Warnings      []string    `json:"warnings,omitempty"`
}

// Trace는 하나의 분산 요청 전체를 표현하는 Span들의 집합이다.
// 동일한 TraceID를 공유하는 모든 Span이 하나의 Trace를 구성한다.
type Trace struct {
	TraceID  string `json:"traceID"`
	Spans    []Span `json:"spans"`
	Warnings []string `json:"warnings,omitempty"`
}

// =============================================================================
// ID 생성 유틸리티
// =============================================================================

// generateTraceID는 128비트 TraceID를 16진수 문자열로 생성한다.
// 실제 Jaeger에서는 pcommon.TraceID (16바이트 배열)를 사용한다.
func generateTraceID() string {
	return fmt.Sprintf("%016x%016x", rand.Int63(), rand.Int63())
}

// generateSpanID는 64비트 SpanID를 16진수 문자열로 생성한다.
// 실제 Jaeger에서는 pcommon.SpanID (8바이트 배열)를 사용한다.
func generateSpanID() string {
	return fmt.Sprintf("%016x", rand.Int63())
}

// =============================================================================
// Span 생성 헬퍼
// =============================================================================

// newSpan은 새로운 Span을 생성한다.
func newSpan(traceID, parentSpanID, operationName, serviceName string,
	startTime time.Time, duration time.Duration,
	refType ReferenceType, tags []KeyValue, logs []Log) Span {

	spanID := generateSpanID()
	var refs []Reference
	if parentSpanID != "" {
		refs = append(refs, Reference{
			RefType: refType,
			TraceID: traceID,
			SpanID:  parentSpanID,
		})
	}

	return Span{
		TraceID:       traceID,
		SpanID:        spanID,
		ParentSpanID:  parentSpanID,
		OperationName: operationName,
		References:    refs,
		StartTime:     startTime,
		Duration:      duration,
		Tags:          tags,
		Logs:          logs,
		Process: &Process{
			ServiceName: serviceName,
			Tags: []KeyValue{
				{Key: "hostname", Type: StringType, Value: serviceName + "-host-01"},
				{Key: "ip", Type: StringType, Value: "10.0.0." + fmt.Sprintf("%d", rand.Intn(254)+1)},
			},
		},
	}
}

// =============================================================================
// Trace 분석 함수들
// =============================================================================

// calculateTraceDuration은 Trace의 전체 소요 시간을 계산한다.
// 가장 이른 시작 시간부터 가장 늦은 종료 시간까지의 차이를 반환한다.
func calculateTraceDuration(trace *Trace) time.Duration {
	if len(trace.Spans) == 0 {
		return 0
	}
	minStart := trace.Spans[0].StartTime
	maxEnd := trace.Spans[0].StartTime.Add(trace.Spans[0].Duration)

	for _, span := range trace.Spans[1:] {
		if span.StartTime.Before(minStart) {
			minStart = span.StartTime
		}
		end := span.StartTime.Add(span.Duration)
		if end.After(maxEnd) {
			maxEnd = end
		}
	}
	return maxEnd.Sub(minStart)
}

// getServiceCount는 Trace에 참여하는 고유 서비스 수를 반환한다.
func getServiceCount(trace *Trace) int {
	services := make(map[string]struct{})
	for _, span := range trace.Spans {
		if span.Process != nil {
			services[span.Process.ServiceName] = struct{}{}
		}
	}
	return len(services)
}

// getServiceNames는 Trace에 참여하는 모든 서비스 이름을 반환한다.
func getServiceNames(trace *Trace) []string {
	services := make(map[string]struct{})
	for _, span := range trace.Spans {
		if span.Process != nil {
			services[span.Process.ServiceName] = struct{}{}
		}
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// getSpanDepth는 특정 Span의 깊이(루트에서의 거리)를 반환한다.
func getSpanDepth(trace *Trace, spanID string) int {
	spanMap := make(map[string]*Span)
	for i := range trace.Spans {
		spanMap[trace.Spans[i].SpanID] = &trace.Spans[i]
	}

	depth := 0
	current := spanMap[spanID]
	for current != nil && current.ParentSpanID != "" {
		depth++
		current = spanMap[current.ParentSpanID]
		if depth > len(trace.Spans) {
			break // 순환 참조 방지
		}
	}
	return depth
}

// =============================================================================
// Trace 시각화
// =============================================================================

// printTraceTree는 Trace를 시각적 트리 형태로 출력한다.
// Jaeger UI의 타임라인 뷰를 ASCII 아트로 재현한다.
func printTraceTree(trace *Trace) {
	if len(trace.Spans) == 0 {
		fmt.Println("  (비어있는 트레이스)")
		return
	}

	// 부모-자식 관계 맵 구성
	childMap := make(map[string][]int) // parentSpanID → 자식 Span 인덱스 목록
	rootIndices := []int{}

	for i, span := range trace.Spans {
		if span.ParentSpanID == "" {
			rootIndices = append(rootIndices, i)
		} else {
			childMap[span.ParentSpanID] = append(childMap[span.ParentSpanID], i)
		}
	}

	// Trace 전체 시간 범위 계산 (타임라인 바 그리기용)
	traceDuration := calculateTraceDuration(trace)
	traceStart := trace.Spans[0].StartTime
	for _, span := range trace.Spans {
		if span.StartTime.Before(traceStart) {
			traceStart = span.StartTime
		}
	}

	// 재귀적으로 트리 출력
	var printNode func(spanIdx int, prefix string, isLast bool)
	printNode = func(spanIdx int, prefix string, isLast bool) {
		span := trace.Spans[spanIdx]

		// 트리 연결선
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		// 참조 유형 표시
		refType := ""
		if len(span.References) > 0 {
			refType = fmt.Sprintf(" [%s]", span.References[0].RefType)
		}

		// 타임라인 바 생성
		bar := generateTimelineBar(span.StartTime, span.Duration, traceStart, traceDuration, 30)

		// Span 정보 출력
		serviceName := "unknown"
		if span.Process != nil {
			serviceName = span.Process.ServiceName
		}
		fmt.Printf("%s%s%s :: %s %s  (%s)%s\n",
			prefix, connector,
			serviceName,
			span.OperationName,
			bar,
			span.Duration,
			refType,
		)

		// 태그 출력
		if len(span.Tags) > 0 {
			childPrefix := prefix + "│   "
			if isLast {
				childPrefix = prefix + "    "
			}
			tagStrs := []string{}
			for _, tag := range span.Tags {
				tagStrs = append(tagStrs, fmt.Sprintf("%s=%v", tag.Key, tag.Value))
			}
			fmt.Printf("%s    태그: {%s}\n", childPrefix, strings.Join(tagStrs, ", "))
		}

		// 로그 출력
		if len(span.Logs) > 0 {
			childPrefix := prefix + "│   "
			if isLast {
				childPrefix = prefix + "    "
			}
			for _, log := range span.Logs {
				fields := []string{}
				for _, f := range log.Fields {
					fields = append(fields, fmt.Sprintf("%s=%v", f.Key, f.Value))
				}
				offset := log.Timestamp.Sub(span.StartTime)
				fmt.Printf("%s    로그 @+%s: {%s}\n", childPrefix, offset, strings.Join(fields, ", "))
			}
		}

		// 자식 Span 출력
		children := childMap[span.SpanID]
		// 시작 시간 순으로 정렬
		sort.Slice(children, func(i, j int) bool {
			return trace.Spans[children[i]].StartTime.Before(trace.Spans[children[j]].StartTime)
		})

		nextPrefix := prefix + "│   "
		if isLast {
			nextPrefix = prefix + "    "
		}
		for i, childIdx := range children {
			printNode(childIdx, nextPrefix, i == len(children)-1)
		}
	}

	for i, rootIdx := range rootIndices {
		printNode(rootIdx, "", i == len(rootIndices)-1)
	}
}

// generateTimelineBar는 Span의 시간 범위를 ASCII 타임라인 바로 표현한다.
func generateTimelineBar(startTime time.Time, duration time.Duration,
	traceStart time.Time, traceDuration time.Duration, width int) string {

	if traceDuration == 0 {
		return "|" + strings.Repeat("=", width) + "|"
	}

	// Span의 상대적 시작 위치와 길이 계산
	offset := float64(startTime.Sub(traceStart)) / float64(traceDuration)
	length := float64(duration) / float64(traceDuration)

	startPos := int(offset * float64(width))
	barLen := int(length * float64(width))
	if barLen < 1 {
		barLen = 1
	}
	if startPos+barLen > width {
		barLen = width - startPos
	}
	if startPos >= width {
		startPos = width - 1
		barLen = 1
	}

	bar := strings.Repeat(" ", startPos) +
		strings.Repeat("=", barLen) +
		strings.Repeat(" ", width-startPos-barLen)

	return "|" + bar + "|"
}

// =============================================================================
// 샘플 Trace 생성
// =============================================================================

// buildSampleTrace는 전자상거래 시나리오의 샘플 Trace를 생성한다.
//
//	frontend (HTTP GET /checkout)
//	├── cart-service (gRPC GetCart)
//	│   └── cart-service (Redis GET)       [CHILD_OF]
//	├── payment-service (gRPC ProcessPayment)   [CHILD_OF]
//	│   ├── payment-service (validate-card)     [CHILD_OF]
//	│   └── payment-gateway (HTTP POST /charge) [CHILD_OF]
//	└── notification-service (async SendEmail)  [FOLLOWS_FROM]
func buildSampleTrace() *Trace {
	traceID := generateTraceID()
	baseTime := time.Now().Add(-5 * time.Second)

	// 루트 Span: frontend
	rootSpan := newSpan(traceID, "",
		"HTTP GET /checkout", "frontend",
		baseTime, 450*time.Millisecond,
		"", // 루트 Span에는 참조 없음
		[]KeyValue{
			{Key: "http.method", Type: StringType, Value: "GET"},
			{Key: "http.url", Type: StringType, Value: "/checkout"},
			{Key: "http.status_code", Type: Int64Type, Value: 200},
			{Key: "span.kind", Type: StringType, Value: "server"},
		},
		[]Log{
			{
				Timestamp: baseTime.Add(1 * time.Millisecond),
				Fields: []KeyValue{
					{Key: "event", Type: StringType, Value: "checkout_started"},
					{Key: "user_id", Type: StringType, Value: "user-12345"},
				},
			},
		},
	)

	// cart-service: GetCart
	cartSpan := newSpan(traceID, rootSpan.SpanID,
		"gRPC GetCart", "cart-service",
		baseTime.Add(5*time.Millisecond), 80*time.Millisecond,
		ChildOf,
		[]KeyValue{
			{Key: "rpc.system", Type: StringType, Value: "grpc"},
			{Key: "rpc.method", Type: StringType, Value: "GetCart"},
			{Key: "cart.items_count", Type: Int64Type, Value: 3},
		},
		nil,
	)

	// cart-service 내부: Redis GET
	redisSpan := newSpan(traceID, cartSpan.SpanID,
		"Redis GET cart:user-12345", "cart-service",
		baseTime.Add(10*time.Millisecond), 15*time.Millisecond,
		ChildOf,
		[]KeyValue{
			{Key: "db.system", Type: StringType, Value: "redis"},
			{Key: "db.operation", Type: StringType, Value: "GET"},
			{Key: "db.statement", Type: StringType, Value: "GET cart:user-12345"},
			{Key: "span.kind", Type: StringType, Value: "client"},
		},
		nil,
	)

	// payment-service: ProcessPayment
	paymentSpan := newSpan(traceID, rootSpan.SpanID,
		"gRPC ProcessPayment", "payment-service",
		baseTime.Add(90*time.Millisecond), 300*time.Millisecond,
		ChildOf,
		[]KeyValue{
			{Key: "rpc.system", Type: StringType, Value: "grpc"},
			{Key: "rpc.method", Type: StringType, Value: "ProcessPayment"},
			{Key: "payment.amount", Type: Float64Type, Value: 99.99},
			{Key: "payment.currency", Type: StringType, Value: "USD"},
		},
		[]Log{
			{
				Timestamp: baseTime.Add(95 * time.Millisecond),
				Fields: []KeyValue{
					{Key: "event", Type: StringType, Value: "payment_initiated"},
					{Key: "amount", Type: Float64Type, Value: 99.99},
				},
			},
		},
	)

	// payment-service 내부: 카드 검증
	validateSpan := newSpan(traceID, paymentSpan.SpanID,
		"validate-card", "payment-service",
		baseTime.Add(95*time.Millisecond), 50*time.Millisecond,
		ChildOf,
		[]KeyValue{
			{Key: "card.type", Type: StringType, Value: "visa"},
			{Key: "card.last4", Type: StringType, Value: "4242"},
			{Key: "validation.result", Type: BoolType, Value: true},
		},
		nil,
	)

	// payment-gateway: 외부 결제 처리
	gatewaySpan := newSpan(traceID, paymentSpan.SpanID,
		"HTTP POST /charge", "payment-gateway",
		baseTime.Add(150*time.Millisecond), 200*time.Millisecond,
		ChildOf,
		[]KeyValue{
			{Key: "http.method", Type: StringType, Value: "POST"},
			{Key: "http.url", Type: StringType, Value: "https://gateway.example.com/charge"},
			{Key: "http.status_code", Type: Int64Type, Value: 200},
			{Key: "span.kind", Type: StringType, Value: "client"},
			{Key: "peer.service", Type: StringType, Value: "stripe-api"},
		},
		[]Log{
			{
				Timestamp: baseTime.Add(340 * time.Millisecond),
				Fields: []KeyValue{
					{Key: "event", Type: StringType, Value: "charge_completed"},
					{Key: "transaction_id", Type: StringType, Value: "txn-abc-123"},
				},
			},
		},
	)

	// notification-service: 비동기 이메일 발송 (FOLLOWS_FROM)
	notifSpan := newSpan(traceID, rootSpan.SpanID,
		"async SendEmail", "notification-service",
		baseTime.Add(400*time.Millisecond), 45*time.Millisecond,
		FollowsFrom,
		[]KeyValue{
			{Key: "message.type", Type: StringType, Value: "email"},
			{Key: "message.destination", Type: StringType, Value: "user@example.com"},
			{Key: "span.kind", Type: StringType, Value: "producer"},
		},
		[]Log{
			{
				Timestamp: baseTime.Add(440 * time.Millisecond),
				Fields: []KeyValue{
					{Key: "event", Type: StringType, Value: "email_queued"},
				},
			},
		},
	)

	return &Trace{
		TraceID: traceID,
		Spans: []Span{
			rootSpan, cartSpan, redisSpan,
			paymentSpan, validateSpan, gatewaySpan,
			notifSpan,
		},
	}
}

// =============================================================================
// 참조 유형 비교 데모
// =============================================================================

// demonstrateReferences는 CHILD_OF와 FOLLOWS_FROM 참조의 차이를 시연한다.
func demonstrateReferences() {
	fmt.Println("=== Span 참조 유형 비교 ===")
	fmt.Println()
	fmt.Println("1. CHILD_OF (동기적 의존 관계)")
	fmt.Println("   ┌─────────────────────────────────┐")
	fmt.Println("   │ 부모 Span                        │")
	fmt.Println("   │  ┌──────────────┐                │")
	fmt.Println("   │  │ 자식 Span     │ ← 부모는 자식  │")
	fmt.Println("   │  │  (CHILD_OF)  │   완료를 대기   │")
	fmt.Println("   │  └──────────────┘                │")
	fmt.Println("   └─────────────────────────────────┘")
	fmt.Println()
	fmt.Println("   예시: HTTP 핸들러 → DB 쿼리")
	fmt.Println("   - 부모(핸들러)는 자식(DB 쿼리) 결과가 필요")
	fmt.Println("   - 자식이 끝나야 부모도 끝남")
	fmt.Println()
	fmt.Println("2. FOLLOWS_FROM (비동기적 인과 관계)")
	fmt.Println("   ┌────────────────┐")
	fmt.Println("   │ 선행 Span       │")
	fmt.Println("   └────────────────┘")
	fmt.Println("          ↓ (fire-and-forget)")
	fmt.Println("          ┌──────────────────┐")
	fmt.Println("          │ 후행 Span          │")
	fmt.Println("          │  (FOLLOWS_FROM)   │")
	fmt.Println("          └──────────────────┘")
	fmt.Println()
	fmt.Println("   예시: 결제 완료 → 이메일 알림 발송")
	fmt.Println("   - 선행 Span이 후행 Span을 유발하지만 결과를 기다리지 않음")
	fmt.Println("   - 두 Span은 독립적으로 완료됨")
	fmt.Println()
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jaeger Span/Trace 데이터 모델 시뮬레이션                      ║")
	fmt.Println("║  (internal/uimodel/model.go 기반)                           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// ─── 1. 참조 유형 설명 ───
	demonstrateReferences()

	// ─── 2. 샘플 Trace 생성 ───
	fmt.Println("=== 샘플 Trace 생성: 전자상거래 체크아웃 흐름 ===")
	fmt.Println()

	trace := buildSampleTrace()

	fmt.Printf("TraceID: %s\n", trace.TraceID)
	fmt.Printf("총 Span 수: %d\n", len(trace.Spans))
	fmt.Printf("참여 서비스 수: %d\n", getServiceCount(trace))
	fmt.Printf("참여 서비스: %s\n", strings.Join(getServiceNames(trace), ", "))
	fmt.Printf("Trace 전체 소요 시간: %s\n", calculateTraceDuration(trace))
	fmt.Println()

	// ─── 3. Trace 트리 시각화 ───
	fmt.Println("=== Trace 트리 시각화 ===")
	fmt.Println()
	fmt.Println("서비스 :: 오퍼레이션  |타임라인 바|  (소요시간) [참조유형]")
	fmt.Println(strings.Repeat("-", 90))
	printTraceTree(trace)
	fmt.Println()

	// ─── 4. 개별 Span 상세 정보 ───
	fmt.Println("=== 개별 Span 상세 정보 ===")
	fmt.Println()
	for i, span := range trace.Spans {
		depth := getSpanDepth(trace, span.SpanID)
		indent := strings.Repeat("  ", depth)

		fmt.Printf("Span #%d %s[깊이:%d]\n", i+1, indent, depth)
		fmt.Printf("  SpanID:        %s\n", span.SpanID)
		if span.ParentSpanID != "" {
			fmt.Printf("  ParentSpanID:  %s\n", span.ParentSpanID)
		} else {
			fmt.Printf("  ParentSpanID:  (루트 Span)\n")
		}
		fmt.Printf("  서비스:         %s\n", span.Process.ServiceName)
		fmt.Printf("  오퍼레이션:     %s\n", span.OperationName)
		fmt.Printf("  시작 시간:      %s\n", span.StartTime.Format("15:04:05.000"))
		fmt.Printf("  소요 시간:      %s\n", span.Duration)
		if len(span.References) > 0 {
			for _, ref := range span.References {
				fmt.Printf("  참조:          %s → SpanID:%s...%s\n",
					ref.RefType, ref.SpanID[:8], ref.SpanID[len(ref.SpanID)-4:])
			}
		}
		if len(span.Tags) > 0 {
			fmt.Printf("  태그 (%d개):\n", len(span.Tags))
			for _, tag := range span.Tags {
				fmt.Printf("    %s [%s] = %v\n", tag.Key, tag.Type, tag.Value)
			}
		}
		if len(span.Logs) > 0 {
			fmt.Printf("  로그 (%d개):\n", len(span.Logs))
			for _, log := range span.Logs {
				offset := log.Timestamp.Sub(span.StartTime)
				fmt.Printf("    @+%s:", offset)
				for _, f := range log.Fields {
					fmt.Printf(" %s=%v", f.Key, f.Value)
				}
				fmt.Println()
			}
		}
		fmt.Println()
	}

	// ─── 5. Trace 통계 요약 ───
	fmt.Println("=== Trace 통계 요약 ===")
	fmt.Println()

	// 서비스별 Span 수
	serviceCounts := make(map[string]int)
	serviceDurations := make(map[string]time.Duration)
	refTypeCounts := make(map[ReferenceType]int)
	tagCount := 0
	logCount := 0

	for _, span := range trace.Spans {
		svc := span.Process.ServiceName
		serviceCounts[svc]++
		serviceDurations[svc] += span.Duration
		tagCount += len(span.Tags)
		logCount += len(span.Logs)
		for _, ref := range span.References {
			refTypeCounts[ref.RefType]++
		}
	}

	fmt.Println("서비스별 통계:")
	fmt.Printf("  %-25s %-10s %-15s\n", "서비스", "Span 수", "총 소요시간")
	fmt.Printf("  %-25s %-10s %-15s\n", strings.Repeat("-", 25), strings.Repeat("-", 10), strings.Repeat("-", 15))
	for _, svc := range getServiceNames(trace) {
		fmt.Printf("  %-25s %-10d %-15s\n", svc, serviceCounts[svc], serviceDurations[svc])
	}

	fmt.Println()
	fmt.Println("참조 유형별 통계:")
	fmt.Printf("  CHILD_OF:     %d개\n", refTypeCounts[ChildOf])
	fmt.Printf("  FOLLOWS_FROM: %d개\n", refTypeCounts[FollowsFrom])
	fmt.Printf("  (루트 Span):  1개\n")
	fmt.Println()
	fmt.Printf("총 태그 수:  %d개\n", tagCount)
	fmt.Printf("총 로그 수:  %d개\n", logCount)
}
