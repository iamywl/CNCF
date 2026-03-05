package main

import (
	"fmt"
	"os"
	"strings"
)

// =============================================================================
// Grafana 설정 로딩 시뮬레이션
//
// Grafana는 pkg/setting/setting.go에서 다계층 설정 로딩을 수행한다.
// 우선순위: defaults.ini → custom.ini → 환경변수(GF_*) → CLI 인자
// 각 계층은 이전 계층의 값을 덮어쓸 수 있다.
// =============================================================================

// ConfigSource는 설정값의 출처를 나타낸다.
type ConfigSource string

const (
	SourceDefault ConfigSource = "default"
	SourceINI     ConfigSource = "ini-file"
	SourceEnv     ConfigSource = "env-var"
	SourceCLI     ConfigSource = "cli-arg"
)

// ConfigValue는 설정값과 출처를 함께 저장한다.
type ConfigValue struct {
	Value  string
	Source ConfigSource
}

// ConfigStore는 섹션별 설정을 저장하는 계층적 구조이다.
// Grafana의 cfg.Raw (ini.File) 에 해당한다.
type ConfigStore struct {
	sections map[string]map[string]*ConfigValue
}

func NewConfigStore() *ConfigStore {
	return &ConfigStore{
		sections: make(map[string]map[string]*ConfigValue),
	}
}

func (c *ConfigStore) Set(section, key, value string, source ConfigSource) {
	if _, ok := c.sections[section]; !ok {
		c.sections[section] = make(map[string]*ConfigValue)
	}
	c.sections[section][key] = &ConfigValue{Value: value, Source: source}
}

func (c *ConfigStore) Get(section, key string) (*ConfigValue, bool) {
	if sec, ok := c.sections[section]; ok {
		if val, ok := sec[key]; ok {
			return val, true
		}
	}
	return nil, false
}

func (c *ConfigStore) GetString(section, key string) string {
	if val, ok := c.Get(section, key); ok {
		return val.Value
	}
	return ""
}

func (c *ConfigStore) AllSections() []string {
	var sections []string
	for s := range c.sections {
		sections = append(sections, s)
	}
	return sections
}

func (c *ConfigStore) AllKeys(section string) []string {
	if sec, ok := c.sections[section]; ok {
		var keys []string
		for k := range sec {
			keys = append(keys, k)
		}
		return keys
	}
	return nil
}

// =============================================================================
// INI 파서
// =============================================================================

// ParseINI는 INI 형식의 문자열을 파싱한다.
// Grafana는 go-ini/ini 라이브러리를 사용하지만, 여기서는 직접 구현한다.
func ParseINI(content string, store *ConfigStore, source ConfigSource) error {
	currentSection := "DEFAULT"
	lines := strings.Split(content, "\n")

	for lineNum, rawLine := range lines {
		line := strings.TrimSpace(rawLine)

		// 빈 줄 무시
		if line == "" {
			continue
		}

		// 주석 처리 (# 또는 ;)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		// 섹션 헤더 [section]
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}

		// key = value
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			return fmt.Errorf("line %d: invalid format (no '='): %s", lineNum+1, line)
		}

		key := strings.TrimSpace(line[:eqIdx])
		value := strings.TrimSpace(line[eqIdx+1:])

		// 인라인 주석 제거 (값 뒤의 # 또는 ;)
		// 단, 따옴표 안의 # ; 는 유지해야 한다 (간단한 구현)
		if !strings.HasPrefix(value, "\"") {
			if idx := strings.Index(value, " #"); idx >= 0 {
				value = strings.TrimSpace(value[:idx])
			}
			if idx := strings.Index(value, " ;"); idx >= 0 {
				value = strings.TrimSpace(value[:idx])
			}
		}

		store.Set(currentSection, key, value, source)
	}

	return nil
}

// =============================================================================
// 환경변수 로딩
// =============================================================================

// LoadEnvOverrides는 GF_ 접두사 환경변수를 설정에 반영한다.
// 매핑 규칙: GF_SECTION_KEY → [section] key
// Grafana: pkg/setting/setting.go의 applyEnvVariableOverrides()
func LoadEnvOverrides(store *ConfigStore, envVars map[string]string) {
	for envKey, envVal := range envVars {
		if !strings.HasPrefix(envKey, "GF_") {
			continue
		}

		// GF_ 접두사 제거
		remainder := envKey[3:]

		// 첫 번째 _ 로 섹션과 키를 분리
		// GF_SERVER_HTTP_PORT → section=server, key=http_port
		// GF_AUTH_BASIC_ENABLED → section=auth.basic, key=enabled
		parts := strings.SplitN(remainder, "_", 2)
		if len(parts) < 2 {
			continue
		}

		section := strings.ToLower(parts[0])
		key := strings.ToLower(parts[1])

		// 점(.) 구분자 처리: GF_AUTH_BASIC_ENABLED → [auth.basic] enabled
		// Grafana에서는 __로 점(.)을 표현한다 (e.g., GF_AUTH__BASIC_ENABLED)
		// 간단한 구현에서는 언더스코어를 그대로 사용
		store.Set(section, key, envVal, SourceEnv)
	}
}

// =============================================================================
// CLI 인자 로딩
// =============================================================================

// LoadCLIOverrides는 CLI 인자를 설정에 반영한다.
// 형식: section.key=value (e.g., server.http_port=9090)
func LoadCLIOverrides(store *ConfigStore, args []string) {
	for _, arg := range args {
		eqIdx := strings.Index(arg, "=")
		if eqIdx < 0 {
			continue
		}

		path := arg[:eqIdx]
		value := arg[eqIdx+1:]

		dotIdx := strings.Index(path, ".")
		if dotIdx < 0 {
			continue
		}

		section := path[:dotIdx]
		key := path[dotIdx+1:]

		store.Set(section, key, value, SourceCLI)
	}
}

// =============================================================================
// Cfg 구조체 - 타입 안전한 설정 접근
// =============================================================================

// Cfg는 Grafana의 pkg/setting/setting.go의 Cfg 구조체에 해당한다.
// 문자열 설정을 Go 타입으로 변환하여 제공한다.
type Cfg struct {
	// [server]
	HTTPAddr  string
	HTTPPort  string
	Protocol  string
	Domain    string
	RootURL   string
	StaticRootPath string

	// [paths]
	DataPath    string
	LogsPath    string
	PluginsPath string

	// [log]
	LogLevel string
	LogMode  string

	// [security]
	SecretKey       string
	AdminUser       string
	AdminPassword   string
	DisableGravatar bool

	// [database]
	DBType string
	DBHost string
	DBName string
	DBUser string

	// [auth]
	LoginCookieName   string
	LoginMaxLifetime  string
	DisableLoginForm  bool

	// 원본 데이터
	Raw *ConfigStore
}

// NewCfg는 ConfigStore에서 Cfg를 생성한다.
func NewCfg(store *ConfigStore) *Cfg {
	cfg := &Cfg{Raw: store}

	// [server]
	cfg.HTTPAddr = store.GetString("server", "http_addr")
	cfg.HTTPPort = store.GetString("server", "http_port")
	cfg.Protocol = store.GetString("server", "protocol")
	cfg.Domain = store.GetString("server", "domain")
	cfg.RootURL = store.GetString("server", "root_url")
	cfg.StaticRootPath = store.GetString("server", "static_root_path")

	// [paths]
	cfg.DataPath = store.GetString("paths", "data")
	cfg.LogsPath = store.GetString("paths", "logs")
	cfg.PluginsPath = store.GetString("paths", "plugins")

	// [log]
	cfg.LogLevel = store.GetString("log", "level")
	cfg.LogMode = store.GetString("log", "mode")

	// [security]
	cfg.SecretKey = store.GetString("security", "secret_key")
	cfg.AdminUser = store.GetString("security", "admin_user")
	cfg.AdminPassword = store.GetString("security", "admin_password")
	cfg.DisableGravatar = store.GetString("security", "disable_gravatar") == "true"

	// [database]
	cfg.DBType = store.GetString("database", "type")
	cfg.DBHost = store.GetString("database", "host")
	cfg.DBName = store.GetString("database", "name")
	cfg.DBUser = store.GetString("database", "user")

	// [auth]
	cfg.LoginCookieName = store.GetString("auth", "login_cookie_name")
	cfg.LoginMaxLifetime = store.GetString("auth", "login_maximum_lifetime")
	cfg.DisableLoginForm = store.GetString("auth", "disable_login_form") == "true"

	return cfg
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== Grafana 설정 로딩 시뮬레이션 ===")
	fmt.Println()

	store := NewConfigStore()

	// ─── 1단계: 기본값 (defaults.ini에 해당) ───
	fmt.Println("━━━ 1단계: 기본값 로딩 (defaults.ini) ━━━")

	defaultsINI := `
# Grafana defaults.ini 시뮬레이션
# 이 파일은 conf/defaults.ini에 해당한다

[paths]
data = /var/lib/grafana
logs = /var/log/grafana
plugins = /var/lib/grafana/plugins

[server]
protocol = http
http_addr = 0.0.0.0
http_port = 3000
domain = localhost
root_url = %(protocol)s://%(domain)s:%(http_port)s/
static_root_path = public

[database]
type = sqlite3
host = 127.0.0.1:3306
name = grafana
user = root

[security]
secret_key = SW2YcwTIb9zpOOhoPsMm
admin_user = admin
admin_password = admin
disable_gravatar = false

[auth]
login_cookie_name = grafana_session
login_maximum_lifetime = 30d
disable_login_form = false

[log]
mode = console
level = info
`

	err := ParseINI(defaultsINI, store, SourceDefault)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Println("  기본값 로딩 완료")
	fmt.Println()

	// ─── 2단계: 사용자 INI 파일 (custom.ini에 해당) ───
	fmt.Println("━━━ 2단계: 사용자 설정 로딩 (custom.ini) ━━━")

	customINI := `
# 사용자 커스텀 설정
# conf/custom.ini 또는 /etc/grafana/grafana.ini

[server]
http_port = 8080
domain = grafana.example.com
root_url = https://grafana.example.com/

[database]
type = postgres
host = db.example.com:5432
name = grafana_prod
user = grafana

[security]
secret_key = MyProductionSecretKey123!
admin_password = StrongP@ssw0rd

[log]
level = warn
mode = console file ; 콘솔과 파일 모두 출력
`

	err = ParseINI(customINI, store, SourceINI)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Println("  사용자 설정 로딩 완료")
	fmt.Println()

	// ─── 3단계: 환경변수 오버라이드 ───
	fmt.Println("━━━ 3단계: 환경변수 오버라이드 (GF_*) ━━━")

	// 실제 환경에서는 os.Environ()으로 읽지만, 시뮬레이션에서는 직접 지정
	envVars := map[string]string{
		"GF_SERVER_HTTP_PORT":       "9090",
		"GF_DATABASE_HOST":          "prod-db.internal:5432",
		"GF_SECURITY_SECRET_KEY":    "EnvOverrideSecretKey!",
		"GF_LOG_LEVEL":              "debug",
		"HOME":                      "/root",       // GF_ 접두사 아님 → 무시
		"GF_INVALID":                "no_key_part", // 섹션만 있고 키 없음 → 무시
		"GF_AUTH_DISABLE_LOGIN_FORM": "true",
	}

	// 데모를 위해 실제 환경변수도 하나 설정
	os.Setenv("GF_SERVER_PROTOCOL", "https")
	envVars["GF_SERVER_PROTOCOL"] = os.Getenv("GF_SERVER_PROTOCOL")

	fmt.Println("  시뮬레이션 환경변수:")
	for k, v := range envVars {
		if strings.HasPrefix(k, "GF_") {
			fmt.Printf("    %s = %s\n", k, v)
		}
	}

	LoadEnvOverrides(store, envVars)
	fmt.Println("  환경변수 오버라이드 완료")
	fmt.Println()

	// ─── 4단계: CLI 인자 오버라이드 ───
	fmt.Println("━━━ 4단계: CLI 인자 오버라이드 ━━━")

	cliArgs := []string{
		"server.http_addr=127.0.0.1",
		"log.level=error",
	}

	fmt.Println("  시뮬레이션 CLI 인자:")
	for _, arg := range cliArgs {
		fmt.Printf("    %s\n", arg)
	}

	LoadCLIOverrides(store, cliArgs)
	fmt.Println("  CLI 인자 오버라이드 완료")
	fmt.Println()

	// ─── 최종 설정 결과 ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("최종 병합 설정 (출처 추적 포함)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// 섹션 순서 지정 (출력 일관성)
	sectionOrder := []string{"server", "paths", "database", "security", "auth", "log"}

	for _, section := range sectionOrder {
		fmt.Printf("\n  [%s]\n", section)
		keys := store.AllKeys(section)

		// 키 정렬을 위한 간단한 정렬
		sortedKeys := sortStrings(keys)

		for _, key := range sortedKeys {
			val, _ := store.Get(section, key)
			sourceLabel := sourceLabel(val.Source)

			displayValue := val.Value
			// 비밀 값 마스킹
			if strings.Contains(key, "secret") || strings.Contains(key, "password") {
				if len(displayValue) > 4 {
					displayValue = displayValue[:4] + strings.Repeat("*", len(displayValue)-4)
				}
			}

			fmt.Printf("    %-25s = %-35s  ← %s\n", key, displayValue, sourceLabel)
		}
	}

	// ─── Cfg 구조체 생성 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Cfg 구조체 (타입 안전한 설정 접근)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	cfg := NewCfg(store)
	fmt.Printf("\n  Server:\n")
	fmt.Printf("    Protocol:   %s\n", cfg.Protocol)
	fmt.Printf("    HTTPAddr:   %s\n", cfg.HTTPAddr)
	fmt.Printf("    HTTPPort:   %s\n", cfg.HTTPPort)
	fmt.Printf("    Domain:     %s\n", cfg.Domain)
	fmt.Printf("    RootURL:    %s\n", cfg.RootURL)

	fmt.Printf("\n  Database:\n")
	fmt.Printf("    Type:       %s\n", cfg.DBType)
	fmt.Printf("    Host:       %s\n", cfg.DBHost)
	fmt.Printf("    Name:       %s\n", cfg.DBName)
	fmt.Printf("    User:       %s\n", cfg.DBUser)

	fmt.Printf("\n  Log:\n")
	fmt.Printf("    Level:      %s\n", cfg.LogLevel)
	fmt.Printf("    Mode:       %s\n", cfg.LogMode)

	fmt.Printf("\n  Auth:\n")
	fmt.Printf("    LoginCookieName:  %s\n", cfg.LoginCookieName)
	fmt.Printf("    LoginMaxLifetime: %s\n", cfg.LoginMaxLifetime)
	fmt.Printf("    DisableLoginForm: %v\n", cfg.DisableLoginForm)

	// ─── 설정 우선순위 요약 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("설정 우선순위 덮어쓰기 추적")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	overrideExamples := []struct {
		section string
		key     string
		desc    string
	}{
		{"server", "http_port", "HTTP 포트"},
		{"server", "http_addr", "바인드 주소"},
		{"log", "level", "로그 레벨"},
		{"database", "host", "DB 호스트"},
		{"security", "secret_key", "시크릿 키"},
	}

	fmt.Printf("\n  %-15s %-15s %-12s %-15s %-12s %-15s %-12s %-15s %s\n",
		"설정", "defaults", "", "custom.ini", "", "env-var", "", "cli-arg", "최종값")
	fmt.Println("  " + strings.Repeat("-", 130))

	for _, ex := range overrideExamples {
		val, _ := store.Get(ex.section, ex.key)

		// 각 단계별 값을 재구성 (시뮬레이션)
		defaults := getValueBySource(defaultsINI, ex.section, ex.key)
		custom := getValueBySource(customINI, ex.section, ex.key)
		env := getEnvValue(envVars, ex.section, ex.key)
		cli := getCLIValue(cliArgs, ex.section, ex.key)

		final := val.Value
		if strings.Contains(ex.key, "secret") {
			if len(final) > 4 {
				final = final[:4] + "****"
			}
			if len(defaults) > 4 {
				defaults = defaults[:4] + "****"
			}
			if len(custom) > 4 {
				custom = custom[:4] + "****"
			}
			if len(env) > 4 {
				env = env[:4] + "****"
			}
		}

		fmt.Printf("  %-15s %-15s  →  %-15s  →  %-15s  →  %-15s = %-15s [%s]\n",
			ex.desc,
			orDash(defaults), orDash(custom), orDash(env), orDash(cli),
			final, val.Source)
	}

	// 환경변수 정리
	os.Unsetenv("GF_SERVER_PROTOCOL")

	fmt.Println()
	fmt.Println("=== 설정 로딩 시뮬레이션 완료 ===")
}

// =============================================================================
// 유틸리티 함수
// =============================================================================

func sourceLabel(source ConfigSource) string {
	switch source {
	case SourceDefault:
		return "[기본값]"
	case SourceINI:
		return "[INI 파일]"
	case SourceEnv:
		return "[환경변수]"
	case SourceCLI:
		return "[CLI 인자]"
	default:
		return "[알 수 없음]"
	}
}

func sortStrings(s []string) []string {
	// 간단한 버블 정렬 (표준 라이브러리의 sort 대신)
	sorted := make([]string, len(s))
	copy(sorted, s)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// getValueBySource는 INI 문자열에서 섹션/키의 값을 찾는다 (추적용).
func getValueBySource(iniContent, section, key string) string {
	currentSection := ""
	for _, line := range strings.Split(iniContent, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if currentSection == section {
			eqIdx := strings.Index(line, "=")
			if eqIdx > 0 {
				k := strings.TrimSpace(line[:eqIdx])
				if k == key {
					v := strings.TrimSpace(line[eqIdx+1:])
					// 인라인 주석 제거
					if idx := strings.Index(v, " #"); idx >= 0 {
						v = strings.TrimSpace(v[:idx])
					}
					if idx := strings.Index(v, " ;"); idx >= 0 {
						v = strings.TrimSpace(v[:idx])
					}
					return v
				}
			}
		}
	}
	return ""
}

func getEnvValue(envVars map[string]string, section, key string) string {
	envKey := "GF_" + strings.ToUpper(section) + "_" + strings.ToUpper(key)
	return envVars[envKey]
}

func getCLIValue(args []string, section, key string) string {
	prefix := section + "." + key + "="
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return arg[len(prefix):]
		}
	}
	return ""
}
