# Apache Kafka 아키텍처

## 1. 개요

Apache Kafka는 분산 이벤트 스트리밍 플랫폼으로, 높은 처리량과 내구성을 갖춘 메시지 브로커 역할을 한다. Kafka 4.0부터 ZooKeeper 의존성을 완전히 제거하고 **KRaft (Kafka Raft)** 모드로 전환했다.

## 2. 전체 아키텍처

```
┌──────────────────────────────────────────────────────────────────────┐
│                         Kafka Cluster                                │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────┐        │
│  │              Metadata Quorum (KRaft)                     │        │
│  │  ┌──────────────┐ ┌──────────────┐ ┌──────────────┐    │        │
│  │  │ Controller 1 │ │ Controller 2 │ │ Controller 3 │    │        │
│  │  │   (Leader)   │ │  (Follower)  │ │  (Follower)  │    │        │
│  │  └──────────────┘ └──────────────┘ └──────────────┘    │        │
│  │         │  __cluster_metadata 토픽 (Raft 합의)           │        │
│  └─────────┼───────────────────────────────────────────────┘        │
│            │ 메타데이터 전파                                          │
│  ┌─────────┴───────────────────────────────────────────────┐        │
│  │                   Data Plane                             │        │
│  │  ┌──────────┐    ┌──────────┐    ┌──────────┐          │        │
│  │  │ Broker 1 │    │ Broker 2 │    │ Broker 3 │          │        │
│  │  │┌────────┐│    │┌────────┐│    │┌────────┐│          │        │
│  │  ││SocketSv││    ││SocketSv││    ││SocketSv││          │        │
│  │  │├────────┤│    │├────────┤│    │├────────┤│          │        │
│  │  ││KafkaApi││    ││KafkaApi││    ││KafkaApi││          │        │
│  │  │├────────┤│    │├────────┤│    │├────────┤│          │        │
│  │  ││Replica ││    ││Replica ││    ││Replica ││          │        │
│  │  ││Manager ││    ││Manager ││    ││Manager ││          │        │
│  │  │├────────┤│    │├────────┤│    │├────────┤│          │        │
│  │  ││  Log   ││    ││  Log   ││    ││  Log   ││          │        │
│  │  ││Manager ││    ││Manager ││    ││Manager ││          │        │
│  │  │└────────┘│    │└────────┘│    │└────────┘│          │        │
│  │  └──────────┘    └──────────┘    └──────────┘          │        │
│  └─────────────────────────────────────────────────────────┘        │
└──────────────────────────────────────────────────────────────────────┘
      ▲          ▲            │           │
      │ Produce  │ Fetch      │ Produce   │ Fetch
┌─────┴──┐ ┌────┴───┐  ┌─────┴──┐  ┌────┴───┐
│Producer│ │Consumer│  │Producer│  │Consumer│
│  App   │ │ Group  │  │  App   │  │ Group  │
└────────┘ └────────┘  └────────┘  └────────┘
```

## 3. KRaft 모드 아키텍처

### 3.1 프로세스 역할 (process.roles)

KRaft 모드에서 Kafka 프로세스는 세 가지 역할을 가질 수 있다:

| 역할 | 설명 | 클래스 |
|------|------|--------|
| `broker` | 데이터 플레인 — 클라이언트 요청 처리, 로그 저장 | `BrokerServer` |
| `controller` | 컨트롤 플레인 — 메타데이터 관리, 리더 선출 | `ControllerServer` |
| `broker,controller` | 두 역할 모두 수행 (소규모 클러스터) | `KafkaRaftServer` |

```properties
# server.properties 예시
process.roles=broker,controller
node.id=1
controller.quorum.voters=1@localhost:9093,2@host2:9093,3@host3:9093
```

### 3.2 ZooKeeper vs KRaft 비교

| 측면 | ZooKeeper 모드 (레거시) | KRaft 모드 |
|------|----------------------|-----------|
| 메타데이터 저장소 | 외부 ZooKeeper 앙상블 | 내부 `__cluster_metadata` 토픽 |
| 컨트롤러 선출 | ZK 임시 노드 경쟁 | Raft 합의 프로토콜 |
| 일관성 모델 | 최종 일관성 (Eventually Consistent) | 강한 일관성 (Strong Consistency) |
| 브로커 등록 | ZK 워치 기반 | `BrokerLifecycleManager` 하트비트 |
| 설정 동적 변경 | ZK 노드 업데이트 | 메타데이터 레코드 기록 |
| 운영 복잡성 | ZK 클러스터 별도 관리 필요 | 단일 시스템으로 통합 |
| 엔트리포인트 | `KafkaServer` (deprecated) | `KafkaRaftServer` |

## 4. 진입점과 초기화 흐름

### 4.1 JVM 진입점

```
kafka-server-start.sh
  └─ kafka-run-class.sh kafka.Kafka
       └─ java -cp [...] kafka.Kafka server.properties
```

**소스 파일**: `core/src/main/scala/kafka/Kafka.scala`

```
Kafka.main(args)
  ├─ 1. getPropsFromArgs(args)     ← server.properties 로드
  ├─ 2. buildServer(props)         ← KafkaRaftServer 생성
  │     └─ KafkaConfig.fromProps()
  │     └─ new KafkaRaftServer(config, Time.SYSTEM)
  ├─ 3. LoggingSignalHandler.register()  ← SIGTERM 핸들러
  ├─ 4. Exit.addShutdownHook()     ← JVM 종료 훅
  ├─ 5. server.startup()           ← 서버 시작
  └─ 6. server.awaitShutdown()     ← 종료 대기 (블로킹)
```

### 4.2 KafkaRaftServer 초기화

**소스 파일**: `core/src/main/scala/kafka/server/KafkaRaftServer.scala`

```
KafkaRaftServer(config, time)
  ├─ initializeLogDirs()           ← 로그 디렉토리, meta.properties 검증
  ├─ Server.initializeMetrics()    ← JMX 메트릭 초기화
  ├─ new SharedServer(...)         ← 브로커/컨트롤러 공유 리소스
  ├─ new BrokerServer(sharedServer)     ← broker 역할이면
  └─ new ControllerServer(sharedServer) ← controller 역할이면

startup():
  ├─ Mx4jLoader.maybeLoad()       ← 모니터링 로드
  ├─ controller.startup()         ← 컨트롤러 먼저 시작 (!)
  ├─ broker.startup()             ← 그 다음 브로커 시작
  └─ info("started")
```

**왜 컨트롤러가 먼저?** 브로커가 시작되면 `BrokerLifecycleManager`가 컨트롤러에 등록 요청을 보내야 하므로, 컨트롤러의 네트워크 엔드포인트가 먼저 준비되어야 한다.

### 4.3 BrokerServer 상세 초기화

**소스 파일**: `core/src/main/scala/kafka/server/BrokerServer.scala`

24단계 순차 초기화 — 각 단계는 이전 단계에 의존:

```
BrokerServer.startup()
  ├─ 1.  SharedServer.startForBroker()     ← Raft 매니저 초기화
  ├─ 2.  QuotaFactory.instantiate()        ← 쿼터 관리자
  ├─ 3.  KafkaScheduler                    ← 백그라운드 태스크 스케줄러
  ├─ 4.  BrokerTopicStats                  ← 토픽별 통계
  ├─ 5.  LogDirFailureChannel              ← 디스크 장애 감지
  ├─ 6.  KRaftMetadataCache                ← 메타데이터 캐시
  ├─ 7.  LogManager                        ← 로그 세그먼트 관리
  ├─ 8.  BrokerLifecycleManager            ← 컨트롤러 등록/하트비트
  ├─ 9.  DelegationTokenCache              ← 인증 토큰
  ├─ 10. CredentialProvider                ← 자격증명
  ├─ 11. NodeToControllerChannelManager    ← 컨트롤러 통신 채널
  ├─ 12. ApiVersionManager                 ← API 버전 관리
  ├─ 13. SocketServer                      ← 네트워크 서버
  ├─ 14. ReplicaManager                    ← 파티션 복제 관리
  ├─ 15. DelegationTokenManager            ← 토큰 관리
  ├─ 16. Authorizer                        ← ACL 인가
  ├─ 17. GroupCoordinator                  ← 컨슈머 그룹
  ├─ 18. TransactionCoordinator            ← 트랜잭션
  ├─ 19. DynamicConfigHandlers             ← 동적 설정
  ├─ 20. BrokerLifecycleManager.start()    ← 하트비트 시작
  ├─ 21. KafkaApis                         ← 요청 핸들러
  ├─ 22. RequestHandlerPool                ← 요청 처리 스레드 풀
  ├─ 23. MetadataPublishers                ← 메타데이터 적용
  └─ 24. SocketServer 활성화               ← 클라이언트 요청 수락 시작
```

**핵심 동기화 포인트 (Future)**:
1. 컨트롤러 쿼럼 Voter 연결 완료
2. 초기 메타데이터 캐치업 (catch-up) 완료
3. 첫 번째 메타데이터 퍼블리시 완료
4. 브로커 언펜싱 (unfencing) 완료
5. Authorizer 초기화 완료
6. SocketServer Acceptor 준비 완료

## 5. 컴포넌트 관계도

```
┌──────────────────────────────────────────────────────────┐
│                     BrokerServer                          │
│                                                          │
│  ┌──────────────┐     ┌──────────────────┐              │
│  │ SocketServer │────▶│  RequestChannel  │              │
│  │  (Acceptor + │     │   (BlockingQueue) │              │
│  │  Processors) │     └────────┬─────────┘              │
│  └──────────────┘              │                         │
│                                ▼                         │
│  ┌──────────────────────────────────────┐               │
│  │        KafkaRequestHandlerPool       │               │
│  │   ┌──────────┐  ┌──────────┐       │               │
│  │   │ Handler1 │  │ Handler2 │ ...   │               │
│  │   └─────┬────┘  └─────┬────┘       │               │
│  └─────────┼──────────────┼────────────┘               │
│            └──────┬───────┘                              │
│                   ▼                                      │
│  ┌──────────────────────────────────┐                   │
│  │           KafkaApis              │                   │
│  │  (80+ API 핸들러 메서드)           │                   │
│  └───┬──────────┬──────────┬────────┘                   │
│      │          │          │                             │
│      ▼          ▼          ▼                             │
│  ┌────────┐ ┌────────┐ ┌─────────────┐                 │
│  │Replica │ │ Group  │ │ Transaction │                 │
│  │Manager │ │Coordin.│ │ Coordinator │                 │
│  └───┬────┘ └────────┘ └─────────────┘                 │
│      │                                                   │
│      ▼                                                   │
│  ┌─────────────────────────────────┐                    │
│  │          LogManager             │                    │
│  │  ┌──────────┐ ┌──────────┐     │                    │
│  │  │UnifiedLog│ │UnifiedLog│ ... │                    │
│  │  │ (P0)     │ │ (P1)     │     │                    │
│  │  └──────────┘ └──────────┘     │                    │
│  └─────────────────────────────────┘                    │
└──────────────────────────────────────────────────────────┘
```

## 6. 데이터 플레인 vs 컨트롤 플레인

### 데이터 플레인 (Broker)

데이터 플레인은 실제 메시지의 생산과 소비를 담당한다:

| 컴포넌트 | 역할 |
|----------|------|
| `SocketServer` | 클라이언트 TCP 연결 수락, NIO 기반 I/O 처리 |
| `KafkaApis` | 80+ 종류의 API 요청을 적절한 핸들러로 라우팅 |
| `ReplicaManager` | 파티션 리더/팔로워 관리, ISR 유지, 복제 |
| `LogManager` | 로그 세그먼트 생성/삭제/압축, 디스크 I/O |
| `GroupCoordinator` | 컨슈머 그룹 관리, 리밸런스, 오프셋 저장 |
| `TransactionCoordinator` | 트랜잭션 상태 관리, 2PC 프로토콜 |

### 컨트롤 플레인 (Controller)

컨트롤 플레인은 클러스터 메타데이터를 관리한다:

| 컴포넌트 | 역할 |
|----------|------|
| `QuorumController` | 모든 메타데이터 변경의 중앙 처리 |
| `KafkaRaftClient` | Raft 합의 프로토콜 구현 |
| `MetadataLoader` | 메타데이터 스냅샷/델타 로드 |
| `ControllerApis` | 컨트롤러 전용 API 처리 |

### 메타데이터 흐름

```
브로커 요청 (예: AlterPartition)
  │
  ▼
QuorumController (리더 컨트롤러)
  │
  ├─ 메타데이터 레코드 생성 (PartitionChangeRecord 등)
  ├─ __cluster_metadata 토픽에 기록
  ├─ Raft 합의로 팔로워에 복제
  │
  ▼
MetadataPublisher (각 브로커)
  │
  ├─ 메타데이터 캐시 업데이트
  ├─ 동적 설정 적용
  └─ ISR/리더 변경 반영
```

## 7. 토픽과 파티션 아키텍처

### 파티션 복제 모델

```
Topic: orders, Replication Factor: 3

Partition 0:
  Broker 1 [Leader]  ──┐
  Broker 2 [Follower]──┤── ISR (In-Sync Replicas)
  Broker 3 [Follower]──┘

Partition 1:
  Broker 2 [Leader]  ──┐
  Broker 3 [Follower]──┤── ISR
  Broker 1 [Follower]──┘

Partition 2:
  Broker 3 [Leader]  ──┐
  Broker 1 [Follower]──┤── ISR
  Broker 2 [Follower]──┘
```

- **리더**: 모든 읽기/쓰기 요청 처리
- **팔로워**: 리더로부터 비동기 복제
- **ISR**: 리더와 동기화된 복제본 집합 (High Watermark 기준)

### 파티션 메타데이터

`PartitionRegistration` 구조 (`metadata/src/main/java/.../PartitionRegistration.java`):

```
PartitionRegistration:
  ├─ replicas[]        ← 모든 복제본 브로커 ID
  ├─ isr[]             ← 동기화된 복제본 (ISR)
  ├─ elr[]             ← 선출 가능 리더 복제본
  ├─ leader            ← 현재 리더 브로커 ID
  ├─ leaderEpoch       ← 리더 에포크 (단조 증가)
  └─ partitionEpoch    ← 파티션 에포크 (ISR 변경 시 증가)
```

## 8. 클라이언트 에코시스템

```
┌─────────────────────────────────────────────────────────┐
│                    Client Ecosystem                      │
│                                                         │
│  ┌───────────┐  ┌───────────┐  ┌────────────────────┐ │
│  │ Producer  │  │ Consumer  │  │   Admin Client     │ │
│  │ ┌───────┐ │  │ ┌───────┐ │  │                    │ │
│  │ │Sender │ │  │ │Fetcher│ │  │  토픽/파티션 관리    │ │
│  │ │Thread │ │  │ │       │ │  │  설정 변경          │ │
│  │ └───────┘ │  │ └───────┘ │  │  ACL 관리          │ │
│  │ ┌───────┐ │  │ ┌───────┐ │  └────────────────────┘ │
│  │ │Record │ │  │ │Coord. │ │                          │
│  │ │Accum. │ │  │ │       │ │  ┌────────────────────┐ │
│  │ └───────┘ │  │ └───────┘ │  │  Kafka Streams     │ │
│  └───────────┘  └───────────┘  │  ┌──────────────┐  │ │
│                                │  │StreamThread[] │  │ │
│  ┌─────────────────────────┐  │  │StateStore     │  │ │
│  │     Kafka Connect       │  │  │Topology       │  │ │
│  │  ┌───────┐ ┌───────┐  │  │  └──────────────┘  │ │
│  │  │Source │ │ Sink  │  │  └────────────────────┘ │
│  │  │Task[] │ │Task[] │  │                          │
│  │  └───────┘ └───────┘  │                          │
│  └─────────────────────────┘                          │
└─────────────────────────────────────────────────────────┘
```

## 9. 배포 토폴로지

### 프로덕션 권장 구성

```
┌─────────────────────────────────────┐
│         Controller Quorum           │
│  (process.roles=controller)         │
│  ┌────┐  ┌────┐  ┌────┐           │
│  │ C1 │  │ C2 │  │ C3 │  (3 or 5) │
│  └────┘  └────┘  └────┘           │
└─────────────┬───────────────────────┘
              │ 메타데이터 복제
┌─────────────┴───────────────────────┐
│          Broker Fleet               │
│  (process.roles=broker)             │
│  ┌────┐ ┌────┐ ┌────┐ ┌────┐      │
│  │ B1 │ │ B2 │ │ B3 │ │ B4 │ ...  │
│  └────┘ └────┘ └────┘ └────┘      │
└─────────────────────────────────────┘
```

- **컨트롤러 전용 노드**: 3~5대, 메타데이터 관리에 집중
- **브로커 전용 노드**: 필요에 따라 수십~수백 대 확장
- **분리 이유**: 컨트롤러의 메타데이터 처리가 브로커의 데이터 처리에 영향 없음

## 10. 핵심 소스 파일 경로

| 컴포넌트 | 파일 경로 |
|----------|----------|
| JVM 진입점 | `core/src/main/scala/kafka/Kafka.scala` |
| KRaft 서버 | `core/src/main/scala/kafka/server/KafkaRaftServer.scala` |
| 브로커 서버 | `core/src/main/scala/kafka/server/BrokerServer.scala` |
| 컨트롤러 서버 | `core/src/main/scala/kafka/server/ControllerServer.scala` |
| 공유 서버 | `core/src/main/scala/kafka/server/SharedServer.scala` |
| API 핸들러 | `core/src/main/scala/kafka/server/KafkaApis.scala` |
| 소켓 서버 | `core/src/main/scala/kafka/network/SocketServer.scala` |
| 복제 관리자 | `core/src/main/scala/kafka/server/ReplicaManager.scala` |
| Raft 클라이언트 | `raft/src/main/java/.../raft/KafkaRaftClient.java` |
| 쿼럼 컨트롤러 | `metadata/src/main/java/.../controller/QuorumController.java` |
| 설정 | `core/src/main/scala/kafka/server/KafkaConfig.scala` |
