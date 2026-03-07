# Apache Kafka 코드 구조

## 1. 프로젝트 구조 개요

Kafka는 **Gradle 멀티프로젝트** 빌드 시스템으로, 30+ 서브프로젝트가 `settings.gradle`에 정의되어 있다.

```
kafka/
├── bin/                    # 실행 스크립트 (kafka-server-start.sh 등)
├── clients/                # [Java] 프로듀서, 컨슈머, AdminClient, 프로토콜
├── connect/                # [Java] Kafka Connect 프레임워크
│   ├── api/               #   커넥터/태스크 인터페이스
│   ├── runtime/           #   Worker, Herder, 오프셋 저장
│   ├── file/              #   FileSource/SinkConnector
│   ├── json/              #   JSON 컨버터
│   ├── mirror/            #   MirrorMaker 2.0
│   ├── mirror-client/     #   미러 클라이언트 라이브러리
│   ├── transforms/        #   SMT (Single Message Transforms)
│   └── test-plugins/      #   테스트용 플러그인
├── coordinator-common/     # [Java] 코디네이터 공통 런타임
├── core/                   # [Scala] 브로커 핵심 (KafkaApis, SocketServer 등)
├── docker/                 # Docker 이미지 빌드
├── docs/                   # 공식 문서 소스
├── examples/               # 예제 코드
├── generator/              # [Java] 프로토콜 메시지 코드 생성기
├── gradle/                 # Gradle 설정, 의존성 정의
├── group-coordinator/      # [Java] 그룹 코디네이터 (KIP-848 포함)
├── jmh-benchmarks/         # JMH 마이크로벤치마크
├── metadata/               # [Java] KRaft 메타데이터, QuorumController
├── raft/                   # [Java] Raft 합의 프로토콜 구현
├── server/                 # [Java] 서버 공통 (네트워크, 핸들러)
├── server-common/          # [Java] 서버/클라이언트 공유 유틸
├── share-coordinator/      # [Java] Share 그룹 코디네이터
├── shell/                  # [Java] 대화형 셸
├── storage/                # [Java] 로그 스토리지 엔진
│   ├── api/               #   스토리지 API 인터페이스
│   └── src/               #   UnifiedLog, LogSegment, Index 등
├── streams/                # [Java] Kafka Streams
│   ├── src/               #   KafkaStreams, Topology, StateStore
│   ├── streams-scala/     #   Scala DSL 래퍼
│   ├── test-utils/        #   테스트 유틸
│   ├── integration-tests/ #   통합 테스트
│   └── examples/          #   Streams 예제
├── test-common/            # 테스트 공통 유틸
├── tools/                  # [Java] CLI 도구 (kafka-console-producer 등)
├── transaction-coordinator/# [Java] 트랜잭션 코디네이터
├── trogdor/                # 부하 테스트 프레임워크
├── config/                 # 기본 설정 파일 (server.properties)
├── build.gradle            # 루트 빌드 스크립트
└── settings.gradle         # 서브프로젝트 정의
```

## 2. 핵심 모듈 상세

### 2.1 clients/ — 클라이언트 라이브러리

```
clients/src/main/java/org/apache/kafka/
├── clients/
│   ├── producer/                 # KafkaProducer, RecordAccumulator, Sender
│   │   ├── KafkaProducer.java   #   핵심 프로듀서 구현
│   │   └── internals/           #   RecordAccumulator, BuiltInPartitioner
│   ├── consumer/                 # KafkaConsumer, Fetcher, SubscriptionState
│   │   ├── KafkaConsumer.java   #   핵심 컨슈머 구현
│   │   └── internals/           #   Fetcher, ConsumerCoordinator
│   ├── admin/                    # AdminClient, 토픽/ACL 관리
│   ├── NetworkClient.java       #   네트워크 클라이언트 기반 클래스
│   └── MetadataUpdater.java     #   메타데이터 갱신
├── common/
│   ├── protocol/                 # ApiKeys, RequestHeader, 프로토콜 정의
│   ├── record/                   # Record, RecordBatch, MemoryRecords
│   │   └── internal/            #   DefaultRecord, DefaultRecordBatch
│   ├── network/                  # Selector, KafkaChannel, NIO 네트워크
│   ├── security/                 # SASL, SSL, ACL, OAuth
│   ├── requests/                 # ProduceRequest/Response 등 80+ API
│   ├── acl/                      # AclBinding, AclOperation
│   └── resource/                 # ResourceType, ResourcePattern
└── server/
    └── authorizer/               # Authorizer 인터페이스
```

**의존성**: 외부 라이브러리 최소화 — 다른 모든 모듈이 이 모듈에 의존

### 2.2 core/ — 브로커 핵심 (Scala)

```
core/src/main/scala/kafka/
├── Kafka.scala                  # JVM 진입점 (main)
├── server/
│   ├── KafkaRaftServer.scala    # KRaft 모드 서버
│   ├── BrokerServer.scala       # 브로커 서버 (24단계 초기화)
│   ├── ControllerServer.scala   # 컨트롤러 서버
│   ├── SharedServer.scala       # 브로커/컨트롤러 공유 리소스
│   ├── KafkaApis.scala          # 80+ API 요청 라우터
│   ├── KafkaConfig.scala        # 브로커 설정 정의
│   ├── ReplicaManager.scala     # 파티션 복제 관리
│   └── ReplicaFetcherThread.scala # 팔로워 복제 스레드
├── network/
│   ├── SocketServer.scala       # Acceptor + Processor (NIO)
│   └── RequestChannel.scala     # 요청/응답 큐
├── cluster/
│   └── Partition.scala          # 파티션 상태, ISR 관리
├── coordinator/
│   └── transaction/             # TransactionCoordinator, StateManager
├── raft/
│   └── KafkaRaftManager.scala   # Raft 매니저 래퍼
└── log/
    └── remote/                  # 원격 로그 매니저 래퍼
```

### 2.3 storage/ — 로그 스토리지

```
storage/src/main/java/org/apache/kafka/storage/internals/log/
├── UnifiedLog.java              # 로컬+원격 통합 로그
├── LocalLog.java                # 로컬 세그먼트 관리
├── LogSegment.java              # 단일 세그먼트 (데이터+인덱스)
├── LogSegments.java             # 세그먼트 컬렉션 (ConcurrentNavigableMap)
├── LogConfig.java               # 로그 설정 (retention, segment 등)
├── LogFileUtils.java            # 파일명 유틸 (20자리 패딩)
├── OffsetIndex.java             # 오프셋→파일위치 인덱스
├── TimeIndex.java               # 타임스탬프→오프셋 인덱스
├── TransactionIndex.java        # 중단 트랜잭션 인덱스
├── LogCleaner.java              # 로그 컴팩션 코디네이터
├── Cleaner.java                 # 실제 컴팩션 로직
├── CleanerConfig.java           # 컴팩션 설정
└── LogManager.java              # 로그 디렉토리 관리
```

### 2.4 raft/ — KRaft 합의

```
raft/src/main/java/org/apache/kafka/raft/
├── KafkaRaftClient.java         # Raft 클라이언트 (핵심 상태 머신)
├── QuorumState.java             # 쿼럼 상태 관리
├── LeaderState.java             # 리더 상태 (HW 계산)
├── FollowerState.java           # 팔로워 상태
├── CandidateState.java          # 후보 상태
├── ElectionState.java           # 선거 상태 영속화
├── VoterSet.java                # 투표자 집합
├── RaftLog.java                 # Raft 로그 인터페이스
└── ReplicatedLog.java           # 복제 로그 구현
```

### 2.5 metadata/ — 메타데이터 관리

```
metadata/src/main/java/org/apache/kafka/
├── controller/
│   └── QuorumController.java    # KRaft 컨트롤러 상태 머신
├── metadata/
│   ├── PartitionRegistration.java # 파티션 메타데이터
│   ├── LeaderAndIsr.java        # 리더/ISR 상태
│   ├── KRaftMetadataCache.java  # 브로커 측 메타데이터 캐시
│   └── Replicas.java            # 복제본 유틸
└── image/
    └── MetadataImage.java       # 메타데이터 스냅샷 이미지
```

### 2.6 group-coordinator/ — 그룹 코디네이터

```
group-coordinator/src/main/java/org/apache/kafka/coordinator/group/
├── GroupCoordinator.java        # 코디네이터 인터페이스
├── GroupCoordinatorService.java # 서비스 구현
├── GroupCoordinatorShard.java   # 파티션별 상태 머신
├── GroupMetadataManager.java    # 그룹 로직 (6000+ 줄)
├── OffsetMetadataManager.java   # 오프셋 관리
├── classic/                     # ClassicGroup (기존 프로토콜)
├── modern/
│   ├── consumer/                # ConsumerGroup (KIP-848)
│   └── share/                   # ShareGroup (KIP-932)
└── assignor/                    # RangeAssignor, UniformAssignor
```

## 3. 빌드 시스템

### 3.1 Gradle 설정

```
build.gradle          ← 루트 빌드 (플러그인, 공통 설정)
settings.gradle       ← 서브프로젝트 정의 (30+)
gradle/
├── dependencies.gradle ← 의존성 버전 관리
├── resources/          ← 리소스 파일
└── wrapper/            ← Gradle Wrapper
```

**주요 빌드 명령**:

| 명령 | 용도 |
|------|------|
| `./gradlew jar` | JAR 빌드 |
| `./gradlew test` | 전체 테스트 |
| `./gradlew unitTest` | 단위 테스트만 |
| `./gradlew integrationTest` | 통합 테스트만 |
| `./gradlew processMessages` | 프로토콜 메시지 코드 생성 |
| `./gradlew clean releaseTarGz` | 릴리즈 타르볼 |
| `./gradlew checkstyleMain spotlessCheck` | 코드 품질 검사 |
| `./gradlew spotbugsMain` | 정적 분석 |

### 3.2 코드 생성

```
clients/src/main/resources/common/message/
├── ProduceRequest.json         ← 프로토콜 스키마 정의
├── ProduceResponse.json
├── FetchRequest.json
├── ...                         ← 80+ API 스키마
└── README.md                   ← 메시지 정의 가이드

generator/src/main/java/
└── MessageGenerator.java       ← JSON → Java 코드 생성
```

`./gradlew processMessages` 실행 시 JSON 스키마에서 Java 직렬화/역직렬화 코드가 자동 생성된다.

### 3.3 Java/Scala 호환

| 설정 | 값 |
|------|-----|
| Scala 버전 | 2.13 (유일하게 지원) |
| Java 최소 (clients, streams) | 11 |
| Java 최소 (그 외) | 17 |
| Java 빌드/테스트 | 17, 25 |

## 4. 모듈 의존성 그래프

```
                    ┌────────────┐
                    │  clients   │ ◀── 모든 모듈의 기반
                    └─────┬──────┘
                          │
          ┌───────────────┼───────────────┐
          │               │               │
    ┌─────┴──────┐  ┌────┴─────┐   ┌─────┴──────┐
    │server-common│  │  raft    │   │  streams   │
    └─────┬──────┘  └────┬─────┘   └────────────┘
          │               │
    ┌─────┴──────┐  ┌────┴─────┐
    │  storage   │  │ metadata │
    └─────┬──────┘  └────┬─────┘
          │               │
    ┌─────┴──────┐        │
    │  server    │        │
    └─────┬──────┘        │
          │               │
    ┌─────┴───────────────┴──────────────┐
    │              core (Scala)           │
    │  BrokerServer, ControllerServer,   │
    │  KafkaApis, SocketServer           │
    └────────────────────────────────────┘
          │
    ┌─────┴──────────────┐
    │ group-coordinator  │
    │ transaction-coord. │
    │ share-coordinator  │
    │ coordinator-common │
    └────────────────────┘
```

## 5. 프로토콜 메시지 구조

### 5.1 요청/응답 프레임

```
Request Frame:
  ┌────────────────────────────────────┐
  │ Size              (Int32)          │  전체 메시지 크기
  │ RequestHeader                      │
  │   ├─ ApiKey       (Int16)          │  API 유형 (0=PRODUCE 등)
  │   ├─ ApiVersion   (Int16)          │  API 버전
  │   ├─ CorrelationId (Int32)         │  요청-응답 매칭 ID
  │   └─ ClientId     (String)         │  클라이언트 식별자
  │ RequestBody       (varies)         │  API별 본문
  └────────────────────────────────────┘

Response Frame:
  ┌────────────────────────────────────┐
  │ Size              (Int32)          │
  │ ResponseHeader                     │
  │   └─ CorrelationId (Int32)         │
  │ ResponseBody      (varies)         │
  └────────────────────────────────────┘
```

### 5.2 API 유형별 분류

| 분류 | API Keys | 용도 |
|------|----------|------|
| **데이터** | PRODUCE(0), FETCH(1) | 메시지 생산/소비 |
| **메타데이터** | METADATA(3), DESCRIBE_CONFIGS(32) | 클러스터 정보 조회 |
| **그룹** | JOIN_GROUP(11), SYNC_GROUP(14), HEARTBEAT(12) | 컨슈머 그룹 |
| **오프셋** | OFFSET_COMMIT(8), OFFSET_FETCH(9), LIST_OFFSETS(2) | 오프셋 관리 |
| **트랜잭션** | INIT_PRODUCER_ID(22), ADD_PARTITIONS_TO_TXN(24), END_TXN(26) | 트랜잭션 |
| **관리** | CREATE_TOPICS(19), DELETE_TOPICS(20), ALTER_PARTITION(56) | 토픽 관리 |
| **KRaft** | VOTE(52), BEGIN_QUORUM_EPOCH(53), FETCH_SNAPSHOT(59) | Raft 합의 |
| **보안** | SASL_HANDSHAKE(17), SASL_AUTHENTICATE(36) | 인증 |

## 6. 테스트 구조

```
각 모듈/
├── src/
│   ├── main/        ← 프로덕션 코드
│   └── test/        ← 테스트 코드
│       ├── java/    ← 단위/통합 테스트
│       └── resources/
│           └── log4j2.yaml  ← 테스트 로깅 설정
```

| 테스트 유형 | 명령 | 특징 |
|------------|------|------|
| 단위 테스트 | `./gradlew unitTest` | 빠름, 외부 의존 없음 |
| 통합 테스트 | `./gradlew integrationTest` | 실제 브로커 시작, 느림 |
| 시스템 테스트 | `tests/` 디렉토리 | 다중 노드, Docker 기반 |
| 벤치마크 | `jmh-benchmarks/` | JMH 마이크로벤치마크 |
| Flaky 테스트 | `-Pkafka.test.run.flaky=true` | 불안정 테스트 별도 실행 |

## 7. 설정 파일 구조

```
config/
├── server.properties         ← 브로커 기본 설정
├── consumer.properties       ← 컨슈머 예제 설정
├── producer.properties       ← 프로듀서 예제 설정
├── connect-distributed.properties  ← Connect 분산 모드
├── connect-standalone.properties   ← Connect 단독 모드
├── log4j2.yaml              ← 로깅 설정
└── kraft/
    └── controller.properties ← KRaft 컨트롤러 설정
```

## 8. 핵심 파일 경로 요약

| 역할 | 파일 경로 | 언어 |
|------|----------|------|
| JVM 진입점 | `core/src/main/scala/kafka/Kafka.scala` | Scala |
| 브로커 서버 | `core/src/main/scala/kafka/server/BrokerServer.scala` | Scala |
| API 핸들러 | `core/src/main/scala/kafka/server/KafkaApis.scala` | Scala |
| 소켓 서버 | `core/src/main/scala/kafka/network/SocketServer.scala` | Scala |
| 프로듀서 | `clients/src/main/java/.../producer/KafkaProducer.java` | Java |
| 컨슈머 | `clients/src/main/java/.../consumer/KafkaConsumer.java` | Java |
| 로그 세그먼트 | `storage/src/main/java/.../log/LogSegment.java` | Java |
| Raft 클라이언트 | `raft/src/main/java/.../raft/KafkaRaftClient.java` | Java |
| 쿼럼 컨트롤러 | `metadata/src/main/java/.../controller/QuorumController.java` | Java |
| 그룹 코디네이터 | `group-coordinator/src/main/java/.../GroupMetadataManager.java` | Java |
| NIO Selector | `clients/src/main/java/.../network/Selector.java` | Java |
| 프로토콜 스키마 | `clients/src/main/resources/common/message/*.json` | JSON |
