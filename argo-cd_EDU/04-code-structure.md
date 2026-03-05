# Argo CD 코드 구조 분석

## 목차

1. [전체 디렉토리 구조](#1-전체-디렉토리-구조)
2. [단일 바이너리 아키텍처](#2-단일-바이너리-아키텍처)
3. [컴포넌트별 디렉토리 상세](#3-컴포넌트별-디렉토리-상세)
4. [빌드 시스템](#4-빌드-시스템)
5. [주요 의존성](#5-주요-의존성)
6. [코드 생성 시스템](#6-코드-생성-시스템)
7. [gitops-engine 서브모듈](#7-gitops-engine-서브모듈)
8. [테스트 구조](#8-테스트-구조)
9. [설정 및 배포 매니페스트](#9-설정-및-배포-매니페스트)
10. [패키지 의존성 그래프](#10-패키지-의존성-그래프)

---

## 1. 전체 디렉토리 구조

Argo CD는 모노레포 구조를 채택하여 하나의 저장소에 모든 컴포넌트가 포함된다. 모듈 경로는 `github.com/argoproj/argo-cd/v3`이며, Go 1.26을 사용한다.

```
argo-cd/
├── cmd/                                     # 진입점 — 단일 바이너리 디스패처
│   ├── main.go                              # os.Args[0] 기반 컴포넌트 분기
│   ├── argocd/                              # CLI 클라이언트 (argocd)
│   │   └── commands/
│   ├── argocd-server/                       # API 서버 (argocd-server)
│   │   └── commands/
│   ├── argocd-application-controller/       # 애플리케이션 컨트롤러
│   │   └── commands/
│   ├── argocd-repo-server/                  # 레포지토리 서버
│   │   └── commands/
│   ├── argocd-applicationset-controller/    # ApplicationSet 컨트롤러
│   │   └── commands/
│   ├── argocd-cmp-server/                   # Config Management Plugin 서버
│   │   └── commands/
│   ├── argocd-commit-server/                # Hydration commit 서버
│   │   └── commands/
│   ├── argocd-dex/                          # Dex SSO 래퍼
│   │   └── commands/
│   ├── argocd-notification/                 # 알림 컨트롤러 CLI
│   │   └── commands/
│   ├── argocd-git-ask-pass/                 # Git 크리덴셜 헬퍼
│   │   └── commands/
│   ├── argocd-k8s-auth/                     # Kubernetes 인증 헬퍼
│   │   └── commands/
│   └── util/                                # cmd 공통 유틸
│
├── server/                                  # API 서버 구현
│   ├── server.go                            # ArgoCDServer (cmux 멀티플렉싱, 1,746줄)
│   ├── application/                         # Application gRPC 서비스
│   ├── applicationset/                      # ApplicationSet 서비스
│   ├── cluster/                             # Cluster 서비스
│   ├── project/                             # Project 서비스
│   ├── repository/                          # Repository 서비스
│   ├── session/                             # Session 서비스 (로그인/로그아웃)
│   ├── settings/                            # Settings 서비스
│   ├── account/                             # Account 서비스
│   ├── certificate/                         # TLS Certificate 서비스
│   ├── gpgkey/                              # GPG Key 서비스
│   ├── repocreds/                           # Repository Credentials 서비스
│   ├── notification/                        # Notification 서비스
│   ├── badge/                               # 배지 이미지 서비스
│   ├── broadcast/                           # WebSocket 브로드캐스트
│   ├── cache/                               # 서버 캐시
│   ├── deeplinks/                           # Deep link 기능
│   ├── extension/                           # 확장 프록시
│   ├── logout/                              # 로그아웃 핸들러
│   ├── metrics/                             # 메트릭 엔드포인트
│   ├── rbacpolicy/                          # RBAC 정책
│   ├── rootpath_test.go
│   ├── server_norace_test.go
│   └── server_test.go
│
├── controller/                              # Application Controller
│   ├── appcontroller.go                     # 핵심 컨트롤러 (2,697줄)
│   ├── state.go                             # AppStateManager (1,238줄)
│   ├── sync.go                              # Sync 실행 (606줄)
│   ├── health.go                            # Health 판정 래퍼
│   ├── hook.go                              # Resource Hook 처리
│   ├── sort_delete.go                       # 삭제 순서 정렬
│   ├── clusterinfoupdater.go               # 클러스터 정보 업데이트
│   ├── sync_namespace.go                    # Namespace 동기화
│   ├── sharding/                            # 클러스터 샤딩
│   │   ├── sharding.go                      # 샤딩 알고리즘
│   │   ├── cache.go                         # 샤딩 캐시
│   │   └── consistent/                      # 일관성 해싱
│   ├── hydrator/                            # Hydration 기능
│   │   ├── hydrator.go
│   │   ├── types/
│   │   └── mocks/
│   ├── cache/                               # 컨트롤러 캐시
│   ├── metrics/                             # 컨트롤러 메트릭
│   └── syncid/                              # Sync ID 관리
│
├── reposerver/                              # Repository Server
│   └── repository/
│       ├── repository.go                    # manifest 생성 서비스 (3,276줄)
│       ├── repository.proto                 # gRPC 서비스 정의
│       ├── chart.go                         # Helm Chart 처리
│       ├── lock.go                          # 레포지토리 락
│       ├── types.go                         # 타입 정의
│       └── utils.go                         # 유틸리티
│
├── applicationset/                          # ApplicationSet Controller
│   ├── controllers/
│   │   ├── applicationset_controller.go     # controller-runtime 기반
│   │   ├── clustereventhandler.go          # 클러스터 이벤트 핸들러
│   │   └── requeue_after/
│   ├── generators/                          # 각종 Generator 구현체
│   │   ├── cluster.go                       # Cluster Generator
│   │   ├── git.go                           # Git Generator
│   │   ├── list.go                          # List Generator
│   │   ├── matrix.go                        # Matrix Generator
│   │   ├── merge.go                         # Merge Generator
│   │   ├── pull_request.go                  # Pull Request Generator
│   │   ├── scm_provider.go                  # SCM Provider Generator
│   │   ├── plugin.go                        # Plugin Generator
│   │   ├── duck_type.go                     # Duck-Type Generator
│   │   └── interface.go                     # Generator 인터페이스
│   ├── metrics/
│   ├── services/
│   ├── status/
│   ├── utils/
│   └── webhook/
│
├── gitops-engine/                           # GitOps 엔진 (라이브러리)
│   └── pkg/
│       ├── sync/                            # Sync 실행 엔진
│       │   ├── sync_context.go              # SyncContext 구현
│       │   ├── sync_phase.go                # Phase 관리
│       │   ├── sync_task.go                 # Task 단위
│       │   ├── sync_tasks.go                # Task 집합
│       │   ├── reconcile.go                 # Reconcile 루프
│       │   ├── hook/                        # Hook 처리
│       │   ├── resource/                    # 리소스 처리
│       │   ├── common/                      # 공통 상수
│       │   ├── ignore/                      # 리소스 무시 규칙
│       │   └── syncwaves/                   # Sync Wave 처리
│       ├── diff/                            # Three-way diff
│       │   ├── diff.go                      # 핵심 diff 알고리즘
│       │   └── internal/                    # 내부 구현
│       ├── health/                          # 리소스 Health 판정
│       │   ├── health.go                    # 메인 Health 로직
│       │   ├── health_deployment.go         # Deployment health
│       │   ├── health_statefulset.go        # StatefulSet health
│       │   ├── health_daemonset.go          # DaemonSet health
│       │   ├── health_job.go                # Job health
│       │   ├── health_pod.go                # Pod health
│       │   ├── health_service.go            # Service health
│       │   ├── health_ingress.go            # Ingress health
│       │   ├── health_pvc.go                # PVC health
│       │   ├── health_hpa.go                # HPA health
│       │   ├── health_replicaset.go         # ReplicaSet health
│       │   ├── health_apiservice.go         # APIService health
│       │   └── health_argo.go               # Argo 리소스 health
│       ├── cache/                           # 클러스터 리소스 캐시
│       │   ├── cluster.go                   # Cluster 캐시 구현
│       │   ├── resource.go                  # 리소스 노드
│       │   ├── settings.go                  # 캐시 설정
│       │   ├── predicates.go               # 캐시 필터
│       │   └── references.go               # 리소스 참조
│       └── utils/
│           └── kube/                        # kubectl 래퍼
│
├── pkg/                                     # 공개 API/타입
│   ├── apis/
│   │   └── application/
│   │       └── v1alpha1/                    # CRD 타입 정의
│   │           ├── types.go                 # 핵심 CRD 타입 (Application, AppProject 등)
│   │           ├── applicationset_types.go  # ApplicationSet 타입
│   │           ├── app_project_types.go     # AppProject 타입
│   │           ├── repository_types.go      # Repository 타입
│   │           ├── generated.pb.go          # protobuf 생성 코드
│   │           ├── generated.proto          # protobuf 소스
│   │           ├── openapi_generated.go     # OpenAPI 생성 코드
│   │           └── zz_generated.deepcopy.go # DeepCopy 생성 코드
│   ├── apiclient/                           # gRPC 클라이언트 팩토리
│   │   ├── apiclient.go                     # 클라이언트 생성
│   │   ├── application/                     # Application 서비스 클라이언트
│   │   ├── cluster/                         # Cluster 서비스 클라이언트
│   │   ├── project/                         # Project 서비스 클라이언트
│   │   ├── repository/                      # Repository 서비스 클라이언트
│   │   ├── session/                         # Session 서비스 클라이언트
│   │   ├── settings/                        # Settings 서비스 클라이언트
│   │   ├── account/                         # Account 서비스 클라이언트
│   │   └── grpcproxy.go                     # gRPC 프록시
│   └── client/                              # Generated Kubernetes clientset
│
├── util/                                    # 유틸리티 패키지 (공유 라이브러리)
│   ├── git/                                 # Git 클라이언트 (go-git 래퍼)
│   ├── helm/                                # Helm 통합
│   ├── kustomize/                           # Kustomize 통합
│   ├── lua/                                 # Lua VM (Health/Action 커스터마이징)
│   ├── rbac/                                # RBAC (Casbin 기반)
│   ├── session/                             # JWT 세션 관리
│   ├── settings/                            # 설정 관리 (ConfigMap/Secret)
│   ├── db/                                  # Kubernetes Secret 기반 DB
│   ├── cache/                               # Redis 캐시
│   ├── oidc/                                # OIDC 통합 (Dex)
│   ├── webhook/                             # Webhook 수신 처리
│   ├── kube/                                # kubectl 래퍼
│   ├── cert/                                # TLS 인증서
│   ├── crypto/                              # 암호화 유틸
│   ├── dex/                                 # Dex 클라이언트
│   ├── grpc/                                # gRPC 유틸리티
│   ├── jwt/                                 # JWT 유틸리티
│   ├── log/                                 # 로깅 (logrus 래퍼)
│   ├── metrics/                             # Prometheus 메트릭
│   ├── oci/                                 # OCI 레지스트리
│   ├── password/                            # 비밀번호 해싱
│   ├── proxy/                               # HTTP/SOCKS 프록시
│   ├── regex/                               # 정규식 유틸
│   ├── resource/                            # 리소스 비교 유틸
│   ├── security/                            # 보안 유틸
│   ├── tls/                                 # TLS 설정
│   └── trace/                               # 추적 유틸
│
├── notification_controller/                 # 알림 컨트롤러
│   └── controller/
│       ├── controller.go                    # 알림 컨트롤러 구현
│       └── controller_test.go
│
├── cmpserver/                               # Config Management Plugin 서버
│   ├── server.go                            # CMP 서버 진입점
│   ├── apiclient/                           # CMP API 클라이언트
│   └── plugin/                              # 플러그인 실행 로직
│
├── commitserver/                            # Commit 서버 (Hydration)
│   ├── server.go                            # Commit 서버 진입점
│   ├── apiclient/                           # Commit API 클라이언트
│   ├── commit/                              # Commit 실행 로직
│   └── metrics/                             # 메트릭
│
├── common/                                  # 전역 상수 및 공통 정의
│   ├── common.go                            # 컴포넌트명, 주소, ConfigMap명 등
│   └── version.go                           # 버전 정보
│
├── manifests/                               # Kubernetes 배포 매니페스트
│   ├── install.yaml                         # 표준 설치 (cluster-admin)
│   ├── namespace-install.yaml               # 네임스페이스 스코프 설치
│   ├── core-install.yaml                    # Core 모드 설치
│   ├── crds/                                # CRD 정의
│   ├── base/                                # Kustomize 베이스
│   ├── ha/                                  # HA 설정
│   ├── cluster-install/
│   ├── namespace-install/
│   └── addons/
│
├── hack/                                    # 개발 도구, 코드 생성 스크립트
│   ├── generate-proto.sh                    # Protobuf 코드 생성
│   ├── generate-mock.sh                     # Mock 코드 생성
│   ├── gen-crd-spec/                        # CRD 스펙 생성
│   ├── gen-docs/                            # 문서 생성
│   ├── gen-catalog/                         # 알림 카탈로그 생성
│   ├── gen-resources/                       # 리소스 생성
│   └── boilerplate.go.txt                   # 라이선스 헤더
│
├── test/                                    # 테스트 코드
│   ├── e2e/                                 # E2E 테스트
│   ├── fixture/                             # 테스트 픽스처
│   ├── manifests/                           # 테스트용 매니페스트
│   ├── cmp/                                 # CMP 테스트
│   ├── container/                           # 컨테이너 테스트
│   └── remote/                              # 원격 테스트
│
├── docs/                                    # MkDocs 문서
│   ├── operator-manual/
│   ├── user-guide/
│   └── developer-guide/
│
├── assets/                                  # 임베디드 자산
├── notifications_catalog/                   # 알림 카탈로그
├── overrides/                               # 리소스 Health 오버라이드
├── examples/                                # 예제 파일
├── Dockerfile                               # 프로덕션 이미지
├── Makefile                                 # 빌드 자동화
├── go.mod                                   # Go 모듈 정의
└── go.sum                                   # 의존성 체크섬
```

---

## 2. 단일 바이너리 아키텍처

### 2.1 설계 원칙: 하나의 바이너리, 여러 컴포넌트

Argo CD의 가장 중요한 구조적 특징은 **단일 바이너리(single binary) 패턴**이다. 모든 컴포넌트(API 서버, 컨트롤러, 레포 서버 등)가 하나의 `argocd` 바이너리로 빌드되고, 실행 시 `os.Args[0]`(실행 파일 이름)을 기반으로 어떤 컴포넌트로 동작할지 결정한다.

### 2.2 main.go 디스패처

파일: `/Users/ywlee/CNCF/argo-cd/cmd/main.go`

```go
func main() {
    var command *cobra.Command

    // 실행 파일 이름으로 컴포넌트 결정
    binaryName := filepath.Base(os.Args[0])
    if val := os.Getenv(binaryNameEnv); val != "" {
        binaryName = val
    }

    switch binaryName {
    case common.CommandCLI:                      // "argocd"
        command = cli.NewCommand()
    case common.CommandServer:                   // "argocd-server"
        command = apiserver.NewCommand()
    case common.CommandApplicationController:   // "argocd-application-controller"
        command = appcontroller.NewCommand()
    case common.CommandRepoServer:              // "argocd-repo-server"
        command = reposerver.NewCommand()
    case common.CommandCMPServer:               // "argocd-cmp-server"
        command = cmpserver.NewCommand()
    case common.CommandCommitServer:            // "argocd-commit-server"
        command = commitserver.NewCommand()
    case common.CommandDex:                     // "argocd-dex"
        command = dex.NewCommand()
    case common.CommandNotifications:           // "argocd-notifications"
        command = notification.NewCommand()
    case common.CommandGitAskPass:              // "argocd-git-ask-pass"
        command = gitaskpass.NewCommand()
    case common.CommandApplicationSetController: // "argocd-applicationset-controller"
        command = applicationset.NewCommand()
    case common.CommandK8sAuth:                 // "argocd-k8s-auth"
        command = k8sauth.NewCommand()
    default:
        // "argocd-linux-amd64" 같은 플랫폼별 바이너리도 CLI로 처리
        command = cli.NewCommand()
    }

    err := command.Execute()
    // ...
}
```

이 패턴의 장점:
- 빌드 아티팩트가 단 하나 → CI/CD 단순화
- 코드 공유 최대화 → 중복 제거
- 환경 변수(`ARGOCD_BINARY_NAME`)로도 컴포넌트 지정 가능

### 2.3 컴포넌트명 상수

파일: `/Users/ywlee/CNCF/argo-cd/common/common.go`

```go
const (
    CommandCLI                      = "argocd"
    CommandApplicationController    = "argocd-application-controller"
    CommandApplicationSetController = "argocd-applicationset-controller"
    CommandServer                   = "argocd-server"
    CommandCMPServer                = "argocd-cmp-server"
    CommandCommitServer             = "argocd-commit-server"
    CommandGitAskPass               = "argocd-git-ask-pass"
    CommandNotifications            = "argocd-notifications"
    CommandK8sAuth                  = "argocd-k8s-auth"
    CommandDex                      = "argocd-dex"
    CommandRepoServer               = "argocd-repo-server"
)
```

### 2.4 Docker 심볼릭 링크 방식

Dockerfile에서 단일 바이너리를 각 컴포넌트명으로 심볼릭 링크를 생성한다:

```dockerfile
COPY --from=argocd-build /go/src/github.com/argoproj/argo-cd/dist/argocd* /usr/local/bin/

RUN ln -s /usr/local/bin/argocd /usr/local/bin/argocd-server && \
    ln -s /usr/local/bin/argocd /usr/local/bin/argocd-repo-server && \
    ln -s /usr/local/bin/argocd /usr/local/bin/argocd-cmp-server && \
    ln -s /usr/local/bin/argocd /usr/local/bin/argocd-application-controller && \
    ln -s /usr/local/bin/argocd /usr/local/bin/argocd-dex && \
    ln -s /usr/local/bin/argocd /usr/local/bin/argocd-notifications && \
    ln -s /usr/local/bin/argocd /usr/local/bin/argocd-applicationset-controller && \
    ln -s /usr/local/bin/argocd /usr/local/bin/argocd-k8s-auth && \
    ln -s /usr/local/bin/argocd /usr/local/bin/argocd-commit-server
```

컨테이너가 시작될 때 OS는 심볼릭 링크 이름을 `os.Args[0]`으로 전달하므로, 각 Kubernetes Pod의 `command` 필드에 컴포넌트 이름만 지정하면 동일한 이미지로 모든 역할을 수행한다.

```
                   [ 단일 argocd 바이너리 ]
                          |
          ┌───────────────┼───────────────┐
          │               │               │
    argocd-server  argocd-repo-server  argocd-application-controller
    (symlink)         (symlink)             (symlink)
          │               │               │
          └───────────────┼───────────────┘
                    main() 디스패처
                  binaryName으로 분기
```

### 2.5 서비스 기본 주소

```go
// common/common.go
const (
    DefaultRepoServerAddr   = "argocd-repo-server:8081"
    DefaultCommitServerAddr = "argocd-commit-server:8086"
    DefaultDexServerAddr    = "argocd-dex-server:5556"
    DefaultRedisAddr        = "argocd-redis:6379"
)
```

---

## 3. 컴포넌트별 디렉토리 상세

### 3.1 server/ — API 서버

`server/server.go`는 Argo CD의 API 서버 구현체로, 1,746줄 규모다. 핵심 특징은 **cmux(connection multiplexer)**를 사용해 단일 TCP 포트에서 gRPC, gRPC-Web, HTTP/1.1을 동시에 처리한다는 점이다.

```
server/
├── server.go               # ArgoCDServer 구조체 및 초기화
├── application/            # Application CRUD gRPC 서비스
├── applicationset/         # ApplicationSet gRPC 서비스
├── cluster/                # Cluster 등록/관리 서비스
├── project/                # AppProject gRPC 서비스
├── repository/             # Repository 서비스
├── session/                # 로그인/JWT 발급 서비스
├── settings/               # argocd-cm 설정 조회 서비스
├── account/                # 사용자 계정 관리
├── certificate/            # TLS 인증서 관리
├── gpgkey/                 # GPG 키 관리
├── repocreds/              # Repository 크리덴셜 템플릿
├── notification/           # 알림 서비스
├── badge/                  # 상태 배지 SVG 생성
├── broadcast/              # SSE/WebSocket 이벤트 브로드캐스트
├── cache/                  # 서버 레이어 캐시
├── deeplinks/              # 외부 시스템 딥링크
├── extension/              # UI 확장 프록시
├── logout/                 # OIDC 로그아웃 핸들러
├── metrics/                # /metrics Prometheus 엔드포인트
└── rbacpolicy/             # RBAC 정책 상수
```

각 서브디렉토리는 gRPC protobuf에서 생성된 인터페이스를 구현하는 서비스 핸들러다. 예를 들어 `server/application/`은 `pkg/apiclient/application/`의 gRPC 서비스 인터페이스를 구현한다.

### 3.2 controller/ — Application Controller

`controller/`는 Kubernetes Operator 패턴으로 구현된 Argo CD의 핵심 조정(Reconcile) 로직이다.

```
controller/
├── appcontroller.go        # ApplicationController (2,697줄)
│                           # Informer 설정, 큐 처리, reconcile 루프
├── state.go                # AppStateManager (1,238줄)
│                           # 실제 상태 vs 원하는 상태 비교
├── sync.go                 # Sync 실행 (606줄)
│                           # gitops-engine SyncContext 호출
├── health.go               # Health 상태 집계
├── hook.go                 # PreSync/Sync/PostSync Hook
├── sort_delete.go          # 삭제 순서 위상 정렬
├── clusterinfoupdater.go   # 클러스터 연결 정보 업데이트
├── sync_namespace.go       # 네임스페이스 자동 생성
├── hydrator_dependencies.go # Hydrator 의존성 래퍼
├── sharding/               # 컨트롤러 샤딩 (대규모 클러스터)
│   ├── sharding.go         # 샤딩 알고리즘 (Legacy/일관성 해싱)
│   ├── cache.go            # 샤드 캐시
│   └── consistent/         # Rendezvous 해싱 구현
├── hydrator/               # Hydration (dry-source → wet-source)
│   ├── hydrator.go
│   ├── types/
│   └── mocks/
├── cache/                  # 컨트롤러 캐시 계층
├── metrics/                # Prometheus 메트릭 (sync 횟수, 지연 등)
└── syncid/                 # Sync 작업 추적 ID
```

핵심 흐름:
- `appcontroller.go`의 `ApplicationController`가 Application Informer를 구독
- Cluster 상태 변화 → 큐(workqueue)에 Application 적재
- `processAppRefreshQueueItem()` → `state.go`의 `GetRepoObjs()` → 매니페스트 계산
- 차이 감지 → `sync.go`의 `sync()` → gitops-engine `SyncContext.Sync()` 호출

### 3.3 reposerver/repository/ — Repository Server

Repository Server는 Git 레포지토리에서 매니페스트를 가져오고 렌더링하는 전담 서비스다. `repository.go`는 3,276줄로 가장 큰 파일 중 하나다.

```
reposerver/repository/
├── repository.go           # RepoServerServiceServer gRPC 구현
├── repository.proto        # gRPC 서비스 정의
├── chart.go                # Helm Chart 처리
├── lock.go                 # 레포지토리별 잠금 (동시성 제어)
├── types.go                # 타입 정의
└── utils.go                # 유틸리티 함수
```

주요 gRPC 메서드:
- `GenerateManifests()` — 핵심 메서드: Git clone → 렌더링(Helm/Kustomize/Jsonnet/plain YAML)
- `GetRepoTree()` — 레포지토리 파일 트리 조회
- `GetRevisionMetadata()` — 커밋 메타데이터 조회
- `ListApps()` — 앱 목록 조회

### 3.4 applicationset/ — ApplicationSet Controller

ApplicationSet Controller는 `sigs.k8s.io/controller-runtime` 프레임워크를 사용한다.

```
applicationset/
├── controllers/
│   ├── applicationset_controller.go  # controller-runtime Reconciler
│   ├── clustereventhandler.go        # Cluster 이벤트 처리
│   └── requeue_after/                # 재처리 스케줄링
├── generators/
│   ├── interface.go          # Generator 인터페이스
│   ├── cluster.go            # 클러스터 목록 기반
│   ├── git.go                # Git 디렉토리/파일 기반
│   ├── list.go               # 정적 목록
│   ├── matrix.go             # 두 Generator 조합 (교차곱)
│   ├── merge.go              # 두 Generator 병합
│   ├── pull_request.go       # PR 기반 (GitHub/GitLab/Bitbucket)
│   ├── scm_provider.go       # SCM 조직 스캔
│   ├── plugin.go             # 사용자 정의 플러그인
│   └── duck_type.go          # DuckType 리소스 기반
├── metrics/                  # ApplicationSet 메트릭
├── services/                 # 서비스 레이어
├── status/                   # 상태 관리
├── utils/                    # 유틸리티
└── webhook/                  # Webhook 수신
```

### 3.5 pkg/ — 공개 API 타입

```
pkg/
├── apis/
│   └── application/
│       └── v1alpha1/
│           ├── types.go              # Application, AppProject, Cluster 등 CRD 타입
│           ├── applicationset_types.go
│           ├── app_project_types.go
│           ├── repository_types.go
│           ├── generated.pb.go       # protobuf 직렬화 코드
│           ├── openapi_generated.go  # OpenAPI 스펙 생성
│           └── zz_generated.deepcopy.go # 딥카피 생성 코드
├── apiclient/
│   ├── apiclient.go     # ClientFactory (gRPC 연결 풀)
│   ├── grpcproxy.go     # gRPC 프록시
│   └── {service}/       # 각 서비스별 생성된 gRPC 클라이언트
└── client/              # Generated Kubernetes clientset/informer/lister
```

### 3.6 util/ — 공유 유틸리티 라이브러리

`util/`은 모든 컴포넌트에서 공유하는 라이브러리 패키지 모음이다. 외부 패키지에 대한 래퍼 또는 Argo CD 전용 구현을 담는다.

```
util/
├── git/        # go-git/v5 기반 Git 클라이언트 래퍼
├── helm/       # Helm SDK 통합 (차트 렌더링, 설치)
├── kustomize/  # kustomize/api 통합
├── lua/        # gopher-lua 임베디드 Lua VM
│               # 커스텀 Health 체크 및 Resource Actions에 사용
├── rbac/       # Casbin RBAC 엔진 래퍼
├── session/    # golang-jwt/v5 기반 JWT 세션
├── settings/   # argocd-cm ConfigMap 설정 파서
├── db/         # Kubernetes Secret을 DB처럼 사용하는 추상화 계층
├── cache/      # go-redis/v9 기반 Redis 캐시
├── oidc/       # coreos/go-oidc OIDC 인증 통합
├── webhook/    # go-playground/webhooks Webhook 파서
├── kube/       # kubectl 및 K8s 클라이언트 래퍼
├── cert/       # X.509 인증서 처리
├── crypto/     # AES 암호화 (Argo CD Secret 암호화)
├── dex/        # Dex OIDC 프록시 클라이언트
├── grpc/       # gRPC 미들웨어, 헬퍼
├── jwt/        # JWT 유틸리티
├── log/        # logrus 기반 구조화 로깅
├── metrics/    # Prometheus 메트릭 레지스트리
├── oci/        # OCI 레지스트리 지원
├── password/   # bcrypt 비밀번호 해싱
├── proxy/      # HTTP/SOCKS5 프록시 지원
├── security/   # 보안 관련 유틸
├── tls/        # mTLS 설정
├── trace/      # 분산 추적
└── webhook/    # Git 호스팅 서비스 Webhook 처리
```

---

## 4. 빌드 시스템

### 4.1 모듈 구성

파일: `/Users/ywlee/CNCF/argo-cd/go.mod`

```
module github.com/argoproj/argo-cd/v3

go 1.26.0
```

Go 1.26을 사용하며 모듈 경로에 `/v3`를 포함해 주요 버전을 명시한다.

### 4.2 주요 Makefile 타겟

Makefile은 빌드, 테스트, 코드 생성, 린트 등 개발 워크플로 전반을 관리한다.

```
all: cli image
```

| 타겟 | 설명 |
|------|------|
| `cli-local` | 로컬 CLI 바이너리 빌드 (`dist/argocd`) |
| `argocd-all` | 단일 바이너리 빌드 (`dist/argocd`) |
| `server` | API 서버 바이너리 빌드 (`dist/argocd-server`) |
| `repo-server` | Repo 서버 바이너리 빌드 |
| `controller` | Application Controller 바이너리 빌드 |
| `image` | Docker 이미지 빌드 (UI 포함) |
| `build-ui` | React UI 빌드 |
| `codegen-local` | 전체 코드 생성 실행 |
| `protogen` | Protobuf 코드 생성 |
| `clientgen` | Kubernetes clientset 생성 |
| `mockgen` | Mock 코드 생성 |
| `openapigen` | OpenAPI 스펙 생성 |
| `manifests-local` | Kustomize 매니페스트 생성 |
| `lint-local` | golangci-lint 실행 |
| `test-local` | 단위 테스트 실행 |
| `test-e2e` | E2E 테스트 실행 |
| `mod-vendor-local` | `go mod vendor` 실행 |

### 4.3 빌드 명령 상세

모든 컴포넌트는 동일한 진입점(`./cmd`)으로 빌드하지만, 출력 파일명을 다르게 지정한다:

```makefile
# 단일 바이너리 (모든 컴포넌트 포함)
argocd-all:
    CGO_ENABLED=${CGO_FLAG} go build \
        -v -ldflags '${LDFLAGS}' \
        -o ${DIST_DIR}/${BIN_NAME} ./cmd

# API 서버 전용 (실제로는 같은 main.go를 빌드)
server:
    CGO_ENABLED=${CGO_FLAG} go build \
        -v -ldflags '${LDFLAGS}' \
        -o ${DIST_DIR}/argocd-server ./cmd

# Application Controller 전용
controller:
    CGO_ENABLED=${CGO_FLAG} go build \
        -v -ldflags '${LDFLAGS}' \
        -o ${DIST_DIR}/argocd-application-controller ./cmd
```

주목할 점: 모두 `./cmd`를 빌드하지만 출력 파일명이 다르기 때문에 `os.Args[0]`에서 다른 이름이 반환되어 자동으로 올바른 컴포넌트로 디스패치된다.

### 4.4 LDFLAGS (빌드 메타데이터 주입)

```makefile
VERSION=$(shell cat ${CURRENT_DIR}/VERSION)
GIT_COMMIT=$(shell git rev-parse HEAD)
GIT_TAG=$(shell git describe --exact-match --tags HEAD 2>/dev/null)
BUILD_DATE=$(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

LDFLAGS="-X ${PACKAGE}.version=${VERSION} \
         -X ${PACKAGE}.buildDate=${BUILD_DATE} \
         -X ${PACKAGE}.gitCommit=${GIT_COMMIT}"
```

### 4.5 CGO 설정

macOS에서는 CGO가 기본 활성화(CGO_ENABLED=1), 그 외 환경에서는 비활성화(CGO_ENABLED=0)된다. 일부 암호화 라이브러리가 CGO를 필요로 하기 때문이다.

```makefile
DEFAULT_CGO_FLAG:=0
ifeq ($(IS_DARWIN),true)
    DEFAULT_CGO_FLAG:=1
endif
CGO_FLAG?=${DEFAULT_CGO_FLAG}
```

---

## 5. 주요 의존성

### 5.1 핵심 의존성 목록

파일: `/Users/ywlee/CNCF/argo-cd/go.mod`에서 확인된 실제 의존성이다.

| 패키지 | 버전 | 용도 |
|--------|------|------|
| `github.com/argoproj/argo-cd/gitops-engine` | v0.7.1-... | 핵심 sync/diff/health 엔진 |
| `github.com/argoproj/notifications-engine` | v0.5.1-... | 알림 트리거/서비스 추상화 |
| `sigs.k8s.io/controller-runtime` | v0.21.0 | ApplicationSet 컨트롤러 프레임워크 |
| `k8s.io/client-go` | v0.34.0 | Kubernetes 클라이언트, Informer, WorkQueue |
| `k8s.io/api` | v0.34.0 | Kubernetes API 타입 |
| `k8s.io/apimachinery` | v0.34.0 | Kubernetes 메타 타입 |
| `k8s.io/kubectl` | v0.34.0 | kubectl 기능 통합 |
| `k8s.io/code-generator` | v0.34.0 | clientset/informer/lister 코드 생성 |
| `google.golang.org/grpc` | (go.sum에서 확인) | gRPC 서버/클라이언트 |
| `github.com/grpc-ecosystem/grpc-gateway` | v1.16.0 | gRPC→REST 트랜스코딩 |
| `github.com/improbable-eng/grpc-web` | v0.15.1 | 브라우저용 gRPC-Web |
| `github.com/soheilhy/cmux` | (go.sum) | TCP 연결 멀티플렉싱 |
| `github.com/casbin/casbin/v2` | v2.135.0 | RBAC 정책 엔진 |
| `github.com/coreos/go-oidc/v3` | v3.14.1 | OIDC 토큰 검증 |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | JWT 생성/검증 |
| `github.com/redis/go-redis/v9` | v9.18.0 | Redis 캐시 |
| `github.com/go-redis/cache/v9` | v9.0.0 | Redis 캐시 레이어 |
| `github.com/go-git/go-git/v5` | v5.14.0 | Git 연산 (순수 Go 구현) |
| `github.com/google/go-jsonnet` | v0.21.0 | Jsonnet 렌더링 |
| `github.com/yuin/gopher-lua` | v1.1.1 | 임베디드 Lua VM (Health/Actions) |
| `github.com/spf13/cobra` | v1.10.2 | CLI 프레임워크 |
| `github.com/sirupsen/logrus` | v1.9.4 | 구조화 로깅 |
| `github.com/prometheus/client_golang` | v1.23.2 | Prometheus 메트릭 |
| `github.com/grpc-ecosystem/go-grpc-middleware/v2` | v2.3.3 | gRPC 미들웨어 체인 |
| `github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus` | v1.1.0 | gRPC 메트릭 |

### 5.2 Kubernetes 생태계 의존성

```
k8s.io/api                  v0.34.0   # Pod, Deployment 등 API 타입
k8s.io/apimachinery         v0.34.0   # ObjectMeta, TypeMeta 등 메타 타입
k8s.io/client-go            v0.34.0   # 클라이언트, Informer, WorkQueue
k8s.io/apiextensions-apiserver v0.34.0 # CRD 지원
k8s.io/kubectl              v0.34.0   # kubectl 기능
k8s.io/code-generator       v0.34.0   # 코드 생성 도구
sigs.k8s.io/controller-runtime v0.21.0 # ApplicationSet Reconciler
sigs.k8s.io/kustomize/api   v0.20.1   # Kustomize 렌더링
sigs.k8s.io/yaml            v1.6.0    # YAML 파싱
```

### 5.3 Git/SCM 통합 의존성

| 패키지 | 용도 |
|--------|------|
| `github.com/go-git/go-git/v5` | Git 클론/페치/체크아웃 (순수 Go) |
| `github.com/go-playground/webhooks/v6` | GitHub/GitLab/Bitbucket Webhook 파싱 |
| `github.com/google/go-github/v69` | GitHub API 클라이언트 |
| `github.com/bradleyfalzon/ghinstallation/v2` | GitHub App 인증 |
| `github.com/gfleury/go-bitbucket-v1` | Bitbucket API 클라이언트 |
| `github.com/gogacts/go-gogs-client` | Gitea/Gogs API 클라이언트 |
| `code.gitea.io/sdk/gitea` | Gitea SDK |

### 5.4 클라우드 프로바이더 의존성

| 패키지 | 용도 |
|--------|------|
| `github.com/aws/aws-sdk-go` | AWS EKS 인증, S3 등 |
| `github.com/Azure/azure-sdk-for-go` | Azure 통합 |
| `github.com/Azure/kubelogin` | Azure AD 인증 |

### 5.5 UI 및 API 게이트웨이 의존성

| 패키지 | 용도 |
|--------|------|
| `github.com/grpc-ecosystem/grpc-gateway` | gRPC → REST HTTP 변환 |
| `github.com/improbable-eng/grpc-web` | gRPC-Web 프로토콜 (브라우저) |
| `github.com/gorilla/websocket` | WebSocket 연결 |
| `github.com/gorilla/handlers` | HTTP 핸들러 유틸 |

### 5.6 의존성 왜(Why) 분석

**왜 `go-git`인가?**
순수 Go 구현으로 CGO 없이 크로스 컴파일이 가능하다. 시스템 git 바이너리 없이도 Git 연산을 수행할 수 있어 컨테이너 보안 강화에 유리하다.

**왜 `gopher-lua`인가?**
Kubernetes 리소스는 종류가 무한하며, 각 리소스의 Health 판정 로직을 Go 코드에 하드코딩하면 매번 Argo CD 업그레이드가 필요하다. Lua 스크립트를 ConfigMap에 저장하면 클러스터 운영자가 Argo CD 바이너리를 수정하지 않고도 커스텀 Health 로직을 주입할 수 있다.

**왜 `Casbin`인가?**
선언적 RBAC 정책(P, G 룰)을 텍스트 파일(ConfigMap)에서 동적으로 로드하고 핫 리로드할 수 있다. 프로그래밍 방식의 RBAC보다 훨씬 유연하게 정책을 관리할 수 있다.

**왜 `cmux`인가?**
단일 포트(8080)에서 gRPC(HTTP/2), gRPC-Web, HTTP/1.1 REST를 모두 서빙하기 위해서다. 로드 밸런서 설정을 단순화하고 TLS 종료 지점을 하나로 통일할 수 있다.

---

## 6. 코드 생성 시스템

Argo CD는 반복적인 보일러플레이트 코드를 자동 생성하는 여러 코드 생성 파이프라인을 갖춘다.

### 6.1 Protobuf 코드 생성

파일: `/Users/ywlee/CNCF/argo-cd/hack/generate-proto.sh`

Protobuf 정의(`.proto` 파일)에서 gRPC 서비스 코드를 생성한다.

```
생성 대상:
  pkg/apis/application/v1alpha1/generated.pb.go    # 타입 직렬화
  pkg/apis/application/v1alpha1/generated.proto    # 타입 스키마
  server/*/               # 각 서비스의 .pb.go 파일
  reposerver/repository/  # RepoServer gRPC 서비스
```

사용 도구:
- `protoc` — protobuf 컴파일러
- `protoc-gen-go` — Go 코드 생성 플러그인
- `protoc-gen-grpc-gateway` — REST 게이트웨이 생성
- `protoc-gen-swagger` — Swagger/OpenAPI 스펙 생성

생성 흐름:
```
.proto 파일 → protoc → .pb.go (직렬화 코드)
                     → .pb.gw.go (REST 게이트웨이)
                     → swagger.json (API 문서)
```

### 6.2 Kubernetes 코드 생성

Makefile 타겟: `clientgen`

`k8s.io/code-generator`를 사용해 CRD 타입에서 필요한 코드를 자동 생성한다.

```
생성 대상:
  pkg/client/clientset/   # Typed clientset (Application, AppProject 등)
  pkg/client/informers/   # Informer (변경 이벤트 감시)
  pkg/client/listers/     # Lister (캐시 기반 조회)
  pkg/apis/application/v1alpha1/zz_generated.deepcopy.go  # DeepCopy 메서드
  pkg/apis/application/v1alpha1/openapi_generated.go      # OpenAPI 스펙
```

생성 도구:
- `deepcopy-gen` — `DeepCopyInto()`, `DeepCopy()` 메서드 생성
- `client-gen` — Typed Kubernetes 클라이언트 생성
- `informer-gen` — SharedIndexInformer 생성
- `lister-gen` — Lister 인터페이스 생성

### 6.3 Mock 코드 생성

파일: `/Users/ywlee/CNCF/argo-cd/hack/generate-mock.sh`

`github.com/vektra/mockery`를 사용해 인터페이스에 대한 Mock 구현체를 생성한다. 단위 테스트에서 실제 Kubernetes 클러스터 없이 테스트를 실행하기 위한 용도다.

```
생성 위치:
  controller/hydrator/mocks/
  applicationset/generators/mocks/
  util/git/mocks/
  util/db/mocks/
```

### 6.4 OpenAPI 스펙 생성

Makefile 타겟: `openapigen`

생성된 swagger.json은 `util/swagger/` 에 저장되며, UI의 API 문서 및 CLI의 자동완성에 사용된다.

### 6.5 코드 생성 파이프라인 전체

```
[.proto 파일]
    │
    ├── protoc → .pb.go (직렬화/역직렬화)
    ├── protoc → .pb.gw.go (HTTP 게이트웨이)
    └── protoc → swagger.json (API 문서)

[types.go CRD 타입]
    │
    ├── deepcopy-gen → zz_generated.deepcopy.go
    ├── client-gen → pkg/client/clientset/
    ├── informer-gen → pkg/client/informers/
    └── lister-gen → pkg/client/listers/

[인터페이스 정의]
    │
    └── mockery → */mocks/*.go
```

모든 생성 코드는 소스 컨트롤에 체크인되어 있으며, 변경 시 `make codegen-local`을 다시 실행해야 한다.

---

## 7. gitops-engine 서브모듈

### 7.1 위치 및 성격

`gitops-engine`은 Argo CD 레포지토리 안에 포함되어 있으면서, 동시에 별도의 Go 모듈로도 발행되는 독특한 구조를 가진다.

```
go.mod에서:
require (
    github.com/argoproj/argo-cd/gitops-engine v0.7.1-0.20250908182407-97ad5b59a627
    ...
)
```

`gitops-engine` 디렉토리가 `argo-cd` 레포지토리 안에 있지만, `go.mod`에서 독립적인 모듈로 참조한다. 이는 gitops-engine이 Argo CD 외의 다른 GitOps 도구에서도 사용될 수 있도록 설계된 것이다.

### 7.2 디렉토리 구조

```
gitops-engine/
└── pkg/
    ├── sync/                    # Sync 실행 엔진
    │   ├── sync_context.go      # SyncContext (sync 상태 머신)
    │   ├── sync_phase.go        # Phase별 실행 (PreSync, Sync, PostSync)
    │   ├── sync_task.go         # 개별 리소스 sync 작업
    │   ├── sync_tasks.go        # Task 집합 및 의존성 해결
    │   ├── reconcile.go         # 최종 Reconcile 실행
    │   ├── hook/                # Hook 실행 로직
    │   │   ├── hook.go
    │   │   └── hook_test.go
    │   ├── resource/            # 리소스별 처리 로직
    │   ├── common/              # 공통 상수 (SyncPhase, HookType 등)
    │   ├── ignore/              # 무시할 리소스 필터
    │   └── syncwaves/           # SyncWave 관련 유틸
    │
    ├── diff/                    # Three-way diff 엔진
    │   ├── diff.go              # DiffArray, ThreeWayDiff
    │   ├── diff_test.go
    │   └── internal/            # 내부 구현 (JSON Patch 등)
    │
    ├── health/                  # 리소스 Health 판정
    │   ├── health.go            # GetResourceHealth() 디스패처
    │   ├── health_deployment.go # Deployment Health 판정
    │   ├── health_statefulset.go
    │   ├── health_daemonset.go
    │   ├── health_job.go
    │   ├── health_pod.go
    │   ├── health_service.go
    │   ├── health_ingress.go
    │   ├── health_pvc.go
    │   ├── health_hpa.go
    │   ├── health_replicaset.go
    │   ├── health_apiservice.go
    │   └── health_argo.go       # Argo Workflow, Rollout health
    │
    ├── cache/                   # 클러스터 리소스 캐시
    │   ├── cluster.go           # ClusterCache 구현
    │   ├── resource.go          # ResourceNode (캐시 단위)
    │   ├── settings.go          # 캐시 설정
    │   ├── predicates.go        # 감시할 리소스 필터
    │   └── references.go        # 리소스 간 참조 추적
    │
    └── utils/
        └── kube/                # kubectl 기능 래퍼
            ├── kubectl.go       # 주요 kubectl 연산
            ├── resource.go      # 리소스 유틸
            └── ...
```

### 7.3 gitops-engine의 역할

gitops-engine은 Argo CD의 핵심 GitOps 기능을 세 가지 추상화로 제공한다:

**1. Sync 엔진 (`pkg/sync/`)**
```
Git 매니페스트 (원하는 상태)
    +
클러스터 현재 상태
    ↓
SyncContext.Sync()
    ↓
Apply / Create / Delete 작업 실행
```

**2. Three-way Diff (`pkg/diff/`)**
```
[원하는 상태] ── diff ──→ [실제 diff 결과]
[실제 상태]  ──┘
[마지막 적용] ── (Three-way merge로 충돌 감지)
```

**3. Health 판정 (`pkg/health/`)**
```
리소스 타입에 따라 다른 Health 로직 적용:
  Deployment → replicas 확인
  Pod → phase 확인
  Job → 완료/실패 조건 확인
  커스텀 → Lua 스크립트 실행
```

### 7.4 왜 별도 라이브러리로 분리했는가?

- **재사용성**: Argo Rollouts, Flux 등 다른 GitOps 도구가 동일한 sync/diff 로직을 사용 가능
- **단일 책임**: 각 컴포넌트가 GitOps 핵심 로직을 직접 구현할 필요 없음
- **독립 테스트**: gitops-engine 자체를 독립적으로 테스트하고 발전시킬 수 있음
- **인터페이스 안정성**: Argo CD 내부 변경이 gitops-engine API에 영향을 미치지 않음

---

## 8. 테스트 구조

### 8.1 테스트 레이어

Argo CD는 세 개의 테스트 레이어를 가진다:

```
테스트 구조:
├── *_test.go (각 패키지 내)     # 단위 테스트
├── test/                         # 통합/E2E 테스트 지원
│   ├── e2e/                      # E2E 테스트
│   ├── fixture/                  # 테스트 픽스처 및 헬퍼
│   ├── manifests/                # 테스트용 Kubernetes 매니페스트
│   ├── cmp/                      # CMP 통합 테스트
│   ├── container/                # 컨테이너 테스트
│   └── remote/                   # 원격 클러스터 테스트
└── gitops-engine/pkg/*/          # gitops-engine 단위 테스트
```

### 8.2 단위 테스트

각 패키지 내 `*_test.go` 파일로 구성된다. Mock을 활용해 외부 의존성(Kubernetes API, Redis, Git)을 격리한다.

```
controller/appcontroller_test.go    # ApplicationController 단위 테스트
controller/state_test.go            # AppStateManager 단위 테스트
controller/sync_test.go             # Sync 로직 단위 테스트
server/server_test.go               # ArgoCDServer 단위 테스트
reposerver/repository/repository_test.go  # RepoServer 단위 테스트
applicationset/generators/*_test.go      # 각 Generator 단위 테스트
util/rbac/rbac_test.go              # RBAC 단위 테스트
util/git/git_test.go                # Git 클라이언트 단위 테스트
```

테스트 실행:
```bash
# 모든 단위 테스트
make test-local

# 특정 패키지만
go test ./controller/...

# 레이스 조건 감지 포함
go test -race ./controller/...
```

### 8.3 E2E 테스트

파일 위치: `/Users/ywlee/CNCF/argo-cd/test/e2e/`

```
test/e2e/
├── app_management_test.go          # 앱 생성/업데이트/삭제 E2E
├── app_autosync_test.go            # Auto-sync 동작 검증
├── app_management_ns_test.go       # 네임스페이스 스코프 앱
├── applicationset_test.go          # ApplicationSet E2E
├── applicationset_git_generator_test.go # Git Generator E2E
├── helm_test.go                    # Helm 렌더링 E2E
├── kustomize_test.go               # Kustomize 렌더링 E2E
├── jsonnet_test.go                 # Jsonnet 렌더링 E2E
├── hook_test.go                    # Sync Hook E2E
├── sync_waves_test.go              # SyncWave E2E
├── custom_tool_test.go             # CMP E2E
├── git_test.go                     # Git 연산 E2E
├── hydrator_test.go                # Hydration E2E
├── notification_test.go            # 알림 E2E
├── metrics_test.go                 # Prometheus 메트릭 E2E
├── rbac_test.go                    # RBAC E2E
├── cluster_test.go                 # 클러스터 관리 E2E
├── project_management_test.go      # AppProject E2E
└── fixture/                        # E2E 테스트 헬퍼 및 클라이언트
```

E2E 테스트는 실제 Kubernetes 클러스터와 실행 중인 Argo CD 인스턴스를 필요로 한다.

```bash
# E2E 포트 설정
ARGOCD_E2E_APISERVER_PORT?=8080
ARGOCD_E2E_REPOSERVER_PORT?=8081
ARGOCD_E2E_REDIS_PORT?=6379
ARGOCD_E2E_DEX_PORT?=5556
```

### 8.4 통합 테스트

`test/fixture/`에는 E2E 테스트를 위한 헬퍼 함수와 Kubernetes 클라이언트 팩토리가 있다. 실제 Kubernetes API를 호출하는 통합 수준 테스트에 사용된다.

### 8.5 테스트에서 Mock 활용

단위 테스트에서는 `mockery`로 생성된 Mock을 활용한다:

```go
// 예시: controller 단위 테스트에서 Repo Server Mock 사용
mockRepoServer := mocks.NewRepoServerServiceClient(t)
mockRepoServer.On("GenerateManifests", ...).Return(fakeManifests, nil)
```

레이스 조건 테스트용 별도 파일도 존재한다:
```
server/server_norace_test.go       # -race 플래그 없이 실행하는 테스트
controller/appcontroller_test.go   # 레이스 감지 포함 테스트
```

---

## 9. 설정 및 배포 매니페스트

### 9.1 manifests/ 디렉토리

```
manifests/
├── install.yaml                    # 표준 설치 (cluster-admin 권한)
├── namespace-install.yaml          # 네임스페이스 스코프 설치
├── core-install.yaml               # Core 모드 (UI/API 없음)
├── install-with-hydrator.yaml      # Hydration 포함 설치
├── crds/                           # CRD 정의 YAML
│   ├── application-crd.yaml
│   ├── applicationset-crd.yaml
│   └── appproject-crd.yaml
├── base/                           # Kustomize 베이스 레이어
├── ha/                             # 고가용성(HA) 설정
│   ├── install.yaml                # HA 설치 (레디스 Sentinel 포함)
│   └── ...
├── cluster-install/                # Cluster 스코프 설치 Kustomize
├── namespace-install/              # 네임스페이스 스코프 Kustomize
└── addons/                         # 선택적 애드온
```

### 9.2 Kubernetes ConfigMap/Secret 설정

Argo CD는 Kubernetes ConfigMap과 Secret을 설정 저장소로 사용한다:

```go
// common/common.go
const (
    ArgoCDConfigMapName              = "argocd-cm"        // 주요 설정
    ArgoCDSecretName                 = "argocd-secret"    // 암호, 인증서
    ArgoCDNotificationsConfigMapName = "argocd-notifications-cm"
    ArgoCDNotificationsSecretName    = "argocd-notifications-secret"
    ArgoCDRBACConfigMapName          = "argocd-rbac-cm"   // Casbin RBAC 정책
    ArgoCDKnownHostsConfigMapName    = "argocd-ssh-known-hosts-cm"
    ArgoCDTLSCertsConfigMapName      = "argocd-tls-certs-cm"
)
```

### 9.3 설치 모드 비교

| 설치 모드 | 매니페스트 | 권한 범위 | 용도 |
|-----------|-----------|----------|------|
| 표준(Cluster) | install.yaml | cluster-admin | 다중 클러스터 관리 |
| 네임스페이스 | namespace-install.yaml | 네임스페이스 내 | 단일 팀/프로젝트 |
| Core | core-install.yaml | cluster-admin | UI/API 없이 컨트롤러만 |
| HA | ha/install.yaml | cluster-admin | 프로덕션 고가용성 |
| Hydrator 포함 | *-with-hydrator.yaml | cluster-admin | Dry-source 기능 포함 |

---

## 10. 패키지 의존성 그래프

### 10.1 컴포넌트 간 의존성

```
                    ┌─────────────────┐
                    │   argocd CLI    │
                    │  cmd/argocd/    │
                    └────────┬────────┘
                             │ gRPC 호출
                    ┌────────▼────────┐
                    │   API 서버      │
                    │ server/         │
                    └────────┬────────┘
                             │ gRPC 호출 │ Redis 공유
              ┌──────────────┼──────────────┐
              │              │              │
     ┌────────▼───┐  ┌───────▼───┐  ┌──────▼──────┐
     │ Application│  │   Repo    │  │ApplicationSet│
     │ Controller │  │  Server   │  │  Controller  │
     │controller/ │  │reposerver/│  │applicationset│
     └────────────┘  └───────────┘  └─────────────┘
              │              │
              │ gitops-engine│ Git/Helm/Kustomize
              ▼              ▼
     ┌────────────┐  ┌───────────┐
     │gitops-engine│ │   util/   │
     │  sync/diff  │ │git/helm/  │
     │  health/    │ │kustomize/ │
     └─────────────┘ └───────────┘
              │
              ▼
     ┌────────────────┐
     │  Kubernetes    │
     │  API Server    │
     └────────────────┘
```

### 10.2 공유 패키지 의존 관계

```
pkg/apis/application/v1alpha1/  ←── 모든 컴포넌트가 공유하는 타입 정의
    ▲
    ├── server/
    ├── controller/
    ├── reposerver/
    ├── applicationset/
    └── pkg/apiclient/

util/  ←── 모든 컴포넌트가 공유하는 유틸리티
    ▲
    ├── server/         (util/rbac, util/session, util/settings, util/db)
    ├── controller/     (util/kube, util/git, util/cache)
    ├── reposerver/     (util/git, util/helm, util/kustomize, util/lua)
    └── applicationset/ (util/db, util/settings)

common/  ←── 전역 상수 (컴포넌트명, 포트, ConfigMap명)
    ▲
    └── 모든 컴포넌트
```

### 10.3 데이터 흐름 요약

```
[Git 레포지토리]
      │
      ▼ (Webhook 또는 폴링)
[argocd-repo-server]  ← GenerateManifests() 요청
      │ Git clone → 렌더링(Helm/Kustomize/Jsonnet)
      │ 결과: 매니페스트 목록
      │
      ▼
[argocd-application-controller]
      │ Three-way diff (gitops-engine)
      │ 변경 감지 → Sync 결정
      │
      ▼ (kubectl apply)
[Kubernetes API Server]
      │
      ▼
[실제 클러스터 상태 반영]
      │
      ▼ (상태 조회)
[argocd-server]
      │ gRPC → REST → 브라우저
      ▼
[Argo CD UI / argocd CLI]
```

### 10.4 코드 크기 현황

주요 파일들의 코드 크기(줄 수)로 보는 복잡도:

| 파일 | 줄 수 | 역할 |
|------|-------|------|
| `controller/appcontroller.go` | 2,697 | ApplicationController 핵심 |
| `reposerver/repository/repository.go` | 3,276 | 매니페스트 생성 서비스 |
| `controller/state.go` | 1,238 | 상태 비교 관리자 |
| `server/server.go` | 1,746 | API 서버 초기화 |
| `controller/sync.go` | 606 | Sync 실행 |

이 수치는 각 컴포넌트의 핵심 복잡도를 반영한다. 특히 `repository.go`의 크기는 매니페스트 생성 로직이 Helm, Kustomize, Jsonnet, plain YAML, OCI 등 다양한 툴체인을 지원하기 때문이다.

---

## 요약

Argo CD의 코드 구조는 다음 세 가지 핵심 설계 원칙을 따른다:

1. **단일 바이너리 패턴**: 하나의 Go 바이너리(`./cmd`)에서 모든 컴포넌트를 분기. 심볼릭 링크와 `os.Args[0]` 디스패처로 구현. 빌드/배포 단순화.

2. **명확한 책임 분리**: `server/`(API), `controller/`(Reconcile), `reposerver/`(매니페스트), `applicationset/`(템플릿)이 각각 독립적인 마이크로서비스로 동작. `pkg/`는 공유 타입, `util/`은 공유 라이브러리.

3. **코드 생성 우선**: Protobuf, Kubernetes code-generator, mockery를 통해 반복적인 보일러플레이트를 자동 생성. `zz_generated.*.go`, `generated.pb.go` 파일들이 그 산물.

이 구조를 이해하면 특정 기능이 어느 패키지에 있는지 직관적으로 파악할 수 있으며, 기여자가 올바른 위치에 코드를 추가하거나 수정할 수 있다.
