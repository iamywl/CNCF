package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// =============================================================================
// PoC-09: 트리 기반 서브커맨드 파싱 시뮬레이션
// =============================================================================
// tart의 Root.swift → AsyncParsableCommand 구조를 Go로 재현한다.
// Swift ArgumentParser의 핵심 개념:
//   - CommandConfiguration: commandName, version, subcommands 트리
//   - @Argument, @Option, @Flag 데코레이터 유사 파싱
//   - validate() → run() 라이프사이클
//   - 자동 도움말 생성
//   - 셸 자동완성 시뮬레이션
// =============================================================================

// ---------------------------------------------------------------------------
// 1. 파라미터 정의 — @Argument, @Option, @Flag 유사 구조
// ---------------------------------------------------------------------------

// ParamType은 Swift ArgumentParser의 @Argument, @Option, @Flag에 대응한다.
type ParamType int

const (
	ParamArgument ParamType = iota // 위치 기반 인자 (@Argument)
	ParamOption                    // 이름 기반 옵션 (@Option)
	ParamFlag                      // 불리언 플래그 (@Flag)
)

// ParamDef는 하나의 커맨드 파라미터 정의이다.
// tart에서 @Argument(help:), @Option(help:), @Flag(help:)에 대응한다.
type ParamDef struct {
	Name         string    // 파라미터 이름 (예: "name", "concurrency")
	Type         ParamType // Argument / Option / Flag
	Help         string    // 도움말 문자열
	DefaultValue string    // 기본값 (빈 문자열이면 필수)
	Required     bool      // 필수 여부
}

// ParsedParams는 파싱 결과를 저장한다.
type ParsedParams struct {
	Arguments map[string]string // 위치 인자 결과
	Options   map[string]string // --key=value 옵션 결과
	Flags     map[string]bool   // --flag 불리언 결과
}

// NewParsedParams는 빈 파싱 결과를 생성한다.
func NewParsedParams() *ParsedParams {
	return &ParsedParams{
		Arguments: make(map[string]string),
		Options:   make(map[string]string),
		Flags:     make(map[string]bool),
	}
}

// ---------------------------------------------------------------------------
// 2. Command 인터페이스 — AsyncParsableCommand 유사 구조
// ---------------------------------------------------------------------------

// CommandConfiguration은 Swift ArgumentParser의 CommandConfiguration에 대응한다.
// tart Root.swift의 static var configuration = CommandConfiguration(...)과 동일한 역할.
type CommandConfiguration struct {
	CommandName string     // 커맨드 이름 (예: "tart", "clone", "run")
	Abstract    string     // 간단한 설명
	Discussion  string     // 상세 설명
	Version     string     // 버전 정보
	Subcommands []Command  // 하위 커맨드 트리
}

// Command는 Swift의 AsyncParsableCommand 프로토콜에 대응한다.
// tart의 모든 커맨드(Create, Clone, Run, Pull 등)가 이 인터페이스를 구현한다.
type Command interface {
	Configuration() CommandConfiguration
	Params() []ParamDef
	Validate(params *ParsedParams) error
	Run(params *ParsedParams) error
}

// ---------------------------------------------------------------------------
// 3. 커맨드 구현 — tart의 실제 서브커맨드를 시뮬레이션
// ---------------------------------------------------------------------------

// RootCommand는 tart의 Root 구조체에 대응한다.
// Root.swift: @main struct Root: AsyncParsableCommand
type RootCommand struct{}

func (c *RootCommand) Configuration() CommandConfiguration {
	return CommandConfiguration{
		CommandName: "tart",
		Abstract:    "macOS 및 Linux VM 관리 도구",
		Version:     "2.22.4",
		Subcommands: []Command{
			&CloneCommand{},
			&RunCommand{},
			&PullCommand{},
			&ListCommand{},
			&PruneCommand{},
		},
	}
}
func (c *RootCommand) Params() []ParamDef    { return nil }
func (c *RootCommand) Validate(_ *ParsedParams) error { return nil }
func (c *RootCommand) Run(_ *ParsedParams) error      { return nil }

// CloneCommand는 tart의 Clone 구조체에 대응한다.
// Clone.swift: struct Clone: AsyncParsableCommand
type CloneCommand struct{}

func (c *CloneCommand) Configuration() CommandConfiguration {
	return CommandConfiguration{
		CommandName: "clone",
		Abstract:    "VM을 복제합니다",
		Discussion: "APFS copy-on-write를 활용하여 로컬 또는 원격 VM을 복제합니다.\n" +
			"복제된 VM은 즉시 전체 공간을 차지하지 않으며, 변경분만 기록됩니다.",
	}
}

func (c *CloneCommand) Params() []ParamDef {
	// Clone.swift의 @Argument, @Option, @Flag 정의에 대응
	return []ParamDef{
		{Name: "source", Type: ParamArgument, Help: "원본 VM 이름", Required: true},
		{Name: "new-name", Type: ParamArgument, Help: "새 VM 이름", Required: true},
		{Name: "insecure", Type: ParamFlag, Help: "HTTP 프로토콜로 OCI 레지스트리 접속"},
		{Name: "concurrency", Type: ParamOption, Help: "네트워크 동시성 수준", DefaultValue: "4"},
		{Name: "prune-limit", Type: ParamOption, Help: "자동 프루닝 최대 크기(GB)", DefaultValue: "100"},
	}
}

func (c *CloneCommand) Validate(params *ParsedParams) error {
	// Clone.swift validate(): newName에 "/" 포함 불가
	if newName, ok := params.Arguments["new-name"]; ok {
		if strings.Contains(newName, "/") {
			return fmt.Errorf("유효성 오류: <new-name>은 로컬 이름이어야 합니다")
		}
	}
	// concurrency >= 1 검증
	if conc, ok := params.Options["concurrency"]; ok {
		if conc == "0" {
			return fmt.Errorf("유효성 오류: 네트워크 동시성은 1 이상이어야 합니다")
		}
	}
	return nil
}

func (c *CloneCommand) Run(params *ParsedParams) error {
	src := params.Arguments["source"]
	dst := params.Arguments["new-name"]
	conc := params.Options["concurrency"]
	insecure := params.Flags["insecure"]
	fmt.Printf("[Clone] 실행: %s → %s (동시성: %s, insecure: %v)\n", src, dst, conc, insecure)
	fmt.Println("  1. OCI 스토리지에서 원본 확인")
	fmt.Println("  2. 임시 VMDirectory 생성")
	fmt.Println("  3. FileLock으로 전역 잠금 획득")
	fmt.Println("  4. APFS copy-on-write 클론 수행")
	fmt.Println("  5. MAC 주소 재생성 (충돌 방지)")
	fmt.Println("  6. Prune.reclaimIfNeeded() 호출")
	fmt.Println("  [완료] VM 복제 성공")
	return nil
}

// RunCommand는 tart의 Run 구조체에 대응한다.
type RunCommand struct{}

func (c *RunCommand) Configuration() CommandConfiguration {
	return CommandConfiguration{
		CommandName: "run",
		Abstract:    "VM을 실행합니다",
	}
}

func (c *RunCommand) Params() []ParamDef {
	return []ParamDef{
		{Name: "name", Type: ParamArgument, Help: "VM 이름", Required: true},
		{Name: "no-graphics", Type: ParamFlag, Help: "그래픽 없이 실행"},
		{Name: "cpu", Type: ParamOption, Help: "CPU 코어 수"},
		{Name: "memory", Type: ParamOption, Help: "메모리 크기 (MB)"},
	}
}

func (c *RunCommand) Validate(params *ParsedParams) error { return nil }
func (c *RunCommand) Run(params *ParsedParams) error {
	name := params.Arguments["name"]
	noGraphics := params.Flags["no-graphics"]
	fmt.Printf("[Run] 실행: %s (no-graphics: %v)\n", name, noGraphics)
	fmt.Println("  VM 시작 완료 (시뮬레이션)")
	return nil
}

// PullCommand는 tart의 Pull 구조체에 대응한다.
type PullCommand struct{}

func (c *PullCommand) Configuration() CommandConfiguration {
	return CommandConfiguration{
		CommandName: "pull",
		Abstract:    "OCI 레지스트리에서 VM 이미지를 가져옵니다",
	}
}

func (c *PullCommand) Params() []ParamDef {
	return []ParamDef{
		{Name: "name", Type: ParamArgument, Help: "원격 VM 이름 (예: ghcr.io/org/image:tag)", Required: true},
		{Name: "insecure", Type: ParamFlag, Help: "HTTP 프로토콜 사용"},
		{Name: "concurrency", Type: ParamOption, Help: "네트워크 동시성", DefaultValue: "4"},
	}
}

func (c *PullCommand) Validate(params *ParsedParams) error { return nil }
func (c *PullCommand) Run(params *ParsedParams) error {
	name := params.Arguments["name"]
	fmt.Printf("[Pull] 실행: %s 이미지 가져오기\n", name)
	return nil
}

// ListCommand는 tart의 List 구조체에 대응한다.
type ListCommand struct{}

func (c *ListCommand) Configuration() CommandConfiguration {
	return CommandConfiguration{
		CommandName: "list",
		Abstract:    "VM 목록을 표시합니다",
	}
}

func (c *ListCommand) Params() []ParamDef {
	return []ParamDef{
		{Name: "source", Type: ParamOption, Help: "소스 필터 (local/oci)", DefaultValue: "local"},
		{Name: "quiet", Type: ParamFlag, Help: "이름만 표시"},
	}
}

func (c *ListCommand) Validate(params *ParsedParams) error { return nil }
func (c *ListCommand) Run(params *ParsedParams) error {
	src := params.Options["source"]
	quiet := params.Flags["quiet"]
	fmt.Printf("[List] 실행: source=%s, quiet=%v\n", src, quiet)
	return nil
}

// PruneCommand는 tart의 Prune 구조체에 대응한다.
type PruneCommand struct{}

func (c *PruneCommand) Configuration() CommandConfiguration {
	return CommandConfiguration{
		CommandName: "prune",
		Abstract:    "OCI 및 IPSW 캐시를 정리합니다",
	}
}

func (c *PruneCommand) Params() []ParamDef {
	// Prune.swift의 @Option, @Flag 정의에 대응
	return []ParamDef{
		{Name: "entries", Type: ParamOption, Help: "정리 대상: caches 또는 vms", DefaultValue: "caches"},
		{Name: "older-than", Type: ParamOption, Help: "n일 이전 항목 삭제"},
		{Name: "space-budget", Type: ParamOption, Help: "공간 예산(GB) 초과 항목 삭제"},
		{Name: "gc", Type: ParamFlag, Help: "가비지 컬렉션 수행"},
	}
}

func (c *PruneCommand) Validate(params *ParsedParams) error {
	// Prune.swift validate(): 최소 하나의 기준 필요
	_, hasOlderThan := params.Options["older-than"]
	_, hasSpaceBudget := params.Options["space-budget"]
	gcFlag := params.Flags["gc"]
	if !hasOlderThan && !hasSpaceBudget && !gcFlag {
		return fmt.Errorf("유효성 오류: 최소 하나의 프루닝 기준을 지정해야 합니다")
	}
	return nil
}

func (c *PruneCommand) Run(params *ParsedParams) error {
	fmt.Println("[Prune] 캐시 정리 수행")
	return nil
}

// ---------------------------------------------------------------------------
// 4. 파서 엔진 — parseAsRoot() + 서브커맨드 라우팅
// ---------------------------------------------------------------------------

// Parser는 Swift ArgumentParser의 핵심 파싱 로직을 시뮬레이션한다.
// tart Root.swift의 parseAsRoot() → command.run() 흐름을 재현한다.
type Parser struct {
	Root Command
}

// parseArgs는 os.Args를 파싱하여 서브커맨드를 찾고, 파라미터를 추출한다.
// tart의 parseAsRoot()가 내부적으로 수행하는 작업:
//   1. 첫 번째 인자로 서브커맨드 매칭
//   2. --option=value, --flag, positional 인자 분류
//   3. 기본값 적용
func (p *Parser) parseArgs(args []string) (Command, *ParsedParams, error) {
	if len(args) == 0 {
		return nil, nil, fmt.Errorf("사용법: %s <subcommand> [옵션...]", p.Root.Configuration().CommandName)
	}

	// --version 처리
	if args[0] == "--version" {
		conf := p.Root.Configuration()
		fmt.Printf("%s version %s\n", conf.CommandName, conf.Version)
		os.Exit(0)
	}

	// --help 또는 help 처리
	if args[0] == "--help" || args[0] == "help" || args[0] == "-h" {
		p.printRootHelp()
		os.Exit(0)
	}

	// 서브커맨드 매칭 — Configuration().Subcommands에서 CommandName으로 탐색
	subcommandName := args[0]
	var matched Command
	for _, sub := range p.Root.Configuration().Subcommands {
		if sub.Configuration().CommandName == subcommandName {
			matched = sub
			break
		}
	}

	if matched == nil {
		return nil, nil, fmt.Errorf("알 수 없는 서브커맨드: '%s'\n사용 가능: %s",
			subcommandName, p.availableSubcommands())
	}

	// 서브커맨드별 --help 처리
	remaining := args[1:]
	for _, a := range remaining {
		if a == "--help" || a == "-h" {
			p.printCommandHelp(matched)
			os.Exit(0)
		}
	}

	// 파라미터 파싱
	params, err := p.parseParams(matched.Params(), remaining)
	if err != nil {
		return nil, nil, err
	}

	return matched, params, nil
}

// parseParams는 커맨드의 ParamDef 목록과 인자 슬라이스를 받아 파싱한다.
// Swift ArgumentParser가 @Argument, @Option, @Flag을 처리하는 방식을 재현한다.
func (p *Parser) parseParams(defs []ParamDef, args []string) (*ParsedParams, error) {
	result := NewParsedParams()

	// 기본값 적용
	for _, def := range defs {
		if def.DefaultValue != "" {
			switch def.Type {
			case ParamOption:
				result.Options[def.Name] = def.DefaultValue
			}
		}
		if def.Type == ParamFlag {
			result.Flags[def.Name] = false
		}
	}

	// 인자 파싱
	argDefs := []ParamDef{} // 위치 인자 정의 순서
	for _, def := range defs {
		if def.Type == ParamArgument {
			argDefs = append(argDefs, def)
		}
	}

	argIdx := 0
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			// --option=value 또는 --flag 처리
			key := strings.TrimPrefix(a, "--")
			// = 포함 여부 확인
			if eqIdx := strings.Index(key, "="); eqIdx >= 0 {
				optName := key[:eqIdx]
				optVal := key[eqIdx+1:]
				result.Options[optName] = optVal
				i++
				continue
			}
			// 플래그인지 옵션인지 판별
			isFlag := false
			for _, def := range defs {
				if def.Name == key && def.Type == ParamFlag {
					isFlag = true
					break
				}
			}
			if isFlag {
				result.Flags[key] = true
				i++
			} else {
				// 다음 인자가 값
				if i+1 < len(args) {
					result.Options[key] = args[i+1]
					i += 2
				} else {
					return nil, fmt.Errorf("옵션 --%s에 값이 필요합니다", key)
				}
			}
		} else {
			// 위치 인자
			if argIdx < len(argDefs) {
				result.Arguments[argDefs[argIdx].Name] = a
				argIdx++
			}
			i++
		}
	}

	// 필수 위치 인자 검증
	for _, def := range defs {
		if def.Type == ParamArgument && def.Required {
			if _, ok := result.Arguments[def.Name]; !ok {
				return nil, fmt.Errorf("필수 인자 <%s>가 누락되었습니다", def.Name)
			}
		}
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// 5. 도움말 자동 생성
// ---------------------------------------------------------------------------

// printRootHelp는 tart --help 출력을 시뮬레이션한다.
// Swift ArgumentParser가 자동으로 생성하는 도움말 형식을 따른다.
func (p *Parser) printRootHelp() {
	conf := p.Root.Configuration()
	fmt.Printf("개요: %s — %s\n\n", conf.CommandName, conf.Abstract)
	fmt.Printf("사용법: %s <subcommand> [옵션...]\n\n", conf.CommandName)
	fmt.Println("서브커맨드:")

	// 서브커맨드 목록을 정렬하여 출력
	type cmdInfo struct {
		name     string
		abstract string
	}
	var cmds []cmdInfo
	maxLen := 0
	for _, sub := range conf.Subcommands {
		sc := sub.Configuration()
		cmds = append(cmds, cmdInfo{sc.CommandName, sc.Abstract})
		if len(sc.CommandName) > maxLen {
			maxLen = len(sc.CommandName)
		}
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].name < cmds[j].name })
	for _, c := range cmds {
		fmt.Printf("  %-*s  %s\n", maxLen+2, c.name, c.abstract)
	}
	fmt.Printf("\n옵션:\n  --help, -h     도움말 표시\n  --version      버전 표시 (%s)\n", conf.Version)
}

// printCommandHelp는 특정 서브커맨드의 도움말을 출력한다.
func (p *Parser) printCommandHelp(cmd Command) {
	conf := cmd.Configuration()
	rootConf := p.Root.Configuration()
	fmt.Printf("개요: %s %s — %s\n\n", rootConf.CommandName, conf.CommandName, conf.Abstract)
	if conf.Discussion != "" {
		fmt.Printf("설명:\n%s\n\n", conf.Discussion)
	}

	params := cmd.Params()
	if len(params) == 0 {
		return
	}

	// @Argument 출력
	fmt.Printf("사용법: %s %s", rootConf.CommandName, conf.CommandName)
	for _, def := range params {
		if def.Type == ParamArgument {
			if def.Required {
				fmt.Printf(" <%s>", def.Name)
			} else {
				fmt.Printf(" [%s]", def.Name)
			}
		}
	}
	fmt.Println(" [옵션...]")

	// @Argument 상세
	hasArgs := false
	for _, def := range params {
		if def.Type == ParamArgument {
			if !hasArgs {
				fmt.Println("\n인자:")
				hasArgs = true
			}
			req := ""
			if def.Required {
				req = " (필수)"
			}
			fmt.Printf("  %-16s %s%s\n", "<"+def.Name+">", def.Help, req)
		}
	}

	// @Option, @Flag 상세
	hasOpts := false
	for _, def := range params {
		if def.Type == ParamOption || def.Type == ParamFlag {
			if !hasOpts {
				fmt.Println("\n옵션:")
				hasOpts = true
			}
			switch def.Type {
			case ParamOption:
				defStr := ""
				if def.DefaultValue != "" {
					defStr = fmt.Sprintf(" (기본: %s)", def.DefaultValue)
				}
				fmt.Printf("  --%-14s %s%s\n", def.Name+" <값>", def.Help, defStr)
			case ParamFlag:
				fmt.Printf("  --%-14s %s\n", def.Name, def.Help)
			}
		}
	}
}

// availableSubcommands는 사용 가능한 서브커맨드 이름을 쉼표로 구분하여 반환한다.
func (p *Parser) availableSubcommands() string {
	var names []string
	for _, sub := range p.Root.Configuration().Subcommands {
		names = append(names, sub.Configuration().CommandName)
	}
	return strings.Join(names, ", ")
}

// ---------------------------------------------------------------------------
// 6. 셸 자동완성 시뮬레이션
// ---------------------------------------------------------------------------

// ShellCompletion은 tart의 ShellCompletions.swift를 시뮬레이션한다.
// completeMachines(), completeLocalMachines() 등의 자동완성 함수에 대응한다.
type ShellCompletion struct {
	root Command
}

// Complete는 현재 입력에 기반한 자동완성 후보를 생성한다.
// tart의 @Argument(completion: .custom(completeMachines))에 대응한다.
func (sc *ShellCompletion) Complete(args []string) []string {
	if len(args) == 0 {
		// 서브커맨드 자동완성
		var names []string
		for _, sub := range sc.root.Configuration().Subcommands {
			names = append(names, sub.Configuration().CommandName)
		}
		sort.Strings(names)
		return names
	}

	// 서브커맨드 부분 매칭
	prefix := args[0]
	if len(args) == 1 {
		var matches []string
		for _, sub := range sc.root.Configuration().Subcommands {
			name := sub.Configuration().CommandName
			if strings.HasPrefix(name, prefix) {
				matches = append(matches, name)
			}
		}
		return matches
	}

	// 서브커맨드가 확정된 후: 옵션 자동완성
	var matched Command
	for _, sub := range sc.root.Configuration().Subcommands {
		if sub.Configuration().CommandName == prefix {
			matched = sub
			break
		}
	}
	if matched == nil {
		return nil
	}

	// 마지막 인자가 "--"로 시작하면 옵션 완성
	last := args[len(args)-1]
	if strings.HasPrefix(last, "--") {
		optPrefix := strings.TrimPrefix(last, "--")
		var opts []string
		for _, def := range matched.Params() {
			if def.Type != ParamArgument && strings.HasPrefix(def.Name, optPrefix) {
				opts = append(opts, "--"+def.Name)
			}
		}
		return opts
	}

	// VM 이름 자동완성 시뮬레이션 (tart의 completeLocalMachines)
	simulatedVMs := []string{"macos-sonoma", "ubuntu-22.04", "debian-12", "macos-ventura"}
	var matches []string
	for _, vm := range simulatedVMs {
		if strings.HasPrefix(vm, last) {
			matches = append(matches, vm)
		}
	}
	return matches
}

// ---------------------------------------------------------------------------
// 7. 메인 함수 — tart Root.main()의 실행 흐름 재현
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("=== PoC-09: 트리 기반 서브커맨드 파싱 시뮬레이션 ===")
	fmt.Println()

	root := &RootCommand{}
	parser := &Parser{Root: root}
	completion := &ShellCompletion{root: root}

	// --- 데모 1: 도움말 출력 ---
	fmt.Println("--- [데모 1] 루트 도움말 ---")
	parser.printRootHelp()
	fmt.Println()

	// --- 데모 2: clone 서브커맨드 도움말 ---
	fmt.Println("--- [데모 2] clone 서브커맨드 도움말 ---")
	for _, sub := range root.Configuration().Subcommands {
		if sub.Configuration().CommandName == "clone" {
			parser.printCommandHelp(sub)
			break
		}
	}
	fmt.Println()

	// --- 데모 3: 정상적인 clone 실행 ---
	fmt.Println("--- [데모 3] 정상적인 clone 실행 ---")
	testArgs := []string{"clone", "ghcr.io/cirruslabs/macos-sonoma-base:latest", "my-vm", "--concurrency", "8"}
	fmt.Printf("입력: tart %s\n", strings.Join(testArgs, " "))
	cmd, params, err := parser.parseArgs(testArgs)
	if err != nil {
		fmt.Printf("파싱 오류: %s\n", err)
	} else {
		if err := cmd.Validate(params); err != nil {
			fmt.Printf("유효성 오류: %s\n", err)
		} else {
			cmd.Run(params)
		}
	}
	fmt.Println()

	// --- 데모 4: 유효성 검증 실패 ---
	fmt.Println("--- [데모 4] 유효성 검증 실패 (new-name에 / 포함) ---")
	testArgs2 := []string{"clone", "source-vm", "org/invalid-name"}
	fmt.Printf("입력: tart %s\n", strings.Join(testArgs2, " "))
	cmd2, params2, err2 := parser.parseArgs(testArgs2)
	if err2 != nil {
		fmt.Printf("파싱 오류: %s\n", err2)
	} else {
		if err := cmd2.Validate(params2); err != nil {
			fmt.Printf("유효성 오류: %s\n", err)
		}
	}
	fmt.Println()

	// --- 데모 5: prune 유효성 검증 (기준 누락) ---
	fmt.Println("--- [데모 5] prune 유효성 검증 실패 (기준 누락) ---")
	testArgs3 := []string{"prune"}
	fmt.Printf("입력: tart %s\n", strings.Join(testArgs3, " "))
	cmd3, params3, err3 := parser.parseArgs(testArgs3)
	if err3 != nil {
		fmt.Printf("파싱 오류: %s\n", err3)
	} else {
		if err := cmd3.Validate(params3); err != nil {
			fmt.Printf("  → %s\n", err)
		}
	}
	fmt.Println()

	// --- 데모 6: prune 정상 실행 ---
	fmt.Println("--- [데모 6] prune --older-than 7 --gc ---")
	testArgs4 := []string{"prune", "--older-than", "7", "--gc"}
	fmt.Printf("입력: tart %s\n", strings.Join(testArgs4, " "))
	cmd4, params4, err4 := parser.parseArgs(testArgs4)
	if err4 != nil {
		fmt.Printf("파싱 오류: %s\n", err4)
	} else {
		if err := cmd4.Validate(params4); err != nil {
			fmt.Printf("유효성 오류: %s\n", err)
		} else {
			cmd4.Run(params4)
		}
	}
	fmt.Println()

	// --- 데모 7: 필수 인자 누락 ---
	fmt.Println("--- [데모 7] 필수 인자 누락 ---")
	testArgs5 := []string{"clone", "source-vm"}
	fmt.Printf("입력: tart %s\n", strings.Join(testArgs5, " "))
	_, _, err5 := parser.parseArgs(testArgs5)
	if err5 != nil {
		fmt.Printf("  → %s\n", err5)
	}
	fmt.Println()

	// --- 데모 8: 알 수 없는 서브커맨드 ---
	fmt.Println("--- [데모 8] 알 수 없는 서브커맨드 ---")
	testArgs6 := []string{"unknown"}
	fmt.Printf("입력: tart %s\n", strings.Join(testArgs6, " "))
	_, _, err6 := parser.parseArgs(testArgs6)
	if err6 != nil {
		fmt.Printf("  → %s\n", err6)
	}
	fmt.Println()

	// --- 데모 9: 셸 자동완성 ---
	fmt.Println("--- [데모 9] 셸 자동완성 시뮬레이션 ---")
	scenarios := []struct {
		desc string
		args []string
	}{
		{"빈 입력 → 서브커맨드 후보", []string{}},
		{"'cl' 입력 → 부분 매칭", []string{"cl"}},
		{"'clone --' → 옵션 완성", []string{"clone", "--"}},
		{"'clone --con' → 옵션 부분 매칭", []string{"clone", "--con"}},
		{"'run mac' → VM 이름 완성", []string{"run", "mac"}},
	}

	for _, s := range scenarios {
		results := completion.Complete(s.args)
		input := strings.Join(s.args, " ")
		if input == "" {
			input = "(빈 입력)"
		}
		fmt.Printf("  입력: %-25s → 후보: %v\n", input, results)
	}
	fmt.Println()

	// --- 데모 10: 실행 흐름 요약 ---
	fmt.Println("--- [데모 10] tart 실행 흐름 요약 ---")
	fmt.Println("  1. Root.main() 진입")
	fmt.Println("  2. SIGINT 핸들러 설정 (Ctrl+C → task.cancel())")
	fmt.Println("  3. parseAsRoot() → 서브커맨드 매칭")
	fmt.Println("  4. OTel 루트 스팬 생성 (커맨드 이름)")
	fmt.Println("  5. Config().gc() — GC 수행 (Pull/Clone 제외)")
	fmt.Println("  6. command.validate() → command.run()")
	fmt.Println("  7. 에러 발생 시 OpenTelemetry에 예외 기록")
	fmt.Println("  8. OTel.shared.flush() — 트레이스 전송")
}
