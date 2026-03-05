package main

import (
	"fmt"
	"strings"
	"sync"
)

// =============================================================================
// Kubernetes Resource Management 시뮬레이션
// ResourceQuota, LimitRange, Resource requests/limits
// 참조:
//   - ResourceQuota: staging/src/k8s.io/apiserver/pkg/admission/plugin/resourcequota/controller.go
//   - LimitRange: plugin/pkg/admission/limitranger/admission.go
//   - Pod 평가자: pkg/quota/v1/evaluator/core/pods.go
// =============================================================================

// --- Resource 타입 ---

type ResourceName string

const (
	ResourceCPU              ResourceName = "cpu"
	ResourceMemory           ResourceName = "memory"
	ResourcePods             ResourceName = "pods"
	ResourceServices         ResourceName = "services"
	ResourceRequestsCPU      ResourceName = "requests.cpu"
	ResourceRequestsMemory   ResourceName = "requests.memory"
	ResourceLimitsCPU        ResourceName = "limits.cpu"
	ResourceLimitsMemory     ResourceName = "limits.memory"
	ResourceServicesNodePort ResourceName = "services.nodeports"
	ResourceServicesLB       ResourceName = "services.loadbalancers"
)

type ResourceList map[ResourceName]int64

// --- LimitRange ---
// 실제 구현: plugin/pkg/admission/limitranger/admission.go

type LimitType string

const (
	LimitTypePod       LimitType = "Pod"
	LimitTypeContainer LimitType = "Container"
)

type LimitRangeItem struct {
	Type           LimitType
	Min            ResourceList
	Max            ResourceList
	Default        ResourceList // 기본 Limits
	DefaultRequest ResourceList // 기본 Requests
}

type LimitRange struct {
	Namespace string
	Limits    []LimitRangeItem
}

type Container struct {
	Name     string
	Requests ResourceList
	Limits   ResourceList
}

type Pod struct {
	Name       string
	Namespace  string
	Containers []Container
}

// LimitRanger Admission Plugin 시뮬레이션

type LimitRanger struct {
	mu          sync.Mutex
	limitRanges map[string][]LimitRange // namespace → []LimitRange
}

func NewLimitRanger() *LimitRanger {
	return &LimitRanger{
		limitRanges: make(map[string][]LimitRange),
	}
}

func (lr *LimitRanger) AddLimitRange(l LimitRange) {
	lr.limitRanges[l.Namespace] = append(lr.limitRanges[l.Namespace], l)
}

// Admit (Mutation): 기본값 설정
// 실제: admission.go:111-113, defaultContainerResourceRequirements:216-233
func (lr *LimitRanger) Admit(pod *Pod) {
	lr.mu.Lock()
	defer lr.mu.Unlock()

	ranges := lr.limitRanges[pod.Namespace]
	for _, lr := range ranges {
		for _, item := range lr.Limits {
			if item.Type != LimitTypeContainer {
				continue
			}
			for i := range pod.Containers {
				c := &pod.Containers[i]
				if c.Requests == nil {
					c.Requests = make(ResourceList)
				}
				if c.Limits == nil {
					c.Limits = make(ResourceList)
				}

				// DefaultRequest 적용
				for name, val := range item.DefaultRequest {
					if _, exists := c.Requests[name]; !exists {
						c.Requests[name] = val
						fmt.Printf("    [LimitRange Admit] %s.requests.%s = %d (기본값)\n",
							c.Name, name, val)
					}
				}

				// Default (Limits) 적용
				for name, val := range item.Default {
					if _, exists := c.Limits[name]; !exists {
						c.Limits[name] = val
						fmt.Printf("    [LimitRange Admit] %s.limits.%s = %d (기본값)\n",
							c.Name, name, val)
					}
				}
			}
		}
	}
}

// Validate: 제약 검증
// 실제: admission.go:116-118, minConstraint:309-324, maxConstraint:342-357
func (lr *LimitRanger) Validate(pod *Pod) error {
	lr.mu.Lock()
	defer lr.mu.Unlock()

	ranges := lr.limitRanges[pod.Namespace]
	for _, lr := range ranges {
		for _, item := range lr.Limits {
			if item.Type != LimitTypeContainer {
				continue
			}
			for _, c := range pod.Containers {
				// Min 검증
				for name, minVal := range item.Min {
					if req, ok := c.Requests[name]; ok && req < minVal {
						return fmt.Errorf("%s.requests.%s (%d) < min (%d)",
							c.Name, name, req, minVal)
					}
				}

				// Max 검증
				for name, maxVal := range item.Max {
					if lim, ok := c.Limits[name]; ok && lim > maxVal {
						return fmt.Errorf("%s.limits.%s (%d) > max (%d)",
							c.Name, name, lim, maxVal)
					}
				}
			}
		}
	}
	return nil
}

// --- ResourceQuota ---
// 실제 구현: staging/src/k8s.io/apiserver/pkg/admission/plugin/resourcequota/controller.go

type ResourceQuotaSpec struct {
	Hard ResourceList
}

type ResourceQuotaStatus struct {
	Hard ResourceList
	Used ResourceList
}

type ResourceQuota struct {
	Name      string
	Namespace string
	Spec      ResourceQuotaSpec
	Status    ResourceQuotaStatus
}

// ResourceQuota Evaluator 시뮬레이션
// 실제: controller.go:411-635 (CheckRequest)

type QuotaEvaluator struct {
	mu     sync.Mutex
	quotas map[string]*ResourceQuota // namespace → quota
}

func NewQuotaEvaluator() *QuotaEvaluator {
	return &QuotaEvaluator{
		quotas: make(map[string]*ResourceQuota),
	}
}

func (qe *QuotaEvaluator) AddQuota(q *ResourceQuota) {
	qe.quotas[q.Namespace] = q
}

// CheckRequest: 할당량 초과 검사
// 실제: controller.go:411-635
func (qe *QuotaEvaluator) CheckRequest(pod *Pod) error {
	qe.mu.Lock()
	defer qe.mu.Unlock()

	quota, ok := qe.quotas[pod.Namespace]
	if !ok {
		return nil // 할당량 없음 → 통과
	}

	// Pod 리소스 사용량 계산
	usage := qe.calculatePodUsage(pod)

	// 할당량 초과 여부 확인
	var exceeded []string
	for name, required := range usage {
		hard, exists := quota.Status.Hard[name]
		if !exists {
			continue
		}
		used := quota.Status.Used[name]
		if used+required > hard {
			exceeded = append(exceeded, fmt.Sprintf(
				"%s: requested=%d, used=%d, hard=%d",
				name, required, used, hard))
		}
	}

	if len(exceeded) > 0 {
		return fmt.Errorf("quota exceeded: %s", strings.Join(exceeded, "; "))
	}

	// 사용량 업데이트
	for name, required := range usage {
		quota.Status.Used[name] += required
	}
	return nil
}

// Pod 리소스 사용량 계산
// 실제: pods.go:381-411 (PodUsageFunc)
func (qe *QuotaEvaluator) calculatePodUsage(pod *Pod) ResourceList {
	usage := make(ResourceList)

	// pods 카운트
	usage[ResourcePods] = 1

	// 컨테이너별 리소스 합산
	for _, c := range pod.Containers {
		if cpu, ok := c.Requests[ResourceCPU]; ok {
			usage[ResourceRequestsCPU] += cpu
		}
		if mem, ok := c.Requests[ResourceMemory]; ok {
			usage[ResourceRequestsMemory] += mem
		}
		if cpu, ok := c.Limits[ResourceCPU]; ok {
			usage[ResourceLimitsCPU] += cpu
		}
		if mem, ok := c.Limits[ResourceMemory]; ok {
			usage[ResourceLimitsMemory] += mem
		}
	}

	return usage
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	fmt.Println("=== Kubernetes Resource Management 시뮬레이션 ===")
	fmt.Println()

	// 1. LimitRange Defaulting
	demo1_LimitRange_Admit()

	// 2. LimitRange Validation
	demo2_LimitRange_Validate()

	// 3. ResourceQuota CheckRequest
	demo3_ResourceQuota()

	// 4. 전체 Admission 흐름
	demo4_FullAdmission()

	printSummary()
}

func demo1_LimitRange_Admit() {
	fmt.Println("--- 1. LimitRange Defaulting (Mutation) ---")

	lr := NewLimitRanger()
	lr.AddLimitRange(LimitRange{
		Namespace: "production",
		Limits: []LimitRangeItem{
			{
				Type: LimitTypeContainer,
				DefaultRequest: ResourceList{
					ResourceCPU:    100,  // 100m
					ResourceMemory: 128 * 1024 * 1024,
				},
				Default: ResourceList{
					ResourceCPU:    200,  // 200m
					ResourceMemory: 256 * 1024 * 1024,
				},
			},
		},
	})

	pod := &Pod{
		Name:      "app",
		Namespace: "production",
		Containers: []Container{
			{Name: "web"}, // requests/limits 미지정
		},
	}

	fmt.Printf("  Before: requests=%v, limits=%v\n",
		pod.Containers[0].Requests, pod.Containers[0].Limits)
	lr.Admit(pod)
	fmt.Printf("  After: requests.cpu=%d, limits.cpu=%d\n",
		pod.Containers[0].Requests[ResourceCPU],
		pod.Containers[0].Limits[ResourceCPU])
	fmt.Println()
}

func demo2_LimitRange_Validate() {
	fmt.Println("--- 2. LimitRange Validation ---")

	lr := NewLimitRanger()
	lr.AddLimitRange(LimitRange{
		Namespace: "production",
		Limits: []LimitRangeItem{
			{
				Type: LimitTypeContainer,
				Min:  ResourceList{ResourceCPU: 50, ResourceMemory: 64 * 1024 * 1024},
				Max:  ResourceList{ResourceCPU: 4000, ResourceMemory: 8 * 1024 * 1024 * 1024},
			},
		},
	})

	// 정상 Pod
	pod1 := &Pod{
		Name:      "normal",
		Namespace: "production",
		Containers: []Container{
			{Name: "app",
				Requests: ResourceList{ResourceCPU: 100, ResourceMemory: 128 * 1024 * 1024},
				Limits:   ResourceList{ResourceCPU: 500, ResourceMemory: 512 * 1024 * 1024}},
		},
	}
	err := lr.Validate(pod1)
	fmt.Printf("  정상 Pod: err=%v\n", err)

	// 초과 Pod
	pod2 := &Pod{
		Name:      "too-big",
		Namespace: "production",
		Containers: []Container{
			{Name: "app",
				Requests: ResourceList{ResourceCPU: 100},
				Limits:   ResourceList{ResourceCPU: 16000}}, // 16 코어 > max 4 코어
		},
	}
	err = lr.Validate(pod2)
	fmt.Printf("  초과 Pod: err=%v\n", err)

	// 미달 Pod
	pod3 := &Pod{
		Name:      "too-small",
		Namespace: "production",
		Containers: []Container{
			{Name: "app",
				Requests: ResourceList{ResourceCPU: 10}}, // 10m < min 50m
		},
	}
	err = lr.Validate(pod3)
	fmt.Printf("  미달 Pod: err=%v\n", err)
	fmt.Println()
}

func demo3_ResourceQuota() {
	fmt.Println("--- 3. ResourceQuota 할당량 검사 ---")

	qe := NewQuotaEvaluator()
	qe.AddQuota(&ResourceQuota{
		Name:      "compute-quota",
		Namespace: "team-a",
		Spec: ResourceQuotaSpec{
			Hard: ResourceList{
				ResourcePods:           10,
				ResourceRequestsCPU:    4000, // 4 cores
				ResourceRequestsMemory: 8 * 1024 * 1024 * 1024,
			},
		},
		Status: ResourceQuotaStatus{
			Hard: ResourceList{
				ResourcePods:           10,
				ResourceRequestsCPU:    4000,
				ResourceRequestsMemory: 8 * 1024 * 1024 * 1024,
			},
			Used: ResourceList{
				ResourcePods:           7,
				ResourceRequestsCPU:    3500, // 3.5 cores 사용 중
				ResourceRequestsMemory: 7 * 1024 * 1024 * 1024,
			},
		},
	})

	// 적은 리소스 요청
	pod1 := &Pod{
		Name:      "small-pod",
		Namespace: "team-a",
		Containers: []Container{
			{Name: "app", Requests: ResourceList{ResourceCPU: 100, ResourceMemory: 128 * 1024 * 1024}},
		},
	}
	err := qe.CheckRequest(pod1)
	fmt.Printf("  small-pod (100m CPU): err=%v\n", err)

	// 큰 리소스 요청 (할당량 초과)
	pod2 := &Pod{
		Name:      "big-pod",
		Namespace: "team-a",
		Containers: []Container{
			{Name: "app", Requests: ResourceList{ResourceCPU: 1000, ResourceMemory: 2 * 1024 * 1024 * 1024}},
		},
	}
	err = qe.CheckRequest(pod2)
	fmt.Printf("  big-pod (1000m CPU): err=%v\n", err)
	fmt.Println()
}

func demo4_FullAdmission() {
	fmt.Println("--- 4. 전체 Admission 흐름 ---")
	fmt.Println("  요청 → LimitRange Admit(기본값) → LimitRange Validate(제약) → ResourceQuota(할당)")

	lr := NewLimitRanger()
	lr.AddLimitRange(LimitRange{
		Namespace: "dev",
		Limits: []LimitRangeItem{
			{
				Type:           LimitTypeContainer,
				Min:            ResourceList{ResourceCPU: 50},
				Max:            ResourceList{ResourceCPU: 2000},
				DefaultRequest: ResourceList{ResourceCPU: 100},
				Default:        ResourceList{ResourceCPU: 200},
			},
		},
	})

	qe := NewQuotaEvaluator()
	qe.AddQuota(&ResourceQuota{
		Name:      "dev-quota",
		Namespace: "dev",
		Status: ResourceQuotaStatus{
			Hard: ResourceList{ResourcePods: 5, ResourceRequestsCPU: 1000},
			Used: ResourceList{ResourcePods: 0, ResourceRequestsCPU: 0},
		},
	})

	pods := []*Pod{
		{Name: "pod-1", Namespace: "dev", Containers: []Container{{Name: "app"}}},
		{Name: "pod-2", Namespace: "dev", Containers: []Container{{Name: "app"}}},
		{Name: "pod-3", Namespace: "dev", Containers: []Container{{Name: "app"}}},
	}

	for _, pod := range pods {
		fmt.Printf("\n  처리: %s\n", pod.Name)

		// 1. LimitRange Admit
		lr.Admit(pod)

		// 2. LimitRange Validate
		if err := lr.Validate(pod); err != nil {
			fmt.Printf("    ✗ Validate 실패: %v\n", err)
			continue
		}
		fmt.Printf("    ✓ Validate 통과\n")

		// 3. ResourceQuota
		if err := qe.CheckRequest(pod); err != nil {
			fmt.Printf("    ✗ Quota 초과: %v\n", err)
			continue
		}
		fmt.Printf("    ✓ Quota 통과 (requests.cpu=%d 할당)\n",
			pod.Containers[0].Requests[ResourceCPU])
	}
	fmt.Println()
}

func printSummary() {
	fmt.Println("=== 핵심 정리 ===")
	items := []string{
		"1. LimitRange는 Admission Plugin으로 Mutation(기본값) + Validation(제약) 수행",
		"2. ResourceQuota는 네임스페이스별 리소스 총량을 제한한다",
		"3. 처리 순서: LimitRange Admit → LimitRange Validate → ResourceQuota Check",
		"4. Pod 사용량 = 모든 컨테이너의 requests/limits 합산",
		"5. Scope로 Terminating/BestEffort 등 특정 Pod만 제한 가능",
		"6. LimitRange Default로 리소스 미지정 컨테이너에 자동 기본값 설정",
	}
	for _, item := range items {
		fmt.Printf("  %s\n", item)
	}
	fmt.Println()
	fmt.Println("소스코드 참조:")
	fmt.Println("  - LimitRange:    plugin/pkg/admission/limitranger/admission.go")
	fmt.Println("  - ResourceQuota: staging/src/k8s.io/apiserver/pkg/admission/plugin/resourcequota/controller.go")
	fmt.Println("  - Pod 평가자:    pkg/quota/v1/evaluator/core/pods.go")
}
