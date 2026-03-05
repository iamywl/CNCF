# PoC-04: 멀티플렉스 스트림 관리

## 개념

하나의 TCP 연결 위에서 여러 HTTP/2 스트림을 동시에 관리하는 gRPC 멀티플렉싱을 시뮬레이션한다.

```
하나의 TCP 연결
┌──────────────────────────────────────────────┐
│  Stream #1 (Unary)     ──▶  ◀──              │
│  Stream #3 (ServerStr) ──▶  ◀══◀══◀══        │
│  Stream #5 (ClientStr) ══▶══▶══▶  ◀──        │
│  Stream #7 (Bidi)      ══▶◀══▶◀══▶◀══       │
└──────────────────────────────────────────────┘

스트림 상태 전이 (RFC 7540):
  Open ──(한쪽 END_STREAM)──▶ HalfClosed ──(반대쪽 END_STREAM)──▶ Closed
```

## 4가지 RPC 패턴

| 패턴 | 요청 | 응답 | 예시 |
|------|------|------|------|
| Unary | 1개 | 1개 | 일반 함수 호출 |
| Server Streaming | 1개 | N개 | 주식 가격 구독 |
| Client Streaming | N개 | 1개 | 파일 업로드 |
| Bidi Streaming | N개 | M개 | 실시간 채팅 |

## 시뮬레이션하는 gRPC 구조

| 구조체/함수 | 실제 위치 | 역할 |
|------------|----------|------|
| `Stream` | `transport.go` | 개별 HTTP/2 스트림 |
| `http2Server` | `http2_server.go` | 서버 트랜스포트 (스트림 관리) |
| `operateHeaders` | `http2_server.go` | 새 스트림 생성 |
| `handleData` | `http2_server.go` | DATA 프레임 처리 |
| `maxConcurrentStreams` | 설정 | 동시 스트림 수 제한 |

## 실행 방법

```bash
cd poc-04-stream
go run main.go
```

## 예상 출력

```
=== 멀티플렉스 스트림 관리 시뮬레이션 ===

── 1. Unary RPC (1:1) ──
[클라이언트] 스트림 #1 생성: /greeter/SayHello [Unary] (활성: 1/4)
  [stream#1] 클라이언트 → 서버: 'Hello' (END_STREAM)
  [stream#1] 서버 → 클라이언트: 'Hi!' (END_STREAM)
[클라이언트] 스트림 #1 종료 (활성: 0/4)
...

── 5. 동시 스트림 멀티플렉싱 ──
[클라이언트] 스트림 #1 생성 (활성: 1/3)
[클라이언트] 스트림 #3 생성 (활성: 2/3)
[클라이언트] 스트림 #5 생성 (활성: 3/3)
  /service/Method4: 최대 동시 스트림 수 초과 (max=3)
...

=== 시뮬레이션 완료 ===
```

## 핵심 포인트

1. **스트림 ID 규칙**: 클라이언트=홀수(1,3,5...), 서버=짝수(2,4,6...), 0=연결 수준
2. **멀티플렉싱**: 하나의 TCP 연결에서 여러 스트림이 독립적으로 데이터를 주고받음
3. **동시 스트림 제한**: `MAX_CONCURRENT_STREAMS` 설정으로 과부하 방지
4. **상태 전이**: Open → HalfClosed(END_STREAM) → Closed
