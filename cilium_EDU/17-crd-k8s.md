# 17. CRD와 Kubernetes 통합

## 목차

1. [개요](#1-개요)
2. [CRD 타입 정의](#2-crd-타입-정의)
3. [CRD 등록과 라이프사이클](#3-crd-등록과-라이프사이클)
4. [K8s Resource 프레임워크](#4-k8s-resource-프레임워크)
5. [K8s 클라이언트 아키텍처](#5-k8s-클라이언트-아키텍처)
6. [Slim Clientset](#6-slim-clientset)
7. [리소스 워처](#7-리소스-워처)
8. [동기화 추적](#8-동기화-추적)
9. [Hive DI 통합](#9-hive-di-통합)
10. [왜 이 아키텍처인가?](#10-왜-이-아키텍처인가)
11. [참고 파일 목록](#11-참고-파일-목록)

---

## 1. 개요

Cilium은 Kubernetes의 **Custom Resource Definition(CRD)** 메커니즘을 활용하여 네트워크 정책,
노드 상태, 엔드포인트, ID, BGP 설정 등을 Kubernetes API로 관리한다. 단순히 CRD를 정의하는
수준을 넘어, Cilium은 CRD의 등록, 버전 관리, 감시(watch), 동기화, 메모리 최적화까지 포괄하는
완전한 K8s 통합 프레임워크를 구축했다.

이 문서에서는 Cilium이 어떻게 20개 이상의 CRD를 정의하고, 클러스터에 등록하며, 변경을
실시간으로 감시하고, 내부 상태와 동기화하는지를 소스코드 수준에서 분석한다.

### 핵심 구성 요소

```
+------------------------------------------------------------------+
|                     Cilium K8s 통합 아키텍처                       |
+------------------------------------------------------------------+
|                                                                    |
|  +-----------------+    +-----------------+    +-----------------+ |
|  |   CRD 타입 정의  |    |   CRD 등록/관리  |    |  K8s Clientset  | |
|  |  cilium.io/v2   |    |  Operator 담당   |    |  Composite 패턴 | |
|  | cnp_types.go    |    | register.go     |    |  cell.go        | |
|  | types.go        |    | crdhelpers/     |    |                 | |
|  +---------+-------+    +--------+--------+    +--------+--------+ |
|            |                     |                       |         |
|            v                     v                       v         |
|  +----------------------------------------------------------+     |
|  |              Resource[T] 프레임워크                        |     |
|  |  - 타입 안전한 리소스 추상화                                |     |
|  |  - Lazy 시작 (필요할 때만 Informer 가동)                   |     |
|  |  - 이벤트 스트림 (Upsert/Delete/Sync)                     |     |
|  |  - 읽기 전용 Store                                        |     |
|  +---------------------------+------------------------------+     |
|                              |                                     |
|            +-----------------+-----------------+                   |
|            v                                   v                   |
|  +-------------------+              +---------------------+        |
|  | 리소스 워처        |              |  동기화 추적          |        |
|  | K8sWatcher        |              |  Resources          |        |
|  | Pod/CNP/Node/...  |              |  CacheStatus        |        |
|  +-------------------+              |  CRDSync Promise    |        |
|                                     +---------------------+        |
+------------------------------------------------------------------+
```

### 왜 CRD인가?

Cilium이 CRD를 선택한 핵심 이유는 다음과 같다.

| 관점 | CRD 선택 이유 |
|------|--------------|
| **API 일관성** | kubectl, RBAC, audit log 등 K8s 생태계 도구를 그대로 활용 |
| **선언적 관리** | GitOps 워크플로우와 자연스럽게 통합 (ArgoCD, Flux 등) |
| **Watch 메커니즘** | K8s Informer를 통한 효율적인 실시간 변경 감지 |
| **RBAC 통합** | K8s 네이티브 권한 관리로 보안 정책 적용 |
| **스키마 검증** | OpenAPI v3 스키마로 리소스 유효성 자동 검증 |
| **상태 서브리소스** | status 서브리소스로 선언(spec)과 현재 상태(status) 분리 |

---

## 2. CRD 타입 정의

### 2.1 API 그룹과 버전

Cilium의 모든 CRD는 `cilium.io` API 그룹 아래에 정의된다.

**파일**: `pkg/k8s/apis/cilium.io/v2/register.go` (14~19행)

```go
const (
    CustomResourceDefinitionGroup   = k8sconst.CustomResourceDefinitionGroup  // "cilium.io"
    CustomResourceDefinitionVersion = "v2"
)

var SchemeGroupVersion = schema.GroupVersion{
    Group:   CustomResourceDefinitionGroup,
    Version: CustomResourceDefinitionVersion,
}
```

Cilium은 두 가지 API 버전을 사용한다.

| 버전 | 안정성 | 용도 |
|------|--------|------|
| `cilium.io/v2` | Stable | 핵심 CRD (CNP, CEP, CiliumNode 등) |
| `cilium.io/v2alpha1` | Alpha | 실험적 CRD (CES, PodIPPool, L2Announcement 등) |

### 2.2 핵심 CRD 타입 전체 목록

`register.go`의 `addKnownTypes()` 함수(220~260행)가 K8s 스킴에 등록하는 전체 타입이다.

```go
func addKnownTypes(scheme *runtime.Scheme) error {
    scheme.AddKnownTypes(SchemeGroupVersion,
        &CiliumNetworkPolicy{},
        &CiliumNetworkPolicyList{},
        &CiliumClusterwideNetworkPolicy{},
        &CiliumClusterwideNetworkPolicyList{},
        &CiliumCIDRGroup{},
        &CiliumCIDRGroupList{},
        &CiliumEgressGatewayPolicy{},
        &CiliumEgressGatewayPolicyList{},
        &CiliumEndpoint{},
        &CiliumEndpointList{},
        &CiliumNode{},
        &CiliumNodeList{},
        &CiliumNodeConfig{},
        &CiliumNodeConfigList{},
        &CiliumIdentity{},
        &CiliumIdentityList{},
        &CiliumLocalRedirectPolicy{},
        &CiliumLocalRedirectPolicyList{},
        &CiliumEnvoyConfig{},
        &CiliumEnvoyConfigList{},
        &CiliumClusterwideEnvoyConfig{},
        &CiliumClusterwideEnvoyConfigList{},
        &CiliumBGPClusterConfig{},
        &CiliumBGPClusterConfigList{},
        &CiliumBGPPeerConfig{},
        &CiliumBGPPeerConfigList{},
        &CiliumBGPAdvertisement{},
        &CiliumBGPAdvertisementList{},
        &CiliumBGPNodeConfig{},
        &CiliumBGPNodeConfigList{},
        &CiliumBGPNodeConfigOverride{},
        &CiliumBGPNodeConfigOverrideList{},
        &CiliumLoadBalancerIPPool{},
        &CiliumLoadBalancerIPPoolList{},
    )
    metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
    return nil
}
```

### 2.3 CRD Kind 상수 정의

각 CRD는 상수로 Kind, Plural, FullName이 정의된다.

**파일**: `pkg/k8s/apis/cilium.io/v2/register.go` (21~179행)

| CRD Kind | Plural | ShortName | Scope | 약어 |
|----------|--------|-----------|-------|------|
| CiliumNetworkPolicy | ciliumnetworkpolicies | cnp, ciliumnp | Namespaced | CNP |
| CiliumClusterwideNetworkPolicy | ciliumclusterwidenetworkpolicies | ccnp | Cluster | CCNP |
| CiliumCIDRGroup | ciliumcidrgroups | ccg | Cluster | CCG |
| CiliumEgressGatewayPolicy | ciliumegressgatewaypolicies | cegp | Cluster | CEGP |
| CiliumEndpoint | ciliumendpoints | cep, ciliumep | Namespaced | CEP |
| CiliumNode | ciliumnodes | cn | Cluster | CN |
| CiliumIdentity | ciliumidentities | cid | Cluster | CID |
| CiliumLocalRedirectPolicy | ciliumlocalredirectpolicies | clrp | Namespaced | CLRP |
| CiliumEnvoyConfig | ciliumenvoyconfigs | cec | Namespaced | CEC |
| CiliumClusterwideEnvoyConfig | ciliumclusterwideenvoyconfigs | ccec | Cluster | CCEC |
| CiliumNodeConfig | ciliumnodeconfigs | cnc | Namespaced | CNC |
| CiliumBGPClusterConfig | ciliumbgpclusterconfigs | bgpcc | Cluster | BGPCC |
| CiliumBGPPeerConfig | ciliumbgppeerconfigs | bgppc | Cluster | BGPPC |
| CiliumBGPAdvertisement | ciliumbgpadvertisements | bgpa | Cluster | BGPA |
| CiliumBGPNodeConfig | ciliumbgpnodeconfigs | bgpnc | Cluster | BGPNC |
| CiliumBGPNodeConfigOverride | ciliumbgpnodeconfigoverrides | bgpnco | Cluster | BGPNCO |
| CiliumLoadBalancerIPPool | ciliumloadbalancerippools | pool | Cluster | Pool |

### 2.4 CiliumNetworkPolicy (CNP) 구조체

Cilium의 가장 핵심적인 CRD인 CNP의 구조를 살펴보자.

**파일**: `pkg/k8s/apis/cilium.io/v2/cnp_types.go` (21~54행)

```go
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +deepequal-gen:private-method=true
// +kubebuilder:resource:categories={cilium,ciliumpolicy},
//   singular="ciliumnetworkpolicy",path="ciliumnetworkpolicies",
//   scope="Namespaced",shortName={cnp,ciliumnp}
// +kubebuilder:printcolumn:JSONPath=".metadata.creationTimestamp",name="Age",type=date
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type=='Valid')].status",
//   name="Valid",type=string
// +kubebuilder:subresource:status
// +kubebuilder:storageversion

type CiliumNetworkPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec   *api.Rule  `json:"spec,omitempty"`
    Specs  api.Rules  `json:"specs,omitempty"`
    Status CiliumNetworkPolicyStatus `json:"status,omitempty"`
}
```

**kubebuilder 마커의 의미**:

| 마커 | 역할 |
|------|------|
| `+genclient` | client-gen이 이 타입에 대한 clientset 코드를 생성 |
| `+k8s:deepcopy-gen` | runtime.Object 인터페이스를 위한 DeepCopy 코드 자동 생성 |
| `+deepequal-gen:private-method=true` | Cilium 전용 DeepEqual 메서드 생성 (private) |
| `+kubebuilder:resource` | CRD 스키마 메타데이터 (scope, shortName 등) |
| `+kubebuilder:printcolumn` | `kubectl get cnp`에서 보이는 컬럼 정의 |
| `+kubebuilder:subresource:status` | /status 서브리소스 활성화 |
| `+kubebuilder:storageversion` | 이 버전이 etcd 저장 버전임을 표시 |

CNP는 `Spec` (단일 규칙) 또는 `Specs` (규칙 리스트) 중 하나로 정책을 정의할 수 있다.
이는 하나의 CNP 리소스에 여러 규칙을 묶을 수 있게 하여, 정책 관리의 유연성을 높인다.

### 2.5 CiliumEndpoint (CEP) 구조체

**파일**: `pkg/k8s/apis/cilium.io/v2/types.go` (23~45행)

```go
// +kubebuilder:resource:categories={cilium},singular="ciliumendpoint",
//   path="ciliumendpoints",scope="Namespaced",shortName={cep,ciliumep}
// +kubebuilder:printcolumn:JSONPath=".status.identity.id",
//   description="Security Identity",name="Security Identity",type=integer
// +kubebuilder:printcolumn:JSONPath=".status.state",
//   description="Endpoint current state",name="Endpoint State",type=string
// +kubebuilder:printcolumn:JSONPath=".status.networking.addressing[0].ipv4",
//   description="Endpoint IPv4 address",name="IPv4",type=string

type CiliumEndpoint struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Status EndpointStatus `json:"status,omitempty"`
}
```

CEP는 `Spec`이 없고 `Status`만 가진다. 이는 CEP가 사용자가 선언적으로 생성하는 리소스가
아니라, cilium-agent가 엔드포인트 상태를 **보고**하기 위한 리소스이기 때문이다.

### 2.6 DeepEqual — 왜 별도 비교 함수가 필요한가?

CNP의 `DeepEqual` 함수(57~74행)는 표준 `reflect.DeepEqual` 대신 커스텀 비교를 수행한다.

```go
func (in *CiliumNetworkPolicy) DeepEqual(other *CiliumNetworkPolicy) bool {
    return objectMetaDeepEqual(in.ObjectMeta, other.ObjectMeta) && in.deepEqual(other)
}

func objectMetaDeepEqual(in, other metav1.ObjectMeta) bool {
    if !(in.Name == other.Name && in.Namespace == other.Namespace) {
        return false
    }
    return comparator.MapStringEqualsIgnoreKeys(
        in.GetAnnotations(), other.GetAnnotations(),
        []string{v1.LastAppliedConfigAnnotation})  // kubectl apply의 흔적 무시
}
```

**왜 커스텀 DeepEqual이 필요한가?**

`kubectl apply`를 사용하면 `last-applied-configuration` 어노테이션이 자동 추가된다.
이 어노테이션은 정책의 실질적 내용과 무관하므로, 비교 시 무시해야 불필요한 정책
재계산(recalculation)을 방지할 수 있다.

---

## 3. CRD 등록과 라이프사이클

### 3.1 CRD 등록 흐름 전체 구조

```
+------------------+
|  Cilium Operator  |
|  (시작 시)         |
+--------+---------+
         |
         v
+--------+---------+    1. CRD YAML 로드
| RegisterCRDsCell |-------> //go:embed crds/v2/*.yaml
| (Hive Cell)      |         (바이너리에 내장)
+--------+---------+
         |
         v
+--------+---------+    2. K8s API Server에 CRD 생성/업데이트
| CreateCustom     |-------> CreateUpdateCRD()
| ResourceDefs()   |         - Get → 존재 확인
+--------+---------+         - 없으면 Create
         |                   - 버전 차이 있으면 Update
         v
+--------+---------+    3. CRD 상태 확인
| waitForV1CRD()   |-------> Established == True 대기
|                  |         (최대 60초 폴링)
+------------------+
         |
         v
+--------+---------+    4. Agent에 CRD 준비 완료 알림
| CRDSync Promise  |-------> promise.Resolve(CRDSync{})
| (Resolve)        |         Agent의 Resource[T]가 Informer 시작
+------------------+
```

### 3.2 CRD YAML 임베딩

**파일**: `pkg/k8s/apis/cilium.io/client/register.go` (208~271행)

```go
var (
    //go:embed crds/v2/ciliumnetworkpolicies.yaml
    crdsCiliumnetworkpolicies []byte

    //go:embed crds/v2/ciliumclusterwidenetworkpolicies.yaml
    crdsCiliumclusterwidenetworkpolicies []byte

    //go:embed crds/v2/ciliumendpoints.yaml
    crdsCiliumendpoints []byte

    //go:embed crds/v2/ciliumidentities.yaml
    crdsCiliumidentities []byte

    //go:embed crds/v2/ciliumnodes.yaml
    crdsCiliumnodes []byte

    // ... 20+ CRD YAML 파일
)
```

**왜 go:embed인가?**

| 대안 | 문제 |
|------|------|
| 파일시스템에서 로드 | 배포 시 YAML 파일 경로 관리 필요, 컨테이너 이미지 복잡도 증가 |
| 코드로 직접 생성 | OpenAPI 스키마가 복잡하여 유지보수 불가능 |
| Helm chart로 분리 | Operator가 직접 등록해야 버전 동기화 보장 |
| **go:embed** | **바이너리에 내장 → 배포 단순화, 버전 일치 보장** |

### 3.3 CreateUpdateCRD — 멱등적 CRD 등록

**파일**: `pkg/k8s/apis/crdhelpers/register.go` (30~71행)

```go
func CreateUpdateCRD(
    logger *slog.Logger,
    clientset apiextensionsclient.Interface,
    targetCRD *apiextensionsv1.CustomResourceDefinition,
    poller poller,
    needsUpdateCRDFunc NeedUpdateCRDFunc,
) error {
    v1CRDClient := clientset.ApiextensionsV1()

    // 1단계: 현재 CRD 상태 확인
    currentCRD, err := v1CRDClient.CustomResourceDefinitions().Get(
        context.TODO(), targetCRD.ObjectMeta.Name, metav1.GetOptions{})

    if errors.IsNotFound(err) {
        // 2단계: 없으면 생성
        currentCRD, err = v1CRDClient.CustomResourceDefinitions().Create(
            context.TODO(), targetCRD, metav1.CreateOptions{})
        // 여러 에이전트가 동시에 생성 시도 → AlreadyExists는 정상
        if errors.IsAlreadyExists(err) {
            return nil
        }
    }

    // 3단계: 스키마 버전이 다르면 업데이트
    if err := updateV1CRD(...); err != nil { return err }

    // 4단계: CRD가 Established 상태가 될 때까지 대기
    if err := waitForV1CRD(...); err != nil { return err }

    return nil
}
```

이 함수의 핵심은 **멱등성(idempotency)**이다. 여러 Operator 인스턴스가 동시에 실행되어도
안전하게 동작한다. `AlreadyExists` 에러를 정상으로 처리하고, Update 시 `Conflict` 에러가
발생하면 재시도한다.

### 3.4 NeedsUpdateV1Factory — 스키마 버전 비교

**파일**: `pkg/k8s/apis/crdhelpers/register.go` (73~96행)

```go
func NeedsUpdateV1Factory(
    crdSchemaVersionLabelKey string,
    minCRDSchemaVersion semver.Version,
) NeedUpdateCRDFunc {
    return func(_, currentCRD *apiextensionsv1.CustomResourceDefinition) (bool, error) {
        if currentCRD.Spec.Versions[0].Schema == nil {
            return true, nil  // 스키마 없음 → 업데이트 필요
        }
        v, ok := currentCRD.Labels[crdSchemaVersionLabelKey]
        if !ok {
            return true, nil  // 버전 라벨 없음 → 업데이트 필요
        }
        currentVersion, err := versioncheck.Version(v)
        if err || currentVersion.LT(minCRDSchemaVersion) {
            return true, nil  // 버전이 낮음 → 업데이트 필요
        }
        return false, nil  // 최신 → 업데이트 불필요
    }
}
```

CRD 라벨에 스키마 버전을 기록하고, Operator 시작 시 이 버전을 비교하여 업데이트 여부를
결정한다. 이로써 Cilium 업그레이드 시 CRD 스키마가 자동으로 마이그레이션된다.

### 3.5 RegisterCRDsCell — Hive 통합

**파일**: `pkg/k8s/apis/cell.go`

```go
var RegisterCRDsCell = cell.Module(
    "create-crds",
    "Create Cilium CRDs",

    cell.Config(defaultConfig),
    cell.Invoke(createCRDs),
    cell.ProvidePrivate(newCiliumGroupCRDs),
)

type RegisterCRDsConfig struct {
    SkipCRDCreation bool  // --skip-crd-creation 플래그
}
```

`createCRDs` 함수는 Lifecycle의 `OnStart` 훅에서 실행되며, `--skip-crd-creation` 플래그로
CRD 생성을 건너뛸 수 있다. 이는 CRD가 이미 Helm chart 등으로 미리 설치된 환경에서 유용하다.

```go
func createCRDs(p params) {
    p.Lifecycle.Append(cell.Hook{
        OnStart: func(ctx cell.HookContext) error {
            if !p.Clientset.IsEnabled() || p.Config.SkipCRDCreation {
                p.Logger.Info("Skipping creation of CRDs")
                return nil
            }
            for _, f := range p.RegisterCRDsFuncs {
                if err := f(p.Logger, p.Clientset); err != nil {
                    return fmt.Errorf("unable to create CRDs: %w", err)
                }
            }
            return nil
        },
    })
}
```

---

## 4. K8s Resource 프레임워크

### 4.1 Resource[T] 인터페이스

Cilium의 `Resource[T]`는 K8s 리소스에 대한 타입 안전한 추상화 계층이다.

**파일**: `pkg/k8s/resource/resource.go` (53~97행)

```go
type Resource[T k8sRuntime.Object] interface {
    // Observable — stream 패키지를 통한 이벤트 관찰
    stream.Observable[Event[T]]

    // Events — 이벤트 채널 반환 (Upsert/Delete/Sync)
    Events(ctx context.Context, opts ...EventsOpt) <-chan Event[T]

    // Store — 읽기 전용 저장소 (동기화 완료 후 접근 가능)
    Store(context.Context) (Store[T], error)
}
```

### 4.2 이벤트 흐름 모델

```
 Informer 시작
     |
     v
 +---------+   +---------+   +---------+
 | Upsert  |-->| Upsert  |-->| Upsert  |  (기존 객체 리플레이)
 +---------+   +---------+   +---------+
     |
     v
 +---------+   +---------+
 | Upsert  |-->| Delete  |  (실시간 증분 업데이트)
 +---------+   +---------+
     |
     v
 +---------+
 |  Sync   |  (API 서버와 동기화 완료 신호)
 +---------+
     |
     v
 +---------+   +---------+   +---------+
 | Upsert  |-->| Delete  |-->| Upsert  |  (이후 변경 사항)
 +---------+   +---------+   +---------+
```

이벤트 처리의 핵심 규칙:
1. 모든 이벤트에 대해 `Done(error)`를 반드시 호출해야 한다
2. `Done(nil)` → 처리 완료, `Done(err)` → 에러 핸들러 호출 (기본: 재큐잉)
3. `Done()`을 호출하지 않으면 해당 키의 새 이벤트가 차단되고, 결국 **panic** 발생

### 4.3 New[T] 생성자

**파일**: `pkg/k8s/resource/resource.go` (139~158행)

```go
func New[T k8sRuntime.Object](
    lc cell.Lifecycle,
    lw cache.ListerWatcher,
    mp workqueue.MetricsProvider,
    opts ...ResourceOption,
) Resource[T] {
    r := &resource[T]{
        subscribers:     make(map[uint64]*subscriber[T]),
        needed:          make(chan struct{}, 1),
        lw:              lw,
        metricsProvider: mp,
    }
    r.opts.sourceObj = func() k8sRuntime.Object {
        var obj T
        return obj
    }
    for _, o := range opts {
        o(&r.opts)
    }
    r.ctx, r.cancel = context.WithCancel(context.Background())
    r.storeResolver, r.storePromise = promise.New[Store[T]]()
    lc.Append(r)  // Lifecycle에 Start/Stop 훅 등록
    return r
}
```

### 4.4 Lazy 시작 메커니즘

**파일**: `pkg/k8s/resource/resource.go` (315~346행)

```go
func (r *resource[T]) startWhenNeeded() {
    // 1. Events() 또는 Store()가 호출될 때까지 대기
    select {
    case <-r.ctx.Done():
        r.wg.Done()
        return
    case <-r.needed:
    }

    // 2. CRD 동기화 완료 대기 (Cilium CRD인 경우)
    if r.opts.crdSyncPromise != nil {
        r.opts.crdSyncPromise.Await(r.ctx)
    }

    // 3. Informer 생성 및 실행
    store, informer := r.newInformer()
    r.storeResolver.Resolve(&typedStore[T]{store})

    go func() {
        defer r.wg.Done()
        informer.Run(r.ctx.Done())
    }()

    // 4. 캐시 동기화 완료 후 Sync 이벤트 발행
    if cache.WaitForCacheSync(r.ctx.Done(), informer.HasSynced) {
        r.mu.Lock()
        for _, sub := range r.subscribers {
            sub.enqueueSync()
        }
        // ...
    }
}
```

**왜 Lazy 시작인가?**

Cilium에는 수십 개의 리소스 타입이 등록되지만, 설정에 따라 일부만 실제로 사용된다.
예를 들어 BGP를 사용하지 않는 클러스터에서는 BGP 관련 CRD Informer를 시작할 필요가 없다.
Lazy 시작은 **불필요한 API 서버 Watch 연결을 방지**하여 클러스터 부하를 줄인다.

### 4.5 ResourceOption 패턴

| 옵션 | 역할 | 사용 예 |
|------|------|---------|
| `WithTransform[From,To]` | 객체 변환 후 저장 (메모리 절약) | Pod → SlimPod |
| `WithLazyTransform` | 지연된 객체 변환 (API 서버 능력에 따라) | CRD 버전별 변환 |
| `WithMetric(scope)` | Prometheus 메트릭 수집 활성화 | "CiliumNetworkPolicy" |
| `WithIndexers(indexers)` | 커스텀 인덱서 추가 | 네임스페이스별 인덱싱 |
| `WithCRDSync(promise)` | CRD 등록 완료까지 시작 지연 | Cilium CRD 리소스 |

### 4.6 리소스 생성자 함수들

**파일**: `pkg/k8s/resource_ctors.go`

리소스 생성자는 특정 K8s 리소스에 대한 `Resource[T]`를 구성한다.

```go
// K8s 네이티브 리소스 — Slim 클라이언트 사용
func ServiceResource(lc cell.Lifecycle, cfg ConfigParams,
    cs client.Clientset, mp workqueue.MetricsProvider,
    opts ...func(*metav1.ListOptions),
) (resource.Resource[*slim_corev1.Service], error) {
    lw := utils.ListerWatcherFromTyped[*slim_corev1.ServiceList](
        cs.Slim().CoreV1().Services(""),
    )
    return resource.New[*slim_corev1.Service](lc, lw, mp,
        resource.WithMetric("Service"),
        resource.WithIndexers(indexers),
    ), nil
}

// Cilium CRD 리소스 — CRDSync 의존성 포함
func CiliumNetworkPolicyResource(params CiliumResourceParams,
    opts ...func(*metav1.ListOptions),
) (resource.Resource[*cilium_api_v2.CiliumNetworkPolicy], error) {
    lw := utils.ListerWatcherFromTyped[*cilium_api_v2.CiliumNetworkPolicyList](
        params.ClientSet.CiliumV2().CiliumNetworkPolicies(""),
    )
    return resource.New[*cilium_api_v2.CiliumNetworkPolicy](
        params.Lifecycle, lw, params.MetricsProvider,
        resource.WithMetric("CiliumNetworkPolicy"),
        resource.WithCRDSync(params.CRDSyncPromise),  // CRD 준비 대기
    ), nil
}
```

핵심 차이: Cilium CRD 리소스는 `CiliumResourceParams`를 받아 `CRDSyncPromise`를
자동으로 연결한다.

```go
type CiliumResourceParams struct {
    cell.In
    Logger          *slog.Logger
    Lifecycle       cell.Lifecycle
    ClientSet       client.Clientset
    CRDSyncPromise  promise.Promise[synced.CRDSync] `optional:"true"`
    MetricsProvider workqueue.MetricsProvider
}
```

---

## 5. K8s 클라이언트 아키텍처

### 5.1 Composite Clientset 패턴

Cilium은 6개의 서로 다른 K8s clientset을 하나의 `Clientset` 인터페이스로 합성한다.

**파일**: `pkg/k8s/client/cell.go` (78~101행)

```go
type Clientset interface {
    mcsapi_clientset.Interface     // Multi-Cluster Service API
    kubernetes.Interface           // 표준 Kubernetes (Pod, Service 등)
    apiext_clientset.Interface     // API Extensions (CRD 자체 관리)
    cilium_clientset.Interface     // Cilium CRD 클라이언트
    policy_clientset.Interface     // Network Policy API
    Getters                        // 편의 getter 메서드들

    Slim() slim_clientset.Interface  // Slim 버전 (메모리 최적화)
    IsEnabled() bool
    Config() Config
    RestConfig() *rest.Config
}
```

### 5.2 Clientset 구성 구조

```
+---------------------------------------------------------------+
|                    Clientset (인터페이스)                        |
+---------------------------------------------------------------+
|                                                                 |
|  +-------------------+  +--------------------+                  |
|  | KubernetesClient  |  |  APIExtClient      |                 |
|  | (protobuf)        |  |  (protobuf)        |                 |
|  | Pod, Service,     |  |  CRD 생성/관리      |                 |
|  | Node, Namespace   |  |                    |                  |
|  +-------------------+  +--------------------+                  |
|                                                                 |
|  +-------------------+  +--------------------+                  |
|  | CiliumClient      |  |  PolicyClient      |                 |
|  | (JSON)            |  |  (protobuf)        |                 |
|  | CNP, CEP, CN,     |  |  NetworkPolicy     |                 |
|  | CID, CEC, BGP...  |  |  ClusterNP         |                 |
|  +-------------------+  +--------------------+                  |
|                                                                 |
|  +-------------------+  +--------------------+                  |
|  | SlimClient        |  |  MCSAPIClient      |                 |
|  | (protobuf)        |  |  (protobuf)        |                 |
|  | 메모리 최적화 버전  |  |  Multi-Cluster SVC |                 |
|  +-------------------+  +--------------------+                  |
+---------------------------------------------------------------+
```

### 5.3 Clientset 초기화

**파일**: `pkg/k8s/client/cell.go` (136~218행)

```go
func newClientset(params compositeClientsetParams) (Clientset, *restConfigManager, error) {
    // ...

    // Slim과 K8s 클라이언트는 protobuf 직렬화 사용
    rc.ContentConfig.ContentType = `application/vnd.kubernetes.protobuf`

    client.slim, _ = slim_clientset.NewForConfigAndClient(rc, httpClient)
    client.APIExtClientset, _ = apiext_clientset.NewForConfigAndClient(rc, httpClient)
    client.MCSAPIClientset, _ = mcsapi_clientset.NewForConfigAndClient(rc, httpClient)
    client.KubernetesClientset, _ = kubernetes.NewForConfigAndClient(rc, httpClient)
    client.PolicyClientset, _ = policy_clientset.NewForConfigAndClient(rc, httpClient)

    // Cilium 클라이언트는 JSON 직렬화 사용
    rc.ContentConfig.ContentType = `application/json`
    client.CiliumClientset, _ = cilium_clientset.NewForConfigAndClient(rc, httpClient)

    return &client, client.restConfigManager, nil
}
```

**왜 Cilium 클라이언트만 JSON인가?**

Protobuf 직렬화는 K8s 네이티브 리소스에는 효율적이지만, Cilium CRD에는 protobuf
스키마가 정의되어 있지 않다. CRD 리소스는 API 서버에서 항상 JSON으로 저장/반환되므로,
Cilium 클라이언트는 JSON을 사용한다. 반면 K8s 네이티브 리소스는 protobuf를 사용하여
직렬화/역직렬화 오버헤드를 줄인다.

### 5.4 Hive Cell 구성

```go
var Cell = cell.Module(
    "k8s-client",
    "Kubernetes Client",

    cell.Config(defaultSharedConfig),
    cell.Config(defaultClientParams),
    cell.Provide(NewClientConfig),
    cell.Provide(newClientset),       // Clientset 싱글톤 제공
    cell.Invoke(registerMappingsUpdater),
)
```

---

## 6. Slim Clientset

### 6.1 개요

Slim Clientset은 Cilium이 **메모리 사용량을 대폭 절감**하기 위해 만든 K8s 클라이언트 변형이다.
수천 개의 Pod, Service, Node를 캐싱하는 대규모 클러스터에서 표준 K8s 객체는 상당한
메모리를 소비한다. Slim 버전은 불필요한 필드를 제거하여 이 문제를 해결한다.

### 6.2 디렉토리 구조

```
pkg/k8s/slim/k8s/
├── api/
│   ├── core/v1/              # slim Pod, Service, Node, Namespace, Secret, ConfigMap
│   │   ├── types.go          # 축소된 타입 정의
│   │   ├── generated.pb.go   # protobuf 직렬화 코드
│   │   └── generated.proto   # protobuf 스키마
│   ├── discovery/v1/         # slim EndpointSlice
│   └── networking/v1/        # slim NetworkPolicy, Ingress
├── apis/meta/v1/             # slim ObjectMeta
└── client/clientset/versioned/ # 생성된 clientset
```

### 6.3 Slim vs Standard 비교

```
표준 Pod 객체                           Slim Pod 객체
+----------------------------------+   +---------------------------+
| metadata                         |   | metadata (slim)           |
|   name, namespace, labels        |   |   name, namespace, labels |
|   annotations, ownerReferences   |   |   annotations             |
|   managedFields (대량!)           |   |   (managedFields 제거)    |
|   creationTimestamp              |   +---------------------------+
+----------------------------------+   | spec (slim)               |
| spec                             |   |   nodeName                |
|   containers[]                   |   |   hostNetwork             |
|     name, image, command         |   |   initContainers (이름만) |
|     env, volumeMounts           |   |   containers (이름만)     |
|     resources, probes           |   |   serviceAccountName      |
|     securityContext             |   +---------------------------+
|   volumes[]                     |   | status (slim)             |
|   affinity, tolerations         |   |   phase, conditions       |
|   dnsPolicy, ...               |   |   podIPs, hostIP          |
+----------------------------------+   +---------------------------+
| status                           |
|   phase, conditions              |
|   podIPs, hostIP                 |
|   containerStatuses[]            |
|   initContainerStatuses[]        |
+----------------------------------+

메모리: ~2-5KB/pod                      메모리: ~0.3-0.8KB/pod
```

### 6.4 왜 Slim인가?

| 시나리오 | 표준 객체 | Slim 객체 | 절감율 |
|----------|----------|-----------|--------|
| 10,000 Pods | ~30MB | ~5MB | **83%** |
| 5,000 Services | ~15MB | ~3MB | **80%** |
| managedFields 포함 시 | ~50MB | ~5MB | **90%** |

대규모 클러스터에서 `managedFields`만 해도 Pod당 수 KB를 차지할 수 있다.
Slim 버전은 Cilium이 실제로 필요로 하는 필드만 남겨 메모리를 절약한다.

### 6.5 Protobuf 직렬화

Slim 타입은 protobuf 스키마를 포함하여 네트워크 전송 시에도 최적화된다.

```
API 서버 → cilium-agent 전송량 비교

표준 JSON:    {"metadata":{"name":"nginx","namespace":"default","labels":{...},...}}
Protobuf:     \x0a\x05nginx\x12\x07default\x1a\x0b...  (바이너리)

전송량 절감: 약 40-60%
```

---

## 7. 리소스 워처

### 7.1 K8sWatcher 구조

**파일**: `pkg/k8s/watchers/watcher.go` (95~121행)

```go
type K8sWatcher struct {
    logger           *slog.Logger
    resourceGroupsFn func(logger *slog.Logger, cfg WatcherConfiguration) (
        resourceGroups, waitForCachesOnly []string)

    clientset client.Clientset

    k8sEventReporter          *K8sEventReporter
    k8sPodWatcher             *K8sPodWatcher
    k8sCiliumNodeWatcher      *K8sCiliumNodeWatcher
    k8sCiliumEndpointsWatcher *K8sCiliumEndpointsWatcher

    // 리소스 동기화 상태 추적
    k8sResourceSynced *synced.Resources
    k8sCacheStatus    synced.CacheStatus
    k8sAPIGroups      *synced.APIGroups

    cfg  WatcherConfiguration
    kcfg interface{ IsEnabled() bool }  // KVStore 활성화 여부
}
```

### 7.2 워처 아키텍처

```
+------------------------------------------------------------------+
|                       K8sWatcher                                  |
+------------------------------------------------------------------+
|                                                                    |
|  +-------------------+  +---------------------+                   |
|  | K8sPodWatcher     |  | K8sCiliumNodeWatcher |                  |
|  | - Pod 변경 감시    |  | - CiliumNode 감시    |                  |
|  | - 엔드포인트 업데이트|  | - 노드 라우팅 업데이트 |                  |
|  +-------------------+  +---------------------+                   |
|                                                                    |
|  +----------------------------+  +-------------------+            |
|  | K8sCiliumEndpointsWatcher  |  | K8sEventReporter  |            |
|  | - CEP/CES 감시             |  | - K8s 이벤트 기록  |            |
|  | - 엔드포인트 상태 동기화     |  | - 감사 로그        |            |
|  +----------------------------+  +-------------------+            |
|                                                                    |
|  +----------------------------------------------------------+    |
|  |                k8sResourceSynced (Resources)               |    |
|  |  - 리소스별 동기화 채널 관리                                 |    |
|  |  - BlockWaitGroupToSyncResources()                         |    |
|  +----------------------------------------------------------+    |
+------------------------------------------------------------------+
```

### 7.3 API 그룹 상수

**파일**: `pkg/k8s/watchers/watcher.go` (34~42행)

```go
const (
    k8sAPIGroupCiliumNetworkPolicyV2            = "cilium/v2::CiliumNetworkPolicy"
    k8sAPIGroupCiliumClusterwideNetworkPolicyV2 = "cilium/v2::CiliumClusterwideNetworkPolicy"
    k8sAPIGroupCiliumCIDRGroupV2                = "cilium/v2::CiliumCIDRGroup"
    k8sAPIGroupCiliumNodeV2                     = "cilium/v2::CiliumNode"
    k8sAPIGroupCiliumEndpointV2                 = "cilium/v2::CiliumEndpoint"
    k8sAPIGroupCiliumLocalRedirectPolicyV2      = "cilium/v2::CiliumLocalRedirectPolicy"
    k8sAPIGroupCiliumEndpointSliceV2Alpha1      = "cilium/v2alpha1::CiliumEndpointSlice"
)
```

이 상수들은 `cilium status`에서 각 API 그룹의 Watch 상태를 표시하는 데 사용된다.

### 7.4 워처가 Resource[T]와 함께 동작하는 방식

```
Resource[T].Events()
     |
     v
+----+----+
| 이벤트   |
| 채널     |
+----+----+
     |
     v
+----+----+   Upsert 이벤트
| 워처     |---> 정책 파서 호출 → 엔드포인트 정책 업데이트
| 루프     |
|          |   Delete 이벤트
|          |---> 정책 제거 → 엔드포인트 정책 재계산
|          |
|          |   Sync 이벤트
|          |---> 초기 동기화 완료 → 대기 중인 작업 시작
+----+----+
     |
     v
event.Done(nil)  ← 반드시 호출
```

---

## 8. 동기화 추적

### 8.1 Resources 구조체

**파일**: `pkg/k8s/synced/resources.go` (22~36행)

```go
type Resources struct {
    logger      *slog.Logger
    CacheStatus CacheStatus

    lock.RWMutex
    // 리소스명 → 동기화 완료 시 닫히는 채널
    resources map[string]<-chan struct{}
    // 캐시 동기화 결과 (true=성공, false=취소)
    stopWait map[string]bool
    // 각 리소스의 마지막 이벤트 수신 시각
    timeSinceLastEvent map[string]time.Time
}
```

### 8.2 BlockWaitGroupToSyncResources

**파일**: `pkg/k8s/synced/resources.go` (69~131행)

이 함수는 K8s 캐시 동기화를 추적하는 핵심 메커니즘이다.

```go
func (r *Resources) BlockWaitGroupToSyncResources(
    stop <-chan struct{},
    swg *lock.StoppableWaitGroup,
    hasSyncedFunc cache.InformerSynced,
    resourceName string,
) {
    // 이미 캐시가 동기화된 후에 호출되면 에러 로그
    if r.CacheStatus.Synchronized() {
        r.logger.Error("BlockWaitGroupToSyncResources called after sync")
        return
    }

    ch := make(chan struct{})
    r.resources[resourceName] = ch

    go func() {
        // cache.WaitForCacheSync — K8s 공식 동기화 대기
        if ok := cache.WaitForCacheSync(stop, hasSyncedFunc); !ok {
            select {
            case <-stop:
                // 취소됨 — 치명적 에러 아님
                r.stopWait[resourceName] = false
            default:
                // 동기화 실패 — Fatal 종료
                logging.Fatal(scopedLog, "failed to wait for cache to sync")
            }
        } else {
            r.stopWait[resourceName] = true
        }
        close(ch)  // 동기화 완료 신호
    }()
}
```

**왜 Fatal인가?**

K8s 캐시 동기화 실패는 Cilium이 오래된/부분적인 데이터로 동작하게 만든다.
네트워크 정책에서 이는 **보안 위반**을 의미할 수 있으므로, 안전하게 실패(fail-safe)하기
위해 프로세스를 종료한다.

### 8.3 CacheStatus

**파일**: `pkg/k8s/synced/cache_status.go`

```go
type CacheStatus chan struct{}

func (cs CacheStatus) Synchronized() bool {
    if cs == nil {
        return true  // 초기화되지 않은 CacheStatus는 동기화된 것으로 간주
    }
    select {
    case <-cs:
        return true   // 채널이 닫혔으면 동기화 완료
    default:
        return false  // 아직 열려 있으면 동기화 중
    }
}
```

`CacheStatus`는 `chan struct{}`를 사용하는 단순하지만 효과적인 패턴이다.
채널을 닫으면 모든 대기자가 동시에 깨어나므로, 브로드캐스트 역할을 한다.

### 8.4 CRDSync Promise

**파일**: `pkg/k8s/synced/cell.go` (48~115행)

```
CRDSync 흐름도:

Operator 시작 → CRD 등록 ──────────────────────────────+
                                                         |
Agent 시작 → CRDSyncCell 생성                            |
              |                                          |
              v                                          v
         SyncCRDs() ← Informer로 CRD 존재 감시 ← CRD가 apiserver에 등록됨
              |
              v
         모든 CRD 발견?
         ├── Yes → crdSyncResolver.Resolve(CRDSync{})
         │           → Resource[T].startWhenNeeded()의
         │             crdSyncPromise.Await() 해제
         │           → Informer 시작
         │
         └── No (타임아웃) → crdSyncResolver.Reject(err)
                              → Fatal 종료
```

```go
type CRDSync struct{}

func newCRDSyncPromise(params syncCRDsPromiseParams) promise.Promise[CRDSync] {
    crdSyncResolver, crdSyncPromise := promise.New[CRDSync]()

    if !params.Clientset.IsEnabled() || option.Config.DryMode {
        crdSyncResolver.Reject(ErrCRDSyncDisabled)
        return crdSyncPromise
    }

    params.JobGroup.Add(job.OneShot("sync-crds", func(ctx context.Context, health cell.Health) error {
        err := SyncCRDs(ctx, params.Logger, params.Clientset,
            params.ResourceNames, params.Resources, params.APIGroups, params.Config)
        if err != nil {
            crdSyncResolver.Reject(err)
        } else {
            crdSyncResolver.Resolve(struct{}{})
        }
        return err
    }))

    return crdSyncPromise
}
```

### 8.5 SyncCRDs — CRD 존재 확인

**파일**: `pkg/k8s/synced/crd.go` (134~220행)

```go
func SyncCRDs(ctx context.Context, logger *slog.Logger,
    clientset client.Clientset, crdNames []string,
    rs *Resources, ag *APIGroups, cfg CRDSyncConfig) error {

    crds := newCRDState(logger, crdNames)

    // CRD 리소스 자체를 Watch하는 Informer
    _, crdController := informer.NewInformer(
        listerWatcher,
        &slim_metav1.PartialObjectMetadata{},
        0,
        cache.ResourceEventHandlerFuncs{
            AddFunc:    func(obj any) { crds.add(obj) },
            DeleteFunc: func(obj any) { crds.remove(obj) },
        },
        nil,
    )

    ctx, cancel := context.WithTimeout(ctx, cfg.CRDWaitTimeout)  // 기본 5분
    defer cancel()

    // 각 CRD에 대해 동기화 대기 등록
    for crd := range crds.m {
        rs.BlockWaitGroupToSyncResources(ctx.Done(), nil,
            func() bool { return crds.m[crd] },
            crd,
        )
    }

    go crdController.Run(ctx.Done())

    // 모든 CRD가 발견될 때까지 또는 타임아웃까지 폴링
    ticker := time.NewTicker(50 * time.Millisecond)
    for {
        select {
        case <-ctx.Done():
            // 타임아웃 → Fatal (Operator가 CRD를 등록하지 않았음)
            logging.Fatal(logger, fmt.Sprintf(
                "Unable to find all Cilium CRDs within %v timeout. "+
                "Missing: %v", cfg.CRDWaitTimeout, crds.unSynced()))
        case <-ticker.C:
            if crds.allSynced() {
                return nil  // 모든 CRD 확인 완료
            }
        }
    }
}
```

### 8.6 Agent가 대기하는 CRD 목록

**파일**: `pkg/k8s/synced/crd.go` (43~96행)

```go
func agentCRDResourceNames() []string {
    result := []string{
        CRDResourceName(v2.CIDName),       // CiliumIdentity (항상 필요)
        CRDResourceName(v2alpha1.CPIPName), // CiliumPodIPPool (항상 필요)
    }

    if !option.Config.DisableCiliumEndpointCRD {
        result = append(result, CRDResourceName(v2.CEPName))
        if option.Config.EnableCiliumEndpointSlice {
            result = append(result, CRDResourceName(v2alpha1.CESName))
        }
    }
    if option.Config.EnableCiliumNodeCRD {
        result = append(result, CRDResourceName(v2.CNName))
    }
    if option.Config.EnableCiliumNetworkPolicy {
        result = append(result, CRDResourceName(v2.CNPName))
    }
    if option.Config.EnableCiliumClusterwideNetworkPolicy {
        result = append(result, CRDResourceName(v2.CCNPName))
    }
    if option.Config.EnableEgressGateway {
        result = append(result, CRDResourceName(v2.CEGPName))
    }
    if option.Config.EnableLocalRedirectPolicy {
        result = append(result, CRDResourceName(v2.CLRPName))
    }
    if option.Config.EnableEnvoyConfig {
        result = append(result, CRDResourceName(v2.CCECName))
        result = append(result, CRDResourceName(v2.CECName))
    }
    if option.Config.EnableBGPControlPlane {
        result = append(result, CRDResourceName(v2.BGPCCName))
        // + BGPA, BGPPC, BGPNC, BGPNCO
    }
    result = append(result,
        CRDResourceName(v2.LBIPPoolName),
        CRDResourceName(v2alpha1.L2AnnouncementName),
    )
    return result
}
```

**왜 조건부 대기인가?**

모든 CRD를 항상 대기하면 불필요한 타임아웃 위험이 있다.
예를 들어 BGP가 비활성화된 클러스터에서 BGP CRD를 기다리면 타임아웃으로 Fatal 종료된다.
따라서 설정에 따라 실제 필요한 CRD만 대기한다.

### 8.7 동기화 상태 추적 타임라인

```
시간 →

Operator 시작                                  Agent 시작
    |                                              |
    |  CRD 등록                                    |  CRDSyncCell 시작
    |  ┌─ CNP CRD ──────┐                         |  SyncCRDs() 호출
    |  ├─ CEP CRD ──────┤                         |  Informer로 CRD Watch
    |  ├─ CN  CRD ──────┤                         |       |
    |  ├─ CID CRD ──────┤                         |       | CNP CRD 발견!
    |  └─ ... ──────────┘                         |       | CEP CRD 발견!
    |                                              |       | ... 계속 ...
    |  모든 CRD Established                        |       |
    |                                              |  모든 CRD 확인!
    |                                              |  Promise Resolve
    |                                              |       |
    |                                              |  Resource[T] Informer 시작
    |                                              |  CNP Informer ─── Watch 시작
    |                                              |  CEP Informer ─── Watch 시작
    |                                              |  CN  Informer ─── Watch 시작
    |                                              |       |
    |                                              |  캐시 동기화 완료
    |                                              |  CacheStatus 채널 닫기
    |                                              |  정상 동작 시작
```

---

## 9. Hive DI 통합

### 9.1 개요

Cilium의 K8s 통합 계층은 **Hive** 의존성 주입 프레임워크 위에 구축된다. 각 구성 요소는
`cell.Module`로 정의되고, `cell.Provide`로 의존성을 제공하며, `cell.Invoke`로 초기화된다.

### 9.2 K8s 관련 Hive Cell 구조

```
Hive (루트)
 |
 +-- k8s-client Cell
 |    └─ cell.Provide(newClientset)          → Clientset
 |
 +-- k8s-synced Cell
 |    ├─ cell.Provide(*APIGroups)            → API 그룹 추적
 |    ├─ cell.Provide(*Resources)            → 리소스 동기화 추적
 |    └─ cell.Provide(CacheStatus)           → 캐시 상태 채널
 |
 +-- k8s-synced-crdsync Cell
 |    ├─ cell.Provide(newCRDSyncPromise)     → Promise[CRDSync]
 |    └─ cell.Config(CRDSyncConfig)          → 타임아웃 설정
 |
 +-- create-crds Cell (Operator만)
 |    ├─ cell.Invoke(createCRDs)             → CRD 생성 실행
 |    └─ cell.Config(RegisterCRDsConfig)     → --skip-crd-creation
 |
 +-- 리소스 Cell들
      ├─ cell.Provide(ServiceResource)       → Resource[*slim_corev1.Service]
      ├─ cell.Provide(NodeResource)          → Resource[*slim_corev1.Node]
      ├─ cell.Provide(CiliumNodeResource)    → Resource[*CiliumNode]
      ├─ cell.Provide(CiliumNetworkPolicyResource) → Resource[*CNP]
      ├─ cell.Provide(CiliumIdentityResource)      → Resource[*CiliumIdentity]
      └─ ... (20+ 리소스)
```

### 9.3 의존성 주입 흐름 예시

`CiliumNetworkPolicyResource`가 어떻게 Hive를 통해 연결되는지 추적해보자.

```
1. Hive가 CiliumNetworkPolicyResource 함수를 호출
   - 필요한 의존성을 자동 주입:
     CiliumResourceParams {
       Logger:         *slog.Logger           ← 로깅 Cell에서 제공
       Lifecycle:      cell.Lifecycle          ← Hive 런타임에서 제공
       ClientSet:      client.Clientset        ← k8s-client Cell에서 제공
       CRDSyncPromise: Promise[CRDSync]        ← k8s-synced-crdsync Cell에서 제공
       MetricsProvider: workqueue.MetricsProvider ← 메트릭 Cell에서 제공
     }

2. CiliumNetworkPolicyResource 함수가 Resource[*CNP]를 반환
   - ListerWatcher 생성: ClientSet.CiliumV2().CiliumNetworkPolicies("")
   - Resource.New() 호출: Lifecycle에 Start/Stop 훅 등록

3. 다른 컴포넌트가 Resource[*CNP]를 의존성으로 요청
   - 예: PolicyWatcher가 CNP 이벤트를 구독
   - resource.Events(ctx) → needed 채널에 신호 → Informer 시작

4. Informer 시작 전 CRDSyncPromise.Await()
   - SyncCRDs()가 CNP CRD를 발견할 때까지 대기
   - CRD 확인 후 Informer가 실제 Watch 시작
```

### 9.4 cell.In과 cell.Out 패턴

```go
// cell.In — 의존성 주입 수신
type CiliumResourceParams struct {
    cell.In                                          // Hive에게 이 구조체가 DI 컨테이너임을 알림
    Logger          *slog.Logger
    Lifecycle       cell.Lifecycle
    ClientSet       client.Clientset
    CRDSyncPromise  promise.Promise[synced.CRDSync] `optional:"true"`  // 없어도 됨
    MetricsProvider workqueue.MetricsProvider
}

// cell.Out — 의존성 주입 제공
type RegisterCRDsFuncOut struct {
    cell.Out                                         // Hive에게 이 구조체가 제공자임을 알림
    Func RegisterCRDsFunc `group:"register-crd-funcs"`  // 그룹으로 여러 함수 수집
}
```

`group` 태그는 여러 Cell이 같은 타입의 값을 제공하고, 소비자가 이를 슬라이스로 받는
패턴이다. `createCRDs` 함수는 `RegisterCRDsFuncs []RegisterCRDsFunc`로 모든 CRD
등록 함수를 받아 순차 실행한다.

### 9.5 Lifecycle 훅

```go
// k8s-client Cell의 Lifecycle 훅
params.Lifecycle.Append(cell.Hook{
    OnStart: client.onStart,   // K8s 연결 확인, 하트비트 시작
    OnStop:  client.onStop,    // 연결 종료
})

// Resource[T]의 Lifecycle 훅 (New 함수에서)
lc.Append(r)  // resource는 cell.HookInterface를 구현

func (r *resource[T]) Start(cell.HookContext) error {
    r.wg.Add(1)
    go r.startWhenNeeded()  // 백그라운드에서 Lazy 시작
    return nil
}
```

---

## 10. 왜 이 아키텍처인가?

### 10.1 계층화된 추상화

```
                     사용 난이도
                         ↑
높음 (저수준)           |  K8s client-go (Informer, ListerWatcher)
                         |
                         |  Cilium Clientset (6개 클라이언트 합성)
                         |
                         |  Resource[T] (타입 안전, 이벤트 기반)
                         |
낮음 (고수준)           |  Resource 생성자 (CiliumNetworkPolicyResource 등)
                         ↓
```

각 계층은 명확한 책임을 가진다.

| 계층 | 책임 | 변경 빈도 |
|------|------|----------|
| client-go | K8s API 서버 통신 | 거의 없음 (K8s 버전 따라감) |
| Clientset | 클라이언트 구성, 직렬화 | 클라이언트 추가/제거 시 |
| Resource[T] | 이벤트 추상화, 캐싱 | 프레임워크 개선 시 |
| Resource 생성자 | 특정 리소스 연결 | 새 CRD 추가 시 |

### 10.2 Operator-Agent 역할 분리

```
+-------------------+                    +-------------------+
|   Cilium Operator  |                    |   Cilium Agent     |
+-------------------+                    +-------------------+
| 역할:              |                    | 역할:              |
| - CRD 등록/관리    |                    | - CRD Watch/반응   |
| - 클러스터 수준 조정 |     CRD 등록       | - 노드 수준 적용    |
| - IP 할당          | ─────────────────→ | - 정책 적용        |
| - 가비지 컬렉션    |                    | - eBPF 프로그래밍   |
+-------------------+                    +-------------------+
```

**왜 분리하는가?**

1. **권한 분리**: Operator만 CRD 생성 권한 필요 (RBAC 최소 권한 원칙)
2. **시작 순서**: Operator가 CRD를 먼저 등록 → Agent가 CRD 확인 후 Watch 시작
3. **스케일링**: Operator는 클러스터당 1~2개, Agent는 노드마다 하나
4. **장애 격리**: Agent 재시작이 CRD에 영향 주지 않음

### 10.3 Promise 기반 동기화

**왜 Promise인가? (chan struct{} 대신)**

```go
// Promise 패턴
crdSyncResolver, crdSyncPromise := promise.New[CRDSync]()

// 제공자 (Operator/SyncCRDs)
crdSyncResolver.Resolve(CRDSync{})  // 성공
crdSyncResolver.Reject(err)          // 실패

// 소비자 (Resource[T])
result, err := crdSyncPromise.Await(ctx)  // 블로킹 대기
```

| 특성 | chan struct{} | Promise[T] |
|------|-------------|------------|
| 값 전달 | 불가능 (신호만) | 값 또는 에러 전달 가능 |
| 에러 처리 | 별도 채널 필요 | Reject()으로 에러 전파 |
| 다중 대기 | close()로 브로드캐스트 | Await()를 여러 곳에서 호출 가능 |
| 재사용 | 한 번 닫으면 끝 | 결과가 캐시됨 |
| 타입 안전 | 없음 | 제네릭으로 타입 안전 |

### 10.4 Lazy 시작과 자원 효율성

```
BGP 비활성화된 클러스터에서:

즉시 시작 방식:
  Informer 시작 → CRD 없음 → Watch 에러 → 재시도 → API 서버 부하
  (5개 BGP CRD × 재시도 = 불필요한 API 호출)

Lazy 시작 방식:
  Resource 등록 → Events()/Store() 미호출 → Informer 시작 안함
  (API 서버 부하 0)
```

### 10.5 Slim Clientset의 필요성

대규모 클러스터 환경에서의 메모리 분석:

```
10,000 노드 클러스터, 100,000 Pod:

표준 Pod 캐시:    100,000 × 3KB  = ~300MB
Slim Pod 캐시:    100,000 × 0.5KB = ~50MB
                                    ─────
                           절약:    ~250MB

managedFields 제거만으로도:
  100,000 × 1.5KB = ~150MB 절약
```

이는 cilium-agent가 각 노드에서 실행되므로, 노드당 수백 MB의 메모리 절약이
클러스터 전체에서 수십 GB의 절약으로 이어진다.

### 10.6 설계 결정 요약

| 설계 결정 | 이유 | 대안 대비 장점 |
|-----------|------|--------------|
| CRD 사용 | K8s 네이티브 API 통합 | ConfigMap/Annotation보다 스키마 검증 |
| go:embed | 바이너리에 CRD YAML 내장 | 파일시스템 의존성 제거 |
| Composite Clientset | 6개 클라이언트 통합 | 개별 전달 대비 인터페이스 단순화 |
| Slim 타입 | 메모리 최적화 | 표준 타입 대비 60~90% 절약 |
| Resource[T] | 타입 안전 이벤트 스트림 | raw Informer 대비 에러 방지 |
| Lazy 시작 | 불필요한 Watch 방지 | 즉시 시작 대비 API 서버 부하 절감 |
| CRDSync Promise | Operator-Agent 동기화 | 폴링 대비 정확한 시점 감지 |
| Fatal on sync failure | Fail-safe 보장 | 부분 데이터로 동작 시 보안 위험 |

---

## 11. 참고 파일 목록

### CRD 타입 정의

| 파일 | 내용 |
|------|------|
| `pkg/k8s/apis/cilium.io/v2/register.go` | SchemeGroupVersion, CRD Kind 상수, addKnownTypes() |
| `pkg/k8s/apis/cilium.io/v2/cnp_types.go` | CiliumNetworkPolicy 구조체, DeepEqual |
| `pkg/k8s/apis/cilium.io/v2/types.go` | CiliumEndpoint, CiliumNode 등 구조체 |
| `pkg/k8s/apis/cilium.io/v2alpha1/register.go` | v2alpha1 CRD 상수 (CES, PodIPPool 등) |

### CRD 등록

| 파일 | 내용 |
|------|------|
| `pkg/k8s/apis/cilium.io/client/register.go` | CRD YAML go:embed, CreateCustomResourceDefinitions(), GetPregeneratedCRD() |
| `pkg/k8s/apis/crdhelpers/register.go` | CreateUpdateCRD(), NeedsUpdateV1Factory() |
| `pkg/k8s/apis/cell.go` | RegisterCRDsCell, --skip-crd-creation |

### Resource 프레임워크

| 파일 | 내용 |
|------|------|
| `pkg/k8s/resource/resource.go` | Resource[T] 인터페이스, New[T](), 옵션 패턴, Lazy 시작 |
| `pkg/k8s/resource_ctors.go` | ServiceResource, CiliumNodeResource, CiliumNetworkPolicyResource 등 |

### K8s 클라이언트

| 파일 | 내용 |
|------|------|
| `pkg/k8s/client/cell.go` | Clientset 인터페이스, compositeClientset, newClientset() |
| `pkg/k8s/slim/k8s/api/core/v1/types.go` | Slim Pod, Service, Node 타입 |

### 워처와 동기화

| 파일 | 내용 |
|------|------|
| `pkg/k8s/watchers/watcher.go` | K8sWatcher, API 그룹 상수 |
| `pkg/k8s/synced/resources.go` | Resources, BlockWaitGroupToSyncResources() |
| `pkg/k8s/synced/crd.go` | SyncCRDs(), AgentCRDResourceNames(), AllCiliumCRDResourceNames() |
| `pkg/k8s/synced/cell.go` | CRDSyncCell, CRDSync Promise, CacheStatus |
| `pkg/k8s/synced/cache_status.go` | CacheStatus 타입 |
