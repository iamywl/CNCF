# 14. 어드미션 컨트롤 심화 (Admission Control Deep-Dive)

## 목차

1. [개요](#1-개요)
2. [어드미션 컨트롤이란](#2-어드미션-컨트롤이란)
3. [Admission 인터페이스 계층 구조](#3-admission-인터페이스-계층-구조)
4. [Admission Attributes](#4-admission-attributes)
5. [Admission Chain: Admit과 Validate](#5-admission-chain-admit과-validate)
6. [내장 어드미션 플러그인](#6-내장-어드미션-플러그인)
7. [createHandler에서의 호출 흐름](#7-createhandler에서의-호출-흐름)
8. [Webhook 어드미션](#8-webhook-어드미션)
9. [ValidatingAdmissionPolicy (CEL)](#9-validatingadmissionpolicy-cel)
10. [Operation 타입](#10-operation-타입)
11. [플러그인 초기화](#11-플러그인-초기화)
12. [ReinvocationContext](#12-reinvocationcontext)
13. [DryRun 처리](#13-dryrun-처리)
14. [설계 원칙: Why](#14-설계-원칙-why)
15. [정리](#15-정리)

---

## 1. 개요

어드미션 컨트롤(Admission Control)은 인증(Authentication)과 인가(Authorization)를
통과한 API 요청이 **etcd에 저장되기 직전**에 실행되는 마지막 관문이다.
요청 오브젝트를 변형(mutate)하거나 유효성 검증(validate)하여
클러스터 정책을 강제할 수 있다.

```
클라이언트 → 인증 → 인가 → 어드미션 컨트롤 → etcd 저장
                           |
                           +-- Mutating Phase (변형)
                           |     순차 실행, 오브젝트 수정 가능
                           |
                           +-- Validating Phase (검증)
                                 순차 실행, 오브젝트 수정 불가
```

**핵심 소스 경로:**

| 구성요소 | 소스 경로 |
|---------|----------|
| Admission 인터페이스 | `staging/src/k8s.io/apiserver/pkg/admission/interfaces.go` |
| Admission Chain | `staging/src/k8s.io/apiserver/pkg/admission/chain.go` |
| 내장 플러그인 디렉토리 | `plugin/pkg/admission/` |
| Create Handler | `staging/src/k8s.io/apiserver/pkg/endpoints/handlers/create.go` |

---

## 2. 어드미션 컨트롤이란

### 2.1 API 요청 처리 파이프라인에서의 위치

```
HTTP 요청
  |
  v
[1] 인증 (Authentication)
  |     "누구인가?" → user.Info
  v
[2] 인가 (Authorization)
  |     "권한이 있는가?" → Allow/Deny
  v
[3] 어드미션 컨트롤 (Admission Control)     ← 이 문서의 주제
  |     "이 요청을 허용할 것인가?"
  |     "오브젝트를 수정할 것인가?"
  v
[4] 유효성 검증 (Validation)
  |     스키마 레벨 검증
  v
[5] etcd 저장
```

### 2.2 어드미션 vs 인가

| 구분 | 인가 (Authorization) | 어드미션 (Admission) |
|------|---------------------|---------------------|
| 질문 | "이 사용자가 pods를 create할 수 있는가?" | "이 Pod를 허용할 것인가?" |
| 대상 | 사용자, 리소스 종류, verb | 실제 오브젝트 내용 |
| 예시 | RBAC 규칙 매칭 | 이미지 정책, 리소스 제한, 보안 정책 |
| 수정 | 불가 | Mutating 플러그인은 수정 가능 |

### 2.3 두 가지 위상

어드미션 컨트롤은 두 위상으로 나뉜다:

```
요청 오브젝트
  |
  v
+------------------------------------------+
| Mutating Phase (변형 위상)                 |
|  +--------+  +--------+  +--------+      |
|  | Plugin1|  | Plugin2|  | Plugin3|      |
|  +--------+  +--------+  +--------+      |
|  오브젝트를 수정할 수 있음                   |
|  순차 실행 (앞 플러그인의 변형을 뒤가 볼 수 있음)|
+------------------------------------------+
  |
  v (변형된 오브젝트)
+------------------------------------------+
| Validating Phase (검증 위상)               |
|  +--------+  +--------+  +--------+      |
|  | Plugin1|  | Plugin2|  | Plugin3|      |
|  +--------+  +--------+  +--------+      |
|  오브젝트를 수정할 수 없음                   |
|  하나라도 거부하면 전체 거부                  |
+------------------------------------------+
  |
  v (유효한 오브젝트)
etcd 저장
```

---

## 3. Admission 인터페이스 계층 구조

### 3.1 기본 인터페이스: Interface

```go
// staging/src/k8s.io/apiserver/pkg/admission/interfaces.go (122-127행)
type Interface interface {
    Handles(operation Operation) bool
}
```

모든 어드미션 플러그인의 최상위 인터페이스.
`Handles` 메서드는 이 플러그인이 해당 작업(CREATE, UPDATE 등)을 처리하는지 반환한다.

### 3.2 MutationInterface

```go
// interfaces.go (129-135행)
type MutationInterface interface {
    Interface

    // Admit makes an admission decision based on the request attributes.
    // Context is used only for timeout/deadline/cancellation and tracing information.
    Admit(ctx context.Context, a Attributes, o ObjectInterfaces) (err error)
}
```

오브젝트를 **변형할 수 있는** 어드미션 플러그인.
`Admit`에서 오브젝트를 수정(기본값 설정, 필드 추가 등)할 수 있다.
에러를 반환하면 요청이 거부된다.

### 3.3 ValidationInterface

```go
// interfaces.go (137-144행)
type ValidationInterface interface {
    Interface

    // Validate makes an admission decision based on the request attributes.
    // It is NOT allowed to mutate.
    // Context is used only for timeout/deadline/cancellation and tracing information.
    Validate(ctx context.Context, a Attributes, o ObjectInterfaces) (err error)
}
```

오브젝트를 **수정할 수 없는** 검증 전용 어드미션 플러그인.
`Validate`에서 에러를 반환하면 요청이 거부된다.

### 3.4 인터페이스 계층 다이어그램

```
                Interface
               /         \
              /           \
    MutationInterface   ValidationInterface
         |                    |
         | Admit()            | Validate()
         | (변형 가능)          | (검증만)
         v                    v

    * 하나의 플러그인이 두 인터페이스를 모두 구현할 수 있다
    * Mutating 위상에서는 MutationInterface만 호출
    * Validating 위상에서는 ValidationInterface만 호출
```

### 3.5 ObjectInterfaces

```go
// interfaces.go (80-93행)
type ObjectInterfaces interface {
    GetObjectCreater() runtime.ObjectCreater
    GetObjectTyper() runtime.ObjectTyper
    GetObjectDefaulter() runtime.ObjectDefaulter
    GetObjectConvertor() runtime.ObjectConvertor
    GetEquivalentResourceMapper() runtime.EquivalentResourceMapper
}
```

어드미션 플러그인이 오브젝트를 다루기 위해 필요한 유틸리티 인터페이스.
CRD와 같은 동적 타입의 오브젝트도 올바르게 처리할 수 있게 해준다.

---

## 4. Admission Attributes

### 4.1 Attributes 인터페이스

```go
// staging/src/k8s.io/apiserver/pkg/admission/interfaces.go (31-77행)
type Attributes interface {
    GetName() string                    // 오브젝트 이름
    GetNamespace() string               // 네임스페이스
    GetResource() schema.GroupVersionResource  // 리소스 (GVR)
    GetSubresource() string             // 하위 리소스
    GetOperation() Operation            // CREATE, UPDATE, DELETE, CONNECT
    GetOperationOptions() runtime.Object // CreateOptions, UpdateOptions 등
    IsDryRun() bool                     // DryRun 여부
    GetObject() runtime.Object          // 요청 오브젝트 (새 오브젝트)
    GetOldObject() runtime.Object       // 기존 오브젝트 (UPDATE/DELETE)
    GetKind() schema.GroupVersionKind   // 오브젝트 타입 (GVK)
    GetUserInfo() user.Info             // 요청 사용자 정보

    // 감사 로그용 어노테이션 추가
    AddAnnotation(key, value string) error
    AddAnnotationWithLevel(key, value string, level auditinternal.Level) error

    // Re-invocation 정책
    GetReinvocationContext() ReinvocationContext
}
```

### 4.2 주요 필드 설명

| 메서드 | 설명 | 예시 |
|--------|------|------|
| `GetName()` | CREATE 시 비어있을 수 있음 (서버 생성) | `"my-pod"` |
| `GetResource()` | GroupVersionResource | `{Group:"", Version:"v1", Resource:"pods"}` |
| `GetOperation()` | 수행 중인 작업 | `CREATE`, `UPDATE` |
| `GetObject()` | 새로 생성/수정할 오브젝트 | Pod 오브젝트 |
| `GetOldObject()` | UPDATE/DELETE 시 기존 오브젝트 | 이전 Pod |
| `IsDryRun()` | true이면 실제 저장 안 됨 | `true/false` |
| `GetUserInfo()` | 인증된 사용자 | `{Name:"admin"}` |

### 4.3 Object vs OldObject

```
CREATE:  GetObject() = 새 오브젝트,    GetOldObject() = nil
UPDATE:  GetObject() = 수정된 오브젝트, GetOldObject() = 기존 오브젝트
DELETE:  GetObject() = nil,            GetOldObject() = 삭제될 오브젝트
CONNECT: 리소스 연결 (포트 포워딩 등)
```

### 4.4 감사 어노테이션

```go
// interfaces.go (62-73행)
AddAnnotation(key, value string) error
AddAnnotationWithLevel(key, value string, level auditinternal.Level) error
```

어드미션 플러그인은 감사 로그에 어노테이션을 추가할 수 있다.
예: `podsecuritypolicy.admission.k8s.io/admit-policy` 키로
어떤 PSP가 적용되었는지 기록.

---

## 5. Admission Chain: Admit과 Validate

### 5.1 chainAdmissionHandler

```go
// staging/src/k8s.io/apiserver/pkg/admission/chain.go (22-28행)
type chainAdmissionHandler []Interface

func NewChainHandler(handlers ...Interface) chainAdmissionHandler {
    return chainAdmissionHandler(handlers)
}
```

체인은 `[]Interface` 슬라이스의 타입 에일리어스이다.
여러 어드미션 플러그인을 순서대로 실행한다.

### 5.2 Admit (Mutating Phase)

```go
// chain.go (31-44행)
func (admissionHandler chainAdmissionHandler) Admit(
    ctx context.Context, a Attributes, o ObjectInterfaces) error {

    for _, handler := range admissionHandler {
        if !handler.Handles(a.GetOperation()) {
            continue  // 이 작업을 처리하지 않는 플러그인은 건너뜀
        }
        if mutator, ok := handler.(MutationInterface); ok {
            err := mutator.Admit(ctx, a, o)
            if err != nil {
                return err  // 첫 번째 에러에서 즉시 중단
            }
        }
    }
    return nil
}
```

#### Admit 실행 흐름

```
Admit(ctx, attributes, objectInterfaces)
  |
  for each handler in chain:
  |   |
  |   +-- handler.Handles(operation)?
  |   |     No → skip
  |   |     Yes ↓
  |   +-- handler implements MutationInterface?
  |   |     No → skip
  |   |     Yes ↓
  |   +-- mutator.Admit(ctx, a, o)
  |         |
  |         +-- err != nil → 즉시 반환 (요청 거부)
  |         +-- err == nil → 다음 핸들러로
  |
  +-- 모든 핸들러 통과 → nil 반환 (성공)
```

### 5.3 Validate (Validating Phase)

```go
// chain.go (47-60행)
func (admissionHandler chainAdmissionHandler) Validate(
    ctx context.Context, a Attributes, o ObjectInterfaces) error {

    for _, handler := range admissionHandler {
        if !handler.Handles(a.GetOperation()) {
            continue
        }
        if validator, ok := handler.(ValidationInterface); ok {
            err := validator.Validate(ctx, a, o)
            if err != nil {
                return err  // 첫 번째 에러에서 즉시 중단
            }
        }
    }
    return nil
}
```

Admit과 구조적으로 동일하지만:
- `MutationInterface` 대신 `ValidationInterface` 확인
- `Admit()` 대신 `Validate()` 호출
- 오브젝트 수정 불가 (호출 규약)

### 5.4 Handles

```go
// chain.go (63-70행)
func (admissionHandler chainAdmissionHandler) Handles(operation Operation) bool {
    for _, handler := range admissionHandler {
        if handler.Handles(operation) {
            return true
        }
    }
    return false
}
```

체인 내 하나라도 해당 Operation을 처리하면 `true`.

### 5.5 두 위상의 실행 순서

```
요청 도착
  |
  v
[Admit Phase] ← Mutating 어드미션 플러그인
  |  플러그인 A의 Admit() → 오브젝트 수정 가능
  |  플러그인 B의 Admit() → A가 수정한 오브젝트를 볼 수 있음
  |  플러그인 C의 Admit() → A, B가 수정한 오브젝트를 볼 수 있음
  |
  v (변형 완료된 오브젝트)
[Validate Phase] ← Validating 어드미션 플러그인
  |  플러그인 A의 Validate() → 검증만, 수정 불가
  |  플러그인 B의 Validate()
  |  플러그인 C의 Validate()
  |
  v (유효한 오브젝트)
etcd 저장
```

---

## 6. 내장 어드미션 플러그인

### 6.1 플러그인 디렉토리 구조

내장 어드미션 플러그인은 `plugin/pkg/admission/` 아래에 위치한다:

```
plugin/pkg/admission/
├── admit/                      # AlwaysAdmit (항상 허용)
├── alwayspullimages/           # AlwaysPullImages
├── antiaffinity/               # LimitPodHardAntiAffinityTopology
├── certificates/               # 인증서 관련
├── defaulttolerationseconds/   # DefaultTolerationSeconds
├── deny/                       # AlwaysDeny (항상 거부)
├── eventratelimit/             # EventRateLimit
├── extendedresourcetoleration/ # ExtendedResourceToleration
├── gc/                         # OwnerReferencesPermissionEnforcement
├── imagepolicy/                # ImagePolicyWebhook
├── limitranger/                # LimitRanger
├── namespace/                  # NamespaceLifecycle, NamespaceExists
├── network/                    # 네트워크 정책 관련
├── nodedeclaredfeatures/       # NodeDeclaredFeatures
├── noderestriction/            # NodeRestriction
├── nodetaint/                  # TaintNodesByCondition
├── podnodeselector/            # PodNodeSelector
├── podtolerationrestriction/   # PodTolerationRestriction
├── podtopologylabels/          # PodTopologyLabels
├── priority/                   # Priority
├── resourcequota/              # ResourceQuota
├── runtimeclass/               # RuntimeClass
├── security/                   # PodSecurity (PSA)
├── serviceaccount/             # ServiceAccount
└── storage/                    # StorageObjectInUseProtection
```

### 6.2 주요 내장 플러그인 상세

| 플러그인 | 타입 | 기능 |
|---------|------|------|
| **NamespaceLifecycle** | Validate | 삭제 중인 네임스페이스에 새 오브젝트 생성 방지 |
| **LimitRanger** | Mutate+Validate | 리소스 요청/제한 기본값 설정 및 검증 |
| **ServiceAccount** | Mutate | Pod에 ServiceAccount 토큰 자동 마운트 |
| **DefaultTolerationSeconds** | Mutate | notready/unreachable toleration 기본 시간 설정 |
| **ResourceQuota** | Validate | 네임스페이스 리소스 쿼터 적용 |
| **PodSecurity** | Validate | Pod Security Standards 적용 (PSA) |
| **NodeRestriction** | Validate | kubelet이 자신의 노드/Pod만 수정 가능하게 제한 |
| **AlwaysPullImages** | Mutate | 모든 Pod 이미지를 Always pull 정책으로 강제 |
| **Priority** | Mutate | PriorityClass 기반 우선순위 설정 |
| **RuntimeClass** | Mutate | RuntimeClass 기반 오버헤드/스케줄링 설정 |
| **StorageObjectInUseProtection** | Mutate | PVC/PV에 finalizer 추가 (사용 중 삭제 방지) |

### 6.3 플러그인 타입별 분류

#### Mutating만 수행하는 플러그인

```
AlwaysPullImages     - imagePullPolicy를 Always로 변경
ServiceAccount       - ServiceAccount 토큰 볼륨 마운트 추가
DefaultTolerationSeconds - toleration 기본값 설정
Priority             - priorityClassName → priority 값 설정
RuntimeClass         - overhead, scheduling 설정 추가
StorageObjectInUseProtection - finalizer 추가
```

#### Validating만 수행하는 플러그인

```
NamespaceLifecycle   - 삭제 중 네임스페이스 보호
ResourceQuota        - 쿼터 초과 검사
PodSecurity          - PSA 레벨 검증
NodeRestriction      - 노드 권한 제한
EventRateLimit       - 이벤트 속도 제한
```

#### 양쪽 모두 수행하는 플러그인

```
LimitRanger          - Mutate: 기본값 설정, Validate: 범위 검증
```

### 6.4 기본 활성화 플러그인

Kubernetes 기본 설정에서 다음 플러그인이 활성화된다 (순서 중요):

```
NamespaceLifecycle,
LimitRanger,
ServiceAccount,
DefaultTolerationSeconds,
DefaultStorageClass,
StorageObjectInUseProtection,
MutatingAdmissionWebhook,
ValidatingAdmissionWebhook,
ResourceQuota,
PodSecurity
```

---

## 7. createHandler에서의 호출 흐름

### 7.1 createHandler 함수 시그니처

```go
// staging/src/k8s.io/apiserver/pkg/endpoints/handlers/create.go (53행)
func createHandler(r rest.NamedCreater, scope *RequestScope,
    admit admission.Interface, includeName bool) http.HandlerFunc {
```

### 7.2 Admission Attributes 생성

```go
// create.go (182행)
admissionAttributes := admission.NewAttributesRecord(
    obj,                    // 새 오브젝트
    nil,                    // OldObject (CREATE이므로 nil)
    scope.Kind,             // GroupVersionKind
    namespace,              // 네임스페이스
    name,                   // 오브젝트 이름
    scope.Resource,         // GroupVersionResource
    scope.Subresource,      // 하위 리소스
    admission.Create,       // Operation
    options,                // CreateOptions
    dryrun.IsDryRun(options.DryRun),  // DryRun 여부
    userInfo,               // 인증된 사용자
)
```

### 7.3 Mutating Admission 호출

```go
// create.go (202-206행)
if mutatingAdmission, ok := admit.(admission.MutationInterface); ok &&
    mutatingAdmission.Handles(admission.Create) {
    if err := mutatingAdmission.Admit(ctx, admissionAttributes, scope); err != nil {
        return nil, err    // 거부 → 에러 반환
    }
}
```

Mutating Admission은 `finisher.FinishRequest` 내부에서 호출된다.
오브젝트 생성 직전에 변형을 수행한다.

### 7.4 Validating Admission 호출

```go
// create.go (188-189행)
requestFunc := func() (runtime.Object, error) {
    return r.Create(
        ctx,
        name,
        obj,
        rest.AdmissionToValidateObjectFunc(admit, admissionAttributes, scope),
        options,
    )
}
```

Validating Admission은 `rest.AdmissionToValidateObjectFunc`를 통해
실제 저장소 `Create` 호출 내부에서 실행된다.
저장 직전에 최종 검증을 수행한다.

### 7.5 전체 흐름 다이어그램

```
createHandler(r, scope, admit, includeName)
  |
  v
[1] URL 파싱 → namespace, name 추출
  |
  v
[2] 요청 본문 읽기 → body
  |
  v
[3] CreateOptions 디코딩
  |
  v
[4] 요청 본문 디코딩 → obj (오브젝트)
  |
  v
[5] AdmissionAttributes 생성
  |
  v
[6] finisher.FinishRequest 내부:
  |   |
  |   +-- FieldManager 처리
  |   |
  |   +-- [Mutating Admission] ← admit.Admit(ctx, attrs, scope)
  |   |     오브젝트 변형 가능
  |   |     에러 시 → 요청 거부
  |   |
  |   +-- owner reference 중복 제거
  |   |
  |   +-- r.Create(ctx, name, obj, validateFunc, options)
  |         |
  |         +-- [Validating Admission] ← validateFunc 내부
  |         |     오브젝트 검증만
  |         |     에러 시 → 요청 거부
  |         |
  |         +-- etcd 저장
  |
  v
[7] 응답 반환 (201 Created)
```

### 7.6 Audit 통합

```go
// create.go (160행)
admit = admission.WithAudit(admit)
```

`WithAudit`으로 어드미션 플러그인을 감싸면,
어드미션 결정과 관련된 어노테이션이 자동으로 감사 로그에 기록된다.

---

## 8. Webhook 어드미션

### 8.1 Webhook 개요

Webhook 어드미션은 외부 서비스에 어드미션 결정을 위임한다.
Kubernetes는 두 종류의 Webhook을 지원한다:

| 종류 | 리소스 | 위상 |
|------|--------|------|
| **MutatingAdmissionWebhook** | MutatingWebhookConfiguration | Mutating Phase |
| **ValidatingAdmissionWebhook** | ValidatingWebhookConfiguration | Validating Phase |

### 8.2 MutatingWebhookConfiguration

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: my-mutating-webhook
webhooks:
- name: my-webhook.example.com
  clientConfig:
    service:
      name: my-webhook-service
      namespace: webhook-system
      path: /mutate
    caBundle: <CA_BUNDLE>
  rules:
  - apiGroups: [""]
    apiVersions: ["v1"]
    resources: ["pods"]
    operations: ["CREATE"]
  admissionReviewVersions: ["v1"]
  sideEffects: None
  reinvocationPolicy: IfNeeded
```

### 8.3 Webhook 호출 흐름

```
API Server → POST https://webhook-service/mutate
  |
  | AdmissionReview 요청:
  | {
  |   "apiVersion": "admission.k8s.io/v1",
  |   "kind": "AdmissionReview",
  |   "request": {
  |     "uid": "...",
  |     "kind": {"group":"","version":"v1","kind":"Pod"},
  |     "resource": {"group":"","version":"v1","resource":"pods"},
  |     "operation": "CREATE",
  |     "object": { ... Pod 오브젝트 ... },
  |     "oldObject": null,
  |     "userInfo": { ... }
  |   }
  | }
  |
  v
Webhook 서비스 → AdmissionReview 응답:
  | {
  |   "apiVersion": "admission.k8s.io/v1",
  |   "kind": "AdmissionReview",
  |   "response": {
  |     "uid": "...",
  |     "allowed": true,
  |     "patchType": "JSONPatch",
  |     "patch": "W3si...fQ==",   ← base64 인코딩된 JSON Patch
  |     "status": { "message": "..." }
  |   }
  | }
```

### 8.4 ValidatingWebhookConfiguration

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: my-validating-webhook
webhooks:
- name: my-validator.example.com
  clientConfig:
    url: "https://my-validator.example.com/validate"
  rules:
  - apiGroups: ["apps"]
    apiVersions: ["v1"]
    resources: ["deployments"]
    operations: ["CREATE", "UPDATE"]
  failurePolicy: Fail       # Webhook 실패 시 요청 거부
  matchPolicy: Equivalent   # 동등 리소스도 매칭
  sideEffects: None
```

### 8.5 failurePolicy

| 정책 | 동작 |
|------|------|
| `Fail` (기본) | Webhook 호출 실패 시 요청 거부 |
| `Ignore` | Webhook 호출 실패 시 무시하고 요청 허용 |

`Fail`은 안전하지만 Webhook 서비스 장애 시 클러스터 영향.
`Ignore`는 가용성 우선이지만 정책 우회 가능.

### 8.6 Webhook 매칭 규칙

```yaml
rules:
- apiGroups: [""]           # 코어 API 그룹
  apiVersions: ["v1"]       # v1 버전
  resources: ["pods"]       # pods 리소스
  operations: ["CREATE"]    # CREATE 작업만
  scope: "Namespaced"       # 네임스페이스 범위 리소스만
```

| 필드 | 와일드카드 | 예시 |
|------|-----------|------|
| `apiGroups` | `"*"` | `["", "apps", "batch"]` |
| `apiVersions` | `"*"` | `["v1", "v1beta1"]` |
| `resources` | `"*"`, `"*/status"` | `["pods", "pods/status"]` |
| `operations` | `"*"` | `["CREATE", "UPDATE"]` |
| `scope` | - | `"Cluster"`, `"Namespaced"`, `"*"` |

### 8.7 Mutating Webhook 순서와 Re-invocation

Mutating Webhook은 설정된 순서대로 실행된다.
`reinvocationPolicy: IfNeeded`가 설정되면,
이전 Webhook의 변형으로 인해 다른 Webhook을 다시 호출할 수 있다:

```
Webhook A → 오브젝트 변형
  |
  v
Webhook B → 오브젝트 변형
  |
  v
Webhook A (재호출) → 이전 결과와 비교, 추가 변형 필요?
  |
  v (최대 1회 재호출)
완료
```

---

## 9. ValidatingAdmissionPolicy (CEL)

### 9.1 개요

ValidatingAdmissionPolicy는 Kubernetes 1.26에서 도입된 기능으로,
**CEL(Common Expression Language)** 표현식을 사용하여
Webhook 서비스 없이 in-process로 유효성 검증을 수행한다.

### 9.2 ValidatingAdmissionPolicy 예시

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: "require-labels"
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
    - apiGroups: ["apps"]
      apiVersions: ["v1"]
      operations: ["CREATE", "UPDATE"]
      resources: ["deployments"]
  validations:
  - expression: "has(object.metadata.labels) && 'app' in object.metadata.labels"
    message: "모든 Deployment에는 'app' 라벨이 필요합니다"
    reason: Invalid
```

### 9.3 CEL 표현식에서 사용 가능한 변수

| 변수 | 설명 |
|------|------|
| `object` | 새로운/수정된 오브젝트 |
| `oldObject` | 기존 오브젝트 (UPDATE 시) |
| `request` | 어드미션 요청 정보 |
| `params` | 파라미터 리소스 (바인딩에서 참조) |
| `namespaceObject` | 네임스페이스 오브젝트 |
| `authorizer` | 인가 확인용 |

### 9.4 ValidatingAdmissionPolicyBinding

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: "require-labels-binding"
spec:
  policyName: "require-labels"
  validationActions: [Deny]
  matchResources:
    namespaceSelector:
      matchLabels:
        environment: production
```

### 9.5 Webhook vs CEL 비교

| 항목 | Webhook | ValidatingAdmissionPolicy (CEL) |
|------|---------|-------------------------------|
| 실행 위치 | 외부 서비스 | API Server 내부 (in-process) |
| 네트워크 | 필요 | 불필요 |
| 지연 | 네트워크 지연 포함 | 매우 빠름 |
| 운영 복잡도 | 서비스 배포/관리 필요 | YAML 정의만으로 충분 |
| 변형 가능 | MutatingWebhook은 가능 | 불가 (검증만) |
| 복잡한 로직 | Go/Python 등으로 자유롭게 | CEL 표현식 제한 |
| 외부 시스템 연동 | 가능 | 불가 |

---

## 10. Operation 타입

### 10.1 정의

```go
// staging/src/k8s.io/apiserver/pkg/admission/interfaces.go (147-155행)
type Operation string

const (
    Create  Operation = "CREATE"
    Update  Operation = "UPDATE"
    Delete  Operation = "DELETE"
    Connect Operation = "CONNECT"
)
```

### 10.2 Operation별 어드미션 동작

| Operation | GetObject() | GetOldObject() | 주요 검증 |
|-----------|-------------|----------------|----------|
| `CREATE` | 새 오브젝트 | nil | 이름 충돌, 쿼터, 정책 |
| `UPDATE` | 수정된 오브젝트 | 기존 오브젝트 | 불변 필드, 정책 |
| `DELETE` | nil (일부 경우 있음) | 삭제 대상 | finalizer, 보호 |
| `CONNECT` | ConnectOptions | nil | 포트 포워딩 제한 등 |

### 10.3 Handles 메서드와의 관계

```go
// 플러그인이 특정 Operation만 처리하도록 제한:
func (p *myPlugin) Handles(operation admission.Operation) bool {
    return operation == admission.Create || operation == admission.Update
}
```

---

## 11. 플러그인 초기화

### 11.1 PluginInitializer

```go
// staging/src/k8s.io/apiserver/pkg/admission/interfaces.go (158-161행)
type PluginInitializer interface {
    Initialize(plugin Interface)
}
```

어드미션 플러그인에 공유 리소스(Informer, Client, Authorizer 등)를
주입하기 위한 인터페이스.

### 11.2 초기화 검증

```go
// interfaces.go (165-167행)
type InitializationValidator interface {
    ValidateInitialization() error
}
```

플러그인이 올바르게 초기화되었는지 검증한다.
필수 리소스가 주입되지 않았으면 에러를 반환한다.

### 11.3 ConfigProvider

```go
// interfaces.go (170-172행)
type ConfigProvider interface {
    ConfigFor(pluginName string) (io.Reader, error)
}
```

플러그인별 설정 파일을 제공한다.
`--admission-control-config-file` 플래그로 전체 설정을 지정할 수 있다.

### 11.4 초기화 흐름

```
API Server 시작
  |
  v
[1] 플러그인 레지스트리에서 활성화된 플러그인 생성
  |
  v
[2] PluginInitializer.Initialize(plugin)
  |     Informer, Client, Authorizer 등 주입
  |
  v
[3] InitializationValidator.ValidateInitialization()
  |     필수 리소스 주입 확인
  |
  v
[4] 플러그인 체인 구성
  |
  v
[5] HTTP 핸들러 체인에 등록
```

---

## 12. ReinvocationContext

### 12.1 인터페이스

```go
// staging/src/k8s.io/apiserver/pkg/admission/interfaces.go (107-120행)
type ReinvocationContext interface {
    IsReinvoke() bool        // 현재 호출이 재호출인지
    SetIsReinvoke()          // 재호출 표시 설정
    ShouldReinvoke() bool    // 재호출이 필요한지
    SetShouldReinvoke()      // 재호출 요청
    SetValue(plugin string, v interface{})  // 플러그인 간 데이터 공유
    Value(plugin string) interface{}        // 플러그인 데이터 조회
}
```

### 12.2 재호출 메커니즘

Mutating Webhook에서 사용되는 재호출 메커니즘:

```
[1차 호출]
  Webhook A → 오브젝트 변형, SetShouldReinvoke() 호출
  Webhook B → 오브젝트 변형

[재호출 확인]
  ShouldReinvoke() == true?
    Yes ↓

[2차 호출] (재호출)
  Webhook A → IsReinvoke() == true, 이전 결과 확인 후 추가 변형
  Webhook B → 재호출
```

### 12.3 데이터 공유

```go
SetValue(plugin string, v interface{})
Value(plugin string) interface{}
```

같은 어드미션 요청 내에서 플러그인 간 데이터를 공유할 수 있다.
예: 첫 번째 호출에서 계산한 값을 재호출에서 재사용.

---

## 13. DryRun 처리

### 13.1 DryRun이란

`kubectl apply --dry-run=server`를 사용하면 실제 저장 없이
어드미션 컨트롤까지 실행한 결과를 볼 수 있다.

### 13.2 IsDryRun 확인

```go
// interfaces.go (48-52행)
// IsDryRun indicates that modifications will definitely not be persisted
// for this request. This is to prevent admission controllers with side
// effects and a method of reconciliation from being overwhelmed.
IsDryRun() bool
```

### 13.3 사이드 이펙트가 있는 플러그인의 DryRun 처리

어드미션 플러그인이 외부 시스템에 사이드 이펙트를 발생시키는 경우
(예: 외부 인증 서비스에 알림, 리소스 카운터 증가 등),
DryRun 요청에서는 이러한 사이드 이펙트를 건너뛰어야 한다:

```go
func (p *myPlugin) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
    // DryRun이면 사이드 이펙트 건너뛰기
    if a.IsDryRun() {
        // 검증만 수행, 외부 호출 생략
        return nil
    }

    // 실제 사이드 이펙트 수행
    externalService.Notify(a.GetObject())
    return nil
}
```

### 13.4 Webhook의 DryRun

Webhook의 `sideEffects` 필드가 DryRun 동작을 결정한다:

| sideEffects | DryRun 시 동작 |
|-------------|---------------|
| `None` | Webhook 호출됨, 사이드 이펙트 없다고 선언 |
| `NoneOnDryRun` | DryRun 시 사이드 이펙트 없다고 선언 |
| `Unknown` | DryRun 시 호출되지 않을 수 있음 |
| `Some` | DryRun 시 호출되지 않을 수 있음 |

---

## 14. 설계 원칙: Why

### 14.1 왜 Mutation과 Validation을 분리하는가?

**문제**: 변형과 검증을 하나의 위상에서 처리하면
순서 의존성 문제가 발생한다.

```
문제 시나리오:
  플러그인 A: "라벨 'env' 없으면 'production' 추가" (변형)
  플러그인 B: "라벨 'env'가 있어야 함" (검증)

  B가 A보다 먼저 실행되면?
  → B가 거부 (아직 A가 라벨을 추가하지 않음)
```

**해결**: Mutation → Validation 순서를 강제하면
모든 변형이 완료된 후에 검증이 실행된다.

```
올바른 순서:
  [Mutation] A: 라벨 추가 → 오브젝트 변형됨
  [Validation] B: 라벨 확인 → 통과
```

### 14.2 왜 체인에서 첫 번째 에러에 즉시 중단하는가?

```go
// chain.go (38-40행)
err := mutator.Admit(ctx, a, o)
if err != nil {
    return err  // 즉시 중단
}
```

**이유**:
1. **안전성**: 변형 플러그인에서 에러가 발생하면 오브젝트가 일관성 없는 상태일 수 있음
2. **성능**: 이미 거부된 요청에 추가 처리 불필요
3. **사이드 이펙트 방지**: 나머지 플러그인의 불필요한 외부 호출 방지

### 14.3 왜 Webhook 어드미션인가?

**문제**: 내장 플러그인만으로는 조직별 커스텀 정책을 구현할 수 없다.
API Server를 수정하고 재컴파일하는 것은 현실적이지 않다.

**해결**: Webhook 패턴으로 어드미션 로직을 외부화.

```
장점:
1. 언어 자유 (Go, Python, Java 등)
2. 독립 배포/업데이트
3. 사이드 이펙트 처리 가능 (외부 시스템 연동)
4. 테스트 용이

단점:
1. 네트워크 지연
2. 가용성 의존성 (Webhook 서비스 장애)
3. 운영 복잡도 증가
```

### 14.4 왜 ValidatingAdmissionPolicy (CEL)를 도입했는가?

**문제**: 간단한 검증을 위해서도 Webhook 서비스를 배포해야 함.
이는 과도한 운영 부담이다.

```
Webhook 배포 요구사항:
  1. 서비스 코드 작성
  2. Docker 이미지 빌드
  3. Deployment, Service 배포
  4. TLS 인증서 관리
  5. WebhookConfiguration 등록

vs

CEL ValidatingAdmissionPolicy:
  1. YAML 한 장 작성
  2. kubectl apply
```

**해결**: CEL 기반 in-process 검증으로
간단한 정책은 YAML만으로 정의할 수 있게 함.

### 14.5 왜 Handles 메서드가 있는가?

```go
type Interface interface {
    Handles(operation Operation) bool
}
```

**이유**: 불필요한 플러그인 실행을 건너뛰는 최적화.
예를 들어 `ResourceQuota` 플러그인은 `DELETE`에 대해
별도 처리가 필요 없으므로 `Handles(DELETE) == false`를 반환한다.

체인이 수십 개의 플러그인을 포함할 수 있으므로,
이 사전 검사가 성능에 중요하다.

### 14.6 왜 AnnotationsGetter를 분리하는가?

```go
// interfaces.go (96-104행)
type privateAnnotationsGetter interface {
    getAnnotations(maxLevel auditinternal.Level) map[string]string
}

type AnnotationsGetter interface {
    GetAnnotations(maxLevel auditinternal.Level) map[string]string
}
```

감사 로그와의 통합을 위해 어드미션 결과를 어노테이션으로 기록한다.
private/public 인터페이스를 분리하여
내부 구현과 외부 인터페이스를 명확히 구분한다.

### 14.7 왜 ObjectInterfaces를 분리하는가?

```go
type ObjectInterfaces interface {
    GetObjectCreater() runtime.ObjectCreater
    GetObjectTyper() runtime.ObjectTyper
    GetObjectDefaulter() runtime.ObjectDefaulter
    GetObjectConvertor() runtime.ObjectConvertor
    GetEquivalentResourceMapper() runtime.EquivalentResourceMapper
}
```

CRD(Custom Resource Definition) 지원 때문이다.
빌트인 리소스와 CRD는 타입 시스템이 다르므로,
어드미션 플러그인이 리소스 타입에 관계없이 동작하려면
이러한 추상화가 필요하다.

---

## 15. 정리

### 15.1 어드미션 컨트롤 전체 아키텍처

```
API 요청 (인증·인가 통과)
  |
  v
+============================================================+
|                    Admission Control                        |
|                                                            |
|  [Mutating Phase]                                          |
|  +--------+  +---------+  +--------+  +---------+         |
|  |Namespace|  |Service  |  |Limit   |  |Mutating |         |
|  |Lifecycle|  |Account  |  |Ranger  |  |Webhooks |         |
|  +--------+  +---------+  +--------+  +---------+         |
|  (validate   (mutate:     (mutate:    (외부 변형             |
|   only이면    토큰 마운트)  기본값)     서비스 호출)           |
|   skip)                                                     |
|                                                            |
|  [Validating Phase]                                        |
|  +--------+  +--------+  +---------+  +----------+        |
|  |Namespace|  |Resource|  |Pod      |  |Validating|        |
|  |Lifecycle|  |Quota   |  |Security |  |Webhooks  |        |
|  +--------+  +--------+  +---------+  +----------+        |
|  (네임스페이스 (쿼터 검증) (PSA 검증)  (외부 검증              |
|   삭제 보호)                           서비스 호출)           |
+============================================================+
  |
  v
etcd 저장
```

### 15.2 인터페이스 관계도

```
                admission.Interface
                    Handles(Op) bool
                   /              \
                  /                \
  MutationInterface          ValidationInterface
    Admit(ctx,a,o) err         Validate(ctx,a,o) err
        |                            |
        v                            v
  chainAdmissionHandler        chainAdmissionHandler
  (Mutating Phase 실행)        (Validating Phase 실행)
```

### 15.3 어드미션 유형 비교

| 유형 | 위치 | 변형 | 검증 | 복잡성 | 지연 |
|------|------|------|------|--------|------|
| 내장 플러그인 | API Server 내부 | 가능 | 가능 | 낮음 | 없음 |
| MutatingWebhook | 외부 서비스 | 가능 | 가능 | 높음 | 네트워크 |
| ValidatingWebhook | 외부 서비스 | 불가 | 가능 | 높음 | 네트워크 |
| ValidatingAdmissionPolicy | API Server 내부 | 불가 | 가능 | 중간 | 없음 |

### 15.4 언제 어떤 방식을 사용하는가

```
정책이 간단하고 검증만 필요?
  → ValidatingAdmissionPolicy (CEL)
    예: "모든 Deployment에 'app' 라벨 필수"

정책이 복잡하거나 외부 시스템 연동 필요?
  → Webhook Admission
    예: "이미지 서명 검증", "외부 정책 엔진 연동"

오브젝트에 기본값을 주입해야 하는가?
  → MutatingWebhook 또는 내장 플러그인
    예: "사이드카 자동 주입", "라벨 추가"

Kubernetes 핵심 기능의 일부인가?
  → 내장 플러그인
    예: NamespaceLifecycle, ServiceAccount
```

### 15.5 주의사항

1. **순서 중요**: Mutating Webhook의 실행 순서가 결과에 영향을 미침
2. **failurePolicy**: `Fail`이 기본값이므로 Webhook 장애 시 영향 고려
3. **성능**: Webhook 호출은 네트워크 지연을 추가하므로 타임아웃 설정 중요
4. **보안**: Webhook 서비스와의 통신은 반드시 TLS 사용
5. **DryRun**: 사이드 이펙트가 있는 플러그인은 DryRun 처리 필수
