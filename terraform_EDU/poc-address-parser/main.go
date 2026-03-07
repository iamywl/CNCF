package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// =============================================================================
// Terraform 주소(Address) 파싱 시뮬레이션
// =============================================================================
// Terraform은 리소스, 모듈, 프로바이더를 주소(Address)로 참조합니다.
// 실제 코드: internal/addrs/ 디렉토리
//
// 주소 형식 예시:
//   리소스:     aws_instance.web
//   인스턴스:   aws_instance.web[0], aws_instance.web["us-east-1"]
//   모듈:       module.network
//   절대주소:   module.network.aws_subnet.main[0]
//   프로바이더: registry.terraform.io/hashicorp/aws
//   데이터소스: data.aws_ami.latest
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// InstanceKey: 인스턴스 키 (count/for_each 인덱스)
// ─────────────────────────────────────────────────────────────────────────────

// InstanceKeyType은 인스턴스 키의 종류입니다.
type InstanceKeyType int

const (
	NoKey     InstanceKeyType = iota // count/for_each 없음
	IntKey                           // count 인덱스 (0, 1, 2, ...)
	StringKey                        // for_each 키 ("us-east-1", "web", ...)
)

// InstanceKey는 리소스 인스턴스의 키입니다.
// 실제: internal/addrs/instance_key.go
type InstanceKey struct {
	Type     InstanceKeyType
	IntVal   int
	StrVal   string
}

func NoInstanceKey() InstanceKey {
	return InstanceKey{Type: NoKey}
}

func IntInstanceKey(v int) InstanceKey {
	return InstanceKey{Type: IntKey, IntVal: v}
}

func StringInstanceKey(v string) InstanceKey {
	return InstanceKey{Type: StringKey, StrVal: v}
}

func (k InstanceKey) String() string {
	switch k.Type {
	case NoKey:
		return ""
	case IntKey:
		return fmt.Sprintf("[%d]", k.IntVal)
	case StringKey:
		return fmt.Sprintf("[%q]", k.StrVal)
	default:
		return ""
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Resource: 리소스 주소 (상대)
// ─────────────────────────────────────────────────────────────────────────────

// ResourceMode는 리소스의 종류입니다.
type ResourceMode int

const (
	ManagedResourceMode ResourceMode = iota // resource 블록
	DataResourceMode                        // data 블록
)

func (m ResourceMode) String() string {
	switch m {
	case ManagedResourceMode:
		return "managed"
	case DataResourceMode:
		return "data"
	default:
		return "unknown"
	}
}

// Resource는 리소스 주소입니다 (모듈 경로 없음).
// 실제: internal/addrs/resource.go
type Resource struct {
	Mode ResourceMode
	Type string
	Name string
}

func (r Resource) String() string {
	switch r.Mode {
	case DataResourceMode:
		return fmt.Sprintf("data.%s.%s", r.Type, r.Name)
	default:
		return fmt.Sprintf("%s.%s", r.Type, r.Name)
	}
}

// ResourceInstance는 리소스 인스턴스 주소입니다.
type ResourceInstance struct {
	Resource Resource
	Key      InstanceKey
}

func (ri ResourceInstance) String() string {
	return ri.Resource.String() + ri.Key.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// ModuleInstance: 모듈 인스턴스 경로
// ─────────────────────────────────────────────────────────────────────────────

// ModuleInstanceStep은 모듈 경로의 한 단계입니다.
// 실제: internal/addrs/module_instance.go
type ModuleInstanceStep struct {
	Name string
	Key  InstanceKey
}

func (s ModuleInstanceStep) String() string {
	return fmt.Sprintf("module.%s%s", s.Name, s.Key.String())
}

// ModuleInstance는 모듈 인스턴스의 전체 경로입니다.
// 예: module.network.module.subnet
type ModuleInstance []ModuleInstanceStep

func (mi ModuleInstance) String() string {
	if len(mi) == 0 {
		return "(root)"
	}
	var parts []string
	for _, step := range mi {
		parts = append(parts, step.String())
	}
	return strings.Join(parts, ".")
}

func (mi ModuleInstance) IsRoot() bool {
	return len(mi) == 0
}

// Parent는 상위 모듈 인스턴스를 반환합니다.
func (mi ModuleInstance) Parent() ModuleInstance {
	if len(mi) == 0 {
		return nil
	}
	return mi[:len(mi)-1]
}

// Child는 하위 모듈 인스턴스를 추가합니다.
func (mi ModuleInstance) Child(name string, key InstanceKey) ModuleInstance {
	ret := make(ModuleInstance, len(mi)+1)
	copy(ret, mi)
	ret[len(ret)-1] = ModuleInstanceStep{Name: name, Key: key}
	return ret
}

// ─────────────────────────────────────────────────────────────────────────────
// AbsResource / AbsResourceInstance: 절대 리소스 주소
// ─────────────────────────────────────────────────────────────────────────────

// AbsResource는 모듈 경로를 포함한 절대 리소스 주소입니다.
// 실제: internal/addrs/resource.go
type AbsResource struct {
	Module   ModuleInstance
	Resource Resource
}

func (ar AbsResource) String() string {
	if ar.Module.IsRoot() {
		return ar.Resource.String()
	}
	return ar.Module.String() + "." + ar.Resource.String()
}

// AbsResourceInstance는 모듈 경로를 포함한 절대 리소스 인스턴스 주소입니다.
type AbsResourceInstance struct {
	Module   ModuleInstance
	Resource ResourceInstance
}

func (ari AbsResourceInstance) String() string {
	if ari.Module.IsRoot() {
		return ari.Resource.String()
	}
	return ari.Module.String() + "." + ari.Resource.String()
}

// ContainingResource는 이 인스턴스가 속한 리소스의 절대 주소를 반환합니다.
func (ari AbsResourceInstance) ContainingResource() AbsResource {
	return AbsResource{
		Module:   ari.Module,
		Resource: ari.Resource.Resource,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Provider: 프로바이더 주소
// ─────────────────────────────────────────────────────────────────────────────

// Provider는 프로바이더의 정규화된 주소입니다.
// 실제: internal/addrs/provider.go
type Provider struct {
	Hostname  string // 예: registry.terraform.io
	Namespace string // 예: hashicorp
	Type      string // 예: aws
}

// DefaultRegistryHost는 기본 레지스트리 호스트입니다.
const DefaultRegistryHost = "registry.terraform.io"

func NewDefaultProvider(typeName string) Provider {
	return Provider{
		Hostname:  DefaultRegistryHost,
		Namespace: "hashicorp",
		Type:      typeName,
	}
}

func NewProvider(hostname, namespace, typeName string) Provider {
	return Provider{
		Hostname:  hostname,
		Namespace: namespace,
		Type:      typeName,
	}
}

func (p Provider) String() string {
	return fmt.Sprintf("%s/%s/%s", p.Hostname, p.Namespace, p.Type)
}

// ForDisplay는 사용자에게 보여줄 간결한 형태를 반환합니다.
func (p Provider) ForDisplay() string {
	if p.Hostname == DefaultRegistryHost {
		if p.Namespace == "hashicorp" {
			return fmt.Sprintf("hashicorp/%s", p.Type)
		}
		return fmt.Sprintf("%s/%s", p.Namespace, p.Type)
	}
	return p.String()
}

// IsBuiltIn는 내장 프로바이더인지 확인합니다.
func (p Provider) IsBuiltIn() bool {
	return p.Hostname == "builtin" && p.Namespace == "terraform"
}

// ─────────────────────────────────────────────────────────────────────────────
// 주소 파서
// ─────────────────────────────────────────────────────────────────────────────

// ParseError는 주소 파싱 오류입니다.
type ParseError struct {
	Input   string
	Message string
	Pos     int
}

func (e *ParseError) Error() string {
	if e.Pos >= 0 {
		return fmt.Sprintf("주소 파싱 오류 (위치 %d): %s\n  입력: %s\n  %s^",
			e.Pos, e.Message, e.Input, strings.Repeat(" ", e.Pos+8))
	}
	return fmt.Sprintf("주소 파싱 오류: %s (입력: %s)", e.Message, e.Input)
}

// AddressParser는 Terraform 주소 문자열을 파싱합니다.
type AddressParser struct{}

// ParseAbsResourceInstance는 절대 리소스 인스턴스 주소를 파싱합니다.
// 예: "module.network.aws_subnet.main[0]"
func (p *AddressParser) ParseAbsResourceInstance(input string) (*AbsResourceInstance, error) {
	remaining := input
	var moduleSteps ModuleInstance

	// 모듈 경로 파싱
	for strings.HasPrefix(remaining, "module.") {
		remaining = remaining[7:] // "module." 제거

		// 모듈 이름 읽기
		name, rest, err := p.readIdentifier(remaining)
		if err != nil {
			return nil, &ParseError{Input: input, Message: "모듈 이름이 필요합니다", Pos: len(input) - len(remaining)}
		}
		remaining = rest

		// 모듈 인스턴스 키 읽기
		key, rest2 := p.readInstanceKey(remaining)
		remaining = rest2

		moduleSteps = append(moduleSteps, ModuleInstanceStep{Name: name, Key: key})

		// 다음 "." 건너뛰기
		if strings.HasPrefix(remaining, ".") {
			remaining = remaining[1:]
		}
	}

	// 데이터 소스 처리
	mode := ManagedResourceMode
	if strings.HasPrefix(remaining, "data.") {
		mode = DataResourceMode
		remaining = remaining[5:] // "data." 제거
	}

	// 리소스 타입 읽기
	typeName, rest, err := p.readIdentifier(remaining)
	if err != nil {
		return nil, &ParseError{Input: input, Message: "리소스 타입이 필요합니다", Pos: len(input) - len(remaining)}
	}
	remaining = rest

	// "." 확인
	if !strings.HasPrefix(remaining, ".") {
		return nil, &ParseError{Input: input, Message: "'.' 구분자가 필요합니다", Pos: len(input) - len(remaining)}
	}
	remaining = remaining[1:]

	// 리소스 이름 읽기
	resName, rest2, err := p.readIdentifier(remaining)
	if err != nil {
		return nil, &ParseError{Input: input, Message: "리소스 이름이 필요합니다", Pos: len(input) - len(remaining)}
	}
	remaining = rest2

	// 인스턴스 키 읽기
	key, rest3 := p.readInstanceKey(remaining)
	remaining = rest3

	if remaining != "" {
		return nil, &ParseError{Input: input, Message: fmt.Sprintf("예상하지 못한 문자: '%s'", remaining), Pos: len(input) - len(remaining)}
	}

	return &AbsResourceInstance{
		Module: moduleSteps,
		Resource: ResourceInstance{
			Resource: Resource{
				Mode: mode,
				Type: typeName,
				Name: resName,
			},
			Key: key,
		},
	}, nil
}

// ParseAbsResource는 절대 리소스 주소를 파싱합니다 (인스턴스 키 없이).
func (p *AddressParser) ParseAbsResource(input string) (*AbsResource, error) {
	inst, err := p.ParseAbsResourceInstance(input)
	if err != nil {
		return nil, err
	}

	return &AbsResource{
		Module:   inst.Module,
		Resource: inst.Resource.Resource,
	}, nil
}

// ParseProvider는 프로바이더 주소를 파싱합니다.
// 형식: hostname/namespace/type 또는 namespace/type 또는 type
func (p *AddressParser) ParseProvider(input string) (*Provider, error) {
	parts := strings.Split(input, "/")

	switch len(parts) {
	case 1:
		// 타입만: "aws" → registry.terraform.io/hashicorp/aws
		if !isValidIdentifier(parts[0]) {
			return nil, &ParseError{Input: input, Message: "유효하지 않은 프로바이더 타입", Pos: -1}
		}
		prov := NewDefaultProvider(parts[0])
		return &prov, nil

	case 2:
		// namespace/type: "hashicorp/aws" → registry.terraform.io/hashicorp/aws
		if !isValidIdentifier(parts[0]) || !isValidIdentifier(parts[1]) {
			return nil, &ParseError{Input: input, Message: "유효하지 않은 프로바이더 namespace 또는 type", Pos: -1}
		}
		prov := NewProvider(DefaultRegistryHost, parts[0], parts[1])
		return &prov, nil

	case 3:
		// hostname/namespace/type: "registry.terraform.io/hashicorp/aws"
		prov := NewProvider(parts[0], parts[1], parts[2])
		return &prov, nil

	default:
		return nil, &ParseError{Input: input, Message: "프로바이더 주소 형식이 올바르지 않습니다", Pos: -1}
	}
}

// ParseModuleInstance는 모듈 인스턴스 경로를 파싱합니다.
func (p *AddressParser) ParseModuleInstance(input string) (ModuleInstance, error) {
	if input == "" {
		return nil, nil // 루트 모듈
	}

	remaining := input
	var steps ModuleInstance

	for remaining != "" {
		if !strings.HasPrefix(remaining, "module.") {
			return nil, &ParseError{Input: input, Message: "'module.' 접두사가 필요합니다", Pos: len(input) - len(remaining)}
		}
		remaining = remaining[7:]

		name, rest, err := p.readIdentifier(remaining)
		if err != nil {
			return nil, &ParseError{Input: input, Message: "모듈 이름이 필요합니다", Pos: len(input) - len(remaining)}
		}
		remaining = rest

		key, rest2 := p.readInstanceKey(remaining)
		remaining = rest2

		steps = append(steps, ModuleInstanceStep{Name: name, Key: key})

		if strings.HasPrefix(remaining, ".") {
			remaining = remaining[1:]
		}
	}

	return steps, nil
}

// readIdentifier는 식별자(identifier)를 읽습니다.
func (p *AddressParser) readIdentifier(input string) (string, string, error) {
	if len(input) == 0 {
		return "", "", fmt.Errorf("빈 입력")
	}

	if !unicode.IsLetter(rune(input[0])) && input[0] != '_' {
		return "", "", fmt.Errorf("식별자는 문자 또는 _로 시작해야 합니다")
	}

	i := 0
	for i < len(input) {
		ch := rune(input[i])
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_' || ch == '-' {
			i++
		} else {
			break
		}
	}

	return input[:i], input[i:], nil
}

// readInstanceKey는 인스턴스 키를 읽습니다.
func (p *AddressParser) readInstanceKey(input string) (InstanceKey, string) {
	if !strings.HasPrefix(input, "[") {
		return NoInstanceKey(), input
	}

	// "]" 찾기
	end := strings.Index(input, "]")
	if end < 0 {
		return NoInstanceKey(), input
	}

	keyStr := input[1:end]
	remaining := input[end+1:]

	// 정수 키 시도
	if intVal, err := strconv.Atoi(keyStr); err == nil {
		return IntInstanceKey(intVal), remaining
	}

	// 문자열 키 시도 ("key" 형태)
	if len(keyStr) >= 2 && keyStr[0] == '"' && keyStr[len(keyStr)-1] == '"' {
		return StringInstanceKey(keyStr[1 : len(keyStr)-1]), remaining
	}

	return NoInstanceKey(), input
}

func isValidIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, ch := range s {
		if i == 0 {
			if !unicode.IsLetter(ch) && ch != '_' {
				return false
			}
		} else {
			if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && ch != '_' && ch != '-' {
				return false
			}
		}
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────────────
// 헬퍼 함수
// ─────────────────────────────────────────────────────────────────────────────

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
}

func printSubSection(title string) {
	fmt.Printf("\n--- %s ---\n\n", title)
}

// ─────────────────────────────────────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Terraform 주소(Address) 파싱 시뮬레이션                    ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  리소스, 모듈, 프로바이더 주소를 파싱하고 구조화합니다              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	parser := &AddressParser{}

	// ─── 1. 단순 리소스 주소 파싱 ───
	printSeparator("1. 단순 리소스 주소 파싱")
	simpleAddresses := []string{
		"aws_instance.web",
		"aws_vpc.main",
		"google_compute_instance.default",
		"null_resource.provisioner",
	}

	for _, addr := range simpleAddresses {
		result, err := parser.ParseAbsResourceInstance(addr)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		fmt.Printf("  입력: %-40s\n", addr)
		fmt.Printf("    모듈:     %s\n", result.Module.String())
		fmt.Printf("    모드:     %s\n", result.Resource.Resource.Mode.String())
		fmt.Printf("    타입:     %s\n", result.Resource.Resource.Type)
		fmt.Printf("    이름:     %s\n", result.Resource.Resource.Name)
		fmt.Printf("    키:       %s\n", result.Resource.Key.String())
		fmt.Printf("    정규화:   %s\n", result.String())
		fmt.Println()
	}

	// ─── 2. 인스턴스 키가 있는 주소 파싱 ───
	printSeparator("2. 인스턴스 키가 있는 주소 (count/for_each)")
	indexedAddresses := []string{
		"aws_instance.web[0]",
		"aws_instance.web[3]",
		`aws_instance.web["us-east-1"]`,
		`aws_subnet.private["database"]`,
		"aws_iam_user.users[0]",
	}

	for _, addr := range indexedAddresses {
		result, err := parser.ParseAbsResourceInstance(addr)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		fmt.Printf("  입력: %-45s\n", addr)
		keyType := "없음"
		keyVal := "N/A"
		switch result.Resource.Key.Type {
		case IntKey:
			keyType = "IntKey (count)"
			keyVal = fmt.Sprintf("%d", result.Resource.Key.IntVal)
		case StringKey:
			keyType = "StringKey (for_each)"
			keyVal = result.Resource.Key.StrVal
		}
		fmt.Printf("    키 유형:  %s\n", keyType)
		fmt.Printf("    키 값:    %s\n", keyVal)
		fmt.Printf("    정규화:   %s\n", result.String())
		fmt.Println()
	}

	// ─── 3. 데이터 소스 주소 파싱 ───
	printSeparator("3. 데이터 소스 주소 파싱")
	dataAddresses := []string{
		"data.aws_ami.latest",
		"data.aws_availability_zones.available",
		"data.terraform_remote_state.vpc",
	}

	for _, addr := range dataAddresses {
		result, err := parser.ParseAbsResourceInstance(addr)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		fmt.Printf("  입력: %-45s\n", addr)
		fmt.Printf("    모드:     %s (data 블록)\n", result.Resource.Resource.Mode.String())
		fmt.Printf("    타입:     %s\n", result.Resource.Resource.Type)
		fmt.Printf("    이름:     %s\n", result.Resource.Resource.Name)
		fmt.Printf("    정규화:   %s\n", result.String())
		fmt.Println()
	}

	// ─── 4. 모듈 리소스 주소 파싱 ───
	printSeparator("4. 모듈 내 리소스 주소 파싱")
	moduleAddresses := []string{
		"module.network.aws_subnet.main",
		"module.network.aws_subnet.main[0]",
		`module.network.aws_subnet.main["private"]`,
		"module.vpc.module.subnets.aws_subnet.this[0]",
		"module.app.data.aws_ami.latest",
	}

	for _, addr := range moduleAddresses {
		result, err := parser.ParseAbsResourceInstance(addr)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		fmt.Printf("  입력: %-55s\n", addr)
		fmt.Printf("    모듈 경로: %s\n", result.Module.String())
		fmt.Printf("    모듈 깊이: %d\n", len(result.Module))
		for i, step := range result.Module {
			fmt.Printf("      [%d] %s\n", i, step.String())
		}
		fmt.Printf("    리소스:    %s\n", result.Resource.String())
		fmt.Printf("    정규화:    %s\n", result.String())
		fmt.Println()
	}

	// ─── 5. 모듈 인스턴스 경로 파싱 ───
	printSeparator("5. 모듈 인스턴스 경로 파싱")
	modulePaths := []string{
		"module.network",
		"module.network.module.subnets",
		"module.services[0]",
		`module.services["web"]`,
		`module.region["us-east-1"].module.vpc`,
	}

	for _, path := range modulePaths {
		result, err := parser.ParseModuleInstance(path)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		fmt.Printf("  입력: %-50s\n", path)
		fmt.Printf("    정규화: %s\n", result.String())
		fmt.Printf("    루트?:  %v\n", result.IsRoot())
		fmt.Printf("    깊이:   %d\n", len(result))
		if len(result) > 0 {
			parent := result.Parent()
			fmt.Printf("    부모:   %s\n", parent.String())
		}
		fmt.Println()
	}

	// ─── 6. 프로바이더 주소 파싱 ───
	printSeparator("6. 프로바이더 주소 파싱")
	providerAddresses := []string{
		"aws",
		"hashicorp/aws",
		"hashicorp/google",
		"registry.terraform.io/hashicorp/aws",
		"registry.terraform.io/hashicorp/azurerm",
		"registry.example.com/myorg/custom",
	}

	for _, addr := range providerAddresses {
		result, err := parser.ParseProvider(addr)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		fmt.Printf("  입력: %-45s\n", addr)
		fmt.Printf("    호스트:     %s\n", result.Hostname)
		fmt.Printf("    네임스페이스: %s\n", result.Namespace)
		fmt.Printf("    타입:       %s\n", result.Type)
		fmt.Printf("    정규화:     %s\n", result.String())
		fmt.Printf("    표시용:     %s\n", result.ForDisplay())
		fmt.Printf("    내장?:      %v\n", result.IsBuiltIn())
		fmt.Println()
	}

	// ─── 7. 내장 프로바이더 ───
	printSeparator("7. 내장(builtin) 프로바이더")
	builtinProvider := NewProvider("builtin", "terraform", "terraform")
	fmt.Printf("  프로바이더: %s\n", builtinProvider.String())
	fmt.Printf("  내장?: %v\n", builtinProvider.IsBuiltIn())

	// ─── 8. 주소 구성 (프로그래밍적 생성) ───
	printSeparator("8. 주소 프로그래밍적 구성")

	// 모듈 경로 구성
	root := ModuleInstance(nil)
	network := root.Child("network", NoInstanceKey())
	subnets := network.Child("subnets", NoInstanceKey())
	fmt.Printf("  루트 → network → subnets: %s\n", subnets.String())

	// for_each 모듈
	services := root.Child("services", StringInstanceKey("web"))
	fmt.Printf("  services[\"web\"]: %s\n", services.String())

	// 절대 리소스 주소 구성
	absRes := AbsResourceInstance{
		Module: network,
		Resource: ResourceInstance{
			Resource: Resource{
				Mode: ManagedResourceMode,
				Type: "aws_subnet",
				Name: "private",
			},
			Key: IntInstanceKey(2),
		},
	}
	fmt.Printf("  절대 주소: %s\n", absRes.String())
	fmt.Printf("  포함 리소스: %s\n", absRes.ContainingResource().String())

	// ─── 9. 파싱 오류 처리 ───
	printSeparator("9. 파싱 오류 처리")
	errorCases := []string{
		"",
		"123invalid",
		"aws_instance",
		"module.",
		"module.network.",
	}

	for _, addr := range errorCases {
		_, err := parser.ParseAbsResourceInstance(addr)
		if err != nil {
			fmt.Printf("  입력: %-30s → 오류: %s\n", fmt.Sprintf("%q", addr), err.Error())
		} else {
			fmt.Printf("  입력: %-30s → 성공 (예상치 못함)\n", fmt.Sprintf("%q", addr))
		}
		fmt.Println()
	}

	// ─── 아키텍처 요약 ───
	printSeparator("주소 체계 아키텍처 요약")
	fmt.Print(`
  Terraform 주소 체계:

  ┌─────────────────────────────────────────────────────────────┐
  │                     AbsResourceInstance                     │
  │  module.network.aws_subnet.private[2]                      │
  │  ├── ModuleInstance: [module.network]                       │
  │  └── ResourceInstance                                       │
  │      ├── Resource                                           │
  │      │   ├── Mode: managed                                  │
  │      │   ├── Type: aws_subnet                               │
  │      │   └── Name: private                                  │
  │      └── InstanceKey: IntKey(2)                             │
  └─────────────────────────────────────────────────────────────┘

  ModuleInstance (경로):
    (root) → module.network → module.network.module.subnets

  InstanceKey 종류:
    NoKey     → aws_instance.web         (count/for_each 없음)
    IntKey    → aws_instance.web[0]      (count 사용)
    StringKey → aws_instance.web["key"]  (for_each 사용)

  Provider 주소:
    단축형: aws
    표준형: hashicorp/aws
    정규형: registry.terraform.io/hashicorp/aws

  리소스 모드:
    managed → resource "aws_instance" "web" { ... }
    data    → data "aws_ami" "latest" { ... }`)
}
