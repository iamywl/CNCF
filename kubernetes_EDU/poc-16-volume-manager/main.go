package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes PV/PVC 바인딩 및 볼륨 수명주기 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/apis/core/types.go                                  : PersistentVolume, PersistentVolumeClaim, StorageClass
//   - pkg/controller/volume/persistentvolume/pv_controller.go : PersistentVolumeController (syncVolume, syncClaim)
//   - pkg/controller/volume/persistentvolume/index.go         : findBestMatchForClaim, persistentVolumeOrderedIndex
//   - plugin/pkg/admission/storage/persistentvolume/resize/   : 볼륨 확장 admission
//
// PV/PVC 바인딩 원리:
//   1. PersistentVolume(PV): 관리자가 미리 프로비저닝한 스토리지 (또는 동적 프로비저닝)
//   2. PersistentVolumeClaim(PVC): 사용자가 요청하는 스토리지 사양
//   3. PV Controller가 미바인딩 PVC에 적합한 PV를 찾아 바인딩
//   4. 바인딩은 양방향: PV.Spec.ClaimRef = PVC, PVC.Spec.VolumeName = PV.Name
//
// 볼륨 수명주기:
//   Available → Bound → Released → (Retain/Delete/Recycle)
//   - Available: 아직 바인딩 안 됨
//   - Bound: PVC에 바인딩됨
//   - Released: PVC 삭제됨, 데이터 보존 중
//   - Failed: 자동 회수 실패

// =============================================================================
// 1. 데이터 모델 — pkg/apis/core/types.go 재현
// =============================================================================

// PersistentVolumePhase는 PV의 수명주기 단계
// 실제: pkg/apis/core/types.go의 PersistentVolumePhase
type PersistentVolumePhase string

const (
	VolumeAvailable PersistentVolumePhase = "Available"
	VolumeBound     PersistentVolumePhase = "Bound"
	VolumeReleased  PersistentVolumePhase = "Released"
	VolumeFailed    PersistentVolumePhase = "Failed"
)

// PersistentVolumeClaimPhase는 PVC의 수명주기 단계
type PersistentVolumeClaimPhase string

const (
	ClaimPending PersistentVolumeClaimPhase = "Pending"
	ClaimBound   PersistentVolumeClaimPhase = "Bound"
	ClaimLost    PersistentVolumeClaimPhase = "Lost"
)

// AccessMode는 볼륨 접근 모드
// 실제: pkg/apis/core/types.go의 PersistentVolumeAccessMode
type AccessMode string

const (
	ReadWriteOnce AccessMode = "ReadWriteOnce" // 단일 노드 읽기/쓰기
	ReadOnlyMany  AccessMode = "ReadOnlyMany"  // 다중 노드 읽기 전용
	ReadWriteMany AccessMode = "ReadWriteMany" // 다중 노드 읽기/쓰기
)

// ReclaimPolicy는 PV 반환 정책
// 실제: pkg/apis/core/types.go의 PersistentVolumeReclaimPolicy
type ReclaimPolicy string

const (
	ReclaimRetain  ReclaimPolicy = "Retain"  // 데이터 보존 (관리자가 수동 정리)
	ReclaimDelete  ReclaimPolicy = "Delete"  // PV + 스토리지 자동 삭제
	ReclaimRecycle ReclaimPolicy = "Recycle" // rm -rf /volume/* (deprecated)
)

// StorageClass는 동적 프로비저닝을 위한 스토리지 클래스
// 실제: staging/src/k8s.io/api/storage/v1/types.go의 StorageClass
type StorageClass struct {
	Name              string
	Provisioner       string        // 예: kubernetes.io/gce-pd, ebs.csi.aws.com
	ReclaimPolicy     ReclaimPolicy
	VolumeBindingMode string        // Immediate / WaitForFirstConsumer
	Parameters        map[string]string // 프로비저너 전달 파라미터
}

// PersistentVolume는 PV 오브젝트
// 실제: pkg/apis/core/types.go의 PersistentVolume, PersistentVolumeSpec
type PersistentVolume struct {
	Name             string
	CapacityBytes    int64           // 바이트 단위 용량
	AccessModes      []AccessMode
	ReclaimPolicy    ReclaimPolicy
	StorageClassName string          // StorageClass 이름
	Phase            PersistentVolumePhase
	ClaimRef         *ClaimReference // 바인딩된 PVC 참조
	Labels           map[string]string
	VolumeSource     string          // 스토리지 유형 (nfs, csi, hostPath 등)
	CreatedAt        time.Time
}

// ClaimReference는 PV가 참조하는 PVC
// 실제: pkg/apis/core/types.go의 ObjectReference (ClaimRef로 사용)
type ClaimReference struct {
	Namespace string
	Name      string
	UID       string
}

// PersistentVolumeClaim은 PVC 오브젝트
// 실제: pkg/apis/core/types.go의 PersistentVolumeClaim, PersistentVolumeClaimSpec
type PersistentVolumeClaim struct {
	Name             string
	Namespace        string
	UID              string
	RequestBytes     int64           // 요청 용량
	AccessModes      []AccessMode
	StorageClassName string          // 원하는 StorageClass
	VolumeName       string          // 바인딩된 PV 이름
	Phase            PersistentVolumeClaimPhase
	Selector         map[string]string // 레이블 셀렉터 (특정 PV 필터)
}

// =============================================================================
// 2. PV/PVC 바인딩 알고리즘 — pkg/controller/volume/persistentvolume/index.go
// =============================================================================

// VolumeIndex는 PV를 AccessMode로 인덱싱하는 구조체
// 실제: pkg/controller/volume/persistentvolume/index.go의 persistentVolumeOrderedIndex
// Access Mode별로 PV를 그룹화하고, 용량 순으로 정렬하여 최적 매칭을 수행한다.
type VolumeIndex struct {
	mu     sync.RWMutex
	byMode map[string][]*PersistentVolume // accessMode 문자열 → PV 목록
}

func NewVolumeIndex() *VolumeIndex {
	return &VolumeIndex{
		byMode: make(map[string][]*PersistentVolume),
	}
}

// accessModesKey는 AccessMode 집합을 문자열 키로 변환
// 실제: k8s.io/kubernetes/pkg/volume/util/util.go의 GetAccessModesAsString()
func accessModesKey(modes []AccessMode) string {
	sorted := make([]string, len(modes))
	for i, m := range modes {
		sorted[i] = string(m)
	}
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// AddVolume은 PV를 인덱스에 추가한다
func (idx *VolumeIndex) AddVolume(pv *PersistentVolume) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := accessModesKey(pv.AccessModes)
	idx.byMode[key] = append(idx.byMode[key], pv)

	// 용량 순 정렬 (작은 것부터)
	sort.Slice(idx.byMode[key], func(i, j int) bool {
		return idx.byMode[key][i].CapacityBytes < idx.byMode[key][j].CapacityBytes
	})
}

// FindBestMatch는 PVC에 가장 적합한 PV를 찾는다
// 실제: pkg/controller/volume/persistentvolume/index.go의 findBestMatchForClaim()
//
// 매칭 기준 (순서대로):
//   1. AccessModes 호환: PV의 모드가 PVC 요구를 포함해야 함
//   2. StorageClass 일치: 동일한 StorageClass
//   3. 용량: PV 용량 ≥ PVC 요청 용량
//   4. 레이블 셀렉터: PVC의 selector와 PV 레이블 매칭
//   5. 최소 낭비: 조건 만족하는 PV 중 가장 작은 것 선택
//
// 실제 알고리즘 (index.go):
//   allPossibleMatchingAccessModes()로 호환 모드 그룹 찾기
//   → 각 그룹에서 가장 작은 적합 PV 선택
//   → 전체 후보 중 가장 작은 것 반환
func (idx *VolumeIndex) FindBestMatch(pvc *PersistentVolumeClaim) *PersistentVolume {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var bestMatch *PersistentVolume

	// 모든 인덱스 키에서 호환되는 모드 그룹 탐색
	// 실제: allPossibleMatchingAccessModes() — 요청 모드를 모두 포함하는 그룹
	for key, pvs := range idx.byMode {
		if !containsAllModes(key, pvc.AccessModes) {
			continue
		}

		for _, pv := range pvs {
			if !isMatchable(pv, pvc) {
				continue
			}

			// 최소 낭비 선택 (이미 용량순 정렬되어 있으므로 첫 번째 매칭이 최적)
			if bestMatch == nil || pv.CapacityBytes < bestMatch.CapacityBytes {
				bestMatch = pv
			}
		}
	}

	return bestMatch
}

// containsAllModes는 인덱스 키가 요청된 모든 모드를 포함하는지 확인
func containsAllModes(indexKey string, requested []AccessMode) bool {
	for _, mode := range requested {
		if !strings.Contains(indexKey, string(mode)) {
			return false
		}
	}
	return true
}

// isMatchable는 PV가 PVC에 바인딩 가능한지 확인
func isMatchable(pv *PersistentVolume, pvc *PersistentVolumeClaim) bool {
	// Available 상태만
	if pv.Phase != VolumeAvailable {
		return false
	}

	// 용량 충분
	if pv.CapacityBytes < pvc.RequestBytes {
		return false
	}

	// StorageClass 일치
	if pvc.StorageClassName != "" && pv.StorageClassName != pvc.StorageClassName {
		return false
	}

	// 레이블 셀렉터 매칭
	if len(pvc.Selector) > 0 {
		for k, v := range pvc.Selector {
			if pv.Labels[k] != v {
				return false
			}
		}
	}

	return true
}

// =============================================================================
// 3. PV Controller — 바인딩 및 수명주기 관리
// =============================================================================

// PVController는 PV/PVC 바인딩 및 수명주기를 관리한다
// 실제: pkg/controller/volume/persistentvolume/pv_controller.go의 PersistentVolumeController
// 두 개의 핵심 루프:
//   - syncVolume: PV 상태 변경 처리
//   - syncClaim: PVC 상태 변경 처리 → 바인딩 시도
type PVController struct {
	mu             sync.Mutex
	volumes        map[string]*PersistentVolume      // name → PV
	claims         map[string]*PersistentVolumeClaim  // namespace/name → PVC
	storageClasses map[string]*StorageClass
	index          *VolumeIndex
	eventLog       []string
}

func NewPVController() *PVController {
	return &PVController{
		volumes:        make(map[string]*PersistentVolume),
		claims:         make(map[string]*PersistentVolumeClaim),
		storageClasses: make(map[string]*StorageClass),
		index:          NewVolumeIndex(),
	}
}

// AddStorageClass는 StorageClass를 등록한다
func (c *PVController) AddStorageClass(sc *StorageClass) {
	c.storageClasses[sc.Name] = sc
}

// AddVolume은 PV를 추가하고 인덱스에 등록한다
func (c *PVController) AddVolume(pv *PersistentVolume) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pv.Phase = VolumeAvailable
	pv.CreatedAt = time.Now()
	c.volumes[pv.Name] = pv
	c.index.AddVolume(pv)
	c.log(fmt.Sprintf("PV %s 추가됨 (용량: %s, 모드: %v, class: %s)",
		pv.Name, formatBytes(pv.CapacityBytes), pv.AccessModes, pv.StorageClassName))
}

// CreateClaim은 PVC를 생성하고 바인딩을 시도한다
// 실제: syncClaim()에서 PVC 생성 이벤트 처리
func (c *PVController) CreateClaim(pvc *PersistentVolumeClaim) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := pvc.Namespace + "/" + pvc.Name
	pvc.Phase = ClaimPending
	c.claims[key] = pvc
	c.log(fmt.Sprintf("PVC %s 생성됨 (요청: %s, 모드: %v, class: %s)",
		key, formatBytes(pvc.RequestBytes), pvc.AccessModes, pvc.StorageClassName))

	// 바인딩 시도
	c.syncClaim(pvc)
	return key
}

// syncClaim은 PVC에 적합한 PV를 찾아 바인딩한다
// 실제: pv_controller.go의 syncClaim()
// 순서:
//   1. 이미 바인딩된 경우 → 검증
//   2. 정적 프로비저닝: findBestMatchForClaim()으로 기존 PV 매칭
//   3. 동적 프로비저닝: StorageClass로 새 PV 생성
func (c *PVController) syncClaim(pvc *PersistentVolumeClaim) {
	if pvc.Phase == ClaimBound {
		return
	}

	// 1. 정적 프로비저닝: 기존 PV에서 최적 매칭 찾기
	bestPV := c.index.FindBestMatch(pvc)

	if bestPV != nil {
		c.bind(pvc, bestPV)
		return
	}

	// 2. 동적 프로비저닝: StorageClass에서 새 PV 생성
	if pvc.StorageClassName != "" {
		sc, ok := c.storageClasses[pvc.StorageClassName]
		if ok {
			c.dynamicProvision(pvc, sc)
			return
		}
	}

	c.log(fmt.Sprintf("PVC %s/%s: 매칭하는 PV 없음, Pending 유지", pvc.Namespace, pvc.Name))
}

// bind는 PV와 PVC를 양방향으로 바인딩한다
// 실제: pv_controller.go의 bind()
// PV.Spec.ClaimRef = PVC 참조, PVC.Spec.VolumeName = PV 이름
func (c *PVController) bind(pvc *PersistentVolumeClaim, pv *PersistentVolume) {
	// PV → PVC 참조 설정
	pv.ClaimRef = &ClaimReference{
		Namespace: pvc.Namespace,
		Name:      pvc.Name,
		UID:       pvc.UID,
	}
	pv.Phase = VolumeBound

	// PVC → PV 참조 설정
	pvc.VolumeName = pv.Name
	pvc.Phase = ClaimBound

	c.log(fmt.Sprintf("바인딩: PV %s ↔ PVC %s/%s (용량: %s, 요청: %s, 낭비: %s)",
		pv.Name, pvc.Namespace, pvc.Name,
		formatBytes(pv.CapacityBytes), formatBytes(pvc.RequestBytes),
		formatBytes(pv.CapacityBytes-pvc.RequestBytes)))
}

// dynamicProvision은 StorageClass를 사용해 동적으로 PV를 생성한다
// 실제: 외부 provisioner (CSI driver)가 처리하지만, 여기서는 시뮬레이션
func (c *PVController) dynamicProvision(pvc *PersistentVolumeClaim, sc *StorageClass) {
	pvName := fmt.Sprintf("pv-dynamic-%s-%s", pvc.Namespace, pvc.Name)

	pv := &PersistentVolume{
		Name:             pvName,
		CapacityBytes:    pvc.RequestBytes, // 요청 크기 그대로 생성
		AccessModes:      pvc.AccessModes,
		ReclaimPolicy:    sc.ReclaimPolicy,
		StorageClassName: sc.Name,
		Phase:            VolumeAvailable,
		VolumeSource:     fmt.Sprintf("csi/%s", sc.Provisioner),
		CreatedAt:        time.Now(),
	}

	c.volumes[pvName] = pv
	c.log(fmt.Sprintf("동적 프로비저닝: PV %s 생성됨 (StorageClass: %s, provisioner: %s)",
		pvName, sc.Name, sc.Provisioner))

	// 바인딩
	c.bind(pvc, pv)
}

// DeleteClaim은 PVC를 삭제하고 PV의 reclaim 정책을 실행한다
// 실제: pv_controller.go의 syncVolume() — PVC 삭제 후 PV 상태 처리
func (c *PVController) DeleteClaim(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := namespace + "/" + name
	pvc, ok := c.claims[key]
	if !ok {
		return
	}

	c.log(fmt.Sprintf("PVC %s 삭제됨", key))

	// 바인딩된 PV 찾기
	if pvc.VolumeName != "" {
		pv, ok := c.volumes[pvc.VolumeName]
		if ok {
			pv.Phase = VolumeReleased
			c.log(fmt.Sprintf("PV %s → Released", pv.Name))

			// Reclaim 정책 적용
			c.reclaimVolume(pv)
		}
	}

	delete(c.claims, key)
}

// reclaimVolume은 PV의 reclaim 정책을 실행한다
// 실제: pv_controller.go의 reclaimVolume()
func (c *PVController) reclaimVolume(pv *PersistentVolume) {
	switch pv.ReclaimPolicy {
	case ReclaimRetain:
		// 데이터 보존 — 관리자가 수동으로 정리
		c.log(fmt.Sprintf("PV %s: Retain 정책 → 데이터 보존, 관리자 수동 정리 필요", pv.Name))
		// Phase는 Released 유지

	case ReclaimDelete:
		// PV와 스토리지 자동 삭제
		c.log(fmt.Sprintf("PV %s: Delete 정책 → PV 및 스토리지 삭제", pv.Name))
		delete(c.volumes, pv.Name)

	case ReclaimRecycle:
		// Deprecated: rm -rf /volume/* 후 Available로 복원
		c.log(fmt.Sprintf("PV %s: Recycle 정책 → 데이터 삭제 후 Available (deprecated)", pv.Name))
		pv.Phase = VolumeAvailable
		pv.ClaimRef = nil
		c.index.AddVolume(pv)
	}
}

// GetVolumeStatus는 모든 PV의 상태를 반환한다
func (c *PVController) GetVolumeStatus() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var sb strings.Builder
	for _, pv := range c.volumes {
		claimInfo := "(미바인딩)"
		if pv.ClaimRef != nil {
			claimInfo = fmt.Sprintf("→ %s/%s", pv.ClaimRef.Namespace, pv.ClaimRef.Name)
		}
		sb.WriteString(fmt.Sprintf("    PV %-20s  %-10s  %-12s  %s  %s\n",
			pv.Name, formatBytes(pv.CapacityBytes),
			pv.Phase, pv.ReclaimPolicy, claimInfo))
	}
	return sb.String()
}

func (c *PVController) log(msg string) {
	c.eventLog = append(c.eventLog, msg)
	fmt.Printf("    %s\n", msg)
}

// =============================================================================
// 4. 헬퍼 함수
// =============================================================================

func formatBytes(b int64) string {
	const gi = 1024 * 1024 * 1024
	const mi = 1024 * 1024
	if b >= gi {
		return fmt.Sprintf("%dGi", b/gi)
	}
	return fmt.Sprintf("%dMi", b/mi)
}

func giBytes(gi int) int64 {
	return int64(gi) * 1024 * 1024 * 1024
}

func printHeader(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printSubHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// =============================================================================
// 5. 메인 — 데모
// =============================================================================

func main() {
	ctrl := NewPVController()

	// =====================================================================
	// 데모 1: 정적 프로비저닝 — PV/PVC 바인딩
	// =====================================================================
	printHeader("데모 1: 정적 프로비저닝 — 최적 PV 매칭")

	// 다양한 크기의 PV 사전 프로비저닝
	pvs := []*PersistentVolume{
		{Name: "pv-small", CapacityBytes: giBytes(5), AccessModes: []AccessMode{ReadWriteOnce},
			ReclaimPolicy: ReclaimRetain, StorageClassName: "standard", VolumeSource: "nfs"},
		{Name: "pv-medium", CapacityBytes: giBytes(20), AccessModes: []AccessMode{ReadWriteOnce},
			ReclaimPolicy: ReclaimDelete, StorageClassName: "standard", VolumeSource: "nfs"},
		{Name: "pv-large", CapacityBytes: giBytes(100), AccessModes: []AccessMode{ReadWriteOnce, ReadOnlyMany},
			ReclaimPolicy: ReclaimRetain, StorageClassName: "fast", VolumeSource: "ssd",
			Labels: map[string]string{"tier": "premium"}},
		{Name: "pv-shared", CapacityBytes: giBytes(50), AccessModes: []AccessMode{ReadWriteOnce, ReadOnlyMany, ReadWriteMany},
			ReclaimPolicy: ReclaimRetain, StorageClassName: "standard", VolumeSource: "nfs"},
	}

	fmt.Println("PV 프로비저닝:")
	for _, pv := range pvs {
		ctrl.AddVolume(pv)
	}

	printSubHeader("PVC 1: 10Gi RWO standard → pv-medium(20Gi) 매칭 기대")
	ctrl.CreateClaim(&PersistentVolumeClaim{
		Name: "data-claim", Namespace: "default", UID: "uid-1",
		RequestBytes: giBytes(10), AccessModes: []AccessMode{ReadWriteOnce},
		StorageClassName: "standard",
	})

	printSubHeader("PVC 2: 8Gi RWX standard → pv-shared(50Gi) 매칭 기대")
	ctrl.CreateClaim(&PersistentVolumeClaim{
		Name: "shared-claim", Namespace: "default", UID: "uid-2",
		RequestBytes: giBytes(8), AccessModes: []AccessMode{ReadWriteMany},
		StorageClassName: "standard",
	})

	printSubHeader("PVC 3: 200Gi RWO → 매칭 실패 (충분한 PV 없음)")
	ctrl.CreateClaim(&PersistentVolumeClaim{
		Name: "huge-claim", Namespace: "default", UID: "uid-3",
		RequestBytes: giBytes(200), AccessModes: []AccessMode{ReadWriteOnce},
		StorageClassName: "standard",
	})

	// =====================================================================
	// 데모 2: 레이블 셀렉터 매칭
	// =====================================================================
	printHeader("데모 2: 레이블 셀렉터로 특정 PV 선택")

	ctrl2 := NewPVController()
	ctrl2.AddVolume(&PersistentVolume{
		Name: "pv-zone-a", CapacityBytes: giBytes(50), AccessModes: []AccessMode{ReadWriteOnce},
		ReclaimPolicy: ReclaimRetain, StorageClassName: "fast", VolumeSource: "ssd",
		Labels: map[string]string{"zone": "us-east-1a", "tier": "premium"},
	})
	ctrl2.AddVolume(&PersistentVolume{
		Name: "pv-zone-b", CapacityBytes: giBytes(50), AccessModes: []AccessMode{ReadWriteOnce},
		ReclaimPolicy: ReclaimRetain, StorageClassName: "fast", VolumeSource: "ssd",
		Labels: map[string]string{"zone": "us-east-1b", "tier": "premium"},
	})

	printSubHeader("PVC: zone=us-east-1a 셀렉터")
	ctrl2.CreateClaim(&PersistentVolumeClaim{
		Name: "zone-claim", Namespace: "default", UID: "uid-zone",
		RequestBytes: giBytes(10), AccessModes: []AccessMode{ReadWriteOnce},
		StorageClassName: "fast",
		Selector: map[string]string{"zone": "us-east-1a"},
	})

	// =====================================================================
	// 데모 3: 동적 프로비저닝
	// =====================================================================
	printHeader("데모 3: StorageClass를 통한 동적 프로비저닝")

	ctrl3 := NewPVController()

	// StorageClass 등록
	ctrl3.AddStorageClass(&StorageClass{
		Name:          "gp3",
		Provisioner:   "ebs.csi.aws.com",
		ReclaimPolicy: ReclaimDelete,
		VolumeBindingMode: "Immediate",
		Parameters: map[string]string{"type": "gp3", "iops": "3000"},
	})
	ctrl3.AddStorageClass(&StorageClass{
		Name:          "io2",
		Provisioner:   "ebs.csi.aws.com",
		ReclaimPolicy: ReclaimRetain,
		VolumeBindingMode: "WaitForFirstConsumer",
		Parameters: map[string]string{"type": "io2", "iops": "10000"},
	})

	fmt.Println("StorageClass 등록:")
	fmt.Println("    gp3: ebs.csi.aws.com (Delete, Immediate)")
	fmt.Println("    io2: ebs.csi.aws.com (Retain, WaitForFirstConsumer)")

	printSubHeader("PVC: gp3 클래스 50Gi 요청 → 동적 생성")
	ctrl3.CreateClaim(&PersistentVolumeClaim{
		Name: "app-data", Namespace: "production", UID: "uid-dyn-1",
		RequestBytes: giBytes(50), AccessModes: []AccessMode{ReadWriteOnce},
		StorageClassName: "gp3",
	})

	printSubHeader("PVC: io2 클래스 100Gi 요청 → 동적 생성")
	ctrl3.CreateClaim(&PersistentVolumeClaim{
		Name: "db-data", Namespace: "production", UID: "uid-dyn-2",
		RequestBytes: giBytes(100), AccessModes: []AccessMode{ReadWriteOnce},
		StorageClassName: "io2",
	})

	// =====================================================================
	// 데모 4: 볼륨 수명주기 — Reclaim 정책 비교
	// =====================================================================
	printHeader("데모 4: PVC 삭제 시 Reclaim 정책 비교")

	ctrl4 := NewPVController()

	ctrl4.AddVolume(&PersistentVolume{
		Name: "pv-retain", CapacityBytes: giBytes(10), AccessModes: []AccessMode{ReadWriteOnce},
		ReclaimPolicy: ReclaimRetain, StorageClassName: "standard", VolumeSource: "nfs"})
	ctrl4.AddVolume(&PersistentVolume{
		Name: "pv-delete", CapacityBytes: giBytes(10), AccessModes: []AccessMode{ReadWriteOnce},
		ReclaimPolicy: ReclaimDelete, StorageClassName: "standard", VolumeSource: "ebs"})
	ctrl4.AddVolume(&PersistentVolume{
		Name: "pv-recycle", CapacityBytes: giBytes(10), AccessModes: []AccessMode{ReadWriteOnce},
		ReclaimPolicy: ReclaimRecycle, StorageClassName: "standard", VolumeSource: "nfs"})

	// 각각에 PVC 바인딩
	ctrl4.CreateClaim(&PersistentVolumeClaim{
		Name: "claim-retain", Namespace: "default", UID: "r1",
		RequestBytes: giBytes(5), AccessModes: []AccessMode{ReadWriteOnce}, StorageClassName: "standard"})
	ctrl4.CreateClaim(&PersistentVolumeClaim{
		Name: "claim-delete", Namespace: "default", UID: "r2",
		RequestBytes: giBytes(5), AccessModes: []AccessMode{ReadWriteOnce}, StorageClassName: "standard"})
	ctrl4.CreateClaim(&PersistentVolumeClaim{
		Name: "claim-recycle", Namespace: "default", UID: "r3",
		RequestBytes: giBytes(5), AccessModes: []AccessMode{ReadWriteOnce}, StorageClassName: "standard"})

	printSubHeader("PVC 삭제 → Reclaim 정책 실행")

	fmt.Println("\n  [Retain 정책]")
	ctrl4.DeleteClaim("default", "claim-retain")

	fmt.Println("\n  [Delete 정책]")
	ctrl4.DeleteClaim("default", "claim-delete")

	fmt.Println("\n  [Recycle 정책 — deprecated]")
	ctrl4.DeleteClaim("default", "claim-recycle")

	printSubHeader("최종 PV 상태")
	fmt.Print(ctrl4.GetVolumeStatus())

	// =====================================================================
	// 데모 5: 수명주기 전체 흐름
	// =====================================================================
	printHeader("데모 5: 볼륨 수명주기 전체 흐름")

	ctrl5 := NewPVController()
	ctrl5.AddStorageClass(&StorageClass{
		Name: "standard", Provisioner: "kubernetes.io/gce-pd",
		ReclaimPolicy: ReclaimDelete, VolumeBindingMode: "Immediate",
	})

	fmt.Println("\n  수명주기: Available → Bound → Released → Delete/Retain")
	fmt.Println()

	// Step 1: PV Available
	ctrl5.AddVolume(&PersistentVolume{
		Name: "pv-lifecycle", CapacityBytes: giBytes(20), AccessModes: []AccessMode{ReadWriteOnce},
		ReclaimPolicy: ReclaimRetain, StorageClassName: "standard", VolumeSource: "gce-pd"})
	fmt.Printf("  Phase: %s\n", ctrl5.volumes["pv-lifecycle"].Phase)

	// Step 2: PVC → Bound
	fmt.Println()
	ctrl5.CreateClaim(&PersistentVolumeClaim{
		Name: "lifecycle-claim", Namespace: "default", UID: "lc-1",
		RequestBytes: giBytes(10), AccessModes: []AccessMode{ReadWriteOnce}, StorageClassName: "standard"})
	fmt.Printf("  Phase: %s\n", ctrl5.volumes["pv-lifecycle"].Phase)

	// Step 3: PVC 삭제 → Released
	fmt.Println()
	ctrl5.DeleteClaim("default", "lifecycle-claim")
	pv := ctrl5.volumes["pv-lifecycle"]
	if pv != nil {
		fmt.Printf("  Phase: %s (Retain 정책이므로 Released 상태 유지)\n", pv.Phase)
	}

	// =====================================================================
	// 요약
	// =====================================================================
	printHeader("요약: PV/PVC 바인딩 알고리즘")
	fmt.Println(`
  바인딩 매칭 기준 (우선순위):
    1. AccessModes: PV 모드 ⊇ PVC 요구 모드
    2. StorageClass: 동일한 클래스
    3. 용량: PV 용량 ≥ PVC 요청 용량
    4. 레이블 셀렉터: PVC.Selector ⊆ PV.Labels
    5. 최소 낭비: 조건 만족 PV 중 가장 작은 것

  수명주기:
    Available ──────→ Bound ──────→ Released
       ↑               (PVC 바인딩)    (PVC 삭제)
       │                                 │
       │   ┌─────────────────────────────┤
       │   │ Retain: Released 유지 (수동 정리)
       │   │ Delete: PV + 스토리지 삭제
       └───┤ Recycle: 데이터 삭제 후 Available (deprecated)
           └─────────────────────────────────

  동적 프로비저닝:
    PVC(StorageClassName) → StorageClass → Provisioner(CSI) → PV 자동 생성

  실제 소스 경로:
  - PV/PVC 타입:     pkg/apis/core/types.go
  - PV Controller:   pkg/controller/volume/persistentvolume/pv_controller.go
  - 매칭 알고리즘:    pkg/controller/volume/persistentvolume/index.go
  - StorageClass:    staging/src/k8s.io/api/storage/v1/types.go`)
}
