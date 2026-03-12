# etcd 아키텍처

## 1. 개요

etcd는 분산 시스템의 핵심 데이터를 저장하는 신뢰할 수 있는 분산 키-값 저장소이다. Raft 합의 알고리즘 위에 MVCC(Multi-Version Concurrency Control) 저장소를 구축하여, 강력한 일관성(Strong Consistency)과 고가용성(High Availability)을 동시에 제공한다.

### 핵심 설계 원칙

| 원칙 | 구현 |
|------|------|
| 강력한 일관성 | Raft 합의 → 모든 쓰기가 과반수 동의 필요 |
| 고가용성 | (N-1)/2 노드 장애 허용 (3노드: 1대, 5노드: 2대) |
| 순서 보장 | 전역 리비전 번호로 모든 변경 순서 추적 |
| 원자적 연산 | 트랜잭션(Txn)으로 Compare-And-Swap 지원 |
| 실시간 알림 | Watch 메커니즘으로 변경 이벤트 스트리밍 |

## 2. 전체 아키텍처

```
┌─────────────────────────────────────────────────────────────────────┐
│                           Client (etcdctl, SDK)                      │
│                          gRPC / HTTP+JSON Gateway                    │
└────────────────────────────────┬────────────────────────────────────┘
                                 │
┌────────────────────────────────▼────────────────────────────────────┐
│                          gRPC Server                                 │
│                                                                      │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐           │
│  │ KV       │  │ Watch    │  │ Lease    │  │ Cluster  │           │
│  │ Server   │  │ Server   │  │ Server   │  │ Server   │           │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘           │
│       │              │              │              │                 │
│  ┌──────────┐  ┌──────────┐                                        │
│  │Maintenance│  │  Auth   │                                        │
│  │ Server    │  │ Server  │                                        │
│  └────┬──────┘  └────┬────┘                                        │
└───────┼──────────────┼─────────────────────────────────────────────┘
        │              │
┌───────▼──────────────▼─────────────────────────────────────────────┐
│                        EtcdServer                                    │
│                                                                      │
│  ┌─────────────────────────────────────────────────────────┐        │
│  │                    요청 처리 계층                          │        │
│  │  Range() → linearizableReadNotify() (선형 읽기)          │        │
│  │  Put()   → raftRequest() → Raft Propose                 │        │
│  │  Txn()   → 읽기전용 vs 쓰기 분기                          │        │
│  └────────────────────────┬────────────────────────────────┘        │
│                           │                                          │
│  ┌────────────────────────▼────────────────────────────────┐        │
│  │                    raftNode                              │        │
│  │  Propose() → Raft 합의 → Ready() → WAL + Apply          │        │
│  │  ProposeConfChange() → 멤버십 변경                        │        │
│  └────────────────────────┬────────────────────────────────┘        │
│                           │                                          │
│  ┌────────────┬───────────┼───────────┬────────────────────┐        │
│  │            │           │           │                    │        │
│  ▼            ▼           ▼           ▼                    ▼        │
│ WAL        Backend      MVCC KV    Lessor            AuthStore     │
│ (선행로그)  (BoltDB)    (다중버전)  (TTL관리)          (RBAC)       │
│                           │                                          │
│                     ┌─────▼─────┐                                   │
│                     │ WatchableKV│                                   │
│                     │  synced    │                                   │
│                     │  unsynced  │                                   │
│                     │  victims   │                                   │
│                     └───────────┘                                    │
└─────────────────────────────────────────────────────────────────────┘
```

## 3. 핵심 컴포넌트

### 3.1 gRPC API 계층

etcd는 6개의 gRPC 서비스를 제공한다. 모두 `api/etcdserverpb/rpc.proto`에 정의되어 있다.

| 서비스 | 핵심 RPC | 역할 |
|--------|---------|------|
| KV | Range, Put, DeleteRange, Txn, Compact | 핵심 키-값 연산 |
| Watch | Watch (양방향 스트림) | 이벤트 실시간 구독 |
| Lease | LeaseGrant, LeaseRevoke, LeaseKeepAlive | TTL 기반 임시 키 |
| Cluster | MemberAdd, MemberRemove, MemberUpdate | 클러스터 멤버십 관리 |
| Maintenance | Alarm, Status, Defragment, Snapshot | 운영/관리 명령 |
| Auth | AuthEnable, Authenticate, UserAdd, RoleAdd | RBAC 인증/권한 |

각 서비스는 `server/etcdserver/api/v3rpc/` 디렉토리에 구현되어 있다:

```go
// server/etcdserver/api/v3rpc/key.go
type kvServer struct {
    hdr header
    kv  etcdserver.RaftKV   // EtcdServer가 구현
    maxTxnOps uint
}
```

### 3.2 EtcdServer (조율자)

`server/etcdserver/server.go`에 정의된 EtcdServer는 모든 하위 시스템을 조율하는 중심 구조체이다.

```go
// server/etcdserver/server.go
type EtcdServer struct {
    // Raft 관련 (원자 연산용 64비트 정렬)
    appliedIndex   atomic.Uint64   // 적용된 Raft 인덱스
    committedIndex atomic.Uint64   // 커밋된 Raft 인덱스
    term           atomic.Uint64   // 현재 Raft 텀
    lead           atomic.Uint64   // 리더 ID

    r raftNode                      // Raft 노드

    // 핵심 저장소
    kv        mvcc.WatchableKV     // MVCC 키-값 저장소
    lessor    lease.Lessor         // Lease 관리자
    be        backend.Backend      // BoltDB 백엔드
    authStore auth.AuthStore       // 인증 저장소

    // 클러스터
    cluster     *membership.RaftCluster
    snapshotter *snap.Snapshotter

    // 채널
    readych  chan struct{}          // 준비 완료 알림
    stop     chan struct{}          // 종료 신호
    done     chan struct{}          // 완료 알림
}
```

### 3.3 Raft 노드

`server/etcdserver/raft.go`에 정의된 raftNode는 etcd/raft 라이브러리를 감싸는 래퍼이다.

```go
// server/etcdserver/raft.go
type raftNode struct {
    raftNodeConfig

    msgSnapC   chan raftpb.Message   // 스냅샷 메시지
    applyc     chan toApply          // 적용할 항목
    readStateC chan raft.ReadState   // ReadIndex 응답

    ticker  *time.Ticker            // Raft 틱 타이머
    td      *contention.TimeoutDetector
}

type toApply struct {
    entries  []raftpb.Entry         // 적용할 엔트리
    snapshot raftpb.Snapshot        // 적용할 스냅샷
    notifyc  chan struct{}          // 완료 알림
}
```

### 3.4 MVCC 저장소

`server/storage/mvcc/kvstore.go`에 정의된 store는 다중 버전 키-값 저장소이다.

```go
// server/storage/mvcc/kvstore.go
type store struct {
    b       backend.Backend    // BoltDB 백엔드
    kvindex index              // B-tree 인덱스

    currentRev     int64       // 현재 리비전
    compactMainRev int64       // 마지막 컴팩션 리비전

    fifoSched schedule.Scheduler  // 비동기 컴팩션 스케줄러
}
```

### 3.5 Watch 시스템

`server/storage/mvcc/watchable_store.go`에 정의된 watchableStore는 store를 감싸서 이벤트 알림을 추가한다.

```go
// server/storage/mvcc/watchable_store.go
type watchableStore struct {
    *store

    unsynced watcherGroup      // 과거 이벤트 동기화 필요
    synced   watcherGroup      // 현재와 동기화됨
    victims  []watcherBatch    // 채널 막힌 워처들
}
```

### 3.6 BoltDB 백엔드

`server/storage/backend/backend.go`에 정의된 backend는 BoltDB를 감싸서 배치 트랜잭션과 읽기 최적화를 제공한다.

```go
// server/storage/backend/backend.go
type backend struct {
    db    *bolt.DB

    batchInterval time.Duration  // 기본 100ms
    batchLimit    int            // 기본 10,000
    batchTx       *batchTxBuffered
    readTx        *readTx
}
```

## 4. 초기화 흐름

etcd 서버의 시작 과정은 다음과 같다:

```
main()                                    // server/etcdmain/main.go
  └─ Main(args)
      ├─ checkSupportArch()              // 아키텍처 검증
      └─ startEtcdOrProxyV2(args)        // server/etcdmain/etcd.go
          ├─ newConfig()                 // 설정 객체 생성
          ├─ cfg.parse(args)             // 명령행 인자 파싱
          ├─ identifyDataDirOrDie()      // 데이터 디렉토리 타입 확인
          └─ startEtcd(&cfg.ec)
              └─ embed.StartEtcd(cfg)    // server/embed/etcd.go
                  ├─ inCfg.Validate()
                  ├─ configurePeerListeners()
                  ├─ configureClientListeners()
                  ├─ etcdserver.NewServer()
                  │   ├─ bootstrap(cfg)
                  │   │   ├─ WAL 초기화/복구
                  │   │   ├─ Raft 스토리지 구성
                  │   │   └─ 클러스터 정보 로드
                  │   ├─ lease.NewLessor()
                  │   ├─ mvcc.New()
                  │   ├─ auth.NewAuthStore()
                  │   ├─ v3compactor.New()
                  │   └─ rafthttp.Transport.Start()
                  ├─ e.Server.Start()
                  │   ├─ run()           // 메인 이벤트 루프
                  │   │   └─ r.start()   // Raft 노드 시작
                  │   ├─ linearizableReadLoop()
                  │   ├─ publishV3()
                  │   ├─ purgeFile()
                  │   └─ monitor*()      // 모니터링 고루틴들
                  ├─ servePeers()
                  ├─ serveClients()
                  └─ serveMetrics()
```

### 4.1 bootstrap 과정

`NewServer()`의 첫 번째 단계인 bootstrap은 디스크 상태를 복구한다:

```
bootstrap(cfg)
  ├─ WAL 존재 확인
  │   ├─ 있음: 기존 데이터에서 복구
  │   │   ├─ WAL 열기 + 재생
  │   │   ├─ 스냅샷 로드
  │   │   └─ Raft MemoryStorage 복원
  │   └─ 없음: 신규 클러스터 시작
  │       ├─ WAL 생성
  │       ├─ 초기 스냅샷 저장
  │       └─ 클러스터 정보 초기화
  ├─ Backend(BoltDB) 열기
  └─ 클러스터 멤버십 복원
```

### 4.2 메인 이벤트 루프 (run)

`server/etcdserver/server.go`의 `run()` 메서드는 서버의 심장이다:

```go
// server/etcdserver/server.go - run()
func (s *EtcdServer) run() {
    sched := schedule.NewFIFOScheduler()

    // Raft Ready 핸들러 구성
    rh := &raftReadyHandler{
        updateLead:          func(lead uint64) { ... },
        updateLeadership:    func(isLeader bool) { ... },
        updateCommittedIndex: func(ci uint64) { ... },
    }

    s.r.start(rh)  // Raft 노드 시작

    for {
        select {
        case ap := <-s.r.apply():
            // Raft에서 적용할 항목 수신
            f := schedule.NewJob("server_applyAll", func(ctx) {
                s.applyAll(&ep, &ap)  // WAL → Apply → Response
            })
            sched.Schedule(f)
        case leases := <-expiredLeaseC:
            // 만료된 Lease 처리
            s.revokeExpiredLeases(leases)
        case err := <-s.errorc:
            return  // 에러 시 종료
        case <-s.stop:
            return  // 종료 신호
        }
    }
}
```

## 5. 요청 처리 흐름

### 5.1 쓰기 요청 (Put)

```
Client → gRPC Put
  → kvServer.Put()                    // v3rpc/key.go
    → checkPutRequest()               // 유효성 검증
    → s.kv.Put(ctx, r)                // EtcdServer.Put()
      → raftRequest(InternalRaftRequest{Put: r})
        → r.Propose(ctx, data)        // Raft 제안
          → Raft 합의 (과반수 동의)
          → WAL에 기록
          → apply() 채널로 전달
          → applyAll()
            → uberApply.Apply(entries)
              → MVCC store.Put()
                → BoltDB에 저장
                → 인덱스 업데이트
                → Watch 이벤트 발생
          → 응답 반환
```

### 5.2 읽기 요청 (Range)

```
Client → gRPC Range
  → kvServer.Range()                  // v3rpc/key.go
    → s.kv.Range(ctx, r)              // EtcdServer.Range()
      → Serializable?
        → YES: 로컬 MVCC에서 직접 읽기
        → NO:  linearizableReadNotify()
                → ReadIndex 프로토콜
                → 리더 확인 + quorum 동의
                → 읽기 허용
      → MVCC store.Range()
        → kvindex.Range() → 리비전 조회
        → backend.Range() → KV 데이터 읽기
        → 응답 반환
```

### 5.3 Watch 요청

```
Client → gRPC Watch (양방향 스트림)
  → watchServer.Watch(stream)         // v3rpc/watch.go
    → serverWatchStream 생성
    → recvLoop() 고루틴
      → WatchCreateRequest 수신
      → mvcc.WatchStream.Watch() 등록
    → sendLoop() 고루틴
      → mvcc.WatchStream에서 이벤트 수신
      → WatchResponse 스트림 전송
```

## 6. 데이터 흐름 계층

```
┌──────────────────────────────────────────────────────────────┐
│ Layer 1: gRPC API                                             │
│ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐        │
│ │ KVServer │ │WatchSvr  │ │LeaseSvr  │ │AuthSvr  │        │
│ └────┬─────┘ └────┬─────┘ └────┬─────┘ └────┬─────┘        │
│      │ 검증 + 요청 위임         │              │              │
└──────┼───────────┼──────────────┼──────────────┼────────────┘
       │           │              │              │
┌──────▼───────────▼──────────────▼──────────────▼────────────┐
│ Layer 2: EtcdServer (조율 계층)                               │
│ ┌──────────────────────────────────────────────────────┐     │
│ │  raftRequest() → Raft Propose → 합의 → Apply          │     │
│ │  linearizableReadNotify() → ReadIndex → 로컬 읽기     │     │
│ └──────────────────────────────────────────────────────┘     │
└──────┬───────────┬──────────────┬──────────────┬────────────┘
       │           │              │              │
┌──────▼───────────▼──────────────▼──────────────▼────────────┐
│ Layer 3: 저장소 계층                                          │
│ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐        │
│ │ MVCC KV  │ │ Lessor   │ │AuthStore │ │   WAL    │        │
│ │ (store)  │ │ (TTL)    │ │ (RBAC)   │ │ (log)   │        │
│ └────┬─────┘ └────┬─────┘ └────┬─────┘ └──────────┘        │
│      │              │              │                         │
│      └──────────────┼──────────────┘                         │
│                     │                                        │
│              ┌──────▼──────┐                                 │
│              │  Backend    │                                 │
│              │  (BoltDB)   │                                 │
│              └─────────────┘                                 │
└──────────────────────────────────────────────────────────────┘
```

## 7. 핵심 상수

`server/etcdserver/server.go`에 정의된 주요 상수들:

| 상수 | 값 | 의미 |
|------|---|------|
| DefaultSnapshotCount | 10,000 | 스냅샷 트리거 Raft 엔트리 수 |
| DefaultSnapshotCatchUpEntries | 5,000 | 느린 팔로워 따라잡기 엔트리 |
| HealthInterval | 5s | 헬스 체크 주기 |
| purgeFileInterval | 30s | 파일 정리 주기 |
| maxInFlightMsgSnap | 16 | 동시 스냅샷 메시지 수 |
| reservedInternalFDNum | 150 | 내부용 예약 파일 디스크립터 |

## 8. 고루틴 구조

etcd 서버가 시작되면 다음 고루틴들이 동시에 실행된다:

```
EtcdServer.Start()
  │
  ├─ run()                    // 메인 이벤트 루프 (Raft apply)
  │   └─ raftNode.start()     // Raft tick + Ready 처리
  │
  ├─ linearizableReadLoop()   // 선형 읽기 처리
  ├─ publishV3()              // v3 엔드포인트 발행
  ├─ purgeFile()              // WAL/스냅샷 파일 정리
  ├─ adjustTicks()            // 선거 틱 조정
  ├─ monitorFileDescriptor()  // FD 모니터링
  ├─ monitorClusterVersions() // 클러스터 버전 추적
  ├─ monitorStorageVersion()  // 저장소 버전 추적
  ├─ monitorKVHash()          // KV 해시 무결성 검사
  ├─ monitorCompactHash()     // 컴팩션 해시 검사
  └─ monitorDowngrade()       // 다운그레이드 모니터링
```

watchableStore도 별도 고루틴을 실행한다:
```
watchableStore
  ├─ syncWatchersLoop()       // 100ms 주기, unsynced → synced
  └─ syncVictimsLoop()        // 10ms 주기, 막힌 워처 재시도
```

## 9. 클러스터 통신

etcd 노드 간 통신은 두 가지 경로를 사용한다:

```
┌──────────────┐              ┌──────────────┐
│   Node 1     │              │   Node 2     │
│              │              │              │
│  rafthttp    │◄────────────►│  rafthttp    │
│  Transport   │  Peer URL    │  Transport   │
│  :2380       │  (Raft 메시지)│  :2380       │
│              │              │              │
│  gRPC        │◄─── Client ──│  gRPC        │
│  :2379       │              │  :2379       │
└──────────────┘              └──────────────┘
```

- **Peer 채널 (:2380)**: Raft 메시지, 스냅샷 전송, 멤버십 변경
- **Client 채널 (:2379)**: gRPC API 요청 처리

### Raft HTTP Transport

`server/etcdserver/api/rafthttp/` 패키지가 노드 간 Raft 메시지를 전달한다:

- **Stream**: 장기 HTTP 연결로 작은 메시지 전송
- **Pipeline**: 독립 HTTP 요청으로 큰 메시지(스냅샷) 전송

## 10. 장애 복구

### 리더 장애 시

```
1. 팔로워들이 heartbeat 타임아웃 감지
2. 선거 타이머 만료된 노드가 후보(Candidate) 상태로 전환
3. 과반수 투표 획득 시 새 리더 선출
4. 새 리더가 커밋되지 않은 로그 재적용
5. 클라이언트 요청 재처리
```

### 노드 재시작 시

```
1. WAL에서 Raft 로그 재생
2. 스냅샷에서 마지막 안정 상태 복원
3. 스냅샷 이후 WAL 엔트리 재적용
4. MVCC 인덱스(treeIndex) 재구성
5. Lease 복원
6. 클러스터 멤버로 재합류
```

## 11. 설정 구조

`server/config/config.go`의 ServerConfig에서 주요 설정:

```go
type ServerConfig struct {
    Name            string        // 노드 이름
    DataDir         string        // 데이터 디렉토리
    DedicatedWALDir string        // 별도 WAL 디렉토리

    // Raft 타이밍
    TickMs        uint            // Raft 틱 주기 (기본 100ms)
    ElectionTicks int             // 선거 타임아웃 (기본 10 틱 = 1초)

    // 스냅샷
    SnapshotCount          uint64 // 스냅샷 생성 엔트리 수
    SnapshotCatchUpEntries uint64 // 따라잡기 엔트리 수

    // 자동 컴팩션
    AutoCompactionMode      string        // "periodic" 또는 "revision"
    AutoCompactionRetention time.Duration // 보존 기간/리비전

    // 리소스 제한
    QuotaBackendBytes int64       // BoltDB 크기 제한 (기본 2GB)
    MaxTxnOps         uint        // 트랜잭션당 최대 연산 수
    MaxRequestBytes   uint        // 요청 최대 크기

    // 보안
    AuthToken  string             // "simple" 또는 "jwt"
    BcryptCost uint               // bcrypt 해싱 강도
}
```

## 12. 아키텍처 요약

```
┌─────────────────────────────────────────────────────────┐
│                   etcd 아키텍처 요약                       │
├─────────────────────────────────────────────────────────┤
│                                                          │
│  Client → gRPC → EtcdServer → Raft → WAL → Apply        │
│                                                          │
│  쓰기: Propose → 합의 → WAL 기록 → MVCC 적용 → 응답      │
│  읽기: ReadIndex → 리더 확인 → MVCC 조회 → 응답           │
│  감시: Watch 등록 → MVCC 이벤트 → 스트림 전송              │
│  만료: Lease 생성 → TTL 추적 → 만료 시 키 삭제             │
│                                                          │
│  저장: BoltDB (키-리비전 매핑)                              │
│  인덱스: B-tree (키 → 리비전 목록)                          │
│  내구성: WAL + 스냅샷 (장애 복구)                           │
│  복제: Raft 로그 (클러스터 전체 동기화)                      │
│                                                          │
└─────────────────────────────────────────────────────────┘
```
