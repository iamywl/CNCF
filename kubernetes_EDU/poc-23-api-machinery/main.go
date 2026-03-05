package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// =============================================================================
// Kubernetes API Machinery 시뮬레이션
// Scheme, Conversion, Strategic Merge Patch, Server-Side Apply
// 참조:
//   - Scheme: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go
//   - SMP: staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch.go
//   - SSA: staging/src/k8s.io/apimachinery/pkg/util/managedfields/fieldmanager.go
// =============================================================================

// --- GroupVersionKind ---

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

// --- runtime.Object 인터페이스 ---
// 실제: staging/src/k8s.io/apimachinery/pkg/runtime/interfaces.go:337-340

type Object interface {
	GetObjectKind() *GroupVersionKind
	DeepCopy() Object
}

// --- Scheme (타입 레지스트리) ---
// 실제: staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go:50-95

type Scheme struct {
	gvkToType     map[GroupVersionKind]reflect.Type
	typeToGVK     map[reflect.Type][]GroupVersionKind
	converters    map[conversionKey]ConvertFunc
	defaulters    map[reflect.Type]DefaultFunc
}

type conversionKey struct {
	src, dst reflect.Type
}

type ConvertFunc func(src, dst interface{}) error
type DefaultFunc func(obj interface{})

func NewScheme() *Scheme {
	return &Scheme{
		gvkToType:  make(map[GroupVersionKind]reflect.Type),
		typeToGVK:  make(map[reflect.Type][]GroupVersionKind),
		converters: make(map[conversionKey]ConvertFunc),
		defaulters: make(map[reflect.Type]DefaultFunc),
	}
}

// AddKnownTypes: GroupVersion에 타입 등록
// 실제: scheme.go:151-161
func (s *Scheme) AddKnownTypes(gv GroupVersionKind, obj Object) {
	t := reflect.TypeOf(obj)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	s.gvkToType[gv] = t
	s.typeToGVK[t] = append(s.typeToGVK[t], gv)
	fmt.Printf("  [Scheme] 등록: %s → %s\n", gv, t.Name())
}

// ObjectKinds: runtime.Object → GVK 매핑
// 실제: scheme.go:254-281
func (s *Scheme) ObjectKinds(obj Object) []GroupVersionKind {
	t := reflect.TypeOf(obj)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return s.typeToGVK[t]
}

// AddConversionFunc: 변환 함수 등록
func (s *Scheme) AddConversionFunc(src, dst interface{}, fn ConvertFunc) {
	key := conversionKey{
		src: reflect.TypeOf(src),
		dst: reflect.TypeOf(dst),
	}
	s.converters[key] = fn
}

// AddDefaultFunc: 기본값 설정 함수 등록
func (s *Scheme) AddDefaultFunc(obj interface{}, fn DefaultFunc) {
	s.defaulters[reflect.TypeOf(obj)] = fn
}

// Default: 객체에 기본값 적용
// 실제: scheme.go:355-359
func (s *Scheme) Default(obj interface{}) {
	t := reflect.TypeOf(obj)
	if fn, ok := s.defaulters[t]; ok {
		fn(obj)
	}
}

// --- Hub-and-Spoke 변환 모델 ---
// internal type을 허브로 사용하여 N개 버전을 2N개 변환으로 처리

type PodInternal struct {
	Name       string
	Namespace  string
	Image      string
	Replicas   int
	RestartPolicy string
}

func (p *PodInternal) GetObjectKind() *GroupVersionKind { return nil }
func (p *PodInternal) DeepCopy() Object { cp := *p; return &cp }

type PodV1 struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Image      string `json:"image"`
	Replicas   int    `json:"replicas,omitempty"`
}

func (p *PodV1) GetObjectKind() *GroupVersionKind {
	return &GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}
}
func (p *PodV1) DeepCopy() Object { cp := *p; return &cp }

type PodV1Beta1 struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	PodName    string `json:"podName"` // v1의 Name이 v1beta1에서는 PodName
	Container  string `json:"container"`
}

func (p *PodV1Beta1) GetObjectKind() *GroupVersionKind {
	return &GroupVersionKind{Group: "", Version: "v1beta1", Kind: "Pod"}
}
func (p *PodV1Beta1) DeepCopy() Object { cp := *p; return &cp }

// --- Strategic Merge Patch ---
// 실제: staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch.go

func StrategicMergePatch(original, patch map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// original 복사
	for k, v := range original {
		result[k] = v
	}

	// patch 적용
	for k, v := range patch {
		if v == nil {
			// nil → 삭제
			delete(result, k)
			continue
		}

		origVal, exists := result[k]
		if !exists {
			result[k] = v
			continue
		}

		// 둘 다 map이면 재귀 병합
		origMap, origIsMap := origVal.(map[string]interface{})
		patchMap, patchIsMap := v.(map[string]interface{})
		if origIsMap && patchIsMap {
			result[k] = StrategicMergePatch(origMap, patchMap)
			continue
		}

		// 그 외: 덮어쓰기
		result[k] = v
	}

	return result
}

// --- Server-Side Apply (Managed Fields) ---
// 실제: staging/src/k8s.io/apimachinery/pkg/util/managedfields/fieldmanager.go

type ManagedFieldsEntry struct {
	Manager     string
	Operation   string // "Apply" or "Update"
	Fields      map[string]bool
	Subresource string
}

type FieldManager struct {
	managedFields []ManagedFieldsEntry
}

func NewFieldManager() *FieldManager {
	return &FieldManager{}
}

// Apply: 필드 소유권 관리
func (fm *FieldManager) Apply(obj map[string]interface{}, manager string, force bool) error {
	fields := extractFields(obj, "")

	// 충돌 확인
	for _, entry := range fm.managedFields {
		if entry.Manager == manager {
			continue
		}
		for field := range fields {
			if entry.Fields[field] {
				if !force {
					return fmt.Errorf("conflict: field '%s' is managed by '%s'", field, entry.Manager)
				}
				// force=true: 기존 관리자에서 필드 제거
				delete(entry.Fields, field)
				fmt.Printf("    [SSA] force: '%s' → '%s'가 '%s'에서 가져옴\n",
					field, manager, entry.Manager)
			}
		}
	}

	// 기존 엔트리 업데이트 또는 새로 추가
	found := false
	for i := range fm.managedFields {
		if fm.managedFields[i].Manager == manager {
			fm.managedFields[i].Fields = fields
			fm.managedFields[i].Operation = "Apply"
			found = true
			break
		}
	}
	if !found {
		fm.managedFields = append(fm.managedFields, ManagedFieldsEntry{
			Manager:   manager,
			Operation: "Apply",
			Fields:    fields,
		})
	}

	return nil
}

func extractFields(obj map[string]interface{}, prefix string) map[string]bool {
	fields := make(map[string]bool)
	for k, v := range obj {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		fields[path] = true

		if nested, ok := v.(map[string]interface{}); ok {
			for nk := range extractFields(nested, path) {
				fields[nk] = true
			}
		}
	}
	return fields
}

func (fm *FieldManager) PrintManagedFields() {
	for _, entry := range fm.managedFields {
		var fieldList []string
		for f := range entry.Fields {
			fieldList = append(fieldList, f)
		}
		fmt.Printf("    Manager=%-15s Op=%-6s Fields=%v\n",
			entry.Manager, entry.Operation, fieldList)
	}
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	fmt.Println("=== Kubernetes API Machinery 시뮬레이션 ===")
	fmt.Println()

	// 1. Scheme 타입 등록
	demo1_Scheme()

	// 2. Hub-and-Spoke 변환
	demo2_Conversion()

	// 3. Strategic Merge Patch
	demo3_SMP()

	// 4. Server-Side Apply
	demo4_SSA()

	// 5. Defaulting
	demo5_Defaulting()

	printSummary()
}

func demo1_Scheme() {
	fmt.Println("--- 1. Scheme 타입 등록 ---")

	s := NewScheme()
	s.AddKnownTypes(
		GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		&PodV1{})
	s.AddKnownTypes(
		GroupVersionKind{Group: "", Version: "v1beta1", Kind: "Pod"},
		&PodV1Beta1{})

	// 역방향 조회
	pod := &PodV1{}
	gvks := s.ObjectKinds(pod)
	fmt.Printf("  PodV1의 GVK: %v\n", gvks)
	fmt.Println()
}

func demo2_Conversion() {
	fmt.Println("--- 2. Hub-and-Spoke 변환 ---")
	fmt.Println("  v1beta1 ──→ internal ──→ v1")
	fmt.Println("  v1beta1 ←── internal ←── v1")
	fmt.Println("  → N개 버전이면 2N개 변환 함수 (N² 아님)")
	fmt.Println()

	// v1 → internal
	podV1 := &PodV1{APIVersion: "v1", Kind: "Pod", Name: "web", Image: "nginx:1.21"}
	internal := &PodInternal{}
	internal.Name = podV1.Name
	internal.Image = podV1.Image
	fmt.Printf("  v1 → internal: {Name:%s, Image:%s}\n", internal.Name, internal.Image)

	// internal → v1beta1
	podV1Beta1 := &PodV1Beta1{
		APIVersion: "v1beta1",
		Kind:       "Pod",
		PodName:    internal.Name,     // Name → PodName 필드명 변경
		Container:  internal.Image,     // Image → Container 필드명 변경
	}
	fmt.Printf("  internal → v1beta1: {PodName:%s, Container:%s}\n",
		podV1Beta1.PodName, podV1Beta1.Container)
	fmt.Println()
}

func demo3_SMP() {
	fmt.Println("--- 3. Strategic Merge Patch ---")

	original := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "web",
			"namespace": "default",
			"labels": map[string]interface{}{
				"app":     "web",
				"version": "v1",
			},
		},
		"spec": map[string]interface{}{
			"replicas": 3,
			"image":    "nginx:1.21",
		},
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				"version": "v2",           // 업데이트
				"env":     "production",   // 추가
			},
		},
		"spec": map[string]interface{}{
			"image": "nginx:1.22",         // 업데이트
		},
	}

	result := StrategicMergePatch(original, patch)

	fmt.Println("  Original:")
	printJSON(original, "    ")
	fmt.Println("  Patch:")
	printJSON(patch, "    ")
	fmt.Println("  Result:")
	printJSON(result, "    ")
	fmt.Println()
}

func demo4_SSA() {
	fmt.Println("--- 4. Server-Side Apply (Managed Fields) ---")

	fm := NewFieldManager()

	// kubectl apply로 배포
	fmt.Println("  Step 1: kubectl이 spec을 관리")
	obj1 := map[string]interface{}{
		"spec": map[string]interface{}{
			"replicas": 3,
			"image":    "nginx:1.21",
		},
	}
	err := fm.Apply(obj1, "kubectl", false)
	fmt.Printf("  err=%v\n", err)
	fm.PrintManagedFields()
	fmt.Println()

	// HPA가 replicas를 업데이트 시도 (충돌)
	fmt.Println("  Step 2: HPA가 replicas 충돌 (force=false)")
	obj2 := map[string]interface{}{
		"spec": map[string]interface{}{
			"replicas": 5,
		},
	}
	err = fm.Apply(obj2, "hpa-controller", false)
	fmt.Printf("  err=%v\n", err)
	fmt.Println()

	// HPA가 force=true로 재시도
	fmt.Println("  Step 3: HPA가 force=true로 소유권 강탈")
	err = fm.Apply(obj2, "hpa-controller", true)
	fmt.Printf("  err=%v\n", err)
	fm.PrintManagedFields()
	fmt.Println()
}

func demo5_Defaulting() {
	fmt.Println("--- 5. Defaulting (기본값 설정) ---")

	s := NewScheme()
	s.AddDefaultFunc(&PodV1{}, func(obj interface{}) {
		pod := obj.(*PodV1)
		if pod.Replicas == 0 {
			pod.Replicas = 1
			fmt.Printf("  [Default] replicas = 1 (기본값)\n")
		}
	})

	pod := &PodV1{Name: "web", Image: "nginx"}
	fmt.Printf("  Before: replicas=%d\n", pod.Replicas)
	s.Default(pod)
	fmt.Printf("  After:  replicas=%d\n", pod.Replicas)
	fmt.Println()
}

func printJSON(obj interface{}, indent string) {
	data, _ := json.MarshalIndent(obj, indent, "  ")
	fmt.Printf("%s%s\n", indent, string(data))
}

func printSummary() {
	fmt.Println("=== 핵심 정리 ===")
	_ = strings.Join
	items := []string{
		"1. Scheme은 GVK ↔ Go Type 양방향 매핑을 관리하는 타입 레지스트리다",
		"2. Hub-and-Spoke: internal type을 허브로 사용 → 2N개 변환 (N² 아님)",
		"3. Strategic Merge Patch: 중첩 map은 재귀 병합, 배열은 merge key 기반",
		"4. Server-Side Apply: FieldManager로 필드 소유권 추적, 충돌 감지/강제 해결",
		"5. Defaulting: Scheme에 등록된 함수로 미지정 필드에 기본값 적용",
		"6. API Aggregation: APIService로 외부 API 서버를 kube-apiserver에 통합",
	}
	for _, item := range items {
		fmt.Printf("  %s\n", item)
	}
	fmt.Println()
	fmt.Println("소스코드 참조:")
	fmt.Println("  - Scheme:        staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go")
	fmt.Println("  - SMP:           staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch.go")
	fmt.Println("  - FieldManager:  staging/src/k8s.io/apimachinery/pkg/util/managedfields/fieldmanager.go")
}
