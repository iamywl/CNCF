package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// =============================================================================
// Terraform 모듈 시스템 시뮬레이션
// =============================================================================
//
// Terraform 모듈은 재사용 가능한 인프라 구성 단위이다.
// 모듈은 다른 모듈을 호출하여 모듈 트리를 형성하고,
// 변수(Variable)로 입력을 받고 출력값(Output)을 반환한다.
//
// 실제 Terraform 소스:
//   - internal/configs/module.go: 모듈 설정
//   - internal/configs/module_call.go: 모듈 호출 정의
//   - internal/initwd/module_install.go: 모듈 설치
//   - internal/modsdir/manifest.go: 모듈 매니페스트 (modules.json)
//   - internal/configs/config.go: 설정 트리 구축
//
// 이 PoC에서 구현하는 핵심 개념:
//   1. 모듈 소스 타입 (local, registry, git-like)
//   2. 모듈 설치 (.terraform/modules/)
//   3. 모듈 매니페스트 (modules.json)
//   4. 변수 전달 (parent → child)
//   5. 출력값 전달 (child → parent)
//   6. 재귀적 모듈 트리 구축

// =============================================================================
// 1. 모듈 소스 타입
// =============================================================================

// ModuleSourceType은 모듈 소스의 종류를 나타낸다.
type ModuleSourceType int

const (
	SourceLocal    ModuleSourceType = iota // "./modules/network" (로컬 경로)
	SourceRegistry                        // "hashicorp/vpc/aws" (레지스트리)
	SourceGit                             // "git::https://..." (Git URL)
)

var sourceTypeNames = map[ModuleSourceType]string{
	SourceLocal:    "local",
	SourceRegistry: "registry",
	SourceGit:      "git",
}

// ModuleSource는 모듈 소스 정보를 나타낸다.
type ModuleSource struct {
	Type    ModuleSourceType
	Raw     string // 원본 소스 문자열
	Version string // 레지스트리 모듈의 버전 제약
}

// ParseModuleSource는 소스 문자열을 파싱한다.
func ParseModuleSource(source string) ModuleSource {
	ms := ModuleSource{Raw: source}

	switch {
	case strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../"):
		ms.Type = SourceLocal
	case strings.HasPrefix(source, "git::"):
		ms.Type = SourceGit
	default:
		// namespace/name/provider 형식이면 레지스트리
		parts := strings.Split(source, "/")
		if len(parts) == 3 {
			ms.Type = SourceRegistry
		} else {
			ms.Type = SourceLocal
		}
	}

	return ms
}

// =============================================================================
// 2. 모듈 설정 구조체
// =============================================================================

// Variable은 모듈의 입력 변수를 나타낸다.
type Variable struct {
	Name        string
	Type        string
	Default     string
	Description string
}

// Output은 모듈의 출력값을 나타낸다.
type Output struct {
	Name        string
	Value       string // 표현식 (예: "aws_vpc.main.id")
	Description string
}

// ResourceDef는 모듈의 리소스 정의를 나타낸다.
type ResourceDef struct {
	Type       string
	Name       string
	Attributes map[string]string
}

// ModuleCall은 모듈 블록(module "name" { ... })을 나타낸다.
// Terraform의 configs.ModuleCall에 대응한다.
type ModuleCall struct {
	Name    string
	Source  ModuleSource
	Inputs  map[string]string // 전달할 변수 값
}

// ModuleConfig는 하나의 모듈 설정을 나타낸다.
// Terraform의 configs.Module에 대응한다.
type ModuleConfig struct {
	Path        string           // 모듈 경로 (예: "root", "root.network")
	Source      string           // 소스 경로
	Variables   []Variable       // 입력 변수
	Outputs     []Output         // 출력값
	Resources   []ResourceDef    // 리소스 정의
	ModuleCalls []ModuleCall     // 자식 모듈 호출
}

// =============================================================================
// 3. 모듈 트리 (Module Tree)
// =============================================================================

// ModuleTreeNode는 모듈 트리의 노드이다.
// Terraform의 configs.Config에 대응한다.
type ModuleTreeNode struct {
	Config        *ModuleConfig
	Children      map[string]*ModuleTreeNode
	Parent        *ModuleTreeNode
	ResolvedVars  map[string]string // 해결된 변수 값
	ResolvedOuts  map[string]string // 해결된 출력 값
}

// =============================================================================
// 4. 모듈 매니페스트 (modules.json)
// =============================================================================

// ManifestEntry는 modules.json의 하나의 항목이다.
// Terraform의 modsdir.Record에 대응한다.
type ManifestEntry struct {
	Key     string `json:"Key"`
	Source  string `json:"Source"`
	Version string `json:"Version,omitempty"`
	Dir     string `json:"Dir"`
}

// Manifest는 modules.json 전체이다.
type Manifest struct {
	Modules []ManifestEntry `json:"Modules"`
}

// =============================================================================
// 5. 모듈 설치 관리자
// =============================================================================

// ModuleInstaller는 모듈 설치를 관리한다.
// Terraform의 internal/initwd/module_install.go에 대응한다.
type ModuleInstaller struct {
	BaseDir      string // .terraform/modules/
	Manifest     *Manifest
	ModuleStore  map[string]*ModuleConfig // 사용 가능한 모듈 (시뮬레이션)
}

// NewModuleInstaller는 새로운 모듈 설치 관리자를 생성한다.
func NewModuleInstaller(baseDir string) *ModuleInstaller {
	modulesDir := filepath.Join(baseDir, ".terraform", "modules")
	os.MkdirAll(modulesDir, 0755)

	return &ModuleInstaller{
		BaseDir:     modulesDir,
		Manifest:    &Manifest{},
		ModuleStore: make(map[string]*ModuleConfig),
	}
}

// RegisterModule은 사용 가능한 모듈을 등록한다 (시뮬레이션).
func (mi *ModuleInstaller) RegisterModule(source string, config *ModuleConfig) {
	mi.ModuleStore[source] = config
}

// InstallModule은 모듈을 설치한다.
func (mi *ModuleInstaller) InstallModule(key, source, version string) (*ModuleConfig, error) {
	parsedSource := ParseModuleSource(source)

	fmt.Printf("    %-10s %s", sourceTypeNames[parsedSource.Type], source)
	if version != "" {
		fmt.Printf(" v%s", version)
	}
	fmt.Println()

	// 모듈 설정 찾기
	config, exists := mi.ModuleStore[source]
	if !exists {
		return nil, fmt.Errorf("모듈 %q 를 찾을 수 없습니다", source)
	}

	// 설치 디렉토리 생성
	installDir := filepath.Join(mi.BaseDir, key)
	os.MkdirAll(installDir, 0755)

	// 모듈 파일 생성 (시뮬레이션)
	writeModuleFiles(installDir, config)

	// 매니페스트에 등록
	mi.Manifest.Modules = append(mi.Manifest.Modules, ManifestEntry{
		Key:     key,
		Source:  source,
		Version: version,
		Dir:     installDir,
	})

	return config, nil
}

// WriteManifest는 modules.json을 기록한다.
func (mi *ModuleInstaller) WriteManifest() error {
	data, err := json.MarshalIndent(mi.Manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(mi.BaseDir, "modules.json"), data, 0644)
}

func writeModuleFiles(dir string, config *ModuleConfig) {
	// main.tf (시뮬레이션)
	var sb strings.Builder
	for _, r := range config.Resources {
		sb.WriteString(fmt.Sprintf("resource \"%s\" \"%s\" {\n", r.Type, r.Name))
		for k, v := range r.Attributes {
			sb.WriteString(fmt.Sprintf("  %s = \"%s\"\n", k, v))
		}
		sb.WriteString("}\n\n")
	}
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(sb.String()), 0644)

	// variables.tf
	var varSb strings.Builder
	for _, v := range config.Variables {
		varSb.WriteString(fmt.Sprintf("variable \"%s\" {\n", v.Name))
		if v.Description != "" {
			varSb.WriteString(fmt.Sprintf("  description = \"%s\"\n", v.Description))
		}
		if v.Default != "" {
			varSb.WriteString(fmt.Sprintf("  default     = \"%s\"\n", v.Default))
		}
		varSb.WriteString("}\n\n")
	}
	os.WriteFile(filepath.Join(dir, "variables.tf"), []byte(varSb.String()), 0644)

	// outputs.tf
	var outSb strings.Builder
	for _, o := range config.Outputs {
		outSb.WriteString(fmt.Sprintf("output \"%s\" {\n", o.Name))
		outSb.WriteString(fmt.Sprintf("  value = %s\n", o.Value))
		outSb.WriteString("}\n\n")
	}
	os.WriteFile(filepath.Join(dir, "outputs.tf"), []byte(outSb.String()), 0644)
}

// =============================================================================
// 6. 모듈 트리 빌더
// =============================================================================

// BuildModuleTree는 재귀적으로 모듈 트리를 구축한다.
// Terraform의 configs.BuildConfig()에 대응한다.
func BuildModuleTree(config *ModuleConfig, parent *ModuleTreeNode, installer *ModuleInstaller, inputVars map[string]string, depth int) (*ModuleTreeNode, error) {
	indent := strings.Repeat("  ", depth+2)

	node := &ModuleTreeNode{
		Config:       config,
		Children:     make(map[string]*ModuleTreeNode),
		Parent:       parent,
		ResolvedVars: make(map[string]string),
		ResolvedOuts: make(map[string]string),
	}

	// 변수 해결 (parent에서 전달받은 값 + 기본값)
	fmt.Printf("%s변수 해결:\n", indent)
	for _, v := range config.Variables {
		if val, exists := inputVars[v.Name]; exists {
			node.ResolvedVars[v.Name] = val
			fmt.Printf("%s  var.%s = %q (부모에서 전달)\n", indent, v.Name, val)
		} else if v.Default != "" {
			node.ResolvedVars[v.Name] = v.Default
			fmt.Printf("%s  var.%s = %q (기본값)\n", indent, v.Name, v.Default)
		} else {
			fmt.Printf("%s  var.%s = (값 없음 - 필수 입력!)\n", indent, v.Name)
		}
	}

	// 리소스 속성에서 변수 참조 치환
	fmt.Printf("%s리소스:\n", indent)
	for _, r := range config.Resources {
		fmt.Printf("%s  %s.%s\n", indent, r.Type, r.Name)
		for k, v := range r.Attributes {
			resolved := v
			for varName, varVal := range node.ResolvedVars {
				resolved = strings.ReplaceAll(resolved, "var."+varName, varVal)
			}
			if resolved != v {
				fmt.Printf("%s    %s: %s → %s\n", indent, k, v, resolved)
			}
		}
	}

	// 출력값 해결
	for _, o := range config.Outputs {
		node.ResolvedOuts[o.Name] = o.Value
	}

	// 자식 모듈 재귀 설치 및 트리 구축
	for _, call := range config.ModuleCalls {
		fmt.Println()
		childKey := config.Path + "." + call.Name
		if config.Path == "" {
			childKey = call.Name
		}

		fmt.Printf("%s자식 모듈: module.%s (source: %s)\n", indent, call.Name, call.Source.Raw)

		// 모듈 설치
		childConfig, err := installer.InstallModule(childKey, call.Source.Raw, call.Source.Version)
		if err != nil {
			return nil, fmt.Errorf("모듈 %q 설치 실패: %w", call.Name, err)
		}

		childConfig.Path = childKey

		// 입력 변수 전달 (부모의 변수 참조 해결)
		resolvedInputs := make(map[string]string)
		for k, v := range call.Inputs {
			resolved := v
			for varName, varVal := range node.ResolvedVars {
				resolved = strings.ReplaceAll(resolved, "var."+varName, varVal)
			}
			resolvedInputs[k] = resolved
		}

		// 재귀적으로 자식 트리 구축
		childNode, err := BuildModuleTree(childConfig, node, installer, resolvedInputs, depth+1)
		if err != nil {
			return nil, err
		}
		node.Children[call.Name] = childNode

		// 자식의 출력값을 부모에서 사용 가능하게 설정
		for outName, outVal := range childNode.ResolvedOuts {
			key := fmt.Sprintf("module.%s.%s", call.Name, outName)
			node.ResolvedOuts[key] = outVal
		}
	}

	return node, nil
}

// =============================================================================
// 7. 모듈 트리 시각화
// =============================================================================

func printModuleTree(node *ModuleTreeNode, indent string, isLast bool) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}

	name := node.Config.Path
	if name == "" {
		name = "root"
	}

	resourceCount := len(node.Config.Resources)
	varCount := len(node.Config.Variables)
	outCount := len(node.Config.Outputs)

	fmt.Printf("%s%s%s (리소스: %d, 변수: %d, 출력: %d)\n",
		indent, connector, name, resourceCount, varCount, outCount)

	childIndent := indent + "│   "
	if isLast {
		childIndent = indent + "    "
	}

	// 리소스 출력
	for _, r := range node.Config.Resources {
		fmt.Printf("%s  ● %s.%s\n", childIndent, r.Type, r.Name)
	}

	// 자식 모듈
	childNames := make([]string, 0, len(node.Children))
	for name := range node.Children {
		childNames = append(childNames, name)
	}

	for i, name := range childNames {
		isChildLast := i == len(childNames)-1
		printModuleTree(node.Children[name], childIndent, isChildLast)
	}
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║   Terraform 모듈 시스템 시뮬레이션                        ║")
	fmt.Println("║   실제 코드: internal/configs/, internal/initwd/         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 임시 디렉토리
	tmpDir, err := os.MkdirTemp("", "terraform-module-poc-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	installer := NewModuleInstaller(tmpDir)

	// =========================================================================
	// 모듈 정의 (사용 가능한 모듈 등록)
	// =========================================================================

	// 서브넷 모듈 (가장 하위)
	subnetModule := &ModuleConfig{
		Source: "./modules/subnet",
		Variables: []Variable{
			{Name: "vpc_id", Type: "string", Description: "VPC ID"},
			{Name: "cidr_block", Type: "string", Description: "서브넷 CIDR"},
			{Name: "availability_zone", Type: "string", Default: "ap-northeast-2a", Description: "가용영역"},
			{Name: "name", Type: "string", Default: "subnet", Description: "서브넷 이름"},
		},
		Outputs: []Output{
			{Name: "subnet_id", Value: "aws_subnet.this.id", Description: "서브넷 ID"},
			{Name: "subnet_cidr", Value: "aws_subnet.this.cidr_block", Description: "서브넷 CIDR"},
		},
		Resources: []ResourceDef{
			{
				Type: "aws_subnet", Name: "this",
				Attributes: map[string]string{
					"vpc_id":            "var.vpc_id",
					"cidr_block":        "var.cidr_block",
					"availability_zone": "var.availability_zone",
					"tags":              "Name=var.name",
				},
			},
		},
	}

	// 네트워크 모듈 (중간 레벨, 서브넷 모듈 호출)
	networkModule := &ModuleConfig{
		Source: "./modules/network",
		Variables: []Variable{
			{Name: "vpc_cidr", Type: "string", Default: "10.0.0.0/16", Description: "VPC CIDR"},
			{Name: "environment", Type: "string", Description: "환경 (dev/staging/prod)"},
			{Name: "public_subnet_cidr", Type: "string", Default: "10.0.1.0/24", Description: "퍼블릭 서브넷 CIDR"},
			{Name: "private_subnet_cidr", Type: "string", Default: "10.0.2.0/24", Description: "프라이빗 서브넷 CIDR"},
		},
		Outputs: []Output{
			{Name: "vpc_id", Value: "aws_vpc.main.id", Description: "VPC ID"},
			{Name: "public_subnet_id", Value: "module.public_subnet.subnet_id", Description: "퍼블릭 서브넷 ID"},
			{Name: "private_subnet_id", Value: "module.private_subnet.subnet_id", Description: "프라이빗 서브넷 ID"},
		},
		Resources: []ResourceDef{
			{
				Type: "aws_vpc", Name: "main",
				Attributes: map[string]string{
					"cidr_block": "var.vpc_cidr",
					"tags":       "Name=var.environment-vpc",
				},
			},
			{
				Type: "aws_internet_gateway", Name: "main",
				Attributes: map[string]string{
					"vpc_id": "aws_vpc.main.id",
				},
			},
		},
		ModuleCalls: []ModuleCall{
			{
				Name:   "public_subnet",
				Source: ParseModuleSource("./modules/subnet"),
				Inputs: map[string]string{
					"vpc_id":     "aws_vpc.main.id",
					"cidr_block": "var.public_subnet_cidr",
					"name":       "var.environment-public",
				},
			},
			{
				Name:   "private_subnet",
				Source: ParseModuleSource("./modules/subnet"),
				Inputs: map[string]string{
					"vpc_id":     "aws_vpc.main.id",
					"cidr_block": "var.private_subnet_cidr",
					"name":       "var.environment-private",
				},
			},
		},
	}

	// 모듈 등록
	installer.RegisterModule("./modules/subnet", subnetModule)
	installer.RegisterModule("./modules/network", networkModule)

	// =========================================================================
	// 루트 모듈 정의
	// =========================================================================
	rootModule := &ModuleConfig{
		Path:   "",
		Source: ".",
		Variables: []Variable{
			{Name: "environment", Type: "string", Default: "production", Description: "배포 환경"},
			{Name: "region", Type: "string", Default: "ap-northeast-2", Description: "AWS 리전"},
		},
		Outputs: []Output{
			{Name: "vpc_id", Value: "module.network.vpc_id", Description: "VPC ID"},
			{Name: "public_subnet_id", Value: "module.network.public_subnet_id", Description: "퍼블릭 서브넷 ID"},
		},
		Resources: []ResourceDef{
			{
				Type: "aws_instance", Name: "web",
				Attributes: map[string]string{
					"ami":       "ami-12345",
					"subnet_id": "module.network.public_subnet_id",
					"tags":      "Name=var.environment-web",
				},
			},
		},
		ModuleCalls: []ModuleCall{
			{
				Name:   "network",
				Source: ParseModuleSource("./modules/network"),
				Inputs: map[string]string{
					"vpc_cidr":    "10.0.0.0/16",
					"environment": "var.environment",
				},
			},
		},
	}

	// =========================================================================
	// 데모 1: 모듈 소스 타입 분석
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 1: 모듈 소스 타입")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	sources := []string{
		"./modules/network",
		"../shared/vpc",
		"hashicorp/vpc/aws",
		"git::https://github.com/example/terraform-module.git",
	}

	for _, src := range sources {
		ms := ParseModuleSource(src)
		fmt.Printf("    %-55s → %s\n", src, sourceTypeNames[ms.Type])
	}
	fmt.Println()

	// =========================================================================
	// 데모 2: 모듈 트리 구축
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 2: 모듈 트리 구축 (재귀적 설치)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  루트 모듈 처리:")
	rootNode, err := BuildModuleTree(rootModule, nil, installer, map[string]string{
		"environment": "production",
		"region":      "ap-northeast-2",
	}, 0)
	if err != nil {
		fmt.Printf("  모듈 트리 구축 실패: %v\n", err)
		return
	}
	fmt.Println()

	// =========================================================================
	// 데모 3: 모듈 트리 시각화
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 3: 모듈 트리 시각화")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	printModuleTree(rootNode, "  ", true)
	fmt.Println()

	// =========================================================================
	// 데모 4: 모듈 매니페스트 (modules.json)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 4: 모듈 매니페스트 (.terraform/modules/modules.json)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	installer.WriteManifest()

	manifestData, _ := json.MarshalIndent(installer.Manifest, "    ", "  ")
	fmt.Printf("    %s\n", string(manifestData))
	fmt.Println()

	// =========================================================================
	// 데모 5: 변수 전달 흐름
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 5: 변수(Variable) 전달 흐름")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  변수 전달 경로:")
	fmt.Println()
	fmt.Println("    root (var.environment = \"production\")")
	fmt.Println("      │")
	fmt.Println("      │  module \"network\" {")
	fmt.Println("      │    environment = var.environment  ← 부모 변수 참조")
	fmt.Println("      │  }")
	fmt.Println("      │")
	fmt.Println("      ▼")
	fmt.Println("    module.network (var.environment = \"production\")")
	fmt.Println("      │")
	fmt.Println("      │  module \"public_subnet\" {")
	fmt.Println("      │    name = \"${var.environment}-public\"  ← 변수 치환")
	fmt.Println("      │  }")
	fmt.Println("      │")
	fmt.Println("      ▼")
	fmt.Println("    module.network.module.public_subnet")
	fmt.Println("      var.name = \"production-public\"")
	fmt.Println()

	// 실제 해결된 변수 출력
	fmt.Println("  해결된 변수:")
	printResolvedVars(rootNode, "    ")
	fmt.Println()

	// =========================================================================
	// 데모 6: 출력값 전달 흐름
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 6: 출력값(Output) 전달 흐름")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  출력값 전달 경로 (아래에서 위로):")
	fmt.Println()
	fmt.Println("    module.network.module.public_subnet")
	fmt.Println("      output \"subnet_id\" = aws_subnet.this.id")
	fmt.Println("      │")
	fmt.Println("      ▲")
	fmt.Println("      │")
	fmt.Println("    module.network")
	fmt.Println("      output \"public_subnet_id\" = module.public_subnet.subnet_id")
	fmt.Println("      │")
	fmt.Println("      ▲")
	fmt.Println("      │")
	fmt.Println("    root")
	fmt.Println("      output \"public_subnet_id\" = module.network.public_subnet_id")
	fmt.Println()

	// =========================================================================
	// 데모 7: 설치 디렉토리 구조
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 7: .terraform/modules/ 디렉토리 구조")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  .terraform/modules/")
	printDirTree(filepath.Join(tmpDir, ".terraform", "modules"), "    ", 0)
	fmt.Println()

	// =========================================================================
	// 핵심 포인트
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  핵심 포인트 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. 모듈은 재사용 가능한 인프라 구성 단위이다")
	fmt.Println("  2. 소스 타입: local(./), registry(ns/name/provider), git(git::)")
	fmt.Println("  3. terraform init 시 모듈을 .terraform/modules/에 설치한다")
	fmt.Println("  4. modules.json 매니페스트로 설치된 모듈 위치를 추적한다")
	fmt.Println("  5. 변수(Variable)로 부모 → 자식 데이터 전달")
	fmt.Println("  6. 출력값(Output)으로 자식 → 부모 데이터 전달")
	fmt.Println("  7. 모듈은 재귀적으로 중첩될 수 있다 (root → network → subnet)")
}

func printResolvedVars(node *ModuleTreeNode, indent string) {
	name := node.Config.Path
	if name == "" {
		name = "root"
	}
	fmt.Printf("%s[%s]\n", indent, name)
	for k, v := range node.ResolvedVars {
		fmt.Printf("%s  var.%s = %q\n", indent, k, v)
	}
	for _, child := range node.Children {
		printResolvedVars(child, indent+"  ")
	}
}

func printDirTree(path string, prefix string, depth int) {
	if depth > 5 {
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}

	for i, entry := range entries {
		isLast := i == len(entries)-1
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		fmt.Printf("%s%s%s\n", prefix, connector, entry.Name())

		if entry.IsDir() {
			nextPrefix := prefix + "│   "
			if isLast {
				nextPrefix = prefix + "    "
			}
			printDirTree(filepath.Join(path, entry.Name()), nextPrefix, depth+1)
		}
	}
}
