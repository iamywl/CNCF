package main

import (
	"fmt"
	"sort"
	"strings"
)

// =============================================================================
// Terraform moved/removed 블록 처리 시뮬레이션
// =============================================================================
// Terraform 1.1+에서 도입된 moved 블록은 리소스 이름 변경이나 모듈 이동 시
// 상태를 자동으로 업데이트하는 기능입니다.
//
// 실제 코드: internal/refactoring/ 디렉토리
//
// moved {
//   from = aws_instance.old
//   to   = aws_instance.new
// }
//
// removed {   # Terraform 1.7+
//   from = aws_instance.legacy
//   lifecycle { destroy = false }
// }
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// 주소 타입
// ─────────────────────────────────────────────────────────────────────────────

// Address는 리소스 주소를 나타냅니다.
type Address struct {
	Module   string // 모듈 경로 (빈 문자열 = 루트 모듈)
	Type     string // 리소스 타입
	Name     string // 리소스 이름
	Key      string // 인스턴스 키 (빈 문자열 = NoKey)
}

func NewAddress(fullAddr string) Address {
	addr := Address{}

	remaining := fullAddr

	// 모듈 경로 추출
	for strings.HasPrefix(remaining, "module.") {
		dotIdx := strings.Index(remaining[7:], ".")
		if dotIdx < 0 {
			break
		}
		moduleName := remaining[7 : 7+dotIdx]
		if addr.Module != "" {
			addr.Module += ".module." + moduleName
		} else {
			addr.Module = "module." + moduleName
		}
		remaining = remaining[7+dotIdx+1:]
	}

	// 인스턴스 키 추출
	if bracketIdx := strings.Index(remaining, "["); bracketIdx >= 0 {
		addr.Key = remaining[bracketIdx:]
		remaining = remaining[:bracketIdx]
	}

	// 타입.이름 추출
	parts := strings.SplitN(remaining, ".", 2)
	if len(parts) == 2 {
		addr.Type = parts[0]
		addr.Name = parts[1]
	}

	return addr
}

func (a Address) String() string {
	var result string
	if a.Module != "" {
		result = a.Module + "."
	}
	result += a.Type + "." + a.Name + a.Key
	return result
}

func (a Address) ResourceAddr() string {
	var result string
	if a.Module != "" {
		result = a.Module + "."
	}
	result += a.Type + "." + a.Name
	return result
}

// ─────────────────────────────────────────────────────────────────────────────
// MoveStatement: moved 블록
// ─────────────────────────────────────────────────────────────────────────────

// MoveStatement는 moved 블록을 나타냅니다.
// 실제: internal/refactoring/move_statement.go
type MoveStatement struct {
	From    Address
	To      Address
	Comment string
}

func (m MoveStatement) String() string {
	return fmt.Sprintf("moved { from = %s, to = %s }", m.From.String(), m.To.String())
}

// ─────────────────────────────────────────────────────────────────────────────
// RemoveStatement: removed 블록 (Terraform 1.7+)
// ─────────────────────────────────────────────────────────────────────────────

// RemoveStatement는 removed 블록을 나타냅니다.
// 실제: internal/refactoring/remove_statement.go
type RemoveStatement struct {
	From    Address
	Destroy bool   // true: 리소스도 삭제, false: 상태에서만 제거
	Comment string
}

func (r RemoveStatement) String() string {
	destroyStr := "false"
	if r.Destroy {
		destroyStr = "true"
	}
	return fmt.Sprintf("removed { from = %s, lifecycle { destroy = %s } }", r.From.String(), destroyStr)
}

// ─────────────────────────────────────────────────────────────────────────────
// State: 상태
// ─────────────────────────────────────────────────────────────────────────────

// ResourceState는 상태에 저장된 리소스입니다.
type ResourceState struct {
	Address    string
	Type       string
	Name       string
	Provider   string
	Attributes map[string]string
}

// State는 Terraform 상태를 나타냅니다.
type State struct {
	Resources map[string]*ResourceState
}

func NewState() *State {
	return &State{
		Resources: make(map[string]*ResourceState),
	}
}

func (s *State) AddResource(addr string, attrs map[string]string) {
	a := NewAddress(addr)
	s.Resources[addr] = &ResourceState{
		Address:    addr,
		Type:       a.Type,
		Name:       a.Name,
		Provider:   fmt.Sprintf("registry.terraform.io/hashicorp/%s", strings.Split(a.Type, "_")[0]),
		Attributes: attrs,
	}
}

func (s *State) RemoveResource(addr string) {
	delete(s.Resources, addr)
}

func (s *State) HasResource(addr string) bool {
	_, ok := s.Resources[addr]
	return ok
}

func (s *State) Print(title string) {
	fmt.Printf("  [%s] (리소스 %d개)\n", title, len(s.Resources))
	addrs := make([]string, 0, len(s.Resources))
	for addr := range s.Resources {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)

	for _, addr := range addrs {
		res := s.Resources[addr]
		fmt.Printf("    - %-45s (type=%s)\n", addr, res.Type)
		for k, v := range res.Attributes {
			fmt.Printf("        %s = %q\n", k, v)
		}
	}
	fmt.Println()
}

// ─────────────────────────────────────────────────────────────────────────────
// Move 처리기
// ─────────────────────────────────────────────────────────────────────────────

// MoveResult는 이동 작업의 결과입니다.
type MoveResult struct {
	From    string
	To      string
	Success bool
	Message string
}

// MoveProcessor는 moved/removed 블록을 처리합니다.
// 실제: internal/refactoring/move_execute.go
type MoveProcessor struct {
	moves   []MoveStatement
	removes []RemoveStatement
}

func NewMoveProcessor() *MoveProcessor {
	return &MoveProcessor{}
}

func (p *MoveProcessor) AddMove(from, to string, comment string) {
	p.moves = append(p.moves, MoveStatement{
		From:    NewAddress(from),
		To:      NewAddress(to),
		Comment: comment,
	})
}

func (p *MoveProcessor) AddRemove(from string, destroy bool, comment string) {
	p.removes = append(p.removes, RemoveStatement{
		From:    NewAddress(from),
		Destroy: destroy,
		Comment: comment,
	})
}

// ValidateMoves는 이동 명세의 유효성을 검증합니다.
// 실제: internal/refactoring/move_validate.go
func (p *MoveProcessor) ValidateMoves() []string {
	var errors []string

	// 1. 대상 충돌 검사: 두 개의 move가 같은 대상을 가질 수 없음
	targets := make(map[string]int)
	for i, m := range p.moves {
		to := m.To.String()
		if prevIdx, ok := targets[to]; ok {
			errors = append(errors, fmt.Sprintf(
				"대상 충돌: move[%d](%s → %s)와 move[%d]이(가) 같은 대상 '%s'을 가집니다",
				i, m.From.String(), m.To.String(), prevIdx, to))
		}
		targets[to] = i
	}

	// 2. 순환 참조 검사
	// A→B, B→A 같은 순환이 있는지 확인
	moveMap := make(map[string]string)
	for _, m := range p.moves {
		moveMap[m.From.String()] = m.To.String()
	}

	for start := range moveMap {
		visited := make(map[string]bool)
		current := start
		for {
			if visited[current] {
				if current == start {
					errors = append(errors, fmt.Sprintf(
						"순환 참조 감지: '%s'에서 시작하는 이동 체인이 순환합니다", start))
				}
				break
			}
			visited[current] = true
			next, ok := moveMap[current]
			if !ok {
				break
			}
			current = next
		}
	}

	return errors
}

// ResolveMoveChains는 이동 체인을 해석합니다.
// A→B, B→C → 효과적으로 A→C
// 실제: internal/refactoring/move_execute.go
func (p *MoveProcessor) ResolveMoveChains() []MoveStatement {
	// 이동 맵 구성
	moveMap := make(map[string]string)
	commentMap := make(map[string]string)

	for _, m := range p.moves {
		moveMap[m.From.String()] = m.To.String()
		commentMap[m.From.String()] = m.Comment
	}

	// 체인 해석: 최종 목적지 찾기
	var resolved []MoveStatement
	processed := make(map[string]bool)

	for _, m := range p.moves {
		from := m.From.String()
		if processed[from] {
			continue
		}

		// 이 from이 다른 move의 to인 경우 건너뛰기 (체인의 중간)
		isIntermediate := false
		for _, other := range p.moves {
			if other.To.String() == from {
				isIntermediate = true
				break
			}
		}
		if isIntermediate {
			continue
		}

		// 최종 목적지 추적
		current := from
		var chain []string
		chain = append(chain, current)

		for {
			next, ok := moveMap[current]
			if !ok {
				break
			}
			chain = append(chain, next)
			current = next
		}

		if len(chain) > 1 {
			finalTo := chain[len(chain)-1]
			comment := fmt.Sprintf("체인 해석: %s", strings.Join(chain, " -> "))
			resolved = append(resolved, MoveStatement{
				From:    NewAddress(from),
				To:      NewAddress(finalTo),
				Comment: comment,
			})
		}

		processed[from] = true
	}

	return resolved
}

// Execute는 이동 및 제거 작업을 상태에 적용합니다.
func (p *MoveProcessor) Execute(state *State) []MoveResult {
	var results []MoveResult

	// 1. 이동 체인 해석
	resolvedMoves := p.ResolveMoveChains()

	// 2. 이동 실행
	for _, m := range resolvedMoves {
		from := m.From.String()
		to := m.To.String()

		if !state.HasResource(from) {
			results = append(results, MoveResult{
				From:    from,
				To:      to,
				Success: false,
				Message: fmt.Sprintf("소스 '%s'이(가) 상태에 없습니다", from),
			})
			continue
		}

		if state.HasResource(to) {
			results = append(results, MoveResult{
				From:    from,
				To:      to,
				Success: false,
				Message: fmt.Sprintf("대상 '%s'이(가) 이미 상태에 존재합니다", to),
			})
			continue
		}

		// 이동 실행
		res := state.Resources[from]
		newAddr := NewAddress(to)
		res.Address = to
		res.Name = newAddr.Name
		state.Resources[to] = res
		state.RemoveResource(from)

		results = append(results, MoveResult{
			From:    from,
			To:      to,
			Success: true,
			Message: m.Comment,
		})
	}

	// 3. 제거 실행
	for _, r := range p.removes {
		from := r.From.String()

		if !state.HasResource(from) {
			results = append(results, MoveResult{
				From:    from,
				To:      "(removed)",
				Success: false,
				Message: fmt.Sprintf("제거 대상 '%s'이(가) 상태에 없습니다", from),
			})
			continue
		}

		action := "상태에서 제거 (인프라 유지)"
		if r.Destroy {
			action = "상태에서 제거 + 인프라 삭제 예정"
		}

		state.RemoveResource(from)
		results = append(results, MoveResult{
			From:    from,
			To:      "(removed)",
			Success: true,
			Message: fmt.Sprintf("%s - %s", action, r.Comment),
		})
	}

	return results
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

func printResults(results []MoveResult) {
	for _, r := range results {
		status := "OK"
		if !r.Success {
			status = "FAIL"
		}
		if r.To == "(removed)" {
			fmt.Printf("    [%s] REMOVE %s\n", status, r.From)
		} else {
			fmt.Printf("    [%s] MOVE %s → %s\n", status, r.From, r.To)
		}
		if r.Message != "" {
			fmt.Printf("          %s\n", r.Message)
		}
	}
	fmt.Println()
}

// ─────────────────────────────────────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Terraform moved/removed 블록 처리 시뮬레이션              ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  리소스 이름 변경, 모듈 이동, 상태 제거를 자동 처리                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// ─── 예제 1: 단순 리소스 이름 변경 ───
	printSeparator("1. 단순 리소스 이름 변경")
	fmt.Println("  moved {")
	fmt.Println("    from = aws_instance.old_web")
	fmt.Println("    to   = aws_instance.web_server")
	fmt.Println("  }")
	fmt.Println()

	state1 := NewState()
	state1.AddResource("aws_instance.old_web", map[string]string{
		"id": "i-abc123", "ami": "ami-12345", "instance_type": "t2.micro",
	})
	state1.AddResource("aws_vpc.main", map[string]string{
		"id": "vpc-xyz789", "cidr_block": "10.0.0.0/16",
	})

	state1.Print("이동 전 상태")

	proc1 := NewMoveProcessor()
	proc1.AddMove("aws_instance.old_web", "aws_instance.web_server", "리소스 이름 변경")

	results1 := proc1.Execute(state1)
	printResults(results1)
	state1.Print("이동 후 상태")

	// ─── 예제 2: 이동 체인 (A→B, B→C) ───
	printSeparator("2. 이동 체인 (A→B, B→C → 실질적으로 A→C)")
	fmt.Println("  moved { from = aws_instance.v1, to = aws_instance.v2 }")
	fmt.Println("  moved { from = aws_instance.v2, to = aws_instance.v3 }")
	fmt.Println()

	state2 := NewState()
	state2.AddResource("aws_instance.v1", map[string]string{
		"id": "i-chain-1", "instance_type": "t2.micro",
	})

	state2.Print("이동 전 상태")

	proc2 := NewMoveProcessor()
	proc2.AddMove("aws_instance.v1", "aws_instance.v2", "v1→v2 (첫 번째 리팩토링)")
	proc2.AddMove("aws_instance.v2", "aws_instance.v3", "v2→v3 (두 번째 리팩토링)")

	// 체인 해석 표시
	resolved := proc2.ResolveMoveChains()
	fmt.Println("  해석된 이동 체인:")
	for _, m := range resolved {
		fmt.Printf("    %s → %s (%s)\n", m.From.String(), m.To.String(), m.Comment)
	}
	fmt.Println()

	results2 := proc2.Execute(state2)
	printResults(results2)
	state2.Print("이동 후 상태")

	// ─── 예제 3: 대상 충돌 감지 ───
	printSeparator("3. 대상 충돌 감지")
	fmt.Println("  moved { from = aws_instance.a, to = aws_instance.target }")
	fmt.Println("  moved { from = aws_instance.b, to = aws_instance.target }  # 충돌!")
	fmt.Println()

	proc3 := NewMoveProcessor()
	proc3.AddMove("aws_instance.a", "aws_instance.target", "a → target")
	proc3.AddMove("aws_instance.b", "aws_instance.target", "b → target (충돌)")

	errors3 := proc3.ValidateMoves()
	if len(errors3) > 0 {
		fmt.Println("  검증 오류:")
		for _, err := range errors3 {
			fmt.Printf("    - %s\n", err)
		}
	} else {
		fmt.Println("  검증 통과")
	}

	// ─── 예제 4: 교차 모듈 이동 ───
	printSeparator("4. 교차 모듈 이동")
	fmt.Println("  # 루트 모듈에서 하위 모듈로 리소스 이동")
	fmt.Println("  moved {")
	fmt.Println("    from = aws_subnet.public")
	fmt.Println("    to   = module.network.aws_subnet.public")
	fmt.Println("  }")
	fmt.Println()

	state4 := NewState()
	state4.AddResource("aws_vpc.main", map[string]string{
		"id": "vpc-main-1", "cidr_block": "10.0.0.0/16",
	})
	state4.AddResource("aws_subnet.public", map[string]string{
		"id": "subnet-pub-1", "vpc_id": "vpc-main-1", "cidr_block": "10.0.1.0/24",
	})
	state4.AddResource("aws_subnet.private", map[string]string{
		"id": "subnet-priv-1", "vpc_id": "vpc-main-1", "cidr_block": "10.0.2.0/24",
	})

	state4.Print("이동 전 상태")

	proc4 := NewMoveProcessor()
	proc4.AddMove("aws_subnet.public", "module.network.aws_subnet.public", "루트→모듈 이동")
	proc4.AddMove("aws_subnet.private", "module.network.aws_subnet.private", "루트→모듈 이동")

	results4 := proc4.Execute(state4)
	printResults(results4)
	state4.Print("이동 후 상태")

	// ─── 예제 5: 모듈 간 이동 ───
	printSeparator("5. 모듈 간 이동")
	fmt.Println("  # module.old_network → module.new_network")
	fmt.Println()

	state5 := NewState()
	state5.AddResource("module.old_network.aws_vpc.main", map[string]string{
		"id": "vpc-old-1",
	})
	state5.AddResource("module.old_network.aws_subnet.web", map[string]string{
		"id": "subnet-old-1",
	})
	state5.AddResource("aws_instance.web", map[string]string{
		"id": "i-web-1",
	})

	state5.Print("이동 전 상태")

	proc5 := NewMoveProcessor()
	proc5.AddMove("module.old_network.aws_vpc.main", "module.new_network.aws_vpc.main", "모듈 이름 변경")
	proc5.AddMove("module.old_network.aws_subnet.web", "module.new_network.aws_subnet.web", "모듈 이름 변경")

	results5 := proc5.Execute(state5)
	printResults(results5)
	state5.Print("이동 후 상태")

	// ─── 예제 6: removed 블록 ───
	printSeparator("6. removed 블록 (상태에서 제거)")
	fmt.Println("  removed {")
	fmt.Println("    from = aws_instance.legacy")
	fmt.Println("    lifecycle {")
	fmt.Println("      destroy = false  # 인프라는 유지, 상태에서만 제거")
	fmt.Println("    }")
	fmt.Println("  }")
	fmt.Println()

	state6 := NewState()
	state6.AddResource("aws_instance.web", map[string]string{
		"id": "i-web-1", "instance_type": "t2.micro",
	})
	state6.AddResource("aws_instance.legacy", map[string]string{
		"id": "i-legacy-1", "instance_type": "t2.small",
	})
	state6.AddResource("aws_instance.deprecated", map[string]string{
		"id": "i-dep-1", "instance_type": "t2.nano",
	})

	state6.Print("제거 전 상태")

	proc6 := NewMoveProcessor()
	proc6.AddRemove("aws_instance.legacy", false, "레거시 리소스 - 인프라 유지")
	proc6.AddRemove("aws_instance.deprecated", true, "폐기된 리소스 - 인프라도 삭제")

	results6 := proc6.Execute(state6)
	printResults(results6)
	state6.Print("제거 후 상태")

	// ─── 예제 7: 복합 시나리오 (이동 + 제거) ───
	printSeparator("7. 복합 시나리오 (이동 + 이동 체인 + 제거)")
	fmt.Println("  # 대규모 리팩토링: 이름 변경 + 모듈 이동 + 레거시 제거")
	fmt.Println()

	state7 := NewState()
	state7.AddResource("aws_instance.app_v1", map[string]string{
		"id": "i-app-v1",
	})
	state7.AddResource("aws_instance.db_old", map[string]string{
		"id": "i-db-old",
	})
	state7.AddResource("aws_instance.cache", map[string]string{
		"id": "i-cache-1",
	})
	state7.AddResource("aws_instance.monitoring", map[string]string{
		"id": "i-mon-1",
	})
	state7.AddResource("aws_instance.temp_worker", map[string]string{
		"id": "i-temp-1",
	})

	state7.Print("변경 전 상태")

	proc7 := NewMoveProcessor()
	// 이름 변경
	proc7.AddMove("aws_instance.app_v1", "aws_instance.application", "애플리케이션 이름 정리")
	// 모듈로 이동
	proc7.AddMove("aws_instance.db_old", "module.database.aws_instance.primary", "DB를 전용 모듈로 이동")
	proc7.AddMove("aws_instance.cache", "module.cache.aws_instance.redis", "캐시를 전용 모듈로 이동")
	// 제거
	proc7.AddRemove("aws_instance.temp_worker", false, "임시 워커 - 수동 관리로 전환")

	results7 := proc7.Execute(state7)
	printResults(results7)
	state7.Print("변경 후 상태")

	// ─── 예제 8: 존재하지 않는 소스 처리 ───
	printSeparator("8. 이미 이동된 리소스 처리 (멱등성)")
	fmt.Println("  # 이미 이동이 완료된 상태에서 동일한 moved 블록 재실행")
	fmt.Println()

	state8 := NewState()
	state8.AddResource("aws_instance.new_name", map[string]string{
		"id": "i-already-moved",
	})

	state8.Print("현재 상태 (이미 이동 완료)")

	proc8 := NewMoveProcessor()
	proc8.AddMove("aws_instance.old_name", "aws_instance.new_name", "이미 완료된 이동")

	results8 := proc8.Execute(state8)
	printResults(results8)
	fmt.Println("  → 소스가 없으므로 이동이 건너뛰어짐 (멱등성 보장)")

	// ─── 아키텍처 요약 ───
	printSeparator("moved/removed 블록 아키텍처 요약")
	fmt.Print(`
  moved 블록 처리 흐름:

  ┌──────────────────┐     ┌──────────────────┐     ┌──────────────────┐
  │  HCL 파싱        │────▶│  검증 (Validate) │────▶│  체인 해석       │
  │                  │     │                  │     │                  │
  │  moved {         │     │  - 대상 충돌     │     │  A→B, B→C       │
  │    from = ...    │     │  - 순환 참조     │     │  → A→C (체인)   │
  │    to   = ...    │     │  - 타입 호환성   │     │                  │
  │  }               │     │                  │     │                  │
  └──────────────────┘     └──────────────────┘     └──────────────────┘
                                                            │
                                                            ▼
  ┌──────────────────┐     ┌──────────────────┐     ┌──────────────────┐
  │  상태 업데이트    │◀────│  이동 실행       │◀────│  상태 매칭       │
  │                  │     │                  │     │                  │
  │  State에 반영    │     │  - 소스 존재 확인│     │  from이 상태에   │
  │  Plan에 표시     │     │  - 대상 비충돌   │     │  있는지 확인     │
  │                  │     │  - 주소 변경     │     │                  │
  └──────────────────┘     └──────────────────┘     └──────────────────┘

  이동 체인 해석:

    moved { from = A, to = B }     A ─→ B ─→ C
    moved { from = B, to = C }     해석: A ─→ C (B는 중간 단계)

  removed 블록:

    removed {
      from = aws_instance.legacy
      lifecycle {
        destroy = false   # 인프라 유지, 상태에서만 제거
        destroy = true    # 인프라도 함께 삭제
      }
    }

  멱등성:
    - 소스가 상태에 없으면 이동 건너뛰기
    - 여러 번 apply해도 동일한 결과
    - plan 단계에서 이동 결과 미리 표시

  실제 코드:
    internal/refactoring/move_statement.go    # MoveStatement 정의
    internal/refactoring/move_validate.go     # 유효성 검증
    internal/refactoring/move_execute.go      # 실행 로직
    internal/refactoring/remove_statement.go  # RemoveStatement 정의
`)
}
