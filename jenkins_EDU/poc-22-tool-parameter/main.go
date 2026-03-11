package main

import (
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// Jenkins Tool Installation + Build Parameters 시뮬레이션
// =============================================================================
//
// Jenkins는 빌드에 필요한 도구(JDK, Maven, Gradle 등)의 자동 설치와
// 빌드 파라미터(String, Choice, Boolean 등)를 관리한다.
//
// 핵심 개념:
//   - ToolInstallation: 도구 버전 관리 및 자동 설치
//   - ToolInstaller: 도구 설치 전략 (다운로드, 명령실행 등)
//   - ParameterDefinition: 빌드 파라미터 정의
//   - ParameterValue: 실행 시 바인딩되는 파라미터 값
//
// 실제 코드 참조:
//   - core/src/main/java/hudson/tools/: 도구 관리
//   - core/src/main/java/hudson/model/ParameterDefinition.java
// =============================================================================

// --- Tool Installation ---

type ToolType string

const (
	ToolJDK    ToolType = "JDK"
	ToolMaven  ToolType = "Maven"
	ToolGradle ToolType = "Gradle"
	ToolNodeJS ToolType = "NodeJS"
	ToolGo     ToolType = "Go"
	ToolDocker ToolType = "Docker"
)

type InstallerType string

const (
	InstallerDownload InstallerType = "download"
	InstallerCommand  InstallerType = "command"
	InstallerExtract  InstallerType = "extract"
)

type ToolInstaller struct {
	Type    InstallerType
	URL     string
	Command string
	Label   string
}

type ToolInstallation struct {
	Name       string
	Type       ToolType
	Version    string
	Home       string // 설치 경로
	Installers []ToolInstaller
	EnvVars    map[string]string
	Installed  bool
}

func (t *ToolInstallation) Install() error {
	fmt.Printf("    [INSTALL] %s %s (%s)\n", t.Type, t.Name, t.Version)
	for _, installer := range t.Installers {
		switch installer.Type {
		case InstallerDownload:
			fmt.Printf("      Downloading from: %s\n", installer.URL)
		case InstallerCommand:
			fmt.Printf("      Running: %s\n", installer.Command)
		case InstallerExtract:
			fmt.Printf("      Extracting to: %s\n", t.Home)
		}
	}
	t.Installed = true
	fmt.Printf("      Installed at: %s\n", t.Home)
	return nil
}

func (t *ToolInstallation) GetEnvVars() map[string]string {
	env := make(map[string]string)
	for k, v := range t.EnvVars {
		env[k] = v
	}
	return env
}

// ToolManager는 전역 도구 관리자이다.
type ToolManager struct {
	tools map[string]*ToolInstallation
}

func NewToolManager() *ToolManager {
	return &ToolManager{tools: make(map[string]*ToolInstallation)}
}

func (tm *ToolManager) Register(tool *ToolInstallation) {
	tm.tools[tool.Name] = tool
}

func (tm *ToolManager) Get(name string) *ToolInstallation {
	return tm.tools[name]
}

func (tm *ToolManager) EnsureInstalled(name string) error {
	tool := tm.tools[name]
	if tool == nil {
		return fmt.Errorf("tool %s not found", name)
	}
	if !tool.Installed {
		return tool.Install()
	}
	fmt.Printf("    [CACHED] %s %s already installed\n", tool.Type, tool.Name)
	return nil
}

// --- Build Parameters ---

type ParamType string

const (
	ParamString  ParamType = "StringParameterDefinition"
	ParamChoice  ParamType = "ChoiceParameterDefinition"
	ParamBool    ParamType = "BooleanParameterDefinition"
	ParamFile    ParamType = "FileParameterDefinition"
	ParamPassword ParamType = "PasswordParameterDefinition"
	ParamRun     ParamType = "RunParameterDefinition"
)

type ParameterDefinition struct {
	Name         string
	Type         ParamType
	Description  string
	DefaultValue string
	Choices      []string // Choice 파라미터용
	Trim         bool     // String 파라미터 trim 옵션
}

type ParameterValue struct {
	Name  string
	Value string
}

// Validate는 파라미터 값을 검증한다.
func (pd *ParameterDefinition) Validate(value string) error {
	switch pd.Type {
	case ParamChoice:
		for _, c := range pd.Choices {
			if c == value {
				return nil
			}
		}
		return fmt.Errorf("invalid choice '%s', must be one of %v", value, pd.Choices)
	case ParamBool:
		if value != "true" && value != "false" {
			return fmt.Errorf("invalid boolean value: %s", value)
		}
	}
	if pd.Trim && pd.Type == ParamString {
		// 실제로는 trim 처리
	}
	return nil
}

func (pd *ParameterDefinition) GetDefaultValue() string {
	if pd.DefaultValue != "" {
		return pd.DefaultValue
	}
	if len(pd.Choices) > 0 {
		return pd.Choices[0]
	}
	if pd.Type == ParamBool {
		return "false"
	}
	return ""
}

// --- Build Configuration ---

type BuildConfig struct {
	JobName    string
	Tools      []string // 필요한 도구 이름
	Parameters []ParameterDefinition
}

type BuildExecution struct {
	Config      BuildConfig
	ParamValues map[string]string
	ToolEnvVars map[string]string
	StartTime   time.Time
	Status      string
}

func (be *BuildExecution) SetupTools(tm *ToolManager) error {
	fmt.Println("  도구 설치/확인:")
	for _, toolName := range be.Config.Tools {
		if err := tm.EnsureInstalled(toolName); err != nil {
			return err
		}
		tool := tm.Get(toolName)
		if tool != nil {
			for k, v := range tool.GetEnvVars() {
				be.ToolEnvVars[k] = v
			}
		}
	}
	return nil
}

func (be *BuildExecution) ValidateParams() error {
	for _, pd := range be.Config.Parameters {
		value, ok := be.ParamValues[pd.Name]
		if !ok {
			value = pd.GetDefaultValue()
			be.ParamValues[pd.Name] = value
		}
		if err := pd.Validate(value); err != nil {
			return fmt.Errorf("parameter %s: %v", pd.Name, err)
		}
	}
	return nil
}

func main() {
	fmt.Println("=== Jenkins Tool Installation + Build Parameters 시뮬레이션 ===")
	fmt.Println()

	// --- 도구 등록 ---
	fmt.Println("[1] 전역 도구 등록")
	fmt.Println(strings.Repeat("-", 60))

	tm := NewToolManager()

	tools := []*ToolInstallation{
		{
			Name: "JDK-17", Type: ToolJDK, Version: "17.0.9",
			Home: "/opt/java/jdk-17",
			Installers: []ToolInstaller{
				{Type: InstallerDownload, URL: "https://adoptium.net/temurin/releases/?version=17"},
				{Type: InstallerExtract},
			},
			EnvVars: map[string]string{"JAVA_HOME": "/opt/java/jdk-17", "PATH+JDK": "/opt/java/jdk-17/bin"},
		},
		{
			Name: "Maven-3.9", Type: ToolMaven, Version: "3.9.6",
			Home: "/opt/maven/3.9.6",
			Installers: []ToolInstaller{
				{Type: InstallerDownload, URL: "https://dlcdn.apache.org/maven/maven-3/3.9.6/binaries/apache-maven-3.9.6-bin.tar.gz"},
			},
			EnvVars: map[string]string{"MAVEN_HOME": "/opt/maven/3.9.6", "PATH+MVN": "/opt/maven/3.9.6/bin"},
		},
		{
			Name: "Go-1.22", Type: ToolGo, Version: "1.22.0",
			Home: "/opt/go/1.22.0",
			Installers: []ToolInstaller{
				{Type: InstallerDownload, URL: "https://go.dev/dl/go1.22.0.linux-amd64.tar.gz"},
			},
			EnvVars: map[string]string{"GOROOT": "/opt/go/1.22.0", "PATH+GO": "/opt/go/1.22.0/bin"},
		},
		{
			Name: "NodeJS-20", Type: ToolNodeJS, Version: "20.11.0",
			Home: "/opt/nodejs/20.11.0",
			Installers: []ToolInstaller{
				{Type: InstallerDownload, URL: "https://nodejs.org/dist/v20.11.0/node-v20.11.0-linux-x64.tar.xz"},
			},
			EnvVars: map[string]string{"NODE_HOME": "/opt/nodejs/20.11.0", "PATH+NODE": "/opt/nodejs/20.11.0/bin"},
		},
	}

	for _, tool := range tools {
		tm.Register(tool)
		fmt.Printf("  Registered: %s (%s %s) at %s\n", tool.Name, tool.Type, tool.Version, tool.Home)
	}
	fmt.Println()

	// --- 빌드 파라미터 정의 ---
	fmt.Println("[2] 빌드 파라미터 정의")
	fmt.Println(strings.Repeat("-", 60))

	buildConfig := BuildConfig{
		JobName: "my-app-build",
		Tools:   []string{"JDK-17", "Maven-3.9"},
		Parameters: []ParameterDefinition{
			{Name: "BRANCH", Type: ParamString, Description: "빌드할 브랜치", DefaultValue: "main", Trim: true},
			{Name: "ENVIRONMENT", Type: ParamChoice, Description: "배포 환경", Choices: []string{"dev", "staging", "production"}},
			{Name: "SKIP_TESTS", Type: ParamBool, Description: "테스트 건너뛰기", DefaultValue: "false"},
			{Name: "VERSION", Type: ParamString, Description: "릴리스 버전", DefaultValue: "1.0.0-SNAPSHOT"},
			{Name: "DEPLOY_PASSWORD", Type: ParamPassword, Description: "배포 비밀번호"},
		},
	}

	for _, pd := range buildConfig.Parameters {
		choices := ""
		if len(pd.Choices) > 0 {
			choices = fmt.Sprintf(" choices=%v", pd.Choices)
		}
		fmt.Printf("  %-18s [%s] default=%q%s\n", pd.Name, pd.Type, pd.GetDefaultValue(), choices)
	}
	fmt.Println()

	// --- 빌드 실행 시뮬레이션 ---
	fmt.Println("[3] 빌드 실행 #1 (기본값)")
	fmt.Println(strings.Repeat("-", 60))

	exec1 := &BuildExecution{
		Config:      buildConfig,
		ParamValues: map[string]string{},
		ToolEnvVars: make(map[string]string),
		StartTime:   time.Now(),
	}

	if err := exec1.ValidateParams(); err != nil {
		fmt.Printf("  Validation Error: %v\n", err)
	} else {
		fmt.Println("  파라미터 (기본값 사용):")
		for _, pd := range buildConfig.Parameters {
			fmt.Printf("    %s = %s\n", pd.Name, exec1.ParamValues[pd.Name])
		}
	}
	exec1.SetupTools(tm)
	fmt.Println("  환경변수:")
	for k, v := range exec1.ToolEnvVars {
		fmt.Printf("    %s=%s\n", k, v)
	}
	fmt.Println()

	// --- 커스텀 파라미터로 실행 ---
	fmt.Println("[4] 빌드 실행 #2 (커스텀 파라미터)")
	fmt.Println(strings.Repeat("-", 60))

	exec2 := &BuildExecution{
		Config: buildConfig,
		ParamValues: map[string]string{
			"BRANCH":      "release/2.0",
			"ENVIRONMENT": "production",
			"SKIP_TESTS":  "false",
			"VERSION":     "2.0.0",
		},
		ToolEnvVars: make(map[string]string),
		StartTime:   time.Now(),
	}

	if err := exec2.ValidateParams(); err != nil {
		fmt.Printf("  Validation Error: %v\n", err)
	} else {
		fmt.Println("  파라미터:")
		for name, val := range exec2.ParamValues {
			fmt.Printf("    %s = %s\n", name, val)
		}
	}
	exec2.SetupTools(tm)
	fmt.Println()

	// --- 검증 실패 케이스 ---
	fmt.Println("[5] 파라미터 검증 실패 케이스")
	fmt.Println(strings.Repeat("-", 60))

	exec3 := &BuildExecution{
		Config: buildConfig,
		ParamValues: map[string]string{
			"ENVIRONMENT": "invalid-env",
		},
		ToolEnvVars: make(map[string]string),
	}
	if err := exec3.ValidateParams(); err != nil {
		fmt.Printf("  [FAIL] %v\n", err)
	}

	exec4 := &BuildExecution{
		Config: buildConfig,
		ParamValues: map[string]string{
			"SKIP_TESTS": "maybe",
		},
		ToolEnvVars: make(map[string]string),
	}
	if err := exec4.ValidateParams(); err != nil {
		fmt.Printf("  [FAIL] %v\n", err)
	}
	fmt.Println()

	// --- 도구 캐시 (두 번째 설치는 캐시됨) ---
	fmt.Println("[6] 도구 캐시 테스트")
	fmt.Println(strings.Repeat("-", 60))
	tm.EnsureInstalled("JDK-17")   // 이미 설치됨
	tm.EnsureInstalled("Maven-3.9") // 이미 설치됨
	tm.EnsureInstalled("Go-1.22")   // 새로 설치
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
