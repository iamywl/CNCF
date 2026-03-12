# 14. gRPC API 계층

## 개요

etcd v3 API는 gRPC 기반으로 설계되어 있다. Protocol Buffers로 서비스와 메시지를 정의하고, gRPC 프레임워크가 직렬화, 전송, 에러 처리를 담당한다. etcd는 6개의 gRPC 서비스를 제공하며, 각 서비스는 독립적인 서버 구조체로 구현된다.

이 계층의 핵심 역할은 클라이언트 요청을 검증하고, etcdserver로 전달하며, 응답에 클러스터 메타데이터(ResponseHeader)를 채워 반환하는 것이다.

---

## rpc.proto: 6개 서비스 정의

`api/etcdserverpb/rpc.proto`에 정의된 6개 gRPC 서비스는 다음과 같다.

```
┌────────────────────────────────────────────────────┐
│                etcd gRPC 서비스                      │
├──────────────┬─────────────────────────────────────┤
│ 서비스        │ RPC 메서드                            │
├──────────────┼─────────────────────────────────────┤
│ KV           │ Range, Put, DeleteRange, Txn,       │
│              │ Compact                             │
├──────────────┼─────────────────────────────────────┤
│ Watch        │ Watch (양방향 스트리밍)               │
├──────────────┼─────────────────────────────────────┤
│ Lease        │ LeaseGrant, LeaseRevoke,            │
│              │ LeaseKeepAlive (양방향 스트리밍),     │
│              │ LeaseTimeToLive, LeaseLeases        │
├──────────────┼─────────────────────────────────────┤
│ Cluster      │ MemberAdd, MemberRemove,            │
│              │ MemberUpdate, MemberList,           │
│              │ MemberPromote                       │
├──────────────┼─────────────────────────────────────┤
│ Maintenance  │ Alarm, Status, Defragment,          │
│              │ Hash, HashKV, Snapshot,             │
│              │ MoveLeader, Downgrade               │
├──────────────┼─────────────────────────────────────┤
│ Auth         │ AuthEnable, AuthDisable,            │
│              │ AuthStatus, Authenticate,           │
│              │ UserAdd, UserGet, UserList,         │
│              │ UserDelete, UserChangePassword,     │
│              │ UserGrantRole, UserRevokeRole,      │
│              │ RoleAdd, RoleGet, RoleList,         │
│              │ RoleDelete, RoleGrantPermission,    │
│              │ RoleRevokePermission                │
└──────────────┴─────────────────────────────────────┘
```

### proto 파일에서의 서비스 정의 예시 (KV)

```protobuf
service KV {
  rpc Range(RangeRequest) returns (RangeResponse) { ... }
  rpc Put(PutRequest) returns (PutResponse) { ... }
  rpc DeleteRange(DeleteRangeRequest) returns (DeleteRangeResponse) { ... }
  rpc Txn(TxnRequest) returns (TxnResponse) { ... }
  rpc Compact(CompactionRequest) returns (CompactionResponse) { ... }
}
```

---

## 서버 등록 흐름: grpc.go

`server/etcdserver/api/v3rpc/grpc.go`의 `Server()` 함수가 모든 gRPC 서비스를 등록한다.

```
Server(s, tls, interceptor, gopts) 흐름:

1. gRPC 서버 옵션 구성:
   ├── CustomCodec (호환성)
   ├── TLS 설정 (있는 경우)
   ├── Unary 인터셉터 체인:
   │   ├── newLogUnaryInterceptor     → 요청 로깅
   │   ├── serverMetrics              → Prometheus 메트릭
   │   ├── newUnaryInterceptor        → 능력/리더 검증
   │   └── (외부 인터셉터)             → 선택적
   ├── Stream 인터셉터 체인:
   │   ├── serverMetrics              → Prometheus 메트릭
   │   └── newStreamInterceptor       → 능력/리더 검증
   ├── MaxRecvMsgSize                 → 최대 수신 메시지 크기
   ├── MaxSendMsgSize                 → MaxInt32
   ├── MaxConcurrentStreams           → 동시 스트림 수 제한
   └── 분산 트레이싱 (선택적)

2. 서비스 등록:
   ├── pb.RegisterKVServer       → NewQuotaKVServer(s)
   ├── pb.RegisterWatchServer    → NewWatchServer(s)
   ├── pb.RegisterLeaseServer    → NewQuotaLeaseServer(s)
   ├── pb.RegisterClusterServer  → NewClusterServer(s)
   ├── pb.RegisterAuthServer     → NewAuthServer(s)
   ├── healthpb.RegisterHealthServer → health.NewServer()
   └── pb.RegisterMaintenanceServer  → NewMaintenanceServer(s, healthNotifier)

3. 메트릭 초기화:
   └── serverMetrics.InitializeMetrics(grpcServer)
```

**왜 Quota 래퍼를 사용하는가?**

`NewQuotaKVServer(s)`와 `NewQuotaLeaseServer(s)`는 쓰기 작업 전에 백엔드의 디스크 용량 쿼타를 확인하는 래퍼이다. 용량 초과 시 쓰기를 거부하여 디스크 풀을 방지한다.

---

## kvServer: KV 서비스 구현

`server/etcdserver/api/v3rpc/key.go`에 정의된 `kvServer`는 KV 서비스의 핵심 구현체이다.

### 구조체

```go
type kvServer struct {
    hdr       header           // ResponseHeader 생성기
    kv        etcdserver.RaftKV // etcdserver (Raft를 통한 KV 연산)
    maxTxnOps uint             // Txn당 최대 연산 수
    pb.UnsafeKVServer          // 새 메서드 추가 시 컴파일 에러 유도
}
```

**`pb.UnsafeKVServer` 임베딩의 의미:**

gRPC-Go에서 서비스 인터페이스에 새 메서드가 추가되면, 모든 구현체가 자동으로 컴파일 에러를 발생시킨다. `UnsafeKVServer`를 임베딩하면 기존 구현이 유지되면서도 새 메서드 미구현 시 감지가 가능하다.

### Range() 메서드

```go
func (s *kvServer) Range(ctx context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error) {
    if err := checkRangeRequest(r); err != nil {
        return nil, err
    }
    resp, err := s.kv.Range(ctx, r)
    if err != nil {
        return nil, togRPCError(err)
    }
    s.hdr.fill(resp.Header)
    return resp, nil
}
```

패턴은 모든 KV 메서드에서 동일하다:
1. **요청 검증** (`check*Request`)
2. **etcdserver 호출** (`s.kv.*`)
3. **에러 변환** (`togRPCError`)
4. **헤더 채우기** (`s.hdr.fill`)

### 요청 검증 함수들

```
checkRangeRequest(r):
├── Key가 비어있으면 → ErrGRPCEmptyKey
├── SortOrder가 유효하지 않으면 → ErrGRPCInvalidSortOption
└── SortTarget이 유효하지 않으면 → ErrGRPCInvalidSortOption

checkPutRequest(r):
├── Key가 비어있으면 → ErrGRPCEmptyKey
├── IgnoreValue=true인데 Value가 있으면 → ErrGRPCValueProvided
└── IgnoreLease=true인데 Lease가 있으면 → ErrGRPCLeaseProvided

checkDeleteRequest(r):
└── Key가 비어있으면 → ErrGRPCEmptyKey

checkTxnRequest(r, maxTxnOps):
├── 연산 수 > maxTxnOps이면 → ErrGRPCTooManyOps
├── Compare의 Key 검증
├── Success 연산 재귀 검증
└── Failure 연산 재귀 검증
```

### Txn() 메서드의 추가 검증

```go
func (s *kvServer) Txn(ctx context.Context, r *pb.TxnRequest) (*pb.TxnResponse, error) {
    if err := checkTxnRequest(r, int(s.maxTxnOps)); err != nil {
        return nil, err
    }
    // put/del 오버랩 확인 (quadratic blowup 방지를 위해 기본 검증 후 수행)
    if _, _, err := checkIntervals(r.Success); err != nil {
        return nil, err
    }
    if _, _, err := checkIntervals(r.Failure); err != nil {
        return nil, err
    }
    // ...
}
```

`checkIntervals()`는 같은 Txn 내에서 Put과 Delete가 같은 키 범위에 겹치는지 확인한다. 겹치면 `ErrGRPCDuplicateKey`를 반환한다. 이 검증은 `IntervalTree`(ADT)를 사용하여 범위 충돌을 효율적으로 감지한다.

---

## header 구조체: ResponseHeader 채우기

`server/etcdserver/api/v3rpc/header.go`에 정의된 `header`는 모든 응답에 클러스터 메타데이터를 추가한다.

### 구조체

```go
type header struct {
    clusterID int64
    memberID  int64
    sg        apply.RaftStatusGetter
    rev       func() int64
}
```

### fill() 메서드

```go
func (h *header) fill(rh *pb.ResponseHeader) {
    if rh == nil {
        panic("unexpected nil resp.Header")
    }
    rh.ClusterId = uint64(h.clusterID)
    rh.MemberId  = uint64(h.memberID)
    rh.RaftTerm  = h.sg.Term()
    if rh.Revision == 0 {
        rh.Revision = h.rev()
    }
}
```

**왜 Revision이 0인 경우에만 채우는가?**

일부 응답(예: Range)은 etcdserver에서 이미 특정 리비전의 데이터를 반환하면서 Revision을 설정한다. 이 경우 덮어쓰면 안 된다. Revision이 0인 경우에만(아직 설정되지 않은 경우) 현재 KV 스토어의 최신 리비전을 채운다.

### ResponseHeader의 필드

```
ResponseHeader:
├── ClusterId  uint64  // 클러스터 고유 ID
├── MemberId   uint64  // 응답한 멤버의 ID
├── Revision   int64   // KV 스토어 리비전
└── RaftTerm   uint64  // 현재 Raft term
```

클라이언트는 이 정보를 사용하여:
- 올바른 클러스터에 연결되어 있는지 확인 (ClusterId)
- 어떤 멤버가 응답했는지 식별 (MemberId)
- Watch의 시작 리비전 결정 (Revision)
- 리더 선출 상태 추적 (RaftTerm)

---

## watchServer와 serverWatchStream: Watch 서비스

`server/etcdserver/api/v3rpc/watch.go`에 구현된 Watch 서비스는 etcd의 가장 복잡한 gRPC API이다. 양방향 스트리밍을 사용하여 실시간 변경 알림을 제공한다.

### watchServer 구조체

```go
type watchServer struct {
    lg            *zap.Logger
    clusterID     int64
    memberID      int64
    maxRequestBytes uint
    sg            apply.RaftStatusGetter
    watchable     mvcc.WatchableKV
    ag            AuthGetter
    pb.UnsafeWatchServer
}
```

`watchServer`는 gRPC Watch 서비스의 진입점이다. 각 클라이언트 스트림에 대해 `serverWatchStream`을 생성한다.

### serverWatchStream 구조체

```go
type serverWatchStream struct {
    lg              *zap.Logger
    clusterID       int64
    memberID        int64
    maxRequestBytes uint
    sg              apply.RaftStatusGetter
    watchable       mvcc.WatchableKV
    ag              AuthGetter

    gRPCStream   pb.Watch_WatchServer    // gRPC 양방향 스트림
    watchStream  mvcc.WatchStream         // MVCC watch 스트림
    ctrlStream   chan *pb.WatchResponse   // 제어 응답 채널 (크기 16)

    mu       sync.RWMutex
    progress map[mvcc.WatchID]bool       // 진행 알림 필요 여부
    prevKV   map[mvcc.WatchID]bool       // 이전 KV 반환 여부
    fragment map[mvcc.WatchID]bool       // 프래그먼트 활성 여부

    closec   chan struct{}               // 스트림 종료 신호
    wg       sync.WaitGroup              // sendLoop 완료 대기
}
```

### ctrlStream 채널 (크기 16)

```go
const ctrlStreamBufLen = 16

ctrlStream: make(chan *pb.WatchResponse, ctrlStreamBufLen)
```

**왜 16인가?**

소스코드 주석에서 설명한다:
> We send ctrl response inside the read loop. We do not want send to block read, but we still want ctrl response we sent to be serialized. Thus we use a buffered chan to solve the problem. A small buffer should be OK for most cases, since we expect the ctrl requests are infrequent.

recvLoop에서 watch 생성/취소 응답을 ctrlStream에 넣고, sendLoop에서 꺼내어 클라이언트에 전송한다. 버퍼가 있어서 recvLoop가 sendLoop를 블로킹하지 않는다. 제어 메시지(생성 확인, 취소 확인)는 드물기 때문에 16이면 충분하다.

---

## recvLoop()/sendLoop(): 이중 고루틴 패턴

Watch 서비스의 핵심은 하나의 gRPC 스트림에 대해 두 개의 고루틴을 운영하는 것이다.

### Watch() 진입점

```go
func (ws *watchServer) Watch(stream pb.Watch_WatchServer) (err error) {
    sws := serverWatchStream{ /* 초기화 */ }

    sws.wg.Add(1)
    go func() {
        sws.sendLoop()    // 이벤트 전송 고루틴
        sws.wg.Done()
    }()

    go func() {
        if rerr := sws.recvLoop(); rerr != nil { // 요청 수신 고루틴
            errc <- rerr
        }
    }()

    select {
    case err = <-errc:       // recvLoop 에러
    case <-stream.Context().Done(): // 스트림 취소
    }

    sws.close()
    return err
}
```

```
┌───────────────────────────────────────────────────┐
│            Watch 스트림 아키텍처                     │
│                                                   │
│  ┌──────────┐                    ┌──────────────┐ │
│  │ 클라이언트 │◄── gRPC Stream ──►│  etcd 서버    │ │
│  └──────────┘                    └──────┬───────┘ │
│                                         │         │
│  서버 내부:                               │         │
│  ┌─────────────────────────────────────▼─┐       │
│  │            serverWatchStream            │       │
│  │                                        │       │
│  │   recvLoop()          sendLoop()       │       │
│  │   (고루틴 1)          (고루틴 2)        │       │
│  │      │                    │            │       │
│  │      │  ┌──────────┐     │            │       │
│  │      ├─►│ctrlStream│────►┤            │       │
│  │      │  │ (크기 16) │     │            │       │
│  │      │  └──────────┘     │            │       │
│  │      │                    │            │       │
│  │      │  ┌──────────┐     │            │       │
│  │      │  │watchStream│     │            │       │
│  │      │  │  .Chan()  │────►┤            │       │
│  │      │  └──────────┘     │            │       │
│  │      │                    │            │       │
│  │      ▼                    ▼            │       │
│  │  stream.Recv()      stream.Send()     │       │
│  └────────────────────────────────────────┘       │
└───────────────────────────────────────────────────┘
```

### recvLoop(): 요청 수신

```
recvLoop() 흐름:
무한 루프:
  1. stream.Recv() → req
  2. req.RequestUnion 타입 분기:

  ── WatchCreateRequest:
     ├── Key/RangeEnd 정규화
     │   ├── 빈 Key → []byte{0} (최소 키)
     │   ├── 빈 RangeEnd → nil (단일 키)
     │   └── RangeEnd == {0} → [] (>= 쿼리)
     ├── StartRevision < 0 → 에러 응답 (Compacted)
     ├── 권한 검사: isWatchPermitted(creq)
     ├── 필터 생성: FiltersFromRequest(creq)
     ├── watchStream.Watch() 호출 → watchID 반환
     ├── progress, prevKV, fragment 맵 업데이트
     └── ctrlStream에 생성 확인 응답 전송

  ── WatchCancelRequest:
     ├── watchStream.Cancel(id) 호출
     ├── ctrlStream에 취소 확인 응답 전송
     └── progress, prevKV, fragment 맵에서 삭제

  ── ProgressRequest:
     └── watchStream.RequestProgressAll() 호출
```

### sendLoop(): 이벤트 전송

```
sendLoop() 흐름:
progressTicker 생성 (기본 10분 + 지터)

무한 루프:
  select:
  ── watchStream.Chan() → wresp (MVCC 이벤트):
     ├── 이벤트를 []*mvccpb.Event로 변환
     ├── prevKV 필요 시: 이전 리비전에서 KV 조회
     ├── WatchResponse 구성:
     │   ├── Header (newResponseHeader)
     │   ├── WatchId
     │   ├── Events
     │   ├── CompactRevision
     │   └── Canceled
     ├── 아직 생성 확인이 안 된 Watch ID:
     │   └── pending 맵에 버퍼링
     ├── 프래그먼트 활성화 시:
     │   └── sendFragments()로 분할 전송
     └── gRPCStream.Send()로 전송

  ── ctrlStream → c (제어 응답):
     ├── gRPCStream.Send(c)
     ├── 취소 응답이면: ids에서 삭제
     └── 생성 응답이면:
         ├── ids에 추가
         └── pending 버퍼 flush

  ── progressTicker.C:
     ├── progress 맵의 각 Watch ID에 대해
     └── watchStream.RequestProgress(id) 호출

  ── closec:
     └── 반환 (루프 종료)
```

**왜 pending 맵이 필요한가?**

Watch 생성 확인(ctrlStream)이 클라이언트에 전송되기 전에, MVCC에서 해당 Watch의 이벤트가 먼저 도착할 수 있다. 클라이언트가 아직 모르는 Watch ID의 이벤트를 받으면 혼란이 발생한다. 따라서 생성 확인이 전송될 때까지 이벤트를 pending 맵에 버퍼링하고, 생성 확인 후 한꺼번에 flush한다.

### sendFragments(): 대용량 응답 분할

```go
func sendFragments(wr *pb.WatchResponse, maxRequestBytes uint, sendFunc func(*pb.WatchResponse) error) error {
    if uint(wr.Size()) < maxRequestBytes || len(wr.Events) < 2 {
        return sendFunc(wr)  // 분할 불필요
    }
    // Fragment=true로 설정하여 분할 전송
    // 마지막 조각만 Fragment=false
}
```

단일 Watch 이벤트가 `maxRequestBytes`를 초과하면 여러 응답으로 분할한다. 이벤트가 1개뿐이면 분할할 수 없으므로 그대로 전송한다.

### 진행 알림 (Progress Notification)

```go
progressReportInterval = 10 * time.Minute  // 기본값
```

진행 알림은 이벤트가 없는 Watch에 주기적으로 현재 리비전을 알려준다. 클라이언트는 이를 통해 연결이 살아있음과 현재 리비전을 확인한다. 지터(jitter)를 추가하여 동시에 생성된 Watch들이 동시에 알림을 받지 않도록 한다.

```go
func GetProgressReportInterval() time.Duration {
    // ...
    jitter := time.Duration(rand.Int63n(int64(interval) / 10))
    return interval + jitter
}
```

### 필터 시스템

```go
func FiltersFromRequest(creq *pb.WatchCreateRequest) []mvcc.FilterFunc {
    // WatchCreateRequest_NOPUT  → filterNoPut  (PUT 이벤트 제외)
    // WatchCreateRequest_NODELETE → filterNoDelete (DELETE 이벤트 제외)
}
```

클라이언트가 관심 없는 이벤트 타입을 필터링할 수 있다.

---

## LeaseServer: Lease 서비스 구현

`server/etcdserver/api/v3rpc/lease.go`에 정의된 `LeaseServer`는 Lease 관련 gRPC API를 구현한다.

### 구조체

```go
type LeaseServer struct {
    lg  *zap.Logger
    hdr header
    le  etcdserver.Lessor
    pb.UnsafeLeaseServer
}
```

### 단항 RPC: LeaseGrant, LeaseRevoke, LeaseTimeToLive, LeaseLeases

```go
func (ls *LeaseServer) LeaseGrant(ctx context.Context, cr *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
    resp, err := ls.le.LeaseGrant(ctx, cr)
    if err != nil {
        return nil, togRPCError(err)
    }
    ls.hdr.fill(resp.Header)
    return resp, nil
}
```

모든 단항 RPC는 동일한 패턴을 따른다: etcdserver 호출 → 에러 변환 → 헤더 채우기.

`LeaseTimeToLive`는 특수 처리가 있다: Lease가 없으면 에러 대신 TTL=-1인 응답을 반환한다.

```go
func (ls *LeaseServer) LeaseTimeToLive(ctx context.Context, rr *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
    resp, err := ls.le.LeaseTimeToLive(ctx, rr)
    if err != nil && !errors.Is(err, lease.ErrLeaseNotFound) {
        return nil, togRPCError(err)
    }
    if errors.Is(err, lease.ErrLeaseNotFound) {
        resp = &pb.LeaseTimeToLiveResponse{
            Header: &pb.ResponseHeader{},
            ID:     rr.ID,
            TTL:    -1,  // Lease 없음을 나타냄
        }
    }
    ls.hdr.fill(resp.Header)
    return resp, nil
}
```

### LeaseKeepAlive: 양방향 스트리밍

```
LeaseKeepAlive(stream) 흐름:
1. 별도 고루틴에서 leaseKeepAlive() 실행
2. select:
   ├── <-errc → leaseKeepAlive 에러
   └── <-stream.Context().Done() → 스트림 종료
      원인: 클라이언트 취소 / 리더 없음 / 서버 종료

leaseKeepAlive(stream) 무한 루프:
1. stream.Recv() → req (KeepAlive 요청)
2. ResponseHeader 생성 (Renew 전에!)
   └── 이유: 리비전이 실제 갱신 시점보다 크면 안 됨
3. ls.le.LeaseRenew(ctx, LeaseID(req.ID))
4. ErrLeaseNotFound이면 → TTL=0 (만료됨)
5. resp.TTL = ttl
6. stream.Send(resp)
```

**왜 ResponseHeader를 Renew 전에 생성하는가?**

소스코드 주석:
> Create header before we sent out the renew request. This can make sure that the revision is strictly smaller or equal to when the keepalive happened at the local server (when the local server is the leader) or remote leader.

Lease가 rev 3에서 리보크되었는데 KeepAlive 응답의 리비전이 4이면, 클라이언트는 rev 4 시점에 Lease가 살아있다고 잘못 판단할 수 있다.

---

## 인터셉터 시스템

`server/etcdserver/api/v3rpc/interceptor.go`에 정의된 인터셉터는 모든 gRPC 요청에 대한 전처리를 담당한다.

### Unary 인터셉터 체인

```
Unary 인터셉터 실행 순서:
1. newLogUnaryInterceptor → 요청 시간 측정, 느린 요청 경고
2. serverMetrics          → Prometheus 메트릭 수집
3. newUnaryInterceptor    → 능력 검증, 리더 확인
4. (외부 인터셉터)         → 선택적
```

### newUnaryInterceptor: 핵심 검증

```
newUnaryInterceptor(s) 흐름:
1. V3rpcCapability 활성화 확인 → ErrGRPCNotCapable
2. 학습자(Learner) 멤버 제한:
   └── 학습자는 직렬화된 Range와 Status만 허용
3. 메타데이터에서 클라이언트 API 버전 확인
4. UTF-8 유효성 검증
5. require-leader 헤더 확인:
   └── 리더 없으면 ErrGRPCNoLeader
```

### newStreamInterceptor: 스트림 검증

```
newStreamInterceptor(s) 흐름:
1. V3rpcCapability 확인
2. 학습자 멤버 제한 (Snapshot 제외)
3. require-leader 헤더 확인:
   ├── 리더 없으면 ErrGRPCNoLeader
   └── 리더 있으면:
       ├── cancellableContext 생성
       ├── smap.streams에 등록
       └── defer에서 정리
```

### monitorLeader: 리더 감시

```go
func monitorLeader(s *etcdserver.EtcdServer) *streamsMap {
    smap := &streamsMap{streams: make(map[grpc.ServerStream]struct{})}
    s.GoAttach(func() {
        election := TickMs * ElectionTicks  // 선거 타임아웃
        noLeaderCnt := 0
        for {
            select {
            case <-s.StoppingNotify():
                return
            case <-time.After(election):
                if s.Leader() == None {
                    noLeaderCnt++
                } else {
                    noLeaderCnt = 0
                }
                // maxNoLeaderCnt(3)번 연속 리더 없으면 모든 스트림 취소
                if noLeaderCnt >= maxNoLeaderCnt {
                    for ss := range smap.streams {
                        ssWithCtx.ctx.Cancel(ErrGRPCNoLeader)
                    }
                }
            }
        }
    })
    return smap
}
```

**왜 즉시 취소하지 않고 3번(maxNoLeaderCnt) 대기하는가?**

소스코드 주석:
> We are more conservative on canceling existing streams. Reconnecting streams cost much more than just rejecting new requests. So we wait until the member cannot find a leader for maxNoLeaderCnt election timeouts to cancel existing streams.

새 요청 거부는 저렴하지만, 기존 Watch 스트림 취소 후 재연결은 비용이 크다. 리더 선출이 곧 완료될 수 있으므로 3번의 선거 타임아웃을 기다린다.

### cancellableContext

```go
type cancellableContext struct {
    context.Context
    lock         sync.RWMutex
    cancel       context.CancelFunc
    cancelReason error
}
```

표준 context의 취소는 원인을 보존하지 않는다. `cancellableContext`는 취소 사유(예: `ErrGRPCNoLeader`)를 보존하여 `Err()` 호출 시 반환한다. 클라이언트는 이를 통해 취소 원인을 구분하고 적절히 대응할 수 있다.

### newLogUnaryInterceptor: 요청 로깅

```
logUnaryRequestStats() 흐름:
1. 요청 처리 시간 측정
2. duration > warnLatency이면 expensive 요청으로 분류
3. 응답 타입별 통계 수집:
   ├── RangeResponse  → reqCount, reqSize, respCount, respSize
   ├── PutResponse    → 값 필드 리다팅 (보안)
   ├── DeleteRangeResponse
   └── TxnResponse    → Success/Failure 분기
4. Debug 레벨: 모든 요청 로깅
5. Warn 레벨: 느린 요청만 로깅
```

---

## 에러 변환: togRPCError

`server/etcdserver/api/v3rpc/util.go`의 `togRPCError()` 함수는 내부 에러를 gRPC 에러 코드로 변환한다.

```go
var toGRPCErrorMap = map[error]error{
    // KV 관련
    mvcc.ErrCompacted:         rpctypes.ErrGRPCCompacted,
    mvcc.ErrFutureRev:         rpctypes.ErrGRPCFutureRev,

    // Lease 관련
    lease.ErrLeaseNotFound:    rpctypes.ErrGRPCLeaseNotFound,
    lease.ErrLeaseExists:      rpctypes.ErrGRPCLeaseExist,

    // Auth 관련
    auth.ErrAuthFailed:        rpctypes.ErrGRPCAuthFailed,
    auth.ErrPermissionDenied:  rpctypes.ErrGRPCPermissionDenied,

    // 클러스터 관련
    errors.ErrNoLeader:        rpctypes.ErrGRPCNoLeader,
    errors.ErrNotLeader:       rpctypes.ErrGRPCNotLeader,
    // ... 약 40개 매핑
}

func togRPCError(err error) error {
    if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
        return err  // gRPC가 자체 변환
    }
    grpcErr, ok := toGRPCErrorMap[err]
    if !ok {
        return status.Error(codes.Unknown, err.Error())
    }
    return grpcErr
}
```

---

## gRPC 스트리밍 Watch의 양방향 통신

### 통신 흐름

```
클라이언트                          서버
   │                                │
   │── WatchCreateRequest ────────►│
   │                                │ recvLoop: watchStream.Watch()
   │◄── WatchResponse(Created) ────│ sendLoop: ctrlStream에서 수신
   │                                │
   │                                │ MVCC 이벤트 발생
   │◄── WatchResponse(Events) ────│ sendLoop: watchStream.Chan()에서 수신
   │◄── WatchResponse(Events) ────│
   │                                │
   │── WatchCancelRequest ────────►│
   │                                │ recvLoop: watchStream.Cancel()
   │◄── WatchResponse(Canceled) ──│ sendLoop: ctrlStream에서 수신
   │                                │
   │     (이벤트 없음, 10분 경과)      │
   │◄── WatchResponse(Progress) ──│ sendLoop: progressTicker
   │                                │
   │── ProgressRequest ──────────►│
   │                                │ recvLoop: RequestProgressAll()
   │◄── WatchResponse(Progress) ──│
   │                                │
   │── (연결 끊김) ───────────────►│
   │                                │ close(): watchStream.Close()
```

### 하나의 gRPC 스트림에 여러 Watch

하나의 gRPC 양방향 스트림 위에 여러 개의 Watch를 다중화(multiplex)할 수 있다. 각 Watch는 고유한 `WatchID`로 식별되며, 클라이언트는 같은 스트림에서 여러 WatchCreateRequest를 보낼 수 있다. 이는 연결 수를 줄이고 리소스 효율성을 높인다.

---

## 서비스별 구현 파일 참조

| 파일 경로 | 서비스 | 핵심 내용 |
|----------|--------|----------|
| `server/etcdserver/api/v3rpc/grpc.go` | 전체 | Server 함수, 서비스 등록, 인터셉터 체인 구성 |
| `server/etcdserver/api/v3rpc/key.go` | KV | kvServer, Range/Put/DeleteRange/Txn/Compact |
| `server/etcdserver/api/v3rpc/watch.go` | Watch | watchServer, serverWatchStream, recvLoop/sendLoop |
| `server/etcdserver/api/v3rpc/lease.go` | Lease | LeaseServer, LeaseKeepAlive 스트리밍 |
| `server/etcdserver/api/v3rpc/auth.go` | Auth | AuthServer, AuthGetter, AuthAdmin |
| `server/etcdserver/api/v3rpc/header.go` | 공통 | header 구조체, ResponseHeader fill |
| `server/etcdserver/api/v3rpc/interceptor.go` | 공통 | Unary/Stream 인터셉터, monitorLeader |
| `server/etcdserver/api/v3rpc/util.go` | 공통 | togRPCError, isClientCtxErr, isRPCSupportedForLearner |
| `api/etcdserverpb/rpc.proto` | 전체 | 6개 서비스와 메시지 정의 |

---

## gRPC API 계층의 설계 원칙 정리

```
┌─────────────────────────────────────────────────────────────┐
│                    gRPC API 계층 아키텍처                      │
│                                                             │
│  클라이언트 요청                                              │
│       │                                                     │
│       ▼                                                     │
│  ┌─────────────────────────────────────────────────┐        │
│  │  인터셉터 체인                                     │        │
│  │  ┌─────────┐ ┌────────┐ ┌──────────┐ ┌───────┐ │        │
│  │  │로깅     │→│메트릭  │→│능력/리더  │→│(외부) │ │        │
│  │  │인터셉터 │ │인터셉터│ │인터셉터  │ │인터셉터│ │        │
│  │  └─────────┘ └────────┘ └──────────┘ └───────┘ │        │
│  └─────────────────────────────────────────────────┘        │
│       │                                                     │
│       ▼                                                     │
│  ┌─────────────────────────────────────────────────┐        │
│  │  서비스 구현 (kvServer, watchServer 등)            │        │
│  │  1. 요청 검증 (check*Request)                     │        │
│  │  2. etcdserver 호출                               │        │
│  │  3. 에러 변환 (togRPCError)                       │        │
│  │  4. 헤더 채우기 (hdr.fill)                        │        │
│  └─────────────────────────────────────────────────┘        │
│       │                                                     │
│       ▼                                                     │
│  ┌─────────────────────────────────────────────────┐        │
│  │  etcdserver (RAFT, MVCC, Lease 등)               │        │
│  └─────────────────────────────────────────────────┘        │
└─────────────────────────────────────────────────────────────┘
```

핵심 설계 원칙:

1. **관심사 분리**: gRPC 계층은 전송/직렬화만 담당, 비즈니스 로직은 etcdserver에 위임
2. **일관된 패턴**: 모든 단항 RPC가 검증 → 호출 → 에러 변환 → 헤더 채우기의 동일 패턴
3. **인터셉터로 횡단 관심사 처리**: 로깅, 메트릭, 인증, 능력 검증을 인터셉터 체인에서 처리
4. **이중 고루틴 패턴**: Watch와 LeaseKeepAlive에서 수신/전송을 독립 고루틴으로 분리하여 양방향 스트리밍의 성능과 안정성 확보
5. **보수적 스트림 관리**: 기존 스트림 취소는 신중하게 (maxNoLeaderCnt 대기)
6. **정확한 에러 전파**: 내부 에러를 gRPC 에러 코드로 정확히 매핑하여 클라이언트가 적절히 대응 가능
