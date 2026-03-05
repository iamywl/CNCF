# PoC 20: Workload Controllers 시뮬레이션

## 개요
Kubernetes의 4대 워크로드 컨트롤러(StatefulSet, DaemonSet, Job, CronJob)의 핵심 알고리즘을 시뮬레이션합니다.

## 다루는 개념
- **StatefulSet**: 순서 보장(ordinal), PVC 관리, Rolling Update (역순)
- **DaemonSet**: 노드당 하나 보장, Toleration 체크, Slow Start Batch
- **Job**: Completions/Parallelism, BackoffLimit, 실패 처리
- **CronJob**: ConcurrencyPolicy(Allow/Forbid/Replace), 히스토리 관리

## 실행
```bash
go run main.go
```

## 참조 소스코드
| 컨트롤러 | 파일 |
|---------|------|
| StatefulSet | `pkg/controller/statefulset/stateful_set_control.go` |
| DaemonSet | `pkg/controller/daemon/daemon_controller.go` |
| Job | `pkg/controller/job/job_controller.go` |
| CronJob | `pkg/controller/cronjob/cronjob_controllerv2.go` |
