# 05. gRPC-Go 핵심 컴포넌트

## 개요

gRPC-Go의 핵심 컴포넌트는 크게 **서버**, **클라이언트**, **트랜스포트**, **스트림** 4가지로
나뉜다. 이 문서에서는 각 컴포넌트의 내부 동작 원리를 소스코드 기반으로 설명한다.

---

## 1. Server — RPC 요청 수신 및 처리

### Server 구조체 (`server.go:126`)

```go
type Server struct {
    opts         serverOptions
    statsHandler stats.Handler

    mu       sync.Mutex
    lis      map[net.Listener]bool
    conns    map[string]map[transport.ServerTransport]bool
    serve    bool
    drain    bool
    cv       *sync.Cond
    services map[string]*serviceInfo
    events   traceEventLog

    quit               *grpcsync.Event
    done               *grpcsync.Event
    serveWG            sync.WaitGroup
    handlersWG         sync.WaitGroup
    channelz           *channelz.Server
    serverWorkerChannel chan func()
}
```

**왜 이런 구조인가?**

- `lis` map: 하나의 Server가 **여러 리스너**를 동시에 서빙할 수 있다 (예: TCP + Unix 소켓)
- `conns` 이중 map: 리스너 주소별로 트랜스포트를 그룹핑하여, 특정 리스너만 종료할 때 해당 커넥션만 정리 가능
- `quit`/`done` Event: Stop()과 GracefulStop()의 2단계 종료를 구분
- `serverWorkerChannel`: 워커 풀 패턴으로 goroutine 생성 오버헤드를 줄임

### 서비스 등록 흐름

```
RegisterService(sd *ServiceDesc, ss any)
    │
    ├── sd.ServiceName으로 키 생성
    ├── sd.HandlerType과 ss의 타입 호환성 검증 (reflect 사용)
    ├── serviceInfo 생성
    │   ├── methods map에 MethodDesc 등록
    │   └── streams map에 StreamDesc 등록
    └── server.services[serviceName] = serviceInfo
```

```go
// server.go: RegisterService
func (s *Server) RegisterService(sd *ServiceDesc, ss any) {
    if ss != nil {
        ht := reflect.TypeOf(sd.HandlerType).Elem()
        st := reflect.TypeOf(ss)
        if !st.Implements(ht) {
            logger.Fatalf("grpc: Server.RegisterService found the handler "+
                "of type %v that does not satisfy %v", st, ht)
        }
    }
    s.register(sd, ss)
}
```

### Serve() 메인 루프 (`server.go`)

```go
func (s *Server) Serve(lis net.Listener) error {
    // 1. 리스너 등록
    s.lis[lis] = true

    // 2. Accept 루프
    for {
        rawConn, err := lis.Accept()
        if err != nil {
            // 재시도 가능한 에러: tempDelay로 백오프
            // 불가능한 에러: return
        }

        // 3. 각 연결을 별도 goroutine에서 처리
        s.serveWG.Add(1)
        go func() {
            s.handleRawConn(lis.Addr().String(), rawConn)
            s.serveWG.Done()
        }()
    }
}
```

**Accept 백오프 전략:**

```
첫 실패:   5ms 대기
두 번째:  10ms 대기
세 번째:  20ms 대기
...
최대:      1초 대기
성공 시:  타이머 리셋
```

### handleRawConn → 트랜스포트 생성

```
handleRawConn(lisAddr, rawConn)
    │
    ├── 1. 연결 타임아웃 설정 (기본 120초)
    ├── 2. TLS 핸드셰이크 (credentials.ServerHandshake)
    ├── 3. HTTP/2 트랜스포트 생성 (newHTTP2Transport)
    ├── 4. conns map에 트랜스포트 등록
    └── 5. serveStreams(ctx, st, rawConn) 호출
```

### 스트림 디스패치 (`server.go: handleStream`)

```go
func (s *Server) handleStream(t transport.ServerTransport, stream *transport.ServerStream, ...) {
    // 1. 메서드 이름 파싱: "/package.Service/Method"
    sm := stream.Method()
    service := sm[:pos]   // "package.Service"
    method := sm[pos+1:]  // "Method"

    // 2. 서비스 검색
    srv, knownService := s.services[service]

    // 3. Unary vs Streaming 디스패치
    if md, ok := srv.methods[method]; ok {
        s.processUnaryRPC(ctx, stream, srv, md, ...)
    } else if sd, ok := srv.streams[method]; ok {
        s.processStreamingRPC(ctx, stream, srv, sd, ...)
    } else {
        // Unknown method → Unimplemented 에러
    }
}
```

### processUnaryRPC 핵심 흐름

```
processUnaryRPC(ctx, stream, info, md, trInfo)
    │
    ├── 1. StatsHandler.HandleRPC(ctx, InHeader)
    ├── 2. recvAndDecompress(stream) → 요청 메시지 읽기
    ├── 3. 인터셉터 체인 실행 (있으면)
    │   └── chainedInterceptor(ctx, req, info, handler)
    ├── 4. MethodDesc.Handler 호출
    │   └── 사용자 핸들러 실행
    ├── 5. sendResponse(stream, reply) → 응답 직렬화 + 전송
    ├── 6. WriteStatus(stream, statusOK)
    └── 7. StatsHandler.HandleRPC(ctx, End)
```

### 서버 워커 풀 (`server.go`)

```go
// NumStreamWorkers 옵션 설정 시
func (s *Server) initServerWorkers() {
    s.serverWorkerChannel = make(chan func())
    for i := uint32(0); i < s.opts.numServerWorkers; i++ {
        go s.serverWorker()
    }
}

func (s *Server) serverWorker() {
    for f := range s.serverWorkerChannel {
        f() // 스트림 핸들러 실행
    }
}
```

**왜 워커 풀인가?**

대규모 서버에서 각 스트림마다 goroutine을 생성하면 GC 압박이 커진다.
워커 풀은 미리 생성된 goroutine에 작업을 분배하여 goroutine 생성/삭제 오버헤드를 없앤다.

---

## 2. ClientConn — 채널 추상화

### ClientConn 구조체 (`clientconn.go`)

```go
type ClientConn struct {
    ctx, cancel context.Context
    target      string
    parsedTarget resolver.Target
    authority    string
    dopts        dialOptions
    channelz     *channelz.Channel

    resolverBuilder resolver.Builder
    idlenessMgr     *idle.Manager
    metricsRecorderList *istats.MetricsRecorderList
    statsHandler    stats.Handler

    csMgr           *connectivityStateManager
    pickerWrapper   *pickerWrapper
    safeConfigSelector iresolver.SafeConfigSelector

    mu              sync.RWMutex
    resolverWrapper *ccResolverWrapper
    balancerWrapper *ccBalancerWrapper
    sc              *ServiceConfig
    conns           map[*addrConn]struct{}
    keepaliveParams keepalive.ClientParameters
    firstResolveEvent *grpcsync.Event
}
```

**왜 ClientConn이 "채널"인가?**

gRPC에서 "채널(Channel)"은 하나의 타겟 서비스에 대한 **논리적 연결**이다.
물리적 TCP 연결은 여러 개일 수 있고 (SubConn), 리졸버가 주소를 갱신하고,
밸런서가 요청을 분배한다. ClientConn은 이 모든 것을 캡슐화한 "채널" 추상화이다.

### 유휴 상태 관리 (Idle Management)

```
                     ┌───────────┐
        첫 RPC ──→   │   Idle    │  ←── 유휴 타임아웃
                     └─────┬─────┘
                           │ exitIdleMode()
                     ┌─────▼─────┐
                     │  Active   │
                     │ (resolver │
                     │  balancer │
                     │  running) │
                     └─────┬─────┘
                           │ 유휴 타임아웃
                     ┌─────▼─────┐
                     │   Idle    │  resolver/balancer 종료
                     │           │  모든 SubConn Close
                     └───────────┘
```

**왜 유휴 관리가 필요한가?**

클라이언트가 서버에 RPC를 보내지 않는 동안에도 DNS 조회, 밸런서 상태 갱신,
keepalive 핑 등이 계속 실행되면 리소스 낭비이다. 유휴 상태로 전환하면
모든 백그라운드 작업을 멈추고, 다음 RPC가 올 때 다시 활성화한다.

### exitIdleMode() 흐름

```go
func (cc *ClientConn) exitIdleMode() error {
    // 1. 이미 활성 상태면 skip
    // 2. connectivityStateManager 초기화
    // 3. ccBalancerWrapper 생성
    // 4. ccResolverWrapper 생성 → resolver.Build() 호출
    //    → DNS 조회 시작
    //    → 주소 결과 → balancer.UpdateClientConnState()
    //    → SubConn 생성 → Connect()
    // 5. pickerWrapper에 Picker 설정
}
```

### 연결 상태 머신 (ConnectivityState)

```
    ┌──────────────────────────────────────────────────┐
    │                                                  │
    │  ┌───────┐  Connect()  ┌────────────┐           │
    │  │ Idle  │────────────▶│ Connecting │           │
    │  └───┬───┘             └──────┬─────┘           │
    │      │                        │                  │
    │      │                   성공  │  실패            │
    │      │                   ┌────▼───┐ ┌──────────┐│
    │      │                   │ Ready  │ │Transient ││
    │      │                   │        │ │ Failure  ││
    │      │                   └────┬───┘ └─────┬────┘│
    │      │                        │       재연결│     │
    │      │                   연결끊김    ┌────▼───┐  │
    │      │                        └─────▶│Connect-│  │
    │      │                               │  ing   │  │
    │      │                               └────────┘  │
    │      │                                           │
    │      └───────── Close() ─────────────────────────┘
    │                           ┌──────────┐
    └──────────────────────────▶│ Shutdown │
                                └──────────┘
```

### RPC 호출 경로 (cc.Invoke)

```go
// call.go
func (cc *ClientConn) Invoke(ctx context.Context, method string,
    args, reply any, opts ...CallOption) error {
    // 인터셉터가 있으면 체인 실행
    // 없으면 직접 invoke
    return cc.unaryInt(ctx, method, args, reply, cc, invoke, opts...)
}
```

```
Invoke(ctx, "/pkg.Svc/Method", req, reply)
    │
    ├── 인터셉터 체인 실행
    │
    ├── newClientStream(ctx, desc, cc, method, opts...)
    │   ├── pickerWrapper.Pick() → Picker.Pick(info)
    │   │   └── SubConn 선택 (balancer 정책에 따라)
    │   ├── SubConn에서 transport 획득
    │   └── transport.NewStream() → HTTP/2 스트림 생성
    │
    ├── cs.SendMsg(req) → 직렬화 + 압축 + 프레임 전송
    │
    ├── cs.RecvMsg(reply) → 프레임 수신 + 압축해제 + 역직렬화
    │
    └── cs.finish() → 스트림 정리
```

---

## 3. Transport — HTTP/2 프레이밍

### ServerTransport 인터페이스 (`internal/transport/transport.go`)

```go
type ServerTransport interface {
    HandleStreams(ctx context.Context, handle func(*ServerStream))
    WriteStatus(s *ServerStream, st *status.Status) error
    Write(s *ServerStream, hdr []byte, data mem.BufferSlice, opts *Options) error
    Drain(debugData string)
    Close(err error)
    Peer() *peer.Peer
    IncrMsgSent()
    IncrMsgRecv()
}
```

### ClientTransport 인터페이스

```go
type ClientTransport interface {
    Close(err error)
    Write(s *ClientStream, hdr []byte, data mem.BufferSlice, opts *Options) error
    NewStream(ctx context.Context, callHdr *CallHdr) (*ClientStream, error)
    CloseStream(s *ClientStream, err error)
    GoAway() <-chan struct{}
    GetGoAwayReason() GoAwayReason
    IncrMsgSent()
    IncrMsgRecv()
}
```

### http2Server 핵심 구조

```
http2Server
├── conn (net.Conn)          ← TCP 연결
├── framer (*framer)         ← HTTP/2 프레임 읽기/쓰기
├── loopy (*loopyWriter)     ← 비동기 프레임 전송 goroutine
├── controlBuf              ← 제어 프레임 큐 (SETTINGS, PING, GOAWAY)
├── fc (*trInFlow)           ← 연결 레벨 수신 흐름 제어
├── activeStreams            ← map[streamID]*ServerStream
├── bdpEst (*bdpEstimator)   ← 대역폭 추정기
└── kp/kep                   ← Keepalive 파라미터
```

**왜 loopyWriter를 별도 goroutine으로 분리하는가?**

HTTP/2에서 HEADERS, DATA, SETTINGS, PING 등 다양한 프레임을 보내야 한다.
이들을 각각의 goroutine에서 직접 쓰면 쓰기 경합이 발생한다.
loopyWriter는 **단일 goroutine**에서 모든 쓰기를 처리하여 경합을 제거하고,
우선순위에 따라 제어 프레임(PING 등)을 데이터 프레임보다 먼저 전송할 수 있다.

### controlBuffer — 프레임 큐잉

```
┌────────────────────────────────────────────┐
│              controlBuffer                  │
├────────────────────────────────────────────┤
│  list (링크드 리스트)                        │
│  ┌───────┐   ┌───────┐   ┌───────┐        │
│  │SETTINGS│──▶│ PING  │──▶│ DATA  │──▶ nil │
│  └───────┘   └───────┘   └───────┘        │
├────────────────────────────────────────────┤
│  ch (chan struct{})  ← put() 시 signal      │
│  done (chan struct{}) ← 종료 신호            │
└────────────────────────────────────────────┘
        │
        ▼ loopyWriter.run()
   get() → 프레임 타입별 처리 → conn.Write()
```

### 흐름 제어 (Flow Control)

```
연결 레벨 흐름 제어:
┌────────────────────────────────────┐
│  trInFlow (수신 흐름 제어)           │
│  ├── limit:      65535 (초기값)     │
│  ├── unacked:    0                  │
│  ├── pendingUpdate: 0               │
│  │                                  │
│  │  DATA 수신 → unacked += len      │
│  │  unacked > limit/4               │
│  │    → WINDOW_UPDATE 전송           │
│  │    → unacked = 0                 │
└────────────────────────────────────┘

스트림 레벨 흐름 제어:
┌────────────────────────────────────┐
│  writeQuota (송신 쿼터)             │
│  ├── quota: 65535 (초기값)          │
│  │                                  │
│  │  DATA 전송 → quota -= len        │
│  │  WINDOW_UPDATE 수신 → quota += n │
│  │  quota == 0 → 전송 대기          │
└────────────────────────────────────┘
```

### BDP Estimator (대역폭 지연 곱 추정)

```go
// internal/transport/bdp_estimator.go
type bdpEstimator struct {
    sentAt   time.Time      // 핑 전송 시각
    bwMax    float64        // 최대 대역폭
    bdp      uint32         // 현재 BDP
    sampleCount uint64      // 샘플 수
    rtt      float64        // 라운드트립 시간
}
```

**왜 BDP 추정이 필요한가?**

HTTP/2의 초기 윈도우 크기(64KB)는 고대역폭 링크에서 너무 작다.
BDP 추정기는 PING-ACK 라운드트립으로 대역폭과 지연을 측정하고,
최적의 윈도우 크기를 동적으로 조정한다. 이로써 높은 BDP 네트워크에서도
파이프라인이 꽉 차지 않도록 한다.

---

## 4. Stream — 메시지 송수신

### ClientStream (`stream.go`)

```go
type clientStream struct {
    callHdr  *transport.CallHdr
    opts     []CallOption
    callInfo *callInfo
    cc       *ClientConn
    desc     *StreamDesc

    codec       encoding.Codec
    comp        encoding.Compressor
    decompSet   bool
    decomp      encoding.Compressor
    p           *parser

    mu     sync.Mutex
    sentLast bool
    closed   bool
    finished bool

    retryThrottler *retryThrottler
    binlogs        []binarylog.MethodLogger

    // transport stream
    t  transport.ClientTransport
    s  *transport.ClientStream
    done func(balancer.DoneInfo)
}
```

### 메시지 직렬화 + 전송 (`rpc_util.go`)

```
SendMsg(m)
    │
    ├── 1. codec.Marshal(m) → []byte
    │      (기본: proto.Marshal)
    │
    ├── 2. compress(data) → []byte
    │      (설정 시: gzip.Compress)
    │
    ├── 3. 메시지 프레이밍
    │      ┌─────────┬────────────────┐
    │      │ 5 bytes │   payload      │
    │      │ header  │                │
    │      ├─────────┤                │
    │      │ 1 byte  │   4 bytes      │
    │      │ compress│   length       │
    │      │ flag    │   (big-endian) │
    │      └─────────┴────────────────┘
    │
    └── 4. transport.Write(stream, hdr, data)
           → HTTP/2 DATA 프레임으로 전송
```

### 메시지 수신 + 역직렬화

```
RecvMsg(m)
    │
    ├── 1. recvMsg(stream) → 5바이트 헤더 읽기
    │      ├── compress flag 확인
    │      └── payload length 확인
    │
    ├── 2. payload 읽기 (length만큼)
    │
    ├── 3. decompress(data) (필요시)
    │
    ├── 4. codec.Unmarshal(data, m)
    │      (기본: proto.Unmarshal)
    │
    └── 5. 메시지 크기 검증
           (maxReceiveMessageSize 초과 시 에러)
```

### 4가지 RPC 패턴에서의 스트림 사용

```
1. Unary (1:1)
   Client ──SendMsg──▶ Server ──SendMsg──▶ Client
                                           RecvMsg

2. Server Streaming (1:N)
   Client ──SendMsg──▶ Server ──SendMsg──▶ Client
                               ──SendMsg──▶ RecvMsg
                               ──SendMsg──▶ RecvMsg
                               ──(EOF)────▶

3. Client Streaming (N:1)
   Client ──SendMsg──▶ Server
          ──SendMsg──▶
          ──SendMsg──▶
          ──CloseSend─▶        ──SendMsg──▶ Client

4. Bidirectional (N:M)
   Client ──SendMsg──▶ Server
          ──SendMsg──▶         ──SendMsg──▶ Client
          ──SendMsg──▶         ──SendMsg──▶
          ──CloseSend─▶        ──SendMsg──▶
                               ──(EOF)────▶
```

---

## 5. Picker Wrapper — 스레드 안전 로드 밸런싱

### pickerWrapper (`picker_wrapper.go`)

```go
type pickerWrapper struct {
    mu         sync.Mutex
    picker     balancer.Picker
    blockingCh chan struct{}
}
```

### Pick 동작

```
Pick(ctx, info)
    │
    ├── picker == nil? → blockingCh에서 대기
    │   (밸런서가 아직 Picker를 설정하지 않음)
    │
    ├── picker.Pick(info)
    │   ├── 성공 → PickResult(SubConn, Done) 반환
    │   ├── ErrNoSubConnAvailable → blockingCh에서 대기
    │   │   (Picker가 갱신될 때까지)
    │   └── 다른 에러 → RPC에 에러 반환
    │
    └── 컨텍스트 취소/데드라인 → 에러 반환
```

**왜 blocking 방식인가?**

클라이언트가 RPC를 보낼 때 아직 밸런서가 Ready 상태의 SubConn을 갖고 있지 않을 수 있다
(DNS 조회 중, 연결 수립 중 등). blocking으로 기다리면 호출자에게 투명하게 연결 완료를
기다릴 수 있다. WaitForReady 옵션과도 자연스럽게 통합된다.

---

## 6. Wrapper 패턴 — 내부 상태 관리

### ccResolverWrapper (`resolver_wrapper.go`)

```
ccResolverWrapper
├── cc       *ClientConn          ← 부모 채널
├── resolver resolver.Resolver    ← 실제 리졸버 인스턴스
├── incomingMu sync.Mutex         ← 업데이트 직렬화
└── serializer *grpcsync.CallbackSerializer  ← 콜백 직렬화

리졸버 이벤트:
  resolver → UpdateState() → serializer에 enqueue
          → serializer goroutine에서 순차 처리
          → balancerWrapper.UpdateClientConnState()
```

### ccBalancerWrapper (`balancer_wrapper.go`)

```
ccBalancerWrapper
├── cc       *ClientConn
├── balancer balancer.Balancer    ← 실제 밸런서 인스턴스
├── serializer *grpcsync.CallbackSerializer
└── curBalancerName string

밸런서 이벤트:
  balancer → UpdateState(State) → serializer에 enqueue
          → serializer goroutine에서 순차 처리
          → pickerWrapper에 새 Picker 설정
```

**왜 CallbackSerializer를 사용하는가?**

리졸버와 밸런서의 콜백은 서로 다른 goroutine에서 올 수 있다.
이를 직접 처리하면 복잡한 락킹이 필요하다. CallbackSerializer는
모든 콜백을 하나의 goroutine에서 순차 실행하여 동시성 문제를 근본적으로 제거한다.

---

## 7. Graceful Shutdown vs Hard Stop

### GracefulStop() 흐름

```
GracefulStop()
    │
    ├── 1. drain = true 설정
    ├── 2. 모든 리스너 Close (새 연결 거부)
    ├── 3. 모든 트랜스포트에 Drain() 호출
    │      → HTTP/2 GOAWAY 프레임 전송
    │      → 진행 중인 RPC는 완료까지 허용
    ├── 4. handlersWG.Wait()
    │      (모든 RPC 핸들러 완료 대기)
    ├── 5. cv.Wait()
    │      (모든 트랜스포트 종료 대기)
    └── 6. done.Fire()
```

### Stop() 흐름 (즉시 종료)

```
Stop()
    │
    ├── 1. 모든 리스너 Close
    ├── 2. 모든 트랜스포트 Close(err)
    │      → 강제 연결 종료
    │      → 진행 중인 RPC에 에러 반환
    └── 3. done.Fire()
```

---

## 8. 인터셉터 체이닝 메커니즘

### 서버 Unary 인터셉터 체인

```go
// server.go: chainUnaryServerInterceptors
func chainUnaryServerInterceptors(s *Server) {
    interceptors := s.opts.chainUnaryInts
    if s.opts.unaryInt != nil {
        interceptors = append([]UnaryServerInterceptor{s.opts.unaryInt}, interceptors...)
    }
    // 체인 생성
    var chainedInt UnaryServerInterceptor
    if len(interceptors) == 0 {
        chainedInt = nil
    } else if len(interceptors) == 1 {
        chainedInt = interceptors[0]
    } else {
        chainedInt = chainUnaryInterceptors(interceptors)
    }
    s.opts.unaryInt = chainedInt
}
```

### 체인 실행 구조

```
요청 ──▶ Interceptor1 ──▶ Interceptor2 ──▶ ... ──▶ 실제 핸들러
         (로깅)            (인증)                    (비즈니스 로직)
응답 ◀── Interceptor1 ◀── Interceptor2 ◀── ... ◀── 실제 핸들러

각 인터셉터는 handler(ctx, req)를 호출하여 다음 단계로 진행.
호출하지 않으면 체인이 중단됨 (인증 실패 등에서 사용).
```

---

## 9. addrConn — SubChannel 구현

### addrConn 구조체

```
addrConn
├── cc        *ClientConn      ← 부모 채널
├── addrs     []resolver.Address ← 대상 주소 목록
├── transport transport.ClientTransport ← 현재 활성 트랜스포트
├── state     connectivity.State ← 현재 연결 상태
├── backoff   backoff.Strategy  ← 재연결 백오프
└── resetBackoff chan struct{}   ← 백오프 리셋 신호
```

### 연결 루프

```
connect()
    │
    └── resetTransport() goroutine 시작
        │
        ├── 주소 목록 순회
        │   ├── createTransport(addr) 시도
        │   │   ├── 성공 → state = Ready
        │   │   └── 실패 → 다음 주소
        │   └── 모든 주소 실패
        │       → state = TransientFailure
        │       → 백오프 대기
        │       → 재시도
        │
        └── 트랜스포트 오류 감지 시
            → state = TransientFailure
            → 재연결 루프 재시작
```

---

## 10. 메시지 크기 제한

| 설정 | 기본값 | 서버 | 클라이언트 |
|------|--------|------|-----------|
| maxReceiveMessageSize | 4MB | `MaxRecvMsgSize()` | `WithDefaultCallOptions(MaxCallRecvMsgSize())` |
| maxSendMessageSize | MaxInt32 | `MaxSendMsgSize()` | `WithDefaultCallOptions(MaxCallSendMsgSize())` |

```go
// 서버: server.go
const (
    defaultServerMaxReceiveMessageSize = 1024 * 1024 * 4  // 4MB
    defaultServerMaxSendMessageSize    = math.MaxInt32
)

// 클라이언트: clientconn.go
const (
    defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4  // 4MB
    defaultClientMaxSendMessageSize    = math.MaxInt32
)
```

**왜 수신만 4MB 제한인가?**

수신 메시지는 메모리에 전체 로드해야 하므로 악의적으로 큰 메시지로 인한 OOM을 방지한다.
송신은 자신이 제어할 수 있으므로 사실상 무제한(MaxInt32)이다.

---

## 컴포넌트 상호작용 종합 다이어그램

```
┌─────────────────────────────────────────────────────────┐
│                     ClientConn                           │
│                                                          │
│  ┌──────────┐    ┌──────────┐    ┌──────────────┐       │
│  │ Resolver │───▶│ Balancer │───▶│ PickerWrapper│       │
│  │ Wrapper  │    │ Wrapper  │    │              │       │
│  └────┬─────┘    └────┬─────┘    └───────┬──────┘       │
│       │               │                  │              │
│  ┌────▼─────┐    ┌────▼─────┐    ┌───────▼──────┐      │
│  │  DNS     │    │pick_first│    │   Picker     │      │
│  │ Resolver │    │ Balancer │    │  (Pick RPC)  │      │
│  └──────────┘    └────┬─────┘    └──────────────┘      │
│                       │                                  │
│              ┌────────▼─────────┐                       │
│              │    addrConn      │ × N                   │
│              │   (SubChannel)   │                       │
│              └────────┬─────────┘                       │
│                       │                                  │
│              ┌────────▼─────────┐                       │
│              │  http2Client     │                       │
│              │ (ClientTransport)│                       │
│              └──────────────────┘                       │
│                                                          │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐              │
│  │Interceptor│  │  Codec   │  │Credentials│             │
│  │  Chain    │  │ (proto)  │  │  (TLS)    │             │
│  └──────────┘  └──────────┘  └──────────┘              │
└─────────────────────────────────────────────────────────┘
```
