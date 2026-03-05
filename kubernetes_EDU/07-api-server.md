# 07. API Server 심화

## 목차

1. [개요](#1-개요)
2. [GenericAPIServer 구조체](#2-genericapiserver-구조체)
3. [Delegation Chain 아키텍처](#3-delegation-chain-아키텍처)
4. [Handler Chain (요청 필터 체인)](#4-handler-chain-요청-필터-체인)
5. [API Group 설치 메커니즘](#5-api-group-설치-메커니즘)
6. [REST 핸들러 상세](#6-rest-핸들러-상세)
7. [Storage Provider 패턴](#7-storage-provider-패턴)
8. [요청 처리 전체 흐름](#8-요청-처리-전체-흐름)
9. [Discovery 메커니즘](#9-discovery-메커니즘)
10. [Graceful Shutdown](#10-graceful-shutdown)
11. [왜 이런 설계인가](#11-왜-이런-설계인가)
12. [정리](#12-정리)

---

## 1. 개요

Kubernetes API Server(kube-apiserver)는 클러스터의 모든 컴포넌트가 통신하는 중앙 허브다.
etcd와 직접 통신하는 유일한 컴포넌트이며, 모든 리소스 CRUD 작업, 인증/인가, Admission Control,
API Discovery를 담당한다.

API Server의 핵심 설계 원칙은 다음과 같다:

| 원칙 | 설명 |
|------|------|
| **Delegation** | 여러 API 서버를 체인으로 연결하여 확장 |
| **Handler Chain** | 미들웨어 패턴으로 인증, 인가, 감사 등을 조합 |
| **REST Storage** | 각 리소스를 독립적인 Storage 구현으로 관리 |
| **API Group** | 버전별 API를 그룹으로 묶어 독립적 진화 |

**핵심 소스 파일:**

```
staging/src/k8s.io/apiserver/pkg/server/
├── genericapiserver.go    # GenericAPIServer 구조체, API 설치
├── config.go              # Config, DefaultBuildHandlerChain
└── handler.go             # APIServerHandler

staging/src/k8s.io/apiserver/pkg/endpoints/
├── handlers/
│   ├── create.go          # CREATE 핸들러
│   ├── get.go             # GET/LIST 핸들러
│   ├── update.go          # UPDATE 핸들러
│   ├── delete.go          # DELETE 핸들러
│   ├── patch.go           # PATCH 핸들러
│   └── watch.go           # WATCH 핸들러
└── installer.go           # REST 라우트 설치

pkg/controlplane/apiserver/
└── apis.go                # RESTStorageProvider, InstallAPIs
```

---

## 2. GenericAPIServer 구조체

### 2.1 구조체 정의

`GenericAPIServer`는 API Server의 핵심 구조체다. 모든 Kubernetes API 서버
(kube-apiserver, extension-apiserver, aggregator)가 이 구조체를 내장(embed)한다.

**파일:** `staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go` (110행~308행)

```go
// GenericAPIServer contains state for a Kubernetes cluster api server.
type GenericAPIServer struct {
    // discoveryAddresses is used to build cluster IPs for discovery.
    discoveryAddresses discovery.Addresses

    // LoopbackClientConfig is a config for a privileged loopback connection
    LoopbackClientConfig *restclient.Config

    // minRequestTimeout is how short the request timeout can be.
    minRequestTimeout time.Duration

    // ShutdownTimeout is the timeout used for server shutdown.
    ShutdownTimeout time.Duration

    // legacyAPIGroupPrefixes is used to set up URL parsing for authorization
    legacyAPIGroupPrefixes sets.String

    // admissionControl is used to build the RESTStorage that backs an API Group.
    admissionControl admission.Interface

    // SecureServingInfo holds configuration of the TLS server.
    SecureServingInfo *SecureServingInfo

    // Serializer controls how common API objects are serialized
    Serializer runtime.NegotiatedSerializer

    // Handler holds the handlers being used by this API server
    Handler *APIServerHandler

    // DiscoveryGroupManager serves /apis in an unaggregated form.
    DiscoveryGroupManager discovery.GroupManager

    // AggregatedDiscoveryGroupManager serves /apis in an aggregated form.
    AggregatedDiscoveryGroupManager discoveryendpoint.ResourceManager

    // Authorizer determines whether a user is allowed to make a certain request.
    Authorizer authorizer.Authorizer

    // delegationTarget is the next delegate in the chain. This is never nil.
    delegationTarget DelegationTarget

    // PostStartHooks are each called after the server has started listening
    postStartHooks         map[string]postStartHookEntry
    preShutdownHooks       map[string]preShutdownHookEntry

    // healthz checks
    healthzRegistry healthCheckRegistry
    readyzRegistry  healthCheckRegistry
    livezRegistry   healthCheckRegistry

    // StorageVersionManager holds the storage versions of the API resources
    StorageVersionManager storageversion.Manager

    // EffectiveVersion determines which apis and features are available
    EffectiveVersion basecompatibility.EffectiveVersion

    // FeatureGate is a way to plumb feature gate through
    FeatureGate featuregate.FeatureGate

    // lifecycleSignals provides access to the various signals during life cycle
    lifecycleSignals lifecycleSignals

    // destroyFns contains functions called on shutdown to clean up resources.
    destroyFns []func()
}
```

### 2.2 핵심 필드 분석

| 필드 | 타입 | 역할 |
|------|------|------|
| `Handler` | `*APIServerHandler` | HTTP 요청 라우팅의 최상위 엔트리포인트 |
| `delegationTarget` | `DelegationTarget` | 다음 API 서버로의 위임 체인 |
| `admissionControl` | `admission.Interface` | Admission Controller 실행 |
| `Authorizer` | `authorizer.Authorizer` | RBAC/Webhook 인가 |
| `Serializer` | `runtime.NegotiatedSerializer` | 요청/응답 직렬화 |
| `postStartHooks` | `map[string]postStartHookEntry` | 서버 시작 후 실행할 훅 |
| `lifecycleSignals` | `lifecycleSignals` | Graceful shutdown 시그널 관리 |
| `EffectiveVersion` | `EffectiveVersion` | API 버전 호환성 관리 |

### 2.3 APIGroupInfo

API Group별 리소스와 스토리지 매핑을 담는 구조체다.

**파일:** `staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go` (72행~99행)

```go
type APIGroupInfo struct {
    PrioritizedVersions []schema.GroupVersion
    // Info about the resources in this group.
    // map from version to resource to the storage.
    VersionedResourcesStorageMap map[string]map[string]rest.Storage
    OptionsExternalVersion *schema.GroupVersion
    MetaGroupVersion *schema.GroupVersion
    Scheme *runtime.Scheme
    NegotiatedSerializer runtime.NegotiatedSerializer
    ParameterCodec runtime.ParameterCodec
    StaticOpenAPISpec map[string]*spec.Schema
}
```

`VersionedResourcesStorageMap`의 구조가 핵심이다:

```
VersionedResourcesStorageMap = {
    "v1": {
        "pods":       podStorage,        // rest.Storage 구현
        "pods/log":   podLogStorage,
        "pods/status": podStatusStorage,
        "services":   serviceStorage,
        "nodes":      nodeStorage,
        ...
    },
    "v1beta1": {
        ...
    }
}
```

---

## 3. Delegation Chain 아키텍처

### 3.1 DelegationTarget 인터페이스

Kubernetes API Server는 3개의 서버가 체인으로 연결된 구조다.
이 체인은 `DelegationTarget` 인터페이스로 구현된다.

**파일:** `staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go` (312행~341행)

```go
type DelegationTarget interface {
    // UnprotectedHandler returns a handler that is NOT protected by a normal chain
    UnprotectedHandler() http.Handler

    // PostStartHooks returns the post-start hooks that need to be combined
    PostStartHooks() map[string]postStartHookEntry

    // PreShutdownHooks returns the pre-stop hooks that need to be combined
    PreShutdownHooks() map[string]preShutdownHookEntry

    // HealthzChecks returns the healthz checks that need to be combined
    HealthzChecks() []healthz.HealthChecker

    // ListedPaths returns the paths for supporting an index
    ListedPaths() []string

    // NextDelegate returns the next delegationTarget in the chain
    NextDelegate() DelegationTarget

    // PrepareRun does post API installation setup steps.
    PrepareRun() preparedGenericAPIServer

    // MuxAndDiscoveryCompleteSignals exposes registered signals
    MuxAndDiscoveryCompleteSignals() map[string]<-chan struct{}

    // Destroy cleans up its resources on shutdown.
    Destroy()
}
```

### 3.2 Delegation Chain 구조

```
┌─────────────────────────────────────────────────────────────────────┐
│                        HTTP Request                                 │
└──────────────────────────┬──────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────────┐
│                 Aggregator API Server                                │
│  ┌───────────────────────────────────────────────────┐              │
│  │ - APIService 관리                                  │              │
│  │ - 외부 API Server로 프록시                         │              │
│  │ - /apis/metrics.k8s.io → metrics-server 라우팅    │              │
│  └───────────────────────┬───────────────────────────┘              │
│                          │ delegationTarget                         │
└──────────────────────────┼──────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────────┐
│                  KubeAPI Server                                      │
│  ┌───────────────────────────────────────────────────┐              │
│  │ - /api/v1 (core API: pods, services, nodes 등)    │              │
│  │ - /apis/apps/v1 (deployments, statefulsets 등)    │              │
│  │ - /apis/batch/v1 (jobs, cronjobs)                 │              │
│  └───────────────────────┬───────────────────────────┘              │
│                          │ delegationTarget                         │
└──────────────────────────┼──────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────────┐
│               API Extensions Server                                  │
│  ┌───────────────────────────────────────────────────┐              │
│  │ - CustomResourceDefinition (CRD) 관리              │              │
│  │ - CR(Custom Resource) 저장/조회                    │              │
│  │ - /apis/<custom-group>/<version>/<resource>       │              │
│  └───────────────────────┬───────────────────────────┘              │
│                          │ delegationTarget                         │
└──────────────────────────┼──────────────────────────────────────────┘
                           │
                           ▼
                     NotFoundHandler
```

### 3.3 Delegation 동작 방식

요청이 들어오면 체인의 각 서버가 자신이 처리할 수 있는지 확인한다:

1. **Aggregator**: APIService에 등록된 외부 API 서버 확인 → 프록시
2. **KubeAPI**: 내장 리소스(pods, services, deployments 등) 처리
3. **Extensions**: CRD로 정의된 커스텀 리소스 처리
4. **NotFound**: 어디에도 매칭되지 않으면 404 반환

`GenericAPIServer`의 `NextDelegate()` 메서드가 체인의 다음 서버를 반환한다:

```go
// genericapiserver.go 360행
func (s *GenericAPIServer) NextDelegate() DelegationTarget {
    return s.delegationTarget
}
```

### 3.4 왜 Delegation Chain인가?

| 이유 | 설명 |
|------|------|
| **분리** | 각 서버가 독립적으로 API를 관리, 관심사 분리 |
| **확장성** | CRD, API Aggregation으로 API 확장 가능 |
| **하위 호환성** | /api/v1 (레거시)와 /apis/ (모던) 공존 |
| **모듈화** | 각 서버를 독립적으로 테스트, 발전 가능 |

---

## 4. Handler Chain (요청 필터 체인)

### 4.1 DefaultBuildHandlerChain

모든 HTTP 요청은 Handler Chain을 통과한다. 이 체인은 `DefaultBuildHandlerChain` 함수에서 조립된다.

**파일:** `staging/src/k8s.io/apiserver/pkg/server/config.go` (1028행~1107행)

Handler Chain은 Go의 미들웨어 패턴을 사용한다. 마지막에 추가된 미들웨어가 가장 먼저 실행된다.
아래는 실행 순서(바깥 → 안쪽)다:

```go
func DefaultBuildHandlerChain(apiHandler http.Handler, c *Config) http.Handler {
    handler := apiHandler

    // === 안쪽 (마지막에 실행) ===

    // 인가 (Authorization)
    handler = genericapifilters.WithAuthorization(handler, c.Authorization.Authorizer, c.Serializer)

    // 우선순위 및 공정성 (Priority and Fairness) 또는 MaxInFlight 제한
    if c.FlowControl != nil {
        handler = genericfilters.WithPriorityAndFairness(handler, ...)
    } else {
        handler = genericfilters.WithMaxInFlightLimit(handler, ...)
    }

    // 사칭 (Impersonation)
    handler = impersonation.WithImpersonation(handler, c.Authorization.Authorizer, c.Serializer)

    // 감사 (Audit)
    handler = genericapifilters.WithAudit(handler, c.AuditBackend, ...)

    // 트레이싱 (Tracing)
    handler = genericapifilters.WithTracing(handler, c.TracerProvider)

    // 인증 (Authentication)
    handler = genericapifilters.WithAuthentication(handler, c.Authentication.Authenticator, ...)

    // CORS
    handler = genericfilters.WithCORS(handler, c.CorsAllowedOriginList, ...)

    // 경고 기록 (Warning Recorder)
    handler = genericapifilters.WithWarningRecorder(handler)

    // Non-LongRunning 요청 타임아웃
    handler = genericfilters.WithTimeoutForNonLongRunningRequests(handler, c.LongRunningFunc)

    // 요청 데드라인
    handler = genericapifilters.WithRequestDeadline(handler, ...)

    // WaitGroup (graceful shutdown을 위해)
    handler = genericfilters.WithWaitGroup(handler, ...)

    // Watch 종료 관리
    handler = genericfilters.WithWatchTerminationDuringShutdown(handler, ...)

    // Goaway (HTTP/2 연결 해제)
    handler = genericfilters.WithProbabilisticGoaway(handler, c.GoawayChance)

    // 캐시 제어
    handler = genericapifilters.WithCacheControl(handler)

    // HSTS
    handler = genericfilters.WithHSTS(handler, c.HSTSDirectives)

    // Retry-After (shutdown 중 거부)
    handler = genericfilters.WithRetryAfter(handler, ...)

    // HTTP 로깅
    handler = genericfilters.WithHTTPLogging(handler)

    // 레이턴시 트래커
    handler = genericapifilters.WithLatencyTrackers(handler)

    // 요청 정보 (RequestInfo) 파싱
    handler = genericapifilters.WithRequestInfo(handler, c.RequestInfoResolver)

    // 요청 수신 타임스탬프
    handler = genericapifilters.WithRequestReceivedTimestamp(handler)

    // MuxAndDiscovery 완료 대기
    handler = genericapifilters.WithMuxAndDiscoveryComplete(handler, ...)

    // 패닉 복구 (가장 바깥쪽)
    handler = genericfilters.WithPanicRecovery(handler, c.RequestInfoResolver)

    // === 바깥쪽 (가장 먼저 실행) ===
    return handler
}
```

### 4.2 Handler Chain 실행 순서 다이어그램

```
HTTP Request
    │
    ▼
┌──────────────────────────────────┐
│  1. PanicRecovery                │  패닉 발생 시 500 반환
├──────────────────────────────────┤
│  2. MuxAndDiscoveryComplete      │  API 설치 완료 대기
├──────────────────────────────────┤
│  3. RequestReceivedTimestamp      │  요청 도착 시간 기록
├──────────────────────────────────┤
│  4. RequestInfo                  │  URL → verb, resource, namespace 파싱
├──────────────────────────────────┤
│  5. LatencyTrackers              │  레이턴시 측정 시작
├──────────────────────────────────┤
│  6. HTTPLogging                  │  HTTP 요청 로깅
├──────────────────────────────────┤
│  7. RetryAfter                   │  shutdown 중이면 429 반환
├──────────────────────────────────┤
│  8. HSTS / CacheControl          │  보안 헤더 설정
├──────────────────────────────────┤
│  9. WaitGroup                    │  요청 카운팅 (graceful shutdown용)
├──────────────────────────────────┤
│ 10. RequestDeadline              │  요청 타임아웃 설정
├──────────────────────────────────┤
│ 11. TimeoutForNonLongRunning     │  비 long-running 요청 타임아웃
├──────────────────────────────────┤
│ 12. WarningRecorder              │  경고 헤더 기록
├──────────────────────────────────┤
│ 13. CORS                        │  Cross-Origin 허용
├──────────────────────────────────┤
│ 14. Authentication               │  인증: Bearer token, x509, OIDC 등
├──────────────────────────────────┤
│ 15. Tracing                      │  분산 트레이싱
├──────────────────────────────────┤
│ 16. Audit                        │  감사 로그 기록
├──────────────────────────────────┤
│ 17. Impersonation                │  사용자 사칭 처리
├──────────────────────────────────┤
│ 18. PriorityAndFairness          │  API 우선순위 기반 흐름제어
├──────────────────────────────────┤
│ 19. Authorization                │  RBAC/Webhook 인가
├──────────────────────────────────┤
│ 20. API Handler (실제 처리)      │  REST 핸들러 실행
└──────────────────────────────────┘
```

### 4.3 왜 Handler Chain 패턴인가?

Go의 `http.Handler` 래퍼 패턴을 사용하는 이유:

1. **조합 가능성**: 각 필터를 독립적으로 개발/테스트 가능
2. **순서 제어**: 인증 → 인가 → 감사 순서가 보장됨
3. **조건부 적용**: Feature Gate로 특정 필터 활성/비활성화
4. **성능**: Go 함수 호출로 구현되어 오버헤드 최소

---

## 5. API Group 설치 메커니즘

### 5.1 InstallLegacyAPIGroup

`/api/v1` 아래의 레거시 코어 API(pods, services, nodes 등)를 설치한다.

**파일:** `staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go` (859행~882행)

```go
func (s *GenericAPIServer) InstallLegacyAPIGroup(apiPrefix string, apiGroupInfo *APIGroupInfo) error {
    if !s.legacyAPIGroupPrefixes.Has(apiPrefix) {
        return fmt.Errorf("%q is not in the allowed legacy API prefixes: %v",
            apiPrefix, s.legacyAPIGroupPrefixes.List())
    }

    openAPIModels, err := s.getOpenAPIModels(apiPrefix, apiGroupInfo)
    if err != nil {
        return fmt.Errorf("unable to get openapi models: %v", err)
    }

    if err := s.installAPIResources(apiPrefix, apiGroupInfo, openAPIModels); err != nil {
        return err
    }

    // Install the version handler at /<apiPrefix> to enumerate supported versions.
    legacyRootAPIHandler := discovery.NewLegacyRootAPIHandler(
        s.discoveryAddresses, s.Serializer, apiPrefix)
    wrapped := discoveryendpoint.WrapAggregatedDiscoveryToHandler(
        legacyRootAPIHandler,
        s.AggregatedLegacyDiscoveryGroupManager,
        s.AggregatedLegacyDiscoveryGroupManager)
    s.Handler.GoRestfulContainer.Add(
        wrapped.GenerateWebService("/api", metav1.APIVersions{}))
    s.registerStorageReadinessCheck("", apiGroupInfo)

    return nil
}
```

### 5.2 InstallAPIGroups

`/apis/<groupName>/<version>` 아래의 모던 API를 설치한다.

**파일:** `staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go` (887행~941행)

```go
func (s *GenericAPIServer) InstallAPIGroups(apiGroupInfos ...*APIGroupInfo) error {
    for _, apiGroupInfo := range apiGroupInfos {
        // 버전 우선순위 검증
        if len(apiGroupInfo.PrioritizedVersions) == 0 {
            return fmt.Errorf("no version priority set for %#v", *apiGroupInfo)
        }
        // 빈 그룹/버전 검증
        if len(apiGroupInfo.PrioritizedVersions[0].Group) == 0 {
            return fmt.Errorf("cannot register handler with an empty group")
        }
    }

    openAPIModels, err := s.getOpenAPIModels(APIGroupPrefix, apiGroupInfos...)
    if err != nil {
        return fmt.Errorf("unable to get openapi models: %v", err)
    }

    for _, apiGroupInfo := range apiGroupInfos {
        if err := s.installAPIResources(APIGroupPrefix, apiGroupInfo, openAPIModels); err != nil {
            return fmt.Errorf("unable to install api resources: %v", err)
        }

        // Discovery 설정
        apiVersionsForDiscovery := []metav1.GroupVersionForDiscovery{}
        for _, groupVersion := range apiGroupInfo.PrioritizedVersions {
            if len(apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version]) == 0 {
                continue
            }
            apiVersionsForDiscovery = append(apiVersionsForDiscovery,
                metav1.GroupVersionForDiscovery{
                    GroupVersion: groupVersion.String(),
                    Version:      groupVersion.Version,
                })
        }

        apiGroup := metav1.APIGroup{
            Name:     apiGroupInfo.PrioritizedVersions[0].Group,
            Versions: apiVersionsForDiscovery,
            PreferredVersion: metav1.GroupVersionForDiscovery{
                GroupVersion: apiGroupInfo.PrioritizedVersions[0].String(),
                Version:      apiGroupInfo.PrioritizedVersions[0].Version,
            },
        }

        s.DiscoveryGroupManager.AddGroup(apiGroup)
        s.Handler.GoRestfulContainer.Add(
            discovery.NewAPIGroupHandler(s.Serializer, apiGroup).WebService())
        s.registerStorageReadinessCheck(
            apiGroupInfo.PrioritizedVersions[0].Group, apiGroupInfo)
    }
    return nil
}
```

### 5.3 installAPIResources

각 API GroupVersion에 대해 REST 핸들러를 실제로 설치하는 내부 메서드다.

**파일:** `staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go` (794행~854행)

```go
func (s *GenericAPIServer) installAPIResources(
    apiPrefix string,
    apiGroupInfo *APIGroupInfo,
    typeConverter managedfields.TypeConverter) error {

    var resourceInfos []*storageversion.ResourceInfo
    for _, groupVersion := range apiGroupInfo.PrioritizedVersions {
        if len(apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version]) == 0 {
            klog.Warningf("Skipping API %v because it has no resources.", groupVersion)
            continue
        }

        apiGroupVersion, err := s.getAPIGroupVersion(apiGroupInfo, groupVersion, apiPrefix)
        if err != nil {
            return err
        }
        apiGroupVersion.TypeConverter = typeConverter
        apiGroupVersion.MaxRequestBodyBytes = s.maxRequestBodyBytes

        // REST 라우트를 GoRestfulContainer에 설치
        discoveryAPIResources, r, err := apiGroupVersion.InstallREST(
            s.Handler.GoRestfulContainer)
        if err != nil {
            return fmt.Errorf("unable to setup API %v: %v", apiGroupInfo, err)
        }
        resourceInfos = append(resourceInfos, r...)

        // Aggregated Discovery에 추가
        s.AggregatedDiscoveryGroupManager.AddGroupVersion(
            groupVersion.Group,
            apidiscoveryv2.APIVersionDiscovery{
                Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent,
                Version:   groupVersion.Version,
                Resources: discoveryAPIResources,
            },
        )
    }

    s.RegisterDestroyFunc(apiGroupInfo.destroyStorage)
    return nil
}
```

### 5.4 API 설치 흐름 다이어그램

```
InstallAPIs()
    │
    ├── 각 RESTStorageProvider에 대해:
    │   │
    │   ├── NewRESTStorage()
    │   │   └── APIGroupInfo { VersionedResourcesStorageMap: {...} } 생성
    │   │
    │   ├── RemoveUnavailableKinds()  ← EffectiveVersion 기반 필터링
    │   │
    │   └── InstallAPIGroups() 또는 InstallLegacyAPIGroup()
    │       │
    │       ├── installAPIResources()
    │       │   │
    │       │   ├── getAPIGroupVersion()
    │       │   │   └── Storage map에서 APIGroupVersion 생성
    │       │   │
    │       │   └── apiGroupVersion.InstallREST()
    │       │       └── 각 리소스에 대해 HTTP 라우트 등록:
    │       │           POST   /apis/{group}/{version}/{resource}
    │       │           GET    /apis/{group}/{version}/{resource}/{name}
    │       │           PUT    /apis/{group}/{version}/{resource}/{name}
    │       │           DELETE /apis/{group}/{version}/{resource}/{name}
    │       │           PATCH  /apis/{group}/{version}/{resource}/{name}
    │       │           GET    /apis/{group}/{version}/{resource}  (LIST)
    │       │           GET    /apis/{group}/{version}/{resource}?watch=true
    │       │
    │       └── Discovery 핸들러 등록
    │
    └── 완료
```

---

## 6. REST 핸들러 상세

### 6.1 Create 핸들러

**파일:** `staging/src/k8s.io/apiserver/pkg/endpoints/handlers/create.go` (53행~)

```go
func createHandler(r rest.NamedCreater, scope *RequestScope,
    admit admission.Interface, includeName bool) http.HandlerFunc {
    return func(w http.ResponseWriter, req *http.Request) {
        ctx := req.Context()
        ctx, span := tracing.Start(ctx, "Create", traceFields(req)...)
        defer span.End(500 * time.Millisecond)

        // 1. namespace, name 파싱
        namespace, name, err := scope.Namer.Name(req)

        // 2. 타임아웃 설정 (최대 34초)
        ctx, cancel := context.WithTimeout(ctx, requestTimeoutUpperBound)
        defer cancel()

        // 3. Content Negotiation
        outputMediaType, _, err := negotiation.NegotiateOutputMediaType(req, ...)
        s, err := negotiation.NegotiateInputSerializer(req, false, ...)

        // 4. Body 읽기 (maxRequestBodyBytes 제한)
        body, err := limitedReadBodyWithRecordMetric(ctx, req, ...)

        // 5. CreateOptions 파싱
        options := &metav1.CreateOptions{}
        metainternalversionscheme.ParameterCodec.DecodeParameters(values, ...)

        // 6. 역직렬화 (JSON/YAML/Protobuf → Go 객체)
        decoder := scope.Serializer.DecoderToVersion(decodeSerializer, scope.HubGroupVersion)
        obj, gvk, err := decoder.Decode(body, &defaultGVK, original)

        // 7. Admission Control (Mutating + Validating)
        // 8. Storage에 저장
        // 9. 응답 직렬화 및 반환
    }
}
```

### 6.2 Get 핸들러

**파일:** `staging/src/k8s.io/apiserver/pkg/endpoints/handlers/get.go` (57행~)

```go
func getResourceHandler(scope *RequestScope, getter getterFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, req *http.Request) {
        ctx := req.Context()
        ctx, span := tracing.Start(ctx, "Get", traceFields(req)...)
        defer span.End(500 * time.Millisecond)

        // 1. namespace, name 파싱
        namespace, name, err := scope.Namer.Name(req)
        ctx = request.WithNamespace(ctx, namespace)

        // 2. Content Negotiation
        outputMediaType, _, err := negotiation.NegotiateOutputMediaType(req, ...)

        // 3. Storage에서 조회
        result, err := getter(ctx, name, req)

        // 4. 응답 직렬화 및 반환
    }
}
```

### 6.3 REST 핸들러 요약

| 핸들러 | 파일 | HTTP Method | Storage 호출 |
|--------|------|-------------|-------------|
| Create | `create.go` | POST | `rest.Creater.Create()` |
| Get | `get.go` | GET (단일) | `rest.Getter.Get()` |
| List | `get.go` | GET (목록) | `rest.Lister.List()` |
| Update | `update.go` | PUT | `rest.Updater.Update()` |
| Patch | `patch.go` | PATCH | `rest.Patcher.Update()` |
| Delete | `delete.go` | DELETE | `rest.GracefulDeleter.Delete()` |
| Watch | `watch.go` | GET (?watch=true) | `rest.Watcher.Watch()` |

### 6.4 요청 처리 상세 흐름 (Create 예시)

```
Client: POST /api/v1/namespaces/default/pods
    │
    ▼
┌────────────────────────────────┐
│  Content Negotiation           │  Accept/Content-Type 확인
├────────────────────────────────┤
│  Body 읽기 및 크기 검증        │  maxRequestBodyBytes 확인
├────────────────────────────────┤
│  Decode (역직렬화)              │  JSON → internal Pod 객체
├────────────────────────────────┤
│  Defaulting                    │  기본값 설정 (restartPolicy 등)
├────────────────────────────────┤
│  Conversion                    │  외부 버전 → 내부 버전 변환
├────────────────────────────────┤
│  Admission (Mutating)          │  Mutating Webhook 실행
├────────────────────────────────┤
│  Validation                    │  객체 유효성 검증
├────────────────────────────────┤
│  Admission (Validating)        │  Validating Webhook 실행
├────────────────────────────────┤
│  Storage.Create()              │  etcd에 저장
├────────────────────────────────┤
│  Encode (직렬화)                │  internal Pod → JSON
├────────────────────────────────┤
│  Response                      │  HTTP 201 Created
└────────────────────────────────┘
```

---

## 7. Storage Provider 패턴

### 7.1 RESTStorageProvider 인터페이스

각 API 그룹은 `RESTStorageProvider`를 구현하여 해당 그룹의 리소스 스토리지를 생성한다.

**파일:** `pkg/controlplane/apiserver/apis.go` (42행~45행)

```go
type RESTStorageProvider interface {
    GroupName() string
    NewRESTStorage(
        apiResourceConfigSource serverstorage.APIResourceConfigSource,
        restOptionsGetter generic.RESTOptionsGetter,
    ) (genericapiserver.APIGroupInfo, error)
}
```

### 7.2 기본 Storage Provider 목록

**파일:** `pkg/controlplane/apiserver/apis.go` (64행~85행)

`GenericStorageProviders()` 메서드가 반환하는 기본 제공자들:

```go
func (c *CompletedConfig) GenericStorageProviders(
    discovery discovery.DiscoveryInterface) ([]RESTStorageProvider, error) {
    return []RESTStorageProvider{
        c.NewCoreGenericConfig(),                   // core (pods, services, ...)
        apiserverinternalrest.StorageProvider{},     // apiserverinternal
        authenticationrest.RESTStorageProvider{...}, // authentication
        authorizationrest.RESTStorageProvider{...},  // authorization
        certificatesrest.RESTStorageProvider{...},   // certificates
        coordinationrest.RESTStorageProvider{},      // coordination (leases)
        rbacrest.RESTStorageProvider{...},           // rbac
        svmrest.RESTStorageProvider{},               // storagemigration
        flowcontrolrest.RESTStorageProvider{...},    // flowcontrol
        admissionregistrationrest.RESTStorageProvider{...}, // admissionregistration
        eventsrest.RESTStorageProvider{TTL: ...},    // events
    }, nil
}
```

### 7.3 InstallAPIs

Storage Provider에서 받은 APIGroupInfo를 실제로 설치한다.

**파일:** `pkg/controlplane/apiserver/apis.go` (88행~)

```go
func (s *Server) InstallAPIs(restStorageProviders ...RESTStorageProvider) error {
    nonLegacy := []*genericapiserver.APIGroupInfo{}

    // EffectiveVersion 기반 만료 리소스 평가기
    resourceExpirationEvaluator, err := genericapiserver.NewResourceExpirationEvaluatorFromOptions(...)

    for _, restStorageBuilder := range restStorageProviders {
        groupName := restStorageBuilder.GroupName()

        // 1. REST Storage 생성
        apiGroupInfo, err := restStorageBuilder.NewRESTStorage(
            s.APIResourceConfigSource, s.RESTOptionsGetter)

        // 2. 리소스가 없으면 스킵
        if len(apiGroupInfo.VersionedResourcesStorageMap) == 0 {
            klog.Infof("API group %q is not enabled, skipping.", groupName)
            continue
        }

        // 3. EffectiveVersion 기반으로 사용 불가 리소스 제거
        err = resourceExpirationEvaluator.RemoveUnavailableKinds(
            groupName, apiGroupInfo.Scheme,
            apiGroupInfo.VersionedResourcesStorageMap, ...)

        // 4. 설치
        nonLegacy = append(nonLegacy, &apiGroupInfo)
    }

    // GenericAPIServer에 등록
    if err := s.GenericAPIServer.InstallAPIGroups(nonLegacy...); err != nil {
        return fmt.Errorf("error in registering group versions: %v", err)
    }
    return nil
}
```

### 7.4 Storage Provider 계층도

```
RESTStorageProvider (인터페이스)
    │
    ├── corerest.GenericConfig
    │   └── NewRESTStorage() → APIGroupInfo {
    │       "v1": {
    │           "pods":           podStorage,
    │           "pods/log":       podLogStorage,
    │           "pods/status":    podStatusStorage,
    │           "services":       serviceStorage,
    │           "nodes":          nodeStorage,
    │           "namespaces":     namespaceStorage,
    │           "configmaps":     configMapStorage,
    │           "secrets":        secretStorage,
    │           "events":         eventStorage,
    │           ...
    │       }
    │   }
    │
    ├── rbacrest.RESTStorageProvider
    │   └── NewRESTStorage() → APIGroupInfo {
    │       "v1": {
    │           "roles":               roleStorage,
    │           "rolebindings":         roleBindingStorage,
    │           "clusterroles":         clusterRoleStorage,
    │           "clusterrolebindings":  clusterRoleBindingStorage,
    │       }
    │   }
    │
    ├── certificatesrest.RESTStorageProvider
    │   └── NewRESTStorage() → ...
    │
    └── ...
```

---

## 8. 요청 처리 전체 흐름

### 8.1 전체 ASCII 다이어그램

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          kubectl get pods                               │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │ HTTPS
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                         TLS Termination                                 │
│                    (SecureServingInfo)                                   │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                      Handler Chain (20단계)                              │
│                                                                         │
│  PanicRecovery → RequestInfo → Authentication → Audit →                 │
│  Impersonation → PriorityAndFairness → Authorization → ...             │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                       APIServerHandler.Director                         │
│                                                                         │
│  GoRestfulContainer에서 URL 매칭                                         │
│  /api/v1/namespaces/default/pods → core API handler                    │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                      REST Handler (get.go)                              │
│                                                                         │
│  1. Namespace/Name 파싱                                                 │
│  2. Content Negotiation                                                 │
│  3. Storage.List() 호출                                                 │
│  4. 응답 직렬화                                                         │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                         Storage Layer                                   │
│                                                                         │
│  Cacher (watchCache) → etcd3 store → etcd cluster                      │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                        HTTP Response                                    │
│  200 OK                                                                 │
│  Content-Type: application/json                                         │
│  { "kind": "PodList", "items": [...] }                                 │
└─────────────────────────────────────────────────────────────────────────┘
```

### 8.2 RequestInfo 파싱

URL에서 verb, apiGroup, version, resource, name, namespace를 추출한다:

```
GET /apis/apps/v1/namespaces/default/deployments/nginx
     ├──┤ ├─┤ ├───────────┤ ├──────┤ ├──────────┤ ├───┤
     │    │    namespace    │   resource        name
     │    version           │
     apiGroup="apps"        subresource=""
     verb="get"
```

```
GET /api/v1/namespaces/default/pods?watch=true
     ├─┤├─┤ ├───────────┤ ├──────┤
     │   │   namespace    resource
     │   version
     apiGroup="" (core)
     verb="watch"
```

---

## 9. Discovery 메커니즘

### 9.1 API Discovery 엔드포인트

| 엔드포인트 | 반환 내용 |
|-----------|----------|
| `/api` | 레거시 API 버전 목록 (`v1`) |
| `/api/v1` | v1 리소스 목록 (pods, services, ...) |
| `/apis` | 모든 API 그룹 목록 |
| `/apis/<group>` | 특정 그룹의 버전 목록 |
| `/apis/<group>/<version>` | 특정 버전의 리소스 목록 |

### 9.2 DiscoveryGroupManager

`InstallAPIGroups()`에서 각 API 그룹이 설치될 때 자동으로 Discovery에 등록된다:

```go
// genericapiserver.go 936행
s.DiscoveryGroupManager.AddGroup(apiGroup)
s.Handler.GoRestfulContainer.Add(
    discovery.NewAPIGroupHandler(s.Serializer, apiGroup).WebService())
```

### 9.3 Aggregated Discovery

Kubernetes 1.27+에서는 `/apis`에 대한 단일 요청으로 모든 리소스 정보를 가져올 수 있다.
`AggregatedDiscoveryGroupManager`가 이를 관리한다:

```go
// genericapiserver.go 159행~163행
AggregatedDiscoveryGroupManager discoveryendpoint.ResourceManager
PeerAggregatedDiscoveryManager discoveryendpoint.PeerAggregatedResourceManager
AggregatedLegacyDiscoveryGroupManager discoveryendpoint.ResourceManager
```

---

## 10. Graceful Shutdown

### 10.1 Shutdown 시그널 체계

`GenericAPIServer`는 `lifecycleSignals`를 통해 종료 과정을 관리한다:

```go
// genericapiserver.go 272행
lifecycleSignals lifecycleSignals
```

### 10.2 Shutdown 과정

**파일:** `staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go` (610행~)

```
1. SIGTERM 수신
   │
   ▼
2. ShutdownDelayDuration 대기 (기본 0초)
   │  └── /readyz 실패 시작, 로드밸런서가 트래픽 전환
   │
   ▼
3. PreShutdownHooks 실행
   │  └── Lease 해제 등 정리 작업
   │
   ▼
4. NotAcceptingNewRequest 시그널
   │  └── 새 요청 거부 시작 (429 Retry-After)
   │
   ▼
5. Non-LongRunning 요청 드레인
   │  └── NonLongRunningRequestWaitGroup.Wait()
   │
   ▼
6. Watch 요청 드레인 (grace period만큼)
   │  └── WatchRequestWaitGroup.Wait()
   │
   ▼
7. HTTP Server 종료
   │
   ▼
8. Audit Backend 종료
   │
   ▼
9. Storage 정리
   └── destroyFns 실행
```

### 10.3 WaitGroup 기반 요청 추적

Handler Chain의 `WithWaitGroup` 필터가 모든 비 long-running 요청을 추적한다:

```go
// config.go 1084행
handler = genericfilters.WithWaitGroup(
    handler, c.LongRunningFunc, c.NonLongRunningRequestWaitGroup)
```

Watch와 같은 long-running 요청은 별도의 `WatchRequestWaitGroup`으로 추적된다:

```go
// config.go 1085~1087행
if c.ShutdownWatchTerminationGracePeriod > 0 {
    handler = genericfilters.WithWatchTerminationDuringShutdown(
        handler, c.lifecycleSignals, c.WatchRequestWaitGroup)
}
```

---

## 11. 왜 이런 설계인가

### 11.1 왜 3개의 서버 체인인가?

| 질문 | 답변 |
|------|------|
| 왜 Aggregator가 맨 앞인가? | 외부 API 서버(metrics-server 등)로의 프록시를 먼저 처리해야 내장 API와 충돌 방지 |
| 왜 Extensions가 맨 뒤인가? | CRD는 동적으로 추가/삭제되므로 내장 API와 이름 충돌 시 내장 API가 우선 |
| 왜 분리했는가? | 각 서버가 독립적으로 진화 가능. Aggregator 없이도 KubeAPI 동작 가능 |

### 11.2 왜 Handler Chain 패턴인가?

**확장성**: 새로운 필터(예: PriorityAndFairness)를 기존 코드 수정 없이 추가 가능

**테스트 용이성**: 각 필터를 독립적으로 단위 테스트 가능

**성능**: 함수 호출 체인이므로 리플렉션/인터페이스 디스패치 오버헤드 없음

### 11.3 왜 Storage Provider 패턴인가?

**캡슐화**: 각 API 그룹이 자신의 스토리지 로직을 완전히 캡슐화

**지연 초기화**: 활성화된 API 그룹만 스토리지 생성 (메모리 절약)

**버전 관리**: 같은 리소스의 여러 버전(v1, v1beta1)을 하나의 Provider에서 관리

### 11.4 왜 go-restful 프레임워크인가?

Kubernetes API Server는 내부적으로 `go-restful` 라이브러리를 사용한다:

- **WebService 추상화**: API 그룹별로 라우트를 WebService로 묶어 관리
- **OpenAPI 자동 생성**: go-restful의 라우트 정보로 OpenAPI 스펙 자동 생성
- **Content Negotiation**: JSON, YAML, Protobuf, CBOR 등 다양한 포맷 지원
- **표준 HTTP**: 결국 `net/http.Handler`로 변환되어 Go 생태계와 호환

### 11.5 왜 리소스별 rest.Storage 인터페이스인가?

```go
// rest.Storage는 최소 인터페이스
type Storage interface {
    New() runtime.Object
    Destroy()
}

// 필요한 기능만 선택적으로 구현
type Getter interface { ... }      // GET
type Lister interface { ... }      // LIST
type Creater interface { ... }     // POST
type Updater interface { ... }     // PUT
type GracefulDeleter interface { ... }  // DELETE
type Watcher interface { ... }     // WATCH
```

각 리소스는 필요한 인터페이스만 구현한다:

- `ConfigMap`: CRUD 전부 (Getter, Lister, Creater, Updater, GracefulDeleter, Watcher)
- `SubjectAccessReview`: Create만 (Creater)
- `pods/log`: Get만 (Getter)

이 설계의 장점:
- **최소 권한 원칙**: 리소스가 지원하지 않는 동사는 자동으로 405 Method Not Allowed
- **타입 안전**: 컴파일 타임에 누락된 인터페이스 구현 감지
- **문서화**: 인터페이스가 곧 리소스의 기능 명세

---

## 12. 정리

### 핵심 구조 요약

```
┌─────────────────────────────────────────────────────────┐
│                  kube-apiserver 프로세스                  │
│                                                         │
│  ┌─────────────────────────────────────────────┐        │
│  │         Aggregator API Server               │        │
│  │  ┌───────────────────────────────────┐      │        │
│  │  │      KubeAPI Server               │      │        │
│  │  │  ┌─────────────────────────┐      │      │        │
│  │  │  │   Extensions Server     │      │      │        │
│  │  │  │  (CRD)                  │      │      │        │
│  │  │  └─────────────────────────┘      │      │        │
│  │  └───────────────────────────────────┘      │        │
│  └─────────────────────────────────────────────┘        │
│                                                         │
│  각 서버는 GenericAPIServer를 내장하고                    │
│  delegationTarget으로 체인 연결                           │
│                                                         │
│  ┌────────────┐   ┌──────────────┐   ┌───────────┐     │
│  │ Handler    │──▶│ REST Handler │──▶│ Storage   │     │
│  │ Chain      │   │ (CRUD)       │   │ (etcd3)   │     │
│  │ (20 필터)  │   └──────────────┘   └───────────┘     │
│  └────────────┘                                         │
└─────────────────────────────────────────────────────────┘
```

### 핵심 파일 참조

| 파일 | 행 | 내용 |
|------|-----|------|
| `genericapiserver.go` | 72~99 | APIGroupInfo 구조체 |
| `genericapiserver.go` | 110~308 | GenericAPIServer 구조체 |
| `genericapiserver.go` | 312~341 | DelegationTarget 인터페이스 |
| `genericapiserver.go` | 794~854 | installAPIResources 메서드 |
| `genericapiserver.go` | 859~882 | InstallLegacyAPIGroup 메서드 |
| `genericapiserver.go` | 887~941 | InstallAPIGroups 메서드 |
| `config.go` | 1028~1107 | DefaultBuildHandlerChain 함수 |
| `handlers/create.go` | 53~ | createHandler 함수 |
| `handlers/get.go` | 57~ | getResourceHandler 함수 |
| `apis.go` | 42~45 | RESTStorageProvider 인터페이스 |
| `apis.go` | 64~85 | GenericStorageProviders 메서드 |
| `apis.go` | 88~ | InstallAPIs 메서드 |

### 설계 키워드

- **Delegation Chain**: 확장 가능한 API 서버 체인
- **Handler Chain**: Go 미들웨어 패턴의 보안/관리 필터
- **Storage Provider**: API 그룹별 캡슐화된 스토리지 팩토리
- **REST 인터페이스 조합**: 리소스별 최소 기능 구현
- **Graceful Shutdown**: WaitGroup 기반 요청 드레인
