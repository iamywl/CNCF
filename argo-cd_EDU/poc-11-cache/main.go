// poc-11-cache/main.go
//
// Argo CD 캐싱 레이어 시뮬레이션
//
// 참조 소스:
//   - gitops-engine/pkg/cache/cluster.go : ClusterCache 인터페이스, parentUIDToChildren 인덱스
//   - util/cache/appstate/cache.go        : App State Cache (appManagedResourcesKey, appResourcesTreeKey)
//   - reposerver/cache/cache.go           : Manifest Cache (CachedManifestResponse, GetManifests, SetManifests)
//   - reposerver/repository/repository.go : double-check locking, PauseGeneration circuit breaker
//
// 핵심 개념:
//   Layer 1 - Cluster Cache: 인메모리, Watch 이벤트 기반
//     - parentUIDToChildren 인덱스: O(1) 리소스 트리 탐색
//     - Watch 이벤트: ADDED, MODIFIED, DELETED
//   Layer 2 - App State Cache: Redis 기반 (시뮬레이션: 인메모리)
//     - SetManifests/GetManifests with TTL
//     - managed resources, resource trees, revision metadata
//   Layer 3 - Manifest Cache with error caching
//     - CachedManifestResponse: 성공/실패 모두 캐싱
//     - double-check locking: 캐시 미스 시 재확인
//     - PauseGeneration circuit breaker: 연속 실패 시 요청 차단

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ============================================================
// Layer 1: Cluster Cache (인메모리, Watch 기반)
// 참조: gitops-engine/pkg/cache/cluster.go
// ============================================================

// ResourceKey — 쿠버네티스 리소스 고유 식별자
// 실제 코드: kube.ResourceKey struct { Group, Kind, Namespace, Name }
type ResourceKey struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

func (k ResourceKey) String() string {
	if k.Namespace != "" {
		return fmt.Sprintf("%s/%s/%s/%s", k.Group, k.Kind, k.Namespace, k.Name)
	}
	return fmt.Sprintf("%s/%s//%s", k.Group, k.Kind, k.Name)
}

// UID — 쿠버네티스 오브젝트 고유 ID
type UID string

// Resource — 클러스터 캐시의 리소스 엔트리
// 실제 코드: cache.Resource struct
type Resource struct {
	Key       ResourceKey
	UID       UID
	OwnerRefs []UID     // ownerReferences[].uid
	Info      any       // OnPopulateResourceInfoHandler 반환값 (앱 관련 메타데이터)
}

// WatchEventType — 쿠버네티스 Watch 이벤트 타입
type WatchEventType string

const (
	EventAdded    WatchEventType = "ADDED"
	EventModified WatchEventType = "MODIFIED"
	EventDeleted  WatchEventType = "DELETED"
)

// ClusterCache 인터페이스
// 참조: gitops-engine/pkg/cache/cluster.go type ClusterCache interface
//
//	EnsureSynced() error
//	FindResources(namespace string, predicates ...func(r *Resource) bool) map[ResourceKey]*Resource
//	GetManagedLiveObjs(targetObjs, isManaged) (map[ResourceKey]*Resource, error)
type ClusterCache interface {
	EnsureSynced() error
	FindResources(namespace string, predicates ...func(r *Resource) bool) map[ResourceKey]*Resource
	GetManagedLiveObjs(targetKeys []ResourceKey, isManaged func(r *Resource) bool) map[ResourceKey]*Resource
	HandleWatchEvent(event WatchEventType, res *Resource)
	GetChildren(uid UID) []ResourceKey
}

// inMemoryClusterCache — clusterCache 시뮬레이션
// 실제 코드: gitops-engine/pkg/cache/cluster.go type clusterCache struct
type inMemoryClusterCache struct {
	mu sync.RWMutex

	// 기본 리소스 저장소: ResourceKey → Resource
	resources map[ResourceKey]*Resource

	// parentUIDToChildren 인덱스 — 실제 소스 주석:
	// "The parentUIDToChildren index enables efficient O(1) cross-namespace traversal
	//  by mapping any resource's UID to its direct children,
	//  eliminating the need for O(n) graph building."
	// 참조: gitops-engine/pkg/cache/cluster.go:23
	parentUIDToChildren map[UID][]ResourceKey

	synced bool
}

func NewClusterCache() ClusterCache {
	return &inMemoryClusterCache{
		resources:           make(map[ResourceKey]*Resource),
		parentUIDToChildren: make(map[UID][]ResourceKey),
	}
}

// EnsureSynced — 캐시 동기화 (실제: K8s API 목록 조회 + Watch 시작)
func (c *inMemoryClusterCache) EnsureSynced() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.synced {
		fmt.Println("  [ClusterCache] 초기 동기화 시작 (List + Watch)")
		c.synced = true
	}
	return nil
}

// addChild — parentUIDToChildren 인덱스에 자식 추가
// 실제 코드: gitops-engine/pkg/cache/cluster.go:534
func (c *inMemoryClusterCache) addChild(parentUID UID, childKey ResourceKey) {
	children := c.parentUIDToChildren[parentUID]
	for _, existing := range children {
		if existing == childKey {
			return
		}
	}
	c.parentUIDToChildren[parentUID] = append(children, childKey)
}

// removeChild — parentUIDToChildren 인덱스에서 자식 제거
// 실제 코드: gitops-engine/pkg/cache/cluster.go:539
func (c *inMemoryClusterCache) removeChild(parentUID UID, childKey ResourceKey) {
	children := c.parentUIDToChildren[parentUID]
	for i, child := range children {
		if child == childKey {
			// 마지막 요소로 교체 후 슬라이스 축소
			children[i] = children[len(children)-1]
			children = children[:len(children)-1]
			if len(children) == 0 {
				delete(c.parentUIDToChildren, parentUID)
			} else {
				c.parentUIDToChildren[parentUID] = children
			}
			return
		}
	}
}

// HandleWatchEvent — Watch 이벤트 처리 (ADDED/MODIFIED/DELETED)
// 실제 코드: clusterCache.onEvent() / processEvents()
func (c *inMemoryClusterCache) HandleWatchEvent(event WatchEventType, res *Resource) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch event {
	case EventAdded, EventModified:
		// 기존 리소스가 있으면 ownerRef 인덱스 업데이트
		if old, exists := c.resources[res.Key]; exists {
			for _, ownerUID := range old.OwnerRefs {
				c.removeChild(ownerUID, res.Key)
			}
		}
		c.resources[res.Key] = res
		// 새 ownerRef로 인덱스 재구성
		for _, ownerUID := range res.OwnerRefs {
			c.addChild(ownerUID, res.Key)
		}
		fmt.Printf("  [ClusterCache] %s: %s\n", event, res.Key)

	case EventDeleted:
		if old, exists := c.resources[res.Key]; exists {
			// ownerRef 인덱스에서 제거
			for _, ownerUID := range old.OwnerRefs {
				c.removeChild(ownerUID, res.Key)
			}
			// 이 리소스가 부모인 경우 자식 인덱스도 제거
			delete(c.parentUIDToChildren, old.UID)
			delete(c.resources, res.Key)
			fmt.Printf("  [ClusterCache] %s: %s\n", event, res.Key)
		}
	}
}

// FindResources — 네임스페이스 + Predicate로 리소스 검색
// 실제 코드: gitops-engine/pkg/cache/cluster.go FindResources()
func (c *inMemoryClusterCache) FindResources(namespace string, predicates ...func(r *Resource) bool) map[ResourceKey]*Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[ResourceKey]*Resource)
	for key, res := range c.resources {
		if namespace != "" && key.Namespace != namespace {
			continue
		}
		match := true
		for _, pred := range predicates {
			if !pred(res) {
				match = false
				break
			}
		}
		if match {
			result[key] = res
		}
	}
	return result
}

// GetManagedLiveObjs — Argo CD가 관리하는 실제 K8s 오브젝트 반환
// 실제 코드: 대상 오브젝트 목록과 isManaged 함수로 필터링
func (c *inMemoryClusterCache) GetManagedLiveObjs(targetKeys []ResourceKey, isManaged func(r *Resource) bool) map[ResourceKey]*Resource {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[ResourceKey]*Resource)
	for _, key := range targetKeys {
		if res, exists := c.resources[key]; exists && isManaged(res) {
			result[key] = res
		}
	}
	return result
}

// GetChildren — parentUIDToChildren 인덱스를 이용한 O(1) 자식 탐색
func (c *inMemoryClusterCache) GetChildren(uid UID) []ResourceKey {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.parentUIDToChildren[uid]
}

// ============================================================
// Layer 2: App State Cache (Redis 기반, 시뮬레이션: 인메모리)
// 참조: util/cache/appstate/cache.go
//
// 키 형식:
//   app|managed-resources|<appName>
//   app|resources-tree|<appName>
// ============================================================

// CacheEntry — TTL 있는 캐시 항목
type CacheEntry struct {
	Value     any
	ExpiresAt time.Time
}

func (e *CacheEntry) IsExpired() bool {
	return time.Now().After(e.ExpiresAt)
}

// AppStateCache — util/cache/appstate/cache.go type Cache struct 시뮬레이션
type AppStateCache struct {
	mu                  sync.RWMutex
	store               map[string]*CacheEntry
	appStateCacheExpiry time.Duration // 기본값: 1시간
}

func NewAppStateCache(expiry time.Duration) *AppStateCache {
	return &AppStateCache{
		store:               make(map[string]*CacheEntry),
		appStateCacheExpiry: expiry,
	}
}

// 키 생성 함수 — 실제 소스 그대로 반영
// 참조: util/cache/appstate/cache.go
func appManagedResourcesKey(appName string) string {
	return "app|managed-resources|" + appName
}

func appResourcesTreeKey(appName string) string {
	return "app|resources-tree|" + appName
}

func clusterInfoKey(server string) string {
	return "cluster|info|" + server
}

func (c *AppStateCache) set(key string, value any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = &CacheEntry{Value: value, ExpiresAt: time.Now().Add(ttl)}
}

func (c *AppStateCache) get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.store[key]
	if !ok || entry.IsExpired() {
		return nil, false
	}
	return entry.Value, true
}

func (c *AppStateCache) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.store, key)
}

// ManagedResource — 관리 리소스 정보
type ManagedResource struct {
	Name      string
	Namespace string
	Kind      string
	Group     string
	Status    string // Synced, OutOfSync, Unknown
}

// ResourceTree — 리소스 트리 (ApplicationTree)
type ResourceTree struct {
	Nodes []ResourceTreeNode
}

type ResourceTreeNode struct {
	Key        ResourceKey
	ParentUID  UID
	Health     string
	Images     []string
}

// SetAppManagedResources — 관리 리소스 캐싱
// 실제 코드: sort.Slice(managedResources, ...) 후 SetItem 호출
func (c *AppStateCache) SetAppManagedResources(appName string, resources []ManagedResource) error {
	if resources == nil {
		c.delete(appManagedResourcesKey(appName))
		return nil
	}
	fmt.Printf("  [AppStateCache] SetAppManagedResources: app=%q resources=%d개\n", appName, len(resources))
	c.set(appManagedResourcesKey(appName), resources, c.appStateCacheExpiry)
	return nil
}

// GetAppManagedResources — 관리 리소스 캐시 조회
func (c *AppStateCache) GetAppManagedResources(appName string) ([]ManagedResource, error) {
	val, ok := c.get(appManagedResourcesKey(appName))
	if !ok {
		return nil, errors.New("cache miss")
	}
	return val.([]ManagedResource), nil
}

// SetAppResourcesTree — 리소스 트리 캐싱
func (c *AppStateCache) SetAppResourcesTree(appName string, tree *ResourceTree) error {
	if tree == nil {
		c.delete(appResourcesTreeKey(appName))
		return nil
	}
	fmt.Printf("  [AppStateCache] SetAppResourcesTree: app=%q nodes=%d개\n", appName, len(tree.Nodes))
	c.set(appResourcesTreeKey(appName), tree, c.appStateCacheExpiry)
	return nil
}

// GetAppResourcesTree — 리소스 트리 캐시 조회
func (c *AppStateCache) GetAppResourcesTree(appName string) (*ResourceTree, error) {
	val, ok := c.get(appResourcesTreeKey(appName))
	if !ok {
		return nil, errors.New("cache miss")
	}
	return val.(*ResourceTree), nil
}

// ============================================================
// Layer 3: Manifest Cache with error caching
// 참조: reposerver/cache/cache.go
//
// CachedManifestResponse:
//   - ManifestResponse: 성공 시 실제 manifest
//   - MostRecentError: 실패 시 에러 메시지 (에러도 캐싱!)
//   - NumberOfConsecutiveFailures: 연속 실패 횟수
//   - NumberOfCachedResponsesReturned: 캐시된 에러 응답 반환 횟수
//   - CacheEntryHash: 무결성 검증용 해시
//
// PauseGeneration circuit breaker:
//   - PauseGenerationAfterFailedGenerationAttempts: N회 실패 후 차단
//   - PauseGenerationOnFailureForMinutes: M분간 차단
//   - PauseGenerationOnFailureForRequests: R회 요청 후 차단 해제
// ============================================================

// ManifestResponse — 매니페스트 생성 응답
type ManifestResponse struct {
	Manifests []string
	Revision  string
	Namespace string
}

// CachedManifestResponse — 매니페스트 캐시 항목
// 참조: reposerver/cache/cache.go type CachedManifestResponse struct
type CachedManifestResponse struct {
	CacheEntryHash                  string
	ManifestResponse                *ManifestResponse
	MostRecentError                 string
	FirstFailureTimestamp           int64
	NumberOfConsecutiveFailures     int
	NumberOfCachedResponsesReturned int
}

// generateCacheEntryHash — 캐시 항목 무결성 해시 생성
// 실제 코드: reposerver/cache/cache.go generateCacheEntryHash()
func (r *CachedManifestResponse) generateCacheEntryHash() (string, error) {
	data, err := json.Marshal(struct {
		MR  *ManifestResponse
		Err string
	}{r.ManifestResponse, r.MostRecentError})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8]), nil
}

// CircuitBreakerConfig — PauseGeneration 설정
type CircuitBreakerConfig struct {
	// N회 이상 연속 실패 시 circuit 열림
	PauseAfterFailedAttempts int // PauseGenerationAfterFailedGenerationAttempts
	// 차단 유지 시간 (분)
	PauseForMinutes int // PauseGenerationOnFailureForMinutes
	// 이 횟수만큼 캐시 응답 반환 후 재시도 허용
	PauseForRequests int // PauseGenerationOnFailureForRequests
}

// ManifestCache — Manifest Cache 시뮬레이션
type ManifestCache struct {
	mu     sync.Mutex
	store  map[string]*CachedManifestResponse
	expiry time.Duration // repoCacheExpiration (기본 24h)
	config CircuitBreakerConfig
}

func NewManifestCache(expiry time.Duration, cbConfig CircuitBreakerConfig) *ManifestCache {
	return &ManifestCache{
		store:  make(map[string]*CachedManifestResponse),
		expiry: expiry,
		config: cbConfig,
	}
}

// manifestCacheKey — 캐시 키 생성
// 실제 코드: reposerver/cache/cache.go manifestCacheKey()
// FNV-32a 해시를 사용하여 revision + appSource + clusterInfo 등을 조합
func manifestCacheKey(revision, appName, namespace string) string {
	h := sha256.Sum256([]byte(revision + "|" + appName + "|" + namespace))
	return fmt.Sprintf("mfst|%x", h[:8])
}

// GetManifests — 캐시 조회
// 실제 코드: reposerver/cache/cache.go func (c *Cache) GetManifests(...)
// - 해시 검증 후 캐시 항목 반환
// - 해시 불일치 → 캐시 삭제 후 ErrCacheMiss
func (c *ManifestCache) GetManifests(revision, appName, namespace string) (*CachedManifestResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := manifestCacheKey(revision, appName, namespace)
	cached, ok := c.store[key]
	if !ok {
		return nil, errors.New("cache miss")
	}

	// 무결성 검증 (실제 코드와 동일)
	expectedHash := cached.CacheEntryHash
	hash, err := cached.generateCacheEntryHash()
	if err != nil {
		return nil, fmt.Errorf("해시 생성 실패: %w", err)
	}
	if hash != expectedHash || (cached.ManifestResponse == nil && cached.MostRecentError == "") {
		// 해시 불일치 → 캐시 삭제 (cache miss로 처리)
		fmt.Printf("  [ManifestCache] 해시 불일치, 캐시 삭제: key=%s\n", key)
		delete(c.store, key)
		return nil, errors.New("cache miss (hash mismatch)")
	}

	// 반환 시 해시 제거
	result := *cached
	result.CacheEntryHash = ""
	return &result, nil
}

// SetManifests — 캐시 저장
// 실제 코드: res.shallowCopy() → generateCacheEntryHash() → cache.SetItem()
func (c *ManifestCache) SetManifests(revision, appName, namespace string, res *CachedManifestResponse) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if res == nil {
		key := manifestCacheKey(revision, appName, namespace)
		delete(c.store, key)
		return nil
	}

	// 저장 전 해시 생성 (실제 코드와 동일)
	hash, err := res.generateCacheEntryHash()
	if err != nil {
		return fmt.Errorf("해시 생성 실패: %w", err)
	}
	res.CacheEntryHash = hash

	key := manifestCacheKey(revision, appName, namespace)
	c.store[key] = res
	return nil
}

// GetManifestsWithCircuitBreaker — double-check locking + PauseGeneration circuit breaker
// 참조: reposerver/repository/repository.go:494 "double-check locking" 주석
// 참조: reposerver/repository/repository.go:898-1016 PauseGeneration 로직
func (c *ManifestCache) GetManifestsWithCircuitBreaker(
	revision, appName, namespace string,
	generate func() (*ManifestResponse, error),
) (*ManifestResponse, error) {

	// 1단계: 캐시 조회 (첫 번째 확인)
	cached, err := c.GetManifests(revision, appName, namespace)
	if err == nil {
		// 에러가 캐싱된 경우 circuit breaker 검사
		if cached.MostRecentError != "" {
			fmt.Printf("  [ManifestCache] 캐시된 에러 발견: failures=%d cached-responses=%d\n",
				cached.NumberOfConsecutiveFailures, cached.NumberOfCachedResponsesReturned)

			// PauseGeneration circuit breaker 검사
			if c.config.PauseAfterFailedAttempts > 0 &&
				cached.NumberOfConsecutiveFailures >= c.config.PauseAfterFailedAttempts {

				// 시간 기반 차단 해제 확인
				if c.config.PauseForMinutes > 0 {
					elapsed := time.Since(time.Unix(cached.FirstFailureTimestamp, 0))
					if elapsed >= time.Duration(c.config.PauseForMinutes)*time.Minute {
						fmt.Println("  [ManifestCache] 차단 시간 만료, 재시도 허용")
						goto generate
					}
				}

				// 요청 횟수 기반 차단 해제 확인
				if c.config.PauseForRequests > 0 &&
					cached.NumberOfCachedResponsesReturned >= c.config.PauseForRequests {
					fmt.Println("  [ManifestCache] 최대 캐시 응답 횟수 초과, 재시도 허용")
					goto generate
				}

				// Circuit breaker 활성화: 캐시된 에러 반환
				cached.NumberOfCachedResponsesReturned++
				c.SetManifests(revision, appName, namespace, cached)
				fmt.Printf("  [ManifestCache] Circuit breaker 활성: 에러 반환 (cached-responses=%d)\n",
					cached.NumberOfCachedResponsesReturned)
				return nil, fmt.Errorf("circuit breaker: %s", cached.MostRecentError)
			}
		} else {
			fmt.Printf("  [ManifestCache] 캐시 HIT: app=%q revision=%s\n", appName, revision[:8])
			return cached.ManifestResponse, nil
		}
	}

generate:
	// 2단계: 매니페스트 생성
	fmt.Printf("  [ManifestCache] 캐시 MISS: app=%q revision=%s → 생성 시작\n", appName, revision[:8])
	resp, genErr := generate()

	if genErr != nil {
		// 실패 캐싱 (circuit breaker용)
		failEntry := &CachedManifestResponse{
			MostRecentError:             genErr.Error(),
			FirstFailureTimestamp:       time.Now().Unix(),
			NumberOfConsecutiveFailures: 1,
		}
		if cached != nil && cached.MostRecentError != "" {
			failEntry.FirstFailureTimestamp = cached.FirstFailureTimestamp
			failEntry.NumberOfConsecutiveFailures = cached.NumberOfConsecutiveFailures + 1
		}
		c.SetManifests(revision, appName, namespace, failEntry)
		fmt.Printf("  [ManifestCache] 생성 실패 캐싱: failures=%d error=%q\n",
			failEntry.NumberOfConsecutiveFailures, genErr.Error())
		return nil, genErr
	}

	// 성공 캐싱: NumberOfConsecutiveFailures 초기화
	successEntry := &CachedManifestResponse{
		ManifestResponse:            resp,
		NumberOfConsecutiveFailures: 0,
	}
	c.SetManifests(revision, appName, namespace, successEntry)
	fmt.Printf("  [ManifestCache] 생성 성공 캐싱: manifests=%d개\n", len(resp.Manifests))
	return resp, nil
}

// ============================================================
// 캐시 무효화 전략
// ============================================================

// InvalidationReason — 캐시 무효화 이유
type InvalidationReason string

const (
	InvalidationReasonRevision   InvalidationReason = "새 커밋 감지"
	InvalidationReasonConfig     InvalidationReason = "설정 변경"
	InvalidationReasonManual     InvalidationReason = "수동 새로고침"
	InvalidationReasonExpiry     InvalidationReason = "TTL 만료"
)

// ============================================================
// Main: 시나리오 시연
// ============================================================

func main() {
	fmt.Println("=======================================================")
	fmt.Println("Argo CD 캐싱 레이어 시뮬레이션")
	fmt.Println("=======================================================")

	// ─── Layer 1: Cluster Cache ──────────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Layer 1: Cluster Cache — parentUIDToChildren 인덱스")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	clusterCache := NewClusterCache()
	clusterCache.EnsureSynced()

	// 리소스 트리 구성:
	// Deployment(uid=d1) → ReplicaSet(uid=r1) → Pod(uid=p1), Pod(uid=p2)
	deployKey := ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "my-app"}
	rsKey := ResourceKey{Group: "apps", Kind: "ReplicaSet", Namespace: "default", Name: "my-app-6f4b"}
	pod1Key := ResourceKey{Kind: "Pod", Namespace: "default", Name: "my-app-6f4b-xvk9d"}
	pod2Key := ResourceKey{Kind: "Pod", Namespace: "default", Name: "my-app-6f4b-mn3kp"}
	svcKey := ResourceKey{Kind: "Service", Namespace: "default", Name: "my-app-svc"}

	fmt.Println("\n  ADDED 이벤트 처리:")
	clusterCache.HandleWatchEvent(EventAdded, &Resource{
		Key: deployKey, UID: "d1", OwnerRefs: nil,
	})
	clusterCache.HandleWatchEvent(EventAdded, &Resource{
		Key: rsKey, UID: "r1", OwnerRefs: []UID{"d1"}, // Deployment가 부모
	})
	clusterCache.HandleWatchEvent(EventAdded, &Resource{
		Key: pod1Key, UID: "p1", OwnerRefs: []UID{"r1"}, // ReplicaSet이 부모
	})
	clusterCache.HandleWatchEvent(EventAdded, &Resource{
		Key: pod2Key, UID: "p2", OwnerRefs: []UID{"r1"},
	})
	clusterCache.HandleWatchEvent(EventAdded, &Resource{
		Key: svcKey, UID: "s1", OwnerRefs: nil,
	})

	// O(1) 트리 탐색 — parentUIDToChildren 활용
	fmt.Println("\n  O(1) 리소스 트리 탐색 (parentUIDToChildren):")
	fmt.Printf("  Deployment(d1) 자식: %v\n", clusterCache.GetChildren("d1"))
	fmt.Printf("  ReplicaSet(r1) 자식: %v\n", clusterCache.GetChildren("r1"))

	// FindResources: Kind=Pod 필터
	pods := clusterCache.FindResources("default", func(r *Resource) bool {
		return r.Key.Kind == "Pod"
	})
	fmt.Printf("\n  FindResources(kind=Pod): %d개\n", len(pods))
	for k := range pods {
		fmt.Printf("    - %s\n", k.Name)
	}

	// GetManagedLiveObjs: 관리 리소스 조회
	managed := clusterCache.GetManagedLiveObjs(
		[]ResourceKey{deployKey, rsKey, pod1Key, svcKey},
		func(r *Resource) bool { return r.Key.Kind != "Service" }, // Service 제외
	)
	fmt.Printf("\n  GetManagedLiveObjs (Service 제외): %d개\n", len(managed))

	// DELETED 이벤트
	fmt.Println("\n  DELETED 이벤트 처리:")
	clusterCache.HandleWatchEvent(EventDeleted, &Resource{Key: pod2Key, UID: "p2", OwnerRefs: []UID{"r1"}})
	fmt.Printf("  ReplicaSet(r1) 자식 (pod2 삭제 후): %v\n", clusterCache.GetChildren("r1"))

	// ─── Layer 2: App State Cache ────────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Layer 2: App State Cache — Redis 기반 (TTL=1h)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	appCache := NewAppStateCache(1 * time.Hour)

	// 관리 리소스 캐싱
	resources := []ManagedResource{
		{Name: "my-app", Namespace: "default", Kind: "Deployment", Group: "apps", Status: "Synced"},
		{Name: "my-app-svc", Namespace: "default", Kind: "Service", Status: "Synced"},
	}
	appCache.SetAppManagedResources("my-app", resources)

	// 리소스 트리 캐싱
	tree := &ResourceTree{
		Nodes: []ResourceTreeNode{
			{Key: deployKey, Health: "Healthy", Images: []string{"nginx:1.19"}},
			{Key: rsKey, ParentUID: "d1", Health: "Healthy"},
			{Key: pod1Key, ParentUID: "r1", Health: "Healthy"},
		},
	}
	appCache.SetAppResourcesTree("my-app", tree)

	// 캐시 조회
	cachedResources, err := appCache.GetAppManagedResources("my-app")
	fmt.Printf("\n  GetAppManagedResources: %d개, error=%v\n", len(cachedResources), err)

	cachedTree, err := appCache.GetAppResourcesTree("my-app")
	fmt.Printf("  GetAppResourcesTree: nodes=%d개, error=%v\n", len(cachedTree.Nodes), err)

	// 존재하지 않는 앱 조회 → cache miss
	_, err = appCache.GetAppManagedResources("nonexistent-app")
	fmt.Printf("  GetAppManagedResources(nonexistent): error=%v\n", err)

	// ─── Layer 3: Manifest Cache + Circuit Breaker ───────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Layer 3: Manifest Cache — double-check locking + circuit breaker")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	manifestCache := NewManifestCache(
		24*time.Hour,
		CircuitBreakerConfig{
			PauseAfterFailedAttempts: 3,   // 3회 실패 후 차단
			PauseForMinutes:          10,  // 10분간 차단
			PauseForRequests:         5,   // 5회 캐시 응답 후 재시도
		},
	)

	revision := "abc123def456789012345678901234567890"
	appName := "guestbook"
	namespace := "default"

	// 1. 첫 번째 요청: 캐시 MISS → 생성 성공
	fmt.Println("\n[요청 1] 첫 번째 요청 (캐시 MISS → 생성)")
	resp, err := manifestCache.GetManifestsWithCircuitBreaker(
		revision, appName, namespace,
		func() (*ManifestResponse, error) {
			return &ManifestResponse{
				Manifests: []string{"apiVersion: apps/v1\nkind: Deployment\n...", "apiVersion: v1\nkind: Service\n..."},
				Revision:  revision,
			}, nil
		},
	)
	fmt.Printf("  결과: manifests=%d개, error=%v\n", len(resp.Manifests), err)

	// 2. 두 번째 요청: 캐시 HIT
	fmt.Println("\n[요청 2] 두 번째 요청 (캐시 HIT)")
	resp, err = manifestCache.GetManifestsWithCircuitBreaker(
		revision, appName, namespace,
		func() (*ManifestResponse, error) {
			return nil, fmt.Errorf("이 함수는 호출되면 안 됨")
		},
	)
	fmt.Printf("  결과: manifests=%d개, error=%v\n", len(resp.Manifests), err)

	// 3. 새 리비전 실패 시나리오 — circuit breaker 동작
	fmt.Println("\n[요청 3~6] 실패 시나리오 — circuit breaker 동작")
	badRevision := "def456abc123789012345678901234567890"
	callCount := 0
	for i := 1; i <= 6; i++ {
		fmt.Printf("\n  [시도 %d]\n", i)
		_, err = manifestCache.GetManifestsWithCircuitBreaker(
			badRevision, "broken-app", namespace,
			func() (*ManifestResponse, error) {
				callCount++
				return nil, fmt.Errorf("helm template 실패: values.yaml 파싱 오류")
			},
		)
		fmt.Printf("  error=%v\n", err)
	}
	fmt.Printf("\n  실제 generate() 호출 횟수: %d (circuit breaker가 3회 이후 차단)\n", callCount)

	// ─── 캐시 무효화 시나리오 ────────────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("캐시 무효화 전략")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	invalidationCases := []struct {
		reason    InvalidationReason
		action    string
		newRevision string
	}{
		{InvalidationReasonRevision, "새 SHA로 키가 변경됨 (자동 무효화)", "newsha123456"},
		{InvalidationReasonManual,   "SetManifests(nil)로 캐시 삭제", ""},
		{InvalidationReasonExpiry,   "TTL 만료 시 자동 삭제", ""},
	}

	for _, c := range invalidationCases {
		fmt.Printf("\n  무효화 이유: %s\n  처리: %s\n", c.reason, c.action)
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("\n핵심 개념 요약:")
	fmt.Println("  Layer 1 - Cluster Cache:")
	fmt.Println("    parentUIDToChildren: O(1) 부모→자식 탐색 (ownerRef 역 인덱스)")
	fmt.Println("    Watch 이벤트: ADDED/MODIFIED/DELETED 실시간 반영")
	fmt.Println("  Layer 2 - App State Cache:")
	fmt.Println("    키: 'app|managed-resources|<name>', 'app|resources-tree|<name>'")
	fmt.Println("    TTL: 1시간 (ARGOCD_APP_STATE_CACHE_EXPIRATION)")
	fmt.Println("  Layer 3 - Manifest Cache:")
	fmt.Println("    CacheEntryHash: SHA-256으로 캐시 무결성 보장")
	fmt.Println("    에러 캐싱: 실패도 캐싱하여 중복 실패 방지")
	fmt.Println("    Circuit Breaker: N회 실패 → M분 차단 / R회 응답 후 재시도")
}
