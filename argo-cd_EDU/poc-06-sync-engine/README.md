# PoC 06 — Sync Engine 상태 머신

## 개요

Argo CD의 sync 엔진 상태 머신을 시뮬레이션한다.
실제 소스: `gitops-engine/pkg/sync/sync_context.go`

## 핵심 개념

### Sync() 반복 호출 패턴

```
gitops-engine/pkg/sync/sync_context.go  syncContext.Sync()
```

Argo CD의 sync는 단일 함수 호출이 아니라 **반복 호출**로 동작한다:

```
for {
    done := sc.Sync()  // 한 번에 한 phase/wave 처리
    if done { break }
    // 다음 반복 전 상태 저장, 헬스체크 대기
}
```

### Sync Phase 순서

```
gitops-engine/pkg/sync/sync_phase.go
```

```
PreSync (wave 0, 1, 2...) → Sync (wave 0, 1, 2...) → PostSync (wave 0, 1, 2...)
                                      ↓ 실패 시
                              SyncFail (롤백/알림 훅)
```

| Phase | 용도 | 예시 |
|-------|------|------|
| PreSync | sync 전 준비 | DB 마이그레이션 Job |
| Sync | 실제 리소스 적용 | Deployment, Service |
| PostSync | sync 후 검증 | 스모크 테스트 Job |
| SyncFail | 실패 시 처리 | 롤백 스크립트, 알림 |

### Sync Wave

각 phase 내에서 wave 번호 순서로 실행된다. 낮은 wave가 먼저 완료되어야 다음 wave가 시작된다.

```yaml
# argocd.argoproj.io/sync-wave: "N" 어노테이션으로 지정
metadata:
  annotations:
    argocd.argoproj.io/sync-wave: "2"  # Sync phase의 wave 2에서 실행
```

### Prune Wave 역순 처리

```
gitops-engine/pkg/sync/sync_context.go:1031-1062
```

리소스 삭제는 생성 순서의 **역순**으로 수행한다:

```
생성 순서:  wave 0 (Namespace) → wave 1 (StatefulSet) → wave 2 (Deployment)
삭제 순서:  wave 2 (Deployment) → wave 1 (StatefulSet) → wave 0 (Namespace)
```

실제 구현: symmetric swap으로 wave를 재배정
```go
endWave := maxWave - (t.Wave - minWave)
t.WaveOverride = &endWave
```

### PruneLast

```
gitops-engine/pkg/sync/sync_context.go:145-148 WithPruneLast()
gitops-engine/pkg/sync/sync_context.go:1061-1062 pruneLast 처리
```

`argocd.argoproj.io/sync-wave-prune-last: "true"` 어노테이션이 있는 리소스는 모든 sync 완료 후 마지막에 삭제된다:

```
overrideWave = lastSyncPhaseWave + 1
```

### Hook generateName

```
gitops-engine/pkg/sync/sync_context.go:923-935
```

`metadata.generateName`이 설정된 hook은 매 sync마다 고유한 이름이 자동 생성된다:

```go
postfix := strings.ToLower(fmt.Sprintf("%s-%s-%d",
    syncRevision,   // revision[0:7]
    phase,          // presync/sync/postsync
    startedAt.Unix() // Unix timestamp
))
targetObj.SetName(generateName + postfix)
```

**이유**: 동일한 hook을 매번 새로운 Job으로 실행하여 멱등성을 보장한다.

### Hook Delete Policy

```
gitops-engine/pkg/sync/sync_context.go:560-566
```

| 정책 | 동작 |
|------|------|
| `HookSucceeded` | 성공 시 삭제 |
| `HookFailed` | 실패 시 삭제 |
| `BeforeHookCreation` | 다음 sync 시작 전 삭제 |

### SyncFail Phase

```
gitops-engine/pkg/sync/sync_context.go:845-880 executeSyncFailPhase()
gitops-engine/pkg/sync/sync_context.go:569 syncFailTasks 분리
```

Sync phase에서 태스크 실패 시 SyncFail phase의 hook이 실행된다:
- 롤백 스크립트
- 알림 발송
- 임시 리소스 정리

## 실행

```bash
go run main.go
```

## 시나리오

| 시나리오 | 내용 |
|----------|------|
| 1 | 기본 Sync: PreSync → Sync → PostSync |
| 2 | Sync Wave: wave 0 → 1 → 2 순차 실행 |
| 3 | Prune Wave 역순: 높은 wave부터 삭제 |
| 4 | PruneLast: 모든 sync 후 마지막 삭제 |
| 5 | generateName Hook 이름 자동 생성 |
| 6 | Sync 실패 → SyncFail Phase 실행 |

## 실제 코드와의 대응

| 시뮬레이션 | 실제 소스 |
|-----------|-----------|
| `SyncPhase` 상수 | `gitops-engine/pkg/sync/common/types.go` |
| `Sync()` 반복 호출 | `gitops-engine/pkg/sync/sync_context.go:Sync()` |
| phase/wave 결정 | `gitops-engine/pkg/sync/sync_context.go:598-603` |
| SyncFail 분리 | `gitops-engine/pkg/sync/sync_context.go:569` |
| prune wave 역순 | `gitops-engine/pkg/sync/sync_context.go:1031-1062` |
| generateName | `gitops-engine/pkg/sync/sync_context.go:923-935` |
| hook delete policy | `gitops-engine/pkg/sync/sync_context.go:560-566` |
