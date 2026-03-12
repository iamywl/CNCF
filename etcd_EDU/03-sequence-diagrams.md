# etcd 시퀀스 다이어그램

## 1. 개요

etcd의 주요 요청 흐름을 시퀀스 다이어그램으로 설명한다. 모든 쓰기는 Raft 합의를 거치며, 읽기는 선형(Linearizable) 또는 직렬(Serializable) 모드를 선택할 수 있다.

## 2. Put 요청 흐름

클라이언트가 키-값을 저장하는 전체 흐름이다.

```mermaid
sequenceDiagram
    participant C as Client
    participant G as gRPC kvServer
    participant E as EtcdServer
    participant R as raftNode
    participant W as WAL
    participant A as UberApplier
    participant M as MVCC store
    participant B as Backend(BoltDB)
    participant WS as WatchableStore

    C->>G: Put(key="/foo", value="bar")
    G->>G: checkPutRequest() 유효성 검증
    G->>E: s.kv.Put(ctx, r)

    Note over E: 쓰기는 반드시 Raft 합의 필요
    E->>E: raftRequest(InternalRaftRequest{Put: r})
    E->>E: processInternalRaftRequestOnce()
    E->>E: w.Register(id) - 대기 등록
    E->>R: r.Propose(ctx, data)

    Note over R: Raft 합의 시작
    R->>R: 리더인지 확인 (팔로워면 리더로 전달)
    R-->>R: MsgProp → Raft 로그에 추가
    R->>R: 팔로워들에게 AppendEntries 전송

    Note over R: 과반수 ACK 수신
    R->>R: 커밋 인덱스 증가
    R->>W: WAL.Save(entries, hardstate)
    W->>W: 엔트리 인코딩 + CRC 기록
    W->>W: fsync (디스크 영속화)

    R->>R: Ready() → toApply 생성
    R->>E: applyc ← toApply{entries}

    E->>A: applyAll(entries)
    A->>M: store.Put("/foo", "bar")
    M->>M: currentRev++ (리비전 증가)
    M->>M: treeIndex.Put(key, rev)
    M->>B: BatchTx.UnsafePut(rev_bytes, kv_bytes)

    M->>WS: notify() - Watch 이벤트 발생
    WS->>WS: synced 워처들에게 Event{PUT} 전송

    A-->>E: PutResponse 반환
    E->>E: w.Trigger(id, result)
    E-->>G: PutResponse
    G-->>C: PutResponse{header{revision}}
```

## 3. Range (Linearizable) 요청 흐름

선형 읽기는 최신 커밋된 데이터를 보장한다.

```mermaid
sequenceDiagram
    participant C as Client
    participant G as gRPC kvServer
    participant E as EtcdServer
    participant R as raftNode
    participant M as MVCC store
    participant I as treeIndex
    participant B as Backend(BoltDB)

    C->>G: Range(key="/foo")
    G->>G: checkRangeRequest() 유효성 검증
    G->>E: s.kv.Range(ctx, r)

    Note over E: Serializable=false → 선형 읽기
    E->>E: linearizableReadNotify()

    Note over E: ReadIndex 프로토콜
    E->>E: readwaitc에 신호 전송

    Note over E: linearizableReadLoop() 고루틴
    E->>R: r.ReadIndex(ctx, ctxToSend)
    R->>R: 리더에게 ReadIndex 요청
    R->>R: 리더가 quorum heartbeat 확인
    R-->>E: ReadState{Index: committedIndex}

    E->>E: appliedIndex >= committedIndex 대기
    E->>E: readNotifier.notify() - 읽기 허용

    Note over E: MVCC에서 데이터 조회
    E->>M: store.Range("/foo", opts)
    M->>I: treeIndex.Range("/foo", nil, atRev)
    I->>I: B-tree에서 keyIndex 조회
    I-->>M: rev{N,0} 반환
    M->>B: UnsafeRange("key", rev_bytes)
    B-->>M: KV 바이트 반환
    M->>M: protobuf Unmarshal
    M-->>E: RangeResult{KVs, Count, Rev}

    E-->>G: RangeResponse
    G-->>C: RangeResponse{kvs, count, header{revision}}
```

## 4. Range (Serializable) 요청 흐름

직렬 읽기는 Raft 확인 없이 로컬에서 바로 읽는다.

```mermaid
sequenceDiagram
    participant C as Client
    participant G as gRPC kvServer
    participant E as EtcdServer
    participant M as MVCC store

    C->>G: Range(key="/foo", serializable=true)
    G->>G: checkRangeRequest()
    G->>E: s.kv.Range(ctx, r)

    Note over E: Serializable=true → Raft 확인 생략
    E->>M: store.Range("/foo", opts)
    M-->>E: RangeResult

    E-->>G: RangeResponse
    G-->>C: RangeResponse (약간 오래된 데이터 가능)
```

## 5. Watch 요청 흐름

Watch는 양방향 gRPC 스트림으로 키 변경 이벤트를 실시간 전달한다.

```mermaid
sequenceDiagram
    participant C as Client
    participant W as watchServer
    participant SS as serverWatchStream
    participant WS as mvcc.WatchStream
    participant WK as watchableStore
    participant S as MVCC store

    C->>W: Watch(stream) - 양방향 스트림 열기
    W->>SS: serverWatchStream 생성
    W->>SS: recvLoop() 고루틴 시작
    W->>SS: sendLoop() 고루틴 시작

    Note over C,SS: recvLoop - 클라이언트 요청 수신
    C->>SS: WatchCreateRequest{key="/app/", range_end="/app0", start_revision=100}
    SS->>WS: watchStream.Watch(key, end, startRev=100)

    alt startRev <= currentRev
        WS->>WK: unsynced 워처 그룹에 추가
        Note over WK: syncWatchersLoop (100ms 주기)
        WK->>S: rangeEvents(minRev=100, currentRev)
        S-->>WK: 과거 이벤트 목록
        WK->>WK: 워처별 이벤트 그룹화
        WK->>WS: watcher.ch ← WatchResponse{events}
        WK->>WK: 동기화 완료 → synced 그룹으로 이동
    else startRev > currentRev
        WS->>WK: synced 워처 그룹에 바로 추가
    end

    SS-->>C: WatchResponse{watch_id, created=true}

    Note over C,SS: 실시간 이벤트 전달
    Note over S: 다른 클라이언트가 Put("/app/config", "v2")
    S->>WK: notify() - 이벤트 발생
    WK->>WK: synced 워처에서 "/app/config" 매칭
    WK->>WS: watcher.ch ← WatchResponse{Event{PUT, KV}}

    Note over SS: sendLoop - 이벤트 전송
    SS->>SS: watchStream.Chan()에서 이벤트 수신
    SS-->>C: WatchResponse{watch_id, events=[Event{PUT}]}

    Note over C,SS: Watch 취소
    C->>SS: WatchCancelRequest{watch_id}
    SS->>WS: watchStream.Cancel(watch_id)
    SS-->>C: WatchResponse{watch_id, canceled=true}
```

## 6. Lease 생명주기

Lease의 생성부터 만료까지 전체 흐름이다.

```mermaid
sequenceDiagram
    participant C as Client
    participant L as LeaseServer
    participant E as EtcdServer
    participant R as Raft
    participant LE as Lessor
    participant M as MVCC store

    Note over C,M: 1. Lease 생성
    C->>L: LeaseGrant(TTL=30)
    L->>E: LeaseGrant(r)
    E->>R: raftRequest(InternalRaftRequest{LeaseGrant})
    R->>R: Raft 합의
    R-->>E: Apply
    E->>LE: lessor.Grant(id, ttl=30)
    LE->>LE: leaseMap[id] = Lease{ttl:30, expiry:now+30s}
    LE->>LE: leaseExpiredNotifier에 등록
    E-->>C: LeaseGrantResponse{ID, TTL=30}

    Note over C,M: 2. 키에 Lease 연결
    C->>E: Put("/session/abc", "data", lease=id)
    E->>R: Raft 합의
    R-->>E: Apply
    E->>M: store.Put("/session/abc", "data")
    E->>LE: lessor.Attach(id, LeaseItem{"/session/abc"})
    E-->>C: PutResponse

    Note over C,M: 3. KeepAlive (TTL 갱신)
    loop 매 TTL/3 주기
        C->>L: LeaseKeepAlive(stream)
        C->>L: LeaseKeepAliveRequest{ID}
        L->>LE: lessor.Renew(id)
        LE->>LE: lease.expiry = now + ttl
        L-->>C: LeaseKeepAliveResponse{TTL=30}
    end

    Note over C,M: 4. Lease 만료 (KeepAlive 중단 시)
    Note over LE: expiredLeaseC ← 만료된 Lease 목록
    LE-->>E: ExpiredLeasesC() ← [Lease{id}]
    E->>E: revokeExpiredLeases()
    E->>R: raftRequest(InternalRaftRequest{LeaseRevoke: id})
    R->>R: Raft 합의
    R-->>E: Apply
    E->>LE: lessor.Revoke(id)
    LE->>LE: 연결된 키 목록 조회
    LE->>M: rd.DeleteRange("/session/abc", nil)
    M->>M: 키 삭제 + Watch DELETE 이벤트
    LE->>LE: leaseMap에서 제거
```

## 7. 트랜잭션 (Txn) 흐름

```mermaid
sequenceDiagram
    participant C as Client
    participant G as gRPC kvServer
    participant E as EtcdServer
    participant R as Raft
    participant A as UberApplier
    participant M as MVCC store

    C->>G: Txn(compare=[Version("/lock")==0], success=[Put("/lock","me")], failure=[Get("/lock")])
    G->>G: checkTxnRequest() 유효성 검증

    alt 쓰기 포함 트랜잭션
        G->>E: s.kv.Txn(ctx, r)
        E->>R: raftRequest(InternalRaftRequest{Txn: r})
        R->>R: Raft 합의
        R-->>E: Apply

        E->>A: applyTxn()
        A->>M: txnRead 시작

        Note over A,M: Compare 평가
        A->>M: Range("/lock") → version 확인

        alt 모든 조건 참
            A->>M: txnWrite 시작
            A->>M: Put("/lock", "me")
            M->>M: currentRev++, 인덱스 업데이트
            A-->>E: TxnResponse{succeeded=true}
        else 조건 거짓
            A->>M: Range("/lock")
            A-->>E: TxnResponse{succeeded=false, kvs=[...]}
        end
    else 읽기 전용 트랜잭션
        Note over E: Raft 없이 로컬 처리
        E->>M: 직접 Compare + 실행
        E-->>G: TxnResponse
    end

    G-->>C: TxnResponse{header, succeeded, responses}
```

## 8. 클러스터 멤버 추가 흐름

```mermaid
sequenceDiagram
    participant C as Client (etcdctl)
    participant E1 as EtcdServer (리더)
    participant R as Raft
    participant E2 as EtcdServer (팔로워1)
    participant E3 as EtcdServer (팔로워2)
    participant N as New Node

    C->>E1: MemberAdd(peerURLs=["http://new:2380"])

    Note over E1: 사전 검증
    E1->>E1: 중복 URL 확인
    E1->>E1: 클러스터 크기 확인
    E1->>E1: Learner 제한 확인

    E1->>E1: configure(ConfChange{AddLearnerNode, nodeID})
    E1->>R: ProposeConfChange(cc)

    R->>R: Raft 합의
    R->>E2: AppendEntries(ConfChange)
    R->>E3: AppendEntries(ConfChange)
    E2-->>R: ACK
    E3-->>R: ACK

    R-->>E1: Apply ConfChange
    E1->>E1: cluster.AddMember(member)
    E1->>E1: Transport.AddPeer(nodeID, peerURLs)

    E1-->>C: MemberAddResponse{member, members}

    Note over N: 새 노드 시작
    N->>N: etcd --initial-cluster="existing+new"
    N->>E1: Raft 스냅샷 요청
    E1->>N: 스냅샷 전송
    N->>N: 스냅샷에서 상태 복원
    N->>N: Learner로 로그 복제 시작

    Note over C,N: Learner → Voter 승격
    C->>E1: MemberPromote(nodeID)
    E1->>R: ProposeConfChange(AddNode)
    R->>R: 합의 + 적용
    Note over N: 이제 투표 참여 가능
```

## 9. 스냅샷 생성 및 복구 흐름

```mermaid
sequenceDiagram
    participant R as raftNode
    participant E as EtcdServer
    participant S as Snapshotter
    participant W as WAL
    participant B as Backend(BoltDB)

    Note over R,B: 스냅샷 트리거 (SnapshotCount 도달)
    R->>E: 커밋 인덱스 - 마지막 스냅샷 > SnapshotCount(10000)

    E->>E: snapshot() 호출
    E->>B: backend.ForceCommit() - 모든 배치 커밋
    E->>B: backend.Snapshot() - BoltDB 스냅샷
    E->>R: r.raftStorage.CreateSnapshot(appliedIndex, confState, data)
    E->>S: snapshotter.SaveSnap(snapshot)
    S->>S: 파일명: {term:016x}-{index:016x}.snap
    S->>S: CRC32 계산 + protobuf 직렬화
    S->>S: WriteAndSyncFile (fsync)

    E->>W: WAL.SaveSnapshot(walSnap{Index, Term})
    E->>W: WAL.ReleaseLockTo(snapIndex)
    Note over E: 오래된 WAL/스냅샷 파일 정리

    Note over R,B: === 서버 재시작 시 복구 ===

    E->>S: snapshotter.Load() - 최신 스냅샷 로드
    S-->>E: raftpb.Snapshot

    E->>W: WAL.OpenForRead(walSnap)
    W->>W: 스냅샷 이후 엔트리 재생
    W-->>E: metadata, hardState, entries

    E->>R: raftStorage.ApplySnapshot(snap)
    E->>R: raftStorage.Append(entries)
    E->>R: raftStorage.SetHardState(st)

    E->>B: backend 열기
    E->>E: MVCC store.restore()
    Note over E: BoltDB에서 모든 KV 읽어서 treeIndex 재구성
    E->>E: Lease 복원
    E->>E: Auth 복원
    E->>E: 클러스터 멤버십 복원
```

## 10. 컴팩션 흐름

```mermaid
sequenceDiagram
    participant C as Client
    participant E as EtcdServer
    participant R as Raft
    participant M as MVCC store
    participant I as treeIndex
    participant B as Backend(BoltDB)
    participant SC as Scheduler

    C->>E: Compact(revision=1000)
    E->>R: raftRequest(InternalRaftRequest{Compaction: rev=1000})
    R->>R: Raft 합의
    R-->>E: Apply

    E->>M: store.Compact(trace, rev=1000)
    M->>M: compactMainRev = 1000

    Note over M,SC: 비동기 컴팩션 (FIFO 스케줄러)
    M->>SC: schedule(compaction job)

    SC->>I: treeIndex.Compact(rev=1000)
    I->>I: 모든 keyIndex 순회
    I->>I: rev < 1000인 오래된 리비전 제거
    I->>I: 빈 generation 삭제
    I-->>SC: 삭제할 리비전 맵 반환

    SC->>B: BatchTx에서 삭제할 리비전 키 제거
    Note over SC,B: 배치 처리 (1000개씩, 10ms 대기)
    loop 배치 처리
        SC->>B: UnsafeDelete("key", rev_bytes) × 1000
        SC->>B: 10ms sleep
    end

    B->>B: 커밋
    E-->>C: CompactionResponse{header{revision}}
```

## 11. 선형 읽기 (linearizableReadLoop) 상세

```mermaid
sequenceDiagram
    participant C1 as Client 1
    participant C2 as Client 2
    participant E as EtcdServer
    participant RL as readLoop 고루틴
    participant R as raftNode
    participant Leader as Raft Leader

    Note over RL: linearizableReadLoop() 상시 실행

    C1->>E: Range("/foo", serializable=false)
    E->>E: linearizableReadNotify()
    E->>E: readwaitc에 신호 전송

    C2->>E: Range("/bar", serializable=false)
    E->>E: linearizableReadNotify()
    Note over E: 이미 신호 보냄 (배칭)

    RL->>RL: readwaitc 수신 → 배치 시작
    RL->>R: r.ReadIndex(ctx, requestID)

    R->>Leader: MsgReadIndex
    Leader->>Leader: quorum heartbeat 확인
    Leader-->>R: ReadState{Index: committedIndex}

    R-->>RL: readStateC ← ReadState

    RL->>RL: appliedIndex >= ReadState.Index 대기
    RL->>RL: readNotifier.notify(nil) - 모든 대기자에게 알림

    Note over C1,C2: C1, C2 모두 읽기 허용됨
    C1->>E: MVCC Range("/foo") 실행
    C2->>E: MVCC Range("/bar") 실행
```

## 12. 흐름 요약

| 요청 | Raft 합의 | 핵심 경로 |
|------|----------|----------|
| Put | 필수 | gRPC → Propose → WAL → Apply → MVCC Put → Watch notify |
| DeleteRange | 필수 | gRPC → Propose → WAL → Apply → MVCC Delete → Watch notify |
| Range (Linear) | ReadIndex | gRPC → ReadIndex → quorum 확인 → MVCC Range |
| Range (Serial) | 불필요 | gRPC → MVCC Range (로컬) |
| Txn (쓰기) | 필수 | gRPC → Propose → Apply → Compare → Success/Failure |
| Watch | 불필요 | gRPC Stream → MVCC Watch 등록 → 이벤트 스트림 |
| LeaseGrant | 필수 | gRPC → Propose → Lessor.Grant |
| LeaseKeepAlive | 불필요 | gRPC Stream → Lessor.Renew (리더만) |
| Compact | 필수 | gRPC → Propose → MVCC Compact (비동기) |
| MemberAdd | 필수 | gRPC → ConfChange Propose → 합의 → 멤버십 변경 |
