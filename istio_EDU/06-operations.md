# Istio 운영 가이드

## 목차

1. [설치 방법](#1-설치-방법)
2. [핵심 설정](#2-핵심-설정)
3. [주요 포트 정리](#3-주요-포트-정리)
4. [모니터링](#4-모니터링)
5. [디버깅 및 트러블슈팅](#5-디버깅-및-트러블슈팅)
6. [운영 모범 사례](#6-운영-모범-사례)

---

## 1. 설치 방법

Istio는 세 가지 주요 설치 방식을 제공한다: `istioctl install`, IstioOperator CR, Helm 차트.
각 방식은 서로 다른 운영 환경과 자동화 수준에 적합하다.

### 1.1 istioctl install

`istioctl install`은 가장 간단하고 권장되는 설치 방법이다. 프로필(profile)을 선택하여 미리 정의된 컴포넌트 조합을 설치할 수 있다.

```bash
# 기본 프로필로 설치
istioctl install

# 특정 프로필로 설치
istioctl install --set profile=demo

# 설치 전 매니페스트 확인 (dry-run)
istioctl manifest generate --set profile=default | kubectl apply --dry-run=client -f -
```

#### 프로필(Profile) 비교

Istio는 `manifests/profiles/` 디렉토리에 프로필을 정의한다.
소스코드에서 확인되는 프로필 파일들:

| 프로필 | 파일 | 설명 |
|--------|------|------|
| `default` | `manifests/profiles/default.yaml` | 프로덕션 권장. base + pilot + ingressGateway 활성화 |
| `demo` | `manifests/profiles/demo.yaml` | 학습/데모용. egressGateway까지 활성화 |
| `minimal` | `manifests/profiles/minimal.yaml` | 최소 설치. ingressGateway 비활성화, 컨트롤 플레인만 |
| `openshift` | `manifests/profiles/openshift.yaml` | OpenShift 환경. CNI 활성화, platform=openshift 설정 |
| `ambient` | `manifests/profiles/ambient.yaml` | Ambient 모드. CNI + ztunnel 활성화, ingressGateway 비활성화 |
| `empty` | `manifests/profiles/empty.yaml` | 빈 프로필. 커스텀 빌드의 기반 |
| `preview` | `manifests/profiles/preview.yaml` | 실험적 기능 포함 |
| `remote` | `manifests/profiles/remote.yaml` | 멀티클러스터에서 원격 클러스터용 |
| `stable` | `manifests/profiles/stable.yaml` | 안정 채널용 |

#### default 프로필 구조 (소스 확인)

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
      enabled: false      # default에서는 egress 비활성화
  values:
    defaultRevision: ""
    global:
      istioNamespace: istio-system
      configValidation: true
```

#### minimal 프로필 구조

```yaml
# manifests/profiles/minimal.yaml - 컨트롤 플레인만 설치
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
spec:
  components:
    ingressGateways:
    - name: istio-ingressgateway
      enabled: false      # Gateway 없이 Istiod만 설치
```

#### ambient 프로필 구조

```yaml
# manifests/profiles/ambient.yaml
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
spec:
  components:
    cni:
      enabled: true       # Istio CNI 필수
    ztunnel:
      enabled: true       # L4 프록시(ztunnel) 활성화
    ingressGateways:
    - name: istio-ingressgateway
      enabled: false
```

### 1.2 IstioOperator CR 커스터마이징

프로필을 기반으로 세부 설정을 오버라이드할 수 있다.

```yaml
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
metadata:
  namespace: istio-system
  name: istio-control-plane
spec:
  profile: default

  # 컴포넌트별 리소스/레플리카 설정
  components:
    pilot:
      enabled: true
      k8s:
        resources:
          requests:
            cpu: 500m
            memory: 2048Mi
          limits:
            cpu: 1000m
            memory: 4096Mi
        hpaSpec:
          minReplicas: 2
          maxReplicas: 5
          targetAverageUtilization: 80

    ingressGateways:
    - name: istio-ingressgateway
      enabled: true
      k8s:
        service:
          type: LoadBalancer
        resources:
          requests:
            cpu: 100m
            memory: 128Mi

  # MeshConfig 설정
  meshConfig:
    accessLogFile: /dev/stdout
    accessLogEncoding: JSON
    enableTracing: true
    defaultConfig:
      holdApplicationUntilProxyStarts: true
      proxyMetadata:
        ISTIO_META_DNS_CAPTURE: "true"

  # Global 값 오버라이드
  values:
    global:
      proxy:
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 512Mi
      logging:
        level: "default:info"
```

```bash
# IstioOperator CR로 설치
istioctl install -f my-istio-config.yaml

# 또는 kubectl apply 후 operator가 처리
kubectl apply -f my-istio-config.yaml
```

### 1.3 Helm 차트 설치

Istio 소스코드의 `manifests/charts/` 디렉토리에 Helm 차트가 정의되어 있다.
확인된 차트들:

| 차트 | 경로 | 설명 |
|------|------|------|
| `base` | `manifests/charts/base/` | CRD, ClusterRole 등 기본 리소스 |
| `istiod` | `manifests/charts/istio-control/istio-discovery/` | Istiod 컨트롤 플레인 |
| `gateway` | `manifests/charts/gateway/` | Istio 게이트웨이 |
| `istio-cni` | `manifests/charts/istio-cni/` | Istio CNI 플러그인 |
| `ztunnel` | `manifests/charts/ztunnel/` | Ambient 모드의 ztunnel |

```bash
# 1) 네임스페이스 생성
kubectl create namespace istio-system

# 2) base 차트 설치 (CRD 등)
helm install istio-base manifests/charts/base -n istio-system

# 3) istiod 설치
helm install istiod manifests/charts/istio-control/istio-discovery \
  -n istio-system \
  --set global.hub=docker.io/istio \
  --set global.tag=1.24.0

# 4) 게이트웨이 설치 (선택)
helm install istio-ingress manifests/charts/gateway \
  -n istio-system

# Ambient 모드의 경우 추가
helm install istio-cni manifests/charts/istio-cni -n istio-system
helm install ztunnel manifests/charts/ztunnel -n istio-system
```

#### istiod Helm values.yaml 주요 설정 (소스 확인)

`manifests/charts/istio-control/istio-discovery/values.yaml`에서 확인된 주요 기본값:

```yaml
# 오토스케일링
_internal_defaults_do_not_set:
  autoscaleEnabled: true
  autoscaleMin: 1
  autoscaleMax: 5
  replicaCount: 1
  rollingMaxSurge: 100%
  rollingMaxUnavailable: 25%

  # Pilot 리소스 요청량
  resources:
    requests:
      cpu: 500m
      memory: 2048Mi

  # 연결 유지 시간 - 부하 분산 목적
  keepaliveMaxServerConnectionAge: 30m

  # Sidecar Injector 설정
  sidecarInjectorWebhook:
    enableNamespacesByDefault: false
    reinvocationPolicy: Never
    rewriteAppHTTPProbe: true

  # 텔레메트리
  telemetry:
    enabled: true
    v2:
      enabled: true
      prometheus:
        enabled: true

  # MeshConfig
  meshConfig:
    enablePrometheusMerge: true

  # Revision 기반 업그레이드
  revision: ""
  revisionTags: []
```

### 1.4 Revision 기반 카나리 업그레이드

Istio는 revision 메커니즘을 통해 다운타임 없는 컨트롤 플레인 업그레이드를 지원한다.
이 방식은 구 버전과 신 버전 Istiod를 동시에 실행하며 점진적으로 워크로드를 마이그레이션한다.

```
+------------------+     +------------------+
|  Istiod (1.23)   |     |  Istiod (1.24)   |
|  revision: 1-23  |     |  revision: 1-24  |
+--------+---------+     +--------+---------+
         |                         |
    기존 워크로드              새 워크로드
  (istio.io/rev=1-23)     (istio.io/rev=1-24)
```

#### 업그레이드 절차

```bash
# 1단계: 새 revision으로 신버전 Istiod 설치
istioctl install --set revision=1-24 --set tag=1.24.0

# 2단계: 새 Istiod가 정상 동작하는지 확인
kubectl get pods -n istio-system -l app=istiod

# 3단계: 네임스페이스 라벨 변경 (워크로드 마이그레이션)
kubectl label namespace my-app istio.io/rev=1-24 --overwrite
# 기존 라벨 제거
kubectl label namespace my-app istio-injection-

# 4단계: 워크로드 재시작으로 새 사이드카 주입
kubectl rollout restart deployment -n my-app

# 5단계: 모든 프록시가 신버전인지 확인
istioctl proxy-status

# 6단계: 구버전 Istiod 제거
istioctl uninstall --revision 1-23
```

#### Revision 태그

`istioctl tag`를 사용하면 revision에 안정적인 별칭(태그)을 부여할 수 있다.
소스코드 `istioctl/pkg/tag/tag.go`에서 태그 관리 로직을 확인할 수 있다:

```go
// istioctl/pkg/tag/tag.go
type TagDescription struct {
    Tag        string   `json:"tag"`
    Revision   string   `json:"revision"`    // 태그가 가리키는 실제 revision
    Namespaces []string `json:"namespaces"`
}
```

```bash
# "stable" 태그를 revision 1-24에 연결
istioctl tag set stable --revision 1-24

# 네임스페이스는 태그를 참조 (revision이 바뀌어도 네임스페이스 라벨 불변)
kubectl label namespace my-app istio.io/rev=stable

# 업그레이드 시 태그만 변경
istioctl tag set stable --revision 1-25 --overwrite
```

---

## 2. 핵심 설정

### 2.1 MeshConfig

MeshConfig는 Istio 메시 전체에 적용되는 설정이다.
소스코드 `pkg/config/mesh/meshwatcher/collection.go`에서 MeshConfig가 여러 소스로부터 병합되는 것을 확인할 수 있다:

```go
// pkg/config/mesh/meshwatcher/collection.go
func NewCollection(opts krt.OptionsBuilder, sources ...MeshConfigSource) krt.Singleton[MeshConfigResource] {
    return krt.NewSingleton[MeshConfigResource](
        func(ctx krt.HandlerContext) *MeshConfigResource {
            meshCfg := mesh.DefaultMeshConfig()
            for _, source := range sources {
                // ...
                n, err := mesh.ApplyMeshConfig(*s, meshCfg)
                // 여러 소스를 순서대로 병합
            }
            return &MeshConfigResource{meshCfg}
        }, opts.WithName("MeshConfig")...,
    )
}
```

MeshConfig 소스 우선순위:
1. 기본값 (`mesh.DefaultMeshConfig()`)
2. ConfigMap (`istio` ConfigMap의 `mesh` 키)
3. 파일 (로컬 파일 워치)
4. IstioOperator CR의 `meshConfig` 섹션

#### ConfigMap 방식

```bash
# istio ConfigMap 확인
kubectl get configmap istio -n istio-system -o yaml
```

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: istio
  namespace: istio-system
data:
  mesh: |
    accessLogFile: /dev/stdout
    accessLogEncoding: JSON
    enableTracing: true
    defaultConfig:
      discoveryAddress: istiod.istio-system.svc:15012
      holdApplicationUntilProxyStarts: true
      tracing:
        zipkin:
          address: zipkin.istio-system:9411
    enablePrometheusMerge: true
    trustDomain: cluster.local
```

#### 주요 MeshConfig 필드

| 필드 | 설명 | 기본값 |
|------|------|--------|
| `accessLogFile` | 액세스 로그 출력 경로 | `""` (비활성) |
| `accessLogEncoding` | 로그 인코딩 (`TEXT` / `JSON`) | `TEXT` |
| `enableTracing` | 분산 추적 활성화 | `false` |
| `enablePrometheusMerge` | 프로메테우스 메트릭 병합 | `true` |
| `defaultConfig` | ProxyConfig 기본값 | (아래 참조) |
| `trustDomain` | mTLS 인증서의 trust domain | `cluster.local` |
| `outboundTrafficPolicy.mode` | 메시 외부 트래픽 정책 (`ALLOW_ANY` / `REGISTRY_ONLY`) | `ALLOW_ANY` |

### 2.2 ProxyConfig

ProxyConfig는 개별 프록시(Envoy 사이드카)의 동작을 제어한다. MeshConfig의 `defaultConfig` 필드에 전역 기본값을 설정하고, 개별 워크로드에서 오버라이드할 수 있다.

```yaml
# MeshConfig의 defaultConfig (전역 ProxyConfig)
meshConfig:
  defaultConfig:
    discoveryAddress: istiod.istio-system.svc:15012
    holdApplicationUntilProxyStarts: true
    concurrency: 2
    proxyStatsMatcher:
      inclusionRegexps:
      - ".*circuit_breakers.*"
    tracing:
      zipkin:
        address: zipkin.istio-system:9411
      sampling: 1.0
```

#### 워크로드별 ProxyConfig 오버라이드

Pod 어노테이션으로 개별 워크로드의 ProxyConfig를 변경할 수 있다:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  template:
    metadata:
      annotations:
        proxy.istio.io/config: |
          concurrency: 4
          holdApplicationUntilProxyStarts: true
          proxyStatsMatcher:
            inclusionPrefixes:
            - "cluster.outbound"
```

또는 ProxyConfig CR을 사용한다:

```yaml
apiVersion: networking.istio.io/v1beta1
kind: ProxyConfig
metadata:
  name: my-app-proxy-config
  namespace: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  concurrency: 4
  environmentVariables:
    ISTIO_META_DNS_CAPTURE: "true"
```

### 2.3 사이드카 인젝션

Istio의 사이드카 인젝션은 Kubernetes MutatingWebhookAdmission을 통해 동작한다.
소스코드 `pkg/kube/inject/inject.go`에서 인젝션 정책을 확인할 수 있다:

```go
// pkg/kube/inject/inject.go
const (
    // InjectionPolicyDisabled - 기본적으로 인젝션하지 않음
    // "sidecar.istio.io/inject" 라벨이 "true"인 리소스만 인젝션
    InjectionPolicyDisabled InjectionPolicy = "disabled"

    // InjectionPolicyEnabled - 기본적으로 인젝션
    // "sidecar.istio.io/inject" 라벨이 "false"인 리소스는 제외
    InjectionPolicyEnabled InjectionPolicy = "enabled"
)

const (
    ProxyContainerName      = "istio-proxy"       // 사이드카 컨테이너 이름
    ValidationContainerName = "istio-validation"   // CNI 검증 init 컨테이너
    InitContainerName       = "istio-init"         // iptables 설정 init 컨테이너
)
```

#### 네임스페이스 수준 인젝션 제어

```bash
# 방법 1: istio-injection 라벨 (가장 일반적)
kubectl label namespace my-app istio-injection=enabled

# 방법 2: revision 라벨 (카나리 업그레이드 시)
kubectl label namespace my-app istio.io/rev=1-24

# 인젝션 비활성화
kubectl label namespace my-app istio-injection=disabled --overwrite
```

#### Pod 수준 인젝션 제어

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    sidecar.istio.io/inject: "true"     # 강제 인젝션
    # sidecar.istio.io/inject: "false"  # 인젝션 제외
  labels:
    sidecar.istio.io/inject: "true"     # 라벨 방식도 지원
spec:
  containers:
  - name: my-app
    image: my-app:latest
```

#### 인젝션 우선순위 (높은 순서)

1. Pod 라벨 `sidecar.istio.io/inject`
2. Pod 어노테이션 `sidecar.istio.io/inject`
3. Webhook의 `neverInjectSelector` / `alwaysInjectSelector`
4. 네임스페이스 라벨 `istio-injection`
5. 전역 정책 (`enableNamespacesByDefault`)

#### 인젝션 확인

```bash
# 네임스페이스의 인젝션 상태 확인
istioctl analyze -n my-app

# 특정 Pod의 인젝션 여부 사전 확인
istioctl experimental check-inject -n my-app deployment/my-app
```

### 2.4 Ambient 모드

Ambient 모드는 사이드카 없이 메시 기능을 제공하는 새로운 데이터 플레인 모드이다.
ztunnel(L4)과 waypoint 프록시(L7)를 사용한다.

```bash
# 네임스페이스를 Ambient 모드로 전환
kubectl label namespace my-app istio.io/dataplane-mode=ambient

# Ambient 모드 해제
kubectl label namespace my-app istio.io/dataplane-mode-
```

```
사이드카 모드 vs Ambient 모드 아키텍처:

[사이드카 모드]
+----------------------------------+
| Pod                              |
| +------------+  +-------------+  |
| | App        |  | istio-proxy |  |
| | Container  |<>| (Envoy)     |  |
| +------------+  +-------------+  |
+----------------------------------+

[Ambient 모드]
+------------------+    +-----------+    +-------------+
| Pod              |    | ztunnel   |    | waypoint    |
| +------------+   |    | (노드별)  |    | (선택, L7)  |
| | App        |   |--->| L4 암호화 |--->| L7 정책     |
| | Container  |   |    | mTLS      |    | 라우팅      |
| +------------+   |    +-----------+    +-------------+
+------------------+
```

Ambient 프로필 설치 시 활성화되는 컴포넌트:
- **istio-cni**: 네트워크 규칙 설정 (iptables/nftables 대체)
- **ztunnel**: 노드별 DaemonSet으로 배포, L4 mTLS 처리
- **waypoint proxy**: L7 정책이 필요한 경우 선택적 배포

---

## 3. 주요 포트 정리

### 3.1 Istiod 포트

소스코드 `pilot/pkg/bootstrap/server.go`에서 확인한 Istiod의 리스닝 포트:

```go
// pilot/pkg/bootstrap/server.go
// httpMux listens on the httpAddr (8080).
// monitoringMux listens on monitoringAddr(:15014).
// httpsMux listens on the httpsAddr(15017), handling webhooks
```

| 포트 | 프로토콜 | 용도 | 소스 참조 |
|------|---------|------|----------|
| **8080** | HTTP | 레디니스 프로브, 디버그 엔드포인트 | `server.go` httpMux |
| **15010** | gRPC | xDS API (평문, 인증 없음) | `server.go` grpcServer |
| **15012** | gRPC (TLS) | xDS API (mTLS 보안 채널) | `server.go` secureGrpcServer |
| **15014** | HTTP | 모니터링 (Prometheus 메트릭, pprof) | `server.go` monitoringMux |
| **15017** | HTTPS | Webhook (사이드카 인젝션, 설정 검증) | `server.go` httpsMux |

```
Istiod 포트 다이어그램:

                    +-----------------------+
                    |       Istiod          |
                    |                       |
 Readiness ------> | :8080  (HTTP)         |
                    |                       |
 Envoy xDS ------> | :15010 (gRPC plain)   |  <-- 레거시, 보안 없음
                    |                       |
 Envoy xDS ------> | :15012 (gRPC mTLS)    |  <-- 권장 채널
                    |                       |
 Prometheus ------> | :15014 (HTTP)         |  /metrics 엔드포인트
                    |                       |
 K8s Webhooks ----> | :15017 (HTTPS)        |  인젝션 + 검증
                    +-----------------------+
```

> **보안 참고**: 포트 15010(평문 gRPC)은 인증 없이 xDS 구성을 제공한다.
> `pilot/pkg/xds/auth.go`의 주석에 따르면, 이 스트림에서는 인증이 수행되지 않으므로
> 프로덕션에서는 15012(mTLS) 채널만 사용해야 한다.

### 3.2 Envoy 사이드카 포트

소스코드의 시뮬레이션 테스트(`pilot/pkg/simulation/traffic.go`)와 에이전트 테스트
(`pkg/istio-agent/agent_test.go`)에서 확인한 포트:

| 포트 | 방향 | 용도 | 소스 참조 |
|------|------|------|----------|
| **15001** | 아웃바운드 | iptables REDIRECT 대상 (아웃바운드 트래픽) | `simulation/traffic.go` CallModeOutbound |
| **15006** | 인바운드 | iptables REDIRECT 대상 (인바운드 트래픽) | `simulation/traffic.go` CallModeInbound |
| **15008** | 인바운드 | HBONE (HTTP/2 기반 터널링) | `model/listener.go` HBoneInboundListenPort |
| **15020** | - | 병합된 Prometheus 메트릭 (앱 + 프록시) | `agent_test.go` StatusPort=15020 |
| **15021** | - | 헬스체크 (Envoy 상태) | `agent_test.go` EnvoyStatusPort=15021 |
| **15090** | - | Envoy 네이티브 Prometheus 통계 | `agent_test.go` EnvoyPrometheusPort=15090 |

```
Envoy 사이드카 포트 흐름:

[인바운드 트래픽]
외부 요청 ---> iptables REDIRECT ---> :15006 (Envoy) ---> App Container

[아웃바운드 트래픽]
App Container ---> iptables REDIRECT ---> :15001 (Envoy) ---> 외부 서비스

[HBONE 터널링 (Ambient)]
ztunnel/waypoint ---> :15008 (HTTP/2 tunnel) ---> App Container

[메트릭 수집]
Prometheus ---> :15020 (병합 메트릭: 앱 + Envoy)
Prometheus ---> :15090 (Envoy 자체 메트릭만)

[헬스체크]
kubelet ---> :15021 (healthz/ready)
```

#### 포트 15020 vs 15090 차이

| 항목 | 15020 | 15090 |
|------|-------|-------|
| 제공 주체 | istio-agent (pilot-agent) | Envoy 직접 |
| 내용 | 앱 메트릭 + Envoy 메트릭 병합 | Envoy 통계만 |
| 활성 조건 | `enablePrometheusMerge: true` | 항상 활성 |
| 엔드포인트 | `/stats/prometheus` | `/stats/prometheus` |

### 3.3 Ztunnel 포트 (Ambient 모드)

| 포트 | 용도 |
|------|------|
| **15001** | 아웃바운드 트래픽 인터셉트 |
| **15006** | 인바운드 트래픽 인터셉트 |
| **15008** | HBONE 터널 리스닝 |

ztunnel은 사이드카와 동일한 포트 번호를 사용하지만, 노드 수준에서 동작하며
L4(TCP) 처리만 수행한다는 점이 다르다.

---

## 4. 모니터링

### 4.1 Prometheus 메트릭 수집

Istio는 컨트롤 플레인(Istiod)과 데이터 플레인(Envoy)에서 Prometheus 메트릭을 노출한다.

| 대상 | 엔드포인트 | 설명 |
|------|-----------|------|
| Istiod | `:15014/metrics` | 컨트롤 플레인 메트릭 |
| Envoy | `:15090/stats/prometheus` | Envoy 네이티브 메트릭 |
| 병합 | `:15020/stats/prometheus` | 앱 + Envoy 병합 메트릭 |

#### Prometheus 스크래핑 설정 예시

```yaml
# Istiod 메트릭 수집
- job_name: 'istiod'
  kubernetes_sd_configs:
  - role: pod
    namespaces:
      names: ['istio-system']
  relabel_configs:
  - source_labels: [__meta_kubernetes_pod_label_app]
    regex: istiod
    action: keep
  - source_labels: [__address__]
    regex: '([^:]+)(?::\d+)?'
    replacement: '${1}:15014'
    target_label: __address__

# Envoy 사이드카 메트릭 수집
- job_name: 'envoy-stats'
  metrics_path: /stats/prometheus
  kubernetes_sd_configs:
  - role: pod
  relabel_configs:
  - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
    regex: true
    action: keep
  - source_labels: [__address__]
    regex: '([^:]+)(?::\d+)?'
    replacement: '${1}:15020'
    target_label: __address__
```

### 4.2 핵심 메트릭

소스코드 `pilot/pkg/xds/monitoring.go`에서 정의된 Istiod 핵심 메트릭:

#### 컨트롤 플레인 메트릭 (Istiod)

```go
// pilot/pkg/xds/monitoring.go
pilot_xds_pushes          // xDS 푸시 횟수 (타입별: cds, eds, lds, rds)
pilot_proxy_convergence_time  // 설정 변경 -> 프록시 수신 완료까지 시간
pilot_services            // Pilot이 인지한 총 서비스 수
pilot_xds                 // xDS로 연결된 엔드포인트(프록시) 수
pilot_debounce_time       // 디바운싱 지연 시간
pilot_pushcontext_init_seconds  // PushContext 초기화 시간
pilot_xds_push_time       // xDS 푸시 소요 시간
pilot_proxy_queue_time    // 프록시가 푸시 큐에서 대기한 시간
pilot_push_triggers       // 푸시 트리거 횟수 (원인별)
pilot_inbound_updates     // 인바운드 업데이트 수 (config, eds, svc 등)
pilot_sds_certificate_errors_total  // SDS 인증서 오류 수
pilot_xds_config_size_bytes  // 클라이언트에 푸시된 설정 크기 분포
```

| 메트릭 | 유형 | 설명 | 알림 기준 |
|--------|------|------|----------|
| `pilot_xds_pushes` | Counter | xDS 푸시 총 횟수 | `senderr` 타입 증가 시 경고 |
| `pilot_proxy_convergence_time` | Histogram | 설정 수렴 시간 | p99 > 10s 시 경고 |
| `pilot_xds` | Gauge | 연결된 프록시 수 | 기대치 대비 급감 시 경고 |
| `pilot_services` | Gauge | 인지된 서비스 수 | 갑작스러운 변화 시 경고 |
| `pilot_xds_push_time` | Histogram | 푸시 소요 시간 | p99 > 30s 시 경고 |
| `pilot_sds_certificate_errors_total` | Counter | 인증서 오류 | 증가 시 즉시 경고 |

#### 데이터 플레인 메트릭 (Envoy)

소스코드 `istioctl/pkg/metrics/metrics.go`에서 확인:

```go
// istioctl/pkg/metrics/metrics.go
reqTot = "istio_requests_total"  // HTTP 요청 총 수
```

| 메트릭 | 유형 | 주요 라벨 | 설명 |
|--------|------|----------|------|
| `istio_requests_total` | Counter | `source_workload`, `destination_workload`, `response_code`, `reporter` | HTTP 요청 총 수 |
| `istio_request_duration_milliseconds` | Histogram | 동일 | 요청 처리 시간 |
| `istio_request_bytes` | Histogram | 동일 | 요청 크기 |
| `istio_response_bytes` | Histogram | 동일 | 응답 크기 |
| `istio_tcp_connections_opened_total` | Counter | 동일 | TCP 연결 열림 수 |
| `istio_tcp_connections_closed_total` | Counter | 동일 | TCP 연결 닫힘 수 |
| `istio_tcp_sent_bytes_total` | Counter | 동일 | TCP 송신 바이트 |
| `istio_tcp_received_bytes_total` | Counter | 동일 | TCP 수신 바이트 |

#### 유용한 PromQL 쿼리

```promql
# 서비스별 요청 성공률 (5xx 제외)
sum(rate(istio_requests_total{response_code!~"5.*", reporter="destination"}[5m]))
by (destination_workload, destination_workload_namespace)
/
sum(rate(istio_requests_total{reporter="destination"}[5m]))
by (destination_workload, destination_workload_namespace)

# xDS 푸시 오류율
sum(rate(pilot_xds_pushes{type=~".*senderr"}[5m]))

# 프록시 수렴 시간 p99
histogram_quantile(0.99, sum(rate(pilot_proxy_convergence_time_bucket[5m])) by (le))

# 연결된 프록시 수 (버전별)
sum(pilot_xds) by (version)
```

### 4.3 Grafana 대시보드

Istio는 세 가지 수준의 Grafana 대시보드를 제공한다:

| 대시보드 | 관점 | 주요 패널 |
|---------|------|----------|
| **Mesh Dashboard** | 전체 메시 | 글로벌 요청량, 성공률, p50/p90/p99 레이턴시 |
| **Service Dashboard** | 서비스별 | 서비스 인바운드/아웃바운드 트래픽, 에러율 |
| **Workload Dashboard** | 워크로드별 | 워크로드 CPU/메모리, 요청 상세, TCP 연결 |
| **Control Plane Dashboard** | Istiod | xDS 푸시, 수렴 시간, 리소스 사용량 |
| **Performance Dashboard** | 성능 | Envoy CPU/메모리, 연결 수 |

---

## 5. 디버깅 및 트러블슈팅

### 5.1 istioctl proxy-status

`istioctl proxy-status`(줄여서 `istioctl ps`)는 모든 프록시의 xDS 동기화 상태를 보여준다.
소스코드 `istioctl/pkg/proxystatus/proxystatus.go`에서 구현을 확인할 수 있다.

```bash
$ istioctl proxy-status
NAME                    CLUSTER   CDS    LDS    EDS    RDS    ECDS   ISTIOD                     VERSION
httpbin-abc123.default  Kubernetes SYNCED SYNCED SYNCED SYNCED        istiod-xyz456.istio-system 1.24.0
sleep-def789.default    Kubernetes SYNCED SYNCED SYNCED SYNCED        istiod-xyz456.istio-system 1.24.0
```

#### 동기화 상태 의미

| 상태 | 의미 | 조치 |
|------|------|------|
| `SYNCED` | 정상. Envoy가 Istiod와 동기화됨 | 없음 |
| `NOT SENT` | Istiod가 해당 타입을 보내지 않음 | 정상일 수 있음 (해당 리소스 미사용) |
| `STALE` | Istiod가 보냈으나 Envoy가 ACK하지 않음 | 네트워크 문제 또는 Envoy 오류 조사 |

```bash
# 특정 프록시의 상세 동기화 비교
istioctl proxy-status <pod-name>.<namespace>

# 파일 기반 비교
istioctl proxy-status <pod-name>.<namespace> --file envoy-config.json
```

### 5.2 istioctl proxy-config

`istioctl proxy-config`(줄여서 `istioctl pc`)는 Envoy의 실제 설정을 덤프한다.
소스코드 `istioctl/pkg/proxyconfig/proxyconfig.go`에서 지원하는 서브커맨드:

```go
// istioctl/pkg/proxyconfig/proxyconfig.go
const (
    jsonOutput             = "json"
    yamlOutput             = "yaml"
    summaryOutput          = "short"
    prometheusOutput       = "prom"
    prometheusMergedOutput = "prom-merged"
)
```

#### clusters (업스트림 클러스터 목록)

```bash
# 모든 클러스터 조회
istioctl proxy-config clusters <pod-name>.<namespace>

# 특정 FQDN의 클러스터
istioctl proxy-config clusters <pod-name>.<namespace> --fqdn reviews.default.svc.cluster.local

# JSON 출력
istioctl proxy-config clusters <pod-name>.<namespace> -o json
```

출력 예시:
```
SERVICE FQDN                          PORT   SUBSET   DIRECTION   TYPE      DESTINATION RULE
reviews.default.svc.cluster.local     9080   -        outbound    EDS
reviews.default.svc.cluster.local     9080   v1       outbound    EDS       reviews-dr.default
reviews.default.svc.cluster.local     9080   v2       outbound    EDS       reviews-dr.default
```

#### listeners (리스너 목록)

```bash
# 모든 리스너 조회
istioctl proxy-config listeners <pod-name>.<namespace>

# 특정 포트의 리스너
istioctl proxy-config listeners <pod-name>.<namespace> --port 15001

# 아웃바운드 리스너만
istioctl proxy-config listeners <pod-name>.<namespace> --type SIDECAR_OUTBOUND
```

#### routes (라우팅 테이블)

```bash
# 모든 라우트 조회
istioctl proxy-config routes <pod-name>.<namespace>

# 특정 라우트 이름
istioctl proxy-config routes <pod-name>.<namespace> --name 9080
```

#### endpoints (엔드포인트 목록)

```bash
# 모든 엔드포인트 조회
istioctl proxy-config endpoints <pod-name>.<namespace>

# 특정 클러스터의 엔드포인트
istioctl proxy-config endpoints <pod-name>.<namespace> \
  --cluster "outbound|9080||reviews.default.svc.cluster.local"

# 상태별 필터링
istioctl proxy-config endpoints <pod-name>.<namespace> --status healthy
```

#### 전체 설정 덤프

```bash
# Envoy 전체 설정 덤프 (JSON)
istioctl proxy-config all <pod-name>.<namespace> -o json > envoy-config-dump.json
```

### 5.3 istioctl analyze

`istioctl analyze`는 Istio 설정의 잠재적 문제를 사전에 탐지한다.
소스코드 `istioctl/pkg/analyze/analyze.go`와 `pkg/config/analysis/analyzers/` 디렉토리에
다양한 분석기가 구현되어 있다.

```bash
# 현재 클러스터의 모든 네임스페이스 분석
istioctl analyze --all-namespaces

# 특정 네임스페이스 분석
istioctl analyze -n my-app

# YAML 파일 분석 (클러스터 없이도 가능)
istioctl analyze my-virtualservice.yaml my-destinationrule.yaml

# 특정 메시지 타입만 표시
istioctl analyze -n my-app --output-threshold Error
```

출력 예시:
```
Warning [IST0101] (VirtualService reviews.default) Referenced host not found: "reviews"
Warning [IST0108] (Service reviews.default) Unknown annotation: networking.istio.io/exportTo
Error   [IST0145] (Gateway httpbin-gw.default) Conflict with Gateway httpbin-gw2.default
Info    [IST0118] (Service ratings.default) Port name must follow <protocol>[-suffix] format
```

#### 주요 분석 메시지 코드

| 코드 | 심각도 | 설명 |
|------|--------|------|
| IST0101 | Warning | VirtualService가 참조하는 호스트를 찾을 수 없음 |
| IST0104 | Warning | Gateway가 참조하는 호스트를 가진 VirtualService 없음 |
| IST0108 | Warning | 알 수 없는 어노테이션 |
| IST0118 | Info | 포트 이름이 프로토콜-접미사 형식이 아님 |
| IST0131 | Warning | VirtualService의 subset이 DestinationRule에 정의되지 않음 |
| IST0145 | Error | Gateway 간 충돌 (동일 호스트+포트) |

### 5.4 자주 발생하는 문제와 해결

#### 문제 1: 프록시가 설정을 수신하지 못함

**증상**: `istioctl proxy-status`에서 `STALE` 상태 또는 프록시가 목록에 없음

**진단 절차**:
```bash
# 1. Istiod 로그 확인
kubectl logs -n istio-system -l app=istiod --tail=100

# 2. 프록시 로그 확인
kubectl logs <pod-name> -c istio-proxy --tail=100

# 3. Istiod 연결 확인
istioctl proxy-config clusters <pod-name>.<namespace> | grep istiod

# 4. Envoy 관리 포트 직접 접근
kubectl exec <pod-name> -c istio-proxy -- curl -s localhost:15000/server_info
```

**일반적 원인과 해결**:
- Istiod 서비스 DNS 해석 실패 -> DNS 정책 및 CoreDNS 확인
- 네트워크 정책이 15012 포트 차단 -> NetworkPolicy 수정
- Istiod 리소스 부족 -> HPA 설정 및 리소스 확인

#### 문제 2: mTLS 실패

**증상**: 서비스 간 통신에서 `503 UC` 또는 `connection reset` 오류

**진단 절차**:
```bash
# 1. PeerAuthentication 확인
kubectl get peerauthentication --all-namespaces

# 2. DestinationRule의 TLS 설정 확인
kubectl get destinationrule --all-namespaces -o yaml | grep -A5 tls

# 3. 인증서 상태 확인
istioctl proxy-config secret <pod-name>.<namespace>

# 4. mTLS 모드 확인
istioctl authn tls-check <pod-name>.<namespace> <target-service>
```

**일반적 원인과 해결**:
- PeerAuthentication이 `STRICT`인데 상대가 mTLS 미지원 -> `PERMISSIVE`로 전환 또는 사이드카 인젝션
- 인증서 만료 -> Istiod CA 상태 및 인증서 갱신 확인
- trust domain 불일치 -> MeshConfig의 `trustDomain` 확인

#### 문제 3: 사이드카 인젝션 실패

**증상**: Pod에 `istio-proxy` 컨테이너가 없음

**진단 절차**:
```bash
# 1. 네임스페이스 라벨 확인
kubectl get namespace my-app --show-labels

# 2. Webhook 설정 확인
kubectl get mutatingwebhookconfigurations | grep istio

# 3. Webhook 로그 확인
kubectl logs -n istio-system -l app=istiod | grep "injection"

# 4. 인젝션 시뮬레이션
istioctl experimental check-inject -n my-app deployment/my-app
```

**일반적 원인과 해결**:
- 네임스페이스에 `istio-injection=enabled` 라벨 누락 -> 라벨 추가
- Pod에 `sidecar.istio.io/inject: "false"` 어노테이션 -> 제거
- Webhook caBundle 만료 -> Istiod 재시작
- `kube-system`, `istio-system` 등 시스템 네임스페이스는 기본 제외

#### 문제 4: DNS 해석 오류

**증상**: `503 NR` (No Route) 또는 `EDS no healthy upstream`

**진단 절차**:
```bash
# 1. 서비스 존재 확인
kubectl get svc -n <namespace>

# 2. 엔드포인트 확인
istioctl proxy-config endpoints <pod-name>.<namespace> --cluster "outbound|<port>||<svc-fqdn>"

# 3. Envoy 리스너/라우트 확인
istioctl proxy-config listeners <pod-name>.<namespace> --port <port>
istioctl proxy-config routes <pod-name>.<namespace> --name <port>
```

---

## 6. 운영 모범 사례

### 6.1 사이드카 리소스 제한 설정

프로덕션 환경에서는 사이드카 프록시의 리소스 요청/제한을 반드시 설정해야 한다.
리소스 미설정 시 메시 전체가 노드 리소스를 과도하게 소비할 수 있다.

```yaml
# IstioOperator를 통한 전역 프록시 리소스 설정
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
spec:
  values:
    global:
      proxy:
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 512Mi
```

워크로드별 리소스 오버라이드:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: high-traffic-app
spec:
  template:
    metadata:
      annotations:
        sidecar.istio.io/proxyCPU: "200m"
        sidecar.istio.io/proxyCPULimit: "1000m"
        sidecar.istio.io/proxyMemory: "256Mi"
        sidecar.istio.io/proxyMemoryLimit: "1Gi"
```

#### 리소스 산정 가이드라인

| 트래픽 수준 | CPU 요청 | CPU 제한 | 메모리 요청 | 메모리 제한 |
|------------|---------|---------|------------|------------|
| 저 (< 100 RPS) | 50m | 200m | 64Mi | 256Mi |
| 중 (100-1000 RPS) | 100m | 500m | 128Mi | 512Mi |
| 고 (> 1000 RPS) | 500m | 2000m | 256Mi | 1Gi |

### 6.2 Revision 기반 제로 다운타임 업그레이드

프로덕션 업그레이드에서 반드시 revision 기반 카나리 방식을 사용한다:

```
업그레이드 순서:

1. 새 revision으로 신버전 Istiod 설치
   istioctl install --set revision=canary

2. 태그를 사용한 점진적 마이그레이션
   istioctl tag set canary --revision=1-24

3. 테스트 네임스페이스부터 전환
   kubectl label ns test istio.io/rev=canary --overwrite

4. 메트릭/로그로 이상 없음 확인
   istioctl proxy-status | grep canary

5. 나머지 네임스페이스 순차 전환

6. 구버전 Istiod 제거
   istioctl uninstall --revision=1-23
```

**주의사항**:
- 업그레이드 시 CRD가 호환되는지 확인 (`istioctl manifest diff`)
- 게이트웨이는 별도 업그레이드 (인그레스 게이트웨이 다운타임 주의)
- 한 번에 두 단계 이상 건너뛰는 업그레이드는 지원하지 않음

### 6.3 Sidecar 리소스를 통한 네임스페이스 격리

`Sidecar` CR을 사용하면 각 네임스페이스의 프록시가 수신하는 설정 범위를 제한할 수 있다.
이는 대규모 메시에서 xDS 설정 크기와 메모리 사용량을 크게 줄인다.

```yaml
# 네임스페이스별 Sidecar 리소스 (권장)
apiVersion: networking.istio.io/v1beta1
kind: Sidecar
metadata:
  name: default
  namespace: my-app
spec:
  egress:
  - hosts:
    - "./*"                        # 같은 네임스페이스의 모든 서비스
    - "istio-system/*"             # istio-system의 서비스
    - "shared-services/api-gw.shared-services.svc.cluster.local"  # 특정 서비스
  outboundTrafficPolicy:
    mode: REGISTRY_ONLY            # 등록된 서비스만 허용
```

#### Sidecar 리소스 미적용 시 영향

```
Sidecar 리소스 없이:
+--------------------------------------------------+
| Envoy Config                                     |
| +----------------------------------------------+ |
| | 전체 메시의 모든 서비스 엔드포인트 (수천 개)    | |
| | -> 대량의 메모리 소비                          | |
| | -> xDS 푸시 시간 증가                          | |
| | -> 설정 수렴 시간 증가                         | |
| +----------------------------------------------+ |
+--------------------------------------------------+

Sidecar 리소스 적용 후:
+----------------------------+
| Envoy Config               |
| +------------------------+ |
| | 필요한 서비스만 (수십 개)| |
| | -> 메모리 절약           | |
| | -> 빠른 푸시             | |
| | -> 빠른 수렴             | |
| +------------------------+ |
+----------------------------+
```

> **소스 참조**: `pilot/pkg/xds/monitoring.go`의 `pilot_xds_config_size_bytes` 메트릭으로
> 클라이언트에 푸시되는 설정 크기를 모니터링할 수 있다. 크기가 4MB(gRPC 기본 제한)에 근접하면
> Sidecar 리소스 적용이 반드시 필요하다.

### 6.4 인증서 로테이션 모니터링

Istio는 내부 CA(Citadel)를 통해 워크로드 인증서를 자동 발급하고 갱신한다.
인증서 만료는 mTLS 실패로 직결되므로 모니터링이 필수적이다.

#### 인증서 상태 확인

```bash
# 워크로드 인증서 확인
istioctl proxy-config secret <pod-name>.<namespace>

# 출력 예시:
# RESOURCE NAME     TYPE           STATUS   VALID CERT   SERIAL NUMBER   NOT AFTER               NOT BEFORE
# default           Cert Chain     ACTIVE   true         abc123...       2024-12-01T00:00:00Z    2024-11-01T00:00:00Z
# ROOTCA            CA             ACTIVE   true         def456...       2034-11-01T00:00:00Z    2024-11-01T00:00:00Z
```

#### 모니터링할 메트릭

| 메트릭 | 설명 | 알림 조건 |
|--------|------|----------|
| `pilot_sds_certificate_errors_total` | SDS 인증서 오류 수 | > 0 |
| `citadel_server_root_cert_expiry_timestamp` | 루트 CA 만료 타임스탬프 | 30일 이내 |
| `istio_agent_cert_expiry_seconds` | 워크로드 인증서 만료까지 남은 시간 | < 3600 (1시간) |

#### 루트 CA 인증서 갱신

```bash
# 현재 CA 인증서 확인
kubectl get secret istio-ca-secret -n istio-system -o jsonpath='{.data.ca-cert\.pem}' | base64 -d | openssl x509 -noout -dates

# 커스텀 CA 인증서 교체 시
kubectl create secret generic cacerts -n istio-system \
  --from-file=ca-cert.pem \
  --from-file=ca-key.pem \
  --from-file=root-cert.pem \
  --from-file=cert-chain.pem

# Istiod 재시작으로 새 CA 적용
kubectl rollout restart deployment/istiod -n istio-system
```

### 6.5 추가 운영 권장 사항

#### 컨트롤 플레인 가용성

```yaml
# Istiod PodDisruptionBudget (기본 활성)
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: istiod
  namespace: istio-system
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: istiod
```

- Istiod를 최소 2개 레플리카로 운영
- `keepaliveMaxServerConnectionAge: 30m` (기본값)으로 프록시 연결을 분산
- 멀티클러스터 환경에서는 각 클러스터에 로컬 Istiod 배포 권장

#### 액세스 로그 설정

```yaml
meshConfig:
  accessLogFile: /dev/stdout
  accessLogEncoding: JSON
  accessLogFormat: |
    {
      "start_time": "%START_TIME%",
      "method": "%REQ(:METHOD)%",
      "path": "%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%",
      "protocol": "%PROTOCOL%",
      "response_code": "%RESPONSE_CODE%",
      "response_flags": "%RESPONSE_FLAGS%",
      "upstream_host": "%UPSTREAM_HOST%",
      "duration": "%DURATION%"
    }
```

#### 디버그 로깅 (임시)

```bash
# Istiod 로그 레벨 동적 변경
kubectl exec -n istio-system deploy/istiod -- \
  curl -XPUT localhost:8080/debug/log?level=debug

# 특정 프록시의 Envoy 로그 레벨 변경
istioctl proxy-config log <pod-name>.<namespace> --level debug

# 특정 컴포넌트만 디버그
istioctl proxy-config log <pod-name>.<namespace> --level connection:debug,router:info
```

#### outboundTrafficPolicy 설정

```yaml
# REGISTRY_ONLY: 메시에 등록된 서비스만 접근 허용 (보안 강화)
meshConfig:
  outboundTrafficPolicy:
    mode: REGISTRY_ONLY

# 외부 서비스 접근이 필요한 경우 ServiceEntry 생성
apiVersion: networking.istio.io/v1beta1
kind: ServiceEntry
metadata:
  name: external-api
spec:
  hosts:
  - api.external.com
  ports:
  - number: 443
    name: https
    protocol: HTTPS
  resolution: DNS
  location: MESH_EXTERNAL
```

---

## 부록: 운영 체크리스트

### 설치 전 체크리스트

- [ ] Kubernetes 버전 호환성 확인
- [ ] 필요한 프로필 선택 (default / ambient / minimal)
- [ ] 리소스 요구사항 산정 (Istiod, 사이드카, 게이트웨이)
- [ ] 네트워크 정책 검토 (필요 포트 개방)
- [ ] 기존 워크로드 영향도 분석

### 운영 중 체크리스트

- [ ] `pilot_xds_pushes` senderr 모니터링
- [ ] `pilot_proxy_convergence_time` p99 < 10s 유지
- [ ] `pilot_sds_certificate_errors_total` = 0 유지
- [ ] 루트 CA 인증서 만료일 모니터링 (30일 전 갱신)
- [ ] Sidecar 리소스로 네임스페이스 격리 적용
- [ ] 정기적 `istioctl analyze` 실행

### 업그레이드 체크리스트

- [ ] 릴리스 노트에서 breaking change 확인
- [ ] revision 기반 카나리 업그레이드 사용
- [ ] 테스트 네임스페이스에서 먼저 검증
- [ ] `istioctl proxy-status`로 모든 프록시 동기화 확인
- [ ] 메트릭 이상 없음 확인 후 나머지 전환
- [ ] 구버전 Istiod 정리
