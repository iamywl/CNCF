package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// =============================================================================
// Helm 의존성 해석 PoC
// =============================================================================
//
// 참조: internal/resolver/resolver.go, pkg/chart/v2/dependency.go,
//       pkg/chart/v2/metadata.go
//
// Helm의 의존성 시스템은 다음을 제공한다:
//   1. SemVer 제약 조건 해석 — ^1.2.0, ~1.2, >=1.0 <2.0 등
//   2. 의존성 트리 구축 — Chart.yaml의 dependencies 섹션 파싱
//   3. Lock 파일 생성 — Chart.lock으로 정확한 버전 고정
//   4. condition/tags 평가 — 조건부 서브차트 활성화/비활성화
// =============================================================================

// --- SemVer: 시맨틱 버전 ---
type SemVer struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
}

// ParseSemVer는 버전 문자열을 파싱한다.
func ParseSemVer(s string) (SemVer, error) {
	s = strings.TrimPrefix(s, "v")

	// prerelease 분리
	var prerelease string
	if idx := strings.Index(s, "-"); idx >= 0 {
		prerelease = s[idx+1:]
		s = s[:idx]
	}

	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return SemVer{}, fmt.Errorf("유효하지 않은 버전: %s", s)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return SemVer{}, fmt.Errorf("major 버전 파싱 오류: %w", err)
	}

	minor := 0
	if len(parts) >= 2 {
		minor, err = strconv.Atoi(parts[1])
		if err != nil {
			return SemVer{}, fmt.Errorf("minor 버전 파싱 오류: %w", err)
		}
	}

	patch := 0
	if len(parts) >= 3 {
		patch, err = strconv.Atoi(parts[2])
		if err != nil {
			return SemVer{}, fmt.Errorf("patch 버전 파싱 오류: %w", err)
		}
	}

	return SemVer{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		Prerelease: prerelease,
	}, nil
}

func (v SemVer) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

// LessThan은 버전 비교를 수행한다.
func (v SemVer) LessThan(other SemVer) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor < other.Minor
	}
	if v.Patch != other.Patch {
		return v.Patch < other.Patch
	}
	// prerelease가 있으면 없는 것보다 낮음
	if v.Prerelease != "" && other.Prerelease == "" {
		return true
	}
	if v.Prerelease == "" && other.Prerelease != "" {
		return false
	}
	return v.Prerelease < other.Prerelease
}

// --- Constraint: SemVer 제약 조건 ---
// Helm은 github.com/Masterminds/semver를 사용한다.
// 이 PoC는 핵심 패턴을 직접 구현한다.
type Constraint struct {
	Raw      string
	Checks   []check
}

type check struct {
	op  string // ">=", "<=", ">", "<", "=", "!="
	ver SemVer
}

// NewConstraint는 제약 조건 문자열을 파싱한다.
// 지원 형식: ^1.2.0, ~1.2, >=1.0 <2.0, 1.2.x, *, 1.2.0
func NewConstraint(s string) (*Constraint, error) {
	c := &Constraint{Raw: s}

	s = strings.TrimSpace(s)
	if s == "" || s == "*" {
		// 모든 버전 허용
		return c, nil
	}

	// ^ (caret) — 호환 가능한 업데이트
	// ^1.2.3 → >=1.2.3 <2.0.0 (major 고정)
	// ^0.2.3 → >=0.2.3 <0.3.0 (0.x일 때 minor 고정)
	if strings.HasPrefix(s, "^") {
		ver, err := ParseSemVer(s[1:])
		if err != nil {
			return nil, err
		}
		c.Checks = append(c.Checks, check{">=", ver})
		if ver.Major > 0 {
			c.Checks = append(c.Checks, check{"<", SemVer{Major: ver.Major + 1}})
		} else {
			c.Checks = append(c.Checks, check{"<", SemVer{Minor: ver.Minor + 1}})
		}
		return c, nil
	}

	// ~ (tilde) — 패치 레벨 업데이트
	// ~1.2.3 → >=1.2.3 <1.3.0
	// ~1.2   → >=1.2.0 <1.3.0
	if strings.HasPrefix(s, "~") {
		ver, err := ParseSemVer(s[1:])
		if err != nil {
			return nil, err
		}
		c.Checks = append(c.Checks, check{">=", ver})
		c.Checks = append(c.Checks, check{"<", SemVer{Major: ver.Major, Minor: ver.Minor + 1}})
		return c, nil
	}

	// x 와일드카드: 1.2.x → >=1.2.0 <1.3.0
	if strings.HasSuffix(s, ".x") || strings.HasSuffix(s, ".X") || strings.HasSuffix(s, ".*") {
		base := s[:len(s)-2]
		ver, err := ParseSemVer(base + ".0")
		if err != nil {
			return nil, err
		}
		c.Checks = append(c.Checks, check{">=", ver})
		c.Checks = append(c.Checks, check{"<", SemVer{Major: ver.Major, Minor: ver.Minor + 1}})
		return c, nil
	}

	// 복합 제약: >=1.0.0 <2.0.0 또는 >=1.0.0, <2.0.0
	parts := splitConstraints(s)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		op, verStr := parseOp(part)
		ver, err := ParseSemVer(verStr)
		if err != nil {
			return nil, fmt.Errorf("제약 조건 파싱 오류 %q: %w", part, err)
		}
		c.Checks = append(c.Checks, check{op, ver})
	}

	return c, nil
}

func splitConstraints(s string) []string {
	// 쉼표 또는 공백으로 분리 (연산자 앞의 공백 주의)
	s = strings.ReplaceAll(s, ",", " ")
	parts := strings.Fields(s)

	// 연산자가 붙어있는 경우 처리
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func parseOp(s string) (string, string) {
	for _, op := range []string{">=", "<=", "!=", ">", "<", "="} {
		if strings.HasPrefix(s, op) {
			return op, strings.TrimSpace(s[len(op):])
		}
	}
	// 연산자 없으면 정확한 매치
	return "=", s
}

// Check는 버전이 제약 조건을 만족하는지 확인한다.
func (c *Constraint) Check(v SemVer) bool {
	if len(c.Checks) == 0 {
		return true // 제약 없음 = 모든 버전 허용
	}

	for _, ch := range c.Checks {
		if !evalCheck(ch, v) {
			return false
		}
	}
	return true
}

func evalCheck(ch check, v SemVer) bool {
	switch ch.op {
	case "=":
		return !v.LessThan(ch.ver) && !ch.ver.LessThan(v)
	case "!=":
		return v.LessThan(ch.ver) || ch.ver.LessThan(v)
	case ">":
		return ch.ver.LessThan(v)
	case ">=":
		return !v.LessThan(ch.ver)
	case "<":
		return v.LessThan(ch.ver)
	case "<=":
		return !ch.ver.LessThan(v)
	}
	return false
}

func (c *Constraint) String() string {
	return c.Raw
}

// --- Dependency: 차트 의존성 정의 ---
// Helm 소스: pkg/chart/v2/dependency.go의 Dependency 구조체
type Dependency struct {
	Name       string   `json:"name"`
	Version    string   `json:"version,omitempty"`
	Repository string   `json:"repository"`
	Condition  string   `json:"condition,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Enabled    bool     `json:"enabled,omitempty"`
	Alias      string   `json:"alias,omitempty"`
}

// --- Lock: 의존성 잠금 파일 ---
// Helm 소스: pkg/chart/v2/dependency.go의 Lock 구조체
type Lock struct {
	Generated    time.Time     `json:"generated"`
	Digest       string        `json:"digest"`
	Dependencies []*Dependency `json:"dependencies"`
}

// --- ChartVersion: 리포지토리의 차트 버전 ---
type ChartVersion struct {
	Name    string
	Version string
	URLs    []string
}

// --- Repository: 차트 리포지토리 (인덱스) ---
type Repository struct {
	Name    string
	URL     string
	Entries map[string][]ChartVersion // chart name → versions (내림차순)
}

// --- Resolver: 의존성 해석기 ---
// Helm 소스: internal/resolver/resolver.go의 Resolver
type Resolver struct {
	repos map[string]*Repository // repo URL → Repository
}

// NewResolver는 새로운 의존성 해석기를 생성한다.
func NewResolver(repos []*Repository) *Resolver {
	repoMap := make(map[string]*Repository)
	for _, r := range repos {
		repoMap[r.URL] = r
	}
	return &Resolver{repos: repoMap}
}

// Resolve는 의존성을 해석하고 Lock 파일을 생성한다.
// Helm 소스: internal/resolver/resolver.go의 Resolve 메서드
// 핵심 로직:
//   1. 각 의존성에 대해 SemVer 제약 조건 파싱
//   2. 리포지토리 인덱스에서 제약을 만족하는 첫 버전 선택 (내림차순이므로 최신)
//   3. 찾지 못한 의존성이 있으면 에러
//   4. Lock 파일 생성 (digest로 변경 감지)
func (r *Resolver) Resolve(deps []*Dependency) (*Lock, error) {
	locked := make([]*Dependency, len(deps))
	var missing []string

	for i, d := range deps {
		fmt.Printf("  의존성 해석: %s (제약: %s, repo: %s)\n", d.Name, d.Version, d.Repository)

		constraint, err := NewConstraint(d.Version)
		if err != nil {
			return nil, fmt.Errorf("의존성 %q의 버전 제약 형식 오류: %w", d.Name, err)
		}

		// file:// 로컬 의존성은 버전 그대로 사용
		if strings.HasPrefix(d.Repository, "file://") {
			locked[i] = &Dependency{
				Name:       d.Name,
				Repository: d.Repository,
				Version:    d.Version,
			}
			fmt.Printf("    → 로컬 의존성, 버전 %s 사용\n", d.Version)
			continue
		}

		// 리포지토리에서 제약 조건을 만족하는 버전 찾기
		repo, ok := r.repos[d.Repository]
		if !ok {
			return nil, fmt.Errorf("리포지토리 %q를 찾을 수 없음 (helm repo update 필요)", d.Repository)
		}

		versions, ok := repo.Entries[d.Name]
		if !ok {
			return nil, fmt.Errorf("차트 %s를 리포지토리 %s에서 찾을 수 없음", d.Name, d.Repository)
		}

		found := false
		locked[i] = &Dependency{
			Name:       d.Name,
			Repository: d.Repository,
			Version:    d.Version,
		}

		// 버전은 이미 내림차순이므로 첫 매치가 최신 버전
		for _, ver := range versions {
			v, err := ParseSemVer(ver.Version)
			if err != nil {
				continue
			}
			if constraint.Check(v) {
				found = true
				locked[i].Version = ver.Version
				fmt.Printf("    → 해석 결과: %s (제약 %s 만족)\n", ver.Version, constraint)
				break
			}
		}

		if !found {
			missing = append(missing, fmt.Sprintf("%q (repository %q, version %q)", d.Name, d.Repository, d.Version))
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("서브차트 %d개의 유효한 버전을 찾을 수 없음: %s",
			len(missing), strings.Join(missing, ", "))
	}

	// Lock 파일 생성
	digest, err := hashReq(deps, locked)
	if err != nil {
		return nil, err
	}

	return &Lock{
		Generated:    time.Now(),
		Digest:       digest,
		Dependencies: locked,
	}, nil
}

// hashReq는 의존성의 해시를 생성한다.
// Helm 소스: internal/resolver/resolver.go의 HashReq
func hashReq(req, lock []*Dependency) (string, error) {
	data, err := json.Marshal([2][]*Dependency{req, lock})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h), nil
}

// --- condition/tags 평가 ---
// Helm 소스: pkg/chart/v2/util/dependencies.go의 processDependencyEnabled

// Values는 차트 values를 표현한다.
type Values map[string]any

// EvaluateCondition은 condition 문자열로 의존성 활성화 여부를 결정한다.
// 예: "mysql.enabled" → values의 mysql.enabled 값 확인
func EvaluateCondition(condition string, values Values) *bool {
	if condition == "" {
		return nil // 조건 없음
	}

	// 쉼표로 분리 (첫 번째 true/false 발견 시 반환)
	for _, cond := range strings.Split(condition, ",") {
		cond = strings.TrimSpace(cond)
		val := getNestedValue(values, cond)
		if b, ok := val.(bool); ok {
			return &b
		}
	}
	return nil // 값이 없으면 판단 불가
}

// EvaluateTags는 tags로 의존성 활성화 여부를 결정한다.
// values의 tags.<tagName>이 false이면 비활성화
func EvaluateTags(tags []string, values Values) *bool {
	if len(tags) == 0 {
		return nil
	}

	tagsMap, ok := values["tags"]
	if !ok {
		return nil
	}
	tm, ok := tagsMap.(map[string]any)
	if !ok {
		return nil
	}

	// 하나라도 true이면 활성화
	for _, tag := range tags {
		if val, ok := tm[tag]; ok {
			if b, ok := val.(bool); ok && b {
				result := true
				return &result
			}
		}
	}

	// 모든 태그가 false이면 비활성화
	result := false
	return &result
}

func getNestedValue(values Values, path string) any {
	keys := strings.Split(path, ".")
	var current any = map[string]any(values)

	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = m[key]
		if !ok {
			return nil
		}
	}
	return current
}

// ProcessDependencyEnabled는 condition과 tags를 평가하여 의존성 활성화 상태를 결정한다.
func ProcessDependencyEnabled(dep *Dependency, values Values) bool {
	// 1. condition이 있으면 condition 우선
	if result := EvaluateCondition(dep.Condition, values); result != nil {
		return *result
	}

	// 2. tags가 있으면 tags 평가
	if result := EvaluateTags(dep.Tags, values); result != nil {
		return *result
	}

	// 3. 둘 다 없으면 기본 활성화
	return true
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              Helm 의존성 해석 PoC                            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("참조: internal/resolver/resolver.go, pkg/chart/v2/dependency.go")
	fmt.Println()

	// =================================================================
	// 1. SemVer 제약 조건 해석
	// =================================================================
	fmt.Println("1. SemVer 제약 조건 해석")
	fmt.Println(strings.Repeat("-", 60))

	testVersions := []string{
		"1.0.0", "1.1.0", "1.2.0", "1.2.3", "1.3.0",
		"1.9.9", "2.0.0", "2.1.0", "0.2.0", "0.2.5", "0.3.0",
	}

	constraintTests := []struct {
		constraint string
		desc       string
	}{
		{"^1.2.0", "caret — major 호환 (>=1.2.0, <2.0.0)"},
		{"~1.2.0", "tilde — minor 호환 (>=1.2.0, <1.3.0)"},
		{">=1.0.0 <2.0.0", "범위 — 명시적 범위"},
		{"1.2.x", "와일드카드 — 패치 자유 (>=1.2.0, <1.3.0)"},
		{"*", "전체 허용"},
		{"^0.2.0", "0.x caret — minor 고정 (>=0.2.0, <0.3.0)"},
		{">=1.2.3", "최소 버전"},
	}

	for _, ct := range constraintTests {
		c, err := NewConstraint(ct.constraint)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}

		var matched []string
		for _, vs := range testVersions {
			v, _ := ParseSemVer(vs)
			if c.Check(v) {
				matched = append(matched, vs)
			}
		}

		fmt.Printf("  %-22s %s\n", ct.constraint, ct.desc)
		fmt.Printf("  %22s 매치: %v\n\n", "", matched)
	}

	// =================================================================
	// 2. 의존성 트리 구축 및 해석
	// =================================================================
	fmt.Println("2. 의존성 트리 구축 및 Lock 파일 생성")
	fmt.Println(strings.Repeat("-", 60))

	// 리포지토리 인덱스 시뮬레이션
	bitnami := &Repository{
		Name: "bitnami",
		URL:  "https://charts.bitnami.com/bitnami",
		Entries: map[string][]ChartVersion{
			"mysql": {
				{Name: "mysql", Version: "9.14.4", URLs: []string{"https://charts.bitnami.com/bitnami/mysql-9.14.4.tgz"}},
				{Name: "mysql", Version: "9.14.3", URLs: []string{"https://charts.bitnami.com/bitnami/mysql-9.14.3.tgz"}},
				{Name: "mysql", Version: "9.13.0", URLs: []string{"https://charts.bitnami.com/bitnami/mysql-9.13.0.tgz"}},
				{Name: "mysql", Version: "9.12.5", URLs: []string{"https://charts.bitnami.com/bitnami/mysql-9.12.5.tgz"}},
				{Name: "mysql", Version: "8.9.2", URLs: []string{"https://charts.bitnami.com/bitnami/mysql-8.9.2.tgz"}},
			},
			"redis": {
				{Name: "redis", Version: "18.6.1", URLs: []string{"https://charts.bitnami.com/bitnami/redis-18.6.1.tgz"}},
				{Name: "redis", Version: "18.5.0", URLs: []string{"https://charts.bitnami.com/bitnami/redis-18.5.0.tgz"}},
				{Name: "redis", Version: "18.4.0", URLs: []string{"https://charts.bitnami.com/bitnami/redis-18.4.0.tgz"}},
				{Name: "redis", Version: "17.15.0", URLs: []string{"https://charts.bitnami.com/bitnami/redis-17.15.0.tgz"}},
			},
			"postgresql": {
				{Name: "postgresql", Version: "13.4.4", URLs: []string{"https://charts.bitnami.com/bitnami/postgresql-13.4.4.tgz"}},
				{Name: "postgresql", Version: "13.3.0", URLs: []string{"https://charts.bitnami.com/bitnami/postgresql-13.3.0.tgz"}},
				{Name: "postgresql", Version: "12.12.10", URLs: []string{"https://charts.bitnami.com/bitnami/postgresql-12.12.10.tgz"}},
			},
		},
	}

	resolver := NewResolver([]*Repository{bitnami})

	// Chart.yaml의 dependencies 시뮬레이션
	deps := []*Dependency{
		{
			Name:       "mysql",
			Version:    "^9.12.0",
			Repository: "https://charts.bitnami.com/bitnami",
			Condition:  "mysql.enabled",
		},
		{
			Name:       "redis",
			Version:    "~18.5",
			Repository: "https://charts.bitnami.com/bitnami",
			Tags:       []string{"cache"},
		},
		{
			Name:       "postgresql",
			Version:    ">=13.0.0 <14.0.0",
			Repository: "https://charts.bitnami.com/bitnami",
			Condition:  "postgresql.enabled",
		},
	}

	fmt.Println("Chart.yaml dependencies:")
	for _, d := range deps {
		fmt.Printf("  - name: %s\n    version: %s\n    repository: %s\n", d.Name, d.Version, d.Repository)
		if d.Condition != "" {
			fmt.Printf("    condition: %s\n", d.Condition)
		}
		if len(d.Tags) > 0 {
			fmt.Printf("    tags: %v\n", d.Tags)
		}
		fmt.Println()
	}

	fmt.Println("의존성 해석 중...")
	lock, err := resolver.Resolve(deps)
	if err != nil {
		fmt.Printf("오류: %v\n", err)
		return
	}

	fmt.Println("\nChart.lock 생성 결과:")
	fmt.Printf("  generated: %s\n", lock.Generated.Format(time.RFC3339))
	fmt.Printf("  digest: %s\n", lock.Digest[:30]+"...")
	fmt.Println("  dependencies:")
	for _, d := range lock.Dependencies {
		fmt.Printf("    - name: %s\n      version: %s\n      repository: %s\n",
			d.Name, d.Version, d.Repository)
	}

	// =================================================================
	// 3. condition/tags 평가
	// =================================================================
	fmt.Println("\n3. condition/tags 평가")
	fmt.Println(strings.Repeat("-", 60))

	values := Values{
		"mysql": map[string]any{
			"enabled": true,
		},
		"postgresql": map[string]any{
			"enabled": false, // 비활성화
		},
		"tags": map[string]any{
			"cache":    true,
			"frontend": false,
		},
	}

	fmt.Printf("  Values:\n")
	b, _ := json.MarshalIndent(values, "    ", "  ")
	fmt.Printf("    %s\n\n", string(b))

	testDeps := []struct {
		dep  Dependency
		desc string
	}{
		{
			Dependency{Name: "mysql", Condition: "mysql.enabled"},
			"condition: mysql.enabled → true",
		},
		{
			Dependency{Name: "postgresql", Condition: "postgresql.enabled"},
			"condition: postgresql.enabled → false",
		},
		{
			Dependency{Name: "redis", Tags: []string{"cache"}},
			"tags: [cache] → true",
		},
		{
			Dependency{Name: "frontend", Tags: []string{"frontend"}},
			"tags: [frontend] → false",
		},
		{
			Dependency{Name: "logging"},
			"condition/tags 없음 → 기본 활성화",
		},
		{
			Dependency{Name: "auth", Condition: "auth.enabled", Tags: []string{"cache"}},
			"condition 우선: auth.enabled 없음 → tags 평가 → cache=true",
		},
	}

	for _, td := range testDeps {
		enabled := ProcessDependencyEnabled(&td.dep, values)
		status := "활성화"
		if !enabled {
			status = "비활성화"
		}
		fmt.Printf("  %-12s : %s → %s\n", td.dep.Name, td.desc, status)
	}

	// =================================================================
	// 4. 의존성 해석 실패 시나리오
	// =================================================================
	fmt.Println("\n4. 의존성 해석 실패 시나리오")
	fmt.Println(strings.Repeat("-", 60))

	failDeps := []*Dependency{
		{
			Name:       "mysql",
			Version:    ">=10.0.0", // 존재하지 않는 버전
			Repository: "https://charts.bitnami.com/bitnami",
		},
	}

	fmt.Println("  의존성: mysql >=10.0.0 (존재하지 않는 버전)")
	_, err = resolver.Resolve(failDeps)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	}

	// =================================================================
	// 5. Lock 파일 변경 감지
	// =================================================================
	fmt.Println("\n5. Lock 파일 변경 감지 (Digest)")
	fmt.Println(strings.Repeat("-", 60))

	deps2 := []*Dependency{
		{Name: "mysql", Version: "^9.14.0", Repository: "https://charts.bitnami.com/bitnami"},
	}
	lock2, _ := resolver.Resolve(deps2)

	fmt.Printf("  원래 Lock digest : %s\n", lock.Digest[:40]+"...")
	fmt.Printf("  변경 후 Lock digest: %s\n", lock2.Digest[:40]+"...")
	fmt.Println("  → Chart.yaml의 dependencies가 변경되면 digest가 달라진다.")
	fmt.Println("    helm dependency build 시 Chart.lock의 digest와 비교하여")
	fmt.Println("    변경 여부를 감지한다.")

	// =================================================================
	// 6. 아키텍처 다이어그램
	// =================================================================
	fmt.Println("\n6. 의존성 해석 아키텍처")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  Chart.yaml                    helm repo index
  ┌──────────────────┐         ┌──────────────────┐
  │ dependencies:     │         │ entries:          │
  │ - name: mysql     │         │   mysql:          │
  │   version: ^9.12  │────────>│     - 9.14.4      │
  │   repo: bitnami   │         │     - 9.14.3      │
  │   condition: ...   │         │     - 9.13.0      │
  │   tags: [cache]   │         │   redis:          │
  └────────┬─────────┘         │     - 18.6.1      │
           │                    └──────────────────┘
           v                              │
  ┌──────────────────┐                    │
  │ Resolver.Resolve  │<──────────────────┘
  │                   │    제약 매칭
  │ 1. SemVer 제약 파싱│
  │ 2. 인덱스에서 검색 │
  │ 3. 최신 매치 선택  │
  │ 4. Lock 생성      │
  └────────┬─────────┘
           │
           v
  ┌──────────────────┐    ┌────────────────────┐
  │ Chart.lock        │    │ condition/tags 평가  │
  │ - mysql: 9.14.4   │    │ values에서 조건 확인  │
  │ - redis: 18.5.0   │    │ 비활성 차트 제외      │
  │ digest: sha256:.. │    └────────────────────┘
  └──────────────────┘
`)
}
