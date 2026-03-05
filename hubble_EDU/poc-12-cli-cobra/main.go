package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// =============================================================================
// Hubble CLI (Cobra) 시뮬레이션
//
// 실제 구현 참조:
//   hubble/cmd/root.go              - rootCmd, PersistentPreRunE, 서브커맨드 등록
//   hubble/cmd/observe/observe.go   - observeCmd, selectorFlags, formattingFlags
//   hubble/cmd/observe/flows.go     - newFlowsCmd, 플래그 파싱
//   hubble/cmd/common/config/flags.go - GlobalFlags, ServerFlags
//
// 핵심 개념:
//   1. Command 트리: root → observe/status/list/watch 서브커맨드
//   2. PersistentFlags: 부모 커맨드의 플래그를 자식이 상속
//   3. RunE: 에러를 반환하는 실행 함수
//   4. 설정 우선순위: flag > env > config > default
// =============================================================================

// --- Flag/Config 시스템 ---

// ConfigValue는 설정값과 그 소스(출처)
type ConfigValue struct {
	Value  string
	Source string // "default", "config", "env", "flag"
}

// Config는 설정 저장소 (Viper 시뮬레이션)
type Config struct {
	values map[string]ConfigValue
}

func NewConfig() *Config {
	return &Config{values: make(map[string]ConfigValue)}
}

func (c *Config) SetDefault(key, value string) {
	if _, exists := c.values[key]; !exists {
		c.values[key] = ConfigValue{Value: value, Source: "default"}
	}
}

func (c *Config) SetFromConfig(key, value string) {
	if v, exists := c.values[key]; !exists || v.Source == "default" {
		c.values[key] = ConfigValue{Value: value, Source: "config"}
	}
}

func (c *Config) SetFromEnv(key, value string) {
	if v, exists := c.values[key]; !exists || v.Source == "default" || v.Source == "config" {
		c.values[key] = ConfigValue{Value: value, Source: "env"}
	}
}

func (c *Config) SetFromFlag(key, value string) {
	// 플래그는 항상 최우선
	c.values[key] = ConfigValue{Value: value, Source: "flag"}
}

func (c *Config) Get(key string) string {
	if v, ok := c.values[key]; ok {
		return v.Value
	}
	return ""
}

func (c *Config) GetSource(key string) string {
	if v, ok := c.values[key]; ok {
		return v.Source
	}
	return "unset"
}

func (c *Config) GetBool(key string) bool {
	return c.Get(key) == "true"
}

// --- Flag 정의 ---

type FlagDef struct {
	Name      string
	Short     string
	Default   string
	Help      string
	IsBool    bool
	IsPersist bool // PersistentFlag
}

type FlagSet struct {
	Name  string
	Flags []FlagDef
}

// --- Command 시스템 ---
// 실제: cobra.Command

type Command struct {
	Use               string
	Short             string
	Long              string
	Parent            *Command
	SubCommands       []*Command
	PersistentFlags   *FlagSet
	LocalFlags        *FlagSet
	RunE              func(cmd *Command, args []string, cfg *Config) error
	PersistentPreRunE func(cmd *Command, args []string, cfg *Config) error
	SilenceErrors     bool
}

func (c *Command) AddCommand(cmds ...*Command) {
	for _, cmd := range cmds {
		cmd.Parent = c
		c.SubCommands = append(c.SubCommands, cmd)
	}
}

// Execute는 CLI를 실행
func (c *Command) Execute(args []string, cfg *Config) error {
	// 인자가 없으면 도움말 출력
	if len(args) == 0 {
		c.PrintHelp("")
		return nil
	}

	// 서브커맨드 찾기
	subcmdName := args[0]
	for _, sub := range c.SubCommands {
		if sub.Use == subcmdName {
			// PersistentPreRunE 실행 (부모 → 자식 순서)
			if c.PersistentPreRunE != nil {
				if err := c.PersistentPreRunE(c, args[1:], cfg); err != nil {
					return err
				}
			}

			// 플래그 파싱
			remaining := parseFlags(args[1:], sub, cfg)

			// 서브커맨드의 서브커맨드 확인
			if len(remaining) > 0 {
				for _, subsub := range sub.SubCommands {
					if subsub.Use == remaining[0] {
						if sub.PersistentPreRunE != nil {
							if err := sub.PersistentPreRunE(sub, remaining[1:], cfg); err != nil {
								return err
							}
						}
						remaining2 := parseFlags(remaining[1:], subsub, cfg)
						if subsub.RunE != nil {
							return subsub.RunE(subsub, remaining2, cfg)
						}
						subsub.PrintHelp("  ")
						return nil
					}
				}
			}

			// RunE 실행
			if sub.RunE != nil {
				return sub.RunE(sub, remaining, cfg)
			}
			sub.PrintHelp("  ")
			return nil
		}
	}

	return fmt.Errorf("unknown command: %s", subcmdName)
}

func parseFlags(args []string, cmd *Command, cfg *Config) []string {
	var remaining []string
	i := 0
	for i < len(args) {
		arg := args[i]
		if strings.HasPrefix(arg, "--") || strings.HasPrefix(arg, "-") {
			flagName := strings.TrimLeft(arg, "-")

			// 모든 플래그셋 검색
			flagSets := []*FlagSet{cmd.LocalFlags, cmd.PersistentFlags}
			if cmd.Parent != nil {
				flagSets = append(flagSets, cmd.Parent.PersistentFlags)
			}

			found := false
			for _, fs := range flagSets {
				if fs == nil {
					continue
				}
				for _, f := range fs.Flags {
					if f.Name == flagName || f.Short == flagName {
						if f.IsBool {
							cfg.SetFromFlag(f.Name, "true")
						} else if i+1 < len(args) {
							i++
							cfg.SetFromFlag(f.Name, args[i])
						}
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				remaining = append(remaining, arg)
			}
		} else {
			remaining = append(remaining, arg)
		}
		i++
	}
	return remaining
}

func (c *Command) PrintHelp(indent string) {
	fmt.Printf("%s%s - %s\n", indent, c.Use, c.Short)
	if c.Long != "" {
		fmt.Printf("%s  %s\n", indent, c.Long)
	}
	if len(c.SubCommands) > 0 {
		fmt.Printf("%s  서브커맨드:\n", indent)
		for _, sub := range c.SubCommands {
			fmt.Printf("%s    %-12s %s\n", indent, sub.Use, sub.Short)
		}
	}
	if c.PersistentFlags != nil {
		fmt.Printf("%s  전역 플래그:\n", indent)
		for _, f := range c.PersistentFlags.Flags {
			shortStr := ""
			if f.Short != "" {
				shortStr = fmt.Sprintf("-%s, ", f.Short)
			}
			fmt.Printf("%s    %s--%-15s %s (기본값: %s)\n", indent, shortStr, f.Name, f.Help, f.Default)
		}
	}
	if c.LocalFlags != nil {
		fmt.Printf("%s  로컬 플래그:\n", indent)
		for _, f := range c.LocalFlags.Flags {
			shortStr := ""
			if f.Short != "" {
				shortStr = fmt.Sprintf("-%s, ", f.Short)
			}
			fmt.Printf("%s    %s--%-15s %s (기본값: %s)\n", indent, shortStr, f.Name, f.Help, f.Default)
		}
	}
}

// --- Hubble CLI 빌드 ---

func buildHubbleCLI() *Command {
	// 실제: config.GlobalFlags
	globalFlags := &FlagSet{
		Name: "global",
		Flags: []FlagDef{
			{Name: "debug", Short: "D", Default: "false", Help: "디버그 모드 활성화", IsBool: true, IsPersist: true},
			{Name: "server", Default: "localhost:4245", Help: "Hubble 서버 주소", IsPersist: true},
			{Name: "tls", Default: "false", Help: "TLS 사용", IsBool: true, IsPersist: true},
			{Name: "config", Default: "", Help: "설정 파일 경로", IsPersist: true},
		},
	}

	// 실제: cmd.New() / cmd.NewWithViper()
	rootCmd := &Command{
		Use:           "hubble",
		Short:         "CLI",
		Long:          "Hubble은 Cilium 기반 클러스터에서 네트워크 트래픽을 관찰하는 도구입니다.",
		SilenceErrors: true,
		PersistentFlags: globalFlags,
		PersistentPreRunE: func(cmd *Command, args []string, cfg *Config) error {
			// 실제: validate.Flags(cmd, vp) + conn.Init(vp)
			server := cfg.Get("server")
			if server == "" {
				return fmt.Errorf("서버 주소가 설정되지 않았습니다")
			}
			fmt.Printf("  [연결 초기화] 서버: %s, TLS: %s\n", server, cfg.Get("tls"))
			return nil
		},
	}

	// === observe 커맨드 ===
	// 실제: observe.New(vp)
	observeCmd := &Command{
		Use:   "observe",
		Short: "Flow 이벤트 관찰",
		Long:  "Hubble 서버에서 네트워크 Flow 이벤트를 관찰합니다.",
		PersistentFlags: &FlagSet{
			Name: "formatting",
			Flags: []FlagDef{
				{Name: "output", Short: "o", Default: "compact", Help: "출력 형식 (compact/dict/json/table)"},
				{Name: "time-format", Default: "StampMilli", Help: "시간 표시 형식"},
				{Name: "print-node-name", Default: "false", Help: "노드 이름 출력", IsBool: true},
				{Name: "color", Default: "auto", Help: "색상 모드 (auto/always/never)"},
			},
		},
		LocalFlags: &FlagSet{
			Name: "selector",
			Flags: []FlagDef{
				{Name: "follow", Short: "f", Default: "false", Help: "실시간 Flow 스트리밍", IsBool: true},
				{Name: "last", Default: "20", Help: "최근 N개 Flow 조회"},
				{Name: "first", Default: "0", Help: "처음 N개 Flow 조회"},
				{Name: "since", Default: "", Help: "특정 시간 이후 Flow 조회"},
				{Name: "until", Default: "", Help: "특정 시간 이전 Flow 조회"},
				{Name: "all", Default: "false", Help: "모든 Flow 조회", IsBool: true},
				{Name: "type", Short: "t", Default: "", Help: "이벤트 타입 필터"},
				{Name: "verdict", Default: "", Help: "판정 결과 필터 (FORWARDED/DROPPED/...)"},
				{Name: "namespace", Short: "n", Default: "", Help: "네임스페이스 필터"},
			},
		},
		RunE: func(cmd *Command, args []string, cfg *Config) error {
			fmt.Printf("\n  [observe 실행]\n")
			fmt.Printf("    출력 형식: %s\n", cfg.Get("output"))
			fmt.Printf("    Follow: %s\n", cfg.Get("follow"))
			fmt.Printf("    Last: %s\n", cfg.Get("last"))
			fmt.Printf("    네임스페이스: %s\n", cfg.Get("namespace"))
			fmt.Printf("    Verdict: %s\n", cfg.Get("verdict"))

			// Flow 시뮬레이션 출력
			fmt.Println()
			if cfg.Get("output") == "table" {
				tw := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
				fmt.Fprintln(tw, "    TIMESTAMP\tSOURCE\tDESTINATION\tTYPE\tVERDICT\tSUMMARY")
				fmt.Fprintf(tw, "    %s\tdefault/frontend-0\tdefault/backend-0\tL3/L4\tFORWARDED\tTCP SYN\n", time.Now().Format(time.StampMilli))
				fmt.Fprintf(tw, "    %s\tprod/api-1\tkube-system/coredns\tL7/DNS\tFORWARDED\tQuery api.svc A\n", time.Now().Format(time.StampMilli))
				tw.Flush()
			} else {
				fmt.Printf("    %s: default/frontend-0 (ID:1234) -> default/backend-0 (ID:5678) L3/L4 FORWARDED (TCP SYN)\n",
					time.Now().Format(time.StampMilli))
				fmt.Printf("    %s: prod/api-1 (ID:9012) -> kube-system/coredns (ID:3456) L7/DNS FORWARDED (Query api.svc A)\n",
					time.Now().Format(time.StampMilli))
			}
			return nil
		},
	}

	// observe flows 서브커맨드
	// 실제: observe.newFlowsCmd(vp)
	flowsCmd := &Command{
		Use:   "flows",
		Short: "Flow 이벤트만 관찰",
		RunE: func(cmd *Command, args []string, cfg *Config) error {
			fmt.Printf("\n  [observe flows 실행] (observe와 동일하지만 명시적)\n")
			return nil
		},
	}

	// observe agent-events 서브커맨드
	agentEventsCmd := &Command{
		Use:   "agent-events",
		Short: "에이전트 이벤트 관찰",
		RunE: func(cmd *Command, args []string, cfg *Config) error {
			fmt.Printf("\n  [observe agent-events 실행]\n")
			fmt.Printf("    에이전트 이벤트 스트림 출력...\n")
			return nil
		},
	}

	observeCmd.AddCommand(flowsCmd, agentEventsCmd)

	// === status 커맨드 ===
	// 실제: status.New(vp)
	statusCmd := &Command{
		Use:   "status",
		Short: "서버 상태 조회",
		LocalFlags: &FlagSet{
			Name: "status",
			Flags: []FlagDef{
				{Name: "output", Short: "o", Default: "compact", Help: "출력 형식"},
			},
		},
		RunE: func(cmd *Command, args []string, cfg *Config) error {
			fmt.Printf("\n  [status 실행]\n")
			fmt.Printf("    Current/Max Flows: 8,192/16,384 (50.00%%)\n")
			fmt.Printf("    Flows/s: 123.45\n")
			fmt.Printf("    Connected Nodes: 3/3\n")
			return nil
		},
	}

	// === list 커맨드 ===
	// 실제: list.New(vp)
	listCmd := &Command{
		Use:   "list",
		Short: "리소스 목록 조회",
	}

	listNodesCmd := &Command{
		Use:   "nodes",
		Short: "노드 목록 조회",
		RunE: func(cmd *Command, args []string, cfg *Config) error {
			fmt.Printf("\n  [list nodes 실행]\n")
			tw := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintln(tw, "    NAME\tSTATUS\tAGE\tFLOWS/s")
			fmt.Fprintln(tw, "    node-0\tConnected\t5d\t42.1")
			fmt.Fprintln(tw, "    node-1\tConnected\t5d\t38.7")
			fmt.Fprintln(tw, "    node-2\tConnected\t3d\t55.3")
			tw.Flush()
			return nil
		},
	}

	listNamespacesCmd := &Command{
		Use:   "namespaces",
		Short: "네임스페이스 목록 조회",
		RunE: func(cmd *Command, args []string, cfg *Config) error {
			fmt.Printf("\n  [list namespaces 실행]\n")
			fmt.Printf("    NAMESPACE\n")
			fmt.Printf("    default\n")
			fmt.Printf("    kube-system\n")
			fmt.Printf("    monitoring\n")
			return nil
		},
	}

	listCmd.AddCommand(listNodesCmd, listNamespacesCmd)

	// === watch 커맨드 ===
	// 실제: watch.New(vp)
	watchCmd := &Command{
		Use:   "watch",
		Short: "리소스 변경 감시",
	}

	watchPeerCmd := &Command{
		Use:   "peer",
		Short: "피어(노드) 상태 변경 감시",
		RunE: func(cmd *Command, args []string, cfg *Config) error {
			fmt.Printf("\n  [watch peer 실행]\n")
			fmt.Printf("    피어 상태 스트리밍...\n")
			return nil
		},
	}

	watchCmd.AddCommand(watchPeerCmd)

	// 루트에 서브커맨드 등록
	// 실제: rootCmd.AddCommand(...)
	rootCmd.AddCommand(observeCmd, statusCmd, listCmd, watchCmd)

	return rootCmd
}

func main() {
	fmt.Println("=== Hubble CLI (Cobra) 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: hubble/cmd/root.go            - rootCmd, PersistentPreRunE")
	fmt.Println("참조: hubble/cmd/observe/observe.go - observeCmd, selectorFlags")
	fmt.Println("참조: hubble/cmd/observe/flows.go   - newFlowsCmd, 플래그 파싱")
	fmt.Println()

	rootCmd := buildHubbleCLI()

	// 설정 우선순위 데모
	fmt.Println("=== 설정 우선순위 데모 (flag > env > config > default) ===")
	fmt.Println()

	cfg := NewConfig()

	// 1. Default 설정
	cfg.SetDefault("server", "localhost:4245")
	cfg.SetDefault("output", "compact")
	cfg.SetDefault("follow", "false")
	cfg.SetDefault("last", "20")
	cfg.SetDefault("tls", "false")
	cfg.SetDefault("debug", "false")

	fmt.Printf("  1. Default: server=%s [%s]\n", cfg.Get("server"), cfg.GetSource("server"))

	// 2. Config 파일 (실제: viper.ReadInConfig())
	cfg.SetFromConfig("server", "hubble-relay.cilium.io:443")
	cfg.SetFromConfig("tls", "true")
	fmt.Printf("  2. Config:  server=%s [%s]\n", cfg.Get("server"), cfg.GetSource("server"))

	// 3. 환경 변수
	cfg.SetFromEnv("server", "hubble.example.com:4245")
	fmt.Printf("  3. Env:     server=%s [%s]\n", cfg.Get("server"), cfg.GetSource("server"))

	// 4. 플래그 (항상 최우선)
	cfg.SetFromFlag("server", "127.0.0.1:4245")
	fmt.Printf("  4. Flag:    server=%s [%s]\n", cfg.Get("server"), cfg.GetSource("server"))

	fmt.Println()
	fmt.Println("최종 설정 값들:")
	for _, key := range []string{"server", "tls", "output", "follow", "last", "debug"} {
		fmt.Printf("  %-10s = %-30s (출처: %s)\n", key, cfg.Get(key), cfg.GetSource(key))
	}
	fmt.Println()

	// CLI 커맨드 실행 시뮬레이션
	testCases := []struct {
		desc string
		args []string
	}{
		{"hubble (도움말)", []string{}},
		{"hubble status", []string{"status"}},
		{"hubble observe --last 5 -o table", []string{"observe", "--last", "5", "--output", "table"}},
		{"hubble observe -f -n kube-system --verdict DROPPED", []string{"observe", "--follow", "--namespace", "kube-system", "--verdict", "DROPPED"}},
		{"hubble list nodes", []string{"list", "nodes"}},
		{"hubble list namespaces", []string{"list", "namespaces"}},
	}

	for i, tc := range testCases {
		fmt.Printf("=== 테스트 %d: %s ===\n", i+1, tc.desc)

		// 각 테스트마다 새 config
		testCfg := NewConfig()
		testCfg.SetDefault("server", "localhost:4245")
		testCfg.SetDefault("output", "compact")
		testCfg.SetDefault("follow", "false")
		testCfg.SetDefault("last", "20")
		testCfg.SetDefault("tls", "false")
		testCfg.SetDefault("namespace", "")
		testCfg.SetDefault("verdict", "")

		err := rootCmd.Execute(tc.args, testCfg)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
		}
		fmt.Println()
	}

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Command 트리: root → observe/status/list/watch → 하위 커맨드")
	fmt.Println("  2. PersistentFlags: 부모의 --server, --debug 등을 모든 자식이 상속")
	fmt.Println("  3. PersistentPreRunE: 커맨드 실행 전 검증 (연결 초기화 등)")
	fmt.Println("  4. 설정 우선순위: flag > env > config > default (Viper 패턴)")
	fmt.Println("  5. SilenceErrors: 에러를 main에서 한 번만 출력 (중복 방지)")
}
