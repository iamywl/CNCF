// poc-03-api-request: 쿠버네티스 API Server 핸들러 체인 시뮬레이션
//
// API Server가 요청을 처리하는 과정에서 거치는 미들웨어 체인을 구현한다.
// 실제 K8s에서는 요청이 다음 순서로 처리된다:
//
//   HTTP 요청 → Authentication → Authorization → Admission (Mutating → Validating) → Storage
//
// 참조 소스:
//   - staging/src/k8s.io/apiserver/pkg/server/config.go (DefaultBuildHandlerChain)
//   - staging/src/k8s.io/apiserver/pkg/endpoints/filters/authentication.go
//   - staging/src/k8s.io/apiserver/pkg/endpoints/filters/authorization.go
//   - plugin/pkg/admission/ (admission controllers)
//   - staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store.go
//
// 실행: go run main.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 데이터 모델
// ============================================================================

// Pod는 간소화된 파드 리소스이다.
type Pod struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Labels     map[string]string `json:"labels,omitempty"`
	Image      string            `json:"image"`
	// Admission Controller가 주입하는 필드들
	ServiceAccount string `json:"serviceAccount,omitempty"`
	DNSPolicy      string `json:"dnsPolicy,omitempty"`
}

// UserInfo는 인증된 사용자 정보이다.
// 실제 소스: staging/src/k8s.io/apiserver/pkg/authentication/user/user.go
type UserInfo struct {
	Username string
	Groups   []string
}

// RBACRule은 RBAC 권한 규칙이다.
// 실제 소스: staging/src/k8s.io/api/rbac/v1/types.go
type RBACRule struct {
	Verbs     []string // get, list, create, update, delete
	Resources []string // pods, services, deployments
	Groups    []string // 이 규칙이 적용되는 그룹
}

// ============================================================================
// 컨텍스트 키 — 미들웨어 간 데이터 전달
// 실제 K8s에서는 context.Context에 저장한다.
// ============================================================================

// requestContextKey는 요청 헤더에 사용자 정보를 저장하기 위한 키이다.
const (
	headerUser   = "X-User"
	headerGroups = "X-Groups"
)

// ============================================================================
// 1. Authentication — 인증 미들웨어
// 실제 소스: staging/src/k8s.io/apiserver/pkg/endpoints/filters/authentication.go
//
// K8s는 여러 인증 전략을 체인으로 시도한다:
// - Bearer Token, X.509 Client Certificate, ServiceAccount Token 등
// ============================================================================

// tokenDB는 유효한 토큰 → 사용자 매핑이다 (실제로는 OIDC, webhook 등).
var tokenDB = map[string]*UserInfo{
	"admin-token": {Username: "admin", Groups: []string{"system:masters"}},
	"dev-token":   {Username: "developer", Groups: []string{"developers"}},
	"readonly-token": {Username: "viewer", Groups: []string{"viewers"}},
}

// AuthenticationMiddleware는 토큰을 검증하고 사용자 정보를 컨텍스트에 주입한다.
func AuthenticationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")

		if authHeader == "" {
			// 익명 접근 시도
			fmt.Println("  [인증] 토큰 없음 → 익명 사용자 (system:anonymous)")
			r.Header.Set(headerUser, "system:anonymous")
			r.Header.Set(headerGroups, "system:unauthenticated")
			next.ServeHTTP(w, r)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		user, ok := tokenDB[token]
		if !ok {
			fmt.Printf("  [인증] 실패 — 유효하지 않은 토큰: %s\n", token)
			http.Error(w, `{"error":"Unauthorized","message":"유효하지 않은 토큰"}`, http.StatusUnauthorized)
			return
		}

		fmt.Printf("  [인증] 성공 — 사용자=%s, 그룹=%v\n", user.Username, user.Groups)
		r.Header.Set(headerUser, user.Username)
		r.Header.Set(headerGroups, strings.Join(user.Groups, ","))
		next.ServeHTTP(w, r)
	})
}

// ============================================================================
// 2. Authorization — 인가 미들웨어 (RBAC)
// 실제 소스: plugin/pkg/auth/authorizer/rbac/rbac.go
//
// K8s는 여러 인가 모드를 지원한다: RBAC, ABAC, Webhook, Node
// 여기서는 RBAC만 구현한다.
// ============================================================================

// rbacRules는 RBAC 정책 규칙이다.
var rbacRules = []RBACRule{
	// system:masters 그룹은 모든 것에 접근 가능
	{Verbs: []string{"*"}, Resources: []string{"*"}, Groups: []string{"system:masters"}},
	// developers 그룹은 pods에 대해 CRUD 가능
	{Verbs: []string{"get", "list", "create", "update", "delete"}, Resources: []string{"pods"}, Groups: []string{"developers"}},
	// viewers 그룹은 pods에 대해 읽기만 가능
	{Verbs: []string{"get", "list"}, Resources: []string{"pods"}, Groups: []string{"viewers"}},
}

// httpMethodToVerb는 HTTP 메서드를 K8s API verb로 변환한다.
func httpMethodToVerb(method string) string {
	switch method {
	case "GET":
		return "get"
	case "POST":
		return "create"
	case "PUT":
		return "update"
	case "DELETE":
		return "delete"
	default:
		return "unknown"
	}
}

// AuthorizationMiddleware는 RBAC 규칙에 따라 요청을 인가한다.
func AuthorizationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get(headerUser)
		groups := strings.Split(r.Header.Get(headerGroups), ",")
		verb := httpMethodToVerb(r.Method)
		resource := "pods" // 간소화: 항상 pods 리소스

		// RBAC 규칙 매칭
		allowed := false
		for _, rule := range rbacRules {
			if matchGroups(rule.Groups, groups) &&
				matchVerb(rule.Verbs, verb) &&
				matchResource(rule.Resources, resource) {
				allowed = true
				break
			}
		}

		if !allowed {
			fmt.Printf("  [인가] 거부 — 사용자=%s, verb=%s, resource=%s\n", user, verb, resource)
			http.Error(w, fmt.Sprintf(`{"error":"Forbidden","message":"%s는 %s에 대해 %s 권한 없음"}`,
				user, resource, verb), http.StatusForbidden)
			return
		}

		fmt.Printf("  [인가] 허용 — 사용자=%s, verb=%s, resource=%s\n", user, verb, resource)
		next.ServeHTTP(w, r)
	})
}

func matchGroups(ruleGroups, userGroups []string) bool {
	for _, rg := range ruleGroups {
		for _, ug := range userGroups {
			if rg == ug {
				return true
			}
		}
	}
	return false
}

func matchVerb(verbs []string, verb string) bool {
	for _, v := range verbs {
		if v == "*" || v == verb {
			return true
		}
	}
	return false
}

func matchResource(resources []string, resource string) bool {
	for _, r := range resources {
		if r == "*" || r == resource {
			return true
		}
	}
	return false
}

// ============================================================================
// 3. Admission Controllers — 변경(Mutating) + 검증(Validating)
// 실제 소스: plugin/pkg/admission/ 디렉토리
//
// Admission Controller는 두 단계로 나뉜다:
// - Mutating: 요청을 변경 (기본값 주입, 사이드카 삽입 등)
// - Validating: 요청 유효성 검증 (정책 준수 확인)
// ============================================================================

// AdmissionController 인터페이스
type AdmissionController interface {
	Name() string
	Admit(pod *Pod) error // Mutating + Validating 겸용
}

// DefaultServiceAccountAdmission은 ServiceAccount를 자동 주입한다.
// 실제 소스: plugin/pkg/admission/serviceaccount/admission.go
type DefaultServiceAccountAdmission struct{}

func (a *DefaultServiceAccountAdmission) Name() string { return "DefaultServiceAccount" }
func (a *DefaultServiceAccountAdmission) Admit(pod *Pod) error {
	if pod.ServiceAccount == "" {
		pod.ServiceAccount = "default"
		fmt.Printf("  [어드미션-Mutating] %s: ServiceAccount='default' 주입\n", a.Name())
	}
	return nil
}

// DefaultDNSPolicyAdmission은 DNS 정책을 자동 주입한다.
type DefaultDNSPolicyAdmission struct{}

func (a *DefaultDNSPolicyAdmission) Name() string { return "DefaultDNSPolicy" }
func (a *DefaultDNSPolicyAdmission) Admit(pod *Pod) error {
	if pod.DNSPolicy == "" {
		pod.DNSPolicy = "ClusterFirst"
		fmt.Printf("  [어드미션-Mutating] %s: DNSPolicy='ClusterFirst' 주입\n", a.Name())
	}
	return nil
}

// NamespaceExistsAdmission은 네임스페이스 존재 여부를 확인한다.
// 실제 소스: plugin/pkg/admission/namespace/exists/admission.go
type NamespaceExistsAdmission struct {
	validNamespaces map[string]bool
}

func (a *NamespaceExistsAdmission) Name() string { return "NamespaceExists" }
func (a *NamespaceExistsAdmission) Admit(pod *Pod) error {
	if !a.validNamespaces[pod.Namespace] {
		return fmt.Errorf("네임스페이스 '%s'가 존재하지 않음", pod.Namespace)
	}
	fmt.Printf("  [어드미션-Validating] %s: 네임스페이스 '%s' 확인 완료\n", a.Name(), pod.Namespace)
	return nil
}

// ImagePolicyAdmission은 이미지 정책을 검증한다.
type ImagePolicyAdmission struct {
	deniedPrefixes []string
}

func (a *ImagePolicyAdmission) Name() string { return "ImagePolicy" }
func (a *ImagePolicyAdmission) Admit(pod *Pod) error {
	for _, prefix := range a.deniedPrefixes {
		if strings.HasPrefix(pod.Image, prefix) {
			return fmt.Errorf("이미지 '%s'는 정책에 의해 차단됨 (차단 접두사: %s)", pod.Image, prefix)
		}
	}
	fmt.Printf("  [어드미션-Validating] %s: 이미지 '%s' 허용\n", a.Name(), pod.Image)
	return nil
}

// AdmissionChain은 여러 Admission Controller를 순서대로 실행한다.
type AdmissionChain struct {
	mutating   []AdmissionController
	validating []AdmissionController
}

// Admit은 Mutating → Validating 순서로 어드미션을 실행한다.
func (ac *AdmissionChain) Admit(pod *Pod) error {
	// Phase 1: Mutating admission
	for _, ctrl := range ac.mutating {
		if err := ctrl.Admit(pod); err != nil {
			return fmt.Errorf("[%s] %v", ctrl.Name(), err)
		}
	}
	// Phase 2: Validating admission
	for _, ctrl := range ac.validating {
		if err := ctrl.Admit(pod); err != nil {
			return fmt.Errorf("[%s] %v", ctrl.Name(), err)
		}
	}
	return nil
}

// ============================================================================
// 4. Storage — 인메모리 저장소 (etcd 역할)
// 실제 소스: staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store.go
// ============================================================================

// InMemoryStorage는 etcd를 대체하는 인메모리 저장소이다.
type InMemoryStorage struct {
	mu   sync.RWMutex
	pods map[string]*Pod // namespace/name → Pod
}

func NewStorage() *InMemoryStorage {
	return &InMemoryStorage{pods: make(map[string]*Pod)}
}

func (s *InMemoryStorage) Create(pod *Pod) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := pod.Namespace + "/" + pod.Name
	if _, exists := s.pods[key]; exists {
		return fmt.Errorf("파드 '%s' 이미 존재", key)
	}
	stored := *pod
	s.pods[key] = &stored
	return nil
}

func (s *InMemoryStorage) Get(namespace, name string) (*Pod, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pods[namespace+"/"+name]
	if ok {
		cp := *p
		return &cp, true
	}
	return nil, false
}

func (s *InMemoryStorage) List() []*Pod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Pod, 0, len(s.pods))
	for _, p := range s.pods {
		cp := *p
		result = append(result, &cp)
	}
	return result
}

func (s *InMemoryStorage) Delete(namespace, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := namespace + "/" + name
	if _, ok := s.pods[key]; ok {
		delete(s.pods, key)
		return true
	}
	return false
}

// ============================================================================
// 5. API Handler — 최종 요청 처리기
// ============================================================================

// APIHandler는 인증/인가/어드미션을 모두 통과한 요청을 처리한다.
type APIHandler struct {
	storage   *InMemoryStorage
	admission *AdmissionChain
}

func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v1/pods":
		// LIST pods
		pods := h.storage.List()
		json.NewEncoder(w).Encode(pods)
		fmt.Printf("  [스토리지] LIST pods → %d개 반환\n", len(pods))

	case r.Method == "POST" && r.URL.Path == "/api/v1/pods":
		// CREATE pod
		body, _ := io.ReadAll(r.Body)
		var pod Pod
		if err := json.Unmarshal(body, &pod); err != nil {
			http.Error(w, `{"error":"BadRequest"}`, http.StatusBadRequest)
			return
		}
		if pod.Namespace == "" {
			pod.Namespace = "default"
		}
		pod.Kind = "Pod"
		pod.APIVersion = "v1"

		// Admission Controller 실행
		if err := h.admission.Admit(&pod); err != nil {
			fmt.Printf("  [어드미션] 거부: %v\n", err)
			http.Error(w, fmt.Sprintf(`{"error":"Forbidden","message":"%s"}`, err.Error()), http.StatusForbidden)
			return
		}

		// 스토리지에 저장
		if err := h.storage.Create(&pod); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"Conflict","message":"%s"}`, err.Error()), http.StatusConflict)
			return
		}

		fmt.Printf("  [스토리지] CREATE pod → %s/%s 저장 완료\n", pod.Namespace, pod.Name)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(pod)

	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"):
		// DELETE pod: /api/v1/namespaces/{ns}/pods/{name}
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) >= 7 {
			ns, name := parts[4], parts[6]
			if h.storage.Delete(ns, name) {
				fmt.Printf("  [스토리지] DELETE pod → %s/%s 삭제 완료\n", ns, name)
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"status":"삭제 완료","name":"%s"}`, name)
			} else {
				http.Error(w, `{"error":"NotFound"}`, http.StatusNotFound)
			}
		}

	default:
		http.Error(w, `{"error":"NotFound"}`, http.StatusNotFound)
	}
}

// ============================================================================
// 핸들러 체인 조립 — DefaultBuildHandlerChain 패턴
// 실제 소스: staging/src/k8s.io/apiserver/pkg/server/config.go
// ============================================================================

func buildHandlerChain(handler http.Handler) http.Handler {
	// 체인은 안쪽에서 바깥으로 감싼다 (실행 순서: 바깥 → 안쪽)
	// 즉, Authentication이 가장 먼저, handler가 가장 마지막에 실행된다
	handler = AuthorizationMiddleware(handler) // 2. 인가
	handler = AuthenticationMiddleware(handler) // 1. 인증
	return handler
}

// ============================================================================
// 테스트 요청 헬퍼
// ============================================================================

func sendRequest(server *httptest.Server, method, path, token, body string) {
	fmt.Printf("\n────────────────────────────────────\n")
	fmt.Printf("요청: %s %s\n", method, path)
	if token != "" {
		fmt.Printf("토큰: %s\n", token)
	}
	if body != "" {
		fmt.Printf("본문: %s\n", body)
	}
	fmt.Println("처리 과정:")

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}

	req, _ := http.NewRequest(method, server.URL+path, reqBody)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	fmt.Printf("응답: %d %s\n", resp.StatusCode, string(respBody))
}

// ============================================================================
// 메인
// ============================================================================

func main() {
	fmt.Println("========================================")
	fmt.Println("Kubernetes API Server 핸들러 체인 시뮬레이션")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Println("요청 처리 순서:")
	fmt.Println("  HTTP → 인증(Authentication) → 인가(Authorization) → 어드미션(Admission) → 스토리지(Storage)")
	fmt.Println()

	// RBAC 규칙 출력
	fmt.Println("RBAC 규칙:")
	fmt.Println("  system:masters → 모든 리소스에 대한 모든 작업")
	fmt.Println("  developers    → pods에 대해 get, list, create, update, delete")
	fmt.Println("  viewers       → pods에 대해 get, list만 가능")
	fmt.Println()

	fmt.Println("등록된 토큰:")
	fmt.Println("  admin-token    → admin (system:masters)")
	fmt.Println("  dev-token      → developer (developers)")
	fmt.Println("  readonly-token → viewer (viewers)")

	// Admission Chain 구성
	admission := &AdmissionChain{
		mutating: []AdmissionController{
			&DefaultServiceAccountAdmission{},
			&DefaultDNSPolicyAdmission{},
		},
		validating: []AdmissionController{
			&NamespaceExistsAdmission{validNamespaces: map[string]bool{
				"default": true, "kube-system": true, "production": true,
			}},
			&ImagePolicyAdmission{deniedPrefixes: []string{"malicious/", "untrusted/"}},
		},
	}

	// API Handler + 핸들러 체인 조립
	apiHandler := &APIHandler{
		storage:   NewStorage(),
		admission: admission,
	}
	handler := buildHandlerChain(apiHandler)
	server := httptest.NewServer(handler)
	defer server.Close()

	time.Sleep(50 * time.Millisecond)

	// ── 시나리오 1: 정상 파드 생성 (admin) ──
	sendRequest(server, "POST", "/api/v1/pods", "admin-token",
		`{"name":"nginx-pod","namespace":"default","image":"nginx:1.25","labels":{"app":"web"}}`)

	// ── 시나리오 2: 개발자가 파드 생성 (developer) ──
	sendRequest(server, "POST", "/api/v1/pods", "dev-token",
		`{"name":"dev-pod","namespace":"default","image":"node:18"}`)

	// ── 시나리오 3: 읽기 전용 사용자가 파드 생성 시도 (권한 부족) ──
	sendRequest(server, "POST", "/api/v1/pods", "readonly-token",
		`{"name":"blocked-pod","namespace":"default","image":"nginx:1.25"}`)

	// ── 시나리오 4: 읽기 전용 사용자가 파드 목록 조회 (허용) ──
	sendRequest(server, "GET", "/api/v1/pods", "readonly-token", "")

	// ── 시나리오 5: 잘못된 토큰 ──
	sendRequest(server, "GET", "/api/v1/pods", "invalid-token", "")

	// ── 시나리오 6: 존재하지 않는 네임스페이스 (Validating Admission 거부) ──
	sendRequest(server, "POST", "/api/v1/pods", "admin-token",
		`{"name":"bad-ns-pod","namespace":"nonexistent","image":"nginx:1.25"}`)

	// ── 시나리오 7: 차단된 이미지 (ImagePolicy Admission 거부) ──
	sendRequest(server, "POST", "/api/v1/pods", "admin-token",
		`{"name":"malware-pod","namespace":"default","image":"malicious/cryptominer:latest"}`)

	// ── 시나리오 8: 파드 삭제 ──
	sendRequest(server, "DELETE", "/api/v1/namespaces/default/pods/nginx-pod", "admin-token", "")

	// 최종 상태
	sendRequest(server, "GET", "/api/v1/pods", "admin-token", "")

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("핵심 포인트:")
	fmt.Println("========================================")
	fmt.Println("1. 핸들러 체인: 인증 → 인가 → 어드미션 → 스토리지 순서로 처리")
	fmt.Println("2. 인증 실패 시 401, 인가 실패 시 403, 어드미션 거부 시 403")
	fmt.Println("3. Mutating Admission: ServiceAccount, DNSPolicy 등 기본값 자동 주입")
	fmt.Println("4. Validating Admission: 네임스페이스 존재 확인, 이미지 정책 검증")
	fmt.Println("5. RBAC: 그룹 기반 권한 제어 (verbs × resources)")
	fmt.Println("6. 미들웨어 패턴: 각 단계가 독립적으로 확장 가능")
}
