# PoC-04: Hubble 파서 파이프라인

## 개요

Hubble의 다계층 파서 체인을 시뮬레이션한다. MonitorEvent(원시 바이트)를 받아 메시지 타입에 따라 적절한 서브 파서로 라우팅하고, 최종적으로 구조화된 Flow/Event 객체를 생성하는 과정을 재현한다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/hubble/parser/parser.go` | 통합 Parser 구조체, Decode() 라우팅 로직 |
| `pkg/hubble/parser/threefour/parser.go` | L3/L4 파서 (gopacket 기반 이더넷/IP/TCP/UDP 디코딩) |
| `pkg/hubble/parser/seven/parser.go` | L7 파서 (AccessLog → HTTP/DNS/Kafka) |
| `pkg/hubble/parser/debug/parser.go` | 디버그 이벤트 파서 |
| `pkg/hubble/parser/sock/parser.go` | 소켓 이벤트 파서 (TraceSock) |
| `pkg/hubble/observer/types/types.go` | MonitorEvent, PerfEvent, AgentEvent 타입 |

## 핵심 개념

### 1. 라우팅 로직 (payload 타입 + Data[0] 분기)

```go
switch payload := monitorEvent.Payload.(type) {
case *PerfEvent:
    switch payload.Data[0] {
    case MessageTypeDebug:     → dbg.Decode()
    case MessageTypeTraceSock: → sock.Decode()
    default:                   → l34.Decode()
    }
case *AgentEvent:
    switch payload.Type {
    case MessageTypeAccessLog: → l7.Decode()
    case MessageTypeAgent:     → AgentNotification
    }
case *LostEvent:
    → LostEvent 직접 생성
}
```

### 2. L3/L4 파싱 (threefour)

실제 구현은 `gopacket.DecodingLayerParser`로 Ethernet -> IPv4/IPv6 -> TCP/UDP/SCTP/ICMPv4/ICMPv6 레이어를 순차적으로 디코딩한다. packetDecoder는 재사용되어 할당을 줄인다.

### 3. L7 파싱 (seven)

AccessLog 레코드(proxy에서 생성)를 받아 HTTP/DNS/Kafka 프로토콜 상세 정보를 Flow.L7에 채운다.

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

1. **2단계 라우팅**: payload 타입(PerfEvent/AgentEvent/LostEvent) + 메시지 타입(Data[0])
2. **서브 파서 조합**: L34, L7, Debug, Sock 파서를 조합한 전략 패턴
3. **Flow 재사용**: 실제 코드에서는 Flow를 미리 할당하고 서브 파서가 채우는 구조
4. **에러 처리**: 알 수 없는 타입은 무시, 빈 데이터는 에러 반환
