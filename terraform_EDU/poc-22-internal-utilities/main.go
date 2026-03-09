// Package main은 Terraform 내부 유틸리티 서브시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Promising - 데드락 프리 비동기 Promise 패턴
// 2. Promise 생명주기 (Unresolved → Resolved/Rejected)
// 3. Task 시스템 (goroutine 기반 비동기 작업)
// 4. 자기 의존성 감지 (데드락 방지)
// 5. Once 패턴 (단일 실행 보장)
// 6. Named Values (변수/로컬/출력 값의 그래프 워크 저장소)
// 7. Placeholder 결과 시스템
// 8. Instance Expander (count/for_each 확장)
// 9. Expansion Mode (Unknown, Known, Count, ForEach)
// 10. JSON Plan/State 직렬화
//
// 실제 소스 참조:
//   - internal/promising/       (Promise, Task, Once)
//   - internal/namedvals/       (State, Named Values 저장소)
//   - internal/instances/       (Expander, Expansion Mode)
//   - internal/command/jsonplan/ (JSON Plan 직렬화)
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. Promising - 데드락 프리 비동기 Promise 패턴
//    (internal/promising/ 시뮬레이션)
// ============================================================================

// PromiseState는 Promise의 상태를 나타낸다.
type PromiseState int

const (
	PromiseUnresolved PromiseState = iota
	PromiseResolved
	PromiseRejected
)

func (s PromiseState) String() string {
	switch s {
	case PromiseUnresolved:
		return "Unresolved"
	case PromiseResolved:
		return "Resolved"
	case PromiseRejected:
		return "Rejected"
	default:
		return "Unknown"
	}
}

// Promise는 비동기 값을 나타내는 제네릭 구조체다.
// 실제 구현: internal/promising/promise.go
type Promise struct {
	mu       sync.Mutex
	state    PromiseState
	value    interface{}
	err      error
	waiters  []chan struct{}
	ownerID  int // 데드락 감지를 위한 소유 태스크 ID
}

// NewPromise는 새 미해결 Promise를 생성한다.
func NewPromise(ownerID int) *Promise {
	return &Promise{
		state:   PromiseUnresolved,
		ownerID: ownerID,
	}
}

// Resolve는 Promise를 성공 값으로 해결한다.
func (p *Promise) Resolve(value interface{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != PromiseUnresolved {
		return fmt.Errorf("Promise가 이미 %s 상태임", p.state)
	}

	p.state = PromiseResolved
	p.value = value
	for _, w := range p.waiters {
		close(w)
	}
	p.waiters = nil
	return nil
}

// Reject는 Promise를 에러로 거부한다.
func (p *Promise) Reject(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != PromiseUnresolved {
		return fmt.Errorf("Promise가 이미 %s 상태임", p.state)
	}

	p.state = PromiseRejected
	p.err = err
	for _, w := range p.waiters {
		close(w)
	}
	p.waiters = nil
	return nil
}

// Get은 Promise가 해결될 때까지 대기하고 값을 반환한다.
// 자기 의존성(데드락)을 감지한다.
func (p *Promise) Get(callerTaskID int) (interface{}, error) {
	p.mu.Lock()
	// 자기 의존성 감지: 자신이 소유한 Promise를 자신이 기다리면 데드락
	if p.ownerID == callerTaskID && p.state == PromiseUnresolved {
		p.mu.Unlock()
		return nil, fmt.Errorf("자기 의존성 감지: 태스크 %d가 자신의 Promise를 대기 (데드락)", callerTaskID)
	}

	if p.state == PromiseResolved {
		v := p.value
		p.mu.Unlock()
		return v, nil
	}
	if p.state == PromiseRejected {
		e := p.err
		p.mu.Unlock()
		return nil, e
	}

	// 대기
	ch := make(chan struct{})
	p.waiters = append(p.waiters, ch)
	p.mu.Unlock()

	<-ch

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == PromiseResolved {
		return p.value, nil
	}
	return nil, p.err
}

// ============================================================================
// 2. Task 시스템 (internal/promising/ 시뮬레이션)
// ============================================================================

// Task는 비동기 작업을 나타낸다.
type Task struct {
	ID      int
	Name    string
	Promise *Promise
}

// TaskRunner는 태스크를 관리하는 러너다.
type TaskRunner struct {
	mu     sync.Mutex
	nextID int
	tasks  []*Task
}

// NewTaskRunner는 새 TaskRunner를 생성한다.
func NewTaskRunner() *TaskRunner {
	return &TaskRunner{nextID: 1}
}

// Spawn은 새 태스크를 생성하고 비동기로 실행한다.
func (r *TaskRunner) Spawn(name string, fn func(taskID int) (interface{}, error)) *Task {
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	task := &Task{
		ID:      id,
		Name:    name,
		Promise: NewPromise(id),
	}
	r.tasks = append(r.tasks, task)
	r.mu.Unlock()

	go func() {
		val, err := fn(id)
		if err != nil {
			task.Promise.Reject(err)
		} else {
			task.Promise.Resolve(val)
		}
	}()

	return task
}

// ============================================================================
// 3. Once 패턴 (internal/promising/once.go 시뮬레이션)
// ============================================================================

// Once는 값을 한 번만 계산하는 패턴이다.
// sync.Once와 유사하지만, 실패 시 재시도하지 않고 에러를 캐시한다.
type Once struct {
	mu      sync.Mutex
	done    bool
	value   interface{}
	err     error
}

// Do는 함수를 한 번만 실행하고 결과를 캐시한다.
func (o *Once) Do(fn func() (interface{}, error)) (interface{}, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.done {
		return o.value, o.err
	}

	o.value, o.err = fn()
	o.done = true
	return o.value, o.err
}

// ============================================================================
// 4. Named Values (internal/namedvals/ 시뮬레이션)
// ============================================================================

// ValueKey는 Named Value의 키를 나타낸다.
type ValueKey struct {
	Kind      string // "var", "local", "output"
	Name      string
	Module    string // 모듈 경로 (root이면 빈 문자열)
	Instance  string // 인스턴스 키 (count/for_each)
}

func (k ValueKey) String() string {
	parts := []string{}
	if k.Module != "" {
		parts = append(parts, "module."+k.Module)
	}
	parts = append(parts, k.Kind+"."+k.Name)
	if k.Instance != "" {
		parts = append(parts, "["+k.Instance+"]")
	}
	return strings.Join(parts, ".")
}

// NamedValuesState는 그래프 워크 중 값을 저장하는 저장소다.
// 실제 구현: internal/namedvals/namedvals.go의 State
type NamedValuesState struct {
	mu     sync.RWMutex
	values map[string]interface{}
	placeholders map[string]bool
}

// NewNamedValuesState는 새 Named Values 저장소를 생성한다.
func NewNamedValuesState() *NamedValuesState {
	return &NamedValuesState{
		values:       make(map[string]interface{}),
		placeholders: make(map[string]bool),
	}
}

// SetValue는 값을 저장한다.
func (s *NamedValuesState) SetValue(key ValueKey, value interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key.String()] = value
	delete(s.placeholders, key.String())
}

// GetValue는 값을 조회한다.
func (s *NamedValuesState) GetValue(key ValueKey) (interface{}, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key.String()]
	return v, ok
}

// SetPlaceholder는 아직 결정되지 않은 값의 플레이스홀더를 설정한다.
// Plan 단계에서 Known 값이 아직 없을 때 사용한다.
func (s *NamedValuesState) SetPlaceholder(key ValueKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.placeholders[key.String()] = true
}

// IsPlaceholder는 값이 플레이스홀더인지 확인한다.
func (s *NamedValuesState) IsPlaceholder(key ValueKey) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.placeholders[key.String()]
}

// AllValues는 모든 값을 반환한다.
func (s *NamedValuesState) AllValues() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]interface{}, len(s.values))
	for k, v := range s.values {
		result[k] = v
	}
	return result
}

// ============================================================================
// 5. Instance Expander (internal/instances/ 시뮬레이션)
// ============================================================================

// ExpansionMode는 리소스의 확장 모드를 나타낸다.
type ExpansionMode int

const (
	ExpansionSingle  ExpansionMode = iota // count/for_each 없음
	ExpansionCount                        // count = N
	ExpansionForEach                      // for_each = {...}
	ExpansionUnknown                      // 아직 결정되지 않음
)

func (m ExpansionMode) String() string {
	switch m {
	case ExpansionSingle:
		return "Single"
	case ExpansionCount:
		return "Count"
	case ExpansionForEach:
		return "ForEach"
	case ExpansionUnknown:
		return "Unknown"
	default:
		return "?"
	}
}

// InstanceKey는 개별 인스턴스를 식별하는 키다.
type InstanceKey struct {
	IntKey    int    // count 사용 시
	StringKey string // for_each 사용 시
	IsString  bool
}

func (k InstanceKey) String() string {
	if k.IsString {
		return fmt.Sprintf("[%q]", k.StringKey)
	}
	return fmt.Sprintf("[%d]", k.IntKey)
}

// Expansion은 리소스의 확장 정보를 나타낸다.
type Expansion struct {
	Address string
	Mode    ExpansionMode
	Count   int
	ForEach map[string]interface{}
}

// InstanceExpander는 count/for_each를 기반으로 인스턴스 목록을 확장한다.
// 실제 구현: internal/instances/expander.go
type InstanceExpander struct {
	mu         sync.Mutex
	expansions map[string]*Expansion
}

// NewInstanceExpander는 새 Instance Expander를 생성한다.
func NewInstanceExpander() *InstanceExpander {
	return &InstanceExpander{
		expansions: make(map[string]*Expansion),
	}
}

// SetSingle은 count/for_each가 없는 단일 인스턴스로 설정한다.
func (e *InstanceExpander) SetSingle(address string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expansions[address] = &Expansion{
		Address: address,
		Mode:    ExpansionSingle,
	}
}

// SetCount는 count 기반 확장을 설정한다.
func (e *InstanceExpander) SetCount(address string, count int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expansions[address] = &Expansion{
		Address: address,
		Mode:    ExpansionCount,
		Count:   count,
	}
}

// SetForEach는 for_each 기반 확장을 설정한다.
func (e *InstanceExpander) SetForEach(address string, each map[string]interface{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expansions[address] = &Expansion{
		Address: address,
		Mode:    ExpansionForEach,
		ForEach: each,
	}
}

// SetUnknown은 아직 결정되지 않은 확장으로 설정한다.
func (e *InstanceExpander) SetUnknown(address string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expansions[address] = &Expansion{
		Address: address,
		Mode:    ExpansionUnknown,
	}
}

// ExpandInstances는 주소에 대한 인스턴스 키 목록을 반환한다.
func (e *InstanceExpander) ExpandInstances(address string) ([]InstanceKey, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	exp, ok := e.expansions[address]
	if !ok {
		return nil, fmt.Errorf("확장 정보 없음: %s", address)
	}

	switch exp.Mode {
	case ExpansionSingle:
		return nil, nil // 인스턴스 키 없음 (단일)
	case ExpansionCount:
		keys := make([]InstanceKey, exp.Count)
		for i := 0; i < exp.Count; i++ {
			keys[i] = InstanceKey{IntKey: i}
		}
		return keys, nil
	case ExpansionForEach:
		keys := make([]InstanceKey, 0, len(exp.ForEach))
		for k := range exp.ForEach {
			keys = append(keys, InstanceKey{StringKey: k, IsString: true})
		}
		// 정렬
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[i].StringKey > keys[j].StringKey {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		return keys, nil
	case ExpansionUnknown:
		return nil, fmt.Errorf("확장이 아직 결정되지 않음: %s (plan 필요)", address)
	default:
		return nil, fmt.Errorf("알 수 없는 확장 모드: %v", exp.Mode)
	}
}

// ============================================================================
// 6. JSON Plan 구조 (internal/command/jsonplan 시뮬레이션)
// ============================================================================

const JSONPlanFormatVersion = "1.2"

// JSONPlan은 terraform show -json의 Plan 출력이다.
type JSONPlan struct {
	FormatVersion    string               `json:"format_version"`
	TerraformVersion string               `json:"terraform_version"`
	Variables        map[string]JSONVar    `json:"variables,omitempty"`
	PlannedValues    *JSONStateValues      `json:"planned_values,omitempty"`
	ResourceChanges  []JSONResourceChange  `json:"resource_changes,omitempty"`
}

// JSONVar는 변수 값이다.
type JSONVar struct {
	Value interface{} `json:"value"`
}

// JSONStateValues는 State의 값들이다.
type JSONStateValues struct {
	RootModule JSONModule `json:"root_module"`
}

// JSONModule은 모듈 단위의 리소스들이다.
type JSONModule struct {
	Resources []JSONResource `json:"resources,omitempty"`
}

// JSONResource는 개별 리소스다.
type JSONResource struct {
	Address      string                 `json:"address"`
	Type         string                 `json:"type"`
	Name         string                 `json:"name"`
	ProviderName string                 `json:"provider_name"`
	Values       map[string]interface{} `json:"values"`
}

// JSONResourceChange는 리소스 변경이다.
type JSONResourceChange struct {
	Address string     `json:"address"`
	Type    string     `json:"type"`
	Name    string     `json:"name"`
	Change  JSONChange `json:"change"`
}

// JSONChange는 변경 상세다.
type JSONChange struct {
	Actions []string    `json:"actions"`
	Before  interface{} `json:"before"`
	After   interface{} `json:"after"`
}

// ============================================================================
// 데모 함수들
// ============================================================================

func demoPromising() {
	fmt.Println("=== 1. Promising - 비동기 Promise 패턴 ===")

	runner := NewTaskRunner()

	// 태스크 체이닝: task1 결과를 task2가 사용
	task1 := runner.Spawn("fetch_ami", func(taskID int) (interface{}, error) {
		time.Sleep(10 * time.Millisecond)
		return "ami-0c55b159cbfafe1f0", nil
	})

	task2 := runner.Spawn("create_instance", func(taskID int) (interface{}, error) {
		// task1의 결과를 기다림
		ami, err := task1.Promise.Get(taskID)
		if err != nil {
			return nil, fmt.Errorf("AMI 조회 실패: %w", err)
		}
		return fmt.Sprintf("i-12345 (ami=%s)", ami), nil
	})

	// 결과 확인
	val, err := task2.Promise.Get(0) // callerTaskID=0 (외부)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  task2 결과: %v\n", val)
	}

	// 자기 의존성 감지
	fmt.Println()
	fmt.Println("  [자기 의존성 감지 테스트]")
	selfPromise := NewPromise(99)
	_, err = selfPromise.Get(99) // 자신의 Promise를 자신이 대기
	fmt.Printf("  결과: %v\n", err)

	// 거부된 Promise
	fmt.Println()
	fmt.Println("  [거부된 Promise 테스트]")
	rejTask := runner.Spawn("failing_task", func(taskID int) (interface{}, error) {
		return nil, fmt.Errorf("프로바이더 초기화 실패")
	})
	_, err = rejTask.Promise.Get(0)
	fmt.Printf("  결과: %v\n", err)
}

func demoOnce() {
	fmt.Println("=== 2. Once 패턴 ===")

	callCount := 0
	once := &Once{}

	for i := 0; i < 3; i++ {
		val, _ := once.Do(func() (interface{}, error) {
			callCount++
			return "computed_value", nil
		})
		fmt.Printf("  호출 %d: 결과=%v (실제 호출 횟수=%d)\n", i+1, val, callCount)
	}
}

func demoNamedValues() {
	fmt.Println("=== 3. Named Values 저장소 ===")

	state := NewNamedValuesState()

	// 변수 설정
	state.SetValue(ValueKey{Kind: "var", Name: "region"}, "ap-northeast-2")
	state.SetValue(ValueKey{Kind: "var", Name: "count"}, 3)

	// 로컬 값 설정
	state.SetValue(ValueKey{Kind: "local", Name: "name_prefix"}, "web")

	// 모듈 내 출력 값
	state.SetValue(ValueKey{Kind: "output", Name: "vpc_id", Module: "network"}, "vpc-abc123")

	// 인스턴스 키가 있는 값
	state.SetValue(ValueKey{Kind: "var", Name: "name", Instance: "0"}, "web-0")
	state.SetValue(ValueKey{Kind: "var", Name: "name", Instance: "1"}, "web-1")

	// 플레이스홀더 (아직 미결정)
	unknownKey := ValueKey{Kind: "output", Name: "instance_ip"}
	state.SetPlaceholder(unknownKey)

	// 조회
	val, ok := state.GetValue(ValueKey{Kind: "var", Name: "region"})
	fmt.Printf("  var.region: %v (존재=%v)\n", val, ok)

	val, ok = state.GetValue(ValueKey{Kind: "output", Name: "vpc_id", Module: "network"})
	fmt.Printf("  module.network.output.vpc_id: %v (존재=%v)\n", val, ok)

	isPH := state.IsPlaceholder(unknownKey)
	fmt.Printf("  output.instance_ip: 플레이스홀더=%v\n", isPH)

	// 전체 값 출력
	fmt.Println()
	fmt.Println("  [전체 저장된 값]")
	for k, v := range state.AllValues() {
		fmt.Printf("    %s = %v\n", k, v)
	}
}

func demoInstanceExpander() {
	fmt.Println("=== 4. Instance Expander ===")

	expander := NewInstanceExpander()

	// 단일 인스턴스
	expander.SetSingle("aws_vpc.main")
	keys, _ := expander.ExpandInstances("aws_vpc.main")
	fmt.Printf("  aws_vpc.main (Single): 인스턴스 키=%v → 주소=aws_vpc.main\n", keys)

	// Count 기반
	expander.SetCount("aws_instance.web", 3)
	keys, _ = expander.ExpandInstances("aws_instance.web")
	fmt.Printf("  aws_instance.web (Count=3): 인스턴스 키:\n")
	for _, k := range keys {
		fmt.Printf("    aws_instance.web%s\n", k)
	}

	// ForEach 기반
	expander.SetForEach("aws_s3_bucket.data", map[string]interface{}{
		"logs":    "my-logs-bucket",
		"assets":  "my-assets-bucket",
		"backups": "my-backups-bucket",
	})
	keys, _ = expander.ExpandInstances("aws_s3_bucket.data")
	fmt.Printf("  aws_s3_bucket.data (ForEach): 인스턴스 키:\n")
	for _, k := range keys {
		fmt.Printf("    aws_s3_bucket.data%s\n", k)
	}

	// Unknown
	expander.SetUnknown("aws_instance.dynamic")
	_, err := expander.ExpandInstances("aws_instance.dynamic")
	fmt.Printf("  aws_instance.dynamic (Unknown): %v\n", err)
}

func demoJSONPlan() {
	fmt.Println("=== 5. JSON Plan 직렬화 ===")

	plan := JSONPlan{
		FormatVersion:    JSONPlanFormatVersion,
		TerraformVersion: "1.10.0",
		Variables: map[string]JSONVar{
			"region": {Value: "ap-northeast-2"},
			"count":  {Value: 3},
		},
		PlannedValues: &JSONStateValues{
			RootModule: JSONModule{
				Resources: []JSONResource{
					{
						Address:      "aws_instance.web[0]",
						Type:         "aws_instance",
						Name:         "web",
						ProviderName: "registry.terraform.io/hashicorp/aws",
						Values: map[string]interface{}{
							"ami":           "ami-0c55b159cbfafe1f0",
							"instance_type": "t3.micro",
						},
					},
				},
			},
		},
		ResourceChanges: []JSONResourceChange{
			{
				Address: "aws_instance.web[0]",
				Type:    "aws_instance",
				Name:    "web",
				Change: JSONChange{
					Actions: []string{"create"},
					Before:  nil,
					After: map[string]interface{}{
						"ami":           "ami-0c55b159cbfafe1f0",
						"instance_type": "t3.micro",
					},
				},
			},
			{
				Address: "aws_security_group.web",
				Type:    "aws_security_group",
				Name:    "web",
				Change: JSONChange{
					Actions: []string{"update"},
					Before: map[string]interface{}{
						"ingress_rules": []string{"80"},
					},
					After: map[string]interface{}{
						"ingress_rules": []string{"80", "443"},
					},
				},
			},
		},
	}

	jsonBytes, _ := json.MarshalIndent(plan, "  ", "  ")
	fmt.Printf("  %s\n", string(jsonBytes))
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Terraform 내부 유틸리티 시뮬레이션 PoC                      ║")
	fmt.Println("║  실제 소스: internal/promising/, namedvals/, instances/      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	demoPromising()
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	demoOnce()
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	demoNamedValues()
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	demoInstanceExpander()
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	demoJSONPlan()
}
