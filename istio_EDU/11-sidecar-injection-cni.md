# 11. 사이드카 인젝션과 CNI 심화 분석

## 목차
1. [개요](#1-개요)
2. [Webhook 구조와 HTTP 핸들러](#2-webhook-구조와-http-핸들러)
3. [인젝션 정책 결정 (injectRequired)](#3-인젝션-정책-결정-injectrequired)
4. [템플릿 시스템](#4-템플릿-시스템)
5. [패치 생성 파이프라인](#5-패치-생성-파이프라인)
6. [수동 인젝션: istioctl kube-inject](#6-수동-인젝션-istioctl-kube-inject)
7. [CNI 플러그인](#7-cni-플러그인)
8. [iptables 규칙과 체인 구조](#8-iptables-규칙과-체인-구조)
9. [REDIRECT vs TPROXY 모드](#9-redirect-vs-tproxy-모드)
10. [nftables 대안](#10-nftables-대안)
11. [UID 기반 제외, DNS 캡처, 포트 제외](#11-uid-기반-제외-dns-캡처-포트-제외)
12. [전체 흐름 요약](#12-전체-흐름-요약)

---

## 1. 개요

Istio의 사이드카 인젝션은 애플리케이션 Pod에 Envoy 프록시 컨테이너를 자동으로 추가하는 메커니즘이다. 이 과정은 크게 두 단계로 나뉜다.

1. **컨트롤 플레인 단계 (Webhook)**: Kubernetes MutatingAdmissionWebhook이 Pod 생성 요청을 가로채서 istio-proxy 컨테이너와 istio-init 컨테이너를 Pod 스펙에 삽입한다.
2. **데이터 플레인 단계 (iptables/CNI)**: Pod 내부에서 네트워크 규칙을 설정하여 트래픽을 Envoy로 리다이렉트한다. 이는 init 컨테이너 또는 CNI 플러그인이 수행한다.

```
전체 흐름 개요
==============

                 kubectl apply -f deployment.yaml
                              |
                              v
                    +-------------------+
                    | Kubernetes API    |
                    | Server            |
                    +--------+----------+
                             |
                    MutatingAdmissionWebhook
                             |
                             v
               +----------------------------+
               | Istiod (Webhook Handler)   |
               |                            |
               | 1. injectRequired() 검사    |
               | 2. RunTemplate() 템플릿 렌더|
               | 3. reapplyOverwritten...   |
               | 4. postProcessPod()        |
               | 5. createPatch() JSON패치   |
               +-------------+--------------+
                             |
                     AdmissionResponse
                     (JSON Patch 반환)
                             |
                             v
               +----------------------------+
               | Pod 스펙에 반영:            |
               | - istio-proxy 컨테이너     |
               | - istio-init 컨테이너      |
               |   (또는 CNI가 대체)        |
               +-------------+--------------+
                             |
                    Pod 스케줄링 & 시작
                             |
              +--------------+---------------+
              |                              |
   +----------v----------+       +-----------v-----------+
   | istio-init (initC)  |       | istio-cni (CNI 모드)  |
   | iptables 규칙 설정   |       | CmdAdd() 호출         |
   +----------+----------+       | iptables 규칙 설정     |
              |                   +-----------+-----------+
              +----------+-------------------+
                         |
                         v
              +---------------------+
              | 트래픽 리다이렉션    |
              | iptables/nftables   |
              | 인바운드 -> :15006  |
              | 아웃바운드 -> :15001|
              +---------------------+
```

### 핵심 소스 파일

| 파일 | 역할 |
|------|------|
| `pkg/kube/inject/webhook.go` | Webhook 구조체, `serveInject()`, `inject()`, `injectPod()` |
| `pkg/kube/inject/inject.go` | `injectRequired()`, `InjectionPolicy`, `SidecarTemplateData`, `RunTemplate()` |
| `pkg/kube/inject/template.go` | `InjectionFuncmap`, 커스텀 템플릿 함수 |
| `cni/pkg/plugin/plugin.go` | CNI `CmdAdd()`, Pod 적격성 검사 |
| `cni/pkg/plugin/sidecar_redirect.go` | `Redirect` 구조체, annotation 기반 설정 |
| `tools/istio-iptables/pkg/capture/run.go` | iptables 규칙 생성 (`IptablesConfigurator.Run()`) |
| `tools/istio-iptables/pkg/builder/iptables_builder_impl.go` | 규칙 빌더 (`IptablesRuleBuilder`) |
| `tools/istio-nftables/pkg/capture/run.go` | nftables 대안 구현 |
| `tools/istio-nftables/pkg/builder/nftables_builder_impl.go` | nftables 규칙 빌더 |

---

## 2. Webhook 구조와 HTTP 핸들러

### 2.1 Webhook 구조체

Webhook은 Istiod 프로세스 내에서 실행되는 HTTP 핸들러로, Kubernetes API 서버의 MutatingAdmissionWebhook 요청을 처리한다.

```
소스: pkg/kube/inject/webhook.go

type Webhook struct {
    mu           sync.RWMutex
    Config       *Config
    meshConfig   *meshconfig.MeshConfig
    valuesConfig ValuesConfig
    namespaces   *multicluster.KclientComponent[*corev1.Namespace]
    nodes        *multicluster.KclientComponent[*corev1.Node]
    watcher      Watcher
    MultiCast    *WatcherMulticast
    env          *model.Environment
    revision     string
}
```

주요 필드 설명:

| 필드 | 역할 |
|------|------|
| `Config` | 인젝션 정책, 템플릿, 셀렉터 설정 (ConfigMap 기반) |
| `meshConfig` | MeshConfig - 프록시 설정, 인터셉션 모드 등 |
| `valuesConfig` | Helm values.yaml에서 파싱한 값들 |
| `namespaces` | OpenShift 환경에서 네임스페이스 접근 (UID 범위 확인) |
| `nodes` | Native Sidecar 지원 여부 감지를 위한 노드 정보 |
| `revision` | Istio revision (canary 배포 지원) |
| `watcher` | ConfigMap 변경 감시 |

### 2.2 Webhook 초기화 (`NewWebhook`)

```
소스: pkg/kube/inject/webhook.go (라인 202~247)

func NewWebhook(p WebhookParameters) (*Webhook, error) {
    wh := &Webhook{
        watcher:    p.Watcher,
        meshConfig: p.Env.Mesh(),
        env:        p.Env,
        revision:   p.Revision,
    }
    // OpenShift 환경이면 네임스페이스 클라이언트 구성
    // Native Sidecar 기능 활성화 시 노드 클라이언트 구성
    // ...
    mc := NewMulticast(p.Watcher, wh.GetConfig)
    mc.AddHandler(wh.updateConfig)
    wh.MultiCast = mc

    // 핵심: HTTP 핸들러 등록
    p.Mux.HandleFunc("/inject", wh.serveInject)
    p.Mux.HandleFunc("/inject/", wh.serveInject)
    // ...
}
```

`/inject`와 `/inject/` 두 가지 경로를 모두 등록한다. `/inject/` 뒤에 추가 경로 파라미터를 붙여 `cluster`나 `net` 값을 전달할 수 있다. 이는 멀티클러스터 환경에서 사용된다.

### 2.3 HTTP 핸들러 (`serveInject`)

```
소스: pkg/kube/inject/webhook.go (라인 1288~)

func (wh *Webhook) serveInject(w http.ResponseWriter, r *http.Request) {
    totalInjections.Increment()
    t0 := time.Now()
    defer func() { injectionTime.Record(time.Since(t0).Seconds()) }()

    // 1) Body 읽기
    var body []byte
    if r.Body != nil {
        if data, err := kube.HTTPConfigReader(r); err == nil {
            body = data
        }
    }

    // 2) Content-Type 검증 (application/json만 허용)
    contentType := r.Header.Get("Content-Type")
    if contentType != "application/json" { ... }

    // 3) AdmissionReview 디코딩 (v1, v1beta1 모두 지원)
    var obj runtime.Object
    if out, _, err := deserializer.Decode(body, nil, obj); err != nil {
        reviewResponse = toAdmissionResponse(err)
    } else {
        ar, err = kube.AdmissionReviewKubeToAdapter(out)
        reviewResponse = wh.inject(ar, path)
    }

    // 4) 응답 구성 및 반환
    response := kube.AdmissionReview{}
    response.Response = reviewResponse
    // ...
}
```

```
serveInject 처리 흐름
=====================

  K8s API Server
       |
       | POST /inject (AdmissionReview)
       v
  +--------------------+
  | serveInject()      |
  | 1. Body 읽기       |
  | 2. Content-Type    |
  |    검증             |
  | 3. Decode          |
  |    AdmissionReview |
  +--------+-----------+
           |
           v
  +--------------------+
  | wh.inject(ar,path) |
  | (정책검사, 패치생성)|
  +--------+-----------+
           |
           v
  +--------------------+
  | AdmissionResponse  |
  | {allowed, patch}   |
  +--------------------+
           |
           v
  K8s API Server
  (Pod 스펙에 패치 적용)
```

### 2.4 inject() 메서드

`inject()`는 실제 인젝션 로직의 진입점이다.

```
소스: pkg/kube/inject/webhook.go (라인 1104~)

func (wh *Webhook) inject(ar *kube.AdmissionReview, path string) *kube.AdmissionResponse {
    req := ar.Request
    var pod corev1.Pod
    json.Unmarshal(req.Object.Raw, &pod)

    // ManagedFields 제거 (CPU 최적화)
    pod.ManagedFields = nil

    // 정책 검사
    wh.mu.RLock()
    if !injectRequired(IgnoredNamespaces.UnsortedList(), wh.Config, &pod.Spec, pod.ObjectMeta) {
        totalSkippedInjections.Increment()
        wh.mu.RUnlock()
        return &kube.AdmissionResponse{Allowed: true}
    }

    // InjectionParameters 구성
    params := InjectionParameters{
        pod:             &pod,
        deployMeta:      deploy,
        templates:       wh.Config.Templates,
        defaultTemplate: wh.Config.DefaultTemplates,
        aliases:         wh.Config.Aliases,
        meshConfig:      wh.meshConfig,
        proxyConfig:     proxyConfig,
        valuesConfig:    wh.valuesConfig,
        revision:        wh.revision,
        proxyEnvs:       parseInjectEnvs(path),
    }

    // Native Sidecar 감지
    params.nativeSidecar = DetectNativeSidecar(nodes, pod.Spec.NodeName)

    wh.mu.RUnlock()

    // 패치 생성
    patchBytes, err := injectPod(params)
    // ...
}
```

`inject()` 내부에서 주목할 점:

1. **ManagedFields 제거**: `pod.ManagedFields = nil` -- Kubernetes가 자동 생성하는 관리 필드를 제거하여 패치 생성 시 CPU 오버헤드를 줄인다.
2. **URL 경로 파싱**: `parseInjectEnvs(path)`는 `/inject/cluster/foo/net/bar` 같은 경로에서 환경변수 맵을 추출한다. `URLParameterToEnv` 맵에 따라 `cluster` -> `ISTIO_META_CLUSTER_ID`, `net` -> `ISTIO_META_NETWORK`로 변환된다.
3. **OpenShift 특수 처리**: OpenShift는 모든 컨테이너에 동일한 RunAsUser를 자동 할당하는데, 이것이 사이드카 프록시와 앱 컨테이너에 동일하게 적용되면 트래픽 인터셉션이 실패한다. 따라서 동일한 경우 사이드카의 RunAsUser를 nil로 리셋한다.

---

## 3. 인젝션 정책 결정 (injectRequired)

`injectRequired()` 함수는 특정 Pod에 사이드카를 주입할지 결정하는 핵심 정책 로직이다.

```
소스: pkg/kube/inject/inject.go (라인 199~315)
```

### 3.1 결정 우선순위

```
인젝션 정책 결정 우선순위 (높은 것이 우선)
============================================

+-------------------------------------------+
| 1. Host Network 확인                       |
|    podSpec.HostNetwork == true → 건너뜀    |
+-------------------------------------------+
                    |
                    v
+-------------------------------------------+
| 2. 무시 네임스페이스 확인                   |
|    kube-system, kube-public 등 → 건너뜀    |
+-------------------------------------------+
                    |
                    v
+-------------------------------------------+
| 3. Label 확인 (최우선)                     |
|    sidecar.istio.io/inject: "true"/"false" |
|    (annotation보다 label이 우선)           |
+-------------------------------------------+
                    |
            값이 없으면 (useDefault=true)
                    |
                    v
+-------------------------------------------+
| 4. NeverInjectSelector 확인                |
|    Pod 라벨이 매칭 → inject=false          |
+-------------------------------------------+
                    |
            매칭 안 되면
                    |
                    v
+-------------------------------------------+
| 5. AlwaysInjectSelector 확인               |
|    Pod 라벨이 매칭 → inject=true           |
+-------------------------------------------+
                    |
            매칭 안 되면 (useDefault=true)
                    |
                    v
+-------------------------------------------+
| 6. 네임스페이스 정책 (Config.Policy)        |
|    InjectionPolicyEnabled → required=true  |
|    InjectionPolicyDisabled → required=false|
+-------------------------------------------+
```

### 3.2 코드 상세

```go
func injectRequired(ignored []string, config *Config, podSpec *corev1.PodSpec,
    metadata metav1.ObjectMeta) bool {

    // (1) Host Network → 즉시 false
    if podSpec.HostNetwork {
        return false
    }

    // (2) 무시 네임스페이스 → 즉시 false
    for _, namespace := range ignored {
        if metadata.Namespace == namespace {
            return false
        }
    }

    // (3) annotation과 label 확인
    annos := metadata.GetAnnotations()
    objectSelector := annos[annotation.SidecarInject.Name]
    // label이 있으면 label 값이 우선
    if lbl, labelPresent := metadata.GetLabels()[label.SidecarInject.Name]; labelPresent {
        objectSelector = lbl
    }

    var useDefault bool
    var inject bool
    switch objectSelector {
    case "true":   inject = true
    case "false":  inject = false
    case "":       useDefault = true
    default:       useDefault = true  // 잘못된 값은 기본값 사용
    }

    // (4) NeverInjectSelector
    if useDefault {
        for _, neverSelector := range config.NeverInjectSelector {
            selector, _ := metav1.LabelSelectorAsSelector(&neverSelector)
            if selector.Matches(labels.Set(metadata.Labels)) {
                inject = false
                useDefault = false
                break
            }
        }
    }

    // (5) AlwaysInjectSelector
    if useDefault {
        for _, alwaysSelector := range config.AlwaysInjectSelector {
            selector, _ := metav1.LabelSelectorAsSelector(&alwaysSelector)
            if selector.Matches(labels.Set(metadata.Labels)) {
                inject = true
                useDefault = false
                break
            }
        }
    }

    // (6) 네임스페이스 정책
    switch config.Policy {
    case InjectionPolicyDisabled:
        if useDefault { required = false } else { required = inject }
    case InjectionPolicyEnabled:
        if useDefault { required = true } else { required = inject }
    }

    return required
}
```

### 3.3 InjectionPolicy 열거형

```
소스: pkg/kube/inject/inject.go (라인 59~75)

type InjectionPolicy string

const (
    InjectionPolicyDisabled InjectionPolicy = "disabled"
    InjectionPolicyEnabled  InjectionPolicy = "enabled"
)
```

| 정책 | useDefault=true | 명시적 "true" | 명시적 "false" |
|------|----------------|--------------|---------------|
| `enabled` | 인젝션 O | 인젝션 O | 인젝션 X |
| `disabled` | 인젝션 X | 인젝션 O | 인젝션 X |

### 3.4 왜 label이 annotation보다 우선인가?

코드에서 `label.SidecarInject.Name`이 존재하면 `objectSelector`를 덮어쓴다. 이는 Istio가 점진적으로 label 기반 API로 전환하고 있기 때문이다. label은 Kubernetes의 네이티브 리소스 선택 메커니즘과 더 잘 통합되며, MutatingWebhookConfiguration의 `objectSelector`에서도 활용 가능하다.

---

## 4. 템플릿 시스템

### 4.1 SidecarTemplateData 구조체

템플릿 렌더링에 전달되는 데이터 구조체이다.

```
소스: pkg/kube/inject/inject.go (라인 103~118)

type SidecarTemplateData struct {
    TypeMeta                 metav1.TypeMeta
    DeploymentMeta           types.NamespacedName
    ObjectMeta               metav1.ObjectMeta
    Spec                     corev1.PodSpec
    ProxyConfig              *meshconfig.ProxyConfig
    MeshConfig               *meshconfig.MeshConfig
    Values                   map[string]any
    Revision                 string
    NativeSidecars           bool
    ProxyImage               string
    ProxyUID                 int64
    ProxyGID                 int64
    InboundTrafficPolicyMode string
    CompliancePolicy         string
}
```

```
SidecarTemplateData 의존 관계
==============================

  InjectionParameters
         |
         |  RunTemplate() 내부에서 조립
         |
         v
  SidecarTemplateData
  +----------------------------------+
  | TypeMeta       ← params.typeMeta |
  | DeploymentMeta ← params.deploy   |
  | ObjectMeta     ← strippedPod     |
  | Spec           ← strippedPod     |
  | ProxyConfig    ← params.proxy    |
  | MeshConfig     ← params.mesh     |
  | Values         ← valuesConfig    |
  | Revision       ← params.revision |
  | NativeSidecars ← 기능 플래그      |
  | ProxyImage     ← ProxyImage()    |
  | ProxyUID/GID   ← GetProxyIDs()   |
  +----------------------------------+
         |
         |  template.Execute()
         v
  렌더링된 Pod YAML 조각
```

### 4.2 Config 구조체와 템플릿 관리

```
소스: pkg/kube/inject/inject.go (라인 134~166)

type Config struct {
    Policy               InjectionPolicy
    DefaultTemplates     []string
    RawTemplates         RawTemplates         // map[string]string
    Aliases              map[string][]string
    NeverInjectSelector  []metav1.LabelSelector
    AlwaysInjectSelector []metav1.LabelSelector
    InjectedAnnotations  map[string]string
    Templates            Templates            // map[string]*template.Template
}
```

`Aliases`는 템플릿 이름의 별칭을 정의한다. 예를 들어 `sidecar: [proxy, init]`으로 설정하면 "sidecar"라는 이름으로 "proxy"와 "init" 두 템플릿을 한 번에 참조할 수 있다.

### 4.3 템플릿 파싱 (`ParseTemplates`)

```
소스: pkg/kube/inject/webhook.go (라인 346~356)

func ParseTemplates(tmpls RawTemplates) (Templates, error) {
    ret := make(Templates, len(tmpls))
    for k, t := range tmpls {
        p, err := parseDryTemplate(t, InjectionFuncmap)
        if err != nil {
            return nil, err
        }
        ret[k] = p
    }
    return ret, nil
}
```

`parseDryTemplate()`는 Go의 `text/template` 패키지를 사용하며, Sprig 함수(`sprig.TxtFuncMap()`)와 Istio 커스텀 함수(`InjectionFuncmap`)를 모두 등록한다.

### 4.4 InjectionFuncmap -- 커스텀 템플릿 함수

```
소스: pkg/kube/inject/template.go (라인 41~70)

var InjectionFuncmap = createInjectionFuncmap()

func createInjectionFuncmap() template.FuncMap {
    return template.FuncMap{
        "formatDuration":         formatDuration,
        "isset":                  isset,
        "excludeInboundPort":     excludeInboundPort,
        "includeInboundPorts":    includeInboundPorts,
        "kubevirtInterfaces":     kubevirtInterfaces,
        "excludeInterfaces":      excludeInterfaces,
        "applicationPorts":       applicationPorts,
        "annotation":             getAnnotation,
        "valueOrDefault":         valueOrDefault,
        "toJSON":                 toJSON,
        "fromJSON":               fromJSON,
        "structToJSON":           structToJSON,
        "protoToJSON":            protoToJSON,
        "toYaml":                 toYaml,
        "indent":                 indent,
        "directory":              directory,
        "contains":               flippedContains,
        "toLower":                strings.ToLower,
        "appendMultusNetwork":    appendMultusNetwork,
        "env":                    env,
        "omit":                   omit,
        "strdict":                strdict,
        "toJsonMap":              toJSONMap,
        "mergeMaps":              mergeMaps,
        "omitNil":                omitNil,
        "otelResourceAttributes": otelResourceAttributes,
    }
}
```

주요 함수 설명:

| 함수 | 역할 | 사용 예 |
|------|------|---------|
| `excludeInboundPort` | 제외 포트 목록에 포트 추가 | readiness probe 포트 제외 |
| `includeInboundPorts` | 모든 컨테이너의 포트 수집 | 인바운드 캡처 대상 |
| `annotation` | Pod annotation 값 조회 | `{{ annotation .ObjectMeta "..." "default" }}` |
| `protoToJSON` | protobuf를 JSON으로 변환 | ProxyConfig 직렬화 |
| `appendMultusNetwork` | Multus CNI 네트워크 추가 | 멀티 네트워크 환경 |
| `env` | Istiod 환경변수 접근 | `{{ env "KEY" "default" }}` |
| `omit` | 맵에서 특정 키 제외 | annotation 필터링 |
| `strdict` | 문자열 키-값 쌍으로 맵 생성 | annotation/label 구성 |
| `otelResourceAttributes` | OTel 리소스 속성 계산 | OpenTelemetry 통합 |

### 4.5 ProxyConfig 정리 (`cleanProxyConfig`)

`protoToJSON` 내부에서 호출되는 `cleanProxyConfig()`는 기본값과 동일한 필드를 제거하여 Pod 스펙의 크기를 줄인다.

```
소스: pkg/kube/inject/template.go (라인 300~359)

func cleanProxyConfig(msg proto.Message) proto.Message {
    pc := protomarshal.Clone(originalProxyConfig)
    defaults := mesh.DefaultProxyConfig()
    // 각 필드를 기본값과 비교하여 동일하면 제거
    if pc.ConfigPath == defaults.ConfigPath { pc.ConfigPath = "" }
    if pc.BinaryPath == defaults.BinaryPath { pc.BinaryPath = "" }
    if pc.DiscoveryAddress == defaults.DiscoveryAddress { pc.DiscoveryAddress = "" }
    // ... 기타 필드
    return proto.Message(pc)
}
```

이 최적화는 etcd에 저장되는 Pod 오브젝트 크기를 줄이고, 네트워크 전송 및 메모리 사용량을 최소화하기 위한 것이다.

---

## 5. 패치 생성 파이프라인

### 5.1 injectPod() -- 전체 파이프라인

```
소스: pkg/kube/inject/webhook.go (라인 463~495)

func injectPod(req InjectionParameters) ([]byte, error) {
    checkPreconditions(req)

    // 1. 원본 Pod 상태 캡처
    originalPodSpec, _ := json.Marshal(req.pod)

    // 2. 템플릿 실행
    mergedPod, injectedPodData, _ := RunTemplate(req)

    // 3. 사용자 오버라이드 재적용
    mergedPod, _ = reapplyOverwrittenContainers(mergedPod, req.pod,
                                                 injectedPodData, req.proxyConfig)

    // 4. 후처리
    postProcessPod(mergedPod, *injectedPodData, req)

    // 5. JSON 패치 생성
    patch, _ := createPatch(mergedPod, originalPodSpec)

    return patch, nil
}
```

```
injectPod() 파이프라인
======================

  originalPod (JSON 직렬화)
       |
       v
  RunTemplate()
  +------------------------------------------+
  | 1. stripPod() - 기존 인젝션 제거          |
  | 2. reinsertOverrides() - 오버라이드 복원   |
  | 3. SidecarTemplateData 조립               |
  | 4. selectTemplates() - 템플릿 이름 결정    |
  | 5. template.Execute() - Go 템플릿 렌더링   |
  | 6. applyOverlayYAML() - 전략적 머지       |
  +------------------------------------------+
       |
       v  (mergedPod, templatePod)
       |
  reapplyOverwrittenContainers()
  +------------------------------------------+
  | 사용자가 istio-proxy 컨테이너를 직접       |
  | 정의한 경우, 해당 설정을 다시 적용          |
  | (재인젝션이 아닌 경우에만)                 |
  +------------------------------------------+
       |
       v
  postProcessPod()
  +------------------------------------------+
  | 1. overwriteClusterInfo() - 클러스터 정보  |
  | 2. applyPrometheusMerge() - 메트릭 설정    |
  | 3. applyRewrite() - 이미지/주석 재작성     |
  | 4. applyMetadata() - sidecar status 주석   |
  | 5. reorderPod() - 컨테이너 순서 조정       |
  +------------------------------------------+
       |
       v
  createPatch()
  +------------------------------------------+
  | jsonpatch.CreatePatch(original, modified) |
  | → JSON Patch (RFC 6902) 생성              |
  +------------------------------------------+
       |
       v
  []byte (JSON Patch)
```

### 5.2 RunTemplate() 상세

```
소스: pkg/kube/inject/inject.go (라인 430~519)

func RunTemplate(params InjectionParameters) (mergedPod, templatePod *corev1.Pod, err error) {
    // annotation 유효성 검사
    validateAnnotations(metadata.GetAnnotations())

    // 클러스터/네트워크 정보 추출
    cluster, network := extractClusterAndNetwork(params)

    // 기존 인젝션 제거 (멱등성 보장)
    strippedPod, _ := reinsertOverrides(stripPod(params))

    // 프록시 UID/GID 결정
    proxyUID, proxyGID := GetProxyIDs(params.namespace)

    // 템플릿 데이터 조립
    data := SidecarTemplateData{
        TypeMeta:       params.typeMeta,
        DeploymentMeta: params.deployMeta,
        ObjectMeta:     strippedPod.ObjectMeta,
        Spec:           strippedPod.Spec,
        ProxyConfig:    params.proxyConfig,
        MeshConfig:     meshConfig,
        Values:         params.valuesConfig.asMap,
        Revision:       params.revision,
        ProxyImage:     ProxyImage(params.valuesConfig.asStruct, ...),
        NativeSidecars: params.nativeSidecar,
        ProxyUID:       proxyUID,
        ProxyGID:       proxyGID,
        // ...
    }

    mergedPod = params.pod
    templatePod = &corev1.Pod{}

    // 각 선택된 템플릿에 대해 반복
    for _, templateName := range selectTemplates(params) {
        parsedTemplate := params.templates[templateName]
        bbuf, _ := runTemplate(parsedTemplate, data)

        templatePod, _ = applyOverlayYAML(templatePod, bbuf.Bytes())

        // Native Sidecar 처리: initContainers와 containers 간 이동
        if native && ... {
            mergedPod.Spec.Containers, mergedPod.Spec.InitContainers =
                moveContainer(...)
        }

        mergedPod, _ = applyOverlayYAML(mergedPod, bbuf.Bytes())
    }

    return mergedPod, templatePod, nil
}
```

### 5.3 selectTemplates() -- 템플릿 선택

```
소스: pkg/kube/inject/inject.go (라인 529~551)

func selectTemplates(params InjectionParameters) []string {
    // 1. Pod에 inject.istio.io/templates annotation이 있으면 그 값 사용
    if a, f := params.pod.Annotations[annotation.InjectTemplates.Name]; f {
        names := strings.Split(a, ",")
        return resolveAliases(params, names)
    }
    // 2. 없으면 기본 템플릿 사용
    return resolveAliases(params, params.defaultTemplate)
}
```

`resolveAliases()`는 Config의 `Aliases` 맵을 통해 이름을 확장한다. 예를 들어 annotation에 `sidecar`를 지정하고 aliases에 `sidecar: [proxy, init]`이 정의되어 있으면, 실제로 `proxy`와 `init` 두 템플릿이 순서대로 실행된다.

### 5.4 stripPod() -- 멱등성 보장

```
소스: pkg/kube/inject/inject.go (라인 553~579)

func stripPod(req InjectionParameters) *corev1.Pod {
    pod := req.pod.DeepCopy()
    prevStatus := injectionStatus(pod)
    if prevStatus == nil {
        return req.pod  // 이전 인젝션 없음
    }
    // 이전에 인젝션된 컨테이너 제거
    for _, c := range prevStatus.Containers {
        pod.Spec.Containers = modifyContainers(pod.Spec.Containers, c, Remove)
    }
    for _, c := range prevStatus.InitContainers {
        pod.Spec.InitContainers = modifyContainers(pod.Spec.InitContainers, c, Remove)
    }
    // ...
    return pod
}
```

`sidecar.istio.io/status` annotation에 기록된 이전 인젝션 상태를 참조하여 이미 주입된 컨테이너를 제거한 뒤 재인젝션한다. 이는 재인젝션 시 컨테이너가 중복 추가되는 것을 방지한다.

### 5.5 postProcessPod()

```
소스: pkg/kube/inject/webhook.go (라인 740~765)

func postProcessPod(pod *corev1.Pod, injectedPod corev1.Pod, req InjectionParameters) error {
    overwriteClusterInfo(pod, req)       // 클러스터/네트워크 환경변수 업데이트
    applyPrometheusMerge(pod, req.meshConfig)  // Prometheus 스크래핑 설정
    applyRewrite(pod, req)               // 이미지 URL, annotation 재작성
    applyMetadata(pod, injectedPod, req) // sidecar status annotation 추가
    reorderPod(pod, req)                 // 컨테이너 순서 조정
    return nil
}
```

`reorderPod()`는 `holdApplicationUntilProxyStarts` 설정에 따라 istio-proxy 컨테이너를 맨 앞(`MoveFirst`) 또는 맨 뒤(`MoveLast`)로 배치한다. `MoveFirst`로 설정하면 프록시가 먼저 시작되어 애플리케이션이 네트워크를 사용할 때 프록시가 준비되어 있도록 보장한다.

### 5.6 createPatch()

```
소스: pkg/kube/inject/webhook.go (라인 726~736)

func createPatch(pod *corev1.Pod, original []byte) ([]byte, error) {
    reinjected, _ := json.Marshal(pod)
    p, _ := jsonpatch.CreatePatch(original, reinjected)
    return json.Marshal(p)
}
```

RFC 6902 JSON Patch를 생성하여 Kubernetes API 서버에 반환한다. API 서버는 이 패치를 원본 Pod 스펙에 적용한다.

---

## 6. 수동 인젝션: istioctl kube-inject

### 6.1 IntoResourceFile()

`istioctl kube-inject`는 CLI에서 수동으로 사이드카를 주입하는 방법이다.

```
소스: pkg/kube/inject/inject.go (라인 624~663)

func IntoResourceFile(injector Injector, sidecarTemplate Templates,
    valuesConfig ValuesConfig, revision string, meshconfig *meshconfig.MeshConfig,
    in io.Reader, out io.Writer, warningHandler func(string)) error {

    reader := yamlDecoder.NewYAMLReader(bufio.NewReaderSize(in, 4096))
    for {
        raw, err := reader.Read()
        if err == io.EOF { break }

        obj, err := FromRawToObject(raw)
        if err == nil {
            outObject, _ := IntoObject(injector, sidecarTemplate, ...)
            updated, _ = yaml.Marshal(outObject)
        }
        out.Write(updated)
        fmt.Fprint(out, "---\n")
    }
    return nil
}
```

### 6.2 IntoObject()

```
소스: pkg/kube/inject/inject.go (라인 686~874)

func IntoObject(...) (any, error) {
    // 리소스 타입별 처리:
    // - CronJob: JobTemplate 내의 PodSpec
    // - Pod: 직접 PodSpec
    // - Deployment: Template 내의 PodSpec
    // - 기타: 리플렉션으로 Spec.Template.Spec 접근

    // Host Network 검사
    if podSpec.HostNetwork { return out, nil }

    // injector가 있으면 injector.Inject() 사용
    // 없으면 직접 injectPod() 호출

    // JSON 패치를 적용하여 최종 결과 반환
    patched, _ := applyJSONPatchToPod(pod, patchBytes)
    // ...
}
```

```
수동 인젝션 vs 자동 인젝션 비교
================================

  수동 인젝션 (istioctl kube-inject)
  +-----------------------------------+
  | YAML 파일 읽기                     |
  | → FromRawToObject()               |
  | → IntoObject()                    |
  |   → injectRequired() (enabled)    |
  |   → injectPod()                   |
  |   → applyJSONPatchToPod()        |
  | → YAML 출력                       |
  +-----------------------------------+

  자동 인젝션 (MutatingWebhook)
  +-----------------------------------+
  | AdmissionReview 수신               |
  | → inject()                        |
  |   → injectRequired()             |
  |   → injectPod()                  |
  | → AdmissionResponse              |
  |   (JSON Patch만 반환,             |
  |    적용은 API 서버가 수행)         |
  +-----------------------------------+
```

차이점: 수동 인젝션은 항상 `InjectionPolicyEnabled`로 동작하며, 결과가 직접 YAML로 출력된다. 자동 인젝션은 ConfigMap의 정책 설정을 따르며, JSON Patch만 반환한다.

---

## 7. CNI 플러그인

### 7.1 왜 CNI 플러그인이 필요한가?

기본적으로 Istio는 `istio-init` 컨테이너(init container)로 iptables 규칙을 설정한다. 이 방식은 `NET_ADMIN`과 `NET_RAW` capability가 필요하다. 보안이 엄격한 환경(PodSecurityPolicy, OpenShift 등)에서는 이러한 권한이 제한될 수 있다.

Istio CNI 플러그인은 노드 레벨의 DaemonSet으로 배포되어, init 컨테이너 대신 CNI 체인에서 iptables 규칙을 설정한다. 이를 통해 Pod에 `NET_ADMIN` 권한이 필요 없게 된다.

```
init 컨테이너 방식 vs CNI 방식
================================

  [init 컨테이너 방식]
  Pod 시작
    → istio-init (initContainer)
       - NET_ADMIN capability 필요
       - iptables 규칙 설정
       - 완료 후 종료
    → istio-proxy (sidecar)
    → app (애플리케이션)

  [CNI 방식]
  Pod 네트워크 네임스페이스 생성
    → kubelet이 CNI 체인 호출
       → ... (기존 CNI 플러그인들)
       → istio-cni (체인된 CNI 플러그인)
          - 노드 레벨 권한으로 실행
          - Pod의 netns에서 iptables 규칙 설정
          - Pod에는 NET_ADMIN 불필요
    → Pod 컨테이너 시작
       → istio-proxy (sidecar, NET_ADMIN 없이)
       → app (애플리케이션)
```

### 7.2 CmdAdd() -- CNI 플러그인 진입점

```
소스: cni/pkg/plugin/plugin.go (라인 146~202)

func CmdAdd(args *skel.CmdArgs) (err error) {
    defer func() {
        if e := recover(); e != nil {
            // 패닉 복구 - CNI 플러그인이 패닉하면 Pod가 시작 못함
            err = errors.New(msg)
        }
    }()

    conf, _ := parseConfig(args.StdinData)

    // Ambient 모드: CNI 자체 Pod인 경우 건너뜀 (데드락 방지)
    if conf.AmbientEnabled {
        k8sArgs := K8sArgs{}
        types.LoadArgs(args.Args, &k8sArgs)
        if isCNIPod(conf, &k8sArgs) {
            return pluginResponse(conf)
        }
    }

    // K8s 클라이언트 생성
    client, _ := newK8sClient(*conf)

    // 규칙 매니저 선택
    mgr := IptablesInterceptRuleMgr()
    if conf.NativeNftables {
        mgr = NftablesInterceptRuleMgr()
    }

    // 실제 규칙 설정
    doAddRun(args, conf, client, mgr)
    return pluginResponse(conf)
}
```

### 7.3 doAddRun() -- Pod 적격성 검사

```
소스: cni/pkg/plugin/plugin.go (라인 204~371)

func doAddRun(args *skel.CmdArgs, conf *Config, kClient kubernetes.Interface,
    rulesMgr InterceptRuleMgr) error {

    // CNI 인자에서 Pod 정보 추출
    k8sArgs := K8sArgs{}
    types.LoadArgs(args.Args, &k8sArgs)
    podNamespace := string(k8sArgs.K8S_POD_NAMESPACE)
    podName := string(k8sArgs.K8S_POD_NAME)

    // (1) 제외 네임스페이스 확인
    for _, excludeNs := range conf.ExcludeNamespaces {
        if podNamespace == excludeNs { return nil }
    }

    // (2) Ambient 모드 처리 (별도 경로)
    if conf.AmbientEnabled { ... }

    // (3) K8s API에서 Pod 정보 조회 (최대 30회 재시도)
    for attempt := 1; attempt <= podRetrievalMaxRetries; attempt++ {
        pi, k8sErr = getK8sPodInfo(kClient, podName, podNamespace)
        if k8sErr == nil { break }
        // 데드락 방지: API 접근 실패 시 CNI Pod 자체는 통과
        if isCNIPod(conf, &k8sArgs) { return nil }
        time.Sleep(podRetrievalInterval)
    }

    // (4) istio-init 컨테이너가 있으면 제외 (이미 설정됨)
    if pi.Containers.Contains(ISTIOINIT) { return nil }

    // (5) DISABLE_ENVOY 환경변수 확인
    if val, ok := pi.ProxyEnvironments["DISABLE_ENVOY"]; ok {
        if val, _ := strconv.ParseBool(val); val { return nil }
    }

    // (6) istio-proxy 컨테이너가 없으면 제외
    if !pi.Containers.Contains(ISTIOPROXY) { return nil }

    // (7) 프록시 타입이 sidecar가 아니면 제외 (gateway 등)
    if pi.ProxyType != "" && pi.ProxyType != "sidecar" { return nil }

    // (8) 인젝션 비활성화 label/annotation 확인
    val := pi.Annotations[injectAnnotationKey]
    if lbl, labelPresent := pi.Labels[label.SidecarInject.Name]; labelPresent {
        val = lbl
    }
    if injectEnabled, _ := strconv.ParseBool(val); !injectEnabled { return nil }

    // (9) sidecar status annotation 필수
    if _, ok := pi.Annotations[sidecarStatusKey]; !ok { return nil }

    // (10) Redirect 설정 생성 및 규칙 적용
    redirect, _ := NewRedirect(pi)
    rulesMgr.Program(podName, args.Netns, redirect)
    return nil
}
```

```
CNI Pod 적격성 검사 흐름도
===========================

  CmdAdd(args)
       |
       v
  parseConfig(stdin)
       |
       v
  [Ambient 모드?] ---yes---> Ambient 경로
       |no
       v
  newK8sClient()
       |
       v
  doAddRun()
       |
  +----+----+----+----+----+----+----+----+----+
  |    |    |    |    |    |    |    |    |    |
  v    v    v    v    v    v    v    v    v    v
 (1)  (2)  (3)  (4)  (5)  (6)  (7)  (8)  (9)  (10)
제외  Ambi  API  init DISA  no   not  inj  no   Redirect
NS   ent  조회  있음 BLE   proxy side  off  stat 설정
                     ENVY       car       us   + 규칙 적용

  각 검사에서 조건 불충족 → return nil (규칙 설정 안 함)
  모든 검사 통과 → NewRedirect() → rulesMgr.Program()
```

### 7.4 PodInfo 구조체

```
소스: cni/pkg/plugin/kubernetes.go (라인 33~41)

type PodInfo struct {
    Containers        sets.String
    Labels            map[string]string
    Annotations       map[string]string
    ProxyType         string
    ProxyEnvironments map[string]string
    ProxyUID          *int64
    ProxyGID          *int64
}
```

### 7.5 CNI Config 구조체

```
소스: cni/pkg/plugin/plugin.go (라인 64~76)

type Config struct {
    types.NetConf
    PluginLogLevel              string
    CNIAgentRunDir              string
    AmbientEnabled              bool
    EnablementSelectors         []util.EnablementSelector
    ExcludeNamespaces           []string
    PodNamespace                string
    NativeNftables              bool
    EnableAmbientDetectionRetry bool
}
```

`NativeNftables` 필드가 `true`이면 iptables 대신 nftables를 사용한다.

### 7.6 InterceptRuleMgr 인터페이스

```
소스: cni/pkg/plugin/sidecar_intercept_rule_mgr.go (라인 19~31)

type InterceptRuleMgr interface {
    Program(podName, netns string, redirect *Redirect) error
}

func IptablesInterceptRuleMgr() InterceptRuleMgr {
    return newIPTables()
}

func NftablesInterceptRuleMgr() InterceptRuleMgr {
    return newNFTables()
}
```

전략 패턴을 사용하여 iptables와 nftables 구현체를 교체 가능하게 설계했다. `Config.NativeNftables` 값에 따라 적절한 구현체가 선택된다.

---

## 8. iptables 규칙과 체인 구조

### 8.1 Istio 커스텀 체인 정의

```
소스: tools/istio-iptables/pkg/constants/constants.go (라인 46~55)

const (
    ISTIOOUTPUT     = "ISTIO_OUTPUT"
    ISTIOOUTPUTDNS  = "ISTIO_OUTPUT_DNS"
    ISTIOINBOUND    = "ISTIO_INBOUND"
    ISTIODIVERT     = "ISTIO_DIVERT"
    ISTIOTPROXY     = "ISTIO_TPROXY"
    ISTIOREDIRECT   = "ISTIO_REDIRECT"
    ISTIOINREDIRECT = "ISTIO_IN_REDIRECT"
    ISTIODROP       = "ISTIO_DROP"
)
```

### 8.2 iptables 체인 관계도

```
iptables 체인 구조 (REDIRECT 모드, nat 테이블)
=================================================

  [인바운드 트래픽]

  PREROUTING (nat)
       |
       +---> ISTIO_INBOUND
                |
                +--- tcp/dport=15008 (tunnel) → RETURN
                |
                +--- tcp/dport=<제외포트> → RETURN
                |
                +---> ISTIO_IN_REDIRECT
                         |
                         +--- tcp → REDIRECT --to-ports 15006
                                    (Envoy inbound listener)


  [아웃바운드 트래픽]

  OUTPUT (nat)
       |
       +---> ISTIO_OUTPUT
                |
                +--- tcp/dport=<제외포트> → RETURN
                |
                +--- uid-owner 1337 (loopback, !dst localhost)
                |        → ISTIO_IN_REDIRECT (자기 자신 호출)
                |
                +--- !uid-owner 1337 (loopback) → RETURN
                |
                +--- uid-owner 1337 → RETURN (무한루프 방지)
                |
                +--- gid-owner 1337 처리 (uid와 동일 패턴)
                |
                +--- dst=localhost → RETURN
                |
                +--- dst=<제외 CIDR> → RETURN
                |
                +---> ISTIO_REDIRECT
                         |
                         +--- tcp → REDIRECT --to-ports 15001
                                    (Envoy outbound listener)


  ISTIO_REDIRECT (nat):
       -p tcp -j REDIRECT --to-ports 15001

  ISTIO_IN_REDIRECT (nat):
       -p tcp -j REDIRECT --to-ports 15006
```

### 8.3 IptablesConfigurator.Run() -- 규칙 생성 흐름

```
소스: tools/istio-iptables/pkg/capture/run.go (라인 206~448)

func (cfg *IptablesConfigurator) Run() error {
    // IP 범위 분리 (v4/v6)
    ipv4RangesExclude, ipv6RangesExclude, _ := config.SeparateV4V6(cfg.cfg.OutboundIPRangesExclude)
    ipv4RangesInclude, ipv6RangesInclude, _ := config.SeparateV4V6(cfg.cfg.OutboundIPRangesInclude)

    // 1. 인터페이스 제외 규칙
    cfg.shortCircuitExcludeInterfaces()
    cfg.shortCircuitKubeInternalInterface()

    // 2. Invalid 패킷 DROP (선택적)
    if dropInvalid {
        cfg.ruleBuilder.AppendRule("PREROUTING", "mangle",
            "-m", "conntrack", "--ctstate", "INVALID", "-j", constants.ISTIODROP)
    }

    // 3. 터널 포트 RETURN
    cfg.ruleBuilder.AppendRule(constants.ISTIOINBOUND, "nat",
        "-p", "tcp", "--dport", cfg.cfg.InboundTunnelPort, "-j", "RETURN")

    // 4. ISTIO_REDIRECT 체인 정의 (아웃바운드 리다이렉트)
    cfg.ruleBuilder.AppendRule(constants.ISTIOREDIRECT, "nat",
        "-p", "tcp", "-j", "REDIRECT", "--to-ports", cfg.cfg.ProxyPort)

    // 5. ISTIO_IN_REDIRECT 체인 정의 (인바운드 리다이렉트)
    cfg.ruleBuilder.AppendRule(constants.ISTIOINREDIRECT, "nat",
        "-p", "tcp", "-j", "REDIRECT", "--to-ports", cfg.cfg.InboundCapturePort)

    // 6. 인바운드 포트 처리
    cfg.handleInboundPortsInclude()

    // 7. OUTPUT → ISTIO_OUTPUT 점프
    cfg.ruleBuilder.AppendRule("OUTPUT", "nat", "-j", constants.ISTIOOUTPUT)

    // 8. 아웃바운드 포트 제외
    for _, port := range config.Split(cfg.cfg.OutboundPortsExclude) {
        cfg.ruleBuilder.AppendRule(constants.ISTIOOUTPUT, "nat",
            "-p", "tcp", "--dport", port, "-j", "RETURN")
    }

    // 9. UID 기반 라우팅 규칙 (ProxyUID)
    for _, uid := range config.Split(cfg.cfg.ProxyUID) {
        // loopback을 통한 자기 호출 → ISTIO_IN_REDIRECT
        // loopback의 비-프록시 트래픽 → RETURN
        // 프록시 트래픽 → RETURN (무한루프 방지)
    }

    // 10. GID 기반 라우팅 규칙 (동일 패턴)
    // 11. DNS 리다이렉트 (선택적)
    // 12. localhost 트래픽 RETURN
    // 13. 아웃바운드 IP 제외/포함 규칙
    // 14. TPROXY 모드 추가 규칙 (mangle 테이블)

    return cfg.executeCommands(&cfg.iptV, &cfg.ipt6V)
}
```

### 8.4 IptablesRuleBuilder 상세

```
소스: tools/istio-iptables/pkg/builder/iptables_builder_impl.go

type Rule struct {
    chain  string    // 체인 이름 (ISTIO_OUTPUT 등)
    table  string    // 테이블 (nat, mangle, filter, raw)
    params []string  // 규칙 파라미터
}

type Rules struct {
    rulesv4 []Rule   // IPv4 규칙
    rulesv6 []Rule   // IPv6 규칙
}

type IptablesRuleBuilder struct {
    rules Rules
    cfg   *config.Config
}
```

빌더는 두 가지 출력 형식을 지원한다:

1. **개별 명령어 실행** (`BuildV4()`, `BuildV6()`): 각 규칙을 별도의 iptables 명령으로 실행
2. **iptables-restore 형식** (`BuildV4Restore()`, `BuildV6Restore()`): 모든 규칙을 한 번에 원자적으로 적용

```
iptables-restore 출력 예시
===========================

* nat
-N ISTIO_REDIRECT
-N ISTIO_IN_REDIRECT
-N ISTIO_INBOUND
-N ISTIO_OUTPUT
-A ISTIO_REDIRECT -p tcp -j REDIRECT --to-ports 15001
-A ISTIO_IN_REDIRECT -p tcp -j REDIRECT --to-ports 15006
-A PREROUTING -p tcp -j ISTIO_INBOUND
-A ISTIO_INBOUND -p tcp --dport 15008 -j RETURN
-A ISTIO_INBOUND -p tcp -j ISTIO_IN_REDIRECT
-A OUTPUT -j ISTIO_OUTPUT
-A ISTIO_OUTPUT -m owner --uid-owner 1337 -j RETURN
-A ISTIO_OUTPUT -j ISTIO_REDIRECT
COMMIT
```

### 8.5 `AppendVersionedRule` -- 듀얼 스택 지원

```
소스: tools/istio-iptables/pkg/builder/iptables_builder_impl.go (라인 386~401)

func (rb *IptablesRuleBuilder) AppendVersionedRule(ipv4, ipv6, chain, table string,
    params ...string) {
    rb.AppendRuleV4(chain, table, replaceVersionSpecific(ipv4, params...)...)
    rb.AppendRuleV6(chain, table, replaceVersionSpecific(ipv6, params...)...)
}
```

`constants.IPVersionSpecific` 플레이스홀더가 파라미터에 있으면, IPv4에서는 `ipv4` 값으로, IPv6에서는 `ipv6` 값으로 대체한다. 예를 들어 loopback 주소를 `127.0.0.1/32`(IPv4)와 `::1/128`(IPv6)로 분기할 때 사용한다.

---

## 9. REDIRECT vs TPROXY 모드

### 9.1 Redirect 구조체

```
소스: cni/pkg/plugin/sidecar_redirect.go (라인 80~96)

type Redirect struct {
    targetPort               string  // 기본 "15001"
    redirectMode             string  // "REDIRECT" 또는 "TPROXY"
    noRedirectUID            string  // 기본 "1337"
    noRedirectGID            string  // 기본 "1337"
    includeIPCidrs           string  // 기본 "*"
    excludeIPCidrs           string
    excludeInboundPorts      string  // 기본 "15020" + 15020,15021,15090 추가
    excludeOutboundPorts     string
    includeInboundPorts      string  // 기본 "*"
    includeOutboundPorts     string
    rerouteVirtualInterfaces string
    excludeInterfaces        string
    dnsRedirect              bool    // ISTIO_META_DNS_CAPTURE 환경변수
    dualStack                bool    // ISTIO_DUAL_STACK 환경변수
    invalidDrop              bool    // INVALID_DROP 환경변수
}
```

### 9.2 REDIRECT 모드

REDIRECT는 `nat` 테이블의 `REDIRECT` 타겟을 사용한다.

```
REDIRECT 모드 동작
===================

  앱 → 외부서비스:80
       |
  OUTPUT (nat) → ISTIO_OUTPUT → ISTIO_REDIRECT
       |
       +---> REDIRECT --to-ports 15001
       |     (목적지 주소가 127.0.0.1:15001로 변경)
       |
       v
  Envoy (outbound listener :15001)
       |
       | SO_ORIGINAL_DST로 원래 목적지 확인
       v
  외부서비스:80


  외부 → 앱:8080
       |
  PREROUTING (nat) → ISTIO_INBOUND → ISTIO_IN_REDIRECT
       |
       +---> REDIRECT --to-ports 15006
       |     (목적지 주소가 127.0.0.1:15006로 변경)
       |
       v
  Envoy (inbound listener :15006)
       |
       v
  앱:8080 (localhost 통신)
```

장점:
- 설정이 간단하다
- 추가 라우팅 설정이 필요 없다

단점:
- 원본 소스 IP가 보존되지 않는다 (모든 인바운드 트래픽이 127.0.0.1에서 오는 것으로 보임)
- 서버 측에서 클라이언트 IP를 확인할 수 없다

### 9.3 TPROXY 모드

TPROXY는 `mangle` 테이블의 `TPROXY` 타겟을 사용하여 원본 IP를 보존한다.

```
TPROXY 모드 동작
=================

  외부(10.0.0.5) → 앱:8080
       |
  PREROUTING (mangle) → ISTIO_INBOUND
       |
       +--- 기존 연결(RELATED,ESTABLISHED) → ISTIO_DIVERT
       |        → MARK 1337
       |        → ACCEPT
       |        (로컬 라우팅 테이블로 loopback 전달)
       |
       +--- 새 연결 → ISTIO_TPROXY
                → TPROXY --tproxy-mark 1337 --on-port 15006
                (소스 IP 10.0.0.5 보존, Envoy에 전달)
                |
                v
           Envoy (inbound listener :15006)
                |
                | 원본 소스 IP: 10.0.0.5 (보존됨)
                v
           앱:8080
```

```
TPROXY 추가 규칙 (mangle 테이블)
=================================

  ISTIO_DIVERT:
    -j MARK --set-mark 1337
    -j ACCEPT

  ISTIO_TPROXY:
    !-d 127.0.0.1/32 -p tcp -j TPROXY
        --tproxy-mark 1337/0xffffffff
        --on-port 15006

  OUTPUT (mangle):
    # envoy → app (pod IP) 트래픽에 mark 1338 설정
    !-d 127.0.0.1/32 -p tcp -o lo --uid-owner 1337
        -j MARK --set-mark 1338

    # connmark 복원 (app → envoy 경로)
    -p tcp -m connmark --mark 1337 -j CONNMARK --restore-mark

  ISTIO_INBOUND (mangle):
    # mark 1337인 패킷은 RETURN (무한루프 방지)
    -p tcp -m mark --mark 1337 -j RETURN
    # 127.0.0.6에서 오는 트래픽 RETURN
    -p tcp -s 127.0.0.6/32 -i lo -j RETURN
    # loopback의 비-mark 1338 트래픽 RETURN
    -p tcp -i lo -m mark !--mark 1338 -j RETURN
```

관련 코드:

```
소스: tools/istio-iptables/pkg/capture/run.go (라인 89~156)

func (cfg *IptablesConfigurator) handleInboundPortsInclude() {
    if cfg.cfg.InboundInterceptionMode == "TPROXY" {
        // ISTIO_DIVERT: mark 설정 + ACCEPT
        cfg.ruleBuilder.AppendRule(constants.ISTIODIVERT, "mangle",
            "-j", "MARK", "--set-mark", cfg.cfg.InboundTProxyMark)
        cfg.ruleBuilder.AppendRule(constants.ISTIODIVERT, "mangle",
            "-j", "ACCEPT")

        // ISTIO_TPROXY: 실제 TPROXY 규칙
        cfg.ruleBuilder.AppendVersionedRule(
            cfg.cfg.HostIPv4LoopbackCidr, "::1/128",
            constants.ISTIOTPROXY, "mangle",
            "!", "-d", constants.IPVersionSpecific,
            "-p", "tcp", "-j", "TPROXY",
            "--tproxy-mark", cfg.cfg.InboundTProxyMark+"/0xffffffff",
            "--on-port", cfg.cfg.InboundCapturePort)
        table = "mangle"
    } else {
        table = "nat"
    }
    // ...
}
```

### 9.4 REDIRECT vs TPROXY 비교

| 특성 | REDIRECT | TPROXY |
|------|----------|--------|
| 테이블 | nat | mangle |
| 소스 IP 보존 | X | O |
| 설정 복잡도 | 낮음 | 높음 (ip rule, ip route 필요) |
| 커널 요구사항 | 기본 | TPROXY 모듈 필요 |
| 사용 사례 | 기본값, 대부분 환경 | 소스 IP가 중요한 환경 |
| annotation | `sidecar.istio.io/interceptionMode: REDIRECT` | `sidecar.istio.io/interceptionMode: TPROXY` |

---

## 10. nftables 대안

### 10.1 왜 nftables인가?

iptables는 리눅스 커널의 레거시 방화벽 프레임워크이다. nftables는 iptables의 후속으로, 더 효율적인 규칙 매칭과 원자적 규칙 업데이트를 지원한다. 일부 최신 리눅스 배포판에서는 iptables-nft(nftables 백엔드를 사용하는 iptables 호환 레이어)가 기본이며, 순수 nftables 지원이 점점 중요해지고 있다.

### 10.2 nftables 테이블과 체인

```
소스: tools/istio-nftables/pkg/constants/constants.go (라인 19~39)

const (
    // nftables 테이블 (iptables의 테이블과 대응)
    IstioProxyNatTable    = "istio-proxy-nat"
    IstioProxyMangleTable = "istio-proxy-mangle"
    IstioProxyRawTable    = "istio-proxy-raw"

    // 베이스 체인
    PreroutingChain = "prerouting"
    OutputChain     = "output"

    // Istio 커스텀 체인
    IstioInboundChain    = "istio-inbound"
    IstioOutputChain     = "istio-output"
    IstioOutputDNSChain  = "istio-output-dns"
    IstioRedirectChain   = "istio-redirect"
    IstioInRedirectChain = "istio-in-redirect"
    IstioDivertChain     = "istio-divert"
    IstioTproxyChain     = "istio-tproxy"
    IstioPreroutingChain = "istio-prerouting"
    IstioDropChain       = "istio-drop"
)
```

```
iptables vs nftables 체인 이름 대응
=====================================

  iptables                    nftables
  ---------                   ---------
  ISTIO_OUTPUT           →    istio-output
  ISTIO_INBOUND          →    istio-inbound
  ISTIO_REDIRECT         →    istio-redirect
  ISTIO_IN_REDIRECT      →    istio-in-redirect
  ISTIO_DIVERT           →    istio-divert
  ISTIO_TPROXY           →    istio-tproxy
  ISTIO_DROP             →    istio-drop
  ISTIO_OUTPUT_DNS       →    istio-output-dns

  nat 테이블             →    istio-proxy-nat
  mangle 테이블          →    istio-proxy-mangle
  raw 테이블             →    istio-proxy-raw
```

### 10.3 NftablesRuleBuilder

```
소스: tools/istio-nftables/pkg/builder/nftables_builder_impl.go (라인 29~91)

type NftablesRuleBuilder struct {
    Rules map[string][]knftables.Rule  // 테이블별 규칙
    cfg   *config.Config
}

func NewNftablesRuleBuilder(cfg *config.Config) *NftablesRuleBuilder {
    rules := make(map[string][]knftables.Rule)
    for _, table := range IstioTableNames {
        rules[table] = []knftables.Rule{}
    }
    return &NftablesRuleBuilder{Rules: rules, cfg: cfg}
}

func (rb *NftablesRuleBuilder) AppendRule(chain, table string, params ...string) {
    rule := knftables.Rule{
        Chain:  chain,
        Table:  table,
        Family: knftables.InetFamily,   // inet (IPv4+IPv6 통합)
        Rule:   knftables.Concat(params),
    }
    rb.Rules[table] = append(rb.Rules[table], rule)
}
```

nftables 빌더의 핵심 차이점:
- **inet 패밀리**: IPv4와 IPv6를 단일 규칙으로 처리 가능 (iptables는 별도 바이너리 필요)
- **knftables 라이브러리 사용**: `sigs.k8s.io/knftables` 패키지를 통해 Go에서 nftables 규칙을 구성
- **카운터 지원**: 모든 규칙에 `counter` 문을 포함하여 패킷 매칭 통계를 추적

### 10.4 NftablesConfigurator

```
소스: tools/istio-nftables/pkg/capture/run.go (라인 36~73)

type NftablesConfigurator struct {
    cfg              *config.Config
    NetworkNamespace string
    ruleBuilder      *builder.NftablesRuleBuilder
    nftProvider      NftProviderFunc
}

func NewNftablesConfigurator(cfg *config.Config, nftProvider NftProviderFunc) (*NftablesConfigurator, error) {
    if nftProvider == nil {
        nftProvider = func(family knftables.Family, table string) (builder.NftablesAPI, error) {
            return builder.NewNftImpl(family, table)
        }
    }
    return &NftablesConfigurator{
        cfg:         cfg,
        ruleBuilder: builder.NewNftablesRuleBuilder(cfg),
        nftProvider: nftProvider,
    }, nil
}
```

### 10.5 nftables 규칙 예시 (인바운드)

```
소스: tools/istio-nftables/pkg/capture/run.go (라인 165~203)

// REDIRECT 모드 인바운드 규칙
func (cfg *NftablesConfigurator) handleInboundPortsInclude() {
    // prerouting → istio-inbound 점프
    cfg.ruleBuilder.AppendRule(constants.PreroutingChain, constants.IstioProxyNatTable,
        "meta l4proto tcp", constants.Counter,
        "jump", constants.IstioInboundChain)

    if cfg.cfg.InboundPortsInclude == "*" {
        // 제외 포트
        for _, port := range config.Split(cfg.cfg.InboundPortsExclude) {
            cfg.ruleBuilder.AppendRule(constants.IstioInboundChain, constants.IstioProxyNatTable,
                "meta l4proto tcp",
                "tcp dport", port, constants.Counter, "return")
        }
        // 나머지 → istio-in-redirect
        cfg.ruleBuilder.AppendRule(constants.IstioInboundChain, constants.IstioProxyNatTable,
            "meta l4proto tcp", constants.Counter,
            "jump", constants.IstioInRedirectChain)
    }
}
```

nftables 규칙은 iptables와 동일한 로직이지만, 문법이 다르다:
- iptables: `-p tcp --dport 80 -j RETURN`
- nftables: `meta l4proto tcp tcp dport 80 counter return`

### 10.6 CNI에서의 nftables 선택

```
소스: cni/pkg/plugin/plugin.go (라인 194~197)

mgr := IptablesInterceptRuleMgr()
if conf.NativeNftables {
    mgr = NftablesInterceptRuleMgr()
}
```

CNI 설정 파일에서 `native_nftables: true`로 설정하면 nftables 구현체가 사용된다.

---

## 11. UID 기반 제외, DNS 캡처, 포트 제외

### 11.1 UID 1337 기반 트래픽 제외

Istio는 프록시 프로세스(Envoy)가 UID 1337로 실행되도록 설정한다. iptables 규칙에서 이 UID를 사용하여 프록시 자체의 트래픽을 리다이렉션에서 제외함으로써 무한 루프를 방지한다.

```
소스: tools/istio-iptables/pkg/constants/constants.go (라인 121~123)

const (
    DefaultProxyUID    = "1337"
    DefaultProxyUIDInt = int64(1337)
)
```

```
UID 기반 제외 로직
===================

  앱 프로세스 (UID != 1337)
       |
       | 아웃바운드 트래픽
       v
  OUTPUT → ISTIO_OUTPUT
       |
       | -m owner --uid-owner 1337 ?
       | → NO (앱 프로세스)
       | → ISTIO_REDIRECT → Envoy (15001)
       v
  Envoy (UID 1337)
       |
       | 아웃바운드 트래픽 (목적지로 전달)
       v
  OUTPUT → ISTIO_OUTPUT
       |
       | -m owner --uid-owner 1337 ?
       | → YES (Envoy 프로세스)
       | → RETURN (리다이렉션 건너뜀)
       v
  직접 목적지로 전달
```

관련 코드:

```
소스: tools/istio-iptables/pkg/capture/run.go (라인 294~343)

for _, uid := range config.Split(cfg.cfg.ProxyUID) {
    // Envoy의 loopback 트래픽 → ISTIO_IN_REDIRECT (자기 호출)
    cfg.ruleBuilder.AppendVersionedRule("127.0.0.1/32", "::1/128",
        constants.ISTIOOUTPUT, "nat",
        "-o", "lo", "!", "-d", constants.IPVersionSpecific,
        "-p", "tcp", "!", "--dport", cfg.cfg.InboundTunnelPort,
        "-m", "owner", "--uid-owner", uid, "-j", constants.ISTIOINREDIRECT)

    // 비-Envoy loopback 트래픽 → RETURN
    cfg.ruleBuilder.AppendRule(constants.ISTIOOUTPUT, "nat",
        "-o", "lo", "-m", "owner", "!", "--uid-owner", uid, "-j", "RETURN")

    // Envoy의 비-loopback 트래픽 → RETURN (무한루프 방지)
    cfg.ruleBuilder.AppendRule(constants.ISTIOOUTPUT, "nat",
        "-m", "owner", "--uid-owner", uid, "-j", "RETURN")
}
```

**왜 127.0.0.6인가?**: `127.0.0.6`은 Istio의 inbound passthrough cluster에서 사용하는 특수 주소이다. Envoy가 로컬 앱으로 트래픽을 전달할 때 이 주소를 소스로 사용하여, 일반 loopback 트래픽과 구분한다.

### 11.2 GetProxyIDs() -- OpenShift UID 범위

```
소스: pkg/kube/inject/inject.go (라인 951~968)

func GetProxyIDs(namespace *corev1.Namespace) (uid int64, gid int64) {
    uid = constants.DefaultProxyUIDInt  // 1337
    gid = constants.DefaultProxyUIDInt

    if namespace == nil {
        return uid, gid
    }

    // OpenShift: 네임스페이스에 할당된 UID 범위의 최대값 사용
    if _, uidMax, err := getPreallocatedUIDRange(namespace); err == nil {
        uid = *uidMax
    }
    if groups, err := getPreallocatedSupplementalGroups(namespace); err == nil {
        gid = groups[0].Max
    }

    return uid, gid
}
```

OpenShift는 각 네임스페이스에 UID 범위를 미리 할당한다. Istio는 해당 범위의 최대값을 프록시 UID로 사용하여 OpenShift의 보안 정책과 호환되도록 한다.

### 11.3 DNS 캡처

DNS 캡처는 애플리케이션의 DNS 요청을 istio-agent의 DNS 프록시(포트 15053)로 리다이렉트한다.

```
소스: tools/istio-iptables/pkg/capture/run.go (라인 450~532)

func SetupDNSRedir(iptables *builder.IptablesRuleBuilder, ...) {
    // raw:OUTPUT → ISTIO_OUTPUT_DNS 점프
    iptables.AppendRule("OUTPUT", "raw", "-j", constants.ISTIOOUTPUTDNS)

    if captureAllDNS {
        // 모든 DNS 트래픽 캡처 (CNI 모드에서 사용)
        iptables.AppendRule(constants.ISTIOOUTPUTDNS, "nat",
            "-p", "tcp", "--dport", "53",
            "-j", "REDIRECT", "--to-ports", constants.IstioAgentDNSListenerPort)
        iptables.AppendRule(constants.ISTIOOUTPUTDNS, "nat",
            "-p", "udp", "--dport", "53",
            "-j", "REDIRECT", "--to-port", constants.IstioAgentDNSListenerPort)
    } else {
        // resolv.conf의 DNS 서버만 캡처
        for _, s := range dnsServersV4 {
            iptables.AppendRuleV4(constants.ISTIOOUTPUTDNS, "nat",
                "-p", "tcp", "--dport", "53", "-d", s+"/32",
                "-j", "REDIRECT", "--to-ports", "15053")
        }
    }

    // DNS conntrack zone 분리
    addDNSConntrackZones(iptables, proxyUID, proxyGID, ...)
}
```

```
DNS 캡처 흐름
==============

  앱 → DNS 요청 (포트 53)
       |
  OUTPUT (raw) → ISTIO_OUTPUT_DNS
       |
       | conntrack zone 2 설정 (앱→istio)
       |
  OUTPUT (nat) → ISTIO_OUTPUT_DNS
       |
       +---> REDIRECT --to-ports 15053
       |
       v
  istio-agent DNS 프록시 (:15053)
       |
       | DNS 해석 + ServiceEntry 지원
       | conntrack zone 1 설정 (istio→upstream)
       |
       v
  업스트림 DNS 서버 (8.8.8.8:53 등)
```

**왜 conntrack zone을 분리하는가?**

UDP는 연결 상태가 없지만, conntrack은 소스/목적지 튜플로 "연결"을 추적한다. DNS 요청과 응답이 동일한 zone에 있으면 race condition이 발생할 수 있다.

- **Zone 1**: istio-agent와 업스트림 DNS 서버 간 트래픽
- **Zone 2**: 앱과 istio-agent 간 트래픽

이렇게 분리하면 각 방향의 conntrack 항목이 충돌하지 않는다.

```
소스: tools/istio-iptables/pkg/capture/run.go (라인 538~596)

func addDNSConntrackZones(...) {
    for _, uid := range config.Split(proxyUID) {
        // istio → upstream (zone 1): 프록시가 DNS 서버에 보내는 요청
        iptables.AppendRule(constants.ISTIOOUTPUTDNS, "raw",
            "-p", "udp", "--dport", "53",
            "-m", "owner", "--uid-owner", uid, "-j", "CT", "--zone", "1")
        // istio → app (zone 2): 프록시가 앱에 보내는 응답
        iptables.AppendRule(constants.ISTIOOUTPUTDNS, "raw",
            "-p", "udp", "--sport", "15053",
            "-m", "owner", "--uid-owner", uid, "-j", "CT", "--zone", "2")
    }
    // 앱 → istio (zone 2): 앱이 보내는 DNS 요청
    iptables.AppendRule(constants.ISTIOOUTPUTDNS, "raw",
        "-p", "udp", "--dport", "53", "-j", "CT", "--zone", "2")
    // upstream → istio (zone 1): DNS 서버의 응답
    iptables.AppendRule(constants.ISTIOINBOUND, "raw",
        "-p", "udp", "--sport", "53", "-j", "CT", "--zone", "1")
}
```

### 11.4 포트 제외

인바운드 포트 제외는 CNI 플러그인의 `NewRedirect()`에서 처리된다.

```
소스: cni/pkg/plugin/sidecar_redirect.go (라인 266~273)

// 항상 제외되는 포트 추가: 15020, 15021, 15090
redir.excludeInboundPorts += "15020,15021,15090"
redir.excludeInboundPorts = strings.Join(
    dedupPorts(splitPorts(redir.excludeInboundPorts)), ",")
```

| 포트 | 용도 | 제외 이유 |
|------|------|----------|
| 15020 | istio-agent 상태 포트 | kubelet health check |
| 15021 | istio-proxy 상태 포트 | kubelet readiness check |
| 15090 | Envoy Prometheus 메트릭 | 외부 스크래핑 접근 필요 |
| 15001 | Envoy outbound listener | 리다이렉트 대상 자체 |
| 15006 | Envoy inbound listener | 리다이렉트 대상 자체 |
| 15008 | HBONE 터널 포트 | 터널 트래픽 직접 접근 |

annotation 기반 포트 제외:

```
소스: cni/pkg/plugin/sidecar_redirect.go (라인 48~76)

var annotationRegistry = map[string]*annotationParam{
    "excludeInboundPorts":  {excludeInboundPortsKey,  "15020", ...},
    "includeInboundPorts":  {includeInboundPortsKey,  "*",     ...},
    "excludeOutboundPorts": {excludeOutboundPortsKey, "15020", ...},
    "includeOutboundPorts": {includeOutboundPortsKey, "",      ...},
    "includeIPCidrs":       {includeIPCidrsKey,       "*",     ...},
    "excludeIPCidrs":       {excludeIPCidrsKey,       "",      ...},
    "redirectMode":         {sidecarInterceptModeKey, "REDIRECT", ...},
    "excludeInterfaces":    {excludeInterfacesKey,    "",      ...},
}
```

Pod annotation으로 세밀한 트래픽 제어가 가능하다:

```yaml
annotations:
  traffic.sidecar.istio.io/excludeInboundPorts: "8080,8443"
  traffic.sidecar.istio.io/excludeOutboundPorts: "5432"
  traffic.sidecar.istio.io/excludeOutboundIPRanges: "10.96.0.0/12"
  traffic.sidecar.istio.io/includeOutboundIPRanges: "10.0.0.0/8"
  sidecar.istio.io/interceptionMode: "TPROXY"
```

---

## 12. 전체 흐름 요약

```
Pod 생성부터 트래픽 캡처까지의 전체 흐름
==========================================

  [1] 사용자가 Deployment 생성
       |
       v
  [2] K8s API Server → MutatingWebhook 호출
       |
       v
  [3] Istiod Webhook (serveInject)
       |
       +-- injectRequired()
       |     - HostNetwork 확인
       |     - 네임스페이스 확인
       |     - label/annotation 확인
       |     - NeverInject/AlwaysInject 셀렉터
       |     - 네임스페이스 정책 (enabled/disabled)
       |
       +-- injectPod()
       |     - RunTemplate()
       |       - stripPod() (멱등성)
       |       - SidecarTemplateData 조립
       |       - Go template 렌더링
       |       - applyOverlayYAML() (전략적 머지)
       |     - reapplyOverwrittenContainers()
       |     - postProcessPod()
       |       - Prometheus 설정
       |       - 컨테이너 순서 조정
       |       - sidecar status annotation
       |     - createPatch() (RFC 6902 JSON Patch)
       |
       v
  [4] API Server가 Pod 스펙에 패치 적용
       - istio-proxy 컨테이너 추가
       - istio-init 컨테이너 추가 (또는 CNI가 대체)
       - 볼륨, 환경변수, annotation 등
       |
       v
  [5] Pod 스케줄링 → Node 할당
       |
       +--- [init 방식] --------+--- [CNI 방식] ---+
       |                        |                   |
       v                        v                   |
  [6a] istio-init           [6b] kubelet이          |
       initContainer             CNI 체인 호출       |
       실행                      |                   |
       |                         v                   |
       |                    istio-cni CmdAdd()       |
       |                    - Pod 적격성 검사         |
       |                    - istio-proxy 있는지     |
       |                    - inject 비활성 아닌지   |
       |                    - sidecar status 있는지  |
       |                         |                   |
       +----------+--------------+                   |
                  |                                   |
                  v                                   |
  [7] iptables/nftables 규칙 설정                     |
       - ISTIO_INBOUND 체인                           |
       - ISTIO_OUTPUT 체인                            |
       - ISTIO_REDIRECT 체인                          |
       - UID 1337 제외                                |
       - DNS 캡처 (선택적)                            |
       - 포트 제외                                    |
                  |
                  v
  [8] 트래픽 캡처 활성화
       - 인바운드 → Envoy :15006
       - 아웃바운드 → Envoy :15001
       - DNS → istio-agent :15053 (선택적)
```

### 설계 원칙 요약

1. **멱등성**: `stripPod()`로 이전 인젝션을 제거한 후 재인젝션하여, 동일한 입력에 대해 항상 동일한 결과를 보장한다.

2. **무한루프 방지**: UID 1337 기반 규칙으로 Envoy 자체의 트래픽이 다시 Envoy로 리다이렉트되는 것을 방지한다.

3. **유연한 정책 계층**: label > annotation > NeverInjectSelector > AlwaysInjectSelector > namespace policy 순서로 정책을 결정하여, 세밀한 제어가 가능하다.

4. **전략 패턴**: `InterceptRuleMgr` 인터페이스를 통해 iptables와 nftables 구현을 교체 가능하게 설계했다.

5. **듀얼 스택 지원**: `IptablesRuleBuilder`는 IPv4와 IPv6 규칙을 별도로 관리하고, nftables는 inet 패밀리로 통합 처리한다.

6. **안전한 롤아웃**: CNI 플러그인은 `isCNIPod()` 검사로 자기 자신의 업그레이드 시 데드락을 방지하고, 최대 30회 재시도로 API 서버 접근 실패를 처리한다.

---

## 참고: 핵심 상수 및 포트 번호

| 포트 | 용도 |
|------|------|
| 15001 | Envoy outbound listener (모든 아웃바운드 트래픽 수신) |
| 15006 | Envoy inbound listener (모든 인바운드 트래픽 수신) |
| 15008 | HBONE 터널 포트 |
| 15020 | istio-agent 상태/헬스체크 |
| 15021 | Envoy 헬스체크 |
| 15053 | istio-agent DNS 프록시 |
| 15090 | Envoy Prometheus 메트릭 |
| 1337 | 프록시 UID/GID (기본값) |
| 1338 | TPROXY 아웃바운드 마크 |
