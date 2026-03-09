# 21. Feature Gates & Logging — 설정 & 관찰 가능성 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Feature Gates 아키텍처](#2-feature-gates-아키텍처)
3. [Gate 타입과 메서드](#3-gate-타입과-메서드)
4. [환경 변수 기반 활성화](#4-환경-변수-기반-활성화)
5. [Feature Gate 활용 패턴](#5-feature-gate-활용-패턴)
6. [Logging 아키텍처](#6-logging-아키텍처)
7. [DebugCheckHandler](#7-debugcheckhandler)
8. [LogHolder 원자적 로거](#8-logholder-원자적-로거)
9. [동적 디버그 로깅](#9-동적-디버그-로깅)
10. [slog.Handler 인터페이스 구현](#10-sloghandler-인터페이스-구현)
11. [Helm에서의 통합 사용](#11-helm에서의-통합-사용)
12. [설계 원칙 분석](#12-설계-원칙-분석)

---

## 1. 개요

| 서브시스템 | 패키지 | 역할 |
|-----------|--------|------|
| **Feature Gates** | `pkg/gates/` | 실험적 기능의 활성화/비활성화 제어 |
| **Logging** | `internal/logging/` | 구조화된 로깅, 동적 디버그 수준 제어 |

### 왜 이 컴포넌트들이 필요한가

```
┌───────────────────────────────────────────────────────────┐
│                    Helm 설정 계층                           │
│                                                           │
│  환경변수: HELM_EXPERIMENTAL_OCI=1                         │
│       │                                                   │
│       ▼                                                   │
│  Feature Gate ──→ gate.IsEnabled()                        │
│       │           ├─ true:  실험 기능 활성화                │
│       │           └─ false: "실험 기능입니다" 에러           │
│       │                                                   │
│  --debug 플래그 ──→ Logging                               │
│       │           ├─ DebugCheckHandler: 동적 필터링         │
│       │           └─ LogHolder: 원자적 로거 교체            │
└───────────────────────────────────────────────────────────┘
```

---

## 2. Feature Gates 아키텍처

### 소스 코드 구조

```
pkg/gates/
├── doc.go      ← 패키지 문서
└── gates.go    ← Gate 타입, IsEnabled, Error 메서드
```

### 패키지 문서

```go
// Package gates provides a general tool for working with experimental feature gates.
// This provides convenience methods where the user can determine if certain
// experimental features are enabled.
```

### 설계 철학

Feature Gate 시스템은 **최소주의 설계**를 따른다:

1. **환경 변수 하나로 제어**: 복잡한 설정 파일이나 플래그 없이 환경 변수만으로 기능 토글
2. **타입 안전성**: `Gate` 타입이 `string`의 별칭으로 컴파일 타임 검증
3. **자기 설명적 에러**: 비활성화된 기능 사용 시 어떻게 활성화하는지 안내

---

## 3. Gate 타입과 메서드

### Gate 타입 정의

`pkg/gates/gates.go`:

```go
type Gate string

func (g Gate) String() string {
    return string(g)
}

func (g Gate) IsEnabled() bool {
    return os.Getenv(string(g)) != ""
}

func (g Gate) Error() error {
    return fmt.Errorf(
        "this feature has been marked as experimental and is not enabled by default. "+
        "Please set %s=1 in your environment to use this feature",
        g.String())
}
```

### 메서드 설계 분석

| 메서드 | 반환 | 동작 |
|--------|------|------|
| `String()` | `string` | Gate 이름 (환경 변수명) |
| `IsEnabled()` | `bool` | 환경 변수가 설정되어 있으면 `true` |
| `Error()` | `error` | 활성화 방법을 안내하는 에러 메시지 |

**왜 `os.Getenv(string(g)) != ""`인가?**

`!= ""`은 값이 무엇이든 설정만 되어 있으면 활성화된다는 의미이다:
- `HELM_EXPERIMENTAL_OCI=1` → 활성화
- `HELM_EXPERIMENTAL_OCI=true` → 활성화
- `HELM_EXPERIMENTAL_OCI=yes` → 활성화
- `HELM_EXPERIMENTAL_OCI=` → 비활성화 (빈 문자열)
- (미설정) → 비활성화

이는 사용자가 직관적으로 사용할 수 있도록 의도된 설계이다.

---

## 4. 환경 변수 기반 활성화

### 사용 패턴

```go
// Feature Gate 상수 정의
const (
    ExperimentalOCI  = gates.Gate("HELM_EXPERIMENTAL_OCI")
    ExperimentalWASM = gates.Gate("HELM_EXPERIMENTAL_WASM")
)

// 코드에서 사용
func pushChart(ref string) error {
    if !ExperimentalOCI.IsEnabled() {
        return ExperimentalOCI.Error()
        // "this feature has been marked as experimental and is not enabled
        //  by default. Please set HELM_EXPERIMENTAL_OCI=1 in your environment
        //  to use this feature"
    }
    // ... OCI 푸시 로직
}
```

### 활성화 방법

```bash
# 쉘에서 일시적 활성화
export HELM_EXPERIMENTAL_OCI=1
helm push mychart.tgz oci://registry.example.com/charts

# 명령어 인라인 활성화
HELM_EXPERIMENTAL_OCI=1 helm push mychart.tgz oci://...

# 영구 활성화 (~/.bashrc 또는 ~/.zshrc)
echo 'export HELM_EXPERIMENTAL_OCI=1' >> ~/.bashrc
```

### Feature Gate 수명 주기

```
실험적 기능 → Gate로 보호 → 안정화 → Gate 제거 → 기본 활성

예:
  Helm 3.x: HELM_EXPERIMENTAL_OCI=1 필요
  Helm 4.x: OCI가 기본, Gate 제거됨
```

**왜 제거하는가?** 기능이 안정화되면 Gate를 유지할 이유가 없다. Gate를 유지하면 사용자가 불필요하게 환경 변수를 설정해야 하고, 코드에 불필요한 분기가 남는다.

---

## 5. Feature Gate 활용 패턴

### 가드 패턴

```go
// 함수 진입점에서 Gate 확인
func experimentalFeature() error {
    if !gate.IsEnabled() {
        return gate.Error()
    }
    // 실험적 기능 로직
    return nil
}
```

### 분기 패턴

```go
// 기능에 따른 동작 분기
func resolve() {
    if gate.IsEnabled() {
        // 새로운 해석 알고리즘 사용
        newResolver()
    } else {
        // 기존 알고리즘 유지
        legacyResolver()
    }
}
```

### 테스트에서의 사용

```go
func TestExperimentalFeature(t *testing.T) {
    t.Setenv("HELM_EXPERIMENTAL_X", "1")
    // Gate가 활성화된 상태에서 테스트
    assert.True(t, ExperimentalX.IsEnabled())
}
```

---

## 6. Logging 아키텍처

### 소스 코드 구조

```
internal/logging/
├── logging.go       ← NewLogger, DebugCheckHandler, LogHolder
└── logging_test.go  ← 테스트
```

### 전체 아키텍처

```
┌─────────────────────────────────────────────────────┐
│                  Logging System                      │
│                                                     │
│  ┌─────────────────┐    ┌──────────────────────┐   │
│  │ slog.Logger     │    │ DebugCheckHandler    │   │
│  │                 │───→│ (동적 디버그 필터링)    │   │
│  │ .Info("...")    │    │                      │   │
│  │ .Debug("...")   │    │ debugEnabled() → bool │   │
│  │ .Warn("...")    │    └──────────┬───────────┘   │
│  └─────────────────┘               │               │
│                              ┌─────▼──────┐        │
│                              │ TextHandler │        │
│                              │ (os.Stderr) │        │
│                              └────────────┘        │
│                                                     │
│  ┌─────────────────┐                               │
│  │ LogHolder       │ ← atomic.Pointer[slog.Logger] │
│  │ (원자적 교체)    │                               │
│  └─────────────────┘                               │
└─────────────────────────────────────────────────────┘
```

---

## 7. DebugCheckHandler

### 구조체 정의

`internal/logging/logging.go` 라인 27-34:

```go
// DebugEnabledFunc는 디버그 로깅 활성화 여부를 결정하는 함수 타입이다.
// 로거 생성 시가 아닌 로그 기록 시에 확인하기 위해 함수를 사용한다.
type DebugEnabledFunc func() bool

type DebugCheckHandler struct {
    handler      slog.Handler     // 실제 출력을 담당하는 핸들러
    debugEnabled DebugEnabledFunc // 디버그 활성화 여부 확인 함수
}
```

### Enabled 메서드

```go
func (h *DebugCheckHandler) Enabled(_ context.Context, level slog.Level) bool {
    if level == slog.LevelDebug {
        if h.debugEnabled == nil {
            return false
        }
        return h.debugEnabled()  // 런타임에 동적으로 확인
    }
    return true  // Info, Warn, Error는 항상 활성
}
```

**왜 함수 포인터를 사용하는가?**

```go
// 잘못된 방식: 로거 생성 시점의 값만 사용
logger := NewLogger(settings.Debug)  // Debug=false 시점
settings.Debug = true                 // 이후 변경
logger.Debug("msg")                   // 여전히 출력 안 됨!

// 올바른 방식: 매번 확인
logger := NewLogger(func() bool { return settings.Debug })
settings.Debug = true                 // 이후 변경
logger.Debug("msg")                   // 출력됨!
```

### slog.Handler 인터페이스 구현

```go
// Handle: 실제 로그 기록을 내부 핸들러에 위임
func (h *DebugCheckHandler) Handle(ctx context.Context, r slog.Record) error {
    return h.handler.Handle(ctx, r)
}

// WithAttrs: 어트리뷰트가 추가된 새 핸들러 반환 (불변성)
func (h *DebugCheckHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
    return &DebugCheckHandler{
        handler:      h.handler.WithAttrs(attrs),
        debugEnabled: h.debugEnabled,
    }
}

// WithGroup: 그룹이 추가된 새 핸들러 반환 (불변성)
func (h *DebugCheckHandler) WithGroup(name string) slog.Handler {
    return &DebugCheckHandler{
        handler:      h.handler.WithGroup(name),
        debugEnabled: h.debugEnabled,
    }
}
```

**왜 WithAttrs/WithGroup이 새 인스턴스를 반환하는가?** `slog.Handler`는 **불변(immutable)** 패턴을 따른다. 각 `WithAttrs`/`WithGroup` 호출은 기존 핸들러를 변경하지 않고 새 핸들러를 반환한다. 이는 goroutine 안전성을 보장한다.

---

## 8. LogHolder 원자적 로거

### 구조체 정의

```go
type LoggerSetterGetter interface {
    SetLogger(newHandler slog.Handler)
    Logger() *slog.Logger
}

type LogHolder struct {
    logger atomic.Pointer[slog.Logger]
}
```

### Logger 메서드

```go
func (l *LogHolder) Logger() *slog.Logger {
    if lg := l.logger.Load(); lg != nil {
        return lg
    }
    return slog.New(slog.DiscardHandler)  // nil이면 discard
}
```

### SetLogger 메서드

```go
func (l *LogHolder) SetLogger(newHandler slog.Handler) {
    if newHandler == nil {
        l.logger.Store(slog.New(slog.DiscardHandler))
        return
    }
    l.logger.Store(slog.New(newHandler))
}
```

**왜 atomic.Pointer를 사용하는가?**

Helm의 플러그인 시스템에서 로거를 교체할 수 있어야 한다:
1. **초기**: 기본 로거
2. **플러그인 로드 후**: 플러그인별 로거
3. **디버그 활성화 시**: 디버그 레벨 로거

이 교체가 **thread-safe**해야 하므로 `atomic.Pointer`를 사용한다. `sync.Mutex`보다 읽기 성능이 뛰어나다 (로그 기록은 읽기가 훨씬 빈번).

---

## 9. 동적 디버그 로깅

### NewLogger 함수

```go
func NewLogger(debugEnabled DebugEnabledFunc) *slog.Logger {
    // 기본 핸들러: stderr에 텍스트 출력
    baseHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
        Level: slog.LevelDebug,  // 모든 레벨 통과 (필터링은 우리가 함)
        ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
            if a.Key == slog.TimeKey {
                return slog.Attr{}  // 타임스탬프 제거
            }
            return a
        },
    })

    // 동적 디버그 핸들러로 래핑
    dynamicHandler := &DebugCheckHandler{
        handler:      baseHandler,
        debugEnabled: debugEnabled,
    }

    return slog.New(dynamicHandler)
}
```

### 핸들러 체인

```
slog.Logger
    │
    ▼
DebugCheckHandler         ← Debug 레벨 동적 필터링
    │ Enabled() 호출
    │  ├─ Debug: debugEnabled() 확인
    │  └─ 그 외: 항상 true
    │
    ▼
slog.TextHandler          ← 실제 출력 (stderr)
    │ Handle() 호출
    │  ├─ 타임스탬프 제거
    │  └─ 텍스트 형식 출력
    │
    ▼
os.Stderr
```

### 타임스탬프 제거 이유

```go
ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
    if a.Key == slog.TimeKey {
        return slog.Attr{}  // 빈 어트리뷰트 → 출력에서 제외
    }
    return a
},
```

**왜 타임스탬프를 제거하는가?** Helm CLI는 대화형 도구이다. 사용자가 `--debug` 플래그로 디버그 로그를 볼 때, 타임스탬프는 노이즈일 뿐이다. 필요한 정보는 메시지 내용과 레벨이다.

---

## 10. slog.Handler 인터페이스 구현

### 인터페이스 정의

Go 1.21에서 도입된 `slog.Handler` 인터페이스:

```go
type Handler interface {
    Enabled(context.Context, Level) bool
    Handle(context.Context, Record) error
    WithAttrs(attrs []Attr) Handler
    WithGroup(name string) Handler
}
```

### DebugCheckHandler의 구현 요약

```
┌───────────────────────────────────────────────────┐
│         DebugCheckHandler 구현                      │
├───────────────────────────────────────────────────┤
│ Enabled(ctx, level)                               │
│   → Debug: debugEnabled() 동적 확인               │
│   → 그 외: true                                   │
├───────────────────────────────────────────────────┤
│ Handle(ctx, record)                               │
│   → handler.Handle(ctx, record) 위임              │
├───────────────────────────────────────────────────┤
│ WithAttrs(attrs)                                  │
│   → 새 DebugCheckHandler(handler.WithAttrs, fn)   │
├───────────────────────────────────────────────────┤
│ WithGroup(name)                                   │
│   → 새 DebugCheckHandler(handler.WithGroup, fn)   │
└───────────────────────────────────────────────────┘
```

---

## 11. Helm에서의 통합 사용

### Feature Gate + Logging 연동

```go
// settings.go에서
var debug bool

// main.go에서
logger := logging.NewLogger(func() bool { return debug })

// 명령어에서
func runExperimentalCmd() error {
    if !gates.Gate("HELM_EXPERIMENTAL_X").IsEnabled() {
        return gates.Gate("HELM_EXPERIMENTAL_X").Error()
    }

    logger.Debug("실험적 기능 시작",
        slog.String("feature", "X"))

    // ... 실험적 기능 로직 ...

    logger.Info("실험적 기능 완료")
    return nil
}
```

### 디버그 로깅 시나리오

```bash
# 일반 실행 (Debug 로그 없음)
$ helm install myrelease ./mychart
NAME: myrelease
STATUS: deployed

# 디버그 활성화 (Debug 로그 출력)
$ helm install myrelease ./mychart --debug
level=DEBUG msg="loading chart" path=./mychart
level=DEBUG msg="rendering templates" release=myrelease
level=DEBUG msg="creating kubernetes resources" count=5
NAME: myrelease
STATUS: deployed
```

### LogHolder 교체 시나리오

```go
// 초기화
holder := &logging.LogHolder{}
holder.SetLogger(slog.NewTextHandler(os.Stderr, nil))

// 플러그인 실행 시 로거 교체
pluginHandler := slog.NewJSONHandler(pluginLog, nil)
holder.SetLogger(pluginHandler)

// 플러그인 완료 후 복원
holder.SetLogger(slog.NewTextHandler(os.Stderr, nil))
```

---

## 12. 설계 원칙 분석

### Feature Gates 설계 원칙

| 원칙 | 구현 | 이유 |
|------|------|------|
| **최소주의** | 타입 3개 메서드 | 학습 비용 최소화 |
| **환경 변수 기반** | `os.Getenv()` | 어디서든 설정 가능 |
| **자기 설명적** | `Error()` 메시지 | 사용자가 해결 방법을 즉시 알 수 있음 |
| **zero-value 안전** | 미설정 = 비활성 | 기본 동작이 안전 |

### Logging 설계 원칙

| 원칙 | 구현 | 이유 |
|------|------|------|
| **동적 제어** | `DebugEnabledFunc` | 런타임에 디버그 수준 변경 |
| **thread-safe** | `atomic.Pointer` | 동시 접근 안전 |
| **불변 핸들러** | `WithAttrs` 새 인스턴스 | goroutine 안전 |
| **slog 표준** | `slog.Handler` 구현 | Go 표준 라이브러리 호환 |
| **타임스탬프 제거** | `ReplaceAttr` | CLI 친화적 출력 |

### 왜 `log/slog`를 사용하는가?

Helm 4.x는 Go 1.21의 `log/slog`를 채택했다:

1. **표준 라이브러리**: 외부 의존성(logrus, zap) 제거
2. **구조화된 로깅**: 키-값 쌍으로 메타데이터 기록
3. **핸들러 추상화**: 출력 형식(text/json)과 로직 분리
4. **성능**: 구조화된 로깅 프레임워크 중 가장 낮은 오버헤드

### 인터페이스 준수 검증

```go
// 컴파일 타임에 인터페이스 구현 확인
var _ LoggerSetterGetter = &LogHolder{}
```

이 패턴은 `LogHolder`가 `LoggerSetterGetter` 인터페이스를 구현하는지 컴파일 시점에 검증한다. 런타임 에러를 방지하는 Go의 관용적 패턴이다.

---

## 부록: 주요 소스 파일 참조

| 파일 | 설명 |
|------|------|
| `pkg/gates/gates.go` | Gate 타입, IsEnabled, Error (39줄) |
| `pkg/gates/doc.go` | 패키지 문서 |
| `internal/logging/logging.go` | DebugCheckHandler, LogHolder, NewLogger (126줄) |
