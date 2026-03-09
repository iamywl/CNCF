package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Pod Disruption Budget (PDB) 및 Eviction API 핵심 알고리즘 시뮬레이션
//
// Kubernetes 소스코드 참조:
//   - pkg/controller/disruption/disruption.go (DisruptionController)
//   - pkg/registry/core/pod/storage/eviction.go (EvictionREST)
//   - staging/src/k8s.io/api/policy/v1/types.go (타입 정의)
//
// 시뮬레이션 항목:
//   1. PDB Status 계산 (DisruptionsAllowed)
//   2. Eviction API 흐름과 checkAndDecrement
//   3. DisruptedPods 맵 관리와 DeletionTimeout
//   4. MinAvailable vs MaxUnavailable 시나리오
//   5. Unhealthy Pod Eviction Policy
// =============================================================================

// =============================================================================
// 타입 정의 (staging/src/k8s.io/api/policy/v1/types.go 참조)
// =============================================================================

// UnhealthyPodEvictionPolicyType은 비정상 Pod의 퇴거 정책을 정의한다.
// types.go 77-95번 라인 참조.
type UnhealthyPodEvictionPolicyType string

const (
	IfHealthyBudget UnhealthyPodEvictionPolicyType = "IfHealthyBudget"
	AlwaysAllow     UnhealthyPodEvictionPolicyType = "AlwaysAllow"
)

// PodPhase는 Pod의 실행 단계를 나타낸다.
type PodPhase string

const (
	PodPending   PodPhase = "Pending"
	PodRunning   PodPhase = "Running"
	PodSucceeded PodPhase = "Succeeded"
	PodFailed    PodPhase = "Failed"
)

// Pod는 Kubernetes Pod의 핵심 필드를 시뮬레이션한다.
type Pod struct {
	Name              string
	Phase             PodPhase
	Ready             bool
	DeletionTimestamp *time.Time // nil이면 삭제되지 않음
	ResourceVersion   int64
	Labels            map[string]string
	OwnerUID          string // ownerReference의 UID 시뮬레이션
}

// PodDisruptionBudgetSpec은 PDB의 사양을 정의한다.
// types.go 28-75번 라인 참조.
type PodDisruptionBudgetSpec struct {
	MinAvailable               *IntOrString
	MaxUnavailable             *IntOrString
	Selector                   map[string]string
	UnhealthyPodEvictionPolicy *UnhealthyPodEvictionPolicyType
}

// PodDisruptionBudgetStatus는 컨트롤러가 계산한 PDB 상태이다.
// types.go 99-154번 라인 참조.
type PodDisruptionBudgetStatus struct {
	ObservedGeneration int64
	DisruptedPods      map[string]time.Time // Pod 이름 → 퇴거 승인 시각
	DisruptionsAllowed int32
	CurrentHealthy     int32
	DesiredHealthy     int32
	ExpectedPods       int32
	Conditions         []Condition
}

// PodDisruptionBudget은 PDB 객체를 시뮬레이션한다.
// types.go 177-189번 라인 참조.
type PodDisruptionBudget struct {
	Name            string
	Generation      int64
	ResourceVersion int64
	Spec            PodDisruptionBudgetSpec
	Status          PodDisruptionBudgetStatus
}

// Condition은 PDB 상태 조건이다.
type Condition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

// IntOrString은 정수 또는 백분율 문자열을 표현한다.
type IntOrString struct {
	IntVal     int32
	StrVal     string
	IsPercent  bool
}

// =============================================================================
// 상수 정의 (disruption.go, eviction.go 참조)
// =============================================================================

const (
	// DeletionTimeout은 DisruptedPods에 등록된 Pod가 삭제되기까지 기다리는 최대 시간이다.
	// disruption.go 69번 라인: DeletionTimeout = 2 * time.Minute
	// 시뮬레이션에서는 3초로 축소
	DeletionTimeout = 3 * time.Second

	// MaxDisruptedPodSize는 DisruptedPods 맵의 최대 크기이다.
	// eviction.go 54번 라인: MaxDisruptedPodSize = 2000
	MaxDisruptedPodSize = 2000

	// 조건 상수
	DisruptionAllowedCondition = "DisruptionAllowed"
	SyncFailedReason           = "SyncFailed"
	SufficientPodsReason       = "SufficientPods"
	InsufficientPodsReason     = "InsufficientPods"
)

// =============================================================================
// Eviction 에러 타입
// =============================================================================

type EvictionError struct {
	Code    int
	Message string
}

func (e *EvictionError) Error() string {
	return fmt.Sprintf("[HTTP %d] %s", e.Code, e.Message)
}

// =============================================================================
// DisruptionController 시뮬레이션
// pkg/controller/disruption/disruption.go 81-120번 라인 참조
// =============================================================================

type DisruptionController struct {
	mu   sync.Mutex
	pods []*Pod
	pdbs []*PodDisruptionBudget
	// controllerScales는 컨트롤러 UID → replicas 매핑
	controllerScales map[string]int32
	clock            func() time.Time
}

func NewDisruptionController() *DisruptionController {
	return &DisruptionController{
		controllerScales: make(map[string]int32),
		clock:            time.Now,
	}
}

// getExpectedPodCount는 expectedCount와 desiredHealthy를 계산한다.
// disruption.go 818-858번 라인 참조.
func (dc *DisruptionController) getExpectedPodCount(
	pdb *PodDisruptionBudget,
	matchingPods []*Pod,
) (expectedCount, desiredHealthy int32) {

	if pdb.Spec.MaxUnavailable != nil {
		// MaxUnavailable 모드: 컨트롤러 scale에서 expectedCount 결정
		expectedCount = dc.getExpectedScale(matchingPods)
		maxUnavailable := getScaledValue(pdb.Spec.MaxUnavailable, int(expectedCount))
		desiredHealthy = expectedCount - int32(maxUnavailable)
		if desiredHealthy < 0 {
			desiredHealthy = 0
		}
	} else if pdb.Spec.MinAvailable != nil {
		if !pdb.Spec.MinAvailable.IsPercent {
			// MinAvailable 절대값 모드
			desiredHealthy = pdb.Spec.MinAvailable.IntVal
			expectedCount = int32(len(matchingPods))
		} else {
			// MinAvailable 백분율 모드
			expectedCount = dc.getExpectedScale(matchingPods)
			minAvailable := getScaledValue(pdb.Spec.MinAvailable, int(expectedCount))
			desiredHealthy = int32(minAvailable)
		}
	}
	return
}

// getExpectedScale은 Pod들의 ownerReference를 추적하여 컨트롤러 scale을 합산한다.
// disruption.go 860-921번 라인 참조.
func (dc *DisruptionController) getExpectedScale(pods []*Pod) int32 {
	seen := make(map[string]bool)
	var total int32
	for _, pod := range pods {
		if pod.OwnerUID == "" || seen[pod.OwnerUID] {
			continue
		}
		seen[pod.OwnerUID] = true
		if scale, ok := dc.controllerScales[pod.OwnerUID]; ok {
			total += scale
		}
	}
	return total
}

// countHealthyPods는 현재 정상 Pod 수를 계산한다.
// disruption.go 924-940번 라인 참조.
func countHealthyPods(pods []*Pod, disruptedPods map[string]time.Time, currentTime time.Time) int32 {
	var currentHealthy int32
	for _, pod := range pods {
		// 조건 1: 삭제 중인 Pod 제외
		if pod.DeletionTimestamp != nil {
			continue
		}
		// 조건 2: DisruptedPods에 있고 DeletionTimeout 내인 Pod 제외
		if disruptionTime, found := disruptedPods[pod.Name]; found {
			if disruptionTime.Add(DeletionTimeout).After(currentTime) {
				continue
			}
		}
		// 조건 3: Ready인 Pod만 카운트
		if pod.Ready {
			currentHealthy++
		}
	}
	return currentHealthy
}

// buildDisruptedPodMap은 DisruptedPods 맵을 정리한다.
// disruption.go 944-976번 라인 참조.
func (dc *DisruptionController) buildDisruptedPodMap(
	pods []*Pod,
	pdb *PodDisruptionBudget,
	currentTime time.Time,
) (map[string]time.Time, *time.Time) {
	disruptedPods := pdb.Status.DisruptedPods
	result := make(map[string]time.Time)
	var recheckTime *time.Time

	if disruptedPods == nil {
		return result, recheckTime
	}

	for _, pod := range pods {
		// 이미 삭제 중인 Pod는 맵에서 제거
		if pod.DeletionTimestamp != nil {
			continue
		}
		disruptionTime, found := disruptedPods[pod.Name]
		if !found {
			continue
		}
		expectedDeletion := disruptionTime.Add(DeletionTimeout)
		if expectedDeletion.Before(currentTime) {
			// DeletionTimeout 초과: 삭제되지 않은 것으로 판단
			fmt.Printf("  [경고] Pod %q는 삭제될 것으로 예상되었으나 삭제되지 않음 (타임아웃)\n", pod.Name)
		} else {
			// 아직 타임아웃 이내: 결과에 유지
			if recheckTime == nil || expectedDeletion.Before(*recheckTime) {
				recheckTime = &expectedDeletion
			}
			result[pod.Name] = disruptionTime
		}
	}
	return result, recheckTime
}

// updatePdbStatus는 PDB Status를 업데이트한다.
// disruption.go 1001-1037번 라인 참조.
func (dc *DisruptionController) updatePdbStatus(
	pdb *PodDisruptionBudget,
	currentHealthy, desiredHealthy, expectedCount int32,
	disruptedPods map[string]time.Time,
) {
	// 핵심 계산: DisruptionsAllowed = max(0, currentHealthy - desiredHealthy)
	disruptionsAllowed := currentHealthy - desiredHealthy
	if expectedCount <= 0 || disruptionsAllowed <= 0 {
		disruptionsAllowed = 0
	}

	pdb.Status.CurrentHealthy = currentHealthy
	pdb.Status.DesiredHealthy = desiredHealthy
	pdb.Status.ExpectedPods = expectedCount
	pdb.Status.DisruptionsAllowed = disruptionsAllowed
	pdb.Status.DisruptedPods = disruptedPods
	pdb.Status.ObservedGeneration = pdb.Generation
	pdb.ResourceVersion++

	// Condition 업데이트
	var cond Condition
	if disruptionsAllowed > 0 {
		cond = Condition{
			Type:   DisruptionAllowedCondition,
			Status: "True",
			Reason: SufficientPodsReason,
		}
	} else if expectedCount <= 0 || currentHealthy <= desiredHealthy {
		cond = Condition{
			Type:   DisruptionAllowedCondition,
			Status: "False",
			Reason: InsufficientPodsReason,
		}
	}
	pdb.Status.Conditions = []Condition{cond}
}

// failSafe는 동기화 실패 시 DisruptionsAllowed를 0으로 설정한다.
// disruption.go 983-999번 라인 참조.
func (dc *DisruptionController) failSafe(pdb *PodDisruptionBudget, err error) {
	pdb.Status.DisruptionsAllowed = 0
	pdb.Status.Conditions = []Condition{{
		Type:    DisruptionAllowedCondition,
		Status:  "False",
		Reason:  SyncFailedReason,
		Message: err.Error(),
	}}
	pdb.ResourceVersion++
	fmt.Printf("  [failSafe] DisruptionsAllowed=0 설정 (원인: %s)\n", err.Error())
}

// trySync는 PDB의 상태를 재계산한다.
// disruption.go 735-772번 라인 참조.
func (dc *DisruptionController) trySync(pdb *PodDisruptionBudget) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	// 1. 매칭 Pod 조회
	matchingPods := dc.getMatchingPods(pdb)

	// 2. expectedCount, desiredHealthy 계산
	expectedCount, desiredHealthy := dc.getExpectedPodCount(pdb, matchingPods)

	// 3. DisruptedPods 맵 정리
	currentTime := dc.clock()
	disruptedPods, recheckTime := dc.buildDisruptedPodMap(matchingPods, pdb, currentTime)

	// 4. 현재 정상 Pod 수 계산
	currentHealthy := countHealthyPods(matchingPods, disruptedPods, currentTime)

	// 5. PDB Status 업데이트
	dc.updatePdbStatus(pdb, currentHealthy, desiredHealthy, expectedCount, disruptedPods)

	if recheckTime != nil {
		fmt.Printf("  [recheckQueue] 재확인 예정: %v 후\n", recheckTime.Sub(currentTime).Round(time.Millisecond))
	}
}

// getMatchingPods는 PDB 셀렉터에 매칭되는 Pod를 반환한다.
func (dc *DisruptionController) getMatchingPods(pdb *PodDisruptionBudget) []*Pod {
	var result []*Pod
	for _, pod := range dc.pods {
		if matchesSelector(pod.Labels, pdb.Spec.Selector) {
			result = append(result, pod)
		}
	}
	return result
}

// =============================================================================
// EvictionREST 시뮬레이션
// pkg/registry/core/pod/storage/eviction.go 70-74번 라인 참조
// =============================================================================

type EvictionREST struct {
	controller *DisruptionController
}

// canIgnorePDB는 PDB 확인을 건너뛸 수 있는 Pod 상태인지 확인한다.
// eviction.go 389-397번 라인 참조.
func canIgnorePDB(pod *Pod) bool {
	if pod.Phase == PodSucceeded || pod.Phase == PodFailed ||
		pod.Phase == PodPending || pod.DeletionTimestamp != nil {
		return true
	}
	return false
}

// checkAndDecrement는 PDB의 DisruptionsAllowed를 확인하고 감소시킨다.
// eviction.go 424-484번 라인 참조.
func (r *EvictionREST) checkAndDecrement(
	podName string,
	pdb *PodDisruptionBudget,
	dryRun bool,
) error {
	// 1. ObservedGeneration 검증
	if pdb.Status.ObservedGeneration < pdb.Generation {
		return &EvictionError{429, fmt.Sprintf(
			"PDB %q의 Status가 아직 동기화되지 않음 (ObservedGeneration=%d < Generation=%d)",
			pdb.Name, pdb.Status.ObservedGeneration, pdb.Generation)}
	}

	// 2. DisruptionsAllowed 음수 검증
	if pdb.Status.DisruptionsAllowed < 0 {
		return &EvictionError{403, fmt.Sprintf(
			"PDB %q의 DisruptionsAllowed가 음수 (%d)", pdb.Name, pdb.Status.DisruptionsAllowed)}
	}

	// 3. DisruptedPods 맵 크기 검증
	if len(pdb.Status.DisruptedPods) > MaxDisruptedPodSize {
		return &EvictionError{403, fmt.Sprintf(
			"PDB %q의 DisruptedPods 맵이 너무 큼 (%d > %d)",
			pdb.Name, len(pdb.Status.DisruptedPods), MaxDisruptedPodSize)}
	}

	// 4. DisruptionsAllowed == 0이면 퇴거 거부
	if pdb.Status.DisruptionsAllowed == 0 {
		var msg string
		cond := findCondition(pdb.Status.Conditions, DisruptionAllowedCondition)
		switch {
		case cond != nil && cond.Reason == SyncFailedReason:
			msg = fmt.Sprintf("PDB %q가 동기화 실패로 퇴거 차단: %s", pdb.Name, cond.Message)
		case pdb.Status.CurrentHealthy <= pdb.Status.DesiredHealthy:
			msg = fmt.Sprintf("PDB %q에 %d개의 정상 Pod가 필요하지만 현재 %d개",
				pdb.Name, pdb.Status.DesiredHealthy, pdb.Status.CurrentHealthy)
		default:
			msg = fmt.Sprintf("PDB %q가 현재 퇴거를 허용하지 않음", pdb.Name)
		}
		return &EvictionError{429, msg}
	}

	// 5. 예산 감소
	pdb.Status.DisruptionsAllowed--

	// 6. Dry-run이면 여기서 종료
	if dryRun {
		return nil
	}

	// 7. DisruptedPods에 Pod 등록
	if pdb.Status.DisruptedPods == nil {
		pdb.Status.DisruptedPods = make(map[string]time.Time)
	}
	pdb.Status.DisruptedPods[podName] = time.Now()

	// 8. PDB ResourceVersion 증가 (etcd 업데이트 시뮬레이션)
	pdb.ResourceVersion++

	return nil
}

// Evict는 Pod 퇴거 요청을 처리한다.
// eviction.go Create 메서드 (129번 라인~) 참조.
func (r *EvictionREST) Evict(podName string, dryRun bool) error {
	r.controller.mu.Lock()
	defer r.controller.mu.Unlock()

	// Pod 조회
	var pod *Pod
	for _, p := range r.controller.pods {
		if p.Name == podName {
			pod = p
			break
		}
	}
	if pod == nil {
		return &EvictionError{404, fmt.Sprintf("Pod %q를 찾을 수 없음", podName)}
	}

	// Terminal/Pending Pod는 PDB 무시하고 직접 삭제
	if canIgnorePDB(pod) {
		if !dryRun {
			now := time.Now()
			pod.DeletionTimestamp = &now
			fmt.Printf("  [Eviction] Pod %q (phase=%s) PDB 무시하고 삭제\n", podName, pod.Phase)
		}
		return nil
	}

	// PDB 조회
	var matchingPDBs []*PodDisruptionBudget
	for _, pdb := range r.controller.pdbs {
		if matchesSelector(pod.Labels, pdb.Spec.Selector) {
			matchingPDBs = append(matchingPDBs, pdb)
		}
	}

	// PDB 2개 이상이면 에러
	if len(matchingPDBs) > 1 {
		return &EvictionError{500, "이 Pod에 2개 이상의 PDB가 적용되어 퇴거 불가"}
	}

	// PDB 없으면 바로 삭제
	if len(matchingPDBs) == 0 {
		if !dryRun {
			now := time.Now()
			pod.DeletionTimestamp = &now
			fmt.Printf("  [Eviction] Pod %q PDB 없이 삭제\n", podName)
		}
		return nil
	}

	pdb := matchingPDBs[0]

	// Unhealthy Pod 정책 확인
	if !pod.Ready {
		if pdb.Spec.UnhealthyPodEvictionPolicy != nil &&
			*pdb.Spec.UnhealthyPodEvictionPolicy == AlwaysAllow {
			// AlwaysAllow: 바로 삭제, PDB 감소 없음
			if !dryRun {
				now := time.Now()
				pod.DeletionTimestamp = &now
				fmt.Printf("  [Eviction] Unhealthy Pod %q AlwaysAllow 정책으로 삭제 (PDB 감소 없음)\n", podName)
			}
			return nil
		}
		// IfHealthyBudget (기본값)
		if pdb.Status.CurrentHealthy >= pdb.Status.DesiredHealthy &&
			pdb.Status.DesiredHealthy > 0 {
			if !dryRun {
				now := time.Now()
				pod.DeletionTimestamp = &now
				fmt.Printf("  [Eviction] Unhealthy Pod %q IfHealthyBudget 정책으로 삭제 (예산 충분, PDB 감소 없음)\n", podName)
			}
			return nil
		}
		// 예산 부족: checkAndDecrement로 진행
	}

	// checkAndDecrement 호출
	if err := r.checkAndDecrement(podName, pdb, dryRun); err != nil {
		return err
	}

	// 성공: Pod 삭제
	if !dryRun {
		now := time.Now()
		pod.DeletionTimestamp = &now
		fmt.Printf("  [Eviction] Pod %q 퇴거 성공 (DisruptionsAllowed: %d→%d)\n",
			podName, pdb.Status.DisruptionsAllowed+1, pdb.Status.DisruptionsAllowed)
	}
	return nil
}

// =============================================================================
// 유틸리티 함수
// =============================================================================

func matchesSelector(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func getScaledValue(val *IntOrString, total int) int32 {
	if !val.IsPercent {
		return val.IntVal
	}
	// 백분율 계산 (올림)
	pct := float64(val.IntVal)
	return int32(math.Ceil(pct / 100.0 * float64(total)))
}

func findCondition(conditions []Condition, condType string) *Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

func printPDBStatus(pdb *PodDisruptionBudget) {
	fmt.Printf("  PDB %q Status:\n", pdb.Name)
	fmt.Printf("    ExpectedPods:       %d\n", pdb.Status.ExpectedPods)
	fmt.Printf("    DesiredHealthy:     %d\n", pdb.Status.DesiredHealthy)
	fmt.Printf("    CurrentHealthy:     %d\n", pdb.Status.CurrentHealthy)
	fmt.Printf("    DisruptionsAllowed: %d\n", pdb.Status.DisruptionsAllowed)
	fmt.Printf("    DisruptedPods:      %v\n", formatDisruptedPods(pdb.Status.DisruptedPods))
	fmt.Printf("    ObservedGeneration: %d (Generation: %d)\n",
		pdb.Status.ObservedGeneration, pdb.Generation)
	if len(pdb.Status.Conditions) > 0 {
		c := pdb.Status.Conditions[0]
		fmt.Printf("    Condition:          %s=%s (reason=%s)\n", c.Type, c.Status, c.Reason)
	}
}

func formatDisruptedPods(dp map[string]time.Time) string {
	if len(dp) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(dp))
	for name := range dp {
		parts = append(parts, name)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printSubSection(title string) {
	fmt.Println()
	fmt.Printf("--- %s ---\n", title)
}

// =============================================================================
// 시뮬레이션 시나리오
// =============================================================================

// 시뮬레이션 1: MinAvailable vs MaxUnavailable 비교
func simulateMinAvailVsMaxUnavail() {
	printSeparator("시뮬레이션 1: MinAvailable vs MaxUnavailable 비교")

	scenarios := []struct {
		name     string
		replicas int32
		spec     PodDisruptionBudgetSpec
	}{
		{
			"minAvailable=3 (절대값)",
			5,
			PodDisruptionBudgetSpec{
				MinAvailable: &IntOrString{IntVal: 3},
				Selector:     map[string]string{"app": "web"},
			},
		},
		{
			"minAvailable=60% (백분율)",
			5,
			PodDisruptionBudgetSpec{
				MinAvailable: &IntOrString{IntVal: 60, IsPercent: true},
				Selector:     map[string]string{"app": "web"},
			},
		},
		{
			"maxUnavailable=2 (절대값)",
			5,
			PodDisruptionBudgetSpec{
				MaxUnavailable: &IntOrString{IntVal: 2},
				Selector:       map[string]string{"app": "web"},
			},
		},
		{
			"maxUnavailable=20% (백분율)",
			5,
			PodDisruptionBudgetSpec{
				MaxUnavailable: &IntOrString{IntVal: 20, IsPercent: true},
				Selector:       map[string]string{"app": "web"},
			},
		},
		{
			"minAvailable=100% (완전 차단)",
			5,
			PodDisruptionBudgetSpec{
				MinAvailable: &IntOrString{IntVal: 100, IsPercent: true},
				Selector:     map[string]string{"app": "web"},
			},
		},
		{
			"maxUnavailable=0 (완전 차단)",
			5,
			PodDisruptionBudgetSpec{
				MaxUnavailable: &IntOrString{IntVal: 0},
				Selector:       map[string]string{"app": "web"},
			},
		},
	}

	for _, sc := range scenarios {
		printSubSection(sc.name)

		dc := NewDisruptionController()
		dc.controllerScales["rs-1"] = sc.replicas

		// Pod 생성: 모두 Ready
		for i := int32(0); i < sc.replicas; i++ {
			dc.pods = append(dc.pods, &Pod{
				Name:     fmt.Sprintf("pod-%d", i),
				Phase:    PodRunning,
				Ready:    true,
				Labels:   map[string]string{"app": "web"},
				OwnerUID: "rs-1",
			})
		}

		pdb := &PodDisruptionBudget{
			Name:       "pdb-test",
			Generation: 1,
			Spec:       sc.spec,
		}
		dc.pdbs = []*PodDisruptionBudget{pdb}

		dc.trySync(pdb)
		printPDBStatus(pdb)
	}
}

// 시뮬레이션 2: Eviction API 흐름과 checkAndDecrement
func simulateEvictionFlow() {
	printSeparator("시뮬레이션 2: Eviction API 흐름 (checkAndDecrement)")

	dc := NewDisruptionController()
	dc.controllerScales["rs-1"] = 5

	// 5개 Pod, 모두 Ready
	for i := 0; i < 5; i++ {
		dc.pods = append(dc.pods, &Pod{
			Name:     fmt.Sprintf("web-%d", i),
			Phase:    PodRunning,
			Ready:    true,
			Labels:   map[string]string{"app": "web"},
			OwnerUID: "rs-1",
		})
	}

	pdb := &PodDisruptionBudget{
		Name:       "web-pdb",
		Generation: 1,
		Spec: PodDisruptionBudgetSpec{
			MinAvailable: &IntOrString{IntVal: 3},
			Selector:     map[string]string{"app": "web"},
		},
	}
	dc.pdbs = []*PodDisruptionBudget{pdb}

	eviction := &EvictionREST{controller: dc}

	// 초기 동기화
	printSubSection("초기 동기화")
	dc.trySync(pdb)
	printPDBStatus(pdb)

	// 연속 퇴거 시도
	for i := 0; i < 4; i++ {
		podName := fmt.Sprintf("web-%d", i)
		printSubSection(fmt.Sprintf("퇴거 시도 %d: %s", i+1, podName))

		err := eviction.Evict(podName, false)
		if err != nil {
			fmt.Printf("  [결과] 거부됨: %s\n", err)
		} else {
			fmt.Printf("  [결과] 성공\n")
		}
		printPDBStatus(pdb)
	}

	// 컨트롤러 재동기화
	printSubSection("컨트롤러 재동기화 (삭제된 Pod 반영)")
	dc.trySync(pdb)
	printPDBStatus(pdb)
}

// 시뮬레이션 3: DisruptedPods 맵과 DeletionTimeout
func simulateDisruptedPodsTimeout() {
	printSeparator("시뮬레이션 3: DisruptedPods 맵과 DeletionTimeout")
	fmt.Printf("  (DeletionTimeout = %v로 축소하여 시뮬레이션)\n", DeletionTimeout)

	dc := NewDisruptionController()
	dc.controllerScales["rs-1"] = 5

	for i := 0; i < 5; i++ {
		dc.pods = append(dc.pods, &Pod{
			Name:     fmt.Sprintf("app-%d", i),
			Phase:    PodRunning,
			Ready:    true,
			Labels:   map[string]string{"app": "demo"},
			OwnerUID: "rs-1",
		})
	}

	pdb := &PodDisruptionBudget{
		Name:       "demo-pdb",
		Generation: 1,
		Spec: PodDisruptionBudgetSpec{
			MinAvailable: &IntOrString{IntVal: 3},
			Selector:     map[string]string{"app": "demo"},
		},
	}
	dc.pdbs = []*PodDisruptionBudget{pdb}
	eviction := &EvictionREST{controller: dc}

	// 초기 동기화
	printSubSection("1단계: 초기 동기화")
	dc.trySync(pdb)
	printPDBStatus(pdb)

	// Pod 퇴거 (삭제하지 않고 DisruptedPods만 등록)
	printSubSection("2단계: Pod 퇴거 승인 (삭제 지연 시뮬레이션)")
	fmt.Println("  → Pod를 실제로 삭제하지 않고 DisruptedPods에만 등록하여")
	fmt.Println("    DeletionTimeout 시나리오를 재현합니다")

	// 직접 DisruptedPods에 등록 (삭제 지연 시뮬레이션)
	dc.mu.Lock()
	pdb.Status.DisruptedPods = map[string]time.Time{
		"app-0": time.Now(),
	}
	pdb.Status.DisruptionsAllowed--
	dc.mu.Unlock()
	printPDBStatus(pdb)

	// 즉시 재동기화: DisruptedPods 내 Pod는 healthy에서 제외
	printSubSection("3단계: 즉시 재동기화 (Pod 아직 존재)")
	dc.trySync(pdb)
	printPDBStatus(pdb)
	fmt.Println("  → app-0는 DisruptedPods에 있으므로 CurrentHealthy에서 제외됨")

	// DeletionTimeout 경과 후 재동기화
	printSubSection("4단계: DeletionTimeout 경과 후 재동기화")
	fmt.Printf("  %v 대기 중...\n", DeletionTimeout+500*time.Millisecond)
	time.Sleep(DeletionTimeout + 500*time.Millisecond)

	dc.trySync(pdb)
	printPDBStatus(pdb)
	fmt.Println("  → DeletionTimeout 초과: app-0이 DisruptedPods에서 제거됨")
	fmt.Println("  → Pod가 삭제되지 않았으므로 다시 Healthy로 카운트됨")

	// 정상 퇴거 후 삭제 확인
	printSubSection("5단계: 정상 퇴거 (Pod 실제 삭제)")
	err := eviction.Evict("app-1", false)
	if err != nil {
		fmt.Printf("  [결과] 거부됨: %s\n", err)
	} else {
		fmt.Printf("  [결과] 성공\n")
	}

	// 삭제 후 재동기화
	printSubSection("6단계: 삭제 후 재동기화")
	dc.trySync(pdb)
	printPDBStatus(pdb)
	fmt.Println("  → 삭제된 Pod는 DeletionTimestamp 설정으로 DisruptedPods에서 제거됨")
}

// 시뮬레이션 4: Unhealthy Pod Eviction Policy 비교
func simulateUnhealthyPodPolicy() {
	printSeparator("시뮬레이션 4: Unhealthy Pod Eviction Policy 비교")

	policies := []struct {
		name   string
		policy *UnhealthyPodEvictionPolicyType
	}{
		{"IfHealthyBudget (기본값)", nil},
		{"AlwaysAllow", ptrPolicy(AlwaysAllow)},
	}

	for _, pc := range policies {
		printSubSection(fmt.Sprintf("정책: %s", pc.name))

		// 하위 시나리오: 예산 충분 vs 예산 부족
		for _, budgetSufficient := range []bool{true, false} {
			dc := NewDisruptionController()
			dc.controllerScales["rs-1"] = 5

			readyCount := 3
			if !budgetSufficient {
				readyCount = 2
			}

			// Pod 생성: 일부 Ready, 1개 Unready
			for i := 0; i < 5; i++ {
				ready := i < readyCount
				dc.pods = append(dc.pods, &Pod{
					Name:     fmt.Sprintf("app-%d", i),
					Phase:    PodRunning,
					Ready:    ready,
					Labels:   map[string]string{"app": "test"},
					OwnerUID: "rs-1",
				})
			}

			pdb := &PodDisruptionBudget{
				Name:       "test-pdb",
				Generation: 1,
				Spec: PodDisruptionBudgetSpec{
					MinAvailable:               &IntOrString{IntVal: 3},
					Selector:                   map[string]string{"app": "test"},
					UnhealthyPodEvictionPolicy: pc.policy,
				},
			}
			dc.pdbs = []*PodDisruptionBudget{pdb}

			eviction := &EvictionREST{controller: dc}
			dc.trySync(pdb)

			budgetLabel := "충분"
			if !budgetSufficient {
				budgetLabel = "부족"
			}

			fmt.Printf("\n  [시나리오] Ready=%d, Unready=%d, 예산 %s\n",
				readyCount, 5-readyCount, budgetLabel)
			printPDBStatus(pdb)

			// Unready Pod 퇴거 시도
			unreadyPod := fmt.Sprintf("app-%d", readyCount) // 첫 번째 unready pod
			fmt.Printf("  Unhealthy Pod %q 퇴거 시도...\n", unreadyPod)
			err := eviction.Evict(unreadyPod, false)
			if err != nil {
				fmt.Printf("  [결과] 거부됨: %s\n", err)
			} else {
				fmt.Printf("  [결과] 성공\n")
			}
		}
	}
}

// 시뮬레이션 5: Fail-Safe 메커니즘과 ObservedGeneration
func simulateFailSafe() {
	printSeparator("시뮬레이션 5: Fail-Safe 메커니즘과 ObservedGeneration")

	dc := NewDisruptionController()
	dc.controllerScales["rs-1"] = 5

	for i := 0; i < 5; i++ {
		dc.pods = append(dc.pods, &Pod{
			Name:     fmt.Sprintf("svc-%d", i),
			Phase:    PodRunning,
			Ready:    true,
			Labels:   map[string]string{"app": "svc"},
			OwnerUID: "rs-1",
		})
	}

	pdb := &PodDisruptionBudget{
		Name:       "svc-pdb",
		Generation: 1,
		Spec: PodDisruptionBudgetSpec{
			MinAvailable: &IntOrString{IntVal: 3},
			Selector:     map[string]string{"app": "svc"},
		},
	}
	dc.pdbs = []*PodDisruptionBudget{pdb}
	eviction := &EvictionREST{controller: dc}

	// 초기 동기화
	printSubSection("1단계: 정상 동기화")
	dc.trySync(pdb)
	printPDBStatus(pdb)

	// failSafe 시뮬레이션
	printSubSection("2단계: Fail-Safe 발동 (Scale API 접근 실패 시뮬레이션)")
	dc.failSafe(pdb, fmt.Errorf("Scale API 접근 불가: connection refused"))
	printPDBStatus(pdb)

	// failSafe 상태에서 퇴거 시도
	printSubSection("3단계: Fail-Safe 상태에서 퇴거 시도")
	err := eviction.Evict("svc-0", false)
	if err != nil {
		fmt.Printf("  [결과] 거부됨: %s\n", err)
	} else {
		fmt.Printf("  [결과] 성공\n")
	}

	// 복구 후 재동기화
	printSubSection("4단계: 복구 후 재동기화")
	dc.trySync(pdb)
	printPDBStatus(pdb)
	fmt.Println("  → Fail-Safe 복구: DisruptionsAllowed가 정상 값으로 복원됨")

	// ObservedGeneration 불일치 시뮬레이션
	printSubSection("5단계: PDB Spec 변경 (ObservedGeneration 불일치)")
	pdb.Generation = 2 // Spec 변경 시뮬레이션
	fmt.Printf("  PDB Spec 변경: Generation=%d, ObservedGeneration=%d\n",
		pdb.Generation, pdb.Status.ObservedGeneration)

	fmt.Println("  퇴거 시도...")
	err = eviction.Evict("svc-0", false)
	if err != nil {
		fmt.Printf("  [결과] 거부됨: %s\n", err)
	}

	// 컨트롤러가 재동기화하면 ObservedGeneration 업데이트
	printSubSection("6단계: 컨트롤러 재동기화 (ObservedGeneration 동기화)")
	dc.trySync(pdb)
	printPDBStatus(pdb)
	fmt.Println("  → ObservedGeneration이 Generation과 일치하면 퇴거 다시 가능")

	err = eviction.Evict("svc-0", false)
	if err != nil {
		fmt.Printf("  퇴거 결과: 거부됨: %s\n", err)
	} else {
		fmt.Printf("  퇴거 결과: 성공\n")
	}
}

// 시뮬레이션 6: canIgnorePDB - Terminal/Pending Pod 퇴거
func simulateTerminalPodEviction() {
	printSeparator("시뮬레이션 6: Terminal/Pending Pod의 PDB 무시 동작")

	dc := NewDisruptionController()
	dc.controllerScales["rs-1"] = 5

	dc.pods = []*Pod{
		{Name: "running-pod", Phase: PodRunning, Ready: true, Labels: map[string]string{"app": "test"}, OwnerUID: "rs-1"},
		{Name: "pending-pod", Phase: PodPending, Ready: false, Labels: map[string]string{"app": "test"}, OwnerUID: "rs-1"},
		{Name: "succeeded-pod", Phase: PodSucceeded, Ready: false, Labels: map[string]string{"app": "test"}, OwnerUID: "rs-1"},
		{Name: "failed-pod", Phase: PodFailed, Ready: false, Labels: map[string]string{"app": "test"}, OwnerUID: "rs-1"},
	}

	pdb := &PodDisruptionBudget{
		Name:       "test-pdb",
		Generation: 1,
		Spec: PodDisruptionBudgetSpec{
			MinAvailable: &IntOrString{IntVal: 100}, // 매우 높게 설정
			Selector:     map[string]string{"app": "test"},
		},
	}
	dc.pdbs = []*PodDisruptionBudget{pdb}

	// 동기화: DisruptionsAllowed = 0 (minAvailable=100)
	dc.trySync(pdb)

	eviction := &EvictionREST{controller: dc}

	printSubSection("PDB Status (DisruptionsAllowed=0)")
	printPDBStatus(pdb)

	phases := []string{"running-pod", "pending-pod", "succeeded-pod", "failed-pod"}
	for _, podName := range phases {
		fmt.Printf("\n  퇴거 시도: %s\n", podName)
		err := eviction.Evict(podName, false)
		if err != nil {
			fmt.Printf("  [결과] 거부됨: %s\n", err)
		} else {
			fmt.Printf("  [결과] 성공 (PDB 무시)\n")
		}
	}
}

// 시뮬레이션 7: 동시 퇴거 경쟁 (Optimistic Concurrency)
func simulateConcurrentEviction() {
	printSeparator("시뮬레이션 7: 동시 퇴거 경쟁 (Optimistic Concurrency)")
	fmt.Println("  여러 클라이언트가 동시에 퇴거를 요청하는 시나리오")

	dc := NewDisruptionController()
	dc.controllerScales["rs-1"] = 10

	for i := 0; i < 10; i++ {
		dc.pods = append(dc.pods, &Pod{
			Name:     fmt.Sprintf("worker-%d", i),
			Phase:    PodRunning,
			Ready:    true,
			Labels:   map[string]string{"app": "worker"},
			OwnerUID: "rs-1",
		})
	}

	pdb := &PodDisruptionBudget{
		Name:       "worker-pdb",
		Generation: 1,
		Spec: PodDisruptionBudgetSpec{
			MinAvailable: &IntOrString{IntVal: 8}, // 최대 2개 퇴거 가능
			Selector:     map[string]string{"app": "worker"},
		},
	}
	dc.pdbs = []*PodDisruptionBudget{pdb}

	dc.trySync(pdb)
	printPDBStatus(pdb)

	eviction := &EvictionREST{controller: dc}

	// 5개 클라이언트가 동시에 퇴거 요청
	var wg sync.WaitGroup
	results := make([]string, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			podName := fmt.Sprintf("worker-%d", idx)
			err := eviction.Evict(podName, false)
			if err != nil {
				results[idx] = fmt.Sprintf("  [클라이언트 %d] %s 퇴거 거부: %s", idx, podName, err)
			} else {
				results[idx] = fmt.Sprintf("  [클라이언트 %d] %s 퇴거 성공", idx, podName)
			}
		}(i)
	}

	wg.Wait()

	printSubSection("동시 퇴거 결과")
	for _, r := range results {
		fmt.Println(r)
	}
	printPDBStatus(pdb)

	successCount := 0
	for _, pod := range dc.pods {
		if pod.DeletionTimestamp != nil {
			successCount++
		}
	}
	fmt.Printf("\n  성공적으로 퇴거된 Pod 수: %d (허용 예산: 2)\n", successCount)
	if successCount <= 2 {
		fmt.Println("  -> PDB 예산 내에서 정확하게 동작함")
	} else {
		fmt.Println("  -> [주의] 예산 초과 발생!")
	}
}

// =============================================================================
// 메인 함수
// =============================================================================

func ptrPolicy(p UnhealthyPodEvictionPolicyType) *UnhealthyPodEvictionPolicyType {
	return &p
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Kubernetes PDB & Eviction API 핵심 알고리즘 시뮬레이션                ║")
	fmt.Println("║                                                                    ║")
	fmt.Println("║  소스 참조:                                                          ║")
	fmt.Println("║    - pkg/controller/disruption/disruption.go                        ║")
	fmt.Println("║    - pkg/registry/core/pod/storage/eviction.go                      ║")
	fmt.Println("║    - staging/src/k8s.io/api/policy/v1/types.go                      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// 시뮬레이션 1: MinAvailable vs MaxUnavailable
	simulateMinAvailVsMaxUnavail()

	// 시뮬레이션 2: Eviction API 흐름
	simulateEvictionFlow()

	// 시뮬레이션 3: DisruptedPods 타임아웃
	simulateDisruptedPodsTimeout()

	// 시뮬레이션 4: Unhealthy Pod 정책
	simulateUnhealthyPodPolicy()

	// 시뮬레이션 5: Fail-Safe 및 ObservedGeneration
	simulateFailSafe()

	// 시뮬레이션 6: Terminal Pod의 PDB 무시
	simulateTerminalPodEviction()

	// 시뮬레이션 7: 동시 퇴거 경쟁
	simulateConcurrentEviction()

	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("  모든 시뮬레이션 완료")
	fmt.Println(strings.Repeat("=", 70))
}
