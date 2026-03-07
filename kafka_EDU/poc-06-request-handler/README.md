# PoC-06: Kafka 요청 처리 파이프라인

## 개요

Kafka의 요청 처리 파이프라인을 시뮬레이션한다. Acceptor가 수락한 연결을 Processor가 읽고, RequestChannel을 통해 KafkaRequestHandler로 전달하며, KafkaApis가 API 키에 따라 적절한 핸들러로 라우팅한다.

## 실행 방법

```bash
go run main.go
```

## Kafka 소스코드 참조

| 컴포넌트 | 원본 파일 | 설명 |
|----------|----------|------|
| KafkaApis | `core/src/main/scala/kafka/server/KafkaApis.scala` | API 키별 요청 라우팅 (40+ API) |
| KafkaRequestHandler | `core/src/main/scala/kafka/server/KafkaRequestHandler.scala` | RequestChannel에서 요청을 꺼내 처리 |
| KafkaRequestHandlerPool | `KafkaRequestHandler.scala` | Handler 스레드 풀 관리 |
| RequestChannel | `core/src/main/scala/kafka/network/RequestChannel.scala` | Processor-Handler 사이 큐 |
| ApiKeys | `clients/src/main/java/.../common/protocol/ApiKeys.java` | API 키 정의 |

## 시뮬레이션하는 핵심 개념

### 1. KafkaApis API 라우팅

```scala
// KafkaApis.scala:150 - handle() 메서드
request.header.apiKey match {
  case ApiKeys.PRODUCE  => handleProduceRequest(request, requestLocal)
  case ApiKeys.FETCH    => handleFetchRequest(request)
  case ApiKeys.METADATA => handleTopicMetadataRequest(request)
  // ... 40+ 다른 API
}
```

이 PoC에서는 PRODUCE(0), FETCH(1), METADATA(3) 세 가지 API를 구현한다.

### 2. KafkaRequestHandler 요청 처리 루프

```scala
// KafkaRequestHandler.scala:107
while (!stopped) {
  val req = requestChannel.receiveRequest(300) // 300ms 타임아웃
  req match {
    case request: RequestChannel.Request =>
      apis.handle(request, requestLocal)
    case null => // continue
  }
}
```

### 3. Handler Pool

```
KafkaRequestHandlerPool은 num.io.threads 설정에 따라 스레드를 생성한다.
모든 Handler가 하나의 RequestChannel에서 경쟁적으로 요청을 가져간다.
resizeThreadPool()으로 런타임에 스레드 수를 변경할 수 있다.
```

### 4. CorrelationID 매칭

```
요청 헤더: [apiKey: 2B][apiVersion: 2B][correlationId: 4B][clientId: varlen]
클라이언트가 설정한 correlationID가 응답에 그대로 반환되어
비동기 파이프라이닝에서 요청-응답 매칭을 보장한다.
```

### 5. Request-Response 흐름

```
Client → Processor (수신) → RequestChannel.requestQueue → Handler (처리)
                                                              |
Client ← Processor (전송) ← RequestChannel.responseQueue[N] ←─┘
```

## 아키텍처 다이어그램

```
                    ┌─────────────────────────────────────────────────────────────┐
                    │                    KafkaApis.handle()                       │
                    │  ┌─────────┐  ┌──────────┐  ┌────────────────────────────┐ │
                    │  │PRODUCE=0│  │ FETCH=1  │  │     METADATA=3            │ │
                    │  │         │  │          │  │                            │ │
                    │  │Replica  │  │Replica   │  │ MetadataCache             │ │
                    │  │Manager  │  │Manager   │  │                            │ │
                    │  └─────────┘  └──────────┘  └────────────────────────────┘ │
                    └────────────────────────┬────────────────────────────────────┘
                                             │
  Processor-0 ──┐     RequestChannel    Handler-0 ──┤
  Processor-1 ──┼──→ [requestQueue] ──→ Handler-1 ──┤──→ KafkaApis.handle()
  Processor-2 ──┘                       Handler-2 ──┤
                                        Handler-3 ──┘
```

## 설정 매핑

| Kafka 설정 | 의미 | PoC 값 |
|-----------|------|--------|
| `num.network.threads` | Processor 수 | 3 |
| `num.io.threads` | Handler 수 | 4 |
| `queued.max.requests` | RequestChannel 큐 크기 | 100 |
