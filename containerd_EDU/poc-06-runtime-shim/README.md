# PoC-06: Shim 프로세스 생명주기

## 목적

containerd의 shim 프로세스 아키텍처를 시뮬레이션한다. containerd는 각 컨테이너마다 독립적인 shim 프로세스를 실행하여 컨테이너 런타임(runc)과 통신한다. shim은 containerd 재시작과 독립적으로 컨테이너를 관리할 수 있는 핵심 아키텍처 컴포넌트이다.

## 핵심 개념

### 1. Shim Manager (Start/Stop)

ShimManager는 shim 프로세스의 생명주기를 관리한다. 실제 containerd에서는 `exec.Command`로 shim 바이너리(`containerd-shim-runc-v2`)를 실행한다.

```
containerd → exec.Command("containerd-shim-runc-v2", "-id", id, "start")
          ← stdout: BootstrapParams JSON
          → TTRPC 연결 (params.Address)
```

### 2. BootstrapParams

shim이 시작되면 stdout에 JSON 형태의 `BootstrapParams`를 출력한다. containerd는 이 정보로 shim에 TTRPC 연결을 설정한다.

```go
type BootstrapParams struct {
    Version  int    // 2 (shim v2)
    Address  string // "unix:///run/containerd/s/..."
    Protocol string // "ttrpc" 또는 "grpc"
}
```

### 3. TTRPC TaskService

shim은 TTRPC 서버로 동작하며 TaskService API를 제공한다:

| API | 설명 |
|-----|------|
| `Create` | 컨테이너 생성 (runc create) |
| `Start` | init 프로세스 시작 (runc start) |
| `Kill` | 시그널 전송 |
| `Delete` | 컨테이너 삭제 |
| `Exec` | 추가 프로세스 실행 |
| `State` | 프로세스 상태 조회 |
| `Wait` | 종료 대기 |

### 4. 이벤트 포워딩 (shim -> containerd)

shim은 내부 이벤트 채널(128 버퍼)을 통해 비동기적으로 이벤트를 containerd에 전달한다.

```
shim.send(event)
  → s.events 채널
  → forward() goroutine
  → publisher.Publish(topic, event)
  → TTRPC 연결 → containerd
```

### 5. 프로세스 Reaping

shim은 subreaper로 설정되어 자식 프로세스의 종료를 감지한다. `processExits()` goroutine이 종료 이벤트를 처리하여 컨테이너 상태를 업데이트하고 exit 이벤트를 발행한다.

```
커널 → SIGCHLD → reaper → s.ec 채널 → processExits()
  → 컨테이너 상태 업데이트 (running → stopped)
  → TaskExit 이벤트 발행
```

### 6. Shim 독립성

각 컨테이너마다 독립적인 shim 프로세스가 존재하므로:
- containerd가 재시작되어도 컨테이너는 계속 실행
- shim이 BootstrapParams의 address로 재연결 가능
- 하나의 shim 장애가 다른 컨테이너에 영향 없음

```
┌───────────┐     TTRPC      ┌──────────┐     runc      ┌───────────┐
│ containerd│ <------------>│  shim    │ ------------> │ container │
│           │               │ (per-ctr)│              │ process   │
└───────────┘               └──────────┘              └───────────┘
```

## 소스 참조

| 파일 | 설명 |
|------|------|
| `pkg/shim/shim.go` | Manager 인터페이스, BootstrapParams, StartOpts, Run(), serve() |
| `cmd/containerd-shim-runc-v2/task/service.go` | TaskService (Create, Start, Kill, Delete, Exec), forward(), processExits() |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
containerd Shim 프로세스 생명주기 시뮬레이션
========================================

--- 시나리오 1: Shim Manager를 통한 Shim 프로세스 시작 ---
  [ShimManager] Shim 시작: id=container-abc, pid=..., address=unix:///..., protocol=ttrpc

  BootstrapParams:
    Version:  2
    Address:  unix:///run/containerd/s/...
    Protocol: ttrpc

--- 시나리오 2: TTRPC TaskService 호출 ---
  [1] Create: 컨테이너 생성
  [이벤트 전달] /tasks/create → containerd(...)
  [2] Start: init 프로세스 시작
  [이벤트 전달] /tasks/start → containerd(...)
  [3] Exec: 추가 프로세스 실행
  [4] Kill: 시그널 전송 → processExits 처리
  [5] Delete: 컨테이너 삭제

--- 시나리오 3: 여러 컨테이너에 대한 독립 Shim 프로세스 ---
  각 컨테이너마다 독립 shim 실행

--- 시나리오 4: Shim 종료 및 정리 ---
  Publisher 닫힘, Shim 종료

--- 이벤트 흐름 요약 ---
  각 Action별 Topic과 이벤트 전달 흐름 표시
```
