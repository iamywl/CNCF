# PoC 11: 캐싱 레이어

## 개요

Argo CD의 3계층 캐싱 아키텍처를 Go 표준 라이브러리만으로 시뮬레이션한다.
각 레이어는 서로 다른 목적과 저장 방식을 가진다.

## 참조 소스 코드

| 파일 | 역할 |
|------|------|
| `gitops-engine/pkg/cache/cluster.go` | ClusterCache 인터페이스, parentUIDToChildren 인덱스 |
| `util/cache/appstate/cache.go` | App State Cache (managed-resources, resources-tree) |
| `reposerver/cache/cache.go` | Manifest Cache (CachedManifestResponse) |
| `reposerver/repository/repository.go` | double-check locking, PauseGeneration circuit breaker |

## 핵심 개념

### 캐싱 레이어 전체 구조

```
┌─────────────────────────────────────────────────────────┐
│ Layer 1: Cluster Cache (인메모리)                         │
│   - Watch 이벤트 기반 실시간 동기화                          │
│   - parentUIDToChildren: O(1) 리소스 트리 탐색             │
│   - FindResources, GetManagedLiveObjs                   │
├─────────────────────────────────────────────────────────┤
│ Layer 2: App State Cache (Redis, TTL=1h)                │
│   - app|managed-resources|<appName>                     │
│   - app|resources-tree|<appName>                        │
│   - cluster|info|<server>                               │
├─────────────────────────────────────────────────────────┤
│ Layer 3: Manifest Cache (Redis, TTL=24h)                │
│   - CachedManifestResponse: 성공+실패 모두 캐싱            │
│   - CacheEntryHash: SHA-256 무결성 검증                   │
│   - Circuit Breaker: PauseGeneration                    │
└─────────────────────────────────────────────────────────┘
```

### Layer 1: Cluster Cache

```go
// 실제 소스: gitops-engine/pkg/cache/cluster.go:23
// "The parentUIDToChildren index enables efficient O(1) cross-namespace traversal
//  by mapping any resource's UID to its direct children,
//  eliminating the need for O(n) graph building."

type clusterCache struct {
    resources           map[kube.ResourceKey]*Resource
    parentUIDToChildren map[types.UID][]kube.ResourceKey  // O(1) 역방향 인덱스
}
```

**Watch 이벤트 처리 흐름:**

```
Watch 이벤트 수신 (ADDED/MODIFIED/DELETED)
    ↓
기존 ownerRef 인덱스 제거 (removeChild)
    ↓
리소스 저장소 업데이트
    ↓
새 ownerRef 인덱스 추가 (addChild)
```

**ClusterCache 인터페이스:**

```go
// 실제 소스: gitops-engine/pkg/cache/cluster.go:140
type ClusterCache interface {
    EnsureSynced() error
    FindResources(namespace string, predicates ...func(r *Resource) bool) map[ResourceKey]*Resource
    GetManagedLiveObjs(targetObjs []*unstructured.Unstructured, isManaged func(r *Resource) bool) (map[ResourceKey]*unstructured.Unstructured, error)
}
```

### Layer 2: App State Cache

```go
// 실제 소스: util/cache/appstate/cache.go
func appManagedResourcesKey(appName string) string {
    return "app|managed-resources|" + appName
}
func appResourcesTreeKey(appName string, shard int64) string {
    key := "app|resources-tree|" + appName
    if shard > 0 {
        key = fmt.Sprintf("%s|%d", key, shard)
    }
    return key
}
func clusterInfoKey(server string) string {
    return "cluster|info|" + server
}
```

**TTL 설정:**
- App State Cache: 기본 1시간 (`ARGOCD_APP_STATE_CACHE_EXPIRATION`)
- Cluster Info: 10분 (`clusterInfoCacheExpiration = 10 * time.Minute`)

**샤딩(Sharding):**
대규모 리소스 트리는 샤드로 분할하여 Redis 트래픽 최적화:
```go
// 실제 소스: util/cache/appstate/cache.go
for i, shard := range resourcesTree.GetShards(treeShardSize) {
    c.SetItem(appResourcesTreeKey(appName, int64(i)), shard, ...)
}
```

### Layer 3: Manifest Cache

```go
// 실제 소스: reposerver/cache/cache.go
type CachedManifestResponse struct {
    CacheEntryHash                  string                  // SHA-256 무결성 해시
    ManifestResponse                *ManifestResponse       // 성공 응답
    MostRecentError                 string                  // 실패 메시지 (에러 캐싱!)
    FirstFailureTimestamp           int64                   // 최초 실패 시각
    NumberOfConsecutiveFailures     int                     // 연속 실패 횟수
    NumberOfCachedResponsesReturned int                     // 캐시 에러 반환 횟수
}
```

**GetManifests 해시 검증:**
```go
// 실제 소스: reposerver/cache/cache.go
if hash != res.CacheEntryHash || res.ManifestResponse == nil && res.MostRecentError == "" {
    // 해시 불일치 → 캐시 삭제 → ErrCacheMiss
    c.DeleteManifests(...)
    return ErrCacheMiss
}
```

### PauseGeneration Circuit Breaker

```
연속 실패 횟수 >= PauseAfterFailedAttempts(N)
    ↓ 차단
캐시된 에러 반환 (NumberOfCachedResponsesReturned 증가)
    ↓ 해제 조건
  경과 시간 >= PauseForMinutes(M분) OR
  캐시 응답 횟수 >= PauseForRequests(R회)
    ↓ 재시도
generate() 함수 재실행
```

환경변수로 설정:
- `ARGOCD_PAUSE_GENERATION_AFTER_FAILED_ATTEMPTS` (기본: 0, 비활성)
- `ARGOCD_PAUSE_GENERATION_ON_FAILURE_FOR_MINUTES`
- `ARGOCD_PAUSE_GENERATION_ON_FAILURE_FOR_REQUESTS`

### double-check locking

```go
// 실제 소스: reposerver/repository/repository.go:494 주석
// "double-check locking"

// 1단계: 빠른 캐시 조회
cached, err := s.cache.GetManifests(cacheKey, ...)
if err == nil { return cached }

// 2단계: 락 획득 후 재확인 (다른 고루틴이 먼저 생성했을 수 있음)
// repositoryLock.Lock(path, revision, allowConcurrent, init)
```

### 캐시 무효화 전략

| 전략 | 동작 | 레이어 |
|------|------|--------|
| 새 커밋 SHA | 캐시 키에 SHA 포함 → 자동 무효화 | L3 |
| 수동 새로고침 | `SetManifests(nil)` 호출 | L3 |
| TTL 만료 | Redis TTL 자동 삭제 | L2, L3 |
| Watch 이벤트 | 실시간 인메모리 업데이트 | L1 |

## 실행 방법

```bash
go run main.go
```

## 실행 결과 요약

```
Layer 1 - Cluster Cache:
  parentUIDToChildren 인덱스로 O(1) 리소스 트리 탐색
  Deployment(d1) → ReplicaSet(r1) → Pod1, Pod2
  DELETED 이벤트로 Pod2 제거, 인덱스 자동 정리

Layer 2 - App State Cache:
  SetAppManagedResources, SetAppResourcesTree TTL 캐싱
  존재하지 않는 앱 → cache miss

Layer 3 - Manifest Cache:
  1회: 캐시 MISS → 생성 → 저장
  2회: 캐시 HIT (generate 함수 미호출)
  3~5회: 실패 캐싱 (failures=1,2,3)
  6회~: Circuit Breaker 활성 (generate 미호출)
  실제 generate() 호출 횟수: 3 (차단 이후 불필요한 재시도 방지)
```

## 핵심 설계 선택의 이유 (Why)

**왜 Cluster Cache가 parentUIDToChildren 역방향 인덱스를 사용하는가?**
쿠버네티스의 ownerReferences는 자식 → 부모 방향이다. Argo CD는 부모 → 자식 방향 탐색이 필요하므로(리소스 트리 표시), 역방향 인덱스가 필수다. O(n) 선형 탐색 대신 O(1) 인덱스 조회로 대규모 클러스터에서도 성능을 보장한다.

**왜 에러도 캐싱하는가(error caching)?**
매니페스트 생성 실패(예: Helm 템플릿 오류)가 발생할 때마다 Git fetch + 생성을 반복하면 repo-server에 과부하가 걸린다. 에러를 캐싱하고 Circuit Breaker로 차단함으로써 반복 실패를 방지하고 시스템 안정성을 높인다.

**왜 CacheEntryHash가 필요한가?**
Redis는 부분적 데이터 손상이나 직렬화 오류가 발생할 수 있다. 저장 시점에 생성한 해시로 조회 시 무결성을 검증하여, 손상된 캐시 항목을 cache miss로 처리해 정확성을 보장한다.
