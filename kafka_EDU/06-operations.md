# Apache Kafka 운영

## 1. 배포 구성

### 1.1 KRaft 모드 배포

**최소 구성 (개발)**:
```properties
# server.properties
process.roles=broker,controller
node.id=1
controller.quorum.voters=1@localhost:9093
listeners=PLAINTEXT://:9092,CONTROLLER://:9093
log.dirs=/var/kafka-logs
```

**프로덕션 구성** (컨트롤러/브로커 분리):

```
컨트롤러 (3~5대):
  process.roles=controller
  node.id=100  (100, 101, 102)
  controller.quorum.voters=100@ctrl1:9093,101@ctrl2:9093,102@ctrl3:9093

브로커 (N대):
  process.roles=broker
  node.id=1  (1, 2, 3, ...)
  controller.quorum.voters=100@ctrl1:9093,101@ctrl2:9093,102@ctrl3:9093
```

### 1.2 초기 설정 절차

```bash
# 1. 클러스터 ID 생성
KAFKA_CLUSTER_ID="$(./bin/kafka-storage.sh random-uuid)"

# 2. 스토리지 포맷 (각 노드에서)
./bin/kafka-storage.sh format \
  -t $KAFKA_CLUSTER_ID \
  -c config/server.properties

# 3. 브로커 시작
./bin/kafka-server-start.sh config/server.properties
```

### 1.3 Docker 배포

```bash
docker run -p 9092:9092 apache/kafka:latest
```

## 2. 핵심 설정

### 2.1 브로커 설정

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `broker.id` / `node.id` | - | 브로커 고유 ID |
| `log.dirs` | `/tmp/kafka-logs` | 로그 디렉토리 (쉼표 구분, 다중 디스크) |
| `num.network.threads` | 3 | Processor 스레드 수 |
| `num.io.threads` | 8 | 요청 핸들러 스레드 수 |
| `socket.send.buffer.bytes` | 102400 | 소켓 전송 버퍼 |
| `socket.receive.buffer.bytes` | 102400 | 소켓 수신 버퍼 |
| `socket.request.max.bytes` | 104857600 (100MB) | 최대 요청 크기 |
| `num.partitions` | 1 | 토픽 기본 파티션 수 |
| `default.replication.factor` | 1 | 기본 복제 팩터 |
| `min.insync.replicas` | 1 | 최소 ISR 수 |
| `unclean.leader.election.enable` | false | ISR 외 리더 선출 허용 |

### 2.2 로그 설정

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `log.retention.hours` | 168 (7일) | 시간 기반 보존 |
| `log.retention.bytes` | -1 (무제한) | 크기 기반 보존 |
| `log.segment.bytes` | 1073741824 (1GB) | 세그먼트 크기 |
| `log.cleanup.policy` | delete | delete, compact, delete+compact |
| `log.cleaner.threads` | 1 | 컴팩션 스레드 수 |
| `log.flush.interval.messages` | Long.MAX | 플러시 간격 (메시지 수) |
| `log.flush.interval.ms` | Long.MAX | 플러시 간격 (시간) |

### 2.3 복제 설정

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `replica.lag.time.max.ms` | 30000 | ISR 이탈 판정 시간 |
| `replica.fetch.max.bytes` | 1048576 | 팔로워 Fetch 최대 크기 |
| `num.replica.fetchers` | 1 | 복제 Fetcher 스레드 수 |

### 2.4 동적 설정 변경

브로커 재시작 없이 설정 변경 가능:

```bash
# 브로커 레벨 설정 변경
./bin/kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --entity-type brokers --entity-name 1 \
  --add-config log.cleaner.threads=2

# 토픽 레벨 설정 변경
./bin/kafka-configs.sh --bootstrap-server localhost:9092 \
  --alter --entity-type topics --entity-name my-topic \
  --add-config retention.ms=86400000
```

## 3. CLI 도구

### 3.1 토픽 관리

```bash
# 토픽 생성
./bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --create --topic orders \
  --partitions 6 --replication-factor 3

# 토픽 목록
./bin/kafka-topics.sh --bootstrap-server localhost:9092 --list

# 토픽 상세 정보
./bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --describe --topic orders

# 토픽 삭제
./bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --delete --topic orders

# 파티션 추가
./bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --alter --topic orders --partitions 12
```

### 3.2 메시지 생산/소비

```bash
# 콘솔 프로듀서
./bin/kafka-console-producer.sh --bootstrap-server localhost:9092 \
  --topic orders

# 콘솔 컨슈머
./bin/kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic orders --from-beginning

# 특정 파티션/오프셋부터
./bin/kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic orders --partition 0 --offset 100
```

### 3.3 컨슈머 그룹 관리

```bash
# 그룹 목록
./bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --list

# 그룹 상세 (lag 확인)
./bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --describe --group my-group

# 오프셋 리셋
./bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --topic orders \
  --reset-offsets --to-earliest --execute
```

### 3.4 클러스터 관리

```bash
# 브로커 목록
./bin/kafka-metadata.sh --snapshot /var/kafka-logs/__cluster_metadata-0/00000000000000000000.log \
  --broker-list

# 메타데이터 쿼럼 정보
./bin/kafka-metadata.sh --bootstrap-controller localhost:9093 \
  --describe --status

# 로그 디렉토리 정보
./bin/kafka-log-dirs.sh --bootstrap-server localhost:9092 \
  --describe --broker-list 1,2,3
```

## 4. 모니터링

### 4.1 JMX 메트릭

Kafka는 모든 메트릭을 JMX로 노출한다:

```bash
# JMX 활성화
KAFKA_JMX_OPTS="-Djcom.sun.management.jmxremote \
  -Djcom.sun.management.jmxremote.port=9999 \
  -Djcom.sun.management.jmxremote.authenticate=false"
```

### 4.2 핵심 모니터링 메트릭

| 메트릭 | MBean | 의미 |
|--------|-------|------|
| **메시지 인입률** | `kafka.server:type=BrokerTopicMetrics,name=MessagesInPerSec` | 초당 메시지 수 |
| **바이트 인입률** | `kafka.server:type=BrokerTopicMetrics,name=BytesInPerSec` | 초당 인입 바이트 |
| **바이트 유출률** | `kafka.server:type=BrokerTopicMetrics,name=BytesOutPerSec` | 초당 유출 바이트 |
| **요청 지연** | `kafka.network:type=RequestMetrics,name=TotalTimeMs,request=Produce` | 요청 처리 시간 |
| **ISR 축소** | `kafka.server:type=ReplicaManager,name=IsrShrinksPerSec` | ISR 축소 빈도 |
| **ISR 확장** | `kafka.server:type=ReplicaManager,name=IsrExpandsPerSec` | ISR 확장 빈도 |
| **언더 복제** | `kafka.server:type=ReplicaManager,name=UnderReplicatedPartitions` | 복제 부족 파티션 수 |
| **오프라인 파티션** | `kafka.controller:type=KafkaController,name=OfflinePartitionsCount` | 오프라인 파티션 수 |
| **활성 컨트롤러** | `kafka.controller:type=KafkaController,name=ActiveControllerCount` | 1이어야 정상 |
| **요청 큐 크기** | `kafka.network:type=RequestChannel,name=RequestQueueSize` | 대기 중 요청 수 |
| **로그 플러시 시간** | `kafka.log:type=LogFlushStats,name=LogFlushRateAndTimeMs` | 디스크 플러시 지연 |
| **네트워크 유휴** | `kafka.network:type=Processor,name=IdlePercent` | Processor 유휴 비율 |

### 4.3 알림 기준

| 지표 | 경고 | 위험 |
|------|------|------|
| UnderReplicatedPartitions | > 0 (5분 이상) | > 0 (15분 이상) |
| OfflinePartitionsCount | > 0 | > 0 (즉시) |
| RequestQueueSize | > 100 | > 500 |
| IsrShrinksPerSec | > 0 (지속) | 급증 |
| NetworkProcessor IdlePercent | < 30% | < 10% |
| 컨슈머 Lag | 증가 추세 | 급증 |

## 5. 성능 튜닝

### 5.1 OS 레벨

```bash
# 파일 디스크립터 제한 증가
ulimit -n 100000

# 가상 메모리 맵 수 증가
sysctl -w vm.max_map_count=262144

# 디스크 I/O 스케줄러 (SSD)
echo deadline > /sys/block/sda/queue/scheduler

# 네트워크 버퍼
sysctl -w net.core.rmem_max=16777216
sysctl -w net.core.wmem_max=16777216
```

### 5.2 JVM 튜닝

```bash
# 힙 메모리 (6GB 권장 for 브로커)
KAFKA_HEAP_OPTS="-Xmx6g -Xms6g"

# GC 설정 (G1GC 권장)
KAFKA_JVM_PERFORMANCE_OPTS="-XX:+UseG1GC \
  -XX:MaxGCPauseMillis=20 \
  -XX:InitiatingHeapOccupancyPercent=35 \
  -XX:G1HeapRegionSize=16M"
```

### 5.3 브로커 튜닝

| 설정 | 권장값 | 이유 |
|------|--------|------|
| `num.network.threads` | CPU 코어 수 | NIO 처리 병렬성 |
| `num.io.threads` | CPU 코어 × 2 | I/O 바운드 작업 |
| `socket.send.buffer.bytes` | 1048576 | 네트워크 처리량 |
| `socket.receive.buffer.bytes` | 1048576 | 네트워크 처리량 |
| `num.replica.fetchers` | 2~4 | 복제 병렬성 |
| `log.flush.interval.messages` | OS에 위임 | OS 페이지 캐시 활용 |

### 5.4 프로듀서 튜닝

| 설정 | 권장값 | 이유 |
|------|--------|------|
| `batch.size` | 65536~131072 | 배치 크기 증가 → 처리량 향상 |
| `linger.ms` | 5~50 | 배치 채움 시간 |
| `compression.type` | lz4 또는 zstd | 네트워크/디스크 절약 |
| `buffer.memory` | 67108864 (64MB) | 프로듀서 버퍼 풀 |
| `acks` | all (-1) | 데이터 안전성 |

### 5.5 컨슈머 튜닝

| 설정 | 권장값 | 이유 |
|------|--------|------|
| `fetch.min.bytes` | 1~65536 | 배치 크기 최적화 |
| `fetch.max.wait.ms` | 500 | 대기 시간 |
| `max.poll.records` | 500~1000 | 처리 배치 크기 |
| `max.partition.fetch.bytes` | 1048576 | 파티션당 최대 Fetch |

## 6. 트러블슈팅

### 6.1 일반적인 문제

| 증상 | 원인 | 해결 |
|------|------|------|
| UnderReplicatedPartitions > 0 | 팔로워 지연, 디스크 느림 | 디스크 I/O 확인, 복제 설정 조정 |
| 컨슈머 Lag 증가 | 처리 속도 < 생산 속도 | 파티션/컨슈머 추가, 배치 크기 조정 |
| 프로듀서 타임아웃 | 리더 없음, 네트워크 문제 | 클러스터 상태 확인, ISR 확인 |
| OOM (OutOfMemory) | 힙 부족, 큰 메시지 | 힙 증가, max.message.bytes 조정 |
| 디스크 풀 | 보존 정책 부적절 | retention.ms/bytes 조정, 디스크 추가 |
| 리밸런스 빈번 | session.timeout 너무 짧음 | 타임아웃 증가, 정적 멤버십 사용 |

### 6.2 로그 확인

```bash
# 브로커 로그
tail -f /var/kafka-logs/server.log

# 컨트롤러 로그
tail -f /var/kafka-logs/controller.log

# GC 로그
tail -f /var/kafka-logs/kafkaServer-gc.log
```

### 6.3 복구 절차

**리더 없는 파티션 복구**:
```bash
# 파티션 상태 확인
./bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --describe --under-replicated-partitions

# 필요시 preferred 리더 선출
./bin/kafka-leader-election.sh --bootstrap-server localhost:9092 \
  --election-type PREFERRED --all-topic-partitions

# unclean 리더 선출 (데이터 손실 가능)
./bin/kafka-leader-election.sh --bootstrap-server localhost:9092 \
  --election-type UNCLEAN --topic my-topic --partition 0
```

## 7. 보안 설정

### 7.1 SSL/TLS

```properties
# server.properties
listeners=SSL://:9093
ssl.keystore.location=/path/to/kafka.server.keystore.jks
ssl.keystore.password=password
ssl.key.password=password
ssl.truststore.location=/path/to/kafka.server.truststore.jks
ssl.truststore.password=password
ssl.client.auth=required
```

### 7.2 SASL

```properties
# SASL/SCRAM 설정
listeners=SASL_PLAINTEXT://:9092
sasl.mechanism.inter.broker.protocol=SCRAM-SHA-256
sasl.enabled.mechanisms=SCRAM-SHA-256
```

### 7.3 ACL

```bash
# ACL 추가
./bin/kafka-acls.sh --bootstrap-server localhost:9092 \
  --add --allow-principal User:alice \
  --operation Read --topic orders

# ACL 조회
./bin/kafka-acls.sh --bootstrap-server localhost:9092 \
  --list --topic orders
```

## 8. 백업과 복구

### 8.1 토픽 데이터 백업

```bash
# MirrorMaker 2.0으로 클러스터 간 복제
./bin/connect-mirror-maker.sh config/mm2.properties
```

### 8.2 메타데이터 백업

KRaft 모드에서는 `__cluster_metadata` 토픽이 메타데이터를 담고 있어 별도 백업이 필요 없다. 컨트롤러 노드 3~5대의 복제가 곧 백업이다.

### 8.3 오프셋 내보내기/가져오기

```bash
# 현재 오프셋 확인
./bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --describe --group my-group

# 오프셋 리셋 (dry-run)
./bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --topic orders \
  --reset-offsets --to-datetime 2026-03-01T00:00:00.000 --dry-run

# 오프셋 리셋 (실행)
./bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group my-group --topic orders \
  --reset-offsets --to-datetime 2026-03-01T00:00:00.000 --execute
```
