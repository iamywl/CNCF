package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// =============================================================================
// Terraform Provider 설치 시뮬레이션
// =============================================================================
//
// Terraform은 프로바이더를 레지스트리에서 다운로드하여 설치한다.
// 버전 제약 조건 매칭, 캐시 관리, 잠금 파일(.terraform.lock.hcl)을 통해
// 재현 가능한 빌드를 보장한다.
//
// 실제 Terraform 소스:
//   - internal/getproviders/registry_client.go: 레지스트리 API 클라이언트
//   - internal/getproviders/package_authentication.go: 패키지 인증
//   - internal/providercache/dir.go: 프로바이더 캐시 디렉토리
//   - internal/depsfile/locks.go: 잠금 파일 관리
//   - internal/command/init.go: terraform init 명령어
//
// 이 PoC에서 구현하는 핵심 개념:
//   1. 프로바이더 레지스트리 (Mock HTTP 서버)
//   2. SemVer 버전 제약 조건 매칭
//   3. 프로바이더 다운로드 시뮬레이션
//   4. 캐시 디렉토리 관리
//   5. 잠금 파일 (.terraform.lock.hcl 시뮬레이션)

// =============================================================================
// 1. SemVer(Semantic Versioning) 구현
// =============================================================================

// Version은 시맨틱 버전을 나타낸다.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
}

// ParseVersion은 버전 문자열을 파싱한다.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimPrefix(s, "v")
	var v Version

	// 프리릴리즈 분리
	if idx := strings.Index(s, "-"); idx >= 0 {
		v.Prerelease = s[idx+1:]
		s = s[:idx]
	}

	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return v, fmt.Errorf("잘못된 버전 형식: %s", s)
	}

	var err error
	v.Major, err = strconv.Atoi(parts[0])
	if err != nil {
		return v, err
	}

	if len(parts) >= 2 {
		v.Minor, err = strconv.Atoi(parts[1])
		if err != nil {
			return v, err
		}
	}

	if len(parts) >= 3 {
		v.Patch, err = strconv.Atoi(parts[2])
		if err != nil {
			return v, err
		}
	}

	return v, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

// Compare는 두 버전을 비교한다. -1, 0, 1을 반환한다.
func (v Version) Compare(other Version) int {
	if v.Major != other.Major {
		if v.Major < other.Major {
			return -1
		}
		return 1
	}
	if v.Minor != other.Minor {
		if v.Minor < other.Minor {
			return -1
		}
		return 1
	}
	if v.Patch != other.Patch {
		if v.Patch < other.Patch {
			return -1
		}
		return 1
	}
	// 프리릴리즈가 있으면 릴리즈보다 낮은 우선순위
	if v.Prerelease != "" && other.Prerelease == "" {
		return -1
	}
	if v.Prerelease == "" && other.Prerelease != "" {
		return 1
	}
	return 0
}

// =============================================================================
// 2. 버전 제약 조건 (Version Constraint)
// =============================================================================

// Constraint는 하나의 버전 제약 조건이다.
type Constraint struct {
	Op      string  // "=", "!=", ">", ">=", "<", "<=", "~>"
	Version Version
}

// ConstraintSet은 여러 제약 조건의 집합이다 (AND로 결합).
type ConstraintSet []Constraint

// ParseConstraint는 제약 조건 문자열을 파싱한다.
func ParseConstraint(s string) (ConstraintSet, error) {
	parts := strings.Split(s, ",")
	var constraints ConstraintSet

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		c := Constraint{}

		// 연산자 추출
		for _, op := range []string{">=", "<=", "!=", "~>", ">", "<", "="} {
			if strings.HasPrefix(part, op) {
				c.Op = op
				part = strings.TrimSpace(part[len(op):])
				break
			}
		}

		if c.Op == "" {
			c.Op = "="
		}

		v, err := ParseVersion(part)
		if err != nil {
			return nil, err
		}
		c.Version = v
		constraints = append(constraints, c)
	}

	return constraints, nil
}

// Check는 주어진 버전이 제약 조건을 만족하는지 확인한다.
func (cs ConstraintSet) Check(v Version) bool {
	for _, c := range cs {
		if !c.check(v) {
			return false
		}
	}
	return true
}

func (c Constraint) check(v Version) bool {
	cmp := v.Compare(c.Version)

	switch c.Op {
	case "=":
		return cmp == 0
	case "!=":
		return cmp != 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case "~>":
		// ~> 연산자: 패시미스틱 제약
		// ~> 1.2 은 >= 1.2, < 2.0
		// ~> 1.2.3 은 >= 1.2.3, < 1.3.0
		if v.Compare(c.Version) < 0 {
			return false
		}
		if c.Version.Patch > 0 || (c.Version.Minor > 0 && c.Version.Patch == 0) {
			// ~> X.Y.Z → < X.(Y+1).0
			upperBound := Version{Major: c.Version.Major, Minor: c.Version.Minor + 1, Patch: 0}
			return v.Compare(upperBound) < 0
		}
		// ~> X.Y → < (X+1).0.0
		upperBound := Version{Major: c.Version.Major + 1, Minor: 0, Patch: 0}
		return v.Compare(upperBound) < 0
	default:
		return false
	}
}

func (cs ConstraintSet) String() string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = fmt.Sprintf("%s %s", c.Op, c.Version)
	}
	return strings.Join(parts, ", ")
}

// =============================================================================
// 3. 프로바이더 레지스트리 (Mock)
// =============================================================================

// ProviderVersion은 레지스트리에서 사용 가능한 프로바이더 버전이다.
type ProviderVersion struct {
	Version   Version
	Platforms []string // "linux_amd64", "darwin_arm64", ...
	Checksum  string
}

// RegistryProvider는 레지스트리의 프로바이더 정보이다.
type RegistryProvider struct {
	Namespace string
	Type      string
	Versions  []ProviderVersion
}

// ProviderRegistry는 프로바이더 레지스트리이다.
type ProviderRegistry struct {
	providers map[string]*RegistryProvider // key: "namespace/type"
}

// NewProviderRegistry는 mock 레지스트리를 생성한다.
func NewProviderRegistry() *ProviderRegistry {
	r := &ProviderRegistry{
		providers: make(map[string]*RegistryProvider),
	}

	// Mock 데이터 등록
	r.Register("hashicorp", "aws", []string{
		"4.67.0", "4.68.0", "5.0.0", "5.1.0", "5.2.0", "5.31.0", "5.32.0",
	})
	r.Register("hashicorp", "google", []string{
		"4.80.0", "4.81.0", "5.0.0", "5.1.0", "5.12.0",
	})
	r.Register("hashicorp", "azurerm", []string{
		"3.70.0", "3.71.0", "3.72.0", "3.73.0",
	})
	r.Register("hashicorp", "local", []string{
		"2.4.0", "2.4.1", "2.5.0", "2.5.1",
	})
	r.Register("hashicorp", "random", []string{
		"3.5.0", "3.5.1", "3.6.0",
	})

	return r
}

func (r *ProviderRegistry) Register(namespace, typeName string, versions []string) {
	key := namespace + "/" + typeName
	rp := &RegistryProvider{
		Namespace: namespace,
		Type:      typeName,
	}

	for _, vs := range versions {
		v, _ := ParseVersion(vs)
		rp.Versions = append(rp.Versions, ProviderVersion{
			Version:   v,
			Platforms: []string{"linux_amd64", "darwin_arm64", "darwin_amd64", "windows_amd64"},
			Checksum:  fmt.Sprintf("h1:sha256-%s-%s", typeName, vs),
		})
	}

	r.providers[key] = rp
}

// ListVersions는 사용 가능한 버전 목록을 반환한다.
func (r *ProviderRegistry) ListVersions(namespace, typeName string) ([]ProviderVersion, error) {
	key := namespace + "/" + typeName
	rp, exists := r.providers[key]
	if !exists {
		return nil, fmt.Errorf("프로바이더 %s/%s 를 찾을 수 없습니다", namespace, typeName)
	}
	return rp.Versions, nil
}

// =============================================================================
// 4. 프로바이더 캐시
// =============================================================================

// ProviderCache는 다운로드된 프로바이더를 캐시하는 디렉토리이다.
// Terraform의 providercache.Dir에 대응한다.
type ProviderCache struct {
	BaseDir string
	cached  map[string]bool // "namespace/type/version" → true
}

// NewProviderCache는 새로운 프로바이더 캐시를 생성한다.
func NewProviderCache(baseDir string) *ProviderCache {
	os.MkdirAll(baseDir, 0755)
	return &ProviderCache{
		BaseDir: baseDir,
		cached:  make(map[string]bool),
	}
}

// IsCached는 프로바이더가 캐시에 있는지 확인한다.
func (c *ProviderCache) IsCached(namespace, typeName, version string) bool {
	key := fmt.Sprintf("%s/%s/%s", namespace, typeName, version)
	return c.cached[key]
}

// Install은 프로바이더를 캐시에 설치한다 (다운로드 시뮬레이션).
func (c *ProviderCache) Install(namespace, typeName, version, platform string) error {
	// 디렉토리 구조: cache/registry.terraform.io/namespace/type/version/platform/
	dir := filepath.Join(c.BaseDir,
		"registry.terraform.io",
		namespace,
		typeName,
		version,
		platform,
	)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// 바이너리 파일 생성 (시뮬레이션)
	binaryName := fmt.Sprintf("terraform-provider-%s_v%s", typeName, version)
	binaryPath := filepath.Join(dir, binaryName)
	content := fmt.Sprintf("#!/bin/sh\n# Mock provider binary: %s/%s v%s\necho 'provider %s v%s'\n",
		namespace, typeName, version, typeName, version)
	if err := os.WriteFile(binaryPath, []byte(content), 0755); err != nil {
		return err
	}

	key := fmt.Sprintf("%s/%s/%s", namespace, typeName, version)
	c.cached[key] = true
	return nil
}

// =============================================================================
// 5. 잠금 파일 (.terraform.lock.hcl 시뮬레이션)
// =============================================================================

// LockFileEntry는 잠금 파일의 하나의 프로바이더 항목이다.
type LockFileEntry struct {
	Namespace  string   `json:"namespace"`
	Type       string   `json:"type"`
	Version    string   `json:"version"`
	Checksums  []string `json:"checksums"`
	Constraints string  `json:"constraints"`
}

// LockFile은 프로바이더 잠금 파일이다.
type LockFile struct {
	Providers []LockFileEntry `json:"providers"`
}

// WriteLockFile은 잠금 파일을 기록한다.
func WriteLockFile(path string, lockFile *LockFile) error {
	data, err := json.MarshalIndent(lockFile, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ReadLockFile은 잠금 파일을 읽는다.
func ReadLockFile(path string) (*LockFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lf LockFile
	return &lf, json.Unmarshal(data, &lf)
}

// FormatLockFileHCL은 잠금 파일을 HCL-like 포맷으로 출력한다.
func FormatLockFileHCL(lf *LockFile) string {
	var sb strings.Builder
	sb.WriteString("# This file is maintained automatically by \"terraform init\".\n")
	sb.WriteString("# Manual edits may be lost in future updates.\n\n")

	for _, entry := range lf.Providers {
		sb.WriteString(fmt.Sprintf("provider \"registry.terraform.io/%s/%s\" {\n", entry.Namespace, entry.Type))
		sb.WriteString(fmt.Sprintf("  version     = \"%s\"\n", entry.Version))
		sb.WriteString(fmt.Sprintf("  constraints = \"%s\"\n", entry.Constraints))
		sb.WriteString("  hashes = [\n")
		for _, h := range entry.Checksums {
			sb.WriteString(fmt.Sprintf("    \"%s\",\n", h))
		}
		sb.WriteString("  ]\n")
		sb.WriteString("}\n\n")
	}

	return sb.String()
}

// =============================================================================
// 6. 프로바이더 설치 관리자
// =============================================================================

// ProviderRequirement는 required_providers의 하나의 항목이다.
type ProviderRequirement struct {
	Source     string // "hashicorp/aws"
	Constraint string // ">= 5.0, < 6.0"
}

// ProviderInstaller는 프로바이더 설치를 관리한다.
// Terraform의 internal/command/init.go에서의 로직에 대응한다.
type ProviderInstaller struct {
	Registry *ProviderRegistry
	Cache    *ProviderCache
	Platform string
}

// ResolveVersion은 제약 조건을 만족하는 최신 버전을 찾는다.
func (i *ProviderInstaller) ResolveVersion(req ProviderRequirement) (*ProviderVersion, error) {
	parts := strings.SplitN(req.Source, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("잘못된 프로바이더 소스: %s", req.Source)
	}
	namespace, typeName := parts[0], parts[1]

	versions, err := i.Registry.ListVersions(namespace, typeName)
	if err != nil {
		return nil, err
	}

	constraints, err := ParseConstraint(req.Constraint)
	if err != nil {
		return nil, fmt.Errorf("제약 조건 파싱 실패: %w", err)
	}

	// 제약 조건을 만족하는 버전 필터링
	var matching []ProviderVersion
	for _, pv := range versions {
		if constraints.Check(pv.Version) {
			matching = append(matching, pv)
		}
	}

	if len(matching) == 0 {
		return nil, fmt.Errorf("프로바이더 %s의 %s 제약 조건을 만족하는 버전이 없습니다",
			req.Source, req.Constraint)
	}

	// 최신 버전 선택
	sort.Slice(matching, func(i, j int) bool {
		return matching[i].Version.Compare(matching[j].Version) > 0
	})

	return &matching[0], nil
}

// Install은 프로바이더를 설치한다.
func (i *ProviderInstaller) Install(req ProviderRequirement) (*LockFileEntry, error) {
	parts := strings.SplitN(req.Source, "/", 2)
	namespace, typeName := parts[0], parts[1]

	// 버전 해결
	resolved, err := i.ResolveVersion(req)
	if err != nil {
		return nil, err
	}

	versionStr := resolved.Version.String()

	// 캐시 확인
	if i.Cache.IsCached(namespace, typeName, versionStr) {
		fmt.Printf("    - %s v%s: 캐시에서 사용\n", req.Source, versionStr)
	} else {
		fmt.Printf("    - %s v%s: 레지스트리에서 다운로드 중...\n", req.Source, versionStr)
		err := i.Cache.Install(namespace, typeName, versionStr, i.Platform)
		if err != nil {
			return nil, fmt.Errorf("설치 실패: %w", err)
		}
		fmt.Printf("      ✓ 설치 완료\n")
	}

	return &LockFileEntry{
		Namespace:   namespace,
		Type:        typeName,
		Version:     versionStr,
		Checksums:   []string{resolved.Checksum},
		Constraints: req.Constraint,
	}, nil
}

// =============================================================================
// 7. Mock HTTP 서버 (레지스트리 API)
// =============================================================================

func startMockRegistryServer(registry *ProviderRegistry) *http.Server {
	mux := http.NewServeMux()

	// 프로바이더 버전 조회 API
	mux.HandleFunc("/v1/providers/", func(w http.ResponseWriter, r *http.Request) {
		// /v1/providers/{namespace}/{type}/versions
		path := strings.TrimPrefix(r.URL.Path, "/v1/providers/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			http.Error(w, "잘못된 경로", 400)
			return
		}

		namespace, typeName := parts[0], parts[1]
		versions, err := registry.ListVersions(namespace, typeName)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}

		var versionList []map[string]interface{}
		for _, v := range versions {
			versionList = append(versionList, map[string]interface{}{
				"version":   v.Version.String(),
				"platforms": v.Platforms,
			})
		}

		resp := map[string]interface{}{
			"id":       fmt.Sprintf("%s/%s", namespace, typeName),
			"versions": versionList,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	server := &http.Server{
		Addr:    "127.0.0.1:0", // 랜덤 포트
		Handler: mux,
	}

	return server
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║   Terraform Provider 설치 시뮬레이션                      ║")
	fmt.Println("║   실제 코드: internal/getproviders/, internal/depsfile/  ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "terraform-installer-poc-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// 레지스트리, 캐시, 설치 관리자 초기화
	registry := NewProviderRegistry()
	cache := NewProviderCache(filepath.Join(tmpDir, ".terraform", "providers"))
	installer := &ProviderInstaller{
		Registry: registry,
		Cache:    cache,
		Platform: "darwin_arm64",
	}

	// =========================================================================
	// 데모 1: 버전 제약 조건 매칭
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 1: SemVer 버전 제약 조건 매칭")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	testCases := []struct {
		constraint string
		versions   []string
	}{
		{
			constraint: ">= 5.0.0, < 6.0.0",
			versions:   []string{"4.68.0", "5.0.0", "5.1.0", "5.31.0", "5.32.0", "6.0.0"},
		},
		{
			constraint: "~> 5.1",
			versions:   []string{"5.0.0", "5.1.0", "5.31.0", "5.32.0", "6.0.0"},
		},
		{
			constraint: "~> 3.70.0",
			versions:   []string{"3.69.0", "3.70.0", "3.70.5", "3.71.0"},
		},
		{
			constraint: "= 2.5.0",
			versions:   []string{"2.4.0", "2.4.1", "2.5.0", "2.5.1"},
		},
	}

	for _, tc := range testCases {
		cs, _ := ParseConstraint(tc.constraint)
		fmt.Printf("  제약 조건: %s\n", tc.constraint)
		for _, vs := range tc.versions {
			v, _ := ParseVersion(vs)
			result := "  "
			if cs.Check(v) {
				result = "OK"
			}
			fmt.Printf("    %s %-10s %s\n", result, vs, func() string {
				if cs.Check(v) {
					return "-> 매칭"
				}
				return ""
			}())
		}
		fmt.Println()
	}

	// ~> 연산자 설명
	fmt.Println("  ~> (패시미스틱 제약) 연산자 설명:")
	fmt.Println("    ~> 5.1   : >= 5.1.0 AND < 6.0.0  (마이너 버전까지 허용)")
	fmt.Println("    ~> 3.70.0: >= 3.70.0 AND < 3.71.0 (패치 버전만 허용)")
	fmt.Println()

	// =========================================================================
	// 데모 2: terraform init - 프로바이더 설치
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 2: terraform init - 프로바이더 설치")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// required_providers 시뮬레이션
	requirements := []ProviderRequirement{
		{Source: "hashicorp/aws", Constraint: ">= 5.0.0, < 6.0.0"},
		{Source: "hashicorp/local", Constraint: "~> 2.5"},
		{Source: "hashicorp/random", Constraint: ">= 3.5.0"},
		{Source: "hashicorp/azurerm", Constraint: "~> 3.70.0"},
	}

	fmt.Println("  required_providers:")
	for _, req := range requirements {
		fmt.Printf("    %-20s %s\n", req.Source, req.Constraint)
	}
	fmt.Println()

	fmt.Println("  Initializing provider plugins...")
	fmt.Println()

	lockFile := &LockFile{}

	for _, req := range requirements {
		entry, err := installer.Install(req)
		if err != nil {
			fmt.Printf("    ✗ %s: %v\n", req.Source, err)
			continue
		}
		lockFile.Providers = append(lockFile.Providers, *entry)
	}
	fmt.Println()

	// =========================================================================
	// 데모 3: 캐시 동작
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 3: 두 번째 terraform init (캐시 활용)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  Initializing provider plugins... (2nd run)")
	fmt.Println()
	for _, req := range requirements {
		_, err := installer.Install(req)
		if err != nil {
			fmt.Printf("    ✗ %s: %v\n", req.Source, err)
		}
	}
	fmt.Println()

	// =========================================================================
	// 데모 4: 잠금 파일 생성
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 4: .terraform.lock.hcl 생성")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	lockFilePath := filepath.Join(tmpDir, ".terraform.lock.hcl")
	WriteLockFile(lockFilePath, lockFile)

	hclOutput := FormatLockFileHCL(lockFile)
	for _, line := range strings.Split(hclOutput, "\n") {
		fmt.Printf("    %s\n", line)
	}

	// =========================================================================
	// 데모 5: 캐시 디렉토리 구조
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 5: 캐시 디렉토리 구조")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  .terraform/providers/")
	printDirTree(filepath.Join(tmpDir, ".terraform", "providers"), "    ", 0)
	fmt.Println()

	// =========================================================================
	// 데모 6: 버전 해결 충돌
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 6: 버전 해결 실패 시나리오")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	failReq := ProviderRequirement{
		Source:     "hashicorp/aws",
		Constraint: ">= 6.0.0",
	}
	_, err = installer.Install(failReq)
	if err != nil {
		fmt.Printf("    ✗ %s (%s): %v\n", failReq.Source, failReq.Constraint, err)
	}
	fmt.Println()

	// =========================================================================
	// 핵심 포인트
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  핵심 포인트 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. terraform init 시 required_providers의 제약 조건으로 버전 해결")
	fmt.Println("  2. ~> 연산자로 호환 가능한 버전 범위를 간편하게 지정")
	fmt.Println("  3. 프로바이더 캐시로 반복 init 시 다운로드 생략")
	fmt.Println("  4. .terraform.lock.hcl로 정확한 버전과 체크섬을 고정")
	fmt.Println("  5. 잠금 파일은 VCS에 커밋하여 팀 전체가 동일 버전 사용")
	fmt.Println("  6. 레지스트리 API로 사용 가능한 버전과 플랫폼 정보 조회")
}

// printDirTree는 디렉토리 트리를 출력한다.
func printDirTree(path string, prefix string, depth int) {
	if depth > 6 {
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
