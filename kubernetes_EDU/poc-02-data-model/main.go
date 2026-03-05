// poc-02-data-model: 쿠버네티스 데이터 모델 패턴 구현
//
// 쿠버네티스의 모든 API 리소스가 따르는 TypeMeta + ObjectMeta + Spec + Status 패턴을
// 충실히 재현한다. 또한 OwnerReference를 이용한 가비지 컬렉션,
// ResourceVersion을 이용한 낙관적 동시성 제어, 라벨 셀렉터 매칭을 구현한다.
//
// 참조 소스:
//   - staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go (TypeMeta, ObjectMeta)
//   - staging/src/k8s.io/api/core/v1/types.go (Pod, Service)
//   - staging/src/k8s.io/api/apps/v1/types.go (Deployment, ReplicaSet)
//   - pkg/controller/garbagecollector/garbagecollector.go (GC)
//
// 실행: go run main.go
package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// TypeMeta — 리소스의 종류와 API 버전을 식별한다.
// 실제 소스: staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go:42
// ============================================================================

// TypeMeta는 모든 K8s 리소스가 내장하는 타입 정보이다.
type TypeMeta struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
}

// ============================================================================
// ObjectMeta — 모든 리소스의 공통 메타데이터.
// 실제 소스: staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go:111
// ============================================================================

// OwnerReference는 이 리소스를 소유하는 리소스에 대한 참조이다.
// 소유자가 삭제되면 가비지 컬렉터가 이 리소스도 삭제한다.
type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller,omitempty"` // true이면 managing controller
}

// ObjectMeta는 모든 K8s 리소스가 가지는 공통 메타데이터이다.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	Generation        int64             `json:"generation"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty"`
}

// ============================================================================
// Object 인터페이스 — 모든 K8s 리소스가 구현해야 하는 인터페이스
// ============================================================================

// Object는 모든 K8s 리소스가 구현하는 인터페이스이다.
type Object interface {
	GetTypeMeta() TypeMeta
	GetObjectMeta() *ObjectMeta
}

// ============================================================================
// Pod — 최소 배포 단위
// 실제 소스: staging/src/k8s.io/api/core/v1/types.go:5463
// ============================================================================

// ContainerSpec은 컨테이너 스펙이다.
type ContainerSpec struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Resources ResourceRequirements `json:"resources,omitempty"`
}

// ResourceRequirements는 컨테이너의 리소스 요구사항이다.
type ResourceRequirements struct {
	Requests ResourceList `json:"requests,omitempty"`
	Limits   ResourceList `json:"limits,omitempty"`
}

// ResourceList는 리소스 이름 → 값 매핑이다.
type ResourceList map[string]string

// PodSpec은 파드의 원하는 상태를 정의한다.
type PodSpec struct {
	Containers []ContainerSpec `json:"containers"`
	NodeName   string          `json:"nodeName,omitempty"`
}

// PodStatus는 파드의 현재 상태를 나타낸다.
type PodStatus struct {
	Phase  string `json:"phase"`
	PodIP  string `json:"podIP,omitempty"`
	HostIP string `json:"hostIP,omitempty"`
}

// Pod는 K8s의 최소 배포 단위이다.
type Pod struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       PodSpec   `json:"spec"`
	Status     PodStatus `json:"status"`
}

func (p *Pod) GetTypeMeta() TypeMeta     { return p.TypeMeta }
func (p *Pod) GetObjectMeta() *ObjectMeta { return &p.ObjectMeta }

// ============================================================================
// Service — 파드 그룹에 대한 네트워크 추상화
// 실제 소스: staging/src/k8s.io/api/core/v1/types.go
// ============================================================================

// ServiceSpec은 서비스의 원하는 상태이다.
type ServiceSpec struct {
	Selector map[string]string `json:"selector"`
	Ports    []ServicePort     `json:"ports"`
	Type     string            `json:"type"` // ClusterIP, NodePort, LoadBalancer
}

// ServicePort는 서비스 포트 정의이다.
type ServicePort struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	TargetPort int    `json:"targetPort"`
	Protocol   string `json:"protocol"`
}

// ServiceStatus는 서비스의 현재 상태이다.
type ServiceStatus struct {
	ClusterIP string `json:"clusterIP,omitempty"`
}

// Service는 파드 그룹에 대한 안정적인 네트워크 엔드포인트이다.
type Service struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       ServiceSpec   `json:"spec"`
	Status     ServiceStatus `json:"status"`
}

func (s *Service) GetTypeMeta() TypeMeta     { return s.TypeMeta }
func (s *Service) GetObjectMeta() *ObjectMeta { return &s.ObjectMeta }

// ============================================================================
// Deployment + ReplicaSet — 선언적 배포 관리
// 실제 소스: staging/src/k8s.io/api/apps/v1/types.go
// ============================================================================

// DeploymentSpec은 디플로이먼트의 원하는 상태이다.
type DeploymentSpec struct {
	Replicas int               `json:"replicas"`
	Selector map[string]string `json:"selector"`
	Template PodTemplateSpec   `json:"template"`
}

// PodTemplateSpec은 파드를 생성할 때 사용할 템플릿이다.
type PodTemplateSpec struct {
	Labels     map[string]string `json:"labels"`
	Containers []ContainerSpec   `json:"containers"`
}

// DeploymentStatus는 디플로이먼트의 현재 상태이다.
type DeploymentStatus struct {
	Replicas          int `json:"replicas"`
	ReadyReplicas     int `json:"readyReplicas"`
	AvailableReplicas int `json:"availableReplicas"`
}

// Deployment는 ReplicaSet을 관리하는 상위 리소스이다.
type Deployment struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       DeploymentSpec   `json:"spec"`
	Status     DeploymentStatus `json:"status"`
}

func (d *Deployment) GetTypeMeta() TypeMeta     { return d.TypeMeta }
func (d *Deployment) GetObjectMeta() *ObjectMeta { return &d.ObjectMeta }

// ReplicaSetSpec은 ReplicaSet의 원하는 상태이다.
type ReplicaSetSpec struct {
	Replicas int               `json:"replicas"`
	Selector map[string]string `json:"selector"`
	Template PodTemplateSpec   `json:"template"`
}

// ReplicaSetStatus는 ReplicaSet의 현재 상태이다.
type ReplicaSetStatus struct {
	Replicas      int `json:"replicas"`
	ReadyReplicas int `json:"readyReplicas"`
}

// ReplicaSet은 동일한 파드의 집합을 관리한다.
type ReplicaSet struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       ReplicaSetSpec   `json:"spec"`
	Status     ReplicaSetStatus `json:"status"`
}

func (rs *ReplicaSet) GetTypeMeta() TypeMeta     { return rs.TypeMeta }
func (rs *ReplicaSet) GetObjectMeta() *ObjectMeta { return &rs.ObjectMeta }

// ============================================================================
// 라벨 셀렉터 — K8s의 핵심 연결 메커니즘
// 실제 소스: staging/src/k8s.io/apimachinery/pkg/labels/selector.go
// ============================================================================

// LabelSelector는 라벨 기반 필터링을 수행한다.
type LabelSelector struct {
	MatchLabels map[string]string
}

// Matches는 주어진 라벨 집합이 셀렉터와 일치하는지 확인한다.
// 셀렉터의 모든 키-값 쌍이 대상 라벨에 존재해야 매칭된다 (AND 연산).
func (ls *LabelSelector) Matches(labels map[string]string) bool {
	for key, value := range ls.MatchLabels {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// ============================================================================
// 인메모리 스토리지 — etcd 역할
// ============================================================================

// Storage는 인메모리 K8s 오브젝트 저장소이다.
type Storage struct {
	mu              sync.RWMutex
	objects         map[string]Object // kind/namespace/name → Object
	resourceVersion int64
}

// NewStorage는 새 스토리지를 생성한다.
func NewStorage() *Storage {
	return &Storage{
		objects: make(map[string]Object),
	}
}

// objectKey는 오브젝트의 고유 키를 생성한다.
func objectKey(obj Object) string {
	tm := obj.GetTypeMeta()
	om := obj.GetObjectMeta()
	return fmt.Sprintf("%s/%s/%s", tm.Kind, om.Namespace, om.Name)
}

// generateUID는 간단한 UID를 생성한다.
func generateUID() string {
	const chars = "abcdef0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return fmt.Sprintf("%s-%s-%s", string(b[:4]), string(b[4:6]), string(b[6:]))
}

// Create는 오브젝트를 생성한다.
func (s *Storage) Create(obj Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := objectKey(obj)
	if _, exists := s.objects[key]; exists {
		return fmt.Errorf("이미 존재: %s", key)
	}

	om := obj.GetObjectMeta()
	om.UID = generateUID()
	s.resourceVersion++
	om.ResourceVersion = fmt.Sprintf("%d", s.resourceVersion)
	om.CreationTimestamp = time.Now()

	s.objects[key] = obj
	return nil
}

// Update는 오브젝트를 업데이트한다. 낙관적 동시성 제어를 적용한다.
func (s *Storage) Update(obj Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := objectKey(obj)
	existing, exists := s.objects[key]
	if !exists {
		return fmt.Errorf("존재하지 않음: %s", key)
	}

	// 낙관적 동시성 제어: ResourceVersion 비교
	om := obj.GetObjectMeta()
	existingOM := existing.GetObjectMeta()
	if om.ResourceVersion != "" && om.ResourceVersion != existingOM.ResourceVersion {
		return fmt.Errorf("충돌: resourceVersion 불일치 (요청=%s, 현재=%s)",
			om.ResourceVersion, existingOM.ResourceVersion)
	}

	s.resourceVersion++
	om.ResourceVersion = fmt.Sprintf("%d", s.resourceVersion)
	om.Generation++
	s.objects[key] = obj
	return nil
}

// Get은 오브젝트를 조회한다.
func (s *Storage) Get(kind, namespace, name string) (Object, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	obj, ok := s.objects[key]
	return obj, ok
}

// List는 특정 종류의 모든 오브젝트를 반환한다. kind가 빈 문자열이면 모든 오브젝트를 반환한다.
func (s *Storage) List(kind string) []Object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Object
	for k, obj := range s.objects {
		if kind == "" || strings.HasPrefix(k, kind+"/") {
			result = append(result, obj)
		}
	}
	return result
}

// Delete는 오브젝트를 삭제한다.
func (s *Storage) Delete(kind, namespace, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s/%s/%s", kind, namespace, name)
	if _, ok := s.objects[key]; ok {
		delete(s.objects, key)
		return true
	}
	return false
}

// ListByOwner는 특정 UID를 소유자로 참조하는 모든 오브젝트를 반환한다.
func (s *Storage) ListByOwner(ownerUID string) []Object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Object
	for _, obj := range s.objects {
		om := obj.GetObjectMeta()
		for _, ref := range om.OwnerReferences {
			if ref.UID == ownerUID {
				result = append(result, obj)
				break
			}
		}
	}
	return result
}

// ============================================================================
// 가비지 컬렉터 — OwnerReference 기반 계단식 삭제
// 실제 소스: pkg/controller/garbagecollector/garbagecollector.go
// ============================================================================

// GarbageCollector는 소유자가 삭제된 오브젝트를 자동으로 정리한다.
type GarbageCollector struct {
	storage *Storage
}

// CollectGarbage는 소유자가 없는 오브젝트를 찾아서 삭제한다.
// 실제 K8s에서는 foreground/background/orphan 삭제 정책이 있다.
func (gc *GarbageCollector) CollectGarbage() []string {
	var deleted []string
	allObjects := gc.storage.List("") // 모든 종류

	// 모든 오브젝트를 순회하며 실제로는 전체 리스트가 아닌 키 기반으로
	for _, obj := range allObjects {
		om := obj.GetObjectMeta()
		for _, ref := range om.OwnerReferences {
			// 소유자가 존재하는지 확인
			if _, exists := gc.storage.Get(ref.Kind, om.Namespace, ref.Name); !exists {
				// 소유자가 없으면 이 오브젝트도 삭제 (cascade)
				tm := obj.GetTypeMeta()
				key := fmt.Sprintf("%s/%s/%s", tm.Kind, om.Namespace, om.Name)
				gc.storage.Delete(tm.Kind, om.Namespace, om.Name)
				deleted = append(deleted, key)
			}
		}
	}
	return deleted
}

// ============================================================================
// 데모 실행
// ============================================================================

func main() {
	fmt.Println("========================================")
	fmt.Println("Kubernetes 데이터 모델 패턴 시뮬레이션")
	fmt.Println("========================================")
	fmt.Println()

	storage := NewStorage()

	// ── 1단계: Deployment → ReplicaSet → Pod 계층 구조 생성 ──
	fmt.Println("── 1단계: 리소스 계층 구조 생성 ──")
	fmt.Println("  Deployment → ReplicaSet → Pod (OwnerReference 체인)")
	fmt.Println()

	// Deployment 생성
	deploy := &Deployment{
		TypeMeta:   TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: ObjectMeta{Name: "web-app", Namespace: "default"},
		Spec: DeploymentSpec{
			Replicas: 3,
			Selector: map[string]string{"app": "web", "version": "v1"},
			Template: PodTemplateSpec{
				Labels: map[string]string{"app": "web", "version": "v1"},
				Containers: []ContainerSpec{
					{Name: "nginx", Image: "nginx:1.25", Resources: ResourceRequirements{
						Requests: ResourceList{"cpu": "100m", "memory": "128Mi"},
						Limits:   ResourceList{"cpu": "200m", "memory": "256Mi"},
					}},
				},
			},
		},
	}
	storage.Create(deploy)
	fmt.Printf("  [생성] Deployment: %s (UID=%s, rv=%s)\n",
		deploy.Name, deploy.UID, deploy.ResourceVersion)

	// ReplicaSet 생성 (Deployment가 소유)
	boolTrue := true
	rs := &ReplicaSet{
		TypeMeta:   TypeMeta{Kind: "ReplicaSet", APIVersion: "apps/v1"},
		ObjectMeta: ObjectMeta{
			Name:      "web-app-7d4f8b6c",
			Namespace: "default",
			Labels:    map[string]string{"app": "web", "version": "v1"},
			OwnerReferences: []OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       deploy.Name,
					UID:        deploy.UID,
					Controller: &boolTrue,
				},
			},
		},
		Spec: ReplicaSetSpec{
			Replicas: 3,
			Selector: map[string]string{"app": "web", "version": "v1"},
		},
	}
	storage.Create(rs)
	fmt.Printf("  [생성] ReplicaSet: %s (UID=%s, 소유자=%s)\n",
		rs.Name, rs.UID, rs.OwnerReferences[0].Name)

	// Pod 3개 생성 (ReplicaSet이 소유)
	for i := 0; i < 3; i++ {
		pod := &Pod{
			TypeMeta: TypeMeta{Kind: "Pod", APIVersion: "v1"},
			ObjectMeta: ObjectMeta{
				Name:      fmt.Sprintf("web-app-7d4f8b6c-%s", randomSuffix()),
				Namespace: "default",
				Labels:    map[string]string{"app": "web", "version": "v1", "pod-template-hash": "7d4f8b6c"},
				OwnerReferences: []OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "ReplicaSet",
						Name:       rs.Name,
						UID:        rs.UID,
						Controller: &boolTrue,
					},
				},
			},
			Spec: PodSpec{
				Containers: []ContainerSpec{
					{Name: "nginx", Image: "nginx:1.25"},
				},
				NodeName: fmt.Sprintf("node-%d", i+1),
			},
			Status: PodStatus{Phase: "Running", PodIP: fmt.Sprintf("10.0.%d.%d", i, rand.Intn(255))},
		}
		storage.Create(pod)
		fmt.Printf("  [생성] Pod: %s (소유자=%s, 노드=%s)\n",
			pod.Name, pod.OwnerReferences[0].Name, pod.Spec.NodeName)
	}

	// Service 생성
	svc := &Service{
		TypeMeta:   TypeMeta{Kind: "Service", APIVersion: "v1"},
		ObjectMeta: ObjectMeta{
			Name:      "web-service",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []ServicePort{{Name: "http", Port: 80, TargetPort: 8080, Protocol: "TCP"}},
			Type:     "ClusterIP",
		},
		Status: ServiceStatus{ClusterIP: "10.96.0.100"},
	}
	storage.Create(svc)
	fmt.Printf("  [생성] Service: %s (selector=%v)\n", svc.Name, svc.Spec.Selector)

	// ── 2단계: 라벨 셀렉터 매칭 ──
	fmt.Println()
	fmt.Println("── 2단계: 라벨 셀렉터 매칭 ──")

	selector := &LabelSelector{MatchLabels: map[string]string{"app": "web", "version": "v1"}}
	fmt.Printf("  셀렉터: %v\n", selector.MatchLabels)

	pods := storage.List("Pod")
	fmt.Printf("  매칭되는 파드:\n")
	for _, obj := range pods {
		om := obj.GetObjectMeta()
		if selector.Matches(om.Labels) {
			fmt.Printf("    - %s (labels=%v)\n", om.Name, om.Labels)
		}
	}

	// 다른 라벨로 매칭 테스트
	svcSelector := &LabelSelector{MatchLabels: svc.Spec.Selector}
	fmt.Printf("\n  Service 셀렉터: %v\n", svcSelector.MatchLabels)
	fmt.Printf("  매칭되는 파드:\n")
	for _, obj := range pods {
		om := obj.GetObjectMeta()
		if svcSelector.Matches(om.Labels) {
			pod := obj.(*Pod)
			fmt.Printf("    - %s → %s:%d\n", om.Name, pod.Status.PodIP, svc.Spec.Ports[0].TargetPort)
		}
	}

	// ── 3단계: 낙관적 동시성 제어 ──
	fmt.Println()
	fmt.Println("── 3단계: 낙관적 동시성 제어 (ResourceVersion) ──")

	// 파드 하나를 가져와서 업데이트 시도
	firstPod := pods[0].(*Pod)
	fmt.Printf("  현재 파드: %s (rv=%s)\n", firstPod.Name, firstPod.ResourceVersion)

	// 정상 업데이트
	firstPod.Status.Phase = "Succeeded"
	err := storage.Update(firstPod)
	if err != nil {
		fmt.Printf("  업데이트 실패: %v\n", err)
	} else {
		fmt.Printf("  업데이트 성공: %s → rv=%s (Phase=Succeeded)\n", firstPod.Name, firstPod.ResourceVersion)
	}

	// 충돌 시뮬레이션: 두 클라이언트가 동시에 같은 오브젝트를 수정하려는 상황
	// 클라이언트 A가 오래된 rv="1"로 업데이트 시도 → 충돌
	stalePod := &Pod{
		TypeMeta:   TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: ObjectMeta{
			Name:            firstPod.Name,
			Namespace:       firstPod.Namespace,
			ResourceVersion: "1", // 오래된 리소스 버전
		},
		Status: PodStatus{Phase: "Failed"},
	}
	err = storage.Update(stalePod)
	if err != nil {
		fmt.Printf("  충돌 감지: %v\n", err)
	}

	// ── 4단계: OwnerReference 기반 가비지 컬렉션 ──
	fmt.Println()
	fmt.Println("── 4단계: OwnerReference 기반 가비지 컬렉션 (Cascade Delete) ──")

	// Deployment 소유 관계 트리 출력
	fmt.Println("  소유 관계 트리:")
	fmt.Printf("  Deployment/%s (UID=%s)\n", deploy.Name, deploy.UID)
	rsOwned := storage.ListByOwner(deploy.UID)
	for _, rsObj := range rsOwned {
		rsOM := rsObj.GetObjectMeta()
		fmt.Printf("    └─ ReplicaSet/%s (UID=%s)\n", rsOM.Name, rsOM.UID)
		podOwned := storage.ListByOwner(rsOM.UID)
		for _, podObj := range podOwned {
			podOM := podObj.GetObjectMeta()
			fmt.Printf("        └─ Pod/%s\n", podOM.Name)
		}
	}

	// Deployment 삭제 → GC가 ReplicaSet, Pod도 정리
	fmt.Println()
	fmt.Printf("  [삭제] Deployment/%s 삭제\n", deploy.Name)
	storage.Delete("Deployment", "default", deploy.Name)

	gc := &GarbageCollector{storage: storage}

	// 1차 GC: ReplicaSet 삭제 (소유자 Deployment 없음)
	deleted := gc.CollectGarbage()
	fmt.Printf("  [GC 1차] 삭제된 오브젝트: %v\n", deleted)

	// 2차 GC: Pod 삭제 (소유자 ReplicaSet 없음)
	deleted = gc.CollectGarbage()
	fmt.Printf("  [GC 2차] 삭제된 오브젝트: %v\n", deleted)

	// 남은 오브젝트 확인
	fmt.Println()
	fmt.Println("  GC 후 남은 오브젝트:")
	allObjects := storage.List("")
	if len(allObjects) == 0 {
		fmt.Println("    (없음 — 모든 소유 오브젝트가 정리됨)")
	}
	for _, obj := range allObjects {
		tm := obj.GetTypeMeta()
		om := obj.GetObjectMeta()
		fmt.Printf("    - %s/%s (소유자 없음 = 독립 리소스)\n", tm.Kind, om.Name)
	}

	// ── 5단계: JSON 직렬화 (API 호환 형식) ──
	fmt.Println()
	fmt.Println("── 5단계: JSON 직렬화 (K8s API 호환 형식) ──")

	samplePod := &Pod{
		TypeMeta:   TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: ObjectMeta{
			Name:      "sample-pod",
			Namespace: "default",
			UID:       generateUID(),
			Labels:    map[string]string{"app": "sample"},
		},
		Spec: PodSpec{
			Containers: []ContainerSpec{
				{
					Name:  "app",
					Image: "myapp:latest",
					Resources: ResourceRequirements{
						Requests: ResourceList{"cpu": "100m", "memory": "64Mi"},
					},
				},
			},
		},
		Status: PodStatus{Phase: "Running", PodIP: "10.0.0.42"},
	}

	jsonData, _ := json.MarshalIndent(samplePod, "  ", "  ")
	fmt.Printf("  %s\n", string(jsonData))

	// ── 핵심 정리 ──
	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("핵심 포인트:")
	fmt.Println("========================================")
	fmt.Println("1. TypeMeta + ObjectMeta + Spec + Status: 모든 K8s 리소스의 표준 구조")
	fmt.Println("2. OwnerReference: 리소스 간 소유 관계 → GC의 계단식 삭제 기반")
	fmt.Println("3. ResourceVersion: 낙관적 동시성 제어 (409 Conflict 방지)")
	fmt.Println("4. Label Selector: AND 매칭으로 리소스 간 느슨한 연결")
	fmt.Println("5. Spec vs Status: 원하는 상태(Spec)와 현재 상태(Status) 분리")
	fmt.Println("6. 계층 구조: Deployment → ReplicaSet → Pod (소유 체인)")
}

// randomSuffix는 K8s 스타일의 5자 랜덤 접미사를 생성한다.
func randomSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}
