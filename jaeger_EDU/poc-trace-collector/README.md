# PoC: Jaeger Trace Collection Pipeline

## 개요

Jaeger의 트레이스 수집 파이프라인(**Receiver -> Processor -> Exporter**)을 시뮬레이션한다.
Jaeger v2는 OpenTelemetry Collector 위에 구축되어 있으며, 동일한 파이프라인 구조를 따른다.

## 시뮬레이션 대상

| 파이프라인 단계 | 시뮬레이션 내용 | 실제 Jaeger 대응 |
|---------------|----------------|-----------------|
| Receiver | HTTP JSON Span 수신기 | OTLP/Jaeger/Zipkin 수신기 |
| Processor | 배치 프로세서 (크기 + 시간 기반 플러시) | OTEL BatchProcessor |
| Exporter | 인메모리 저장소 기록기 | Cassandra/ES/Memory Exporter |

## 핵심 개념

### 파이프라인 아키텍처

```
┌──────────────┐    ┌───────────────────┐    ┌───────────────┐
│  HTTP        │    │  Batch            │    │  Storage      │
│  Receiver    │───>│  Processor        │───>│  Exporter     │
│              │    │                   │    │               │
│  JSON Span   │    │  maxBatchSize: 3  │    │  In-Memory    │
│  수신        │    │  flushInterval:   │    │  Map 저장     │
│              │    │    200ms           │    │               │
└──────────────┘    └───────────────────┘    └───────────────┘
```

### 배치 프로세서 동작 원리

배치 프로세서는 개별 Span을 모아서 배치로 내보내며, 두 가지 플러시 조건이 있다:

1. **크기 기반 플러시**: 배치에 누적된 Span 수가 `maxBatchSize`에 도달하면 즉시 플러시
2. **타이머 기반 플러시**: `flushInterval` 경과 시 남은 Span이 있으면 플러시

이 이중 조건은 **처리량(throughput)**과 **지연(latency)** 사이의 균형을 맞추는 핵심 메커니즘이다.

### 메트릭 수집

파이프라인의 각 단계에서 수집하는 메트릭:
- **ReceivedCount**: Receiver가 수신한 총 Span 수
- **ProcessedCount**: Processor를 통과한 총 Span 수
- **ExportedCount**: Exporter가 저장소에 기록한 총 Span 수
- **BatchCount**: 생성된 배치의 총 수
- **BatchFlush**: 크기 기반으로 플러시된 횟수
- **FlushCount**: 타이머 기반으로 플러시된 횟수

### 그레이스풀 셧다운

파이프라인 종료 시 데이터 유실을 방지하기 위해 순방향으로 종료한다:
1. Receiver 종료 (새 데이터 수신 중단)
2. Processor 종료 (남은 배치 플러시)
3. Exporter 종료 (남은 데이터 기록 완료)

## 실행 방법

```bash
cd poc-trace-collector
go run main.go
```

## 출력 내용

1. **파이프라인 아키텍처 다이어그램**: 3단계 파이프라인 구조 시각화
2. **실시간 파이프라인 로그**: 각 단계의 수신/배치/저장 이벤트
3. **파이프라인 메트릭**: 단계별 처리 통계
4. **저장된 Trace 요약**: 최종 저장소에 기록된 Trace 정보
5. **배치 처리 동작 분석**: 크기 기반 vs 타이머 기반 플러시 통계

## Jaeger 소스코드와의 대응

| PoC 코드 | Jaeger/OTEL 소스 |
|----------|-----------------|
| `Pipeline` 구조체 | OpenTelemetry Collector 파이프라인 |
| `HTTPReceiver` | `go.opentelemetry.io/collector/receiver` |
| `BatchProcessor` | `go.opentelemetry.io/collector/processor/batchprocessor` |
| `StorageExporter` | `internal/storage/v2/memory` (Exporter 역할) |
| `PipelineMetrics` | `internal/metrics` (메트릭 수집) |
