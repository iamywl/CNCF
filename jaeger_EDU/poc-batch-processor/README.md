# PoC 10: 배치 프로세서 시뮬레이터

## 개요

Jaeger v2가 기반으로 하는 OTel(OpenTelemetry) Collector의 배치 프로세서를 시뮬레이션한다. 배치 프로세서는 개별 스팬을 모아서 일괄 전송함으로써 네트워크 오버헤드를 줄이고 처리 효율을 높이는 핵심 컴포넌트이다.

Go 표준 라이브러리만으로 OTel Collector의 `batchprocessor` 패키지 핵심 로직을 재현한다.

## Jaeger/OTel Collector 소스코드 대응

| 시뮬레이션 개념 | 소스 위치 |
|---|---|
| `batchProcessor` 구조체 | `opentelemetry-collector/processor/batchprocessor/batch_processor.go` |
| `processLoop()` | 같은 파일의 메인 처리 루프 |
| `SendBatchSize`, `Timeout` 설정 | `batchprocessor/config.go` |
| 메모리 리미터 연동 | `opentelemetry-collector/extension/memorylimiterextension/` |
| Jaeger에서의 사용 | `jaeger/cmd/jaeger/internal/all-in-one.go` 파이프라인 구성 |

## 시뮬레이션 내용

### 1. 이중 트리거 메커니즘
- **크기 기반 (sendBatchSize)**: 배치에 누적된 스팬 수가 설정값에 도달하면 즉시 플러시
- **시간 기반 (timeout)**: 마지막 플러시 이후 설정 시간이 경과하면 현재 배치를 플러시
- 두 조건 중 **먼저** 도달하는 조건이 플러시를 트리거

### 2. 메모리 리미터
- 현재 메모리 사용량이 설정 한도를 초과하면 새 스팬을 거부(드롭)
- OOM(Out of Memory) 방지를 위한 안전 장치
- 피크 메모리 사용량 추적

### 3. SendBatchMaxSize
- 단일 배치의 최대 크기 제한
- 초과 스팬은 다음 배치로 이월
- 다운스트림 시스템의 부하 제어

### 4. 메트릭 추적
- 수신/처리/드롭 스팬 수
- 배치 전송 횟수 및 플러시 사유별 분류
- 메모리 사용량 (현재/피크)

## 핵심 알고리즘

```
processLoop():
  loop:
    select:
      case span from inputCh:
        if memory > limit:
          DROP span
          continue
        append span to batch
        if len(batch) >= sendBatchSize:
          FLUSH(reason=SIZE)
          reset timer

      case timer expired:
        if len(batch) > 0:
          FLUSH(reason=TIMEOUT)
        reset timer

      case done signal:
        FLUSH(reason=SHUTDOWN)
        return

FLUSH(reason):
  if sendBatchMaxSize > 0 && len(batch) > sendBatchMaxSize:
    export batch[:sendBatchMaxSize]
    keep batch[sendBatchMaxSize:] for next batch
  else:
    export entire batch
  update metrics
```

## 테스트 시나리오

| 시나리오 | 설정 | 기대 결과 |
|---|---|---|
| 1. 기본 배치 처리 | 배치=20, 타임아웃=300ms | 크기 도달 4회 + 타임아웃 1회 |
| 2. 가변 인입 속도 | 배치=30, 타임아웃=200ms | 저속 시 타임아웃, 고속 시 크기 도달 |
| 3. 메모리 리미터 | 한도=10KB | 일부 스팬 드롭 발생 |
| 4. MaxSize 제한 | 배치=20, 최대=25 | 배치당 25개 이하 보장 |

## 실행

```bash
go run main.go
```

## 출력 예시

```
[배치 프로세서] 시작됨 (배치크기=20, 타임아웃=300ms, 메모리한도=0바이트)
  [내보내기] 배치 #1: 20개 스팬 (사유: 크기 도달)
  [내보내기] 배치 #2: 20개 스팬 (사유: 크기 도달)
  ...
  [내보내기] 배치 #5: 15개 스팬 (사유: 타임아웃)

=== 배치 프로세서 메트릭 ===
  수신된 스팬:      95
  처리된 스팬:      95
  드롭된 스팬:      0
  전송된 배치:      5
    크기 도달:      4
    타임아웃:       1
```
