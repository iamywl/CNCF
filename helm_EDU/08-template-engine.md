# 08. 템플릿 엔진 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Engine 구조체](#2-engine-구조체)
3. [렌더링 파이프라인](#3-렌더링-파이프라인)
4. [템플릿 데이터 준비: allTemplates](#4-템플릿-데이터-준비-alltemplates)
5. [커스텀 템플릿 함수](#5-커스텀-템플릿-함수)
6. [include와 tpl 함수](#6-include와-tpl-함수)
7. [lookup 함수와 Kubernetes 연동](#7-lookup-함수와-kubernetes-연동)
8. [Files 객체](#8-files-객체)
9. [renderResources: 액션 계층에서의 통합](#9-renderresources-액션-계층에서의-통합)
10. [에러 처리와 디버깅](#10-에러-처리와-디버깅)
11. [설계 결정과 Why 분석](#11-설계-결정과-why-분석)

---

## 1. 개요

Helm의 템플릿 엔진은 Go의 `text/template` 패키지 위에 구축된 차트 렌더링 시스템이다.
단순한 변수 치환을 넘어서, Kubernetes 클러스터 조회(`lookup`), 동적 템플릿 평가(`tpl`),
재귀적 포함(`include`), 다양한 데이터 형식 변환 함수를 제공한다.

### 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| 스코핑 | 각 차트의 템플릿은 자신의 Values만 접근 |
| 지연 바인딩 | `include`, `tpl`, `lookup`은 렌더링 시점에 바인딩 |
| 결정론적 순서 | 템플릿 파싱과 실행 순서가 예측 가능 |
| 안전한 기본값 | 환경 변수 접근, DNS 조회 등 기본 비활성화 |
| 확장성 | `CustomTemplateFuncs`로 사용자 정의 함수 추가 가능 |

### 관련 소스 파일

```
pkg/engine/
├── engine.go       # Engine 구조체, Render(), render(), allTemplates()
├── funcs.go        # funcMap(), toYAML, fromJSON 등 커스텀 함수
├── lookup_func.go  # lookup 함수, ClientProvider 인터페이스
├── files.go        # Files 객체 (Glob, AsConfig, AsSecrets)
└── doc.go          # 패키지 문서

pkg/action/
└── action.go       # renderResources() - 엔진과 액션 통합
```

---

## 2. Engine 구조체

### 2.1 구조체 정의

```go
// pkg/engine/engine.go

type Engine struct {
    Strict              bool
    LintMode            bool
    clientProvider      *ClientProvider
    EnableDNS           bool
    CustomTemplateFuncs template.FuncMap
}
```

| 필드 | 타입 | 기본값 | 설명 |
|------|------|--------|------|
| `Strict` | `bool` | `false` | `true`이면 존재하지 않는 값 참조 시 에러 |
| `LintMode` | `bool` | `false` | `true`이면 `required`, `fail` 함수가 에러 대신 경고 출력 |
| `clientProvider` | `*ClientProvider` | `nil` | Kubernetes API 접근 제공자 (비공개) |
| `EnableDNS` | `bool` | `false` | `true`이면 `getHostByName` 활성화 |
| `CustomTemplateFuncs` | `template.FuncMap` | `nil` | 사용자 정의 템플릿 함수 |

### 2.2 생성 함수

```go
// pkg/engine/engine.go

func New(config *rest.Config) Engine {
    var clientProvider ClientProvider = clientProviderFromConfig{config}
    return Engine{
        clientProvider: &clientProvider,
    }
}
```

**왜 `clientProvider`가 포인터인가?**
- `nil` 포인터로 "Kubernetes 접속 불가" 상태를 표현
- `helm template` 같은 오프라인 명령에서는 clientProvider가 nil
- nil 검사로 `lookup` 함수의 활성화/비활성화를 결정

### 2.3 렌더링 엔트리 포인트

```go
// pkg/engine/engine.go

// 인스턴스 메서드
func (e Engine) Render(chrt ci.Charter, values common.Values) (map[string]string, error) {
    tmap := allTemplates(chrt, values)  // 템플릿 맵 구성
    return e.render(tmap)                // 실제 렌더링
}

// 패키지 레벨 편의 함수
func Render(chrt ci.Charter, values common.Values) (map[string]string, error) {
    return new(Engine).Render(chrt, values)
}

// REST 설정을 포함한 렌더링
func RenderWithClient(chrt ci.Charter, values common.Values, config *rest.Config) (map[string]string, error) {
    var clientProvider ClientProvider = clientProviderFromConfig{config}
    return Engine{clientProvider: &clientProvider}.Render(chrt, values)
}
```

---

## 3. 렌더링 파이프라인

### 3.1 전체 흐름

```
Engine.Render(chart, values)
    │
    ├── 1. allTemplates()
    │       ├── recAllTpls() 재귀 호출
    │       │   ├── Accessor로 차트 메타데이터 추출
    │       │   ├── Values 스코핑
    │       │   ├── 서브차트 재귀 처리
    │       │   └── renderable 맵 구성
    │       └── map[string]renderable 반환
    │
    ├── 2. render(tmap)
    │       ├── template.New("gotpl") 생성
    │       ├── missingkey 옵션 설정
    │       ├── initFunMap() - 함수 맵 등록
    │       ├── sortTemplates() - 렌더링 순서 결정
    │       ├── 모든 템플릿 파싱 (Parse)
    │       └── 각 템플릿 실행 (ExecuteTemplate)
    │           ├── 파셜(_로 시작) 건너뛰기
    │           ├── Template 메타데이터 주입
    │           └── <no value> 치환
    │
    └── 3. map[string]string 반환 (파일명 → 렌더링 결과)
```

### 3.2 renderable 구조체

```go
// pkg/engine/engine.go

type renderable struct {
    tpl      string        // 템플릿 원본 텍스트
    vals     common.Values // 이 템플릿에 전달될 값
    basePath string        // 템플릿 기본 경로
}
```

### 3.3 render 메서드 상세

```go
// pkg/engine/engine.go

func (e Engine) render(tpls map[string]renderable) (rendered map[string]string, err error) {
    // 패닉 복구 (text/template이 패닉을 발생시킬 수 있음)
    defer func() {
        if r := recover(); r != nil {
            err = fmt.Errorf("rendering template failed: %v", r)
        }
    }()

    // 1. 루트 템플릿 생성
    t := template.New("gotpl")

    // 2. missingkey 옵션 설정
    if e.Strict {
        t.Option("missingkey=error")
    } else {
        t.Option("missingkey=zero")
    }

    // 3. 함수 맵 등록
    e.initFunMap(t)

    // 4. 정렬 (상위 디렉토리 우선)
    keys := sortTemplates(tpls)

    // 5. 모든 템플릿 파싱
    for _, filename := range keys {
        r := tpls[filename]
        if _, err := t.New(filename).Parse(r.tpl); err != nil {
            return map[string]string{}, cleanupParseError(filename, err)
        }
    }

    // 6. 각 템플릿 실행
    rendered = make(map[string]string, len(keys))
    for _, filename := range keys {
        // 파셜(언더스코어 시작)은 건너뛰기
        if strings.HasPrefix(path.Base(filename), "_") {
            continue
        }

        // Template 메타데이터 주입
        vals := tpls[filename].vals
        vals["Template"] = common.Values{
            "Name":     filename,
            "BasePath": tpls[filename].basePath,
        }

        var buf strings.Builder
        if err := t.ExecuteTemplate(&buf, filename, vals); err != nil {
            return map[string]string{}, reformatExecErrorMsg(filename, err)
        }

        // <no value> 치환 (missingkey=zero의 한계 극복)
        rendered[filename] = strings.ReplaceAll(buf.String(), "<no value>", "")
    }

    return rendered, nil
}
```

### 3.4 템플릿 정렬

```go
// pkg/engine/engine.go

func sortTemplates(tpls map[string]renderable) []string {
    keys := make([]string, len(tpls))
    // ...
    sort.Sort(sort.Reverse(byPathLen(keys)))
    return keys
}

type byPathLen []string

func (p byPathLen) Less(i, j int) bool {
    a, b := p[i], p[j]
    ca, cb := strings.Count(a, "/"), strings.Count(b, "/")
    if ca == cb {
        return strings.Compare(a, b) == -1
    }
    return ca < cb  // 경로 깊이가 얕은 것이 먼저
}
```

**정렬 규칙:**
1. 경로 깊이가 깊은 것(서브차트)이 먼저 파싱됨
2. 같은 깊이면 알파벳 역순
3. `sort.Reverse`로 뒤집으므로 실제로는 **깊은 경로 우선**

**왜 이 순서인가?**
- 서브차트의 `define` 블록이 먼저 등록되어야 부모 차트에서 `include`할 수 있음
- Go의 `text/template`는 같은 이름의 define이 여러 번 나오면 마지막 것이 승리
- 상위 차트가 나중에 파싱되므로 상위의 define이 하위를 덮어씀

---

## 4. 템플릿 데이터 준비: allTemplates

### 4.1 allTemplates 함수

```go
// pkg/engine/engine.go

func allTemplates(c ci.Charter, vals common.Values) map[string]renderable {
    templates := make(map[string]renderable)
    recAllTpls(c, templates, vals)
    return templates
}
```

### 4.2 recAllTpls 재귀 함수

```go
// pkg/engine/engine.go

func recAllTpls(c ci.Charter, templates map[string]renderable, values common.Values) map[string]any {
    vals := values.AsMap()
    subCharts := make(map[string]any)
    accessor, _ := ci.NewAccessor(c)

    // 차트 메타데이터를 맵으로 변환 ({{ .Chart.Name }} 등에 사용)
    chartMetaData := accessor.MetadataAsMap()
    chartMetaData["IsRoot"] = accessor.IsRoot()

    // 이 차트의 템플릿에 전달될 데이터 구성
    next := map[string]any{
        "Chart":        chartMetaData,
        "Files":        newFiles(accessor.Files()),
        "Release":      vals["Release"],
        "Capabilities": vals["Capabilities"],
        "Values":       make(common.Values),
        "Subcharts":    subCharts,
    }

    // Values 스코핑
    if accessor.IsRoot() {
        next["Values"] = vals["Values"]
    } else if vs, err := values.Table("Values." + accessor.Name()); err == nil {
        next["Values"] = vs
    }

    // 서브차트 재귀 처리
    for _, child := range accessor.Dependencies() {
        sub, _ := ci.NewAccessor(child)
        subCharts[sub.Name()] = recAllTpls(child, templates, next)
    }

    // 템플릿 등록
    newParentID := accessor.ChartFullPath()
    for _, t := range accessor.Templates() {
        if t == nil { continue }
        if !isTemplateValid(accessor, t.Name) { continue }

        templates[path.Join(newParentID, t.Name)] = renderable{
            tpl:      string(t.Data),
            vals:     next,
            basePath: path.Join(newParentID, "templates"),
        }
    }

    return next
}
```

### 4.3 Values 스코핑 다이어그램

```
사용자 입력 values:
{
  "Release": {...},
  "Capabilities": {...},
  "Values": {
    "replicaCount": 3,
    "subchart-a": {
      "port": 8080
    }
  }
}

루트 차트가 보는 Values:
{
  "Chart":        {Name: "myapp", ...},
  "Release":      {...},
  "Capabilities": {...},
  "Values": {
    "replicaCount": 3,
    "subchart-a": {"port": 8080}
  },
  "Subcharts": {"subchart-a": {...}}
}

subchart-a가 보는 Values:
{
  "Chart":        {Name: "subchart-a", ...},
  "Release":      {...},
  "Capabilities": {...},
  "Values": {
    "port": 8080          ← 부모의 "subchart-a" 섹션이 여기로
  },
  "Subcharts": {}
}
```

**왜 이런 스코핑을 하는가?**
- 각 차트가 독립적으로 개발/테스트 가능하도록 함
- 서브차트는 자신의 값만 보므로, 부모 차트의 내부 구조에 의존하지 않음
- `global` 키를 통해 차트 간 공유 값을 명시적으로 전달

### 4.4 Library 차트 필터링

```go
// pkg/engine/engine.go

func isTemplateValid(accessor ci.Accessor, templateName string) bool {
    if accessor.IsLibraryChart() {
        return strings.HasPrefix(filepath.Base(templateName), "_")
    }
    return true
}
```

Library 차트는 `_`로 시작하는 파셜 템플릿만 포함할 수 있다.
`deployment.yaml` 같은 일반 템플릿은 library 차트에서 렌더링되지 않는다.

---

## 5. 커스텀 템플릿 함수

### 5.1 함수 맵 구성

```go
// pkg/engine/funcs.go

func funcMap() template.FuncMap {
    f := sprig.TxtFuncMap()    // sprig 함수 전체 로드

    // 보안상 제거하는 함수들
    delete(f, "env")           // 환경 변수 접근 차단
    delete(f, "expandenv")     // 환경 변수 확장 차단

    // Helm 추가 함수
    extra := template.FuncMap{
        "toToml":        toTOML,
        "fromToml":      fromTOML,
        "toYaml":        toYAML,
        "mustToYaml":    mustToYAML,
        "toYamlPretty":  toYAMLPretty,
        "fromYaml":      fromYAML,
        "fromYamlArray": fromYAMLArray,
        "toJson":        toJSON,
        "mustToJson":    mustToJSON,
        "fromJson":      fromJSON,
        "fromJsonArray": fromJSONArray,
        "include":       func(string, any) string { return "not implemented" },
        "tpl":           func(string, any) any { return "not implemented" },
        "required":      func(string, any) (any, error) { return "not implemented", nil },
        "lookup":        func(string, string, string, string) (map[string]any, error) {
            return map[string]any{}, nil
        },
    }

    maps.Copy(f, extra)
    return f
}
```

### 5.2 데이터 변환 함수

| 함수 | 입력 | 출력 | 에러 처리 |
|------|------|------|-----------|
| `toYaml` | `any` | YAML 문자열 | 빈 문자열 반환 |
| `mustToYaml` | `any` | YAML 문자열 | 패닉 |
| `toYamlPretty` | `any` | 들여쓰기된 YAML | 빈 문자열 반환 |
| `fromYaml` | 문자열 | `map[string]any` | Error 키에 메시지 |
| `fromYamlArray` | 문자열 | `[]any` | 에러 메시지를 배열 첫 원소에 |
| `toJson` | `any` | JSON 문자열 | 빈 문자열 반환 |
| `mustToJson` | `any` | JSON 문자열 | 패닉 |
| `fromJson` | 문자열 | `map[string]any` | Error 키에 메시지 |
| `fromJsonArray` | 문자열 | `[]any` | 에러 메시지를 배열 첫 원소에 |
| `toToml` | `any` | TOML 문자열 | 에러 메시지 반환 |
| `fromToml` | 문자열 | `map[string]any` | Error 키에 메시지 |

### 5.3 toYAML 구현 상세

```go
// pkg/engine/funcs.go

func toYAML(v any) string {
    data, err := yaml.Marshal(v)
    if err != nil {
        return ""  // 템플릿 내부에서 에러를 삼킴
    }
    return strings.TrimSuffix(string(data), "\n")
}

func mustToYAML(v any) string {
    data, err := yaml.Marshal(v)
    if err != nil {
        panic(err)  // 렌더링 실패를 의도적으로 발생
    }
    return strings.TrimSuffix(string(data), "\n")
}
```

**왜 `toYaml`과 `mustToYaml`이 분리되어 있는가?**
- `toYaml`: 변환 실패해도 렌더링을 계속해야 하는 경우 (기본)
- `mustToYaml`: 반드시 유효한 YAML이어야 하는 중요 설정에 사용
- 패닉은 `render()` 메서드의 `defer recover()`에 의해 잡혀서 에러로 변환됨

### 5.4 toYAMLPretty - 들여쓰기 지원

```go
// pkg/engine/funcs.go

func toYAMLPretty(v any) string {
    var data bytes.Buffer
    encoder := goYaml.NewEncoder(&data)
    encoder.SetIndent(2)  // 2칸 들여쓰기
    err := encoder.Encode(v)
    if err != nil {
        return ""
    }
    return strings.TrimSuffix(data.String(), "\n")
}
```

### 5.5 fromYAML의 에러 처리 패턴

```go
// pkg/engine/funcs.go

func fromYAML(str string) map[string]any {
    m := map[string]any{}
    if err := yaml.Unmarshal([]byte(str), &m); err != nil {
        m["Error"] = err.Error()
    }
    return m
}
```

**왜 에러를 반환하지 않고 맵에 넣는가?**
- Go의 `text/template`에서 함수 반환값으로 에러를 주면 렌더링이 중단됨
- `fromYaml`은 파싱 실패해도 렌더링을 계속하고, 템플릿에서 `.Error`를 검사하도록 함
- 예: `{{ $data := fromYaml .someYaml }}{{ if $data.Error }}FAIL{{ end }}`

---

## 6. include와 tpl 함수

### 6.1 include 함수

```go
// pkg/engine/engine.go

func includeFun(t *template.Template, includedNames map[string]int) func(string, any) (string, error) {
    return func(name string, data any) (string, error) {
        var buf strings.Builder

        // 재귀 깊이 검사 (무한 루프 방지)
        if v, ok := includedNames[name]; ok {
            if v > recursionMaxNums {  // 1000회 초과
                return "", fmt.Errorf(
                    "rendering template has a nested reference name: %s",
                    name)
            }
            includedNames[name]++
        } else {
            includedNames[name] = 1
        }

        err := t.ExecuteTemplate(&buf, name, data)
        includedNames[name]--  // 재귀 카운터 감소

        return buf.String(), err
    }
}
```

**`include` vs Go의 내장 `template` 액션의 차이:**

| 특성 | `{{ template "name" . }}` | `{{ include "name" . }}` |
|------|---------------------------|--------------------------|
| 반환값 | 직접 출력에 삽입 | 문자열 반환 |
| 파이프라인 연계 | 불가 | 가능 (`\| indent 4`) |
| 재귀 보호 | Go 기본 | Helm 커스텀 (1000회 제한) |

**왜 include가 필요한가?**
- `{{ template }}` 액션은 결과를 파이프라인으로 전달할 수 없음
- `{{ include "helpers.labels" . | indent 4 }}` 같은 패턴이 Helm에서 매우 흔함
- `include`의 결과를 변수에 저장하거나 다른 함수에 전달 가능

### 6.2 tpl 함수

```go
// pkg/engine/engine.go

func tplFun(parent *template.Template, includedNames map[string]int, strict bool) func(string, any) (string, error) {
    return func(tpl string, vals any) (string, error) {
        // 1. 부모 템플릿 복제
        t, err := parent.Clone()

        // 2. missingkey 옵션 재설정 (Go 버그 우회)
        if strict {
            t.Option("missingkey=error")
        } else {
            t.Option("missingkey=zero")
        }

        // 3. include/tpl 재주입 (클로저 갱신)
        t.Funcs(template.FuncMap{
            "include": includeFun(t, includedNames),
            "tpl":     tplFun(t, includedNames, strict),
        })

        // 4. 새 템플릿으로 파싱
        t, err = t.New(parent.Name()).Parse(tpl)

        // 5. 실행
        var buf strings.Builder
        t.Execute(&buf, vals)

        // 6. <no value> 치환
        return strings.ReplaceAll(buf.String(), "<no value>", ""), nil
    }
}
```

**`tpl`의 용도:**
- Values에 저장된 템플릿 문자열을 동적으로 평가

```yaml
# values.yaml
greeting: "Hello {{ .Release.Name }}!"
```

```yaml
# templates/configmap.yaml
data:
  greeting: {{ tpl .Values.greeting . }}
```

**왜 Clone을 사용하는가?**
- `tpl`에서 `define` 블록을 선언하면 전역 템플릿 네임스페이스에 영향
- Clone으로 격리된 복사본에서 파싱하여 부작용 방지
- 중첩된 `tpl` 호출에서 각각의 `define`이 충돌하지 않도록 함

### 6.3 지연 바인딩과 initFunMap

```go
// pkg/engine/engine.go

func (e Engine) initFunMap(t *template.Template) {
    funcMap := funcMap()           // 기본 함수 맵
    includedNames := make(map[string]int)

    // 지연 바인딩: include/tpl은 template 인스턴스 t에 대한 클로저
    funcMap["include"] = includeFun(t, includedNames)
    funcMap["tpl"] = tplFun(t, includedNames, e.Strict)

    // required: LintMode에 따라 동작 변경
    funcMap["required"] = func(warn string, val any) (any, error) {
        if val == nil {
            if e.LintMode {
                slog.Warn("missing required value", "message", warn)
                return "", nil
            }
            return val, errors.New(warnWrap(warn))
        }
        // 빈 문자열도 required 실패
        if _, ok := val.(string); ok && val == "" {
            if e.LintMode {
                return "", nil
            }
            return val, errors.New(warnWrap(warn))
        }
        return val, nil
    }

    // fail: LintMode에서는 에러 대신 로그
    funcMap["fail"] = func(msg string) (string, error) {
        if e.LintMode {
            slog.Info("funcMap fail", "message", msg)
            return "", nil
        }
        return "", errors.New(warnWrap(msg))
    }

    // lookup: 클러스터 접속 가능할 때만 활성화
    if !e.LintMode && e.clientProvider != nil {
        funcMap["lookup"] = newLookupFunction(*e.clientProvider)
    }

    // DNS 비활성화 시 getHostByName을 빈 문자열 반환으로 대체
    if !e.EnableDNS {
        funcMap["getHostByName"] = func(_ string) string { return "" }
    }

    // 사용자 정의 함수 추가
    maps.Copy(funcMap, e.CustomTemplateFuncs)

    t.Funcs(funcMap)
}
```

**지연 바인딩 흐름:**

```
funcMap()에서 placeholder 등록
    │
    ▼
initFunMap()에서 실제 구현으로 교체
    ├── include → includeFun(t, ...) 클로저
    ├── tpl     → tplFun(t, ...) 클로저
    ├── required → LintMode 인식 클로저
    ├── fail    → LintMode 인식 클로저
    └── lookup  → newLookupFunction(*clientProvider)
```

**왜 placeholder가 필요한가?**
- `funcMap()`은 Lint 검사에서도 사용되므로, 모든 함수가 선언되어 있어야 함
- 파싱 단계에서 알 수 없는 함수가 있으면 에러 발생
- placeholder는 파싱을 통과시키고, 실행 시 실제 구현으로 교체됨

---

## 7. lookup 함수와 Kubernetes 연동

### 7.1 ClientProvider 인터페이스

```go
// pkg/engine/lookup_func.go

type ClientProvider interface {
    GetClientFor(apiVersion, kind string) (dynamic.NamespaceableResourceInterface, bool, error)
}

type clientProviderFromConfig struct {
    config *rest.Config
}

func (c clientProviderFromConfig) GetClientFor(apiVersion, kind string) (
    dynamic.NamespaceableResourceInterface, bool, error) {
    return getDynamicClientOnKind(apiVersion, kind, c.config)
}
```

### 7.2 lookup 함수 구현

```go
// pkg/engine/lookup_func.go

type lookupFunc = func(apiversion string, resource string, namespace string, name string) (map[string]any, error)

func newLookupFunction(clientProvider ClientProvider) lookupFunc {
    return func(apiversion string, kind string, namespace string, name string) (map[string]any, error) {
        c, namespaced, err := clientProvider.GetClientFor(apiversion, kind)
        if err != nil { return map[string]any{}, err }

        var client dynamic.ResourceInterface
        if namespaced && namespace != "" {
            client = c.Namespace(namespace)
        } else {
            client = c
        }

        if name != "" {
            // 단일 리소스 조회
            obj, err := client.Get(context.Background(), name, metav1.GetOptions{})
            if err != nil {
                if apierrors.IsNotFound(err) {
                    return map[string]any{}, nil  // 없으면 빈 맵
                }
                return map[string]any{}, err
            }
            return obj.UnstructuredContent(), nil
        }

        // 리소스 목록 조회
        obj, err := client.List(context.Background(), metav1.ListOptions{})
        if err != nil {
            if apierrors.IsNotFound(err) {
                return map[string]any{}, nil
            }
            return map[string]any{}, err
        }
        return obj.UnstructuredContent(), nil
    }
}
```

### 7.3 동적 클라이언트 구성

```go
// pkg/engine/lookup_func.go

func getDynamicClientOnKind(apiversion string, kind string, config *rest.Config) (
    dynamic.NamespaceableResourceInterface, bool, error) {
    gvk := schema.FromAPIVersionAndKind(apiversion, kind)
    apiRes, err := getAPIResourceForGVK(gvk, config)  // Discovery API 사용
    // ...
    gvr := schema.GroupVersionResource{
        Group: apiRes.Group, Version: apiRes.Version, Resource: apiRes.Name,
    }
    intf, _ := dynamic.NewForConfig(config)
    res := intf.Resource(gvr)
    return res, apiRes.Namespaced, nil
}
```

**lookup 사용 예시:**

```yaml
# ConfigMap이 존재하면 그 데이터를 사용
{{ $cm := lookup "v1" "ConfigMap" "default" "my-config" }}
{{ if $cm }}
data: {{ $cm.data | toYaml | nindent 2 }}
{{ end }}

# 네임스페이스의 모든 Secret 나열
{{ $secrets := lookup "v1" "Secret" "default" "" }}
```

### 7.4 lookup의 동작 모드

| 상황 | lookup 동작 |
|------|------------|
| `helm install/upgrade` | Kubernetes API 실제 조회 |
| `helm template` | 항상 빈 맵 반환 (placeholder) |
| `helm lint` | 항상 빈 맵 반환 (LintMode) |
| `--dry-run=server` | Kubernetes API 실제 조회 |
| `--dry-run=client` | 빈 맵 반환 |

---

## 8. Files 객체

### 8.1 files 타입

```go
// pkg/engine/files.go

type files map[string][]byte

func newFiles(from []*common.File) files {
    files := make(map[string][]byte)
    for _, f := range from {
        files[f.Name] = f.Data
    }
    return files
}
```

### 8.2 주요 메서드

```go
// pkg/engine/files.go

// 파일 내용을 문자열로 반환
func (f files) Get(name string) string {
    return string(f.GetBytes(name))
}

// 파일 내용을 바이트 배열로 반환
func (f files) GetBytes(name string) []byte {
    if v, ok := f[name]; ok {
        return v
    }
    return []byte{}
}

// 글로빙 패턴으로 파일 필터링
func (f files) Glob(pattern string) files {
    g, err := glob.Compile(pattern, '/')
    if err != nil {
        g, _ = glob.Compile("**")  // 실패 시 전체 매칭
    }
    nf := newFiles(nil)
    for name, contents := range f {
        if g.Match(name) {
            nf[name] = contents
        }
    }
    return nf
}

// ConfigMap data 섹션용 YAML 생성
func (f files) AsConfig() string {
    m := make(map[string]string)
    for k, v := range f {
        m[path.Base(k)] = string(v)  // 파일명만 키로 사용
    }
    return toYAML(m)
}

// Secret data 섹션용 Base64 인코딩 YAML 생성
func (f files) AsSecrets() string {
    m := make(map[string]string)
    for k, v := range f {
        m[path.Base(k)] = base64.StdEncoding.EncodeToString(v)
    }
    return toYAML(m)
}

// 줄 단위 순회
func (f files) Lines(path string) []string {
    if f == nil || f[path] == nil {
        return []string{}
    }
    s := string(f[path])
    if s[len(s)-1] == '\n' {
        s = s[:len(s)-1]
    }
    return strings.Split(s, "\n")
}
```

### 8.3 Files 사용 패턴

```yaml
# ConfigMap에 설정 파일 포함
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-config
data:
{{ (.Files.Glob "config/**").AsConfig | indent 2 }}

# Secret에 인증서 포함
apiVersion: v1
kind: Secret
metadata:
  name: {{ .Release.Name }}-certs
type: Opaque
data:
{{ (.Files.Glob "certs/*").AsSecrets | indent 2 }}

# 개별 파일 읽기
data:
  nginx.conf: |
{{ .Files.Get "files/nginx.conf" | indent 4 }}
```

**왜 `AsConfig`에서 `path.Base`를 사용하는가?**
- ConfigMap의 키로 전체 경로가 아닌 파일명만 사용하는 것이 일반적
- `config/prod/app.conf`의 키가 `app.conf`가 됨
- 단, 다른 디렉토리에 같은 이름의 파일이 있으면 덮어씌워짐 (문서에 경고)

---

## 9. renderResources: 액션 계층에서의 통합

### 9.1 renderResources 함수

```go
// pkg/action/action.go

func (cfg *Configuration) renderResources(
    ch *chart.Chart, values common.Values,
    releaseName, outputDir string,
    subNotes, useReleaseName, includeCrds bool,
    pr postrenderer.PostRenderer,
    interactWithRemote, enableDNS, hideSecret bool,
) ([]*release.Hook, *bytes.Buffer, string, error) {

    // 1. Capabilities 확인
    caps, _ := cfg.getCapabilities()
    if ch.Metadata.KubeVersion != "" {
        if !chartutil.IsCompatibleRange(ch.Metadata.KubeVersion, caps.KubeVersion.String()) {
            return ..., fmt.Errorf("chart requires kubeVersion: %s", ch.Metadata.KubeVersion)
        }
    }

    // 2. 엔진 생성 및 렌더링
    var files map[string]string
    if interactWithRemote && cfg.RESTClientGetter != nil {
        restConfig, _ := cfg.RESTClientGetter.ToRESTConfig()
        e := engine.New(restConfig)
        e.EnableDNS = enableDNS
        e.CustomTemplateFuncs = cfg.CustomTemplateFuncs
        files, _ = e.Render(ch, values)
    } else {
        var e engine.Engine
        e.EnableDNS = enableDNS
        e.CustomTemplateFuncs = cfg.CustomTemplateFuncs
        files, _ = e.Render(ch, values)
    }

    // 3. NOTES.txt 추출
    for k, v := range files {
        if strings.HasSuffix(k, notesFileSuffix) {
            // NOTES.txt 내용 별도 수집
            delete(files, k)
        }
    }

    // 4. Post-renderer 실행 (있는 경우)
    if pr != nil {
        merged, _ := annotateAndMerge(files)
        postRendered, _ := pr.Run(bytes.NewBufferString(merged))
        files, _ = splitAndDeannotate(postRendered.String())
    }

    // 5. Hook과 일반 매니페스트 분류
    hs, manifests, _ := releaseutil.SortManifests(files, nil, releaseutil.InstallOrder)
    // ...
}
```

### 9.2 전체 렌더링 파이프라인

```
┌─────────────────────────────────────────────────────────────┐
│                    renderResources()                         │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  1. KubeVersion 호환성 검사                                  │
│       │                                                      │
│  2. Engine 생성 (클러스터 접속 여부에 따라)                    │
│       │                                                      │
│  3. Engine.Render() → map[string]string                      │
│       │                                                      │
│  4. NOTES.txt 추출 및 제거                                    │
│       │                                                      │
│  5. Post-renderer 실행 (선택)                                 │
│       │                                                      │
│  6. SortManifests() → Hook + Manifest 분리                    │
│       │                                                      │
│  7. 반환: hooks, manifests, notes                             │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

---

## 10. 에러 처리와 디버깅

### 10.1 파싱 에러 정리

```go
// pkg/engine/engine.go

func cleanupParseError(filename string, err error) error {
    tokens := strings.Split(err.Error(), ": ")
    if len(tokens) == 1 {
        return fmt.Errorf("parse error in (%s): %w", filename, err)
    }
    location := tokens[1]
    errMsg := tokens[len(tokens)-1]
    return fmt.Errorf("parse error at (%s): %s", location, errMsg)
}
```

### 10.2 실행 에러 개선

```go
// pkg/engine/engine.go

type TraceableError struct {
    location         string  // "myapp/templates/deployment.yaml:15:10"
    message          string  // "nil pointer evaluating ..."
    executedFunction string  // "executing \"include\" at <.Values.missing>"
}

func (t TraceableError) String() string {
    var errorString strings.Builder
    if t.location != "" {
        fmt.Fprintf(&errorString, "%s\n  ", t.location)
    }
    if t.executedFunction != "" {
        fmt.Fprintf(&errorString, "%s\n    ", t.executedFunction)
    }
    if t.message != "" {
        fmt.Fprintf(&errorString, "%s\n", t.message)
    }
    return errorString.String()
}
```

**에러 메시지 형식:**
```
myapp/templates/deployment.yaml:15:10
  executing "include" at <.Values.missing>:
    map has no entry for key "missing"
```

### 10.3 warnWrap 메커니즘

```go
// pkg/engine/engine.go

const warnStartDelim = "HELM_ERR_START"
const warnEndDelim = "HELM_ERR_END"

func warnWrap(warn string) string {
    return warnStartDelim + warn + warnEndDelim
}

var warnRegex = regexp.MustCompile(warnStartDelim + `((?s).*)` + warnEndDelim)
```

`required`와 `fail` 함수는 사용자 메시지를 구분자로 감싸서 반환한다.
`reformatExecErrorMsg`에서 이 구분자를 감지하여 사용자 메시지만 추출하고,
Go의 내부 에러 프레임을 제거하여 깔끔한 에러 메시지를 생성한다.

---

## 11. 설계 결정과 Why 분석

### 11.1 왜 Go의 text/template을 사용하는가?

- Go 표준 라이브러리이므로 외부 의존성 없음
- 로직 분리 가능: 템플릿에서 직접 부작용을 만들기 어려움
- 보안: `text/template`은 HTML 이스케이프 없이 원본 출력 (YAML 생성에 적합)
- sprig 함수 라이브러리와의 통합이 잘 되어 있음

### 11.2 왜 env와 expandenv를 제거하는가?

```go
delete(f, "env")
delete(f, "expandenv")
```

- 차트가 호스트 시스템의 환경 변수에 접근하면 보안 위험
- 차트는 이식 가능해야 하므로 환경에 의존하면 안 됨
- 값은 명시적으로 `values.yaml`이나 `--set`으로 전달해야 함

### 11.3 왜 missingkey=zero이고 <no value>를 후처리하는가?

Go의 `text/template`에는 세 가지 missingkey 모드가 있다:
- `invalid`: 아무 것도 출력하지 않음 (기본값, 에러 감지 불가)
- `zero`: 제로 값 출력 (맵은 `<no value>` 출력)
- `error`: 에러 발생

Helm은 `zero`를 사용하되, `<no value>` 문자열을 빈 문자열로 대체한다.
이는 Go 자체의 한계를 우회하기 위한 것이다
(Go 이슈 #43022 참조).

### 11.4 왜 DNS 조회가 기본 비활성화인가?

- `helm template`은 오프라인에서도 동작해야 함
- DNS 조회는 네트워크 의존성을 만들어 비결정적 동작을 유발
- CI/CD 환경에서 DNS가 제한될 수 있음
- 명시적으로 `--enable-dns` 플래그로만 활성화

### 11.5 왜 recursionMaxNums가 1000인가?

```go
const recursionMaxNums = 1000
```

- 실제 사용 사례에서 1000회 이상 재귀가 필요한 경우는 거의 없음
- 무한 재귀 감지를 위한 합리적 상한
- 너무 낮으면 정상적인 깊은 중첩을 막을 수 있음
- 너무 높으면 스택 오버플로 발생 전 감지 불가

---

## 참고: 핵심 소스 파일 경로

| 파일 | 경로 |
|------|------|
| Engine 구조체/Render | `pkg/engine/engine.go` |
| 커스텀 함수(toYaml 등) | `pkg/engine/funcs.go` |
| lookup 함수 | `pkg/engine/lookup_func.go` |
| Files 객체 | `pkg/engine/files.go` |
| renderResources | `pkg/action/action.go` |
