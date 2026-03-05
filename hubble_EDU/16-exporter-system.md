# 16. Hubble 익스포터 시스템 Deep Dive

## 목차

1. [개요](#1-개요)
2. [익스포터 아키텍처](#2-익스포터-아키텍처)
3. [FlowLogExporter 인터페이스](#3-flowlogexporter-인터페이스)
4. [정적 익스포터 (exporter)](#4-정적-익스포터-exporter)
5. [Export 파이프라인](#5-export-파이프라인)
6. [Encoder 시스템](#6-encoder-시스템)
7. [Writer 시스템과 파일 로테이션](#7-writer-시스템과-파일-로테이션)
8. [FieldMask (필드 마스킹)](#8-fieldmask-필드-마스킹)
9. [Aggregator (플로우 집계)](#9-aggregator-플로우-집계)
10. [AggregatorRunner (주기적 내보내기)](#10-aggregatorrunner-주기적-내보내기)
11. [동적 익스포터 (dynamicExporter)](#11-동적-익스포터-dynamicexporter)
12. [설정 파일 구조 (FlowLogConfig)](#12-설정-파일-구조-flowlogconfig)
13. [ConfigWatcher (설정 감시)](#13-configwatcher-설정-감시)
14. [OnExportEvent 훅 시스템](#14-onexportevent-훅-시스템)
15. [이벤트 변환 (eventToExportEvent)](#15-이벤트-변환-eventtoexportevent)
16. [Prometheus 메트릭](#16-prometheus-메트릭)
17. [설계 결정 분석 (Why)](#17-설계-결정-분석-why)

---

## 1. 개요

Hubble 익스포터 시스템은 Hubble에서 수집한 네트워크 플로우와 이벤트를 **파일, 표준 출력
등 외부 대상으로 내보내는** 서브시스템이다. 단순한 로그 기록을 넘어서 **필터링, 필드
마스킹, 플로우 집계, 동적 설정 리로드, 파일 로테이션** 등 프로덕션급 기능을 제공한다.

```
소스 코드 위치:
  pkg/hubble/exporter/exporter.go        -- 정적 익스포터 핵심
  pkg/hubble/exporter/dynamic_exporter.go -- 동적 익스포터
  pkg/hubble/exporter/aggregator.go      -- 플로우 집계
  pkg/hubble/exporter/encoder.go         -- 인코더 인터페이스
  pkg/hubble/exporter/writer.go          -- 라이터 (파일/stdout)
  pkg/hubble/exporter/option.go          -- 옵션/설정
  pkg/hubble/exporter/config.go          -- 동적 설정 파서
  pkg/hubble/exporter/config_watcher.go  -- 설정 파일 감시
  pkg/hubble/exporter/metrics.go         -- Prometheus 메트릭
```

### 핵심 기능 요약

| 기능 | 설명 | 관련 파일 |
|------|------|----------|
| 필터링 | AllowList/DenyList로 내보낼 이벤트 선별 | option.go |
| 필드 마스킹 | FieldMask로 특정 필드만 내보내기 | option.go |
| 플로우 집계 | FieldAggregate로 동일 패턴 플로우 카운트 | aggregator.go |
| 파일 로테이션 | lumberjack 기반 크기/개수 제한 | writer.go |
| 동적 리로드 | YAML 설정 변경 시 자동 적용 | dynamic_exporter.go |
| 훅 시스템 | OnExportEvent로 파이프라인 확장 | exporter.go |
| JSON 인코딩 | ExportEvent를 JSON으로 직렬화 | encoder.go |

---

## 2. 익스포터 아키텍처

### 전체 구성도

```
┌─────────────────────────────────────────────────────────────┐
│                    Hubble Exporter System                    │
│                                                             │
│  ┌──────────────────────────────────────────────────────┐  │
│  │           dynamicExporter (동적 익스포터)              │  │
│  │                                                      │  │
│  │  ┌─────────────────┐    ┌──────────────────────────┐│  │
│  │  │ ConfigWatcher   │───→│ onConfigReload()         ││  │
│  │  │ (5초 주기 감시)  │    │  add / update / remove   ││  │
│  │  └─────────────────┘    └──────────────────────────┘│  │
│  │                                                      │  │
│  │  managedExporters map[string]*managedExporter        │  │
│  │  ┌────────────┐ ┌────────────┐ ┌────────────┐      │  │
│  │  │ exporter-1 │ │ exporter-2 │ │ exporter-N │      │  │
│  │  └─────┬──────┘ └─────┬──────┘ └─────┬──────┘      │  │
│  └────────┼──────────────┼──────────────┼──────────────┘  │
│           │              │              │                   │
│           ▼              ▼              ▼                   │
│  ┌─────────────────────────────────────────────────────┐   │
│  │              개별 exporter 파이프라인                  │   │
│  │                                                     │   │
│  │  Event → Filter → OnExportEvent → Aggregate? →     │   │
│  │          (Allow/   (훅 체인)       (집계 시)         │   │
│  │           Deny)                                     │   │
│  │                                      ↓              │   │
│  │                              eventToExportEvent     │   │
│  │                                      ↓              │   │
│  │                              FieldMask 적용          │   │
│  │                                      ↓              │   │
│  │                              Encoder.Encode         │   │
│  │                                      ↓              │   │
│  │                              Writer (파일/stdout)    │   │
│  └─────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

### 두 가지 익스포터 모드

```
[1. 정적 익스포터 (exporter)]
  - 코드에서 직접 생성
  - 런타임에 설정 변경 불가
  - 단일 출력 대상

[2. 동적 익스포터 (dynamicExporter)]
  - YAML 설정 파일로 정의
  - 5초마다 설정 변경 감지 → 자동 리로드
  - 여러 개의 정적 익스포터를 관리
  - 각 익스포터가 독립적인 필터/출력 설정
```

---

## 3. FlowLogExporter 인터페이스

```go
// pkg/hubble/exporter/exporter.go

type FlowLogExporter interface {
    // Export exports the received event.
    Export(ctx context.Context, ev *v1.Event) error

    // Stop stops this exporter instance from further events processing.
    Stop() error
}
```

이 인터페이스는 익스포터 시스템의 모든 구현체가 따르는 계약이다.

| 구현체 | 설명 |
|--------|------|
| `*exporter` | 단일 출력 대상에 이벤트를 기록하는 정적 익스포터 |
| `*dynamicExporter` | 여러 정적 익스포터를 관리하는 동적 래퍼 |

두 구현체 모두 `FlowLogExporter` 인터페이스를 만족한다:

```go
var _ FlowLogExporter = (*exporter)(nil)
var _ FlowLogExporter = (*dynamicExporter)(nil)
```

---

## 4. 정적 익스포터 (exporter)

### 구조체 정의

```go
// pkg/hubble/exporter/exporter.go

type exporter struct {
    logger     *slog.Logger       // 로깅
    encoder    Encoder            // JSON 인코더
    writer     io.WriteCloser     // 출력 대상 (파일/stdout)
    flow       *flowpb.Flow       // FieldMask용 재사용 객체
    aggregator *AggregatorRunner  // 플로우 집계 (옵션)
    opts       Options            // 설정
}
```

### 생성 과정

```go
func NewExporter(logger *slog.Logger, options ...Option) (*exporter, error) {
    opts := DefaultOptions
    for _, opt := range options {
        if err := opt(&opts); err != nil {
            return nil, fmt.Errorf("failed to apply option: %w", err)
        }
    }
    return newExporter(logger, opts)
}

func newExporter(logger *slog.Logger, opts Options) (*exporter, error) {
    // 1. Writer 생성 (파일 또는 stdout)
    writer, err := opts.NewWriterFunc()()
    if err != nil {
        return nil, fmt.Errorf("failed to create writer: %w", err)
    }
    // 2. Encoder 생성 (기본: JSON)
    encoder, err := opts.NewEncoderFunc()(writer)
    if err != nil {
        writer.Close()
        return nil, fmt.Errorf("failed to create encoder: %w", err)
    }
    // 3. FieldMask 설정 (활성화 시 재사용 Flow 객체 할당)
    var flow *flowpb.Flow
    if opts.FieldMask.Active() {
        flow = new(flowpb.Flow)
        opts.FieldMask.Alloc(flow.ProtoReflect())
    }
    // 4. Aggregator 설정 (활성화 시)
    ex := &exporter{logger: logger, encoder: encoder, writer: writer, flow: flow, opts: opts}
    if opts.FieldAggregate.Active() && opts.aggregationInterval > 0 {
        ex.aggregator = &AggregatorRunner{
            aggregator: NewAggregatorWithFields(opts.FieldAggregate, logger),
            interval:   opts.aggregationInterval,
            encoder:    encoder,
            logger:     logger,
        }
        ex.aggregator.Start()
    }
    return ex, nil
}
```

```
[exporter 생성 단계]

  1. DefaultOptions 복사
     │ Writer: StdoutNoOpWriter
     │ Encoder: JsonEncoder
     ▼
  2. 사용자 Option 적용
     │ AllowList, DenyList, FieldMask, Writer 등
     ▼
  3. Writer 인스턴스 생성
     │ FileWriter(lumberjack) 또는 StdoutNoOpWriter
     ▼
  4. Encoder 인스턴스 생성
     │ json.NewEncoder(writer)
     ▼
  5. FieldMask 초기화
     │ 재사용 Flow 객체 + 필드 할당
     ▼
  6. Aggregator 시작 (옵션)
     │ FieldAggregate.Active() && interval > 0
     ▼
  7. exporter 반환
```

### Stop (정지)

```go
func (e *exporter) Stop() error {
    if e.aggregator != nil {
        e.aggregator.Stop() // 1. 집계 고루틴 정지 (마지막 플러시)
    }
    if e.writer == nil {
        return nil // 이미 정지됨
    }
    err := e.writer.Close() // 2. 파일 핸들 닫기
    e.writer = nil           // 3. 재정지 방지
    return err
}
```

**중요**: 주석에 "Stopped instances cannot be restarted and should be re-created"라고
명시되어 있다. `writer`를 `nil`로 설정하면 재사용이 불가능하다.

---

## 5. Export 파이프라인

`Export` 메서드는 이벤트를 받아서 여러 단계의 처리를 거쳐 최종 출력한다.

```go
// pkg/hubble/exporter/exporter.go

func (e *exporter) Export(ctx context.Context, ev *v1.Event) error {
    // 0. 컨텍스트 취소 확인
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }

    // 1. 필터 적용 (AllowList/DenyList)
    if !filters.Apply(e.opts.AllowFilters(), e.opts.DenyFilters(), ev) {
        return nil // 필터에 의해 제외됨
    }

    // 2. OnExportEvent 훅 체인 실행
    for _, f := range e.opts.OnExportEvent {
        stop, err := f.OnExportEvent(ctx, ev, e.encoder)
        if err != nil {
            e.logger.Warn("OnExportEvent failed", logfields.Error, err)
        }
        if stop {
            return nil // 훅이 파이프라인 중단 요청
        }
    }

    // 3. 집계 처리 (활성화 시, Flow 이벤트만)
    if e.aggregator != nil {
        if _, ok := ev.Event.(*flowpb.Flow); ok {
            e.aggregator.Add(ev) // 집계기에 추가, 즉시 내보내지 않음
            return nil
        }
    }

    // 4. ExportEvent로 변환
    res := e.eventToExportEvent(ev)
    if res == nil {
        return nil // 지원하지 않는 이벤트 타입
    }

    // 5. 인코딩 + 쓰기
    return e.encoder.Encode(res)
}
```

```
[Export 파이프라인 흐름도]

  Event 수신
       │
       ▼
  ┌──────────────────┐     거부
  │ ctx.Done() 확인  │ ──────────→ ctx.Err() 반환
  └────────┬─────────┘
           │ 계속
           ▼
  ┌──────────────────┐     거부
  │ filters.Apply()  │ ──────────→ nil 반환 (무시)
  │ Allow ∩ ¬Deny    │
  └────────┬─────────┘
           │ 허용
           ▼
  ┌──────────────────┐     stop=true
  │ OnExportEvent    │ ──────────→ nil 반환 (중단)
  │ 훅 체인 실행     │
  └────────┬─────────┘
           │ 계속
           ▼
  ┌──────────────────┐     Flow 이벤트
  │ aggregator != nil│ ──────────→ aggregator.Add(ev)
  │ && Flow 타입?    │              nil 반환 (나중에 배치 내보내기)
  └────────┬─────────┘
           │ 비-Flow 또는 aggregator 없음
           ▼
  ┌──────────────────┐     nil
  │ eventToExport-   │ ──────────→ nil 반환 (미지원 타입)
  │ Event() 변환     │
  └────────┬─────────┘
           │ 변환 성공
           ▼
  ┌──────────────────┐
  │ encoder.Encode() │ ──→ JSON 직렬화 + Writer에 쓰기
  └──────────────────┘
```

### 필터 적용 로직

```go
if !filters.Apply(e.opts.AllowFilters(), e.opts.DenyFilters(), ev) {
    return nil
}
```

`filters.Apply`는 11-filter-chain.md에서 설명한 것과 동일한 로직이다:
- `AllowFilters`가 비어있으면 모든 이벤트 허용
- `AllowFilters`가 있으면 하나라도 매칭되어야 허용
- `DenyFilters` 중 하나라도 매칭되면 거부

---

## 6. Encoder 시스템

### Encoder 인터페이스

```go
// pkg/hubble/exporter/encoder.go

type Encoder interface {
    Encode(v any) error
}

type NewEncoderFunc func(writer io.Writer) (Encoder, error)
```

### JSON 인코더 (기본)

```go
func JsonEncoder(writer io.Writer) (Encoder, error) {
    return json.NewEncoder(writer), nil
}
```

Go 표준 라이브러리 `encoding/json`의 `Encoder`를 그대로 사용한다.
`json.Encoder`는 `Encode` 호출마다 줄바꿈(`\n`)을 자동 추가하므로,
출력은 자연스럽게 **JSONL (JSON Lines)** 형식이 된다.

```
[JSON 출력 예시 (각 줄이 하나의 ExportEvent)]

{"time":"2024-01-15T10:30:00Z","nodeName":"node-1","flow":{"source":{"identity":1234},...}}
{"time":"2024-01-15T10:30:01Z","nodeName":"node-1","flow":{"source":{"identity":5678},...}}
{"time":"2024-01-15T10:30:02Z","nodeName":"node-2","lostEvents":{"source":"PERF_EVENT_RING","numEventsLost":5}}
```

### 기본 옵션

```go
// pkg/hubble/exporter/option.go

var DefaultOptions = Options{
    newWriterFunc:  StdoutNoOpWriter,  // 기본: 표준 출력
    newEncoderFunc: JsonEncoder,       // 기본: JSON
}
```

---

## 7. Writer 시스템과 파일 로테이션

### Writer 인터페이스

```go
// pkg/hubble/exporter/writer.go

type NewWriterFunc func() (io.WriteCloser, error)
```

Writer는 `io.WriteCloser` 인터페이스를 만족하면 어떤 출력 대상이든 사용할 수 있다.

### StdoutNoOpWriter (표준 출력)

```go
func StdoutNoOpWriter() (io.WriteCloser, error) {
    return &noopWriteCloser{os.Stdout}, nil
}

type noopWriteCloser struct {
    w io.Writer
}

func (nwc *noopWriteCloser) Write(p []byte) (int, error) {
    return nwc.w.Write(p)
}

func (nwc *noopWriteCloser) Close() error {
    return nil // stdout은 닫지 않음
}
```

`os.Stdout`을 감싸되 `Close()`를 no-op으로 만든다.
stdout은 프로세스 수명 동안 유지되어야 하므로 닫으면 안 된다.

### FileWriter (파일 로테이션)

```go
type FileWriterConfig struct {
    Filename   string     // 파일 경로
    MaxSize    int        // 최대 크기 (MB)
    MaxAge     int        // 보관 일수
    MaxBackups int        // 백업 파일 최대 개수
    LocalTime  bool       // 로컬 시간 사용 여부
    Compress   bool       // gzip 압축 여부
    FileMode   fs.FileMode // 파일 퍼미션
}

func FileWriter(config FileWriterConfig) func() (io.WriteCloser, error) {
    return func() (io.WriteCloser, error) {
        return &lumberjack.Logger{
            Filename:   config.Filename,
            MaxSize:    config.MaxSize,
            MaxBackups: config.MaxBackups,
            Compress:   config.Compress,
        }, nil
    }
}
```

`lumberjack.Logger`는 자동 파일 로테이션을 제공하는 서드파티 라이브러리이다.

```
[파일 로테이션 동작]

  /var/run/hubble/flows.log         (현재 로그, 최대 10MB)
       │
       │ MaxSize(10MB) 초과 시
       ▼
  /var/run/hubble/flows.log         (새 파일)
  /var/run/hubble/flows-20240115-1.log  (백업 1)
       │
       │ 다시 초과 시
       ▼
  /var/run/hubble/flows.log         (새 파일)
  /var/run/hubble/flows-20240115-2.log  (백업 2)
  /var/run/hubble/flows-20240115-1.log  (백업 1)
       │
       │ MaxBackups(5) 초과 시
       ▼
  가장 오래된 백업 삭제
  Compress=true이면 .gz로 압축
```

### 기본 파일 설정값

```go
const (
    DefaultFileMaxSizeMB  = 10  // 파일당 최대 10MB
    DefaultFileMaxBackups = 5   // 백업 파일 최대 5개
)
```

---

## 8. FieldMask (필드 마스킹)

FieldMask는 `Flow` 이벤트에서 **특정 필드만** 내보내는 기능이다.
대용량 플로우 로그에서 불필요한 필드를 제거하여 저장 공간과 네트워크 대역폭을 절약한다.

### FieldMask 옵션 설정

```go
// pkg/hubble/exporter/option.go

func WithFieldMask(paths []string) Option {
    return func(o *Options) error {
        fm, err := fieldmaskpb.New(&flowpb.Flow{}, paths...)
        if err != nil {
            return err
        }
        fieldMask, err := fieldmask.New(fm)
        if err != nil {
            return err
        }
        o.FieldMask = fieldMask
        return nil
    }
}
```

### FieldMask 적용 과정

```go
// exporter.go - newExporter
var flow *flowpb.Flow
if opts.FieldMask.Active() {
    flow = new(flowpb.Flow)
    opts.FieldMask.Alloc(flow.ProtoReflect()) // 필드 미리 할당
}

// exporter.go - eventToExportEvent
case *flowpb.Flow:
    if e.opts.FieldMask.Active() {
        e.opts.FieldMask.Copy(e.flow.ProtoReflect(), ev.ProtoReflect())
        ev = e.flow // 마스킹된 Flow 사용
    }
```

```
[FieldMask 동작 원리]

  원본 Flow 이벤트:
  {
    "source": {"identity": 1234, "namespace": "default", "labels": [...]},
    "destination": {"identity": 5678, "namespace": "kube-system", "labels": [...]},
    "ip": {"source": "10.0.1.1", "destination": "10.0.2.2"},
    "l4": {"TCP": {"source_port": 54321, "destination_port": 80}},
    "verdict": "FORWARDED",
    "type": "L3_L4",
    "node_name": "node-1",
    "time": "2024-01-15T10:30:00Z",
    "trace_observation_point": "TO_ENDPOINT",
    ...  (수십 개의 필드)
  }

  FieldMask: ["source.identity", "destination.identity", "verdict"]
       ↓
  마스킹된 Flow:
  {
    "source": {"identity": 1234},
    "destination": {"identity": 5678},
    "verdict": "FORWARDED"
  }
```

### 재사용 객체 패턴

`e.flow`는 exporter 생성 시 한 번만 할당되고, `Export` 호출마다 재사용된다.
매 이벤트마다 새 `Flow` 객체를 만들지 않으므로 **GC 압력을 최소화**한다.

```go
// 생성 시 (1번)
flow = new(flowpb.Flow)
opts.FieldMask.Alloc(flow.ProtoReflect())

// Export 호출마다 (N번) - 새 할당 없이 필드만 복사
e.opts.FieldMask.Copy(e.flow.ProtoReflect(), ev.ProtoReflect())
ev = e.flow
```

---

## 9. Aggregator (플로우 집계)

### 왜 집계가 필요한가?

Kubernetes 클러스터에서는 동일한 서비스 간의 통신이 초당 수천~수만 번 발생한다.
모든 개별 플로우를 기록하면:
- 디스크 용량 급속 소진
- 로그 분석 시 노이즈
- 실질적으로 동일한 정보의 반복

집계를 사용하면 "10.0.1.1 → 10.0.2.2, port 80, FORWARDED"가 10초간 1000번 발생했다면,
**하나의 레코드**에 `IngressFlowCount: 1000`으로 요약된다.

### Aggregator 구조체

```go
// pkg/hubble/exporter/aggregator.go

type AggregateKey string

type AggregateValue struct {
    IngressFlowCount          int           // 인그레스 방향 플로우 수
    EgressFlowCount           int           // 이그레스 방향 플로우 수
    UnknownDirectionFlowCount int           // 방향 불명 플로우 수
    ProcessedFlow             *flowpb.Flow  // 집계 대표 플로우
}

type Aggregator struct {
    m              map[AggregateKey]*AggregateValue  // 집계 맵
    mu             lock.RWMutex                       // 동시성 보호
    fieldAggregate fa.FieldAggregate                  // 집계 필드 정의
    logger         *slog.Logger
}
```

### 집계 키 생성

```go
func generateAggregationKey(processedFlow *flowpb.Flow) AggregateKey {
    b, _ := proto.Marshal(processedFlow.ProtoReflect().Interface())
    return AggregateKey(b)
}
```

**핵심 아이디어**: `FieldAggregate`로 선택된 필드만 가진 `Flow`를 protobuf로 직렬화한
바이트열 자체를 맵 키로 사용한다. 같은 필드 값을 가진 플로우는 같은 키를 생성하므로
자연스럽게 그룹화된다.

```
[집계 키 생성 과정]

  원본 Flow:
  {source: {identity: 1234, pod: "pod-a"}, dest: {identity: 5678},
   ip: {src: "10.0.1.1", dst: "10.0.2.2"}, verdict: "FORWARDED",
   l4: {TCP: {dst_port: 80, src_port: 54321}}}

  FieldAggregate: ["source.identity", "destination.identity", "verdict"]
       ↓
  processedFlow (집계 필드만):
  {source: {identity: 1234}, dest: {identity: 5678}, verdict: "FORWARDED"}
       ↓
  proto.Marshal() → 바이트열 → AggregateKey
       ↓
  같은 source.identity + dest.identity + verdict 조합의
  모든 플로우가 동일한 키를 가짐
```

### Add 메서드

```go
func (a *Aggregator) Add(ev *v1.Event) {
    f := ev.GetFlow()
    if f == nil { return }

    // 1. 집계 필드만 복사한 processedFlow 생성
    processedFlow := &flowpb.Flow{}
    a.fieldAggregate.Copy(processedFlow.ProtoReflect(), f.ProtoReflect())

    // 2. 집계 키 생성 (타임스탬프 제외)
    k := generateAggregationKey(processedFlow)

    // 3. 타임스탬프는 키 생성 후 추가 (시간 컨텍스트 보존)
    processedFlow.Time = f.GetTime()

    // 4. 집계 맵에 추가/갱신
    a.mu.Lock()
    defer a.mu.Unlock()
    v, ok := a.m[k]
    if !ok {
        // 새 키 → 새 AggregateValue 생성
        switch f.GetTrafficDirection() {
        case flowpb.TrafficDirection_INGRESS:
            v = &AggregateValue{IngressFlowCount: 1, ProcessedFlow: processedFlow}
        case flowpb.TrafficDirection_EGRESS:
            v = &AggregateValue{EgressFlowCount: 1, ProcessedFlow: processedFlow}
        default:
            v = &AggregateValue{UnknownDirectionFlowCount: 1, ProcessedFlow: processedFlow}
        }
        a.m[k] = v
    } else {
        // 기존 키 → 카운트 증가만
        switch f.GetTrafficDirection() {
        case flowpb.TrafficDirection_INGRESS:  v.IngressFlowCount++
        case flowpb.TrafficDirection_EGRESS:   v.EgressFlowCount++
        default:                               v.UnknownDirectionFlowCount++
        }
    }
}
```

**타임스탬프를 키에 포함하지 않는 이유**:

코드 주석에 "Enrich the processed flow with timestamp after key generation. This ensures
timestamp doesn't affect aggregation, but preserves temporal context."라고 설명되어 있다.
타임스탬프가 키에 포함되면 모든 플로우가 고유한 키를 가지게 되어 집계 효과가 사라진다.

### Export 메서드

```go
func (a *Aggregator) Export(encoder Encoder) {
    a.mu.Lock()
    defer a.mu.Unlock()

    for _, value := range a.m {
        exportEvent := processedFlowToAggregatedExportEvent(
            value.ProcessedFlow,
            value.IngressFlowCount,
            value.EgressFlowCount,
            value.UnknownDirectionFlowCount,
        )
        if exportEvent == nil { continue }
        if err := encoder.Encode(exportEvent); err != nil {
            a.logger.Error("Failed to export aggregate", logfields.Error, err)
        }
    }

    // 맵 초기화 (새 집계 시작)
    a.m = make(map[AggregateKey]*AggregateValue)
}
```

### processedFlowToAggregatedExportEvent

```go
func processedFlowToAggregatedExportEvent(processedFlow *flowpb.Flow,
    ingressCount, egressCount, unknownDirectionFlowCount int) *observerpb.ExportEvent {

    aggregate := &flowpb.Aggregate{
        IngressFlowCount:          uint32(ingressCount),
        EgressFlowCount:           uint32(egressCount),
        UnknownDirectionFlowCount: uint32(unknownDirectionFlowCount),
    }
    processedFlow.Aggregate = aggregate // Flow에 집계 정보 추가

    return &observerpb.ExportEvent{
        Time:     processedFlow.GetTime(),
        NodeName: processedFlow.GetNodeName(),
        ResponseTypes: &observerpb.ExportEvent_Flow{
            Flow: processedFlow,
        },
    }
}
```

```
[집계된 ExportEvent 출력 예시]

{
  "time": "2024-01-15T10:30:10Z",
  "nodeName": "node-1",
  "flow": {
    "source": {"identity": 1234},
    "destination": {"identity": 5678},
    "verdict": "FORWARDED",
    "aggregate": {
      "ingressFlowCount": 847,
      "egressFlowCount": 153,
      "unknownDirectionFlowCount": 0
    }
  }
}
```

---

## 10. AggregatorRunner (주기적 내보내기)

### 구조체

```go
// pkg/hubble/exporter/aggregator.go

type AggregatorRunner struct {
    aggregator *Aggregator      // 실제 집계 로직
    interval   time.Duration    // 내보내기 주기
    encoder    Encoder          // 출력 인코더
    logger     *slog.Logger

    stop chan struct{}           // 종료 신호
    wg   sync.WaitGroup         // 고루틴 동기화
}
```

### 라이프사이클

```go
func (r *AggregatorRunner) Start() {
    if r.stop != nil {
        r.logger.Error("AggregatorRunner is already started.")
        return
    }
    r.stop = make(chan struct{})
    r.wg.Add(1)
    go r.run()
}

func (r *AggregatorRunner) Stop() {
    if r.stop != nil {
        close(r.stop)
    }
    r.wg.Wait()      // 마지막 플러시 완료 대기
    r.stop = nil
}
```

### run 루프

```go
func (r *AggregatorRunner) run() {
    defer r.wg.Done()
    ticker := time.NewTicker(r.interval)
    for {
        select {
        case <-r.stop:
            r.exportAggregates() // 종료 전 마지막 플러시
            return
        case <-ticker.C:
            r.exportAggregates() // 주기적 내보내기
        }
    }
}
```

```
[AggregatorRunner 타임라인]

  시간   0s ─────── 10s ─────── 20s ─────── 30s ── Stop
         │          │           │           │       │
  Add:   F1,F2,F3   F4,F5      F6          F7,F8   │
         │          │           │           │       │
  Export: ──────────→ 배치1      배치2       배치3   배치4
                     (F1-F3     (F4-F5      (F6     (F7-F8
                      집계)      집계)       집계)   최종 플러시)
```

**Stop 시 마지막 플러시가 중요한 이유**: Stop 호출 시점에 아직 내보내지 않은 집계
데이터가 있을 수 있다. `r.exportAggregates()`를 Stop에서 한 번 더 호출하여
데이터 손실을 방지한다.

---

## 11. 동적 익스포터 (dynamicExporter)

### 구조체

```go
// pkg/hubble/exporter/dynamic_exporter.go

type dynamicExporter struct {
    logger          *slog.Logger
    watcher         *configWatcher          // 설정 파일 감시
    exporterFactory ExporterFactory         // 익스포터 팩토리

    mu               lock.RWMutex
    managedExporters map[string]*managedExporter // 관리 중인 익스포터 맵
}

type managedExporter struct {
    config   ExporterConfig     // 현재 설정
    exporter FlowLogExporter    // 실행 중인 익스포터
}
```

### 생성

```go
func NewDynamicExporter(logger *slog.Logger, configFilePath string,
    exporterFactory ExporterFactory,
    exporterConfigParser ExporterConfigParser) *dynamicExporter {

    dynamicExporter := &dynamicExporter{
        logger:           logger,
        exporterFactory:  exporterFactory,
        managedExporters: make(map[string]*managedExporter),
    }

    // ConfigWatcher 생성 (초기 로드 포함)
    watcher := NewConfigWatcher(logger, configFilePath, exporterConfigParser,
        func(configs map[string]ExporterConfig, hash uint64) {
            if err := dynamicExporter.onConfigReload(configs, hash); err != nil {
                logger.Error("Failed to reload exporter manager", logfields.Error, err)
            }
        })
    dynamicExporter.watcher = watcher

    registerMetrics(dynamicExporter) // Prometheus 메트릭 등록
    return dynamicExporter
}
```

### Export (팬아웃)

```go
func (d *dynamicExporter) Export(ctx context.Context, ev *v1.Event) error {
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }

    d.mu.RLock()
    defer d.mu.RUnlock()

    var errs error
    for _, me := range d.managedExporters {
        errs = errors.Join(errs, me.exporter.Export(ctx, ev))
    }
    return errs
}
```

하나의 이벤트를 **모든 관리 중인 익스포터에 동시에 전달**한다 (팬아웃 패턴).
각 익스포터는 독립적인 필터를 가지므로, 같은 이벤트가 어떤 익스포터에서는 기록되고
다른 익스포터에서는 무시될 수 있다.

```
[동적 익스포터 팬아웃]

  Event ──┐
          │
          ├──→ exporter-1 (DNS 플로우 → /var/log/dns-flows.log)
          │    AllowFilter: source_fqdn="*.cluster.local"
          │
          ├──→ exporter-2 (거부 플로우 → /var/log/dropped.log)
          │    AllowFilter: verdict=DROPPED
          │
          └──→ exporter-3 (전체 플로우 → stdout)
               AllowFilter: (없음, 전체)
```

### onConfigReload (설정 변경 처리)

```go
func (d *dynamicExporter) onConfigReload(configs map[string]ExporterConfig,
    hash uint64) error {

    d.mu.Lock()
    defer d.mu.Unlock()

    // 1. 새로운/변경된 익스포터 적용
    configuredExporterNames := make(map[string]struct{})
    for name, config := range configs {
        configuredExporterNames[name] = struct{}{}
        var label string
        if _, ok := d.managedExporters[name]; ok {
            label = "update"
        } else {
            label = "add"
        }
        if d.applyUpdatedConfig(name, config) {
            DynamicExporterReconfigurations.WithLabelValues(label).Inc()
        }
    }

    // 2. 설정에서 삭제된 익스포터 제거
    for name := range d.managedExporters {
        if _, ok := configuredExporterNames[name]; !ok {
            if d.removeExporter(name) {
                DynamicExporterReconfigurations.WithLabelValues("remove").Inc()
            }
        }
    }

    // 3. 메트릭 갱신
    DynamicExporterConfigHash.WithLabelValues().Set(float64(hash))
    DynamicExporterConfigLastApplied.WithLabelValues().SetToCurrentTime()
    return nil
}
```

```
[설정 리로드 3가지 시나리오]

  현재 관리 중: {A, B, C}
  새 설정:     {A', B, D}

  1. A → A' (변경):
     - applyUpdatedConfig("A", A')
     - config.Equal(A') == false → 기존 A 정지 + 새 A' 생성
     - 메트릭: reconfigurations{op="update"}++

  2. B → B (동일):
     - applyUpdatedConfig("B", B)
     - config.Equal(B) == true → 아무것도 안함
     - 메트릭: 변화 없음

  3. C (삭제):
     - configuredExporterNames에 "C" 없음
     - removeExporter("C") → C.Stop() + 맵에서 삭제
     - 메트릭: reconfigurations{op="remove"}++

  4. D (추가):
     - d.managedExporters에 "D" 없음
     - applyUpdatedConfig("D", D) → 새 익스포터 생성
     - 메트릭: reconfigurations{op="add"}++
```

### applyUpdatedConfig (설정 적용)

```go
func (d *dynamicExporter) applyUpdatedConfig(name string, config ExporterConfig) bool {
    me, ok := d.managedExporters[name]
    if ok && me.config.Equal(config) {
        return false // 변경 없음
    }

    exporter, err := d.exporterFactory.Create(config)
    if err != nil {
        d.logger.Error("Failed to create exporter for config",
            logfields.Error, err, logfields.Name, name)
        return false
    }

    d.removeExporter(name)     // 기존 익스포터 정지 + 삭제
    d.managedExporters[name] = &managedExporter{
        config:   config,
        exporter: exporter,
    }
    return true
}
```

---

## 12. 설정 파일 구조 (FlowLogConfig)

### DynamicExportersConfig

```go
// pkg/hubble/exporter/config.go

type DynamicExportersConfig struct {
    FlowLogs []*FlowLogConfig `json:"flowLogs" yaml:"flowLogs"`
}
```

### FlowLogConfig 구조체

```go
type FlowLogConfig struct {
    Name                string         `yaml:"name"`
    FilePath            string         `yaml:"filePath"`
    FieldMask           FieldMask      `yaml:"fieldMask"`
    FieldAggregate      FieldAggregate `yaml:"fieldAggregate"`
    AggregationInterval Duration       `yaml:"aggregationInterval"`
    IncludeFilters      FlowFilters    `yaml:"includeFilters"`
    ExcludeFilters      FlowFilters    `yaml:"excludeFilters"`
    FileMaxSizeMB       int            `yaml:"fileMaxSizeMb"`
    FileMaxBackups      int            `yaml:"fileMaxBackups"`
    FileCompress        bool           `yaml:"fileCompress"`
    End                 *time.Time     `yaml:"end"`
}
```

### YAML 설정 파일 예시

```yaml
flowLogs:
  - name: dns-flowlog
    filePath: /var/run/hubble/dns-flows.log
    fieldMask:
      - source.identity
      - source.namespace
      - destination.identity
      - destination.namespace
      - source_names
      - destination_names
      - verdict
      - l7.dns
    includeFilters:
      - source_fqdn:
          - "*.cluster.local"
    fileMaxSizeMb: 10
    fileMaxBackups: 5
    fileCompress: true

  - name: dropped-flowlog
    filePath: /var/run/hubble/dropped-flows.log
    includeFilters:
      - verdict:
          - DROPPED
    fieldAggregate:
      - source.identity
      - destination.identity
      - verdict
    aggregationInterval: "10s"
    fileMaxSizeMb: 20
    fileMaxBackups: 3

  - name: all-flowlog
    filePath: stdout
    end: "2024-03-01T00:00:00Z"
```

### 설정 검증 (Validate)

```go
func (c *DynamicExportersConfig) Validate() error {
    flowlogNames := make(map[string]struct{}, len(c.FlowLogs))
    flowlogPaths := make(map[string]struct{}, len(c.FlowLogs))
    var errs error
    for i := range c.FlowLogs {
        if c.FlowLogs[i] == nil {
            errs = errors.Join(errs, fmt.Errorf("invalid flowlog at index %d", i))
            continue
        }
        name := c.FlowLogs[i].Name
        if name == "" {
            errs = errors.Join(errs, fmt.Errorf("name is required"))
        } else if _, ok := flowlogNames[name]; ok {
            errs = errors.Join(errs, fmt.Errorf("duplicated flowlog name %s", name))
        }
        // ... filePath 중복 검사도 동일
    }
    return errs
}
```

검증 규칙:
1. `name`은 필수이고 고유해야 함
2. `filePath`는 필수이고 고유해야 함
3. nil 항목이 있으면 안 됨

### IsActive (만료 체크)

```go
func (f *FlowLogConfig) IsActive() bool {
    return f.End == nil || f.End.After(time.Now())
}
```

`End` 필드가 설정되면 해당 시각 이후에는 이벤트를 내보내지 않는다.
이를 통해 **일시적 디버깅 로그**를 안전하게 설정할 수 있다.

### Duration 커스텀 타입

```go
type Duration time.Duration

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
    var s string
    if err := unmarshal(&s); err != nil { return err }
    duration, err := time.ParseDuration(s)
    if err != nil { return err }
    *d = Duration(duration)
    return nil
}
```

Go의 `time.Duration`은 YAML에서 문자열로 파싱할 수 없으므로, `"10s"`, `"5m"` 같은
문자열을 `time.ParseDuration`으로 변환하는 커스텀 타입이다.

---

## 13. ConfigWatcher (설정 감시)

### 구조체

```go
// pkg/hubble/exporter/config_watcher.go

type configWatcher struct {
    logger         *slog.Logger
    configFilePath string
    configParser   ExporterConfigParser
    callback       configWatcherCallback
}

type configWatcherCallback func(configs map[string]ExporterConfig, hash uint64)
```

### 감시 루프

```go
var reloadInterval = 5 * time.Second

func (c *configWatcher) watch(ctx context.Context, interval time.Duration) error {
    ticker := time.NewTicker(interval) // 기본 5초
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-ticker.C:
            c.reload()
        }
    }
}
```

### 리로드 과정

```go
func (c *configWatcher) reload() {
    configs, hash, err := c.parseConfig()
    if err != nil {
        DynamicExporterReconfigurations.WithLabelValues("failure").Inc()
        c.logger.Error("Failed to parse dynamic exporter config", logfields.Error, err)
        return
    }
    c.callback(configs, hash) // onConfigReload 호출
}

func (c *configWatcher) parseConfig() (map[string]ExporterConfig, uint64, error) {
    // 1. 파일 읽기
    content, err := os.ReadFile(c.configFilePath)
    if err != nil { return nil, 0, err }

    // 2. YAML 파싱
    configs, err := c.configParser.Parse(bytes.NewReader(content))
    if err != nil { return nil, 0, err }

    // 3. MD5 해시 계산
    hash := calculateHash(content)
    return configs, hash, nil
}

func calculateHash(file []byte) uint64 {
    sum := md5.Sum(file)
    return binary.LittleEndian.Uint64(sum[0:16])
}
```

```
[ConfigWatcher 동작 흐름]

  ┌──────────────────────────────────────────┐
  │  5초 타이머                               │
  │     │                                     │
  │     ▼                                     │
  │  os.ReadFile(configFilePath)              │
  │     │                                     │
  │     ├── 실패 → 에러 로그 + failure 메트릭  │
  │     │                                     │
  │     ▼                                     │
  │  configParser.Parse(content)              │
  │     │                                     │
  │     ├── 실패 → 에러 로그 + failure 메트릭  │
  │     │                                     │
  │     ▼                                     │
  │  calculateHash(content) → MD5 해시        │
  │     │                                     │
  │     ▼                                     │
  │  callback(configs, hash)                  │
  │  → dynamicExporter.onConfigReload()       │
  └──────────────────────────────────────────┘
```

**TODO 주석**: 코드에 `// TODO replace ticker reloads with inotify watchers`라는
주석이 있다. 현재는 폴링 방식(5초마다 파일 읽기)이지만, 향후 inotify/fsnotify 기반의
이벤트 구동 방식으로 전환할 계획이다.

### 초기 로드

```go
func NewConfigWatcher(...) *configWatcher {
    watcher := &configWatcher{...}
    watcher.reload() // 생성 즉시 한 번 로드
    return watcher
}
```

`Watch`를 시작하기 전에 `NewConfigWatcher`에서 이미 첫 번째 로드를 수행한다.
이로써 **초기 설정 없이 서비스가 시작되는 상황**을 방지한다.

---

## 14. OnExportEvent 훅 시스템

### 인터페이스

```go
// pkg/hubble/exporter/exporter.go

type OnExportEvent interface {
    OnExportEvent(ctx context.Context, ev *v1.Event, encoder Encoder) (stop bool, err error)
}
```

| 반환값 | 의미 |
|--------|------|
| `stop=false, err=nil` | 정상, 다음 훅/기본 처리 계속 |
| `stop=true, err=nil` | 파이프라인 중단 (이 이벤트 무시) |
| `stop=false, err!=nil` | 경고 로그 후 계속 |
| `stop=true, err!=nil` | 경고 로그 후 파이프라인 중단 |

### 함수형 구현

```go
type OnExportEventFunc func(ctx context.Context, ev *v1.Event,
    encoder Encoder) (stop bool, err error)

func (f OnExportEventFunc) OnExportEvent(ctx context.Context, ev *v1.Event,
    encoder Encoder) (bool, error) {
    return f(ctx, ev, encoder)
}
```

### 동적 익스포터에서의 활용

```go
// config.go - exporterFactory.create()
WithOnExportEventFunc(func(ctx context.Context, ev *v1.Event,
    encoder Encoder) (bool, error) {
    stop := !config.IsActive()  // End 시각 지난 경우 stop=true
    return stop, nil
}),
```

이 훅은 `FlowLogConfig.End` 필드를 구현하는 데 사용된다. 설정된 종료 시각이 지나면
`IsActive()`가 `false`를 반환하여 `stop=true`가 되고, 해당 이벤트는 기록되지 않는다.

### 훅 체인 실행 순서

```go
for _, f := range e.opts.OnExportEvent {
    stop, err := f.OnExportEvent(ctx, ev, e.encoder)
    if err != nil {
        e.logger.Warn("OnExportEvent failed", logfields.Error, err)
    }
    if stop {
        return nil // 파이프라인 중단
    }
}
```

- 훅은 등록 순서대로 실행
- 하나의 훅이 `stop=true`를 반환하면 **이후 훅은 실행되지 않음**
- 에러가 발생해도 `stop`이 아니면 계속 진행 (탄력적 설계)

---

## 15. 이벤트 변환 (eventToExportEvent)

### 지원 이벤트 타입

```go
func (e *exporter) eventToExportEvent(event *v1.Event) *observerpb.ExportEvent {
    switch ev := event.Event.(type) {
    case *flowpb.Flow:
        // FieldMask 적용 후 ExportEvent_Flow 생성
    case *flowpb.LostEvent:
        // ExportEvent_LostEvents 생성
    case *flowpb.AgentEvent:
        // ExportEvent_AgentEvent 생성
    case *flowpb.DebugEvent:
        // ExportEvent_DebugEvent 생성
    default:
        return nil // 미지원 타입
    }
}
```

### 각 이벤트 타입별 변환

```
[이벤트 타입별 ExportEvent 매핑]

  ┌─────────────────┐    ┌────────────────────────────────┐
  │ *flowpb.Flow    │ →  │ ExportEvent_Flow               │
  │                 │    │  Time: ev.GetTime()             │
  │                 │    │  NodeName: ev.GetNodeName()     │
  │                 │    │  Flow: ev (FieldMask 적용 가능)  │
  └─────────────────┘    └────────────────────────────────┘

  ┌─────────────────┐    ┌────────────────────────────────┐
  │ *flowpb.LostEvent│ → │ ExportEvent_LostEvents         │
  │                 │    │  Time: event.Timestamp          │
  │                 │    │  NodeName: nodeTypes.GetName()  │
  │                 │    │  LostEvents: ev                 │
  └─────────────────┘    └────────────────────────────────┘

  ┌─────────────────┐    ┌────────────────────────────────┐
  │ *flowpb.AgentEvent│→ │ ExportEvent_AgentEvent          │
  │                 │    │  Time: event.Timestamp          │
  │                 │    │  NodeName: nodeTypes.GetName()  │
  │                 │    │  AgentEvent: ev                 │
  └─────────────────┘    └────────────────────────────────┘

  ┌─────────────────┐    ┌────────────────────────────────┐
  │ *flowpb.DebugEvent│→│ ExportEvent_DebugEvent           │
  │                 │    │  Time: event.Timestamp          │
  │                 │    │  NodeName: nodeTypes.GetName()  │
  │                 │    │  DebugEvent: ev                 │
  └─────────────────┘    └────────────────────────────────┘
```

**Flow vs 비-Flow 이벤트의 차이**:
- `Flow`: 이벤트 자체에 `Time`과 `NodeName`이 포함됨 → `ev.GetTime()`, `ev.GetNodeName()`
- 비-Flow: 상위 `Event` 래퍼에서 `Timestamp`, `nodeTypes.GetName()`을 사용

---

## 16. Prometheus 메트릭

### 메트릭 목록

```go
// pkg/hubble/exporter/metrics.go

// 1. 활성/비활성 익스포터 수
var exportersDesc = prometheus.NewDesc(
    "hubble_dynamic_exporter_exporters_total",
    "Number of configured exporters",
    []string{"status"}, nil,  // status: "active" | "inactive"
)

// 2. 개별 익스포터 상태
var individualExportersDesc = prometheus.NewDesc(
    "hubble_dynamic_exporter_up",
    "Status of individual exporters",
    []string{"name"}, nil,  // name: 익스포터 이름
)

// 3. 재구성 횟수
var DynamicExporterReconfigurations = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Name: "hubble_dynamic_exporter_reconfigurations_total",
        Help: "Number of dynamic exporters reconfigurations",
    }, []string{"op"},  // op: "add" | "update" | "remove" | "failure"
)

// 4. 설정 해시
var DynamicExporterConfigHash = prometheus.NewGaugeVec(
    prometheus.GaugeOpts{
        Name: "hubble_dynamic_exporter_config_hash",
        Help: "Hash of last applied config",
    }, []string{},
)

// 5. 마지막 설정 적용 시각
var DynamicExporterConfigLastApplied = prometheus.NewGaugeVec(
    prometheus.GaugeOpts{
        Name: "hubble_dynamic_exporter_config_last_applied",
        Help: "Timestamp of last applied config",
    }, []string{},
)
```

### 동적 Gauge 수집기

```go
type dynamicExporterGaugeCollector struct {
    prometheus.Collector
    exporter *dynamicExporter
}

func (d *dynamicExporterGaugeCollector) Collect(ch chan<- prometheus.Metric) {
    var activeExporters, inactiveExporters float64
    d.exporter.mu.RLock()
    for name, me := range d.exporter.managedExporters {
        var value float64
        if me.config.IsActive() {
            value = 1
            activeExporters++
        } else {
            inactiveExporters++
        }
        ch <- prometheus.MustNewConstMetric(
            individualExportersDesc, prometheus.GaugeValue, value, name)
    }
    d.exporter.mu.RUnlock()

    ch <- prometheus.MustNewConstMetric(
        exportersDesc, prometheus.GaugeValue, activeExporters, "active")
    ch <- prometheus.MustNewConstMetric(
        exportersDesc, prometheus.GaugeValue, inactiveExporters, "inactive")
}
```

`dynamicExporterGaugeCollector`는 `Collect` 호출 시점에 **실시간으로** 관리 중인
익스포터 목록을 읽어서 메트릭을 생성한다. 이로써 익스포터가 동적으로 추가/삭제되어도
Prometheus가 항상 최신 상태를 수집할 수 있다.

### 메트릭 사용 예시

```
# 활성 익스포터 2개, 비활성 1개
hubble_dynamic_exporter_exporters_total{status="active"} 2
hubble_dynamic_exporter_exporters_total{status="inactive"} 1

# 개별 익스포터 상태
hubble_dynamic_exporter_up{name="dns-flowlog"} 1
hubble_dynamic_exporter_up{name="dropped-flowlog"} 1
hubble_dynamic_exporter_up{name="expired-debug"} 0

# 재구성 이력
hubble_dynamic_exporter_reconfigurations_total{op="add"} 3
hubble_dynamic_exporter_reconfigurations_total{op="update"} 1
hubble_dynamic_exporter_reconfigurations_total{op="remove"} 0
hubble_dynamic_exporter_reconfigurations_total{op="failure"} 0

# 설정 해시 (변경 감지용)
hubble_dynamic_exporter_config_hash 1.234567890123e+18

# 마지막 설정 적용 (Unix timestamp)
hubble_dynamic_exporter_config_last_applied 1.705312200e+09
```

---

## 17. 설계 결정 분석 (Why)

### Q1: 왜 정적/동적 두 가지 익스포터 모드인가?

**정적 익스포터**는 코드에서 직접 생성하는 간단한 사용 사례(예: Cilium Agent의 기본
로그 출력)에 적합하다. **동적 익스포터**는 운영 중에 디버깅 로그를 추가/제거해야 하는
프로덕션 환경에 필수적이다.

두 모드 모두 `FlowLogExporter` 인터페이스를 구현하므로, 상위 시스템은 어떤 모드를
사용하는지 알 필요가 없다 (다형성).

### Q2: 왜 proto.Marshal을 집계 키로 사용하는가?

```go
func generateAggregationKey(processedFlow *flowpb.Flow) AggregateKey {
    b, _ := proto.Marshal(processedFlow.ProtoReflect().Interface())
    return AggregateKey(b)
}
```

protobuf 직렬화는 **결정적(deterministic)**이다. 같은 필드 값을 가진 메시지는 항상
같은 바이트열로 직렬화된다. 이를 맵 키로 사용하면:
- 별도의 해시 함수 구현 불필요
- 필드 조합이 변경되어도 자동 대응
- 성능: 직렬화 비용은 있지만, 직접 각 필드를 비교하는 것보다 코드가 간결

### Q3: 왜 파일 로테이션에 lumberjack을 사용하는가?

직접 구현하면:
- 파일 크기 모니터링 로직
- 원자적 파일 교체
- 백업 파일 관리/삭제
- gzip 압축 고루틴
- 에지 케이스 (디스크 풀, 권한 오류 등)

이 모든 것을 처리해야 한다. `lumberjack`은 Go 생태계에서 가장 널리 사용되는
로그 로테이션 라이브러리로, 이러한 기능을 안정적으로 제공한다.

### Q4: 왜 타임스탬프를 집계 키에 포함하지 않는가?

타임스탬프가 키에 포함되면 모든 플로우가 고유한 키를 가진다 (동일 나노초에 발생하지
않는 한). 이는 집계의 의미를 완전히 무효화한다. 코드 주석:

> "Enrich the processed flow with timestamp after key generation.
> This ensures timestamp doesn't affect aggregation, but preserves temporal context."

타임스탬프는 키 생성 후에 `processedFlow.Time`에 설정되어 **마지막 발생 시각**만 기록한다.

### Q5: 왜 동적 익스포터에서 errors.Join을 사용하는가?

```go
var errs error
for _, me := range d.managedExporters {
    errs = errors.Join(errs, me.exporter.Export(ctx, ev))
}
```

하나의 익스포터가 실패해도 다른 익스포터는 계속 동작해야 한다.
`errors.Join`은 여러 에러를 하나로 합쳐서 반환하되, nil 에러는 무시한다.
이로써 **부분 실패를 허용**하면서도 호출자에게 모든 에러를 알린다.

### Q6: 왜 MD5를 설정 해시로 사용하는가?

```go
func calculateHash(file []byte) uint64 {
    sum := md5.Sum(file)
    return binary.LittleEndian.Uint64(sum[0:16])
}
```

이 해시는 **보안 목적이 아닌 변경 감지 목적**이다. MD5는:
- 빠른 계산 속도
- 충돌 가능성이 있지만, 같은 파일의 변경 감지에는 충분
- Prometheus 메트릭(`config_hash`)으로 외부에서 설정 변경을 모니터링 가능

### Q7: 왜 폴링 방식인가 (inotify가 아닌)?

코드에 TODO가 있듯이, 현재는 5초마다 파일을 읽는 폴링 방식이다.
- **장점**: 모든 OS에서 동작, 구현이 간단, ConfigMap 마운트 환경에서 안정적
- **단점**: 최대 5초 지연, 불필요한 파일 읽기
- Kubernetes ConfigMap은 `inotify`로 변경을 감지하기 어려운 경우가 있어서
  (symlink 기반 업데이트), 폴링이 더 안정적일 수 있다

### Q8: 왜 StdoutNoOpWriter의 Close가 no-op인가?

```go
func (nwc *noopWriteCloser) Close() error {
    return nil
}
```

`exporter.Stop()`은 항상 `writer.Close()`를 호출한다. stdout을 실제로 닫으면
프로세스의 모든 표준 출력이 실패한다. no-op으로 만들어 `Stop()` 로직의 일관성을
유지하면서 stdout을 보호한다.

---

## 요약

| 컴포넌트 | 역할 | 핵심 메커니즘 |
|----------|------|-------------|
| FlowLogExporter | 인터페이스 | Export(ctx, ev) + Stop() |
| exporter | 정적 익스포터 | Filter → Hook → Aggregate? → Encode |
| dynamicExporter | 동적 관리 | ConfigWatcher → onConfigReload 팬아웃 |
| Aggregator | 플로우 집계 | proto.Marshal 키 + 방향별 카운트 |
| AggregatorRunner | 주기적 내보내기 | Ticker → Export → 맵 초기화 |
| Encoder | 직렬화 | json.Encoder (JSONL 형식) |
| FileWriter | 파일 출력 | lumberjack 로테이션 |
| ConfigWatcher | 설정 감시 | 5초 폴링 + MD5 해시 |
| OnExportEvent | 훅 시스템 | stop 반환으로 파이프라인 제어 |
| FieldMask | 필드 마스킹 | protobuf reflection Copy |

익스포터 시스템의 핵심 설계 원칙:
1. **유연성**: 필터, 마스크, 집계, 훅을 조합하여 다양한 출력 요구사항 충족
2. **동적성**: 서비스 재시작 없이 런타임에 설정 변경 가능
3. **탄력성**: 부분 실패 허용, 에러 시 로그만 남기고 계속 동작
4. **효율성**: 재사용 객체, 집계로 저장 공간 절약, 필드 마스킹으로 대역폭 절약
