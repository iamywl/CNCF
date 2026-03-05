# 14. gRPC API (프로토콜 버퍼 & 서비스 정의)

## 개요

Hubble의 gRPC API는 네트워크 플로우 관찰, 에이전트 이벤트 모니터링, 피어 디스커버리,
릴레이 상태 관리를 위한 통합 인터페이스를 제공한다. 이 API는 네 개의 Proto 파일로
정의되며, Hubble의 모든 구성 요소 간 통신의 기반이 된다.

이 문서에서는 각 프로토콜 버퍼 정의를 상세히 분석하고, 메시지 구조, 필터링 메커니즘,
서비스 RPC 정의를 소스코드 수준에서 설명한다.

## Proto 파일 구조

```
cilium/api/v1/
├── flow/
│   └── flow.proto           # Flow 메시지, 필터, 이벤트 타입 정의
├── observer/
│   └── observer.proto       # Observer 서비스 (GetFlows, ServerStatus 등)
├── peer/
│   └── peer.proto           # Peer 서비스 (Notify)
└── relay/
    └── relay.proto          # Relay 확장 (NodeStatusEvent, NodeState)
```

### 의존성 관계

```
observer.proto
    |
    +-- import "flow/flow.proto"          (Flow, FlowFilter 등)
    +-- import "relay/relay.proto"        (NodeStatusEvent, NodeState)
    +-- import "google/protobuf/*.proto"  (Timestamp, FieldMask 등)

peer.proto
    (독립적, 외부 의존 없음)

relay.proto
    (독립적, 외부 의존 없음)
```

## Observer 서비스

### 서비스 정의

```protobuf
// 소스: cilium/api/v1/observer/observer.proto

service Observer {
    // 구조화된 플로우 데이터 스트리밍
    rpc GetFlows(GetFlowsRequest) returns (stream GetFlowsResponse) {}

    // Cilium 에이전트 이벤트 스트리밍
    rpc GetAgentEvents(GetAgentEventsRequest) returns (stream GetAgentEventsResponse) {}

    // Cilium 데이터패스 디버그 이벤트 스트리밍
    rpc GetDebugEvents(GetDebugEventsRequest) returns (stream GetDebugEventsResponse) {}

    // 클러스터 노드 정보 조회
    rpc GetNodes(GetNodesRequest) returns (GetNodesResponse) {}

    // 네임스페이스 목록 조회 (최근 1시간 내 플로우가 있는)
    rpc GetNamespaces(GetNamespacesRequest) returns (GetNamespacesResponse) {}

    // Hubble 서버 상태 조회
    rpc ServerStatus(ServerStatusRequest) returns (ServerStatusResponse) {}
}
```

### RPC 유형 분류

| RPC | 유형 | 설명 |
|-----|------|------|
| GetFlows | Server Streaming | 플로우 이벤트 실시간 스트리밍 |
| GetAgentEvents | Server Streaming | 에이전트 이벤트 실시간 스트리밍 |
| GetDebugEvents | Server Streaming | 디버그 이벤트 실시간 스트리밍 |
| GetNodes | Unary | 단일 요청-응답 (노드 목록) |
| GetNamespaces | Unary | 단일 요청-응답 (네임스페이스 목록) |
| ServerStatus | Unary | 단일 요청-응답 (서버 상태) |

### GetFlowsRequest

```protobuf
// 소스: cilium/api/v1/observer/observer.proto

message GetFlowsRequest {
    // 반환할 플로우 수. since/until과 비호환.
    uint64 number = 1;

    // true면 가장 오래된 number개, false면 최신 number개 반환. follow와 비호환.
    bool first = 9;

    // true면 마지막 N개 출력 후 새 플로우를 계속 스트리밍
    bool follow = 3;

    // 블랙리스트: 하나라도 매치하면 제외
    repeated flow.FlowFilter blacklist = 5;

    // 화이트리스트: 하나라도 매치하면 포함
    // 결과 = whitelist - blacklist
    repeated flow.FlowFilter whitelist = 6;

    // 시작 시간. number와 비호환
    google.protobuf.Timestamp since = 7;

    // 종료 시간. number와 비호환
    google.protobuf.Timestamp until = 8;

    // 반환할 필드를 제한하는 FieldMask
    google.protobuf.FieldMask field_mask = 10;

    // 실험적 기능
    message Experimental {
        reserved 1;  // field_mask가 외부로 이동됨
    }
    Experimental experimental = 999;

    // 임의 추가 메타데이터
    google.protobuf.Any extensions = 150000;
}
```

#### 필터링 동작

```
플로우 이벤트 발생
    |
    +-- whitelist가 비어있지 않으면:
    |       whitelist 중 하나라도 매치해야 통과
    |
    +-- blacklist가 비어있지 않으면:
    |       blacklist 중 하나라도 매치하면 제외
    |
    +-- 최종 결과 = whitelist 매치 AND NOT blacklist 매치
```

#### 매개변수 호환성

| 조합 | 허용 여부 | 설명 |
|------|-----------|------|
| number + since | 비호환 | 둘 다 범위를 지정 |
| number + until | 비호환 | 둘 다 범위를 지정 |
| first + follow | 비호환 | 방향 충돌 |
| follow + since | 허용 | 특정 시점 이후부터 계속 |
| whitelist + blacklist | 허용 | 차집합 연산 |

### GetFlowsResponse

```protobuf
// 소스: cilium/api/v1/observer/observer.proto

message GetFlowsResponse {
    oneof response_types {
        flow.Flow flow = 1;
        relay.NodeStatusEvent node_status = 2;
        flow.LostEvent lost_events = 3;
    }
    string node_name = 1000;
    google.protobuf.Timestamp time = 1001;
}
```

응답에는 세 가지 유형이 올 수 있다:

| 유형 | 설명 | 발생 시점 |
|------|------|-----------|
| Flow | 실제 네트워크 플로우 이벤트 | 패킷/연결 처리 시 |
| NodeStatusEvent | Relay 노드 상태 변경 | 노드 연결/해제 시 |
| LostEvent | 이벤트 손실 알림 | 버퍼 오버플로우 시 |

### ServerStatusResponse

```protobuf
// 소스: cilium/api/v1/observer/observer.proto

message ServerStatusResponse {
    uint64 num_flows = 1;                              // 현재 캡처된 플로우 수
    uint64 max_flows = 2;                              // 링 버퍼 최대 용량
    uint64 seen_flows = 3;                             // 시작 이후 총 관찰 플로우 수
    uint64 uptime_ns = 4;                              // 가동 시간 (나노초)
    google.protobuf.UInt32Value num_connected_nodes = 5;   // 연결된 노드 수
    google.protobuf.UInt32Value num_unavailable_nodes = 6; // 사용불가 노드 수
    repeated string unavailable_nodes = 7;             // 사용불가 노드 목록
    string version = 8;                                // Cilium/Hubble 버전
    double flows_rate = 9;                             // 최근 1분간 초당 플로우 비율
}
```

멀티노드 환경(Relay 경유)에서는 각 값이 모든 노드의 합산이다.

### GetNodes / GetNamespaces

```protobuf
// 소스: cilium/api/v1/observer/observer.proto

message Node {
    string name = 1;
    string version = 2;
    string address = 3;
    relay.NodeState state = 4;
    TLS tls = 5;
    uint64 uptime_ns = 6;
    uint64 num_flows = 7;
    uint64 max_flows = 8;
    uint64 seen_flows = 9;
}

message Namespace {
    string cluster = 1;
    string namespace = 2;
}
```

### ExportEvent

```protobuf
// 소스: cilium/api/v1/observer/observer.proto

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

ExportEvent는 Hubble 익스포터가 내부적으로 사용하는 통합 이벤트 메시지이다.
GetFlowsResponse와 달리 AgentEvent와 DebugEvent도 포함한다.

## Flow 메시지 상세 구조

### 핵심 필드 구성

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

message Flow {
    // 시간 & 식별
    google.protobuf.Timestamp time = 1;
    string uuid = 34;
    Emitter emitter = 41;

    // 판정 & 드롭
    Verdict verdict = 2;
    DropReason drop_reason_desc = 25;
    AuthType auth_type = 35;

    // 네트워크 계층
    Ethernet ethernet = 4;       // L2
    IP IP = 5;                   // L3
    Layer4 l4 = 6;               // L4
    Tunnel tunnel = 39;          // 터널링

    // 엔드포인트
    Endpoint source = 8;
    Endpoint destination = 9;

    // 서비스
    Service source_service = 20;
    Service destination_service = 21;

    // L7 (HTTP/DNS/Kafka)
    Layer7 l7 = 15;

    // 메타데이터
    FlowType Type = 10;
    string node_name = 11;
    repeated string node_labels = 37;
    TrafficDirection traffic_direction = 22;

    // 이름 해석
    repeated string source_names = 13;
    repeated string destination_names = 14;

    // 추적 & 디버그
    CiliumEventType event_type = 19;
    TraceObservationPoint trace_observation_point = 24;
    TraceReason trace_reason = 36;
    TraceContext trace_context = 30;
    FileInfo file = 38;
    IPTraceID ip_trace_id = 40;

    // 정책
    repeated Policy egress_allowed_by = 21001;
    repeated Policy ingress_allowed_by = 21002;
    repeated Policy egress_denied_by = 21004;
    repeated Policy ingress_denied_by = 21005;
    repeated string policy_log = 21006;

    // 집계
    Aggregate aggregate = 21007;

    // 소켓 관련
    SocketTranslationPoint sock_xlate_point = 31;
    uint64 socket_cookie = 32;
    uint64 cgroup_id = 33;

    // 기타
    google.protobuf.BoolValue is_reply = 26;
    DebugCapturePoint debug_capture_point = 27;
    NetworkInterface interface = 28;
    uint32 proxy_port = 29;

    // 확장
    google.protobuf.Any extensions = 150000;
}
```

### 필드 카테고리별 분류

```
Flow 메시지 (40+ 필드)
    |
    +-- 식별/시간 -------- time, uuid, emitter
    |
    +-- 판정 ------------- verdict, drop_reason_desc, auth_type
    |
    +-- 네트워크 계층
    |   +-- L2 ----------- ethernet (src/dst MAC)
    |   +-- L3 ----------- IP (src/dst IP, version, encrypted)
    |   +-- L4 ----------- l4 (TCP/UDP/SCTP/ICMPv4/ICMPv6/VRRP/IGMP)
    |   +-- 터널 --------- tunnel (protocol, IP, l4, vni)
    |
    +-- 엔드포인트 ------- source, destination (ID, identity, labels, pod)
    |
    +-- 서비스 ----------- source_service, destination_service
    |
    +-- L7 --------------- l7 (HTTP/DNS, type, latency_ns)
    |
    +-- 정책 ------------- egress/ingress allowed/denied by, policy_log
    |
    +-- 추적 ------------- event_type, trace_observation_point, trace_reason
    |
    +-- 메타 ------------- node_name, traffic_direction, is_reply
```

### Verdict (판정)

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

enum Verdict {
    VERDICT_UNKNOWN = 0;
    FORWARDED = 1;    // 다음 처리 엔티티로 전달됨
    DROPPED = 2;      // 연결/패킷이 드롭됨
    ERROR = 3;        // 처리 중 에러 발생
    AUDIT = 4;        // 감사 모드에서 드롭될 뻔한 플로우
    REDIRECTED = 5;   // 프록시로 리다이렉트됨
    TRACED = 6;       // 추적 포인트에서 관찰됨 (판정 미결)
    TRANSLATED = 7;   // 주소가 변환됨
}
```

### Layer4 프로토콜

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

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

message TCP {
    uint32 source_port = 1;
    uint32 destination_port = 2;
    TCPFlags flags = 3;
}

message TCPFlags {
    bool FIN = 1;
    bool SYN = 2;
    bool RST = 3;
    bool PSH = 4;
    bool ACK = 5;
    bool URG = 6;
    bool ECE = 7;
    bool CWR = 8;
    bool NS = 9;
}
```

### Layer7 프로토콜

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

message Layer7 {
    L7FlowType type = 1;       // REQUEST / RESPONSE / SAMPLE
    uint64 latency_ns = 2;     // 응답 지연 시간
    oneof record {
        DNS dns = 100;
        HTTP http = 101;
        Kafka kafka = 102;      // deprecated
    }
}

message DNS {
    string query = 1;           // 조회 도메인명 (예: "isovalent.com.")
    repeated string ips = 2;    // 응답 IP 목록
    uint32 ttl = 3;
    repeated string cnames = 4;
    string observation_source = 5;
    uint32 rcode = 6;           // DNS 반환 코드
    repeated string qtypes = 7; // 쿼리 타입 (A, AAAA 등)
    repeated string rrtypes = 8;
}

message HTTP {
    uint32 code = 1;            // HTTP 상태 코드
    string method = 2;          // GET, POST 등
    string url = 3;
    string protocol = 4;
    repeated HTTPHeader headers = 5;
}
```

### Endpoint (엔드포인트)

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

message Endpoint {
    uint32 ID = 1;                     // Cilium 엔드포인트 ID
    uint32 identity = 2;              // 보안 아이덴티티
    string cluster_name = 7;          // 클러스터 이름
    string namespace = 3;             // K8s 네임스페이스
    repeated string labels = 4;       // "foo=bar" 형식 레이블
    string pod_name = 5;              // Pod 이름
    repeated Workload workloads = 6;  // 워크로드 (Deployment 등)
}

message Workload {
    string name = 1;
    string kind = 2;   // "Deployment", "DaemonSet" 등
}
```

### DropReason (드롭 사유)

```protobuf
// 소스: cilium/api/v1/flow/flow.proto (요약)

enum DropReason {
    DROP_REASON_UNKNOWN = 0;
    INVALID_SOURCE_IP = 132;
    POLICY_DENIED = 133;
    INVALID_PACKET_DROPPED = 134;
    // ... (80+ 드롭 사유)
    UNSUPPORTED_L3_PROTOCOL = 139;
    MISSED_TAIL_CALL = 140;
    UNKNOWN_L4_PROTOCOL = 142;
    SERVICE_BACKEND_NOT_FOUND = 158;
    FIB_LOOKUP_FAILED = 169;
    AUTH_REQUIRED = 189;
    TTL_EXCEEDED = 196;
    DROP_RATE_LIMITED = 198;
    DROP_HOST_NOT_READY = 202;
    DROP_EP_NOT_READY = 203;
    DROP_NO_EGRESS_IP = 204;
    DROP_PUNT_PROXY = 205;
    DROP_NO_DEVICE = 206;
}
```

eBPF 데이터패스의 드롭 사유가 80개 이상 정의되어 있다. 이 값들은
`pkg/monitor/api/drop.go`와 `bpf/lib/common.h`에서 공유된다.

### TraceObservationPoint

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

enum TraceObservationPoint {
    UNKNOWN_POINT = 0;
    TO_PROXY = 1;           // L7 프록시로 전송
    TO_HOST = 2;            // 호스트 네임스페이스로 전송
    TO_STACK = 3;           // 리눅스 커널 네트워크 스택으로 전송
    TO_OVERLAY = 4;         // 터널 디바이스로 전송
    TO_ENDPOINT = 101;      // 컨테이너로 전송
    FROM_ENDPOINT = 5;      // 컨테이너에서 수신
    FROM_PROXY = 6;         // L7 프록시에서 수신
    FROM_HOST = 7;          // 호스트 네임스페이스에서 수신
    FROM_STACK = 8;         // 리눅스 커널 스택에서 수신
    FROM_OVERLAY = 9;       // 터널 디바이스에서 수신
    FROM_NETWORK = 10;      // 네이티브 디바이스에서 수신
    TO_NETWORK = 11;        // 네이티브 디바이스로 전송
    FROM_CRYPTO = 12;       // 복호화 프로세스에서 수신
    TO_CRYPTO = 13;         // 암호화 프로세스로 전송
}
```

### 패킷 경로 시각화

```
                    FROM_NETWORK
                         |
                         v
+------------------------------------------------------+
|                  eBPF Datapath                        |
|                                                       |
|  FROM_ENDPOINT  +---------+  TO_PROXY                |
|  <------------- | Routing | ------------>            |
|                 +---------+                           |
|       |              |              |                 |
|  TO_ENDPOINT    TO_OVERLAY    TO_STACK                |
|       |              |              |                 |
|       v              v              v                 |
|  [Container]    [Tunnel]     [Host Stack]            |
+------------------------------------------------------+
                         |
                         v
                    TO_NETWORK
```

## FlowFilter 상세

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

message FlowFilter {
    // UUID 필터
    repeated string uuid = 29;

    // 소스 기반 필터
    repeated string source_ip = 1;              // "1.1.1.1" 또는 "1.1.1.0/24"
    repeated string source_ip_xlated = 34;      // SNAT 이후 소스 IP
    repeated string source_pod = 2;             // "xwing" 또는 "kube-system/coredns-"
    repeated string source_fqdn = 7;
    repeated string source_label = 10;          // K8s 레이블 셀렉터
    repeated string source_service = 16;
    repeated Workload source_workload = 26;
    repeated string source_cluster_name = 37;

    // 목적지 기반 필터
    repeated string destination_ip = 3;
    repeated string destination_pod = 4;
    repeated string destination_fqdn = 8;
    repeated string destination_label = 11;
    repeated string destination_service = 17;
    repeated Workload destination_workload = 27;
    repeated string destination_cluster_name = 38;

    // 트래픽 방향 & 판정
    repeated TrafficDirection traffic_direction = 30;
    repeated Verdict verdict = 5;
    repeated DropReason drop_reason_desc = 33;

    // 인터페이스 & 이벤트 타입
    repeated NetworkInterface interface = 35;
    repeated EventTypeFilter event_type = 6;

    // HTTP 필터
    repeated string http_status_code = 9;       // "4+", "404", "5+"
    repeated string http_method = 21;           // "GET", "POST"
    repeated string http_path = 22;             // 정규식
    repeated string http_url = 31;              // 정규식
    repeated HTTPHeader http_header = 32;       // key:value 쌍

    // L4 필터
    repeated string protocol = 12;              // "tcp", "http"
    repeated string source_port = 13;
    repeated string destination_port = 14;

    // DNS 필터
    repeated string dns_query = 18;             // RE2 정규식

    // 아이덴티티 & 노드
    repeated uint32 source_identity = 19;
    repeated uint32 destination_identity = 20;
    repeated string node_name = 24;             // "k8s*", "cluster/"
    repeated string node_labels = 36;

    // 기타
    repeated bool reply = 15;
    repeated TCPFlags tcp_flags = 23;
    repeated IPVersion ip_version = 25;
    repeated string trace_id = 28;
    repeated uint64 ip_trace_id = 39;
    repeated bool encrypted = 40;

    // 실험적: CEL 표현식
    message Experimental {
        repeated string cel_expression = 1;
    }
    Experimental experimental = 999;
}
```

### 필터 필드 수 통계

| 카테고리 | 필드 수 | 예시 |
|----------|---------|------|
| 소스 기반 | 8 | source_ip, source_pod, source_label |
| 목적지 기반 | 7 | destination_ip, destination_pod |
| HTTP | 5 | http_status_code, http_method, http_path |
| L4 | 3 | protocol, source_port, destination_port |
| 판정/방향 | 3 | verdict, drop_reason_desc, traffic_direction |
| 추적/식별 | 5 | uuid, trace_id, ip_trace_id, node_name |
| 기타 | 5 | reply, tcp_flags, ip_version, encrypted |
| **합계** | **36+** | |

### 필터 매칭 규칙

```
FlowFilter 내부 필드들은 AND 관계:
    source_ip = "10.0.0.0/8"
    AND destination_port = "80"
    AND verdict = DROPPED
    -> 10.0.0.0/8에서 80 포트로 가는 드롭된 플로우만 매치

같은 필드 내 여러 값은 OR 관계:
    verdict = [DROPPED, ERROR]
    -> DROPPED 또는 ERROR인 플로우 매치

whitelist의 여러 FlowFilter는 OR 관계:
    whitelist = [filter1, filter2]
    -> filter1 OR filter2 매치
```

### CEL 표현식 (실험적)

```protobuf
// FlowFilter.Experimental
message Experimental {
    // CEL 표현식으로 Flow 필드에 접근
    // 변수명: _flow
    // 예: _flow.source.namespace == "production"
    repeated string cel_expression = 1;
}
```

CEL(Common Expression Language)은 다른 필터보다 성능 비용이 높으므로
가능하면 기본 필터를 먼저 사용하고, CEL은 마지막에 배치하는 것을 권장한다.

## 이벤트 메시지

### AgentEvent

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

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

message AgentEvent {
    AgentEventType type = 1;
    oneof notification {
        AgentEventUnknown unknown = 100;
        TimeNotification agent_start = 101;
        PolicyUpdateNotification policy_update = 102;
        EndpointRegenNotification endpoint_regenerate = 103;
        EndpointUpdateNotification endpoint_update = 104;
        IPCacheNotification ipcache_update = 105;
    }
}
```

에이전트 이벤트 유형별 알림 데이터:

| 이벤트 | 알림 메시지 | 주요 필드 |
|--------|-------------|-----------|
| AGENT_STARTED | TimeNotification | 시작 시간 |
| POLICY_UPDATED/DELETED | PolicyUpdateNotification | labels, revision, rule_count |
| ENDPOINT_REGENERATE_* | EndpointRegenNotification | id, labels, error |
| ENDPOINT_CREATED/DELETED | EndpointUpdateNotification | id, labels, pod_name, namespace |
| IPCACHE_UPSERTED/DELETED | IPCacheNotification | cidr, identity, host_ip |

### LostEvent

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

enum LostEventSource {
    UNKNOWN_LOST_EVENT_SOURCE = 0;
    PERF_EVENT_RING_BUFFER = 1;   // BPF perf 이벤트 링 버퍼 드롭
    OBSERVER_EVENTS_QUEUE = 2;    // Hubble 이벤트 큐 가득 참
    HUBBLE_RING_BUFFER = 3;       // Hubble 링 버퍼에서 덮어씀
}

message LostEvent {
    LostEventSource source = 1;
    uint64 num_events_lost = 2;
    google.protobuf.Int32Value cpu = 3;
    google.protobuf.Timestamp first = 4;
    google.protobuf.Timestamp last = 5;
}
```

이벤트 손실 경로:

```
eBPF 프로그램 -> BPF Perf Ring Buffer -> Cilium Agent -> Hubble Observer Queue -> Ring Buffer
                       ^                       ^                    ^                  ^
                       |                       |                    |                  |
              PERF_EVENT_RING_BUFFER   (에이전트 레벨)    OBSERVER_EVENTS_QUEUE  HUBBLE_RING_BUFFER
```

### DebugEvent

```protobuf
// 소스: cilium/api/v1/flow/flow.proto

message DebugEvent {
    DebugEventType type = 1;
    Endpoint source = 2;
    google.protobuf.UInt32Value hash = 3;
    google.protobuf.UInt32Value arg1 = 4;
    google.protobuf.UInt32Value arg2 = 5;
    google.protobuf.UInt32Value arg3 = 6;
    string message = 7;
    google.protobuf.Int32Value cpu = 8;
}
```

DebugEventType은 68개의 디버그 이벤트 유형을 정의한다 (DBG_GENERIC부터
DBG_LB6_LOOPBACK_SNAT_REV까지).

## Peer 서비스

```protobuf
// 소스: cilium/api/v1/peer/peer.proto

service Peer {
    rpc Notify(NotifyRequest) returns (stream ChangeNotification) {}
}

message ChangeNotification {
    string name = 1;           // 피어 이름 (예: "cluster/node1")
    string address = 2;        // gRPC 서비스 주소 (예: "10.0.0.1:4244")
    ChangeNotificationType type = 3;
    TLS tls = 4;
}

enum ChangeNotificationType {
    UNKNOWN = 0;
    PEER_ADDED = 1;
    PEER_DELETED = 2;
    PEER_UPDATED = 3;
}
```

(상세 내용은 13-peer-service.md 참고)

## Relay 확장

```protobuf
// 소스: cilium/api/v1/relay/relay.proto

message NodeStatusEvent {
    NodeState state_change = 1;
    repeated string node_names = 2;
    string message = 3;
}

enum NodeState {
    UNKNOWN_NODE_STATE = 0;
    NODE_CONNECTED = 1;       // 연결 수립됨, 플로우 수신 가능
    NODE_UNAVAILABLE = 2;     // 연결 불가, 플로우 수신 불가
    NODE_GONE = 3;            // 클러스터에서 제거됨, 재연결 없음
    NODE_ERROR = 4;           // 요청 처리 에러, 재연결 없음
}
```

### Relay 노드 상태 전이

```
              NODE_CONNECTED
                   |
          +--------+--------+
          |                 |
    NODE_UNAVAILABLE   NODE_ERROR
          |                 |
    NODE_CONNECTED    (종료 상태)
          |
     NODE_GONE
          |
    (종료 상태)
```

## FieldMask 지원

GetFlowsRequest에 FieldMask를 지정하면 서버는 해당 필드만 채워서 응답한다.

```protobuf
// 사용 예시
GetFlowsRequest {
    field_mask: {
        paths: ["source.id", "destination.id", "verdict"]
    }
}
```

이를 통해:
- **네트워크 대역폭 절약**: 불필요한 필드 전송 방지
- **처리 성능 향상**: 직렬화/역직렬화 비용 감소
- **메모리 절약**: 클라이언트 측 불필요한 데이터 저장 방지

## 필드 번호 규약

| 범위 | 용도 |
|------|------|
| 1-999 | 표준 필드 |
| 1000-1999 | 응답 메타데이터 (node_name, time) |
| 21001-21006 | 정책 관련 확장 필드 |
| 100000 | deprecated (Summary) |
| 150000 | extensions (Any) |
| 999 | experimental |

높은 필드 번호(21001+, 100000, 150000)를 사용하는 이유는 향후 표준 필드 확장과의
충돌을 방지하기 위함이다.

## 정리

Hubble gRPC API는 다음 설계 원칙을 따른다:

1. **서버 스트리밍 중심**: 실시간 이벤트 관찰을 위한 기본 패턴
2. **풍부한 필터링**: 36+ 필터 필드로 정밀한 이벤트 선택
3. **oneof를 통한 다형성**: 하나의 응답 메시지에 여러 이벤트 유형 수용
4. **확장 가능성**: extensions(Any), experimental, 높은 필드 번호
5. **후방 호환성**: reserved 필드, deprecated 마킹으로 API 진화 관리

### 파일 참조

| 파일 | 경로 | 설명 |
|------|------|------|
| Observer Proto | `cilium/api/v1/observer/observer.proto` | Observer 서비스 및 요청/응답 |
| Flow Proto | `cilium/api/v1/flow/flow.proto` | Flow 메시지, 필터, 이벤트 |
| Peer Proto | `cilium/api/v1/peer/peer.proto` | Peer 디스커버리 서비스 |
| Relay Proto | `cilium/api/v1/relay/relay.proto` | Relay 노드 상태 |
