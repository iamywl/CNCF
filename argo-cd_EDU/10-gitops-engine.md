# Argo CD GitOps Engine 심화 분석

## 목차

1. [GitOps Engine 개요](#1-gitops-engine-개요)
2. [패키지 구조와 의존 관계](#2-패키지-구조와-의존-관계)
3. [Diff 엔진](#3-diff-엔진)
4. [ThreeWayDiff 알고리즘](#4-threewaydiff-알고리즘)
5. [ServerSideDiff](#5-serversijediff)
6. [StructuredMergeDiff](#6-structuredmergediff)
7. [Sync 엔진](#7-sync-엔진)
8. [Sync Waves와 Hooks](#8-sync-waves와-hooks)
9. [Health 엔진](#9-health-엔진)
10. [Cluster Cache](#10-cluster-cache)
11. [GitOpsEngine 인터페이스 (최상위 조합)](#11-gitopsengine-인터페이스-최상위-조합)
12. [왜 이런 설계인가](#12-왜-이런-설계인가)

---

## 1. GitOps Engine 개요

GitOps Engine은 Argo CD의 핵심 GitOps 로직을 독립 라이브러리로 분리한 패키지다. Argo CD 리포지토리 내부에 `gitops-engine/` 디렉토리로 존재하며, 자체 `go.mod`를 가진 별도 모듈이다.

```
/Users/ywlee/CNCF/argo-cd/gitops-engine/
├── go.mod                    # 별도 Go 모듈 선언
├── pkg/
│   ├── cache/                # 클러스터 리소스 인메모리 캐시
│   ├── diff/                 # 리소스 diff 계산 엔진
│   ├── engine/               # 최상위 GitOpsEngine 인터페이스
│   ├── health/               # 리소스 헬스 체크
│   ├── sync/                 # 동기화 실행 엔진
│   └── utils/                # kube 유틸리티, JSON 유틸리티
└── agent/                    # 독립 에이전트 예제
```

### 4개 핵심 패키지 역할

| 패키지 | 파일 크기 | 역할 |
|--------|----------|------|
| `pkg/diff` | diff.go: 1,208줄 | Git 설정 ↔ 클러스터 상태 비교 |
| `pkg/sync` | sync_context.go: 1,762줄 | 동기화 실행 상태 머신 |
| `pkg/health` | health.go: 152줄 | 리소스 건강 상태 판단 |
| `pkg/cache` | cluster.go: 1,653줄 | 클러스터 리소스 Watch/캐시 |

```
┌─────────────────────────────────────────────────────────────────┐
│                    GitOps Engine                                 │
│                                                                  │
│   ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐ │
│   │  cache   │    │   diff   │    │   sync   │    │  health  │ │
│   │          │    │          │    │          │    │          │ │
│   │ Watch    │───▶│ ThreeWay │    │ SyncCtx  │───▶│ GetHealth│ │
│   │ ListPage │    │ ServerSide│   │ Waves    │    │ Override │ │
│   │ UID Index│    │ Structured│   │ Hooks    │    │ Built-in │ │
│   └──────────┘    └──────────┘    └──────────┘    └──────────┘ │
│         │              │               │                │        │
│         └──────────────┴───────────────┴────────────────┘        │
│                              engine.go                           │
└─────────────────────────────────────────────────────────────────┘
```

### 독립 라이브러리인 이유

`gitops-engine/go.mod`를 보면 Argo CD와 별도로 버전 관리된다. 원칙적으로 Argo CD 외에 다른 GitOps 툴도 이 엔진을 가져다 쓸 수 있다는 설계 의도다. Argo CD는 내부적으로 `gitops-engine/pkg/` 경로를 직접 참조하므로 vendoring 없이 동일 리포지토리 내에서 공유한다.

---

## 2. 패키지 구조와 의존 관계

```
engine.go
    │
    ├── cache.ClusterCache          (EnsureSynced, GetManagedLiveObjs)
    │       └── cache/cluster.go   Watch API + List pager + parentUIDToChildren 인덱스
    │
    ├── diff.DiffArray              (config vs live 비교)
    │       └── diff/diff.go       4가지 모드 결정 트리
    │
    ├── sync.Reconcile              (target+live → ReconciliationResult)
    │       └── sync/reconcile.go  splitHooks + dedupLiveResources
    │
    └── sync.NewSyncContext         (Sync 상태 머신)
            └── sync/sync_context.go  getSyncTasks → runTasks → GetState
                    └── health.GetResourceHealth
```

### 주요 데이터 흐름

```
Git 매니페스트
      │
      ▼
[cache.GetManagedLiveObjs]   클러스터 현재 상태와 매칭
      │
      ▼
[sync.Reconcile]             Target[] + Live[] + Hooks[] 구성
      │
      ▼
[diff.DiffArray]             변경 여부 판단 (4가지 diff 모드)
      │
      ▼
[sync.NewSyncContext]        Sync 상태 머신 초기화
      │
      ▼
[syncCtx.Sync() 반복]        Wave 순서대로 runTasks → kubectl apply
      │
      ▼
[syncCtx.GetState()]         OperationPhase: Running/Succeeded/Failed
```

---

## 3. Diff 엔진

**파일**: `gitops-engine/pkg/diff/diff.go` (1,208줄)

### DiffResult 구조체

```go
// gitops-engine/pkg/diff/diff.go:43-50
type DiffResult struct {
    // Modified is set to true if resources are not matching
    Modified bool
    // Contains YAML representation of a live resource with applied normalizations
    NormalizedLive []byte
    // Contains "expected" YAML representation of a live resource
    PredictedLive []byte
}
```

- `NormalizedLive`: 클러스터 현재 상태 (정규화 적용 후)
- `PredictedLive`: Argo CD가 적용했을 때 예측되는 상태
- `Modified`: 두 값이 다르면 `true` — Out-of-Sync 판정의 근거

### 4가지 Diff 모드 결정 트리

`Diff()` 함수가 진입점으로, 아래 순서로 모드를 결정한다.

```
gitops-engine/pkg/diff/diff.go:76-134
```

```
Diff(config, live, opts...)
        │
        ▼
 o.serverSideDiff == true?
        │ Yes ──────────────────────────▶ ServerSideDiff()
        │                                 kubectl apply --dry-run=server
        │ No
        ▼
 structuredMergeDiff == true
 OR config에 "ServerSideApply=true" 어노테이션?
        │ Yes ──────────────────────────▶ StructuredMergeDiff()
        │                                 sigs.k8s.io/structured-merge-diff
        │ No
        ▼
 live에 last-applied-configuration
 어노테이션이 존재하는가?
        │ Yes ──────────────────────────▶ ThreeWayDiff(orig, config, live)
        │                                 orig = last-applied-configuration
        │ No (or error)
        ▼
 TwoWayDiff(config, live)
        └──────────────────────────────▶ ThreeWayDiff(config, config, live)
                                          orig = config (사실상 two-way)
```

```go
// structuredMergeDiff 결정 로직 (L.113-114)
structuredMergeDiff := o.structuredMergeDiff ||
    (config != nil && resource.HasAnnotationOption(config, syncOptAnnotation, ssaAnnotation))
```

### DiffResultList

```go
type DiffResultList struct {
    Diffs    []DiffResult
    Modified bool
}
```

`DiffArray()`는 configArray와 liveArray를 쌍으로 받아 각 리소스를 개별 비교한 후 `DiffResultList`를 반환한다. 하나라도 `Modified=true`이면 `DiffResultList.Modified=true`다.

---

## 4. ThreeWayDiff 알고리즘

**파일**: `gitops-engine/pkg/diff/diff.go:689-725`

ThreeWayDiff는 `orig`(이전에 적용된 설정), `config`(현재 Git 설정), `live`(클러스터 현재 상태) 세 가지를 입력으로 받는다.

```go
// gitops-engine/pkg/diff/diff.go:689-725
func ThreeWayDiff(orig, config, live *unstructured.Unstructured) (*DiffResult, error) {
    orig = removeNamespaceAnnotation(orig)
    config = removeNamespaceAnnotation(config)

    // 1. calculate a 3-way merge patch
    patchBytes, newVersionedObject, err := threeWayMergePatch(orig, config, live)
    if err != nil {
        return nil, err
    }

    // 2. get expected live object by applying the patch against the live object
    liveBytes, err := json.Marshal(live)
    ...
    var predictedLiveBytes []byte
    if newVersionedObject != nil {
        // Apply patch while applying scheme defaults
        liveBytes, predictedLiveBytes, err = applyPatch(liveBytes, patchBytes, newVersionedObject)
    } else {
        // Otherwise, merge patch directly as JSON
        predictedLiveBytes, err = jsonpatch.MergePatch(liveBytes, patchBytes)
    }

    return buildDiffResult(predictedLiveBytes, liveBytes), nil
}
```

### threeWayMergePatch 내부 분기

```go
// gitops-engine/pkg/diff/diff.go:773-821
func threeWayMergePatch(orig, config, live *unstructured.Unstructured) ([]byte, func() (runtime.Object, error), error) {
    // K8s 등록 타입인 경우
    if versionedObject, err := scheme.Scheme.New(orig.GroupVersionKind()); err == nil {
        // StatefulSet 특별 처리
        if (gk.Group == "apps" || gk.Group == "extensions") && gk.Kind == "StatefulSet" {
            live = statefulSetWorkaround(orig, live)
        }
        // Strategic Merge Patch 사용
        patch, err := strategicpatch.CreateThreeWayMergePatch(origBytes, configBytes, liveBytes, lookupPatchMeta, true)
        return patch, newVersionedObject, nil
    }
    // CRD (미등록 타입)인 경우
    // live에서 orig에 없는 필드를 제거 (defaulted fields 제거)
    live = &unstructured.Unstructured{Object: jsonutil.RemoveMapFields(orig.Object, live.Object)}
    // JSON Merge Patch 사용
    patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch(origBytes, configBytes, liveBytes)
    return patch, nil, nil
}
```

### 알고리즘 단계별 설명

```
Step 1: removeNamespaceAnnotation(orig), removeNamespaceAnnotation(config)
        ─ namespace 필드와 빈 annotations 맵 제거
        ─ live에는 namespace가 있지만 orig/config에는 없는 경우가 많아 불필요한 diff 방지

Step 2: threeWayMergePatch(orig, config, live)
        ─ K8s 등록 타입: strategicpatch.CreateThreeWayMergePatch
            (list merge key 지원, 배열 요소 병합 가능)
        ─ CRD 미등록 타입: jsonutil.RemoveMapFields + jsonpatch.CreateMergePatch
            (orig에 없는 live 필드를 먼저 제거하여 defaulted fields 노이즈 차단)

Step 3: predictedLive 계산
        ─ K8s 등록 타입: applyPatch() → scheme defaults도 함께 적용
        ─ CRD: jsonpatch.MergePatch(liveBytes, patchBytes)

Step 4: buildDiffResult(predictedLiveBytes, liveBytes)
        ─ string 비교로 Modified 여부 결정
```

### 왜 Three-way인가

```
orig(last-applied) ──▶ config: Argo CD가 변경한 것
orig(last-applied) ──▶ live:   다른 주체가 변경한 것 (사용자 수동 변경, 다른 컨트롤러)

Three-way merge는 "Argo CD가 변경한 것"만 diff로 표시하고
"다른 주체가 변경한 것"은 보존한다.
```

실제 예시:

```yaml
# orig (last-applied)
spec:
  replicas: 1

# config (Git 현재)
spec:
  replicas: 3     # Argo CD가 변경

# live (클러스터)
spec:
  replicas: 2     # 사용자가 수동 변경

# Three-way 결과 (predictedLive)
spec:
  replicas: 3     # Argo CD 변경이 우선

# Two-way 결과 (naive)
spec:
  replicas: 3     # 같지만, 사용자 변경 2를 무시했다는 것을 알 수 없음
```

---

## 5. ServerSideDiff

**파일**: `gitops-engine/pkg/diff/diff.go:140-293`

### 동작 원리

```go
// gitops-engine/pkg/diff/diff.go:165-204
func serverSideDiff(config, live *unstructured.Unstructured, opts ...Option) (*DiffResult, error) {
    o := applyOptions(opts)
    // 1. kubectl apply --dry-run=server 실행
    predictedLiveStr, err := o.serverSideDryRunner.Run(context.Background(), config, o.manager)
    ...
    predictedLive, err := jsonStrToUnstructured(predictedLiveStr)

    // 2. mutation webhook이 주입한 필드 제거 (기본 활성화)
    if o.ignoreMutationWebhook {
        predictedLive, err = removeWebhookMutation(predictedLive, live, o.gvkParser, o.manager)
    }

    // 3. 정규화 및 managedFields 제거
    predictedLive = remarshal(predictedLive, o)
    Normalize(predictedLive, opts...)
    unstructured.RemoveNestedField(predictedLive.Object, "metadata", "managedFields")

    // 4. buildDiffResult
    return buildDiffResult(predictedLiveBytes, liveBytes), nil
}
```

### ServerSideDryRunner 인터페이스

```go
// gitops-engine/pkg/diff/diff_options.go:49-52
type ServerSideDryRunner interface {
    Run(ctx context.Context, obj *unstructured.Unstructured, manager string) (string, error)
}
```

`K8sServerSideDryRunner.Run()`은 내부적으로:

```go
// gitops-engine/pkg/diff/diff_options.go:71-73
func (kdr *K8sServerSideDryRunner) Run(ctx context.Context, obj *unstructured.Unstructured, manager string) (string, error) {
    return kdr.dryrunApplier.ApplyResource(ctx, obj, cmdutil.DryRunServer, false, false, true, manager)
}
```

`cmdutil.DryRunServer` = `kubectl apply --dry-run=server`에 해당한다.

### removeWebhookMutation 알고리즘

mutation webhook이 `predictedLive`에 추가한 필드를 제거하는 로직이다.

```
gitops-engine/pkg/diff/diff.go:212-293
```

```
Step 1: predictedLive.GetManagedFields() 추출
        ─ managedFields가 없으면 에러 반환 (SSA가 활성화되지 않은 경우)

Step 2: GvkParser.Type(gvk) → ParseableType
        ─ K8s 타입 스키마 정보 획득

Step 3: pt.FromUnstructured(predictedLive.Object) → typedPredictedLive
        pt.FromUnstructured(live.Object) → typedLive

Step 4: managedFields를 순회하며 지정된 manager(Argo CD)가 소유한 필드 집합 계산
        ─ managerFieldsSet = Union of all fields owned by manager

Step 5: predictedLive 전체 필드 집합에서 managerFieldsSet을 뺀다
        ─ nonArgoFieldsSet = predictedLiveFieldSet.Difference(managerFieldsSet)
        ─ 이것이 webhook이 주입한 필드들

Step 6: composite key 필드 보존 (list merge 에러 방지)
        ─ filterOutCompositeKeyFields()로 연관 list key 필드 제외

Step 7: typedPredictedLive.RemoveItems(nonArgoFieldsSet)
        ─ Argo CD 소유가 아닌 필드 제거

Step 8: typedLive.Merge(typedPredictedLive)
        ─ 제거된 필드를 live 값으로 복원 (webhook 주입 필드 = live 그대로)

Step 9: removed 필드 재제거
        ─ typedPredictedLive.RemoveItems(comparison.Removed)
        ─ Argo CD가 명시적으로 삭제한 필드는 webhook 복원에서 제외
```

```
predictedLive (after SSA dry-run)
     │
     ├── Argo CD owned fields (managerFieldsSet)    ←── 보존
     ├── webhook injected fields (nonArgoFieldsSet) ←── 제거 후 live 값으로 대체
     └── Argo CD removed fields (comparison.Removed) ←── live 복원에서도 제외
```

---

## 6. StructuredMergeDiff

**파일**: `gitops-engine/pkg/diff/diff.go:358-464`

Server-Side Apply(SSA)를 사용하는 리소스에 대한 클라이언트 측 diff 계산이다. K8s API 서버와 동일한 라이브러리(`sigs.k8s.io/structured-merge-diff`)를 사용한다.

```go
// gitops-engine/pkg/diff/diff.go:382-430
func structuredMergeDiff(p *SMDParams) (*DiffResult, error) {
    gvk := p.config.GetObjectKind().GroupVersionKind()
    pt := gescheme.ResolveParseableType(gvk, p.gvkParser)

    // Build typed value from live and config unstructures
    tvLive, err := pt.FromUnstructured(p.live.Object)
    tvConfig, err := pt.FromUnstructured(p.config.Object)

    // Invoke the apply function (same logic as K8s server-side apply)
    mergedLive, err := apply(tvConfig, tvLive, p)

    if mergedLive == nil {
        // No change: predictedLive = live
        return buildDiffResult(liveBytes, liveBytes), nil
    }

    predictedLive, err := normalizeTypedValue(mergedLive)
    taintedLive, err := normalizeTypedValue(tvLive)
    return buildDiffResult(predictedLive, taintedLive), nil
}
```

### apply() — K8s 서버와 동일한 함수 호출

```go
// gitops-engine/pkg/diff/diff.go:435-463
func apply(tvConfig, tvLive *typed.TypedValue, p *SMDParams) (*typed.TypedValue, error) {
    // Build the structured-merge-diff Updater (K8s 내부와 동일)
    updater := merge.Updater{
        Converter: fieldmanager.NewVersionConverter(p.gvkParser, scheme.Scheme, ...),
    }

    managed, err := fieldmanager.DecodeManagedFields(p.live.GetManagedFields())
    version := fieldpath.APIVersion(p.config.GetAPIVersion())
    managerKey, err := buildManagerInfoForApply(p.manager)

    // updater.Apply = K8s API 서버가 SSA에서 실제로 호출하는 함수
    mergedLive, _, err := updater.Apply(tvLive, tvConfig, version, managed.Fields(), managerKey, true)
    return mergedLive, nil
}
```

### 세 가지 diff 방식 비교

| 방식 | 사용 시점 | 장점 | 단점 |
|------|----------|------|------|
| TwoWayDiff | last-applied 없음 | 단순 | webhook 주입 필드가 diff에 포함 |
| ThreeWayDiff | last-applied 있음 | 수동 변경 보존 | CRD에서 정확도 제한 |
| StructuredMergeDiff | SSA 어노테이션 | 필드 소유권 정확 | GVKParser 필요 |
| ServerSideDiff | serverSideDiff 옵션 | 가장 정확 | K8s API 서버 호출 비용 |

---

## 7. Sync 엔진

**파일**: `gitops-engine/pkg/sync/sync_context.go` (1,762줄)

### SyncContext 인터페이스

```go
// gitops-engine/pkg/sync/sync_context.go:53-61
type SyncContext interface {
    // 동기화 작업 비동기 종료 (진행 중인 hook 삭제, 상태 업데이트)
    Terminate()
    // 다음 동기화 스텝 실행 및 상태 업데이트
    Sync()
    // 현재 동기화 상태 및 리소스 목록 반환
    GetState() (common.OperationPhase, string, []common.ResourceSyncResult)
}
```

### syncContext 내부 구조

```go
// gitops-engine/pkg/sync/sync_context.go:346-
type syncContext struct {
    healthOverride      health.HealthOverride
    permissionValidator common.PermissionValidator
    resources           map[kubeutil.ResourceKey]reconciledResource
    hooks               []*unstructured.Unstructured
    // ...
    phase    common.OperationPhase   // Running/Succeeded/Failed/Error
    message  string
    syncRes  map[string]common.ResourceSyncResult
    startedAt time.Time
    // Sync 옵션
    validate             bool
    skipHooks            bool
    pruneLast            bool
    applyOutOfSyncOnly   bool
    serverSideApply      bool
    serverSideApplyManager string
}
```

### Sync() 상태 머신 전체 흐름

```go
// gitops-engine/pkg/sync/sync_context.go:449-660
func (sc *syncContext) Sync() {
    // Step 1: 모든 sync task 생성
    tasks, ok := sc.getSyncTasks()
    if !ok {
        sc.setOperationPhase(OperationFailed, "...")
        return
    }

    if !sc.started() {
        // Step 2: 첫 호출 — dry-run ALL tasks (검증)
        if sc.runTasks(dryRunTasks, true) == failed {
            sc.setOperationPhase(OperationFailed, "one or more objects failed to apply (dry run)")
            return
        }
    }

    // Step 3: 실행 중인 task의 상태 업데이트
    for _, task := range tasks.Filter(func(t *syncTask) bool {
        return t.running() && t.liveObj != nil
    }) {
        if task.isHook() {
            operationState, message, err := sc.getOperationPhase(task.liveObj)
            sc.setResourceResult(task, ...)
        } else {
            healthStatus, _ := health.GetResourceHealth(task.liveObj, sc.healthOverride)
            // Healthy → Succeeded, Degraded → Failed
        }
    }

    // Step 4: running task 있으면 대기
    runningTasks := tasks.Filter(func(t *syncTask) bool { return (multiStep || t.isHook()) && t.running() })
    if runningTasks.Len() > 0 {
        sc.setRunningPhase(runningTasks, false)
        return
    }

    // Step 5: prune된 리소스의 deletion 완료 대기
    prunedTasksPendingDelete := tasks.Filter(func(t *syncTask) bool {
        return t.pruned() && t.liveObj != nil && t.liveObj.GetDeletionTimestamp() != nil
    })
    if prunedTasksPendingDelete.Len() > 0 {
        sc.setRunningPhase(prunedTasksPendingDelete, true)
        return
    }

    // Step 6: 완료된 hook finalizer 정리
    for _, task := range hooksCompleted {
        sc.removeHookFinalizer(task)
    }

    // Step 7: 성공/실패 조건의 hook 수집
    hooksPendingDeletionSuccessful := ...
    hooksPendingDeletionFailed := ...

    // Step 8: SyncFail hook 분리
    syncFailTasks, tasks := tasks.Split(func(t *syncTask) bool {
        return t.phase == common.SyncPhaseSyncFail
    })

    // Step 9: 실패 task 있으면 SyncFail phase 실행
    if tasks.Any(func(t *syncTask) bool { return t.completed() && !t.successful() }) {
        sc.executeSyncFailPhase(syncFailTasks, syncFailedTasks, "...")
        return
    }

    // Step 10: 완료된 task 제거 (pending만 남김)
    tasks = tasks.Filter(func(t *syncTask) bool { return t.pending() })

    // Step 11: applyOutOfSyncOnly 필터
    if sc.applyOutOfSyncOnly {
        tasks = sc.filterOutOfSyncTasks(tasks)
    }

    // Step 12: 남은 task 없으면 성공
    if len(tasks) == 0 {
        sc.deleteHooks(hooksPendingDeletionSuccessful)
        sc.setOperationPhase(OperationSucceeded, "successfully synced (no more tasks)")
        return
    }

    // Step 13: 현재 phase + wave 결정 (가장 낮은 것부터)
    phase := tasks.phase()
    wave := tasks.wave()

    // Step 14: 현재 phase+wave에 해당하는 task만 필터
    tasks, remainingTasks := tasks.Split(func(t *syncTask) bool {
        return t.phase == phase && t.wave() == wave
    })

    // Step 15: runTasks() — 실제 kubectl apply
    runState := sc.runTasks(tasks, false)

    // Step 16: syncWaveHook 호출 (wave 완료 콜백)
    if sc.syncWaveHook != nil && runState != failed {
        sc.syncWaveHook(phase, wave, finalWave)
    }
    ...
}
```

### Sync 상태 머신 다이어그램

```
           ┌─────────────────────────────────────────────────────────────┐
           │                    Sync() 호출                              │
           └──────────────────────────┬──────────────────────────────────┘
                                      │
                        ┌─────────────▼─────────────┐
                        │     getSyncTasks()         │
                        │  resource + hook tasks     │
                        └─────────────┬─────────────┘
                                      │
                        ┌─────────────▼─────────────┐
                  No    │  sc.started() ?            │
           ┌────────────│  (syncRes 비어있음)         │
           │            └─────────────┬─────────────┘
           │                   Yes    │
           ▼                          │
    ┌──────────────┐                  ▼
    │ dry-run ALL  │         ┌─────────────────┐
    │ (검증)       │         │ running task    │
    └──────┬───────┘         │ 상태 업데이트   │
           │ failed          └────────┬────────┘
           │                         │ running있음
           ▼                         ▼
    ┌──────────────┐         ┌─────────────────┐
    │ OperationFailed│        │  대기 (return)  │
    └──────────────┘         └─────────────────┘
                                      │ running없음
                                      ▼
                             ┌─────────────────┐
                             │ prune pending   │
                             │ deletion 대기?  │
                             └────────┬────────┘
                                      │ 없음
                                      ▼
                             ┌─────────────────┐
                             │ hook finalizer  │
                             │ 정리            │
                             └────────┬────────┘
                                      │
                                      ▼
                             ┌─────────────────┐
                             │ 실패 task 있음? │
                             └────────┬────────┘
                              있음    │    없음
                               ▼      │     ▼
                        SyncFail      │  pending tasks
                        phase         │  applyOutOfSyncOnly 필터
                                      │
                                      ▼
                             ┌─────────────────┐
                             │ tasks 없음?     │
                             └────────┬────────┘
                              없음    │    있음
                               ▼      │     ▼
                        Succeeded     │  wave + phase 결정
                                      │  runTasks() → kubectl apply
                                      │  syncWaveHook 호출
                                      │
                                      ▼ (다음 Sync() 호출 대기)
```

---

## 8. Sync Waves와 Hooks

### Phase 순서

```go
// gitops-engine/pkg/sync/common/types.go:67-72
const (
    SyncPhasePreSync  = "PreSync"
    SyncPhaseSync     = "Sync"
    SyncPhasePostSync = "PostSync"
    SyncPhaseSyncFail = "SyncFail"
)
```

```
실행 순서: PreSync → Sync → PostSync
                                  ↘
                         (실패 시) SyncFail
```

### syncTask 구조체

```go
// gitops-engine/pkg/sync/sync_task.go:18-27
type syncTask struct {
    phase          common.SyncPhase      // PreSync/Sync/PostSync/SyncFail
    liveObj        *unstructured.Unstructured  // nil이면 신규 생성
    targetObj      *unstructured.Unstructured  // nil이면 prune
    skipDryRun     bool
    syncStatus     common.ResultCode     // Synced/SyncFailed/Pruned/PruneSkipped
    operationState common.OperationPhase // Running/Succeeded/Failed
    message        string
    waveOverride   *int                  // prune 역순/pruneLast 구현에 사용
}
```

`targetObj == nil`이면 prune task, `liveObj == nil`이면 신규 생성 task다.

### Wave 결정 로직

```go
// gitops-engine/pkg/sync/syncwaves/waves.go:12-21
func Wave(obj *unstructured.Unstructured) int {
    text, ok := obj.GetAnnotations()[common.AnnotationSyncWave]
    if ok {
        val, err := strconv.Atoi(text)
        if err == nil {
            return val  // argocd.argoproj.io/sync-wave 어노테이션 값
        }
    }
    return helmhook.Weight(obj)  // Helm hook weight (fallback)
}
```

Wave 어노테이션 사용 예:

```yaml
metadata:
  annotations:
    argocd.argoproj.io/sync-wave: "5"
```

### Prune Wave 역순 처리

삭제 작업은 생성의 역순으로 실행한다. Wave 0 → 1 → 2로 생성했다면, 삭제는 Wave 2 → 1 → 0 순서다.

```go
// gitops-engine/pkg/sync/sync_context.go:1031-1059
// symmetric swap on prune waves
n := len(uniquePruneWaves)
for i := 0; i < n/2; i++ {
    startWave := uniquePruneWaves[i]
    endWave := uniquePruneWaves[n-1-i]

    // wave i와 wave (n-1-i)를 교환
    for _, task := range pruneTasks[startWave] {
        task.waveOverride = &endWave
    }
    for _, task := range pruneTasks[endWave] {
        task.waveOverride = &startWave
    }
}
```

```
생성 순서: Wave 0  → Wave 1  → Wave 2
삭제 순서: Wave 2  → Wave 1  → Wave 0  (역순)

이유: 의존성 있는 리소스를 안전하게 삭제하기 위함
     예) Deployment(wave 1)를 삭제한 후 Namespace(wave 0)를 삭제
```

### PruneLast

```go
// gitops-engine/pkg/sync/sync_context.go:1073-1078
syncPhaseLastWave = syncPhaseLastWave + 1  // 마지막 wave + 1
for _, task := range tasks {
    if task.isPrune() &&
        (sc.pruneLast || resourceutil.HasAnnotationOption(task.liveObj, ...SyncOptionPruneLast)) {
        task.waveOverride = &syncPhaseLastWave
    }
}
```

`PruneLast=true` 어노테이션이 있거나 전역 `pruneLast` 옵션이 활성화된 리소스는 모든 Sync phase가 완료된 후 마지막에 삭제된다.

### Hook 이름 합성

generateName 기반 hook의 이름은 결정론적으로 생성된다:

```go
// gitops-engine/pkg/sync/sync_context.go:928-935
if targetObj.GetName() == "" {
    var syncRevision string
    if len(sc.revision) >= 8 {
        syncRevision = sc.revision[0:7]  // git revision 앞 7자리
    } else {
        syncRevision = sc.revision
    }
    postfix := strings.ToLower(fmt.Sprintf("%s-%s-%d", syncRevision, phase, sc.startedAt.UTC().Unix()))
    generateName := obj.GetGenerateName()
    targetObj.SetName(fmt.Sprintf("%s%s", generateName, postfix))
}
```

예: `generateName: "db-migrate-"` → `db-migrate-abc1234-presync-1709500000`

### Hook Finalizer

```go
// gitops-engine/pkg/sync/hook/hook.go:13
const HookFinalizer = "argocd.argoproj.io/hook-finalizer"
```

hook이 생성될 때 이 finalizer가 추가된다. hook이 완료되면 finalizer를 제거하고 삭제 policy에 따라 hook 리소스를 삭제한다.

```go
// gitops-engine/pkg/sync/sync_task.go:121-131
func (t *syncTask) hasHookDeletePolicy(policy common.HookDeletePolicy) bool {
    for _, p := range hook.DeletePolicies(t.obj()) {
        if p == policy {
            return true
        }
    }
    return false
}
```

| DeletePolicy | 동작 |
|-------------|------|
| `HookSucceeded` | 성공 시 hook 삭제 |
| `HookFailed` | 실패 시 hook 삭제 |
| `BeforeHookCreation` | 다음 sync 전에 이전 hook 삭제 |

### Helm Hook 통합

```go
// gitops-engine/pkg/sync/hook/hook.go:43-58
func Types(obj *unstructured.Unstructured) []common.HookType {
    var types []common.HookType
    for _, text := range resourceutil.GetAnnotationCSVs(obj, common.AnnotationKeyHook) {
        t, ok := common.NewHookType(text)
        if ok {
            types = append(types, t)
        }
    }
    // Argo hook이 없을 때만 Helm hook 사용
    if len(types) == 0 {
        for _, t := range helmhook.Types(obj) {
            types = append(types, t.HookType())
        }
    }
    return types
}
```

Argo CD hook 어노테이션이 없으면 Helm hook 어노테이션(`helm.sh/hook`)을 폴백으로 사용한다.

---

## 9. Health 엔진

**파일**: `gitops-engine/pkg/health/health.go` (152줄)

### HealthStatusCode 우선순위

```go
// gitops-engine/pkg/health/health.go:44-52
var healthOrder = []HealthStatusCode{
    HealthStatusHealthy,     // 인덱스 0 (가장 좋음)
    HealthStatusSuspended,   // 인덱스 1
    HealthStatusProgressing, // 인덱스 2
    HealthStatusMissing,     // 인덱스 3
    HealthStatusDegraded,    // 인덱스 4
    HealthStatusUnknown,     // 인덱스 5 (가장 나쁨)
}
```

```
상태 심각도: Healthy < Suspended < Progressing < Missing < Degraded < Unknown

IsWorse(current, new HealthStatusCode) bool:
    new의 인덱스 > current의 인덱스이면 true
    → 더 나쁜 상태로 전이하는지 판단
    → Application 전체 health는 리소스 중 가장 나쁜 상태로 결정
```

### GetResourceHealth() 우선순위

```go
// gitops-engine/pkg/health/health.go:70-101
func GetResourceHealth(obj *unstructured.Unstructured, healthOverride HealthOverride) (*HealthStatus, error) {
    // 우선순위 1: DeletionTimestamp → Progressing (삭제 진행 중)
    if obj.GetDeletionTimestamp() != nil && !hook.HasHookFinalizer(obj) {
        return &HealthStatus{
            Status:  HealthStatusProgressing,
            Message: "Pending deletion",
        }, nil
    }

    // 우선순위 2: healthOverride (Lua 스크립트 기반 커스텀 체크)
    if healthOverride != nil {
        health, err := healthOverride.GetResourceHealth(obj)
        if health != nil {
            return health, nil
        }
    }

    // 우선순위 3: 내장 health check (GVK 기반 분기)
    if healthCheck := GetHealthCheckFunc(obj.GroupVersionKind()); healthCheck != nil {
        if health, err = healthCheck(obj); err != nil {
            health = &HealthStatus{Status: HealthStatusUnknown, Message: err.Error()}
        }
    }
    return health, err  // nil이면 health 개념 없는 리소스
}
```

### 내장 Health Check 함수 매핑

```go
// gitops-engine/pkg/health/health.go:104-152
func GetHealthCheckFunc(gvk schema.GroupVersionKind) func(*unstructured.Unstructured) (*HealthStatus, error) {
    switch gvk.Group {
    case "apps":
        switch gvk.Kind {
        case "Deployment":  return getDeploymentHealth
        case "StatefulSet": return getStatefulSetHealth
        case "ReplicaSet":  return getReplicaSetHealth
        case "DaemonSet":   return getDaemonSetHealth
        }
    case "extensions":
        if gvk.Kind == "Ingress": return getIngressHealth
    case "argoproj.io":
        if gvk.Kind == "Workflow": return getArgoWorkflowHealth
    case "apiregistration.k8s.io":
        if gvk.Kind == "APIService": return getAPIServiceHealth
    case "networking.k8s.io":
        if gvk.Kind == "Ingress": return getIngressHealth
    case "":
        switch gvk.Kind {
        case "Service":              return getServiceHealth
        case "PersistentVolumeClaim": return getPVCHealth
        case "Pod":                  return getPodHealth
        }
    case "batch":
        if gvk.Kind == "Job": return getJobHealth
    case "autoscaling":
        if gvk.Kind == "HorizontalPodAutoscaler": return getHPAHealth
    }
    return nil  // health 개념 없는 리소스 (ConfigMap, Secret 등)
}
```

| 그룹 | 종류 | 체크 로직 파일 |
|------|------|--------------|
| apps | Deployment | health_deployment.go |
| apps | StatefulSet | health_statefulset.go |
| apps | ReplicaSet | health_replicaset.go |
| apps | DaemonSet | health_daemonset.go |
| networking.k8s.io / extensions | Ingress | health_ingress.go |
| batch | Job | health_job.go |
| autoscaling | HPA | health_hpa.go |
| "" | Pod | health_pod.go |
| "" | PVC | health_pvc.go |
| "" | Service | health_service.go |
| apiregistration.k8s.io | APIService | health_apiservice.go |
| argoproj.io | Workflow | health_argo.go |

### Deployment Health 체크 예시

```go
// gitops-engine/pkg/health/health_deployment.go:28-70
func getAppsv1DeploymentHealth(deployment *appsv1.Deployment) (*HealthStatus, error) {
    if deployment.Spec.Paused {
        return &HealthStatus{Status: HealthStatusSuspended, Message: "Deployment is paused"}, nil
    }
    if deployment.Generation <= deployment.Status.ObservedGeneration {
        cond := getAppsv1DeploymentCondition(deployment.Status, appsv1.DeploymentProgressing)
        switch {
        case cond != nil && cond.Reason == "ProgressDeadlineExceeded":
            return &HealthStatus{Status: HealthStatusDegraded, ...}, nil
        case deployment.Spec.Replicas != nil && deployment.Status.UpdatedReplicas < *deployment.Spec.Replicas:
            return &HealthStatus{Status: HealthStatusProgressing, ...}, nil
        case deployment.Status.Replicas > deployment.Status.UpdatedReplicas:
            return &HealthStatus{Status: HealthStatusProgressing, ...}, nil
        case deployment.Status.AvailableReplicas < deployment.Status.UpdatedReplicas:
            return &HealthStatus{Status: HealthStatusProgressing, ...}, nil
        }
    } else {
        return &HealthStatus{Status: HealthStatusProgressing,
            Message: "Waiting for rollout to finish: observed deployment generation less than desired generation"}, nil
    }
    return &HealthStatus{Status: HealthStatusHealthy}, nil
}
```

### HealthOverride 인터페이스 (Lua 스크립트 연동)

```go
// gitops-engine/pkg/health/health.go:34-36
type HealthOverride interface {
    GetResourceHealth(obj *unstructured.Unstructured) (*HealthStatus, error)
}
```

Argo CD는 이 인터페이스를 Lua 스크립트로 구현한다. `argocd-cm` ConfigMap의 `resource.customizations.health.{group}/{kind}` 키에 Lua 스크립트를 지정하면, 해당 리소스 타입에 대해 내장 체크 대신 Lua 스크립트가 실행된다.

---

## 10. Cluster Cache

**파일**: `gitops-engine/pkg/cache/cluster.go` (1,653줄)

### ClusterCache 인터페이스

```go
// gitops-engine/pkg/cache/cluster.go:140-173
type ClusterCache interface {
    EnsureSynced() error
    GetServerVersion() string
    GetAPIResources() []kube.APIResourceInfo
    GetOpenAPISchema() openapi.Resources
    GetGVKParser() *managedfields.GvkParser
    Invalidate(opts ...UpdateSettingsFunc)
    FindResources(namespace string, predicates ...func(r *Resource) bool) map[kube.ResourceKey]*Resource
    IterateHierarchyV2(keys []kube.ResourceKey, action func(resource *Resource, namespaceResources map[kube.ResourceKey]*Resource) bool)
    IsNamespaced(gk schema.GroupKind) (bool, error)
    GetManagedLiveObjs(targetObjs []*unstructured.Unstructured, isManaged func(r *Resource) bool) (map[kube.ResourceKey]*unstructured.Unstructured, error)
    GetClusterInfo() ClusterInfo
    OnResourceUpdated(handler OnResourceUpdatedHandler) Unsubscribe
    OnEvent(handler OnEventHandler) Unsubscribe
    OnProcessEventsHandler(handler OnProcessEventsHandler) Unsubscribe
}
```

### clusterCache 내부 구조

```go
// gitops-engine/pkg/cache/cluster.go:222-278
type clusterCache struct {
    syncStatus    clusterCacheSync

    apisMeta      map[schema.GroupKind]*apiMeta   // API 그룹별 watch 메타데이터
    resources     map[kube.ResourceKey]*Resource   // 전체 리소스 인덱스
    nsIndex       map[string]map[kube.ResourceKey]*Resource  // 네임스페이스별 인덱스

    // 리소스 계층 구조 인덱스
    // UID → 직접 자식 ResourceKey 목록
    // O(1) 계층 탐색을 위한 사전 계산 인덱스
    parentUIDToChildren map[types.UID][]kube.ResourceKey

    // 페이징 설정
    listPageSize       int64   // 기본 500
    listPageBufferSize int32   // 기본 1 (1페이지 프리패치)
    listSemaphore      WeightedSemaphore  // 기본 50 동시 목록 조회 제한

    // Watch 설정
    watchResyncTimeout      time.Duration  // 기본 10분
    clusterSyncRetryTimeout time.Duration  // 기본 10초
    eventProcessingInterval time.Duration  // 기본 100ms

    // 전체 캐시 재동기화
    // defaultClusterResyncTimeout = 24시간
}
```

### 캐시 초기화 파라미터

```go
// gitops-engine/pkg/cache/cluster.go:63-84
const (
    defaultClusterResyncTimeout = 24 * time.Hour  // 24시간마다 전체 재동기화
    defaultWatchResyncTimeout   = 10 * time.Minute // 10분마다 watch 재시작
    defaultListPageSize         = 500               // K8s client-go와 동일
    defaultListPageBufferSize   = 1                 // 1페이지 프리패치
    defaultListSemaphoreWeight  = 50               // 동시 list 조회 최대 50개
    defaultEventProcessingInterval = 100 * time.Millisecond
)
```

### parentUIDToChildren 인덱스

```go
// gitops-engine/pkg/cache/cluster.go:274-277
// Parent-to-children index for O(1) hierarchy traversal
// Maps any resource's UID to its direct children's ResourceKeys
// Eliminates need for O(n) graph building during hierarchy traversal
parentUIDToChildren map[types.UID][]kube.ResourceKey
```

이 인덱스 덕분에 `IterateHierarchyV2()`는 O(1) 자식 조회가 가능하다. 기존 방식(O(n) 그래프 빌드)과 비교:

```
기존 방식 (O(n)):
  1. 전체 리소스 목록 순회
  2. 각 리소스의 ownerRefs 확인
  3. 부모-자식 그래프 동적 빌드
  4. 탐색 실행

parentUIDToChildren 방식 (O(1)):
  1. parentUIDToChildren[uid] → 직접 자식 목록 반환
  (업데이트 시점에 사전 계산되어 저장됨)
```

### Resource 구조체

```go
// gitops-engine/pkg/cache/resource.go:16-32
type Resource struct {
    ResourceVersion   string
    Ref               corev1.ObjectReference     // API 참조 정보
    OwnerRefs         []metav1.OwnerReference    // 부모 리소스 참조
    CreationTimestamp *metav1.Time
    Info              any                         // 커스텀 메타데이터
    Resource          *unstructured.Unstructured  // 전체 매니페스트 (선택적)
    isInferredParentOf func(key kube.ResourceKey) bool
}
```

`cacheManifest=true`일 때만 `Resource.Resource` 필드에 전체 매니페스트를 저장한다. 그렇지 않으면 메타데이터만 보관하여 메모리를 절약한다.

### Watch API + List Pager

```
Watch 기반 실시간 동기화:
  EnsureSynced()
      │
      ▼
  ListPage(page=500개씩)
      │ 초기 목록 로드
      ▼
  Watch(resourceVersion)
      │ 이후 변경사항 실시간 수신
      ▼
  ADDED/MODIFIED/DELETED 이벤트 → resources/nsIndex/parentUIDToChildren 갱신
      │
      ▼
  OnResourceUpdated 핸들러 호출 (Argo CD controller에 변경 알림)
```

리소스 Watch가 10분 후 만료되면 자동으로 re-list + 새 Watch를 시작한다. 전체 캐시는 24시간마다 강제로 재동기화된다.

---

## 11. GitOpsEngine 인터페이스 (최상위 조합)

**파일**: `gitops-engine/pkg/engine/engine.go`

```go
// gitops-engine/pkg/engine/engine.go:34-39
type GitOpsEngine interface {
    // 엔진 초기화 (캐시 동기화)
    Run() (StopFunc, error)
    // 클러스터 리소스 동기화
    Sync(ctx context.Context, resources []*unstructured.Unstructured,
         isManaged func(r *cache.Resource) bool, revision string, namespace string,
         opts ...sync.SyncOpt) ([]common.ResourceSyncResult, error)
}
```

### Sync() 전체 실행 흐름

```go
// gitops-engine/pkg/engine/engine.go:70-129
func (e *gitOpsEngine) Sync(...) ([]common.ResourceSyncResult, error) {
    // 1. 클러스터에서 관리 중인 live 오브젝트 조회
    managedResources, err := e.cache.GetManagedLiveObjs(resources, isManaged)

    // 2. target + live → ReconciliationResult (hooks 분리 포함)
    result := sync.Reconcile(resources, managedResources, namespace, e.cache)

    // 3. diff 계산 (변경 여부 확인)
    diffRes, err := diff.DiffArray(result.Target, result.Live, diff.WithLogr(e.log))

    // skipHooks: 변경 없으면 hook 실행 생략
    opts = append(opts, sync.WithSkipHooks(!diffRes.Modified))

    // 4. Sync 상태 머신 초기화
    syncCtx, cleanup, err := sync.NewSyncContext(revision, result, e.config, ...)
    defer cleanup()

    // 5. cache 변경 감지용 채널 구독
    resUpdated := make(chan bool)
    unsubscribe := e.cache.OnResourceUpdated(func(...) {
        resUpdated <- true
    })
    defer unsubscribe()

    // 6. 완료될 때까지 반복
    for {
        syncCtx.Sync()
        phase, message, resources := syncCtx.GetState()
        if phase.Completed() {
            return resources, err
        }
        select {
        case <-ctx.Done():         // 컨텍스트 취소
            syncCtx.Terminate()
            return resources, ctx.Err()
        case <-time.After(1 * time.Second): // 1초 타임아웃 후 재시도
        case <-resUpdated:          // 리소스 변경 감지 즉시 재시도
        }
    }
}
```

### Reconcile() — splitHooks + dedupLiveResources

```go
// gitops-engine/pkg/sync/reconcile.go:71-117
func Reconcile(targetObjs []*unstructured.Unstructured,
    liveObjByKey map[kubeutil.ResourceKey]*unstructured.Unstructured,
    namespace string, resInfo kubeutil.ResourceInfoProvider) ReconciliationResult {

    // hook과 일반 리소스 분리
    targetObjs, hooks := splitHooks(targetObjs)

    // 같은 UID의 중복 live 리소스 제거 (apps/Deployment vs extensions/Deployment)
    dedupLiveResources(targetObjs, liveObjByKey)

    // target 리소스마다 매칭되는 live 리소스 찾기
    managedLiveObj := make([]*unstructured.Unstructured, len(targetObjs))
    for i, obj := range targetObjs {
        // namespaced vs cluster-scoped 양쪽 모두 확인
        ...
    }
    // live에만 있는 리소스 → prune 대상 (target=nil, live=obj)
    for _, obj := range liveObjByKey {
        targetObjs = append(targetObjs, nil)
        managedLiveObj = append(managedLiveObj, obj)
    }
    return ReconciliationResult{
        Target: targetObjs,   // [obj1, obj2, nil, nil]
        Hooks:  hooks,        // hook 리소스 목록
        Live:   managedLiveObj, // [live1, nil, live3, live4]
    }
}
```

`Target[i]=nil, Live[i]=obj3` → prune task (삭제 대상)
`Target[i]=obj2, Live[i]=nil` → 신규 생성 task

---

## 12. 왜 이런 설계인가

### 왜 별도 라이브러리인가

```
Argo CD = GitOps Engine + UI + RBAC + SSO + Notifications + ...

GitOps Engine만 독립 라이브러리로 분리하면:
  - 다른 GitOps 도구(Flux 등)도 같은 엔진 사용 가능
  - Argo CD 자체 로직과 GitOps 핵심 로직 명확히 분리
  - 단위 테스트 용이 (K8s 클러스터 없이 엔진만 테스트)
```

### 왜 4가지 Diff 모드인가

```
K8s 리소스 관리 방식이 다양하기 때문:

  kubectl apply (CSA)     → last-applied annotation 존재 → ThreeWayDiff
  kubectl create          → last-applied 없음              → TwoWayDiff
  kubectl apply --server-side (SSA) → managedFields 존재 → StructuredMergeDiff
  Argo CD serverSideDiff 활성화    → API 서버 dry-run     → ServerSideDiff

하나의 diff 방식으로는 모든 케이스를 정확하게 처리할 수 없다.
```

### 왜 Three-way Diff인가

```
Two-way (config vs live):
  - 사용자가 live에서 수동 변경한 값도 diff로 표시됨
  - 매 sync마다 수동 변경이 덮어씌워짐

Three-way (orig vs config, orig vs live):
  - "Argo CD가 변경한 것"만 sync 대상으로 판단
  - 다른 주체의 변경은 live 상태 그대로 보존
  - Self-healing 범위를 Argo CD 관리 필드로 한정

이는 kubectl apply의 동작 방식과 동일하다.
kubectl apply도 last-applied-configuration을 사용한 3-way merge를 수행한다.
```

### 왜 Sync Wave 상태 머신인가

```
컨트롤러는 언제든지 재시작될 수 있다.
  - Pod 재스케줄, OOM Kill, 배포 업데이트 등

상태 머신 + 영속적 상태 저장(syncRes):
  - 재시작 후 어느 wave/phase까지 완료됐는지 복원 가능
  - sc.started() = len(sc.syncRes) > 0 → dry-run 재실행 방지
  - 각 리소스의 resultKey로 이미 처리된 task 식별

이를 통해 Sync 작업이 멱등성(idempotent)을 가진다.
```

### 왜 parentUIDToChildren 인덱스인가

```
Argo CD는 리소스 트리를 자주 탐색한다:
  - Application 상태 계산
  - 리소스 삭제 시 orphan 체크
  - UI 트리 렌더링

O(n) 그래프 빌드를 반복하면 대규모 클러스터에서 성능 문제:
  - 1000개 리소스 × 100번 탐색 = 100,000번 연산

parentUIDToChildren 사전 계산:
  - 업데이트 시 O(1)로 인덱스 갱신
  - 탐색 시 O(k) (k = 자식 수)
  - 클러스터 규모에 관계없이 탐색 성능 일정
```

### 왜 Watch + List Pager인가

```
순수 Polling (주기적 전체 List):
  - 단점: 클러스터 규모에 비례한 API 부하
  - 1000개 리소스를 10초마다 List = 초당 100개 처리

Watch 기반:
  - 변경된 리소스만 이벤트로 수신
  - 초기 List Pager로 500개씩 페이지 단위 로드 (메모리 스파이크 방지)
  - 이후 Watch로 delta만 수신 → API 부하 최소화

Watch 만료(10분) 후 re-list가 필요한 이유:
  - K8s API 서버의 Watch 버퍼(etcd compaction 등)로 인한 gap 발생 가능
  - re-list로 완전한 일관성 보장
```

---

## 정리 — GitOps Engine 전체 구조 요약

```
┌─────────────────────────────────────────────────────────────────┐
│                  GitOpsEngine.Sync()                             │
│                                                                  │
│  cache.GetManagedLiveObjs()                                      │
│  ─────────────────────────                                       │
│  ClusterCache                                                    │
│  ├── Watch API (실시간 변경 감지)                                │
│  ├── List Pager (500개 단위 초기 로드)                           │
│  ├── resources map (전체 리소스 인덱스)                          │
│  └── parentUIDToChildren (O(1) 계층 탐색)                       │
│                    │                                             │
│                    ▼                                             │
│  sync.Reconcile()                                                │
│  ─────────────────                                               │
│  splitHooks()        → Target[], Hooks[], Live[]                 │
│  dedupLiveResources()                                            │
│                    │                                             │
│                    ▼                                             │
│  diff.DiffArray()                                                │
│  ────────────────                                                │
│  ServerSideDiff     (serverSideDiff 옵션)                        │
│  StructuredMergeDiff (SSA 어노테이션)                            │
│  ThreeWayDiff       (last-applied 존재)                          │
│  TwoWayDiff         (그 외)                                      │
│                    │                                             │
│                    ▼                                             │
│  sync.NewSyncContext() + Sync() 반복                             │
│  ─────────────────────────────────                               │
│  getSyncTasks()   → syncTask[] (phase/wave 포함)                 │
│  dry-run ALL      → 검증 (첫 호출만)                             │
│  wave 순서 실행   → runTasks() → kubectl apply                   │
│  hook finalizer 관리                                             │
│  health.GetResourceHealth() → 상태 업데이트                      │
│  syncWaveHook 콜백                                               │
│                    │                                             │
│                    ▼                                             │
│  GetState() → OperationPhase + ResourceSyncResult[]              │
└─────────────────────────────────────────────────────────────────┘
```

GitOps Engine은 "Git 상태를 클러스터에 적용"하는 최소한의 핵심 로직만을 담고 있다. 인증, RBAC, UI, 알림은 모두 Argo CD 상위 레이어의 책임이며, 엔진 자체는 순수하게 diff → sync → health 체크의 루프에 집중한다.
