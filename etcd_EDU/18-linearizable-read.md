# 18. 선형 읽기 & 동시성 (Linearizable Read & Concurrency)

## 개요

분산 시스템에서 "가장 최신 데이터를 읽는다"는 것은 단순한 문제가 아니다. 리더가 교체되었을 수 있고, 아직 적용되지 않은 커밋이 있을 수 있으며, 네트워크 파티션으로 인해 오래된 데이터를 반환할 수 있다. etcd는 이 문제를 Raft의 ReadIndex 프로토콜을 사용하여 해결하며, 이를 통해 **선형성(Linearizability)**을 보장한다.

선형성이란 "모든 읽기가 가장 최근에 완료된 쓰기의 결과를 반환한다"는 일관성 모델이다. etcd는 이를 위해 읽기 요청마다 리더에게 현재 커밋 인덱스를 확인하고, 해당 인덱스까지 로컬에 적용된 후에야 응답을 반환한다.

이 문서에서는 linearizableReadLoop, ReadIndex 프로토콜, readNotifier 배칭 메커니즘, Serializable vs Linearizable 차이, wait.Wait/WaitTime 레지스트리, EtcdServer의 동시성 패턴을 소스코드 기반으로 분석한다.

---

## Serializable vs Linearizable

### 두 가지 읽기 모드

etcd v3 API는 두 가지 읽기 일관성 수준을 제공한다:

```
┌─────────────────────────────────────────────────────────────────┐
│            Serializable vs Linearizable 읽기                    │
├─────────────────────┬───────────────────────────────────────────┤
│                     │ Serializable           │ Linearizable     │
├─────────────────────┼────────────────────────┼──────────────────┤
│ 일관성              │ 직렬 가능 (약한)        │ 선형 (강한)       │
│ 최신 보장           │ X (오래된 데이터 가능)   │ O (최신 보장)     │
│ 추가 비용           │ 없음                    │ ReadIndex 확인    │
│ 네트워크 왕복       │ 0회                     │ 리더 확인 1회     │
│ 리더 필요 여부      │ X (아무 노드에서 읽기)   │ O (리더 확인)     │
│ 지연 시간           │ 낮음                    │ 약간 높음         │
│ 사용 시나리오       │ 오래된 데이터 허용       │ 정확성 필수       │
│ 기본값              │                        │ O (기본 모드)     │
└─────────────────────┴────────────────────────┴──────────────────┘
```

### Range() 구현에서의 분기

```
소스 경로: server/etcdserver/v3_server.go (105행)

func (s *EtcdServer) Range(ctx context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error) {
    // ... tracing, span setup ...

    var resp *pb.RangeResponse
    var err error

    if !r.Serializable {
        // Linearizable: ReadIndex 프로토콜로 최신 확인
        err = s.linearizableReadNotify(ctx)
        trace.Step("agreement among raft nodes before linearized reading")
        if err != nil {
            return nil, err
        }
    }

    // 인증 확인
    chk := func(ai *auth.AuthInfo) error {
        return s.authStore.IsRangePermitted(ai, r.Key, r.RangeEnd)
    }

    // 실제 데이터 조회
    get := func() { resp, _, err = txn.Range(ctx, s.Logger(), s.KV(), r) }
    if serr := s.doSerialize(ctx, chk, get); serr != nil {
        err = serr
        return nil, err
    }
    return resp, err
}
```

**핵심 분기 로직:**

- `r.Serializable == true`: 즉시 로컬 데이터에서 읽기 (빠르지만 오래된 데이터 가능)
- `r.Serializable == false` (기본값): `linearizableReadNotify()`를 호출하여 최신 확인 후 읽기

---

## linearizableReadNotify: 읽기 요청 진입점

```
소스 경로: server/etcdserver/v3_server.go (1140행)

func (s *EtcdServer) linearizableReadNotify(ctx context.Context) error {
    // 1. 현재 readNotifier 획득
    s.readMu.RLock()
    nc := s.readNotifier
    s.readMu.RUnlock()

    // 2. linearizableReadLoop에 신호 전송 (비차단)
    select {
    case s.readwaitc <- struct{}{}:
    default:
    }

    // 3. 결과 대기
    select {
    case <-nc.c:          // 성공 또는 실패 알림
        return nc.err
    case <-ctx.Done():    // 클라이언트 타임아웃
        return ctx.Err()
    case <-s.done:        // 서버 종료
        return errors.ErrStopped
    }
}
```

**배칭(Batching) 메커니즘:**

```
┌─────────────────────────────────────────────────────────────────┐
│                    읽기 요청 배칭 메커니즘                        │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  시간 →                                                         │
│                                                                 │
│  요청1 ──┐                                                      │
│  요청2 ──┤  같은 readNotifier (nc1) 공유                         │
│  요청3 ──┘                                                      │
│           │                                                     │
│           ▼                                                     │
│  readwaitc ← struct{}{} (한 번만 전송)                           │
│           │                                                     │
│           ▼                                                     │
│  linearizableReadLoop 루프 1회 실행                              │
│  - readNotifier를 nc2로 교체                                    │
│  - ReadIndex → confirmedIndex 획득                              │
│  - applyWait.Wait(confirmedIndex)                               │
│  - nc1.notify(nil) → 요청1,2,3 모두 해제                        │
│                                                                 │
│  요청4 ──┐                                                      │
│  요청5 ──┘  새 readNotifier (nc2) 공유                           │
│           │                                                     │
│           ▼  다음 루프에서 처리                                   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**왜 배칭인가:**

1. ReadIndex는 리더에게 네트워크 왕복이 필요하다. 동시에 들어온 100개의 읽기 요청이 각각 ReadIndex를 보내면 100번 왕복해야 한다.
2. 배칭을 통해 같은 시점에 들어온 모든 읽기 요청을 하나의 ReadIndex로 처리한다.
3. `readwaitc`는 버퍼 1의 채널이므로, 여러 요청이 동시에 신호를 보내도 한 번만 루프가 실행된다.

---

## linearizableReadLoop: 핵심 루프

```
소스 경로: server/etcdserver/v3_server.go (972행)

func (s *EtcdServer) linearizableReadLoop() {
    for {
        // 1. 리더 변경 시 현재 루프 건너뛰기
        leaderChangedNotifier := s.leaderChanged.Receive()
        select {
        case <-leaderChangedNotifier:
            continue    // 리더가 바뀌면 기존 ReadIndex 무효
        case <-s.readwaitc:
            // 읽기 요청 수신
        case <-s.stopping:
            return
        }

        // 2. 새 readNotifier 생성 및 교체
        nextnr := newNotifier()
        s.readMu.Lock()
        nr := s.readNotifier       // 현재 대기 중인 notifier 가져오기
        s.readNotifier = nextnr    // 새 notifier로 교체
        s.readMu.Unlock()

        // 3. ReadIndex 프로토콜 실행
        confirmedIndex, err := s.requestCurrentIndex(leaderChangedNotifier)
        if isStopped(err) {
            return
        }
        if err != nil {
            nr.notify(err)     // 에러를 대기 중인 모든 요청에 전파
            continue
        }

        // 4. confirmedIndex까지 적용 완료 대기
        appliedIndex := s.getAppliedIndex()
        if appliedIndex < confirmedIndex {
            select {
            case <-s.applyWait.Wait(confirmedIndex):
                // 적용 완료
            case <-s.stopping:
                return
            }
        }

        // 5. 대기 중인 모든 읽기 요청 해제
        nr.notify(nil)
    }
}
```

**전체 흐름 다이어그램:**

```
┌─────────────────────────────────────────────────────────────────┐
│              linearizableReadLoop 상세 흐름                      │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  linearizableReadLoop (고루틴)                                   │
│  ┌────────────────────────────────────────────────────────┐     │
│  │  for {                                                 │     │
│  │    select:                                             │     │
│  │      leaderChanged → continue (ReadIndex 무효화)       │     │
│  │      readwaitc     → 처리 시작                         │     │
│  │      stopping      → return                           │     │
│  │                                                        │     │
│  │    readNotifier 교체 (nr = old, nextnr = new)          │     │
│  │                                                        │     │
│  │    requestCurrentIndex(leaderChangedNotifier)          │     │
│  │    ┌──────────────────────────────────────────────┐    │     │
│  │    │  sendReadIndex(requestID)                     │    │     │
│  │    │       │                                      │    │     │
│  │    │       ▼                                      │    │     │
│  │    │  Raft Leader:                                │    │     │
│  │    │    1. 현재 commitIndex 기록                   │    │     │
│  │    │    2. 과반수에 heartbeat 전송                  │    │     │
│  │    │    3. 과반수 응답 확인 → ReadState 반환        │    │     │
│  │    │       │                                      │    │     │
│  │    │       ▼                                      │    │     │
│  │    │  <-s.r.readStateC                            │    │     │
│  │    │  confirmedIndex = rs.Index                   │    │     │
│  │    └──────────────────────────────────────────────┘    │     │
│  │                                                        │     │
│  │    appliedIndex < confirmedIndex?                      │     │
│  │      → applyWait.Wait(confirmedIndex) 대기             │     │
│  │                                                        │     │
│  │    nr.notify(nil) → 대기 중인 모든 읽기 요청 해제       │     │
│  │  }                                                     │     │
│  └────────────────────────────────────────────────────────┘     │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## ReadIndex 프로토콜 상세

### requestCurrentIndex

```
소스 경로: server/etcdserver/v3_server.go (1028행)

func (s *EtcdServer) requestCurrentIndex(leaderChangedNotifier <-chan struct{}) (uint64, error) {
    requestIDs := map[uint64]struct{}{}
    requestID := s.reqIDGen.Next()
    requestIDs[requestID] = struct{}{}

    // 1. ReadIndex 메시지 전송
    err := s.sendReadIndex(requestID)
    if err != nil {
        return 0, err
    }

    lg := s.Logger()
    errorTimer := time.NewTimer(s.Cfg.ReqTimeout())
    defer errorTimer.Stop()
    retryTimer := time.NewTimer(readIndexRetryTime)   // 500ms
    defer retryTimer.Stop()

    firstCommitInTermNotifier := s.firstCommitInTerm.Receive()

    for {
        select {
        case rs := <-s.r.readStateC:
            // ReadState 수신
            select {
            case <-leaderChangedNotifier:
                readIndexFailed.Inc()
                return 0, errors.ErrLeaderChanged
            default:
            }

            requestID := binary.BigEndian.Uint64(rs.RequestCtx)
            if _, ok := requestIDs[requestID]; !ok {
                continue   // 자신의 요청이 아니면 무시
            }
            return rs.Index, nil   // confirmedIndex 반환

        case <-leaderChangedNotifier:
            return 0, errors.ErrLeaderChanged

        case <-firstCommitInTermNotifier:
            // 새 텀에서 첫 커밋 발생 → ReadIndex 재시도
            firstCommitInTermNotifier = s.firstCommitInTerm.Receive()
            requestID = s.reqIDGen.Next()
            requestIDs[requestID] = struct{}{}
            err = s.sendReadIndex(requestID)

        case <-retryTimer.C:
            // 500ms 초과 시 재시도
            requestID = s.reqIDGen.Next()
            requestIDs[requestID] = struct{}{}
            err = s.sendReadIndex(requestID)
            retryTimer.Reset(readIndexRetryTime)

        case <-errorTimer.C:
            // 전체 타임아웃
            readIndexFailed.Inc()
            return 0, errors.ErrTimeout

        case <-s.stopping:
            return 0, errors.ErrStopped
        }
    }
}
```

### ReadIndex 프로토콜의 Raft 내부 동작

```
┌─────────────────────────────────────────────────────────────────┐
│                  ReadIndex 프로토콜 상세                          │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  팔로워 노드                      리더 노드                      │
│  ┌──────────┐                    ┌──────────┐                   │
│  │ 1. Read  │  MsgReadIndex      │          │                   │
│  │    요청   │ ─────────────────→ │ 2. 현재  │                   │
│  │          │                    │ commitIdx│                   │
│  │          │                    │ 기록     │                   │
│  │          │                    │          │                   │
│  │          │                    │ 3. 과반수│                   │
│  │          │                    │ heartbeat│                   │
│  │          │                    │ 전송     │                   │
│  │          │                    │          │                   │
│  │          │                    │ 4. 과반수│                   │
│  │          │                    │ 응답 확인│                   │
│  │          │                    │          │                   │
│  │          │  MsgReadIndexResp  │ 5. Read  │                   │
│  │ 6. Read  │ ←───────────────── │ State    │                   │
│  │ State    │  (commitIdx 포함)   │ 반환     │                   │
│  │ 수신     │                    │          │                   │
│  │          │                    │          │                   │
│  │ 7. apply │                    │          │                   │
│  │ Wait     │                    │          │                   │
│  │ (commit  │                    │          │                   │
│  │ Idx까지) │                    │          │                   │
│  │          │                    │          │                   │
│  │ 8. 읽기  │                    │          │                   │
│  │ 수행     │                    │          │                   │
│  └──────────┘                    └──────────┘                   │
│                                                                 │
│  왜 heartbeat 확인이 필요한가:                                   │
│  - 네트워크 파티션으로 리더가 격리되었을 수 있음                    │
│  - 과반수 heartbeat 성공 = "나는 아직 유효한 리더"               │
│  - 이 확인 없이 commitIndex를 반환하면 오래된 데이터 가능          │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**리더 노드에서의 ReadIndex (자기 자신이 리더인 경우):**

리더 노드에서도 ReadIndex를 실행하며, 이 경우에도 과반수 heartbeat 확인이 필요하다. 리더가 네트워크 파티션에 의해 격리되었을 수 있기 때문이다.

---

## readwaitc, readNotifier 메커니즘

### readwaitc 채널

```
소스 경로: server/etcdserver/server.go

type EtcdServer struct {
    // ...
    readMu sync.RWMutex
    // read routine notifies etcd server that it waits for reading by sending
    // an empty struct to readwaitC
    readwaitc chan struct{}
    // readNotifier is used to notify the read routine that it can process
    // the request when there is no error
    readNotifier *notifier
    // ...
}

// 초기화
s.readwaitc = make(chan struct{}, 1)   // 버퍼 1
s.readNotifier = newNotifier()
```

**버퍼 1의 의미:**

- 버퍼가 0이면 `linearizableReadLoop`가 대기하지 않을 때 신호가 손실될 수 있다.
- 버퍼가 1이면 한 번의 신호가 버퍼에 저장되어, 루프가 다음에 select할 때 수신한다.
- `default` 분기로 인해 버퍼가 가득 차면 추가 신호는 무시된다 (이미 처리 대기 중).

### notifier 구조체

```
소스 경로: server/etcdserver/util.go (83행)

type notifier struct {
    c   chan struct{}
    err error
}

func newNotifier() *notifier {
    return &notifier{
        c: make(chan struct{}),   // 언버퍼드 채널
    }
}

func (nc *notifier) notify(err error) {
    nc.err = err
    close(nc.c)    // 채널을 닫아 모든 대기자 해제
}
```

**close(nc.c)의 브로드캐스트 효과:**

Go의 채널은 닫으면 모든 수신자가 즉시 해제된다. 이것이 1:N 브로드캐스트를 구현하는 핵심 패턴이다. 100개의 고루틴이 `<-nc.c`에서 대기 중이면, `close(nc.c)` 호출 한 번으로 100개 모두 해제된다.

---

## firstCommitInTerm: 텀 변경 시 첫 커밋 대기

### 왜 필요한가

Raft에서 새 리더가 선출되면 새 텀이 시작된다. 하지만 새 리더는 이전 텀의 커밋된 인덱스를 아직 확인하지 못한 상태일 수 있다. ReadIndex는 리더의 commitIndex를 기준으로 하므로, 새 텀에서 첫 번째 커밋이 발생해야 리더의 commitIndex가 정확해진다.

```
┌─────────────────────────────────────────────────────────────────┐
│            텀 변경과 firstCommitInTerm                           │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  Term 5 (리더 A)                                                │
│  ┌──────────────────────────────────────┐                       │
│  │ commitIndex = 100                    │                       │
│  │ 정상 ReadIndex 응답 가능              │                       │
│  └──────────────────────────────────────┘                       │
│                                                                 │
│  Term 6 (리더 B 선출)                                            │
│  ┌──────────────────────────────────────┐                       │
│  │ commitIndex = 100 (이전 텀에서 가져옴) │                       │
│  │ ⚠ 아직 Term 6에서 커밋된 것 없음       │                       │
│  │ ReadIndex 응답 불가                    │                       │
│  │                                       │                       │
│  │ no-op 엔트리 제안 → 커밋               │                       │
│  │ commitIndex = 101                     │                       │
│  │ firstCommitInTerm.Notify() 호출        │                       │
│  │ ✓ ReadIndex 응답 가능                  │                       │
│  └──────────────────────────────────────┘                       │
│                                                                 │
│  Raft 프로토콜: 새 리더는 현재 텀에서 첫 엔트리를                   │
│  커밋해야 이전 텀의 커밋 상태를 확인할 수 있음                      │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 소스코드에서의 사용

```
소스 경로: server/etcdserver/server.go (1946행)

// 빈 데이터 엔트리 (no-op) 적용 시 firstCommitInTerm 알림
if len(e.Data) == 0 {
    s.firstCommitInTerm.Notify()
    // Learner 승격 등의 후속 처리 ...
}
```

```
소스 경로: server/etcdserver/server.go (1709행)

// FirstCommitInTermNotify: 새 텀에서 첫 커밋 발생 시 알림 채널
func (s *EtcdServer) FirstCommitInTermNotify() <-chan struct{} {
    return s.firstCommitInTerm.Receive()
}
```

`requestCurrentIndex`에서 `firstCommitInTermNotifier`를 감시하여, 새 텀의 첫 커밋이 발생하면 ReadIndex를 재시도한다.

---

## wait.Wait 레지스트리

### Wait 인터페이스

```
소스 경로: pkg/wait/wait.go

type Wait interface {
    Register(id uint64) <-chan any    // ID로 대기 채널 등록
    Trigger(id uint64, x any)         // ID에 결과 전달
    IsRegistered(id uint64) bool
}
```

### list 구현: 샤딩된 맵

```
소스 경로: pkg/wait/wait.go

const defaultListElementLength = 64

type list struct {
    e []listElement
}

type listElement struct {
    l sync.RWMutex
    m map[uint64]chan any
}

func New() Wait {
    res := list{
        e: make([]listElement, defaultListElementLength),
    }
    for i := 0; i < len(res.e); i++ {
        res.e[i].m = make(map[uint64]chan any)
    }
    return &res
}
```

**64개 샤드로 분할하는 이유:**

```
┌─────────────────────────────────────────────────────────────────┐
│               wait.Wait 샤딩 구조                                │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  단일 맵 + 단일 뮤텍스:                                          │
│  ┌──────────────────────────────────────┐                       │
│  │  sync.RWMutex                        │ ← 모든 요청이 경합     │
│  │  map[uint64]chan any                  │                       │
│  └──────────────────────────────────────┘                       │
│                                                                 │
│  64개 샤드:                                                      │
│  ┌────────┐ ┌────────┐ ┌────────┐     ┌────────┐              │
│  │ shard0 │ │ shard1 │ │ shard2 │ ... │ shard63│              │
│  │ mutex  │ │ mutex  │ │ mutex  │     │ mutex  │              │
│  │ map    │ │ map    │ │ map    │     │ map    │              │
│  └────────┘ └────────┘ └────────┘     └────────┘              │
│                                                                 │
│  ID % 64 = 샤드 인덱스                                          │
│  → 동시에 최대 64개의 독립적인 잠금 가능                           │
│  → 락 경합 대폭 감소                                             │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Register / Trigger

```
func (w *list) Register(id uint64) <-chan any {
    idx := id % defaultListElementLength
    newCh := make(chan any, 1)   // 버퍼 1: Trigger가 먼저 호출되어도 차단 안 됨
    w.e[idx].l.Lock()
    defer w.e[idx].l.Unlock()
    if _, ok := w.e[idx].m[id]; !ok {
        w.e[idx].m[id] = newCh
    } else {
        log.Panicf("dup id %x", id)   // 중복 ID 방지
    }
    return newCh
}

func (w *list) Trigger(id uint64, x any) {
    idx := id % defaultListElementLength
    w.e[idx].l.Lock()
    ch := w.e[idx].m[id]
    delete(w.e[idx].m, id)   // 맵에서 제거 (1회성)
    w.e[idx].l.Unlock()
    if ch != nil {
        ch <- x       // 결과 전달
        close(ch)     // 채널 닫기
    }
}
```

### configure() 메서드에서의 사용

```
func (s *EtcdServer) configure(ctx context.Context, cc raftpb.ConfChange) ([]*membership.Member, error) {
    cc.ID = s.reqIDGen.Next()
    ch := s.w.Register(cc.ID)     // Wait 레지스트리에 등록

    s.r.ProposeConfChange(ctx, cc) // Raft에 제안

    select {
    case x := <-ch:               // 적용 결과 수신
        resp := x.(*confChangeResponse)
        return resp.membs, resp.err
    case <-ctx.Done():
        s.w.Trigger(cc.ID, nil)   // 타임아웃 시 정리
        return nil, ...
    }
}
```

---

## wait.WaitTime: 적용 완료 대기

### WaitTime 인터페이스

```
소스 경로: pkg/wait/wait_time.go

type WaitTime interface {
    Wait(deadline uint64) <-chan struct{}
    Trigger(deadline uint64)
}
```

### timeList 구현

```
type timeList struct {
    l                   sync.Mutex
    lastTriggerDeadline uint64
    m                   map[uint64]chan struct{}
}

func (tl *timeList) Wait(deadline uint64) <-chan struct{} {
    tl.l.Lock()
    defer tl.l.Unlock()

    // 이미 트리거된 deadline이면 즉시 닫힌 채널 반환
    if tl.lastTriggerDeadline >= deadline {
        return closec   // var closec = make(chan struct{}); close(closec)
    }

    ch := tl.m[deadline]
    if ch == nil {
        ch = make(chan struct{})
        tl.m[deadline] = ch
    }
    return ch
}

func (tl *timeList) Trigger(deadline uint64) {
    tl.l.Lock()
    defer tl.l.Unlock()

    tl.lastTriggerDeadline = deadline

    // deadline 이하의 모든 대기자 해제
    for t, ch := range tl.m {
        if t <= deadline {
            delete(tl.m, t)
            close(ch)   // 브로드캐스트
        }
    }
}
```

**WaitTime vs Wait 차이:**

```
┌─────────────────────────────────────────────────────────────────┐
│                    Wait vs WaitTime                              │
├──────────────────┬──────────────────────────────────────────────┤
│      Wait        │  WaitTime                                    │
├──────────────────┼──────────────────────────────────────────────┤
│ 용도: 특정 요청   │  용도: 인덱스 기반 대기                      │
│ ID 기반 1:1 매핑  │  deadline 기반 N:M 매핑                     │
│ Register + Trigger│  Wait + Trigger                             │
│ 결과 전달 (any)   │  완료 신호만 (struct{})                      │
│ ConfChange 대기   │  appliedIndex 대기                          │
│ 1회성             │  deadline ≤ lastTrigger면 즉시 반환          │
└──────────────────┴──────────────────────────────────────────────┘
```

### applyWait 사용 패턴

```
소스 경로: server/etcdserver/server.go

// 초기화
s.applyWait = wait.NewTimeList()

// Trigger: apply loop에서 인덱스 적용 완료 시
proposalsApplied.Set(float64(ep.appliedi))
s.applyWait.Trigger(ep.appliedi)

// Wait: linearizableReadLoop에서 confirmedIndex까지 대기
if appliedIndex < confirmedIndex {
    select {
    case <-s.applyWait.Wait(confirmedIndex):
    case <-s.stopping:
        return
    }
}

// Wait: ApplyWait() 퍼블릭 API
func (s *EtcdServer) ApplyWait() <-chan struct{} {
    return s.applyWait.Wait(s.getCommittedIndex())
}
```

---

## pkg/notify.Notifier 패턴

### Notifier 구조체

```
소스 경로: pkg/notify/notify.go

type Notifier struct {
    mu      sync.RWMutex
    channel chan struct{}
}

func NewNotifier() *Notifier {
    return &Notifier{
        channel: make(chan struct{}),
    }
}

func (n *Notifier) Receive() <-chan struct{} {
    n.mu.RLock()
    defer n.mu.RUnlock()
    return n.channel
}

func (n *Notifier) Notify() {
    newChannel := make(chan struct{})
    n.mu.Lock()
    channelToClose := n.channel
    n.channel = newChannel     // 새 채널로 교체
    n.mu.Unlock()
    close(channelToClose)      // 이전 채널 닫기 (브로드캐스트)
}
```

**Notify 패턴의 재사용성:**

```
┌─────────────────────────────────────────────────────────────────┐
│                  Notifier 패턴 동작                               │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  시점 1: ch1 = Receive()                                         │
│  ┌──────────────────────────────────┐                           │
│  │ Notifier.channel = ch1 (open)    │                           │
│  └──────────────────────────────────┘                           │
│                                                                 │
│  시점 2: Notify() 호출                                           │
│  ┌──────────────────────────────────┐                           │
│  │ 1. 새 채널 ch2 생성               │                           │
│  │ 2. channel = ch2                  │                           │
│  │ 3. close(ch1) → 시점1 대기자 해제  │                           │
│  └──────────────────────────────────┘                           │
│                                                                 │
│  시점 3: ch2 = Receive()                                         │
│  ┌──────────────────────────────────┐                           │
│  │ Notifier.channel = ch2 (open)    │                           │
│  │ 다음 Notify()까지 대기            │                           │
│  └──────────────────────────────────┘                           │
│                                                                 │
│  핵심: 채널을 닫으면 재사용 불가 → 새 채널로 교체                   │
│  → 반복적인 이벤트 알림 가능                                      │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### EtcdServer에서의 Notifier 사용

| Notifier 인스턴스 | 알림 시점 | 소비자 |
|-------------------|----------|--------|
| `leaderChanged` | 리더 변경 시 | linearizableReadLoop |
| `firstCommitInTerm` | 새 텀에서 첫 no-op 커밋 | requestCurrentIndex |
| `clusterVersionChanged` | 클러스터 버전 변경 | monitorVersions |
| `versionChanged` (RaftCluster) | 버전 설정 변경 | 버전 관련 작업 |

---

## EtcdServer의 동시성 패턴

### atomic 변수

```
소스 경로: server/etcdserver/server.go

// 원자적 카운터: 락 없이 안전한 읽기/쓰기
func (s *EtcdServer) setAppliedIndex(v uint64) {
    s.appliedIndex.Store(v)
}
func (s *EtcdServer) getAppliedIndex() uint64 {
    return s.appliedIndex.Load()
}

func (s *EtcdServer) setCommittedIndex(v uint64) {
    s.committedIndex.Store(v)
}

func (s *EtcdServer) setTerm(v uint64) {
    s.term.Store(v)
}

func (s *EtcdServer) setLead(v uint64) {
    s.lead.Store(v)
}
```

**왜 atomic인가:**

- `appliedIndex`, `committedIndex`, `term`, `lead`는 매우 빈번하게 읽히고 드물게 쓰인다.
- sync.RWMutex를 사용하면 읽기마다 락 획득/해제 오버헤드가 발생한다.
- `atomic.Uint64`는 하드웨어 수준의 원자적 연산으로 락 없이 안전한 접근을 보장한다.

### sync.RWMutex 사용

```
// readMu: readNotifier 보호 (읽기 빈번, 쓰기 드묾)
s.readMu.RLock()
nc := s.readNotifier
s.readMu.RUnlock()

// readMu: readNotifier 교체 시 쓰기 잠금
s.readMu.Lock()
nr := s.readNotifier
s.readNotifier = nextnr
s.readMu.Unlock()
```

**RWMutex vs Mutex 선택 기준:**

| 패턴 | 사용 동기화 도구 | 이유 |
|------|---------------|------|
| 단순 카운터 | atomic | 최소 오버헤드 |
| 빈번한 읽기 + 드문 쓰기 | sync.RWMutex | 읽기 병렬성 보장 |
| 복합 상태 변경 | sync.Mutex | 상호 배제 |
| 이벤트 대기 | channel | Go 관용적 패턴 |
| 1:N 브로드캐스트 | close(channel) | 모든 대기자 해제 |

### channel 기반 동시성

```
EtcdServer의 주요 채널:

┌─────────────────────────────────────────────────────────────────┐
│                    EtcdServer 채널 구조                           │
├────────────────────┬────────────────────────────────────────────┤
│ 채널               │ 용도                                       │
├────────────────────┼────────────────────────────────────────────┤
│ readwaitc          │ 읽기 요청 신호 (buf=1)                     │
│ stopping           │ 서버 정지 중 신호 (buf=1)                   │
│ stop               │ 서버 정지 요청                              │
│ done               │ 모든 고루틴 완료                            │
│ errorc             │ 에러 전파                                   │
│ r.readStateC       │ Raft ReadState 수신                        │
│ confChangeResponse │ ConfChange 결과 (via wait.Wait)            │
│ raftAdvanceC       │ Raft advance 완료 신호                     │
└────────────────────┴────────────────────────────────────────────┘
```

### select 다중 채널 패턴

linearizableReadLoop의 select는 Go의 다중 채널 처리 패턴을 잘 보여준다:

```go
select {
case <-leaderChangedNotifier:   // 우선순위 1: 리더 변경
    continue
case <-s.readwaitc:             // 일반: 읽기 요청
    // 처리
case <-s.stopping:              // 종료: 서버 정지
    return
}
```

**주의**: Go의 select는 여러 채널이 동시에 준비되면 무작위로 선택한다. `requestCurrentIndex`에서 `leaderChangedNotifier`를 이중 검사하는 이유가 여기에 있다:

```go
case rs := <-s.r.readStateC:
    // ReadState를 수신했지만, 동시에 리더가 변경되었을 수 있음
    select {
    case <-leaderChangedNotifier:
        readIndexFailed.Inc()
        return 0, errors.ErrLeaderChanged   // ReadState 무효
    default:
        // 리더 변경 없음 → ReadState 유효
    }
```

---

## 선형 읽기의 성능 최적화

### 배칭의 효과

```
배칭 없이 (요청당 ReadIndex):

  요청1 → ReadIndex → heartbeat → 응답 → 읽기    ~2ms
  요청2 → ReadIndex → heartbeat → 응답 → 읽기    ~2ms
  요청3 → ReadIndex → heartbeat → 응답 → 읽기    ~2ms
  총: ~6ms, 네트워크 왕복 3회

배칭 있음 (N개 요청 1회 ReadIndex):

  요청1 ─┐
  요청2 ──┼→ ReadIndex → heartbeat → 응답 → 읽기1,2,3  ~2ms
  요청3 ─┘
  총: ~2ms, 네트워크 왕복 1회
```

### readIndexRetryTime

```
const readIndexRetryTime = 500 * time.Millisecond
```

500ms 후에 ReadIndex를 재시도한다. 이 값은:
- 일반적인 Raft 선거 타임아웃(~1초)보다 짧아 빠른 복구 가능
- 너무 짧으면 불필요한 재시도로 네트워크 부하 증가

---

## 전체 선형 읽기 흐름 요약

```
┌─────────────────────────────────────────────────────────────────┐
│                   선형 읽기 전체 흐름                              │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. 클라이언트 Range 요청 (Serializable=false)                    │
│     │                                                           │
│  2. linearizableReadNotify()                                     │
│     ├─ readNotifier 획득                                         │
│     ├─ readwaitc에 신호 전송                                     │
│     └─ readNotifier.c 대기                                       │
│     │                                                           │
│  3. linearizableReadLoop (별도 고루틴)                             │
│     ├─ readNotifier 교체 (old → new)                             │
│     ├─ requestCurrentIndex()                                     │
│     │   ├─ sendReadIndex(requestID)                              │
│     │   ├─ Raft 리더: commitIndex 기록 + 과반수 heartbeat         │
│     │   └─ readStateC에서 confirmedIndex 수신                    │
│     ├─ applyWait.Wait(confirmedIndex)                            │
│     └─ nr.notify(nil) → close(nr.c) → 모든 대기자 해제           │
│     │                                                           │
│  4. linearizableReadNotify() 반환 (err=nil)                      │
│     │                                                           │
│  5. txn.Range() 실행 (로컬 데이터에서 읽기)                        │
│     │                                                           │
│  6. 클라이언트에 응답 반환                                         │
│                                                                 │
│  시간: ~2ms (네트워크 왕복 1회 + 로컬 적용 대기)                    │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## 핵심 정리

| 주제 | 핵심 내용 |
|------|----------|
| Serializable 읽기 | 로컬 데이터 즉시 반환, 오래된 데이터 가능 |
| Linearizable 읽기 | ReadIndex로 최신 확인 후 반환, 강한 일관성 |
| ReadIndex 프로토콜 | 리더의 commitIndex + 과반수 heartbeat 확인 |
| 배칭 | 동시 읽기 요청을 하나의 ReadIndex로 묶어 네트워크 비용 절감 |
| readwaitc | buf=1 채널, 읽기 요청 신호 (중복 신호 무시) |
| readNotifier | close(channel)로 1:N 브로드캐스트, 매 루프 교체 |
| firstCommitInTerm | 새 텀에서 no-op 커밋 후 ReadIndex 가능 |
| wait.Wait | 64 샤드 맵, Register/Trigger로 요청-응답 매칭 |
| wait.WaitTime | deadline 기반, appliedIndex 대기에 사용 |
| notify.Notifier | 재사용 가능 브로드캐스트, 채널 교체 패턴 |
| atomic | appliedIndex, term, lead 등 빈번한 읽기용 |
| sync.RWMutex | readNotifier 등 읽기 빈번 + 쓰기 드문 패턴 |
| applyWait.Trigger | apply loop에서 appliedIndex 갱신 시 호출 |
| leaderChanged | 리더 변경 시 기존 ReadIndex 무효화 |
