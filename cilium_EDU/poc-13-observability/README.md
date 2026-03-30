# PoC-13: Observability (관측성 파이프라인)

## 개요

Cilium/Hubble의 관측성 파이프라인을 Go 표준 라이브러리만으로 시뮬레이션한다.
BPF perf 이벤트가 커널에서 사용자 공간으로 전달되어 Monitor Agent가 수집하고,
Lock-free Ring Buffer에 저장한 뒤, Hubble Observer가 소비하여 Prometheus 스타일
메트릭으로 집계하는 전체 흐름을 재현한다.

## 배경 지식

### 네트워크 관측이 왜 중요한가

쿠버네티스 환경에서는 수천 개의 Pod이 동적으로 생성/소멸되며 서로 통신한다.
전통적인 패킷 캡처(tcpdump) 방식은 이 규모에서 병목이 되고, 컨테이너 네트워크의
오버레이 특성상 가시성 확보가 어렵다. Cilium은 eBPF를 활용하여 커널 수준에서
패킷 흐름을 관찰하므로, 애플리케이션 수정 없이 L3/L4/L7 수준의 네트워크 가시성을
제공할 수 있다.

### Hubble 아키텍처

Hubble은 Cilium의 관측성 레이어로, 다음과 같은 구조를 가진다:

- **BPF 프로그램**: 커널에서 패킷 이벤트(trace, drop, policy-verdict)를 perf event map에 기록
- **Monitor Agent** (`pkg/monitor/agent/`): perf event map을 polling하여 이벤트를 수집/디코딩
- **Ring Buffer** (`pkg/container/ring/`): 수집된 이벤트를 고정 크기 순환 버퍼에 저장
- **Hubble Server** (`pkg/hubble/`): gRPC API로 이벤트를 필터링하여 클라이언트에 제공
- **Hubble Relay**: 여러 노드의 Hubble Server를 통합하여 클러스터 전체 관측을 제공

### Lock-free Ring Buffer의 원리

고성능 이벤트 파이프라인에서 mutex 기반 동기화는 경합으로 인한 성능 저하를 유발한다.
Ring Buffer는 다음 전략으로 lock-free 동작을 달성한다:

- **Atomic write pointer**: `sync/atomic`으로 쓰기 위치를 단조 증가시켜 잠금 없이 기록
- **비트 마스크 인덱싱**: 버퍼 크기를 2의 거듭제곱으로 설정, `pos & (size-1)`로 나머지 연산 대체
- **Cycle detection**: `writePos - readPos > size`이면 데이터가 이미 덮어쓰기되었음을 감지
- **Lost event tracking**: 덮어쓰기 발생 횟수를 atomic 카운터로 추적

### Prometheus 메트릭 노출

Hubble은 `hubble_flows_processed_total{type, verdict, protocol}` 등의 카운터를
Prometheus exposition 형식으로 노출한다. Grafana 대시보드에서 트래픽 패턴,
드롭율, 프로토콜 분포 등을 실시간으로 모니터링할 수 있다.
## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 경로 | PoC 구현 | 설명 |
|------|------------------|----------|------|
| BPF Perf Event | 커널 BPF 프로그램 | `generateFlowEvent()` | 랜덤 네트워크 이벤트 생성 |
| Monitor Agent | `pkg/monitor/agent/` | `MonitorAgent` struct | 이벤트 수집 및 팬아웃 |
| Ring Buffer | `pkg/container/ring/` | `RingBuffer` struct | Lock-free 순환 버퍼 |
| Hubble Observer | `pkg/hubble/observer/` | `HubbleObserver` struct | 이벤트 소비 및 필터링 |
| 메트릭 집계 | `pkg/hubble/metrics/` | `MetricAggregator` struct | Prometheus 카운터 집계 |
| Flow Event | `flow.Flow` protobuf | `FlowEvent` struct | L3/L4 플로우 이벤트 표현 |

## 아키텍처 / 흐름 다이어그램

```
+---------------------+
|   BPF Perf Events   |   커널 공간에서 패킷 이벤트 발생
| generateFlowEvent() |   (80% FORWARDED, 15% DROPPED, 5% AUDIT)
+----------+----------+
           |
           v
+----------+----------+
|    Monitor Agent     |   이벤트 수집 및 디코딩
|   ProcessEvent()     |
+----------+----------+
           |
           +---------------------------+
           |                           |
           v                           v
+----------+----------+   +------------+-----------+
|    Ring Buffer       |   |   Fan-out (채널)       |
| (lock-free, atomic)  |   |   비차단 전송           |
|  writePos -> idx     |   +------------+-----------+
|  = pos & mask        |                |
|  Cycle Detection:    |                v
|  wPos - rPos > size  |   +------------+-----------+
+----------------------+   |   Hubble Observer      |
                           |   Run() -> Observe()   |
                           +------------+-----------+
                                        |
                                        v
                           +------------------------+
                           | Prometheus Exposition  |
                           | hubble_flows_processed |
                           | _total{type,verdict,   |
                           |        protocol}       |
                           +------------------------+
```

## 코드 해설

### RingBuffer - Lock-free 순환 버퍼

`buffer []FlowEvent` + `writePos uint64` (atomic) + `mask uint64` (size-1)로 구성된다.
`Write()`에서 `atomic.AddUint64(&rb.writePos, 1)`로 위치를 확보하고 `pos & rb.mask`로
실제 인덱스를 계산한다. 200개 이벤트를 크기 64 버퍼에 기록하면 136개가 덮어쓰기되어
lost로 집계된다. `Read()`는 readPos가 유효 범위(`writePos - size ~ writePos`) 안에
있는지 확인하여 cycle detection을 수행한다.

### MonitorAgent - 이벤트 수집 및 팬아웃

`ProcessEvent()`가 이벤트를 Ring Buffer에 저장하고, 등록된 모든 소비자 채널에
`select/default` 패턴으로 비차단 팬아웃한다. 채널이 가득 차면 이벤트를 드롭하여
생산자가 블로킹되지 않도록 한다 (backpressure).

### MetricAggregator - Double-check Locking 패턴

`{type, verdict, protocol}` 레이블 조합을 키로 사용하는 카운터 맵이다.
RLock으로 기존 키를 조회하고 atomic으로 증가시키는 빠른 경로와, 새 키 등록 시에만
Lock을 잡는 느린 경로를 분리하여 경합을 최소화한다.

### HubbleObserver - 이벤트 소비 루프

채널에서 이벤트를 수신하여 `MetricAggregator.Observe()`를 호출한다.
`stopCh`로 종료 시그널을 받으면 루프를 탈출하는 Go 관용적 패턴을 사용한다.

### FlowEvent - 네트워크 플로우 표현

`flow.Flow` protobuf를 단순화한 구조체. Timestamp, Type, Verdict, SrcIP/DstIP,
SrcPort/DstPort, Protocol, DropReason 필드를 포함한다.
## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-13-observability
go run main.go
```

예상 출력 (발췌):

```
=== Cilium Observability PoC ===
[1] 구성 요소 초기화
  Ring Buffer 생성: capacity=64 / Monitor Agent / Metric Aggregator / Hubble Observer
[3] BPF Perf Event 생성 시뮬레이션 (200개)
  Event #1: [14:30:01.123] TRACE 10.42.1.5:8080 → 10.42.2.3:443 TCP verdict=FORWARDED
  Event #2: [14:30:01.123] DROP 10.42.3.7:12345 → 10.42.1.9:80 UDP verdict=DROPPED reason=POLICY_DENIED
[4] Ring Buffer 통계: 총 기록=200, 손실=136, 용량=64
[6] Cycle Detection: 위치 0 읽기 valid=false, 유효 범위=[136, 200)
[7] hubble_flows_processed_total{type=TRACE,verdict=FORWARDED,protocol=TCP} 54
[8] Observer 처리 이벤트: ~32 (채널 버퍼=32 → 일부 드롭)
```

## 핵심 포인트

1. **Lock-free Ring Buffer**: mutex 없이 atomic 연산만으로 고속 이벤트 저장을 달성한다.
   버퍼 크기를 2의 거듭제곱으로 제한하여 비트 마스크(`&`)로 모듈로 연산을 대체한다.

2. **Backpressure 전략**: 소비자 채널이 가득 차면 이벤트를 드롭한다.
   블로킹하여 생산자를 멈추는 대신 최신 데이터 수집을 우선하는 설계 철학이다.

3. **Cycle Detection**: 단조 증가하는 writePos와 버퍼 크기를 비교하여
   읽으려는 데이터가 이미 덮어쓰기되었는지 O(1)으로 판별한다.

4. **Fan-out 패턴**: Monitor Agent가 하나의 이벤트를 Ring Buffer 저장과
   다수의 소비자 채널 전송을 동시에 수행하여, 저장과 실시간 스트리밍을 분리한다.

5. **레이블 기반 메트릭**: `{type, verdict, protocol}` 조합별 독립 카운터를 유지하여
   Prometheus/Grafana에서 다차원 쿼리가 가능한 메트릭 구조를 제공한다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium/Hubble | 이 PoC |
|------|-------------------|--------|
| 이벤트 소스 | eBPF가 커널에서 perf event map에 기록 | `generateFlowEvent()` 랜덤 생성 |
| Ring Buffer | C 구현, mmap 공유 메모리, CPU별 버퍼 | Go slice + atomic, 단일 버퍼 |
| 이벤트 형식 | Protocol Buffers (`flow.Flow`) | Go struct (`FlowEvent`) |
| API | gRPC 스트리밍 (`GetFlows`, `ServerStatus`) | 없음 (메트릭 집계만) |
| Relay | 다중 노드 Hubble Server 통합 컴포넌트 | 없음 (단일 노드) |
| L7 관측 | HTTP, gRPC, Kafka, DNS 프로토콜 파싱 | L3/L4 수준만 시뮬레이션 |
| Identity | Security Identity → Pod/Namespace 매핑 | 랜덤 숫자 ID |
| 필터링 | 필터 체인 (`pkg/hubble/filters/`) | 없음 (모든 이벤트 통과) |
| 메트릭 | client_golang, 히스토그램/게이지 포함 | 맵 기반 카운터, 문자열 출력 |
| 동시성 | CPU별 perf buffer, 고루틴 풀 | 단일 생산자/단일 소비자 |

## 관련 문서

- [13-observability.md](../13-observability.md) - Cilium 관측성 심화 문서
