# 18. 빌드 코드 생성 심화

## 목차
1. [개요](#1-개요)
2. [Makefile 빌드 시스템](#2-makefile-빌드-시스템)
3. [staging/ 메커니즘과 go.work](#3-staging-메커니즘과-gowork)
4. [코드 생성 도구 전체 목록](#4-코드-생성-도구-전체-목록)
5. [deepcopy-gen](#5-deepcopy-gen)
6. [client-gen](#6-client-gen)
7. [informer-gen과 lister-gen](#7-informer-gen과-lister-gen)
8. [defaulter-gen과 conversion-gen](#8-defaulter-gen과-conversion-gen)
9. [기타 코드 생성 도구](#9-기타-코드-생성-도구)
10. [hack/ 스크립트 체계](#10-hack-스크립트-체계)
11. [코드 생성 태그 시스템](#11-코드-생성-태그-시스템)
12. [생성된 파일 패턴](#12-생성된-파일-패턴)
13. [CI/CD 파이프라인](#13-cicd-파이프라인)
14. [설계 원칙: Why](#14-설계-원칙-why)

---

## 1. 개요

Kubernetes는 약 **230만 줄 이상의 Go 코드**로 구성된 대규모 프로젝트이다. 이 중 상당 부분은 **자동 생성된 코드**이다. 코드 생성은 Kubernetes의 API 타입 안전성, 일관성, 생산성을 보장하는 핵심 메커니즘이다.

### 코드 생성이 필요한 이유

Go 언어의 한계로 인해 많은 보일러플레이트 코드가 필요하다:
- Go에는 제네릭이 제한적 (1.18+에서 추가되었지만, Kubernetes는 역사적으로 코드 생성 사용)
- 런타임 리플렉션 대신 컴파일 타임 타입 안전성 선호
- 수백 개의 API 타입에 대해 동일한 패턴의 코드가 반복

### 핵심 소스 파일

| 파일/디렉토리 | 역할 |
|-------------|------|
| `Makefile` | 최상위 빌드 타겟 |
| `hack/make-rules/build.sh` | 실제 빌드 로직 |
| `hack/update-codegen.sh` | 코드 생성 통합 스크립트 |
| `hack/verify-codegen.sh` | 코드 생성 검증 |
| `go.work` | workspace 모듈 관리 |
| `staging/src/k8s.io/code-generator/cmd/` | 코드 생성 도구 소스 |

---

## 2. Makefile 빌드 시스템

### 최상위 Makefile

**소스 위치**: `Makefile` (라인 1-98)

```makefile
# Makefile:33
SHELL := /usr/bin/env bash -o errexit -o pipefail -o nounset
BASH_ENV := ./hack/lib/logging.sh

# Makefile:51
OUT_DIR ?= _output
BIN_DIR := $(OUT_DIR)/bin

# Makefile:65
KUBE_VERBOSE ?= 1
```

### 주요 빌드 타겟

**소스 위치**: `Makefile` (라인 67-280)

| 타겟 | 명령 | 설명 |
|------|------|------|
| `all` | `hack/make-rules/build.sh $(WHAT)` | 코드 빌드 |
| `test` | `hack/make-rules/test.sh $(WHAT)` | 단위 테스트 |
| `test-integration` | `hack/make-rules/test-integration.sh` | 통합 테스트 |
| `test-e2e-node` | `hack/make-rules/test-e2e-node.sh` | 노드 e2e 테스트 |
| `verify` | `hack/make-rules/verify.sh` | 사전 검증 |
| `update` | `hack/make-rules/update.sh` | 생성 코드 업데이트 |

### 빌드 예시

```bash
# 전체 빌드
make all

# 특정 바이너리만 빌드
make all WHAT=cmd/kubelet GOFLAGS=-v

# 디버그 빌드 (최적화 비활성화)
make all DBG=1

# 테스트 실행
make test WHAT=./pkg/kubelet

# 코드 검증
make verify

# 코드 생성 업데이트
make update
```

### 빌드 흐름

```
make all WHAT=cmd/kubelet
    |
    v
hack/make-rules/build.sh cmd/kubelet
    |
    v
hack/lib/golang.sh (환경 설정)
    |
    +-- GOFLAGS 설정
    +-- LDFLAGS 설정 (버전 정보 임베딩)
    +-- 크로스 컴파일 설정
    |
    v
go build -o _output/bin/kubelet ./cmd/kubelet
    |
    v
_output/bin/kubelet (바이너리 출력)
```

### WHAT 변수

```makefile
# Makefile:67-78
define ALL_HELP_INFO
# Build code.
#
# Args:
#   WHAT: Directory or Go package names to build.  If any of these
#   directories has a 'main' package, the build will produce
#   executable files under $(OUT_DIR)/bin.
#     "vendor/<module>/<path>" is accepted as alias for "<module>/<path>".
#     "ginkgo" is an alias for the ginkgo CLI.
endef
```

### 검증 시스템

```makefile
# Makefile:127-137
.PHONY: verify
verify:
    KUBE_VERIFY_GIT_BRANCH=$(BRANCH) hack/make-rules/verify.sh

# Makefile:150-152
.PHONY: quick-verify
quick-verify:
    QUICK=true SILENT=false hack/make-rules/verify.sh
```

---

## 3. staging/ 메커니즘과 go.work

### staging/ 디렉토리란?

Kubernetes는 모노레포이지만, 일부 패키지를 독립적인 Go 모듈로 게시해야 한다. `staging/` 디렉토리는 이 **"모노레포 내 독립 모듈"** 패턴을 구현한다.

### staging/ 하위 모듈 목록

```
staging/src/k8s.io/
├── api                     # API 타입 정의 → k8s.io/api
├── apiextensions-apiserver # CRD 서버 → k8s.io/apiextensions-apiserver
├── apimachinery            # API 기계 → k8s.io/apimachinery
├── apiserver               # 공통 API 서버 → k8s.io/apiserver
├── cli-runtime             # CLI 런타임 → k8s.io/cli-runtime
├── client-go               # Go 클라이언트 → k8s.io/client-go
├── cloud-provider          # 클라우드 프로바이더 → k8s.io/cloud-provider
├── cluster-bootstrap       # 클러스터 부트스트랩 → k8s.io/cluster-bootstrap
├── code-generator          # 코드 생성 도구 → k8s.io/code-generator
├── component-base          # 컴포넌트 공통 → k8s.io/component-base
├── component-helpers       # 컴포넌트 헬퍼 → k8s.io/component-helpers
├── controller-manager      # 컨트롤러 매니저 → k8s.io/controller-manager
├── cri-api                 # CRI API → k8s.io/cri-api
├── cri-client              # CRI 클라이언트 → k8s.io/cri-client
├── csi-translation-lib     # CSI 변환 → k8s.io/csi-translation-lib
├── dynamic-resource-allocation # DRA → k8s.io/dynamic-resource-allocation
├── endpointslice           # 엔드포인트슬라이스 → k8s.io/endpointslice
├── externaljwt             # 외부 JWT → k8s.io/externaljwt
├── kms                     # KMS → k8s.io/kms
├── kube-aggregator         # API Aggregator → k8s.io/kube-aggregator
├── kube-controller-manager # 컨트롤러 매니저 → k8s.io/kube-controller-manager
├── kube-proxy              # kube-proxy → k8s.io/kube-proxy
├── kube-scheduler          # 스케줄러 → k8s.io/kube-scheduler
├── kubectl                 # kubectl → k8s.io/kubectl
├── kubelet                 # kubelet API → k8s.io/kubelet
├── metrics                 # 메트릭 API → k8s.io/metrics
├── mount-utils             # 마운트 유틸리티 → k8s.io/mount-utils
├── pod-security-admission  # PSA → k8s.io/pod-security-admission
├── sample-apiserver        # 샘플 API 서버 → k8s.io/sample-apiserver
├── sample-cli-plugin       # 샘플 CLI 플러그인 → k8s.io/sample-cli-plugin
└── sample-controller       # 샘플 컨트롤러 → k8s.io/sample-controller
```

### go.work 파일

**소스 위치**: `go.work` (최상위)

```go
// go.work (자동 생성)
go 1.25.0

godebug default=go1.25

use (
    .
    ./staging/src/k8s.io/api
    ./staging/src/k8s.io/apiextensions-apiserver
    ./staging/src/k8s.io/apimachinery
    ./staging/src/k8s.io/apiserver
    ./staging/src/k8s.io/cli-runtime
    ./staging/src/k8s.io/client-go
    ./staging/src/k8s.io/cloud-provider
    ./staging/src/k8s.io/cluster-bootstrap
    ./staging/src/k8s.io/code-generator
    ./staging/src/k8s.io/component-base
    // ... 30개 이상의 모듈
)
```

### staging 게시 프로세스

```
Kubernetes 모노레포
    |
    +-- staging/src/k8s.io/client-go/ (소스)
    |
    +-- 릴리스 시 publishing-bot이
    |   staging/src/k8s.io/client-go/ 를
    |   github.com/kubernetes/client-go 로 동기화
    |
    v
k8s.io/client-go (독립 모듈)
  → 외부 프로젝트에서 go get k8s.io/client-go 로 사용
```

### go.work의 역할

```
+---모노레포 개발 시-----------------------------------------+
|                                                            |
|  go.work가 staging/ 모듈들을 로컬 경로로 매핑              |
|                                                            |
|  import "k8s.io/client-go/..."                             |
|    → ./staging/src/k8s.io/client-go/ (로컬 경로)           |
|                                                            |
|  장점:                                                     |
|  - 모노레포에서 즉시 변경사항 반영                          |
|  - 별도의 replace 지시어 불필요                             |
|  - 순환 의존성 방지                                        |
+------------------------------------------------------------+

+---외부 프로젝트 사용 시-------------------------------------+
|                                                            |
|  go get k8s.io/client-go@v0.30.0                           |
|    → github.com/kubernetes/client-go 에서 다운로드          |
|                                                            |
|  go.work 불필요 (일반 Go 모듈)                              |
+------------------------------------------------------------+
```

---

## 4. 코드 생성 도구 전체 목록

### staging/src/k8s.io/code-generator/cmd/ 하위 도구

```
staging/src/k8s.io/code-generator/cmd/
├── applyconfiguration-gen   # Apply Configuration 타입 생성
├── client-gen               # 타입별 클라이언트셋 생성
├── conversion-gen           # 버전 간 변환 함수 생성
├── deepcopy-gen             # DeepCopy 메서드 생성
├── defaulter-gen            # 기본값 설정 함수 생성
├── go-to-protobuf           # Go → Protobuf 변환
├── informer-gen             # Informer 생성
├── lister-gen               # Lister 생성
├── prerelease-lifecycle-gen # API 버전 수명 태그 생성
├── register-gen             # 스키마 등록 함수 생성
└── validation-gen           # 유효성 검증 함수 생성
```

### 도구별 역할 요약

| 도구 | 입력 | 출력 | 태그 |
|------|------|------|------|
| `deepcopy-gen` | types.go | zz_generated.deepcopy.go | `+k8s:deepcopy-gen` |
| `defaulter-gen` | types.go + defaults.go | zz_generated.defaults.go | `+k8s:defaulter-gen` |
| `conversion-gen` | types.go (internal+external) | zz_generated.conversion.go | `+k8s:conversion-gen` |
| `client-gen` | types.go | kubernetes/ (clientset) | `+genclient` |
| `lister-gen` | types.go | listers/ | `+genclient` |
| `informer-gen` | types.go | informers/ | `+genclient` |
| `register-gen` | doc.go | zz_generated.register.go | `+k8s:register-gen` |
| `prerelease-lifecycle-gen` | types.go | zz_generated.prerelease-lifecycle.go | `+k8s:prerelease-lifecycle-gen` |
| `validation-gen` | types.go | zz_generated.validations.go | `+k8s:validation-gen` |
| `applyconfiguration-gen` | types.go | applyconfigurations/ | (자동) |
| `openapi-gen` | types.go | zz_generated.openapi.go | `+k8s:openapi-gen` |

---

## 5. deepcopy-gen

### 목적

Go에서 포인터를 포함한 구조체를 복사하면 **얕은 복사(shallow copy)**가 된다. Kubernetes의 API 오브젝트들은 포인터, 슬라이스, 맵을 다수 포함하므로, 안전한 깊은 복사를 위해 `DeepCopy()` 메서드가 필요하다.

### 태그

```go
// +k8s:deepcopy-gen=package           // 패키지 전체 타입에 대해 생성
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
```

### update-codegen.sh에서의 실행

**소스 위치**: `hack/update-codegen.sh` (라인 155-212)

```bash
# hack/update-codegen.sh:165
function codegen::deepcopy() {
    # Build the tool.
    GOPROXY=off go install \
        k8s.io/code-generator/cmd/deepcopy-gen

    # The result file, in each pkg, of deep-copy generation.
    local output_file="${GENERATED_FILE_PREFIX}deepcopy.go"

    # Find all directories that request deep-copy generation.
    local tag_dirs=()
    kube::util::read-array tag_dirs < <( \
        grep -l --null '+k8s:deepcopy-gen=' "${ALL_K8S_TAG_FILES[@]}" \
            | while read -r -d $'\0' F; do dirname "${F}"; done \
            | sort -u)

    deepcopy-gen \
        -v "${KUBE_VERBOSE}" \
        --go-header-file "${BOILERPLATE_FILENAME}" \
        --output-file "${output_file}" \
        --bounding-dirs "k8s.io/kubernetes,k8s.io/api" \
        "${tag_pkgs[@]}"
}
```

### 생성된 코드 예시

**소스 위치**: `staging/src/k8s.io/api/core/v1/zz_generated.deepcopy.go` (라인 1-44)

```go
//go:build !ignore_autogenerated
// +build !ignore_autogenerated

// Code generated by deepcopy-gen. DO NOT EDIT.

package v1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function
func (in *AWSElasticBlockStoreVolumeSource) DeepCopyInto(out *AWSElasticBlockStoreVolumeSource) {
    *out = *in
    return
}

func (in *AWSElasticBlockStoreVolumeSource) DeepCopy() *AWSElasticBlockStoreVolumeSource {
    if in == nil {
        return nil
    }
    out := new(AWSElasticBlockStoreVolumeSource)
    in.DeepCopyInto(out)
    return out
}
```

### DeepCopy 패턴

```go
// 포인터 필드가 있는 경우 깊은 복사
func (in *Affinity) DeepCopyInto(out *Affinity) {
    *out = *in
    if in.NodeAffinity != nil {
        in, out := &in.NodeAffinity, &out.NodeAffinity
        *out = new(NodeAffinity)
        (*in).DeepCopyInto(*out)     // 재귀적 깊은 복사
    }
    if in.PodAffinity != nil {
        in, out := &in.PodAffinity, &out.PodAffinity
        *out = new(PodAffinity)
        (*in).DeepCopyInto(*out)
    }
}
```

### runtime.Object 인터페이스 구현

`+k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object` 태그가 있으면:

```go
func (in *Pod) DeepCopyObject() runtime.Object {
    if c := in.DeepCopy(); c != nil {
        return c
    }
    return nil
}
```

이를 통해 모든 API 타입이 `runtime.Object` 인터페이스를 자동으로 구현한다.

---

## 6. client-gen

### 목적

각 API 타입에 대한 타입 안전한 클라이언트 코드를 자동 생성한다. 수동으로 작성하면 수백 개의 API 타입에 대해 거의 동일한 CRUD 코드를 반복해야 한다.

### 태그

```go
// +genclient                          // 클라이언트 생성
// +genclient:nonNamespaced            // cluster-scoped 리소스
// +genclient:noVerbs                  // CRUD 메서드 없음
// +genclient:onlyVerbs=create,delete  // 특정 메서드만
// +genclient:skipVerbs=watch          // 특정 메서드 제외
```

### update-codegen.sh에서의 실행

**소스 위치**: `hack/update-codegen.sh` (라인 756-798)

```bash
# hack/update-codegen.sh:756
function codegen::clients() {
    GOPROXY=off go install \
        k8s.io/code-generator/cmd/client-gen

    client-gen \
        -v "${KUBE_VERBOSE}" \
        --go-header-file "${BOILERPLATE_FILENAME}" \
        --output-dir "${KUBE_ROOT}/staging/src/k8s.io/client-go" \
        --output-pkg="k8s.io/client-go" \
        --clientset-name="kubernetes" \
        # ...
}
```

### 생성되는 코드 구조

```
staging/src/k8s.io/client-go/
├── kubernetes/                      # Clientset
│   ├── clientset.go                 # Clientset 인터페이스
│   ├── typed/
│   │   ├── core/v1/
│   │   │   ├── core_client.go       # CoreV1Client
│   │   │   ├── pod.go               # PodsGetter, PodInterface
│   │   │   ├── service.go           # ServicesGetter, ServiceInterface
│   │   │   └── ...
│   │   ├── apps/v1/
│   │   │   ├── apps_client.go       # AppsV1Client
│   │   │   ├── deployment.go        # DeploymentsGetter, DeploymentInterface
│   │   │   └── ...
│   │   └── ...
│   └── fake/                        # 테스트용 fake 클라이언트
```

### 생성된 클라이언트 사용 예시

```go
// client-gen이 생성한 타입 안전 클라이언트
clientset, _ := kubernetes.NewForConfig(config)

// Pod 생성
pod, err := clientset.CoreV1().Pods("default").Create(ctx, podSpec, metav1.CreateOptions{})

// Deployment 조회
deploy, err := clientset.AppsV1().Deployments("default").Get(ctx, "nginx", metav1.GetOptions{})

// Service 목록
services, err := clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})
```

모든 메서드가 **타입 안전**하다. `Pods()`.`Create()`는 `*v1.Pod`만 받고, `*v1.Service`는 컴파일 에러가 된다.

---

## 7. informer-gen과 lister-gen

### Lister의 목적

Lister는 **로컬 캐시에서** API 오브젝트를 읽어오는 인터페이스이다. API 서버에 직접 요청하지 않으므로 성능이 좋다.

### Informer의 목적

Informer는 API 서버의 변경을 Watch하고, 로컬 캐시를 업데이트하며, 이벤트 핸들러를 호출하는 메커니즘이다.

### 관계

```
API Server
    |
    | Watch (HTTP long-poll)
    v
Informer
    |
    +-- 로컬 캐시 업데이트 (Store/Indexer)
    |
    +-- 이벤트 핸들러 호출 (AddFunc, UpdateFunc, DeleteFunc)
    |
    v
Lister
    |
    +-- 캐시에서 읽기 (List, Get)
    |
    v
Controller/Operator
```

### 생성되는 코드 구조

```
staging/src/k8s.io/client-go/
├── informers/                       # Informer Factory
│   ├── factory.go                   # SharedInformerFactory
│   ├── core/v1/
│   │   ├── interface.go             # v1.Interface
│   │   ├── pod.go                   # PodInformer
│   │   ├── service.go               # ServiceInformer
│   │   └── ...
│   ├── apps/v1/
│   │   ├── deployment.go            # DeploymentInformer
│   │   └── ...
│   └── ...
│
├── listers/                         # Lister
│   ├── core/v1/
│   │   ├── pod.go                   # PodLister, PodNamespaceLister
│   │   ├── service.go               # ServiceLister
│   │   └── ...
│   ├── apps/v1/
│   │   ├── deployment.go            # DeploymentLister
│   │   └── ...
│   └── ...
```

### 생성된 Informer/Lister 사용 예시

```go
// SharedInformerFactory 생성
factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)

// Pod Informer 획득
podInformer := factory.Core().V1().Pods()

// 이벤트 핸들러 등록
podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
    AddFunc:    func(obj interface{}) { /* Pod 추가 */ },
    UpdateFunc: func(old, new interface{}) { /* Pod 수정 */ },
    DeleteFunc: func(obj interface{}) { /* Pod 삭제 */ },
})

// Lister로 캐시에서 조회
podLister := podInformer.Lister()
pod, err := podLister.Pods("default").Get("my-pod")
pods, err := podLister.Pods("default").List(labels.Everything())

// Informer 시작
factory.Start(stopCh)
factory.WaitForCacheSync(stopCh)
```

---

## 8. defaulter-gen과 conversion-gen

### defaulter-gen

API 오브젝트의 기본값을 설정하는 함수를 자동 생성한다.

### 태그

```go
// +k8s:defaulter-gen=TypeMeta           // TypeMeta 필드가 있는 타입에 기본값
// +k8s:defaulter-gen=true               // 이 타입에 항상 기본값 생성
```

### update-codegen.sh에서의 실행

**소스 위치**: `hack/update-codegen.sh` (라인 337-399)

```bash
# hack/update-codegen.sh:353
function codegen::defaults() {
    GOPROXY=off go install \
        k8s.io/code-generator/cmd/defaulter-gen

    local output_file="${GENERATED_FILE_PREFIX}defaults.go"

    local tag_dirs=()
    kube::util::read-array tag_dirs < <( \
        grep -l --null '+k8s:defaulter-gen=' "${ALL_K8S_TAG_FILES[@]}" \
            | while read -r -d $'\0' F; do dirname "${F}"; done \
            | sort -u)

    defaulter-gen \
        -v "${KUBE_VERBOSE}" \
        --go-header-file "${BOILERPLATE_FILENAME}" \
        --output-file "${output_file}" \
        "${tag_pkgs[@]}"
}
```

### 기본값 설정 패턴

사용자가 `SetDefaults_*` 함수를 작성하면, defaulter-gen이 이를 호출하는 래퍼를 생성한다:

```go
// 사용자 작성 (defaults.go)
func SetDefaults_PodSpec(obj *v1.PodSpec) {
    if obj.RestartPolicy == "" {
        obj.RestartPolicy = v1.RestartPolicyAlways
    }
    if obj.DNSPolicy == "" {
        obj.DNSPolicy = v1.DNSClusterFirst
    }
}

// 자동 생성 (zz_generated.defaults.go)
func SetObjectDefaults_Pod(in *v1.Pod) {
    SetDefaults_PodSpec(&in.Spec)
    for i := range in.Spec.InitContainers {
        a := &in.Spec.InitContainers[i]
        SetDefaults_Container(a)
    }
    for i := range in.Spec.Containers {
        a := &in.Spec.Containers[i]
        SetDefaults_Container(a)
    }
}
```

### conversion-gen

내부(internal) 타입과 외부(versioned) 타입 간의 변환 함수를 자동 생성한다.

### 태그

```go
// +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/core  // 내부 타입 패키지
// +k8s:conversion-gen-external-types=k8s.io/api/core/v1  // 외부 타입 패키지
```

### update-codegen.sh에서의 실행

**소스 위치**: `hack/update-codegen.sh` (라인 499-552)

```bash
# hack/update-codegen.sh:499
function codegen::conversions() {
    GOPROXY=off go install \
        k8s.io/code-generator/cmd/conversion-gen

    local output_file="${GENERATED_FILE_PREFIX}conversion.go"

    local extra_peer_pkgs=(
        k8s.io/kubernetes/pkg/apis/core
        k8s.io/kubernetes/pkg/apis/core/v1
        k8s.io/api/core/v1
    )

    conversion-gen \
        -v "${KUBE_VERBOSE}" \
        --go-header-file "${BOILERPLATE_FILENAME}" \
        --output-file "${output_file}" \
        $(printf -- " --extra-peer-dirs %s" "${extra_peer_pkgs[@]}") \
        "${tag_pkgs[@]}"
}
```

### 변환 패턴

```
외부 타입 (v1)                    내부 타입 (internal)
v1.Pod                    <-->    core.Pod
  |                                  |
  +-- conversion-gen이 자동 변환 함수 생성
  |
  +-- Convert_v1_Pod_To_core_Pod()
  +-- Convert_core_Pod_To_v1_Pod()
```

```go
// 자동 생성 (zz_generated.conversion.go)
func Convert_v1_PodSpec_To_core_PodSpec(in *v1.PodSpec, out *core.PodSpec, s conversion.Scope) error {
    out.Volumes = *(*[]core.Volume)(unsafe.Pointer(&in.Volumes))
    out.InitContainers = *(*[]core.Container)(unsafe.Pointer(&in.InitContainers))
    out.Containers = *(*[]core.Container)(unsafe.Pointer(&in.Containers))
    out.RestartPolicy = core.RestartPolicy(in.RestartPolicy)
    // ...
}
```

---

## 9. 기타 코드 생성 도구

### register-gen

**소스 위치**: `hack/update-codegen.sh` (라인 554-606)

스키마 등록 코드를 자동 생성한다.

```go
// +k8s:register-gen=package

// 생성됨: zz_generated.register.go
func addKnownTypes(scheme *runtime.Scheme) error {
    scheme.AddKnownTypes(SchemeGroupVersion,
        &Pod{},
        &PodList{},
        &Service{},
        &ServiceList{},
        // ...
    )
    return nil
}
```

### prerelease-lifecycle-gen

**소스 위치**: `hack/update-codegen.sh` (라인 284-335)

API 타입의 릴리스 수명주기(Alpha, Beta, GA) 정보를 생성한다.

```go
// types.go 의 태그:
// +k8s:prerelease-lifecycle-gen:introduced=1.0

// 생성됨: zz_generated.prerelease-lifecycle.go
func (in *Pod) APILifecycleIntroduced() (major, minor int) {
    return 1, 0
}
```

### validation-gen

**소스 위치**: `hack/update-codegen.sh` (라인 401-476)

```bash
# hack/update-codegen.sh:415
function codegen::validation() {
    GOPROXY=off go install \
        k8s.io/code-generator/cmd/validation-gen

    local output_file="${GENERATED_FILE_PREFIX}validations.go"

    validation-gen \
        -v "${KUBE_VERBOSE}" \
        --go-header-file "${BOILERPLATE_FILENAME}" \
        --output-file "${output_file}" \
        $(printf -- " --readonly-pkg %s" "${readonly_pkgs[@]}") \
        "${tag_pkgs[@]}"
}
```

### openapi-gen

**소스 위치**: `hack/update-codegen.sh` (라인 626-712)

OpenAPI 스펙을 자동 생성한다.

```go
// +k8s:openapi-gen=true

// 생성됨: zz_generated.openapi.go
func GetOpenAPIDefinitions(ref common.ReferenceCallback) map[string]common.OpenAPIDefinition {
    return map[string]common.OpenAPIDefinition{
        "k8s.io/api/core/v1.Pod": schema_k8sio_api_core_v1_Pod(ref),
        // ...
    }
}
```

### applyconfiguration-gen

**소스 위치**: `hack/update-codegen.sh` (라인 714-754)

Server-Side Apply를 위한 Apply Configuration 타입을 생성한다.

```bash
# hack/update-codegen.sh:714
function codegen::applyconfigs() {
    GOPROXY=off go install \
        k8s.io/kubernetes/pkg/generated/openapi/cmd/models-schema \
        k8s.io/code-generator/cmd/applyconfiguration-gen

    applyconfiguration-gen \
        -v "${KUBE_VERBOSE}" \
        --openapi-schema <(models-schema) \
        --go-header-file "${BOILERPLATE_FILENAME}" \
        --output-dir "${KUBE_ROOT}/staging/src/${APPLYCONFIG_PKG}" \
        --output-pkg "${APPLYCONFIG_PKG}" \
        "${ext_apis[@]}"
}
```

### protobuf 생성

**소스 위치**: `hack/update-codegen.sh` (라인 115-153)

```bash
# hack/update-codegen.sh:115
function codegen::protobuf() {
    local apis=()
    kube::util::read-array apis < <(
        git grep --untracked --null -l \
            -e '// +k8s:protobuf-gen=package' \
            -- cmd pkg staging \
            | while read -r -d $'\0' F; do dirname "${F}"; done \
            | sed 's|^|k8s.io/kubernetes/|;s|k8s.io/kubernetes/staging/src/||' \
            | sort -u)

    hack/_update-generated-protobuf-dockerized.sh "${apis[@]}"
}
```

---

## 10. hack/ 스크립트 체계

### update-* vs verify-* 패턴

Kubernetes는 모든 코드 생성에 **update/verify 쌍**을 사용한다.

```
hack/update-codegen.sh    # 코드 생성 실행 (파일 수정)
hack/verify-codegen.sh    # 코드 생성 결과 확인 (수정 없음)
```

`verify-*` 스크립트는 `update-*`를 실행한 후 `git diff`로 차이가 있는지 확인한다. 차이가 있으면 CI가 실패한다.

### 주요 update-* 스크립트

| 스크립트 | 역할 |
|---------|------|
| `hack/update-codegen.sh` | 모든 코드 생성 (deepcopy, client, informer, lister 등) |
| `hack/update-vendor.sh` | vendor/ 디렉토리 동기화 |
| `hack/update-gofmt.sh` | Go 코드 포맷팅 |
| `hack/update-generated-docs.sh` | API 문서 생성 |
| `hack/update-openapi-spec.sh` | OpenAPI 스펙 업데이트 |
| `hack/update-mocks.sh` | Mock 코드 업데이트 |
| `hack/update-translations.sh` | 번역 파일 업데이트 |
| `hack/update-internal-modules.sh` | 내부 모듈 동기화 |
| `hack/update-vendor-licenses.sh` | 벤더 라이선스 업데이트 |

### 주요 verify-* 스크립트

| 스크립트 | 역할 |
|---------|------|
| `hack/verify-codegen.sh` | 생성 코드가 최신인지 확인 |
| `hack/verify-vendor.sh` | vendor/ 동기화 상태 확인 |
| `hack/verify-gofmt.sh` | 코드 포맷 확인 |
| `hack/verify-golangci-lint.sh` | 린트 검사 |
| `hack/verify-boilerplate.sh` | 라이선스 헤더 확인 |
| `hack/verify-typecheck.sh` | 타입 체크 |
| `hack/verify-imports.sh` | import 순서 확인 |
| `hack/verify-spelling.sh` | 스펠링 검사 |
| `hack/verify-shellcheck.sh` | 셸 스크립트 검사 |
| `hack/verify-openapi-spec.sh` | OpenAPI 스펙 확인 |
| `hack/verify-featuregates.sh` | 피처 게이트 일관성 확인 |

### update-codegen.sh 내부 구조

**소스 위치**: `hack/update-codegen.sh` (라인 32-38)

```bash
# hack/update-codegen.sh:32
DBG_CODEGEN="${DBG_CODEGEN:-0}"
GENERATED_FILE_PREFIX="${GENERATED_FILE_PREFIX:-zz_generated.}"
UPDATE_API_KNOWN_VIOLATIONS="${UPDATE_API_KNOWN_VIOLATIONS:-}"
API_KNOWN_VIOLATIONS_DIR="${API_KNOWN_VIOLATIONS_DIR:-"${KUBE_ROOT}/api/api-rules"}"
OUT_DIR="_output"
BOILERPLATE_FILENAME="hack/boilerplate/boilerplate.generatego.txt"
APPLYCONFIG_PKG="k8s.io/client-go/applyconfigurations"
PLURAL_EXCEPTIONS="Endpoints:Endpoints"
```

### 코드 생성 실행 순서

```
hack/update-codegen.sh
    |
    +-- 1. protobuf 생성 (codegen::protobuf)
    |      - +k8s:protobuf-gen=package 태그 검색
    |      - go-to-protobuf 실행
    |
    +-- 2. deepcopy 생성 (codegen::deepcopy)
    |      - +k8s:deepcopy-gen 태그 검색
    |      - zz_generated.deepcopy.go 생성
    |
    +-- 3. swagger 문서 생성 (codegen::swagger)
    |      - types_swagger_doc_generated.go 생성
    |
    +-- 4. prerelease 수명주기 (codegen::prerelease)
    |      - +k8s:prerelease-lifecycle-gen 태그 검색
    |      - zz_generated.prerelease-lifecycle.go 생성
    |
    +-- 5. defaulter 생성 (codegen::defaults)
    |      - +k8s:defaulter-gen 태그 검색
    |      - zz_generated.defaults.go 생성
    |
    +-- 6. validation 생성 (codegen::validation)
    |      - +k8s:validation-gen 태그 검색
    |      - zz_generated.validations.go 생성
    |
    +-- 7. conversion 생성 (codegen::conversions)
    |      - +k8s:conversion-gen 태그 검색
    |      - zz_generated.conversion.go 생성
    |
    +-- 8. register 생성 (codegen::register)
    |      - +k8s:register-gen 태그 검색
    |      - zz_generated.register.go 생성
    |
    +-- 9. OpenAPI 생성 (codegen::openapi)
    |      - +k8s:openapi-gen 태그 검색
    |      - zz_generated.openapi.go 생성
    |
    +-- 10. ApplyConfiguration 생성 (codegen::applyconfigs)
    |      - applyconfiguration-gen 실행
    |
    +-- 11. Client 생성 (codegen::clients)
    |      - client-gen, lister-gen, informer-gen 실행
    |
    v
    완료
```

---

## 11. 코드 생성 태그 시스템

### 태그 형식

코드 생성 태그는 Go 주석에 `+` 접두사로 작성한다.

```go
// +k8s:deepcopy-gen=package
// +k8s:defaulter-gen=TypeMeta
// +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/core
// +genclient
// +genclient:nonNamespaced
```

### 태그 스코프

| 스코프 | 위치 | 예시 |
|--------|------|------|
| 패키지 | `doc.go` | `+k8s:deepcopy-gen=package` |
| 타입 | 타입 정의 바로 위 | `+genclient` |
| 필드 | 필드 정의 바로 위 | (일부 태그) |

### 실제 사용 예시

```go
// staging/src/k8s.io/api/core/v1/types.go

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.0

// Pod is a collection of containers that can run on a host.
type Pod struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   PodSpec   `json:"spec,omitempty"`
    Status PodStatus `json:"status,omitempty"`
}
```

### 태그 검색 메커니즘

**소스 위치**: `hack/update-codegen.sh` (라인 90-105)

```bash
# hack/update-codegen.sh:96
ALL_K8S_TAG_FILES=()
kube::util::read-array ALL_K8S_TAG_FILES < <(
    git_grep -l \
        -e '^// *+k8s:'                `# match +k8s: tags` \
        -- \
        ':!:*/testdata/*'              `# not under any testdata` \
        ':(glob)**/*.go'               `# in any *.go file` \
    )
```

모든 `+k8s:` 태그가 있는 Go 파일을 수집한 후, 각 코드 생성 도구가 자신의 태그를 가진 파일만 필터링한다.

### 전체 태그 목록

| 태그 | 도구 | 생성 파일 |
|------|------|----------|
| `+k8s:deepcopy-gen=package` | deepcopy-gen | `zz_generated.deepcopy.go` |
| `+k8s:deepcopy-gen:interfaces=...` | deepcopy-gen | `DeepCopyObject()` 메서드 |
| `+k8s:defaulter-gen=TypeMeta` | defaulter-gen | `zz_generated.defaults.go` |
| `+k8s:conversion-gen=<pkg>` | conversion-gen | `zz_generated.conversion.go` |
| `+k8s:conversion-gen-external-types=<pkg>` | conversion-gen | 외부 타입 지정 |
| `+k8s:openapi-gen=true` | openapi-gen | `zz_generated.openapi.go` |
| `+k8s:prerelease-lifecycle-gen:introduced=1.x` | prerelease-lifecycle-gen | `zz_generated.prerelease-lifecycle.go` |
| `+k8s:prerelease-lifecycle-gen:deprecated=1.x` | prerelease-lifecycle-gen | deprecated 버전 |
| `+k8s:register-gen=package` | register-gen | `zz_generated.register.go` |
| `+k8s:validation-gen=<value>` | validation-gen | `zz_generated.validations.go` |
| `+k8s:protobuf-gen=package` | go-to-protobuf | `generated.proto`, `generated.pb.go` |
| `+genclient` | client-gen | 클라이언트 코드 |
| `+genclient:nonNamespaced` | client-gen | cluster-scoped 클라이언트 |

---

## 12. 생성된 파일 패턴

### zz_generated.* 패턴

모든 자동 생성 파일은 `zz_generated.` 접두사를 사용한다.

```bash
# hack/update-codegen.sh:33
GENERATED_FILE_PREFIX="${GENERATED_FILE_PREFIX:-zz_generated.}"
```

`zz_` 접두사의 이유:
- 파일 목록에서 맨 마지막에 정렬되어 수동 작성 코드와 구분이 쉽다
- `ls` 또는 IDE에서 생성 파일을 한눈에 식별할 수 있다
- `.gitignore`나 검색 제외 패턴에서 쉽게 매칭된다

### 생성 파일 목록

```
staging/src/k8s.io/api/core/v1/
├── types.go                              # 수동 작성 (API 타입)
├── doc.go                                # 수동 작성 (패키지 태그)
├── zz_generated.deepcopy.go              # DeepCopy 메서드
├── zz_generated.prerelease-lifecycle.go  # 릴리스 수명주기
└── zz_generated.model_name.go            # 모델 이름

pkg/apis/core/v1/
├── defaults.go                           # 수동 작성 (SetDefaults_*)
├── conversion.go                         # 수동 작성 (Convert_*)
├── zz_generated.defaults.go              # 기본값 래퍼
├── zz_generated.conversion.go            # 변환 래퍼
└── zz_generated.register.go              # 스키마 등록

staging/src/k8s.io/client-go/
├── kubernetes/
│   └── typed/core/v1/
│       ├── pod.go                        # client-gen 생성
│       └── ...
├── listers/core/v1/
│   ├── pod.go                            # lister-gen 생성
│   └── ...
├── informers/core/v1/
│   ├── pod.go                            # informer-gen 생성
│   └── ...
└── applyconfigurations/core/v1/
    ├── pod.go                            # applyconfiguration-gen 생성
    └── ...
```

### 보일러플레이트 헤더

모든 생성 파일에는 라이선스 헤더와 "DO NOT EDIT" 경고가 포함된다.

```go
//go:build !ignore_autogenerated
// +build !ignore_autogenerated

/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
...
*/

// Code generated by deepcopy-gen. DO NOT EDIT.
```

헤더 파일 위치: `hack/boilerplate/boilerplate.generatego.txt`

---

## 13. CI/CD 파이프라인

### 검증 흐름

```
PR 제출
    |
    v
CI 트리거 (Prow)
    |
    +-- make verify
    |     |
    |     +-- hack/verify-codegen.sh     # 코드 생성 최신 확인
    |     +-- hack/verify-gofmt.sh       # 포맷 확인
    |     +-- hack/verify-golangci-lint.sh # 린트 확인
    |     +-- hack/verify-boilerplate.sh # 라이선스 헤더
    |     +-- hack/verify-vendor.sh      # vendor 동기화
    |     +-- hack/verify-typecheck.sh   # 타입 체크
    |     +-- hack/verify-imports.sh     # import 정리
    |     +-- hack/verify-spelling.sh    # 스펠링
    |     +-- hack/verify-shellcheck.sh  # 셸 스크립트
    |     +-- hack/verify-openapi-spec.sh # OpenAPI
    |     +-- hack/verify-featuregates.sh # 피처 게이트
    |     +-- ... (총 40개 이상의 검증)
    |
    +-- make test
    |     |
    |     +-- 단위 테스트 (약 30분)
    |
    +-- make test-integration
    |     |
    |     +-- 통합 테스트 (약 1-2시간)
    |
    +-- e2e 테스트
          |
          +-- 다양한 플랫폼/구성에서 실행 (수 시간)
```

### verify-codegen.sh의 동작 원리

```bash
# 1. 현재 코드 상태를 임시 디렉토리에 복사
# 2. update-codegen.sh 실행 (코드 재생성)
# 3. git diff로 차이 확인
# 4. 차이가 있으면 실패 → "코드를 생성하지 않고 커밋했다"
```

### vendor/ 관리

```bash
# vendor/ 업데이트
hack/update-vendor.sh

# vendor/ 검증
hack/verify-vendor.sh
```

Kubernetes는 `go.mod`와 `vendor/`를 모두 유지한다. `vendor/` 디렉토리가 있는 이유:
1. **재현 가능한 빌드**: 네트워크 없이도 빌드 가능
2. **보안 감사**: 모든 의존성 코드가 리포지토리에 포함
3. **CI 속도**: 의존성 다운로드 불필요

---

## 14. 설계 원칙: Why

### Why: 코드 생성을 선택한 이유

Kubernetes가 런타임 리플렉션 대신 코드 생성을 선택한 이유:

1. **타입 안전성**: 컴파일 타임에 오류를 잡을 수 있다
   ```go
   // 타입 안전: 컴파일 에러 (client-gen)
   clientset.CoreV1().Pods("default").Create(ctx, deployment, ...)  // 에러!

   // 타입 불안전: 런타임 에러 (리플렉션 기반)
   client.Resource("pods").Create(ctx, anyObject, ...)  // 런타임에야 에러
   ```

2. **성능**: 생성된 코드는 리플렉션보다 10-100배 빠르다
   ```go
   // deepcopy-gen: 직접 필드 복사 (빠름)
   out.Name = in.Name
   out.Spec = in.Spec

   // 리플렉션: reflect.Copy (느림)
   reflect.Copy(reflect.ValueOf(out), reflect.ValueOf(in))
   ```

3. **IDE 지원**: 생성된 코드에 대해 자동 완성, 리팩토링 등이 가능하다

4. **문서화**: 생성된 코드가 명시적이므로 동작을 이해하기 쉽다

### Why: staging/ 메커니즘을 사용하는 이유

```
문제: Kubernetes는 모노레포이지만, client-go 같은 패키지는
      외부 프로젝트에서 독립적으로 사용해야 한다.

해결: staging/ + publishing-bot
      1. 개발 시: go.work로 로컬 참조
      2. 릴리스 시: publishing-bot이 독립 리포지토리로 동기화
```

장점:
- **원자적 변경**: API 타입과 클라이언트를 동시에 수정 가능
- **테스트 통합**: 모든 변경이 하나의 CI에서 검증
- **독립 배포**: 외부 사용자는 `k8s.io/client-go`만 가져감

### Why: zz_generated 접두사를 사용하는 이유

```
zz_generated.deepcopy.go
^^
||
|+-- 정렬 시 맨 뒤로
+--- "자동 생성" 표시
```

이점:
1. `ls`나 IDE에서 수동 코드와 명확히 구분
2. 코드 리뷰 시 생성 파일을 건너뛸 수 있음
3. `.gitattributes`에서 diff 제외 가능
4. 검색/grep에서 쉽게 제외

### Why: update/verify 패턴을 사용하는 이유

```
update-codegen.sh → 코드 생성 (파일 수정)
verify-codegen.sh → 코드 검증 (수정 없음, diff 확인)
```

이 패턴이 필요한 이유:
1. **CI에서의 안전성**: verify만 실행하면 CI가 코드를 수정하지 않음
2. **개발자 워크플로**: update를 실행하면 로컬에서 코드 갱신
3. **변경 추적**: git diff로 정확히 어떤 생성 코드가 변경되었는지 확인
4. **선택적 실행**: 특정 도구만 실행 가능 (`DBG_CODEGEN=1`으로 디버그)

### Why: 보일러플레이트 헤더가 필수인 이유

```go
// Code generated by deepcopy-gen. DO NOT EDIT.
```

1. **법적 요구**: Apache 2.0 라이선스 헤더 필수
2. **자동 감지**: CI가 생성 파일을 식별하여 검증 범위 결정
3. **수정 방지**: 개발자가 실수로 생성 파일을 수동 수정하는 것을 방지
4. **빌드 태그**: `//go:build !ignore_autogenerated`로 테스트에서 제외 가능

### Why: protobuf 생성이 다른 도구보다 먼저 실행되는 이유

```bash
# hack/update-codegen.sh:112-113
# Some of the later codegens depend on the results of this, so it
# needs to come first in the case of regenerating everything.
```

protobuf → 다른 코드 생성 순서의 이유:
1. protobuf가 새로운 `.pb.go` 파일을 생성할 수 있음
2. 이 파일들이 이후 deepcopy, conversion 등의 입력으로 사용됨
3. 순서가 잘못되면 없는 타입을 참조하여 컴파일 에러 발생

---

## 요약

Kubernetes의 빌드/코드 생성 시스템은 대규모 Go 프로젝트의 유지보수성을 보장하는 핵심 인프라이다.

```
+---빌드 시스템---------------------------------------------------+
|                                                                  |
|  Makefile → hack/make-rules/*.sh → go build                     |
|  make all / make test / make verify / make update                |
|                                                                  |
+------------------------------------------------------------------+
                              |
+---모듈 시스템---------------------------------------------------+
|                                                                  |
|  go.work → staging/ 모듈 → publishing-bot → 독립 리포지토리     |
|  30+ 독립 모듈이 모노레포에서 개발, 독립으로 배포                 |
|                                                                  |
+------------------------------------------------------------------+
                              |
+---코드 생성 시스템----------------------------------------------+
|                                                                  |
|  hack/update-codegen.sh → 11개 코드 생성 도구                    |
|                                                                  |
|  +k8s:deepcopy-gen  → DeepCopy 메서드     (zz_generated.deepcopy.go)    |
|  +genclient         → 타입 안전 클라이언트 (kubernetes/typed/)          |
|  +k8s:defaulter-gen → 기본값 설정          (zz_generated.defaults.go)   |
|  +k8s:conversion-gen → 버전 변환           (zz_generated.conversion.go) |
|  +k8s:openapi-gen   → OpenAPI 스펙         (zz_generated.openapi.go)    |
|                                                                  |
+------------------------------------------------------------------+
                              |
+---검증 시스템---------------------------------------------------+
|                                                                  |
|  hack/verify-*.sh (40+ 검증 스크립트)                            |
|  CI/CD: Prow → make verify → make test → e2e                    |
|                                                                  |
+------------------------------------------------------------------+
```

이 설계를 통해 Kubernetes는:
- 수백 개의 API 타입에 대한 **보일러플레이트를 제거**하고
- **타입 안전한 클라이언트**를 자동 생성하며
- **일관된 API 패턴**을 전체 프로젝트에 강제하고
- **30개 이상의 독립 모듈**을 하나의 모노레포에서 관리한다
