# 16. Operator와 설치 시스템 Deep Dive

## 목차

1. [개요](#1-개요)
2. [IstioOperator API](#2-istiooperator-api)
3. [프로파일 시스템](#3-프로파일-시스템)
4. [매니페스트 렌더링 파이프라인](#4-매니페스트-렌더링-파이프라인)
5. [Helm 차트 구조](#5-helm-차트-구조)
6. [컴포넌트 시스템](#6-컴포넌트-시스템)
7. [install 명령](#7-install-명령)
8. [manifest generate 명령](#8-manifest-generate-명령)
9. [manifest translate 명령](#9-manifest-translate-명령)
10. [upgrade 명령](#10-upgrade-명령)
11. [uninstall 명령](#11-uninstall-명령)
12. [Revision 기반 카나리 업그레이드](#12-revision-기반-카나리-업그레이드)
13. [K8s 설정 지원과 후처리](#13-k8s-설정-지원과-후처리)
14. [values.Map 시스템](#14-valuesmap-시스템)
15. [설치 후 리소스 대기와 검증](#15-설치-후-리소스-대기와-검증)
16. [설계 결정과 아키텍처 분석](#16-설계-결정과-아키텍처-분석)

---

## 1. 개요

Istio의 설치 시스템은 `istioctl` CLI와 IstioOperator CR(Custom Resource)을 중심으로 구성된다. 사용자는 선언적 CR 파일로 원하는 Istio 구성을 기술하고, 설치 시스템이 이를 Helm 차트 렌더링과 후처리를 거쳐 최종 Kubernetes 매니페스트로 변환한다.

### 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| 선언적 API | IstioOperator CR로 원하는 상태를 선언 |
| 프로파일 기반 | 사전 정의된 프로파일 위에 사용자 커스터마이징 적용 |
| Helm 통합 | 내부적으로 Helm 라이브러리를 사용하여 차트 렌더링 |
| 후처리 파이프라인 | Helm 렌더링 후 K8s 리소스 설정을 StrategicMergePatch로 적용 |
| 컴포넌트 단위 관리 | 각 컴포넌트를 독립적으로 활성화/비활성화/커스터마이징 |

### 소스코드 구조

```
operator/
├── cmd/mesh/           # CLI 명령 구현
│   ├── install.go      # install 명령
│   ├── upgrade.go      # upgrade 명령 (install의 별칭)
│   ├── uninstall.go    # uninstall 명령
│   ├── manifest.go     # manifest 상위 명령
│   ├── manifest-generate.go    # manifest generate
│   ├── manifest-translate.go   # manifest translate (Helm 마이그레이션)
│   ├── root.go         # 공통 플래그/타입
│   └── shared.go       # 유틸리티 (Confirm, applyFlagAliases)
├── pkg/
│   ├── apis/           # IstioOperator 타입 정의
│   │   └── types.go    # IstioOperator, IstioOperatorSpec, KubernetesResources
│   ├── component/      # 컴포넌트 정의 (AllComponents)
│   │   └── component.go
│   ├── render/         # 매니페스트 렌더링 파이프라인
│   │   ├── manifest.go # GenerateManifest, MergeInputs
│   │   └── postprocess.go  # K8s 리소스 후처리 패치
│   ├── install/        # 클러스터 적용 로직
│   │   ├── install.go  # Installer, InstallManifests
│   │   └── wait.go     # WaitForResources
│   ├── helm/           # Helm 차트 로딩/렌더링
│   │   └── helm.go     # Render, loadChart, renderChart
│   ├── manifest/       # Manifest 타입과 파싱
│   │   ├── manifest.go # Manifest, ManifestSet, Parse
│   │   └── name.go     # 레이블 상수 정의
│   ├── uninstall/      # 리소스 제거 로직
│   │   └── prune.go    # GetPrunedResources, DeleteObjectsList
│   ├── values/         # values.Map 동적 설정 관리
│   │   └── map.go      # Map, MergeFrom, GetPath, SetPath
│   └── webhook/        # 웹훅 충돌 검사/배포
│       └── webhook.go  # CheckWebhooks, WebhooksToDeploy
manifests/
├── profiles/           # 설치 프로파일 YAML
│   ├── default.yaml
│   ├── demo.yaml
│   ├── minimal.yaml
│   ├── ambient.yaml
│   ├── openshift.yaml
│   └── ...
└── charts/             # Helm 차트
    ├── base/           # CRD, 클러스터 리소스
    ├── istio-control/istio-discovery/  # istiod
    ├── gateways/       # ingress/egress 게이트웨이
    ├── istio-cni/      # CNI 플러그인
    ├── ztunnel/        # ambient ztunnel
    └── gateway/        # Gateway API 기반 게이트웨이
```

---

## 2. IstioOperator API

IstioOperator API는 Istio 설치를 위한 선언적 인터페이스다. `install.istio.io/v1alpha1` API 그룹에 정의되며, `IstioOperator` Kind의 CR로 사용된다.

### 2.1 최상위 구조

`operator/pkg/apis/types.go`에 정의된 `IstioOperator` 구조체:

```go
// operator/pkg/apis/types.go

type IstioOperator struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec IstioOperatorSpec `json:"spec,omitempty"`
}

type IstioOperatorSpec struct {
    Profile            string                `json:"profile,omitempty"`
    InstallPackagePath string                `json:"installPackagePath,omitempty"`
    Hub                string                `json:"hub,omitempty"`
    Tag                any                   `json:"tag,omitempty"`
    Namespace          string                `json:"namespace,omitempty"`
    Revision           string                `json:"revision,omitempty"`
    CompatibilityVersion string              `json:"compatibilityVersion,omitempty"`
    MeshConfig         json.RawMessage       `json:"meshConfig,omitempty"`
    Components         *IstioComponentSpec   `json:"components,omitempty"`
    Values             json.RawMessage       `json:"values,omitempty"`
    UnvalidatedValues  any                   `json:"unvalidatedValues,omitempty"`
}
```

### 2.2 주요 필드 설명

| 필드 | 타입 | 설명 |
|------|------|------|
| `profile` | string | 사용할 프로파일 이름 (default, demo, minimal 등) |
| `hub` | string | Docker 이미지 레지스트리 (예: `gcr.io/istio-testing`) |
| `tag` | any | Docker 이미지 태그 (문자열 또는 숫자 모두 허용) |
| `namespace` | string | 컨트롤 플레인 설치 네임스페이스 |
| `revision` | string | 리비전 이름 (카나리 업그레이드용) |
| `compatibilityVersion` | string | 이전 버전 호환 설정 (예: `1.23`) |
| `meshConfig` | JSON | 메시 전역 설정 (tracing, accessLog 등) |
| `components` | IstioComponentSpec | 컴포넌트별 활성화/설정 |
| `values` | JSON | Helm values.yaml 패스스루 |
| `unvalidatedValues` | any | 검증 없이 전달되는 값 (커스텀 템플릿용) |

### 2.3 컴포넌트 설정 (IstioComponentSpec)

```go
// operator/pkg/apis/types.go

type IstioComponentSpec struct {
    Base            *BaseComponentSpec     `json:"base,omitempty"`
    Pilot           *ComponentSpec         `json:"pilot,omitempty"`
    Cni             *ComponentSpec         `json:"cni,omitempty"`
    Ztunnel         *ComponentSpec         `json:"ztunnel,omitempty"`
    IstiodRemote    *ComponentSpec         `json:"istiodRemote,omitempty"`
    IngressGateways []GatewayComponentSpec `json:"ingressGateways,omitempty"`
    EgressGateways  []GatewayComponentSpec `json:"egressGateways,omitempty"`
}
```

각 컴포넌트는 독립적으로 활성화/비활성화할 수 있고, 네임스페이스와 hub/tag를 개별 지정할 수 있다:

```go
type ComponentSpec struct {
    Enabled    *BoolValue           `json:"enabled,omitempty"`
    Namespace  string               `json:"namespace,omitempty"`
    Hub        string               `json:"hub,omitempty"`
    Tag        any                  `json:"tag,omitempty"`
    Kubernetes *KubernetesResources `json:"k8s,omitempty"`
}
```

게이트웨이는 `GatewayComponentSpec`으로 확장되어 `name`과 `label` 필드가 추가된다:

```go
type GatewayComponentSpec struct {
    ComponentSpec
    Name  string            `json:"name,omitempty"`
    Label map[string]string `json:"label,omitempty"`
}
```

### 2.4 KubernetesResources 설정

`KubernetesResources` 구조체는 각 컴포넌트의 K8s 리소스를 직접 커스터마이징하는 통합 인터페이스를 제공한다:

```go
// operator/pkg/apis/types.go

type KubernetesResources struct {
    Affinity            *corev1.Affinity                           `json:"affinity,omitempty"`
    Env                 []*corev1.EnvVar                           `json:"env,omitempty"`
    HpaSpec             *autoscaling.HorizontalPodAutoscalerSpec   `json:"hpaSpec,omitempty"`
    ImagePullPolicy     string                                     `json:"imagePullPolicy,omitempty"`
    NodeSelector        map[string]string                          `json:"nodeSelector,omitempty"`
    PodDisruptionBudget *policy.PodDisruptionBudgetSpec            `json:"podDisruptionBudget,omitempty"`
    PodAnnotations      map[string]string                          `json:"podAnnotations,omitempty"`
    PriorityClassName   string                                     `json:"priorityClassName,omitempty"`
    ReadinessProbe      *corev1.Probe                              `json:"readinessProbe,omitempty"`
    ReplicaCount        uint32                                     `json:"replicaCount,omitempty"`
    Resources           *corev1.ResourceRequirements               `json:"resources,omitempty"`
    Service             *corev1.ServiceSpec                        `json:"service,omitempty"`
    Strategy            *appsv1.DeploymentStrategy                 `json:"strategy,omitempty"`
    Tolerations         []*corev1.Toleration                       `json:"tolerations,omitempty"`
    ServiceAnnotations  map[string]string                          `json:"serviceAnnotations,omitempty"`
    SecurityContext     *corev1.PodSecurityContext                  `json:"securityContext,omitempty"`
    Volumes             []*corev1.Volume                           `json:"volumes,omitempty"`
    VolumeMounts        []*corev1.VolumeMount                      `json:"volumeMounts,omitempty"`
    Overlays            []KubernetesOverlay                        `json:"overlays,omitempty"`
}
```

이 타입들은 모두 Kubernetes 네이티브 API 타입을 직접 임베드한다. 예를 들어 `Affinity` 필드는 `corev1.Affinity` 타입이므로 Kubernetes에서 사용하는 것과 동일한 스키마를 사용한다.

### 2.5 Overlays - 임의 패치 메커니즘

`KubernetesOverlay`는 렌더링된 매니페스트에 임의의 패치를 적용하는 메커니즘이다:

```go
type KubernetesOverlay struct {
    ApiVersion string  `json:"apiVersion,omitempty"`
    Kind       string  `json:"kind,omitempty"`
    Name       string  `json:"name,omitempty"`
    Patches    []Patch `json:"patches,omitempty"`
}

type Patch struct {
    Path  string `json:"path,omitempty"`   // 패치 경로 (예: spec.template.spec.containers)
    Value any    `json:"value,omitempty"`  // 패치 값
}
```

### 2.6 CR 예시

```yaml
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
metadata:
  namespace: istio-system
spec:
  profile: default
  hub: gcr.io/istio-testing
  tag: latest
  revision: canary
  meshConfig:
    accessLogFile: /dev/stdout
    enableTracing: true
  components:
    pilot:
      enabled: true
      k8s:
        resources:
          requests:
            cpu: 500m
            memory: 2Gi
        hpaSpec:
          minReplicas: 2
          maxReplicas: 5
        nodeSelector:
          cloud.google.com/gke-nodepool: istio-pool
    cni:
      enabled: true
    ingressGateways:
    - name: istio-ingressgateway
      enabled: true
      k8s:
        service:
          type: LoadBalancer
    egressGateways:
    - name: istio-egressgateway
      enabled: false
  values:
    global:
      istioNamespace: istio-system
```

---

## 3. 프로파일 시스템

프로파일은 사전 정의된 IstioOperator 설정의 집합으로, 다양한 사용 시나리오에 맞춘 기본값을 제공한다.

### 3.1 사용 가능한 프로파일

`manifests/profiles/` 디렉토리에 YAML 파일로 정의된다:

| 프로파일 | 파일 | 주요 컴포넌트 | 용도 |
|----------|------|-------------|------|
| **default** | `default.yaml` | Base, Pilot, IngressGateway | 프로덕션 기본 |
| **demo** | `demo.yaml` | Base, Pilot, IngressGateway, EgressGateway | 데모/학습용 |
| **minimal** | `minimal.yaml` | Base, Pilot | 최소 설치 |
| **ambient** | `ambient.yaml` | Base, Pilot, CNI, Ztunnel | 사이드카 없는 ambient 메시 |
| **openshift** | `openshift.yaml` | Base, Pilot, IngressGateway, CNI | OpenShift 플랫폼 |
| **remote** | `remote.yaml` | Base, Pilot (외부 컨트롤 플레인) | 멀티클러스터 |
| **preview** | `preview.yaml` | default 기반 | 실험적 기능 |
| **empty** | `empty.yaml` | 없음 | 빈 프로파일 |

### 3.2 프로파일 파일 분석

**default.yaml** - 프로덕션 기본:

```yaml
# manifests/profiles/default.yaml
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
metadata:
  namespace: istio-system
spec:
  hub: gcr.io/istio-testing
  tag: latest
  components:
    base:
      enabled: true
    pilot:
      enabled: true
    ingressGateways:
    - name: istio-ingressgateway
      enabled: true
    egressGateways:
    - name: istio-egressgateway
      enabled: false
  values:
    defaultRevision: ""
    global:
      istioNamespace: istio-system
      configValidation: true
```

**ambient.yaml** - Ambient 메시 모드:

```yaml
# manifests/profiles/ambient.yaml
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
spec:
  components:
    cni:
      enabled: true
    ztunnel:
      enabled: true
    ingressGateways:
    - name: istio-ingressgateway
      enabled: false
  values:
    profile: ambient
```

**minimal.yaml** - 컨트롤 플레인만:

```yaml
# manifests/profiles/minimal.yaml
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
spec:
  components:
    ingressGateways:
    - name: istio-ingressgateway
      enabled: false
```

### 3.3 프로파일 로딩 메커니즘

`operator/pkg/render/manifest.go`의 `readProfile()` 함수가 프로파일 로딩을 담당한다:

```go
// operator/pkg/render/manifest.go

func readProfile(path string, profile string) (values.Map, error) {
    if profile == "" {
        profile = "default"
    }
    // 모든 프로파일은 'default' 위에 오버레이로 적용
    base, err := readProfileInternal(path, "default")
    if err != nil {
        return nil, err
    }
    if profile == "default" {
        return base, nil
    }
    // default가 아니면, default 위에 요청된 프로파일을 머지
    top, err := readProfileInternal(path, profile)
    if err != nil {
        return nil, err
    }
    base.MergeFrom(top)
    return base, nil
}
```

핵심 설계: 모든 프로파일은 `default` 프로파일을 기반으로 하며, 각 프로파일의 YAML은 default와의 차이(delta)만 기술한다. 이로써 프로파일 파일이 간결하게 유지된다.

### 3.4 프로파일 우선순위

```
base(default) < profile < 클러스터 자동 감지 < 사용자 파일(-f) < --set 플래그
```

이 우선순위는 `MergeInputs()` 함수에서 구현된다.

---

## 4. 매니페스트 렌더링 파이프라인

설치의 핵심은 매니페스트 렌더링 파이프라인이다. 사용자 입력(CR 파일, --set 플래그)을 받아 최종 Kubernetes 매니페스트를 생성하는 전체 흐름을 분석한다.

### 4.1 전체 파이프라인 흐름

```
사용자 입력 (CR 파일 + --set 플래그)
         │
         ▼
┌─────────────────────────┐
│  1. MergeInputs()       │  입력 병합
│  - default 프로파일 로드  │
│  - 프로파일 오버레이      │
│  - 클러스터 자동 감지     │
│  - 사용자 파일 적용       │
│  - --set 플래그 적용      │
└────────────┬────────────┘
             ▼
┌─────────────────────────┐
│  2. validateIstioOperator│  검증
│  - 스키마 검증            │
│  - 상호 의존성 검증       │
│  - --force로 경고만 출력  │
└────────────┬────────────┘
             ▼
┌─────────────────────────┐
│  3. 컴포넌트별 렌더링     │  Helm 렌더링
│  - AllComponents 순회     │
│  - comp.Get(merged)      │
│  - applyComponentValues  │
│  - helm.Render()         │
└────────────┬────────────┘
             ▼
┌─────────────────────────┐
│  4. postProcess()        │  후처리
│  - K8s 리소스 패치       │
│  - StrategicMergePatch   │
│  - Overlay 적용          │
└────────────┬────────────┘
             ▼
     최종 ManifestSet[]
```

### 4.2 MergeInputs - 입력 병합

`operator/pkg/render/manifest.go`의 `MergeInputs()` 함수는 모든 설정 소스를 병합한다:

```go
// operator/pkg/render/manifest.go

func MergeInputs(filenames []string, flags []string, client kube.Client) (values.Map, error) {
    // 1. 초기 기본 IstioOperator 구조 생성
    userConfigBase, err := values.MapFromJSON([]byte(`{
      "apiVersion": "install.istio.io/v1alpha1",
      "kind": "IstioOperator",
      "metadata": {},
      "spec": {}
    }`))

    // 2. 사용자 파일 순서대로 적용 (여러 파일 오버레이 가능)
    for i, fn := range filenames {
        b, err := os.ReadFile(strings.TrimSpace(fn))
        m, err := values.MapFromYaml(b)
        userConfigBase.MergeFrom(m)
    }

    // 3. --set 플래그 적용
    userConfigBase.SetSpecPaths(flags...)

    // 4. 프로파일과 installPackagePath 추출
    installPackagePath := userConfigBase.GetPathString("spec.installPackagePath")
    profile := userConfigBase.GetPathString("spec.profile")

    // 5. 프로파일 기반 base 로드
    base, err := readProfile(installPackagePath, profile)

    // 6. 클러스터 특화 설정 적용 (GKE 자동 감지 등)
    base.SetSpecPaths(clusterSpecificSettings(client)...)

    // 7. 컴파일 시점 hub/tag 삽입
    base.SetSpecPaths(hubTagOverlay()...)

    // 8. 사용자 설정을 base 위에 머지
    base.MergeFrom(userConfigBase)

    // 9. IstioOperator 필드를 Helm values로 번역
    base, err = translateIstioOperatorToHelm(base)

    // 10. 사용자 values를 최종 적용 (번역 결과보다 우선)
    if userValues != nil {
        base.MergeFrom(values.Map{"spec": values.Map{"values": userValues}})
    }
    return base, nil
}
```

### 4.3 IstioOperator-to-Helm 번역

`translateIstioOperatorToHelm()` 함수는 IstioOperator의 최상위 필드를 Helm values 형태로 변환한다:

```go
// operator/pkg/render/manifest.go

func translateIstioOperatorToHelm(base values.Map) (values.Map, error) {
    translations := map[string]string{
        "spec.hub":                  "global.hub",
        "spec.tag":                  "global.tag",
        "spec.revision":             "revision",
        "spec.meshConfig":           "meshConfig",
        "spec.compatibilityVersion": "compatibilityVersion",
    }
    // ...
    // 컴포넌트 활성화 상태를 values로 전파
    base.SetPath("spec.values.pilot.enabled", base.GetPathBool("spec.components.pilot.enabled"))
    base.SetPath("spec.values.pilot.cni.enabled", base.GetPathBool("spec.components.cni.enabled"))
    // ...
}
```

이 번역 테이블을 통해 `spec.hub: gcr.io/istio` 같은 IstioOperator 형식이 `values.global.hub: gcr.io/istio` 같은 Helm values 형식으로 변환된다.

### 4.4 클러스터 자동 감지

GKE와 같은 관리형 Kubernetes 환경을 자동 감지하여 적절한 설정을 적용한다:

```go
// operator/pkg/render/manifest.go

func clusterSpecificSettings(client kube.Client) []string {
    if client == nil {
        return nil
    }
    ver, _ := client.GetKubernetesVersion()
    // GKE 감지: GitVersion에 "-gke" 포함 여부 확인
    if allowGKEAutoDetection && strings.Contains(ver.GitVersion, "-gke") {
        return []string{
            "components.cni.namespace=kube-system",
            "values.global.platform=gke",
            "values.cni.resourceQuotas.enabled=false",
        }
    }
    return nil
}
```

### 4.5 GenerateManifest - 전체 렌더링 오케스트레이션

```go
// operator/pkg/render/manifest.go

func GenerateManifest(files []string, setFlags []string, force bool,
    client kube.Client, logger clog.Logger) ([]manifest.ManifestSet, values.Map, error) {

    // 1. 모든 입력을 병합하여 최종 IstioOperator 생성
    merged, err := MergeInputs(files, setFlags, client)

    // 2. 유효성 검증
    validateIstioOperator(merged, client, logger, force)

    // 3. unvalidatedValues 적용
    if unvalidatedValues, _ := merged.GetPathMap("spec.unvalidatedValues"); unvalidatedValues != nil {
        merged.MergeFrom(values.MakeMap(unvalidatedValues, "spec", "values"))
    }

    // 4. Kubernetes 버전 정보 가져오기 (차트 조건부 렌더링용)
    var kubernetesVersion *version.Info
    if client != nil {
        kubernetesVersion, _ = client.GetKubernetesVersion()
    }

    // 5. 각 컴포넌트별 렌더링
    allManifests := map[component.Name]manifest.ManifestSet{}
    for _, comp := range component.AllComponents {
        specs, _ := comp.Get(merged)
        for _, spec := range specs {
            // 컴포넌트별 values 조정
            compVals := applyComponentValuesToHelmValues(comp, spec, merged)
            // Helm 차트 렌더링
            rendered, warnings, _ := helm.Render("istio", spec.Namespace,
                comp.HelmSubdir, compVals, kubernetesVersion)
            // 후처리 (K8s 패치, 오버레이)
            finalized, _ := postProcess(comp, spec, rendered, compVals)
            // 결과 수집
            allManifests[comp.UserFacingName] = manifest.ManifestSet{
                Component: comp.UserFacingName,
                Manifests: finalized,
            }
        }
    }
    return values, merged, nil
}
```

---

## 5. Helm 차트 구조

Istio의 Helm 차트는 `manifests/charts/` 디렉토리에 위치하며, 각 컴포넌트별로 독립된 차트가 존재한다.

### 5.1 차트 목록

| 차트 경로 | Chart 이름 | 설명 | 컴포넌트 |
|-----------|-----------|------|----------|
| `charts/base/` | base | CRD, 클러스터 전역 리소스 | Base |
| `charts/istio-control/istio-discovery/` | istiod | 컨트롤 플레인 (Pilot) | Pilot |
| `charts/gateways/istio-ingress/` | - | Ingress Gateway | IngressGateways |
| `charts/gateways/istio-egress/` | - | Egress Gateway | EgressGateways |
| `charts/istio-cni/` | cni | CNI 플러그인 | Cni |
| `charts/ztunnel/` | ztunnel | Ambient ztunnel | Ztunnel |
| `charts/gateway/` | gateway | Gateway API 기반 게이트웨이 | - |

### 5.2 차트 내부 구조

각 차트는 표준 Helm 차트 구조를 따른다:

```
charts/<component>/
├── Chart.yaml          # 차트 메타데이터 (이름, 버전, 설명)
├── values.yaml         # 기본 Helm values
├── templates/          # Go 템플릿 파일
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── configmap.yaml
│   └── ...
└── files/              # 정적 파일 (설정 파일 등)
```

### 5.3 차트 로딩과 렌더링

`operator/pkg/helm/helm.go`의 `Render()` 함수가 차트를 로딩하고 렌더링한다:

```go
// operator/pkg/helm/helm.go

func Render(releaseName, namespace string, directory string,
    iop values.Map, kubernetesVersion *version.Info) ([]manifest.Manifest, util.Errors, error) {

    vals, _ := iop.GetPathMap("spec.values")
    installPackagePath := iop.GetPathString("spec.installPackagePath")

    // 내장 또는 로컬 파일시스템에서 차트 로드
    f := manifests.BuiltinOrDir(installPackagePath)
    path := pathJoin("charts", directory)
    chrt, err := loadChart(f, path)

    // Helm 엔진으로 렌더링
    output, warnings, err := renderChart(releaseName, namespace, vals, chrt, kubernetesVersion)

    // 렌더링 결과를 Manifest 객체로 파싱
    mfs, err := manifest.Parse(output)
    return mfs, warnings, err
}
```

렌더링 시 Helm 라이브러리의 `engine.Render()`를 직접 호출한다:

```go
func renderChart(releaseName string, namespace string, vals values.Map,
    chrt *chart.Chart, version *version.Info) ([]string, Warnings, error) {

    options := chartutil.ReleaseOptions{
        Name:      releaseName,
        Namespace: namespace,
    }
    caps := *chartutil.DefaultCapabilities
    // Kubernetes 버전 정보 설정
    if version != nil {
        caps.KubeVersion = chartutil.KubeVersion{
            Version: version.GitVersion,
            Major:   version.Major,
            Minor:   version.Minor,
        }
    }
    helmVals, _ := chartutil.ToRenderValues(chrt, vals, options, &caps)
    files, _ := engine.Render(chrt, helmVals)
    // ...
}
```

### 5.4 차트 소스 - 내장 vs 로컬

차트는 두 가지 소스에서 로딩될 수 있다:

1. **내장(Builtin)**: 바이너리에 `go:embed`로 포함된 차트 (기본)
2. **로컬 파일시스템**: `--manifests` 또는 `spec.installPackagePath`로 지정

`manifests.BuiltinOrDir()` 함수가 소스를 결정한다. 로컬 경로가 지정되면 파일시스템에서 로드하고, 그렇지 않으면 내장 차트를 사용한다.

---

## 6. 컴포넌트 시스템

컴포넌트 시스템은 Istio의 각 구성 요소를 독립적으로 관리하기 위한 프레임워크다.

### 6.1 AllComponents 정의

`operator/pkg/component/component.go`에 모든 컴포넌트가 정의된다:

```go
// operator/pkg/component/component.go

var AllComponents = []Component{
    {
        UserFacingName:       BaseComponentName,      // "Base"
        SpecName:             "base",
        Default:              true,
        HelmSubdir:           "base",
        ToHelmValuesTreeRoot: "global",
        ReleaseName:          "base",
    },
    {
        UserFacingName:       PilotComponentName,     // "Pilot"
        SpecName:             "pilot",
        Default:              true,
        ResourceType:         "Deployment",
        ResourceName:         "istiod",
        ContainerName:        "discovery",
        HelmSubdir:           "istio-control/istio-discovery",
        ToHelmValuesTreeRoot: "pilot",
        ReleaseName:          "istiod",
    },
    {
        UserFacingName:       IngressComponentName,   // "IngressGateways"
        SpecName:             "ingressGateways",
        Multi:                true,                    // 여러 인스턴스 가능
        Default:              true,
        ResourceType:         "Deployment",
        ResourceName:         "istio-ingressgateway",
        ContainerName:        "istio-proxy",
        HelmSubdir:           "gateways/istio-ingress",
        ToHelmValuesTreeRoot: "gateways.istio-ingressgateway",
    },
    // EgressGateways, CNI, Ztunnel ...
}
```

### 6.2 컴포넌트 필드 설명

| 필드 | 설명 |
|------|------|
| `UserFacingName` | 사용자에게 표시되는 이름 (진행 로그 등) |
| `SpecName` | IstioOperator CR에서의 경로 키 |
| `Default` | 프로파일에서 명시하지 않았을 때 기본 활성화 여부 |
| `Multi` | 여러 인스턴스를 가질 수 있는지 (게이트웨이) |
| `ResourceType` | 렌더링된 K8s 리소스 종류 (Deployment, DaemonSet) |
| `ResourceName` | 렌더링된 K8s 리소스 이름 |
| `ContainerName` | Deployment 내 주 컨테이너 이름 |
| `HelmSubdir` | Helm 차트 하위 디렉토리 |
| `ToHelmValuesTreeRoot` | Helm values에서의 루트 경로 |
| `FlattenValues` | values 평탄화 여부 (ztunnel) |
| `ReleaseName` | Helm 릴리즈 이름 (마이그레이션용) |

### 6.3 컴포넌트 의존성

설치 시 컴포넌트 간 의존성이 적용된다. `operator/pkg/install/install.go`에 정의:

```go
// operator/pkg/install/install.go

var componentDependencies = map[component.Name][]component.Name{
    component.PilotComponentName: {
        component.IngressComponentName,    // Pilot 완료 후 Ingress 설치
        component.EgressComponentName,     // Pilot 완료 후 Egress 설치
    },
    component.BaseComponentName: {
        component.CNIComponentName,        // Base 완료 후 CNI 설치
        component.PilotComponentName,      // Base 완료 후 Pilot 설치
    },
    component.CNIComponentName: {
        component.ZtunnelComponentName,    // CNI 완료 후 Ztunnel 설치
    },
}
```

의존성 그래프:

```
Base ──┬──> Pilot ──┬──> IngressGateways
       │            └──> EgressGateways
       └──> CNI ──> Ztunnel
```

### 6.4 컴포넌트 활성화 판단 (Get 메서드)

```go
// operator/pkg/component/component.go

func (c Component) Get(merged values.Map) ([]apis.GatewayComponentSpec, error) {
    defaultNamespace := merged.GetPathString("metadata.namespace")
    def := c.Default

    // AltEnablementPath로 대안 활성화 경로 확인
    if c.AltEnablementPath != "" {
        if merged.GetPathBool(c.AltEnablementPath) {
            def = true
        }
    }

    // Multi 컴포넌트 (게이트웨이): 리스트에서 각 인스턴스의 enabled 확인
    if c.Multi {
        s, ok := merged.GetPath("spec.components." + c.SpecName)
        for _, cur := range s.([]any) {
            spec, _ := buildSpec(m)
            if spec.Enabled.GetValueOrTrue() {
                specs = append(specs, spec)
            }
        }
        return specs, nil
    }

    // 단일 컴포넌트: enabled 확인
    s, ok := merged.GetPathMap("spec.components." + c.SpecName)
    spec, _ := buildSpec(s)
    if !(spec.Enabled.GetValueOrTrue()) {
        return nil, nil  // 비활성화
    }
    return []apis.GatewayComponentSpec{spec}, nil
}
```

핵심: `Enabled`가 명시적으로 `false`로 설정되지 않으면 기본적으로 `true`로 간주된다 (`GetValueOrTrue()`).

---

## 7. install 명령

`istioctl install`은 IstioOperator CR로부터 매니페스트를 생성하고 클러스터에 적용하는 핵심 명령이다.

### 7.1 명령 구조

```go
// operator/cmd/mesh/install.go

type InstallArgs struct {
    InFilenames      []string      // 입력 CR 파일 경로
    ReadinessTimeout time.Duration // 리소스 준비 대기 타임아웃 (기본 300초)
    SkipConfirmation bool          // 확인 프롬프트 건너뛰기
    Force            bool          // 검증 에러 무시
    Verify           bool          // 설치 후 검증
    Set              []string      // --set 플래그
    ManifestsPath    string        // 차트/프로파일 경로
    Revision         string        // 리비전 이름
}
```

### 7.2 Install 함수 - 전체 흐름

```go
// operator/cmd/mesh/install.go

func Install(kubeClient kube.CLIClient, rootArgs *RootArgs,
    iArgs *InstallArgs, stdOut io.Writer, l clog.Logger, p Printer) error {

    // 1단계: K8s 버전 호환성 검사
    k8sversion.IsK8VersionSupported(kubeClient, l)

    // 2단계: Istio 버전 확인 및 EOL 경고
    tag, _ := GetTagVersion(operatorVer.OperatorVersionString)
    if operatorVer.IsEOL() {
        // EOL 경고 출력
    }

    // 3단계: 플래그 별칭 적용 (--manifests, --revision)
    setFlags := applyFlagAliases(iArgs.Set, iArgs.ManifestsPath, iArgs.Revision)

    // 4단계: 매니페스트 생성 (전체 렌더링 파이프라인)
    manifests, vals, _ := render.GenerateManifest(
        iArgs.InFilenames, setFlags, iArgs.Force, kubeClient, l)

    // 5단계: 버전 변경 감지 (업그레이드/다운그레이드 경고)
    detectIstioVersionDiff(p, tag, namespace, kubeClient, revision)

    // 6단계: 사용자 확인 (--skip-confirmation이 아닌 경우)
    if !rootArgs.DryRun && !iArgs.SkipConfirmation {
        prompt := fmt.Sprintf("This will install the Istio %s profile %q...", tag, profile)
        if !Confirm(prompt, stdOut) {
            os.Exit(1)
        }
    }

    // 7단계: Installer로 클러스터에 적용
    i := install.Installer{
        Force:          iArgs.Force,
        DryRun:         rootArgs.DryRun,
        Kube:           kubeClient,
        WaitTimeout:    iArgs.ReadinessTimeout,
        Logger:         l,
        Values:         vals,
        ProgressLogger: progress.NewLog(),
    }
    i.InstallManifests(manifests)

    return nil
}
```

### 7.3 InstallManifests - 클러스터 적용

```go
// operator/pkg/install/install.go

func (i Installer) InstallManifests(manifests []manifest.ManifestSet) error {
    // 1. 시스템 네임스페이스 생성
    i.installSystemNamespace()

    // 2. 웹훅 충돌 사전 검사
    webhook.CheckWebhooks(manifests, i.Values, i.Kube, i.Logger)

    // 3. 매니페스트 적용 (의존성 순서)
    i.install(manifests)

    // 4. 태그 웹훅 배포 (필요시)
    webhooks, _ := webhook.WebhooksToDeploy(i.Values, i.Kube, ownerLabels, i.DryRun)
    for _, wh := range webhooks {
        i.serverSideApply(wh)
    }
    return nil
}
```

### 7.4 병렬 설치와 의존성 관리

`install()` 메서드는 컴포넌트를 병렬로 설치하되, 의존성 순서를 채널로 관리한다:

```go
// operator/pkg/install/install.go

func (i Installer) install(manifests []manifest.ManifestSet) error {
    dependencyWaitCh := dependenciesChannels()

    for _, mf := range manifests {
        wg.Add(1)
        go func() {
            defer wg.Done()
            // 의존성이 있는 컴포넌트는 부모 완료까지 대기
            if s := dependencyWaitCh[c]; s != nil {
                <-s  // 부모 완료 시그널 수신
            }
            // 매니페스트 적용
            i.applyManifestSet(mf)
            // 자식 컴포넌트에게 완료 시그널 전송
            for _, ch := range componentDependencies[c] {
                dependencyWaitCh[ch] <- struct{}{}
            }
        }()
    }
    wg.Wait()

    // 불필요한 리소스 정리 (pruning)
    i.prune(manifests)
    return nil
}
```

### 7.5 Server-Side Apply

모든 리소스 적용은 Kubernetes Server-Side Apply를 사용한다:

```go
// operator/pkg/install/install.go

func (i Installer) serverSideApply(obj manifest.Manifest) error {
    const fieldOwnerOperator = "istio-operator"
    dc, _ := i.Kube.DynamicClientFor(obj.GroupVersionKind(), obj.Unstructured, "")
    dc.Patch(context.TODO(), obj.GetName(), types.ApplyPatchType,
        []byte(obj.Content), metav1.PatchOptions{
            Force:        ptr.Of(true),
            FieldManager: fieldOwnerOperator,
        })
    return nil
}
```

`fieldOwnerOperator = "istio-operator"`를 사용하여 다른 관리자와의 충돌을 방지한다.

### 7.6 매니페스트 정렬 순서

매니페스트 적용 시 리소스 종류에 따라 정렬 우선순위가 적용된다:

```go
// operator/cmd/mesh/manifest-generate.go

func objectKindOrdering(m manifest.Manifest) int {
    switch {
    case gk == "apiextensions.k8s.io/CustomResourceDefinition":
        return -1000     // CRD 먼저
    case gk == "/ServiceAccount" || gk == "rbac.authorization.k8s.io/ClusterRole":
        return 1         // SA/Role 다음
    case gk == "rbac.authorization.k8s.io/ClusterRoleBinding":
        return 2         // RoleBinding
    case gk == "admissionregistration.k8s.io/ValidatingWebhookConfiguration":
        return 3         // Webhook (FAIL-OPEN 리셋용)
    case gk == "/ConfigMap" || gk == "/Secrets":
        return 100       // ConfigMap/Secret
    case gk == "apps/Deployment":
        return 1000      // Deployment
    case gk == "autoscaling/HorizontalPodAutoscaler":
        return 1001      // HPA
    case gk == "/Service":
        return 10000     // Service 마지막
    }
}
```

이 순서의 설계 이유:
- CRD를 먼저 생성해야 해당 CRD의 인스턴스를 생성할 수 있다
- ServiceAccount와 Role이 있어야 RoleBinding을 생성할 수 있다
- ConfigMap/Secret이 있어야 Pod가 마운트할 수 있다
- Deployment가 준비된 후 Service를 생성해야 트래픽을 받을 수 있다

---

## 8. manifest generate 명령

`istioctl manifest generate`는 실제 클러스터에 적용하지 않고 매니페스트만 생성하는 "드라이 런" 명령이다.

### 8.1 명령 구조

```go
// operator/cmd/mesh/manifest-generate.go

type ManifestGenerateArgs struct {
    InFilenames          []string // 입력 CR 파일
    EnableClusterSpecific bool   // 클러스터 특화 설정 자동 감지
    Set                  []string // --set 플래그
    Force                bool     // 검증 에러 무시
    ManifestsPath        string   // 차트/프로파일 경로
    Revision             string   // 리비전
    Filter               []string // 렌더링할 컴포넌트 필터 (숨겨진 플래그)
}
```

### 8.2 실행 흐름

```go
// operator/cmd/mesh/manifest-generate.go

func ManifestGenerate(kubeClient kube.CLIClient, mgArgs *ManifestGenerateArgs, l clog.Logger) error {
    setFlags := applyFlagAliases(mgArgs.Set, mgArgs.ManifestsPath, mgArgs.Revision)

    // 매니페스트 생성 (install과 동일한 파이프라인)
    manifests, _, _ := render.GenerateManifest(
        mgArgs.InFilenames, setFlags, mgArgs.Force, kubeClient, nil)

    // 정렬 후 stdout으로 출력
    for _, manifest := range sortManifestSet(manifests) {
        l.Print(manifest + YAMLSeparator)
    }
    return nil
}
```

`manifest generate`와 `install`의 차이점은 단 하나: 생성된 매니페스트를 클러스터에 적용하지 않고 stdout으로 출력한다는 것이다. 동일한 `render.GenerateManifest()` 파이프라인을 사용하므로 출력 결과가 `install`이 적용하는 내용과 정확히 동일하다.

### 8.3 사용 패턴

```bash
# 기본 매니페스트 생성
istioctl manifest generate > istio.yaml

# demo 프로파일로 생성
istioctl manifest generate --set profile=demo > demo.yaml

# 특정 설정과 함께 생성
istioctl manifest generate --set meshConfig.enableTracing=true > traced.yaml

# 클러스터 특화 설정을 포함하여 생성
istioctl manifest generate --cluster-specific > cluster.yaml

# 생성된 매니페스트를 직접 kubectl로 적용
istioctl manifest generate | kubectl apply -f -
```

---

## 9. manifest translate 명령

`istioctl manifest translate`는 IstioOperator 기반 설치를 Helm 기반 설치로 마이그레이션하기 위한 도구다.

### 9.1 핵심 기능

```go
// operator/cmd/mesh/manifest-translate.go

func ManifestTranslate(kubeClient kube.CLIClient, mgArgs *ManifestTranslateArgs, l clog.Logger) error {
    // 1. istioctl 방식으로 매니페스트 생성 (비교용)
    istioctlGeneratedManifests, _, _ := render.GenerateManifest(
        mgArgs.InFilenames, setFlags, false, kubeClient, nil)

    // 2. Helm 방식으로 마이그레이션 결과 생성
    res, _ := render.Migrate(mgArgs.InFilenames, setFlags, kubeClient)

    // 3. 각 컴포넌트별 마이그레이션 결과물 출력
    for _, info := range res.Components {
        // values.yaml 파일 생성
        write(valuesName, vals.YAML())

        // install 스크립트 생성 (kubectl annotate + helm upgrade)
        commands := []string{
            "kubectl annotate ... meta.helm.sh/release-name=...",
            "kubectl label ... app.kubernetes.io/managed-by=Helm",
            "helm upgrade --install ... -f values.yaml oci://...",
        }
        write(fmt.Sprintf("install-%s.sh", name), strings.Join(commands, "\n"))

        // istioctl vs helm 차이 비교
        if helmManifests != istioctlManifests {
            write("diff-...-helm-output.yaml", helmManifests)
            write("diff-...-istioctl-output.yaml", istioctlManifests)
        }
    }
    // README.md 생성
    write("README.md", readme)
    return nil
}
```

### 9.2 출력 구조

```
output-directory/
├── README.md                        # 마이그레이션 안내
├── istiod-values.yaml               # istiod Helm values
├── install-istiod.sh                # istiod 설치 스크립트
├── base-values.yaml                 # base Helm values
├── install-base.sh                  # base 설치 스크립트
├── diff-istiod-helm-output.yaml     # Helm 렌더링 결과 (비교용)
└── diff-istiod-istioctl-output.yaml # istioctl 렌더링 결과 (비교용)
```

---

## 10. upgrade 명령

`istioctl upgrade`는 사실상 `install` 명령의 별칭(alias)이다.

### 10.1 구현

```go
// operator/cmd/mesh/upgrade.go

type upgradeArgs struct {
    *InstallArgs  // InstallArgs를 그대로 임베드
}

func UpgradeCmd(ctx cli.Context) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "upgrade",
        Short: "Upgrade Istio control plane in-place",
        Long:  "The upgrade command is an alias for the install command",
        RunE: func(cmd *cobra.Command, args []string) (e error) {
            // install과 완전히 동일한 Install() 함수 호출
            return Install(client, rootArgs, upgradeArgs.InstallArgs, cmd.OutOrStdout(), l, p)
        },
    }
    return cmd
}
```

`upgrade`가 `install`과 동일한 이유: Istio의 설치 시스템은 선언적이다. 현재 클러스터 상태와 관계없이, 원하는 상태(IstioOperator CR)를 적용하면 Server-Side Apply가 차이만 적용한다. 따라서 신규 설치와 업그레이드의 구현이 동일하다.

### 10.2 버전 변경 감지

`install` 함수 내의 `detectIstioVersionDiff()`가 업그레이드/다운그레이드를 감지한다:

```go
// operator/cmd/mesh/install.go

func detectIstioVersionDiff(p Printer, tag string, ns string,
    kubeClient kube.CLIClient, revision string) {

    icps, _ := kubeClient.GetIstioVersions(context.TODO(), ns)
    for _, icp := range *icps {
        if icp.Revision != revision {
            continue
        }
        tagVer, _ := GetTagVersion(icp.Info.GitTag)
        icpTags = append(icpTags, tagVer)
    }
    // 최신 설치 버전과 현재 설치하려는 버전 비교
    if icpTag != "" && tag != icpTag {
        if icpTag < tag {
            p.Printf("WARNING: Istio is being upgraded from %s to %s.", icpTag, tag)
        } else {
            p.Printf("WARNING: Istio is being downgraded from %s to %s.", icpTag, tag)
        }
    }
}
```

---

## 11. uninstall 명령

`istioctl uninstall`은 클러스터에서 Istio 리소스를 제거한다. 세 가지 모드를 지원한다:

### 11.1 uninstall 모드

| 모드 | 플래그 | 동작 |
|------|--------|------|
| 리비전별 | `--revision foo` | 특정 리비전의 리소스만 제거 |
| CR 파일 기반 | `-f iop.yaml` | CR에 정의된 리소스 제거 |
| 전체 정리 | `--purge` | 모든 Istio 리소스 제거 (CRD 포함) |

### 11.2 핵심 흐름

```go
// operator/cmd/mesh/uninstall.go

func runUninstall(cmd *cobra.Command, ctx cli.Context,
    rootArgs *RootArgs, uiArgs *uninstallArgs) error {

    // 1. 리비전 존재 확인
    if uiArgs.revision != "" {
        revisions, _ := tag.ListRevisionDescriptions(kubeClient)
        if _, exists := revisions[uiArgs.revision]; !exists {
            return errors.New("could not find target revision")
        }
    }

    // 2. 설정 병합
    values, _ := render.MergeInputs(filenames, setFlags, nil)

    // 3. 정리할 리소스 목록 조회
    objectsList, _ := uninstall.GetPrunedResources(
        kubeClient,
        values.GetPathString("metadata.name"),
        values.GetPathString("metadata.namespace"),
        uiArgs.revision,
        uiArgs.purge,
    )

    // 4. 사전 검사 경고 (프록시 여전히 연결된 경우 등)
    preCheckWarnings(cmd, kubeClient, uiArgs, ctx.IstioNamespace(),
        uiArgs.revision, objectsList, l, rootArgs.DryRun)

    // 5. 리소스 삭제
    uninstall.DeleteObjectsList(kubeClient, rootArgs.DryRun, l, objectsList)
    return nil
}
```

### 11.3 리소스 정리 (Pruning)

`operator/pkg/uninstall/prune.go`에서 정리할 리소스를 레이블 기반으로 조회한다:

```go
// operator/pkg/uninstall/prune.go

func GetPrunedResources(clt kube.CLIClient, iopName, iopNamespace,
    revision string, includeClusterResources bool) ([]*unstructured.UnstructuredList, error) {

    labels := make(map[string]string)
    if revision != "" {
        labels[label.IoIstioRev.Name] = revision
    }
    if iopName != "" {
        labels[manifest.OwningResourceName] = iopName
    }

    // 정리 대상 리소스 유형
    gvkList := append(NamespacedResources(), ClusterCPResources...)
    if includeClusterResources {
        gvkList = append(NamespacedResources(), AllClusterResources...)
    }

    for _, gvk := range gvkList {
        result, _ := c.List(context.Background(), metav1.ListOptions{
            LabelSelector: selector.String(),
        })
        usList = append(usList, result)
    }
    return usList, nil
}
```

정리 대상 리소스 유형:

| 범위 | 리소스 유형 |
|------|-----------|
| 네임스페이스 | Deployment, DaemonSet, Service, ConfigMap, Pod, Secret, ServiceAccount, RoleBinding, Role, PDB, HPA, EnvoyFilter |
| 클러스터 | MutatingWebhookConfiguration, ValidatingWebhookConfiguration, ClusterRole, ClusterRoleBinding |
| 퍼지 전용 | 위 모두 + CustomResourceDefinition, NetworkAttachmentDefinition |

### 11.4 사전 검사 경고

삭제 전 안전 검사:

```go
// operator/cmd/mesh/uninstall.go

func preCheckWarnings(cmd *cobra.Command, kubeClient kube.CLIClient,
    uiArgs *uninstallArgs, istioNamespace, rev string, ...) {

    // 1. 여전히 해당 리비전을 사용하는 프록시가 있는지 확인
    pids, _ := proxyinfo.GetIDsFromProxyInfo(kubeClient, istioNamespace)
    if len(pids) != 0 && rev != "" {
        message += fmt.Sprintf("There are still %d proxies pointing to revision %s",
            len(pids), rev)
    }

    // 2. 게이트웨이가 삭제될 경우 다운타임 경고
    if gwList != "" {
        message += fmt.Sprintf(GatewaysRemovedWarning, gwList)
    }
}
```

---

## 12. Revision 기반 카나리 업그레이드

Istio는 Revision 메커니즘을 통해 안전한 카나리 업그레이드를 지원한다. 여러 버전의 컨트롤 플레인을 동시에 실행하고, 네임스페이스 단위로 점진적으로 마이그레이션할 수 있다.

### 12.1 리비전 개념

```
클러스터 상태 (카나리 업그레이드 중):

┌──────────────────────────────────────────┐
│ istio-system                             │
│                                          │
│  ┌─────────────┐  ┌──────────────────┐   │
│  │ istiod       │  │ istiod-canary    │   │
│  │ (rev=default)│  │ (rev=canary)     │   │
│  │ v1.21        │  │ v1.22            │   │
│  └──────────────┘  └──────────────────┘   │
└──────────────────────────────────────────┘

┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│ namespace-a  │  │ namespace-b  │  │ namespace-c  │
│ rev: default │  │ rev: canary  │  │ rev: default │
│ -> istiod    │  │ -> istiod-   │  │ -> istiod    │
│              │  │    canary    │  │              │
└──────────────┘  └──────────────┘  └──────────────┘
```

### 12.2 리비전 설치

```bash
# 기존 설치 (default revision)
istioctl install

# 새 버전을 canary 리비전으로 설치
istioctl install --revision canary --set profile=default
```

리비전이 설정되면 Pilot 컴포넌트의 리소스 이름에 리비전이 접미사로 추가된다:

```go
// operator/pkg/render/postprocess.go

if comp.UserFacingName == component.PilotComponentName {
    if rev := vals.GetPathStringOr("spec.values.revision", "default"); rev != "default" {
        rn = rn + "-" + rev   // "istiod" -> "istiod-canary"
    }
}
```

### 12.3 리비전 레이블

설치된 모든 리소스에는 리비전 레이블이 부여된다:

```go
// operator/pkg/install/install.go

func getOwnerLabels(iop values.Map, c string) map[string]string {
    labels := make(map[string]string)
    labels[manifest.OperatorManagedLabel] = "Reconcile"
    labels[manifest.OperatorVersionLabel] = version.Info.Version
    // 리비전 레이블
    labels[label.IoIstioRev.Name] = iop.GetPathStringOr(
        "spec.values.revision", "default")
    // ...
}
```

이 레이블들은 다음 목적으로 사용된다:

| 레이블 | 값 | 목적 |
|--------|---|------|
| `operator.istio.io/managed` | `Reconcile` | Operator 관리 리소스 식별 |
| `operator.istio.io/version` | `1.22.0` | 설치된 버전 추적 |
| `operator.istio.io/component` | `Pilot` | 컴포넌트 소속 식별 |
| `istio.io/rev` | `canary` | 리비전 식별 |
| `install.operator.istio.io/owning-resource` | CR 이름 | CR 소유권 추적 |
| `install.operator.istio.io/owning-resource-namespace` | CR NS | CR 소유 네임스페이스 |

### 12.4 카나리 업그레이드 절차

1. 새 리비전 설치: `istioctl install --revision canary`
2. 테스트 네임스페이스 전환: `kubectl label namespace test istio.io/rev=canary`
3. 워크로드 재시작: `kubectl rollout restart deployment -n test`
4. 검증 후 나머지 네임스페이스 전환
5. 기존 리비전 제거: `istioctl uninstall --revision default`

### 12.5 리비전별 정리

```go
// operator/cmd/mesh/uninstall.go
// --revision 플래그로 특정 리비전만 삭제

if uiArgs.revision != "" {
    labels[label.IoIstioRev.Name] = revision
    // 해당 리비전 레이블이 있는 리소스만 정리 대상
}
```

---

## 13. K8s 설정 지원과 후처리

IstioOperator의 `k8s` 필드에 설정된 Kubernetes 리소스 커스터마이징은 Helm 렌더링 후 후처리(postprocess) 단계에서 StrategicMergePatch로 적용된다.

### 13.1 후처리 파이프라인

```go
// operator/pkg/render/postprocess.go

func postProcess(comp component.Component, spec apis.GatewayComponentSpec,
    manifests []manifest.Manifest, vals values.Map) ([]manifest.Manifest, error) {

    if spec.Kubernetes == nil {
        return manifests, nil  // k8s 설정이 없으면 패스스루
    }

    // 패치 매핑 테이블: k8s 필드 -> 패치 템플릿
    patches := map[string]Patch{
        "affinity":       {Kind: rt, Name: rn,
            Patch: `{"spec":{"template":{"spec":{"affinity":%s}}}}`},
        "env":            {Kind: rt, Name: rn,
            Patch: `{"spec":{"template":{"spec":{"containers":[{"name":"discovery", "env": %s}]}}}}`},
        "hpaSpec":        {Kind: "HorizontalPodAutoscaler", Name: rn,
            Patch: `{"spec":%s}`},
        "nodeSelector":   {Kind: rt, Name: rn,
            Patch: `{"spec":{"template":{"spec":{"nodeSelector":%s}}}}`},
        "podDisruptionBudget": {Kind: "PodDisruptionBudget", Name: rn,
            Patch: `{"spec":%s}`, PostProcess: postProcessPodDisruptionBudget},
        "replicaCount":   {Kind: rt, Name: rn,
            Patch: `{"spec":{"replicas":%s}}`},
        "resources":      {Kind: rt, Name: rn,
            Patch: `{"spec":{"template":{"spec":{"containers":[{"name":"discovery", "resources": %s}]}}}}`},
        "tolerations":    {Kind: rt, Name: rn,
            Patch: `{"spec":{"template":{"spec":{"tolerations":%s}}}}`},
        "serviceAnnotations": {Kind: "Service", Name: rn,
            Patch: `{"metadata":{"annotations":%s}}`},
        // ...
    }
    // ...
}
```

### 13.2 패치 적용 과정

```
사용자 설정 (IstioOperator CR):
  spec.components.pilot.k8s.resources.requests.cpu = "500m"

        │
        ▼
1. spec.Raw에서 k8s.resources 값 추출
        │
        ▼
2. 패치 템플릿에 삽입:
   {"spec":{"template":{"spec":{"containers":[
     {"name":"discovery","resources":{"requests":{"cpu":"500m"}}}
   ]}}}}
        │
        ▼
3. 대상 Deployment 매니페스트 매칭 (Kind + Name)
        │
        ▼
4. StrategicMergePatch 적용
        │
        ▼
5. 패치된 매니페스트로 교체
```

### 13.3 PDB 후처리

PodDisruptionBudget은 특별한 후처리가 필요하다. Kubernetes는 `minAvailable`과 `maxUnavailable`을 동시에 설정할 수 없지만, StrategicMergePatch는 이를 자동 처리하지 않는다:

```go
// operator/pkg/render/postprocess.go

func postProcessPodDisruptionBudget(bytes []byte) ([]byte, error) {
    v, _ := values.MapFromJSON(bytes)
    _, hasMax := v.GetPath("spec.maxUnavailable")
    _, hasMin := v.GetPath("spec.minAvailable")
    // 둘 다 있으면 minAvailable 제거
    if hasMax && hasMin {
        v.SetPath("spec.minAvailable", nil)
    }
    return []byte(v.JSON()), nil
}
```

### 13.4 Overlay (임의 패치)

구조화된 K8s 패치로 불충분한 경우, Overlay를 통해 임의 경로에 값을 설정할 수 있다:

```go
// operator/pkg/render/postprocess.go

for _, o := range spec.Kubernetes.Overlays {
    for idx, m := range manifests {
        if o.Kind == m.GetKind() && o.Name == m.GetName() {
            mfs, _ := applyPatches(m, o.Patches)
            manifests[idx] = mfs
        }
    }
}
```

Overlay는 `tpath.WritePathContext()`를 사용하여 YAML 트리 내 임의 경로에 값을 삽입한다.

### 13.5 사용 예시

```yaml
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
spec:
  components:
    pilot:
      k8s:
        # 리소스 제한
        resources:
          requests:
            cpu: 500m
            memory: 2Gi
          limits:
            cpu: 2000m
            memory: 4Gi
        # HPA 설정
        hpaSpec:
          minReplicas: 2
          maxReplicas: 10
          metrics:
          - type: Resource
            resource:
              name: cpu
              target:
                type: Utilization
                averageUtilization: 80
        # PDB 설정
        podDisruptionBudget:
          minAvailable: 1
        # 노드 셀렉터
        nodeSelector:
          topology.kubernetes.io/zone: us-east-1a
        # Affinity
        affinity:
          podAntiAffinity:
            preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                labelSelector:
                  matchExpressions:
                  - key: app
                    operator: In
                    values: [istiod]
                topologyKey: kubernetes.io/hostname
        # 임의 패치 (Overlay)
        overlays:
        - apiVersion: apps/v1
          kind: Deployment
          name: istiod
          patches:
          - path: spec.template.spec.containers.[name:discovery].lifecycle
            value:
              preStop:
                exec:
                  command: ["/bin/sh", "-c", "sleep 5"]
```

---

## 14. values.Map 시스템

`values.Map`은 Istio 오퍼레이터 코드베이스 전반에서 사용되는 비정형(untyped) 맵 래퍼다.

### 14.1 존재 이유

```go
// operator/pkg/values/map.go 주석 발췌

// Map은 비정형 맵의 래퍼다. 직관적이지 않지만 여러 문제를 해결한다:
// - Helm values는 본질적으로 비정형이다
// - unvalidatedValues는 완전히 불투명한 blob이다
// - 많은 타입이 string과 int 모두 허용된다 (예: tag)
// - spec.values.foo.bar=baz 같은 동적 경로 접근이 필요하다
// - protobuf struct 사용 시 golang/gogo protobuf 간 호환 문제
// - 정형 타입은 명시적 설정 vs 기본값 구별이 어렵다
//   (pilot.enabled=false vs 미설정(기본 true) 구별 불가)
```

### 14.2 핵심 메서드

| 메서드 | 설명 |
|--------|------|
| `MergeFrom(other)` | 다른 맵을 현재 맵에 재귀적으로 병합 (other 우선) |
| `GetPath(name)` | 점 표기법 경로로 값 조회 |
| `SetPath(paths, value)` | 점 표기법 경로로 값 설정 |
| `SetSpecPaths(paths...)` | `spec.` 접두사 하에 경로로 값 설정 |
| `GetPathString(s)` | 문자열 값 조회 |
| `GetPathBool(s)` | 불리언 값 조회 |
| `GetPathMap(name)` | 하위 맵 조회 |
| `DeepClone()` | 깊은 복사 |

### 14.3 MergeFrom - 병합 알고리즘

```go
// operator/pkg/values/map.go

func (m Map) MergeFrom(other Map) {
    for k, v := range other {
        if v, ok := v.(map[string]any); ok {
            // 양쪽 모두 맵이면 재귀적으로 병합
            if bv, ok := m[k]; ok {
                if bv, ok := bv.(map[string]any); ok {
                    Map(bv).MergeFrom(v)
                    continue
                }
            }
        }
        // 그 외에는 단순 덮어쓰기
        m[k] = v
    }
}
```

이 병합 알고리즘이 프로파일 시스템의 핵심이다. 맵 내 맵은 재귀적으로 병합되고, 스칼라 값과 배열은 덮어쓴다.

### 14.4 --set 플래그 파싱

```go
func (m Map) SetPaths(paths ...string) error {
    for _, sf := range paths {
        p, v := getPV(sf)   // "key=value" -> ("key", "value")
        var val any = v
        if !isAlwaysString(p) {
            val = parseValue(v)  // 자동 타입 변환 (int, float, bool, string)
        }
        m.SetPath(p, val)
    }
    return nil
}
```

`isAlwaysString`으로 보호되는 경로들은 항상 문자열로 유지된다:

```go
var alwaysString = []string{
    "spec.values.compatibilityVersion",
    "spec.tag",
    "spec.values.global.tag",
    "spec.meshConfig.defaultConfig.proxyMetadata.",
    "spec.compatibilityVersion",
}
```

이는 `tag: 1` 같은 값이 정수 `1`이 아닌 문자열 `"1"`로 처리되도록 보장한다.

---

## 15. 설치 후 리소스 대기와 검증

`operator/pkg/install/wait.go`에서 리소스의 준비 상태를 폴링하여 확인한다.

### 15.1 WaitForResources

```go
// operator/pkg/install/wait.go

func WaitForResources(objects []manifest.Manifest, client kube.Client,
    waitTimeout time.Duration, dryRun bool, l *progress.ManifestLog) error {

    // 즉시 준비 상태 확인 (2초 딜레이 방지)
    if ready, _, _, err := waitForResources(objects, client, l); err == nil && ready {
        return nil
    }

    // 타임아웃까지 2초 간격으로 폴링
    errPoll := wait.PollUntilContextTimeout(context.Background(),
        2*time.Second, waitTimeout, false, func(context.Context) (bool, error) {
            isReady, notReadyObjects, debugInfoObjects, err :=
                waitForResources(objects, client, l)
            notReady = notReadyObjects
            debugInfo = debugInfoObjects
            return isReady, err
        })
    // ...
}
```

### 15.2 리소스별 준비 상태 판단

```go
func waitForResources(objects []manifest.Manifest, k kube.Client,
    l *progress.ManifestLog) (bool, []string, map[string]string, error) {

    for _, o := range objects {
        switch kind {
        case "CustomResourceDefinition":
            // CRD: Established 조건 확인
        case "Namespace":
            // 네임스페이스: Active 상태 확인
        case "Deployment":
            // Deployment: ReadyReplicas >= Spec.Replicas
        case "DaemonSet":
            // DaemonSet: UpdatedNumberScheduled == DesiredNumberScheduled
            //           NumberReady >= DesiredNumberScheduled
        case "StatefulSet":
            // StatefulSet: UpdatedReplicas == expectedReplicas
        }
    }
    // 모든 리소스가 준비되었는지 반환
    isReady := dr && nsr && dsr && stsr && pr && crdr
    return isReady, notReady, resourceDebugInfo, nil
}
```

### 15.3 Deployment 준비 상태 및 장애 진단

```go
func deploymentsReady(cs kubernetes.Interface, deployments []deployment,
    info map[string]string) (bool, []string) {

    for _, v := range deployments {
        if v.replicaSets.Status.ReadyReplicas >= *v.deployment.Spec.Replicas {
            continue  // 준비됨
        }
        // 미준비 원인 추출
        failure := extractPodFailureReason(cs, v.deployment.Namespace,
            v.deployment.Spec.Selector)
        // ContainerStatuses에서 Waiting 상태 확인
        // PodReady 조건이 False인 경우 메시지 확인
    }
}
```

---

## 16. 설계 결정과 아키텍처 분석

### 16.1 왜 Helm을 직접 사용하지 않고 래핑하는가?

Istio가 Helm 라이브러리를 내부에서 사용하면서도 별도의 설치 시스템을 구축한 이유:

1. **통합 API**: IstioOperator CR은 프로파일, 컴포넌트 활성화, K8s 리소스 설정을 단일 API로 통합한다. 순수 Helm에서는 여러 차트에 각각 values를 전달해야 한다.

2. **후처리 파이프라인**: Helm 차트 렌더링 후 StrategicMergePatch를 적용할 수 있어, 차트 자체를 수정하지 않고도 K8s 리소스를 정밀하게 커스터마이징할 수 있다.

3. **컴포넌트 의존성**: 컴포넌트 간 설치 순서를 보장하고, 병렬 설치와 의존성 대기를 동시에 구현한다.

4. **리비전 기반 카나리 업그레이드**: 여러 컨트롤 플레인 버전을 동시에 운영하고 점진적으로 마이그레이션하는 기능은 순수 Helm으로 구현하기 어렵다.

### 16.2 왜 values.Map (비정형 맵)을 사용하는가?

정형 타입(protobuf struct) 대신 비정형 맵을 사용하는 결정적 이유:

1. **nil vs zero-value 구별**: `enabled: false`와 `enabled` 미설정을 구별해야 한다. Go의 `bool`은 기본값이 `false`이므로 구별할 수 없다.

2. **동적 경로 접근**: `--set spec.components.pilot.k8s.resources.requests.cpu=500m` 같은 동적 경로를 리플렉션 없이 처리할 수 있다.

3. **Helm values 호환성**: Helm values는 본질적으로 비정형이며, 직접 전달할 수 있다.

### 16.3 왜 upgrade가 install의 별칭인가?

Istio의 설치 모델은 선언적이다:

- Server-Side Apply는 현재 상태와 원하는 상태의 차이만 적용한다
- 프루닝(pruning) 로직이 더 이상 필요하지 않은 리소스를 자동으로 제거한다
- 버전 감지 로직이 업그레이드/다운그레이드 경고를 제공한다

따라서 "설치"와 "업그레이드"의 구현이 동일하다. `upgrade` 명령은 사용자 UX를 위한 의미적 별칭일 뿐이다.

### 16.4 프루닝(Pruning) 메커니즘

설치 시 프루닝은 더 이상 필요 없는 리소스를 제거한다:

```
현재 설치 매니페스트        클러스터의 기존 리소스
(렌더링 결과)              (레이블로 조회)
        │                         │
        ▼                         ▼
    hash 집합                 hash 집합
        │                         │
        └──────── 비교 ──────────┘
                   │
                   ▼
        클러스터에 있지만 매니페스트에 없는 리소스
                   │
                   ▼
              리소스 삭제
```

이 메커니즘은 컴포넌트를 비활성화하거나, 게이트웨이를 제거하는 경우에 자동으로 관련 리소스가 정리되도록 보장한다.

### 16.5 웹훅 안전성

설치 시 웹훅 충돌 검사는 중요한 안전 메커니즘이다:

```go
// operator/pkg/webhook/webhook.go

func CheckWebhooks(manifests []manifest.ManifestSet, iop values.Map,
    clt kube.Client, logger clog.Logger) error {
    // 1. 새로 설치할 웹훅과 클러스터의 기존 웹훅을 수집
    // 2. webhook.Analyzer로 충돌 분석
    // 3. 충돌이 있으면 에러 반환 (--force로 우회 가능)
}
```

이는 서로 다른 리비전의 웹훅이 동일한 리소스를 잡으려 하는 상황을 방지한다.

---

## 요약

Istio의 Operator와 설치 시스템은 다음과 같은 계층 구조로 동작한다:

```
사용자 입력
  │
  ├── IstioOperator CR 파일 (-f)
  ├── --set 플래그
  └── --revision, --manifests 등
          │
          ▼
┌─────────────────────────────────────┐
│         MergeInputs()               │
│  default 프로파일 + 사용자 설정 병합   │
│  + 클러스터 자동 감지 + 번역          │
└──────────────┬──────────────────────┘
               ▼
┌─────────────────────────────────────┐
│      GenerateManifest()             │
│  컴포넌트별 Helm 렌더링 + 후처리      │
│  (K8s 패치, Overlay)                │
└──────────────┬──────────────────────┘
               ▼
┌─────────────────────────────────────┐
│      InstallManifests()             │
│  네임스페이스 생성 → 웹훅 검사        │
│  → 의존성 기반 병렬 적용              │
│  → Server-Side Apply                │
│  → WaitForResources                 │
│  → Pruning                          │
└─────────────────────────────────────┘
```

이 시스템의 강점은 선언적 API와 프로파일을 통한 추상화, Helm의 차트 렌더링 능력, 그리고 K8s 네이티브 타입을 직접 사용하는 후처리 파이프라인의 조합에 있다. 사용자는 복잡한 Helm values를 직접 다루지 않고도, IstioOperator CR의 구조화된 API를 통해 Istio의 모든 측면을 제어할 수 있다.
