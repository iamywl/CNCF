# PoC 12: Cilium ClusterMesh 멀티클러스터 메커니즘 시뮬레이션

Cilium ClusterMesh의 핵심 메커니즘을 순수 Go(stdlib만 사용)로 시뮬레이션한다.

---

## 구조

```
이 PoC가 재현하는 패턴:

Cluster A (ID=1)    Cluster B (ID=2)    Cluster C (ID=3)
     |                   |                   |
     +------- etcd ------+------- etcd ------+
              |                    |
     GlobalServiceCache   RemoteIdentityCache
              |                    |
     ServiceAffinity     CrossCluster Policy
```

## 시뮬레이션 대상

| 데모 | 설명 | 참조 소스 |
|------|------|-----------|
| DEMO 1 | ClusterID 기반 Identity 충돌 방지 | `pkg/clustermesh/idsmgr.go` |
| DEMO 2 | 글로벌 서비스 동기화 (Shared 서비스만) | `pkg/clustermesh/common/services.go` |
| DEMO 3 | ServiceAffinity (local/remote/none) | `pkg/clustermesh/selectbackends.go` |
| DEMO 4 | 크로스 클러스터 네트워크 정책 평가 | `pkg/clustermesh/types/option.go` |
| DEMO 5 | WatcherCache (stale 데이터 정리) | `pkg/kvstore/watcher_cache.go` |
| DEMO 6 | Cluster ID 충돌 감지 | `pkg/clustermesh/idsmgr.go` |
| DEMO 7 | 실시간 서비스 업데이트 (Watch) | `pkg/clustermesh/remote_cluster.go` |
| DEMO 8 | 크로스 클러스터 Identity 매핑 조회 | `pkg/identity/cache/allocator.go` |

## 실행 방법

```bash
cd EDU/poc-12-multicluster
go run main.go
```

## 기대 출력

```
======================================================================
Cilium ClusterMesh PoC - 멀티클러스터 메커니즘 시뮬레이션
======================================================================

======================================================================
DEMO 1: 클러스터 설정 및 ClusterID 기반 Identity 충돌 방지
======================================================================

클러스터 생성 완료:
  - cluster-a (ID=1)
  - cluster-b (ID=2)
  - cluster-c (ID=3)

--- 같은 라벨 {app:frontend}로 각 클러스터에서 Identity 할당 ---

  cluster-a   : GlobalID=65537    (ClusterID=1, LocalID=1)
  cluster-b   : GlobalID=131073   (ClusterID=2, LocalID=1)
  cluster-c   : GlobalID=196609   (ClusterID=3, LocalID=1)

  => 같은 라벨이지만 ClusterID 덕분에 모든 Identity가 고유합니다!
```

## 핵심 개념 매핑

### 1. Identity 충돌 방지

실제 Cilium에서 Security Identity는 24비트이며, 상위 비트에 ClusterID가 인코딩된다:

```
GlobalIdentity = (ClusterID << 16) | LocalIdentity

cluster-a (ID=1): Identity 1 -> GlobalID = (1 << 16) | 1 = 65537
cluster-b (ID=2): Identity 1 -> GlobalID = (2 << 16) | 1 = 131073
```

참조: `pkg/clustermesh/types/types.go`

### 2. 글로벌 서비스 캐시

실제 Cilium의 `GlobalServiceCache`는 `NamespacedName`을 키로, 각 클러스터의 `ClusterService`를 값으로 관리한다:

```go
// 실제 코드: pkg/clustermesh/common/services.go
type GlobalService struct {
    ClusterServices map[string]*serviceStore.ClusterService
}

type GlobalServiceCache struct {
    byName map[types.NamespacedName]*GlobalService
}
```

서비스는 `service.cilium.io/global: "true"` 어노테이션이 있을 때만 공유된다.

### 3. ServiceAffinity

`pkg/clustermesh/selectbackends.go`의 백엔드 선택 로직:

- `none`: 로컬 + 원격 모든 백엔드 사용
- `local`: 로컬 건강한 백엔드 우선, 없으면 원격
- `remote`: 원격 백엔드 우선, 없으면 로컬

### 4. 동기화 흐름

```
K8s Resource -> clustermesh-apiserver -> etcd (로컬)
                                            |
                                     etcd Watch (원격 에이전트)
                                            |
                                     GlobalServiceCache 업데이트
                                            |
                                     BPF 데이터패스 갱신
```

### 5. WatcherCache 패턴

etcd 재연결 시 stale 데이터를 정리하는 3단계 패턴:

```
MarkAllForDeletion()  -> 모든 기존 키를 삭제 후보로 표시
MarkInUse(key)        -> List 결과에 포함된 키는 복원
RemoveDeleted()       -> 여전히 삭제 후보인 키 제거 (stale)
```

참조: `pkg/kvstore/watcher_cache.go`

## 소스 파일 참조

| PoC 구현 | Cilium 실제 소스 |
|----------|-----------------|
| `ClusterInfo` | `pkg/clustermesh/types/option.go` - `ClusterInfo` |
| `ClusterService` | `pkg/clustermesh/store/store.go` - `ClusterService` |
| `ClusterMeshUsedIDs` | `pkg/clustermesh/idsmgr.go` - `ClusterMeshUsedIDs` |
| `GlobalServiceCache` | `pkg/clustermesh/common/services.go` - `GlobalServiceCache` |
| `RemoteIdentityCache` | `pkg/clustermesh/remote_cluster.go` - `remoteIdentityCache` |
| `WatcherCache` | `pkg/kvstore/watcher_cache.go` - `watcherCache` |
| `KVStore` | `pkg/kvstore/etcd.go` - etcd 백엔드 |
| `SyncRemoteCluster` | `pkg/clustermesh/remote_cluster.go` - `Run` |
| `DiscoverBackends` | `pkg/clustermesh/selectbackends.go` - `SelectBackends` |
| `EvaluatePolicy` | `pkg/policy/` + `pkg/clustermesh/types/option.go` |
| `NetworkPolicy.AllowedCluster` | `io.cilium.k8s.policy.cluster` 라벨 선택자 |
