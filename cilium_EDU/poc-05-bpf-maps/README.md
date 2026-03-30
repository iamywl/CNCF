# PoC-05: Cilium BPF 맵 타입 시뮬레이션

## 개요

Cilium이 사용하는 세 가지 주요 BPF 맵 타입을 Go 표준 라이브러리만으로 시뮬레이션한다.
LRU Hash (Connection Tracking), Hash (Policy), Per-CPU Hash (Metrics)의 핵심 동작 원리를
`container/list`, `sync`, `map` 등 표준 라이브러리만으로 재현하며, GC(Garbage Collection)와
LPM(Longest Prefix Match) 조회 로직까지 포함한다.

## 배경 지식

### BPF 맵이란

BPF 맵은 Linux 커널의 eBPF 프로그램과 유저스페이스 프로세스 간에 데이터를 공유하는 핵심 메커니즘이다.
커널 내부의 BPF 프로그램은 패킷을 처리하면서 맵에 데이터를 읽고 쓰고, 유저스페이스의 Cilium 에이전트는
같은 맵을 통해 정책을 주입하거나 통계를 수집한다. 맵은 `bpf()` 시스템 콜로 생성되며, 파일 디스크립터를
통해 양쪽에서 접근한다.

### Cilium이 사용하는 주요 맵 종류

| 맵 타입 | 커널 상수 | 특성 | Cilium 용도 |
|---------|----------|------|------------|
| **Hash** | `BPF_MAP_TYPE_HASH` | 임의의 키-값 쌍, O(1) 조회 | Policy 맵 (`cilium_policy_v2_*`), LB Service 맵 |
| **LRU Hash** | `BPF_MAP_TYPE_LRU_HASH` | Hash + 용량 초과 시 LRU 퇴출 | CT 맵 (`cilium_ct4_global`), NAT 맵 (`cilium_snat_v4`) |
| **Array** | `BPF_MAP_TYPE_ARRAY` | 고정 크기, 인덱스 기반 접근 | Tail-call 맵, 설정 맵 |
| **LPM Trie** | `BPF_MAP_TYPE_LPM_TRIE` | Longest Prefix Match, CIDR 매칭 | IPCACHE 맵 (`cilium_ipcache`) |
| **Per-CPU Hash** | `BPF_MAP_TYPE_PERCPU_HASH` | CPU별 독립 메모리, lock-free | Metrics 맵 (`cilium_metrics`) |

- **Hash/LRU Hash**: 범용적이며, LRU는 CT처럼 자연스럽게 오래된 엔트리가 퇴출되어야 하는 경우에 적합하다.
- **LPM Trie**: IP 프리픽스 매칭에 최적화되어 CIDR 기반 정책 조회에 사용된다.
- **Per-CPU**: 각 CPU가 독립된 메모리 영역을 갖기 때문에 락 경합 없이 고성능 카운터를 구현할 수 있다.

## 시뮬레이션하는 개념

| BPF 맵 타입 | Cilium 맵 | 시뮬레이션 구현 | 소스 참조 |
|------------|-----------|---------------|----------|
| `BPF_MAP_TYPE_LRU_HASH` | `cilium_ct4_global` | `LRUHashMap` -- `container/list` 기반 LRU 퇴출 | `pkg/maps/ctmap/ctmap.go` |
| `BPF_MAP_TYPE_HASH` (LPM) | `cilium_policy_v2_{ep}` | `PolicyHashMap` -- 3단계 와일드카드 매칭 | `pkg/maps/policymap/policymap.go` |
| `BPF_MAP_TYPE_PERCPU_HASH` | `cilium_metrics` | `PerCPUMetricsMap` -- CPU별 독립 카운터 + 합산 | `pkg/maps/metricsmap/metricsmap.go` |
| GC (Garbage Collection) | `gc/gc.go` | `GCFilter` -- 만료/IP 매칭 기반 엔트리 제거 | `pkg/maps/ctmap/gc/gc.go` |

## 아키텍처 다이어그램

```
  유저스페이스 (Cilium Agent)
  +-------------------------------------------------------------+
  |  PolicyMap.Allow()    CTMap.Lookup()    MetricsMap.ReadAll() |
  +-------+--------------------+--------------------+-----------+
          |          bpf() 시스템 콜               |
  --------v--------------------v--------------------v-----------
  커널 공간 (eBPF 맵)
  +----------------+  +------------------+  +------------------+
  | Policy Map     |  | CT Map           |  | Metrics Map      |
  | (Hash)         |  | (LRU Hash)       |  | (Per-CPU Hash)   |
  | Key: Identity  |  | Key: 5-tuple     |  | Key: Reason+Dir  |
  |   +Dir+Proto   |  |   +Flags         |  | Val: [CPU0..N]   |
  | Val: ALLOW/    |  | Val: Pkts,Bytes  |  |   count, bytes   |
  |   DENY+Proxy   |  |   Lifetime,Flags |  |   (lock-free)    |
  +----------------+  +--------+---------+  +------------------+
                               |
                        +------v-------+
                        | GC (30초)    |
                        | 만료+NAT정리 |
                        +--------------+

  LPM 정책 조회 폴백:
  [정확한 매칭] --miss--> [포트 와일드카드] --miss--> [프로토콜 와일드카드] --miss--> DROP

  Per-CPU 카운터:
  CPU0:+1  CPU1:+1  CPU2:+1  CPU3:+1  -->  유저스페이스: 합산 total=4
```

## 코드 해설

### 1. LRUHashMap (CT 맵 시뮬레이션)

`container/list`를 이용한 이중 연결 리스트로 LRU 순서를 관리한다. `data` 맵은 O(1) 조회를,
`order` 리스트는 접근 순서를 추적한다. `Update()` 시 용량을 초과하면 리스트의 `Back()`(가장 오래된)
엔트리를 자동 퇴출하고, `Lookup()` 시에는 해당 엔트리를 `Front()`로 이동시켜 최근 접근으로 표시한다.
실제 커널의 `BPF_MAP_TYPE_LRU_HASH`가 수행하는 자동 퇴출을 정확히 재현한다.

### 2. GCFilter (CT 맵 가비지 컬렉션)

`RunGC()`는 Cilium의 `gc/gc.go`가 30초 주기로 수행하는 GC를 시뮬레이션한다.
`RemoveExpired` 플래그로 만료된 엔트리를 제거하고, `MatchIPs`로 특정 IP에 연결된 엔트리를
선택적으로 삭제한다. 실제 Cilium에서는 CT 엔트리 삭제 시 연관된 NAT 맵 엔트리도 함께 정리한다.

### 3. PolicyHashMap (정책 맵 시뮬레이션)

엔드포인트별로 생성되는 정책 맵(`cilium_policy_v2_{ep_id}`)을 구현한다. `Lookup()`은
LPM(Longest Prefix Match) 방식의 3단계 폴백 조회를 수행한다: (1) 정확한 키 매칭,
(2) 포트 와일드카드(`DestPort=0`), (3) 프로토콜 와일드카드(`Protocol=0`).
매칭에 실패하면 기본 거부(default deny)를 반환한다.

### 4. PerCPUMetricsMap (메트릭 맵 시뮬레이션)

`data[key][cpu_id]` 2차원 구조로 Per-CPU 메모리 레이아웃을 재현한다.
`Increment()`는 특정 CPU의 카운터만 갱신하고, `ReadAll()`은 모든 CPU 값을 합산하여 반환한다.
`IterateWithCallback()`은 Prometheus Collector(`metricsmapCollector`)가 `/metrics`
엔드포인트에서 메트릭을 노출하는 패턴을 재현한다.

### 5. BPFMap 공통 인터페이스

`BPFMap` 인터페이스는 `pkg/bpf/map_linux.go`의 `Map` 구조체가 제공하는 공통 메서드
(`Name()`, `Type()`, `MaxEntries()`, `Count()`, `Dump()`)를 추상화한다.
세 가지 맵 타입 모두 이 인터페이스를 구현한다.

## 실행 방법

```bash
cd cilium_EDU/poc-05-bpf-maps
go run main.go
```

### 예상 출력 (요약)

```
시나리오 1: Connection Tracking Map (LRU Hash)
  [CT] 연결 6개 추가 -> GC 실행 -> 만료 엔트리 1개 제거 (lifetime=900)
  [CT] LRU 퇴출 시연 -> max=8 초과 시 가장 오래된 엔트리 자동 퇴출
  [CT] IP 기반 GC -> 특정 IP(10.0.1.1) 관련 엔트리 선택 제거

시나리오 2: Policy Map (Hash + LPM 매칭)
  id=100 Ingress TCP:80  -> ALLOW proxy=15001  (정확한 매칭)
  id=100 Ingress TCP:8080 -> DROP               (매칭 실패, 기본 거부)
  id=200 Egress TCP:443  -> ALLOW               (프로토콜+포트 와일드카드)
  id=300 Ingress TCP:22  -> DENY                (정확한 DENY 매칭)
  id=300 Ingress TCP:80  -> ALLOW               (포트 와일드카드 폴백)

시나리오 3: Metrics Map (Per-CPU 카운터)
  CPU별 독립 카운터 갱신 -> ReadAll()로 합산
  cilium_forward_count_total{direction="ingress"} NNNNN
  cilium_drop_count_total{reason="POLICY_DENIED",direction="ingress"} NNN
```

랜덤 값이 포함되어 숫자는 실행마다 다르다.

## 핵심 포인트

1. **LRU 퇴출과 GC는 별개의 메커니즘이다**: LRU 퇴출은 커널이 용량 초과 시 자동 수행하고,
   GC는 Cilium 에이전트가 주기적으로 만료된 엔트리를 명시적으로 제거한다. 두 메커니즘이 함께
   동작하여 CT 맵의 크기를 적정 수준으로 유지한다.

2. **LPM 폴백 조회가 정책 유연성의 핵심이다**: 정확한 매칭이 실패하면 포트 와일드카드, 프로토콜
   와일드카드 순으로 폴백하므로, 세밀한 정책과 넓은 정책을 하나의 맵에서 공존시킬 수 있다.
   DENY가 ALLOW보다 항상 우선하는 것은 같은 수준에서 정확히 매칭되는 키가 먼저 반환되기 때문이다.

3. **Per-CPU 맵은 고성능 통계의 핵심이다**: CPU별 독립 메모리 영역 덕분에 BPF 프로그램은
   락 없이 카운터를 갱신할 수 있다. 유저스페이스에서 읽을 때만 합산하므로 데이터플레인 성능에
   영향을 주지 않는다.

4. **맵 이름 규칙이 운영의 기본이다**: `cilium_ct4_global`, `cilium_policy_v2_{ep_id}`,
   `cilium_metrics` 등 맵 이름을 알면 `bpftool map dump name <맵이름>`으로 직접 내용을
   확인할 수 있다.

## 실제 Cilium과의 차이점

| 항목 | 이 시뮬레이션 | 실제 Cilium |
|------|-------------|------------|
| LRU 퇴출 | Go `container/list`로 구현 | 커널 `BPF_MAP_TYPE_LRU_HASH`가 내부적으로 수행 |
| GC 실행 | 동기 호출 `RunGC()` | `gc/gc.go`에서 30초 간격 타이머로 비동기 실행 |
| GC 후 NAT 정리 | 미구현 | CT 엔트리 삭제 시 연관된 NAT 맵 엔트리도 함께 제거 |
| LPM 조회 | 3단계 폴백 (`if-else`) | 커널 `BPF_MAP_TYPE_LPM_TRIE` + Prefixlen 기반 조회 |
| Per-CPU | `[]MetricsValue` 슬라이스 | 커널이 CPU별 독립 메모리 페이지를 할당 |
| 동시성 | `sync.RWMutex` | 커널 BPF 맵은 RCU 기반 lock-free 읽기, spin-lock 쓰기 |
| 캐시 동기화 | 없음 | `pkg/bpf/map_linux.go`에서 유저스페이스 캐시와 커널 맵 간 비동기 동기화 |
| 맵 핀닝 | 없음 | `/sys/fs/bpf/tc/globals/`에 맵을 핀닝하여 프로세스 재시작 후에도 유지 |
| Prometheus 노출 | 콘솔 출력 | `metricsmapCollector`가 Prometheus Collector 인터페이스를 구현하여 `/metrics` 제공 |

## 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/maps/ctmap/ctmap.go` | CT 맵 구조체, CtMap 인터페이스, GCFilter |
| `pkg/maps/ctmap/types.go` | CtEntry (Packets, Bytes, Lifetime, Flags), CtKey4Global |
| `pkg/maps/ctmap/gc/gc.go` | GC 구조체, ConntrackGCInterval, 만료 엔트리 제거 |
| `pkg/maps/policymap/policymap.go` | PolicyMap, PolicyKey, PolicyEntry, policyEntryFlags |
| `pkg/maps/metricsmap/metricsmap.go` | Key, Value, metricsmapCollector (Prometheus Collector) |
| `pkg/bpf/map_linux.go` | Map 구조체, MapKey/MapValue 인터페이스, 캐시 동기화 |
