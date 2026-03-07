# PoC-05: Kafka NIO 스타일 네트워크 아키텍처

## 개요

Kafka의 NIO 기반 네트워크 레이어를 Go로 시뮬레이션한다. Kafka는 Java NIO의 Selector를 활용한 Reactor 패턴으로 소수의 스레드가 수천 개의 연결을 효율적으로 처리한다.

## 실행 방법

```bash
go run main.go
```

## Kafka 소스코드 참조

| 컴포넌트 | 원본 파일 | 설명 |
|----------|----------|------|
| SocketServer | `core/src/main/scala/kafka/network/SocketServer.scala` | 네트워크 레이어 전체를 관리 |
| Acceptor | `SocketServer.scala` (내부 클래스) | 새 연결을 수락하는 스레드 |
| Processor | `SocketServer.scala` (내부 클래스) | 연결별 I/O 다중화 스레드 |
| RequestChannel | `core/src/main/scala/kafka/network/RequestChannel.scala` | 요청/응답 큐 |
| Selector | `clients/src/main/java/.../common/network/Selector.java` | Java NIO Selector 래퍼 |

## 시뮬레이션하는 핵심 개념

### 1. Acceptor → Processor 라운드로빈 분배

```
Acceptor가 연결을 수락하면 currentProcessorIndex를 증가시키며
라운드로빈으로 Processor를 선택하여 newConnections 큐에 전달한다.
```

### 2. Processor의 I/O 다중화

```
각 Processor는 자체 NIO Selector를 가지며 다음을 반복한다:
  configureNewConnections()  → 새 연결 등록
  processNewResponses()      → 응답 쓰기 등록
  poll()                     → Selector.poll()로 I/O 이벤트 처리
  processCompletedReceives() → 수신 완료된 요청을 RequestChannel에 전달
  processCompletedSends()    → 전송 완료 후 정리
```

### 3. Size-Prefixed 메시지 프레이밍

```
모든 Kafka 메시지는 4바이트 크기 헤더가 선행한다:
  [4 bytes: message length][N bytes: payload]
이를 통해 TCP 스트림에서 메시지 경계를 정확하게 식별한다.
```

### 4. RequestChannel 큐

```
Processor가 수신한 요청을 Handler가 처리할 수 있도록 연결하는 버퍼링된 큐.
maxQueuedRequests 설정으로 큐 크기를 제한하여 메모리 사용량을 제어한다.
각 Processor마다 별도의 응답 큐가 존재한다.
```

### 5. CorrelationID 매칭

```
모든 요청에 correlationID가 포함되며, 응답에도 동일한 correlationID가 반환된다.
클라이언트는 이를 통해 비동기로 전송한 요청과 응답을 매칭한다.
```

## 아키텍처 다이어그램

```
Client-1 ──┐
Client-2 ──┼──→ [Acceptor] ──라운드로빈──→ [Processor-0] ──┐
Client-3 ──┤                              [Processor-1] ──┼──→ [RequestChannel] ──→ [Handler-0]
Client-4 ──┤                              [Processor-2] ──┘                        [Handler-1]
Client-5 ──┘                                   ↑                                      |
                                               └────── Response Queue ←────────────────┘
```

## Go 시뮬레이션 매핑

| Kafka (Java) | Go PoC |
|--------------|--------|
| Java NIO Selector | goroutine per connection + channel |
| Thread | goroutine |
| ArrayBlockingQueue | buffered channel |
| SelectionKey.OP_ACCEPT | net.Listener.Accept() |
| SelectionKey.OP_READ | io.ReadFull() in goroutine |
| ByteBuffer | []byte slice |
