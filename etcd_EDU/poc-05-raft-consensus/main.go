// poc-05-raft-consensus: Raft 합의 알고리즘 시뮬레이션
//
// etcd의 Raft 구현(go.etcd.io/raft)을 기반으로
// 리더 선거, 로그 복제, 하트비트, 재선거를 시뮬레이션한다.
//
// 참조: etcd/server/etcdserver/raft.go
//       go.etcd.io/raft/v3/raft.go
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ========== 상수 및 타입 ==========

// NodeState는 Raft 노드의 상태를 나타낸다
type NodeState int

const (
	Follower  NodeState = iota // 팔로워: 리더의 명령을 따름
	Candidate                  // 후보자: 리더 선거에 참여 중
	Leader                     // 리더: 클러스터를 이끌고 로그를 복제
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

// MsgType은 Raft 메시지 유형
type MsgType int

const (
	MsgRequestVote     MsgType = iota // 투표 요청 (Candidate → All)
	MsgRequestVoteResp                // 투표 응답
	MsgAppendEntries                  // 로그 복제 / 하트비트 (Leader → Followers)
	MsgAppendEntriesResp              // 로그 복제 응답
)

// LogEntry는 Raft 로그 엔트리
// etcd의 raftpb.Entry에 해당
type LogEntry struct {
	Term    int    // 엔트리가 생성된 텀
	Index   int    // 로그 인덱스
	Command string // 명령 (예: "PUT key=value")
}

// Message는 Raft 노드 간 전달되는 메시지
// etcd의 raftpb.Message에 해당
type Message struct {
	Type     MsgType
	From     int // 송신 노드 ID
	To       int // 수신 노드 ID
	Term     int // 메시지 텀
	LogIndex int // 마지막 로그 인덱스 (AppendEntries용)
	LogTerm  int // 마지막 로그 텀
	Entries  []LogEntry
	Success  bool // 응답 성공 여부
	Commit   int  // 커밋 인덱스
}

// ========== Raft 노드 ==========

// RaftNode는 하나의 Raft 노드를 나타낸다
// etcd의 raft.raft 구조체를 단순화한 것
type RaftNode struct {
	mu sync.Mutex

	id    int       // 노드 ID
	state NodeState // 현재 상태
	term  int       // 현재 텀 (리더 선출 세대)

	votedFor  int // 이번 텀에서 투표한 대상 (-1: 미투표)
	voteCount int // 받은 투표 수

	log         []LogEntry // 로그 엔트리 목록
	commitIndex int        // 커밋된 마지막 인덱스
	lastApplied int        // 적용된 마지막 인덱스

	// 리더 전용: 각 팔로워의 다음 전송할 인덱스
	nextIndex  map[int]int
	matchIndex map[int]int

	// 선거 타이머 관련
	electionTimeout  time.Duration
	heartbeatTimeout time.Duration
	lastHeartbeat    time.Time

	// 메시지 전달용 채널
	recvCh chan Message
	peers  map[int]chan Message // 다른 노드의 수신 채널

	// 클러스터 정보
	clusterSize int

	// 종료 제어
	stopCh chan struct{}
	alive  bool // 노드 생존 여부 (장애 시뮬레이션)

	// 로그 출력
	appliedCmds []string
}

// NewRaftNode는 새 Raft 노드를 생성한다
func NewRaftNode(id int, clusterSize int) *RaftNode {
	return &RaftNode{
		id:               id,
		state:            Follower,
		term:             0,
		votedFor:         -1,
		log:              []LogEntry{{Term: 0, Index: 0, Command: ""}}, // 인덱스 0은 더미
		commitIndex:      0,
		lastApplied:      0,
		nextIndex:        make(map[int]int),
		matchIndex:       make(map[int]int),
		electionTimeout:  randomTimeout(150, 300),
		heartbeatTimeout: 50 * time.Millisecond,
		lastHeartbeat:    time.Now(),
		recvCh:           make(chan Message, 100),
		peers:            make(map[int]chan Message),
		clusterSize:      clusterSize,
		stopCh:           make(chan struct{}),
		alive:            true,
	}
}

func randomTimeout(minMs, maxMs int) time.Duration {
	return time.Duration(minMs+rand.Intn(maxMs-minMs)) * time.Millisecond
}

// lastLogIndex는 마지막 로그 인덱스를 반환
func (n *RaftNode) lastLogIndex() int {
	return len(n.log) - 1
}

// lastLogTerm은 마지막 로그 텀을 반환
func (n *RaftNode) lastLogTerm() int {
	return n.log[len(n.log)-1].Term
}

// ========== Raft 핵심 로직 ==========

// Run은 노드의 메인 루프를 실행한다
// etcd의 raft.Node.run()에 해당
func (n *RaftNode) Run() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-n.stopCh:
			return
		case msg := <-n.recvCh:
			n.handleMessage(msg)
		case <-ticker.C:
			n.tick()
		}
	}
}

// tick은 타이머를 확인하고 필요시 동작을 수행한다
// etcd의 raft.tickElection(), tickHeartbeat()에 해당
func (n *RaftNode) tick() {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.alive {
		return
	}

	elapsed := time.Since(n.lastHeartbeat)

	switch n.state {
	case Follower, Candidate:
		// 선거 타임아웃: 리더로부터 하트비트를 받지 못하면 선거 시작
		if elapsed > n.electionTimeout {
			n.startElection()
		}
	case Leader:
		// 하트비트 타임아웃: 주기적으로 하트비트 전송
		if elapsed > n.heartbeatTimeout {
			n.sendHeartbeats()
		}
	}
}

// startElection은 리더 선거를 시작한다
// etcd의 raft.campaign()에 해당 - 텀 증가, 자기 투표, RequestVote 전송
func (n *RaftNode) startElection() {
	n.term++
	n.state = Candidate
	n.votedFor = n.id
	n.voteCount = 1
	n.electionTimeout = randomTimeout(150, 300)
	n.lastHeartbeat = time.Now()

	fmt.Printf("  [노드 %d] 선거 시작! (텀=%d)\n", n.id, n.term)

	// 모든 피어에게 투표 요청
	for peerID, ch := range n.peers {
		msg := Message{
			Type:     MsgRequestVote,
			From:     n.id,
			To:       peerID,
			Term:     n.term,
			LogIndex: n.lastLogIndex(),
			LogTerm:  n.lastLogTerm(),
		}
		select {
		case ch <- msg:
		default:
		}
	}
}

// becomeLeader는 과반수 투표를 받아 리더가 된다
// etcd의 raft.becomeLeader()에 해당
func (n *RaftNode) becomeLeader() {
	n.state = Leader
	n.lastHeartbeat = time.Now()

	// nextIndex를 마지막 로그 + 1로 초기화
	for peerID := range n.peers {
		n.nextIndex[peerID] = n.lastLogIndex() + 1
		n.matchIndex[peerID] = 0
	}

	fmt.Printf("  [노드 %d] *** 리더 당선! *** (텀=%d)\n", n.id, n.term)

	// 즉시 하트비트 전송
	n.sendHeartbeats()
}

// becomeFollower는 팔로워로 전환한다
// etcd의 raft.becomeFollower()에 해당
func (n *RaftNode) becomeFollower(term int) {
	n.state = Follower
	n.term = term
	n.votedFor = -1
	n.voteCount = 0
	n.electionTimeout = randomTimeout(150, 300)
	n.lastHeartbeat = time.Now()
}

// sendHeartbeats는 모든 팔로워에게 하트비트를 전송한다
// etcd의 raft.bcastAppend()에 해당
func (n *RaftNode) sendHeartbeats() {
	n.lastHeartbeat = time.Now()

	for peerID, ch := range n.peers {
		nextIdx := n.nextIndex[peerID]
		prevLogIndex := nextIdx - 1
		prevLogTerm := 0
		if prevLogIndex > 0 && prevLogIndex < len(n.log) {
			prevLogTerm = n.log[prevLogIndex].Term
		}

		// 전송할 새 엔트리가 있으면 포함
		var entries []LogEntry
		if nextIdx <= n.lastLogIndex() {
			entries = make([]LogEntry, len(n.log[nextIdx:]))
			copy(entries, n.log[nextIdx:])
		}

		msg := Message{
			Type:     MsgAppendEntries,
			From:     n.id,
			To:       peerID,
			Term:     n.term,
			LogIndex: prevLogIndex,
			LogTerm:  prevLogTerm,
			Entries:  entries,
			Commit:   n.commitIndex,
		}
		select {
		case ch <- msg:
		default:
		}
	}
}

// handleMessage는 수신된 메시지를 처리한다
// etcd의 raft.Step()에 해당
func (n *RaftNode) handleMessage(msg Message) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.alive {
		return
	}

	// 더 높은 텀의 메시지를 받으면 팔로워로 전환
	// etcd Raft의 핵심 규칙: 높은 텀이 항상 우선
	if msg.Term > n.term {
		n.becomeFollower(msg.Term)
	}

	switch msg.Type {
	case MsgRequestVote:
		n.handleRequestVote(msg)
	case MsgRequestVoteResp:
		n.handleRequestVoteResp(msg)
	case MsgAppendEntries:
		n.handleAppendEntries(msg)
	case MsgAppendEntriesResp:
		n.handleAppendEntriesResp(msg)
	}
}

// handleRequestVote는 투표 요청을 처리한다
// etcd의 raft.Step() 내 MsgVote 처리에 해당
func (n *RaftNode) handleRequestVote(msg Message) {
	granted := false

	// 투표 조건: (1) 같은 텀 (2) 아직 투표 안 함 (3) 후보의 로그가 최소한 자신만큼 최신
	if msg.Term >= n.term &&
		(n.votedFor == -1 || n.votedFor == msg.From) &&
		(msg.LogTerm > n.lastLogTerm() ||
			(msg.LogTerm == n.lastLogTerm() && msg.LogIndex >= n.lastLogIndex())) {
		granted = true
		n.votedFor = msg.From
		n.lastHeartbeat = time.Now() // 투표 시 타이머 리셋
	}

	// 응답 전송
	if ch, ok := n.peers[msg.From]; ok {
		resp := Message{
			Type:    MsgRequestVoteResp,
			From:    n.id,
			To:      msg.From,
			Term:    n.term,
			Success: granted,
		}
		select {
		case ch <- resp:
		default:
		}
	}
}

// handleRequestVoteResp는 투표 응답을 처리한다
func (n *RaftNode) handleRequestVoteResp(msg Message) {
	if n.state != Candidate || msg.Term != n.term {
		return
	}

	if msg.Success {
		n.voteCount++
		// 과반수 투표를 받으면 리더로 전환
		if n.voteCount > n.clusterSize/2 {
			n.becomeLeader()
		}
	}
}

// handleAppendEntries는 로그 복제/하트비트 요청을 처리한다
// etcd의 raft.handleAppendEntries()에 해당
func (n *RaftNode) handleAppendEntries(msg Message) {
	n.lastHeartbeat = time.Now()
	n.state = Follower

	success := false

	// 로그 일관성 확인: prevLogIndex 위치의 텀이 일치해야 함
	if msg.LogIndex == 0 || (msg.LogIndex <= n.lastLogIndex() && n.log[msg.LogIndex].Term == msg.LogTerm) {
		success = true

		// 새 엔트리 추가
		if len(msg.Entries) > 0 {
			// 충돌하는 엔트리 제거 후 새 엔트리 추가
			insertPoint := msg.LogIndex + 1
			for i, entry := range msg.Entries {
				idx := insertPoint + i
				if idx < len(n.log) {
					if n.log[idx].Term != entry.Term {
						n.log = n.log[:idx]
						n.log = append(n.log, msg.Entries[i:]...)
						break
					}
				} else {
					n.log = append(n.log, msg.Entries[i:]...)
					break
				}
			}
		}

		// 커밋 인덱스 업데이트
		if msg.Commit > n.commitIndex {
			newCommit := msg.Commit
			if newCommit > n.lastLogIndex() {
				newCommit = n.lastLogIndex()
			}
			n.commitIndex = newCommit
			n.applyCommitted()
		}
	}

	// 응답 전송
	if ch, ok := n.peers[msg.From]; ok {
		resp := Message{
			Type:     MsgAppendEntriesResp,
			From:     n.id,
			To:       msg.From,
			Term:     n.term,
			Success:  success,
			LogIndex: n.lastLogIndex(),
		}
		select {
		case ch <- resp:
		default:
		}
	}
}

// handleAppendEntriesResp는 로그 복제 응답을 처리한다
// etcd의 raft.stepLeader() 내 MsgAppResp 처리에 해당
func (n *RaftNode) handleAppendEntriesResp(msg Message) {
	if n.state != Leader {
		return
	}

	if msg.Success {
		// matchIndex, nextIndex 업데이트
		n.matchIndex[msg.From] = msg.LogIndex
		n.nextIndex[msg.From] = msg.LogIndex + 1

		// 과반수가 복제한 엔트리를 커밋
		n.updateCommitIndex()
	} else {
		// 실패 시 nextIndex 감소 후 재시도
		if n.nextIndex[msg.From] > 1 {
			n.nextIndex[msg.From]--
		}
	}
}

// updateCommitIndex는 과반수 복제된 로그를 커밋한다
// etcd의 raft.maybeCommit()에 해당
func (n *RaftNode) updateCommitIndex() {
	for idx := n.lastLogIndex(); idx > n.commitIndex; idx-- {
		if n.log[idx].Term != n.term {
			continue
		}
		replicatedCount := 1 // 리더 자신
		for _, matchIdx := range n.matchIndex {
			if matchIdx >= idx {
				replicatedCount++
			}
		}
		// 과반수가 복제했으면 커밋
		if replicatedCount > n.clusterSize/2 {
			n.commitIndex = idx
			n.applyCommitted()
			break
		}
	}
}

// applyCommitted는 커밋된 로그를 상태 머신에 적용한다
func (n *RaftNode) applyCommitted() {
	for n.lastApplied < n.commitIndex {
		n.lastApplied++
		cmd := n.log[n.lastApplied].Command
		if cmd != "" {
			n.appliedCmds = append(n.appliedCmds, cmd)
			fmt.Printf("  [노드 %d] 커밋 적용: index=%d, cmd=%q\n", n.id, n.lastApplied, cmd)
		}
	}
}

// Propose는 새 명령을 제안한다 (리더만 가능)
// etcd의 raft.Node.Propose()에 해당
func (n *RaftNode) Propose(command string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.state != Leader || !n.alive {
		return false
	}

	entry := LogEntry{
		Term:    n.term,
		Index:   n.lastLogIndex() + 1,
		Command: command,
	}
	n.log = append(n.log, entry)
	// 리더의 matchIndex 자신도 업데이트
	n.matchIndex[n.id] = n.lastLogIndex()

	fmt.Printf("  [노드 %d] 명령 제안: %q (index=%d)\n", n.id, command, entry.Index)

	// 즉시 복제 시작
	n.sendHeartbeats()
	return true
}

// Kill은 노드를 장애 상태로 만든다
func (n *RaftNode) Kill() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.alive = false
	fmt.Printf("  [노드 %d] *** 장애 발생! ***\n", n.id)
}

// Revive는 노드를 복구한다
func (n *RaftNode) Revive() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.alive = true
	n.state = Follower
	n.votedFor = -1
	n.lastHeartbeat = time.Now()
	n.electionTimeout = randomTimeout(150, 300)
	fmt.Printf("  [노드 %d] 복구됨\n", n.id)
}

// GetState는 노드의 현재 상태를 반환한다
func (n *RaftNode) GetState() (NodeState, int, int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state, n.term, n.commitIndex, n.lastLogIndex()
}

// ========== 클러스터 ==========

// Cluster는 Raft 클러스터를 관리한다
type Cluster struct {
	nodes []*RaftNode
}

func NewCluster(size int) *Cluster {
	nodes := make([]*RaftNode, size)
	for i := 0; i < size; i++ {
		nodes[i] = NewRaftNode(i, size)
	}

	// 피어 채널 연결
	for i := 0; i < size; i++ {
		for j := 0; j < size; j++ {
			if i != j {
				nodes[i].peers[j] = nodes[j].recvCh
			}
		}
	}

	return &Cluster{nodes: nodes}
}

func (c *Cluster) Start() {
	for _, node := range c.nodes {
		go node.Run()
	}
}

func (c *Cluster) Stop() {
	for _, node := range c.nodes {
		close(node.stopCh)
	}
}

// FindLeader는 현재 살아있는 리더를 찾는다
func (c *Cluster) FindLeader() *RaftNode {
	for _, node := range c.nodes {
		node.mu.Lock()
		alive := node.alive
		state := node.state
		node.mu.Unlock()
		if state == Leader && alive {
			return node
		}
	}
	return nil
}

// PrintStatus는 모든 노드 상태를 출력한다
func (c *Cluster) PrintStatus() {
	fmt.Println("\n  ┌─────────────────────────────────────────────────────┐")
	fmt.Println("  │                 클러스터 상태                         │")
	fmt.Println("  ├──────┬───────────┬──────┬────────┬─────────────────┤")
	fmt.Println("  │ 노드 │   상태    │  텀  │ 커밋   │  로그 길이      │")
	fmt.Println("  ├──────┼───────────┼──────┼────────┼─────────────────┤")
	for _, node := range c.nodes {
		state, term, commit, logLen := node.GetState()
		node.mu.Lock()
		alive := node.alive
		node.mu.Unlock()
		status := state.String()
		if !alive {
			status = "DEAD"
		}
		fmt.Printf("  │  %d   │ %-9s │  %2d  │   %2d   │       %2d        │\n",
			node.id, status, term, commit, logLen)
	}
	fmt.Println("  └──────┴───────────┴──────┴────────┴─────────────────┘")
}

// ========== 메인 ==========

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" etcd PoC-05: Raft 합의 알고리즘 시뮬레이션")
	fmt.Println("==========================================================")
	fmt.Println()

	// 1. 리더 선출
	fmt.Println("[1] 3노드 클러스터 시작 - 리더 선출")
	fmt.Println("──────────────────────────────────────")

	cluster := NewCluster(3)
	cluster.Start()
	defer cluster.Stop()

	// 리더 선출 대기
	time.Sleep(500 * time.Millisecond)
	cluster.PrintStatus()

	leader := cluster.FindLeader()
	if leader == nil {
		fmt.Println("  리더 선출 실패, 추가 대기...")
		time.Sleep(500 * time.Millisecond)
		leader = cluster.FindLeader()
	}
	if leader != nil {
		fmt.Printf("\n  리더: 노드 %d\n", leader.id)
	}

	// 2. 로그 복제
	fmt.Println("\n[2] 로그 복제 - 리더에 명령 제안")
	fmt.Println("──────────────────────────────────────")

	if leader != nil {
		leader.Propose("PUT /config/db_host = 10.0.1.5")
		time.Sleep(200 * time.Millisecond)

		leader.Propose("PUT /config/db_port = 5432")
		time.Sleep(200 * time.Millisecond)

		leader.Propose("PUT /config/max_conn = 100")
		time.Sleep(200 * time.Millisecond)
	}

	cluster.PrintStatus()

	// 3. 리더 장애 및 재선거
	fmt.Println("\n[3] 리더 장애 → 재선거")
	fmt.Println("──────────────────────────────────────")

	if leader != nil {
		leader.Kill()
		fmt.Println("  리더 장애 발생. 재선거 대기...")
		time.Sleep(800 * time.Millisecond)
	}

	cluster.PrintStatus()

	newLeader := cluster.FindLeader()
	if newLeader != nil {
		fmt.Printf("\n  새 리더: 노드 %d\n", newLeader.id)

		// 새 리더에 추가 명령
		fmt.Println("\n[4] 새 리더에 명령 제안")
		fmt.Println("──────────────────────────────────────")

		newLeader.Propose("PUT /config/timeout = 30s")
		time.Sleep(200 * time.Millisecond)

		newLeader.Propose("DELETE /config/max_conn")
		time.Sleep(200 * time.Millisecond)
	}

	cluster.PrintStatus()

	// 5. 장애 노드 복구
	fmt.Println("\n[5] 장애 노드 복구")
	fmt.Println("──────────────────────────────────────")

	if leader != nil {
		leader.Revive()
		time.Sleep(500 * time.Millisecond)
	}

	cluster.PrintStatus()

	// 요약
	fmt.Println("\n==========================================================")
	fmt.Println(" 시뮬레이션 요약")
	fmt.Println("==========================================================")
	fmt.Println()
	fmt.Println("  etcd Raft 합의 알고리즘의 핵심 동작:")
	fmt.Println("  1. 리더 선거: 선거 타임아웃 → RequestVote → 과반수 투표 → 리더")
	fmt.Println("  2. 로그 복제: Leader.Propose() → AppendEntries → ACK → 커밋")
	fmt.Println("  3. 하트비트: 리더가 주기적으로 전송하여 리더십 유지")
	fmt.Println("  4. 재선거: 리더 장애 시 팔로워가 타임아웃 후 새 선거 시작")
	fmt.Println("  5. 안전성: 더 높은 텀이 항상 우선, 로그 일관성 보장")
	fmt.Println()
	fmt.Println("  참조 소스:")
	fmt.Println("  - go.etcd.io/raft/v3/raft.go         (Raft 상태머신)")
	fmt.Println("  - server/etcdserver/raft.go           (etcd Raft 통합)")
	fmt.Println()
}
