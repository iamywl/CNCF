# PoC-09: gRPC 서버 스트리밍 (GetFlows)

## 개요

Hubble Observer의 `GetFlows()` RPC가 구현하는 서버 스트리밍 패턴을 시뮬레이션한다. 클라이언트가 한 번 요청하면 서버가 연속적으로 Flow 이벤트를 전송하는 구조를 TCP 소켓 + JSON으로 재현한다.

핵심 동작은 세 가지 모드이다:
- `--last N`: 링 버퍼에서 최근 N개 Flow를 읽고 종료
- `--follow`: 새 이벤트가 올 때까지 블로킹 대기하며 지속 전송
- `--follow --last N`: 최근 N개를 먼저 전송한 후 실시간 스트리밍

## 핵심 개념

### 1. 서버 스트리밍 RPC

gRPC 서버 스트리밍에서는 클라이언트가 한 번 요청을 보내면, 서버가 여러 응답을 연속으로 전송한다. Hubble의 `GetFlows()`가 대표적인 예이다.

### 2. Follow 모드 (NextFollow)

`RingReader.NextFollow()`는 링 버퍼에 아직 쓰이지 않은 위치를 읽으려 하면, 새 이벤트가 쓰일 때까지 블로킹 대기한다. 실제 코드에서는 조건 변수(conditional variable)를 사용하여 Writer가 새 이벤트를 쓸 때 Reader에게 알린다.

### 3. 링 버퍼 기반 읽기

`GetFlows()`는 Ring에서 RingReader를 생성하여 이벤트를 읽는다. 시작 위치는 요청 파라미터에 따라 결정된다:
- `--first`: `OldestWrite()`부터
- `--follow` (number=0): `LastWrite()+1`부터 (미래 이벤트만)
- `--last N`: `LastWrite()+1-N`부터

### 4. Context 취소

클라이언트가 연결을 끊으면 gRPC context가 취소되고, 서버의 스트리밍 루프가 정리된다. 이 PoC에서는 TCP 연결 종료 감지로 이를 시뮬레이션한다.

## 실행 방법

```bash
go run main.go
```

TCP 서버를 시작하고 세 가지 테스트를 순서대로 실행한다:
1. `--last 5`: 최근 5개 Flow 조회 후 종료
2. `--follow`: 3초간 실시간 스트리밍
3. `--follow --last 3`: 최근 3개 + 실시간 3초 스트리밍

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `cilium/pkg/hubble/observer/local_observer.go` | `LocalObserverServer.GetFlows()` - 서버 스트리밍 진입점 |
| `cilium/pkg/hubble/container/ring_reader.go` | `RingReader.NextFollow()` - 블로킹 읽기 |
| `cilium/pkg/hubble/container/ring_reader.go` | `RingReader.Next()` - 비블로킹 읽기 |
| `cilium/pkg/hubble/container/ring.go` | `Ring.Write()` - 링 버퍼 쓰기 + 알림 |
| `cilium/pkg/hubble/observer/local_observer.go` | `LocalObserverServer.Start()` - 이벤트 처리 루프 |
