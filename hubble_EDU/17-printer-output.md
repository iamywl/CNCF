# 17. Hubble 프린터/출력 포맷 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Printer 구조체](#2-printer-구조체)
3. [5가지 출력 모드](#3-5가지-출력-모드)
4. [WriteProtoFlow: 플로우 출력](#4-writeprotoflow-플로우-출력)
5. [호스트명 해석 (Hostname)](#5-호스트명-해석-hostname)
6. [Verdict 컬러링](#6-verdict-컬러링)
7. [FlowType 결정 로직](#7-flowtype-결정-로직)
8. [NodeStatusEvent 출력](#8-nodestatusevent-출력)
9. [AgentEvent 출력](#9-agentevent-출력)
10. [DebugEvent 출력](#10-debugevent-출력)
11. [ServerStatus 출력](#11-serverstatus-출력)
12. [LostEvent 출력](#12-lostevent-출력)
13. [Color 시스템](#13-color-시스템)
14. [Terminal Escaper](#14-terminal-escaper)
15. [Options 시스템](#15-options-시스템)
16. [설계 결정 분석 (Why)](#16-설계-결정-분석-why)

---

## 1. 개요

Hubble CLI의 프린터 시스템은 gRPC로 수신한 네트워크 이벤트를 **사람이 읽을 수 있는
형태**로 포맷팅하여 터미널에 출력하는 서브시스템이다. 5가지 출력 모드, ANSI 컬러
지원, IP-to-Pod 변환, 보안 정책 이름 표시 등 CLI 사용자 경험의 핵심 요소를 담당한다.

```
소스 코드 위치:
  hubble/pkg/printer/printer.go   -- 핵심 출력 로직
  hubble/pkg/printer/options.go   -- 옵션 정의 (Output 모드, 기능 토글)
  hubble/pkg/printer/color.go     -- ANSI 컬러 시스템
```

### 프린터가 처리하는 이벤트 타입

| 이벤트 | 메서드 | 설명 |
|--------|--------|------|
| Flow | `WriteProtoFlow` | 네트워크 플로우 (핵심) |
| NodeStatus | `WriteProtoNodeStatusEvent` | 노드 연결/에러 상태 |
| AgentEvent | `WriteProtoAgentEvent` | Cilium Agent 이벤트 |
| DebugEvent | `WriteProtoDebugEvent` | 디버그 이벤트 |
| LostEvent | `WriteLostEvent` | 이벤트 유실 알림 |
| ServerStatus | `WriteServerStatusResponse` | 서버 상태 요약 |

---

## 2. Printer 구조체

```go
// hubble/pkg/printer/printer.go

type Printer struct {
    opts          Options               // 출력 설정
    line          int                   // 현재 출력 줄 번호 (헤더 판정용)
    tw            *tabwriter.Writer     // Tab 모드용 정렬 라이터
    jsonEncoder   *json.Encoder         // JSON 모드용 인코더
    color         *colorer              // ANSI 컬러 관리자
    writerBuilder *terminalEscaperBuilder // 터미널 이스케이프 제거
}
```

### 생성 과정

```go
func New(fopts ...Option) *Printer {
    opts := Options{
        output:     TabOutput,             // 기본: 탭 정렬
        w:          os.Stdout,             // 기본: 표준 출력
        werr:       os.Stderr,             // 기본: 표준 에러
        timeFormat: time.StampMilli,       // 기본: "Jan _2 15:04:05.000"
    }
    for _, fopt := range fopts {
        fopt(&opts)
    }

    p := &Printer{
        opts:  opts,
        color: newColorer(opts.color),
    }

    switch opts.output {
    case TabOutput:
        // tabwriter: 최소 너비 2, 패딩 3, 공백 채움
        p.tw = tabwriter.NewWriter(opts.w, 2, 0, 3, ' ', 0)
        p.color.disable() // tabwriter는 ANSI 코드와 호환 안됨
    case JSONLegacyOutput, JSONPBOutput:
        p.jsonEncoder = json.NewEncoder(p.opts.w)
    }

    p.writerBuilder = newTerminalEscaperBuilder(p.color.sequences())
    return p
}
```

```
[Printer 초기화 분기]

  output 모드
       │
       ├── TabOutput ──→ tabwriter 생성 + 컬러 비활성화
       │
       ├── JSONLegacyOutput ──→ json.Encoder 생성
       │
       ├── JSONPBOutput ──→ json.Encoder 생성
       │
       ├── CompactOutput ──→ (특별한 초기화 없음)
       │
       └── DictOutput ──→ (특별한 초기화 없음)
```

### Close

```go
func (p *Printer) Close() error {
    if p.tw != nil {
        return p.tw.Flush() // tabwriter 버퍼 플러시
    }
    return nil
}
```

`tabwriter`는 내부적으로 버퍼링하여 컬럼 너비를 계산한다.
`Close()`를 호출하지 않으면 마지막 몇 줄이 출력되지 않을 수 있다.

---

## 3. 5가지 출력 모드

```go
// hubble/pkg/printer/options.go

type Output int

const (
    TabOutput        Output = iota  // 탭 정렬 테이블
    JSONLegacyOutput                // Flow만 JSON
    CompactOutput                   // 한 줄 요약
    DictOutput                      // 키:값 사전형
    JSONPBOutput                    // GetFlowsResponse 전체 JSON
)
```

### 각 모드별 출력 예시

#### TabOutput (기본)

```
TIMESTAMP              SOURCE                      DESTINATION                 TYPE        VERDICT    SUMMARY
Jan  2 15:04:05.000    default/frontend-abc-123    default/backend-xyz-456     to-endpoint FORWARDED  TCP Flags: SYN
Jan  2 15:04:05.001    default/backend-xyz-456     default/frontend-abc-123    to-endpoint FORWARDED  TCP Flags: SYN, ACK
```

#### CompactOutput

```
Jan  2 15:04:05.000 [node-1]: default/frontend-abc-123 (ID:1234) -> default/backend-xyz-456 (ID:5678) to-endpoint FORWARDED (TCP Flags: SYN)
Jan  2 15:04:05.001 [node-1]: default/backend-xyz-456 (ID:5678) <- default/frontend-abc-123 (ID:1234) to-endpoint FORWARDED (TCP Flags: SYN, ACK)
```

#### DictOutput

```
  TIMESTAMP: Jan  2 15:04:05.000
       NODE: node-1
     SOURCE: default/frontend-abc-123
DESTINATION: default/backend-xyz-456
       TYPE: to-endpoint
    VERDICT: FORWARDED
    SUMMARY: TCP Flags: SYN
------------
  TIMESTAMP: Jan  2 15:04:05.001
       NODE: node-1
     SOURCE: default/backend-xyz-456
DESTINATION: default/frontend-abc-123
       TYPE: to-endpoint
    VERDICT: FORWARDED
    SUMMARY: TCP Flags: SYN, ACK
```

#### JSONLegacyOutput

```json
{"time":"2024-01-02T15:04:05.000Z","verdict":"FORWARDED","ethernet":{...},"IP":{...},"l4":{...},"source":{...},"destination":{...},...}
```

#### JSONPBOutput

```json
{"flow":{"time":"2024-01-02T15:04:05.000Z","verdict":"FORWARDED",...},"node_name":"node-1","time":"2024-01-02T15:04:05.000Z"}
```

### 모드 선택 비교

| 특성 | Tab | Compact | Dict | JSONLegacy | JSONPB |
|------|-----|---------|------|------------|--------|
| 가독성 | 높음 | 중간 | 높음 | 낮음 | 낮음 |
| 정보량 | 핵심만 | 핵심+ID | 핵심만 | 전체 | 전체 |
| 컬러 | 불가 | 가능 | 가능 | 불가 | 불가 |
| 파이프 | 적합 | 적합 | 부적합 | 적합 | 적합 |
| 자동화 | 부적합 | 부적합 | 부적합 | 적합 | 적합 |
| 줄 수 | 1줄/flow | 1줄/flow | 7줄/flow | 1줄/flow | 1줄/flow |

---

## 4. WriteProtoFlow: 플로우 출력

`WriteProtoFlow`는 프린터의 핵심 메서드로, 각 출력 모드에 따라 다른 포맷으로
플로우를 렌더링한다.

### 전체 구조

```go
func (p *Printer) WriteProtoFlow(res *observerpb.GetFlowsResponse) error {
    f := res.GetFlow()

    switch p.opts.output {
    case TabOutput:    // 탭 정렬 테이블
    case DictOutput:   // 키:값 사전
    case CompactOutput: // 한 줄 요약
    case JSONLegacyOutput: return p.jsonEncoder.Encode(f)   // Flow만
    case JSONPBOutput:     return p.jsonEncoder.Encode(res)  // 전체 응답
    }
    p.line++
    return nil
}
```

### TabOutput 렌더링

```go
case TabOutput:
    w := p.createTabWriter()
    src, dst := p.GetHostNames(f)

    // 첫 번째 줄에만 헤더 출력
    if p.line == 0 {
        w.print("TIMESTAMP", tab)
        if p.opts.nodeName {
            w.print("NODE", tab)
        }
        w.print("SOURCE", tab, "DESTINATION", tab,
                "TYPE", tab, "VERDICT", tab, "SUMMARY", newline)
    }
    // 데이터 행
    w.print(fmtTimestamp(p.opts.timeFormat, f.GetTime()), tab)
    if p.opts.nodeName {
        w.print(f.GetNodeName(), tab)
    }
    w.print(src, tab, dst, tab, GetFlowType(f), tab,
            p.getVerdict(f), tab, p.getSummary(f), newline)
```

### CompactOutput 렌더링

CompactOutput은 응답 방향에 따라 **화살표 방향을 뒤집는** 독특한 기능이 있다.

```go
case CompactOutput:
    src, dst := p.GetHostNames(f)
    srcIdentity, dstIdentity := p.GetSecurityIdentities(f)

    arrow := "->"
    if f.GetIsReply() == nil {
        arrow = "<>"  // 방향 불명
    } else if f.GetIsReply().GetValue() {
        // 응답 패킷: src/dst 교환 + 화살표 반전
        src, dst = dst, src
        srcIdentity, dstIdentity = dstIdentity, srcIdentity
        arrow = "<-"
    }
    w.printf("%s%s: %s %s %s %s %s %s %s (%s)\n",
        fmtTimestamp(...), node,
        src, srcIdentity, arrow, dst, dstIdentity,
        GetFlowType(f), p.getVerdict(f), p.getSummary(f))
```

```
[CompactOutput 화살표 방향]

  요청 패킷 (IsReply=false):
  frontend (ID:1234) -> backend (ID:5678) to-endpoint FORWARDED

  응답 패킷 (IsReply=true):
  frontend (ID:1234) <- backend (ID:5678) to-endpoint FORWARDED
  ↑ 실제로는 backend → frontend이지만, 시각적으로 같은 방향 유지

  방향 불명 (IsReply=nil):
  endpoint-a (ID:1234) <> endpoint-b (ID:5678) to-endpoint FORWARDED
```

### JSONLegacy vs JSONPB

```go
case JSONLegacyOutput:
    return p.jsonEncoder.Encode(f)   // Flow 객체만 직렬화
case JSONPBOutput:
    return p.jsonEncoder.Encode(res) // GetFlowsResponse 전체 직렬화
```

| | JSONLegacy | JSONPB |
|---|-----------|--------|
| 직렬화 대상 | `*flowpb.Flow` | `*observerpb.GetFlowsResponse` |
| 포함 정보 | 플로우 데이터만 | 플로우 + 노드명 + 타임스탬프 |
| 호환성 | 이전 버전 | 현재 권장 |
| NodeStatus | 별도 처리 | 함께 포함 |

---

## 5. 호스트명 해석 (Hostname)

### GetHostNames

```go
func (p *Printer) GetHostNames(f *flowpb.Flow) (string, string) {
    // IP가 없으면 Ethernet MAC 주소 반환
    if f.GetIP() == nil {
        if eth := f.GetEthernet(); eth != nil {
            return p.color.host(eth.GetSource()), p.color.host(eth.GetDestination())
        }
        return "", ""
    }

    // Pod/Service/DNS 이름 추출
    srcNamespace, srcPodName := f.GetSource().GetNamespace(), f.GetSource().GetPodName()
    dstNamespace, dstPodName := f.GetDestination().GetNamespace(), f.GetDestination().GetPodName()
    // Service가 있으면 Pod보다 우선
    if svc := f.GetSourceService(); svc != nil {
        srcNamespace, srcSvcName = svc.GetNamespace(), svc.GetName()
    }
    // ...
    srcPort, dstPort := p.GetPorts(f)
    src := p.Hostname(f.GetIP().GetSource(), srcPort, srcNamespace, srcPodName, srcSvcName, f.GetSourceNames())
    dst := p.Hostname(f.GetIP().GetDestination(), dstPort, dstNamespace, dstPodName, dstSvcName, f.GetDestinationNames())
    return p.color.host(src), p.color.host(dst)
}
```

### Hostname 메서드

```go
func (p *Printer) Hostname(ip, port string, ns, pod, svc string, names []string) string {
    host := ip  // 기본: IP 주소
    if p.opts.enableIPTranslation {
        switch {
        case pod != "":
            host = path.Join(ns, pod)         // "default/frontend-abc"
        case svc != "":
            host = path.Join(ns, svc)         // "kube-system/kube-dns"
        case len(names) != 0:
            host = strings.Join(names, ",")    // "www.example.com"
        }
    }
    if port != "" && port != "0" {
        return net.JoinHostPort(host, p.color.port(port))  // "default/frontend:80"
    }
    return host
}
```

```
[호스트명 해석 우선순위]

  enableIPTranslation = true 일 때:
  1. Pod 이름     → "namespace/pod-name"
  2. Service 이름 → "namespace/service-name"
  3. DNS 이름     → "www.example.com,cdn.example.com"
  4. IP 주소      → "10.0.1.1" (fallback)

  enableIPTranslation = false 일 때:
  항상 IP 주소 → "10.0.1.1"

  포트가 있으면:
  "default/frontend:80" 또는 "10.0.1.1:80"
```

### Security Identity 포맷

```go
func (p *Printer) fmtIdentity(i uint32) string {
    numeric := identity.NumericIdentity(i)
    if numeric.IsReservedIdentity() {
        return p.color.identity(fmt.Sprintf("(%s)", numeric))  // "(host)"
    }
    return p.color.identity(fmt.Sprintf("(ID:%d)", i))          // "(ID:1234)"
}
```

예약 Identity (0~15)는 이름으로 표시:
- `(host)` - 호스트 네트워크
- `(world)` - 외부 트래픽
- `(unmanaged)` - 비관리 엔드포인트
- `(health)` - 헬스체크

일반 Identity는 숫자로: `(ID:1234)`

---

## 6. Verdict 컬러링

### getVerdict 메서드

```go
func (p Printer) getVerdict(f *flowpb.Flow) string {
    verdict := f.GetVerdict()
    msg := verdict.String()
    switch verdict {
    case flowpb.Verdict_FORWARDED, flowpb.Verdict_REDIRECTED:
        if f.GetEventType().GetType() == api.MessageTypePolicyVerdict {
            msg = "ALLOWED"
            if p.opts.policyNames {
                // 정책 이름 추가: "ALLOWED BY my-policy (CiliumNetworkPolicy)"
                if f.GetTrafficDirection() == flowpb.TrafficDirection_EGRESS {
                    msg += formatPolicyNames(f.GetEgressAllowedBy())
                } else if f.GetTrafficDirection() == flowpb.TrafficDirection_INGRESS {
                    msg += formatPolicyNames(f.GetIngressAllowedBy())
                }
            }
        }
        return p.color.verdictForwarded(msg) // 초록색
    case flowpb.Verdict_DROPPED, flowpb.Verdict_ERROR:
        if f.GetEventType().GetType() == api.MessageTypePolicyVerdict {
            msg = "DENIED"
            if p.opts.policyNames {
                // ... 거부 정책 이름
            }
        }
        return p.color.verdictDropped(msg) // 빨간색
    case flowpb.Verdict_AUDIT:
        if f.GetEventType().GetType() == api.MessageTypePolicyVerdict {
            msg = "AUDITED"
        }
        return p.color.verdictAudit(msg) // 노란색
    case flowpb.Verdict_TRACED:
        return p.color.verdictTraced(msg) // 노란색
    case flowpb.Verdict_TRANSLATED:
        return p.color.verdictTranslated(msg) // 노란색
    default:
        return msg // 무색
    }
}
```

### Verdict 색상 매핑

```
[Verdict 컬러 매핑]

  ┌──────────────────┬────────────────┬────────┐
  │ Verdict          │ 표시 텍스트    │ 색상   │
  ├──────────────────┼────────────────┼────────┤
  │ FORWARDED        │ FORWARDED      │ 초록   │
  │ REDIRECTED       │ REDIRECTED     │ 초록   │
  │ FORWARDED+Policy │ ALLOWED        │ 초록   │
  ├──────────────────┼────────────────┼────────┤
  │ DROPPED          │ DROPPED        │ 빨강   │
  │ ERROR            │ ERROR          │ 빨강   │
  │ DROPPED+Policy   │ DENIED         │ 빨강   │
  ├──────────────────┼────────────────┼────────┤
  │ AUDIT            │ AUDIT/AUDITED  │ 노랑   │
  │ TRACED           │ TRACED         │ 노랑   │
  │ TRANSLATED       │ TRANSLATED     │ 노랑   │
  └──────────────────┴────────────────┴────────┘
```

### Policy 이름 포맷

```go
func formatPolicyNames(policies []*flowpb.Policy) string {
    msg := ""
    i := 0
    for _, policy := range policies {
        if policy.GetKind() != "" && policy.GetName() != "" {
            if i == 0 {
                msg += " BY "
            } else {
                msg += ", "
            }
            msg += fmt.Sprintf("%s (%s)", policy.GetName(), policy.GetKind())
            i += 1
        }
    }
    return msg
}
```

출력 예시:
```
ALLOWED BY allow-http-ingress (CiliumNetworkPolicy), cluster-default (CiliumClusterwideNetworkPolicy)
DENIED BY deny-external (CiliumNetworkPolicy)
```

---

## 7. FlowType 결정 로직

```go
func GetFlowType(f *flowpb.Flow) string {
    // L7 이벤트: 프로토콜별 분류
    if l7 := f.GetL7(); l7 != nil {
        l7Protocol := "l7"
        l7Type := strings.ToLower(l7.GetType().String()) // "request", "response"
        switch l7.GetRecord().(type) {
        case *flowpb.Layer7_Http:
            l7Protocol = "http"
        case *flowpb.Layer7_Dns:
            l7Protocol = "dns"
            l7Type += " " + l7.GetDns().GetObservationSource()
        case *flowpb.Layer7_Kafka:
            l7Protocol = "kafka"
        }
        return l7Protocol + "-" + l7Type
        // 예: "http-request", "dns-response proxy", "kafka-request"
    }

    // L3/L4 이벤트: EventType 기반 분류
    switch f.GetEventType().GetType() {
    case api.MessageTypeTrace:
        return api.TraceObservationPoint(uint8(f.GetEventType().GetSubType()))
        // 예: "to-endpoint", "to-stack", "from-network"
    case api.MessageTypeDrop:
        return api.DropReason(uint8(f.GetEventType().GetSubType()))
        // 예: "Policy denied", "Invalid source ip"
    case api.MessageTypePolicyVerdict:
        return fmt.Sprintf("%s:%s %s",
            api.MessageTypeNamePolicyVerdict,
            api.PolicyMatchType(f.GetPolicyMatchType()).String(),
            f.GetTrafficDirection().String())
        // 예: "policy-verdict:L3-L4 INGRESS"
    case api.MessageTypeCapture:
        return f.GetDebugCapturePoint().String()
    case api.MessageTypeTraceSock:
        // 소켓 변환 포인트별 분류
        switch f.GetSockXlatePoint() {
        case flowpb.SocketTranslationPoint_SOCK_XLATE_POINT_POST_DIRECTION_FWD:
            return "post-xlate-fwd"
        // ...
        }
    }
    return "UNKNOWN"
}
```

```
[FlowType 결정 트리]

  L7 이벤트?
  ├── Yes → L7 프로토콜 + 타입
  │   ├── HTTP  → "http-request" / "http-response"
  │   ├── DNS   → "dns-request" / "dns-response proxy"
  │   └── Kafka → "kafka-request" / "kafka-response"
  │
  └── No → EventType 기반
      ├── Trace     → "to-endpoint", "to-stack", "from-network" 등
      ├── Drop      → "Policy denied", "Invalid source ip" 등
      ├── PolicyVerdict → "policy-verdict:L3-L4 INGRESS"
      ├── Capture   → DebugCapturePoint 문자열
      └── TraceSock → "post-xlate-fwd", "pre-xlate-rev" 등
```

---

## 8. NodeStatusEvent 출력

### WriteProtoNodeStatusEvent

```go
func (p *Printer) WriteProtoNodeStatusEvent(r *observerpb.GetFlowsResponse) error {
    s := r.GetNodeStatus()
    if s == nil {
        return errors.New("not a node status event")
    }

    // 디버그 모드가 아니면 ERROR/UNAVAILABLE만 출력
    if !p.opts.enableDebug {
        switch s.GetStateChange() {
        case relaypb.NodeState_NODE_ERROR, relaypb.NodeState_NODE_UNAVAILABLE:
            break // 출력
        default:
            return nil // 건너뜀 (CONNECTED, GONE 등)
        }
    }
    // ...
}
```

### 출력 모드별 NodeStatus 렌더링

```
[Tab/Compact 모드]

  NODE_CONNECTED:
  Jan  2 15:04:05.000 [relay-node]: Receiving flows from 3 nodes: node-1, node-2, node-3

  NODE_UNAVAILABLE:
  Jan  2 15:04:05.000 [relay-node]: 2 nodes are unavailable: node-4, node-5

  NODE_GONE:
  Jan  2 15:04:05.000 [relay-node]: 1 nodes removed from cluster: node-6

  NODE_ERROR:
  Jan  2 15:04:05.000 [relay-node]: Error "connection refused" on 2 nodes: node-7, node-8
```

### joinWithCutOff: 노드 이름 축약

```go
func joinWithCutOff(elems []string, sep string, targetLen int) string {
    strLen := 0
    end := len(elems)
    for i, elem := range elems {
        strLen += len(elem) + len(sep)
        if strLen > targetLen && i > 0 {
            end = i
            break
        }
    }
    joined := strings.Join(elems[:end], sep)
    omitted := len(elems) - end
    if omitted == 0 {
        return joined
    }
    return fmt.Sprintf("%s (and %d more)", joined, omitted)
}
```

```
[joinWithCutOff 동작 예시]

  targetLen = 50

  입력: ["node-1", "node-2", "node-3", ..., "node-20"]
  출력: "node-1, node-2, node-3, node-4 (and 16 more)"
                                        ^
                              50자 초과 시점에서 절단

  입력: ["very-long-node-name-abc-def-ghi-jkl"]
  출력: "very-long-node-name-abc-def-ghi-jkl"
        ^
        첫 번째 요소는 항상 포함 (targetLen 초과해도)
```

---

## 9. AgentEvent 출력

### getAgentEventDetails

AgentEvent는 Cilium Agent의 내부 상태 변화를 나타낸다.

```go
func getAgentEventDetails(e *flowpb.AgentEvent, timeLayout string) string {
    switch e.GetType() {
    case flowpb.AgentEventType_AGENT_STARTED:
        // "start time: Jan  2 15:04:05.000"
    case flowpb.AgentEventType_POLICY_UPDATED, flowpb.AgentEventType_POLICY_DELETED:
        // "labels: [k8s:app=frontend], revision: 42, count: 3"
    case flowpb.AgentEventType_ENDPOINT_REGENERATE_SUCCESS, _FAILURE:
        // "id: 1234, labels: [k8s:app=frontend], error: ..."
    case flowpb.AgentEventType_ENDPOINT_CREATED, _DELETED:
        // "id: 1234, namespace: default, pod name: frontend-abc"
    case flowpb.AgentEventType_IPCACHE_UPSERTED, _DELETED:
        // "cidr: 10.0.1.0/24, identity: 1234, host ip: 192.168.1.1"
    case flowpb.AgentEventType_SERVICE_UPSERTED:
        // "id: 42, frontend: 10.0.0.1:80, backends: [10.0.1.1:8080,...]"
    case flowpb.AgentEventType_SERVICE_DELETED:
        // "id: 42"
    }
    return "UNKNOWN"
}
```

### AgentEvent 타입별 세부 정보

| 이벤트 타입 | 세부 정보 |
|------------|----------|
| `AGENT_STARTED` | 시작 시각 |
| `POLICY_UPDATED/DELETED` | 라벨, 리비전, 규칙 수 |
| `ENDPOINT_REGENERATE_*` | 엔드포인트 ID, 라벨, 에러 |
| `ENDPOINT_CREATED/DELETED` | ID, 네임스페이스, Pod 이름 |
| `IPCACHE_UPSERTED/DELETED` | CIDR, Identity, 호스트 IP, 암호화 키 |
| `SERVICE_UPSERTED` | ID, 프론트엔드, 백엔드, 타입, 네임스페이스 |
| `SERVICE_DELETED` | ID |

---

## 10. DebugEvent 출력

### CompactOutput 형식

```go
case CompactOutput:
    w.printf("%s%s: %s %s MARK: %s CPU: %s (%s)\n",
        fmtTimestamp(...),    // 시간
        node,                // [node-name]
        fmtEndpointShort(e.GetSource()), // 소스 엔드포인트
        e.GetType(),         // 이벤트 타입
        fmtHexUint32(e.GetHash()), // 패킷 마크 (16진수)
        fmtCPU(e.GetCpu()),  // CPU 번호
        e.GetMessage(),      // 디버그 메시지
    )
```

### 헬퍼 함수들

```go
func fmtEndpointShort(ep *flowpb.Endpoint) string {
    if ep == nil { return "N/A" }
    str := fmt.Sprintf("ID: %d", ep.GetID())
    if ns, pod := ep.GetNamespace(), ep.GetPodName(); ns != "" && pod != "" {
        str = fmt.Sprintf("%s/%s (%s)", ns, pod, str) // "default/pod (ID: 42)"
    } else if lbls := ep.GetLabels(); len(lbls) == 1 && strings.HasPrefix(lbls[0], "reserved:") {
        str = fmt.Sprintf("%s (%s)", lbls[0], str)     // "reserved:host (ID: 1)"
    }
    return str
}

func fmtHexUint32(v *wrapperspb.UInt32Value) string {
    if v == nil { return "N/A" }
    return "0x" + strconv.FormatUint(uint64(v.GetValue()), 16) // "0xabcdef12"
}

func fmtCPU(cpu *wrapperspb.Int32Value) string {
    if cpu == nil { return "N/A" }
    return fmt.Sprintf("%02d", cpu.GetValue()) // "07"
}
```

---

## 11. ServerStatus 출력

### WriteServerStatusResponse

ServerStatus는 `hubble status` 명령의 출력을 담당한다.

```go
func (p *Printer) WriteServerStatusResponse(res *observerpb.ServerStatusResponse) error {
    numConnectedNodes := "N/A"
    if n := res.GetNumConnectedNodes(); n != nil {
        numConnectedNodes = fmt.Sprintf("%d", n.GetValue())
    }
    // ...
    switch p.opts.output {
    case TabOutput:     // 헤더 + 데이터 행
    case DictOutput:    // 키:값 쌍
    case CompactOutput: // 요약 형식
    case JSONPBOutput:  // JSON
    }
}
```

### CompactOutput ServerStatus 예시

```
Current/Max Flows: 8,192/16,383 (50.00%)
Flows/s: 42.50
Connected Nodes: 3/5
Unavailable Nodes: 2
  - node-4
  - node-5
```

### uint64Grouping

대수를 읽기 쉽게 천 단위 구분자(`,`)를 추가하는 함수:

```
16383 → "16,383"
1234567 → "1,234,567"
```

---

## 12. LostEvent 출력

### WriteLostEvent

이벤트 유실은 커널의 perf event ring buffer가 가득 차서 이벤트를 놓친 경우 발생한다.

```go
func (p *Printer) WriteLostEvent(res *observerpb.GetFlowsResponse) error {
    f := res.GetLostEvents()
    // ...
    case CompactOutput:
        w.printf("EVENTS LOST: %s CPU(%d) %d\n",
            f.GetSource(),          // "PERF_EVENT_RING" 등
            f.GetCpu().GetValue(),  // CPU 번호
            f.GetNumEventsLost(),   // 유실 수
        )
}
```

```
[LostEvent 출력 예시]

  Compact: EVENTS LOST: PERF_EVENT_RING CPU(3) 42

  Tab:
  TIMESTAMP   SOURCE             DESTINATION   TYPE          VERDICT   SUMMARY
              PERF_EVENT_RING                  EVENTS LOST             CPU(3) - 42
```

---

## 13. Color 시스템

### colorer 구조체

```go
// hubble/pkg/printer/color.go

type colorer struct {
    colors  []*color.Color  // 모든 색상 객체 목록
    red     sprinter        // 빨강 (DROPPED, ERROR)
    green   sprinter        // 초록 (FORWARDED, Auth 활성)
    blue    sprinter        // 파랑
    cyan    sprinter        // 시안 (호스트명)
    magenta sprinter        // 마젠타 (Identity)
    yellow  sprinter        // 노랑 (AUDIT, TRACED, 포트)
}
```

### 색상 모드 제어

```go
func newColorer(when string) *colorer {
    // 6가지 색상 객체 생성
    red := color.New(color.FgRed)
    green := color.New(color.FgGreen)
    blue := color.New(color.FgBlue)
    cyan := color.New(color.FgCyan)
    magenta := color.New(color.FgMagenta)
    yellow := color.New(color.FgYellow)
    // ...
    switch strings.ToLower(when) {
    case "always": c.enable()   // 항상 컬러
    case "never":  c.disable()  // 항상 무색
    case "auto":   c.auto()     // 터미널이면 컬러
    }
}
```

### 색상 용도 매핑

```
[색상 → 의미 매핑]

  ┌─────────┬──────────────────────────────────┐
  │ 색상    │ 용도                              │
  ├─────────┼──────────────────────────────────┤
  │ 초록    │ FORWARDED/REDIRECTED/ALLOWED      │
  │         │ Auth 활성                         │
  ├─────────┼──────────────────────────────────┤
  │ 빨강    │ DROPPED/ERROR/DENIED              │
  │         │ Auth TEST_ALWAYS_FAIL            │
  ├─────────┼──────────────────────────────────┤
  │ 노랑    │ AUDIT/AUDITED/TRACED/TRANSLATED  │
  │         │ 포트 번호                         │
  ├─────────┼──────────────────────────────────┤
  │ 시안    │ 호스트명 (Pod/Service/IP)         │
  ├─────────┼──────────────────────────────────┤
  │ 마젠타  │ Security Identity                │
  ├─────────┼──────────────────────────────────┤
  │ 파랑    │ (현재 미사용, 확장 예비)          │
  └─────────┴──────────────────────────────────┘
```

### TabOutput에서 컬러가 비활성화되는 이유

```go
case TabOutput:
    p.tw = tabwriter.NewWriter(opts.w, 2, 0, 3, ' ', 0)
    p.color.disable() // tabwriter는 ANSI 코드와 호환 안됨
```

`tabwriter.Writer`는 문자열의 **바이트 길이**를 기준으로 컬럼 너비를 계산한다.
ANSI 이스케이프 시퀀스(`\033[31m`처럼 보이지 않는 문자)가 포함되면 실제 표시 너비와
바이트 길이가 달라져 정렬이 깨진다. 따라서 TabOutput 모드에서는 컬러를 완전히
비활성화한다.

### sequences 메서드 (Terminal Escaper용)

```go
func (c *colorer) sequences() []string {
    unique := make(map[string]struct{})
    for _, v := range c.colors {
        seq := v.Sprint("|")         // "\033[31m|\033[0m"
        split := strings.Split(seq, "|")
        if len(split) != 2 { continue }
        unique[split[0]] = struct{}{} // "\033[31m" (시작)
        unique[split[1]] = struct{}{} // "\033[0m" (리셋)
    }
    return slices.Collect(maps.Keys(unique))
}
```

---

## 14. Terminal Escaper

### 왜 Terminal Escaper가 필요한가?

ANSI 이스케이프 시퀀스가 포함된 출력을 `tabwriter`에 보내면 정렬이 깨진다.
하지만 사용자가 `hubble observe -o dict` (DictOutput)에서 `| grep DROPPED` 같은
파이프라인을 사용하면, 컬러 코드가 grep 결과를 오염시킬 수 있다.

`terminalEscaperBuilder`와 `terminalEscaperWriter`는 비-터미널 출력 시
ANSI 이스케이프 시퀀스를 **자동 제거**하는 레이어이다.

```go
func (p *Printer) createStdoutWriter() *terminalEscaperWriter {
    return p.writerBuilder.NewWriter(p.opts.w)
}

func (p *Printer) createStderrWriter() *terminalEscaperWriter {
    return p.writerBuilder.NewWriter(p.opts.werr)
}

func (p *Printer) createTabWriter() *terminalEscaperWriter {
    return p.writerBuilder.NewWriter(p.tw)
}
```

---

## 15. Options 시스템

### Options 구조체

```go
// hubble/pkg/printer/options.go

type Options struct {
    output              Output     // 출력 모드 (Tab/Compact/Dict/JSON/JSONPB)
    w                   io.Writer  // 표준 출력 대상
    werr                io.Writer  // 에러 출력 대상
    enableDebug         bool       // 디버그 메시지 출력
    enableIPTranslation bool       // IP → Pod/Service 이름 변환
    nodeName            bool       // 노드 이름 표시
    policyNames         bool       // 정책 이름 표시
    timeFormat          string     // 시간 형식 레이아웃
    color               string     // 컬러 모드 ("auto"/"always"/"never")
}
```

### Option 함수들

| Option 함수 | 효과 |
|-------------|------|
| `Tab()` | TabOutput 모드 |
| `Compact()` | CompactOutput 모드 |
| `Dict()` | DictOutput 모드 |
| `JSONLegacy()` | JSONLegacyOutput 모드 |
| `JSONPB()` | JSONPBOutput 모드 |
| `Writer(w)` | 출력 대상 변경 |
| `IgnoreStderr()` | stderr를 `io.Discard`로 |
| `WithColor(when)` | 컬러 모드 설정 |
| `WithDebug()` | 디버그 메시지 활성화 |
| `WithIPTranslation()` | IP → 이름 변환 활성화 |
| `WithNodeName()` | 노드 이름 컬럼 추가 |
| `WithPolicyNames()` | Verdict에 정책 이름 표시 |
| `WithTimeFormat(layout)` | 시간 포맷 변경 |

### WithColor 옵션

```go
func WithColor(when string) Option {
    return func(opts *Options) {
        opts.color = when
    }
}
```

| `when` 값 | 동작 |
|-----------|------|
| `"auto"` | `color.NoColor` 전역 변수로 터미널 여부 자동 판단 |
| `"always"` | 항상 ANSI 컬러 사용 |
| `"never"` | 항상 무색 |

### IgnoreStderr 옵션

```go
func IgnoreStderr() Option {
    return func(opts *Options) {
        opts.werr = io.Discard
    }
}
```

NodeStatusEvent는 `werr`(stderr)에 출력된다. `IgnoreStderr`를 사용하면
노드 상태 메시지를 완전히 억제할 수 있다.

---

## 16. 설계 결정 분석 (Why)

### Q1: 왜 stdout과 stderr를 분리하는가?

```go
opts := Options{
    w:    os.Stdout,  // 플로우 데이터
    werr: os.Stderr,  // NodeStatus, 에러 메시지
}
```

Unix 철학에 따라:
- `stdout`: 구조화된 데이터 출력 (파이프라인 가능)
- `stderr`: 부수 정보/에러 메시지

`hubble observe -o json | jq .` 같은 파이프라인에서 NodeStatus 메시지가 stdout에
섞이면 JSON 파싱이 깨진다. stderr로 분리하면 데이터 스트림의 순수성이 보장된다.

### Q2: 왜 TabOutput에서 컬러를 비활성화하는가?

`tabwriter`의 정렬 알고리즘은 문자열의 **바이트 길이**를 기준으로 컬럼 너비를
계산한다. ANSI 이스케이프 시퀀스(`\033[31m`DROPPED`\033[0m`)는 화면에 보이지
않지만 바이트를 차지하므로, "DROPPED"라는 7글자가 실제로는 20+ 바이트로 측정되어
정렬이 완전히 어긋난다.

CompactOutput과 DictOutput은 고정 폭 컬럼을 사용하지 않으므로 컬러가 안전하다.

### Q3: 왜 CompactOutput에서 응답 패킷의 방향을 뒤집는가?

```go
if f.GetIsReply().GetValue() {
    src, dst = dst, src
    arrow = "<-"
}
```

네트워크 관점에서 TCP 핸드셰이크는:
```
클라이언트 → 서버 (SYN)      : src=클라이언트, dst=서버
서버 → 클라이언트 (SYN-ACK)  : src=서버, dst=클라이언트
```

방향을 뒤집지 않으면 같은 연결의 패킷이 src/dst가 번갈아 나와서 혼란스럽다.
뒤집으면 항상 `클라이언트 -> 서버` / `클라이언트 <- 서버`로 표시되어
**하나의 연결이 시각적으로 일관된 방향**을 가진다.

### Q4: 왜 JSONLegacy와 JSONPB 두 가지 JSON 모드인가?

```go
case JSONLegacyOutput: return p.jsonEncoder.Encode(f)   // Flow만
case JSONPBOutput:     return p.jsonEncoder.Encode(res)  // 전체 응답
```

**JSONLegacy**: 초기 Hubble에서 사용하던 형식으로, `Flow` 객체만 직렬화한다.
기존 도구/스크립트와의 호환성을 위해 유지된다.

**JSONPB**: proto3의 JSON 매핑 규칙에 따라 `GetFlowsResponse` 전체를 직렬화한다.
NodeStatus 이벤트도 같은 스트림에서 처리할 수 있으며, `node_name`과 `time` 필드가
최상위에 포함된다. 현재 권장 형식이다.

### Q5: 왜 line 카운터를 사용하는가?

```go
if p.line == 0 {
    // 헤더 출력
    w.print("TIMESTAMP", tab, "SOURCE", tab, ...)
}
// 데이터 행 출력
p.line++
```

TabOutput에서 **헤더는 한 번만** 출력되어야 한다. `p.line`이 0일 때만 헤더를
출력하고, 이후에는 데이터 행만 출력한다.

DictOutput에서는 `p.line != 0`일 때 구분선(`------------`)을 출력한다.
첫 번째 항목 전에는 구분선이 불필요하다.

### Q6: 왜 디버그 모드가 아니면 NodeStatus를 필터링하는가?

```go
if !p.opts.enableDebug {
    switch s.GetStateChange() {
    case relaypb.NodeState_NODE_ERROR, relaypb.NodeState_NODE_UNAVAILABLE:
        break // 출력
    default:
        return nil // 건너뜀
    }
}
```

일반 사용자에게 "Receiving flows from 3 nodes: node-1, node-2, node-3" 같은
정보성 메시지는 노이즈다. **ERROR와 UNAVAILABLE만** 기본적으로 표시하여
문제가 있을 때만 주의를 끈다. 디버그 모드(`--debug`)에서는 모든 상태를 표시한다.

### Q7: 왜 nodeNamesCutOff = 50인가?

```go
const nodeNamesCutOff = 50
```

100개 노드 클러스터에서 "2 nodes are unavailable: node-1, node-2, ..., node-100"
같은 메시지는 터미널에서 읽기 어렵다. 50자로 절단하면 대략 3~5개 노드 이름만
표시되고 나머지는 "(and N more)"로 요약된다. 이는 문제 진단에 충분하면서도
출력이 깔끔하게 유지된다.

### Q8: 왜 Auth 정보를 Summary에 추가하는가?

```go
func (p Printer) getSummary(f *flowpb.Flow) string {
    auth := p.getAuth(f)
    if auth == "" {
        return f.GetSummary()
    }
    return fmt.Sprintf("%s; Auth: %s", f.GetSummary(), auth)
}
```

Cilium의 상호 인증(Mutual Authentication) 기능이 활성화되면 플로우마다 인증 타입이
기록된다. 이를 Summary 필드에 추가하여 **기존 출력 형식을 깨지 않으면서** 인증
정보를 표시한다. `DISABLED` 상태에서는 빈 문자열을 반환하여 불필요한 정보를 숨긴다.

---

## 요약

| 컴포넌트 | 역할 | 핵심 메커니즘 |
|----------|------|-------------|
| Printer | 이벤트 포맷 렌더링 | switch 기반 5개 출력 모드 |
| Options | 출력 설정 | Functional Options 패턴 |
| colorer | ANSI 컬러 관리 | auto/always/never + 의미적 색상 |
| tabwriter | 탭 정렬 | Go 표준 라이브러리 활용 |
| json.Encoder | JSON 직렬화 | Legacy(Flow)/JSONPB(Response) |
| Hostname | IP→이름 변환 | Pod > Service > DNS > IP 우선순위 |
| getVerdict | Verdict 렌더링 | PolicyVerdict 특수 처리 + 컬러 |
| GetFlowType | 이벤트 타입 분류 | L7/L3L4/Drop/PolicyVerdict 계층 |

프린터 시스템의 핵심 설계 원칙:
1. **사용자 중심**: 같은 데이터를 5가지 다른 방식으로 볼 수 있는 유연성
2. **의미적 컬러링**: 색상이 정보(verdict, identity, host)를 전달
3. **Unix 호환**: stdout/stderr 분리, 파이프라인 안전
4. **탄력적 출력**: 정보 부족 시 N/A 표시, nil 안전 처리
