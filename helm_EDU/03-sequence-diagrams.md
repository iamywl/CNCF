# Helm v4 시퀀스 다이어그램

## 1. 개요

이 문서는 Helm v4의 주요 작업 흐름을 소스 코드 기반으로 분석하고, Mermaid 시퀀스 다이어그램으로 시각화한다. 다루는 흐름은 다음과 같다:

1. `helm install` -- 차트 설치
2. `helm upgrade` -- 릴리스 업그레이드
3. `helm rollback` -- 릴리스 롤백
4. `helm uninstall` -- 릴리스 제거
5. Hook 실행 시퀀스 -- 공통 훅 실행 로직

각 다이어그램은 실제 소스 코드의 함수 호출 순서를 따른다.

## 2. helm install 흐름

소스 경로: `pkg/cmd/install.go`, `pkg/action/install.go`

### 2.1 CLI 계층 흐름

`helm install myapp ./mychart -f values.yaml --set key=value` 명령 실행 시:

```mermaid
sequenceDiagram
    participant User
    participant CLI as pkg/cmd/install.go
    participant Action as action.Install
    participant Loader as chart/loader
    participant Values as values.Options

    User->>CLI: helm install myapp ./mychart -f values.yaml
    CLI->>CLI: newInstallCmd(cfg, out)
    CLI->>Action: action.NewInstall(cfg)
    CLI->>CLI: runInstall(args, client, valueOpts, out)

    CLI->>Action: client.NameAndChart(args)
    Action-->>CLI: name="myapp", chartRef="./mychart"

    CLI->>Action: client.LocateChart(chartRef, settings)
    Note over Action: 로컬 경로 / URL / OCI 레지스트리<br/>에서 차트 위치 확인
    Action-->>CLI: chartPath

    CLI->>Values: valueOpts.MergeValues(getters)
    Note over Values: -f values.yaml 파일 로드<br/>--set key=value 파싱<br/>--set-string, --set-json 처리
    Values-->>CLI: vals (map[string]any)

    CLI->>Loader: loader.Load(chartPath)
    Loader-->>CLI: chartRequested (*chart.Chart)

    CLI->>CLI: checkIfInstallable(accessor)
    Note over CLI: library 차트는 설치 불가

    CLI->>Action: CheckDependencies(chart, reqs)
    Note over Action: charts/ 디렉토리에<br/>모든 의존성 존재 확인

    CLI->>Action: client.RunWithContext(ctx, chart, vals)
    Action-->>CLI: release, error

    CLI->>CLI: outfmt.Write(out, statusPrinter)
```

### 2.2 Action 계층 흐름 (Install.RunWithContext)

소스 경로: `pkg/action/install.go` -- `RunWithContext()`, `performInstall()`

```mermaid
sequenceDiagram
    participant Action as Install.RunWithContext()
    participant KubeClient as cfg.KubeClient
    participant Engine as cfg.renderResources()
    participant Storage as cfg.Releases
    participant Perform as performInstall()
    participant Hook as cfg.execHook()

    Action->>KubeClient: IsReachable()
    Note over Action: 클러스터 접근 가능 확인<br/>(DryRun=client 제외)

    Action->>Action: availableName()
    Note over Action: 릴리스 이름 유효성 검사<br/>이미 사용 중인지 확인

    Action->>Action: chartutil.ProcessDependencies(chrt, vals)
    Note over Action: 서브차트 활성화/비활성화<br/>Values 병합

    alt CRD 존재 & 서버 상호작용 모드
        Action->>Action: installCRDs(crds)
        Action->>KubeClient: Build(crdManifest)
        Action->>KubeClient: Create(resources, SSA옵션)
        Action->>KubeClient: GetWaiter().Wait(resources, 60s)
        Note over Action: CRD가 클러스터에<br/>인식될 때까지 대기
    end

    Action->>Action: getCapabilities()
    Note over Action: K8s 버전, API 목록 조회<br/>(DryRun=client일 때는 기본값 사용)

    Action->>Action: ToRenderValuesWithSchemaValidation()
    Note over Action: 값 병합 + JSON Schema 검증

    Action->>Action: createRelease(chrt, vals, labels)
    Note over Action: Release 객체 생성<br/>Version=1, Status=unknown

    Action->>Engine: renderResources(chrt, valuesToRender, ...)
    Note over Engine: Go 템플릿 렌더링<br/>PostRenderer 적용<br/>Hooks/Manifests 분리
    Engine-->>Action: hooks, manifestDoc, notes

    Action->>Action: SetStatus(StatusPendingInstall)

    Action->>KubeClient: Build(manifest)
    KubeClient-->>Action: resources (ResourceList)

    Action->>Action: setMetadataVisitor(name, namespace)
    Note over Action: 리소스에 Helm 메타데이터 설정

    alt 서버 상호작용 모드 & 신규 설치
        Action->>Action: existingResourceConflict(resources)
        Note over Action: 이미 존재하는 리소스 확인<br/>충돌 시 에러 반환
    end

    alt DryRun 모드
        Action-->>Action: Description = "Dry run complete"
        Note over Action: 여기서 반환 (리소스 미생성)
    end

    alt CreateNamespace 설정
        Action->>KubeClient: Create(namespace)
    end

    alt Replace 설정
        Action->>Action: replaceRelease(rel)
        Note over Action: 이전 릴리스를 superseded로 변경
    end

    Action->>Storage: Create(rel)
    Note over Storage: 릴리스를 Storage에 저장<br/>(이 시점에서 pending-install 상태)

    Action->>Perform: performInstallCtx(ctx, rel, toBeAdopted, resources)

    rect rgb(240, 248, 255)
        Note over Perform: performInstall() - goroutine에서 실행
        Perform->>Hook: execHook(rel, HookPreInstall, ...)
        Note over Hook: pre-install 훅 실행

        alt toBeAdopted 없음
            Perform->>KubeClient: Create(resources, SSA옵션)
        else toBeAdopted 있음 (기존 리소스 인수)
            Perform->>KubeClient: Update(toBeAdopted, resources, ...)
        end

        Perform->>KubeClient: GetWaiter(strategy)
        alt WaitForJobs
            Perform->>KubeClient: waiter.WaitWithJobs(resources, timeout)
        else
            Perform->>KubeClient: waiter.Wait(resources, timeout)
        end

        Perform->>Hook: execHook(rel, HookPostInstall, ...)
        Note over Hook: post-install 훅 실행

        Perform->>Perform: SetStatus(StatusDeployed, "Install complete")
        Perform->>Storage: Update(rel)
        Note over Storage: 릴리스 상태를 deployed로 갱신
    end
```

### 2.3 Install 실패 시 흐름

소스 경로: `pkg/action/install.go` -- `failRelease()`

```mermaid
sequenceDiagram
    participant Install as Install
    participant Uninstall as Uninstall.Run()
    participant Storage as cfg.Releases

    Note over Install: performInstall() 에서 에러 발생
    Install->>Install: failRelease(rel, err)
    Install->>Install: SetStatus(StatusFailed, msg)

    alt RollbackOnFailure 설정
        Install->>Uninstall: NewUninstall(cfg)
        Install->>Uninstall: uninstall.Run(releaseName)
        Note over Uninstall: 실패한 릴리스를 완전히 제거
        Note over Install: "release failed, and has been<br/>uninstalled due to rollback-on-failure"
    else RollbackOnFailure 미설정
        Install->>Storage: Update(rel)
        Note over Storage: failed 상태로 릴리스 기록
    end
```

## 3. helm upgrade 흐름

소스 경로: `pkg/action/upgrade.go`

### 3.1 전체 흐름 (Upgrade.RunWithContext)

```mermaid
sequenceDiagram
    participant Action as Upgrade.RunWithContext()
    participant Prepare as prepareUpgrade()
    participant Perform as performUpgrade()
    participant KubeClient as cfg.KubeClient
    participant Storage as cfg.Releases
    participant Engine as cfg.renderResources()
    participant Hook as cfg.execHook()

    Action->>KubeClient: IsReachable()
    Action->>Action: chartutil.ValidateReleaseName(name)

    Action->>Prepare: prepareUpgrade(name, chrt, vals)

    rect rgb(255, 248, 240)
        Note over Prepare: prepareUpgrade() -- 업그레이드 준비
        Prepare->>Storage: Last(name)
        Note over Storage: 가장 최근 릴리스 조회
        Storage-->>Prepare: lastRelease

        Prepare->>Prepare: lastRelease.Info.Status.IsPending()?
        Note over Prepare: pending 상태면 errPending 반환<br/>(동시 실행 방지)

        alt lastRelease가 deployed 상태
            Note over Prepare: currentRelease = lastRelease
        else lastRelease가 deployed 아님
            Prepare->>Storage: Deployed(name)
            Note over Storage: 배포 중인 릴리스 조회
            Storage-->>Prepare: currentRelease
        end

        Prepare->>Prepare: reuseValues(chrt, currentRelease, vals)
        Note over Prepare: 값 정책 결정:<br/>ResetValues: 차트 기본값만<br/>ReuseValues: 이전 값 + 새 값<br/>ResetThenReuseValues: 차트 기본값 + 이전 config<br/>기본: 새 값 없으면 이전 값 복사

        Prepare->>Prepare: chartutil.ProcessDependencies(chrt, vals)
        Prepare->>Prepare: getCapabilities()

        Prepare->>Prepare: ToRenderValuesWithSchemaValidation()
        Prepare->>Engine: renderResources(chrt, valuesToRender, ...)
        Engine-->>Prepare: hooks, manifestDoc, notesTxt

        Prepare->>Prepare: getUpgradeServerSideValue(u.ServerSideApply, lastRelease.ApplyMethod)
        Note over Prepare: SSA 결정:<br/>"auto" -> 이전 릴리스 방식 유지<br/>"true" -> SSA 강제<br/>"false" -> CSA 강제

        Note over Prepare: upgradedRelease 생성<br/>Version = lastRelease.Version + 1<br/>Status = StatusPendingUpgrade
    end

    Prepare-->>Action: currentRelease, upgradedRelease, serverSideApply

    Action->>Action: cfg.Releases.MaxHistory = u.MaxHistory

    Action->>Perform: performUpgrade(ctx, currentRelease, upgradedRelease, serverSideApply)

    rect rgb(240, 255, 240)
        Note over Perform: performUpgrade()
        Perform->>KubeClient: Build(currentRelease.Manifest)
        KubeClient-->>Perform: current (ResourceList)
        Perform->>KubeClient: Build(upgradedRelease.Manifest)
        KubeClient-->>Perform: target (ResourceList)

        Perform->>Perform: setMetadataVisitor(name, namespace)

        Perform->>Perform: 신규 리소스 식별 (target - current)
        Perform->>Perform: existingResourceConflict(toBeCreated)
        Note over Perform: 새로 생성될 리소스가<br/>이미 존재하는지 확인

        alt DryRun 모드
            Perform-->>Action: upgradedRelease
        end

        Perform->>Storage: Create(upgradedRelease)
        Note over Storage: pending-upgrade 상태로 저장

        Note over Perform: goroutine: releasingUpgrade()
        Perform->>Hook: execHook(rel, HookPreUpgrade, ...)
        Note over Hook: pre-upgrade 훅 실행

        Perform->>KubeClient: Update(current, target, SSA옵션)
        Note over KubeClient: 3-way merge / SSA로<br/>리소스 업데이트

        Perform->>KubeClient: GetWaiter(strategy).Wait(target, timeout)

        Perform->>Hook: execHook(rel, HookPostUpgrade, ...)
        Note over Hook: post-upgrade 훅 실행

        Perform->>Perform: originalRelease.Status = StatusSuperseded
        Perform->>Storage: recordRelease(originalRelease)
        Note over Storage: 이전 릴리스를 superseded로 변경

        Perform->>Perform: upgradedRelease.Status = StatusDeployed
    end

    Action->>Storage: Update(upgradedRelease)
    Note over Storage: deployed 상태로 갱신
```

### 3.2 Values 재사용 정책 상세

소스 경로: `pkg/action/upgrade.go` -- `reuseValues()`

```
+-----------------------+----------------------------------+
| 플래그                | 동작                              |
+-----------------------+----------------------------------+
| --reset-values        | 차트의 values.yaml만 사용         |
|                       | 이전 릴리스 Config 무시            |
+-----------------------+----------------------------------+
| --reuse-values        | 이전 릴리스의 병합된 값을 기반으로  |
|                       | 새 값을 위에 덮어쓴다              |
|                       | chart.Values = 이전 병합값         |
|                       | newVals = CoalesceTables(newVals,  |
|                       |           current.Config)          |
+-----------------------+----------------------------------+
| --reset-then-reuse    | 차트의 values.yaml을 기반으로      |
|                       | 이전 Config을 위에 병합             |
|                       | newVals = CoalesceTables(newVals,  |
|                       |           current.Config)          |
+-----------------------+----------------------------------+
| (기본)                | 새 값이 없으면 이전 Config 복사    |
|                       | if len(newVals)==0 &&              |
|                       |    len(current.Config)>0 {         |
|                       |    newVals = current.Config        |
|                       | }                                  |
+-----------------------+----------------------------------+
```

### 3.3 Upgrade 실패 시 흐름

소스 경로: `pkg/action/upgrade.go` -- `failRelease()`

```mermaid
sequenceDiagram
    participant Upgrade as Upgrade
    participant Storage as cfg.Releases
    participant KubeClient as cfg.KubeClient
    participant Rollback as Rollback.Run()

    Note over Upgrade: releasingUpgrade() 에서 에러 발생
    Upgrade->>Upgrade: failRelease(rel, created, err)
    Upgrade->>Upgrade: rel.Status = StatusFailed
    Upgrade->>Storage: recordRelease(rel)

    alt CleanupOnFail 설정 & created 리소스 있음
        Upgrade->>KubeClient: Delete(created, PropagationBackground)
        Note over KubeClient: 이번 업그레이드에서 새로<br/>생성된 리소스만 삭제
    end

    alt RollbackOnFailure 설정
        Upgrade->>Storage: History(rel.Name)
        Note over Storage: 전체 릴리스 이력 조회
        Upgrade->>Upgrade: superseded/deployed 상태만 필터
        Note over Upgrade: 가장 최근의 성공한 리비전 찾기

        Upgrade->>Rollback: NewRollback(cfg)
        Upgrade->>Rollback: rollin.Version = filteredHistory[0].Version
        Upgrade->>Rollback: rollin.Run(rel.Name)
        Note over Rollback: 이전 성공 리비전으로 자동 롤백
    end
```

## 4. helm rollback 흐름

소스 경로: `pkg/action/rollback.go`

### 4.1 전체 흐름 (Rollback.Run)

```mermaid
sequenceDiagram
    participant Action as Rollback.Run()
    participant Prepare as prepareRollback()
    participant Perform as performRollback()
    participant KubeClient as cfg.KubeClient
    participant Storage as cfg.Releases
    participant Hook as cfg.execHook()

    Action->>KubeClient: IsReachable()
    Action->>Action: cfg.Releases.MaxHistory = r.MaxHistory

    Action->>Prepare: prepareRollback(name)

    rect rgb(255, 245, 238)
        Note over Prepare: prepareRollback()
        Prepare->>Prepare: chartutil.ValidateReleaseName(name)

        Prepare->>Storage: Last(name)
        Storage-->>Prepare: currentRelease
        Note over Prepare: currentRelease.Version = 예) 5

        Prepare->>Prepare: previousVersion 결정
        Note over Prepare: r.Version > 0 이면 해당 버전<br/>r.Version == 0 이면 currentVersion - 1

        Prepare->>Storage: History(name)
        Storage-->>Prepare: historyReleases
        Note over Prepare: 지정된 버전이 이력에<br/>존재하는지 확인

        Prepare->>Storage: Get(name, previousVersion)
        Storage-->>Prepare: previousRelease
        Note over Prepare: 롤백 대상 릴리스 조회<br/>(예: Version 3의 차트, Config, Manifest)

        Prepare->>Prepare: getUpgradeServerSideValue(r.ServerSideApply, previousRelease.ApplyMethod)

        Note over Prepare: targetRelease 생성
        Note over Prepare: Version = currentRelease.Version + 1 (예: 6)
        Note over Prepare: Chart = previousRelease.Chart
        Note over Prepare: Config = previousRelease.Config
        Note over Prepare: Manifest = previousRelease.Manifest
        Note over Prepare: Hooks = previousRelease.Hooks
        Note over Prepare: Status = StatusPendingRollback
        Note over Prepare: Description = "Rollback to {version}"
    end

    Prepare-->>Action: currentRelease, targetRelease, serverSideApply

    alt DryRun이 아닌 경우
        Action->>Storage: Create(targetRelease)
        Note over Storage: pending-rollback 상태로 저장
    end

    Action->>Perform: performRollback(currentRelease, targetRelease, serverSideApply)

    rect rgb(240, 255, 240)
        Note over Perform: performRollback()

        alt DryRun 모드
            Perform-->>Action: targetRelease
        end

        Perform->>KubeClient: Build(currentRelease.Manifest)
        KubeClient-->>Perform: current (ResourceList)
        Perform->>KubeClient: Build(targetRelease.Manifest)
        KubeClient-->>Perform: target (ResourceList)

        Perform->>Hook: execHook(targetRelease, HookPreRollback, ...)
        Note over Hook: pre-rollback 훅 실행

        Perform->>Perform: setMetadataVisitor(name, namespace)
        Perform->>KubeClient: Update(current, target, SSA옵션)
        Note over KubeClient: 리소스를 이전 상태로 업데이트

        alt Update 실패
            Perform->>Perform: currentRelease.Status = StatusSuperseded
            Perform->>Perform: targetRelease.Status = StatusFailed
            Perform->>Storage: recordRelease(currentRelease)
            Perform->>Storage: recordRelease(targetRelease)
            alt CleanupOnFail
                Perform->>KubeClient: Delete(created)
            end
        end

        Perform->>KubeClient: GetWaiter(strategy)
        Perform->>KubeClient: waiter.Wait(target, timeout)

        Perform->>Hook: execHook(targetRelease, HookPostRollback, ...)
        Note over Hook: post-rollback 훅 실행

        Perform->>Storage: DeployedAll(currentRelease.Name)
        Note over Storage: 기존 deployed 릴리스 모두 조회
        loop 기존 deployed 릴리스마다
            Perform->>Perform: rel.Status = StatusSuperseded
            Perform->>Storage: recordRelease(rel)
        end

        Perform->>Perform: targetRelease.Status = StatusDeployed
    end

    alt DryRun이 아닌 경우
        Action->>Storage: Update(targetRelease)
        Note over Storage: deployed 상태로 갱신
    end
```

### 4.2 Rollback의 핵심 특성

롤백은 **새 리비전을 생성**한다. 이전 리비전의 차트, 설정, 매니페스트를 복사하여 새 리비전으로 만든다:

```
리비전 이력 예시:

v1 (deployed)  -> helm install
v2 (superseded)-> helm upgrade
v3 (failed)    -> helm upgrade (실패)
v4 (superseded)-> helm rollback 2 (v2의 내용을 복사하여 새 리비전 생성)
v5 (deployed)  -> helm upgrade (성공)
```

소스 코드에서 확인할 수 있듯이, `targetRelease.Version = currentRelease.Version + 1`이며, `targetRelease.Chart`, `Config`, `Manifest`, `Hooks`는 모두 `previousRelease`에서 복사된다:

```go
// pkg/action/rollback.go - prepareRollback() 중
targetRelease := &release.Release{
    Name:      name,
    Namespace: currentRelease.Namespace,
    Chart:     previousRelease.Chart,        // 이전 차트
    Config:    previousRelease.Config,       // 이전 설정
    Info: &release.Info{
        FirstDeployed: currentRelease.Info.FirstDeployed,
        LastDeployed:  time.Now(),
        Status:        common.StatusPendingRollback,
        Notes:         previousRelease.Info.Notes,
        Description:   fmt.Sprintf("Rollback to %d", previousVersion),
    },
    Version:     currentRelease.Version + 1,  // 새 리비전 번호
    Labels:      previousRelease.Labels,
    Manifest:    previousRelease.Manifest,    // 이전 매니페스트
    Hooks:       previousRelease.Hooks,       // 이전 훅
    ApplyMethod: string(determineReleaseSSApplyMethod(serverSideApply)),
}
```

## 5. helm uninstall 흐름

소스 경로: `pkg/action/uninstall.go`

### 5.1 전체 흐름 (Uninstall.Run)

```mermaid
sequenceDiagram
    participant Action as Uninstall.Run()
    participant KubeClient as cfg.KubeClient
    participant Storage as cfg.Releases
    participant Hook as cfg.execHook()

    Action->>KubeClient: IsReachable()
    Action->>KubeClient: GetWaiter(strategy)
    KubeClient-->>Action: waiter

    alt DryRun 모드
        Action->>Storage: releaseContent(name, 0)
        Storage-->>Action: release
        Action-->>Action: UninstallReleaseResponse{Release: rel}
        Note over Action: 여기서 반환
    end

    Action->>Action: chartutil.ValidateReleaseName(name)

    Action->>Storage: History(name)
    Storage-->>Action: rels (전체 이력)
    Note over Action: 리비전 정렬 후 최신 릴리스 선택

    alt 이미 uninstalled 상태
        alt KeepHistory = false
            Action->>Action: purgeReleases(rels...)
            Note over Action: Storage에서 모든 이력 삭제
        else KeepHistory = true
            Note over Action: 에러: "already deleted"
        end
    end

    Action->>Action: rel.Status = StatusUninstalling
    Action->>Action: rel.Info.Deleted = time.Now()
    Note over Action: res = UninstallReleaseResponse{Release: rel}

    rect rgb(255, 240, 240)
        Note over Action: pre-delete 훅 실행
        Action->>Hook: execHook(rel, HookPreDelete, ...)
    end

    Action->>Storage: Update(rel)
    Note over Storage: uninstalling 상태 저장

    Action->>Action: deleteRelease(rel)

    rect rgb(255, 248, 240)
        Note over Action: deleteRelease() 상세
        Action->>Action: releaseutil.SplitManifests(rel.Manifest)
        Action->>Action: releaseutil.SortManifests(..., UninstallOrder)
        Note over Action: 삭제 순서: InstallOrder의 역순

        Action->>Action: filterManifestsToKeep(files)
        Note over Action: helm.sh/resource-policy: keep<br/>어노테이션이 있는 리소스 제외

        Action->>KubeClient: Build(manifestsToDelete)
        Action->>KubeClient: Delete(resources, propagation)
    end

    Action->>KubeClient: waiter.WaitForDelete(deletedResources, timeout)
    Note over KubeClient: 리소스 삭제 완료 대기

    rect rgb(240, 255, 240)
        Note over Action: post-delete 훅 실행
        Action->>Hook: execHook(rel, HookPostDelete, ...)
    end

    Action->>Action: rel.Status = StatusUninstalled
    Action->>Action: rel.Info.Description = "Uninstallation complete"

    alt KeepHistory = false
        Action->>Action: purgeReleases(rels...)
        Note over Storage: Storage에서 모든 릴리스 이력 삭제
    else KeepHistory = true
        Action->>Storage: Update(rel)
        Note over Storage: uninstalled 상태로 보존

        Action->>Storage: DeployedAll(name)
        loop 기존 deployed 릴리스마다
            Action->>Action: rel.Status = StatusSuperseded
            Action->>Storage: Update(rel)
        end
    end
```

### 5.2 리소스 삭제 순서

`deleteRelease()` 함수에서 `releaseutil.SortManifests()`는 `UninstallOrder`를 사용한다. 이는 `InstallOrder`의 역순으로, 의존하는 리소스부터 먼저 삭제한다:

```
설치 순서 (InstallOrder):
  Namespace -> ServiceAccount -> Role -> RoleBinding -> Service -> Deployment -> ...

삭제 순서 (UninstallOrder):
  ... -> Deployment -> Service -> RoleBinding -> Role -> ServiceAccount -> Namespace
```

### 5.3 리소스 보존 정책

`filterManifestsToKeep()` 함수는 `helm.sh/resource-policy: keep` 어노테이션이 있는 리소스를 삭제 대상에서 제외한다:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-volume
  annotations:
    "helm.sh/resource-policy": keep
spec:
  # ...
```

### 5.4 Deletion Propagation

소스 경로: `pkg/action/uninstall.go` -- `parseCascadingFlag()`

```go
// pkg/action/uninstall.go
func parseCascadingFlag(cascadingFlag string) v1.DeletionPropagation {
    switch cascadingFlag {
    case "orphan":
        return v1.DeletePropagationOrphan       // 자식 리소스 유지
    case "foreground":
        return v1.DeletePropagationForeground    // 자식 먼저 삭제
    case "background":
        return v1.DeletePropagationBackground    // 비동기 삭제 (기본)
    default:
        return v1.DeletePropagationBackground
    }
}
```

## 6. Hook 실행 시퀀스

소스 경로: `pkg/action/hooks.go`

### 6.1 execHook() 전체 흐름

```mermaid
sequenceDiagram
    participant Caller as Action (Install/Upgrade/...)
    participant Hook as cfg.execHook()
    participant ExecH as execHookWithDelayedShutdown()
    participant KubeClient as cfg.KubeClient
    participant Storage as cfg.recordRelease()

    Caller->>Hook: execHook(rel, hookEvent, waitStrategy, waitOptions, timeout, ssa)
    Hook->>ExecH: execHookWithDelayedShutdown(...)

    ExecH->>ExecH: 해당 이벤트의 훅 필터링
    Note over ExecH: rl.Hooks에서 hookEvent와<br/>일치하는 훅만 선택

    ExecH->>ExecH: sort.Stable(hookByWeight(executingHooks))
    Note over ExecH: Weight 기준 오름차순 정렬<br/>(같은 Weight이면 Name 순)

    loop 각 훅(h)에 대해
        ExecH->>ExecH: hookSetDeletePolicy(h)
        Note over ExecH: 삭제 정책 미지정 시<br/>기본값: before-hook-creation

        ExecH->>ExecH: deleteHookByPolicy(h, HookBeforeHookCreation, ...)
        Note over ExecH: 이전에 생성된 동일 훅 삭제<br/>(재실행 보장)

        alt 이전 훅 존재 & before-hook-creation 정책
            ExecH->>KubeClient: Build(h.Manifest)
            ExecH->>KubeClient: Delete(resources)
            ExecH->>KubeClient: waiter.WaitForDelete(resources, timeout)
        end

        ExecH->>KubeClient: Build(h.Manifest, validate=true)
        KubeClient-->>ExecH: resources

        ExecH->>ExecH: h.LastRun = {StartedAt: now, Phase: Running}
        ExecH->>Storage: recordRelease(rl)
        Note over Storage: 실행 시작 기록

        ExecH->>KubeClient: Create(resources, SSA옵션)
        Note over KubeClient: 훅 리소스를 클러스터에 생성

        alt Create 실패
            ExecH->>ExecH: h.LastRun.Phase = HookPhaseFailed
            ExecH-->>Hook: error
        end

        ExecH->>KubeClient: GetWaiter(waitStrategy)
        ExecH->>KubeClient: waiter.WatchUntilReady(resources, timeout)
        Note over KubeClient: Job 완료 등 리소스가<br/>Ready 상태가 될 때까지 대기

        ExecH->>ExecH: h.LastRun.CompletedAt = now

        alt WatchUntilReady 실패
            ExecH->>ExecH: h.LastRun.Phase = HookPhaseFailed
            ExecH->>ExecH: outputLogsByPolicy(h, namespace, HookOutputOnFailed)
            Note over ExecH: 실패 시 로그 수집 (정책에 따라)

            Note over ExecH: shutdown 함수 반환:
            Note over ExecH: 1. 실패한 훅: deleteHookByPolicy(HookFailed)
            Note over ExecH: 2. 이전 성공 훅들: deleteHookByPolicy(HookSucceeded)
            ExecH-->>Hook: shutdown, error
        else WatchUntilReady 성공
            ExecH->>ExecH: h.LastRun.Phase = HookPhaseSucceeded
        end
    end

    Note over ExecH: 모든 훅 성공 시 shutdown 함수 반환:
    Note over ExecH: 역순으로 각 훅에 대해:
    Note over ExecH: 1. outputLogsByPolicy(HookOutputOnSucceeded)
    Note over ExecH: 2. deleteHookByPolicy(HookSucceeded)
    ExecH-->>Hook: shutdown, nil

    Hook->>Hook: shutdown()
    Note over Hook: 삭제 정책에 따라 훅 리소스 정리
```

### 6.2 Hook Weight 정렬

소스 경로: `pkg/action/hooks.go`

```go
// pkg/action/hooks.go
type hookByWeight []*release.Hook

func (x hookByWeight) Less(i, j int) bool {
    if x[i].Weight == x[j].Weight {
        return x[i].Name < x[j].Name   // 같은 weight면 이름순
    }
    return x[i].Weight < x[j].Weight    // 낮은 weight 먼저
}
```

`sort.Stable`을 사용하므로 동일 weight의 훅은 원래 순서(리소스 종류 순서)가 유지된다.

### 6.3 삭제 정책 처리

```
+----------------------------+--------------------------------------------------+
| 정책                        | 동작                                              |
+----------------------------+--------------------------------------------------+
| before-hook-creation (기본) | 훅 실행 전에 이전 훅 리소스 삭제                    |
|                            | -> 매번 깨끗한 상태에서 실행                        |
+----------------------------+--------------------------------------------------+
| hook-succeeded             | 훅 성공 후 리소스 삭제                              |
|                            | -> 성공한 Job 리소스 정리                           |
+----------------------------+--------------------------------------------------+
| hook-failed                | 훅 실패 후 리소스 삭제                              |
|                            | -> 실패한 리소스 정리                               |
+----------------------------+--------------------------------------------------+
```

### 6.4 CRD는 삭제하지 않음

```go
// pkg/action/hooks.go - deleteHookByPolicy() 중
func (cfg *Configuration) deleteHookByPolicy(h *release.Hook, policy release.HookDeletePolicy, ...) error {
    if h.Kind == "CustomResourceDefinition" {
        return nil  // CRD는 절대 삭제하지 않음
    }
    // ...
}
```

CRD를 삭제하면 해당 CRD로 생성된 모든 커스텀 리소스가 가비지 컬렉션되므로, 훅 삭제 정책과 무관하게 CRD는 보호된다.

### 6.5 Hook 로그 출력

소스 경로: `pkg/action/hooks.go` -- `outputLogsByPolicy()`

```go
// pkg/action/hooks.go
func (cfg *Configuration) outputLogsByPolicy(h *release.Hook, releaseNamespace string,
    policy release.HookOutputLogPolicy) error {

    if !hookHasOutputLogPolicy(h, policy) {
        return nil
    }
    namespace, err := cfg.deriveNamespace(h, releaseNamespace)
    // ...
    switch h.Kind {
    case "Job":
        return cfg.outputContainerLogsForListOptions(namespace,
            metav1.ListOptions{LabelSelector: fmt.Sprintf("job-name=%s", h.Name)})
    case "Pod":
        return cfg.outputContainerLogsForListOptions(namespace,
            metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", h.Name)})
    default:
        return nil  // Job, Pod 외의 리소스는 로그 미출력
    }
}
```

`hook-output-log-policy` 어노테이션이 설정된 Job이나 Pod의 경우, 훅 성공/실패 시 컨테이너 로그를 자동으로 수집하여 출력한다. 이 로그는 `Configuration.HookOutputFunc`을 통해 지정된 Writer로 전달된다.

## 7. 공통 패턴 요약

### 7.1 각 Action의 훅 이벤트 쌍

| Action | Pre 훅 | Post 훅 |
|--------|--------|---------|
| Install | `pre-install` | `post-install` |
| Upgrade | `pre-upgrade` | `post-upgrade` |
| Rollback | `pre-rollback` | `post-rollback` |
| Uninstall | `pre-delete` | `post-delete` |
| Test | - | `test` |

### 7.2 상태 전이 패턴

모든 Action이 따르는 공통 상태 전이 패턴:

```
1. 릴리스 생성/조회
2. Status = Pending (pending-install / pending-upgrade / pending-rollback)
3. Storage에 저장 (낙관적 잠금 역할)
4. Pre 훅 실행
5. 리소스 생성/업데이트/삭제
6. 리소스 준비 대기 (Wait)
7. Post 훅 실행
8. Status = Deployed (성공) / Failed (실패)
9. Storage 갱신
```

### 7.3 Context 취소 처리

Install과 Upgrade는 `context.Context`를 사용하여 SIGTERM/SIGINT 처리를 지원한다:

```go
// pkg/cmd/install.go - runInstall() 중
ctx := context.Background()
ctx, cancel := context.WithCancel(ctx)

cSignal := make(chan os.Signal, 2)
signal.Notify(cSignal, os.Interrupt, syscall.SIGTERM)
go func() {
    <-cSignal
    fmt.Fprintf(out, "Release %s has been cancelled.\n", args[0])
    cancel()
}()

ri, err := client.RunWithContext(ctx, chartRequested, vals)
```

Install의 `performInstallCtx()`는 goroutine에서 실행되며, context 취소 시 goroutine은 백그라운드에서 계속 실행된다:

```go
// pkg/action/install.go - performInstallCtx() 중
go func() {
    rel, err := i.performInstall(rel, toBeAdopted, resources)
    resultChan <- Msg{rel, err}
}()
select {
case <-ctx.Done():
    return rel, ctx.Err()       // context 취소 -- 즉시 반환
case msg := <-resultChan:
    return msg.r, msg.e          // 정상 완료
}
```

### 7.4 Server-Side Apply 결정 로직

```go
// pkg/action/upgrade.go
func getUpgradeServerSideValue(serverSideOption string, releaseApplyMethod string) (bool, error) {
    switch serverSideOption {
    case "auto":
        return releaseApplyMethod == "ssa", nil  // 이전 릴리스 방식 유지
    case "false":
        return false, nil                         // CSA 강제
    case "true":
        return true, nil                          // SSA 강제
    default:
        return false, fmt.Errorf("invalid/unknown release server-side apply method: %s", serverSideOption)
    }
}
```

| 현재 `--server-side` | 이전 릴리스 ApplyMethod | 결과 |
|---|---|---|
| `auto` (기본) | `"ssa"` | SSA 사용 |
| `auto` (기본) | `"csa"` 또는 `""` | CSA 사용 |
| `true` | (무관) | SSA 강제 |
| `false` | (무관) | CSA 강제 |

Install의 경우 `ServerSideApply`가 `bool` 타입으로 기본값이 `true`이며, Upgrade와 Rollback은 `string` 타입으로 기본값이 `"auto"`이다. 이는 Install은 항상 새로운 릴리스이므로 이전 방식을 참조할 필요가 없고, Upgrade/Rollback은 기존 릴리스와의 호환성을 유지해야 하기 때문이다.
