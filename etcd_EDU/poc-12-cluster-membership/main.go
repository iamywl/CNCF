// poc-12-cluster-membership: etcd 클러스터 멤버십 관리 시뮬레이션
//
// etcd는 Raft 클러스터의 멤버를 동적으로 추가/제거/승격할 수 있다.
// 실제 구현 (server/etcdserver/api/membership/):
// - Member: ID(sha1 해시 기반), name, peerURLs, clientURLs, isLearner
// - RaftCluster: members map, removed set, AddMember/RemoveMember/PromoteMember
// - computeMemberID(): peerURLs + clusterName + timestamp의 SHA1 해시
// - ConfigChangeContext: ConfChange 시 전달되는 멤버 정보 + IsPromote 플래그
// - Learner: 투표권 없이 로그만 복제받는 멤버 → PromoteMember로 Voter 승격
//
// 사용법: go run main.go

package main

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ===== 에러 정의 =====

var (
	ErrMemberExists    = errors.New("membership: 동일한 ID의 멤버가 이미 존재함")
	ErrMemberNotFound  = errors.New("membership: 멤버를 찾을 수 없음")
	ErrMemberRemoved   = errors.New("membership: 이미 제거된 멤버 ID는 재사용 불가")
	ErrNotLearner      = errors.New("membership: 멤버가 Learner가 아님 (승격 불가)")
	ErrLearnerNotReady = errors.New("membership: Learner가 아직 준비되지 않음")
	ErrTooManyLearners = errors.New("membership: 최대 Learner 수 초과")
)

// ===== MemberID =====
// etcd에서 멤버 ID는 uint64이며, 16진수 문자열로 표시한다.

type MemberID uint64

func (id MemberID) String() string {
	return fmt.Sprintf("%x", uint64(id))
}

// ===== Member =====
// server/etcdserver/api/membership/member.go의 Member 구조체 재현

type RaftAttributes struct {
	PeerURLs  []string `json:"peerURLs"`
	IsLearner bool     `json:"isLearner,omitempty"`
}

type Attributes struct {
	Name       string   `json:"name,omitempty"`
	ClientURLs []string `json:"clientURLs,omitempty"`
}

type Member struct {
	ID MemberID `json:"id"`
	RaftAttributes
	Attributes
}

// computeMemberID는 peerURLs + clusterName + timestamp의 SHA1 해시로 멤버 ID를 생성한다.
// server/etcdserver/api/membership/member.go의 computeMemberID() 재현
func computeMemberID(peerURLs []string, clusterName string, now *time.Time) MemberID {
	// peerURLs를 정렬하여 일관된 해시 생성
	sorted := make([]string, len(peerURLs))
	copy(sorted, peerURLs)
	sort.Strings(sorted)

	b := []byte(strings.Join(sorted, ""))
	b = append(b, []byte(clusterName)...)
	if now != nil {
		b = append(b, []byte(fmt.Sprintf("%d", now.Unix()))...)
	}

	hash := sha1.Sum(b)
	return MemberID(binary.BigEndian.Uint64(hash[:8]))
}

// NewMember는 새 Voter 멤버를 생성한다.
func NewMember(name string, peerURLs []string, clusterName string, now *time.Time) *Member {
	id := computeMemberID(peerURLs, clusterName, now)
	return &Member{
		ID: id,
		RaftAttributes: RaftAttributes{
			PeerURLs:  peerURLs,
			IsLearner: false,
		},
		Attributes: Attributes{Name: name},
	}
}

// NewMemberAsLearner는 새 Learner 멤버를 생성한다.
// server/etcdserver/api/membership/member.go의 NewMemberAsLearner() 재현
func NewMemberAsLearner(name string, peerURLs []string, clusterName string, now *time.Time) *Member {
	id := computeMemberID(peerURLs, clusterName, now)
	return &Member{
		ID: id,
		RaftAttributes: RaftAttributes{
			PeerURLs:  peerURLs,
			IsLearner: true,
		},
		Attributes: Attributes{Name: name},
	}
}

func (m *Member) IsStarted() bool {
	return len(m.Name) != 0
}

func (m *Member) Clone() *Member {
	mm := &Member{
		ID: m.ID,
		RaftAttributes: RaftAttributes{
			IsLearner: m.IsLearner,
		},
		Attributes: Attributes{
			Name: m.Name,
		},
	}
	if m.PeerURLs != nil {
		mm.PeerURLs = make([]string, len(m.PeerURLs))
		copy(mm.PeerURLs, m.PeerURLs)
	}
	if m.ClientURLs != nil {
		mm.ClientURLs = make([]string, len(m.ClientURLs))
		copy(mm.ClientURLs, m.ClientURLs)
	}
	return mm
}

// ===== ConfChangeType =====
// Raft ConfChange 타입. raftpb/raft.pb.go 참조

type ConfChangeType int

const (
	ConfChangeAddNode    ConfChangeType = 0
	ConfChangeRemoveNode ConfChangeType = 1
	ConfChangeAddLearner ConfChangeType = 3
)

func (t ConfChangeType) String() string {
	switch t {
	case ConfChangeAddNode:
		return "AddNode"
	case ConfChangeRemoveNode:
		return "RemoveNode"
	case ConfChangeAddLearner:
		return "AddLearner"
	default:
		return "Unknown"
	}
}

// ConfChange는 Raft 설정 변경 요청이다.
type ConfChange struct {
	Type   ConfChangeType
	NodeID MemberID
}

// ConfigChangeContext는 ConfChange와 함께 전달되는 컨텍스트이다.
// server/etcdserver/api/membership/cluster.go의 ConfigChangeContext 재현
type ConfigChangeContext struct {
	Member
	IsPromote bool `json:"isPromote"`
}

// ===== RaftCluster =====
// server/etcdserver/api/membership/cluster.go의 RaftCluster 재현

type RaftCluster struct {
	mu          sync.RWMutex
	clusterID   MemberID
	localID     MemberID
	members     map[MemberID]*Member
	removed     map[MemberID]bool // 제거된 멤버 ID (재사용 불가)
	maxLearners int
	confChanges []ConfChange // ConfChange 히스토리
}

func NewCluster(clusterName string, maxLearners int) *RaftCluster {
	// 클러스터 ID도 해시 기반으로 생성
	hash := sha1.Sum([]byte(clusterName))
	cid := MemberID(binary.BigEndian.Uint64(hash[:8]))

	return &RaftCluster{
		clusterID:   cid,
		members:     make(map[MemberID]*Member),
		removed:     make(map[MemberID]bool),
		maxLearners: maxLearners,
	}
}

// AddMember는 클러스터에 멤버를 추가한다.
// server/etcdserver/api/membership/cluster.go의 AddMember() 재현
func (c *RaftCluster) AddMember(m *Member) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.members[m.ID]; ok {
		return ErrMemberExists
	}
	if c.removed[m.ID] {
		return ErrMemberRemoved
	}

	// Learner 수 제한 검사
	if m.IsLearner {
		learnerCount := 0
		for _, existing := range c.members {
			if existing.IsLearner {
				learnerCount++
			}
		}
		if learnerCount >= c.maxLearners {
			return ErrTooManyLearners
		}
	}

	c.members[m.ID] = m

	// ConfChange 기록
	changeType := ConfChangeAddNode
	if m.IsLearner {
		changeType = ConfChangeAddLearner
	}
	c.confChanges = append(c.confChanges, ConfChange{
		Type:   changeType,
		NodeID: m.ID,
	})

	role := "Voter"
	if m.IsLearner {
		role = "Learner"
	}
	fmt.Printf("  [클러스터 %s] 멤버 추가: %s (ID=%s, %s)\n",
		c.clusterID, m.Name, m.ID, role)

	return nil
}

// RemoveMember는 클러스터에서 멤버를 제거한다.
// server/etcdserver/api/membership/cluster.go의 RemoveMember() 재현
func (c *RaftCluster) RemoveMember(id MemberID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	m, ok := c.members[id]
	if !ok {
		return ErrMemberNotFound
	}

	name := m.Name
	delete(c.members, id)
	c.removed[id] = true

	c.confChanges = append(c.confChanges, ConfChange{
		Type:   ConfChangeRemoveNode,
		NodeID: id,
	})

	fmt.Printf("  [클러스터 %s] 멤버 제거: %s (ID=%s)\n",
		c.clusterID, name, id)

	return nil
}

// PromoteMember는 Learner를 Voter로 승격한다.
// server/etcdserver/api/membership/cluster.go의 PromoteMember() 재현
// etcd에서는 AddNode ConfChange를 사용하되 IsPromote=true 컨텍스트를 전달한다.
func (c *RaftCluster) PromoteMember(id MemberID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	m, ok := c.members[id]
	if !ok {
		return ErrMemberNotFound
	}

	if !m.IsLearner {
		return ErrNotLearner
	}

	m.IsLearner = false

	// 승격은 ConfChangeAddNode + IsPromote=true로 처리된다
	c.confChanges = append(c.confChanges, ConfChange{
		Type:   ConfChangeAddNode,
		NodeID: id,
	})

	fmt.Printf("  [클러스터 %s] 멤버 승격: %s (ID=%s, Learner → Voter)\n",
		c.clusterID, m.Name, id)

	return nil
}

// Members는 현재 멤버 목록을 반환한다.
func (c *RaftCluster) Members() []*Member {
	c.mu.RLock()
	defer c.mu.RUnlock()

	members := make([]*Member, 0, len(c.members))
	for _, m := range c.members {
		members = append(members, m.Clone())
	}

	// ID 순으로 정렬
	sort.Slice(members, func(i, j int) bool {
		return members[i].ID < members[j].ID
	})
	return members
}

// VoterCount는 투표 가능한 멤버 수를 반환한다.
func (c *RaftCluster) VoterCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	count := 0
	for _, m := range c.members {
		if !m.IsLearner {
			count++
		}
	}
	return count
}

// LearnerCount는 Learner 수를 반환한다.
func (c *RaftCluster) LearnerCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	count := 0
	for _, m := range c.members {
		if m.IsLearner {
			count++
		}
	}
	return count
}

// Quorum은 과반수 합의에 필요한 최소 멤버 수를 반환한다.
// Learner는 quorum 계산에 포함되지 않는다.
func (c *RaftCluster) Quorum() int {
	return c.VoterCount()/2 + 1
}

// IsReadyToPromoteMember는 Learner가 승격 가능한 상태인지 확인한다.
// 실제 etcd에서는 Learner가 Leader와 충분히 동기화되었는지 확인한다.
func (c *RaftCluster) IsReadyToPromoteMember(id MemberID) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	m, ok := c.members[id]
	if !ok {
		return false
	}
	if !m.IsLearner {
		return false
	}
	// 실제로는 Learner의 로그 인덱스가 Leader와 충분히 가까운지 확인
	// 이 PoC에서는 Started 상태이면 준비된 것으로 간주
	return m.IsStarted()
}

// ConfChangeHistory는 ConfChange 히스토리를 반환한다.
func (c *RaftCluster) ConfChangeHistory() []ConfChange {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]ConfChange, len(c.confChanges))
	copy(result, c.confChanges)
	return result
}

// printMembers는 현재 멤버 목록을 출력한다.
func printMembers(cluster *RaftCluster) {
	members := cluster.Members()
	fmt.Printf("\n  현재 클러스터 멤버 (%d명, Voter=%d, Learner=%d, Quorum=%d):\n",
		len(members), cluster.VoterCount(), cluster.LearnerCount(), cluster.Quorum())
	fmt.Println("  ┌────────────────┬──────────┬─────────────────────────┬───────────┐")
	fmt.Println("  │ ID             │ 이름     │ PeerURLs                │ 역할      │")
	fmt.Println("  ├────────────────┼──────────┼─────────────────────────┼───────────┤")
	for _, m := range members {
		role := "Voter"
		if m.IsLearner {
			role = "Learner"
		}
		peerURLs := strings.Join(m.PeerURLs, ",")
		fmt.Printf("  │ %-14s │ %-8s │ %-23s │ %-9s │\n",
			m.ID, m.Name, peerURLs, role)
	}
	fmt.Println("  └────────────────┴──────────┴─────────────────────────┴───────────┘")
}

// ===== 메인 =====

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║  etcd 클러스터 멤버십 PoC                               ║")
	fmt.Println("║  동적 멤버 추가/제거/승격 시뮬레이션                    ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")

	clusterName := "etcd-cluster-poc"
	cluster := NewCluster(clusterName, 2) // 최대 Learner 2개

	// ========================================
	// 1. 초기 3노드 클러스터 구성
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("1단계: 초기 3노드 클러스터 구성")
	fmt.Println(strings.Repeat("─", 55))

	now := time.Now()
	members := []*Member{
		NewMember("etcd-1", []string{"http://10.0.0.1:2380"}, clusterName, &now),
		NewMember("etcd-2", []string{"http://10.0.0.2:2380"}, clusterName, &now),
		NewMember("etcd-3", []string{"http://10.0.0.3:2380"}, clusterName, &now),
	}

	// ClientURLs 설정
	members[0].ClientURLs = []string{"http://10.0.0.1:2379"}
	members[1].ClientURLs = []string{"http://10.0.0.2:2379"}
	members[2].ClientURLs = []string{"http://10.0.0.3:2379"}

	for _, m := range members {
		if err := cluster.AddMember(m); err != nil {
			fmt.Printf("  멤버 추가 실패: %v\n", err)
		}
	}

	cluster.localID = members[0].ID
	printMembers(cluster)

	// ========================================
	// 2. 멤버 ID 생성 원리
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("2단계: 멤버 ID 생성 원리")
	fmt.Println(strings.Repeat("─", 55))

	fmt.Println("  etcd는 SHA1(peerURLs + clusterName + timestamp)의 상위 8바이트로")
	fmt.Println("  멤버 ID를 생성한다.")
	fmt.Println()

	// 동일 입력이면 동일 ID
	t1 := time.Unix(1000000, 0)
	id1 := computeMemberID([]string{"http://10.0.0.1:2380"}, "test", &t1)
	id2 := computeMemberID([]string{"http://10.0.0.1:2380"}, "test", &t1)
	fmt.Printf("  동일 입력 → 동일 ID: %s == %s (%v)\n", id1, id2, id1 == id2)

	// 다른 peerURL이면 다른 ID
	id3 := computeMemberID([]string{"http://10.0.0.2:2380"}, "test", &t1)
	fmt.Printf("  다른 peerURL → 다른 ID: %s != %s (%v)\n", id1, id3, id1 != id3)

	// timestamp가 nil이면 결정적 ID (부트스트랩용)
	id4 := computeMemberID([]string{"http://10.0.0.1:2380"}, "test", nil)
	id5 := computeMemberID([]string{"http://10.0.0.1:2380"}, "test", nil)
	fmt.Printf("  timestamp=nil → 결정적 ID: %s == %s (%v)\n", id4, id5, id4 == id5)

	// ========================================
	// 3. Learner 멤버 추가
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("3단계: Learner 멤버 추가 (4번째 노드)")
	fmt.Println(strings.Repeat("─", 55))

	fmt.Println("  Learner는 투표권 없이 로그만 복제받는 멤버이다.")
	fmt.Println("  → 데이터 동기화 완료 후 Voter로 승격할 수 있다.")

	learner := NewMemberAsLearner("etcd-4", []string{"http://10.0.0.4:2380"}, clusterName, &now)
	learner.ClientURLs = []string{"http://10.0.0.4:2379"}

	if err := cluster.AddMember(learner); err != nil {
		fmt.Printf("  Learner 추가 실패: %v\n", err)
	}

	printMembers(cluster)
	fmt.Printf("\n  Quorum 변화 없음: Learner는 투표에 참여하지 않으므로 Quorum=%d\n",
		cluster.Quorum())

	// ========================================
	// 4. Learner → Voter 승격
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("4단계: Learner → Voter 승격")
	fmt.Println(strings.Repeat("─", 55))

	// 승격 가능 여부 확인
	ready := cluster.IsReadyToPromoteMember(learner.ID)
	fmt.Printf("  승격 준비 상태: %v (Name이 설정되어 Started 상태)\n", ready)

	if err := cluster.PromoteMember(learner.ID); err != nil {
		fmt.Printf("  승격 실패: %v\n", err)
	}

	printMembers(cluster)
	fmt.Printf("\n  Quorum 증가: Voter가 4명이므로 Quorum=%d\n", cluster.Quorum())

	// ========================================
	// 5. 멤버 제거
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("5단계: 멤버 제거 (etcd-3)")
	fmt.Println(strings.Repeat("─", 55))

	removedID := members[2].ID
	if err := cluster.RemoveMember(removedID); err != nil {
		fmt.Printf("  제거 실패: %v\n", err)
	}

	printMembers(cluster)
	fmt.Printf("\n  Quorum 변화: Voter가 3명이므로 Quorum=%d\n", cluster.Quorum())

	// ========================================
	// 6. 에러 케이스
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("6단계: 에러 케이스")
	fmt.Println(strings.Repeat("─", 55))

	// 이미 제거된 멤버 ID 재사용 시도
	reuseMember := &Member{
		ID:             removedID,
		RaftAttributes: RaftAttributes{PeerURLs: []string{"http://10.0.0.5:2380"}},
		Attributes:     Attributes{Name: "etcd-5"},
	}
	err := cluster.AddMember(reuseMember)
	fmt.Printf("  제거된 ID 재사용: %v\n", err)

	// 존재하지 않는 멤버 제거
	err = cluster.RemoveMember(MemberID(0xdeadbeef))
	fmt.Printf("  존재하지 않는 멤버 제거: %v\n", err)

	// 이미 Voter인 멤버 승격
	err = cluster.PromoteMember(members[0].ID)
	fmt.Printf("  Voter 재승격 시도: %v\n", err)

	// Learner 수 제한 테스트
	fmt.Println("\n  Learner 수 제한 테스트 (최대 2개):")
	l1 := NewMemberAsLearner("learner-1", []string{"http://10.0.0.11:2380"}, clusterName, &now)
	l2 := NewMemberAsLearner("learner-2", []string{"http://10.0.0.12:2380"}, clusterName, &now)
	l3 := NewMemberAsLearner("learner-3", []string{"http://10.0.0.13:2380"}, clusterName, &now)

	cluster.AddMember(l1)
	cluster.AddMember(l2)
	err = cluster.AddMember(l3)
	fmt.Printf("  3번째 Learner 추가: %v\n", err)

	// ========================================
	// 7. ConfChange 히스토리
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("7단계: ConfChange 히스토리")
	fmt.Println(strings.Repeat("─", 55))

	fmt.Println("  Raft ConfChange는 클러스터 멤버십 변경을 Raft 로그에 기록한다.")
	fmt.Println("  승격은 ConfChangeAddNode이지만 IsPromote=true 컨텍스트를 가진다.")
	fmt.Println()

	history := cluster.ConfChangeHistory()
	for i, cc := range history {
		fmt.Printf("  [%d] %s → NodeID=%s\n", i+1, cc.Type, cc.NodeID)
	}

	// ========================================
	// 8. 클러스터 멤버십 요약
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("8단계: 클러스터 멤버십 관리 요약")
	fmt.Println(strings.Repeat("─", 55))

	printMembers(cluster)

	fmt.Println("\n  멤버십 변경 흐름:")
	fmt.Println("  ┌─────────────────────────────────────────────────────┐")
	fmt.Println("  │ 1. 클라이언트 → API 서버: 멤버 추가/제거/승격 요청 │")
	fmt.Println("  │ 2. API 서버 → Raft: ConfChange 프로포즈            │")
	fmt.Println("  │ 3. Raft: 과반수 합의 후 커밋                       │")
	fmt.Println("  │ 4. 각 노드: ConfChange 적용 (members map 업데이트) │")
	fmt.Println("  │ 5. 제거된 ID는 removed set에 기록 (재사용 불가)    │")
	fmt.Println("  └─────────────────────────────────────────────────────┘")

	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("✓ 클러스터 멤버십 PoC 완료")
	fmt.Println("  - SHA1 해시 기반 멤버 ID 생성")
	fmt.Println("  - 동적 멤버 추가/제거 + removed set 관리")
	fmt.Println("  - Learner 추가 → Voter 승격 흐름")
	fmt.Println("  - Quorum 계산 (Learner 제외)")
	fmt.Println("  - ConfChange 히스토리 기록")
}
