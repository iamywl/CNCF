package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// =============================================================================
// Terraform Provider 플러그인 시스템 시뮬레이션
// =============================================================================
//
// Terraform은 프로바이더를 별도 프로세스로 실행하고 gRPC로 통신한다.
// 이 플러그인 아키텍처를 통해 프로바이더를 독립적으로 개발/배포할 수 있다.
//
// 실제 Terraform 소스:
//   - internal/plugin/plugin.go: 프로바이더 플러그인 클라이언트
//   - internal/plugin6/serve.go: gRPC 서버 (프로바이더 측)
//   - internal/providers/provider.go: Provider 인터페이스
//   - internal/terraform/node_resource_apply.go: Apply 시 프로바이더 호출
//
// 이 PoC에서 구현하는 핵심 개념:
//   1. Provider 인터페이스 (GetSchema, Configure, Plan, Apply, Read)
//   2. Mock "local" 프로바이더 (파일을 리소스로 관리)
//   3. 채널 기반 RPC 통신 (gRPC 시뮬레이션)
//   4. 스키마 캐싱
//   5. Plan → Apply 워크플로우

// =============================================================================
// 1. 스키마 정의
// =============================================================================

// AttributeSchema는 하나의 속성 스키마를 나타낸다.
type AttributeSchema struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // "string", "number", "bool"
	Required    bool   `json:"required"`
	Computed    bool   `json:"computed"`     // 프로바이더가 계산하는 값
	ForceNew    bool   `json:"force_new"`    // 변경 시 교체 필요
	Description string `json:"description"`
}

// ResourceSchema는 리소스 타입의 스키마를 나타낸다.
type ResourceSchema struct {
	TypeName   string            `json:"type_name"`
	Attributes []AttributeSchema `json:"attributes"`
}

// ProviderSchema는 프로바이더의 전체 스키마를 나타낸다.
type ProviderSchema struct {
	ProviderName string                    `json:"provider_name"`
	Resources    map[string]ResourceSchema `json:"resources"`
}

// =============================================================================
// 2. Provider 인터페이스
// =============================================================================

// Provider는 Terraform 프로바이더 인터페이스이다.
// Terraform의 providers.Interface에 대응한다.
//
// 실제 Terraform에서는 gRPC를 통해 이 인터페이스의 메서드를 호출한다:
//   - GetProviderSchema → 스키마 조회
//   - ConfigureProvider → 프로바이더 설정 (인증 정보 등)
//   - PlanResourceChange → 변경 계획 계산
//   - ApplyResourceChange → 실제 변경 적용
//   - ReadResource → 현재 리소스 상태 읽기
type Provider interface {
	GetSchema() *ProviderSchema
	Configure(config map[string]string) error
	PlanResourceChange(typeName string, priorState, proposedNew map[string]string) (*ResourceChange, error)
	ApplyResourceChange(typeName string, change *ResourceChange) (*ApplyResult, error)
	ReadResource(typeName string, currentState map[string]string) (map[string]string, error)
}

// ResourceChange는 Plan 결과를 나타낸다.
type ResourceChange struct {
	Action     string            // "create", "update", "delete", "replace"
	PriorState map[string]string // 이전 상태
	PlannedNew map[string]string // 계획된 새 상태
}

// ApplyResult는 Apply 결과를 나타낸다.
type ApplyResult struct {
	NewState map[string]string
	Error    error
}

// =============================================================================
// 3. Local 프로바이더 구현
// =============================================================================

// LocalProvider는 파일 시스템을 관리하는 mock 프로바이더이다.
// 파일을 "리소스"로 관리하여 프로바이더의 동작을 시뮬레이션한다.
type LocalProvider struct {
	configured bool
	baseDir    string
}

func NewLocalProvider() *LocalProvider {
	return &LocalProvider{}
}

// GetSchema는 프로바이더 스키마를 반환한다.
func (p *LocalProvider) GetSchema() *ProviderSchema {
	return &ProviderSchema{
		ProviderName: "local",
		Resources: map[string]ResourceSchema{
			"local_file": {
				TypeName: "local_file",
				Attributes: []AttributeSchema{
					{Name: "filename", Type: "string", Required: true, ForceNew: true, Description: "파일 경로"},
					{Name: "content", Type: "string", Required: true, Description: "파일 내용"},
					{Name: "id", Type: "string", Computed: true, Description: "리소스 ID"},
					{Name: "content_md5", Type: "string", Computed: true, Description: "내용 MD5 해시"},
					{Name: "file_permission", Type: "string", Required: false, Description: "파일 권한"},
				},
			},
			"local_sensitive_file": {
				TypeName: "local_sensitive_file",
				Attributes: []AttributeSchema{
					{Name: "filename", Type: "string", Required: true, ForceNew: true, Description: "파일 경로"},
					{Name: "content", Type: "string", Required: true, Description: "민감한 파일 내용"},
					{Name: "id", Type: "string", Computed: true, Description: "리소스 ID"},
				},
			},
		},
	}
}

// Configure는 프로바이더를 설정한다.
func (p *LocalProvider) Configure(config map[string]string) error {
	baseDir := config["base_dir"]
	if baseDir == "" {
		return fmt.Errorf("base_dir 설정이 필요합니다")
	}
	p.baseDir = baseDir
	p.configured = true

	// 베이스 디렉토리 생성
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return fmt.Errorf("base_dir 생성 실패: %w", err)
	}

	fmt.Printf("    [LocalProvider] 설정 완료: base_dir=%s\n", baseDir)
	return nil
}

// PlanResourceChange는 리소스 변경 계획을 계산한다.
func (p *LocalProvider) PlanResourceChange(typeName string, priorState, proposedNew map[string]string) (*ResourceChange, error) {
	if !p.configured {
		return nil, fmt.Errorf("프로바이더가 설정되지 않았습니다")
	}

	change := &ResourceChange{
		PriorState: priorState,
		PlannedNew: make(map[string]string),
	}

	// 기존 상태가 없으면 Create
	if len(priorState) == 0 || priorState["id"] == "" {
		change.Action = "create"
		for k, v := range proposedNew {
			change.PlannedNew[k] = v
		}
		// Computed 속성은 "(known after apply)"로 표시
		change.PlannedNew["id"] = "(known after apply)"
		change.PlannedNew["content_md5"] = "(known after apply)"
		return change, nil
	}

	// proposedNew가 비어있으면 Delete
	if len(proposedNew) == 0 {
		change.Action = "delete"
		return change, nil
	}

	// ForceNew 속성 변경 확인
	schema := p.GetSchema().Resources[typeName]
	forceReplace := false
	for _, attr := range schema.Attributes {
		if attr.ForceNew {
			oldVal := priorState[attr.Name]
			newVal := proposedNew[attr.Name]
			if oldVal != newVal && oldVal != "" && newVal != "" {
				forceReplace = true
				break
			}
		}
	}

	if forceReplace {
		change.Action = "replace"
	} else {
		change.Action = "update"
	}

	for k, v := range proposedNew {
		change.PlannedNew[k] = v
	}
	change.PlannedNew["id"] = priorState["id"]
	change.PlannedNew["content_md5"] = "(known after apply)"

	return change, nil
}

// ApplyResourceChange는 리소스 변경을 실제로 적용한다.
func (p *LocalProvider) ApplyResourceChange(typeName string, change *ResourceChange) (*ApplyResult, error) {
	if !p.configured {
		return nil, fmt.Errorf("프로바이더가 설정되지 않았습니다")
	}

	switch change.Action {
	case "create", "update":
		filename := change.PlannedNew["filename"]
		content := change.PlannedNew["content"]
		permission := change.PlannedNew["file_permission"]
		if permission == "" {
			permission = "0644"
		}

		fullPath := filepath.Join(p.baseDir, filename)
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return &ApplyResult{Error: err}, nil
		}

		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return &ApplyResult{Error: err}, nil
		}

		// Computed 속성 계산
		newState := make(map[string]string)
		for k, v := range change.PlannedNew {
			newState[k] = v
		}
		newState["id"] = fmt.Sprintf("local-%s", filename)
		newState["content_md5"] = fmt.Sprintf("%x", simpleHash(content))

		return &ApplyResult{NewState: newState}, nil

	case "delete":
		filename := change.PriorState["filename"]
		fullPath := filepath.Join(p.baseDir, filename)
		os.Remove(fullPath)
		return &ApplyResult{NewState: nil}, nil

	case "replace":
		// 먼저 삭제
		if change.PriorState["filename"] != "" {
			oldPath := filepath.Join(p.baseDir, change.PriorState["filename"])
			os.Remove(oldPath)
		}
		// 다시 생성
		change.Action = "create"
		return p.ApplyResourceChange(typeName, change)

	default:
		return nil, fmt.Errorf("알 수 없는 액션: %s", change.Action)
	}
}

// ReadResource는 리소스의 현재 상태를 읽는다.
func (p *LocalProvider) ReadResource(typeName string, currentState map[string]string) (map[string]string, error) {
	if !p.configured {
		return nil, fmt.Errorf("프로바이더가 설정되지 않았습니다")
	}

	filename := currentState["filename"]
	fullPath := filepath.Join(p.baseDir, filename)

	data, err := os.ReadFile(fullPath)
	if err != nil {
		// 파일이 없으면 nil 반환 (리소스가 외부에서 삭제됨)
		return nil, nil
	}

	state := map[string]string{
		"id":              currentState["id"],
		"filename":        filename,
		"content":         string(data),
		"content_md5":     fmt.Sprintf("%x", simpleHash(string(data))),
		"file_permission": "0644",
	}
	return state, nil
}

// simpleHash는 간단한 해시 함수이다 (MD5 대체).
func simpleHash(s string) uint32 {
	var h uint32
	for _, c := range s {
		h = h*31 + uint32(c)
	}
	return h
}

// =============================================================================
// 4. 플러그인 RPC 시뮬레이션
// =============================================================================

// RPCRequest는 RPC 요청을 나타낸다.
type RPCRequest struct {
	Method   string
	Args     json.RawMessage
	Response chan RPCResponse
}

// RPCResponse는 RPC 응답을 나타낸다.
type RPCResponse struct {
	Result json.RawMessage
	Error  string
}

// PluginServer는 프로바이더를 별도 "프로세스"(goroutine)로 실행하는 서버이다.
// Terraform에서는 실제로 go-plugin 라이브러리를 사용하여
// 프로바이더를 별도 바이너리 프로세스로 실행하고 gRPC로 통신한다.
type PluginServer struct {
	provider Provider
	requests chan RPCRequest
	done     chan struct{}
}

// NewPluginServer는 새로운 플러그인 서버를 생성한다.
func NewPluginServer(provider Provider) *PluginServer {
	return &PluginServer{
		provider: provider,
		requests: make(chan RPCRequest, 10),
		done:     make(chan struct{}),
	}
}

// Start는 플러그인 서버를 goroutine으로 시작한다.
func (s *PluginServer) Start() {
	go func() {
		for {
			select {
			case req := <-s.requests:
				resp := s.handleRequest(req)
				req.Response <- resp
			case <-s.done:
				return
			}
		}
	}()
}

// Stop은 플러그인 서버를 중지한다.
func (s *PluginServer) Stop() {
	close(s.done)
}

func (s *PluginServer) handleRequest(req RPCRequest) RPCResponse {
	switch req.Method {
	case "GetSchema":
		schema := s.provider.GetSchema()
		data, _ := json.Marshal(schema)
		return RPCResponse{Result: data}

	case "Configure":
		var config map[string]string
		json.Unmarshal(req.Args, &config)
		err := s.provider.Configure(config)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Result: []byte(`"ok"`)}

	case "PlanResourceChange":
		var args struct {
			TypeName    string            `json:"type_name"`
			PriorState  map[string]string `json:"prior_state"`
			ProposedNew map[string]string `json:"proposed_new"`
		}
		json.Unmarshal(req.Args, &args)
		change, err := s.provider.PlanResourceChange(args.TypeName, args.PriorState, args.ProposedNew)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		data, _ := json.Marshal(change)
		return RPCResponse{Result: data}

	case "ApplyResourceChange":
		var change ResourceChange
		json.Unmarshal(req.Args, &change)
		// typeName을 change에서 추출하는 대신 간소화
		result, err := s.provider.ApplyResourceChange("local_file", &change)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		if result.Error != nil {
			return RPCResponse{Error: result.Error.Error()}
		}
		data, _ := json.Marshal(result.NewState)
		return RPCResponse{Result: data}

	default:
		return RPCResponse{Error: fmt.Sprintf("알 수 없는 메서드: %s", req.Method)}
	}
}

// =============================================================================
// 5. 플러그인 클라이언트 (Terraform 측)
// =============================================================================

// PluginClient는 플러그인 서버와 통신하는 클라이언트이다.
// Terraform의 plugin.GRPCProvider에 대응한다.
type PluginClient struct {
	server      *PluginServer
	schemaCache *ProviderSchema
	cacheMu     sync.Mutex
}

// NewPluginClient는 새로운 플러그인 클라이언트를 생성한다.
func NewPluginClient(server *PluginServer) *PluginClient {
	return &PluginClient{
		server: server,
	}
}

func (c *PluginClient) call(method string, args interface{}) (json.RawMessage, error) {
	argsData, _ := json.Marshal(args)

	respCh := make(chan RPCResponse, 1)
	c.server.requests <- RPCRequest{
		Method:   method,
		Args:     argsData,
		Response: respCh,
	}

	resp := <-respCh
	if resp.Error != "" {
		return nil, fmt.Errorf(resp.Error)
	}
	return resp.Result, nil
}

// GetSchema는 스키마를 조회한다 (캐싱 적용).
func (c *PluginClient) GetSchema() (*ProviderSchema, error) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	if c.schemaCache != nil {
		fmt.Println("    [Client] 스키마 캐시 히트")
		return c.schemaCache, nil
	}

	fmt.Println("    [Client] 스키마 조회 RPC 호출")
	result, err := c.call("GetSchema", nil)
	if err != nil {
		return nil, err
	}

	var schema ProviderSchema
	json.Unmarshal(result, &schema)
	c.schemaCache = &schema
	return &schema, nil
}

// Configure는 프로바이더를 설정한다.
func (c *PluginClient) Configure(config map[string]string) error {
	_, err := c.call("Configure", config)
	return err
}

// PlanResourceChange는 변경 계획을 요청한다.
func (c *PluginClient) PlanResourceChange(typeName string, priorState, proposedNew map[string]string) (*ResourceChange, error) {
	args := map[string]interface{}{
		"type_name":    typeName,
		"prior_state":  priorState,
		"proposed_new": proposedNew,
	}
	result, err := c.call("PlanResourceChange", args)
	if err != nil {
		return nil, err
	}

	var change ResourceChange
	json.Unmarshal(result, &change)
	return &change, nil
}

// ApplyResourceChange는 변경을 적용한다.
func (c *PluginClient) ApplyResourceChange(change *ResourceChange) (map[string]string, error) {
	result, err := c.call("ApplyResourceChange", change)
	if err != nil {
		return nil, err
	}

	var newState map[string]string
	json.Unmarshal(result, &newState)
	return newState, nil
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║   Terraform Provider 플러그인 시스템 시뮬레이션             ║")
	fmt.Println("║   실제 코드: internal/plugin/, internal/providers/       ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "terraform-provider-poc-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// =========================================================================
	// 데모 1: 프로바이더 플러그인 시작
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 1: 프로바이더 플러그인 프로세스 시작")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 프로바이더 생성 및 플러그인 서버 시작
	localProvider := NewLocalProvider()
	server := NewPluginServer(localProvider)
	server.Start()
	defer server.Stop()

	fmt.Println("    ✓ LocalProvider 플러그인 서버 시작 (goroutine)")
	fmt.Println("    통신: 채널 기반 RPC (실제: gRPC over unix socket)")
	fmt.Println()

	// 클라이언트 생성
	client := NewPluginClient(server)
	fmt.Println("    ✓ 플러그인 클라이언트 생성 (Terraform 측)")
	fmt.Println()

	// =========================================================================
	// 데모 2: 스키마 조회 (캐싱 포함)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 2: 스키마 조회 (GetSchema RPC)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 첫 번째 호출 (RPC 실행)
	schema, err := client.GetSchema()
	if err != nil {
		fmt.Printf("  스키마 조회 실패: %v\n", err)
		return
	}

	fmt.Printf("    프로바이더: %s\n", schema.ProviderName)
	fmt.Printf("    리소스 타입: %d개\n", len(schema.Resources))
	for typeName, rs := range schema.Resources {
		fmt.Printf("      %s:\n", typeName)
		for _, attr := range rs.Attributes {
			tags := []string{}
			if attr.Required {
				tags = append(tags, "required")
			}
			if attr.Computed {
				tags = append(tags, "computed")
			}
			if attr.ForceNew {
				tags = append(tags, "force_new")
			}
			fmt.Printf("        %-20s %-8s [%s] %s\n",
				attr.Name, attr.Type, strings.Join(tags, ","), attr.Description)
		}
	}
	fmt.Println()

	// 두 번째 호출 (캐시 히트)
	_, _ = client.GetSchema()
	fmt.Println()

	// =========================================================================
	// 데모 3: 프로바이더 설정 (Configure)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 3: 프로바이더 설정 (Configure RPC)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	err = client.Configure(map[string]string{
		"base_dir": tmpDir,
	})
	if err != nil {
		fmt.Printf("  설정 실패: %v\n", err)
		return
	}
	fmt.Println("    ✓ 프로바이더 설정 완료")
	fmt.Println()

	// =========================================================================
	// 데모 4: Plan → Apply 워크플로우
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 4: Plan → Apply 워크플로우")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// --- 리소스 1: 생성 (Create) ---
	fmt.Println("  [리소스 1] local_file.hello - 생성(Create)")
	fmt.Println()

	// Plan
	plan1, err := client.PlanResourceChange("local_file",
		nil, // 이전 상태 없음 (새 리소스)
		map[string]string{
			"filename": "hello.txt",
			"content":  "안녕하세요, Terraform!",
		},
	)
	if err != nil {
		fmt.Printf("    Plan 실패: %v\n", err)
		return
	}
	fmt.Printf("    Plan 결과: action=%s\n", plan1.Action)
	for k, v := range plan1.PlannedNew {
		fmt.Printf("      + %-20s = %s\n", k, v)
	}
	fmt.Println()

	// Apply
	newState1, err := client.ApplyResourceChange(plan1)
	if err != nil {
		fmt.Printf("    Apply 실패: %v\n", err)
		return
	}
	fmt.Println("    Apply 결과:")
	for k, v := range newState1 {
		fmt.Printf("      %-20s = %s\n", k, v)
	}
	fmt.Println()

	// 실제 파일 확인
	content, _ := os.ReadFile(filepath.Join(tmpDir, "hello.txt"))
	fmt.Printf("    실제 파일 내용: %s\n", string(content))
	fmt.Println()

	// --- 리소스 2: 생성 및 업데이트 ---
	fmt.Println("  [리소스 2] local_file.config - 생성(Create) → 업데이트(Update)")
	fmt.Println()

	// Create
	plan2, _ := client.PlanResourceChange("local_file",
		nil,
		map[string]string{
			"filename": "config.json",
			"content":  `{"version": 1, "env": "dev"}`,
		},
	)
	newState2, _ := client.ApplyResourceChange(plan2)
	fmt.Printf("    Create 완료: id=%s\n", newState2["id"])

	// Update (content 변경)
	plan3, _ := client.PlanResourceChange("local_file",
		newState2,
		map[string]string{
			"filename": "config.json",
			"content":  `{"version": 2, "env": "prod"}`,
		},
	)
	fmt.Printf("    Update Plan: action=%s\n", plan3.Action)

	newState3, _ := client.ApplyResourceChange(plan3)
	fmt.Printf("    Update 완료: content_md5=%s\n", newState3["content_md5"])

	// 파일 내용 확인
	content2, _ := os.ReadFile(filepath.Join(tmpDir, "config.json"))
	fmt.Printf("    실제 파일 내용: %s\n", string(content2))
	fmt.Println()

	// --- 리소스 3: Replace (filename 변경 = ForceNew) ---
	fmt.Println("  [리소스 3] local_file.config - 교체(Replace, filename 변경)")
	fmt.Println()

	plan4, _ := client.PlanResourceChange("local_file",
		newState3,
		map[string]string{
			"filename": "config-v2.json",                    // ForceNew 속성 변경!
			"content":  `{"version": 2, "env": "prod"}`,
		},
	)
	fmt.Printf("    Plan 결과: action=%s (filename이 ForceNew이므로 교체)\n", plan4.Action)

	newState4, _ := client.ApplyResourceChange(plan4)
	fmt.Printf("    Replace 완료: id=%s\n", newState4["id"])

	// 이전 파일 삭제 확인
	_, errOld := os.Stat(filepath.Join(tmpDir, "config.json"))
	fmt.Printf("    이전 파일(config.json) 존재: %v\n", !os.IsNotExist(errOld))

	_, errNew := os.Stat(filepath.Join(tmpDir, "config-v2.json"))
	fmt.Printf("    새 파일(config-v2.json) 존재: %v\n", !os.IsNotExist(errNew))
	fmt.Println()

	// =========================================================================
	// 통신 아키텍처 다이어그램
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  프로바이더 플러그인 통신 아키텍처")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  ┌───────────────────────┐     ┌───────────────────────┐")
	fmt.Println("  │    Terraform Core     │     │   Provider Plugin     │")
	fmt.Println("  │                       │     │   (별도 프로세스)       │")
	fmt.Println("  │  ┌─────────────────┐  │     │  ┌─────────────────┐  │")
	fmt.Println("  │  │  PluginClient   │──┼─RPC─┼─▶│  PluginServer   │  │")
	fmt.Println("  │  │  (gRPC Client)  │◀─┼─RPC─┼──│  (gRPC Server)  │  │")
	fmt.Println("  │  └─────────────────┘  │     │  └────────┬────────┘  │")
	fmt.Println("  │                       │     │           │           │")
	fmt.Println("  │  ┌─────────────────┐  │     │  ┌────────▼────────┐  │")
	fmt.Println("  │  │  Schema Cache   │  │     │  │  LocalProvider  │  │")
	fmt.Println("  │  └─────────────────┘  │     │  │  (실제 구현체)    │  │")
	fmt.Println("  │                       │     │  └─────────────────┘  │")
	fmt.Println("  └───────────────────────┘     └───────────────────────┘")
	fmt.Println()

	// =========================================================================
	// 핵심 포인트
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  핵심 포인트 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. 프로바이더는 별도 바이너리 프로세스로 실행된다 (go-plugin)")
	fmt.Println("  2. Terraform Core와 프로바이더는 gRPC로 통신한다")
	fmt.Println("  3. 스키마를 통해 리소스 속성의 Required/Computed/ForceNew를 정의한다")
	fmt.Println("  4. PlanResourceChange로 변경 계획을 계산하고")
	fmt.Println("     ApplyResourceChange로 실제 변경을 적용한다")
	fmt.Println("  5. 스키마 캐싱으로 불필요한 RPC 호출을 줄인다")
	fmt.Println("  6. 프로바이더 분리로 Terraform Core와 독립적으로 개발/배포 가능")
}
