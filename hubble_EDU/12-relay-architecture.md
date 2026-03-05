# 12. Hubble Relay 아키텍처 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Relay의 존재 이유](#2-relay의-존재-이유)
3. [전체 아키텍처](#3-전체-아키텍처)
4. [Relay Server 구조](#4-relay-server-구조)
5. [PeerManager 3-고루틴 모델](#5-peermanager-3-고루틴-모델)
6. [연결 풀 관리](#6-연결-풀-관리)
7. [GetFlows 다중 노드 집계](#7-getflows-다중-노드-집계)
8. [Priority Queue 정렬](#8-priority-queue-정렬)
9. [에러 집계 (Error Aggregation)](#9-에러-집계-error-aggregation)
10. [NodeStatusEvent 시스템](#10-nodestatusevent-시스템)
11. [Health Check 시스템](#11-health-check-시스템)
12. [Prometheus 메트릭](#12-prometheus-메트릭)
13. [TLS 보안 아키텍처](#13-tls-보안-아키텍처)
14. [Observer Server API 구현](#14-observer-server-api-구현)
15. [설정 기본값과 튜닝](#15-설정-기본값과-튜닝)
16. [동시성 모델 분석](#16-동시성-모델-분석)
17. [설계 결정 분석 (Why)](#17-설계-결정-분석-why)

---

## 1. 개요

Hubble Relay는 Kubernetes 클러스터의 **모든 노드에 분산된 Hubble 인스턴스**를 하나의 통합된
gRPC API로 제공하는 프록시 서버이다. 각 노드의 Cilium 에이전트 안에서 실행되는 Hubble 서버는
해당 노드의 네트워크 플로우만 관찰할 수 있지만, Relay는 이들을 **중앙에서 집계**하여
클러스터 전체의 네트워크 가시성을 제공한다.

```
소스 코드 위치:
  pkg/hubble/relay/server/server.go    -- Relay Server 메인
  pkg/hubble/relay/observer/server.go  -- Observer API 구현
  pkg/hubble/relay/observer/observer.go -- 플로우 수집/정렬/에러집계
  pkg/hubble/relay/pool/manager.go     -- PeerManager (3-고루틴)
  pkg/hubble/relay/pool/client.go      -- gRPC 클라이언트 연결 빌더
  pkg/hubble/relay/queue/priority_queue.go -- 시간순 정렬 큐
  pkg/hubble/relay/defaults/defaults.go    -- 기본 설정값
```

### Relay가 처리하는 핵심 문제

| 문제 | Relay의 해결 방법 |
|------|-------------------|
| 분산된 Hubble 인스턴스 | 모든 노드에 gRPC 연결 풀 유지 |
| 시간순 정렬 필요 | Priority Queue로 타임스탬프 기반 정렬 |
| 노드 추가/삭제 | Peer Service에서 변경 알림 수신 |
| 연결 실패 처리 | Exponential backoff로 자동 재연결 |
| 에러 폭주 방지 | 동일 에러 메시지 시간 윈도우 집계 |
| 클라이언트 단일 진입점 | `hubble-relay:4245`로 통합 접근 |

---

## 2. Relay의 존재 이유

### 왜 각 노드에 직접 연결하지 않는가?

Kubernetes 클러스터에서 Hubble이 직면하는 근본 문제를 이해해야 한다.

```
[문제 상황: Relay 없이]

  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐
  │ Node-1  │  │ Node-2  │  │ Node-3  │  │ Node-N  │
  │ Hubble  │  │ Hubble  │  │ Hubble  │  │ Hubble  │
  │ :4244   │  │ :4244   │  │ :4244   │  │ :4244   │
  └────┬────┘  └────┬────┘  └────┬────┘  └────┬────┘
       │            │            │            │
       └────────────┼────────────┼────────────┘
                    │
              ┌─────┴─────┐
              │ CLI/UI    │  ← N개 연결 관리?
              │ 클라이언트 │     시간순 정렬?
              └───────────┘     노드 추가/삭제?

  문제점:
  1. 클라이언트가 모든 노드 주소를 알아야 함
  2. N개 gRPC 스트림을 동시 관리해야 함
  3. 다중 노드 플로우를 시간순으로 병합해야 함
  4. 노드가 추가/삭제될 때 동적 대응 필요
  5. 각 클라이언트마다 이 로직을 구현해야 함
```

```
[해결: Relay 도입]

  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐
  │ Node-1  │  │ Node-2  │  │ Node-3  │  │ Node-N  │
  │ Hubble  │  │ Hubble  │  │ Hubble  │  │ Hubble  │
  │ :4244   │  │ :4244   │  │ :4244   │  │ :4244   │
  └────┬────┘  └────┬────┘  └────┬────┘  └────┬────┘
       │            │            │            │
       └────────────┼────────────┼────────────┘
                    │
            ┌───────┴───────┐
            │  Hubble Relay │  ← 연결 풀 관리
            │  :4245        │     시간순 정렬
            │               │     에러 집계
            └───────┬───────┘     노드 동적 관리
                    │
              ┌─────┴─────┐
              │ CLI/UI    │  ← 단일 연결!
              │ 클라이언트 │
              └───────────┘
```

### Relay의 핵심 가치

1. **추상화**: 클라이언트는 클러스터 토폴로지를 알 필요 없음
2. **정렬**: 분산 노드의 이벤트를 전역 시간순으로 병합
3. **탄력성**: 노드 변경에 자동 대응, 연결 실패 시 재시도
4. **단순화**: 클라이언트는 하나의 gRPC 연결만 유지

---

## 3. 전체 아키텍처

### Relay 내부 구성요소 관계도

```
┌──────────────────────────────────────────────────────────────┐
│                       Hubble Relay                           │
│                                                              │
│  ┌──────────────────────┐     ┌───────────────────────────┐ │
│  │  gRPC Server (:4245) │     │  Health Server (:4222)    │ │
│  │  ┌─────────────────┐ │     │  ┌──────────────────────┐ │ │
│  │  │ Observer Server │ │     │  │ healthServer         │ │ │
│  │  │ (GetFlows,      │ │     │  │  - probeInterval: 5s │ │ │
│  │  │  ServerStatus,  │ │     │  │  - PeerServiceConn?  │ │ │
│  │  │  GetNodes,      │ │     │  │  - AvailablePeers>0? │ │ │
│  │  │  GetNamespaces) │ │     │  └──────────────────────┘ │ │
│  │  └────────┬────────┘ │     └───────────────────────────┘ │
│  │           │          │                                    │
│  │  ┌────────┴────────┐ │     ┌───────────────────────────┐ │
│  │  │ TLS Credentials │ │     │  Metrics Server (:9966)   │ │
│  │  │ (TLS 1.3 min)   │ │     │  /metrics                 │ │
│  │  └─────────────────┘ │     └───────────────────────────┘ │
│  └──────────────────────┘                                    │
│                                                              │
│  ┌──────────────────────────────────────────────────────────┐│
│  │              PeerManager (3 고루틴)                       ││
│  │  ┌──────────────────┐ ┌──────────────┐ ┌──────────────┐ ││
│  │  │ watchNotifica-   │ │ manage-      │ │ reportConn-  │ ││
│  │  │ tions()          │ │ Connections()│ │ ectionStatus()│││
│  │  │                  │ │              │ │              │ ││
│  │  │ Peer Service     │ │ 주기적 연결  │ │ Prometheus   │ ││
│  │  │ Notify 스트림    │ │ 상태 확인    │ │ 메트릭 보고  │ ││
│  │  │ → upsert/remove  │ │ + 백오프     │ │ (5초 주기)   │ ││
│  │  └──────────────────┘ └──────────────┘ └──────────────┘ ││
│  │                                                          ││
│  │  peers map[string]*peer ← 연결 풀                        ││
│  └──────────────────────────────────────────────────────────┘│
└──────────────────────────────────────────────────────────────┘
```

### 데이터 흐름 파이프라인

```
[GetFlows 요청 처리 파이프라인]

  Client Request
       │
       ▼
  ┌─────────────┐
  │ GetFlows()  │ ← Observer Server
  └─────┬───────┘
        │
        ▼
  ┌─────────────┐     peers.List()      ┌──────────┐
  │ flowCollec- │ ──────────────────────→│ PeerMgr  │
  │ tor.collect │ ←─────────────────────│ .List()  │
  └─────┬───────┘     []Peer            └──────────┘
        │
        │ (각 peer에 대해 goroutine 시작)
        │
  ┌─────┴───────────────────────────────────────┐
  │  goroutine 1   goroutine 2   goroutine N    │
  │  retrieveFlows retrieveFlows retrieveFlows  │
  │  FromPeer()    FromPeer()    FromPeer()     │
  └─────┬──────────────┬──────────────┬─────────┘
        │              │              │
        └──────────────┼──────────────┘
                       │
                 flows chan ← 공유 채널
                       │
                       ▼
                ┌──────────────┐
                │ aggregate-   │ ← 동일 에러 병합
                │ Errors()     │    (10초 윈도우)
                └──────┬───────┘
                       │
                       ▼
                ┌──────────────┐
                │ sortFlows()  │ ← Priority Queue
                └──────┬───────┘    (타임스탬프 순)
                       │
                       ▼
                ┌──────────────┐
                │ sendFlows-   │ ← gRPC 스트림 전송
                │ Response()   │
                └──────────────┘
```

---

## 4. Relay Server 구조

### Server 구조체

`relay/server/server.go`의 `Server` 구조체가 Relay의 최상위 컨테이너이다.

```go
// pkg/hubble/relay/server/server.go

type Server struct {
    server           *grpc.Server      // 메인 gRPC 서버 (Observer API)
    grpcHealthServer *grpc.Server      // 헬스체크 전용 gRPC 서버
    pm               *pool.PeerManager // 피어 연결 풀 관리자
    healthServer     *healthServer     // 헬스체크 로직
    metricsServer    *http.Server      // Prometheus 메트릭 HTTP 서버
    opts             options           // 설정 옵션
}
```

### 서버 초기화 과정 (New)

```go
func New(options ...Option) (*Server, error) {
    opts := defaultOptions
    options = append(options, DefaultOptions...)
    for _, opt := range options {
        if err := opt(&opts); err != nil {
            return nil, fmt.Errorf("failed to apply option: %w", err)
        }
    }
    // TLS 설정 검증 (서버/클라이언트 모두)
    if opts.clientTLSConfig == nil && !opts.insecureClient {
        return nil, ErrNoClientTLSConfig
    }
    if opts.serverTLSConfig == nil && !opts.insecureServer {
        return nil, ErrNoServerTLSConfig
    }
    // ...
}
```

초기화 순서를 단계별로 보면:

```
[Server 초기화 단계]

  1. 옵션 적용
     │  defaultOptions + 사용자 옵션
     ▼
  2. TLS 검증
     │  클라이언트 TLS / 서버 TLS 필수 확인
     ▼
  3. PeerClientBuilder 결정
     │  unix:// → LocalClientBuilder
     │  그 외   → RemoteClientBuilder (TLS 포함)
     ▼
  4. PeerManager 생성
     │  pool.NewPeerManager(registry, options...)
     ▼
  5. gRPC 서버 생성
     │  Interceptor 체인 + TLS Credentials
     ▼
  6. Observer Server 등록
     │  observer.NewServer(pm, observerOptions...)
     │  observerpb.RegisterObserverServer(grpcServer, observerSrv)
     ▼
  7. Health Server 등록
     │  healthpb.RegisterHealthServer(grpcServer, healthSrv.svc)
     │  healthpb.RegisterHealthServer(grpcHealthServer, healthSrv.svc)
     ▼
  8. Metrics 서버 생성 (선택)
     │  /metrics 엔드포인트
     ▼
  9. Server 구조체 반환
```

### Peer Client Builder 선택 로직

```go
// Relay → Peer Service 연결 방식 결정
var peerClientBuilder peerTypes.ClientBuilder = &peerTypes.LocalClientBuilder{}
if !strings.HasPrefix(opts.peerTarget, "unix://") {
    peerClientBuilder = &peerTypes.RemoteClientBuilder{
        TLSConfig:     opts.clientTLSConfig,
        TLSServerName: peer.TLSServerName(defaults.PeerServiceName, opts.clusterName),
    }
}
```

| 조건 | Builder | 설명 |
|------|---------|------|
| `unix://` prefix | `LocalClientBuilder` | 같은 노드의 Cilium Agent UDS 연결 |
| 그 외 | `RemoteClientBuilder` | 원격 Peer Service에 TLS 연결 |

### Serve 메서드 (errgroup 3-고루틴)

```go
func (s *Server) Serve() error {
    var eg errgroup.Group

    // 고루틴 1: Metrics HTTP 서버
    if s.metricsServer != nil {
        eg.Go(func() error {
            return s.metricsServer.ListenAndServe()
        })
    }

    // 고루틴 2: 메인 gRPC 서버 + PeerManager 시작
    eg.Go(func() error {
        s.pm.Start()          // PeerManager 3-고루틴 시작
        s.healthServer.start() // 헬스체크 시작
        socket, err := net.Listen("tcp", s.opts.listenAddress)
        if err != nil {
            return err
        }
        return s.server.Serve(socket)
    })

    // 고루틴 3: Health gRPC 서버
    eg.Go(func() error {
        socket, err := net.Listen("tcp", s.opts.healthListenAddress)
        if err != nil {
            return err
        }
        return s.grpcHealthServer.Serve(socket)
    })

    return eg.Wait()
}
```

`errgroup.Group`을 사용하므로 **하나의 고루틴이라도 에러를 반환하면** 전체가 종료된다.
이것이 `Serve()`가 "Serve will return a non-nil error if Stop() is not called"인 이유다.

### Stop 메서드 (우아한 종료)

```go
func (s *Server) Stop() {
    s.server.Stop()                                    // 1. gRPC 서버 정지
    if s.metricsServer != nil {
        s.metricsServer.Shutdown(context.Background())  // 2. 메트릭 서버 종료
    }
    s.pm.Stop()                                        // 3. PeerManager 종료
    s.healthServer.stop()                              // 4. 헬스체크 종료
}
```

종료 순서가 중요하다:
1. gRPC 서버를 먼저 멈춰서 새 요청을 차단
2. 메트릭 서버 종료
3. PeerManager 종료 (모든 3개 고루틴이 완료될 때까지 `wg.Wait()`)
4. 헬스체크 종료

---

## 5. PeerManager 3-고루틴 모델

PeerManager는 Relay의 핵심 컴포넌트로, 클러스터 내 모든 Hubble 피어를 관리한다.
**정확히 3개의 고루틴**이 서로 다른 역할을 수행한다.

### PeerManager 구조체

```go
// pkg/hubble/relay/pool/manager.go

type PeerManager struct {
    opts                 options          // 설정 (주소, 타임아웃 등)
    updated              chan string      // 연결 요청 채널 (버퍼: 100)
    wg                   sync.WaitGroup   // 고루틴 동기화
    stop                 chan struct{}     // 종료 신호
    peerServiceConnected atomic.Bool      // Peer Service 연결 상태
    mu                   lock.RWMutex     // peers 맵 보호
    peers                map[string]*peer // 피어 풀
    metrics              *PoolMetrics     // Prometheus 메트릭
}

type peer struct {
    mu              lock.Mutex       // 개별 피어 보호
    peerTypes.Peer                   // 이름, 주소, TLS 정보
    conn            poolTypes.ClientConn  // gRPC 연결
    connAttempts    int              // 연결 시도 횟수 (백오프용)
    nextConnAttempt time.Time        // 다음 연결 시도 시각
}
```

### Start: 3개 고루틴 시작

```go
func (m *PeerManager) Start() {
    m.wg.Add(3)
    go func() {
        defer m.wg.Done()
        m.watchNotifications()     // 고루틴 1: 피어 변경 감시
    }()
    go func() {
        defer m.wg.Done()
        m.manageConnections()      // 고루틴 2: 연결 관리
    }()
    go func() {
        defer m.wg.Done()
        m.reportConnectionStatus() // 고루틴 3: 메트릭 보고
    }()
}
```

### 고루틴 1: watchNotifications

Peer Service의 gRPC Notify 스트림을 구독하여 클러스터 토폴로지 변경을 감지한다.

```
[watchNotifications 상태 머신]

  ┌─────────┐
  │ START   │
  └────┬────┘
       │
       ▼
  ┌─────────────────┐
  │ 피어 클라이언트  │──── 실패 ──→ retryTimeout 대기 ──┐
  │ 생성 시도       │                                   │
  └────┬────────────┘ ←────────────────────────────────┘
       │ 성공
       ▼
  ┌─────────────────┐
  │ Notify 스트림   │──── 실패 ──→ cl.Close() + 대기 ──┐
  │ 생성            │                                   │
  └────┬────────────┘ ←────────────────────────────────┘
       │ 성공
       │ peerServiceConnected = true
       ▼
  ┌─────────────────┐
  │ client.Recv()   │──── 에러 ──→ peerServiceConnected = false
  │ 대기 (블로킹)   │              cl.Close() + retryTimeout 대기
  └────┬────────────┘              → 맨 위로 (continue connect)
       │ 정상 수신
       ▼
  ┌─────────────────┐
  │ 알림 타입 분기  │
  │ PEER_ADDED   ──→ upsert(p)
  │ PEER_UPDATED ──→ upsert(p)
  │ PEER_DELETED ──→ remove(p)
  └────┬────────────┘
       │
       └──→ Recv() 루프로 돌아감
```

실제 코드의 핵심 부분:

```go
func (m *PeerManager) watchNotifications() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go func() { <-m.stop; cancel() }()

connect:
    for {
        cl, err := m.opts.peerClientBuilder.Client(m.opts.peerServiceAddress)
        if err != nil {
            // retryTimeout(30초) 후 재시도
            select {
            case <-m.stop: return
            case <-time.After(m.opts.retryTimeout): continue
            }
        }
        client, err := cl.Notify(ctx, &peerpb.NotifyRequest{})
        if err != nil {
            cl.Close()
            select {
            case <-m.stop: return
            case <-time.After(m.opts.retryTimeout): continue
            }
        }
        m.peerServiceConnected.Store(true) // 원자적 상태 설정

        for {
            select {
            case <-m.stop: cl.Close(); return
            default:
            }
            cn, err := client.Recv()
            if err != nil {
                cl.Close()
                m.peerServiceConnected.Store(false)
                select {
                case <-m.stop: return
                case <-time.After(m.opts.retryTimeout): continue connect
                }
            }
            p := peerTypes.FromChangeNotification(cn)
            switch cn.GetType() {
            case peerpb.ChangeNotificationType_PEER_ADDED:   m.upsert(p)
            case peerpb.ChangeNotificationType_PEER_DELETED: m.remove(p)
            case peerpb.ChangeNotificationType_PEER_UPDATED: m.upsert(p)
            }
        }
    }
}
```

### 고루틴 2: manageConnections

연결 상태를 주기적으로 확인하고, 끊어진 연결을 복구한다.

```go
func (m *PeerManager) manageConnections() {
    for {
        select {
        case <-m.stop:
            return
        case name := <-m.updated:
            // 즉시 연결 (upsert에서 알림받음)
            m.mu.RLock()
            p := m.peers[name]
            m.mu.RUnlock()
            m.wg.Add(1)
            go func(p *peer) {
                defer m.wg.Done()
                m.connect(p, true) // ignoreBackoff = true
            }(p)
        case <-time.After(m.opts.connCheckInterval):
            // 주기적 점검 (기본: 2분)
            m.mu.RLock()
            for _, p := range m.peers {
                m.wg.Add(1)
                go func(p *peer) {
                    defer m.wg.Done()
                    m.connect(p, false) // ignoreBackoff = false
                }(p)
            }
            m.mu.RUnlock()
        }
    }
}
```

```
[manageConnections 이벤트 처리]

  ┌──────────────────┐
  │ manageConnections│
  │ (고루틴 2)       │
  └────┬─────────────┘
       │
       ├──→ updated 채널 수신 ──→ 즉시 connect(p, ignoreBackoff=true)
       │    (upsert에서 전송)      백오프 무시하고 바로 연결
       │
       └──→ 2분 타이머 ──→ 모든 피어 connect(p, ignoreBackoff=false)
            (connCheckInterval)   백오프 존중
```

### 고루틴 3: reportConnectionStatus

Prometheus 메트릭에 연결 상태를 주기적으로 보고한다.

```go
func (m *PeerManager) reportConnectionStatus() {
    for {
        select {
        case <-m.stop:
            return
        case <-time.After(m.opts.connStatusInterval): // 기본: 5초
            m.mu.RLock()
            connStates := make(map[connectivity.State]uint32)
            var nilConnPeersNum uint32 = 0
            for _, p := range m.peers {
                p.mu.Lock()
                if p.conn == nil {
                    nilConnPeersNum++
                    p.mu.Unlock()
                    continue
                }
                state := p.conn.GetState()
                connStates[state] = connStates[state] + 1
                p.mu.Unlock()
            }
            m.mu.RUnlock()
            m.metrics.ObservePeerConnectionStatus(connStates, nilConnPeersNum)
        }
    }
}
```

보고되는 연결 상태 종류:

| connectivity.State | 의미 |
|-------------------|------|
| `Idle` | 연결 미사용 대기 |
| `Connecting` | 연결 시도 중 |
| `Ready` | 연결 활성, RPC 가능 |
| `TransientFailure` | 일시적 연결 실패 |
| `Shutdown` | 연결 종료 |
| `NIL_CONNECTION` | 연결 객체 자체가 없음 |

---

## 6. 연결 풀 관리

### upsert (추가/갱신)

```go
func (m *PeerManager) upsert(hp *peerTypes.Peer) {
    if hp == nil { return }
    m.mu.Lock()
    p := m.peers[hp.Name]

    if p != nil && p.Peer.Equal(*hp) {
        m.mu.Unlock()
        return // 변경 없음 → 재연결 불필요
    }

    if p != nil {
        m.disconnect(p) // 기존 연결 종료
    }
    m.peers[hp.Name] = &peer{Peer: *hp}
    m.mu.Unlock()

    select {
    case <-m.stop:
    case m.updated <- hp.Name: // manageConnections에 알림
    }
}
```

```
[upsert 흐름]

  Peer 변경 알림 수신
       │
       ▼
  ┌─────────────┐    같음     ┌────────────┐
  │ 기존 피어   │ ──────────→ │ 무시 (리턴)│
  │ 존재 확인   │             └────────────┘
  └─────┬───────┘
        │ 변경됨 또는 새 피어
        ▼
  ┌─────────────┐
  │ 기존 연결   │ ──→ disconnect() 호출
  │ 종료 (있으면)│
  └─────┬───────┘
        │
        ▼
  ┌─────────────┐
  │ 새 peer 객체│
  │ 맵에 저장   │
  └─────┬───────┘
        │
        ▼
  ┌─────────────┐
  │ updated 채널│ ──→ manageConnections가 수신 → connect()
  │ 에 이름 전송│
  └─────────────┘
```

### connect (연결 수립)

```go
func (m *PeerManager) connect(p *peer, ignoreBackoff bool) {
    if p == nil { return }
    p.mu.Lock()
    defer p.mu.Unlock()

    // 이미 유효한 연결이 있으면 스킵
    if p.conn != nil && p.conn.GetState() != connectivity.Shutdown {
        return
    }

    now := time.Now()
    // 백오프 시간 확인 (ignoreBackoff가 아닌 경우)
    if p.Address == nil || (p.nextConnAttempt.After(now) && !ignoreBackoff) {
        return
    }

    conn, err := m.opts.clientConnBuilder.ClientConn(p.Address.String(), p.TLSServerName)
    if err != nil {
        duration := m.opts.backoff.Duration(p.connAttempts) // 지수 백오프
        p.nextConnAttempt = now.Add(duration)
        p.connAttempts++
        return
    }
    p.nextConnAttempt = time.Time{} // 백오프 리셋
    p.connAttempts = 0
    p.conn = conn
}
```

### Exponential Backoff

```go
// pkg/hubble/relay/pool/option.go
func defaultBackoff(logger *slog.Logger) *backoff.Exponential {
    return &backoff.Exponential{
        Min:    time.Second,   // 최소 1초
        Max:    time.Minute,   // 최대 1분
        Factor: 2.0,           // 2배씩 증가
    }
}
```

```
[백오프 증가 패턴]

  시도 1 실패 → 1초 대기
  시도 2 실패 → 2초 대기
  시도 3 실패 → 4초 대기
  시도 4 실패 → 8초 대기
  시도 5 실패 → 16초 대기
  시도 6 실패 → 32초 대기
  시도 7 실패 → 60초 대기 (Max 도달)
  시도 8 실패 → 60초 대기
  ...

  연결 성공 → connAttempts = 0, nextConnAttempt = zero time
```

### disconnect (연결 해제)

```go
func (m *PeerManager) disconnect(p *peer) {
    if p == nil { return }
    p.mu.Lock()
    defer p.mu.Unlock()
    if p.conn == nil { return }

    if err := p.conn.Close(); err != nil {
        // 로그만 남기고 계속 진행
    }
    p.conn = nil
}
```

### List (피어 목록 조회)

Observer Server가 `GetFlows`를 처리할 때 호출하는 메서드:

```go
func (m *PeerManager) List() []poolTypes.Peer {
    m.mu.RLock()
    defer m.mu.RUnlock()
    if len(m.peers) == 0 {
        return nil
    }
    peers := make([]poolTypes.Peer, 0, len(m.peers))
    for _, v := range m.peers {
        v.mu.Lock()
        peers = append(peers, poolTypes.Peer{
            Peer: peerTypes.Peer{
                Name:          v.Name,
                Address:       v.Address,
                TLSEnabled:    v.TLSEnabled,
                TLSServerName: v.TLSServerName,
            },
            Conn: v.conn, // nil일 수 있음 (연결 실패)
        })
        v.mu.Unlock()
    }
    return peers
}
```

**중요**: `List()`는 `Conn`이 nil인 피어도 포함하여 반환한다.
호출자(Observer Server)가 `isAvailable(p.Conn)`으로 직접 확인해야 한다.

---

## 7. GetFlows 다중 노드 집계

GetFlows는 Relay의 가장 복잡한 API로, 분산된 노드의 플로우를 수집, 정렬, 집계하여
클라이언트에게 스트리밍한다.

### 전체 처리 과정

```go
// pkg/hubble/relay/observer/server.go

func (s *Server) GetFlows(req *observerpb.GetFlowsRequest,
    stream observerpb.Observer_GetFlowsServer) error {

    ctx := stream.Context()
    // 1. 메타데이터 전달 (incoming → outgoing)
    md, ok := metadata.FromIncomingContext(ctx)
    if ok {
        ctx = metadata.NewOutgoingContext(ctx, md)
    }
    ctx, cancel := context.WithCancel(ctx)
    defer cancel()

    // 2. 피어 목록 조회 + 정렬 버퍼 크기 결정
    peers := s.peers.List()
    qlen := s.opts.sortBufferMaxLen // 기본 100
    if nqlen := req.GetNumber() * uint64(len(peers)); nqlen > 0 && nqlen < uint64(qlen) {
        qlen = int(nqlen) // 필요 이상으로 크지 않게
    }

    // 3. 플로우 수집 시작
    g, gctx := errgroup.WithContext(ctx)
    flows := make(chan *observerpb.GetFlowsResponse, qlen)
    fc := newFlowCollector(req, s.opts)
    connectedNodes, unavailableNodes := fc.collect(gctx, g, peers, flows)

    // 4. Follow 모드: 주기적으로 새 피어 확인
    if req.GetFollow() {
        go func() {
            for {
                select {
                case <-time.After(s.opts.peerUpdateInterval): // 2초
                    peers := s.peers.List()
                    _, _ = fc.collect(gctx, g, peers, flows)
                case <-gctx.Done():
                    return
                }
            }
        }()
    }

    // 5. 모든 수집 고루틴 완료 시 채널 닫기
    go func() {
        g.Wait()
        close(flows)
    }()

    // 6. 파이프라인: 에러 집계 → 시간순 정렬
    aggregated := aggregateErrors(ctx, flows, s.opts.errorAggregationWindow)
    sortedFlows := sortFlows(ctx, aggregated, qlen, s.opts.sortBufferDrainTimeout)

    // 7. 노드 상태 알림 전송
    if len(connectedNodes) > 0 {
        status := nodeStatusEvent(relaypb.NodeState_NODE_CONNECTED, connectedNodes...)
        stream.Send(status)
    }
    if len(unavailableNodes) > 0 {
        status := nodeStatusEvent(relaypb.NodeState_NODE_UNAVAILABLE, unavailableNodes...)
        stream.Send(status)
    }

    // 8. 정렬된 플로우 스트리밍
    err := sendFlowsResponse(ctx, stream, sortedFlows)
    if err != nil { return err }
    return g.Wait()
}
```

### flowCollector: 피어별 수집 고루틴

```go
// pkg/hubble/relay/observer/observer.go

type flowCollector struct {
    log *slog.Logger
    ocb observerClientBuilder
    req *observerpb.GetFlowsRequest
    mu             lock.Mutex
    connectedNodes map[string]struct{} // 이미 연결된 노드 추적
}
```

```go
func (fc *flowCollector) collect(ctx context.Context, g *errgroup.Group,
    peers []poolTypes.Peer, flows chan *observerpb.GetFlowsResponse) ([]string, []string) {

    var connected, unavailable []string
    fc.mu.Lock()
    defer fc.mu.Unlock()

    for _, p := range peers {
        // 이미 수집 중인 노드 → 스킵
        if _, ok := fc.connectedNodes[p.Name]; ok {
            connected = append(connected, p.Name)
            continue
        }
        // 연결 불가 → unavailable 목록
        if !isAvailable(p.Conn) {
            unavailable = append(unavailable, p.Name)
            continue
        }
        // 새 노드 → 고루틴 시작
        connected = append(connected, p.Name)
        fc.connectedNodes[p.Name] = struct{}{}
        g.Go(func() error {
            err := retrieveFlowsFromPeer(ctx, fc.ocb.observerClient(&p), fc.req, flows)
            if err != nil {
                fc.mu.Lock()
                delete(fc.connectedNodes, p.Name) // 실패 시 재시도 가능하도록 삭제
                fc.mu.Unlock()
                select {
                case flows <- nodeStatusError(err, p.Name):
                case <-ctx.Done():
                }
            }
            return nil
        })
    }
    return connected, unavailable
}
```

```
[flowCollector.collect 분기 로직]

  각 peer에 대해:
       │
       ├── connectedNodes에 존재? ──→ "이미 수집 중" → connected 추가, 스킵
       │
       ├── isAvailable(p.Conn) == false? ──→ unavailable 추가, 스킵
       │
       └── 새 노드 ──→ connectedNodes에 등록
                       errgroup.Go로 수집 고루틴 시작
                       retrieveFlowsFromPeer() 호출
                            │
                            ├── 성공: flows 채널에 계속 전송
                            │
                            └── 실패: connectedNodes에서 삭제
                                      nodeStatusError를 flows에 전송
```

### retrieveFlowsFromPeer: 단일 피어에서 플로우 수신

```go
func retrieveFlowsFromPeer(
    ctx context.Context,
    client observerpb.ObserverClient,
    req *observerpb.GetFlowsRequest,
    flows chan<- *observerpb.GetFlowsResponse,
) error {
    c, err := client.GetFlows(ctx, req) // gRPC 스트림 열기
    if err != nil {
        return err
    }
    for {
        flow, err := c.Recv() // 블로킹 수신
        if err != nil {
            if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
                return nil // 정상 종료
            }
            if status.Code(err) == codes.Canceled {
                return nil
            }
            return err // 에러 반환
        }
        select {
        case flows <- flow:     // 공유 채널에 전송
        case <-ctx.Done():
            return nil
        }
    }
}
```

### Follow 모드의 동적 피어 갱신

Follow 모드(`--follow` 플래그)에서는 스트림이 무기한 지속되므로, **새로 추가된 노드**를
주기적으로 감지해야 한다.

```go
if req.GetFollow() {
    go func() {
        for {
            select {
            case <-time.After(s.opts.peerUpdateInterval): // 2초마다
                peers := s.peers.List()
                _, _ = fc.collect(gctx, g, peers, flows) // 새 피어만 추가
            case <-gctx.Done():
                return
            }
        }
    }()
}
```

`connectedNodes` 맵 덕분에 이미 수집 중인 피어는 중복 시작되지 않는다.

---

## 8. Priority Queue 정렬

### 왜 정렬이 필요한가?

분산 시스템에서 각 노드의 플로우는 **독립적인 시간순**으로 도착한다.
하지만 클라이언트는 **전역 시간순**을 기대한다.

```
[정렬 없이 - 문제 상황]

  Node-1:  t=1  t=3  t=5  t=7
  Node-2:  t=2  t=4  t=6  t=8

  채널에 도착하는 순서 (비결정적):
  t=1, t=2, t=4, t=3, t=5, t=6, t=8, t=7  ← 순서 보장 안됨!

[Priority Queue 적용 후]

  정렬 버퍼에서 가장 오래된 것부터 꺼냄:
  t=1, t=2, t=3, t=4, t=5, t=6, t=7, t=8  ← 전역 시간순!
```

### PriorityQueue 구현

```go
// pkg/hubble/relay/queue/priority_queue.go

type PriorityQueue struct {
    h minHeap
}

type minHeap []*observerpb.GetFlowsResponse

// heap.Interface 구현 보장
var _ heap.Interface = (*minHeap)(nil)
```

Go 표준 라이브러리 `container/heap`을 기반으로 한 **최소 힙(min-heap)** 구현이다.
타임스탬프가 가장 오래된(작은) 항목이 루트에 위치한다.

### Less 비교 함수 (정렬 기준)

```go
func (h minHeap) Less(i, j int) bool {
    if h[i].GetTime().GetSeconds() == h[j].GetTime().GetSeconds() {
        return h[i].GetTime().GetNanos() < h[j].GetTime().GetNanos()
    }
    return h[i].GetTime().GetSeconds() < h[j].GetTime().GetSeconds()
}
```

비교 순서:
1. 먼저 `Seconds` 비교
2. 같으면 `Nanos`(나노초) 비교
3. 더 작은(오래된) 값이 우선순위가 높음

### PopOlderThan: 시간 기반 배치 추출

```go
func (pq *PriorityQueue) PopOlderThan(t time.Time) []*observerpb.GetFlowsResponse {
    // 전체 큐를 담을 수 있는 슬라이스 사전 할당
    ret := make([]*observerpb.GetFlowsResponse, 0, pq.Len())
    for {
        resp := pq.Pop()
        if resp == nil {
            return ret // 큐 비어있음
        }
        if t.Before(resp.GetTime().AsTime()) {
            pq.Push(resp) // 아직 충분히 오래되지 않음 → 다시 넣기
            return ret
        }
        ret = append(ret, resp)
    }
}
```

### sortFlows: 정렬 파이프라인

```go
func sortFlows(ctx context.Context,
    flows <-chan *observerpb.GetFlowsResponse,
    qlen int,
    bufferDrainTimeout time.Duration,
) <-chan *observerpb.GetFlowsResponse {

    pq := queue.NewPriorityQueue(qlen) // 기본 용량 100
    sortedFlows := make(chan *observerpb.GetFlowsResponse, qlen)

    go func() {
        defer close(sortedFlows)
    flowsLoop:
        for {
            select {
            case flow, ok := <-flows:
                if !ok {
                    break flowsLoop // 입력 채널 닫힘
                }
                if pq.Len() == qlen {
                    f := pq.Pop()         // 버퍼 가득 → 가장 오래된 것 배출
                    sortedFlows <- f
                }
                pq.Push(flow)             // 새 플로우 추가
            case t := <-time.After(bufferDrainTimeout): // 기본 1초
                // 새 플로우가 없으면 오래된 것 배출
                for _, f := range pq.PopOlderThan(t.Add(-bufferDrainTimeout)) {
                    sortedFlows <- f
                }
            case <-ctx.Done():
                return
            }
        }
        // 입력 종료 → 큐 완전 배출
        for f := pq.Pop(); f != nil; f = pq.Pop() {
            sortedFlows <- f
        }
    }()
    return sortedFlows
}
```

```
[sortFlows 동작 원리]

  입력 채널 (flows) ──┐
                       │
                       ▼
  ┌────────────────────────────────┐
  │        Priority Queue          │
  │  (최대 qlen=100개 버퍼링)      │
  │                                │
  │  새 플로우 도착                 │
  │  ├── 큐 미가득 → Push          │
  │  └── 큐 가득  → Pop(가장 오래된│
  │                  것) 후 Push    │
  │                                │
  │  타임아웃 (1초간 플로우 없음)    │
  │  └── PopOlderThan() 실행       │
  │      → 충분히 오래된 것들 배출  │
  │                                │
  │  입력 종료                      │
  │  └── 전체 큐 순서대로 배출      │
  └─────────────┬──────────────────┘
                │
                ▼
  출력 채널 (sortedFlows) ──→ sendFlowsResponse ──→ Client
```

### 정렬 버퍼 크기 최적화

```go
// 버퍼 크기 결정 로직
qlen := s.opts.sortBufferMaxLen // 기본 100
if nqlen := req.GetNumber() * uint64(len(peers)); nqlen > 0 && nqlen < uint64(qlen) {
    qlen = int(nqlen)
}
```

예시:
- `hubble observe --last 5` (5노드 클러스터) → `5 * 5 = 25` → `qlen = 25`
- `hubble observe --last 200` (3노드) → `200 * 3 = 600` → `qlen = 100` (최대값)
- `hubble observe --follow` (Number=0) → `qlen = 100` (기본값)

---

## 9. 에러 집계 (Error Aggregation)

### 왜 에러 집계가 필요한가?

100개 노드 클러스터에서 네트워크 파티션이 발생하면 동시에 50개 노드에서 같은 에러가
발생할 수 있다. 이를 모두 개별 전달하면:
- 클라이언트에 에러 메시지 폭주
- 유용한 플로우 데이터가 에러에 묻힘
- 동일 에러의 반복으로 가독성 저하

### aggregateErrors 구현

```go
// pkg/hubble/relay/observer/observer.go

func aggregateErrors(ctx context.Context,
    responses <-chan *observerpb.GetFlowsResponse,
    errorAggregationWindow time.Duration, // 기본 10초
) <-chan *observerpb.GetFlowsResponse {

    aggregated := make(chan *observerpb.GetFlowsResponse, cap(responses))
    var flushPending <-chan time.Time
    var pendingResponse *observerpb.GetFlowsResponse

    go func() {
        defer close(aggregated)
    aggregateErrorsLoop:
        for {
            select {
            case response, ok := <-responses:
                if !ok {
                    // 보류 중인 에러 플러시 후 종료
                    if pendingResponse != nil {
                        aggregated <- pendingResponse
                    }
                    return
                }

                // 비에러 응답 → 바로 전달
                current := response.GetNodeStatus()
                if current.GetStateChange() != relaypb.NodeState_NODE_ERROR {
                    aggregated <- response
                    continue aggregateErrorsLoop
                }

                // 에러 응답 → 병합 시도
                if pending := pendingResponse.GetNodeStatus(); pending != nil {
                    if current.GetMessage() == pending.GetMessage() {
                        // 같은 에러 → 노드 이름만 추가
                        pending.NodeNames = append(pending.NodeNames, current.NodeNames...)
                        continue aggregateErrorsLoop
                    }
                    // 다른 에러 → 기존 것 플러시
                    aggregated <- pendingResponse
                }
                pendingResponse = response
                flushPending = time.After(errorAggregationWindow)

            case <-flushPending:
                // 타임아웃 → 보류 에러 플러시
                aggregated <- pendingResponse
                pendingResponse = nil
                flushPending = nil

            case <-ctx.Done():
                return
            }
        }
    }()
    return aggregated
}
```

```
[에러 집계 동작 시나리오]

  시간   이벤트                      pendingResponse          출력
  ─────────────────────────────────────────────────────────────────
  t=0    Flow(정상)                   nil                      Flow 전달
  t=1    Error("timeout", Node-1)    Error(Node-1)            -
  t=2    Error("timeout", Node-2)    Error(Node-1,2)          -
  t=3    Error("timeout", Node-3)    Error(Node-1,2,3)        -
  t=4    Flow(정상)                   Error(Node-1,2,3)        Flow 전달
  t=5    Error("refused", Node-4)    Error(Node-4)            Error(timeout,
                                                               Node-1,2,3) 플러시
  t=15   10초 타임아웃               nil                      Error(refused,
                                                               Node-4) 플러시
```

결과: 3개의 개별 timeout 에러가 하나로 병합되어 `NodeNames: [Node-1, Node-2, Node-3]`으로 전달.

---

## 10. NodeStatusEvent 시스템

### NodeStatusEvent 타입

Relay는 플로우 스트림에 **노드 상태 이벤트**를 함께 전송하여 클라이언트가
클러스터 상태를 파악할 수 있게 한다.

```go
// nodeStatusEvent 생성
func nodeStatusEvent(state relaypb.NodeState, nodeNames ...string) *observerpb.GetFlowsResponse {
    return &observerpb.GetFlowsResponse{
        NodeName: nodeTypes.GetAbsoluteNodeName(),
        Time:     timestamppb.New(time.Now()),
        ResponseTypes: &observerpb.GetFlowsResponse_NodeStatus{
            NodeStatus: &relaypb.NodeStatusEvent{
                StateChange: state,
                NodeNames:   nodeNames,
            },
        },
    }
}
```

### 노드 상태 종류

| NodeState | 의미 | 발생 시점 |
|-----------|------|----------|
| `NODE_CONNECTED` | 노드에 연결됨 | GetFlows 시작 시 |
| `NODE_UNAVAILABLE` | 노드 연결 불가 | GetFlows 시작 시 |
| `NODE_ERROR` | 노드 에러 발생 | 플로우 수집 중 에러 |
| `NODE_GONE` | 노드 제거됨 | (피어 삭제 시) |

### GetFlows에서의 NodeStatusEvent 전송 순서

```go
// 1. 먼저 연결된 노드 알림
if len(connectedNodes) > 0 {
    status := nodeStatusEvent(relaypb.NodeState_NODE_CONNECTED, connectedNodes...)
    stream.Send(status) // 모든 연결 노드를 한 번에 알림
}
// 2. 그 다음 불가용 노드 알림
if len(unavailableNodes) > 0 {
    status := nodeStatusEvent(relaypb.NodeState_NODE_UNAVAILABLE, unavailableNodes...)
    stream.Send(status)
}
// 3. 이후 실제 플로우 스트리밍 시작
err := sendFlowsResponse(ctx, stream, sortedFlows)
```

### nodeStatusError (에러 이벤트)

```go
func nodeStatusError(err error, nodeNames ...string) *observerpb.GetFlowsResponse {
    msg := err.Error()
    if s, ok := status.FromError(err); ok && s.Code() == codes.Unknown {
        msg = s.Message() // gRPC Unknown 에러는 메시지만 추출
    }
    return &observerpb.GetFlowsResponse{
        NodeName: nodeTypes.GetAbsoluteNodeName(),
        Time:     timestamppb.New(time.Now()),
        ResponseTypes: &observerpb.GetFlowsResponse_NodeStatus{
            NodeStatus: &relaypb.NodeStatusEvent{
                StateChange: relaypb.NodeState_NODE_ERROR,
                NodeNames:   nodeNames,
                Message:     msg,
            },
        },
    }
}
```

---

## 11. Health Check 시스템

### healthServer 구조

```go
// pkg/hubble/relay/server/health.go

type healthServer struct {
    svc           *health.Server       // gRPC 헬스 서비스
    pm            peerStatusReporter   // PeerManager 상태
    probeInterval time.Duration        // 기본 5초
    stopChan      chan struct{}
}
```

### 헬스 판정 로직

```go
func (hs healthServer) start() {
    check := func() {
        st := hs.pm.Status()
        if st.PeerServiceConnected && st.AvailablePeers > 0 {
            // SERVING: Peer Service 연결 + 가용 피어 있음
            hs.svc.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
            hs.svc.SetServingStatus(v1.ObserverServiceName,
                healthpb.HealthCheckResponse_SERVING)
        } else {
            // NOT_SERVING: 조건 미충족
            hs.svc.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
            hs.svc.SetServingStatus(v1.ObserverServiceName,
                healthpb.HealthCheckResponse_NOT_SERVING)
        }
    }
    go func() {
        check() // 즉시 한 번 실행
        for {
            select {
            case <-hs.stopChan: return
            case <-time.After(hs.probeInterval): check() // 5초마다
            }
        }
    }()
}
```

```
[Health Check 판정 매트릭스]

  PeerServiceConnected  │  AvailablePeers > 0  │  결과
  ──────────────────────┼──────────────────────┼────────────
  true                  │  true                │  SERVING
  true                  │  false               │  NOT_SERVING
  false                 │  true                │  NOT_SERVING
  false                 │  false               │  NOT_SERVING
```

### PeerManager.Status 메서드

```go
func (m *PeerManager) Status() Status {
    m.mu.RLock()
    defer m.mu.RUnlock()
    availablePeers := 0
    for _, peer := range m.peers {
        peer.mu.Lock()
        if peer.conn != nil {
            state := peer.conn.GetState()
            if state != connectivity.TransientFailure && state != connectivity.Shutdown {
                availablePeers++
            }
        }
        peer.mu.Unlock()
    }
    return Status{
        PeerServiceConnected: m.peerServiceConnected.Load(), // atomic
        AvailablePeers:       availablePeers,
    }
}
```

`isAvailable`과 `Status`의 판정 기준이 동일하다:
- `conn != nil`
- `state != TransientFailure`
- `state != Shutdown`

---

## 12. Prometheus 메트릭

### 메트릭 레지스트리 초기화

```go
// pkg/hubble/relay/server/server.go

var registry = prometheus.NewPedanticRegistry()

func init() {
    registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
    registry.MustRegister(collectors.NewGoCollector())
}
```

`NewPedanticRegistry()`는 표준 `NewRegistry()`보다 엄격한 검증을 수행한다:
- 동일 이름의 메트릭 중복 등록 방지
- 잘못된 라벨 조합 검출

### 연결 풀 메트릭

```go
// pkg/hubble/relay/pool/metrics.go

type PoolMetrics struct {
    PeerConnStatus   *prometheus.GaugeVec
    peerConnStatusMu lock.Mutex
}

func NewPoolMetrics(registry prometheus.Registerer) *PoolMetrics {
    m := &PoolMetrics{
        PeerConnStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
            Namespace: "hubble_relay",
            Subsystem: "pool",
            Name:      "peer_connection_status",
            Help:      "Measures the connectivity status of all peers...",
        }, []string{"status"}),
    }
    registry.MustRegister(m.PeerConnStatus)
    return m
}
```

### 메트릭 보고 시 전체 상태 초기화

```go
func (m *PoolMetrics) ObservePeerConnectionStatus(
    peerConnStatus map[connectivity.State]uint32,
    nilConnNum uint32,
) {
    // 모든 상태를 0으로 초기화 후 설정
    status := map[string]uint32{
        connectivity.Idle.String():             0,
        connectivity.Connecting.String():       0,
        connectivity.Ready.String():            0,
        connectivity.TransientFailure.String(): 0,
        connectivity.Shutdown.String():         0,
        nilConnectionLabelValue:                0,  // "NIL_CONNECTION"
    }
    status[nilConnectionLabelValue] = nilConnNum
    for state, num := range peerConnStatus {
        status[state.String()] = num
    }
    m.peerConnStatusMu.Lock()
    for state, num := range status {
        m.PeerConnStatus.WithLabelValues(state).Set(float64(num))
    }
    m.peerConnStatusMu.Unlock()
}
```

### 사용 가능한 메트릭 목록

| 메트릭 이름 | 타입 | 라벨 | 설명 |
|-------------|------|------|------|
| `hubble_relay_pool_peer_connection_status` | Gauge | status | 피어 연결 상태별 개수 |
| `process_*` | Various | - | 프로세스 레벨 메트릭 (CPU, 메모리 등) |
| `go_*` | Various | - | Go 런타임 메트릭 (GC, 고루틴 수 등) |
| `grpc_server_*` | Various | - | gRPC 서버 메트릭 (WithGRPCMetrics 시) |

### Metrics 서버 설정

```go
// server.go - New()
if opts.metricsListenAddress != "" {
    mux := http.NewServeMux()
    mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
    metricsServer = &http.Server{
        Addr:    opts.metricsListenAddress,
        Handler: mux,
    }
}
```

---

## 13. TLS 보안 아키텍처

### 이중 TLS 구성

Relay는 **서버 측**과 **클라이언트 측** 두 가지 TLS 설정이 필요하다.

```
[Relay TLS 아키텍처]

  Hubble CLI          Relay             Hubble (각 노드)
  ┌────────┐     ┌───────────────┐     ┌────────────┐
  │        │────→│ Server TLS    │     │            │
  │        │ TLS │ (TLS 1.3+)   │     │            │
  │        │     │               │────→│            │
  │        │     │ Client TLS   │ TLS │            │
  │        │     │ (mTLS)       │     │            │
  └────────┘     └───────────────┘     └────────────┘

  서버 TLS: CLI → Relay 연결 보호
  클라이언트 TLS: Relay → Hubble 연결 보호 (mTLS)
```

### 서버 TLS 설정

```go
// server.go - New()
if opts.serverTLSConfig != nil {
    tlsConfig := opts.serverTLSConfig.ServerConfig(&tls.Config{
        MinVersion: MinTLSVersion, // TLS 1.3
    })
    serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConfig)))
}

// server/option.go
var MinTLSVersion uint16 = tls.VersionTLS13
```

### 클라이언트 TLS (인증서 핫 리로드)

```go
// pkg/hubble/relay/pool/client.go

type grpcTLSCredentialsWrapper struct {
    credentials.TransportCredentials
    mu        lock.Mutex
    baseConf  *tls.Config
    TLSConfig certloader.ClientConfigBuilder
}

// 매 연결마다 최신 인증서 적용
func (w *grpcTLSCredentialsWrapper) ClientHandshake(ctx context.Context,
    addr string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
    w.mu.Lock()
    defer w.mu.Unlock()
    // 핸드셰이크 때마다 인증서 갱신
    w.TransportCredentials = credentials.NewTLS(w.TLSConfig.ClientConfig(w.baseConf))
    return w.TransportCredentials.ClientHandshake(ctx, addr, conn)
}
```

**왜 인증서 핫 리로드가 필요한가?**

Kubernetes 환경에서 인증서는 cert-manager 등에 의해 주기적으로 갱신된다.
`grpcTLSCredentialsWrapper`는 `ClientHandshake`가 호출될 때마다 `certloader`에서
최신 인증서를 가져오므로, **Relay 재시작 없이** 인증서 갱신이 가능하다.

### TLS ServerName 결정

```go
// Peer Service의 TLS ServerName
peerClientBuilder = &peerTypes.RemoteClientBuilder{
    TLSConfig:     opts.clientTLSConfig,
    TLSServerName: peer.TLSServerName(defaults.PeerServiceName, opts.clusterName),
    // 결과: "hubble-peer.default.hubble-grpc.cilium.io" 형태
}
```

---

## 14. Observer Server API 구현

### ServerStatus (클러스터 상태 집계)

```go
func (s *Server) ServerStatus(ctx context.Context,
    req *observerpb.ServerStatusRequest) (*observerpb.ServerStatusResponse, error) {

    peers := s.peers.List()
    statuses := make(chan *observerpb.ServerStatusResponse, len(peers))

    for _, p := range peers {
        if !isAvailable(p.Conn) {
            numUnavailableNodes++
            continue
        }
        g.Go(func() error {
            client := s.opts.ocb.observerClient(&p)
            status, err := client.ServerStatus(ctx, req)
            if err != nil {
                numUnavailableNodes++
                return nil // 에러여도 계속 진행
            }
            statuses <- status
            return nil
        })
    }

    // 응답 집계
    resp := &observerpb.ServerStatusResponse{
        Version: build.RelayVersion.String(),
    }
    for status := range statuses {
        resp.MaxFlows += status.MaxFlows     // 합산
        resp.NumFlows += status.NumFlows     // 합산
        resp.SeenFlows += status.SeenFlows   // 합산
        if resp.UptimeNs < status.UptimeNs {
            resp.UptimeNs = status.UptimeNs  // 최대값 (가장 오래 실행된 것)
        }
        resp.FlowsRate += status.FlowsRate   // 합산
    }
    resp.NumConnectedNodes = uint32(len(peers) - numUnavailableNodes)
    resp.NumUnavailableNodes = uint32(numUnavailableNodes)
    return resp, nil
}
```

```
[ServerStatus 집계 방식]

  필드             │ 집계 방식  │ 이유
  ─────────────────┼───────────┼────────────────────────────
  MaxFlows         │ 합산(+)   │ 클러스터 전체 용량 = 각 노드 합
  NumFlows         │ 합산(+)   │ 현재 저장된 플로우 총합
  SeenFlows        │ 합산(+)   │ 지금까지 관찰된 플로우 총합
  UptimeNs         │ 최대(max) │ 가장 오래 실행된 노드의 업타임
  FlowsRate        │ 합산(+)   │ 초당 플로우 비율의 합
  ConnectedNodes   │ 카운트    │ 연결 가능 노드 수
  UnavailableNodes │ 카운트    │ 연결 불가 노드 수 (최대 10개 이름 보고)
```

### GetNodes (노드 목록 조회)

```go
func (s *Server) GetNodes(ctx context.Context,
    req *observerpb.GetNodesRequest) (*observerpb.GetNodesResponse, error) {

    peers := s.peers.List()
    nodes := make([]*observerpb.Node, 0, len(peers))

    for _, p := range peers {
        n := &observerpb.Node{
            Name: p.Name,
            Tls:  &observerpb.TLS{
                Enabled:    p.TLSEnabled,
                ServerName: p.TLSServerName,
            },
        }
        if p.Address != nil {
            n.Address = p.Address.String()
        }
        nodes = append(nodes, n)

        if !isAvailable(p.Conn) {
            n.State = relaypb.NodeState_NODE_UNAVAILABLE
            continue
        }
        n.State = relaypb.NodeState_NODE_CONNECTED

        // 각 노드에 ServerStatus 병렬 질의
        g.Go(func() error {
            client := s.opts.ocb.observerClient(&p)
            status, err := client.ServerStatus(ctx, &observerpb.ServerStatusRequest{})
            if err != nil {
                n.State = relaypb.NodeState_NODE_ERROR
                return nil
            }
            n.Version = status.GetVersion()
            n.UptimeNs = status.GetUptimeNs()
            n.MaxFlows = status.GetMaxFlows()
            n.NumFlows = status.GetNumFlows()
            n.SeenFlows = status.GetSeenFlows()
            return nil
        })
    }
    g.Wait()
    return &observerpb.GetNodesResponse{Nodes: nodes}, nil
}
```

### GetNamespaces (네임스페이스 수집)

```go
func (s *Server) GetNamespaces(ctx context.Context,
    req *observerpb.GetNamespacesRequest) (*observerpb.GetNamespacesResponse, error) {

    // errgroup.WithContext 사용하지 않음 → 부분 결과 반환 가능
    g := new(errgroup.Group)
    nsManager := namespace.NewManager()

    for _, p := range s.peers.List() {
        if !isAvailable(p.Conn) { continue }
        g.Go(func() error {
            client := s.opts.ocb.observerClient(&p)
            nsResp, err := client.GetNamespaces(ctx, req)
            if err != nil { return nil } // 에러 무시, 부분 결과
            for _, ns := range nsResp.GetNamespaces() {
                nsManager.AddNamespace(ns)
            }
            return nil
        })
    }
    g.Wait()
    return &observerpb.GetNamespacesResponse{
        Namespaces: nsManager.GetNamespaces(),
    }, nil
}
```

**`errgroup.WithContext`를 사용하지 않는 이유**:
코드 주석에 명시되어 있다 - "We are not using errgroup.WithContext because we will
return partial results over failing on the first error". 하나의 노드가 실패해도
다른 노드의 결과는 반환하겠다는 의도이다.

---

## 15. 설정 기본값과 튜닝

### Relay 기본값 테이블

```go
// pkg/hubble/relay/defaults/defaults.go

const (
    ClusterName            = "default"
    HealthCheckInterval    = 5 * time.Second
    GopsPort               = 9893
    PprofAddress           = "localhost"
    PprofPort              = 6062
    RetryTimeout           = 30 * time.Second
    PeerTarget             = "unix://" + hubbleDefaults.SocketPath
    PeerServiceName        = "hubble-peer"
    SortBufferMaxLen       = 100
    SortBufferDrainTimeout = 1 * time.Second
    ErrorAggregationWindow = 10 * time.Second
    PeerUpdateInterval     = 2 * time.Second
)

var (
    ListenAddress       = ":4245"   // hubbleDefaults.RelayPort
    HealthListenAddress = ":4222"
)
```

### PeerManager 기본값

```go
// pkg/hubble/relay/pool/option.go

var defaultOptions = options{
    peerServiceAddress: defaults.PeerTarget,     // unix:///var/run/cilium/hubble.sock
    peerClientBuilder:  peerTypes.LocalClientBuilder{},
    clientConnBuilder:  GRPCClientConnBuilder{},
    connCheckInterval:  2 * time.Minute,          // 연결 상태 확인 주기
    connStatusInterval: 5 * time.Second,           // 메트릭 보고 주기
    retryTimeout:       defaults.RetryTimeout,     // 30초
}
```

### 튜닝 가이드

| 파라미터 | 기본값 | 조정 방향 | 영향 |
|----------|--------|----------|------|
| `SortBufferMaxLen` | 100 | 30~100 권장 | 높을수록 정렬 정확, 지연 증가 |
| `SortBufferDrainTimeout` | 1초 | 500ms~3초 | 낮으면 정렬 효과 감소 |
| `ErrorAggregationWindow` | 10초 | 5~30초 | 넓으면 에러 병합 더 많이 |
| `PeerUpdateInterval` | 2초 | 1~10초 | Follow 모드 피어 갱신 주기 |
| `connCheckInterval` | 2분 | 30초~5분 | 연결 복구 속도 vs 오버헤드 |
| `RetryTimeout` | 30초 | 10~60초 | Peer Service 재연결 간격 |
| `Backoff Min` | 1초 | - | 첫 재연결 대기 |
| `Backoff Max` | 1분 | - | 최대 재연결 대기 |
| `Backoff Factor` | 2.0 | - | 지수 증가 배수 |

---

## 16. 동시성 모델 분석

### 잠금 계층 구조

```
[Relay의 잠금 계층]

  PeerManager.mu (RWMutex)
     │
     └── peer.mu (Mutex)  ← 각 피어 개별 잠금

  flowCollector.mu (Mutex) ← connectedNodes 보호

  PoolMetrics.peerConnStatusMu (Mutex) ← 메트릭 보고
```

**교착 상태 방지 규칙**:
- `PeerManager.mu`를 먼저 획득한 후 `peer.mu`를 획득
- 역방향 잠금은 절대 발생하지 않음
- `connect()`는 `peer.mu`만 사용 (PeerManager.mu 불필요)

### 고루틴 라이프사이클

```
[Relay 시작 시 고루틴 생성 순서]

  Server.Serve()
  ├── errgroup 고루틴 1: metricsServer.ListenAndServe()
  ├── errgroup 고루틴 2: gRPC Server
  │   └── pm.Start()
  │       ├── PeerManager 고루틴 1: watchNotifications()
  │       ├── PeerManager 고루틴 2: manageConnections()
  │       └── PeerManager 고루틴 3: reportConnectionStatus()
  │   └── healthServer.start()
  │       └── 헬스체크 고루틴
  └── errgroup 고루틴 3: healthGRPCServer.Serve()

  GetFlows 요청 시 추가 고루틴:
  ├── 각 피어당 1개: retrieveFlowsFromPeer()
  ├── g.Wait() + close(flows) 고루틴
  ├── aggregateErrors 고루틴
  ├── sortFlows 고루틴
  └── (Follow 모드) 피어 갱신 고루틴
```

### 채널 사용 패턴

| 채널 | 버퍼 크기 | 목적 |
|------|----------|------|
| `PeerManager.updated` | 100 | 피어 변경 알림 (watchNotifications → manageConnections) |
| `PeerManager.stop` | 0 (unbuffered) | 종료 신호 (close로 브로드캐스트) |
| `flows` | qlen (기본 100) | 피어별 수집 플로우 합류 |
| `aggregated` | cap(flows) | 에러 집계 결과 |
| `sortedFlows` | qlen | 시간순 정렬 결과 |
| `statuses` | len(peers) | ServerStatus 응답 수집 |

### isAvailable 판정 로직

```go
func isAvailable(conn poolTypes.ClientConn) bool {
    if conn == nil {
        return false
    }
    state := conn.GetState()
    return state != connectivity.TransientFailure &&
        state != connectivity.Shutdown
}
```

이 함수는 `Idle`, `Connecting`, `Ready` 상태를 모두 "사용 가능"으로 판정한다.
`Idle`이나 `Connecting` 상태의 연결을 사용하면 gRPC가 자동으로 연결을 시도한다.

---

## 17. 설계 결정 분석 (Why)

### Q1: 왜 errgroup을 사용하는가?

**Server.Serve()**에서 3개의 독립 서버를 동시에 실행할 때 `errgroup.Group`을 사용한다.
이는 "하나가 실패하면 전체가 실패해야 한다"는 원칙을 구현한다.

만약 메트릭 서버만 종료되고 gRPC 서버는 계속 돌면, 모니터링 없이 서비스가 운영되는
위험한 상태가 된다. `errgroup`은 이를 방지한다.

단, `GetNamespaces`에서는 의도적으로 `errgroup.WithContext`를 사용하지 않는다.
부분 결과라도 반환하는 것이 아무것도 반환하지 않는 것보다 낫기 때문이다.

### Q2: 왜 PeerManager에 3개 고루틴인가?

```
단일 고루틴 (가능하지만 비효율적):
  loop:
    watchNotifications() ← 블로킹! 다른 작업 불가

3개 고루틴 (현재 설계):
  고루틴 1: watchNotifications() ← 블로킹 수신 전담
  고루틴 2: manageConnections()  ← 연결 관리 전담
  고루틴 3: reportConnectionStatus() ← 메트릭 전담
```

`watchNotifications`의 `client.Recv()`는 **블로킹 호출**이다.
이것이 단일 고루틴에서 실행되면, Recv() 대기 중에는 연결 관리나 메트릭 보고가 불가능하다.
3개로 분리하면 각각 독립적으로 동작할 수 있다.

### Q3: 왜 updated 채널에 버퍼 크기 100인가?

```go
updated: make(chan string, 100),
```

`watchNotifications`가 빠르게 여러 피어 변경을 수신할 수 있다 (예: 클러스터 시작 시).
버퍼가 없으면 `upsert` → `updated <- hp.Name`에서 블로킹되어 `watchNotifications`의
Recv 루프가 멈출 수 있다. 100의 버퍼는 대부분의 클러스터 크기를 커버한다.

### Q4: 왜 정렬 버퍼 크기를 요청별로 조정하는가?

```go
if nqlen := req.GetNumber() * uint64(len(peers)); nqlen > 0 && nqlen < uint64(qlen) {
    qlen = int(nqlen)
}
```

`hubble observe --last 5`로 5개 플로우만 요청하면, 100개짜리 버퍼는 낭비이다.
`5 * 피어수`로 줄이면 메모리를 절약하면서도 충분한 정렬이 가능하다.
주석에도 "don't make the queue bigger than necessary"라고 명시되어 있다.

### Q5: 왜 에러 집계에 시간 윈도우를 사용하는가?

네트워크 파티션은 일반적으로 여러 노드에 **거의 동시에** 영향을 미친다.
10초 윈도우는 "거의 동시"의 범위를 정의한다.

- 너무 짧으면 (1초): 같은 원인의 에러가 별도로 보고됨
- 너무 길면 (60초): 에러 보고가 지연됨
- 10초: 대부분의 네트워크 파티션에서 모든 영향받는 노드의 에러를 캡처

### Q6: 왜 인증서 핫 리로드가 필요한가?

Kubernetes에서 cert-manager는 인증서를 주기적으로 갱신한다 (예: 90일마다).
`grpcTLSCredentialsWrapper`는 `ClientHandshake`가 호출될 때마다 최신 인증서를
`certloader`에서 가져온다. 이렇게 하면:

1. Relay를 재시작하지 않고도 인증서 갱신 가능
2. 서비스 중단 없이 mTLS 보안 유지
3. 자동화된 인증서 관리 파이프라인과 호환

### Q7: 왜 Follow 모드에서 피어를 주기적으로 갱신하는가?

```go
case <-time.After(s.opts.peerUpdateInterval): // 2초
    peers := s.peers.List()
    _, _ = fc.collect(gctx, g, peers, flows)
```

`--follow` 모드는 수 시간, 수 일 동안 실행될 수 있다. 그 동안:
- 새 노드가 클러스터에 추가될 수 있음
- `connectedNodes` 맵이 이미 수집 중인 노드를 추적하므로 중복 방지
- 2초 간격은 새 노드를 빠르게 발견하면서도 과도한 피어 조회를 방지

### Q8: 왜 numUnavailableNodesReportMax가 10인가?

```go
const numUnavailableNodesReportMax = 10
```

1000개 노드 클러스터에서 절반이 불가용하면 500개 이름을 보고하는 것은 무의미하다.
10개면 문제의 패턴(특정 존, 특정 노드 그룹)을 파악하기에 충분하고,
응답 크기도 합리적으로 유지된다.

### Q9: 왜 비에러 응답은 에러 집계 파이프라인을 바로 통과하는가?

```go
if current.GetStateChange() != relaypb.NodeState_NODE_ERROR {
    aggregated <- response
    continue aggregateErrorsLoop
}
```

정상 플로우의 지연을 최소화하기 위함이다. 에러 집계는 에러 응답에만 적용되고,
정상 플로우와 NodeStatus 이벤트(CONNECTED, UNAVAILABLE)는 **즉시 전달**된다.
이는 관찰 가능성의 핵심인 "실시간성"을 보장한다.

---

## 요약

Hubble Relay는 분산 네트워크 관찰 데이터를 중앙에서 통합하는 정교한 프록시이다.

| 컴포넌트 | 역할 | 핵심 메커니즘 |
|----------|------|-------------|
| Server | 전체 조율 | errgroup 3-고루틴 서버 |
| PeerManager | 피어 관리 | 3-고루틴 (감시, 연결, 보고) |
| Observer Server | API 구현 | 병렬 수집 + 파이프라인 처리 |
| PriorityQueue | 시간순 정렬 | min-heap 기반 버퍼 |
| aggregateErrors | 에러 병합 | 시간 윈도우 기반 동일 에러 결합 |
| healthServer | 가용성 판정 | PeerService + AvailablePeers 조건 |
| PoolMetrics | 모니터링 | connectivity.State별 Gauge |
| TLS Wrapper | 보안 | 인증서 핫 리로드 |

핵심 설계 원칙:
1. **탄력성**: 부분 실패에도 서비스 계속 (에러 무시, 부분 결과 반환)
2. **투명성**: 인증서 리로드, 피어 동적 관리를 클라이언트에게 투명하게
3. **효율성**: 정렬 버퍼 크기 동적 조정, 에러 집계로 노이즈 감소
4. **관찰성**: Prometheus 메트릭, NodeStatusEvent로 상태 가시화
