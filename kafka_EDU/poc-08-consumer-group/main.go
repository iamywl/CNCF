package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kafka Consumer Group Coordination Simulation
// Based on: ClassicGroup.java, ClassicGroupState.java, GroupMetadataManager.java
//
// Kafka의 클래식 컨슈머 그룹은 다음 상태 머신을 따른다:
//   EMPTY → PREPARING_REBALANCE → COMPLETING_REBALANCE → STABLE → ...
//
// 프로토콜:
//   1. JoinGroup: 멤버가 그룹에 참여 요청
//   2. SyncGroup: 리더가 파티션 할당을 제출, 팔로워가 할당을 수신
//   3. Heartbeat: 멤버의 생존 확인
//   4. LeaveGroup: 멤버가 그룹에서 탈퇴
//
// 이 PoC는 전체 리밸런스 사이클과 하트비트 기반 장애 감지를 시뮬레이션한다.
// =============================================================================

// --- 그룹 상태 (ClassicGroupState.java) ---
// 상태 전이 규칙:
//   EMPTY.validPreviousStates = {PREPARING_REBALANCE}
//   PREPARING_REBALANCE.validPreviousStates = {STABLE, COMPLETING_REBALANCE, EMPTY}
//   COMPLETING_REBALANCE.validPreviousStates = {PREPARING_REBALANCE}
//   STABLE.validPreviousStates = {COMPLETING_REBALANCE}
//   DEAD.validPreviousStates = {STABLE, PREPARING_REBALANCE, COMPLETING_REBALANCE, EMPTY, DEAD}
type GroupState int

const (
	Empty              GroupState = iota // 멤버 없음, 오프셋만 존재 가능
	PreparingRebalance                  // 리밸런스 준비 중, 멤버 참여 대기
	CompletingRebalance                 // 리더의 파티션 할당 대기
	Stable                              // 정상 운영 중
	Dead                                // 그룹 삭제 대기
)

var validPreviousStates = map[GroupState][]GroupState{
	Empty:              {PreparingRebalance},
	PreparingRebalance: {Stable, CompletingRebalance, Empty},
	CompletingRebalance: {PreparingRebalance},
	Stable:             {CompletingRebalance},
	Dead:               {Stable, PreparingRebalance, CompletingRebalance, Empty, Dead},
}

func (s GroupState) String() string {
	switch s {
	case Empty:
		return "EMPTY"
	case PreparingRebalance:
		return "PREPARING_REBALANCE"
	case CompletingRebalance:
		return "COMPLETING_REBALANCE"
	case Stable:
		return "STABLE"
	case Dead:
		return "DEAD"
	}
	return "UNKNOWN"
}

// --- 멤버 (ClassicGroupMember.java) ---
type GroupMember struct {
	MemberID       string
	ClientID       string
	GroupInstanceID string // 정적 멤버십용
	Assignment     []TopicPartition
	LastHeartbeat  time.Time
	JoinResponse   chan *JoinGroupResponse
	SyncResponse   chan *SyncGroupResponse
}

type TopicPartition struct {
	Topic     string
	Partition int
}

func (tp TopicPartition) String() string {
	return fmt.Sprintf("%s-%d", tp.Topic, tp.Partition)
}

// --- 프로토콜 메시지 ---
type JoinGroupRequest struct {
	GroupID  string
	MemberID string
	ClientID string
}

type JoinGroupResponse struct {
	MemberID     string
	GenerationID int
	LeaderID     string
	Members      []string // 리더에게만 전달
	Error        string
}

type SyncGroupRequest struct {
	GroupID      string
	MemberID     string
	GenerationID int
	Assignments  map[string][]TopicPartition // 리더만 제출
}

type SyncGroupResponse struct {
	Assignment []TopicPartition
	Error      string
}

type HeartbeatResponse struct {
	Error string
}

// --- ClassicGroup (ClassicGroup.java) ---
type ClassicGroup struct {
	mu sync.Mutex

	groupID       string
	state         GroupState
	previousState GroupState
	generationID  int
	leaderID      string
	members       map[string]*GroupMember
	protocolName  string

	// 토픽-파티션 정보
	subscribedTopics []string
	partitions       []TopicPartition

	// 하트비트 타임아웃
	sessionTimeoutMs int

	// 이벤트 로그
	eventLog []string
}

func NewClassicGroup(groupID string, topics []string, partitionsPerTopic int, sessionTimeoutMs int) *ClassicGroup {
	var partitions []TopicPartition
	for _, topic := range topics {
		for p := 0; p < partitionsPerTopic; p++ {
			partitions = append(partitions, TopicPartition{Topic: topic, Partition: p})
		}
	}

	return &ClassicGroup{
		groupID:          groupID,
		state:            Empty,
		previousState:    Dead,
		generationID:     0,
		members:          make(map[string]*GroupMember),
		subscribedTopics: topics,
		partitions:       partitions,
		sessionTimeoutMs: sessionTimeoutMs,
	}
}

func (g *ClassicGroup) logEvent(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	g.eventLog = append(g.eventLog, msg)
	fmt.Printf("  [Group:%s/%s] %s\n", g.groupID, g.state, msg)
}

// transitionTo는 그룹 상태를 전환한다.
// ClassicGroup.java:995 - assertValidTransition()으로 유효성 검사
func (g *ClassicGroup) transitionTo(newState GroupState) error {
	valid := false
	for _, prev := range validPreviousStates[newState] {
		if prev == g.state {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("잘못된 상태 전이: %s → %s", g.state, newState)
	}

	g.previousState = g.state
	g.state = newState
	g.logEvent("상태 전이: %s → %s", g.previousState, g.state)
	return nil
}

// canRebalance는 리밸런스 가능 여부를 확인한다.
// ClassicGroup.java:987
func (g *ClassicGroup) canRebalance() bool {
	for _, prev := range validPreviousStates[PreparingRebalance] {
		if prev == g.state {
			return true
		}
	}
	return false
}

// --- JoinGroup 프로토콜 ---
// ClassicGroup.java의 멤버 추가 + 리밸런스 트리거

func (g *ClassicGroup) HandleJoinGroup(req *JoinGroupRequest) *JoinGroupResponse {
	g.mu.Lock()
	defer g.mu.Unlock()

	memberID := req.MemberID
	if memberID == "" {
		// 새 멤버: memberID 생성
		// ClassicGroup.java의 MEMBER_ID_DELIMITER = "-"
		memberID = fmt.Sprintf("%s-%s-%d", req.ClientID, "uuid", time.Now().UnixNano()%10000)
	}

	// 멤버 등록
	member, exists := g.members[memberID]
	if !exists {
		member = &GroupMember{
			MemberID:      memberID,
			ClientID:      req.ClientID,
			LastHeartbeat: time.Now(),
			JoinResponse:  make(chan *JoinGroupResponse, 1),
			SyncResponse:  make(chan *SyncGroupResponse, 1),
		}
		g.members[memberID] = member
		g.logEvent("멤버 추가: %s (client=%s)", memberID, req.ClientID)

		// 리더가 없으면 첫 멤버가 리더
		if g.leaderID == "" {
			g.leaderID = memberID
			g.logEvent("그룹 리더 설정: %s", memberID)
		}
	} else {
		member.LastHeartbeat = time.Now()
	}

	// 리밸런스 트리거
	if g.canRebalance() {
		g.transitionTo(PreparingRebalance)
	}

	return &JoinGroupResponse{
		MemberID:     memberID,
		GenerationID: g.generationID,
		LeaderID:     g.leaderID,
	}
}

// PrepareRebalance는 모든 멤버가 참여한 후 리밸런스를 준비한다.
func (g *ClassicGroup) PrepareRebalance() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state != PreparingRebalance {
		return
	}

	// generationID 증가 (ClassicGroup.java의 generationId 필드)
	g.generationID++
	g.logEvent("generationID 증가: %d", g.generationID)

	// 상태 전이: PREPARING_REBALANCE → COMPLETING_REBALANCE
	g.transitionTo(CompletingRebalance)

	// 모든 멤버에게 JoinResponse 전달
	memberIDs := make([]string, 0, len(g.members))
	for id := range g.members {
		memberIDs = append(memberIDs, id)
	}
	sort.Strings(memberIDs)

	for _, memberID := range memberIDs {
		member := g.members[memberID]
		resp := &JoinGroupResponse{
			MemberID:     memberID,
			GenerationID: g.generationID,
			LeaderID:     g.leaderID,
		}
		// 리더에게만 멤버 목록 전달
		if memberID == g.leaderID {
			resp.Members = memberIDs
		}
		select {
		case member.JoinResponse <- resp:
		default:
		}
	}
}

// --- SyncGroup 프로토콜 ---
// 리더가 파티션 할당을 제출하면 모든 멤버에게 전달

func (g *ClassicGroup) HandleSyncGroup(req *SyncGroupRequest) *SyncGroupResponse {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state != CompletingRebalance {
		return &SyncGroupResponse{Error: "NOT_IN_COMPLETING_REBALANCE"}
	}

	// generationID 확인
	if req.GenerationID != g.generationID {
		return &SyncGroupResponse{Error: fmt.Sprintf("ILLEGAL_GENERATION: expected=%d, got=%d", g.generationID, req.GenerationID)}
	}

	// 리더가 할당을 제출한 경우
	if req.MemberID == g.leaderID && req.Assignments != nil {
		g.logEvent("리더가 파티션 할당 제출: %d개 멤버", len(req.Assignments))

		// 모든 멤버에게 할당 전달
		for memberID, assignment := range req.Assignments {
			if member, ok := g.members[memberID]; ok {
				member.Assignment = assignment
				partStrs := make([]string, len(assignment))
				for i, tp := range assignment {
					partStrs[i] = tp.String()
				}
				g.logEvent("  %s → [%s]", memberID, strings.Join(partStrs, ", "))

				select {
				case member.SyncResponse <- &SyncGroupResponse{Assignment: assignment}:
				default:
				}
			}
		}

		// 상태 전이: COMPLETING_REBALANCE → STABLE
		g.transitionTo(Stable)
	}

	// 현재 멤버의 할당 반환
	member := g.members[req.MemberID]
	if member != nil {
		return &SyncGroupResponse{Assignment: member.Assignment}
	}
	return &SyncGroupResponse{Error: "UNKNOWN_MEMBER_ID"}
}

// --- Range 파티션 할당 전략 ---
// org.apache.kafka.clients.consumer.RangeAssignor
//
// 각 토픽별로:
//   1. 파티션을 번호순으로 정렬
//   2. 멤버를 사전순으로 정렬
//   3. 각 멤버에게 연속된 파티션 범위를 할당
//   4. 나머지 파티션은 앞쪽 멤버에게 1개씩 추가
func RangeAssign(members []string, partitions []TopicPartition) map[string][]TopicPartition {
	assignment := make(map[string][]TopicPartition)
	for _, m := range members {
		assignment[m] = []TopicPartition{}
	}

	// 토픽별로 그룹화
	topicPartitions := make(map[string][]TopicPartition)
	for _, tp := range partitions {
		topicPartitions[tp.Topic] = append(topicPartitions[tp.Topic], tp)
	}

	sort.Strings(members)

	for _, topic := range sortedKeys(topicPartitions) {
		tps := topicPartitions[topic]
		sort.Slice(tps, func(i, j int) bool {
			return tps[i].Partition < tps[j].Partition
		})

		numPartitions := len(tps)
		numMembers := len(members)
		partitionsPerMember := numPartitions / numMembers
		remainder := numPartitions % numMembers

		idx := 0
		for i, member := range members {
			count := partitionsPerMember
			if i < remainder {
				count++ // 나머지 파티션을 앞쪽 멤버에 추가
			}
			for j := 0; j < count && idx < numPartitions; j++ {
				assignment[member] = append(assignment[member], tps[idx])
				idx++
			}
		}
	}

	return assignment
}

func sortedKeys(m map[string][]TopicPartition) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- Heartbeat 및 장애 감지 ---

func (g *ClassicGroup) HandleHeartbeat(memberID string, generationID int) *HeartbeatResponse {
	g.mu.Lock()
	defer g.mu.Unlock()

	member, exists := g.members[memberID]
	if !exists {
		return &HeartbeatResponse{Error: "UNKNOWN_MEMBER_ID"}
	}

	if g.state == PreparingRebalance || g.state == CompletingRebalance {
		return &HeartbeatResponse{Error: "REBALANCE_IN_PROGRESS"}
	}

	if generationID != g.generationID {
		return &HeartbeatResponse{Error: "ILLEGAL_GENERATION"}
	}

	member.LastHeartbeat = time.Now()
	return &HeartbeatResponse{}
}

// CheckHeartbeatTimeout는 타임아웃된 멤버를 감지하고 리밸런스를 트리거한다.
func (g *ClassicGroup) CheckHeartbeatTimeout() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state != Stable {
		return nil
	}

	var expired []string
	now := time.Now()
	timeout := time.Duration(g.sessionTimeoutMs) * time.Millisecond

	for memberID, member := range g.members {
		if now.Sub(member.LastHeartbeat) > timeout {
			expired = append(expired, memberID)
		}
	}

	// 만료된 멤버 제거 및 리밸런스 트리거
	if len(expired) > 0 {
		for _, memberID := range expired {
			g.logEvent("하트비트 타임아웃: %s 제거", memberID)
			delete(g.members, memberID)

			// 리더가 제거되면 새 리더 선출
			if memberID == g.leaderID {
				g.leaderID = ""
				for id := range g.members {
					g.leaderID = id
					break
				}
				if g.leaderID != "" {
					g.logEvent("새 그룹 리더: %s", g.leaderID)
				}
			}
		}

		if len(g.members) > 0 && g.canRebalance() {
			g.transitionTo(PreparingRebalance)
		} else if len(g.members) == 0 {
			g.transitionTo(PreparingRebalance)
			g.transitionTo(Empty)
		}
	}

	return expired
}

// HandleLeaveGroup은 멤버의 자발적 탈퇴를 처리한다.
func (g *ClassicGroup) HandleLeaveGroup(memberID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.members[memberID]; !exists {
		return
	}

	g.logEvent("멤버 탈퇴: %s", memberID)
	delete(g.members, memberID)

	// 리더가 탈퇴하면 새 리더 선출
	if memberID == g.leaderID {
		g.leaderID = ""
		for id := range g.members {
			g.leaderID = id
			break
		}
		if g.leaderID != "" {
			g.logEvent("새 그룹 리더: %s", g.leaderID)
		}
	}

	// 리밸런스 트리거
	if len(g.members) > 0 && g.canRebalance() {
		g.transitionTo(PreparingRebalance)
	} else if len(g.members) == 0 {
		g.transitionTo(PreparingRebalance)
		g.transitionTo(Empty)
	}
}

func (g *ClassicGroup) PrintState() {
	g.mu.Lock()
	defer g.mu.Unlock()

	fmt.Printf("  그룹 상태: %s, generationID=%d, 멤버=%d, 리더=%s\n",
		g.state, g.generationID, len(g.members), g.leaderID)
	for _, member := range g.members {
		partStrs := make([]string, len(member.Assignment))
		for i, tp := range member.Assignment {
			partStrs[i] = tp.String()
		}
		fmt.Printf("    %s (client=%s): [%s]\n",
			member.MemberID, member.ClientID, strings.Join(partStrs, ", "))
	}
}

func main() {
	fmt.Println("=== Kafka Consumer Group Coordination PoC ===")
	fmt.Println()
	fmt.Println("상태 머신 (ClassicGroupState.java):")
	fmt.Println("  EMPTY → PREPARING_REBALANCE → COMPLETING_REBALANCE → STABLE")
	fmt.Println("    ↑                                                     |")
	fmt.Println("    └──────── 멤버 장애/탈퇴/참여 ────────────────────────┘")
	fmt.Println()

	// --- 1. 그룹 생성 ---
	fmt.Println("--- 1단계: 그룹 생성 ---")
	group := NewClassicGroup(
		"order-processing-group",
		[]string{"orders", "events"},
		3, // 토픽당 3개 파티션
		500, // 세션 타임아웃 500ms (시뮬레이션용으로 짧게)
	)
	fmt.Printf("  그룹: %s\n", group.groupID)
	fmt.Printf("  파티션: ")
	for _, tp := range group.partitions {
		fmt.Printf("%s ", tp)
	}
	fmt.Println()
	fmt.Println()

	// --- 2. 멤버 3명 JoinGroup ---
	fmt.Println("--- 2단계: 멤버 3명 JoinGroup ---")
	fmt.Println()

	clientIDs := []string{"consumer-A", "consumer-B", "consumer-C"}
	memberIDs := make([]string, 3)

	for i, clientID := range clientIDs {
		resp := group.HandleJoinGroup(&JoinGroupRequest{
			GroupID:  "order-processing-group",
			ClientID: clientID,
		})
		memberIDs[i] = resp.MemberID
		fmt.Printf("  JoinGroup 응답: memberID=%s, leader=%s\n", resp.MemberID, resp.LeaderID)
	}
	fmt.Println()

	// --- 3. PrepareRebalance → CompletingRebalance ---
	fmt.Println("--- 3단계: 리밸런스 준비 ---")
	fmt.Println()
	group.PrepareRebalance()
	fmt.Println()

	// --- 4. 리더가 Range 할당 수행 후 SyncGroup ---
	fmt.Println("--- 4단계: SyncGroup (Range 할당) ---")
	fmt.Println()

	// 리더가 Range 할당 수행
	group.mu.Lock()
	leaderID := group.leaderID
	sortedMembers := make([]string, 0, len(group.members))
	for id := range group.members {
		sortedMembers = append(sortedMembers, id)
	}
	sort.Strings(sortedMembers)
	allPartitions := group.partitions
	genID := group.generationID
	group.mu.Unlock()

	assignments := RangeAssign(sortedMembers, allPartitions)

	fmt.Println("  Range 할당 결과:")
	for _, memberID := range sortedMembers {
		parts := assignments[memberID]
		partStrs := make([]string, len(parts))
		for i, tp := range parts {
			partStrs[i] = tp.String()
		}
		fmt.Printf("    %s → [%s]\n", memberID, strings.Join(partStrs, ", "))
	}
	fmt.Println()

	// 리더가 SyncGroup 제출
	syncResp := group.HandleSyncGroup(&SyncGroupRequest{
		GroupID:      "order-processing-group",
		MemberID:     leaderID,
		GenerationID: genID,
		Assignments:  assignments,
	})
	fmt.Printf("  리더 SyncGroup 응답: error=%q\n", syncResp.Error)
	fmt.Println()

	// 팔로워들도 SyncGroup
	for _, memberID := range sortedMembers {
		if memberID == leaderID {
			continue
		}
		resp := group.HandleSyncGroup(&SyncGroupRequest{
			GroupID:      "order-processing-group",
			MemberID:     memberID,
			GenerationID: genID,
		})
		partStrs := make([]string, len(resp.Assignment))
		for i, tp := range resp.Assignment {
			partStrs[i] = tp.String()
		}
		fmt.Printf("  팔로워 %s SyncGroup 응답: [%s]\n", memberID, strings.Join(partStrs, ", "))
	}
	fmt.Println()

	// --- 5. STABLE 상태 확인 ---
	fmt.Println("--- 5단계: STABLE 상태 ---")
	group.PrintState()
	fmt.Println()

	// --- 6. 하트비트 시뮬레이션 ---
	fmt.Println("--- 6단계: 하트비트 시뮬레이션 ---")
	fmt.Println()

	// 정상 하트비트
	for _, memberID := range sortedMembers {
		resp := group.HandleHeartbeat(memberID, genID)
		fmt.Printf("  Heartbeat %s: error=%q\n", memberID, resp.Error)
	}
	fmt.Println()

	// --- 7. 멤버 장애 → 하트비트 타임아웃 → 리밸런스 ---
	fmt.Println("--- 7단계: 멤버 장애 (하트비트 타임아웃) ---")
	fmt.Println()

	// 마지막 멤버의 하트비트를 과거로 설정 (타임아웃 시뮬레이션)
	lastMember := sortedMembers[len(sortedMembers)-1]
	group.mu.Lock()
	group.members[lastMember].LastHeartbeat = time.Now().Add(-1 * time.Second)
	group.mu.Unlock()

	fmt.Printf("  %s의 하트비트를 1초 전으로 설정 (타임아웃=%dms)\n", lastMember, group.sessionTimeoutMs)

	expired := group.CheckHeartbeatTimeout()
	fmt.Printf("  타임아웃된 멤버: %v\n", expired)
	fmt.Println()

	// --- 8. 나머지 멤버로 리밸런스 ---
	fmt.Printf("--- 8단계: 리밸런스 (남은 멤버 %d명) ---\n", len(sortedMembers)-1)
	fmt.Println()

	group.PrepareRebalance()
	fmt.Println()

	// 새 할당
	group.mu.Lock()
	leaderID = group.leaderID
	remainingMembers := make([]string, 0, len(group.members))
	for id := range group.members {
		remainingMembers = append(remainingMembers, id)
	}
	sort.Strings(remainingMembers)
	genID = group.generationID
	group.mu.Unlock()

	newAssignments := RangeAssign(remainingMembers, allPartitions)

	fmt.Println("  새 Range 할당 결과:")
	for _, memberID := range remainingMembers {
		parts := newAssignments[memberID]
		partStrs := make([]string, len(parts))
		for i, tp := range parts {
			partStrs[i] = tp.String()
		}
		fmt.Printf("    %s → [%s]\n", memberID, strings.Join(partStrs, ", "))
	}
	fmt.Println()

	group.HandleSyncGroup(&SyncGroupRequest{
		GroupID:      "order-processing-group",
		MemberID:     leaderID,
		GenerationID: genID,
		Assignments:  newAssignments,
	})
	fmt.Println()

	// --- 9. 멤버 자발적 탈퇴 → 리밸런스 ---
	fmt.Println("--- 9단계: 멤버 자발적 탈퇴 (LeaveGroup) ---")
	fmt.Println()

	if len(remainingMembers) > 1 {
		leavingMember := remainingMembers[0]
		group.HandleLeaveGroup(leavingMember)
		fmt.Println()

		// 리밸런스
		group.PrepareRebalance()
		fmt.Println()

		group.mu.Lock()
		leaderID = group.leaderID
		finalMembers := make([]string, 0, len(group.members))
		for id := range group.members {
			finalMembers = append(finalMembers, id)
		}
		sort.Strings(finalMembers)
		genID = group.generationID
		group.mu.Unlock()

		finalAssignments := RangeAssign(finalMembers, allPartitions)

		fmt.Println("  최종 Range 할당 결과:")
		for _, memberID := range finalMembers {
			parts := finalAssignments[memberID]
			partStrs := make([]string, len(parts))
			for i, tp := range parts {
				partStrs[i] = tp.String()
			}
			fmt.Printf("    %s → [%s] (모든 파티션 담당)\n", memberID, strings.Join(partStrs, ", "))
		}

		group.HandleSyncGroup(&SyncGroupRequest{
			GroupID:      "order-processing-group",
			MemberID:     leaderID,
			GenerationID: genID,
			Assignments:  finalAssignments,
		})
	}
	fmt.Println()

	// --- 최종 상태 ---
	fmt.Println("--- 최종 그룹 상태 ---")
	group.PrintState()
	fmt.Println()

	// --- 아키텍처 요약 ---
	fmt.Println("=== 아키텍처 요약 ===")
	fmt.Println()
	fmt.Println("상태 전이 규칙 (ClassicGroupState.java):")
	fmt.Println("  EMPTY.validPreviousStates               = {PREPARING_REBALANCE}")
	fmt.Println("  PREPARING_REBALANCE.validPreviousStates = {STABLE, COMPLETING_REBALANCE, EMPTY}")
	fmt.Println("  COMPLETING_REBALANCE.validPreviousStates = {PREPARING_REBALANCE}")
	fmt.Println("  STABLE.validPreviousStates              = {COMPLETING_REBALANCE}")
	fmt.Println("  DEAD.validPreviousStates                = {모든 상태}")
	fmt.Println()
	fmt.Println("리밸런스 프로토콜:")
	fmt.Println("  1. JoinGroup: 모든 멤버가 코디네이터에 참여 요청")
	fmt.Println("  2. 코디네이터가 리더를 선정하고 멤버 목록을 리더에게 전달")
	fmt.Println("  3. SyncGroup: 리더가 파티션 할당을 제출")
	fmt.Println("  4. 코디네이터가 모든 멤버에게 할당을 전달")
	fmt.Println("  5. 그룹이 STABLE 상태로 전환")
	fmt.Println()
	fmt.Println("Range 할당 전략:")
	fmt.Println("  - 토픽별로 파티션을 번호순, 멤버를 사전순 정렬")
	fmt.Println("  - 파티션을 멤버 수로 나누어 연속 범위 할당")
	fmt.Println("  - 나머지 파티션은 앞쪽 멤버에게 1개씩 추가")
	fmt.Println()
	fmt.Println("리밸런스 트리거 조건:")
	fmt.Println("  - 새 멤버 참여 (JoinGroup)")
	fmt.Println("  - 멤버 자발적 탈퇴 (LeaveGroup)")
	fmt.Println("  - 하트비트 타임아웃 (session.timeout.ms 초과)")
	fmt.Println("  - 구독 토픽 변경")
}
