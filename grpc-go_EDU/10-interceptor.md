# 10. gRPC-Go 인터셉터 (Interceptor) 심화 분석

## 목차

1. [개요](#1-개요)
2. [4가지 인터셉터 타입](#2-4가지-인터셉터-타입)
3. [함수 시그니처 상세 분석](#3-함수-시그니처-상세-분석)
4. [인터셉터 등록 메커니즘](#4-인터셉터-등록-메커니즘)
5. [체이닝 메커니즘 구현 분석](#5-체이닝-메커니즘-구현-분석)
6. [실행 순서: 등록 순서와 실행 순서의 관계](#6-실행-순서-등록-순서와-실행-순서의-관계)
7. [서버 인터셉터 실행 흐름](#7-서버-인터셉터-실행-흐름)
8. [클라이언트 인터셉터 실행 흐름](#8-클라이언트-인터셉터-실행-흐름)
9. [스트리밍 인터셉터의 차이점](#9-스트리밍-인터셉터의-차이점)
10. [일반적인 인터셉터 패턴](#10-일반적인-인터셉터-패턴)
11. [인터셉터 vs StatsHandler 비교](#11-인터셉터-vs-statshandler-비교)
12. [설계 결정: 왜(Why) 이렇게 만들었는가](#12-설계-결정-왜why-이렇게-만들었는가)

---

## 1. 개요

gRPC-Go의 인터셉터(Interceptor)는 RPC 호출의 전후에 횡단 관심사(cross-cutting concerns)를 삽입할 수 있는 미들웨어 메커니즘이다. HTTP 미들웨어와 유사한 개념이지만, gRPC의 Unary/Streaming 이중 구조에 맞추어 4가지 타입으로 분화되어 있다.

```
소스코드 위치: grpc-go/interceptor.go (전체 109줄)
```

인터셉터가 해결하는 문제:
- **로깅**: 모든 RPC 호출에 대한 요청/응답 로그
- **인증/인가**: 메타데이터에서 토큰 추출 및 검증
- **메트릭**: 지연시간, 에러율, 요청 카운트 수집
- **에러 복구**: panic으로부터의 복구
- **재시도**: 실패한 요청의 자동 재시도
- **검증**: 요청 메시지의 유효성 검사

### 전체 아키텍처

```
클라이언트 측:
┌─────────────────────────────────────────────────────────┐
│  Application Code                                       │
│       │                                                 │
│       ▼                                                 │
│  cc.Invoke(ctx, method, req, reply, opts...)            │
│       │                                                 │
│       ▼                                                 │
│  ┌──────────────────────────┐                           │
│  │ UnaryClientInterceptor   │ ← 체이닝된 단일 인터셉터  │
│  │  [0] → [1] → [2] → ...  │                           │
│  └──────────┬───────────────┘                           │
│             ▼                                           │
│  invoke() → newClientStream() → 네트워크 전송            │
└─────────────────────────────────────────────────────────┘

서버 측:
┌─────────────────────────────────────────────────────────┐
│  네트워크 수신 → processUnaryRPC()                       │
│       │                                                 │
│       ▼                                                 │
│  md.Handler(srv, ctx, df, s.opts.unaryInt)              │
│       │                                                 │
│       ▼  (protoc 생성 코드 내부)                         │
│  ┌──────────────────────────┐                           │
│  │ UnaryServerInterceptor   │ ← 체이닝된 단일 인터셉터  │
│  │  [0] → [1] → [2] → ...  │                           │
│  └──────────┬───────────────┘                           │
│             ▼                                           │
│  Service Method Implementation                          │
└─────────────────────────────────────────────────────────┘
```

---

## 2. 4가지 인터셉터 타입

gRPC-Go는 클라이언트/서버와 Unary/Stream의 조합으로 정확히 4가지 인터셉터 타입을 정의한다.

```
소스코드: grpc-go/interceptor.go:19-109
```

| 타입 | 위치 | RPC 종류 | 정의 위치 |
|------|------|----------|-----------|
| `UnaryClientInterceptor` | 클라이언트 | Unary | interceptor.go:43 |
| `StreamClientInterceptor` | 클라이언트 | Streaming | interceptor.go:63 |
| `UnaryServerInterceptor` | 서버 | Unary | interceptor.go:87 |
| `StreamServerInterceptor` | 서버 | Streaming | interceptor.go:108 |

### 왜 4가지인가?

Unary RPC와 Streaming RPC는 근본적으로 다른 생명주기를 가진다:
- **Unary**: 요청 1개 → 응답 1개, 함수 호출과 유사
- **Streaming**: 지속적인 메시지 교환, 스트림 객체를 통한 I/O

클라이언트와 서버도 인터셉터에서 접근하는 매개변수가 다르다:
- **클라이언트**: `ClientConn`, `CallOption`, `method` 접근
- **서버**: `Server` 인스턴스, `ServerStream`, `UnaryServerInfo` 접근

이 2x2 조합이 자연스럽게 4가지 타입을 만들어낸다.

---

## 3. 함수 시그니처 상세 분석

### 3.1 UnaryClientInterceptor

```go
// grpc-go/interceptor.go:43
type UnaryClientInterceptor func(
    ctx     context.Context,    // RPC 컨텍스트 (데드라인, 메타데이터 포함)
    method  string,             // 전체 메서드 이름 (예: "/package.service/Method")
    req     any,                // 요청 메시지 (protobuf)
    reply   any,                // 응답 메시지를 채울 포인터
    cc      *ClientConn,        // 클라이언트 연결 객체
    invoker UnaryInvoker,       // 다음 핸들러 (체인의 다음 또는 실제 RPC 호출)
    opts    ...CallOption,      // 호출 옵션 (타임아웃, 압축 등)
) error
```

보조 타입:
```go
// grpc-go/interceptor.go:26
type UnaryInvoker func(
    ctx    context.Context,
    method string,
    req, reply any,
    cc   *ClientConn,
    opts ...CallOption,
) error
```

핵심 설계: `invoker`는 **다음 체인을 호출하는 함수**이다. 인터셉터가 `invoker`를 호출하지 않으면 실제 RPC는 실행되지 않는다. 이것이 인터셉터가 요청을 거부하거나 캐시된 응답을 반환할 수 있는 근거이다.

### 3.2 StreamClientInterceptor

```go
// grpc-go/interceptor.go:63
type StreamClientInterceptor func(
    ctx      context.Context,
    desc     *StreamDesc,       // 스트림 설명 (서버/클라이언트 스트리밍 여부)
    cc       *ClientConn,
    method   string,
    streamer Streamer,          // 다음 핸들러 (ClientStream 생성자)
    opts     ...CallOption,
) (ClientStream, error)
```

보조 타입:
```go
// grpc-go/interceptor.go:46
type Streamer func(
    ctx    context.Context,
    desc   *StreamDesc,
    cc     *ClientConn,
    method string,
    opts   ...CallOption,
) (ClientStream, error)
```

Unary와의 차이: 반환 타입이 `ClientStream`이다. 인터셉터는 반환되는 `ClientStream`을 래핑하여 `SendMsg`/`RecvMsg` 호출을 가로챌 수 있다.

### 3.3 UnaryServerInterceptor

```go
// grpc-go/interceptor.go:87
type UnaryServerInterceptor func(
    ctx     context.Context,
    req     any,                // 이미 역직렬화된 요청 메시지
    info    *UnaryServerInfo,   // 메서드 정보
    handler UnaryHandler,       // 다음 핸들러 (체인의 다음 또는 실제 서비스 메서드)
) (resp any, err error)
```

보조 타입들:
```go
// grpc-go/interceptor.go:67-72
type UnaryServerInfo struct {
    Server     any      // 서비스 구현체 (읽기 전용)
    FullMethod string   // 전체 RPC 메서드 이름 ("/package.service/method")
}

// grpc-go/interceptor.go:81
type UnaryHandler func(ctx context.Context, req any) (any, error)
```

핵심 설계: `UnaryServerInfo.Server`를 통해 인터셉터가 서비스 구현체에 접근할 수 있다. 이를 통해 타입 어설션으로 서비스별 특수 처리가 가능하다.

### 3.4 StreamServerInterceptor

```go
// grpc-go/interceptor.go:108
type StreamServerInterceptor func(
    srv     any,                // 서비스 구현체
    ss      ServerStream,       // 서버 스트림 객체
    info    *StreamServerInfo,  // 스트리밍 RPC 정보
    handler StreamHandler,      // 서비스 메서드 핸들러
) error
```

보조 타입들:
```go
// grpc-go/interceptor.go:91-98
type StreamServerInfo struct {
    FullMethod     string  // "/package.service/method"
    IsClientStream bool    // 클라이언트 → 서버 스트리밍 여부
    IsServerStream bool    // 서버 → 클라이언트 스트리밍 여부
}

// grpc-go/stream.go:62
type StreamHandler func(srv any, stream ServerStream) error
```

Unary와의 차이: `StreamServerInfo`에 `IsClientStream`/`IsServerStream` 플래그가 있어 양방향/단방향 스트리밍을 구분할 수 있다. 인터셉터는 `ServerStream`을 래핑하여 메시지 송수신을 가로챌 수 있다.

---

## 4. 인터셉터 등록 메커니즘

### 4.1 서버 측 등록

서버에는 4가지 등록 함수가 있다:

```go
// grpc-go/server.go:468 - 단일 Unary 인터셉터 등록
func UnaryInterceptor(i UnaryServerInterceptor) ServerOption

// grpc-go/server.go:481 - 체인 Unary 인터셉터 등록
func ChainUnaryInterceptor(interceptors ...UnaryServerInterceptor) ServerOption

// grpc-go/server.go:489 - 단일 Stream 인터셉터 등록
func StreamInterceptor(i StreamServerInterceptor) ServerOption

// grpc-go/server.go:502 - 체인 Stream 인터셉터 등록
func ChainStreamInterceptor(interceptors ...StreamServerInterceptor) ServerOption
```

**`UnaryInterceptor` vs `ChainUnaryInterceptor` 차이**:

| 특성 | `UnaryInterceptor` | `ChainUnaryInterceptor` |
|------|---------------------|--------------------------|
| 호출 횟수 | **1번만** (2번째 호출 시 panic) | **여러 번 가능** (append) |
| 저장 위치 | `serverOptions.unaryInt` | `serverOptions.chainUnaryInts` |
| 실행 순서 | **항상 첫 번째** | unaryInt 다음 순서대로 |

```go
// grpc-go/server.go:468-474 - UnaryInterceptor는 중복 설정 시 panic
func UnaryInterceptor(i UnaryServerInterceptor) ServerOption {
    return newFuncServerOption(func(o *serverOptions) {
        if o.unaryInt != nil {
            panic("The unary server interceptor was already set and may not be reset.")
        }
        o.unaryInt = i
    })
}

// grpc-go/server.go:481-484 - ChainUnaryInterceptor는 append로 누적
func ChainUnaryInterceptor(interceptors ...UnaryServerInterceptor) ServerOption {
    return newFuncServerOption(func(o *serverOptions) {
        o.chainUnaryInts = append(o.chainUnaryInts, interceptors...)
    })
}
```

서버 옵션 구조체에서 두 필드가 분리되어 있다:
```go
// grpc-go/server.go:154-162
type serverOptions struct {
    // ...
    unaryInt        UnaryServerInterceptor    // 단일 인터셉터
    streamInt       StreamServerInterceptor   // 단일 인터셉터
    chainUnaryInts  []UnaryServerInterceptor  // 체인 인터셉터 슬라이스
    chainStreamInts []StreamServerInterceptor // 체인 인터셉터 슬라이스
    // ...
}
```

### 4.2 클라이언트 측 등록

클라이언트도 동일한 패턴의 4가지 등록 함수가 있다:

```go
// grpc-go/dialoptions.go:573 - 단일 Unary 인터셉터
func WithUnaryInterceptor(f UnaryClientInterceptor) DialOption

// grpc-go/dialoptions.go:584 - 체인 Unary 인터셉터
func WithChainUnaryInterceptor(interceptors ...UnaryClientInterceptor) DialOption

// grpc-go/dialoptions.go:592 - 단일 Stream 인터셉터
func WithStreamInterceptor(f StreamClientInterceptor) DialOption

// grpc-go/dialoptions.go:603 - 체인 Stream 인터셉터
func WithChainStreamInterceptor(interceptors ...StreamClientInterceptor) DialOption
```

클라이언트 측 차이:
```go
// grpc-go/dialoptions.go:573-577 - WithUnaryInterceptor는 덮어씀 (panic 없음)
func WithUnaryInterceptor(f UnaryClientInterceptor) DialOption {
    return newFuncDialOption(func(o *dialOptions) {
        o.unaryInt = f  // 기존 값을 단순 덮어씀
    })
}

// grpc-go/dialoptions.go:584-588 - WithChainUnaryInterceptor는 append
func WithChainUnaryInterceptor(interceptors ...UnaryClientInterceptor) DialOption {
    return newFuncDialOption(func(o *dialOptions) {
        o.chainUnaryInts = append(o.chainUnaryInts, interceptors...)
    })
}
```

**서버와의 차이**: 서버의 `UnaryInterceptor`는 중복 호출 시 panic을 발생시키지만, 클라이언트의 `WithUnaryInterceptor`는 조용히 덮어쓴다. 이는 서버가 보통 한 곳에서 설정되는 반면, 클라이언트 옵션은 여러 소스에서 합쳐질 수 있기 때문이다.

다이얼 옵션 구조체:
```go
// grpc-go/dialoptions.go:69-74
type dialOptions struct {
    unaryInt    UnaryClientInterceptor      // 단일 인터셉터
    streamInt   StreamClientInterceptor     // 단일 인터셉터
    chainUnaryInts  []UnaryClientInterceptor  // 체인 인터셉터 슬라이스
    chainStreamInts []StreamClientInterceptor // 체인 인터셉터 슬라이스
    // ...
}
```

---

## 5. 체이닝 메커니즘 구현 분석

체이닝은 `NewServer()`와 `NewClient()` 시점에 수행되며, 여러 개의 인터셉터를 **단일 인터셉터 함수**로 합성한다.

### 5.1 서버 Unary 인터셉터 체이닝

```go
// grpc-go/server.go:1207-1226
func chainUnaryServerInterceptors(s *Server) {
    // 1. unaryInt가 있으면 chainUnaryInts 앞에 prepend
    interceptors := s.opts.chainUnaryInts
    if s.opts.unaryInt != nil {
        interceptors = append([]UnaryServerInterceptor{s.opts.unaryInt}, s.opts.chainUnaryInts...)
    }

    // 2. 개수에 따라 분기
    var chainedInt UnaryServerInterceptor
    if len(interceptors) == 0 {
        chainedInt = nil           // 인터셉터 없음
    } else if len(interceptors) == 1 {
        chainedInt = interceptors[0]  // 단일: 그대로 사용
    } else {
        chainedInt = chainUnaryInterceptors(interceptors)  // 복수: 체이닝
    }

    // 3. 결과를 다시 unaryInt에 저장 (단일 함수로 통합)
    s.opts.unaryInt = chainedInt
}
```

핵심 체이닝 로직:
```go
// grpc-go/server.go:1228-1241
func chainUnaryInterceptors(interceptors []UnaryServerInterceptor) UnaryServerInterceptor {
    return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (any, error) {
        // 첫 번째 인터셉터를 호출하되, handler를 체인 핸들러로 교체
        return interceptors[0](ctx, req, info, getChainUnaryHandler(interceptors, 0, info, handler))
    }
}

func getChainUnaryHandler(interceptors []UnaryServerInterceptor, curr int, info *UnaryServerInfo, finalHandler UnaryHandler) UnaryHandler {
    if curr == len(interceptors)-1 {
        return finalHandler  // 마지막 인터셉터: 실제 핸들러 반환
    }
    return func(ctx context.Context, req any) (any, error) {
        // 다음 인터셉터를 호출, handler는 또다시 체인 핸들러
        return interceptors[curr+1](ctx, req, info, getChainUnaryHandler(interceptors, curr+1, info, finalHandler))
    }
}
```

이 재귀적 체이닝을 그림으로 표현하면:

```
interceptors = [A, B, C]
finalHandler = serviceMethod

chainUnaryInterceptors 호출 결과:

호출 시점:
  interceptors[0](ctx, req, info, handler_0)
                                      │
  handler_0 = func(ctx, req) {        │
      interceptors[1](ctx, req, info, handler_1)
                                          │
      handler_1 = func(ctx, req) {        │
          interceptors[2](ctx, req, info, finalHandler)
                                              │
          finalHandler = serviceMethod ◄──────┘
      }
  }

실행 흐름:
  A.before → B.before → C.before → serviceMethod → C.after → B.after → A.after
```

### 5.2 클라이언트 Unary 인터셉터 체이닝

```go
// grpc-go/clientconn.go:517-536
func chainUnaryClientInterceptors(cc *ClientConn) {
    interceptors := cc.dopts.chainUnaryInts
    if cc.dopts.unaryInt != nil {
        interceptors = append([]UnaryClientInterceptor{cc.dopts.unaryInt}, interceptors...)
    }
    var chainedInt UnaryClientInterceptor
    if len(interceptors) == 0 {
        chainedInt = nil
    } else if len(interceptors) == 1 {
        chainedInt = interceptors[0]
    } else {
        chainedInt = func(ctx context.Context, method string, req, reply any, cc *ClientConn, invoker UnaryInvoker, opts ...CallOption) error {
            return interceptors[0](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, 0, invoker), opts...)
        }
    }
    cc.dopts.unaryInt = chainedInt
}

// grpc-go/clientconn.go:538-546
func getChainUnaryInvoker(interceptors []UnaryClientInterceptor, curr int, finalInvoker UnaryInvoker) UnaryInvoker {
    if curr == len(interceptors)-1 {
        return finalInvoker
    }
    return func(ctx context.Context, method string, req, reply any, cc *ClientConn, opts ...CallOption) error {
        return interceptors[curr+1](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, curr+1, finalInvoker), opts...)
    }
}
```

서버와 동일한 재귀 패턴이지만, 매개변수가 클라이언트 컨텍스트에 맞춰져 있다 (`UnaryInvoker` vs `UnaryHandler`).

### 5.3 서버 Stream 인터셉터 체이닝

```go
// grpc-go/server.go:1537-1571
func chainStreamServerInterceptors(s *Server) {
    interceptors := s.opts.chainStreamInts
    if s.opts.streamInt != nil {
        interceptors = append([]StreamServerInterceptor{s.opts.streamInt}, s.opts.chainStreamInts...)
    }
    var chainedInt StreamServerInterceptor
    if len(interceptors) == 0 {
        chainedInt = nil
    } else if len(interceptors) == 1 {
        chainedInt = interceptors[0]
    } else {
        chainedInt = chainStreamInterceptors(interceptors)
    }
    s.opts.streamInt = chainedInt
}

func chainStreamInterceptors(interceptors []StreamServerInterceptor) StreamServerInterceptor {
    return func(srv any, ss ServerStream, info *StreamServerInfo, handler StreamHandler) error {
        return interceptors[0](srv, ss, info, getChainStreamHandler(interceptors, 0, info, handler))
    }
}

func getChainStreamHandler(interceptors []StreamServerInterceptor, curr int, info *StreamServerInfo, finalHandler StreamHandler) StreamHandler {
    if curr == len(interceptors)-1 {
        return finalHandler
    }
    return func(srv any, stream ServerStream) error {
        return interceptors[curr+1](srv, stream, info, getChainStreamHandler(interceptors, curr+1, info, finalHandler))
    }
}
```

### 5.4 클라이언트 Stream 인터셉터 체이닝

```go
// grpc-go/clientconn.go:548-577
func chainStreamClientInterceptors(cc *ClientConn) {
    interceptors := cc.dopts.chainStreamInts
    if cc.dopts.streamInt != nil {
        interceptors = append([]StreamClientInterceptor{cc.dopts.streamInt}, interceptors...)
    }
    var chainedInt StreamClientInterceptor
    if len(interceptors) == 0 {
        chainedInt = nil
    } else if len(interceptors) == 1 {
        chainedInt = interceptors[0]
    } else {
        chainedInt = func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, streamer Streamer, opts ...CallOption) (ClientStream, error) {
            return interceptors[0](ctx, desc, cc, method, getChainStreamer(interceptors, 0, streamer), opts...)
        }
    }
    cc.dopts.streamInt = chainedInt
}

func getChainStreamer(interceptors []StreamClientInterceptor, curr int, finalStreamer Streamer) Streamer {
    if curr == len(interceptors)-1 {
        return finalStreamer
    }
    return func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error) {
        return interceptors[curr+1](ctx, desc, cc, method, getChainStreamer(interceptors, curr+1, finalStreamer), opts...)
    }
}
```

### 5.5 체이닝 최적화

4가지 체이닝 함수 모두 동일한 최적화 패턴을 사용한다:

```
인터셉터 0개: chainedInt = nil        → 인터셉터 호출 자체를 건너뜀
인터셉터 1개: chainedInt = interceptors[0]  → 래핑 없이 직접 사용
인터셉터 N개: chainedInt = chain(...)  → 재귀 체이닝
```

왜 이 최적화가 중요한가? 인터셉터가 없는 경우(대부분의 간단한 서버)에 불필요한 함수 호출과 클로저 생성을 피할 수 있다. 인터셉터가 1개인 경우도 래핑 함수 없이 직접 호출함으로써 가비지 생성을 줄인다.

---

## 6. 실행 순서: 등록 순서와 실행 순서의 관계

### 6.1 기본 규칙

```
WithUnaryInterceptor(A)              → 항상 가장 먼저 실행 (prepend)
WithChainUnaryInterceptor(B, C, D)   → A 다음, 등록 순서대로
WithChainUnaryInterceptor(E)         → D 다음에 추가 (append)

최종 실행 순서: A → B → C → D → E → 실제 RPC
```

이 순서는 체이닝 함수에서 prepend 로직으로 보장된다:

```go
// grpc-go/server.go:1211-1213
if s.opts.unaryInt != nil {
    interceptors = append([]UnaryServerInterceptor{s.opts.unaryInt}, s.opts.chainUnaryInts...)
}
```

### 6.2 구체적 예시

```go
// 서버 설정
s := grpc.NewServer(
    grpc.UnaryInterceptor(authInterceptor),         // [0] 인증
    grpc.ChainUnaryInterceptor(
        loggingInterceptor,                          // [1] 로깅
        metricsInterceptor,                          // [2] 메트릭
    ),
    grpc.ChainUnaryInterceptor(
        recoveryInterceptor,                         // [3] 복구
    ),
)
```

내부 처리:
```
1. serverOptions.unaryInt = authInterceptor
2. serverOptions.chainUnaryInts = [loggingInterceptor, metricsInterceptor, recoveryInterceptor]

chainUnaryServerInterceptors() 호출:
3. interceptors = [authInterceptor, loggingInterceptor, metricsInterceptor, recoveryInterceptor]
4. chainedInt = chainUnaryInterceptors(interceptors)
5. s.opts.unaryInt = chainedInt  (4개를 합친 단일 함수)
```

실행 순서 (양파 모델):
```
요청 ──►  auth.before
              │
              ▼
          logging.before
                │
                ▼
            metrics.before
                  │
                  ▼
              recovery.before
                    │
                    ▼
               [서비스 메서드]
                    │
                    ▼
              recovery.after
                  │
                  ▼
            metrics.after
                │
                ▼
          logging.after
              │
              ▼
          auth.after  ──► 응답
```

### 6.3 클라이언트 측도 동일

```go
// 클라이언트 설정
conn, _ := grpc.NewClient(target,
    grpc.WithUnaryInterceptor(retryInterceptor),     // [0] 재시도
    grpc.WithChainUnaryInterceptor(
        loggingInterceptor,                           // [1] 로깅
        timeoutInterceptor,                           // [2] 타임아웃
    ),
)
```

```
// grpc-go/clientconn.go:519-523
interceptors := cc.dopts.chainUnaryInts
if cc.dopts.unaryInt != nil {
    interceptors = append([]UnaryClientInterceptor{cc.dopts.unaryInt}, interceptors...)
}
// interceptors = [retryInterceptor, loggingInterceptor, timeoutInterceptor]
```

---

## 7. 서버 인터셉터 실행 흐름

### 7.1 Unary 서버 인터셉터

서버가 Unary RPC를 수신하면 `processUnaryRPC`가 호출된다.

```go
// grpc-go/server.go:1426
reply, appErr := md.Handler(info.serviceImpl, ctx, df, s.opts.unaryInt)
```

여기서 `md.Handler`는 protoc이 생성한 핸들러 함수이다:

```go
// grpc-go/server.go:96
type MethodHandler func(srv any, ctx context.Context, dec func(any) error, interceptor UnaryServerInterceptor) (any, error)
```

protoc 생성 코드의 실제 모습 (예시):
```go
// reflection/grpc_testing/test_grpc.pb.go:122-138
func _SearchService_Search_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
    in := new(SearchRequest)
    if err := dec(in); err != nil {
        return nil, err
    }
    if interceptor == nil {
        return srv.(SearchServiceServer).Search(ctx, in)  // 인터셉터 없으면 직접 호출
    }
    info := &grpc.UnaryServerInfo{
        Server:     srv,
        FullMethod: SearchService_Search_FullMethodName,
    }
    handler := func(ctx context.Context, req interface{}) (interface{}, error) {
        return srv.(SearchServiceServer).Search(ctx, req.(*SearchRequest))
    }
    return interceptor(ctx, in, info, handler)  // 인터셉터에 위임
}
```

전체 흐름:

```
네트워크 수신
     │
     ▼
processUnaryRPC()                     (server.go:1243)
     │
     ├── statsHandler.HandleRPC()     (통계 시작)
     ├── recvAndDecompress()          (메시지 수신 + 역압축)
     │
     ▼
md.Handler(srv, ctx, df, s.opts.unaryInt)   (server.go:1426)
     │
     ▼
[protoc 생성 코드]
     │
     ├── dec(in)  ─── df() ───┐       (메시지 역직렬화)
     │                        │
     │   if interceptor == nil │
     │       └── srv.Method() │       (직접 호출)
     │   else                 │
     │       └── interceptor(ctx, in, info, handler)
     │                │
     │                ▼
     │         chainedInterceptor[0]
     │                │
     │                ▼
     │         chainedInterceptor[1]
     │                │
     │                ▼
     │         ...
     │                │
     │                ▼
     │         handler(ctx, req)       (실제 서비스 메서드)
     │
     ▼
응답 직렬화 + 전송
```

### 7.2 Stream 서버 인터셉터

```go
// grpc-go/server.go:1711-1720
if s.opts.streamInt == nil {
    appErr = sd.Handler(server, ss)              // 인터셉터 없으면 직접 호출
} else {
    info := &StreamServerInfo{
        FullMethod:     stream.Method(),
        IsClientStream: sd.ClientStreams,
        IsServerStream: sd.ServerStreams,
    }
    appErr = s.opts.streamInt(server, ss, info, sd.Handler)  // 인터셉터에 위임
}
```

Unary와의 차이: protoc 생성 코드가 아닌 `processStreamingRPC`에서 직접 인터셉터를 호출한다. `sd.Handler`는 `StreamHandler` 타입(`func(srv any, stream ServerStream) error`)이다.

---

## 8. 클라이언트 인터셉터 실행 흐름

### 8.1 Unary 클라이언트 인터셉터

```go
// grpc-go/call.go:29-38
func (cc *ClientConn) Invoke(ctx context.Context, method string, args, reply any, opts ...CallOption) error {
    // 기본 옵션 + 호출별 옵션 합치기
    opts = combine(cc.dopts.callOptions, opts)

    if cc.dopts.unaryInt != nil {
        return cc.dopts.unaryInt(ctx, method, args, reply, cc, invoke, opts...)
    }
    return invoke(ctx, method, args, reply, cc, opts...)
}
```

여기서 `invoke`가 실제 RPC를 수행하는 `finalInvoker`이다:
```go
// grpc-go/call.go:65-74
func invoke(ctx context.Context, method string, req, reply any, cc *ClientConn, opts ...CallOption) error {
    cs, err := newClientStream(ctx, unaryStreamDesc, cc, method, opts...)
    if err != nil {
        return err
    }
    if err := cs.SendMsg(req); err != nil {
        return err
    }
    return cs.RecvMsg(reply)
}
```

전체 흐름:

```
Application Code
     │
     ▼
cc.Invoke(ctx, method, args, reply, opts...)    (call.go:29)
     │
     ├── combine(defaultOpts, callOpts)          (옵션 합치기)
     │
     ├── if unaryInt != nil:
     │       unaryInt(ctx, method, args, reply, cc, invoke, opts...)
     │            │
     │            ▼
     │       chainedInterceptor[0]
     │            │
     │            ▼
     │       chainedInterceptor[1]
     │            │
     │            ▼
     │       ...
     │            │
     │            ▼
     │       invoke(ctx, method, req, reply, cc, opts...)  ← finalInvoker
     │            │
     │            ├── newClientStream()
     │            ├── cs.SendMsg(req)
     │            └── cs.RecvMsg(reply)
     │
     └── else:
             invoke(ctx, method, args, reply, cc, opts...)  (직접 호출)
```

### 8.2 Stream 클라이언트 인터셉터

```go
// grpc-go/stream.go:166-175
func (cc *ClientConn) NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error) {
    opts = combine(cc.dopts.callOptions, opts)

    if cc.dopts.streamInt != nil {
        return cc.dopts.streamInt(ctx, desc, cc, method, newClientStream, opts...)
    }
    return newClientStream(ctx, desc, cc, method, opts...)
}
```

`newClientStream`이 `finalStreamer` 역할을 한다. 인터셉터 체인의 마지막에서 호출되어 실제 gRPC 스트림을 생성한다.

---

## 9. 스트리밍 인터셉터의 차이점

### 9.1 근본적 차이: 생명주기

| 특성 | Unary 인터셉터 | Stream 인터셉터 |
|------|----------------|-----------------|
| 실행 시점 | 요청마다 한 번 | 스트림 생성 시 한 번 |
| 메시지 접근 | req/reply 직접 접근 | Stream 래핑으로 간접 접근 |
| 반환 시점 | 응답 완료 후 | 스트림 종료 후 |
| 메시지 수정 | 매개변수로 가능 | SendMsg/RecvMsg 래핑 필요 |

### 9.2 스트리밍에서 메시지를 가로채려면

Unary 인터셉터는 `req`와 `reply`가 매개변수로 주어지므로 직접 접근/수정이 가능하다. 반면 스트리밍 인터셉터에서 개별 메시지를 가로채려면 `ServerStream` 또는 `ClientStream`을 래핑해야 한다.

```go
// 서버 스트리밍 인터셉터에서 메시지 가로채기 패턴
type wrappedServerStream struct {
    grpc.ServerStream
}

func (w *wrappedServerStream) RecvMsg(m interface{}) error {
    // 수신 전 처리
    err := w.ServerStream.RecvMsg(m)
    // 수신 후 처리 (로깅 등)
    return err
}

func (w *wrappedServerStream) SendMsg(m interface{}) error {
    // 송신 전 처리
    err := w.ServerStream.SendMsg(m)
    // 송신 후 처리
    return err
}

func myStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
    wrapped := &wrappedServerStream{ss}
    return handler(srv, wrapped)  // 래핑된 스트림 전달
}
```

### 9.3 StreamServerInfo의 스트림 타입 구분

```go
// grpc-go/interceptor.go:91-98
type StreamServerInfo struct {
    FullMethod     string
    IsClientStream bool
    IsServerStream bool
}
```

4가지 조합:

| IsClientStream | IsServerStream | RPC 타입 |
|----------------|----------------|----------|
| false | true | Server Streaming |
| true | false | Client Streaming |
| true | true | Bidirectional Streaming |
| false | false | (해당 없음: Unary는 별도 인터셉터 사용) |

이 정보를 활용하면 스트리밍 인터셉터에서 RPC 타입별로 다른 동작을 구현할 수 있다:

```go
func streamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
    if info.IsClientStream && info.IsServerStream {
        // 양방향 스트리밍: 특별 처리
    } else if info.IsServerStream {
        // 서버 스트리밍: 응답 메시지 카운트
    }
    return handler(srv, ss)
}
```

---

## 10. 일반적인 인터셉터 패턴

### 10.1 로깅 인터셉터

```go
func loggingUnaryServerInterceptor(
    ctx context.Context,
    req any,
    info *grpc.UnaryServerInfo,
    handler grpc.UnaryHandler,
) (any, error) {
    start := time.Now()
    log.Printf("[gRPC] 시작: %s", info.FullMethod)

    // 다음 핸들러 호출
    resp, err := handler(ctx, req)

    // 완료 후 로깅
    duration := time.Since(start)
    if err != nil {
        log.Printf("[gRPC] 실패: %s, 소요: %v, 에러: %v", info.FullMethod, duration, err)
    } else {
        log.Printf("[gRPC] 성공: %s, 소요: %v", info.FullMethod, duration)
    }
    return resp, err
}
```

### 10.2 인증 인터셉터

```go
func authUnaryServerInterceptor(
    ctx context.Context,
    req any,
    info *grpc.UnaryServerInfo,
    handler grpc.UnaryHandler,
) (any, error) {
    // 메타데이터에서 토큰 추출
    md, ok := metadata.FromIncomingContext(ctx)
    if !ok {
        return nil, status.Error(codes.Unauthenticated, "메타데이터 없음")
    }

    tokens := md.Get("authorization")
    if len(tokens) == 0 {
        return nil, status.Error(codes.Unauthenticated, "인증 토큰 없음")
    }

    // 토큰 검증
    userID, err := validateToken(tokens[0])
    if err != nil {
        return nil, status.Error(codes.Unauthenticated, "잘못된 토큰")
    }

    // 검증된 사용자 정보를 컨텍스트에 추가
    newCtx := context.WithValue(ctx, "userID", userID)
    return handler(newCtx, req)  // 수정된 컨텍스트로 계속
}
```

핵심: 인터셉터는 `handler`를 호출하지 않고 에러를 반환함으로써 RPC를 거부할 수 있다. 또한 컨텍스트를 수정하여 하위 체인과 서비스 메서드에 정보를 전달할 수 있다.

### 10.3 메트릭 인터셉터

```go
func metricsUnaryServerInterceptor(
    ctx context.Context,
    req any,
    info *grpc.UnaryServerInfo,
    handler grpc.UnaryHandler,
) (any, error) {
    start := time.Now()

    // 요청 카운터 증가
    requestCounter.WithLabelValues(info.FullMethod).Inc()

    resp, err := handler(ctx, req)

    // 지연시간 기록
    duration := time.Since(start).Seconds()
    requestDuration.WithLabelValues(info.FullMethod).Observe(duration)

    // 상태 코드별 카운터
    code := status.Code(err)
    responseCounter.WithLabelValues(info.FullMethod, code.String()).Inc()

    return resp, err
}
```

### 10.4 복구(Recovery) 인터셉터

```go
func recoveryUnaryServerInterceptor(
    ctx context.Context,
    req any,
    info *grpc.UnaryServerInfo,
    handler grpc.UnaryHandler,
) (resp any, err error) {
    defer func() {
        if r := recover(); r != nil {
            // 스택 트레이스 수집
            stack := debug.Stack()
            log.Printf("[PANIC] %s: %v\n%s", info.FullMethod, r, string(stack))

            // panic을 gRPC 에러로 변환
            err = status.Errorf(codes.Internal, "내부 서버 오류")
        }
    }()
    return handler(ctx, req)
}
```

왜 recovery 인터셉터가 필요한가? gRPC 서버에서 panic이 발생하면 해당 goroutine이 종료되지만, 서버 전체가 죽지는 않는다. 그러나 클라이언트는 연결이 끊어진 것으로 인식한다. recovery 인터셉터는 panic을 적절한 gRPC 에러 코드로 변환하여 클라이언트에 명확한 에러 응답을 반환한다.

### 10.5 재시도 인터셉터 (클라이언트)

```go
func retryUnaryClientInterceptor(maxRetries int) grpc.UnaryClientInterceptor {
    return func(
        ctx context.Context,
        method string,
        req, reply any,
        cc *grpc.ClientConn,
        invoker grpc.UnaryInvoker,
        opts ...grpc.CallOption,
    ) error {
        var lastErr error
        for attempt := 0; attempt <= maxRetries; attempt++ {
            lastErr = invoker(ctx, method, req, reply, cc, opts...)
            if lastErr == nil {
                return nil
            }

            // 재시도 가능한 에러인지 확인
            code := status.Code(lastErr)
            if code != codes.Unavailable && code != codes.ResourceExhausted {
                return lastErr  // 재시도 불가능한 에러
            }

            // 지수 백오프
            backoff := time.Duration(math.Pow(2, float64(attempt))) * 100 * time.Millisecond
            select {
            case <-ctx.Done():
                return ctx.Err()
            case <-time.After(backoff):
            }
        }
        return lastErr
    }
}
```

### 10.6 검증 인터셉터

```go
// 인터페이스 기반 검증 패턴
type Validator interface {
    Validate() error
}

func validationUnaryServerInterceptor(
    ctx context.Context,
    req any,
    info *grpc.UnaryServerInfo,
    handler grpc.UnaryHandler,
) (any, error) {
    if v, ok := req.(Validator); ok {
        if err := v.Validate(); err != nil {
            return nil, status.Errorf(codes.InvalidArgument, "검증 실패: %v", err)
        }
    }
    return handler(ctx, req)
}
```

### 10.7 권장 인터셉터 등록 순서

```go
s := grpc.NewServer(
    grpc.ChainUnaryInterceptor(
        recoveryInterceptor,    // [1] 가장 바깥: panic 복구
        loggingInterceptor,     // [2] 모든 요청 로깅
        metricsInterceptor,     // [3] 메트릭 수집
        authInterceptor,        // [4] 인증 (인증 실패 시 이후 실행 안함)
        validationInterceptor,  // [5] 요청 검증
    ),
)
```

순서의 이유:
- **recovery가 가장 바깥**: 어떤 인터셉터에서 panic이 나도 잡을 수 있다
- **logging이 그 다음**: 인증 실패도 로깅해야 한다
- **metrics가 그 다음**: 인증 실패 요청도 카운트해야 한다
- **auth가 뒤쪽**: 인증 실패 시 validation과 서비스 메서드를 실행하지 않는다
- **validation이 가장 안쪽**: 인증된 요청만 검증한다

---

## 11. 인터셉터 vs StatsHandler 비교

gRPC-Go는 인터셉터 외에도 `stats.Handler`라는 별도의 관찰 메커니즘을 제공한다.

```go
// grpc-go/stats/handlers.go:53-72
type Handler interface {
    TagRPC(context.Context, *RPCTagInfo) context.Context
    HandleRPC(context.Context, RPCStats)
    TagConn(context.Context, *ConnTagInfo) context.Context
    HandleConn(context.Context, ConnStats)
}
```

### 상세 비교

| 특성 | 인터셉터 | StatsHandler |
|------|----------|-------------|
| **목적** | 요청 흐름 제어 (미들웨어) | 관찰/통계 (옵저버) |
| **흐름 제어** | handler/invoker를 호출하지 않아 RPC 거부 가능 | RPC 흐름 제어 불가 |
| **요청/응답 수정** | 가능 (req, ctx 수정) | 불가 (읽기 전용) |
| **연결 레벨 이벤트** | 불가 | TagConn/HandleConn으로 가능 |
| **이벤트 세분화** | before/after 2단계 | Begin, InPayload, OutPayload, End 등 다단계 |
| **복수 등록** | WithChain*으로 체이닝 | WithStatsHandler 여러 번 호출 가능 |
| **적용 범위** | RPC 레벨 | RPC + 연결 레벨 |
| **등록 방법 (서버)** | ChainUnaryInterceptor | grpc.StatsHandler(h) |
| **등록 방법 (클라이언트)** | WithChainUnaryInterceptor | grpc.WithStatsHandler(h) |

### StatsHandler가 더 적합한 경우

1. **순수 관찰**: RPC 흐름을 바꾸지 않고 통계만 수집
2. **연결 관리**: 새 연결/연결 종료 이벤트 추적
3. **세밀한 이벤트**: 메시지별 페이로드 크기, 와이어 크기 등
4. **OpenTelemetry 연동**: otel 계측은 StatsHandler를 선호

### 인터셉터가 더 적합한 경우

1. **인증/인가**: 요청을 거부해야 하는 경우
2. **요청 수정**: 컨텍스트에 정보 추가, 메타데이터 수정
3. **에러 변환**: 내부 에러를 gRPC 상태 코드로 변환
4. **재시도**: 실패 시 재시도 로직
5. **캐싱**: 특정 요청에 대해 캐시된 응답 반환

### 호출 시점 비교

```
서버 processUnaryRPC에서:

1. statsHandler.HandleRPC(ctx, &stats.Begin{...})     ← StatsHandler 시작
2. recvAndDecompress()                                 ← 메시지 수신
3. statsHandler.HandleRPC(ctx, &stats.InPayload{...})  ← StatsHandler 수신
4. md.Handler(srv, ctx, df, s.opts.unaryInt)           ← 인터셉터 + 서비스 메서드
   └── interceptor chain → service method
5. statsHandler.HandleRPC(ctx, &stats.OutPayload{...}) ← StatsHandler 송신
6. statsHandler.HandleRPC(ctx, &stats.End{...})        ← StatsHandler 종료
```

핵심: StatsHandler는 인터셉터보다 더 넓은 범위를 커버한다. 메시지 역직렬화 전후, 네트워크 전송 전후의 이벤트까지 포착한다. 반면 인터셉터는 역직렬화된 메시지가 주어진 상태에서 동작한다.

---

## 12. 설계 결정: 왜(Why) 이렇게 만들었는가

### 12.1 왜 체이닝을 프레임워크가 제공하는가?

초기 gRPC-Go(v1.28 이전)에는 서버/클라이언트 각각 하나의 인터셉터만 등록할 수 있었다. 복수 인터셉터 체이닝은 `go-grpc-middleware` 같은 외부 라이브러리에 의존했다.

이것이 문제가 된 이유:
- 모든 프로젝트가 동일한 체이닝 로직을 반복 구현
- 체이닝 순서에 대한 버그 발생 가능성
- 서드파티 의존성 추가 필요

v1.28부터 `ChainUnaryInterceptor`/`ChainStreamInterceptor`가 공식 제공되어, 외부 의존성 없이 복수 인터셉터를 등록할 수 있게 되었다. 하위 호환성을 위해 기존 `UnaryInterceptor`/`StreamInterceptor`도 유지된다.

### 12.2 왜 재귀적 체이닝인가?

```go
// 재귀적 접근 (gRPC-Go 실제 구현)
func getChainUnaryHandler(interceptors []UnaryServerInterceptor, curr int, info *UnaryServerInfo, finalHandler UnaryHandler) UnaryHandler {
    if curr == len(interceptors)-1 {
        return finalHandler
    }
    return func(ctx context.Context, req any) (any, error) {
        return interceptors[curr+1](ctx, req, info, getChainUnaryHandler(interceptors, curr+1, info, finalHandler))
    }
}
```

대안 1: 반복적(iterative) 접근으로 미리 모든 핸들러를 생성할 수 있다. 그러나 재귀적 접근은:
- 클로저 생성을 **호출 시점까지 지연**한다 (lazy evaluation)
- 인터셉터가 `handler`를 호출하지 않으면 이후 클로저가 생성되지 않는다
- 코드가 더 간결하다

대안 2: 슬라이스 순회 방식은 인터셉터가 `handler`를 여러 번 호출하는 경우(재시도) 인덱스 관리가 복잡해진다. 재귀적 클로저는 각 호출이 독립적인 스코프를 가져 이 문제가 없다.

### 12.3 왜 protoc 생성 코드에서 인터셉터를 호출하는가? (서버 Unary)

```go
// protoc 생성 코드
func _SearchService_Search_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
    in := new(SearchRequest)
    if err := dec(in); err != nil { return nil, err }
    if interceptor == nil {
        return srv.(SearchServiceServer).Search(ctx, in)
    }
    // ...
    return interceptor(ctx, in, info, handler)
}
```

인터셉터가 **역직렬화된 메시지**를 받도록 설계한 이유:
- 인터셉터에서 타입 안전한 요청 접근이 가능하다
- 검증 인터셉터가 메시지 필드를 검사할 수 있다
- 역직렬화 비용은 인터셉터 유무와 관계없이 항상 발생한다

반면 스트리밍에서는 `processStreamingRPC`에서 직접 인터셉터를 호출한다:
```go
// server.go:1711-1719
if s.opts.streamInt == nil {
    appErr = sd.Handler(server, ss)
} else {
    info := &StreamServerInfo{...}
    appErr = s.opts.streamInt(server, ss, info, sd.Handler)
}
```

이는 스트리밍에서 메시지가 스트림 생명주기 동안 지속적으로 교환되기 때문이다. protoc 코드에서 "한 번에" 처리할 수 없다.

### 12.4 왜 WithUnaryInterceptor와 WithChainUnaryInterceptor를 분리했는가?

**하위 호환성**: `WithUnaryInterceptor`는 gRPC-Go 초기부터 존재했다. 이미 이 API를 사용하는 코드가 많으므로 제거할 수 없다.

**명확한 의미**:
- `WithUnaryInterceptor`: "이것이 나의 인터셉터이다" (단일)
- `WithChainUnaryInterceptor`: "이 인터셉터들을 체인에 추가하라" (복수)

**실행 순서 보장**: `WithUnaryInterceptor`로 등록한 것이 항상 체인의 맨 앞에 온다. 이를 통해 라이브러리가 `WithChainUnaryInterceptor`로 인터셉터를 추가하더라도, 사용자가 `WithUnaryInterceptor`로 등록한 것이 항상 우선 실행된다.

### 12.5 왜 체이닝 결과를 단일 인터셉터로 저장하는가?

```go
// chainUnaryServerInterceptors 마지막 줄 (server.go:1225)
s.opts.unaryInt = chainedInt
```

체이닝 후 결과를 `unaryInt` 필드에 다시 저장하는 이유:
- **호출 경로 단순화**: `processUnaryRPC`에서 `s.opts.unaryInt`만 확인하면 된다
- **nil 체크 최적화**: 인터셉터가 없으면 `unaryInt == nil`로 빠르게 건너뛸 수 있다
- **기존 코드 변경 최소화**: 체이닝 기능 추가 시 호출 측 코드를 수정할 필요가 없다

---

## 부록: 인터셉터 관련 소스코드 위치 요약

| 파일 | 라인 | 내용 |
|------|------|------|
| `interceptor.go` | 26 | `UnaryInvoker` 타입 정의 |
| `interceptor.go` | 43 | `UnaryClientInterceptor` 타입 정의 |
| `interceptor.go` | 46 | `Streamer` 타입 정의 |
| `interceptor.go` | 63 | `StreamClientInterceptor` 타입 정의 |
| `interceptor.go` | 67-72 | `UnaryServerInfo` 구조체 |
| `interceptor.go` | 81 | `UnaryHandler` 타입 정의 |
| `interceptor.go` | 87 | `UnaryServerInterceptor` 타입 정의 |
| `interceptor.go` | 91-98 | `StreamServerInfo` 구조체 |
| `interceptor.go` | 108 | `StreamServerInterceptor` 타입 정의 |
| `server.go` | 96 | `MethodHandler` 타입 정의 |
| `server.go` | 154-162 | `serverOptions` 인터셉터 필드 |
| `server.go` | 468-475 | `UnaryInterceptor()` 서버 옵션 |
| `server.go` | 481-485 | `ChainUnaryInterceptor()` 서버 옵션 |
| `server.go` | 489-496 | `StreamInterceptor()` 서버 옵션 |
| `server.go` | 502-505 | `ChainStreamInterceptor()` 서버 옵션 |
| `server.go` | 705-706 | `NewServer`에서 체이닝 호출 |
| `server.go` | 1207-1226 | `chainUnaryServerInterceptors` |
| `server.go` | 1228-1241 | `chainUnaryInterceptors` + `getChainUnaryHandler` |
| `server.go` | 1426 | `processUnaryRPC`에서 인터셉터 호출 |
| `server.go` | 1537-1556 | `chainStreamServerInterceptors` |
| `server.go` | 1558-1571 | `chainStreamInterceptors` + `getChainStreamHandler` |
| `server.go` | 1711-1720 | `processStreamingRPC`에서 인터셉터 호출 |
| `stream.go` | 62 | `StreamHandler` 타입 정의 |
| `stream.go` | 166-175 | `NewStream`에서 스트림 인터셉터 호출 |
| `clientconn.go` | 69-74 | `dialOptions` 인터셉터 필드 |
| `clientconn.go` | 222-223 | `NewClient`에서 체이닝 호출 |
| `clientconn.go` | 517-536 | `chainUnaryClientInterceptors` |
| `clientconn.go` | 538-546 | `getChainUnaryInvoker` |
| `clientconn.go` | 548-567 | `chainStreamClientInterceptors` |
| `clientconn.go` | 569-577 | `getChainStreamer` |
| `dialoptions.go` | 573-577 | `WithUnaryInterceptor()` |
| `dialoptions.go` | 584-588 | `WithChainUnaryInterceptor()` |
| `dialoptions.go` | 592-596 | `WithStreamInterceptor()` |
| `dialoptions.go` | 603-607 | `WithChainStreamInterceptor()` |
| `call.go` | 29-38 | `Invoke`에서 unary 인터셉터 호출 |
| `call.go` | 65-74 | `invoke` 함수 (finalInvoker) |
| `stats/handlers.go` | 53-72 | `stats.Handler` 인터페이스 |
