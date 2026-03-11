# containerd Streaming Service & Introspection Service Deep-Dive

## Part A: Streaming Service

### 1. 개요

containerd의 Streaming Service는 gRPC 양방향 스트리밍을 통해 **바이너리 데이터를 실시간으로 전송**하는 서브시스템이다.
컨테이너의 stdin/stdout/stderr, 로그, 파일 전송 등에 활용되며, containerd v2에서 Transfer Service와 함께 도입되었다.

#### 1.1 왜 별도 Streaming Service가 필요한가

| 방식 | 장점 | 단점 |
|------|------|------|
| Content Store | 영구 저장, 해시 검증 | 스트리밍 불가, 저장 공간 필요 |
| gRPC 단방향 | 간단한 구현 | 양방향 통신 불가 |
| **Streaming Service** | **양방향, 실시간, 네임스페이스 격리** | 영구 저장 아님 |

#### 1.2 소스 위치

```
containerd/
├── core/streaming/
│   ├── streaming.go         # Stream, StreamManager 인터페이스 (47줄)
│   └── proxy/               # gRPC 프록시 클라이언트
└── plugins/streaming/
    └── manager.go           # streamManager 구현 (269줄)
```

### 2. 핵심 인터페이스

`core/streaming/streaming.go`에서 정의:

```go
type Stream interface {
    Send(typeurl.Any) error    // 객체 전송
    Recv() (typeurl.Any, error) // 객체 수신
    Close() error               // 스트림 닫기
}

type StreamManager interface {
    StreamGetter
    Register(context.Context, string, Stream) error  // 스트림 등록
}

type StreamGetter interface {
    Get(context.Context, string) (Stream, error)     // 스트림 조회
}

type StreamCreator interface {
    Create(context.Context, string) (Stream, error)  // 스트림 생성
}
```

### 3. StreamManager 구현

`plugins/streaming/manager.go`의 `streamManager`:

```go
type streamManager struct {
    // 네임스페이스 → 이름 → 스트림
    streams map[string]map[string]*managedStream
    // 네임스페이스 → 리스 → 스트림 이름 집합
    byLease map[string]map[string]map[string]struct{}
    rwlock  sync.RWMutex
}
```

#### 3.1 데이터 구조

```
streams:                          byLease:
┌───────────┐                     ┌───────────┐
│ "default"  │                    │ "default"  │
│  ├─ "s1" → managedStream      │  ├─ "lease1"│
│  ├─ "s2" → managedStream      │  │   ├─ "s1"│
│  └─ "s3" → managedStream      │  │   └─ "s2"│
│ "k8s.io"  │                    │  └─ "lease2"│
│  └─ "s4" → managedStream      │       └─ "s3"│
└───────────┘                     └───────────┘
```

#### 3.2 Register: 스트림 등록

```go
func (sm *streamManager) Register(ctx context.Context, name string, stream streaming.Stream) error {
    ns, _ := namespaces.Namespace(ctx)  // 네임스페이스 추출
    ls, _ := leases.FromContext(ctx)     // 리스 추출 (있으면)

    ms := &managedStream{
        Stream: stream, ns: ns, name: name, lease: ls, manager: sm,
    }

    sm.rwlock.Lock()
    defer sm.rwlock.Unlock()

    // 네임스페이스별 맵에 등록
    nsMap := sm.streams[ns]
    if nsMap[name] exists → ErrAlreadyExists

    // 리스가 있으면 리스 맵에도 등록
    if ls != "" → sm.byLease[ns][ls][name] = struct{}{}
}
```

#### 3.3 GC 통합

StreamManager는 `metadata.CollectibleResource`를 구현하여 GC와 통합된다:

```go
md.(*metadata.DB).RegisterCollectibleResource(metadata.ResourceStream, sm)
```

GC 컬렉션 컨텍스트 인터페이스:

| 메서드 | 역할 |
|--------|------|
| `All(fn)` | 모든 스트림을 GC 노드로 열거 |
| `Active(ns, fn)` | 리스 없는 활성 스트림 열거 |
| `Leased(ns, lease, fn)` | 특정 리스에 연결된 스트림 열거 |
| `Remove(n)` | 삭제 대상으로 표시 |
| `Finish()` | 삭제 대상 스트림 닫기 |

#### 3.4 managedStream의 Close

```go
func (m *managedStream) Close() error {
    m.manager.rwlock.Lock()
    // streams 맵에서 제거
    delete(nsMap, m.name)
    // byLease 맵에서도 제거
    if m.lease != "" { delete(lsMap, m.name) }
    m.manager.rwlock.Unlock()
    return m.Stream.Close()  // 실제 스트림 닫기
}
```

### 4. 설계 결정

#### 4.1 왜 네임스페이스별로 격리하는가?

멀티테넌트 환경에서 한 네임스페이스의 스트림이 다른 네임스페이스에 노출되면 안 된다.
`context`에서 네임스페이스를 추출하여 자동으로 격리한다.

#### 4.2 왜 리스와 연동하는가?

스트림은 일시적이지만, 연관된 리소스(이미지 전송 중인 blob 등)는 GC로부터 보호되어야 한다.
리스에 스트림을 연결하면 리스가 유효한 동안 관련 리소스가 보호된다.

#### 4.3 왜 RWMutex를 사용하는가?

`Get()`은 읽기 전용이므로 `RLock`으로 동시 접근을 허용한다.
`Register()`, `Close()`는 쓰기이므로 `Lock`으로 배타적 접근을 보장한다.

---

## Part B: Introspection Service

### 5. 개요

Introspection Service는 containerd 서버의 **내부 상태를 조회**하는 API를 제공한다.
플러그인 목록, 서버 정보, 플러그인 상세 정보 등을 gRPC로 노출한다.

#### 5.1 소스 위치

```
containerd/
├── core/introspection/
│   ├── introspection.go    # Service 인터페이스 (31줄)
│   └── proxy/              # gRPC 프록시 클라이언트
└── api/services/introspection/v1/
    └── introspection.proto  # gRPC API 정의
```

### 6. Service 인터페이스

`core/introspection/introspection.go`:

```go
type Service interface {
    // Plugins는 필터 조건에 맞는 플러그인 목록을 반환한다
    Plugins(context.Context, ...string) (*api.PluginsResponse, error)

    // Server는 서버 정보 (버전, UUID 등)를 반환한다
    Server(context.Context) (*api.ServerResponse, error)

    // PluginInfo는 특정 플러그인의 상세 정보를 반환한다
    PluginInfo(context.Context, string, string, any) (*api.PluginInfoResponse, error)
}
```

### 7. 활용 시나리오

```
┌──────────────────────────────────────────────┐
│              Introspection API                │
│                                              │
│  ctr/nerdctl ──→ Plugins() ──→ 플러그인 목록    │
│                                              │
│  모니터링 도구 ──→ Server() ──→ 서버 상태/버전   │
│                                              │
│  디버깅 도구 ──→ PluginInfo() ──→ 플러그인 상세  │
└──────────────────────────────────────────────┘
```

### 8. gRPC API

```protobuf
service Introspection {
    rpc Plugins(PluginsRequest) returns (PluginsResponse);
    rpc Server(google.protobuf.Empty) returns (ServerResponse);
    rpc PluginInfo(PluginInfoRequest) returns (PluginInfoResponse);
}

message PluginsResponse {
    repeated Plugin plugins = 1;
}

message ServerResponse {
    string uuid = 1;
    int64 pid = 2;
    repeated DeprecationWarning deprecations = 3;
}
```

### 9. 플러그인 필터링

`Plugins()` 메서드는 가변 인자 필터를 지원한다:

```
// 예시 필터 패턴
Plugins(ctx, "type==io.containerd.snapshotter.v1")  // 스냅샷터만
Plugins(ctx, "id==overlayfs")                        // ID 기준
```

### 10. 운영에서의 활용

| 시나리오 | 사용 API | 용도 |
|---------|---------|------|
| 헬스체크 | `Server()` | containerd 프로세스 상태 확인 |
| 설정 검증 | `Plugins()` | 필요한 플러그인 로드 확인 |
| 디버깅 | `PluginInfo()` | 플러그인 설정/상태 상세 조회 |
| 호환성 검사 | `Server()` | 버전 확인 |

## 11. 정리

| 구성요소 | 역할 | 소스 |
|---------|------|------|
| Stream | 양방향 바이너리 데이터 전송 | `core/streaming/streaming.go` |
| StreamManager | 네임스페이스별 스트림 관리 + GC 통합 | `plugins/streaming/manager.go` |
| managedStream | 자동 정리 기능이 있는 스트림 래퍼 | `plugins/streaming/manager.go` |
| Introspection Service | 서버/플러그인 상태 조회 API | `core/introspection/` |

---

---

## 12. 실제 소스 코드 심화 분석

### 12.1 플러그인 등록 메커니즘

```go
// 소스: plugins/streaming/manager.go (35-56행)
func init() {
    registry.Register(&plugin.Registration{
        Type: plugins.StreamingPlugin,
        ID:   "manager",
        Requires: []plugin.Type{
            plugins.MetadataPlugin,
        },
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            md, err := ic.GetSingle(plugins.MetadataPlugin)
            if err != nil {
                return nil, err
            }
            sm := &streamManager{
                streams: map[string]map[string]*managedStream{},
                byLease: map[string]map[string]map[string]struct{}{},
            }
            md.(*metadata.DB).RegisterCollectibleResource(metadata.ResourceStream, sm)
            return sm, nil
        },
    })
}
```

**왜 `init()` 함수로 등록하는가?**

containerd는 플러그인 기반 아키텍처를 사용한다. `init()` 함수는 Go 패키지 임포트 시 자동으로 실행되므로, 플러그인 패키지를 임포트하면 자동으로 레지스트리에 등록된다. `Requires` 필드로 MetadataPlugin에 대한 의존성을 선언하여, containerd가 올바른 초기화 순서를 보장한다.

### 12.2 GC 통합 상세: CollectionContext 인터페이스

```go
// 소스: plugins/streaming/manager.go (172-268행)
type collectionContext struct {
    manager *streamManager
    removed []gc.Node          // 삭제 예정 노드 목록
}
```

GC 통합은 containerd의 메타데이터 GC 사이클과 연동된다:

| 단계 | 메서드 | 역할 |
|------|--------|------|
| 1단계 | `StartCollection()` | rwlock.Lock() — GC 동안 변경 차단 |
| 2단계 | `All(fn)` | 모든 스트림을 GC 그래프에 등록 |
| 3단계 | `Active(ns, fn)` | 리스 없는 활성 스트림만 보고 |
| 4단계 | `Leased(ns, lease, fn)` | 특정 리스에 연결된 스트림 보고 |
| 5단계 | `Remove(n)` | 삭제 대상으로 표시 (아직 삭제 안 함) |
| 6단계 | `Finish()` | 실제 삭제 수행 + rwlock.Unlock() |
| 오류 | `Cancel()` | GC 취소 시 rwlock.Unlock()만 수행 |

### 12.3 Finish() 상세 — 안전한 삭제 패턴

```go
// 소스: plugins/streaming/manager.go (230-268행)
func (cc *collectionContext) Finish() error {
    var closeStreams []streaming.Stream
    for _, node := range cc.removed {
        // 1. streams 맵에서 제거
        if nsMap, ok := cc.manager.streams[node.Namespace]; ok {
            if ms, ok := nsMap[node.Key]; ok {
                delete(nsMap, node.Key)
                closeStreams = append(closeStreams, ms.Stream)
                lease = ms.lease
            }
            if len(nsMap) == 0 {
                delete(cc.manager.streams, node.Namespace)
            }
        }
        // 2. byLease 맵에서도 제거
        // ... (동일 패턴)
    }
    // 3. 먼저 잠금 해제
    cc.manager.rwlock.Unlock()

    // 4. 잠금 해제 후 스트림 닫기
    var errs []error
    for _, s := range closeStreams {
        if err := s.Close(); err != nil {
            errs = append(errs, err)
        }
    }
    return errors.Join(errs...)
}
```

**왜 잠금 해제 후 스트림을 닫는가?**

스트림의 `Close()` 호출은 네트워크 I/O를 포함할 수 있어 시간이 오래 걸릴 수 있다. 잠금을 유지한 채로 `Close()`를 호출하면 다른 goroutine이 스트림을 등록/조회할 수 없게 된다. 맵에서 먼저 제거하고 잠금을 해제한 뒤 I/O를 수행하는 것이 동시성 성능에 유리하다.

### 12.4 Active()의 리스 처리

```go
// 소스: plugins/streaming/manager.go (190-205행)
func (cc *collectionContext) Active(ns string, fn func(gc.Node)) {
    if nsMap, ok := cc.manager.streams[ns]; ok {
        for name, stream := range nsMap {
            // 리스가 있는 스트림은 Active로 보고하지 않음
            // TODO: expire non-active streams
            if stream.lease == "" {
                fn(gc.Node{
                    Type:      metadata.ResourceStream,
                    Namespace: ns,
                    Key:       name,
                })
            }
        }
    }
}
```

**왜 리스가 있는 스트림은 Active로 보고하지 않는가?**

리스가 있는 스트림의 생명주기는 리스가 결정한다. 리스가 만료되면 GC가 리스에 연결된 모든 리소스(스트림 포함)를 정리한다. Active로 보고하면 GC가 이 스트림을 "사용 중"으로 판단하여 리스 만료 시에도 정리하지 않게 된다.

---

## 13. Proxy StreamCreator 상세

### 13.1 클라이언트 타입 멀티플렉싱

```go
// 소스: core/streaming/proxy/streaming.go (37-60행)
func NewStreamCreator(client any) streaming.StreamCreator {
    switch c := client.(type) {
    case streamingapi.StreamingClient:      // gRPC 클라이언트
        return &streamCreator{client: convertClient{c}}
    case grpc.ClientConnInterface:           // gRPC 연결
        return &streamCreator{client: convertClient{streamingapi.NewStreamingClient(c)}}
    case streamingapi.TTRPCStreamingClient:  // ttrpc 클라이언트
        return &streamCreator{client: c}
    case *ttrpc.Client:                      // ttrpc 연결
        return &streamCreator{client: streamingapi.NewTTRPCStreamingClient(c)}
    case streaming.StreamCreator:            // 이미 StreamCreator
        return c
    default:
        panic(fmt.Errorf("unsupported stream client %T: %w", client, errdefs.ErrNotImplemented))
    }
}
```

**왜 gRPC와 ttrpc를 모두 지원하는가?**

containerd는 두 가지 RPC 프로토콜을 지원한다:
- **gRPC**: 표준 Kubernetes 환경에서 사용 (HTTP/2 기반)
- **ttrpc**: 저오버헤드 환경에서 사용 (containerd 자체 프로토콜, 메모리 절약)

ttrpc는 gRPC 대비 메모리 사용량이 훨씬 적어 IoT나 임베디드 환경에 적합하다.

### 13.2 스트림 초기화 핸드셰이크

```go
// 소스: core/streaming/proxy/streaming.go (74-105행)
func (sc *streamCreator) Create(ctx context.Context, id string) (streaming.Stream, error) {
    stream, err := sc.client.Stream(ctx)
    // ...

    // 1. StreamInit 메시지 전송 (스트림 ID 포함)
    a, err := typeurl.MarshalAny(&streamingapi.StreamInit{ID: id})
    err = stream.Send(typeurl.MarshalProto(a))

    // 2. ACK 수신 대기 (서버가 스트림 준비 완료)
    if _, err = stream.Recv(); err != nil {
        return nil, err
    }

    return &clientStream{s: stream}, nil
}
```

**왜 ACK 핸드셰이크가 필요한가?**

서버 측에서 스트림을 Register()로 등록하는 데 시간이 걸릴 수 있다. ACK 없이 바로 데이터를 전송하면, 서버가 아직 준비되지 않은 상태에서 데이터를 받을 수 있다. 핸드셰이크로 양쪽이 준비된 것을 확인한 후 데이터를 교환한다.

### 13.3 에러 변환 패턴

```go
// 소스: core/streaming/proxy/streaming.go (111-125행)
func (cs *clientStream) Send(a typeurl.Any) (err error) {
    err = cs.s.Send(typeurl.MarshalProto(a))
    if !errors.Is(err, io.EOF) {
        err = errgrpc.ToNative(err)
    }
    return
}
```

gRPC 에러 코드를 containerd의 네이티브 에러 타입(`errdefs.ErrNotFound` 등)으로 변환한다. 단, `io.EOF`는 스트림 종료를 나타내는 정상적인 상태이므로 변환하지 않는다.

---

## 14. Introspection Proxy 상세

### 14.1 PluginInfo — 런타임 플러그인 정보

```go
// 소스: core/introspection/proxy/remote.go (81-96행)
func (i *introspectionRemote) PluginInfo(ctx context.Context,
    pluginType, id string, options any) (resp *api.PluginInfoResponse, err error) {

    var optionsPB *anypb.Any
    if options != nil {
        optionsPB, err = typeurl.MarshalAnyToProto(options)
        if err != nil {
            return nil, fmt.Errorf("failed to marshal runtime requst: %w", err)
        }
    }
    resp, err = i.client.PluginInfo(ctx, &api.PluginInfoRequest{
        Type:    pluginType,
        ID:      id,
        Options: optionsPB,
    })
    return resp, errgrpc.ToNative(err)
}
```

`options`는 `any` 타입으로 플러그인별 커스텀 옵션을 전달할 수 있다. `typeurl.MarshalAnyToProto`로 Protobuf Any 메시지로 직렬화하여 전송한다. 이 설계는 Introspection API가 모든 플러그인 타입에 대해 일반적인 인터페이스를 제공하면서도, 플러그인별 특화 옵션을 전달할 수 있게 한다.

### 14.2 convertIntrospection 어댑터

```go
// 소스: core/introspection/proxy/remote.go (98-110행)
type convertIntrospection struct {
    client api.IntrospectionClient
}

func (c convertIntrospection) Plugins(ctx context.Context,
    req *api.PluginsRequest) (*api.PluginsResponse, error) {
    return c.client.Plugins(ctx, req)
}
```

gRPC의 `IntrospectionClient`와 ttrpc의 `TTRPCIntrospectionService`는 인터페이스가 약간 다르다. `convertIntrospection` 어댑터가 gRPC 클라이언트를 ttrpc 인터페이스에 맞춰 통일된 코드 경로를 사용할 수 있게 한다.

---

## 15. 에러 처리 패턴 요약

```
┌─────────────────────────────────────────────────┐
│            에러 처리 패턴                          │
│                                                   │
│  StreamManager:                                   │
│  ├── Register: 이름 중복 → errdefs.ErrAlreadyExists│
│  └── Get: 존재하지 않음 → errdefs.ErrNotFound     │
│                                                   │
│  Proxy:                                           │
│  ├── gRPC 에러 → errgrpc.ToNative() 변환          │
│  ├── io.EOF → 변환 없이 그대로 전달               │
│  └── 마샬링 실패 → fmt.Errorf()로 래핑            │
│                                                   │
│  GC:                                              │
│  ├── Close 실패 → errors.Join()으로 다중 에러 결합 │
│  └── Cancel → 잠금 해제만 수행, 에러 무시          │
│                                                   │
│  Introspection:                                   │
│  ├── 지원하지 않는 클라이언트 → panic              │
│  └── 옵션 마샬링 실패 → fmt.Errorf()              │
│                                                   │
│  공통 패턴:                                       │
│  ├── containerd errdefs 패키지의 표준 에러 사용    │
│  ├── gRPC/ttrpc 에러를 네이티브로 변환             │
│  └── 빈 맵 정리 (delete 후 len==0이면 상위도 삭제) │
│                                                   │
│  빈 맵 정리의 이유:                                │
│  - 메모리 누수 방지 (빈 맵 객체가 남지 않도록)      │
│  - GC 순회 시 빈 네임스페이스를 건너뛸 수 있어 효율적│
└─────────────────────────────────────────────────┘
```

---

## 16. 성능 고려사항

| 항목 | 설계 | 이유 |
|------|------|------|
| RWMutex | Get()은 RLock, Register()/Close()는 Lock | 읽기 동시성 허용 |
| 빈 맵 삭제 | delete 후 len==0 검사 | 메모리 누수 방지 |
| GC 중 잠금 | StartCollection에서 Lock, Finish/Cancel에서 Unlock | GC 일관성 보장 |
| Close 후 잠금 해제 | Unlock 후 I/O 수행 | 잠금 보유 시간 최소화 |
| 핸드셰이크 | Init → ACK → Data | 양쪽 준비 상태 확인 |

---

*소스 참조: `core/streaming/streaming.go`, `plugins/streaming/manager.go`, `core/introspection/introspection.go`*
*containerd 버전: v2.0*
