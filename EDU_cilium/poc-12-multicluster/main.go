// SPDX-License-Identifier: Apache-2.0
// Cilium ClusterMesh PoC - 멀티클러스터 메커니즘 시뮬레이션
//
// 이 PoC는 Cilium ClusterMesh의 핵심 메커니즘을 순수 Go(stdlib만 사용)로 시뮬레이션한다.
// 실제 Cilium 소스코드의 구조를 반영하며, 다음을 데모한다:
//
//  1. 다중 클러스터 (3개)의 독립적인 Identity/Service/Endpoint 저장소
//  2. ClusterMesh 동기화: 클러스터 A의 서비스 정보가 클러스터 B에 동기화
//  3. 글로벌 서비스 디스커버리 (Global Service Cache)
//  4. 크로스 클러스터 Identity 매핑
//  5. 크로스 클러스터 네트워크 정책 평가
//  6. ClusterID를 통한 Identity 충돌 방지
//
// 참조:
//   - pkg/clustermesh/clustermesh.go
//   - pkg/clustermesh/remote_cluster.go
//   - pkg/clustermesh/common/services.go
//   - pkg/clustermesh/store/store.go
//   - pkg/clustermesh/idsmgr.go
//   - pkg/clustermesh/types/types.go

package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// =============================================================================
// 1. 기본 타입 정의
//    참조: pkg/clustermesh/types/types.go, pkg/clustermesh/types/option.go
// =============================================================================

const (
	ClusterIDMin = 0
	ClusterIDMax = 255

	// Identity 구조: 상위 8비트 = ClusterID, 하위 16비트 = LocalIdentity
	// 참조: ClusterID 기반 Identity 격리 메커니즘
	IdentityClusterShift = 16
	IdentityLocalMask    = 0xFFFF
)

// ClusterInfo는 클러스터의 고유 식별 정보.
// 참조: pkg/clustermesh/types/option.go - ClusterInfo struct
type ClusterInfo struct {
	ID   uint32
	Name string
}

// PortConfig는 L4 포트 설정.
// 참조: pkg/clustermesh/store/store.go - PortConfiguration
type PortConfig struct {
	Port     uint16
	Protocol string
}

// ClusterService는 kvstore에 저장되는 서비스 정보.
// 참조: pkg/clustermesh/store/store.go - ClusterService struct
type ClusterService struct {
	Cluster   string
	Namespace string
	Name      string
	ClusterID uint32
	Backends  map[string]PortConfig // backendIP -> port config
	Shared    bool                  // 다른 클러스터에 공유 여부
	Labels    map[string]string
}

func (s *ClusterService) NamespacedName() string {
	return s.Namespace + "/" + s.Name
}

func (s *ClusterService) GetKeyName() string {
	return fmt.Sprintf("%s/%s/%s", s.Cluster, s.Namespace, s.Name)
}

// SecurityIdentity는 보안 식별자.
// 참조: pkg/identity/ - Identity 개념
type SecurityIdentity struct {
	ID        uint32
	ClusterID uint32
	Labels    map[string]string
}

// GlobalIdentityID는 ClusterID와 LocalID를 결합한 전역 고유 Identity를 생성한다.
// 이것이 ClusterID가 Identity 충돌을 방지하는 핵심 메커니즘이다.
func GlobalIdentityID(clusterID uint32, localID uint32) uint32 {
	return (clusterID << IdentityClusterShift) | (localID & IdentityLocalMask)
}

// ExtractClusterID는 글로벌 Identity에서 ClusterID를 추출한다.
func ExtractClusterID(globalID uint32) uint32 {
	return globalID >> IdentityClusterShift
}

// ExtractLocalID는 글로벌 Identity에서 로컬 Identity를 추출한다.
func ExtractLocalID(globalID uint32) uint32 {
	return globalID & IdentityLocalMask
}

// Endpoint는 서비스 엔드포인트.
type Endpoint struct {
	IP        string
	ServiceNS string
	ServiceN  string
	Identity  uint32
	ClusterID uint32
}

// NetworkPolicy는 크로스 클러스터 네트워크 정책.
// 참조: CiliumNetworkPolicy, CiliumClusterwideNetworkPolicy
type NetworkPolicy struct {
	Name                string
	TargetLabels        map[string]string
	AllowedSourceLabels map[string]string
	AllowedCluster      string // "" = 모든 클러스터, "cluster-a" = 특정 클러스터만
	DenyAll             bool
}

// =============================================================================
// 2. ClusterMeshUsedIDs - Cluster ID 관리자
//    참조: pkg/clustermesh/idsmgr.go
// =============================================================================

type ClusterMeshUsedIDs struct {
	mu             sync.RWMutex
	localClusterID uint32
	usedIDs        map[uint32]struct{}
}

func NewClusterMeshUsedIDs(localID uint32) *ClusterMeshUsedIDs {
	return &ClusterMeshUsedIDs{
		localClusterID: localID,
		usedIDs:        make(map[uint32]struct{}),
	}
}

func (m *ClusterMeshUsedIDs) ReserveClusterID(id uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if id == ClusterIDMin {
		return fmt.Errorf("clusterID %d is reserved", id)
	}
	if id == m.localClusterID {
		return fmt.Errorf("clusterID %d is assigned to the local cluster", id)
	}
	if id > ClusterIDMax {
		return fmt.Errorf("clusterID %d exceeds maximum %d", id, ClusterIDMax)
	}
	if _, ok := m.usedIDs[id]; ok {
		return fmt.Errorf("clusterID %d is already used", id)
	}
	m.usedIDs[id] = struct{}{}
	return nil
}

func (m *ClusterMeshUsedIDs) ReleaseClusterID(id uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.usedIDs, id)
}

// =============================================================================
// 3. KVStore - etcd 시뮬레이션
//    참조: pkg/kvstore/etcd.go, pkg/kvstore/store/store.go
// =============================================================================

type KVStoreEvent struct {
	Type  string // "PUT" or "DELETE"
	Key   string
	Value interface{}
}

type WatchCallback func(event KVStoreEvent)

// KVStore는 etcd를 시뮬레이션하는 인메모리 키-값 저장소.
type KVStore struct {
	mu       sync.RWMutex
	data     map[string]interface{}
	watchers map[string][]WatchCallback
	name     string
}

func NewKVStore(name string) *KVStore {
	return &KVStore{
		data:     make(map[string]interface{}),
		watchers: make(map[string][]WatchCallback),
		name:     name,
	}
}

func (kv *KVStore) Put(key string, value interface{}) {
	kv.mu.Lock()
	kv.data[key] = value
	// Collect watchers while holding the lock
	var callbacks []WatchCallback
	for prefix, cbs := range kv.watchers {
		if strings.HasPrefix(key, prefix) {
			callbacks = append(callbacks, cbs...)
		}
	}
	kv.mu.Unlock()

	// Fire callbacks outside the lock to avoid deadlocks
	for _, cb := range callbacks {
		cb(KVStoreEvent{Type: "PUT", Key: key, Value: value})
	}
}

func (kv *KVStore) Delete(key string) {
	kv.mu.Lock()
	val, exists := kv.data[key]
	if exists {
		delete(kv.data, key)
	}
	var callbacks []WatchCallback
	for prefix, cbs := range kv.watchers {
		if strings.HasPrefix(key, prefix) {
			callbacks = append(callbacks, cbs...)
		}
	}
	kv.mu.Unlock()

	if exists {
		for _, cb := range callbacks {
			cb(KVStoreEvent{Type: "DELETE", Key: key, Value: val})
		}
	}
}

func (kv *KVStore) Get(key string) (interface{}, bool) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	v, ok := kv.data[key]
	return v, ok
}

func (kv *KVStore) ListPrefix(prefix string) map[string]interface{} {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	result := make(map[string]interface{})
	for k, v := range kv.data {
		if strings.HasPrefix(k, prefix) {
			result[k] = v
		}
	}
	return result
}

// Watch는 지정된 prefix에 대한 감시를 등록한다.
// 참조: pkg/kvstore/etcd.go의 Watch/ListAndWatch 패턴
func (kv *KVStore) Watch(prefix string, cb WatchCallback) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.watchers[prefix] = append(kv.watchers[prefix], cb)
}

// =============================================================================
// 4. GlobalServiceCache - 글로벌 서비스 캐시
//    참조: pkg/clustermesh/common/services.go
// =============================================================================

// GlobalService는 같은 이름의 서비스를 여러 클러스터에서 통합한 것.
// 참조: pkg/clustermesh/common/services.go - GlobalService struct
type GlobalService struct {
	ClusterServices map[string]*ClusterService // clusterName -> ClusterService
}

// GlobalServiceCache는 모든 클러스터의 서비스 정보를 통합 관리.
// 참조: pkg/clustermesh/common/services.go - GlobalServiceCache struct
type GlobalServiceCache struct {
	mu     sync.RWMutex
	byName map[string]*GlobalService // namespacedName -> GlobalService
}

func NewGlobalServiceCache() *GlobalServiceCache {
	return &GlobalServiceCache{
		byName: make(map[string]*GlobalService),
	}
}

func (c *GlobalServiceCache) OnUpdate(svc *ClusterService) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nn := svc.NamespacedName()
	gs, ok := c.byName[nn]
	if !ok {
		gs = &GlobalService{
			ClusterServices: make(map[string]*ClusterService),
		}
		c.byName[nn] = gs
	}
	gs.ClusterServices[svc.Cluster] = svc
}

func (c *GlobalServiceCache) OnDelete(svc *ClusterService) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nn := svc.NamespacedName()
	if gs, ok := c.byName[nn]; ok {
		delete(gs.ClusterServices, svc.Cluster)
		if len(gs.ClusterServices) == 0 {
			delete(c.byName, nn)
		}
	}
}

// GetGlobalService는 글로벌 서비스를 조회한다.
func (c *GlobalServiceCache) GetGlobalService(namespacedName string) *GlobalService {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if gs, ok := c.byName[namespacedName]; ok {
		// 얕은 복사 반환 (thread safety)
		copied := &GlobalService{
			ClusterServices: make(map[string]*ClusterService),
		}
		for k, v := range gs.ClusterServices {
			copied.ClusterServices[k] = v
		}
		return copied
	}
	return nil
}

// GetAllServices는 모든 글로벌 서비스를 반환한다.
func (c *GlobalServiceCache) GetAllServices() map[string]*GlobalService {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]*GlobalService)
	for k, v := range c.byName {
		result[k] = v
	}
	return result
}

// =============================================================================
// 5. RemoteIdentityCache - 원격 Identity 캐시
//    참조: pkg/clustermesh/remote_cluster.go의 remoteIdentityCache
// =============================================================================

type RemoteIdentityCache struct {
	mu         sync.RWMutex
	identities map[uint32]*SecurityIdentity // globalID -> identity
	clusterID  uint32
}

func NewRemoteIdentityCache(clusterID uint32) *RemoteIdentityCache {
	return &RemoteIdentityCache{
		identities: make(map[uint32]*SecurityIdentity),
		clusterID:  clusterID,
	}
}

func (c *RemoteIdentityCache) Upsert(id *SecurityIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.identities[id.ID] = id
}

func (c *RemoteIdentityCache) Delete(globalID uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.identities, globalID)
}

func (c *RemoteIdentityCache) GetAll() map[uint32]*SecurityIdentity {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[uint32]*SecurityIdentity)
	for k, v := range c.identities {
		result[k] = v
	}
	return result
}

func (c *RemoteIdentityCache) NumEntries() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.identities)
}

// =============================================================================
// 6. WatcherCache - etcd Watch 캐시 (stale 데이터 정리)
//    참조: pkg/kvstore/watcher_cache.go
// =============================================================================

type watchState struct {
	deletionMark bool
}

type WatcherCache struct {
	entries map[string]watchState
}

func NewWatcherCache() *WatcherCache {
	return &WatcherCache{entries: make(map[string]watchState)}
}

func (wc *WatcherCache) MarkInUse(key string) {
	wc.entries[key] = watchState{deletionMark: false}
}

func (wc *WatcherCache) MarkAllForDeletion() {
	for k := range wc.entries {
		wc.entries[k] = watchState{deletionMark: true}
	}
}

func (wc *WatcherCache) RemoveDeleted() []string {
	var deleted []string
	for k, s := range wc.entries {
		if s.deletionMark {
			deleted = append(deleted, k)
			delete(wc.entries, k)
		}
	}
	return deleted
}

// =============================================================================
// 7. Cluster - 개별 클러스터 시뮬레이션
// =============================================================================

// Cluster는 하나의 Kubernetes 클러스터를 시뮬레이션한다.
type Cluster struct {
	Info ClusterInfo

	// 로컬 저장소
	Services   map[string]*ClusterService
	Identities map[uint32]*SecurityIdentity
	Endpoints  map[string]*Endpoint

	// etcd (clustermesh-apiserver가 동기화하는 대상)
	KVStore *KVStore

	// ClusterMesh 컴포넌트
	GlobalServices      *GlobalServiceCache
	RemoteIdentityCaches map[string]*RemoteIdentityCache // clusterName -> cache
	UsedIDs             *ClusterMeshUsedIDs
	Policies            []*NetworkPolicy

	// 로컬 Identity 할당 카운터
	nextLocalIdentity uint32
}

func NewCluster(id uint32, name string) *Cluster {
	return &Cluster{
		Info:                 ClusterInfo{ID: id, Name: name},
		Services:             make(map[string]*ClusterService),
		Identities:           make(map[uint32]*SecurityIdentity),
		Endpoints:            make(map[string]*Endpoint),
		KVStore:              NewKVStore(name),
		GlobalServices:       NewGlobalServiceCache(),
		RemoteIdentityCaches: make(map[string]*RemoteIdentityCache),
		UsedIDs:              NewClusterMeshUsedIDs(id),
		nextLocalIdentity:    1,
	}
}

// AllocateIdentity는 로컬 Identity를 할당하고 글로벌 ID를 반환한다.
func (c *Cluster) AllocateIdentity(labels map[string]string) *SecurityIdentity {
	localID := c.nextLocalIdentity
	c.nextLocalIdentity++

	globalID := GlobalIdentityID(c.Info.ID, localID)

	id := &SecurityIdentity{
		ID:        globalID,
		ClusterID: c.Info.ID,
		Labels:    labels,
	}
	c.Identities[globalID] = id

	// clustermesh-apiserver가 etcd에 동기화하는 것을 시뮬레이션
	key := fmt.Sprintf("cilium/state/identities/v1/%d", globalID)
	c.KVStore.Put(key, id)

	return id
}

// AddService는 서비스를 추가하고 etcd에 동기화한다.
func (c *Cluster) AddService(namespace, name string, shared bool, backends map[string]PortConfig, labels map[string]string) *ClusterService {
	svc := &ClusterService{
		Cluster:   c.Info.Name,
		Namespace: namespace,
		Name:      name,
		ClusterID: c.Info.ID,
		Backends:  backends,
		Shared:    shared,
		Labels:    labels,
	}

	c.Services[svc.NamespacedName()] = svc

	// clustermesh-apiserver가 etcd에 동기화하는 것을 시뮬레이션
	// 참조: pkg/clustermesh/store/store.go - ServiceStorePrefix
	key := fmt.Sprintf("cilium/state/services/v1/%s", svc.GetKeyName())
	c.KVStore.Put(key, svc)

	return svc
}

// AddEndpoint는 엔드포인트를 추가한다.
func (c *Cluster) AddEndpoint(ip, serviceNS, serviceName string, identity uint32) *Endpoint {
	ep := &Endpoint{
		IP:        ip,
		ServiceNS: serviceNS,
		ServiceN:  serviceName,
		Identity:  identity,
		ClusterID: c.Info.ID,
	}
	c.Endpoints[ip] = ep

	key := fmt.Sprintf("cilium/state/ip/v1/%s", ip)
	c.KVStore.Put(key, ep)

	return ep
}

// AddPolicy는 네트워크 정책을 추가한다.
func (c *Cluster) AddPolicy(policy *NetworkPolicy) {
	c.Policies = append(c.Policies, policy)
}

// =============================================================================
// 8. ClusterMesh 동기화 로직
//    참조: pkg/clustermesh/remote_cluster.go - Run
// =============================================================================

// SyncRemoteCluster는 원격 클러스터의 데이터를 로컬 클러스터에 동기화한다.
// 이는 Cilium 에이전트가 원격 etcd를 Watch하는 것을 시뮬레이션한다.
func SyncRemoteCluster(local *Cluster, remote *Cluster) error {
	fmt.Printf("\n  [Sync] %s -> %s 동기화 시작\n", remote.Info.Name, local.Info.Name)

	// 1. Cluster ID 예약
	// 참조: pkg/clustermesh/idsmgr.go - ReserveClusterID
	if err := local.UsedIDs.ReserveClusterID(remote.Info.ID); err != nil {
		return fmt.Errorf("cluster ID 예약 실패: %w", err)
	}
	fmt.Printf("    [ID] ClusterID %d 예약 완료 (from %s)\n", remote.Info.ID, remote.Info.Name)

	// 2. Identity 동기화
	// 참조: pkg/clustermesh/remote_cluster.go - WatchRemoteIdentities
	remoteIDCache := NewRemoteIdentityCache(remote.Info.ID)
	local.RemoteIdentityCaches[remote.Info.Name] = remoteIDCache

	identityPrefix := "cilium/state/identities/v1/"
	identities := remote.KVStore.ListPrefix(identityPrefix)
	for _, v := range identities {
		if id, ok := v.(*SecurityIdentity); ok {
			remoteIDCache.Upsert(id)
		}
	}
	fmt.Printf("    [Identity] %d개의 원격 Identity 동기화 완료\n", remoteIDCache.NumEntries())

	// 3. 서비스 동기화 (Shared 서비스만)
	// 참조: pkg/clustermesh/common/services.go - remoteServiceObserver.OnUpdate
	servicePrefix := fmt.Sprintf("cilium/state/services/v1/%s/", remote.Info.Name)
	services := remote.KVStore.ListPrefix(servicePrefix)
	sharedCount := 0
	for _, v := range services {
		if svc, ok := v.(*ClusterService); ok {
			if !svc.Shared {
				fmt.Printf("    [Service] %s: 공유되지 않은 서비스 무시\n", svc.NamespacedName())
				continue
			}
			local.GlobalServices.OnUpdate(svc)
			sharedCount++
		}
	}
	fmt.Printf("    [Service] %d개의 공유 서비스 동기화 완료\n", sharedCount)

	// 4. Watch 등록 (실시간 업데이트 수신)
	// 참조: pkg/clustermesh/remote_cluster.go - Run 내부의 mgr.Register
	remote.KVStore.Watch(servicePrefix, func(event KVStoreEvent) {
		if svc, ok := event.Value.(*ClusterService); ok {
			switch event.Type {
			case "PUT":
				if svc.Shared {
					local.GlobalServices.OnUpdate(svc)
					fmt.Printf("    [Watch] %s에서 서비스 업데이트 수신: %s\n",
						remote.Info.Name, svc.NamespacedName())
				}
			case "DELETE":
				local.GlobalServices.OnDelete(svc)
				fmt.Printf("    [Watch] %s에서 서비스 삭제 수신: %s\n",
					remote.Info.Name, svc.NamespacedName())
			}
		}
	})

	remote.KVStore.Watch(identityPrefix, func(event KVStoreEvent) {
		if id, ok := event.Value.(*SecurityIdentity); ok {
			switch event.Type {
			case "PUT":
				remoteIDCache.Upsert(id)
			case "DELETE":
				remoteIDCache.Delete(id.ID)
			}
		}
	})

	fmt.Printf("  [Sync] %s -> %s 동기화 완료\n", remote.Info.Name, local.Info.Name)
	return nil
}

// =============================================================================
// 9. 크로스 클러스터 네트워크 정책 평가
//    참조: CiliumNetworkPolicy, CCNP
// =============================================================================

// EvaluatePolicy는 소스에서 대상으로의 트래픽이 정책에 의해 허용되는지 평가한다.
func EvaluatePolicy(
	localCluster *Cluster,
	srcIdentity *SecurityIdentity,
	srcClusterName string,
	dstLabels map[string]string,
) (allowed bool, matchedPolicy string) {
	for _, policy := range localCluster.Policies {
		// 대상 라벨 매칭 확인
		if !labelsMatch(dstLabels, policy.TargetLabels) {
			continue
		}

		// DenyAll 정책
		if policy.DenyAll {
			return false, policy.Name + " (deny-all)"
		}

		// 소스 라벨 매칭 확인
		if !labelsMatch(srcIdentity.Labels, policy.AllowedSourceLabels) {
			continue
		}

		// 클러스터 제한 확인
		// 참조: pkg/clustermesh/types/option.go - PolicyConfig.PolicyDefaultLocalCluster
		if policy.AllowedCluster != "" && policy.AllowedCluster != srcClusterName {
			continue
		}

		return true, policy.Name
	}

	// 기본 거부 (정책이 있지만 매칭되는 것이 없으면)
	return false, "(no matching policy)"
}

func labelsMatch(src, required map[string]string) bool {
	for k, v := range required {
		if sv, ok := src[k]; !ok || sv != v {
			return false
		}
	}
	return true
}

// =============================================================================
// 10. 서비스 디스커버리 시뮬레이션
//     참조: pkg/clustermesh/selectbackends.go
// =============================================================================

// DiscoverBackends는 글로벌 서비스에서 백엔드를 검색한다.
// ServiceAffinity를 시뮬레이션한다.
func DiscoverBackends(
	cache *GlobalServiceCache,
	localCluster string,
	namespacedName string,
	affinity string, // "none", "local", "remote"
) []string {
	gs := cache.GetGlobalService(namespacedName)
	if gs == nil {
		return nil
	}

	var localBackends, remoteBackends []string

	for clusterName, svc := range gs.ClusterServices {
		for ip := range svc.Backends {
			if clusterName == localCluster {
				localBackends = append(localBackends, fmt.Sprintf("%s@%s", ip, clusterName))
			} else {
				remoteBackends = append(remoteBackends, fmt.Sprintf("%s@%s", ip, clusterName))
			}
		}
	}

	sort.Strings(localBackends)
	sort.Strings(remoteBackends)

	// 참조: pkg/clustermesh/selectbackends.go - SelectBackends
	switch affinity {
	case "local":
		if len(localBackends) > 0 {
			return localBackends
		}
		return remoteBackends // 로컬이 없으면 원격 사용
	case "remote":
		if len(remoteBackends) > 0 {
			return remoteBackends
		}
		return localBackends // 원격이 없으면 로컬 사용
	default: // "none"
		return append(localBackends, remoteBackends...)
	}
}

// =============================================================================
// 11. WatcherCache 데모
//     참조: pkg/kvstore/watcher_cache.go
// =============================================================================

func demoWatcherCache() {
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("DEMO 5: WatcherCache - etcd 재연결 시 stale 데이터 정리")
	fmt.Println(strings.Repeat("=", 70))

	cache := NewWatcherCache()

	// 초기 데이터 로드
	fmt.Println("\n[Phase 1] 초기 데이터 로드")
	cache.MarkInUse("cilium/state/services/v1/cluster-a/default/svc-1")
	cache.MarkInUse("cilium/state/services/v1/cluster-a/default/svc-2")
	cache.MarkInUse("cilium/state/services/v1/cluster-a/default/svc-3")
	fmt.Printf("  캐시된 키: %d개\n", len(cache.entries))

	// 재연결 시뮬레이션: 모든 키를 삭제 후보로 표시
	fmt.Println("\n[Phase 2] etcd 재연결 - 모든 키를 삭제 후보로 표시")
	cache.MarkAllForDeletion()
	fmt.Printf("  삭제 후보: %d개\n", len(cache.entries))

	// 새 List 결과에서 존재하는 키만 복원
	fmt.Println("\n[Phase 3] 새 List 결과 수신 (svc-1, svc-3만 존재)")
	cache.MarkInUse("cilium/state/services/v1/cluster-a/default/svc-1")
	cache.MarkInUse("cilium/state/services/v1/cluster-a/default/svc-3")

	// stale 키 정리
	fmt.Println("\n[Phase 4] stale 키 정리")
	deleted := cache.RemoveDeleted()
	for _, key := range deleted {
		fmt.Printf("  [삭제] %s (stale)\n", key)
	}
	fmt.Printf("  최종 캐시: %d개\n", len(cache.entries))
}

// =============================================================================
// 12. 메인 데모
// =============================================================================

func main() {
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("Cilium ClusterMesh PoC - 멀티클러스터 메커니즘 시뮬레이션")
	fmt.Println(strings.Repeat("=", 70))

	// =====================================================================
	// DEMO 1: 클러스터 설정 및 Identity 충돌 방지
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("DEMO 1: 클러스터 설정 및 ClusterID 기반 Identity 충돌 방지")
	fmt.Println(strings.Repeat("=", 70))

	// 3개 클러스터 생성
	clusterA := NewCluster(1, "cluster-a")
	clusterB := NewCluster(2, "cluster-b")
	clusterC := NewCluster(3, "cluster-c")

	fmt.Printf("\n클러스터 생성 완료:\n")
	fmt.Printf("  - %s (ID=%d)\n", clusterA.Info.Name, clusterA.Info.ID)
	fmt.Printf("  - %s (ID=%d)\n", clusterB.Info.Name, clusterB.Info.ID)
	fmt.Printf("  - %s (ID=%d)\n", clusterC.Info.Name, clusterC.Info.ID)

	// 각 클러스터에서 동일한 라벨의 Identity 할당
	fmt.Println("\n--- 같은 라벨 {app:frontend}로 각 클러스터에서 Identity 할당 ---")
	idA := clusterA.AllocateIdentity(map[string]string{"app": "frontend"})
	idB := clusterB.AllocateIdentity(map[string]string{"app": "frontend"})
	idC := clusterC.AllocateIdentity(map[string]string{"app": "frontend"})

	fmt.Printf("\n  %-12s: GlobalID=%-8d (ClusterID=%d, LocalID=%d)\n",
		clusterA.Info.Name, idA.ID, ExtractClusterID(idA.ID), ExtractLocalID(idA.ID))
	fmt.Printf("  %-12s: GlobalID=%-8d (ClusterID=%d, LocalID=%d)\n",
		clusterB.Info.Name, idB.ID, ExtractClusterID(idB.ID), ExtractLocalID(idB.ID))
	fmt.Printf("  %-12s: GlobalID=%-8d (ClusterID=%d, LocalID=%d)\n",
		clusterC.Info.Name, idC.ID, ExtractClusterID(idC.ID), ExtractLocalID(idC.ID))

	fmt.Printf("\n  => 같은 라벨이지만 ClusterID 덕분에 모든 Identity가 고유합니다!\n")
	fmt.Printf("     ID 충돌 없음: %d != %d != %d\n", idA.ID, idB.ID, idC.ID)

	// 추가 Identity 할당
	idABackend := clusterA.AllocateIdentity(map[string]string{"app": "backend"})
	idBBackend := clusterB.AllocateIdentity(map[string]string{"app": "backend"})
	idCBackend := clusterC.AllocateIdentity(map[string]string{"app": "backend"})

	// =====================================================================
	// DEMO 2: 서비스 생성 및 글로벌 서비스 동기화
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("DEMO 2: 글로벌 서비스 동기화 (ClusterMesh Sync)")
	fmt.Println(strings.Repeat("=", 70))

	// 각 클러스터에 서비스 생성
	clusterA.AddService("default", "web-api", true,
		map[string]PortConfig{
			"10.0.1.10": {Port: 8080, Protocol: "TCP"},
			"10.0.1.11": {Port: 8080, Protocol: "TCP"},
		},
		map[string]string{"app": "web-api"},
	)

	clusterB.AddService("default", "web-api", true,
		map[string]PortConfig{
			"10.0.2.10": {Port: 8080, Protocol: "TCP"},
		},
		map[string]string{"app": "web-api"},
	)

	clusterC.AddService("default", "web-api", true,
		map[string]PortConfig{
			"10.0.3.10": {Port: 8080, Protocol: "TCP"},
			"10.0.3.11": {Port: 8080, Protocol: "TCP"},
			"10.0.3.12": {Port: 8080, Protocol: "TCP"},
		},
		map[string]string{"app": "web-api"},
	)

	// 비공유 서비스 (Shared=false)
	clusterA.AddService("default", "internal-db", false,
		map[string]PortConfig{
			"10.0.1.50": {Port: 5432, Protocol: "TCP"},
		},
		map[string]string{"app": "postgres"},
	)

	// 엔드포인트 추가
	clusterA.AddEndpoint("10.0.1.10", "default", "web-api", idABackend.ID)
	clusterB.AddEndpoint("10.0.2.10", "default", "web-api", idBBackend.ID)
	clusterC.AddEndpoint("10.0.3.10", "default", "web-api", idCBackend.ID)

	fmt.Printf("\n서비스 생성 완료:\n")
	fmt.Printf("  cluster-a: web-api (shared=true, backends=2), internal-db (shared=false, backends=1)\n")
	fmt.Printf("  cluster-b: web-api (shared=true, backends=1)\n")
	fmt.Printf("  cluster-c: web-api (shared=true, backends=3)\n")

	// ClusterMesh 동기화: A에서 B, C의 서비스를 동기화
	fmt.Println("\n--- cluster-a에서 원격 클러스터 동기화 ---")
	if err := SyncRemoteCluster(clusterA, clusterB); err != nil {
		fmt.Printf("  [ERROR] %v\n", err)
	}
	if err := SyncRemoteCluster(clusterA, clusterC); err != nil {
		fmt.Printf("  [ERROR] %v\n", err)
	}

	// 로컬 서비스도 GlobalServiceCache에 추가
	for _, svc := range clusterA.Services {
		if svc.Shared {
			clusterA.GlobalServices.OnUpdate(svc)
		}
	}

	// =====================================================================
	// DEMO 3: 글로벌 서비스 디스커버리 및 ServiceAffinity
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("DEMO 3: 글로벌 서비스 디스커버리 및 ServiceAffinity")
	fmt.Println(strings.Repeat("=", 70))

	// GlobalServiceCache 상태 출력
	fmt.Println("\nGlobalServiceCache 상태 (cluster-a 관점):")
	allServices := clusterA.GlobalServices.GetAllServices()
	for nn, gs := range allServices {
		fmt.Printf("  서비스: %s\n", nn)
		for cluster, svc := range gs.ClusterServices {
			backends := make([]string, 0)
			for ip := range svc.Backends {
				backends = append(backends, ip)
			}
			sort.Strings(backends)
			fmt.Printf("    - %s: backends=%v\n", cluster, backends)
		}
	}

	// 다양한 Affinity 설정으로 백엔드 검색
	fmt.Println("\n--- ServiceAffinity에 따른 백엔드 선택 (cluster-a 관점) ---")

	affinities := []string{"none", "local", "remote"}
	for _, aff := range affinities {
		backends := DiscoverBackends(clusterA.GlobalServices, "cluster-a", "default/web-api", aff)
		fmt.Printf("\n  affinity=%s:\n", aff)
		for _, b := range backends {
			fmt.Printf("    - %s\n", b)
		}
	}

	// =====================================================================
	// DEMO 4: 크로스 클러스터 네트워크 정책 평가
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("DEMO 4: 크로스 클러스터 네트워크 정책 평가")
	fmt.Println(strings.Repeat("=", 70))

	// 정책 추가
	clusterA.AddPolicy(&NetworkPolicy{
		Name:                "allow-frontend-to-backend",
		TargetLabels:        map[string]string{"app": "backend"},
		AllowedSourceLabels: map[string]string{"app": "frontend"},
		AllowedCluster:      "", // 모든 클러스터 허용
	})

	clusterA.AddPolicy(&NetworkPolicy{
		Name:                "allow-local-frontend-only",
		TargetLabels:        map[string]string{"app": "web-api"},
		AllowedSourceLabels: map[string]string{"app": "frontend"},
		AllowedCluster:      "cluster-a", // cluster-a에서만 허용
	})

	clusterA.AddPolicy(&NetworkPolicy{
		Name:         "deny-all-to-db",
		TargetLabels: map[string]string{"app": "postgres"},
		DenyAll:      true,
	})

	fmt.Println("\n정책:")
	fmt.Println("  1. allow-frontend-to-backend: frontend -> backend (모든 클러스터)")
	fmt.Println("  2. allow-local-frontend-only: frontend -> web-api (cluster-a만)")
	fmt.Println("  3. deny-all-to-db: * -> postgres (모두 거부)")

	// 정책 평가 시나리오
	fmt.Println("\n--- 정책 평가 결과 ---")

	testCases := []struct {
		srcCluster string
		srcID      *SecurityIdentity
		dstLabels  map[string]string
		desc       string
	}{
		{"cluster-a", idA, map[string]string{"app": "backend"}, "cluster-a frontend -> backend"},
		{"cluster-b", idB, map[string]string{"app": "backend"}, "cluster-b frontend -> backend"},
		{"cluster-c", idC, map[string]string{"app": "backend"}, "cluster-c frontend -> backend"},
		{"cluster-a", idA, map[string]string{"app": "web-api"}, "cluster-a frontend -> web-api"},
		{"cluster-b", idB, map[string]string{"app": "web-api"}, "cluster-b frontend -> web-api"},
		{"cluster-a", idA, map[string]string{"app": "postgres"}, "cluster-a frontend -> postgres"},
	}

	for _, tc := range testCases {
		allowed, policy := EvaluatePolicy(clusterA, tc.srcID, tc.srcCluster, tc.dstLabels)
		status := "ALLOW"
		if !allowed {
			status = "DENY "
		}
		fmt.Printf("  [%s] %-45s => 정책: %s\n", status, tc.desc, policy)
	}

	// =====================================================================
	// DEMO 5: WatcherCache
	// =====================================================================
	demoWatcherCache()

	// =====================================================================
	// DEMO 6: Cluster ID 충돌 감지
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("DEMO 6: Cluster ID 충돌 감지")
	fmt.Println(strings.Repeat("=", 70))

	fmt.Println("\ncluster-a에서 이미 예약된 Cluster ID로 새 클러스터 연결 시도:")

	// 이미 예약된 ID 재사용 시도
	err := clusterA.UsedIDs.ReserveClusterID(2) // cluster-b의 ID
	fmt.Printf("  ReserveClusterID(2): %v\n", err)

	// 로컬 ID 사용 시도
	err = clusterA.UsedIDs.ReserveClusterID(1) // 로컬 ID
	fmt.Printf("  ReserveClusterID(1): %v\n", err)

	// 범위 초과
	err = clusterA.UsedIDs.ReserveClusterID(300)
	fmt.Printf("  ReserveClusterID(300): %v\n", err)

	// ID 0 사용 시도
	err = clusterA.UsedIDs.ReserveClusterID(0)
	fmt.Printf("  ReserveClusterID(0): %v\n", err)

	// 새 유효 ID
	err = clusterA.UsedIDs.ReserveClusterID(4)
	fmt.Printf("  ReserveClusterID(4): %v\n", err)

	// =====================================================================
	// DEMO 7: 실시간 업데이트 (Watch 시뮬레이션)
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("DEMO 7: 실시간 서비스 업데이트 (etcd Watch 시뮬레이션)")
	fmt.Println(strings.Repeat("=", 70))

	// cluster-b에 새 백엔드 추가
	fmt.Println("\ncluster-b에 web-api 백엔드 추가 (10.0.2.11):")
	clusterB.AddService("default", "web-api", true,
		map[string]PortConfig{
			"10.0.2.10": {Port: 8080, Protocol: "TCP"},
			"10.0.2.11": {Port: 8080, Protocol: "TCP"}, // 새 백엔드
		},
		map[string]string{"app": "web-api"},
	)

	// Watch 이벤트로 cluster-a의 글로벌 서비스 캐시가 업데이트됨
	fmt.Println("\n업데이트 후 cluster-a의 글로벌 서비스 캐시:")
	gs := clusterA.GlobalServices.GetGlobalService("default/web-api")
	if gs != nil {
		for cluster, svc := range gs.ClusterServices {
			backends := make([]string, 0)
			for ip := range svc.Backends {
				backends = append(backends, ip)
			}
			sort.Strings(backends)
			fmt.Printf("  %s: %v\n", cluster, backends)
		}
	}

	// 전체 백엔드 (affinity=none)
	allBackends := DiscoverBackends(clusterA.GlobalServices, "cluster-a", "default/web-api", "none")
	fmt.Printf("\n전체 글로벌 백엔드 (affinity=none):\n")
	for _, b := range allBackends {
		fmt.Printf("  - %s\n", b)
	}

	// =====================================================================
	// DEMO 8: 크로스 클러스터 Identity 매핑 조회
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("DEMO 8: 크로스 클러스터 Identity 매핑 조회")
	fmt.Println(strings.Repeat("=", 70))

	fmt.Println("\ncluster-a의 원격 Identity 캐시:")
	for remoteName, cache := range clusterA.RemoteIdentityCaches {
		ids := cache.GetAll()
		fmt.Printf("\n  원격 클러스터: %s (%d개의 Identity)\n", remoteName, len(ids))
		// Sort by ID for consistent output
		sortedIDs := make([]uint32, 0, len(ids))
		for id := range ids {
			sortedIDs = append(sortedIDs, id)
		}
		sort.Slice(sortedIDs, func(i, j int) bool { return sortedIDs[i] < sortedIDs[j] })

		for _, globalID := range sortedIDs {
			id := ids[globalID]
			labels := make([]string, 0)
			for k, v := range id.Labels {
				labels = append(labels, fmt.Sprintf("%s=%s", k, v))
			}
			sort.Strings(labels)
			fmt.Printf("    GlobalID=%-8d ClusterID=%d LocalID=%-5d Labels={%s}\n",
				globalID, ExtractClusterID(globalID), ExtractLocalID(globalID),
				strings.Join(labels, ", "))
		}
	}

	// Identity를 사용한 패킷 출처 확인
	fmt.Println("\n--- Identity로 패킷 출처 확인 ---")
	testIdentities := []*SecurityIdentity{idA, idB, idC, idABackend, idBBackend, idCBackend}
	for _, id := range testIdentities {
		clusterID := ExtractClusterID(id.ID)
		localID := ExtractLocalID(id.ID)
		labels := make([]string, 0)
		for k, v := range id.Labels {
			labels = append(labels, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(labels)
		clusterName := "unknown"
		switch clusterID {
		case 1:
			clusterName = "cluster-a"
		case 2:
			clusterName = "cluster-b"
		case 3:
			clusterName = "cluster-c"
		}
		fmt.Printf("  ID=%-8d -> Cluster=%s(ID=%d) Local=%d Labels={%s}\n",
			id.ID, clusterName, clusterID, localID, strings.Join(labels, ", "))
	}

	// =====================================================================
	// 요약
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("요약: ClusterMesh 핵심 메커니즘")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println(`
  1. ClusterID: 각 클러스터에 고유 ID(1-255)를 할당하여 Identity 충돌 방지
     - GlobalIdentity = (ClusterID << 16) | LocalIdentity
     - ClusterMeshUsedIDs가 ID 고유성 보장

  2. 서비스 동기화: clustermesh-apiserver가 K8s 서비스를 etcd에 동기화
     - Shared=true인 서비스만 원격에 공유
     - GlobalServiceCache가 모든 클러스터의 서비스 통합 관리

  3. ServiceAffinity: 백엔드 선택 우선순위 제어
     - none: 모든 백엔드 사용
     - local: 로컬 우선, 없으면 원격 사용
     - remote: 원격 우선, 없으면 로컬 사용

  4. 크로스 클러스터 정책: Identity 기반으로 클러스터 경계를 넘는 정책 적용
     - AllowedCluster로 특정 클러스터의 트래픽만 허용 가능

  5. WatcherCache: etcd 재연결 시 stale 데이터를 감지하고 정리
     - MarkAllForDeletion -> MarkInUse -> RemoveDeleted 패턴

  6. KVStoreMesh: 원격 etcd를 로컬 etcd로 캐싱하여 대규모 확장성 지원
`)
}
