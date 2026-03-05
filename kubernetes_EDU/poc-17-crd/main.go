package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes CustomResourceDefinition (CRD) 처리 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go
//     : CustomResourceDefinition, CustomResourceDefinitionSpec, CustomResourceDefinitionNames
//   - staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/customresource_handler.go
//     : crdHandler — CRD 등록 시 동적 REST 핸들러 생성
//   - staging/src/k8s.io/apiextensions-apiserver/pkg/registry/customresource/
//     : CustomResource REST 저장소 (CRUD)
//   - staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/schema/
//     : 스키마 검증 (OpenAPI v3 기반)
//
// CRD 동작 원리:
//   1. 사용자가 CRD 오브젝트를 생성 (API 그룹, 리소스명, 스키마 정의)
//   2. apiextensions-apiserver가 CRD를 감지하고 동적 REST 핸들러를 등록
//   3. 이후 해당 API 그룹/버전/리소스에 대한 CRUD가 가능해짐
//   4. Custom Resource 생성 시 스키마 검증 수행
//
// CRD 스펙 핵심 필드 (types.go:41-73):
//   - Group: API 그룹 (예: "stable.example.com")
//   - Names: Plural/Singular/Kind/ShortNames
//   - Scope: Namespaced / Cluster
//   - Versions: 버전 목록 + 각 버전의 OpenAPI 스키마

// =============================================================================
// 1. 데이터 모델 — apiextensions/v1/types.go 재현
// =============================================================================

// ResourceScope는 리소스의 범위
type ResourceScope string

const (
	ClusterScoped   ResourceScope = "Cluster"
	NamespaceScoped ResourceScope = "Namespaced"
)

// CustomResourceDefinitionNames는 CRD의 이름 정보
// 실제: types.go의 CustomResourceDefinitionNames
type CustomResourceDefinitionNames struct {
	Plural     string   // URL 경로에 사용 (예: "crontabs")
	Singular   string   // 단수형 (예: "crontab")
	Kind       string   // Go 타입 이름 (예: "CronTab")
	ShortNames []string // 축약형 (예: ["ct"])
	ListKind   string   // List 타입 이름 (예: "CronTabList")
}

// JSONSchemaProps는 OpenAPI v3 스키마의 간소화 버전
// 실제: staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go의 JSONSchemaProps
type JSONSchemaProps struct {
	Type        string                     // "object", "string", "integer", "boolean", "array"
	Properties  map[string]JSONSchemaProps  // 오브젝트 필드 정의
	Required    []string                   // 필수 필드
	Description string                     // 필드 설명
	Minimum     *float64                   // 최소값 (integer/number)
	Maximum     *float64                   // 최대값
	Enum        []string                   // 허용 값 목록
	Items       *JSONSchemaProps           // 배열 항목 스키마
}

// CustomResourceDefinitionVersion은 CRD의 버전 정의
// 실제: types.go의 CustomResourceDefinitionVersion
type CustomResourceDefinitionVersion struct {
	Name    string          // 버전 이름 (예: "v1alpha1", "v1")
	Served  bool            // API에서 제공 여부
	Storage bool            // etcd 저장 버전 (하나만 true)
	Schema  *JSONSchemaProps // 이 버전의 스키마
}

// CustomResourceDefinition은 CRD 오브젝트
// 실제: types.go:20-73의 CustomResourceDefinition
type CustomResourceDefinition struct {
	Name       string // <plural>.<group> 형식
	Group      string
	Names      CustomResourceDefinitionNames
	Scope      ResourceScope
	Versions   []CustomResourceDefinitionVersion
	CreatedAt  time.Time
	Conditions []CRDCondition
}

// CRDCondition은 CRD의 상태 조건
type CRDCondition struct {
	Type    string // Established, NamesAccepted, Terminating
	Status  string // True, False
	Message string
}

// CustomResource는 CRD로 정의된 커스텀 리소스 인스턴스
type CustomResource struct {
	APIVersion string                 // <group>/<version>
	Kind       string
	Name       string
	Namespace  string                 // NamespaceScoped일 때만 유효
	UID        string
	Spec       map[string]interface{} // 사용자 정의 스펙
	Status     map[string]interface{} // 상태 (서브리소스)
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// =============================================================================
// 2. 스키마 검증기 — apiserver/schema 패키지 재현
// =============================================================================

// SchemaValidator는 Custom Resource의 스키마 검증을 수행한다
// 실제: staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/schema/validation.go
type SchemaValidator struct{}

// Validate는 데이터가 스키마에 맞는지 검증한다
// 실제 검증 항목:
//   - 필수 필드 존재 여부 (Required)
//   - 타입 일치 (string, integer, boolean, object, array)
//   - 범위 제한 (Minimum, Maximum)
//   - Enum 제한 (허용 값 목록)
//   - 중첩 오브젝트 재귀 검증
func (v *SchemaValidator) Validate(data map[string]interface{}, schema *JSONSchemaProps, path string) []string {
	var errors []string

	if schema == nil {
		return errors
	}

	// 1. 필수 필드 검증
	for _, req := range schema.Required {
		if _, ok := data[req]; !ok {
			errors = append(errors, fmt.Sprintf("%s.%s: 필수 필드 누락", path, req))
		}
	}

	// 2. 각 필드의 타입 및 값 검증
	for fieldName, fieldSchema := range schema.Properties {
		val, exists := data[fieldName]
		if !exists {
			continue // 필수가 아닌 필드는 없어도 됨
		}

		fieldPath := fmt.Sprintf("%s.%s", path, fieldName)
		fieldErrors := v.validateField(val, &fieldSchema, fieldPath)
		errors = append(errors, fieldErrors...)
	}

	// 3. 스키마에 정의되지 않은 필드 검출
	for fieldName := range data {
		if _, ok := schema.Properties[fieldName]; !ok {
			errors = append(errors, fmt.Sprintf("%s.%s: 스키마에 정의되지 않은 필드", path, fieldName))
		}
	}

	return errors
}

// validateField는 단일 필드를 검증한다
func (v *SchemaValidator) validateField(val interface{}, schema *JSONSchemaProps, path string) []string {
	var errors []string

	switch schema.Type {
	case "string":
		str, ok := val.(string)
		if !ok {
			errors = append(errors, fmt.Sprintf("%s: string 타입 기대, 실제: %T", path, val))
			return errors
		}
		// Enum 검증
		if len(schema.Enum) > 0 {
			found := false
			for _, e := range schema.Enum {
				if str == e {
					found = true
					break
				}
			}
			if !found {
				errors = append(errors, fmt.Sprintf("%s: '%s'는 허용되지 않음, 허용: %v", path, str, schema.Enum))
			}
		}

	case "integer":
		var num float64
		switch n := val.(type) {
		case float64:
			num = n
		case int:
			num = float64(n)
		case json.Number:
			f, _ := n.Float64()
			num = f
		default:
			errors = append(errors, fmt.Sprintf("%s: integer 타입 기대, 실제: %T", path, val))
			return errors
		}
		if schema.Minimum != nil && num < *schema.Minimum {
			errors = append(errors, fmt.Sprintf("%s: 값 %.0f < 최소값 %.0f", path, num, *schema.Minimum))
		}
		if schema.Maximum != nil && num > *schema.Maximum {
			errors = append(errors, fmt.Sprintf("%s: 값 %.0f > 최대값 %.0f", path, num, *schema.Maximum))
		}

	case "boolean":
		if _, ok := val.(bool); !ok {
			errors = append(errors, fmt.Sprintf("%s: boolean 타입 기대, 실제: %T", path, val))
		}

	case "object":
		obj, ok := val.(map[string]interface{})
		if !ok {
			errors = append(errors, fmt.Sprintf("%s: object 타입 기대, 실제: %T", path, val))
			return errors
		}
		errors = append(errors, v.Validate(obj, schema, path)...)

	case "array":
		arr, ok := val.([]interface{})
		if !ok {
			errors = append(errors, fmt.Sprintf("%s: array 타입 기대, 실제: %T", path, val))
			return errors
		}
		if schema.Items != nil {
			for i, item := range arr {
				itemPath := fmt.Sprintf("%s[%d]", path, i)
				itemErrors := v.validateField(item, schema.Items, itemPath)
				errors = append(errors, itemErrors...)
			}
		}
	}

	return errors
}

// =============================================================================
// 3. Dynamic REST Handler — customresource_handler.go 재현
// =============================================================================

// CRDStore는 CRD별 커스텀 리소스 저장소
// 실제: staging/src/k8s.io/apiextensions-apiserver/pkg/registry/customresource/etcd.go
// etcd에 저장하지만, 여기서는 인메모리 맵으로 시뮬레이션
type CRDStore struct {
	mu        sync.RWMutex
	resources map[string]*CustomResource // namespace/name 또는 name (cluster-scoped)
	crd       *CustomResourceDefinition
	validator *SchemaValidator
}

func NewCRDStore(crd *CustomResourceDefinition) *CRDStore {
	return &CRDStore{
		resources: make(map[string]*CustomResource),
		crd:       crd,
		validator: &SchemaValidator{},
	}
}

// getStorageVersion은 storage=true인 버전을 반환한다
func (s *CRDStore) getStorageVersion() *CustomResourceDefinitionVersion {
	for i := range s.crd.Versions {
		if s.crd.Versions[i].Storage {
			return &s.crd.Versions[i]
		}
	}
	return nil
}

// getServedVersion은 특정 버전의 정의를 반환한다
func (s *CRDStore) getServedVersion(version string) *CustomResourceDefinitionVersion {
	for i := range s.crd.Versions {
		if s.crd.Versions[i].Name == version && s.crd.Versions[i].Served {
			return &s.crd.Versions[i]
		}
	}
	return nil
}

// resourceKey는 리소스의 저장소 키를 생성한다
func (s *CRDStore) resourceKey(namespace, name string) string {
	if s.crd.Scope == ClusterScoped {
		return name
	}
	return namespace + "/" + name
}

// Create는 커스텀 리소스를 생성한다
// 실제: customresource_handler.go의 ServeHTTP → POST 처리
func (s *CRDStore) Create(cr *CustomResource) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 버전 확인
	version := extractVersion(cr.APIVersion)
	verDef := s.getServedVersion(version)
	if verDef == nil {
		return fmt.Errorf("버전 '%s'는 제공되지 않음", version)
	}

	// 스키마 검증
	if verDef.Schema != nil {
		errors := s.validator.Validate(cr.Spec, verDef.Schema, "spec")
		if len(errors) > 0 {
			return fmt.Errorf("스키마 검증 실패:\n  %s", strings.Join(errors, "\n  "))
		}
	}

	// Kind 검증
	if cr.Kind != s.crd.Names.Kind {
		return fmt.Errorf("Kind 불일치: 기대=%s, 실제=%s", s.crd.Names.Kind, cr.Kind)
	}

	key := s.resourceKey(cr.Namespace, cr.Name)
	if _, exists := s.resources[key]; exists {
		return fmt.Errorf("리소스 '%s' 이미 존재", key)
	}

	cr.UID = fmt.Sprintf("uid-%s-%d", cr.Name, time.Now().UnixNano())
	cr.CreatedAt = time.Now()
	cr.UpdatedAt = cr.CreatedAt
	s.resources[key] = cr
	return nil
}

// Get는 커스텀 리소스를 조회한다
func (s *CRDStore) Get(namespace, name string) (*CustomResource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := s.resourceKey(namespace, name)
	cr, ok := s.resources[key]
	if !ok {
		return nil, fmt.Errorf("리소스 '%s' 없음", key)
	}
	return cr, nil
}

// Update는 커스텀 리소스를 갱신한다
func (s *CRDStore) Update(cr *CustomResource) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.resourceKey(cr.Namespace, cr.Name)
	existing, ok := s.resources[key]
	if !ok {
		return fmt.Errorf("리소스 '%s' 없음", key)
	}

	// 버전 확인 및 스키마 검증
	version := extractVersion(cr.APIVersion)
	verDef := s.getServedVersion(version)
	if verDef == nil {
		return fmt.Errorf("버전 '%s'는 제공되지 않음", version)
	}

	if verDef.Schema != nil {
		errors := s.validator.Validate(cr.Spec, verDef.Schema, "spec")
		if len(errors) > 0 {
			return fmt.Errorf("스키마 검증 실패:\n  %s", strings.Join(errors, "\n  "))
		}
	}

	cr.UID = existing.UID
	cr.CreatedAt = existing.CreatedAt
	cr.UpdatedAt = time.Now()
	s.resources[key] = cr
	return nil
}

// Delete는 커스텀 리소스를 삭제한다
func (s *CRDStore) Delete(namespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.resourceKey(namespace, name)
	if _, ok := s.resources[key]; !ok {
		return fmt.Errorf("리소스 '%s' 없음", key)
	}

	delete(s.resources, key)
	return nil
}

// List는 네임스페이스의 모든 커스텀 리소스를 반환한다
func (s *CRDStore) List(namespace string) []*CustomResource {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*CustomResource
	for _, cr := range s.resources {
		if s.crd.Scope == ClusterScoped || cr.Namespace == namespace {
			result = append(result, cr)
		}
	}
	return result
}

func extractVersion(apiVersion string) string {
	parts := strings.Split(apiVersion, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return apiVersion
}

// =============================================================================
// 4. CRD Registry — CRD 등록 및 핸들러 관리
// =============================================================================

// CRDRegistry는 CRD와 동적 핸들러를 관리한다
// 실제: staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/apiserver.go의
// CustomResourceDefinitionHandler가 CRD 변경을 감지하고 핸들러를 동적으로 등록/제거
type CRDRegistry struct {
	mu     sync.RWMutex
	crds   map[string]*CustomResourceDefinition // CRD name → CRD
	stores map[string]*CRDStore                  // CRD name → 저장소
}

func NewCRDRegistry() *CRDRegistry {
	return &CRDRegistry{
		crds:   make(map[string]*CustomResourceDefinition),
		stores: make(map[string]*CRDStore),
	}
}

// RegisterCRD는 CRD를 등록하고 동적 REST 핸들러를 생성한다
// 실제: crdHandler.ServeHTTP()에서 CRD 생성 이벤트 감지 시
// crdInfo를 생성하고 REST storage를 동적으로 생성
func (r *CRDRegistry) RegisterCRD(crd *CustomResourceDefinition) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// CRD 이름 형식 검증: <plural>.<group>
	expectedName := crd.Names.Plural + "." + crd.Group
	if crd.Name != expectedName {
		return fmt.Errorf("CRD 이름 형식 오류: 기대=%s, 실제=%s", expectedName, crd.Name)
	}

	// 저장 버전이 하나만 있는지 확인
	storageCount := 0
	for _, v := range crd.Versions {
		if v.Storage {
			storageCount++
		}
	}
	if storageCount != 1 {
		return fmt.Errorf("정확히 1개의 storage 버전이 필요, 현재=%d", storageCount)
	}

	crd.CreatedAt = time.Now()
	crd.Conditions = []CRDCondition{
		{Type: "NamesAccepted", Status: "True", Message: "이름 충돌 없음"},
		{Type: "Established", Status: "True", Message: "API 엔드포인트 등록됨"},
	}

	r.crds[crd.Name] = crd
	r.stores[crd.Name] = NewCRDStore(crd)
	return nil
}

// GetStore는 CRD의 저장소를 반환한다
func (r *CRDRegistry) GetStore(crdName string) (*CRDStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	store, ok := r.stores[crdName]
	if !ok {
		return nil, fmt.Errorf("CRD '%s' 등록되지 않음", crdName)
	}
	return store, nil
}

// GetCRD는 CRD를 반환한다
func (r *CRDRegistry) GetCRD(name string) *CustomResourceDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.crds[name]
}

// ListAPIEndpoints는 등록된 API 엔드포인트를 출력한다
func (r *CRDRegistry) ListAPIEndpoints() {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, crd := range r.crds {
		for _, ver := range crd.Versions {
			if ver.Served {
				if crd.Scope == NamespaceScoped {
					fmt.Printf("    /apis/%s/%s/namespaces/{ns}/%s\n",
						crd.Group, ver.Name, crd.Names.Plural)
				} else {
					fmt.Printf("    /apis/%s/%s/%s\n",
						crd.Group, ver.Name, crd.Names.Plural)
				}
			}
		}
	}
}

// =============================================================================
// 5. 데모 헬퍼
// =============================================================================

func printHeader(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printSubHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

func floatPtr(f float64) *float64 { return &f }

// =============================================================================
// 6. 메인 — 데모
// =============================================================================

func main() {
	registry := NewCRDRegistry()

	// =====================================================================
	// 데모 1: CRD 등록 — CronTab 커스텀 리소스
	// =====================================================================
	printHeader("데모 1: CRD 등록 — CronTab")

	cronTabCRD := &CustomResourceDefinition{
		Name:  "crontabs.stable.example.com",
		Group: "stable.example.com",
		Names: CustomResourceDefinitionNames{
			Plural:     "crontabs",
			Singular:   "crontab",
			Kind:       "CronTab",
			ShortNames: []string{"ct"},
			ListKind:   "CronTabList",
		},
		Scope: NamespaceScoped,
		Versions: []CustomResourceDefinitionVersion{
			{
				Name:    "v1alpha1",
				Served:  true,
				Storage: false,
				Schema: &JSONSchemaProps{
					Type: "object",
					Properties: map[string]JSONSchemaProps{
						"cronSpec": {Type: "string", Description: "Cron 표현식"},
						"image":    {Type: "string", Description: "컨테이너 이미지"},
						"replicas": {Type: "integer", Description: "레플리카 수", Minimum: floatPtr(1), Maximum: floatPtr(10)},
					},
					Required: []string{"cronSpec", "image"},
				},
			},
			{
				Name:    "v1beta1",
				Served:  true,
				Storage: false,
				Schema: &JSONSchemaProps{
					Type: "object",
					Properties: map[string]JSONSchemaProps{
						"cronSpec":       {Type: "string", Description: "Cron 표현식"},
						"image":          {Type: "string", Description: "컨테이너 이미지"},
						"replicas":       {Type: "integer", Description: "레플리카 수", Minimum: floatPtr(1), Maximum: floatPtr(100)},
						"suspendOnError": {Type: "boolean", Description: "에러 시 일시 중단"},
					},
					Required: []string{"cronSpec", "image"},
				},
			},
			{
				Name:    "v1",
				Served:  true,
				Storage: true, // 저장 버전
				Schema: &JSONSchemaProps{
					Type: "object",
					Properties: map[string]JSONSchemaProps{
						"cronSpec": {Type: "string", Description: "Cron 표현식"},
						"image":    {Type: "string", Description: "컨테이너 이미지"},
						"replicas": {Type: "integer", Description: "레플리카 수", Minimum: floatPtr(1), Maximum: floatPtr(1000)},
						"suspendOnError": {Type: "boolean", Description: "에러 시 일시 중단"},
						"timezone":       {Type: "string", Description: "타임존", Enum: []string{"UTC", "Asia/Seoul", "US/Eastern", "Europe/London"}},
					},
					Required: []string{"cronSpec", "image", "replicas"},
				},
			},
		},
	}

	err := registry.RegisterCRD(cronTabCRD)
	if err != nil {
		fmt.Printf("CRD 등록 실패: %v\n", err)
		return
	}

	fmt.Printf("CRD 등록 성공: %s\n", cronTabCRD.Name)
	fmt.Printf("  Group:      %s\n", cronTabCRD.Group)
	fmt.Printf("  Kind:       %s\n", cronTabCRD.Names.Kind)
	fmt.Printf("  Plural:     %s\n", cronTabCRD.Names.Plural)
	fmt.Printf("  ShortNames: %v\n", cronTabCRD.Names.ShortNames)
	fmt.Printf("  Scope:      %s\n", cronTabCRD.Scope)
	fmt.Printf("  Versions:   ")
	for _, v := range cronTabCRD.Versions {
		storage := ""
		if v.Storage {
			storage = " (storage)"
		}
		fmt.Printf("%s%s  ", v.Name, storage)
	}
	fmt.Println()

	printSubHeader("등록된 API 엔드포인트")
	registry.ListAPIEndpoints()

	fmt.Println("\n  Conditions:")
	for _, c := range cronTabCRD.Conditions {
		fmt.Printf("    %s: %s (%s)\n", c.Type, c.Status, c.Message)
	}

	// =====================================================================
	// 데모 2: CRUD 오퍼레이션
	// =====================================================================
	printHeader("데모 2: Custom Resource CRUD")

	store, _ := registry.GetStore("crontabs.stable.example.com")

	// Create
	printSubHeader("Create — CronTab 생성")
	cr1 := &CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "CronTab",
		Name:       "my-cron-job",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "*/5 * * * *",
			"image":    "my-app:v1.0",
			"replicas": float64(3),
			"timezone": "Asia/Seoul",
		},
	}

	err = store.Create(cr1)
	if err != nil {
		fmt.Printf("  생성 실패: %v\n", err)
	} else {
		fmt.Printf("  생성 성공: %s/%s (Kind: %s)\n", cr1.Namespace, cr1.Name, cr1.Kind)
		specJSON, _ := json.MarshalIndent(cr1.Spec, "    ", "  ")
		fmt.Printf("    Spec: %s\n", specJSON)
	}

	// Read
	printSubHeader("Read — CronTab 조회")
	got, err := store.Get("default", "my-cron-job")
	if err != nil {
		fmt.Printf("  조회 실패: %v\n", err)
	} else {
		fmt.Printf("  조회 성공: %s/%s\n", got.Namespace, got.Name)
		fmt.Printf("    APIVersion: %s\n", got.APIVersion)
		fmt.Printf("    Kind:       %s\n", got.Kind)
		fmt.Printf("    UID:        %s\n", got.UID)
	}

	// Update
	printSubHeader("Update — replicas 변경")
	updated := &CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "CronTab",
		Name:       "my-cron-job",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "*/5 * * * *",
			"image":    "my-app:v2.0",
			"replicas": float64(5),
			"timezone": "UTC",
		},
	}

	err = store.Update(updated)
	if err != nil {
		fmt.Printf("  갱신 실패: %v\n", err)
	} else {
		fmt.Printf("  갱신 성공: image=%s, replicas=%.0f, timezone=%s\n",
			updated.Spec["image"], updated.Spec["replicas"], updated.Spec["timezone"])
	}

	// List
	printSubHeader("List — 네임스페이스 리소스 목록")
	// 추가 리소스 생성
	store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "CronTab",
		Name:       "backup-job",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "0 2 * * *",
			"image":    "backup:latest",
			"replicas": float64(1),
		},
	})

	items := store.List("default")
	fmt.Printf("  default 네임스페이스: %d개\n", len(items))
	for _, item := range items {
		fmt.Printf("    - %s (image: %s, replicas: %.0f)\n",
			item.Name, item.Spec["image"], item.Spec["replicas"])
	}

	// Delete
	printSubHeader("Delete — CronTab 삭제")
	err = store.Delete("default", "backup-job")
	if err != nil {
		fmt.Printf("  삭제 실패: %v\n", err)
	} else {
		fmt.Printf("  삭제 성공: default/backup-job\n")
	}

	items = store.List("default")
	fmt.Printf("  남은 리소스: %d개\n", len(items))

	// =====================================================================
	// 데모 3: 스키마 검증
	// =====================================================================
	printHeader("데모 3: 스키마 검증")

	printSubHeader("검증 1: 필수 필드 누락 (replicas)")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "CronTab",
		Name:       "bad-no-replicas",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "* * * * *",
			"image":    "test:v1",
			// replicas 누락 (v1에서는 필수)
		},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	printSubHeader("검증 2: 타입 불일치 (replicas에 문자열)")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "CronTab",
		Name:       "bad-type",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "* * * * *",
			"image":    "test:v1",
			"replicas": "three", // integer여야 함
		},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	printSubHeader("검증 3: 범위 초과 (replicas > 1000)")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "CronTab",
		Name:       "bad-range",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "* * * * *",
			"image":    "test:v1",
			"replicas": float64(2000),
		},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	printSubHeader("검증 4: Enum 위반 (timezone에 잘못된 값)")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "CronTab",
		Name:       "bad-enum",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "* * * * *",
			"image":    "test:v1",
			"replicas": float64(1),
			"timezone": "Mars/Olympus",
		},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	printSubHeader("검증 5: 정의되지 않은 필드")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "CronTab",
		Name:       "bad-unknown-field",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec":  "* * * * *",
			"image":     "test:v1",
			"replicas":  float64(1),
			"badField":  "unknown",
		},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	printSubHeader("검증 6: Kind 불일치")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1",
		Kind:       "WrongKind",
		Name:       "bad-kind",
		Namespace:  "default",
		Spec:       map[string]interface{}{},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	// =====================================================================
	// 데모 4: 버전별 스키마 차이
	// =====================================================================
	printHeader("데모 4: API 버전별 스키마 차이")

	// v1alpha1: replicas는 선택사항, 범위 1-10
	printSubHeader("v1alpha1: replicas 선택사항, 범위 1-10")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1alpha1",
		Kind:       "CronTab",
		Name:       "alpha-job",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "*/10 * * * *",
			"image":    "alpha:v1",
			// replicas 없어도 됨 (v1alpha1에서는 선택)
		},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	} else {
		fmt.Println("  생성 성공 (replicas 없이도 가능)")
	}

	// v1alpha1에서 replicas > 10 → 실패
	printSubHeader("v1alpha1: replicas=15 → 범위 초과 (max=10)")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1alpha1",
		Kind:       "CronTab",
		Name:       "alpha-over",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec": "*/10 * * * *",
			"image":    "alpha:v1",
			"replicas": float64(15),
		},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	// v1beta1: replicas 범위 1-100
	printSubHeader("v1beta1: replicas=50 (max=100)")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v1beta1",
		Kind:       "CronTab",
		Name:       "beta-job",
		Namespace:  "default",
		Spec: map[string]interface{}{
			"cronSpec":       "0 * * * *",
			"image":          "beta:v1",
			"replicas":       float64(50),
			"suspendOnError": true,
		},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	} else {
		fmt.Println("  생성 성공 (replicas=50, v1beta1에서 범위 내)")
	}

	// 서빙되지 않는 버전 시도
	printSubHeader("v2 (서빙되지 않는 버전)")
	err = store.Create(&CustomResource{
		APIVersion: "stable.example.com/v2",
		Kind:       "CronTab",
		Name:       "v2-job",
		Namespace:  "default",
		Spec:       map[string]interface{}{},
	})
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	// =====================================================================
	// 데모 5: Cluster-Scoped CRD
	// =====================================================================
	printHeader("데모 5: Cluster-Scoped CRD")

	clusterCRD := &CustomResourceDefinition{
		Name:  "clusterbackuppolicies.backup.example.com",
		Group: "backup.example.com",
		Names: CustomResourceDefinitionNames{
			Plural:   "clusterbackuppolicies",
			Singular: "clusterbackuppolicy",
			Kind:     "ClusterBackupPolicy",
			ListKind: "ClusterBackupPolicyList",
		},
		Scope: ClusterScoped,
		Versions: []CustomResourceDefinitionVersion{
			{
				Name: "v1", Served: true, Storage: true,
				Schema: &JSONSchemaProps{
					Type: "object",
					Properties: map[string]JSONSchemaProps{
						"schedule":      {Type: "string"},
						"retentionDays": {Type: "integer", Minimum: floatPtr(1), Maximum: floatPtr(365)},
						"target":        {Type: "string", Enum: []string{"etcd", "volumes", "all"}},
					},
					Required: []string{"schedule", "retentionDays", "target"},
				},
			},
		},
	}

	err = registry.RegisterCRD(clusterCRD)
	if err != nil {
		fmt.Printf("등록 실패: %v\n", err)
		return
	}

	clusterStore, _ := registry.GetStore("clusterbackuppolicies.backup.example.com")

	fmt.Printf("CRD 등록: %s (Scope: %s)\n", clusterCRD.Name, clusterCRD.Scope)

	printSubHeader("Cluster-Scoped 리소스 생성 (namespace 없음)")
	err = clusterStore.Create(&CustomResource{
		APIVersion: "backup.example.com/v1",
		Kind:       "ClusterBackupPolicy",
		Name:       "daily-backup",
		Spec: map[string]interface{}{
			"schedule":      "0 3 * * *",
			"retentionDays": float64(30),
			"target":        "all",
		},
	})
	if err != nil {
		fmt.Printf("  생성 실패: %v\n", err)
	} else {
		fmt.Println("  생성 성공: daily-backup (cluster-wide)")
	}

	printSubHeader("등록된 전체 API 엔드포인트")
	registry.ListAPIEndpoints()

	// =====================================================================
	// 요약
	// =====================================================================
	printHeader("요약: CRD 처리 핵심 동작")
	fmt.Println(`
  CRD 등록 → 동적 API 엔드포인트 생성:
    1. CRD 오브젝트 생성 (Group, Names, Scope, Versions 정의)
    2. apiextensions-apiserver가 감지 → REST 핸들러 동적 등록
    3. /apis/<group>/<version>/[namespaces/<ns>/]<plural> 경로에서 CRUD 가능
    4. 각 요청마다 버전별 OpenAPI 스키마로 검증

  스키마 검증 항목:
    - Required: 필수 필드 존재
    - Type: string/integer/boolean/object/array 타입 일치
    - Minimum/Maximum: 숫자 범위
    - Enum: 허용 값 목록
    - 미정의 필드 거부

  버전 관리:
    - Served: API에서 제공 여부 (여러 버전 동시 제공 가능)
    - Storage: etcd 저장 버전 (정확히 하나)
    - 버전 간 변환: Webhook 또는 None 전략

  실제 소스 경로:
  - CRD 타입:       staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go
  - 동적 핸들러:     staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/customresource_handler.go
  - REST 저장소:     staging/src/k8s.io/apiextensions-apiserver/pkg/registry/customresource/
  - 스키마 검증:     staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/schema/`)
}
