// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble 설정 우선순위 시스템
//
// Hubble은 Viper를 사용하여 4단계 설정 우선순위를 구현합니다:
//   1. CLI 플래그 (최우선)
//   2. 환경 변수 (HUBBLE_ 접두어)
//   3. 설정 파일 (config.yaml)
//   4. 기본값 (최하위)
//
// 실행: go run main.go
// 환경변수 테스트: MINI_HUBBLE_SERVER=env-relay:4245 go run main.go
// 플래그 테스트: go run main.go --server=flag-relay:4245

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ========================================
// 1. Config 시스템 (Viper 패턴 시뮬레이션)
// ========================================

// ConfigManager는 Viper의 핵심 기능을 시뮬레이션합니다.
//
// 실제 Hubble에서는:
//   viper.SetEnvPrefix("HUBBLE")
//   viper.AutomaticEnv()
//   viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
//   viper.SetConfigName("config")
//   viper.AddConfigPath(".")
type ConfigManager struct {
	defaults map[string]string // 기본값
	fileVals map[string]string // 설정 파일에서 읽은 값
	envVals  map[string]string // 환경 변수에서 읽은 값
	flagVals map[string]string // CLI 플래그에서 읽은 값

	envPrefix    string
	configPaths  []string
	resolveLog   []string // 어떤 소스에서 값을 가져왔는지 추적
}

func NewConfigManager(envPrefix string) *ConfigManager {
	return &ConfigManager{
		defaults:  make(map[string]string),
		fileVals:  make(map[string]string),
		envVals:   make(map[string]string),
		flagVals:  make(map[string]string),
		envPrefix: envPrefix,
	}
}

// SetDefault는 기본값을 설정합니다.
func (c *ConfigManager) SetDefault(key, value string) {
	c.defaults[key] = value
}

// AddConfigPath는 설정 파일 탐색 경로를 추가합니다.
func (c *ConfigManager) AddConfigPath(path string) {
	c.configPaths = append(c.configPaths, path)
}

// LoadConfigFile은 설정 파일을 로드합니다.
func (c *ConfigManager) LoadConfigFile() {
	for _, dir := range c.configPaths {
		configFile := filepath.Join(dir, "config.yaml")
		data, err := os.ReadFile(configFile)
		if err != nil {
			continue
		}
		// 간이 YAML 파서 (key: value 형식만 지원)
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				val := strings.TrimSpace(parts[1])
				c.fileVals[key] = val
			}
		}
		fmt.Printf("  [설정 파일] %s 로드됨\n", configFile)
		return
	}
	fmt.Println("  [설정 파일] 설정 파일 없음")
}

// LoadEnvVars는 환경 변수를 읽습니다.
// 규칙: HUBBLE_SERVER → server, HUBBLE_TLS_ALLOW_INSECURE → tls-allow-insecure
func (c *ConfigManager) LoadEnvVars(keys []string) {
	for _, key := range keys {
		envKey := c.envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
		if val, ok := os.LookupEnv(envKey); ok {
			c.envVals[key] = val
			fmt.Printf("  [환경 변수] %s=%s\n", envKey, val)
		}
	}
}

// LoadFlags는 CLI 플래그를 파싱합니다.
func (c *ConfigManager) LoadFlags(args []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		key := strings.TrimPrefix(arg, "--")
		if strings.Contains(key, "=") {
			parts := strings.SplitN(key, "=", 2)
			c.flagVals[parts[0]] = parts[1]
			fmt.Printf("  [CLI 플래그] --%s=%s\n", parts[0], parts[1])
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			c.flagVals[key] = args[i+1]
			fmt.Printf("  [CLI 플래그] --%s %s\n", key, args[i+1])
			i++
		} else {
			c.flagVals[key] = "true"
			fmt.Printf("  [CLI 플래그] --%s\n", key)
		}
	}
}

// Get은 설정값을 우선순위에 따라 반환합니다.
//
// 우선순위: Flag > Env > File > Default
// 이것이 Viper의 핵심 동작입니다.
func (c *ConfigManager) Get(key string) (string, string) {
	if val, ok := c.flagVals[key]; ok {
		return val, "CLI 플래그 (최우선)"
	}
	if val, ok := c.envVals[key]; ok {
		return val, "환경 변수"
	}
	if val, ok := c.fileVals[key]; ok {
		return val, "설정 파일"
	}
	if val, ok := c.defaults[key]; ok {
		return val, "기본값 (최하위)"
	}
	return "", "미설정"
}

// ========================================
// 2. 임시 설정 파일 생성
// ========================================

func createTempConfig() string {
	tmpDir := os.TempDir()
	configDir := filepath.Join(tmpDir, "mini-hubble-poc")
	os.MkdirAll(configDir, 0755)

	configContent := `# Mini-Hubble 설정 파일
# 실제 Hubble: ~/.hubble/config.yaml
server: file-relay:4245
timeout: 10s
tls: true
debug: false
`
	configFile := filepath.Join(configDir, "config.yaml")
	os.WriteFile(configFile, []byte(configContent), 0644)
	return configDir
}

// ========================================
// 3. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble 설정 우선순위 시스템 ===")
	fmt.Println()
	fmt.Println("Viper 패턴: Flag > Env > File > Default")
	fmt.Println()

	// 임시 설정 파일 생성
	configDir := createTempConfig()
	defer os.RemoveAll(configDir)

	// ConfigManager 초기화
	cfg := NewConfigManager("MINI_HUBBLE")

	// 기본값 설정 (4순위)
	cfg.SetDefault("server", "localhost:4245")
	cfg.SetDefault("timeout", "5s")
	cfg.SetDefault("tls", "false")
	cfg.SetDefault("debug", "false")
	cfg.SetDefault("request-timeout", "12s")

	fmt.Println("── 설정 소스 로드 ──")
	fmt.Println()

	// 설정 파일 로드 (3순위)
	cfg.AddConfigPath(configDir)
	cfg.AddConfigPath(".")
	cfg.LoadConfigFile()

	// 환경 변수 로드 (2순위)
	fmt.Println()
	cfg.LoadEnvVars([]string{"server", "timeout", "tls", "debug", "request-timeout"})

	// CLI 플래그 로드 (1순위)
	fmt.Println()
	cfg.LoadFlags(os.Args[1:])

	// 결과 출력
	fmt.Println()
	fmt.Println("── 최종 설정값 (우선순위 적용 후) ──")
	fmt.Println()

	keys := []string{"server", "timeout", "tls", "debug", "request-timeout"}
	for _, key := range keys {
		val, source := cfg.Get(key)
		fmt.Printf("  %-20s = %-25s ← %s\n", key, val, source)
	}

	fmt.Println()
	fmt.Println("-------------------------------------------")
	fmt.Println()
	fmt.Println("실험해보세요:")
	fmt.Println()
	fmt.Println("  # 기본값만 사용")
	fmt.Println("  go run main.go")
	fmt.Println()
	fmt.Println("  # 환경 변수로 server 오버라이드")
	fmt.Println("  MINI_HUBBLE_SERVER=env-relay:4245 go run main.go")
	fmt.Println()
	fmt.Println("  # CLI 플래그로 최우선 오버라이드")
	fmt.Println("  MINI_HUBBLE_SERVER=env-relay:4245 go run main.go --server=flag-relay:4245")
	fmt.Println()
	fmt.Println("  # 여러 설정 동시에")
	fmt.Println("  MINI_HUBBLE_TLS=true go run main.go --timeout=30s --debug=true")
	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - Viper: Flag > Env > File > Default 우선순위 자동 관리")
	fmt.Println("  - HUBBLE_ 접두어: 다른 환경 변수와 충돌 방지")
	fmt.Println("  - 대시→밑줄 변환: --tls-allow-insecure → HUBBLE_TLS_ALLOW_INSECURE")
}
