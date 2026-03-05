# PoC 07 — 헬스 평가 엔진

## 개요

Argo CD의 헬스 평가 엔진을 시뮬레이션한다.
실제 소스: `gitops-engine/pkg/health/health.go`

## 핵심 개념

### HealthStatusCode

```
gitops-engine/pkg/health/health.go:14-31
```

| 상태 | 설명 |
|------|------|
| `Healthy` | 100% 정상 |
| `Progressing` | 아직 Healthy 아니지만 도달 가능한 진행 중 |
| `Suspended` | 일시 중단 (CronJob suspend 등) |
| `Missing` | 클러스터에 리소스 없음 |
| `Degraded` | 실패 또는 시간 내 Healthy 불가 |
| `Unknown` | 헬스 평가 자체 실패 |

### healthOrder — 상태 우선순위

```
gitops-engine/pkg/health/health.go:44-52
```

```go
var healthOrder = []HealthStatusCode{
    HealthStatusHealthy,
    HealthStatusSuspended,
    HealthStatusProgressing,
    HealthStatusMissing,
    HealthStatusDegraded,
    HealthStatusUnknown,
}
```

앱 레벨 헬스는 모든 리소스 중 `healthOrder`에서 가장 뒤에 있는(가장 나쁜) 상태가 된다.

### IsWorse()

```
gitops-engine/pkg/health/health.go:54-67
```

```go
func IsWorse(current, new HealthStatusCode) bool {
    // healthOrder에서 new의 인덱스 > current의 인덱스면 더 나쁨
}
```

### GetResourceHealth() 우선순위

```
gitops-engine/pkg/health/health.go:70-101
```

```
1. DeletionTimestamp != nil → Progressing ("Pending deletion")
2. healthOverride (Lua 스크립트) → nil 아니면 반환, nil이면 다음으로
3. GetHealthCheckFunc(gvk) → 내장 GVK별 체크 함수
4. nil → 헬스 개념 없음 (ConfigMap, Secret 등)
```

### GVK별 내장 헬스 체크

```
gitops-engine/pkg/health/health.go:103-152  GetHealthCheckFunc()
```

| GVK | 파일 | 핵심 로직 |
|-----|------|-----------|
| apps/Deployment | `health_deployment.go` | Paused→Suspended, replicas/conditions 확인 |
| apps/StatefulSet | `health_statefulset.go` | readyReplicas, revision 확인 |
| apps/ReplicaSet | `health_replicaset.go` | readyReplicas 확인 |
| apps/DaemonSet | `health_daemonset.go` | numberReady 확인 |
| /Pod | `health_pod.go` | phase + restartPolicy + 컨테이너 상태 |
| /PersistentVolumeClaim | `health_pvc.go` | phase (Bound/Pending/Lost) |
| /Service | `health_service.go` | 기본 Healthy |
| batch/Job | `health_job.go` | conditions (Complete/Failed/Suspended) |
| autoscaling/HPA | `health_hpa.go` | conditions 확인 |
| networking.k8s.io/Ingress | `health_ingress.go` | LoadBalancer ingress |

### Deployment 헬스 로직

```
gitops-engine/pkg/health/health_deployment.go:28-70
```

```
spec.paused=true          → Suspended
generation > observedGen  → Progressing (spec 변경 미관찰)
ProgressDeadlineExceeded  → Degraded
updatedReplicas < replicas → Progressing (롤아웃 중)
availableReplicas < updatedReplicas → Progressing (가용 대기)
그 외                     → Healthy
```

### Pod 헬스 로직

```
gitops-engine/pkg/health/health_pod.go:30-134
```

RestartPolicy=Always일 때 컨테이너 오류 먼저 확인:
- `ErrImagePull`, `ImagePullBackOff`, `CrashLoopBackOff` 등 → Degraded

Phase 기반:
- `Pending` → Progressing
- `Succeeded` → Healthy
- `Failed` → Degraded
- `Running` + RestartPolicy=Always + Ready → Healthy
- `Running` + RestartPolicy=OnFailure/Never → Progressing (hook pod)

### 앱 레벨 헬스 집계

```
controller/health.go:20-104  setApplicationHealth()
```

```go
// Hook 리소스는 집계 제외
if hookutil.IsHook(res.Live) { continue }

// Missing 리소스는 집계 제외 (OutOfSync로 이미 표시됨)
if res.Live == nil && healthStatus.Status == HealthStatusMissing { continue }

// 가장 나쁜 상태 추적
if health.IsWorse(appHealthStatus, healthStatus.Status) {
    appHealthStatus = healthStatus.Status
}
```

## 실행

```bash
go run main.go
```

## 시나리오

| 시나리오 | 내용 |
|----------|------|
| 1 | DeletionTimestamp → 무조건 Progressing |
| 2 | Deployment 6가지 상태 (Healthy/Suspended/Progressing/Degraded) |
| 3 | Pod: phase + restartPolicy + 컨테이너 에러 |
| 4 | PVC, Job, StatefulSet 헬스 |
| 5 | ConfigMap 등 헬스 개념 없는 리소스 |
| 6 | Lua healthOverride (CRD 커스텀 헬스) |
| 7 | 앱 레벨 헬스 집계 (IsWorse + Hook 제외) |

## 실제 코드와의 대응

| 시뮬레이션 | 실제 소스 |
|-----------|-----------|
| `HealthStatusCode` 상수 | `gitops-engine/pkg/health/health.go:14` |
| `healthOrder` 배열 | `gitops-engine/pkg/health/health.go:44` |
| `IsWorse()` | `gitops-engine/pkg/health/health.go:54` |
| `GetResourceHealth()` | `gitops-engine/pkg/health/health.go:70` |
| `getDeploymentHealth()` | `gitops-engine/pkg/health/health_deployment.go:28` |
| `getPodHealth()` | `gitops-engine/pkg/health/health_pod.go:30` |
| `getPVCHealth()` | `gitops-engine/pkg/health/health_pvc.go:28` |
| `getJobHealth()` | `gitops-engine/pkg/health/health_job.go:30` |
| `getStatefulSetHealth()` | `gitops-engine/pkg/health/health_statefulset.go:28` |
| `setApplicationHealth()` | `controller/health.go:20` |
