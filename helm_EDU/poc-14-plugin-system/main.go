package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// =============================================================================
// Helm 플러그인 시스템 PoC
// =============================================================================
//
// 참조: internal/plugin/plugin.go, internal/plugin/loader.go,
//       internal/plugin/metadata.go, pkg/cmd/load_plugins.go
//
// Helm v4의 플러그인 시스템은 다음을 제공한다:
//   1. plugin.yaml 구조 — 플러그인 메타데이터 (이름, 타입, 런타임)
//   2. 플러그인 탐색 — HELM_PLUGINS 경로에서 plugin.yaml 스캔
//   3. 환경변수 전달 — HELM_PLUGIN_DIR, HELM_PLUGIN_NAME 등
//   4. 런타임 실행 — subprocess(os/exec) 또는 wasm 기반 실행
//
// Helm v4에서는 플러그인 API가 크게 변경되었다:
//   - apiVersion: v1 (이전: 없음 → "legacy")
//   - type: cli/v1, getter/v1, postrenderer/v1
//   - runtime: subprocess, extism/v1 (wasm)
// =============================================================================

// --- Plugin 인터페이스 ---
// Helm 소스: internal/plugin/plugin.go의 Plugin 인터페이스
type Plugin interface {
	Dir() string
	Metadata() Metadata
	Invoke(input *Input) (*Output, error)
}

// --- Metadata: 플러그인 메타데이터 ---
// Helm 소스: internal/plugin/metadata.go의 Metadata 구조체
type Metadata struct {
	APIVersion string `json:"apiVersion"`        // "v1" 또는 "legacy"
	Name       string `json:"name"`              // 플러그인 이름
	Type       string `json:"type"`              // cli/v1, getter/v1, postrenderer/v1
	Runtime    string `json:"runtime"`           // subprocess, extism/v1
	Version    string `json:"version,omitempty"` // SemVer 버전
	SourceURL  string `json:"sourceURL,omitempty"`
	Config     Config `json:"config,omitempty"`
}

// Config는 플러그인 타입별 설정이다.
type Config struct {
	// CLI 플러그인용
	Usage     string `json:"usage,omitempty"`
	ShortHelp string `json:"shortHelp,omitempty"`
	LongHelp  string `json:"longHelp,omitempty"`
	// Getter 플러그인용
	Protocols []string `json:"protocols,omitempty"`
}

// --- validPluginName: 플러그인 이름 유효성 검사 ---
// Helm 소스: internal/plugin/plugin.go의 validPluginName
var validPluginName = regexp.MustCompile("^[A-Za-z0-9_-]+$")

// Validate는 메타데이터를 검증한다.
func (m *Metadata) Validate() error {
	var errs []string

	if !validPluginName.MatchString(m.Name) {
		errs = append(errs, fmt.Sprintf("유효하지 않은 플러그인 이름: %q", m.Name))
	}
	if m.APIVersion == "" {
		errs = append(errs, "apiVersion이 비어 있음")
	}
	if m.Type == "" {
		errs = append(errs, "type이 비어 있음")
	}
	if m.Runtime == "" {
		errs = append(errs, "runtime이 비어 있음")
	}

	if len(errs) > 0 {
		return fmt.Errorf("플러그인 검증 실패: %s", strings.Join(errs, "; "))
	}
	return nil
}

// --- Input/Output: 플러그인 호출 메시지 ---
// Helm 소스: internal/plugin/plugin.go의 Input/Output
type Input struct {
	Message any
	Env     []string // "KEY=VALUE" 형식
	Args    []string
}

type Output struct {
	Message any
}

// --- Descriptor: 플러그인 검색 조건 ---
// Helm 소스: internal/plugin/descriptor.go
type Descriptor struct {
	Name string
	Type string
}

// --- SubprocessPlugin: subprocess 런타임 기반 플러그인 ---
type SubprocessPlugin struct {
	dir      string
	metadata Metadata
	Command  string   // 실행 명령어
	Args     []string // 기본 인수
}

func (p *SubprocessPlugin) Dir() string        { return p.dir }
func (p *SubprocessPlugin) Metadata() Metadata { return p.metadata }

// Invoke는 플러그인을 실행한다 (시뮬레이션).
// 실제 Helm에서는 os/exec로 외부 프로세스를 실행한다.
func (p *SubprocessPlugin) Invoke(input *Input) (*Output, error) {
	fmt.Printf("    [실행] %s %s %s\n", p.Command, strings.Join(p.Args, " "), strings.Join(input.Args, " "))
	fmt.Println("    [환경변수]")
	for _, env := range input.Env {
		fmt.Printf("      %s\n", env)
	}
	return &Output{Message: "OK"}, nil
}

// --- PluginManager: 플러그인 관리자 ---
type PluginManager struct {
	pluginDirs []string
	plugins    []Plugin
}

// NewPluginManager는 플러그인 디렉토리에서 플러그인을 로드한다.
func NewPluginManager(pluginDirs []string) *PluginManager {
	return &PluginManager{
		pluginDirs: pluginDirs,
		plugins:    make([]Plugin, 0),
	}
}

// LoadAll은 모든 플러그인을 로드한다.
// Helm 소스: internal/plugin/loader.go의 LoadAll
// 디렉토리 스캔 패턴: basedir/*/plugin.yaml
func (pm *PluginManager) LoadAll() error {
	for _, baseDir := range pm.pluginDirs {
		fmt.Printf("  플러그인 디렉토리 스캔: %s\n", baseDir)

		entries, err := os.ReadDir(baseDir)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("    디렉토리 없음, 건너뜀\n")
				continue
			}
			return err
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			pluginDir := filepath.Join(baseDir, entry.Name())
			pluginFile := filepath.Join(pluginDir, "plugin.yaml")

			if _, err := os.Stat(pluginFile); os.IsNotExist(err) {
				continue
			}

			// 실제 Helm은 여기서 plugin.yaml을 파싱한다
			fmt.Printf("    발견: %s/plugin.yaml\n", pluginDir)
		}
	}
	return nil
}

// RegisterPlugin은 플러그인을 등록한다.
func (pm *PluginManager) RegisterPlugin(p Plugin) error {
	// 중복 이름 검사
	// Helm 소스: internal/plugin/loader.go의 detectDuplicates
	for _, existing := range pm.plugins {
		if existing.Metadata().Name == p.Metadata().Name {
			return fmt.Errorf("플러그인 이름 중복: %q (경로: %s, %s)",
				p.Metadata().Name, existing.Dir(), p.Dir())
		}
	}
	pm.plugins = append(pm.plugins, p)
	return nil
}

// FindPlugins는 조건에 맞는 플러그인을 검색한다.
// Helm 소스: internal/plugin/loader.go의 FindPlugins
func (pm *PluginManager) FindPlugins(desc Descriptor) []Plugin {
	var result []Plugin
	for _, p := range pm.plugins {
		if desc.Name != "" && p.Metadata().Name != desc.Name {
			continue
		}
		if desc.Type != "" && p.Metadata().Type != desc.Type {
			continue
		}
		result = append(result, p)
	}
	return result
}

// FindPlugin은 단일 플러그인을 검색한다.
// Helm 소스: internal/plugin/loader.go의 FindPlugin
func (pm *PluginManager) FindPlugin(desc Descriptor) (Plugin, error) {
	plugins := pm.FindPlugins(desc)
	if len(plugins) == 0 {
		return nil, fmt.Errorf("플러그인을 찾을 수 없음: %+v", desc)
	}
	return plugins[0], nil
}

// --- 환경변수 구성 ---
// Helm이 플러그인 실행 시 전달하는 환경변수
func buildPluginEnv(p Plugin, helmBin string) []string {
	return []string{
		"HELM_PLUGIN_DIR=" + p.Dir(),
		"HELM_PLUGIN_NAME=" + p.Metadata().Name,
		"HELM_BIN=" + helmBin,
		"HELM_DEBUG=false",
		"HELM_REGISTRY_CONFIG=" + filepath.Join(os.Getenv("HOME"), ".config/helm/registry/config.json"),
		"HELM_REPOSITORY_CONFIG=" + filepath.Join(os.Getenv("HOME"), ".config/helm/repositories.yaml"),
		"HELM_REPOSITORY_CACHE=" + filepath.Join(os.Getenv("HOME"), ".cache/helm/repository"),
		"HELM_PLUGIN=" + p.Dir(),
		"HELM_NAMESPACE=default",
	}
}

func prettyJSON(v any) string {
	b, _ := json.MarshalIndent(v, "    ", "  ")
	return string(b)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              Helm 플러그인 시스템 PoC                         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("참조: internal/plugin/plugin.go, internal/plugin/loader.go,")
	fmt.Println("      internal/plugin/metadata.go")
	fmt.Println()

	// =================================================================
	// 1. plugin.yaml 구조 (v1 vs legacy)
	// =================================================================
	fmt.Println("1. plugin.yaml 구조")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println("\n  Helm v4 plugin.yaml (apiVersion: v1):")
	v1Plugin := map[string]any{
		"apiVersion": "v1",
		"name":       "my-plugin",
		"type":       "cli/v1",
		"runtime":    "subprocess",
		"version":    "1.0.0",
		"sourceURL":  "https://github.com/example/helm-my-plugin",
		"config": map[string]any{
			"usage":     "my-plugin [flags]",
			"shortHelp": "짧은 설명",
			"longHelp":  "자세한 설명",
		},
		"runtimeConfig": map[string]any{
			"command": []map[string]any{
				{"command": "./bin/my-plugin"},
			},
		},
	}
	fmt.Printf("    %s\n", prettyJSON(v1Plugin))

	fmt.Println("\n  Legacy plugin.yaml (apiVersion 없음):")
	legacyPlugin := map[string]any{
		"name":        "legacy-plugin",
		"usage":       "helm legacy-plugin",
		"description": "레거시 플러그인",
		"version":     "0.1.0",
		"command":     "$HELM_PLUGIN_DIR/bin/legacy",
		"hooks": map[string]string{
			"install": "cd $HELM_PLUGIN_DIR && scripts/install.sh",
			"update":  "cd $HELM_PLUGIN_DIR && scripts/update.sh",
		},
	}
	fmt.Printf("    %s\n", prettyJSON(legacyPlugin))

	fmt.Println("\n  플러그인 타입:")
	types := []struct {
		typ  string
		desc string
	}{
		{"cli/v1", "CLI 확장 — helm <plugin-name> 명령 추가"},
		{"getter/v1", "차트 다운로더 — 커스텀 프로토콜 지원 (s3://, gs:// 등)"},
		{"postrenderer/v1", "렌더링 후처리 — 매니페스트 변환"},
	}
	for _, t := range types {
		fmt.Printf("    %-20s : %s\n", t.typ, t.desc)
	}

	fmt.Println("\n  런타임 타입:")
	runtimes := []struct {
		rt   string
		desc string
	}{
		{"subprocess", "os/exec로 외부 프로세스 실행 (기본)"},
		{"extism/v1", "WebAssembly(Wasm) 런타임으로 실행"},
	}
	for _, r := range runtimes {
		fmt.Printf("    %-20s : %s\n", r.rt, r.desc)
	}

	// =================================================================
	// 2. 플러그인 등록 및 검색
	// =================================================================
	fmt.Println("\n2. 플러그인 등록 및 검색")
	fmt.Println(strings.Repeat("-", 60))

	pm := NewPluginManager([]string{"/tmp/helm-plugins-poc"})

	// 플러그인 시뮬레이션 등록
	plugins := []Plugin{
		&SubprocessPlugin{
			dir: "/tmp/helm-plugins-poc/helm-diff",
			metadata: Metadata{
				APIVersion: "v1",
				Name:       "diff",
				Type:       "cli/v1",
				Runtime:    "subprocess",
				Version:    "3.9.0",
				Config: Config{
					ShortHelp: "릴리스 간 차이 비교",
				},
			},
			Command: "./bin/diff",
		},
		&SubprocessPlugin{
			dir: "/tmp/helm-plugins-poc/helm-secrets",
			metadata: Metadata{
				APIVersion: "v1",
				Name:       "secrets",
				Type:       "cli/v1",
				Runtime:    "subprocess",
				Version:    "4.5.1",
				Config: Config{
					ShortHelp: "시크릿 암호화/복호화",
				},
			},
			Command: "./bin/secrets",
		},
		&SubprocessPlugin{
			dir: "/tmp/helm-plugins-poc/helm-s3",
			metadata: Metadata{
				APIVersion: "v1",
				Name:       "s3",
				Type:       "getter/v1",
				Runtime:    "subprocess",
				Version:    "0.15.1",
				Config: Config{
					Protocols: []string{"s3"},
					ShortHelp: "S3 차트 리포지토리",
				},
			},
			Command: "./bin/helm-s3",
		},
		&SubprocessPlugin{
			dir: "/tmp/helm-plugins-poc/helm-kustomize",
			metadata: Metadata{
				APIVersion: "v1",
				Name:       "kustomize",
				Type:       "postrenderer/v1",
				Runtime:    "subprocess",
				Version:    "1.0.0",
				Config: Config{
					ShortHelp: "kustomize 기반 후처리",
				},
			},
			Command: "kustomize",
			Args:    []string{"build"},
		},
	}

	for _, p := range plugins {
		md := p.Metadata()
		if err := md.Validate(); err != nil {
			fmt.Printf("  검증 실패: %v\n", err)
			continue
		}
		if err := pm.RegisterPlugin(p); err != nil {
			fmt.Printf("  등록 실패: %v\n", err)
			continue
		}
		fmt.Printf("  등록: %-15s (type: %-18s version: %s)\n",
			p.Metadata().Name, p.Metadata().Type, p.Metadata().Version)
	}

	// 검색 테스트
	fmt.Println("\n  CLI 플러그인 검색 (type=cli/v1):")
	cliPlugins := pm.FindPlugins(Descriptor{Type: "cli/v1"})
	for _, p := range cliPlugins {
		fmt.Printf("    - %s: %s\n", p.Metadata().Name, p.Metadata().Config.ShortHelp)
	}

	fmt.Println("\n  Getter 플러그인 검색 (type=getter/v1):")
	getterPlugins := pm.FindPlugins(Descriptor{Type: "getter/v1"})
	for _, p := range getterPlugins {
		fmt.Printf("    - %s: protocols=%v\n", p.Metadata().Name, p.Metadata().Config.Protocols)
	}

	fmt.Println("\n  이름으로 검색 (name=diff):")
	diffPlugin, err := pm.FindPlugin(Descriptor{Name: "diff"})
	if err == nil {
		fmt.Printf("    발견: %s (dir: %s)\n", diffPlugin.Metadata().Name, diffPlugin.Dir())
	}

	// =================================================================
	// 3. 플러그인 실행 (환경변수 전달)
	// =================================================================
	fmt.Println("\n3. 플러그인 실행 (환경변수 전달)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  helm diff upgrade my-release ./my-chart 실행 시뮬레이션:")

	if diffPlugin != nil {
		env := buildPluginEnv(diffPlugin, "/usr/local/bin/helm")
		input := &Input{
			Args:    []string{"upgrade", "my-release", "./my-chart"},
			Env:     env,
			Message: nil,
		}
		_, _ = diffPlugin.Invoke(input)
	}

	// =================================================================
	// 4. 플러그인 이름 검증
	// =================================================================
	fmt.Println("\n4. 플러그인 이름 검증")
	fmt.Println(strings.Repeat("-", 60))

	nameTests := []struct {
		name  string
		valid bool
	}{
		{"diff", true},
		{"my-plugin", true},
		{"s3_getter", true},
		{"plugin v2", false},   // 공백
		{"plugin/sub", false},  // 슬래시
		{"plugin.ext", false},  // 점
		{"", false},            // 빈 문자열
	}

	for _, t := range nameTests {
		valid := validPluginName.MatchString(t.name)
		status := "유효"
		if !valid {
			status = "무효"
		}
		fmt.Printf("  %-20q → %s\n", t.name, status)
	}

	// =================================================================
	// 5. 중복 검출
	// =================================================================
	fmt.Println("\n5. 중복 플러그인 검출")
	fmt.Println(strings.Repeat("-", 60))

	duplicatePlugin := &SubprocessPlugin{
		dir:      "/other/path/helm-diff",
		metadata: Metadata{APIVersion: "v1", Name: "diff", Type: "cli/v1", Runtime: "subprocess"},
		Command:  "./bin/diff-v2",
	}

	err = pm.RegisterPlugin(duplicatePlugin)
	if err != nil {
		fmt.Printf("  오류 (예상): %v\n", err)
	}

	// =================================================================
	// 6. 플러그인 디렉토리 구조
	// =================================================================
	fmt.Println("\n6. 플러그인 디렉토리 구조")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  HELM_PLUGINS 디렉토리 구조:
  ~/.local/share/helm/plugins/
  ├── helm-diff/
  │   ├── plugin.yaml          ← 플러그인 메타데이터
  │   ├── bin/
  │   │   └── diff              ← 실행 바이너리
  │   └── scripts/
  │       └── install.sh        ← 설치 훅
  ├── helm-secrets/
  │   ├── plugin.yaml
  │   └── bin/secrets
  └── helm-s3/
      ├── plugin.yaml
      └── bin/helm-s3

  플러그인 탐색 흐름:
  ┌─────────────────────────────────────────┐
  │  1. HELM_PLUGINS 환경변수 확인            │
  │     (기본: ~/.local/share/helm/plugins)  │
  │                                          │
  │  2. basedir/*/plugin.yaml 패턴 스캔       │
  │     filepath.Glob(scanpath)              │
  │                                          │
  │  3. 각 plugin.yaml 파싱:                  │
  │     a. apiVersion 확인 (v1 vs legacy)     │
  │     b. Metadata 생성                      │
  │     c. Validate() 실행                    │
  │                                          │
  │  4. 런타임 매핑:                           │
  │     "subprocess" → RuntimeSubprocess      │
  │     "extism/v1"  → RuntimeExtismV1       │
  │                                          │
  │  5. Plugin 인스턴스 생성                   │
  │     runtime.CreatePlugin(dir, metadata)   │
  │                                          │
  │  6. 중복 이름 검출                         │
  │     detectDuplicates(plugins)             │
  └─────────────────────────────────────────┘

  Plugin.Invoke 호출 흐름:
  ┌──────────────┐     ┌─────────────────┐
  │  helm diff   │     │  SubprocessPlugin│
  │  upgrade ... │────>│  .Invoke(input)  │
  └──────────────┘     └────────┬────────┘
                                │
                    ┌───────────v───────────┐
                    │  os/exec.Command       │
                    │  - env: HELM_PLUGIN_*  │
                    │  - stdin: input.Message│
                    │  - stdout → output     │
                    └───────────────────────┘
`)
}
