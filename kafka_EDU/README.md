# Apache Kafka 교육 자료 (EDU)

## 프로젝트 개요

Apache Kafka는 **분산 이벤트 스트리밍 플랫폼**으로, 고성능 데이터 파이프라인, 스트리밍 분석, 데이터 통합, 미션 크리티컬 애플리케이션에 사용된다. LinkedIn에서 시작되어 Apache Software Foundation에 기증된 오픈소스 프로젝트다.

### 핵심 특징

| 특성 | 설명 |
|------|------|
| **분산 시스템** | 수백 대의 브로커로 클러스터 구성, 수평 확장 가능 |
| **내구성** | 디스크 기반 로그 저장, 복제를 통한 데이터 안전성 보장 |
| **고처리량** | 초당 수백만 메시지 처리, 배치 I/O와 제로카피 전송 |
| **저지연** | 밀리초 단위 end-to-end 지연시간 |
| **정확히 한 번** | Idempotent Producer + Transaction으로 exactly-once 보장 |
| **KRaft** | ZooKeeper 없이 자체 Raft 기반 메타데이터 관리 (Kafka 4.0+) |

### 핵심 개념

```
┌─────────────────────────────────────────────────────────┐
│                    Kafka Cluster                         │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐                │
│  │Broker 1 │  │Broker 2 │  │Broker 3 │                │
│  │(Leader) │  │(Follower)│  │(Follower)│                │
│  └────┬────┘  └────┬────┘  └────┬────┘                │
│       │            │            │                       │
│  ┌────┴────────────┴────────────┴────┐                 │
│  │     Topic: orders (3 partitions)   │                 │
│  │  ┌──────┐ ┌──────┐ ┌──────┐      │                 │
│  │  │ P0   │ │ P1   │ │ P2   │      │                 │
│  │  └──────┘ └──────┘ └──────┘      │                 │
│  └───────────────────────────────────┘                 │
└─────────────────────────────────────────────────────────┘
     ▲                                    │
     │ Produce                            │ Consume
┌────┴────┐                          ┌────┴────┐
│Producer │                          │Consumer │
│  App    │                          │ Group   │
└─────────┘                          └─────────┘
```

- **토픽(Topic)**: 메시지의 논리적 카테고리
- **파티션(Partition)**: 토픽의 물리적 분할 단위, 순서 보장의 기본 단위
- **브로커(Broker)**: Kafka 서버 인스턴스
- **프로듀서(Producer)**: 메시지를 토픽에 발행
- **컨슈머(Consumer)**: 토픽에서 메시지를 구독
- **컨슈머 그룹(Consumer Group)**: 파티션을 분배받아 병렬 처리

### 소스 언어

- **Java** (주력): Clients, Connect, Streams, Server, Storage
- **Scala**: Core broker (KafkaServer, KafkaApis), 레거시 코드
- 빌드 시스템: **Gradle** (settings.gradle에서 30+ 서브프로젝트 관리)

---

## 문서 목차

### 기본 문서

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, KRaft 모드, 브로커 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | Record, RecordBatch, Topic/Partition, 프로토콜 스키마 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | Produce/Fetch/Rebalance 주요 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 모듈 의존성, 빌드 시스템 |
| 05 | [핵심 컴포넌트](05-core-components.md) | SocketServer, ReplicaManager, LogManager, Controller |
| 06 | [운영](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서

| # | 문서 | 내용 |
|---|------|------|
| 07 | [로그 스토리지 엔진](07-log-storage-engine.md) | Segment, Index, 쓰기/읽기 경로, Compaction |
| 08 | [네트워킹과 프로토콜](08-networking-protocol.md) | NIO Selector, Acceptor/Processor, 요청 처리 파이프라인 |
| 09 | [복제 시스템](09-replication.md) | ISR, High Watermark, Leader/Follower 복제 |
| 10 | [KRaft 합의](10-kraft-consensus.md) | Raft 프로토콜, 메타데이터 쿼럼, 컨트롤러 |
| 11 | [프로듀서와 컨슈머](11-producer-consumer.md) | 배치, 압축, 파티셔닝, 폴링, 오프셋 관리 |
| 12 | [그룹 코디네이터](12-group-coordinator.md) | 리밸런스, __consumer_offsets, KIP-848 |
| 13 | [트랜잭션 시스템](13-transaction-system.md) | Exactly-once, 2PC, Transaction Coordinator |
| 14 | [Kafka Streams](14-kafka-streams.md) | 토폴로지, StateStore, 스트림 처리 |
| 15 | [Kafka Connect](15-kafka-connect.md) | Worker, Connector, Task, SMT |
| 16 | [컨트롤러](16-controller.md) | QuorumController, 메타데이터 로그, 토픽 관리 |
| 17 | [보안](17-security.md) | SASL, SSL, ACL, 인증/인가 |
| 18 | [운영 도구](18-operations-tools.md) | CLI 도구, 모니터링, 성능 튜닝 |

### PoC (Proof of Concept)

| # | PoC | 핵심 개념 |
|---|-----|----------|
| 01 | [로그 세그먼트](poc-01-log-segment/) | 파일 기반 append-only 로그와 오프셋 인덱스 |
| 02 | [파티셔닝](poc-02-partitioning/) | 해시/라운드로빈 파티션 분배 |
| 03 | [복제](poc-03-replication/) | Leader-Follower 복제와 ISR |
| 04 | [레코드 배치](poc-04-record-batch/) | RecordBatch 인코딩/디코딩 |
| 05 | [NIO 네트워크](poc-05-nio-network/) | Java NIO 스타일 Selector/Channel |
| 06 | [요청 처리](poc-06-request-handler/) | Acceptor → Processor → Handler 파이프라인 |
| 07 | [KRaft 합의](poc-07-kraft-raft/) | Raft 리더 선출과 로그 복제 |
| 08 | [컨슈머 그룹](poc-08-consumer-group/) | 그룹 코디네이터와 리밸런스 |
| 09 | [프로듀서 배치](poc-09-producer-batch/) | RecordAccumulator 배치 전략 |
| 10 | [로그 컴팩션](poc-10-log-compaction/) | 키 기반 중복 제거 |
| 11 | [트랜잭션](poc-11-transaction/) | 2PC와 exactly-once 시뮬레이션 |
| 12 | [오프셋 인덱스](poc-12-offset-index/) | 이진 탐색 기반 오프셋 검색 |
| 13 | [High Watermark](poc-13-high-watermark/) | HW 계산과 복제 동기화 |
| 14 | [스트림 토폴로지](poc-14-stream-topology/) | DAG 기반 스트림 처리 |
| 15 | [커넥트 프레임워크](poc-15-connect-framework/) | Source/Sink 커넥터 런타임 |
| 16 | [ACL 인가](poc-16-acl-auth/) | 리소스 기반 접근 제어 |

---

## 소스코드 참조

- 소스 디렉토리: `/kafka/` (Apache Kafka trunk)
- 주요 언어: Java 17+, Scala 2.13
- 빌드: `./gradlew jar`
- 실행: `./bin/kafka-server-start.sh config/server.properties`

---

## 학습 로드맵

```
[입문] README → 01-architecture → 04-code-structure
  ↓
[기본] 02-data-model → 03-sequence-diagrams → 05-core-components
  ↓
[심화] 07-log-storage → 08-networking → 09-replication → 10-kraft
  ↓
[클라이언트] 11-producer-consumer → 12-group-coordinator → 13-transaction
  ↓
[에코시스템] 14-streams → 15-connect → 16-controller
  ↓
[운영] 06-operations → 17-security → 18-operations-tools
```
