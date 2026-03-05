# PoC 19: Pod Lifecycle 시뮬레이션

## 개요
Kubernetes Pod Lifecycle의 핵심 메커니즘을 시뮬레이션합니다.

## 다루는 개념
- **QoS 클래스 결정**: Guaranteed / Burstable / BestEffort 분류 알고리즘
- **OOM Score 조정**: QoS 기반 OOM Kill 우선순위 계산
- **Eviction Manager**: 리소스 압박 시 Pod 축출 로직
- **Preemption**: 우선순위 기반 선점 (피해자 선택 알고리즘)
- **Priority Sort**: 스케줄링 큐 정렬
- **Graceful Shutdown**: 종료 순서 (Main → Sidecar → SIGKILL)

## 실행
```bash
go run main.go
```

## 참조 소스코드
| 기능 | 파일 |
|------|------|
| QoS 결정 | `pkg/apis/core/v1/helper/qos/qos.go` |
| OOM Score | `pkg/kubelet/qos/policy.go` |
| Eviction | `pkg/kubelet/eviction/eviction_manager.go` |
| Preemption | `pkg/scheduler/framework/plugins/defaultpreemption/default_preemption.go` |
| Priority | `pkg/apis/scheduling/types.go` |
