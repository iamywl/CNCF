# PoC-02: Hubble 데이터 모델

## 개요

Hubble의 핵심 데이터 구조인 Flow, Endpoint, Layer4, Layer7, Verdict, Event 타입을 Go 코드로 시뮬레이션한다. Hubble의 데이터 모델은 protobuf로 정의되며, Flow 하나에 40개 이상의 필드가 포함된다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `api/v1/flow/flow.pb.go` | Flow, Endpoint, Layer4, Layer7, Verdict 등 protobuf 생성 코드 |
| `pkg/hubble/api/v1/types.go` | Event 래퍼 구조체, GetFlow(), GetAgentEvent() 등 타입 추출 |
| `pkg/hubble/observer/types/types.go` | MonitorEvent, PerfEvent, AgentEvent, LostEvent |

## 핵심 개념

### 1. 데이터 모델 계층

```
MonitorEvent (BPF에서 수신한 원시 이벤트)
    │
    │ Parser.Decode()
    V
Event{Timestamp, Event: any}
    ├── *Flow      - 네트워크 플로우 (L3/L4/L7)
    ├── *AgentEvent - Cilium 에이전트 이벤트
    ├── *DebugEvent - 디버그 이벤트
    └── *LostEvent  - 유실 이벤트
```

### 2. Flow 구조체의 주요 필드

- **시간/식별**: Time, UUID, NodeName
- **판정**: Verdict(FORWARDED/DROPPED/...), DropReasonDesc
- **네트워크 계층**: Ethernet(L2), IP(L3), Layer4(L4), Layer7(L7)
- **엔드포인트**: Source, Destination (Pod/Namespace/Labels)
- **메타데이터**: TrafficDirection, IsReply, EventType

### 3. Event 래퍼의 타입 안전 추출

```go
func (ev *Event) GetFlow() *Flow {
    if ev == nil || ev.Event == nil { return nil }
    if f, ok := ev.Event.(*Flow); ok { return f }
    return nil
}
```

nil 안전하게 타입 assertion을 수행한다.

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

1. **풍부한 필드 구조**: Flow 하나에 L2/L3/L4/L7, 엔드포인트, 서비스, 정책 정보가 모두 포함
2. **타입 안전 래퍼**: Event.Event는 `any` 타입이지만, GetFlow() 등 메서드로 안전하게 추출
3. **enum 패턴**: Verdict, FlowType 등은 int32 기반 enum으로 정의
4. **MonitorEvent -> Event 변환**: 파서가 원시 바이트를 구조화된 Flow로 디코딩
