package main

import (
	"fmt"
	"sort"
	"strings"
)

// =============================================================================
// Terraform Plan 엔진 시뮬레이션
// =============================================================================
//
// Terraform Plan은 현재 상태(State)와 원하는 설정(Config)을 비교하여
// 어떤 변경이 필요한지 계산하는 핵심 프로세스이다.
//
// 실제 Terraform 소스:
//   - internal/terraform/node_resource_plan.go: 리소스 계획 노드
//   - internal/terraform/node_resource_plan_instance.go: 인스턴스별 계획
//   - internal/plans/changes.go: 변경 사항 구조체
//   - internal/plans/action.go: 액션 타입 (Create, Update, Delete, ...)
//
// 이 PoC에서 구현하는 핵심 개념:
//   1. 리소스 상태(State) 정의
//   2. 원하는 설정(Config)과 현재 상태 비교 (Diff)
//   3. 액션 결정 (Create, Update, Delete, NoOp, Replace)
//   4. "forces replacement" 속성 처리
//   5. Plan 구조체에 변경 사항 기록

// =============================================================================
// 1. 액션 타입 정의
// =============================================================================

// Action은 리소스에 대한 변경 액션을 나타낸다.
// Terraform의 plans.Action에 대응한다.
type Action int

const (
	NoOp    Action = iota // 변경 없음
	Create                // 새로 생성
	Update                // 속성 변경 (in-place)
	Delete                // 삭제
	Replace               // 교체 (delete + create)
)

var actionNames = map[Action]string{
	NoOp:    "no-op",
	Create:  "create",
	Update:  "update",
	Delete:  "delete",
	Replace: "replace",
}

var actionSymbols = map[Action]string{
	NoOp:    "  ",
	Create:  "+ ",
	Update:  "~ ",
	Delete:  "- ",
	Replace: "-/+",
}

var actionColors = map[Action]string{
	NoOp:    "",
	Create:  "[녹색]",
	Update:  "[노랑]",
	Delete:  "[빨강]",
	Replace: "[빨강/녹색]",
}

// =============================================================================
// 2. 리소스 상태 / 설정 구조체
// =============================================================================

// ResourceState는 리소스의 현재 상태를 나타낸다.
// Terraform의 states.ResourceInstanceObjectSrc에 대응한다.
type ResourceState struct {
	Type       string
	Name       string
	Attributes map[string]string
}

func (s *ResourceState) Address() string {
	return fmt.Sprintf("%s.%s", s.Type, s.Name)
}

// ResourceConfig는 원하는 리소스 설정을 나타낸다.
// Terraform의 configs.Resource에 대응한다.
type ResourceConfig struct {
	Type             string
	Name             string
	Attributes       map[string]string
	ForceNewAttrs    []string // 이 속성이 변경되면 교체(Replace) 필요
}

func (c *ResourceConfig) Address() string {
	return fmt.Sprintf("%s.%s", c.Type, c.Name)
}

// =============================================================================
// 3. 속성 변경 (Attribute Change)
// =============================================================================

// AttributeChange는 하나의 속성 변경을 나타낸다.
type AttributeChange struct {
	Key      string
	OldValue string
	NewValue string
	ForceNew bool // 이 속성 변경이 리소스 교체를 강제하는가?
}

// =============================================================================
// 4. 리소스 변경 (Resource Change)
// =============================================================================

// ResourceChange는 하나의 리소스에 대한 전체 변경 사항이다.
// Terraform의 plans.ResourceInstanceChange에 대응한다.
type ResourceChange struct {
	Address          string
	Action           Action
	AttributeChanges []AttributeChange
	Reason           string // 교체 사유
}

// =============================================================================
// 5. Plan 구조체
// =============================================================================

// Plan은 전체 변경 계획을 나타낸다.
// Terraform의 plans.Plan에 대응한다.
type Plan struct {
	Changes      []ResourceChange
	CreateCount  int
	UpdateCount  int
	DeleteCount  int
	ReplaceCount int
	NoOpCount    int
}

// =============================================================================
// 6. Plan 엔진 (Diff 계산)
// =============================================================================

// PlanEngine은 상태와 설정을 비교하여 Plan을 생성한다.
type PlanEngine struct {
	CurrentState map[string]*ResourceState
	DesiredConfig map[string]*ResourceConfig
}

// NewPlanEngine은 새로운 Plan 엔진을 생성한다.
func NewPlanEngine() *PlanEngine {
	return &PlanEngine{
		CurrentState:  make(map[string]*ResourceState),
		DesiredConfig: make(map[string]*ResourceConfig),
	}
}

// AddState는 현재 상태에 리소스를 추가한다.
func (e *PlanEngine) AddState(s *ResourceState) {
	e.CurrentState[s.Address()] = s
}

// AddConfig는 원하는 설정에 리소스를 추가한다.
func (e *PlanEngine) AddConfig(c *ResourceConfig) {
	e.DesiredConfig[c.Address()] = c
}

// ComputePlan은 현재 상태와 원하는 설정을 비교하여 Plan을 계산한다.
//
// Terraform의 실제 계획 프로세스:
//   1. NodePlannableResource.Execute() 에서 시작
//   2. plan() 메서드에서 프로바이더의 PlanResourceChange RPC 호출
//   3. 프로바이더가 proposed new state를 반환
//   4. 반환된 상태를 현재 상태와 비교하여 diff 계산
func (e *PlanEngine) ComputePlan() *Plan {
	plan := &Plan{}

	// 모든 주소 수집 (상태 + 설정)
	allAddrs := make(map[string]bool)
	for addr := range e.CurrentState {
		allAddrs[addr] = true
	}
	for addr := range e.DesiredConfig {
		allAddrs[addr] = true
	}

	// 주소를 정렬하여 일관된 순서로 처리
	sortedAddrs := make([]string, 0, len(allAddrs))
	for addr := range allAddrs {
		sortedAddrs = append(sortedAddrs, addr)
	}
	sort.Strings(sortedAddrs)

	for _, addr := range sortedAddrs {
		state, hasState := e.CurrentState[addr]
		config, hasConfig := e.DesiredConfig[addr]

		var change ResourceChange
		change.Address = addr

		switch {
		case !hasState && hasConfig:
			// 상태에 없고 설정에 있음 → Create
			change.Action = Create
			for key, value := range config.Attributes {
				change.AttributeChanges = append(change.AttributeChanges, AttributeChange{
					Key:      key,
					OldValue: "",
					NewValue: value,
				})
			}
			plan.CreateCount++

		case hasState && !hasConfig:
			// 상태에 있고 설정에 없음 → Delete
			change.Action = Delete
			for key, value := range state.Attributes {
				change.AttributeChanges = append(change.AttributeChanges, AttributeChange{
					Key:      key,
					OldValue: value,
					NewValue: "",
				})
			}
			plan.DeleteCount++

		case hasState && hasConfig:
			// 둘 다 있음 → 비교하여 Update/NoOp/Replace 결정
			change = e.diffResource(state, config)
			switch change.Action {
			case NoOp:
				plan.NoOpCount++
			case Update:
				plan.UpdateCount++
			case Replace:
				plan.ReplaceCount++
			}
		}

		plan.Changes = append(plan.Changes, change)
	}

	return plan
}

// diffResource는 하나의 리소스에 대해 상태와 설정을 비교한다.
func (e *PlanEngine) diffResource(state *ResourceState, config *ResourceConfig) ResourceChange {
	change := ResourceChange{
		Address: state.Address(),
	}

	// ForceNew 속성 집합 구축
	forceNewSet := make(map[string]bool)
	for _, attr := range config.ForceNewAttrs {
		forceNewSet[attr] = true
	}

	// 모든 속성 키 수집
	allKeys := make(map[string]bool)
	for key := range state.Attributes {
		allKeys[key] = true
	}
	for key := range config.Attributes {
		allKeys[key] = true
	}

	hasChanges := false
	forceReplace := false
	replaceReason := ""

	sortedKeys := make([]string, 0, len(allKeys))
	for key := range allKeys {
		sortedKeys = append(sortedKeys, key)
	}
	sort.Strings(sortedKeys)

	for _, key := range sortedKeys {
		oldVal := state.Attributes[key]
		newVal := config.Attributes[key]

		if oldVal != newVal {
			hasChanges = true
			ac := AttributeChange{
				Key:      key,
				OldValue: oldVal,
				NewValue: newVal,
				ForceNew: forceNewSet[key],
			}
			change.AttributeChanges = append(change.AttributeChanges, ac)

			if forceNewSet[key] {
				forceReplace = true
				replaceReason = fmt.Sprintf("속성 %q 변경 시 교체 필요 (forces replacement)", key)
			}
		}
	}

	if !hasChanges {
		change.Action = NoOp
	} else if forceReplace {
		change.Action = Replace
		change.Reason = replaceReason
	} else {
		change.Action = Update
	}

	return change
}

// =============================================================================
// 7. Plan 출력 (terraform plan 형식)
// =============================================================================

func printPlan(plan *Plan) {
	fmt.Println()
	fmt.Println("  Terraform이 다음 작업을 수행합니다:")
	fmt.Println()

	for _, change := range plan.Changes {
		symbol := actionSymbols[change.Action]
		actionName := actionNames[change.Action]
		color := actionColors[change.Action]

		if change.Action == NoOp {
			continue // NoOp은 표시하지 않음
		}

		fmt.Printf("  %s %s %s %s\n", color, symbol, change.Address, actionName)

		if change.Reason != "" {
			fmt.Printf("       # %s\n", change.Reason)
		}

		// 속성 변경 표시
		for _, ac := range change.AttributeChanges {
			forceNewTag := ""
			if ac.ForceNew {
				forceNewTag = " # forces replacement"
			}

			switch {
			case ac.OldValue == "" && ac.NewValue != "":
				fmt.Printf("       + %-25s = %q%s\n", ac.Key, ac.NewValue, forceNewTag)
			case ac.OldValue != "" && ac.NewValue == "":
				fmt.Printf("       - %-25s = %q%s\n", ac.Key, ac.OldValue, forceNewTag)
			default:
				fmt.Printf("       ~ %-25s = %q → %q%s\n", ac.Key, ac.OldValue, ac.NewValue, forceNewTag)
			}
		}
		fmt.Println()
	}

	// 요약
	fmt.Println("  ─────────────────────────────────────────────────")
	fmt.Printf("  Plan: %d to add, %d to change, %d to destroy, %d to replace.\n",
		plan.CreateCount, plan.UpdateCount, plan.DeleteCount, plan.ReplaceCount)
	if plan.NoOpCount > 0 {
		fmt.Printf("  (%d resources unchanged)\n", plan.NoOpCount)
	}
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║   Terraform Plan 엔진 시뮬레이션                          ║")
	fmt.Println("║   실제 코드: internal/terraform/node_resource_plan.go    ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	engine := NewPlanEngine()

	// =========================================================================
	// 현재 상태 (State) 정의
	// 이미 존재하는 인프라 리소스
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  현재 상태 (terraform.tfstate)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// VPC (변경 없음 - NoOp)
	engine.AddState(&ResourceState{
		Type: "aws_vpc", Name: "main",
		Attributes: map[string]string{
			"cidr_block":           "10.0.0.0/16",
			"enable_dns_hostnames": "true",
			"id":                   "vpc-abc123",
		},
	})

	// Subnet (속성 변경 - Update)
	engine.AddState(&ResourceState{
		Type: "aws_subnet", Name: "public",
		Attributes: map[string]string{
			"vpc_id":            "vpc-abc123",
			"cidr_block":        "10.0.1.0/24",
			"availability_zone": "ap-northeast-2a",
			"id":                "subnet-def456",
		},
	})

	// EC2 Instance (AMI 변경 → forces replacement - Replace)
	engine.AddState(&ResourceState{
		Type: "aws_instance", Name: "web",
		Attributes: map[string]string{
			"ami":           "ami-old12345",
			"instance_type": "t3.micro",
			"subnet_id":     "subnet-def456",
			"id":            "i-ghi789",
		},
	})

	// Security Group (설정에서 제거 - Delete)
	engine.AddState(&ResourceState{
		Type: "aws_security_group", Name: "deprecated",
		Attributes: map[string]string{
			"name":   "deprecated-sg",
			"vpc_id": "vpc-abc123",
			"id":     "sg-old999",
		},
	})

	// 현재 상태 출력
	stateAddrs := make([]string, 0)
	for addr := range engine.CurrentState {
		stateAddrs = append(stateAddrs, addr)
	}
	sort.Strings(stateAddrs)
	for _, addr := range stateAddrs {
		s := engine.CurrentState[addr]
		fmt.Printf("    %s (id=%s)\n", addr, s.Attributes["id"])
		for k, v := range s.Attributes {
			if k != "id" {
				fmt.Printf("      %-25s = %s\n", k, v)
			}
		}
		fmt.Println()
	}

	// =========================================================================
	// 원하는 설정 (Config) 정의
	// .tf 파일에서 선언한 리소스
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  원하는 설정 (*.tf 파일)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// VPC (변경 없음)
	engine.AddConfig(&ResourceConfig{
		Type: "aws_vpc", Name: "main",
		Attributes: map[string]string{
			"cidr_block":           "10.0.0.0/16",
			"enable_dns_hostnames": "true",
			"id":                   "vpc-abc123",
		},
	})

	// Subnet (태그 추가 → Update)
	engine.AddConfig(&ResourceConfig{
		Type: "aws_subnet", Name: "public",
		Attributes: map[string]string{
			"vpc_id":            "vpc-abc123",
			"cidr_block":        "10.0.1.0/24",
			"availability_zone": "ap-northeast-2a",
			"tags":              "Name=public-subnet",
			"id":                "subnet-def456",
		},
	})

	// EC2 Instance (AMI 변경 → Replace)
	engine.AddConfig(&ResourceConfig{
		Type: "aws_instance", Name: "web",
		Attributes: map[string]string{
			"ami":           "ami-new67890",
			"instance_type": "t3.micro",
			"subnet_id":     "subnet-def456",
			"id":            "i-ghi789",
		},
		ForceNewAttrs: []string{"ami"}, // AMI 변경 시 교체 필요
	})

	// EIP (신규 생성 → Create)
	engine.AddConfig(&ResourceConfig{
		Type: "aws_eip", Name: "web",
		Attributes: map[string]string{
			"instance": "i-ghi789",
			"vpc":      "true",
		},
	})

	// deprecated SG는 설정에서 제거 → Delete

	configAddrs := make([]string, 0)
	for addr := range engine.DesiredConfig {
		configAddrs = append(configAddrs, addr)
	}
	sort.Strings(configAddrs)
	for _, addr := range configAddrs {
		c := engine.DesiredConfig[addr]
		forceNew := ""
		if len(c.ForceNewAttrs) > 0 {
			forceNew = fmt.Sprintf(" [ForceNew: %s]", strings.Join(c.ForceNewAttrs, ", "))
		}
		fmt.Printf("    %s%s\n", addr, forceNew)
		for k, v := range c.Attributes {
			if k != "id" {
				fmt.Printf("      %-25s = %s\n", k, v)
			}
		}
		fmt.Println()
	}

	// =========================================================================
	// Plan 계산
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  Plan 계산 결과")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	plan := engine.ComputePlan()
	printPlan(plan)

	// =========================================================================
	// 액션별 설명
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  액션 유형 설명")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────────────────────────┐")
	fmt.Println("  │ 액션       │ 기호 │ 설명                              │")
	fmt.Println("  ├──────────────────────────────────────────────────────┤")
	fmt.Println("  │ Create     │ +    │ 신규 리소스 생성                    │")
	fmt.Println("  │ Update     │ ~    │ 기존 리소스 속성 변경 (in-place)     │")
	fmt.Println("  │ Delete     │ -    │ 기존 리소스 삭제                    │")
	fmt.Println("  │ Replace    │ -/+  │ 삭제 후 재생성 (forces replacement) │")
	fmt.Println("  │ NoOp       │      │ 변경 없음                         │")
	fmt.Println("  └──────────────────────────────────────────────────────┘")
	fmt.Println()

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  핵심 포인트 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. Plan은 현재 상태(State)와 원하는 설정(Config)의 diff이다")
	fmt.Println("  2. 상태에 없고 설정에 있으면 Create, 반대면 Delete")
	fmt.Println("  3. 둘 다 있으면 속성별 비교로 Update/NoOp/Replace 결정")
	fmt.Println("  4. ForceNew 속성(예: ami)이 변경되면 in-place 변경 불가 → Replace")
	fmt.Println("  5. 프로바이더가 어떤 속성이 ForceNew인지 스키마로 정의한다")
	fmt.Println("  6. Plan 결과는 apply 전에 사용자에게 보여주어 확인받는다")
}
