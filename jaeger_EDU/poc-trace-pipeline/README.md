# PoC 11: 트레이스 파이프라인 시뮬레이터

## 개요

Jaeger v2의 핵심인 OTel Collector의 Receiver -> Processor -> Exporter 파이프라인 패턴을 시뮬레이션한다.

Jaeger v2는 자체 파이프라인을 완전히 OTel Collector 기반으로 전환하였으며, 이 PoC는 해당 아키텍처의 핵심 개념을 Go 표준 라이브러리만으로 재현한다.

## Jaeger/OTel Collector 소스코드 대응

| 시뮬레이션 개념 | 소스 위치 |
|---|---|
| Component 인터페이스 | `opentelemetry-collector/component/component.go` |
| Receiver 인터페이스 | `opentelemetry-collector/receiver/receiver.go` |
| Processor 인터페이스 | `opentelemetry-collector/processor/processor.go` |
| Exporter 인터페이스 | `opentelemetry-collector/exporter/exporter.go` |
| Pipeline 구성 | `opentelemetry-collector/service/pipelines/` |
| Jaeger 파이프라인 빌드 | `jaeger/cmd/jaeger/internal/all-in-one.go` |
| OTLP Receiver | `opentelemetry-collector/receiver/otlpreceiver/` |
| Batch Processor | `opentelemetry-collector/processor/batchprocessor/` |

## 시뮬레이션 구성

### 컴포넌트 인터페이스

```
Component (기본)
  ├── Start(ctx) error     -- 컴포넌트 시작
  ├── Shutdown(ctx) error  -- 정상 종료
  └── Name() string        -- 이름

Receiver (데이터 수신)
  └── Component + (내부적으로 Consumer에 데이터 전달)

Processor (데이터 처리)
  └── Component + ProcessTraces(ctx, traces) (traces, error)

Exporter (데이터 내보내기)
  └── Component + ExportTraces(ctx, traces) error
```

### 리시버 구현
- **OTLPReceiver**: OTLP 프로토콜 시뮬레이션 (Jaeger v2 기본 리시버)
- **JaegerReceiver**: Jaeger 네이티브 프로토콜 시뮬레이션 (하위 호환성)

### 프로세서 구현
- **BatchProcessor**: 스팬을 배치로 묶어 처리
- **FilterProcessor**: 특정 서비스의 스팬만 통과
- **TailSamplingProcessor**: 확률적 샘플링 + 에러 스팬 항상 유지

### 익스포터 구현
- **MemoryExporter**: 메모리에 스팬 저장 (통계 포함)
- **ConsoleExporter**: 콘솔에 스팬 정보 출력

## 파이프라인 토폴로지

```
Fan-in (다중 리시버)        Chaining (프로세서 체인)          Fan-out (다중 익스포터)

[Receiver 1] ──┐                                        ┌──> [Exporter 1]
               ├──> [Proc 1] -> [Proc 2] -> [Proc N] ──┤
[Receiver N] ──┘                                        └──> [Exporter N]
```

## 라이프사이클 관리

```
시작 순서 (데이터 수신 준비가 먼저):
  1. Exporter.Start()    -- 데이터 저장 준비
  2. Processor.Start()   -- 데이터 처리 준비
  3. processLoop 시작    -- 파이프라인 처리 루프
  4. Receiver.Start()    -- 데이터 생성 시작

종료 순서 (데이터 생성 중단이 먼저):
  1. Receiver.Shutdown()  -- 새 데이터 수신 중단
  2. processLoop 종료     -- 남은 데이터 드레인
  3. Processor.Shutdown() -- 버퍼 플러시
  4. Exporter.Shutdown()  -- 최종 저장 완료
```

이 시작/종료 순서가 데이터 손실을 방지하는 핵심이다.

## 테스트 시나리오

| 시나리오 | 구성 | 검증 포인트 |
|---|---|---|
| 1. 단순 파이프라인 | OTLP -> Memory + Console | 기본 데이터 흐름 |
| 2. 다중 리시버 + 프로세서 체인 | OTLP + Jaeger -> Batch -> TailSampling -> Memory | Fan-in, 배치, 샘플링 |
| 3. 필터 + 팬아웃 | OTLP -> Filter -> Memory + Console | 필터링, Fan-out |
| 4. 전체 통합 | OTLP + Jaeger -> Batch -> Filter -> TailSampling -> Memory + Console | 전체 파이프라인 |

## 실행

```bash
go run main.go
```

## 출력 예시

```
[파이프라인] 'full-pipeline' 시작
--------------------------------------------------
  >>> 익스포터 시작
  [익스포터] memory-store-4 시작
  [익스포터] console-4 시작
  >>> 프로세서 시작
  [프로세서] batch-proc 시작
  [프로세서] svc-filter 시작
  [프로세서] tail-sampler 시작 (샘플률: 70%)
  >>> 리시버 시작
  [리시버] otlp-receiver-1 시작
  [리시버] jaeger-receiver-1 시작
--------------------------------------------------
    [콘솔] 15개 스팬 내보냄
    [콘솔] 8개 스팬 내보냄
...
[파이프라인] 'full-pipeline' 종료 완료
```
