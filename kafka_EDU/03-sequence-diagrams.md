# Apache Kafka 시퀀스 다이어그램

## 1. 개요

Kafka의 주요 유즈케이스를 요청 흐름으로 분석한다. 모든 흐름은 소스코드에서 확인한 실제 클래스와 메서드를 기반으로 한다.

## 2. Produce 흐름 (메시지 발행)

### 2.1 프로듀서 측 흐름

```
┌──────────┐     ┌──────────────┐     ┌────────┐     ┌────────────┐
│   App    │     │RecordAccum.  │     │ Sender │     │  Broker    │
│          │     │(배치 버퍼)     │     │(IO 스레드)│    │            │
└────┬─────┘     └──────┬───────┘     └───┬────┘     └─────┬──────┘
     │                   │                 │                │
     │ send(record)      │                 │                │
     │──────────────────▶│                 │                │
     │                   │                 │                │
     │  1. serialize(key, value)           │                │
     │  2. partition(topic, key)           │                │
     │  3. append(batch)  │                │                │
     │◀─ Future<Meta>─────│                │                │
     │                   │                 │                │
     │                   │  배치 full 또는   │                │
     │                   │  linger.ms 초과   │                │
     │                   │────wakeup()────▶│                │
     │                   │                 │                │
     │                   │ ready()         │                │
     │                   │◀────────────────│                │
     │                   │                 │                │
     │                   │ drain(batches)  │                │
     │                   │◀────────────────│                │
     │                   │                 │                │
     │                   │                 │ ProduceRequest │
     │                   │                 │───────────────▶│
     │                   │                 │                │
     │                   │                 │ ProduceResponse│
     │                   │                 │◀───────────────│
     │                   │                 │                │
     │                   │  complete(future, offset)        │
     │ callback(metadata)│◀────────────────│                │
     │◀──────────────────│                 │                │
```

**핵심 클래스**:
- `KafkaProducer.doSend()` — 직렬화, 파티셔닝, 배치 추가
- `RecordAccumulator.append()` — 배치에 레코드 추가, 새 배치 생성
- `Sender.runOnce()` → `sendProducerData()` — 배치 수집 및 전송
- `Sender.sendProduceRequest()` — ProduceRequest 빌드, 네트워크 전송

### 2.2 브로커 측 처리

```
┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐
│ Acceptor │   │Processor │   │ Request  │   │ KafkaApis│   │ Replica  │
│          │   │(NIO)     │   │ Channel  │   │          │   │ Manager  │
└────┬─────┘   └────┬─────┘   └────┬─────┘   └────┬─────┘   └────┬─────┘
     │              │              │              │              │
     │ accept()     │              │              │              │
     │──────────────▶              │              │              │
     │              │              │              │              │
     │ assign to    │              │              │              │
     │ processor    │              │              │              │
     │              │ poll()       │              │              │
     │              │──selector───▶│              │              │
     │              │              │              │              │
     │              │ completed    │              │              │
     │              │ receives     │              │              │
     │              │◀─────────────│              │              │
     │              │              │              │              │
     │              │ parse header │              │              │
     │              │ create Request              │              │
     │              │──────────────▶ sendRequest()│              │
     │              │              │──────────────▶              │
     │              │              │              │              │
     │              │              │     receiveRequest()        │
     │              │              │              │              │
     │              │              │ handleProduceRequest()      │
     │              │              │              │──────────────▶
     │              │              │              │              │
     │              │              │              │ appendToLocal│
     │              │              │              │ Log(records) │
     │              │              │              │◀─────────────│
     │              │              │              │              │
     │              │              │ sendResponse │              │
     │              │              │◀─────────────│              │
     │              │ enqueue      │              │              │
     │              │ response     │              │              │
     │              │◀─────────────│              │              │
     │              │              │              │              │
     │              │ write to     │              │              │
     │              │ socket       │              │              │
```

**핵심 메서드**:
- `Acceptor.acceptNewConnections()` — 새 연결 수락, Processor에 할당
- `Processor.processCompletedReceives()` — 완료된 수신 파싱, RequestChannel에 전달
- `KafkaApis.handleProduceRequest()` — Produce 요청 처리
- `ReplicaManager.appendRecords()` — 로컬 로그에 기록

## 3. Fetch 흐름 (메시지 소비)

### 3.1 컨슈머 측 흐름

```
┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐
│   App    │     │KafkaConsu│     │ Fetcher  │     │  Broker  │
│          │     │mer       │     │          │     │          │
└────┬─────┘     └────┬─────┘     └────┬─────┘     └────┬─────┘
     │                │                │                │
     │  poll(timeout) │                │                │
     │───────────────▶│                │                │
     │                │                │                │
     │                │ updateAssignment               │
     │                │ MetadataIfNeeded               │
     │                │                │                │
     │                │ sendFetches()  │                │
     │                │───────────────▶│                │
     │                │                │                │
     │                │                │ FetchRequest   │
     │                │                │───────────────▶│
     │                │                │                │
     │                │                │ FetchResponse  │
     │                │                │◀───────────────│
     │                │                │                │
     │                │ collectFetch() │                │
     │                │───────────────▶│                │
     │                │                │                │
     │                │  decompress &  │                │
     │                │  deserialize   │                │
     │                │◀───────────────│                │
     │                │                │                │
     │ ConsumerRecords│                │                │
     │◀───────────────│                │                │
     │                │                │                │
     │ [auto-commit]  │                │                │
     │                │ OffsetCommit   │                │
     │                │───────────────────────────────▶│
```

### 3.2 브로커 측 Fetch 처리

```
KafkaApis.handleFetchRequest()
  │
  ├─ ReplicaManager.readFromLog()
  │   │
  │   ├─ Partition.readRecords()
  │   │   │
  │   │   └─ UnifiedLog.read(startOffset, maxLength, isolation)
  │   │       │
  │   │       ├─ FetchIsolation 결정:
  │   │       │   LOG_END      → 최신 데이터까지
  │   │       │   HIGH_WATERMARK → HW까지 (일반 컨슈머)
  │   │       │   TXN_COMMITTED → LSO까지 (read_committed)
  │   │       │
  │   │       └─ LocalLog.read()
  │   │           │
  │   │           ├─ LogSegments에서 시작 세그먼트 찾기
  │   │           ├─ OffsetIndex.lookup(startOffset)  ← 이진 탐색
  │   │           └─ FileRecords.slice(position, size)
  │   │
  │   └─ updateFollowerFetchState()  (팔로워 Fetch인 경우)
  │       └─ maybeExpandIsr()  ← ISR 확장 검토
  │
  └─ 응답 전송 (zero-copy sendfile 가능)
```

## 4. 컨슈머 그룹 리밸런스 (Classic Protocol)

```
┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐
│Consumer A│   │Consumer B│   │  Group   │   │Controller│
│ (Leader) │   │(Follower)│   │Coordinator│  │          │
└────┬─────┘   └────┬─────┘   └────┬─────┘   └────┬─────┘
     │              │              │              │
     │ JoinGroup    │              │              │
     │─────────────────────────────▶              │
     │              │              │              │
     │              │ JoinGroup    │              │
     │              │─────────────▶│              │
     │              │              │              │
     │              │   PREPARING_REBALANCE       │
     │              │              │              │
     │              │   모든 멤버 합류 대기         │
     │              │              │              │
     │ JoinResponse │              │              │
     │ (leader=A,   │              │              │
     │  members=[]) │              │              │
     │◀─────────────────────────────│              │
     │              │              │              │
     │              │ JoinResponse │              │
     │              │ (leader=A)  │              │
     │              │◀─────────────│              │
     │              │              │              │
     │              │   COMPLETING_REBALANCE      │
     │              │              │              │
     │ 파티션 할당  │              │              │
     │ 계산 (클라이언트│             │              │
     │  측 assignor)│              │              │
     │              │              │              │
     │ SyncGroup    │              │              │
     │ (assignments)│              │              │
     │─────────────────────────────▶              │
     │              │              │              │
     │              │ SyncGroup   │              │
     │              │─────────────▶│              │
     │              │              │              │
     │              │   __consumer_offsets에       │
     │              │   그룹 메타데이터 저장        │
     │              │              │              │
     │ SyncResponse │              │              │
     │ (assignment) │              │              │
     │◀─────────────────────────────│              │
     │              │              │              │
     │              │SyncResponse │              │
     │              │(assignment) │              │
     │              │◀─────────────│              │
     │              │              │              │
     │              │      STABLE                 │
     │              │              │              │
     │ Heartbeat    │              │              │
     │─────────(주기적)────────────▶│              │
```

## 5. KRaft 리더 선출

```
┌──────────┐   ┌──────────┐   ┌──────────┐
│  Node 1  │   │  Node 2  │   │  Node 3  │
│(Candidate)│  │ (Voter)  │   │ (Voter)  │
└────┬─────┘   └────┬─────┘   └────┬─────┘
     │              │              │
     │  선거 타임아웃  │              │
     │  발생         │              │
     │              │              │
     │  에포크 증가   │              │
     │  자기 자신에   │              │
     │  투표         │              │
     │              │              │
     │ VoteRequest  │              │
     │ (epoch=2)    │              │
     │─────────────▶│              │
     │────────────────────────────▶│
     │              │              │
     │              │ 로그 비교:    │
     │              │ lastEpoch,   │
     │              │ endOffset    │
     │              │              │
     │ VoteResponse │              │
     │ (granted=T)  │              │
     │◀─────────────│              │
     │              │              │
     │              │ VoteResponse │
     │              │ (granted=T)  │
     │◀────────────────────────────│
     │              │              │
     │  과반수 확보   │              │
     │  → Leader!   │              │
     │              │              │
     │ BeginQuorum  │              │
     │ EpochRequest │              │
     │─────────────▶│              │
     │────────────────────────────▶│
     │              │              │
     │              │  Follower    │
     │              │  전환        │
     │              │              │
     │◀──Fetch──────│              │
     │◀──Fetch────────────────────│
     │              │              │
     │  High Watermark 진행:       │
     │  HW = median(voterOffsets)  │
```

**소스**: `raft/src/main/java/org/apache/kafka/raft/KafkaRaftClient.java`

**상태 전이**:
```
Resigned → Unattached → Prospective → Candidate → Leader
                ↑            ↑                       │
                │            │                       │
                └────────────┴───────────────────────┘
                         (높은 에포크 발견)
```

## 6. 트랜잭션 흐름 (Exactly-Once)

```
┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐
│Transact. │   │  Txn     │   │ Broker   │   │ Broker   │
│Producer  │   │Coordinat.│   │ (P0 ldr) │   │ (P1 ldr) │
└────┬─────┘   └────┬─────┘   └────┬─────┘   └────┬─────┘
     │              │              │              │
     │ InitProdId   │              │              │
     │─────────────▶│              │              │
     │              │ PID=100,     │              │
     │              │ epoch=0 할당  │              │
     │◀─────────────│              │              │
     │              │              │              │
     │ beginTransaction()          │              │
     │ (클라이언트 로컬)            │              │
     │              │              │              │
     │ AddPartitions│              │              │
     │ ToTxn(P0,P1) │              │              │
     │─────────────▶│              │              │
     │              │ EMPTY→ONGOING│              │
     │◀──OK─────────│              │              │
     │              │              │              │
     │ Produce(P0)  │              │              │
     │─────────────────────────────▶ (txn flag)   │
     │              │              │              │
     │ Produce(P1)  │              │              │
     │──────────────────────────────────────────▶│
     │              │              │              │
     │ EndTxn       │              │              │
     │ (COMMIT)     │              │              │
     │─────────────▶│              │              │
     │              │              │              │
     │              │ Phase 1: __transaction_state에 기록
     │              │ ONGOING → PREPARE_COMMIT    │
     │              │              │              │
     │              │ Phase 2: WriteTxnMarkers    │
     │              │─────────────▶│              │
     │              │────────────────────────────▶│
     │              │              │              │
     │              │ COMMIT 마커  │ COMMIT 마커  │
     │              │ 기록 완료     │ 기록 완료     │
     │              │◀─────────────│              │
     │              │◀────────────────────────────│
     │              │              │              │
     │              │ PREPARE_COMMIT              │
     │              │ → COMPLETE_COMMIT           │
     │◀──OK─────────│              │              │
```

**2PC (Two-Phase Commit)**:
1. **Phase 1 (Prepare)**: 코디네이터가 `__transaction_state`에 PREPARE_COMMIT 기록
2. **Phase 2 (Commit)**: 관련 파티션 리더에게 COMMIT 마커 기록 요청

## 7. 브로커 시작 및 메타데이터 동기화

```
┌──────────┐   ┌──────────┐   ┌──────────┐
│  Broker  │   │  Shared  │   │Controller│
│  Server  │   │  Server  │   │ (Leader) │
└────┬─────┘   └────┬─────┘   └────┬─────┘
     │              │              │
     │ startup()    │              │
     │─────────────▶│              │
     │              │              │
     │              │ startForBroker()
     │              │──────────────▶
     │              │              │
     │              │ MetadataLoader
     │              │ 시작          │
     │              │              │
     │              │◀──스냅샷/델타─│
     │              │              │
     │ BrokerLifecycleManager     │
     │ .start()     │              │
     │──────────────────────────▶ │
     │              │ BrokerRegistration
     │              │              │
     │              │    등록 확인  │
     │◀────────────────────────────
     │              │              │
     │ 초기 메타데이터 │              │
     │ 캐치업 대기    │              │
     │              │              │
     │◀──첫 퍼블리시──│              │
     │              │              │
     │ 언펜싱 대기    │              │
     │              │              │
     │◀──언펜싱 완료────────────────
     │              │              │
     │ SocketServer │              │
     │ 활성화        │              │
     │              │              │
     │ ▶▶ STARTED ◀◀│              │
```

## 8. Follower 복제 흐름

```
┌──────────┐   ┌──────────┐
│  Leader  │   │ Follower │
│ (Broker1)│   │ (Broker2)│
└────┬─────┘   └────┬─────┘
     │              │
     │◀──FetchReq───│  ReplicaFetcherThread
     │  (offset=100)│  .processPartitionData()
     │              │
     │──FetchResp──▶│
     │  (records,   │
     │   HW=95)     │
     │              │
     │              │ 1. appendAsFollower(records)
     │              │    → UnifiedLog에 기록
     │              │
     │              │ 2. updateHighWatermark(95)
     │              │    → 팔로워 HW 업데이트
     │              │
     │              │ 3. LEO가 리더 HW 이상이면
     │              │    ISR 확장 가능
     │              │
     │◀──FetchReq───│  (offset=150, 다음 배치)
     │              │
     │              │
     │  HW 진행:    │
     │  HW = min(ISR 멤버들의 LEO)
     │              │
     │  ISR 축소:    │
     │  replica.lagTime > replicaLagTimeMaxMs
     │  → ISR에서 제거
```

## 9. 오프셋 커밋 흐름

```
┌──────────┐   ┌──────────┐   ┌──────────┐
│Consumer  │   │  Group   │   │__consumer│
│          │   │Coordinator│  │_offsets  │
└────┬─────┘   └────┬─────┘   └────┬─────┘
     │              │              │
     │ OffsetCommit │              │
     │ (group=g1,   │              │
     │  topic=t,    │              │
     │  part=0,     │              │
     │  offset=100) │              │
     │─────────────▶│              │
     │              │              │
     │              │ validate:    │
     │              │ - 그룹 존재?  │
     │              │ - 멤버 유효? │
     │              │ - 세대 일치?  │
     │              │              │
     │              │ produce      │
     │              │ (key=g1/t/0, │
     │              │  val=offset) │
     │              │─────────────▶│
     │              │              │
     │              │◀──ack────────│
     │              │              │
     │◀──success────│              │
     │              │              │
     │ OffsetFetch  │              │
     │ (group=g1)   │              │
     │─────────────▶│              │
     │              │              │
     │              │ 인메모리 맵에서│
     │              │ 조회          │
     │              │              │
     │◀──offsets────│              │
```

## 10. 핵심 소스 파일 참조

| 흐름 | 소스 파일 | 핵심 메서드 |
|------|----------|-------------|
| Produce (클라이언트) | `clients/.../producer/KafkaProducer.java` | `doSend()` |
| Produce (배치) | `clients/.../producer/internals/RecordAccumulator.java` | `append()` |
| Produce (전송) | `clients/.../producer/internals/Sender.java` | `sendProducerData()` |
| Produce (브로커) | `core/.../server/KafkaApis.scala` | `handleProduceRequest()` |
| Fetch (컨슈머) | `clients/.../consumer/internals/ClassicKafkaConsumer.java` | `poll()` |
| Fetch (브로커) | `core/.../server/KafkaApis.scala` | `handleFetchRequest()` |
| Rebalance | `group-coordinator/.../GroupMetadataManager.java` | `classicGroupJoin()` |
| KRaft 선출 | `raft/.../raft/KafkaRaftClient.java` | `handleVoteRequest()` |
| 트랜잭션 | `core/.../coordinator/transaction/TransactionCoordinator.scala` | 2PC 흐름 |
| 복제 | `core/.../server/ReplicaFetcherThread.scala` | `processPartitionData()` |
