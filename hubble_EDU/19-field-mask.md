# 19. Field Mask (필드 마스크)

## 1. 개요

Hubble의 **Field Mask**는 `GetFlows` API 응답에서 반환되는 Flow 필드를 클라이언트가 선택적으로
제한할 수 있게 하는 기능이다. Google의 `google.protobuf.FieldMask`를 활용하여,
대규모 네트워크 관측 환경에서 불필요한 필드 전송을 제거하고 대역폭과 파싱 비용을 절감한다.

이 기능은 처음에 `Experimental` 네스티드 메시지의 필드로 도입되었으며,
안정화 이후 `GetFlowsRequest`의 최상위 `field_mask` 필드로 승격되었다.
기존 `experimental.field_mask`는 deprecated되어 v1.17에서 제거 예정이다.

### 핵심 소스 파일

| 파일 | 역할 |
|------|------|
| `api/v1/observer/observer.proto` | GetFlowsRequest에 field_mask 정의 |
| `hubble/cmd/observe/observe.go` | `--experimental-field-mask` CLI 플래그 |
| `hubble/cmd/observe/flows.go` | field_mask를 GetFlowsRequest에 설정 |
| `hubble/pkg/defaults/defaults.go` | 기본 FieldMask 경로 목록 (FieldMask 변수) |
| `google.golang.org/protobuf/types/known/fieldmaskpb` | FieldMask protobuf 구현 |

---

## 2. Protobuf 정의

### 2.1 GetFlowsRequest의 field_mask 필드

소스: `api/v1/observer/observer.proto`

```protobuf
message GetFlowsRequest {
    uint64 number = 1;
    bool first = 9;
    bool follow = 3;
    repeated flow.FlowFilter blacklist = 5;
    repeated flow.FlowFilter whitelist = 6;
    google.protobuf.Timestamp since = 7;
    google.protobuf.Timestamp until = 8;

    // FieldMask allows clients to limit flow's fields that will be returned.
    // For example, {paths: ["source.id", "destination.id"]} will return flows
    // with only these two fields set.
    google.protobuf.FieldMask field_mask = 10;

    message Experimental {
        // Deprecated in favor of top-level field_mask.
        // This field will be removed in v1.17.
        google.protobuf.FieldMask field_mask = 1 [deprecated=true];
    }
    Experimental experimental = 999;
}
```

### 2.2 google.protobuf.FieldMask

```protobuf
// google/protobuf/field_mask.proto
message FieldMask {
    // 요청할 필드 경로 목록
    // 예: ["source.id", "destination.namespace", "verdict"]
    repeated string paths = 1;
}
```

FieldMask의 `paths`는 protobuf 메시지의 필드 경로를 점(`.`) 구분자로 나타낸다.
중첩된 메시지의 필드를 선택하려면 `source.id`, `destination.pod_name`처럼 사용한다.

### 2.3 왜 최상위로 이동했나

처음 `experimental.field_mask`로 도입한 이유:
1. **안정성 검증**: API 변경의 영향도를 실험적으로 확인
2. **하위 호환성**: 기존 클라이언트에 영향 없이 신규 기능 추가
3. **피드백 수집**: 커뮤니티 피드백 후 안정화 여부 결정

최상위로 이동한 이유:
1. **API 간결성**: `experimental` 래퍼 없이 직접 접근 가능
2. **안정화 완료**: 충분한 실사용 검증 후 안정 필드로 승격
3. **Deprecated 경로**: 기존 코드의 마이그레이션 시간 확보 (v1.17까지)

---

## 3. CLI 인터페이스

### 3.1 CLI 플래그 정의

소스: `hubble/cmd/observe/observe.go`

```go
var experimentalOpts struct {
    fieldMask       []string
    useDefaultMasks bool
}

func init() {
    otherFlags.StringSliceVar(&experimentalOpts.fieldMask,
        "experimental-field-mask", nil,
        "Field mask to apply to flows returned by the server")
    otherFlags.BoolVar(&experimentalOpts.useDefaultMasks,
        "experimental-use-default-field-masks", false,
        "Use default field masks")
}
```

두 가지 CLI 플래그가 제공된다:

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--experimental-field-mask` | nil | 사용자 지정 필드 경로 목록 |
| `--experimental-use-default-field-masks` | false | 기본 필드 마스크 사용 여부 |

### 3.2 사용 예시

```bash
# 특정 필드만 요청
hubble observe --experimental-field-mask source.pod_name,destination.pod_name,verdict

# 기본 필드 마스크 사용
hubble observe --experimental-use-default-field-masks -o json

# JSON 출력과 함께 사용 (JSON/JSONPB에서만 커스텀 마스크 가능)
hubble observe --experimental-field-mask source.id,destination.id -o json
```

### 3.3 출력 형식 호환성

소스: `hubble/cmd/observe/flows.go`

```go
if !jsonOut {
    if len(experimentalOpts.fieldMask) > 0 {
        return fmt.Errorf("%s output format is not compatible with custom field mask",
            formattingOpts.output)
    }
    if experimentalOpts.useDefaultMasks {
        experimentalOpts.fieldMask = defaults.FieldMask
    }
}
```

**핵심 제약**: 커스텀 field mask는 JSON/JSONPB 출력에서만 사용할 수 있다.
`compact`, `dict`, `tab` 출력 형식에서는 기본 필드 마스크(`defaults.FieldMask`)만 자동 적용된다.

| 출력 형식 | 커스텀 마스크 | 기본 마스크 | 비고 |
|----------|------------|-----------|------|
| `json` | O | O | 모든 마스크 사용 가능 |
| `jsonpb` | O | O | Protobuf JSON 형식 |
| `compact` | X | O (`--experimental-use-default-field-masks`) | 기본 마스크만 |
| `dict` | X | O | 기본 마스크만 |
| `tab` | X | O | 기본 마스크만 |

---

## 4. 기본 Field Mask

### 4.1 defaults.FieldMask

소스: `hubble/pkg/defaults/defaults.go`

```go
var FieldMask = []string{
    "time",
    "source.identity",
    "source.namespace",
    "source.pod_name",
    "destination.identity",
    "destination.namespace",
    "destination.pod_name",
    "source_service",
    "destination_service",
    "l4",
    "IP",
    "ethernet",
    "l7",
    "Type",
    "node_name",
    "is_reply",
    "event_type",
    "verdict",
    "Summary",
}
```

### 4.2 필드별 분류

```
┌──────────────────────────────────────────────────────────────┐
│                    기본 Field Mask 경로                        │
│                                                               │
│  ┌─────────────────┐  ┌─────────────────┐  ┌──────────────┐ │
│  │  소스 정보       │  │  목적지 정보     │  │  네트워크     │ │
│  │  source.identity │  │  destination.*  │  │  l4          │ │
│  │  source.namespace│  │  dest.identity  │  │  IP          │ │
│  │  source.pod_name │  │  dest.namespace │  │  ethernet    │ │
│  └─────────────────┘  │  dest.pod_name  │  │  l7          │ │
│                       └─────────────────┘  └──────────────┘ │
│  ┌─────────────────┐  ┌─────────────────┐  ┌──────────────┐ │
│  │  서비스 정보     │  │  메타데이터      │  │  판정        │ │
│  │  source_service  │  │  time           │  │  verdict     │ │
│  │  dest._service   │  │  node_name      │  │  Summary     │ │
│  │                  │  │  event_type     │  │  is_reply    │ │
│  │                  │  │  Type           │  │              │ │
│  └─────────────────┘  └─────────────────┘  └──────────────┘ │
└──────────────────────────────────────────────────────────────┘
```

### 4.3 왜 이 필드들인가

기본 마스크는 Hubble의 **표준 출력 형식**에서 표시하는 필드만 포함한다.
compact/dict/tab 형식에서는 이 필드들만으로 충분한 정보를 표시할 수 있다.

| 카테고리 | 필드 | 용도 |
|---------|------|------|
| 소스 식별 | `source.identity`, `source.namespace`, `source.pod_name` | 트래픽 발신자 식별 |
| 목적지 식별 | `destination.*` | 트래픽 수신자 식별 |
| 서비스 매핑 | `source_service`, `destination_service` | K8s 서비스 연결 |
| 네트워크 계층 | `l4`, `IP`, `ethernet`, `l7` | 프로토콜/포트/IP 정보 |
| 판정 | `verdict`, `Summary` | 트래픽 허용/차단 판정 |
| 메타데이터 | `time`, `node_name`, `event_type`, `Type`, `is_reply` | 이벤트 컨텍스트 |

**제외된 필드 (대역폭 절감 대상)**:
- `source.workloads`: 워크로드 상세 정보 (대부분 불필요)
- `traffic_direction`: 일부 형식에서만 사용
- `trace_context`: 분산 추적 컨텍스트 (디버깅 전용)
- `extensions`: 확장 필드
- 기타 세부 필드들

---

## 5. 요청 구성 흐름

### 5.1 GetFlowsRequest 생성

소스: `hubble/cmd/observe/flows.go`

```go
func getFlowsRequestWithRecordedDefaults() (*observerpb.GetFlowsRequest, error) {
    // ... (필터, since/until 등 설정)

    req := &observerpb.GetFlowsRequest{
        Number:    number,
        Follow:    selectorOpts.follow,
        Whitelist: wl,
        Blacklist: bl,
        Since:     since,
        Until:     until,
        First:     first,
    }

    if len(experimentalOpts.fieldMask) > 0 {
        fm, err := fieldmaskpb.New(&flowpb.Flow{}, experimentalOpts.fieldMask...)
        if err != nil {
            return nil, fmt.Errorf("failed to construct field mask: %w", err)
        }
        req.Experimental = &observerpb.GetFlowsRequest_Experimental{
            FieldMask: fm,
        }
    }

    return req, nil
}
```

### 5.2 흐름도

```
┌──────────────────────────────────────────────────────────────┐
│                   Field Mask 처리 흐름                         │
│                                                               │
│  CLI 입력                                                     │
│  ├── --experimental-field-mask=source.pod_name,verdict        │
│  │   └── experimentalOpts.fieldMask = ["source.pod_name",    │
│  │                                     "verdict"]             │
│  └── --experimental-use-default-field-masks                   │
│      └── experimentalOpts.useDefaultMasks = true              │
│                                                               │
│  출력 형식 결정                                                 │
│  ├── JSON/JSONPB → 커스텀 마스크 허용                           │
│  └── compact/dict/tab                                         │
│      ├── 커스텀 마스크 → 에러                                   │
│      └── useDefaultMasks → defaults.FieldMask 적용             │
│                                                               │
│  GetFlowsRequest 구성                                          │
│  ├── fieldmaskpb.New(&flowpb.Flow{}, paths...)                │
│  │   └── Flow 메시지 구조 기반 경로 검증                         │
│  └── req.Experimental.FieldMask = fm                          │
│                                                               │
│  서버 응답                                                     │
│  └── 요청된 필드만 설정된 Flow 메시지 반환                        │
│      (나머지 필드는 zero value)                                  │
└──────────────────────────────────────────────────────────────┘
```

### 5.3 fieldmaskpb.New() 검증

`fieldmaskpb.New()` 함수는 주어진 protobuf 메시지 타입을 기준으로 경로를 검증한다.

```go
fm, err := fieldmaskpb.New(&flowpb.Flow{}, "source.pod_name", "verdict")
```

이 함수는:
1. `flowpb.Flow` 메시지의 descriptor를 사용
2. 각 경로가 실제 존재하는 필드인지 확인
3. 중첩된 경로(`source.pod_name`)도 재귀적으로 검증
4. 유효하지 않은 경로가 있으면 에러 반환

---

## 6. 서버 측 처리

### 6.1 Field Mask 적용 원리

서버(Hubble Agent 또는 Hubble Relay)는 GetFlowsRequest의 `field_mask`를 받아
응답 Flow 메시지에서 해당 필드만 설정하고 나머지를 zero value로 남긴다.

```
원본 Flow 메시지:
{
    "time": "2024-01-01T00:00:00Z",
    "verdict": "FORWARDED",
    "ethernet": { "source": "aa:bb:cc:dd:ee:ff", ... },
    "IP": { "source": "10.0.0.1", "destination": "10.0.0.2", ... },
    "l4": { "TCP": { "source_port": 12345, "destination_port": 80 } },
    "source": {
        "id": 100,
        "identity": 12345,
        "namespace": "default",
        "pod_name": "frontend-abc",
        "workloads": [{ ... }],
        "labels": ["app=frontend", ...]
    },
    "destination": {
        "id": 200,
        "identity": 67890,
        "namespace": "default",
        "pod_name": "backend-xyz",
        ...
    },
    "Type": "L3_L4",
    "node_name": "k8s-node-1",
    "event_type": { "type": 4 },
    "Summary": "TCP Flags: SYN",
    "trace_context": { ... },
    "source_service": { "name": "frontend", "namespace": "default" },
    "destination_service": { "name": "backend", "namespace": "default" },
    "traffic_direction": "EGRESS",
    ...
}

field_mask: ["source.pod_name", "destination.pod_name", "verdict"]

필터링된 Flow:
{
    "verdict": "FORWARDED",
    "source": {
        "pod_name": "frontend-abc"
    },
    "destination": {
        "pod_name": "backend-xyz"
    }
}
```

### 6.2 protobuf 필드 마스킹 메커니즘

protobuf의 FieldMask는 `proto.Merge` + `fieldmaskpb.Filter`로 적용된다:

1. **경로 파싱**: "source.pod_name" → ["source", "pod_name"]
2. **트리 구성**: 경로를 트리 구조로 변환
3. **필터링**: 원본 메시지에서 마스크 트리에 포함된 필드만 복사
4. **zero value**: 마스크에 없는 필드는 설정하지 않음 (protobuf의 기본 동작)

### 6.3 대역폭 절감 효과

Flow 메시지의 전체 크기와 필드 마스크 적용 후 크기 비교:

| 시나리오 | 예상 크기 | 절감률 |
|---------|----------|--------|
| 마스크 없음 (전체) | ~500-1000 bytes | 0% |
| 기본 마스크 (19개 필드) | ~200-400 bytes | ~50-60% |
| 최소 마스크 (3개 필드) | ~50-100 bytes | ~85-90% |

대규모 환경(초당 수만 flows)에서는 이 절감이 유의미한 네트워크 대역폭 차이를 만든다.

---

## 7. Experimental에서 최상위로의 마이그레이션

### 7.1 Deprecated 경고

기존 코드:
```go
req.Experimental = &observerpb.GetFlowsRequest_Experimental{
    FieldMask: fm,  // deprecated
}
```

신규 코드:
```go
req.FieldMask = fm  // 최상위 필드
```

### 7.2 서버 측 하위 호환

서버는 두 필드를 모두 확인한다:
1. `req.FieldMask`가 설정되어 있으면 사용
2. 그렇지 않으면 `req.Experimental.FieldMask` 확인
3. 둘 다 없으면 마스크 없이 전체 Flow 반환

### 7.3 마이그레이션 일정

```
v1.14: field_mask를 GetFlowsRequest 최상위에 추가
v1.15: CLI에서 최상위 field_mask 사용으로 전환
v1.16: experimental.field_mask에 deprecation 경고
v1.17: experimental.field_mask 제거 예정
```

---

## 8. Flow 메시지 구조와 Field Mask 경로

### 8.1 Flow 메시지의 주요 필드

```
flow.Flow
├── time                      google.protobuf.Timestamp
├── verdict                   Verdict (enum)
├── drop_reason               uint32
├── ethernet                  Ethernet
│   ├── source                string
│   └── destination           string
├── IP                        IP
│   ├── source                string
│   ├── destination           string
│   ├── ipVersion             IPVersion (enum)
│   └── encrypted             bool
├── l4                        Layer4
│   ├── TCP                   TCP
│   │   ├── source_port       uint32
│   │   ├── destination_port  uint32
│   │   └── flags             TCPFlags
│   └── UDP                   UDP
│       ├── source_port       uint32
│       └── destination_port  uint32
├── source                    Endpoint
│   ├── id                    uint32
│   ├── identity              uint32
│   ├── namespace             string
│   ├── labels                []string
│   ├── pod_name              string
│   └── workloads             []Workload
├── destination               Endpoint
│   └── (source와 동일 구조)
├── Type                      FlowType (enum)
├── node_name                 string
├── source_names              []string
├── destination_names         []string
├── l7                        Layer7
├── is_reply                  google.protobuf.BoolValue
├── event_type                CiliumEventType
├── source_service            Service
├── destination_service       Service
├── traffic_direction         TrafficDirection (enum)
├── trace_context             TraceContext
├── sock_xlate_point          SocketTranslationPoint
├── socket_cookie             uint64
├── cgroup_id                 uint64
├── Summary                   string
├── extensions                google.protobuf.Any
├── egress_allowed_by         []Policy
├── ingress_allowed_by        []Policy
├── egress_denied_by          []Policy
├── ingress_denied_by         []Policy
├── drop_reason_desc          DropReason (enum)
├── is_l7_lb                  bool
├── trace_observation_point   TraceObservationPoint
├── auth_type                 AuthType
├── ip_version                IPVersion
└── ...
```

### 8.2 유효한 Field Mask 경로 예시

| 경로 | 선택하는 정보 |
|------|-------------|
| `source` | 소스 Endpoint 전체 |
| `source.pod_name` | 소스 Pod 이름만 |
| `source.identity` | 소스 Security Identity |
| `destination.namespace` | 목적지 네임스페이스 |
| `verdict` | 트래픽 판정 (FORWARDED, DROPPED 등) |
| `l4` | L4 프로토콜 전체 (TCP/UDP) |
| `l4.TCP.destination_port` | TCP 목적지 포트만 |
| `IP.source` | 소스 IP 주소만 |
| `time` | 이벤트 타임스탬프 |
| `Summary` | 요약 문자열 |

---

## 9. 성능 최적화 분석

### 9.1 Field Mask의 성능 이점

```
┌────────────────────────────────────────────────────────────┐
│             Field Mask 없이 (전체 Flow)                      │
│                                                             │
│  Hubble Agent → gRPC serialize → 네트워크 전송 → deserialize│
│                                                             │
│  비용: serialize 전체, 전송 전체, deserialize 전체             │
│  Flow 크기: ~500-1000 bytes                                  │
│  초당 10만 flows: ~50-100 MB/s                               │
└────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────┐
│             Field Mask 적용 (선택 필드만)                      │
│                                                             │
│  Hubble Agent → 필드 필터링 → gRPC serialize → 전송 →       │
│  deserialize                                                │
│                                                             │
│  추가 비용: 필터링 (O(n), n=필드 수)                           │
│  절감: serialize 크기 감소, 전송 크기 감소, deserialize 감소   │
│  Flow 크기: ~50-200 bytes                                    │
│  초당 10만 flows: ~5-20 MB/s (최대 75% 절감)                  │
└────────────────────────────────────────────────────────────┘
```

### 9.2 서버 측 처리 비용

Field Mask 적용의 서버 측 CPU 비용은 필드 복사에 비해 매우 작다:
- 경로 파싱: 요청 당 한 번 (캐시 가능)
- 필드 필터링: Flow 당 O(m), m = 마스크 경로 수
- 전체 직렬화 비용 대비 2-5%의 추가 CPU

### 9.3 Hubble Relay에서의 효과

Hubble Relay는 여러 Hubble Agent로부터 flows를 수집하여 클라이언트에 전달한다.
Field Mask는 Agent → Relay 구간에도 적용되어 Relay의 네트워크 대역폭을 절감한다.

```
                 Field Mask 전파
┌──────────┐                    ┌──────────┐
│  Client   │ ──field_mask──── │  Relay    │
│  (hubble  │                   │           │ ──field_mask── Agent 1
│   CLI)    │ ←filtered flows── │           │ ──field_mask── Agent 2
│           │                   │           │ ──field_mask── Agent 3
└──────────┘                    └──────────┘
```

---

## 10. 왜(Why) 이렇게 설계했나

### 10.1 왜 Protobuf FieldMask를 사용하나?

**선택지 1: 커스텀 필드 선택 파라미터** → 새로운 파싱 로직 필요, 표준 없음
**선택지 2: GraphQL 스타일** → gRPC API와 어울리지 않음
**선택지 3: google.protobuf.FieldMask** → 표준, protobuf 도구 체인 활용, 검증 내장

FieldMask는 Google의 API 설계 가이드에서 권장하는 표준 패턴이며,
protobuf 도구 체인(`fieldmaskpb.New`, `fieldmaskpb.Filter`)이 경로 검증과
필터링을 자동으로 처리한다.

### 10.2 왜 Experimental로 먼저 도입했나?

1. **API 안정성**: gRPC API의 변경은 모든 클라이언트에 영향을 미침
2. **피드백 루프**: 실제 사용 패턴을 관찰한 후 기본 마스크 경로 결정
3. **위험 최소화**: 기능에 버그가 있어도 experimental 플래그로 쉽게 비활성화

### 10.3 왜 기본 마스크에 19개 필드를 포함했나?

기본 마스크는 `compact`, `dict`, `tab` 출력 형식에서 표시하는 모든 필드를 포함한다.
사용자가 `--experimental-use-default-field-masks`를 활성화하면,
출력 형식에 불필요한 필드가 자동으로 제거되어 대역폭이 절감된다.

19개 필드를 선택한 기준:
- **Printer가 사용하는 필드**: compact/dict/tab 프린터가 참조하는 모든 필드
- **IP/L4 정보**: 네트워크 트래픽 분석에 필수적인 계층별 정보
- **서비스 매핑**: Kubernetes 서비스와의 연결 정보
- **판정(verdict)**: 트래픽 허용/차단 결과

### 10.4 왜 JSON 출력에서만 커스텀 마스크를 허용하나?

compact/dict/tab 출력 형식은 특정 필드를 가정하고 포매팅 로직이 구현되어 있다.
필드가 누락되면 출력이 깨지거나 의미 없는 데이터가 표시될 수 있다.
JSON 출력은 필드의 존재 여부에 따라 유연하게 출력하므로 커스텀 마스크와 호환된다.

---

## 11. 사용 시나리오

### 11.1 대규모 클러스터 모니터링

```bash
# 최소한의 필드만 요청하여 대역폭 절감
hubble observe -f \
    --experimental-field-mask source.pod_name,destination.pod_name,verdict \
    -o json
```

### 11.2 네트워크 정책 디버깅

```bash
# 정책 관련 필드만 요청
hubble observe -f \
    --experimental-field-mask \
        source.pod_name,source.identity,\
        destination.pod_name,destination.identity,\
        verdict,drop_reason_desc,\
        egress_denied_by,ingress_denied_by \
    -o json
```

### 11.3 서비스 메시 트래픽 분석

```bash
# L7 정보와 서비스 매핑만 요청
hubble observe -f \
    --experimental-field-mask \
        source_service,destination_service,\
        l7,verdict,Summary \
    -o json
```

### 11.4 기본 마스크로 일반 관측

```bash
# 기본 마스크 활성화 (compact 형식에 최적)
hubble observe --experimental-use-default-field-masks
```

---

## 12. PoC 매핑

| PoC | 시뮬레이션 대상 | 핵심 알고리즘 |
|-----|-------------|------------|
| `poc-17-field-mask` | Field Mask 필터링 시스템 | protobuf 필드 경로 파싱, 트리 기반 필터링, 중첩 필드 선택 |

---

## 13. 관련 문서

- [08-observer-pipeline.md](08-observer-pipeline.md): Observer 파이프라인에서 Field Mask 적용 위치
- [14-grpc-api.md](14-grpc-api.md): GetFlows API 전체 구조
- [17-printer-output.md](17-printer-output.md): 출력 형식별 필드 요구사항
- [Google API Design: Field Masks](https://google.aip.dev/161): FieldMask 표준 설계 가이드
