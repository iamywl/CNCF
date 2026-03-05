# PoC 16: 클러스터 샤딩 (Cluster Sharding)

## 개요

Argo CD의 클러스터 샤딩 시스템(`controller/sharding/`)을 Go 표준 라이브러리만으로 시뮬레이션합니다.

고가용성 모드에서 여러 Application Controller 레플리카가 클러스터를 분담 관리하는 세 가지 알고리즘과, 하트비트 기반 데드 샤드 감지/인계 메커니즘을 구현합니다.

## 실행

```bash
go run main.go
```

## 핵심 개념

### 1. 샤딩 아키텍처

```
argocd-application-controller (StatefulSet)
├── Pod 0 (샤드 0) → cluster-001, cluster-004, cluster-007
├── Pod 1 (샤드 1) → cluster-002, cluster-005, cluster-008
└── Pod 2 (샤드 2) → cluster-003, cluster-006, cluster-009
```

각 Pod는 자신이 담당하는 샤드의 클러스터만 조정(Reconcile)합니다.

### 2. 분배 알고리즘 비교

| 알고리즘 | 분배 방식 | 특징 |
|----------|-----------|------|
| `legacy` | FNV-32a(clusterID) % replicas | 단순, 균등성 보장 없음 |
| `round-robin` | sort(clusters)[i] % replicas | 결정론적, 균등 분배 |
| `consistent-hashing` | Bounded Loads (capacityFactor=1.25) | 확장/축소 시 이동 최소화 |

#### Legacy (FNV-32a)

```go
func LegacyDistribute(clusters []*Cluster, replicas int) map[string]int {
    for _, c := range clusters {
        shard := fnv32aHash(c.ID) % replicas
    }
}
```

#### RoundRobin

```go
// 클러스터를 ID 순으로 정렬 후 인덱스 기반 할당
sorted := sort.Slice(clusters, by ID)
shard = index % replicas
```

#### ConsistentHashing (Bounded Loads)

```
선호 샤드 = FNV32a(clusterID) % replicas
if 선호 샤드 부하 < maxLoad:
    → 선호 샤드 할당
else:
    → 최소 부하 샤드(GetLeast()) 할당

maxLoad = ceil((total+1) * 1.25 / replicas)
```

### 3. ClusterShardingCache 인터페이스

```go
type ClusterShardingCache interface {
    Init(clusters []*Cluster, replicas int)
    Add(cluster *Cluster)
    Delete(clusterID string)
    Update(cluster *Cluster)
    IsManagedCluster(clusterID string) bool
    GetShardNumber(clusterID string) (int, bool)
}
```

클러스터 추가/삭제/변경 시 자동으로 전체 재분배가 수행됩니다.

### 4. 하트비트 프로토콜

```
상수:
  HeartbeatDuration = 10s  (하트비트 전송 주기)
  HeartbeatTimeout  = 30s  (3 * HeartbeatDuration)

ConfigMap 구조:
  argocd-app-controller-shard-lock
  data:
    "shard-0": {hostname, heartbeatAt}
    "shard-1": {hostname, heartbeatAt}
    "shard-2": {hostname, heartbeatAt}

데드 감지:
  now - heartbeatAt > 30s → 데드 샤드
```

### 5. 샤드 번호 결정 우선순위

```
getOrUpdateShardNumberForController(hostname, replicas, configMap)
        │
        ▼
① StatefulSet 호스트명 파싱
   "argocd-application-controller-2" → 샤드 2
        │
        ▼ (실패 시)
② ConfigMap에서 빈 슬롯 탐색
   (등록되지 않은 샤드 번호)
        │
        ▼ (없을 시)
③ 데드 샤드 인계
   (HeartbeatTimeout 초과 샤드)
```

### 6. StatefulSet 호스트명 파싱

```go
func InferShard(hostname string) (int, error) {
    parts := strings.Split(hostname, "-")
    suffix := parts[len(parts)-1]
    return strconv.Atoi(suffix)
}

// 예시:
// "argocd-application-controller-0" → 0
// "argocd-application-controller-2" → 2
// "app-controller-abc" → 오류
```

## 시뮬레이션 시나리오

| 시나리오 | 내용 |
|----------|------|
| 1 | 3가지 알고리즘 비교 — 균등성 수치(표준편차) 포함 |
| 2 | ClusterShardingCache — 추가/삭제/IsManagedCluster |
| 3 | 하트비트 타임아웃 → 데드 샤드 감지 |
| 4 | StatefulSet 호스트명 파싱 — 정상/비정상 케이스 |
| 5 | getOrUpdateShardNumberForController — 3가지 할당 경로 |
| 6 | 레플리카 확장(3→5) 및 축소(5→3) 시 재분배 |

## 실제 Argo CD 코드 참조

| 구성요소 | 소스 위치 |
|----------|-----------|
| 분배 알고리즘 | `controller/sharding/sharding.go` |
| ConsistentHashing | `controller/sharding/consistent_hashing.go` |
| ClusterShardingCache | `controller/sharding/sharding.go` |
| 하트비트 | `controller/sharding/sharding.go:heartbeatCurrentShard()` |
| InferShard | `controller/sharding/sharding.go:InferShard()` |
| getOrUpdateShard | `controller/sharding/sharding.go:getOrUpdateShardNumberForController()` |
| ConfigMap 키 | `argocd-app-controller-shard-lock` |
