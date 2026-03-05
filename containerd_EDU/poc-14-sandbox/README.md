# PoC-14: containerd Pod 샌드박스 컨트롤러 시뮬레이션

## 목적

containerd v2에서 Kubernetes Pod의 격리 환경을 관리하는 Sandbox Controller의
동작 방식을 시뮬레이션한다. CRI의 RunPodSandbox/StopPodSandbox/RemovePodSandbox가
내부적으로 Sandbox Controller의 Create/Start/Stop/Shutdown으로 매핑되는 과정을 이해한다.

## 핵심 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| Sandbox 메타데이터 | `core/sandbox/store.go` | ID, Runtime, Spec, Sandboxer, Labels |
| Controller | `core/sandbox/controller.go` | Create, Start, Stop, Wait, Status, Shutdown |
| ControllerInstance | `core/sandbox/controller.go` | SandboxID, Pid, Address |
| Store | `core/sandbox/store.go` | Get, List, Create, Update, Delete |
| CreateOpt/StopOpt | `core/sandbox/controller.go` | WithNetNSPath, WithTimeout 등 |
| CRI 매핑 | `plugins/services/sandbox/` | RunPodSandbox → Create+Start 등 |

## 소스 참조

| 파일 | 핵심 내용 |
|------|----------|
| `core/sandbox/controller.go` | Controller 인터페이스 (Create, Start, Stop, Wait, Status, Shutdown, Metrics, Update) |
| `core/sandbox/store.go` | Sandbox 구조체, Store 인터페이스, AddLabel/GetLabel 헬퍼 |
| `core/sandbox/bridge.go` | gRPC/tTRPC 브릿지 (프로토콜 변환) |
| `core/sandbox/helpers.go` | CreateOptions, StopOptions, WithNetNSPath, WithTimeout |
| `core/metadata/sandbox.go` | BoltDB 기반 메타데이터 저장 |
| `core/metadata/buckets.go` | BoltDB 경로: v1/<namespace>/sandboxes/<id> |

## 시뮬레이션 흐름

```
1. CRI RunPodSandbox 시뮬레이션
   a) Controller.Create: 메타데이터 저장, shim 초기화
   b) Controller.Start: shim 프로세스 시작 → PID, Address 반환
2. Pod 내 컨테이너 실행 (개념 설명)
3. Controller.Status: 상세 상태 조회
4. Store.List: 전체/필터 조회
5. Controller.Wait: 비동기 종료 대기 (goroutine)
6. CRI StopPodSandbox → Controller.Stop
7. CRI RemovePodSandbox → Controller.Shutdown
8. 정리 확인
```

## 실행 방법

```bash
cd containerd_EDU/poc-14-sandbox
go run main.go
```

## 예상 출력

```
=== containerd Pod 샌드박스 컨트롤러 시뮬레이션 ===

[1] CRI RunPodSandbox 시뮬레이션
------------------------------------------------------------
  [Create]
    [Controller] 샌드박스 생성: ID=sb-pod-nginx-abc123, Runtime=io.containerd.runc.v2, Sandboxer=podsandbox
    [Controller]   NetNS: /var/run/netns/cni-abc123
  [Start]
    [Controller] 샌드박스 시작: ID=sb-pod-nginx-abc123, PID=xxxxx, Address=/run/containerd/s/sb-pod-n.sock
    반환값: SandboxID=sb-pod-nginx-abc123, PID=xxxxx, Address=/run/containerd/s/sb-pod-n.sock

[3] Sandbox Status 조회
  SandboxID: sb-pod-nginx-abc123
  PID:       xxxxx
  State:     running
  ...

[5] Wait 시뮬레이션 (비동기 종료 대기)
[6] CRI StopPodSandbox 시뮬레이션
    [Controller] 샌드박스 정지 완료: ID=sb-pod-nginx-abc123, ExitCode=0
    [Wait] 샌드박스 종료 감지: ExitCode=0

[7] CRI RemovePodSandbox 시뮬레이션
    [Controller] 샌드박스 정리 완료: ID=sb-pod-nginx-abc123
  남은 샌드박스 수: 1

[CRI ↔ Sandbox Controller 매핑]
  CRI API              →  Sandbox Controller
  ─────────────────────────────────────────────
  RunPodSandbox        →  Create + Start
  StopPodSandbox       →  Stop
  RemovePodSandbox     →  Shutdown
  PodSandboxStatus     →  Status
  ListPodSandbox       →  Store.List
```

## 핵심 포인트

- **2단계 시작**: RunPodSandbox는 Create(초기화) + Start(실행)로 분리된다. Create에서 메타데이터 저장과 네임스페이스 설정을 하고, Start에서 shim 프로세스를 실제로 실행한다.
- **Wait 패턴**: Wait()는 블로킹 호출로, 샌드박스가 종료될 때까지 대기한다. 별도 goroutine에서 호출하여 비동기 종료 감지에 사용된다.
- **메타데이터 분리**: Sandbox 메타데이터(Store)와 런타임 상태(Controller)가 분리되어 있다. Store는 BoltDB에 영구 저장되고, Controller는 shim 프로세스의 실시간 상태를 관리한다.
- **CRI 매핑**: Kubernetes CRI의 Pod 생명주기 API가 containerd Sandbox Controller의 메서드에 직접 매핑된다.
