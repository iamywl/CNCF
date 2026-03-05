# PoC-08: CRI RunPodSandbox / CreateContainer

## 목적

containerd의 CRI(Container Runtime Interface) 구현을 시뮬레이션한다. Kubernetes의 kubelet이 CRI를 통해 Pod Sandbox를 생성하고, 그 안에 컨테이너를 생성/시작/중지/제거하는 전체 흐름을 재현한다.

## 핵심 개념

### 1. CRI와 containerd의 관계

containerd는 CRI를 내장 플러그인으로 구현한다. kubelet은 gRPC로 containerd의 CRI 서비스에 요청을 보내고, containerd는 이를 내부 sandbox/container 관리 로직으로 변환한다.

```
kubelet ──gRPC──> containerd (CRI Plugin) ──> shim ──> runc
```

### 2. RunPodSandbox 흐름

RunPodSandbox는 Pod 수준의 격리 환경을 생성한다. 이 과정이 CRI에서 가장 복잡한 API 호출이다.

```
RunPodSandbox:
  1. GenerateID()         - 고유 Sandbox ID 생성
  2. Reserve(name, id)    - 이름 예약 (동시 요청 방지)
  3. Lease.Create(id)     - 리소스 보호용 Lease 생성
  4. NewNetNS()           - 네트워크 네임스페이스 생성
  5. setupPodNetwork()    - CNI 플러그인으로 네트워크 설정 (IP 할당)
  6. CreateSandbox()      - Sandbox 메타데이터 저장
  7. StartSandbox()       - pause 컨테이너 시작
  8. State = Ready        - Sandbox 상태 업데이트
  9. ExitMonitor          - Sandbox 종료 감시 시작
```

### 3. CreateContainer 흐름

CreateContainer는 기존 Pod Sandbox 안에 새 컨테이너를 생성한다.

```
CreateContainer:
  1. sandboxStore.Get()       - Sandbox 조회 및 상태 확인
  2. GenerateID()             - 고유 Container ID 생성
  3. LocalResolve(image)      - 이미지 존재 확인
  4. buildContainerSpec()     - OCI Runtime Spec 생성
  5. WithNewSnapshot()        - 이미지 레이어 기반 스냅샷 준비
  6. client.NewContainer()    - containerd에 컨테이너 메타데이터 저장
  7. containerStore.Add()     - CRI 내부 스토어에 추가
```

### 4. StartContainer / StopContainer / RemoveContainer

```
StartContainer:
  1. containerStore.Get()  - 컨테이너 조회 (CREATED 상태 확인)
  2. Task.Create()         - shim에 Task 생성 요청
  3. Task.Start()          - init 프로세스 시작
  4. State = Running       - 상태 업데이트

StopContainer:
  1. Task.Kill(SIGTERM)    - 그레이스풀 종료 시도
  2. Wait(timeout)         - 타임아웃 대기
  3. Task.Kill(SIGKILL)    - 강제 종료 (타임아웃 시)
  4. State = Stopped       - 상태 업데이트

RemoveContainer:
  1. Task.Delete()         - shim에서 Task 삭제
  2. Snapshot.Remove()     - 스냅샷 삭제
  3. containerStore.Delete() - 메타데이터 삭제
  4. nameIndex.Release()   - 이름 해제
```

### 5. Pod와 Container의 관계

하나의 Pod Sandbox 안에 여러 Container가 존재한다. Sandbox가 네트워크 네임스페이스와 공유 리소스를 제공하고, 각 Container는 동일한 네트워크 공간에서 실행된다.

```
┌─────────────────────────────────────┐
│ Pod Sandbox (NetNS, Cgroup)         │
│                                     │
│ ┌────────┐ ┌────────┐ ┌──────────┐ │
│ │ pause  │ │  app   │ │ sidecar  │ │
│ │(infra) │ │        │ │          │ │
│ └────────┘ └────────┘ └──────────┘ │
│                                     │
│ Network: 10.244.x.x                │
└─────────────────────────────────────┘
```

### 6. 리소스 정리 순서

Pod 삭제 시 역순으로 정리한다:

```
StopContainer (각 컨테이너)
  → RemoveContainer (각 컨테이너)
  → StopPodSandbox (네트워크 해제, sandbox 중지)
  → RemovePodSandbox (lease 삭제, 메타데이터 정리)
```

## 소스 참조

| 파일 | 설명 |
|------|------|
| `internal/cri/server/sandbox_run.go` | RunPodSandbox (ID 생성, Lease, 네트워크, Sandbox 생성/시작) |
| `internal/cri/server/container_create.go` | CreateContainer (Spec 생성, Snapshot, Container 저장) |
| `internal/cri/server/container_start.go` | StartContainer (Task Create/Start) |
| `internal/cri/server/container_stop.go` | StopContainer (SIGTERM/SIGKILL) |
| `internal/cri/server/container_remove.go` | RemoveContainer (Task Delete, Snapshot Remove) |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
containerd CRI 시뮬레이션
(RunPodSandbox / CreateContainer)
========================================

--- 시나리오 1: RunPodSandbox ---
    [1] ID 생성: id=..., name=nginx-pod_default_uid-abc-123_0
    [2] 이름 예약
    [3] Lease 생성
    [4] 네트워크 설정: netns=/var/run/netns/..., ip=10.244.x.x
    [5] Sandbox 생성
    [6] Sandbox 시작: pid=...
    [7] 상태: SANDBOX_READY

--- 시나리오 2: CreateContainer ---
    nginx, istio-proxy 두 컨테이너 생성

--- 시나리오 3: StartContainer ---
    각 컨테이너 Task 생성 및 시작

--- 시나리오 4: StopContainer ---
    nginx 컨테이너 graceful 중지

--- 시나리오 5: RemoveContainer ---
    nginx 컨테이너 스냅샷/메타데이터 삭제

--- 시나리오 6: StopPodSandbox / RemovePodSandbox ---
    네트워크 해제, Lease 삭제, Sandbox 완전 제거

--- CRI 호출 흐름 ---
    각 API별 kubelet → containerd → shim/runc 흐름 표시

--- Pod와 Container 관계 ---
    ASCII 아트로 Pod Sandbox 구조 시각화
```
