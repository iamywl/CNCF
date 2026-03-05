# 16. Jenkins CLI & Remoting 심화

## 목차

1. [개요](#1-개요)
2. [CLI 아키텍처](#2-cli-아키텍처)
3. [CLICommand 기본 클래스](#3-clicommand-기본-클래스)
4. [CLI 통신 프로토콜](#4-cli-통신-프로토콜)
5. [PlainCLIProtocol 프레임 구조](#5-plaincliprotocol-프레임-구조)
6. [서버 측 CLI 처리: CLIAction](#6-서버-측-cli-처리-cliaction)
7. [주요 내장 CLI 명령](#7-주요-내장-cli-명령)
8. [Remoting (에이전트 통신)](#8-remoting-에이전트-통신)
9. [SlaveComputer와 Channel](#9-slavecomputer와-channel)
10. [ComputerLauncher 에이전트 연결 방식](#10-computerlauncher-에이전트-연결-방식)
11. [RetentionStrategy 에이전트 생존 전략](#11-retentionstrategy-에이전트-생존-전략)
12. [FilePath 원격 파일시스템 추상화](#12-filepath-원격-파일시스템-추상화)
13. [보안 고려사항](#13-보안-고려사항)
14. [CLI와 Remoting 비교](#14-cli와-remoting-비교)
15. [정리](#15-정리)

---

## 1. 개요

Jenkins CLI는 명령줄 인터페이스를 통해 Jenkins 서버를 원격으로 제어하는 기능이다.
Remoting은 Jenkins 컨트롤러와 에이전트(슬레이브) 간의 양방향 RPC 통신 채널을 제공하는 기반 기술이다.

이 두 시스템은 서로 다른 목적을 갖지만, 역사적으로 밀접하게 연관되어 있었다.
과거에는 CLI도 Remoting 기반 채널을 사용했으나, 보안 문제로 인해 현재 CLI는
HTTP/WebSocket 기반의 PlainCLIProtocol로 전환되었다. Remoting은 에이전트 통신에만 사용된다.

```
+---------------------------+      +------------------------------+
|   jenkins-cli.jar         |      |     Jenkins Controller       |
|   (클라이언트)             |      |                              |
|                           |      |   CLIAction                  |
|   CLI.main()              | HTTP |     ServerSideImpl           |
|     Mode.WEB_SOCKET  ------>--->----->  PlainCLIProtocol        |
|     Mode.HTTP        ------>--->----->    ServerSide             |
|     Mode.SSH         ------>--->----->  SSHD Plugin              |
|                           |      |                              |
|   PlainCLIProtocol        |      |   CLICommand.clone(name)     |
|     ClientSide            |      |     .main(args, locale, ...) |
+---------------------------+      +------------------------------+

+---------------------------+      +------------------------------+
|   Agent (에이전트)         |      |     Jenkins Controller       |
|                           |      |                              |
|   remoting.jar            | TCP/ |   SlaveComputer              |
|   hudson.remoting.Engine  | WS   |     Channel                  |
|     Channel          <------>--<-->     setChannel()             |
|     Callable 실행         |      |     ComputerLauncher         |
|     FilePath 원격 작업    |      |     RetentionStrategy        |
+---------------------------+      +------------------------------+
```

---

## 2. CLI 아키텍처

### 전체 구조

Jenkins CLI 시스템은 세 개의 핵심 계층으로 구성된다.

```
+------------------------------------------------------------------+
|                     클라이언트 계층                                |
|  cli/src/main/java/hudson/cli/                                   |
|    CLI.java           -- 진입점, 모드 분기                        |
|    PlainCLIProtocol   -- 프레임 기반 바이너리 프로토콜             |
|    SSHCLI             -- SSH 모드 연결                            |
|    CLIConnectionFactory -- HTTP 인증 정보 관리                    |
+------------------------------------------------------------------+
        |                    |                    |
        | WebSocket          | HTTP               | SSH
        v                    v                    v
+------------------------------------------------------------------+
|                     서버 계층                                     |
|  core/src/main/java/hudson/cli/                                  |
|    CLIAction.java         -- HTTP/WS 엔드포인트 (/cli)           |
|      ServerSideImpl       -- PlainCLIProtocol 서버 측             |
|    CLICommand.java        -- 명령 기본 클래스                     |
|    *Command.java          -- 개별 CLI 명령 (약 40+개)            |
+------------------------------------------------------------------+
        |
        v
+------------------------------------------------------------------+
|                     명령 실행 계층                                 |
|    Jenkins API (Job, Node, Plugin, View, Queue 등)               |
+------------------------------------------------------------------+
```

### 왜 이런 구조인가

Jenkins CLI의 설계는 **ExtensionPoint 패턴**에 기반한다. 모든 CLI 명령은 `CLICommand`를
상속하고 `@Extension` 어노테이션을 붙이면 자동으로 등록된다. 이 방식 덕분에:

1. **플러그인도 CLI 명령을 추가 가능**: 플러그인이 `CLICommand`를 구현하면 자동 발견
2. **명령 이름 자동 유도**: 클래스명 `FooBarCommand` -> 명령 `foo-bar`
3. **인자 파싱 통일**: args4j 기반으로 모든 명령의 인자/옵션 파싱이 일관됨

---

## 3. CLICommand 기본 클래스

### 소스 위치

`core/src/main/java/hudson/cli/CLICommand.java`

### 클래스 선언

```java
// CLICommand.java (라인 105)
public abstract class CLICommand implements ExtensionPoint, Cloneable {
```

`CLICommand`는 `ExtensionPoint`와 `Cloneable`을 동시에 구현한다.
`ExtensionPoint`는 Jenkins의 확장 메커니즘으로, `@Extension` 어노테이션이 붙은 클래스를
자동으로 발견하고 등록한다. `Cloneable`인 이유는 명령 실행 시마다 인스턴스를 복제하여
상태 오염을 방지하기 위해서다.

### 핵심 필드

```java
// CLICommand.java (라인 126-153)
public transient PrintStream stdout, stderr;  // 원격 클라이언트의 출력 스트림
public transient InputStream stdin;           // 원격 클라이언트의 입력 스트림
@Deprecated
public transient Channel channel;             // 더 이상 사용되지 않음 (Remoting 제거)
public transient Locale locale;               // 클라이언트 로케일
private transient Charset encoding;           // 클라이언트 인코딩
private transient Authentication transportAuth;  // 전송 계층 인증 정보
```

`stdout`, `stderr`, `stdin`은 원격 클라이언트와 연결된 스트림이다.
CLI 명령 구현체가 `stdout.println()`을 호출하면, 해당 출력이 원격 클라이언트의
터미널에 표시된다. `System.out`과는 다르다 -- `System.out`은 서버 로그로 출력된다.

### getName() - 명령 이름 자동 유도

```java
// CLICommand.java (라인 178-188)
public String getName() {
    String name = getClass().getName();
    name = name.substring(name.lastIndexOf('.') + 1);   // 패키지명 제거
    name = name.substring(name.lastIndexOf('$') + 1);   // 내부 클래스 접두사 제거
    if (name.endsWith("Command"))
        name = name.substring(0, name.length() - 7);    // "Command" 접미사 제거

    // "FooBarZot" -> "foo-bar-zot" (CamelCase를 kebab-case로 변환)
    return name.replaceAll("([a-z0-9])([A-Z])", "$1-$2").toLowerCase(Locale.ENGLISH);
}
```

이 규칙에 의해:
- `BuildCommand` -> `build`
- `WhoAmICommand` -> `who-am-i`
- `ListJobsCommand` -> `list-jobs`
- `CreateJobCommand` -> `create-job`
- `InstallPluginCommand` -> `install-plugin`

### main() - 명령 실행 진입점

```java
// CLICommand.java (라인 236-273)
public int main(List<String> args, Locale locale, InputStream stdin,
                PrintStream stdout, PrintStream stderr) {
    this.stdin = new BufferedInputStream(stdin);
    this.stdout = stdout;
    this.stderr = stderr;
    this.locale = locale;
    CmdLineParser p = getCmdLineParser();

    Authentication auth = getTransportAuthentication2();
    CLIContext context = new CLIContext(getName(), args, auth);

    SecurityContext sc = null;
    Authentication old = null;
    try {
        sc = SecurityContextHolder.getContext();
        old = sc.getAuthentication();
        sc.setAuthentication(auth);  // 인증 컨텍스트 설정

        // HelpCommand와 WhoAmICommand를 제외한 모든 명령은 READ 권한 필요
        if (!(this instanceof HelpCommand || this instanceof WhoAmICommand))
            Jenkins.get().checkPermission(Jenkins.READ);

        p.parseArgument(args.toArray(new String[0]));  // args4j 파싱

        Listeners.notify(CLIListener.class, true,
            listener -> listener.onExecution(context));
        int res = run();  // 실제 명령 실행 (하위 클래스 구현)
        Listeners.notify(CLIListener.class, true,
            listener -> listener.onCompleted(context, res));

        return res;
    } catch (Throwable e) {
        int exitCode = handleException(e, context, p);
        Listeners.notify(CLIListener.class, true,
            listener -> listener.onThrowable(context, e));
        return exitCode;
    } finally {
        if (sc != null)
            sc.setAuthentication(old);  // 인증 컨텍스트 복원
    }
}
```

실행 흐름:

```
main() 호출
  |
  +-- 스트림 설정 (stdin, stdout, stderr)
  |
  +-- 인증 컨텍스트 설정 (SecurityContextHolder)
  |
  +-- Jenkins.READ 권한 확인 (HelpCommand, WhoAmICommand 제외)
  |
  +-- args4j로 인자 파싱 (@Argument, @Option 어노테이션)
  |
  +-- CLIListener.onExecution() 알림
  |
  +-- run() 호출 (추상 메서드 -- 하위 클래스가 구현)
  |
  +-- CLIListener.onCompleted() 알림
  |
  +-- 예외 발생 시 handleException()으로 종료 코드 결정
  |
  +-- 인증 컨텍스트 복원 (finally)
```

### 종료 코드 규약

```java
// CLICommand.java (라인 278-314)
protected int handleException(Throwable e, CLIContext context, CmdLineParser p) {
    int exitCode;
    switch (e) {
        case CmdLineException        -> exitCode = 2;   // 인자 파싱 오류
        case IllegalArgumentException -> exitCode = 3;   // 잘못된 인자
        case IllegalStateException    -> exitCode = 4;   // 잘못된 상태
        case AbortException           -> exitCode = 5;   // 실행 중단
        case AccessDeniedException    -> exitCode = 6;   // 권한 부족
        case BadCredentialsException  -> exitCode = 7;   // 인증 실패
        case null, default            -> exitCode = 1;   // 알 수 없는 오류
    }
    return exitCode;
}
```

| 종료 코드 | 의미 | 예외 타입 |
|-----------|------|----------|
| 0 | 성공 | - |
| 1 | 알 수 없는 오류 | 기타 Exception |
| 2 | 인자 파싱 오류 | CmdLineException |
| 3 | 잘못된 인자 | IllegalArgumentException |
| 4 | 잘못된 상태 | IllegalStateException |
| 5 | 실행 중단 | AbortException |
| 6 | 권한 부족 | AccessDeniedException |
| 7 | 인증 실패 | BadCredentialsException |
| 8-15 | 예약됨 | - |
| 16+ | 커스텀 코드 | 명령별 정의 |

### 명령 등록 및 조회

```java
// CLICommand.java (라인 539-551)
public static ExtensionList<CLICommand> all() {
    return ExtensionList.lookup(CLICommand.class);
}

public static CLICommand clone(String name) {
    for (CLICommand cmd : all())
        if (name.equals(cmd.getName()))
            return cmd.createClone();
    return null;
}
```

`clone()` 메서드는 이름으로 명령을 찾아 복제본을 반환한다. 원본 인스턴스는 싱글톤으로
등록되어 있으므로, 상태 오염 방지를 위해 항상 복제본을 사용한다.

### ThreadLocal로 현재 실행 중인 명령 추적

```java
// CLICommand.java (라인 553-566)
private static final ThreadLocal<CLICommand> CURRENT_COMMAND = new ThreadLocal<>();

static CLICommand setCurrent(CLICommand cmd) {
    CLICommand old = getCurrent();
    CURRENT_COMMAND.set(cmd);
    return old;
}

public static CLICommand getCurrent() {
    return CURRENT_COMMAND.get();
}
```

---

## 4. CLI 통신 프로토콜

### 클라이언트 진입점

`cli/src/main/java/hudson/cli/CLI.java`

CLI 클라이언트는 세 가지 모드를 지원한다.

```java
// CLI.java (라인 114)
private enum Mode { HTTP, SSH, WEB_SOCKET }
```

### 모드 선택 흐름

```java
// CLI.java (라인 116-324) - _main() 메서드에서 발췌
public static int _main(String[] _args) throws Exception {
    // 환경 변수에서 URL 확인
    String url = System.getenv("JENKINS_URL");
    if (url == null) url = System.getenv("HUDSON_URL");

    Mode mode = null;
    String auth = null;
    String bearer = null;

    // 인자 파싱
    while (!args.isEmpty()) {
        String head = args.get(0);
        switch (head) {
            case "-http"      -> mode = Mode.HTTP;
            case "-ssh"       -> mode = Mode.SSH;
            case "-webSocket" -> mode = Mode.WEB_SOCKET;
            case "-remoting"  -> { printUsage("-remoting mode is no longer supported"); return -1; }
            // -s URL, -auth user:token, -bearer token, -i keyfile ...
        }
    }

    // 기본 모드: WebSocket
    if (mode == null) {
        mode = Mode.WEB_SOCKET;
    }

    // 모드별 분기
    if (mode == Mode.SSH)
        return SSHCLI.sshConnection(url, user, args, provider, strictHostKey);
    if (mode == Mode.HTTP)
        return plainHttpConnection(url, args, factory);
    if (mode == Mode.WEB_SOCKET)
        return webSocketConnection(url, args, factory);
}
```

기본 모드가 `WEB_SOCKET`인 이유는 WebSocket이 HTTP 기반이면서도 양방향 통신을 지원하고,
방화벽/프록시를 통과하기 쉽기 때문이다. `-remoting` 모드는 보안 취약점으로 인해
완전히 제거되었다.

### 인증 방법

```
1. -auth user:token    -- HTTP Basic 인증 (API 토큰)
2. -auth @/path/file   -- 파일에서 인증 정보 읽기
3. -bearer token       -- Bearer 토큰 인증
4. 환경 변수:
   JENKINS_USER_ID + JENKINS_API_TOKEN
5. -ssh + -user + -i   -- SSH 키 기반 인증
```

### WebSocket 연결

```java
// CLI.java (라인 342-413)
private static int webSocketConnection(String url, List<String> args,
                                        CLIConnectionFactory factory) throws Exception {
    // ws:// 또는 wss:// 로 URL 변환
    // /cli/ws 엔드포인트에 WebSocket 연결
    session = client.connectToServer(
        new CLIEndpoint(), config,
        URI.create(url.replaceFirst("^http", "ws") + "cli/ws"));

    // PlainCLIProtocol을 WebSocket 바이너리 메시지로 전송
    PlainCLIProtocol.Output out = new PlainCLIProtocol.Output() {
        public void send(byte[] data) throws IOException {
            session.getBasicRemote().sendBinary(ByteBuffer.wrap(data));
        }
        public void close() throws IOException {
            session.close();
        }
    };

    // ClientSideImpl으로 명령 전송 및 응답 수신
    try (ClientSideImpl connection = new ClientSideImpl(out)) {
        session.addMessageHandler(InputStream.class, is -> {
            connection.handle(new DataInputStream(is));
        });
        connection.start(args);       // 인자 전송 + START 신호
        return connection.exit();     // 종료 코드 대기
    }
}
```

### HTTP 연결

```java
// CLI.java (라인 415-450)
private static int plainHttpConnection(String url, List<String> args,
                                        CLIConnectionFactory factory) {
    // 전이중(Full-Duplex) HTTP 스트림 생성
    FullDuplexHttpStream streams = new FullDuplexHttpStream(
        new URL(url), "cli?remoting=false", factory.authorization);

    // PlainCLIProtocol.FramedOutput으로 프레임 전송
    try (ClientSideImpl connection = new ClientSideImpl(
            new PlainCLIProtocol.FramedOutput(streams.getOutputStream()))) {
        connection.start(args);
        // 프레임 읽기 시작
        new PlainCLIProtocol.FramedReader(connection, is).start();
        // 주기적 핑 전송 (JENKINS-46659)
        // 3초마다 ENCODING 프레임을 no-op으로 전송하여 연결 유지
        return connection.exit();
    }
}
```

### SSH 연결

```java
// SSHCLI.java (라인 60-123)
static int sshConnection(String jenkinsUrl, String user, List<String> args,
                          PrivateKeyProvider provider, boolean strictHostKey) {
    // X-SSH-Endpoint 헤더에서 SSH 호스트/포트 확인
    URL url = new URL(jenkinsUrl + "login");
    URLConnection conn = openConnection(url);
    String endpointDescription = conn.getHeaderField("X-SSH-Endpoint");

    int sshPort = Integer.parseInt(endpointDescription.split(":")[1]);
    String sshHost = endpointDescription.split(":")[0];

    // Apache SSHD 클라이언트로 연결
    try (SshClient client = SshClient.setUpDefaultClient()) {
        client.start();
        ConnectFuture cf = client.connect(user, sshHost, sshPort);
        try (ClientSession session = cf.getSession()) {
            // SSH 키 인증
            for (KeyPair pair : provider.getKeys()) {
                session.addPublicKeyIdentity(pair);
            }
            session.auth().verify(10000L);

            // exec 채널로 명령 실행
            try (ClientChannel channel = session.createExecChannel(command.toString())) {
                channel.setIn(new NoCloseInputStream(System.in));
                channel.setOut(new NoCloseOutputStream(System.out));
                channel.setErr(new NoCloseOutputStream(System.err));
                channel.open().await();
                channel.waitFor(List.of(ClientChannelEvent.CLOSED), 0L);
                return channel.getExitStatus();
            }
        }
    }
}
```

---

## 5. PlainCLIProtocol 프레임 구조

### 소스 위치

`cli/src/main/java/hudson/cli/PlainCLIProtocol.java`

### 프레임 형식

```
+----------+--------+------------------+
| int(4B)  | byte   | byte[]           |
| 프레임길이 | 연산코드 | 페이로드          |
+----------+--------+------------------+

프레임길이: 연산코드 + 페이로드의 길이 (프레임길이 자체는 포함하지 않음)
연산코드: Op enum의 ordinal 값
```

### 연산 코드 (Op)

```java
// PlainCLIProtocol.java (라인 55-80)
private enum Op {
    ARG(true),        // 0: 명령 인자 (UTF-8), 클라이언트→서버
    LOCALE(true),     // 1: 로케일 (UTF-8), 클라이언트→서버
    ENCODING(true),   // 2: 인코딩 (UTF-8), 클라이언트→서버
    START(true),      // 3: 실행 시작 신호, 클라이언트→서버
    EXIT(false),      // 4: 종료 코드 (int), 서버→클라이언트
    STDIN(true),      // 5: stdin 데이터 청크, 클라이언트→서버
    END_STDIN(true),  // 6: stdin EOF, 클라이언트→서버
    STDOUT(false),    // 7: stdout 데이터 청크, 서버→클라이언트
    STDERR(false);    // 8: stderr 데이터 청크, 서버→클라이언트

    final boolean clientSide;  // true: 클라이언트가 보냄, false: 서버가 보냄
}
```

### 통신 시퀀스

```
클라이언트                              서버 (CLIAction.ServerSideImpl)
   |                                       |
   |-- ARG("build") ---------------------->|
   |-- ARG("my-job") -------------------->|
   |-- ARG("-f") ------------------------>|
   |-- ENCODING("UTF-8") --------------->|
   |-- LOCALE("ko_KR") ----------------->|
   |-- START ---------------------------->|  <- run() 시작
   |                                       |
   |<--- STDOUT(빌드 진행 로그) -----------|
   |<--- STDERR(경고 메시지) -------------|
   |                                       |
   |-- STDIN(입력 데이터) --------------->|  (필요한 경우)
   |-- END_STDIN ----------------------->|
   |                                       |
   |<--- EXIT(0) ------------------------|  <- 성공
   |                                       |
```

### FramedOutput과 FramedReader

```java
// PlainCLIProtocol.java (라인 86-167)
// 프레임 전송
static final class FramedOutput implements Output {
    private final DataOutputStream dos;

    public void send(byte[] data) throws IOException {
        dos.writeInt(data.length - 1);  // 연산코드 제외한 길이
        dos.write(data);
        dos.flush();
    }
}

// 프레임 수신 (별도 스레드)
static final class FramedReader extends Thread {
    public void run() {
        while (true) {
            int framelen = dis.readInt();       // 프레임 길이 읽기
            side.handle(new DataInputStream(    // 프레임 내용 처리
                new BoundedInputStream(dis, framelen + 1)));
        }
    }
}
```

### ServerSide와 ClientSide

```java
// PlainCLIProtocol.java (라인 256-377)
// 서버 측: 클라이언트가 보낸 Op 처리
abstract static class ServerSide extends EitherSide {
    protected boolean handle(Op op, DataInputStream dis) {
        return switch (op) {
            case ARG      -> { onArg(dis.readUTF()); yield true; }
            case LOCALE   -> { onLocale(dis.readUTF()); yield true; }
            case ENCODING -> { onEncoding(dis.readUTF()); yield true; }
            case START    -> { onStart(); yield true; }
            case STDIN    -> { onStdin(dis.readAllBytes()); yield true; }
            case END_STDIN -> { onEndStdin(); yield true; }
            default -> false;
        };
    }
    public final void sendExit(int code) throws IOException { send(Op.EXIT, code); }
    public final OutputStream streamStdout() { return stream(Op.STDOUT); }
    public final OutputStream streamStderr() { return stream(Op.STDERR); }
}

// 클라이언트 측: 서버가 보낸 Op 처리
abstract static class ClientSide extends EitherSide {
    protected boolean handle(Op op, DataInputStream dis) {
        return switch (op) {
            case EXIT   -> { onExit(dis.readInt()); yield true; }
            case STDOUT -> { onStdout(dis.readAllBytes()); yield true; }
            case STDERR -> { onStderr(dis.readAllBytes()); yield true; }
            default -> false;
        };
    }
    public final void sendArg(String text) throws IOException { send(Op.ARG, text); }
    public final void sendStart() throws IOException { send(Op.START); }
    public final OutputStream streamStdin() { return stream(Op.STDIN); }
}
```

---

## 6. 서버 측 CLI 처리: CLIAction

### 소스 위치

`core/src/main/java/hudson/cli/CLIAction.java`

### CLIAction의 역할

`CLIAction`은 `/cli` URL에 매핑된 서버 측 엔드포인트다. WebSocket과 HTTP 두 가지
전송 방식을 모두 처리한다.

```java
// CLIAction.java (라인 71-73)
@Extension @Symbol("cli")
@Restricted(NoExternalUse.class)
public class CLIAction implements UnprotectedRootAction, StaplerProxy {
```

`UnprotectedRootAction`을 구현한 이유는 CLI 연결 자체는 인증 없이 접근 가능해야 하기 때문이다.
인증은 프로토콜 레벨에서 처리된다 (HTTP 헤더 또는 SSH 키).

### WebSocket 엔드포인트 (/cli/ws)

```java
// CLIAction.java (라인 138-229)
public HttpResponse doWs(StaplerRequest2 req) {
    // WebSocket 지원 확인
    if (!WebSockets.isSupported()) { ... }

    // Origin 검증 (CSRF 방지)
    if (ALLOW_WEBSOCKET == null) {
        String actualOrigin = req.getHeader("Origin");
        // 예상 Origin과 비교
    }

    Authentication authentication = Jenkins.getAuthentication2();
    return WebSockets.upgrade(new WebSocketSession() {
        ServerSideImpl connection;

        @Override
        protected void opened() {
            connection = new ServerSideImpl(new OutputImpl(), authentication);
            new Thread(() -> {
                connection.run();   // 명령 실행 대기
                connection.close();
            }).start();
        }

        @Override
        protected void binary(byte[] payload, int offset, int len) {
            // 수신된 바이너리 프레임을 PlainCLIProtocol으로 전달
            connection.handle(new DataInputStream(
                new ByteArrayInputStream(payload, offset, len)));
        }
    });
}
```

### ServerSideImpl - 명령 실행

```java
// CLIAction.java (라인 247-358)
static class ServerSideImpl extends PlainCLIProtocol.ServerSide {
    private final List<String> args = new ArrayList<>();
    private Locale locale = Locale.getDefault();
    private Charset encoding = Charset.defaultCharset();
    private final PipedInputStream stdin = new PipedInputStream();
    private final PipedOutputStream stdinMatch = new PipedOutputStream();
    private final Authentication authentication;

    // 클라이언트가 보낸 인자/로케일/인코딩을 저장
    @Override protected void onArg(String text)      { args.add(text); }
    @Override protected void onLocale(String text)    { locale = ...; }
    @Override protected void onEncoding(String text)  { encoding = Charset.forName(text); }
    @Override protected void onStart()                { ready(); }
    @Override protected void onStdin(byte[] chunk)    { stdinMatch.write(chunk); }
    @Override protected void onEndStdin()             { stdinMatch.close(); }

    void run() throws IOException, InterruptedException {
        // START 신호 대기
        synchronized (this) {
            while (!ready && System.currentTimeMillis() < end) { wait(1000); }
        }

        // 명령 찾기
        String commandName = args.getFirst();
        CLICommand command = CLICommand.clone(commandName);
        if (command == null) {
            stderr.println("No such command " + commandName);
            sendExit(2);
            return;
        }

        // 인증 및 인코딩 설정
        command.setTransportAuth2(authentication);
        command.setClientCharset(encoding);

        // 명령 실행
        CLICommand orig = CLICommand.setCurrent(command);
        try {
            int exit = command.main(
                args.subList(1, args.size()),  // 첫 번째 인자(명령명) 제외
                locale, stdin, stdout, stderr);
            stdout.flush();
            sendExit(exit);
        } finally {
            CLICommand.setCurrent(orig);
        }
    }
}
```

### HTTP 엔드포인트

```java
// CLIAction.java (라인 232-245, 364-382)
@Override
public Object getTarget() {
    StaplerRequest2 req = Stapler.getCurrentRequest2();
    if (req.getRestOfPath().isEmpty() && "POST".equals(req.getMethod())) {
        if ("false".equals(req.getParameter("remoting"))) {
            throw new PlainCliEndpointResponse();  // PlainCLI 모드
        } else {
            throw HttpResponses.forbidden();        // remoting 모드 거부
        }
    }
    return this;
}

// FullDuplexHttpService 기반
private class PlainCliEndpointResponse extends FullDuplexHttpService.Response {
    @Override
    protected FullDuplexHttpService createService(StaplerRequest2 req, UUID uuid) {
        return new FullDuplexHttpService(uuid) {
            @Override
            protected void run(InputStream upload, OutputStream download) {
                try (ServerSideImpl connection = new ServerSideImpl(
                        new PlainCLIProtocol.FramedOutput(download),
                        Jenkins.getAuthentication2())) {
                    new PlainCLIProtocol.FramedReader(connection, upload).start();
                    connection.run();
                }
            }
        };
    }
}
```

---

## 7. 주요 내장 CLI 명령

### 명령 분류

Jenkins 코어에는 약 40개 이상의 CLI 명령이 내장되어 있다.

```
core/src/main/java/hudson/cli/
  ├── BuildCommand.java          # build
  ├── WhoAmICommand.java         # who-am-i
  ├── HelpCommand.java           # help
  ├── ListJobsCommand.java       # list-jobs
  ├── GetJobCommand.java         # get-job
  ├── CreateJobCommand.java      # create-job
  ├── UpdateJobCommand.java      # update-job
  ├── DeleteJobCommand.java      # delete-job
  ├── CopyJobCommand.java        # copy-job
  ├── ReloadJobCommand.java      # reload-job
  ├── InstallPluginCommand.java  # install-plugin
  ├── ListPluginsCommand.java    # list-plugins
  ├── EnablePluginCommand.java   # enable-plugin
  ├── DisablePluginCommand.java  # disable-plugin
  ├── ConnectNodeCommand.java    # connect-node
  ├── DisconnectNodeCommand.java # disconnect-node
  ├── OfflineNodeCommand.java    # offline-node
  ├── OnlineNodeCommand.java     # online-node
  ├── CreateNodeCommand.java     # create-node
  ├── GetNodeCommand.java        # get-node
  ├── UpdateNodeCommand.java     # update-node
  ├── DeleteNodeCommand.java     # delete-node
  ├── CreateViewCommand.java     # create-view
  ├── GetViewCommand.java        # get-view
  ├── UpdateViewCommand.java     # update-view
  ├── DeleteViewCommand.java     # delete-view
  ├── AddJobToViewCommand.java   # add-job-to-view
  ├── RemoveJobFromViewCommand.java # remove-job-from-view
  ├── ConsoleCommand.java        # console
  ├── VersionCommand.java        # version
  ├── SessionIdCommand.java      # session-id
  ├── QuietDownCommand.java      # quiet-down
  ├── CancelQuietDownCommand.java # cancel-quiet-down
  ├── ClearQueueCommand.java     # clear-queue
  ├── ReloadConfigurationCommand.java # reload-configuration
  ├── GroovyCommand.java         # groovy
  ├── GroovyshCommand.java       # groovysh
  └── ...
```

### build - 빌드 트리거

```java
// BuildCommand.java (라인 67-272)
@Extension
public class BuildCommand extends CLICommand {
    @Argument(metaVar = "JOB", usage = "Name of the job to build", required = true)
    public Job<?, ?> job;

    @Option(name = "-f", usage = "Follow the build progress")
    public boolean follow = false;

    @Option(name = "-s", usage = "Wait until completion/abortion")
    public boolean sync = false;

    @Option(name = "-w", usage = "Wait until the start")
    public boolean wait = false;

    @Option(name = "-c", usage = "Check for SCM changes")
    public boolean checkSCM = false;

    @Option(name = "-p", usage = "Build parameters in key=value format")
    public Map<String, String> parameters = new HashMap<>();

    @Option(name = "-v", usage = "Print console output. Use with -s")
    public boolean consoleOutput = false;

    @Override
    protected int run() throws Exception {
        job.checkPermission(Item.BUILD);

        // 파라미터 처리
        if (!parameters.isEmpty()) {
            ParametersDefinitionProperty pdp = job.getProperty(...);
            // 파라미터 검증 및 ParametersAction 생성
        }

        // 빌드 스케줄링
        Queue.Item item = ParameterizedJobMixIn.scheduleBuild2(
            job, 0, new CauseAction(new CLICause(...)), a);

        // 옵션에 따라 대기
        if (wait || sync || follow) {
            Run<?, ?> b = f.waitForStart();
            stdout.println("Started " + b.getFullDisplayName());
            if (sync || follow) {
                if (consoleOutput) b.writeWholeLogTo(stdout);
                f.get();  // 완료 대기
                return b.getResult().ordinal;
            }
        }
        return 0;
    }
}
```

사용 예시:
```bash
java -jar jenkins-cli.jar -s http://jenkins:8080 -auth admin:token \
    build my-job -p key1=value1 -s -v
```

### who-am-i - 현재 사용자 확인

```java
// WhoAmICommand.java (라인 38-54)
@Extension
public class WhoAmICommand extends CLICommand {
    @Override
    protected int run() {
        Authentication a = Jenkins.getAuthentication2();
        stdout.println("Authenticated as: " + a.getName());
        stdout.println("Authorities:");
        for (GrantedAuthority ga : a.getAuthorities()) {
            stdout.println("  " + ga.getAuthority());
        }
        return 0;
    }
}
```

### list-jobs - Job 목록 조회

```java
// ListJobsCommand.java (라인 42-90)
@Extension
public class ListJobsCommand extends CLICommand {
    @Argument(metaVar = "NAME", usage = "Name of the view", required = false)
    public String name;

    @Override
    protected int run() throws Exception {
        Jenkins h = Jenkins.get();
        Collection<TopLevelItem> jobs;

        if (name != null) {
            // 뷰 또는 아이템 그룹에서 Job 조회
            View view = h.getView(name);
            if (view != null) {
                jobs = view.getAllItems();
            } else {
                Item item = h.getItemByFullName(name);
                if (item instanceof ModifiableTopLevelItemGroup) {
                    jobs = ((ModifiableTopLevelItemGroup) item).getItems();
                } else {
                    throw new IllegalArgumentException("No view or item group: " + name);
                }
            }
        } else {
            jobs = h.getItems();  // 전체 Job 목록
        }

        for (TopLevelItem item : jobs) {
            stdout.println(item.getName());
        }
        return 0;
    }
}
```

### get-job / create-job / delete-job - Job CRUD (XML)

```java
// GetJobCommand.java -- Job 설정을 XML로 출력
@Extension
public class GetJobCommand extends CLICommand {
    @Argument(metaVar = "JOB", usage = "Name of the job", required = true)
    public AbstractItem job;

    @Override
    protected int run() throws Exception {
        job.writeConfigDotXml(stdout);  // config.xml 내용 출력
        return 0;
    }
}

// CreateJobCommand.java -- stdin에서 XML을 읽어 Job 생성
@Extension
public class CreateJobCommand extends CLICommand {
    @Argument(metaVar = "NAME", required = true)
    public String name;

    @Override
    protected int run() throws Exception {
        Jenkins h = Jenkins.get();
        if (h.getItemByFullName(name) != null)
            throw new IllegalStateException("Job '" + name + "' already exists");
        Jenkins.checkGoodName(name);
        ig.createProjectFromXML(name, stdin);  // stdin에서 XML 읽기
        return 0;
    }
}

// DeleteJobCommand.java -- 여러 Job 한번에 삭제
@Extension
public class DeleteJobCommand extends CLICommand {
    @Argument(usage = "Name of the job(s) to delete", required = true, multiValued = true)
    private List<String> jobs;

    @Override
    protected int run() throws Exception {
        for (String job_s : new HashSet<>(jobs)) {
            AbstractItem job = (AbstractItem) jenkins.getItemByFullName(job_s);
            job.checkPermission(Item.DELETE);
            job.delete();
        }
        return 0;
    }
}
```

### install-plugin - 플러그인 설치

```java
// InstallPluginCommand.java (라인 57-191)
@Extension
public class InstallPluginCommand extends CLICommand {
    @Argument(metaVar = "SOURCE", required = true)
    public List<String> sources = new ArrayList<>();

    @Option(name = "-restart")
    public boolean restart;

    @Option(name = "-deploy")
    public boolean dynamicLoad;

    @Override
    protected int run() throws Exception {
        h.checkPermission(Jenkins.ADMINISTER);

        for (String source : sources) {
            if (source.equals("=")) {
                // stdin에서 플러그인 파일 읽기
                FileUtils.copyInputStreamToFile(stdin, f);
            } else {
                try {
                    // URL에서 다운로드
                    FileUtils.copyURLToFile(new URL(source), f);
                } catch (MalformedURLException e) {
                    // Update Center에서 찾기
                    UpdateSite.Plugin p = h.getUpdateCenter().getPlugin(source);
                    p.deploy(dynamicLoad).get();
                }
            }
        }
        if (restart) h.safeRestart();
        return 0;
    }
}
```

### help - 명령 도움말

```java
// HelpCommand.java (라인 41-90)
@Extension
public class HelpCommand extends CLICommand {
    @Argument(metaVar = "COMMAND")
    public String command;

    @Override
    protected int run() throws Exception {
        if (command != null)
            return showCommandDetails();  // 특정 명령 상세 도움말
        showAllCommands();                // 전체 명령 목록
        return 0;
    }

    private int showAllCommands() {
        Map<String, CLICommand> commands = new TreeMap<>();
        for (CLICommand c : CLICommand.all())
            commands.put(c.getName(), c);
        for (CLICommand c : commands.values()) {
            stderr.println("  " + c.getName());
            stderr.println("    " + c.getShortDescription());
        }
        return 0;
    }
}
```

### 명령 사용 예시 요약

```bash
# 접속 기본 형식
java -jar jenkins-cli.jar -s JENKINS_URL -auth user:token COMMAND [OPTIONS]

# 빌드 트리거 (파라미터 포함, 동기 대기, 콘솔 출력)
java -jar jenkins-cli.jar build my-job -p branch=main -s -v

# Job 설정 백업/복원 (XML 파이프라인)
java -jar jenkins-cli.jar get-job my-job > my-job.xml
java -jar jenkins-cli.jar create-job new-job < my-job.xml

# 플러그인 설치 (이름 또는 URL)
java -jar jenkins-cli.jar install-plugin git -deploy
java -jar jenkins-cli.jar install-plugin https://example.com/my.hpi -deploy

# 안전한 재시작
java -jar jenkins-cli.jar safe-restart
```

---

## 8. Remoting (에이전트 통신)

### Remoting이란

Jenkins Remoting은 컨트롤러와 에이전트 간의 양방향 RPC(Remote Procedure Call)
채널을 제공하는 별도의 라이브러리다 (`remoting.jar`). 에이전트에서 코드를 원격 실행하거나,
원격 파일 시스템을 조작하는 데 사용된다.

```
+-----------------------------+         +-----------------------------+
|    Jenkins Controller       |         |    Agent (에이전트)          |
|                             |         |                             |
|  SlaveComputer              |  TCP/   |  hudson.remoting.Engine     |
|    Channel channel    <--------WS-------->  Channel                |
|    FilePath (remote)        |         |    FilePath (local)         |
|                             |         |                             |
|  Callable<T,E>              |  RPC    |  Callable<T,E>             |
|    channel.call(callable) ---->---->---->  callable.call()          |
|    T result           <------<----<----<  return T                 |
|                             |         |                             |
|  MasterToSlaveCallable      |         |  슬레이브 측 실행           |
|  SlaveToMasterCallable      |         |  (보안 제한 적용)           |
+-----------------------------+         +-----------------------------+
```

### Remoting과 CLI의 관계

과거에는 Jenkins CLI도 Remoting Channel을 사용하여 동작했다. 그러나 이 방식은
다음과 같은 심각한 보안 문제를 야기했다:

1. **임의 코드 실행**: Remoting Channel을 통해 서버 측에서 임의의 Java 코드 실행 가능
2. **직렬화 취약점**: Java 직렬화 기반 통신으로 역직렬화 공격에 취약
3. **인증 우회**: Channel 수립 시점의 인증이 불충분

이로 인해 Jenkins 2.x에서 CLI의 Remoting 모드는 완전히 제거되었다.

```java
// CLICommand.java (라인 336-339) - Remoting 모드 거부
@Deprecated
public Channel checkChannel() throws AbortException {
    throw new AbortException(
        "This command is requesting the -remoting mode which is no longer supported.");
}
```

```java
// CLI.java (라인 173-176) - Remoting 모드 거부
case "-remoting" -> {
    printUsage("-remoting mode is no longer supported");
    return -1;
}
```

현재 Remoting은 **에이전트 통신 전용**으로만 사용된다.

---

## 9. SlaveComputer와 Channel

### 소스 위치

`core/src/main/java/hudson/slaves/SlaveComputer.java`

### SlaveComputer 핵심 구조

```java
// SlaveComputer.java (라인 112-160)
public class SlaveComputer extends Computer {
    private volatile Channel channel;              // Remoting 채널
    private transient volatile boolean acceptingTasks = true;
    private Charset defaultCharset;
    private Boolean isUnix;
    private ComputerLauncher launcher;             // 에이전트 연결 방식
    private final RewindableFileOutputStream log;  // 에이전트 로그
    private final TaskListener taskListener;       // 로그 리스너
    private transient int numRetryAttempt;         // 재접속 시도 횟수
    private volatile Future<?> lastConnectActivity; // 비동기 연결 추적
    private transient volatile String absoluteRemoteFs; // 원격 FS 절대 경로
}
```

### 연결 흐름 (_connect)

```java
// SlaveComputer.java (라인 279-329)
@Override
protected Future<?> _connect(boolean forceReconnect) {
    if (channel != null) return Futures.precomputed(null);  // 이미 연결됨

    closeChannel();
    return lastConnectActivity = Computer.threadPoolForRemoting.submit(() -> {
        // 별도 스레드에서 실행 (UI 차단 방지)
        try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
            log.rewind();
            // ComputerListener에 사전 알림
            for (ComputerListener cl : ComputerListener.all())
                cl.preLaunch(SlaveComputer.this, taskListener);

            offlineCause = null;
            launcher.launch(SlaveComputer.this, taskListener);  // 실제 연결
        }
    });
}
```

`_connect()`는 항상 별도 스레드에서 실행된다. 이는 SSH 연결이나 에이전트 프로세스
시작 같은 시간이 걸리는 작업이 UI 스레드를 차단하지 않도록 하기 위함이다.

### setChannel() - 채널 수립

```java
// SlaveComputer.java (라인 430-443)
public void setChannel(@NonNull InputStream in, @NonNull OutputStream out,
                       @CheckForNull OutputStream launchLog,
                       @CheckForNull Channel.Listener listener) throws IOException, InterruptedException {
    ChannelBuilder cb = new ChannelBuilder(nodeName, threadPoolForRemoting)
        .withMode(Channel.Mode.NEGOTIATE)
        .withHeaderStream(launchLog);

    for (ChannelConfigurator cc : ChannelConfigurator.all()) {
        cc.onChannelBuilding(cb, this);
    }

    Channel channel = cb.build(in, out);
    setChannel(channel, launchLog, listener);
}
```

### setChannel(Channel, ...) - 채널 초기화

```java
// SlaveComputer.java (라인 628-770)
public void setChannel(@NonNull Channel channel, ...) {
    if (this.channel != null) throw new IllegalStateException("Already connected");

    channel.setProperty(SlaveComputer.class, this);

    // 채널 종료 리스너 등록
    channel.addListener(new LoggingChannelListener(logger, Level.FINEST) {
        @Override
        public void onClosed(Channel c, IOException cause) {
            if (cause != null) offlineCause = new ChannelTermination(cause);
            closeChannel();
            launcher.afterDisconnect(SlaveComputer.this, taskListener);
        }
    });

    // 에이전트 정보 수집 (원격 호출)
    String slaveVersion = channel.call(new SlaveVersion());
    log.println("Remoting version: " + slaveVersion);

    // 최소 Remoting 버전 검증
    VersionNumber agentVersion = new VersionNumber(slaveVersion);
    if (agentVersion.isOlderThan(RemotingVersionInfo.getMinimumSupportedVersion())) {
        if (!ALLOW_UNSUPPORTED_REMOTING_VERSIONS) {
            disconnect(new OfflineCause.LaunchFailed());
            return;
        }
    }

    // OS 감지, 인코딩 감지
    boolean _isUnix = channel.call(new DetectOS());
    String defaultCharsetName = channel.call(new DetectDefaultCharset());

    // 원격 파일 시스템 경로 확인
    String remoteFS = node.getRemoteFS();
    if (Util.isRelativePath(remoteFS)) {
        remoteFS = channel.call(new AbsolutePath(remoteFS));
    }
    FilePath root = new FilePath(channel, remoteFS);

    // 클래스로더 고정 (GC 방지)
    channel.pinClassLoader(getClass().getClassLoader());

    // ComputerListener에 사전 알림
    channel.call(new SlaveInitializer(DEFAULT_RING_BUFFER_SIZE));
    for (ComputerListener cl : ComputerListener.all()) {
        cl.preOnline(this, channel, root, taskListener);
    }

    // 원자적 채널 설정
    synchronized (channelLock) {
        if (this.channel != null) {
            channel.close();
            throw new IllegalStateException("Already connected");
        }
        isUnix = _isUnix;
        numRetryAttempt = 0;
        this.channel = channel;
        this.absoluteRemoteFs = remoteFS;
        defaultCharset = Charset.forName(defaultCharsetName);
    }

    // ComputerListener에 온라인 알림
    for (ComputerListener cl : ComputerListener.all()) {
        cl.onOnline(this, taskListener);
    }
    log.println("Agent successfully connected and online");
    Jenkins.get().getQueue().scheduleMaintenance();
}
```

채널 수립 과정:

```
1. Channel 객체 생성 (ChannelBuilder)
2. ChannelConfigurator 적용 (플러그인 확장)
3. 채널 종료 리스너 등록
4. 에이전트 Remoting 버전 확인
5. OS 감지 (Unix/Windows)
6. 기본 문자셋 감지
7. 원격 파일 시스템 경로 확인
8. 클래스로더 고정 (GC 방지)
9. SlaveInitializer 호출 (에이전트 측 초기화)
10. ComputerListener.preOnline() 알림
11. 원자적으로 채널 필드 설정
12. ComputerListener.onOnline() 알림
13. 큐 유지보수 트리거
```

### Channel을 통한 원격 호출

```java
// SlaveComputer 내부 Callable 예시
// SlaveComputer.java (라인 579-618)
static class LoadingCount extends MasterToSlaveCallable<Integer, RuntimeException> {
    private final boolean resource;

    LoadingCount(boolean resource) { this.resource = resource; }

    @Override
    public Integer call() {
        Channel c = Channel.current();  // 현재 채널
        if (c == null) return -1;
        return resource ? c.resourceLoadingCount.get() : c.classLoadingCount.get();
    }
}
```

`MasterToSlaveCallable`은 컨트롤러에서 에이전트로만 전송 가능한 Callable이다.
보안을 위해 `SlaveToMasterCallable` (에이전트->컨트롤러)은 엄격하게 제한된다.

### 재접속 전략

```java
// SlaveComputer.java (라인 853-860)
public void tryReconnect() {
    numRetryAttempt++;
    if (numRetryAttempt < 6 || numRetryAttempt % 12 == 0) {
        // 처음 5번은 빠르게 재시도, 이후 12번마다 1회 재시도
        logger.info("Attempting to reconnect " + nodeName);
        connect(true);
    }
}
```

---

## 10. ComputerLauncher 에이전트 연결 방식

### 소스 위치

`core/src/main/java/hudson/slaves/ComputerLauncher.java`

### 기본 인터페이스

```java
// ComputerLauncher.java (라인 60)
public abstract class ComputerLauncher
    implements Describable<ComputerLauncher>, ExtensionPoint {

    // 에이전트 연결
    public void launch(SlaveComputer computer, TaskListener listener)
        throws IOException, InterruptedException { ... }

    // 연결 해제 전/후 콜백
    public void beforeDisconnect(SlaveComputer computer, TaskListener listener) { ... }
    public void afterDisconnect(SlaveComputer computer, TaskListener listener) { ... }

    // 프로그래매틱 시작 지원 여부
    public boolean isLaunchSupported() { return true; }
}
```

`launch()` 메서드는 동기적으로 동작해야 한다. 비동기성은 `Computer.connect()`가 제공한다.
`launch()` 내에서 성공적으로 연결이 완료되면
`SlaveComputer.setChannel(InputStream, OutputStream, TaskListener, Channel.Listener)`를
호출하여 채널을 수립해야 한다.

### JNLPLauncher (인바운드 에이전트)

```java
// JNLPLauncher.java (라인 56-331)
@SuppressWarnings("deprecation")
public class JNLPLauncher extends ComputerLauncher {
    public String tunnel;                                  // 터널링 설정
    private RemotingWorkDirSettings workDirSettings;       // 작업 디렉토리 설정
    private boolean webSocket;                             // WebSocket 모드

    @Override
    public boolean isLaunchSupported() {
        return false;  // 자체 시작 불가 (에이전트가 먼저 접속)
    }

    @Override
    public void launch(SlaveComputer computer, TaskListener listener) {
        // 아무 작업도 하지 않음 -- 에이전트가 접속해야 함
    }
}
```

`JNLPLauncher`가 `isLaunchSupported() = false`인 이유는, 인바운드(JNLP) 방식에서는
에이전트가 먼저 컨트롤러에 접속하기 때문이다. 컨트롤러가 에이전트를 시작하는 것이 아니다.

에이전트 시작 명령:

```bash
# TCP 방식
java -jar agent.jar -url http://jenkins:8080 \
    -name agent1 -secret <secret> -webSocket

# WebSocket 방식 (권장)
java -jar agent.jar -url http://jenkins:8080 \
    -name agent1 -secret <secret> -webSocket
```

### SSHLauncher (SSH 기반)

SSH 기반 런처는 별도 플러그인(`ssh-slaves`)으로 제공된다. 개념적으로는:

```
Controller                            Agent Host
    |                                      |
    |-- SSH 연결 --------------------------->|
    |-- java -jar agent.jar 실행 ----------->|
    |                                      |
    |<--- stdin/stdout 스트림 연결 ----------|
    |                                      |
    setChannel(in, out, listener)
    |<======= Channel 수립 ================|
```

### CommandLauncher (커스텀 명령)

사용자가 지정한 명령으로 에이전트를 시작하는 방식:

```bash
# 예: SSH로 원격 에이전트 시작
ssh agent-host java -jar /path/to/agent.jar
```

### DelegatingComputerLauncher

런처를 래핑하여 추가 기능을 제공하는 패턴:

```java
// SlaveComputer.java (라인 264-276)
public ComputerLauncher getDelegatedLauncher() {
    ComputerLauncher l = launcher;
    while (true) {
        if (l instanceof DelegatingComputerLauncher) {
            l = ((DelegatingComputerLauncher) l).getLauncher();
        } else if (l instanceof ComputerLauncherFilter) {
            l = ((ComputerLauncherFilter) l).getCore();
        } else {
            break;
        }
    }
    return l;
}
```

---

## 11. RetentionStrategy 에이전트 생존 전략

### 소스 위치

`core/src/main/java/hudson/slaves/RetentionStrategy.java`

### 기본 인터페이스

```java
// RetentionStrategy.java (라인 54-66)
public abstract class RetentionStrategy<T extends Computer>
    implements Describable<RetentionStrategy<?>>, ExtensionPoint {

    // 주기적으로 호출되어 에이전트 상태 결정
    // 반환값: 다음 확인까지 대기할 분 수
    @GuardedBy("hudson.model.Queue.lock")
    public abstract long check(@NonNull T c);

    // 수동 실행 허용 여부
    public boolean isManualLaunchAllowed(T c) { return true; }

    // 태스크 수락 여부
    public boolean isAcceptingTasks(T c) { return true; }
}
```

`check()` 메서드는 `Queue.lock` 내에서 호출된다. 따라서 큐 상태를 안전하게 읽을 수 있지만,
장시간 블로킹하면 안 된다.

### Always - 항상 온라인 유지

```java
// RetentionStrategy.java (라인 161-185)
public static class Always extends RetentionStrategy<SlaveComputer> {
    @DataBoundConstructor
    public Always() {}

    @Override
    @GuardedBy("hudson.model.Queue.lock")
    public long check(SlaveComputer c) {
        if (c.isOffline() && !c.isConnecting() && c.isLaunchSupported())
            c.tryReconnect();  // 오프라인이면 재접속 시도
        return 0;  // 즉시 다시 확인
    }
}
```

`Always` 전략은 가장 단순하다. 에이전트가 오프라인이면 즉시 재접속을 시도한다.

### Demand - 필요 시 연결

```java
// RetentionStrategy.java (라인 190-301)
public static class Demand extends RetentionStrategy<SlaveComputer> {
    private final long inDemandDelay;  // 수요 지속 시간 (분)
    private final long idleDelay;      // 유휴 시간 (분)

    @DataBoundConstructor
    public Demand(long inDemandDelay, long idleDelay) {
        this.inDemandDelay = Math.max(0, inDemandDelay);
        this.idleDelay = Math.max(1, idleDelay);
    }

    @Override
    public long check(final SlaveComputer c) {
        if (c.isOffline() && c.isLaunchSupported()) {
            // 다른 노드가 처리할 수 없는 빌드가 있는지 확인
            for (Queue.BuildableItem item : Queue.getInstance().getBuildableItems()) {
                boolean needExecutor = true;
                // 현재 가용한 노드들로 처리 가능한지 확인
                for (Computer o : availableComputers.keySet()) {
                    Node otherNode = o.getNode();
                    if (otherNode != null && otherNode.canTake(item) == null) {
                        needExecutor = false;
                        break;
                    }
                }
                // 이 에이전트만 처리 가능하고, 충분한 시간 동안 수요가 있었으면
                if (needExecutor && checkedNode.canTake(item) == null) {
                    demandMilliseconds = System.currentTimeMillis() - item.buildableStartMilliseconds;
                    needComputer = demandMilliseconds > TimeUnit.MINUTES.toMillis(inDemandDelay);
                    break;
                }
            }
            if (needComputer) {
                c.connect(false);  // 연결 시작
            }
        } else if (c.isIdle()) {
            long idleMilliseconds = System.currentTimeMillis() - c.getIdleStartMilliseconds();
            if (idleMilliseconds > TimeUnit.MINUTES.toMillis(idleDelay)) {
                c.disconnect(new OfflineCause.IdleOfflineCause());  // 유휴 해제
            }
        }
        return 0;
    }
}
```

`Demand` 전략의 동작:

```
[오프라인 상태]
  |
  +-- 큐에 빌드 가능 항목이 있는가?
  |     |
  |     +-- 다른 노드가 처리할 수 있는가?
  |     |     |
  |     |     YES -> 아무것도 안 함
  |     |     NO  -> 이 노드만 처리 가능
  |     |            |
  |     |            +-- inDemandDelay 이상 수요 지속?
  |     |                  YES -> connect()
  |     |                  NO  -> 대기
  |     |
  |     NO -> 아무것도 안 함
  |
[온라인 유휴 상태]
  |
  +-- idleDelay 이상 유휴?
        YES -> disconnect()
        NO  -> 대기
```

### NOOP - 아무것도 안 함

```java
// RetentionStrategy.java (라인 125-151)
public static final RetentionStrategy<Computer> NOOP = new NoOp();

private static final class NoOp extends RetentionStrategy<Computer> {
    public long check(Computer c) { return 60; }  // 60분마다 확인
    public void start(Computer c) { c.connect(false); }  // 시작 시 연결
}
```

---

## 12. FilePath 원격 파일시스템 추상화

### 소스 위치

`core/src/main/java/hudson/FilePath.java`

### 핵심 개념

`FilePath`는 `java.io.File`과 유사하지만, 원격 노드의 파일 시스템을 투명하게
추상화한다. 컨트롤러에서 `FilePath` 메서드를 호출하면, 실제 파일 조작은
Remoting Channel을 통해 해당 파일이 위치한 노드에서 실행된다.

```java
// FilePath.java (라인 214, 238-246)
public final class FilePath implements SerializableOnlyOverRemoting {
    // 원격 노드와의 통신 채널
    // null이면 로컬, non-null이면 원격
    private transient VirtualChannel channel;

    // 파일 경로 (원격 노드 기준)
    private /*final*/ String remote;
}
```

### 생성자

```java
// FilePath.java (라인 255-258)
// 원격 파일 경로 생성
public FilePath(@CheckForNull VirtualChannel channel, @NonNull String remote) {
    this.channel = channel instanceof LocalChannel ? null : channel;
    this.remote = normalize(remote);
}

// FilePath.java (라인 267)
// 로컬 파일 경로 생성
public FilePath(@NonNull File localPath) { ... }
```

### FileCallable - 원격 코드 실행

```java
// FilePath의 act() 메서드를 통한 원격 실행
file.act(new FileCallable<Void>() {
    @Override
    public Void invoke(File f, VirtualChannel channel) {
        // f는 원격 노드의 실제 File 객체
        // 이 코드는 파일이 위치한 노드에서 실행됨
        f.delete();
        f.mkdirs();
        return null;
    }
});
```

이 방식이 중요한 이유는 **데이터 이동 최소화** 때문이다. 예를 들어 파일의 MD5 해시를
계산할 때, 파일 전체를 컨트롤러로 전송하는 대신 `FileCallable`을 에이전트로 보내서
에이전트에서 직접 계산한다.

### FilePath와 Remoting Channel의 관계

```
Controller                              Agent
+-------------------+                  +-------------------+
| FilePath           |                  |                   |
|   channel = ch     |   Channel ch     | FilePath          |
|   remote = "/ws"   |  =============>  |   channel = null  |
|                    |                  |   remote = "/ws"  |
| fp.act(callable) --|---serialize---->-| callable.invoke() |
|     result     <---|---serialize-----|-|   return result   |
+-------------------+                  +-------------------+
```

`FilePath`가 직렬화되어 에이전트로 전송되면, `channel` 필드는 `transient`이므로
null이 된다. 에이전트에서는 `channel == null`이므로 해당 파일이 로컬로 인식된다.
반대로, 로컬 `FilePath`(channel=null)가 에이전트로 전송되면, 역직렬화 시
`readResolve()`에서 현재 채널을 `channel` 필드에 설정하여 컨트롤러의 파일을
원격으로 참조하게 된다.

### SlaveComputer에서의 FilePath 사용

```java
// SlaveComputer.java의 setChannel() 내에서 (라인 714)
FilePath root = new FilePath(channel, remoteFS);
```

이 `root`는 에이전트의 작업 디렉토리를 나타내며, 빌드 시 워크스페이스 할당에 사용된다.

---

## 13. 보안 고려사항

### CLI 인증

```
+----------------------------------------------------+
|              CLI 인증 방식                           |
+----------------------------------------------------+
| 방식              | 프로토콜 | 보안 수준             |
|-------------------|---------|----------------------|
| -auth user:token  | HTTP/WS | API 토큰 기반 (권장)  |
| -bearer token     | HTTP/WS | Bearer 토큰           |
| -ssh + -i keyfile | SSH     | SSH 키 기반            |
| -auth @filepath   | HTTP/WS | 파일에서 읽기          |
| JENKINS_USER_ID + | HTTP/WS | 환경 변수에서 읽기     |
|  JENKINS_API_TOKEN|         |                       |
+----------------------------------------------------+
```

API 토큰은 Jenkins 사용자 설정에서 생성한다. 비밀번호와 달리 토큰은
개별 취소가 가능하고, 스크립트에서 안전하게 사용할 수 있다.

### CLI 권한 체계

```java
// CLICommand.java의 main()에서 (라인 256-257)
if (!(this instanceof HelpCommand || this instanceof WhoAmICommand))
    Jenkins.get().checkPermission(Jenkins.READ);
```

- `help`와 `who-am-i`는 인증 없이 접근 가능
- 나머지 명령은 최소 `Jenkins.READ` 권한 필요
- 개별 명령은 추가 권한을 확인 (예: `Item.BUILD`, `Jenkins.ADMINISTER`)

### Remoting 보안: Secret 기반 핸드셰이크

```java
// SlaveComputer.java (라인 187-189)
public String getJnlpMac() {
    return JnlpAgentReceiver.SLAVE_SECRET.mac(getName());
}
```

인바운드 에이전트가 컨트롤러에 접속할 때, 에이전트 이름에 대한 HMAC(Hash-based
Message Authentication Code)을 사용하여 인증한다. 이 Secret은 컨트롤러에서
생성되며, 에이전트 설정 시 사용자에게 제공된다.

### Remoting 보안: Callable 방향 제한

```java
// MasterToSlaveCallable -- 컨트롤러 -> 에이전트만 허용
public abstract class MasterToSlaveCallable<V, T extends Throwable>
    implements Callable<V, T> {
    @Override
    public void checkRoles(RoleChecker checker) throws SecurityException {
        checker.check(this, Roles.SLAVE);
    }
}
```

- `MasterToSlaveCallable`: 컨트롤러에서 에이전트로만 전송 가능 (안전)
- `SlaveToMasterCallable`: 에이전트에서 컨트롤러로 전송 가능 (위험 -- 제한적 사용)

에이전트에서 컨트롤러로의 Callable 실행은 `Agent -> Controller Security` 기능으로
엄격하게 통제된다. 허가되지 않은 `SlaveToMasterCallable`은 거부된다.

### Agent Protocol

| 프로토콜 | 전송 방식 | 특징 |
|---------|----------|------|
| TCP | `TcpSlaveAgentListener` | 전용 포트, 방화벽 설정 필요 |
| WebSocket | HTTP 업그레이드 | 기존 HTTP 포트 사용, 프록시 통과 용이 |

WebSocket 방식이 권장되는 이유:
1. 추가 포트 오픈 불필요
2. HTTP 프록시/로드밸런서 통과 가능
3. TLS(HTTPS)를 자연스럽게 활용

```java
// JNLPLauncher.java (라인 267, 297-309)
@Extension @Symbol({"inbound", "jnlp"})
public static class DescriptorImpl extends Descriptor<ComputerLauncher> {
    public boolean isTcpSupported() {
        return Jenkins.get().getTcpSlaveAgentListener() != null;
    }
    public boolean isWebSocketSupported() {
        return WebSockets.isSupported();
    }
}
```

### WebSocket Origin 검증 (CSRF 방지)

```java
// CLIAction.java (라인 142-159)
if (ALLOW_WEBSOCKET == null) {
    final String actualOrigin = req.getHeader("Origin");
    // Jenkins Root URL에서 Origin 추출
    String expectedOrigin = ...;
    if (actualOrigin == null || !actualOrigin.equals(expectedOrigin)) {
        return statusWithExplanation(HttpServletResponse.SC_FORBIDDEN,
            "Unexpected request origin");
    }
}
```

WebSocket CLI 엔드포인트는 CSRF 공격 방지를 위해 `Origin` 헤더를 검증한다.

---

## 14. CLI와 Remoting 비교

| 관점 | CLI | Remoting |
|------|-----|---------|
| **목적** | 관리자의 원격 명령 실행 | 에이전트와의 양방향 RPC |
| **사용자** | 관리자, CI/CD 스크립트 | Jenkins 내부 (빌드 시스템) |
| **프로토콜** | PlainCLIProtocol (프레임 기반) | Java 직렬화 기반 RPC |
| **전송** | HTTP, WebSocket, SSH | TCP, WebSocket |
| **인증** | API 토큰, SSH 키 | Secret HMAC |
| **방향** | 단방향 (클라이언트 -> 서버) | 양방향 (컨트롤러 <-> 에이전트) |
| **코드 실행** | 서버 측 CLICommand만 실행 | 원격 노드에서 Callable 실행 |
| **파일 접근** | stdin/stdout 파이프 | FilePath를 통한 원격 FS |
| **보안 모델** | Jenkins 권한 체계 | 방향별 Callable 제한 |

### 역사적 변천

```
Jenkins 1.x:
  CLI: Remoting Channel 기반 (보안 취약)
  에이전트: Remoting Channel 기반

Jenkins 2.x 초기:
  CLI: SSH + Remoting (deprecated)
  에이전트: Remoting Channel 기반

현재:
  CLI: PlainCLIProtocol (HTTP/WebSocket/SSH)
  에이전트: Remoting Channel (TCP/WebSocket)
```

---

## 15. 정리

### CLI 시스템 핵심

1. **CLICommand**: `ExtensionPoint` 기반의 추상 클래스. `@Extension`으로 자동 등록되며,
   클래스명에서 명령 이름이 자동 유도된다 (`FooBarCommand` -> `foo-bar`).

2. **PlainCLIProtocol**: 프레임 기반의 경량 바이너리 프로토콜. 9개의 Op 코드로
   인자/스트림/종료코드를 양방향으로 전달한다.

3. **CLIAction**: `/cli` 엔드포인트. WebSocket과 HTTP 두 전송 방식을 지원하며,
   `ServerSideImpl`에서 명령을 찾아 실행한다.

4. **세 가지 모드**: WebSocket(기본), HTTP, SSH. Remoting 모드는 보안상 제거됨.

### Remoting 시스템 핵심

1. **Channel**: 컨트롤러와 에이전트 간 양방향 RPC 채널. Java 직렬화 기반.

2. **SlaveComputer**: 에이전트를 나타내는 서버 측 객체. Channel을 보유하고
   연결/해제 라이프사이클을 관리한다.

3. **ComputerLauncher**: 에이전트 연결 방식의 확장 포인트. SSH, JNLP(인바운드),
   커스텀 명령 등 다양한 방식 지원.

4. **RetentionStrategy**: 에이전트의 생존 전략. Always(항상 온라인),
   Demand(필요 시 연결/해제) 등.

5. **FilePath**: 원격 파일 시스템 추상화. `FileCallable`을 통해 데이터가 위치한
   노드에서 코드를 실행하여 네트워크 오버헤드를 최소화한다.

### 설계 원칙

- **ExtensionPoint 패턴**: CLI 명령, 런처, 생존 전략 모두 플러그인으로 확장 가능
- **보안 우선**: Remoting CLI 제거, 방향별 Callable 제한, Secret 기반 인증
- **코드 이동 원칙**: 데이터를 이동하는 대신 코드를 데이터 위치로 이동 (FilePath.act())
- **비동기 연결**: UI 차단 방지를 위한 별도 스레드 풀에서 에이전트 연결 수행
- **명확한 책임 분리**: CLI는 관리 명령, Remoting은 빌드 실행이라는 역할 분리
