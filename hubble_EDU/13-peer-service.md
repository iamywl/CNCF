# 13. 피어 서비스 (Peer Service)

## 개요

Hubble Peer 서비스는 클러스터 내 Hubble 피어(노드)의 가입, 탈퇴, 업데이트를 실시간으로
클라이언트에게 알려주는 gRPC 스트리밍 서비스이다. Hubble Relay가 각 노드의 Hubble
인스턴스를 발견하고 연결을 유지하는 데 핵심적인 역할을 한다.

이 문서에서는 Peer 서비스의 프로토콜 정의(proto), 서버 구현(Service), 이벤트 핸들러(handler),
알림 버퍼(buffer), 서비스 옵션(serviceoption), TLS 서버 이름 구성 등을 소스코드 수준에서
분석한다.

## 아키텍처 개요

```
+------------------+       Subscribe        +------------------+
|                  | ----------------------> |                  |
|   Node Manager   |                        |     handler      |
|  (Cilium 내부)   | -- NodeAdd/Update/  -> |  (NodeHandler    |
|                  |    Delete 콜백         |   구현체)         |
+------------------+                        +--------+---------+
                                                     |
                                            unbuffered chan
                                            ChangeNotification
                                                     |
                                                     v
                                            +--------+---------+
                                            |                  |
                                            |     buffer       |
                                            |  (thread-safe    |
                                            |   FIFO 큐)       |
                                            +--------+---------+
                                                     |
                                                Pop() 호출
                                                     |
                                                     v
                                            +--------+---------+
                                            |  gRPC Stream     |
                                            |  stream.Send()   |
                                            +--------+---------+
                                                     |
                                                     v
                                            +------------------+
                                            |   Relay Client   |
                                            |  (Hubble Relay)  |
                                            +------------------+
```

## Protobuf 정의

### peer.proto

피어 서비스의 프로토콜 버퍼 정의는 `api/v1/peer/peer.proto`에 위치한다.

```protobuf
// 소스: cilium/api/v1/peer/peer.proto

service Peer {
    // Notify는 클러스터의 hubble 피어 정보를 전송한다.
    // 호출 시 이미 클러스터에 속한 모든 피어를 PEER_ADDED 타입으로 전송하고,
    // 이후 변경사항을 실시간으로 알려준다.
    rpc Notify(NotifyRequest) returns (stream ChangeNotification) {}
}

message NotifyRequest {}

message ChangeNotification {
    // 피어 이름 (보통 호스트명). 클러스터 이름이 default가 아니면 앞에 붙는다.
    // 예: "runtime1", "testcluster/runtime1"
    string name = 1;
    // 피어의 gRPC 서비스 주소
    string address = 2;
    // 변경 유형: PEER_ADDED / PEER_DELETED / PEER_UPDATED
    ChangeNotificationType type = 3;
    // TLS 연결 정보 (없으면 TLS 비활성화)
    TLS tls = 4;
}

enum ChangeNotificationType {
    UNKNOWN = 0;
    PEER_ADDED = 1;
    PEER_DELETED = 2;
    PEER_UPDATED = 3;
}

message TLS {
    // 인증서 검증에 사용하는 서버 이름
    string server_name = 1;
}
```

### 핵심 설계 결정

| 항목 | 설계 | 이유 |
|------|------|------|
| Server Streaming | `returns (stream ChangeNotification)` | 클라이언트가 한 번 호출 후 지속적으로 알림 수신 |
| 빈 요청 | `NotifyRequest {}` | 필터링 없이 모든 피어 알림 수신 |
| TLS 분리 | 별도 `TLS` 메시지 | TLS 비활성화 시 null로 전달 가능 |
| 이름 형식 | `cluster/nodename` | 멀티클러스터 환경에서 고유 식별 |

## Service 구조체

### 정의

```go
// 소스: cilium/pkg/hubble/peer/service.go

// Service는 peerpb.PeerServer gRPC 서비스를 구현한다.
type Service struct {
    stop     chan struct{}
    notifier manager.Notifier
    opts     serviceoption.Options
}
```

Service는 세 가지 필드를 갖는다:
- `stop`: 서비스 종료 시그널 채널
- `notifier`: Cilium Node Manager의 Notifier 인터페이스 (Subscribe/Unsubscribe)
- `opts`: 서비스 설정 옵션

### 생성

```go
// 소스: cilium/pkg/hubble/peer/service.go

func NewService(notifier manager.Notifier, options ...serviceoption.Option) *Service {
    opts := serviceoption.Default
    for _, opt := range options {
        opt(&opts)
    }
    return &Service{
        stop:     make(chan struct{}),
        notifier: notifier,
        opts:     opts,
    }
}
```

함수형 옵션 패턴(Functional Options Pattern)을 사용하여 설정을 주입한다.
`serviceoption.Default`에서 시작하여 전달된 옵션들을 순차적으로 적용한다.

### Notify RPC 구현

Notify 메서드는 Peer 서비스의 핵심이다. errgroup을 사용하여 세 개의 고루틴을
병렬로 실행한다.

```go
// 소스: cilium/pkg/hubble/peer/service.go

func (s *Service) Notify(_ *peerpb.NotifyRequest, stream peerpb.Peer_NotifyServer) error {
    ctx, cancel := context.WithCancel(context.Background())
    g, ctx := errgroup.WithContext(ctx)

    // 고루틴 1: 글로벌 stop 시그널 모니터링
    h := newHandler(s.opts.WithoutTLSInfo, s.opts.AddressFamilyPreference, s.opts.HubblePort)
    g.Go(func() error {
        defer h.Close()
        select {
        case <-s.stop:
            cancel()
            return nil
        case <-ctx.Done():
            return nil
        }
    })

    // 고루틴 2: handler 채널에서 읽어 buffer에 Push
    buf := newBuffer(s.opts.MaxSendBufferSize)
    g.Go(func() error {
        defer buf.Close()
        for {
            select {
            case cn, ok := <-h.C:
                if !ok {
                    return nil
                }
                if err := buf.Push(cn); err != nil {
                    return ErrStreamSendBlocked
                }
            case <-ctx.Done():
                return nil
            }
        }
    })

    // 고루틴 3: buffer에서 Pop하여 클라이언트에 Send
    g.Go(func() error {
        for {
            cn, err := buf.Pop()
            if err != nil {
                if errors.Is(err, io.EOF) {
                    return nil
                }
                return err
            }
            if err := stream.Send(cn); err != nil {
                return err
            }
        }
    })

    s.notifier.Subscribe(h)
    defer s.notifier.Unsubscribe(h)
    return g.Wait()
}
```

### 세 고루틴의 역할

```
+-------------------+     +-------------------+     +-------------------+
|   고루틴 1         |     |   고루틴 2         |     |   고루틴 3         |
|   (Stop Monitor)  |     |   (Reader)        |     |   (Sender)        |
|                   |     |                   |     |                   |
| - s.stop 감시     |     | - h.C에서 읽기     |     | - buf.Pop() 호출  |
| - ctx 취소 감시   |     | - buf.Push() 호출  |     | - stream.Send()   |
| - h.Close() 정리  |     | - buf.Close() 정리 |     |   호출             |
+-------------------+     +-------------------+     +-------------------+
        |                         |                         |
        v                         v                         v
    errgroup.Wait() -- 어느 하나라도 종료하면 전체 종료
```

### 왜 이 구조인가?

1. **Handler의 채널이 unbuffered**: Node Manager가 Subscribe 콜백으로 알림을 보낼 때
   데드락을 방지하려면 클라이언트가 즉시 읽어야 한다.
2. **Buffer가 필요한 이유**: gRPC stream.Send()가 느린 클라이언트 때문에 블로킹될 수 있다.
   buffer가 이를 흡수한다.
3. **errgroup 사용**: 하나의 고루틴이 에러를 반환하면 context가 취소되어 나머지도 정리된다.

### Slow Client 보호

버퍼가 가득 차면 `ErrStreamSendBlocked` 에러가 반환되어 연결이 종료된다.

```go
var ErrStreamSendBlocked = errors.New(
    "server stream send was blocked for too long")
```

이는 느린 클라이언트가 서버 리소스를 고갈시키는 것을 방지하는 back-pressure 메커니즘이다.

## Handler (이벤트 핸들러)

### 구조체 정의

```go
// 소스: cilium/pkg/hubble/peer/handler.go

type handler struct {
    stop        chan struct{}
    C           chan *peerpb.ChangeNotification  // unbuffered 채널
    tls         bool
    addressPref serviceoption.AddressFamilyPreference
    hubblePort  int
}
```

handler는 `datapath.NodeHandler` 인터페이스를 구현하여 Cilium의 Node Manager로부터
노드 변경 알림을 수신한다.

### NodeHandler 인터페이스 구현

#### NodeAdd

```go
// 소스: cilium/pkg/hubble/peer/handler.go

func (h *handler) NodeAdd(n types.Node) error {
    cn := h.newChangeNotification(n, peerpb.ChangeNotificationType_PEER_ADDED)
    select {
    case h.C <- cn:
    case <-h.stop:
    }
    return nil
}
```

새 노드가 클러스터에 가입하면 `PEER_ADDED` 알림을 생성하여 채널에 전송한다.

#### NodeUpdate

```go
// 소스: cilium/pkg/hubble/peer/handler.go

func (h *handler) NodeUpdate(o, n types.Node) error {
    oAddr, nAddr := nodeAddress(o, h.addressPref), nodeAddress(n, h.addressPref)
    if o.Fullname() == n.Fullname() {
        if oAddr.String() == nAddr.String() {
            // 같은 피어, 같은 주소 => 알림 불필요
            return nil
        }
        // 같은 이름이지만 주소 변경 => PEER_UPDATED
        cn := h.newChangeNotification(n, peerpb.ChangeNotificationType_PEER_UPDATED)
        select {
        case h.C <- cn:
        case <-h.stop:
        }
        return nil
    }
    // 이름이 변경됨 => 이전 PEER_DELETED + 새 PEER_ADDED
    ocn := h.newChangeNotification(o, peerpb.ChangeNotificationType_PEER_DELETED)
    select {
    case h.C <- ocn:
    case <-h.stop:
        return nil
    }
    ncn := h.newChangeNotification(n, peerpb.ChangeNotificationType_PEER_ADDED)
    select {
    case h.C <- ncn:
    case <-h.stop:
    }
    return nil
}
```

NodeUpdate의 분기 로직:

```
NodeUpdate(old, new) 호출
    |
    +-- 이름 같음?
    |       |
    |       +-- 주소도 같음? -> 무시 (알림 없음)
    |       |
    |       +-- 주소 다름? -> PEER_UPDATED 전송
    |
    +-- 이름 다름?
            |
            +-- PEER_DELETED(old) 전송
            +-- PEER_ADDED(new) 전송
```

#### NodeDelete

```go
// 소스: cilium/pkg/hubble/peer/handler.go

func (h *handler) NodeDelete(n types.Node) error {
    cn := h.newChangeNotification(n, peerpb.ChangeNotificationType_PEER_DELETED)
    select {
    case h.C <- cn:
    case <-h.stop:
    }
    return nil
}
```

### ChangeNotification 생성

```go
// 소스: cilium/pkg/hubble/peer/handler.go

func (h *handler) newChangeNotification(
    n types.Node,
    t peerpb.ChangeNotificationType,
) *peerpb.ChangeNotification {
    var tls *peerpb.TLS
    if h.tls {
        tls = &peerpb.TLS{
            ServerName: TLSServerName(n.Name, n.Cluster),
        }
    }

    addr := ""
    if ip := nodeAddress(n, h.addressPref); ip != nil {
        addr = ip.String()
        if h.hubblePort != 0 {
            addr = net.JoinHostPort(addr, strconv.Itoa(h.hubblePort))
        }
    }

    return &peerpb.ChangeNotification{
        Name:    n.Fullname(),
        Address: addr,
        Type:    t,
        Tls:     tls,
    }
}
```

| 필드 | 소스 | 설명 |
|------|------|------|
| Name | `n.Fullname()` | `cluster/nodename` 형식 |
| Address | `nodeAddress()` + port | IP:Port 형식 (예: `10.0.0.1:4244`) |
| Type | 파라미터 `t` | ADDED/DELETED/UPDATED |
| Tls | TLS 활성화 시 | ServerName 포함 |

### 주소 패밀리 선호

```go
// 소스: cilium/pkg/hubble/peer/handler.go

func nodeAddress(n types.Node, pref serviceoption.AddressFamilyPreference) net.IP {
    for _, family := range pref {
        switch family {
        case serviceoption.AddressFamilyIPv4:
            if addr := n.GetNodeIP(false); addr.To4() != nil {
                return addr
            }
        case serviceoption.AddressFamilyIPv6:
            if addr := n.GetNodeIP(true); addr.To4() == nil {
                return addr
            }
        }
    }
    return nil
}
```

듀얼스택 환경에서 IPv4와 IPv6 중 어느 주소를 우선할지 설정 가능하다:

```go
// 소스: cilium/pkg/hubble/peer/serviceoption/option.go

var (
    AddressPreferIPv4 = AddressFamilyPreference{AddressFamilyIPv4, AddressFamilyIPv6}
    AddressPreferIPv6 = AddressFamilyPreference{AddressFamilyIPv6, AddressFamilyIPv4}
)
```

## TLS 서버 이름 구성

```go
// 소스: cilium/pkg/hubble/peer/handler.go

func TLSServerName(nodeName, clusterName string) string {
    if nodeName == "" {
        return ""
    }
    nn := strings.ReplaceAll(nodeName, ".", "-")
    if clusterName == "" {
        clusterName = ciliumDefaults.ClusterName
    }
    cn := strings.ReplaceAll(clusterName, ".", "-")
    return strings.Join([]string{
        nn,
        cn,
        defaults.GRPCServiceName,
        defaults.DomainName,
    }, ".")
}
```

### TLS 서버 이름 형식

```
<nodeName>.<clusterName>.<hubble-grpc-svc-name>.<domain>
```

예시:

| nodeName | clusterName | 결과 |
|----------|-------------|------|
| moseisley | tatooine | `moseisley.tatooine.hubble-grpc.cilium.io` |
| node-1.zone-a | default | `node-1-zone-a.default.hubble-grpc.cilium.io` |
| worker-2 | prod.cluster | `worker-2.prod-cluster.hubble-grpc.cilium.io` |

점(`.`)은 하이픈(`-`)으로 치환된다. 이는 DNS 도메인 레벨이 일정하게 유지되도록 하기 위함이다.
Kubernetes는 노드 이름에 점을 허용하므로 이 정규화가 필요하다.

## Buffer (알림 버퍼)

### 구조체

```go
// 소스: cilium/pkg/hubble/peer/buffer.go

type buffer struct {
    max    int
    buf    []*peerpb.ChangeNotification
    mu     lock.Mutex
    notify chan struct{}
    stop   chan struct{}
}
```

buffer는 동시 접근에 안전한(thread-safe) FIFO 큐이다.

### 생성

```go
// 소스: cilium/pkg/hubble/peer/buffer.go

func newBuffer(max int) *buffer {
    return &buffer{
        max:    max,
        notify: nil,
        stop:   make(chan struct{}),
    }
}
```

초기 용량 0에서 시작하여 `max`까지 동적으로 증가한다.

### Push 동작

```go
// 소스: cilium/pkg/hubble/peer/buffer.go

func (b *buffer) Push(cn *peerpb.ChangeNotification) error {
    b.mu.Lock()
    defer b.mu.Unlock()
    select {
    case <-b.stop:
        return errors.New("buffer closed")
    default:
        if len(b.buf) == b.max {
            return fmt.Errorf("max buffer size=%d reached", b.max)
        }
    }
    b.buf = append(b.buf, cn)
    if b.notify != nil {
        close(b.notify)
        b.notify = nil
    }
    return nil
}
```

Push의 동작 흐름:

```
Push(cn) 호출
    |
    +-- buffer 닫힘? -> 에러 반환
    |
    +-- buffer 가득 참? -> 에러 반환 (slow client 보호)
    |
    +-- buf에 append
    |
    +-- Pop이 대기 중이면 notify 채널 close로 깨움
```

### Pop 동작

```go
// 소스: cilium/pkg/hubble/peer/buffer.go

func (b *buffer) Pop() (*peerpb.ChangeNotification, error) {
    b.mu.Lock()
    if len(b.buf) == 0 {
        if b.notify == nil {
            b.notify = make(chan struct{})
        }
        notify := b.notify
        b.mu.Unlock()
        select {
        case <-notify:     // Push가 데이터를 넣고 깨워줌
            b.mu.Lock()
        case <-b.stop:     // buffer 닫힘
            return nil, io.EOF
        }
    }
    select {
    case <-b.stop:
        b.mu.Unlock()
        return nil, io.EOF
    default:
    }
    cn := b.buf[0]
    b.buf[0] = nil        // GC를 위해 참조 해제
    b.buf = b.buf[1:]
    b.mu.Unlock()
    return cn, nil
}
```

Pop의 동작 흐름:

```
Pop() 호출
    |
    +-- buffer 비어있음?
    |       |
    |       +-- notify 채널 생성
    |       +-- mutex 해제
    |       +-- select로 대기:
    |           - notify 시그널 (Push가 깨움) -> 다시 lock 획득
    |           - stop 시그널 -> io.EOF 반환
    |
    +-- buffer에 데이터 있음
            |
            +-- buf[0] 반환
            +-- buf[0] = nil (GC 힌트)
            +-- buf = buf[1:] (슬라이스 앞부분 제거)
```

### 알림 메커니즘 (Condition Variable 대안)

전통적인 condition variable 대신 Go 채널을 사용한 알림 메커니즘:

```
Producer (Push)                    Consumer (Pop)
    |                                  |
    +-- buf에 추가                      +-- buf 비어있으면 대기
    |                                  |
    +-- notify != nil ?                +-- notify 채널 생성
    |     |                            |
    |     +-- YES: close(notify)       +-- <-notify 로 대기
    |     |        notify = nil        |
    |     +-- NO: 아무것도 안 함       +-- 깨어남, lock 재획득
```

이 패턴의 장점:
- `sync.Cond`보다 Go 관용적
- select문과 자연스럽게 조합 가능 (stop 채널과 함께)
- close(notify)는 모든 대기자를 동시에 깨움

## 서비스 옵션 (Service Options)

### Options 구조체

```go
// 소스: cilium/pkg/hubble/peer/serviceoption/option.go

type Options struct {
    MaxSendBufferSize       int
    WithoutTLSInfo          bool
    AddressFamilyPreference AddressFamilyPreference
    HubblePort              int
}
```

### 기본값

```go
// 소스: cilium/pkg/hubble/peer/serviceoption/defaults.go

var Default = Options{
    MaxSendBufferSize:       65_536,
    AddressFamilyPreference: AddressPreferIPv4,
}
```

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| MaxSendBufferSize | 65,536 | 최대 버퍼 크기 (ChangeNotification 개수) |
| WithoutTLSInfo | false | TLS 정보 포함 여부 |
| AddressFamilyPreference | IPv4 우선 | 듀얼스택 시 주소 선호 |
| HubblePort | 0 | 알림 주소에 포함할 포트 |

### 옵션 함수들

```go
// 소스: cilium/pkg/hubble/peer/serviceoption/option.go

// 버퍼 크기 설정 - 초기 Subscribe 시 모든 노드의 burst를 수용할 수 있어야 함
func WithMaxSendBufferSize(size int) Option {
    return func(o *Options) {
        o.MaxSendBufferSize = size
    }
}

// TLS 정보 없이 알림 전송 (TLS 비활성화 시)
func WithoutTLSInfo() Option {
    return func(o *Options) {
        o.WithoutTLSInfo = true
    }
}

// IPv4/IPv6 주소 선호 설정
func WithAddressFamilyPreference(pref AddressFamilyPreference) Option {
    return func(o *Options) {
        o.AddressFamilyPreference = pref
    }
}

// Hubble 포트 설정
func WithHubblePort(port int) Option {
    return func(o *Options) {
        o.HubblePort = port
    }
}
```

## gRPC 서비스 등록

### 서버 측 인터페이스

```go
// 소스: cilium/api/v1/peer/peer_grpc.pb.go

type PeerServer interface {
    Notify(*NotifyRequest, grpc.ServerStreamingServer[ChangeNotification]) error
}

func RegisterPeerServer(s grpc.ServiceRegistrar, srv PeerServer) {
    if t, ok := srv.(interface{ testEmbeddedByValue() }); ok {
        t.testEmbeddedByValue()
    }
    s.RegisterService(&Peer_ServiceDesc, srv)
}
```

### 클라이언트 측 인터페이스

```go
// 소스: cilium/api/v1/peer/peer_grpc.pb.go

type PeerClient interface {
    Notify(ctx context.Context, in *NotifyRequest, opts ...grpc.CallOption) (
        grpc.ServerStreamingClient[ChangeNotification], error)
}
```

### 서비스 디스크립터

```go
// 소스: cilium/api/v1/peer/peer_grpc.pb.go

var Peer_ServiceDesc = grpc.ServiceDesc{
    ServiceName: "peer.Peer",
    HandlerType: (*PeerServer)(nil),
    Methods:     []grpc.MethodDesc{},
    Streams: []grpc.StreamDesc{
        {
            StreamName:    "Notify",
            Handler:       _Peer_Notify_Handler,
            ServerStreams:  true,
        },
    },
    Metadata: "peer/peer.proto",
}
```

Notify는 서버 스트리밍 RPC이므로 `Methods`가 아닌 `Streams`에 등록된다.

## Node Manager 연동

### Subscribe/Unsubscribe 흐름

```
Notify RPC 호출
    |
    +-- handler 생성
    +-- buffer 생성
    +-- 3개 고루틴 시작 (errgroup)
    +-- notifier.Subscribe(handler)   <-- Node Manager에 등록
    |
    |   [노드 변경 이벤트 수신 중...]
    |   NodeAdd/NodeUpdate/NodeDelete -> handler.C -> buffer -> stream.Send
    |
    +-- 종료 시 (에러 또는 클라이언트 disconnect)
    +-- notifier.Unsubscribe(handler) <-- Node Manager에서 해제
    +-- g.Wait() 반환
```

### 인터페이스 보장

```go
// 소스: cilium/pkg/hubble/peer/handler.go

// handler가 NodeHandler 인터페이스를 구현하는지 컴파일 타임에 확인
var _ datapath.NodeHandler = (*handler)(nil)

// Service가 PeerServer 인터페이스를 구현하는지 컴파일 타임에 확인
var _ peerpb.PeerServer = (*Service)(nil)
```

## 에러 처리와 생명주기

### 종료 시나리오

| 시나리오 | 트리거 | 결과 |
|----------|--------|------|
| 서비스 종료 | `Service.Close()` -> `close(s.stop)` | 고루틴 1이 cancel() 호출, 전체 종료 |
| 클라이언트 disconnect | `stream.Send()` 에러 | 고루틴 3이 에러 반환, errgroup 종료 |
| Slow client | `buf.Push()` 에러 (buffer full) | `ErrStreamSendBlocked` 반환, 연결 종료 |
| Handler 닫힘 | `h.Close()` -> `close(h.stop)` | NodeAdd 등에서 send 대신 stop으로 빠짐 |

### Service.Close()

```go
// 소스: cilium/pkg/hubble/peer/service.go

func (s *Service) Close() error {
    close(s.stop)
    return nil
}
```

close(s.stop)은 모든 활성 Notify 스트림의 고루틴 1에서 감지되어 전체 정리를 트리거한다.

## 전체 데이터 흐름 시퀀스

```
Client              Service              handler           buffer        Node Manager
  |                    |                    |                 |               |
  |-- Notify() ------->|                    |                 |               |
  |                    |-- newHandler() --->|                 |               |
  |                    |-- newBuffer() -----|---------------->|               |
  |                    |                    |                 |               |
  |                    |-- Subscribe(h) ----|-----------------|-------------->|
  |                    |                    |                 |               |
  |                    |                    |<-- NodeAdd(n) --|---------------|
  |                    |                    |-- PEER_ADDED -->|               |
  |                    |                    |                 |-- Push(cn) -->|
  |                    |                    |                 |               |
  |<-- Send(cn) -------|<--- Pop(cn) -------|-----------------|               |
  |                    |                    |                 |               |
  |                    |                    |<-- NodeUpdate --|---------------|
  |                    |                    |-- PEER_UPDATED->|               |
  |                    |                    |                 |-- Push(cn) -->|
  |                    |                    |                 |               |
  |<-- Send(cn) -------|<--- Pop(cn) -------|-----------------|               |
  |                    |                    |                 |               |
  |  [disconnect]      |                    |                 |               |
  |                    |-- Unsubscribe(h) --|-----------------|-------------->|
  |                    |-- return err ----->|                 |               |
```

## 성능 고려사항

### 버퍼 크기 결정

기본 `MaxSendBufferSize`는 65,536이다. 이 값은 초기 Subscribe 시 클러스터의 모든
노드가 `PEER_ADDED`로 전송되는 burst를 수용할 수 있도록 충분히 커야 한다.

```
초기 burst 크기 = 클러스터 노드 수
예: 5,000 노드 클러스터 -> 5,000개의 PEER_ADDED 알림이 한 번에 발생
```

### Unbuffered 채널의 의미

handler의 `C` 채널이 unbuffered인 이유:
1. Node Manager는 동기적으로 Subscribe 콜백을 호출한다
2. handler가 Subscribe 되기 전에 buffer goroutine이 이미 `h.C`에서 읽기 시작해야 한다
3. 따라서 Subscribe 호출을 errgroup.Go 이후로 배치한 것이다

```go
// 순서가 중요!
g.Go(/* reader goroutine */)  // h.C에서 읽기 시작
s.notifier.Subscribe(h)        // 그 후에 Subscribe
```

## Relay와의 상호작용

Hubble Relay는 Peer 서비스의 주요 클라이언트이다:

```
Relay 시작
    |
    +-- 각 알려진 노드에 Peer.Notify() 호출
    |
    +-- PEER_ADDED 수신 -> 해당 노드의 Observer에 연결
    +-- PEER_UPDATED 수신 -> 연결 주소 업데이트
    +-- PEER_DELETED 수신 -> 해당 노드 연결 종료
    |
    +-- 새 PEER_ADDED가 오면 -> 새 Observer 연결 수립
```

이를 통해 Relay는 클러스터 토폴로지 변경에 자동으로 적응한다.

## 정리

Hubble Peer 서비스는 다음과 같은 설계 원칙을 따른다:

1. **스트리밍 기반 디스커버리**: 초기 상태 동기화 + 이후 증분 업데이트
2. **Back-pressure**: 느린 클라이언트를 버퍼 오버플로우로 감지하고 연결 종료
3. **깔끔한 생명주기 관리**: errgroup으로 고루틴을 묶어 하나의 실패가 전체 정리를 트리거
4. **TLS 정보 전파**: 각 노드의 TLS 서버 이름을 알림에 포함하여 mTLS 연결 지원
5. **듀얼스택 지원**: IPv4/IPv6 주소 선호 설정으로 유연한 네트워크 구성 지원

### 파일 참조

| 파일 | 경로 |
|------|------|
| Proto 정의 | `cilium/api/v1/peer/peer.proto` |
| gRPC 생성 코드 | `cilium/api/v1/peer/peer_grpc.pb.go` |
| Service 구현 | `cilium/pkg/hubble/peer/service.go` |
| Handler | `cilium/pkg/hubble/peer/handler.go` |
| Buffer | `cilium/pkg/hubble/peer/buffer.go` |
| 서비스 옵션 | `cilium/pkg/hubble/peer/serviceoption/option.go` |
| 기본값 | `cilium/pkg/hubble/peer/serviceoption/defaults.go` |
