// poc-17-crd-k8s: Cilium CRD/Kubernetes 통합 서브시스템 시뮬레이션
//
// 이 프로그램은 Cilium이 Kubernetes CRD와 통합하는 핵심 메커니즘을 순수 Go로
// 시뮬레이션한다. 외부 의존성 없이 다음을 구현한다:
//
// 1. K8s Informer/Watcher 패턴: Watch -> Event -> Handler -> Queue -> Process
// 2. CRD 생명주기: Create -> Validate -> Store -> Reconcile -> Update Status
// 3. 다중 CRD 타입과 관계: CNP -> Policy, CEP -> Endpoint, CID -> Identity
// 4. 컨트롤러 Reconciliation 루프 (에러 시 requeue)
// 5. ResourceVersion을 사용한 Optimistic Concurrency Control
//
// 참조 소스:
//   - pkg/k8s/apis/cilium.io/v2/types.go        (CRD 타입 정의)
//   - pkg/k8s/apis/cilium.io/v2/cnp_types.go    (CNP 타입)
//   - pkg/k8s/apis/cilium.io/v2/register.go     (CRD 등록)
//   - pkg/k8s/resource/resource.go               (Resource[T] 추상화)
//   - pkg/k8s/resource_ctors.go                  (Resource 생성자)
//   - pkg/k8s/watchers/watcher.go                (K8sWatcher)
//   - pkg/k8s/watchers/cilium_endpoint.go        (CEP Watcher)
//   - pkg/k8s/informer/informer.go               (커스텀 Informer)
//   - operator/watchers/cilium_endpoint.go        (Operator CEP)
//   - operator/identitygc/gc.go                   (Identity GC)
//   - operator/endpointgc/gc.go                   (Endpoint GC)
//   - operator/pkg/controller-runtime/cell.go     (controller-runtime 통합)

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 1. K8s 기본 타입 시뮬레이션
//    참조: k8s.io/apimachinery/pkg/apis/meta/v1
// ============================================================================

// ObjectMeta는 K8s ObjectMeta를 시뮬레이션한다.
type ObjectMeta struct {
	Name            string
	Namespace       string
	UID             string
	ResourceVersion string
	Labels          map[string]string
	Annotations     map[string]string
	CreationTime    time.Time
	OwnerRefs       []OwnerReference
}

// OwnerReference는 소유자 참조를 나타낸다.
type OwnerReference struct {
	Kind string
	Name string
	UID  string
}

// TypeMeta는 K8s TypeMeta를 시뮬레이션한다.
type TypeMeta struct {
	Kind       string
	APIVersion string
}

// ConditionStatus는 조건 상태를 나타낸다.
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition은 K8s 스타일 조건이다.
type Condition struct {
	Type               string
	Status             ConditionStatus
	LastTransitionTime time.Time
	Reason             string
	Message            string
}

// ============================================================================
// 2. CRD 타입 정의
//    참조: pkg/k8s/apis/cilium.io/v2/types.go
//          pkg/k8s/apis/cilium.io/v2/cnp_types.go
// ============================================================================

// CRDObject는 모든 CRD가 구현해야 하는 인터페이스다.
type CRDObject interface {
	GetTypeMeta() TypeMeta
	GetObjectMeta() ObjectMeta
	SetResourceVersion(rv string)
	GetResourceVersion() string
	GetKey() string
	DeepCopy() CRDObject
}

// --- CiliumNetworkPolicy (CNP) ---
// 참조: pkg/k8s/apis/cilium.io/v2/cnp_types.go

// PolicyRule은 네트워크 정책 규칙을 나타낸다.
type PolicyRule struct {
	Description     string
	EndpointSelector map[string]string // 레이블 셀렉터
	IngressRules    []IngressRule
	EgressRules     []EgressRule
}

// IngressRule은 인그레스 규칙이다.
type IngressRule struct {
	FromEndpoints []map[string]string // 소스 엔드포인트 셀렉터
	ToPorts       []PortRule
}

// EgressRule은 이그레스 규칙이다.
type EgressRule struct {
	ToEndpoints []map[string]string // 대상 엔드포인트 셀렉터
	ToPorts     []PortRule
	ToCIDRs     []string
}

// PortRule은 포트 규칙이다.
type PortRule struct {
	Port     int
	Protocol string
}

// CNPNodeStatus는 노드별 CNP 적용 상태이다.
type CNPNodeStatus struct {
	OK        bool
	Error     string
	Enforcing bool
	Revision  uint64
}

// CiliumNetworkPolicy는 CNP CRD를 나타낸다.
type CiliumNetworkPolicy struct {
	TypeMeta   TypeMeta
	ObjectMeta ObjectMeta
	Spec       *PolicyRule
	Specs      []*PolicyRule
	Status     struct {
		Conditions         []Condition
		DerivativePolicies map[string]CNPNodeStatus
	}
}

func (c *CiliumNetworkPolicy) GetTypeMeta() TypeMeta       { return c.TypeMeta }
func (c *CiliumNetworkPolicy) GetObjectMeta() ObjectMeta   { return c.ObjectMeta }
func (c *CiliumNetworkPolicy) SetResourceVersion(rv string) { c.ObjectMeta.ResourceVersion = rv }
func (c *CiliumNetworkPolicy) GetResourceVersion() string  { return c.ObjectMeta.ResourceVersion }
func (c *CiliumNetworkPolicy) GetKey() string {
	if c.ObjectMeta.Namespace != "" {
		return c.ObjectMeta.Namespace + "/" + c.ObjectMeta.Name
	}
	return c.ObjectMeta.Name
}
func (c *CiliumNetworkPolicy) DeepCopy() CRDObject {
	cp := *c
	if c.Spec != nil {
		specCopy := *c.Spec
		cp.Spec = &specCopy
	}
	return &cp
}

// Parse는 CNP를 내부 정책 규칙으로 변환한다.
// 참조: pkg/k8s/apis/cilium.io/v2/cnp_types.go Parse()
func (c *CiliumNetworkPolicy) Parse() ([]*PolicyRule, error) {
	if c.ObjectMeta.Name == "" {
		return nil, fmt.Errorf("CiliumNetworkPolicy must have name")
	}
	if c.Spec == nil && len(c.Specs) == 0 {
		return nil, fmt.Errorf("empty CNP: spec and specs cannot both be empty")
	}

	var rules []*PolicyRule
	if c.Spec != nil {
		if err := validatePolicyRule(c.Spec); err != nil {
			return nil, fmt.Errorf("invalid spec: %w", err)
		}
		rules = append(rules, c.Spec)
	}
	for _, r := range c.Specs {
		if err := validatePolicyRule(r); err != nil {
			return nil, fmt.Errorf("invalid specs: %w", err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func validatePolicyRule(r *PolicyRule) error {
	if r.EndpointSelector == nil {
		return fmt.Errorf("endpointSelector is required")
	}
	return nil
}

// --- CiliumEndpoint (CEP) ---
// 참조: pkg/k8s/apis/cilium.io/v2/types.go CiliumEndpoint

// EndpointIdentity는 엔드포인트의 보안 Identity이다.
type EndpointIdentity struct {
	ID     int64
	Labels []string
}

// AddressPair는 IPv4/IPv6 주소 쌍이다.
type AddressPair struct {
	IPv4 string
	IPv6 string
}

// EndpointNetworking은 엔드포인트의 네트워크 정보이다.
type EndpointNetworking struct {
	Addressing []AddressPair
	NodeIP     string
}

// EndpointPolicyDirection은 정책 적용 방향이다.
type EndpointPolicyDirection struct {
	Enforcing bool
	State     string
}

// EndpointPolicy는 엔드포인트의 정책 상태이다.
type EndpointPolicy struct {
	Ingress *EndpointPolicyDirection
	Egress  *EndpointPolicyDirection
}

// EndpointStatus는 CEP의 상태이다.
type EndpointStatus struct {
	ID         int64
	Identity   *EndpointIdentity
	Networking *EndpointNetworking
	State      string
	Policy     *EndpointPolicy
	Encryption struct{ Key int }
}

// CiliumEndpoint는 CEP CRD를 나타낸다.
type CiliumEndpoint struct {
	TypeMeta   TypeMeta
	ObjectMeta ObjectMeta
	Status     EndpointStatus
}

func (c *CiliumEndpoint) GetTypeMeta() TypeMeta       { return c.TypeMeta }
func (c *CiliumEndpoint) GetObjectMeta() ObjectMeta   { return c.ObjectMeta }
func (c *CiliumEndpoint) SetResourceVersion(rv string) { c.ObjectMeta.ResourceVersion = rv }
func (c *CiliumEndpoint) GetResourceVersion() string  { return c.ObjectMeta.ResourceVersion }
func (c *CiliumEndpoint) GetKey() string {
	if c.ObjectMeta.Namespace != "" {
		return c.ObjectMeta.Namespace + "/" + c.ObjectMeta.Name
	}
	return c.ObjectMeta.Name
}
func (c *CiliumEndpoint) DeepCopy() CRDObject {
	cp := *c
	return &cp
}

// --- CiliumIdentity (CID) ---
// 참조: pkg/k8s/apis/cilium.io/v2/types.go CiliumIdentity

// CiliumIdentity는 CID CRD를 나타낸다.
type CiliumIdentity struct {
	TypeMeta       TypeMeta
	ObjectMeta     ObjectMeta
	SecurityLabels map[string]string
}

func (c *CiliumIdentity) GetTypeMeta() TypeMeta       { return c.TypeMeta }
func (c *CiliumIdentity) GetObjectMeta() ObjectMeta   { return c.ObjectMeta }
func (c *CiliumIdentity) SetResourceVersion(rv string) { c.ObjectMeta.ResourceVersion = rv }
func (c *CiliumIdentity) GetResourceVersion() string  { return c.ObjectMeta.ResourceVersion }
func (c *CiliumIdentity) GetKey() string              { return c.ObjectMeta.Name }
func (c *CiliumIdentity) DeepCopy() CRDObject {
	cp := *c
	if c.SecurityLabels != nil {
		cp.SecurityLabels = make(map[string]string)
		for k, v := range c.SecurityLabels {
			cp.SecurityLabels[k] = v
		}
	}
	return &cp
}

// --- CiliumNode (CN) ---
// 참조: pkg/k8s/apis/cilium.io/v2/types.go CiliumNode

// CiliumNode는 CN CRD를 나타낸다.
type CiliumNode struct {
	TypeMeta   TypeMeta
	ObjectMeta ObjectMeta
	Spec       struct {
		InstanceID string
		Addresses  []struct {
			Type string
			IP   string
		}
		IPAM struct {
			PodCIDRs []string
		}
	}
	Status struct {
		IPAM struct {
			Used      map[string]string
			Available map[string]string
		}
	}
}

func (c *CiliumNode) GetTypeMeta() TypeMeta       { return c.TypeMeta }
func (c *CiliumNode) GetObjectMeta() ObjectMeta   { return c.ObjectMeta }
func (c *CiliumNode) SetResourceVersion(rv string) { c.ObjectMeta.ResourceVersion = rv }
func (c *CiliumNode) GetResourceVersion() string  { return c.ObjectMeta.ResourceVersion }
func (c *CiliumNode) GetKey() string              { return c.ObjectMeta.Name }
func (c *CiliumNode) DeepCopy() CRDObject {
	cp := *c
	return &cp
}

// ============================================================================
// 3. CRD 등록 (Scheme)
//    참조: pkg/k8s/apis/cilium.io/v2/register.go
// ============================================================================

// CRDScheme는 K8s runtime.Scheme을 시뮬레이션한다.
type CRDScheme struct {
	mu        sync.RWMutex
	knownTypes map[string]CRDTypeInfo
}

// CRDTypeInfo는 CRD 타입 메타데이터이다.
type CRDTypeInfo struct {
	Kind       string
	Plural     string
	ShortNames []string
	Scope      string // "Namespaced" 또는 "Cluster"
	Group      string
	Version    string
	Categories []string
}

// NewCRDScheme은 새 Scheme을 생성한다.
func NewCRDScheme() *CRDScheme {
	return &CRDScheme{
		knownTypes: make(map[string]CRDTypeInfo),
	}
}

// Register는 CRD 타입을 등록한다.
func (s *CRDScheme) Register(info CRDTypeInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fullName := info.Plural + "." + info.Group
	s.knownTypes[fullName] = info
	fmt.Printf("  [Scheme] CRD 등록: %s (Kind=%s, Scope=%s, ShortNames=%v)\n",
		fullName, info.Kind, info.Scope, info.ShortNames)
}

// IsRegistered는 CRD가 등록되었는지 확인한다.
func (s *CRDScheme) IsRegistered(fullName string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.knownTypes[fullName]
	return ok
}

// GetRegisteredTypes는 등록된 모든 타입을 반환한다.
func (s *CRDScheme) GetRegisteredTypes() []CRDTypeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	types := make([]CRDTypeInfo, 0, len(s.knownTypes))
	for _, t := range s.knownTypes {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Kind < types[j].Kind })
	return types
}

// registerCiliumCRDs는 Cilium의 모든 CRD를 등록한다.
// 참조: pkg/k8s/apis/cilium.io/v2/register.go addKnownTypes()
func registerCiliumCRDs(scheme *CRDScheme) {
	group := "cilium.io"
	v2 := "v2"
	v2a1 := "v2alpha1"

	// v2 CRD들
	crds := []CRDTypeInfo{
		{Kind: "CiliumNetworkPolicy", Plural: "ciliumnetworkpolicies", ShortNames: []string{"cnp", "ciliumnp"}, Scope: "Namespaced", Group: group, Version: v2, Categories: []string{"cilium", "ciliumpolicy"}},
		{Kind: "CiliumClusterwideNetworkPolicy", Plural: "ciliumclusterwidenetworkpolicies", ShortNames: []string{"ccnp"}, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium", "ciliumpolicy"}},
		{Kind: "CiliumEndpoint", Plural: "ciliumendpoints", ShortNames: []string{"cep", "ciliumep"}, Scope: "Namespaced", Group: group, Version: v2, Categories: []string{"cilium"}},
		{Kind: "CiliumIdentity", Plural: "ciliumidentities", ShortNames: []string{"ciliumid"}, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium"}},
		{Kind: "CiliumNode", Plural: "ciliumnodes", ShortNames: []string{"cn", "ciliumn"}, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium"}},
		{Kind: "CiliumNodeConfig", Plural: "ciliumnodeconfigs", ShortNames: []string{"cnc"}, Scope: "Namespaced", Group: group, Version: v2, Categories: []string{"cilium"}},
		{Kind: "CiliumEnvoyConfig", Plural: "ciliumenvoyconfigs", ShortNames: []string{"cec"}, Scope: "Namespaced", Group: group, Version: v2, Categories: []string{"cilium"}},
		{Kind: "CiliumClusterwideEnvoyConfig", Plural: "ciliumclusterwideenvoyconfigs", ShortNames: []string{"ccec"}, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium"}},
		{Kind: "CiliumLocalRedirectPolicy", Plural: "ciliumlocalredirectpolicies", ShortNames: []string{"clrp"}, Scope: "Namespaced", Group: group, Version: v2, Categories: []string{"cilium", "ciliumpolicy"}},
		{Kind: "CiliumEgressGatewayPolicy", Plural: "ciliumegressgatewaypolicies", ShortNames: []string{"cegp"}, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium", "ciliumpolicy"}},
		{Kind: "CiliumCIDRGroup", Plural: "ciliumcidrgroups", ShortNames: []string{"ccg"}, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium"}},
		{Kind: "CiliumLoadBalancerIPPool", Plural: "ciliumloadbalancerippools", ShortNames: []string{"lbippool"}, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium"}},
		{Kind: "CiliumBGPClusterConfig", Plural: "ciliumbgpclusterconfigs", ShortNames: []string{"cbgpcluster"}, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium", "ciliumbgp"}},
		{Kind: "CiliumBGPPeerConfig", Plural: "ciliumbgppeerconfigs", ShortNames: nil, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium", "ciliumbgp"}},
		{Kind: "CiliumBGPAdvertisement", Plural: "ciliumbgpadvertisements", ShortNames: nil, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium", "ciliumbgp"}},
		{Kind: "CiliumBGPNodeConfig", Plural: "ciliumbgpnodeconfigs", ShortNames: nil, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium", "ciliumbgp"}},
		{Kind: "CiliumBGPNodeConfigOverride", Plural: "ciliumbgpnodeconfigoverrides", ShortNames: nil, Scope: "Cluster", Group: group, Version: v2, Categories: []string{"cilium", "ciliumbgp"}},
	}

	// v2alpha1 CRD들
	alpha1CRDs := []CRDTypeInfo{
		{Kind: "CiliumEndpointSlice", Plural: "ciliumendpointslices", ShortNames: []string{"ces"}, Scope: "Cluster", Group: group, Version: v2a1, Categories: []string{"cilium"}},
		{Kind: "CiliumL2AnnouncementPolicy", Plural: "ciliuml2announcementpolicies", ShortNames: []string{"l2announcement"}, Scope: "Cluster", Group: group, Version: v2a1, Categories: []string{"cilium"}},
		{Kind: "CiliumPodIPPool", Plural: "ciliumpodippools", ShortNames: []string{"cpip"}, Scope: "Cluster", Group: group, Version: v2a1, Categories: []string{"cilium"}},
	}

	for _, crd := range append(crds, alpha1CRDs...) {
		scheme.Register(crd)
	}
}

// ============================================================================
// 4. API Server 시뮬레이션 (etcd 기반 저장소)
//    참조: k8s.io/apiserver (실제 K8s 구현)
// ============================================================================

// EventType은 Watch 이벤트 타입이다.
type EventType string

const (
	EventAdded    EventType = "ADDED"
	EventModified EventType = "MODIFIED"
	EventDeleted  EventType = "DELETED"
)

// WatchEvent는 Watch 이벤트이다.
type WatchEvent struct {
	Type   EventType
	Object CRDObject
}

// APIServer는 K8s API Server를 시뮬레이션한다.
type APIServer struct {
	mu              sync.RWMutex
	store           map[string]map[string]CRDObject // kind -> key -> object
	resourceVersion int64
	watchers        map[string][]chan WatchEvent // kind -> watcher channels
	scheme          *CRDScheme
}

// NewAPIServer는 새 API Server를 생성한다.
func NewAPIServer(scheme *CRDScheme) *APIServer {
	return &APIServer{
		store:    make(map[string]map[string]CRDObject),
		watchers: make(map[string][]chan WatchEvent),
		scheme:   scheme,
	}
}

// nextResourceVersion은 다음 ResourceVersion을 반환한다.
func (a *APIServer) nextResourceVersion() string {
	rv := atomic.AddInt64(&a.resourceVersion, 1)
	return fmt.Sprintf("%d", rv)
}

// Create는 새 객체를 생성한다 (Optimistic Concurrency).
func (a *APIServer) Create(obj CRDObject) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	kind := obj.GetTypeMeta().Kind
	key := obj.GetKey()

	if _, ok := a.store[kind]; !ok {
		a.store[kind] = make(map[string]CRDObject)
	}

	if _, exists := a.store[kind][key]; exists {
		return fmt.Errorf("AlreadyExists: %s %q already exists", kind, key)
	}

	rv := a.nextResourceVersion()
	obj.SetResourceVersion(rv)
	a.store[kind][key] = obj.DeepCopy()

	a.notifyWatchers(kind, WatchEvent{Type: EventAdded, Object: obj.DeepCopy()})
	return nil
}

// Update는 기존 객체를 업데이트한다 (Optimistic Concurrency).
func (a *APIServer) Update(obj CRDObject) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	kind := obj.GetTypeMeta().Kind
	key := obj.GetKey()

	if _, ok := a.store[kind]; !ok {
		return fmt.Errorf("NotFound: %s %q not found", kind, key)
	}

	existing, exists := a.store[kind][key]
	if !exists {
		return fmt.Errorf("NotFound: %s %q not found", kind, key)
	}

	// Optimistic Concurrency Control: ResourceVersion 체크
	if obj.GetResourceVersion() != "" && obj.GetResourceVersion() != existing.GetResourceVersion() {
		return fmt.Errorf("Conflict: the object %q has been modified (expected rv=%s, got rv=%s)",
			key, obj.GetResourceVersion(), existing.GetResourceVersion())
	}

	rv := a.nextResourceVersion()
	obj.SetResourceVersion(rv)
	a.store[kind][key] = obj.DeepCopy()

	a.notifyWatchers(kind, WatchEvent{Type: EventModified, Object: obj.DeepCopy()})
	return nil
}

// Delete는 객체를 삭제한다.
func (a *APIServer) Delete(kind, key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.store[kind]; !ok {
		return fmt.Errorf("NotFound: %s %q not found", kind, key)
	}

	obj, exists := a.store[kind][key]
	if !exists {
		return fmt.Errorf("NotFound: %s %q not found", kind, key)
	}

	delete(a.store[kind], key)
	a.notifyWatchers(kind, WatchEvent{Type: EventDeleted, Object: obj})
	return nil
}

// Get은 객체를 조회한다.
func (a *APIServer) Get(kind, key string) (CRDObject, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if _, ok := a.store[kind]; !ok {
		return nil, fmt.Errorf("NotFound: %s %q not found", kind, key)
	}

	obj, exists := a.store[kind][key]
	if !exists {
		return nil, fmt.Errorf("NotFound: %s %q not found", kind, key)
	}

	return obj.DeepCopy(), nil
}

// List는 특정 Kind의 모든 객체를 반환한다.
func (a *APIServer) List(kind string) []CRDObject {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var result []CRDObject
	if kindStore, ok := a.store[kind]; ok {
		for _, obj := range kindStore {
			result = append(result, obj.DeepCopy())
		}
	}
	return result
}

// Watch는 특정 Kind에 대한 Watch 채널을 반환한다.
func (a *APIServer) Watch(kind string) chan WatchEvent {
	a.mu.Lock()
	defer a.mu.Unlock()

	ch := make(chan WatchEvent, 100)
	a.watchers[kind] = append(a.watchers[kind], ch)

	// 현재 저장된 모든 객체를 ADDED 이벤트로 전송 (초기 List)
	if kindStore, ok := a.store[kind]; ok {
		for _, obj := range kindStore {
			ch <- WatchEvent{Type: EventAdded, Object: obj.DeepCopy()}
		}
	}

	return ch
}

// notifyWatchers는 Watcher들에게 이벤트를 전달한다.
func (a *APIServer) notifyWatchers(kind string, event WatchEvent) {
	for _, ch := range a.watchers[kind] {
		select {
		case ch <- event:
		default:
			// 채널이 가득 차면 드롭 (실제로는 backpressure 처리)
		}
	}
}

// ============================================================================
// 5. SharedInformer 시뮬레이션
//    참조: pkg/k8s/informer/informer.go
//          k8s.io/client-go/tools/cache
// ============================================================================

// Store는 로컬 캐시를 시뮬레이션한다.
type Store struct {
	mu      sync.RWMutex
	items   map[string]CRDObject
	indices map[string]map[string][]string // indexName -> indexValue -> keys
}

// NewStore는 새 Store를 생성한다.
func NewStore() *Store {
	return &Store{
		items:   make(map[string]CRDObject),
		indices: make(map[string]map[string][]string),
	}
}

// Add는 객체를 Store에 추가한다.
func (s *Store) Add(obj CRDObject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[obj.GetKey()] = obj.DeepCopy()
}

// Delete는 객체를 Store에서 제거한다.
func (s *Store) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, key)
}

// Get은 객체를 조회한다.
func (s *Store) Get(key string) (CRDObject, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.items[key]
	return obj, ok
}

// List는 모든 객체를 반환한다.
func (s *Store) List() []CRDObject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]CRDObject, 0, len(s.items))
	for _, obj := range s.items {
		result = append(result, obj)
	}
	return result
}

// AddIndexer는 인덱스 함수를 추가한다 (개선된 조회 지원).
// 참조: operator/watchers/cilium_endpoint.go identityIndexFunc
func (s *Store) AddIndex(indexName, indexValue, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.indices[indexName]; !ok {
		s.indices[indexName] = make(map[string][]string)
	}
	s.indices[indexName][indexValue] = append(s.indices[indexName][indexValue], key)
}

// GetByIndex는 인덱스로 객체를 조회한다.
func (s *Store) GetByIndex(indexName, indexValue string) []CRDObject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []CRDObject
	if idx, ok := s.indices[indexName]; ok {
		if keys, ok := idx[indexValue]; ok {
			for _, key := range keys {
				if obj, ok := s.items[key]; ok {
					result = append(result, obj)
				}
			}
		}
	}
	return result
}

// TransformFunc은 객체 변환 함수이다.
// 참조: operator/watchers/cilium_endpoint.go transformToCiliumEndpoint
type TransformFunc func(CRDObject) CRDObject

// SharedInformer는 client-go SharedInformer를 시뮬레이션한다.
type SharedInformer struct {
	kind       string
	apiServer  *APIServer
	store      *Store
	handlers   []ResourceEventHandler
	transform  TransformFunc
	synced     atomic.Bool
	stopCh     chan struct{}
}

// ResourceEventHandler는 이벤트 핸들러이다.
type ResourceEventHandler struct {
	OnAdd    func(obj CRDObject)
	OnUpdate func(oldObj, newObj CRDObject)
	OnDelete func(obj CRDObject)
}

// NewSharedInformer는 새 SharedInformer를 생성한다.
func NewSharedInformer(kind string, apiServer *APIServer, transform TransformFunc) *SharedInformer {
	return &SharedInformer{
		kind:      kind,
		apiServer: apiServer,
		store:     NewStore(),
		transform: transform,
		stopCh:    make(chan struct{}),
	}
}

// AddEventHandler는 이벤트 핸들러를 등록한다.
func (i *SharedInformer) AddEventHandler(handler ResourceEventHandler) {
	i.handlers = append(i.handlers, handler)
}

// HasSynced는 초기 동기화 완료 여부를 반환한다.
func (i *SharedInformer) HasSynced() bool {
	return i.synced.Load()
}

// Run은 Informer를 시작한다.
func (i *SharedInformer) Run() {
	// Watch 시작 (List + Watch)
	watchCh := i.apiServer.Watch(i.kind)

	// 이벤트 처리 goroutine
	go func() {
		for {
			select {
			case event := <-watchCh:
				obj := event.Object
				if i.transform != nil {
					obj = i.transform(obj)
				}

				switch event.Type {
				case EventAdded:
					i.store.Add(obj)
					for _, h := range i.handlers {
						if h.OnAdd != nil {
							h.OnAdd(obj)
						}
					}
				case EventModified:
					oldObj, _ := i.store.Get(obj.GetKey())
					i.store.Add(obj)
					for _, h := range i.handlers {
						if h.OnUpdate != nil {
							h.OnUpdate(oldObj, obj)
						}
					}
				case EventDeleted:
					i.store.Delete(obj.GetKey())
					for _, h := range i.handlers {
						if h.OnDelete != nil {
							h.OnDelete(obj)
						}
					}
				}
			case <-i.stopCh:
				return
			}
		}
	}()

	// 초기 동기화 완료 마킹 (약간의 지연 후)
	go func() {
		time.Sleep(50 * time.Millisecond)
		i.synced.Store(true)
	}()
}

// Stop은 Informer를 중지한다.
func (i *SharedInformer) Stop() {
	close(i.stopCh)
}

// ============================================================================
// 6. WorkQueue 시뮬레이션
//    참조: k8s.io/client-go/util/workqueue
// ============================================================================

// RateLimitingQueue는 Rate-Limiting WorkQueue를 시뮬레이션한다.
type RateLimitingQueue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	items      []string
	processing map[string]bool
	dirty      map[string]bool
	retries    map[string]int
	maxRetries int
	closed     bool
}

// NewRateLimitingQueue는 새 Queue를 생성한다.
func NewRateLimitingQueue(maxRetries int) *RateLimitingQueue {
	q := &RateLimitingQueue{
		processing: make(map[string]bool),
		dirty:      make(map[string]bool),
		retries:    make(map[string]int),
		maxRetries: maxRetries,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add는 아이템을 큐에 추가한다.
func (q *RateLimitingQueue) Add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return
	}

	// 이미 처리 중이면 dirty로 마킹
	if q.processing[key] {
		q.dirty[key] = true
		return
	}

	// 이미 큐에 있으면 스킵
	for _, item := range q.items {
		if item == key {
			return
		}
	}

	q.items = append(q.items, key)
	q.cond.Signal()
}

// Get은 큐에서 아이템을 가져온다 (블로킹).
func (q *RateLimitingQueue) Get() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}

	if q.closed && len(q.items) == 0 {
		return "", true
	}

	key := q.items[0]
	q.items = q.items[1:]
	q.processing[key] = true
	return key, false
}

// Done은 아이템 처리 완료를 표시한다.
func (q *RateLimitingQueue) Done(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.processing, key)

	if q.dirty[key] {
		delete(q.dirty, key)
		q.items = append(q.items, key)
		q.cond.Signal()
	}
}

// Requeue는 에러 시 아이템을 재큐잉한다 (재시도 제한).
func (q *RateLimitingQueue) Requeue(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.retries[key]++
	if q.retries[key] > q.maxRetries {
		fmt.Printf("  [Queue] 최대 재시도 초과 (%d/%d): %s\n", q.retries[key], q.maxRetries, key)
		delete(q.processing, key)
		delete(q.retries, key)
		return false
	}

	delete(q.processing, key)
	q.items = append(q.items, key)
	q.cond.Signal()
	return true
}

// Forget은 재시도 카운터를 초기화한다.
func (q *RateLimitingQueue) Forget(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.retries, key)
}

// ShutDown은 큐를 종료한다.
func (q *RateLimitingQueue) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

// Len은 큐의 크기를 반환한다.
func (q *RateLimitingQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// ============================================================================
// 7. Resource[T] 추상화 시뮬레이션
//    참조: pkg/k8s/resource/resource.go
// ============================================================================

// ResourceEventKind는 Resource 이벤트 종류이다.
type ResourceEventKind int

const (
	ResourceSync   ResourceEventKind = iota
	ResourceUpsert
	ResourceDelete
)

func (k ResourceEventKind) String() string {
	switch k {
	case ResourceSync:
		return "Sync"
	case ResourceUpsert:
		return "Upsert"
	case ResourceDelete:
		return "Delete"
	default:
		return "Unknown"
	}
}

// ResourceEvent는 Resource 이벤트이다.
type ResourceEvent struct {
	Kind   ResourceEventKind
	Key    string
	Object CRDObject
	done   chan error
}

// Done은 이벤트 처리 완료를 알린다.
// 참조: pkg/k8s/resource/resource.go - Done() must be called
func (e *ResourceEvent) Done(err error) {
	if e.done != nil {
		e.done <- err
		close(e.done)
	}
}

// Resource는 Cilium의 Resource[T] 추상화를 시뮬레이션한다.
type Resource struct {
	kind      string
	informer  *SharedInformer
	eventCh   chan ResourceEvent
	transform TransformFunc
}

// NewResource는 새 Resource를 생성한다.
func NewResource(kind string, apiServer *APIServer, transform TransformFunc) *Resource {
	informer := NewSharedInformer(kind, apiServer, transform)
	r := &Resource{
		kind:      kind,
		informer:  informer,
		eventCh:   make(chan ResourceEvent, 100),
		transform: transform,
	}

	// Informer 이벤트를 Resource 이벤트로 변환
	informer.AddEventHandler(ResourceEventHandler{
		OnAdd: func(obj CRDObject) {
			r.eventCh <- ResourceEvent{
				Kind:   ResourceUpsert,
				Key:    obj.GetKey(),
				Object: obj,
				done:   make(chan error, 1),
			}
		},
		OnUpdate: func(_, newObj CRDObject) {
			r.eventCh <- ResourceEvent{
				Kind:   ResourceUpsert,
				Key:    newObj.GetKey(),
				Object: newObj,
				done:   make(chan error, 1),
			}
		},
		OnDelete: func(obj CRDObject) {
			r.eventCh <- ResourceEvent{
				Kind:   ResourceDelete,
				Key:    obj.GetKey(),
				Object: obj,
				done:   make(chan error, 1),
			}
		},
	})

	return r
}

// Events는 이벤트 채널을 반환한다.
func (r *Resource) Events() <-chan ResourceEvent {
	return r.eventCh
}

// Store는 읽기 전용 Store를 반환한다.
func (r *Resource) Store() *Store {
	return r.informer.store
}

// Start는 Resource를 시작한다.
func (r *Resource) Start() {
	r.informer.Run()

	// 동기화 완료 후 Sync 이벤트 발행
	go func() {
		for !r.informer.HasSynced() {
			time.Sleep(10 * time.Millisecond)
		}
		r.eventCh <- ResourceEvent{
			Kind: ResourceSync,
			done: make(chan error, 1),
		}
	}()
}

// ============================================================================
// 8. 컨트롤러 (Reconciler) 시뮬레이션
//    참조: pkg/k8s/watchers/cilium_endpoint.go
//          operator/endpointgc/gc.go
// ============================================================================

// ReconcileResult는 Reconcile 함수의 결과이다.
type ReconcileResult struct {
	Requeue      bool
	RequeueAfter time.Duration
}

// ReconcileFunc은 Reconcile 함수 타입이다.
type ReconcileFunc func(key string) (ReconcileResult, error)

// Controller는 K8s 컨트롤러를 시뮬레이션한다.
type Controller struct {
	name      string
	queue     *RateLimitingQueue
	reconcile ReconcileFunc
	stopCh    chan struct{}
	workers   int
}

// NewController는 새 Controller를 생성한다.
func NewController(name string, workers int, reconcile ReconcileFunc) *Controller {
	return &Controller{
		name:      name,
		queue:     NewRateLimitingQueue(5),
		reconcile: reconcile,
		stopCh:    make(chan struct{}),
		workers:   workers,
	}
}

// Enqueue는 아이템을 큐에 추가한다.
func (c *Controller) Enqueue(key string) {
	c.queue.Add(key)
}

// Run은 Controller를 시작한다.
func (c *Controller) Run() {
	fmt.Printf("  [Controller:%s] %d worker 시작\n", c.name, c.workers)
	for i := 0; i < c.workers; i++ {
		go c.processLoop(i)
	}
}

// processLoop은 큐에서 아이템을 처리하는 루프이다.
func (c *Controller) processLoop(workerID int) {
	for {
		key, shutdown := c.queue.Get()
		if shutdown {
			return
		}

		result, err := c.reconcile(key)
		if err != nil {
			fmt.Printf("  [Controller:%s/w%d] Reconcile 에러 (key=%s): %v\n",
				c.name, workerID, key, err)
			if c.queue.Requeue(key) {
				fmt.Printf("  [Controller:%s/w%d] 재큐잉: %s\n", c.name, workerID, key)
			}
			c.queue.Done(key)
			time.Sleep(10 * time.Millisecond) // backoff 시뮬레이션
			continue
		}

		c.queue.Forget(key)
		c.queue.Done(key)

		if result.Requeue {
			fmt.Printf("  [Controller:%s/w%d] Reconcile 재큐잉 요청 (key=%s, after=%v)\n",
				c.name, workerID, key, result.RequeueAfter)
			if result.RequeueAfter > 0 {
				go func() {
					time.Sleep(result.RequeueAfter)
					c.queue.Add(key)
				}()
			} else {
				c.queue.Add(key)
			}
		}
	}
}

// Stop은 Controller를 종료한다.
func (c *Controller) Stop() {
	c.queue.ShutDown()
	close(c.stopCh)
}

// ============================================================================
// 9. 내부 데이터 구조 (정책 저장소, Identity 맵 등)
//    참조: pkg/policy/repository.go
//          pkg/identity/cache/
// ============================================================================

// PolicyRepository는 정책 저장소를 시뮬레이션한다.
type PolicyRepository struct {
	mu       sync.RWMutex
	policies map[string][]*PolicyRule // namespace/name -> rules
	revision uint64
}

// NewPolicyRepository는 새 PolicyRepository를 생성한다.
func NewPolicyRepository() *PolicyRepository {
	return &PolicyRepository{
		policies: make(map[string][]*PolicyRule),
	}
}

// Add는 정책을 추가한다.
func (p *PolicyRepository) Add(key string, rules []*PolicyRule) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policies[key] = rules
	p.revision++
	return p.revision
}

// Delete는 정책을 삭제한다.
func (p *PolicyRepository) Delete(key string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.policies, key)
	p.revision++
	return p.revision
}

// GetRevision은 현재 리비전을 반환한다.
func (p *PolicyRepository) GetRevision() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.revision
}

// IdentityAllocator는 Identity 할당기를 시뮬레이션한다.
type IdentityAllocator struct {
	mu         sync.Mutex
	nextID     int64
	identities map[string]int64 // labels-key -> identity ID
}

// NewIdentityAllocator는 새 IdentityAllocator를 생성한다.
func NewIdentityAllocator() *IdentityAllocator {
	return &IdentityAllocator{
		nextID:     1000,
		identities: make(map[string]int64),
	}
}

// Allocate는 레이블에 대한 Identity를 할당한다.
func (a *IdentityAllocator) Allocate(labels map[string]string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	// 레이블을 정렬된 키로 직렬화
	key := labelsToKey(labels)
	if id, ok := a.identities[key]; ok {
		return id
	}

	a.nextID++
	a.identities[key] = a.nextID
	return a.nextID
}

// Release는 Identity를 해제한다.
func (a *IdentityAllocator) Release(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.identities {
		if v == id {
			delete(a.identities, k)
			return
		}
	}
}

func labelsToKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// IPCache는 IP-Identity 매핑 캐시를 시뮬레이션한다.
type IPCache struct {
	mu    sync.RWMutex
	cache map[string]int64 // IP -> identity ID
}

// NewIPCache는 새 IPCache를 생성한다.
func NewIPCache() *IPCache {
	return &IPCache{
		cache: make(map[string]int64),
	}
}

// Upsert는 IP-Identity 매핑을 추가한다.
func (c *IPCache) Upsert(ip string, identityID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[ip] = identityID
}

// Delete는 IP-Identity 매핑을 삭제한다.
func (c *IPCache) Delete(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, ip)
}

// Lookup은 IP에 대한 Identity를 조회한다.
func (c *IPCache) Lookup(ip string) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.cache[ip]
	return id, ok
}

// ============================================================================
// 10. CiliumAgent 및 Operator 시뮬레이션
// ============================================================================

// CiliumAgent는 cilium-agent의 K8s 통합을 시뮬레이션한다.
type CiliumAgent struct {
	apiServer   *APIServer
	policyRepo  *PolicyRepository
	identityAlloc *IdentityAllocator
	ipCache     *IPCache

	cnpResource *Resource
	cepResource *Resource
	cidResource *Resource
	cnResource  *Resource

	cnpController *Controller
	cepController *Controller

	synced chan struct{}
}

// NewCiliumAgent는 새 CiliumAgent를 생성한다.
func NewCiliumAgent(apiServer *APIServer) *CiliumAgent {
	agent := &CiliumAgent{
		apiServer:     apiServer,
		policyRepo:    NewPolicyRepository(),
		identityAlloc: NewIdentityAllocator(),
		ipCache:       NewIPCache(),
		synced:        make(chan struct{}),
	}

	// Resource 생성 (LazyTransform 포함)
	// 참조: pkg/k8s/resource_ctors.go
	agent.cnpResource = NewResource("CiliumNetworkPolicy", apiServer, nil)
	agent.cepResource = NewResource("CiliumEndpoint", apiServer,
		// Transform: 최소 필드만 유지 (메모리 최적화)
		// 참조: operator/watchers/cilium_endpoint.go transformToCiliumEndpoint
		func(obj CRDObject) CRDObject {
			cep, ok := obj.(*CiliumEndpoint)
			if !ok {
				return obj
			}
			// 최소화된 CiliumEndpoint (Slim 버전)
			slim := &CiliumEndpoint{
				TypeMeta:   cep.TypeMeta,
				ObjectMeta: ObjectMeta{
					Name:            cep.ObjectMeta.Name,
					Namespace:       cep.ObjectMeta.Namespace,
					ResourceVersion: cep.ObjectMeta.ResourceVersion,
					UID:             cep.ObjectMeta.UID,
				},
				Status: EndpointStatus{
					Identity:   cep.Status.Identity,
					Networking: cep.Status.Networking,
					State:      cep.Status.State,
				},
			}
			return slim
		},
	)
	agent.cidResource = NewResource("CiliumIdentity", apiServer, nil)
	agent.cnResource = NewResource("CiliumNode", apiServer, nil)

	// CNP 컨트롤러 생성
	agent.cnpController = NewController("cnp-reconciler", 2, agent.reconcileCNP)
	// CEP 컨트롤러 생성
	agent.cepController = NewController("cep-reconciler", 2, agent.reconcileCEP)

	return agent
}

// Start는 Agent의 K8s 서브시스템을 시작한다.
// 참조: pkg/k8s/watchers/watcher.go InitK8sSubsystem()
func (a *CiliumAgent) Start() {
	fmt.Println("\n=== [Agent] K8s 서브시스템 시작 ===")

	// Resource 시작 (Informer 가동)
	a.cnpResource.Start()
	a.cepResource.Start()
	a.cidResource.Start()
	a.cnResource.Start()

	// Controller 시작
	a.cnpController.Run()
	a.cepController.Run()

	// CNP 이벤트 처리
	// 참조: pkg/k8s/watchers/cilium_endpoint.go ciliumEndpointsInit
	go func() {
		syncDone := false
		cache := make(map[string]CRDObject)

		for event := range a.cnpResource.Events() {
			switch event.Kind {
			case ResourceSync:
				syncDone = true
				fmt.Println("  [Agent/CNP] 초기 동기화 완료")
				event.Done(nil)
			case ResourceUpsert:
				oldObj := cache[event.Key]
				cache[event.Key] = event.Object
				if oldObj == nil {
					fmt.Printf("  [Agent/CNP] 새 정책 감지: %s\n", event.Key)
				} else {
					fmt.Printf("  [Agent/CNP] 정책 업데이트 감지: %s\n", event.Key)
				}
				a.cnpController.Enqueue(event.Key)
				event.Done(nil)
			case ResourceDelete:
				delete(cache, event.Key)
				fmt.Printf("  [Agent/CNP] 정책 삭제 감지: %s\n", event.Key)
				a.cnpController.Enqueue(event.Key)
				event.Done(nil)
			}
			_ = syncDone
		}
	}()

	// CEP 이벤트 처리
	go func() {
		cache := make(map[string]CRDObject)

		for event := range a.cepResource.Events() {
			switch event.Kind {
			case ResourceSync:
				fmt.Println("  [Agent/CEP] 초기 동기화 완료")
				event.Done(nil)
			case ResourceUpsert:
				cache[event.Key] = event.Object
				a.cepController.Enqueue(event.Key)
				event.Done(nil)
			case ResourceDelete:
				delete(cache, event.Key)
				a.cepController.Enqueue(event.Key)
				event.Done(nil)
			}
		}
	}()

	// 초기 캐시 동기화 완료 대기
	go func() {
		time.Sleep(100 * time.Millisecond) // 모든 Informer 동기화 대기
		close(a.synced)
		fmt.Println("  [Agent] 모든 캐시 동기화 완료")
	}()
}

// reconcileCNP는 CNP Reconcile 로직이다.
// 참조: pkg/policy/k8s/ (정책 k8s 통합)
func (a *CiliumAgent) reconcileCNP(key string) (ReconcileResult, error) {
	obj, exists := a.cnpResource.Store().Get(key)
	if !exists {
		// 삭제된 정책 처리
		rev := a.policyRepo.Delete(key)
		fmt.Printf("  [Agent/CNP-Reconcile] 정책 삭제됨: %s (revision=%d)\n", key, rev)
		return ReconcileResult{}, nil
	}

	cnp, ok := obj.(*CiliumNetworkPolicy)
	if !ok {
		return ReconcileResult{}, fmt.Errorf("unexpected object type for key %s", key)
	}

	// CNP 파싱 (CRD -> 내부 규칙)
	rules, err := cnp.Parse()
	if err != nil {
		fmt.Printf("  [Agent/CNP-Reconcile] 파싱 에러: %s - %v\n", key, err)
		return ReconcileResult{}, err
	}

	// PolicyRepository에 추가
	rev := a.policyRepo.Add(key, rules)
	fmt.Printf("  [Agent/CNP-Reconcile] 정책 적용: %s (%d rules, revision=%d)\n",
		key, len(rules), rev)

	return ReconcileResult{}, nil
}

// reconcileCEP는 CEP Reconcile 로직이다.
// 참조: pkg/k8s/watchers/cilium_endpoint.go endpointUpdated
func (a *CiliumAgent) reconcileCEP(key string) (ReconcileResult, error) {
	obj, exists := a.cepResource.Store().Get(key)
	if !exists {
		// 삭제된 엔드포인트 처리
		fmt.Printf("  [Agent/CEP-Reconcile] 엔드포인트 삭제됨: %s (IPCache 정리)\n", key)
		return ReconcileResult{}, nil
	}

	cep, ok := obj.(*CiliumEndpoint)
	if !ok {
		return ReconcileResult{}, fmt.Errorf("unexpected object type for key %s", key)
	}

	// Identity가 없으면 재큐잉
	if cep.Status.Identity == nil {
		return ReconcileResult{Requeue: true, RequeueAfter: 50 * time.Millisecond},
			nil
	}

	// Networking 정보가 없으면 재큐잉
	if cep.Status.Networking == nil || cep.Status.Networking.NodeIP == "" {
		fmt.Printf("  [Agent/CEP-Reconcile] NodeIP 없음, 재큐잉: %s\n", key)
		return ReconcileResult{Requeue: true, RequeueAfter: 50 * time.Millisecond},
			nil
	}

	// IPCache 업데이트
	identityID := cep.Status.Identity.ID
	for _, pair := range cep.Status.Networking.Addressing {
		if pair.IPv4 != "" {
			a.ipCache.Upsert(pair.IPv4, identityID)
			fmt.Printf("  [Agent/CEP-Reconcile] IPCache 업데이트: %s -> identity=%d\n",
				pair.IPv4, identityID)
		}
	}

	return ReconcileResult{}, nil
}

// CiliumOperator는 cilium-operator를 시뮬레이션한다.
type CiliumOperator struct {
	apiServer     *APIServer
	identityAlloc *IdentityAllocator

	endpointGCController *Controller
	identityGCController *Controller

	cepStore *Store
	cidStore *Store
}

// NewCiliumOperator는 새 CiliumOperator를 생성한다.
func NewCiliumOperator(apiServer *APIServer, identityAlloc *IdentityAllocator) *CiliumOperator {
	op := &CiliumOperator{
		apiServer:     apiServer,
		identityAlloc: identityAlloc,
		cepStore:      NewStore(),
		cidStore:      NewStore(),
	}

	// Endpoint GC 컨트롤러
	// 참조: operator/endpointgc/gc.go
	op.endpointGCController = NewController("endpoint-gc", 1, op.reconcileEndpointGC)

	// Identity GC 컨트롤러
	// 참조: operator/identitygc/gc.go
	op.identityGCController = NewController("identity-gc", 1, op.reconcileIdentityGC)

	return op
}

// Start는 Operator를 시작한다.
func (o *CiliumOperator) Start() {
	fmt.Println("\n=== [Operator] 시작 ===")

	// CEP 감시 시작
	cepInformer := NewSharedInformer("CiliumEndpoint", o.apiServer, nil)
	cepInformer.AddEventHandler(ResourceEventHandler{
		OnAdd: func(obj CRDObject) {
			o.cepStore.Add(obj)
		},
		OnUpdate: func(_, newObj CRDObject) {
			o.cepStore.Add(newObj)
		},
		OnDelete: func(obj CRDObject) {
			o.cepStore.Delete(obj.GetKey())
		},
	})
	cepInformer.Run()

	// CID 감시 시작
	cidInformer := NewSharedInformer("CiliumIdentity", o.apiServer, nil)
	cidInformer.AddEventHandler(ResourceEventHandler{
		OnAdd: func(obj CRDObject) {
			o.cidStore.Add(obj)
		},
		OnUpdate: func(_, newObj CRDObject) {
			o.cidStore.Add(newObj)
		},
		OnDelete: func(obj CRDObject) {
			o.cidStore.Delete(obj.GetKey())
		},
	})
	cidInformer.Run()

	// 컨트롤러 시작
	o.endpointGCController.Run()
	o.identityGCController.Run()

	fmt.Println("  [Operator] 모든 감시자 및 컨트롤러 시작됨")
}

// RunEndpointGC는 Endpoint GC 사이클을 실행한다.
func (o *CiliumOperator) RunEndpointGC() {
	fmt.Println("\n  [Operator/EndpointGC] GC 사이클 시작")
	for _, cep := range o.cepStore.List() {
		o.endpointGCController.Enqueue(cep.GetKey())
	}
}

// RunIdentityGC는 Identity GC 사이클을 실행한다.
func (o *CiliumOperator) RunIdentityGC() {
	fmt.Println("\n  [Operator/IdentityGC] GC 사이클 시작")
	for _, cid := range o.cidStore.List() {
		o.identityGCController.Enqueue(cid.GetKey())
	}
}

// reconcileEndpointGC는 Endpoint GC Reconcile 로직이다.
func (o *CiliumOperator) reconcileEndpointGC(key string) (ReconcileResult, error) {
	cep, exists := o.cepStore.Get(key)
	if !exists {
		return ReconcileResult{}, nil
	}

	ep := cep.(*CiliumEndpoint)
	// Pod 존재 여부 확인 (시뮬레이션: State가 "disconnected"이면 삭제 후보)
	if ep.Status.State == "disconnected" {
		fmt.Printf("  [Operator/EndpointGC] 고아 CEP 삭제: %s (state=%s)\n",
			key, ep.Status.State)
		o.apiServer.Delete("CiliumEndpoint", key)
	}

	return ReconcileResult{}, nil
}

// reconcileIdentityGC는 Identity GC Reconcile 로직이다.
func (o *CiliumOperator) reconcileIdentityGC(key string) (ReconcileResult, error) {
	cid, exists := o.cidStore.Get(key)
	if !exists {
		return ReconcileResult{}, nil
	}

	identity := cid.(*CiliumIdentity)
	identityIDStr := identity.ObjectMeta.Name

	// 이 Identity를 참조하는 CEP가 있는지 확인
	referenced := false
	for _, cep := range o.cepStore.List() {
		ep := cep.(*CiliumEndpoint)
		if ep.Status.Identity != nil && fmt.Sprintf("%d", ep.Status.Identity.ID) == identityIDStr {
			referenced = true
			break
		}
	}

	if !referenced {
		fmt.Printf("  [Operator/IdentityGC] 미참조 Identity 삭제: %s (labels=%v)\n",
			key, identity.SecurityLabels)
		o.apiServer.Delete("CiliumIdentity", key)
		o.identityAlloc.Release(0) // 시뮬레이션
	}

	return ReconcileResult{}, nil
}

// ============================================================================
// 11. 데모 시나리오 실행
// ============================================================================

func printSectionHeader(title string) {
	fmt.Printf("\n%s\n%s %s %s\n%s\n",
		strings.Repeat("=", 70),
		strings.Repeat("=", 5), title, strings.Repeat("=", 60-len(title)),
		strings.Repeat("=", 70))
}

func main() {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	fmt.Println("========================================================================")
	fmt.Println("  Cilium CRD/K8s 통합 서브시스템 시뮬레이션")
	fmt.Println("  (순수 Go 구현 - 외부 의존성 없음)")
	fmt.Println("========================================================================")

	// ------------------------------------------------------------------
	// Phase 1: CRD 등록 (Scheme)
	// ------------------------------------------------------------------
	printSectionHeader("Phase 1: CRD 스키마 등록")
	scheme := NewCRDScheme()
	registerCiliumCRDs(scheme)

	registeredTypes := scheme.GetRegisteredTypes()
	fmt.Printf("\n  총 %d개 CRD 등록 완료\n", len(registeredTypes))

	// ------------------------------------------------------------------
	// Phase 2: API Server 및 Agent/Operator 시작
	// ------------------------------------------------------------------
	printSectionHeader("Phase 2: API Server / Agent / Operator 시작")

	apiServer := NewAPIServer(scheme)
	fmt.Println("  [APIServer] K8s API Server 시뮬레이션 시작")

	agent := NewCiliumAgent(apiServer)
	agent.Start()

	operator := NewCiliumOperator(apiServer, agent.identityAlloc)
	operator.Start()

	// 동기화 대기
	time.Sleep(200 * time.Millisecond)

	// ------------------------------------------------------------------
	// Phase 3: CRD 생명주기 - CiliumIdentity 생성
	// ------------------------------------------------------------------
	printSectionHeader("Phase 3: CiliumIdentity 생성")

	identity1 := &CiliumIdentity{
		TypeMeta: TypeMeta{Kind: "CiliumIdentity", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:   "1001",
			Labels: map[string]string{"k8s:app": "web", "k8s:io.kubernetes.pod.namespace": "default"},
		},
		SecurityLabels: map[string]string{
			"k8s:app":                            "web",
			"k8s:io.kubernetes.pod.namespace":    "default",
			"k8s:io.cilium.k8s.policy.cluster":  "default",
		},
	}
	if err := apiServer.Create(identity1); err != nil {
		fmt.Printf("  Identity 생성 실패: %v\n", err)
	} else {
		fmt.Printf("  [APIServer] CiliumIdentity 생성: name=%s, rv=%s\n",
			identity1.ObjectMeta.Name, identity1.ObjectMeta.ResourceVersion)
	}

	identity2 := &CiliumIdentity{
		TypeMeta: TypeMeta{Kind: "CiliumIdentity", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:   "1002",
			Labels: map[string]string{"k8s:app": "api", "k8s:io.kubernetes.pod.namespace": "default"},
		},
		SecurityLabels: map[string]string{
			"k8s:app":                            "api",
			"k8s:io.kubernetes.pod.namespace":    "default",
		},
	}
	apiServer.Create(identity2)
	fmt.Printf("  [APIServer] CiliumIdentity 생성: name=%s, rv=%s\n",
		identity2.ObjectMeta.Name, identity2.ObjectMeta.ResourceVersion)

	// 미참조 Identity (GC 대상)
	identityOrphan := &CiliumIdentity{
		TypeMeta: TypeMeta{Kind: "CiliumIdentity", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:   "9999",
			Labels: map[string]string{"k8s:app": "deleted-app"},
		},
		SecurityLabels: map[string]string{
			"k8s:app": "deleted-app",
		},
	}
	apiServer.Create(identityOrphan)
	fmt.Printf("  [APIServer] CiliumIdentity 생성 (고아): name=%s\n",
		identityOrphan.ObjectMeta.Name)

	time.Sleep(100 * time.Millisecond)

	// ------------------------------------------------------------------
	// Phase 4: CRD 생명주기 - CiliumEndpoint 생성
	// ------------------------------------------------------------------
	printSectionHeader("Phase 4: CiliumEndpoint 생성 및 Status 업데이트")

	cep1 := &CiliumEndpoint{
		TypeMeta: TypeMeta{Kind: "CiliumEndpoint", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:      "web-pod-1",
			Namespace: "default",
			UID:       "uid-cep-1",
		},
		Status: EndpointStatus{
			ID:    100,
			State: "creating",
			Identity: &EndpointIdentity{
				ID:     1001,
				Labels: []string{"k8s:app=web"},
			},
			Networking: &EndpointNetworking{
				Addressing: []AddressPair{{IPv4: "10.0.1.10"}},
				NodeIP:     "192.168.1.1",
			},
		},
	}

	if err := apiServer.Create(cep1); err != nil {
		fmt.Printf("  CEP 생성 실패: %v\n", err)
	} else {
		fmt.Printf("  [APIServer] CiliumEndpoint 생성: %s/%s (state=%s, rv=%s)\n",
			cep1.ObjectMeta.Namespace, cep1.ObjectMeta.Name,
			cep1.Status.State, cep1.ObjectMeta.ResourceVersion)
	}

	time.Sleep(100 * time.Millisecond)

	// Status 업데이트: creating -> ready
	fmt.Println("\n  --- CEP Status 업데이트: creating -> ready ---")
	cep1Updated, _ := apiServer.Get("CiliumEndpoint", "default/web-pod-1")
	if cep1Updated != nil {
		cep1Typed := cep1Updated.(*CiliumEndpoint)
		cep1Typed.Status.State = "ready"
		cep1Typed.Status.Policy = &EndpointPolicy{
			Ingress: &EndpointPolicyDirection{Enforcing: true, State: "enforcing"},
			Egress:  &EndpointPolicyDirection{Enforcing: true, State: "enforcing"},
		}
		if err := apiServer.Update(cep1Typed); err != nil {
			fmt.Printf("  CEP 업데이트 실패: %v\n", err)
		} else {
			fmt.Printf("  [APIServer] CEP 업데이트: state=%s, rv=%s\n",
				cep1Typed.Status.State, cep1Typed.ObjectMeta.ResourceVersion)
		}
	}

	// 두 번째 CEP
	cep2 := &CiliumEndpoint{
		TypeMeta: TypeMeta{Kind: "CiliumEndpoint", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:      "api-pod-1",
			Namespace: "default",
			UID:       "uid-cep-2",
		},
		Status: EndpointStatus{
			ID:    101,
			State: "ready",
			Identity: &EndpointIdentity{
				ID:     1002,
				Labels: []string{"k8s:app=api"},
			},
			Networking: &EndpointNetworking{
				Addressing: []AddressPair{{IPv4: "10.0.1.20"}},
				NodeIP:     "192.168.1.1",
			},
		},
	}
	apiServer.Create(cep2)
	fmt.Printf("  [APIServer] CiliumEndpoint 생성: %s/%s (state=%s)\n",
		cep2.ObjectMeta.Namespace, cep2.ObjectMeta.Name, cep2.Status.State)

	// disconnected CEP (GC 대상)
	cep3 := &CiliumEndpoint{
		TypeMeta: TypeMeta{Kind: "CiliumEndpoint", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:      "orphan-pod",
			Namespace: "default",
			UID:       "uid-cep-3",
		},
		Status: EndpointStatus{
			ID:    102,
			State: "disconnected",
		},
	}
	apiServer.Create(cep3)
	fmt.Printf("  [APIServer] CiliumEndpoint 생성 (고아): %s/%s (state=%s)\n",
		cep3.ObjectMeta.Namespace, cep3.ObjectMeta.Name, cep3.Status.State)

	time.Sleep(200 * time.Millisecond)

	// ------------------------------------------------------------------
	// Phase 5: CRD 생명주기 - CiliumNetworkPolicy 생성
	// ------------------------------------------------------------------
	printSectionHeader("Phase 5: CiliumNetworkPolicy 생성 및 Reconcile")

	cnp1 := &CiliumNetworkPolicy{
		TypeMeta: TypeMeta{Kind: "CiliumNetworkPolicy", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:      "allow-web-ingress",
			Namespace: "default",
			UID:       "uid-cnp-1",
		},
		Spec: &PolicyRule{
			Description:     "Allow ingress to web pods from api pods on port 80",
			EndpointSelector: map[string]string{"app": "web"},
			IngressRules: []IngressRule{
				{
					FromEndpoints: []map[string]string{{"app": "api"}},
					ToPorts:       []PortRule{{Port: 80, Protocol: "TCP"}},
				},
			},
		},
	}

	if err := apiServer.Create(cnp1); err != nil {
		fmt.Printf("  CNP 생성 실패: %v\n", err)
	} else {
		fmt.Printf("  [APIServer] CiliumNetworkPolicy 생성: %s/%s (rv=%s)\n",
			cnp1.ObjectMeta.Namespace, cnp1.ObjectMeta.Name,
			cnp1.ObjectMeta.ResourceVersion)
	}

	// 두 번째 CNP (유효하지 않은 정책 - 에러 발생)
	cnpInvalid := &CiliumNetworkPolicy{
		TypeMeta: TypeMeta{Kind: "CiliumNetworkPolicy", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:      "invalid-policy",
			Namespace: "default",
			UID:       "uid-cnp-2",
		},
		Spec: &PolicyRule{
			Description:     "Invalid: no endpointSelector",
			EndpointSelector: nil, // 필수 필드 누락
		},
	}

	apiServer.Create(cnpInvalid)
	fmt.Printf("  [APIServer] CiliumNetworkPolicy 생성 (잘못된 정책): %s/%s\n",
		cnpInvalid.ObjectMeta.Namespace, cnpInvalid.ObjectMeta.Name)

	// 이그레스 규칙이 있는 CNP
	cnp2 := &CiliumNetworkPolicy{
		TypeMeta: TypeMeta{Kind: "CiliumNetworkPolicy", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name:      "allow-api-egress",
			Namespace: "default",
			UID:       "uid-cnp-3",
		},
		Spec: &PolicyRule{
			Description:     "Allow egress from api pods to external",
			EndpointSelector: map[string]string{"app": "api"},
			EgressRules: []EgressRule{
				{
					ToCIDRs: []string{"0.0.0.0/0"},
					ToPorts: []PortRule{{Port: 443, Protocol: "TCP"}},
				},
			},
		},
	}
	apiServer.Create(cnp2)
	fmt.Printf("  [APIServer] CiliumNetworkPolicy 생성: %s/%s\n",
		cnp2.ObjectMeta.Namespace, cnp2.ObjectMeta.Name)

	time.Sleep(300 * time.Millisecond)

	// ------------------------------------------------------------------
	// Phase 6: Optimistic Concurrency Control 시연
	// ------------------------------------------------------------------
	printSectionHeader("Phase 6: Optimistic Concurrency Control (OCC)")

	// 같은 객체를 두 번 업데이트 시도 (충돌)
	obj1, _ := apiServer.Get("CiliumNetworkPolicy", "default/allow-web-ingress")
	obj2, _ := apiServer.Get("CiliumNetworkPolicy", "default/allow-web-ingress")

	if obj1 != nil && obj2 != nil {
		cnpA := obj1.(*CiliumNetworkPolicy)
		cnpB := obj2.(*CiliumNetworkPolicy)

		fmt.Printf("  두 클라이언트가 동일 CNP 조회: rv=%s\n", cnpA.ObjectMeta.ResourceVersion)

		// 첫 번째 업데이트 성공
		cnpA.Spec.Description = "Updated by client A"
		err := apiServer.Update(cnpA)
		fmt.Printf("  Client A 업데이트: err=%v, new_rv=%s\n", err, cnpA.ObjectMeta.ResourceVersion)

		// 두 번째 업데이트 실패 (ResourceVersion 충돌)
		cnpB.Spec.Description = "Updated by client B"
		err = apiServer.Update(cnpB)
		fmt.Printf("  Client B 업데이트 (충돌 예상): err=%v\n", err)

		// 재시도: 최신 버전을 다시 가져와서 업데이트
		obj3, _ := apiServer.Get("CiliumNetworkPolicy", "default/allow-web-ingress")
		if obj3 != nil {
			cnpRetry := obj3.(*CiliumNetworkPolicy)
			cnpRetry.Spec.Description = "Updated by client B (retry)"
			err = apiServer.Update(cnpRetry)
			fmt.Printf("  Client B 재시도: err=%v, new_rv=%s\n", err, cnpRetry.ObjectMeta.ResourceVersion)
		}
	}

	time.Sleep(100 * time.Millisecond)

	// ------------------------------------------------------------------
	// Phase 7: CiliumNode 생성
	// ------------------------------------------------------------------
	printSectionHeader("Phase 7: CiliumNode 생성")

	cn1 := &CiliumNode{
		TypeMeta: TypeMeta{Kind: "CiliumNode", APIVersion: "cilium.io/v2"},
		ObjectMeta: ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				"kubernetes.io/hostname": "node-1",
				"topology.kubernetes.io/zone": "us-east-1a",
			},
		},
	}
	cn1.Spec.InstanceID = "i-0123456789abcdef0"
	cn1.Spec.Addresses = []struct {
		Type string
		IP   string
	}{
		{Type: "InternalIP", IP: "192.168.1.1"},
		{Type: "CiliumInternalIP", IP: "10.0.0.1"},
	}
	cn1.Spec.IPAM.PodCIDRs = []string{"10.0.1.0/24"}

	if err := apiServer.Create(cn1); err != nil {
		fmt.Printf("  CiliumNode 생성 실패: %v\n", err)
	} else {
		fmt.Printf("  [APIServer] CiliumNode 생성: %s (instanceID=%s, podCIDR=%v)\n",
			cn1.ObjectMeta.Name, cn1.Spec.InstanceID, cn1.Spec.IPAM.PodCIDRs)
	}

	time.Sleep(100 * time.Millisecond)

	// ------------------------------------------------------------------
	// Phase 8: Operator GC 실행
	// ------------------------------------------------------------------
	printSectionHeader("Phase 8: Operator GC 실행")

	time.Sleep(100 * time.Millisecond)

	// Endpoint GC: orphan pod 삭제
	operator.RunEndpointGC()
	time.Sleep(200 * time.Millisecond)

	// Identity GC: 미참조 Identity 삭제
	operator.RunIdentityGC()
	time.Sleep(200 * time.Millisecond)

	// ------------------------------------------------------------------
	// Phase 9: CRD 삭제 및 이벤트 전파
	// ------------------------------------------------------------------
	printSectionHeader("Phase 9: CRD 삭제 및 이벤트 전파")

	// CNP 삭제
	fmt.Println("  --- CNP 삭제 ---")
	err := apiServer.Delete("CiliumNetworkPolicy", "default/allow-web-ingress")
	if err != nil {
		fmt.Printf("  CNP 삭제 실패: %v\n", err)
	} else {
		fmt.Println("  [APIServer] CNP 삭제됨: default/allow-web-ingress")
	}

	time.Sleep(200 * time.Millisecond)

	// CEP 삭제
	fmt.Println("\n  --- CEP 삭제 ---")
	err = apiServer.Delete("CiliumEndpoint", "default/web-pod-1")
	if err != nil {
		fmt.Printf("  CEP 삭제 실패: %v\n", err)
	} else {
		fmt.Println("  [APIServer] CEP 삭제됨: default/web-pod-1")
	}

	time.Sleep(200 * time.Millisecond)

	// ------------------------------------------------------------------
	// Phase 10: 최종 상태 확인
	// ------------------------------------------------------------------
	printSectionHeader("Phase 10: 최종 상태 확인")

	// API Server 상태
	fmt.Println("\n  [APIServer] 저장소 상태:")
	for _, kind := range []string{"CiliumNetworkPolicy", "CiliumEndpoint", "CiliumIdentity", "CiliumNode"} {
		objs := apiServer.List(kind)
		fmt.Printf("    %s: %d 개\n", kind, len(objs))
		for _, obj := range objs {
			meta := obj.GetObjectMeta()
			fmt.Printf("      - %s (rv=%s)\n", obj.GetKey(), meta.ResourceVersion)
		}
	}

	// Policy Repository 상태
	fmt.Printf("\n  [Agent] PolicyRepository revision: %d\n", agent.policyRepo.GetRevision())

	// IPCache 상태
	fmt.Println("\n  [Agent] IPCache 상태:")
	for _, ip := range []string{"10.0.1.10", "10.0.1.20"} {
		if id, ok := agent.ipCache.Lookup(ip); ok {
			fmt.Printf("    %s -> identity=%d\n", ip, id)
		} else {
			fmt.Printf("    %s -> (없음)\n", ip)
		}
	}

	// 등록된 CRD 요약
	fmt.Println("\n  [Scheme] 등록된 CRD 요약:")
	for _, t := range scheme.GetRegisteredTypes() {
		fmt.Printf("    %s/%s %-40s scope=%-10s short=%v\n",
			t.Group, t.Version, t.Kind, t.Scope, t.ShortNames)
	}

	fmt.Println("\n========================================================================")
	fmt.Println("  시뮬레이션 완료!")
	fmt.Println("========================================================================")
}
