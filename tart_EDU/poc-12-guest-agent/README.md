# PoC-12: Guest Agent 명령 실행 시뮬레이션

## 개요

Tart의 `tart exec` 명령이 사용하는 Guest Agent 통신 구조를 Go로 재현한다. Unix 도메인 소켓(`control.sock`)을 통한 호스트-게스트 통신, 명령 실행 요청/응답 프로토콜, stdin/stdout/stderr 동시 스트리밍, 대화형 모드를 시뮬레이션한다.

## Tart 소스코드 매핑

| Tart 소스 | PoC 대응 | 설명 |
|-----------|---------|------|
| `Commands/Exec.swift` — `struct Exec: AsyncParsableCommand` | `main()` 전체 | exec 명령 구조 |
| `AgentAsyncClient(channel:)` | `HostClient` 구조체 | 호스트 측 gRPC 클라이언트 |
| `execCall.requestStream.send(.command(...))` | `HostClient.Execute()` | 명령 전송 |
| `execCall.responseStream` 반복 | 응답 수신 루프 | stdout/stderr/exit 스트리밍 |
| `GRPCChannelPool.with(target: .unixDomainSocket(...))` | `net.Dial("unix", ...)` | Unix 소켓 연결 |
| `FileHandle.standardInput.readabilityHandler` | `HostClient.SendStdin()` | stdin 전달 |
| `withThrowingTaskGroup` 병렬 처리 | `sync.WaitGroup` 기반 goroutine | stdout/stderr 동시 스트리밍 |
| `ExecCustomExitCodeError(exitCode:)` | `ExecResponse.Code` | 커스텀 종료 코드 전파 |
| Unix 소켓 104바이트 제한 회피 (cwd 변경) | `filepath.Join(tmpDir, ...)` | 소켓 경로 관리 |

## 핵심 개념

### 1. 통신 아키텍처

```
호스트 (tart exec)                    게스트 (Tart Guest Agent)
┌──────────────┐                     ┌──────────────────┐
│ HostClient   │ ── Unix Socket ──→  │ GuestAgent       │
│              │    control.sock     │                  │
│ stdin ──────→│ ── MsgCommand ───→  │ exec.Command()   │
│              │ ── MsgStdinData ──→ │   ├── stdin      │
│ stdout ←────│ ←── MsgStdout ────  │   ├── stdout ───→│
│ stderr ←────│ ←── MsgStderr ────  │   └── stderr ───→│
│ exit code ←─│ ←── MsgExit ──────  │   exit code ────→│
└──────────────┘                     └──────────────────┘
```

### 2. 프로토콜 메시지

| 방향 | 타입 | 필드 | 설명 |
|------|------|------|------|
| 호스트 → 게스트 | `command` | name, args, interactive, tty | 명령 실행 요청 |
| 호스트 → 게스트 | `standard_input` | data | stdin 데이터 전달 (빈 문자열 = EOF) |
| 호스트 → 게스트 | `terminal_resize` | cols, rows | 터미널 크기 변경 (SIGWINCH) |
| 게스트 → 호스트 | `standard_output` | data | stdout 스트리밍 |
| 게스트 → 호스트 | `standard_error` | data | stderr 스트리밍 |
| 게스트 → 호스트 | `exit` | code | 종료 코드 (0 = 성공) |

### 3. 대화형 모드 (-i 플래그)

대화형 모드에서는 호스트의 stdin을 게스트로 스트리밍한다. Tart에서는 `FileHandle.standardInput.readabilityHandler`로 비동기 입력을 처리하고, EOF 시 빈 Data를 전송하여 게스트에 입력 종료를 알린다.

### 4. Unix 소켓 경로 제한

Unix 도메인 소켓 경로는 최대 104바이트(macOS)로 제한된다. Tart는 이를 회피하기 위해 VM 디렉토리의 `baseURL`로 현재 작업 디렉토리를 변경한 뒤, 상대 경로(`control.sock`)로 소켓에 접근한다.

## 실행 방법

```bash
cd poc-12-guest-agent
go run main.go
```

## 실행 결과 (요약)

```
tart Guest Agent 명령 실행 시뮬레이션

--- 1. 기본 명령 실행 (echo) ---
[게스트] 에이전트 시작: /tmp/.../control.sock
[호스트] 명령 전송: echo Hello from tart VM!
[게스트] 명령 수신: echo Hello from tart VM!
[호스트:stdout] Hello from tart VM!
[호스트] 종료 코드 수신: 0

--- 3. stderr 출력 (존재하지 않는 경로) ---
[호스트:stderr] ls: /nonexistent-path-tart-poc: No such file or directory
[호스트] 종료 코드 수신: 1 (0이 아님 = 에러)

--- 5. 대화형 모드 — stdin → 게스트 전달 ---
[호스트:stdout] 첫 번째 줄
[호스트:stdout] 두 번째 줄
```

## 학습 포인트

1. **Unix 도메인 소켓 기반 IPC**: 파일 시스템 경로로 식별되는 로컬 프로세스 간 통신
2. **양방향 스트리밍**: stdin/stdout/stderr를 동시에 처리하는 다중 goroutine 패턴
3. **프로토콜 설계**: 타입 기반 메시지 분기 (command/stdin/stdout/stderr/exit)
4. **EOF 시그널링**: 빈 데이터 전송으로 입력 종료를 알리는 규약
5. **종료 코드 전파**: 게스트 프로세스의 exit code를 호스트로 전달하여 동일한 코드로 종료
6. **경로 길이 제한 회피**: cwd 변경 + 상대 경로 사용 패턴
