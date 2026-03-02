// Cilium의 설정 체계 및 트러블슈팅 과정 시뮬레이션
//
// Cilium 실제 코드:
//   설정 구조체:  pkg/option/config.go → DaemonConfig
//   기본값:       pkg/defaults/defaults.go
//   Viper 통합:   github.com/spf13/viper
//
// 실행: go run main.go [--flags...]
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------
// DaemonConfig — pkg/option/config.go의 DaemonConfig 구조체 단순화
// 실제로는 ~400개 옵션이 있다
// -----------------------------------------------------------
type DaemonConfig struct {
	EnablePolicy         string `yaml:"enable-policy"`
	RoutingMode          string `yaml:"routing-mode"`
	TunnelProtocol       string `yaml:"tunnel-protocol"`
	EnableIPv4           bool   `yaml:"enable-ipv4"`
	EnableIPv6           bool   `yaml:"enable-ipv6"`
	EnableHubble         bool   `yaml:"enable-hubble"`
	KubeProxyReplacement bool   `yaml:"kube-proxy-replacement"`
	BPFLBAlgorithm       string `yaml:"bpf-lb-algorithm"`
	ClusterName          string `yaml:"cluster-name"`
	ClusterID            int    `yaml:"cluster-id"`
}

// 기본값 — pkg/defaults/defaults.go에서 정의
func defaultConfig() DaemonConfig {
	return DaemonConfig{
		EnablePolicy:         "default",
		RoutingMode:          "tunnel",
		TunnelProtocol:       "vxlan",
		EnableIPv4:           true,
		EnableIPv6:           true,
		EnableHubble:         false,
		KubeProxyReplacement: false,
		BPFLBAlgorithm:       "random",
		ClusterName:          "default",
		ClusterID:            0,
	}
}

// -----------------------------------------------------------
// 설정 로딩 (우선순위: 플래그 > 환경변수 > 파일 > 기본값)
// -----------------------------------------------------------
func loadConfig(configFile string, flags map[string]string) DaemonConfig {
	cfg := defaultConfig()
	source := make(map[string]string)
	for k := range flags {
		source[k] = "기본값"
	}

	fmt.Println("[설정 로딩 과정]")
	fmt.Println(strings.Repeat("─", 55))

	// Layer 1: 기본값 (이미 적용됨)
	fmt.Println("  (5) 기본값 적용 완료")

	// Layer 2: 설정 파일
	if configFile != "" {
		fmt.Printf("  (3) 설정 파일: %s\n", configFile)
		data, err := os.ReadFile(configFile)
		if err == nil {
			var fileCfg DaemonConfig
			if err := yaml.Unmarshal(data, &fileCfg); err == nil {
				if fileCfg.EnablePolicy != "" {
					cfg.EnablePolicy = fileCfg.EnablePolicy
					source["enable-policy"] = "설정 파일"
				}
				if fileCfg.RoutingMode != "" {
					cfg.RoutingMode = fileCfg.RoutingMode
					source["routing-mode"] = "설정 파일"
				}
				if fileCfg.TunnelProtocol != "" {
					cfg.TunnelProtocol = fileCfg.TunnelProtocol
					source["tunnel-protocol"] = "설정 파일"
				}
				cfg.EnableIPv4 = fileCfg.EnableIPv4
				cfg.EnableIPv6 = fileCfg.EnableIPv6
				cfg.EnableHubble = fileCfg.EnableHubble
				cfg.KubeProxyReplacement = fileCfg.KubeProxyReplacement
				if fileCfg.BPFLBAlgorithm != "" {
					cfg.BPFLBAlgorithm = fileCfg.BPFLBAlgorithm
					source["bpf-lb-algorithm"] = "설정 파일"
				}
				if fileCfg.ClusterName != "" {
					cfg.ClusterName = fileCfg.ClusterName
					source["cluster-name"] = "설정 파일"
				}
				if fileCfg.ClusterID != 0 {
					cfg.ClusterID = fileCfg.ClusterID
					source["cluster-id"] = "설정 파일"
				}
			}
		}
	}

	// Layer 3: 환경 변수 (CILIUM_ 접두사)
	envVars := map[string]*string{
		"CILIUM_ENABLE_POLICY":   &cfg.EnablePolicy,
		"CILIUM_ROUTING_MODE":    &cfg.RoutingMode,
		"CILIUM_CLUSTER_NAME":    &cfg.ClusterName,
		"CILIUM_BPF_LB_ALGORITHM": &cfg.BPFLBAlgorithm,
	}
	envBools := map[string]*bool{
		"CILIUM_ENABLE_HUBBLE": &cfg.EnableHubble,
	}

	for envKey, ptr := range envVars {
		if val := os.Getenv(envKey); val != "" {
			*ptr = val
			name := strings.ToLower(strings.TrimPrefix(envKey, "CILIUM_"))
			name = strings.ReplaceAll(name, "_", "-")
			source[name] = fmt.Sprintf("환경변수 (%s)", envKey)
			fmt.Printf("  (2) 환경변수: %s=%s\n", envKey, val)
		}
	}
	for envKey, ptr := range envBools {
		if val := os.Getenv(envKey); val != "" {
			*ptr = val == "true" || val == "1"
			name := strings.ToLower(strings.TrimPrefix(envKey, "CILIUM_"))
			name = strings.ReplaceAll(name, "_", "-")
			source[name] = fmt.Sprintf("환경변수 (%s)", envKey)
			fmt.Printf("  (2) 환경변수: %s=%s\n", envKey, val)
		}
	}

	// Layer 4: 커맨드라인 플래그 (최우선)
	for k, v := range flags {
		if v == "" {
			continue
		}
		switch k {
		case "enable-policy":
			cfg.EnablePolicy = v
			source[k] = "커맨드라인 플래그"
		case "routing-mode":
			cfg.RoutingMode = v
			source[k] = "커맨드라인 플래그"
		}
		fmt.Printf("  (1) 플래그: --%s=%s\n", k, v)
	}

	fmt.Println()
	return cfg
}

// -----------------------------------------------------------
// 트러블슈팅 시뮬레이션
// -----------------------------------------------------------
func simulateTroubleshooting(cfg DaemonConfig) {
	fmt.Println()
	fmt.Println("[트러블슈팅 시뮬레이션]")
	fmt.Println(strings.Repeat("═", 55))
	fmt.Println()
	fmt.Println("문제: Pod A → Pod B:80 통신이 안 됨")
	fmt.Println()

	// Step 1
	fmt.Println("Step 1: cilium-dbg endpoint list")
	fmt.Println(strings.Repeat("─", 55))
	fmt.Printf("  %-6s %-18s %-10s %-10s\n", "ID", "POD", "STATE", "IDENTITY")
	fmt.Printf("  %-6d %-18s %-10s %-10d\n", 1234, "pod-a", "ready", 48312)
	fmt.Printf("  %-6d %-18s %-10s %-10d\n", 5678, "pod-b", "ready", 48313)
	fmt.Println("  → 양쪽 Endpoint 모두 ready 상태 ✓")
	fmt.Println()

	// Step 2
	fmt.Println("Step 2: cilium-dbg identity list")
	fmt.Println(strings.Repeat("─", 55))
	fmt.Printf("  ID %-8d Labels: {app:frontend}\n", 48312)
	fmt.Printf("  ID %-8d Labels: {app:backend}\n", 48313)
	fmt.Println("  → Identity 정상 할당됨 ✓")
	fmt.Println()

	// Step 3
	fmt.Println("Step 3: cilium-dbg bpf policy get 5678")
	fmt.Println(strings.Repeat("─", 55))
	if cfg.EnablePolicy == "always" || cfg.EnablePolicy == "default" {
		fmt.Println("  IDENTITY   PORT     PROTO    ACTION")
		fmt.Println("  48312      80       TCP      ALLOW")
		fmt.Println("  → frontend(48312) → backend(5678):80 정책 존재 ✓")
	} else {
		fmt.Println("  (정책 비활성 — enable-policy=never)")
		fmt.Println("  → 정책이 꺼져있어 모든 트래픽 허용됨")
	}
	fmt.Println()

	// Step 4
	fmt.Println("Step 4: hubble observe --pod default/pod-a --verdict DROPPED")
	fmt.Println(strings.Repeat("─", 55))
	if !cfg.EnableHubble {
		fmt.Println("  ERROR: Hubble이 비활성 상태!")
		fmt.Println("  → --enable-hubble=true 또는 Helm values.yaml에서 hubble.enabled=true 설정 필요")
		fmt.Println()
		fmt.Println("  해결: cilium-dbg config EnableHubble=true")
	} else {
		fmt.Println("  [15:30:01] xx DROPPED  pod-a → pod-b:80  TCP  drop_reason=POLICY_DENIED")
		fmt.Println("  → 정책에 의해 차단됨. CiliumNetworkPolicy 확인 필요")
	}
	fmt.Println()

	// Step 5
	fmt.Println("Step 5: cilium-dbg bpf ct list global | grep 10.0.1.10")
	fmt.Println(strings.Repeat("─", 55))
	fmt.Println("  (엔트리 없음)")
	fmt.Println("  → CT 엔트리가 없다 = 패킷이 정책 단계에서 드롭되어 CT까지 도달하지 못함")
	fmt.Println()

	fmt.Println("[결론]")
	fmt.Println("  원인: CiliumNetworkPolicy에 frontend → backend:80 허용 규칙 누락")
	fmt.Println("  해결: kubectl apply -f allow-frontend-to-backend.yaml")
}

func main() {
	fmt.Println("=== Cilium 설정 체계 및 트러블슈팅 시뮬레이터 ===")
	fmt.Println()
	fmt.Println("실제 코드 위치:")
	fmt.Println("  설정 구조체: pkg/option/config.go")
	fmt.Println("  기본값:      pkg/defaults/defaults.go")
	fmt.Println()

	// 플래그 파싱
	configFile := flag.String("config", "", "설정 파일 경로 (YAML)")
	enablePolicy := flag.String("enable-policy", "", "정책 적용 모드 (default/always/never)")
	routingMode := flag.String("routing-mode", "", "라우팅 모드 (tunnel/native)")
	simulateTrouble := flag.Bool("simulate-trouble", false, "트러블슈팅 시뮬레이션")
	flag.Parse()

	flags := map[string]string{
		"enable-policy": *enablePolicy,
		"routing-mode":  *routingMode,
	}

	cfg := loadConfig(*configFile, flags)

	// 최종 설정 출력
	fmt.Println("[최종 적용된 설정]")
	fmt.Println(strings.Repeat("─", 55))
	fmt.Printf("  %-25s = %s\n", "enable-policy", cfg.EnablePolicy)
	fmt.Printf("  %-25s = %s\n", "routing-mode", cfg.RoutingMode)
	fmt.Printf("  %-25s = %s\n", "tunnel-protocol", cfg.TunnelProtocol)
	fmt.Printf("  %-25s = %v\n", "enable-ipv4", cfg.EnableIPv4)
	fmt.Printf("  %-25s = %v\n", "enable-ipv6", cfg.EnableIPv6)
	fmt.Printf("  %-25s = %v\n", "enable-hubble", cfg.EnableHubble)
	fmt.Printf("  %-25s = %v\n", "kube-proxy-replacement", cfg.KubeProxyReplacement)
	fmt.Printf("  %-25s = %s\n", "bpf-lb-algorithm", cfg.BPFLBAlgorithm)
	fmt.Printf("  %-25s = %s\n", "cluster-name", cfg.ClusterName)
	fmt.Printf("  %-25s = %d\n", "cluster-id", cfg.ClusterID)

	if *simulateTrouble {
		simulateTroubleshooting(cfg)
	}
}
