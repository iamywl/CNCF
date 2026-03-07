# 03. Terraform 시퀀스 다이어그램

## 1. terraform init 흐름

`terraform init`은 프로젝트 초기화의 핵심 명령으로, 모듈 설치 → 프로바이더 설치 → 백엔드 초기화를 수행한다.

```mermaid
sequenceDiagram
    participant User as 사용자
    participant CLI as CLI (init.go)
    participant Meta as command.Meta
    participant ModInst as ModuleInstaller
    participant Registry as Terraform Registry
    participant ProvInst as Provider Installer
    participant Backend as Backend
    participant State as State Manager

    User->>CLI: terraform init
    CLI->>Meta: PrepareBackend()

    Note over CLI: 1단계: 코어 버전 확인
    CLI->>CLI: checkCoreVersionRequirements()

    Note over CLI: 2단계: 모듈 설치
    CLI->>ModInst: InstallModules()
    ModInst->>ModInst: Config 트리 순회
    loop 각 module 블록
        alt Registry 모듈
            ModInst->>Registry: AvailableVersions()
            Registry-->>ModInst: 버전 목록
            ModInst->>Registry: PackageLocation()
            Registry-->>ModInst: 다운로드 URL
            ModInst->>ModInst: 다운로드 & 추출
        else Git/HTTP 모듈
            ModInst->>ModInst: go-getter로 다운로드
        else 로컬 모듈
            ModInst->>ModInst: 심볼릭 링크 또는 복사
        end
    end
    ModInst->>ModInst: modules.json 매니페스트 갱신
    ModInst-->>CLI: 설치 완료

    Note over CLI: 3단계: 프로바이더 설치
    CLI->>ProvInst: EnsureProviderVersions()
    ProvInst->>ProvInst: Config에서 required_providers 추출
    loop 각 프로바이더
        ProvInst->>ProvInst: .terraform.lock.hcl 확인
        alt 캐시에 존재
            ProvInst->>ProvInst: 캐시에서 로드
        else 새로 설치 필요
            ProvInst->>Registry: AvailableVersions()
            Registry-->>ProvInst: 버전 목록
            ProvInst->>Registry: PackageMeta()
            Registry-->>ProvInst: 다운로드 URL, 해시
            ProvInst->>ProvInst: 다운로드 & 서명 검증
            ProvInst->>ProvInst: .terraform/providers/ 에 설치
        end
    end
    ProvInst->>ProvInst: .terraform.lock.hcl 갱신
    ProvInst-->>CLI: 설치 완료

    Note over CLI: 4단계: 백엔드 초기화
    CLI->>Backend: Backend() 초기화
    Backend->>Backend: Config에서 backend/cloud 블록 파싱
    alt 백엔드 변경됨
        Backend->>State: 기존 State 마이그레이션
        State-->>Backend: 마이그레이션 완료
    end
    Backend-->>CLI: 백엔드 준비 완료

    CLI-->>User: Terraform has been successfully initialized!
```

## 2. terraform plan 흐름

```mermaid
sequenceDiagram
    participant User as 사용자
    participant CLI as CLI (plan.go)
    participant Meta as command.Meta
    participant Backend as Backend
    participant Context as terraform.Context
    participant Graph as Graph Builder
    participant Walker as Graph Walker
    participant Provider as Provider (gRPC)

    User->>CLI: terraform plan
    CLI->>Meta: Backend() 초기화
    Meta-->>CLI: backend

    CLI->>Backend: Operation(opReq)

    Note over Backend: Local Backend 실행
    Backend->>Backend: Config 로드 (HCL 파싱)
    Backend->>Backend: State 로드 (백엔드에서)

    Backend->>Context: NewContext(opts)
    Context->>Context: 프로바이더 팩토리 설정
    Context->>Context: 훅 등록

    Note over Context: Plan 실행
    Context->>Graph: BuildPlanGraph()

    Note over Graph: 그래프 구축
    Graph->>Graph: ConfigTransformer (리소스 노드 추가)
    Graph->>Graph: ModuleExpansionTransformer (모듈 확장)
    Graph->>Graph: ProviderTransformer (프로바이더 연결)
    Graph->>Graph: ReferenceTransformer (의존성 엣지)
    Graph->>Graph: TransitiveReduction (불필요 엣지 제거)
    Graph-->>Context: 완성된 DAG

    Context->>Walker: walk(graph, walkPlan)

    Note over Walker: DAG 병렬 실행
    loop 위상 정렬 순서
        Walker->>Walker: 준비된 노드 선택

        par 독립 노드 병렬 실행
            Walker->>Provider: InitProvider() (최초 1회)
            Provider-->>Walker: 프로바이더 ready

            Walker->>Provider: PlanResourceChange(prior, proposed)
            Provider-->>Walker: PlannedState, RequiresReplace

            Walker->>Walker: plan.Changes에 변경 기록
        end
    end

    Walker-->>Context: Plan 완료
    Context-->>Backend: Plan 객체

    alt -out 옵션
        Backend->>Backend: Plan 파일 저장
    end

    Backend-->>CLI: Plan 결과
    CLI-->>User: Plan 출력 (create/change/destroy 카운트)
```

## 3. terraform apply 흐름

```mermaid
sequenceDiagram
    participant User as 사용자
    participant CLI as CLI (apply.go)
    participant Backend as Backend
    participant Context as terraform.Context
    participant Graph as Graph Builder
    participant Walker as Graph Walker
    participant Provider as Provider (gRPC)
    participant State as SyncState

    User->>CLI: terraform apply [plan-file]

    alt Plan 파일 지정
        CLI->>CLI: Plan 파일 로드
    else Plan 파일 미지정
        CLI->>CLI: 새 Plan 생성 (plan 흐름과 동일)
    end

    alt auto-approve가 아닌 경우
        CLI->>User: 변경 사항 표시 + 승인 요청
        User->>CLI: "yes"
    end

    CLI->>Backend: Operation(opReq, TypeApply)

    Backend->>Context: Apply(plan)

    Context->>Graph: BuildApplyGraph(plan.Changes)

    Note over Graph: Apply 그래프 구축
    Graph->>Graph: plan.Changes에서 노드 생성
    Graph->>Graph: Create 노드: 정방향 의존성
    Graph->>Graph: Delete 노드: 역방향 의존성
    Graph->>Graph: 프로바이더/프로비저너 노드 연결
    Graph-->>Context: Apply DAG

    Context->>Walker: walk(graph, walkApply)

    loop 위상 정렬 순서
        par 독립 노드 병렬 실행
            Note over Walker: Create/Update 리소스
            Walker->>Provider: ApplyResourceChange(prior, planned)
            Provider->>Provider: 클라우드 API 호출
            Provider-->>Walker: NewState

            Walker->>State: SetResourceInstanceCurrent(addr, newState)

            Note over Walker: Hook 호출
            Walker->>Walker: PostApply hook (UI 업데이트)
        end
    end

    Walker-->>Context: Apply 완료

    Context-->>Backend: 새 State

    Backend->>State: WriteState() → 백엔드 저장
    Backend->>State: PersistState()

    Backend-->>CLI: Apply 결과
    CLI-->>User: Apply complete! Resources: N added, N changed, N destroyed.
```

## 4. terraform destroy 흐름

```mermaid
sequenceDiagram
    participant User as 사용자
    participant CLI as CLI (apply.go, Destroy=true)
    participant Context as terraform.Context
    participant Graph as Graph Builder
    participant Walker as Graph Walker
    participant Provider as Provider (gRPC)
    participant State as SyncState

    User->>CLI: terraform destroy

    CLI->>Context: Plan(mode=DestroyMode)

    Note over Context: Destroy Plan
    Context->>Graph: BuildPlanGraph(mode=Destroy)
    Graph->>Graph: State의 모든 리소스에 Delete 액션
    Graph->>Graph: 역방향 의존성 순서 (자식 먼저 삭제)
    Graph-->>Context: Destroy Plan

    Context-->>CLI: Plan (모든 리소스 Delete)
    CLI->>User: 삭제 대상 표시 + 승인 요청
    User->>CLI: "yes"

    CLI->>Context: Apply(destroyPlan)

    Context->>Walker: walk(graph, walkApply)
    loop 역방향 의존성 순서
        Walker->>Provider: ApplyResourceChange(prior=current, planned=null)
        Provider->>Provider: 클라우드 리소스 삭제
        Provider-->>Walker: 삭제 완료
        Walker->>State: RemoveResourceInstanceObject()
    end

    Walker-->>Context: Destroy 완료
    Context-->>CLI: 빈 State
    CLI-->>User: Destroy complete! Resources: N destroyed.
```

## 5. 프로바이더 플러그인 라이프사이클

```mermaid
sequenceDiagram
    participant TF as Terraform Core
    participant PM as Plugin Manager
    participant Proc as Provider Process
    participant GRPC as gRPC Client

    Note over TF: 그래프 워크 중 프로바이더 필요

    TF->>PM: InitProvider(addr)
    PM->>PM: Factory 조회
    PM->>Proc: go-plugin으로 프로세스 시작
    Proc->>Proc: Provider 바이너리 실행
    Proc->>Proc: gRPC 서버 시작
    Proc-->>PM: 연결 정보 (포트, 프로토콜)
    PM->>GRPC: NewGRPCProvider(conn)
    GRPC-->>TF: providers.Interface

    Note over TF: 스키마 캐싱
    TF->>GRPC: GetProviderSchema()
    GRPC->>Proc: gRPC 호출
    Proc-->>GRPC: 스키마 응답
    GRPC->>GRPC: 스키마 캐시 저장
    GRPC-->>TF: GetProviderSchemaResponse

    Note over TF: 프로바이더 설정
    TF->>GRPC: ConfigureProvider(config)
    GRPC->>Proc: gRPC 호출
    Proc->>Proc: 인증 정보 설정 (API 키 등)
    Proc-->>GRPC: ConfigureProviderResponse
    GRPC-->>TF: 설정 완료

    Note over TF: 리소스 작업 (Plan/Apply 중 반복)
    loop 각 리소스
        TF->>GRPC: PlanResourceChange() 또는 ApplyResourceChange()
        GRPC->>Proc: gRPC 호출
        Proc->>Proc: 클라우드 API 호출
        Proc-->>GRPC: 응답
        GRPC-->>TF: 결과
    end

    Note over TF: 정리
    TF->>GRPC: Close()
    GRPC->>Proc: gRPC 연결 종료
    PM->>Proc: 프로세스 종료 (SIGTERM)
```

## 6. 그래프 워크 상세 흐름

```mermaid
sequenceDiagram
    participant CW as ContextGraphWalker
    participant DAG as dag.AcyclicGraph
    participant Walker as dag.Walker
    participant Node as Graph Node
    participant EC as EvalContext
    participant Provider as Provider

    CW->>DAG: Walk(walkFn)
    DAG->>DAG: TopologicalOrder()
    DAG->>Walker: 초기화 (모든 소스 노드 ready)

    loop 준비된 노드 존재하는 동안
        Walker->>Walker: 의존성 완료된 노드 선택

        par 세마포어 범위 내 병렬 실행
            Walker->>Node: walkFn(node)

            alt GraphNodeDynamicExpandable
                Node->>EC: DynamicExpand(ctx)
                EC-->>Node: 서브 그래프
                Node->>Node: 서브 그래프 재귀 워크
            end

            alt GraphNodeExecutable
                Node->>EC: Execute(ctx, walkOp)

                alt 리소스 노드 (Plan)
                    EC->>EC: 표현식 평가 (변수, 참조 해석)
                    EC->>Provider: PlanResourceChange()
                    Provider-->>EC: PlannedState
                    EC->>EC: plan.Changes에 기록
                else 리소스 노드 (Apply)
                    EC->>Provider: ApplyResourceChange()
                    Provider-->>EC: NewState
                    EC->>EC: state에 기록
                else 출력 노드
                    EC->>EC: output 표현식 평가
                    EC->>EC: namedvals에 저장
                else 로컬 노드
                    EC->>EC: local 표현식 평가
                end

                EC-->>Node: Diagnostics
            end

            Node-->>Walker: 완료 (또는 에러)
        end

        Walker->>Walker: 완료된 노드의 하위 노드 ready 마킹
    end

    Walker-->>DAG: 워크 완료
    DAG-->>CW: 최종 Diagnostics
```

## 7. State 잠금 흐름

```mermaid
sequenceDiagram
    participant TF1 as Terraform 인스턴스 1
    participant TF2 as Terraform 인스턴스 2
    participant Backend as 백엔드 (S3+DynamoDB)
    participant State as State 파일

    Note over TF1: terraform apply 시작
    TF1->>Backend: Lock(lockInfo)
    Backend->>Backend: DynamoDB PutItem (조건부)
    Backend-->>TF1: LockID 반환

    TF1->>Backend: State() 읽기
    Backend->>State: S3 GetObject
    State-->>TF1: 현재 State

    Note over TF2: 동시에 terraform apply 시도
    TF2->>Backend: Lock(lockInfo)
    Backend->>Backend: DynamoDB PutItem (조건부)
    Backend-->>TF2: Error: 이미 잠김!
    TF2-->>TF2: "Error: state is locked by TF1"

    Note over TF1: 작업 수행
    TF1->>TF1: Plan → Apply

    TF1->>Backend: WriteState(newState)
    Backend->>State: S3 PutObject
    State-->>Backend: 저장 완료

    TF1->>Backend: Unlock(lockID)
    Backend->>Backend: DynamoDB DeleteItem
    Backend-->>TF1: 잠금 해제 완료

    Note over TF2: 재시도 가능
    TF2->>Backend: Lock(lockInfo)
    Backend-->>TF2: LockID 반환 (성공)
```

## 8. 모듈 인스턴스 확장 흐름

```
HCL Config:
  module "server" {
    count  = 3
    source = "./modules/server"
  }

그래프 변환 과정:
┌────────────────────────────┐
│  nodeExpandModule          │
│  (module.server)           │
│  count = 3                 │
└─────────────┬──────────────┘
              │ DynamicExpand()
              ▼
┌──────────────────────────────────────────┐
│  확장된 서브그래프                         │
│  ┌──────────────┐                        │
│  │ module.      │                        │
│  │ server[0]    │                        │
│  ├──────────────┤                        │
│  │ module.      │                        │
│  │ server[1]    │                        │
│  ├──────────────┤                        │
│  │ module.      │                        │
│  │ server[2]    │                        │
│  └──────────────┘                        │
│  각각 내부 리소스 그래프를 가짐             │
└──────────────────────────────────────────┘
```

## 9. import 흐름

```mermaid
sequenceDiagram
    participant User as 사용자
    participant CLI as CLI (import.go)
    participant Context as terraform.Context
    participant Provider as Provider (gRPC)
    participant State as State

    User->>CLI: terraform import aws_instance.web i-1234567890

    CLI->>CLI: 주소 파싱 (aws_instance.web)
    CLI->>CLI: Config에서 해당 리소스 블록 확인

    CLI->>Context: Import(targets)

    Context->>Provider: ImportResourceState(typeName, id)
    Provider->>Provider: 클라우드 API로 리소스 조회
    Provider-->>Context: ImportedResource(s)

    Note over Context: 가져온 리소스로 State 구성
    Context->>Provider: ReadResource(importedState)
    Provider-->>Context: 최신 State

    Context->>State: SetResourceInstanceCurrent()

    Context-->>CLI: 새 State
    CLI->>CLI: State 저장

    CLI-->>User: Import successful!
```
