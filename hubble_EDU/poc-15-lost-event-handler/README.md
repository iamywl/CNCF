# PoC-15: 이벤트 유실 감지/보고

## 개요

Hubble의 이벤트 유실(Lost Event) 감지 및 보고 시스템을 시뮬레이션한다. 링 버퍼에서 writer가 reader를 추월할 때 발생하는 `ErrInvalidRead`, LostEvent 메타 이벤트 생성, `IntervalRangeCounter`를 통한 rate-limited 배치 보고 패턴을 재현한다.

## 핵심 개념

### 1. 링 버퍼 오버플로우

Hubble의 링 버퍼는 고정 크기이다. 새 이벤트가 빠르게 쓰이면 아직 읽히지 않은 오래된 이벤트가 덮어씌워진다. Reader가 이 위치를 읽으려 하면 `ErrInvalidRead`가 발생한다.

```
Ring Buffer (크기 5):
Write → [5][6][7][8][9]  ← 0~4번 이벤트는 이미 덮어씌워짐
          ↑
        Reader가 index 2를 읽으려 함 → ErrInvalidRead
```

### 2. LostEvent 메타 이벤트

유실된 이벤트 수와 시간 범위를 기록하는 메타 이벤트이다:

```go
type LostEvent struct {
    Source        LostEventSource  // HUBBLE_RING_BUFFER 또는 PERF_EVENT_RING_BUFFER
    NumEventsLost uint64           // 유실된 이벤트 수
    First         time.Time        // 유실 시작 시간
    Last          time.Time        // 유실 종료 시간
}
```

### 3. IntervalRangeCounter (Rate Limiting)

유실 이벤트를 즉시 전달하면 클라이언트에 부하가 걸리므로, `IntervalRangeCounter`로 배치 보고한다:

- 기본 간격: 10초 (`LostEventSendInterval`)
- `Increment()`: 유실 발생 시 카운터 증가
- `IsElapsed()`: 간격 경과 여부 확인
- `Clear()`: 카운터 리셋 후 누적값(count/first/last) 반환

### 4. 유실 소스별 처리

| 소스 | 처리 방식 | 원인 |
|------|----------|------|
| `HUBBLE_RING_BUFFER` | Rate-limited 배치 보고 | Observer가 Flow 처리 속도보다 느림 |
| `PERF_EVENT_RING_BUFFER` | 즉시 전달 | 커널 perf 버퍼 오버플로우 |

### 5. 모니터링

유실 이벤트 수는 Prometheus 메트릭으로 추적된다. 운영자는 유실 빈도에 따라 링 버퍼 크기를 조정할 수 있다.

## 실행 방법

```bash
go run main.go
```

5가지 테스트를 실행한다: 링 버퍼 오버플로우 감지, LostEvent 삽입, IntervalRangeCounter, GetFlows 스트림 내 LostEvent 처리, 유실 소스 구분.

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `cilium/pkg/hubble/observer/local_observer.go` | `GetFlows()` - LostEvent 처리 로직 |
| `cilium/pkg/hubble/container/ring_reader.go` | `Next()` - ErrInvalidRead 발생 |
| `cilium/pkg/hubble/container/ring.go` | `Ring.Write()`, `Ring.read()` - 오버플로우 감지 |
| `cilium/pkg/hubble/observer/local_observer.go` | `lostEventCounter` - IntervalRangeCounter 사용 |
| `cilium/pkg/hubble/container/ring.go` | `ErrInvalidRead` - writer 추월 에러 |
