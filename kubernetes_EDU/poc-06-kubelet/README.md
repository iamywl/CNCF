# PoC-06: Kubelet Sync Loop 시뮬레이션

## 개요

Kubelet의 핵심 이벤트 루프인 **syncLoop**를 구현한다.
syncLoop는 `select` 문으로 4개의 채널을 동시에 감시하며, 파드의 전체 생명주기를 관리한다.

## 구현 내용

| 컴포넌트 | 역할 | 실제 소스 위치 |
|---------|------|-------------|
| syncLoop | 메인 이벤트 루프 (4채널 select) | `pkg/kubelet/kubelet.go:2505` |
| syncLoopIteration | 한 번의 루프 반복 | `pkg/kubelet/kubelet.go:2580` |
| PLEG | 컨테이너 상태 변경 감지 | `pkg/kubelet/pleg/generic.go` |
| PodWorker | 파드별 상태 머신 (Sync/Terminating/Terminated) | `pkg/kubelet/pod_workers.go` |
| FakeCRI | 컨테이너 런타임 인터페이스 | `cri-api/pkg/apis/runtime/v1/` |
| StatusManager | API Server에 상태 보고 | `pkg/kubelet/status/status_manager.go` |

## syncLoop 채널 구조

```
┌─ configCh        (API Server → 파드 스펙 변경 ADD/UPDATE/DELETE)
├─ plegCh          (PLEG → ContainerStarted/ContainerDied 이벤트)
├─ syncCh          (타이머 → 주기적 전체 파드 동기화 확인)
└─ housekeepingCh  (타이머 → GC, 로그 정리, 고아 컨테이너 정리)
```

## PodWorker 상태 머신

```
Idle → Syncing → Running → Terminating → Terminated
```

## 시뮬레이션 시나리오

1. 파드 추가 (2개 컨테이너: nginx + sidecar)
2. 두 번째 파드 추가 (redis)
3. PLEG가 컨테이너 시작 감지 → StatusManager에 보고
4. 파드 삭제 → PodWorker 종료 → CRI 컨테이너 정지
5. 컨테이너 비정상 종료 시뮬레이션 → PLEG 감지 → ContainerDied 이벤트

## 실행

```bash
go run main.go
```
