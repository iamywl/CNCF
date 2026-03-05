package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// PoC-16: 스팬 기반 분산 추적(텔레메트리) 시뮬레이션
// =============================================================================
// Tart의 Root.swift(OpenTelemetry 부분)와 OTel.swift를 Go로 재현한다.
// 핵심 개념:
//   - 루트 스팬: 명령 실행 단위 (커맨드 이름으로 생성)
//   - 자식 스팬: 내부 작업 단위 (pull, prune 등)
//   - 속성(Attributes): 키-값 쌍으로 스팬에 메타데이터 추가
//   - 이벤트(Events): 스팬 내에서 발생한 주요 사건 기록
//   - 에러 캡처: recordException()으로 에러를 스팬에 기록
//   - OTLP 형식: OpenTelemetry Protocol 출력 시뮬레이션
//   - TRACEPARENT: W3C Trace Context 전파
//
// 실제 소스:
//   - Sources/tart/Root.swift: 루트 스팬 생성, 에러 캡처
//   - Sources/tart/OTel.swift: TracerProvider 초기화, OTLP 내보내기
// =============================================================================

// ---------------------------------------------------------------------------
// 1. 스팬 모델 — OpenTelemetry Span
// ---------------------------------------------------------------------------

// SpanStatus는 스팬의 상태를 나타낸다.
type SpanStatus int

const (
	StatusUnset SpanStatus = iota
	StatusOK
	StatusError
)

func (s SpanStatus) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusError:
		return "ERROR"
	default:
		return "UNSET"
	}
}

// AttributeValue는 속성값을 나타낸다.
// OpenTelemetryApi의 AttributeValue에 대응:
//   .string(String), .int(Int), .bool(Bool), .array(AttributeArray)
type AttributeValue struct {
	Type  string      `json:"type"` // "string", "int", "bool", "string_array"
	Value interface{} `json:"value"`
}

func StringAttr(v string) AttributeValue {
	return AttributeValue{Type: "string", Value: v}
}
func IntAttr(v int) AttributeValue {
	return AttributeValue{Type: "int", Value: v}
}
func BoolAttr(v bool) AttributeValue {
	return AttributeValue{Type: "bool", Value: v}
}
func StringArrayAttr(v []string) AttributeValue {
	return AttributeValue{Type: "string_array", Value: v}
}

// SpanEvent는 스팬 내에서 발생한 이벤트를 나타낸다.
// OpenTelemetry: span.addEvent(name: "...")
type SpanEvent struct {
	Name       string                 `json:"name"`
	Timestamp  time.Time              `json:"timestamp"`
	Attributes map[string]AttributeValue `json:"attributes,omitempty"`
}

// Span은 하나의 추적 단위를 나타낸다.
// Root.swift에서 루트 스팬 생성:
//   let span = OTel.shared.tracer.spanBuilder(spanName: type(of: command)._commandName).startSpan()
//   OpenTelemetry.instance.contextProvider.setActiveSpan(span)
type Span struct {
	TraceID    string                    `json:"trace_id"`
	SpanID     string                    `json:"span_id"`
	ParentID   string                    `json:"parent_span_id,omitempty"`
	Name       string                    `json:"name"`
	Kind       string                    `json:"kind"` // "INTERNAL", "CLIENT", "SERVER"
	StartTime  time.Time                 `json:"start_time"`
	EndTime    time.Time                 `json:"end_time,omitempty"`
	Status     SpanStatus                `json:"status"`
	Attributes map[string]AttributeValue `json:"attributes,omitempty"`
	Events     []SpanEvent               `json:"events,omitempty"`
	Children   []*Span                   `json:"-"`

	mu      sync.Mutex
	ended   bool
}

// SetAttribute는 스팬에 속성을 추가한다.
// Root.swift: span.setAttribute(key: "Command-line arguments", value: .array(...))
func (s *Span) SetAttribute(key string, value AttributeValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Attributes == nil {
		s.Attributes = make(map[string]AttributeValue)
	}
	s.Attributes[key] = value
}

// SetAttributes는 여러 속성을 한 번에 추가한다.
// Prune.swift: span?.setAttributes(["key1": .int(v1), "key2": .int(v2)])
func (s *Span) SetAttributes(attrs map[string]AttributeValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Attributes == nil {
		s.Attributes = make(map[string]AttributeValue)
	}
	for k, v := range attrs {
		s.Attributes[k] = v
	}
}

// AddEvent는 스팬에 이벤트를 추가한다.
// Prune.swift: span?.addEvent(name: "Pruned N bytes for path")
func (s *Span) AddEvent(name string, attrs ...map[string]AttributeValue) {
	s.mu.Lock()
	defer s.mu.Unlock()

	event := SpanEvent{
		Name:      name,
		Timestamp: time.Now(),
	}
	if len(attrs) > 0 {
		event.Attributes = attrs[0]
	}
	s.Events = append(s.Events, event)
}

// RecordException는 에러를 스팬에 기록한다.
// Root.swift: OpenTelemetry.instance.contextProvider.activeSpan?.recordException(error)
func (s *Span) RecordException(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Status = StatusError

	event := SpanEvent{
		Name:      "exception",
		Timestamp: time.Now(),
		Attributes: map[string]AttributeValue{
			"exception.type":    StringAttr(fmt.Sprintf("%T", err)),
			"exception.message": StringAttr(err.Error()),
		},
	}
	s.Events = append(s.Events, event)
}

// End는 스팬을 종료한다.
// Root.swift: defer { span.end() }
func (s *Span) End() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.ended {
		s.EndTime = time.Now()
		s.ended = true
	}
}

// Duration은 스팬의 지속 시간을 반환한다.
func (s *Span) Duration() time.Duration {
	if s.EndTime.IsZero() {
		return time.Since(s.StartTime)
	}
	return s.EndTime.Sub(s.StartTime)
}

// ---------------------------------------------------------------------------
// 2. Tracer — OpenTelemetry Tracer
// ---------------------------------------------------------------------------

var spanIDCounter atomic.Int64

func generateID() string {
	id := spanIDCounter.Add(1)
	return fmt.Sprintf("%016x", id+rand.Int63n(1000000))
}

// Tracer는 스팬을 생성하는 추적기이다.
// OTel.swift: tracer = OpenTelemetry.instance.tracerProvider.get(
//   instrumentationName: "tart", instrumentationVersion: CI.version)
type Tracer struct {
	InstrumentationName    string
	InstrumentationVersion string
}

// SpanBuilder는 스팬 빌더를 시뮬레이션한다.
// OTel.swift: OTel.shared.tracer.spanBuilder(spanName: "pull").setActive(true).startSpan()
type SpanBuilder struct {
	tracer   *Tracer
	spanName string
	traceID  string
	parentID string
	active   bool
}

func (t *Tracer) SpanBuilder(spanName string) *SpanBuilder {
	return &SpanBuilder{
		tracer:   t,
		spanName: spanName,
		traceID:  generateID(),
	}
}

func (sb *SpanBuilder) SetTraceID(traceID string) *SpanBuilder {
	sb.traceID = traceID
	return sb
}

func (sb *SpanBuilder) SetParentID(parentID string) *SpanBuilder {
	sb.parentID = parentID
	return sb
}

func (sb *SpanBuilder) SetActive(active bool) *SpanBuilder {
	sb.active = active
	return sb
}

func (sb *SpanBuilder) StartSpan() *Span {
	span := &Span{
		TraceID:    sb.traceID,
		SpanID:     generateID(),
		ParentID:   sb.parentID,
		Name:       sb.spanName,
		Kind:       "INTERNAL",
		StartTime:  time.Now(),
		Status:     StatusUnset,
		Attributes: make(map[string]AttributeValue),
	}
	return span
}

// ---------------------------------------------------------------------------
// 3. OTel 싱글톤 — Sources/tart/OTel.swift 참조
// ---------------------------------------------------------------------------

// OTelInstance는 Tart의 OTel 싱글톤을 시뮬레이션한다.
// OTel.swift:
//   class OTel {
//     let tracerProvider: TracerProviderSdk?
//     let tracer: Tracer
//     static let shared = OTel()
//     init() {
//       tracerProvider = Self.initializeTracing()
//       tracer = OpenTelemetry.instance.tracerProvider.get(
//         instrumentationName: "tart", instrumentationVersion: CI.version)
//     }
//   }
type OTelInstance struct {
	Tracer     *Tracer
	ActiveSpan *Span
	AllSpans   []*Span
	Enabled    bool // TRACEPARENT 환경변수 존재 시 활성화

	mu sync.Mutex
}

// NewOTel은 OTel 인스턴스를 초기화한다.
// OTel.swift initializeTracing():
//   guard let _ = ProcessInfo.processInfo.environment["TRACEPARENT"] else { return nil }
func NewOTel(version string) *OTelInstance {
	_, hasTraceParent := os.LookupEnv("TRACEPARENT")

	return &OTelInstance{
		Tracer: &Tracer{
			InstrumentationName:    "tart",
			InstrumentationVersion: version,
		},
		AllSpans: make([]*Span, 0),
		Enabled:  hasTraceParent,
	}
}

// StartRootSpan은 루트 스팬을 생성하고 활성 스팬으로 설정한다.
func (o *OTelInstance) StartRootSpan(name string) *Span {
	o.mu.Lock()
	defer o.mu.Unlock()

	span := o.Tracer.SpanBuilder(name).StartSpan()
	o.ActiveSpan = span
	o.AllSpans = append(o.AllSpans, span)
	return span
}

// StartChildSpan은 현재 활성 스팬의 자식 스팬을 생성한다.
func (o *OTelInstance) StartChildSpan(name string) *Span {
	o.mu.Lock()
	defer o.mu.Unlock()

	var parentID, traceID string
	if o.ActiveSpan != nil {
		parentID = o.ActiveSpan.SpanID
		traceID = o.ActiveSpan.TraceID
	} else {
		traceID = generateID()
	}

	span := o.Tracer.SpanBuilder(name).
		SetTraceID(traceID).
		SetParentID(parentID).
		StartSpan()

	if o.ActiveSpan != nil {
		o.ActiveSpan.Children = append(o.ActiveSpan.Children, span)
	}
	o.AllSpans = append(o.AllSpans, span)
	return span
}

// Flush는 모든 스팬을 내보내고 활성 스팬을 종료한다.
// OTel.swift:
//   func flush() {
//     OpenTelemetry.instance.contextProvider.activeSpan?.end()
//     tracerProvider?.forceFlush()
//     Thread.sleep(forTimeInterval: .fromMilliseconds(100))
//   }
func (o *OTelInstance) Flush() {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.ActiveSpan != nil && !o.ActiveSpan.ended {
		o.ActiveSpan.End()
	}
}

// ExportOTLP는 OTLP 형식으로 모든 스팬을 출력한다.
func (o *OTelInstance) ExportOTLP() {
	o.mu.Lock()
	defer o.mu.Unlock()

	fmt.Println("  OTLP Export:")
	fmt.Printf("  ┌─ Resource: service.name=tart, service.version=%s\n",
		o.Tracer.InstrumentationVersion)

	for _, span := range o.AllSpans {
		indent := "  │  "
		if span.ParentID != "" {
			indent = "  │  │  "
		}

		status := span.Status.String()
		fmt.Printf("%s┌─ Span: %s [%s] (%.2fms)\n",
			indent, span.Name, status, span.Duration().Seconds()*1000)
		fmt.Printf("%s│  trace_id: %s\n", indent, span.TraceID)
		fmt.Printf("%s│  span_id: %s\n", indent, span.SpanID)
		if span.ParentID != "" {
			fmt.Printf("%s│  parent_span_id: %s\n", indent, span.ParentID)
		}

		// 속성 출력
		for k, v := range span.Attributes {
			fmt.Printf("%s│  attr: %s = %v\n", indent, k, v.Value)
		}

		// 이벤트 출력
		for _, event := range span.Events {
			fmt.Printf("%s│  event: %s (at %s)\n",
				indent, event.Name, event.Timestamp.Format("15:04:05.000"))
			for ek, ev := range event.Attributes {
				fmt.Printf("%s│    %s = %v\n", indent, ek, ev.Value)
			}
		}

		fmt.Printf("%s└─\n", indent)
	}
}

// ---------------------------------------------------------------------------
// 4. 출력 헬퍼
// ---------------------------------------------------------------------------

func printSeparator(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// ---------------------------------------------------------------------------
// 5. 메인 함수 — Root.main()의 OpenTelemetry 흐름 재현
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("=== PoC-16: 스팬 기반 분산 추적(텔레메트리) 시뮬레이션 ===")
	fmt.Println("  소스: Root.swift (OpenTelemetry), OTel.swift")
	fmt.Println()

	otel := NewOTel("2.22.4")

	// =========================================================================
	// 데모 1: Root.main() — 루트 스팬 생성
	// =========================================================================
	printSeparator("데모 1: Root.main() -- 루트 스팬 생성")

	// Root.swift:
	//   var command = try parseAsRoot()
	//   let span = OTel.shared.tracer.spanBuilder(spanName: type(of: command)._commandName).startSpan()
	//   defer { span.end() }
	//   OpenTelemetry.instance.contextProvider.setActiveSpan(span)
	rootSpan := otel.StartRootSpan("clone")
	fmt.Printf("  루트 스팬 생성: name=%s, trace_id=%s\n", rootSpan.Name, rootSpan.TraceID)

	// 커맨드라인 인자를 속성으로 추가
	// Root.swift: span.setAttribute(key: "Command-line arguments", value: .array(...))
	cmdArgs := []string{"tart", "clone", "ghcr.io/cirruslabs/macos-sonoma:latest", "my-vm"}
	rootSpan.SetAttribute("Command-line arguments", StringArrayAttr(cmdArgs))
	fmt.Printf("  속성 추가: Command-line arguments = %v\n", cmdArgs)

	// Cirrus CI 태그
	// Root.swift: if let tags = ProcessInfo.processInfo.environment["CIRRUS_SENTRY_TAGS"] { ... }
	rootSpan.SetAttributes(map[string]AttributeValue{
		"ci.provider": StringAttr("cirrus"),
		"ci.build_id": StringAttr("12345"),
	})
	fmt.Println("  CI 태그 추가: ci.provider=cirrus, ci.build_id=12345")

	// =========================================================================
	// 데모 2: 자식 스팬 — pull 작업
	// =========================================================================
	printSeparator("데모 2: 자식 스팬 -- pull 작업")

	// VMStorageOCI.pull()에서 스팬 생성:
	// let span = OTel.shared.tracer.spanBuilder(spanName: "pull").setActive(true).startSpan()
	pullSpan := otel.StartChildSpan("pull")
	fmt.Printf("  자식 스팬 생성: name=%s, parent=%s\n", pullSpan.Name, pullSpan.ParentID)

	// OCI 이미지 속성
	// VMStorageOCI.pull(): span?.setAttribute(key: "oci.image-name", value: .string(name.description))
	pullSpan.SetAttribute("oci.image-name", StringAttr("ghcr.io/cirruslabs/macos-sonoma:latest"))

	// 디스크 크기 속성
	// VMStorageOCI.pull(): span?.setAttribute(key: "oci.image-uncompressed-disk-size-bytes", value: .int(...))
	pullSpan.SetAttribute("oci.image-uncompressed-disk-size-bytes", IntAttr(68719476736))

	time.Sleep(10 * time.Millisecond) // pull 시뮬레이션
	pullSpan.SetAttribute("pull.layers-downloaded", IntAttr(5))
	pullSpan.End()
	fmt.Printf("  pull 스팬 종료: duration=%.2fms\n", pullSpan.Duration().Seconds()*1000)

	// =========================================================================
	// 데모 3: 자식 스팬 — prune 작업 + 이벤트
	// =========================================================================
	printSeparator("데모 3: 자식 스팬 -- prune 작업 + 이벤트")

	// Prune.reclaimIfPossible()에서 스팬 생성:
	// let span = OTel.shared.tracer.spanBuilder(spanName: "prune").startSpan()
	pruneSpan := otel.StartChildSpan("prune")

	// Prune.reclaimIfNeeded():
	//   span?.setAttribute(key: "prune.required-bytes", value: .int(Int(requiredBytes)))
	//   span?.setAttributes(["prune.volume-available-capacity-bytes": .int(...), ...])
	pruneSpan.SetAttributes(map[string]AttributeValue{
		"prune.required-bytes":                IntAttr(15 * 1024 * 1024 * 1024),
		"prune.volume-available-capacity-bytes": IntAttr(8 * 1024 * 1024 * 1024),
	})

	// 이벤트: 개별 프루닝 작업 기록
	// Prune.reclaimIfPossible():
	//   span?.addEvent(name: "Pruned N bytes for /path")
	pruneSpan.AddEvent("Pruned 3221225472 bytes for ghcr.io/org/macos-monterey:old")
	pruneSpan.AddEvent("Pruned 2147483648 bytes for ghcr.io/org/ubuntu-22:latest")

	// 최종 이벤트
	// Prune.reclaimIfPossible(): span?.addEvent(name: "Reclaimed N bytes")
	pruneSpan.AddEvent("Reclaimed 5368709120 bytes")

	time.Sleep(5 * time.Millisecond)
	pruneSpan.End()
	fmt.Printf("  prune 스팬 종료: duration=%.2fms, events=%d\n",
		pruneSpan.Duration().Seconds()*1000, len(pruneSpan.Events))

	// =========================================================================
	// 데모 4: 에러 캡처
	// =========================================================================
	printSeparator("데모 4: 에러 캡처 (recordException)")

	errorSpan := otel.StartChildSpan("pull-failed")

	// Root.swift:
	//   } catch {
	//     OpenTelemetry.instance.contextProvider.activeSpan?.recordException(error)
	//     ...
	//   }
	simulatedErr := fmt.Errorf("URLError: connection timed out")
	errorSpan.RecordException(simulatedErr)
	fmt.Printf("  에러 기록: %v\n", simulatedErr)
	fmt.Printf("  스팬 상태: %s\n", errorSpan.Status)

	errorSpan.End()

	// =========================================================================
	// 데모 5: 루트 스팬 종료 + Flush
	// =========================================================================
	printSeparator("데모 5: 루트 스팬 종료 + Flush")

	time.Sleep(5 * time.Millisecond)

	// Root.swift: defer { OTel.shared.flush() }
	otel.Flush()
	fmt.Printf("  루트 스팬 종료: duration=%.2fms\n", rootSpan.Duration().Seconds()*1000)
	fmt.Println("  OTel.shared.flush() 호출 완료")

	// =========================================================================
	// 데모 6: OTLP 형식 출력
	// =========================================================================
	printSeparator("데모 6: OTLP 형식 출력")
	otel.ExportOTLP()

	// =========================================================================
	// 데모 7: 스팬 트리 시각화
	// =========================================================================
	printSeparator("데모 7: 스팬 트리 시각화")

	fmt.Println("  [trace_id: " + rootSpan.TraceID + "]")
	fmt.Println()

	// 타임라인 출력
	baseTime := rootSpan.StartTime
	maxDuration := rootSpan.Duration()
	barWidth := 50

	for _, span := range otel.AllSpans {
		offset := span.StartTime.Sub(baseTime)
		duration := span.Duration()

		// 상대 위치 계산
		startPos := int(float64(offset) / float64(maxDuration) * float64(barWidth))
		endPos := int(float64(offset+duration) / float64(maxDuration) * float64(barWidth))
		if startPos < 0 {
			startPos = 0
		}
		if endPos > barWidth {
			endPos = barWidth
		}
		if endPos <= startPos {
			endPos = startPos + 1
		}

		// 바 그리기
		prefix := "  "
		if span.ParentID != "" {
			prefix = "    "
		}

		bar := strings.Repeat(" ", startPos) +
			strings.Repeat("=", endPos-startPos) +
			strings.Repeat(" ", barWidth-endPos)

		statusMark := " "
		if span.Status == StatusError {
			statusMark = "!"
		}

		fmt.Printf("%s%-15s |%s|%s %.2fms\n",
			prefix, span.Name, bar, statusMark, duration.Seconds()*1000)
	}

	// =========================================================================
	// 데모 8: OTel.swift 초기화 흐름
	// =========================================================================
	printSeparator("데모 8: OTel.swift 초기화 흐름")

	fmt.Println("  OTel.init():")
	fmt.Println("    1. tracerProvider = Self.initializeTracing()")
	fmt.Println("       -> guard let _ = ProcessInfo.processInfo.environment[\"TRACEPARENT\"]")
	fmt.Println("       -> TRACEPARENT 없으면 nil 반환 (트레이싱 비활성)")
	fmt.Println("    2. resource = DefaultResources().get()")
	fmt.Println("       -> resource.merge(service.name=\"tart\", service.version=CI.version)")
	fmt.Println("    3. spanExporter = OtlpHttpTraceExporter(endpoint:)")
	fmt.Println("       -> OTEL_EXPORTER_OTLP_TRACES_ENDPOINT 환경변수 사용")
	fmt.Println("    4. spanProcessor = SimpleSpanProcessor(spanExporter:)")
	fmt.Println("    5. tracerProvider = TracerProviderBuilder().add(spanProcessor:).with(resource:).build()")
	fmt.Println("    6. OpenTelemetry.registerTracerProvider(tracerProvider:)")
	fmt.Println()
	fmt.Println("  OTel.flush():")
	fmt.Println("    1. activeSpan?.end() — 현재 활성 스팬 종료")
	fmt.Println("    2. tracerProvider?.forceFlush() — 버퍼된 스팬 전송")
	fmt.Println("    3. Thread.sleep(100ms) — OpenTelemetry Swift SDK 버그 회피")
	fmt.Println("       -> https://github.com/open-telemetry/opentelemetry-swift/issues/685")

	// =========================================================================
	// 데모 9: Root.main()의 전체 실행 흐름과 OTel 통합
	// =========================================================================
	printSeparator("데모 9: Root.main() + OTel 통합 흐름")

	fmt.Println("  Root.main():")
	fmt.Println("    ├── signal(SIGINT, SIG_IGN) — 기본 핸들러 비활성")
	fmt.Println("    ├── SIGINT -> task.cancel() — 커스텀 취소 핸들링")
	fmt.Println("    ├── setlinebuf(stdout) — 라인 버퍼링")
	fmt.Println("    ├── defer { OTel.shared.flush() }")
	fmt.Println("    ├── parseAsRoot() — 커맨드 파싱")
	fmt.Println("    ├── OTel.shared.tracer.spanBuilder(commandName).startSpan()")
	fmt.Println("    ├── setActiveSpan(span)")
	fmt.Println("    ├── span.setAttribute(\"Command-line arguments\", args)")
	fmt.Println("    ├── span.setAttribute(CIRRUS_SENTRY_TAGS)")
	fmt.Println("    ├── Config().gc() — GC 수행 (Pull/Clone 제외)")
	fmt.Println("    ├── command.run()")
	fmt.Println("    │   ├── [pull] OTel.shared.tracer.spanBuilder(\"pull\").startSpan()")
	fmt.Println("    │   ├── [prune] spanBuilder(\"prune\").startSpan()")
	fmt.Println("    │   └── span.addEvent(\"Pruned N bytes\")")
	fmt.Println("    └── catch {")
	fmt.Println("        ├── activeSpan?.recordException(error)")
	fmt.Println("        ├── ExecCustomExitCodeError -> Foundation.exit(code)")
	fmt.Println("        └── HasExitCode -> fputs + Foundation.exit(code)")
	fmt.Println("    }")

	// =========================================================================
	// 데모 10: JSON 형식 스팬 출력 (OTLP 시뮬레이션)
	// =========================================================================
	printSeparator("데모 10: JSON 형식 스팬 출력")

	// 단일 스팬의 JSON 출력
	sampleSpan := otel.AllSpans[0] // 루트 스팬
	jsonData, err := json.MarshalIndent(sampleSpan, "  ", "  ")
	if err != nil {
		fmt.Printf("  JSON 변환 실패: %v\n", err)
	} else {
		fmt.Printf("  %s\n", string(jsonData))
	}

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("텔레메트리 설계 요약")
	fmt.Println("  1. TRACEPARENT 기반 활성화: 환경변수가 없으면 트레이싱 비활성 (오버헤드 0)")
	fmt.Println("  2. 루트 스팬: 커맨드 실행 단위, 커맨드 이름으로 생성")
	fmt.Println("  3. 자식 스팬: pull, prune 등 내부 작업 단위")
	fmt.Println("  4. 속성: 커맨드라인 인자, OCI 이미지 크기, 디스크 용량 등 메타데이터")
	fmt.Println("  5. 이벤트: 프루닝된 바이트 수, 회수된 공간 등 주요 사건")
	fmt.Println("  6. 에러 캡처: recordException()으로 예외 정보를 스팬에 기록")
	fmt.Println("  7. OTLP 내보내기: OtlpHttpTraceExporter로 HTTP 전송")
	fmt.Println("  8. flush 워크어라운드: OpenTelemetry Swift SDK 비동기 전송 버그 회피")
}
