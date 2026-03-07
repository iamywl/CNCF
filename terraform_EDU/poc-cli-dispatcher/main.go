package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// =============================================================================
// Terraform CLI 커맨드 디스패치 시뮬레이션
// =============================================================================
// Terraform CLI는 명령어 패턴으로 설계되어 있습니다.
// 실제 코드: internal/command/ 디렉토리
//
// 핵심 구조:
// 1. Command 인터페이스 - 모든 명령어의 공통 계약
// 2. CommandFactory - 명령어 지연 생성 (메모리 효율)
// 3. Meta 구조체 - 모든 명령어가 공유하는 기반 기능
// 4. CLI 디스패처 - 인자 파싱 → 명령어 찾기 → 실행
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// Command 인터페이스
// ─────────────────────────────────────────────────────────────────────────────

// Command는 모든 CLI 명령어가 구현해야 하는 인터페이스입니다.
// 실제: github.com/mitchellh/cli 패키지의 Command 인터페이스
type Command interface {
	// Run은 명령어를 실행합니다. 반환값은 exit code입니다.
	Run(args []string) int
	// Help는 상세 도움말을 반환합니다.
	Help() string
	// Synopsis는 한 줄 요약을 반환합니다.
	Synopsis() string
}

// CommandFactory는 Command를 지연 생성하는 팩토리 함수입니다.
// 실제 Terraform에서는 모든 명령어를 미리 생성하지 않고,
// 필요할 때 팩토리를 통해 생성합니다 (메모리 절약).
type CommandFactory func() (Command, error)

// ─────────────────────────────────────────────────────────────────────────────
// Meta 구조체 - 모든 명령어의 기반
// ─────────────────────────────────────────────────────────────────────────────

// Meta는 모든 명령어가 임베딩하는 기반 구조체입니다.
// 실제: internal/command/meta.go
type Meta struct {
	// Color는 컬러 출력 활성화 여부입니다.
	Color bool
	// WorkingDir는 작업 디렉토리입니다.
	WorkingDir string
	// StatePath는 상태 파일 경로입니다.
	StatePath string
	// BackendConfig는 백엔드 설정입니다.
	BackendConfig map[string]string
}

// Ui는 간단한 출력 헬퍼입니다.
func (m *Meta) Ui() *SimpleUI {
	return &SimpleUI{Color: m.Color}
}

// ParseArgs는 공통 인자를 파싱합니다.
func (m *Meta) ParseArgs(args []string) (remaining []string) {
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-no-color":
			m.Color = false
		case "-chdir":
			if i+1 < len(args) {
				m.WorkingDir = args[i+1]
				i++
			}
		case "-state":
			if i+1 < len(args) {
				m.StatePath = args[i+1]
				i++
			}
		default:
			rest = append(rest, args[i])
		}
	}
	return rest
}

// SimpleUI는 간단한 사용자 인터페이스입니다.
type SimpleUI struct {
	Color bool
}

func (ui *SimpleUI) Output(msg string) {
	fmt.Println(msg)
}

func (ui *SimpleUI) Info(msg string) {
	if ui.Color {
		fmt.Printf("\033[1;34m%s\033[0m\n", msg)
	} else {
		fmt.Println(msg)
	}
}

func (ui *SimpleUI) Warn(msg string) {
	if ui.Color {
		fmt.Printf("\033[1;33mWarning: %s\033[0m\n", msg)
	} else {
		fmt.Printf("Warning: %s\n", msg)
	}
}

func (ui *SimpleUI) Error(msg string) {
	if ui.Color {
		fmt.Printf("\033[1;31mError: %s\033[0m\n", msg)
	} else {
		fmt.Printf("Error: %s\n", msg)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 명령어 구현체들
// ─────────────────────────────────────────────────────────────────────────────

// InitCommand: terraform init
type InitCommand struct {
	Meta
}

func (c *InitCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	ui.Info("Initializing the backend...")
	ui.Output("")

	upgrade := false
	reconfigure := false
	for _, arg := range remaining {
		if arg == "-upgrade" {
			upgrade = true
		}
		if arg == "-reconfigure" {
			reconfigure = true
		}
	}

	if upgrade {
		ui.Output("- Upgrading modules and providers...")
	}
	if reconfigure {
		ui.Output("- Reconfiguring backend (ignoring saved config)...")
	}

	ui.Output("Initializing provider plugins...")
	ui.Output("- Finding hashicorp/aws versions matching \"~> 4.0\"...")
	ui.Output("- Installing hashicorp/aws v4.67.0...")
	ui.Output("- Installed hashicorp/aws v4.67.0")
	ui.Output("")
	ui.Info("Terraform has been successfully initialized!")
	return 0
}

func (c *InitCommand) Help() string {
	return `사용법: terraform init [옵션]

  현재 디렉토리의 Terraform 설정을 초기화합니다.
  프로바이더를 다운로드하고 백엔드를 구성합니다.

옵션:
  -upgrade        모듈과 프로바이더를 최신 버전으로 업그레이드
  -reconfigure    백엔드를 재구성 (저장된 설정 무시)
  -no-color       컬러 출력 비활성화
`
}

func (c *InitCommand) Synopsis() string {
	return "작업 디렉토리 초기화 (프로바이더 설치, 백엔드 구성)"
}

// PlanCommand: terraform plan
type PlanCommand struct {
	Meta
}

func (c *PlanCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	var target string
	var destroy bool
	out := ""

	for i := 0; i < len(remaining); i++ {
		switch remaining[i] {
		case "-target":
			if i+1 < len(remaining) {
				target = remaining[i+1]
				i++
			}
		case "-destroy":
			destroy = true
		case "-out":
			if i+1 < len(remaining) {
				out = remaining[i+1]
				i++
			}
		}
	}

	if destroy {
		ui.Warn("이 plan은 모든 리소스를 삭제합니다!")
	}

	ui.Info("Terraform will perform the following actions:")
	ui.Output("")

	if target != "" {
		ui.Output(fmt.Sprintf("  (대상 제한: %s)", target))
	}

	ui.Output("  # aws_instance.web will be created")
	ui.Output("  + resource \"aws_instance\" \"web\" {")
	ui.Output("      + ami           = \"ami-12345\"")
	ui.Output("      + instance_type = \"t2.micro\"")
	ui.Output("      + id            = (known after apply)")
	ui.Output("    }")
	ui.Output("")
	ui.Info("Plan: 1 to add, 0 to change, 0 to destroy.")

	if out != "" {
		ui.Output(fmt.Sprintf("\nPlan이 '%s'에 저장되었습니다.", out))
	}

	return 0
}

func (c *PlanCommand) Help() string {
	return `사용법: terraform plan [옵션]

  실행 계획을 생성하여 어떤 변경이 필요한지 보여줍니다.

옵션:
  -target=ADDR    특정 리소스만 대상으로 plan
  -destroy        삭제 plan 생성
  -out=FILE       plan을 파일에 저장
  -no-color       컬러 출력 비활성화
`
}

func (c *PlanCommand) Synopsis() string {
	return "인프라 변경 사항 미리보기"
}

// ApplyCommand: terraform apply
type ApplyCommand struct {
	Meta
}

func (c *ApplyCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	autoApprove := false
	var planFile string

	for i := 0; i < len(remaining); i++ {
		switch remaining[i] {
		case "-auto-approve":
			autoApprove = true
		default:
			if !strings.HasPrefix(remaining[i], "-") {
				planFile = remaining[i]
			}
		}
	}

	if planFile != "" {
		ui.Output(fmt.Sprintf("Plan 파일 '%s'을(를) 적용합니다...", planFile))
	}

	if !autoApprove && planFile == "" {
		ui.Output("Do you want to perform these actions?")
		ui.Output("  Terraform will perform the actions described above.")
		ui.Output("  Only 'yes' will be accepted to approve.")
		ui.Output("")
		ui.Output("  Enter a value: yes (시뮬레이션: 자동 승인)")
	}

	ui.Output("")
	ui.Info("aws_instance.web: Creating...")
	ui.Info("aws_instance.web: Creation complete after 32s [id=i-abc123]")
	ui.Output("")
	ui.Info("Apply complete! Resources: 1 added, 0 changed, 0 destroyed.")

	return 0
}

func (c *ApplyCommand) Help() string {
	return `사용법: terraform apply [옵션] [PLAN_FILE]

  Terraform 변경 사항을 적용합니다.

옵션:
  -auto-approve   대화형 승인 건너뛰기
  -target=ADDR    특정 리소스만 적용
  -no-color       컬러 출력 비활성화
`
}

func (c *ApplyCommand) Synopsis() string {
	return "인프라 변경 사항 적용"
}

// DestroyCommand: terraform destroy
type DestroyCommand struct {
	Meta
}

func (c *DestroyCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	autoApprove := false
	for _, arg := range remaining {
		if arg == "-auto-approve" {
			autoApprove = true
		}
	}

	ui.Warn("이 작업은 모든 관리 리소스를 삭제합니다!")
	ui.Output("")

	if !autoApprove {
		ui.Output("정말로 모든 리소스를 삭제하시겠습니까?")
		ui.Output("  'yes'를 입력하세요.")
		ui.Output("  (시뮬레이션: 자동 승인)")
	}

	ui.Output("")
	ui.Info("aws_instance.web: Destroying... [id=i-abc123]")
	ui.Info("aws_instance.web: Destruction complete after 10s")
	ui.Output("")
	ui.Info("Destroy complete! Resources: 1 destroyed.")

	return 0
}

func (c *DestroyCommand) Help() string {
	return `사용법: terraform destroy [옵션]

  모든 관리 리소스를 삭제합니다.

옵션:
  -auto-approve   대화형 승인 건너뛰기
  -target=ADDR    특정 리소스만 삭제
  -no-color       컬러 출력 비활성화
`
}

func (c *DestroyCommand) Synopsis() string {
	return "모든 관리 리소스 삭제"
}

// ─────────────────────────────────────────────────────────────────────────────
// 서브커맨드: terraform state
// ─────────────────────────────────────────────────────────────────────────────

// StateListCommand: terraform state list
type StateListCommand struct {
	Meta
}

func (c *StateListCommand) Run(args []string) int {
	c.ParseArgs(args)
	ui := c.Ui()

	ui.Output("aws_instance.web")
	ui.Output("aws_vpc.main")
	ui.Output("aws_subnet.public[0]")
	ui.Output("aws_subnet.public[1]")
	ui.Output("module.network.aws_route_table.main")
	return 0
}

func (c *StateListCommand) Help() string {
	return `사용법: terraform state list [옵션] [FILTER]

  현재 상태의 모든 리소스를 나열합니다.

옵션:
  -state=FILE   상태 파일 경로
`
}

func (c *StateListCommand) Synopsis() string {
	return "상태의 리소스 목록 표시"
}

// StateShowCommand: terraform state show
type StateShowCommand struct {
	Meta
}

func (c *StateShowCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	if len(remaining) == 0 {
		ui.Error("리소스 주소를 지정해야 합니다.")
		return 1
	}

	addr := remaining[0]
	ui.Output(fmt.Sprintf("# %s:", addr))
	ui.Output(fmt.Sprintf("resource \"%s\" {", addr))
	ui.Output("  ami           = \"ami-12345\"")
	ui.Output("  instance_type = \"t2.micro\"")
	ui.Output("  id            = \"i-abc123\"")
	ui.Output("  tags = {")
	ui.Output("    Name = \"web-server\"")
	ui.Output("  }")
	ui.Output("}")
	return 0
}

func (c *StateShowCommand) Help() string {
	return `사용법: terraform state show [옵션] ADDRESS

  상태에서 특정 리소스의 속성을 표시합니다.

옵션:
  -state=FILE   상태 파일 경로
`
}

func (c *StateShowCommand) Synopsis() string {
	return "상태에서 리소스 상세 정보 표시"
}

// StateMvCommand: terraform state mv
type StateMvCommand struct {
	Meta
}

func (c *StateMvCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	if len(remaining) < 2 {
		ui.Error("소스와 대상 주소를 모두 지정해야 합니다.")
		return 1
	}

	src := remaining[0]
	dst := remaining[1]

	ui.Output(fmt.Sprintf("Move \"%s\" to \"%s\"", src, dst))
	ui.Info("Successfully moved 1 object(s).")
	return 0
}

func (c *StateMvCommand) Help() string {
	return `사용법: terraform state mv [옵션] SOURCE DESTINATION

  상태에서 리소스를 이동합니다.
`
}

func (c *StateMvCommand) Synopsis() string {
	return "상태에서 리소스 이동/이름 변경"
}

// StateRmCommand: terraform state rm
type StateRmCommand struct {
	Meta
}

func (c *StateRmCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	if len(remaining) == 0 {
		ui.Error("제거할 리소스 주소를 지정해야 합니다.")
		return 1
	}

	for _, addr := range remaining {
		ui.Output(fmt.Sprintf("Removed %s", addr))
	}
	ui.Info(fmt.Sprintf("Successfully removed %d object(s).", len(remaining)))
	return 0
}

func (c *StateRmCommand) Help() string {
	return `사용법: terraform state rm [옵션] ADDRESS...

  상태에서 리소스를 제거합니다 (실제 인프라는 유지).
`
}

func (c *StateRmCommand) Synopsis() string {
	return "상태에서 리소스 제거 (인프라 유지)"
}

// VersionCommand: terraform version
type VersionCommand struct {
	Meta
	Version string
}

func (c *VersionCommand) Run(args []string) int {
	ui := c.Ui()
	ui.Output(fmt.Sprintf("Terraform v%s", c.Version))
	ui.Output("on darwin_arm64")
	ui.Output("")
	ui.Output("Your version of Terraform is out of date! The latest version")
	ui.Output("is 1.8.0. You can update by downloading from terraform.io")
	return 0
}

func (c *VersionCommand) Help() string {
	return "사용법: terraform version\n\n  현재 Terraform 버전을 출력합니다.\n"
}

func (c *VersionCommand) Synopsis() string {
	return "Terraform 버전 출력"
}

// FmtCommand: terraform fmt
type FmtCommand struct {
	Meta
}

func (c *FmtCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	check := false
	recursive := false
	for _, arg := range remaining {
		if arg == "-check" {
			check = true
		}
		if arg == "-recursive" {
			recursive = true
		}
	}

	files := []string{"main.tf", "variables.tf", "outputs.tf"}
	if recursive {
		files = append(files, "modules/vpc/main.tf", "modules/vpc/variables.tf")
	}

	if check {
		ui.Output("다음 파일들의 포맷이 올바르지 않습니다:")
		for _, f := range files {
			ui.Output(f)
		}
		return 3 // 포맷 불일치 시 exit code 3
	}

	for _, f := range files {
		ui.Output(f)
	}
	return 0
}

func (c *FmtCommand) Help() string {
	return `사용법: terraform fmt [옵션] [DIR]

  Terraform 설정 파일을 표준 포맷으로 정리합니다.

옵션:
  -check       포맷 확인만 (수정하지 않음)
  -recursive   하위 디렉토리도 포맷
  -diff        변경 사항 diff 표시
`
}

func (c *FmtCommand) Synopsis() string {
	return "설정 파일 포맷 정리"
}

// ValidateCommand: terraform validate
type ValidateCommand struct {
	Meta
}

func (c *ValidateCommand) Run(args []string) int {
	c.ParseArgs(args)
	ui := c.Ui()

	ui.Info("Success! The configuration is valid.")
	return 0
}

func (c *ValidateCommand) Help() string {
	return "사용법: terraform validate [옵션]\n\n  설정 파일의 문법과 내부 일관성을 검증합니다.\n"
}

func (c *ValidateCommand) Synopsis() string {
	return "설정 파일 유효성 검증"
}

// OutputCommand: terraform output
type OutputCommand struct {
	Meta
}

func (c *OutputCommand) Run(args []string) int {
	remaining := c.ParseArgs(args)
	ui := c.Ui()

	outputs := map[string]string{
		"instance_ip":  "10.0.1.5",
		"vpc_id":       "vpc-abc123",
		"subnet_count": "3",
	}

	if len(remaining) > 0 {
		name := remaining[0]
		if val, ok := outputs[name]; ok {
			ui.Output(val)
		} else {
			ui.Error(fmt.Sprintf("출력값 '%s'을(를) 찾을 수 없습니다.", name))
			return 1
		}
	} else {
		for k, v := range outputs {
			ui.Output(fmt.Sprintf("%s = \"%s\"", k, v))
		}
	}
	return 0
}

func (c *OutputCommand) Help() string {
	return "사용법: terraform output [옵션] [NAME]\n\n  출력값을 표시합니다.\n"
}

func (c *OutputCommand) Synopsis() string {
	return "출력값 표시"
}

// ─────────────────────────────────────────────────────────────────────────────
// CLI 디스패처
// ─────────────────────────────────────────────────────────────────────────────

// CLI는 커맨드 라인 인터페이스 디스패처입니다.
// 실제: main.go에서 mitchellh/cli.CLI 사용
type CLI struct {
	Commands  map[string]CommandFactory
	HelpFunc  func(commands map[string]CommandFactory) string
	Version   string
}

// NewCLI는 새 CLI 디스패처를 생성합니다.
func NewCLI() *CLI {
	cli := &CLI{
		Commands: make(map[string]CommandFactory),
		Version:  "1.7.0-poc",
	}

	meta := Meta{Color: true, WorkingDir: "."}

	// 기본 명령어 등록
	cli.Commands["init"] = func() (Command, error) {
		return &InitCommand{Meta: meta}, nil
	}
	cli.Commands["plan"] = func() (Command, error) {
		return &PlanCommand{Meta: meta}, nil
	}
	cli.Commands["apply"] = func() (Command, error) {
		return &ApplyCommand{Meta: meta}, nil
	}
	cli.Commands["destroy"] = func() (Command, error) {
		return &DestroyCommand{Meta: meta}, nil
	}
	cli.Commands["fmt"] = func() (Command, error) {
		return &FmtCommand{Meta: meta}, nil
	}
	cli.Commands["validate"] = func() (Command, error) {
		return &ValidateCommand{Meta: meta}, nil
	}
	cli.Commands["output"] = func() (Command, error) {
		return &OutputCommand{Meta: meta}, nil
	}
	cli.Commands["version"] = func() (Command, error) {
		return &VersionCommand{Meta: meta, Version: cli.Version}, nil
	}

	// 서브커맨드 등록 (중첩 명령어)
	cli.Commands["state list"] = func() (Command, error) {
		return &StateListCommand{Meta: meta}, nil
	}
	cli.Commands["state show"] = func() (Command, error) {
		return &StateShowCommand{Meta: meta}, nil
	}
	cli.Commands["state mv"] = func() (Command, error) {
		return &StateMvCommand{Meta: meta}, nil
	}
	cli.Commands["state rm"] = func() (Command, error) {
		return &StateRmCommand{Meta: meta}, nil
	}

	return cli
}

// Run은 주어진 인자로 적절한 명령어를 찾아 실행합니다.
func (c *CLI) Run(args []string) int {
	if len(args) == 0 {
		c.printHelp()
		return 0
	}

	// 서브커맨드 탐색: "state list" → "state list" 키로 검색
	// 가장 긴 매칭을 먼저 시도
	cmdName := ""
	cmdArgs := args

	for i := len(args); i > 0; i-- {
		candidate := strings.Join(args[:i], " ")
		if _, ok := c.Commands[candidate]; ok {
			cmdName = candidate
			cmdArgs = args[i:]
			break
		}
	}

	// 특수 플래그 처리
	if len(args) > 0 {
		switch args[0] {
		case "-help", "--help", "-h":
			c.printHelp()
			return 0
		case "-version", "--version", "-v":
			cmdName = "version"
			cmdArgs = nil
		}
	}

	if cmdName == "" {
		fmt.Fprintf(os.Stderr, "\nTerraform에 '%s' 명령어가 없습니다.\n", args[0])
		c.suggestCommand(args[0])
		return 1
	}

	factory, ok := c.Commands[cmdName]
	if !ok {
		fmt.Fprintf(os.Stderr, "명령어 '%s'을(를) 찾을 수 없습니다.\n", cmdName)
		return 1
	}

	// 도움말 요청 확인
	for _, arg := range cmdArgs {
		if arg == "-help" || arg == "--help" || arg == "-h" {
			cmd, err := factory()
			if err != nil {
				fmt.Fprintf(os.Stderr, "명령어 생성 오류: %v\n", err)
				return 1
			}
			fmt.Println(cmd.Help())
			return 0
		}
	}

	cmd, err := factory()
	if err != nil {
		fmt.Fprintf(os.Stderr, "명령어 생성 오류: %v\n", err)
		return 1
	}

	return cmd.Run(cmdArgs)
}

// printHelp는 전체 도움말을 출력합니다.
func (c *CLI) printHelp() {
	fmt.Printf("Usage: terraform [global options] <subcommand> [args]\n\n")
	fmt.Printf("Terraform v%s\n\n", c.Version)

	// 명령어를 카테고리별로 분류
	mainCmds := []string{"init", "validate", "plan", "apply", "destroy"}
	otherCmds := []string{"fmt", "output", "version"}
	stateCmds := []string{"state list", "state show", "state mv", "state rm"}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)

	fmt.Fprintln(w, "주요 명령어:")
	for _, name := range mainCmds {
		if factory, ok := c.Commands[name]; ok {
			cmd, _ := factory()
			fmt.Fprintf(w, "  %s\t%s\n", name, cmd.Synopsis())
		}
	}

	fmt.Fprintln(w, "\n기타 명령어:")
	for _, name := range otherCmds {
		if factory, ok := c.Commands[name]; ok {
			cmd, _ := factory()
			fmt.Fprintf(w, "  %s\t%s\n", name, cmd.Synopsis())
		}
	}

	fmt.Fprintln(w, "\nState 서브커맨드:")
	for _, name := range stateCmds {
		if factory, ok := c.Commands[name]; ok {
			cmd, _ := factory()
			fmt.Fprintf(w, "  terraform %s\t%s\n", name, cmd.Synopsis())
		}
	}

	w.Flush()
}

// suggestCommand는 오타에 대해 유사한 명령어를 제안합니다.
// Levenshtein 거리를 사용하여 가장 유사한 명령어를 찾습니다.
func (c *CLI) suggestCommand(input string) {
	type suggestion struct {
		name     string
		distance int
	}

	var suggestions []suggestion

	for name := range c.Commands {
		// 서브커맨드의 경우 첫 번째 단어만 비교
		parts := strings.Fields(name)
		compareName := parts[0]

		dist := levenshteinDistance(input, compareName)
		if dist <= 3 { // 거리 3 이하만 제안
			suggestions = append(suggestions, suggestion{name: name, distance: dist})
		}
	}

	// 거리순 정렬
	sort.Slice(suggestions, func(i, j int) bool {
		return suggestions[i].distance < suggestions[j].distance
	})

	// 중복 제거 (첫 번째 단어 기준)
	seen := make(map[string]bool)
	var unique []suggestion
	for _, s := range suggestions {
		parts := strings.Fields(s.name)
		if !seen[parts[0]] {
			seen[parts[0]] = true
			unique = append(unique, s)
		}
	}

	if len(unique) > 0 {
		fmt.Println("\n혹시 다음 명령어를 의미하셨나요?")
		for i, s := range unique {
			if i >= 3 {
				break
			}
			fmt.Printf("  terraform %s\n", s.name)
		}
	}
	fmt.Println()
}

// levenshteinDistance는 두 문자열 간의 편집 거리를 계산합니다.
func levenshteinDistance(a, b string) int {
	la := len(a)
	lb := len(b)

	dp := make([][]int, la+1)
	for i := range dp {
		dp[i] = make([]int, lb+1)
	}

	for i := 0; i <= la; i++ {
		dp[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		dp[0][j] = j
	}

	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			dp[i][j] = int(math.Min(
				float64(dp[i-1][j]+1),
				math.Min(
					float64(dp[i][j-1]+1),
					float64(dp[i-1][j-1]+cost),
				),
			))
		}
	}

	return dp[la][lb]
}

// ─────────────────────────────────────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────────────────────────────────────

func printSection(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Terraform CLI 커맨드 디스패치 시뮬레이션                    ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  Command 인터페이스 → CommandFactory → CLI 디스패처 패턴             ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	cli := NewCLI()

	// 1. 전체 도움말
	printSection("1. 전체 도움말 (terraform)")
	cli.Run([]string{})

	// 2. 기본 명령어 실행
	printSection("2. terraform version")
	cli.Run([]string{"version"})

	printSection("3. terraform init")
	cli.Run([]string{"init"})

	printSection("4. terraform init -upgrade")
	cli.Run([]string{"init", "-upgrade"})

	printSection("5. terraform plan")
	cli.Run([]string{"plan"})

	printSection("6. terraform plan -target=aws_instance.web -out=plan.out")
	cli.Run([]string{"plan", "-target", "aws_instance.web", "-out", "plan.out"})

	printSection("7. terraform apply -auto-approve")
	cli.Run([]string{"apply", "-auto-approve"})

	printSection("8. terraform destroy -auto-approve")
	cli.Run([]string{"destroy", "-auto-approve"})

	// 3. 서브커맨드
	printSection("9. terraform state list")
	cli.Run([]string{"state", "list"})

	printSection("10. terraform state show aws_instance.web")
	cli.Run([]string{"state", "show", "aws_instance.web"})

	printSection("11. terraform state mv aws_instance.web aws_instance.web_new")
	cli.Run([]string{"state", "mv", "aws_instance.web", "aws_instance.web_new"})

	printSection("12. terraform state rm aws_instance.old")
	cli.Run([]string{"state", "rm", "aws_instance.old"})

	// 4. 도움말 요청
	printSection("13. terraform plan -help")
	cli.Run([]string{"plan", "-help"})

	// 5. fmt와 validate
	printSection("14. terraform fmt -recursive")
	cli.Run([]string{"fmt", "-recursive"})

	printSection("15. terraform validate")
	cli.Run([]string{"validate"})

	// 6. 잘못된 명령어 - "Did you mean?" 제안
	printSection("16. 오타 제안: terraform destory (오타)")
	cli.Run([]string{"destory"})

	printSection("17. 오타 제안: terraform plna (오타)")
	cli.Run([]string{"plna"})

	printSection("18. 오타 제안: terraform aply (오타)")
	cli.Run([]string{"aply"})

	printSection("19. 알 수 없는 명령어: terraform foobar")
	cli.Run([]string{"foobar"})

	// 아키텍처 요약
	printSection("CLI 디스패치 아키텍처 요약")
	fmt.Print(`
  사용자 입력: terraform state show aws_instance.web
               ─────── ────────── ──────────────────
                 CLI    서브커맨드      인자

  디스패치 흐름:

  ┌──────────────┐     ┌────────────────────┐     ┌──────────────┐
  │   CLI.Run()  │────▶│  명령어 매칭        │────▶│ Command.Run()│
  │              │     │                    │     │              │
  │ args 파싱    │     │ "state show"       │     │ 플래그 파싱  │
  │              │     │ → StateShowCommand │     │ 실행         │
  └──────────────┘     └────────────────────┘     └──────────────┘
         │                      │
         │ 매칭 실패            │ 서브커맨드 탐색
         ▼                      ▼
  ┌──────────────┐     ┌────────────────────┐
  │ suggestCmd() │     │ 가장 긴 매칭 우선  │
  │              │     │ "state show" > "state" │
  │ Levenshtein  │     └────────────────────┘
  │ 거리 계산    │
  └──────────────┘

  CommandFactory 패턴:

  map[string]CommandFactory{
    "init":       func() → InitCommand{Meta},
    "plan":       func() → PlanCommand{Meta},
    "apply":      func() → ApplyCommand{Meta},
    "state list": func() → StateListCommand{Meta},
    ...
  }

  → 명령어를 미리 생성하지 않고, 필요할 때만 팩토리로 생성
  → Meta를 임베딩하여 공통 기능(플래그 파싱, UI) 공유`)
}
