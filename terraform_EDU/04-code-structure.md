# 04. Terraform 코드 구조

## 1. 최상위 디렉토리 구조

```
terraform/
├── main.go                    # 프로그램 진입점
├── commands.go                # CLI 명령 등록 (initCommands)
├── checkpoint.go              # 버전 체크 (HashiCorp 업데이트 확인)
├── helper.go                  # 프로바이더 소스 헬퍼
├── signal_unix.go             # Unix 시그널 처리
├── signal_windows.go          # Windows 시그널 처리
│
├── go.mod                     # Go 모듈 정의
├── go.sum                     # 의존성 체크섬
├── Makefile                   # 빌드 타겟
│
├── internal/                  # 핵심 소스코드 (60+ 패키지)
├── version/                   # 버전 정보
├── testing/                   # 동등성 테스트
├── tools/                     # 개발 도구
├── scripts/                   # 빌드/릴리스 스크립트
├── docs/                      # 개발자 문서
├── .changes/                  # 체인지로그 엔트리
└── website/                   # 웹사이트 (비어있음, 별도 관리)
```

## 2. internal/ 패키지 상세

Terraform의 모든 핵심 로직은 `internal/` 하위에 위치한다. Go의 `internal` 패키지 규칙에 따라 외부에서 직접 import할 수 없다.

### 2.1 코어 엔진

| 패키지 | 파일 수 | 역할 |
|--------|--------|------|
| `terraform/` | 80+ | **핵심 엔진** — Context, Graph, Node, Walker, EvalContext |
| `dag/` | 5~6 | **DAG 라이브러리** — 위상 정렬, 병렬 워크, 사이클 검출 |
| `plans/` | 15+ | **Plan 모델** — 변경 계획, 액션, 직렬화 |
| `states/` | 15+ | **State 모델** — 상태 구조, SyncState, 직렬화 |
| `addrs/` | 70+ | **주소 체계** — 리소스/모듈/프로바이더 주소 파싱 |

### 2.2 설정 & 언어

| 패키지 | 역할 |
|--------|------|
| `configs/` | HCL 설정 파싱, Module/Resource/Variable 구조체 |
| `lang/` | 표현식 평가, 스코프, 내장 함수 (150+) |
| `experiments/` | 실험적 기능 플래그 관리 |
| `genconfig/` | `terraform plan -generate-config-out` 설정 생성 |

### 2.3 프로바이더 & 플러그인

| 패키지 | 역할 |
|--------|------|
| `providers/` | Provider 인터페이스 정의, 스키마 캐시 |
| `plugin/` | gRPC 프로바이더 클라이언트 (Protocol 5) |
| `plugin6/` | gRPC 프로바이더 클라이언트 (Protocol 6) |
| `pluginshared/` | 프로토콜 5/6 공통 유틸리티 |
| `tfplugin5/` | Protocol Buffers 정의 (v5) |
| `tfplugin6/` | Protocol Buffers 정의 (v6) |
| `grpcwrap/` | Provider 인터페이스 ↔ gRPC 변환 |
| `getproviders/` | 프로바이더 검색, 다운로드, 설치 |
| `providercache/` | 프로바이더 캐시 관리 |

### 2.4 CLI & 명령

| 패키지 | 역할 |
|--------|------|
| `command/` | **모든 CLI 명령 구현** (init, plan, apply, destroy, import 등) |
| `command/arguments/` | 명령 인자 파싱 |
| `command/cliconfig/` | CLI 설정 (~/.terraformrc) |
| `command/format/` | 출력 포맷팅 |
| `command/jsonformat/` | JSON 출력 |
| `command/views/` | 명령별 뷰 (human/JSON) |
| `terminal/` | 터미널 I/O 추상화 |

### 2.5 백엔드 & 상태 저장

| 패키지 | 역할 |
|--------|------|
| `backend/` | 백엔드 인터페이스, 공통 로직 |
| `backend/init/` | 백엔드 등록 (팩토리 패턴) |
| `backend/local/` | 로컬 백엔드 (파일 시스템) |
| `backend/remote/` | HCP Terraform 백엔드 |
| `cloud/` | HCP Terraform/Enterprise 통합 |

### 2.6 모듈 시스템

| 패키지 | 역할 |
|--------|------|
| `initwd/` | 모듈 설치 (ModuleInstaller) |
| `getmodules/` | 모듈 다운로드 (go-getter 래퍼) |
| `modsdir/` | .terraform/modules/ 매니페스트 관리 |
| `moduledeps/` | 모듈 의존성 분석 |
| `moduleref/` | 모듈 참조 해석 |
| `moduletest/` | terraform test 프레임워크 |

### 2.7 리팩토링 & 마이그레이션

| 패키지 | 역할 |
|--------|------|
| `refactoring/` | moved/removed 블록 처리 |
| `instances/` | count/for_each 인스턴스 확장 |
| `depsfile/` | .terraform.lock.hcl 의존성 잠금 |

### 2.8 기타 유틸리티

| 패키지 | 역할 |
|--------|------|
| `tfdiags/` | 구조화된 진단(에러/경고) |
| `logging/` | 로깅 설정, 플러그인 패닉 수집 |
| `httpclient/` | HTTP 클라이언트 (User-Agent 등) |
| `registry/` | Terraform Registry API 클라이언트 |
| `namedvals/` | 변수/출력/로컬 값 추적 |
| `checks/` | check 블록 실행 |
| `promising/` | Promise 패턴 (비동기 값 해석) |
| `collections/` | 제네릭 컬렉션 유틸리티 |
| `stacks/` | Terraform Stacks (실험적) |
| `rpcapi/` | RPC API 서버 (IDE 통합 등) |

## 3. 핵심 패키지 내부 구조

### 3.1 internal/terraform/ (코어 엔진)

```
internal/terraform/
├── context.go                  # Context 구조체 정의
├── context_walk.go             # walk 오퍼레이션 오케스트레이션
├── graph.go                    # Graph 래퍼 (dag.AcyclicGraph 임베딩)
├── graph_walk.go               # 그래프 워크 엔진
├── graph_walk_context.go       # ContextGraphWalker (EvalContext 구현)
│
├── graph_builder.go            # GraphBuilder 인터페이스
├── graph_builder_plan.go       # Plan 그래프 빌더
├── graph_builder_apply.go      # Apply 그래프 빌더
├── graph_builder_eval.go       # Eval 그래프 빌더
│
├── transform_*.go              # 20+ 그래프 변환기
│   ├── transform_config.go     #   Config에서 노드 생성
│   ├── transform_reference.go  #   참조 기반 의존성 엣지
│   ├── transform_provider.go   #   프로바이더 노드 연결
│   ├── transform_module_expansion.go  # count/for_each 확장
│   └── transform_targets.go    #   -target 필터링
│
├── node_resource_*.go          # 리소스 노드 (Plan/Apply/Destroy)
├── node_data_*.go              # 데이터 소스 노드
├── node_module_*.go            # 모듈 노드
├── node_provider_*.go          # 프로바이더 노드
├── node_output_*.go            # 출력 노드
├── node_local_*.go             # 로컬 값 노드
│
├── eval_context.go             # EvalContext 인터페이스
├── eval_context_builtin.go     # 기본 EvalContext 구현
│
├── hook.go                     # Hook 인터페이스
├── hook_stop.go                # 중지 훅
│
└── schemas.go                  # 스키마 로딩
```

### 3.2 internal/command/ (CLI 명령)

```
internal/command/
├── meta.go                     # Meta 기반 구조체 (공통)
├── meta_backend.go             # 백엔드 초기화 (125KB!)
├── meta_config.go              # Config 로딩
│
├── apply.go                    # terraform apply / destroy
├── plan.go                     # terraform plan
├── init.go                     # terraform init (65KB)
├── import.go                   # terraform import
├── refresh.go                  # terraform refresh
├── show.go                     # terraform show
├── validate.go                 # terraform validate
│
├── state_mv.go                 # terraform state mv
├── state_rm.go                 # terraform state rm
├── state_pull.go               # terraform state pull
├── state_push.go               # terraform state push
├── state_list.go               # terraform state list
├── state_show.go               # terraform state show
│
├── workspace_*.go              # terraform workspace 명령들
├── providers_*.go              # terraform providers 명령들
│
├── fmt.go                      # terraform fmt
├── console.go                  # terraform console
├── graph.go                    # terraform graph
├── output.go                   # terraform output
├── taint.go                    # terraform taint (deprecated)
├── untaint.go                  # terraform untaint
├── test.go                     # terraform test
│
├── arguments/                  # 인자 파싱 구조체
├── cliconfig/                  # CLI 설정 로딩
├── format/                     # 출력 포맷
├── jsonformat/                 # JSON 포맷
├── views/                      # 명령별 뷰
└── testdata/                   # 테스트 데이터
```

### 3.3 internal/dag/ (DAG 라이브러리)

```
internal/dag/
├── graph.go                    # Graph 기본 구조 (정점, 간선, 인접 리스트)
├── dag.go                      # AcyclicGraph (위상 정렬, 사이클 검출, 워크)
├── walk.go                     # Walker (병렬 DAG 워크)
├── set.go                      # Set 유틸리티
├── marshal.go                  # 그래프 직렬화 (dot 형식)
└── *_test.go                   # 테스트
```

## 4. 빌드 시스템

### 4.1 Go 모듈

```
// go.mod
module github.com/hashicorp/terraform
go 1.25.7
```

주요 의존성:

| 의존성 | 용도 |
|--------|------|
| `github.com/hashicorp/hcl/v2` | HCL2 파싱 |
| `github.com/zclconf/go-cty` | 타입 안전 값 시스템 |
| `github.com/hashicorp/go-plugin` | gRPC 플러그인 프레임워크 |
| `github.com/hashicorp/go-getter` | 원격 소스 다운로드 |
| `github.com/hashicorp/cli` | CLI 프레임워크 |
| `github.com/hashicorp/terraform-svchost` | 서비스 디스커버리 |
| `google.golang.org/grpc` | gRPC |
| `google.golang.org/protobuf` | Protocol Buffers |
| `go.opentelemetry.io/otel` | OpenTelemetry 텔레메트리 |

### 4.2 빌드 명령

```bash
# 기본 빌드
go install .

# 테스트
go test ./...

# 특정 패키지 테스트
go test ./internal/terraform/...
go test ./internal/command/...

# 코드 생성
go generate ./...

# Protobuf 컴파일
make protobuf
```

### 4.3 Protocol Buffers

```
internal/tfplugin5/              # Provider Protocol v5
├── tfplugin5.proto              # 프로토콜 정의
└── tfplugin5.pb.go              # 생성된 Go 코드

internal/tfplugin6/              # Provider Protocol v6
├── tfplugin6.proto              # 프로토콜 정의
└── tfplugin6.pb.go              # 생성된 Go 코드
```

## 5. 코드 흐름 맵

### 5.1 진입점에서 실행까지

```
main.go:main()
    ↓
main.go:realMain()
    ↓
commands.go:initCommands()
    ↓ (명령 등록)
cli.CLI.Run()
    ↓ (명령 디스패치)
internal/command/{cmd}.go:Run()
    ↓
internal/command/meta.go:Backend()
    ↓
internal/command/meta_backend.go:Backend()
    ↓
internal/backend/local/backend.go:Operation()
    ↓
internal/terraform/context.go:Plan() 또는 Apply()
    ↓
internal/terraform/graph_builder_{plan,apply}.go:Build()
    ↓
internal/terraform/graph_walk.go:Walk()
    ↓
internal/terraform/node_resource_*.go:Execute()
    ↓
internal/plugin/grpc_provider.go:PlanResourceChange() 또는 ApplyResourceChange()
    ↓
gRPC → Provider Plugin Process → Cloud API
```

### 5.2 설정 로딩 경로

```
.tf 파일
    ↓ (HCL2 파싱)
internal/configs/parser.go
    ↓
internal/configs/module.go (Module 구조체)
    ↓
internal/configs/config.go (Config 트리)
    ↓ (모듈 호출 해석)
internal/initwd/module_install.go (모듈 설치)
    ↓
internal/getmodules/getter.go (원격 다운로드)
```

### 5.3 State 경로

```
백엔드 (S3, Local, ...)
    ↓ (읽기)
internal/states/statefile/ (JSON 역직렬화)
    ↓
internal/states/state.go (State 구조체)
    ↓ (래핑)
internal/states/sync.go (SyncState — 스레드 안전)
    ↓ (그래프 워크 중 접근)
internal/terraform/graph_walk_context.go
    ↓ (변경 후)
internal/states/statefile/ (JSON 직렬화)
    ↓ (쓰기)
백엔드
```

## 6. 테스트 구조

```
# 단위 테스트 — 각 패키지 내 *_test.go
internal/terraform/context_plan_test.go
internal/terraform/context_apply_test.go
internal/dag/dag_test.go
internal/states/state_test.go

# E2E 테스트
internal/e2e/                    # End-to-End 테스트 프레임워크

# 동등성 테스트
testing/equivalence-tests/       # Plan/State 동등성 검증

# 테스트 데이터
internal/command/testdata/       # 명령별 테스트 픽스처
internal/terraform/testdata/     # 코어 엔진 테스트 픽스처

# 테스트 헬퍼
internal/provider-simple/        # 단순 테스트 프로바이더 (v5)
internal/provider-simple-v6/     # 단순 테스트 프로바이더 (v6)
internal/provider-terraform/     # 내장 terraform 프로바이더
```

## 7. 개발 도구

```
tools/
├── loggraphdiff/               # 그래프 변경 비교 도구
├── protobuf-compile/           # Protobuf 컴파일 도구
└── terraform-bundle/           # 프로바이더 번들 (deprecated)

scripts/
├── build.sh                    # 빌드 스크립트
└── ...
```

## 8. 파일 크기 기준 핵심 파일

| 파일 | 크기 | 역할 |
|------|------|------|
| `internal/command/meta_backend.go` | ~125KB | 백엔드 초기화, 마이그레이션 전체 로직 |
| `internal/command/init.go` | ~65KB | init 명령 전체 구현 |
| `internal/command/meta.go` | ~30KB | Meta 기반 구조체 |
| `internal/terraform/context.go` | ~20KB | Context 정의 및 핵심 메서드 |
| `internal/command/apply.go` | ~14KB | apply/destroy 명령 |
| `internal/command/state_mv.go` | ~17KB | state mv 명령 |

> **왜 meta_backend.go가 125KB인가?** — 백엔드 초기화는 Terraform에서 가장 복잡한 로직 중 하나다. 로컬↔리모트 마이그레이션, 워크스페이스 관리, 클라우드 백엔드 호환성, 상태 마이그레이션 확인 등 수많은 엣지 케이스를 처리해야 하기 때문이다.
