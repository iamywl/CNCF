# 04. Kubernetes 코드 구조

## 개요

Kubernetes는 Go 언어로 작성된 대규모 모노레포다.
약 350만 줄의 코드로 구성되며, `staging/` 메커니즘을 통해 핵심 라이브러리를 독립 모듈로 분리한다.

## 최상위 디렉토리 구조

```
kubernetes/
├── api/                    # API 정의 (OpenAPI spec, discovery)
│   ├── api-rules/          # API 호환성 규칙
│   ├── discovery/          # API 디스커버리 데이터
│   └── openapi-spec/       # OpenAPI v2/v3 스펙
│
├── build/                  # 빌드 스크립트, Docker 이미지
│
├── cmd/                    # 바이너리 진입점 (main 패키지)
│   ├── kube-apiserver/     # API 서버
│   ├── kube-controller-manager/  # 컨트롤러 매니저
│   ├── kube-scheduler/     # 스케줄러
│   ├── kubelet/            # Kubelet
│   ├── kube-proxy/         # Proxy
│   ├── kubectl/            # CLI 도구
│   ├── kubeadm/            # 클러스터 부트스트래핑
│   └── cloud-controller-manager/ # 클라우드 컨트롤러
│
├── hack/                   # 개발/CI 스크립트
│   ├── update-vendor.sh    # 벤더 디렉토리 업데이트
│   ├── verify-*.sh         # 검증 스크립트
│   └── update-codegen.sh   # 코드 생성 실행
│
├── pkg/                    # 핵심 비즈니스 로직 (외부 사용 가능)
│   ├── api/                # API 유틸리티
│   ├── apis/               # 내부 API 타입
│   ├── controller/         # 내장 컨트롤러 구현
│   │   ├── deployment/     # Deployment 컨트롤러
│   │   ├── replicaset/     # ReplicaSet 컨트롤러
│   │   ├── job/            # Job 컨트롤러
│   │   ├── cronjob/        # CronJob 컨트롤러
│   │   ├── daemon/         # DaemonSet 컨트롤러
│   │   ├── statefulset/    # StatefulSet 컨트롤러
│   │   ├── namespace/      # Namespace 컨트롤러
│   │   ├── garbagecollector/ # GC 컨트롤러
│   │   ├── nodelifecycle/  # Node 라이프사이클
│   │   └── ...
│   ├── controlplane/       # API 서버 Control Plane 로직
│   ├── kubelet/            # Kubelet 핵심 로직
│   │   ├── cm/             # Container Manager
│   │   ├── pleg/           # Pod Lifecycle Event Generator
│   │   └── ...
│   ├── proxy/              # kube-proxy 로직
│   ├── scheduler/          # 스케줄러 핵심 로직
│   │   ├── framework/      # 스케줄링 프레임워크
│   │   └── ...
│   ├── registry/           # API 리소스 레지스트리 (RBAC 등)
│   ├── volume/             # 볼륨 플러그인
│   └── ...
│
├── plugin/                 # 플러그인
│   └── pkg/
│       ├── auth/           # 인증/인가 플러그인
│       │   └── authorizer/rbac/ # RBAC 인가기
│       └── admission/      # 어드미션 플러그인 (26개)
│
├── staging/                # 독립 모듈로 발행되는 패키지
│   └── src/k8s.io/
│       ├── api/            # → k8s.io/api
│       ├── apimachinery/   # → k8s.io/apimachinery
│       ├── apiserver/      # → k8s.io/apiserver
│       ├── client-go/      # → k8s.io/client-go
│       ├── kubectl/        # → k8s.io/kubectl
│       ├── kubelet/        # → k8s.io/kubelet
│       └── ...             # 30+ 모듈
│
├── test/                   # 테스트 (e2e, integration)
│
├── vendor/                 # 벤더링된 의존성
│
├── go.mod                  # 모듈 정의
├── go.work                 # Go workspace (staging 포함)
└── Makefile                # 빌드 시스템 진입점
```

## cmd/ — 바이너리 진입점

모든 바이너리의 `main()` 함수는 동일한 패턴을 따른다:

```go
// cmd/kube-apiserver/apiserver.go
func main() {
    command := app.NewAPIServerCommand()
    code := cli.Run(command)
    os.Exit(code)
}
```

| 바이너리 | 진입점 | app 패키지 | 핵심 함수 |
|----------|--------|-----------|----------|
| kube-apiserver | `cmd/kube-apiserver/apiserver.go` | `cmd/kube-apiserver/app/` | `NewAPIServerCommand()` |
| kube-controller-manager | `cmd/kube-controller-manager/controller-manager.go` | `cmd/kube-controller-manager/app/` | `NewControllerManagerCommand()` |
| kube-scheduler | `cmd/kube-scheduler/scheduler.go` | `cmd/kube-scheduler/app/` | `NewSchedulerCommand()` |
| kubelet | `cmd/kubelet/kubelet.go` | `cmd/kubelet/app/` | `NewKubeletCommand()` |
| kube-proxy | `cmd/kube-proxy/proxy.go` | `cmd/kube-proxy/app/` | `NewProxyCommand()` |
| kubectl | `cmd/kubectl/kubectl.go` | staging `k8s.io/kubectl` | `NewDefaultKubectlCommand()` |
| kubeadm | `cmd/kubeadm/kubeadm.go` | `cmd/kubeadm/app/` | `NewKubeadmCommand()` |

## staging/ — 독립 모듈 시스템

### 왜 staging이 필요한가?

Kubernetes는 거대한 모노레포이지만, `client-go`, `api`, `apimachinery` 등은 외부 프로젝트에서도 사용해야 한다.
staging 메커니즘으로 이 패키지들을 독립 Go 모듈(예: `k8s.io/client-go`)로 발행한다.

```
모노레포 내부:
  staging/src/k8s.io/client-go/   ← 실제 코드 위치

발행 대상:
  github.com/kubernetes/client-go  ← 동기화된 미러

사용자 import:
  import "k8s.io/client-go/kubernetes"
```

### 주요 staging 모듈

| 모듈 | 경로 | 역할 |
|------|------|------|
| `k8s.io/api` | `staging/src/k8s.io/api/` | API 타입 정의 (Pod, Service, ...) |
| `k8s.io/apimachinery` | `staging/src/k8s.io/apimachinery/` | API 인프라 (TypeMeta, ObjectMeta, Scheme) |
| `k8s.io/apiserver` | `staging/src/k8s.io/apiserver/` | 범용 API 서버 라이브러리 |
| `k8s.io/client-go` | `staging/src/k8s.io/client-go/` | Go 클라이언트 (Informer, WorkQueue) |
| `k8s.io/controller-manager` | `staging/src/k8s.io/controller-manager/` | 컨트롤러 매니저 프레임워크 |
| `k8s.io/kube-scheduler` | `staging/src/k8s.io/kube-scheduler/` | 스케줄러 프레임워크 인터페이스 |
| `k8s.io/kubelet` | `staging/src/k8s.io/kubelet/` | Kubelet API 타입 |
| `k8s.io/cri-api` | `staging/src/k8s.io/cri-api/` | Container Runtime Interface |
| `k8s.io/component-base` | `staging/src/k8s.io/component-base/` | 공통 기반 (로깅, 메트릭, CLI) |
| `k8s.io/apiextensions-apiserver` | `staging/src/k8s.io/apiextensions-apiserver/` | CRD 서버 |
| `k8s.io/kube-aggregator` | `staging/src/k8s.io/kube-aggregator/` | API Aggregation 서버 |
| `k8s.io/code-generator` | `staging/src/k8s.io/code-generator/` | 코드 생성 도구 |

### go.work (Go Workspace)

`go.work` 파일로 staging 모듈들을 워크스페이스로 통합:

```go
// go.work
go 1.25.0

use (
    .
    ./staging/src/k8s.io/api
    ./staging/src/k8s.io/apimachinery
    ./staging/src/k8s.io/apiserver
    ./staging/src/k8s.io/client-go
    // ... 30+ 모듈
)
```

## pkg/ — 핵심 로직

### 컨트롤러 구현 (pkg/controller/)

```
pkg/controller/
├── deployment/
│   ├── deployment_controller.go    # DeploymentController
│   ├── sync.go                     # syncDeployment 핵심 로직
│   ├── rolling.go                  # 롤링 업데이트 전략
│   └── recreate.go                 # Recreate 전략
│
├── replicaset/
│   └── replica_set.go             # ReplicaSetController, manageReplicas
│
├── job/
│   └── job_controller.go          # JobController, syncJob
│
├── statefulset/
│   └── stateful_set.go            # StatefulSetController
│
├── garbagecollector/
│   └── garbagecollector.go        # GC (OwnerReference 기반)
│
├── controller_utils.go            # 공통 유틸리티
└── controller_ref_manager.go      # OwnerReference 관리
```

### 스케줄러 구현 (pkg/scheduler/)

```
pkg/scheduler/
├── scheduler.go                   # Scheduler 구조체, New(), Run()
├── schedule_one.go                # ScheduleOne(), 핵심 스케줄링 로직
├── framework/
│   ├── interface.go               # 로컬 프레임워크 확장
│   ├── runtime/
│   │   └── framework.go           # 플러그인 실행 엔진
│   └── plugins/
│       ├── noderesources/         # 노드 리소스 필터/스코어
│       ├── nodeaffinity/          # 노드 어피니티
│       ├── interpodaffinity/      # Pod 간 어피니티
│       ├── tainttoleration/       # Taint/Toleration
│       └── ...
├── apis/config/                   # 스케줄러 설정 타입
└── profile/                       # 스케줄러 프로파일
```

### Kubelet 구현 (pkg/kubelet/)

```
pkg/kubelet/
├── kubelet.go                     # Kubelet 구조체, NewMainKubelet, Run
├── kubelet_pods.go                # Pod 동기화, SyncPod
├── pod_workers.go                 # Pod 워커 (상태 머신)
├── pleg/
│   └── generic.go                 # GenericPLEG, Relist
├── cm/
│   └── container_manager.go       # ContainerManager 인터페이스
├── prober/                        # 라이브니스/레디니스 프로브
├── status/                        # 상태 매니저
├── volumemanager/                 # 볼륨 매니저
└── images/                        # 이미지 GC
```

## 빌드 시스템

### Makefile 타겟

소스: `Makefile`

```bash
# 전체 빌드
make all              # 모든 바이너리 빌드
make                  # all과 동일

# 개별 빌드
make WHAT=cmd/kube-apiserver       # API 서버만 빌드
make WHAT=cmd/kubectl              # kubectl만 빌드

# 테스트
make test             # 단위 테스트
make test-integration # 통합 테스트
make test-e2e         # E2E 테스트

# 코드 생성
make generated_files  # 코드 생성 (deepcopy, defaults, conversion)
make update           # 모든 자동 생성 파일 업데이트

# 검증
make verify           # 모든 검증 스크립트 실행

# 릴리스
make quick-release    # Docker 기반 빌드
make release          # 전체 릴리스 빌드
```

### 코드 생성 도구

Kubernetes는 boilerplate 코드를 자동 생성한다:

| 도구 | 역할 | 생성 파일 |
|------|------|----------|
| deepcopy-gen | DeepCopy 메서드 생성 | `zz_generated.deepcopy.go` |
| defaulter-gen | Default 값 설정 함수 | `zz_generated.defaults.go` |
| conversion-gen | 버전 간 변환 함수 | `zz_generated.conversion.go` |
| client-gen | 타입별 클라이언트 | `kubernetes/typed/core/v1/` |
| informer-gen | Informer 팩토리 | `informers/core/v1/` |
| lister-gen | 타입별 Lister | `listers/core/v1/` |
| openapi-gen | OpenAPI 스펙 | `zz_generated.openapi.go` |
| register-gen | API 그룹 등록 | `zz_generated.register.go` |

### 생성 파일 규칙

```
zz_generated.*.go    ← 자동 생성 파일 (편집 금지)
doc.go               ← 패키지 문서 + 코드 생성 태그
register.go          ← API 그룹 등록
types.go             ← 타입 정의 (수동 작성)
```

## 의존성 관리

### go.mod

```
module k8s.io/kubernetes
go 1.25.0

require (
    github.com/spf13/cobra v1.x       // CLI 프레임워크
    github.com/google/cadvisor v0.56   // 컨테이너 메트릭
    github.com/google/cel-go v0.26     // CEL 표현식 엔진
    github.com/prometheus/client_golang // Prometheus 메트릭
    go.etcd.io/etcd/client/v3          // etcd 클라이언트
    google.golang.org/grpc             // gRPC
    k8s.io/api                         // API 타입 (staging)
    k8s.io/apimachinery                // API 인프라 (staging)
    k8s.io/client-go                   // Go 클라이언트 (staging)
    // ... 100+ 의존성
)
```

### vendor/

```bash
# 의존성 관리
hack/pin-dependency.sh    # 의존성 버전 고정
hack/update-vendor.sh     # vendor 디렉토리 동기화
```

## 테스트 구조

```
test/
├── e2e/                # End-to-End 테스트
│   ├── apps/           # apps API 그룹 테스트
│   ├── auth/           # 인증/인가 테스트
│   ├── network/        # 네트워킹 테스트
│   ├── scheduling/     # 스케줄링 테스트
│   └── ...
│
├── integration/        # 통합 테스트
│   ├── apiserver/      # API 서버 통합 테스트
│   ├── scheduler/      # 스케줄러 통합 테스트
│   └── ...
│
└── cmd/                # 테스트 도구
```

패키지 내 단위 테스트: `*_test.go` 파일

```
pkg/controller/deployment/
├── deployment_controller.go
├── deployment_controller_test.go  # 단위 테스트
├── sync.go
└── sync_test.go                   # 단위 테스트
```
