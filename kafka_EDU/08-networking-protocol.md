# 08. Kafka 네트워킹과 프로토콜 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Java NIO 기반 아키텍처](#2-java-nio-기반-아키텍처)
3. [SocketServer 구조](#3-socketserver-구조)
4. [Acceptor: 연결 수락](#4-acceptor-연결-수락)
5. [Processor: NIO 멀티플렉싱](#5-processor-nio-멀티플렉싱)
6. [Selector.java: I/O 엔진](#6-selectorjava-io-엔진)
7. [KafkaChannel과 NetworkReceive](#7-kafkachannel과-networkreceive)
8. [RequestChannel: 큐 시스템](#8-requestchannel-큐-시스템)
9. [KafkaRequestHandler: 요청 처리 스레드](#9-kafkarequesthandler-요청-처리-스레드)
10. [채널 뮤팅 메커니즘](#10-채널-뮤팅-메커니즘)
11. [프로토콜 프레임 포맷](#11-프로토콜-프레임-포맷)
12. [API 버전 협상](#12-api-버전-협상)
13. [Zero-Copy 전송](#13-zero-copy-전송)
14. [설계 결정의 이유 (Why)](#14-설계-결정의-이유-why)

---

## 1. 개요

Kafka의 네트워크 계층은 높은 처리량의 메시지 브로커를 지원하기 위해 **Java NIO(Non-blocking I/O)**
기반의 다단계 파이프라인으로 설계되었다. 이 계층은 수만 개의 동시 연결을 적은 수의 스레드로
효율적으로 처리한다.

### 핵심 설계 원칙

1. **Reactor 패턴**: 소수의 I/O 스레드가 다수의 연결을 멀티플렉싱
2. **관심사 분리**: 네트워크 I/O와 비즈니스 로직을 분리
3. **배압(Backpressure)**: 요청 큐와 채널 뮤팅으로 부하 제어
4. **Zero-Copy**: OS의 sendfile 시스템 콜을 활용한 데이터 전송

### 핵심 소스 파일

| 파일 | 경로 | 역할 |
|------|------|------|
| SocketServer.scala | `core/src/main/scala/kafka/network/SocketServer.scala` | 서버 소켓 관리 |
| RequestChannel.scala | `core/src/main/scala/kafka/network/RequestChannel.scala` | 요청/응답 큐 |
| KafkaRequestHandler.scala | `core/src/main/scala/kafka/server/KafkaRequestHandler.scala` | 요청 처리 스레드 풀 |
| Selector.java | `clients/src/main/java/org/apache/kafka/common/network/Selector.java` | NIO 셀렉터 래퍼 |
| KafkaChannel.java | `clients/src/main/java/org/apache/kafka/common/network/KafkaChannel.java` | 채널 추상화 |
| NetworkReceive.java | `clients/src/main/java/org/apache/kafka/common/network/NetworkReceive.java` | 수신 메시지 |
| NetworkSend.java | `clients/src/main/java/org/apache/kafka/common/network/NetworkSend.java` | 송신 메시지 |

---

## 2. Java NIO 기반 아키텍처

### 2.1 왜 Java NIO인가?

전통적인 Java I/O(블로킹 I/O)에서는 연결당 하나의 스레드가 필요하다. 10만 개의 동시
연결을 처리하려면 10만 개의 스레드가 필요하고, 이는 컨텍스트 스위칭 비용과 메모리 소비로
인해 실용적이지 않다.

```
블로킹 I/O:                         논블로킹 I/O (NIO):
+--------+  +--------+              +--------+
| Thread | | Thread |  ...x10만     | Thread | <-- 1개의 스레드가
+---+----+  +---+----+              +---+----+     N개 연결 관리
    |           |                       |
    v           v                       v
 [Conn1]    [Conn2]                [Selector]
                                    /  |   \
                                   /   |    \
                               [Conn1][Conn2]...[ConnN]
```

Java NIO의 핵심 컴포넌트:

- **`java.nio.channels.Selector`**: I/O 이벤트를 감시하는 멀티플렉서
- **`SocketChannel`**: 비블로킹 소켓 채널
- **`SelectionKey`**: 채널의 관심 이벤트(READ, WRITE, ACCEPT)를 등록
- **`ByteBuffer`**: 오프-힙 직접 버퍼 또는 힙 버퍼

### 2.2 전체 스레드 모델

```
                              Kafka 브로커 네트워크 아키텍처
                              ============================

  클라이언트들                    브로커
  ==========                    ======

  +--------+
  | Client |--+
  +--------+  |    +-------------------------------------------+
              |    |  SocketServer                              |
  +--------+  |    |                                            |
  | Client |--+--->| [Acceptor Thread]                          |
  +--------+  |    |      |                                     |
              |    |      | (라운드로빈으로 새 연결 배분)           |
  +--------+  |    |      |                                     |
  | Client |--+    |   +--+--+--+--+--+                         |
  +--------+       |   |  P0 |  P1 |  P2 |  ... (Processor)    |
                   |   +--+--+--+--+--+--+                     |
                   |      |     |     |                         |
                   |      v     v     v                         |
                   |   +----------------------------------+     |
                   |   |     RequestChannel               |     |
                   |   |  [========= requestQueue ========]     |
                   |   |  (ArrayBlockingQueue, 공유)       |     |
                   |   +----------------------------------+     |
                   |      |     |     |                         |
                   |      v     v     v                         |
                   |   +--+--+--+--+--+--+                     |
                   |   |  H0 |  H1 |  H2 |  ... (Handler)     |
                   |   +--+--+--+--+--+--+                     |
                   |      |     |     |                         |
                   |      v     v     v                         |
                   |   [KafkaApis: 비즈니스 로직 처리]            |
                   |      |     |     |                         |
                   |      v     v     v                         |
                   |   +----------------------------------+     |
                   |   |  각 Processor의 responseQueue    |     |
                   |   |  P0:[resp] P1:[resp] P2:[resp]   |     |
                   |   +----------------------------------+     |
                   |      |     |     |                         |
                   |      v     v     v                         |
                   |   Processor가 응답을 클라이언트에 전송       |
                   +-------------------------------------------+
```

---

## 3. SocketServer 구조

### 3.1 클래스 정의

```scala
// SocketServer.scala (라인 71~)
class SocketServer(
  val config: KafkaConfig,
  val metrics: Metrics,
  val time: Time,
  val credentialProvider: CredentialProvider,
  val apiVersionManager: ApiVersionManager,
  val socketFactory: ServerSocketFactory = ServerSocketFactory.INSTANCE,
  val connectionDisconnectListeners: Seq[ConnectionDisconnectListener] = Seq.empty
) extends Logging with BrokerReconfigurable
```

### 3.2 Data-Plane과 Control-Plane

Kafka는 두 가지 네트워크 평면을 분리한다:

```scala
// SocketServer.scala (라인 98~)
// data-plane
private[network] val dataPlaneAcceptors = new ConcurrentHashMap[Endpoint, DataPlaneAcceptor]()
val dataPlaneRequestChannel = new RequestChannel(maxQueuedRequests, time, ...)
```

```
Data-Plane vs Control-Plane:

  +-------------------------------------------+
  | Data-Plane (데이터 경로)                    |
  |  - Produce/Fetch 요청                      |
  |  - 클라이언트 ↔ 브로커 통신                  |
  |  - 브로커 ↔ 브로커 복제                      |
  |  - N개의 Acceptor (리스너당 1개)             |
  |  - M개의 Processor (리스너당 설정 가능)       |
  +-------------------------------------------+

  +-------------------------------------------+
  | Control-Plane (제어 경로)                   |
  |  - 컨트롤러 ↔ 브로커 통신                    |
  |  - LeaderAndIsr, UpdateMetadata 등          |
  |  - 별도의 리스너/Acceptor/Processor          |
  |  - 데이터 트래픽에 영향 받지 않음             |
  +-------------------------------------------+
```

**왜 두 개의 평면으로 분리하는가?**

데이터 트래픽(Produce/Fetch)이 폭증할 때 컨트롤러 요청이 지연되면 리더 선출이나
ISR 변경 같은 중요한 작업이 지연된다. 평면을 분리하면 데이터 트래픽과 무관하게
컨트롤 요청이 처리된다.

### 3.3 연결 할당량 (Connection Quotas)

```scala
// SocketServer.scala (라인 103)
val connectionQuotas = new ConnectionQuotas(config, time, metrics)
```

IP별, 브로커별 최대 연결 수를 제한하여 단일 클라이언트가 과도한 리소스를 사용하는 것을
방지한다.

---

## 4. Acceptor: 연결 수락

### 4.1 DataPlaneAcceptor

```scala
// SocketServer.scala (라인 360~)
class DataPlaneAcceptor(socketServer: SocketServer, ...)
```

```scala
// SocketServer.scala (라인 458~)
private[kafka] abstract class Acceptor(val socketServer: SocketServer, ...)
```

### 4.2 Acceptor의 동작

Acceptor는 전용 스레드에서 실행되며, `ServerSocketChannel`의 `ACCEPT` 이벤트를 감시한다.

```
Acceptor 실행 루프:

  while (isRunning) {
      // 1. NIO Selector에서 ACCEPT 이벤트 대기
      nioSelector.select(500ms)

      // 2. 새 연결 수락
      for (key <- selectedKeys if key.isAcceptable) {
          socketChannel = serverChannel.accept()
          socketChannel.configureBlocking(false)  // 비블로킹 모드

          // 3. 연결 할당량 확인
          connectionQuotas.inc(...)

          // 4. 라운드로빈으로 Processor 선택 및 할당
          processor = roundRobinSelectProcessor()
          processor.accept(socketChannel)
      }
  }
```

```
Acceptor → Processor 라운드로빈 분배:

  새 연결 도착 순서: C1, C2, C3, C4, C5, C6 ...

  Processor 0 ← C1, C4, ...
  Processor 1 ← C2, C5, ...
  Processor 2 ← C3, C6, ...

  → 균등 분배로 Processor 간 부하 균형
```

**왜 Acceptor를 별도 스레드로 분리하는가?**

새 TCP 연결을 수락하는 작업(`accept()`)은 빈번하지 않지만, 기존 연결의 I/O를
처리하는 Processor와 같은 스레드에 있으면 `accept()`가 I/O 처리를 지연시키거나
그 반대가 될 수 있다. 분리하면 새 연결 수락과 기존 연결 I/O가 독립적으로 진행된다.

---

## 5. Processor: NIO 멀티플렉싱

### 5.1 Processor 클래스

```scala
// SocketServer.scala (라인 797~)
private[kafka] class Processor(
  // 각 Processor가 자체 NIO Selector를 보유
  // 할당된 연결들의 READ/WRITE 이벤트를 감시
)
```

### 5.2 Processor 실행 루프

```
Processor 실행 루프:

  while (isRunning) {
      // 1. Acceptor가 할당한 새 연결 등록
      configureNewConnections()
        → 각 소켓 채널을 자신의 NIO Selector에 등록
        → OP_READ 관심 이벤트 설정

      // 2. 응답 큐에서 응답 가져와서 처리
      processNewResponses()
        → responseQueue에서 응답 꺼내기
        → 해당 채널에 Send 객체 설정
        → OP_WRITE 관심 이벤트 추가

      // 3. NIO Selector poll (핵심)
      poll()
        → selector.poll(300ms)
        → READ 이벤트: 데이터 읽기 → NetworkReceive 완성
        → WRITE 이벤트: 데이터 쓰기 → NetworkSend 완성

      // 4. 완료된 수신 처리
      processCompletedReceives()
        → RequestChannel.requestQueue에 요청 추가

      // 5. 완료된 송신 처리
      processCompletedSends()
        → 채널 뮤트 해제

      // 6. 연결 해제 처리
      processDisconnected()
  }
```

```
Processor 내부 상태:

  +--------------------------------------------+
  | Processor #0                               |
  |                                            |
  | NIO Selector                               |
  |  +------+------+------+------+             |
  |  | Key0 | Key1 | Key2 | Key3 | ...         |
  |  +--+---+--+---+--+---+--+---+             |
  |     |      |      |      |                 |
  |     v      v      v      v                 |
  |  [Chan0] [Chan1] [Chan2] [Chan3]           |
  |                                            |
  | newConnections: [SocketChannel, ...]       |
  | responseQueue: [Response, ...]             |
  | inflightResponses: {connId -> Response}    |
  +--------------------------------------------+
```

---

## 6. Selector.java: I/O 엔진

### 6.1 핵심 구조

Kafka의 `Selector`는 `java.nio.channels.Selector`를 래핑한 클래스로, 클라이언트와
서버 양쪽에서 사용된다.

```java
// Selector.java (라인 88~)
public class Selector implements Selectable, AutoCloseable {
    private final java.nio.channels.Selector nioSelector;
    private final Map<String, KafkaChannel> channels;            // 활성 채널들
    private final Set<KafkaChannel> explicitlyMutedChannels;     // 명시적 뮤트
    private boolean outOfMemory;                                  // 메모리 부족 상태
    private final List<NetworkSend> completedSends;              // 전송 완료 목록
    private final LinkedHashMap<String, NetworkReceive> completedReceives; // 수신 완료
    private final Map<String, KafkaChannel> closingChannels;     // 닫는 중인 채널
    private final Map<String, ChannelState> disconnected;        // 연결 해제된 채널
    private final List<String> connected;                        // 새로 연결된 채널
}
```

### 6.2 poll() 메서드

`poll()`은 Selector의 핵심이다. 하나의 호출로 여러 채널의 I/O를 처리한다.

```java
// Selector.java (라인 420~)
// 주석 발췌:
// Do whatever I/O can be done on each connection without blocking.
// At most one entry is added to "completedReceives" for a channel in each poll.
```

```
poll() 실행 흐름:

  poll(timeout)
      |
      +--- 1. clear() - 이전 poll 결과 초기화
      |        completedSends.clear()
      |        completedReceives.clear()
      |
      +--- 2. nioSelector.select(timeout) 또는 selectNow()
      |        → OS에서 준비된 I/O 이벤트 가져오기
      |
      +--- 3. pollSelectionKeys(selectedKeys)
      |        for each readyKey:
      |          |
      |          +-- isReadable?
      |          |     → attemptRead(channel)
      |          |        → channel.read() → NetworkReceive에 데이터 축적
      |          |        → 완료되면 completedReceives에 추가
      |          |
      |          +-- isWritable?
      |          |     → attemptWrite(channel)
      |          |        → channel.write() → 바이트 전송
      |          |        → 완료되면 completedSends에 추가
      |          |
      |          +-- isConnectable?
      |                → channel.finishConnect()
      |                → connected 목록에 추가
      |
      +--- 4. addToCompletedReceives()
               → 버퍼링된 수신 데이터를 completedReceives로 이동
               → 채널당 최대 1개 (순서 보장)
```

### 6.3 채널당 최대 1개 수신 제한

```java
// Selector.java (라인 434~)
// At most one entry is added to "completedReceives" for a channel
// in each poll. This is necessary to guarantee that requests are
// processed in order for each channel.
```

```
채널당 1개 수신 제한의 효과:

  채널 A에서 요청 3개가 버퍼에 있을 때:
    [Req1] [Req2] [Req3]

  poll() 호출 1: completedReceives = {A: Req1}
    → Req1 처리 시작, 채널 A 뮤트
  poll() 호출 2: completedReceives = {A: Req2} (Req1 응답 후 뮤트 해제 시)
    → Req2 처리 시작
  poll() 호출 3: completedReceives = {A: Req3}
    → Req3 처리 시작

  → 채널 A의 요청은 반드시 순서대로 처리됨
```

---

## 7. KafkaChannel과 NetworkReceive

### 7.1 KafkaChannel

```java
// KafkaChannel.java (라인 67~)
public class KafkaChannel implements AutoCloseable {
    // TransportLayer: 실제 네트워크 I/O (PlaintextTransportLayer 또는 SslTransportLayer)
    // Authenticator: SASL/SSL 인증
    // MemoryPool: 수신 버퍼 메모리 관리
    // NetworkReceive: 현재 수신 중인 데이터
    // Send: 현재 전송 중인 데이터
    // ChannelMuteState: 뮤트 상태
}
```

```
KafkaChannel 내부 구조:

  +--------------------------------------------+
  | KafkaChannel                               |
  |                                            |
  |  +------------------+                      |
  |  | TransportLayer   |  (Plaintext/SSL)     |
  |  | - SocketChannel  |                      |
  |  | - SelectionKey   |                      |
  |  +------------------+                      |
  |                                            |
  |  +------------------+                      |
  |  | Authenticator    |  (SASL/PLAINTEXT)    |
  |  +------------------+                      |
  |                                            |
  |  +------------------+                      |
  |  | NetworkReceive   |  (현재 수신 중)       |
  |  | - size: 4 bytes  |                      |
  |  | - buffer: N bytes|                      |
  |  +------------------+                      |
  |                                            |
  |  +------------------+                      |
  |  | Send             |  (전송 대기/진행 중)  |
  |  +------------------+                      |
  |                                            |
  |  muteState: NOT_MUTED / MUTED / ...       |
  +--------------------------------------------+
```

### 7.2 채널 뮤트 상태

```java
// KafkaChannel.java (라인 70~)
/**
 * Mute States for KafkaChannel:
 *  NOT_MUTED: Channel is not muted. This is the default state.
 *  MUTED: Channel is muted. Channel must be in this state to be unmuted.
 *  MUTED_AND_RESPONSE_PENDING: Channel is muted and SocketServer has not
 *      sent a response back to the client yet.
 *  MUTED_AND_THROTTLED: Channel is muted and throttling is in progress
 *      due to quota violation.
 *  MUTED_AND_THROTTLED_AND_RESPONSE_PENDING: both conditions combined.
 */
```

```
뮤트 상태 전이 다이어그램:

  NOT_MUTED
      |
      | (요청 수신 완료)
      v
  MUTED_AND_RESPONSE_PENDING
      |
      +--- (응답 전송, 쓰로틀 없음) ---> MUTED ---> NOT_MUTED
      |                                              (unmute)
      +--- (쓰로틀 시작) ---> MUTED_AND_THROTTLED_AND_RESPONSE_PENDING
                                  |
                                  | (응답 전송)
                                  v
                              MUTED_AND_THROTTLED
                                  |
                                  | (쓰로틀 종료)
                                  v
                                MUTED ---> NOT_MUTED
```

### 7.3 NetworkReceive: 2단계 읽기

```java
// NetworkReceive.java (라인 32~)
/**
 * A size delimited Receive that consists of a 4 byte network-ordered
 * size N followed by N bytes of content
 */
public class NetworkReceive implements Receive {
    private final String source;
    private final ByteBuffer size;   // 4바이트 크기 헤더
    private final int maxSize;
    private final MemoryPool memoryPool;
    private int requestedBufferSize = -1;
    private ByteBuffer buffer;       // 실제 페이로드 버퍼
}
```

```
NetworkReceive 2단계 읽기:

단계 1: 크기 헤더 읽기 (4바이트)
  +--------+
  | Size   |  ← 4바이트 big-endian 정수
  | (4B)   |     예: 0x00000100 = 256바이트
  +--------+

단계 2: 페이로드 읽기 (Size 바이트)
  +--------+-------------------------------------------+
  | Size   |           Payload (Size 바이트)             |
  | (4B)   |   (RequestHeader + RequestBody)            |
  +--------+-------------------------------------------+

NetworkReceive.readFrom(channel):
  if (size.hasRemaining()) {
      // 아직 4바이트를 다 읽지 못함 → 계속 읽기
      channel.read(size);
      if (!size.hasRemaining()) {
          // 4바이트 완료 → 페이로드 크기 확인
          int receiveSize = size.getInt(0);
          // maxSize 초과 시 예외
          // 페이로드 버퍼 할당 (MemoryPool에서)
          buffer = memoryPool.tryAllocate(receiveSize);
      }
  }
  if (buffer != null) {
      // 페이로드 읽기
      channel.read(buffer);
  }

complete() = !size.hasRemaining() && buffer != null && !buffer.hasRemaining()
```

**왜 2단계로 읽는가?**

네트워크에서 데이터는 조각(fragment)으로 도착할 수 있다. 먼저 크기를 알아야
정확한 크기의 버퍼를 할당할 수 있고, 부분적으로 도착한 데이터를 올바르게 누적할 수
있다. 또한 `maxSize`를 확인하여 악의적으로 큰 요청을 거부할 수 있다.

---

## 8. RequestChannel: 큐 시스템

### 8.1 구조

```scala
// RequestChannel.scala (라인 343~)
class RequestChannel(val queueSize: Int, val time: Time, ...)
```

```
RequestChannel 구조:

  +--------------------------------------------------+
  | RequestChannel                                    |
  |                                                   |
  |  requestQueue: ArrayBlockingQueue[BaseRequest]    |
  |  (공유 큐, 크기 = queued.max.requests)             |
  |                                                   |
  |  +------+------+------+                           |
  |  | Req1 | Req2 | Req3 | ...                       |
  |  +------+------+------+                           |
  |                                                   |
  |  Processor별 responseQueue:                       |
  |  +------------------------------------------+     |
  |  | P0: [Resp1, Resp2]                       |     |
  |  | P1: [Resp3]                              |     |
  |  | P2: []                                   |     |
  |  +------------------------------------------+     |
  +--------------------------------------------------+
```

### 8.2 요청 흐름

```
Processor → RequestChannel → Handler:

  1. Processor: 요청 수신 완료
     → requestChannel.sendRequest(request)
     → requestQueue.put(request)  [블로킹, 큐 가득 차면 대기]

  2. Handler: 요청 처리
     → requestChannel.receiveRequest(timeout)
     → requestQueue.poll(timeout)  [블로킹, 큐 비면 대기]
     → KafkaApis.handle(request)

  3. KafkaApis: 응답 생성
     → requestChannel.sendResponse(response)
     → processor의 responseQueue에 추가
     → processor.wakeup()  [NIO Selector 깨우기]
```

### 8.3 응답 유형

```
RequestChannel의 응답 유형:

  SendResponse:              실제 데이터를 클라이언트에 전송
  NoOpResponse:              아무 동작 없음 (acks=0인 Produce)
  CloseConnectionResponse:   연결 종료
  StartThrottlingResponse:   쓰로틀 시작
  EndThrottlingResponse:     쓰로틀 종료
```

**왜 requestQueue는 공유하고 responseQueue는 Processor별인가?**

요청은 어떤 Handler 스레드든 처리할 수 있으므로 공유 큐가 적합하다. 그러나 응답은
원래 요청을 수신한 Processor의 소켓 채널을 통해 전송해야 하므로, Processor별로
분리된 큐가 필요하다.

---

## 9. KafkaRequestHandler: 요청 처리 스레드

### 9.1 구조

```scala
// KafkaRequestHandler.scala (라인 39~)
object KafkaRequestHandler {
    private val threadRequestChannel = new ThreadLocal[RequestChannel]
    private val threadCurrentRequest = new ThreadLocal[RequestChannel.Request]
}
```

```scala
// KafkaRequestHandler 실행 루프 (개념)
class KafkaRequestHandler(id: Int, ...) extends Runnable {
  override def run(): Unit = {
    while (isRunning) {
      // 1. 요청 큐에서 블로킹으로 요청 가져오기
      val req = requestChannel.receiveRequest(300)  // 300ms 타임아웃

      // 2. ThreadLocal에 현재 요청 정보 설정
      threadRequestChannel.set(requestChannel)
      threadCurrentRequest.set(req)

      // 3. API 핸들러로 요청 처리 위임
      apis.handle(req, requestLocal)

      // 4. ThreadLocal 정리
      threadCurrentRequest.remove()
    }
  }
}
```

### 9.2 스레드 풀 구성

```
KafkaRequestHandlerPool:

  설정: num.io.threads = 8 (기본값)

  +-----+-----+-----+-----+-----+-----+-----+-----+
  | H0  | H1  | H2  | H3  | H4  | H5  | H6  | H7  |
  +--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
     |     |     |     |     |     |     |     |
     +-----+-----+-----+-----+-----+-----+-----+
                         |
                         v
              requestQueue (공유)
                         ^
                         |
     +-----+-----+-----+
     |     |     |
  +--+--+--+--+--+--+
  | P0  | P1  | P2  |  (Processor들이 요청 추가)
  +-----+-----+-----+
```

### 9.3 비동기 콜백 스케줄링

```scala
// KafkaRequestHandler.scala (라인 62~)
def wrapAsyncCallback[T](asyncCompletionCallback: (RequestLocal, T) => Unit,
                          requestLocal: RequestLocal): T => Unit = {
    val requestChannel = threadRequestChannel.get()
    val currentRequest = threadCurrentRequest.get()
    // ...
    t => {
        if (threadCurrentRequest.get() == currentRequest) {
            // 같은 요청 스레드에서 실행 → 직접 호출
            asyncCompletionCallback(requestLocal, t)
        } else {
            // 다른 스레드에서 실행 → 요청 스레드에 재스케줄
            requestChannel.sendCallbackRequest(...)
        }
    }
}
```

**왜 콜백을 요청 스레드에 재스케줄하는가?**

비동기 작업(예: Purgatory에서 완료)의 콜백이 I/O 스레드나 타이머 스레드에서 실행되면,
해당 스레드가 블로킹되거나 응답 전송이 복잡해진다. 요청 처리 스레드에 재스케줄하면
응답 전송 경로가 일관되고, ThreadLocal 상태를 올바르게 관리할 수 있다.

---

## 10. 채널 뮤팅 메커니즘

### 10.1 뮤팅의 목적

채널 뮤팅은 두 가지 핵심 목적을 달성한다:

1. **요청 순서 보장**: 하나의 채널에서 이전 요청의 응답이 전송되기 전에 다음 요청을 읽지 않음
2. **메모리 압력 제어**: 메모리 풀이 부족하면 추가 읽기를 중단

### 10.2 요청 순서 보장 뮤팅

```
요청 순서 보장을 위한 뮤팅 흐름:

  시간 →

  Channel A:  [Req1 수신] → MUTE → [Req1 처리] → [Resp1 전송] → UNMUTE → [Req2 수신]
                    |                                    |
                    +---- 뮤트 기간: 추가 읽기 차단 --------+

  만약 뮤팅이 없다면:
  Channel A:  [Req1 수신] [Req2 수신] [Req3 수신]
              → Req2가 Req1보다 먼저 처리될 수 있음!
              → 클라이언트가 기대하는 순서 보장 위반
```

### 10.3 메모리 부족 시 뮤팅

```
메모리 부족 시 뮤팅:

  MemoryPool 상태:
    +---+---+---+---+---+---+---+---+
    | ■ | ■ | ■ | ■ | ■ | ■ | ■ | □ |  (거의 가득 참)
    +---+---+---+---+---+---+---+---+
                                  ^
                            마지막 여유 블록

  새 NetworkReceive 할당 시도:
    buffer = memoryPool.tryAllocate(receiveSize);

    실패 시:
      → outOfMemory = true
      → 모든 채널 뮤트 (읽기 중단)
      → 기존 응답 전송 완료 후 버퍼 반환
      → 메모리 확보 시 뮤트 해제
```

---

## 11. 프로토콜 프레임 포맷

### 11.1 전체 프레임 구조

Kafka 프로토콜은 TCP 위에서 동작하는 크기 선행(size-prefixed) 이진 프로토콜이다.

```
Kafka 프로토콜 프레임:

  +------+-------------------------------------------+
  | Size | RequestHeader + RequestBody               |
  | 4B   |                                           |
  +------+-------------------------------------------+

  Size = RequestHeader + RequestBody의 총 바이트 수 (4바이트 자체는 제외)
```

### 11.2 Request Header

```
Request Header v2 (대부분의 API):

  +--------+---------+---------------+--------------+---+
  | ApiKey | Version | CorrelationId | ClientId     |...|
  | int16  | int16   | int32         | NullableStr  |   |
  +--------+---------+---------------+--------------+---+

  ApiKey:          API 유형 (0=Produce, 1=Fetch, 18=ApiVersions, ...)
  Version:         API 버전 (하위 호환성)
  CorrelationId:   요청-응답 매칭용 고유 ID
  ClientId:        클라이언트 식별자 (메트릭/디버깅용)
```

### 11.3 Response Header

```
Response Header:

  +---------------+
  | CorrelationId |
  | int32         |
  +---------------+

  → 요청의 CorrelationId와 동일한 값
  → 클라이언트가 파이프라이닝된 응답을 매칭
```

### 11.4 주요 API 요청/응답 예시

```
Produce 요청 (ApiKey=0):

  Header:
    ApiKey=0, Version=9, CorrelationId=42, ClientId="my-producer"

  Body:
    TransactionalId: null
    Acks: -1 (all)
    TimeoutMs: 30000
    TopicData:
      - Topic: "my-topic"
        PartitionData:
          - Partition: 0
            Records: [RecordBatch(baseOffset=0, records=[...])]

Fetch 요청 (ApiKey=1):

  Header:
    ApiKey=1, Version=16, CorrelationId=43, ClientId="my-consumer"

  Body:
    ReplicaId: -1 (컨슈머)
    MaxWaitMs: 500
    MinBytes: 1
    MaxBytes: 52428800
    Topics:
      - Topic: "my-topic"
        Partitions:
          - Partition: 0
            FetchOffset: 5367851
            MaxBytes: 1048576
```

### 11.5 프로토콜 직렬화

Kafka는 자체 직렬화 포맷을 사용하며, `.json` 스키마 파일에서 Java 코드를 자동 생성한다.

```
직렬화 타입:
  int8, int16, int32, int64    → 고정 크기 정수
  varint, varlong              → 가변 길이 정수 (ZigZag 인코딩)
  string, bytes                → 길이 선행 데이터
  array                        → 길이 선행 배열
  nullable_*                   → null 가능 타입 (-1 = null)
  compact_*                    → 길이에 varint 사용 (v2+)
  tagged_fields                → 태그 기반 확장 필드
```

---

## 12. API 버전 협상

### 12.1 ApiVersionsRequest

클라이언트가 브로커에 연결하면 가장 먼저 `ApiVersions` 요청을 보내
지원되는 API 버전 범위를 확인한다.

```
API 버전 협상 흐름:

  Client                              Broker
    |                                    |
    |  ApiVersionsRequest(v0~v3)         |
    |----------------------------------->|
    |                                    |
    |  ApiVersionsResponse               |
    |  { apiKey=0, minVer=0, maxVer=11,  |
    |    apiKey=1, minVer=0, maxVer=16,  |
    |    apiKey=18, minVer=0, maxVer=3,  |
    |    ... }                           |
    |<-----------------------------------|
    |                                    |
    | (각 API에 대해 min(클라이언트 max,     |
    |  브로커 max) 버전 사용)              |
    |                                    |
    |  ProduceRequest(v9)                |
    |----------------------------------->|
```

### 12.2 왜 API 버전 협상이 필요한가?

Kafka는 **무중단 롤링 업그레이드**를 지원한다. 클러스터의 브로커들이 서로 다른 버전일 수
있고, 클라이언트도 브로커와 다른 버전일 수 있다. 버전 협상을 통해:

1. 클라이언트가 브로커의 지원 범위 내에서 가장 높은 버전을 사용
2. 새 필드가 추가되어도 이전 버전 클라이언트가 영향 받지 않음
3. 태그 필드(tagged fields)로 미래 확장성 확보

### 12.3 하위 호환성 메커니즘

```
API 버전 진화 예시 (Produce API):

  v0-v2: 기본 Produce
  v3:    TransactionalId 추가
  v5:    RecordBatch 포맷 변경 (magic v2)
  v9:    flexible versions (태그 필드 지원)

  브로커가 v0~v11 지원, 클라이언트가 v0~v9 지원:
    → 사용 버전: v9 (양쪽의 min(maxVer))

  오래된 클라이언트 (v0~v3 지원):
    → 사용 버전: v3
    → 트랜잭션 기능만 지원, 태그 필드 없음
```

---

## 13. Zero-Copy 전송

### 13.1 전통적 데이터 전송 vs Zero-Copy

```
전통적 전송 (4번의 데이터 복사):

  디스크 → [커널 버퍼] → [사용자 버퍼] → [소켓 버퍼] → 네트워크
         read()          처리           write()
         DMA copy        CPU copy       DMA copy

  복사 횟수: 4회
  컨텍스트 스위치: 4회

Zero-Copy 전송 (sendfile, 2번의 DMA 복사):

  디스크 → [커널 버퍼] ――――――――――――――→ 네트워크
         DMA copy    sendfile()      DMA copy

  복사 횟수: 2회 (DMA만, CPU 복사 없음)
  컨텍스트 스위치: 2회
```

### 13.2 Kafka에서의 Zero-Copy

Kafka의 Fetch 응답에서 로그 데이터를 전송할 때 Zero-Copy를 사용한다.

```
관련 소스 파일:
  clients/src/main/java/org/apache/kafka/common/record/internal/FileRecords.java
  clients/src/main/java/org/apache/kafka/common/record/internal/DefaultRecordsSend.java

FileRecords.writeTo(GatheringByteChannel channel, long position, int length):
  → FileChannel.transferTo(position, count, channel)
  → 내부적으로 sendfile(2) 시스템 콜 사용
```

```
Zero-Copy의 Fetch 응답 경로:

  1. FetchRequest 도착
  2. ReplicaManager.readFromLog()
     → LogSegment.read() → FileRecords.slice()
     → FileRecords 객체 반환 (파일 참조만, 데이터 복사 없음)

  3. 응답 구성
     → FetchResponse에 FileRecords 포함
     → MultiRecordsSend로 래핑

  4. Processor가 응답 전송
     → FileRecords.writeTo(socketChannel)
     → FileChannel.transferTo() → sendfile(2)
     → 디스크 → 네트워크 (커널 공간에서 직접)
```

### 13.3 Zero-Copy가 불가능한 경우

```
Zero-Copy 불가 상황:

  1. SSL/TLS 연결:
     → 암호화가 필요하므로 사용자 공간에서 처리
     → 데이터를 메모리로 읽어서 암호화 후 전송

  2. 메시지 형식 변환:
     → 오래된 클라이언트가 이전 메시지 형식 요청
     → 변환을 위해 메모리에서 처리

  3. 압축 변환:
     → 클라이언트가 요청한 압축과 저장된 압축이 다른 경우
```

**왜 Zero-Copy가 중요한가?**

Kafka의 주요 사용 사례인 Fetch 요청에서, 데이터는 단순히 디스크에서 네트워크로
이동하기만 하면 된다. 브로커가 데이터를 해석하거나 변환할 필요가 없으므로,
sendfile을 통해 CPU 사용량과 메모리 대역폭을 크게 절감할 수 있다. 이것이
Kafka가 디스크 기반임에도 높은 처리량을 달성하는 핵심 요인 중 하나이다.

---

## 14. 설계 결정의 이유 (Why)

### 14.1 왜 Reactor 패턴인가?

Kafka는 Netty 같은 프레임워크 대신 직접 구현한 Reactor 패턴을 사용한다.

1. **의존성 최소화**: 외부 네트워크 프레임워크에 대한 의존 없음
2. **프로토콜 최적화**: Kafka 프로토콜의 특성(크기 선행, 순서 보장)에 최적화
3. **메모리 제어**: MemoryPool을 통한 정밀한 버퍼 메모리 관리
4. **Zero-Copy 통합**: sendfile 시스템 콜과의 깔끔한 통합

### 14.2 왜 요청 큐를 공유하는가?

Processor별로 별도의 Handler를 두는 대신, 단일 requestQueue를 공유한다.

```
공유 큐의 이점:

  Processor별 별도 Handler:          공유 큐:
  P0 → [Queue0] → H0                P0 ─┐
  P1 → [Queue1] → H1                P1 ─┤→ [Queue] → H0, H1, H2 ...
  P2 → [Queue2] → H2                P2 ─┘

  문제: P0에 요청이 몰리면             → 모든 Handler가 균등하게 처리
  H0만 바쁘고 나머지는 유휴           → 자연스러운 부하 분산
```

### 14.3 왜 num.network.threads와 num.io.threads를 분리하는가?

```
num.network.threads = 3    (Processor 수)
num.io.threads = 8         (Handler 수)

  Network Thread (Processor):
    - NIO 셀렉터 관리
    - 바이트 수준 I/O
    - CPU-light, I/O-heavy

  I/O Thread (Handler):
    - 요청 역직렬화/검증
    - 디스크 I/O (로그 읽기/쓰기)
    - CPU-moderate, 디스크-heavy

  분리 이유:
    - 네트워크 I/O와 디스크 I/O의 특성이 다름
    - Processor가 블로킹되면 모든 연결의 I/O가 멈춤
    - 디스크 I/O를 Handler에서 수행하면 Processor는 항상 반응 가능
```

### 14.4 왜 CorrelationId를 사용하는가?

하나의 TCP 연결에서 여러 요청을 파이프라이닝(pipelining)할 수 있다.
CorrelationId가 없으면 응답을 어떤 요청에 매칭할지 알 수 없다.

```
파이프라이닝 예시:

  Client → Broker:
    [Req1: CorrId=1] [Req2: CorrId=2] [Req3: CorrId=3]

  Broker → Client:
    [Resp: CorrId=1] [Resp: CorrId=2] [Resp: CorrId=3]

  → 응답 순서가 요청 순서와 동일 (채널 뮤팅으로 보장)
  → CorrId로 추가 검증 가능
```

### 14.5 왜 바이너리 프로토콜인가?

JSON이나 Protocol Buffers 대신 자체 바이너리 프로토콜을 사용하는 이유:

1. **오버헤드 최소화**: 필드명 없이 위치 기반 인코딩
2. **Zero-Copy 호환**: 직렬화된 데이터를 그대로 디스크에 저장하고 네트워크로 전송
3. **정밀한 버전 관리**: API별로 독립적인 버전 진화
4. **스키마 진화**: 태그 필드로 하위 호환성 유지하면서 확장

### 14.6 왜 queued.max.requests 제한이 있는가?

```
requestQueue 크기 제한의 이유:

  제한 없을 때:
    → 빠른 프로듀서가 느린 소비를 압도
    → requestQueue가 무한히 커짐
    → OOM 발생

  제한 있을 때 (기본 500):
    → requestQueue.put()이 블로킹
    → Processor가 대기 → NIO Selector가 읽기 중단
    → 클라이언트 측 TCP 버퍼가 가득 참
    → 자연스러운 배압(backpressure) 전파
```

---

## 요약 테이블

| 계층 | 컴포넌트 | 스레드 수 | 역할 |
|------|---------|----------|------|
| 연결 수락 | Acceptor | 리스너당 1 | TCP accept |
| 네트워크 I/O | Processor | num.network.threads | NIO 읽기/쓰기 |
| 요청 처리 | KafkaRequestHandler | num.io.threads | 비즈니스 로직 |
| 큐 | RequestChannel | - | Processor ↔ Handler 연결 |

| 설정 | 기본값 | 설명 |
|------|--------|------|
| num.network.threads | 3 | Processor 스레드 수 |
| num.io.threads | 8 | Handler 스레드 수 |
| queued.max.requests | 500 | 요청 큐 최대 크기 |
| socket.send.buffer.bytes | 102400 | SO_SNDBUF |
| socket.receive.buffer.bytes | 102400 | SO_RCVBUF |
| socket.request.max.bytes | 104857600 | 최대 요청 크기 (100MB) |
| connections.max.idle.ms | 600000 | 유휴 연결 타임아웃 |

---

## 참고 소스 파일 전체 경로

```
core/src/main/scala/kafka/network/
  +-- SocketServer.scala          # Acceptor + Processor
  +-- RequestChannel.scala        # 요청/응답 큐

core/src/main/scala/kafka/server/
  +-- KafkaRequestHandler.scala   # 요청 처리 스레드 풀

clients/src/main/java/org/apache/kafka/common/network/
  +-- Selector.java               # NIO 셀렉터 래퍼
  +-- KafkaChannel.java           # 채널 추상화 (뮤트 상태 포함)
  +-- NetworkReceive.java         # 수신 메시지 (2단계 읽기)
  +-- NetworkSend.java            # 송신 메시지
  +-- TransportLayer.java         # 전송 계층 인터페이스
  +-- PlaintextTransportLayer.java # 평문 전송
  +-- SslTransportLayer.java      # SSL 전송

clients/src/main/java/org/apache/kafka/common/record/internal/
  +-- FileRecords.java            # 파일 레코드 (Zero-Copy 지원)
  +-- DefaultRecordsSend.java     # 레코드 전송 래퍼
  +-- MultiRecordsSend.java       # 다중 레코드 전송
  +-- RecordsSend.java            # 레코드 전송 인터페이스
```
