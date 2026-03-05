# PoC-05: Cilium BPF 맵 타입 시뮬레이션

## 개요

Cilium이 사용하는 세 가지 주요 BPF 맵 타입을 Go 표준 라이브러리만으로 시뮬레이션한다.
LRU Hash (Connection Tracking), Hash (Policy), Per-CPU Hash (Metrics)의
핵심 동작 원리를 재현한다.

## 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/maps/ctmap/ctmap.go` | CT 맵 구조체, CtMap 인터페이스, GCFilter |
| `pkg/maps/ctmap/types.go` | CtEntry (Packets, Bytes, Lifetime, Flags), CtKey4Global |
| `pkg/maps/ctmap/gc/gc.go` | GC 구조체, ConntrackGCInterval, 만료 엔트리 제거 |
| `pkg/maps/policymap/policymap.go` | PolicyMap, PolicyKey, PolicyEntry, policyEntryFlags |
| `pkg/maps/metricsmap/metricsmap.go` | Key, Value, metricsmapCollector (Prometheus Collector) |
| `pkg/bpf/map_linux.go` | Map 구조체, MapKey/MapValue 인터페이스, 캐시 동기화 |

## 시뮬레이션하는 핵심 개념

| BPF 맵 타입 | Cilium 맵 | 시뮬레이션 |
|------------|-----------|-----------|
| `BPF_MAP_TYPE_LRU_HASH` | `cilium_ct4_global` | LRUHashMap — container/list 기반 LRU 퇴출 |
| `BPF_MAP_TYPE_HASH` (LPM) | `cilium_policy_v2_{ep}` | PolicyHashMap — 3단계 와일드카드 매칭 |
| `BPF_MAP_TYPE_PERCPU_HASH` | `cilium_metrics` | PerCPUMetricsMap — CPU별 독립 카운터 + 합산 |
| GC (Garbage Collection) | `gc/gc.go` | GCFilter — 만료/IP 매칭 기반 제거 |

## 4가지 시나리오

| # | 시나리오 | 검증 내용 |
|---|---------|----------|
| 1 | CT Map (LRU Hash + GC) | 연결 추가/조회, 만료 GC, LRU 퇴출, IP 기반 GC |
| 2 | Policy Map (Hash + LPM) | 정확한 매칭, 포트 와일드카드, 프로토콜 와일드카드, DENY/ALLOW |
| 3 | Metrics Map (Per-CPU) | CPU별 카운터 갱신, 합산 읽기, Prometheus 형식 출력 |
| 4 | 맵 타입 비교 | 전체 맵 종류, GC 흐름, Per-CPU 동작 다이어그램 |

## 실행 방법

```bash
cd cilium_EDU/poc-05-bpf-maps
go run main.go
```

## 핵심 설계 원리

### 1. LRU Hash Map (CT Map)
- 커널 `BPF_MAP_TYPE_LRU_HASH`는 용량 초과 시 가장 오래 접근되지 않은 엔트리를 자동 퇴출한다.
- CT 맵의 기본 크기: TCP=524288, Any=262144 (option.CTMapEntriesGlobalTCPDefault)
- GC는 30초 간격으로 실행되며, `GCFilter.RemoveExpired`로 만료 엔트리를 제거한다.
- GC 후 연관된 NAT 맵 엔트리도 함께 정리한다.

### 2. Policy Map (Hash + LPM)
- 엔드포인트별로 `cilium_policy_v2_{ep_id}` 맵이 생성된다.
- 조회 시 LPM(Longest Prefix Match) 순서로 매칭한다:
  1. 정확한 키 (Identity + Direction + Protocol + Port)
  2. 포트 와일드카드 (AllPorts=0)
  3. 프로토콜 와일드카드 (Protocol=0)
- 매칭되지 않으면 기본 거부(default deny).

### 3. Per-CPU Metrics Map
- 커널 `BPF_MAP_TYPE_PERCPU_HASH`는 각 CPU가 독립된 메모리 영역에서 카운터를 갱신한다.
- lock-free로 고성능: BPF 프로그램은 락 없이 현재 CPU의 값만 갱신한다.
- 유저스페이스에서 읽을 때 모든 CPU 값의 배열이 반환되며, `Values.Count()`/`Values.Bytes()`로 합산한다.
- `metricsmapCollector`가 Prometheus Collector 인터페이스를 구현하여 `/metrics` 엔드포인트에 노출한다.
