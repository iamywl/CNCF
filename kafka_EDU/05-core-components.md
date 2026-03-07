# Apache Kafka 핵심 컴포넌트

## 1. 개요

Kafka 브로커는 다수의 핵심 컴포넌트가 협력하여 동작한다. 각 컴포넌트의 역할, 내부 구조, 상호 작용을 분석한다.

## 2. SocketServer — 네트워크 계층

### 2.1 아키텍처

**소스**: `core/src/main/scala/kafka/network/SocketServer.scala`

```
SocketServer
├── DataPlaneAcceptor (클라이언트 요청용)
│   ├── Processor[0] ── NIO Selector
│   ├── Processor[1] ── NIO Selector
│   └── Processor[N] ── NIO Selector
├── ControlPlaneAcceptor (컨트롤러 요청용, 선택적)
│   └── Processor[0] ── NIO Selector
└── RequestChannel
    ├── requestQueue (ArrayBlockingQueue)
    └── responseQueues[] (per-Processor)
```

### 2.2 Acceptor

- **역할**: ServerSocketChannel에서 새 TCP 연결을 수락
- **동작**: `nioSelector.select(500)` → `key.isAcceptable` → 라운드로빈으로 Processor에 할당
- **핵심 메서드**: `acceptNewConnections()`, `accept()`

```
Acceptor 스레드:
  loop:
    nioSelector.select(500ms)
    for key in selectedKeys:
      if key.isAcceptable:
        socketChannel = serverChannel.accept()
        connectionQuotas.inc()
        assign(socketChannel, nextProcessor)
```

### 2.3 Processor

- **역할**: NIO Selector로 다수의 연결을 멀티플렉싱, 요청 파싱, 응답 전송
- **스레드 수**: `num.network.threads` 설정 (기본 3)

```
Processor 스레드 메인 루프 (SocketServer.scala:889):
  while (shouldRun):
    configureNewConnections()    ← 새 연결 등록
    processNewResponses()        ← 응답 큐에서 꺼내 전송 등록
    poll()                       ← NIO selector.poll()
    processCompletedReceives()   ← 수신 완료된 요청 파싱 → RequestChannel
    processCompletedSends()      ← 전송 완료된 응답 처리
    processDisconnected()        ← 연결 끊김 처리
    closeExcessConnections()     ← 쿼터 초과 연결 정리
```

### 2.4 채널 뮤팅 (Flow Control)

```
채널 뮤트 상태:
  NOT_MUTED          → 정상 읽기/쓰기
  MUTED              → 읽기 일시 중지
  MUTED_AND_RESPONSE_PENDING → 핸들러 응답 대기
  MUTED_AND_THROTTLED → 쿼터 제한 중
```

**왜 뮤팅인가?** 한 연결에서 요청을 받으면 해당 연결을 뮤트하여 다음 요청을 받지 않는다. 핸들러가 응답을 보낸 후에야 언뮤트한다. 이는 요청 순서 보장과 메모리 압력 제어를 위함이다.

## 3. KafkaApis — 요청 라우터

### 3.1 구조

**소스**: `core/src/main/scala/kafka/server/KafkaApis.scala`

```scala
override def handle(request: RequestChannel.Request, requestLocal: RequestLocal): Unit = {
  request.header.apiKey match {
    case ApiKeys.PRODUCE            => handleProduceRequest(request, requestLocal)
    case ApiKeys.FETCH              => handleFetchRequest(request)
    case ApiKeys.METADATA           => handleTopicMetadataRequest(request)
    case ApiKeys.OFFSET_COMMIT      => handleOffsetCommitRequest(request, requestLocal)
    case ApiKeys.JOIN_GROUP         => handleJoinGroupRequest(request, requestLocal)
    // ... 80+ API 핸들러
  }
}
```

### 3.2 요청 처리 파이프라인

```
RequestChannel.Request
  ├── processor: Int            ← 어느 Processor에서 왔는지
  ├── context: RequestContext    ← 헤더, 연결 정보
  ├── startTimeNanos: Long      ← 요청 수신 시각
  ├── memoryPool: MemoryPool    ← 버퍼 풀
  └── body: AbstractRequest     ← 역직렬화된 요청 본문
```

**처리 순서**:
1. `Processor` → `RequestChannel.sendRequest(req)` → `requestQueue`에 삽입
2. `KafkaRequestHandler.run()` → `requestChannel.receiveRequest(300ms)` → 블로킹 대기
3. `KafkaApis.handle(request)` → API 키에 따라 핸들러 호출
4. 핸들러 → `requestChannel.sendResponse()` → `Processor.responseQueue`에 삽입

## 4. ReplicaManager — 복제 관리

### 4.1 역할

**소스**: `core/src/main/scala/kafka/server/ReplicaManager.scala`

- 파티션의 리더/팔로워 역할 관리
- 메시지 쓰기 (로컬 로그에 append)
- 팔로워 복제 스레드 관리
- ISR (In-Sync Replicas) 유지
- High Watermark 계산

### 4.2 핵심 메서드

| 메서드 | 역할 |
|--------|------|
| `appendRecords()` | Produce 요청 처리 → 로컬 로그에 기록 |
| `readFromLog()` | Fetch 요청 처리 → 로그에서 읽기 |
| `becomeLeaderOrFollower()` | 리더/팔로워 전환 |
| `maybeShrinkIsr()` | ISR 축소 검토 (주기적) |

### 4.3 쓰기 경로 상세

```
ReplicaManager.appendRecords(timeout, acks, records)
  │
  ├─ for each (partition, records) in entriesPerPartition:
  │   ├─ Partition.appendRecordsToLeader(records, requiredAcks)
  │   │   └─ UnifiedLog.appendAsLeader(records, leaderEpoch)
  │   │       ├─ analyzeAndValidateRecords()
  │   │       ├─ trimInvalidBytes()
  │   │       ├─ synchronized(lock):
  │   │       │   ├─ LogValidator.validateMessagesAndAssignOffsets()
  │   │       │   └─ LocalLog.append(records)
  │   │       │       └─ LogSegment.append(largestOffset, records)
  │   │       │           ├─ FileRecords.append(records)  ← 디스크 쓰기
  │   │       │           ├─ OffsetIndex.append()          ← 인덱스 업데이트
  │   │       │           └─ TimeIndex.maybeAppend()       ← 시간 인덱스
  │   │       └─ updateHighWatermark()
  │   │
  │   └─ 활성 세그먼트 롤링 체크 (크기/시간 초과 시)
  │
  ├─ if acks == 0: 즉시 응답
  ├─ if acks == 1: 리더 쓰기 완료 시 응답
  └─ if acks == -1 (all): DelayedProduce 생성 → ISR 전체 ack 대기
```

### 4.4 ISR 관리

```
ISR 확장 (maybeExpandIsr):
  조건: 팔로워 LEO >= 리더 HW
  처리: AlterPartition 요청 → 컨트롤러가 ISR 업데이트

ISR 축소 (maybeShrinkIsr):
  조건: 팔로워가 replicaLagTimeMaxMs 동안 Fetch 안 함
  주기: replicaLagTimeMaxMs / 2 마다 체크
  처리: AlterPartition 요청 → 컨트롤러가 ISR 업데이트
```

## 5. LogManager — 로그 관리

### 5.1 역할

**소스**: `storage/src/main/java/org/apache/kafka/storage/internals/log/`

- 파티션별 `UnifiedLog` 인스턴스 관리
- 로그 디렉토리 관리 (다중 디스크 지원)
- 로그 보존/삭제/압축 스케줄링
- 체크포인트 파일 관리

### 5.2 로그 보존 정책

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `log.retention.hours` | 168 (7일) | 시간 기반 보존 |
| `log.retention.bytes` | -1 (무제한) | 크기 기반 보존 |
| `log.segment.bytes` | 1GB | 세그먼트 파일 크기 |
| `log.segment.ms` | 7일 | 세그먼트 롤링 주기 |
| `log.cleanup.policy` | delete | delete 또는 compact |

### 5.3 로그 컴팩션

```
로그 컴팩션 프로세스 (Cleaner.java):

1. 더티 영역 식별:
   [Clean]──[Dirty]──[Uncleanable(활성)]
     │         │          │
     완료      대상       보호

2. 키 중복 맵 생성:
   OffsetMap: key → last_offset
   (MD5 해시 사용으로 메모리 효율적)

3. 세그먼트 재작성:
   for segment in dirty:
     for record in segment:
       if record.offset == offsetMap[record.key]:
         keep(record)   ← 최신 버전 유지
       else:
         skip(record)   ← 이전 버전 제거

4. .cleaned → 원본 교체 → 이전 파일 삭제
```

## 6. Partition — 파티션 상태 관리

### 6.1 역할

**소스**: `core/src/main/scala/kafka/cluster/Partition.scala`

- 파티션 단위의 리더/팔로워 상태 관리
- ISR 확장/축소 판단
- High Watermark 계산 및 전파
- 로그 읽기/쓰기 위임

### 6.2 High Watermark 계산

```
maybeIncrementLeaderHW():
  ISR 멤버들의 LEO (Log End Offset) 수집
  HW = ISR 멤버 중 가장 낮은 LEO

  예시:
    ISR = [Broker1(LEO=100), Broker2(LEO=95), Broker3(LEO=98)]
    HW = min(100, 95, 98) = 95

  → 오프셋 0~94까지만 커밋됨 (컨슈머에게 노출)
```

**High Watermark의 의미**:
- HW 이하의 데이터만 컨슈머에게 보임
- HW 이상의 데이터는 아직 ISR 전체에 복제되지 않음
- 리더 장애 시 HW까지만 보존 보장

## 7. QuorumController — KRaft 컨트롤러

### 7.1 역할

**소스**: `metadata/src/main/java/org/apache/kafka/controller/QuorumController.java`

- 모든 클러스터 메타데이터 변경 처리
- 토픽 생성/삭제, 파티션 할당
- ISR 변경 승인
- 브로커 등록/펜싱
- 동적 설정 관리

### 7.2 메타데이터 레코드 유형

| 레코드 | 용도 |
|--------|------|
| `TopicRecord` | 토픽 생성 |
| `PartitionRecord` | 파티션 생성 |
| `PartitionChangeRecord` | ISR/리더 변경 |
| `RegisterBrokerRecord` | 브로커 등록 |
| `FenceBrokerRecord` | 브로커 펜싱 |
| `UnfenceBrokerRecord` | 브로커 언펜싱 |
| `ConfigRecord` | 동적 설정 변경 |
| `FeatureLevelRecord` | 기능 버전 업그레이드 |

### 7.3 메타데이터 전파 흐름

```
1. 클라이언트/브로커 → 관리 요청 (예: CreateTopics)
   │
2. ControllerApis → QuorumController
   │
3. QuorumController: 상태 검증 + 레코드 생성
   │
4. __cluster_metadata 토픽에 기록 (Raft 합의)
   │
5. Raft 팔로워에 복제 → HW 진행
   │
6. MetadataLoader → MetadataPublisher (각 브로커)
   │
7. KRaftMetadataCache 업데이트
   │
8. 브로커 동작에 반영 (ISR, 리더 등)
```

## 8. GroupCoordinator — 그룹 관리

### 8.1 구조

**소스**: `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/`

```
GroupCoordinatorService
  └── GroupCoordinatorShard[] (파티션별)
       ├── GroupMetadataManager  ← 그룹 상태
       └── OffsetMetadataManager ← 오프셋 상태
```

### 8.2 그룹 상태 머신 (Classic)

```
           JoinGroup
EMPTY ──────────────▶ PREPARING_REBALANCE
  ▲                        │
  │ 오프셋 만료              │ 모든 멤버 참여
  │                        ▼
DEAD ◀── 빈 그룹 ── COMPLETING_REBALANCE
                           │
                           │ SyncGroup (리더)
                           ▼
                        STABLE
                           │
                           │ 멤버 이탈/참여
                           ▼
                   PREPARING_REBALANCE
```

### 8.3 오프셋 저장

`__consumer_offsets` 내부 토픽 (50 파티션, 기본):
- **키**: `(groupId, topic, partition)`
- **값**: `(offset, leaderEpoch, metadata, timestamp)`
- **파티션 결정**: `hash(groupId) % numPartitions`
- **컴팩션**: `cleanup.policy=compact` — 최신 오프셋만 유지

## 9. TransactionCoordinator — 트랜잭션 관리

### 9.1 역할

**소스**: `core/src/main/scala/kafka/coordinator/transaction/TransactionCoordinator.scala`

- 프로듀서 ID/에포크 할당
- 트랜잭션 상태 관리
- 2PC (Two-Phase Commit) 프로토콜 실행
- `__transaction_state` 토픽에 상태 영속화

### 9.2 트랜잭션 상태 머신

```
         InitProdId
(없음) ──────────▶ EMPTY
                     │
                     │ AddPartitions
                     ▼
                  ONGOING
                  ╱       ╲
         EndTxn  ╱         ╲  EndTxn
        (commit)╱           ╲(abort)
               ▼             ▼
        PREPARE_COMMIT   PREPARE_ABORT
               │             │
               │ markers     │ markers
               ▼             ▼
        COMPLETE_COMMIT  COMPLETE_ABORT
               │             │
               └──────┬──────┘
                      │ 만료
                      ▼
                    DEAD
```

## 10. NIO Selector — 비동기 I/O

### 10.1 구조

**소스**: `clients/src/main/java/org/apache/kafka/common/network/Selector.java`

```
Selector:
  ├── nioSelector: java.nio.channels.Selector
  ├── channels: Map<String, KafkaChannel>
  ├── completedSends: ArrayList<NetworkSend>
  ├── completedReceives: LinkedHashMap<String, NetworkReceive>
  ├── memoryPool: MemoryPool
  └── metricsPerConnection: Map
```

### 10.2 poll() 루프

```
Selector.poll(timeout):
  1. select(timeout)           ← Java NIO select
  2. for key in readyKeys:
     if connectable → channel.finishConnect()
     if readable    → attemptRead(channel)
                        └─ channel.read() → NetworkReceive
                        └─ completedReceives에 추가
     if writable    → attemptWrite(channel)
                        └─ channel.write() → NetworkSend
                        └─ completedSends에 추가
```

### 10.3 NetworkReceive 프로토콜

```
2단계 읽기:
  1단계: 4바이트 크기 헤더 읽기
         └─ 메시지 전체 크기 파악
  2단계: 크기만큼 페이로드 버퍼 할당 후 읽기
         └─ complete() == true 되면 완료
```

**왜 2단계인가?** TCP 스트림에서는 메시지 경계가 없다. 먼저 크기를 읽어 버퍼를 할당해야 전체 메시지를 정확히 수신할 수 있다.

## 11. 컴포넌트 간 상호작용 요약

```
┌─────────────────────────────────────────────────────────────┐
│                     요청 처리 경로                             │
│                                                             │
│  클라이언트 TCP    →  Acceptor     →  Processor             │
│                                        │                    │
│                     RequestChannel ◀───┘                    │
│                        │                                    │
│                     KafkaRequestHandler                     │
│                        │                                    │
│                     KafkaApis.handle()                      │
│                     ╱    │    ╲                              │
│            ┌───────┘     │     └────────┐                   │
│            ▼             ▼              ▼                   │
│     ReplicaManager  GroupCoord.  TransactionCoord.          │
│            │                                                │
│         Partition                                           │
│            │                                                │
│        UnifiedLog                                           │
│            │                                                │
│        LogSegment                                           │
│         ╱  │  ╲                                             │
│     .log .index .timeindex                                  │
└─────────────────────────────────────────────────────────────┘
```

## 12. 핵심 소스 파일 경로

| 컴포넌트 | 파일 | 줄 수 (대략) |
|----------|------|-------------|
| SocketServer | `core/.../network/SocketServer.scala` | ~1200 |
| KafkaApis | `core/.../server/KafkaApis.scala` | ~3000+ |
| ReplicaManager | `core/.../server/ReplicaManager.scala` | ~1500 |
| Partition | `core/.../cluster/Partition.scala` | ~1200 |
| UnifiedLog | `storage/.../log/UnifiedLog.java` | ~2000 |
| LogSegment | `storage/.../log/LogSegment.java` | ~500 |
| Selector | `clients/.../network/Selector.java` | ~1100 |
| KafkaRaftClient | `raft/.../raft/KafkaRaftClient.java` | ~2500 |
| QuorumController | `metadata/.../controller/QuorumController.java` | ~2000 |
| GroupMetadataManager | `group-coordinator/.../GroupMetadataManager.java` | ~6000+ |
