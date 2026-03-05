# PoC-10: 헤더/트레일러 전파 시뮬레이션

## 개념

gRPC 메타데이터는 HTTP/2 헤더 프레임을 통해 전달되는 key-value 쌍이다. grpc-go에서는 `metadata.MD` 타입(`map[string][]string`)으로 표현되며, `context.Context`를 통해 전파된다.

### MD 타입과 전파 방향

```
클라이언트                                서버
    │                                      │
    │── HEADERS (outgoing MD) ────────────→│  요청 헤더
    │── DATA (요청 본문) ─────────────────→│
    │── END_STREAM ───────────────────────→│
    │                                      │
    │←── HEADERS (응답 헤더 MD) ───────────│  응답 헤더
    │←── DATA (응답 본문) ─────────────────│
    │←── HEADERS (트레일러 MD) + END_STREAM│  트레일러
    │                                      │
```

### Context 통합

```
NewOutgoingContext(ctx, md)    → 클라이언트가 보낼 메타데이터 설정
FromOutgoingContext(ctx)       → 전송 계층에서 메타데이터 추출
NewIncomingContext(ctx, md)    → 수신한 메타데이터 저장 (서버 측)
FromIncomingContext(ctx)       → 서버 핸들러에서 메타데이터 읽기
AppendToOutgoingContext(ctx)   → 기존 메타데이터에 추가
```

### 바이너리 헤더

키가 `-bin`으로 끝나면 바이너리 데이터로 간주하여 base64 인코딩으로 전송한다. protobuf 직렬화된 데이터를 메타데이터로 전달할 때 사용한다.

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
gRPC Metadata 시뮬레이션
========================================

[1] MD 기본 연산
──────────────────
  New()로 생성:
  authorization: Bearer token-abc
  x-request-id: req-12345
  Pairs()로 생성:
  content-type: application/grpc
  user-agent: grpc-go/1.60.0
  x-custom: value1
  x-custom: value2
  Join() 결과: 5개 키

[2] 바이너리 헤더 (-bin 접미사)
───────────────────────────────
  원본 바이너리: 089601120a48656c6c6f (10 bytes)
  base64 인코딩: CJYBEgpIZWxsbw==
  ...

[4] Unary RPC 메타데이터 전파 흐름
────────────────────────────────────
  [클라이언트 → 서버] 요청 헤더:
  authorization: Bearer my-token
  x-request-id: req-...
  x-trace-id: trace-abc-123
  [서버 → 클라이언트] 트레일러:
  x-processing-time-ms: 42
  x-rpc-status: OK
```

## 관련 소스

| 파일 | 설명 |
|------|------|
| `metadata/metadata.go` | MD 타입, Context 통합 함수 |
| `Documentation/grpc-metadata.md` | 메타데이터 사용 가이드 |
| `internal/transport/http2_client.go` | 실제 HTTP/2 헤더 전송 |
