package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// =============================================================================
// Helm 리포지토리 인덱스 PoC
// =============================================================================
//
// 참조: pkg/repo/v1/index.go, pkg/repo/v1/chartrepo.go
//
// Helm 차트 리포지토리의 핵심은 index.yaml 파일이다.
// 이 PoC는 다음을 시뮬레이션한다:
//   1. IndexFile 구조 — YAML 포맷, entries(chart → versions)
//   2. Chart 검색 — 이름, 버전 필터
//   3. 인덱스 생성 — 차트 패키지에서 인덱스 빌드
//   4. 인덱스 병합 — 두 리포지토리 인덱스 병합
// =============================================================================

// --- SemVer: 간이 시맨틱 버전 ---
type SemVer struct {
	Major, Minor, Patch int
	Prerelease          string
}

func ParseSemVer(s string) (SemVer, error) {
	s = strings.TrimPrefix(s, "v")
	var pre string
	if idx := strings.Index(s, "-"); idx >= 0 {
		pre = s[idx+1:]
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 3 {
		for len(parts) < 3 {
			parts = append(parts, "0")
		}
	}
	maj, _ := strconv.Atoi(parts[0])
	min, _ := strconv.Atoi(parts[1])
	pat, _ := strconv.Atoi(parts[2])
	return SemVer{maj, min, pat, pre}, nil
}

func (v SemVer) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

func (v SemVer) LessThan(o SemVer) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	if v.Patch != o.Patch {
		return v.Patch < o.Patch
	}
	if v.Prerelease != "" && o.Prerelease == "" {
		return true
	}
	return false
}

// --- Metadata: 차트 메타데이터 ---
// Helm 소스: pkg/chart/v2/metadata.go의 Metadata 구조체
type Metadata struct {
	APIVersion  string `json:"apiVersion"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	AppVersion  string `json:"appVersion,omitempty"`
	Type        string `json:"type,omitempty"` // application 또는 library
}

// --- ChartVersion: 인덱스의 차트 엔트리 ---
// Helm 소스: pkg/repo/v1/index.go의 ChartVersion 구조체
type ChartVersion struct {
	*Metadata
	URLs    []string  `json:"urls"`
	Created time.Time `json:"created"`
	Digest  string    `json:"digest,omitempty"`
	Removed bool      `json:"removed,omitempty"`
}

// --- ChartVersions: 버전 목록 (SemVer 정렬 가능) ---
// Helm 소스: pkg/repo/v1/index.go의 ChartVersions 타입
type ChartVersions []*ChartVersion

func (c ChartVersions) Len() int      { return len(c) }
func (c ChartVersions) Swap(i, j int) { c[i], c[j] = c[j], c[i] }

// Less — 낮은 버전이 앞에 (SortEntries에서 Reverse로 내림차순)
// Helm 소스: 파싱 실패 시 뒤로 밀어냄
func (c ChartVersions) Less(a, b int) bool {
	i, err := ParseSemVer(c[a].Version)
	if err != nil {
		return true
	}
	j, err := ParseSemVer(c[b].Version)
	if err != nil {
		return false
	}
	return i.LessThan(j)
}

// --- IndexFile: 리포지토리 인덱스 파일 ---
// Helm 소스: pkg/repo/v1/index.go의 IndexFile 구조체
// index.yaml의 최상위 구조
type IndexFile struct {
	APIVersion string                   `json:"apiVersion"`
	Generated  time.Time                `json:"generated"`
	Entries    map[string]ChartVersions `json:"entries"`
	PublicKeys []string                 `json:"publicKeys,omitempty"`
}

// NewIndexFile은 빈 인덱스 파일을 생성한다.
// Helm 소스: pkg/repo/v1/index.go의 NewIndexFile
func NewIndexFile() *IndexFile {
	return &IndexFile{
		APIVersion: "v1",
		Generated:  time.Now(),
		Entries:    map[string]ChartVersions{},
		PublicKeys: []string{},
	}
}

// MustAdd는 차트를 인덱스에 추가한다.
// Helm 소스: pkg/repo/v1/index.go의 MustAdd
func (idx *IndexFile) MustAdd(md *Metadata, filename, baseURL, digest string) error {
	if idx.Entries == nil {
		return fmt.Errorf("entries가 초기화되지 않음")
	}
	if md.APIVersion == "" {
		md.APIVersion = "v2"
	}

	url := filename
	if baseURL != "" {
		url = baseURL + "/" + filename
	}

	cv := &ChartVersion{
		Metadata: md,
		URLs:     []string{url},
		Created:  time.Now(),
		Digest:   digest,
	}

	idx.Entries[md.Name] = append(idx.Entries[md.Name], cv)
	return nil
}

// SortEntries는 모든 엔트리를 버전 내림차순으로 정렬한다.
// Helm 소스: pkg/repo/v1/index.go의 SortEntries
// 최신 버전이 0번 인덱스에 위치하도록 정렬
func (idx *IndexFile) SortEntries() {
	for _, versions := range idx.Entries {
		sort.Sort(sort.Reverse(versions))
	}
}

// Has는 특정 이름/버전의 차트가 존재하는지 확인한다.
// Helm 소스: pkg/repo/v1/index.go의 Has
func (idx *IndexFile) Has(name, version string) bool {
	_, err := idx.Get(name, version)
	return err == nil
}

// Get은 이름과 버전으로 차트를 검색한다.
// Helm 소스: pkg/repo/v1/index.go의 Get
// version이 빈 문자열이면 안정 최신 버전 반환 (prerelease 제외)
func (idx *IndexFile) Get(name, version string) (*ChartVersion, error) {
	vs, ok := idx.Entries[name]
	if !ok {
		return nil, fmt.Errorf("차트 %q를 찾을 수 없음", name)
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("차트 %q의 버전 없음", name)
	}

	if version == "" {
		// 안정 최신 버전 (prerelease 제외)
		for _, v := range vs {
			sv, err := ParseSemVer(v.Version)
			if err != nil {
				continue
			}
			if sv.Prerelease == "" {
				return v, nil
			}
		}
		return vs[0], nil // prerelease만 있으면 첫 번째 반환
	}

	// 정확한 버전 매치
	for _, v := range vs {
		if v.Version == version {
			return v, nil
		}
	}

	return nil, fmt.Errorf("차트 %s-%s를 찾을 수 없음", name, version)
}

// Merge는 다른 인덱스 파일을 병합한다.
// Helm 소스: pkg/repo/v1/index.go의 Merge
// 이미 존재하는 name/version 조합은 건너뛴다 (기존 레코드 유지)
func (idx *IndexFile) Merge(other *IndexFile) {
	for _, cvs := range other.Entries {
		for _, cv := range cvs {
			if !idx.Has(cv.Name, cv.Version) {
				idx.Entries[cv.Name] = append(idx.Entries[cv.Name], cv)
			}
		}
	}
}

// Search는 키워드로 차트를 검색한다.
func (idx *IndexFile) Search(keyword string) []*ChartVersion {
	var results []*ChartVersion
	keyword = strings.ToLower(keyword)

	for _, versions := range idx.Entries {
		for _, cv := range versions {
			if strings.Contains(strings.ToLower(cv.Name), keyword) ||
				strings.Contains(strings.ToLower(cv.Description), keyword) {
				results = append(results, cv)
				break // 차트당 최신 버전만
			}
		}
	}
	return results
}

// --- IndexDirectory: 디렉토리에서 인덱스 생성 ---
// Helm 소스: pkg/repo/v1/index.go의 IndexDirectory
// 실제 Helm은 *.tgz 파일을 스캔하지만, 여기서는 메타데이터로 시뮬레이션
func IndexDirectory(charts []struct {
	Metadata *Metadata
	Filename string
	Digest   string
}, baseURL string) (*IndexFile, error) {
	idx := NewIndexFile()
	for _, ch := range charts {
		if err := idx.MustAdd(ch.Metadata, ch.Filename, baseURL, ch.Digest); err != nil {
			return nil, fmt.Errorf("인덱스 추가 실패 %s: %w", ch.Filename, err)
		}
	}
	return idx, nil
}

func prettyJSON(v any) string {
	b, _ := json.MarshalIndent(v, "    ", "  ")
	return string(b)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              Helm 리포지토리 인덱스 PoC                       ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("참조: pkg/repo/v1/index.go, pkg/repo/v1/chartrepo.go")
	fmt.Println()

	// =================================================================
	// 1. IndexFile 구조
	// =================================================================
	fmt.Println("1. IndexFile 구조 (index.yaml)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  index.yaml 구조:
  ┌─────────────────────────────────────────┐
  │ apiVersion: v1                           │
  │ generated: 2024-01-15T10:30:00Z          │
  │ entries:                                 │
  │   nginx:                                 │
  │     - name: nginx                        │
  │       version: 15.4.0                    │
  │       urls: [https://repo/nginx-15.4.tgz]│
  │       created: 2024-01-15T10:30:00Z      │
  │       digest: sha256:abc123...           │
  │     - name: nginx                        │
  │       version: 15.3.0                    │
  │       ...                                │
  │   mysql:                                 │
  │     - name: mysql                        │
  │       version: 9.14.4                    │
  │       ...                                │
  └─────────────────────────────────────────┘
`)

	// =================================================================
	// 2. 인덱스 생성 (helm repo index)
	// =================================================================
	fmt.Println("2. 인덱스 생성 (helm repo index 시뮬레이션)")
	fmt.Println(strings.Repeat("-", 60))

	charts := []struct {
		Metadata *Metadata
		Filename string
		Digest   string
	}{
		{
			Metadata: &Metadata{Name: "nginx", Version: "15.4.0", Description: "NGINX 웹 서버", AppVersion: "1.25.3", Type: "application"},
			Filename: "nginx-15.4.0.tgz",
			Digest:   "sha256:a1b2c3d4e5f6...",
		},
		{
			Metadata: &Metadata{Name: "nginx", Version: "15.3.0", Description: "NGINX 웹 서버", AppVersion: "1.25.2", Type: "application"},
			Filename: "nginx-15.3.0.tgz",
			Digest:   "sha256:b2c3d4e5f6g7...",
		},
		{
			Metadata: &Metadata{Name: "nginx", Version: "15.2.0", Description: "NGINX 웹 서버", AppVersion: "1.25.1", Type: "application"},
			Filename: "nginx-15.2.0.tgz",
			Digest:   "sha256:c3d4e5f6g7h8...",
		},
		{
			Metadata: &Metadata{Name: "nginx", Version: "16.0.0-beta.1", Description: "NGINX 웹 서버 (베타)", AppVersion: "1.26.0", Type: "application"},
			Filename: "nginx-16.0.0-beta.1.tgz",
			Digest:   "sha256:d4e5f6g7h8i9...",
		},
		{
			Metadata: &Metadata{Name: "mysql", Version: "9.14.4", Description: "MySQL 데이터베이스", AppVersion: "8.0.36", Type: "application"},
			Filename: "mysql-9.14.4.tgz",
			Digest:   "sha256:e5f6g7h8i9j0...",
		},
		{
			Metadata: &Metadata{Name: "mysql", Version: "9.13.0", Description: "MySQL 데이터베이스", AppVersion: "8.0.35", Type: "application"},
			Filename: "mysql-9.13.0.tgz",
			Digest:   "sha256:f6g7h8i9j0k1...",
		},
		{
			Metadata: &Metadata{Name: "redis", Version: "18.6.1", Description: "Redis 인메모리 데이터 저장소", AppVersion: "7.2.4", Type: "application"},
			Filename: "redis-18.6.1.tgz",
			Digest:   "sha256:g7h8i9j0k1l2...",
		},
		{
			Metadata: &Metadata{Name: "common", Version: "2.13.3", Description: "Bitnami 공통 라이브러리 차트", Type: "library"},
			Filename: "common-2.13.3.tgz",
			Digest:   "sha256:h8i9j0k1l2m3...",
		},
	}

	idx, err := IndexDirectory(charts, "https://charts.bitnami.com/bitnami")
	if err != nil {
		fmt.Printf("오류: %v\n", err)
		return
	}

	// 정렬 (최신 버전 먼저)
	idx.SortEntries()

	fmt.Println("생성된 인덱스:")
	fmt.Printf("  apiVersion: %s\n", idx.APIVersion)
	fmt.Printf("  generated: %s\n", idx.Generated.Format(time.RFC3339))
	fmt.Printf("  차트 수: %d\n", len(idx.Entries))

	for name, versions := range idx.Entries {
		fmt.Printf("\n  %s: (%d 버전)\n", name, len(versions))
		for _, v := range versions {
			fmt.Printf("    - version: %-15s appVersion: %-8s type: %-11s url: %s\n",
				v.Version, v.AppVersion, v.Type, v.URLs[0])
		}
	}

	// =================================================================
	// 3. 차트 검색
	// =================================================================
	fmt.Println("\n3. 차트 검색")
	fmt.Println(strings.Repeat("-", 60))

	// Get — 이름 + 버전 검색
	fmt.Println("\n  Get(nginx, \"\") — 안정 최신 버전:")
	cv, err := idx.Get("nginx", "")
	if err == nil {
		fmt.Printf("    %s (version: %s, appVersion: %s)\n", cv.Name, cv.Version, cv.AppVersion)
	}

	fmt.Println("\n  Get(nginx, \"15.3.0\") — 정확한 버전:")
	cv, err = idx.Get("nginx", "15.3.0")
	if err == nil {
		fmt.Printf("    %s (version: %s, appVersion: %s)\n", cv.Name, cv.Version, cv.AppVersion)
	}

	fmt.Println("\n  Get(nginx, \"99.0.0\") — 없는 버전:")
	_, err = idx.Get("nginx", "99.0.0")
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	}

	fmt.Println("\n  Get(nonexistent, \"\") — 없는 차트:")
	_, err = idx.Get("nonexistent", "")
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	}

	// Has — 존재 여부 확인
	fmt.Println("\n  Has 확인:")
	fmt.Printf("    Has(mysql, 9.14.4) = %v\n", idx.Has("mysql", "9.14.4"))
	fmt.Printf("    Has(mysql, 10.0.0) = %v\n", idx.Has("mysql", "10.0.0"))

	// Search — 키워드 검색
	fmt.Println("\n  Search(\"데이터\") — 키워드 검색:")
	results := idx.Search("데이터")
	for _, r := range results {
		fmt.Printf("    %s (version: %s) — %s\n", r.Name, r.Version, r.Description)
	}

	fmt.Println("\n  Search(\"library\") — 타입으로 검색:")
	results = idx.Search("라이브러리")
	for _, r := range results {
		fmt.Printf("    %s (version: %s, type: %s) — %s\n", r.Name, r.Version, r.Type, r.Description)
	}

	// =================================================================
	// 4. 인덱스 병합 (helm repo index --merge)
	// =================================================================
	fmt.Println("\n4. 인덱스 병합 (helm repo index --merge)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("기존 인덱스에 새로운 차트 추가 시, 이미 있는 name/version은 유지")

	// 새로운 차트 버전 추가
	newCharts := []struct {
		Metadata *Metadata
		Filename string
		Digest   string
	}{
		{
			Metadata: &Metadata{Name: "nginx", Version: "15.5.0", Description: "NGINX 웹 서버", AppVersion: "1.25.4", Type: "application"},
			Filename: "nginx-15.5.0.tgz",
			Digest:   "sha256:new1...",
		},
		{
			Metadata: &Metadata{Name: "nginx", Version: "15.4.0", Description: "NGINX 웹 서버 (업데이트)", AppVersion: "1.25.3", Type: "application"},
			Filename: "nginx-15.4.0-updated.tgz",
			Digest:   "sha256:updated...",
		},
		{
			Metadata: &Metadata{Name: "postgresql", Version: "13.4.4", Description: "PostgreSQL 데이터베이스", AppVersion: "16.2", Type: "application"},
			Filename: "postgresql-13.4.4.tgz",
			Digest:   "sha256:pg1...",
		},
	}

	newIdx, _ := IndexDirectory(newCharts, "https://charts.bitnami.com/bitnami")

	fmt.Println("\n  기존 인덱스 nginx 버전:")
	for _, v := range idx.Entries["nginx"] {
		fmt.Printf("    - %s (digest: %s)\n", v.Version, v.Digest)
	}

	fmt.Println("\n  새 인덱스에 추가할 항목:")
	fmt.Println("    - nginx 15.5.0 (신규)")
	fmt.Println("    - nginx 15.4.0 (이미 존재 → 무시)")
	fmt.Println("    - postgresql 13.4.4 (신규 차트)")

	// 병합 실행
	idx.Merge(newIdx)
	idx.SortEntries()

	fmt.Println("\n  병합 후 nginx 버전:")
	for _, v := range idx.Entries["nginx"] {
		fmt.Printf("    - %s (digest: %s)\n", v.Version, v.Digest)
	}

	fmt.Println("\n  병합 후 postgresql 추가됨:")
	if pgVersions, ok := idx.Entries["postgresql"]; ok {
		for _, v := range pgVersions {
			fmt.Printf("    - %s (appVersion: %s)\n", v.Version, v.AppVersion)
		}
	}

	// =================================================================
	// 5. 인덱스 YAML 출력 (시뮬레이션)
	// =================================================================
	fmt.Println("\n5. 인덱스 직렬화 (JSON 형식)")
	fmt.Println(strings.Repeat("-", 60))

	// nginx만 발췌하여 표시
	sample := &IndexFile{
		APIVersion: idx.APIVersion,
		Generated:  idx.Generated,
		Entries: map[string]ChartVersions{
			"nginx": idx.Entries["nginx"][:2], // 상위 2개만
		},
	}
	fmt.Printf("    %s\n", prettyJSON(sample))

	// =================================================================
	// 6. 아키텍처 다이어그램
	// =================================================================
	fmt.Println("\n6. 리포지토리 인덱스 아키텍처")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  helm repo add bitnami https://charts.bitnami.com/bitnami
       │
       v
  ┌─────────────────────────┐    GET /index.yaml
  │  helm repo update        │──────────────────>  HTTP 서버
  │  (인덱스 다운로드/캐싱)   │<──────────────────  index.yaml
  └────────┬────────────────┘
           │
           v
  ~/.cache/helm/repository/
  ├── bitnami-index.yaml     ← 캐싱된 인덱스
  └── stable-index.yaml

  helm search repo nginx
       │
       v
  ┌─────────────────────────┐
  │ IndexFile.Search("nginx")│
  │  - 캐싱된 인덱스 로드     │
  │  - Entries에서 검색       │
  │  - SortEntries (내림차순) │
  └─────────────────────────┘

  helm repo index ./charts --url https://example.com/charts
       │
       v
  ┌─────────────────────────┐
  │ IndexDirectory           │
  │  1. *.tgz 파일 스캔      │
  │  2. 각 파일에서 Metadata  │
  │     추출 (Chart.yaml)    │
  │  3. SHA256 digest 계산   │
  │  4. IndexFile.MustAdd     │
  │  5. SortEntries           │
  │  6. WriteFile → index.yaml│
  └─────────────────────────┘

  helm repo index . --merge existing-index.yaml
       │
       v
  ┌─────────────────────────┐
  │ IndexFile.Merge           │
  │  - name+version로 중복 검사│
  │  - 새로운 엔트리만 추가    │
  │  - 기존 레코드 유지       │
  └─────────────────────────┘
`)
}
