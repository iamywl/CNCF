# 07. Chart 시스템 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Chart 구조체와 핵심 필드](#2-chart-구조체와-핵심-필드)
3. [Metadata와 Dependency](#3-metadata와-dependency)
4. [Accessor 패턴과 버전 추상화](#4-accessor-패턴과-버전-추상화)
5. [공통 타입: File, Values, Capabilities](#5-공통-타입-file-values-capabilities)
6. [Chart 로더 시스템](#6-chart-로더-시스템)
7. [CRD 처리](#7-crd-처리)
8. [Values 병합(Coalesce)](#8-values-병합coalesce)
9. [Lock 파일과 의존성 고정](#9-lock-파일과-의존성-고정)
10. [유효성 검증](#10-유효성-검증)
11. [설계 결정과 Why 분석](#11-설계-결정과-why-분석)

---

## 1. 개요

Helm의 Chart 시스템은 Kubernetes 애플리케이션을 패키징하고 배포하기 위한 핵심 기반이다.
Chart는 단순한 YAML/템플릿 묶음이 아니라, 메타데이터, 의존성 트리, 값(Values), 스키마,
CRD, 템플릿 파일을 포함하는 구조화된 패키지이다.

Helm v4에서는 Chart 시스템에 **버전 추상화 계층**이 도입되어,
v1/v2 API 버전의 차트와 v3 내부 포맷을 통일된 인터페이스로 다룰 수 있게 되었다.

### 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| 버전 독립성 | `Accessor` 인터페이스로 v2/v3 차트를 통일 접근 |
| 트리 구조 | 부모-자식 관계로 의존성 차트를 표현 |
| 불변 Raw 보존 | `Raw` 필드로 원본 파일 내용을 보존 |
| 보안 우선 | 아카이브 로딩 시 경로 탐색 공격 방어 |
| Values 계층화 | 상위 차트 값이 하위 차트 값을 우선 |

### 관련 소스 파일

```
pkg/chart/
├── interfaces.go              # Accessor, DependencyAccessor 인터페이스
├── common.go                  # v2/v3 Accessor 구현, NewAccessor/NewDependencyAccessor
├── dependency.go              # DependencyAccessor 구현
├── common/
│   ├── file.go                # File 구조체
│   ├── values.go              # Values 타입, Table(), PathValue()
│   ├── capabilities.go        # Capabilities, KubeVersion, VersionSet
│   └── util/
│       ├── coalesce.go        # CoalesceValues(), MergeValues()
│       └── values.go          # 값 유틸리티
├── v2/
│   ├── chart.go               # Chart 구조체, CRD, Dependencies
│   ├── metadata.go            # Metadata, Maintainer
│   ├── dependency.go          # Dependency, Lock
│   └── loader/
│       ├── load.go            # LoadFiles(), LoadValues(), MergeMaps()
│       ├── directory.go       # LoadDir()
│       └── archive.go         # LoadFile(), LoadArchive()
└── loader/
    ├── load.go                # 최상위 Load(), LoadDir(), LoadFile()
    └── archive/
        └── archive.go         # LoadArchiveFiles(), EnsureArchive()
```

---

## 2. Chart 구조체와 핵심 필드

### 2.1 Chart 구조체 정의

`pkg/chart/v2/chart.go`에 정의된 `Chart` 구조체는 하나의 Helm 패키지를 나타낸다.

```go
// pkg/chart/v2/chart.go

type Chart struct {
    Raw           []*common.File    `json:"-"`
    Metadata      *Metadata         `json:"metadata"`
    Lock          *Lock             `json:"lock"`
    Templates     []*common.File    `json:"templates"`
    Values        map[string]any    `json:"values"`
    Schema        []byte            `json:"schema"`
    SchemaModTime time.Time         `json:"schemamodtime"`
    Files         []*common.File    `json:"files"`
    ModTime       time.Time         `json:"modtime,omitzero"`

    parent       *Chart   // 부모 차트 (비공개)
    dependencies []*Chart // 의존성 차트 목록 (비공개)
}
```

### 2.2 필드별 역할

| 필드 | 타입 | 역할 | JSON 직렬화 |
|------|------|------|-------------|
| `Raw` | `[]*common.File` | 원본 파일 내용 보존 (`helm show values` 등에서 사용) | 제외 (`json:"-"`) |
| `Metadata` | `*Metadata` | Chart.yaml 내용 (이름, 버전, 의존성 등) | 포함 |
| `Lock` | `*Lock` | Chart.lock 내용 (의존성 고정 정보) | 포함 |
| `Templates` | `[]*common.File` | `templates/` 디렉토리 파일들 | 포함 |
| `Values` | `map[string]any` | `values.yaml` 기본값 | 포함 |
| `Schema` | `[]byte` | `values.schema.json` JSON Schema | 포함 |
| `Files` | `[]*common.File` | 기타 파일 (README, LICENSE, crds/) | 포함 |
| `parent` | `*Chart` | 부모 차트 참조 | 비공개 |
| `dependencies` | `[]*Chart` | 자식 차트 목록 | 비공개 |

### 2.3 트리 탐색 메서드

```
                  Root Chart (parent=nil)
                  /          \
            SubChart-A     SubChart-B
            /
       SubChart-C
```

```go
// pkg/chart/v2/chart.go

func (ch *Chart) IsRoot() bool     { return ch.parent == nil }
func (ch *Chart) Parent() *Chart   { return ch.parent }
func (ch *Chart) Dependencies() []*Chart { return ch.dependencies }

// Root는 재귀적으로 최상위 차트를 찾는다
func (ch *Chart) Root() *Chart {
    if ch.IsRoot() {
        return ch
    }
    return ch.Parent().Root()
}
```

**왜 parent가 비공개(unexported)인가?**
- 외부에서 직접 parent를 조작하면 트리 일관성이 깨질 수 있다
- `AddDependency()`를 통해서만 부모-자식 관계를 설정하도록 강제한다

```go
// pkg/chart/v2/chart.go

func (ch *Chart) AddDependency(charts ...*Chart) {
    for i, x := range charts {
        charts[i].parent = ch  // 부모 참조 설정
        ch.dependencies = append(ch.dependencies, x)
    }
}

func (ch *Chart) SetDependencies(charts ...*Chart) {
    ch.dependencies = nil       // 기존 의존성 초기화
    ch.AddDependency(charts...) // 새로 설정
}
```

### 2.4 경로 표현

차트는 두 가지 경로 표현을 제공한다.

```go
// pkg/chart/v2/chart.go

// 점(.) 표기법: "root.subchart-a.subchart-c"
func (ch *Chart) ChartPath() string {
    if !ch.IsRoot() {
        return ch.Parent().ChartPath() + "." + ch.Name()
    }
    return ch.Name()
}

// 파일 시스템 경로 표기법: "root/charts/subchart-a/charts/subchart-c"
func (ch *Chart) ChartFullPath() string {
    if !ch.IsRoot() {
        return ch.Parent().ChartFullPath() + "/charts/" + ch.Name()
    }
    return ch.Name()
}
```

| 메서드 | SubChart-C의 결과 | 용도 |
|--------|-------------------|------|
| `ChartPath()` | `"myapp.subchart-a.subchart-c"` | 로그, 디버그 |
| `ChartFullPath()` | `"myapp/charts/subchart-a/charts/subchart-c"` | 템플릿 경로 매핑 |

---

## 3. Metadata와 Dependency

### 3.1 Metadata 구조체

`pkg/chart/v2/metadata.go`에 정의된 `Metadata`는 `Chart.yaml` 파일의 내용을 모델링한다.

```go
// pkg/chart/v2/metadata.go

type Metadata struct {
    Name         string            `json:"name,omitempty"`
    Home         string            `json:"home,omitempty"`
    Sources      []string          `json:"sources,omitempty"`
    Version      string            `json:"version,omitempty"`
    Description  string            `json:"description,omitempty"`
    Keywords     []string          `json:"keywords,omitempty"`
    Maintainers  []*Maintainer     `json:"maintainers,omitempty"`
    Icon         string            `json:"icon,omitempty"`
    APIVersion   string            `json:"apiVersion,omitempty"`
    Condition    string            `json:"condition,omitempty"`
    Tags         string            `json:"tags,omitempty"`
    AppVersion   string            `json:"appVersion,omitempty"`
    Deprecated   bool              `json:"deprecated,omitempty"`
    Annotations  map[string]string `json:"annotations,omitempty"`
    KubeVersion  string            `json:"kubeVersion,omitempty"`
    Dependencies []*Dependency     `json:"dependencies,omitempty"`
    Type         string            `json:"type,omitempty"`
}
```

### 3.2 필수 필드와 유효성 검증

`Metadata.Validate()` 메서드는 차트 로딩 후 반드시 호출된다.

```go
// pkg/chart/v2/metadata.go

func (md *Metadata) Validate() error {
    // 1. 문자열 새니타이징 (비인쇄 문자 제거)
    md.Name = sanitizeString(md.Name)
    md.Description = sanitizeString(md.Description)
    // ... 모든 문자열 필드에 대해 수행

    // 2. 필수 필드 검증
    if md.APIVersion == "" {
        return ValidationError("chart.metadata.apiVersion is required")
    }
    if md.Name == "" {
        return ValidationError("chart.metadata.name is required")
    }
    if md.Name != filepath.Base(md.Name) {
        return ValidationErrorf("chart.metadata.name %q is invalid", md.Name)
    }
    if md.Version == "" {
        return ValidationError("chart.metadata.version is required")
    }
    if !isValidSemver(md.Version) {
        return ValidationErrorf("chart.metadata.version %q is invalid", md.Version)
    }
    if !isValidChartType(md.Type) {
        return ValidationError("chart.metadata.type must be application or library")
    }

    // 3. 의존성 중복 검사
    dependencies := map[string]*Dependency{}
    for _, dependency := range md.Dependencies {
        key := dependency.Name
        if dependency.Alias != "" {
            key = dependency.Alias
        }
        if dependencies[key] != nil {
            return ValidationErrorf("more than one dependency with name or alias %q", key)
        }
        dependencies[key] = dependency
    }
    return nil
}
```

**검증 규칙 요약:**

| 검증 항목 | 규칙 |
|-----------|------|
| `apiVersion` | 필수, 비어있으면 안 됨 |
| `name` | 필수, 경로 구분자 포함 불가 |
| `version` | 필수, 유효한 SemVer |
| `type` | `""`, `"application"`, `"library"` 중 하나 |
| `dependencies` | name/alias 기준 중복 불가 |
| alias | `^[a-zA-Z0-9_-]+$` 패턴만 허용 |

### 3.3 Chart 타입

```go
// pkg/chart/v2/metadata.go

func isValidChartType(in string) bool {
    switch in {
    case "", "application", "library":
        return true
    }
    return false
}
```

| 타입 | 설명 |
|------|------|
| `""` (비어있음) | `application`과 동일하게 취급 |
| `application` | 일반 차트, 모든 템플릿 렌더링 |
| `library` | 재사용 가능한 정의만 제공, `_` 접두사 파일만 포함 가능 |

**왜 library 차트가 존재하는가?**
- 공통 헬퍼 템플릿(`_helpers.tpl`)을 여러 차트에서 재사용하기 위함
- library 차트의 비-파셜 템플릿은 렌더링되지 않으므로, 직접 리소스를 생성하지 않음
- 엔진에서 `isTemplateValid()` 함수가 이를 필터링함

### 3.4 Dependency 구조체

```go
// pkg/chart/v2/dependency.go

type Dependency struct {
    Name         string   `json:"name" yaml:"name"`
    Version      string   `json:"version,omitempty" yaml:"version,omitempty"`
    Repository   string   `json:"repository" yaml:"repository"`
    Condition    string   `json:"condition,omitempty" yaml:"condition,omitempty"`
    Tags         []string `json:"tags,omitempty" yaml:"tags,omitempty"`
    Enabled      bool     `json:"enabled,omitempty" yaml:"enabled,omitempty"`
    ImportValues []any    `json:"import-values,omitempty" yaml:"import-values,omitempty"`
    Alias        string   `json:"alias,omitempty" yaml:"alias,omitempty"`
}
```

### 3.5 Maintainer 구조체

```go
// pkg/chart/v2/metadata.go

type Maintainer struct {
    Name  string `json:"name,omitempty"`
    Email string `json:"email,omitempty"`
    URL   string `json:"url,omitempty"`
}
```

---

## 4. Accessor 패턴과 버전 추상화

### 4.1 왜 Accessor 패턴이 필요한가

Helm v4는 v2 포맷(기존 Helm 2/3 차트)과 v3 포맷(새 내부 포맷)을 동시에 지원해야 한다.
직접 타입 변환을 사용하면 모든 소비 코드가 버전별 분기를 포함해야 하므로,
`Accessor` 인터페이스를 통해 통일된 접근 계층을 제공한다.

```
      ┌──────────────────┐
      │  Engine, Action   │  (소비자)
      │  Storage, CLI     │
      └────────┬─────────┘
               │ Accessor 인터페이스
      ┌────────┴─────────┐
      │  chart.Accessor  │  (추상 계층)
      └───┬──────────┬───┘
          │          │
   ┌──────┴──┐  ┌───┴──────┐
   │v2Accessor│  │v3Accessor│  (구현체)
   └────┬────┘  └────┬─────┘
        │             │
   ┌────┴────┐  ┌────┴─────┐
   │v2.Chart │  │v3.Chart  │  (데이터)
   └─────────┘  └──────────┘
```

### 4.2 Accessor 인터페이스

```go
// pkg/chart/interfaces.go

type Charter any       // 차트 타입의 타입 별칭
type Dependency any    // 의존성 타입의 타입 별칭

type Accessor interface {
    Name() string
    IsRoot() bool
    MetadataAsMap() map[string]any
    Files() []*common.File
    Templates() []*common.File
    ChartFullPath() string
    IsLibraryChart() bool
    Dependencies() []Charter
    MetaDependencies() []Dependency
    Values() map[string]any
    Schema() []byte
    Deprecated() bool
}

type DependencyAccessor interface {
    Name() string
    Alias() string
}
```

### 4.3 NewAccessor 팩토리

```go
// pkg/chart/common.go

var NewAccessor func(chrt Charter) (Accessor, error) = NewDefaultAccessor

func NewDefaultAccessor(chrt Charter) (Accessor, error) {
    switch v := chrt.(type) {
    case v2chart.Chart:
        return &v2Accessor{&v}, nil
    case *v2chart.Chart:
        return &v2Accessor{v}, nil
    case v3chart.Chart:
        return &v3Accessor{&v}, nil
    case *v3chart.Chart:
        return &v3Accessor{v}, nil
    default:
        return nil, errors.New("unsupported chart type")
    }
}
```

**왜 `NewAccessor`가 전역 변수인가?**
- 테스트에서 mock으로 교체할 수 있도록 함
- 서드파티 확장에서 커스텀 차트 타입을 지원할 수 있도록 함
- Go의 인터페이스 체계로는 기존 타입에 메서드를 추가할 수 없으므로, 팩토리 패턴으로 우회

### 4.4 v2Accessor 구현 예시

```go
// pkg/chart/common.go

type v2Accessor struct {
    chrt *v2chart.Chart
}

func (r *v2Accessor) Name() string         { return r.chrt.Metadata.Name }
func (r *v2Accessor) IsRoot() bool         { return r.chrt.IsRoot() }
func (r *v2Accessor) Files() []*common.File { return r.chrt.Files }
func (r *v2Accessor) Templates() []*common.File { return r.chrt.Templates }
func (r *v2Accessor) ChartFullPath() string { return r.chrt.ChartFullPath() }
func (r *v2Accessor) Values() map[string]any { return r.chrt.Values }
func (r *v2Accessor) Schema() []byte       { return r.chrt.Schema }
func (r *v2Accessor) Deprecated() bool     { return r.chrt.Metadata.Deprecated }

func (r *v2Accessor) IsLibraryChart() bool {
    return strings.EqualFold(r.chrt.Metadata.Type, "library")
}

func (r *v2Accessor) Dependencies() []Charter {
    var deps = make([]Charter, len(r.chrt.Dependencies()))
    for i, c := range r.chrt.Dependencies() {
        deps[i] = c
    }
    return deps
}

func (r *v2Accessor) MetaDependencies() []Dependency {
    var deps = make([]Dependency, len(r.chrt.Metadata.Dependencies))
    for i, c := range r.chrt.Metadata.Dependencies {
        deps[i] = c
    }
    return deps
}
```

**`MetadataAsMap()` - 리플렉션 기반 변환:**

```go
// pkg/chart/common.go

func (r *v2Accessor) MetadataAsMap() map[string]any {
    ret, err := structToMap(r.chrt.Metadata)
    // ...
    return ret
}
```

이 메서드는 `reflect` 패키지를 사용하여 Metadata 구조체를 `map[string]any`로 변환한다.
템플릿 엔진에서 `.Chart.Name`, `.Chart.Version` 등으로 접근하기 위함이다.

---

## 5. 공통 타입: File, Values, Capabilities

### 5.1 File 구조체

```go
// pkg/chart/common/file.go

type File struct {
    Name    string    `json:"name"`     // 차트 기준 상대 경로
    Data    []byte    `json:"data"`     // 파일 내용
    ModTime time.Time `json:"modtime,omitzero"` // 수정 시각
}
```

`File`은 차트 내의 모든 파일을 나타내는 범용 구조체이다.
`Name`은 차트 루트 디렉토리 기준 상대 경로이며, `templates/deployment.yaml` 또는 `crds/mycrd.yaml`과 같은 형태이다.

### 5.2 Values 타입

```go
// pkg/chart/common/values.go

const GlobalKey = "global"

type Values map[string]any
```

`Values`는 `map[string]any`의 타입 별칭으로, 차트의 설정 값을 나타낸다.

**핵심 메서드:**

```go
// 점(.) 구분 경로로 하위 테이블 접근
func (v Values) Table(name string) (Values, error) {
    table := v
    for _, n := range parsePath(name) {
        table, err = tableLookup(table, n)
        // ...
    }
    return table, err
}

// 경로를 따라 단일 값 조회
func (v Values) PathValue(path string) (any, error) {
    return v.pathValue(parsePath(path))
}

// YAML 직렬화
func (v Values) YAML() (string, error) {
    b, err := yaml.Marshal(v)
    return string(b), err
}

// 안전한 맵 변환 (nil 방지)
func (v Values) AsMap() map[string]any {
    if len(v) == 0 {
        return map[string]any{}
    }
    return v
}
```

**`GlobalKey` 상수의 의미:**
- `"global"` 키 아래의 값은 모든 서브차트에 전파됨
- 부모 차트의 `global.imageRegistry`는 모든 자식 차트에서 접근 가능

### 5.3 ReleaseOptions

```go
// pkg/chart/common/values.go

type ReleaseOptions struct {
    Name      string
    Namespace string
    Revision  int
    IsUpgrade bool
    IsInstall bool
}
```

렌더링 시 `.Release.Name`, `.Release.Namespace` 등의 내장 객체를 생성하는 데 사용된다.

### 5.4 Capabilities 구조체

```go
// pkg/chart/common/capabilities.go

type Capabilities struct {
    KubeVersion KubeVersion
    APIVersions VersionSet
    HelmVersion helmversion.BuildInfo
}

type KubeVersion struct {
    Version           string // 전체 버전 (예: v1.33.4-gke.1245000)
    normalizedVersion string // 정규화 버전 (예: v1.33.4)
    Major             string
    Minor             string
}

type VersionSet []string

func (v VersionSet) Has(apiVersion string) bool {
    return slices.Contains(v, apiVersion)
}
```

**Capabilities의 용도:**
- 템플릿에서 `.Capabilities.KubeVersion`으로 Kubernetes 버전 확인
- `.Capabilities.APIVersions.Has "apps/v1"` 으로 API 버전 확인
- 조건부 리소스 생성에 활용

```
┌──────────────────────────────────────────────┐
│              Capabilities                     │
├──────────────────────────────────────────────┤
│ KubeVersion:                                  │
│   Version = "v1.33.4-gke.1245000"            │
│   normalizedVersion = "v1.33.4"              │
│   Major = "1"                                 │
│   Minor = "33"                                │
├──────────────────────────────────────────────┤
│ APIVersions: ["v1", "apps/v1", ...]          │
├──────────────────────────────────────────────┤
│ HelmVersion: BuildInfo{...}                   │
└──────────────────────────────────────────────┘
```

**기본 Capabilities 생성:**

```go
// pkg/chart/common/capabilities.go

var DefaultCapabilities = func() *Capabilities {
    caps, err := makeDefaultCapabilities()
    // ...
    return caps
}()

func makeDefaultCapabilities() (*Capabilities, error) {
    // k8s.io/client-go 모듈 버전에서 Kubernetes 버전을 추론
    vstr, err := helmversion.K8sIOClientGoModVersion()
    // client-go 버전 + 1 = Kubernetes 메이저 버전
    kubeVersionMajor := v.Major() + 1
    kubeVersionMinor := v.Minor()
    return newCapabilities(kubeVersionMajor, kubeVersionMinor)
}
```

**왜 client-go 버전에서 +1하는가?**
- client-go의 메이저 버전은 Kubernetes 메이저 버전보다 1 낮은 규칙을 따름
- 예: client-go v0.33.x -> Kubernetes v1.33.x

---

## 6. Chart 로더 시스템

### 6.1 로더 아키텍처

```
                    ┌─────────────────────┐
                    │   loader.Load()     │  최상위 진입점
                    │ pkg/chart/loader/   │
                    └────────┬────────────┘
                             │
                    ┌────────┴────────────┐
                    │  Chart.yaml에서      │
                    │  apiVersion 감지     │
                    └───┬──────────┬──────┘
                        │          │
              ┌─────────┴──┐  ┌───┴──────────┐
              │ v2 loader  │  │ v3 loader     │
              │ (v1/v2)    │  │ (내부 포맷)    │
              └─────┬──────┘  └───┬───────────┘
                    │              │
           ┌───────┴───────┐     ...
           │               │
    ┌──────┴──────┐  ┌────┴──────┐
    │ LoadDir()   │  │ LoadFile()│
    │ 디렉토리    │  │ .tgz 아카이브│
    └─────────────┘  └───────────┘
```

### 6.2 최상위 로더 (버전 감지)

```go
// pkg/chart/loader/load.go

func Load(name string) (chart.Charter, error) {
    l, err := Loader(name)  // 파일/디렉토리 자동 감지
    return l.Load()
}

func LoadDir(dir string) (chart.Charter, error) {
    // 1. Chart.yaml 읽기
    data, err := os.ReadFile(filepath.Join(topdir, "Chart.yaml"))
    // 2. apiVersion 감지
    c := new(chartBase)
    yaml.Unmarshal(data, c)
    // 3. 버전별 로더로 위임
    switch c.APIVersion {
    case c2.APIVersionV1, c2.APIVersionV2, "":
        return c2load.Load(dir)   // v2 로더
    case c3.APIVersionV3:
        return c3load.Load(dir)   // v3 로더 (내부)
    default:
        return nil, errors.New("unsupported chart version")
    }
}
```

**API 버전 상수:**
```go
// pkg/chart/v2/chart.go
const APIVersionV1 = "v1"
const APIVersionV2 = "v2"
```

### 6.3 v2 디렉토리 로더

```go
// pkg/chart/v2/loader/directory.go

func LoadDir(dir string) (*chart.Chart, error) {
    topdir, _ := filepath.Abs(dir)
    c := &chart.Chart{}

    // 1. .helmignore 파일 파싱
    rules := ignore.Empty()
    ifile := filepath.Join(topdir, ignore.HelmIgnore)
    if _, err := os.Stat(ifile); err == nil {
        r, _ := ignore.ParseFile(ifile)
        rules = r
    }
    rules.AddDefaults()

    // 2. 파일 시스템 순회
    files := []*archive.BufferedFile{}
    walk := func(name string, fi os.FileInfo, err error) error {
        n := strings.TrimPrefix(name, topdir)
        n = filepath.ToSlash(n)

        if fi.IsDir() {
            if rules.Ignore(n, fi) {
                return filepath.SkipDir  // 디렉토리 전체 건너뛰기
            }
            return nil
        }
        if rules.Ignore(n, fi) { return nil }
        if !fi.Mode().IsRegular() {
            return fmt.Errorf("cannot load irregular file %s", name)
        }
        if fi.Size() > archive.MaxDecompressedFileSize {
            return fmt.Errorf("chart file %q is larger than maximum", fi.Name())
        }

        data, _ := os.ReadFile(name)
        data = bytes.TrimPrefix(data, utf8bom)  // BOM 제거

        files = append(files, &archive.BufferedFile{Name: n, ModTime: fi.ModTime(), Data: data})
        return nil
    }
    sympath.Walk(topdir, walk)  // 심볼릭 링크 지원

    return LoadFiles(files)
}
```

### 6.4 LoadFiles - 핵심 조립 로직

```go
// pkg/chart/v2/loader/load.go

func LoadFiles(files []*archive.BufferedFile) (*chart.Chart, error) {
    c := new(chart.Chart)
    subcharts := make(map[string][]*archive.BufferedFile)

    // 1단계: Chart.yaml 먼저 처리 (순서 독립적)
    for _, f := range files {
        c.Raw = append(c.Raw, &common.File{...})
        if f.Name == "Chart.yaml" {
            c.Metadata = new(chart.Metadata)
            yaml.Unmarshal(f.Data, c.Metadata)
            if c.Metadata.APIVersion == "" {
                c.Metadata.APIVersion = chart.APIVersionV1  // 하위 호환성
            }
        }
    }

    // 2단계: 나머지 파일 분류
    for _, f := range files {
        switch {
        case f.Name == "Chart.yaml":
            continue  // 이미 처리됨
        case f.Name == "Chart.lock":
            c.Lock = new(chart.Lock)
            yaml.Unmarshal(f.Data, &c.Lock)
        case f.Name == "values.yaml":
            values, _ := LoadValues(bytes.NewReader(f.Data))
            c.Values = values
        case f.Name == "values.schema.json":
            c.Schema = f.Data
        case f.Name == "requirements.yaml":   // Deprecated
            // Helm 2 호환성
        case strings.HasPrefix(f.Name, "templates/"):
            c.Templates = append(c.Templates, &common.File{...})
        case strings.HasPrefix(f.Name, "charts/"):
            // 서브차트 파일 수집
            fname := strings.TrimPrefix(f.Name, "charts/")
            cname := strings.SplitN(fname, "/", 2)[0]
            subcharts[cname] = append(subcharts[cname], ...)
        default:
            c.Files = append(c.Files, &common.File{...})
        }
    }

    // 3단계: 유효성 검증
    c.Validate()

    // 4단계: 서브차트 재귀 로딩
    for n, files := range subcharts {
        switch {
        case strings.IndexAny(n, "_.") == 0:
            continue  // 숨김 파일 건너뛰기
        case filepath.Ext(n) == ".tgz":
            sc, _ = LoadArchive(bytes.NewBuffer(file.Data))
        default:
            sc, _ = LoadFiles(buff)
        }
        c.AddDependency(sc)
    }

    return c, nil
}
```

### 6.5 아카이브 로더와 보안

```go
// pkg/chart/loader/archive/archive.go

var MaxDecompressedChartSize int64 = 100 * 1024 * 1024 // 100 MiB
var MaxDecompressedFileSize  int64 = 5 * 1024 * 1024   // 5 MiB

func LoadArchiveFiles(in io.Reader) ([]*BufferedFile, error) {
    unzipped, _ := gzip.NewReader(in)
    tr := tar.NewReader(unzipped)
    remainingSize := MaxDecompressedChartSize

    for {
        hd, err := tr.Next()
        // ...

        // 보안 검사들:
        if path.IsAbs(n) {
            return nil, errors.New("chart illegally contains absolute paths")
        }
        if strings.HasPrefix(n, "..") {
            return nil, errors.New("chart illegally references parent directory")
        }
        if drivePathPattern.MatchString(n) {
            return nil, errors.New("chart contains illegally named files")
        }

        // 크기 제한 검사
        if hd.Size > remainingSize { return nil, ... }
        if hd.Size > MaxDecompressedFileSize { return nil, ... }

        // BOM 제거
        data := bytes.TrimPrefix(b.Bytes(), utf8bom)
        files = append(files, &BufferedFile{Name: n, ModTime: hd.ModTime, Data: data})
    }
}
```

**보안 검사 요약:**

| 검사 | 방어 대상 |
|------|-----------|
| 절대 경로 차단 | 파일 시스템 임의 접근 |
| `..` 경로 차단 | 디렉토리 탐색 공격 |
| 드라이브 문자 패턴 차단 | Windows 경로 공격 |
| 압축 해제 크기 제한 | zip bomb 공격 |
| 개별 파일 크기 제한 | 메모리 소진 공격 |

### 6.6 LoadValues - 멀티 도큐먼트 YAML 지원

```go
// pkg/chart/v2/loader/load.go

func LoadValues(data io.Reader) (map[string]any, error) {
    values := map[string]any{}
    reader := utilyaml.NewYAMLReader(bufio.NewReader(data))
    for {
        raw, err := reader.Read()
        if errors.Is(err, io.EOF) { break }
        currentMap := map[string]any{}
        yaml.Unmarshal(raw, &currentMap)
        values = MergeMaps(values, currentMap)
    }
    return values, nil
}

func MergeMaps(a, b map[string]any) map[string]any {
    out := make(map[string]any, len(a))
    maps.Copy(out, a)
    for k, v := range b {
        if v, ok := v.(map[string]any); ok {
            if bv, ok := out[k]; ok {
                if bv, ok := bv.(map[string]any); ok {
                    out[k] = MergeMaps(bv, v)  // 재귀 병합
                    continue
                }
            }
        }
        out[k] = v  // 스칼라/배열은 덮어쓰기
    }
    return out
}
```

**왜 멀티 도큐먼트 YAML을 지원하는가?**
- `values.yaml`에 `---` 구분자로 여러 섹션을 작성할 수 있게 함
- 대규모 values 파일을 논리적으로 분리하여 관리 가능

---

## 7. CRD 처리

### 7.1 CRD 구조체

```go
// pkg/chart/v2/chart.go

type CRD struct {
    Name     string       // File.Name (예: "crds/mycrd.yaml")
    Filename string       // 차트 경로 포함 (예: "myapp/charts/sub/crds/mycrd.yaml")
    File     *common.File // 파일 객체
}
```

### 7.2 CRD 수집 - 재귀적 탐색

```go
// pkg/chart/v2/chart.go

func (ch *Chart) CRDObjects() []CRD {
    crds := []CRD{}
    for _, f := range ch.Files {
        if strings.HasPrefix(f.Name, "crds/") && hasManifestExtension(f.Name) {
            mycrd := CRD{
                Name:     f.Name,
                Filename: filepath.Join(ch.ChartFullPath(), f.Name),
                File:     f,
            }
            crds = append(crds, mycrd)
        }
    }
    // 의존성 차트의 CRD도 재귀적으로 수집
    for _, dep := range ch.Dependencies() {
        crds = append(crds, dep.CRDObjects()...)
    }
    return crds
}

func hasManifestExtension(fname string) bool {
    ext := filepath.Ext(fname)
    return strings.EqualFold(ext, ".yaml") ||
           strings.EqualFold(ext, ".yml") ||
           strings.EqualFold(ext, ".json")
}
```

**CRD 처리 흐름:**

```
차트 로딩 시:
  crds/ 디렉토리 파일 → ch.Files에 저장 (templates/ 아님)

설치/업그레이드 시:
  1. ch.CRDObjects() 호출로 모든 CRD 수집
  2. CRD를 먼저 클러스터에 적용
  3. 그 후 일반 템플릿 렌더링 및 적용
```

**왜 CRD가 templates/가 아닌 crds/에 있는가?**
- CRD는 템플릿 렌더링 없이 원본 그대로 적용되어야 함
- CRD는 다른 리소스보다 먼저 설치되어야 함 (API 등록 필요)
- 업그레이드 시 CRD 교체 정책이 일반 리소스와 다름

---

## 8. Values 병합(Coalesce)

### 8.1 CoalesceValues 흐름

```go
// pkg/chart/common/util/coalesce.go

func CoalesceValues(chrt chart.Charter, vals map[string]any) (common.Values, error) {
    valsCopy, err := copyValues(vals)  // 깊은 복사
    return coalesce(log.Printf, chrt, valsCopy, "", false)
}

func coalesce(printf printFn, ch chart.Charter, dest map[string]any, prefix string, merge bool) (map[string]any, error) {
    coalesceValues(printf, ch, dest, prefix, merge)   // 현재 차트 값 병합
    return coalesceDeps(printf, ch, dest, prefix, merge) // 의존성 차트 값 병합
}
```

### 8.2 병합 규칙

```
우선순위: 사용자 제공 값 > 부모 차트 값 > 자식 차트 기본값

                    사용자 values.yaml
                         │
                    ┌────┴─────┐
                    │ 루트 차트  │  기본 Values
                    └────┬─────┘
                         │ 병합 (상위 우선)
                    ┌────┴─────┐
                    │ 서브차트   │  기본 Values
                    └──────────┘
```

| 상황 | CoalesceValues (merge=false) | MergeValues (merge=true) |
|------|------------------------------|--------------------------|
| 사용자가 null 설정 | 키 삭제 | null 값 유지 |
| 사용자가 값 설정 | 덮어쓰기 | 덮어쓰기 |
| 사용자가 미설정 | 차트 기본값 사용 | 차트 기본값 사용 |
| 맵끼리 충돌 | 재귀 병합 | 재귀 병합 |
| 맵 vs 스칼라 충돌 | 경고 + 무시 | 경고 + 무시 |

### 8.3 Global 값 전파

```go
// pkg/chart/common/util/coalesce.go

func coalesceGlobals(printf printFn, dest, src map[string]any, prefix string, _ bool) {
    // src의 global 키를 dest의 global로 병합
    for key, val := range sg {
        if istable(val) {
            // 맵이면 재귀 병합
            coalesceTablesFullKey(printf, vv, destvmap, subPrefix, true)
            dg[key] = vv
        } else {
            dg[key] = val  // 스칼라는 직접 설정
        }
    }
    dest[common.GlobalKey] = dg
}
```

---

## 9. Lock 파일과 의존성 고정

### 9.1 Lock 구조체

```go
// pkg/chart/v2/dependency.go

type Lock struct {
    Generated    time.Time     `json:"generated"`
    Digest       string        `json:"digest"`
    Dependencies []*Dependency `json:"dependencies"`
}
```

| 필드 | 역할 |
|------|------|
| `Generated` | Lock 파일 생성 시각 |
| `Digest` | Chart.yaml dependencies 섹션의 해시 |
| `Dependencies` | 고정된 의존성 목록 (정확한 버전) |

**왜 Lock 파일이 필요한가?**
- `Chart.yaml`의 `version: "^2.0.0"`은 범위 지정이므로, 빌드 시점에 따라 다른 버전이 설치될 수 있음
- `Chart.lock`은 `helm dependency update` 실행 시 생성되며, 정확한 버전을 고정함
- `helm dependency build`는 Lock 파일 기반으로 동일한 버전을 보장함
- Digest로 Chart.yaml의 변경 여부를 감지하여, Lock이 구식인지 판별

---

## 10. 유효성 검증

### 10.1 검증 체인

```
Chart.Validate()
    └── Metadata.Validate()
            ├── 문자열 새니타이징
            ├── 필수 필드 확인
            ├── SemVer 검증
            ├── 차트 타입 검증
            ├── Maintainer.Validate()
            └── Dependency.Validate()
                    ├── 문자열 새니타이징
                    └── Alias 패턴 검증
```

### 10.2 sanitizeString 함수

```go
// pkg/chart/v2/metadata.go

func sanitizeString(str string) string {
    return strings.Map(func(r rune) rune {
        if unicode.IsSpace(r) {
            return ' '             // 모든 공백을 단일 공백으로 정규화
        }
        if unicode.IsPrint(r) {
            return r               // 인쇄 가능 문자는 유지
        }
        return -1                  // 비인쇄 문자 제거
    }, str)
}
```

**왜 새니타이징이 필요한가?**
- 차트 이름에 제어 문자가 포함되면 터미널 출력이 깨질 수 있음
- 탭, 줄바꿈 등이 포함되면 YAML 파싱에 문제를 일으킬 수 있음
- 보안 취약점(터미널 이스케이프 시퀀스 주입 등) 방지

---

## 11. 설계 결정과 Why 분석

### 11.1 왜 Raw 필드가 존재하는가?

`Raw` 필드는 `json:"-"` 태그로 직렬화에서 제외된다. 이 필드는 원본 파일 내용을 보존하기 위한 것이다.

`helm show values` 명령은 파싱된 Values 맵이 아니라, 원본 `values.yaml` 파일을 보여주어야 한다.
주석과 서식이 보존된 원본을 표시하려면 Raw 데이터가 필요하다.

### 11.2 왜 Charter와 Dependency가 any 타입인가?

```go
type Charter any
type Dependency any
```

Go의 타입 시스템에서 서로 다른 패키지의 구조체에 공통 인터페이스를 강제할 수 없다.
v2.Chart와 v3.Chart는 별도 패키지에 정의되어 있으므로, `any` 타입 별칭을 사용하고
런타임에 타입 스위치로 처리한다.

### 11.3 왜 2단계 로더 아키텍처인가?

```
최상위 로더 (pkg/chart/loader/)  →  버전 감지만 담당
        │
v2 로더 (pkg/chart/v2/loader/)  →  실제 파일 파싱 담당
```

- 버전 감지 로직과 파싱 로직을 분리하여 단일 책임 원칙 준수
- 새로운 차트 버전(v3) 추가 시 최상위 로더의 switch 문만 확장하면 됨
- v2 로더는 v2.Chart 타입에만 의존하므로 순환 의존성 방지

### 11.4 왜 values.yaml을 멀티 도큐먼트로 지원하는가?

대규모 프로젝트에서는 values 파일이 수백 줄에 달할 수 있다.
멀티 도큐먼트 YAML을 지원하면 환경별 또는 컴포넌트별로 논리적 분리가 가능하다.
뒤의 도큐먼트가 앞의 도큐먼트를 재귀적으로 병합하므로, 점진적으로 값을 오버라이드할 수 있다.

### 11.5 왜 .helmignore가 아카이브에서는 무시되는가?

아카이브(`.tgz`)는 이미 패키징이 완료된 산출물이다.
패키징 시점에 `.helmignore`가 적용되어 불필요한 파일이 제외되었으므로,
아카이브 로딩 시 다시 적용할 필요가 없다.
또한, 아카이브에서 `.helmignore` 자체가 제외되었을 수 있으므로 적용이 불가능할 수 있다.

### 11.6 전체 데이터 흐름 요약

```
  파일 시스템 / 아카이브
        │
        ▼
  ┌─────────────┐
  │ Load()       │  파일 읽기 + .helmignore 필터링
  └──────┬──────┘
         │
         ▼
  ┌──────────────┐
  │ LoadFiles()  │  파일 분류 + Chart.yaml 파싱
  │              │  + 서브차트 재귀 로딩
  └──────┬───────┘
         │
         ▼
  ┌──────────────┐
  │ Validate()   │  메타데이터 + 의존성 검증
  └──────┬───────┘
         │
         ▼
  ┌──────────────┐
  │ CoalesceValues│  사용자 값 + 차트 기본값 병합
  └──────┬───────┘
         │
         ▼
  ┌──────────────┐
  │ Engine.Render│  템플릿 렌더링
  └──────────────┘
```

---

## 참고: 핵심 소스 파일 경로

| 파일 | 경로 |
|------|------|
| Chart 구조체 | `pkg/chart/v2/chart.go` |
| Metadata/Maintainer | `pkg/chart/v2/metadata.go` |
| Dependency/Lock | `pkg/chart/v2/dependency.go` |
| Accessor 인터페이스 | `pkg/chart/interfaces.go` |
| Accessor 구현 | `pkg/chart/common.go` |
| File 구조체 | `pkg/chart/common/file.go` |
| Values 타입 | `pkg/chart/common/values.go` |
| Capabilities | `pkg/chart/common/capabilities.go` |
| 최상위 로더 | `pkg/chart/loader/load.go` |
| v2 파일 로더 | `pkg/chart/v2/loader/load.go` |
| v2 디렉토리 로더 | `pkg/chart/v2/loader/directory.go` |
| v2 아카이브 로더 | `pkg/chart/v2/loader/archive.go` |
| 아카이브 유틸 | `pkg/chart/loader/archive/archive.go` |
| Values Coalesce | `pkg/chart/common/util/coalesce.go` |
