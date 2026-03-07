# 07. xDS Discovery Service 심화 분석

## 목차

1. [개요](#1-개요)
2. [DiscoveryServer 구조와 핵심 필드](#2-discoveryserver-구조와-핵심-필드)
3. [ADS와 Delta XDS 프로토콜 비교](#3-ads와-delta-xds-프로토콜-비교)
4. [디바운싱 메커니즘](#4-디바운싱-메커니즘)
5. [PushQueue 상세](#5-pushqueue-상세)
6. [동시성 제어](#6-동시성-제어)
7. [커넥션 라이프사이클](#7-커넥션-라이프사이클)
8. [푸시 순서](#8-푸시-순서)
9. [캐싱 전략과 부분 푸시](#9-캐싱-전략과-부분-푸시)
10. [Generator별 동작](#10-generator별-동작)
11. [왜 이렇게 설계했나](#11-왜-이렇게-설계했나)

---

## 1. 개요

Istio의 컨트롤 플레인(istiod)은 Envoy 프록시에 구성을 전달하기 위해 **xDS(x Discovery Service)** 프로토콜을 사용한다. xDS는 Envoy가 동적으로 설정을 수신할 수 있도록 gRPC 기반의 스트리밍 API를 제공하며, Istio의 Pilot 컴포넌트가 이 서버 역할을 수행한다.

xDS Discovery Service는 다음과 같은 핵심 문제를 해결한다:

- **동적 구성 전달**: Kubernetes 리소스(Service, Endpoint, Istio CRD 등)의 변경을 실시간으로 Envoy에 반영
- **효율적 푸시**: 수천 개의 Envoy 프록시에 동시에 구성을 전달하면서도 컨트롤 플레인 과부하를 방지
- **일관성 보장**: CDS -> EDS -> LDS -> RDS 순서로 구성을 전달하여 Envoy가 일관된 상태를 유지
- **부분 업데이트**: 변경된 리소스만 선택적으로 전달하여 네트워크 대역폭과 Envoy의 재구성 비용을 절약

핵심 소스코드 위치:

```
pilot/pkg/xds/
├── discovery.go       # DiscoveryServer, Start(), Push(), ConfigUpdate(), debounce()
├── ads.go             # Stream(), initConnection(), pushConnection(), PushOrder
├── delta.go           # StreamDeltas(), pushDeltaXds(), pushConnectionDelta()
├── xdsgen.go          # pushXds(), findGenerator()
├── pushqueue.go       # PushQueue, Enqueue(), Dequeue(), MarkDone()
├── proxy_dependencies.go  # DefaultProxyNeedsPush, 프록시별 필터링
├── cds.go             # CdsGenerator
├── eds.go             # EdsGenerator, EDSUpdate()
├── lds.go             # LdsGenerator
└── rds.go             # RdsGenerator
```

---

## 2. DiscoveryServer 구조와 핵심 필드

### 2.1 구조체 정의

`DiscoveryServer`는 Istio Pilot의 xDS gRPC 구현체다. Envoy의 ADS(Aggregated Discovery Service) 인터페이스를 구현하며, 모든 xDS 리소스 타입(CDS, EDS, LDS, RDS, SDS 등)을 단일 gRPC 스트림으로 서비스한다.

> 소스: `pilot/pkg/xds/discovery.go` 65~141행

```go
// DiscoveryServer is Pilot's gRPC implementation for Envoy's xds APIs
type DiscoveryServer struct {
    Env *model.Environment

    Generators map[string]model.XdsResourceGenerator
    Collections map[string]CollectionGenerator

    ProxyNeedsPush func(proxy *model.Proxy, req *model.PushRequest) (*model.PushRequest, bool)

    concurrentPushLimit chan struct{}
    RequestRateLimit    *rate.Limiter

    InboundUpdates   *atomic.Int64
    CommittedUpdates *atomic.Int64

    pushChannel chan *model.PushRequest
    pushQueue   *PushQueue

    adsClients      map[string]*Connection
    adsClientsMutex sync.RWMutex

    Authenticators []security.Authenticator

    serverReady atomic.Bool
    DebounceOptions DebounceOptions
    Cache model.XdsCache

    pushVersion atomic.Uint64
    DiscoveryStartTime time.Time
    // ...
}
```

### 2.2 핵심 필드 상세

각 필드의 역할을 다이어그램으로 표현하면 다음과 같다:

```
 ConfigUpdate() 호출
       |
       v
 +------------------+         +-------------------+         +-----------------+
 |   pushChannel    | ------> |    debounce()     | ------> |   Push()        |
 | (chan, 버퍼 10)   |         | (병합 + 타이머)    |         | (PushContext    |
 +------------------+         +-------------------+         |  초기화)         |
                                                            +-----------------+
                                                                    |
                                                                    v
                                                            +-----------------+
                                                            |   pushQueue     |
                                                            | (FIFO + 병합)   |
                                                            +-----------------+
                                                                    |
                                                   concurrentPushLimit (세마포어)
                                                                    |
                                                                    v
                                                            +-----------------+
                                                            |  각 Connection  |
                                                            | 의 PushCh()에   |
                                                            | Event 전달      |
                                                            +-----------------+
```

**pushChannel** (`chan *model.PushRequest`, 버퍼 크기 10):
- `ConfigUpdate()`가 호출될 때 `PushRequest`가 이 채널로 전달된다.
- 디바운싱의 입력 버퍼 역할을 하며, 버퍼 크기 10은 짧은 시간에 발생하는 연속적 변경 이벤트를 수용한다.

**pushQueue** (`*PushQueue`):
- 디바운싱이 완료된 후 각 커넥션에 대한 푸시 요청을 관리하는 FIFO 큐다.
- 같은 커넥션에 대한 중복 요청은 자동으로 병합된다 (상세는 5장에서 설명).

**concurrentPushLimit** (`chan struct{}`, 용량 = `PushThrottle`):
- 동시에 진행할 수 있는 푸시의 수를 제한하는 세마포어 역할이다.
- 기본값은 CPU 코어 수에 따라 자동 결정되며 최대 100이다 (상세는 6장에서 설명).

**adsClients** (`map[string]*Connection`):
- 현재 연결된 모든 Envoy 프록시를 추적한다. 키는 `{proxy.ID}-{connectionNumber}` 형식이다.
- `adsClientsMutex`(RWMutex)로 보호되어 안전한 동시 접근을 보장한다.

**Generators** (`map[string]model.XdsResourceGenerator`):
- 각 xDS 리소스 타입에 대한 생성기(Generator)를 등록한다.
- 키는 `typeURL` 또는 `{proxyType}/{typeURL}` 또는 `{generatorMetadata}/{typeURL}` 형식이다.
- `findGenerator()`가 이 맵에서 적절한 생성기를 찾는다.

**DebounceOptions**:
- `DebounceAfter` (기본 100ms): 이벤트 후 최소 대기 시간
- `debounceMax` (기본 10s): 이벤트 병합 최대 대기 시간
- `enableEDSDebounce` (기본 true): EDS 푸시도 디바운싱 적용 여부

### 2.3 초기화 - NewDiscoveryServer

> 소스: `pilot/pkg/xds/discovery.go` 144~175행

```go
func NewDiscoveryServer(env *model.Environment, clusterAliases map[string]string,
    debugger *krt.DebugHandler) *DiscoveryServer {
    out := &DiscoveryServer{
        Env:                 env,
        Generators:          map[string]model.XdsResourceGenerator{},
        ProxyNeedsPush:      DefaultProxyNeedsPush,
        concurrentPushLimit: make(chan struct{}, features.PushThrottle),
        RequestRateLimit:    rate.NewLimiter(rate.Limit(features.RequestLimit), 1),
        InboundUpdates:      atomic.NewInt64(0),
        CommittedUpdates:    atomic.NewInt64(0),
        pushChannel:         make(chan *model.PushRequest, 10),
        pushQueue:           NewPushQueue(),
        adsClients:          map[string]*Connection{},
        DebounceOptions: DebounceOptions{
            DebounceAfter:     features.DebounceAfter,    // 100ms
            debounceMax:       features.DebounceMax,      // 10s
            enableEDSDebounce: features.EnableEDSDebounce, // true
        },
        Cache: env.Cache,
        // ...
    }
    // ...
}
```

### 2.4 Start - 고루틴 시작

`Start()` 메서드는 DiscoveryServer의 핵심 고루틴 4개를 시작한다:

> 소스: `pilot/pkg/xds/discovery.go` 226~232행

```go
func (s *DiscoveryServer) Start(stopCh <-chan struct{}) {
    go s.WorkloadEntryController.Run(stopCh)  // 워크로드 자동 등록
    go s.handleUpdates(stopCh)                 // 디바운싱 루프
    go s.periodicRefreshMetrics(stopCh)        // 메트릭 갱신 (10s 주기)
    go s.sendPushes(stopCh)                    // 푸시 큐 소비 루프
    go s.Cache.Run(stopCh)                     // XDS 캐시 관리
}
```

```
                    Start()
                      |
        +-------------+-------------+-------------+
        |             |             |             |
        v             v             v             v
  handleUpdates  sendPushes  periodicRefresh  Cache.Run
  (debounce)     (pushQueue)  Metrics(10s)    (캐시 관리)
```

---

## 3. ADS와 Delta XDS 프로토콜 비교

Istio는 두 가지 xDS 프로토콜 변형을 모두 지원한다: **State of the World (SotW)** ADS와 **Delta** (Incremental) xDS.

### 3.1 SotW ADS (Stream)

SotW 방식에서는 매 응답마다 해당 타입의 **전체 리소스 목록**을 전송한다. 예를 들어 CDS 푸시 시 모든 클러스터 정보를 보낸다.

> 소스: `pilot/pkg/xds/ads.go` 188~236행

```go
func (s *DiscoveryServer) Stream(stream DiscoveryStream) error {
    // 1. 서버 준비 상태 확인
    if !s.IsServerReady() {
        return status.Error(codes.Unavailable, "server is not ready...")
    }
    // 2. Rate Limit 대기
    if err := s.WaitForRequestLimit(stream.Context()); err != nil {
        return status.Errorf(codes.ResourceExhausted, "...")
    }
    // 3. 인증
    ids, err := s.authenticate(ctx)
    // 4. PushContext 초기화
    s.globalPushContext().InitContext(s.Env, nil, nil)
    // 5. Connection 생성 및 xDS 스트림 시작
    con := newConnection(peerAddr, stream)
    con.ids = ids
    con.s = s
    return xds.Stream(con)
}
```

SotW 흐름에서 `pushConnection()`이 각 커넥션에 대한 구성을 생성하고 전송한다:

> 소스: `pilot/pkg/xds/ads.go` 472~496행

```go
func (s *DiscoveryServer) pushConnection(con *Connection, pushEv *Event) error {
    pushRequest := pushEv.pushRequest
    if pushRequest.Full {
        s.computeProxyState(con.proxy, pushRequest)
    }
    pushRequest, needsPush := s.ProxyNeedsPush(con.proxy, pushRequest)
    if !needsPush {
        return nil
    }
    // watchedResourcesByOrder()가 PushOrder에 따라 정렬된 리소스 목록을 반환
    wrl := con.watchedResourcesByOrder()
    for _, w := range wrl {
        if err := s.pushXds(con, w, pushRequest); err != nil {
            return err
        }
    }
    return nil
}
```

### 3.2 Delta XDS (StreamDeltas)

Delta 방식에서는 **변경된 리소스만** 전송한다. 새로 추가/수정된 리소스와 제거된 리소스 목록을 별도로 전달한다.

> 소스: `pilot/pkg/xds/delta.go` 43~148행

```go
func (s *DiscoveryServer) StreamDeltas(stream DeltaDiscoveryStream) error {
    // ... (인증, Rate Limit 등 SotW와 동일)
    con := newDeltaConnection(peerAddr, stream)
    go s.receiveDelta(con, ids)

    <-con.InitializedCh()  // 초기화 완료 대기

    for {
        // 요청을 먼저 처리 (높은 우선순위)
        select {
        case req, ok := <-con.deltaReqChan:
            if ok {
                if err := s.processDeltaRequest(req, con); err != nil {
                    return err
                }
            }
        default:
        }
        // 요청과 푸시를 폴링
        select {
        case req, ok := <-con.deltaReqChan:
            // ...
        case ev := <-con.PushCh():
            pushEv := ev.(*Event)
            err := s.pushConnectionDelta(con, pushEv)
            pushEv.done()
            // ...
        case <-con.StopCh():
            return nil
        }
    }
}
```

Delta 방식의 핵심 차이는 `pushDeltaXds()` 함수에서 나타난다:

> 소스: `pilot/pkg/xds/delta.go` 465~594행

```go
func (s *DiscoveryServer) pushDeltaXds(con *Connection, w *model.WatchedResource,
    req *model.PushRequest) error {
    gen := s.findGenerator(w.TypeUrl, con)
    // ...
    var res model.Resources
    var deletedRes model.DeletedResources
    switch g := gen.(type) {
    case model.XdsDeltaResourceGenerator:
        // Delta 지원 Generator는 GenerateDeltas()를 호출
        res, deletedRes, logdata, usedDelta, err = g.GenerateDeltas(con.proxy, req, w)
    case model.XdsResourceGenerator:
        // 일반 Generator는 Generate()를 호출
        res, logdata, err = g.Generate(con.proxy, w, req)
    }
    // DeltaDiscoveryResponse에 RemovedResources 포함
    resp := &discovery.DeltaDiscoveryResponse{
        TypeUrl:   w.TypeUrl,
        Resources: res,
    }
    if usedDelta {
        resp.RemovedResources = deletedRes
    } else if req.Full {
        // SotW 폴백: 현재 리소스에 없는 이전 리소스를 제거 목록에 추가
        removed := w.ResourceNames.Copy()
        for _, r := range res {
            removed.Delete(r.Name)
        }
        resp.RemovedResources = sets.SortedList(removed)
    }
    return con.sendDelta(resp, newResourceNames)
}
```

### 3.3 프로토콜 비교 요약

| 항목 | SotW (ADS) | Delta (Incremental) |
|------|-----------|---------------------|
| 스트림 구조 | 단일 goroutine (xds.Stream) | 별도 수신 goroutine (receiveDelta) + 메인 루프 |
| 응답 형식 | `DiscoveryResponse` (전체 리소스) | `DeltaDiscoveryResponse` (추가 + 제거 목록) |
| 요청 형식 | `ResourceNames` (전체 구독 목록) | `ResourceNamesSubscribe` + `ResourceNamesUnsubscribe` |
| ACK/NACK | `ResponseNonce` + `VersionInfo` | `ResponseNonce` + `ErrorDetail` |
| 재연결 | 전체 리소스 재전송 | `InitialResourceVersions`로 기존 상태 전달 |
| Generator 인터페이스 | `Generate()` | `GenerateDeltas()` (미지원 시 `Generate()` 폴백) |
| CDS 후 EDS 보장 | Envoy가 EDS 요청 전송 | `forceEDSPush()`로 서버가 능동적으로 전송 |

### 3.4 Delta의 CDS 후 EDS 강제 푸시

Delta 프로토콜에서는 CDS 요청 처리 후 반드시 EDS 푸시를 강제로 수행한다. 이는 Envoy가 Delta 모드에서 CDS 응답 후 EDS 요청을 보내지 않을 수 있어 클러스터가 영원히 "warming" 상태에 머물 수 있는 문제를 해결한다:

> 소스: `pilot/pkg/xds/delta.go` 336~349행

```go
func (s *DiscoveryServer) forceEDSPush(con *Connection) error {
    if dwr := con.proxy.GetWatchedResource(v3.EndpointType); dwr != nil {
        request := &model.PushRequest{
            Full:   true,
            Push:   con.proxy.LastPushContext,
            Reason: model.NewReasonStats(model.DependentResource),
            Start:  con.proxy.LastPushTime,
            Forced: true,
        }
        deltaLog.Infof("ADS:%s: FORCE %s PUSH for warming.",
            v3.GetShortType(v3.EndpointType), con.ID())
        return s.pushDeltaXds(con, dwr, request)
    }
    return nil
}
```

---

## 4. 디바운싱 메커니즘

### 4.1 왜 디바운싱이 필요한가

Kubernetes 환경에서 하나의 논리적 변경(예: Deployment 스케일 아웃)은 수십~수백 개의 개별 리소스 이벤트를 발생시킨다. 각 이벤트마다 즉시 푸시하면 컨트롤 플레인이 과부하되고 Envoy가 불완전한 중간 상태를 수신할 수 있다. 디바운싱은 이러한 이벤트를 하나의 푸시로 병합한다.

### 4.2 디바운싱 설정값

> 소스: `pilot/pkg/features/tuning.go` 77~97행

| 환경변수 | 기본값 | 설명 |
|---------|--------|------|
| `PILOT_DEBOUNCE_AFTER` | 100ms | 마지막 이벤트 후 최소 대기 시간 |
| `PILOT_DEBOUNCE_MAX` | 10s | 이벤트 병합 최대 대기 시간 |
| `PILOT_ENABLE_EDS_DEBOUNCE` | true | EDS 푸시도 디바운싱 적용 여부 |

### 4.3 디바운싱 흐름

`ConfigUpdate()` -> `pushChannel` -> `debounce()` -> `Push()`의 흐름을 거친다.

> 소스: `pilot/pkg/xds/discovery.go` 311~331행

```go
func (s *DiscoveryServer) ConfigUpdate(req *model.PushRequest) {
    // ...
    inboundConfigUpdates.Increment()
    s.InboundUpdates.Inc()
    s.pushChannel <- req    // 디바운싱 입력
}

func (s *DiscoveryServer) handleUpdates(stopCh <-chan struct{}) {
    debounce(s.pushChannel, stopCh, s.DebounceOptions, s.Push, s.CommittedUpdates)
}
```

### 4.4 debounce() 함수 상세

> 소스: `pilot/pkg/xds/discovery.go` 343~425행

디바운싱의 핵심 로직은 `debounce()` 함수에 있다. 타이머, 이벤트 병합, 직렬화된 푸시의 세 가지 메커니즘이 조합된다.

```
 시간축 -->

 이벤트:  E1    E2  E3       E4                        E5
          |     |   |        |                         |
          v     v   v        v                         v
 타이머:  [--100ms--]        [--100ms--]
                   ^새 타이머          ^조용해짐 → 푸시!
                   (리셋)

 병합:    req = E1.Merge(E2).Merge(E3)  →  Push(req)
          E4 → 새 디바운스 주기 시작
```

```go
func debounce(ch chan *model.PushRequest, stopCh <-chan struct{},
    opts DebounceOptions, pushFn func(req *model.PushRequest),
    updateSent *atomic.Int64) {

    var timeChan <-chan time.Time
    var startDebounce time.Time
    var lastConfigUpdateTime time.Time
    var req *model.PushRequest
    free := true
    freeCh := make(chan struct{}, 1)

    push := func(req *model.PushRequest, debouncedEvents int, startDebounce time.Time) {
        pushFn(req)
        updateSent.Add(int64(debouncedEvents))
        freeCh <- struct{}{}    // 푸시 완료 신호
    }

    pushWorker := func() {
        eventDelay := time.Since(startDebounce)
        quietTime := time.Since(lastConfigUpdateTime)

        // 조건: (1) 최대 대기시간 초과 또는 (2) 충분히 조용해짐
        if eventDelay >= opts.debounceMax || quietTime >= opts.DebounceAfter {
            if req != nil {
                free = false
                go push(req, debouncedEvents, startDebounce)
                req = nil
                debouncedEvents = 0
            }
        } else {
            // 아직 조건 미충족: 남은 시간만큼 타이머 재설정
            timeChan = time.After(opts.DebounceAfter - quietTime)
        }
    }

    for {
        select {
        case <-freeCh:
            free = true
            pushWorker()    // 이전 푸시 완료 후 대기중인 요청 처리

        case r := <-ch:
            if !opts.enableEDSDebounce && !r.Full {
                // EDS 디바운싱 비활성 시 즉시 푸시
                go func(req *model.PushRequest) {
                    pushFn(req)
                    updateSent.Inc()
                }(r)
                continue
            }
            lastConfigUpdateTime = time.Now()
            if debouncedEvents == 0 {
                timeChan = time.After(opts.DebounceAfter)
                startDebounce = lastConfigUpdateTime
            }
            debouncedEvents++
            req = req.Merge(r)   // 요청 병합

        case <-timeChan:
            if free {
                pushWorker()
            }
            // free가 아니면 freeCh에서 깨어날 때 처리

        case <-stopCh:
            return
        }
    }
}
```

### 4.5 디바운싱 상태 다이어그램

```
                     +----------+
                     |   IDLE   |
                     | (req=nil)|
                     +----+-----+
                          |
                    이벤트 수신 (r := <-ch)
                    req = r
                    timer = After(100ms)
                          |
                          v
                   +--------------+
               +-->| ACCUMULATING |<--+
               |   | (debouncedN++)|  |
               |   +------+-------+  |
               |          |           |
               |   timer 만료         |
               |   quietTime < 100ms  |
               |   (타이머 재설정)      |
               +----------+           |
                                      |
                    이벤트 수신         |
                    req.Merge(r)       |
                    lastConfigUpdate=now|
                    (타이머 유지)       |
                    ------------------+
                          |
              timer 만료 AND
              (quietTime >= 100ms OR
               eventDelay >= 10s)
                          |
                          v
                   +--------------+
                   |   PUSHING    |
                   | (free=false) |
                   +------+-------+
                          |
                    freeCh <- struct{}
                    (푸시 완료)
                          |
                          v
                   +----------+
                   |   IDLE   |
                   +----------+
```

### 4.6 직렬화 보장 (free 플래그)

`debounce()` 함수에서 `free` 변수는 한 번에 하나의 푸시만 진행되도록 보장한다. 이전 `Push()`가 완료되기 전에 타이머가 만료되더라도, `pushWorker()`는 `free`가 false이면 실행되지 않는다. 이전 푸시가 완료되면(`freeCh`에서 신호 수신) 그제야 대기 중인 요청을 처리한다.

이 직렬화는 `Push()` 내부에서 `initPushContext()`가 호출될 때 순서 보장이 필요하기 때문이다. `initPushContext()`는 전역 PushContext를 설정하므로 병렬 실행 시 역순 기록(A 후 B가 시작되었지만 B가 먼저 완료되어 A의 오래된 상태로 덮어쓰는 문제)이 발생할 수 있다.

---

## 5. PushQueue 상세

### 5.1 구조

> 소스: `pilot/pkg/xds/pushqueue.go` 23~39행

```go
type PushQueue struct {
    cond *sync.Cond

    // pending: 큐에 대기 중인 커넥션별 푸시 요청
    pending map[*Connection]*model.PushRequest

    // queue: FIFO 순서를 유지하는 슬라이스
    queue []*Connection

    // processing: Dequeue()되었지만 MarkDone() 안 된 커넥션
    // 값이 nil이면 진행 중, non-nil이면 재큐잉 필요
    processing map[*Connection]*model.PushRequest

    shuttingDown bool
}
```

### 5.2 3-상태 모델

PushQueue는 각 커넥션에 대해 3가지 상태를 관리한다:

```
                  Enqueue()
    +----------+ --------> +---------+
    |  없음     |           | pending |
    | (큐 밖)   | <-------- | (대기)  |
    +----------+  Dequeue() +---------+
         ^                       |
         |                  Dequeue()
         |                       |
         |                       v
         |              +------------+
         +------------- | processing |
           MarkDone()   | (처리 중)   |
           (재큐잉 없음) +-----+------+
                              |
                         Enqueue() 호출 시:
                         processing[con] = merged_req
                              |
                         MarkDone() 호출 시:
                         pending으로 이동
                              |
                              v
                        +---------+
                        | pending |
                        | (재큐잉)|
                        +---------+
```

### 5.3 Enqueue - 요청 추가와 병합

> 소스: `pilot/pkg/xds/pushqueue.go` 51~74행

```go
func (p *PushQueue) Enqueue(con *Connection, pushRequest *model.PushRequest) {
    p.cond.L.Lock()
    defer p.cond.L.Unlock()

    if p.shuttingDown {
        return
    }

    // 이미 처리 중인 커넥션이면 processing에 병합
    if request, f := p.processing[con]; f {
        p.processing[con] = request.CopyMerge(pushRequest)
        return
    }

    // 이미 대기 중인 커넥션이면 pending에 병합
    if request, f := p.pending[con]; f {
        p.pending[con] = request.CopyMerge(pushRequest)
        return
    }

    // 새로운 커넥션: pending에 추가하고 큐에 삽입
    p.pending[con] = pushRequest
    p.queue = append(p.queue, con)
    p.cond.Signal()
}
```

핵심 설계 포인트:
1. **같은 커넥션의 중복 큐잉 방지**: pending이나 processing에 이미 있는 커넥션은 큐에 다시 추가하지 않고 요청만 병합한다.
2. **CopyMerge 사용**: 기존 요청과 새 요청을 병합하여 `ConfigsUpdated`, `Full`, `Reason` 등을 합산한다.
3. **processing 중 Enqueue**: 현재 푸시가 진행 중인 커넥션에 새 변경이 발생하면, processing 맵에 저장해두었다가 `MarkDone()` 시 자동 재큐잉한다.

### 5.4 Dequeue - FIFO 추출

> 소스: `pilot/pkg/xds/pushqueue.go` 77~104행

```go
func (p *PushQueue) Dequeue() (con *Connection, request *model.PushRequest, shutdown bool) {
    p.cond.L.Lock()
    defer p.cond.L.Unlock()

    // 큐가 비어있으면 대기
    for len(p.queue) == 0 && !p.shuttingDown {
        p.cond.Wait()
    }
    if len(p.queue) == 0 {
        return nil, nil, true
    }

    con = p.queue[0]
    p.queue[0] = nil        // GC를 위한 nil 설정
    p.queue = p.queue[1:]

    request = p.pending[con]
    delete(p.pending, con)

    // processing 상태로 전환
    p.processing[con] = nil

    return con, request, false
}
```

**GC 최적화**: `p.queue[0] = nil`은 슬라이스의 기저 배열이 여전히 참조를 유지하여 GC가 수거하지 못하는 문제를 방지한다. grpc-go에서도 동일한 이슈가 보고된 바 있다 (grpc/grpc-go#4758).

### 5.5 MarkDone - 완료와 재큐잉

> 소스: `pilot/pkg/xds/pushqueue.go` 106~119행

```go
func (p *PushQueue) MarkDone(con *Connection) {
    p.cond.L.Lock()
    defer p.cond.L.Unlock()
    request := p.processing[con]
    delete(p.processing, con)

    // 처리 중에 Enqueue된 요청이 있으면 재큐잉
    if request != nil {
        p.pending[con] = request
        p.queue = append(p.queue, con)
        p.cond.Signal()
    }
}
```

이 메커니즘은 "진행 중인 푸시가 완료된 후 최신 변경사항 반영"을 보장한다. 푸시가 완료되기를 기다리지 않고도 새 변경을 안전하게 기록할 수 있다.

### 5.6 PushQueue 전체 흐름 예시

```
시간  작업                          pending      processing   queue
----  ----                          -------      ----------   -----
T1    Enqueue(A, req1)              {A:req1}     {}           [A]
T2    Enqueue(B, req2)              {A:r1,B:r2}  {}           [A,B]
T3    Enqueue(A, req3)  ← 병합      {A:r1+r3,B}  {}           [A,B]
T4    Dequeue() → (A, r1+r3)       {B:r2}       {A:nil}      [B]
T5    Enqueue(A, req4) ← proc 병합  {B:r2}       {A:r4}       [B]
T6    MarkDone(A) → 재큐잉          {B:r2,A:r4}  {}           [B,A]
T7    Dequeue() → (B, r2)          {A:r4}       {B:nil}      [A]
```

---

## 6. 동시성 제어

### 6.1 PILOT_PUSH_THROTTLE

> 소스: `pilot/pkg/features/tuning.go` 39~56행

```go
PushThrottle = func() int {
    v := env.Register(
        "PILOT_PUSH_THROTTLE",
        0,
        "Limits the number of concurrent pushes allowed...",
    ).Get()
    if v > 0 {
        return v
    }
    procs := runtime.GOMAXPROCS(0)
    // 휴리스틱: 코어 수에 비례하여 스케일
    // 1코어: 20, 2코어: 25, 4코어: 35, 32코어: 100
    return min(15+5*procs, 100)
}()
```

기본값이 0이면 CPU 코어 수에 기반한 휴리스틱으로 자동 결정된다. 공식은 `min(15 + 5*GOMAXPROCS, 100)`이다.

| CPU 코어 | PushThrottle |
|---------|-------------|
| 1 | 20 |
| 2 | 25 |
| 4 | 35 |
| 8 | 55 |
| 16 | 95 |
| 17+ | 100 (최대) |

### 6.2 세마포어 기반 동시성 제어

> 소스: `pilot/pkg/xds/discovery.go` 469~514행

`doSendPushes()` 함수에서 `concurrentPushLimit` 채널을 세마포어로 사용한다:

```go
func doSendPushes(stopCh <-chan struct{}, semaphore chan struct{}, queue *PushQueue) {
    for {
        select {
        case <-stopCh:
            return
        default:
            // 세마포어 획득: 채널이 가득 차면 블록
            semaphore <- struct{}{}

            // 큐에서 다음 커넥션과 푸시 요청을 꺼냄
            client, push, shuttingdown := queue.Dequeue()
            if shuttingdown {
                return
            }

            // 완료 콜백: 세마포어 해제 + MarkDone
            doneFunc := func() {
                queue.MarkDone(client)
                <-semaphore               // 세마포어 릴리스
            }

            // 비동기로 PushCh에 이벤트 전달
            go func() {
                pushEv := &Event{
                    pushRequest: push,
                    done:        doneFunc,
                }
                select {
                case client.PushCh() <- pushEv:
                    return
                case <-closed:
                    doneFunc()
                }
            }()
        }
    }
}
```

```
  doSendPushes goroutine (단일)
         |
         |  semaphore <- struct{}{}  (용량 = PushThrottle)
         |         |
         |    queue.Dequeue() (블로킹)
         |         |
         |    go func() {
         |         client.PushCh() <- pushEv  ← 각 커넥션의 처리 goroutine으로 전달
         |    }
         |
         |  ... 반복 ...
         |
  세마포어가 가득 차면 doSendPushes가 블록됨
  → 기존 푸시 중 하나가 doneFunc()으로 세마포어를 해제해야 진행
```

### 6.3 RequestRateLimit

새 xDS 연결의 유입 속도를 제한하기 위해 `rate.Limiter`를 사용한다:

> 소스: `pilot/pkg/xds/discovery.go` 585~595행

```go
func (s *DiscoveryServer) WaitForRequestLimit(ctx context.Context) error {
    if s.RequestRateLimit.Limit() == 0 {
        return nil
    }
    wait, cancel := context.WithTimeout(ctx, time.Second)
    defer cancel()
    return s.RequestRateLimit.Wait(wait)
}
```

기본값은 PushThrottle과 동일한 휴리스틱(`min(15+5*procs, 100)` QPS)이며, `PILOT_MAX_REQUESTS_PER_SECOND`로 오버라이드 가능하다. 1초 이내에 레이트 리밋이 통과되지 않으면 `ResourceExhausted` 에러를 반환하여 클라이언트가 다른 istiod 인스턴스로 재연결하도록 유도한다.

---

## 7. 커넥션 라이프사이클

### 7.1 전체 흐름

```
  Envoy 프록시                        istiod (DiscoveryServer)
      |                                       |
      |--- gRPC 연결 ---->                    |
      |                          Stream() / StreamDeltas()
      |                               |
      |                          서버 준비 상태 확인
      |                          Rate Limit 대기
      |                          authenticate()
      |                               |
      |<-- 첫 번째 요청 ------------>  |
      |   (Node ID, Metadata)     initConnection()
      |                             ├─ initProxyMetadata()
      |                             ├─ authorize()
      |                             ├─ addCon()  (adsClients에 등록)
      |                             ├─ initializeProxy()
      |                             │   ├─ WorkloadEntryController.OnConnect()
      |                             │   ├─ computeProxyState()
      |                             │   │   ├─ SetServiceTargets()
      |                             │   │   ├─ SetWorkloadLabels()
      |                             │   │   ├─ setTopologyLabels()
      |                             │   │   ├─ SetSidecarScope()
      |                             │   │   └─ SetGatewaysForProxy()
      |                             │   ├─ DiscoverIPMode()
      |                             │   └─ WatchedResources 초기화
      |                             └─ MarkInitialized()
      |                                       |
      |<-- CDS 응답 --                        |
      |<-- EDS 응답 --                        |
      |<-- LDS 응답 --                        |
      |<-- RDS 응답 --                        |
      |                                       |
      |--- ACK/요청 --------->   processRequest() / processDeltaRequest()
      |                                       |
      |  (구성 변경 발생 시)                    |
      |                          pushConnection() / pushConnectionDelta()
      |<-- 업데이트 푸시 --                    |
      |                                       |
      |--- 연결 종료 --------->  closeConnection()
      |                             ├─ removeCon()
      |                             └─ WorkloadEntryController.OnDisconnect()
```

### 7.2 initConnection 상세

> 소스: `pilot/pkg/xds/ads.go` 240~282행

```go
func (s *DiscoveryServer) initConnection(node *core.Node, con *Connection,
    identities []string) error {
    // 1. 프록시 메타데이터 파싱
    proxy, err := s.initProxyMetadata(node)
    // 2. 클러스터 별칭 적용
    if alias, exists := s.ClusterAliases[proxy.Metadata.ClusterID]; exists {
        proxy.Metadata.ClusterID = alias
    }
    // 3. LastPushContext 설정 (단조 증가 보장)
    proxy.LastPushContext = s.globalPushContext()
    // 4. 커넥션 ID 생성 및 등록
    con.SetID(connectionID(proxy.ID))
    con.node = node
    con.proxy = proxy
    // 5. 인가 확인
    if err := s.authorize(con, identities); err != nil {
        return err
    }
    // 6. adsClients에 등록 (푸시 수신 가능)
    //    중요: initializeProxy보다 먼저 등록해야 새 PushContext를 놓치지 않음
    s.addCon(con.ID(), con)
    defer con.MarkInitialized()
    // 7. 프록시 완전 초기화
    if err := s.initializeProxy(con); err != nil {
        s.closeConnection(con)
        return err
    }
    return nil
}
```

**`addCon` 타이밍이 중요한 이유**: 주석에서 설명하듯, `initializeProxy`가 완료된 후 `addCon`을 하면 경쟁 조건이 발생한다. `initializeProxy`와 `addCon` 사이에 새로운 PushContext가 생성되면, 해당 프록시는 새 PushContext에 대한 푸시를 받지 못한다. 따라서 먼저 등록하고 나중에 초기화한다.

### 7.3 computeProxyState

> 소스: `pilot/pkg/xds/ads.go` 385~446행

`computeProxyState()`는 프록시의 상태를 최신 PushContext에 맞게 갱신한다. 전체 푸시와 초기 연결 시 호출되며, 변경된 config 종류에 따라 선택적으로 재계산한다:

```go
func (s *DiscoveryServer) computeProxyState(proxy *model.Proxy, request *model.PushRequest) {
    proxy.Lock()
    defer proxy.Unlock()

    // ServiceTargets: 초기화 또는 서비스 변경 시 재계산
    if request == nil || request.Forced ||
        proxy.ShouldUpdateServiceTargets(request.ConfigsUpdated) {
        proxy.SetServiceTargets(s.Env.ServiceDiscovery)
        shouldResetGateway = true
    }

    // WorkloadLabels: 초기화 또는 프록시 업데이트 시 재계산
    if request == nil || request.IsProxyUpdate() {
        proxy.SetWorkloadLabels(s.Env)
        setTopologyLabels(proxy)
    }

    // SidecarScope: 초기화 또는 관련 config 변경 시 재계산
    //   (ServiceEntry, DestinationRule, VirtualService, Sidecar, Ingress)
    if shouldResetSidecarScope {
        proxy.SetSidecarScope(push)
    }

    // Gateway: Router 또는 Ambient East-West Gateway일 때만
    if shouldResetGateway && (proxy.Type == model.Router || ...) {
        proxy.SetGatewaysForProxy(push)
    }
}
```

---

## 8. 푸시 순서

### 8.1 PushOrder 정의

> 소스: `pilot/pkg/xds/ads.go` 500~509행

```go
var PushOrder = []string{
    v3.ClusterType,                // CDS
    v3.EndpointType,               // EDS
    v3.ListenerType,               // LDS
    v3.RouteType,                  // RDS
    v3.SecretType,                 // SDS
    v3.AddressType,                // Address (Ambient)
    v3.WorkloadType,               // Workload (Ambient)
    v3.WorkloadAuthorizationType,  // WorkloadAuthorization (Ambient)
}
```

### 8.2 순서 보장 메커니즘

`watchedResourcesByOrder()`가 프록시가 구독한 리소스를 `PushOrder`에 정의된 순서대로 정렬하여 반환한다:

> 소스: `pilot/pkg/xds/ads.go` 622~638행

```go
func (conn *Connection) watchedResourcesByOrder() []*model.WatchedResource {
    allWatched := conn.proxy.ShallowCloneWatchedResources()
    ordered := make([]*model.WatchedResource, 0, len(allWatched))
    // 1. 알려진 타입을 PushOrder 순서로 추가
    for _, tp := range PushOrder {
        if allWatched[tp] != nil {
            ordered = append(ordered, allWatched[tp])
        }
    }
    // 2. 미지정 타입을 그 뒤에 추가
    for tp, res := range allWatched {
        if !KnownOrderedTypeUrls.Contains(tp) {
            ordered = append(ordered, res)
        }
    }
    return ordered
}
```

### 8.3 왜 CDS -> EDS -> LDS -> RDS 순서인가

이 순서는 Envoy의 xDS 프로토콜 스펙에 명시된 의존성 순서를 따른다:

```
  CDS (Cluster Discovery)
   |
   +-- 클러스터 정의가 있어야...
   |
   v
  EDS (Endpoint Discovery)
   |
   +-- 엔드포인트가 있어야 클러스터가 "warming" 완료
   |
   v
  LDS (Listener Discovery)
   |
   +-- 리스너가 라우트를 참조하므로...
   |
   v
  RDS (Route Discovery)
   |
   +-- 라우트가 클러스터를 참조 (CDS가 먼저 있어야 함)
   |
   v
  SDS (Secret Discovery)
       +-- TLS 인증서 (리스너/클러스터가 참조)
```

1. **CDS 먼저**: 클러스터 정의가 있어야 EDS 엔드포인트를 매핑할 수 있다.
2. **EDS 다음**: 엔드포인트가 할당되어야 클러스터의 "warming" 상태가 완료된다.
3. **LDS 이후**: 리스너가 활성화되어야 트래픽을 받을 수 있다.
4. **RDS 마지막**: 라우트 설정은 리스너가 참조하며, 리스너 없이는 의미가 없다.

이 순서를 어기면 Envoy에서 "warming" 상태가 해소되지 않거나 알 수 없는 클러스터를 참조하는 라우트가 생겨 트래픽 손실이 발생할 수 있다.

---

## 9. 캐싱 전략과 부분 푸시

### 9.1 XDS 캐시

DiscoveryServer는 `Cache` 필드(`model.XdsCache`)를 통해 생성된 xDS 리소스를 캐싱한다. 캐시 무효화는 변경된 설정에 따라 선택적으로 이루어진다:

> 소스: `pilot/pkg/xds/discovery.go` 259~267행

```go
func (s *DiscoveryServer) dropCacheForRequest(req *model.PushRequest) {
    if req.Forced {
        // 강제 푸시: 전체 캐시 클리어
        s.Cache.ClearAll()
    } else {
        // 일반 푸시: 변경된 설정에 해당하는 캐시만 클리어
        s.Cache.Clear(req.ConfigsUpdated)
    }
}
```

### 9.2 EDS 캐시 상세

EDS Generator는 클러스터별로 엔드포인트를 캐싱한다. 캐시 히트 시 재생성을 건너뛴다:

> 소스: `pilot/pkg/xds/eds.go` 167~228행

```go
func (eds *EdsGenerator) buildEndpoints(proxy *model.Proxy,
    req *model.PushRequest, w *model.WatchedResource) (model.Resources, model.XdsLogDetails) {

    var edsUpdatedServices map[string]struct{}
    // 부분 푸시 가능 여부 확인
    if !req.Full || canSendPartialFullPushes(req) {
        edsUpdatedServices = model.ConfigNamesOfKind(req.ConfigsUpdated, kind.ServiceEntry)
    }

    for clusterName := range w.ResourceNames {
        // 변경된 서비스에 해당하지 않으면 스킵
        if edsUpdatedServices != nil {
            if _, ok := edsUpdatedServices[...]; !ok {
                continue
            }
        }
        builder := endpoints.NewEndpointBuilder(clusterName, proxy, req.Push)

        // 캐시 확인
        cachedEndpoint := eds.Cache.Get(&builder)
        if cachedEndpoint != nil {
            resources = append(resources, cachedEndpoint)
            cached++
            continue
        }

        // 캐시 미스: 새로 생성하고 캐시에 추가
        l := builder.BuildClusterLoadAssignment(eds.EndpointIndex)
        resource := &discovery.Resource{...}
        eds.Cache.Add(&builder, req, resource)
    }
    return resources, model.XdsLogDetails{
        Incremental:    len(edsUpdatedServices) != 0,
        AdditionalInfo: fmt.Sprintf("empty:%v cached:%v/%v", empty, cached, cached+regenerated),
    }
}
```

### 9.3 부분 푸시 (Partial Push)

부분 푸시는 전체 리소스를 재생성하지 않고 변경된 부분만 업데이트하는 최적화 기법이다. 두 가지 수준에서 적용된다:

**1. 프록시 수준 필터링 (DefaultProxyNeedsPush)**:

> 소스: `pilot/pkg/xds/proxy_dependencies.go` 114~127행

```go
func DefaultProxyNeedsPush(proxy *model.Proxy, req *model.PushRequest) (*model.PushRequest, bool) {
    if req.Forced {
        return req, true
    }
    if proxy.IsWaypointProxy() || proxy.IsZTunnel() {
        return req, true
    }
    // 프록시와 관련된 설정 변경만 필터링
    req = filterRelevantUpdates(proxy, req)
    return req, len(req.ConfigsUpdated) > 0
}
```

`filterRelevantUpdates()`는 SidecarScope의 `DependsOnConfig()` 등을 사용하여 해당 프록시에 실제로 영향을 미치는 설정 변경만 남긴다. 예를 들어 namespace A의 VirtualService 변경은 namespace B의 Sidecar 프록시에는 전달하지 않는다.

**2. 인크리멘탈 EDS (non-Full Push)**:

Non-Full Push (EDS only)는 디바운싱 단계에서 `req.Full = false`로 처리된다. 이 경우 PushContext를 새로 생성하지 않고 기존 것을 사용한다:

> 소스: `pilot/pkg/xds/discovery.go` 270~276행

```go
func (s *DiscoveryServer) Push(req *model.PushRequest) {
    if !req.Full {
        req.Push = s.globalPushContext()
        s.dropCacheForRequest(req)
        s.AdsPushAll(req)
        return
    }
    // Full push: PushContext 재생성 ...
}
```

**3. EDS Delta Push**:

> 소스: `pilot/pkg/xds/eds.go` 140~165행

```go
func shouldUseDeltaEds(req *model.PushRequest) bool {
    if !req.Full {
        return false
    }
    return canSendPartialFullPushes(req)
}

// canSendPartialFullPushes: ConfigsUpdated에 ServiceEntry만 있으면
// (= 엔드포인트만 변경) 부분 전체 푸시가 가능
func canSendPartialFullPushes(req *model.PushRequest) bool {
    if req.Forced {
        return false
    }
    for cfg := range req.ConfigsUpdated {
        if skippedEdsConfigs.Contains(cfg.Kind) {
            continue
        }
        if cfg.Kind != kind.ServiceEntry {
            return false
        }
    }
    return true
}
```

`buildDeltaEndpoints()`는 변경된 서비스의 엔드포인트만 재생성하고, 삭제된 서비스는 `removed` 목록에 추가한다. 이를 통해 수천 개의 클러스터 중 변경된 소수만 업데이트할 수 있다.

---

## 10. Generator별 동작

### 10.1 findGenerator - Generator 탐색 순서

> 소스: `pilot/pkg/xds/xdsgen.go` 73~107행

```go
func (s *DiscoveryServer) findGenerator(typeURL string, con *Connection) model.XdsResourceGenerator {
    // 1. Agentgateway 전용 Collections 확인
    if con.proxy.Type == model.Agentgateway && features.EnableAgentgateway {
        c, f := s.Collections[typeURL]
        if f { return c }
        return CollectionGenerator{}
    }
    // 2. Generator 메타데이터 + TypeURL (예: "grpc/CDS")
    if g, f := s.Generators[con.proxy.Metadata.Generator+"/"+typeURL]; f {
        return g
    }
    // 3. 프록시 타입 + TypeURL (예: "sidecar/CDS")
    if g, f := s.Generators[string(con.proxy.Type)+"/"+typeURL]; f {
        return g
    }
    // 4. TypeURL만 (예: "CDS")
    if g, f := s.Generators[typeURL]; f {
        return g
    }
    // 5. 커넥션 기본 Generator
    g := con.proxy.XdsResourceGenerator
    if g == nil {
        if strings.HasPrefix(typeURL, TypeDebugPrefix) {
            g = s.Generators["event"]
        } else {
            g = s.Generators["api"]   // MCP generator (기본)
        }
    }
    return g
}
```

탐색 순서가 구체적인 것에서 일반적인 것으로 이동하여, 특정 프록시 타입이나 Generator에 대한 커스터마이징이 가능하다.

### 10.2 CDS Generator

> 소스: `pilot/pkg/xds/cds.go`

```go
type CdsGenerator struct {
    ConfigGenerator core.ConfigGenerator
}

var _ model.XdsDeltaResourceGenerator = &CdsGenerator{}
```

**needsPush 필터링** (`cdsNeedsPush()`):

CDS에 영향을 주지 않는 config 종류를 `skippedCdsConfigs`로 정의하여 불필요한 재생성을 방지한다:

```go
var skippedCdsConfigs = sets.New(
    kind.Gateway,             // CDS에 무관
    kind.WorkloadEntry,       // CDS에 무관
    kind.WorkloadGroup,       // CDS에 무관
    kind.AuthorizationPolicy, // CDS에 무관
    kind.RequestAuthentication,
    kind.Secret,
    kind.Telemetry,
    kind.WasmPlugin,
    kind.ProxyConfig,
    kind.DNSName,
)
```

추가 최적화:
- **Router 프록시**: VirtualService는 CDS 빌드에 거의 사용되지 않으므로 Router에서는 스킵 (Sidecar는 SidecarScope에서 VS 목적지를 사용하므로 스킵 불가)
- **Gateway 필터링**: `PILOT_FILTER_GATEWAY_CLUSTER_CONFIG=true`일 때, Gateway에 실제 연결된 서비스만 CDS에 포함
- **Auto-passthrough Gateway**: Gateway 설정 변경 시 auto-passthrough 모드/호스트 변경을 추가 확인

**Generate/GenerateDeltas**:

```go
func (c CdsGenerator) Generate(...) (model.Resources, model.XdsLogDetails, error) {
    req, needsPush := cdsNeedsPush(req, proxy)
    if !needsPush {
        return nil, model.DefaultXdsLogDetails, nil
    }
    clusters, logs := c.ConfigGenerator.BuildClusters(proxy, req)
    return clusters, logs, nil
}

func (c CdsGenerator) GenerateDeltas(...) (...) {
    // cdsNeedsPush 후 BuildDeltaClusters 호출
    updatedClusters, removedClusters, logs, usedDelta :=
        c.ConfigGenerator.BuildDeltaClusters(proxy, req, w)
    return updatedClusters, removedClusters, logs, usedDelta, nil
}
```

### 10.3 EDS Generator

> 소스: `pilot/pkg/xds/eds.go`

```go
type EdsGenerator struct {
    Cache         model.XdsCache
    EndpointIndex *model.EndpointIndex
}

var _ model.XdsDeltaResourceGenerator = &EdsGenerator{}
```

**needsPush 필터링** (`edsNeedsPush()`):

```go
var skippedEdsConfigs = sets.New(
    kind.Gateway,
    kind.VirtualService,      // EDS에 무관
    kind.WorkloadGroup,
    kind.AuthorizationPolicy,
    kind.RequestAuthentication,
    kind.Secret,
    kind.Telemetry,
    kind.WasmPlugin,
    kind.ProxyConfig,
    kind.DNSName,
)
```

CDS와의 주요 차이점: EDS에서는 VirtualService를 스킵한다(경로 정보는 엔드포인트에 영향 없음). 반면 CDS에서는 Sidecar의 SidecarScope가 VirtualService 목적지를 참조하므로 스킵하지 못한다.

**EDSUpdate 흐름**:

> 소스: `pilot/pkg/xds/eds.go` 47~61행

```go
func (s *DiscoveryServer) EDSUpdate(shard model.ShardKey, serviceName string,
    namespace string, istioEndpoints []*model.IstioEndpoint) {

    inboundEDSUpdates.Increment()
    // EndpointIndex 업데이트 후 푸시 타입 결정
    pushType := s.Env.EndpointIndex.UpdateServiceEndpoints(
        shard, serviceName, namespace, istioEndpoints, true)

    if pushType == model.IncrementalPush || pushType == model.FullPush {
        s.ConfigUpdate(&model.PushRequest{
            Full:           pushType == model.FullPush,
            ConfigsUpdated: sets.New(model.ConfigKey{
                Kind: kind.ServiceEntry, Name: serviceName, Namespace: namespace,
            }),
            Reason: model.NewReasonStats(model.EndpointUpdate),
        })
    }
}
```

엔드포인트 변경은 `IncrementalPush`(non-full)로 처리될 수 있으며, 이 경우 디바운싱에서 `enableEDSDebounce`가 false이면 즉시 전달된다.

### 10.4 LDS Generator

> 소스: `pilot/pkg/xds/lds.go`

```go
type LdsGenerator struct {
    ConfigGenerator core.ConfigGenerator
}
var _ model.XdsResourceGenerator = &LdsGenerator{}
```

LDS는 `XdsDeltaResourceGenerator`를 구현하지 않는다(SotW `Generate()`만 구현). Delta 스트림에서도 `Generate()`가 폴백으로 호출된다.

**needsPush 필터링** (`ldsNeedsPush()`):

LDS는 프록시 타입별로 스킵 목록이 다르다:

```go
var skippedLdsConfigs = map[model.NodeType]sets.Set[kind.Kind]{
    model.Router: sets.New(
        kind.WorkloadGroup, kind.WorkloadEntry, kind.Secret,
        kind.ProxyConfig, kind.DNSName,
    ),
    model.SidecarProxy: sets.New(
        kind.Gateway,       // Gateway는 Sidecar LDS에 무관
        kind.WorkloadGroup, kind.WorkloadEntry, kind.Secret,
        kind.ProxyConfig, kind.DNSName,
    ),
    model.Waypoint: sets.New(
        kind.Gateway, kind.WorkloadGroup, kind.WorkloadEntry,
        kind.Secret, kind.ProxyConfig, kind.DNSName,
    ),
}
```

Router는 `kind.Gateway`를 스킵하지 않는다(Gateway가 리스너 구성에 직접 영향). 반면 SidecarProxy와 Waypoint는 Gateway를 스킵한다.

### 10.5 RDS Generator

> 소스: `pilot/pkg/xds/rds.go`

```go
type RdsGenerator struct {
    ConfigGenerator core.ConfigGenerator
}
var _ model.XdsResourceGenerator = &RdsGenerator{}
```

**needsPush 필터링** (`rdsNeedsPush()`):

```go
var skippedRdsConfigs = sets.New[kind.Kind](
    kind.WorkloadEntry,
    kind.WorkloadGroup,
    kind.AuthorizationPolicy,
    kind.RequestAuthentication,
    kind.PeerAuthentication,
    kind.Secret,
    kind.WasmPlugin,
    kind.Telemetry,
    kind.ProxyConfig,
    kind.DNSName,
)
```

RDS는 `PeerAuthentication`도 스킵한다(mTLS 설정은 라우트에 직접 영향 없음). VirtualService와 DestinationRule은 스킵하지 않는다(라우트 설정에 핵심적).

### 10.6 Generator 필터링 비교표

| Kind | CDS | EDS | LDS (Sidecar) | LDS (Router) | RDS |
|------|-----|-----|---------------|-------------|-----|
| ServiceEntry | O | O | O | O | O |
| DestinationRule | O | O | O | O | O |
| VirtualService | Router 스킵 | 스킵 | O | O | O |
| Gateway | 스킵 | 스킵 | 스킵 | O | O |
| Sidecar | O | O | O | O | O |
| AuthorizationPolicy | 스킵 | 스킵 | O | O | 스킵 |
| RequestAuthentication | 스킵 | 스킵 | O | O | 스킵 |
| PeerAuthentication | O | O | O | O | 스킵 |
| WorkloadEntry | 스킵 | 스킵 | 스킵 | 스킵 | 스킵 |
| Secret | 스킵 | 스킵 | 스킵 | 스킵 | 스킵 |
| Telemetry | 스킵 | 스킵 | O | O | 스킵 |
| WasmPlugin | 스킵 | 스킵 | O | O | 스킵 |

(O = 푸시 필요, 스킵 = 해당 변경 시 재생성 불필요)

---

## 11. 왜 이렇게 설계했나

### 11.1 왜 디바운싱을 사용하는가?

**문제**: Kubernetes에서 Deployment 롤아웃 한 번이 수십~수백 개의 Pod/Endpoint 이벤트를 연쇄적으로 발생시킨다. 각 이벤트마다 모든 Envoy에 즉시 구성을 푸시하면:
- istiod CPU/메모리 폭증
- Envoy가 초당 수십 번 재구성
- 중간 상태(일부 엔드포인트만 업데이트된 상태)가 노출

**해결**: 100ms 디바운싱으로 이벤트를 병합하되, 10초 최대 대기로 응답성도 보장한다. `free` 플래그를 통한 직렬화로 PushContext 생성의 순서 일관성을 유지한다.

### 11.2 왜 PushQueue에 3-상태 모델을 사용하는가?

**문제**: 한 커넥션의 푸시가 진행 중일 때 새로운 변경이 발생하면 어떻게 해야 하는가?
- 즉시 중단하고 새 푸시 시작? -> 이전 푸시가 완료되지 않아 Envoy에 불완전한 상태 전달
- 새 변경을 버림? -> 최신 상태가 반영되지 않음
- 큐에 중복 추가? -> 같은 커넥션에 대해 연속 두 번 푸시 낭비

**해결**: `processing` 맵에 병합된 요청을 저장해두었다가 `MarkDone()` 시 자동 재큐잉한다. 이를 통해:
- 진행 중인 푸시는 정상 완료
- 새 변경사항은 유실 없이 보존
- 불필요한 중간 푸시 없이 최종 상태로 직행

### 11.3 왜 세마포어로 동시성을 제한하는가?

**문제**: 수천 개의 Envoy 커넥션에 동시에 구성을 생성하고 전송하면:
- Go 런타임의 goroutine 스케줄링 과부하
- 메모리 급증 (각 구성 생성에 수 MB)
- gRPC 전송 버퍼 경합

**해결**: 채널 기반 세마포어(`concurrentPushLimit`)로 동시 푸시를 CPU 코어에 비례하는 합리적 수준(최대 100)으로 제한한다. 채널을 세마포어로 사용하는 패턴은 Go에서 표준적이며, 뮤텍스보다 `select`문과 자연스럽게 결합된다.

### 11.4 왜 Generator 패턴을 사용하는가?

**문제**: 모든 xDS 리소스 타입을 하나의 거대한 함수에서 처리하면 코드가 비대해지고 확장이 어렵다.

**해결**: `XdsResourceGenerator` 인터페이스로 각 타입별 생성 로직을 분리하고, `findGenerator()`의 계층적 탐색으로 프록시 타입별 커스터마이징이 가능하다. 새로운 xDS 타입(예: Ambient의 Address/Workload)을 추가할 때 기존 코드를 수정하지 않고 Generator만 등록하면 된다.

### 11.5 왜 각 Generator가 개별 needsPush 필터를 갖는가?

**문제**: 모든 config 변경에 대해 모든 xDS 타입을 재생성하면 불필요한 계산이 발생한다.

**해결**: 각 Generator가 자신에게 영향을 미치는 config 종류를 정확히 알고, 관련 없는 변경 시 `nil`을 반환하여 전송을 건너뛴다. 예를 들어:
- VirtualService 변경 → RDS만 재생성, EDS는 스킵
- Secret 변경 → 모든 Generator가 스킵 (SDS만 관련, 별도 경로)
- AuthorizationPolicy 변경 → LDS만 재생성 (필터 체인에 RBAC 삽입)

이 "opt-out" 패턴(`skippedConfigs` set)은 새 config 종류가 추가되었을 때 기본적으로 모든 Generator가 반응하도록 하여 안전한 기본값을 제공한다. 새 config이 특정 Generator에 무관한 것이 확인되면 그때 `skippedConfigs`에 추가한다.

### 11.6 왜 Delta XDS에서 CDS 후 EDS를 강제 푸시하는가?

**문제**: Delta 프로토콜에서 Envoy가 CDS 응답을 받은 후 EDS 요청을 보내지 않을 수 있다. SotW에서는 Envoy가 CDS 응답 후 EDS 요청을 보내지만, Delta에서는 이 동작이 보장되지 않는다.

> 참조: https://github.com/envoyproxy/envoy/issues/13009

**시나리오**:
1. Envoy가 재연결 후 EDS 요청을 먼저 보내고 서버가 응답
2. CDS 요청이 이어지고 서버가 새 클러스터 목록으로 응답
3. Envoy가 클러스터 변경을 감지하고 warming을 시작하지만 EDS 요청을 보내지 않음
4. 클러스터가 영원히 warming 상태에 머무름

**해결**: Delta 프로토콜에서 CDS 요청 처리 후 `forceEDSPush()`를 호출하여 서버가 능동적으로 EDS를 전송한다.

### 11.7 왜 addCon을 initializeProxy보다 먼저 하는가?

**문제**: initializeProxy 후 addCon을 하면 다음과 같은 경쟁 조건이 발생한다:

```
시간  Thread A (새 커넥션)      Thread B (Config 변경)
T1    initializeProxy() 시작
T2                               ConfigUpdate() 호출
T3                               Push() → initPushContext()
T4                               StartPush() → 모든 클라이언트 순회
T5                               (새 커넥션은 아직 미등록 → 누락!)
T6    initializeProxy() 완료
T7    addCon() ← 이미 늦음
```

**해결**: `addCon()` 후 `initializeProxy()`를 하면 T4 시점에 새 커넥션이 이미 등록되어 있으므로 푸시 대상에 포함된다. 초기화가 완료되지 않은 상태에서 푸시를 받더라도 `InitializedCh()`로 대기하므로 문제가 없다.

---

## 요약

Istio의 xDS Discovery Service는 대규모 서비스 메시에서 수천 개의 Envoy 프록시에 실시간으로 구성을 전달하기 위해 정교한 엔지니어링을 적용한다. 핵심 설계 원칙은 다음과 같다:

1. **다층 버퍼링**: pushChannel(디바운싱 입력) -> debounce(병합) -> pushQueue(커넥션별 큐) -> PushCh(커넥션별 채널)
2. **선택적 계산**: 각 Generator가 자신에게 관련된 변경만 처리하고, 프록시 수준에서도 관련 업데이트만 필터링
3. **순서 보장**: CDS -> EDS -> LDS -> RDS의 의존성 순서를 엄격히 준수
4. **동시성 제어**: CPU 기반 휴리스틱으로 세마포어 크기를 자동 조정하여 과부하 방지
5. **캐싱**: EDS 엔드포인트 등 비용이 큰 계산 결과를 캐싱하고 변경 시에만 무효화
6. **안전한 기본값**: 새로운 config 종류는 기본적으로 모든 Generator가 반응 (opt-out 패턴)
