# PoC-15: Kafka Connect Source/Sink 런타임

## 개요

Kafka Connect의 Source/Sink 런타임을 시뮬레이션한다. Connector-Task 구조, SourceTask.poll()/SinkTask.put() 패턴, SMT(Single Message Transform) 체인, Worker의 태스크 생명주기 관리를 구현한다.

## 참조 소스코드

| 파일 | 핵심 로직 |
|------|----------|
| `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/Worker.java` | 커넥터/태스크 생명주기 관리 |
| `connect/api/src/main/java/org/apache/kafka/connect/source/SourceTask.java` | `poll()` - 외부 시스템에서 레코드 배치 가져오기 |
| `connect/api/src/main/java/org/apache/kafka/connect/sink/SinkTask.java` | `put()` - Kafka 레코드를 외부 시스템에 쓰기 |
| `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/TransformationChain.java` | SMT 체인 순차 적용, null 반환 시 필터링 |

## 핵심 알고리즘

```
Worker 루프:
  1. SourceTask.poll() -> SourceRecord 배치
  2. TransformationChain.apply(record) -> 변환/필터링
  3. Producer.send(record) -> Kafka 토픽에 프로듀스
  4. SourceTask.commit() -> 소스 오프셋 커밋

  5. Consumer.poll() -> SinkRecord 배치
  6. SinkTask.put(records) -> 외부 시스템에 쓰기
  7. SinkTask.flush() -> 배치 커밋

TransformationChain.apply():
  for each transform in chain:
    record = transform.apply(record)
    if record == null: break  // 필터링
  return record
```

## 시뮬레이션 시나리오

| 시나리오 | 설명 |
|---------|------|
| 1. 기본 파이프라인 | FileSource -> Kafka -> ConsoleSink 엔드투엔드 전달 |
| 2. SMT 체인 | FilterByKey -> AddPrefix -> ToUpperCase 변환 체인 |
| 3. 오프셋 추적 | 소스 오프셋 저장으로 재시작 시 중복 방지 |

## 실행

```bash
go run main.go
```

## 핵심 개념

- **Connector**: 설정 관리 및 TaskConfig 생성 (데이터 처리 자체는 하지 않음)
- **SourceTask.poll()**: 외부 시스템에서 SourceRecord 배치를 가져옴
- **SinkTask.put()**: Kafka에서 받은 SinkRecord를 외부 시스템에 기록
- **TransformationChain**: SMT를 순서대로 적용, null 반환 시 필터링
- **Worker**: 전체 생명주기 오케스트레이션 (시작/중지/오프셋 커밋)
- **Source Offset**: 소스 시스템의 위치를 추적하여 exactly-once 의미론 지원
