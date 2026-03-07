# 18. Kafka 운영 도구와 성능 최적화 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [bin/ 스크립트 체계](#2-bin-스크립트-체계)
3. [kafka-server-start.sh와 kafka-storage.sh](#3-kafka-server-startsh와-kafka-storagesh)
4. [kafka-topics.sh: 토픽 관리](#4-kafka-topicssh-토픽-관리)
5. [kafka-console-producer/consumer.sh](#5-kafka-console-producerconsumersh)
6. [kafka-consumer-groups.sh: 컨슈머 그룹 관리](#6-kafka-consumer-groupssh-컨슈머-그룹-관리)
7. [kafka-configs.sh: 동적 설정 변경](#7-kafka-configssh-동적-설정-변경)
8. [kafka-metadata.sh: KRaft 메타데이터 조회](#8-kafka-metadatash-kraft-메타데이터-조회)
9. [파티션 관리 도구](#9-파티션-관리-도구)
10. [JMX 메트릭과 모니터링](#10-jmx-메트릭과-모니터링)
11. [성능 튜닝](#11-성능-튜닝)
12. [부하 테스트: Trogdor와 성능 도구](#12-부하-테스트-trogdor와-성능-도구)
13. [Docker 운영](#13-docker-운영)
14. [롤링 업그레이드](#14-롤링-업그레이드)
15. [왜(Why) 이렇게 설계했는가](#15-왜why-이렇게-설계했는가)

---

## 1. 개요

Kafka는 다양한 CLI 도구를 제공하여 클러스터 운영, 모니터링, 성능 튜닝을 수행할 수 있다.
모든 도구는 `bin/` 디렉토리의 쉘 스크립트로 제공되며, 내부적으로 Java 클래스를 실행한다.

```
소스 파일 위치:
  bin/                                          -- 쉘 스크립트 (진입점)
  tools/src/main/java/org/apache/kafka/tools/   -- 도구 Java 구현
  trogdor/                                      -- 부하 테스트 프레임워크
  docker/                                       -- Docker 이미지 빌드
```

### 도구 분류

```
+-------------------------------------------------------------------+
|                     Kafka 운영 도구                                 |
+-------------------------------------------------------------------+
|                                                                   |
|  [클러스터 관리]                                                    |
|    kafka-server-start.sh    -- 브로커 시작                         |
|    kafka-server-stop.sh     -- 브로커 중지                         |
|    kafka-storage.sh         -- 스토리지 포맷 (KRaft)               |
|    kafka-cluster.sh         -- 클러스터 ID 관리                    |
|                                                                   |
|  [토픽/파티션 관리]                                                 |
|    kafka-topics.sh          -- 토픽 CRUD                           |
|    kafka-reassign-partitions.sh -- 파티션 재할당                   |
|    kafka-leader-election.sh -- 리더 선출                           |
|    kafka-delete-records.sh  -- 레코드 삭제                         |
|                                                                   |
|  [프로듀서/컨슈머]                                                  |
|    kafka-console-producer.sh -- 콘솔 프로듀서                      |
|    kafka-console-consumer.sh -- 콘솔 컨슈머                        |
|    kafka-verifiable-producer.sh -- 검증 가능 프로듀서               |
|    kafka-verifiable-consumer.sh -- 검증 가능 컨슈머                 |
|                                                                   |
|  [컨슈머 그룹/오프셋]                                               |
|    kafka-consumer-groups.sh -- 컨슈머 그룹 관리                    |
|    kafka-get-offsets.sh     -- 오프셋 조회                          |
|                                                                   |
|  [설정/보안]                                                        |
|    kafka-configs.sh         -- 동적 설정 변경                       |
|    kafka-acls.sh            -- ACL 관리                            |
|    kafka-delegation-tokens.sh -- 위임 토큰 관리                     |
|                                                                   |
|  [모니터링/진단]                                                    |
|    kafka-log-dirs.sh        -- 로그 디렉토리 상태                   |
|    kafka-dump-log.sh        -- 로그 세그먼트 덤프                   |
|    kafka-metadata-quorum.sh -- KRaft 쿼럼 상태                     |
|    kafka-metadata-shell.sh  -- 메타데이터 쉘                       |
|    kafka-jmx.sh             -- JMX 메트릭 조회                     |
|    kafka-broker-api-versions.sh -- API 버전 조회                   |
|                                                                   |
|  [성능 테스트]                                                      |
|    kafka-producer-perf-test.sh  -- 프로듀서 성능 테스트             |
|    kafka-consumer-perf-test.sh  -- 컨슈머 성능 테스트               |
|    kafka-e2e-latency.sh        -- E2E 레이턴시 측정                |
|                                                                   |
|  [기능 관리]                                                        |
|    kafka-features.sh        -- 기능 버전 관리                       |
|    kafka-transactions.sh    -- 트랜잭션 관리                        |
+-------------------------------------------------------------------+
```

---

## 2. bin/ 스크립트 체계

### 스크립트 구조

모든 `bin/` 스크립트는 동일한 패턴을 따른다:

```bash
# bin/kafka-topics.sh (간소화)
exec $(dirname $0)/kafka-run-class.sh org.apache.kafka.tools.TopicCommand "$@"
```

### kafka-run-class.sh: 공통 실행기

```
kafka-run-class.sh <main-class> [args...]
    |
    +---> 1. CLASSPATH 구성
    |         - libs/*.jar
    |         - connect 관련 jar (Connect 도구 시)
    |
    +---> 2. JVM 옵션 설정
    |         - KAFKA_HEAP_OPTS (기본: -Xmx256M)
    |         - KAFKA_JVM_PERFORMANCE_OPTS
    |         - KAFKA_GC_LOG_OPTS
    |         - KAFKA_JMX_OPTS
    |
    +---> 3. Log4j 설정
    |         - KAFKA_LOG4J_OPTS
    |
    +---> 4. java 실행
              java $KAFKA_HEAP_OPTS $JVM_OPTS -cp $CLASSPATH <main-class> "$@"
```

### kafka-server-start.sh JVM 기본값

```bash
# bin/kafka-server-start.sh
if [ "x$KAFKA_HEAP_OPTS" = "x" ]; then
    export KAFKA_HEAP_OPTS="-Xmx1G -Xms1G"
fi

# 서버용 힙 크기: 1GB (기본)
# 프로덕션에서는 6~8GB 권장
```

---

## 3. kafka-server-start.sh와 kafka-storage.sh

### kafka-server-start.sh

**소스 파일**: `bin/kafka-server-start.sh`

```bash
# 사용법
kafka-server-start.sh [-daemon] server.properties [--override property=value]*

# 예시: 데몬 모드로 시작
kafka-server-start.sh -daemon config/kraft/server.properties

# 설정 오버라이드
kafka-server-start.sh config/kraft/server.properties \
  --override log.dirs=/data/kafka-logs \
  --override num.partitions=3
```

### kafka-storage.sh: KRaft 스토리지 초기화

**소스 파일**: `bin/kafka-storage.sh` → `kafka.tools.StorageTool`

KRaft 모드에서는 브로커 시작 전에 반드시 스토리지를 포맷해야 한다.

```
KRaft 클러스터 초기 설정 절차:

1단계: 클러스터 ID 생성
  $ kafka-storage.sh random-uuid
  출력: xtzWWN4bTjitpL3kfd9s5g

2단계: 스토리지 포맷
  $ kafka-storage.sh format -t xtzWWN4bTjitpL3kfd9s5g \
      -c config/kraft/server.properties
  출력: Formatting /tmp/kraft-combined-logs with metadata.version 3.8-IV0

3단계: 브로커 시작
  $ kafka-server-start.sh config/kraft/server.properties
```

### 스토리지 포맷이 하는 일

```
kafka-storage.sh format
    |
    +---> 1. 클러스터 ID 검증
    |
    +---> 2. log.dirs 디렉토리 생성
    |
    +---> 3. meta.properties 생성
    |         {
    |           cluster.id=xtzWWN4bTjitpL3kfd9s5g
    |           node.id=1
    |           version=1
    |         }
    |
    +---> 4. __cluster_metadata-0/ 초기화 (컨트롤러 노드)
    |         - 부트스트랩 메타데이터 레코드 기록
    |         - FeatureLevelRecord (metadata.version)
    |
    +---> 5. 완료 메시지 출력
```

---

## 4. kafka-topics.sh: 토픽 관리

**소스 파일**: `tools/src/main/java/org/apache/kafka/tools/TopicCommand.java`

```java
public class TopicCommand {
    // Admin 클라이언트를 사용한 토픽 CRUD 작업
    // --create, --delete, --describe, --list, --alter 지원
}
```

### 토픽 생성

```bash
# 기본 토픽 생성
kafka-topics.sh --bootstrap-server localhost:9092 \
  --create --topic my-topic \
  --partitions 6 \
  --replication-factor 3

# 설정과 함께 생성
kafka-topics.sh --bootstrap-server localhost:9092 \
  --create --topic my-topic \
  --partitions 6 \
  --replication-factor 3 \
  --config retention.ms=604800000 \
  --config max.message.bytes=1048576
```

### 토픽 조회

```bash
# 토픽 목록
kafka-topics.sh --bootstrap-server localhost:9092 --list

# 토픽 상세 정보
kafka-topics.sh --bootstrap-server localhost:9092 \
  --describe --topic my-topic

# 출력 예시:
# Topic: my-topic   TopicId: abc123   PartitionCount: 6   ReplicationFactor: 3
#   Topic: my-topic   Partition: 0   Leader: 1   Replicas: 1,2,3   Isr: 1,2,3
#   Topic: my-topic   Partition: 1   Leader: 2   Replicas: 2,3,1   Isr: 2,3,1
#   ...

# Under-replicated 파티션만 조회
kafka-topics.sh --bootstrap-server localhost:9092 \
  --describe --under-replicated-partitions

# 리더가 없는 파티션 조회
kafka-topics.sh --bootstrap-server localhost:9092 \
  --describe --unavailable-partitions
```

### 토픽 변경 및 삭제

```bash
# 파티션 수 증가 (감소 불가)
kafka-topics.sh --bootstrap-server localhost:9092 \
  --alter --topic my-topic --partitions 12

# 토픽 삭제
kafka-topics.sh --bootstrap-server localhost:9092 \
  --delete --topic my-topic
```

---

## 5. kafka-console-producer/consumer.sh

### 콘솔 프로듀서

```bash
# 기본 사용
kafka-console-producer.sh --bootstrap-server localhost:9092 \
  --topic my-topic
> hello
> world

# 키-값 형태로 전송
kafka-console-producer.sh --bootstrap-server localhost:9092 \
  --topic my-topic \
  --property parse.key=true \
  --property key.separator=:
> key1:value1
> key2:value2

# 파일에서 전송
kafka-console-producer.sh --bootstrap-server localhost:9092 \
  --topic my-topic < input.txt

# ACK 설정
kafka-console-producer.sh --bootstrap-server localhost:9092 \
  --topic my-topic \
  --producer-property acks=all
```

### 콘솔 컨슈머

```bash
# 최신 메시지부터 소비
kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic my-topic

# 처음부터 소비
kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic my-topic --from-beginning

# 키-값 출력
kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic my-topic \
  --property print.key=true \
  --property key.separator=:

# 특정 파티션, 특정 오프셋부터
kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic my-topic \
  --partition 0 --offset 100

# 최대 N개 메시지만 소비
kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic my-topic --max-messages 10

# 타임스탬프 출력
kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic my-topic \
  --property print.timestamp=true
```

---

## 6. kafka-consumer-groups.sh: 컨슈머 그룹 관리

### 그룹 목록과 상태

```bash
# 컨슈머 그룹 목록
kafka-consumer-groups.sh --bootstrap-server localhost:9092 --list

# 그룹 상세 정보 (파티션별 오프셋과 랙)
kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --describe --group my-group
```

### 출력 예시

```
GROUP     TOPIC     PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG    CONSUMER-ID  HOST      CLIENT-ID
my-group  orders    0          12345           12350           5      consumer-1   /10.0.0.1 client-1
my-group  orders    1          23456           23460           4      consumer-1   /10.0.0.1 client-1
my-group  orders    2          34567           34567           0      consumer-2   /10.0.0.2 client-2
```

### 그룹 상태 유형

```
+-------------------------------------------------------------------+
| 그룹 상태         | 설명                                           |
+-------------------------------------------------------------------+
| Empty             | 멤버 없음, 오프셋은 남아있을 수 있음              |
| PreparingRebalance| 리밸런스 진행 중                                 |
| CompletingRebalance| 리밸런스 완료 대기                              |
| Stable            | 정상 운영 중, 모든 멤버 할당 완료                 |
| Dead              | 그룹 삭제됨 또는 오프셋 만료                     |
+-------------------------------------------------------------------+
```

### 오프셋 리셋

```bash
# 처음으로 리셋
kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --reset-offsets --to-earliest --execute \
  --topic my-topic

# 최신으로 리셋
kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --reset-offsets --to-latest --execute \
  --topic my-topic

# 특정 오프셋으로 리셋
kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --reset-offsets --to-offset 1000 --execute \
  --topic my-topic:0  # 파티션 0만

# 특정 시간으로 리셋
kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --reset-offsets \
  --to-datetime 2024-01-01T00:00:00.000 --execute \
  --topic my-topic

# 상대적 리셋 (현재 위치에서 -100)
kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --reset-offsets --shift-by -100 --execute \
  --topic my-topic

# dry-run (실제 리셋 없이 결과 미리보기)
kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --reset-offsets --to-earliest --dry-run \
  --topic my-topic
```

### 오프셋 리셋 시 주의사항

```
+-------------------------------------------------------------------+
| 주의: 오프셋 리셋은 반드시 컨슈머 그룹이 비활성(Empty) 상태일 때     |
| 수행해야 한다. 활성 컨슈머가 있으면 거부된다.                        |
+-------------------------------------------------------------------+

절차:
  1. 컨슈머 애플리케이션 중지
  2. 그룹 상태 확인 (Empty 확인)
  3. --dry-run으로 예상 결과 확인
  4. --execute로 실제 리셋
  5. 컨슈머 애플리케이션 재시작
```

---

## 7. kafka-configs.sh: 동적 설정 변경

### 엔터티 타입

```
+-------------------------------------------------------------------+
| entity-type | 대상          | 예시                                |
+-------------------------------------------------------------------+
| brokers     | 브로커        | --entity-name 0 (브로커 ID)         |
| brokers     | 클러스터 기본  | --entity-default                    |
| topics      | 토픽          | --entity-name my-topic              |
| users       | 사용자        | --entity-name alice                 |
| clients     | 클라이언트 ID | --entity-name client-1              |
| ips         | IP 주소       | --entity-name 10.0.0.1              |
+-------------------------------------------------------------------+
```

### 설정 변경 예시

```bash
# 토픽 설정 변경
kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --topic my-topic \
  --add-config retention.ms=86400000,max.message.bytes=1048576

# 토픽 설정 삭제 (기본값으로 복원)
kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --topic my-topic \
  --delete-config retention.ms

# 브로커 동적 설정 변경 (재시작 불필요)
kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --entity-type brokers --entity-name 0 \
  --add-config log.cleaner.threads=2

# 클러스터 전체 기본 설정
kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --entity-type brokers --entity-default \
  --add-config log.retention.hours=168

# 사용자 쿼터 설정
kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --entity-type users --entity-name alice \
  --add-config producer_byte_rate=1048576,consumer_byte_rate=2097152

# SCRAM 자격증명 생성
kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --entity-type users --entity-name alice \
  --add-config 'SCRAM-SHA-256=[iterations=8192,password=alice-secret]'

# 현재 설정 조회
kafka-configs.sh --bootstrap-server localhost:9092 \
  --describe --topic my-topic

# 모든 동적 브로커 설정 조회
kafka-configs.sh --bootstrap-server localhost:9092 \
  --describe --entity-type brokers --entity-name 0
```

### 동적 설정 카테고리

```
+-------------------------------------------------------------------+
| 카테고리              | 설명                     | 재시작 필요   |
+-------------------------------------------------------------------+
| read-only             | server.properties에서만  | 필요         |
| per-broker            | 브로커별 동적 변경       | 불필요       |
| cluster-wide          | 클러스터 전체 동적 변경  | 불필요       |
+-------------------------------------------------------------------+

per-broker 예시:
  - log.cleaner.threads
  - num.io.threads
  - num.network.threads
  - ssl.keystore.location (인증서 갱신!)

cluster-wide 예시:
  - log.retention.hours
  - log.retention.bytes
  - message.max.bytes
```

---

## 8. kafka-metadata.sh: KRaft 메타데이터 조회

### kafka-metadata-quorum.sh

```bash
# 쿼럼 상태 조회
kafka-metadata-quorum.sh --bootstrap-server localhost:9092 describe --status

# 출력 예시:
# ClusterId:              xtzWWN4bTjitpL3kfd9s5g
# LeaderId:               1
# LeaderEpoch:            5
# HighWatermark:          12345
# MaxFollowerLag:         0
# MaxFollowerLagTimeMs:   0
# CurrentVoters:          [1, 2, 3]
# CurrentObservers:       [4, 5, 6]

# 복제 상태 조회
kafka-metadata-quorum.sh --bootstrap-server localhost:9092 describe --replication

# 출력 예시:
# NodeId  LogEndOffset  Lag  LastFetchTimestamp  LastCaughtUpTimestamp  Status
# 1       12345         0    1704067200000       1704067200000         Leader
# 2       12345         0    1704067199500       1704067199500         Follower
# 3       12343         2    1704067199000       1704067198000         Follower
```

### kafka-metadata-shell.sh

메타데이터 스냅샷을 파일 시스템처럼 탐색할 수 있는 대화형 쉘:

```bash
kafka-metadata-shell.sh --snapshot /var/kafka-logs/__cluster_metadata-0/...

# 쉘 명령어:
>> ls /
brokers  topicIds  topics  configs  features

>> ls /brokers
1  2  3

>> cat /brokers/1
{
  "brokerId": 1,
  "endpoints": [...],
  "rack": "rack-a",
  "fenced": false
}

>> ls /topics
my-topic  orders  __consumer_offsets

>> cat /topics/my-topic/0
{
  "partitionId": 0,
  "leader": 1,
  "replicas": [1, 2, 3],
  "isr": [1, 2, 3]
}
```

### kafka-log-dirs.sh

```bash
# 전체 로그 디렉토리 상태
kafka-log-dirs.sh --bootstrap-server localhost:9092 --describe

# 특정 브로커만
kafka-log-dirs.sh --bootstrap-server localhost:9092 \
  --describe --broker-list 0,1

# 특정 토픽만
kafka-log-dirs.sh --bootstrap-server localhost:9092 \
  --describe --topic-list my-topic

# 출력 예시:
# {
#   "brokers": [{
#     "broker": 0,
#     "logDirs": [{
#       "logDir": "/data/kafka-logs",
#       "error": null,
#       "partitions": [{
#         "partition": "my-topic-0",
#         "size": 1048576,
#         "offsetLag": 0,
#         "isFuture": false
#       }]
#     }]
#   }]
# }
```

---

## 9. 파티션 관리 도구

### kafka-reassign-partitions.sh

파티션의 레플리카를 다른 브로커로 이동한다.

```bash
# 1단계: 재할당 계획 생성
cat > topics-to-move.json << 'EOF'
{
  "version": 1,
  "topics": [
    {"topic": "my-topic"}
  ]
}
EOF

kafka-reassign-partitions.sh --bootstrap-server localhost:9092 \
  --topics-to-move-json-file topics-to-move.json \
  --broker-list "1,2,3" \
  --generate

# 출력: 현재 할당과 제안된 할당 JSON

# 2단계: 재할당 실행
kafka-reassign-partitions.sh --bootstrap-server localhost:9092 \
  --reassignment-json-file reassignment.json \
  --execute

# 3단계: 진행 상황 확인
kafka-reassign-partitions.sh --bootstrap-server localhost:9092 \
  --reassignment-json-file reassignment.json \
  --verify
```

### 재할당 JSON 형식

```json
{
  "version": 1,
  "partitions": [
    {
      "topic": "my-topic",
      "partition": 0,
      "replicas": [4, 5, 6],
      "log_dirs": ["any", "any", "any"]
    },
    {
      "topic": "my-topic",
      "partition": 1,
      "replicas": [5, 6, 4]
    }
  ]
}
```

### kafka-leader-election.sh

```bash
# Preferred 리더 선출 (안전)
kafka-leader-election.sh --bootstrap-server localhost:9092 \
  --election-type PREFERRED \
  --topic my-topic --partition 0

# 모든 파티션에 대해 preferred 리더 선출
kafka-leader-election.sh --bootstrap-server localhost:9092 \
  --election-type PREFERRED --all-topic-partitions

# Unclean 리더 선출 (데이터 손실 가능!)
kafka-leader-election.sh --bootstrap-server localhost:9092 \
  --election-type UNCLEAN \
  --topic my-topic --partition 0
```

### 리더 선출 비교

```
PREFERRED 선출:                    UNCLEAN 선출:
+----------------------------+    +----------------------------+
| ISR에 preferred leader가   |    | ISR이 비어있고,            |
| 있을 때만 성공              |    | 데이터 손실을 감수하고      |
|                            |    | ISR 밖 레플리카를 리더로    |
| 데이터 손실: 없음           |    | 데이터 손실: 가능          |
| 사용 시나리오:              |    | 사용 시나리오:             |
|   - 롤링 재시작 후 복구     |    |   - 장애 복구 (최후 수단)  |
|   - 리더 불균형 해소        |    |   - 가용성 > 내구성        |
+----------------------------+    +----------------------------+
```

---

## 10. JMX 메트릭과 모니터링

### kafka-jmx.sh

**소스 파일**: `tools/src/main/java/org/apache/kafka/tools/JmxTool.java`

```bash
# JMX 메트릭 조회
kafka-jmx.sh --jmx-url service:jmx:rmi:///jndi/rmi://localhost:9999/jmxrmi \
  --object-name kafka.server:type=BrokerTopicMetrics,name=MessagesInPerSec \
  --reporting-interval 1000
```

### 핵심 JMX 메트릭 카탈로그

```
+-------------------------------------------------------------------+
|  카테고리: 브로커 전체 성능                                         |
+-------------------------------------------------------------------+
| MBean                                          | 설명              |
+-------------------------------------------------------------------+
| kafka.server:type=BrokerTopicMetrics,           |                  |
|   name=MessagesInPerSec                         | 초당 메시지 수입 |
|   name=BytesInPerSec                            | 초당 바이트 수입 |
|   name=BytesOutPerSec                           | 초당 바이트 수출 |
|   name=TotalProduceRequestsPerSec               | 프로듀스 요청/초 |
|   name=TotalFetchRequestsPerSec                 | 페치 요청/초     |
|   name=FailedProduceRequestsPerSec              | 실패한 프로듀스  |
|   name=FailedFetchRequestsPerSec                | 실패한 페치      |
+-------------------------------------------------------------------+

+-------------------------------------------------------------------+
|  카테고리: 요청 지연                                                |
+-------------------------------------------------------------------+
| kafka.network:type=RequestMetrics,              |                  |
|   name=TotalTimeMs,request=Produce              | 프로듀스 총 시간 |
|   name=TotalTimeMs,request=FetchConsumer        | 컨슈머 페치 시간 |
|   name=RequestQueueTimeMs,request=Produce       | 큐 대기 시간     |
|   name=LocalTimeMs,request=Produce              | 로컬 처리 시간   |
|   name=RemoteTimeMs,request=Produce             | 원격 대기 시간   |
|   name=ResponseQueueTimeMs,request=Produce      | 응답 큐 시간     |
|   name=ResponseSendTimeMs,request=Produce       | 응답 전송 시간   |
+-------------------------------------------------------------------+

+-------------------------------------------------------------------+
|  카테고리: 복제 상태                                                |
+-------------------------------------------------------------------+
| kafka.server:type=ReplicaManager,               |                  |
|   name=UnderReplicatedPartitions                | ISR 부족 파티션  |
|   name=UnderMinIsrPartitionCount                | min.ISR 미달     |
|   name=PartitionCount                           | 파티션 총 수     |
|   name=LeaderCount                              | 리더 파티션 수   |
|   name=IsrShrinksPerSec                         | ISR 축소 빈도    |
|   name=IsrExpandsPerSec                         | ISR 확장 빈도    |
+-------------------------------------------------------------------+

+-------------------------------------------------------------------+
|  카테고리: 컨트롤러                                                 |
+-------------------------------------------------------------------+
| kafka.controller:type=KafkaController,          |                  |
|   name=ActiveControllerCount                    | 활성 컨트롤러 수 |
|   name=OfflinePartitionsCount                   | 오프라인 파티션  |
|   name=PreferredReplicaImbalanceCount           | 리더 불균형      |
+-------------------------------------------------------------------+

+-------------------------------------------------------------------+
|  카테고리: 네트워크                                                 |
+-------------------------------------------------------------------+
| kafka.network:type=SocketServer,                |                  |
|   name=NetworkProcessorAvgIdlePercent           | 네트워크 유휴율  |
| kafka.server:type=KafkaRequestHandlerPool,      |                  |
|   name=RequestHandlerAvgIdlePercent             | 요청 핸들러 유휴 |
+-------------------------------------------------------------------+
```

### Prometheus/Grafana 연동

```
+-------------------------------------------------------------------+
|  Kafka JMX → Prometheus → Grafana 파이프라인                       |
|                                                                   |
|  Kafka 브로커                                                      |
|    JMX 포트 :9999                                                  |
|        |                                                           |
|        v                                                           |
|  JMX Exporter (Java Agent)                                        |
|    - jmx_prometheus_javaagent.jar                                  |
|    - HTTP :7071/metrics                                            |
|        |                                                           |
|        v                                                           |
|  Prometheus                                                        |
|    - scrape_configs → kafka:7071                                   |
|    - 메트릭 저장 및 쿼리                                            |
|        |                                                           |
|        v                                                           |
|  Grafana 대시보드                                                   |
|    - 사전 구성된 Kafka 대시보드 템플릿                               |
+-------------------------------------------------------------------+
```

### JMX Exporter 설정

```yaml
# jmx-exporter-config.yaml
lowercaseOutputName: true
rules:
  - pattern: kafka.server<type=BrokerTopicMetrics, name=(\w+)><>(\w+)
    name: kafka_server_brokertopicmetrics_$1_$2
    type: GAUGE

  - pattern: kafka.server<type=ReplicaManager, name=(\w+)><>Value
    name: kafka_server_replicamanager_$1
    type: GAUGE

  - pattern: kafka.network<type=RequestMetrics, name=(\w+), request=(\w+)><>(\w+)
    name: kafka_network_requestmetrics_$1_$3
    labels:
      request: $2
```

```bash
# JMX Exporter를 Java Agent로 설정
export KAFKA_OPTS="-javaagent:/opt/jmx_prometheus_javaagent.jar=7071:/opt/jmx-config.yaml"
kafka-server-start.sh config/kraft/server.properties
```

---

## 11. 성능 튜닝

### OS 레벨 튜닝

```
+-------------------------------------------------------------------+
|  OS 파라미터              | 권장값           | 이유                |
+-------------------------------------------------------------------+
| vm.max_map_count          | 262144           | mmap 파일 수 제한   |
| vm.swappiness             | 1                | 스왑 최소화         |
| vm.dirty_ratio            | 60~80            | 쓰기 버퍼 비율      |
| vm.dirty_background_ratio | 5~10             | 백그라운드 플러시    |
| net.core.wmem_max         | 2097152 (2MB)    | 소켓 쓰기 버퍼      |
| net.core.rmem_max         | 2097152 (2MB)    | 소켓 읽기 버퍼      |
| net.ipv4.tcp_wmem         | 4096 65536 2097152| TCP 쓰기 버퍼     |
| net.ipv4.tcp_rmem         | 4096 65536 2097152| TCP 읽기 버퍼     |
| fs.file-max               | 100000+          | 파일 디스크립터 상한 |
| nofile (ulimit)           | 100000+          | 프로세스별 FD 상한  |
+-------------------------------------------------------------------+
```

### JVM 튜닝

```bash
# 프로덕션 권장 JVM 설정
export KAFKA_HEAP_OPTS="-Xmx6g -Xms6g"
export KAFKA_JVM_PERFORMANCE_OPTS="
  -server
  -XX:+UseG1GC
  -XX:MaxGCPauseMillis=20
  -XX:InitiatingHeapOccupancyPercent=35
  -XX:+ExplicitGCInvokesConcurrent
  -XX:MaxInlineLevel=15
  -Djava.awt.headless=true
"
```

### G1GC를 사용하는 이유

```
+-------------------------------------------------------------------+
| GC 알고리즘  | 특성                | Kafka에서의 적합성              |
+-------------------------------------------------------------------+
| G1GC         | 짧은 GC pause       | 가장 적합 -- 20ms 이하 목표   |
|              | 대용량 힙 최적화     | 6~8GB 힙에서 우수              |
|              | 예측 가능한 지연     | 프로듀서/컨슈머 지연 최소화     |
+-------------------------------------------------------------------+
| ZGC          | 극도로 짧은 pause    | 실험적 -- 매우 큰 힙에 유리    |
+-------------------------------------------------------------------+
| Parallel GC  | 높은 처리량          | GC pause가 길어 부적합         |
+-------------------------------------------------------------------+
```

### 브로커 튜닝

```
+-------------------------------------------------------------------+
|  설정                          | 기본값    | 권장 (프로덕션)       |
+-------------------------------------------------------------------+
|  num.network.threads           | 3         | 8 (CPU 코어에 비례)  |
|  num.io.threads                | 8         | 16 (디스크 수에 비례)|
|  socket.send.buffer.bytes      | 102400    | 1048576 (1MB)       |
|  socket.receive.buffer.bytes   | 102400    | 1048576 (1MB)       |
|  socket.request.max.bytes      | 104857600 | 유지 (100MB)        |
|  log.flush.interval.messages   | MAX       | 유지 (OS에 위임)    |
|  log.flush.interval.ms         | null      | 유지 (OS에 위임)    |
|  replica.fetch.min.bytes       | 1         | 유지                 |
|  replica.fetch.wait.max.ms     | 500       | 유지                 |
+-------------------------------------------------------------------+
```

### 프로듀서 튜닝

```
+-------------------------------------------------------------------+
|  설정                    | 처리량 우선        | 지연 우선           |
+-------------------------------------------------------------------+
|  acks                    | 1                  | all               |
|  batch.size              | 65536 (64KB)       | 16384 (16KB)      |
|  linger.ms               | 50~100             | 0~5               |
|  buffer.memory           | 67108864 (64MB)    | 33554432 (32MB)   |
|  compression.type        | lz4 / zstd         | none / lz4        |
|  max.in.flight.requests  | 5                  | 1 (순서 보장 시)   |
+-------------------------------------------------------------------+
```

### 컨슈머 튜닝

```
+-------------------------------------------------------------------+
|  설정                    | 처리량 우선        | 지연 우선           |
+-------------------------------------------------------------------+
|  fetch.min.bytes         | 1048576 (1MB)      | 1                 |
|  fetch.max.wait.ms       | 500                | 100               |
|  max.poll.records        | 1000               | 100               |
|  max.partition.fetch.bytes| 1048576 (1MB)     | 유지               |
|  session.timeout.ms      | 45000              | 10000             |
|  auto.offset.reset       | latest             | 용도에 따라        |
+-------------------------------------------------------------------+
```

---

## 12. 부하 테스트: Trogdor와 성능 도구

### 프로듀서 성능 테스트

```bash
# 기본 프로듀서 성능 테스트
kafka-producer-perf-test.sh \
  --topic perf-test \
  --num-records 1000000 \
  --record-size 1024 \
  --throughput -1 \
  --producer-props \
    bootstrap.servers=localhost:9092 \
    acks=1 \
    linger.ms=50 \
    batch.size=65536

# 출력:
# 1000000 records sent, 95238.1 records/sec (93.01 MB/sec),
# 2.5 ms avg latency, 150.0 ms max latency,
# 2 ms 50th, 3 ms 95th, 10 ms 99th, 25 ms 99.9th.
```

### 컨슈머 성능 테스트

**소스 파일**: `tools/src/main/java/org/apache/kafka/tools/ConsumerPerformance.java`

```bash
# 컨슈머 성능 테스트
kafka-consumer-perf-test.sh \
  --bootstrap-server localhost:9092 \
  --topic perf-test \
  --messages 1000000 \
  --threads 1

# 출력:
# start.time, end.time, data.consumed.in.MB, MB.sec,
# data.consumed.in.nMsg, nMsg.sec, rebalance.time.ms, ...
```

### E2E 레이턴시 측정

```bash
# End-to-End 레이턴시 측정
kafka-e2e-latency.sh \
  --bootstrap-server localhost:9092 \
  --topic latency-test \
  --num-messages 10000 \
  --producer-props acks=all \
  --consumer-props group.id=latency-group
```

### Trogdor 부하 테스트 프레임워크

**소스 파일**: `trogdor/src/main/java/org/apache/kafka/trogdor/`

Trogdor는 Kafka의 내장 분산 부하 테스트 프레임워크다.

```
Trogdor 아키텍처:
+-------------------------------------------------------------------+
|                                                                   |
|  Coordinator (코디네이터)                                          |
|    - REST API로 테스트 정의/시작/종료                              |
|    - 태스크를 Agent에 분배                                         |
|    |                                                              |
|    +---> Agent #1                                                 |
|    |       - ProduceBenchWorker: 프로듀서 부하 생성                |
|    |       - ConsumeBenchWorker: 컨슈머 부하 생성                  |
|    |       - RoundTripWorker: E2E 레이턴시 측정                   |
|    |                                                              |
|    +---> Agent #2                                                 |
|    |       - NetworkPartitionFaultWorker: 네트워크 단절 시뮬레이션  |
|    |       - ProcessStopFaultWorker: 프로세스 중단 시뮬레이션      |
|    |                                                              |
|    +---> Agent #3                                                 |
|            - 추가 부하 생성                                       |
|                                                                   |
+-------------------------------------------------------------------+
```

### Trogdor 태스크 유형

| 태스크 | 설명 |
|--------|------|
| ProduceBenchWorker | 설정 가능한 프로듀서 부하 |
| ConsumeBenchWorker | 설정 가능한 컨슈머 부하 |
| RoundTripWorker | 프로듀스-컨슘 왕복 레이턴시 |
| NetworkPartitionFaultWorker | 네트워크 파티션 시뮬레이션 |
| ProcessStopFaultWorker | 프로세스 중단 시뮬레이션 |
| ExternalCommandWorker | 외부 명령 실행 |

---

## 13. Docker 운영

**소스 파일**: `docker/`

### Apache Kafka Docker 이미지

```bash
# 공식 Docker 이미지로 단일 노드 실행
docker run -d \
  --name kafka \
  -p 9092:9092 \
  apache/kafka:latest

# 환경 변수로 설정 오버라이드
docker run -d \
  --name kafka \
  -p 9092:9092 \
  -e KAFKA_NODE_ID=1 \
  -e KAFKA_PROCESS_ROLES=broker,controller \
  -e KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093 \
  -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 \
  -e KAFKA_LOG_DIRS=/var/lib/kafka/data \
  apache/kafka:latest
```

### Docker Compose 멀티 노드

```yaml
# docker-compose.yml (3노드 KRaft 클러스터)
version: '3'
services:
  kafka-1:
    image: apache/kafka:latest
    environment:
      KAFKA_NODE_ID: 1
      KAFKA_PROCESS_ROLES: broker,controller
      KAFKA_LISTENERS: PLAINTEXT://:9092,CONTROLLER://:9093
      KAFKA_CONTROLLER_QUORUM_VOTERS: 1@kafka-1:9093,2@kafka-2:9093,3@kafka-3:9093
      KAFKA_INTER_BROKER_LISTENER_NAME: PLAINTEXT
      KAFKA_CONTROLLER_LISTENER_NAMES: CONTROLLER
      KAFKA_LOG_DIRS: /var/lib/kafka/data
      CLUSTER_ID: 'xtzWWN4bTjitpL3kfd9s5g'
    ports:
      - "9092:9092"
    volumes:
      - kafka-1-data:/var/lib/kafka/data

  kafka-2:
    image: apache/kafka:latest
    environment:
      KAFKA_NODE_ID: 2
      # ... (유사 설정)

  kafka-3:
    image: apache/kafka:latest
    environment:
      KAFKA_NODE_ID: 3
      # ... (유사 설정)

volumes:
  kafka-1-data:
  kafka-2-data:
  kafka-3-data:
```

### Docker 설정 오버라이드 규칙

```
환경 변수 → server.properties 매핑:

KAFKA_<PROPERTY> → <property>
  - 대문자 → 소문자
  - 밑줄 → 점

예:
  KAFKA_LOG_DIRS             → log.dirs
  KAFKA_NUM_PARTITIONS       → num.partitions
  KAFKA_LISTENER_SECURITY_PROTOCOL_MAP → listener.security.protocol.map
  KAFKA_AUTO_CREATE_TOPICS_ENABLE → auto.create.topics.enable
```

---

## 14. 롤링 업그레이드

### 업그레이드 절차

```
+-------------------------------------------------------------------+
|  Kafka 클러스터 롤링 업그레이드 (3.7 → 3.8)                        |
+-------------------------------------------------------------------+
|                                                                   |
|  1단계: 바이너리 업그레이드 (브로커별 순차)                          |
|                                                                   |
|    브로커 1: 중지 → 바이너리 교체 → 시작                            |
|    (나머지 브로커가 서비스 유지)                                     |
|                                                                   |
|    브로커 2: 중지 → 바이너리 교체 → 시작                            |
|    (나머지 브로커가 서비스 유지)                                     |
|                                                                   |
|    브로커 3: 중지 → 바이너리 교체 → 시작                            |
|                                                                   |
|    → 이 시점: 모든 브로커 3.8 바이너리, metadata.version 3.7       |
|                                                                   |
|  2단계: 기능 버전 업그레이드                                        |
|                                                                   |
|    kafka-features.sh --bootstrap-server localhost:9092 \           |
|      upgrade --metadata 3.8-IV0                                   |
|                                                                   |
|    → 이 시점: metadata.version 3.8-IV0, 새 기능 사용 가능          |
|                                                                   |
+-------------------------------------------------------------------+
```

### 안전한 롤링 재시작 체크리스트

```
브로커 재시작 전 확인사항:

  [ ] UnderReplicatedPartitions == 0
      (모든 파티션이 완전 복제 상태)

  [ ] min.insync.replicas 충족 가능한가?
      (예: RF=3, min.isr=2 → 1대씩 재시작 가능)

  [ ] 연결된 클라이언트 확인
      (graceful shutdown으로 클라이언트가 다른 브로커로 전환)

  [ ] controlled.shutdown.enable=true (기본값)
      (리더 파티션을 먼저 이전 후 종료)

브로커 재시작 후 확인사항:

  [ ] 브로커가 정상적으로 ISR에 복귀했는가?
  [ ] UnderReplicatedPartitions가 다시 0인가?
  [ ] 다음 브로커 재시작 진행
```

### Controlled Shutdown

```
controlled.shutdown.enable=true (기본값)

정상 종료 절차:
    |
    +---> 1. 브로커가 컨트롤러에 종료 의사 전달
    |         (BrokerHeartbeat: wantShutDown=true)
    |
    +---> 2. 컨트롤러가 해당 브로커의 리더 파티션을 이전
    |         - ISR의 다른 브로커를 새 리더로 선출
    |         - PartitionChangeRecord 생성
    |
    +---> 3. 모든 리더 이전 완료 확인
    |
    +---> 4. 컨트롤러: shouldShutDown=true 응답
    |
    +---> 5. 브로커 프로세스 종료
    |
    결과: 클라이언트 입장에서 무중단 (새 리더로 자동 전환)
```

---

## 15. 왜(Why) 이렇게 설계했는가

### Q: 왜 모든 도구가 쉘 스크립트 + Java 클래스 조합인가?

1. **플랫폼 독립성**: Java로 구현되어 JVM이 있는 모든 플랫폼에서 동작
2. **클라이언트 라이브러리 재사용**: AdminClient, Producer, Consumer를 그대로 사용
3. **일관성**: 모든 도구가 동일한 설정 체계(Properties)를 사용
4. **디버깅**: Java 스택 트레이스로 오류 추적 가능

### Q: 왜 로그 플러시를 OS에 위임하는가?

```
log.flush.interval.messages = Long.MAX_VALUE  (기본값)
log.flush.interval.ms = null                  (기본값)

이유:
1. OS 페이지 캐시가 디스크 I/O를 최적화
2. fsync를 자주 호출하면 성능 급감
3. Kafka의 복제(replication)가 이미 내구성 보장
4. acks=all + min.insync.replicas로 데이터 손실 방지
```

### Q: 왜 num.io.threads를 디스크 수에 비례하게 설정하는가?

```
I/O 스레드는 디스크 작업을 병렬 처리한다:
  - 로그 세그먼트 읽기/쓰기
  - 인덱스 파일 접근
  - 로그 컴팩션

디스크 수 × 2 정도가 적절:
  - 4개 디스크 → num.io.threads=8
  - SSD 사용 시 더 높게 설정 가능
  - 너무 많으면 컨텍스트 스위칭 오버헤드
```

### Q: 왜 Controlled Shutdown이 기본 활성화인가?

Controlled Shutdown 없이 브로커를 중지하면:

1. 리더 파티션이 갑자기 사라짐 → 클라이언트 에러
2. 컨트롤러가 하트비트 타임아웃까지 감지 못함 (수초~수십초)
3. ISR이 축소되어 min.insync.replicas 위반 가능

Controlled Shutdown으로:

1. 리더를 먼저 이전 → 클라이언트 무중단
2. 즉시 감지 → 빠른 전환
3. ISR 유지 → 가용성 보장

### Q: 왜 롤링 업그레이드에서 기능 버전을 별도로 올리는가?

```
바이너리 업그레이드와 기능 활성화를 분리하는 이유:

1. 안전한 롤백:
   바이너리만 업그레이드한 상태에서 문제가 생기면
   기능 버전을 올리지 않았으므로 안전하게 다운그레이드 가능

2. 호환성:
   새 바이너리가 이전 메타데이터 버전을 이해할 수 있어야 함
   반대 방향(새 메타데이터 → 구 바이너리)은 불가능할 수 있음

3. 점진적 활성화:
   모든 브로커가 새 버전임을 확인한 후에만 새 기능 활성화
```

### Q: 왜 kafka-storage.sh format이 필요한가?

KRaft 모드에서 스토리지 포맷이 필요한 이유:

1. **클러스터 ID 바인딩**: 브로커가 잘못된 클러스터에 합류하는 것을 방지
2. **메타데이터 초기화**: __cluster_metadata 파티션의 부트스트랩 레코드 생성
3. **버전 검증**: metadata.version을 명시적으로 설정하여 호환성 보장
4. **보안**: 포맷 없이는 브로커가 시작되지 않으므로 실수 방지

---

## 부록: 핵심 도구 빠른 참조

### 일상 운영 명령어

```bash
# 클러스터 상태 확인
kafka-metadata-quorum.sh --bootstrap-server :9092 describe --status
kafka-broker-api-versions.sh --bootstrap-server :9092

# 토픽 관리
kafka-topics.sh --bootstrap-server :9092 --list
kafka-topics.sh --bootstrap-server :9092 --describe --topic X
kafka-topics.sh --bootstrap-server :9092 --create --topic X --partitions 6 --replication-factor 3
kafka-topics.sh --bootstrap-server :9092 --delete --topic X

# 컨슈머 그룹 모니터링
kafka-consumer-groups.sh --bootstrap-server :9092 --list
kafka-consumer-groups.sh --bootstrap-server :9092 --describe --group G

# 설정 변경
kafka-configs.sh --bootstrap-server :9092 --describe --topic X
kafka-configs.sh --bootstrap-server :9092 --alter --topic X --add-config retention.ms=86400000

# 문제 진단
kafka-topics.sh --bootstrap-server :9092 --describe --under-replicated-partitions
kafka-log-dirs.sh --bootstrap-server :9092 --describe
kafka-dump-log.sh --files /data/kafka-logs/my-topic-0/00000000000000000000.log
```

### 긴급 상황 대응

```bash
# ISR 부족 파티션 확인
kafka-topics.sh --bootstrap-server :9092 --describe --under-replicated-partitions

# 리더 없는 파티션 확인
kafka-topics.sh --bootstrap-server :9092 --describe --unavailable-partitions

# 긴급 리더 선출 (데이터 손실 가능)
kafka-leader-election.sh --bootstrap-server :9092 \
  --election-type UNCLEAN --all-topic-partitions

# 컨슈머 그룹 오프셋 리셋 (재처리)
kafka-consumer-groups.sh --bootstrap-server :9092 \
  --group G --reset-offsets --to-earliest --execute --topic X

# 파티션 재할당 (브로커 교체 시)
kafka-reassign-partitions.sh --bootstrap-server :9092 \
  --reassignment-json-file plan.json --execute
```

---

## 부록: 주요 소스 파일 색인

| 파일 | 경로 | 설명 |
|------|------|------|
| kafka-server-start.sh | bin/kafka-server-start.sh | 브로커 시작 스크립트 |
| kafka-storage.sh | bin/kafka-storage.sh | 스토리지 포맷 |
| kafka-run-class.sh | bin/kafka-run-class.sh | 공통 Java 실행기 |
| TopicCommand.java | tools/.../tools/TopicCommand.java | 토픽 관리 |
| ConsumerPerformance.java | tools/.../tools/ConsumerPerformance.java | 컨슈머 성능 테스트 |
| ProducerPerformance.java | tools/.../tools/ProducerPerformance.java | 프로듀서 성능 테스트 |
| JmxTool.java | tools/.../tools/JmxTool.java | JMX 메트릭 도구 |
| MetadataQuorumCommand.java | tools/.../tools/MetadataQuorumCommand.java | 쿼럼 상태 |
| AclCommand.java | tools/.../tools/AclCommand.java | ACL 관리 |
| DelegationTokenCommand.java | tools/.../tools/DelegationTokenCommand.java | 위임 토큰 |
| LeaderElectionCommand.java | tools/.../tools/LeaderElectionCommand.java | 리더 선출 |
| LogDirsCommand.java | tools/.../tools/LogDirsCommand.java | 로그 디렉토리 |
| DumpLogSegments.java | tools/.../tools/DumpLogSegments.java | 로그 덤프 |
| EndToEndLatency.java | tools/.../tools/EndToEndLatency.java | E2E 레이턴시 |
| FeatureCommand.java | tools/.../tools/FeatureCommand.java | 기능 버전 관리 |
| GroupsCommand.java | tools/.../tools/GroupsCommand.java | 그룹 관리 |
| ConsoleProducer.java | tools/.../tools/ConsoleProducer.java | 콘솔 프로듀서 |
