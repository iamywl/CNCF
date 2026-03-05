# 05. Hubble 핵심 컴포넌트

## 개요

이 문서는 Hubble의 6대 핵심 컴포넌트의 동작 원리를 설명한다.
각 컴포넌트의 내부 구조, 알고리즘, 설계 의도를 소스코드 기반으로 분석한다.

---

## 1. LocalObserverServer

### 역할

LocalObserverServer는 Hubble의 심장이다. MonitorAgent에서 원시 이벤트를 수신하여 파싱하고, Ring Buffer에 저장하며, gRPC를 통해 클라이언트에 Flow를 제공한다.

### 핵심 구조체

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go (Line 44-70)
type LocalObserverServer struct {
    ring             *container.Ring                    // Flow 저장소
    events           chan *observerTypes.MonitorEvent   // 이벤트 수신 채널
    stopped          chan struct{}                      // 종료 시그널
    log              *slog.Logger
    payloadParser    parser.Decoder                    // MonitorEvent -> Flow
    opts             observeroption.Options             // Hook, 설정
    startTime        time.Time
    numObservedFlows atomic.Uint64                     // 관측 카운터
    nsManager        namespace.Manager                 // 네임스페이스 추적
}
```

### 생성

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go (Line 73-113)
func NewLocalServer(
    payloadParser parser.Decoder,
    nsManager namespace.Manager,
    logger *slog.Logger,
    options ...observeroption.Option,
) (*LocalObserverServer, error) {
    opts := observeroption.Default
    for _, opt := range options {
        opt(&opts)
    }
    s := &LocalObserverServer{
        ring:          container.NewRing(opts.MaxFlows),        // Ring Buffer 생성
        events:        make(chan *observerTypes.MonitorEvent, opts.MonitorBuffer), // 버퍼링된 채널
        payloadParser: payloadParser,
        // ...
    }
    // OnServerInit Hook 실행
    for _, f := range s.opts.OnServerInit {
        f.OnServerInit(s)
    }
    return s, nil
}
```

### 이벤트 루프 (Start)

`Start()`는 goroutine으로 실행되며, `events` 채널이 닫힐 때까지 계속 실행된다.

```
이벤트 루프 흐름:
  for monitorEvent := range events {
      1. OnMonitorEvent Hook (stop 가능)
      2. payloadParser.Decode(monitorEvent) -> v1.Event
      3. Flow인 경우:
         a. trackNamespaces(flow)
         b. OnDecodedFlow Hook (메트릭, 드롭 emitter)
         c. numObservedFlows.Add(1)
      4. OnDecodedEvent Hook (Export, 로깅)
      5. ring.Write(event)
  }
```

Hook 체인의 `stop` 반환값이 true이면 해당 이벤트의 나머지 처리를 건너뛴다.
이는 필터링, 샘플링, 또는 특정 조건에서의 조기 종료를 가능하게 한다.

### GetFlows 처리

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go (Line 260-428)
func (s *LocalObserverServer) GetFlows(req, server) error {
    // 1. 필터 빌드 (whitelist, blacklist)
    whitelist := filters.BuildFilterList(req.Whitelist, defaultFilters)
    blacklist := filters.BuildFilterList(req.Blacklist, defaultFilters)

    // 2. RingReader 생성 (시작 위치 계산)
    ringReader := newRingReader(ring, req, whitelist, blacklist)

    // 3. eventsReader 생성 (시간/수량 제한 관리)
    eventsReader := newEventsReader(ringReader, req, whitelist, blacklist)

    // 4. FieldMask 설정
    mask := fieldmask.New(req.FieldMask)

    // 5. LostEvent rate-limiter 설정
    lostEventCounter := counter.NewIntervalRangeCounter(lostEventSendInterval)

    // 6. 이벤트 읽기 루프
    for {
        e := eventsReader.Next(ctx)
        switch ev := e.Event.(type) {
        case *flowpb.Flow:
            // OnFlowDelivery Hook -> FieldMask 적용 -> server.Send
        case *flowpb.LostEvent:
            // rate-limit으로 주기적 전송
        }
    }
}
```

### eventsReader

eventsReader는 RingReader 위에 비즈니스 로직(필터, 시간 범위, 수량 제한)을 추가하는 어댑터이다.

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go (Line 590-605)
type eventsReader struct {
    ringReader           *container.RingReader
    whitelist, blacklist filters.FilterFuncs
    maxEvents            uint64
    follow, timeRange    bool
    since, until         *time.Time
    eventCount           uint64
}
```

`Next()` 메서드의 핵심 로직:

```
for {
    1. follow 모드: ringReader.NextFollow(ctx) -- 새 이벤트 대기
       non-follow: ringReader.Next() -- EOF 시 반환
    2. maxEvents 초과 시 io.EOF 반환
    3. LostEvent는 필터/시간 무시하고 항상 전달
    4. 시간 범위 체크 (until 초과 -> EOF, since 미만 -> skip)
    5. filters.Apply(whitelist, blacklist, event)
    6. 통과하면 반환
}
```

### newRingReader (시작 위치 계산)

```
// 소스: cilium/pkg/hubble/observer/local_observer.go (Line 729-790)
시작 위치 결정 로직:
  1. first==true && since==nil -> ring.OldestWrite() (맨 처음부터)
  2. follow && number==0 && since==nil -> ring.LastWriteParallel() (현재부터)
  3. 그 외: 뒤에서부터 역방향 탐색
     - since가 있으면: since보다 오래된 이벤트까지 되감기
     - number가 있으면: number개 이벤트만큼 되감기
     - 되감기 중 LostEvent(HUBBLE_RING_BUFFER) 만나면 중단
```

---

## 2. Parser (이벤트 파서)

### 역할

Parser는 MonitorEvent의 원시 바이트를 구조화된 v1.Event(Flow/AgentEvent/DebugEvent)로 변환한다.

### 구조

```go
// 소스: cilium/pkg/hubble/parser/parser.go (Line 37-42)
type Parser struct {
    l34  *threefour.Parser  // L3/L4 파서 (IP, TCP, UDP, ICMP)
    l7   *seven.Parser      // L7 파서 (HTTP, DNS, Kafka)
    dbg  *debug.Parser      // Debug 이벤트 파서
    sock *sock.Parser       // Socket Trace 파서
}
```

### Decoder 인터페이스

```go
// 소스: cilium/pkg/hubble/parser/parser.go (Line 31-34)
type Decoder interface {
    Decode(monitorEvent *observerTypes.MonitorEvent) (*v1.Event, error)
}
```

### Decode 분기 로직

```go
// 소스: cilium/pkg/hubble/parser/parser.go (Line 100-204)
func (p *Parser) Decode(monitorEvent *observerTypes.MonitorEvent) (*v1.Event, error) {
    ev := &v1.Event{Timestamp: timestamppb.New(monitorEvent.Timestamp)}

    switch payload := monitorEvent.Payload.(type) {
    case *observerTypes.PerfEvent:
        // eBPF perf ring buffer에서 온 이벤트
        flow := &pb.Flow{Emitter: &pb.Emitter{Name: "Hubble"}, Uuid: uuid}
        switch payload.Data[0] {
        case monitorAPI.MessageTypeDebug:
            ev.Event = p.dbg.Decode(payload.Data, payload.CPU)
        case monitorAPI.MessageTypeTraceSock:
            p.sock.Decode(payload.Data, flow)
            ev.Event = flow
        default:  // MessageTypeDrop, MessageTypeTrace, etc.
            p.l34.Decode(payload.Data, flow)
            ev.Event = flow
        }

    case *observerTypes.AgentEvent:
        switch payload.Type {
        case monitorAPI.MessageTypeAccessLog:
            // L7 프록시 로그 -> HTTP/DNS/Kafka Flow
            p.l7.Decode(&logrecord, flow)
            ev.Event = flow
        case monitorAPI.MessageTypeAgent:
            // 에이전트 알림 -> AgentEvent
            ev.Event = agent.NotifyMessageToProto(msg)
        }

    case *observerTypes.LostEvent:
        ev.Event = &pb.LostEvent{Source: ..., NumEventsLost: ...}
    }
    return ev, nil
}
```

### 파서별 역할

| 파서 | 입력 | 출력 | 처리 내용 |
|------|------|------|----------|
| `threefour` | PerfEvent.Data | Flow | IP 헤더, TCP/UDP/ICMP 파싱, Endpoint/Identity/Service 메타데이터 |
| `seven` | LogRecord | Flow | HTTP method/code/url, DNS query/rcode, Kafka topic |
| `debug` | PerfEvent.Data | DebugEvent | 디버그 타입, source endpoint, hash, args |
| `sock` | PerfEvent.Data | Flow (SOCK) | Socket trace, cgroup ID, cookie |
| `agent` | AgentNotifyMessage | AgentEvent | 정책 변경, 엔드포인트 이벤트, IPCache 변경 |

### Parser 생성 시 주입되는 Getter

```go
// 소스: cilium/pkg/hubble/parser/parser.go (Line 45-55)
func New(log, endpointGetter, identityGetter, dnsGetter,
         ipGetter, serviceGetter, linkGetter, cgroupGetter, opts...) (*Parser, error)
```

| Getter | 용도 |
|--------|------|
| EndpointGetter | Cilium 엔드포인트 ID -> Pod/Namespace/Labels 조회 |
| IdentityGetter | Security Identity -> 라벨 셋 조회 |
| DNSGetter | IP -> DNS 이름 역방향 조회 |
| IPGetter | IP -> Identity, Endpoint 매핑 |
| ServiceGetter | IP:Port -> Service Name/Namespace 조회 |
| LinkGetter | Interface Index -> Interface Name 조회 |
| PodMetadataGetter | cgroup ID -> Pod 메타데이터 조회 |

이 Getter들이 원시 패킷 데이터에 Cilium의 풍부한 메타데이터를 부여하는 핵심이다.

---

## 3. Ring Buffer

### 역할

Ring Buffer는 Hubble의 Flow 저장소이다. Lock-free 설계로 단일 writer와 다중 reader의 동시 접근을 지원한다.

### 핵심 구조

```go
// 소스: cilium/pkg/hubble/container/ring.go (Line 86-110)
type Ring struct {
    mask      uint64           // 인덱스 마스킹 (data 배열 크기 - 1)
    write     atomic.Uint64    // 다음 쓰기 위치 (monotonically increasing)
    cycleExp  uint8            // 사이클 비트 시프트 (log2(dataLen))
    cycleMask uint64           // 사이클 마스크
    halfCycle uint64           // 반 사이클 (reader가 뒤처졌는지 판단)
    dataLen   uint64           // 내부 배열 길이 (2^n)
    data      []*v1.Event      // 실제 데이터 배열
    notifyMu  lock.Mutex       // reader 알림용
    notifyCh  chan struct{}     // reader 깨우기 채널
}
```

### 용량 (Capacity)

용량은 `2^n - 1` 형태만 허용된다. 1개 슬롯은 쓰기용으로 예약되어 읽을 수 없다.

```go
// 소스: cilium/pkg/hubble/container/ring.go (Line 47-64)
const (
    Capacity1     capacity = 1      // 2^1 - 1
    Capacity3     capacity = 3      // 2^2 - 1
    Capacity7     capacity = 7      // 2^3 - 1
    // ...
    Capacity4095  capacity = 4095   // 2^12 - 1
    Capacity65535 capacity = 65535  // 2^16 - 1
)
```

### Write (쓰기)

```go
// 소스: cilium/pkg/hubble/container/ring.go (Line 168-190)
func (r *Ring) Write(entry *v1.Event) {
    r.notifyMu.Lock()

    write := r.write.Add(1)          // atomic increment
    writeIdx := (write - 1) & r.mask  // 실제 배열 인덱스
    r.dataStoreAtomic(writeIdx, entry) // atomic store

    // 대기 중인 reader 깨우기
    if r.notifyCh != nil {
        close(r.notifyCh)
        r.notifyCh = nil
    }

    r.notifyMu.Unlock()
}
```

**왜 notifyMu를 잠그는가?** `write` 갱신과 `notifyCh` 닫기 사이에 reader가 sleep에 들어가는 race condition을 방지하기 위해서이다. 실제 데이터 쓰기는 atomic이므로 lock의 영향이 최소화된다.

### Read (읽기) - 사이클 기반 유효성 검사

```go
// 소스: cilium/pkg/hubble/container/ring.go (Line 240-293)
func (r *Ring) read(read uint64) (*v1.Event, error) {
    readIdx := read & r.mask
    event := r.dataLoadAtomic(readIdx)

    lastWrite := r.write.Load() - 1
    lastWriteIdx := lastWrite & r.mask

    readCycle := read >> r.cycleExp
    writeCycle := lastWrite >> r.cycleExp

    switch {
    case readCycle == writeCycle && readIdx < lastWriteIdx:
        return event, nil         // 같은 사이클, 유효 범위 내
    case readCycle == prevWriteCycle && readIdx > lastWriteIdx:
        return event, nil         // 이전 사이클, 아직 덮어써지지 않음
    case readCycle >= writeCycle && readCycle < maxWriteCycle:
        return nil, io.EOF        // reader가 writer보다 앞서 있음
    default:
        return getLostEvent(), nil // writer가 reader를 추월함 (데이터 유실)
    }
}
```

### 사이클 감지 시각화

```
용량 7 (mask=0x7, dataLen=8, cycleExp=3):

write 값:  0  1  2  3  4  5  6  7  8  9  A  B  C  D  E  F  10 11 ...
data idx:  0  1  2  3  4  5  6  7  0  1  2  3  4  5  6  7  0  1  ...
cycle:     0  0  0  0  0  0  0  0  1  1  1  1  1  1  1  1  2  2  ...

write=0x0F (cycle=1, idx=7), read=0x05 (cycle=0, idx=5):
  readCycle(0) == prevWriteCycle(0) && readIdx(5) > lastWriteIdx(7-1=6)?
  NO -> 5 > 6 is false -> default -> LostEvent!

write=0x0F, read=0x0C (cycle=1, idx=4):
  readCycle(1) == writeCycle(1) && readIdx(4) < lastWriteIdx(6)?
  YES -> 유효한 읽기
```

### readFrom (Follow 모드)

Follow 모드에서 reader는 writer를 따라잡으면 sleep한다.

```
// 소스: cilium/pkg/hubble/container/ring.go (Line 297-398)
readFrom 로직:
  for read := startIdx; ; read++ {
      event := dataLoadAtomic(readIdx)
      switch {
      case 이전 사이클, 유효 범위:
          ch <- event
      case 같은 사이클, writer보다 뒤:
          ch <- event
      case 같은 사이클, writer 따라잡음:
          // sleep: notifyMu.Lock -> 마지막 확인 -> notifyCh 대기
          // Write()가 notifyCh를 close하면 깨어남
      case writer가 reader 추월:
          ch <- getLostEvent()  // 유실 알림
      }
  }
```

---

## 4. Filter Chain (필터 엔진)

### 역할

Filter Chain은 GetFlows 요청의 whitelist/blacklist를 기반으로 Flow를 걸러낸다.

### 핵심 타입

```go
// 소스: cilium/pkg/hubble/filters/filters.go (Line 16-19)
type FilterFunc func(ev *v1.Event) bool    // 단일 필터 함수
type FilterFuncs []FilterFunc              // 필터 함수 목록
```

### Apply 함수

```go
// 소스: cilium/pkg/hubble/filters/filters.go (Line 23-25)
func Apply(whitelist, blacklist FilterFuncs, ev *v1.Event) bool {
    return whitelist.MatchOne(ev) && blacklist.MatchNone(ev)
}
```

- `MatchOne`: 하나라도 매칭되면 true (OR). 빈 목록이면 true.
- `MatchNone`: 아무것도 매칭 안 되면 true (NOR). 빈 목록이면 true.

### 24개 기본 필터

```go
// 소스: cilium/pkg/hubble/filters/filters.go (Line 127-155)
func DefaultFilters(log *slog.Logger) []OnBuildFilter {
    return []OnBuildFilter{
        &UUIDFilter{},                // uuid로 특정 Flow 검색
        &EventTypeFilter{},           // Cilium 이벤트 타입/서브타입
        &VerdictFilter{},             // FORWARDED, DROPPED, ERROR 등
        &DropReasonDescFilter{},      // 드롭 사유
        &ReplyFilter{},               // 응답 패킷 여부
        &EncryptedFilter{},           // 암호화 여부
        &IdentityFilter{},            // 보안 Identity
        &ProtocolFilter{},            // TCP, UDP, HTTP 등 프로토콜
        &IPFilter{},                  // 소스/목적지 IP (CIDR 지원)
        &PodFilter{},                 // 소스/목적지 Pod 이름/네임스페이스
        &WorkloadFilter{},            // 워크로드 (Deployment, ReplicaSet 등)
        &ServiceFilter{},             // 서비스 이름/네임스페이스
        &FQDNFilter{},                // DNS 이름 매칭
        &LabelsFilter{},              // 라벨 셀렉터
        &PortFilter{},                // L4 포트 번호/범위
        &HTTPFilter{},                // HTTP method, status, path, url, header
        &TCPFilter{},                 // TCP 플래그 (SYN, FIN, RST 등)
        &NodeNameFilter{},            // 노드 이름 (와일드카드)
        &ClusterNameFilter{},         // 클러스터 이름
        &IPVersionFilter{},           // IPv4 / IPv6
        &TraceIDFilter{},             // 분산 추적 ID
        &TrafficDirectionFilter{},    // INGRESS / EGRESS
        &CELExpressionFilter{},       // CEL 표현식 (실험적)
        &NetworkInterfaceFilter{},    // 네트워크 인터페이스
        &IPTraceIDFilter{},           // IP 옵션 기반 추적 ID
    }
}
```

### 필터 빌드 과정

```go
// 소스: cilium/pkg/hubble/filters/filters.go (Line 84-124)
// 1. 각 FlowFilter에 대해 24개 빌더를 순회
func BuildFilter(ctx, ff *FlowFilter, auxFilters) (FilterFuncs, error) {
    for _, f := range auxFilters {
        fl, _ := f.OnBuildFilter(ctx, ff)
        // 해당 필드가 설정된 경우에만 FilterFunc 반환
        fs = append(fs, fl...)
    }
    return fs, nil  // AND로 결합될 FilterFuncs
}

// 2. 여러 FlowFilter를 OR로 결합
func BuildFilterList(ctx, ff []*FlowFilter, auxFilters) (FilterFuncs, error) {
    for _, flowFilter := range ff {
        tf := BuildFilter(ctx, flowFilter, auxFilters)
        // 각 FlowFilter의 모든 조건을 AND로 묶은 하나의 FilterFunc
        filterFunc := func(ev) bool { return tf.MatchAll(ev) }
        filterList = append(filterList, filterFunc)
    }
    return filterList, nil  // 최종: OR로 결합
}
```

### 필터 적용 예시

```
요청: GetFlows(whitelist=[
    {source_pod: "nginx", verdict: DROPPED},     // A: nginx에서 드롭
    {destination_ip: "10.0.0.0/8"}               // B: 10.x 대역 목적지
], blacklist=[
    {protocol: "ICMP"}                            // C: ICMP 제외
])

빌드 결과:
  whitelist = [
      func(ev) { return podMatch("nginx", ev) && verdictMatch(DROPPED, ev) },  // A
      func(ev) { return ipMatch("10.0.0.0/8", ev) },                           // B
  ]
  blacklist = [
      func(ev) { return protocolMatch("ICMP", ev) },                           // C
  ]

Apply:
  (A 매칭 OR B 매칭) AND (C 비매칭)
  = (nginx에서 드롭 이벤트 OR 10.x 대역 목적지) AND (ICMP 아님)
```

---

## 5. Relay Observer (다중노드 집계)

### 역할

Relay Observer는 클러스터의 모든 노드에서 Flow를 수집하고, 타임스탬프 기반으로 정렬하여 클라이언트에 전달한다.

### Server 구조

```go
// 소스: cilium/pkg/hubble/relay/observer/server.go (Line 41-44)
type Server struct {
    opts  options
    peers PeerLister  // PeerManager가 구현
}
```

### GetFlows 처리 흐름

```go
// 소스: cilium/pkg/hubble/relay/observer/server.go (Line 62-125)
func (s *Server) GetFlows(req, stream) error {
    peers := s.peers.List()
    qlen := s.opts.sortBufferMaxLen

    // 1. Flow 수집기 생성
    fc := newFlowCollector(req, s.opts)

    // 2. 각 피어에서 Flow 수집 (goroutine)
    connectedNodes, unavailableNodes := fc.collect(ctx, g, peers, flows)

    // 3. follow 모드: 주기적으로 새 피어 추가
    if req.GetFollow() {
        go func() {
            for {
                <-time.After(peerUpdateInterval)
                fc.collect(peers) // 새 노드 연결
            }
        }()
    }

    // 4. 에러 집계 (같은 에러 메시지의 노드 그룹핑)
    aggregated := aggregateErrors(ctx, flows, errorAggregationWindow)

    // 5. 타임스탬프 정렬
    sortedFlows := sortFlows(ctx, aggregated, qlen, sortBufferDrainTimeout)

    // 6. 노드 상태 알림
    stream.Send(nodeStatusEvent(CONNECTED, connectedNodes...))
    stream.Send(nodeStatusEvent(UNAVAILABLE, unavailableNodes...))

    // 7. 정렬된 Flow 전송
    sendFlowsResponse(ctx, stream, sortedFlows)
}
```

### FlowCollector

```go
// 소스: cilium/pkg/hubble/relay/observer/observer.go (Line 252-305)
type flowCollector struct {
    log            *slog.Logger
    ocb            observerClientBuilder
    req            *observerpb.GetFlowsRequest
    mu             lock.Mutex
    connectedNodes map[string]struct{}  // 이미 연결된 노드 추적
}

func (fc *flowCollector) collect(ctx, g, peers, flows) (connected, unavailable) {
    for _, p := range peers {
        if _, ok := fc.connectedNodes[p.Name]; ok {
            continue  // 이미 연결됨
        }
        if !isAvailable(p.Conn) {
            unavailable = append(unavailable, p.Name)
            continue
        }
        fc.connectedNodes[p.Name] = struct{}{}
        g.Go(func() error {
            err := retrieveFlowsFromPeer(ctx, client, req, flows)
            if err != nil {
                delete(fc.connectedNodes, p.Name) // 재연결 허용
                flows <- nodeStatusError(err, p.Name)
            }
            return nil
        })
    }
}
```

### sortFlows (PriorityQueue)

```go
// 소스: cilium/pkg/hubble/relay/observer/observer.go (Line 67-119)
func sortFlows(ctx, flows <-chan, qlen, drainTimeout) <-chan {
    pq := queue.NewPriorityQueue(qlen)

    go func() {
        for {
            select {
            case flow := <-flows:
                if pq.Len() == qlen {
                    sortedFlows <- pq.Pop()  // 가장 오래된 것 방출
                }
                pq.Push(flow)
            case t := <-time.After(drainTimeout):
                // 오래된 Flow 일괄 방출 (정렬 윈도우)
                for _, f := range pq.PopOlderThan(t.Add(-drainTimeout)) {
                    sortedFlows <- f
                }
            }
        }
        // 종료 시 큐 전부 drain
        for f := pq.Pop(); f != nil; f = pq.Pop() {
            sortedFlows <- f
        }
    }()
}
```

### PriorityQueue 구현

```go
// 소스: cilium/pkg/hubble/relay/queue/priority_queue.go (Line 19-105)
type PriorityQueue struct {
    h minHeap  // container/heap 기반
}

type minHeap []*observerpb.GetFlowsResponse

func (h minHeap) Less(i, j int) bool {
    // 타임스탬프 기준 오름차순 (오래된 것이 높은 우선순위)
    if h[i].GetTime().GetSeconds() == h[j].GetTime().GetSeconds() {
        return h[i].GetTime().GetNanos() < h[j].GetTime().GetNanos()
    }
    return h[i].GetTime().GetSeconds() < h[j].GetTime().GetSeconds()
}
```

### aggregateErrors

동일한 에러 메시지를 가진 노드 상태 이벤트를 시간 윈도우 내에서 병합한다.

```
// 소스: cilium/pkg/hubble/relay/observer/observer.go (Line 153-222)
aggregateErrors 로직:
  - 비에러 응답: 즉시 전달
  - 에러 응답:
    - pending이 있고 같은 메시지면: nodeNames에 추가 (병합)
    - pending이 있고 다른 메시지면: pending 전송, 새 pending 설정
    - errorAggregationWindow 경과 시: pending 전송
```

---

## 6. PeerManager (피어 관리)

### 역할

PeerManager는 클러스터 내 Hubble 노드를 발견하고, 각 노드에 대한 gRPC 연결을 관리한다.

### 구조

```go
// 소스: cilium/pkg/hubble/relay/pool/manager.go (Line 33-42)
type PeerManager struct {
    opts                 options
    updated              chan string         // 연결 요청 채널
    wg                   sync.WaitGroup
    stop                 chan struct{}       // 종료 시그널
    peerServiceConnected atomic.Bool
    mu                   lock.RWMutex
    peers                map[string]*peer    // 피어 맵
    metrics              *PoolMetrics
}

type peer struct {
    mu              lock.Mutex
    peerTypes.Peer                          // Name, Address, TLS 정보
    conn            poolTypes.ClientConn    // gRPC 연결
    connAttempts    int                     // 연결 시도 횟수 (backoff용)
    nextConnAttempt time.Time              // 다음 연결 시도 시각
}
```

### 3개의 goroutine

PeerManager는 `Start()`에서 3개의 goroutine을 시작한다.

```go
// 소스: cilium/pkg/hubble/relay/pool/manager.go (Line 71-85)
func (m *PeerManager) Start() {
    m.wg.Add(3)
    go m.watchNotifications()     // 1. 피어 변경 감시
    go m.manageConnections()      // 2. 연결 관리
    go m.reportConnectionStatus() // 3. 연결 상태 보고
}
```

#### 1. watchNotifications

Peer gRPC 서비스에 연결하여 ChangeNotification 스트림을 수신한다.

```
// 소스: cilium/pkg/hubble/relay/pool/manager.go (Line 87-163)
watchNotifications 로직:
  connect:
  for {
      // Peer 서비스 클라이언트 생성
      cl := peerClientBuilder.Client(peerServiceAddress)
      // Notify 스트림 시작
      client := cl.Notify(ctx, NotifyRequest{})
      peerServiceConnected = true

      for {
          cn := client.Recv()  // ChangeNotification 수신
          switch cn.Type {
          case PEER_ADDED:   m.upsert(peer)
          case PEER_DELETED: m.remove(peer)
          case PEER_UPDATED: m.upsert(peer)  // 기존 연결 끊고 재연결
          }
      }
      // 연결 끊어지면 retryTimeout 후 재연결
  }
```

#### 2. manageConnections

피어 추가/갱신 알림을 받거나, 주기적으로 연결 상태를 점검한다.

```
// 소스: cilium/pkg/hubble/relay/pool/manager.go (Line 166-193)
manageConnections 로직:
  for {
      select {
      case name := <-m.updated:
          // 새 피어 추가됨 -> 즉시 연결 시도 (ignoreBackoff=true)
          go m.connect(peers[name], true)
      case <-time.After(connCheckInterval):
          // 주기적 점검 -> 모든 피어 연결 확인
          for _, p := range m.peers {
              go m.connect(p, false)
          }
      }
  }
```

#### 3. connect (backoff 로직)

```go
// 소스: cilium/pkg/hubble/relay/pool/manager.go (Line 311-349)
func (m *PeerManager) connect(p *peer, ignoreBackoff bool) {
    // 이미 연결되어 있으면 skip
    if p.conn != nil && p.conn.GetState() != connectivity.Shutdown {
        return
    }
    // backoff 시간이 지나지 않았으면 skip (ignoreBackoff 제외)
    if p.nextConnAttempt.After(now) && !ignoreBackoff {
        return
    }
    // gRPC 연결 시도
    conn, err := clientConnBuilder.ClientConn(addr, tlsServerName)
    if err != nil {
        // 실패: backoff 증가
        duration := backoff.Duration(p.connAttempts)
        p.nextConnAttempt = now.Add(duration)
        p.connAttempts++
        return
    }
    // 성공: 카운터 초기화
    p.connAttempts = 0
    p.conn = conn
}
```

### List (피어 목록)

Relay Observer가 GetFlows에서 호출하는 인터페이스이다.

```go
// 소스: cilium/pkg/hubble/relay/pool/manager.go (Line 228-250)
func (m *PeerManager) List() []poolTypes.Peer {
    m.mu.RLock()
    defer m.mu.RUnlock()
    peers := make([]poolTypes.Peer, 0, len(m.peers))
    for _, v := range m.peers {
        peers = append(peers, poolTypes.Peer{
            Peer: peerTypes.Peer{Name, Address, TLS...},
            Conn: v.conn,
        })
    }
    return peers
}
```

---

## 컴포넌트 간 관계 요약

```
MonitorAgent
    |
    | (MonitorEvent 채널)
    v
LocalObserverServer -----> Parser (L3/L4, L7, Debug, Sock)
    |                         |
    |                         v
    |                    v1.Event
    |                         |
    v                         v
Ring Buffer <------------- Write
    |
    | (read/readFrom)
    v
RingReader --> eventsReader --> Filter Chain (Apply)
    |                             |
    v                             v
GetFlows Response <----------- 필터 통과

PeerManager
    |
    | (List)
    v
Relay Observer --> 각 노드 GetFlows --> flows 채널
    |                                      |
    v                                      v
aggregateErrors --> sortFlows (PriorityQueue) --> 클라이언트
```
