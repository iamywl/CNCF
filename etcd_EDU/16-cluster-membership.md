# 16. 클러스터 멤버십 (Cluster Membership)

## 개요

etcd 클러스터 멤버십은 분산 합의 시스템에서 노드의 추가, 제거, 업데이트, 승격을 안전하게 관리하는 서브시스템이다. 단순한 노드 목록 관리가 아니라, Raft 합의 프로토콜과 긴밀하게 통합되어 클러스터의 일관성을 보장하면서 동적 구성 변경(Dynamic Reconfiguration)을 수행한다.

이 문서에서는 etcd의 클러스터 멤버십 관리를 구성하는 핵심 데이터 구조, 멤버 변경 흐름, Learner 노드 개념, 피어 통신, 멤버 ID 생성, 클러스터 부트스트랩 과정을 소스코드 기반으로 분석한다.

---

## 핵심 데이터 구조

### RaftCluster 구조체

`server/etcdserver/api/membership/cluster.go`에 정의된 `RaftCluster`는 클러스터의 전체 멤버십 상태를 관리하는 중심 구조체다.

```
소스 경로: server/etcdserver/api/membership/cluster.go

type RaftCluster struct {
    lg      *zap.Logger
    localID types.ID          // 로컬 노드의 ID
    cid     types.ID          // 클러스터 ID

    be MembershipBackend      // 백엔드 저장소 (bbolt)

    sync.Mutex                // 아래 필드 보호
    version    *semver.Version          // 클러스터 버전
    members    map[types.ID]*Member     // 현재 멤버 맵
    removed    map[types.ID]bool        // 제거된 멤버 ID (재사용 불가)
    downgradeInfo  *serverversion.DowngradeInfo  // 다운그레이드 정보
    maxLearners    int                           // 최대 Learner 수
    versionChanged *notify.Notifier              // 버전 변경 알림
}
```

**설계 의도:**

| 필드 | 역할 | 왜 필요한가 |
|------|------|------------|
| `members` | 현재 활성 멤버 맵 | ID 기반 O(1) 조회로 빠른 멤버 검색 |
| `removed` | 제거된 멤버 ID 집합 | 제거된 ID의 재사용을 방지하여 혼란 차단 |
| `sync.Mutex` | 동시성 보호 | 여러 고루틴이 동시에 멤버십을 수정할 수 있음 |
| `localID` | 로컬 노드 식별 | 자기 자신에 대한 작업(메트릭, 판단)에 사용 |
| `cid` | 클러스터 식별 | 서로 다른 클러스터 간 메시지 혼입 방지 |
| `maxLearners` | Learner 수 제한 | 무분별한 Learner 추가로 인한 리소스 낭비 방지 |

**제거된 멤버 ID 재사용 방지의 이유:**

Raft 프로토콜에서 노드 ID는 고유해야 한다. 만약 제거된 노드의 ID를 재사용하면, 이전 노드의 WAL(Write-Ahead Log)이나 스냅샷 데이터가 새 노드에 잘못 적용될 위험이 있다. `removed` 맵은 이 문제를 원천적으로 차단한다.

### Member 구조체

`server/etcdserver/api/membership/member.go`에 정의된 `Member`는 클러스터 내 개별 노드를 나타낸다.

```
소스 경로: server/etcdserver/api/membership/member.go

type RaftAttributes struct {
    PeerURLs  []string `json:"peerURLs"`           // 피어 통신 URL
    IsLearner bool     `json:"isLearner,omitempty"` // Learner 여부
}

type Attributes struct {
    Name       string   `json:"name,omitempty"`      // 노드 이름
    ClientURLs []string `json:"clientURLs,omitempty"` // 클라이언트 URL
}

type Member struct {
    ID types.ID `json:"id"`
    RaftAttributes                   // Raft 관련 속성 (임베딩)
    Attributes                       // 비-Raft 속성 (임베딩)
}
```

**두 가지 속성 분리의 이유:**

- `RaftAttributes`는 Raft 합의에 필수적인 정보(피어 URL, Learner 상태)로, 변경 시 반드시 Raft ConfChange를 통해야 한다.
- `Attributes`는 클라이언트 URL, 노드 이름 등 Raft 합의와 무관한 메타데이터로, ConfChange 없이 업데이트 가능하다.

이 분리를 통해 클라이언트 URL 변경 같은 가벼운 작업이 무거운 Raft 합의를 거치지 않아도 된다.

### ConfigChangeContext 구조체

```
소스 경로: server/etcdserver/api/membership/cluster.go

type ConfigChangeContext struct {
    Member
    IsPromote bool `json:"isPromote"`
}
```

`ConfigChangeContext`는 Raft의 `ConfChange` 메시지에 함께 전달되는 컨텍스트 정보다. `IsPromote` 플래그가 필요한 이유는 Raft 프로토콜에서 "새 멤버 추가"와 "Learner 승격" 모두 `ConfChangeAddNode` 타입을 사용하기 때문이다. 이 플래그로 두 작업을 구분한다.

---

## 멤버 ID 생성 알고리즘

### computeMemberID 함수

```
소스 경로: server/etcdserver/api/membership/member.go

func computeMemberID(peerURLs types.URLs, clusterName string, now *time.Time) types.ID {
    peerURLstrs := peerURLs.StringSlice()
    sort.Strings(peerURLstrs)
    joinedPeerUrls := strings.Join(peerURLstrs, "")
    b := []byte(joinedPeerUrls)

    b = append(b, []byte(clusterName)...)
    if now != nil {
        b = append(b, []byte(fmt.Sprintf("%d", now.Unix()))...)
    }

    hash := sha1.Sum(b)
    return types.ID(binary.BigEndian.Uint64(hash[:8]))
}
```

**알고리즘 상세:**

```
┌─────────────────────────────────────────────────────────────┐
│                  멤버 ID 생성 과정                           │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  1. PeerURLs 정렬 (결정적 순서 보장)                          │
│     ["http://b:2380", "http://a:2380"]                      │
│     → ["http://a:2380", "http://b:2380"]                    │
│                                                             │
│  2. 문자열 결합                                               │
│     "http://a:2380http://b:2380"                            │
│                                                             │
│  3. 클러스터 이름 추가                                        │
│     "http://a:2380http://b:2380my-cluster"                  │
│                                                             │
│  4. 타임스탬프 추가 (런타임 추가 시)                           │
│     "http://a:2380http://b:2380my-cluster1678901234"        │
│                                                             │
│  5. SHA-1 해시 계산                                          │
│     sha1.Sum(bytes) → 20바이트 해시                          │
│                                                             │
│  6. 상위 8바이트를 uint64로 변환                               │
│     binary.BigEndian.Uint64(hash[:8]) → Member ID           │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

**왜 SHA-1인가:**

- SHA-1은 20바이트(160비트) 해시를 생성하며, 그 중 상위 8바이트(64비트)만 사용한다.
- 충돌 확률이 극히 낮아 실용적으로 고유 ID를 보장한다.
- 같은 입력에 대해 항상 같은 ID를 생성하므로, 초기 부트스트랩 시 모든 노드가 독립적으로 동일한 ID를 계산할 수 있다.

**부트스트랩 vs 런타임 추가의 차이:**

- 초기 부트스트랩: `now`이 `nil`이므로 PeerURLs + clusterName만으로 ID 생성. 모든 노드가 동일한 ID를 독립적으로 계산 가능.
- 런타임 추가: `now`에 현재 시간이 포함되어 고유성을 더욱 강화. 같은 PeerURL로 재추가해도 다른 ID 생성.

### 클러스터 ID 생성 (genID)

```
소스 경로: server/etcdserver/api/membership/cluster.go

func (c *RaftCluster) genID() {
    mIDs := c.MemberIDs()
    b := make([]byte, 8*len(mIDs))
    for i, id := range mIDs {
        binary.BigEndian.PutUint64(b[8*i:], uint64(id))
    }
    hash := sha1.Sum(b)
    c.cid = types.ID(binary.BigEndian.Uint64(hash[:8]))
}
```

클러스터 ID는 모든 멤버 ID를 결합한 SHA-1 해시로 생성된다. 이를 통해:
- 같은 멤버 구성의 클러스터는 항상 같은 클러스터 ID를 가진다.
- 서로 다른 클러스터 간 메시지 혼입을 클러스터 ID 비교로 감지할 수 있다.

---

## 멤버 변경 흐름

### configure() 메서드: 핵심 ConfChange 제안 경로

모든 멤버십 변경 작업(추가, 제거, 업데이트, 승격)은 궁극적으로 `configure()` 메서드를 통해 Raft에 제안된다.

```
소스 경로: server/etcdserver/server.go (1742행)

func (s *EtcdServer) configure(ctx context.Context, cc raftpb.ConfChange) ([]*membership.Member, error) {
    cc.ID = s.reqIDGen.Next()     // 고유 요청 ID 생성
    ch := s.w.Register(cc.ID)     // Wait 레지스트리에 등록

    start := time.Now()
    if err := s.r.ProposeConfChange(ctx, cc); err != nil {
        s.w.Trigger(cc.ID, nil)   // 실패 시 대기 해제
        return nil, err
    }

    select {
    case x := <-ch:               // ConfChange 적용 완료 대기
        resp := x.(*confChangeResponse)
        <-resp.raftAdvanceC       // Raft advance 완료까지 대기
        return resp.membs, resp.err

    case <-ctx.Done():
        s.w.Trigger(cc.ID, nil)   // 타임아웃 시 정리
        return nil, s.parseProposeCtxErr(ctx.Err(), start)

    case <-s.stopping:
        return nil, errors.ErrStopped
    }
}
```

**흐름 다이어그램:**

```
┌──────────┐    ConfChange     ┌───────┐    Commit     ┌─────────┐
│  Client  │ ───────────────→ │  Raft  │ ──────────→  │  Apply   │
│  API     │                   │ Leader │              │  Loop    │
└──────────┘                   └───────┘              └────┬────┘
     │                              │                       │
     │  1. reqIDGen.Next()          │                       │
     │  2. w.Register(id)           │                       │
     │  3. ProposeConfChange()      │                       │
     │                              │                       │
     │          ← ─ ─ ─ ─ ─ ─ ─ ─ ─│─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─│
     │                              │  4. ValidateCC()      │
     │                              │  5. ApplyConfChange() │
     │  6. <-ch (결과 수신)          │  6. w.Trigger(id,resp)│
     │  7. <-raftAdvanceC           │                       │
     └──────────────────────────────┘──────────────────────┘
```

**raftAdvanceC를 기다리는 이유:**

Raft 엔진이 ConfChange를 커밋했더라도, 아직 내부적으로 advance되지 않은 상태에서 다음 ConfChange를 제안하면 거부될 수 있다. `raftAdvanceC` 채널을 통해 Raft가 완전히 새 설정을 반영한 후에야 응답을 반환하여, 연속적인 ConfChange 요청의 안정성을 보장한다.

### AddMember() 흐름

```
소스 경로: server/etcdserver/server.go (1381행)

func (s *EtcdServer) AddMember(ctx context.Context, memb membership.Member) ([]*membership.Member, error) {
    // 1. 권한 확인
    if err := s.checkMembershipOperationPermission(ctx); err != nil {
        return nil, err
    }

    // 2. 멤버 정보를 JSON 직렬화
    b, err := json.Marshal(memb)

    // 3. 건강 상태 확인 (StrictReconfigCheck 활성 시)
    if err := s.mayAddMember(memb); err != nil {
        return nil, err
    }

    // 4. ConfChange 구성
    cc := raftpb.ConfChange{
        Type:    raftpb.ConfChangeAddNode,
        NodeID:  uint64(memb.ID),
        Context: b,
    }

    // 5. Learner이면 타입 변경
    if memb.IsLearner {
        cc.Type = raftpb.ConfChangeAddLearnerNode
    }

    // 6. Raft에 제안
    return s.configure(ctx, cc)
}
```

**mayAddMember 안전 검사:**

```
func (s *EtcdServer) mayAddMember(memb membership.Member) error {
    // 1. StrictReconfigCheck가 비활성이면 검사 생략
    if !s.Cfg.StrictReconfigCheck {
        return nil
    }

    // 2. 투표 멤버 추가 시, 현재 쿼럼이 유지되는지 확인
    if !memb.IsLearner && !s.cluster.IsReadyToAddVotingMember() {
        return errors.ErrNotEnoughStartedMembers
    }

    // 3. 모든 투표 멤버와 연결되어 있는지 확인 (최근 5초 이내)
    if !isConnectedFullySince(..., HealthInterval, ...) {
        return errors.ErrUnhealthy
    }
    return nil
}
```

**IsReadyToAddVotingMember의 쿼럼 보호:**

```
소스 경로: server/etcdserver/api/membership/cluster.go

func (c *RaftCluster) IsReadyToAddVotingMember() bool {
    nmembers := 1    // 추가될 새 멤버 포함
    nstarted := 0    // 시작된(활성) 멤버 수

    for _, member := range c.VotingMembers() {
        if member.IsStarted() {
            nstarted++
        }
        nmembers++
    }

    // 특수 케이스: 1노드 클러스터에서 2노드로 확장 (데이터 복구 시나리오)
    if nstarted == 1 && nmembers == 2 {
        return true
    }

    // 쿼럼 검사: 시작된 멤버 수 >= 쿼럼
    nquorum := nmembers/2 + 1
    if nstarted < nquorum {
        return false
    }
    return true
}
```

### RemoveMember() 흐름

```
소스 경로: server/etcdserver/server.go (1440행)

func (s *EtcdServer) RemoveMember(ctx context.Context, id uint64) ([]*membership.Member, error) {
    if err := s.checkMembershipOperationPermission(ctx); err != nil {
        return nil, err
    }

    // 제거 후에도 쿼럼이 유지되는지 확인
    if err := s.mayRemoveMember(types.ID(id)); err != nil {
        return nil, err
    }

    cc := raftpb.ConfChange{
        Type:   raftpb.ConfChangeRemoveNode,
        NodeID: id,
    }
    return s.configure(ctx, cc)
}
```

**IsReadyToRemoveVotingMember:**

제거 후 남은 활성 멤버 수가 쿼럼 이상인지 검사한다. 예를 들어 3노드 클러스터에서 1노드가 이미 다운된 상태에서 또 다른 노드를 제거하면 쿼럼(2)을 유지할 수 없으므로 거부된다.

### UpdateMember() 흐름

```
소스 경로: server/etcdserver/server.go (1652행)

func (s *EtcdServer) UpdateMember(ctx context.Context, memb membership.Member) ([]*membership.Member, error) {
    b, merr := json.Marshal(memb)
    if merr != nil {
        return nil, merr
    }

    if err := s.checkMembershipOperationPermission(ctx); err != nil {
        return nil, err
    }

    cc := raftpb.ConfChange{
        Type:    raftpb.ConfChangeUpdateNode,
        NodeID:  uint64(memb.ID),
        Context: b,    // 새 PeerURLs 포함
    }
    return s.configure(ctx, cc)
}
```

UpdateMember는 주로 PeerURL을 변경할 때 사용된다. Raft ConfChange를 통해 모든 노드에 일관되게 적용된다.

### PromoteMember() 흐름

```
소스 경로: server/etcdserver/server.go (1458행)

func (s *EtcdServer) PromoteMember(ctx context.Context, id uint64) ([]*membership.Member, error) {
    // 리더에서만 Learner 준비 상태 판단 가능
    resp, err := s.promoteMember(ctx, id)
    if err == nil {
        learnerPromoteSucceed.Inc()
        return resp, nil
    }

    // ErrNotLeader인 경우, 리더에게 HTTP로 전달
    if !errorspkg.Is(err, errors.ErrNotLeader) {
        learnerPromoteFailed.WithLabelValues(err.Error()).Inc()
        return resp, err
    }

    // 리더 노드에 HTTP 요청으로 포워딩
    for cctx.Err() == nil {
        leader, err := s.waitLeader(cctx)
        for _, url := range leader.PeerURLs {
            resp, err := promoteMemberHTTP(cctx, url, id, s.peerRt)
            if err == nil {
                return resp, nil
            }
        }
    }
}
```

**리더 전달이 필요한 이유:**

Learner의 준비 상태(로그 복제 진행률)는 리더만 알 수 있다. 팔로워가 승격 요청을 받으면 리더에게 전달해야 정확한 판단이 가능하다.

---

## ValidateConfigurationChange: ConfChange 검증

모든 ConfChange는 적용 전에 `ValidateConfigurationChange()`로 검증된다.

```
소스 경로: server/etcdserver/api/membership/cluster.go (305행)

func (c *RaftCluster) ValidateConfigurationChange(cc raftpb.ConfChange, shouldApplyV3 ShouldApplyV3) error {
    membersMap, removedMap := c.be.MustReadMembersFromBackend()
    id := types.ID(cc.NodeID)

    // 제거된 ID는 어떤 작업도 불가
    if removedMap[id] {
        return ErrIDRemoved
    }

    switch cc.Type {
    case raftpb.ConfChangeAddNode, raftpb.ConfChangeAddLearnerNode:
        confChangeContext := new(ConfigChangeContext)
        json.Unmarshal(cc.Context, confChangeContext)

        if confChangeContext.IsPromote {
            // 승격: 멤버가 존재하고 Learner여야 함
            if membersMap[id] == nil { return ErrIDNotFound }
            if !membersMap[id].IsLearner { return ErrMemberNotLearner }
        } else {
            // 추가: ID가 이미 존재하면 안 됨
            if membersMap[id] != nil { return ErrIDExists }
            // PeerURL 중복 검사
            // Learner 수 제한 검사 (maxLearners)
        }

    case raftpb.ConfChangeRemoveNode:
        if membersMap[id] == nil { return ErrIDNotFound }

    case raftpb.ConfChangeUpdateNode:
        if membersMap[id] == nil { return ErrIDNotFound }
        // PeerURL 중복 검사 (자기 자신 제외)
    }
    return nil
}
```

**검증 항목 요약:**

| ConfChange 타입 | 검증 내용 |
|-----------------|----------|
| AddNode | ID 중복 없음, PeerURL 중복 없음, 제거된 ID 아님 |
| AddLearnerNode | 위와 동일 + maxLearners 제한 |
| AddNode (Promote) | 멤버 존재, IsLearner=true |
| RemoveNode | 멤버 존재 |
| UpdateNode | 멤버 존재, PeerURL 중복 없음 |

---

## Learner 노드 개념과 승격

### Learner란 무엇인가

Learner(학습자)는 투표에 참여하지 않는 Raft 노드다. Raft 3.4에서 도입되었으며, 새 노드를 클러스터에 안전하게 추가하기 위한 메커니즘이다.

```
┌─────────────────────────────────────────────────────────────────┐
│                    Learner 노드의 역할                           │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  일반 멤버 (Voter)                   Learner                     │
│  ┌─────────────┐                   ┌─────────────┐              │
│  │ 투표 참여: O │                   │ 투표 참여: X │              │
│  │ 로그 수신: O │                   │ 로그 수신: O │              │
│  │ 리더 가능: O │                   │ 리더 가능: X │              │
│  │ 쿼럼 영향: O │                   │ 쿼럼 영향: X │              │
│  │ 읽기 제공: O │                   │ 읽기 제공: X │              │
│  └─────────────┘                   └─────────────┘              │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │ Learner가 로그를 충분히 따라잡으면 (readyPercentThreshold │    │
│  │ = 0.9, 즉 90%) Voter로 승격 가능                        │    │
│  └─────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────┘
```

**Learner가 필요한 이유:**

3노드 클러스터에 4번째 노드를 직접 Voter로 추가하면, 이 노드가 로그를 따라잡기 전까지 쿼럼이 3에서 3(4/2+1)으로 유지되지만, 새 노드가 다운되면 사실상 3노드 중 3이 필요한 상황이 된다. Learner는 쿼럼에 영향을 주지 않으므로 안전하게 로그를 동기화한 후 승격할 수 있다.

### 승격 프로세스

```
소스 경로: server/etcdserver/server.go (1504행)

func (s *EtcdServer) promoteMember(ctx context.Context, id uint64) ([]*membership.Member, error) {
    // 1. Learner 상태 확인
    if err := s.mayPromoteMember(types.ID(id)); err != nil {
        return nil, err
    }

    // 2. 승격 ConfChange 컨텍스트 구성
    promoteChangeContext := membership.ConfigChangeContext{
        Member:    membership.Member{ID: types.ID(id)},
        IsPromote: true,
    }

    // 3. ConfChangeAddNode (IsPromote=true)로 제안
    cc := raftpb.ConfChange{
        Type:    raftpb.ConfChangeAddNode,
        NodeID:  id,
        Context: b,
    }
    return s.configure(ctx, cc)
}
```

### RaftCluster.PromoteMember() 적용

```
소스 경로: server/etcdserver/api/membership/cluster.go (497행)

func (c *RaftCluster) PromoteMember(id types.ID, shouldApplyV3 ShouldApplyV3) {
    c.Lock()
    defer c.Unlock()

    if id == c.localID {
        isLearner.Set(0)   // 메트릭 업데이트: Learner가 아님
    }

    if shouldApplyV3 {
        if m, ok := c.members[id]; ok {
            m.RaftAttributes.IsLearner = false   // Learner 해제
            c.updateMembershipMetric(id, true)
            c.be.MustSaveMemberToBackend(m)      // 백엔드 저장
        }
    }
}
```

---

## rafthttp Transport: 피어 통신

### Transport 구조체

```
소스 경로: server/etcdserver/api/rafthttp/transport.go

type Transport struct {
    Logger      *zap.Logger
    DialTimeout time.Duration
    ID          types.ID              // 로컬 노드 ID
    ClusterID   types.ID              // 클러스터 ID
    Raft        Raft                  // Raft 인터페이스
    Snapshotter *snap.Snapshotter
    // ...
    peers   map[types.ID]Peer         // 피어 연결 관리
    streamRt   http.RoundTripper      // Stream용 HTTP 전송
    pipelineRt http.RoundTripper      // Pipeline용 HTTP 전송
}
```

### Stream vs Pipeline

etcd의 rafthttp는 두 가지 통신 채널을 사용한다:

```
┌─────────────────────────────────────────────────────────────┐
│                   피어 통신 아키텍처                          │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌─────────────────────┐    ┌─────────────────────┐        │
│  │     Stream 채널      │    │    Pipeline 채널     │        │
│  ├─────────────────────┤    ├─────────────────────┤        │
│  │ 방식: 장기 HTTP 연결  │    │ 방식: 개별 HTTP 요청  │        │
│  │ 용도: 작은 메시지      │    │ 용도: 큰 메시지       │        │
│  │ - MsgApp             │    │ - 대량 로그 전송       │        │
│  │ - MsgHeartbeat       │    │ - 스냅샷 전송          │        │
│  │ - MsgVote            │    │                       │        │
│  │ 특성: 저지연, 고효율   │    │ 특성: 높은 처리량      │        │
│  │ 연결: 지속적           │    │ 연결: 요청당 생성      │        │
│  └─────────────────────┘    └─────────────────────┘        │
│                                                             │
│  Handler 매핑:                                               │
│  /raft         → pipelineHandler (대용량 메시지)              │
│  /raft/stream/ → streamHandler (스트리밍 메시지)              │
│  /raft/snapshot → snapshotHandler (스냅샷 전송)               │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### peer 구조체

```
소스 경로: server/etcdserver/api/rafthttp/peer.go

type peer struct {
    lg      *zap.Logger
    localID types.ID
    id      types.ID              // 원격 피어 ID

    r Raft
    status *peerStatus

    picker *urlPicker             // URL 선택기

    msgAppV2Writer *streamWriter  // MsgApp v2 스트림 쓰기
    writer         *streamWriter  // 일반 스트림 쓰기
    pipeline       *pipeline      // 파이프라인 채널
    snapSender     *snapshotSender

    msgAppV2Reader *streamReader  // MsgApp v2 스트림 읽기
    msgAppReader   *streamReader  // 일반 스트림 읽기

    recvc chan raftpb.Message      // 수신 메시지 채널
}
```

각 피어는 여러 개의 streamWriter/streamReader와 pipeline을 가지며, 메시지 종류에 따라 적절한 채널로 라우팅된다.

---

## 클러스터 토큰과 부트스트랩

### 초기 부트스트랩 과정

```
NewClusterFromURLsMap() 흐름:

1. token + URLsMap으로 RaftCluster 생성
2. 각 멤버에 대해:
   a. NewMember(name, urls, token, nil)  → 시간 없이 ID 생성
   b. 멤버 ID 중복 확인
   c. Raft.None(0) ID 확인
   d. members 맵에 추가
3. genID()로 클러스터 ID 생성
```

```
소스 경로: server/etcdserver/api/membership/cluster.go

func NewClusterFromURLsMap(lg *zap.Logger, token string, urlsmap types.URLsMap, ...) (*RaftCluster, error) {
    c := NewCluster(lg, opts...)
    for name, urls := range urlsmap {
        m := NewMember(name, urls, token, nil)  // now=nil: 부트스트랩 시
        if _, ok := c.members[m.ID]; ok {
            return nil, fmt.Errorf("member exists with identical ID %v", m)
        }
        if uint64(m.ID) == raft.None {
            return nil, fmt.Errorf("cannot use %x as member id", raft.None)
        }
        c.members[m.ID] = m
    }
    c.genID()
    return c, nil
}
```

**클러스터 토큰의 역할:**

- `--initial-cluster-token` 옵션으로 지정
- 멤버 ID 생성에 포함되어, 같은 URL이라도 다른 토큰을 사용하면 다른 ID 생성
- 여러 etcd 클러스터가 같은 네트워크에 있을 때 혼입 방지

### ValidateClusterAndAssignIDs

기존 클러스터에 조인할 때, 로컬 설정과 기존 클러스터를 비교하여 ID를 할당한다.

```
소스 경로: server/etcdserver/api/membership/cluster.go (761행)

func ValidateClusterAndAssignIDs(lg *zap.Logger, local *RaftCluster, existing *RaftCluster) error {
    ems := existing.Members()
    lms := local.Members()

    // 멤버 수 일치 확인
    if len(ems) != len(lms) {
        return fmt.Errorf("member count is unequal")
    }

    // PeerURL 매칭으로 ID 할당
    for i := range ems {
        for j := range lms {
            if ok, _ := netutil.URLStringsEqual(ctx, lg, ems[i].PeerURLs, lms[j].PeerURLs); ok {
                lms[j].ID = ems[i].ID  // 기존 ID 사용
                break
            }
        }
    }
    return nil
}
```

---

## 클러스터 버전 관리와 다운그레이드

### 클러스터 버전이란

클러스터 버전은 클러스터 전체의 최소 호환 버전이다. 모든 멤버가 지원하는 가장 낮은 major.minor 버전으로 설정된다.

```
소스 경로: server/etcdserver/api/membership/cluster.go (595행)

func (c *RaftCluster) SetVersion(ver *semver.Version, onSet func(*zap.Logger, *semver.Version), shouldApplyV3 ShouldApplyV3) {
    c.Lock()
    defer c.Unlock()

    oldVer := c.version
    c.version = ver

    // 다운그레이드 감지
    serverversion.MustDetectDowngrade(c.lg, sv, c.version)

    // 백엔드 저장
    if shouldApplyV3 {
        c.be.MustSaveClusterVersionToBackend(ver)
    }

    // 메트릭 업데이트
    if oldVer != nil {
        ClusterVersionMetrics.With(prometheus.Labels{"cluster_version": oldVer.String()}).Set(0)
    }
    ClusterVersionMetrics.With(prometheus.Labels{"cluster_version": ver.String()}).Set(1)

    // 버전 변경 알림
    if c.versionChanged != nil {
        c.versionChanged.Notify()
    }
    onSet(c.lg, ver)
}
```

### 다운그레이드 관리

```
소스 경로: server/etcdserver/api/membership/cluster.go

func (c *RaftCluster) SetDowngradeInfo(d *serverversion.DowngradeInfo, shouldApplyV3 ShouldApplyV3) {
    c.Lock()
    defer c.Unlock()

    if shouldApplyV3 {
        c.be.MustSaveDowngradeToBackend(d)
    }
    c.downgradeInfo = d
}
```

`DowngradeInfo`는 클러스터 전체의 다운그레이드 상태를 추적한다:
- `Enabled`: 다운그레이드 활성 여부
- `TargetVersion`: 다운그레이드 대상 버전

다운그레이드가 활성화되면 복구 시 버전 호환성 검사에서 이를 고려한다.

---

## RaftCluster 적용 메서드

### AddMember 적용

```
소스 경로: server/etcdserver/api/membership/cluster.go (393행)

func (c *RaftCluster) AddMember(m *Member, shouldApplyV3 ShouldApplyV3) {
    c.Lock()
    defer c.Unlock()

    if m.ID == c.localID {
        setIsLearnerMetric(m)   // 로컬 노드면 Learner 메트릭 설정
    }

    if shouldApplyV3 {
        c.be.MustSaveMemberToBackend(m)     // bbolt에 저장
        c.members[m.ID] = m                  // 메모리 맵 업데이트
        c.updateMembershipMetric(m.ID, true) // 메트릭 업데이트
    }
}
```

### RemoveMember 적용

```
소스 경로: server/etcdserver/api/membership/cluster.go (428행)

func (c *RaftCluster) RemoveMember(id types.ID, shouldApplyV3 ShouldApplyV3) {
    c.Lock()
    defer c.Unlock()

    if shouldApplyV3 {
        c.be.MustDeleteMemberFromBackend(id)  // bbolt에서 삭제
        delete(c.members, id)                  // 메모리 맵에서 제거
        c.removed[id] = true                   // 제거 목록에 추가
        c.updateMembershipMetric(id, false)
    }
}
```

### shouldApplyV3 플래그

모든 적용 메서드에는 `shouldApplyV3 ShouldApplyV3` 파라미터가 있다:

| 값 | 의미 | 사용 시점 |
|----|------|----------|
| `ApplyBoth` (true) | 백엔드에 실제 저장 | 새로운 ConfChange 적용 |
| `ApplyV2storeOnly` (false) | 저장 없이 로깅만 | 이미 백엔드에 있는 데이터 복구 |

이 메커니즘은 스냅샷 복원 시 이미 저장된 데이터를 중복 저장하지 않기 위해 필요하다.

---

## 복구 (Recover)

서버 재시작 시 백엔드에서 멤버십 상태를 복구한다.

```
소스 경로: server/etcdserver/api/membership/cluster.go (262행)

func (c *RaftCluster) Recover(onSet func(*zap.Logger, *semver.Version)) {
    c.Lock()
    defer c.Unlock()

    // 1. 백엔드에서 멤버, 제거 목록, 버전, 다운그레이드 정보 로드
    c.UnsafeLoad()

    // 2. 메트릭 재구성
    c.buildMembershipMetric()

    // 3. 다운그레이드 감지
    sv := semver.Must(semver.NewVersion(version.Version))
    if c.downgradeInfo != nil && c.downgradeInfo.Enabled {
        // 다운그레이드 진행 중 로깅
    }
    serverversion.MustDetectDowngrade(c.lg, sv, c.version)
    onSet(c.lg, c.version)
}

func (c *RaftCluster) UnsafeLoad() {
    c.version = c.be.ClusterVersionFromBackend()
    c.members, c.removed = c.be.MustReadMembersFromBackend()
    c.downgradeInfo = c.be.DowngradeInfoFromBackend()
}
```

---

## Learner 수 제한 (ValidateMaxLearnerConfig)

```
소스 경로: server/etcdserver/api/membership/cluster.go (883행)

func ValidateMaxLearnerConfig(maxLearners int, members []*Member, scaleUpLearners bool) error {
    numLearners := 0
    for _, m := range members {
        if m.IsLearner {
            numLearners++
        }
    }
    if scaleUpLearners {
        numLearners++   // 추가될 Learner 포함
    }
    if numLearners > maxLearners {
        return ErrTooManyLearners
    }
    return nil
}
```

이 검증은 `ValidateConfigurationChange`에서 `ConfChangeAddLearnerNode` 처리 시 호출된다.

---

## 메트릭 관리

RaftCluster는 멤버십 변경에 따른 메트릭을 관리한다:

```
func (c *RaftCluster) buildMembershipMetric() {
    if c.localID == 0 { return }
    for p := range c.members {
        knownPeers.WithLabelValues(c.localID.String(), p.String()).Set(1)
    }
    for p := range c.removed {
        knownPeers.WithLabelValues(c.localID.String(), p.String()).Set(0)
    }
}

func (c *RaftCluster) updateMembershipMetric(peer types.ID, known bool) {
    if c.localID == 0 { return }
    v := float64(0)
    if known { v = 1 }
    knownPeers.WithLabelValues(c.localID.String(), peer.String()).Set(v)
}
```

| 메트릭 | 설명 |
|--------|------|
| `knownPeers` | 알려진 피어 상태 (1=활성, 0=제거) |
| `isLearner` | 로컬 노드의 Learner 상태 (0 또는 1) |
| `ClusterVersionMetrics` | 클러스터 버전 |

---

## IsStarted() 판별

```
소스 경로: server/etcdserver/api/membership/member.go

func (m *Member) IsStarted() bool {
    return len(m.Name) != 0
}
```

멤버의 `Name`이 빈 문자열이 아니면 "시작된" 것으로 간주한다. 이 판별은:
- 초기 부트스트랩 시 `Name`은 빈 상태로 시작
- 노드가 실제로 시작되면 `publish` 과정에서 자신의 `Name`과 `ClientURLs`를 설정
- 쿼럼 검사에서 "시작된 멤버 수"를 계산할 때 사용

---

## 전체 아키텍처 요약

```
┌─────────────────────────────────────────────────────────────────────┐
│                     클러스터 멤버십 아키텍처                          │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  클라이언트 API                                                      │
│  ┌────────────┬────────────┬────────────┬────────────┐              │
│  │ AddMember  │RemoveMember│UpdateMember│PromoteMember│              │
│  └─────┬──────┴─────┬──────┴─────┬──────┴─────┬──────┘              │
│        │            │            │            │                      │
│        └────────────┴────────────┴────────────┘                      │
│                           │                                          │
│                    ┌──────▼──────┐                                    │
│                    │  configure() │  ← Raft ConfChange 제안           │
│                    │  + wait.Wait │  ← 결과 대기                      │
│                    └──────┬──────┘                                    │
│                           │                                          │
│                    ┌──────▼──────┐                                    │
│                    │    Raft     │  ← 합의                            │
│                    │  Consensus  │                                    │
│                    └──────┬──────┘                                    │
│                           │                                          │
│                    ┌──────▼──────┐                                    │
│                    │ Validate    │  ← ValidateConfigurationChange()   │
│                    │ + Apply     │  ← AddMember/RemoveMember/...      │
│                    └──────┬──────┘                                    │
│                           │                                          │
│                    ┌──────▼──────┐                                    │
│                    │  RaftCluster │  ← 멤버십 상태 관리                │
│                    │  + Backend   │  ← bbolt 영구 저장                │
│                    └─────────────┘                                    │
│                                                                      │
│  피어 통신: rafthttp.Transport                                        │
│  ┌─────────────┐  ┌──────────────┐  ┌─────────────────┐             │
│  │  Stream      │  │  Pipeline     │  │  Snapshot       │             │
│  │  (장기 연결)  │  │  (개별 요청)   │  │  (대용량 전송)   │             │
│  └─────────────┘  └──────────────┘  └─────────────────┘             │
│                                                                      │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 핵심 정리

| 주제 | 핵심 내용 |
|------|----------|
| 멤버 ID | SHA-1(peerURLs + clusterName [+ timestamp])의 상위 8바이트 |
| 클러스터 ID | SHA-1(모든 멤버 ID)의 상위 8바이트 |
| ConfChange 경로 | API → configure() → Raft → Validate → Apply → RaftCluster |
| Learner | 투표 불참 노드, 쿼럼 영향 없이 로그 동기화 후 승격 |
| 승격 | 리더만 판단 가능 (로그 복제 진행률), 팔로워는 HTTP 포워딩 |
| 쿼럼 보호 | 추가/제거 전 쿼럼 유지 가능 여부 검사 (StrictReconfigCheck) |
| 통신 채널 | Stream (장기 연결, 소형 메시지) + Pipeline (개별 요청, 대형 메시지) |
| 제거된 ID | removed 맵에 영구 기록, 재사용 불가 |
| 버전 관리 | 클러스터 최소 호환 버전 추적, 다운그레이드 지원 |
| 복구 | 백엔드(bbolt)에서 멤버/제거/버전/다운그레이드 정보 복원 |
