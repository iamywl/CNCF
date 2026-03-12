# 07. Raft 합의 엔진 Deep-Dive

## 개요

etcd의 핵심은 Raft 합의 알고리즘이다. 모든 쓰기 요청은 Raft 로그를 통해 클러스터 전체에 복제되며, 과반수(quorum)의 승인을 받아야 커밋된다. 이 문서에서는 etcd가 Raft 라이브러리(`go.etcd.io/raft/v3`)를 어떻게 래핑하고, Ready 채널 기반의 이벤트 루프로 로그 복제/선거/스냅샷/선형 읽기를 처리하는지 소스코드 수준에서 분석한다.

소스코드 경로:
- `server/etcdserver/raft.go` - raftNode, raftNodeConfig, toApply 구조체 및 start() 루프
- `server/etcdserver/v3_server.go` - linearizableReadLoop(), requestCurrentIndex()
- `server/etcdserver/bootstrap.go` - raftConfig(), PreVote/ElectionTick 설정
- `server/etcdserver/server.go` - raftReadyHandler, readwaitc, readNotifier
- `pkg/contention/contention.go` - TimeoutDetector

---

## 1. 핵심 구조체

### 1.1 toApply 구조체

`toApply`는 Raft Ready에서 추출한 커밋된 엔트리와 스냅샷을 etcd 상태 머신(apply 계층)에 전달하는 운반 객체이다.

```
// server/etcdserver/raft.go:69-78
type toApply struct {
    entries  []raftpb.Entry
    snapshot raftpb.Snapshot
    notifyc chan struct{}
    raftAdvancedC <-chan struct{}
}
```

| 필드 | 역할 |
|------|------|
| `entries` | `rd.CommittedEntries` - Raft가 커밋 확정한 로그 엔트리 목록 |
| `snapshot` | `rd.Snapshot` - 비어있지 않으면 스냅샷 복원이 필요 |
| `notifyc` | WAL 디스크 쓰기 완료를 apply 계층에 알리는 동기화 채널 |
| `raftAdvancedC` | ConfChange 엔트리 포함 시 `r.Advance()` 완료를 알리는 채널 |

`notifyc`의 핵심 설계 의도: apply 계층은 WAL 쓰기가 완료되기 전에 엔트리를 병렬로 처리할 수 있지만, 스냅샷을 트리거하기 전에는 반드시 WAL 영속화가 보장되어야 한다. `notifyc`는 이 동기화 지점을 제공한다.

### 1.2 raftNode 구조체

`raftNode`는 Raft 라이브러리의 `raft.Node` 인터페이스를 래핑하고, etcd 서버와 Raft 엔진 사이의 브릿지 역할을 한다.

```
// server/etcdserver/raft.go:80-104
type raftNode struct {
    lg *zap.Logger

    tickMu *sync.RWMutex
    latestTickTs time.Time
    raftNodeConfig

    msgSnapC chan raftpb.Message   // 스냅샷 메시지 전달 채널
    applyc   chan toApply          // apply 계층으로의 출력 채널
    readStateC chan raft.ReadState // ReadIndex 결과 전달 채널

    ticker *time.Ticker
    td *contention.TimeoutDetector  // 하트비트 지연 감지

    stopped chan struct{}
    done    chan struct{}
}
```

**왜 이 구조인가?**

etcd는 Raft 라이브러리를 "라이브러리로서" 사용한다. Raft 라이브러리는 네트워크 I/O나 디스크 I/O를 직접 수행하지 않는다. 대신 `Ready()` 채널을 통해 "해야 할 일"을 알려주고, 애플리케이션이 실제 I/O를 수행한다. `raftNode`가 바로 이 I/O를 담당하는 어댑터이다.

### 1.3 raftNodeConfig 구조체

```
// server/etcdserver/raft.go:106-120
type raftNodeConfig struct {
    lg *zap.Logger
    isIDRemoved func(id uint64) bool
    raft.Node                          // Raft 노드 인터페이스 임베딩
    raftStorage *raft.MemoryStorage    // Raft 인메모리 로그 저장소
    storage     serverstorage.Storage  // WAL + Snap 디스크 저장소
    heartbeat   time.Duration
    transport   rafthttp.Transporter   // 네트워크 전송 계층
}
```

핵심 포인트:
- `raft.Node` 인터페이스를 임베딩하여 `Propose()`, `Ready()`, `Advance()`, `ReadIndex()` 등을 직접 호출 가능
- `raftStorage`는 Raft 라이브러리 내부의 인메모리 로그 (MemoryStorage)
- `storage`는 etcd의 WAL + 스냅샷 디스크 저장소
- `transport`는 gRPC 기반의 Raft 메시지 전송 계층 (rafthttp)

---

## 2. raftNode.start() - Ready 처리 루프

`start()` 메서드는 etcd Raft 엔진의 핵심 이벤트 루프이다. 별도의 고루틴에서 실행되며, Raft 라이브러리가 생성하는 `Ready` 이벤트를 순차적으로 처리한다.

### 2.1 전체 루프 구조

```
// server/etcdserver/raft.go:173-339
func (r *raftNode) start(rh *raftReadyHandler) {
    internalTimeout := time.Second

    go func() {
        defer r.onStop()
        islead := false

        for {
            select {
            case <-r.ticker.C:
                r.tick()
            case rd := <-r.Ready():
                // Ready 처리 (아래 상세)
            case <-r.stopped:
                return
            }
        }
    }()
}
```

세 가지 이벤트를 처리한다:

| 이벤트 | 처리 |
|--------|------|
| `r.ticker.C` | Raft tick 발생 - 선거 타이머/하트비트 타이머 진행 |
| `r.Ready()` | Raft 엔진이 처리할 작업을 생성 |
| `r.stopped` | 서버 종료 신호 |

### 2.2 Ready 처리 상세 흐름

Ready 이벤트 수신 시 처리 순서:

```
Ready 수신
    │
    ├─ 1. SoftState 처리 (리더 변경 감지)
    │      - 새 리더 감지 시 leaderChanges 메트릭 증가
    │      - hasLeader, isLeader 메트릭 업데이트
    │      - rh.updateLeadership() 콜백 호출
    │      - TimeoutDetector 리셋
    │
    ├─ 2. ReadStates 전달
    │      - readStateC 채널로 ReadState 전달
    │      - 타임아웃(1초) 내 전달 실패 시 로그 경고
    │
    ├─ 3. toApply 생성 및 전달
    │      - CommittedEntries + Snapshot → toApply 구조체
    │      - updateCommittedIndex() 호출
    │      - applyc 채널로 전달 (EtcdServer가 수신)
    │
    ├─ 4. 리더인 경우: 메시지 먼저 전송
    │      - processMessages() 후 transport.Send()
    │      - Raft 논문 10.2.1: 리더는 디스크 쓰기와 복제를 병렬화
    │
    ├─ 5. 스냅샷 저장 (비어있지 않은 경우)
    │      - r.storage.SaveSnap() → WAL 스냅샷 엔트리 기록
    │
    ├─ 6. HardState + Entries WAL 저장
    │      - r.storage.Save() → WAL에 HardState와 새 엔트리 영속화
    │
    ├─ 7. 스냅샷 적용 (비어있지 않은 경우)
    │      - r.storage.Sync() → WAL fsync
    │      - notifyc 신호 → apply 계층에 스냅샷 영속화 완료 알림
    │      - r.raftStorage.ApplySnapshot() → 인메모리 스토리지 업데이트
    │      - r.storage.Release() → 오래된 WAL 데이터 해제
    │
    ├─ 8. 인메모리 로그 추가
    │      - r.raftStorage.Append(rd.Entries)
    │
    ├─ 9. 팔로워/후보인 경우: 메시지 후속 전송
    │      - processMessages() 후 notifyc 신호
    │      - ConfChange 포함 시 추가 동기화 대기
    │      - transport.Send()
    │
    └─ 10. r.Advance() 호출
           - Raft 라이브러리에 Ready 처리 완료 알림
           - ConfChange 포함 시 raftAdvancedC 신호
```

### 2.3 리더와 팔로워의 처리 차이

Raft 논문 10.2.1절의 핵심 최적화가 여기에 구현되어 있다:

```
┌─────────────────────────────────────────────────┐
│                    리더                          │
│  1. processMessages + Send  ← 먼저 복제 전송     │
│  2. SaveSnap / Save        ← 동시에 디스크 쓰기  │
│  3. notifyc                                      │
│  4. Advance                                      │
│                                                  │
│  → 네트워크 전송과 디스크 쓰기를 병렬화           │
└─────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────┐
│                   팔로워                         │
│  1. SaveSnap / Save        ← 먼저 디스크 쓰기   │
│  2. processMessages        ← 메시지 처리        │
│  3. notifyc                                      │
│  4. ConfChange 시 추가 대기                       │
│  5. Send                                         │
│  6. Advance                                      │
│                                                  │
│  → 디스크 쓰기 후 응답 전송 (안전성 우선)         │
└─────────────────────────────────────────────────┘
```

**왜 이렇게 다른가?**

리더는 팔로워들에게 로그를 복제하면서 동시에 자신의 디스크에 쓸 수 있다. 팔로워의 디스크 쓰기와 리더의 디스크 쓰기가 병렬로 진행되므로 지연 시간이 줄어든다. 반면 팔로워는 자신의 디스크에 먼저 기록한 후 응답(MsgAppResp)을 보내야 데이터 안전성이 보장된다.

---

## 3. processMessages() - 메시지 전처리

```
// server/etcdserver/raft.go:355-399
func (r *raftNode) processMessages(ms []raftpb.Message) []raftpb.Message {
    sentAppResp := false
    for i := len(ms) - 1; i >= 0; i-- {
        // 1. 제거된 멤버로의 메시지 무효화
        if r.isIDRemoved(ms[i].To) {
            ms[i].To = 0
            continue
        }
        // 2. MsgAppResp 중복 제거 (최신 것만 유지)
        if ms[i].Type == raftpb.MsgAppResp {
            if sentAppResp {
                ms[i].To = 0
            } else {
                sentAppResp = true
            }
        }
        // 3. MsgSnap → msgSnapC 채널로 리다이렉트
        if ms[i].Type == raftpb.MsgSnap {
            select {
            case r.msgSnapC <- ms[i]:
            default:
            }
            ms[i].To = 0
        }
        // 4. MsgHeartbeat 타이밍 관찰
        if ms[i].Type == raftpb.MsgHeartbeat {
            ok, exceed := r.td.Observe(ms[i].To)
            if !ok {
                // 하트비트 지연 경고
            }
        }
    }
    return ms
}
```

**역순 순회의 이유**: `MsgAppResp`를 역순으로 검사하여 가장 최신(마지막) 응답만 전송한다. 이전 응답들은 최신 응답에 이미 포함되어 있으므로 네트워크 대역폭을 절약한다.

**MsgSnap 리다이렉트의 이유**: 스냅샷 메시지는 Raft 로그뿐 아니라 etcd의 KV 스토어 스냅샷도 포함해야 한다. `msgSnapC` 채널을 통해 etcd 서버 메인 루프로 보내 KV 스냅샷을 병합한 후 전송한다.

---

## 4. Raft 선거 과정

### 4.1 타이밍 설정

```
// server/etcdserver/bootstrap.go:539-551
func raftConfig(cfg config.ServerConfig, id uint64, s *raft.MemoryStorage) *raft.Config {
    return &raft.Config{
        ID:              id,
        ElectionTick:    cfg.ElectionTicks,
        HeartbeatTick:   1,
        CheckQuorum:     true,
        PreVote:         cfg.PreVote,
        ...
    }
}
```

| 설정 | 값 | 의미 |
|------|-----|------|
| `TickMs` | 설정 파일 (기본 100ms) | 하나의 Raft tick 주기 |
| `HeartbeatTick` | 1 | 1 tick마다 하트비트 전송 = TickMs마다 |
| `ElectionTick` | `cfg.ElectionTicks` (기본 10) | 선거 타임아웃 = ElectionTick * TickMs |
| `CheckQuorum` | true | 리더가 과반수 팔로워와 통신 못하면 스텝다운 |
| `PreVote` | 설정에 따라 | 네트워크 파티션에서 불필요한 term 증가 방지 |

heartbeat 필드 설정 (`server/etcdserver/bootstrap.go:522`):
```
heartbeat: time.Duration(cfg.TickMs) * time.Millisecond,
```

### 4.2 Tick 처리

```
// server/etcdserver/raft.go:158-163
func (r *raftNode) tick() {
    r.tickMu.Lock()
    r.Tick()                      // raft.Node.Tick() 호출
    r.latestTickTs = time.Now()   // 최신 tick 타임스탬프 기록
    r.tickMu.Unlock()
}
```

`r.Tick()`이 호출되면 Raft 라이브러리 내부에서:
- 리더: heartbeat 카운터 증가 → HeartbeatTick 도달 시 모든 팔로워에 하트비트 전송
- 팔로워/후보: election 카운터 증가 → ElectionTick 도달 시 선거 시작

### 4.3 PreVote 메커니즘

PreVote는 etcd가 활성화하는 Raft 확장이다. 네트워크 파티션에서 격리된 노드가 term을 무한히 올려 복귀 시 클러스터를 교란하는 문제를 방지한다.

```
일반 선거:
  노드 A (격리) → term 증가 → 복귀 → 높은 term으로 기존 리더 무효화

PreVote 선거:
  노드 A (격리) → PreVote 요청 (term 증가 없음) → 과반수 거부 → 선거 안 함
  노드 A (정상) → PreVote 요청 → 과반수 승인 → 실제 선거 시작 (term 증가)
```

PreVote 흐름:
```
팔로워 (선거 타임아웃)
    │
    ├─ 1. MsgPreVote 전송 (term+1, 현재 로그 정보)
    │      ※ 실제 term은 증가시키지 않음
    │
    ├─ 2. 각 노드가 MsgPreVoteResp 응답
    │      - 조건: 발신자의 로그가 최소 자신만큼 최신이고,
    │              현재 리더로부터 최근 하트비트를 받지 않은 경우
    │
    ├─ 3. 과반수 PreVote 획득
    │      │
    │      └─ MsgVote 전송 (term 실제 증가)
    │          → 일반 선거 프로토콜 진행
    │
    └─ 3'. 과반수 미달
           └─ 선거 포기 → term 보존
```

### 4.4 일반 선거 흐름 (Vote)

```
[팔로워] election timeout 발생
    │
    ├── Raft 상태를 Candidate로 전환
    ├── currentTerm++
    ├── 자신에게 투표
    ├── 모든 피어에게 MsgVote(term, lastLogIndex, lastLogTerm) 전송
    │
    │   [각 피어]
    │   ├── term이 자신보다 높으면 팔로워로 전환
    │   ├── 아직 이 term에 투표하지 않았고, 후보의 로그가 최신이면
    │   │   └── MsgVoteResp(granted=true) 응답
    │   └── 그 외
    │       └── MsgVoteResp(granted=false) 응답
    │
    ├── 과반수 granted 수신
    │   └── 리더로 전환 → 모든 피어에 하트비트 전송
    │
    ├── 다른 리더의 하트비트 수신 (같은/높은 term)
    │   └── 팔로워로 전환
    │
    └── 선거 타임아웃 (아무도 과반수 못 얻음)
        └── 새로운 선거 시작
```

---

## 5. 로그 복제 흐름

### 5.1 Propose → Ready → WAL → Apply 전체 흐름

```
클라이언트 Put("key", "value")
    │
    ▼
EtcdServer.processInternalRaftRequestOnce()
    │
    ├── 요청을 protobuf로 직렬화
    └── s.r.Propose(ctx, data)     ← raft.Node.Propose()
        │
        ▼
    Raft 라이브러리 내부
    ├── 리더: 로컬 로그에 추가, 모든 팔로워에 MsgApp 생성
    └── 팔로워: 리더에 전달
        │
        ▼
    Ready 채널에 결과 전달
    ├── rd.Entries: 새로 추가된 엔트리 (영속화 필요)
    ├── rd.Messages: 전송할 Raft 메시지
    ├── rd.HardState: 업데이트된 term/vote/commit
    └── rd.CommittedEntries: 커밋 확정된 엔트리 (적용 필요)
        │
        ▼
    raftNode.start() Ready 처리
    ├── 1. toApply 생성 → applyc 채널 전달
    ├── 2. 리더: transport.Send(Messages)
    ├── 3. storage.Save(HardState, Entries)  → WAL 디스크 쓰기
    ├── 4. raftStorage.Append(Entries)       → 인메모리 로그 업데이트
    └── 5. r.Advance()                       → Ready 소비 완료
        │
        ▼
    EtcdServer.applyAll()   (별도 고루틴)
    ├── <-applyc 수신
    ├── <-notifyc 대기 (WAL 쓰기 완료 확인)
    ├── 각 CommittedEntry에 대해:
    │   ├── EntryNormal: KV 스토어에 적용
    │   └── EntryConfChange: 멤버십 변경 적용
    └── applyWait.Trigger(index) → 대기 중인 읽기에 알림
```

### 5.2 과반수 커밋 메커니즘

```
3노드 클러스터: 노드1(리더), 노드2, 노드3

[로그 인덱스 10 복제]

노드1 (리더):
  ├── 로컬 로그에 추가 (match[1] = 10)
  ├── 노드2에 MsgApp 전송
  └── 노드3에 MsgApp 전송

노드2: MsgApp 수신 → 로그에 추가 → MsgAppResp(index=10) 응답
노드3: (네트워크 지연)

리더가 MsgAppResp(노드2, index=10) 수신:
  ├── match[2] = 10
  ├── 정렬: match = [10, 10, ?]
  ├── 중앙값(quorum): match[3/2] = match[1] = 10
  └── commitIndex = 10  (과반수 2/3 확인)
      → CommittedEntries에 포함 → 다음 Ready에서 적용
```

---

## 6. ReadIndex 프로토콜 (선형 읽기)

### 6.1 왜 ReadIndex가 필요한가

etcd는 선형 일관성(linearizable consistency)을 보장한다. 즉, 읽기 요청은 가장 최신 커밋된 상태를 반영해야 한다. 단순히 로컬 상태를 읽으면:
- 리더가 아닌 노드에서 읽을 경우 오래된 데이터를 반환할 수 있다
- 네트워크 파티션으로 격리된 구 리더에서 읽을 경우 오래된 데이터를 반환할 수 있다

ReadIndex는 "현재 리더가 진짜 리더인지" 확인하고, "커밋 인덱스까지 적용된 후" 읽기를 수행하는 프로토콜이다.

### 6.2 linearizableReadNotify()

```
// server/etcdserver/v3_server.go:1140-1160
func (s *EtcdServer) linearizableReadNotify(ctx context.Context) error {
    s.readMu.RLock()
    nc := s.readNotifier
    s.readMu.RUnlock()

    // linearizableReadLoop에 읽기 요청 신호
    select {
    case s.readwaitc <- struct{}{}:
    default:
    }

    // ReadState 결과 대기
    select {
    case <-nc.c:
        return nc.err
    case <-ctx.Done():
        return ctx.Err()
    case <-s.done:
        return errors.ErrStopped
    }
}
```

`readwaitc`는 버퍼 크기 1의 채널이다. 여러 읽기 요청이 동시에 오면 하나의 ReadIndex 요청으로 모두 해결할 수 있다 (배칭 효과).

### 6.3 linearizableReadLoop()

이 함수는 etcd 서버 시작 시 별도 고루틴으로 실행되며, ReadIndex 요청을 처리하는 전용 루프이다.

```
// server/etcdserver/v3_server.go:972-1022
func (s *EtcdServer) linearizableReadLoop() {
    for {
        leaderChangedNotifier := s.leaderChanged.Receive()
        select {
        case <-leaderChangedNotifier:
            continue                    // 리더 변경 시 재시작
        case <-s.readwaitc:             // 읽기 요청 수신
        case <-s.stopping:
            return
        }

        nextnr := newNotifier()
        s.readMu.Lock()
        nr := s.readNotifier             // 현재 대기자들의 notifier
        s.readNotifier = nextnr           // 새 notifier로 교체
        s.readMu.Unlock()

        confirmedIndex, err := s.requestCurrentIndex(leaderChangedNotifier)
        if err != nil {
            nr.notify(err)
            continue
        }

        appliedIndex := s.getAppliedIndex()
        if appliedIndex < confirmedIndex {
            select {
            case <-s.applyWait.Wait(confirmedIndex):  // 적용 완료 대기
            case <-s.stopping:
                return
            }
        }
        nr.notify(nil)  // 모든 대기자에게 읽기 가능 알림
    }
}
```

### 6.4 requestCurrentIndex() 상세

```
// server/etcdserver/v3_server.go:1028-1110
func (s *EtcdServer) requestCurrentIndex(leaderChangedNotifier <-chan struct{}) (uint64, error) {
    requestIDs := map[uint64]struct{}{}
    requestID := s.reqIDGen.Next()
    requestIDs[requestID] = struct{}{}
    err := s.sendReadIndex(requestID)
    // ...

    for {
        select {
        case rs := <-s.r.readStateC:
            // ReadState 수신 → confirmedIndex 반환
            responseID := binary.BigEndian.Uint64(rs.RequestCtx)
            if _, ok := requestIDs[responseID]; !ok {
                continue  // 이전 요청의 응답은 무시
            }
            return rs.Index, nil

        case <-leaderChangedNotifier:
            return 0, errors.ErrLeaderChanged

        case <-firstCommitInTermNotifier:
            // 새 term의 첫 커밋 후 재시도
            requestID = s.reqIDGen.Next()
            requestIDs[requestID] = struct{}{}
            s.sendReadIndex(requestID)

        case <-retryTimer.C:
            // 500ms 재시도 타이머
            requestID = s.reqIDGen.Next()
            requestIDs[requestID] = struct{}{}
            s.sendReadIndex(requestID)

        case <-errorTimer.C:
            return 0, errors.ErrTimeout
        }
    }
}
```

### 6.5 ReadIndex 전체 시퀀스

```
클라이언트 Range(key, serializable=false)
    │
    ▼
linearizableReadNotify(ctx)
    ├── readwaitc <- struct{}{}   (루프 깨우기)
    └── <-nc.c 대기               (결과 대기)
        │
        ▼
linearizableReadLoop()
    ├── readwaitc 수신
    ├── nr = 현재 readNotifier
    ├── readNotifier = 새 notifier
    └── requestCurrentIndex()
        │
        ├── sendReadIndex(requestID)
        │   └── r.ReadIndex(ctx, requestID)  ← raft.Node.ReadIndex()
        │       │
        │       ▼ Raft 라이브러리 내부:
        │       리더: 현재 commitIndex 기록
        │       리더: 과반수에 하트비트 전송
        │       과반수 응답 수신 → ReadState 생성
        │       │
        │       ▼ Ready 루프에서:
        │       rd.ReadStates → readStateC 채널
        │
        ├── <-readStateC 수신
        │   confirmedIndex = rs.Index
        │
        ├── appliedIndex < confirmedIndex 이면:
        │   └── applyWait.Wait(confirmedIndex) 대기
        │
        └── nr.notify(nil)  → 모든 대기자 해제
            │
            ▼
        클라이언트의 <-nc.c 해제
        → 로컬 KV 스토어에서 읽기 수행
```

### 6.6 ReadState 전달 경로

`raftNode.start()`의 Ready 처리에서 ReadStates를 `readStateC`로 전달한다:

```go
// server/etcdserver/raft.go:208-216
if len(rd.ReadStates) != 0 {
    select {
    case r.readStateC <- rd.ReadStates[len(rd.ReadStates)-1]:
    case <-time.After(internalTimeout):
        r.lg.Warn("timed out sending read state", ...)
    case <-r.stopped:
        return
    }
}
```

마지막 ReadState만 전달하는 이유: 여러 ReadIndex 요청이 같은 Ready에 포함될 수 있지만, 가장 최신의 confirmedIndex가 이전 것들을 모두 커버한다.

---

## 7. 스냅샷 전송

### 7.1 msgSnapC 채널

스냅샷은 두 가지 데이터를 합쳐야 한다:
1. Raft 상태 (term, index, confState) - Raft 라이브러리가 생성
2. KV 스토어 데이터 - etcd 서버가 생성

```
processMessages()에서:
    MsgSnap 감지 → msgSnapC 채널로 전달
    원본 메시지의 To = 0으로 설정 (transport.Send에서 무시됨)

EtcdServer 메인 루프에서:
    <-msgSnapC 수신
    → KV 스토어 스냅샷 병합
    → 완성된 스냅샷을 transport.SendSnapshot()으로 전송
```

### 7.2 스냅샷 수신 및 적용

Ready 루프에서 스냅샷을 수신한 경우:

```
1. r.storage.SaveSnap(rd.Snapshot)
   → WAL에 스냅샷 마커 기록

2. r.storage.Save(rd.HardState, rd.Entries)
   → HardState와 엔트리 WAL 기록

3. r.storage.Sync()
   → WAL fsync 보장

4. notifyc <- struct{}{}
   → apply 계층에 "스냅샷 디스크 기록 완료" 알림

5. r.raftStorage.ApplySnapshot(rd.Snapshot)
   → 인메모리 Raft 로그 잘라내기

6. r.storage.Release(rd.Snapshot)
   → 스냅샷 이전의 오래된 WAL 데이터 해제
```

Sync()와 Release() 사이에 notifyc를 보내는 이유 (`raft.go:263-282`): Release()가 오래된 WAL 데이터를 삭제하기 전에, apply 계층이 스냅샷 데이터가 디스크에 안전하게 기록되었음을 알아야 한다. 이슈 #10219에서 발견된 순서 문제를 방지한다.

---

## 8. Raft 타이밍 설정

### 8.1 기본 타이밍 계산

```
기본 설정:
  TickMs = 100ms
  ElectionTicks = 10
  HeartbeatTick = 1

계산:
  하트비트 주기    = TickMs * HeartbeatTick = 100ms
  선거 타임아웃    = TickMs * ElectionTicks = 1000ms (1초)

  실제 선거 타임아웃은 [ElectionTicks, 2*ElectionTicks) 범위에서
  랜덤하게 결정됨 = [1초, 2초)
```

### 8.2 raftNodeConfig의 heartbeat 필드

`server/etcdserver/bootstrap.go:522`에서 설정:
```go
heartbeat: time.Duration(cfg.TickMs) * time.Millisecond,
```

이 값은 두 가지 용도로 사용된다:
1. `raftNode`의 `ticker` 주기 설정 (`newRaftNode`에서 `time.NewTicker(r.heartbeat)`)
2. `TimeoutDetector`의 기준 시간 (`2 * cfg.heartbeat`)

### 8.3 타이밍과 성능 트레이드오프

```
                    짧은 TickMs              긴 TickMs
                    (예: 50ms)              (예: 500ms)
    ┌──────────────────────────────────────────────────────┐
    │ 장애 감지      빠름 (0.5-1초)          느림 (5-10초)  │
    │ CPU 사용       높음 (tick 빈번)        낮음           │
    │ 네트워크       높음 (heartbeat 빈번)   낮음           │
    │ 디스크 경합    낮음 (작은 배치)        높음 (큰 배치) │
    │ 적합 환경      같은 DC                 WAN/크로스 DC  │
    └──────────────────────────────────────────────────────┘
```

### 8.4 advanceTicks()

```go
// server/etcdserver/raft.go:442-446
func (r *raftNode) advanceTicks(ticks int) {
    for i := 0; i < ticks; i++ {
        r.tick()
    }
}
```

이 함수는 선거를 빠르게 트리거하기 위해 tick을 인위적으로 전진시킨다. 멀티 데이터센터 배포에서 선거 과정을 가속화하는 데 사용할 수 있다.

---

## 9. contention.TimeoutDetector

### 9.1 구조

```
// pkg/contention/contention.go:27-32
type TimeoutDetector struct {
    mu          sync.Mutex
    maxDuration time.Duration
    records     map[uint64]time.Time  // 이벤트 ID → 마지막 발생 시각
}
```

### 9.2 동작 원리

`TimeoutDetector`는 하트비트 메시지의 전송 간격을 모니터링한다.

```go
// pkg/contention/contention.go:54-70
func (td *TimeoutDetector) Observe(id uint64) (bool, time.Duration) {
    td.mu.Lock()
    defer td.mu.Unlock()

    ok := true
    now := time.Now()
    exceed := time.Duration(0)

    if pt, found := td.records[id]; found {
        exceed = now.Sub(pt) - td.maxDuration
        if exceed > 0 {
            ok = false  // 예상 시간 초과
        }
    }
    td.records[id] = now
    return ok, exceed
}
```

`raftNode`에서의 사용:

```go
// newRaftNode에서 초기화 (raft.go:142)
td: contention.NewTimeoutDetector(2 * cfg.heartbeat),

// processMessages에서 관찰 (raft.go:383-396)
if ms[i].Type == raftpb.MsgHeartbeat {
    ok, exceed := r.td.Observe(ms[i].To)
    if !ok {
        r.lg.Warn(
            "leader failed to send out heartbeat on time; ...",
            zap.Duration("heartbeat-interval", r.heartbeat),
            zap.Duration("expected-duration", 2*r.heartbeat),
            zap.Duration("exceeded-duration", exceed),
        )
        heartbeatSendFailures.Inc()
    }
}
```

`maxDuration`이 `2 * heartbeat`인 이유: 하트비트는 매 tick마다 전송되지만, 디스크 I/O나 CPU 경합으로 인해 정확한 간격을 보장할 수 없다. 2배의 여유를 두어 일시적인 지연은 허용하되, 심각한 지연만 감지한다.

### 9.3 Reset 시점

```go
// raftNode.start()에서 SoftState 처리 시 (raft.go:205)
r.td.Reset()
```

리더가 변경되면 `TimeoutDetector`의 기록을 초기화한다. 새 리더의 하트비트 타이밍은 이전 리더의 기록과 무관하기 때문이다.

---

## 10. raftReadyHandler - 상태 머신과 Raft의 디커플링

### 10.1 구조

```
// server/etcdserver/server.go:748-756
type raftReadyHandler struct {
    getLead              func() (lead uint64)
    updateLead           func(lead uint64)
    updateLeadership     func(newLeader bool)
    updateCommittedIndex func(uint64)
}
```

### 10.2 왜 콜백 패턴인가

`raftReadyHandler`는 `raftNode`가 `EtcdServer`의 상태를 직접 참조하지 않도록 인터페이스를 분리한다. 이를 통해:
- `raftNode`는 순수한 Raft I/O 처리에 집중
- 리더십 변경, 커밋 인덱스 업데이트 등의 상태 머신 로직은 `EtcdServer`가 콜백으로 제공
- 테스트에서 `raftReadyHandler`를 모킹하여 Raft 루프를 독립적으로 테스트 가능

### 10.3 updateLeadership 콜백의 역할

`server/etcdserver/server.go:769`에서 설정되는 이 콜백은:
- 리더 → 팔로워 전환 시: Lessor를 demote하고 lease 만료 중지
- 팔로워 → 리더 전환 시: Lessor를 promote하고 lease 관리 시작
- 컨센서스 모드 전환에 따른 알림 서버 리더십 갱신

---

## 11. 서버 종료 처리

### 11.1 stop() 과 onStop()

```go
// server/etcdserver/raft.go:405-425
func (r *raftNode) stop() {
    select {
    case r.stopped <- struct{}{}:  // 종료 요청
    case <-r.done:                 // 이미 종료됨
        return
    }
    <-r.done  // 종료 완료 대기
}

func (r *raftNode) onStop() {
    r.Stop()              // raft.Node.Stop()
    r.ticker.Stop()       // tick 타이머 중지
    r.transport.Stop()    // 네트워크 전송 중지
    r.storage.Close()     // WAL/Snap 저장소 닫기
    close(r.done)         // 종료 완료 신호
}
```

`stopped`와 `done` 두 채널의 역할:
- `stopped`: 종료 요청을 전달하는 채널 (비버퍼)
- `done`: 종료 완료를 알리는 채널 (close로 브로드캐스트)

---

## 12. 메트릭과 관찰 가능성

Ready 루프에서 업데이트되는 주요 메트릭:

| 메트릭 | 위치 | 설명 |
|--------|------|------|
| `leaderChanges` | SoftState 처리 | 리더 변경 횟수 |
| `hasLeader` | SoftState 처리 | 리더 존재 여부 (0 or 1) |
| `isLeader` | SoftState 처리 | 현재 노드가 리더인지 (0 or 1) |
| `proposalsCommitted` | HardState 처리 | 커밋된 제안 인덱스 |
| `heartbeatSendFailures` | processMessages | 하트비트 전송 지연 횟수 |
| `readIndexFailed` | requestCurrentIndex | ReadIndex 실패 횟수 |
| `slowReadIndex` | requestCurrentIndex | 느린 ReadIndex 횟수 |

expvar를 통한 Raft 상태 노출:

```go
// server/etcdserver/raft.go:54-63
func init() {
    expvar.Publish("raft.status", expvar.Func(func() any {
        raftStatusMu.Lock()
        defer raftStatusMu.Unlock()
        if raftStatus == nil {
            return nil
        }
        return raftStatus()
    }))
}
```

---

## 13. 정리: Raft 합의 엔진 아키텍처

```
┌──────────────────────────────────────────────────────────────┐
│                       EtcdServer                              │
│  ┌─────────────────┐  ┌─────────────────────────────────┐    │
│  │ linearizable     │  │ applyAll()                       │    │
│  │ ReadLoop         │  │  ├── <-applyc 수신               │    │
│  │  ├── readwaitc   │  │  ├── CommittedEntries 적용       │    │
│  │  ├── ReadIndex   │  │  └── applyWait.Trigger()         │    │
│  │  └── applyWait   │  │                                   │    │
│  └────────┬─────────┘  └─────────────┬───────────────────┘    │
│           │readStateC                 │applyc                  │
│  ┌────────┴───────────────────────────┴───────────────────┐    │
│  │                    raftNode                             │    │
│  │  start() 고루틴:                                        │    │
│  │   for { select {                                        │    │
│  │     ticker.C → tick()                                   │    │
│  │     Ready() → SoftState/ReadStates/toApply/Save/Send   │    │
│  │     stopped → return                                    │    │
│  │   }}                                                    │    │
│  │                                                         │    │
│  │  processMessages() → 중복제거, MsgSnap 리다이렉트       │    │
│  │  TimeoutDetector → 하트비트 지연 감시                    │    │
│  └─────────────┬───────────────────────┬──────────────────┘    │
│                │raft.Node              │transport               │
│  ┌─────────────┴──────────┐  ┌────────┴──────────────────┐    │
│  │  go.etcd.io/raft/v3    │  │  rafthttp.Transporter     │    │
│  │  - 선거 (PreVote/Vote) │  │  - gRPC 기반 메시지 전송   │    │
│  │  - 로그 복제            │  │  - 스냅샷 스트리밍          │    │
│  │  - ReadIndex            │  │  - 피어 연결 관리           │    │
│  │  - MemoryStorage        │  │                            │    │
│  └────────────────────────┘  └────────────────────────────┘    │
│                │storage                                        │
│  ┌─────────────┴──────────────────────────────────────────┐    │
│  │  serverstorage.Storage (WAL + Snap)                     │    │
│  │  - SaveSnap() / Save() / Sync() / Release()            │    │
│  └─────────────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────┘
```

이 아키텍처의 핵심 설계 원칙:
1. **Raft 라이브러리는 I/O를 하지 않는다** - Ready 채널을 통한 이벤트 기반 인터페이스
2. **리더의 병렬 최적화** - 디스크 쓰기와 네트워크 복제를 동시 수행
3. **ReadIndex로 선형 읽기** - Raft 로그를 거치지 않고도 일관된 읽기 보장
4. **배칭과 중복 제거** - MsgAppResp 중복 제거, ReadIndex 배칭으로 성능 최적화
5. **장애 감지와 관찰 가능성** - TimeoutDetector, 메트릭, expvar로 운영 지원
