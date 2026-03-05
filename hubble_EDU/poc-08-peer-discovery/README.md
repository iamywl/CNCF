# PoC-08: Peer Discovery (피어 노드 발견)

## 개요

Hubble Peer Service가 클러스터 노드의 추가/수정/삭제를 실시간으로 감지하고, gRPC 스트리밍으로 클라이언트에게 ChangeNotification을 전달하는 과정을 시뮬레이션한다.

Relay는 Peer Service를 통해 클러스터의 Hubble 인스턴스 위치를 파악한다. 노드 매니저가 노드 변경을 감지하면 handler를 통해 ChangeNotification이 생성되고, buffer를 거쳐 gRPC 스트림으로 전달된다.

## 핵심 개념

### 1. handler (NodeHandler 인터페이스)

`handler`는 Cilium 노드 매니저의 `NodeHandler` 인터페이스를 구현한다. 노드 이벤트를 수신하여 `ChangeNotification`으로 변환한다.

- `NodeAdd()`: PEER_ADDED 알림 전송
- `NodeUpdate()`: 이름 동일 + 주소 변경 시 PEER_UPDATED, 이름 변경 시 old DELETE + new ADD
- `NodeDelete()`: PEER_DELETED 알림 전송
- 채널 C는 **unbuffered** → 수신측이 준비되어야 전달 가능

### 2. buffer (느린 클라이언트 보호)

고정 크기 버퍼로 handler와 stream.Send 사이에 위치한다.

- `Push()`: 버퍼가 가득 차면 `ErrStreamSendBlocked` 반환 (느린 클라이언트 감지 → 연결 종료)
- `Pop()`: 비어있으면 새 알림이 올 때까지 블로킹 (조건변수 패턴)
- `Close()`: 메모리 해제 및 대기 중인 Pop 해제

### 3. Service.Notify 파이프라인

3개의 goroutine으로 구성된 파이프라인:

```
NodeManager.Subscribe(handler)
         │
         ▼
  goroutine 1: handler.C ──→ buffer.Push()
         │                        │
         │                        ▼
  goroutine 2:              buffer.Pop() ──→ stream.Send()
         │
  goroutine 3: stop 신호 감시 (서비스 종료 시 정리)
```

### 4. TLS 서버 이름

`TLSServerName()` 함수가 노드 이름과 클러스터 이름으로 TLS 서버 이름을 생성한다:

```
형식: {nodeName}.{clusterName}.hubble-grpc.cilium.io
예시: moseisley.tatooine.hubble-grpc.cilium.io
```

이름에 포함된 `.`(점)은 `-`(하이픈)으로 대체하여 DNS 도메인 레벨을 일정하게 유지한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 테스트 항목

| 테스트 | 내용 |
|--------|------|
| 테스트 1 | handler 기본 동작 - Node → ChangeNotification 변환 |
| 테스트 2 | NodeUpdate - 주소 변경(UPDATED), 이름 변경(DELETE+ADD) |
| 테스트 3 | buffer Push/Pop - 블로킹 Pop 동작 |
| 테스트 4 | 느린 클라이언트 보호 - 버퍼 오버플로우 시 에러 |
| 테스트 5 | 전체 Notify 파이프라인 - NodeManager → handler → buffer → sendFn |
| 테스트 6 | TLS 서버 이름 생성 패턴 |
| 테스트 7 | 동시성 안전성 - 버퍼 동시 접근 |

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `cilium/pkg/hubble/peer/service.go` | `Service.Notify()` - gRPC 스트리밍 진입점 |
| `cilium/pkg/hubble/peer/handler.go` | `handler` - NodeHandler 구현, ChangeNotification 생성 |
| `cilium/pkg/hubble/peer/handler.go` | `TLSServerName()` - TLS 서버 이름 생성 |
| `cilium/pkg/hubble/peer/buffer.go` | `buffer` - Push/Pop, 최대 크기 제한 |
| `cilium/pkg/hubble/peer/serviceoption/option.go` | `Options` - MaxSendBufferSize 등 |
