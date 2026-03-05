# PoC 03: Application Controller Reconciliation Loop

## 개요

Argo CD의 핵심 엔진은 Application Controller의 Reconciliation Loop다. 이 PoC는 `processAppRefreshQueueItem()`과 `processAppOperationQueueItem()` 두 워커가 `appRefreshQueue`/`appOperationQueue` 두 워크큐를 소비하며 GitOps 조정을 수행하는 방식을 시뮬레이션한다. 특히 `needRefreshAppStatus()`의 판단 로직, `autoSync()` 7가지 가드 조건, self-heal 지수 백오프를 실제 소스코드 기반으로 구현한다.

## 다루는 개념

| 개념 | 설명 | 실제 소스 |
|------|------|-----------|
| appRefreshQueue | 상태 갱신이 필요한 앱 큐 (rate limiting) | `controller/appcontroller.go` |
| appOperationQueue | Sync 작업이 필요한 앱 큐 | `controller/appcontroller.go` |
| CompareWith 레벨 | Nothing/Recent/Latest/LatestForceResolve | `controller/appcontroller.go` |
| needRefreshAppStatus() | 갱신 필요 여부 판단 (어노테이션/변경/타임아웃) | `controller/appcontroller.go` |
| processAppRefreshQueueItem() | CompareAppState → autoSync → persistStatus | `controller/appcontroller.go` |
| processAppOperationQueueItem() | API에서 직접 앱 조회 (인포머 캐시 아님) | `controller/appcontroller.go` |
| autoSync() 7가지 가드 | Sync 차단 조건 체계적 검사 | `controller/appcontroller.go` |
| selfHeal 백오프 | 5s × 2^n (최대 3분) 지수 백오프 | `controller/appcontroller.go` |

## Reconciliation 흐름

```
K8s Informer (Application Watch)
       │
       ▼
appRefreshQueue
       │
       ▼
processAppRefreshQueueItem()
  ├─ needRefreshAppStatus()?
  │    ├─ forced-refresh annotation → CompareWithLatestForceResolve
  │    ├─ git revision changed      → CompareWithLatest
  │    ├─ refresh timeout           → CompareWithRecent
  │    └─ never reconciled         → CompareWithLatest
  │
  ├─ compareAppState(compareWith)
  │    ├─ Repo Server: 매니페스트 생성
  │    └─ K8s API: live state 조회 → diff
  │
  ├─ autoSync()?
  │    ├─ Guard 1: autoSync 활성?
  │    ├─ Guard 2: 이미 Synced?
  │    ├─ Guard 3: Operation 진행 중?
  │    ├─ Guard 4: SyncWindow 허용?
  │    ├─ Guard 5: selfHeal 없는데 클러스터 변경?
  │    ├─ Guard 6: selfHeal 백오프 경과?
  │    └─ Guard 7: 매니페스트 비어있음?
  │         └─ 모두 통과 → appOperationQueue에 추가
  │
  └─ persistAppStatus() → K8s API Status 업데이트

appOperationQueue
       │
       ▼
processAppOperationQueueItem()
  ├─ API 서버에서 직접 앱 조회 (인포머 캐시 X)
  ├─ Sync 실행 (kubectl apply)
  └─ Status 업데이트 (Synced/Healthy)
```

## CompareWith 레벨

| 레벨 | 이름 | 동작 | 사용 시점 |
|------|------|------|-----------|
| 0 | Nothing | 비교 생략 | 이미 최신 상태 |
| 1 | Recent | 캐시된 매니페스트 사용 | 타임아웃 갱신 |
| 2 | Latest | Git HEAD 최신 조회 | webhook, 소스 변경 |
| 3 | LatestForceResolve | 브랜치/태그 강제 재해석 | hard refresh |

## autoSync 7가지 가드 조건

| 번호 | 조건 | 차단 이유 |
|------|------|-----------|
| 1 | autoSync 정책 비활성 | 수동 sync 앱은 자동 sync 안 됨 |
| 2 | 이미 Synced + 리비전 동일 | 불필요한 sync 방지 |
| 3 | Operation 이미 진행 중 | 중복 sync 방지 |
| 4 | SyncWindow deny | 시간 기반 배포 제어 |
| 5 | selfHeal=false, 클러스터 직접 변경 | 명시적 허용 없으면 클러스터 변경 유지 |
| 6 | selfHeal 백오프 미경과 | 빠른 재시도 루프 방지 |
| 7 | 빈 매니페스트 + allowEmpty=false | 실수로 모든 리소스 삭제 방지 |

## 실행 방법

```bash
cd poc-03-reconciliation
go run main.go
```

### 예상 출력

```
=================================================================
 Argo CD Application Controller Reconciliation 시뮬레이션
=================================================================

[ 1단계: needRefreshAppStatus() — 갱신 필요 여부 판단 ]
  처음 reconcile (ReconciledAt=zero)         → needRefresh=true, Latest(2), reason=never-reconciled
  이미 Synced, 타임아웃 미경과               → needRefresh=false, Nothing(0), reason=
  hard refresh 어노테이션                     → needRefresh=true, LatestForceResolve(3), reason=forced-refresh-annotation
  OutOfSync 상태                             → needRefresh=true, Latest(2), reason=git-source-changed

[ 2단계: autoSync() 7가지 가드 조건 ]
  Guard 1: autoSync 비활성                   → 차단
  Guard 2: 이미 Synced + 리비전 동일         → 차단
  ...
  모든 가드 통과 → Sync 허용                 → 허용

[ 3단계: 전체 Reconciliation 사이클 ]
  ...

[ 4단계: Self-heal 백오프 계산 ]
  attempt | 대기 시간
    0     | 5s
    1     | 10s
    2     | 20s
    3     | 40s
    4     | 1m20s
    5     | 2m40s
```

## 참조 소스코드

| 파일 | 함수 | 설명 |
|------|------|------|
| `controller/appcontroller.go` | `needRefreshAppStatus()` | 갱신 필요 여부 판단 |
| `controller/appcontroller.go` | `processAppRefreshQueueItem()` | Refresh 워크큐 처리 |
| `controller/appcontroller.go` | `processAppOperationQueueItem()` | Operation 워크큐 처리 |
| `controller/appcontroller.go` | `autoSync()` | 자동 sync 트리거 |
| `controller/appcontroller.go` | `compareAppState()` | Git vs 클러스터 비교 |
| `controller/appcontroller.go` | `persistAppStatus()` | K8s API Status 업데이트 |

## 핵심 설계 결정

**왜 두 개의 큐를 분리하는가?**
상태 갱신(Refresh)과 Sync 실행(Operation)은 빈도가 다르다. Refresh는 매 3분마다 발생하지만, Sync는 변경이 있을 때만 발생한다. 큐를 분리함으로써 각각 독립적인 worker 수와 rate limiting을 적용할 수 있다.

**왜 Operation 큐에서 인포머 캐시 대신 API 서버를 직접 조회하는가?**
Sync 도중 Application 상태가 변경될 수 있다. 인포머 캐시는 수십 밀리초 지연이 있으므로, Operation 처리에서는 항상 API 서버에서 최신 상태를 조회하여 stale read를 방지한다.

**왜 7개 가드 조건인가?**
자동 동기화는 의도치 않은 상황에서 실행되면 치명적일 수 있다. 빈 매니페스트 삭제, 중복 sync, 배포 금지 시간 무시 등의 사고를 방지하기 위해 다층적 검사를 수행한다.
