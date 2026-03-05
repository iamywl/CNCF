# PoC-13: Observability (관측성 파이프라인)

## 개요

Cilium/Hubble의 관측성 파이프라인을 시뮬레이션한다.
BPF perf 이벤트가 커널에서 사용자 공간으로 전달되어 수집, 저장, 집계되는 전체 흐름을 재현한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 Cilium 경로 | PoC 구현 |
|----------|------------------|----------|
| BPF Perf Event | 커널 BPF 프로그램 | `generateFlowEvent()` |
| Monitor Agent | `pkg/monitor/agent/` | `MonitorAgent` struct |
| Ring Buffer | `pkg/container/ring/` | `RingBuffer` (lock-free) |
| Hubble Observer | `pkg/hubble/observer/` | `HubbleObserver` struct |
| 메트릭 집계 | `pkg/hubble/metrics/` | `MetricAggregator` |

## 핵심 개념

### Lock-Free Ring Buffer
- **Atomic write pointer**: `sync/atomic`으로 쓰기 위치를 관리, 잠금 불필요
- **비트 마스크 인덱싱**: 크기를 2의 거듭제곱으로 설정하여 `pos & mask`로 빠른 모듈로 연산
- **Cycle detection**: `writePos - readPos > size`이면 읽으려는 데이터가 덮어쓰기됨을 감지
- **Lost event tracking**: 버퍼 크기 초과 시 덮어쓰기 횟수를 atomic으로 추적

### Flow Event 타입
- **TRACE**: 패킷 추적 이벤트 (정상 전달)
- **DROP**: 패킷 드롭 이벤트 (정책 거부, 파싱 오류 등)
- **POLICY_VERDICT**: 정책 판정 이벤트 (감사 모드)

### Prometheus 메트릭
- `hubble_flows_processed_total{type, verdict, protocol}` 카운터
- 레이블 조합별 독립적 카운팅

## 실행 방법

```bash
go run main.go
```

## 출력 예시

- 생성된 Flow 이벤트 샘플 (trace/drop/policy-verdict)
- Ring Buffer 통계 (기록/손실/용량)
- Cycle detection 시연 (오래된 이벤트 읽기 실패)
- Prometheus exposition 형식의 메트릭 출력

## 관련 문서

- [13-observability.md](../13-observability.md) - Cilium 관측성 심화 문서
