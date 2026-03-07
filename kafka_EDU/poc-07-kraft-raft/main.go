package main

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// =============================================================================
// KRaft (Kafka Raft) Consensus Simulation
// Based on: KafkaRaftClient.java, LeaderState.java, CandidateState.java
//
// KRaft는 Kafka의 ZooKeeper 대체를 위한 Raft 합의 프로토콜 구현이다.
// 리더 선출은 표준 Raft를 따르지만, 로그 복제는 Kafka의 fetch 프로토콜을 사용한다.
//
// 이 PoC는 다음을 시뮬레이션한다:
// - 3노드 클러스터의 리더 선출 (VoteRequest/VoteResponse)
// - 에포크 기반 리더 추적
// - 랜덤화된 선거 타임아웃
// - 리더에서 팔로워로의 로그 복제
// - 하이 워터마크 계산 (투표자 오프셋의 중앙값)
// =============================================================================

// --- 노드 상태 (Raft State) ---
type NodeRole int

const (
	Follower  NodeRole = iota
	Candidate          // CandidateState.java
	Leader             // LeaderState.java
)

func (r NodeRole) String() string {
	switch r {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	}
	return "Unknown"
}

// --- 로그 엔트리 ---
type LogEntry struct {
	Offset int64
	Epoch  int32
	Data   string
}

// --- Raft 메시지 ---
// VoteRequest: CandidateState가 선거 시 전송
// KafkaRaftClient.java의 handleVoteRequest()에서 처리
type VoteRequest struct {
	CandidateID    int
	CandidateEpoch int32
	LastLogOffset  int64
	LastLogEpoch   int32
}

type VoteResponse struct {
	VoterID     int
	Epoch       int32
	VoteGranted bool
}

// BeginQuorumEpochRequest: 리더가 자신의 리더십을 알림
type BeginQuorumEpoch struct {
	LeaderID    int
	LeaderEpoch int32
}

// FetchRequest/Response: 팔로워가 리더에게 로그를 요청
type FetchRequest struct {
	FollowerID  int
	FetchOffset int64
	Epoch       int32
}

type FetchResponse struct {
	LeaderID    int
	Epoch       int32
	HighWatermark int64
	Entries     []LogEntry
}

// ReplicaState는 리더가 각 팔로워의 복제 상태를 추적하는 구조체이다.
// LeaderState.java의 ReplicaState와 대응된다.
type ReplicaState struct {
	NodeID    int
	EndOffset int64 // 이 노드가 복제한 마지막 오프셋
}

// --- RaftNode: 하나의 Raft 노드 ---
type RaftNode struct {
	mu sync.Mutex

	// 기본 상태
	id    int
	role  NodeRole
	epoch int32

	// 투표 상태 (CandidateState.java의 EpochElection)
	votedFor int // 이 에포크에서 투표한 대상 (-1 = 없음)

	// 로그
	log []LogEntry

	// 리더 상태 (LeaderState.java)
	replicaStates map[int]*ReplicaState // 팔로워 ID → 복제 상태
	highWatermark int64

	// 선거 타임아웃 (랜덤화)
	electionTimeoutMs int
	lastHeartbeat     time.Time

	// 클러스터 정보
	peers    []int // 다른 노드 ID 목록
	cluster  map[int]*RaftNode
	eventLog []string
}

func NewRaftNode(id int, peers []int) *RaftNode {
	return &RaftNode{
		id:                id,
		role:              Follower,
		epoch:             0,
		votedFor:          -1,
		log:               []LogEntry{},
		replicaStates:     make(map[int]*ReplicaState),
		highWatermark:     0,
		electionTimeoutMs: 150 + rand.Intn(150), // 150-300ms 랜덤
		lastHeartbeat:     time.Now(),
		peers:             peers,
		cluster:           make(map[int]*RaftNode),
	}
}

func (n *RaftNode) logEvent(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	n.eventLog = append(n.eventLog, msg)
	fmt.Printf("  [Node-%d/%s/epoch=%d] %s\n", n.id, n.role, n.epoch, msg)
}

func (n *RaftNode) lastLogOffset() int64 {
	if len(n.log) == 0 {
		return 0
	}
	return n.log[len(n.log)-1].Offset
}

func (n *RaftNode) lastLogEpoch() int32 {
	if len(n.log) == 0 {
		return 0
	}
	return n.log[len(n.log)-1].Epoch
}

// --- 리더 선출 ---

// StartElection은 후보자가 되어 투표를 요청한다.
// CandidateState.java: 생성 시 자기 자신에게 투표하고 (epochElection.recordVote(localId, true))
// VoteRequest를 다른 노드에 전송한다.
func (n *RaftNode) StartElection() {
	n.mu.Lock()
	n.epoch++
	n.role = Candidate
	n.votedFor = n.id
	currentEpoch := n.epoch
	lastOffset := n.lastLogOffset()
	lastEpoch := n.lastLogEpoch()
	n.logEvent("선거 시작: epoch=%d, lastLogOffset=%d, lastLogEpoch=%d, electionTimeout=%dms",
		currentEpoch, lastOffset, lastEpoch, n.electionTimeoutMs)
	n.mu.Unlock()

	// VoteRequest 전송
	voteReq := &VoteRequest{
		CandidateID:    n.id,
		CandidateEpoch: currentEpoch,
		LastLogOffset:  lastOffset,
		LastLogEpoch:   lastEpoch,
	}

	grantedVotes := 1 // 자기 자신의 투표
	totalVoters := len(n.peers) + 1
	majority := totalVoters/2 + 1

	for _, peerID := range n.peers {
		peer := n.cluster[peerID]
		resp := peer.HandleVoteRequest(voteReq)

		n.mu.Lock()
		if resp.VoteGranted {
			grantedVotes++
			n.logEvent("투표 수신: Node-%d가 투표 승인 (%d/%d)",
				resp.VoterID, grantedVotes, majority)
		} else {
			n.logEvent("투표 수신: Node-%d가 투표 거부 (epoch=%d)",
				resp.VoterID, resp.Epoch)
			// 더 높은 에포크를 발견하면 팔로워로 전환
			if resp.Epoch > n.epoch {
				n.epoch = resp.Epoch
				n.role = Follower
				n.votedFor = -1
				n.logEvent("더 높은 에포크 발견 → 팔로워로 전환")
				n.mu.Unlock()
				return
			}
		}
		n.mu.Unlock()
	}

	// 과반수 투표 획득 확인
	n.mu.Lock()
	defer n.mu.Unlock()

	if grantedVotes >= majority && n.role == Candidate {
		n.role = Leader
		n.logEvent("리더 당선! (득표: %d/%d, 과반수: %d)", grantedVotes, totalVoters, majority)

		// 리더 상태 초기화 (LeaderState.java 생성자)
		// 모든 투표자의 ReplicaState를 초기화한다
		n.replicaStates = make(map[int]*ReplicaState)
		n.replicaStates[n.id] = &ReplicaState{NodeID: n.id, EndOffset: n.lastLogOffset()}
		for _, peerID := range n.peers {
			n.replicaStates[peerID] = &ReplicaState{NodeID: peerID, EndOffset: 0}
		}

		// BeginQuorumEpoch 전송 (리더십 통보)
		for _, peerID := range n.peers {
			peer := n.cluster[peerID]
			peer.HandleBeginQuorumEpoch(&BeginQuorumEpoch{
				LeaderID:    n.id,
				LeaderEpoch: n.epoch,
			})
		}
	}
}

// HandleVoteRequest는 투표 요청을 처리한다.
// KafkaRaftClient.java의 handleVoteRequest()와 대응:
// - 에포크가 더 높으면 투표 승인
// - 이미 다른 후보에게 투표했으면 거부
// - 로그가 최신인지 확인 (Raft 안전성)
func (n *RaftNode) HandleVoteRequest(req *VoteRequest) *VoteResponse {
	n.mu.Lock()
	defer n.mu.Unlock()

	// 1. 후보의 에포크가 현재보다 낮으면 거부
	if req.CandidateEpoch < n.epoch {
		n.logEvent("투표 거부: Node-%d의 epoch(%d) < 내 epoch(%d)",
			req.CandidateID, req.CandidateEpoch, n.epoch)
		return &VoteResponse{VoterID: n.id, Epoch: n.epoch, VoteGranted: false}
	}

	// 2. 더 높은 에포크면 팔로워로 전환하고 투표 상태 초기화
	if req.CandidateEpoch > n.epoch {
		n.epoch = req.CandidateEpoch
		n.role = Follower
		n.votedFor = -1
	}

	// 3. 이미 다른 후보에게 투표했는지 확인
	if n.votedFor != -1 && n.votedFor != req.CandidateID {
		n.logEvent("투표 거부: 이미 Node-%d에 투표함", n.votedFor)
		return &VoteResponse{VoterID: n.id, Epoch: n.epoch, VoteGranted: false}
	}

	// 4. 후보의 로그가 최신인지 확인 (Raft Log Up-to-date Check)
	// 에포크가 더 높거나, 에포크가 같으면 오프셋이 더 크거나 같아야 함
	candidateLogOk := req.LastLogEpoch > n.lastLogEpoch() ||
		(req.LastLogEpoch == n.lastLogEpoch() && req.LastLogOffset >= n.lastLogOffset())

	if !candidateLogOk {
		n.logEvent("투표 거부: Node-%d의 로그가 뒤처짐 (후보: offset=%d/epoch=%d, 나: offset=%d/epoch=%d)",
			req.CandidateID, req.LastLogOffset, req.LastLogEpoch, n.lastLogOffset(), n.lastLogEpoch())
		return &VoteResponse{VoterID: n.id, Epoch: n.epoch, VoteGranted: false}
	}

	// 5. 투표 승인
	n.votedFor = req.CandidateID
	n.lastHeartbeat = time.Now()
	n.logEvent("투표 승인: Node-%d에 투표 (epoch=%d)", req.CandidateID, req.CandidateEpoch)
	return &VoteResponse{VoterID: n.id, Epoch: n.epoch, VoteGranted: true}
}

// HandleBeginQuorumEpoch는 새 리더의 에포크를 통지받는다.
func (n *RaftNode) HandleBeginQuorumEpoch(req *BeginQuorumEpoch) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if req.LeaderEpoch >= n.epoch {
		n.epoch = req.LeaderEpoch
		n.role = Follower
		n.votedFor = req.LeaderID
		n.lastHeartbeat = time.Now()
		n.logEvent("BeginQuorumEpoch 수신: Node-%d가 리더 (epoch=%d)", req.LeaderID, req.LeaderEpoch)
	}
}

// --- 로그 복제 ---

// AppendEntries는 리더가 로그에 엔트리를 추가한다.
func (n *RaftNode) AppendEntries(data string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.role != Leader {
		return
	}

	offset := n.lastLogOffset() + 1
	entry := LogEntry{Offset: offset, Epoch: n.epoch, Data: data}
	n.log = append(n.log, entry)
	n.replicaStates[n.id].EndOffset = offset
	n.logEvent("로그 추가: offset=%d, epoch=%d, data=%q", offset, n.epoch, data)
}

// ReplicateToFollowers는 리더가 팔로워에게 로그를 복제한다.
// Kafka Raft에서는 팔로워가 Fetch 요청을 보내지만,
// 이 시뮬레이션에서는 리더가 능동적으로 복제하여 단순화한다.
func (n *RaftNode) ReplicateToFollowers() {
	n.mu.Lock()
	if n.role != Leader {
		n.mu.Unlock()
		return
	}
	epoch := n.epoch
	n.mu.Unlock()

	for _, peerID := range n.peers {
		peer := n.cluster[peerID]

		// 팔로워의 현재 오프셋 확인
		n.mu.Lock()
		rs := n.replicaStates[peerID]
		fetchOffset := rs.EndOffset
		n.mu.Unlock()

		// FetchResponse 생성: fetchOffset 이후의 엔트리
		n.mu.Lock()
		var entries []LogEntry
		for _, entry := range n.log {
			if entry.Offset > fetchOffset {
				entries = append(entries, entry)
			}
		}
		hw := n.highWatermark
		n.mu.Unlock()

		if len(entries) == 0 {
			continue
		}

		resp := &FetchResponse{
			LeaderID:      n.id,
			Epoch:         epoch,
			HighWatermark: hw,
			Entries:       entries,
		}

		// 팔로워에 로그 전달
		newEndOffset := peer.HandleFetchResponse(resp)

		// 팔로워의 복제 상태 업데이트
		n.mu.Lock()
		n.replicaStates[peerID].EndOffset = newEndOffset
		n.logEvent("복제 완료: Node-%d의 endOffset=%d", peerID, newEndOffset)
		n.mu.Unlock()
	}
}

// HandleFetchResponse는 팔로워가 리더로부터 받은 로그를 적용한다.
func (n *RaftNode) HandleFetchResponse(resp *FetchResponse) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()

	// 에포크 확인
	if resp.Epoch < n.epoch {
		return n.lastLogOffset()
	}

	// 로그 엔트리 추가
	for _, entry := range resp.Entries {
		if entry.Offset > n.lastLogOffset() {
			n.log = append(n.log, entry)
		}
	}

	// 하이 워터마크 업데이트
	if resp.HighWatermark > n.highWatermark {
		n.highWatermark = resp.HighWatermark
	}

	n.logEvent("Fetch 수신: %d개 엔트리, endOffset=%d, hwm=%d",
		len(resp.Entries), n.lastLogOffset(), n.highWatermark)

	return n.lastLogOffset()
}

// --- 하이 워터마크 계산 ---
// LeaderState.java:727 - maybeUpdateHighWatermark()
//
// 투표자들의 endOffset을 내림차순으로 정렬한 뒤,
// indexOfHw = voterStates.size() / 2 위치의 값을 하이 워터마크로 사용한다.
// 이는 과반수가 복제한 최대 오프셋을 의미한다.
//
// 예: 3노드에서 오프셋이 [10, 7, 5]이면
//     indexOfHw = 3/2 = 1 → 하이 워터마크 = 7 (과반수인 2노드가 7 이상)
func (n *RaftNode) UpdateHighWatermark() {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.role != Leader {
		return
	}

	// 모든 투표자의 endOffset을 수집
	var offsets []int64
	for _, rs := range n.replicaStates {
		offsets = append(offsets, rs.EndOffset)
	}

	// 내림차순 정렬
	sort.Slice(offsets, func(i, j int) bool {
		return offsets[i] > offsets[j]
	})

	// indexOfHw = voterStates.size() / 2
	indexOfHw := len(offsets) / 2
	newHW := offsets[indexOfHw]

	if newHW > n.highWatermark {
		oldHW := n.highWatermark
		n.highWatermark = newHW
		n.logEvent("하이 워터마크 업데이트: %d → %d (투표자 오프셋: %v, indexOfHw=%d)",
			oldHW, newHW, offsets, indexOfHw)
	}
}

func main() {
	fmt.Println("=== KRaft (Kafka Raft) Consensus PoC ===")
	fmt.Println()
	fmt.Println("KRaft 구조:")
	fmt.Println("  - Raft 기반 합의 프로토콜 (ZooKeeper 대체)")
	fmt.Println("  - 리더 선출: VoteRequest/VoteResponse (표준 Raft)")
	fmt.Println("  - 로그 복제: Kafka Fetch 프로토콜 기반")
	fmt.Println("  - 하이 워터마크: 투표자 오프셋의 중앙값")
	fmt.Println()

	// --- 1. 3노드 클러스터 생성 ---
	fmt.Println("--- 1단계: 3노드 클러스터 초기화 ---")
	nodes := make(map[int]*RaftNode)
	nodeIDs := []int{0, 1, 2}

	for _, id := range nodeIDs {
		var peers []int
		for _, pid := range nodeIDs {
			if pid != id {
				peers = append(peers, pid)
			}
		}
		nodes[id] = NewRaftNode(id, peers)
		fmt.Printf("  Node-%d 생성: role=%s, electionTimeout=%dms\n",
			id, nodes[id].role, nodes[id].electionTimeoutMs)
	}

	// 클러스터 참조 설정
	for _, node := range nodes {
		node.cluster = nodes
	}
	fmt.Println()

	// --- 2. 리더 선출 ---
	fmt.Println("--- 2단계: 리더 선출 (Node-0이 후보가 됨) ---")
	fmt.Println()

	// Node-0이 선거 타임아웃이 되어 후보가 됨
	nodes[0].StartElection()
	fmt.Println()

	// 리더 확인
	var leaderID int = -1
	for id, node := range nodes {
		if node.role == Leader {
			leaderID = id
			break
		}
	}
	fmt.Printf("  현재 리더: Node-%d, epoch=%d\n", leaderID, nodes[leaderID].epoch)
	fmt.Println()

	// --- 3. 로그 복제 ---
	fmt.Println("--- 3단계: 로그 복제 ---")
	fmt.Println()

	leader := nodes[leaderID]

	// 리더에 로그 추가
	testData := []string{
		"topic-create:orders",
		"partition-assign:0",
		"topic-create:events",
		"config-change:retention.ms=86400000",
		"partition-assign:1",
	}

	for _, data := range testData {
		leader.AppendEntries(data)
	}
	fmt.Println()

	// 1차 복제: 일부만 복제 (네트워크 지연 시뮬레이션)
	fmt.Println("  --- 1차 복제 (모든 팔로워에 전체 복제) ---")
	leader.ReplicateToFollowers()
	fmt.Println()

	// 하이 워터마크 계산
	fmt.Println("  --- 하이 워터마크 계산 ---")
	leader.UpdateHighWatermark()
	fmt.Println()

	// --- 4. 클러스터 상태 출력 ---
	fmt.Println("--- 4단계: 클러스터 상태 ---")
	for _, id := range nodeIDs {
		node := nodes[id]
		node.mu.Lock()
		fmt.Printf("  Node-%d: role=%s, epoch=%d, logSize=%d, lastOffset=%d, hwm=%d\n",
			id, node.role, node.epoch, len(node.log), node.lastLogOffset(), node.highWatermark)
		if node.role == Leader {
			for _, rs := range node.replicaStates {
				fmt.Printf("    ReplicaState[Node-%d]: endOffset=%d\n", rs.NodeID, rs.EndOffset)
			}
		}
		node.mu.Unlock()
	}
	fmt.Println()

	// --- 5. 추가 로그 복제 및 하이 워터마크 진행 ---
	fmt.Println("--- 5단계: 추가 로그 추가 및 부분 복제 ---")
	fmt.Println()

	leader.AppendEntries("broker-registration:broker-3")
	leader.AppendEntries("topic-create:metrics")

	// 하나의 팔로워만 복제 (부분 복제 시뮬레이션)
	leader.mu.Lock()
	var entries []LogEntry
	for _, entry := range leader.log {
		if entry.Offset > 5 { // 새로 추가된 것만
			entries = append(entries, entry)
		}
	}
	leader.mu.Unlock()

	// Node-1만 복제
	peer1 := nodes[1]
	resp := &FetchResponse{
		LeaderID:      leaderID,
		Epoch:         leader.epoch,
		HighWatermark: leader.highWatermark,
		Entries:       entries,
	}
	newOffset := peer1.HandleFetchResponse(resp)
	leader.mu.Lock()
	leader.replicaStates[1].EndOffset = newOffset
	leader.logEvent("부분 복제: Node-1만 endOffset=%d로 업데이트", newOffset)
	leader.mu.Unlock()

	fmt.Println()
	fmt.Println("  --- 부분 복제 후 하이 워터마크 계산 ---")
	leader.UpdateHighWatermark()
	fmt.Println()

	// Node-2도 복제
	fmt.Println("  --- Node-2도 복제 후 하이 워터마크 계산 ---")
	peer2 := nodes[2]
	resp2 := &FetchResponse{
		LeaderID:      leaderID,
		Epoch:         leader.epoch,
		HighWatermark: leader.highWatermark,
		Entries:       entries,
	}
	newOffset2 := peer2.HandleFetchResponse(resp2)
	leader.mu.Lock()
	leader.replicaStates[2].EndOffset = newOffset2
	leader.mu.Unlock()

	leader.UpdateHighWatermark()
	fmt.Println()

	// --- 6. 최종 상태 ---
	fmt.Println("--- 6단계: 최종 클러스터 상태 ---")
	for _, id := range nodeIDs {
		node := nodes[id]
		node.mu.Lock()
		fmt.Printf("  Node-%d: role=%s, epoch=%d, logSize=%d, lastOffset=%d, hwm=%d\n",
			id, node.role, node.epoch, len(node.log), node.lastLogOffset(), node.highWatermark)
		node.mu.Unlock()
	}
	fmt.Println()

	// --- 7. 새 리더 선출 시뮬레이션 ---
	fmt.Println("--- 7단계: 리더 장애 → 새 리더 선출 ---")
	fmt.Println()

	// Node-0(현재 리더)가 장애 → Node-1이 선거 시작
	nodes[0].mu.Lock()
	nodes[0].role = Follower
	nodes[0].logEvent("장애 발생 시뮬레이션: 리더 역할 해제")
	nodes[0].mu.Unlock()

	// Node-1이 선거 타임아웃 후 후보가 됨
	nodes[1].StartElection()
	fmt.Println()

	// 최종 상태
	fmt.Println("--- 최종 클러스터 상태 ---")
	for _, id := range nodeIDs {
		node := nodes[id]
		node.mu.Lock()
		fmt.Printf("  Node-%d: role=%s, epoch=%d, logSize=%d, lastOffset=%d, hwm=%d\n",
			id, node.role, node.epoch, len(node.log), node.lastLogOffset(), node.highWatermark)
		node.mu.Unlock()
	}
	fmt.Println()

	// --- 아키텍처 요약 ---
	fmt.Println("=== 아키텍처 요약 ===")
	fmt.Println()
	fmt.Println("KRaft 리더 선출 (KafkaRaftClient.java, CandidateState.java):")
	fmt.Println("  1. 선거 타임아웃 만료 → epoch 증가, Candidate로 전환")
	fmt.Println("  2. 자기 자신에게 투표 (epochElection.recordVote(localId, true))")
	fmt.Println("  3. 다른 투표자에게 VoteRequest 전송")
	fmt.Println("  4. 과반수 승인 → Leader로 전환, BeginQuorumEpoch 전송")
	fmt.Println()
	fmt.Println("하이 워터마크 계산 (LeaderState.java:727):")
	fmt.Println("  1. 모든 투표자의 endOffset을 내림차순 정렬")
	fmt.Println("  2. indexOfHw = voterStates.size() / 2")
	fmt.Println("  3. 해당 위치의 오프셋이 새 하이 워터마크")
	fmt.Println("  4. epochStartOffset보다 커야 하이 워터마크로 설정 가능")
	fmt.Println()
	fmt.Println("Kafka Raft vs 표준 Raft:")
	fmt.Println("  - 리더 선출: 거의 동일한 Raft 프로토콜")
	fmt.Println("  - 로그 복제: 리더가 push하는 대신 팔로워가 Fetch로 pull")
	fmt.Println("  - 로그 조정: Kafka의 로그 재조정 프로토콜로 truncate")
}
