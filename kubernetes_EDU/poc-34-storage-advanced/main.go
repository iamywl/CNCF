// poc-34-storage-advanced: CSI Migration 상태 머신, In-tree→CSI 볼륨 변환,
// Ephemeral Volume PVC 생성 라이프사이클, Owner Reference 기반 GC,
// Volume Lifecycle Mode 결정을 시뮬레이션한다.
//
// 실행: go run main.go
// 외부 의존성 없이 Go 표준 라이브러리만 사용한다.
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. CSI Migration 상태 머신
// =============================================================================

// MigrationPhase는 CSI Migration의 3단계를 나타낸다.
// 실제 Kubernetes에서는 Feature Gate로 제어된다.
type MigrationPhase int

const (
	PhaseAlpha    MigrationPhase = iota // In-tree만 사용
	PhaseBeta                           // CSI 변환 + In-tree 동시 등록 (롤백 가능)
	PhaseGA                             // In-tree 완전 제거 (롤백 불가)
)

func (p MigrationPhase) String() string {
	switch p {
	case PhaseAlpha:
		return "Alpha (In-tree only)"
	case PhaseBeta:
		return "Beta (CSI + In-tree, rollback possible)"
	case PhaseGA:
		return "GA (CSI only, no rollback)"
	default:
		return "Unknown"
	}
}

// PluginMigrationState는 각 In-tree 플러그인의 마이그레이션 상태를 추적한다.
// 실제 소스: pkg/volume/csimigration/plugin_manager.go 의 PluginManager 구조체
type PluginMigrationState struct {
	InTreeName    string         // 예: "kubernetes.io/aws-ebs"
	CSIDriverName string         // 예: "ebs.csi.aws.com"
	Phase         MigrationPhase // 현재 마이그레이션 단계
}

// PluginManager는 In-tree 플러그인의 마이그레이션 상태를 관리한다.
// 실제 소스: pkg/volume/csimigration/plugin_manager.go (라인 36-39)
type PluginManager struct {
	mu      sync.RWMutex
	plugins map[string]*PluginMigrationState // key: InTreeName
}

// NewPluginManager는 지원 플러그인 목록으로 PluginManager를 생성한다.
func NewPluginManager() *PluginManager {
	pm := &PluginManager{
		plugins: make(map[string]*PluginMigrationState),
	}

	// 실제 Kubernetes에서 지원하는 7개 플러그인 등록
	// 소스: pkg/volume/csimigration/plugin_manager.go (라인 59-76)
	supportedPlugins := []struct {
		inTree string
		csi    string
	}{
		{"kubernetes.io/aws-ebs", "ebs.csi.aws.com"},
		{"kubernetes.io/gce-pd", "pd.csi.storage.gke.io"},
		{"kubernetes.io/azure-disk", "disk.csi.azure.com"},
		{"kubernetes.io/azure-file", "file.csi.azure.com"},
		{"kubernetes.io/cinder", "cinder.csi.openstack.org"},
		{"kubernetes.io/vsphere-volume", "csi.vsphere.vmware.com"},
		{"kubernetes.io/portworx-volume", "pxd.portworx.com"},
	}

	for _, p := range supportedPlugins {
		pm.plugins[p.inTree] = &PluginMigrationState{
			InTreeName:    p.inTree,
			CSIDriverName: p.csi,
			Phase:         PhaseAlpha, // 초기 상태
		}
	}

	return pm
}

// IsMigrationEnabledForPlugin은 플러그인의 마이그레이션이 활성화되었는지 확인한다.
// 실제 소스: pkg/volume/csimigration/plugin_manager.go (라인 81-102)
func (pm *PluginManager) IsMigrationEnabledForPlugin(pluginName string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	state, ok := pm.plugins[pluginName]
	if !ok {
		return false
	}
	return state.Phase >= PhaseBeta
}

// IsMigrationCompleteForPlugin은 마이그레이션이 완전히 완료되었는지 확인한다.
// 실제 소스: pkg/volume/csimigration/plugin_manager.go (라인 52-77)
func (pm *PluginManager) IsMigrationCompleteForPlugin(pluginName string) bool {
	if !pm.IsMigrationEnabledForPlugin(pluginName) {
		return false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	state := pm.plugins[pluginName]
	return state.Phase == PhaseGA
}

// IsMigratable은 볼륨 스펙이 마이그레이션 가능한지 판단한다.
// 실제 소스: pkg/volume/csimigration/plugin_manager.go (라인 107-118)
func (pm *PluginManager) IsMigratable(inTreePluginName string) bool {
	if inTreePluginName == "" {
		return false
	}
	return pm.IsMigrationEnabledForPlugin(inTreePluginName)
}

// AdvancePhase는 플러그인의 마이그레이션 단계를 진행시킨다.
func (pm *PluginManager) AdvancePhase(pluginName string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	state, ok := pm.plugins[pluginName]
	if !ok {
		return fmt.Errorf("unknown plugin: %s", pluginName)
	}
	if state.Phase >= PhaseGA {
		return fmt.Errorf("plugin %s is already at GA, cannot advance further", pluginName)
	}
	state.Phase++
	return nil
}

// GetCSIDriverName은 In-tree 플러그인 이름으로 CSI 드라이버 이름을 조회한다.
func (pm *PluginManager) GetCSIDriverName(pluginName string) (string, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	state, ok := pm.plugins[pluginName]
	if !ok {
		return "", fmt.Errorf("unknown plugin: %s", pluginName)
	}
	return state.CSIDriverName, nil
}

// =============================================================================
// 2. In-tree → CSI 볼륨 스펙 변환
// =============================================================================

// VolumeSpec은 Kubernetes 볼륨 스펙을 단순화한 것이다.
type VolumeSpec struct {
	// In-tree 볼륨 필드
	InTreeDriver string            // 예: "kubernetes.io/aws-ebs"
	VolumeID     string            // 예: "vol-0123456789abcdef0"
	FSType       string            // 예: "ext4"
	Attributes   map[string]string // 추가 속성

	// CSI 볼륨 필드 (변환 후 채워짐)
	CSIDriver       string            // 예: "ebs.csi.aws.com"
	CSIVolumeHandle string            // 예: "vol-0123456789abcdef0"
	CSIFSType       string            // 예: "ext4"
	CSIAttributes   map[string]string // 볼륨 속성

	// 메타데이터
	Migrated     bool // TranslateInTreeSpecToCSI에서 설정
	InlineVolume bool // Inline 볼륨 여부
	ReadOnly     bool
}

// TranslateInTreeSpecToCSI는 In-tree 볼륨 스펙을 CSI로 변환한다.
// 실제 소스: pkg/volume/csimigration/plugin_manager.go (라인 129-150)
func TranslateInTreeSpecToCSI(pm *PluginManager, spec *VolumeSpec) (*VolumeSpec, error) {
	if spec == nil {
		return nil, fmt.Errorf("volume spec is nil")
	}

	if spec.InTreeDriver == "" {
		return nil, fmt.Errorf("not a valid in-tree volume spec")
	}

	// CSI 드라이버 이름 조회
	csiDriver, err := pm.GetCSIDriverName(spec.InTreeDriver)
	if err != nil {
		return nil, fmt.Errorf("failed to get CSI driver name: %v", err)
	}

	// 변환된 스펙 생성
	translated := &VolumeSpec{
		CSIDriver:       csiDriver,
		CSIVolumeHandle: normalizeVolumeID(spec.InTreeDriver, spec.VolumeID),
		CSIFSType:       spec.FSType,
		CSIAttributes:   make(map[string]string),
		Migrated:        true, // 마이그레이션된 볼륨임을 표시
		InlineVolume:    spec.InlineVolume,
		ReadOnly:        spec.ReadOnly,
	}

	// 속성 복사
	for k, v := range spec.Attributes {
		translated.CSIAttributes[k] = v
	}

	return translated, nil
}

// normalizeVolumeID는 In-tree 볼륨 ID를 CSI 형식으로 정규화한다.
// 예: AWS EBS의 경우 "aws://us-east-1a/vol-xxx" -> "vol-xxx"
func normalizeVolumeID(driver, volumeID string) string {
	if driver == "kubernetes.io/aws-ebs" && strings.HasPrefix(volumeID, "aws://") {
		parts := strings.Split(volumeID, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	return volumeID
}

// =============================================================================
// 3. Ephemeral Volume PVC 생성 라이프사이클
// =============================================================================

// PersistentVolumeClaim은 PVC를 단순화한 것이다.
type PersistentVolumeClaim struct {
	Name            string
	Namespace       string
	StorageClass    string
	RequestSize     string
	OwnerReferences []OwnerReference
	Labels          map[string]string
	Annotations     map[string]string
	BoundPVName     string // 바인딩된 PV 이름
}

// OwnerReference는 리소스의 소유자를 나타낸다.
type OwnerReference struct {
	APIVersion         string
	Kind               string
	Name               string
	UID                string
	Controller         bool
	BlockOwnerDeletion bool
}

// Pod는 Pod를 단순화한 것이다.
type Pod struct {
	Name      string
	Namespace string
	UID       string
	Volumes   []Volume
	Deleted   bool
}

// Volume은 Pod의 볼륨 스펙을 나타낸다.
type Volume struct {
	Name      string
	Ephemeral *EphemeralVolumeSource // nil이면 Ephemeral이 아님
}

// EphemeralVolumeSource는 Ephemeral 볼륨 소스를 나타낸다.
type EphemeralVolumeSource struct {
	StorageClass string
	RequestSize  string
	Labels       map[string]string
	Annotations  map[string]string
}

// VolumeClaimName은 Ephemeral 볼륨의 PVC 이름을 결정한다.
// 실제 소스: staging/src/k8s.io/component-helpers/storage/ephemeral/ephemeral.go (라인 41-42)
func VolumeClaimName(podName, volumeName string) string {
	return podName + "-" + volumeName
}

// VolumeIsForPod는 PVC가 해당 Pod를 위해 생성된 것인지 확인한다.
// 실제 소스: staging/src/k8s.io/component-helpers/storage/ephemeral/ephemeral.go (라인 49-57)
func VolumeIsForPod(pod *Pod, pvc *PersistentVolumeClaim) error {
	if pvc.Namespace != pod.Namespace {
		return fmt.Errorf("PVC %s/%s was not created for pod %s/%s (namespace mismatch)",
			pvc.Namespace, pvc.Name, pod.Namespace, pod.Name)
	}
	// Owner Reference에서 Pod UID 확인
	for _, ref := range pvc.OwnerReferences {
		if ref.Kind == "Pod" && ref.Name == pod.Name && ref.UID == pod.UID && ref.Controller {
			return nil
		}
	}
	return fmt.Errorf("PVC %s/%s was not created for pod %s/%s (pod is not owner)",
		pvc.Namespace, pvc.Name, pod.Namespace, pod.Name)
}

// EphemeralController는 Ephemeral Volume Controller를 시뮬레이션한다.
// 실제 소스: pkg/controller/volume/ephemeral/controller.go (라인 51-76)
type EphemeralController struct {
	mu   sync.Mutex
	pvcs map[string]*PersistentVolumeClaim // key: "namespace/name"
	pods map[string]*Pod                   // key: "namespace/name"

	// PodPVC 인덱스: PVC 이름 -> Pod 이름 매핑
	pvcToPod map[string]string // key: "namespace/pvc-name", value: "namespace/pod-name"

	// 메트릭
	createAttempts int
	createFailures int

	// 이벤트 로그
	events []string
}

// NewEphemeralController는 Ephemeral Volume Controller를 생성한다.
func NewEphemeralController() *EphemeralController {
	return &EphemeralController{
		pvcs:     make(map[string]*PersistentVolumeClaim),
		pods:     make(map[string]*Pod),
		pvcToPod: make(map[string]string),
	}
}

// HandlePodAdd는 Pod 생성 이벤트를 처리한다.
// 실제 소스: enqueuePod() -> syncHandler() -> handleVolume()
func (ec *EphemeralController) HandlePodAdd(pod *Pod) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	if pod.Deleted {
		ec.logEvent("SKIP", "Pod %s/%s is being deleted, ignoring", pod.Namespace, pod.Name)
		return
	}

	// Pod 등록
	podKey := pod.Namespace + "/" + pod.Name
	ec.pods[podKey] = pod

	// Ephemeral 볼륨이 있는지 확인
	hasEphemeral := false
	for _, vol := range pod.Volumes {
		if vol.Ephemeral != nil {
			hasEphemeral = true
			break
		}
	}

	if !hasEphemeral {
		ec.logEvent("SKIP", "Pod %s/%s has no ephemeral volumes", pod.Namespace, pod.Name)
		return
	}

	ec.logEvent("ENQUEUE", "Pod %s/%s has ephemeral volumes, processing", pod.Namespace, pod.Name)

	// 각 볼륨에 대해 handleVolume 호출
	for _, vol := range pod.Volumes {
		ec.handleVolume(pod, vol)
	}
}

// handleVolume은 개별 볼륨의 PVC 생성을 처리한다.
// 실제 소스: pkg/controller/volume/ephemeral/controller.go (라인 254-302)
func (ec *EphemeralController) handleVolume(pod *Pod, vol Volume) {
	if vol.Ephemeral == nil {
		return // Ephemeral이 아닌 볼륨은 무시
	}

	// 1. PVC 이름 결정
	pvcName := VolumeClaimName(pod.Name, vol.Name)
	pvcKey := pod.Namespace + "/" + pvcName

	// 2. PVC 존재 여부 확인
	if existingPVC, ok := ec.pvcs[pvcKey]; ok {
		// 이미 존재하면 Owner 확인
		if err := VolumeIsForPod(pod, existingPVC); err != nil {
			ec.logEvent("ERROR", "PVC name conflict: %v", err)
			return
		}
		ec.logEvent("EXISTS", "PVC %s already exists for pod %s/%s",
			pvcName, pod.Namespace, pod.Name)
		return
	}

	// 3. PVC 생성
	ec.createAttempts++
	pvc := &PersistentVolumeClaim{
		Name:         pvcName,
		Namespace:    pod.Namespace,
		StorageClass: vol.Ephemeral.StorageClass,
		RequestSize:  vol.Ephemeral.RequestSize,
		OwnerReferences: []OwnerReference{
			{
				APIVersion:         "v1",
				Kind:               "Pod",
				Name:               pod.Name,
				UID:                pod.UID,
				Controller:         true,
				BlockOwnerDeletion: true,
			},
		},
		Labels:      vol.Ephemeral.Labels,
		Annotations: vol.Ephemeral.Annotations,
	}

	ec.pvcs[pvcKey] = pvc
	podKey := pod.Namespace + "/" + pod.Name
	ec.pvcToPod[pvcKey] = podKey

	ec.logEvent("CREATE", "Created PVC %s/%s for pod %s (owner: %s, UID: %s)",
		pod.Namespace, pvcName, pod.Name, pod.Name, pod.UID)
}

// HandlePVCDelete는 PVC 삭제 이벤트를 처리한다 (자가 치유).
// 실제 소스: pkg/controller/volume/ephemeral/controller.go (라인 148-168)
func (ec *EphemeralController) HandlePVCDelete(namespace, pvcName string) {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	pvcKey := namespace + "/" + pvcName

	// PVC 삭제
	delete(ec.pvcs, pvcKey)

	// PodPVC 인덱서로 관련 Pod 찾기
	podKey, ok := ec.pvcToPod[pvcKey]
	if !ok {
		ec.logEvent("DELETE", "PVC %s deleted, no associated pod found", pvcKey)
		return
	}

	pod, ok := ec.pods[podKey]
	if !ok || pod.Deleted {
		ec.logEvent("DELETE", "PVC %s deleted, pod %s is gone or deleted", pvcKey, podKey)
		delete(ec.pvcToPod, pvcKey)
		return
	}

	ec.logEvent("HEAL", "PVC %s deleted, re-creating for pod %s (self-healing)",
		pvcKey, podKey)

	// Pod를 다시 enqueue하여 PVC 재생성
	for _, vol := range pod.Volumes {
		ec.handleVolume(pod, vol)
	}
}

func (ec *EphemeralController) logEvent(action, format string, args ...interface{}) {
	msg := fmt.Sprintf("[%-7s] %s", action, fmt.Sprintf(format, args...))
	ec.events = append(ec.events, msg)
}

// =============================================================================
// 4. Owner Reference 기반 Garbage Collection
// =============================================================================

// GarbageCollector는 Owner Reference 기반 GC를 시뮬레이션한다.
type GarbageCollector struct {
	mu     sync.Mutex
	events []string
}

// NewGarbageCollector는 GC를 생성한다.
func NewGarbageCollector() *GarbageCollector {
	return &GarbageCollector{}
}

// ProcessPodDeletion은 Pod 삭제 시 종속 PVC를 정리한다.
func (gc *GarbageCollector) ProcessPodDeletion(pod *Pod, pvcs []*PersistentVolumeClaim) []string {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	deletedPVCs := []string{}

	gc.logEvent("GC", "Processing deletion of pod %s/%s (UID: %s)",
		pod.Namespace, pod.Name, pod.UID)

	// 1. Pod의 dependents(종속 리소스) 탐색
	for _, pvc := range pvcs {
		isDependent := false
		hasBlockOwnerDeletion := false

		for _, ref := range pvc.OwnerReferences {
			if ref.Kind == "Pod" && ref.Name == pod.Name && ref.UID == pod.UID {
				isDependent = true
				hasBlockOwnerDeletion = ref.BlockOwnerDeletion
				break
			}
		}

		if isDependent {
			gc.logEvent("GC", "  Found dependent PVC: %s/%s (BlockOwnerDeletion=%v)",
				pvc.Namespace, pvc.Name, hasBlockOwnerDeletion)

			if hasBlockOwnerDeletion {
				gc.logEvent("GC", "  Deleting PVC %s/%s (foreground deletion)",
					pvc.Namespace, pvc.Name)
			} else {
				gc.logEvent("GC", "  Deleting PVC %s/%s (background deletion)",
					pvc.Namespace, pvc.Name)
			}
			deletedPVCs = append(deletedPVCs, pvc.Name)
		}
	}

	if len(deletedPVCs) == 0 {
		gc.logEvent("GC", "  No dependent PVCs found")
	} else {
		gc.logEvent("GC", "  Total %d PVC(s) deleted, pod deletion complete",
			len(deletedPVCs))
	}

	return deletedPVCs
}

func (gc *GarbageCollector) logEvent(action, format string, args ...interface{}) {
	msg := fmt.Sprintf("[%-7s] %s", action, fmt.Sprintf(format, args...))
	gc.events = append(gc.events, msg)
}

// =============================================================================
// 5. Volume Lifecycle Mode 결정
// =============================================================================

// VolumeLifecycleMode는 볼륨의 라이프사이클 모드를 나타낸다.
// 실제 소스: staging/src/k8s.io/api/storage/v1/types.go (라인 520-555)
type VolumeLifecycleMode string

const (
	VolumeLifecyclePersistent VolumeLifecycleMode = "Persistent"
	VolumeLifecycleEphemeral  VolumeLifecycleMode = "Ephemeral"
)

// CSIDriverSpec은 CSI 드라이버의 스펙을 나타낸다.
// 실제 소스: staging/src/k8s.io/api/storage/v1/types.go (라인 299-379+)
type CSIDriverSpec struct {
	Name                 string
	AttachRequired       bool
	PodInfoOnMount       bool
	VolumeLifecycleModes []VolumeLifecycleMode
	StorageCapacity      bool
}

// CSIVolumeSpec은 볼륨의 CSI 소스를 단순화한 것이다.
type CSIVolumeSpec struct {
	Driver       string
	VolumeHandle string
	IsInline     bool // true: CSIVolumeSource (Ephemeral), false: CSIPersistentVolumeSource (Persistent)
}

// GetVolumeLifecycleMode는 볼륨 스펙 기반으로 라이프사이클 모드를 결정한다.
// 실제 소스: pkg/volume/csi/csi_plugin.go (라인 892-904)
func GetVolumeLifecycleMode(spec *CSIVolumeSpec) VolumeLifecycleMode {
	if spec.IsInline {
		return VolumeLifecycleEphemeral
	}
	return VolumeLifecyclePersistent
}

// CanAttach는 볼륨의 Attach 가능 여부를 결정한다.
// 실제 소스: pkg/volume/csi/csi_plugin.go (라인 675-699)
func CanAttach(spec *CSIVolumeSpec, driver *CSIDriverSpec) bool {
	mode := GetVolumeLifecycleMode(spec)
	if mode == VolumeLifecycleEphemeral {
		return false // Ephemeral 볼륨은 Attach 불필요
	}
	return driver.AttachRequired
}

// CanDeviceMount는 볼륨의 디바이스 마운트 가능 여부를 결정한다.
// 실제 소스: pkg/volume/csi/csi_plugin.go (라인 702-715)
func CanDeviceMount(spec *CSIVolumeSpec, driver *CSIDriverSpec) bool {
	mode := GetVolumeLifecycleMode(spec)
	if mode == VolumeLifecycleEphemeral {
		return false // Ephemeral 볼륨은 DeviceMount 불필요
	}
	return true // Persistent 볼륨은 DeviceMount 지원
}

// =============================================================================
// 메인 함수: 전체 시뮬레이션 실행
// =============================================================================

func main() {
	fmt.Println("=================================================================")
	fmt.Println(" PoC-34: CSI Migration & Ephemeral Volume 심화 시뮬레이션")
	fmt.Println("=================================================================")
	fmt.Println()

	demoCSIMigrationStateMachine()
	demoInTreeToCSITranslation()
	demoEphemeralVolumeLifecycle()
	demoOwnerReferenceGC()
	demoVolumeLifecycleMode()
}

func demoCSIMigrationStateMachine() {
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println(" [1] CSI Migration 상태 머신")
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println()

	pm := NewPluginManager()
	pluginName := "kubernetes.io/aws-ebs"

	fmt.Printf("플러그인: %s\n\n", pluginName)

	// Phase 1: Alpha
	fmt.Printf("Phase 1 (Alpha):\n")
	fmt.Printf("  IsMigrationEnabled:  %v\n", pm.IsMigrationEnabledForPlugin(pluginName))
	fmt.Printf("  IsMigrationComplete: %v\n", pm.IsMigrationCompleteForPlugin(pluginName))
	fmt.Printf("  IsMigratable:        %v\n", pm.IsMigratable(pluginName))
	fmt.Println()

	// Phase 2: Beta
	pm.AdvancePhase(pluginName)
	fmt.Printf("Phase 2 (Beta) - Feature Gate 활성화:\n")
	fmt.Printf("  IsMigrationEnabled:  %v  (CSI 변환 시작)\n", pm.IsMigrationEnabledForPlugin(pluginName))
	fmt.Printf("  IsMigrationComplete: %v (In-tree도 아직 등록)\n", pm.IsMigrationCompleteForPlugin(pluginName))
	fmt.Printf("  IsMigratable:        %v\n", pm.IsMigratable(pluginName))
	fmt.Println()

	// Phase 3: GA
	pm.AdvancePhase(pluginName)
	fmt.Printf("Phase 3 (GA) - Feature Gate 잠금:\n")
	fmt.Printf("  IsMigrationEnabled:  %v  (항상 활성화)\n", pm.IsMigrationEnabledForPlugin(pluginName))
	fmt.Printf("  IsMigrationComplete: %v (In-tree 완전 제거)\n", pm.IsMigrationCompleteForPlugin(pluginName))
	fmt.Printf("  IsMigratable:        %v\n", pm.IsMigratable(pluginName))
	fmt.Println()

	// 지원하지 않는 플러그인
	unknownPlugin := "kubernetes.io/nfs"
	fmt.Printf("미지원 플러그인 (%s):\n", unknownPlugin)
	fmt.Printf("  IsMigrationEnabled:  %v\n", pm.IsMigrationEnabledForPlugin(unknownPlugin))
	fmt.Printf("  IsMigrationComplete: %v\n", pm.IsMigrationCompleteForPlugin(unknownPlugin))
	fmt.Printf("  IsMigratable:        %v\n", pm.IsMigratable(unknownPlugin))
	fmt.Println()
}

func demoInTreeToCSITranslation() {
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println(" [2] In-tree -> CSI 볼륨 스펙 변환")
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println()

	pm := NewPluginManager()
	// GA 상태로 전환
	for _, name := range []string{
		"kubernetes.io/aws-ebs", "kubernetes.io/gce-pd",
		"kubernetes.io/azure-disk", "kubernetes.io/azure-file",
	} {
		pm.AdvancePhase(name)
		pm.AdvancePhase(name)
	}

	testCases := []struct {
		name string
		spec *VolumeSpec
	}{
		{
			name: "AWS EBS PV (일반 볼륨 ID)",
			spec: &VolumeSpec{
				InTreeDriver: "kubernetes.io/aws-ebs",
				VolumeID:     "vol-0123456789abcdef0",
				FSType:       "ext4",
				Attributes:   map[string]string{"partition": "0"},
			},
		},
		{
			name: "AWS EBS PV (aws:// 형식 볼륨 ID)",
			spec: &VolumeSpec{
				InTreeDriver: "kubernetes.io/aws-ebs",
				VolumeID:     "aws://us-east-1a/vol-abcdef0123456789",
				FSType:       "xfs",
				Attributes:   map[string]string{},
			},
		},
		{
			name: "GCE PD PV",
			spec: &VolumeSpec{
				InTreeDriver: "kubernetes.io/gce-pd",
				VolumeID:     "projects/my-project/zones/us-central1-a/disks/my-disk",
				FSType:       "ext4",
				Attributes:   map[string]string{},
			},
		},
		{
			name: "Azure Disk Inline Volume",
			spec: &VolumeSpec{
				InTreeDriver: "kubernetes.io/azure-disk",
				VolumeID:     "/subscriptions/sub-id/resourceGroups/rg/providers/Microsoft.Compute/disks/my-disk",
				FSType:       "ext4",
				InlineVolume: true,
				Attributes:   map[string]string{"cachingMode": "ReadOnly"},
			},
		},
	}

	for _, tc := range testCases {
		fmt.Printf("변환: %s\n", tc.name)
		fmt.Printf("  원본:\n")
		fmt.Printf("    In-tree Driver: %s\n", tc.spec.InTreeDriver)
		fmt.Printf("    Volume ID:      %s\n", tc.spec.VolumeID)
		fmt.Printf("    FS Type:        %s\n", tc.spec.FSType)
		fmt.Printf("    Inline:         %v\n", tc.spec.InlineVolume)

		translated, err := TranslateInTreeSpecToCSI(pm, tc.spec)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
		} else {
			fmt.Printf("  변환 결과:\n")
			fmt.Printf("    CSI Driver:     %s\n", translated.CSIDriver)
			fmt.Printf("    Volume Handle:  %s\n", translated.CSIVolumeHandle)
			fmt.Printf("    FS Type:        %s\n", translated.CSIFSType)
			fmt.Printf("    Migrated:       %v\n", translated.Migrated)
			fmt.Printf("    Inline:         %v\n", translated.InlineVolume)
			if len(translated.CSIAttributes) > 0 {
				fmt.Printf("    Attributes:     %v\n", translated.CSIAttributes)
			}
		}
		fmt.Println()
	}
}

func demoEphemeralVolumeLifecycle() {
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println(" [3] Ephemeral Volume PVC 생성 라이프사이클")
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println()

	ec := NewEphemeralController()

	// 시나리오 1: Ephemeral 볼륨이 있는 Pod 생성
	pod1 := &Pod{
		Name:      "ml-worker-1",
		Namespace: "default",
		UID:       "uid-pod-001",
		Volumes: []Volume{
			{
				Name: "cache",
				Ephemeral: &EphemeralVolumeSource{
					StorageClass: "fast-ssd",
					RequestSize:  "10Gi",
					Labels:       map[string]string{"app": "ml"},
				},
			},
			{
				Name: "scratch",
				Ephemeral: &EphemeralVolumeSource{
					StorageClass: "standard",
					RequestSize:  "50Gi",
				},
			},
			{
				Name:      "config",
				Ephemeral: nil, // Ephemeral이 아닌 볼륨
			},
		},
	}

	fmt.Println("[시나리오 1] Pod 생성 - Ephemeral 볼륨 PVC 자동 생성")
	ec.HandlePodAdd(pod1)

	// 시나리오 2: 동일 Pod 재처리 (멱등성 확인)
	fmt.Println("\n[시나리오 2] 동일 Pod 재처리 (멱등성 확인)")
	ec.HandlePodAdd(pod1)

	// 시나리오 3: Ephemeral이 없는 Pod
	pod2 := &Pod{
		Name:      "web-server",
		Namespace: "default",
		UID:       "uid-pod-002",
		Volumes: []Volume{
			{Name: "data", Ephemeral: nil},
		},
	}
	fmt.Println("\n[시나리오 3] Ephemeral 볼륨이 없는 Pod")
	ec.HandlePodAdd(pod2)

	// 시나리오 4: PVC 삭제 후 자가 치유
	fmt.Println("\n[시나리오 4] PVC 삭제 후 자가 치유 (self-healing)")
	ec.HandlePVCDelete("default", "ml-worker-1-cache")

	// 시나리오 5: 삭제 중인 Pod
	pod3 := &Pod{
		Name:      "terminating-pod",
		Namespace: "default",
		UID:       "uid-pod-003",
		Volumes: []Volume{
			{Name: "tmp", Ephemeral: &EphemeralVolumeSource{StorageClass: "standard", RequestSize: "1Gi"}},
		},
		Deleted: true,
	}
	fmt.Println("\n[시나리오 5] 삭제 중인 Pod (무시해야 함)")
	ec.HandlePodAdd(pod3)

	// 이벤트 로그 출력
	fmt.Println("\n--- 전체 이벤트 로그 ---")
	for _, event := range ec.events {
		fmt.Println("  " + event)
	}

	fmt.Printf("\n메트릭: createAttempts=%d, createFailures=%d\n\n",
		ec.createAttempts, ec.createFailures)
}

func demoOwnerReferenceGC() {
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println(" [4] Owner Reference 기반 Garbage Collection")
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println()

	gc := NewGarbageCollector()

	// Pod와 종속 PVC 준비
	pod := &Pod{
		Name:      "ml-worker-1",
		Namespace: "default",
		UID:       "uid-pod-001",
	}

	pvcs := []*PersistentVolumeClaim{
		{
			Name:      "ml-worker-1-cache",
			Namespace: "default",
			OwnerReferences: []OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Pod",
					Name:               "ml-worker-1",
					UID:                "uid-pod-001",
					Controller:         true,
					BlockOwnerDeletion: true,
				},
			},
		},
		{
			Name:      "ml-worker-1-scratch",
			Namespace: "default",
			OwnerReferences: []OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Pod",
					Name:               "ml-worker-1",
					UID:                "uid-pod-001",
					Controller:         true,
					BlockOwnerDeletion: true,
				},
			},
		},
		{
			Name:      "shared-data",
			Namespace: "default",
			OwnerReferences: []OwnerReference{
				{
					Kind: "StatefulSet",
					Name: "data-processor",
					UID:  "uid-sts-001",
				},
			},
		},
	}

	fmt.Println("[시나리오] Pod 삭제 시 종속 PVC 정리")
	fmt.Printf("  Pod: %s/%s (UID: %s)\n", pod.Namespace, pod.Name, pod.UID)
	fmt.Printf("  총 PVC 수: %d\n\n", len(pvcs))

	deletedPVCs := gc.ProcessPodDeletion(pod, pvcs)

	fmt.Println("\n--- GC 이벤트 로그 ---")
	for _, event := range gc.events {
		fmt.Println("  " + event)
	}

	fmt.Printf("\n결과: %d개 PVC 삭제됨: %v\n", len(deletedPVCs), deletedPVCs)
	fmt.Printf("shared-data PVC는 StatefulSet 소유이므로 유지됨\n\n")
}

func demoVolumeLifecycleMode() {
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println(" [5] Volume Lifecycle Mode 결정")
	fmt.Println("-------------------------------------------------------------------")
	fmt.Println()

	// CSI 드라이버 정의
	drivers := map[string]*CSIDriverSpec{
		"ebs.csi.aws.com": {
			Name:                 "ebs.csi.aws.com",
			AttachRequired:       true,
			PodInfoOnMount:       false,
			VolumeLifecycleModes: []VolumeLifecycleMode{VolumeLifecyclePersistent},
		},
		"secrets-store.csi.k8s.io": {
			Name:                 "secrets-store.csi.k8s.io",
			AttachRequired:       false,
			PodInfoOnMount:       true,
			VolumeLifecycleModes: []VolumeLifecycleMode{VolumeLifecycleEphemeral},
		},
		"example-driver.csi.k8s.io": {
			Name:                 "example-driver.csi.k8s.io",
			AttachRequired:       true,
			PodInfoOnMount:       true,
			VolumeLifecycleModes: []VolumeLifecycleMode{VolumeLifecyclePersistent, VolumeLifecycleEphemeral},
		},
	}

	// 볼륨 스펙 테스트 케이스
	testCases := []struct {
		name   string
		spec   *CSIVolumeSpec
		driver string
	}{
		{
			name:   "EBS PV (Persistent)",
			spec:   &CSIVolumeSpec{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-xxx", IsInline: false},
			driver: "ebs.csi.aws.com",
		},
		{
			name:   "Secrets Store (Ephemeral Inline)",
			spec:   &CSIVolumeSpec{Driver: "secrets-store.csi.k8s.io", IsInline: true},
			driver: "secrets-store.csi.k8s.io",
		},
		{
			name:   "Example Driver - PV (Persistent)",
			spec:   &CSIVolumeSpec{Driver: "example-driver.csi.k8s.io", VolumeHandle: "pv-123", IsInline: false},
			driver: "example-driver.csi.k8s.io",
		},
		{
			name:   "Example Driver - Inline (Ephemeral)",
			spec:   &CSIVolumeSpec{Driver: "example-driver.csi.k8s.io", IsInline: true},
			driver: "example-driver.csi.k8s.io",
		},
	}

	fmt.Printf("%-40s %-12s %-10s %-14s\n", "볼륨", "모드", "CanAttach", "CanDeviceMount")
	fmt.Println(strings.Repeat("-", 80))

	for _, tc := range testCases {
		mode := GetVolumeLifecycleMode(tc.spec)
		driverSpec := drivers[tc.driver]
		canAttach := CanAttach(tc.spec, driverSpec)
		canDevMount := CanDeviceMount(tc.spec, driverSpec)

		fmt.Printf("%-40s %-12s %-10v %-14v\n",
			tc.name, mode, canAttach, canDevMount)
	}

	fmt.Println()
	fmt.Println("마운트 파이프라인 비교:")
	fmt.Println()
	fmt.Println("  Persistent 모드:")
	fmt.Println("    ControllerPublish -> Attach -> NodeStage -> NodePublish")
	fmt.Println("    (CanAttach=true)    (AD Ctrl)  (CanDeviceMount=true)")
	fmt.Println()
	fmt.Println("  Ephemeral 모드:")
	fmt.Println("    NodePublish (직접)")
	fmt.Println("    (CanAttach=false, CanDeviceMount=false)")
	fmt.Println()

	// 드라이버별 지원 모드 출력
	fmt.Println("등록된 CSI 드라이버:")
	fmt.Println()
	fmt.Printf("  %-35s %-10s %-15s %-15s\n",
		"드라이버", "Attach", "PodInfo", "지원 모드")
	fmt.Println("  " + strings.Repeat("-", 75))
	for _, d := range []*CSIDriverSpec{
		drivers["ebs.csi.aws.com"],
		drivers["secrets-store.csi.k8s.io"],
		drivers["example-driver.csi.k8s.io"],
	} {
		modes := make([]string, len(d.VolumeLifecycleModes))
		for i, m := range d.VolumeLifecycleModes {
			modes[i] = string(m)
		}
		fmt.Printf("  %-35s %-10v %-15v %s\n",
			d.Name, d.AttachRequired, d.PodInfoOnMount, strings.Join(modes, ", "))
	}
	fmt.Println()

	_ = time.Now() // 패키지 사용 확인
}
