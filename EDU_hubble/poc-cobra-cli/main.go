// SPDX-License-Identifier: Apache-2.0
// PoC: Cobra CLI 패턴 (Hubble CLI 구조 시뮬레이션)
//
// Hubble CLI는 Cobra 프레임워크로 서브커맨드 구조를 구현합니다.
// 이 PoC는 표준 라이브러리만으로 Cobra의 핵심 패턴을 시뮬레이션합니다.
//
// 실행 예시:
//   go run main.go observe --follow --verdict DROPPED
//   go run main.go status
//   go run main.go config view
//   go run main.go version

package main

import (
	"fmt"
	"os"
	"strings"
)

// ========================================
// 1. Command 구조 (Cobra의 핵심)
// ========================================

// Command는 Cobra의 cobra.Command를 간소화한 버전입니다.
//
// 실제 Hubble에서는:
//   rootCmd.AddCommand(observeCmd, statusCmd, listCmd, configCmd, versionCmd)
//   각 서브커맨드가 독립적인 플래그와 Run 함수를 가짐
type Command struct {
	Name        string
	Description string
	SubCommands map[string]*Command
	Flags       map[string]string // 플래그 이름 → 값
	Run         func(flags map[string]string)
}

func (c *Command) AddSubCommand(sub *Command) {
	if c.SubCommands == nil {
		c.SubCommands = make(map[string]*Command)
	}
	c.SubCommands[sub.Name] = sub
}

func (c *Command) Execute(args []string) {
	// 서브커맨드 탐색
	if len(args) > 0 {
		if sub, ok := c.SubCommands[args[0]]; ok {
			// 나머지 인자에서 플래그 파싱
			flags := parseFlags(args[1:])
			// 서브커맨드의 서브커맨드 확인
			if len(args) > 1 {
				if subsub, ok := sub.SubCommands[args[1]]; ok {
					flags = parseFlags(args[2:])
					if subsub.Run != nil {
						subsub.Run(flags)
						return
					}
				}
			}
			if sub.Run != nil {
				sub.Run(flags)
				return
			}
		}
	}

	// 루트 명령 또는 도움말 출력
	c.printHelp()
}

func (c *Command) printHelp() {
	fmt.Printf("mini-hubble - Hubble CLI 패턴 시뮬레이션\n\n")
	fmt.Printf("Usage:\n  go run main.go <command> [flags]\n\n")
	fmt.Printf("Available Commands:\n")
	for name, sub := range c.SubCommands {
		fmt.Printf("  %-12s %s\n", name, sub.Description)
	}
	fmt.Println()
}

func parseFlags(args []string) map[string]string {
	flags := make(map[string]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--") {
			key := strings.TrimPrefix(arg, "--")
			if strings.Contains(key, "=") {
				parts := strings.SplitN(key, "=", 2)
				flags[parts[0]] = parts[1]
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				flags[key] = args[i+1]
				i++
			} else {
				flags[key] = "true"
			}
		} else if strings.HasPrefix(arg, "-") {
			key := strings.TrimPrefix(arg, "-")
			flags[key] = "true"
		}
	}
	return flags
}

// ========================================
// 2. 커맨드 정의 (Hubble CLI 구조 미러링)
// ========================================

func buildRootCommand() *Command {
	root := &Command{
		Name:        "mini-hubble",
		Description: "Hubble CLI 시뮬레이션",
		SubCommands: make(map[string]*Command),
	}

	// ── observe 커맨드 ──
	// 실제: hubble observe [--follow] [--verdict X] [--source-pod X]
	observe := &Command{
		Name:        "observe",
		Description: "네트워크 Flow 관찰 (GetFlows RPC 시뮬레이션)",
		Run: func(flags map[string]string) {
			fmt.Println("=== hubble observe ===")
			fmt.Println()

			follow := flags["follow"] == "true" || flags["f"] == "true"
			verdict := flags["verdict"]
			sourcePod := flags["source-pod"]
			output := flags["o"]
			if output == "" {
				output = "compact"
			}

			fmt.Printf("  모드:       %s\n", map[bool]string{true: "실시간 스트림 (--follow)", false: "버퍼 조회 (--last 20)"}[follow])
			fmt.Printf("  출력 형식:  %s\n", output)

			if verdict != "" {
				fmt.Printf("  필터:       verdict=%s\n", verdict)
			}
			if sourcePod != "" {
				fmt.Printf("  필터:       source-pod=%s\n", sourcePod)
			}

			fmt.Println()
			fmt.Println("  [시뮬레이션 Flow 출력]")

			// 실제 Hubble: gRPC로 GetFlows 호출 → Stream 수신 → Printer로 포맷팅
			flows := []struct {
				src, dst, verdict, proto string
			}{
				{"default/frontend", "default/backend:8080", "FORWARDED", "TCP"},
				{"default/frontend", "kube-system/coredns:53", "FORWARDED", "DNS"},
				{"untrusted/scanner", "default/database:3306", "DROPPED", "TCP"},
				{"default/backend", "default/cache:6379", "FORWARDED", "TCP"},
			}

			for _, f := range flows {
				if verdict != "" && f.verdict != strings.ToUpper(verdict) {
					continue
				}
				if sourcePod != "" && !strings.Contains(f.src, sourcePod) {
					continue
				}

				icon := "→"
				if f.verdict == "DROPPED" {
					icon = "✗"
				}
				fmt.Printf("  %s %s %s [%s] %s\n", f.src, icon, f.dst, f.proto, f.verdict)
			}
		},
	}

	// ── status 커맨드 ──
	// 실제: hubble status [--server X]
	status := &Command{
		Name:        "status",
		Description: "서버 상태 조회 (ServerStatus RPC 시뮬레이션)",
		Run: func(flags map[string]string) {
			server := flags["server"]
			if server == "" {
				server = "localhost:4245"
			}

			fmt.Println("=== hubble status ===")
			fmt.Println()
			fmt.Printf("  Hubble Server: %s\n", server)
			fmt.Println()
			fmt.Println("  Healthcheck (via GRPC): Ok")
			fmt.Println("  Status:                 Ok")
			fmt.Println()
			fmt.Println("  Flows:          8,421/16,384 (51.4%)")
			fmt.Println("  Seen Flows:     142,857")
			fmt.Println("  Flows/s:        23.5")
			fmt.Println("  Connected:      3/3 nodes")
			fmt.Println("  Uptime:         2h34m12s")
			fmt.Println("  Version:        v1.18.6")
		},
	}

	// ── config 커맨드 (서브커맨드 포함) ──
	config := &Command{
		Name:        "config",
		Description: "설정 관리",
		SubCommands: make(map[string]*Command),
		Run: func(flags map[string]string) {
			fmt.Println("Usage: go run main.go config <view|get|set|reset>")
		},
	}

	configView := &Command{
		Name: "view",
		Run: func(flags map[string]string) {
			fmt.Println("=== hubble config view ===")
			fmt.Println()
			fmt.Println("  server: localhost:4245")
			fmt.Println("  timeout: 5s")
			fmt.Println("  request-timeout: 12s")
			fmt.Println("  tls: false")
			fmt.Println("  debug: false")
		},
	}
	config.AddSubCommand(configView)

	// ── version 커맨드 ──
	version := &Command{
		Name:        "version",
		Description: "버전 정보",
		Run: func(flags map[string]string) {
			fmt.Println("=== hubble version ===")
			fmt.Println()
			fmt.Println("  hubble v1.18.6")
			fmt.Println("  compiled with go1.25.6 on darwin/arm64")
		},
	}

	// ── list 커맨드 ──
	list := &Command{
		Name:        "list",
		Description: "클러스터 객체 목록",
		SubCommands: make(map[string]*Command),
		Run: func(flags map[string]string) {
			fmt.Println("Usage: go run main.go list <nodes|namespaces>")
		},
	}

	listNodes := &Command{
		Name: "nodes",
		Run: func(flags map[string]string) {
			fmt.Println("=== hubble list nodes ===")
			fmt.Println()
			fmt.Println("  NAME              STATUS      AGE     FLOWS/S")
			fmt.Println("  worker-node-1     Connected   5d      12.3")
			fmt.Println("  worker-node-2     Connected   5d      8.7")
			fmt.Println("  worker-node-3     Connected   5d      15.1")
		},
	}
	list.AddSubCommand(listNodes)

	root.AddSubCommand(observe)
	root.AddSubCommand(status)
	root.AddSubCommand(config)
	root.AddSubCommand(version)
	root.AddSubCommand(list)

	return root
}

func main() {
	fmt.Println("=== PoC: Cobra CLI 패턴 (Hubble CLI 구조) ===")
	fmt.Println()

	root := buildRootCommand()

	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("이 PoC는 Hubble CLI의 Cobra 기반 서브커맨드 구조를 보여줍니다.")
		fmt.Println()
		fmt.Println("사용 예시:")
		fmt.Println("  go run main.go observe --follow --verdict DROPPED")
		fmt.Println("  go run main.go observe --source-pod frontend")
		fmt.Println("  go run main.go status --server relay:4245")
		fmt.Println("  go run main.go config view")
		fmt.Println("  go run main.go list nodes")
		fmt.Println("  go run main.go version")
		fmt.Println()
		root.printHelp()
		fmt.Println("핵심 포인트:")
		fmt.Println("  - 서브커맨드 구조: hubble <command> [subcommand] [flags]")
		fmt.Println("  - 실제 Hubble: Cobra + Viper로 구현")
		fmt.Println("  - 각 커맨드가 독립적인 Run 함수와 플래그를 가짐")
		return
	}

	root.Execute(args)
}
