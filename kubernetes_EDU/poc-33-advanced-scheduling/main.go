// Kubernetes 고급 스케줄링 PoC: Scheduling Gates + Dynamic Resource Allocation
//
// 이 PoC는 두 가지 고급 스케줄링 메커니즘을 시뮬레이션한다:
//
// Part 1: Scheduling Gates
//   - Pod에 Gate를 추가하여 스케줄링 큐 진입을 차단
//   - 외부 컨트롤러가 Gate를 제거하면 스케줄링 재개
//   - UID 기반 QueueingHint로 정확한 이벤트 매칭
//
// Part 2: Dynamic Resource Allocation (DRA)
//   - ResourceClaim 생성 및 디바이스 요청
//   - DeviceRequest 매칭 (CEL 셀렉터 시뮬레이션)
//   - Claim 템플릿 인스턴스화
//   - PreFilter -> Filter -> Reserve -> PreBind 전체 사이클
//   - 낙관적 동시성 제어 (inFlight + AssumeCache)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 공통 타입
// =============================================================================

// SchedulingStatus는 스케줄링 상태를 나타내는 코드.
// 실제 Kubernetes: staging/src/k8s.io/kube-scheduler/framework/interface.go
type SchedulingStatus int

const (
	StatusSuccess SchedulingStatus = iota
	StatusUnschedulable
	StatusUnschedulableAndUnresolvable
	StatusError
	StatusSkip
)

func (s SchedulingStatus) String() string {
	switch s {
	case StatusSuccess:
		return "Success"
	case StatusUnschedulable:
		return "Unschedulable"
	case StatusUnschedulableAndUnresolvable:
		return "UnschedulableAndUnresolvable"
	case StatusError:
		return "Error"
	case StatusSkip:
		return "Skip"
	default:
		return "Unknown"
	}
}

// StatusResult는 플러그인 반환값.
type StatusResult struct {
	Code    SchedulingStatus
	Message string
}

// QueueingHint는 이벤트가 Pod를 스케줄 가능하게 만드는지 판단한 결과.
type QueueingHint int

const (
	Queue     QueueingHint = iota // 큐로 이동 (스케줄 가능할 수 있음)
	QueueSkip                     // 무시 (관련 없는 이벤트)
)

func (q QueueingHint) String() string {
	if q == Queue {
		return "Queue"
	}
	return "QueueSkip"
}

// ClusterEventType은 클러스터 이벤트 타입.
type ClusterEventType string

const (
	EventUpdatePodSchedulingGatesEliminated ClusterEventType = "UpdatePodSchedulingGatesEliminated"
	EventResourceClaimUpdate               ClusterEventType = "ResourceClaimUpdate"
)

// ClusterEvent는 클러스터에서 발생한 이벤트.
type ClusterEvent struct {
	Type    ClusterEventType
	PodUID  string
	Payload interface{}
}

// =============================================================================
// Part 1: Scheduling Gates 시뮬레이션
// =============================================================================

// PodSchedulingGate는 Pod의 스케줄링을 차단하는 Gate.
// 실제 Kubernetes: staging/src/k8s.io/api/core/v1/types.go:4577
type PodSchedulingGate struct {
	Name string
}

// Pod는 스케줄링 대상 워크로드.
type Pod struct {
	UID             string
	Name            string
	Namespace       string
	SchedulingGates []PodSchedulingGate
	ResourceClaims  []PodResourceClaimRef // ResourceClaim 참조
	NodeName        string               // 할당된 노드
}

// PodResourceClaimRef는 Pod에서 ResourceClaim을 참조하는 방법.
// 실제 Kubernetes: staging/src/k8s.io/api/core/v1/types.go:4480
type PodResourceClaimRef struct {
	Name                      string  // Pod 내 고유 이름
	ResourceClaimName         *string // 직접 참조
	ResourceClaimTemplateName *string // 템플릿 참조
}

// HasGates는 Pod에 Gate가 있는지 확인한다.
func (p *Pod) HasGates() bool {
	return len(p.SchedulingGates) > 0
}

// RemoveGate는 특정 이름의 Gate를 제거한다.
func (p *Pod) RemoveGate(name string) bool {
	for i, gate := range p.SchedulingGates {
		if gate.Name == name {
			p.SchedulingGates = append(p.SchedulingGates[:i], p.SchedulingGates[i+1:]...)
			return true
		}
	}
	return false
}

// SchedulingGatesPlugin은 Scheduling Gates 플러그인.
// 실제 Kubernetes: pkg/scheduler/framework/plugins/schedulinggates/scheduling_gates.go:37
type SchedulingGatesPlugin struct {
	enableQueueHint bool
}

// PreEnqueue는 Pod가 큐에 들어가기 전에 Gate를 확인한다.
// Gate가 있으면 UnschedulableAndUnresolvable을 반환하여 큐 진입을 차단.
// 실제 Kubernetes: scheduling_gates.go:48-57
func (pl *SchedulingGatesPlugin) PreEnqueue(pod *Pod) *StatusResult {
	if len(pod.SchedulingGates) == 0 {
		return nil // Gate 없음 -> 통과
	}
	gates := make([]string, 0, len(pod.SchedulingGates))
	for _, gate := range pod.SchedulingGates {
		gates = append(gates, gate.Name)
	}
	return &StatusResult{
		Code:    StatusUnschedulableAndUnresolvable,
		Message: fmt.Sprintf("waiting for scheduling gates: %v", gates),
	}
}

// IsSchedulableAfterGateEliminated는 Gate 제거 이벤트에 대한 QueueingHint.
// 대기 중인 Pod의 UID와 이벤트의 Pod UID를 비교한다.
// 실제 Kubernetes: scheduling_gates.go:82-94
func (pl *SchedulingGatesPlugin) IsSchedulableAfterGateEliminated(
	waitingPod *Pod, event ClusterEvent,
) QueueingHint {
	if event.PodUID != waitingPod.UID {
		return QueueSkip // 다른 Pod의 이벤트 -> 무시
	}
	return Queue // 이 Pod의 Gate가 제거됨 -> 큐로 이동
}

// =============================================================================
// Part 2: Dynamic Resource Allocation (DRA) 시뮬레이션
// =============================================================================

// DeviceID는 장치를 고유하게 식별하는 키.
type DeviceID struct {
	Driver string
	Pool   string
	Device string
}

func (d DeviceID) String() string {
	return fmt.Sprintf("%s/%s/%s", d.Driver, d.Pool, d.Device)
}

// Node는 클러스터 노드.
type Node struct {
	Name   string
	Labels map[string]string
}

// DeviceInfo는 개별 장치 정보.
type DeviceInfo struct {
	Name       string
	Attributes map[string]string
}

// ResourceSlice는 노드의 가용 장치 목록.
// 실제 Kubernetes: DRA 드라이버가 ResourceSlice를 발행하여 노드의 장치를 등록
type ResourceSlice struct {
	NodeName string
	Driver   string
	Pool     string
	Devices  []DeviceInfo
}

// DeviceClass는 장치 클래스 정의.
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:1772
type DeviceClass struct {
	Name       string
	Selectors  []string          // CEL 셀렉터 시뮬레이션 ("key=value")
	Attributes map[string]string // 장치 속성 매칭
}

// AllocationMode는 장치 할당 모드.
type AllocationMode string

const (
	AllocationModeExactCount AllocationMode = "ExactCount"
	AllocationModeAll        AllocationMode = "All"
)

// ExactDeviceRequest는 정확한 장치 요청.
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:882
type ExactDeviceRequest struct {
	DeviceClassName string
	Selectors       []string // CEL 셀렉터 시뮬레이션
	AllocationMode  AllocationMode
	Count           int
}

// DeviceSubRequest는 FirstAvailable의 서브요청.
type DeviceSubRequest struct {
	Name            string
	DeviceClassName string
	Selectors       []string
	Count           int
}

// DeviceRequest는 장치 요청.
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:831
type DeviceRequest struct {
	Name           string
	Exactly        *ExactDeviceRequest
	FirstAvailable []DeviceSubRequest
}

// DeviceClaim은 장치 요청 집합.
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:772
type DeviceClaim struct {
	Requests []DeviceRequest
}

// ResourceClaimSpec은 리소스 요청 사양 (불변).
type ResourceClaimSpec struct {
	Devices DeviceClaim
}

// DeviceAllocationResult는 개별 장치 할당 결과.
type DeviceAllocationResult struct {
	Request string
	Driver  string
	Pool    string
	Device  string
}

// AllocationResult는 할당 결과.
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:1534
type AllocationResult struct {
	Devices      []DeviceAllocationResult
	NodeSelector string // 할당된 노드
}

// ConsumerReference는 리소스 소비자 참조.
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:1516
type ConsumerReference struct {
	Resource string
	Name     string
	UID      string
}

// ResourceClaimStatus는 리소스 할당 상태 (가변).
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:1445
type ResourceClaimStatus struct {
	Allocation  *AllocationResult
	ReservedFor []ConsumerReference
}

// ResourceClaim은 동적 리소스 요청.
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:743
type ResourceClaim struct {
	UID       string
	Name      string
	Namespace string
	OwnerPod  string // Owner Pod 이름 (템플릿에서 생성 시)
	Spec      ResourceClaimSpec
	Status    ResourceClaimStatus
}

// ResourceClaimTemplate은 Claim 생성 템플릿.
// 실제 Kubernetes: staging/src/k8s.io/api/resource/v1/types.go:1860
type ResourceClaimTemplate struct {
	Name      string
	Namespace string
	Spec      ResourceClaimSpec
}

// =============================================================================
// claimStore: Pod의 모든 ResourceClaim을 관리
// 실제 Kubernetes: pkg/scheduler/framework/plugins/dynamicresources/claims.go
// =============================================================================

type claimStore struct {
	claims []*ResourceClaim
}

func newClaimStore(claims []*ResourceClaim) claimStore {
	return claimStore{claims: claims}
}

func (cs *claimStore) empty() bool {
	return len(cs.claims) == 0
}

func (cs *claimStore) all() []*ResourceClaim {
	return cs.claims
}

func (cs *claimStore) toAllocate() []indexedClaim {
	var result []indexedClaim
	for i, claim := range cs.claims {
		if claim.Status.Allocation == nil {
			result = append(result, indexedClaim{Index: i, Claim: claim})
		}
	}
	return result
}

type indexedClaim struct {
	Index int
	Claim *ResourceClaim
}

// =============================================================================
// stateData: 스케줄링 사이클 동안의 DRA 상태
// 실제 Kubernetes: dynamicresources.go:67
// =============================================================================

type stateData struct {
	claims               claimStore
	allocator            *Allocator
	mutex                sync.Mutex
	unavailableClaims    map[int]bool
	informationsForClaim []informationForClaim
	nodeAllocations      map[string]nodeAllocation
}

type informationForClaim struct {
	availableOnNode string // 단순화: 노드 이름
	allocation      *AllocationResult
}

type nodeAllocation struct {
	allocationResults []AllocationResult
}

// =============================================================================
// claimTracker: AssumeCache + inFlightAllocations
// 실제 Kubernetes: pkg/scheduler/framework/plugins/dynamicresources/dra_manager.go
// =============================================================================

type claimTracker struct {
	assumeCache map[string]*ResourceClaim // key: namespace/name
	assumeMu    sync.RWMutex
	inFlight    sync.Map // key: claim UID, value: *ResourceClaim
	allocatedMu sync.RWMutex
	allocated   map[DeviceID]bool
}

func newClaimTracker() *claimTracker {
	return &claimTracker{
		assumeCache: make(map[string]*ResourceClaim),
		allocated:   make(map[DeviceID]bool),
	}
}

func (ct *claimTracker) Get(namespace, name string) (*ResourceClaim, bool) {
	key := namespace + "/" + name
	ct.assumeMu.RLock()
	defer ct.assumeMu.RUnlock()
	claim, ok := ct.assumeCache[key]
	return claim, ok
}

func (ct *claimTracker) AssumeClaimAfterAPICall(claim *ResourceClaim) {
	key := claim.Namespace + "/" + claim.Name
	ct.assumeMu.Lock()
	defer ct.assumeMu.Unlock()
	ct.assumeCache[key] = claim
	fmt.Printf("    [AssumeCache] 저장: '%s' (UID: %s)\n", claim.Name, claim.UID)
}

func (ct *claimTracker) SignalClaimPendingAllocation(claimUID string, claim *ResourceClaim) {
	ct.inFlight.Store(claimUID, claim)
	fmt.Printf("    [InFlight] 마킹: '%s' (UID: %s)\n", claim.Name, claimUID)
}

func (ct *claimTracker) RemoveClaimPendingAllocation(claimUID string) bool {
	_, loaded := ct.inFlight.LoadAndDelete(claimUID)
	if loaded {
		fmt.Printf("    [InFlight] 제거 (UID: %s)\n", claimUID)
	}
	return loaded
}

func (ct *claimTracker) ClaimHasPendingAllocation(claimUID string) bool {
	_, found := ct.inFlight.Load(claimUID)
	return found
}

func (ct *claimTracker) ListAllAllocatedDevices() map[DeviceID]bool {
	ct.allocatedMu.RLock()
	defer ct.allocatedMu.RUnlock()
	clone := make(map[DeviceID]bool, len(ct.allocated))
	for k, v := range ct.allocated {
		clone[k] = v
	}
	// inFlight 할당도 포함
	ct.inFlight.Range(func(key, value interface{}) bool {
		claim := value.(*ResourceClaim)
		if claim.Status.Allocation != nil {
			for _, result := range claim.Status.Allocation.Devices {
				deviceID := DeviceID{Driver: result.Driver, Pool: result.Pool, Device: result.Device}
				clone[deviceID] = true
			}
		}
		return true
	})
	return clone
}

func (ct *claimTracker) AddAllocatedDevice(id DeviceID) {
	ct.allocatedMu.Lock()
	defer ct.allocatedMu.Unlock()
	ct.allocated[id] = true
}

// =============================================================================
// Allocator: 노드별 장치 할당 시뮬레이션
// 실제 Kubernetes: k8s.io/dynamic-resource-allocation/structured 패키지
// =============================================================================

type Allocator struct {
	allocatedDevices map[DeviceID]bool
	deviceClasses    map[string]*DeviceClass
	resourceSlices   []*ResourceSlice
}

func NewAllocator(
	allocated map[DeviceID]bool,
	classes map[string]*DeviceClass,
	slices []*ResourceSlice,
) *Allocator {
	return &Allocator{
		allocatedDevices: allocated,
		deviceClasses:    classes,
		resourceSlices:   slices,
	}
}

// Allocate는 특정 노드에서 claim들의 장치를 할당 시도한다.
func (a *Allocator) Allocate(nodeName string, claims []*ResourceClaim) ([]AllocationResult, error) {
	var results []AllocationResult
	for _, claim := range claims {
		result, err := a.allocateClaim(nodeName, claim)
		if err != nil {
			return nil, err
		}
		results = append(results, *result)
	}
	return results, nil
}

func (a *Allocator) allocateClaim(nodeName string, claim *ResourceClaim) (*AllocationResult, error) {
	var allDeviceResults []DeviceAllocationResult

	for _, request := range claim.Spec.Devices.Requests {
		results, err := a.allocateRequest(nodeName, request)
		if err != nil {
			return nil, fmt.Errorf("request '%s': %v", request.Name, err)
		}
		allDeviceResults = append(allDeviceResults, results...)
	}

	return &AllocationResult{
		Devices:      allDeviceResults,
		NodeSelector: nodeName,
	}, nil
}

func (a *Allocator) allocateRequest(nodeName string, request DeviceRequest) ([]DeviceAllocationResult, error) {
	if request.Exactly != nil {
		return a.allocateExactRequest(nodeName, request.Name, *request.Exactly)
	}
	if len(request.FirstAvailable) > 0 {
		return a.allocateFirstAvailable(nodeName, request.Name, request.FirstAvailable)
	}
	return nil, fmt.Errorf("unknown request type")
}

func (a *Allocator) allocateExactRequest(nodeName, requestName string, req ExactDeviceRequest) ([]DeviceAllocationResult, error) {
	class, ok := a.deviceClasses[req.DeviceClassName]
	if !ok {
		return nil, fmt.Errorf("device class '%s' not found", req.DeviceClassName)
	}

	// 해당 노드의 ResourceSlice에서 가용 장치 찾기
	var available []struct {
		device DeviceInfo
		driver string
		pool   string
	}

	for _, slice := range a.resourceSlices {
		if slice.NodeName != nodeName {
			continue
		}
		for _, device := range slice.Devices {
			deviceID := DeviceID{Driver: slice.Driver, Pool: slice.Pool, Device: device.Name}
			if a.allocatedDevices[deviceID] {
				continue
			}
			// DeviceClass 셀렉터 매칭
			if !matchesSelectors(device, class.Selectors) {
				continue
			}
			// DeviceClass 속성 매칭
			if !matchesAttributes(device, class.Attributes) {
				continue
			}
			// Request 셀렉터 매칭
			if !matchesSelectors(device, req.Selectors) {
				continue
			}
			available = append(available, struct {
				device DeviceInfo
				driver string
				pool   string
			}{device, slice.Driver, slice.Pool})
		}
	}

	count := req.Count
	if count == 0 {
		count = 1
	}

	mode := req.AllocationMode
	if mode == "" {
		mode = AllocationModeExactCount
	}

	if mode == AllocationModeAll {
		if len(available) == 0 {
			return nil, fmt.Errorf("no matching devices on node '%s'", nodeName)
		}
		count = len(available)
	}

	if len(available) < count {
		return nil, fmt.Errorf("not enough devices on node '%s': need %d, available %d",
			nodeName, count, len(available))
	}

	var results []DeviceAllocationResult
	for i := 0; i < count; i++ {
		d := available[i]
		deviceID := DeviceID{Driver: d.driver, Pool: d.pool, Device: d.device.Name}
		a.allocatedDevices[deviceID] = true
		results = append(results, DeviceAllocationResult{
			Request: requestName,
			Driver:  d.driver,
			Pool:    d.pool,
			Device:  d.device.Name,
		})
	}

	return results, nil
}

func (a *Allocator) allocateFirstAvailable(nodeName, requestName string, subs []DeviceSubRequest) ([]DeviceAllocationResult, error) {
	// FirstAvailable: 순서대로 시도하여 첫 번째 성공한 서브요청 사용
	for _, sub := range subs {
		exactReq := ExactDeviceRequest{
			DeviceClassName: sub.DeviceClassName,
			Selectors:       sub.Selectors,
			AllocationMode:  AllocationModeExactCount,
			Count:           sub.Count,
		}
		if exactReq.Count == 0 {
			exactReq.Count = 1
		}
		results, err := a.allocateExactRequest(nodeName, requestName, exactReq)
		if err == nil {
			fmt.Printf("      FirstAvailable: 서브요청 '%s' 선택됨\n", sub.Name)
			return results, nil
		}
	}
	return nil, fmt.Errorf("no subrequest could be satisfied on node '%s'", nodeName)
}

func matchesSelectors(device DeviceInfo, selectors []string) bool {
	for _, sel := range selectors {
		parts := strings.SplitN(sel, "=", 2)
		if len(parts) != 2 {
			continue
		}
		if device.Attributes[parts[0]] != parts[1] {
			return false
		}
	}
	return true
}

func matchesAttributes(device DeviceInfo, attrs map[string]string) bool {
	for key, value := range attrs {
		if device.Attributes[key] != value {
			return false
		}
	}
	return true
}

// =============================================================================
// ResourceClaim Controller 시뮬레이션
// 실제 Kubernetes: pkg/controller/resourceclaim/controller.go
// =============================================================================

type ResourceClaimController struct {
	templates map[string]*ResourceClaimTemplate // namespace/name -> template
	claims    map[string]*ResourceClaim         // namespace/name -> claim
	mu        sync.Mutex
	uidSeq    int
}

func NewResourceClaimController(
	templates map[string]*ResourceClaimTemplate,
	claims map[string]*ResourceClaim,
) *ResourceClaimController {
	return &ResourceClaimController{
		templates: templates,
		claims:    claims,
	}
}

// SyncPod는 Pod의 ResourceClaim 참조를 처리한다.
// 실제 Kubernetes: controller.go syncPod() + handleClaim()
func (c *ResourceClaimController) SyncPod(pod *Pod) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range pod.ResourceClaims {
		ref := &pod.ResourceClaims[i]

		if ref.ResourceClaimName != nil {
			// 직접 참조: Claim이 존재하는지 확인만
			key := pod.Namespace + "/" + *ref.ResourceClaimName
			if _, ok := c.claims[key]; !ok {
				return fmt.Errorf("ResourceClaim '%s' not found", *ref.ResourceClaimName)
			}
			fmt.Printf("    [Controller] Claim '%s': 직접 참조, 이미 존재\n", *ref.ResourceClaimName)
			continue
		}

		if ref.ResourceClaimTemplateName != nil {
			// 템플릿 참조: Claim 생성
			templateKey := pod.Namespace + "/" + *ref.ResourceClaimTemplateName
			template, ok := c.templates[templateKey]
			if !ok {
				return fmt.Errorf("ResourceClaimTemplate '%s' not found", *ref.ResourceClaimTemplateName)
			}

			// generateName: <pod>-<claim>-<random>
			c.uidSeq++
			claimName := fmt.Sprintf("%s-%s-%04d", pod.Name, ref.Name, c.uidSeq)
			claimUID := fmt.Sprintf("claim-uid-%d", c.uidSeq)

			claim := &ResourceClaim{
				UID:       claimUID,
				Name:      claimName,
				Namespace: pod.Namespace,
				OwnerPod:  pod.Name,
				Spec:      template.Spec,
			}

			key := pod.Namespace + "/" + claimName
			c.claims[key] = claim

			// Pod의 참조를 업데이트 (실제에서는 pod.status.resourceClaimStatuses)
			claimNameStr := claimName
			ref.ResourceClaimName = &claimNameStr

			fmt.Printf("    [Controller] 템플릿 '%s' -> Claim '%s' 생성 (Owner: %s)\n",
				*ref.ResourceClaimTemplateName, claimName, pod.Name)
		}
	}
	return nil
}

// =============================================================================
// DynamicResourcesPlugin: DRA 스케줄러 플러그인 시뮬레이션
// 실제 Kubernetes: dynamicresources.go:138
// =============================================================================

type DynamicResourcesPlugin struct {
	claimTracker   *claimTracker
	deviceClasses  map[string]*DeviceClass
	resourceSlices []*ResourceSlice
	claims         map[string]*ResourceClaim // namespace/name -> claim
}

func NewDynamicResourcesPlugin(
	classes map[string]*DeviceClass,
	slices []*ResourceSlice,
	claims map[string]*ResourceClaim,
) *DynamicResourcesPlugin {
	return &DynamicResourcesPlugin{
		claimTracker:   newClaimTracker(),
		deviceClasses:  classes,
		resourceSlices: slices,
		claims:         claims,
	}
}

// PreEnqueue: Claim이 모두 존재하는지 확인
func (pl *DynamicResourcesPlugin) PreEnqueue(pod *Pod) *StatusResult {
	for _, ref := range pod.ResourceClaims {
		if ref.ResourceClaimName == nil {
			return &StatusResult{
				Code:    StatusUnschedulable,
				Message: fmt.Sprintf("PodResourceClaim '%s': claim not yet created", ref.Name),
			}
		}
		key := pod.Namespace + "/" + *ref.ResourceClaimName
		if _, ok := pl.claims[key]; !ok {
			if _, ok := pl.claimTracker.Get(pod.Namespace, *ref.ResourceClaimName); !ok {
				return &StatusResult{
					Code:    StatusUnschedulable,
					Message: fmt.Sprintf("ResourceClaim '%s' not found", *ref.ResourceClaimName),
				}
			}
		}
	}
	return nil
}

// PreFilter: claim 수집, Allocator 초기화
func (pl *DynamicResourcesPlugin) PreFilter(pod *Pod) (*stateData, *StatusResult) {
	fmt.Printf("  [PreFilter] Pod '%s'\n", pod.Name)

	var podClaims []*ResourceClaim
	for _, ref := range pod.ResourceClaims {
		if ref.ResourceClaimName == nil {
			continue
		}
		key := pod.Namespace + "/" + *ref.ResourceClaimName
		claim, ok := pl.claims[key]
		if !ok {
			claim, ok = pl.claimTracker.Get(pod.Namespace, *ref.ResourceClaimName)
			if !ok {
				return nil, &StatusResult{
					Code:    StatusUnschedulable,
					Message: fmt.Sprintf("ResourceClaim '%s' not found", *ref.ResourceClaimName),
				}
			}
		}
		podClaims = append(podClaims, claim)
	}

	if len(podClaims) == 0 {
		return nil, &StatusResult{Code: StatusSkip, Message: "no claims"}
	}

	state := &stateData{
		claims:               newClaimStore(podClaims),
		informationsForClaim: make([]informationForClaim, len(podClaims)),
		nodeAllocations:      make(map[string]nodeAllocation),
	}

	numToAllocate := 0
	for i, claim := range podClaims {
		if claim.Status.Allocation != nil {
			state.informationsForClaim[i].availableOnNode = claim.Status.Allocation.NodeSelector
			fmt.Printf("    Claim '%s': 이미 할당됨 (node: %s)\n",
				claim.Name, claim.Status.Allocation.NodeSelector)
		} else {
			numToAllocate++
			if pl.claimTracker.ClaimHasPendingAllocation(claim.UID) {
				return nil, &StatusResult{
					Code:    StatusUnschedulable,
					Message: fmt.Sprintf("claim '%s' has pending allocation", claim.Name),
				}
			}
			// DeviceClass 검증
			for _, req := range claim.Spec.Devices.Requests {
				if req.Exactly != nil {
					if _, ok := pl.deviceClasses[req.Exactly.DeviceClassName]; !ok {
						return nil, &StatusResult{
							Code:    StatusUnschedulable,
							Message: fmt.Sprintf("device class '%s' does not exist", req.Exactly.DeviceClassName),
						}
					}
				}
			}
			fmt.Printf("    Claim '%s': 할당 필요 (%d개 request)\n",
				claim.Name, len(claim.Spec.Devices.Requests))
		}
	}

	if numToAllocate > 0 {
		allocatedDevices := pl.claimTracker.ListAllAllocatedDevices()
		state.allocator = NewAllocator(allocatedDevices, pl.deviceClasses, pl.resourceSlices)
		fmt.Printf("    Allocator 초기화 (이미 할당된 장치: %d개)\n", len(allocatedDevices))
	}

	return state, nil
}

// Filter: 노드별 할당 가능 여부 확인
func (pl *DynamicResourcesPlugin) Filter(state *stateData, pod *Pod, node *Node) *StatusResult {
	if state.claims.empty() {
		return nil
	}

	// 이미 할당된 claim의 노드 셀렉터 확인
	for i, claim := range state.claims.all() {
		info := state.informationsForClaim[i]
		if info.availableOnNode != "" && info.availableOnNode != node.Name {
			return &StatusResult{
				Code:    StatusUnschedulable,
				Message: fmt.Sprintf("claim '%s' not available on node '%s'", claim.Name, node.Name),
			}
		}
	}

	// 미할당 claim에 대해 할당 시뮬레이션
	if state.allocator != nil {
		claimsToAllocate := make([]*ResourceClaim, 0)
		for _, ic := range state.claims.toAllocate() {
			claimsToAllocate = append(claimsToAllocate, ic.Claim)
		}

		allocations, err := state.allocator.Allocate(node.Name, claimsToAllocate)
		if err != nil {
			return &StatusResult{
				Code:    StatusUnschedulable,
				Message: fmt.Sprintf("allocation failed: %v", err),
			}
		}

		// 결과 캐싱 (mutex 보호 -- Filter는 병렬 실행)
		state.mutex.Lock()
		state.nodeAllocations[node.Name] = nodeAllocation{allocationResults: allocations}
		state.mutex.Unlock()
	}

	return nil
}

// Reserve: 할당 결과 확정, inFlight 마킹
func (pl *DynamicResourcesPlugin) Reserve(state *stateData, pod *Pod, nodeName string) *StatusResult {
	fmt.Printf("  [Reserve] Pod '%s' -> Node '%s'\n", pod.Name, nodeName)
	if state.claims.empty() {
		return nil
	}

	allocations, ok := state.nodeAllocations[nodeName]
	if !ok {
		return &StatusResult{Code: StatusError, Message: "allocation not found for node"}
	}

	allocIdx := 0
	for _, ic := range state.claims.toAllocate() {
		if allocIdx >= len(allocations.allocationResults) {
			return &StatusResult{Code: StatusError, Message: "allocation result mismatch"}
		}
		allocation := allocations.allocationResults[allocIdx]
		state.informationsForClaim[ic.Index].allocation = &allocation

		claimCopy := *ic.Claim
		claimCopy.Status.Allocation = &allocation
		pl.claimTracker.SignalClaimPendingAllocation(ic.Claim.UID, &claimCopy)
		allocIdx++
	}
	return nil
}

// Unreserve: 할당 롤백
func (pl *DynamicResourcesPlugin) Unreserve(state *stateData, pod *Pod) {
	fmt.Printf("  [Unreserve] Pod '%s': 롤백\n", pod.Name)
	for _, claim := range state.claims.all() {
		pl.claimTracker.RemoveClaimPendingAllocation(claim.UID)
	}
}

// PreBind: API 서버에 할당 기록 (시뮬레이션)
func (pl *DynamicResourcesPlugin) PreBind(state *stateData, pod *Pod, nodeName string) *StatusResult {
	fmt.Printf("  [PreBind] Pod '%s': API 서버에 기록\n", pod.Name)
	for i, claim := range state.claims.all() {
		allocation := state.informationsForClaim[i].allocation
		if allocation == nil && claim.Status.Allocation != nil {
			claim.Status.ReservedFor = append(claim.Status.ReservedFor, ConsumerReference{
				Resource: "pods", Name: pod.Name, UID: pod.UID,
			})
			fmt.Printf("    Claim '%s': ReservedFor에 Pod '%s' 추가\n", claim.Name, pod.Name)
			continue
		}
		if allocation != nil {
			claim.Status.Allocation = allocation
			claim.Status.ReservedFor = append(claim.Status.ReservedFor, ConsumerReference{
				Resource: "pods", Name: pod.Name, UID: pod.UID,
			})
			fmt.Printf("    Claim '%s': 할당 확정 (node: %s, devices: %d개)\n",
				claim.Name, allocation.NodeSelector, len(allocation.Devices))
			pl.claimTracker.AssumeClaimAfterAPICall(claim)
			for _, device := range allocation.Devices {
				deviceID := DeviceID{Driver: device.Driver, Pool: device.Pool, Device: device.Device}
				pl.claimTracker.AddAllocatedDevice(deviceID)
			}
			pl.claimTracker.RemoveClaimPendingAllocation(claim.UID)
		}
	}
	return nil
}

// =============================================================================
// SchedulingQueue: 스케줄링 큐 시뮬레이션
// =============================================================================

type SchedulingQueue struct {
	activeQueue        []*Pod
	unschedulableQueue map[string]*Pod
	gatesPlugin        *SchedulingGatesPlugin
	mu                 sync.Mutex
}

func NewSchedulingQueue(gatesPlugin *SchedulingGatesPlugin) *SchedulingQueue {
	return &SchedulingQueue{
		unschedulableQueue: make(map[string]*Pod),
		gatesPlugin:        gatesPlugin,
	}
}

func (q *SchedulingQueue) Add(pod *Pod) {
	q.mu.Lock()
	defer q.mu.Unlock()
	result := q.gatesPlugin.PreEnqueue(pod)
	if result != nil && result.Code == StatusUnschedulableAndUnresolvable {
		q.unschedulableQueue[pod.UID] = pod
		fmt.Printf("  [Queue] Pod '%s' -> Unschedulable (%s)\n", pod.Name, result.Message)
		return
	}
	q.activeQueue = append(q.activeQueue, pod)
	fmt.Printf("  [Queue] Pod '%s' -> Active\n", pod.Name)
}

func (q *SchedulingQueue) HandleEvent(event ClusterEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if event.Type != EventUpdatePodSchedulingGatesEliminated {
		return
	}
	for uid, pod := range q.unschedulableQueue {
		hint := q.gatesPlugin.IsSchedulableAfterGateEliminated(pod, event)
		if hint == Queue {
			result := q.gatesPlugin.PreEnqueue(pod)
			if result == nil {
				delete(q.unschedulableQueue, uid)
				q.activeQueue = append(q.activeQueue, pod)
				fmt.Printf("  [Queue] Pod '%s': Unschedulable -> Active\n", pod.Name)
			} else {
				fmt.Printf("  [Queue] Pod '%s': 아직 Gate 남아있음\n", pod.Name)
			}
		}
	}
}

func (q *SchedulingQueue) Pop() *Pod {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.activeQueue) == 0 {
		return nil
	}
	pod := q.activeQueue[0]
	q.activeQueue = q.activeQueue[1:]
	return pod
}

func (q *SchedulingQueue) ActiveLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.activeQueue)
}

func (q *SchedulingQueue) UnschedulableLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.unschedulableQueue)
}

// =============================================================================
// 메인: 데모 실행
// =============================================================================

func main() {
	fmt.Println("==========================================================")
	fmt.Println("Kubernetes 고급 스케줄링 PoC")
	fmt.Println("Scheduling Gates + Dynamic Resource Allocation (DRA)")
	fmt.Println("==========================================================")
	fmt.Println()

	demoSchedulingGates()
	fmt.Println()
	demoClaimTemplateInstantiation()
	fmt.Println()
	demoDRASchedulingCycle()
	fmt.Println()
	demoDeviceRequestMatching()
	fmt.Println()
	demoIntegrated()
}

// =============================================================================
// Demo 1: Scheduling Gates 차단/해제
// =============================================================================

func demoSchedulingGates() {
	fmt.Println("##########################################################")
	fmt.Println("Demo 1: Scheduling Gates 차단/해제")
	fmt.Println("##########################################################")
	fmt.Println()

	gatesPlugin := &SchedulingGatesPlugin{enableQueueHint: true}
	queue := NewSchedulingQueue(gatesPlugin)

	// 1. Gate가 있는 Pod 생성
	fmt.Println("[1] Pod 생성 (Gate 2개)")
	pod := &Pod{
		UID:       fmt.Sprintf("uid-%d", rand.Intn(10000)),
		Name:      "gated-pod",
		Namespace: "default",
		SchedulingGates: []PodSchedulingGate{
			{Name: "example.com/storage-ready"},
			{Name: "example.com/network-ready"},
		},
	}
	queue.Add(pod)
	fmt.Printf("  Active: %d, Unschedulable: %d\n\n", queue.ActiveLen(), queue.UnschedulableLen())

	// 2. Gate 없는 Pod
	fmt.Println("[2] Pod 생성 (Gate 없음)")
	normalPod := &Pod{
		UID:       fmt.Sprintf("uid-%d", rand.Intn(10000)),
		Name:      "normal-pod",
		Namespace: "default",
	}
	queue.Add(normalPod)
	fmt.Printf("  Active: %d, Unschedulable: %d\n\n", queue.ActiveLen(), queue.UnschedulableLen())

	// 3. 첫 번째 Gate 제거
	fmt.Println("[3] Gate 'storage-ready' 제거")
	pod.RemoveGate("example.com/storage-ready")
	queue.HandleEvent(ClusterEvent{
		Type: EventUpdatePodSchedulingGatesEliminated, PodUID: pod.UID,
	})
	fmt.Printf("  Active: %d, Unschedulable: %d\n\n", queue.ActiveLen(), queue.UnschedulableLen())

	// 4. 두 번째 Gate 제거
	fmt.Println("[4] Gate 'network-ready' 제거 -> 모든 Gate 해제")
	pod.RemoveGate("example.com/network-ready")
	queue.HandleEvent(ClusterEvent{
		Type: EventUpdatePodSchedulingGatesEliminated, PodUID: pod.UID,
	})
	fmt.Printf("  Active: %d, Unschedulable: %d\n\n", queue.ActiveLen(), queue.UnschedulableLen())

	// 5. QueueSkip 확인
	fmt.Println("[5] 다른 Pod의 이벤트 -> QueueSkip")
	hint := gatesPlugin.IsSchedulableAfterGateEliminated(pod, ClusterEvent{
		Type: EventUpdatePodSchedulingGatesEliminated, PodUID: "other-uid",
	})
	fmt.Printf("  QueueingHint: %s\n\n", hint)

	// 6. 큐 처리
	fmt.Println("[6] Active 큐 처리")
	for {
		p := queue.Pop()
		if p == nil {
			break
		}
		fmt.Printf("  스케줄링 대상: '%s'\n", p.Name)
	}
}

// =============================================================================
// Demo 2: Claim 템플릿 인스턴스화
// =============================================================================

func demoClaimTemplateInstantiation() {
	fmt.Println("##########################################################")
	fmt.Println("Demo 2: ResourceClaim 템플릿 인스턴스화")
	fmt.Println("##########################################################")
	fmt.Println()

	templates := map[string]*ResourceClaimTemplate{
		"default/gpu-template": {
			Name:      "gpu-template",
			Namespace: "default",
			Spec: ResourceClaimSpec{
				Devices: DeviceClaim{
					Requests: []DeviceRequest{
						{
							Name: "gpu",
							Exactly: &ExactDeviceRequest{
								DeviceClassName: "gpu.nvidia.com",
								Count:           1,
							},
						},
					},
				},
			},
		},
	}

	claims := make(map[string]*ResourceClaim)
	controller := NewResourceClaimController(templates, claims)

	templateName := "gpu-template"
	pod := &Pod{
		UID:       "pod-uid-template-001",
		Name:      "ml-job",
		Namespace: "default",
		ResourceClaims: []PodResourceClaimRef{
			{
				Name:                      "gpu",
				ResourceClaimTemplateName: &templateName,
			},
		},
	}

	fmt.Println("[1] Pod 생성 (ResourceClaimTemplateName 참조)")
	fmt.Printf("    Pod: %s, Template: %s\n\n", pod.Name, templateName)

	fmt.Println("[2] Controller.SyncPod() -> Claim 생성")
	if err := controller.SyncPod(pod); err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	fmt.Println()

	fmt.Println("[3] 생성된 Claim 확인")
	for key, claim := range claims {
		fmt.Printf("    Key: %s\n", key)
		fmt.Printf("    Name: %s, UID: %s, Owner: %s\n", claim.Name, claim.UID, claim.OwnerPod)
		fmt.Printf("    Requests: %d개\n", len(claim.Spec.Devices.Requests))
	}
	fmt.Println()

	fmt.Println("[4] Pod의 ResourceClaimRef 업데이트 확인")
	for _, ref := range pod.ResourceClaims {
		if ref.ResourceClaimName != nil {
			fmt.Printf("    ref.Name='%s' -> ResourceClaimName='%s'\n", ref.Name, *ref.ResourceClaimName)
		}
	}
}

// =============================================================================
// Demo 3: DRA 스케줄링 사이클 (전체 흐름)
// =============================================================================

func demoDRASchedulingCycle() {
	fmt.Println("##########################################################")
	fmt.Println("Demo 3: DRA 스케줄링 사이클 (전체 흐름)")
	fmt.Println("##########################################################")
	fmt.Println()

	deviceClasses := map[string]*DeviceClass{
		"gpu.nvidia.com": {
			Name:       "gpu.nvidia.com",
			Attributes: map[string]string{"type": "gpu"},
		},
	}

	resourceSlices := []*ResourceSlice{
		{
			NodeName: "node-1",
			Driver:   "nvidia.com",
			Pool:     "node-1-gpus",
			Devices: []DeviceInfo{
				{Name: "gpu-0", Attributes: map[string]string{"type": "gpu", "memory": "16Gi", "model": "A100"}},
				{Name: "gpu-1", Attributes: map[string]string{"type": "gpu", "memory": "16Gi", "model": "A100"}},
				{Name: "gpu-2", Attributes: map[string]string{"type": "gpu", "memory": "8Gi", "model": "T4"}},
			},
		},
		{
			NodeName: "node-2",
			Driver:   "nvidia.com",
			Pool:     "node-2-gpus",
			Devices: []DeviceInfo{
				{Name: "gpu-0", Attributes: map[string]string{"type": "gpu", "memory": "8Gi", "model": "T4"}},
				{Name: "gpu-1", Attributes: map[string]string{"type": "gpu", "memory": "8Gi", "model": "T4"}},
			},
		},
		{
			NodeName: "node-3",
			Driver:   "nvidia.com",
			Pool:     "node-3-gpus",
			Devices: []DeviceInfo{
				{Name: "gpu-0", Attributes: map[string]string{"type": "gpu", "memory": "16Gi", "model": "A100"}},
			},
		},
	}

	claimName := "training-gpu"
	gpuClaim := &ResourceClaim{
		UID:       "claim-uid-001",
		Name:      claimName,
		Namespace: "ml-team",
		Spec: ResourceClaimSpec{
			Devices: DeviceClaim{
				Requests: []DeviceRequest{
					{
						Name: "gpus",
						Exactly: &ExactDeviceRequest{
							DeviceClassName: "gpu.nvidia.com",
							Selectors:       []string{"memory=16Gi"},
							Count:           2,
						},
					},
				},
			},
		},
	}

	claims := map[string]*ResourceClaim{"ml-team/training-gpu": gpuClaim}
	nodes := []*Node{
		{Name: "node-1"}, {Name: "node-2"}, {Name: "node-3"},
	}

	plugin := NewDynamicResourcesPlugin(deviceClasses, resourceSlices, claims)

	pod := &Pod{
		UID:       "pod-uid-001",
		Name:      "training-job",
		Namespace: "ml-team",
		ResourceClaims: []PodResourceClaimRef{
			{Name: "gpu", ResourceClaimName: &claimName},
		},
	}

	fmt.Printf("[1] 스케줄링 시작: Pod=%s, Claim=%s (A100 16Gi x 2)\n\n", pod.Name, gpuClaim.Name)

	// PreFilter
	fmt.Println("[2] PreFilter")
	state, status := plugin.PreFilter(pod)
	if status != nil && status.Code != StatusSuccess && status.Code != StatusSkip {
		fmt.Printf("  실패: %s\n", status.Message)
		return
	}
	fmt.Println()

	// Filter
	fmt.Println("[3] Filter (노드별)")
	var passedNodes []*Node
	for _, node := range nodes {
		fs := plugin.Filter(state, pod, node)
		if fs != nil {
			fmt.Printf("  Node '%s': FAIL - %s\n", node.Name, fs.Message)
		} else {
			fmt.Printf("  Node '%s': PASS\n", node.Name)
			passedNodes = append(passedNodes, node)
		}
	}
	fmt.Println()

	if len(passedNodes) == 0 {
		fmt.Println("  모든 노드에서 할당 실패!")
		return
	}

	selectedNode := passedNodes[0]
	fmt.Printf("[4] 노드 선택: '%s'\n\n", selectedNode.Name)

	// Reserve
	fmt.Println("[5] Reserve")
	plugin.Reserve(state, pod, selectedNode.Name)
	fmt.Println()

	// 동시성 테스트
	fmt.Println("[6] 동시성: 다른 Pod가 같은 claim 접근")
	otherClaimName := claimName
	otherPod := &Pod{
		UID:       "pod-uid-002",
		Name:      "inference-job",
		Namespace: "ml-team",
		ResourceClaims: []PodResourceClaimRef{
			{Name: "gpu", ResourceClaimName: &otherClaimName},
		},
	}
	_, otherStatus := plugin.PreFilter(otherPod)
	if otherStatus != nil {
		fmt.Printf("  Pod '%s': %s (%s)\n", otherPod.Name, otherStatus.Code, otherStatus.Message)
	}
	fmt.Println()

	// PreBind
	fmt.Println("[7] PreBind")
	plugin.PreBind(state, pod, selectedNode.Name)
	fmt.Println()

	// 결과
	fmt.Println("[8] 최종 상태")
	fmt.Printf("  Pod '%s' -> Node '%s'\n", pod.Name, selectedNode.Name)
	if gpuClaim.Status.Allocation != nil {
		fmt.Printf("  Devices:\n")
		for _, d := range gpuClaim.Status.Allocation.Devices {
			fmt.Printf("    - %s/%s/%s\n", d.Driver, d.Pool, d.Device)
		}
	}
}

// =============================================================================
// Demo 4: DeviceRequest 매칭 (Exactly + FirstAvailable)
// =============================================================================

func demoDeviceRequestMatching() {
	fmt.Println("##########################################################")
	fmt.Println("Demo 4: DeviceRequest 매칭 (Exactly + FirstAvailable)")
	fmt.Println("##########################################################")
	fmt.Println()

	deviceClasses := map[string]*DeviceClass{
		"gpu-a100": {
			Name:       "gpu-a100",
			Attributes: map[string]string{"type": "gpu", "model": "A100"},
		},
		"gpu-h100": {
			Name:       "gpu-h100",
			Attributes: map[string]string{"type": "gpu", "model": "H100"},
		},
		"gpu-t4": {
			Name:       "gpu-t4",
			Attributes: map[string]string{"type": "gpu", "model": "T4"},
		},
	}

	slices := []*ResourceSlice{
		{
			NodeName: "gpu-node",
			Driver:   "nvidia.com",
			Pool:     "gpu-pool",
			Devices: []DeviceInfo{
				{Name: "gpu-0", Attributes: map[string]string{"type": "gpu", "model": "T4", "memory": "8Gi"}},
				{Name: "gpu-1", Attributes: map[string]string{"type": "gpu", "model": "T4", "memory": "8Gi"}},
			},
		},
	}

	// Scenario A: Exactly 요청 (A100을 원하지만 노드에 없음)
	fmt.Println("[Scenario A] Exactly 요청 - A100 (노드에 없음)")
	claimA := &ResourceClaim{
		UID: "claim-a", Name: "want-a100", Namespace: "default",
		Spec: ResourceClaimSpec{
			Devices: DeviceClaim{
				Requests: []DeviceRequest{
					{
						Name: "gpu",
						Exactly: &ExactDeviceRequest{
							DeviceClassName: "gpu-a100",
							Count:           1,
						},
					},
				},
			},
		},
	}

	allocator := NewAllocator(make(map[DeviceID]bool), deviceClasses, slices)
	_, err := allocator.Allocate("gpu-node", []*ResourceClaim{claimA})
	if err != nil {
		fmt.Printf("  결과: 할당 실패 - %v\n\n", err)
	}

	// Scenario B: FirstAvailable (H100 -> A100 -> T4 순서로 시도)
	fmt.Println("[Scenario B] FirstAvailable 요청 - H100 > A100 > T4 순서")
	claimB := &ResourceClaim{
		UID: "claim-b", Name: "flexible-gpu", Namespace: "default",
		Spec: ResourceClaimSpec{
			Devices: DeviceClaim{
				Requests: []DeviceRequest{
					{
						Name: "gpu",
						FirstAvailable: []DeviceSubRequest{
							{Name: "prefer-h100", DeviceClassName: "gpu-h100", Count: 1},
							{Name: "prefer-a100", DeviceClassName: "gpu-a100", Count: 1},
							{Name: "fallback-t4", DeviceClassName: "gpu-t4", Count: 1},
						},
					},
				},
			},
		},
	}

	allocator2 := NewAllocator(make(map[DeviceID]bool), deviceClasses, slices)
	results, err := allocator2.Allocate("gpu-node", []*ResourceClaim{claimB})
	if err != nil {
		fmt.Printf("  결과: 할당 실패 - %v\n\n", err)
	} else {
		fmt.Printf("  결과: 할당 성공\n")
		for _, r := range results {
			for _, d := range r.Devices {
				fmt.Printf("    Device: %s/%s/%s (request: %s)\n", d.Driver, d.Pool, d.Device, d.Request)
			}
		}
		fmt.Println()
	}

	// Scenario C: AllocationMode=All (모든 T4 할당)
	fmt.Println("[Scenario C] AllocationMode=All - 모든 T4 GPU 할당")
	claimC := &ResourceClaim{
		UID: "claim-c", Name: "all-gpus", Namespace: "default",
		Spec: ResourceClaimSpec{
			Devices: DeviceClaim{
				Requests: []DeviceRequest{
					{
						Name: "all-gpus",
						Exactly: &ExactDeviceRequest{
							DeviceClassName: "gpu-t4",
							AllocationMode:  AllocationModeAll,
						},
					},
				},
			},
		},
	}

	allocator3 := NewAllocator(make(map[DeviceID]bool), deviceClasses, slices)
	results3, err := allocator3.Allocate("gpu-node", []*ResourceClaim{claimC})
	if err != nil {
		fmt.Printf("  결과: 할당 실패 - %v\n\n", err)
	} else {
		fmt.Printf("  결과: 할당 성공 (%d개 장치)\n", len(results3[0].Devices))
		for _, d := range results3[0].Devices {
			fmt.Printf("    Device: %s/%s/%s\n", d.Driver, d.Pool, d.Device)
		}
	}
}

// =============================================================================
// Demo 5: 통합 시나리오 (Scheduling Gates + 템플릿 + DRA)
// =============================================================================

func demoIntegrated() {
	fmt.Println("##########################################################")
	fmt.Println("Demo 5: 통합 - Gates + 템플릿 + DRA 스케줄링")
	fmt.Println("##########################################################")
	fmt.Println()

	// 설정
	gatesPlugin := &SchedulingGatesPlugin{enableQueueHint: true}
	queue := NewSchedulingQueue(gatesPlugin)

	deviceClasses := map[string]*DeviceClass{
		"gpu.nvidia.com": {
			Name:       "gpu.nvidia.com",
			Attributes: map[string]string{"type": "gpu"},
		},
	}

	slices := []*ResourceSlice{
		{
			NodeName: "gpu-node-1",
			Driver:   "nvidia.com",
			Pool:     "gpu-pool",
			Devices: []DeviceInfo{
				{Name: "gpu-0", Attributes: map[string]string{"type": "gpu", "memory": "32Gi"}},
			},
		},
	}

	templates := map[string]*ResourceClaimTemplate{
		"default/gpu-claim-tmpl": {
			Name:      "gpu-claim-tmpl",
			Namespace: "default",
			Spec: ResourceClaimSpec{
				Devices: DeviceClaim{
					Requests: []DeviceRequest{
						{
							Name: "gpu",
							Exactly: &ExactDeviceRequest{
								DeviceClassName: "gpu.nvidia.com",
								Count:           1,
							},
						},
					},
				},
			},
		},
	}

	claims := make(map[string]*ResourceClaim)
	controller := NewResourceClaimController(templates, claims)

	templateName := "gpu-claim-tmpl"
	pod := &Pod{
		UID:       fmt.Sprintf("uid-%d", rand.Intn(10000)),
		Name:      "ml-training",
		Namespace: "default",
		SchedulingGates: []PodSchedulingGate{
			{Name: "nvidia.com/driver-installed"},
		},
		ResourceClaims: []PodResourceClaimRef{
			{Name: "gpu", ResourceClaimTemplateName: &templateName},
		},
	}

	// 1. Pod 생성 -> Gate에 의해 차단
	fmt.Println("[1] Pod 생성 (Gate: driver-installed, Template: gpu-claim-tmpl)")
	queue.Add(pod)
	fmt.Printf("  Active: %d, Unschedulable: %d\n\n", queue.ActiveLen(), queue.UnschedulableLen())

	// 2. Controller가 템플릿에서 Claim 생성
	fmt.Println("[2] Controller: 템플릿에서 Claim 생성")
	if err := controller.SyncPod(pod); err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	fmt.Println()

	// 3. 드라이버 설치 완료 -> Gate 제거
	fmt.Println("[3] 드라이버 설치 완료 -> Gate 제거")
	time.Sleep(50 * time.Millisecond)
	pod.RemoveGate("nvidia.com/driver-installed")
	queue.HandleEvent(ClusterEvent{
		Type: EventUpdatePodSchedulingGatesEliminated, PodUID: pod.UID,
	})
	fmt.Printf("  Active: %d, Unschedulable: %d\n\n", queue.ActiveLen(), queue.UnschedulableLen())

	// 4. 스케줄링 시작
	fmt.Println("[4] 스케줄링 사이클 시작")
	schedulePod := queue.Pop()
	if schedulePod == nil {
		fmt.Println("  큐가 비어있음")
		return
	}
	fmt.Printf("  Pod '%s' 스케줄링 시작\n\n", schedulePod.Name)

	draPlugin := NewDynamicResourcesPlugin(deviceClasses, slices, claims)

	// PreFilter
	fmt.Println("[5] DRA PreFilter")
	state, status := draPlugin.PreFilter(schedulePod)
	if status != nil && status.Code != StatusSuccess && status.Code != StatusSkip {
		fmt.Printf("  실패: %s\n", status.Message)
		return
	}
	fmt.Println()

	// Filter
	fmt.Println("[6] DRA Filter")
	node := &Node{Name: "gpu-node-1"}
	filterStatus := draPlugin.Filter(state, schedulePod, node)
	if filterStatus != nil {
		fmt.Printf("  Node '%s': FAIL - %s\n", node.Name, filterStatus.Message)
		return
	}
	fmt.Printf("  Node '%s': PASS\n\n", node.Name)

	// Reserve
	fmt.Println("[7] DRA Reserve")
	draPlugin.Reserve(state, schedulePod, node.Name)
	fmt.Println()

	// PreBind
	fmt.Println("[8] DRA PreBind")
	draPlugin.PreBind(state, schedulePod, node.Name)
	fmt.Println()

	// 결과
	fmt.Println("[9] 통합 시나리오 완료")
	fmt.Printf("  흐름: Gate 대기 -> 템플릿 Claim 생성 -> Gate 해제 -> DRA 할당 -> 배치\n")
	fmt.Printf("  Pod '%s' -> Node '%s'\n", schedulePod.Name, node.Name)
	for _, claim := range claims {
		if claim.Status.Allocation != nil {
			for _, d := range claim.Status.Allocation.Devices {
				fmt.Printf("  할당 GPU: %s/%s/%s\n", d.Driver, d.Pool, d.Device)
			}
		}
	}
}
