# 17. CRD API 확장 심화

## 목차
1. [개요](#1-개요)
2. [CustomResourceDefinition 구조체](#2-customresourcedefinition-구조체)
3. [CRD 라이프사이클](#3-crd-라이프사이클)
4. [API Aggregation](#4-api-aggregation)
5. [Server Chain 아키텍처](#5-server-chain-아키텍처)
6. [CRD Validation](#6-crd-validation)
7. [CRD Versioning과 Conversion Webhook](#7-crd-versioning과-conversion-webhook)
8. [apiextensions-apiserver 내부 구조](#8-apiextensions-apiserver-내부-구조)
9. [kube-aggregator 내부 구조](#9-kube-aggregator-내부-구조)
10. [CRD 컨트롤러들](#10-crd-컨트롤러들)
11. [CRD Subresources](#11-crd-subresources)
12. [설계 원칙: Why](#12-설계-원칙-why)

---

## 1. 개요

Kubernetes의 API 확장 메커니즘은 **Kubernetes를 재컴파일하지 않고도** 새로운 API 리소스를 추가할 수 있게 해주는 핵심 설계이다. 이는 Kubernetes 생태계의 폭발적 성장(Istio, Argo, Cert-Manager 등)을 가능하게 한 근본적인 아키텍처 결정이다.

API 확장에는 두 가지 주요 메커니즘이 있다:

| 메커니즘 | 복잡도 | 유연성 | 사용 시나리오 |
|---------|-------|--------|-------------|
| **CRD** | 낮음 | 중간 | 대부분의 커스텀 리소스 |
| **API Aggregation** | 높음 | 높음 | 전용 API 서버가 필요한 경우 |

### 핵심 소스 파일

| 파일 | 역할 |
|------|------|
| `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` | CRD 타입 정의 |
| `staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/apiserver.go` | CRD API 서버 |
| `staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/customresource_handler.go` | CR 요청 핸들러 |
| `staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/types.go` | APIService 타입 정의 |
| `staging/src/k8s.io/kube-aggregator/pkg/apiserver/apiserver.go` | Aggregator 서버 |
| `staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go` | 프록시 핸들러 |

---

## 2. CustomResourceDefinition 구조체

### CRD 최상위 구조

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 401-413)

```go
// staging/.../apiextensions/v1/types.go:401
type CustomResourceDefinition struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   CustomResourceDefinitionSpec   `json:"spec"`
    Status CustomResourceDefinitionStatus `json:"status,omitempty"`
}
```

CRD는 **cluster-scoped 리소스**이다. `+genclient:nonNamespaced` 태그가 이를 확인해준다 (라인 395).

### CRDSpec - CRD의 핵심 명세

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 40-73)

```go
// staging/.../apiextensions/v1/types.go:41
type CustomResourceDefinitionSpec struct {
    Group                 string                             // API 그룹
    Names                 CustomResourceDefinitionNames      // 리소스 이름
    Scope                 ResourceScope                      // Cluster | Namespaced
    Versions              []CustomResourceDefinitionVersion  // 버전 목록
    Conversion            *CustomResourceConversion          // 버전 변환
    PreserveUnknownFields bool                               // 미지 필드 보존
}
```

### CRD Names - 리소스 이름 체계

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 252-279)

```go
// staging/.../apiextensions/v1/types.go:252
type CustomResourceDefinitionNames struct {
    Plural     string   // 복수형: "crontabs"      -> /apis/<group>/<version>/crontabs
    Singular   string   // 단수형: "crontab"       -> kubectl get crontab
    ShortNames []string // 약칭: ["ct"]            -> kubectl get ct
    Kind       string   // Kind: "CronTab"          -> YAML의 kind 필드
    ListKind   string   // ListKind: "CronTabList"
    Categories []string // 카테고리: ["all"]        -> kubectl get all
}
```

### CRD 이름 규칙

CRD의 `metadata.name`은 반드시 `<names.plural>.<group>` 형식이어야 한다.

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: crontabs.stable.example.com    # <-- plural.group
spec:
  group: stable.example.com            # <-- group
  names:
    plural: crontabs                    # <-- plural
    singular: crontab
    shortNames: ["ct"]
    kind: CronTab
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                cronSpec:
                  type: string
                replicas:
                  type: integer
```

### ResourceScope

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 281-287)

```go
// staging/.../apiextensions/v1/types.go:284
const (
    ClusterScoped   ResourceScope = "Cluster"
    NamespaceScoped ResourceScope = "Namespaced"
)
```

---

## 3. CRD 라이프사이클

### CRD 버전 정의

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 169-211)

```go
// staging/.../apiextensions/v1/types.go:170
type CustomResourceDefinitionVersion struct {
    Name                    string                           // "v1", "v1beta1"
    Served                  bool                             // API로 제공 여부
    Storage                 bool                             // etcd 저장 버전 (정확히 1개만 true)
    Deprecated              bool                             // 지원 중단 표시
    DeprecationWarning      *string                          // 경고 메시지
    Schema                  *CustomResourceValidation        // OpenAPI 스키마
    Subresources            *CustomResourceSubresources      // /status, /scale
    AdditionalPrinterColumns []CustomResourceColumnDefinition // kubectl 출력 열
    SelectableFields        []SelectableField                // 필드 셀렉터
}
```

핵심 규칙:
- `Served: true` - 해당 버전의 API 엔드포인트가 활성화됨
- `Storage: true` - etcd에 이 버전으로 저장됨 (전체 버전 중 정확히 1개)
- 여러 버전이 동시에 `Served: true`일 수 있다

### CRD Condition (상태 조건)

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 301-335)

```go
// staging/.../apiextensions/v1/types.go:304
const (
    Established          CustomResourceDefinitionConditionType = "Established"
    NamesAccepted        CustomResourceDefinitionConditionType = "NamesAccepted"
    NonStructuralSchema  CustomResourceDefinitionConditionType = "NonStructuralSchema"
    Terminating          CustomResourceDefinitionConditionType = "Terminating"
    KubernetesAPIApprovalPolicyConformant CustomResourceDefinitionConditionType = "KubernetesAPIApprovalPolicyConformant"
)
```

| Condition | 의미 |
|-----------|------|
| `Established` | CRD가 활성화되어 CR을 생성/조회 가능 |
| `NamesAccepted` | CRD 이름이 다른 리소스와 충돌하지 않음 |
| `NonStructuralSchema` | 비구조적 스키마 경고 |
| `Terminating` | CRD가 삭제 중 (CR 정리 중) |
| `KubernetesAPIApprovalPolicyConformant` | *.k8s.io 네임스페이스 승인 여부 |

### CRD Status

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 362-388)

```go
// staging/.../apiextensions/v1/types.go:362
type CustomResourceDefinitionStatus struct {
    Conditions     []CustomResourceDefinitionCondition
    AcceptedNames  CustomResourceDefinitionNames  // 실제 사용 중인 이름
    StoredVersions []string                       // etcd에 저장된 적 있는 모든 버전
    ObservedGeneration int64                      // CRD 컨트롤러가 관찰한 세대
}
```

`StoredVersions`가 중요한 이유:
- etcd에 저장된 적이 있는 모든 버전을 추적한다
- 마이그레이션 후 이전 버전의 오브젝트가 모두 변환되면, 해당 버전을 `StoredVersions`에서 제거할 수 있다
- `spec.versions`에서 제거하려면 먼저 `StoredVersions`에서 제거되어야 한다

### CRD 생성에서 사용까지의 전체 흐름

```
1. CRD 생성
   kubectl apply -f crontab-crd.yaml
          |
          v
2. API Server가 CRD 오브젝트를 etcd에 저장
          |
          v
3. CRD Controller들이 CRD를 처리
   - Naming Controller: 이름 충돌 확인 → NamesAccepted
   - Establishing Controller: API 엔드포인트 활성화 → Established
   - OpenAPI Controller: OpenAPI 스펙 업데이트
          |
          v
4. crdHandler가 동적 REST 스토리지 생성
   - /apis/<group>/<version>/<plural> 엔드포인트 활성화
   - watch/list 지원
          |
          v
5. CR 생성 가능
   kubectl apply -f my-crontab.yaml
          |
          v
6. crdHandler → OpenAPI 검증 → etcd 저장
          |
          v
7. CR 조회 가능
   kubectl get crontabs
```

### CRD Finalizer

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 392)

```go
const CustomResourceCleanupFinalizer = "customresourcecleanup.apiextensions.k8s.io"
```

CRD를 삭제하면:
1. Finalizer가 존재하므로 즉시 삭제되지 않음
2. `Terminating` 조건이 설정됨
3. 해당 CRD의 모든 CR이 삭제됨
4. Finalizer 컨트롤러가 정리 완료 후 Finalizer 제거
5. CRD 오브젝트 최종 삭제

---

## 4. API Aggregation

### APIService 구조체

API Aggregation은 외부 API 서버를 kube-apiserver에 등록하여, 마치 네이티브 API처럼 사용할 수 있게 하는 메커니즘이다.

**소스 위치**: `staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/types.go` (라인 153-164)

```go
// staging/.../apiregistration/v1/types.go:153
type APIService struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   APIServiceSpec   `json:"spec,omitempty"`
    Status APIServiceStatus `json:"status,omitempty"`
}
```

### APIServiceSpec

**소스 위치**: `staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/types.go` (라인 49-94)

```go
// staging/.../apiregistration/v1/types.go:49
type APIServiceSpec struct {
    Service               *ServiceReference // 백엔드 서비스 참조 (nil이면 로컬 처리)
    Group                 string            // API 그룹 ("metrics.k8s.io")
    Version               string            // API 버전 ("v1beta1")
    InsecureSkipTLSVerify bool              // TLS 검증 건너뛰기 (비권장)
    CABundle              []byte            // CA 인증서 번들
    GroupPriorityMinimum  int32             // 그룹 우선순위 (높을수록 우선)
    VersionPriority       int32             // 버전 우선순위
}
```

### Service가 nil인 경우

```go
// staging/.../apiregistration/v1/types.go:52-56
// Service is a reference to the service for this API server.
// It must communicate on port 443.
// If the Service is nil, that means the handling for the API
// groupversion is handled locally on this server.
// The call will simply delegate to the normal handler chain.
```

Service가 nil이면 로컬 kube-apiserver가 해당 API를 직접 처리한다. 이는 Kubernetes의 빌트인 API가 등록되는 방식이다.

### APIService 이름 규칙

APIService의 이름은 `<version>.<group>` 형식이다.

```yaml
apiVersion: apiregistration.k8s.io/v1
kind: APIService
metadata:
  name: v1beta1.metrics.k8s.io    # <-- version.group
spec:
  group: metrics.k8s.io
  version: v1beta1
  service:
    name: metrics-server
    namespace: kube-system
  groupPriorityMinimum: 100
  versionPriority: 100
  caBundle: <base64-encoded-ca>
```

### APIServiceCondition

**소스 위치**: `staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/types.go` (라인 109-143)

```go
// staging/.../apiregistration/v1/types.go:112
const (
    Available APIServiceConditionType = "Available"
)

// staging/.../apiregistration/v1/types.go:118
type APIServiceCondition struct {
    Type               APIServiceConditionType
    Status             ConditionStatus          // True, False, Unknown
    LastTransitionTime metav1.Time
    Reason             string
    Message            string
}
```

APIService는 `Available` 조건 하나만 존재한다. 이는 백엔드 서비스가 도달 가능한지를 나타낸다.

### CRD vs API Aggregation 비교

```
+---CRD 방식--------------------------------------------+
|                                                        |
|  Client --> kube-apiserver --> apiextensions-apiserver  |
|                                      |                 |
|                                   etcd 직접 저장       |
|                                                        |
|  장점: 단순, etcd 활용, webhook으로 커스터마이징 가능   |
|  단점: 비즈니스 로직 제한, etcd 스키마 제약             |
+--------------------------------------------------------+

+---API Aggregation 방식---------------------------------+
|                                                        |
|  Client --> kube-aggregator --(프록시)--> 외부 API 서버  |
|                                              |          |
|                                        자체 스토리지    |
|                                                        |
|  장점: 완전한 제어, 자체 스토리지, 고급 API 로직       |
|  단점: 운영 복잡도, 자체 서버 관리 필요                |
+--------------------------------------------------------+
```

| 기준 | CRD | API Aggregation |
|------|-----|----------------|
| 배포 복잡도 | 낮음 (CRD YAML만 적용) | 높음 (별도 서버 필요) |
| 스토리지 | etcd (kube-apiserver와 공유) | 자체 선택 가능 |
| 인증/인가 | kube-apiserver RBAC | 자체 또는 프록시 |
| Validation | OpenAPI, CEL, Webhook | 자체 구현 |
| 서브리소스 | /status, /scale | 자유 정의 |
| 긴 실행 API | 불가 | 가능 |
| 프로토콜 | REST (JSON/protobuf) | 자유 선택 |

---

## 5. Server Chain 아키텍처

### 삼중 서버 체인

Kubernetes의 kube-apiserver는 실제로 세 개의 서버가 체인으로 연결된 구조이다.

```
+------------------------------------------------------------------+
|                     kube-apiserver 프로세스                        |
|                                                                  |
|  +---Aggregator Server (kube-aggregator)--------------------+    |
|  |                                                          |    |
|  |  요청 수신 → APIService 조회                              |    |
|  |                                                          |    |
|  |  Service != nil?                                         |    |
|  |    Yes → 외부 API 서버로 프록시                           |    |
|  |    No  → 다음 서버로 위임 (delegate)                      |    |
|  |                |                                         |    |
|  +----------------|-----------------------------------------+    |
|                   v                                              |
|  +---KubeAPI Server-----------------------------------------+    |
|  |                                                          |    |
|  |  빌트인 리소스 처리                                       |    |
|  |  (pods, services, deployments, etc.)                      |    |
|  |                                                          |    |
|  |  처리 불가?                                               |    |
|  |    → 다음 서버로 위임 (delegate)                          |    |
|  |                |                                         |    |
|  +----------------|-----------------------------------------+    |
|                   v                                              |
|  +---apiextensions-apiserver (Extensions)--------------------+   |
|  |                                                          |    |
|  |  CRD 기반 커스텀 리소스 처리                              |    |
|  |  (crontabs.stable.example.com, etc.)                      |    |
|  |                                                          |    |
|  |  처리 불가?                                               |    |
|  |    → 404 Not Found                                        |    |
|  +-----------------------------------------------------------+   |
+------------------------------------------------------------------+
```

### 요청 라우팅 흐름

```
Client 요청: GET /apis/stable.example.com/v1/crontabs
         |
         v
1. Aggregator Server 수신
   - APIService "v1.stable.example.com" 조회
   - Service 필드가 nil → 로컬 처리
   - 다음 서버로 위임
         |
         v
2. KubeAPI Server
   - 빌트인 리소스가 아님
   - 다음 서버로 위임
         |
         v
3. apiextensions-apiserver
   - CRD "crontabs.stable.example.com" 조회
   - crdHandler가 동적 스토리지에서 처리
   - 응답 반환
         |
         v
Client 응답: CronTab 목록
```

```
Client 요청: GET /apis/metrics.k8s.io/v1beta1/nodes
         |
         v
1. Aggregator Server 수신
   - APIService "v1beta1.metrics.k8s.io" 조회
   - Service = {name: "metrics-server", namespace: "kube-system"}
   - 외부 서버로 프록시
         |
         v
2. metrics-server Pod
   - 요청 처리 후 응답 반환
         |
         v
Client 응답: NodeMetrics 목록
```

---

## 6. CRD Validation

### OpenAPI v3 Schema

모든 CRD 버전은 OpenAPI v3 스키마를 가져야 한다. 이 스키마는 CR의 구조를 정의하고, 유효성 검증에 사용된다.

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 431-436)

```go
// staging/.../apiextensions/v1/types.go:431
type CustomResourceValidation struct {
    OpenAPIV3Schema *JSONSchemaProps `json:"openAPIV3Schema,omitempty"`
}
```

### 스키마 예시

```yaml
schema:
  openAPIV3Schema:
    type: object
    required: ["spec"]
    properties:
      spec:
        type: object
        required: ["cronSpec", "image"]
        properties:
          cronSpec:
            type: string
            pattern: '^(\d+|\*)(/\d+)?(\s+(\d+|\*)(/\d+)?){4}$'
          image:
            type: string
          replicas:
            type: integer
            minimum: 1
            maximum: 10
            default: 1
      status:
        type: object
        properties:
          replicas:
            type: integer
          lastScheduleTime:
            type: string
            format: date-time
```

### 구조적 스키마 (Structural Schema) 요구사항

`NonStructuralSchema` 조건이 존재하는 이유는 스키마가 **구조적(structural)**이어야 하기 때문이다.

구조적 스키마의 조건:
1. 모든 값에 대해 타입이 지정되어야 함
2. `x-kubernetes-int-or-string: true` 또는 `x-kubernetes-preserve-unknown-fields: true` 예외
3. `allOf`, `anyOf`, `oneOf`, `not` 하위에서 type, additionalProperties, default 등을 사용할 수 없음

비구조적 스키마의 제약:
- Pruning (미지 필드 자동 제거) 불가
- Defaulting (기본값 자동 설정) 불가
- OpenAPI 스펙에 게시 불가
- Webhook 변환 불가

### CEL (Common Expression Language) Validation

Kubernetes 1.25+에서 CRD에 CEL 기반 유효성 검증을 추가할 수 있다.

```yaml
schema:
  openAPIV3Schema:
    type: object
    properties:
      spec:
        type: object
        properties:
          minReplicas:
            type: integer
          maxReplicas:
            type: integer
        x-kubernetes-validations:
          - rule: "self.minReplicas <= self.maxReplicas"
            message: "minReplicas must be <= maxReplicas"
          - rule: "self.maxReplicas <= 100"
            message: "maxReplicas cannot exceed 100"
```

CEL의 장점:
- Webhook 서버 없이 인라인 검증 가능
- 낮은 지연 시간 (Go 프로세스 내 실행)
- 표현력 있는 규칙 정의 (비교, 리스트 연산, 문자열 패턴 등)

### Validation 아키텍처

```
CR 생성/수정 요청
      |
      v
+--OpenAPI Schema Validation--+
|  - 타입 검사                 |
|  - required 필드             |
|  - minimum/maximum           |
|  - pattern                   |
|  - enum                      |
+-----------+-----------------+
            |
            v
+--CEL Validation--------------+
|  - x-kubernetes-validations  |
|  - 크로스 필드 검증          |
|  - 조건부 검증               |
+-----------+-----------------+
            |
            v
+--Validating Webhook----------+
|  (선택 사항)                 |
|  - 외부 서버 호출            |
|  - 복잡한 비즈니스 로직      |
+-----------+-----------------+
            |
            v
      etcd 저장
```

---

## 7. CRD Versioning과 Conversion Webhook

### 다중 버전 지원

하나의 CRD에 여러 버전을 동시에 제공할 수 있다.

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: crontabs.stable.example.com
spec:
  group: stable.example.com
  versions:
    - name: v1
      served: true
      storage: true        # v1으로 저장
      schema: { ... }
    - name: v2
      served: true
      storage: false        # v2로 서빙만
      schema: { ... }
  conversion:
    strategy: Webhook
    webhook:
      clientConfig:
        service:
          name: crontab-conversion
          namespace: default
          path: /convert
        caBundle: <base64>
      conversionReviewVersions: ["v1"]
```

### ConversionStrategy

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 25-38)

```go
// staging/.../apiextensions/v1/types.go:26
type ConversionStrategyType string

const (
    NoneConverter    ConversionStrategyType = "None"     // apiVersion만 변경
    WebhookConverter ConversionStrategyType = "Webhook"  // 외부 webhook 호출
)
```

### CustomResourceConversion

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 75-86)

```go
// staging/.../apiextensions/v1/types.go:76
type CustomResourceConversion struct {
    Strategy ConversionStrategyType    // "None" 또는 "Webhook"
    Webhook  *WebhookConversion        // Strategy가 "Webhook"일 때 필수
}
```

### WebhookConversion

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 88-102)

```go
// staging/.../apiextensions/v1/types.go:89
type WebhookConversion struct {
    ClientConfig              *WebhookClientConfig
    ConversionReviewVersions  []string  // 지원하는 ConversionReview 버전
}
```

### WebhookClientConfig

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 104-147)

```go
// staging/.../apiextensions/v1/types.go:105
type WebhookClientConfig struct {
    URL      *string            // 외부 URL (https://...)
    Service  *ServiceReference  // 클러스터 내부 서비스
    CABundle []byte             // CA 인증서
}
```

정확히 `URL` 또는 `Service` 중 하나만 설정해야 한다.

### 변환 흐름

```
Client: GET /apis/stable.example.com/v2/crontabs/my-crontab
                    |
                    v
         apiextensions-apiserver
                    |
                    v
         etcd에서 v1 형식으로 조회
         (storage: true인 버전)
                    |
                    v
         v1 → v2 변환 필요
                    |
         +----------+----------+
         |                     |
    None Strategy         Webhook Strategy
         |                     |
    apiVersion만 변경     Webhook 서버 호출
         |                     |
         |              ConversionReview 전송
         |              { objects: [...], desiredAPIVersion: "v2" }
         |                     |
         |              변환된 objects 수신
         |                     |
         +----------+----------+
                    |
                    v
         v2 형식으로 응답
```

### 저장 버전 마이그레이션

```
1단계: CRD에 v1(storage), v2(served) 추가
2단계: 모든 CR을 읽어서 다시 저장 (v1 → v1, 자동으로 최신 스키마 적용)
3단계: storage를 v2로 변경
4단계: 모든 CR을 읽어서 다시 저장 (이제 v2로 저장)
5단계: storedVersions에서 v1 제거
6단계: (선택) served에서 v1 제거
```

---

## 8. apiextensions-apiserver 내부 구조

### 서버 구조

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/apiserver.go` (라인 79-112)

```go
// staging/.../apiserver/apiserver.go:79
type ExtraConfig struct {
    CRDRESTOptionsGetter genericregistry.RESTOptionsGetter
    MasterCount          int
    ServiceResolver      webhook.ServiceResolver
    AuthResolverWrapper  webhook.AuthenticationInfoResolverWrapper
}

// staging/.../apiserver/apiserver.go:107
type CustomResourceDefinitions struct {
    GenericAPIServer *genericapiserver.GenericAPIServer
    Informers        externalinformers.SharedInformerFactory
}
```

### 핵심 컨트롤러 목록

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/apiserver.go` (라인 19-51의 import)

```go
import (
    "k8s.io/apiextensions-apiserver/pkg/controller/apiapproval"          // API 승인 컨트롤러
    "k8s.io/apiextensions-apiserver/pkg/controller/establish"            // CRD 설정 컨트롤러
    "k8s.io/apiextensions-apiserver/pkg/controller/finalizer"            // 정리 컨트롤러
    "k8s.io/apiextensions-apiserver/pkg/controller/nonstructuralschema"  // 스키마 검증 컨트롤러
    openapicontroller "k8s.io/apiextensions-apiserver/pkg/controller/openapi"    // OpenAPI v2
    openapiv3controller "k8s.io/apiextensions-apiserver/pkg/controller/openapiv3" // OpenAPI v3
    "k8s.io/apiextensions-apiserver/pkg/controller/status"              // 상태 컨트롤러
)
```

### crdHandler - 동적 REST 핸들러

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/customresource_handler.go` (라인 89-100)

```go
// staging/.../apiserver/customresource_handler.go:91
type crdHandler struct {
    versionDiscoveryHandler *versionDiscoveryHandler
    groupDiscoveryHandler   *groupDiscoveryHandler
    customStorageLock       sync.Mutex
    customStorage           atomic.Value     // crdStorageMap (원자적 읽기 최적화)
    // ...
}
```

`customStorage`가 `atomic.Value`를 사용하는 이유:

```go
// staging/.../apiserver/customresource_handler.go:96-99
// atomic.Value has a very good read performance compared to sync.RWMutex
// see https://gist.github.com/dim/152e6bf80e1384ea72e17ac717a5000a
// which is suited for most read and rarely write cases
```

읽기가 대부분이고 쓰기가 드문 경우(CRD 생성/삭제는 드물고, CR 조회는 빈번), `atomic.Value`가 `sync.RWMutex`보다 훨씬 빠르다.

### 관련 패키지 구조

```
staging/src/k8s.io/apiextensions-apiserver/
├── pkg/
│   ├── apis/apiextensions/
│   │   ├── types.go              # 내부 타입 정의
│   │   ├── v1/types.go           # v1 외부 타입
│   │   └── v1beta1/types.go      # v1beta1 외부 타입 (deprecated)
│   │
│   ├── apiserver/
│   │   ├── apiserver.go          # 서버 초기화
│   │   ├── customresource_handler.go  # CR 요청 처리 핸들러
│   │   ├── customresource_discovery.go # 디스커버리
│   │   ├── customresource_discovery_controller.go # 디스커버리 컨트롤러
│   │   ├── conversion/           # 버전 변환 로직
│   │   ├── schema/               # 스키마 관련
│   │   │   ├── defaulting/       # 기본값 설정
│   │   │   ├── objectmeta/       # 메타데이터 처리
│   │   │   └── pruning/          # 미지 필드 제거
│   │   └── validation/           # 유효성 검증
│   │
│   ├── controller/
│   │   ├── apiapproval/          # API 승인 검증
│   │   ├── establish/            # CRD 활성화
│   │   ├── finalizer/            # CRD 삭제 시 정리
│   │   ├── nonstructuralschema/  # 비구조적 스키마 감지
│   │   ├── openapi/              # OpenAPI v2 게시
│   │   ├── openapiv3/            # OpenAPI v3 게시
│   │   └── status/               # 상태 조건 업데이트
│   │
│   └── registry/
│       ├── customresourcedefinition/
│       │   ├── etcd.go           # CRD etcd 스토리지
│       │   └── strategy.go       # CRD CRUD 전략
│       └── customresource/
│           └── tableconvertor/   # kubectl 테이블 변환
```

---

## 9. kube-aggregator 내부 구조

### Aggregator 서버

**소스 위치**: `staging/src/k8s.io/kube-aggregator/pkg/apiserver/apiserver.go` (라인 79-116)

```go
// staging/.../kube-aggregator/pkg/apiserver/apiserver.go:86
type ExtraConfig struct {
    PeerAdvertiseAddress  peerreconcilers.PeerAdvertiseAddress
    ProxyClientCertFile   string
    ProxyClientKeyFile    string
    ProxyTransport        *http.Transport
    ServiceResolver       ServiceResolver
    RejectForwardingRedirects bool
    DisableRemoteAvailableConditionController bool
    PeerProxy             utilpeerproxy.Interface
}
```

### proxyHandler - 프록시 핸들러

**소스 위치**: `staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go` (라인 49-69)

```go
// staging/.../kube-aggregator/pkg/apiserver/handler_proxy.go:51
type proxyHandler struct {
    localDelegate             http.Handler           // 로컬 API 위임
    proxyCurrentCertKeyContent certKeyFunc            // 프록시 인증서
    proxyTransportDial        *transport.DialHolder  // 전송 다이얼러
    serviceResolver           ServiceResolver        // 서비스 -> IP 변환
    handlingInfo              atomic.Value            // proxyHandlingInfo
    rejectForwardingRedirects bool
    tracerProvider            tracing.TracerProvider
}
```

### proxyHandlingInfo

**소스 위치**: `staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go` (라인 71-80)

```go
// staging/.../kube-aggregator/pkg/apiserver/handler_proxy.go:71
type proxyHandlingInfo struct {
    local           bool              // 로컬 처리 여부
    name            string            // APIService 이름
    transportConfig *transport.Config // 라운드트리퍼 설정
    // ...
}
```

`local: true`이면 요청을 프록시하지 않고 `localDelegate`로 위임한다.

### 프록시 메커니즘

```
Client → Aggregator
              |
              v
      proxyHandler.ServeHTTP()
              |
              +-- handlingInfo.local?
              |     |
              |     +-- true: localDelegate.ServeHTTP()
              |     |         (KubeAPI → Extensions 체인)
              |     |
              |     +-- false: 외부 프록시
              |                |
              |                +-- ServiceResolver로 서비스 IP 확인
              |                +-- TLS 설정 (CABundle, ClientCert)
              |                +-- http.ReverseProxy로 전달
              |                +-- X-Forwarded-* 헤더 추가
              |                +-- 응답 반환
              |
              v
         Client 응답
```

### Aggregator 디렉토리 구조

```
staging/src/k8s.io/kube-aggregator/
├── pkg/
│   ├── apis/apiregistration/
│   │   ├── types.go              # 내부 타입
│   │   ├── v1/
│   │   │   ├── types.go          # APIService v1 타입
│   │   │   └── helper/           # 헬퍼 유틸리티
│   │   └── v1beta1/types.go      # v1beta1 타입
│   │
│   ├── apiserver/
│   │   ├── apiserver.go          # Aggregator 서버 초기화
│   │   ├── handler_proxy.go      # 프록시 핸들러 (핵심)
│   │   ├── handler_apis.go       # /apis 엔드포인트
│   │   ├── handler_discovery.go  # API 디스커버리
│   │   ├── apiservice_controller.go  # APIService 컨트롤러
│   │   ├── resolvers.go          # 서비스 리졸버
│   │   └── metrics.go            # 메트릭
│   │
│   ├── controllers/
│   │   ├── status/
│   │   │   ├── local/            # 로컬 APIService 가용성
│   │   │   └── remote/           # 원격 APIService 가용성
│   │   ├── openapi/              # OpenAPI 집계
│   │   └── openapiv3/            # OpenAPI v3 집계
│   │
│   └── registry/
│       └── apiservice/
│           └── rest/             # APIService CRUD
```

---

## 10. CRD 컨트롤러들

### Establishing Controller

CRD가 생성되면 Establishing Controller가 해당 CRD의 API 엔드포인트를 활성화한다.

```
CRD 생성
    |
    v
Establishing Controller
    |
    +-- 이름 충돌 확인
    +-- HA 환경에서 5초 대기 (MasterCount > 1)
    +-- Established 조건 설정
    |
    v
API 엔드포인트 활성화
/apis/<group>/<version>/<plural> 사용 가능
```

소스코드에서 HA 대기 로직을 확인할 수 있다:

```go
// staging/.../apiserver/apiserver.go:83
type ExtraConfig struct {
    // MasterCount is used to detect whether cluster is HA, and if it is
    // the CRD Establishing will be hold by 5 seconds.
    MasterCount int
}
```

### Naming Controller

이름 충돌을 감지하고 `NamesAccepted` 조건을 관리한다.

```
CRD 이름 검증 프로세스:
1. plural 이름이 다른 CRD와 충돌하는가?
2. singular 이름이 충돌하는가?
3. shortNames가 충돌하는가?
4. kind가 충돌하는가?

충돌 없음 → NamesAccepted: True
충돌 있음 → NamesAccepted: False, message에 충돌 상세 기록
```

### Finalizer Controller

CRD 삭제 시 관련 CR을 정리하는 역할이다.

```
CRD 삭제 요청
    |
    v
Finalizer Controller
    |
    +-- CustomResourceCleanupFinalizer 확인
    +-- Terminating 조건 설정
    +-- 해당 CRD의 모든 CR 삭제 시작
    |     |
    |     +-- 네임스페이스별 순회
    |     +-- CR 삭제 (DeleteCollection)
    |
    +-- 모든 CR 삭제 완료?
    |     |
    |     +-- Yes: Finalizer 제거 → CRD 최종 삭제
    |     +-- No: 재시도
```

### OpenAPI Controller

CRD의 스키마를 OpenAPI 스펙에 게시하여, `kubectl explain`과 클라이언트 라이브러리에서 사용할 수 있게 한다.

```
CRD 생성/수정 이벤트
    |
    v
OpenAPI v2 Controller     OpenAPI v3 Controller
    |                          |
    +-- CRD 스키마를           +-- CRD 스키마를
    |   OpenAPI v2로 변환      |   OpenAPI v3으로 변환
    |                          |
    +-- /openapi/v2에          +-- /openapi/v3에
        게시                       게시
```

### NonStructuralSchema Controller

CRD의 스키마가 구조적인지 검증하고, `NonStructuralSchema` 조건을 관리한다.

### Status Controller

CRD의 전반적인 상태를 관리한다.

---

## 11. CRD Subresources

### Subresources 정의

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 438-449)

```go
// staging/.../apiextensions/v1/types.go:439
type CustomResourceSubresources struct {
    Status *CustomResourceSubresourceStatus  // /status 서브리소스
    Scale  *CustomResourceSubresourceScale   // /scale 서브리소스
}
```

### /status 서브리소스

`/status` 서브리소스를 활성화하면:
1. 기본 엔드포인트(`/apis/<group>/<version>/<plural>/<name>`)에서 `status` 필드 변경이 무시됨
2. `/status` 엔드포인트에서 `status` 이외의 필드 변경이 무시됨
3. 이를 통해 "사용자는 spec을, 컨트롤러는 status를" 패턴 구현

```yaml
subresources:
  status: {}    # /status 활성화
```

### /scale 서브리소스

`/scale` 서브리소스를 활성화하면 HPA(Horizontal Pod Autoscaler)가 CR의 replicas를 자동 조절할 수 있다.

```yaml
subresources:
  scale:
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .status.labelSelector
```

### AdditionalPrinterColumns

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 226-249)

```go
// staging/.../apiextensions/v1/types.go:227
type CustomResourceColumnDefinition struct {
    Name        string // 열 이름
    Type        string // OpenAPI 타입 (string, integer, date 등)
    Format      string // 형식 (name, date-time 등)
    Description string // 설명
    Priority    int32  // 우선순위 (0이 높음)
    JSONPath    string // JSON 경로 (.spec.cronSpec)
}
```

```yaml
additionalPrinterColumns:
  - name: Schedule
    type: string
    jsonPath: .spec.cronSpec
  - name: Replicas
    type: integer
    jsonPath: .spec.replicas
  - name: Age
    type: date
    jsonPath: .metadata.creationTimestamp
```

결과:
```
NAME        SCHEDULE      REPLICAS   AGE
my-crontab  */5 * * * *   3          5m
```

### SelectableFields

**소스 위치**: `staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go` (라인 213-224)

```go
// staging/.../apiextensions/v1/types.go:214
type SelectableField struct {
    JSONPath string  // 필드 셀렉터로 사용할 JSON 경로
}
```

```yaml
selectableFields:
  - jsonPath: .spec.color
  - jsonPath: .spec.size
```

이렇게 하면 `kubectl get crontabs --field-selector spec.color=blue` 같은 필드 셀렉터를 사용할 수 있다. 최대 8개까지 지정 가능하다.

---

## 12. 설계 원칙: Why

### Why: CRD가 필요한 이유

1. **재컴파일 없는 확장**: Kubernetes 소스코드를 수정하지 않고 새 API 타입 추가
2. **생태계 성장**: Istio(VirtualService), Argo(Workflow), Cert-Manager(Certificate) 등이 CRD로 구현
3. **표준화된 API 경험**: `kubectl`, RBAC, Watch, OpenAPI 등 Kubernetes의 모든 인프라를 자동으로 활용
4. **선언적 관리**: Operator 패턴으로 복잡한 애플리케이션을 선언적으로 관리

### Why: 양방향 확장 (CRD + Aggregation)이 모두 필요한 이유

```
                     유연성 높음
                         ^
                         |
    API Aggregation      |  완전한 제어
    (metrics-server,     |  자체 스토리지
     custom-apiserver)   |  자체 인증
                         |
    - - - - - - - - - - -|- - - - - - -
                         |
    CRD + Webhook        |  충분한 유연성
    (Istio, Argo,        |  Webhook으로 커스터마이징
     Cert-Manager)       |
                         |
    CRD (기본)           |  단순함
    (간단한 CRUD)        |  표준 etcd 저장
                         |
    ------+--------------+------------>
       간단함                     복잡함
```

CRD는 대부분의 사용 사례를 커버하지만, 다음 경우에는 Aggregation이 필요하다:
- 자체 스토리지 필요 (etcd 외)
- 고급 API 로직 (WebSocket, 스트리밍)
- 기존 시스템의 API를 Kubernetes로 노출

### Why: Server Chain 구조를 선택한 이유

```go
// staging/.../kube-aggregator/pkg/apiserver/apiserver.go:79
// legacyAPIServiceName = "v1."
```

1. **하위 호환성**: 기존 빌트인 API를 깨뜨리지 않으면서 확장 가능
2. **관심사 분리**: 각 서버가 자신의 영역만 처리
3. **독립적 발전**: apiextensions-apiserver는 별도 모듈로 발전 가능
4. **우선순위 제어**: Aggregator → KubeAPI → Extensions 순서로, 빌트인 API가 항상 우선

### Why: atomic.Value를 사용하는 이유

crdHandler의 customStorage는 `sync.RWMutex` 대신 `atomic.Value`를 사용한다.

```go
// staging/.../apiserver/customresource_handler.go:96-99
// atomic.Value has a very good read performance compared to sync.RWMutex
// which is suited for most read and rarely write cases
```

이유:
- CR 조회(읽기)는 매우 빈번하다 (모든 kubectl get, list, watch)
- CRD 생성/삭제(쓰기)는 매우 드물다
- `atomic.Value`는 읽기에서 잠금이 없어 성능이 월등하다
- 쓰기 시 전체 맵을 교체하는 방식으로 동시성 안전 보장

### Why: 구조적 스키마가 강제되는 이유

비구조적 스키마에서는 다음이 불가능하다:
1. **Pruning**: 미지 필드를 자동으로 제거하여 스키마 진화 시 안전성 보장
2. **Defaulting**: 기본값 자동 설정으로 사용자 편의성 증대
3. **OpenAPI 게시**: 정확한 타입 정보 없이는 클라이언트 생성 불가
4. **Webhook 변환**: 타입을 모르면 안전한 변환이 불가능

### Why: Finalizer로 CRD 삭제를 보호하는 이유

```go
const CustomResourceCleanupFinalizer = "customresourcecleanup.apiextensions.k8s.io"
```

CRD를 삭제하면 해당 CRD의 모든 CR도 삭제되어야 한다. Finalizer 없이 CRD를 즉시 삭제하면:
1. CR들이 etcd에 남아 "고아 데이터"가 됨
2. 해당 API가 갑자기 사라져 기존 컨트롤러들이 오류 발생
3. etcd에 접근 불가능한 데이터가 쌓임

Finalizer를 통해 "CRD 삭제 전에 CR 정리" 순서를 보장한다.

### Why: HA 환경에서 5초 대기하는 이유

```go
// MasterCount is used to detect whether cluster is HA, and if it is
// the CRD Establishing will be hold by 5 seconds.
MasterCount int
```

HA 환경에서 여러 kube-apiserver 인스턴스가 동시에 CRD를 처리할 수 있다. 5초 대기는:
1. 모든 인스턴스가 CRD를 인지할 시간을 확보
2. 레이스 컨디션으로 인한 일시적 404 방지
3. 클라이언트에게 일관된 API 가용성 보장

---

## 요약

Kubernetes API 확장 시스템은 세 가지 핵심 컴포넌트로 구성된다:

```
+---kube-apiserver (단일 프로세스)-----------------------------------+
|                                                                    |
|  1. kube-aggregator                                                |
|     - APIService 관리                                              |
|     - 외부 API 서버로 프록시                                        |
|     - 빌트인 API는 로컬 위임                                       |
|                                                                    |
|  2. kube-apiserver (core)                                          |
|     - Pod, Service, Deployment 등 빌트인 리소스                     |
|     - RBAC, Admission, etcd 접근                                   |
|                                                                    |
|  3. apiextensions-apiserver                                        |
|     - CRD 관리 (생성, 삭제, 상태)                                   |
|     - CR 동적 스토리지                                              |
|     - OpenAPI 스키마 검증                                           |
|     - 버전 변환 (None/Webhook)                                     |
|                                                                    |
+--------------------------------------------------------------------+
```

이 설계를 통해 Kubernetes는:
- **재컴파일 없이** 무한히 확장 가능한 API 플랫폼이 되었다
- CRD로 대부분의 커스텀 리소스를 지원하고
- API Aggregation으로 고급 사용 사례를 커버한다
- Server Chain으로 하위 호환성과 확장성을 동시에 달성한다
