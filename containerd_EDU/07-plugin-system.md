# 07. containerd 플러그인 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [플러그인 시스템이 필요한 이유](#2-플러그인-시스템이-필요한-이유)
3. [핵심 데이터 구조](#3-핵심-데이터-구조)
4. [Registration 구조체 상세](#4-registration-구조체-상세)
5. [Graph() DFS 의존성 해석 알고리즘](#5-graph-dfs-의존성-해석-알고리즘)
6. [InitContext와 플러그인 조회 메커니즘](#6-initcontext와-플러그인-조회-메커니즘)
7. [Plugin Set: 초기화된 플러그인 관리](#7-plugin-set-초기화된-플러그인-관리)
8. [플러그인 타입 체계](#8-플러그인-타입-체계)
9. [전역 Registry와 Register 패턴](#9-전역-registry와-register-패턴)
10. [실제 플러그인 등록 예시: Overlay Snapshotter](#10-실제-플러그인-등록-예시-overlay-snapshotter)
11. [Shim에서의 플러그인 로딩 흐름](#11-shim에서의-플러그인-로딩-흐름)
12. [Meta: Capabilities와 Exports](#12-meta-capabilities와-exports)
13. [에러 처리와 Skip 메커니즘](#13-에러-처리와-skip-메커니즘)
14. [설계 철학과 트레이드오프](#14-설계-철학과-트레이드오프)

---

## 1. 개요

containerd의 플러그인 시스템은 모든 기능을 **교체 가능한 컴포넌트**로 구성하는 핵심 아키텍처 패턴이다. Snapshotter, Content Store, Runtime, gRPC 서비스, 이벤트, GC, CRI 등 containerd의 거의 모든 서브시스템이 플러그인으로 구현되어 있다.

```
소스 위치:
  vendor/github.com/containerd/plugin/plugin.go      -- Registration, Registry, Graph()
  vendor/github.com/containerd/plugin/context.go      -- InitContext, Meta, Plugin, Set
  vendor/github.com/containerd/plugin/registry/register.go -- 전역 등록
  plugins/types.go                                     -- 플러그인 타입 상수 정의
  plugins/snapshots/overlay/plugin/plugin.go           -- 실제 등록 예시
```

## 2. 플러그인 시스템이 필요한 이유

### 왜 하드코딩이 아닌 플러그인인가?

containerd는 다양한 환경에서 동작해야 한다:

| 환경 | Snapshotter | Runtime | 특이사항 |
|------|-------------|---------|---------|
| 표준 Linux | overlayfs | runc v2 | 가장 일반적 |
| AWS Firecracker | devmapper | firecracker | microVM |
| 임베디드 | native | runc v2 | overlayfs 미지원 |
| Windows | windows | runhcs | 완전히 다른 스택 |
| macOS (개발) | native | - | 제한적 지원 |

하드코딩하면 이 모든 조합을 if/else로 관리해야 한다. 플러그인 시스템은 이를 **선언적 등록 + 의존성 기반 초기화**로 해결한다.

### 핵심 설계 원칙

```
+-------------------------------------------------------------------+
|                        containerd daemon                           |
|                                                                    |
|  +----------+  +----------+  +----------+  +----------+           |
|  | Snapshot  |  | Content  |  | Runtime  |  |   gRPC   |           |
|  | Plugin    |  | Plugin   |  | Plugin   |  |  Plugin  |           |
|  +----+-----+  +----+-----+  +----+-----+  +----+-----+           |
|       |              |              |              |                |
|  +----v--------------v--------------v--------------v-----+         |
|  |              Plugin Registry (Graph)                   |         |
|  |   Registration -> 의존성 해석 -> 순서 결정 -> Init     |         |
|  +--------------------------------------------------------+         |
+-------------------------------------------------------------------+
```

1. **등록은 init()에서** -- Go의 패키지 초기화 메커니즘 활용
2. **의존성은 Type 기반** -- 특정 인스턴스가 아닌 타입에 의존
3. **초기화는 Graph 순서** -- DFS로 의존성 순서 보장
4. **실패는 격리** -- ErrSkipPlugin으로 선택적 로딩

---

## 3. 핵심 데이터 구조

### 전체 타입 관계도

```
Registry ([]*Registration)
    |
    +-- Graph(filter) --> []Registration (정렬됨)
    |
    +-- Register(r) --> Registry (새 슬라이스)

Registration
    |
    +-- Type      plugin.Type ("io.containerd.snapshotter.v1" 등)
    +-- ID        string      ("overlayfs" 등)
    +-- Config    interface{} (TOML 설정 역직렬화 대상)
    +-- Requires  []Type      (의존하는 플러그인 타입들)
    +-- InitFn    func(*InitContext) (interface{}, error)
    +-- ConfigMigration  func(ctx, version, map) error
    |
    +-- Init(ic) --> *Plugin
    +-- URI()    --> "Type.ID"

InitContext
    |
    +-- Context    context.Context
    +-- Properties map[string]string (RootDir, StateDir 등)
    +-- Config     interface{}
    +-- Meta       *Meta
    +-- plugins    *Set
    |
    +-- GetSingle(Type) --> (interface{}, error)
    +-- GetByType(Type) --> (map[string]interface{}, error)
    +-- GetByID(Type, ID) --> (interface{}, error)

Plugin
    |
    +-- Registration  Registration
    +-- Config        interface{}
    +-- Meta          Meta
    +-- instance      interface{}  (InitFn의 반환값)
    +-- err           error
    |
    +-- Instance() --> (interface{}, error)
    +-- Err()      --> error

Set
    |
    +-- ordered      []*Plugin              (초기화 순서)
    +-- byTypeAndID  map[Type]map[string]*Plugin
    |
    +-- Add(p) / Get(t, id) / GetAll()

Meta
    +-- Platforms    []Platform  (지원 플랫폼)
    +-- Exports      map[string]string (내보내는 값)
    +-- Capabilities []string  (기능 플래그)
```

---

## 4. Registration 구조체 상세

`Registration`은 플러그인의 "설계도"이다. 아직 초기화되지 않은 상태의 플러그인을 기술한다.

```go
// 소스: vendor/github.com/containerd/plugin/plugin.go:58-82

type Registration struct {
    Type    Type            // 플러그인 카테고리
    ID      string          // 카테고리 내 고유 식별자
    Config  interface{}     // 설정 구조체 포인터
    Requires []Type         // 의존하는 플러그인 타입들
    InitFn  func(*InitContext) (interface{}, error)  // 초기화 함수
    ConfigMigration func(context.Context, int, map[string]interface{}) error
}
```

### 각 필드의 역할

**Type과 ID: URI 기반 식별**

```go
// 소스: vendor/github.com/containerd/plugin/plugin.go:97-99
func (r *Registration) URI() string {
    return r.Type.String() + "." + r.ID
}
// 예: "io.containerd.snapshotter.v1.overlayfs"
```

URI는 `Type.ID` 형태로, 전체 Registry 내에서 고유해야 한다. 중복 URI 등록 시 `checkUnique()`에서 `ErrIDRegistered` 에러와 함께 panic이 발생한다.

**Requires: 타입 기반 의존성**

```go
Requires: []Type{plugins.MetadataPlugin, plugins.ContentPlugin}
```

개별 인스턴스가 아닌 **타입**에 의존한다는 것이 핵심이다. 이는 동일 타입의 플러그인 구현체를 교체해도 의존 관계가 깨지지 않음을 의미한다. 특수 값 `"*"`는 "모든 플러그인에 의존"을 의미하며, 이때 Requires 슬라이스에는 `"*"` 하나만 있어야 한다.

**InitFn: 지연 초기화**

```go
InitFn: func(ic *plugin.InitContext) (interface{}, error) {
    // 1. Config 읽기
    config := ic.Config.(*MyConfig)
    // 2. 의존 플러그인 조회
    dep, err := ic.GetSingle(plugins.ContentPlugin)
    // 3. 인스턴스 생성 후 반환
    return NewMyPlugin(config, dep), nil
}
```

InitFn은 Registration이 아닌 **초기화 시점**에 호출된다. 이 "지연 초기화" 덕분에:
- 등록 순서와 초기화 순서를 분리할 수 있다
- 의존 플러그인이 이미 초기화된 상태에서 접근할 수 있다
- 환경에 따라 초기화를 건너뛸 수 있다 (ErrSkipPlugin)

**Init: Registration을 Plugin으로 변환**

```go
// 소스: vendor/github.com/containerd/plugin/plugin.go:85-94
func (r Registration) Init(ic *InitContext) *Plugin {
    p, err := r.InitFn(ic)
    return &Plugin{
        Registration: r,
        Config:       ic.Config,
        Meta:         *ic.Meta,
        instance:     p,
        err:          err,
    }
}
```

주목할 점: InitFn이 에러를 반환해도 `*Plugin`은 생성된다. 에러는 Plugin.err에 저장되고, `Instance()` 호출 시 함께 반환된다. 이로써 초기화 실패한 플러그인도 Set에 기록되어 디버깅이 가능하다.

---

## 5. Graph() DFS 의존성 해석 알고리즘

Graph()는 등록된 플러그인들을 **의존성 순서대로 정렬**하는 핵심 알고리즘이다.

### 알고리즘 코드

```go
// 소스: vendor/github.com/containerd/plugin/plugin.go:112-133

func (registry Registry) Graph(filter DisableFilter) []Registration {
    disabled := map[*Registration]bool{}
    for _, r := range registry {
        if filter(r) {
            disabled[r] = true
        }
    }

    ordered := make([]Registration, 0, len(registry)-len(disabled))
    added := map[*Registration]bool{}
    for _, r := range registry {
        if disabled[r] {
            continue
        }
        children(r, registry, added, disabled, &ordered)
        if !added[r] {
            ordered = append(ordered, *r)
            added[r] = true
        }
    }
    return ordered
}
```

### children() 재귀 함수

```go
// 소스: vendor/github.com/containerd/plugin/plugin.go:135-147

func children(reg *Registration, registry []*Registration, added, disabled map[*Registration]bool, ordered *[]Registration) {
    for _, t := range reg.Requires {
        for _, r := range registry {
            if !disabled[r] && r.URI() != reg.URI() && (t == "*" || r.Type == t) {
                children(r, registry, added, disabled, ordered)
                if !added[r] {
                    *ordered = append(*ordered, *r)
                    added[r] = true
                }
            }
        }
    }
}
```

### 알고리즘 동작 원리

```
예시: 4개 플러그인의 의존성

  ContentPlugin (C)  -- requires: 없음
  MetadataPlugin (M) -- requires: [ContentPlugin]
  SnapshotPlugin (S) -- requires: 없음
  ServicePlugin (V)  -- requires: [MetadataPlugin, SnapshotPlugin]

단계별 처리:

1. registry를 순회, 첫 번째 r = C
   children(C, ...) → C.Requires 비어있음 → 재귀 없음
   C 미추가 → ordered에 C 추가
   ordered = [C]

2. 두 번째 r = M
   children(M, ...) → M.Requires = [ContentPlugin]
     → registry에서 Type == ContentPlugin인 것: C
     → children(C, ...) → 재귀 없음
     → C 이미 추가됨 → skip
   M 미추가 → ordered에 M 추가
   ordered = [C, M]

3. 세 번째 r = S
   children(S, ...) → S.Requires 비어있음
   S 미추가 → ordered에 S 추가
   ordered = [C, M, S]

4. 네 번째 r = V
   children(V, ...) → V.Requires = [MetadataPlugin, SnapshotPlugin]
     → MetadataPlugin: M
       → children(M, ...) → C 이미 추가 → M 이미 추가
     → SnapshotPlugin: S
       → children(S, ...) → S 이미 추가
   V 미추가 → ordered에 V 추가
   ordered = [C, M, S, V]

결과: Content → Metadata → Snapshot → Service (의존성 순서 보장)
```

### 왜 DFS인가?

위상 정렬(Topological Sort)의 한 형태인 DFS 기반 접근법을 사용하는 이유:
1. **단순성** -- Kahn 알고리즘보다 구현이 간결
2. **added 맵으로 중복 방지** -- 동일 플러그인이 여러 경로로 참조되어도 한 번만 추가
3. **순환 의존성 미검출** -- 의도적 설계. containerd의 플러그인 타입 체계가 순환을 구조적으로 방지

### DisableFilter

```go
type DisableFilter func(r *Registration) bool
```

설정 파일에서 비활성화된 플러그인을 걸러내는 콜백이다. 비활성화된 플러그인은 `disabled` 맵에 기록되어 Graph 순회와 children 재귀 모두에서 제외된다.

---

## 6. InitContext와 플러그인 조회 메커니즘

InitContext는 플러그인 초기화 시 주입되는 컨텍스트 객체로, 의존 플러그인 접근을 위한 세 가지 조회 메서드를 제공한다.

### InitContext 구조체

```go
// 소스: vendor/github.com/containerd/plugin/context.go:27-37

type InitContext struct {
    Context           context.Context
    Properties        map[string]string        // 디렉토리 경로 등
    Config            interface{}              // TOML 설정 역직렬화 결과
    RegisterReadiness func() func()            // 준비 완료 신호
    Meta              *Meta                    // 초기화 중 설정할 메타데이터
    plugins           *Set                     // 이미 초기화된 플러그인들
}
```

### GetSingle: 단일 인스턴스 조회

```go
// 소스: vendor/github.com/containerd/plugin/context.go:137-160

func (i *InitContext) GetSingle(t Type) (interface{}, error) {
    var (
        found    bool
        instance interface{}
    )
    for _, v := range i.plugins.byTypeAndID[t] {
        i, err := v.Instance()
        if err != nil {
            if IsSkipPlugin(err) {
                continue  // Skip된 플러그인은 무시
            }
            return i, err
        }
        if found {
            return nil, fmt.Errorf("multiple plugins registered for %s: %w",
                t, ErrPluginMultipleInstances)
        }
        instance = i
        found = true
    }
    if !found {
        return nil, fmt.Errorf("no plugins registered for %s: %w",
            t, ErrPluginNotFound)
    }
    return instance, nil
}
```

**왜 GetSingle이 중요한가:**
- MetadataPlugin, ContentPlugin 등 **하나만 존재해야 하는** 플러그인을 조회할 때 사용
- 여러 개가 등록되어 있으면 `ErrPluginMultipleInstances` 반환
- Skip된 플러그인은 존재하지 않는 것으로 처리

### GetByType: 타입별 전체 조회

```go
// 소스: vendor/github.com/containerd/plugin/context.go:182-199

func (i *InitContext) GetByType(t Type) (map[string]interface{}, error) {
    pi := map[string]interface{}{}
    for id, p := range i.plugins.byTypeAndID[t] {
        i, err := p.Instance()
        if err != nil {
            if IsSkipPlugin(err) {
                continue
            }
            return nil, err
        }
        pi[id] = i
    }
    if len(pi) == 0 {
        return nil, fmt.Errorf("no plugins registered for %s: %w",
            t, ErrPluginNotFound)
    }
    return pi, nil
}
```

여러 인스턴스가 존재할 수 있는 SnapshotPlugin, GRPCPlugin 등을 조회할 때 사용한다. 반환값은 `map[ID]instance` 형태이다.

### GetByID: 특정 인스턴스 조회

```go
// 소스: vendor/github.com/containerd/plugin/context.go:173-179

func (i *InitContext) GetByID(t Type, id string) (interface{}, error) {
    p := i.plugins.Get(t, id)
    if p == nil {
        return nil, fmt.Errorf("no plugins registered for %s.%s: %w",
            t, id, ErrPluginNotFound)
    }
    return p.Instance()
}
```

Type과 ID를 모두 지정하여 정확한 플러그인을 조회한다. 예를 들어 `GetByID(plugins.RuntimePluginV2, "task")`처럼 사용한다.

---

## 7. Plugin Set: 초기화된 플러그인 관리

### Set 구조체

```go
// 소스: vendor/github.com/containerd/plugin/context.go:89-92

type Set struct {
    ordered     []*Plugin                      // 초기화 순서대로
    byTypeAndID map[Type]map[string]*Plugin    // 2단계 인덱스
}
```

Set은 두 가지 접근 패턴을 지원한다:
1. **순서 기반** -- `ordered` 슬라이스로 초기화 순서 보존
2. **Type+ID 기반** -- `byTypeAndID` 2단계 맵으로 O(1) 조회

### Add: 중복 방지

```go
// 소스: vendor/github.com/containerd/plugin/context.go:102-115

func (ps *Set) Add(p *Plugin) error {
    if byID, typeok := ps.byTypeAndID[p.Registration.Type]; !typeok {
        ps.byTypeAndID[p.Registration.Type] = map[string]*Plugin{
            p.Registration.ID: p,
        }
    } else if _, idok := byID[p.Registration.ID]; !idok {
        byID[p.Registration.ID] = p
    } else {
        return fmt.Errorf("plugin add failed for %s: %w",
            p.Registration.URI(), ErrPluginInitialized)
    }
    ps.ordered = append(ps.ordered, p)
    return nil
}
```

Type이 처음 등록되면 새 맵을 생성하고, 같은 Type의 다른 ID면 기존 맵에 추가한다. 같은 Type.ID가 이미 있으면 `ErrPluginInitialized` 에러를 반환한다.

---

## 8. 플러그인 타입 체계

containerd는 30개 이상의 플러그인 타입을 정의한다. 주요 타입들을 기능별로 분류한다.

### 플러그인 타입 상수

```go
// 소스: plugins/types.go:25-84

const (
    InternalPlugin         Type = "io.containerd.internal.v1"
    RuntimePlugin          Type = "io.containerd.runtime.v1"
    RuntimePluginV2        Type = "io.containerd.runtime.v2"
    ServicePlugin          Type = "io.containerd.service.v1"
    GRPCPlugin             Type = "io.containerd.grpc.v1"
    TTRPCPlugin            Type = "io.containerd.ttrpc.v1"
    SnapshotPlugin         Type = "io.containerd.snapshotter.v1"
    TaskMonitorPlugin      Type = "io.containerd.monitor.task.v1"
    ContainerMonitorPlugin Type = "io.containerd.monitor.container.v1"
    DiffPlugin             Type = "io.containerd.differ.v1"
    MetadataPlugin         Type = "io.containerd.metadata.v1"
    ContentPlugin          Type = "io.containerd.content.v1"
    GCPlugin               Type = "io.containerd.gc.v1"
    EventPlugin            Type = "io.containerd.event.v1"
    LeasePlugin            Type = "io.containerd.lease.v1"
    StreamingPlugin        Type = "io.containerd.streaming.v1"
    TracingProcessorPlugin Type = "io.containerd.tracing.processor.v1"
    NRIApiPlugin           Type = "io.containerd.nri.v1"
    TransferPlugin         Type = "io.containerd.transfer.v1"
    SandboxStorePlugin     Type = "io.containerd.sandbox.store.v1"
    SandboxControllerPlugin Type = "io.containerd.sandbox.controller.v1"
    ImageVerifierPlugin    Type = "io.containerd.image-verifier.v1"
    WarningPlugin          Type = "io.containerd.warning.v1"
    CRIServicePlugin       Type = "io.containerd.cri.v1"
    ShimPlugin             Type = "io.containerd.shim.v1"
    HTTPHandler            Type = "io.containerd.http.v1"
)
```

### 기능별 분류

| 카테고리 | 타입 | 설명 | 다중 인스턴스 |
|---------|------|------|:---:|
| **저장소** | ContentPlugin | 블롭 CAS 저장 | 단일 |
| | MetadataPlugin | BoltDB 메타데이터 | 단일 |
| | SnapshotPlugin | 파일시스템 스냅샷 | 다중 |
| **런타임** | RuntimePluginV2 | shim v2 런타임 | 단일 |
| | TaskMonitorPlugin | 태스크 상태 모니터링 | 단일 |
| **네트워크** | GRPCPlugin | gRPC 서비스 제공 | 다중 |
| | TTRPCPlugin | ttrpc 서비스 제공 | 다중 |
| **라이프사이클** | GCPlugin | 가비지 컬렉션 | 단일 |
| | LeasePlugin | 리스 관리 | 단일 |
| | EventPlugin | 이벤트 발행/구독 | 단일 |
| **상위 서비스** | ServicePlugin | 비즈니스 로직 | 다중 |
| | CRIServicePlugin | CRI 구현 | 단일 |
| | TransferPlugin | 이미지 전송 | 단일 |

### Property 상수

```go
// 소스: plugins/types.go:96-109

const (
    PropertyRootDir      = "io.containerd.plugin.root"
    PropertyStateDir     = "io.containerd.plugin.state"
    PropertyGRPCAddress  = "io.containerd.plugin.grpc.address"
    PropertyTTRPCAddress = "io.containerd.plugin.ttrpc.address"
)

const (
    SnapshotterRootDir = "root"
)
```

이 상수들은 InitContext.Properties 맵의 키로 사용되어, 각 플러그인이 데이터를 저장할 루트 디렉토리, 상태 디렉토리, 서버 주소 등을 참조할 수 있게 한다.

---

## 9. 전역 Registry와 Register 패턴

### 전역 등록 메커니즘

```go
// 소스: vendor/github.com/containerd/plugin/registry/register.go:25-50

var register = struct {
    sync.RWMutex
    r plugin.Registry
}{}

func Register(r *plugin.Registration) {
    register.Lock()
    defer register.Unlock()
    register.r = register.r.Register(r)
}

func Graph(filter plugin.DisableFilter) []plugin.Registration {
    register.RLock()
    defer register.RUnlock()
    return register.r.Graph(filter)
}
```

**왜 전역 변수인가:**
- Go의 `init()` 함수에서 호출되므로, 패키지 임포트만으로 플러그인이 등록된다
- `sync.RWMutex`로 동시성 안전을 보장한다
- `Registry`는 불변(immutable) -- `Register`는 기존 슬라이스에 append한 새 슬라이스를 반환한다

### Registry.Register: 유효성 검증

```go
// 소스: vendor/github.com/containerd/plugin/plugin.go:151-169

func (registry Registry) Register(r *Registration) Registry {
    if r.Type == "" {
        panic(ErrNoType)
    }
    if r.ID == "" {
        panic(ErrNoPluginID)
    }
    if err := checkUnique(registry, r); err != nil {
        panic(err)
    }
    for _, requires := range r.Requires {
        if requires == "*" && len(r.Requires) != 1 {
            panic(ErrInvalidRequires)
        }
    }
    return append(registry, r)
}
```

**panic을 사용하는 이유:**
- `init()` 시점의 프로그래밍 에러를 즉시 감지
- Type/ID 누락이나 중복은 개발 시점에 잡아야 하는 버그
- `"*"` 와일드카드 의존성과 다른 타입 의존성 혼합은 논리적 모순

---

## 10. 실제 플러그인 등록 예시: Overlay Snapshotter

### 등록 코드 분석

```go
// 소스: plugins/snapshots/overlay/plugin/plugin.go:54-102

func init() {
    registry.Register(&plugin.Registration{
        Type:   plugins.SnapshotPlugin,
        ID:     "overlayfs",
        Config: &Config{},
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            // 1. 플랫폼 선언
            ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())

            // 2. Config 타입 단언
            config, ok := ic.Config.(*Config)
            if !ok {
                return nil, errors.New("invalid overlay configuration")
            }

            // 3. 루트 디렉토리 결정
            root := ic.Properties[plugins.PropertyRootDir]
            if config.RootPath != "" {
                root = config.RootPath
            }

            // 4. 옵션 구성
            var oOpts []overlay.Opt
            if config.UpperdirLabel {
                oOpts = append(oOpts, overlay.WithUpperdirLabel)
            }
            if !config.SyncRemove {
                oOpts = append(oOpts, overlay.AsynchronousRemove)
            }
            if len(config.MountOptions) > 0 {
                oOpts = append(oOpts, overlay.WithMountOptions(config.MountOptions))
            }

            // 5. 능력(Capability) 선언
            if ok, err := overlayutils.SupportsIDMappedMounts(); err == nil && ok {
                oOpts = append(oOpts, overlay.WithRemapIDs)
                ic.Meta.Capabilities = append(ic.Meta.Capabilities, "remap-ids")
            }
            if config.SlowChown {
                oOpts = append(oOpts, overlay.WithSlowChown)
            } else {
                ic.Meta.Capabilities = append(ic.Meta.Capabilities, "only-remap-ids")
            }
            ic.Meta.Capabilities = append(ic.Meta.Capabilities, "rebase")

            // 6. Export 설정
            ic.Meta.Exports[plugins.SnapshotterRootDir] = root

            // 7. 실제 인스턴스 생성
            return overlay.NewSnapshotter(root, oOpts...)
        },
    })
}
```

### 흐름 분석

```
패키지 임포트 (blank import)
    |
    v
init() 호출
    |
    v
registry.Register() -- Type: SnapshotPlugin, ID: "overlayfs"
    |
    v
... (containerd 시작 시) ...
    |
    v
registry.Graph(filter)로 정렬
    |
    v
Registration.Init(ic) 호출
    |
    v
InitFn 실행:
    1. 플랫폼 메타데이터 설정
    2. Config 구조체에서 설정값 추출
    3. Properties에서 루트 디렉토리 획득
    4. 기능 옵션 빌더 패턴으로 구성
    5. 런타임 감지한 능력 Meta.Capabilities에 기록
    6. Meta.Exports에 루트 디렉토리 노출
    7. overlay.NewSnapshotter() 호출 → Snapshotter 인터페이스 반환
```

### Config 구조체

```go
// 소스: plugins/snapshots/overlay/plugin/plugin.go:39-52

type Config struct {
    RootPath      string   `toml:"root_path"`
    UpperdirLabel bool     `toml:"upperdir_label"`
    SyncRemove    bool     `toml:"sync_remove"`
    SlowChown     bool     `toml:"slow_chown"`
    MountOptions  []string `toml:"mount_options"`
}
```

TOML 태그로 containerd 설정 파일의 해당 섹션을 자동 역직렬화한다.

---

## 11. Shim에서의 플러그인 로딩 흐름

containerd-shim도 동일한 플러그인 시스템을 사용하지만, 범위가 제한적이다.

### Shim의 플러그인 로딩

```go
// 소스: pkg/shim/shim.go:319-409 (run 함수 내부)

// shutdown과 publisher를 내부 플러그인으로 등록
registry.Register(&plugin.Registration{
    Type: plugins.InternalPlugin,
    ID:   "shutdown",
    InitFn: func(ic *plugin.InitContext) (interface{}, error) {
        return sd, nil
    },
})

registry.Register(&plugin.Registration{
    Type: plugins.EventPlugin,
    ID:   "publisher",
    InitFn: func(ic *plugin.InitContext) (interface{}, error) {
        return NewPublisher(ttrpcAddress, ...)
    },
})

// 전체 Registry를 Graph로 정렬하고 순회
var (
    initialized   = plugin.NewPluginSet()
    ttrpcServices = []TTRPCService{}
)

for _, p := range registry.Graph(func(*plugin.Registration) bool { return false }) {
    initContext := plugin.NewContext(
        ctx,
        initialized,
        map[string]string{
            plugins.PropertyStateDir:     filepath.Join(bundlePath, p.URI()),
            plugins.PropertyGRPCAddress:  addressFlag,
            plugins.PropertyTTRPCAddress: ttrpcAddress,
        },
    )

    result := p.Init(initContext)
    if err := initialized.Add(result); err != nil {
        return err
    }

    instance, err := result.Instance()
    if err != nil {
        if plugin.IsSkipPlugin(err) {
            continue  // Skip된 플러그인은 로그만 남기고 계속
        }
        return err  // 그 외 에러는 치명적
    }

    // TTRPCService 인터페이스를 구현하면 서비스로 등록
    if src, ok := instance.(TTRPCService); ok {
        ttrpcServices = append(ttrpcServices, src)
    }
}
```

### Shim vs Daemon의 차이

| 항목 | containerd daemon | containerd-shim |
|------|------------------|-----------------|
| 플러그인 수 | 30+ | 3~5 |
| RootDir | 있음 | 없음 (StateDir만) |
| gRPC | 있음 | 없음 (ttrpc만) |
| DisableFilter | 설정 파일 기반 | 항상 false (전부 로딩) |
| 목적 | 전체 컨테이너 관리 | 단일 컨테이너 런타임 |

---

## 12. Meta: Capabilities와 Exports

Meta 구조체는 플러그인이 초기화 과정에서 자신의 능력과 내보내는 값을 선언하는 메커니즘이다.

### Meta 구조체

```go
// 소스: vendor/github.com/containerd/plugin/context.go:56-60

type Meta struct {
    Platforms    []imagespec.Platform  // 지원 플랫폼
    Exports      map[string]string     // 내보내는 값
    Capabilities []string              // 기능 플래그
}
```

### Capabilities 예시

overlay snapshotter에서 선언하는 능력들:

```
"remap-ids"       -- 커널이 ID-mapped 마운트를 지원하는 경우
"only-remap-ids"  -- slowChown 비활성화 시 (ID-mapped만 허용)
"rebase"          -- 항상 선언 (스냅샷 리베이스 지원)
```

이 능력들은 다른 컴포넌트가 snapshotter의 기능을 동적으로 확인하는 데 사용된다. 예를 들어 CRI 플러그인이 user namespace 컨테이너를 생성할 때 snapshotter가 `"remap-ids"`를 지원하는지 확인한다.

### Exports 예시

```go
ic.Meta.Exports[plugins.SnapshotterRootDir] = root
```

Exports는 플러그인이 다른 플러그인에게 노출하는 키-값 쌍이다. 여기서는 snapshotter의 루트 디렉토리를 노출하여, 다른 플러그인이 이 정보를 참조할 수 있게 한다.

---

## 13. 에러 처리와 Skip 메커니즘

### ErrSkipPlugin

```go
// 소스: vendor/github.com/containerd/plugin/plugin.go:33-35

ErrSkipPlugin = errors.New("skip plugin")

func IsSkipPlugin(err error) bool {
    return errors.Is(err, ErrSkipPlugin)
}
```

ErrSkipPlugin은 **의도적 비활성화**를 나타낸다. 예시:
- Windows에서 Linux 전용 snapshotter 로드 시도
- 커널이 특정 기능을 미지원하는 경우
- 설정에서 명시적으로 비활성화된 경우

### 에러 계층 구조

```
InitFn 에러 반환
    |
    +-- ErrSkipPlugin --> Plugin.err에 저장, 로그 출력, 계속 진행
    |                     GetSingle/GetByType에서 자동으로 무시
    |
    +-- 기타 에러 -----> Plugin.err에 저장
                         shim: 치명적 에러로 종료
                         daemon: 설정에 따라 처리
```

### Plugin.Instance()

```go
// 소스: vendor/github.com/containerd/plugin/context.go:79-81

func (p *Plugin) Instance() (interface{}, error) {
    return p.instance, p.err
}
```

초기화에 실패한 플러그인도 Set에 존재하며, Instance() 호출 시 에러와 함께 반환된다. 이는 초기화 실패를 조회 시점까지 지연시키는 패턴이다. 이로써 초기화 순서에 관계없이 일관된 에러 전파가 가능하다.

---

## 14. 설계 철학과 트레이드오프

### 장점

1. **극단적 모듈성** -- 거의 모든 것이 교체 가능
2. **단순한 등록** -- `init()` + `registry.Register()` 한 줄
3. **자동 의존성 해석** -- Type 기반으로 명시적 순서 지정 불필요
4. **점진적 확장** -- 새 플러그인 타입 추가가 기존 코드에 영향 없음
5. **동적 기능 감지** -- Capabilities로 런타임에 기능 확인

### 트레이드오프

1. **interface{} 사용** -- 타입 안전성을 런타임 타입 단언으로 위임
2. **순환 의존성 미검출** -- Graph()가 무한 재귀에 빠질 수 있으나, 타입 체계로 방지
3. **전역 상태** -- registry 패키지의 전역 변수. 테스트 시 Reset() 필요
4. **panic 기반 검증** -- Register 시점의 프로그래밍 에러는 panic으로 처리

### 다른 DI 프레임워크와 비교

| 항목 | containerd plugin | wire (Google) | fx (Uber) |
|------|------------------|---------------|-----------|
| 의존성 선언 | `Requires []Type` | 함수 매개변수 | `fx.In` 구조체 |
| 해석 시점 | 런타임 (Graph) | 컴파일 타임 | 런타임 |
| 타입 안전성 | `interface{}` | 완전 | 리플렉션 |
| 복잡도 | 낮음 | 중간 | 높음 |
| 적합한 규모 | 중간 | 대규모 | 대규모 |

containerd의 플러그인 시스템은 **단순성**을 최우선으로 한다. 200줄 미만의 코어 코드(`plugin.go` + `context.go`)로 30개 이상의 서브시스템을 관리한다. 이는 containerd의 "단순하고 견고하게"라는 철학을 잘 보여주는 설계이다.
