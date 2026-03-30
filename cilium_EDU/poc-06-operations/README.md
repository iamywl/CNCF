# PoC-06: Cilium 운영 시뮬레이션

## 개요

Cilium 에이전트의 운영(Operations) 관련 핵심 메커니즘을 Go 표준 라이브러리만으로 시뮬레이션한다.
YAML 설정 파일 로딩/검증, 헬스 체크 엔드포인트, Prometheus 메트릭 수집 패턴의 세 가지 축을 재현하여,
프로덕션 환경에서 Cilium을 안정적으로 운영하기 위한 내부 동작 원리를 이해한다.

## 배경 지식

### Cilium 운영의 3대 핵심 요소

Cilium 에이전트는 Kubernetes 클러스터의 각 노드에서 DaemonSet으로 실행되며, 운영 관점에서 세 가지 핵심 요소를 갖는다.

**1. 설정 관리 (DaemonConfig)**

Cilium은 `DaemonConfig`라는 1400개 이상의 필드를 가진 거대한 구조체로 설정을 관리한다.
ConfigMap(YAML), 환경변수(`CILIUM_` 접두사), CLI 플래그를 viper로 통합 로드한다.
ConfigMap 기반의 장점은 K8s 네이티브 변경/롤백이 가능하다는 것이고,
단점은 크기 제한(1MB)과 일부 설정 변경 시 에이전트 재시작이 필요하다는 점이다.

**2. 헬스 체크 (cilium-health)**

`CiliumHealthManager`는 Hive Cell로 에이전트 라이프사이클을 따르며,
60초 간격으로 모든 노드를 ICMP/HTTP로 프로빙하여 L3~L7 연결성을 확인한다.
5분간 성공 응답이 없으면(`successfulPingTimeout`) health endpoint를 자동 재시작한다.

**3. 메트릭 수집 (Prometheus)**

`metricsmapCollector`가 BPF 메트릭 맵에서 커널 수준 데이터를 읽어 Prometheus 형식으로 변환한다.
`:9962/metrics`로 패킷 포워딩/드롭 카운터, 엔드포인트 수, 정책 상태 등을 노출한다.
Per-CPU 값을 합산하여 cardinality를 낮게 유지한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | PoC 시뮬레이션 |
|------|-----------------|---------------|
| 설정 로드 | `viper` + ConfigMap + 환경변수 + CLI | 간이 YAML 파서 + `DaemonConfig` 구조체 |
| 설정 검증 | `DaemonConfig.Validate()` | 값 범위, 호환성, 필수 필드 검증 로직 |
| 변경 감지 | `ValidateUnchanged()` (SHA256 체크섬) | 체크섬 비교 필드 (`shaSum`) |
| 헬스 체크 | `CiliumHealthManager` + `prober` | `HealthServer` 컴포넌트 + 노드 프로빙 |
| ICMP/HTTP 프로브 | `prober.probeNodes()` | 시뮬레이션된 프로브 결과 (랜덤 지연/실패) |
| 메트릭 수집 | `metricsmapCollector` + `pkg/metrics/` | `MetricRegistry` Counter/Gauge 등록 |
| Prometheus 노출 | `/metrics` 엔드포인트 | text/plain exposition format 직접 생성 |

## 아키텍처 다이어그램

```
+-------------------------------------------------------------------+
|                        cilium-agent                                |
|                                                                   |
|  +------------------+  +------------------+  +-----------------+  |
|  | DaemonConfig     |  | HealthManager    |  | MetricRegistry  |  |
|  | (pkg/option/)    |  | (pkg/health/)    |  | (pkg/metrics/)  |  |
|  |                  |  |                  |  |                 |  |
|  | YAML+Env+CLI     |  | ICMP/HTTP Probe  |  | Prometheus      |  |
|  | -> Validate()    |  | -> 60s interval  |  | -> /metrics     |  |
|  | -> checksum()    |  | -> node health   |  | -> Counter      |  |
|  +--------+---------+  +--------+---------+  | -> Gauge        |  |
|           |                     |             +---------+-------+  |
|           v                     v                       |          |
|  +--------+---------+  +-------+----------+             |          |
|  | viper            |  | cilium-health    |     :9962/metrics      |
|  | ConfigMap watch  |  | :4240/healthz    |                        |
|  +------------------+  +------------------+                        |
+-------------------------------------------------------------------+

설정 로드:
ConfigMap (K8s) --+
환경변수         --+--> viper --> DaemonConfig --> Validate() --> 각 서브시스템
CLI 플래그       --+

헬스 체크:
+--------+     +----------+     +----------+
| Timer  |---->| 노드 목록|---->| ICMP/HTTP|---> 결과 캐시
| (60초) |     | 조회     |     | 프로빙   |---> /healthz API
+--------+     +----------+     +----------+
```

## 코드 해설

### 1. DaemonConfig 구조체

Cilium 에이전트의 전체 설정을 담는 핵심 구조체이다. 실제 `pkg/option/config.go`에서
1400개 이상의 필드를 갖지만, PoC에서는 네트워킹(TunnelMode, RoutingMode, MTU),
BPF(CTMapEntriesTCP, PolicyMapMaxEntries), 보안(EnablePolicy, EnableL7Proxy) 등
대표 필드만 포함한다. `shaSum` 필드는 런타임 설정 변경 감지를 위한 SHA256 체크섬 저장 용도이다.

### 2. Validate() 메서드

설정값의 정합성을 검증한다. 클러스터 이름/ID 범위, 터널/라우팅 모드 유효성, BPF 맵 크기 최솟값(1024),
그리고 **상호 호환성**(native 라우팅에서는 tunnel=disabled 필수)까지 검사한다.
실제 코드에서는 IPv6 CIDR, IPAM 호환성 등 훨씬 더 많은 교차 검증이 이루어진다.

### 3. HealthServer 구조체

`cilium-health` 서버를 시뮬레이션한다. `sync.RWMutex`로 동시 접근을 보호하며,
`components` 맵에 컴포넌트 상태(ok/warning/error/degraded), `nodes` 맵에 노드별 프로브 결과를 저장한다.
`ProbeNode()`는 랜덤 지연과 실패율로 실제 네트워크 프로빙을 시뮬레이션한다.

### 4. MetricRegistry와 Exposition()

Prometheus 메트릭 레지스트리를 구현한다. Counter/Gauge 타입을 지원하며,
`Exposition()` 메서드가 `# HELP`/`# TYPE`/메트릭 라인을 text exposition format으로 직접 생성한다.
레이블 정렬, 중복 HELP/TYPE 방지, `cilium_` 네임스페이스 접두사를 재현한다.

### 5. startHTTPServer 함수

`/healthz`(상태 ok 아니면 503), `/status`(상세 JSON), `/metrics`(exposition format) 엔드포인트를 제공한다.
실제로는 에이전트(:9962)와 cilium-health(:4240)가 분리되지만, PoC에서는 단일 서버로 통합했다.

## 실행 방법

```bash
cd cilium_EDU/poc-06-operations
go run main.go
```

프로그램이 `sample-config.yaml`을 자동 생성하고, 로드한 뒤 삭제한다.
임시 HTTP 서버가 시작되어 `/healthz`, `/metrics` 요청이 실제로 수행된다.

### 예상 출력 (주요 부분 발췌)

```
시나리오 1: YAML 설정 파일 로드 및 검증
  [config] 로드된 설정:
    cluster-name:           production-cluster
    tunnel:                 vxlan
    routing-mode:           tunnel
  [config] 설정 검증: 모든 검증 통과
  [config] 잘못된 설정 검증 테스트:
    오류: cluster-name은 비어있을 수 없음
    오류: cluster-id는 0-255 범위여야 함: 300
    오류: native 라우팅에서는 tunnel=disabled여야 함

시나리오 2: 헬스 체크 엔드포인트
  [health] 노드 프로빙:
    node-01 (10.0.0.1): ICMP: ok (3ms), HTTP: ok (12ms)
    node-02 (10.0.0.2): ICMP: ok (7ms), HTTP: ok (28ms)

시나리오 3: Prometheus 메트릭 수집
    # HELP cilium_forward_count_total Total forwarded packets
    # TYPE cilium_forward_count_total counter
    cilium_forward_count_total{direction="ingress"} 1523456

시나리오 4: HTTP 엔드포인트 시연
  [http] GET http://localhost:19xxx/healthz
    상태 코드: 200 (또는 503)
    응답: {"status":"degraded", ...}
```

## 핵심 포인트

| 포인트 | 설명 |
|--------|------|
| 설정 검증은 방어적으로 | `Validate()`는 개별 필드뿐 아니라 필드 간 **상호 호환성**까지 검증한다 (예: native 라우팅 + tunnel 조합) |
| 헬스 체크는 L3~L7 전 계층 | ICMP(L3)와 HTTP(L7) 프로브를 병렬 수행하여 네트워크 계층별 장애를 분리 진단한다 |
| 메트릭은 BPF 맵 기반 | 커널 BPF 맵에서 직접 읽어오므로 사용자 공간 오버헤드 없이 정확한 패킷 통계를 수집한다 |
| Exposition format 직접 생성 | Prometheus 클라이언트 라이브러리 없이도 `# HELP`/`# TYPE`/메트릭 라인만 올바르게 출력하면 스크레이핑 가능 |
| 컴포넌트별 상태 분리 | 7개 컴포넌트(agent, health, k8s, kvstore, endpoint, policy, datapath) 각각의 상태를 독립 추적한다 |

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|------------|--------|
| 설정 로드 | viper로 YAML/Env/CLI 통합, ConfigMap watch 동적 갱신 | 간이 YAML 파서 (key: value 파싱) |
| 설정 필드 수 | 1400개 이상 (`pkg/option/config.go`) | 약 20개 대표 필드 |
| 헬스 프로브 | 실제 ICMP 소켓 + HTTP 클라이언트 | `math/rand`로 지연/실패 시뮬레이션 |
| 프로브 대상 | Kubernetes Node 목록에서 동적 조회 | 하드코딩된 4개 노드 |
| 메트릭 소스 | BPF 맵에서 Per-CPU 값 합산 | 인메모리 카운터 직접 설정 |
| HTTP 포트 | 에이전트(:9962) + cilium-health(:4240) 분리 | 단일 랜덤 포트에 통합 |
| Hive 통합 | Cell 기반 라이프사이클, 의존성 주입 | 단순 함수 호출 |

## 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/option/config.go` | DaemonConfig (1400+ 필드), Validate(), ValidateUnchanged() |
| `pkg/health/health_manager.go` | CiliumHealthManager, controllerInterval(60s) |
| `pkg/health/server/prober.go` | ICMP/HTTP 프로빙, probeInterval |
| `pkg/maps/metricsmap/metricsmap.go` | metricsmapCollector, Prometheus Collector + `pkg/metrics/` 레지스트리 |
