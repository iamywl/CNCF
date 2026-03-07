# PoC-14: Kafka Streams 토폴로지 처리

## 개요

Kafka Streams의 토폴로지 기반 스트림 처리를 시뮬레이션한다. Source, Processor, Sink 노드로 구성된 DAG(Directed Acyclic Graph)를 통해 데이터가 변환/집계되어 흐르는 과정을 구현한다.

## 참조 소스코드

| 파일 | 핵심 로직 |
|------|----------|
| `streams/src/main/java/org/apache/kafka/streams/Topology.java` | `addSource()`, `addProcessor()`, `addSink()`로 DAG 구성 |
| `streams/src/main/java/org/apache/kafka/streams/processor/internals/StreamThread.java` | 토폴로지 실행 스레드 |
| `streams/src/main/java/org/apache/kafka/streams/processor/api/ProcessorContext.java` | `forward()` 메커니즘, StateStore 접근 |

## 핵심 알고리즘

```
Topology DAG:
  Source(topic) -> Processor1 -> Processor2 -> ... -> Sink(topic)
                       |
                   StateStore (optional)

StreamThread.runOnce():
  1. consumer.poll()로 레코드 읽기
  2. 해당 토픽의 SourceNode로 레코드 주입
  3. SourceNode -> ProcessorNode 체인 -> SinkNode로 forward
  4. offsets commit
```

## 시뮬레이션 시나리오

| 시나리오 | 설명 |
|---------|------|
| 1. Filter + Map | 선형 파이프라인: 필터링 후 변환하여 출력 |
| 2. Word Count | StateStore를 활용한 상태 유지 집계 처리 |
| 3. Branch | 조건에 따라 다른 싱크로 분기하는 토폴로지 |
| 4. Merge | 여러 소스 토픽의 레코드를 하나로 합류 |

## 실행

```bash
go run main.go
```

## 핵심 개념

- **SourceNode**: 입력 토픽에서 레코드를 소비하여 하위 노드로 전달
- **ProcessorNode**: 비즈니스 로직 수행 (filter, map, aggregate 등)
- **SinkNode**: 처리 결과를 출력 토픽에 기록
- **forward()**: 프로세서가 하위 노드로 레코드를 전달하는 메커니즘
- **StateStore**: 상태 유지 처리를 위한 로컬 키-값 저장소
