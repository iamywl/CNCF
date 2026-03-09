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

*소스 참조: `core/streaming/streaming.go`, `plugins/streaming/manager.go`, `core/introspection/introspection.go`*
*containerd 버전: v2.0*
