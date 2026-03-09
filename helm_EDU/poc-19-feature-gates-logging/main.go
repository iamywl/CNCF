// Package main은 Helm의 Feature Gates와 Logging 시스템의 핵심 개념을 시뮬레이션한다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Feature Gates (환경 변수 기반 실험적 기능 활성화)
// 2. DebugCheckHandler (동적 디버그 레벨 제어, slog.Handler 래핑)
// 3. LogHolder (atomic.Pointer 기반 스레드-안전 로거 교체)
// 4. NewLogger (타임스탬프 제거, 동적 디버그 필터링)
//
// 실행: go run main.go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────
// 1. Feature Gates
// ─────────────────────────────────────────────

// Gate는 기능 게이트의 이름이다.
// 실제 소스: pkg/gates/gates.go Gate
// 핵심: string 별칭 타입으로, 환경 변수 이름을 직접 나타낸다.
type Gate string

// String은 기능 게이트의 문자열 표현을 반환한다.
func (g Gate) String() string {
	return string(g)
}

// IsEnabled는 기능 게이트가 활성화되었는지 판별한다.
// 실제 소스: pkg/gates/gates.go IsEnabled
// 핵심: os.Getenv(string(g)) != "" — 값이 무엇이든 비어있지 않으면 활성화
func (g Gate) IsEnabled() bool {
	return os.Getenv(string(g)) != ""
}

// Error는 기능이 비활성화되었을 때의 자체 서술적 에러를 반환한다.
// 실제 소스: pkg/gates/gates.go Error
// 핵심: 에러 메시지 자체에 활성화 방법을 안내한다.
func (g Gate) Error() error {
	return fmt.Errorf(
		"this feature has been marked as experimental and is not enabled by default. "+
			"Please set %s=1 in your environment to use this feature", g.String())
}

// 실제 Helm에서 정의된 기능 게이트 상수들
const (
	// HELM_EXPERIMENTAL_OCI는 OCI 레지스트리 지원 게이트이다.
	GateOCI Gate = "HELM_EXPERIMENTAL_OCI"
	// HELM_DRIVER_SQL는 SQL 드라이버 사용 게이트이다.
	GateDriverSQL Gate = "HELM_DRIVER_SQL"
	// HELM_DEBUG는 디버그 모드 게이트이다.
	GateDebug Gate = "HELM_DEBUG"
)

// GateGuard는 기능 게이트가 활성화되었을 때만 함수를 실행한다.
func GateGuard(gate Gate, fn func()) error {
	if !gate.IsEnabled() {
		return gate.Error()
	}
	fn()
	return nil
}

// ─────────────────────────────────────────────
// 2. Logging — slog.Handler 인터페이스 시뮬레이션
// ─────────────────────────────────────────────

// LogLevel은 로그 레벨을 나타낸다.
type LogLevel int

const (
	LevelDebug LogLevel = -4
	LevelInfo  LogLevel = 0
	LevelWarn  LogLevel = 4
	LevelError LogLevel = 8
)

func (l LogLevel) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return fmt.Sprintf("LEVEL(%d)", l)
	}
}

// LogRecord는 로그 레코드이다.
type LogRecord struct {
	Level   LogLevel
	Message string
	Attrs   []LogAttr
	Time    time.Time
	Group   string
}

// LogAttr은 키-값 속성이다.
type LogAttr struct {
	Key   string
	Value string
}

// Handler는 slog.Handler 인터페이스를 시뮬레이션한다.
// 실제 소스: log/slog.Handler 인터페이스
type Handler interface {
	Enabled(ctx context.Context, level LogLevel) bool
	Handle(ctx context.Context, r LogRecord) error
	WithAttrs(attrs []LogAttr) Handler
	WithGroup(name string) Handler
}

// ─────────────────────────────────────────────
// 2.1 TextHandler (기본 핸들러)
// ─────────────────────────────────────────────

// TextHandler는 텍스트 형식의 로그 핸들러이다.
type TextHandler struct {
	level         LogLevel
	attrs         []LogAttr
	group         string
	removeTime    bool
	output        *strings.Builder
}

type TextHandlerOptions struct {
	Level      LogLevel
	RemoveTime bool
}

func NewTextHandler(output *strings.Builder, opts *TextHandlerOptions) *TextHandler {
	h := &TextHandler{
		level:  LevelInfo,
		output: output,
	}
	if opts != nil {
		h.level = opts.Level
		h.removeTime = opts.RemoveTime
	}
	return h
}

func (h *TextHandler) Enabled(_ context.Context, level LogLevel) bool {
	return level >= h.level
}

func (h *TextHandler) Handle(_ context.Context, r LogRecord) error {
	var sb strings.Builder

	// 타임스탬프 (removeTime이 false인 경우에만)
	if !h.removeTime {
		sb.WriteString(fmt.Sprintf("time=%s ", r.Time.Format("15:04:05")))
	}

	// 레벨
	sb.WriteString(fmt.Sprintf("level=%s ", r.Level.String()))

	// 그룹 접두사
	prefix := ""
	if h.group != "" {
		prefix = h.group + "."
	}
	if r.Group != "" {
		prefix = r.Group + "."
	}

	// 메시지
	sb.WriteString(fmt.Sprintf("msg=%q ", r.Message))

	// 핸들러 레벨 속성
	for _, attr := range h.attrs {
		sb.WriteString(fmt.Sprintf("%s%s=%s ", prefix, attr.Key, attr.Value))
	}

	// 레코드 레벨 속성
	for _, attr := range r.Attrs {
		sb.WriteString(fmt.Sprintf("%s%s=%s ", prefix, attr.Key, attr.Value))
	}

	line := strings.TrimSpace(sb.String())
	h.output.WriteString(line + "\n")
	return nil
}

func (h *TextHandler) WithAttrs(attrs []LogAttr) Handler {
	newAttrs := make([]LogAttr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &TextHandler{
		level:      h.level,
		attrs:      newAttrs,
		group:      h.group,
		removeTime: h.removeTime,
		output:     h.output,
	}
}

func (h *TextHandler) WithGroup(name string) Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &TextHandler{
		level:      h.level,
		attrs:      h.attrs,
		group:      g,
		removeTime: h.removeTime,
		output:     h.output,
	}
}

// ─────────────────────────────────────────────
// 2.2 DebugCheckHandler (동적 디버그 레벨 제어)
// ─────────────────────────────────────────────

// DebugEnabledFunc는 디버그 로깅 활성화 여부를 결정하는 함수이다.
// 실제 소스: internal/logging/logging.go DebugEnabledFunc
// 핵심: 로거 생성 시가 아니라 로그 기록 시점에 설정을 확인한다.
type DebugEnabledFunc func() bool

// DebugCheckHandler는 로그 시점에 디버그 활성화를 확인하는 핸들러이다.
// 실제 소스: internal/logging/logging.go DebugCheckHandler
// 핵심: slog.Handler를 래핑하여 Debug 레벨만 동적으로 필터링
type DebugCheckHandler struct {
	handler      Handler
	debugEnabled DebugEnabledFunc
}

// Enabled는 slog.Handler.Enabled를 구현한다.
// 실제 소스: internal/logging/logging.go (*DebugCheckHandler).Enabled
// 핵심: Debug 레벨만 동적 확인, 나머지는 항상 true
func (h *DebugCheckHandler) Enabled(_ context.Context, level LogLevel) bool {
	if level == LevelDebug {
		if h.debugEnabled == nil {
			return false
		}
		return h.debugEnabled()
	}
	return true // 다른 레벨은 항상 로깅
}

// Handle은 slog.Handler.Handle을 구현한다.
func (h *DebugCheckHandler) Handle(ctx context.Context, r LogRecord) error {
	return h.handler.Handle(ctx, r)
}

// WithAttrs는 slog.Handler.WithAttrs를 구현한다.
// 실제 소스: 래핑 패턴 — 내부 핸들러에 위임하면서 debugEnabled 유지
func (h *DebugCheckHandler) WithAttrs(attrs []LogAttr) Handler {
	return &DebugCheckHandler{
		handler:      h.handler.WithAttrs(attrs),
		debugEnabled: h.debugEnabled,
	}
}

// WithGroup은 slog.Handler.WithGroup을 구현한다.
func (h *DebugCheckHandler) WithGroup(name string) Handler {
	return &DebugCheckHandler{
		handler:      h.handler.WithGroup(name),
		debugEnabled: h.debugEnabled,
	}
}

// ─────────────────────────────────────────────
// 2.3 Logger & LogHolder
// ─────────────────────────────────────────────

// Logger는 핸들러 기반 로거이다.
type Logger struct {
	handler Handler
}

func NewLoggerFromHandler(h Handler) *Logger {
	return &Logger{handler: h}
}

func (l *Logger) log(ctx context.Context, level LogLevel, msg string, attrs ...LogAttr) {
	if !l.handler.Enabled(ctx, level) {
		return
	}
	r := LogRecord{
		Level:   level,
		Message: msg,
		Attrs:   attrs,
		Time:    time.Now(),
	}
	l.handler.Handle(ctx, r)
}

func (l *Logger) Debug(msg string, attrs ...LogAttr) {
	l.log(context.Background(), LevelDebug, msg, attrs...)
}

func (l *Logger) Info(msg string, attrs ...LogAttr) {
	l.log(context.Background(), LevelInfo, msg, attrs...)
}

func (l *Logger) Warn(msg string, attrs ...LogAttr) {
	l.log(context.Background(), LevelWarn, msg, attrs...)
}

func (l *Logger) Error(msg string, attrs ...LogAttr) {
	l.log(context.Background(), LevelError, msg, attrs...)
}

// NewLogger는 동적 디버그 확인 기능이 있는 새 로거를 생성한다.
// 실제 소스: internal/logging/logging.go NewLogger
// 핵심:
// 1. 기본 핸들러: LevelDebug로 설정 (모든 메시지 통과)
// 2. DebugCheckHandler로 래핑 (Debug만 동적 필터링)
// 3. 타임스탬프 제거 (ReplaceAttr)
func NewLogger(debugEnabled DebugEnabledFunc, output *strings.Builder) *Logger {
	baseHandler := NewTextHandler(output, &TextHandlerOptions{
		Level:      LevelDebug, // 모든 메시지 통과
		RemoveTime: true,       // 타임스탬프 제거
	})

	dynamicHandler := &DebugCheckHandler{
		handler:      baseHandler,
		debugEnabled: debugEnabled,
	}

	return NewLoggerFromHandler(dynamicHandler)
}

// LoggerSetterGetter는 로거를 설정하고 가져올 수 있는 인터페이스이다.
// 실제 소스: internal/logging/logging.go LoggerSetterGetter
type LoggerSetterGetter interface {
	SetLogger(newHandler Handler)
	GetLogger() *Logger
}

// LogHolder는 atomic.Pointer 기반으로 스레드-안전하게 로거를 관리한다.
// 실제 소스: internal/logging/logging.go LogHolder
// 핵심: atomic.Pointer[Logger]로 lock-free 읽기/쓰기
type LogHolder struct {
	logger atomic.Pointer[Logger]
}

// GetLogger는 저장된 로거를 반환한다.
// 실제 소스: internal/logging/logging.go (*LogHolder).Logger
func (l *LogHolder) GetLogger() *Logger {
	if lg := l.logger.Load(); lg != nil {
		return lg
	}
	// DiscardHandler 대신 빈 출력의 로거 반환
	return NewLoggerFromHandler(&discardHandler{})
}

// SetLogger는 새 핸들러로 로거를 교체한다.
// 실제 소스: internal/logging/logging.go (*LogHolder).SetLogger
func (l *LogHolder) SetLogger(newHandler Handler) {
	if newHandler == nil {
		l.logger.Store(NewLoggerFromHandler(&discardHandler{}))
		return
	}
	l.logger.Store(NewLoggerFromHandler(newHandler))
}

// discardHandler는 모든 로그를 버리는 핸들러이다.
type discardHandler struct{}

func (h *discardHandler) Enabled(_ context.Context, _ LogLevel) bool { return false }
func (h *discardHandler) Handle(_ context.Context, _ LogRecord) error { return nil }
func (h *discardHandler) WithAttrs(_ []LogAttr) Handler               { return h }
func (h *discardHandler) WithGroup(_ string) Handler                  { return h }

// ─────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║  Helm Feature Gates & Logging 시뮬레이션          ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. Feature Gates 데모 ===
	demoFeatureGates()

	// === 2. DebugCheckHandler 데모 ===
	demoDebugCheckHandler()

	// === 3. LogHolder 데모 ===
	demoLogHolder()

	// === 4. 통합 시나리오 ===
	demoIntegration()

	fmt.Println("시뮬레이션 완료.")
}

func demoFeatureGates() {
	fmt.Println("━━━ 1. Feature Gates ━━━")

	// 저장 및 복원
	origOCI := os.Getenv(string(GateOCI))
	origSQL := os.Getenv(string(GateDriverSQL))
	origDebug := os.Getenv(string(GateDebug))

	// 초기 상태: 모두 비활성
	os.Unsetenv(string(GateOCI))
	os.Unsetenv(string(GateDriverSQL))
	os.Unsetenv(string(GateDebug))

	fmt.Println("  [초기 상태]")
	gates := []Gate{GateOCI, GateDriverSQL, GateDebug}
	for _, g := range gates {
		fmt.Printf("    %-30s → IsEnabled: %v\n", g.String(), g.IsEnabled())
	}

	// 활성화 테스트
	fmt.Println()
	fmt.Println("  [게이트 활성화]")
	os.Setenv(string(GateOCI), "1")
	fmt.Printf("    %s=1 설정 후 → IsEnabled: %v\n", GateOCI, GateOCI.IsEnabled())

	// 값이 무엇이든 비어있지 않으면 활성화
	os.Setenv(string(GateDriverSQL), "true")
	fmt.Printf("    %s=true 설정 후 → IsEnabled: %v\n", GateDriverSQL, GateDriverSQL.IsEnabled())

	os.Setenv(string(GateDebug), "yes")
	fmt.Printf("    %s=yes 설정 후 → IsEnabled: %v\n", GateDebug, GateDebug.IsEnabled())

	// Error() 메시지
	fmt.Println()
	fmt.Println("  [자체 서술적 에러 메시지]")
	os.Unsetenv(string(GateOCI))
	err := GateOCI.Error()
	fmt.Printf("    %v\n", err)

	// GateGuard 패턴
	fmt.Println()
	fmt.Println("  [GateGuard 패턴]")
	// OCI 비활성 — 실행 차단
	err = GateGuard(GateOCI, func() {
		fmt.Println("    OCI 기능 실행됨")
	})
	if err != nil {
		fmt.Printf("    OCI 차단: %v\n", truncate(err.Error(), 80))
	}

	// Debug 활성 — 실행 허용
	err = GateGuard(GateDebug, func() {
		fmt.Println("    Debug 기능 실행됨!")
	})
	if err != nil {
		fmt.Printf("    Debug 차단: %v\n", err)
	}

	// 복원
	restoreEnv(string(GateOCI), origOCI)
	restoreEnv(string(GateDriverSQL), origSQL)
	restoreEnv(string(GateDebug), origDebug)

	fmt.Println()
}

func demoDebugCheckHandler() {
	fmt.Println("━━━ 2. DebugCheckHandler (동적 디버그 레벨) ━━━")

	var debugEnabled atomic.Bool
	debugEnabled.Store(false)

	var output strings.Builder
	logger := NewLogger(func() bool {
		return debugEnabled.Load()
	}, &output)

	// 디버그 비활성 상태
	fmt.Println("  [debugEnabled=false]")
	output.Reset()
	logger.Debug("디버그 메시지 1")
	logger.Info("정보 메시지 1")
	logger.Warn("경고 메시지 1")
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	// 디버그 활성화
	fmt.Println("  [debugEnabled=true]")
	debugEnabled.Store(true)
	output.Reset()
	logger.Debug("디버그 메시지 2")
	logger.Info("정보 메시지 2")
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	// 다시 비활성화 — 동적 반영
	fmt.Println("  [debugEnabled=false (동적 변경)]")
	debugEnabled.Store(false)
	output.Reset()
	logger.Debug("이 메시지는 무시됨")
	logger.Info("이 메시지는 출력됨")
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	// WithAttrs 래핑
	fmt.Println("  [WithAttrs 래핑]")
	debugEnabled.Store(true)
	output.Reset()
	attrHandler := logger.handler.WithAttrs([]LogAttr{
		{Key: "component", Value: "helm"},
	})
	attrLogger := NewLoggerFromHandler(attrHandler)
	attrLogger.Info("컴포넌트 메시지")
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	// WithGroup 래핑
	fmt.Println("  [WithGroup 래핑]")
	output.Reset()
	groupHandler := logger.handler.WithGroup("action")
	groupLogger := NewLoggerFromHandler(groupHandler)
	groupLogger.Info("설치 시작", LogAttr{Key: "chart", Value: "nginx"})
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	// nil debugEnabled 함수
	fmt.Println("  [nil DebugEnabledFunc]")
	output.Reset()
	nilDebugHandler := &DebugCheckHandler{
		handler:      NewTextHandler(&output, &TextHandlerOptions{Level: LevelDebug, RemoveTime: true}),
		debugEnabled: nil,
	}
	nilDebugLogger := NewLoggerFromHandler(nilDebugHandler)
	nilDebugLogger.Debug("이 메시지는 무시됨 (nil func)")
	nilDebugLogger.Info("이 메시지는 출력됨")
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	fmt.Println()
}

func demoLogHolder() {
	fmt.Println("━━━ 3. LogHolder (atomic.Pointer 기반 로거 교체) ━━━")

	holder := &LogHolder{}

	// 초기 상태: DiscardHandler
	fmt.Println("  [초기 상태: DiscardHandler]")
	logger := holder.GetLogger()
	logger.Info("이 메시지는 버려짐")
	fmt.Println("    (로그 없음 — DiscardHandler)")

	// 로거 설정
	fmt.Println()
	fmt.Println("  [로거 설정]")
	var output strings.Builder
	handler := NewTextHandler(&output, &TextHandlerOptions{
		Level:      LevelInfo,
		RemoveTime: true,
	})
	holder.SetLogger(handler)
	logger = holder.GetLogger()
	logger.Info("첫 번째 메시지")
	logger.Warn("경고 메시지")
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	// 로거 교체 (핫 스왑)
	fmt.Println("  [로거 핫 스왑]")
	var output2 strings.Builder
	handler2 := NewTextHandler(&output2, &TextHandlerOptions{
		Level:      LevelDebug,
		RemoveTime: true,
	})
	holder.SetLogger(handler2)
	logger = holder.GetLogger()
	logger.Debug("교체 후 디버그")
	logger.Info("교체 후 정보")
	fmt.Printf("    새 출력:\n")
	printIndented(output2.String(), "      ")

	// nil 핸들러 설정 → DiscardHandler
	fmt.Println("  [nil 핸들러 설정 → DiscardHandler]")
	holder.SetLogger(nil)
	logger = holder.GetLogger()
	logger.Info("이 메시지는 버려짐")
	fmt.Println("    (로그 없음 — DiscardHandler)")

	// 동시성 테스트
	fmt.Println()
	fmt.Println("  [동시성 테스트]")
	var output3 strings.Builder
	handler3 := NewTextHandler(&output3, &TextHandlerOptions{
		Level:      LevelInfo,
		RemoveTime: true,
	})
	holder.SetLogger(handler3)

	var wg sync.WaitGroup
	var successCount atomic.Int32

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			l := holder.GetLogger()
			l.Info(fmt.Sprintf("goroutine-%d 메시지", id))
			successCount.Add(1)
		}(i)
	}
	wg.Wait()
	fmt.Printf("    goroutine 완료: %d/10 (경합 없음)\n", successCount.Load())
	fmt.Println()
}

func demoIntegration() {
	fmt.Println("━━━ 4. 통합 시나리오: Feature Gate + Logging ━━━")

	// Feature Gate로 디버그 로깅 제어
	origDebug := os.Getenv(string(GateDebug))
	os.Unsetenv(string(GateDebug))

	var output strings.Builder
	logger := NewLogger(func() bool {
		return GateDebug.IsEnabled()
	}, &output)

	// 디버그 비활성 — Feature Gate가 off
	fmt.Println("  [HELM_DEBUG 미설정]")
	output.Reset()
	logger.Debug("디버그: 내부 상태 덤프")
	logger.Info("설치 시작: nginx-1.0.0")
	logger.Info("설치 완료")
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	// 디버그 활성화 — Feature Gate on
	fmt.Println("  [HELM_DEBUG=1 설정]")
	os.Setenv(string(GateDebug), "1")
	output.Reset()
	logger.Debug("디버그: 차트 매니페스트 렌더링 시작")
	logger.Info("설치 시작: nginx-1.0.0")
	logger.Debug("디버그: values.yaml 병합 완료")
	logger.Info("설치 완료")
	fmt.Printf("    출력:\n")
	printIndented(output.String(), "      ")

	// LogHolder를 사용한 런타임 로거 교체
	fmt.Println("  [LogHolder 런타임 교체]")
	holder := &LogHolder{}

	// 초기: 간단한 핸들러
	var out1 strings.Builder
	h1 := NewTextHandler(&out1, &TextHandlerOptions{Level: LevelInfo, RemoveTime: true})
	holder.SetLogger(h1)
	holder.GetLogger().Info("Phase 1: 초기 핸들러")
	fmt.Printf("    Phase 1 출력:\n")
	printIndented(out1.String(), "      ")

	// 교체: 디버그 핸들러
	var out2 strings.Builder
	h2 := NewTextHandler(&out2, &TextHandlerOptions{Level: LevelDebug, RemoveTime: true})
	holder.SetLogger(h2)
	holder.GetLogger().Debug("Phase 2: 디버그 활성")
	holder.GetLogger().Info("Phase 2: 정보 메시지")
	fmt.Printf("    Phase 2 출력:\n")
	printIndented(out2.String(), "      ")

	// 아키텍처 요약
	fmt.Println()
	fmt.Println("  [아키텍처 요약]")
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │ Feature Gates                                                │")
	fmt.Println("  │  type Gate string                                            │")
	fmt.Println("  │  IsEnabled() → os.Getenv(string(g)) != \"\"                    │")
	fmt.Println("  │  Error() → 자체 서술적 에러 (활성화 방법 안내)                   │")
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")
	fmt.Println("  │ Logging                                                      │")
	fmt.Println("  │  DebugCheckHandler                                           │")
	fmt.Println("  │    ├── handler: slog.Handler (래핑 대상)                       │")
	fmt.Println("  │    └── debugEnabled: func() bool (로그 시점에 평가)             │")
	fmt.Println("  │  Enabled(Debug) → debugEnabled()                             │")
	fmt.Println("  │  Enabled(기타) → true (항상 로깅)                              │")
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")
	fmt.Println("  │ LogHolder                                                    │")
	fmt.Println("  │  atomic.Pointer[Logger] — lock-free 스레드 안전               │")
	fmt.Println("  │  SetLogger() → atomic.Store                                  │")
	fmt.Println("  │  Logger()    → atomic.Load                                   │")
	fmt.Println("  └──────────────────────────────────────────────────────────────┘")

	// 복원
	restoreEnv(string(GateDebug), origDebug)
	fmt.Println()
}

// ─────────────────────────────────────────────
// 유틸리티
// ─────────────────────────────────────────────

func restoreEnv(key, origVal string) {
	if origVal != "" {
		os.Setenv(key, origVal)
	} else {
		os.Unsetenv(key)
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func printIndented(s, indent string) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for _, line := range lines {
		if line != "" {
			fmt.Printf("%s%s\n", indent, line)
		}
	}
}
