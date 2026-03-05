# 02. Hubble 데이터 모델

## 개요

Hubble의 데이터 모델은 **Protocol Buffers**로 정의된다.
핵심은 네트워크 이벤트를 표현하는 `Flow` 메시지이며,
이를 관찰/전달하기 위한 Observer API와 노드 관리를 위한 Peer API가 함께 구성된다.

## Proto 파일 구조

```
cilium/api/v1/
├── flow/
│   └── flow.proto           # Flow, Endpoint, Layer4, Layer7, Verdict, FlowFilter
├── observer/
│   └── observer.proto       # Observer 서비스, GetFlows, ServerStatus
├── peer/
│   └── peer.proto           # Peer 서비스, ChangeNotification
└── relay/
    └── relay.proto          # NodeStatusEvent, NodeState
```

---

## Flow 메시지 (핵심 데이터 구조)

Flow는 Hubble이 관측하는 네트워크 이벤트의 구조화된 표현이다.
40개 이상의 필드로 L2부터 L7까지의 정보를 담는다.

### 소스 위치
- Proto 정의: `cilium/api/v1/flow/flow.proto` (Line 14-158)
- Go 생성: `cilium/api/v1/flow/flow.pb.go`

### Flow 필드 구조

```
message Flow {
    +-----------+-------------------------+------------------------------------------+
    | 카테고리   | 필드                     | 설명                                     |
    +-----------+-------------------------+------------------------------------------+
    | 시간/ID   | time                    | 이벤트 발생 시각 (Timestamp)                |
    |           | uuid                    | 고유 식별자                                |
    |           | emitter                 | 이벤트 발생원 (Hubble 이름/버전)             |
    +-----------+-------------------------+------------------------------------------+
    | 판정      | verdict                 | FORWARDED, DROPPED, ERROR, AUDIT 등       |
    |           | drop_reason_desc        | 드롭 사유 (POLICY_DENIED 등 70+ enum)      |
    |           | auth_type               | 인증 타입 (DISABLED, SPIRE)                |
    +-----------+-------------------------+------------------------------------------+
    | L2        | ethernet                | 소스/목적지 MAC 주소                        |
    +-----------+-------------------------+------------------------------------------+
    | L3        | IP                      | 소스/목적지 IP, IP 버전, 암호화 여부          |
    |           | tunnel                  | 터널 프로토콜 (VXLAN, GENEVE), VNI          |
    +-----------+-------------------------+------------------------------------------+
    | L4        | l4                      | TCP/UDP/ICMPv4/ICMPv6/SCTP 프로토콜 정보    |
    +-----------+-------------------------+------------------------------------------+
    | Endpoint  | source                  | 소스 엔드포인트 (ID, identity, pod, labels)  |
    |           | destination             | 목적지 엔드포인트                            |
    +-----------+-------------------------+------------------------------------------+
    | Service   | source_service          | 소스 서비스 (name, namespace)               |
    |           | destination_service     | 목적지 서비스                                |
    +-----------+-------------------------+------------------------------------------+
    | DNS/Names | source_names            | 소스 IP의 모든 DNS 이름                     |
    |           | destination_names       | 목적지 IP의 모든 DNS 이름                    |
    +-----------+-------------------------+------------------------------------------+
    | L7        | l7                      | HTTP, DNS, Kafka 프로토콜 상세 정보          |
    +-----------+-------------------------+------------------------------------------+
    | 메타      | Type                    | L3_L4, L7, SOCK                          |
    |           | node_name               | 노드 이름                                  |
    |           | node_labels             | 노드 라벨                                  |
    |           | traffic_direction       | INGRESS, EGRESS                           |
    |           | is_reply                | 응답 패킷 여부                              |
    |           | event_type              | Cilium 이벤트 타입/서브타입                  |
    |           | trace_observation_point | 관측 지점 (FROM_ENDPOINT, TO_PROXY 등)     |
    |           | trace_reason            | 추적 사유 (NEW, ESTABLISHED, REPLY 등)     |
    +-----------+-------------------------+------------------------------------------+
    | Policy    | egress_allowed_by       | 이그레스 허용 정책 목록                      |
    |           | ingress_allowed_by      | 인그레스 허용 정책 목록                      |
    |           | egress_denied_by        | 이그레스 거부 정책 목록                      |
    |           | ingress_denied_by       | 인그레스 거부 정책 목록                      |
    +-----------+-------------------------+------------------------------------------+
    | 확장      | extensions              | google.protobuf.Any (임의 메타데이터)       |
    |           | aggregate               | Flow 집계 카운터 (ingress/egress count)     |
    +-----------+-------------------------+------------------------------------------+
```

---

## Endpoint

`Endpoint`는 네트워크 통신의 소스/목적지를 표현한다.

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 300-309)
message Endpoint {
    uint32 ID = 1;              // Cilium 엔드포인트 ID
    uint32 identity = 2;        // 보안 Identity
    string cluster_name = 7;    // 클러스터 이름
    string namespace = 3;       // 쿠버네티스 네임스페이스
    repeated string labels = 4; // 라벨 ("k8s:app=nginx" 형식)
    string pod_name = 5;        // Pod 이름
    repeated Workload workloads = 6; // 워크로드 (name, kind)
}
```

Endpoint의 `identity`는 Cilium의 보안 모델의 핵심이다. 같은 라벨 셋을 가진 엔드포인트는 같은 identity를 공유하며, 네트워크 정책은 identity 기반으로 적용된다.

---

## Layer4 프로토콜

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 251-262)
message Layer4 {
    oneof protocol {
        TCP TCP = 1;
        UDP UDP = 2;
        ICMPv4 ICMPv4 = 3;
        ICMPv6 ICMPv6 = 4;
        SCTP SCTP = 5;
        VRRP VRRP = 6;
        IGMP IGMP = 7;
    }
}
```

### TCP 메시지

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 316-320)
message TCP {
    uint32 source_port = 1;
    uint32 destination_port = 2;
    TCPFlags flags = 3;   // FIN, SYN, RST, PSH, ACK, URG, ECE, CWR, NS
}
```

### IP 메시지

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 322-333)
message IP {
    string source = 1;           // 소스 IP
    string source_xlated = 5;    // SNAT 후 소스 IP
    string destination = 2;      // 목적지 IP
    IPVersion ipVersion = 3;     // IPv4 / IPv6
    bool encrypted = 4;          // WireGuard/IPsec 암호화 여부
}
```

---

## Layer7 프로토콜

L7 정보는 Cilium의 L7 프록시를 통과하는 트래픽에서만 수집된다.

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 273-283)
message Layer7 {
    L7FlowType type = 1;   // REQUEST, RESPONSE, SAMPLE
    uint64 latency_ns = 2; // 응답 지연시간 (나노초)
    oneof record {
        DNS dns = 100;
        HTTP http = 101;
        Kafka kafka = 102;  // deprecated
    }
}
```

### DNS

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 728-749)
message DNS {
    string query = 1;                  // 조회 도메인 ("isovalent.com.")
    repeated string ips = 2;           // 응답 IP 목록
    uint32 ttl = 3;                    // TTL
    repeated string cnames = 4;        // CNAME 목록
    string observation_source = 5;     // 관측 소스
    uint32 rcode = 6;                  // DNS 응답 코드
    repeated string qtypes = 7;        // 쿼리 타입 ("A", "AAAA")
    repeated string rrtypes = 8;       // 리소스 레코드 타입
}
```

### HTTP

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 757-763)
message HTTP {
    uint32 code = 1;                   // 상태 코드 (200, 404 등)
    string method = 2;                 // GET, POST, PUT 등
    string url = 3;                    // 요청 URL
    string protocol = 4;              // HTTP/1.1, HTTP/2
    repeated HTTPHeader headers = 5;   // 헤더 키-값 쌍
}
```

---

## Verdict (판정)

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 413-436)
enum Verdict {
    VERDICT_UNKNOWN = 0;   // 판정 없음
    FORWARDED = 1;         // 전달됨
    DROPPED = 2;           // 드롭됨 (drop_reason_desc에 사유)
    ERROR = 3;             // 처리 중 오류
    AUDIT = 4;             // 감사 모드 (정책 위반이나 실제 드롭 안 함)
    REDIRECTED = 5;        // 프록시로 리다이렉트
    TRACED = 6;            // 추적 포인트에서 관측 (판정 미확정)
    TRANSLATED = 7;        // 주소 변환됨
}
```

### DropReason

드롭 사유는 70개 이상의 enum 값으로 정의된다. 주요 사유:

| 코드 | 이름 | 설명 |
|------|------|------|
| 132 | INVALID_SOURCE_IP | 유효하지 않은 소스 IP |
| 133 | POLICY_DENIED | 네트워크 정책에 의한 거부 |
| 140 | MISSED_TAIL_CALL | 테일 콜 실패 |
| 142 | UNKNOWN_L4_PROTOCOL | 알 수 없는 L4 프로토콜 |
| 158 | SERVICE_BACKEND_NOT_FOUND | 서비스 백엔드 미발견 |
| 181 | POLICY_DENY | 정책 명시적 거부 |
| 189 | AUTH_REQUIRED | 인증 필요 |
| 195 | UNENCRYPTED_TRAFFIC | 암호화되지 않은 트래픽 |
| 196 | TTL_EXCEEDED | TTL 초과 |

---

## FlowFilter (필터)

FlowFilter는 Flow를 걸러내기 위한 조건을 정의한다. 모든 필드는 선택적이며, 설정된 필드는 AND로 결합된다.

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 588-716)
message FlowFilter {
    repeated string uuid = 29;
    // 소스 필터
    repeated string source_ip = 1;         // CIDR 지원 ("10.0.0.0/24")
    repeated string source_pod = 2;        // "namespace/pod-prefix"
    repeated string source_fqdn = 7;       // 정규화된 도메인 이름
    repeated string source_label = 10;     // 라벨 셀렉터
    repeated string source_service = 16;   // 서비스 이름
    repeated Workload source_workload = 26;
    // 목적지 필터 (동일 구조)
    repeated string destination_ip = 3;
    repeated string destination_pod = 4;
    // ... 동일 패턴
    // 공통 필터
    repeated TrafficDirection traffic_direction = 30;
    repeated Verdict verdict = 5;
    repeated DropReason drop_reason_desc = 33;
    repeated EventTypeFilter event_type = 6;
    repeated string http_status_code = 9;
    repeated string protocol = 12;
    repeated string source_port = 13;
    repeated string destination_port = 14;
    repeated bool reply = 15;
    repeated string dns_query = 18;        // RE2 정규식 패턴
    repeated string http_method = 21;
    repeated string http_path = 22;
    repeated TCPFlags tcp_flags = 23;
    repeated string node_name = 24;        // 와일드카드 ("k8s*")
    repeated IPVersion ip_version = 25;
    repeated string trace_id = 28;
    // 실험적
    Experimental experimental = 999;       // CEL 표현식 필터
}
```

### 필터 동작 모델

```
GetFlowsRequest {
    whitelist: [FlowFilter, FlowFilter, ...]  // OR 결합
    blacklist: [FlowFilter, FlowFilter, ...]  // OR 결합
}

결과 = whitelist.MatchOne(flow) AND blacklist.MatchNone(flow)

각 FlowFilter 내부: 설정된 필드는 모두 AND로 결합
여러 FlowFilter 간: OR로 결합
whitelist와 blacklist 관계: 차집합 (whitelist - blacklist)
```

---

## Observer API 메시지

### GetFlowsRequest

```protobuf
// 소스: cilium/api/v1/observer/observer.proto (Line 83-137)
message GetFlowsRequest {
    uint64 number = 1;                          // 반환할 Flow 수
    bool first = 9;                             // 첫 N개 (기본: 마지막 N개)
    bool follow = 3;                            // 실시간 스트리밍
    repeated flow.FlowFilter blacklist = 5;     // 제외 필터
    repeated flow.FlowFilter whitelist = 6;     // 포함 필터
    google.protobuf.Timestamp since = 7;        // 시작 시각
    google.protobuf.Timestamp until = 8;        // 종료 시각
    google.protobuf.FieldMask field_mask = 10;  // 반환 필드 제한
    google.protobuf.Any extensions = 150000;    // 확장 메타데이터
}
```

### GetFlowsResponse

```protobuf
// 소스: cilium/api/v1/observer/observer.proto (Line 140-154)
message GetFlowsResponse {
    oneof response_types {
        flow.Flow flow = 1;                     // Flow 데이터
        relay.NodeStatusEvent node_status = 2;  // 노드 상태 이벤트
        flow.LostEvent lost_events = 3;         // 유실된 이벤트 알림
    }
    string node_name = 1000;                    // 관측 노드 이름
    google.protobuf.Timestamp time = 1001;      // 관측 시각
}
```

### ServerStatusResponse

```protobuf
// 소스: cilium/api/v1/observer/observer.proto (Line 44-81)
message ServerStatusResponse {
    uint64 num_flows = 1;          // 현재 캡처된 Flow 수
    uint64 max_flows = 2;          // Ring Buffer 최대 용량
    uint64 seen_flows = 3;         // 총 관측된 Flow 수
    uint64 uptime_ns = 4;          // 가동 시간 (나노초)
    UInt32Value num_connected_nodes = 5;    // 연결된 노드 수
    UInt32Value num_unavailable_nodes = 6;  // 비가용 노드 수
    repeated string unavailable_nodes = 7;  // 비가용 노드 목록
    string version = 8;            // Cilium/Hubble 버전
    double flows_rate = 9;         // 초당 Flow 속도 (최근 1분)
}
```

---

## Peer API 메시지

### ChangeNotification

```protobuf
// 소스: cilium/api/v1/peer/peer.proto (Line 22-44)
message ChangeNotification {
    string name = 1;                       // 피어 이름 ("cluster/node")
    string address = 2;                    // gRPC 서비스 주소
    ChangeNotificationType type = 3;       // PEER_ADDED, DELETED, UPDATED
    TLS tls = 4;                          // TLS 연결 정보
}

enum ChangeNotificationType {
    UNKNOWN = 0;
    PEER_ADDED = 1;
    PEER_DELETED = 2;
    PEER_UPDATED = 3;
}
```

---

## Relay API 메시지

### NodeStatusEvent

```protobuf
// 소스: cilium/api/v1/relay/relay.proto (Line 12-20)
message NodeStatusEvent {
    NodeState state_change = 1;            // 상태 변경
    repeated string node_names = 2;        // 대상 노드 목록
    string message = 3;                    // 상태 메시지 (에러 등)
}

enum NodeState {
    UNKNOWN_NODE_STATE = 0;
    NODE_CONNECTED = 1;      // 연결됨
    NODE_UNAVAILABLE = 2;    // 연결 불가
    NODE_GONE = 3;           // 클러스터에서 제거됨
    NODE_ERROR = 4;          // 오류 발생
}
```

---

## 내부 데이터 구조 (Go)

### v1.Event

`v1.Event`는 Ring Buffer에 저장되는 내부 이벤트 래퍼이다.

```go
// 소스: cilium/pkg/hubble/api/v1/types.go (Line 13-18)
type Event struct {
    Timestamp *timestamppb.Timestamp  // 관측 시각
    Event     any                     // Flow, AgentEvent, DebugEvent, LostEvent 중 하나
}
```

타입 어서션 헬퍼 메서드:

| 메서드 | 반환 타입 | 설명 |
|--------|----------|------|
| `GetFlow()` | `*pb.Flow` | Flow 이벤트 추출 |
| `GetAgentEvent()` | `*pb.AgentEvent` | 에이전트 이벤트 추출 |
| `GetDebugEvent()` | `*pb.DebugEvent` | 디버그 이벤트 추출 |
| `GetLostEvent()` | `*pb.LostEvent` | 유실 이벤트 추출 |

### MonitorEvent

`MonitorEvent`는 Cilium MonitorAgent에서 Observer로 전달되는 원시 이벤트이다.

```go
// 소스: cilium/pkg/hubble/observer/types/types.go (Line 29-38)
type MonitorEvent struct {
    UUID      uuid.UUID   // 고유 ID
    Timestamp time.Time   // 수신 시각
    NodeName  string      // 노드 이름
    Payload   any         // AgentEvent, PerfEvent, LostEvent 중 하나
}
```

### Payload 타입

```go
// PerfEvent: eBPF perf ring buffer에서 온 원시 데이터
type PerfEvent struct {
    Data []byte   // 원시 바이트 (첫 바이트가 메시지 타입)
    CPU  int      // 이벤트가 발생한 CPU 번호
}

// AgentEvent: Cilium 에이전트 이벤트
type AgentEvent struct {
    Type    int   // monitorAPI.MessageType* 값
    Message any   // LogRecord, AgentNotifyMessage 등
}

// LostEvent: 유실된 이벤트
type LostEvent struct {
    Source        int       // 유실 위치 (PerfRingBuffer, EventsQueue, HubbleRingBuffer)
    NumLostEvents uint64   // 유실된 이벤트 수
    CPU           int      // CPU 번호 (perf ring buffer의 경우)
    First         time.Time // 첫 유실 시각
    Last          time.Time // 마지막 유실 시각
}
```

---

## 데이터 흐름 요약

```
eBPF Program
    |
    v
Perf Ring Buffer (커널)
    |
    v
MonitorAgent (Cilium)
    |  MonitorEvent { UUID, Timestamp, NodeName,
    |                 Payload: PerfEvent{Data, CPU} }
    v
LocalObserverServer.events (채널)
    |
    v
Parser.Decode(monitorEvent)
    |  Data[0]으로 타입 분기:
    |    MessageTypeDebug  -> debug.Parser  -> DebugEvent
    |    MessageTypeSock   -> sock.Parser   -> Flow
    |    기타 (Trace/Drop) -> l34.Parser    -> Flow
    |    MessageTypeAccess -> l7.Parser     -> Flow (HTTP/DNS/Kafka)
    |    MessageTypeAgent  -> AgentEvent
    |    LostEvent         -> LostEvent
    v
v1.Event { Timestamp, Event: Flow|AgentEvent|DebugEvent|LostEvent }
    |
    v
Ring Buffer (container.Ring)
    |
    v
GetFlows (gRPC)
    |  Ring Reader -> Filter (whitelist/blacklist) -> FieldMask
    v
GetFlowsResponse { Flow | NodeStatus | LostEvents }
```

---

## AgentEvent 타입

AgentEvent는 Cilium 에이전트의 상태 변경을 표현한다.

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 819-835)
enum AgentEventType {
    AGENT_EVENT_UNKNOWN = 0;
    AGENT_STARTED = 2;
    POLICY_UPDATED = 3;
    POLICY_DELETED = 4;
    ENDPOINT_REGENERATE_SUCCESS = 5;
    ENDPOINT_REGENERATE_FAILURE = 6;
    ENDPOINT_CREATED = 7;
    ENDPOINT_DELETED = 8;
    IPCACHE_UPSERTED = 9;
    IPCACHE_DELETED = 10;
}
```

### AgentEvent 메시지

```protobuf
message AgentEvent {
    AgentEventType type = 1;
    oneof notification {
        AgentEventUnknown unknown = 100;
        TimeNotification agent_start = 101;           // 에이전트 시작 시각
        PolicyUpdateNotification policy_update = 102;  // 정책 변경 (labels, revision, rule_count)
        EndpointRegenNotification endpoint_regenerate = 103;  // 엔드포인트 재생성
        EndpointUpdateNotification endpoint_update = 104;     // 엔드포인트 생성/삭제
        IPCacheNotification ipcache_update = 105;             // IP 캐시 변경
    }
}
```

---

## LostEvent

유실 이벤트는 Hubble 파이프라인에서 이벤트가 손실되었음을 클라이언트에 알린다.

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (Line 801-815)
message LostEvent {
    LostEventSource source = 1;     // 유실 위치
    uint64 num_events_lost = 2;     // 유실된 수
    Int32Value cpu = 3;             // CPU 번호 (perf인 경우)
    Timestamp first = 4;            // 첫 유실 시각
    Timestamp last = 5;             // 마지막 유실 시각
}

enum LostEventSource {
    UNKNOWN_LOST_EVENT_SOURCE = 0;
    PERF_EVENT_RING_BUFFER = 1;     // BPF perf ring buffer 오버런
    OBSERVER_EVENTS_QUEUE = 2;      // Observer 이벤트 큐 풀
    HUBBLE_RING_BUFFER = 3;         // Hubble Ring Buffer 덮어쓰기
}
```

---

## ExportEvent

Flow Export를 위한 통합 이벤트 메시지이다.

```protobuf
// 소스: cilium/api/v1/observer/observer.proto (Line 283-302)
message ExportEvent {
    oneof response_types {
        flow.Flow flow = 1;
        relay.NodeStatusEvent node_status = 2;
        flow.LostEvent lost_events = 3;
        flow.AgentEvent agent_event = 4;
        flow.DebugEvent debug_event = 5;
    }
    string node_name = 1000;
    google.protobuf.Timestamp time = 1001;
}
```

---

## 왜 이런 데이터 모델인가?

### 1. 왜 Protobuf인가?
- 바이너리 직렬화로 gRPC 전송 효율 극대화
- 스키마 정의로 타입 안전성 보장
- 언어 중립적이며 코드 자동 생성
- FieldMask로 필요한 필드만 선택적으로 전송 가능

### 2. 왜 Flow에 40+ 필드인가?
- eBPF가 커널 레벨에서 수집하는 풍부한 정보를 손실 없이 표현
- L2/L3/L4/L7 전 계층을 단일 메시지로 통합
- Cilium 내부 메타데이터(identity, policy, endpoint)를 네트워크 이벤트에 바인딩

### 3. 왜 FlowFilter가 별도 메시지인가?
- 서버 측에서 필터링하여 네트워크 대역폭 절약
- 복잡한 필터 조합 (whitelist OR + blacklist NOR)을 표현
- 25+ 필터 타입으로 세밀한 조건 지정

### 4. 왜 v1.Event에 any 타입인가?
- Flow, AgentEvent, DebugEvent, LostEvent를 단일 Ring Buffer에 저장
- Go의 타입 어서션으로 런타임에 안전하게 분기
- 새로운 이벤트 타입 추가가 용이
