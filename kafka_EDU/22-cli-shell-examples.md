# 22. CLI 도구, Metadata Shell, Examples

> Kafka의 CLI 도구 생태계 — kafka-topics, kafka-consumer-groups부터 Metadata Shell과 공식 예제까지

---

## 1. 개요

Kafka는 브로커/컨트롤러 코어 외에도 운영자와 개발자를 위한 풍부한 CLI 도구 생태계를 제공한다.
이 문서는 세 가지 영역을 다룬다:

1. **CLI 도구** (`tools/src/`): kafka-topics, kafka-consumer-groups 등 운영 명령줄 도구
2. **Metadata Shell** (`shell/src/`): KRaft 메타데이터 스냅샷을 대화형으로 탐색하는 셸
3. **Examples** (`examples/src/`): 공식 프로듀서/컨슈머/트랜잭션 예제 코드

### 왜 CLI 도구가 중요한가?

Kafka 클러스터의 일상 운영에서 CLI 도구는 필수적이다. 웹 UI가 없는 환경에서도
토픽 생성, 컨슈머 그룹 상태 확인, 오프셋 리셋 등을 수행할 수 있어야 한다.
Metadata Shell은 KRaft 전환 이후 메타데이터 디버깅의 핵심 도구가 되었다.

---

## 2. CLI 도구 아키텍처

### 2.1 도구 분류

```
tools/src/main/java/org/apache/kafka/tools/
├── GroupsCommand.java         # kafka-groups (그룹 목록)
├── ConsumerPerformance.java   # 컨슈머 성능 테스트
├── ProducerPerformance.java   # 프로듀서 성능 테스트
├── GetOffsetShell.java        # 오프셋 조회
├── DumpLogSegments.java       # 로그 세그먼트 덤프
├── ClusterTool.java           # 클러스터 ID 조회
├── FeatureCommand.java        # 기능 버전 관리
├── BrokerApiVersionsCommand.java # API 버전 조회
├── JmxTool.java               # JMX 메트릭 조회
├── DeleteRecordsCommand.java  # 레코드 삭제
├── LeaderElectionCommand.java # 리더 선출 트리거
├── LogDirsCommand.java        # 로그 디렉토리 관리
├── MetadataQuorumCommand.java # 메타데이터 쿼럼 상태
├── DelegationTokenCommand.java # 위임 토큰 관리
├── ClientMetricsCommand.java  # 클라이언트 메트릭
├── ConnectPluginPath.java     # Connect 플러그인 경로
└── consumer/                  # 콘솔 컨슈머
    └── ConsoleConsumer.java
```

### 2.2 공통 아키텍처 패턴

모든 CLI 도구는 동일한 아키텍처 패턴을 따른다:

```
┌──────────────────────────────────────────────┐
│               main(String[] args)             │
│  ┌──────────────────────────────────────────┐ │
│  │  1. ArgumentParser / OptionParser 설정    │ │
│  │  2. 옵션 파싱 (bootstrap-server, etc.)   │ │
│  │  3. Properties → AdminClient 생성        │ │
│  │  4. 비즈니스 로직 실행                    │ │
│  │  5. 결과 포맷팅 + 출력                   │ │
│  │  6. Exit.exit(code)                      │ │
│  └──────────────────────────────────────────┘ │
└──────────────────────────────────────────────┘
```

### 2.3 GroupsCommand 상세 분석

`GroupsCommand`는 Kafka 그룹 관리의 핵심 CLI이다.

**소스 위치**: `tools/src/main/java/org/apache/kafka/tools/GroupsCommand.java`

#### 진입점과 서비스 분리

```java
public class GroupsCommand {
    public static void main(String... args) {
        Exit.exit(mainNoExit(args));
    }

    static void execute(String... args) throws Exception {
        GroupsCommandOptions opts = new GroupsCommandOptions(args);
        Properties config = opts.commandConfig();
        config.put(AdminClientConfig.BOOTSTRAP_SERVERS_CONFIG, opts.bootstrapServer());

        try (GroupsService service = new GroupsService(config)) {
            if (opts.hasListOption()) {
                service.listGroups(opts);
            }
        }
    }
}
```

**왜 Service 패턴을 사용하는가?**

`GroupsService`를 `AutoCloseable`로 분리한 이유:
1. **테스트 용이성**: `GroupsService(Admin adminClient)` 생성자로 Mock AdminClient 주입 가능
2. **리소스 관리**: try-with-resources로 AdminClient를 확실히 정리
3. **관심사 분리**: 옵션 파싱(Options)과 비즈니스 로직(Service)이 명확히 분리

#### 그룹 필터링 메커니즘

```java
private boolean combinedFilter(GroupListing group,
        Optional<GroupType> groupTypeFilter,
        Optional<String> protocolFilter,
        boolean consumerGroupFilter,
        boolean shareGroupFilter,
        boolean streamsGroupFilter) {

    Optional<GroupType> groupType = group.type();
    String protocol = group.protocol();

    if (groupTypeFilter.isPresent()) {
        // --group-type 필터
        return groupType.filter(gt -> gt == groupTypeFilter.get()).isPresent()
            && protocolFilter.map(protocol::equals).orElse(true);
    } else if (consumerGroupFilter) {
        // --consumer 필터: consumer 프로토콜 또는 CONSUMER 타입
        return protocol.equals("consumer") || protocol.isEmpty()
            || groupType.filter(gt -> gt == GroupType.CONSUMER).isPresent();
    } else if (shareGroupFilter) {
        return groupType.filter(gt -> gt == GroupType.SHARE).isPresent();
    } else if (streamsGroupFilter) {
        return groupType.filter(gt -> gt == GroupType.STREAMS).isPresent();
    }
    return true;
}
```

**왜 여러 필터 모드가 존재하는가?**

Kafka 4.0부터 그룹 타입이 다양해졌다:
- **Classic Group**: 기존 컨슈머 그룹 프로토콜
- **Consumer Group**: 새 서버사이드 할당 프로토콜
- **Share Group**: 공유 구독 (KIP-932)
- **Streams Group**: Streams 애플리케이션 전용

각 타입별로 필터링할 수 있어야 운영자가 효율적으로 관리할 수 있다.

### 2.4 옵션 파싱 프레임워크

Kafka CLI는 두 가지 옵션 파싱 라이브러리를 사용한다:

| 라이브러리 | 사용 도구 | 특징 |
|-----------|----------|------|
| jopt-simple | GroupsCommand, 레거시 도구 | `--옵션` 스타일, OptionSpec 기반 |
| argparse4j | ProducerPerformance, MetadataShell | Python argparse 스타일, 위치 인수 지원 |

#### CommandDefaultOptions 베이스 클래스

```java
public class GroupsCommandOptions extends CommandDefaultOptions {
    // CommandDefaultOptions가 --help, --version 자동 제공
    private final ArgumentAcceptingOptionSpec<String> bootstrapServerOpt;
    private final OptionSpecBuilder listOpt;

    public GroupsCommandOptions(String[] args) {
        super(args);
        bootstrapServerOpt = parser.accepts("bootstrap-server", "...")
            .withRequiredArg().required().ofType(String.class);
        listOpt = parser.accepts("list", "List the groups.");
        // ...
        checkArgs();  // 유효성 검증
    }
}
```

**왜 checkArgs()를 별도로 두는가?**

파싱과 검증을 분리함으로써:
- 상호 배타적 옵션(mutual exclusion) 검증
- 필수 옵션 조합 검증
- 의미적 유효성 검증 (예: group-type이 유효한 값인지)

---

## 3. Metadata Shell

### 3.1 아키텍처

KRaft 메타데이터를 대화형으로 탐색하는 셸 도구이다.

```
┌─────────────────────────────────────────────────┐
│                   MetadataShell                  │
│                                                  │
│  ┌──────────────┐   ┌──────────────────────────┐│
│  │SnapshotFile  │──▶│    MetadataLoader        ││
│  │  Reader      │   │  (배치 처리/퍼블리셔)     ││
│  └──────────────┘   └──────────┬───────────────┘│
│                                │                 │
│                     ┌──────────▼───────────────┐ │
│                     │  MetadataShellPublisher   │ │
│                     │  (상태 갱신)              │ │
│                     └──────────┬───────────────┘ │
│                                │                 │
│                     ┌──────────▼───────────────┐ │
│                     │   MetadataShellState      │ │
│                     │  (토픽/파티션/브로커 등)   │ │
│                     └──────────┬───────────────┘ │
│                                │                 │
│  ┌─────────────────────────────▼───────────────┐ │
│  │          InteractiveShell                    │ │
│  │  ┌──────────────┐  ┌─────────────────────┐  │ │
│  │  │ JLine Reader │  │ MetadataShell       │  │ │
│  │  │ (자동완성,   │  │ Completer           │  │ │
│  │  │  히스토리)    │  │ (명령어 탭 완성)     │  │ │
│  │  └──────────────┘  └─────────────────────┘  │ │
│  └─────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────┘
```

**소스 위치**: `shell/src/main/java/org/apache/kafka/shell/`

### 3.2 MetadataShell 진입점

```java
public final class MetadataShell {
    public void run(List<String> args) throws Exception {
        initializeWithSnapshotFileReader();
        loader.installPublishers(List.of(publisher)).get(15, TimeUnit.MINUTES);

        if (args == null || args.isEmpty()) {
            // 대화형 모드
            waitUntilCaughtUp();
            try (InteractiveShell shell = new InteractiveShell(state)) {
                shell.runMainLoop();
            }
        } else {
            // 비대화형 모드 (단일 명령 실행)
            waitUntilCaughtUp();
            Commands commands = new Commands(false);
            Commands.Handler handler = commands.parseCommand(args);
            handler.run(Optional.empty(), writer, state);
        }
    }
}
```

**왜 두 가지 모드를 지원하는가?**

- **대화형**: 운영자가 실시간으로 메타데이터를 탐색
- **비대화형**: 스크립트에서 특정 정보를 추출 (예: `kafka-metadata-shell --snapshot file --cat /brokers/1`)

### 3.3 InteractiveShell과 JLine

```java
public final class InteractiveShell implements AutoCloseable {
    public InteractiveShell(MetadataShellState state) throws IOException {
        this.terminal = TerminalBuilder.builder()
            .system(true).nativeSignals(true).build();
        this.history = new DefaultHistory();
        this.reader = LineReaderBuilder.builder()
            .terminal(terminal)
            .parser(new DefaultParser())
            .history(history)
            .completer(new MetadataShellCompleter(state))
            .build();
    }

    public void runMainLoop() throws Exception {
        terminal.writer().println("[ Kafka Metadata Shell ]");
        Commands commands = new Commands(true);
        while (true) {
            reader.readLine(">> ");
            ParsedLine parsedLine = reader.getParsedLine();
            Commands.Handler handler = commands.parseCommand(parsedLine.words());
            handler.run(Optional.of(this), terminal.writer(), state);
        }
    }
}
```

**왜 JLine을 사용하는가?**

JLine은 Java용 터미널 라이브러리로:
1. **자동완성**: Tab 키로 명령어/경로 자동완성
2. **히스토리**: 이전 명령어 검색 (화살표 키)
3. **시그널 처리**: Ctrl+C 등 터미널 시그널 우아한 처리
4. **플랫폼 독립**: Windows/Unix 터미널 차이 추상화

### 3.4 명령어 자동완성 메커니즘

```java
static class MetadataShellCompleter implements Completer {
    @Override
    public void complete(LineReader reader, ParsedLine line,
                        List<Candidate> candidates) {
        if (line.words().isEmpty()) {
            // 빈 입력: 모든 명령어 후보 제시
            CommandUtils.completeCommand("", candidates);
        } else if (line.words().size() == 1) {
            // 첫 단어: 명령어 이름 완성
            CommandUtils.completeCommand(line.words().get(0), candidates);
        } else {
            // 후속 인수: 명령어별 특화 완성
            String command = line.words().get(0);
            Commands.Type type = Commands.TYPES.get(command);
            type.completeNext(state, nextWords, candidates);
        }
    }
}
```

### 3.5 스냅샷 로딩 파이프라인

```java
private void initializeWithSnapshotFileReader() throws Exception {
    // 1. 디렉토리 잠금 (다른 브로커와 충돌 방지)
    this.fileLock = takeDirectoryLockIfExists(
        parentParent(new File(snapshotPath)));

    // 2. MetadataLoader 생성
    this.loader = new MetadataLoader.Builder()
        .setFaultHandler(faultHandler)
        .setNodeId(-1)  // 셸은 노드가 아님
        .setHighWaterMarkAccessor(() -> snapshotFileReader.highWaterMark())
        .build();

    // 3. SnapshotFileReader가 배치를 Loader에 공급
    snapshotFileReader = new SnapshotFileReader(snapshotPath, loader);
    snapshotFileReader.startup();
}
```

**왜 FileLock을 사용하는가?**

브로커가 사용 중인 스냅샷을 셸이 동시에 읽으면 일관성 문제가 발생할 수 있다.
`takeDirectoryLockIfExists`는 `.lock` 파일이 존재하는 경우에만 잠금을 시도하여,
브로커 디렉토리의 스냅샷은 안전하게 읽고, 독립 스냅샷 파일은 잠금 없이 읽는다.

### 3.6 셸 명령어 체계

```
┌──────────────────────────────────────┐
│          Commands.TYPES Map           │
├──────────────────────────────────────┤
│ "cat"     → CatCommandHandler        │
│ "cd"      → CdCommandHandler         │
│ "find"    → FindCommandHandler       │
│ "help"    → HelpCommandHandler       │
│ "history" → HistoryCommandHandler    │
│ "ls"      → LsCommandHandler         │
│ "man"     → ManCommandHandler        │
│ "pwd"     → PwdCommandHandler        │
│ "exit"    → ExitCommandHandler       │
│ "tree"    → TreeCommandHandler       │
└──────────────────────────────────────┘
```

메타데이터를 파일시스템처럼 탐색할 수 있다:
- `/brokers/0` - 브로커 0의 정보
- `/topics/my-topic/0` - my-topic 파티션 0의 메타데이터
- `/features/` - 기능 버전 정보

---

## 4. Kafka Examples

### 4.1 공식 예제 구조

```
examples/src/main/java/kafka/examples/
├── KafkaConsumerProducerDemo.java   # 기본 프로듀서/컨슈머 데모
├── KafkaExactlyOnceDemo.java        # Exactly-Once 트랜잭션 데모
├── Producer.java                     # 프로듀서 구현체
├── Consumer.java                     # 컨슈머 구현체
├── ExactlyOnceMessageProcessor.java  # EOS 메시지 프로세서
├── TransactionalClientDemo.java      # 트랜잭션 클라이언트 데모
├── KafkaProperties.java             # 설정 상수
└── Utils.java                        # 유틸리티
```

### 4.2 KafkaConsumerProducerDemo 분석

```java
public class KafkaConsumerProducerDemo {
    public static final String TOPIC_NAME = "my-topic";
    public static final String GROUP_NAME = "my-group";

    public static void main(String[] args) {
        int numRecords = Integer.parseInt(args[0]);
        boolean isAsync = args.length == 1
            || !args[1].trim().equalsIgnoreCase("sync");

        // 1단계: 토픽 재생성 (깨끗한 상태)
        Utils.recreateTopics(KafkaProperties.BOOTSTRAP_SERVERS,
                            -1, TOPIC_NAME);
        CountDownLatch latch = new CountDownLatch(2);

        // 2단계: 프로듀서 스레드 시작
        Producer producerThread = new Producer("producer",
            KafkaProperties.BOOTSTRAP_SERVERS, TOPIC_NAME,
            isAsync, null, false, numRecords, -1, latch);
        producerThread.start();

        // 3단계: 컨슈머 스레드 시작
        Consumer consumerThread = new Consumer("consumer",
            KafkaProperties.BOOTSTRAP_SERVERS, TOPIC_NAME,
            GROUP_NAME, Optional.empty(), false, numRecords, latch);
        consumerThread.start();

        // 타임아웃 대기
        latch.await(5, TimeUnit.MINUTES);
    }
}
```

**왜 CountDownLatch를 사용하는가?**

프로듀서와 컨슈머가 독립 스레드에서 실행되므로, 둘 다 완료될 때까지
main 스레드가 대기해야 한다. CountDownLatch(2)로 두 스레드의 완료를 동기화한다.

### 4.3 Exactly-Once 데모

```java
public class KafkaExactlyOnceDemo {
    // read-process-write 패턴
    // 입력 토픽에서 읽고, 처리하고, 출력 토픽에 트랜잭셔널하게 쓰기

    // 핵심: consumer.commitAsync()가 아닌
    // producer.sendOffsetsToTransaction()으로 오프셋을 커밋
    // → 메시지 전송과 오프셋 커밋이 원자적으로 발생
}
```

**왜 이 예제가 중요한가?**

Exactly-Once Semantics(EOS)는 Kafka의 가장 복잡한 기능 중 하나이다.
이 예제는 `read-process-write` 패턴의 정확한 구현을 보여주며,
많은 프로덕션 스트림 처리 파이프라인의 기반이 된다.

### 4.4 예제 설계 패턴

| 패턴 | 설명 | 예제 |
|------|------|------|
| Thread-per-role | 프로듀서/컨슈머를 별도 스레드로 | KafkaConsumerProducerDemo |
| Latch 동기화 | CountDownLatch로 완료 대기 | 모든 데모 |
| Graceful Shutdown | wakeup() + shutdown flag | Consumer.java |
| 토픽 자동 관리 | recreateTopics()로 깨끗한 환경 | Utils.java |

---

## 5. CLI 도구 확장 포인트

### 5.1 새 CLI 도구 추가 패턴

```
1. CommandOptions 클래스 생성 (extends CommandDefaultOptions)
   - parser.accepts()로 옵션 정의
   - checkArgs()로 유효성 검증

2. Service 클래스 생성 (implements AutoCloseable)
   - Admin.create(config)로 AdminClient 생성
   - 비즈니스 로직 구현
   - close()에서 리소스 정리

3. main()에서 조합
   - Options 파싱
   - Service 생성 (try-with-resources)
   - 결과 출력
   - Exit.exit()
```

### 5.2 Exit 처리

```java
public static void main(String... args) {
    Exit.exit(mainNoExit(args));
}

static int mainNoExit(String... args) {
    try {
        execute(args);
        return 0;
    } catch (Throwable e) {
        System.err.println(e.getMessage());
        return 1;
    }
}
```

**왜 `mainNoExit`를 분리하는가?**

`Exit.exit()`는 JVM을 종료시키므로 테스트에서 호출하면 테스트 프로세스가 죽는다.
`mainNoExit`를 분리하면 테스트에서는 이 메서드를 호출하고 반환 코드만 검증할 수 있다.
`Exit.exit()`도 테스트에서 오버라이드 가능하도록 설계되어 있다.

---

## 6. 핵심 도구별 기능 요약

### 6.1 주요 CLI 도구

| 도구 | 클래스 | 핵심 기능 |
|------|--------|----------|
| kafka-groups | GroupsCommand | 그룹 목록, 타입별 필터링 |
| kafka-get-offsets | GetOffsetShell | 파티션별 최신/최초 오프셋 조회 |
| kafka-dump-log | DumpLogSegments | 로그 세그먼트 바이너리 분석 |
| kafka-cluster | ClusterTool | 클러스터 ID, 비활성화 |
| kafka-features | FeatureCommand | 기능 플래그 조회/업그레이드/다운그레이드 |
| kafka-leader-election | LeaderElectionCommand | 선호/비선호 리더 선출 |
| kafka-log-dirs | LogDirsCommand | 로그 디렉토리 용량/파티션 정보 |
| kafka-metadata-quorum | MetadataQuorumCommand | KRaft 쿼럼 상태 |
| kafka-delegation-tokens | DelegationTokenCommand | 위임 토큰 CRUD |
| kafka-broker-api-versions | BrokerApiVersionsCommand | 브로커 지원 API 버전 |

### 6.2 GetOffsetShell 동작 원리

```java
// tools/src/main/java/org/apache/kafka/tools/GetOffsetShell.java
// --time 옵션으로 특정 시점의 오프셋 조회
// -1: latest, -2: earliest, -3: max-timestamp
// 양수: 해당 타임스탬프 이후 첫 오프셋
```

### 6.3 DumpLogSegments 동작 원리

```java
// tools/src/main/java/org/apache/kafka/tools/DumpLogSegments.java
// .log 파일의 RecordBatch를 파싱하여 사람이 읽을 수 있는 형태로 출력
// --deep-iteration: 각 레코드까지 출력
// --verify-index-only: 인덱스 무결성만 검증
```

---

## 7. 셸 노드 시스템

### 7.1 메타데이터를 파일시스템으로 매핑

```
/                           # 루트
├── brokers/                # 브로커 목록
│   ├── 0                   # 브로커 0 상세정보
│   ├── 1
│   └── 2
├── topics/                 # 토픽 목록
│   ├── my-topic/           # 토픽 디렉토리
│   │   ├── 0               # 파티션 0 (리더, ISR, 레플리카 등)
│   │   ├── 1
│   │   └── 2
│   └── __consumer_offsets/
├── features/               # 기능 플래그
│   ├── metadata.version
│   └── kraft.version
├── client-quotas/          # 클라이언트 쿼터
└── acls/                   # ACL 규칙
```

### 7.2 노드 구현

```
shell/src/main/java/org/apache/kafka/shell/node/
├── MetadataNode.java          # 노드 인터페이스
├── RootShellNode.java         # 루트 "/" 노드
├── BrokerNode.java            # /brokers/{id}
├── TopicNode.java             # /topics/{name}
├── PartitionNode.java         # /topics/{name}/{partition}
└── ...
```

각 노드는 `ls`, `cat`, `cd` 등의 명령어에 응답할 수 있다.

---

## 8. 보안과 인증

### 8.1 CLI 도구의 인증 설정

```properties
# command-config.properties
security.protocol=SASL_SSL
sasl.mechanism=PLAIN
sasl.jaas.config=org.apache.kafka.common.security.plain.PlainLoginModule \
  required username="admin" password="secret";
ssl.truststore.location=/path/to/truststore.jks
ssl.truststore.password=truststore-pass
```

```bash
kafka-groups.sh --bootstrap-server broker:9093 \
  --command-config command-config.properties \
  --list
```

### 8.2 AdminClient 설정 전파

```java
Properties config = opts.commandConfig();
config.put(AdminClientConfig.BOOTSTRAP_SERVERS_CONFIG,
           opts.bootstrapServer());
// commandConfig의 모든 프로퍼티가 AdminClient에 전달됨
// → SASL/SSL 설정이 자동으로 적용됨
```

---

## 9. 실행 흐름 시퀀스

### 9.1 kafka-groups --list

```
사용자                 GroupsCommand              AdminClient            Broker
  │                        │                         │                     │
  │ --list --bootstrap-    │                         │                     │
  │    server broker:9092  │                         │                     │
  │───────────────────────▶│                         │                     │
  │                        │ parse options            │                     │
  │                        │────────────┐             │                     │
  │                        │◀───────────┘             │                     │
  │                        │                         │                     │
  │                        │ Admin.create(props)      │                     │
  │                        │────────────────────────▶│                     │
  │                        │                         │                     │
  │                        │ listGroups().all()       │                     │
  │                        │────────────────────────▶│ ListGroupsRequest   │
  │                        │                         │────────────────────▶│
  │                        │                         │ ListGroupsResponse  │
  │                        │                         │◀────────────────────│
  │                        │◀────────────────────────│                     │
  │                        │                         │                     │
  │                        │ combinedFilter()         │                     │
  │                        │ printGroupDetails()      │                     │
  │◀───────────────────────│                         │                     │
  │ GROUP  TYPE  PROTOCOL  │                         │                     │
  │ my-grp CONSUMER ...    │                         │                     │
```

---

## 10. 정리

### 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| Command-Service 분리 | 옵션 파싱과 비즈니스 로직 분리 |
| AutoCloseable | 리소스 자동 정리 (AdminClient, 터미널) |
| mainNoExit 패턴 | 테스트 가능한 진입점 |
| 필터 합성 | 다중 필터 조건의 직관적 조합 |
| 파일시스템 메타포 | 메타데이터를 디렉토리 트리로 탐색 |
| 듀얼 모드 | 대화형 + 비대화형 지원 |

### CLI 도구 생태계의 가치

Kafka CLI 도구는 단순한 래퍼가 아니다. AdminClient API 위에 구축된
도메인 특화 인터페이스로, 복잡한 관리 작업을 직관적인 명령줄로 추상화한다.
Metadata Shell은 KRaft 시대의 핵심 디버깅 도구로, 메타데이터를 파일시스템처럼
탐색할 수 있게 해준다.

---

*소스 참조: tools/src/main/java/org/apache/kafka/tools/, shell/src/main/java/org/apache/kafka/shell/, examples/src/main/java/kafka/examples/*
