// Package main은 Terraform의 Import 기능과 설정 자동 생성(genconfig) 시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. 리소스 스키마 정의 (Block, Attribute, NestedBlock)
// 2. Import 대상 파싱 (리소스 주소 + ID)
// 3. Provider의 ImportResourceState 시뮬레이션
// 4. 상태값에서 HCL 설정 자동 생성 (genconfig)
// 5. 속성 필터링 (Computed, Deprecated, Sensitive)
// 6. 중첩 블록 처리 (Single, List, Map)
// 7. JSON 값의 jsonencode() 래핑
// 8. Import 블록의 for_each 지원
// 9. 생성된 설정 파일 출력
// 10. 상태값 필터링 (ExtractLegacyConfigFromState)
//
// 실제 소스 참조:
//   - internal/command/import.go          (CLI import 명령)
//   - internal/configs/import.go          (import 블록 디코딩)
//   - internal/genconfig/generate_config.go  (설정 생성 핵심)
//   - internal/genconfig/generate_config_write.go (파일 출력)
//   - internal/terraform/context_import.go (Core import 실행)
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ============================================================================
// 1. 스키마 정의 (configschema 시뮬레이션)
// ============================================================================

// AttributeType은 속성의 데이터 타입을 나타낸다.
type AttributeType int

const (
	TypeString AttributeType = iota
	TypeNumber
	TypeBool
	TypeList
	TypeMap
)

func (t AttributeType) FriendlyName() string {
	switch t {
	case TypeString:
		return "string"
	case TypeNumber:
		return "number"
	case TypeBool:
		return "bool"
	case TypeList:
		return "list"
	case TypeMap:
		return "map"
	default:
		return "unknown"
	}
}

// Attribute는 리소스 속성의 스키마를 정의한다.
// 실제 코드: internal/configs/configschema/schema.go
type Attribute struct {
	Type       AttributeType
	Required   bool
	Optional   bool
	Computed   bool
	Sensitive  bool
	Deprecated bool
}

// NestingMode는 블록의 중첩 방식을 나타낸다.
type NestingMode int

const (
	NestingSingle NestingMode = iota
	NestingList
	NestingSet
	NestingMap
	NestingGroup
)

// NestedBlock은 중첩 블록의 스키마를 정의한다.
type NestedBlock struct {
	Nesting    NestingMode
	Attributes map[string]*Attribute
	MinItems   int
}

// Block은 리소스의 전체 스키마를 정의한다.
type Block struct {
	Attributes map[string]*Attribute
	BlockTypes map[string]*NestedBlock
}

// ============================================================================
// 2. 리소스 주소 및 Import 대상
// ============================================================================

// ResourceAddr은 Terraform 리소스의 주소를 나타낸다.
// 실제 코드: internal/addrs/resource.go
type ResourceAddr struct {
	Module string // 모듈 경로 (빈 문자열 = 루트)
	Type   string // 리소스 타입 (예: aws_instance)
	Name   string // 리소스 이름 (예: web)
	Key    string // 인스턴스 키 (예: [0], ["prod"])
}

func (r ResourceAddr) String() string {
	var buf strings.Builder
	if r.Module != "" {
		buf.WriteString("module.")
		buf.WriteString(r.Module)
		buf.WriteString(".")
	}
	buf.WriteString(r.Type)
	buf.WriteString(".")
	buf.WriteString(r.Name)
	if r.Key != "" {
		buf.WriteString("[")
		buf.WriteString(r.Key)
		buf.WriteString("]")
	}
	return buf.String()
}

// ImpliedProvider는 리소스 타입에서 추론된 Provider 이름을 반환한다.
func (r ResourceAddr) ImpliedProvider() string {
	parts := strings.SplitN(r.Type, "_", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return r.Type
}

// ImportTarget은 단일 import 대상을 나타낸다.
// 실제 코드: internal/terraform/context_import.go
type ImportTarget struct {
	Addr ResourceAddr // 대상 리소스 주소
	ID   string       // Provider별 리소스 식별자
}

// ImportBlock은 HCL import 블록을 나타낸다.
// 실제 코드: internal/configs/import.go
type ImportBlock struct {
	To       ResourceAddr
	ID       string
	ForEach  map[string]string // for_each 지원
	Provider string            // Provider 설정 참조
}

// ============================================================================
// 3. Provider Import 시뮬레이션
// ============================================================================

// ImportedResource는 Provider가 반환하는 import 결과이다.
type ImportedResource struct {
	TypeName string
	State    map[string]interface{}
}

// ProviderImporter는 Provider의 ImportResourceState를 시뮬레이션한다.
type ProviderImporter struct {
	// resources는 "존재하는 인프라"를 시뮬레이션한다
	resources map[string]map[string]interface{}
}

func NewProviderImporter() *ProviderImporter {
	return &ProviderImporter{
		resources: map[string]map[string]interface{}{
			// 시뮬레이션 인프라: AWS EC2 인스턴스
			"i-1234567890abcdef0": {
				"id":                 "i-1234567890abcdef0",
				"ami":                "ami-0abcdef1234567890",
				"instance_type":      "t3.micro",
				"subnet_id":          "subnet-abc123",
				"availability_zone":  "us-west-2a",
				"private_ip":         "10.0.1.42",
				"public_ip":          "54.123.45.67",
				"arn":                "arn:aws:ec2:us-west-2:123456789012:instance/i-1234567890abcdef0",
				"cpu_core_count":     2,
				"key_name":           "my-keypair",
				"monitoring":         true,
				"security_groups":    []string{"sg-12345", "sg-67890"},
				"user_data":          "",
				"metadata_options":   map[string]interface{}{"http_endpoint": "enabled", "http_tokens": "required"},
				"tags":               map[string]interface{}{"Name": "web-server", "Environment": "production"},
				"tags_all":           map[string]interface{}{"Name": "web-server", "Environment": "production"},
				"ebs_optimized":      false,
				"source_dest_check":  true,
				"old_deprecated_field": "deprecated_value",
				"policy_json":        `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`,
			},
			// 시뮬레이션 인프라: S3 버킷
			"my-data-bucket": {
				"id":                "my-data-bucket",
				"bucket":            "my-data-bucket",
				"bucket_domain_name": "my-data-bucket.s3.amazonaws.com",
				"region":            "us-west-2",
				"acl":               "private",
				"versioning":        []interface{}{map[string]interface{}{"enabled": true, "mfa_delete": false}},
				"tags":              map[string]interface{}{"Team": "data-engineering"},
			},
		},
	}
}

// ImportResourceState는 ID로 리소스를 조회하여 상태를 반환한다.
func (p *ProviderImporter) ImportResourceState(resourceType, id string) (*ImportedResource, error) {
	state, ok := p.resources[id]
	if !ok {
		return nil, fmt.Errorf("리소스를 찾을 수 없습니다: %s (ID: %s)", resourceType, id)
	}
	return &ImportedResource{
		TypeName: resourceType,
		State:    state,
	}, nil
}

// ============================================================================
// 4. 설정 생성 엔진 (genconfig)
// ============================================================================

// GenerateResourceConfig는 리소스의 상태값과 스키마로 HCL 설정을 생성한다.
// 실제 코드: internal/genconfig/generate_config.go GenerateResourceContents
func GenerateResourceConfig(
	addr ResourceAddr,
	schema *Block,
	providerName string,
	state map[string]interface{},
) string {
	var buf strings.Builder

	// Provider 주소 생성 (필요한 경우)
	if providerName != "" && providerName != addr.ImpliedProvider() {
		buf.WriteString(fmt.Sprintf("  provider = %s\n", providerName))
	}

	// 속성 생성
	writeAttributesFromState(&buf, schema.Attributes, state, 2)

	// 블록 생성
	writeBlocksFromState(&buf, schema.BlockTypes, state, 2)

	return buf.String()
}

// writeAttributesFromState는 상태값에서 속성을 HCL로 변환한다.
// 실제 코드: internal/genconfig/generate_config.go writeConfigAttributesFromExisting
func writeAttributesFromState(buf *strings.Builder, attrs map[string]*Attribute, state map[string]interface{}, indent int) {
	// 속성명 정렬 (일관된 출력 보장)
	var names []string
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		attrS := attrs[name]
		val, exists := state[name]

		// Computed이면서 값이 없는 속성은 건너뛰기
		if attrS.Computed && (!exists || val == nil) {
			continue
		}

		// Deprecated 속성은 건너뛰기
		if attrS.Deprecated {
			continue
		}

		// Sensitive 속성은 null로 표시
		if attrS.Sensitive {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = null # sensitive\n", name))
			continue
		}

		// 값이 없으면 빈 값 생성 (스키마 기반)
		if !exists || val == nil {
			if attrS.Required {
				buf.WriteString(strings.Repeat(" ", indent))
				buf.WriteString(fmt.Sprintf("%s = null", name))
				writeAttrConstraint(buf, attrS)
				buf.WriteString("\n")
			}
			continue
		}

		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(fmt.Sprintf("%s = ", name))

		// JSON 문자열 감지 및 jsonencode() 래핑
		if strVal, ok := val.(string); ok && isValidJSON(strVal) && !isPrimitive(strVal) {
			buf.WriteString("jsonencode(")
			writeHCLValue(buf, parseJSON(strVal))
			buf.WriteString(")")
		} else {
			writeHCLValue(buf, val)
		}

		buf.WriteString("\n")
	}
}

// writeBlocksFromState는 상태값에서 블록을 HCL로 변환한다.
// 실제 코드: internal/genconfig/generate_config.go writeConfigBlocksFromExisting
func writeBlocksFromState(buf *strings.Builder, blocks map[string]*NestedBlock, state map[string]interface{}, indent int) {
	if len(blocks) == 0 {
		return
	}

	var names []string
	for name := range blocks {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		blockS := blocks[name]
		val, exists := state[name]
		if !exists || val == nil {
			continue
		}

		switch blockS.Nesting {
		case NestingSingle, NestingGroup:
			// 단일 블록
			if m, ok := val.(map[string]interface{}); ok {
				buf.WriteString(strings.Repeat(" ", indent))
				buf.WriteString(fmt.Sprintf("%s {\n", name))
				writeAttributesFromState(buf, blockS.Attributes, m, indent+2)
				buf.WriteString(strings.Repeat(" ", indent))
				buf.WriteString("}\n")
			}

		case NestingList, NestingSet:
			// 리스트 블록 - 각 요소마다 블록 반복
			if list, ok := val.([]interface{}); ok {
				for _, item := range list {
					if m, ok := item.(map[string]interface{}); ok {
						buf.WriteString(strings.Repeat(" ", indent))
						buf.WriteString(fmt.Sprintf("%s {\n", name))
						writeAttributesFromState(buf, blockS.Attributes, m, indent+2)
						buf.WriteString(strings.Repeat(" ", indent))
						buf.WriteString("}\n")
					}
				}
			}

		case NestingMap:
			// 맵 블록 - 키별 블록
			if m, ok := val.(map[string]interface{}); ok {
				var keys []string
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, key := range keys {
					if inner, ok := m[key].(map[string]interface{}); ok {
						buf.WriteString(strings.Repeat(" ", indent))
						buf.WriteString(fmt.Sprintf("%s %q {\n", name, key))
						writeAttributesFromState(buf, blockS.Attributes, inner, indent+2)
						buf.WriteString(strings.Repeat(" ", indent))
						buf.WriteString("}\n")
					}
				}
			}
		}
	}
}

// ============================================================================
// 5. 상태값 필터링
// ============================================================================

// ExtractLegacyConfigFromState는 상태값을 설정에 적합한 형태로 필터링한다.
// 실제 코드: internal/genconfig/generate_config.go ExtractLegacyConfigFromState
func ExtractLegacyConfigFromState(schema *Block, state map[string]interface{}) map[string]interface{} {
	filtered := make(map[string]interface{})

	for name, val := range state {
		attrS, isAttr := schema.Attributes[name]

		if isAttr {
			// deprecated 속성 건너뛰기
			if attrS.Deprecated {
				continue
			}
			// read-only (Computed && !Optional) 건너뛰기
			if attrS.Computed && !attrS.Optional {
				continue
			}
			// Legacy SDK의 Optional+Computed "id" 속성 건너뛰기
			if name == "id" && attrS.Computed && attrS.Optional {
				continue
			}
			// Optional이면서 빈 문자열 건너뛰기 (Legacy SDK)
			if str, ok := val.(string); ok && attrS.Optional && len(str) == 0 {
				continue
			}
			filtered[name] = val
		} else {
			// 블록 타입이면 그대로 복사
			filtered[name] = val
		}
	}

	return filtered
}

// ============================================================================
// 6. HCL 값 포맷팅 유틸리티
// ============================================================================

// writeHCLValue는 Go 값을 HCL 형식으로 출력한다.
func writeHCLValue(buf *strings.Builder, val interface{}) {
	switch v := val.(type) {
	case string:
		buf.WriteString(fmt.Sprintf("%q", v))
	case int:
		buf.WriteString(fmt.Sprintf("%d", v))
	case float64:
		if v == float64(int(v)) {
			buf.WriteString(fmt.Sprintf("%d", int(v)))
		} else {
			buf.WriteString(fmt.Sprintf("%g", v))
		}
	case bool:
		buf.WriteString(fmt.Sprintf("%t", v))
	case []string:
		buf.WriteString("[")
		for i, s := range v {
			if i > 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(fmt.Sprintf("%q", s))
		}
		buf.WriteString("]")
	case []interface{}:
		buf.WriteString("[")
		for i, item := range v {
			if i > 0 {
				buf.WriteString(", ")
			}
			writeHCLValue(buf, item)
		}
		buf.WriteString("]")
	case map[string]interface{}:
		buf.WriteString("{\n")
		var keys []string
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			buf.WriteString("    ")
			buf.WriteString(fmt.Sprintf("%s = ", k))
			writeHCLValue(buf, v[k])
			buf.WriteString("\n")
		}
		buf.WriteString("  }")
	case nil:
		buf.WriteString("null")
	default:
		buf.WriteString(fmt.Sprintf("%v", v))
	}
}

// writeAttrConstraint는 속성 타입 제약 조건 주석을 출력한다.
func writeAttrConstraint(buf *strings.Builder, attr *Attribute) {
	if attr.Required {
		buf.WriteString(fmt.Sprintf(" # REQUIRED %s", attr.Type.FriendlyName()))
	} else if attr.Optional {
		buf.WriteString(fmt.Sprintf(" # OPTIONAL %s", attr.Type.FriendlyName()))
	}
}

func isValidJSON(s string) bool {
	return json.Valid([]byte(s))
}

func isPrimitive(s string) bool {
	// JSON 원시 값 (문자열, 숫자, bool, null) 확인
	s = strings.TrimSpace(s)
	if s == "true" || s == "false" || s == "null" {
		return true
	}
	// 숫자 확인
	var n json.Number
	if err := json.Unmarshal([]byte(s), &n); err == nil {
		return true
	}
	// 따옴표로 둘러싸인 문자열
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return true
	}
	return false
}

func parseJSON(s string) interface{} {
	var result interface{}
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return s
	}
	return result
}

// ============================================================================
// 7. 파일 출력
// ============================================================================

// Change는 생성된 설정의 단일 변경사항을 나타낸다.
// 실제 코드: internal/genconfig/generate_config_write.go
type Change struct {
	Addr            string
	ImportID        string
	GeneratedConfig string
}

// GenerateFile은 변경사항들을 하나의 파일 내용으로 조합한다.
func GenerateFile(changes []Change) string {
	var buf strings.Builder

	// 파일 헤더
	buf.WriteString("# __generated__ by Terraform\n")
	buf.WriteString("# Please review these resources and move them into your main configuration files.\n")

	for _, c := range changes {
		if len(c.GeneratedConfig) == 0 {
			continue
		}
		buf.WriteString("\n# __generated__ by Terraform")
		if len(c.ImportID) > 0 {
			buf.WriteString(fmt.Sprintf(" from %q", c.ImportID))
		}
		buf.WriteString("\n")
		buf.WriteString(c.GeneratedConfig)
		buf.WriteString("\n")
	}

	return buf.String()
}

// ValidateTargetFile은 대상 파일이 이미 존재하는지 확인한다.
// 실제 코드: internal/genconfig/generate_config_write.go
func ValidateTargetFile(path string) error {
	// 새 파일에만 쓸 수 있다는 규칙 시뮬레이션
	if path == "" {
		return fmt.Errorf("출력 파일 경로가 지정되지 않았습니다")
	}
	return nil
}

// ============================================================================
// 8. Import 실행 엔진
// ============================================================================

// ResourceConfig는 기존 설정에 존재하는 리소스를 나타낸다.
type ResourceConfig struct {
	Type string
	Name string
}

// ImportExecutor는 import 실행 엔진을 시뮬레이션한다.
type ImportExecutor struct {
	provider       *ProviderImporter
	schemas        map[string]*Block
	existingConfig map[string]*ResourceConfig // "type.name" -> config
}

func NewImportExecutor() *ImportExecutor {
	return &ImportExecutor{
		provider:       NewProviderImporter(),
		schemas:        buildSchemas(),
		existingConfig: make(map[string]*ResourceConfig),
	}
}

// AddExistingConfig는 기존 설정에 리소스를 등록한다.
func (e *ImportExecutor) AddExistingConfig(resType, name string) {
	key := resType + "." + name
	e.existingConfig[key] = &ResourceConfig{Type: resType, Name: name}
}

// ExecuteCLIImport는 CLI import를 시뮬레이션한다.
// 실제 코드: internal/command/import.go ImportCommand.Run
func (e *ImportExecutor) ExecuteCLIImport(addrStr, id string) (string, error) {
	// 1. 리소스 주소 파싱
	addr, err := parseResourceAddr(addrStr)
	if err != nil {
		return "", fmt.Errorf("잘못된 리소스 주소: %s", err)
	}

	// 2. 설정에 리소스가 있는지 확인 (CLI import의 핵심 요구사항)
	configKey := addr.Type + "." + addr.Name
	if _, exists := e.existingConfig[configKey]; !exists {
		return "", fmt.Errorf(
			"리소스 주소 %q가 설정에 존재하지 않습니다.\n\n"+
				"이 리소스를 import하기 전에, 설정에 리소스를 추가하세요:\n\n"+
				"resource %q %q {\n  # (리소스 인수)\n}",
			addr.String(), addr.Type, addr.Name,
		)
	}

	// 3. Provider에 ImportResourceState 호출
	imported, err := e.provider.ImportResourceState(addr.Type, id)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Import 성공! %s (ID: %s)\n상태에 %d개 속성 추가됨",
		addr.String(), id, len(imported.State)), nil
}

// ExecuteDeclarativeImport는 선언적 import + genconfig을 시뮬레이션한다.
func (e *ImportExecutor) ExecuteDeclarativeImport(blocks []ImportBlock, generateConfig bool) ([]Change, error) {
	var changes []Change

	for _, block := range blocks {
		// for_each 확장
		targets := expandImportBlock(block)

		for _, target := range targets {
			// Provider에서 리소스 상태 가져오기
			imported, err := e.provider.ImportResourceState(target.Addr.Type, target.ID)
			if err != nil {
				return nil, fmt.Errorf("%s import 실패: %v", target.Addr.String(), err)
			}

			var generatedConfig string
			if generateConfig {
				// 스키마 조회
				schema, ok := e.schemas[target.Addr.Type]
				if !ok {
					return nil, fmt.Errorf("스키마를 찾을 수 없습니다: %s", target.Addr.Type)
				}

				// 상태값 필터링
				filteredState := ExtractLegacyConfigFromState(schema, imported.State)

				// HCL 설정 생성
				body := GenerateResourceConfig(target.Addr, schema, "", filteredState)
				generatedConfig = fmt.Sprintf("resource %q %q {\n%s}\n",
					target.Addr.Type, target.Addr.Name, body)
			}

			changes = append(changes, Change{
				Addr:            target.Addr.String(),
				ImportID:        target.ID,
				GeneratedConfig: generatedConfig,
			})
		}
	}

	return changes, nil
}

// expandImportBlock은 for_each가 있는 import 블록을 여러 ImportTarget으로 확장한다.
func expandImportBlock(block ImportBlock) []ImportTarget {
	if len(block.ForEach) == 0 {
		return []ImportTarget{
			{Addr: block.To, ID: block.ID},
		}
	}

	var targets []ImportTarget
	var keys []string
	for k := range block.ForEach {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		id := block.ForEach[key]
		addr := block.To
		addr.Key = fmt.Sprintf("%q", key)
		targets = append(targets, ImportTarget{
			Addr: addr,
			ID:   id,
		})
	}
	return targets
}

// ============================================================================
// 유틸리티
// ============================================================================

// parseResourceAddr는 문자열에서 ResourceAddr을 파싱한다.
func parseResourceAddr(s string) (ResourceAddr, error) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return ResourceAddr{}, fmt.Errorf("최소 type.name 형식이어야 합니다: %s", s)
	}

	// module.xxx.type.name 형식 처리
	if parts[0] == "module" && len(parts) >= 4 {
		return ResourceAddr{
			Module: parts[1],
			Type:   parts[2],
			Name:   parts[3],
		}, nil
	}

	return ResourceAddr{
		Type: parts[0],
		Name: parts[1],
	}, nil
}

// buildSchemas는 테스트용 리소스 스키마를 구축한다.
func buildSchemas() map[string]*Block {
	return map[string]*Block{
		"aws_instance": {
			Attributes: map[string]*Attribute{
				"ami":               {Type: TypeString, Required: true},
				"instance_type":     {Type: TypeString, Required: true},
				"subnet_id":         {Type: TypeString, Optional: true},
				"availability_zone": {Type: TypeString, Optional: true, Computed: true},
				"key_name":          {Type: TypeString, Optional: true},
				"monitoring":        {Type: TypeBool, Optional: true},
				"security_groups":   {Type: TypeList, Optional: true},
				"tags":              {Type: TypeMap, Optional: true},
				"tags_all":          {Type: TypeMap, Optional: true, Computed: true},
				"ebs_optimized":     {Type: TypeBool, Optional: true},
				"source_dest_check": {Type: TypeBool, Optional: true},
				"user_data":         {Type: TypeString, Optional: true},
				"policy_json":       {Type: TypeString, Optional: true},
				// Computed-only (read-only) 속성
				"id":              {Type: TypeString, Computed: true, Optional: true},
				"arn":             {Type: TypeString, Computed: true},
				"private_ip":     {Type: TypeString, Computed: true},
				"public_ip":      {Type: TypeString, Computed: true},
				"cpu_core_count": {Type: TypeNumber, Computed: true},
				// Deprecated 속성
				"old_deprecated_field": {Type: TypeString, Deprecated: true},
			},
			BlockTypes: map[string]*NestedBlock{
				"metadata_options": {
					Nesting: NestingSingle,
					Attributes: map[string]*Attribute{
						"http_endpoint": {Type: TypeString, Optional: true},
						"http_tokens":   {Type: TypeString, Optional: true},
					},
				},
			},
		},
		"aws_s3_bucket": {
			Attributes: map[string]*Attribute{
				"bucket":             {Type: TypeString, Required: true},
				"acl":                {Type: TypeString, Optional: true},
				"tags":               {Type: TypeMap, Optional: true},
				"id":                 {Type: TypeString, Computed: true, Optional: true},
				"bucket_domain_name": {Type: TypeString, Computed: true},
				"region":             {Type: TypeString, Computed: true},
			},
			BlockTypes: map[string]*NestedBlock{
				"versioning": {
					Nesting: NestingList,
					Attributes: map[string]*Attribute{
						"enabled":    {Type: TypeBool, Optional: true},
						"mfa_delete": {Type: TypeBool, Optional: true},
					},
				},
			},
		},
	}
}

// ============================================================================
// 메인: 시뮬레이션 실행
// ============================================================================

func main() {
	fmt.Println("=== Terraform Import & 설정 생성 (genconfig) 시뮬레이션 ===")
	fmt.Println()

	executor := NewImportExecutor()

	// --- 1단계: CLI Import (설정 없이 시도 → 실패) ---
	fmt.Println("--- 1단계: CLI Import - 설정 없이 시도 ---")

	result, err := executor.ExecuteCLIImport("aws_instance.web", "i-1234567890abcdef0")
	if err != nil {
		fmt.Printf("  [에러] %s\n", err)
	} else {
		fmt.Printf("  [성공] %s\n", result)
	}
	fmt.Println()

	// --- 2단계: CLI Import (설정 추가 후 → 성공) ---
	fmt.Println("--- 2단계: CLI Import - 설정 추가 후 시도 ---")

	executor.AddExistingConfig("aws_instance", "web")
	result, err = executor.ExecuteCLIImport("aws_instance.web", "i-1234567890abcdef0")
	if err != nil {
		fmt.Printf("  [에러] %s\n", err)
	} else {
		fmt.Printf("  [성공] %s\n", result)
	}
	fmt.Println()

	// --- 3단계: 선언적 Import + 설정 자동 생성 ---
	fmt.Println("--- 3단계: 선언적 Import + 설정 자동 생성 ---")

	importBlocks := []ImportBlock{
		{
			To: ResourceAddr{Type: "aws_instance", Name: "web"},
			ID: "i-1234567890abcdef0",
		},
		{
			To: ResourceAddr{Type: "aws_s3_bucket", Name: "data"},
			ID: "my-data-bucket",
		},
	}

	changes, err := executor.ExecuteDeclarativeImport(importBlocks, true)
	if err != nil {
		fmt.Printf("  [에러] %s\n", err)
		return
	}

	fileContent := GenerateFile(changes)
	fmt.Println("  === 생성된 설정 파일 (generated.tf) ===")
	fmt.Println(fileContent)

	// --- 4단계: 상태값 필터링 데모 ---
	fmt.Println("--- 4단계: 상태값 필터링 (ExtractLegacyConfigFromState) ---")

	schema := executor.schemas["aws_instance"]
	fullState := map[string]interface{}{
		"id":                   "i-1234567890abcdef0",
		"ami":                  "ami-0abcdef1234567890",
		"instance_type":        "t3.micro",
		"arn":                  "arn:aws:ec2:...",           // Computed-only
		"private_ip":           "10.0.1.42",                // Computed-only
		"cpu_core_count":       2,                           // Computed-only
		"old_deprecated_field": "deprecated_value",          // Deprecated
		"user_data":            "",                           // Optional 빈 문자열
	}

	filteredState := ExtractLegacyConfigFromState(schema, fullState)

	fmt.Println("  원본 상태 속성:")
	var fullKeys []string
	for k := range fullState {
		fullKeys = append(fullKeys, k)
	}
	sort.Strings(fullKeys)
	for _, k := range fullKeys {
		fmt.Printf("    %-25s = %v\n", k, fullState[k])
	}

	fmt.Println("\n  필터링 후 (설정에 적합한 값만):")
	var filteredKeys []string
	for k := range filteredState {
		filteredKeys = append(filteredKeys, k)
	}
	sort.Strings(filteredKeys)
	for _, k := range filteredKeys {
		fmt.Printf("    %-25s = %v\n", k, filteredState[k])
	}

	fmt.Println("\n  제거된 속성:")
	for _, k := range fullKeys {
		if _, exists := filteredState[k]; !exists {
			reason := "알 수 없음"
			if attr, ok := schema.Attributes[k]; ok {
				if attr.Deprecated {
					reason = "Deprecated"
				} else if attr.Computed && !attr.Optional {
					reason = "Computed-only (read-only)"
				} else if k == "id" && attr.Computed && attr.Optional {
					reason = "Legacy SDK id 속성"
				} else if str, isStr := fullState[k].(string); isStr && attr.Optional && len(str) == 0 {
					reason = "Optional 빈 문자열"
				}
			}
			fmt.Printf("    %-25s → %s\n", k, reason)
		}
	}
	fmt.Println()

	// --- 5단계: for_each Import ---
	fmt.Println("--- 5단계: for_each Import ---")

	forEachBlock := ImportBlock{
		To: ResourceAddr{Type: "aws_instance", Name: "server"},
		ForEach: map[string]string{
			"web": "i-1234567890abcdef0",
			// "api": "i-0987654321fedcba0", // 시뮬레이션에 없으므로 제외
		},
	}

	targets := expandImportBlock(forEachBlock)
	fmt.Printf("  for_each 확장 결과 (%d개 대상):\n", len(targets))
	for _, t := range targets {
		fmt.Printf("    - %s → ID: %s\n", t.Addr.String(), t.ID)
	}

	forEachChanges, err := executor.ExecuteDeclarativeImport([]ImportBlock{forEachBlock}, true)
	if err != nil {
		fmt.Printf("  [에러] %s\n", err)
	} else {
		fmt.Printf("\n  for_each 설정 생성 결과 (%d개):\n", len(forEachChanges))
		for _, c := range forEachChanges {
			fmt.Printf("  --- %s (from %q) ---\n", c.Addr, c.ImportID)
			// 처음 몇 줄만 출력
			lines := strings.Split(c.GeneratedConfig, "\n")
			for i, line := range lines {
				if i >= 5 {
					fmt.Printf("    ... (%d줄 더)\n", len(lines)-5)
					break
				}
				fmt.Printf("    %s\n", line)
			}
		}
	}
	fmt.Println()

	// --- 6단계: JSON 값의 jsonencode 래핑 ---
	fmt.Println("--- 6단계: JSON 값의 jsonencode() 래핑 ---")

	jsonVal := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`
	fmt.Printf("  원본 JSON: %s\n", jsonVal)
	fmt.Printf("  유효한 JSON: %v\n", isValidJSON(jsonVal))
	fmt.Printf("  원시 타입: %v\n", isPrimitive(jsonVal))

	var buf strings.Builder
	buf.WriteString("jsonencode(")
	writeHCLValue(&buf, parseJSON(jsonVal))
	buf.WriteString(")")
	fmt.Printf("  HCL 출력: %s\n", buf.String())
	fmt.Println()

	// --- 7단계: Import 블록 스키마 표시 ---
	fmt.Println("--- 7단계: Import 블록 스키마 ---")

	fmt.Println("  import {")
	fmt.Println("    to       = <resource_address>  # 필수")
	fmt.Println("    id       = <provider_id>       # id 또는 identity 중 하나 필수")
	fmt.Println("    identity = { ... }             # id의 대안")
	fmt.Println("    provider = <provider_ref>      # 선택적")
	fmt.Println("    for_each = <map_or_set>        # 선택적")
	fmt.Println("  }")
	fmt.Println()

	// --- 8단계: 대상 파일 검증 ---
	fmt.Println("--- 8단계: 생성 파일 규칙 ---")

	err = ValidateTargetFile("")
	if err != nil {
		fmt.Printf("  빈 경로: [에러] %s\n", err)
	}
	err = ValidateTargetFile("generated.tf")
	if err != nil {
		fmt.Printf("  유효한 경로: [에러] %s\n", err)
	} else {
		fmt.Println("  유효한 경로: [통과] generated.tf")
	}
	fmt.Println("  주의: 기존 파일이 존재하면 Terraform은 에러를 반환합니다")
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
