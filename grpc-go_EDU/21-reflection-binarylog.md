# 21. 서버 리플렉션 & 바이너리 로깅 심화

## 목차
1. [개요](#1-개요)
2. [서버 리플렉션 아키텍처](#2-서버-리플렉션-아키텍처)
3. [리플렉션 프로토콜 분석](#3-리플렉션-프로토콜-분석)
4. [FileDescriptor 의존성 그래프](#4-filedescriptor-의존성-그래프)
5. [v1/v1alpha 버전 호환성](#5-v1v1alpha-버전-호환성)
6. [바이너리 로깅 아키텍처](#6-바이너리-로깅-아키텍처)
7. [환경변수 기반 필터 설정](#7-환경변수-기반-필터-설정)
8. [MethodLogger와 이벤트 타입](#8-methodlogger와-이벤트-타입)
9. [Sink 계층 구조](#9-sink-계층-구조)
10. [메시지 트런케이션 메커니즘](#10-메시지-트런케이션-메커니즘)
11. [실제 활용 패턴](#11-실제-활용-패턴)
12. [설계 철학과 Why](#12-설계-철학과-why)

---

## 1. 개요

서버 리플렉션(Server Reflection)과 바이너리 로깅(Binary Logging)은 gRPC-Go의 디버깅/운영 서브시스템이다. 리플렉션은 런타임에 서버가 노출하는 서비스 정보를 질의할 수 있게 해주며, 바이너리 로깅은 RPC 메시지를 이진 형태로 기록하여 사후 분석을 가능하게 한다.

### 핵심 소스 경로

| 컴포넌트 | 소스 경로 |
|----------|----------|
| Reflection 패키지 | `reflection/serverreflection.go` |
| Reflection 내부 | `reflection/internal/internal.go` |
| Reflection v1 proto | `reflection/grpc_reflection_v1/` |
| Reflection v1alpha proto | `reflection/grpc_reflection_v1alpha/` |
| BinaryLog 패키지 | `binarylog/sink.go` |
| BinaryLog 내부 | `internal/binarylog/binarylog.go` |
| MethodLogger | `internal/binarylog/method_logger.go` |
| 환경변수 파서 | `internal/binarylog/env_config.go` |
| BinaryLog Sink | `internal/binarylog/sink.go` |

### 이 두 기능을 하나로 묶는 이유

리플렉션과 바이너리 로깅은 모두 **RPC 디버깅/진단**이라는 공통 목적을 가진다. 리플렉션은 "이 서버가 어떤 서비스를 제공하는가?"라는 질문에 답하고, 바이너리 로깅은 "이 RPC에서 어떤 데이터가 오갔는가?"라는 질문에 답한다. 두 기능 모두 프로덕션 환경에서의 문제 진단에 핵심적이다.

---

## 2. 서버 리플렉션 아키텍처

### 2.1 전체 구조

```
┌─────────────────────────────────────────────────────┐
│                   gRPC Server                        │
│                                                      │
│  ┌──────────┐  ┌──────────┐  ┌───────────────────┐  │
│  │ Service A │  │ Service B │  │ Reflection Service│  │
│  └──────────┘  └──────────┘  └───────────────────┘  │
│       │              │              │                 │
│       ▼              ▼              ▼                 │
│  ┌─────────────────────────────────────────────────┐ │
│  │         ServiceRegistrar (grpc.Server)           │ │
│  │   GetServiceInfo() → map[string]ServiceInfo      │ │
│  └─────────────────────────────────────────────────┘ │
│                          │                            │
│                          ▼                            │
│  ┌─────────────────────────────────────────────────┐ │
│  │     protoregistry.GlobalFiles (DescResolver)     │ │
│  │   FindFileByPath() / FindDescriptorByName()      │ │
│  └─────────────────────────────────────────────────┘ │
│                          │                            │
│                          ▼                            │
│  ┌─────────────────────────────────────────────────┐ │
│  │   protoregistry.GlobalTypes (ExtResolver)        │ │
│  │   FindExtensionByNumber() / RangeExtensions()    │ │
│  └─────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────┘
```

### 2.2 등록 과정

`reflection.Register(s)` 호출 시 실행되는 과정:

```
reflection.Register(s GRPCServer)
    │
    ├─ NewServerV1(ServerOptions{Services: s})
    │   ├─ opts.DescriptorResolver = protoregistry.GlobalFiles  (기본값)
    │   └─ opts.ExtensionResolver = protoregistry.GlobalTypes   (기본값)
    │
    ├─ v1alpha.RegisterServerReflectionServer(s, asV1Alpha(svr))
    │   └─ v1alpha 서비스를 서버에 등록
    │
    └─ v1.RegisterServerReflectionServer(s, svr)
        └─ v1 서비스를 서버에 등록
```

소스코드 `reflection/serverreflection.go`에서 확인할 수 있다:

```go
// Register registers the server reflection service on the given gRPC server.
// Both the v1 and v1alpha versions are registered.
func Register(s GRPCServer) {
    svr := NewServerV1(ServerOptions{Services: s})
    v1alphareflectiongrpc.RegisterServerReflectionServer(s, asV1Alpha(svr))
    v1reflectiongrpc.RegisterServerReflectionServer(s, svr)
}
```

### 2.3 핵심 인터페이스

리플렉션 서버가 의존하는 두 가지 핵심 인터페이스:

| 인터페이스 | 역할 | 기본 구현 |
|-----------|------|----------|
| `ServiceInfoProvider` | 등록된 서비스 목록 제공 | `*grpc.Server` |
| `protodesc.Resolver` | proto 파일/심볼 디스크립터 검색 | `protoregistry.GlobalFiles` |
| `ExtensionResolver` | 확장 필드 정보 검색 | `protoregistry.GlobalTypes` |

```go
// ServerOptions represents the options used to construct a reflection server.
type ServerOptions struct {
    Services          ServiceInfoProvider
    DescriptorResolver protodesc.Resolver
    ExtensionResolver  ExtensionResolver
}
```

**왜 이런 설계인가?** 기본적으로 `protoregistry.GlobalFiles`와 `protoregistry.GlobalTypes`를 사용하지만, 사용자가 커스텀 Resolver를 제공하면 서비스 디스크립터 소스를 완전히 교체할 수 있다. 이는 동적으로 proto 파일을 로드하는 시나리오(예: proto 파일을 DB에서 로드)를 지원한다.

---

## 3. 리플렉션 프로토콜 분석

### 3.1 스트리밍 RPC 기반 프로토콜

리플렉션은 **양방향 스트리밍** RPC를 사용한다. 클라이언트가 요청을 보내면 서버가 응답을 스트림으로 보낸다.

```
Client                          Server
  │                               │
  │── ServerReflectionRequest ──→ │
  │   (FileByFilename)            │
  │                               │
  │←── ServerReflectionResponse ──│
  │   (FileDescriptorResponse)    │
  │                               │
  │── ServerReflectionRequest ──→ │
  │   (ListServices)              │
  │                               │
  │←── ServerReflectionResponse ──│
  │   (ListServicesResponse)      │
  │                               │
  │── EOF ────────────────────→   │
  │                               │
```

### 3.2 요청 타입 (5가지)

`reflection/internal/internal.go`의 `ServerReflectionInfo` 메서드에서 처리하는 5가지 요청 타입:

| 요청 타입 | 설명 | 응답 |
|----------|------|------|
| `FileByFilename` | proto 파일명으로 FileDescriptor 검색 | FileDescriptorResponse |
| `FileContainingSymbol` | 심볼명(서비스/메서드/타입)으로 검색 | FileDescriptorResponse |
| `FileContainingExtension` | 확장 필드로 검색 | FileDescriptorResponse |
| `AllExtensionNumbersOfType` | 타입의 모든 확장 번호 조회 | ExtensionNumberResponse |
| `ListServices` | 등록된 모든 서비스 목록 | ListServicesResponse |

### 3.3 핵심 핸들러 흐름

```go
func (s *ServerReflectionServer) ServerReflectionInfo(
    stream v1reflectiongrpc.ServerReflection_ServerReflectionInfoServer,
) error {
    sentFileDescriptors := make(map[string]bool)  // 중복 전송 방지
    for {
        in, err := stream.Recv()
        if err == io.EOF {
            return nil
        }
        // ...
        switch req := in.MessageRequest.(type) {
        case *v1reflectionpb.ServerReflectionRequest_FileByFilename:
            // 파일명으로 검색
        case *v1reflectionpb.ServerReflectionRequest_FileContainingSymbol:
            // 심볼명으로 검색
        case *v1reflectionpb.ServerReflectionRequest_ListServices:
            // 서비스 목록 반환
        // ...
        }
        stream.Send(out)
    }
}
```

**왜 스트리밍인가?** 단일 요청으로는 충분하지 않다. 클라이언트(grpcurl 등)가 서비스 목록을 먼저 받고, 그 중 하나의 서비스에 대한 디스크립터를 요청하고, 다시 의존 파일을 요청하는 식의 **탐색적 질의** 패턴을 지원하기 위해 스트리밍이 필요하다.

---

## 4. FileDescriptor 의존성 그래프

### 4.1 BFS 기반 의존성 수집

리플렉션에서 가장 핵심적인 알고리즘은 `FileDescWithDependencies`이다. 하나의 proto 파일이 import하는 모든 의존 파일을 **BFS(너비 우선 탐색)**로 수집한다.

```go
func (s *ServerReflectionServer) FileDescWithDependencies(
    fd protoreflect.FileDescriptor,
    sentFileDescriptors map[string]bool,
) ([][]byte, error) {
    var r [][]byte
    queue := []protoreflect.FileDescriptor{fd}
    for len(queue) > 0 {
        currentfd := queue[0]
        queue = queue[1:]
        if currentfd.IsPlaceholder() {
            continue  // 누락된 의존 파일은 건너뜀
        }
        if sent := sentFileDescriptors[currentfd.Path()]; len(r) == 0 || !sent {
            sentFileDescriptors[currentfd.Path()] = true
            fdProto := protodesc.ToFileDescriptorProto(currentfd)
            currentfdEncoded, err := proto.Marshal(fdProto)
            // ...
            r = append(r, currentfdEncoded)
        }
        for i := 0; i < currentfd.Imports().Len(); i++ {
            queue = append(queue, currentfd.Imports().Get(i))
        }
    }
    return r, nil
}
```

### 4.2 의존성 그래프 예시

```
google/protobuf/timestamp.proto
        ▲
        │ import
helloworld.proto ──import──→ google/protobuf/duration.proto
        ▲
        │ import
my_service.proto ──import──→ google/protobuf/any.proto
```

BFS 순회 결과: `[my_service.proto, helloworld.proto, google/protobuf/any.proto, google/protobuf/timestamp.proto, google/protobuf/duration.proto]`

### 4.3 sentFileDescriptors의 역할

```
┌─────────────────────────────────────────────────┐
│           sentFileDescriptors (map)              │
│                                                   │
│  Key                           │ Value           │
│  "helloworld.proto"            │ true            │
│  "google/protobuf/any.proto"   │ true            │
│  ...                           │ ...             │
│                                                   │
│  역할: 스트림 생명주기 동안 이미 전송한          │
│        FileDescriptor를 추적하여 중복 전송 방지   │
└─────────────────────────────────────────────────┘
```

**왜 이 최적화가 중요한가?** 여러 서비스가 같은 common.proto를 import할 때, 각 요청마다 같은 파일을 반복 전송하면 대역폭이 낭비된다. `sentFileDescriptors`는 스트림 생명주기 동안 유지되어 이미 전송한 파일은 건너뛴다.

### 4.4 Placeholder 처리

```go
if fd.IsPlaceholder() {
    // If the given root file is a placeholder, treat it
    // as missing instead of serializing it.
    return nil, protoregistry.NotFound
}
```

Placeholder는 아직 resolve되지 않은 proto 파일을 나타낸다. 루트 파일이 placeholder이면 에러를 반환하고, 의존 파일이 placeholder이면 조용히 건너뛴다. 이는 **부분적 proto 레지스트리**에서도 리플렉션이 동작하게 하는 방어적 설계이다.

---

## 5. v1/v1alpha 버전 호환성

### 5.1 두 버전 동시 등록

gRPC Reflection은 v1과 v1alpha 두 버전이 존재한다. `Register()`는 **두 버전 모두 등록**한다:

```go
func Register(s GRPCServer) {
    svr := NewServerV1(ServerOptions{Services: s})
    v1alphareflectiongrpc.RegisterServerReflectionServer(s, asV1Alpha(svr))
    v1reflectiongrpc.RegisterServerReflectionServer(s, svr)
}
```

### 5.2 어댑터 패턴

v1alpha 서비스는 v1 서버를 감싸는 **어댑터**로 구현된다. `adapt.go`의 `asV1Alpha` 함수가 v1 서버를 v1alpha 인터페이스로 변환한다.

```
┌─────────────────────────────────────────┐
│              v1 Server                   │
│  (ServerReflectionServer)               │
│                                          │
│  ServerReflectionInfo(v1.Stream)         │
│                                          │
├─────────────────────────────────────────┤
│     v1Alpha Adapter (asV1Alpha)          │
│                                          │
│  ┌────────────────────────────────────┐  │
│  │  v1alpha Request                   │  │
│  │      │                             │  │
│  │      ▼ V1AlphaToV1Request()        │  │
│  │  v1 Request                        │  │
│  │      │                             │  │
│  │      ▼ v1 Server 처리               │  │
│  │  v1 Response                       │  │
│  │      │                             │  │
│  │      ▼ V1ToV1AlphaResponse()       │  │
│  │  v1alpha Response                  │  │
│  └────────────────────────────────────┘  │
└─────────────────────────────────────────┘
```

### 5.3 변환 함수 분석

`internal.go`에 정의된 4개의 변환 함수:

| 함수 | 방향 | 용도 |
|------|------|------|
| `V1AlphaToV1Request` | v1alpha → v1 | 어댑터에서 수신 요청 변환 |
| `V1ToV1AlphaResponse` | v1 → v1alpha | 어댑터에서 응답 변환 |
| `V1ToV1AlphaRequest` | v1 → v1alpha | OriginalRequest 필드 변환 |
| `V1AlphaToV1Response` | v1alpha → v1 | (역방향 호환용) |

**왜 두 버전을 지원하는가?** v1alpha가 먼저 나왔고 많은 클라이언트(grpcurl 포함)가 v1alpha를 사용한다. v1이 정식 표준이지만 하위 호환을 위해 두 버전 모두 지원해야 한다.

---

## 6. 바이너리 로깅 아키텍처

### 6.1 전체 구조

```
┌─────────────────────────────────────────────────────────────┐
│                    gRPC Client/Server                        │
│                                                              │
│  ┌──────────────────┐     ┌──────────────────────────────┐  │
│  │ RPC Handler      │     │ Binary Logging System         │  │
│  │                  │     │                               │  │
│  │  ┌─────────┐    │     │  ┌─────────────────────────┐  │  │
│  │  │ Unary   │────┼─────┼─→│ GetMethodLogger()       │  │  │
│  │  │ Stream  │    │     │  │   → MethodLogger         │  │  │
│  │  └─────────┘    │     │  └─────────┬───────────────┘  │  │
│  └──────────────────┘     │            │                  │  │
│                           │            ▼                  │  │
│                           │  ┌─────────────────────────┐  │  │
│                           │  │ TruncatingMethodLogger   │  │  │
│                           │  │  - headerMaxLen          │  │  │
│                           │  │  - messageMaxLen         │  │  │
│                           │  │  - callID                │  │  │
│                           │  │  - sequenceID            │  │  │
│                           │  └─────────┬───────────────┘  │  │
│                           │            │                  │  │
│                           │            ▼                  │  │
│                           │  ┌─────────────────────────┐  │  │
│                           │  │ Sink (Write)             │  │  │
│                           │  │  - noopSink              │  │  │
│                           │  │  - writerSink            │  │  │
│                           │  │  - bufferedSink          │  │  │
│                           │  └─────────────────────────┘  │  │
│                           └──────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

### 6.2 초기화 흐름

바이너리 로깅은 `init()` 함수에서 환경변수로 초기화된다:

```go
// internal/binarylog/binarylog.go
func init() {
    const envStr = "GRPC_BINARY_LOG_FILTER"
    configStr := os.Getenv(envStr)
    binLogger = NewLoggerFromConfigString(configStr)
}
```

### 6.3 Logger 인터페이스 계층

```
Logger (interface)
    │
    ├─ GetMethodLogger(methodName) → MethodLogger
    │
    └─ logger (struct)
        ├─ config.All       → 전역 룰 (*MethodLoggerConfig)
        ├─ config.Services  → 서비스별 룰 (map[string]*MethodLoggerConfig)
        ├─ config.Methods   → 메서드별 룰 (map[string]*MethodLoggerConfig)
        └─ config.Blacklist → 블랙리스트 (map[string]struct{})

MethodLogger (interface)
    │
    └─ TruncatingMethodLogger (struct)
        ├─ headerMaxLen   (헤더 최대 길이)
        ├─ messageMaxLen  (메시지 최대 길이)
        ├─ callID         (RPC 호출 고유 ID)
        ├─ idWithinCallGen (시퀀스 ID 생성기)
        └─ sink           (출력 대상)
```

---

## 7. 환경변수 기반 필터 설정

### 7.1 GRPC_BINARY_LOG_FILTER 문법

`internal/binarylog/env_config.go`에서 파싱되는 필터 문법:

| 패턴 | 의미 | 예시 |
|------|------|------|
| `*` | 모든 메서드 로깅 | `*` |
| `*{h}` | 헤더만 로깅 | `*{h}` |
| `*{m:256}` | 메시지 256바이트까지 | `*{m:256}` |
| `*{h:128;m:256}` | 헤더 128, 메시지 256바이트 | `*{h:128;m:256}` |
| `Foo/*` | Foo 서비스 전체 | `Foo/*` |
| `-Foo/Bar` | Foo/Bar 메서드 제외 | `Foo/*,-Foo/Bar` |

### 7.2 필터 우선순위

`GetMethodLogger`에서의 룩업 순서 (가장 구체적인 것이 우선):

```
1. Methods["service/method"]  → 정확히 일치하는 메서드별 룰
2. Blacklist["service/method"] → 블랙리스트에 있으면 nil 반환
3. Services["service"]         → 서비스 와일드카드 룰
4. All                         → 전역 룰 (없으면 nil)
```

```go
func (l *logger) GetMethodLogger(methodName string) MethodLogger {
    s, m, err := grpcutil.ParseMethod(methodName)
    if ml, ok := l.config.Methods[s+"/"+m]; ok {
        return NewTruncatingMethodLogger(ml.Header, ml.Message)
    }
    if _, ok := l.config.Blacklist[s+"/"+m]; ok {
        return nil
    }
    if ml, ok := l.config.Services[s]; ok {
        return NewTruncatingMethodLogger(ml.Header, ml.Message)
    }
    if l.config.All == nil {
        return nil
    }
    return NewTruncatingMethodLogger(l.config.All.Header, l.config.All.Message)
}
```

### 7.3 정규식 기반 파싱

필터 문자열은 정규식으로 파싱된다:

```go
const (
    longMethodConfigRegexpStr = `^([\w./]+)/((?:\w+)|[*])(.+)?$`
    headerConfigRegexpStr     = `^{h(?::(\d+))?}$`
    messageConfigRegexpStr    = `^{m(?::(\d+))?}$`
    headerMessageConfigRegexpStr = `^{h(?::(\d+))?;m(?::(\d+))?}$`
)
```

---

## 8. MethodLogger와 이벤트 타입

### 8.1 7가지 이벤트 타입

각 RPC에서 기록되는 이벤트 타입 (GrpcLogEntry_EventType):

| 이벤트 | 설명 | 클라이언트 | 서버 |
|--------|------|-----------|------|
| `CLIENT_HEADER` | 클라이언트 헤더 전송 | ✅ | ✅ |
| `SERVER_HEADER` | 서버 헤더 전송 | ✅ | ✅ |
| `CLIENT_MESSAGE` | 클라이언트 메시지 | ✅ | ✅ |
| `SERVER_MESSAGE` | 서버 메시지 | ✅ | ✅ |
| `CLIENT_HALF_CLOSE` | 클라이언트 전송 종료 | ✅ | ✅ |
| `SERVER_TRAILER` | 서버 트레일러 (상태 포함) | ✅ | ✅ |
| `CANCEL` | RPC 취소 | ✅ | ✅ |

### 8.2 LogEntry 구조

각 이벤트는 `GrpcLogEntry` protobuf 메시지로 직렬화된다:

```
GrpcLogEntry
├─ timestamp          (기록 시각)
├─ callId             (RPC 호출 고유 ID, atomic 증가)
├─ sequenceIdWithinCall (호출 내 시퀀스 번호)
├─ type               (이벤트 타입)
├─ logger             (LOGGER_CLIENT or LOGGER_SERVER)
├─ payload            (oneof)
│   ├─ ClientHeader   (method, authority, metadata, timeout)
│   ├─ ServerHeader   (metadata)
│   ├─ Message        (length, data)
│   └─ Trailer        (metadata, statusCode, statusMessage, statusDetails)
├─ payloadTruncated   (트런케이션 여부)
└─ peer               (Address: type, address, ipPort)
```

### 8.3 Build 메서드

```go
func (ml *TruncatingMethodLogger) Build(c LogEntryConfig) *binlogpb.GrpcLogEntry {
    m := c.toProto()
    m.Timestamp = timestamppb.Now()
    m.CallId = ml.callID
    m.SequenceIdWithinCall = ml.idWithinCallGen.next()

    switch pay := m.Payload.(type) {
    case *binlogpb.GrpcLogEntry_ClientHeader:
        m.PayloadTruncated = ml.truncateMetadata(pay.ClientHeader.GetMetadata())
    case *binlogpb.GrpcLogEntry_ServerHeader:
        m.PayloadTruncated = ml.truncateMetadata(pay.ServerHeader.GetMetadata())
    case *binlogpb.GrpcLogEntry_Message:
        m.PayloadTruncated = ml.truncateMessage(pay.Message)
    }
    return m
}
```

### 8.4 CallID 생성기

```go
type callIDGenerator struct {
    id uint64
}

func (g *callIDGenerator) next() uint64 {
    id := atomic.AddUint64(&g.id, 1)
    return id
}
```

전역 `idGen`이 RPC별 고유 callID를 생성하고, 각 `TruncatingMethodLogger` 인스턴스 내부의 `idWithinCallGen`이 해당 RPC 내 시퀀스 번호를 생성한다.

---

## 9. Sink 계층 구조

### 9.1 Sink 인터페이스

```go
// internal/binarylog/sink.go
type Sink interface {
    Write(*binlogpb.GrpcLogEntry) error
    Close() error
}
```

### 9.2 3가지 Sink 구현

```
Sink (interface)
    │
    ├─ noopSink
    │   └─ Write/Close: 아무것도 안 함 (기본값)
    │
    ├─ writerSink
    │   ├─ 4바이트 Big-Endian 길이 헤더 + proto 바이트
    │   └─ 버퍼링 없음, io.Writer 직접 쓰기
    │
    └─ bufferedSink
        ├─ bufio.Writer 기반 버퍼링
        ├─ 60초마다 자동 flush 고루틴
        └─ Close() 시 flush + 리소스 정리
```

### 9.3 bufferedSink의 주기적 flush

```go
const bufFlushDuration = 60 * time.Second

func (fs *bufferedSink) startFlushGoroutine() {
    fs.writeTicker = time.NewTicker(bufFlushDuration)
    go func() {
        for {
            select {
            case <-fs.done:
                return
            case <-fs.writeTicker.C:
            }
            fs.mu.Lock()
            fs.buf.Flush()
            fs.mu.Unlock()
        }
    }()
}
```

**왜 60초인가?** 프로덕션 환경에서는 수천 개의 RPC가 동시에 발생할 수 있다. 매 로그 엔트리마다 flush하면 I/O 부하가 크므로, 60초 간격으로 배치 flush하여 성능과 데이터 안전성 사이의 균형을 맞춘다.

### 9.4 와이어 포맷

```
┌──────────┬─────────────────────┐
│ 4 bytes  │ N bytes             │
│ (BE u32) │ (proto-encoded)     │
│ length   │ GrpcLogEntry        │
├──────────┼─────────────────────┤
│ 4 bytes  │ M bytes             │
│ length   │ GrpcLogEntry        │
├──────────┼─────────────────────┤
│   ...    │ ...                 │
└──────────┴─────────────────────┘
```

```go
func (ws *writerSink) Write(e *binlogpb.GrpcLogEntry) error {
    b, err := proto.Marshal(e)
    hdr := make([]byte, 4)
    binary.BigEndian.PutUint32(hdr, uint32(len(b)))
    ws.out.Write(hdr)
    ws.out.Write(b)
    return nil
}
```

---

## 10. 메시지 트런케이션 메커니즘

### 10.1 헤더 트런케이션

```go
func (ml *TruncatingMethodLogger) truncateMetadata(mdPb *binlogpb.Metadata) (truncated bool) {
    if ml.headerMaxLen == maxUInt {
        return false  // 무제한
    }
    var (
        bytesLimit = ml.headerMaxLen
        index      int
    )
    for ; index < len(mdPb.Entry); index++ {
        entry := mdPb.Entry[index]
        if entry.Key == "grpc-trace-bin" {
            continue  // 특수 키는 크기 계산에서 제외
        }
        currentEntryLen := uint64(len(entry.GetKey())) + uint64(len(entry.GetValue()))
        if currentEntryLen > bytesLimit {
            break
        }
        bytesLimit -= currentEntryLen
    }
    truncated = index < len(mdPb.Entry)
    mdPb.Entry = mdPb.Entry[:index]
    return truncated
}
```

**왜 `grpc-trace-bin`은 예외인가?** 이 키는 분산 트레이싱에 사용되는 바이너리 헤더로, 사용자에게 가시적이며 디버깅에 중요하다. 트런케이션하면 트레이싱 정보가 손실되므로 예외 처리한다.

### 10.2 메시지 트런케이션

```go
func (ml *TruncatingMethodLogger) truncateMessage(msgPb *binlogpb.Message) (truncated bool) {
    if ml.messageMaxLen == maxUInt {
        return false
    }
    if ml.messageMaxLen >= uint64(len(msgPb.Data)) {
        return false
    }
    msgPb.Data = msgPb.Data[:ml.messageMaxLen]
    return true
}
```

### 10.3 maxUInt 센티넬 값

```go
const maxUInt = ^uint64(0)  // 18446744073709551615
```

`maxUInt`는 "무제한"을 나타내는 센티넬 값이다. 필터에서 크기 제한을 지정하지 않으면 이 값이 사용되어 사실상 트런케이션이 비활성화된다.

---

## 11. 실제 활용 패턴

### 11.1 grpcurl과 리플렉션

```bash
# 서비스 목록 조회
grpcurl -plaintext localhost:50051 list

# 서비스 메서드 조회
grpcurl -plaintext localhost:50051 describe helloworld.Greeter

# RPC 호출 (리플렉션으로 proto 스키마 자동 발견)
grpcurl -plaintext -d '{"name":"world"}' \
    localhost:50051 helloworld.Greeter/SayHello
```

### 11.2 바이너리 로깅 활용

```bash
# 모든 RPC를 로깅
export GRPC_BINARY_LOG_FILTER="*"

# 특정 서비스만 로깅
export GRPC_BINARY_LOG_FILTER="helloworld.Greeter/*"

# 메시지는 1024바이트까지만
export GRPC_BINARY_LOG_FILTER="*{h;m:1024}"

# 프로그래밍 방식 Sink 설정
sink, _ := binarylog.NewTempFileSink()
binarylog.SetSink(sink)
```

### 11.3 메타데이터 필터링

```go
func metadataKeyOmit(key string) bool {
    switch key {
    case "lb-token", ":path", ":authority",
         "content-encoding", "content-type", "user-agent", "te":
        return true
    case "grpc-trace-bin":
        return false  // 예외: 가시적이므로 로깅
    }
    return strings.HasPrefix(key, "grpc-")  // grpc- 접두사는 제외
}
```

**왜 특정 키를 제외하는가?** HTTP/2 의사 헤더(`:path`, `:authority`)와 gRPC 내부 헤더(`grpc-status`, `grpc-message` 등)는 이미 다른 필드에서 기록되므로 중복을 방지한다. `grpc-trace-bin`만 예외적으로 포함되는데, 이는 사용자 영역에서 의미 있는 유일한 `grpc-` 접두사 헤더이기 때문이다.

---

## 12. 설계 철학과 Why

### 12.1 리플렉션의 설계 원칙

1. **제로 의존성**: proto 파일을 별도로 배포하지 않아도 런타임에 스키마 발견 가능
2. **증분 전송**: `sentFileDescriptors`로 이미 전송한 파일은 건너뜀
3. **하위 호환**: v1alpha와 v1 동시 등록으로 모든 클라이언트 지원
4. **확장성**: 커스텀 Resolver 주입으로 디스크립터 소스 교체 가능

### 12.2 바이너리 로깅의 설계 원칙

1. **관심사 분리**: Logger → MethodLogger → Sink 3단계 파이프라인
2. **환경변수 설정**: 코드 변경 없이 배포 시점에 로깅 범위 조정
3. **성능 우선**: bufferedSink으로 I/O 오버헤드 최소화
4. **크기 제어**: 트런케이션으로 로그 크기 폭증 방지
5. **gRPC A16 준수**: [proposal A16](https://github.com/grpc/proposal/blob/master/A16-binary-logging.md) 스펙 구현

### 12.3 리플렉션과 바이너리 로깅의 시너지

```
┌──────────────────────────────────────────────┐
│            디버깅 워크플로우                    │
│                                               │
│  1. grpcurl list (리플렉션)                    │
│     → 서버에 어떤 서비스가 있는지 확인          │
│                                               │
│  2. grpcurl describe (리플렉션)                │
│     → 메서드 시그니처, 메시지 타입 확인         │
│                                               │
│  3. GRPC_BINARY_LOG_FILTER 설정               │
│     → 문제가 의심되는 메서드만 로깅 활성화      │
│                                               │
│  4. 바이너리 로그 분석                          │
│     → 실제 전송된 헤더/메시지 내용 확인         │
│     → 에러 상태/트레일러 분석                   │
└──────────────────────────────────────────────┘
```

### 12.4 주요 코드 경로 요약

| 기능 | 진입점 | 핵심 파일 |
|------|--------|----------|
| 리플렉션 등록 | `reflection.Register(s)` | `reflection/serverreflection.go` |
| 리플렉션 요청 처리 | `ServerReflectionInfo()` | `reflection/internal/internal.go` |
| 바이너리 로깅 초기화 | `init()` | `internal/binarylog/binarylog.go` |
| 필터 파싱 | `NewLoggerFromConfigString()` | `internal/binarylog/env_config.go` |
| 이벤트 기록 | `TruncatingMethodLogger.Log()` | `internal/binarylog/method_logger.go` |
| 로그 출력 | `Sink.Write()` | `internal/binarylog/sink.go` |

---

*본 문서 위치: `grpc-go_EDU/21-reflection-binarylog.md`*
*소스코드 기준: `grpc-go/reflection/`, `grpc-go/binarylog/`, `grpc-go/internal/binarylog/`*
