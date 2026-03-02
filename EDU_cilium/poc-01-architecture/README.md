# PoC 01: Cilium 컴포넌트 간 통신 구조 체험

Cilium의 핵심 통신 패턴(gRPC 스트리밍, REST over UNIX 소켓)을 직접 구현해본다.

---

## 구조

```
이 PoC가 재현하는 패턴:

cilium-daemon ◄──UNIX socket REST──► cilium-dbg (CLI)
hubble-observer ──gRPC stream──► hubble-relay ──gRPC stream──► hubble CLI
```

## 실행 방법

```bash
cd EDU/poc-01-architecture

# 1) gRPC 스트리밍 서버 (hubble-relay 역할) 실행
go run grpc_server.go &

# 2) gRPC 클라이언트 (hubble CLI 역할)로 Flow 스트림 수신
go run grpc_client.go

# 3) UNIX 소켓 REST 서버 (cilium-daemon 역할) 실행
go run unix_rest_server.go &

# 4) UNIX 소켓 REST 클라이언트 (cilium-dbg 역할)로 상태 조회
go run unix_rest_client.go

# 정리
pkill -f "go run.*grpc_server" ; pkill -f "go run.*unix_rest_server"
```

## 핵심 매커니즘

### 1. gRPC 서버 스트리밍 (Hubble 패턴)

Hubble은 **서버 스트리밍 RPC**를 사용한다.
클라이언트가 `GetFlows`를 한 번 호출하면, 서버가 Flow를 계속 `stream.Send()`한다.

```
Client ──GetFlows()──► Server
Client ◄──Flow 1──── Server
Client ◄──Flow 2──── Server
Client ◄──Flow 3──── Server
         ...계속...
```

이 방식의 장점:
- 클라이언트가 매번 요청하지 않아도 실시간 데이터를 수신
- 연결 하나로 수천 개의 Flow를 전달 (HTTP 폴링 대비 오버헤드 극소)
- Cilium 실제 코드: `api/v1/observer/observer.proto`의 `GetFlows` RPC

### 2. UNIX 소켓 REST API (Daemon 패턴)

cilium-daemon은 TCP가 아닌 **UNIX 도메인 소켓**으로 REST API를 노출한다.

```
cilium-dbg ──HTTP over UNIX socket──► /var/run/cilium/cilium.sock
```

이 방식의 장점:
- 네트워크를 거치지 않으므로 같은 노드 내에서만 접근 가능 (보안)
- 파일 시스템 권한으로 접근 제어 (root만 접근)
- TCP 오버헤드 없이 빠른 통신
