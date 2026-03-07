package main

import (
	"fmt"
	"sort"
	"strings"
)

// =============================================================================
// Terraform 리소스 Diff 계산 시뮬레이션
// =============================================================================
// Terraform은 현재 상태(prior state)와 계획된 상태(planned state)를 비교하여
// 리소스별 변경 사항(diff)을 계산합니다.
//
// 실제 코드:
//   internal/plans/changes.go          - ResourceInstanceChange
//   internal/plans/objchange/          - 객체 변경 비교
//   internal/command/format.go         - diff 출력 포맷
//
// 핵심 개념:
// 1. 속성별 변경 타입: Added, Removed, Modified, Unchanged
// 2. ForceNew: 특정 속성 변경 시 리소스 교체(destroy + create)
// 3. Sensitive: 민감 속성 마스킹
// 4. Nested Block: 중첩 블록(예: ingress 규칙) 비교
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// 스키마 정의
// ─────────────────────────────────────────────────────────────────────────────

// AttrType은 속성의 타입입니다.
type AttrType int

const (
	AttrTypeString AttrType = iota
	AttrTypeNumber
	AttrTypeBool
	AttrTypeList
	AttrTypeMap
)

func (t AttrType) String() string {
	switch t {
	case AttrTypeString:
		return "string"
	case AttrTypeNumber:
		return "number"
	case AttrTypeBool:
		return "bool"
	case AttrTypeList:
		return "list"
	case AttrTypeMap:
		return "map"
	default:
		return "unknown"
	}
}

// SchemaAttribute는 리소스 스키마의 속성 정의입니다.
// 실제: internal/providers/schema.go
type SchemaAttribute struct {
	Name      string
	Type      AttrType
	Required  bool
	Optional  bool
	Computed  bool // 프로바이더가 계산하는 값
	ForceNew  bool // 변경 시 리소스 교체 필요
	Sensitive bool // 민감 정보 (출력 시 마스킹)
}

// NestedBlockDef는 중첩 블록 정의입니다.
type NestedBlockDef struct {
	Name       string
	Attributes []SchemaAttribute
	MinItems   int
	MaxItems   int // 0 = 제한 없음
}

// ResourceSchema는 리소스의 전체 스키마입니다.
type ResourceSchema struct {
	TypeName     string
	Attributes   []SchemaAttribute
	NestedBlocks []NestedBlockDef
}

// GetAttribute는 이름으로 속성을 찾습니다.
func (s *ResourceSchema) GetAttribute(name string) *SchemaAttribute {
	for i := range s.Attributes {
		if s.Attributes[i].Name == name {
			return &s.Attributes[i]
		}
	}
	return nil
}

// GetNestedBlock는 이름으로 중첩 블록을 찾습니다.
func (s *ResourceSchema) GetNestedBlock(name string) *NestedBlockDef {
	for i := range s.NestedBlocks {
		if s.NestedBlocks[i].Name == name {
			return &s.NestedBlocks[i]
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 리소스 값
// ─────────────────────────────────────────────────────────────────────────────

// ResourceValues는 리소스의 속성 값들입니다.
type ResourceValues struct {
	Attributes   map[string]string              // 단순 속성
	NestedBlocks map[string][]map[string]string  // 중첩 블록 (이름 → 블록 인스턴스 목록)
}

func NewResourceValues() *ResourceValues {
	return &ResourceValues{
		Attributes:   make(map[string]string),
		NestedBlocks: make(map[string][]map[string]string),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 변경 타입 (Change Type)
// ─────────────────────────────────────────────────────────────────────────────

// ChangeAction은 리소스 레벨의 변경 타입입니다.
// 실제: internal/plans/action.go
type ChangeAction int

const (
	ActionNoop    ChangeAction = iota // 변경 없음
	ActionCreate                      // 새로 생성
	ActionRead                        // 데이터 소스 읽기
	ActionUpdate                      // 제자리 업데이트
	ActionReplace                     // 교체 (delete + create)
	ActionDelete                      // 삭제
)

func (a ChangeAction) String() string {
	switch a {
	case ActionNoop:
		return "no-op"
	case ActionCreate:
		return "create"
	case ActionRead:
		return "read"
	case ActionUpdate:
		return "update"
	case ActionReplace:
		return "replace (destroy then create)"
	case ActionDelete:
		return "destroy"
	default:
		return "unknown"
	}
}

func (a ChangeAction) Symbol() string {
	switch a {
	case ActionNoop:
		return " "
	case ActionCreate:
		return "+"
	case ActionUpdate:
		return "~"
	case ActionReplace:
		return "-/+"
	case ActionDelete:
		return "-"
	default:
		return "?"
	}
}

// AttrChangeType은 속성 레벨의 변경 타입입니다.
type AttrChangeType int

const (
	AttrUnchanged AttrChangeType = iota
	AttrAdded
	AttrRemoved
	AttrModified
)

func (t AttrChangeType) String() string {
	switch t {
	case AttrUnchanged:
		return "unchanged"
	case AttrAdded:
		return "added"
	case AttrRemoved:
		return "removed"
	case AttrModified:
		return "modified"
	default:
		return "unknown"
	}
}

func (t AttrChangeType) Symbol() string {
	switch t {
	case AttrUnchanged:
		return " "
	case AttrAdded:
		return "+"
	case AttrRemoved:
		return "-"
	case AttrModified:
		return "~"
	default:
		return "?"
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Diff 결과
// ─────────────────────────────────────────────────────────────────────────────

// AttrDiff는 단일 속성의 변경 정보입니다.
type AttrDiff struct {
	Name      string
	OldValue  string
	NewValue  string
	Change    AttrChangeType
	ForceNew  bool
	Sensitive bool
	Computed  bool
}

// NestedBlockDiff는 중첩 블록의 변경 정보입니다.
type NestedBlockDiff struct {
	Name    string
	Added   []map[string]string // 추가된 블록들
	Removed []map[string]string // 제거된 블록들
	Changed []NestedBlockChange // 변경된 블록들
}

// NestedBlockChange는 중첩 블록 하나의 변경입니다.
type NestedBlockChange struct {
	OldValues map[string]string
	NewValues map[string]string
	AttrDiffs []AttrDiff
}

// ResourceDiff는 리소스 전체의 변경 정보입니다.
type ResourceDiff struct {
	ResourceType string
	ResourceName string
	Action       ChangeAction
	AttrDiffs    []AttrDiff
	BlockDiffs   []NestedBlockDiff
	RequiresNew  bool // ForceNew 속성 변경으로 교체 필요
	ForceNewAttr string // 교체를 유발한 속성
}

// ─────────────────────────────────────────────────────────────────────────────
// Diff 계산기
// ─────────────────────────────────────────────────────────────────────────────

// DiffCalculator는 리소스 diff를 계산합니다.
type DiffCalculator struct {
	schema *ResourceSchema
}

func NewDiffCalculator(schema *ResourceSchema) *DiffCalculator {
	return &DiffCalculator{schema: schema}
}

// Calculate는 이전 상태와 새 상태를 비교하여 diff를 계산합니다.
func (c *DiffCalculator) Calculate(
	resourceName string,
	old *ResourceValues,
	new *ResourceValues,
) *ResourceDiff {
	diff := &ResourceDiff{
		ResourceType: c.schema.TypeName,
		ResourceName: resourceName,
	}

	// 1. 생성 / 삭제 판단
	if old == nil && new != nil {
		diff.Action = ActionCreate
		c.calculateCreateDiff(diff, new)
		return diff
	}
	if old != nil && new == nil {
		diff.Action = ActionDelete
		c.calculateDeleteDiff(diff, old)
		return diff
	}
	if old == nil && new == nil {
		diff.Action = ActionNoop
		return diff
	}

	// 2. 속성별 diff 계산
	c.calculateAttrDiffs(diff, old, new)

	// 3. 중첩 블록 diff 계산
	c.calculateBlockDiffs(diff, old, new)

	// 4. ForceNew 확인
	for _, ad := range diff.AttrDiffs {
		if ad.ForceNew && ad.Change == AttrModified {
			diff.RequiresNew = true
			diff.ForceNewAttr = ad.Name
			break
		}
	}

	// 5. 최종 액션 결정
	if diff.RequiresNew {
		diff.Action = ActionReplace
	} else {
		hasChanges := false
		for _, ad := range diff.AttrDiffs {
			if ad.Change != AttrUnchanged {
				hasChanges = true
				break
			}
		}
		for _, bd := range diff.BlockDiffs {
			if len(bd.Added) > 0 || len(bd.Removed) > 0 || len(bd.Changed) > 0 {
				hasChanges = true
				break
			}
		}

		if hasChanges {
			diff.Action = ActionUpdate
		} else {
			diff.Action = ActionNoop
		}
	}

	return diff
}

func (c *DiffCalculator) calculateCreateDiff(diff *ResourceDiff, new *ResourceValues) {
	for _, attr := range c.schema.Attributes {
		val, exists := new.Attributes[attr.Name]
		if !exists && attr.Computed {
			diff.AttrDiffs = append(diff.AttrDiffs, AttrDiff{
				Name:      attr.Name,
				NewValue:  "(known after apply)",
				Change:    AttrAdded,
				Computed:  true,
				Sensitive: attr.Sensitive,
			})
		} else if exists {
			diff.AttrDiffs = append(diff.AttrDiffs, AttrDiff{
				Name:      attr.Name,
				NewValue:  val,
				Change:    AttrAdded,
				Sensitive: attr.Sensitive,
			})
		}
	}

	for _, block := range c.schema.NestedBlocks {
		if blocks, ok := new.NestedBlocks[block.Name]; ok {
			diff.BlockDiffs = append(diff.BlockDiffs, NestedBlockDiff{
				Name:  block.Name,
				Added: blocks,
			})
		}
	}
}

func (c *DiffCalculator) calculateDeleteDiff(diff *ResourceDiff, old *ResourceValues) {
	for _, attr := range c.schema.Attributes {
		if val, exists := old.Attributes[attr.Name]; exists {
			diff.AttrDiffs = append(diff.AttrDiffs, AttrDiff{
				Name:      attr.Name,
				OldValue:  val,
				Change:    AttrRemoved,
				Sensitive: attr.Sensitive,
			})
		}
	}

	for _, block := range c.schema.NestedBlocks {
		if blocks, ok := old.NestedBlocks[block.Name]; ok {
			diff.BlockDiffs = append(diff.BlockDiffs, NestedBlockDiff{
				Name:    block.Name,
				Removed: blocks,
			})
		}
	}
}

func (c *DiffCalculator) calculateAttrDiffs(diff *ResourceDiff, old, new *ResourceValues) {
	// 모든 속성을 확인
	allKeys := make(map[string]bool)
	for k := range old.Attributes {
		allKeys[k] = true
	}
	for k := range new.Attributes {
		allKeys[k] = true
	}

	for _, attr := range c.schema.Attributes {
		oldVal, oldExists := old.Attributes[attr.Name]
		newVal, newExists := new.Attributes[attr.Name]

		ad := AttrDiff{
			Name:      attr.Name,
			OldValue:  oldVal,
			NewValue:  newVal,
			ForceNew:  attr.ForceNew,
			Sensitive: attr.Sensitive,
			Computed:  attr.Computed,
		}

		if !oldExists && newExists {
			ad.Change = AttrAdded
		} else if oldExists && !newExists {
			if attr.Computed {
				ad.NewValue = "(known after apply)"
				ad.Change = AttrModified
			} else {
				ad.Change = AttrRemoved
			}
		} else if oldExists && newExists {
			if oldVal != newVal {
				ad.Change = AttrModified
			} else {
				ad.Change = AttrUnchanged
			}
		} else {
			continue // 양쪽 모두 없음
		}

		diff.AttrDiffs = append(diff.AttrDiffs, ad)
	}
}

func (c *DiffCalculator) calculateBlockDiffs(diff *ResourceDiff, old, new *ResourceValues) {
	for _, blockDef := range c.schema.NestedBlocks {
		oldBlocks := old.NestedBlocks[blockDef.Name]
		newBlocks := new.NestedBlocks[blockDef.Name]

		bd := NestedBlockDiff{Name: blockDef.Name}

		// 간단한 비교: 동일한 식별 속성(첫 번째 속성)으로 매칭
		oldMap := indexBlocks(oldBlocks)
		newMap := indexBlocks(newBlocks)

		// 추가된 블록
		for key, block := range newMap {
			if _, exists := oldMap[key]; !exists {
				bd.Added = append(bd.Added, block)
			}
		}

		// 제거된 블록
		for key, block := range oldMap {
			if _, exists := newMap[key]; !exists {
				bd.Removed = append(bd.Removed, block)
			}
		}

		// 변경된 블록
		for key, newBlock := range newMap {
			if oldBlock, exists := oldMap[key]; exists {
				// 속성 비교
				var attrDiffs []AttrDiff
				allAttrs := make(map[string]bool)
				for k := range oldBlock {
					allAttrs[k] = true
				}
				for k := range newBlock {
					allAttrs[k] = true
				}

				hasChange := false
				for attrName := range allAttrs {
					oldV := oldBlock[attrName]
					newV := newBlock[attrName]
					if oldV != newV {
						hasChange = true
						change := AttrModified
						if oldV == "" {
							change = AttrAdded
						} else if newV == "" {
							change = AttrRemoved
						}
						attrDiffs = append(attrDiffs, AttrDiff{
							Name:     attrName,
							OldValue: oldV,
							NewValue: newV,
							Change:   change,
						})
					}
				}

				if hasChange {
					bd.Changed = append(bd.Changed, NestedBlockChange{
						OldValues: oldBlock,
						NewValues: newBlock,
						AttrDiffs: attrDiffs,
					})
				}
			}
		}

		if len(bd.Added) > 0 || len(bd.Removed) > 0 || len(bd.Changed) > 0 {
			diff.BlockDiffs = append(diff.BlockDiffs, bd)
		}
	}
}

// indexBlocks는 블록 목록을 키로 인덱싱합니다.
func indexBlocks(blocks []map[string]string) map[string]map[string]string {
	result := make(map[string]map[string]string)
	for _, block := range blocks {
		// 첫 번째 키-값 쌍으로 식별 (간단화)
		var keys []string
		for k := range block {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", k, block[k]))
		}
		key := strings.Join(parts, ",")
		result[key] = block
	}
	return result
}

// ─────────────────────────────────────────────────────────────────────────────
// Diff 포맷터 (Terraform plan 출력 형태)
// ─────────────────────────────────────────────────────────────────────────────

func FormatDiff(diff *ResourceDiff) string {
	var sb strings.Builder

	// 헤더
	symbol := diff.Action.Symbol()
	sb.WriteString(fmt.Sprintf("  # %s.%s will be %s\n",
		diff.ResourceType, diff.ResourceName, diff.Action.String()))

	if diff.RequiresNew {
		sb.WriteString(fmt.Sprintf("  # ('%s' 속성 변경으로 리소스 교체 필요)\n", diff.ForceNewAttr))
	}

	switch diff.Action {
	case ActionCreate:
		sb.WriteString(fmt.Sprintf("  %s resource \"%s\" \"%s\" {\n",
			symbol, diff.ResourceType, diff.ResourceName))
	case ActionUpdate:
		sb.WriteString(fmt.Sprintf("  %s resource \"%s\" \"%s\" {\n",
			symbol, diff.ResourceType, diff.ResourceName))
	case ActionReplace:
		sb.WriteString(fmt.Sprintf("  %s resource \"%s\" \"%s\" {\n",
			symbol, diff.ResourceType, diff.ResourceName))
	case ActionDelete:
		sb.WriteString(fmt.Sprintf("  %s resource \"%s\" \"%s\" {\n",
			symbol, diff.ResourceType, diff.ResourceName))
	case ActionNoop:
		sb.WriteString("  (변경 없음)\n")
		return sb.String()
	}

	// 속성 diff
	for _, ad := range diff.AttrDiffs {
		oldDisplay := ad.OldValue
		newDisplay := ad.NewValue

		if ad.Sensitive {
			if oldDisplay != "" {
				oldDisplay = "(sensitive)"
			}
			if newDisplay != "" {
				newDisplay = "(sensitive)"
			}
		}

		switch ad.Change {
		case AttrAdded:
			forceNew := ""
			if ad.ForceNew {
				forceNew = " # forces replacement"
			}
			sb.WriteString(fmt.Sprintf("      + %-20s = %q%s\n", ad.Name, newDisplay, forceNew))
		case AttrRemoved:
			sb.WriteString(fmt.Sprintf("      - %-20s = %q -> null\n", ad.Name, oldDisplay))
		case AttrModified:
			forceNew := ""
			if ad.ForceNew {
				forceNew = " # forces replacement"
			}
			sb.WriteString(fmt.Sprintf("      ~ %-20s = %q -> %q%s\n",
				ad.Name, oldDisplay, newDisplay, forceNew))
		case AttrUnchanged:
			sb.WriteString(fmt.Sprintf("        %-20s = %q\n", ad.Name, oldDisplay))
		}
	}

	// 중첩 블록 diff
	for _, bd := range diff.BlockDiffs {
		for _, block := range bd.Added {
			sb.WriteString(fmt.Sprintf("\n      + %s {\n", bd.Name))
			keys := sortedKeys(block)
			for _, k := range keys {
				sb.WriteString(fmt.Sprintf("          + %-16s = %q\n", k, block[k]))
			}
			sb.WriteString("        }\n")
		}

		for _, block := range bd.Removed {
			sb.WriteString(fmt.Sprintf("\n      - %s {\n", bd.Name))
			keys := sortedKeys(block)
			for _, k := range keys {
				sb.WriteString(fmt.Sprintf("          - %-16s = %q\n", k, block[k]))
			}
			sb.WriteString("        }\n")
		}

		for _, change := range bd.Changed {
			sb.WriteString(fmt.Sprintf("\n      ~ %s {\n", bd.Name))
			for _, ad := range change.AttrDiffs {
				switch ad.Change {
				case AttrModified:
					sb.WriteString(fmt.Sprintf("          ~ %-16s = %q -> %q\n",
						ad.Name, ad.OldValue, ad.NewValue))
				case AttrAdded:
					sb.WriteString(fmt.Sprintf("          + %-16s = %q\n", ad.Name, ad.NewValue))
				case AttrRemoved:
					sb.WriteString(fmt.Sprintf("          - %-16s = %q\n", ad.Name, ad.OldValue))
				}
			}
			sb.WriteString("        }\n")
		}
	}

	sb.WriteString("    }\n")
	return sb.String()
}

func sortedKeys(m map[string]string) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// ─────────────────────────────────────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Terraform 리소스 Diff 계산 시뮬레이션                      ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  스키마 기반 속성 비교, ForceNew, Sensitive, Nested Block           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// ─── 스키마 정의 ───
	// aws_instance 스키마
	instanceSchema := &ResourceSchema{
		TypeName: "aws_instance",
		Attributes: []SchemaAttribute{
			{Name: "id", Type: AttrTypeString, Computed: true},
			{Name: "ami", Type: AttrTypeString, Required: true, ForceNew: true},
			{Name: "instance_type", Type: AttrTypeString, Required: true},
			{Name: "key_name", Type: AttrTypeString, Optional: true, ForceNew: true},
			{Name: "subnet_id", Type: AttrTypeString, Optional: true, ForceNew: true},
			{Name: "private_ip", Type: AttrTypeString, Computed: true},
			{Name: "public_ip", Type: AttrTypeString, Computed: true},
			{Name: "user_data", Type: AttrTypeString, Optional: true, ForceNew: true, Sensitive: true},
			{Name: "tags", Type: AttrTypeMap, Optional: true},
		},
	}

	// aws_security_group 스키마 (중첩 블록 포함)
	sgSchema := &ResourceSchema{
		TypeName: "aws_security_group",
		Attributes: []SchemaAttribute{
			{Name: "id", Type: AttrTypeString, Computed: true},
			{Name: "name", Type: AttrTypeString, Required: true, ForceNew: true},
			{Name: "description", Type: AttrTypeString, Optional: true, ForceNew: true},
			{Name: "vpc_id", Type: AttrTypeString, Required: true, ForceNew: true},
		},
		NestedBlocks: []NestedBlockDef{
			{
				Name:     "ingress",
				MaxItems: 0,
				Attributes: []SchemaAttribute{
					{Name: "from_port", Type: AttrTypeNumber, Required: true},
					{Name: "to_port", Type: AttrTypeNumber, Required: true},
					{Name: "protocol", Type: AttrTypeString, Required: true},
					{Name: "cidr_blocks", Type: AttrTypeList, Optional: true},
					{Name: "description", Type: AttrTypeString, Optional: true},
				},
			},
			{
				Name:     "egress",
				MaxItems: 0,
				Attributes: []SchemaAttribute{
					{Name: "from_port", Type: AttrTypeNumber, Required: true},
					{Name: "to_port", Type: AttrTypeNumber, Required: true},
					{Name: "protocol", Type: AttrTypeString, Required: true},
					{Name: "cidr_blocks", Type: AttrTypeList, Optional: true},
				},
			},
		},
	}

	// ─── 예제 1: 리소스 생성 (Create) ───
	printSeparator("1. 리소스 생성 (Create)")

	newInstance := NewResourceValues()
	newInstance.Attributes["ami"] = "ami-0abcdef1234567890"
	newInstance.Attributes["instance_type"] = "t2.micro"
	newInstance.Attributes["key_name"] = "my-key"
	newInstance.Attributes["subnet_id"] = "subnet-abc123"
	newInstance.Attributes["user_data"] = "#!/bin/bash\necho hello"
	newInstance.Attributes["tags"] = "Name=web-server"

	calc := NewDiffCalculator(instanceSchema)
	diff1 := calc.Calculate("web", nil, newInstance)
	fmt.Print(FormatDiff(diff1))

	// ─── 예제 2: 제자리 업데이트 (Update) ───
	printSeparator("2. 제자리 업데이트 (Update in-place)")
	fmt.Println("  instance_type 변경: t2.micro → t2.small (ForceNew 아님)")
	fmt.Println()

	oldInstance := NewResourceValues()
	oldInstance.Attributes["id"] = "i-abc123"
	oldInstance.Attributes["ami"] = "ami-0abcdef1234567890"
	oldInstance.Attributes["instance_type"] = "t2.micro"
	oldInstance.Attributes["key_name"] = "my-key"
	oldInstance.Attributes["subnet_id"] = "subnet-abc123"
	oldInstance.Attributes["private_ip"] = "10.0.1.5"
	oldInstance.Attributes["public_ip"] = "54.123.45.67"
	oldInstance.Attributes["tags"] = "Name=web-server"

	updatedInstance := NewResourceValues()
	updatedInstance.Attributes["id"] = "i-abc123"
	updatedInstance.Attributes["ami"] = "ami-0abcdef1234567890"
	updatedInstance.Attributes["instance_type"] = "t2.small"
	updatedInstance.Attributes["key_name"] = "my-key"
	updatedInstance.Attributes["subnet_id"] = "subnet-abc123"
	updatedInstance.Attributes["private_ip"] = "10.0.1.5"
	updatedInstance.Attributes["public_ip"] = "54.123.45.67"
	updatedInstance.Attributes["tags"] = "Name=web-server-v2"

	diff2 := calc.Calculate("web", oldInstance, updatedInstance)
	fmt.Print(FormatDiff(diff2))

	// ─── 예제 3: ForceNew (교체) ───
	printSeparator("3. ForceNew 속성 변경 (교체 필요)")
	fmt.Println("  ami 변경: ForceNew=true → 리소스 교체 (destroy + create)")
	fmt.Println()

	replaceInstance := NewResourceValues()
	replaceInstance.Attributes["id"] = "i-abc123"
	replaceInstance.Attributes["ami"] = "ami-0new1234567890" // ForceNew!
	replaceInstance.Attributes["instance_type"] = "t2.micro"
	replaceInstance.Attributes["key_name"] = "my-key"
	replaceInstance.Attributes["subnet_id"] = "subnet-abc123"

	diff3 := calc.Calculate("web", oldInstance, replaceInstance)
	fmt.Print(FormatDiff(diff3))

	// ─── 예제 4: Sensitive 속성 ───
	printSeparator("4. Sensitive 속성 (마스킹)")
	fmt.Println("  user_data(Sensitive=true) 변경 → 값이 마스킹됨")
	fmt.Println()

	oldSensitive := NewResourceValues()
	oldSensitive.Attributes["id"] = "i-abc123"
	oldSensitive.Attributes["ami"] = "ami-0abcdef1234567890"
	oldSensitive.Attributes["instance_type"] = "t2.micro"
	oldSensitive.Attributes["user_data"] = "#!/bin/bash\necho old-secret"

	newSensitive := NewResourceValues()
	newSensitive.Attributes["id"] = "i-abc123"
	newSensitive.Attributes["ami"] = "ami-0abcdef1234567890"
	newSensitive.Attributes["instance_type"] = "t2.micro"
	newSensitive.Attributes["user_data"] = "#!/bin/bash\necho new-secret"

	diff4 := calc.Calculate("web", oldSensitive, newSensitive)
	fmt.Print(FormatDiff(diff4))

	// ─── 예제 5: 리소스 삭제 (Destroy) ───
	printSeparator("5. 리소스 삭제 (Destroy)")

	diff5 := calc.Calculate("web", oldInstance, nil)
	fmt.Print(FormatDiff(diff5))

	// ─── 예제 6: 중첩 블록 (Security Group) ───
	printSeparator("6. 중첩 블록 변경 (Security Group ingress 규칙)")
	fmt.Println("  ingress 규칙 추가/제거/변경")
	fmt.Println()

	sgCalc := NewDiffCalculator(sgSchema)

	oldSG := NewResourceValues()
	oldSG.Attributes["id"] = "sg-abc123"
	oldSG.Attributes["name"] = "web-sg"
	oldSG.Attributes["description"] = "Web security group"
	oldSG.Attributes["vpc_id"] = "vpc-main"
	oldSG.NestedBlocks["ingress"] = []map[string]string{
		{"from_port": "80", "to_port": "80", "protocol": "tcp", "cidr_blocks": "0.0.0.0/0", "description": "HTTP"},
		{"from_port": "443", "to_port": "443", "protocol": "tcp", "cidr_blocks": "0.0.0.0/0", "description": "HTTPS"},
		{"from_port": "22", "to_port": "22", "protocol": "tcp", "cidr_blocks": "10.0.0.0/8", "description": "SSH"},
	}
	oldSG.NestedBlocks["egress"] = []map[string]string{
		{"from_port": "0", "to_port": "0", "protocol": "-1", "cidr_blocks": "0.0.0.0/0"},
	}

	newSG := NewResourceValues()
	newSG.Attributes["id"] = "sg-abc123"
	newSG.Attributes["name"] = "web-sg"
	newSG.Attributes["description"] = "Web security group"
	newSG.Attributes["vpc_id"] = "vpc-main"
	newSG.NestedBlocks["ingress"] = []map[string]string{
		{"from_port": "80", "to_port": "80", "protocol": "tcp", "cidr_blocks": "0.0.0.0/0", "description": "HTTP"},
		{"from_port": "443", "to_port": "443", "protocol": "tcp", "cidr_blocks": "0.0.0.0/0", "description": "HTTPS"},
		// SSH 규칙 제거됨
		// 8080 규칙 추가
		{"from_port": "8080", "to_port": "8080", "protocol": "tcp", "cidr_blocks": "10.0.0.0/8", "description": "API"},
	}
	newSG.NestedBlocks["egress"] = []map[string]string{
		{"from_port": "0", "to_port": "0", "protocol": "-1", "cidr_blocks": "0.0.0.0/0"},
	}

	diff6 := sgCalc.Calculate("web_sg", oldSG, newSG)
	fmt.Print(FormatDiff(diff6))

	// ─── 예제 7: 변경 없음 (Noop) ───
	printSeparator("7. 변경 없음 (No-op)")

	diff7 := calc.Calculate("web", oldInstance, oldInstance)
	fmt.Print(FormatDiff(diff7))

	// ─── 예제 8: 복합 변경 (여러 속성 동시 변경) ───
	printSeparator("8. 복합 변경 (여러 속성 동시 변경)")

	complexOld := NewResourceValues()
	complexOld.Attributes["id"] = "i-complex-1"
	complexOld.Attributes["ami"] = "ami-old-version"
	complexOld.Attributes["instance_type"] = "t2.micro"
	complexOld.Attributes["key_name"] = "old-key"
	complexOld.Attributes["subnet_id"] = "subnet-old"
	complexOld.Attributes["private_ip"] = "10.0.1.5"
	complexOld.Attributes["user_data"] = "old-script"
	complexOld.Attributes["tags"] = "Name=old-name,Env=dev"

	complexNew := NewResourceValues()
	complexNew.Attributes["ami"] = "ami-new-version"            // ForceNew!
	complexNew.Attributes["instance_type"] = "t2.large"          // Update
	complexNew.Attributes["key_name"] = "new-key"                // ForceNew!
	complexNew.Attributes["subnet_id"] = "subnet-new"            // ForceNew!
	complexNew.Attributes["user_data"] = "new-script"            // ForceNew + Sensitive
	complexNew.Attributes["tags"] = "Name=new-name,Env=prod"     // Update

	diff8 := calc.Calculate("complex", complexOld, complexNew)
	fmt.Print(FormatDiff(diff8))

	// ─── Plan 요약 ───
	printSeparator("Plan 요약")

	allDiffs := []*ResourceDiff{diff1, diff2, diff3, diff5, diff6, diff7, diff8}

	createCount := 0
	updateCount := 0
	replaceCount := 0
	deleteCount := 0
	noopCount := 0

	for _, d := range allDiffs {
		switch d.Action {
		case ActionCreate:
			createCount++
		case ActionUpdate:
			updateCount++
		case ActionReplace:
			replaceCount++
		case ActionDelete:
			deleteCount++
		case ActionNoop:
			noopCount++
		}
	}

	fmt.Printf("  Plan: %d to add, %d to change, %d to replace, %d to destroy, %d unchanged.\n",
		createCount, updateCount, replaceCount, deleteCount, noopCount)

	// ─── 아키텍처 요약 ───
	printSeparator("Diff 계산 아키텍처 요약")
	fmt.Print(`
  Diff 계산 흐름:

  ┌──────────────┐     ┌──────────────┐     ┌──────────────┐
  │ Prior State  │     │ Resource     │     │ Planned      │
  │ (현재 상태)  │     │ Schema       │     │ State        │
  │              │     │ (스키마)     │     │ (목표 상태)  │
  └──────┬───────┘     └──────┬───────┘     └──────┬───────┘
         │                    │                    │
         └────────────┬───────┘────────────────────┘
                      │
                      ▼
         ┌────────────────────────┐
         │   DiffCalculator      │
         │                       │
         │  1. 속성별 비교       │
         │  2. ForceNew 확인     │
         │  3. 중첩 블록 비교    │
         │  4. 액션 결정         │
         └────────────┬──────────┘
                      │
                      ▼
         ┌────────────────────────┐
         │   ResourceDiff         │
         │                       │
         │  Action: create/       │
         │          update/       │
         │          replace/      │
         │          delete/noop   │
         │                       │
         │  AttrDiffs:            │
         │    +/~/- 속성들       │
         │                       │
         │  BlockDiffs:           │
         │    중첩 블록 변경들   │
         └───────────────────────┘

  스키마 속성 플래그:

    Required  → 필수 (사용자 지정)
    Optional  → 선택 (사용자 지정)
    Computed  → 프로바이더 계산 (id, arn 등)
    ForceNew  → 변경 시 리소스 교체
    Sensitive → 출력 시 값 마스킹

  액션 결정 로직:

    old=nil, new!=nil        → Create
    old!=nil, new=nil        → Delete
    ForceNew 속성 변경       → Replace (Delete + Create)
    일반 속성 변경           → Update (in-place)
    변경 없음                → Noop

  실제 코드:
    internal/plans/changes.go        ResourceInstanceChange
    internal/plans/objchange/        객체 변경 비교
    internal/command/format.go       Diff 포맷팅
`)
}
