// Package main은 Alertmanager의 Feature Control과 Distributed Tracing 시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Flagger 인터페이스 (기능 플래그 조회)
// 2. Flags 구조체 (명령행 옵션 기반 활성화)
// 3. NewFlags 팩토리 (쉼표 구분 문자열 파싱)
// 4. NoopFlagger (모든 기능 비활성화 기본값)
// 5. OpenTelemetry 트레이서 설정
// 6. HTTP 트랜스포트 래핑 (요청별 스팬 생성)
// 7. 트레이싱 설정 (gRPC/HTTP 내보내기)
// 8. 자동 GOMEMLIMIT / GOMAXPROCS
// 9. Classic Mode vs UTF-8 Strict Mode
// 10. 기능 플래그 검증 (알 수 없는 플래그 거부)
//
// 실제 소스 참조:
//   - featurecontrol/featurecontrol.go (Flagger 인터페이스, Flags 구조체)
//   - tracing/tracing.go              (OpenTelemetry 트레이서)
//   - tracing/config.go               (트레이싱 설정)
//   - tracing/http.go                 (HTTP 트랜스포트 래핑)
package main

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ============================================================================
// 1. Feature Control (featurecontrol/featurecontrol.go 시뮬레이션)
// ============================================================================

// Feature 상수
const (
	FeatureAlertNamesInMetrics   = "alert-names-in-metrics"
	FeatureReceiverNameInMetrics = "receiver-name-in-metrics"
	FeatureClassicMode           = "classic-mode"
	FeatureUTF8StrictMode        = "utf8-strict-mode"
	FeatureAutoGOMEMLIMIT        = "auto-gomemlimit"
	FeatureAutoGOMAXPROCS        = "auto-gomaxprocs"
)

// AllFeatures는 지원되는 모든 기능 플래그 목록이다.
var AllFeatures = map[string]string{
	FeatureAlertNamesInMetrics:   "알림 이름을 Prometheus 메트릭 레이블에 포함 (카디널리티 위험)",
	FeatureReceiverNameInMetrics: "Receiver 이름을 메트릭 레이블에 포함",
	FeatureClassicMode:           "UTF-8 이전 클래식 매처 모드",
	FeatureUTF8StrictMode:        "UTF-8 엄격 검증 모드",
	FeatureAutoGOMEMLIMIT:        "Linux 컨테이너 메모리 한도에 맞춰 GOMEMLIMIT 자동 설정",
	FeatureAutoGOMAXPROCS:        "Linux 컨테이너 CPU 쿼터에 맞춰 GOMAXPROCS 자동 설정",
}

// Flagger는 기능 플래그 조회 인터페이스다.
type Flagger interface {
	EnableAlertNamesInMetrics() bool
	EnableReceiverNamesInMetrics() bool
	ClassicMode() bool
	UTF8StrictMode() bool
	EnableAutoGOMEMLIMIT() bool
	EnableAutoGOMAXPROCS() bool
}

// Flags는 실제 기능 플래그 구현체다.
type Flags struct {
	alertNamesInMetrics    bool
	receiverNamesInMetrics bool
	classicMode            bool
	utf8StrictMode         bool
	autoGOMEMLIMIT         bool
	autoGOMAXPROCS         bool
}

func (f *Flags) EnableAlertNamesInMetrics() bool    { return f.alertNamesInMetrics }
func (f *Flags) EnableReceiverNamesInMetrics() bool  { return f.receiverNamesInMetrics }
func (f *Flags) ClassicMode() bool                   { return f.classicMode }
func (f *Flags) UTF8StrictMode() bool                { return f.utf8StrictMode }
func (f *Flags) EnableAutoGOMEMLIMIT() bool          { return f.autoGOMEMLIMIT }
func (f *Flags) EnableAutoGOMAXPROCS() bool          { return f.autoGOMAXPROCS }

// NewFlags는 쉼표 구분 문자열에서 Flags를 생성한다.
// 알 수 없는 기능 플래그가 있으면 에러를 반환한다.
func NewFlags(features string) (*Flags, error) {
	flags := &Flags{}
	if features == "" {
		return flags, nil
	}

	for _, f := range strings.Split(features, ",") {
		f = strings.TrimSpace(f)
		switch f {
		case FeatureAlertNamesInMetrics:
			flags.alertNamesInMetrics = true
		case FeatureReceiverNameInMetrics:
			flags.receiverNamesInMetrics = true
		case FeatureClassicMode:
			flags.classicMode = true
		case FeatureUTF8StrictMode:
			flags.utf8StrictMode = true
		case FeatureAutoGOMEMLIMIT:
			flags.autoGOMEMLIMIT = true
		case FeatureAutoGOMAXPROCS:
			flags.autoGOMAXPROCS = true
		default:
			return nil, fmt.Errorf("알 수 없는 기능 플래그: %q (지원 목록: %s)",
				f, supportedFeatures())
		}
	}

	// 상호 배타 검증
	if flags.classicMode && flags.utf8StrictMode {
		return nil, fmt.Errorf("classic-mode와 utf8-strict-mode는 동시에 사용할 수 없습니다")
	}

	return flags, nil
}

func supportedFeatures() string {
	var names []string
	for k := range AllFeatures {
		names = append(names, k)
	}
	return strings.Join(names, ", ")
}

// NoopFlagger는 모든 기능이 비활성화된 기본 Flagger다.
type NoopFlagger struct{}

func (NoopFlagger) EnableAlertNamesInMetrics() bool    { return false }
func (NoopFlagger) EnableReceiverNamesInMetrics() bool  { return false }
func (NoopFlagger) ClassicMode() bool                   { return false }
func (NoopFlagger) UTF8StrictMode() bool                { return false }
func (NoopFlagger) EnableAutoGOMEMLIMIT() bool          { return false }
func (NoopFlagger) EnableAutoGOMAXPROCS() bool          { return false }

// ============================================================================
// 2. Distributed Tracing (tracing/ 시뮬레이션)
// ============================================================================

// TracingConfig는 트레이싱 설정이다.
type TracingConfig struct {
	Endpoint     string  // OTLP 엔드포인트
	Protocol     string  // "grpc" or "http"
	SamplingRate float64 // 0.0 ~ 1.0
	ServiceName  string
	Insecure     bool
}

// Span은 단일 추적 스팬이다.
type Span struct {
	TraceID   string
	SpanID    string
	ParentID  string
	Name      string
	StartTime time.Time
	EndTime   time.Time
	Attributes map[string]string
	Status     string // "OK", "ERROR"
}

// Tracer는 트레이싱 생성기다.
type Tracer struct {
	Config  TracingConfig
	spans   []Span
	enabled bool
}

// NewTracer는 새 트레이서를 생성한다.
func NewTracer(config TracingConfig) *Tracer {
	return &Tracer{
		Config:  config,
		enabled: config.Endpoint != "",
	}
}

func randomHex(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hex[rand.Intn(len(hex))]
	}
	return string(b)
}

// StartSpan은 새 스팬을 시작한다.
func (t *Tracer) StartSpan(ctx context.Context, name string) (context.Context, *Span) {
	if !t.enabled {
		return ctx, nil
	}

	// 샘플링 결정
	if rand.Float64() > t.Config.SamplingRate {
		return ctx, nil
	}

	span := &Span{
		TraceID:    randomHex(32),
		SpanID:     randomHex(16),
		Name:       name,
		StartTime:  time.Now(),
		Attributes: make(map[string]string),
	}

	return ctx, span
}

// EndSpan은 스팬을 종료한다.
func (t *Tracer) EndSpan(span *Span, err error) {
	if span == nil {
		return
	}
	span.EndTime = time.Now()
	if err != nil {
		span.Status = "ERROR"
		span.Attributes["error.message"] = err.Error()
	} else {
		span.Status = "OK"
	}
	t.spans = append(t.spans, *span)
}

// ============================================================================
// 3. HTTP Transport 래핑 (tracing/http.go 시뮬레이션)
// ============================================================================

// TracingTransport는 HTTP 요청에 트레이싱을 추가하는 래퍼다.
type TracingTransport struct {
	Tracer *Tracer
}

// RoundTrip은 HTTP 요청을 래핑하여 스팬을 생성한다.
func (t *TracingTransport) RoundTrip(method, url string) (*Span, error) {
	ctx := context.Background()
	_, span := t.Tracer.StartSpan(ctx, fmt.Sprintf("HTTP %s", method))
	if span != nil {
		span.Attributes["http.method"] = method
		span.Attributes["http.url"] = url
		span.Attributes["http.status_code"] = "200"
	}

	// 요청 시뮬레이션
	time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)

	t.Tracer.EndSpan(span, nil)
	return span, nil
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Alertmanager Feature Control & Tracing 시뮬레이션 PoC      ║")
	fmt.Println("║  실제 소스: featurecontrol/, tracing/                       ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. Feature Flags 파싱 ===
	fmt.Println("=== 1. Feature Flags 파싱 ===")

	testCases := []string{
		"",
		"receiver-name-in-metrics,auto-gomemlimit",
		"classic-mode",
		"utf8-strict-mode",
		"classic-mode,utf8-strict-mode",       // 상호 배타
		"receiver-name-in-metrics,unknown-flag", // 알 수 없는 플래그
	}

	for _, tc := range testCases {
		flags, err := NewFlags(tc)
		input := tc
		if input == "" {
			input = "(빈 문자열)"
		}
		if err != nil {
			fmt.Printf("  --enable-feature=%s\n    오류: %v\n\n", input, err)
		} else {
			fmt.Printf("  --enable-feature=%s\n", input)
			fmt.Printf("    ReceiverNamesInMetrics=%v, ClassicMode=%v, AutoGOMEMLIMIT=%v\n\n",
				flags.EnableReceiverNamesInMetrics(),
				flags.ClassicMode(),
				flags.EnableAutoGOMEMLIMIT())
		}
	}

	// === 2. Flagger 인터페이스 활용 ===
	fmt.Println("=== 2. Flagger 인터페이스 활용 ===")

	var flagger Flagger = NoopFlagger{}
	fmt.Printf("  NoopFlagger: ReceiverNamesInMetrics=%v\n", flagger.EnableReceiverNamesInMetrics())

	flags, _ := NewFlags("receiver-name-in-metrics,auto-gomemlimit")
	flagger = flags
	fmt.Printf("  CustomFlags: ReceiverNamesInMetrics=%v, AutoGOMEMLIMIT=%v\n",
		flagger.EnableReceiverNamesInMetrics(), flagger.EnableAutoGOMEMLIMIT())

	// 기능 플래그 기반 메트릭 레이블 결정
	fmt.Println()
	fmt.Println("  [메트릭 레이블 결정 예시]")
	labels := map[string]string{"status": "firing"}
	if flagger.EnableReceiverNamesInMetrics() {
		labels["receiver"] = "slack-ops"
	}
	fmt.Printf("    메트릭 레이블: %v\n", labels)
	fmt.Println()

	// === 3. 지원 기능 목록 ===
	fmt.Println("=== 3. 지원 기능 플래그 목록 ===")
	for name, desc := range AllFeatures {
		fmt.Printf("  %-30s %s\n", name, desc)
	}
	fmt.Println()

	// === 4. 트레이싱 설정 ===
	fmt.Println("=== 4. Distributed Tracing ===")

	tracer := NewTracer(TracingConfig{
		Endpoint:     "localhost:4317",
		Protocol:     "grpc",
		SamplingRate: 1.0,
		ServiceName:  "alertmanager",
		Insecure:     true,
	})
	fmt.Printf("  트레이서 설정:\n")
	fmt.Printf("    Endpoint: %s (%s)\n", tracer.Config.Endpoint, tracer.Config.Protocol)
	fmt.Printf("    SamplingRate: %.1f\n", tracer.Config.SamplingRate)
	fmt.Printf("    ServiceName: %s\n", tracer.Config.ServiceName)
	fmt.Println()

	// === 5. 스팬 생성 ===
	fmt.Println("=== 5. 스팬 생성 ===")

	ctx := context.Background()
	_, rootSpan := tracer.StartSpan(ctx, "notify.pipeline")
	if rootSpan != nil {
		rootSpan.Attributes["receiver"] = "slack-ops"
		rootSpan.Attributes["alerts.count"] = "3"
	}

	// 하위 스팬: 템플릿 렌더링
	_, tmplSpan := tracer.StartSpan(ctx, "template.render")
	if tmplSpan != nil {
		tmplSpan.ParentID = rootSpan.SpanID
		tmplSpan.Attributes["template.name"] = "slack.message"
	}
	tracer.EndSpan(tmplSpan, nil)

	// 하위 스팬: HTTP 전송
	transport := &TracingTransport{Tracer: tracer}
	httpSpan, _ := transport.RoundTrip("POST", "https://hooks.slack.com/services/xxx")
	if httpSpan != nil && rootSpan != nil {
		httpSpan.ParentID = rootSpan.SpanID
	}

	tracer.EndSpan(rootSpan, nil)

	// 스팬 출력
	for _, span := range tracer.spans {
		fmt.Printf("  [%s] %s (trace=%s..., span=%s...)\n",
			span.Status, span.Name, span.TraceID[:8], span.SpanID[:8])
		for k, v := range span.Attributes {
			fmt.Printf("    %s: %s\n", k, v)
		}
		fmt.Printf("    duration: %v\n", span.EndTime.Sub(span.StartTime).Round(time.Microsecond))
		fmt.Println()
	}

	// === 6. 비활성 트레이서 ===
	fmt.Println("=== 6. 비활성 트레이서 (Endpoint 미설정) ===")
	noopTracer := NewTracer(TracingConfig{})
	_, noopSpan := noopTracer.StartSpan(ctx, "should-not-trace")
	fmt.Printf("  스팬 생성됨: %v (nil이어야 함)\n", noopSpan != nil)
}
