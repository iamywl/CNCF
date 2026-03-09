# 29. Pod Security Admission (PSA) 심화

## 목차

1. [개요](#1-개요)
2. [Security Level 정의 (Privileged/Baseline/Restricted)](#2-security-level-정의)
3. [Namespace 라벨 기반 설정](#3-namespace-라벨-기반-설정)
4. [Admission 구조체와 Validate 흐름](#4-admission-구조체와-validate-흐름)
5. [Policy Evaluator와 Check Registry](#5-policy-evaluator와-check-registry)
6. [Check 구현 상세 (19개 체크)](#6-check-구현-상세)
7. [Baseline Level 체크 목록](#7-baseline-level-체크-목록)
8. [Restricted Level 체크 목록](#8-restricted-level-체크-목록)
9. [Exemption 시스템](#9-exemption-시스템)
10. [Kubernetes Plugin 통합](#10-kubernetes-plugin-통합)
11. [왜 이런 설계인가](#11-왜-이런-설계인가)
12. [정리](#12-정리)

---

## 1. 개요

Pod Security Admission(PSA)은 Kubernetes v1.25에서 GA(General Availability)로 승격된 빌트인 Admission Controller이다. 이전 PodSecurityPolicy(PSP)를 대체하며, Pod가 정의된 보안 표준(Pod Security Standards, PSS)을 준수하는지 검증한다.

### PSA가 해결하는 문제

```
기존 PodSecurityPolicy의 문제:
+--------------------------------------------------+
|  1. 복잡한 RBAC 바인딩 필요                        |
|  2. 정책 충돌 시 예측 불가능한 동작                  |
|  3. 뮤테이션과 밸리데이션이 혼합                     |
|  4. 디버깅이 극도로 어려움                          |
+--------------------------------------------------+
                     |
                     v
PSA의 해결 방식:
+--------------------------------------------------+
|  1. Namespace 라벨 기반 단순 설정                   |
|  2. 세 가지 명확한 보안 레벨                        |
|  3. 밸리데이션 전용 (뮤테이션 없음)                  |
|  4. Enforce/Audit/Warn 세 가지 모드                |
+--------------------------------------------------+
```

### 소스코드 위치

PSA 관련 코드는 Kubernetes 소스 트리에서 다음 위치에 있다.

| 디렉토리 | 역할 |
|----------|------|
| `staging/src/k8s.io/pod-security-admission/api/` | Level, Version, Policy 등 기본 타입 정의 |
| `staging/src/k8s.io/pod-security-admission/policy/` | Check 인터페이스, Registry, 19개 체크 구현 |
| `staging/src/k8s.io/pod-security-admission/admission/` | Admission 핵심 로직 (Validate, EvaluatePod) |
| `plugin/pkg/admission/security/podsecurity/` | kube-apiserver에 통합되는 Plugin 래퍼 |

---

## 2. Security Level 정의

PSS는 세 가지 보안 레벨을 정의한다. 소스코드에서의 정의는 `api/constants.go`(19-25행)에 있다.

```go
// staging/src/k8s.io/pod-security-admission/api/constants.go
type Level string

const (
    LevelPrivileged Level = "privileged"
    LevelBaseline   Level = "baseline"
    LevelRestricted Level = "restricted"
)
```

### 레벨별 보안 강도

```
보안 강도 (낮음 ←────────────────────────→ 높음)

┌─────────────┐   ┌─────────────┐   ┌─────────────┐
│  Privileged │   │  Baseline   │   │ Restricted  │
│             │   │             │   │             │
│ 제한 없음    │   │ 위험한 권한  │   │ 최소 권한    │
│ (모든 허용)  │   │  에스컬이션  │   │  원칙 적용   │
│             │   │  차단        │   │             │
│ 체크: 0개   │   │ 체크: 12개  │   │ 체크: 7개   │
│             │   │             │   │ +Baseline   │
└─────────────┘   └─────────────┘   └─────────────┘
```

| 레벨 | 목적 | 적용 대상 | 체크 수 |
|------|------|----------|---------|
| Privileged | 제한 없음, 모든 설정 허용 | 시스템 네임스페이스 (kube-system) | 0 |
| Baseline | 알려진 권한 상승 공격 차단 | 일반 워크로드의 기본 보안 | 12 |
| Restricted | 최소 권한 원칙에 따른 엄격한 제한 | 보안 민감 워크로드 | 7 + Baseline 전체 |

### Privileged 레벨

Privileged 레벨은 체크를 하나도 수행하지 않는다. `checkRegistry.EvaluatePod()`에서 즉시 반환한다.

```go
// staging/src/k8s.io/pod-security-admission/policy/registry.go (67-70행)
func (r *checkRegistry) EvaluatePod(lv api.LevelVersion, ...) []CheckResult {
    if lv.Level == api.LevelPrivileged {
        return nil  // 체크 없이 즉시 허용
    }
    // ...
}
```

### Baseline 레벨

Baseline 레벨은 호스트 네임스페이스 공유, 특권 컨테이너, 위험한 capability 추가 등 "명백하게 위험한" 설정만 차단한다. 대부분의 컨테이너화된 애플리케이션이 수정 없이 통과할 수 있는 수준이다.

### Restricted 레벨

Restricted 레벨은 Baseline의 모든 체크에 더해, 비루트 실행 강제, capability 전체 드롭, seccomp 프로파일 필수 지정 등 적극적인 보안 정책을 적용한다. 일부 체크는 Baseline 체크를 더 엄격한 버전으로 "오버라이드"한다.

---

## 3. Namespace 라벨 기반 설정

PSA 정책은 Namespace 라벨을 통해 설정된다. `api/constants.go`(37-50행)에서 정의된 라벨 키를 사용한다.

```go
// staging/src/k8s.io/pod-security-admission/api/constants.go (37-50행)
const (
    labelPrefix = "pod-security.kubernetes.io/"

    EnforceLevelLabel   = labelPrefix + "enforce"
    EnforceVersionLabel = labelPrefix + "enforce-version"
    AuditLevelLabel     = labelPrefix + "audit"
    AuditVersionLabel   = labelPrefix + "audit-version"
    WarnLevelLabel      = labelPrefix + "warn"
    WarnVersionLabel    = labelPrefix + "warn-version"
)
```

### 세 가지 모드

```
Namespace 라벨 예시:
  pod-security.kubernetes.io/enforce: baseline
  pod-security.kubernetes.io/enforce-version: v1.28
  pod-security.kubernetes.io/audit: restricted
  pod-security.kubernetes.io/audit-version: latest
  pod-security.kubernetes.io/warn: restricted
  pod-security.kubernetes.io/warn-version: latest

                    |
                    v
+------------------------------------------------------+
|                   PSA Controller                      |
|                                                       |
|  +----------+  +----------+  +----------+            |
|  | Enforce  |  |  Audit   |  |  Warn    |            |
|  |          |  |          |  |          |            |
|  | 위반 시   |  | 위반 시   |  | 위반 시   |            |
|  | 요청 거부 |  | 감사 로그 |  | 경고 헤더 |            |
|  |          |  | 기록     |  | 응답     |            |
|  +----------+  +----------+  +----------+            |
+------------------------------------------------------+
```

| 모드 | 라벨 | 위반 시 동작 | Pod 생성 차단 |
|------|------|-------------|-------------|
| Enforce | `enforce` | API 요청 거부 (403 Forbidden) | 예 |
| Audit | `audit` | 감사 로그에 annotation 기록 | 아니오 |
| Warn | `warn` | API 응답에 Warning 헤더 추가 | 아니오 |

### Policy 구조체

세 모드의 레벨과 버전은 `Policy` 구조체로 집약된다.

```go
// staging/src/k8s.io/pod-security-admission/api/helpers.go (154-158행)
type Policy struct {
    Enforce LevelVersion
    Audit   LevelVersion
    Warn    LevelVersion
}
```

`LevelVersion`은 보안 레벨과 해당 레벨을 적용할 Kubernetes 버전의 쌍이다. 예를 들어 `baseline:v1.28`은 "v1.28 기준의 baseline 정책"을 의미한다. 버전에 따라 체크 항목이 달라질 수 있기 때문에 이 정보가 필요하다.

### 버전 지정의 의미

`enforce-version: v1.28`로 설정하면, v1.28 시점에 정의된 체크 목록으로 평가한다. 이후 v1.29에 새로운 체크가 추가되더라도 v1.28 기준으로 평가하므로 클러스터 업그레이드 시 기존 워크로드가 갑자기 거부되는 상황을 방지할 수 있다.

```
버전 지정에 따른 체크 범위:

enforce-version: v1.22     enforce-version: v1.28     enforce-version: latest
+------------------+       +------------------+       +------------------+
| 1.22에 존재하는   |       | 1.22 체크         |       | 모든 체크         |
| 체크만 적용       |       | + 1.25 추가 체크   |       | (최신 버전 기준)   |
|                  |       | + 1.27 추가 체크   |       |                  |
|                  |       | + 1.28 추가 체크   |       |                  |
+------------------+       +------------------+       +------------------+
```

---

## 4. Admission 구조체와 Validate 흐름

### Admission 구조체

PSA의 핵심 로직은 `Admission` 구조체에 집약되어 있다.

```go
// staging/src/k8s.io/pod-security-admission/admission/admission.go (51-73행)
type Admission struct {
    Configuration *admissionapi.PodSecurityConfiguration

    // 레벨/버전별 정책 체크 수행
    Evaluator policy.Evaluator

    // 메트릭 기록
    Metrics metrics.Recorder

    // 임의 객체 -> PodSpec 추출
    PodSpecExtractor PodSpecExtractor

    // API 연결
    NamespaceGetter NamespaceGetter
    PodLister       PodLister

    defaultPolicy api.Policy

    namespaceMaxPodsToCheck  int
    namespacePodCheckTimeout time.Duration
}
```

### 의존성 인터페이스

`Admission`은 네 가지 주요 인터페이스에 의존한다.

```go
// Evaluator: 체크 레지스트리
type Evaluator interface {
    EvaluatePod(lv api.LevelVersion, podMetadata *metav1.ObjectMeta,
        podSpec *corev1.PodSpec) []CheckResult
}

// NamespaceGetter: Namespace 조회
type NamespaceGetter interface {
    GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error)
}

// PodLister: Pod 목록 조회 (Namespace 정책 변경 시 기존 Pod 검사용)
type PodLister interface {
    ListPods(ctx context.Context, namespace string) ([]*corev1.Pod, error)
}

// PodSpecExtractor: 다양한 리소스에서 PodSpec 추출
type PodSpecExtractor interface {
    HasPodSpec(schema.GroupResource) bool
    ExtractPodSpec(runtime.Object) (*metav1.ObjectMeta, *corev1.PodSpec, error)
}
```

### Validate 메서드 - 진입점

모든 Admission 요청은 `Validate()` 메서드를 거친다.

```go
// staging/src/k8s.io/pod-security-admission/admission/admission.go (213-224행)
func (a *Admission) Validate(ctx context.Context, attrs api.Attributes) *admissionv1.AdmissionResponse {
    var response *admissionv1.AdmissionResponse
    switch attrs.GetResource().GroupResource() {
    case namespacesResource:
        response = a.ValidateNamespace(ctx, attrs)
    case podsResource:
        response = a.ValidatePod(ctx, attrs)
    default:
        response = a.ValidatePodController(ctx, attrs)
    }
    return response
}
```

리소스 타입에 따라 세 가지 경로로 분기한다.

```
                    Validate()
                       |
          +------------+------------+
          |            |            |
          v            v            v
  ValidateNamespace  ValidatePod  ValidatePodController
  (namespace 라벨    (Pod 직접    (Deployment, Job 등
   유효성 검증 +      검증)        PodSpec 추출 후 검증)
   기존 Pod 검사)
```

### ValidatePod 흐름

Pod에 대한 검증의 핵심 흐름이다. 코드에서 다수의 단축 경로(short-circuit)를 통해 불필요한 검사를 피한다.

```go
// staging/src/k8s.io/pod-security-admission/admission/admission.go (329-389행)
func (a *Admission) ValidatePod(ctx context.Context, attrs api.Attributes) *admissionv1.AdmissionResponse {
    // 1. 무시할 서브리소스 단축 경로 (exec, attach, log 등)
    if ignoredPodSubresources[attrs.GetSubresource()] {
        return sharedAllowedResponse
    }
    // 2. 면제 네임스페이스 확인
    if a.exemptNamespace(attrs.GetNamespace()) {
        a.Metrics.RecordExemption(attrs)
        return sharedAllowedByNamespaceExemptionResponse
    }
    // 3. 면제 사용자 확인
    if a.exemptUser(attrs.GetUserName()) {
        a.Metrics.RecordExemption(attrs)
        return sharedAllowedByUserExemptionResponse
    }
    // 4. 네임스페이스 정책 조회 및 FullyPrivileged 확인
    namespace, err := a.NamespaceGetter.GetNamespace(ctx, attrs.GetNamespace())
    nsPolicy, nsPolicyErrs := a.PolicyToEvaluate(namespace.Labels)
    if len(nsPolicyErrs) == 0 && nsPolicy.FullyPrivileged() {
        return sharedAllowedPrivilegedResponse
    }
    // 5. Pod 디코딩
    obj, err := attrs.GetObject()
    pod, ok := obj.(*corev1.Pod)
    // 6. 업데이트 시 유의미한 변경인지 확인
    if attrs.GetOperation() == admissionv1.Update {
        oldPod, _ := ...
        if !isSignificantPodUpdate(pod, oldPod) {
            return sharedAllowedResponse
        }
    }
    // 7. EvaluatePod 호출
    return a.EvaluatePod(ctx, nsPolicy, nsPolicyErrs.ToAggregate(),
        &pod.ObjectMeta, &pod.Spec, attrs, true)
}
```

### 무시되는 Pod 서브리소스

```go
// admission.go (316-325행)
var ignoredPodSubresources = map[string]bool{
    "exec":        true,
    "attach":      true,
    "binding":     true,
    "eviction":    true,
    "log":         true,
    "portforward": true,
    "proxy":       true,
    "status":      true,
}
```

이 서브리소스들은 PodSpec을 변경하지 않으므로 검사할 필요가 없다.

### 전체 흐름 시퀀스

```
 클라이언트         kube-apiserver         PSA Admission         Namespace Cache
    |                    |                      |                      |
    |  Pod 생성 요청      |                      |                      |
    +------------------->|                      |                      |
    |                    |  Validate(attrs)     |                      |
    |                    +--------------------->|                      |
    |                    |                      |                      |
    |                    |                      | GetNamespace()       |
    |                    |                      +--------------------->|
    |                    |                      |<---------------------+
    |                    |                      |                      |
    |                    |                      | 라벨에서 Policy 파싱  |
    |                    |                      |                      |
    |                    |                      | EvaluatePod()        |
    |                    |                      |  - Enforce 평가      |
    |                    |                      |  - Audit 평가        |
    |                    |                      |  - Warn 평가         |
    |                    |                      |                      |
    |                    |  AdmissionResponse   |                      |
    |                    |<---------------------+                      |
    |  응답 (허용/거부)   |                      |                      |
    |<-------------------+                      |                      |
```

### EvaluatePod - 핵심 평가 로직

`EvaluatePod`은 Enforce, Audit, Warn 세 모드를 순차적으로 평가하며, 동일한 레벨+버전이면 캐시를 활용한다.

```go
// staging/src/k8s.io/pod-security-admission/admission/admission.go (455-528행)
func (a *Admission) EvaluatePod(ctx context.Context, nsPolicy api.Policy,
    nsPolicyErr error, podMetadata *metav1.ObjectMeta,
    podSpec *corev1.PodSpec, attrs api.Attributes, enforce bool) *admissionv1.AdmissionResponse {

    // 면제 RuntimeClass 확인
    if a.exemptRuntimeClass(podSpec.RuntimeClassName) {
        return sharedAllowedByRuntimeClassExemptionResponse
    }

    cachedResults := make(map[api.LevelVersion]policy.AggregateCheckResult)
    response := allowedResponse()

    // [1단계] Enforce 평가 (enforce=true일 때만)
    if enforce {
        auditAnnotations[api.EnforcedPolicyAnnotationKey] = nsPolicy.Enforce.String()

        result := policy.AggregateCheckResults(
            a.Evaluator.EvaluatePod(nsPolicy.Enforce, podMetadata, podSpec))
        if !result.Allowed {
            response = forbiddenResponse(attrs, fmt.Errorf(
                "violates PodSecurity %q: %s",
                nsPolicy.Enforce.String(), result.ForbiddenDetail()))
            a.Metrics.RecordEvaluation(metrics.DecisionDeny, nsPolicy.Enforce,
                metrics.ModeEnforce, attrs)
        } else {
            a.Metrics.RecordEvaluation(metrics.DecisionAllow, nsPolicy.Enforce,
                metrics.ModeEnforce, attrs)
        }
        cachedResults[nsPolicy.Enforce] = result
    }

    // [2단계] Audit 평가 (캐시 재사용 가능)
    auditResult, ok := cachedResults[nsPolicy.Audit]
    if !ok {
        auditResult = policy.AggregateCheckResults(
            a.Evaluator.EvaluatePod(nsPolicy.Audit, podMetadata, podSpec))
        cachedResults[nsPolicy.Audit] = auditResult
    }
    if !auditResult.Allowed {
        auditAnnotations[api.AuditViolationsAnnotationKey] = fmt.Sprintf(
            "would violate PodSecurity %q: %s",
            nsPolicy.Audit.String(), auditResult.ForbiddenDetail())
    }

    // [3단계] Warn 평가 (이미 거부된 요청에는 경고 추가 안 함)
    if response.Allowed {
        warnResult, ok := cachedResults[nsPolicy.Warn]
        if !ok {
            warnResult = policy.AggregateCheckResults(
                a.Evaluator.EvaluatePod(nsPolicy.Warn, podMetadata, podSpec))
        }
        if !warnResult.Allowed {
            response.Warnings = append(response.Warnings, fmt.Sprintf(
                "would violate PodSecurity %q: %s",
                nsPolicy.Warn.String(), warnResult.ForbiddenDetail()))
        }
    }

    response.AuditAnnotations = auditAnnotations
    return response
}
```

핵심 최적화: `cachedResults` 맵을 통해 동일한 `LevelVersion`이면 평가를 재사용한다. 예를 들어 enforce=baseline:v1.28, audit=baseline:v1.28이면 Evaluator를 한 번만 호출한다.

```
EvaluatePod 내부 흐름:

                     +----------------------+
                     |  exemptRuntimeClass? |
                     +----------+-----------+
                                | No
                     +----------v-----------+
              +------+  enforce=true?       |
              | No   +----------+-----------+
              |                 | Yes
              |      +----------v-----------+
              |      | Enforce 평가         +--+
              |      | (결과 캐시)           |  | 위반 -> 403 Forbidden
              |      +----------+-----------+  |
              |                 |               |
              +--------+-------+               |
              |        |                       |
              |  +-----v---------------+       |
              |  | Audit 평가           |       |
              |  | (캐시 확인 -> 재사용)  |       |
              |  +-----+---------------+       |
              |        |                       |
              |  +-----v---------------+       |
              |  | 이미 거부됨?         |       |
              |  +--+-------------+----+       |
              | Yes |             | No         |
              |     |  +----------v------+     |
              |     |  | Warn 평가       |     |
              |     |  | (캐시 확인)      |     |
              |     |  +-----------------+     |
              |     |                          |
              +-----+--------------------------+
                    |
              +-----v--------------+
              |  응답 반환          |
              |  - Allowed/Denied  |
              |  - AuditAnnot.     |
              |  - Warnings        |
              +--------------------+
```

### PodSpec 추출 (Pod Controller 처리)

PSA는 Pod뿐만 아니라 Pod를 내장하는 컨트롤러도 검사한다. `DefaultPodSpecExtractor`가 다양한 리소스 타입에서 PodSpec을 추출한다.

```go
// 지원하는 리소스 (admission.go 94-104행)
var defaultPodSpecResources = map[schema.GroupResource]bool{
    corev1.Resource("pods"):                   true,
    corev1.Resource("replicationcontrollers"): true,
    corev1.Resource("podtemplates"):           true,
    appsv1.Resource("replicasets"):            true,
    appsv1.Resource("deployments"):            true,
    appsv1.Resource("statefulsets"):           true,
    appsv1.Resource("daemonsets"):             true,
    batchv1.Resource("jobs"):                  true,
    batchv1.Resource("cronjobs"):              true,
}
```

`ExtractPodSpec()`은 리소스 타입별로 switch문을 통해 PodSpec을 추출한다.

```go
// admission.go (112-135행)
func (DefaultPodSpecExtractor) ExtractPodSpec(obj runtime.Object) (...) {
    switch o := obj.(type) {
    case *corev1.Pod:
        return &o.ObjectMeta, &o.Spec, nil
    case *appsv1.Deployment:
        return extractPodSpecFromTemplate(&o.Spec.Template)
    case *batchv1.CronJob:
        return extractPodSpecFromTemplate(&o.Spec.JobTemplate.Spec.Template)
    // ... 기타 리소스 타입
    }
}
```

Pod Controller에 대해서는 Enforce 모드가 적용되지 않는다. `ValidatePodController()`는 `EvaluatePod()`를 `enforce=false`로 호출하여 Audit과 Warn만 적용한다.

---

## 5. Policy Evaluator와 Check Registry

### Evaluator 인터페이스

```go
// staging/src/k8s.io/pod-security-admission/policy/registry.go (28-32행)
type Evaluator interface {
    EvaluatePod(lv api.LevelVersion, podMetadata *metav1.ObjectMeta,
        podSpec *corev1.PodSpec) []CheckResult
}
```

단일 메서드 인터페이스로, 주어진 레벨+버전 조합에 대해 PodSpec을 평가하고 결과 목록을 반환한다.

### checkRegistry 구조체

`Evaluator`의 기본 구현체는 `checkRegistry`이다.

```go
// staging/src/k8s.io/pod-security-admission/policy/registry.go (35-41행)
type checkRegistry struct {
    // 버전 -> 체크 함수 슬라이스 맵
    baselineChecks, restrictedChecks map[api.Version][]CheckPodFn
    // 캐시된 최대 버전
    maxVersion api.Version
}
```

핵심 설계: 버전별로 미리 계산된 체크 함수 슬라이스를 맵에 저장한다. 평가 시점에는 맵 조회 한 번으로 해당 버전의 전체 체크 목록을 가져온다.

```
checkRegistry 내부 구조:

baselineChecks:
  v1.0  -> [privileged_1_0, hostNamespaces_1_0, hostPorts_1_0, ...]
  v1.19 -> [privileged_1_0, hostNamespaces_1_0, ..., seccompBaseline_1_19, ...]
  v1.25 -> [privileged_1_0, ..., seccompBaseline_1_19, ...]
  v1.27 -> [..., sysctls_v1.27, ...]
  v1.31 -> [..., seLinuxOptions1_31, ...]
  v1.34 -> [..., hostProbesAndHostLifecycleV1Dot34, ...]
  v1.35 -> [..., procMount1_35baseline, ...]
  ...

restrictedChecks:
  v1.0  -> [baseline체크들... + restrictedVolumes_1_0, runAsNonRoot_1_0, ...]
  v1.8  -> [..., allowPrivilegeEscalation_1_8, ...]
  v1.19 -> [..., seccompRestricted_1_19, ...]  (seccompBaseline 오버라이드)
  v1.22 -> [..., capabilitiesRestricted_1_22, ...] (capabilitiesBaseline 오버라이드)
  v1.23 -> [..., runAsUser_1_23, ...]
  v1.25 -> [..., Windows 면제 버전들, ...]
  v1.35 -> [..., procMount_restricted, ...] (procMount 오버라이드)
  ...
```

### NewEvaluator - Registry 생성

```go
// registry.go (49-65행)
func NewEvaluator(checks []Check, emulationVersion *api.Version) (*checkRegistry, error) {
    if err := validateChecks(checks); err != nil {
        return nil, err
    }
    r := &checkRegistry{
        baselineChecks:   map[api.Version][]CheckPodFn{},
        restrictedChecks: map[api.Version][]CheckPodFn{},
    }
    populate(r, checks)

    // 에뮬레이션 버전이 maxVersion보다 작으면 제한
    if emulationVersion != nil && (*emulationVersion).Older(r.maxVersion) {
        r.maxVersion = *emulationVersion
    }
    return r, nil
}
```

### 체크 유효성 검증

```go
// registry.go (90-138행)
func validateChecks(checks []Check) error {
    ids := map[CheckID]api.Level{}
    for _, check := range checks {
        // 1. ID 중복 불가
        if _, ok := ids[check.ID]; ok {
            return fmt.Errorf("multiple checks registered for ID %s", check.ID)
        }
        // 2. Level은 Baseline 또는 Restricted만 가능
        if check.Level != api.LevelBaseline && check.Level != api.LevelRestricted {
            return fmt.Errorf("check %s: invalid level %s", check.ID, check.Level)
        }
        // 3. Versions은 비어 있으면 안 되고, 엄격히 증가해야 함
        // 4. 'latest' 버전 불허
    }
    // 두 번째 패스: override 검증
    for _, check := range checks {
        for _, c := range check.Versions {
            // OverrideCheckIDs는 Restricted 체크만 가능
            if check.Level != api.LevelRestricted {
                return fmt.Errorf("check %s: only restricted checks may set overrides", check.ID)
            }
            // 오버라이드 대상은 Baseline 체크만 가능
        }
    }
}
```

### 버전 인플레이션 (Version Inflation)

`populate()` 함수가 각 체크의 버전 범위를 "인플레이트"하여 모든 마이너 버전에 대응하는 체크 슬라이스를 사전 계산한다.

```go
// registry.go (194-212행)
func inflateVersions(check Check, versions map[api.Version]map[CheckID]VersionedCheck,
    maxVersion api.Version) {
    for i, c := range check.Versions {
        var nextVersion api.Version
        if i+1 < len(check.Versions) {
            nextVersion = check.Versions[i+1].MinimumVersion
        } else {
            nextVersion = nextMinor(maxVersion)
        }
        // MinimumVersion부터 다음 버전 직전까지 동일한 체크 함수를 할당
        for v := c.MinimumVersion; v.Older(nextVersion); v = nextMinor(v) {
            if versions[v] == nil {
                versions[v] = map[CheckID]VersionedCheck{}
            }
            versions[v][check.ID] = check.Versions[i]
        }
    }
}
```

예를 들어, `sysctls` 체크는 v1.0, v1.27, v1.29, v1.32에서 각각 다른 구현을 갖는다.

```
sysctls 체크의 버전 인플레이션:

체크 정의:  v1.0  -> sysctlsV1Dot0
            v1.27 -> sysctlsV1Dot27
            v1.29 -> sysctlsV1Dot29
            v1.32 -> sysctlsV1Dot32

인플레이트 결과:
  v1.0  ~ v1.26  -> sysctlsV1Dot0 함수
  v1.27 ~ v1.28  -> sysctlsV1Dot27 함수
  v1.29 ~ v1.31  -> sysctlsV1Dot29 함수
  v1.32+         -> sysctlsV1Dot32 함수
```

### EvaluatePod 메서드

```go
// registry.go (67-88행)
func (r *checkRegistry) EvaluatePod(lv api.LevelVersion,
    podMetadata *metav1.ObjectMeta, podSpec *corev1.PodSpec) []CheckResult {

    if lv.Level == api.LevelPrivileged {
        return nil  // Privileged -> 즉시 허용
    }
    if r.maxVersion.Older(lv.Version) {
        lv.Version = r.maxVersion  // 요청 버전이 최대보다 크면 최대 버전으로 cap
    }

    var checks []CheckPodFn
    if lv.Level == api.LevelBaseline {
        checks = r.baselineChecks[lv.Version]
    } else {
        // Restricted는 비-오버라이드 baseline 체크를 포함
        checks = r.restrictedChecks[lv.Version]
    }

    var results []CheckResult
    for _, check := range checks {
        results = append(results, check(podMetadata, podSpec))
    }
    return results
}
```

### 오버라이드 시스템

Restricted 체크 중 일부는 Baseline 체크를 "오버라이드"한다. 예를 들어 `capabilities_restricted`는 `capabilities_baseline`을 오버라이드한다. 같은 필드에 대해 더 엄격한 검사를 수행하므로 중복 검사를 방지한다.

```go
// check_capabilities_restricted.go (59-77행)
func CheckCapabilitiesRestricted() Check {
    return Check{
        ID:    "capabilities_restricted",
        Level: api.LevelRestricted,
        Versions: []VersionedCheck{
            {
                MinimumVersion:   api.MajorMinorVersion(1, 22),
                CheckPod:         capabilitiesRestricted_1_22,
                OverrideCheckIDs: []CheckID{checkCapabilitiesBaselineID},
            },
        },
    }
}
```

`populate()` 함수에서 오버라이드가 처리된다.

```go
// registry.go (170-191행)
for v := ... {
    // Restricted에서 오버라이드하는 baseline 체크 ID 수집
    overrides := map[CheckID]bool{}
    for _, c := range restrictedVersionedChecks[v] {
        for _, override := range c.OverrideCheckIDs {
            overrides[override] = true
        }
    }
    // 오버라이드되지 않은 baseline 체크만 restricted에 추가
    for id, c := range baselineVersionedChecks[v] {
        if overrides[id] {
            continue // 오버라이드된 체크는 건너뜀
        }
        restrictedVersionedChecks[v][id] = c
    }
}
```

---

## 6. Check 구현 상세

### Check 타입 정의

```go
// staging/src/k8s.io/pod-security-admission/policy/checks.go (27-39행)
type Check struct {
    ID       CheckID           // 체크 고유 식별자
    Level    api.Level         // Baseline 또는 Restricted
    Versions []VersionedCheck  // 버전별 구현
}

type VersionedCheck struct {
    MinimumVersion   api.Version    // 이 구현이 적용되는 최소 버전
    CheckPod         CheckPodFn     // 실제 검사 함수
    OverrideCheckIDs []CheckID      // 오버라이드할 baseline 체크 (Restricted만)
}

// 체크 함수 시그니처
type CheckPodFn func(podMetadata *metav1.ObjectMeta, podSpec *corev1.PodSpec) CheckResult

// 체크 결과
type CheckResult struct {
    Allowed         bool
    ForbiddenReason string  // 짧은 거부 사유 (예: "privileged")
    ForbiddenDetail string  // 상세 설명 (예: "container \"nginx\" must not set ...")
}
```

### init() 패턴을 통한 체크 등록

모든 체크는 `init()` 함수에서 `addCheck()`를 호출하여 전역 체크 목록에 등록된다.

```go
// 예: check_privileged.go
func init() {
    addCheck(CheckPrivileged)
}
```

`policy.DefaultChecks()`가 이 전역 목록을 반환하고, `NewEvaluator()`가 이를 받아 `checkRegistry`를 구축한다.

### visitContainers 헬퍼

대부분의 체크는 `visitContainers()` 헬퍼를 사용하여 init 컨테이너와 일반 컨테이너를 모두 순회한다.

```go
// 모든 containers + initContainers를 순회
visitContainers(podSpec, func(container *corev1.Container) {
    // 각 컨테이너에 대한 체크 로직
})
```

### 전체 19개 체크 총괄 테이블

| # | 체크 ID | 레벨 | 시작 버전 | 파일명 | 검사 대상 |
|---|---------|------|----------|--------|----------|
| 1 | privileged | Baseline | v1.0 | check_privileged.go | securityContext.privileged |
| 2 | capabilities_baseline | Baseline | v1.0 | check_capabilities_baseline.go | capabilities.add |
| 3 | hostNamespaces | Baseline | v1.0 | check_hostNamespaces.go | hostNetwork, hostPID, hostIPC |
| 4 | hostPorts | Baseline | v1.0 | check_hostPorts.go | ports[*].hostPort |
| 5 | hostPathVolumes | Baseline | v1.0 | check_hostPathVolumes.go | volumes[*].hostPath |
| 6 | seLinuxOptions | Baseline | v1.0 | check_seLinuxOptions.go | seLinuxOptions type/user/role |
| 7 | procMount | Baseline | v1.0 | check_procMount_baseline.go | procMount |
| 8 | seccompProfile_baseline | Baseline | v1.0 | check_seccompProfile_baseline.go | seccompProfile.type |
| 9 | appArmorProfile | Baseline | v1.0 | check_appArmorProfile.go | appArmorProfile.type |
| 10 | sysctls | Baseline | v1.0 | check_sysctls.go | securityContext.sysctls |
| 11 | windowsHostProcess | Baseline | v1.0 | check_windowsHostProcess.go | windowsOptions.hostProcess |
| 12 | hostProbesAndHostLifecycle | Baseline | v1.34 | check_hostProbesAndhostLifecycle.go | probes/lifecycle host 필드 |
| 13 | restrictedVolumes | Restricted | v1.0 | check_restrictedVolumes.go | 볼륨 타입 제한 |
| 14 | runAsNonRoot | Restricted | v1.0 | check_runAsNonRoot.go | runAsNonRoot |
| 15 | allowPrivilegeEscalation | Restricted | v1.8 | check_allowPrivilegeEscalation.go | allowPrivilegeEscalation |
| 16 | seccompProfile_restricted | Restricted | v1.19 | check_seccompProfile_restricted.go | seccompProfile 필수 설정 |
| 17 | capabilities_restricted | Restricted | v1.22 | check_capabilities_restricted.go | drop ALL, add 제한 |
| 18 | runAsUser | Restricted | v1.23 | check_runAsUser.go | runAsUser=0 금지 |
| 19 | procMount_restricted | Restricted | v1.35 | check_procMount_restricted.go | procMount 무조건 차단 |

---

## 7. Baseline Level 체크 목록

### 7.1 privileged (v1.0+)

특권 컨테이너를 금지한다. 특권 컨테이너는 호스트의 모든 장치에 접근할 수 있고, 대부분의 보안 메커니즘이 비활성화된다.

```go
// check_privileged.go (56-75행)
func privileged_1_0(podMetadata *metav1.ObjectMeta, podSpec *corev1.PodSpec) CheckResult {
    var badContainers []string
    visitContainers(podSpec, func(container *corev1.Container) {
        if container.SecurityContext != nil &&
           container.SecurityContext.Privileged != nil &&
           *container.SecurityContext.Privileged {
            badContainers = append(badContainers, container.Name)
        }
    })
    if len(badContainers) > 0 {
        return CheckResult{
            Allowed:         false,
            ForbiddenReason: "privileged",
            ForbiddenDetail: fmt.Sprintf(
                "%s %s must not set securityContext.privileged=true",
                pluralize("container", "containers", len(badContainers)),
                joinQuote(badContainers)),
        }
    }
    return CheckResult{Allowed: true}
}
```

| 필드 | 허용 값 |
|------|--------|
| `spec.containers[*].securityContext.privileged` | `false`, `undefined/null` |
| `spec.initContainers[*].securityContext.privileged` | `false`, `undefined/null` |

### 7.2 capabilities_baseline (v1.0+)

기본 세트를 넘는 capability 추가를 금지한다. NET_RAW 같은 위험한 capability 추가를 차단한다.

```go
// check_capabilities_baseline.go (61-76행)
var capabilities_allowed_1_0 = sets.NewString(
    "AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID",
    "KILL", "MKNOD", "NET_BIND_SERVICE", "SETFCAP", "SETGID",
    "SETPCAP", "SETUID", "SYS_CHROOT",
)
```

허용되는 13개 capability 외의 것을 `capabilities.add`에 추가하면 거부된다.

| 필드 | 허용 값 |
|------|--------|
| `capabilities.add` | 위 13개 기본 capability + `undefined/empty` |

### 7.3 hostNamespaces (v1.0+)

호스트 네트워크/PID/IPC 네임스페이스 공유를 금지한다. 호스트 네임스페이스를 공유하면 컨테이너 격리가 무력화된다.

```go
// check_hostNamespaces.go (58-82행)
func hostNamespaces_1_0(...) CheckResult {
    var hostNamespaces []string
    if podSpec.HostNetwork {
        hostNamespaces = append(hostNamespaces, "hostNetwork=true")
    }
    if podSpec.HostPID {
        hostNamespaces = append(hostNamespaces, "hostPID=true")
    }
    if podSpec.HostIPC {
        hostNamespaces = append(hostNamespaces, "hostIPC=true")
    }
    // hostNamespaces가 비어있지 않으면 Allowed: false
}
```

| 필드 | 허용 값 |
|------|--------|
| `spec.hostNetwork` | `false`, `undefined` |
| `spec.hostPID` | `false`, `undefined` |
| `spec.hostIPC` | `false`, `undefined` |

### 7.4 hostPorts (v1.0+)

호스트 포트 바인딩을 금지한다. hostPort를 사용하면 노드의 특정 포트를 점유하여 스케줄링 제약이 생긴다.

```go
// check_hostPorts.go (60-91행)
func hostPorts_1_0(...) CheckResult {
    visitContainers(podSpec, func(container *corev1.Container) {
        for _, c := range container.Ports {
            if c.HostPort != 0 {
                // ...
            }
        }
    })
}
```

| 필드 | 허용 값 |
|------|--------|
| `spec.containers[*].ports[*].hostPort` | `0`, `undefined` |

### 7.5 hostPathVolumes (v1.0+)

hostPath 볼륨 마운트를 금지한다. 호스트 파일시스템에 직접 접근하는 것은 컨테이너 탈출의 경로가 될 수 있다.

```go
// check_hostPathVolumes.go (58-76행)
func hostPathVolumes_1_0(...) CheckResult {
    for _, volume := range podSpec.Volumes {
        if volume.HostPath != nil {
            hostVolumes = append(hostVolumes, volume.Name)
        }
    }
}
```

| 필드 | 허용 값 |
|------|--------|
| `spec.volumes[*].hostPath` | `undefined/null` |

### 7.6 seLinuxOptions (v1.0+, v1.31+)

SELinux 옵션의 type을 제한하고, user/role 설정을 금지한다.

```go
// check_seLinuxOptions.go (77-80행)
var (
    selinuxAllowedTypes1_0  = sets.New("", "container_t", "container_init_t", "container_kvm_t")
    selinuxAllowedTypes1_31 = sets.New("", "container_t", "container_init_t",
                                        "container_kvm_t", "container_engine_t")
)
```

v1.31에서 `container_engine_t` 타입이 허용 목록에 추가되었다.

| 필드 | 허용 값 |
|------|--------|
| `seLinuxOptions.type` | `""`, `container_t`, `container_init_t`, `container_kvm_t` (v1.31+: `container_engine_t`) |
| `seLinuxOptions.user` | `""` (빈 문자열만 허용) |
| `seLinuxOptions.role` | `""` (빈 문자열만 허용) |

### 7.7 procMount (v1.0+, v1.35+)

/proc 파일시스템의 마스킹 해제를 금지한다. Unmasked procMount는 컨테이너 내에서 호스트 정보에 접근할 수 있게 한다.

```go
// check_procMount_baseline.go (66-106행)
func procMount_1_0(...) CheckResult {
    visitContainers(podSpec, func(container *corev1.Container) {
        if container.SecurityContext != nil &&
           container.SecurityContext.ProcMount != nil &&
           *container.SecurityContext.ProcMount != corev1.DefaultProcMount {
            badContainers = append(badContainers, container.Name)
        }
    })
}

// v1.35: User Namespace Pod에 대해 완화
func procMount1_35baseline(...) CheckResult {
    if relaxPolicyForUserNamespacePod(podSpec) {
        return CheckResult{Allowed: true}
    }
    return procMount_1_0(podMetadata, podSpec)
}
```

| 필드 | 허용 값 |
|------|--------|
| `securityContext.procMount` | `undefined/null`, `"Default"` |
| v1.35+ User Namespace Pod | 모든 값 허용 (baseline에서) |

### 7.8 seccompProfile_baseline (v1.0+, v1.19+)

seccomp 프로파일이 설정된 경우 안전한 값만 허용한다. Baseline에서는 미설정도 허용한다.

```go
// check_seccompProfile_baseline.go (75-78행)
func validSeccomp(t corev1.SeccompProfileType) bool {
    return t == corev1.SeccompProfileTypeLocalhost ||
        t == corev1.SeccompProfileTypeRuntimeDefault
}
```

| 버전 | 필드 | 허용 값 |
|------|------|--------|
| v1.0-v1.18 | annotation 기반 | `runtime/default`, `docker/default`, `localhost/*`, `undefined` |
| v1.19+ | `seccompProfile.type` | `RuntimeDefault`, `Localhost`, `undefined` |

### 7.9 appArmorProfile (v1.0+)

AppArmor 프로파일 타입을 제한한다. Unconfined 프로파일을 차단한다.

```go
// check_appArmorProfile.go (73-81행)
func allowedProfileType(profile corev1.AppArmorProfileType) bool {
    switch profile {
    case corev1.AppArmorProfileTypeRuntimeDefault,
        corev1.AppArmorProfileTypeLocalhost:
        return true
    default:
        return false
    }
}
```

| 필드 | 허용 값 |
|------|--------|
| `appArmorProfile.type` | `RuntimeDefault`, `Localhost`, `undefined` |
| beta annotation | `runtime/default`, `localhost/*`, `empty`, `undefined` |

### 7.10 sysctls (v1.0+, v1.27+, v1.29+, v1.32+)

안전한 sysctl만 허용한다. 버전에 따라 허용 목록이 확장된다.

```
허용 sysctl 목록 변화:

v1.0:  kernel.shm_rmid_forced
       net.ipv4.ip_local_port_range
       net.ipv4.tcp_syncookies
       net.ipv4.ping_group_range
       net.ipv4.ip_unprivileged_port_start

v1.27: + net.ipv4.ip_local_reserved_ports

v1.29: + net.ipv4.tcp_keepalive_time
       + net.ipv4.tcp_fin_timeout
       + net.ipv4.tcp_keepalive_intvl
       + net.ipv4.tcp_keepalive_probes

v1.32: + net.ipv4.tcp_rmem
       + net.ipv4.tcp_wmem
```

이 체크는 버전별 체크 시스템의 전형적인 예이다. 허용 sysctl 목록이 Union 연산으로 점진적으로 확장된다.

```go
// check_sysctls.go (86-106행)
var (
    sysctlsAllowedV1Dot0  = sets.NewString("kernel.shm_rmid_forced", ...)
    sysctlsAllowedV1Dot27 = sysctlsAllowedV1Dot0.Union(sets.NewString(
        "net.ipv4.ip_local_reserved_ports",
    ))
    sysctlsAllowedV1Dot29 = sysctlsAllowedV1Dot27.Union(sets.NewString(...))
    sysctlsAllowedV1Dot32 = sysctlsAllowedV1Dot29.Union(sets.NewString(...))
)
```

### 7.11 windowsHostProcess (v1.0+)

Windows HostProcess 컨테이너를 금지한다.

| 필드 | 허용 값 |
|------|--------|
| `windowsOptions.hostProcess` | `false`, `undefined` |

### 7.12 hostProbesAndHostLifecycle (v1.34+)

프로브와 라이프사이클 핸들러에서 host 필드 설정을 금지한다. v1.34에서 새로 추가된 체크이다. 프로브의 host 필드를 통해 노드 내부 서비스에 접근하는 것을 방지한다.

```go
// check_hostProbesAndhostLifecycle.go (63-73행)
func CheckHostProbesAndHostLifecycle() Check {
    return Check{
        ID:    "hostProbesAndHostLifecycle",
        Level: api.LevelBaseline,
        Versions: []VersionedCheck{
            {
                MinimumVersion: api.MajorMinorVersion(1, 34),
                CheckPod:       hostProbesAndHostLifecycleV1Dot34,
            },
        },
    }
}
```

검사 대상 필드:

| 필드 | 허용 값 |
|------|--------|
| `livenessProbe.httpGet.host` | `""`, `undefined` |
| `readinessProbe.httpGet.host` | `""`, `undefined` |
| `startupProbe.httpGet.host` | `""`, `undefined` |
| `livenessProbe.tcpSocket.host` | `""`, `undefined` |
| `readinessProbe.tcpSocket.host` | `""`, `undefined` |
| `startupProbe.tcpSocket.host` | `""`, `undefined` |
| `lifecycle.postStart.httpGet.host` | `""`, `undefined` |
| `lifecycle.preStop.httpGet.host` | `""`, `undefined` |
| `lifecycle.postStart.tcpSocket.host` | `""`, `undefined` |
| `lifecycle.preStop.tcpSocket.host` | `""`, `undefined` |

---

## 8. Restricted Level 체크 목록

Restricted 레벨은 위의 Baseline 체크 전부에 더해 아래 체크들을 추가로 적용한다. 일부는 Baseline 체크를 오버라이드한다.

### 8.1 restrictedVolumes (v1.0+) -- hostPathVolumes 오버라이드

안전한 볼륨 타입만 허용한다. Baseline의 `hostPathVolumes` 체크를 오버라이드하여 훨씬 넓은 범위를 차단한다.

```go
// check_restrictedVolumes.go (74-86행)
func CheckRestrictedVolumes() Check {
    return Check{
        ID:    "restrictedVolumes",
        Level: api.LevelRestricted,
        Versions: []VersionedCheck{
            {
                MinimumVersion:   api.MajorMinorVersion(1, 0),
                CheckPod:         restrictedVolumes_1_0,
                OverrideCheckIDs: []CheckID{checkHostPathVolumesID},
            },
        },
    }
}
```

허용 볼륨 타입과 차단 볼륨 타입:

| 허용 볼륨 타입 | 차단 볼륨 타입 (예시) |
|---------------|---------------------|
| configMap | hostPath |
| downwardAPI | gcePersistentDisk |
| emptyDir | awsElasticBlockStore |
| projected | nfs, iscsi, glusterfs |
| secret | rbd, flexVolume, cinder |
| csi | cephfs, flocker, fc |
| persistentVolumeClaim | azureFile, azureDisk |
| ephemeral | vsphereVolume, quobyte |
| image | portworxVolume, scaleIO, storageos 등 |

### 8.2 runAsNonRoot (v1.0+, v1.35+)

컨테이너가 비루트 사용자로 실행되도록 강제한다.

```go
// check_runAsNonRoot.go (78-144행)
func runAsNonRoot1_0(podMetadata *metav1.ObjectMeta, podSpec *corev1.PodSpec) CheckResult {
    podRunAsNonRoot := false
    if podSpec.SecurityContext != nil && podSpec.SecurityContext.RunAsNonRoot != nil {
        if !*podSpec.SecurityContext.RunAsNonRoot {
            badSetters = append(badSetters, "pod")
        } else {
            podRunAsNonRoot = true
        }
    }

    visitContainers(podSpec, func(container *corev1.Container) {
        if container.SecurityContext != nil && container.SecurityContext.RunAsNonRoot != nil {
            if !*container.SecurityContext.RunAsNonRoot {
                explicitlyBadContainers = append(...)
            }
        } else {
            if !podRunAsNonRoot {
                implicitlyBadContainers = append(...)
            }
        }
    })
}
```

핵심 로직: Pod 레벨에서 `runAsNonRoot=true`이면 개별 컨테이너에서는 설정하지 않아도 된다. 하지만 Pod 레벨에서도 미설정이고 컨테이너에서도 미설정이면 "암시적 위반"으로 거부된다.

v1.35부터는 User Namespace Pod (`hostUsers: false`)에 대해 이 체크를 완화한다.

| 필드 | 허용 값 |
|------|--------|
| `runAsNonRoot` | `true` (pod 레벨 또는 모든 컨테이너에서) |

### 8.3 allowPrivilegeEscalation (v1.8+, v1.25+)

SUID/SGID 비트를 통한 권한 상승을 차단한다. 이 필드는 반드시 명시적으로 `false`로 설정해야 한다.

```go
// check_allowPrivilegeEscalation.go (64-83행)
func allowPrivilegeEscalation_1_8(...) CheckResult {
    visitContainers(podSpec, func(container *corev1.Container) {
        if container.SecurityContext == nil ||
           container.SecurityContext.AllowPrivilegeEscalation == nil ||
           *container.SecurityContext.AllowPrivilegeEscalation {
            badContainers = append(badContainers, container.Name)
        }
    })
}
```

`nil`이면 기본값 `true`이므로 명시적으로 `false`를 설정해야만 통과한다. v1.25부터 Windows Pod는 이 체크에서 면제된다.

| 필드 | 허용 값 |
|------|--------|
| `allowPrivilegeEscalation` | `false` (명시적 설정 필수) |

### 8.4 seccompProfile_restricted (v1.19+, v1.25+) -- seccompBaseline 오버라이드

seccomp 프로파일을 반드시 설정하도록 강제한다. Baseline은 미설정을 허용했지만, Restricted는 반드시 `RuntimeDefault` 또는 `Localhost`로 설정해야 한다.

```go
// check_seccompProfile_restricted.go (48-66행)
func CheckSeccompProfileRestricted() Check {
    return Check{
        ID:    "seccompProfile_restricted",
        Level: api.LevelRestricted,
        Versions: []VersionedCheck{
            {
                MinimumVersion:   api.MajorMinorVersion(1, 19),
                CheckPod:         seccompProfileRestricted_1_19,
                OverrideCheckIDs: []CheckID{checkSeccompBaselineID},
            },
            {
                MinimumVersion:   api.MajorMinorVersion(1, 25),
                CheckPod:         seccompProfileRestricted_1_25,
                OverrideCheckIDs: []CheckID{checkSeccompBaselineID},
            },
        },
    }
}
```

Baseline과의 차이: Baseline은 "설정했다면 안전한 값이어야 한다"이고, Restricted는 "반드시 설정해야 하고 안전한 값이어야 한다"이다.

| 필드 | 허용 값 |
|------|--------|
| `seccompProfile.type` | `RuntimeDefault`, `Localhost` (반드시 설정 필수) |

### 8.5 capabilities_restricted (v1.22+, v1.25+) -- capabilitiesBaseline 오버라이드

모든 capability를 드롭하고, `NET_BIND_SERVICE`만 추가 허용한다.

```go
// check_capabilities_restricted.go (79-136행)
func capabilitiesRestricted_1_22(...) CheckResult {
    visitContainers(podSpec, func(container *corev1.Container) {
        // capabilities.drop에 "ALL"이 포함되어야 함
        droppedAll := false
        for _, c := range container.SecurityContext.Capabilities.Drop {
            if c == capabilityAll {
                droppedAll = true
                break
            }
        }
        if !droppedAll {
            containersMissingDropAll = append(...)
        }

        // capabilities.add에 NET_BIND_SERVICE 외에는 불허
        for _, c := range container.SecurityContext.Capabilities.Add {
            if c != capabilityNetBindService {
                containersAddingForbidden = append(...)
            }
        }
    })
}
```

Baseline과의 차이: Baseline은 "기본 13개 capability 외의 것을 add하지 마라"이고, Restricted는 "반드시 ALL을 drop하고, NET_BIND_SERVICE만 add할 수 있다"이다.

| 필드 | 허용 값 |
|------|--------|
| `capabilities.drop` | 반드시 `["ALL"]` 포함 |
| `capabilities.add` | `undefined/empty` 또는 `["NET_BIND_SERVICE"]`만 |

### 8.6 runAsUser (v1.23+, v1.35+)

UID 0(root)으로의 명시적 실행을 금지한다.

```go
// check_runAsUser.go (79-116행)
func runAsUser1_23(...) CheckResult {
    if podSpec.SecurityContext != nil &&
       podSpec.SecurityContext.RunAsUser != nil &&
       *podSpec.SecurityContext.RunAsUser == 0 {
        badSetters = append(badSetters, "pod")
    }
    visitContainers(podSpec, func(container *corev1.Container) {
        if container.SecurityContext != nil &&
           container.SecurityContext.RunAsUser != nil &&
           *container.SecurityContext.RunAsUser == 0 {
            explicitlyBadContainers = append(...)
        }
    })
}
```

`runAsNonRoot`와의 차이: `runAsNonRoot`는 "비루트 실행을 강제"하고, `runAsUser`는 "명시적으로 UID 0을 지정한 경우만 차단"한다. `runAsUser: 1000`은 허용되고, 미지정도 허용된다.

v1.35부터 User Namespace Pod에 대해 완화된다.

| 필드 | 허용 값 |
|------|--------|
| `runAsUser` | 0이 아닌 값, `undefined/null` |

### 8.7 procMount_restricted (v1.35+) -- procMount 오버라이드

v1.35에서 Baseline의 procMount 체크가 User Namespace Pod에 대해 완화되면서, Restricted에서는 여전히 무조건 차단하는 별도 체크가 추가되었다.

```go
// check_procMount_restricted.go (40-56행)
func CheckProcMountRestricted() Check {
    return Check{
        ID:    "procMount_restricted",
        Level: api.LevelRestricted,
        Versions: []VersionedCheck{
            {
                MinimumVersion:   api.MajorMinorVersion(1, 35),
                CheckPod:         procMount_1_0,  // 무조건 차단 버전 재사용
                OverrideCheckIDs: []CheckID{"procMount"},
            },
        },
    }
}
```

이 체크는 `procMount_1_0` 함수를 재사용한다. Baseline에서는 v1.35부터 User Namespace Pod를 완화했지만, Restricted에서는 항상 엄격하게 유지한다.

### 오버라이드 관계 요약

```
Restricted 오버라이드 관계:

Baseline 체크               Restricted 오버라이드 체크         버전
-----------------------     -----------------------------------  ------
hostPathVolumes        <--- restrictedVolumes                   v1.0+
capabilities_baseline  <--- capabilities_restricted             v1.22+
seccompProfile_baseline <-- seccompProfile_restricted           v1.19+
procMount              <--- procMount_restricted                v1.35+
```

---

## 9. Exemption 시스템

PSA는 세 가지 면제(Exemption) 메커니즘을 제공한다. 면제는 Configuration에서 정적으로 설정되며 런타임에 변경할 수 없다.

### 9.1 네임스페이스 면제

```go
// admission.go (673-679행)
func (a *Admission) exemptNamespace(namespace string) bool {
    if len(namespace) == 0 {
        return false
    }
    return containsString(namespace, a.Configuration.Exemptions.Namespaces)
}
```

특정 네임스페이스(예: `kube-system`, `kube-public`)를 정책 검사에서 완전히 면제한다. 시스템 컴포넌트가 실행되는 네임스페이스에 필수적이다.

### 9.2 사용자 면제

```go
// admission.go (680-686행)
func (a *Admission) exemptUser(username string) bool {
    if len(username) == 0 {
        return false
    }
    return containsString(username, a.Configuration.Exemptions.Usernames)
}
```

특정 사용자(예: 시스템 서비스 계정, CI/CD 파이프라인 계정)의 요청을 면제한다.

### 9.3 RuntimeClass 면제

```go
// admission.go (687-693행)
func (a *Admission) exemptRuntimeClass(runtimeClass *string) bool {
    if runtimeClass == nil || len(*runtimeClass) == 0 {
        return false
    }
    return containsString(*runtimeClass, a.Configuration.Exemptions.RuntimeClasses)
}
```

특정 RuntimeClass(예: `gvisor`, `kata-containers`)를 사용하는 Pod를 면제한다. 이런 런타임은 자체적으로 강력한 격리를 제공하므로 PSA 체크가 불필요할 수 있다.

### 면제 검사 순서

면제 검사는 비용이 적은 것부터 순서대로 수행된다.

```
요청 도착
    |
    +-- exemptNamespace()?  --> Yes --> 즉시 허용 (RecordExemption)
    |
    +-- exemptUser()?       --> Yes --> 즉시 허용 (RecordExemption)
    |
    +-- FullyPrivileged()?  --> Yes --> 즉시 허용
    |
    +-- [Pod 디코딩]
    |
    +-- exemptRuntimeClass()? -> Yes --> 즉시 허용 (RecordExemption)
    |
    +-- 체크 수행
```

`exemptRuntimeClass`는 Pod 디코딩 후에야 확인할 수 있으므로 가장 마지막에 검사된다 (`EvaluatePod()` 진입 직후).

면제가 적용되면 해당 사실이 메트릭에 기록된다(`RecordExemption`). 이를 통해 운영자는 면제가 남용되고 있는지 모니터링할 수 있다.

### 면제 네임스페이스 경고

면제된 네임스페이스에 비-Privileged 정책 라벨이 설정되면 경고를 반환한다. 이는 관리자가 의도하지 않은 설정을 감지할 수 있게 한다.

```go
// admission.go (735-738행)
func (a *Admission) exemptNamespaceWarning(exemptNamespace string,
    policy api.Policy, nsLabels map[string]string) string {
    if policy.FullyPrivileged() || policy.Equivalent(&a.defaultPolicy) {
        return ""
    }
    // 면제 네임스페이스에 정책 라벨이 있지만 면제되어 적용되지 않는다는 경고 반환
}
```

---

## 10. Kubernetes Plugin 통합

### Plugin 구조체

PSA는 kube-apiserver의 Admission Plugin 프레임워크에 통합된다.

```go
// plugin/pkg/admission/security/podsecurity/admission.go (68-80행)
type Plugin struct {
    *admission.Handler

    inspectedEffectiveVersion bool
    emulationVersion          *podsecurityadmissionapi.Version

    client          kubernetes.Interface
    namespaceLister corev1listers.NamespaceLister
    podLister       corev1listers.PodLister

    delegate *podsecurityadmission.Admission
}
```

`Plugin`은 `admission.ValidationInterface`를 구현하며, 실제 로직은 `delegate` 필드의 `Admission` 구조체에 위임한다. 이 패턴은 PSA 로직을 kube-apiserver 외부에서도 독립적으로 사용할 수 있게 한다.

### Plugin 등록

```go
// plugin/pkg/admission/security/podsecurity/admission.go (62-66행)
const PluginName = "PodSecurity"

func Register(plugins *admission.Plugins) {
    plugins.Register(PluginName, func(reader io.Reader) (admission.Interface, error) {
        return newPlugin(reader)
    })
}
```

kube-apiserver가 시작할 때 이 플러그인을 자동으로 로드한다.

### 초기화 과정

```go
// plugin/pkg/admission/security/podsecurity/admission.go (100-115행)
func newPlugin(reader io.Reader) (*Plugin, error) {
    config, err := podsecurityconfigloader.LoadFromReader(reader)
    return &Plugin{
        Handler: admission.NewHandler(admission.Create, admission.Update),
        delegate: &podsecurityadmission.Admission{
            Configuration:    config,
            Metrics:          getDefaultRecorder(),
            PodSpecExtractor: podsecurityadmission.DefaultPodSpecExtractor{},
        },
    }, nil
}
```

`updateDelegate()`는 모든 의존성(namespaceLister, podLister, client, effectiveVersion)이 준비되면 `Admission` 구조체를 완전히 초기화한다.

```go
// plugin/pkg/admission/security/podsecurity/admission.go (132-159행)
func (p *Plugin) updateDelegate() {
    if p.namespaceLister == nil { return }
    if p.podLister == nil { return }
    if p.client == nil { return }
    if !p.inspectedEffectiveVersion { return }

    if p.delegate.Evaluator == nil {
        evaluator, err := policy.NewEvaluator(policy.DefaultChecks(), p.emulationVersion)
        if err != nil {
            panic(fmt.Errorf("could not create PodSecurityRegistry: %w", err))
        }
        p.delegate.Evaluator = evaluator
    }
}
```

`policy.DefaultChecks()`가 19개 체크를 모두 수집하고, `NewEvaluator()`가 `checkRegistry`를 구축한다.

### Emulation Version

```go
// plugin/pkg/admission/security/podsecurity/admission.go (161-170행)
func (p *Plugin) InspectEffectiveVersion(version compatibility.EffectiveVersion) {
    p.inspectedEffectiveVersion = true
    binaryVersion := version.BinaryVersion()
    emulationVersion := version.EmulationVersion()
    binaryMajorMinor := podsecurityadmissionapi.MajorMinorVersion(
        int(binaryVersion.Major()), int(binaryVersion.Minor()))
    emulationMajorMinor := podsecurityadmissionapi.MajorMinorVersion(
        int(emulationVersion.Major()), int(emulationVersion.Minor()))
    if binaryMajorMinor != emulationMajorMinor {
        p.emulationVersion = &emulationMajorMinor
    }
}
```

에뮬레이션 버전을 통해 클러스터가 이전 버전의 정책으로 동작하게 할 수 있다. 업그레이드 전 호환성 테스트에 유용하다.

### Plugin의 Validate 메서드

```go
// plugin/pkg/admission/security/podsecurity/admission.go (193-233행)
func (p *Plugin) Validate(ctx context.Context, a admission.Attributes,
    o admission.ObjectInterfaces) error {

    gr := a.GetResource().GroupResource()
    if !applicableResources[gr] && !p.delegate.PodSpecExtractor.HasPodSpec(gr) {
        return nil  // 관련 없는 리소스는 통과
    }

    // lazyConvertingAttributes로 감싸서 내부 타입 -> 외부 v1 타입 변환
    result := p.delegate.Validate(ctx, &lazyConvertingAttributes{Attributes: a})

    // Warning 헤더 추가
    for _, w := range result.Warnings {
        warning.AddWarning(ctx, "", w)
    }
    // Audit annotation 추가
    if len(result.AuditAnnotations) > 0 {
        audit.AddAuditAnnotations(ctx, annotations...)
    }
    // 거부 시 에러 반환
    if !result.Allowed {
        retval := admission.NewForbidden(a, errors.New("Not allowed by PodSecurity"))
        if result.Result != nil {
            if len(result.Result.Message) > 0 {
                retval.ErrStatus.Message = result.Result.Message
            }
        }
        return retval
    }
    return nil
}
```

### Lazy Converting Attributes

Plugin은 kube-apiserver 내부 타입(예: `core.Pod`)을 외부 타입(예: `corev1.Pod`)으로 변환해야 한다. `lazyConvertingAttributes`는 이 변환을 지연(lazy) 실행하여 불필요한 변환을 방지한다.

```go
// plugin/pkg/admission/security/podsecurity/admission.go (235-259행)
type lazyConvertingAttributes struct {
    admission.Attributes
    convertObjectOnce    sync.Once
    convertedObject      runtime.Object
    convertedObjectError error
}

func (l *lazyConvertingAttributes) GetObject() (runtime.Object, error) {
    l.convertObjectOnce.Do(func() {
        l.convertedObject, l.convertedObjectError = convert(l.Attributes.GetObject())
    })
    return l.convertedObject, l.convertedObjectError
}
```

`convert()` 함수는 `legacyscheme.Scheme.Convert()`를 사용하여 내부 타입을 외부 v1 타입으로 변환한다.

```
Plugin 통합 아키텍처:

+--------------------------------------------------+
|                kube-apiserver                      |
|                                                    |
|  +---------------------------------------------+  |
|  |         Admission Chain                       |  |
|  |                                               |  |
|  |  MutatingWebhook -> ... -> Plugin(PSA) -> ... |  |
|  |                             |                 |  |
|  |                    +--------v---------+       |  |
|  |                    | lazyConverting   |       |  |
|  |                    | Attributes       |       |  |
|  |                    +--------+---------+       |  |
|  |                             |                 |  |
|  |                    +--------v---------+       |  |
|  |                    |   Admission      |       |  |
|  |                    |   (delegate)     |       |  |
|  |                    +--------+---------+       |  |
|  |                             |                 |  |
|  |                    +--------v---------+       |  |
|  |                    |  checkRegistry   |       |  |
|  |                    |  (Evaluator)     |       |  |
|  |                    +------------------+       |  |
|  +---------------------------------------------+  |
|                                                    |
|  Informer Cache: Namespace, Pod                    |
+--------------------------------------------------+
```

---

## 11. 왜 이런 설계인가

### 11.1 왜 세 가지 레벨인가?

PSP는 무한한 커스텀 정책을 허용했지만, 이는 관리 복잡성을 기하급수적으로 증가시켰다. PSA는 의도적으로 세 가지 레벨로 제한하여:

- **표준화**: 모든 클러스터에서 동일한 보안 기준선을 공유한다. "우리 클러스터는 baseline을 적용합니다"라고 말하면 누구나 그 의미를 안다.
- **이해 가능성**: 각 레벨의 의미를 팀원 누구나 이해할 수 있다. PSP처럼 "이 정책이 정확히 뭘 허용하는지" 분석할 필요가 없다.
- **점진적 적용**: Privileged -> Baseline -> Restricted 순서로 단계적으로 보안을 강화할 수 있다.

### 11.2 왜 Namespace 라벨인가?

라벨 기반 설정의 장점:

- **RBAC 분리**: Namespace 관리자가 직접 보안 정책을 설정할 수 있다. 별도의 ClusterRole 없이 Namespace 수정 권한만 있으면 된다.
- **GitOps 친화적**: Namespace 매니페스트에 라벨을 포함하여 선언적으로 관리한다. 별도의 CRD나 리소스가 필요 없다.
- **즉시 적용**: 라벨 변경 시 새로운 Pod부터 즉시 적용된다 (기존 Pod는 영향 없음).
- **단순성**: PSP의 "ClusterRole + RoleBinding + PodSecurityPolicy 리소스" 조합 대신 라벨 하나로 충분하다.

### 11.3 왜 Enforce/Audit/Warn 세 모드인가?

```
마이그레이션 시나리오:

1단계: 현재 상태 파악 (Audit/Warn만)
  pod-security.kubernetes.io/audit: baseline
  pod-security.kubernetes.io/warn: baseline
  -> 기존 워크로드는 영향 없음, 위반만 기록/경고

2단계: 위반 워크로드 수정
  -> Audit 로그와 Warning을 보고 위반 Pod를 수정

3단계: Enforce 활성화
  pod-security.kubernetes.io/enforce: baseline
  -> 새로운 위반 Pod 차단 시작

4단계: 다음 레벨 준비
  pod-security.kubernetes.io/audit: restricted
  pod-security.kubernetes.io/warn: restricted
  -> restricted 위반 현황 파악
```

이 설계는 운영 중인 클러스터에서 "무중단 보안 강화"를 가능하게 한다. 세 모드를 독립적으로 설정할 수 있어 점진적 전환이 가능하다.

### 11.4 왜 버전별 체크인가?

Kubernetes는 마이너 버전마다 새로운 보안 체크를 추가할 수 있다. 버전별 체크 시스템이 없다면:

- 클러스터를 v1.27에서 v1.29로 업그레이드하면 `tcp_keepalive` 관련 sysctl이 갑자기 차단될 수 있음
- `enforce-version: v1.27`로 고정하면, v1.29 체크가 추가되어도 v1.27 기준으로 평가
- `latest`로 설정하면 항상 최신 체크 적용 (보안 최우선 환경)

이 시스템은 Kubernetes 전체에서 활용되는 "Feature Gate"와 유사한 점진적 도입 패턴이다.

### 11.5 왜 오버라이드 시스템인가?

Restricted 레벨은 Baseline의 일부 체크를 더 엄격한 버전으로 대체해야 한다. 오버라이드 없이 두 체크를 모두 실행하면:

- 동일 필드에 대한 중복 에러 메시지 발생 (예: capabilities에 대해 "기본 외 capability 추가됨" + "ALL drop 필요")
- 사용자 혼란 야기
- 불필요한 연산 비용

오버라이드를 통해 `capabilities_baseline`과 `capabilities_restricted`가 Restricted 레벨에서 동시에 실행되지 않고, 더 엄격한 restricted 버전만 실행된다.

### 11.6 왜 뮤테이션이 없는가?

PSP는 Pod를 수정(mutate)할 수 있었다 (예: 자동으로 `runAsNonRoot: true` 추가). PSA는 순수 검증(validation)만 수행한다.

- **예측 가능성**: Pod 사양이 자동으로 변경되지 않으므로 "내가 정의한 것"과 "실제 적용된 것"이 일치한다.
- **디버깅 용이성**: PSP에서는 "왜 내 Pod에 이 securityContext가 추가되었는지" 추적이 어려웠다.
- **선언적 모델**: 사용자가 명시한 것만 적용된다.
- **단순성**: Admission 로직이 단순해져 버그 가능성이 감소한다.

### 11.7 왜 Pod Controller도 검사하는가?

Deployment를 생성할 때 Audit/Warn을 통해 미리 알려주지 않으면, 실제 Pod가 생성될 때서야 Enforce에 의해 거부된다. ReplicaSet이 Pod 생성에 실패하고 이유를 알기 어려운 상황이 발생한다.

Pod Controller 검사를 통해 "이 Deployment로 생성될 Pod는 정책을 위반합니다"라는 경고를 미리 제공한다. 다만, Pod Controller에 대해서는 `enforce=false`로 호출하여 Enforce를 적용하지 않는다. 실제 차단은 Pod 수준에서만 발생한다.

### 11.8 왜 Lazy Converting인가?

kube-apiserver는 내부적으로 버전 없는(versionless) 타입을 사용하지만, PSA 로직은 외부 v1 타입으로 동작한다. 모든 요청마다 즉시 변환하면 불필요한 오버헤드가 발생한다.

- 면제 네임스페이스나 사용자의 요청은 변환 없이 즉시 허용
- FullyPrivileged 네임스페이스도 변환 없이 통과
- 실제 체크가 필요한 경우에만 `sync.Once`를 통해 한 번만 변환

---

## 12. 정리

### 아키텍처 요약

```
+----------------------------------------------------------+
|                   Pod Security Admission                   |
|                                                            |
|  +---------+    +----------+    +--------------------+    |
|  | Plugin  +--->| Admission+--->|  checkRegistry     |    |
|  | (kube-  |    |          |    |  (Evaluator)       |    |
|  | apiserver)   | Validate |    |                    |    |
|  |  통합)   |    | Exempt   |    |  baselineChecks    |    |
|  |         |    | Evaluate |    |  restrictedChecks  |    |
|  +---------+    +----------+    +--------------------+    |
|                                          |                 |
|                               +----------+----------+     |
|                               |    19개 체크 함수     |     |
|                               |                      |     |
|                               |  Baseline (12개)     |     |
|                               |  Restricted (7개)    |     |
|                               |  + 오버라이드 4쌍    |     |
|                               +----------------------+     |
|                                                            |
|  설정: Namespace 라벨 (enforce/audit/warn + version)        |
|  면제: Namespace / User / RuntimeClass                      |
+----------------------------------------------------------+
```

### 핵심 설계 원칙

| 원칙 | 구현 |
|------|------|
| 단순성 | 3 레벨, 3 모드, Namespace 라벨 설정 |
| 점진적 적용 | Audit/Warn으로 시작 -> Enforce로 전환 |
| 버전 안전성 | 버전별 체크로 업그레이드 시 파괴적 변경 방지 |
| 확장성 | 새 체크를 새 버전에 추가하면 자동 인플레이트 |
| 성능 | 결과 캐싱, 단축 경로(short-circuit), 지연 변환 |
| 호환성 | Emulation Version으로 이전 버전 정책 에뮬레이션 |

### 주요 소스 파일 경로

| 파일 | 핵심 내용 |
|------|----------|
| `staging/.../pod-security-admission/api/constants.go` | Level 타입, Namespace 라벨 상수 |
| `staging/.../pod-security-admission/api/helpers.go` | Version, Policy, LevelVersion 타입 |
| `staging/.../pod-security-admission/policy/checks.go` | Check, VersionedCheck, CheckResult 타입 |
| `staging/.../pod-security-admission/policy/registry.go` | Evaluator, checkRegistry, 버전 인플레이션 |
| `staging/.../pod-security-admission/policy/check_*.go` | 19개 체크 구현 (12 Baseline + 7 Restricted) |
| `staging/.../pod-security-admission/admission/admission.go` | Admission, Validate, EvaluatePod |
| `plugin/pkg/admission/security/podsecurity/admission.go` | kube-apiserver Plugin 통합 |
