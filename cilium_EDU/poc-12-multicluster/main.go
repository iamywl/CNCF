package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium 멀티클러스터 (ClusterMesh) 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/clustermesh/clustermesh.go         : ClusterMesh 메인 구조체
//   - pkg/clustermesh/remote_cluster.go      : remoteCluster (원격 클러스터 연결)
//   - pkg/clustermesh/common/services.go     : GlobalServiceCache, GlobalService
//   - pkg/clustermesh/store/store.go         : ClusterService (KVStore 서비스 데이터)
//   - pkg/clustermesh/kvstoremesh/kvstoremesh.go : KVStoreMesh (캐싱 프록시)
//   - pkg/clustermesh/service_merger.go      : serviceMerger (로컬 LB와 병합)
//   - pkg/clustermesh/selectbackends.go      : ServiceAffinity 기반 백엔드 선택
//   - pkg/clustermesh/types/types.go         : ClusterInfo, CiliumClusterConfig
//   - pkg/clustermesh/idsmgr.go              : ClusterID 관리
//
// 핵심 구조:
//   각 클러스터 → etcd에 서비스/ID/노드 정보 게시
//   KVStoreMesh → 원격 etcd 감시 → 로컬 etcd에 캐시
//   ClusterMesh → 캐시된 데이터 소비 → GlobalServiceCache → LB 병합

// =============================================================================
// 1. 데이터 모델 — KVStore 서비스 정의
// =============================================================================

// ClusterInfo는 클러스터 기본 정보
// 실제: pkg/clustermesh/types/types.go
type ClusterInfo struct {
	Name string
	ID   uint32
}

// L4Addr은 L4 주소
// 실제: pkg/loadbalancer/loadbalancer.go
type L4Addr struct {
	Protocol string
	Port     uint16
}

// PortConfiguration은 포트 설정 맵
// 실제: pkg/clustermesh/store/store.go의 PortConfiguration
type PortConfiguration map[string]*L4Addr

// ClusterService는 클러스터 서비스 정의
// 실제: pkg/clustermesh/store/store.go의 ClusterService
// WARNING — STABLE API: 구조 변경 시 하위 호환성 깨짐
type ClusterService struct {
	Cluster         string                       `json:"cluster"`
	Namespace       string                       `json:"namespace"`
	Name            string                       `json:"name"`
	Frontends       map[string]PortConfiguration `json:"frontends"`
	Backends        map[string]PortConfiguration `json:"backends"`
	Labels          map[string]string            `json:"labels"`
	Selector        map[string]string            `json:"selector"`
	IncludeExternal bool                         `json:"includeExternal"`
	Shared          bool                         `json:"shared"`
	ClusterID       uint32                       `json:"clusterID"`
}

func (s *ClusterService) String() string {
	return fmt.Sprintf("%s/%s/%s", s.Cluster, s.Namespace, s.Name)
}

func (s *ClusterService) NamespacedName() string {
	return s.Namespace + "/" + s.Name
}

// NumericIdentity는 보안 Identity
type NumericIdentity uint32

// IdentityEntry는 KVStore의 Identity 항목
type IdentityEntry struct {
	ID      NumericIdentity   `json:"id"`
	Labels  map[string]string `json:"labels"`
	Cluster string            `json:"cluster"`
}

// =============================================================================
// 2. KVStore — etcd 시뮬레이션
// =============================================================================
// 실제: pkg/kvstore/ 패키지 — etcd 클라이언트
// 키 구조: cilium/state/services/v1/<cluster>/<namespace>/<name>

type KVStore struct {
	mu       sync.RWMutex
	data     map[string][]byte
	watchers map[string][]chan WatchEvent
}

type WatchEvent struct {
	Key    string
	Value  []byte
	Delete bool
}

func NewKVStore() *KVStore {
	return &KVStore{
		data:     make(map[string][]byte),
		watchers: make(map[string][]chan WatchEvent),
	}
}

func (kv *KVStore) Put(key string, value []byte) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.data[key] = value
	kv.notifyWatchers(key, WatchEvent{Key: key, Value: value})
}

func (kv *KVStore) Get(key string) ([]byte, bool) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	v, ok := kv.data[key]
	return v, ok
}

func (kv *KVStore) Delete(key string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.data, key)
	kv.notifyWatchers(key, WatchEvent{Key: key, Delete: true})
}

func (kv *KVStore) ListPrefix(prefix string) map[string][]byte {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	result := make(map[string][]byte)
	for k, v := range kv.data {
		if strings.HasPrefix(k, prefix) {
			result[k] = v
		}
	}
	return result
}

func (kv *KVStore) Watch(prefix string) chan WatchEvent {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	ch := make(chan WatchEvent, 100)
	kv.watchers[prefix] = append(kv.watchers[prefix], ch)
	return ch
}

func (kv *KVStore) notifyWatchers(key string, event WatchEvent) {
	for prefix, channels := range kv.watchers {
		if strings.HasPrefix(key, prefix) {
			for _, ch := range channels {
				select {
				case ch <- event:
				default:
				}
			}
		}
	}
}

func (kv *KVStore) Size() int {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return len(kv.data)
}

// =============================================================================
// 3. GlobalServiceCache — pkg/clustermesh/common/services.go 재현
// =============================================================================

// GlobalService는 여러 클러스터에 걸친 글로벌 서비스
// 실제: pkg/clustermesh/common/services.go의 GlobalService
type GlobalService struct {
	ClusterServices map[string]*ClusterService
}

// GlobalServiceCache는 글로벌 서비스 캐시
// 실제: pkg/clustermesh/common/services.go의 GlobalServiceCache
type GlobalServiceCache struct {
	mu     sync.RWMutex
	byName map[string]*GlobalService
}

func NewGlobalServiceCache() *GlobalServiceCache {
	return &GlobalServiceCache{byName: make(map[string]*GlobalService)}
}

// OnUpdate는 서비스 업데이트 이벤트 처리
// 실제: GlobalServiceCache.OnUpdate() — shared 서비스만 캐시
func (c *GlobalServiceCache) OnUpdate(svc *ClusterService) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nn := svc.NamespacedName()
	gs, ok := c.byName[nn]
	if !ok {
		gs = &GlobalService{ClusterServices: make(map[string]*ClusterService)}
		c.byName[nn] = gs
	}
	gs.ClusterServices[svc.Cluster] = svc
}

// OnDelete는 서비스 삭제 이벤트 처리
// 실제: GlobalServiceCache.OnDelete()
func (c *GlobalServiceCache) OnDelete(svc *ClusterService) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	nn := svc.NamespacedName()
	gs, ok := c.byName[nn]
	if !ok {
		return false
	}
	if _, exists := gs.ClusterServices[svc.Cluster]; !exists {
		return false
	}
	delete(gs.ClusterServices, svc.Cluster)
	if len(gs.ClusterServices) == 0 {
		delete(c.byName, nn)
	}
	return true
}

func (c *GlobalServiceCache) GetGlobalService(nn string) *GlobalService {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byName[nn]
}

func (c *GlobalServiceCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byName)
}

func (c *GlobalServiceCache) AllServices() map[string]*GlobalService {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]*GlobalService, len(c.byName))
	for k, v := range c.byName {
		result[k] = v
	}
	return result
}

// =============================================================================
// 4. Identity 동기화 캐시
// =============================================================================

// IdentityCache는 클러스터 간 Identity 동기화 캐시
// 실제: pkg/allocator/RemoteIDCache
type IdentityCache struct {
	mu         sync.RWMutex
	identities map[NumericIdentity]*IdentityEntry
}

func NewIdentityCache() *IdentityCache {
	return &IdentityCache{identities: make(map[NumericIdentity]*IdentityEntry)}
}

func (ic *IdentityCache) Upsert(entry *IdentityEntry) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	ic.identities[entry.ID] = entry
}

func (ic *IdentityCache) Delete(id NumericIdentity) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	delete(ic.identities, id)
}

func (ic *IdentityCache) Size() int {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	return len(ic.identities)
}

// =============================================================================
// 5. RemoteCluster — pkg/clustermesh/remote_cluster.go 재현
// =============================================================================

// RemoteCluster는 원격 클러스터 연결
// 실제: pkg/clustermesh/remote_cluster.go의 remoteCluster
type RemoteCluster struct {
	name           string
	clusterID      uint32
	kvstore        *KVStore
	globalSvcCache *GlobalServiceCache
	identityCache  *IdentityCache
	connected      bool
	synced         bool
}

func (rc *RemoteCluster) Connect() { rc.connected = true }

// SyncServices는 원격 서비스를 동기화
// 실제: remoteCluster.Run() → store.WatchStore
func (rc *RemoteCluster) SyncServices() int {
	prefix := "cilium/state/services/v1/" + rc.name + "/"
	data := rc.kvstore.ListPrefix(prefix)
	count := 0
	for _, value := range data {
		var svc ClusterService
		if err := json.Unmarshal(value, &svc); err == nil && svc.Shared {
			rc.globalSvcCache.OnUpdate(&svc)
			count++
		}
	}
	rc.synced = true
	return count
}

// SyncIdentities는 원격 Identity를 동기화
// 실제: remoteIdentityWatcher.WatchRemoteIdentities()
func (rc *RemoteCluster) SyncIdentities() int {
	prefix := "cilium/state/identities/v1/" + rc.name + "/"
	data := rc.kvstore.ListPrefix(prefix)
	count := 0
	for _, value := range data {
		var entry IdentityEntry
		if err := json.Unmarshal(value, &entry); err == nil {
			rc.identityCache.Upsert(&entry)
			count++
		}
	}
	return count
}

// =============================================================================
// 6. ClusterMesh — pkg/clustermesh/clustermesh.go 재현
// =============================================================================

type ClusterMesh struct {
	localCluster   ClusterInfo
	remoteClusters map[string]*RemoteCluster
	globalSvcCache *GlobalServiceCache
	identityCache  *IdentityCache
	mu             sync.RWMutex
}

func NewClusterMesh(local ClusterInfo) *ClusterMesh {
	return &ClusterMesh{
		localCluster:   local,
		remoteClusters: make(map[string]*RemoteCluster),
		globalSvcCache: NewGlobalServiceCache(),
		identityCache:  NewIdentityCache(),
	}
}

func (cm *ClusterMesh) AddRemoteCluster(name string, id uint32, kv *KVStore) *RemoteCluster {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	rc := &RemoteCluster{
		name: name, clusterID: id, kvstore: kv,
		globalSvcCache: cm.globalSvcCache,
		identityCache:  cm.identityCache,
	}
	cm.remoteClusters[name] = rc
	return rc
}

// BackendInfo는 백엔드 정보
type BackendInfo struct {
	Address     string
	Port        uint16
	Protocol    string
	PortName    string
	ClusterName string
	ClusterID   uint32
	IsLocal     bool
}

func (bi BackendInfo) String() string {
	loc := "remote"
	if bi.IsLocal {
		loc = "local"
	}
	return fmt.Sprintf("%s:%d/%s [%s/%s]", bi.Address, bi.Port, bi.Protocol, bi.ClusterName, loc)
}

// SelectBackends는 ServiceAffinity 기반 백엔드 선택
// 실제: pkg/clustermesh/selectbackends.go의 ClusterMeshSelectBackends
func (cm *ClusterMesh) SelectBackends(svcName, affinity string) []BackendInfo {
	gs := cm.globalSvcCache.GetGlobalService(svcName)
	if gs == nil {
		return nil
	}

	var localBe, remoteBe []BackendInfo
	for cluster, svc := range gs.ClusterServices {
		isLocal := cluster == cm.localCluster.Name
		for addr, portCfg := range svc.Backends {
			for portName, l4 := range portCfg {
				bi := BackendInfo{
					Address: addr, Port: l4.Port, Protocol: l4.Protocol,
					PortName: portName, ClusterName: cluster,
					ClusterID: svc.ClusterID, IsLocal: isLocal,
				}
				if isLocal {
					localBe = append(localBe, bi)
				} else {
					remoteBe = append(remoteBe, bi)
				}
			}
		}
	}

	switch affinity {
	case "local":
		if len(localBe) > 0 {
			return localBe
		}
		return remoteBe
	case "remote":
		if len(remoteBe) > 0 {
			return remoteBe
		}
		return localBe
	default:
		return append(localBe, remoteBe...)
	}
}

// =============================================================================
// 7. KVStoreMesh — pkg/clustermesh/kvstoremesh/kvstoremesh.go 재현
// =============================================================================

// KVStoreMesh는 원격 KVStore를 로컬에 캐싱하는 프록시
// 실제: kvstoremesh.go — 원격 etcd 감시 → 로컬 etcd에 미러링
// cilium/state/ → cilium/cache/ 접두사 변환
type KVStoreMesh struct {
	localKV   *KVStore
	remoteKVs map[string]*KVStore
	prefixes  []string
}

func NewKVStoreMesh(localKV *KVStore) *KVStoreMesh {
	return &KVStoreMesh{
		localKV:   localKV,
		remoteKVs: make(map[string]*KVStore),
		prefixes: []string{
			"cilium/state/services/v1/",
			"cilium/state/identities/v1/",
			"cilium/state/nodes/v1/",
		},
	}
}

func (km *KVStoreMesh) AddRemote(name string, kv *KVStore) {
	km.remoteKVs[name] = kv
}

// SyncAll은 모든 원격 데이터를 로컬로 동기화
func (km *KVStoreMesh) SyncAll() map[string]int {
	counts := make(map[string]int)
	for name, remote := range km.remoteKVs {
		total := 0
		for _, prefix := range km.prefixes {
			for key, value := range remote.ListPrefix(prefix) {
				cachedKey := strings.Replace(key, "cilium/state/", "cilium/cache/", 1)
				km.localKV.Put(cachedKey, value)
				total++
			}
		}
		counts[name] = total
	}
	return counts
}

// =============================================================================
// 8. 헬퍼 함수
// =============================================================================

func publishService(kv *KVStore, svc *ClusterService) {
	key := fmt.Sprintf("cilium/state/services/v1/%s/%s/%s", svc.Cluster, svc.Namespace, svc.Name)
	data, _ := json.Marshal(svc)
	kv.Put(key, data)
}

func publishIdentity(kv *KVStore, cluster string, entry *IdentityEntry) {
	key := fmt.Sprintf("cilium/state/identities/v1/%s/%d", cluster, entry.ID)
	data, _ := json.Marshal(entry)
	kv.Put(key, data)
}

func printSep(title string) {
	fmt.Printf("\n━━━ %s ━━━\n\n", title)
}

// =============================================================================
// 9. 데모 실행
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Cilium ClusterMesh 멀티클러스터 시뮬레이션                 ║")
	fmt.Println("║  소스: pkg/clustermesh/                                     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	// 환경: 3개 클러스터
	printSep("환경 설정: 3개 클러스터")
	kvC1 := NewKVStore()
	kvC2 := NewKVStore()
	kvC3 := NewKVStore()

	clusters := []ClusterInfo{
		{Name: "us-east", ID: 1},
		{Name: "eu-west", ID: 2},
		{Name: "ap-south", ID: 3},
	}
	for _, c := range clusters {
		fmt.Printf("  클러스터: %s (ID=%d)\n", c.Name, c.ID)
	}

	// =========================================================================
	// 데모 1: 서비스 게시
	// =========================================================================
	printSep("데모 1: 서비스 게시 (KVStore)")

	publishService(kvC1, &ClusterService{
		Cluster: "us-east", Namespace: "default", Name: "web-api",
		Backends: map[string]PortConfiguration{
			"10.1.1.1": {"http": {Protocol: "TCP", Port: 8080}},
			"10.1.1.2": {"http": {Protocol: "TCP", Port: 8080}},
		},
		Labels: map[string]string{"app": "web-api"}, Shared: true,
		IncludeExternal: true, ClusterID: 1,
	})
	publishService(kvC1, &ClusterService{
		Cluster: "us-east", Namespace: "default", Name: "database",
		Backends: map[string]PortConfiguration{
			"10.1.2.1": {"mysql": {Protocol: "TCP", Port: 3306}},
		},
		Labels: map[string]string{"app": "database"}, Shared: false, ClusterID: 1,
	})
	publishService(kvC2, &ClusterService{
		Cluster: "eu-west", Namespace: "default", Name: "web-api",
		Backends: map[string]PortConfiguration{
			"10.2.1.1": {"http": {Protocol: "TCP", Port: 8080}},
			"10.2.1.2": {"http": {Protocol: "TCP", Port: 8080}},
			"10.2.1.3": {"http": {Protocol: "TCP", Port: 8080}},
		},
		Labels: map[string]string{"app": "web-api"}, Shared: true,
		IncludeExternal: true, ClusterID: 2,
	})
	publishService(kvC3, &ClusterService{
		Cluster: "ap-south", Namespace: "default", Name: "web-api",
		Backends: map[string]PortConfiguration{
			"10.3.1.1": {"http": {Protocol: "TCP", Port: 8080}},
		},
		Labels: map[string]string{"app": "web-api"}, Shared: true,
		IncludeExternal: true, ClusterID: 3,
	})

	fmt.Printf("  us-east: %d키, eu-west: %d키, ap-south: %d키\n",
		kvC1.Size(), kvC2.Size(), kvC3.Size())
	fmt.Println("  주의: database (Shared=false) → 멀티클러스터에서 제외")

	// =========================================================================
	// 데모 2: Identity 게시
	// =========================================================================
	printSep("데모 2: Identity 동기화")
	publishIdentity(kvC1, "us-east", &IdentityEntry{ID: 10001, Labels: map[string]string{"app": "web-api"}, Cluster: "us-east"})
	publishIdentity(kvC2, "eu-west", &IdentityEntry{ID: 20001, Labels: map[string]string{"app": "web-api"}, Cluster: "eu-west"})
	publishIdentity(kvC3, "ap-south", &IdentityEntry{ID: 30001, Labels: map[string]string{"app": "web-api"}, Cluster: "ap-south"})
	fmt.Println("  us-east ID=10001, eu-west ID=20001, ap-south ID=30001")

	// =========================================================================
	// 데모 3: KVStoreMesh 동기화
	// =========================================================================
	printSep("데모 3: KVStoreMesh 동기화 (캐싱 프록시)")
	localKV := NewKVStore()
	kvMesh := NewKVStoreMesh(localKV)
	kvMesh.AddRemote("us-east", kvC1)
	kvMesh.AddRemote("eu-west", kvC2)
	kvMesh.AddRemote("ap-south", kvC3)

	fmt.Println("  미러링: cilium/state/ → cilium/cache/")
	counts := kvMesh.SyncAll()
	for name, count := range counts {
		fmt.Printf("    %s: %d개 키\n", name, count)
	}
	fmt.Printf("  로컬 캐시: %d개 키\n", localKV.Size())

	allKeys := localKV.ListPrefix("cilium/cache/")
	sortedKeys := make([]string, 0, len(allKeys))
	for k := range allKeys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	fmt.Println("\n  캐시 키:")
	for _, k := range sortedKeys {
		fmt.Printf("    %s\n", k)
	}

	// =========================================================================
	// 데모 4: ClusterMesh 서비스 디스커버리
	// =========================================================================
	printSep("데모 4: ClusterMesh 서비스 디스커버리")
	cm := NewClusterMesh(ClusterInfo{Name: "us-east", ID: 1})
	rc2 := cm.AddRemoteCluster("eu-west", 2, kvC2)
	rc3 := cm.AddRemoteCluster("ap-south", 3, kvC3)

	cm.globalSvcCache.OnUpdate(&ClusterService{
		Cluster: "us-east", Namespace: "default", Name: "web-api",
		Backends: map[string]PortConfiguration{
			"10.1.1.1": {"http": {Protocol: "TCP", Port: 8080}},
			"10.1.1.2": {"http": {Protocol: "TCP", Port: 8080}},
		},
		Shared: true, ClusterID: 1,
	})

	rc2.Connect()
	rc3.Connect()
	s2 := rc2.SyncServices()
	s3 := rc3.SyncServices()
	i2 := rc2.SyncIdentities()
	i3 := rc3.SyncIdentities()
	fmt.Printf("  eu-west: %d svc, %d id | ap-south: %d svc, %d id\n", s2, i2, s3, i3)
	fmt.Printf("  글로벌 서비스: %d개\n", cm.globalSvcCache.Size())

	for name, gs := range cm.globalSvcCache.AllServices() {
		fmt.Printf("\n  서비스: %s\n", name)
		for cluster, svc := range gs.ClusterServices {
			addrs := make([]string, 0)
			for addr := range svc.Backends {
				addrs = append(addrs, addr)
			}
			fmt.Printf("    %s (ID=%d): %v\n", cluster, svc.ClusterID, addrs)
		}
	}

	// =========================================================================
	// 데모 5: Cluster-aware 라우팅 (ServiceAffinity)
	// =========================================================================
	printSep("데모 5: Cluster-aware 라우팅 (ServiceAffinity)")

	fmt.Println("  affinity='none' (모든 백엔드):")
	for _, be := range cm.SelectBackends("default/web-api", "none") {
		fmt.Printf("    %s\n", be)
	}

	fmt.Println("\n  affinity='local' (로컬 우선):")
	for _, be := range cm.SelectBackends("default/web-api", "local") {
		fmt.Printf("    %s\n", be)
	}

	fmt.Println("\n  affinity='remote' (리모트 우선):")
	for _, be := range cm.SelectBackends("default/web-api", "remote") {
		fmt.Printf("    %s\n", be)
	}

	// 로컬 다운 → 폴백
	fmt.Println("\n  [시뮬레이션] 로컬 백엔드 다운 → 폴백:")
	cm.globalSvcCache.OnDelete(&ClusterService{
		Cluster: "us-east", Namespace: "default", Name: "web-api",
	})
	fallback := cm.SelectBackends("default/web-api", "local")
	fmt.Printf("    폴백 백엔드 (%d개):\n", len(fallback))
	for _, be := range fallback {
		fmt.Printf("      %s\n", be)
	}
	// 복원
	cm.globalSvcCache.OnUpdate(&ClusterService{
		Cluster: "us-east", Namespace: "default", Name: "web-api",
		Backends: map[string]PortConfiguration{
			"10.1.1.1": {"http": {Protocol: "TCP", Port: 8080}},
			"10.1.1.2": {"http": {Protocol: "TCP", Port: 8080}},
		},
		Shared: true, ClusterID: 1,
	})

	// =========================================================================
	// 데모 6: 동적 업데이트
	// =========================================================================
	printSep("데모 6: 동적 업데이트")

	fmt.Println("  [이벤트] ap-south 서비스 제거")
	cm.globalSvcCache.OnDelete(&ClusterService{
		Cluster: "ap-south", Namespace: "default", Name: "web-api",
	})
	gs := cm.globalSvcCache.GetGlobalService("default/web-api")
	fmt.Printf("  남은 클러스터: %d개\n", len(gs.ClusterServices))

	fmt.Println("\n  [이벤트] eu-west 스케일 아웃")
	cm.globalSvcCache.OnUpdate(&ClusterService{
		Cluster: "eu-west", Namespace: "default", Name: "web-api",
		Backends: map[string]PortConfiguration{
			"10.2.1.1": {"http": {Protocol: "TCP", Port: 8080}},
			"10.2.1.2": {"http": {Protocol: "TCP", Port: 8080}},
			"10.2.1.3": {"http": {Protocol: "TCP", Port: 8080}},
			"10.2.1.4": {"http": {Protocol: "TCP", Port: 8080}},
			"10.2.1.5": {"http": {Protocol: "TCP", Port: 8080}},
		},
		Shared: true, IncludeExternal: true, ClusterID: 2,
	})
	all := cm.SelectBackends("default/web-api", "none")
	fmt.Printf("  전체 백엔드: %d개\n", len(all))
	for _, be := range all {
		fmt.Printf("    %s\n", be)
	}

	// =========================================================================
	// 데모 7: 부하 분산 시뮬레이션
	// =========================================================================
	printSep("데모 7: 멀티클러스터 부하 분산")

	fmt.Println("  1000 요청 (affinity=none):")
	bes := cm.SelectBackends("default/web-api", "none")
	dist := make(map[string]int)
	for i := 0; i < 1000; i++ {
		be := bes[rand.Intn(len(bes))]
		dist[be.ClusterName]++
	}
	for c, n := range dist {
		bar := strings.Repeat("█", n/20)
		fmt.Printf("    %-10s: %4d %s\n", c, n, bar)
	}

	fmt.Println("\n  1000 요청 (affinity=local):")
	lbes := cm.SelectBackends("default/web-api", "local")
	dist2 := make(map[string]int)
	for i := 0; i < 1000; i++ {
		be := lbes[rand.Intn(len(lbes))]
		dist2[be.ClusterName]++
	}
	for c, n := range dist2 {
		bar := strings.Repeat("█", n/20)
		fmt.Printf("    %-10s: %4d %s\n", c, n, bar)
	}

	// =========================================================================
	// 요약
	// =========================================================================
	printSep("요약")

	fmt.Println("  ClusterMesh 아키텍처:")
	fmt.Println("    ┌──────────┐   KVStore 동기화   ┌──────────┐")
	fmt.Println("    │Cluster A │ ◄────────────────► │Cluster B │")
	fmt.Println("    │  (etcd)  │                    │  (etcd)  │")
	fmt.Println("    └────┬─────┘                    └────┬─────┘")
	fmt.Println("         │                               │")
	fmt.Println("    ┌────┴─────┐                    ┌────┴─────┐")
	fmt.Println("    │KVStoreMesh│                    │KVStoreMesh│")
	fmt.Println("    │(캐시)    │                    │(캐시)    │")
	fmt.Println("    └────┬─────┘                    └────┬─────┘")
	fmt.Println("         │                               │")
	fmt.Println("    ┌────┴─────┐                    ┌────┴─────┐")
	fmt.Println("    │  Agent   │                    │  Agent   │")
	fmt.Println("    │GlobalSvc │                    │GlobalSvc │")
	fmt.Println("    └──────────┘                    └──────────┘")
	fmt.Println()
	fmt.Println("  동기화 대상:")
	fmt.Println("    1. Services (cilium/state/services/v1/) — Shared=true만")
	fmt.Println("    2. Identities (cilium/state/identities/v1/) — 보안 정책용")
	fmt.Println("    3. Nodes (cilium/state/nodes/v1/) — 터널링용")
	fmt.Println()
	fmt.Println("  ServiceAffinity (pkg/clustermesh/selectbackends.go):")
	fmt.Println("    none:   모든 클러스터 백엔드")
	fmt.Println("    local:  로컬 우선 → 없으면 리모트 폴백")
	fmt.Println("    remote: 리모트 우선 → 없으면 로컬 폴백")
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════")

	_ = time.Now()
}
