package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes Horizontal Pod Autoscaler (HPA) 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/controller/podautoscaler/horizontal.go         : HorizontalController, reconcileAutoscaler
//   - pkg/controller/podautoscaler/replica_calculator.go  : ReplicaCalculator, calcPlainMetricReplicas
//   - staging/src/k8s.io/api/autoscaling/v2/types.go     : HorizontalPodAutoscaler 스펙
//
// HPA 핵심 공식:
//   desiredReplicas = ceil(currentReplicas * (currentMetricValue / desiredMetricValue))
//
// 실제 코드 (replica_calculator.go:118):
//   return int32(math.Ceil(usageRatio * float64(readyPodCount)))
//   여기서 usageRatio = currentMetricValue / desiredMetricValue
//
// Stabilization Window:
//   - scaleDown: 최근 5분(기본) 동안의 추천값 중 최대값 선택 (급격한 축소 방지)
//   - scaleUp: 최근 추천값 중 최소값 선택 (급격한 확장 제한 가능)
//   - 실제: horizontal.go의 stabilizeRecommendation(), recommendations 맵
//
// Tolerance (기본 0.1):
//   |usageRatio - 1.0| ≤ tolerance이면 스케일링하지 않음 (플래핑 방지)
//   실제: replica_calculator.go의 Tolerances.isWithin()

// =============================================================================
// 1. 데이터 모델
// =============================================================================

// MetricSourceType은 메트릭 소스 유형
// 실제: staging/src/k8s.io/api/autoscaling/v2/types.go의 MetricSourceType
type MetricSourceType string

const (
	ResourceMetricSourceType MetricSourceType = "Resource"  // CPU, Memory
	PodsMetricSourceType     MetricSourceType = "Pods"      // 커스텀 Pod 메트릭
	ObjectMetricSourceType   MetricSourceType = "Object"    // 외부 오브젝트 메트릭
)

// MetricSpec은 HPA가 감시할 메트릭 정의
// 실제: staging/src/k8s.io/api/autoscaling/v2/types.go의 MetricSpec
type MetricSpec struct {
	Type               MetricSourceType
	ResourceName       string // "cpu" 또는 "memory"
	TargetUtilization  int32  // 목표 사용률 (%) — Resource 타입
	TargetAverageValue int64  // 목표 평균값 — Pods/Object 타입
}

// HorizontalPodAutoscaler는 HPA 오브젝트
// 실제: staging/src/k8s.io/api/autoscaling/v2/types.go의 HorizontalPodAutoscaler
type HorizontalPodAutoscaler struct {
	Name        string
	Namespace   string
	TargetRef   string // 대상 Deployment/ReplicaSet 이름

	// Spec
	MinReplicas int32
	MaxReplicas int32
	Metrics     []MetricSpec

	// Status
	CurrentReplicas int32
	DesiredReplicas int32
	LastScaleTime   time.Time

	// Behavior — scaleUp/scaleDown 정책
	ScaleUpStabilizationWindow   time.Duration // 기본: 0 (즉시)
	ScaleDownStabilizationWindow time.Duration // 기본: 5분
}

// Pod는 메트릭을 제공하는 Pod
type Pod struct {
	Name         string
	CPURequest   int64   // 밀리코어 (예: 200 = 200m)
	CPUUsage     int64   // 현재 사용량 (밀리코어)
	MemoryUsage  int64   // 바이트
	Ready        bool
}

// timestampedRecommendation은 시간 기록이 있는 추천값
// 실제: horizontal.go의 timestampedRecommendation
type timestampedRecommendation struct {
	recommendation int32
	timestamp      time.Time
}

// timestampedScaleEvent는 스케일 이벤트 기록
// 실제: horizontal.go의 timestampedScaleEvent
type timestampedScaleEvent struct {
	replicaChange int32 // 변화량 (절대값)
	timestamp     time.Time
}

// =============================================================================
// 2. ReplicaCalculator — 핵심 레플리카 계산 로직
// =============================================================================

// Tolerances는 스케일링 민감도 설정
// 실제: replica_calculator.go의 Tolerances
// |usageRatio - 1.0| ≤ tolerance이면 스케일링을 건너뛴다
type Tolerances struct {
	ScaleUp   float64 // 기본: 0.1
	ScaleDown float64 // 기본: 0.1
}

// isWithin은 사용률 비율이 허용 범위 내인지 확인
// 실제: replica_calculator.go:56
//   func (t Tolerances) isWithin(usageRatio float64) bool {
//       return (1.0-t.scaleDown) <= usageRatio && usageRatio <= (1.0+t.scaleUp)
//   }
func (t Tolerances) isWithin(usageRatio float64) bool {
	return (1.0-t.ScaleDown) <= usageRatio && usageRatio <= (1.0+t.ScaleUp)
}

// ReplicaCalculator는 메트릭 기반 레플리카 수 계산기
// 실제: pkg/controller/podautoscaler/replica_calculator.go의 ReplicaCalculator
type ReplicaCalculator struct {
	tolerance Tolerances
}

func NewReplicaCalculator(tolerance float64) *ReplicaCalculator {
	return &ReplicaCalculator{
		tolerance: Tolerances{ScaleUp: tolerance, ScaleDown: tolerance},
	}
}

// CalcDesiredReplicas는 CPU 사용률 기반 원하는 레플리카 수를 계산한다
// 핵심 공식 (replica_calculator.go:118):
//   usageRatio = currentMetricValue / desiredMetricValue
//   desiredReplicas = ceil(usageRatio * readyPodCount)
//
// 세부 동작:
//   1. Pod별 CPU 사용률 수집
//   2. Ready/NotReady/Missing Pod 분류
//   3. usageRatio 계산 (= 평균 사용률 / 목표 사용률)
//   4. tolerance 이내면 현재 값 유지 (플래핑 방지)
//   5. ceil(usageRatio * readyPodCount) 반환
func (rc *ReplicaCalculator) CalcDesiredReplicas(
	pods []*Pod,
	targetUtilization int32,
	currentReplicas int32,
) (desiredReplicas int32, avgUtilization int32) {

	// Ready Pod만 필터링 및 사용률 계산
	var totalUsage int64
	var totalRequest int64
	readyCount := 0

	for _, pod := range pods {
		if !pod.Ready {
			continue
		}
		totalUsage += pod.CPUUsage
		totalRequest += pod.CPURequest
		readyCount++
	}

	if readyCount == 0 || totalRequest == 0 {
		return currentReplicas, 0
	}

	// 평균 사용률 (%) = (총 사용량 / 총 요청량) * 100
	avgUtilization = int32(totalUsage * 100 / totalRequest)

	// usageRatio = 현재 사용률 / 목표 사용률
	usageRatio := float64(avgUtilization) / float64(targetUtilization)

	// tolerance 이내면 현재 레플리카 유지
	// 실제: replica_calculator.go:112
	if rc.tolerance.isWithin(usageRatio) {
		return currentReplicas, avgUtilization
	}

	// 레플리카 계산: ceil(usageRatio * readyPodCount)
	// 실제: replica_calculator.go:118
	desiredReplicas = int32(math.Ceil(usageRatio * float64(readyCount)))

	if desiredReplicas < 1 {
		desiredReplicas = 1
	}

	return desiredReplicas, avgUtilization
}

// =============================================================================
// 3. HorizontalController — 전체 HPA 제어 루프
// =============================================================================

// HorizontalController는 HPA 컨트롤러
// 실제: pkg/controller/podautoscaler/horizontal.go의 HorizontalController
type HorizontalController struct {
	mu sync.Mutex

	// 추천값 히스토리 (stabilization window용)
	// 실제: horizontal.go의 recommendations map[string][]timestampedRecommendation
	recommendations map[string][]timestampedRecommendation

	// 스케일 이벤트 히스토리
	scaleUpEvents   map[string][]timestampedScaleEvent
	scaleDownEvents map[string][]timestampedScaleEvent

	replicaCalc *ReplicaCalculator

	// 실제 scaleUpLimitFactor = 2.0, scaleUpLimitMinimum = 4.0
	// 스케일업 제한: max(2 * currentReplicas, 4)
	scaleUpLimitFactor  float64
	scaleUpLimitMinimum float64
}

func NewHorizontalController(tolerance float64) *HorizontalController {
	return &HorizontalController{
		recommendations: make(map[string][]timestampedRecommendation),
		scaleUpEvents:   make(map[string][]timestampedScaleEvent),
		scaleDownEvents: make(map[string][]timestampedScaleEvent),
		replicaCalc:     NewReplicaCalculator(tolerance),
		scaleUpLimitFactor:  2.0,
		scaleUpLimitMinimum: 4.0,
	}
}

// ReconcileAutoscaler는 HPA의 상태를 조정하는 핵심 루프
// 실제: horizontal.go의 reconcileAutoscaler()
// 동작 순서:
//   1. 메트릭 수집 → 원하는 레플리카 계산
//   2. min/max 제한 적용
//   3. scaleUp limit 적용: max(2 * currentReplicas, 4)
//   4. stabilization window 적용 (scaleDown: 최근 5분 중 최대값)
//   5. cooldown 확인 (lastScaleTime 이후 충분한 시간 경과)
//   6. 스케일 실행
func (hc *HorizontalController) ReconcileAutoscaler(
	hpa *HorizontalPodAutoscaler,
	pods []*Pod,
	now time.Time,
) (scaled bool, reason string) {

	hc.mu.Lock()
	defer hc.mu.Unlock()

	key := hpa.Namespace + "/" + hpa.Name

	// 1단계: 메트릭 기반 원하는 레플리카 수 계산
	var desiredReplicas int32
	var avgUtil int32

	for _, metric := range hpa.Metrics {
		if metric.Type == ResourceMetricSourceType && metric.ResourceName == "cpu" {
			desiredReplicas, avgUtil = hc.replicaCalc.CalcDesiredReplicas(
				pods, metric.TargetUtilization, hpa.CurrentReplicas)
		}
	}

	rawDesired := desiredReplicas

	// 2단계: scaleUp limit 적용
	// 실제: horizontal.go:62-63
	//   scaleUpLimitFactor = 2.0, scaleUpLimitMinimum = 4.0
	// 한 번에 최대 2배 또는 +4 중 큰 값까지만 스케일업
	if desiredReplicas > hpa.CurrentReplicas {
		scaleUpLimit := int32(math.Max(
			hc.scaleUpLimitFactor*float64(hpa.CurrentReplicas),
			hc.scaleUpLimitMinimum,
		))
		if desiredReplicas > scaleUpLimit {
			desiredReplicas = scaleUpLimit
		}
	}

	// 3단계: min/max 바운드 적용
	if desiredReplicas < hpa.MinReplicas {
		desiredReplicas = hpa.MinReplicas
	}
	if desiredReplicas > hpa.MaxReplicas {
		desiredReplicas = hpa.MaxReplicas
	}

	// 4단계: Stabilization Window 적용
	// scaleDown 시 최근 N분 동안의 추천값 중 최대값 선택 (급격한 축소 방지)
	// 실제: horizontal.go의 stabilizeRecommendation()
	desiredReplicas = hc.stabilizeRecommendation(key, desiredReplicas, now, hpa)

	// 5단계: 스케일 결정
	if desiredReplicas == hpa.CurrentReplicas {
		return false, fmt.Sprintf("변경 없음 (현재=%d, CPU 평균=%d%%)", hpa.CurrentReplicas, avgUtil)
	}

	// 스케일 방향 결정
	direction := "up"
	if desiredReplicas < hpa.CurrentReplicas {
		direction = "down"
	}

	oldReplicas := hpa.CurrentReplicas
	hpa.DesiredReplicas = desiredReplicas
	hpa.CurrentReplicas = desiredReplicas
	hpa.LastScaleTime = now

	// 스케일 이벤트 기록
	change := int32(math.Abs(float64(desiredReplicas - oldReplicas)))
	event := timestampedScaleEvent{
		replicaChange: change,
		timestamp:     now,
	}
	if direction == "up" {
		hc.scaleUpEvents[key] = append(hc.scaleUpEvents[key], event)
	} else {
		hc.scaleDownEvents[key] = append(hc.scaleDownEvents[key], event)
	}

	return true, fmt.Sprintf("scale %s: %d→%d (원시=%d, CPU 평균=%d%%, 목표=%d%%)",
		direction, oldReplicas, desiredReplicas, rawDesired, avgUtil,
		hpa.Metrics[0].TargetUtilization)
}

// stabilizeRecommendation은 안정화 윈도우를 적용한다
// 실제: horizontal.go의 stabilizeRecommendation()
//
// scaleDown 안정화:
//   윈도우 내 모든 추천값 중 최대값을 사용 → 급격한 축소 방지
//   기본 5분 동안 "이 기간 중 가장 높은 추천값"을 유지
//
// scaleUp 안정화:
//   윈도우 내 모든 추천값 중 최소값을 사용 → 급격한 확장 제한
//   기본 0초 (즉시 scaleUp 허용)
func (hc *HorizontalController) stabilizeRecommendation(
	key string,
	desiredReplicas int32,
	now time.Time,
	hpa *HorizontalPodAutoscaler,
) int32 {

	// 추천값 저장
	hc.recommendations[key] = append(hc.recommendations[key], timestampedRecommendation{
		recommendation: desiredReplicas,
		timestamp:      now,
	})

	// 만료된 추천값 제거
	// scaleDown window와 scaleUp window 중 더 긴 것을 기준으로
	maxWindow := hpa.ScaleDownStabilizationWindow
	if hpa.ScaleUpStabilizationWindow > maxWindow {
		maxWindow = hpa.ScaleUpStabilizationWindow
	}

	var valid []timestampedRecommendation
	for _, rec := range hc.recommendations[key] {
		if now.Sub(rec.timestamp) <= maxWindow {
			valid = append(valid, rec)
		}
	}
	hc.recommendations[key] = valid

	// scaleDown이면: 최근 윈도우 내 최대값 사용
	if desiredReplicas < hpa.CurrentReplicas {
		stabilized := desiredReplicas
		for _, rec := range valid {
			if now.Sub(rec.timestamp) <= hpa.ScaleDownStabilizationWindow {
				if rec.recommendation > stabilized {
					stabilized = rec.recommendation
				}
			}
		}
		return stabilized
	}

	// scaleUp이면: 최근 윈도우 내 최소값 사용 (window=0이면 즉시)
	if desiredReplicas > hpa.CurrentReplicas && hpa.ScaleUpStabilizationWindow > 0 {
		stabilized := desiredReplicas
		for _, rec := range valid {
			if now.Sub(rec.timestamp) <= hpa.ScaleUpStabilizationWindow {
				if rec.recommendation < stabilized {
					stabilized = rec.recommendation
				}
			}
		}
		return stabilized
	}

	return desiredReplicas
}

// =============================================================================
// 4. 시뮬레이션 헬퍼
// =============================================================================

// generatePods는 주어진 CPU 사용률로 Pod 목록을 생성한다
func generatePods(count int, cpuRequest int64, avgUtilPercent int) []*Pod {
	pods := make([]*Pod, count)
	for i := 0; i < count; i++ {
		// 평균 주위로 약간의 변동
		jitter := rand.Intn(21) - 10 // -10% ~ +10%
		util := avgUtilPercent + jitter
		if util < 0 {
			util = 0
		}
		pods[i] = &Pod{
			Name:       fmt.Sprintf("pod-%d", i+1),
			CPURequest: cpuRequest,
			CPUUsage:   cpuRequest * int64(util) / 100,
			Ready:      true,
		}
	}
	return pods
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

// printPodMetrics는 Pod별 메트릭을 출력한다
func printPodMetrics(pods []*Pod) {
	for _, p := range pods {
		util := float64(p.CPUUsage) * 100.0 / float64(p.CPURequest)
		bar := strings.Repeat("#", int(util/5))
		fmt.Printf("    %-8s %4dm/%4dm (%3.0f%%) |%-20s|\n",
			p.Name, p.CPUUsage, p.CPURequest, util, bar)
	}
}

// =============================================================================
// 5. 메인 — 데모
// =============================================================================

func main() {
	rand.Seed(42) // 재현 가능한 결과

	controller := NewHorizontalController(0.1) // tolerance = 10%

	// =====================================================================
	// 데모 1: 기본 스케일업
	// =====================================================================
	printHeader("데모 1: CPU 부하 증가에 따른 스케일업")

	hpa := &HorizontalPodAutoscaler{
		Name:        "web-hpa",
		Namespace:   "default",
		TargetRef:   "deployment/web",
		MinReplicas: 2,
		MaxReplicas: 10,
		Metrics: []MetricSpec{
			{Type: ResourceMetricSourceType, ResourceName: "cpu", TargetUtilization: 50},
		},
		CurrentReplicas:              3,
		ScaleUpStabilizationWindow:   0,
		ScaleDownStabilizationWindow: 5 * time.Minute,
	}

	fmt.Printf("HPA 설정: target=%d%%, min=%d, max=%d, 현재=%d\n",
		hpa.Metrics[0].TargetUtilization, hpa.MinReplicas, hpa.MaxReplicas, hpa.CurrentReplicas)

	// CPU 사용률 80% → 스케일업 기대
	pods := generatePods(3, 200, 80)
	fmt.Printf("\n현재 상태 (CPU 평균 ~80%%, 목표 50%%):\n")
	printPodMetrics(pods)

	fmt.Println("\n계산:")
	fmt.Println("  usageRatio = 80 / 50 = 1.6")
	fmt.Println("  desiredReplicas = ceil(1.6 * 3) = ceil(4.8) = 5")

	now := time.Now()
	scaled, reason := controller.ReconcileAutoscaler(hpa, pods, now)
	fmt.Printf("\n결과: scaled=%v, %s\n", scaled, reason)

	// =====================================================================
	// 데모 2: Tolerance 내 → 스케일링 건너뜀
	// =====================================================================
	printHeader("데모 2: Tolerance 범위 내 — 스케일링 건너뜀")

	hpa2 := &HorizontalPodAutoscaler{
		Name:        "api-hpa",
		Namespace:   "default",
		TargetRef:   "deployment/api",
		MinReplicas: 2,
		MaxReplicas: 10,
		Metrics: []MetricSpec{
			{Type: ResourceMetricSourceType, ResourceName: "cpu", TargetUtilization: 50},
		},
		CurrentReplicas:              4,
		ScaleDownStabilizationWindow: 5 * time.Minute,
	}

	// CPU 사용률 ~52% → usageRatio = 1.04 → tolerance(0.1) 이내
	pods2 := generatePods(4, 200, 52)
	fmt.Printf("HPA 설정: target=50%%, 현재 replicas=4\n")
	fmt.Printf("현재 CPU 평균 ~52%%:\n")
	printPodMetrics(pods2)

	fmt.Println("\n계산:")
	fmt.Println("  usageRatio = 52 / 50 = 1.04")
	fmt.Println("  |1.04 - 1.0| = 0.04 ≤ tolerance(0.1)")
	fmt.Println("  → 스케일링 건너뜀 (플래핑 방지)")

	scaled, reason = controller.ReconcileAutoscaler(hpa2, pods2, now)
	fmt.Printf("\n결과: scaled=%v, %s\n", scaled, reason)

	// =====================================================================
	// 데모 3: 스케일다운 + Stabilization Window
	// =====================================================================
	printHeader("데모 3: 스케일다운 + Stabilization Window")

	hpa3 := &HorizontalPodAutoscaler{
		Name:        "worker-hpa",
		Namespace:   "default",
		TargetRef:   "deployment/worker",
		MinReplicas: 2,
		MaxReplicas: 20,
		Metrics: []MetricSpec{
			{Type: ResourceMetricSourceType, ResourceName: "cpu", TargetUtilization: 50},
		},
		CurrentReplicas:              8,
		ScaleDownStabilizationWindow: 5 * time.Minute,
	}

	controller3 := NewHorizontalController(0.1)

	fmt.Printf("HPA 설정: target=50%%, 현재=8, scaleDown window=5분\n\n")

	// 시뮬레이션: CPU가 갑자기 떨어졌다가 다시 올라가는 시나리오
	scenarios := []struct {
		timeOffset time.Duration
		cpuPercent int
		replicas   int
	}{
		{0, 20, 8},            // T+0: CPU 급락 → 원시 추천 = ceil(0.4*8) = 4
		{1 * time.Minute, 15, 8}, // T+1분: 더 떨어짐 → 원시 추천 = ceil(0.3*8) = 3
		{2 * time.Minute, 30, 8}, // T+2분: 약간 회복 → 원시 추천 = ceil(0.6*8) = 5
		{3 * time.Minute, 25, 8}, // T+3분: 다시 하락
		{4 * time.Minute, 20, 8}, // T+4분: 유지
		{6 * time.Minute, 20, 8}, // T+6분: window 벗어남 (5분 경과)
	}

	baseTime := time.Now()
	for _, s := range scenarios {
		t := baseTime.Add(s.timeOffset)
		pods3 := generatePods(int(hpa3.CurrentReplicas), 200, s.cpuPercent)

		scaled, reason := controller3.ReconcileAutoscaler(hpa3, pods3, t)
		fmt.Printf("  T+%v: CPU~%d%%, ", s.timeOffset, s.cpuPercent)
		if scaled {
			fmt.Printf("SCALED → %s\n", reason)
		} else {
			fmt.Printf("유지 → %s\n", reason)
		}
	}

	fmt.Println("\n  [안정화 윈도우 원리]")
	fmt.Println("  scaleDown 시 최근 5분 동안의 모든 추천값 중 최대값을 사용한다.")
	fmt.Println("  T+0~T+4분: 추천값 4,3,5,4,4 → 최대 5 (현재 8이므로 유지)")
	fmt.Println("  T+6분: T+0의 추천값이 만료 → 윈도우 내 추천값 재평가")

	// =====================================================================
	// 데모 4: scaleUp limit
	// =====================================================================
	printHeader("데모 4: 스케일업 제한 (scaleUpLimitFactor)")

	hpa4 := &HorizontalPodAutoscaler{
		Name:        "burst-hpa",
		Namespace:   "default",
		TargetRef:   "deployment/burst",
		MinReplicas: 1,
		MaxReplicas: 100,
		Metrics: []MetricSpec{
			{Type: ResourceMetricSourceType, ResourceName: "cpu", TargetUtilization: 50},
		},
		CurrentReplicas:              2,
		ScaleDownStabilizationWindow: 5 * time.Minute,
	}

	controller4 := NewHorizontalController(0.1)

	// CPU 500% → desiredReplicas = ceil(5.0*2) = 10, 하지만 limit은 max(2*2, 4) = 4
	pods4 := generatePods(2, 200, 250)
	fmt.Printf("현재 replicas=2, CPU 평균 ~250%% (과부하)\n")
	fmt.Println("\n계산:")
	fmt.Println("  usageRatio = 250 / 50 = 5.0")
	fmt.Println("  원시 desiredReplicas = ceil(5.0 * 2) = 10")
	fmt.Println("  scaleUpLimit = max(2.0 * 2, 4.0) = 4")
	fmt.Println("  → 4로 제한 (한 번에 최대 2배)")

	scaled, reason = controller4.ReconcileAutoscaler(hpa4, pods4, now)
	fmt.Printf("\n결과: scaled=%v, %s\n", scaled, reason)

	fmt.Println("\n다음 주기에 다시 스케일업:")
	pods4b := generatePods(int(hpa4.CurrentReplicas), 200, 200)
	scaled, reason = controller4.ReconcileAutoscaler(hpa4, pods4b, now.Add(30*time.Second))
	fmt.Printf("결과: scaled=%v, %s\n", scaled, reason)

	// =====================================================================
	// 데모 5: Min/Max 바운드
	// =====================================================================
	printHeader("데모 5: Min/Max 바운드 제한")

	hpa5 := &HorizontalPodAutoscaler{
		Name:        "bounded-hpa",
		Namespace:   "default",
		TargetRef:   "deployment/bounded",
		MinReplicas: 3,
		MaxReplicas: 8,
		Metrics: []MetricSpec{
			{Type: ResourceMetricSourceType, ResourceName: "cpu", TargetUtilization: 50},
		},
		CurrentReplicas:              5,
		ScaleDownStabilizationWindow: 0, // 즉시 스케일다운 (데모용)
	}

	controller5 := NewHorizontalController(0.1)

	// 스케일다운: CPU 10% → desired = ceil(0.2*5)=1, but min=3
	fmt.Println("[MinReplicas 적용]")
	pods5 := generatePods(5, 200, 10)
	scaled, reason = controller5.ReconcileAutoscaler(hpa5, pods5, now)
	fmt.Printf("  CPU~10%%: %s\n", reason)

	// 스케일업: CPU 300% → desired = ceil(6.0*3)=18, but max=8
	hpa5.CurrentReplicas = 3
	fmt.Println("\n[MaxReplicas 적용]")
	pods5b := generatePods(3, 200, 300)
	scaled, reason = controller5.ReconcileAutoscaler(hpa5b(hpa5), pods5b, now)
	_ = scaled
	fmt.Printf("  CPU~300%%: %s\n", reason)

	// =====================================================================
	// 데모 6: 연속 조정 시뮬레이션 (트래픽 급증 → 감소 시나리오)
	// =====================================================================
	printHeader("데모 6: 트래픽 패턴 시뮬레이션 (급증 → 감소)")

	hpa6 := &HorizontalPodAutoscaler{
		Name:        "traffic-hpa",
		Namespace:   "default",
		TargetRef:   "deployment/traffic",
		MinReplicas: 2,
		MaxReplicas: 20,
		Metrics: []MetricSpec{
			{Type: ResourceMetricSourceType, ResourceName: "cpu", TargetUtilization: 60},
		},
		CurrentReplicas:              3,
		ScaleDownStabilizationWindow: 3 * time.Minute, // 데모용 3분
	}

	controller6 := NewHorizontalController(0.1)

	// 30초 간격으로 트래픽 변화
	trafficPattern := []int{60, 90, 150, 200, 180, 120, 80, 50, 30, 30, 30, 30, 30}
	// 목표 60% → 60%은 tolerance 내, 90%→스케일업, 150%→대폭 스케일업 ...

	fmt.Printf("HPA: target=%d%%, min=%d, max=%d, stabilization=3분\n",
		hpa6.Metrics[0].TargetUtilization, hpa6.MinReplicas, hpa6.MaxReplicas)
	fmt.Printf("시간 간격: 30초, 트래픽 패턴: %v\n\n", trafficPattern)

	fmt.Printf("%-6s  %-8s  %-10s  %-10s  %s\n",
		"시간", "CPU(%)", "Replicas", "Desired", "Action")
	fmt.Println(strings.Repeat("-", 65))

	base6 := time.Now()
	for i, cpu := range trafficPattern {
		t := base6.Add(time.Duration(i) * 30 * time.Second)
		pods6 := generatePods(int(hpa6.CurrentReplicas), 200, cpu)

		oldReplicas := hpa6.CurrentReplicas
		scaled, _ := controller6.ReconcileAutoscaler(hpa6, pods6, t)

		action := "유지"
		if scaled {
			if hpa6.CurrentReplicas > oldReplicas {
				action = fmt.Sprintf("UP (%d→%d)", oldReplicas, hpa6.CurrentReplicas)
			} else {
				action = fmt.Sprintf("DOWN (%d→%d)", oldReplicas, hpa6.CurrentReplicas)
			}
		}

		fmt.Printf("T+%3ds  %-8d  %-10d  %-10d  %s\n",
			i*30, cpu, hpa6.CurrentReplicas, hpa6.DesiredReplicas, action)
	}

	// =====================================================================
	// 요약
	// =====================================================================
	printHeader("요약: HPA 핵심 알고리즘")
	fmt.Println(`
  1. 핵심 공식: desiredReplicas = ceil(usageRatio * readyPodCount)
     - usageRatio = currentMetricValue / targetMetricValue
     - 실제: replica_calculator.go:118 → int32(math.Ceil(usageRatio * float64(readyPodCount)))

  2. Tolerance (기본 0.1): |usageRatio - 1.0| ≤ 0.1 → 변경 없음
     - 작은 변동에 의한 불필요한 스케일링(플래핑) 방지

  3. ScaleUp Limit: max(2 * currentReplicas, 4)
     - 한 번에 최대 2배까지만 확장 (급격한 확장으로 인한 자원 낭비 방지)
     - 실제: horizontal.go:62-63 (scaleUpLimitFactor, scaleUpLimitMinimum)

  4. Stabilization Window:
     - scaleDown: 최근 5분(기본) 중 최대 추천값 사용 → 급격한 축소 방지
     - scaleUp:   최근 0초(기본) → 즉시 확장 허용
     - 실제: horizontal.go의 stabilizeRecommendation()

  5. Min/Max Bounds: minReplicas ≤ desired ≤ maxReplicas
     - 최소 가용성 보장 + 자원 사용 상한 설정

  실제 소스 경로:
  - HPA 컨트롤러:    pkg/controller/podautoscaler/horizontal.go
  - 레플리카 계산:    pkg/controller/podautoscaler/replica_calculator.go
  - HPA API 타입:    staging/src/k8s.io/api/autoscaling/v2/types.go`)
}

// hpa5b는 데모 5에서 재사용하기 위한 헬퍼
func hpa5b(hpa *HorizontalPodAutoscaler) *HorizontalPodAutoscaler {
	return hpa
}
