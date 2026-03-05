// Helm v4 아키텍처 PoC: CLI 디스패치 + Action 패턴
//
// 이 PoC는 Helm v4의 핵심 아키텍처 패턴을 시뮬레이션합니다:
//   1. Cobra 스타일의 CLI 디스패치 (cmd/helm/helm.go, pkg/cmd/root.go)
//   2. Action 패턴 - Configuration 공유 (pkg/action/action.go)
//   3. 서브커맨드 (install/upgrade/list) 실행 흐름
//
// 실행: go run main.go
//       go run main.go install myapp ./mychart
//       go run main.go upgrade myapp ./mychart
//       go run main.go list
//       go run main.go list --namespace production
//       go run main.go --debug install myapp ./mychart

package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// =============================================================================
// Configuration: Helm의 pkg/action/action.go → Configuration 구조체
// 모든 Action이 공유하는 의존성 컨테이너.
// 실제 Helm에서는 RESTClientGetter, Releases(Storage), KubeClient,
// RegistryClient 등을 포함한다.
// =============================================================================

// Configuration은 모든 Action이 공유하는 설정/의존성을 보관한다.
// Helm v4의 핵심: Action 간 설정을 공유하면서도 각 Action은 독립적으로 동작.
type Configuration struct {
	// Namespace는 Kubernetes 네임스페이스
	Namespace string
	// Debug 모드 여부
	Debug bool
	// Releases는 릴리스 저장소 (인메모리 시뮬레이션)
	Releases *ReleaseStorage
	// KubeClient 시뮬레이션
	KubeClient string
	// HelmDriver: secret, configmap, memory
	HelmDriver string
}

// NewConfiguration은 기본 Configuration을 생성한다.
// 실제 Helm: action.NewConfiguration(options ...ConfigurationOption)
func NewConfiguration() *Configuration {
	return &Configuration{
		Namespace:  "default",
		HelmDriver: "memory",
		Releases:   NewReleaseStorage(),
		KubeClient: "simulated-kube-client",
	}
}

// Init은 Configuration을 초기화한다.
// 실제 Helm: cfg.Init(getter, namespace, helmDriver)
// → 스토리지 드라이버 선택, KubeClient 생성 등
func (cfg *Configuration) Init(namespace, helmDriver string) error {
	cfg.Namespace = namespace
	cfg.HelmDriver = helmDriver

	if cfg.Debug {
		fmt.Printf("[DEBUG] Configuration 초기화: namespace=%s, driver=%s\n", namespace, helmDriver)
	}

	switch helmDriver {
	case "secret", "configmap", "memory", "":
		// 유효한 드라이버
	default:
		return fmt.Errorf("알 수 없는 드라이버: %q", helmDriver)
	}

	return nil
}

// =============================================================================
// ReleaseStorage: 릴리스 저장소 시뮬레이션
// =============================================================================

// Release는 간단한 릴리스 정보
type Release struct {
	Name      string
	Namespace string
	Chart     string
	Version   int
	Status    string
	Timestamp time.Time
}

// ReleaseStorage는 인메모리 릴리스 저장소
type ReleaseStorage struct {
	releases []*Release
}

func NewReleaseStorage() *ReleaseStorage {
	return &ReleaseStorage{releases: make([]*Release, 0)}
}

func (rs *ReleaseStorage) Create(rel *Release) {
	rs.releases = append(rs.releases, rel)
}

func (rs *ReleaseStorage) List(namespace string) []*Release {
	if namespace == "" {
		return rs.releases
	}
	var result []*Release
	for _, r := range rs.releases {
		if r.Namespace == namespace {
			result = append(result, r)
		}
	}
	return result
}

func (rs *ReleaseStorage) Get(name string) *Release {
	for i := len(rs.releases) - 1; i >= 0; i-- {
		if rs.releases[i].Name == name {
			return rs.releases[i]
		}
	}
	return nil
}

// =============================================================================
// Action 패턴: 각 명령(install, upgrade, list)은 Action 구조체로 구현
// 실제 Helm: pkg/action/install.go, upgrade.go, list.go
// 모든 Action은 *Configuration을 공유하며, Action별 설정을 별도로 가진다.
// =============================================================================

// Install은 install Action 구조체
// 실제: action.Install{cfg, ChartPathOptions, DryRunStrategy, ...}
type Install struct {
	cfg          *Configuration
	ReleaseName  string
	ChartPath    string
	DryRun       bool
	DisableHooks bool
}

// NewInstall은 Configuration을 주입받아 Install Action을 생성한다.
func NewInstall(cfg *Configuration) *Install {
	return &Install{cfg: cfg}
}

// Run은 install을 실행한다.
// 실제 Helm의 Install.RunWithContext(ctx, chrt, vals) 패턴을 단순화.
func (i *Install) Run() (*Release, error) {
	if i.ReleaseName == "" {
		return nil, fmt.Errorf("릴리스 이름이 필요합니다")
	}
	if i.ChartPath == "" {
		return nil, fmt.Errorf("차트 경로가 필요합니다")
	}

	// 이미 존재하는 릴리스 확인
	existing := i.cfg.Releases.Get(i.ReleaseName)
	if existing != nil && existing.Status == "deployed" {
		return nil, fmt.Errorf("릴리스 %q 가 이미 존재합니다. 'helm upgrade'를 사용하세요", i.ReleaseName)
	}

	if i.cfg.Debug {
		fmt.Printf("[DEBUG] Install: name=%s, chart=%s, namespace=%s\n",
			i.ReleaseName, i.ChartPath, i.cfg.Namespace)
	}

	if i.DryRun {
		fmt.Println("[DRY-RUN] 설치를 시뮬레이션합니다 (실제 변경 없음)")
	}

	rel := &Release{
		Name:      i.ReleaseName,
		Namespace: i.cfg.Namespace,
		Chart:     i.ChartPath,
		Version:   1,
		Status:    "deployed",
		Timestamp: time.Now(),
	}

	if !i.DryRun {
		i.cfg.Releases.Create(rel)
	}

	return rel, nil
}

// Upgrade는 upgrade Action 구조체
type Upgrade struct {
	cfg          *Configuration
	ReleaseName  string
	ChartPath    string
	Install      bool // --install 플래그: 없으면 설치
	DryRun       bool
	DisableHooks bool
}

func NewUpgrade(cfg *Configuration) *Upgrade {
	return &Upgrade{cfg: cfg}
}

func (u *Upgrade) Run() (*Release, error) {
	if u.ReleaseName == "" {
		return nil, fmt.Errorf("릴리스 이름이 필요합니다")
	}

	existing := u.cfg.Releases.Get(u.ReleaseName)
	if existing == nil {
		if u.Install {
			// install-or-upgrade 패턴
			fmt.Printf("릴리스 %q 가 없으므로 설치합니다\n", u.ReleaseName)
			inst := NewInstall(u.cfg)
			inst.ReleaseName = u.ReleaseName
			inst.ChartPath = u.ChartPath
			return inst.Run()
		}
		return nil, fmt.Errorf("릴리스 %q 를 찾을 수 없습니다", u.ReleaseName)
	}

	if u.cfg.Debug {
		fmt.Printf("[DEBUG] Upgrade: name=%s, chart=%s, version=%d→%d\n",
			u.ReleaseName, u.ChartPath, existing.Version, existing.Version+1)
	}

	// 이전 릴리스를 superseded로 변경
	existing.Status = "superseded"

	rel := &Release{
		Name:      u.ReleaseName,
		Namespace: u.cfg.Namespace,
		Chart:     u.ChartPath,
		Version:   existing.Version + 1,
		Status:    "deployed",
		Timestamp: time.Now(),
	}

	if !u.DryRun {
		u.cfg.Releases.Create(rel)
	}

	return rel, nil
}

// List는 list Action 구조체
type List struct {
	cfg       *Configuration
	AllFilter bool   // 모든 상태 포함
	Namespace string // 특정 네임스페이스 필터
}

func NewList(cfg *Configuration) *List {
	return &List{cfg: cfg}
}

func (l *List) Run() []*Release {
	ns := l.Namespace
	if ns == "" {
		ns = l.cfg.Namespace
	}
	if l.AllFilter {
		ns = "" // 모든 네임스페이스
	}
	return l.cfg.Releases.List(ns)
}

// =============================================================================
// Command: Cobra 스타일 CLI 디스패치 시뮬레이션
// 실제 Helm: spf13/cobra 라이브러리 사용
// 여기서는 표준 라이브러리만으로 동일한 패턴 구현
// =============================================================================

// Command는 cobra.Command를 간소화한 구조체
type Command struct {
	Use   string
	Short string
	// RunE는 실제 실행 로직
	RunE func(cmd *Command, args []string) error
	// subcommands
	commands []*Command
	// flags
	flags map[string]string
	// parent
	parent *Command
}

// NewCommand는 새 커맨드를 생성한다
func NewCommand(use, short string) *Command {
	return &Command{
		Use:   use,
		Short: short,
		flags: make(map[string]string),
	}
}

// AddCommand는 서브커맨드를 등록한다
// 실제 Helm: cmd.AddCommand(newInstallCmd, newUpgradeCmd, newListCmd, ...)
func (c *Command) AddCommand(cmds ...*Command) {
	for _, cmd := range cmds {
		cmd.parent = c
		c.commands = append(c.commands, cmd)
	}
}

// SetFlag는 플래그 값을 설정한다
func (c *Command) SetFlag(name, value string) {
	c.flags[name] = value
}

// GetFlag는 플래그 값을 반환한다
func (c *Command) GetFlag(name string) string {
	return c.flags[name]
}

// Execute는 커맨드 트리에서 적절한 서브커맨드를 찾아 실행한다
func (c *Command) Execute(args []string) error {
	// 글로벌 플래그 파싱
	// 불리언 플래그 목록 (값 인자를 받지 않음)
	boolFlags := map[string]bool{
		"debug": true, "dry-run": true, "install": true,
		"all-namespaces": true,
	}

	var cleanArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			flagName := strings.TrimPrefix(args[i], "--")
			parts := strings.SplitN(flagName, "=", 2)
			if len(parts) == 2 {
				c.SetFlag(parts[0], parts[1])
			} else if boolFlags[parts[0]] {
				c.SetFlag(parts[0], "true")
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				c.SetFlag(parts[0], args[i+1])
				i++
			} else {
				c.SetFlag(parts[0], "true")
			}
		} else {
			cleanArgs = append(cleanArgs, args[i])
		}
	}

	// 서브커맨드 탐색
	if len(cleanArgs) > 0 {
		for _, sub := range c.commands {
			if sub.Use == cleanArgs[0] {
				// 부모의 플래그를 서브커맨드에 전달
				for k, v := range c.flags {
					if _, exists := sub.flags[k]; !exists {
						sub.SetFlag(k, v)
					}
				}
				return sub.Execute(cleanArgs[1:])
			}
		}
	}

	// 현재 커맨드 실행
	if c.RunE != nil {
		return c.RunE(c, cleanArgs)
	}

	// 서브커맨드가 없으면 도움말 표시
	c.printHelp()
	return nil
}

func (c *Command) printHelp() {
	fmt.Printf("사용법: %s [command]\n\n", c.Use)
	fmt.Printf("  %s\n\n", c.Short)
	if len(c.commands) > 0 {
		fmt.Println("사용 가능한 명령:")
		for _, sub := range c.commands {
			fmt.Printf("  %-12s %s\n", sub.Use, sub.Short)
		}
	}
	fmt.Println()
}

// =============================================================================
// Root Command 구성: 실제 Helm의 NewRootCmd/newRootCmdWithConfig 패턴
// =============================================================================

func newRootCmd(actionConfig *Configuration) *Command {
	root := NewCommand("helm", "Kubernetes 패키지 매니저 (Helm v4 시뮬레이션)")

	// 서브커맨드 등록
	// 실제 Helm: cmd.AddCommand(newInstallCmd(actionConfig, out), ...)
	root.AddCommand(
		newInstallCmd(actionConfig),
		newUpgradeCmd(actionConfig),
		newListCmd(actionConfig),
	)

	return root
}

// newInstallCmd는 install 서브커맨드를 생성한다.
// 실제 Helm: pkg/cmd/install.go → newInstallCmd(cfg, out)
// Action 패턴: Command는 Configuration을 받아 Install Action에 전달한다.
func newInstallCmd(actionConfig *Configuration) *Command {
	client := NewInstall(actionConfig)

	cmd := NewCommand("install", "차트를 클러스터에 설치")
	cmd.RunE = func(cmd *Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("사용법: helm install <RELEASE_NAME> <CHART>")
		}
		client.ReleaseName = args[0]
		client.ChartPath = args[1]

		// --dry-run 플래그 처리
		if cmd.GetFlag("dry-run") == "true" {
			client.DryRun = true
		}

		// Configuration에 namespace 플래그 반영
		if ns := cmd.GetFlag("namespace"); ns != "" {
			actionConfig.Namespace = ns
		}

		rel, err := client.Run()
		if err != nil {
			return err
		}

		fmt.Printf("NAME: %s\n", rel.Name)
		fmt.Printf("NAMESPACE: %s\n", rel.Namespace)
		fmt.Printf("STATUS: %s\n", rel.Status)
		fmt.Printf("REVISION: %d\n", rel.Version)
		fmt.Printf("CHART: %s\n", rel.Chart)
		fmt.Printf("DEPLOYED AT: %s\n", rel.Timestamp.Format(time.RFC3339))
		return nil
	}

	return cmd
}

func newUpgradeCmd(actionConfig *Configuration) *Command {
	client := NewUpgrade(actionConfig)

	cmd := NewCommand("upgrade", "릴리스를 업그레이드")
	cmd.RunE = func(cmd *Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("사용법: helm upgrade <RELEASE_NAME> <CHART>")
		}
		client.ReleaseName = args[0]
		client.ChartPath = args[1]

		if cmd.GetFlag("install") == "true" {
			client.Install = true
		}

		if ns := cmd.GetFlag("namespace"); ns != "" {
			actionConfig.Namespace = ns
		}

		rel, err := client.Run()
		if err != nil {
			return err
		}

		fmt.Printf("릴리스 %q 가 업그레이드되었습니다.\n", rel.Name)
		fmt.Printf("REVISION: %d\n", rel.Version)
		fmt.Printf("STATUS: %s\n", rel.Status)
		return nil
	}

	return cmd
}

func newListCmd(actionConfig *Configuration) *Command {
	client := NewList(actionConfig)

	cmd := NewCommand("list", "릴리스 목록 조회")
	cmd.RunE = func(cmd *Command, args []string) error {
		if ns := cmd.GetFlag("namespace"); ns != "" {
			client.Namespace = ns
		}
		if cmd.GetFlag("all-namespaces") == "true" {
			client.AllFilter = true
		}

		releases := client.Run()
		if len(releases) == 0 {
			fmt.Println("릴리스가 없습니다.")
			return nil
		}

		fmt.Printf("%-15s %-15s %-10s %-10s %-20s\n",
			"NAME", "NAMESPACE", "REVISION", "STATUS", "CHART")
		for _, r := range releases {
			fmt.Printf("%-15s %-15s %-10d %-10s %-20s\n",
				r.Name, r.Namespace, r.Version, r.Status, r.Chart)
		}
		return nil
	}

	return cmd
}

// =============================================================================
// main: 실제 Helm의 cmd/helm/helm.go → main() 패턴
// =============================================================================

func main() {
	fmt.Println("=== Helm v4 아키텍처 PoC: CLI 디스패치 + Action 패턴 ===")
	fmt.Println()

	// 1) Configuration 생성 (모든 Action이 공유)
	// 실제 Helm: actionConfig := action.NewConfiguration()
	actionConfig := NewConfiguration()

	// 2) Root 커맨드 구성
	// 실제 Helm: cmd, err := helmcmd.NewRootCmd(os.Stdout, os.Args[1:], logSetup)
	rootCmd := newRootCmd(actionConfig)

	// 3) 커맨드 라인 인자로 실행 또는 데모 실행
	if len(os.Args) > 1 {
		// 실제 CLI 모드: os.Args 전달
		if err := rootCmd.Execute(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 데모 모드: 여러 시나리오 자동 실행
	runDemo(actionConfig)
}

func runDemo(cfg *Configuration) {
	fmt.Println("--- 데모: CLI 디스패치 시뮬레이션 ---")
	fmt.Println()

	rootCmd := newRootCmd(cfg)

	// 시나리오 1: helm (인자 없음 → 도움말)
	fmt.Println("[1] helm (도움말 표시)")
	rootCmd.Execute(nil)

	// 시나리오 2: helm install myapp ./nginx-chart
	fmt.Println("[2] helm install myapp ./nginx-chart")
	rootCmd = newRootCmd(cfg)
	if err := rootCmd.Execute([]string{"install", "myapp", "./nginx-chart"}); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}
	fmt.Println()

	// 시나리오 3: helm install myapp ./nginx-chart (중복 설치 → 에러)
	fmt.Println("[3] helm install myapp ./nginx-chart (중복 설치 → 에러)")
	rootCmd = newRootCmd(cfg)
	if err := rootCmd.Execute([]string{"install", "myapp", "./nginx-chart"}); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}
	fmt.Println()

	// 시나리오 4: helm install webapp ./react-chart --namespace production
	fmt.Println("[4] helm install webapp ./react-chart --namespace production")
	rootCmd = newRootCmd(cfg)
	if err := rootCmd.Execute([]string{"install", "webapp", "./react-chart", "--namespace", "production"}); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}
	fmt.Println()

	// 시나리오 5: helm upgrade myapp ./nginx-chart-v2
	fmt.Println("[5] helm upgrade myapp ./nginx-chart-v2")
	cfg.Namespace = "default" // 네임스페이스 복원
	rootCmd = newRootCmd(cfg)
	if err := rootCmd.Execute([]string{"upgrade", "myapp", "./nginx-chart-v2"}); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}
	fmt.Println()

	// 시나리오 6: helm upgrade newapp ./chart --install (없으면 설치)
	fmt.Println("[6] helm upgrade newapp ./chart --install (없으면 설치)")
	rootCmd = newRootCmd(cfg)
	if err := rootCmd.Execute([]string{"upgrade", "newapp", "./chart", "--install"}); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}
	fmt.Println()

	// 시나리오 7: helm list
	fmt.Println("[7] helm list")
	rootCmd = newRootCmd(cfg)
	if err := rootCmd.Execute([]string{"list", "--all-namespaces"}); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}
	fmt.Println()

	// 시나리오 8: --debug 플래그
	fmt.Println("[8] helm --debug install debug-app ./debug-chart")
	cfg.Debug = true
	rootCmd = newRootCmd(cfg)
	if err := rootCmd.Execute([]string{"--debug", "install", "debug-app", "./debug-chart"}); err != nil {
		fmt.Printf("  Error: %v\n", err)
	}
	cfg.Debug = false
	fmt.Println()

	fmt.Println("=== 데모 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Configuration 공유: 모든 Action(Install/Upgrade/List)이 동일한 Configuration 참조")
	fmt.Println("  2. Action 패턴: 각 명령은 독립된 구조체로, Run()을 호출하여 실행")
	fmt.Println("  3. CLI 디스패치: Root → SubCommand 트리에서 인자에 따라 적절한 Action 실행")
	fmt.Println("  4. Flag 전파: 글로벌 플래그(--debug, --namespace)는 모든 서브커맨드에 전파")
}
