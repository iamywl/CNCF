# PoC 02: Argo CD 핵심 데이터 모델

## 개요

Argo CD의 모든 동작은 세 가지 CRD(Application, AppProject, ApplicationSet)와 두 가지 K8s Secret 기반 타입(Cluster, Repository)을 중심으로 이루어진다. 이 PoC는 각 타입의 정확한 구조와 상호 관계, 그리고 Application 데이터 생명주기를 실제 소스코드 기반으로 시뮬레이션한다.

## 다루는 개념

| 개념 | 설명 | 실제 소스 |
|------|------|-----------|
| Application CRD | Spec/Status/Operation 삼중 구조 | `pkg/apis/application/v1alpha1/types.go:68` |
| ApplicationSpec | Source, Destination, SyncPolicy | `pkg/apis/application/v1alpha1/types.go:77` |
| ApplicationStatus | Sync, Health, Resources, OperationState | `pkg/apis/application/v1alpha1/types.go` |
| Operation 트리거 패턴 | Sync 요청 → Operation 필드 설정 → 실행 후 제거 | `controller/appcontroller.go` |
| AppProject CRD | SourceRepos, Destinations, Roles, SyncWindows | `pkg/apis/application/v1alpha1/types.go` |
| ApplicationSet CRD | List/Cluster/Git 제너레이터, 템플릿 | `pkg/apis/application/v1alpha1/applicationset_types.go` |
| Cluster 타입 | K8s Secret + argocd.argoproj.io/secret-type=cluster | `util/db/cluster.go` |
| Repository 타입 | K8s Secret + argocd.argoproj.io/secret-type=repository | `util/db/repository.go` |
| Health 모델 | HealthStatusCode 순서, IsWorse(), 집계 로직 | `util/health/health.go` |

## 데이터 구조 다이어그램

```
Application
├── Spec (원하는 상태 — 사용자 정의, Git에 저장)
│   ├── Source
│   │   ├── RepoURL, Path, TargetRevision
│   │   ├── Helm (releaseName, values, parameters)
│   │   └── Kustomize (namePrefix, images, commonLabels)
│   ├── Destination (server, namespace)
│   ├── Project
│   └── SyncPolicy
│       ├── Automated (prune, selfHeal, allowEmpty)
│       ├── SyncOptions []string
│       └── Retry (limit, backoff)
│
├── Status (현재 상태 — 컨트롤러 전용 쓰기)
│   ├── Sync (status: Synced|OutOfSync|Unknown, revision)
│   ├── Health (status: Healthy|Progressing|Degraded|Suspended|Missing|Unknown)
│   ├── Resources []ResourceStatus
│   └── OperationState (완료된 작업 결과)
│
└── Operation (대기 중인 작업 — Sync 트리거)
    ├── Sync (revision, prune, dryRun, resources)
    └── InitiatedBy (username, automated)

AppProject
├── SourceRepos []string        — 허용된 레포 URL 패턴
├── Destinations []Destination  — 허용된 클러스터/네임스페이스
├── Roles []ProjectRole         — 역할 + Casbin 정책
└── SyncWindows []SyncWindow    — 시간 기반 sync 허용/차단

ApplicationSet
├── Generators
│   ├── List  (정적 파라미터 목록)
│   ├── Cluster (등록된 클러스터마다 생성)
│   └── Git  (디렉토리/파일 목록으로 생성)
└── Template  (Go template 문법, {{.cluster}} 등)
```

## 데이터 생명주기

```
생성         → [Unknown / Unknown]
CompareState → [OutOfSync / Healthy]  (Git과 클러스터 비교)
autoSync     → Operation 필드 설정   (Sync 트리거)
Sync 시작    → [OutOfSync / Progressing] + OperationState.Phase=Running
Sync 완료    → [Synced / Healthy] + OperationState.Phase=Succeeded
```

## 실행 방법

```bash
cd poc-02-data-model
go run main.go
```

### 예상 출력

```
=================================================================
 Argo CD 핵심 데이터 모델 시뮬레이션
=================================================================

[단계 1] Application 생성
  name:      myapp
  project:   myproject
  repoURL:   https://github.com/myorg/myapp.git
  ...

[단계 2] Controller: CompareAppState 실행 → OutOfSync 감지
  syncStatus: OutOfSync (revision: a1b2c3d4e5f6)
    Deployment   myapp                OutOfSync
    Service      myapp                Synced
    ConfigMap    myapp-config         OutOfSync
...

Health 상태 심각도 순서:
  1. Healthy        (order: 0)
  2. Suspended      (order: 1)
  3. Progressing    (order: 2)
  4. Missing        (order: 3)
  5. Degraded       (order: 4)
  6. Unknown        (order: 5)
```

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `pkg/apis/application/v1alpha1/types.go` | Application, AppProject, Cluster, Repository 핵심 타입 |
| `pkg/apis/application/v1alpha1/applicationset_types.go` | ApplicationSet, Generator 타입 |
| `pkg/apis/application/v1alpha1/repository_types.go` | Repository 타입 |
| `util/health/health.go` | IsWorse(), 헬스 집계 |
| `util/db/cluster.go` | 클러스터 Secret CRUD |
| `util/db/repository.go` | 레포지토리 Secret CRUD |
| `controller/appcontroller.go` | Status 갱신, Operation 처리 |

## 핵심 설계 결정

**왜 Operation 필드를 CRD 안에 두는가?**
Sync 작업을 명시적 Operation 필드로 표현함으로써, 작업 시작 → 진행 → 완료의 전환이 K8s etcd Watch를 통해 자연스럽게 전파된다. 별도의 작업 큐 없이 Application 리소스 자체가 작업 요청을 전달하는 채널이 된다.

**왜 Health 심각도를 숫자로 정의하는가?**
여러 리소스의 헬스를 하나의 App 헬스로 집계할 때 `IsWorse()` 비교가 필요하다. 숫자 순서를 정의함으로써 N개 리소스에 대한 최악 헬스 탐색을 O(N) 선형 탐색으로 처리할 수 있다.

**왜 ApplicationSet에 Go template을 사용하는가?**
클러스터 이름, 환경 변수 등 동적 값을 정적 YAML에 삽입하기 위해 Go의 내장 템플릿 엔진을 활용한다. 추가 의존성 없이 강력한 문자열 조작이 가능하다.
