# PoC-12: ClusterMesh 멀티클러스터 시뮬레이션

## 개요

Cilium ClusterMesh의 멀티클러스터 서비스 디스커버리 및 로드밸런싱 메커니즘을 Go 표준 라이브러리만으로 재현한다.
3개 클러스터(us-east, eu-west, ap-south) 환경에서 etcd 기반 상태 동기화, KVStoreMesh 캐싱 프록시,
GlobalServiceCache 서비스 병합, ServiceAffinity 기반 클러스터 인식 라우팅까지의 전체 흐름을 시뮬레이션한다.

## 배경 지식

### 멀티클러스터 네트워킹이 필요한 이유

단일 Kubernetes 클러스터는 장애 도메인, 리전 분산, 규모 한계 등의 문제가 있다.
멀티클러스터 환경에서는 동일한 서비스가 여러 클러스터에 배포되며, 클러스터 간 트래픽 라우팅이 필요하다.
Cilium의 ClusterMesh는 이를 **별도의 게이트웨이 없이** 각 클러스터의 Cilium Agent가 직접 원격 백엔드로
트래픽을 전달하는 방식으로 해결한다.

### ClusterMesh 아키텍처 - etcd 미러링 기반 서비스 디스커버리

ClusterMesh의 핵심은 **각 클러스터의 etcd에 저장된 상태 정보를 상호 미러링**하는 것이다.
각 클러스터의 Cilium Agent는 자신의 서비스, Identity, 노드 정보를 로컬 etcd에 게시한다.
KVStoreMesh가 원격 클러스터의 etcd를 Watch하여 로컬 etcd에 캐시로 복제하면,
Cilium Agent는 로컬 캐시만 읽어 원격 클러스터의 상태를 파악할 수 있다.

이 구조는 원격 etcd에 대한 직접 연결 부하를 줄이고, 네트워크 단절 시에도 캐시된 데이터로 동작을 유지한다.

### Global Service의 개념

여러 클러스터에 동일한 namespace/name으로 배포된 서비스 중 `Shared=true`로 표시된 것들은
**GlobalService**로 병합된다. 예를 들어 us-east와 eu-west 모두에 `default/web-api`가 있으면,
GlobalServiceCache가 두 클러스터의 백엔드를 하나의 논리적 서비스로 통합한다.
`Shared=false`인 서비스(예: database)는 해당 클러스터 내부에서만 접근 가능하다.

### ServiceAffinity - 로컬 우선 vs 원격 우선

GlobalService의 백엔드 선택 시 ServiceAffinity 정책이 적용된다:

- **none**: 모든 클러스터의 백엔드를 균등하게 사용 (글로벌 로드밸런싱)
- **local**: 로컬 클러스터 백엔드를 우선 사용하고, 로컬이 없을 때만 원격으로 폴백 (지연 최소화)
- **remote**: 원격 클러스터 백엔드를 우선 사용하고, 원격이 없을 때만 로컬로 폴백 (DR 시나리오)

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 소스 | 시뮬레이션 내용 |
|------|-----------------|----------------|
| KVStore (etcd) | `pkg/kvstore/` | Watch 기반 이벤트 전파, prefix 검색 |
| ClusterService | `pkg/clustermesh/store/store.go` | Shared/Backend/ClusterID 포함 서비스 모델 |
| KVStoreMesh | `pkg/clustermesh/kvstoremesh/kvstoremesh.go` | `cilium/state/` → `cilium/cache/` 미러링 |
| RemoteCluster | `pkg/clustermesh/remote_cluster.go` | 원격 KVStore 연결 및 서비스/Identity 동기화 |
| GlobalServiceCache | `pkg/clustermesh/common/services.go` | OnUpdate/OnDelete로 멀티클러스터 서비스 병합 |
| SelectBackends | `pkg/clustermesh/selectbackends.go` | ServiceAffinity 기반 백엔드 필터링 |
| IdentityCache | `pkg/allocator/` | 클러스터 간 보안 Identity 동기화 |

## 아키텍처 다이어그램

```
  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
  │  us-east     │    │  eu-west     │    │  ap-south    │
  │  etcd (KV)   │    │  etcd (KV)   │    │  etcd (KV)   │
  │  services    │    │  services    │    │  services    │
  │  identities  │    │  identities  │    │  identities  │
  └──────┬───────┘    └──────┬───────┘    └──────┬───────┘
         │                   │                    │
         ▼                   ▼                    ▼
  ┌─────────────────────────────────────────────────────┐
  │  KVStoreMesh (캐싱 프록시)                            │
  │  cilium/state/ → cilium/cache/ 접두사 변환            │
  └──────────────────────┬──────────────────────────────┘
                         │
                         ▼
  ┌─────────────────────────────────────────────────────┐
  │  ClusterMesh (로컬 Agent)                            │
  │  ┌───────────────────────────────────────────────┐  │
  │  │  GlobalServiceCache                           │  │
  │  │  default/web-api:                             │  │
  │  │    us-east → [10.1.1.1, 10.1.1.2]            │  │
  │  │    eu-west → [10.2.1.1, .2, .3]              │  │
  │  │    ap-south → [10.3.1.1]                     │  │
  │  └─────────────────────┬─────────────────────────┘  │
  │                        ▼                            │
  │  SelectBackends (ServiceAffinity: none/local/remote)│
  └─────────────────────────────────────────────────────┘
```

### KVStore 키 구조

```
cilium/state/services/v1/<cluster>/<namespace>/<name>   → ClusterService JSON
cilium/state/identities/v1/<cluster>/<id>               → IdentityEntry JSON
cilium/state/nodes/v1/<cluster>/<node>                  → Node JSON

KVStoreMesh 미러링 시 접두사 변환:
  cilium/state/  →  cilium/cache/
```

## 코드 해설

### ClusterService 구조체

KVStore에 게시되는 서비스 단위 데이터 모델이다. `Shared` 필드가 `true`인 서비스만 멀티클러스터에서
GlobalService로 병합된다. `Backends` 맵은 IP 주소를 키로, 포트 설정을 값으로 가진다.
실제 Cilium에서 이 구조체는 Stable API로 관리되어 하위 호환성이 보장된다.

### GlobalServiceCache.OnUpdate()

서비스 업데이트 이벤트를 받아 `namespace/name` 키로 GlobalService에 병합한다.
동일한 namespace/name을 가진 여러 클러스터의 서비스가 하나의 GlobalService 아래 모인다.
`ClusterServices` 맵에 클러스터 이름을 키로 각 클러스터의 ClusterService를 저장한다.

### KVStoreMesh.SyncAll()

모든 원격 클러스터의 KVStore에서 services, identities, nodes 접두사 데이터를 읽어
로컬 KVStore에 캐시한다. 키 접두사를 `cilium/state/`에서 `cilium/cache/`로 변환하여
원본과 캐시를 구분한다. 이 설계로 Cilium Agent는 로컬 etcd만 접근하면 된다.

### ClusterMesh.SelectBackends()

GlobalServiceCache에서 서비스를 조회한 뒤, 각 백엔드를 로컬/리모트로 분류한다.
ServiceAffinity 값에 따라 반환할 백엔드 집합을 결정한다.
`local` 모드에서 로컬 백엔드가 없으면 리모트로 폴백하는 장애 복구 로직이 핵심이다.

### RemoteCluster.SyncServices()

원격 KVStore에서 해당 클러스터의 서비스 키를 prefix 검색으로 조회한다.
JSON 디코딩 후 `Shared=true`인 서비스만 GlobalServiceCache에 등록한다.
이는 실제 Cilium에서 WatchStore를 통한 이벤트 기반 동기화를 단순화한 것이다.

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-12-multicluster
go run main.go
```

예상 출력 (주요 부분):

```
━━━ 데모 3: KVStoreMesh 동기화 (캐싱 프록시) ━━━
  미러링: cilium/state/ → cilium/cache/
    us-east: 3개 키 | eu-west: 2개 키 | ap-south: 2개 키
  로컬 캐시: 7개 키

━━━ 데모 5: Cluster-aware 라우팅 (ServiceAffinity) ━━━
  affinity='local' (로컬 우선):
    10.1.1.1:8080/TCP [us-east/local]
    10.1.1.2:8080/TCP [us-east/local]
  [시뮬레이션] 로컬 백엔드 다운 → 폴백:
    폴백 백엔드 (4개): 10.2.1.1:8080/TCP [eu-west/remote] ...

━━━ 데모 7: 멀티클러스터 부하 분산 ━━━
  1000 요청 (affinity=none):
    us-east   :  280 ██████████████
    eu-west   :  520 ██████████████████████████
    ap-south  :  200 ██████████
```

## 핵심 포인트

1. **Shared 플래그가 멀티클러스터의 게이트키퍼**: `Shared=false`인 서비스(예: database)는 클러스터 경계를 넘지 않는다. 민감한 서비스를 로컬로 격리하는 보안 메커니즘이다.

2. **KVStoreMesh가 확장성의 핵심**: 원격 etcd 직접 연결 대신 캐싱 프록시를 두어, N개 Agent가 원격 etcd에 N개 연결을 맺는 대신 KVStoreMesh 하나만 연결한다. O(N) → O(1) 연결 절감.

3. **ServiceAffinity의 폴백 설계**: `local` 모드에서 로컬 백엔드가 전멸하면 자동으로 리모트로 폴백한다. 명시적 장애 조치 설정 없이 고가용성을 확보하는 설계다.

4. **prefix 변환으로 원본/캐시 분리**: `cilium/state/` → `cilium/cache/` 변환은 단순하지만, 원본 데이터와 캐시 데이터를 같은 etcd에 안전하게 공존시키는 핵심 설계다.

5. **부하 분산은 백엔드 수에 비례**: `affinity=none` 시 eu-west(3개 백엔드)가 us-east(2개)보다 더 많은 트래픽을 받는다. 백엔드 수 기반 자연스러운 가중치 분배다.

## 실제 Cilium과의 차이점

| 항목 | 이 PoC | 실제 Cilium |
|------|--------|------------|
| KVStore | 인메모리 맵 기반 | etcd 클라이언트, TLS 인증, 리스/TTL |
| Watch 메커니즘 | 채널 기반 단순 알림 | etcd Watch API, 리비전 기반 증분 동기화 |
| KVStoreMesh | 즉시 전체 복사 | 점진적 동기화, 재시도, 헬스체크 |
| Identity 동기화 | 단순 캐시 | CRD 기반 할당, 충돌 해소, ClusterID 접두사 |
| 서비스 병합 | GlobalServiceCache만 | serviceMerger → BPF LB 맵 업데이트 |
| 네트워크 | 시뮬레이션 없음 | VxLAN/Geneve 터널 또는 직접 라우팅 |
| 장애 감지 | 없음 | 헬스체크, 연결 끊김 감지, 자동 재연결 |
| ClusterID 관리 | 정적 할당 | idsmgr.go의 동적 할당/해제, 255개 제한 |
