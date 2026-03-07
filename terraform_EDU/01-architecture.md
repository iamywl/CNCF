# 01. Terraform 아키텍처 개요

## 1. Terraform이란?

Terraform은 **Infrastructure as Code(IaC)** 도구로, 선언적 설정 파일(HCL)을 통해 클라우드 인프라를 정의하고 관리한다. 핵심 철학은 "원하는 상태를 선언하면, Terraform이 현재 상태와 비교하여 필요한 변경만 수행한다"는 것이다.

### 해결하는 문제

| 문제 | Terraform의 해결 방식 |
|------|---------------------|
| 수동 인프라 관리 | HCL로 선언적 정의, 코드 리뷰 가능 |
| 변경의 불확실성 | Plan으로 변경 사항 미리 확인 |
| 리소스 간 의존성 | DAG로 자동 의존성 해석 및 병렬 실행 |
| 상태 추적 | State 파일로 인프라 현재 상태 기록 |
| 멀티 클라우드 | 프로바이더 플러그인으로 다양한 API 지원 |

## 2. 전체 아키텍처

```
┌─────────────────────────────────────────────────────────────────┐
│                         CLI Layer                                │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐        │
│  │  init    │  │  plan    │  │  apply   │  │ destroy  │  ...   │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘        │
│       └──────────────┼──────────────┼──────────────┘             │
│                      ▼                                           │
│              ┌──────────────┐                                    │
│              │  command.Meta │  ← 모든 명령의 공통 기반           │
│              └──────┬───────┘                                    │
├─────────────────────┼───────────────────────────────────────────┤
│                     ▼           Core Engine                      │
│  ┌──────────────────────────────────────────────┐               │
│  │            terraform.Context                  │               │
│  │  ┌────────────┐  ┌───────────┐  ┌──────────┐ │               │
│  │  │ Config     │  │   DAG     │  │  State   │ │               │
│  │  │ (HCL 파싱) │  │ (그래프)  │  │ (상태)   │ │               │
│  │  └─────┬──────┘  └─────┬─────┘  └────┬─────┘ │               │
│  │        │               │              │       │               │
│  │        ▼               ▼              ▼       │               │
│  │  ┌──────────────────────────────────────────┐ │               │
│  │  │         Graph Walk Engine                │ │               │
│  │  │  (위상 정렬 → 병렬 실행 → 상태 갱신)     │ │               │
│  │  └──────────────────┬───────────────────────┘ │               │
│  └─────────────────────┼─────────────────────────┘               │
├─────────────────────────┼───────────────────────────────────────┤
│                         ▼          Plugin Layer                  │
│  ┌──────────────────────────────────────────────┐               │
│  │          Provider Plugin (gRPC)               │               │
│  │  ┌─────────┐  ┌─────────┐  ┌─────────┐      │               │
│  │  │  AWS    │  │  GCP    │  │  Azure  │ ...  │               │
│  │  └────┬────┘  └────┬────┘  └────┬────┘      │               │
│  │       └─────────────┼───────────┘            │               │
│  │                     ▼                         │               │
│  │            Cloud Provider APIs                │               │
│  └──────────────────────────────────────────────┘               │
├─────────────────────────────────────────────────────────────────┤
│                     Storage Layer                                │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐       │
│  │  Local   │  │   S3     │  │  Consul  │  │   GCS    │ ...  │
│  │  File    │  │ Backend  │  │ Backend  │  │ Backend  │       │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘       │
└─────────────────────────────────────────────────────────────────┘
```

## 3. 핵심 컴포넌트

### 3.1 CLI Layer (`main.go`, `commands.go`)

프로그램 진입점. `main()` → `realMain()`에서 다음 순서로 초기화한다:

```
main() → realMain()
    ├── openTelemetryInit()          // 텔레메트리 초기화
    ├── terminal.Init()               // 터미널 스트림 설정
    ├── cliconfig.LoadConfig()        // CLI 설정 로드 (~/.terraformrc)
    ├── disco.NewWithCredentialsSource()  // 서비스 디스커버리
    ├── providerSource()              // 프로바이더 소스 설정
    ├── backendInit.Init()            // 백엔드 초기화
    ├── initCommands()                // 모든 CLI 명령 등록
    └── cli.CLI.Run()                 // 명령 디스패치 및 실행
```

> **소스 위치**: `main.go:61-352` — `realMain()` 함수

### 3.2 Command Layer (`internal/command/`)

모든 명령은 `command.Meta` 구조체를 임베딩하여 공통 기능을 공유한다:

```
command.Meta
├── WorkingDir          // 작업 디렉토리 상태
├── Streams             // 터미널 I/O (stdout/stderr/stdin)
├── View                // 출력 추상화 (사람/기계 모두 대응)
├── ProviderSource      // 프로바이더 검색 소스
├── Services            // 서비스 디스커버리 (레지스트리 등)
└── ShutdownCh          // 인터럽트 처리 채널
```

주요 명령과 내부 동작:

| 명령 | 파일 | 핵심 동작 |
|------|------|----------|
| `terraform init` | `init.go` | 모듈 설치 → 프로바이더 설치 → 백엔드 초기화 |
| `terraform plan` | `plan.go` | Config 로드 → 그래프 구축 → walkPlan → Plan 생성 |
| `terraform apply` | `apply.go` | Plan 로드/생성 → 그래프 구축 → walkApply → State 갱신 |
| `terraform destroy` | `apply.go` | Destroy=true로 apply 실행 |
| `terraform import` | `import.go` | 외부 리소스를 State로 가져오기 |

> **소스 위치**: `internal/command/meta.go` — Meta 구조체 정의

### 3.3 Core Engine (`internal/terraform/`)

Terraform의 심장부. `terraform.Context`가 모든 실행을 조율한다:

```go
// internal/terraform/context.go
type Context struct {
    meta            *ContextMeta       // 메타데이터
    plugins         *contextPlugins    // 프로바이더/프로비저너 팩토리
    hooks           []Hook             // 라이프사이클 훅
    parallelSem     Semaphore          // 동시성 제어 (기본 10)
    runContext      context.Context    // 실행 컨텍스트
}
```

Context가 제공하는 핵심 오퍼레이션:

| 메서드 | 설명 |
|--------|------|
| `Plan()` | Config와 State를 비교하여 실행 계획 생성 |
| `Apply()` | Plan을 실제 인프라에 적용 |
| `Refresh()` | 원격 인프라에서 최신 상태를 읽어와 State 갱신 |
| `Import()` | 기존 인프라를 Terraform 관리 하에 가져오기 |

### 3.4 DAG Engine (`internal/dag/`)

리소스 간 의존성을 **DAG(Directed Acyclic Graph)**로 모델링한다:

```
┌───────┐     ┌───────┐
│  VPC  │────▶│ Subnet│
└───┬───┘     └───┬───┘
    │             │
    ▼             ▼
┌───────┐     ┌───────┐
│  IGW  │     │  EC2  │
└───────┘     └───┬───┘
                  │
                  ▼
              ┌───────┐
              │  EIP  │
              └───────┘
```

핵심 연산:
- **위상 정렬(Topological Sort)**: 의존성 순서 결정
- **병렬 워크(Parallel Walk)**: 독립적인 노드를 동시 실행
- **이행적 축소(Transitive Reduction)**: 불필요한 간접 의존성 제거
- **사이클 검출(Cycle Detection)**: 순환 의존성 감지

> **소스 위치**: `internal/dag/dag.go` — AcyclicGraph 구현

### 3.5 Provider Plugin System (`internal/plugin/`, `internal/providers/`)

프로바이더는 별도 프로세스로 실행되며, **gRPC**를 통해 Terraform 코어와 통신한다:

```
Terraform Core Process              Provider Plugin Process
┌──────────────────┐                ┌──────────────────┐
│  GRPCProvider    │ ──── gRPC ───▶ │  Provider 구현    │
│  (클라이언트)     │ ◀── gRPC ──── │  (서버)           │
└──────────────────┘                └──────┬───────────┘
                                          │
                                          ▼
                                   Cloud Provider API
                                   (AWS, GCP, Azure...)
```

프로바이더 인터페이스 핵심 메서드:

| 메서드 | 설명 |
|--------|------|
| `GetProviderSchema()` | 프로바이더가 지원하는 리소스/데이터 소스 스키마 반환 |
| `ConfigureProvider()` | 인증 정보 등 프로바이더 설정 |
| `PlanResourceChange()` | 리소스 변경 계획 (create/update/delete) |
| `ApplyResourceChange()` | 리소스 변경 실행 |
| `ReadResource()` | 원격에서 리소스 현재 상태 읽기 |
| `ImportResourceState()` | 기존 리소스를 Terraform State로 가져오기 |

> **소스 위치**: `internal/providers/provider.go` — Interface 정의

### 3.6 State Management (`internal/states/`)

State는 Terraform이 관리하는 모든 리소스의 현재 상태를 기록한다:

```
State
├── Modules: map[string]*Module
│   ├── "" (루트 모듈)
│   │   ├── Resources: map[string]*Resource
│   │   │   ├── "aws_instance.web"
│   │   │   │   └── Instances: map[InstanceKey]*ResourceInstance
│   │   │   │       ├── NoKey → Current: *InstanceObjectSrc
│   │   │   │       └── IntKey(0) → Current: *InstanceObjectSrc
│   │   │   └── "aws_vpc.main"
│   │   │       └── ...
│   │   └── OutputValues: map[string]*OutputValue
│   └── "module.network"
│       └── ...
└── CheckResults: *CheckResults
```

동시성 안전을 위해 `SyncState`가 `sync.RWMutex`로 보호한다.

> **소스 위치**: `internal/states/state.go`, `internal/states/sync.go`

### 3.7 Backend System (`internal/backend/`)

State 저장소를 추상화한다. 내장 백엔드:

| 백엔드 | 저장소 | 잠금 지원 |
|--------|--------|----------|
| `local` | 로컬 파일 시스템 | 파일 잠금 |
| `s3` | AWS S3 | DynamoDB |
| `azurerm` | Azure Blob | Blob 리스 |
| `gcs` | Google Cloud Storage | GCS 잠금 |
| `consul` | HashiCorp Consul | Consul KV 잠금 |
| `pg` | PostgreSQL | Advisory Lock |
| `http` | HTTP 엔드포인트 | 커스텀 |
| `kubernetes` | K8s ConfigMap | Annotation 기반 |

> **소스 위치**: `internal/backend/init/init.go` — 백엔드 등록

## 4. 실행 흐름 개요

### 4.1 `terraform plan` 흐름

```
1. CLI 인자 파싱
    ↓
2. 백엔드 초기화 (State 저장소 연결)
    ↓
3. Config 로드 (HCL 파싱 → Config 트리)
    ↓
4. State 로드 (백엔드에서 현재 상태 읽기)
    ↓
5. terraform.Context 생성
    ↓
6. Plan 실행:
   ├── BuildPlanGraph()
   │   ├── 리소스/모듈/프로바이더 노드 추가
   │   └── GraphTransformer 체인으로 의존성 연결
   ├── walk(graph, walkPlan)
   │   ├── 위상 정렬 순서로 노드 실행
   │   ├── 각 노드: provider.PlanResourceChange() 호출
   │   └── 변경 사항을 plan.Changes에 수집
   └── Plan 객체 반환
    ↓
7. Plan 파일 저장 (선택적: -out 옵션)
    ↓
8. 변경 사항 사용자에게 표시
```

### 4.2 `terraform apply` 흐름

```
1. Plan 파일 로드 또는 새 Plan 생성
    ↓
2. 사용자 확인 (auto-approve가 아닌 경우)
    ↓
3. Apply 실행:
   ├── BuildApplyGraph()
   │   └── plan.Changes 기반으로 그래프 구축
   ├── walk(graph, walkApply)
   │   ├── 각 변경: provider.ApplyResourceChange() 호출
   │   └── 결과를 State에 반영
   └── 최종 State 반환
    ↓
4. State 저장 (백엔드에 기록)
    ↓
5. 결과 출력 (생성/변경/삭제 카운트)
```

## 5. 핵심 설계 원칙

### 5.1 선언적 모델 (Declarative Model)

사용자는 "원하는 상태"를 선언하고, Terraform이 "현재 상태"와 비교하여 필요한 변경을 계산한다. 이것이 Plan 단계의 핵심이다.

### 5.2 그래프 기반 실행 (Graph-based Execution)

모든 리소스를 DAG 노드로 모델링하여:
- **의존성 자동 해석**: `vpc_id = aws_vpc.main.id`와 같은 참조에서 의존성 추론
- **병렬 실행**: 독립적인 리소스는 동시에 생성/삭제
- **순서 보장**: 의존 관계가 있는 리소스는 올바른 순서로 처리

### 5.3 프로바이더 플러그인 분리

코어와 프로바이더를 분리하여:
- **독립적 버전 관리**: 프로바이더 업데이트가 코어에 영향 없음
- **확장성**: 누구나 새 프로바이더 개발 가능
- **프로세스 격리**: 프로바이더 크래시가 코어에 영향 없음

### 5.4 상태 기반 추적 (State-based Tracking)

State 파일이 "진실의 원천(Source of Truth)"으로:
- 원격 리소스와 설정 파일 간의 매핑 유지
- 리소스 메타데이터(ID, 속성) 저장
- 리팩토링(moved 블록) 시 리소스 추적

## 6. 기술 스택

| 영역 | 기술 |
|------|------|
| 언어 | Go |
| 설정 언어 | HCL2 (hashicorp/hcl) |
| 플러그인 통신 | gRPC (go-plugin 프레임워크) |
| 직렬화 | Protocol Buffers (tfplugin5/6) |
| 그래프 | 자체 구현 DAG (internal/dag) |
| CLI 프레임워크 | hashicorp/cli |
| 값 시스템 | cty (zclconf/go-cty) |
| 레지스트리 | HTTPS API (registry.terraform.io) |
| 텔레메트리 | OpenTelemetry |
