# 03. Hubble 시퀀스 다이어그램

## 개요

이 문서는 Hubble의 주요 유즈케이스를 Mermaid 시퀀스 다이어그램으로 표현한다.
실제 소스코드의 함수 호출 흐름을 기반으로 작성하였다.

---

## 1. hubble observe 요청 흐름

CLI에서 `hubble observe`를 실행하면 gRPC 스트리밍으로 Flow를 수신한다.

```mermaid
sequenceDiagram
    participant User as 사용자
    participant CLI as Hubble CLI
    participant Conn as conn.New()
    participant Relay as Hubble Relay
    participant Server as Observer Server

    User->>CLI: hubble observe --follow -n kube-system
    CLI->>CLI: observe.New(vp) 커맨드 실행
    CLI->>CLI: 플래그 파싱 (follow, since, last, filters)
    CLI->>Conn: conn.NewWithFlags(ctx, vp)
    Conn->>Conn: grpc.NewClient(server, dialOptions...)
    Conn-->>CLI: *grpc.ClientConn

    CLI->>CLI: FlowFilter 빌드 (source_pod, namespace 등)
    CLI->>Relay: GetFlows(GetFlowsRequest{follow:true, whitelist:[...]})

    loop 스트리밍 루프
        Relay->>Server: GetFlows(요청 전달, 각 노드)
        Server->>Server: Ring Buffer 읽기 + 필터 적용
        Server-->>Relay: GetFlowsResponse{Flow}
        Relay->>Relay: PriorityQueue 정렬
        Relay-->>CLI: GetFlowsResponse{Flow}
        CLI->>CLI: Printer.WriteProtoFlow(flow)
        CLI-->>User: 포맷된 Flow 출력
    end

    User->>CLI: Ctrl+C (취소)
    CLI->>CLI: context.Cancel()
    CLI-->>Relay: gRPC 스트림 종료
```

### 관련 소스

- CLI observe: `hubble/cmd/observe/observe.go`
- gRPC 연결: `hubble/cmd/common/conn/conn.go` (Line 114-122)
- GetFlows 서버: `cilium/pkg/hubble/observer/local_observer.go` (Line 260-428)

---

## 2. eBPF 이벤트 캡처 및 파싱

eBPF가 수집한 패킷 이벤트가 Observer를 거쳐 Ring Buffer에 저장되는 흐름.

```mermaid
sequenceDiagram
    participant eBPF as eBPF Datapath
    participant Perf as Perf Ring Buffer
    participant MA as MonitorAgent
    participant Obs as LocalObserverServer
    participant Parser as Parser
    participant L34 as threefour.Parser
    participant L7 as seven.Parser
    participant Hook as OnDecodedFlow Hooks
    participant Ring as Ring Buffer

    eBPF->>Perf: 패킷 이벤트 기록
    Perf->>MA: perf event 읽기
    MA->>MA: MonitorEvent{UUID, Timestamp, Payload:PerfEvent} 생성
    MA->>Obs: events 채널로 전송

    Obs->>Obs: OnMonitorEvent Hook 실행 (전처리)
    Obs->>Parser: Decode(monitorEvent)

    alt PerfEvent.Data[0] == MessageTypeDrop/Trace
        Parser->>L34: l34.Decode(data, flow)
        L34->>L34: IP/TCP/UDP 헤더 파싱
        L34->>L34: Endpoint 메타데이터 조회
        L34-->>Parser: flow (L3/L4 정보 채움)
    else PerfEvent.Data[0] == MessageTypeDebug
        Parser->>Parser: dbg.Decode(data, cpu)
        Parser-->>Obs: DebugEvent
    else PerfEvent.Data[0] == MessageTypeSock
        Parser->>Parser: sock.Decode(data, flow)
        Parser-->>Obs: Flow (SOCK 타입)
    else AgentEvent (MessageTypeAccessLog)
        Parser->>L7: l7.Decode(&logrecord, flow)
        L7->>L7: HTTP/DNS/Kafka 파싱
        L7-->>Parser: flow (L7 정보 채움)
    end

    Parser-->>Obs: v1.Event{Timestamp, Event: Flow}

    Obs->>Obs: trackNamespaces(flow)
    Obs->>Hook: OnDecodedFlow (메트릭 수집, 드롭 emitter)
    Note over Hook: stop=false이면 계속 진행
    Obs->>Hook: OnDecodedEvent (Flow Export, 로깅)
    Obs->>Ring: Write(event)
    Ring->>Ring: atomic write + notify readers
```

### 관련 소스

- Observer Start: `cilium/pkg/hubble/observer/local_observer.go` (Line 116-197)
- Parser Decode: `cilium/pkg/hubble/parser/parser.go` (Line 100-204)
- Ring Write: `cilium/pkg/hubble/container/ring.go` (Line 168-190)

---

## 3. GetFlows 서버 측 처리

Observer가 GetFlows 요청을 받아 Ring Buffer에서 Flow를 읽어 스트리밍하는 상세 흐름.

```mermaid
sequenceDiagram
    participant Client as gRPC Client
    participant GF as GetFlows()
    participant Filter as FilterBuilder
    participant Ring as Ring Buffer
    participant RR as RingReader
    participant ER as eventsReader
    participant FM as FieldMask

    Client->>GF: GetFlowsRequest{number:100, whitelist, blacklist}
    GF->>GF: validateRequest() (first+follow 동시 불가)
    GF->>GF: OnGetFlows Hook 실행

    GF->>Filter: BuildFilterList(whitelist, DefaultFilters)
    Filter->>Filter: 24개 필터 빌더 순회
    Filter-->>GF: whitelist FilterFuncs
    GF->>Filter: BuildFilterList(blacklist, DefaultFilters)
    Filter-->>GF: blacklist FilterFuncs

    GF->>Ring: LastWriteParallel()
    GF->>RR: newRingReader(ring, req, whitelist, blacklist)
    Note over RR: number=100이면 뒤에서 100개 되감기<br/>since가 있으면 시각 기준 되감기
    RR->>RR: Previous()로 역방향 탐색

    GF->>ER: newEventsReader(ringReader, req, whitelist, blacklist)
    GF->>FM: fieldmask.New(req.FieldMask)

    loop 이벤트 스트리밍
        GF->>ER: Next(ctx)
        ER->>RR: Next() 또는 NextFollow(ctx)
        RR->>Ring: read(idx) - atomic load
        Ring-->>RR: *v1.Event
        ER->>ER: 시간 범위 체크 (since/until)
        ER->>ER: filters.Apply(whitelist, blacklist, event)
        ER-->>GF: 필터 통과한 *v1.Event

        alt Flow 이벤트
            GF->>GF: OnFlowDelivery Hook
            GF->>FM: mask.Copy(flow) - 필드 마스크 적용
            GF->>Client: Send(GetFlowsResponse{Flow})
        else LostEvent
            GF->>GF: lostEventCounter 업데이트
            Note over GF: rate-limiting으로 주기적 전송
        end
    end

    alt EOF 또는 number 도달
        GF-->>Client: 스트림 종료
    else context 취소
        GF-->>Client: 스트림 종료
    end
```

### 관련 소스

- GetFlows: `cilium/pkg/hubble/observer/local_observer.go` (Line 260-428)
- eventsReader.Next: `cilium/pkg/hubble/observer/local_observer.go` (Line 648-701)
- newRingReader: `cilium/pkg/hubble/observer/local_observer.go` (Line 729-790)
- filters.Apply: `cilium/pkg/hubble/filters/filters.go` (Line 23-25)

---

## 4. Relay 다중노드 Flow 집계

Relay가 여러 노드에서 Flow를 수집하고, 타임스탬프 기반으로 정렬하여 클라이언트에 전달.

```mermaid
sequenceDiagram
    participant CLI as Hubble CLI
    participant Relay as Relay Observer
    participant FC as FlowCollector
    participant Node1 as Node1 Observer
    participant Node2 as Node2 Observer
    participant Node3 as Node3 Observer
    participant PQ as PriorityQueue
    participant AE as AggregateErrors

    CLI->>Relay: GetFlows(req{follow:true})
    Relay->>Relay: peers := s.peers.List()
    Relay->>Relay: qlen := sortBufferMaxLen

    Relay->>FC: newFlowCollector(req, opts)
    FC->>FC: collect(ctx, g, peers, flows)

    par 각 노드에 병렬 요청
        FC->>Node1: retrieveFlowsFromPeer(ctx, client, req, flows)
        Note over FC,Node1: goroutine으로 실행
        FC->>Node2: retrieveFlowsFromPeer(ctx, client, req, flows)
        FC->>Node3: retrieveFlowsFromPeer(ctx, client, req, flows)
    end

    Relay->>CLI: NodeStatus{CONNECTED, [node1,node2,node3]}
    Note over Relay: unavailable 노드가 있으면 별도 알림

    loop follow 모드에서 주기적 피어 갱신
        Relay->>Relay: time.After(peerUpdateInterval)
        Relay->>FC: collect(peers) - 새로운 노드 추가
    end

    loop Flow 수집 및 정렬
        Node1-->>FC: GetFlowsResponse (flows 채널)
        Node2-->>FC: GetFlowsResponse (flows 채널)
        Node3-->>FC: GetFlowsResponse (flows 채널)

        FC->>AE: aggregateErrors(flows)
        Note over AE: 같은 에러 메시지의 노드를 그룹핑<br/>errorAggregationWindow 내 병합

        AE->>PQ: sortFlows(aggregated, qlen, drainTimeout)
        PQ->>PQ: Push(flow)
        alt 큐가 가득 참
            PQ->>PQ: Pop() - 가장 오래된 Flow 방출
        end
        alt bufferDrainTimeout 경과
            PQ->>PQ: PopOlderThan(t) - 오래된 Flow 일괄 방출
        end

        PQ-->>Relay: 정렬된 Flow
        Relay->>CLI: Send(GetFlowsResponse)
    end

    Note over Node1: 노드 연결 끊어짐
    Node1-->>FC: error
    FC->>FC: delete(connectedNodes, "node1")
    FC-->>AE: NodeStatusEvent{NODE_ERROR, "node1", errMsg}
```

### 관련 소스

- Relay GetFlows: `cilium/pkg/hubble/relay/observer/server.go` (Line 62-125)
- flowCollector.collect: `cilium/pkg/hubble/relay/observer/observer.go` (Line 262-305)
- sortFlows: `cilium/pkg/hubble/relay/observer/observer.go` (Line 67-119)
- aggregateErrors: `cilium/pkg/hubble/relay/observer/observer.go` (Line 153-222)
- PriorityQueue: `cilium/pkg/hubble/relay/queue/priority_queue.go` (Line 19-105)

---

## 5. Peer Discovery (노드 발견)

PeerManager가 Peer 서비스를 통해 노드를 발견하고 gRPC 연결을 관리하는 흐름.

```mermaid
sequenceDiagram
    participant PM as PeerManager
    participant PS as Peer Service
    participant MC as manageConnections
    participant RS as reportConnectionStatus

    Note over PM: Start() - 3개 goroutine 시작

    par watchNotifications
        PM->>PS: peerClientBuilder.Client(peerServiceAddress)
        PS-->>PM: PeerClient
        PM->>PS: Notify(ctx, NotifyRequest{})
        PS-->>PM: stream ChangeNotification

        loop 알림 수신
            PS->>PM: ChangeNotification{name:"node1", type:PEER_ADDED, addr:"10.0.1.1:4244"}
            PM->>PM: upsert(peer{Name:"node1", Address:...})
            PM->>MC: updated 채널에 "node1" 전송

            PS->>PM: ChangeNotification{name:"node2", type:PEER_ADDED}
            PM->>PM: upsert(peer{Name:"node2", ...})
            PM->>MC: updated 채널에 "node2" 전송

            PS->>PM: ChangeNotification{name:"node1", type:PEER_UPDATED, addr:"10.0.1.2:4244"}
            PM->>PM: disconnect(old) + upsert(new)
            PM->>MC: updated 채널에 "node1" 전송

            PS->>PM: ChangeNotification{name:"node3", type:PEER_DELETED}
            PM->>PM: disconnect(peer) + delete(peers, "node3")
        end

        Note over PM,PS: 연결 끊어지면 retryTimeout 후 재연결
    and manageConnections
        MC->>MC: <-updated (새 피어 알림 수신)
        MC->>MC: connect(peer, ignoreBackoff:true)
        MC->>MC: clientConnBuilder.ClientConn(addr, tlsServerName)
        Note over MC: 연결 실패 시 backoff 증가

        MC->>MC: <-time.After(connCheckInterval)
        Note over MC: 주기적으로 모든 피어 연결 상태 점검
        MC->>MC: 각 피어에 대해 connect(peer, false)
    and reportConnectionStatus
        RS->>RS: <-time.After(connStatusInterval)
        RS->>RS: 각 피어 conn.GetState() 확인
        RS->>RS: metrics.ObservePeerConnectionStatus(states)
    end
```

### 관련 소스

- PeerManager: `cilium/pkg/hubble/relay/pool/manager.go` (Line 32-68)
- watchNotifications: `cilium/pkg/hubble/relay/pool/manager.go` (Line 87-163)
- manageConnections: `cilium/pkg/hubble/relay/pool/manager.go` (Line 166-193)
- connect: `cilium/pkg/hubble/relay/pool/manager.go` (Line 311-349)

---

## 6. Ring Buffer 읽기/쓰기

Lock-free Ring Buffer의 동시 읽기/쓰기 시퀀스.

```mermaid
sequenceDiagram
    participant Writer as Observer (Writer)
    participant Ring as Ring Buffer
    participant Reader1 as GetFlows Reader 1
    participant Reader2 as GetFlows Reader 2

    Note over Ring: capacity=7 (2^3-1), mask=0x7<br/>data[8], write=0

    Writer->>Ring: Write(event_A)
    Ring->>Ring: write.Add(1) -> write=1
    Ring->>Ring: writeIdx = (1-1) & 0x7 = 0
    Ring->>Ring: dataStoreAtomic(0, event_A)
    Ring->>Ring: notify sleeping readers (close notifyCh)

    Writer->>Ring: Write(event_B)
    Ring->>Ring: write.Add(1) -> write=2
    Ring->>Ring: writeIdx = (2-1) & 0x7 = 1
    Ring->>Ring: dataStoreAtomic(1, event_B)

    Reader1->>Ring: read(0) - event_A 읽기
    Ring->>Ring: readIdx = 0 & 0x7 = 0
    Ring->>Ring: event = dataLoadAtomic(0) -> event_A
    Ring->>Ring: readCycle==writeCycle && readIdx < lastWriteIdx
    Ring-->>Reader1: event_A (정상 읽기)

    Reader2->>Ring: readFrom(ctx, 2, ch) - follow 모드
    Ring->>Ring: readCycle == writeCycle && readIdx >= lastWriteIdx
    Ring->>Ring: reader가 writer를 따라잡음 - sleep
    Ring->>Ring: notifyMu.Lock() -> notifyCh 생성

    Writer->>Ring: Write(event_C)
    Ring->>Ring: write.Add(1) -> write=3
    Ring->>Ring: close(notifyCh) -> Reader2 깨움

    Reader2->>Ring: 깨어남, read(2) 재시도
    Ring-->>Reader2: event_C

    Note over Ring: 8회 이상 쓰기 후 (덮어쓰기 발생)
    Writer->>Ring: Write(event_I) - write=9, idx=0 (event_A 덮어쓰기)

    Reader1->>Ring: read(0) - event_A 다시 읽으려 시도
    Ring->>Ring: readCycle이 writeCycle보다 한참 뒤처짐
    Ring-->>Reader1: getLostEvent() (유실 알림)
```

### 관련 소스

- Ring.Write: `cilium/pkg/hubble/container/ring.go` (Line 168-190)
- Ring.read: `cilium/pkg/hubble/container/ring.go` (Line 240-293)
- Ring.readFrom: `cilium/pkg/hubble/container/ring.go` (Line 297-398)
- RingReader.NextFollow: `cilium/pkg/hubble/container/ring_reader.go` (Line 76-122)

---

## 7. 필터 빌드 및 적용

GetFlows 요청의 whitelist/blacklist 필터가 빌드되고 적용되는 흐름.

```mermaid
sequenceDiagram
    participant GF as GetFlows
    participant BFL as BuildFilterList
    participant BF as BuildFilter
    participant DF as DefaultFilters (24개)
    participant FF as FilterFunc
    participant Apply as Apply()

    GF->>BFL: BuildFilterList(req.Whitelist, defaultFilters)

    loop 각 FlowFilter
        BFL->>BF: BuildFilter(ctx, flowFilter, auxFilters)
        loop 24개 필터 빌더
            BF->>DF: OnBuildFilter(ctx, flowFilter)
            Note over DF: UUIDFilter: uuid 필드 검사
            Note over DF: IPFilter: source_ip/dest_ip CIDR 매칭
            Note over DF: PodFilter: source_pod/dest_pod prefix 매칭
            Note over DF: VerdictFilter: verdict enum 매칭
            Note over DF: ProtocolFilter: L4 프로토콜 매칭
            Note over DF: PortFilter: L4 포트 범위 매칭
            Note over DF: HTTPFilter: status_code/method/path 매칭
            Note over DF: CELExpressionFilter: CEL 표현식 평가
            DF-->>BF: []FilterFunc (해당 필드 설정 시)
        end
        BF-->>BFL: FilterFuncs (AND 결합)
    end
    BFL-->>GF: whitelist (각 FlowFilter는 OR 결합)

    Note over GF: blacklist도 동일하게 빌드

    loop 이벤트 필터링
        GF->>Apply: Apply(whitelist, blacklist, event)
        Apply->>Apply: whitelist.MatchOne(event)
        Note over Apply: 하나라도 매칭되면 true (OR)
        Apply->>Apply: blacklist.MatchNone(event)
        Note over Apply: 아무것도 매칭 안 되면 true (NOR)
        Apply-->>GF: true (포함) / false (제외)
    end
```

### 필터 적용 로직

```
whitelist = [filter_A, filter_B]   // OR
blacklist = [filter_C]             // NOR

filter_A = {source_pod: "nginx", verdict: DROPPED}  // AND
  -> source_pod이 "nginx"이고 verdict가 DROPPED인 Flow

filter_B = {destination_ip: "10.0.0.0/8"}
  -> destination_ip가 10.0.0.0/8 대역인 Flow

결과: (filter_A 매칭 OR filter_B 매칭) AND (filter_C 비매칭)
```

### 관련 소스

- DefaultFilters: `cilium/pkg/hubble/filters/filters.go` (Line 127-155)
- BuildFilterList: `cilium/pkg/hubble/filters/filters.go` (Line 105-124)
- Apply: `cilium/pkg/hubble/filters/filters.go` (Line 23-25)
- MatchOne: `cilium/pkg/hubble/filters/filters.go` (Line 39-50)
- MatchNone: `cilium/pkg/hubble/filters/filters.go` (Line 54-65)

---

## 8. Relay ServerStatus 집계

Relay가 모든 노드의 ServerStatus를 수집하여 통합 응답을 반환하는 흐름.

```mermaid
sequenceDiagram
    participant CLI as Hubble CLI
    participant Relay as Relay Server
    participant Node1 as Node 1
    participant Node2 as Node 2
    participant Node3 as Node 3 (Unavailable)

    CLI->>Relay: ServerStatus(ServerStatusRequest{})
    Relay->>Relay: peers := s.peers.List()

    Relay->>Node3: isAvailable(conn) -> false
    Relay->>Relay: numUnavailableNodes++, unavailableNodes=["node3"]

    par 연결된 노드에 병렬 요청
        Relay->>Node1: ServerStatus(req)
        Node1-->>Relay: {num_flows:5000, max_flows:65535, seen_flows:100000, uptime_ns:3600s, flows_rate:50.0}
        Relay->>Node2: ServerStatus(req)
        Node2-->>Relay: {num_flows:3000, max_flows:65535, seen_flows:80000, uptime_ns:7200s, flows_rate:30.0}
    end

    Relay->>Relay: 응답 집계
    Note over Relay: max_flows = 65535 + 65535 = 131070<br/>num_flows = 5000 + 3000 = 8000<br/>seen_flows = 100000 + 80000 = 180000<br/>uptime_ns = max(3600, 7200) = 7200 (가장 오래된 기준)<br/>flows_rate = 50.0 + 30.0 = 80.0<br/>num_connected = 2<br/>num_unavailable = 1

    Relay-->>CLI: ServerStatusResponse{<br/>  version: relay_version,<br/>  num_flows: 8000,<br/>  max_flows: 131070,<br/>  seen_flows: 180000,<br/>  uptime_ns: 7200s,<br/>  flows_rate: 80.0,<br/>  num_connected_nodes: 2,<br/>  num_unavailable_nodes: 1,<br/>  unavailable_nodes: ["node3"]<br/>}
```

### 관련 소스

- Relay ServerStatus: `cilium/pkg/hubble/relay/observer/server.go` (Line 247-336)

---

## 9. Hive Cell 초기화

Cilium Agent 시작 시 Hubble이 Hive Cell로 초기화되는 흐름.

```mermaid
sequenceDiagram
    participant Hive as Hive Framework
    participant Cell as hubblecell.Cell
    participant HI as HubbleIntegration
    participant Parser as Parser
    participant NS as NamespaceManager
    participant Observer as LocalObserverServer
    participant Server as gRPC Server
    participant Peer as PeerService

    Hive->>Cell: cell.Module("hubble") 등록
    Cell->>Cell: ConfigProviders 초기화
    Cell->>Cell: certloaderGroup (TLS 인증서)
    Cell->>Cell: parsercell.Cell (Parser 생성)
    Cell->>Cell: namespace.Cell (Manager 생성)
    Cell->>Cell: peercell.Cell (Peer 서비스 생성)
    Cell->>Cell: exportercell.Cell (Exporter 생성)
    Cell->>Cell: metricscell.Cell (메트릭 핸들러)

    Cell->>HI: newHubbleIntegration(params)
    Note over HI: params에 DI로 주입된 모든 의존성 포함:<br/>Parser, NamespaceManager, PeerService,<br/>MetricsFlowProcessor, ExporterBuilders 등

    HI->>HI: createHubbleIntegration(...)
    Hive->>HI: JobGroup.Add(OneShot("hubble"))

    Note over HI: Hive가 준비되면 Launch 실행

    HI->>Observer: NewLocalServer(parser, nsManager, options...)
    Observer->>Observer: Ring = NewRing(maxFlows)
    Observer->>Observer: events = make(chan *MonitorEvent, monitorBuffer)

    HI->>Server: NewServer(observer, peer, health, tls...)
    Server->>Server: RegisterObserverServer
    Server->>Server: RegisterPeerServer
    Server->>Server: RegisterHealthServer

    HI->>Observer: go Start() (이벤트 루프 시작)
    HI->>Server: go Serve() (gRPC 리스닝 시작)
    HI->>HI: MonitorAgent에 이벤트 수신 등록
```

### 관련 소스

- Cell 정의: `cilium/pkg/hubble/cell/cell.go` (Line 38-67)
- hubbleParams: `cilium/pkg/hubble/cell/cell.go` (Line 77-109)
- newHubbleIntegration: `cilium/pkg/hubble/cell/cell.go` (Line 116-146)
