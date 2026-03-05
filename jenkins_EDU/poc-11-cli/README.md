# PoC-11: Jenkins CLI & Remoting 시뮬레이션

## 목적

Jenkins CLI 시스템의 핵심 동작 원리를 Go 표준 라이브러리만으로 시뮬레이션한다.
PlainCLIProtocol의 바이너리 프레임 프로토콜, CLICommand의 ExtensionPoint 기반 명령 레지스트리,
HTTP/WebSocket을 통한 CLI 통신 전체 흐름을 재현한다.

## 핵심 개념

### 1. PlainCLIProtocol — 바이너리 프레임 프로토콜

Jenkins CLI는 SSH나 Remoting 없이 순수 소켓/WebSocket 위에서 동작하는 경량 프레임 프로토콜을 사용한다.
각 프레임은 `[int length][byte Op][data]` 구조이며, Op 코드에 따라 클라이언트→서버 또는 서버→클라이언트 방향이 결정된다.

```
프레임 구조:
┌──────────────┬──────────┬──────────────────────────┐
│ Length (4B)  │ Op (1B)  │ Data (Length bytes)      │
│ Big-endian   │ ordinal  │ Op에 따라 다름           │
└──────────────┴──────────┴──────────────────────────┘

Op 코드:
  0=ARG       (C→S)  명령명/인자 (writeUTF 형식)
  1=LOCALE    (C→S)  로케일 식별자
  2=ENCODING  (C→S)  클라이언트 인코딩
  3=START     (C→S)  명령 실행 시작 신호
  4=EXIT      (S→C)  종료 코드 (writeInt)
  5=STDIN     (C→S)  stdin 데이터 청크
  6=END_STDIN (C→S)  stdin EOF
  7=STDOUT    (S→C)  stdout 데이터 청크
  8=STDERR    (S→C)  stderr 데이터 청크
```

### 2. CLICommand — ExtensionPoint 기반 명령 디스패치

모든 CLI 명령은 `CLICommand` 추상 클래스를 상속하고 `@Extension` 어노테이션으로 등록된다.
`CLICommand.clone(name)`으로 이름 기반 조회 후, `main()` → `run()` 체인으로 실행한다.

명령명 자동 변환 규칙 (`getName()`):
- 클래스명에서 `Command` 접미사 제거
- CamelCase → kebab-case 변환
- 예: `BuildCommand` → `build`, `ListJobsCommand` → `list-jobs`

### 3. CLI 통신 흐름

```
Client (jenkins-cli.jar)          Server (CLIAction)
┌─────────────────────┐           ┌─────────────────────┐
│ ClientSideImpl      │           │ ServerSideImpl      │
│                     │  ARG      │                     │
│ sendArg("build")    ├──────────►│ onArg("build")      │
│ sendArg("my-job")   ├──────────►│ onArg("my-job")     │
│ sendEncoding(...)   ├──────────►│ onEncoding(...)     │
│ sendLocale(...)     ├──────────►│ onLocale(...)       │
│ sendStart()         ├──────────►│ onStart() → ready() │
│                     │           │ CLICommand.clone()  │
│                     │           │ command.main(args)  │
│ onStdout(chunk)     │◄──────────┤ streamStdout()      │
│ onExit(code)        │◄──────────┤ sendExit(code)      │
└─────────────────────┘           └─────────────────────┘
```

### 4. 인증

CLI 클라이언트에서 `-auth user:token` 옵션으로 Basic 인증 헤더를 설정하면,
서버 측 `CLIAction`에서 `Jenkins.getAuthentication2()`로 인증 정보를 추출하여
`CLICommand.setTransportAuth2()`에 주입한다.

### 5. Remoting Channel (간략)

과거 CLI에서 사용하던 `hudson.remoting.Channel`은 양방향 직렬화 RPC 채널이다.
현재 CLI에서는 제거되었으나(`-remoting` 모드 비활성화), Jenkins 에이전트 통신에서 여전히 핵심이다.
요청 ID 기반으로 비동기 응답을 매칭하는 `pendingCalls` 패턴을 사용한다.

## 실제 소스 참조

| 구성 요소 | 소스 경로 | 핵심 내용 |
|-----------|----------|----------|
| PlainCLIProtocol | `cli/src/main/java/hudson/cli/PlainCLIProtocol.java` | Op enum, FramedOutput.send(), FramedReader.run(), EitherSide.send() |
| CLICommand | `core/src/main/java/hudson/cli/CLICommand.java` | getName(), main(), run(), clone(), all(), ExtensionPoint |
| CLIAction | `core/src/main/java/hudson/cli/CLIAction.java` | ServerSideImpl, doWs() WebSocket 엔드포인트, PlainCliEndpointResponse |
| CLI (클라이언트) | `cli/src/main/java/hudson/cli/CLI.java` | main(), ClientSideImpl, start(), webSocketConnection(), plainHttpConnection() |
| BuildCommand | `core/src/main/java/hudson/cli/BuildCommand.java` | @Extension, @Argument/@Option, Item.BUILD 권한 체크 |
| ListJobsCommand | `core/src/main/java/hudson/cli/ListJobsCommand.java` | @Extension, Jenkins.get().getItems() |
| HelpCommand | `core/src/main/java/hudson/cli/HelpCommand.java` | CLICommand.all() 순회, showAllCommands() |
| WhoAmICommand | `core/src/main/java/hudson/cli/WhoAmICommand.java` | Jenkins.getAuthentication2(), GrantedAuthority 출력 |

## 시뮬레이션 내용

| 데모 | 내용 |
|------|------|
| 데모 1 | PlainCLIProtocol 프레임 인코딩/디코딩 시각화 |
| 데모 2 | CLICommand 레지스트리 등록 및 직접 디스패치 테스트 |
| 데모 3 | TCP 소켓 위 완전한 CLI 프로토콜 흐름 (서버+클라이언트) |
| 데모 4 | Remoting Channel RPC 시뮬레이션 |
| 데모 5 | CLICommand.getName() 클래스명 → 명령명 변환 규칙 |
| 데모 6 | CLICommand 종료 코드 매핑 (0~16+) |

## 실행 방법

```bash
cd jenkins_EDU/poc-11-cli
go run main.go
```

## 예상 출력

```
================================================================================
  Jenkins CLI & Remoting 시뮬레이션
  PlainCLIProtocol 프레임 기반 통신 + CLICommand 레지스트리
================================================================================

[데모 1] PlainCLIProtocol 프레임 인코딩/디코딩
----------------------------------------------------------------------

프레임 구조 (PlainCLIProtocol.java):

  ┌──────────────┬──────────┬──────────────────────────┐
  │ Length (4B)  │ Op (1B)  │ Data (Length bytes)      │
  │ Big-endian   │ ordinal  │ Op에 따라 다름           │
  └──────────────┴──────────┴──────────────────────────┘

Op 코드 목록:
  ...

클라이언트 → 서버 프레임 시퀀스 예제:

  ARG        len=7   hex=[00 00 00 07 00 00 05 62 75 69 6c 64] → "build"
  ARG        len=13  hex=[...] → "my-pipeline"
  ENCODING   len=7   hex=[...] → "UTF-8"
  LOCALE     len=7   hex=[...] → "ko_KR"
  START      len=0   hex=[00 00 00 00 03]

인코딩/디코딩 왕복 검증:
  ARG       : encode(12 bytes) → decode → Op일치=true, Data일치=true
  ...

[데모 2] CLICommand 레지스트리 및 디스패치
----------------------------------------------------------------------

등록된 명령 (ExtensionList<CLICommand> 시뮬레이션):
  build                — 작업을 빌드합니다 (선택적으로 완료까지 대기)
  help                 — 모든 사용 가능한 명령 목록을 출력합니다
  list-jobs            — 모든 작업을 해당 뷰/항목 그룹에서 나열합니다
  safe-restart         — 빌드 완료 후 Jenkins를 안전하게 재시작합니다
  version              — Jenkins 버전을 출력합니다
  who-am-i             — 현재 인증 정보와 권한을 출력합니다

명령 디스패치 테스트:

  [version] → exit=0
    stdout: Jenkins ver. 2.462.1-SIMULATION
  ...

[데모 3] TCP 소켓 위 PlainCLIProtocol 전체 흐름
----------------------------------------------------------------------

CLI 서버 시작: 127.0.0.1:xxxxx

--- help 명령 (전체 명령 목록) ---
  요청: java -jar jenkins-cli.jar help
  종료 코드: 0
  stdout: build                ...
  ...

--- build my-pipeline (인증 포함) ---
  요청: java -jar jenkins-cli.jar build my-pipeline -auth admin:token123
  종료 코드: 0
  stdout: Started my-pipeline #1
  stdout: Completed my-pipeline #1 : SUCCESS

[데모 4] Remoting Channel RPC 시뮬레이션 (간략)
----------------------------------------------------------------------

  getSystemProperty(os.name) → "Linux"
  getEnvironmentVariable(JENKINS_HOME) → "/var/jenkins_home"
  ...

[데모 5] CLICommand.getName() — 클래스명 → 명령명 변환 규칙
----------------------------------------------------------------------

  BuildCommand                   → build
  ListJobsCommand                → list-jobs
  WhoAmICommand                  → who-am-i
  ...

[데모 6] CLICommand 종료 코드 매핑
----------------------------------------------------------------------

  0 = 정상 완료
  1 = 예기치 않은 예외
  ...

================================================================================
  시뮬레이션 완료
================================================================================
```
