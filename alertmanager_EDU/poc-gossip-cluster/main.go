// Alertmanager Gossip Cluster PoC
//
// Alertmanager의 Gossip 기반 클러스터링을 시뮬레이션한다.
// memberlist의 Gossip 프로토콜과 CRDT 기반 상태 병합을 재현한다.
//
// 핵심 개념:
//   - Gossip 프로토콜 (랜덤 피어 선택, 전파)
//   - CRDT (최신 타임스탬프 승리)
//   - State 인터페이스 (MarshalBinary, Merge)
//   - 최종 일관성 (eventually consistent)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// Entry는 클러스터 간 동기화되는 데이터 항목이다.
type Entry struct {
	Key       string
	Value     string
	Timestamp time.Time // CRDT: 최신 타임스탬프 승리
}

// State는 노드의 상태 저장소이다 (Silence, nflog 등).
type State struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

// NewState는 새 State를 생성한다.
func NewState() *State {
	return &State{entries: make(map[string]*Entry)}
}

// Set은 항목을 설정한다.
func (s *State) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = &Entry{
		Key:       key,
		Value:     value,
		Timestamp: time.Now(),
	}
}

// Merge는 원격 노드의 항목을 병합한다 (CRDT: 최신 타임스탬프 승리).
func (s *State) Merge(remote *Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.entries[remote.Key]
	if !ok {
		// 새 항목: 추가
		s.entries[remote.Key] = remote
		return true
	}

	// CRDT: 최신 타임스탬프가 승리
	if remote.Timestamp.After(existing.Timestamp) {
		s.entries[remote.Key] = remote
		return true
	}

	return false // 기존이 더 최신
}

// GetAll은 모든 항목의 복사본을 반환한다.
func (s *State) GetAll() []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		result = append(result, e)
	}
	return result
}

// Get은 특정 키의 항목을 반환한다.
func (s *State) Get(key string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	return e, ok
}

// Node는 클러스터의 한 노드이다.
type Node struct {
	Name  string
	State *State
	Peers []*Node // 피어 노드
	mu    sync.Mutex
}

// NewNode는 새 노드를 생성한다.
func NewNode(name string) *Node {
	return &Node{
		Name:  name,
		State: NewState(),
	}
}

// Broadcast는 항목을 Gossip으로 전파한다.
// 랜덤하게 선택된 피어에게 전달하고, 피어는 다시 전파한다.
func (n *Node) Broadcast(entry *Entry, ttl int) {
	if ttl <= 0 || len(n.Peers) == 0 {
		return
	}

	// Gossip: 랜덤 피어 선택 (fanout=1 간소화)
	n.mu.Lock()
	peer := n.Peers[rand.Intn(len(n.Peers))]
	n.mu.Unlock()

	// 피어에게 전달
	merged := peer.State.Merge(entry)
	if merged {
		fmt.Printf("    [Gossip] %s → %s: %s=%q (병합됨, TTL=%d)\n",
			n.Name, peer.Name, entry.Key, entry.Value, ttl)
		// 피어가 다시 전파
		peer.Broadcast(entry, ttl-1)
	} else {
		fmt.Printf("    [Gossip] %s → %s: %s=%q (이미 최신, 전파 중단)\n",
			n.Name, peer.Name, entry.Key, entry.Value)
	}
}

// PushPull은 전체 상태를 교환한다 (주기적 TCP 동기화).
func (n *Node) PushPull(peer *Node) {
	fmt.Printf("  [Push-Pull] %s ↔ %s 상태 교환\n", n.Name, peer.Name)

	// Push: 내 상태를 피어에게
	myEntries := n.State.GetAll()
	pushCount := 0
	for _, e := range myEntries {
		if peer.State.Merge(e) {
			pushCount++
		}
	}

	// Pull: 피어 상태를 나에게
	peerEntries := peer.State.GetAll()
	pullCount := 0
	for _, e := range peerEntries {
		if n.State.Merge(e) {
			pullCount++
		}
	}

	fmt.Printf("  [Push-Pull] Push: %d개, Pull: %d개 병합\n", pushCount, pullCount)
}

// PrintState는 노드의 현재 상태를 출력한다.
func (n *Node) PrintState() {
	entries := n.State.GetAll()
	fmt.Printf("  %s 상태 (%d개):\n", n.Name, len(entries))
	for _, e := range entries {
		fmt.Printf("    %s = %q (at %s)\n",
			e.Key, e.Value, e.Timestamp.Format("15:04:05.000"))
	}
}

func main() {
	fmt.Println("=== Alertmanager Gossip Cluster PoC ===")
	fmt.Println()

	// 3노드 클러스터 생성
	node1 := NewNode("AM-1")
	node2 := NewNode("AM-2")
	node3 := NewNode("AM-3")

	// 피어 연결 (Full Mesh)
	node1.Peers = []*Node{node2, node3}
	node2.Peers = []*Node{node1, node3}
	node3.Peers = []*Node{node1, node2}

	fmt.Println("클러스터: AM-1, AM-2, AM-3 (Full Mesh)")
	fmt.Println()

	// 시나리오 1: Gossip 전파
	fmt.Println("--- 시나리오 1: Gossip 전파 ---")
	fmt.Println("AM-1에서 Silence 생성:")

	entry1 := &Entry{Key: "silence-001", Value: "alertname=HighCPU", Timestamp: time.Now()}
	node1.State.Set("silence-001", "alertname=HighCPU")
	node1.Broadcast(entry1, 3)
	fmt.Println()

	fmt.Println("전파 후 상태:")
	node1.PrintState()
	node2.PrintState()
	node3.PrintState()
	fmt.Println()

	// 시나리오 2: CRDT 충돌 해결
	fmt.Println("--- 시나리오 2: CRDT 충돌 해결 ---")
	fmt.Println("AM-1과 AM-2에서 같은 키를 다른 값으로 업데이트:")

	time.Sleep(10 * time.Millisecond) // 타임스탬프 차이 보장
	entry2a := &Entry{Key: "silence-002", Value: "severity=warning", Timestamp: time.Now()}
	node1.State.Merge(entry2a)
	fmt.Printf("  AM-1: silence-002 = %q (시간: %s)\n",
		entry2a.Value, entry2a.Timestamp.Format("15:04:05.000"))

	time.Sleep(10 * time.Millisecond)
	entry2b := &Entry{Key: "silence-002", Value: "severity=critical", Timestamp: time.Now()}
	node2.State.Merge(entry2b)
	fmt.Printf("  AM-2: silence-002 = %q (시간: %s, 더 최신)\n",
		entry2b.Value, entry2b.Timestamp.Format("15:04:05.000"))
	fmt.Println()

	// Push-Pull로 상태 교환
	fmt.Println("--- 시나리오 3: Push-Pull 동기화 ---")
	node1.PushPull(node2)
	fmt.Println()

	fmt.Println("동기화 후 상태:")
	node1.PrintState()
	node2.PrintState()
	fmt.Println()

	// 충돌 해결 확인
	e1, _ := node1.State.Get("silence-002")
	e2, _ := node2.State.Get("silence-002")
	fmt.Printf("  AM-1 silence-002 = %q\n", e1.Value)
	fmt.Printf("  AM-2 silence-002 = %q\n", e2.Value)
	if e1.Value == e2.Value {
		fmt.Println("  → CRDT 충돌 해결 성공: 최신 타임스탬프 승리!")
	}
	fmt.Println()

	// 시나리오 4: 전체 클러스터 동기화
	fmt.Println("--- 시나리오 4: 전체 클러스터 동기화 ---")
	node3.State.Set("nflog-001", "group=alertname:HighCPU")
	fmt.Println("AM-3에서 nflog 기록 추가")
	node2.PushPull(node3)
	node1.PushPull(node2)
	fmt.Println()

	fmt.Println("최종 상태:")
	node1.PrintState()
	node2.PrintState()
	node3.PrintState()

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. Gossip: 랜덤 피어 선택 → UDP로 변경사항 전파 → 점진적 확산")
	fmt.Println("2. Push-Pull: 주기적 TCP로 전체 상태 교환 → 누락 방지")
	fmt.Println("3. CRDT: 최신 타임스탬프가 승리 → 충돌 자동 해결")
	fmt.Println("4. 최종 일관성: 모든 노드가 결국 같은 상태로 수렴")
}
