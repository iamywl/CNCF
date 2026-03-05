# 09. Release 라이프사이클 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Release 구조체](#2-release-구조체)
3. [Info와 Status](#3-info와-status)
4. [Hook 시스템](#4-hook-시스템)
5. [Accessor 패턴](#5-accessor-패턴)
6. [매니페스트 분류와 정렬](#6-매니페스트-분류와-정렬)
7. [Release 필터링과 정렬](#7-release-필터링과-정렬)
8. [라이프사이클 상태 전이](#8-라이프사이클-상태-전이)
9. [ApplyMethod: CSA vs SSA](#9-applymethod-csa-vs-ssa)
10. [설계 결정과 Why 분석](#10-설계-결정과-why-분석)

---

## 1. 개요

Release는 Helm에서 차트의 **배포 인스턴스**를 나타내는 핵심 개념이다.
같은 차트를 여러 번 설치하면 각각이 독립적인 Release가 된다.
각 Release는 고유한 이름, 네임스페이스, 리비전 번호를 갖고,
설치/업그레이드/롤백/삭제의 라이프사이클을 거친다.

### Release의 핵심 특성

| 특성 | 설명 |
|------|------|
| 고유 식별 | 이름 + 네임스페이스 조합으로 유일하게 식별 |
| 버전 관리 | 매 업그레이드마다 리비전 번호 증가 |
| 상태 추적 | 9가지 상태로 배포 상태 표현 |
| Hook 지원 | 라이프사이클 이벤트에 Hook 실행 |
| 스냅샷 보존 | 차트, 설정값, 매니페스트를 모두 저장 |

### 관련 소스 파일

```
pkg/release/
├── interfaces.go            # Accessor, HookAccessor 인터페이스
├── common.go                # NewAccessor/NewHookAccessor 구현, v1/v2 Accessor
├── responses.go             # UninstallReleaseResponse
├── common/
│   └── status.go            # Status 타입, 상태 상수
└── v1/
    ├── release.go           # Release 구조체, ApplyMethod
    ├── info.go              # Info 구조체, 커스텀 JSON 마샬링
    ├── hook.go              # Hook, HookEvent, HookPhase, HookDeletePolicy
    ├── mock.go              # 테스트용 Mock
    └── util/
        ├── filter.go        # FilterFunc, Any, All, StatusFilter
        ├── sorter.go        # SortByName, SortByDate, SortByRevision
        ├── manifest.go      # SplitManifests, BySplitManifestsOrder
        ├── manifest_sorter.go # SortManifests, Hook/Manifest 분류
        └── kind_sorter.go   # InstallOrder, UninstallOrder
```

---

## 2. Release 구조체

### 2.1 구조체 정의

```go
// pkg/release/v1/release.go

type ApplyMethod string

const ApplyMethodClientSideApply ApplyMethod = "csa"
const ApplyMethodServerSideApply ApplyMethod = "ssa"

type Release struct {
    Name        string            `json:"name,omitempty"`
    Info        *Info             `json:"info,omitempty"`
    Chart       *chart.Chart      `json:"chart,omitempty"`
    Config      map[string]any    `json:"config,omitempty"`
    Manifest    string            `json:"manifest,omitempty"`
    Hooks       []*Hook           `json:"hooks,omitempty"`
    Version     int               `json:"version,omitempty"`
    Namespace   string            `json:"namespace,omitempty"`
    Labels      map[string]string `json:"-"`
    ApplyMethod string            `json:"apply_method,omitempty"`
}
```

### 2.2 필드별 역할

| 필드 | 타입 | 역할 | 비고 |
|------|------|------|------|
| `Name` | `string` | Release 이름 | 클러스터 내 유일 |
| `Info` | `*Info` | 배포 시각, 상태, 설명 | 아래 상세 |
| `Chart` | `*chart.Chart` | 배포에 사용된 차트 스냅샷 | 전체 차트 보존 |
| `Config` | `map[string]any` | 사용자가 제공한 오버라이드 값 | 차트 기본값 제외 |
| `Manifest` | `string` | 렌더링된 Kubernetes 매니페스트 | YAML 문자열 |
| `Hooks` | `[]*Hook` | Hook 리소스 목록 | 아래 상세 |
| `Version` | `int` | 리비전 번호 | 1부터 시작, 업그레이드마다 +1 |
| `Namespace` | `string` | 대상 네임스페이스 | |
| `Labels` | `map[string]string` | 사용자 라벨 | JSON 직렬화 제외 |
| `ApplyMethod` | `string` | 적용 방식 (`"csa"` or `"ssa"`) | v4 신규 |

### 2.3 SetStatus 헬퍼

```go
// pkg/release/v1/release.go

func (r *Release) SetStatus(status common.Status, msg string) {
    r.Info.Status = status
    r.Info.Description = msg
}
```

**왜 Labels가 `json:"-"`인가?**
- Labels는 스토리지 드라이버의 메타데이터 필드에 별도 저장됨
- Kubernetes Secret/ConfigMap의 ObjectMeta.Labels에 저장
- Release body에 중복 저장하면 일관성 문제 발생 가능
- 스토리지 드라이버가 읽어올 때 별도로 Labels를 주입

### 2.4 Release 데이터 구조 다이어그램

```
┌──────────────────────────────────────────┐
│              Release                      │
├──────────────────────────────────────────┤
│ Name:      "my-app"                       │
│ Namespace: "production"                   │
│ Version:   3                              │
│ ApplyMethod: "ssa"                        │
├──────────────────────────────────────────┤
│ Info:                                     │
│   ├── Status: "deployed"                  │
│   ├── FirstDeployed: 2024-01-15T10:00:00  │
│   ├── LastDeployed:  2024-03-01T14:30:00  │
│   ├── Description: "Upgrade complete"     │
│   └── Notes: "Visit http://..."           │
├──────────────────────────────────────────┤
│ Chart: *chart.Chart                       │
│   ├── Metadata: {Name: "my-app", ...}    │
│   ├── Templates: [deployment.yaml, ...]   │
│   └── Values: {replicaCount: 3, ...}     │
├──────────────────────────────────────────┤
│ Config: {replicaCount: 5, image: "v2"}   │
├──────────────────────────────────────────┤
│ Manifest: "---\napiVersion: apps/v1\n..." │
├──────────────────────────────────────────┤
│ Hooks:                                    │
│   ├── pre-upgrade: db-migrate Job         │
│   └── post-upgrade: health-check Pod      │
├──────────────────────────────────────────┤
│ Labels: {"team": "backend"}               │
└──────────────────────────────────────────┘
```

---

## 3. Info와 Status

### 3.1 Info 구조체

```go
// pkg/release/v1/info.go

type Info struct {
    FirstDeployed time.Time                      `json:"first_deployed,omitzero"`
    LastDeployed  time.Time                      `json:"last_deployed,omitzero"`
    Deleted       time.Time                      `json:"deleted,omitzero"`
    Description   string                         `json:"description,omitempty"`
    Status        common.Status                  `json:"status,omitempty"`
    Notes         string                         `json:"notes,omitempty"`
    Resources     map[string][]runtime.Object    `json:"resources,omitempty"`
}
```

| 필드 | 설명 |
|------|------|
| `FirstDeployed` | 최초 설치 시각 (이후 변경되지 않음) |
| `LastDeployed` | 마지막 배포(업그레이드/롤백) 시각 |
| `Deleted` | 삭제 시각 (`--keep-history` 사용 시) |
| `Description` | 현재 상태에 대한 설명 메시지 |
| `Status` | 현재 Release 상태 |
| `Notes` | `templates/NOTES.txt` 렌더링 결과 |
| `Resources` | 배포된 리소스 정보 (v4 신규) |

### 3.2 커스텀 JSON 마샬링

```go
// pkg/release/v1/info.go

// infoJSON은 포인터 타임 필드를 사용하여 zero value 처리
type infoJSON struct {
    FirstDeployed *time.Time    `json:"first_deployed,omitempty"`
    LastDeployed  *time.Time    `json:"last_deployed,omitempty"`
    Deleted       *time.Time    `json:"deleted,omitempty"`
    Description   string        `json:"description,omitempty"`
    Status        common.Status `json:"status,omitempty"`
    Notes         string        `json:"notes,omitempty"`
    Resources     map[string][]runtime.Object `json:"resources,omitempty"`
}

func (i *Info) UnmarshalJSON(data []byte) error {
    var raw map[string]any
    json.Unmarshal(data, &raw)

    // 빈 문자열 시간 필드를 nil로 변환 (하위 호환성)
    for _, field := range []string{"first_deployed", "last_deployed", "deleted"} {
        if val, ok := raw[field]; ok {
            if str, ok := val.(string); ok && str == "" {
                raw[field] = nil
            }
        }
    }
    // ...
}

func (i Info) MarshalJSON() ([]byte, error) {
    tmp := infoJSON{
        Description: i.Description,
        Status:      i.Status,
        Notes:       i.Notes,
        Resources:   i.Resources,
    }
    // zero time은 omit
    if !i.FirstDeployed.IsZero() {
        tmp.FirstDeployed = &i.FirstDeployed
    }
    // ...
    return json.Marshal(tmp)
}
```

**왜 커스텀 JSON 마샬링이 필요한가?**
- 이전 Helm 버전에서 시간 필드가 빈 문자열(`""`)로 저장된 경우가 있음
- Go의 `time.Time`은 빈 문자열을 파싱할 수 없어 에러 발생
- 커스텀 Unmarshaler가 빈 문자열을 nil로 변환하여 하위 호환성 확보
- Marshaler는 zero time을 생략하여 JSON 크기 절약

### 3.3 Status 타입과 상수

```go
// pkg/release/common/status.go

type Status string

const (
    StatusUnknown         Status = "unknown"
    StatusDeployed        Status = "deployed"
    StatusUninstalled     Status = "uninstalled"
    StatusSuperseded      Status = "superseded"
    StatusFailed          Status = "failed"
    StatusUninstalling    Status = "uninstalling"
    StatusPendingInstall  Status = "pending-install"
    StatusPendingUpgrade  Status = "pending-upgrade"
    StatusPendingRollback Status = "pending-rollback"
)

func (x Status) String() string { return string(x) }

func (x Status) IsPending() bool {
    return x == StatusPendingInstall ||
           x == StatusPendingUpgrade ||
           x == StatusPendingRollback
}
```

### 3.4 상태 분류

```
┌─────────────────────────────────────────┐
│           Release 상태 분류               │
├──────────────┬──────────────────────────┤
│    안정 상태   │  전이 상태                │
├──────────────┼──────────────────────────┤
│  deployed    │  pending-install         │
│  uninstalled │  pending-upgrade         │
│  superseded  │  pending-rollback        │
│  failed      │  uninstalling            │
│  unknown     │                          │
└──────────────┴──────────────────────────┘
```

| 상태 | 의미 | 전환 조건 |
|------|------|-----------|
| `unknown` | 불확실한 상태 | 비정상 종료 시 |
| `deployed` | 클러스터에 배포됨 | install/upgrade 성공 |
| `uninstalled` | 삭제됨 (이력 보존) | uninstall --keep-history |
| `superseded` | 새 리비전으로 대체됨 | 업그레이드 시 이전 리비전 |
| `failed` | 배포 실패 | install/upgrade 실패 |
| `pending-install` | 설치 진행 중 | install 시작 |
| `pending-upgrade` | 업그레이드 진행 중 | upgrade 시작 |
| `pending-rollback` | 롤백 진행 중 | rollback 시작 |
| `uninstalling` | 삭제 진행 중 | uninstall 시작 |

---

## 4. Hook 시스템

### 4.1 Hook 구조체

```go
// pkg/release/v1/hook.go

type Hook struct {
    Name              string                `json:"name,omitempty"`
    Kind              string                `json:"kind,omitempty"`
    Path              string                `json:"path,omitempty"`
    Manifest          string                `json:"manifest,omitempty"`
    Events            []HookEvent           `json:"events,omitempty"`
    LastRun           HookExecution         `json:"last_run"`
    Weight            int                   `json:"weight,omitempty"`
    DeletePolicies    []HookDeletePolicy    `json:"delete_policies,omitempty"`
    OutputLogPolicies []HookOutputLogPolicy `json:"output_log_policies,omitempty"`
}
```

| 필드 | 설명 |
|------|------|
| `Name` | Kubernetes 리소스 이름 (metadata.name) |
| `Kind` | Kubernetes 리소스 종류 (Job, Pod 등) |
| `Path` | 차트 내 템플릿 파일 경로 |
| `Manifest` | 렌더링된 YAML 매니페스트 |
| `Events` | 이 Hook이 실행되는 이벤트 목록 |
| `LastRun` | 마지막 실행 정보 |
| `Weight` | 실행 순서 가중치 (낮은 것이 먼저) |
| `DeletePolicies` | Hook 리소스 삭제 정책 |
| `OutputLogPolicies` | Hook 로그 출력 정책 (v4 신규) |

### 4.2 Hook 이벤트 타입

```go
// pkg/release/v1/hook.go

type HookEvent string

const (
    HookPreInstall   HookEvent = "pre-install"
    HookPostInstall  HookEvent = "post-install"
    HookPreDelete    HookEvent = "pre-delete"
    HookPostDelete   HookEvent = "post-delete"
    HookPreUpgrade   HookEvent = "pre-upgrade"
    HookPostUpgrade  HookEvent = "post-upgrade"
    HookPreRollback  HookEvent = "pre-rollback"
    HookPostRollback HookEvent = "post-rollback"
    HookTest         HookEvent = "test"
)
```

### 4.3 Hook 라이프사이클 다이어그램

```
helm install:
  ┌──────────────┐   ┌──────────────┐   ┌───────────────┐
  │ pre-install   │──▶│ 리소스 생성    │──▶│ post-install   │
  │ Hook 실행     │   │              │   │ Hook 실행      │
  └──────────────┘   └──────────────┘   └───────────────┘

helm upgrade:
  ┌──────────────┐   ┌──────────────┐   ┌───────────────┐
  │ pre-upgrade   │──▶│ 리소스 업데이트 │──▶│ post-upgrade   │
  │ Hook 실행     │   │              │   │ Hook 실행      │
  └──────────────┘   └──────────────┘   └───────────────┘

helm rollback:
  ┌──────────────┐   ┌──────────────┐   ┌────────────────┐
  │ pre-rollback  │──▶│ 리소스 복원    │──▶│ post-rollback   │
  │ Hook 실행     │   │              │   │ Hook 실행       │
  └──────────────┘   └──────────────┘   └────────────────┘

helm uninstall:
  ┌──────────────┐   ┌──────────────┐   ┌───────────────┐
  │ pre-delete    │──▶│ 리소스 삭제    │──▶│ post-delete    │
  │ Hook 실행     │   │              │   │ Hook 실행      │
  └──────────────┘   └──────────────┘   └───────────────┘

helm test:
  ┌──────────────┐
  │ test          │
  │ Hook 실행     │
  └──────────────┘
```

### 4.4 Hook 삭제 정책

```go
// pkg/release/v1/hook.go

type HookDeletePolicy string

const (
    HookSucceeded          HookDeletePolicy = "hook-succeeded"
    HookFailed             HookDeletePolicy = "hook-failed"
    HookBeforeHookCreation HookDeletePolicy = "before-hook-creation"
)
```

| 정책 | 의미 |
|------|------|
| `hook-succeeded` | Hook 성공 시 리소스 삭제 |
| `hook-failed` | Hook 실패 시 리소스 삭제 |
| `before-hook-creation` | 다음 실행 전에 기존 Hook 리소스 삭제 |

### 4.5 Hook 로그 출력 정책 (v4 신규)

```go
// pkg/release/v1/hook.go

type HookOutputLogPolicy string

const (
    HookOutputOnSucceeded HookOutputLogPolicy = "hook-succeeded"
    HookOutputOnFailed    HookOutputLogPolicy = "hook-failed"
)
```

이 정책은 Pod/Job Hook의 로그를 메인 프로세스로 복사할지 결정한다.

### 4.6 Hook 어노테이션

```go
// pkg/release/v1/hook.go

const HookAnnotation       = "helm.sh/hook"
const HookWeightAnnotation = "helm.sh/hook-weight"
const HookDeleteAnnotation = "helm.sh/hook-delete-policy"
const HookOutputLogAnnotation = "helm.sh/hook-output-log-policy"
```

**Hook YAML 예시:**

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: db-migrate
  annotations:
    helm.sh/hook: pre-upgrade
    helm.sh/hook-weight: "-5"
    helm.sh/hook-delete-policy: before-hook-creation
    helm.sh/hook-output-log-policy: hook-succeeded,hook-failed
spec:
  template:
    spec:
      containers:
      - name: migrate
        image: myapp/migrate:latest
      restartPolicy: Never
```

### 4.7 HookExecution - 실행 추적

```go
// pkg/release/v1/hook.go

type HookExecution struct {
    StartedAt   time.Time `json:"started_at,omitzero"`
    CompletedAt time.Time `json:"completed_at,omitzero"`
    Phase       HookPhase `json:"phase"`
}

type HookPhase string

const (
    HookPhaseUnknown   HookPhase = "Unknown"
    HookPhaseRunning   HookPhase = "Running"
    HookPhaseSucceeded HookPhase = "Succeeded"
    HookPhaseFailed    HookPhase = "Failed"
)
```

---

## 5. Accessor 패턴

### 5.1 Release Accessor 인터페이스

```go
// pkg/release/interfaces.go

type Releaser any
type Hook any

type Accessor interface {
    Name() string
    Namespace() string
    Version() int
    Hooks() []Hook
    Manifest() string
    Notes() string
    Labels() map[string]string
    Chart() chart.Charter
    Status() string
    ApplyMethod() string
    DeployedAt() time.Time
}

type HookAccessor interface {
    Path() string
    Manifest() string
}
```

### 5.2 NewAccessor 팩토리

```go
// pkg/release/common.go

var NewAccessor func(rel Releaser) (Accessor, error) = newDefaultAccessor

func newDefaultAccessor(rel Releaser) (Accessor, error) {
    switch v := rel.(type) {
    case v1release.Release:
        return &v1Accessor{&v}, nil
    case *v1release.Release:
        return &v1Accessor{v}, nil
    case v2release.Release:
        return &v2Accessor{&v}, nil
    case *v2release.Release:
        return &v2Accessor{v}, nil
    default:
        return nil, fmt.Errorf("unsupported release type: %T", rel)
    }
}
```

**왜 Release에도 Accessor 패턴을 사용하는가?**
- Chart와 동일한 이유: v1(현행)과 v2(내부 신규) Release 포맷을 통일
- Storage 계층이 Release 버전에 독립적으로 동작 가능
- 테스트에서 NewAccessor를 교체하여 mock 주입 가능

### 5.3 v1Accessor 구현

```go
// pkg/release/common.go

type v1Accessor struct {
    rel *v1release.Release
}

func (a *v1Accessor) Name() string           { return a.rel.Name }
func (a *v1Accessor) Namespace() string      { return a.rel.Namespace }
func (a *v1Accessor) Version() int           { return a.rel.Version }
func (a *v1Accessor) Manifest() string       { return a.rel.Manifest }
func (a *v1Accessor) Notes() string          { return a.rel.Info.Notes }
func (a *v1Accessor) Labels() map[string]string { return a.rel.Labels }
func (a *v1Accessor) Chart() chart.Charter   { return a.rel.Chart }
func (a *v1Accessor) Status() string         { return a.rel.Info.Status.String() }
func (a *v1Accessor) ApplyMethod() string    { return a.rel.ApplyMethod }
func (a *v1Accessor) DeployedAt() time.Time  { return a.rel.Info.LastDeployed }

func (a *v1Accessor) Hooks() []Hook {
    var hooks = make([]Hook, len(a.rel.Hooks))
    for i, h := range a.rel.Hooks {
        hooks[i] = h
    }
    return hooks
}
```

### 5.4 UninstallReleaseResponse

```go
// pkg/release/responses.go

type UninstallReleaseResponse struct {
    Release Releaser `json:"release,omitempty"`
    Info    string   `json:"info,omitempty"`
}
```

---

## 6. 매니페스트 분류와 정렬

### 6.1 SplitManifests - YAML 스트림 분리

```go
// pkg/release/v1/util/manifest.go

type SimpleHead struct {
    Version  string `json:"apiVersion"`
    Kind     string `json:"kind,omitempty"`
    Metadata *struct {
        Name        string            `json:"name"`
        Annotations map[string]string `json:"annotations"`
    } `json:"metadata,omitempty"`
}

var sep = regexp.MustCompile("(?:^|\\s*\n)---\\s*")

func SplitManifests(bigFile string) map[string]string {
    tpl := "manifest-%d"
    res := map[string]string{}
    bigFileTmp := strings.TrimSpace(bigFile)
    docs := sep.Split(bigFileTmp, -1)
    var count int
    for _, d := range docs {
        if d == "" { continue }
        d = strings.TrimSpace(d)
        res[fmt.Sprintf(tpl, count)] = d
        count++
    }
    return res
}
```

### 6.2 SortManifests - Hook과 매니페스트 분류

```go
// pkg/release/v1/util/manifest_sorter.go

type Manifest struct {
    Name    string
    Content string
    Head    *SimpleHead
}

func SortManifests(files map[string]string, _ common.VersionSet, ordering KindSortOrder) (
    []*release.Hook, []Manifest, error) {

    result := &result{}

    // 1. 파일 경로 정렬
    var sortedFilePaths []string
    for filePath := range files {
        sortedFilePaths = append(sortedFilePaths, filePath)
    }
    sort.Strings(sortedFilePaths)

    // 2. 각 파일의 매니페스트를 분류
    for _, filePath := range sortedFilePaths {
        content := files[filePath]
        if strings.HasPrefix(path.Base(filePath), "_") { continue }  // 파셜 건너뛰기
        if strings.TrimSpace(content) == "" { continue }              // 빈 파일 건너뛰기

        manifestFile := &manifestFile{
            entries: SplitManifests(content),
            path:    filePath,
        }
        manifestFile.sort(result)
    }

    // 3. Kind 기준 정렬
    return sortHooksByKind(result.hooks, ordering),
           sortManifestsByKind(result.generic, ordering), nil
}
```

### 6.3 Hook 분류 로직

```go
// pkg/release/v1/util/manifest_sorter.go

func (file *manifestFile) sort(result *result) error {
    for _, entryKey := range sortedEntryKeys {
        m := file.entries[entryKey]
        var entry SimpleHead
        yaml.Unmarshal([]byte(m), &entry)

        // 어노테이션이 없으면 일반 매니페스트
        if !hasAnyAnnotation(entry) {
            result.generic = append(result.generic, Manifest{...})
            continue
        }

        // helm.sh/hook 어노테이션이 없으면 일반 매니페스트
        hookTypes, ok := entry.Metadata.Annotations[release.HookAnnotation]
        if !ok {
            result.generic = append(result.generic, Manifest{...})
            continue
        }

        // Hook 생성
        hw := calculateHookWeight(entry)
        h := &release.Hook{
            Name:     entry.Metadata.Name,
            Kind:     entry.Kind,
            Path:     file.path,
            Manifest: m,
            Weight:   hw,
            // ...
        }

        // 이벤트 파싱
        for hookType := range strings.SplitSeq(hookTypes, ",") {
            hookType = strings.ToLower(strings.TrimSpace(hookType))
            e, ok := events[hookType]
            if !ok {
                isUnknownHook = true
                break
            }
            h.Events = append(h.Events, e)
        }

        // 삭제 정책 파싱
        operateAnnotationValues(entry, release.HookDeleteAnnotation, func(value string) {
            h.DeletePolicies = append(h.DeletePolicies, release.HookDeletePolicy(value))
        })

        // 로그 출력 정책 파싱
        operateAnnotationValues(entry, release.HookOutputLogAnnotation, func(value string) {
            h.OutputLogPolicies = append(h.OutputLogPolicies, release.HookOutputLogPolicy(value))
        })

        result.hooks = append(result.hooks, h)
    }
    return nil
}
```

### 6.4 Hook 가중치 계산

```go
// pkg/release/v1/util/manifest_sorter.go

func calculateHookWeight(entry SimpleHead) int {
    hws := entry.Metadata.Annotations[release.HookWeightAnnotation]
    hw, err := strconv.Atoi(hws)
    if err != nil {
        hw = 0  // 파싱 실패 시 기본값 0
    }
    return hw
}
```

---

## 7. Release 필터링과 정렬

### 7.1 Kind 기반 설치/삭제 순서

```go
// pkg/release/v1/util/kind_sorter.go

var InstallOrder KindSortOrder = []string{
    "PriorityClass",
    "Namespace",
    "NetworkPolicy",
    "ResourceQuota",
    "LimitRange",
    "PodSecurityPolicy",
    "PodDisruptionBudget",
    "ServiceAccount",
    "Secret",
    "SecretList",
    "ConfigMap",
    "StorageClass",
    "PersistentVolume",
    "PersistentVolumeClaim",
    "CustomResourceDefinition",
    "ClusterRole",
    "ClusterRoleList",
    "ClusterRoleBinding",
    "ClusterRoleBindingList",
    "Role",
    "RoleList",
    "RoleBinding",
    "RoleBindingList",
    "Service",
    "DaemonSet",
    "Pod",
    "ReplicationController",
    "ReplicaSet",
    "Deployment",
    "HorizontalPodAutoscaler",
    "StatefulSet",
    "Job",
    "CronJob",
    "IngressClass",
    "Ingress",
    "APIService",
    "MutatingWebhookConfiguration",
    "ValidatingWebhookConfiguration",
}
```

**왜 이 순서인가?**

| 순서 | Kind | 이유 |
|------|------|------|
| 1 | Namespace | 다른 리소스가 속할 네임스페이스 먼저 |
| 2-3 | NetworkPolicy, ResourceQuota | 정책 먼저 설정 |
| 4 | ServiceAccount | Pod가 사용할 SA 먼저 |
| 5-6 | Secret, ConfigMap | Pod가 참조할 설정 먼저 |
| 7 | StorageClass | PV/PVC가 참조 |
| 8-9 | PV, PVC | 워크로드가 마운트할 볼륨 먼저 |
| 10 | CRD | CR 생성 전에 정의 필요 |
| 11-14 | RBAC | 워크로드 실행 권한 먼저 |
| 15 | Service | 워크로드 접근점 먼저 |
| 16-22 | 워크로드 | 마지막에 배포 |
| 23-25 | Ingress/Webhook | 워크로드 준비 후 트래픽 연결 |

**UninstallOrder는 정확히 역순:**

```go
var UninstallOrder KindSortOrder = []string{
    "ValidatingWebhookConfiguration",  // Webhook 먼저 제거 (삭제 방해 방지)
    "MutatingWebhookConfiguration",
    // ... 설치 역순
    "Namespace",
    "PriorityClass",
}
```

### 7.2 FilterFunc 패턴

```go
// pkg/release/v1/util/filter.go

type FilterFunc func(*rspb.Release) bool

func (fn FilterFunc) Check(rls *rspb.Release) bool {
    if rls == nil { return false }
    return fn(rls)
}

func (fn FilterFunc) Filter(rels []*rspb.Release) (rets []*rspb.Release) {
    for _, rel := range rels {
        if fn.Check(rel) {
            rets = append(rets, rel)
        }
    }
    return
}

// 조합: OR 로직
func Any(filters ...FilterFunc) FilterFunc {
    return func(rls *rspb.Release) bool {
        for _, filter := range filters {
            if filter(rls) { return true }
        }
        return false
    }
}

// 조합: AND 로직
func All(filters ...FilterFunc) FilterFunc {
    return func(rls *rspb.Release) bool {
        for _, filter := range filters {
            if !filter(rls) { return false }
        }
        return true
    }
}

// 상태 기반 필터
func StatusFilter(status common.Status) FilterFunc {
    return FilterFunc(func(rls *rspb.Release) bool {
        if rls == nil { return true }
        return rls.Info.Status == status
    })
}
```

**사용 예시:**
```go
// deployed 또는 failed 상태의 릴리스 필터링
filter := Any(
    StatusFilter(common.StatusDeployed),
    StatusFilter(common.StatusFailed),
)
filtered := filter.Filter(releases)
```

### 7.3 정렬 함수

```go
// pkg/release/v1/util/sorter.go

func SortByName(list []*rspb.Release) {
    sort.Slice(list, func(i, j int) bool {
        return list[i].Name < list[j].Name
    })
}

func SortByDate(list []*rspb.Release) {
    sort.Slice(list, func(i, j int) bool {
        ti := list[i].Info.LastDeployed.Unix()
        tj := list[j].Info.LastDeployed.Unix()
        if ti != tj {
            return ti < tj
        }
        return list[i].Name < list[j].Name  // 동률 시 이름으로
    })
}

func SortByRevision(list []*rspb.Release) {
    sort.Slice(list, func(i, j int) bool {
        return list[i].Version < list[j].Version
    })
}

// 역순 유틸리티
func Reverse(list []*rspb.Release, sortFn func([]*rspb.Release)) {
    sortFn(list)
    for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
        list[i], list[j] = list[j], list[i]
    }
}
```

---

## 8. 라이프사이클 상태 전이

### 8.1 Install 흐름

```
          ┌─────────────────┐
          │ helm install     │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ pending-install  │  Release v1 생성
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ pre-install Hook │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ 리소스 생성       │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ post-install Hook│
          └───┬──────────┬──┘
              │          │
     ┌────────▼──┐   ┌──▼────────┐
     │ deployed  │   │ failed    │
     │ (성공)     │   │ (실패)    │
     └───────────┘   └───────────┘
```

### 8.2 Upgrade 흐름

```
          ┌─────────────────┐
          │ helm upgrade     │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │  이전 릴리스:     │
          │  superseded     │  이전 v(n) 상태 변경
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ pending-upgrade  │  새 v(n+1) 생성
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ pre-upgrade Hook │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ 리소스 업데이트    │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ post-upgrade Hook│
          └───┬──────────┬──┘
              │          │
     ┌────────▼──┐   ┌──▼────────┐
     │ deployed  │   │ failed    │
     └───────────┘   └───────────┘
```

### 8.3 Rollback 흐름

```
          ┌─────────────────┐
          │ helm rollback    │  지정 리비전으로 복원
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │  현재 릴리스:     │
          │  superseded     │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ pending-rollback │  새 v(n+1) 생성 (지정 리비전 내용 복사)
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ pre-rollback Hook│
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ 리소스 복원       │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ post-rollback    │
          └───┬──────────┬──┘
              │          │
     ┌────────▼──┐   ┌──▼────────┐
     │ deployed  │   │ failed    │
     └───────────┘   └───────────┘
```

### 8.4 Uninstall 흐름

```
          ┌─────────────────┐
          │ helm uninstall   │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ uninstalling     │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ pre-delete Hook  │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ 리소스 삭제       │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │ post-delete Hook │
          └────────┬────────┘
                   │
        ┌──────────┴──────────┐
        │                     │
  ┌─────▼──────┐    ┌────────▼───────┐
  │ 이력 삭제   │    │ --keep-history │
  │ (기본)     │    │ uninstalled    │
  └────────────┘    └────────────────┘
```

### 8.5 리비전 히스토리 예시

```
Release "my-app" 히스토리:

Version │ Status      │ Description
────────┼─────────────┼──────────────────
   1    │ superseded  │ Install complete
   2    │ superseded  │ Upgrade to v2.0
   3    │ failed      │ Upgrade failed
   4    │ deployed    │ Rollback to v2 ← 현재
```

---

## 9. ApplyMethod: CSA vs SSA

### 9.1 정의

```go
// pkg/release/v1/release.go

type ApplyMethod string

const ApplyMethodClientSideApply ApplyMethod = "csa"
const ApplyMethodServerSideApply ApplyMethod = "ssa"
```

| 방식 | 값 | 동작 |
|------|-----|------|
| Client-Side Apply (CSA) | `"csa"` | 클라이언트가 리소스 diff 계산 후 patch 전송 |
| Server-Side Apply (SSA) | `"ssa"` | 서버가 필드 소유권 기반으로 merge |
| 미설정 | `""` | CSA로 취급 (하위 호환성) |

**왜 SSA가 추가되었는가?**
- CSA는 `kubectl.kubernetes.io/last-applied-configuration` 어노테이션에 의존
- SSA는 Kubernetes API 서버가 필드 소유권을 관리하여 충돌 방지
- 여러 관리 도구가 같은 리소스를 관리할 때 SSA가 안전
- v4에서는 SSA를 기본으로 전환하려는 방향

---

## 10. 설계 결정과 Why 분석

### 10.1 왜 Release에 차트 전체를 저장하는가?

`Release.Chart` 필드에는 배포에 사용된 차트 전체가 저장된다.
이는 저장 공간을 많이 차지하지만 다음과 같은 이점이 있다:

- 롤백 시 이전 차트 버전으로 정확하게 복원 가능
- `helm get manifest`, `helm get values` 등이 스토리지만으로 동작
- 차트 레지스트리가 불가용해도 이력 조회 가능
- `helm diff` 같은 도구가 정확한 비교 수행 가능

### 10.2 왜 Config와 Values가 분리되어 있는가?

- `Chart.Values`: 차트에 내장된 기본값 (`values.yaml`)
- `Release.Config`: 사용자가 `--set` 또는 `-f`로 제공한 오버라이드

이 분리로 `helm get values --all`은 병합된 전체 값을,
`helm get values`는 사용자 오버라이드만 보여줄 수 있다.

### 10.3 왜 Hook을 매니페스트와 분리하는가?

- Hook은 릴리스의 주요 리소스와 다른 라이프사이클을 가짐
- Hook은 특정 이벤트에서만 실행되고, 삭제 정책에 따라 관리됨
- `helm get manifest`에서 Hook은 포함되지 않아야 함
- Hook 실행 순서(가중치)를 별도로 관리해야 함

### 10.4 왜 InstallOrder/UninstallOrder가 하드코딩되어 있는가?

동적으로 의존성을 분석하여 순서를 결정하는 것은:
- CRD와 CR 사이의 관계를 파악해야 하므로 복잡
- 사용자 정의 리소스의 의존 관계를 알 수 없음
- 런타임 비용이 높음

대신 Kubernetes 리소스의 일반적인 의존 관계를 하드코딩하여:
- 예측 가능한 동작 보장
- 대부분의 사용 사례를 커버
- 알 수 없는 Kind는 맨 뒤에 정렬

### 10.5 왜 FilterFunc이 함수 타입인가?

```go
type FilterFunc func(*rspb.Release) bool
```

Go에서는 인터페이스 대신 함수 타입을 사용하면:
- 람다(클로저)로 인라인 필터 작성 가능
- `Any()`, `All()` 같은 조합자 함수로 복합 필터 구성
- 단일 메서드 인터페이스보다 간결
- 함수형 프로그래밍 스타일로 체이닝 가능

### 10.6 왜 time.Time에 커스텀 JSON 처리가 필요한가?

Helm 2에서 protobuf를 사용하던 시절, 시간 필드가 빈 문자열로 저장되는 경우가 있었다.
Helm 3에서 JSON으로 전환하면서 이런 레거시 데이터와의 호환성이 필요해졌다.
Go의 `time.Time`은 빈 문자열을 파싱하면 에러가 발생하므로,
커스텀 Unmarshaler에서 이를 nil/zero value로 처리한다.

---

## 참고: 핵심 소스 파일 경로

| 파일 | 경로 |
|------|------|
| Release 구조체 | `pkg/release/v1/release.go` |
| Info 구조체 | `pkg/release/v1/info.go` |
| Hook 구조체/이벤트 | `pkg/release/v1/hook.go` |
| Status 상수 | `pkg/release/common/status.go` |
| Release Accessor | `pkg/release/interfaces.go`, `pkg/release/common.go` |
| FilterFunc | `pkg/release/v1/util/filter.go` |
| 정렬 함수 | `pkg/release/v1/util/sorter.go` |
| 매니페스트 분리 | `pkg/release/v1/util/manifest.go` |
| 매니페스트 정렬 | `pkg/release/v1/util/manifest_sorter.go` |
| Kind 순서 | `pkg/release/v1/util/kind_sorter.go` |
| 응답 구조체 | `pkg/release/responses.go` |
