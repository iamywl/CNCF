# PoC-11: gRPC 서버 (HTTP/JSON 시뮬레이션)

## 개요

etcd의 gRPC KV/Watch 서비스를 net/http + JSON으로 시뮬레이션한다. 실제 etcd는 gRPC(protobuf)를 사용하지만, 외부 의존성 없이 동일한 API 구조를 재현한다.

## 핵심 개념

| 개념 | 설명 |
|------|------|
| ResponseHeader | 모든 응답에 포함: cluster_id, member_id, revision, raft_term |
| KV 서비스 | Put, Range(Get), DeleteRange 엔드포인트 |
| Watch 서비스 | SSE(Server-Sent Events)로 실시간 변경 알림 |
| MVCC Store | 서버 내부의 리비전 기반 키-값 저장소 |

## API 엔드포인트

| 메서드 | 경로 | 설명 |
|--------|------|------|
| POST | `/v3/kv/put` | 키-값 저장 |
| POST | `/v3/kv/range` | 키 조회 (단일 키 또는 프리픽스) |
| POST | `/v3/kv/deleterange` | 키 삭제 |
| GET | `/v3/watch` | Watch 스트리밍 (SSE) |

## etcd 소스코드 참조

- `server/etcdserver/api/v3rpc/kv.go` — KV 서비스 gRPC 핸들러
- `server/etcdserver/api/v3rpc/watch.go` — Watch 서비스 양방향 스트리밍
- `api/etcdserverpb/rpc.proto` — gRPC 서비스/메시지 정의
- `server/etcdserver/apply.go` — 요청 적용 로직

## 실행 방법

```bash
go run main.go
```

프로그램 내에서 자동으로 HTTP 서버 시작 + 클라이언트 테스트를 수행한다.

## 데모 시나리오

1. HTTP 서버 시작 (127.0.0.1:12380)
2. KV Put: 여러 키 저장 + ResponseHeader 확인
3. KV Range: 단일 키 조회 + 프리픽스 범위 조회
4. Watch: SSE 스트리밍으로 Put/Delete 이벤트 수신
5. Delete: 키 삭제 + 확인
6. ResponseHeader 구조 설명
7. API 엔드포인트 및 curl 테스트 예시
