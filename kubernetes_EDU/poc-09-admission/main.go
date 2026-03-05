package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
)

// =============================================================================
// Kubernetes Admission Controller 체인 시뮬레이션
//
// 실제 구현 참조:
//   - staging/src/k8s.io/apiserver/pkg/admission/interfaces.go (Interface, MutationInterface, ValidationInterface)
//   - staging/src/k8s.io/apiserver/pkg/admission/chain.go (chainAdmissionHandler)
//   - plugin/pkg/admission/ (각 빌트인 플러그인 구현)
//
// 핵심 개념:
//   1. Admission 인터페이스: Handles(), Admit(), Validate()
//   2. 체인 실행 순서: Mutating 먼저 → Validating 나중에
//   3. 빌트인 플러그인: NamespaceLifecycle, LimitRanger, ServiceAccount 기본값 주입
//   4. Webhook 시뮬레이션: 외부 HTTP 서버에 검증 요청
//   5. Mutation이 먼저 실행되어 객체를 수정한 후, Validation이 수정된 결과를 검증
// =============================================================================

// --- Operation 타입 ---

// Operation은 API 요청의 종류를 나타낸다.
// 실제 admission.Operation에 대응한다.
type Operation string

const (
	Create  Operation = "CREATE"
	Update  Operation = "UPDATE"
	Delete  Operation = "DELETE"
	Connect Operation = "CONNECT"
)

// --- 리소스 객체 ---

// Resource는 Kubernetes API 요청에 포함된 리소스 객체를 나타낸다.
// 실제로는 runtime.Object이지만, 여기서는 map으로 단순화한다.
type Resource struct {
	Kind       string
	Name       string
	Namespace  string
	Labels     map[string]string
	Spec       map[string]interface{}
	Metadata   map[string]string
}

// DeepCopy는 리소스의 깊은 복사본을 반환한다.
func (r *Resource) DeepCopy() *Resource {
	cp := &Resource{
		Kind:      r.Kind,
		Name:      r.Name,
		Namespace: r.Namespace,
		Labels:    make(map[string]string),
		Spec:      make(map[string]interface{}),
		Metadata:  make(map[string]string),
	}
	for k, v := range r.Labels {
		cp.Labels[k] = v
	}
	for k, v := range r.Spec {
		cp.Spec[k] = v
	}
	for k, v := range r.Metadata {
		cp.Metadata[k] = v
	}
	return cp
}

// --- Admission Attributes ---

// AdmissionAttributes는 Admission 요청의 컨텍스트 정보이다.
// 실제 admission.Attributes 인터페이스에 대응한다.
type AdmissionAttributes struct {
	Operation Operation
	Resource  *Resource
	OldObject *Resource // UPDATE 시 이전 객체
	UserInfo  string
}

// --- Admission 인터페이스 ---

// AdmissionPlugin은 Admission Controller 플러그인의 기본 인터페이스이다.
// 실제 admission.Interface에 대응한다.
type AdmissionPlugin interface {
	// Handles는 이 플러그인이 해당 Operation을 처리하는지 반환한다.
	Handles(op Operation) bool
	// Name은 플러그인 이름을 반환한다.
	Name() string
}

// MutatingPlugin은 리소스를 변경(mutate)할 수 있는 플러그인이다.
// 실제 admission.MutationInterface에 대응한다.
type MutatingPlugin interface {
	AdmissionPlugin
	// Admit는 리소스를 검사하고, 필요시 변경한다.
	Admit(attrs *AdmissionAttributes) error
}

// ValidatingPlugin은 리소스를 검증(validate)하는 플러그인이다.
// 실제 admission.ValidationInterface에 대응한다.
type ValidatingPlugin interface {
	AdmissionPlugin
	// Validate는 리소스를 검증한다. 변경은 허용되지 않는다.
	Validate(attrs *AdmissionAttributes) error
}

// --- Admission Chain ---

// AdmissionChain은 여러 Admission 플러그인을 순차적으로 실행한다.
// 실제 admission/chain.go의 chainAdmissionHandler에 대응한다.
//
// 실행 순서 (실제 Kubernetes와 동일):
//   1. Mutating 플러그인을 순서대로 실행 (Admit)
//      - 각 플러그인이 리소스를 변경할 수 있다
//      - 에러 발생 시 즉시 중단
//   2. Validating 플러그인을 순서대로 실행 (Validate)
//      - Mutation 이후의 최종 객체를 검증한다
//      - 에러 발생 시 즉시 중단
type AdmissionChain struct {
	plugins []AdmissionPlugin
}

func NewAdmissionChain(plugins ...AdmissionPlugin) *AdmissionChain {
	return &AdmissionChain{plugins: plugins}
}

// RunAdmission은 전체 Admission 체인을 실행한다.
func (c *AdmissionChain) RunAdmission(attrs *AdmissionAttributes) error {
	fmt.Printf("  [체인] Admission 시작: %s %s/%s (사용자: %s)\n",
		attrs.Operation, attrs.Resource.Namespace, attrs.Resource.Name, attrs.UserInfo)

	// Phase 1: Mutating (실제 chain.go의 Admit 메서드에 대응)
	fmt.Println("  [Phase 1] Mutating Admission 실행")
	for _, plugin := range c.plugins {
		if !plugin.Handles(attrs.Operation) {
			continue
		}
		if mutator, ok := plugin.(MutatingPlugin); ok {
			fmt.Printf("    → %s (mutating)\n", plugin.Name())
			if err := mutator.Admit(attrs); err != nil {
				fmt.Printf("    ✗ %s: 거부 - %v\n", plugin.Name(), err)
				return fmt.Errorf("admission denied by %s: %v", plugin.Name(), err)
			}
		}
	}

	// Phase 2: Validating (실제 chain.go의 Validate 메서드에 대응)
	fmt.Println("  [Phase 2] Validating Admission 실행")
	for _, plugin := range c.plugins {
		if !plugin.Handles(attrs.Operation) {
			continue
		}
		if validator, ok := plugin.(ValidatingPlugin); ok {
			fmt.Printf("    → %s (validating)\n", plugin.Name())
			if err := validator.Validate(attrs); err != nil {
				fmt.Printf("    ✗ %s: 거부 - %v\n", plugin.Name(), err)
				return fmt.Errorf("admission denied by %s: %v", plugin.Name(), err)
			}
		}
	}

	fmt.Println("  [체인] Admission 완료: 허가됨")
	return nil
}

// =============================================================================
// 빌트인 Admission 플러그인 구현
// =============================================================================

// --- NamespaceLifecycle ---

// NamespaceLifecycle은 삭제 중인 네임스페이스에 새 리소스 생성을 차단한다.
// 실제 plugin/pkg/admission/namespace/lifecycle에 대응한다.
type NamespaceLifecycle struct {
	terminatingNamespaces map[string]bool
}

func NewNamespaceLifecycle() *NamespaceLifecycle {
	return &NamespaceLifecycle{
		terminatingNamespaces: map[string]bool{
			"kube-system-old": true, // 테스트용: 삭제 중인 네임스페이스
		},
	}
}

func (p *NamespaceLifecycle) Name() string { return "NamespaceLifecycle" }

func (p *NamespaceLifecycle) Handles(op Operation) bool {
	return op == Create
}

func (p *NamespaceLifecycle) Validate(attrs *AdmissionAttributes) error {
	if p.terminatingNamespaces[attrs.Resource.Namespace] {
		return fmt.Errorf("네임스페이스 %q가 삭제 중 (Terminating)이므로 새 리소스를 생성할 수 없습니다",
			attrs.Resource.Namespace)
	}
	return nil
}

// --- ServiceAccountDefault ---

// ServiceAccountDefault는 Pod에 ServiceAccount가 지정되지 않은 경우 "default"를 주입한다.
// 실제 plugin/pkg/admission/serviceaccount에 대응한다.
// 이것은 MutatingPlugin이다 (리소스를 변경한다).
type ServiceAccountDefault struct{}

func (p *ServiceAccountDefault) Name() string { return "ServiceAccount" }

func (p *ServiceAccountDefault) Handles(op Operation) bool {
	return op == Create
}

func (p *ServiceAccountDefault) Admit(attrs *AdmissionAttributes) error {
	if attrs.Resource.Kind != "Pod" {
		return nil
	}

	// ServiceAccount가 지정되지 않은 경우 "default"를 주입
	if _, ok := attrs.Resource.Spec["serviceAccountName"]; !ok {
		attrs.Resource.Spec["serviceAccountName"] = "default"
		fmt.Printf("      [mutation] ServiceAccount 'default' 주입됨\n")
	}

	// automountServiceAccountToken이 없으면 true로 설정
	if _, ok := attrs.Resource.Spec["automountServiceAccountToken"]; !ok {
		attrs.Resource.Spec["automountServiceAccountToken"] = true
		fmt.Printf("      [mutation] automountServiceAccountToken=true 설정됨\n")
	}

	return nil
}

// --- LimitRanger ---

// LimitRanger는 리소스 제한이 없는 컨테이너에 기본값을 주입하고,
// 최소/최대 제한을 검증한다.
// 실제 plugin/pkg/admission/limitranger에 대응한다.
// MutatingPlugin + ValidatingPlugin 모두 구현한다.
type LimitRanger struct {
	defaultCPU    string
	defaultMemory string
	maxCPU        string
	maxMemory     string
}

func NewLimitRanger() *LimitRanger {
	return &LimitRanger{
		defaultCPU:    "100m",
		defaultMemory: "128Mi",
		maxCPU:        "4000m",
		maxMemory:     "8Gi",
	}
}

func (p *LimitRanger) Name() string { return "LimitRanger" }

func (p *LimitRanger) Handles(op Operation) bool {
	return op == Create || op == Update
}

// Admit은 리소스 제한이 없으면 기본값을 주입한다 (Mutation).
func (p *LimitRanger) Admit(attrs *AdmissionAttributes) error {
	if attrs.Resource.Kind != "Pod" {
		return nil
	}

	if _, ok := attrs.Resource.Spec["cpuRequest"]; !ok {
		attrs.Resource.Spec["cpuRequest"] = p.defaultCPU
		fmt.Printf("      [mutation] 기본 CPU 요청 '%s' 주입됨\n", p.defaultCPU)
	}

	if _, ok := attrs.Resource.Spec["memoryRequest"]; !ok {
		attrs.Resource.Spec["memoryRequest"] = p.defaultMemory
		fmt.Printf("      [mutation] 기본 메모리 요청 '%s' 주입됨\n", p.defaultMemory)
	}

	return nil
}

// Validate는 리소스 제한이 최대값을 초과하지 않는지 검증한다 (Validation).
func (p *LimitRanger) Validate(attrs *AdmissionAttributes) error {
	if attrs.Resource.Kind != "Pod" {
		return nil
	}

	if cpu, ok := attrs.Resource.Spec["cpuLimit"]; ok {
		if cpuStr, ok := cpu.(string); ok && cpuStr == "exceeded" {
			return fmt.Errorf("CPU 제한 %s가 최대 %s를 초과합니다", cpuStr, p.maxCPU)
		}
	}

	return nil
}

// --- PodSecurity ---

// PodSecurity는 Pod 보안 정책을 검증한다.
// Pod가 privileged 모드인지, hostNetwork를 사용하는지 등을 확인한다.
// ValidatingPlugin만 구현한다.
type PodSecurity struct{}

func (p *PodSecurity) Name() string { return "PodSecurity" }

func (p *PodSecurity) Handles(op Operation) bool {
	return op == Create || op == Update
}

func (p *PodSecurity) Validate(attrs *AdmissionAttributes) error {
	if attrs.Resource.Kind != "Pod" {
		return nil
	}

	if privileged, ok := attrs.Resource.Spec["privileged"]; ok {
		if priv, ok := privileged.(bool); ok && priv {
			// 네임스페이스의 보안 수준에 따라 거부할 수 있다
			if attrs.Resource.Namespace != "kube-system" {
				return fmt.Errorf("privileged 컨테이너는 %q 네임스페이스에서 허용되지 않습니다 (baseline 정책 위반)",
					attrs.Resource.Namespace)
			}
		}
	}

	if hostNet, ok := attrs.Resource.Spec["hostNetwork"]; ok {
		if hn, ok := hostNet.(bool); ok && hn {
			if attrs.Resource.Namespace == "default" {
				return fmt.Errorf("hostNetwork는 'default' 네임스페이스에서 허용되지 않습니다")
			}
		}
	}

	return nil
}

// --- DefaultTolerationSeconds ---

// DefaultTolerationSeconds는 Pod에 기본 Toleration을 주입한다.
// MutatingPlugin만 구현한다.
type DefaultTolerationSeconds struct {
	notReadySeconds   int
	unreachableSeconds int
}

func NewDefaultTolerationSeconds() *DefaultTolerationSeconds {
	return &DefaultTolerationSeconds{
		notReadySeconds:   300,
		unreachableSeconds: 300,
	}
}

func (p *DefaultTolerationSeconds) Name() string { return "DefaultTolerationSeconds" }

func (p *DefaultTolerationSeconds) Handles(op Operation) bool {
	return op == Create
}

func (p *DefaultTolerationSeconds) Admit(attrs *AdmissionAttributes) error {
	if attrs.Resource.Kind != "Pod" {
		return nil
	}

	if _, ok := attrs.Resource.Spec["tolerations"]; !ok {
		attrs.Resource.Spec["tolerations"] = fmt.Sprintf(
			"[{not-ready: %ds}, {unreachable: %ds}]",
			p.notReadySeconds, p.unreachableSeconds)
		fmt.Printf("      [mutation] 기본 Toleration 주입됨 (notReady=%ds, unreachable=%ds)\n",
			p.notReadySeconds, p.unreachableSeconds)
	}

	return nil
}

// --- Webhook (외부 검증자 시뮬레이션) ---

// WebhookValidator는 외부 HTTP 서버에 검증 요청을 보내는 Admission Webhook이다.
// 실제 MutatingWebhookConfiguration / ValidatingWebhookConfiguration에 대응한다.
type WebhookValidator struct {
	name    string
	url     string
}

func NewWebhookValidator(name, url string) *WebhookValidator {
	return &WebhookValidator{name: name, url: url}
}

func (p *WebhookValidator) Name() string { return fmt.Sprintf("Webhook[%s]", p.name) }

func (p *WebhookValidator) Handles(op Operation) bool {
	return op == Create || op == Update
}

func (p *WebhookValidator) Validate(attrs *AdmissionAttributes) error {
	// 실제 AdmissionReview JSON을 생성하여 Webhook 서버에 전송
	reqBody := map[string]interface{}{
		"kind": "AdmissionReview",
		"request": map[string]interface{}{
			"operation": attrs.Operation,
			"object": map[string]interface{}{
				"kind":      attrs.Resource.Kind,
				"name":      attrs.Resource.Name,
				"namespace": attrs.Resource.Namespace,
				"labels":    attrs.Resource.Labels,
			},
		},
	}

	jsonBody, _ := json.Marshal(reqBody)
	resp, err := http.Post(p.url, "application/json", strings.NewReader(string(jsonBody)))
	if err != nil {
		return fmt.Errorf("webhook 호출 실패: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Response struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
		} `json:"response"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("webhook 응답 파싱 실패: %v", err)
	}

	if !result.Response.Allowed {
		return fmt.Errorf("webhook 거부: %s", result.Response.Reason)
	}

	fmt.Printf("      [webhook] %s: 허가됨\n", p.name)
	return nil
}

// --- 데모 실행 ---

func main() {
	fmt.Println("=== Kubernetes Admission Controller 체인 시뮬레이션 ===")
	fmt.Println()

	// -----------------------------------------------
	// Webhook 테스트 서버 설정
	// -----------------------------------------------
	webhookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Request struct {
				Object struct {
					Labels map[string]string `json:"labels"`
					Name   string            `json:"name"`
				} `json:"object"`
			} `json:"request"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		allowed := true
		reason := ""

		// 정책: "env" 레이블이 필수
		if _, ok := req.Request.Object.Labels["env"]; !ok {
			allowed = false
			reason = "'env' 레이블이 필수입니다 (company policy)"
		}

		resp := map[string]interface{}{
			"response": map[string]interface{}{
				"allowed": allowed,
				"reason":  reason,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer webhookServer.Close()

	// -----------------------------------------------
	// Admission 체인 구성
	// -----------------------------------------------
	chain := NewAdmissionChain(
		// Mutating 플러그인 (순서 중요 - 실제 Kubernetes 순서 반영)
		&ServiceAccountDefault{},           // Pod에 기본 SA 주입
		NewLimitRanger(),                   // 리소스 제한 기본값 주입 + 검증
		NewDefaultTolerationSeconds(),      // 기본 Toleration 주입
		// Validating 플러그인
		NewNamespaceLifecycle(),            // 삭제 중 네임스페이스 차단
		&PodSecurity{},                     // Pod 보안 검증
		NewWebhookValidator("label-policy", webhookServer.URL), // 외부 정책 검증
	)

	fmt.Println("체인 구성:")
	fmt.Println("  [Mutating]   ServiceAccount → LimitRanger → DefaultTolerationSeconds")
	fmt.Println("  [Validating] NamespaceLifecycle → LimitRanger → PodSecurity → Webhook[label-policy]")
	fmt.Println()

	// -----------------------------------------------
	// 테스트 1: 정상적인 Pod 생성 (모든 Admission 통과)
	// -----------------------------------------------
	fmt.Println("========================================")
	fmt.Println("테스트 1: 정상적인 Pod 생성")
	fmt.Println("========================================")

	pod1 := &Resource{
		Kind:      "Pod",
		Name:      "nginx",
		Namespace: "default",
		Labels:    map[string]string{"app": "nginx", "env": "production"},
		Spec:      map[string]interface{}{"image": "nginx:1.21"},
		Metadata:  map[string]string{},
	}

	err := chain.RunAdmission(&AdmissionAttributes{
		Operation: Create,
		Resource:  pod1,
		UserInfo:  "alice",
	})
	if err != nil {
		fmt.Printf("  결과: 거부 - %v\n", err)
	} else {
		fmt.Println("  결과: 허가됨")
		fmt.Printf("  변경된 Spec: %v\n", pod1.Spec)
	}
	fmt.Println()

	// -----------------------------------------------
	// 테스트 2: Mutation 효과 확인 (SA, 리소스 기본값 주입)
	// -----------------------------------------------
	fmt.Println("========================================")
	fmt.Println("테스트 2: Mutation 효과 확인")
	fmt.Println("========================================")

	pod2 := &Resource{
		Kind:      "Pod",
		Name:      "redis",
		Namespace: "default",
		Labels:    map[string]string{"app": "redis", "env": "staging"},
		Spec:      map[string]interface{}{"image": "redis:7"},
		Metadata:  map[string]string{},
	}

	fmt.Printf("  Mutation 전 Spec: %v\n", pod2.Spec)
	err = chain.RunAdmission(&AdmissionAttributes{
		Operation: Create,
		Resource:  pod2,
		UserInfo:  "bob",
	})
	if err == nil {
		fmt.Printf("  Mutation 후 Spec: %v\n", pod2.Spec)
		fmt.Println("  → ServiceAccount, CPU/메모리 요청, Toleration이 자동 주입됨")
	}
	fmt.Println()

	// -----------------------------------------------
	// 테스트 3: NamespaceLifecycle 차단 (삭제 중인 네임스페이스)
	// -----------------------------------------------
	fmt.Println("========================================")
	fmt.Println("테스트 3: 삭제 중인 네임스페이스에 생성 시도")
	fmt.Println("========================================")

	pod3 := &Resource{
		Kind:      "Pod",
		Name:      "test",
		Namespace: "kube-system-old",
		Labels:    map[string]string{"env": "test"},
		Spec:      map[string]interface{}{},
		Metadata:  map[string]string{},
	}

	err = chain.RunAdmission(&AdmissionAttributes{
		Operation: Create,
		Resource:  pod3,
		UserInfo:  "admin",
	})
	if err != nil {
		fmt.Printf("  결과: 거부됨 (정상) - %v\n", err)
	}
	fmt.Println()

	// -----------------------------------------------
	// 테스트 4: PodSecurity 위반 (privileged 컨테이너)
	// -----------------------------------------------
	fmt.Println("========================================")
	fmt.Println("테스트 4: privileged 컨테이너 거부")
	fmt.Println("========================================")

	pod4 := &Resource{
		Kind:      "Pod",
		Name:      "hack-pod",
		Namespace: "default",
		Labels:    map[string]string{"env": "test"},
		Spec:      map[string]interface{}{"privileged": true},
		Metadata:  map[string]string{},
	}

	err = chain.RunAdmission(&AdmissionAttributes{
		Operation: Create,
		Resource:  pod4,
		UserInfo:  "hacker",
	})
	if err != nil {
		fmt.Printf("  결과: 거부됨 (정상) - %v\n", err)
	}
	fmt.Println()

	// -----------------------------------------------
	// 테스트 5: Webhook 거부 (env 레이블 없음)
	// -----------------------------------------------
	fmt.Println("========================================")
	fmt.Println("테스트 5: Webhook에 의한 거부 (env 레이블 없음)")
	fmt.Println("========================================")

	pod5 := &Resource{
		Kind:      "Pod",
		Name:      "no-label-pod",
		Namespace: "default",
		Labels:    map[string]string{"app": "test"}, // env 레이블 없음!
		Spec:      map[string]interface{}{},
		Metadata:  map[string]string{},
	}

	err = chain.RunAdmission(&AdmissionAttributes{
		Operation: Create,
		Resource:  pod5,
		UserInfo:  "dev",
	})
	if err != nil {
		fmt.Printf("  결과: 거부됨 (정상) - %v\n", err)
	}
	fmt.Println()

	// -----------------------------------------------
	// 테스트 6: DELETE 연산 (대부분의 플러그인이 무시)
	// -----------------------------------------------
	fmt.Println("========================================")
	fmt.Println("테스트 6: DELETE 연산 (Handles 필터링)")
	fmt.Println("========================================")

	pod6 := &Resource{
		Kind:      "Pod",
		Name:      "to-delete",
		Namespace: "default",
		Labels:    map[string]string{},
		Spec:      map[string]interface{}{},
		Metadata:  map[string]string{},
	}

	err = chain.RunAdmission(&AdmissionAttributes{
		Operation: Delete,
		Resource:  pod6,
		UserInfo:  "admin",
	})
	if err != nil {
		fmt.Printf("  결과: 거부 - %v\n", err)
	} else {
		fmt.Println("  결과: 허가됨 (DELETE에는 대부분의 플러그인이 Handles=false)")
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 요약:")
	fmt.Println("  1. Admission은 Mutating → Validating 순서로 실행된다")
	fmt.Println("  2. Mutating 플러그인은 리소스를 변경할 수 있다 (기본값 주입)")
	fmt.Println("  3. Validating 플러그인은 최종 상태를 검증만 한다 (변경 불가)")
	fmt.Println("  4. Handles()로 관심 있는 Operation만 처리한다")
	fmt.Println("  5. 하나의 플러그인이 Mutating + Validating 모두 구현할 수 있다 (예: LimitRanger)")
	fmt.Println("  6. Webhook은 외부 HTTP 서버에 검증을 위임한다")
}
