# 09. Hubble 파서 시스템 (Parser System)

## 목차
1. [개요](#1-개요)
2. [파서 아키텍처](#2-파서-아키텍처)
3. [최상위 Parser (Decoder)](#3-최상위-parser-decoder)
4. [L3/L4 파서 (threefour)](#4-l3l4-파서-threefour)
5. [L7 파서 (seven)](#5-l7-파서-seven)
6. [Debug 파서](#6-debug-파서)
7. [Sock 파서](#7-sock-파서)
8. [gopacket 기반 패킷 디코딩](#8-gopacket-기반-패킷-디코딩)
9. [Endpoint/Identity 리졸버](#9-endpointidentity-리졸버)
10. [Getter 인터페이스](#10-getter-인터페이스)
11. [LRU 캐시 시스템](#11-lru-캐시-시스템)
12. [Verdict와 TrafficDirection 결정](#12-verdict와-trafficdirection-결정)
13. [오버레이 네트워크 처리](#13-오버레이-네트워크-처리)
14. [파서 옵션 시스템](#14-파서-옵션-시스템)

---

## 1. 개요

Hubble 파서 시스템은 Cilium의 eBPF 데이터패스에서 생성된 원시 모니터 이벤트를
구조화된 Hubble Flow 객체로 변환하는 핵심 파이프라인이다. 파서는 계층적 구조로
설계되어, 각 네트워크 레이어(L3/L4, L7)와 이벤트 유형(debug, sock)을 전문적으로
처리한다.

```
  eBPF 데이터패스
       │
       v
  ┌────────────────┐
  │ MonitorEvent    │     모니터 에이전트로부터 수신
  │ (raw bytes)     │
  └───────┬────────┘
          │
          v
  ┌────────────────┐
  │ Parser.Decode() │     최상위 디스패처
  │ (parser.go)     │
  └───┬──┬──┬──┬───┘
      │  │  │  │
      v  v  v  v
  ┌──┐┌──┐┌──┐┌──┐
  │L34││L7││db││sk│    전문 파서들
  └──┘└──┘└──┘└──┘
      │  │  │  │
      v  v  v  v
  ┌────────────────┐
  │ *pb.Flow       │     구조화된 Flow protobuf
  │ *pb.DebugEvent │
  └────────────────┘
```

소스 경로:
- 최상위 파서: `cilium/pkg/hubble/parser/parser.go`
- L3/L4 파서: `cilium/pkg/hubble/parser/threefour/parser.go`
- L7 파서: `cilium/pkg/hubble/parser/seven/parser.go`
- Getter 인터페이스: `cilium/pkg/hubble/parser/getters/getters.go`

---

## 2. 파서 아키텍처

### 2.1 Parser 구조체

소스 경로: `cilium/pkg/hubble/parser/parser.go`

```go
// Parser는 모든 플로우의 파서이다
type Parser struct {
    l34  *threefour.Parser  // L3/L4 (네트워크/전송 계층) 파서
    l7   *seven.Parser      // L7 (애플리케이션 계층) 파서
    dbg  *debug.Parser      // 디버그 이벤트 파서
    sock *sock.Parser       // 소켓 이벤트 파서
}
```

각 서브 파서의 역할:

| 파서 | 패키지 | 대상 이벤트 | 입력 |
|------|--------|-----------|------|
| L3/L4 (threefour) | `parser/threefour` | Drop, Trace, PolicyVerdict, Capture | `PerfEvent` 바이트 |
| L7 (seven) | `parser/seven` | HTTP, DNS, Kafka 등 L7 로그 | `AgentEvent` (AccessLog) |
| Debug | `parser/debug` | 디버그 메시지 | `PerfEvent` (MessageTypeDebug) |
| Sock | `parser/sock` | 소켓 레벨 추적 (TraceSock) | `PerfEvent` (MessageTypeTraceSock) |

### 2.2 Parser 생성

```go
func New(
    log *slog.Logger,
    endpointGetter getters.EndpointGetter,   // 엔드포인트 조회
    identityGetter getters.IdentityGetter,   // 보안 ID 조회
    dnsGetter      getters.DNSGetter,        // DNS 이름 조회
    ipGetter       getters.IPGetter,         // IP 메타데이터 조회
    serviceGetter  getters.ServiceGetter,    // 서비스 조회
    linkGetter     getters.LinkGetter,       // 네트워크 인터페이스 조회
    cgroupGetter   getters.PodMetadataGetter,// cgroup→Pod 매핑
    opts ...options.Option,
) (*Parser, error) {
    // 각 서브 파서에 필요한 Getter만 전달
    l34, _ := threefour.New(log, endpointGetter, identityGetter,
                             dnsGetter, ipGetter, serviceGetter, linkGetter, opts...)
    l7, _ := seven.New(log, dnsGetter, ipGetter, serviceGetter,
                        endpointGetter, opts...)
    dbg, _ := debug.New(log, endpointGetter, opts...)
    sock, _ := sock.New(log, endpointGetter, identityGetter,
                         dnsGetter, ipGetter, serviceGetter, cgroupGetter, opts...)

    return &Parser{l34: l34, l7: l7, dbg: dbg, sock: sock}, nil
}
```

핵심 설계: **의존성 주입(Dependency Injection)** 패턴을 사용한다. 각 Getter 인터페이스는
Cilium 에이전트의 내부 상태(엔드포인트 맵, IP 캐시, DNS 캐시 등)에 대한 읽기 전용
접근을 제공한다. 파서 자체는 상태를 가지지 않으며, 모든 컨텍스트 정보를 Getter를
통해 조회한다.

---

## 3. 최상위 Parser (Decoder)

### 3.1 Decoder 인터페이스

```go
// Decoder는 파서의 인터페이스이다.
// 모니터 이벤트를 Hubble 이벤트로 디코딩한다.
type Decoder interface {
    Decode(monitorEvent *observerTypes.MonitorEvent) (*v1.Event, error)
}
```

### 3.2 Decode 분기 로직

`Parser.Decode()`는 `MonitorEvent`의 `Payload` 타입에 따라 적절한 서브 파서에
작업을 위임한다:

```go
func (p *Parser) Decode(monitorEvent *observerTypes.MonitorEvent) (*v1.Event, error) {
    ts := timestamppb.New(monitorEvent.Timestamp)
    ev := &v1.Event{Timestamp: ts}

    switch payload := monitorEvent.Payload.(type) {
    case *observerTypes.PerfEvent:
        // PerfEvent: eBPF perf 링 버퍼에서 온 이벤트
        flow := &pb.Flow{...}

        switch payload.Data[0] {
        case monitorAPI.MessageTypeDebug:
            // Debug 이벤트 → debug 파서
            dbg, _ := p.dbg.Decode(payload.Data, payload.CPU)
            ev.Event = dbg
            return ev, nil

        case monitorAPI.MessageTypeTraceSock:
            // 소켓 추적 이벤트 → sock 파서
            p.sock.Decode(payload.Data, flow)

        default:
            // Drop, Trace, PolicyVerdict, Capture → L3/L4 파서
            p.l34.Decode(payload.Data, flow)
        }
        flow.Time = ts
        flow.NodeName = monitorEvent.NodeName
        ev.Event = flow

    case *observerTypes.AgentEvent:
        switch payload.Type {
        case monitorAPI.MessageTypeAccessLog:
            // L7 액세스 로그 → L7 파서
            logrecord := payload.Message.(accesslog.LogRecord)
            flow := &pb.Flow{...}
            p.l7.Decode(&logrecord, flow)
            ev.Event = flow

        case monitorAPI.MessageTypeAgent:
            // 에이전트 알림 → 직접 변환
            ev.Event = agent.NotifyMessageToProto(agentNotifyMessage)
        }

    case *observerTypes.LostEvent:
        // 이벤트 손실 알림
        ev.Event = &pb.LostEvent{
            Source:        lostEventSourceToProto(payload.Source),
            NumEventsLost: payload.NumLostEvents,
            Cpu:           &wrapperspb.Int32Value{Value: int32(payload.CPU)},
        }
    }
    return ev, nil
}
```

### 3.3 이벤트 분기 다이어그램

```
                  MonitorEvent.Payload
                        │
            ┌───────────┼──────────┬──────────────┐
            │           │          │              │
       PerfEvent    AgentEvent  LostEvent       nil
            │           │          │              │
     Data[0] 확인   Type 확인    LostEvent    ErrEmptyData
            │           │        변환
   ┌────────┼──────┐    │
   │        │      │    │
 Debug  TraceSock  기타  ├── AccessLog → L7 파서
   │        │      │    │
  dbg     sock    l34   └── Agent → agent.NotifyMessageToProto
 파서     파서    파서

 출력: DebugEvent  출력: Flow         출력: AgentEvent
```

### 3.4 LostEvent 소스 매핑

```go
func lostEventSourceToProto(source int) pb.LostEventSource {
    switch source {
    case observerTypes.LostEventSourcePerfRingBuffer:
        return pb.LostEventSource_PERF_EVENT_RING_BUFFER    // eBPF perf 버퍼 오버플로우
    case observerTypes.LostEventSourceEventsQueue:
        return pb.LostEventSource_OBSERVER_EVENTS_QUEUE     // Observer 큐 오버플로우
    case observerTypes.LostEventSourceHubbleRingBuffer:
        return pb.LostEventSource_HUBBLE_RING_BUFFER        // Hubble Ring Buffer 오버플로우
    default:
        return pb.LostEventSource_UNKNOWN_LOST_EVENT_SOURCE
    }
}
```

---

## 4. L3/L4 파서 (threefour)

### 4.1 Parser 구조체

소스 경로: `cilium/pkg/hubble/parser/threefour/parser.go`

```go
type Parser struct {
    log            *slog.Logger
    endpointGetter getters.EndpointGetter
    identityGetter getters.IdentityGetter
    dnsGetter      getters.DNSGetter
    ipGetter       getters.IPGetter
    serviceGetter  getters.ServiceGetter
    linkGetter     getters.LinkGetter

    // 이벤트 타입별 디코더 함수
    dropNotifyDecoder          options.DropNotifyDecoderFunc
    traceNotifyDecoder         options.TraceNotifyDecoderFunc
    policyVerdictNotifyDecoder options.PolicyVerdictNotifyDecoderFunc
    debugCaptureDecoder        options.DebugCaptureDecoderFunc

    // 패킷 디코더 (gopacket 기반)
    packetDecoder options.L34PacketDecoder

    // 엔드포인트 리졸버
    epResolver          *common.EndpointResolver
    correlateL3L4Policy bool
}
```

### 4.2 Decode 메서드 상세

L3/L4 Decode는 가장 복잡한 디코딩 경로이다. 단계별로 분석한다:

**1단계: 이벤트 타입 파싱**

```go
func (p *Parser) Decode(data []byte, decoded *pb.Flow) error {
    eventType := data[0]  // 첫 번째 바이트가 이벤트 타입

    switch eventType {
    case monitorAPI.MessageTypeDrop:       // 드롭 이벤트
        dn, err = p.dropNotifyDecoder(data, decoded)
        eventSubType = dn.SubType
        packetOffset = int(dn.DataOffset())

    case monitorAPI.MessageTypeTrace:      // 트레이스 이벤트
        tn, err = p.traceNotifyDecoder(data, decoded)
        eventSubType = tn.ObsPoint
        decoded.TraceObservationPoint = pb.TraceObservationPoint(tn.ObsPoint)
        packetOffset = int(tn.DataOffset())

    case monitorAPI.MessageTypePolicyVerdict:  // 정책 판정 이벤트
        pvn, err = p.policyVerdictNotifyDecoder(data, decoded)
        eventSubType = pvn.SubType
        packetOffset = int(pvn.DataOffset())
        authType = pb.AuthType(pvn.GetAuthType())

    case monitorAPI.MessageTypeCapture:    // 디버그 캡처 이벤트
        dbg, err = p.debugCaptureDecoder(data, decoded)
        eventSubType = dbg.SubType
        packetOffset = int(dbg.DataOffset())
    }
    // ...
}
```

**2단계: 패킷 디코딩**

```go
    // 디바이스 타입과 IP 버전 플래그 추출
    isL3Device := tn != nil && tn.IsL3Device() || dn != nil && dn.IsL3Device()
    isIPv6 := tn != nil && tn.IsIPv6() || dn != nil && dn.IsIPv6()
    isVXLAN := tn != nil && tn.IsVXLAN() || dn != nil && dn.IsVXLAN()
    isGeneve := tn != nil && tn.IsGeneve() || dn != nil && dn.IsGeneve()

    // gopacket으로 패킷 헤더 파싱
    srcIP, dstIP, srcPort, dstPort, err := p.packetDecoder.DecodePacket(
        data[packetOffset:], decoded, isL3Device, isIPv6, isVXLAN, isGeneve,
    )
```

**3단계: 엔드포인트 해석 및 메타데이터 보강**

```go
    // 보안 ID 추출
    srcLabelID, dstLabelID := decodeSecurityIdentities(dn, tn, pvn)

    // 엔드포인트 정보 해석 (IP → Pod, Label, Identity)
    srcEndpoint := p.epResolver.ResolveEndpoint(srcIP, srcLabelID, datapathContext)
    dstEndpoint := p.epResolver.ResolveEndpoint(dstIP, dstLabelID, datapathContext)

    // 서비스 정보 해석 (IP:Port → Service)
    sourceService = p.serviceGetter.GetServiceByAddr(srcIP, srcPort)
    destinationService = p.serviceGetter.GetServiceByAddr(dstIP, dstPort)
```

**4단계: Flow 필드 설정**

```go
    decoded.Verdict = decodeVerdict(dn, tn, pvn)
    decoded.AuthType = authType
    decoded.DropReason = decodeDropReason(dn, pvn)
    decoded.Source = srcEndpoint
    decoded.Destination = dstEndpoint
    decoded.Type = pb.FlowType_L3_L4
    decoded.SourceNames = p.resolveNames(dstEndpoint.ID, srcIP)
    decoded.DestinationNames = p.resolveNames(srcEndpoint.ID, dstIP)
    decoded.IsReply = decodeIsReply(tn, pvn)
    decoded.TrafficDirection = decodeTrafficDirection(srcEndpoint.ID, dn, tn, pvn)
    decoded.EventType = decodeCiliumEventType(eventType, eventSubType)
    // ...
```

### 4.3 이벤트 타입별 처리 흐름

```
 data[0]
    │
    ├── MessageTypeDrop (0x01)
    │   ├── DropNotify 구조체 파싱
    │   ├── SubType → 드롭 사유
    │   ├── SrcLabel, DstLabel → 보안 ID
    │   └── Verdict = DROPPED
    │
    ├── MessageTypeTrace (0x02)
    │   ├── TraceNotify 구조체 파싱
    │   ├── ObsPoint → 관찰 포인트 (TO_ENDPOINT, TO_PROXY 등)
    │   ├── OriginalIP → SNAT 전 원본 IP
    │   ├── IsEncrypted → 암호화 여부
    │   └── Verdict = FORWARDED
    │
    ├── MessageTypePolicyVerdict (0x06)
    │   ├── PolicyVerdictNotify 파싱
    │   ├── Verdict < 0 → DROPPED
    │   ├── Verdict > 0 → REDIRECTED
    │   ├── IsTrafficAudited → AUDIT
    │   ├── Verdict == 0 → FORWARDED
    │   └── AuthType → 인증 방식
    │
    └── MessageTypeCapture (0x04)
        ├── DebugCapture 파싱
        ├── SubType → 캡처 포인트
        └── DebugCapturePoint 설정
```

---

## 5. L7 파서 (seven)

### 5.1 Parser 구조체

소스 경로: `cilium/pkg/hubble/parser/seven/parser.go`

```go
type Parser struct {
    log               *slog.Logger
    timestampCache    *lru.Cache[string, time.Time]       // 요청 타임스탬프 캐시
    traceContextCache *lru.Cache[string, *flowpb.TraceContext]  // 분산 추적 캐시
    dnsGetter         getters.DNSGetter
    ipGetter          getters.IPGetter
    serviceGetter     getters.ServiceGetter
    endpointGetter    getters.EndpointGetter
    opts              *options.Options
}
```

L7 파서는 두 개의 LRU 캐시를 사용한다:
- **timestampCache**: HTTP 요청/응답 간 레이턴시 계산용 (requestID → 요청 시각)
- **traceContextCache**: 분산 추적 컨텍스트 전파용 (requestID → TraceContext)

### 5.2 L7 파서 생성

```go
func New(log *slog.Logger, dnsGetter getters.DNSGetter, ...) (*Parser, error) {
    args := &options.Options{
        CacheSize: 10000,  // LRU 캐시 기본 크기
        HubbleRedactSettings: options.HubbleRedactSettings{
            Enabled:            false,
            RedactHTTPUserInfo: true,   // 기본: URL 사용자 정보 마스킹
            RedactHTTPQuery:    false,  // 기본: URL 쿼리 미마스킹
        },
    }

    timestampCache, _ := lru.New[string, time.Time](args.CacheSize)
    traceIDCache, _ := lru.New[string, *flowpb.TraceContext](args.CacheSize)

    return &Parser{
        timestampCache:    timestampCache,
        traceContextCache: traceIDCache,
        // ...
    }, nil
}
```

### 5.3 L7 Decode 흐름

```go
func (p *Parser) Decode(r *accesslog.LogRecord, decoded *flowpb.Flow) error {
    // 1. 타임스탬프 파싱
    timestamp, pbTimestamp, _ := decodeTime(r.Timestamp)

    // 2. IP 정보 추출
    ip := decodeIP(r.IPVersion, r.SourceEndpoint, r.DestinationEndpoint)
    sourceIP, _ := netip.ParseAddr(ip.Source)
    destinationIP, _ := netip.ParseAddr(ip.Destination)

    // 3. DNS 이름 해석 (역방향: 목적지가 출발지의 이름을 해석)
    sourceNames = p.dnsGetter.GetNamesOf(uint32(r.DestinationEndpoint.ID), sourceIP)
    destinationNames = p.dnsGetter.GetNamesOf(uint32(r.SourceEndpoint.ID), destinationIP)

    // 4. K8s 메타데이터 보강
    if meta := p.ipGetter.GetK8sMetadata(sourceIP); meta != nil {
        sourceNamespace, sourcePod = meta.Namespace, meta.PodName
    }

    // 5. 엔드포인트 및 서비스 정보 해석
    srcEndpoint := decodeEndpoint(r.SourceEndpoint, sourceNamespace, sourcePod)
    dstEndpoint := decodeEndpoint(r.DestinationEndpoint, ...)
    l4, sourcePort, destinationPort := decodeLayer4(r.TransportProtocol, ...)

    // 6. Flow 필드 설정
    decoded.Type = flowpb.FlowType_L7
    decoded.Verdict = decodeVerdict(r.Verdict)
    decoded.L7 = decodeLayer7(r, p.opts)           // HTTP/DNS/Kafka 파싱
    decoded.L7.LatencyNs = p.computeResponseTime(r, timestamp)  // 레이턴시 계산
    decoded.TraceContext = p.getTraceContext(r)     // 분산 추적

    return nil
}
```

### 5.4 L7 프로토콜 디코딩

```go
func decodeLayer7(r *accesslog.LogRecord, opts *options.Options) *flowpb.Layer7 {
    var flowType flowpb.L7FlowType
    switch r.Type {
    case accesslog.TypeRequest:  flowType = flowpb.L7FlowType_REQUEST
    case accesslog.TypeResponse: flowType = flowpb.L7FlowType_RESPONSE
    case accesslog.TypeSample:   flowType = flowpb.L7FlowType_SAMPLE
    }

    switch {
    case r.DNS != nil:
        return &flowpb.Layer7{Type: flowType, Record: decodeDNS(r.Type, r.DNS)}
    case r.HTTP != nil:
        return &flowpb.Layer7{Type: flowType, Record: decodeHTTP(r.Type, r.HTTP, opts)}
    default:
        return &flowpb.Layer7{Type: flowType}
    }
}
```

**지원 프로토콜:**

| 프로토콜 | 필드 | 주요 정보 |
|---------|------|----------|
| HTTP | `r.HTTP` | Method, URL, StatusCode, Headers |
| DNS | `r.DNS` | Query, RRType, RCode, Answers |
| Kafka | `r.Kafka` | Topic, APIKey, CorrelationID |
| Generic L7 | `r.L7` | Proto, Fields |

### 5.5 레이턴시 계산

```go
func (p *Parser) computeResponseTime(r *accesslog.LogRecord, timestamp time.Time) uint64 {
    requestID := extractRequestID(r)  // X-Request-Id 헤더
    if requestID == "" {
        return 0
    }

    switch r.Type {
    case accesslog.TypeRequest:
        // 요청 시 타임스탬프 캐시에 저장
        p.timestampCache.Add(requestID, timestamp)
    case accesslog.TypeResponse:
        // 응답 시 캐시에서 요청 시각 조회하여 레이턴시 계산
        requestTimestamp, ok := p.timestampCache.Get(requestID)
        if !ok {
            return 0
        }
        p.timestampCache.Remove(requestID)
        latency := timestamp.Sub(requestTimestamp).Nanoseconds()
        if latency < 0 {
            return 0
        }
        return uint64(latency)
    }
    return 0
}
```

이 설계에서 핵심은 **X-Request-Id** HTTP 헤더를 사용하여 요청-응답 쌍을 매칭한다는
것이다. Envoy 프록시가 이 헤더를 자동 주입하므로, Cilium Service Mesh 환경에서
레이턴시를 정확하게 측정할 수 있다.

### 5.6 분산 추적 컨텍스트

```go
func (p *Parser) getTraceContext(r *accesslog.LogRecord) *flowpb.TraceContext {
    requestID := extractRequestID(r)
    switch r.Type {
    case accesslog.TypeRequest:
        // 요청에서 traceContext 추출 후 캐시
        traceContext := extractTraceContext(r)
        if requestID != "" {
            p.traceContextCache.Add(requestID, traceContext)
        }
        return traceContext
    case accesslog.TypeResponse:
        // 응답에서 캐시된 traceContext 반환
        traceContext, ok := p.traceContextCache.Get(requestID)
        if ok {
            p.traceContextCache.Remove(requestID)
        }
        return traceContext
    }
    return nil
}
```

---

## 6. Debug 파서

Debug 파서는 `monitorAPI.MessageTypeDebug` 이벤트를 처리한다. 이 이벤트는 Cilium
데이터패스의 내부 디버그 메시지로, 패킷 헤더 없이 텍스트 메시지와 메타데이터를 담는다.

```
Debug 이벤트 구조:
┌──────────┬──────────┬──────────┬──────────┐
│ Type(1B) │ SubType  │ Source   │ Message  │
│ =Debug   │          │ (EP ID)  │          │
└──────────┴──────────┴──────────┴──────────┘
```

Debug 이벤트는 `*pb.Flow`가 아닌 `*pb.DebugEvent`로 변환된다. 이는 최상위 파서에서
특별하게 처리되는 유일한 경우이다:

```go
case monitorAPI.MessageTypeDebug:
    dbg, err := p.dbg.Decode(payload.Data, payload.CPU)
    ev.Event = dbg   // *pb.DebugEvent (Flow가 아님)
    return ev, nil
```

---

## 7. Sock 파서

Sock 파서는 `monitorAPI.MessageTypeTraceSock` 이벤트를 처리한다. 소켓 레벨의
주소 변환(XLATE) 이벤트로, cgroup 기반 소켓 후크에서 발생한다.

```
TraceSock 이벤트:
┌──────────┬──────────┬──────────┬──────────┬──────────┐
│ Type(1B) │ XlatePoint│ CgroupId │ IP/Port  │ Identity │
│ =TraceSock│          │          │          │          │
└──────────┴──────────┴──────────┴──────────┴──────────┘
```

Sock 파서의 특징:
- `PodMetadataGetter`를 통해 cgroup ID에서 Pod 메타데이터를 직접 조회
- 패킷 헤더 없이 소켓 정보만으로 Flow를 구성
- `SockXlatePoint` 필드로 변환 시점(pre/post, fwd/rev)을 표시

---

## 8. gopacket 기반 패킷 디코딩

### 8.1 packetDecoder 구조체

소스 경로: `cilium/pkg/hubble/parser/threefour/parser.go`

```go
type packetDecoder struct {
    lock.Mutex  // 동시 접근 보호 (재사용 가능 구조체)

    // L2 디바이스용 디코더 (Ethernet 헤더 시작)
    decLayerL2Dev *gopacket.DecodingLayerParser

    // L3 디바이스용 디코더 (IP 헤더 시작, Ethernet 없음)
    decLayerL3Dev struct {
        IPv4 *gopacket.DecodingLayerParser
        IPv6 *gopacket.DecodingLayerParser
    }

    // 오버레이 네트워크 디코더
    decLayerOverlay struct {
        VXLAN  *gopacket.DecodingLayerParser
        Geneve *gopacket.DecodingLayerParser
    }

    // 디코딩된 레이어 목록
    Layers []gopacket.LayerType

    // 재사용 가능한 레이어 구조체들
    layers.Ethernet
    layers.IPv4
    layers.IPv6
    layers.ICMPv4
    layers.ICMPv6
    layers.TCP
    layers.UDP
    layers.SCTP
    layers.VRRPv2
    layers.IGMPv1or2

    // 오버레이 레이어 (별도 네임스페이스)
    overlay struct {
        Layers []gopacket.LayerType
        layers.VXLAN
        layers.Geneve
        layers.Ethernet
        layers.IPv4
        // ... (동일 구조)
    }
}
```

**왜 이런 구조인가?**

gopacket의 `DecodingLayerParser`는 메모리 할당을 최소화하기 위해 pre-allocated 레이어
객체를 재사용한다. `packetDecoder`는 이 객체들을 구조체 필드로 임베딩하여 매 패킷
디코딩 시 새 객체를 할당하지 않는다. Mutex로 동시 접근을 보호한다.

### 8.2 디코더 초기화

```go
func New(...) (*Parser, error) {
    packet := &packetDecoder{}

    // 공통 디코더 레이어
    decoders := []gopacket.DecodingLayer{
        &packet.Ethernet,
        &packet.IPv4, &packet.IPv6,
        &packet.ICMPv4, &packet.ICMPv6,
        &packet.TCP, &packet.UDP, &packet.SCTP,
        &packet.VRRPv2, &packet.IGMPv1or2,
    }

    // L2 디바이스: Ethernet 헤더부터 시작
    packet.decLayerL2Dev = gopacket.NewDecodingLayerParser(
        layers.LayerTypeEthernet, decoders...)

    // L3 디바이스: IP 헤더부터 시작 (veth 등)
    packet.decLayerL3Dev.IPv4 = gopacket.NewDecodingLayerParser(
        layers.LayerTypeIPv4, decoders...)
    packet.decLayerL3Dev.IPv6 = gopacket.NewDecodingLayerParser(
        layers.LayerTypeIPv6, decoders...)

    // 미지원 레이어 무시 (에러 대신 nil 반환)
    packet.decLayerL2Dev.IgnoreUnsupported = true
    packet.decLayerL3Dev.IPv4.IgnoreUnsupported = true
    // ...
}
```

### 8.3 DecodePacket 메서드

```go
func (d *packetDecoder) DecodePacket(
    payload []byte, decoded *pb.Flow,
    isL3Device, isIPv6, isVXLAN, isGeneve bool,
) (sourceIP, destinationIP netip.Addr, sourcePort, destinationPort uint16, err error) {

    d.Lock()
    defer d.Unlock()

    // 1단계: 디바이스 타입에 따라 적절한 디코더 선택
    switch {
    case !isL3Device:
        err = d.decLayerL2Dev.DecodeLayers(payload, &d.Layers)  // Ethernet부터
    case isIPv6:
        err = d.decLayerL3Dev.IPv6.DecodeLayers(payload, &d.Layers)  // IPv6부터
    default:
        err = d.decLayerL3Dev.IPv4.DecodeLayers(payload, &d.Layers)  // IPv4부터
    }

    // 2단계: 디코딩된 레이어를 Flow 필드에 매핑
    for _, typ := range d.Layers {
        switch typ {
        case layers.LayerTypeEthernet:
            decoded.Ethernet = decodeEthernet(&d.Ethernet)
        case layers.LayerTypeIPv4:
            decoded.IP, sourceIP, destinationIP = decodeIPv4(&d.IPv4)
        case layers.LayerTypeTCP:
            decoded.L4, sourcePort, destinationPort = decodeTCP(&d.TCP)
            decoded.Summary = "TCP Flags: " + getTCPFlags(d.TCP)
        case layers.LayerTypeUDP:
            decoded.L4, sourcePort, destinationPort = decodeUDP(&d.UDP)
        // ... ICMPv4, ICMPv6, SCTP, VRRP, IGMP 등
        }
    }

    // 3단계: 오버레이 네트워크 처리 (VXLAN/Geneve)
    // (별도 섹션에서 상세 설명)
    return
}
```

### 8.4 레이어 디코딩 함수들

```go
func decodeTCP(tcp *layers.TCP) (l4 *pb.Layer4, src, dst uint16) {
    return &pb.Layer4{
        Protocol: &pb.Layer4_TCP{
            TCP: &pb.TCP{
                SourcePort:      uint32(tcp.SrcPort),
                DestinationPort: uint32(tcp.DstPort),
                Flags: &pb.TCPFlags{
                    FIN: tcp.FIN, SYN: tcp.SYN, RST: tcp.RST,
                    PSH: tcp.PSH, ACK: tcp.ACK, URG: tcp.URG,
                    ECE: tcp.ECE, CWR: tcp.CWR, NS: tcp.NS,
                },
            },
        },
    }, uint16(tcp.SrcPort), uint16(tcp.DstPort)
}

func decodeUDP(udp *layers.UDP) (l4 *pb.Layer4, src, dst uint16) {
    return &pb.Layer4{
        Protocol: &pb.Layer4_UDP{
            UDP: &pb.UDP{
                SourcePort:      uint32(udp.SrcPort),
                DestinationPort: uint32(udp.DstPort),
            },
        },
    }, uint16(udp.SrcPort), uint16(udp.DstPort)
}
```

**지원 프로토콜 목록:**

| 레이어 | 프로토콜 | protobuf 타입 |
|--------|---------|--------------|
| L2 | Ethernet | `pb.Ethernet` |
| L3 | IPv4, IPv6 | `pb.IP` |
| L4 | TCP | `pb.Layer4_TCP` |
| L4 | UDP | `pb.Layer4_UDP` |
| L4 | SCTP | `pb.Layer4_SCTP` |
| L4 | ICMPv4, ICMPv6 | `pb.Layer4_ICMPv4/v6` |
| L4 | VRRP | `pb.Layer4_VRRP` |
| L4 | IGMP | `pb.Layer4_IGMP` |

---

## 9. Endpoint/Identity 리졸버

### 9.1 엔드포인트 해석 과정

`EndpointResolver`는 IP 주소와 보안 Identity를 기반으로 Flow의 Source/Destination
엔드포인트 정보를 해석한다:

```
  IP + Security Identity
         │
         v
  ┌──────────────────┐
  │ EndpointResolver │
  │ .ResolveEndpoint │
  └──────┬───────────┘
         │
    ┌────┴────┐
    │         │
    v         v
  EndpointGetter  IdentityGetter
  (IP → EP Info)  (ID → Labels)
    │         │
    v         v
  pb.Endpoint
  ├── ID
  ├── Identity
  ├── Namespace
  ├── PodName
  ├── Labels
  ├── ClusterName
  └── Workloads
```

### 9.2 DNS 이름 해석

DNS 이름은 역방향으로 해석된다. 즉, **출발지 엔드포인트의 관점**에서 목적지 IP의
이름을 조회한다:

```go
// 목적지 EP의 ID로 출발지 IP의 이름 조회
decoded.SourceNames = p.resolveNames(dstEndpoint.ID, srcIP)
// 출발지 EP의 ID로 목적지 IP의 이름 조회
decoded.DestinationNames = p.resolveNames(srcEndpoint.ID, dstIP)

func (p *Parser) resolveNames(epID uint32, ip netip.Addr) (names []string) {
    if p.dnsGetter != nil {
        return p.dnsGetter.GetNamesOf(epID, ip)
    }
    return nil
}
```

---

## 10. Getter 인터페이스

소스 경로: `cilium/pkg/hubble/parser/getters/getters.go`

파서가 사용하는 Getter 인터페이스는 Cilium 에이전트의 내부 상태에 대한 추상화 계층이다.

### 10.1 DNSGetter

```go
type DNSGetter interface {
    // IP의 FQDN 조회 (sourceEpID 관점)
    GetNamesOf(sourceEpID uint32, ip netip.Addr) (names []string)
}
```

### 10.2 EndpointGetter

```go
type EndpointGetter interface {
    // IP로 엔드포인트 조회
    GetEndpointInfo(ip netip.Addr) (endpoint EndpointInfo, ok bool)
    // ID로 엔드포인트 조회
    GetEndpointInfoByID(id uint16) (endpoint EndpointInfo, ok bool)
}
```

### 10.3 IdentityGetter

```go
type IdentityGetter interface {
    // 숫자 보안 ID로 전체 Identity 객체 조회
    GetIdentity(id uint32) (*identity.Identity, error)
}
```

### 10.4 IPGetter

```go
type IPGetter interface {
    // IP의 K8s 메타데이터 (Namespace, PodName) 조회
    GetK8sMetadata(ip netip.Addr) *ipcache.K8sMetadata
    // IP의 보안 ID 조회
    LookupSecIDByIP(ip netip.Addr) (ipcache.Identity, bool)
}
```

### 10.5 ServiceGetter

```go
type ServiceGetter interface {
    // IP:Port로 서비스 조회
    GetServiceByAddr(ip netip.Addr, port uint16) *flowpb.Service
}
```

### 10.6 LinkGetter

```go
type LinkGetter interface {
    // 인터페이스 인덱스로 이름 조회 (캐시 기반)
    GetIfNameCached(ifIndex int) (string, bool)
    // 인터페이스 인덱스로 이름 반환
    Name(ifIndex uint32) string
}
```

### 10.7 PodMetadataGetter

```go
type PodMetadataGetter interface {
    // cgroup ID로 Pod 메타데이터 조회
    GetPodMetadataForContainer(cgroupId uint64) *cgroupManager.PodMetadata
}
```

### 10.8 EndpointInfo 인터페이스

```go
type EndpointInfo interface {
    GetID() uint64
    GetIdentity() identity.NumericIdentity
    GetK8sPodName() string
    GetK8sNamespace() string
    GetLabels() labels.Labels
    GetPod() *slim_corev1.Pod
    GetPolicyCorrelationInfoForKey(key policyTypes.Key) (policyTypes.PolicyCorrelationInfo, bool)
}
```

---

## 11. LRU 캐시 시스템

### 11.1 L7 파서의 캐시

L7 파서는 `hashicorp/golang-lru/v2`를 사용하여 두 가지 캐시를 관리한다:

```
                 HTTP Request
                     │
                     │ X-Request-Id: abc123
                     v
              ┌──────────────┐
              │ timestampCache│       캐시 크기: 10,000
              │ [abc123]=T1  │
              └──────────────┘
                     │
                     │ ... 시간 경과 ...
                     │
                HTTP Response
                     │
                     │ X-Request-Id: abc123
                     v
              ┌──────────────┐
              │ timestampCache│
              │ Get(abc123)  │ → T1
              │ Remove(abc123)│ → latency = T2 - T1
              └──────────────┘
```

```go
// 캐시 크기 설정 (기본 10,000)
args := &options.Options{
    CacheSize: 10000,
}

// 타임스탬프 캐시: requestID → 요청 시각
timestampCache, _ := lru.New[string, time.Time](args.CacheSize)

// 트레이스 컨텍스트 캐시: requestID → TraceContext
traceIDCache, _ := lru.New[string, *flowpb.TraceContext](args.CacheSize)
```

### 11.2 왜 LRU 캐시인가?

- **메모리 제한**: 무한 증가 방지. 10,000개 이상의 미완료 요청이 있으면 가장 오래된 항목이 자동 제거
- **성능**: O(1) 조회/삽입/삭제
- **무응답 요청 처리**: 응답 없는 요청은 LRU 정책에 의해 자연스럽게 만료

---

## 12. Verdict와 TrafficDirection 결정

### 12.1 Verdict (판정) 결정 로직

```go
func decodeVerdict(dn *monitor.DropNotify, tn *monitor.TraceNotify,
                   pvn *monitor.PolicyVerdictNotify) pb.Verdict {
    switch {
    case dn != nil:
        return pb.Verdict_DROPPED           // 드롭 이벤트
    case tn != nil:
        return pb.Verdict_FORWARDED         // 트레이스 이벤트 (전달됨)
    case pvn != nil:
        if pvn.Verdict < 0 {
            return pb.Verdict_DROPPED       // 정책에 의해 드롭
        }
        if pvn.Verdict > 0 {
            return pb.Verdict_REDIRECTED    // 프록시로 리다이렉트
        }
        if pvn.IsTrafficAudited() {
            return pb.Verdict_AUDIT         // 감사 모드 (로그만)
        }
        return pb.Verdict_FORWARDED         // 정책 허용
    }
    return pb.Verdict_VERDICT_UNKNOWN
}
```

### 12.2 TrafficDirection 결정 로직

TrafficDirection은 패킷이 ingress인지 egress인지를 판단하는 복잡한 로직이다:

```go
func decodeTrafficDirection(srcEP uint32, dn *monitor.DropNotify,
    tn *monitor.TraceNotify, pvn *monitor.PolicyVerdictNotify) pb.TrafficDirection {

    // 드롭 이벤트: 드롭 소스가 패킷 출발지와 같으면 egress
    if dn != nil && dn.Source != 0 {
        if dn.Source == uint16(srcEP) {
            return pb.TrafficDirection_EGRESS
        }
        return pb.TrafficDirection_INGRESS
    }

    // 트레이스 이벤트: CT(Connection Tracking) 결과 고려
    if tn != nil && tn.Source != 0 {
        if tn.TraceReasonIsKnown() {
            isSourceEP := tn.Source == uint16(srcEP)
            isSNATed := !tn.OriginalIP().IsUnspecified()
            isReply := tn.TraceReasonIsReply()

            switch {
            case isSourceEP != isReply:
                return pb.TrafficDirection_EGRESS
            case isSNATed:
                return pb.TrafficDirection_EGRESS
            }
            return pb.TrafficDirection_INGRESS
        }
    }

    // PolicyVerdict: 직접 방향 정보 포함
    if pvn != nil {
        if pvn.IsTrafficIngress() {
            return pb.TrafficDirection_INGRESS
        }
        return pb.TrafficDirection_EGRESS
    }

    return pb.TrafficDirection_TRAFFIC_DIRECTION_UNKNOWN
}
```

### 12.3 Reply 패킷 판단

```go
func decodeIsReply(tn *monitor.TraceNotify, pvn *monitor.PolicyVerdictNotify) *wrapperspb.BoolValue {
    switch {
    case tn != nil && tn.TraceReasonIsKnown():
        if tn.TraceReasonIsEncap() || tn.TraceReasonIsDecap() {
            return nil  // 캡슐화/역캡슐화는 방향 불분명
        }
        return &wrapperspb.BoolValue{Value: tn.TraceReasonIsReply()}

    case pvn != nil && pvn.Verdict >= 0:
        // 전달된 PolicyVerdict는 연결의 첫 패킷이므로 reply가 아님
        return &wrapperspb.BoolValue{Value: false}

    default:
        return nil  // 판단 불가
    }
}
```

---

## 13. 오버레이 네트워크 처리

### 13.1 VXLAN/Geneve 디코딩

Hubble은 VXLAN과 Geneve 오버레이 터널 패킷을 디코딩하여 내부(inner) 패킷 정보를
추출한다:

```go
// 오버레이 패킷 디코딩
switch {
case isVXLAN:
    err = d.decLayerOverlay.VXLAN.DecodeLayers(d.UDP.Payload, &d.overlay.Layers)
case isGeneve:
    err = d.decLayerOverlay.Geneve.DecodeLayers(d.UDP.Payload, &d.overlay.Layers)
}

// 터널 정보 설정
switch d.overlay.Layers[0] {
case layers.LayerTypeVXLAN:
    decoded.Tunnel = &pb.Tunnel{
        Protocol: pb.Tunnel_VXLAN,
        IP: decoded.IP,       // 외부(underlay) IP
        L4: decoded.L4,       // 외부 L4
        Vni: d.overlay.VXLAN.VNI,
    }
case layers.LayerTypeGeneve:
    decoded.Tunnel = &pb.Tunnel{
        Protocol: pb.Tunnel_GENEVE,
        IP: decoded.IP,
        L4: decoded.L4,
        Vni: d.overlay.Geneve.VNI,
    }
}

// 외부 정보 초기화하고 내부 패킷으로 대체
decoded.Ethernet, decoded.IP, decoded.L4 = nil, nil, nil
sourceIP, destinationIP = netip.Addr{}, netip.Addr{}
```

### 13.2 오버레이 처리 흐름

```
  원본 패킷:
  ┌──────────┬──────┬──────┬──────────────────────┐
  │ Outer    │Outer │Outer │ VXLAN/Geneve Header   │
  │ Ethernet │ IP   │ UDP  │ + Inner Packet        │
  └──────────┴──────┴──────┴──────────────────────┘

  1차 디코딩 결과:
  decoded.Ethernet = Outer Ethernet
  decoded.IP = Outer IP
  decoded.L4 = Outer UDP

  VXLAN/Geneve 감지:
  decoded.Tunnel = {Protocol, OuterIP, OuterL4, VNI}

  2차 디코딩 (내부 패킷):
  decoded.Ethernet = Inner Ethernet (또는 nil)
  decoded.IP = Inner IP
  decoded.L4 = Inner TCP/UDP/SCTP
```

---

## 14. 파서 옵션 시스템

### 14.1 Options 구조체

파서 옵션은 함수형 옵션 패턴을 사용한다:

```go
type Option func(*Options)

type Options struct {
    CacheSize                     int
    EnableNetworkPolicyCorrelation bool

    // 이벤트 디코더 함수 (교체 가능)
    DropNotifyDecoder          DropNotifyDecoderFunc
    TraceNotifyDecoder         TraceNotifyDecoderFunc
    PolicyVerdictNotifyDecoder PolicyVerdictNotifyDecoderFunc
    DebugCaptureDecoder        DebugCaptureDecoderFunc
    L34PacketDecoder           L34PacketDecoder

    // HTTP 데이터 마스킹
    HubbleRedactSettings HubbleRedactSettings
}

type HubbleRedactSettings struct {
    Enabled            bool
    RedactHTTPUserInfo bool    // URL에서 사용자 정보 마스킹
    RedactHTTPQuery    bool    // URL에서 쿼리 파라미터 마스킹
    RedactHttpHeaders  HttpHeadersList  // 헤더 허용/거부 목록
}
```

### 14.2 기본 디코더 함수

```go
args := &options.Options{
    DropNotifyDecoder: func(data []byte, decoded *pb.Flow) (*monitor.DropNotify, error) {
        dn := &monitor.DropNotify{}
        return dn, dn.Decode(data)
    },
    TraceNotifyDecoder: func(data []byte, decoded *pb.Flow) (*monitor.TraceNotify, error) {
        tn := &monitor.TraceNotify{}
        return tn, tn.Decode(data)
    },
    PolicyVerdictNotifyDecoder: func(data []byte, decoded *pb.Flow) (*monitor.PolicyVerdictNotify, error) {
        pvn := &monitor.PolicyVerdictNotify{}
        return pvn, pvn.Decode(data)
    },
    DebugCaptureDecoder: func(data []byte, decoded *pb.Flow) (*monitor.DebugCapture, error) {
        dbg := &monitor.DebugCapture{}
        return dbg, dbg.Decode(data)
    },
    L34PacketDecoder: packet,  // gopacket 기반 packetDecoder
}
```

이 설계의 장점:
- 테스트에서 디코더를 모킹할 수 있음
- 향후 디코더 구현을 교체할 수 있음
- 플러그인 아키텍처를 지원

---

## 요약

| 구성요소 | 역할 | 입력 | 출력 |
|---------|------|------|------|
| Parser (최상위) | 이벤트 분기 | MonitorEvent | v1.Event |
| threefour.Parser | L3/L4 디코딩 | []byte (PerfEvent) | pb.Flow |
| seven.Parser | L7 디코딩 | accesslog.LogRecord | pb.Flow |
| debug.Parser | 디버그 디코딩 | []byte | pb.DebugEvent |
| sock.Parser | 소켓 디코딩 | []byte | pb.Flow |
| packetDecoder | gopacket 래퍼 | []byte (패킷 헤더) | IP/Port/L4 |
| EndpointResolver | EP 해석 | IP + Identity | pb.Endpoint |
| LRU Cache | 레이턴시 계산 | requestID | timestamp/TraceContext |
