# PoC-16: 스팬 기반 분산 추적(텔레메트리) 시뮬레이션

## 개요

Tart의 `Root.swift`(OpenTelemetry 통합)과 `OTel.swift`(TracerProvider 초기화)의 분산 추적 구조를 Go로 재현한다. 루트/자식 스팬 생성, 속성 및 이벤트 기록, 에러 캡처, OTLP 형식 출력을 시뮬레이션한다.

## Tart 소스코드 매핑

| Tart 소스 | PoC 대응 | 설명 |
|-----------|---------|------|
| `OTel.swift` — `class OTel` | `OTelInstance` 구조체 | 싱글톤 텔레메트리 인스턴스 |
| `OTel.initializeTracing()` — TRACEPARENT 확인 | `NewOTel()` — `os.LookupEnv("TRACEPARENT")` | 환경변수 기반 활성화 |
| `OTel.shared.tracer.spanBuilder(spanName:).startSpan()` | `Tracer.SpanBuilder().StartSpan()` | 스팬 생성 |
| `Root.swift` — `span.setAttribute(key:, value:)` | `Span.SetAttribute()` | 커맨드라인 인자 등 속성 추가 |
| `Prune.swift` — `span?.addEvent(name:)` | `Span.AddEvent()` | 프루닝 이벤트 기록 |
| `Root.swift` — `activeSpan?.recordException(error)` | `Span.RecordException()` | 에러 캡처 |
| `OTel.flush()` | `OTelInstance.Flush()` | 스팬 종료 + 강제 전송 |
| `OtlpHttpTraceExporter` | `ExportOTLP()` | OTLP HTTP 내보내기 시뮬레이션 |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | 환경변수 참조 | 커스텀 OTLP 엔드포인트 |

## 핵심 개념

### 1. 스팬 계층 구조

```
[Root Span: "clone"]
  ├── Command-line arguments: ["tart", "clone", "image", "name"]
  ├── ci.provider: "cirrus"
  │
  ├── [Child Span: "pull"]
  │   ├── oci.image-name: "ghcr.io/cirruslabs/macos-sonoma:latest"
  │   └── oci.image-uncompressed-disk-size-bytes: 68719476736
  │
  ├── [Child Span: "prune"]
  │   ├── prune.required-bytes: 16106127360
  │   ├── Event: "Pruned 3221225472 bytes for ghcr.io/org/macos-monterey:old"
  │   └── Event: "Reclaimed 5368709120 bytes"
  │
  └── [catch] recordException(error)
      └── Event: "exception" {type: URLError, message: "..."}
```

### 2. OTel 초기화 흐름

```
OTel.init()
├── TRACEPARENT 환경변수 확인
│   └── 없으면 nil 반환 (트레이싱 비활성, 오버헤드 0)
├── Resource 생성 (service.name="tart", service.version=...)
├── SpanExporter = OtlpHttpTraceExporter(endpoint:)
├── SpanProcessor = SimpleSpanProcessor(spanExporter:)
└── TracerProvider = TracerProviderBuilder().build()
```

### 3. Root.main()과 OTel 통합

```
Root.main()
├── SIGINT 핸들러 설정
├── defer { OTel.shared.flush() }
├── parseAsRoot() -> command
├── span = spanBuilder(commandName).startSpan()
├── setActiveSpan(span)
├── setAttribute("Command-line arguments", args)
├── Config().gc()
├── command.run()
│   └── 내부에서 자식 스팬 생성
└── catch { recordException(error) }
```

### 4. flush 워크어라운드

OTel.swift의 `flush()`는 `Thread.sleep(100ms)`를 포함한다. OpenTelemetry Swift SDK의 비동기 전송 버그를 회피하기 위한 것으로, forceFlush() 호출 후에도 즉시 전송되지 않는 문제가 있다.

## 실행 방법

```bash
cd poc-16-telemetry
go run main.go
```

## 실행 결과 (요약)

```
=== PoC-16: 스팬 기반 분산 추적(텔레메트리) 시뮬레이션 ===

--- 데모 1: Root.main() -- 루트 스팬 생성 ---
  루트 스팬 생성: name=clone, trace_id=...
  속성 추가: Command-line arguments = [tart clone ...]

--- 데모 6: OTLP 형식 출력 ---
  ┌─ Resource: service.name=tart, service.version=2.22.4
  │  ┌─ Span: clone [UNSET] (25.30ms)
  │  │  attr: Command-line arguments = [tart clone ...]
  │  │  ┌─ Span: pull [UNSET] (10.15ms)
  │  │  │  attr: oci.image-name = ghcr.io/cirruslabs/macos-sonoma:latest
  │  │  ┌─ Span: prune [UNSET] (5.08ms)
  │  │  │  event: Pruned 3221225472 bytes for ...
  │  │  │  event: Reclaimed 5368709120 bytes
  │  │  ┌─ Span: pull-failed [ERROR] (0.05ms)
  │  │  │  event: exception {type: *errors.errorString, message: ...}

--- 데모 7: 스팬 트리 시각화 ---
  clone           |==================================================|  25.30ms
    pull           |==================                                |  10.15ms
    prune          |                  =========                       |   5.08ms
    pull-failed    |                           =                      |!  0.05ms
```

## 학습 포인트

1. **TRACEPARENT 기반 활성화**: 환경변수가 없으면 트레이싱을 완전히 비활성화하여 성능 영향 0
2. **계층적 스팬**: 루트 스팬에서 시작하여 자식 스팬으로 작업 단위를 세분화
3. **속성 vs 이벤트**: 속성은 스팬 전체 메타데이터, 이벤트는 시점 기반 사건 기록
4. **에러 캡처**: recordException()으로 예외를 스팬에 구조화하여 기록
5. **OTLP 프로토콜**: OpenTelemetry 표준 형식으로 스팬 데이터를 내보내기
6. **SDK 버그 회피**: 실전에서 만나는 라이브러리 제한사항과 워크어라운드
