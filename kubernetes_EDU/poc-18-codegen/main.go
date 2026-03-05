// Kubernetes 코드 생성 패턴 시뮬레이션
//
// Kubernetes는 반복적인 boilerplate 코드를 자동 생성한다.
// 이 PoC는 핵심 코드 생성 패턴을 Go 표준 라이브러리만으로 재현한다:
// 1. DeepCopy 생성 — reflect 기반 깊은 복사
// 2. Scheme과 타입 등록 — GVK → Go 타입 매핑
// 3. 버전 변환 — v1 ↔ internal 변환
// 4. Defaulting — 기본값 설정 함수
// 5. 타입별 클라이언트 생성 패턴

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// ============================================================================
// 1. 기본 타입 시스템 — TypeMeta, ObjectMeta
// ============================================================================

// GroupVersionKind는 API 객체의 타입을 식별한다
// 소스: staging/src/k8s.io/apimachinery/pkg/runtime/schema/group_version.go
type GroupVersionKind struct {
	Group   string
	Version string
	Kind    string
}

func (gvk GroupVersionKind) String() string {
	if gvk.Group == "" {
		return fmt.Sprintf("%s/%s", gvk.Version, gvk.Kind)
	}
	return fmt.Sprintf("%s/%s/%s", gvk.Group, gvk.Version, gvk.Kind)
}

// Object는 모든 API 객체가 구현하는 인터페이스
// 소스: staging/src/k8s.io/apimachinery/pkg/runtime/interfaces.go
type Object interface {
	GetObjectKind() *TypeMeta
	DeepCopyObject() Object
}

// TypeMeta는 API 객체의 타입 정보
type TypeMeta struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
}

// ObjectMeta는 모든 리소스의 공통 메타데이터
type ObjectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	ResourceVersion string            `json:"resourceVersion"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// ============================================================================
// 2. 내부(Internal) 타입 — 버전이 없는 허브 타입
// ============================================================================

// InternalPod는 내부 표현 (모든 버전의 공통 구조)
// Kubernetes에서 internal 타입은 버전이 없으며, 버전 간 변환의 허브 역할을 한다
type InternalPod struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       InternalPodSpec   `json:"spec"`
	Status     InternalPodStatus `json:"status"`
}

type InternalPodSpec struct {
	Containers    []InternalContainer `json:"containers"`
	NodeName      string              `json:"nodeName,omitempty"`
	RestartPolicy string              `json:"restartPolicy"`
	// 내부 타입에만 있는 필드 (예: 계산된 값)
	Priority int `json:"priority"`
}

type InternalContainer struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// 리소스 요청/제한 (내부 표현: 바이트 단위)
	CPURequestMillis    int64 `json:"cpuRequestMillis"`
	MemoryRequestBytes  int64 `json:"memoryRequestBytes"`
	CPULimitMillis      int64 `json:"cpuLimitMillis"`
	MemoryLimitBytes    int64 `json:"memoryLimitBytes"`
}

type InternalPodStatus struct {
	Phase      string `json:"phase"`
	PodIP      string `json:"podIP,omitempty"`
	HostIP     string `json:"hostIP,omitempty"`
	StartTime  string `json:"startTime,omitempty"`
}

func (p *InternalPod) GetObjectKind() *TypeMeta { return &p.TypeMeta }

// ============================================================================
// 3. DeepCopy 생성 — zz_generated.deepcopy.go 시뮬레이션
// ============================================================================

// 실제 Kubernetes에서는 deepcopy-gen 도구가 각 타입에 대해
// DeepCopy, DeepCopyInto, DeepCopyObject 메서드를 자동 생성한다.
// 생성 파일: zz_generated.deepcopy.go

// DeepCopyInto는 dst에 깊은 복사를 수행한다 (생성 코드 패턴)
func (in *InternalPod) DeepCopyInto(out *InternalPod) {
	*out = *in
	// ObjectMeta 깊은 복사 — map은 별도 복사 필요
	if in.Labels != nil {
		out.Labels = make(map[string]string, len(in.Labels))
		for k, v := range in.Labels {
			out.Labels[k] = v
		}
	}
	if in.Annotations != nil {
		out.Annotations = make(map[string]string, len(in.Annotations))
		for k, v := range in.Annotations {
			out.Annotations[k] = v
		}
	}
	// Spec.Containers 깊은 복사 — slice는 별도 복사 필요
	if in.Spec.Containers != nil {
		out.Spec.Containers = make([]InternalContainer, len(in.Spec.Containers))
		for i := range in.Spec.Containers {
			in.Spec.Containers[i].deepCopyInto(&out.Spec.Containers[i])
		}
	}
}

func (in *InternalContainer) deepCopyInto(out *InternalContainer) {
	*out = *in
	if in.Command != nil {
		out.Command = make([]string, len(in.Command))
		copy(out.Command, in.Command)
	}
	if in.Env != nil {
		out.Env = make(map[string]string, len(in.Env))
		for k, v := range in.Env {
			out.Env[k] = v
		}
	}
}

// DeepCopy는 새 객체를 반환한다
func (in *InternalPod) DeepCopy() *InternalPod {
	if in == nil {
		return nil
	}
	out := new(InternalPod)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject는 Object 인터페이스 구현
func (in *InternalPod) DeepCopyObject() Object {
	return in.DeepCopy()
}

// ============================================================================
// 4. 외부(Versioned) 타입 — v1alpha1, v1
// ============================================================================

// V1Alpha1Pod — 초기 API 버전 (필드 이름이 다를 수 있음)
type V1Alpha1Pod struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       V1Alpha1PodSpec   `json:"spec"`
	Status     V1Alpha1PodStatus `json:"status"`
}

type V1Alpha1PodSpec struct {
	Containers    []V1Alpha1Container `json:"containers"`
	NodeName      string              `json:"nodeName,omitempty"`
	RestartPolicy string              `json:"restartPolicy,omitempty"` // 기본값 없음 → defaulting 필요
	// v1alpha1에는 priority 필드가 없음
}

type V1Alpha1Container struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	// v1alpha1: 리소스를 문자열로 표현
	CPURequest    string `json:"cpuRequest,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
}

type V1Alpha1PodStatus struct {
	Phase string `json:"phase"`
	PodIP string `json:"podIP,omitempty"`
}

func (p *V1Alpha1Pod) GetObjectKind() *TypeMeta { return &p.TypeMeta }
func (p *V1Alpha1Pod) DeepCopyObject() Object {
	data, _ := json.Marshal(p)
	out := &V1Alpha1Pod{}
	json.Unmarshal(data, out)
	return out
}

// V1Pod — 안정화된 API 버전
type V1Pod struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Spec       V1PodSpec   `json:"spec"`
	Status     V1PodStatus `json:"status"`
}

type V1PodSpec struct {
	Containers    []V1Container `json:"containers"`
	NodeName      string        `json:"nodeName,omitempty"`
	RestartPolicy string        `json:"restartPolicy,omitempty"`
	Priority      *int          `json:"priority,omitempty"` // v1에서 추가됨
}

type V1Container struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	// v1: 구조화된 리소스 표현
	Resources ResourceRequirements `json:"resources,omitempty"`
}

type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type V1PodStatus struct {
	Phase     string `json:"phase"`
	PodIP     string `json:"podIP,omitempty"`
	HostIP    string `json:"hostIP,omitempty"`
	StartTime string `json:"startTime,omitempty"`
}

func (p *V1Pod) GetObjectKind() *TypeMeta { return &p.TypeMeta }
func (p *V1Pod) DeepCopyObject() Object {
	data, _ := json.Marshal(p)
	out := &V1Pod{}
	json.Unmarshal(data, out)
	return out
}

// ============================================================================
// 5. Scheme — 타입 등록 시스템
// ============================================================================

// Scheme은 GVK ↔ Go 타입 매핑, 변환 함수, 기본값 함수를 관리한다
// 소스: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go
type Scheme struct {
	mu sync.RWMutex
	// GVK → Go 타입
	gvkToType map[GroupVersionKind]reflect.Type
	// Go 타입 → GVK
	typeToGVK map[reflect.Type]GroupVersionKind
	// 변환 함수: (srcGVK, dstGVK) → converter
	converters map[conversionKey]ConvertFunc
	// 기본값 함수: GVK → defaulter
	defaulters map[GroupVersionKind]DefaultFunc
}

type conversionKey struct {
	src, dst GroupVersionKind
}

// ConvertFunc는 src 객체를 dst 객체로 변환한다
type ConvertFunc func(src, dst Object) error

// DefaultFunc는 객체에 기본값을 설정한다
type DefaultFunc func(obj Object)

// NewScheme은 새 Scheme을 생성한다
func NewScheme() *Scheme {
	return &Scheme{
		gvkToType:  make(map[GroupVersionKind]reflect.Type),
		typeToGVK:  make(map[reflect.Type]GroupVersionKind),
		converters: make(map[conversionKey]ConvertFunc),
		defaulters: make(map[GroupVersionKind]DefaultFunc),
	}
}

// AddKnownType은 GVK와 Go 타입을 매핑 등록한다
func (s *Scheme) AddKnownType(gvk GroupVersionKind, obj Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := reflect.TypeOf(obj)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	s.gvkToType[gvk] = t
	s.typeToGVK[t] = gvk
}

// NewObject는 GVK에 해당하는 새 객체를 생성한다
func (s *Scheme) NewObject(gvk GroupVersionKind) (Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.gvkToType[gvk]
	if !ok {
		return nil, fmt.Errorf("unknown GVK: %s", gvk)
	}
	obj := reflect.New(t).Interface().(Object)
	obj.GetObjectKind().Kind = gvk.Kind
	obj.GetObjectKind().APIVersion = gvk.Group + "/" + gvk.Version
	if gvk.Group == "" {
		obj.GetObjectKind().APIVersion = gvk.Version
	}
	return obj, nil
}

// ObjectKind는 Go 타입에서 GVK를 조회한다
func (s *Scheme) ObjectKind(obj Object) (GroupVersionKind, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := reflect.TypeOf(obj)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	gvk, ok := s.typeToGVK[t]
	if !ok {
		return GroupVersionKind{}, fmt.Errorf("unknown type: %s", t)
	}
	return gvk, nil
}

// AddConversionFunc는 변환 함수를 등록한다
func (s *Scheme) AddConversionFunc(src, dst GroupVersionKind, fn ConvertFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.converters[conversionKey{src, dst}] = fn
}

// Convert는 src를 dst GVK로 변환한다
func (s *Scheme) Convert(src Object, dstGVK GroupVersionKind) (Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	srcGVK, err := s.ObjectKind(src)
	if err != nil {
		return nil, err
	}

	// 같은 타입이면 복사만
	if srcGVK == dstGVK {
		return src.DeepCopyObject(), nil
	}

	// 직접 변환 함수 찾기
	fn, ok := s.converters[conversionKey{srcGVK, dstGVK}]
	if !ok {
		return nil, fmt.Errorf("no converter: %s → %s", srcGVK, dstGVK)
	}

	dst, err := s.NewObject(dstGVK)
	if err != nil {
		return nil, err
	}

	if err := fn(src, dst); err != nil {
		return nil, err
	}
	return dst, nil
}

// AddDefaultingFunc는 기본값 설정 함수를 등록한다
func (s *Scheme) AddDefaultingFunc(gvk GroupVersionKind, fn DefaultFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaulters[gvk] = fn
}

// Default는 객체에 기본값을 설정한다
func (s *Scheme) Default(obj Object) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	gvk, err := s.ObjectKind(obj)
	if err != nil {
		return
	}
	if fn, ok := s.defaulters[gvk]; ok {
		fn(obj)
	}
}

// ============================================================================
// 6. 변환 함수 등록 — zz_generated.conversion.go 시뮬레이션
// ============================================================================

// 실제 Kubernetes에서는 conversion-gen이 자동 생성한다.
// 필드 이름이 같으면 자동 매핑, 다르면 수동 변환 함수 작성.

// registerV1Alpha1Conversions — v1alpha1 ↔ internal 변환 등록
func registerV1Alpha1Conversions(s *Scheme) {
	v1alpha1GVK := GroupVersionKind{Group: "", Version: "v1alpha1", Kind: "Pod"}
	internalGVK := GroupVersionKind{Group: "", Version: "__internal", Kind: "Pod"}

	// v1alpha1 → internal
	s.AddConversionFunc(v1alpha1GVK, internalGVK, func(src, dst Object) error {
		in := src.(*V1Alpha1Pod)
		out := dst.(*InternalPod)

		out.TypeMeta = TypeMeta{Kind: "Pod", APIVersion: "__internal"}
		out.ObjectMeta = in.ObjectMeta

		// Spec 변환
		out.Spec.NodeName = in.Spec.NodeName
		out.Spec.RestartPolicy = in.Spec.RestartPolicy
		out.Spec.Priority = 0 // v1alpha1에는 priority 없음

		// Container 변환 (필드 구조가 다름)
		out.Spec.Containers = make([]InternalContainer, len(in.Spec.Containers))
		for i, c := range in.Spec.Containers {
			out.Spec.Containers[i] = InternalContainer{
				Name:    c.Name,
				Image:   c.Image,
				Command: c.Command,
			}
			// 문자열 리소스 → 숫자로 변환
			if c.CPURequest != "" {
				out.Spec.Containers[i].CPURequestMillis = parseMilliCPU(c.CPURequest)
			}
			if c.MemoryRequest != "" {
				out.Spec.Containers[i].MemoryRequestBytes = parseMemory(c.MemoryRequest)
			}
		}

		// Status 변환
		out.Status.Phase = in.Status.Phase
		out.Status.PodIP = in.Status.PodIP

		return nil
	})

	// internal → v1alpha1
	s.AddConversionFunc(internalGVK, v1alpha1GVK, func(src, dst Object) error {
		in := src.(*InternalPod)
		out := dst.(*V1Alpha1Pod)

		out.TypeMeta = TypeMeta{Kind: "Pod", APIVersion: "v1alpha1"}
		out.ObjectMeta = in.ObjectMeta
		out.Spec.NodeName = in.Spec.NodeName
		out.Spec.RestartPolicy = in.Spec.RestartPolicy
		// priority는 v1alpha1에 없으므로 손실됨

		out.Spec.Containers = make([]V1Alpha1Container, len(in.Spec.Containers))
		for i, c := range in.Spec.Containers {
			out.Spec.Containers[i] = V1Alpha1Container{
				Name:    c.Name,
				Image:   c.Image,
				Command: c.Command,
			}
			if c.CPURequestMillis > 0 {
				out.Spec.Containers[i].CPURequest = formatMilliCPU(c.CPURequestMillis)
			}
			if c.MemoryRequestBytes > 0 {
				out.Spec.Containers[i].MemoryRequest = formatMemory(c.MemoryRequestBytes)
			}
		}

		out.Status.Phase = in.Status.Phase
		out.Status.PodIP = in.Status.PodIP

		return nil
	})
}

// registerV1Conversions — v1 ↔ internal 변환 등록
func registerV1Conversions(s *Scheme) {
	v1GVK := GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}
	internalGVK := GroupVersionKind{Group: "", Version: "__internal", Kind: "Pod"}

	// v1 → internal
	s.AddConversionFunc(v1GVK, internalGVK, func(src, dst Object) error {
		in := src.(*V1Pod)
		out := dst.(*InternalPod)

		out.TypeMeta = TypeMeta{Kind: "Pod", APIVersion: "__internal"}
		out.ObjectMeta = in.ObjectMeta
		out.Spec.NodeName = in.Spec.NodeName
		out.Spec.RestartPolicy = in.Spec.RestartPolicy
		if in.Spec.Priority != nil {
			out.Spec.Priority = *in.Spec.Priority
		}

		out.Spec.Containers = make([]InternalContainer, len(in.Spec.Containers))
		for i, c := range in.Spec.Containers {
			out.Spec.Containers[i] = InternalContainer{
				Name:    c.Name,
				Image:   c.Image,
				Command: c.Command,
			}
			if req, ok := c.Resources.Requests["cpu"]; ok {
				out.Spec.Containers[i].CPURequestMillis = parseMilliCPU(req)
			}
			if req, ok := c.Resources.Requests["memory"]; ok {
				out.Spec.Containers[i].MemoryRequestBytes = parseMemory(req)
			}
			if lim, ok := c.Resources.Limits["cpu"]; ok {
				out.Spec.Containers[i].CPULimitMillis = parseMilliCPU(lim)
			}
			if lim, ok := c.Resources.Limits["memory"]; ok {
				out.Spec.Containers[i].MemoryLimitBytes = parseMemory(lim)
			}
		}

		out.Status.Phase = in.Status.Phase
		out.Status.PodIP = in.Status.PodIP
		out.Status.HostIP = in.Status.HostIP
		out.Status.StartTime = in.Status.StartTime

		return nil
	})

	// internal → v1
	s.AddConversionFunc(internalGVK, v1GVK, func(src, dst Object) error {
		in := src.(*InternalPod)
		out := dst.(*V1Pod)

		out.TypeMeta = TypeMeta{Kind: "Pod", APIVersion: "v1"}
		out.ObjectMeta = in.ObjectMeta
		out.Spec.NodeName = in.Spec.NodeName
		out.Spec.RestartPolicy = in.Spec.RestartPolicy
		if in.Spec.Priority != 0 {
			p := in.Spec.Priority
			out.Spec.Priority = &p
		}

		out.Spec.Containers = make([]V1Container, len(in.Spec.Containers))
		for i, c := range in.Spec.Containers {
			out.Spec.Containers[i] = V1Container{
				Name:    c.Name,
				Image:   c.Image,
				Command: c.Command,
				Resources: ResourceRequirements{
					Requests: make(map[string]string),
					Limits:   make(map[string]string),
				},
			}
			if c.CPURequestMillis > 0 {
				out.Spec.Containers[i].Resources.Requests["cpu"] = formatMilliCPU(c.CPURequestMillis)
			}
			if c.MemoryRequestBytes > 0 {
				out.Spec.Containers[i].Resources.Requests["memory"] = formatMemory(c.MemoryRequestBytes)
			}
			if c.CPULimitMillis > 0 {
				out.Spec.Containers[i].Resources.Limits["cpu"] = formatMilliCPU(c.CPULimitMillis)
			}
			if c.MemoryLimitBytes > 0 {
				out.Spec.Containers[i].Resources.Limits["memory"] = formatMemory(c.MemoryLimitBytes)
			}
		}

		out.Status.Phase = in.Status.Phase
		out.Status.PodIP = in.Status.PodIP
		out.Status.HostIP = in.Status.HostIP
		out.Status.StartTime = in.Status.StartTime

		return nil
	})
}

// ============================================================================
// 7. Defaulting 함수 — zz_generated.defaults.go 시뮬레이션
// ============================================================================

// 실제 Kubernetes에서는 defaulter-gen이 자동 생성한다.

func registerV1Alpha1Defaults(s *Scheme) {
	gvk := GroupVersionKind{Group: "", Version: "v1alpha1", Kind: "Pod"}
	s.AddDefaultingFunc(gvk, func(obj Object) {
		pod := obj.(*V1Alpha1Pod)
		// RestartPolicy 기본값
		if pod.Spec.RestartPolicy == "" {
			pod.Spec.RestartPolicy = "Always"
		}
		// Namespace 기본값
		if pod.Namespace == "" {
			pod.Namespace = "default"
		}
	})
}

func registerV1Defaults(s *Scheme) {
	gvk := GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}
	s.AddDefaultingFunc(gvk, func(obj Object) {
		pod := obj.(*V1Pod)
		if pod.Spec.RestartPolicy == "" {
			pod.Spec.RestartPolicy = "Always"
		}
		if pod.Namespace == "" {
			pod.Namespace = "default"
		}
		// v1: 기본 리소스 요청 설정
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Resources.Requests == nil {
				pod.Spec.Containers[i].Resources.Requests = map[string]string{
					"cpu":    "100m",
					"memory": "128Mi",
				}
			}
		}
	})
}

// ============================================================================
// 8. 타입별 클라이언트 패턴 — client-gen 시뮬레이션
// ============================================================================

// 실제 Kubernetes에서는 client-gen이 각 리소스에 대한 타입별 클라이언트를 생성한다.
// 예: clientset.CoreV1().Pods("default").Create(ctx, pod, opts)

// PodInterface는 Pod 리소스의 클라이언트 인터페이스 (생성되는 코드)
type PodInterface interface {
	Create(pod *V1Pod) (*V1Pod, error)
	Get(name string) (*V1Pod, error)
	Update(pod *V1Pod) (*V1Pod, error)
	Delete(name string) error
	List() ([]*V1Pod, error)
}

// pods는 PodInterface의 구현체
type pods struct {
	namespace string
	store     map[string]*V1Pod
	scheme    *Scheme
	mu        sync.RWMutex
	rvCounter int
}

func newPods(namespace string, scheme *Scheme) PodInterface {
	return &pods{
		namespace: namespace,
		store:     make(map[string]*V1Pod),
		scheme:    scheme,
	}
}

func (p *pods) Create(pod *V1Pod) (*V1Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := pod.Name
	if _, exists := p.store[key]; exists {
		return nil, fmt.Errorf("pod %q already exists", key)
	}

	// Defaulting 적용
	p.scheme.Default(pod)

	p.rvCounter++
	pod.ResourceVersion = fmt.Sprintf("%d", p.rvCounter)
	pod.Namespace = p.namespace
	p.store[key] = pod.DeepCopyObject().(*V1Pod)
	return pod, nil
}

func (p *pods) Get(name string) (*V1Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pod, ok := p.store[name]
	if !ok {
		return nil, fmt.Errorf("pod %q not found", name)
	}
	return pod.DeepCopyObject().(*V1Pod), nil
}

func (p *pods) Update(pod *V1Pod) (*V1Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing, ok := p.store[pod.Name]
	if !ok {
		return nil, fmt.Errorf("pod %q not found", pod.Name)
	}
	// 낙관적 동시성 검사
	if pod.ResourceVersion != existing.ResourceVersion {
		return nil, fmt.Errorf("conflict: resourceVersion mismatch (got %s, current %s)",
			pod.ResourceVersion, existing.ResourceVersion)
	}

	p.rvCounter++
	pod.ResourceVersion = fmt.Sprintf("%d", p.rvCounter)
	p.store[pod.Name] = pod.DeepCopyObject().(*V1Pod)
	return pod, nil
}

func (p *pods) Delete(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.store[name]; !ok {
		return fmt.Errorf("pod %q not found", name)
	}
	delete(p.store, name)
	return nil
}

func (p *pods) List() ([]*V1Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]*V1Pod, 0, len(p.store))
	for _, pod := range p.store {
		result = append(result, pod.DeepCopyObject().(*V1Pod))
	}
	return result, nil
}

// CoreV1Interface는 core/v1 API 그룹 클라이언트
type CoreV1Interface interface {
	Pods(namespace string) PodInterface
}

type coreV1Client struct {
	scheme *Scheme
}

func (c *coreV1Client) Pods(namespace string) PodInterface {
	return newPods(namespace, c.scheme)
}

// Clientset은 모든 API 그룹 클라이언트를 포함한다
type Clientset struct {
	coreV1 *coreV1Client
}

func NewClientset(scheme *Scheme) *Clientset {
	return &Clientset{
		coreV1: &coreV1Client{scheme: scheme},
	}
}

func (cs *Clientset) CoreV1() CoreV1Interface {
	return cs.coreV1
}

// ============================================================================
// 유틸리티 함수
// ============================================================================

func parseMilliCPU(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "m") {
		s = strings.TrimSuffix(s, "m")
		var v int64
		fmt.Sscanf(s, "%d", &v)
		return v
	}
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return int64(v * 1000)
}

func parseMemory(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "Gi") {
		s = strings.TrimSuffix(s, "Gi")
		var v int64
		fmt.Sscanf(s, "%d", &v)
		return v * 1024 * 1024 * 1024
	}
	if strings.HasSuffix(s, "Mi") {
		s = strings.TrimSuffix(s, "Mi")
		var v int64
		fmt.Sscanf(s, "%d", &v)
		return v * 1024 * 1024
	}
	var v int64
	fmt.Sscanf(s, "%d", &v)
	return v
}

func formatMilliCPU(millis int64) string {
	if millis%1000 == 0 {
		return fmt.Sprintf("%d", millis/1000)
	}
	return fmt.Sprintf("%dm", millis)
}

func formatMemory(bytes int64) string {
	if bytes%(1024*1024*1024) == 0 {
		return fmt.Sprintf("%dGi", bytes/(1024*1024*1024))
	}
	if bytes%(1024*1024) == 0 {
		return fmt.Sprintf("%dMi", bytes/(1024*1024))
	}
	return fmt.Sprintf("%d", bytes)
}

// ============================================================================
// main — 전체 시연
// ============================================================================

func main() {
	fmt.Println("=== Kubernetes 코드 생성 패턴 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────
	// 1. Scheme 초기화 및 타입 등록
	// ─────────────────────────────────────────────
	fmt.Println("--- 1. Scheme 초기화 및 타입 등록 ---")

	scheme := NewScheme()

	// 내부 타입 등록
	internalGVK := GroupVersionKind{Group: "", Version: "__internal", Kind: "Pod"}
	scheme.AddKnownType(internalGVK, &InternalPod{})

	// v1alpha1 타입 등록
	v1alpha1GVK := GroupVersionKind{Group: "", Version: "v1alpha1", Kind: "Pod"}
	scheme.AddKnownType(v1alpha1GVK, &V1Alpha1Pod{})

	// v1 타입 등록
	v1GVK := GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}
	scheme.AddKnownType(v1GVK, &V1Pod{})

	fmt.Printf("  등록된 GVK: %s → %s\n", internalGVK, reflect.TypeOf(InternalPod{}))
	fmt.Printf("  등록된 GVK: %s → %s\n", v1alpha1GVK, reflect.TypeOf(V1Alpha1Pod{}))
	fmt.Printf("  등록된 GVK: %s → %s\n", v1GVK, reflect.TypeOf(V1Pod{}))

	// 변환 함수 등록
	registerV1Alpha1Conversions(scheme)
	registerV1Conversions(scheme)

	// 기본값 함수 등록
	registerV1Alpha1Defaults(scheme)
	registerV1Defaults(scheme)

	fmt.Println("  변환 함수 등록 완료 (v1alpha1 ↔ internal ↔ v1)")
	fmt.Println("  기본값 함수 등록 완료")
	fmt.Println()

	// ─────────────────────────────────────────────
	// 2. DeepCopy 테스트
	// ─────────────────────────────────────────────
	fmt.Println("--- 2. DeepCopy 테스트 ---")

	original := &InternalPod{
		TypeMeta:   TypeMeta{Kind: "Pod", APIVersion: "__internal"},
		ObjectMeta: ObjectMeta{Name: "nginx", Namespace: "default", Labels: map[string]string{"app": "nginx"}},
		Spec: InternalPodSpec{
			Containers: []InternalContainer{
				{Name: "nginx", Image: "nginx:1.21", CPURequestMillis: 500, MemoryRequestBytes: 256 * 1024 * 1024},
			},
			RestartPolicy: "Always",
		},
	}

	copied := original.DeepCopy()

	// 원본 수정이 복사본에 영향 없음을 확인
	original.Labels["app"] = "modified"
	original.Spec.Containers[0].Image = "nginx:1.22"

	fmt.Printf("  원본 Labels: %v, Image: %s\n", original.Labels, original.Spec.Containers[0].Image)
	fmt.Printf("  복사본 Labels: %v, Image: %s\n", copied.Labels, copied.Spec.Containers[0].Image)
	fmt.Printf("  → 깊은 복사 검증: 원본 수정이 복사본에 영향 없음 ✓\n")
	fmt.Println()

	// ─────────────────────────────────────────────
	// 3. Defaulting 테스트
	// ─────────────────────────────────────────────
	fmt.Println("--- 3. Defaulting (기본값 설정) 테스트 ---")

	v1Pod := &V1Pod{
		TypeMeta:   TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: ObjectMeta{Name: "test-pod"},
		Spec: V1PodSpec{
			Containers: []V1Container{
				{Name: "app", Image: "myapp:latest"},
			},
			// RestartPolicy, Namespace 미설정
		},
	}

	fmt.Printf("  Defaulting 전: Namespace=%q, RestartPolicy=%q, Resources=%v\n",
		v1Pod.Namespace, v1Pod.Spec.RestartPolicy, v1Pod.Spec.Containers[0].Resources.Requests)

	scheme.Default(v1Pod)

	fmt.Printf("  Defaulting 후: Namespace=%q, RestartPolicy=%q, Resources=%v\n",
		v1Pod.Namespace, v1Pod.Spec.RestartPolicy, v1Pod.Spec.Containers[0].Resources.Requests)
	fmt.Println()

	// ─────────────────────────────────────────────
	// 4. 버전 변환 테스트: v1alpha1 → internal → v1
	// ─────────────────────────────────────────────
	fmt.Println("--- 4. 버전 변환 테스트: v1alpha1 → internal → v1 ---")

	v1alpha1Pod := &V1Alpha1Pod{
		TypeMeta:   TypeMeta{Kind: "Pod", APIVersion: "v1alpha1"},
		ObjectMeta: ObjectMeta{Name: "converter-test", Labels: map[string]string{"env": "dev"}},
		Spec: V1Alpha1PodSpec{
			Containers: []V1Alpha1Container{
				{
					Name:          "web",
					Image:         "nginx:1.21",
					Command:       []string{"nginx", "-g", "daemon off;"},
					CPURequest:    "250m",
					MemoryRequest: "512Mi",
				},
			},
		},
		Status: V1Alpha1PodStatus{Phase: "Running", PodIP: "10.0.1.5"},
	}

	// Defaulting 적용
	scheme.Default(v1alpha1Pod)
	fmt.Printf("  v1alpha1 Pod: Name=%s, RestartPolicy=%s, CPU=%s, Memory=%s\n",
		v1alpha1Pod.Name, v1alpha1Pod.Spec.RestartPolicy,
		v1alpha1Pod.Spec.Containers[0].CPURequest,
		v1alpha1Pod.Spec.Containers[0].MemoryRequest)

	// v1alpha1 → internal
	internalObj, err := scheme.Convert(v1alpha1Pod, internalGVK)
	if err != nil {
		fmt.Printf("  변환 에러: %v\n", err)
		return
	}
	internalPod := internalObj.(*InternalPod)
	fmt.Printf("  internal Pod: CPUMillis=%d, MemoryBytes=%d, RestartPolicy=%s\n",
		internalPod.Spec.Containers[0].CPURequestMillis,
		internalPod.Spec.Containers[0].MemoryRequestBytes,
		internalPod.Spec.RestartPolicy)

	// internal → v1
	v1Obj, err := scheme.Convert(internalPod, v1GVK)
	if err != nil {
		fmt.Printf("  변환 에러: %v\n", err)
		return
	}
	v1Result := v1Obj.(*V1Pod)
	fmt.Printf("  v1 Pod: CPU=%s, Memory=%s, RestartPolicy=%s, Labels=%v\n",
		v1Result.Spec.Containers[0].Resources.Requests["cpu"],
		v1Result.Spec.Containers[0].Resources.Requests["memory"],
		v1Result.Spec.RestartPolicy,
		v1Result.Labels)

	fmt.Println("  → v1alpha1 → internal → v1 변환 성공 ✓")
	fmt.Println()

	// ─────────────────────────────────────────────
	// 5. Scheme.NewObject 테스트
	// ─────────────────────────────────────────────
	fmt.Println("--- 5. Scheme.NewObject (GVK로 객체 생성) ---")

	obj, err := scheme.NewObject(v1GVK)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	fmt.Printf("  생성된 객체: Type=%T, Kind=%s, APIVersion=%s\n",
		obj, obj.GetObjectKind().Kind, obj.GetObjectKind().APIVersion)
	fmt.Println()

	// ─────────────────────────────────────────────
	// 6. 타입별 클라이언트 테스트
	// ─────────────────────────────────────────────
	fmt.Println("--- 6. 타입별 클라이언트 (client-gen 패턴) ---")

	clientset := NewClientset(scheme)
	podClient := clientset.CoreV1().Pods("default")

	// Create
	createPod := &V1Pod{
		TypeMeta:   TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: ObjectMeta{Name: "nginx-pod"},
		Spec: V1PodSpec{
			Containers: []V1Container{
				{Name: "nginx", Image: "nginx:1.25"},
			},
		},
	}
	created, err := podClient.Create(createPod)
	if err != nil {
		fmt.Printf("  Create 에러: %v\n", err)
		return
	}
	fmt.Printf("  Create: name=%s, rv=%s, ns=%s, restartPolicy=%s\n",
		created.Name, created.ResourceVersion, created.Namespace, created.Spec.RestartPolicy)
	fmt.Printf("  → Defaulting 자동 적용: RestartPolicy=%q, Resources=%v\n",
		created.Spec.RestartPolicy, created.Spec.Containers[0].Resources.Requests)

	// Get
	got, err := podClient.Get("nginx-pod")
	if err != nil {
		fmt.Printf("  Get 에러: %v\n", err)
		return
	}
	fmt.Printf("  Get: name=%s, rv=%s\n", got.Name, got.ResourceVersion)

	// Update
	got.Spec.Containers[0].Image = "nginx:1.26"
	updated, err := podClient.Update(got)
	if err != nil {
		fmt.Printf("  Update 에러: %v\n", err)
		return
	}
	fmt.Printf("  Update: name=%s, rv=%s→%s, image=%s\n",
		updated.Name, got.ResourceVersion, updated.ResourceVersion, updated.Spec.Containers[0].Image)

	// Conflict test
	got.Spec.Containers[0].Image = "nginx:1.27"
	_, err = podClient.Update(got) // 이전 rv로 업데이트 시도 → 충돌
	if err != nil {
		fmt.Printf("  Conflict: %v ✓\n", err)
	}

	// List
	list, err := podClient.List()
	if err != nil {
		fmt.Printf("  List 에러: %v\n", err)
		return
	}
	fmt.Printf("  List: %d개 Pod\n", len(list))

	// Delete
	err = podClient.Delete("nginx-pod")
	if err != nil {
		fmt.Printf("  Delete 에러: %v\n", err)
		return
	}
	fmt.Printf("  Delete: nginx-pod 삭제 완료\n")

	list, _ = podClient.List()
	fmt.Printf("  List after delete: %d개 Pod\n", len(list))
	fmt.Println()

	// ─────────────────────────────────────────────
	// 7. 전체 코드 생성 패턴 요약
	// ─────────────────────────────────────────────
	fmt.Println("=== 코드 생성 패턴 요약 ===")
	fmt.Println()
	fmt.Println("Kubernetes는 다음 코드를 자동 생성한다:")
	fmt.Println()
	fmt.Println("  도구               생성 파일                          역할")
	fmt.Println("  ────────────────── ──────────────────────────────── ────────────────────────")
	fmt.Println("  deepcopy-gen       zz_generated.deepcopy.go         DeepCopy/DeepCopyInto 메서드")
	fmt.Println("  defaulter-gen      zz_generated.defaults.go         기본값 설정 함수")
	fmt.Println("  conversion-gen     zz_generated.conversion.go       버전 간 변환 함수")
	fmt.Println("  client-gen         kubernetes/typed/core/v1/pod.go  타입별 REST 클라이언트")
	fmt.Println("  informer-gen       informers/core/v1/pod.go         SharedInformer 팩토리")
	fmt.Println("  lister-gen         listers/core/v1/pod.go           타입별 캐시 조회")
	fmt.Println("  register-gen       zz_generated.register.go         API 그룹 Scheme 등록")
	fmt.Println("  openapi-gen        zz_generated.openapi.go          OpenAPI 스펙 정의")
	fmt.Println()
	fmt.Println("  핵심 원리:")
	fmt.Println("  1. types.go에 타입 정의 (수동 작성)")
	fmt.Println("  2. doc.go에 코드 생성 태그 설정 (+k8s:deepcopy-gen=package-level)")
	fmt.Println("  3. hack/update-codegen.sh 실행 → zz_generated.*.go 파일 자동 생성")
	fmt.Println("  4. 생성된 코드는 편집하지 않음 (// Code generated ... DO NOT EDIT.)")
	fmt.Println()
	fmt.Println("  변환 허브 패턴:")
	fmt.Println("    v1alpha1 ──→ internal ──→ v1")
	fmt.Println("    v1alpha1 ←── internal ←── v1")
	fmt.Println("    → N개 버전이면 2N개 변환 함수 (N² 아님)")
}
