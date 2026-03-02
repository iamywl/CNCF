// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Peer 디스커버리 & 관리 패턴
//
// Hubble Relay는 여러 Hubble Server(노드)를 발견하고 관리합니다:
//   - Peer 서비스로 노드 목록 수신 (gRPC 스트리밍)
//   - 변경 알림: PEER_ADDED, PEER_UPDATED, PEER_DELETED
//   - 연결 상태 추적: CONNECTING → READY → IDLE
//   - 자동 재연결: 실패 시 지수 백오프
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ========================================
// 1. Peer 관련 타입
// ========================================

// ChangeType은 Peer 변경 유형입니다.
// 실제 Hubble: peerpb.ChangeNotificationType
type ChangeType int

const (
	PeerAdded   ChangeType = iota
	PeerUpdated
	PeerDeleted
)

func (t ChangeType) String() string {
	switch t {
	case PeerAdded:
		return "PEER_ADDED"
	case PeerUpdated:
		return "PEER_UPDATED"
	case PeerDeleted:
		return "PEER_DELETED"
	default:
		return "UNKNOWN"
	}
}

// ConnState는 gRPC 연결 상태입니다.
// 실제: google.golang.org/grpc/connectivity.State
type ConnState int

const (
	StateIdle ConnState = iota
	StateConnecting
	StateReady
	StateTransientFailure
	StateShutdown
)

func (s ConnState) String() string {
	switch s {
	case StateIdle:
		return "IDLE"
	case StateConnecting:
		return "CONNECTING"
	case StateReady:
		return "READY"
	case StateTransientFailure:
		return "TRANSIENT_FAILURE"
	case StateShutdown:
		return "SHUTDOWN"
	default:
		return "UNKNOWN"
	}
}

// Peer는 Hubble Server 노드를 나타냅니다.
type Peer struct {
	Name    string
	Address string
	State   ConnState
	TLSName string
}

// ChangeNotification은 Peer 변경 알림입니다.
type ChangeNotification struct {
	Type ChangeType
	Peer Peer
}

// ========================================
// 2. PeerManager (Hubble의 pool/manager.go 패턴)
// ========================================

// PeerManager는 Peer 연결 풀을 관리합니다.
// 실제 Hubble: pkg/hubble/relay/pool/manager.go
type PeerManager struct {
	mu    sync.RWMutex
	peers map[string]*Peer
	stop  chan struct{}

	// 메트릭
	connStatusCounts map[ConnState]int
}

func NewPeerManager() *PeerManager {
	return &PeerManager{
		peers:            make(map[string]*Peer),
		stop:             make(chan struct{}),
		connStatusCounts: make(map[ConnState]int),
	}
}

// Upsert는 Peer를 추가하거나 업데이트합니다.
func (m *PeerManager) Upsert(p Peer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.peers[p.Name]; ok {
		fmt.Printf("    [PeerManager] Peer 업데이트: %s (%s → %s)\n",
			p.Name, existing.State, p.State)
		existing.Address = p.Address
		existing.State = p.State
		existing.TLSName = p.TLSName
	} else {
		fmt.Printf("    [PeerManager] Peer 추가: %s (addr=%s, tls=%s)\n",
			p.Name, p.Address, p.TLSName)
		m.peers[p.Name] = &p
	}
}

// Remove는 Peer를 제거합니다.
func (m *PeerManager) Remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.peers[name]; ok {
		delete(m.peers, name)
		fmt.Printf("    [PeerManager] Peer 제거: %s\n", name)
	}
}

// UpdateState는 Peer의 연결 상태를 변경합니다.
func (m *PeerManager) UpdateState(name string, state ConnState) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if p, ok := m.peers[name]; ok {
		oldState := p.State
		p.State = state
		fmt.Printf("    [PeerManager] 상태 변경: %s (%s → %s)\n",
			name, oldState, state)
	}
}

// ReportStatus는 현재 연결 상태 분포를 보고합니다.
func (m *PeerManager) ReportStatus() map[ConnState]int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	counts := make(map[ConnState]int)
	for _, p := range m.peers {
		counts[p.State]++
	}
	return counts
}

// List는 모든 Peer를 반환합니다.
func (m *PeerManager) List() []Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]Peer, 0, len(m.peers))
	for _, p := range m.peers {
		result = append(result, *p)
	}
	return result
}

// ========================================
// 3. Notification Watcher (gRPC 스트림 시뮬레이션)
// ========================================

// WatchNotifications는 Peer 변경 알림을 스트리밍합니다.
// 실제 Hubble: PeerManager.watchNotifications()
func WatchNotifications(notifications <-chan ChangeNotification, mgr *PeerManager, done chan struct{}) {
	defer close(done)

	for cn := range notifications {
		fmt.Printf("  [Watch] 알림 수신: %s\n", cn.Type)

		switch cn.Type {
		case PeerAdded:
			mgr.Upsert(cn.Peer)
		case PeerUpdated:
			mgr.Upsert(cn.Peer)
		case PeerDeleted:
			mgr.Remove(cn.Peer.Name)
		}
		fmt.Println()
	}
}

// ========================================
// 4. 연결 재시도 (지수 백오프)
// ========================================

// BackoffConfig는 재시도 설정입니다.
type BackoffConfig struct {
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	Multiplier float64
	Jitter     float64
}

func DefaultBackoff() BackoffConfig {
	return BackoffConfig{
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   5 * time.Second,
		Multiplier: 2.0,
		Jitter:     0.2,
	}
}

// NextDelay는 다음 재시도 대기 시간을 계산합니다.
func (b BackoffConfig) NextDelay(attempt int) time.Duration {
	delay := float64(b.BaseDelay)
	for i := 0; i < attempt; i++ {
		delay *= b.Multiplier
	}
	if delay > float64(b.MaxDelay) {
		delay = float64(b.MaxDelay)
	}
	// 지터 추가
	jitter := delay * b.Jitter * (2*rand.Float64() - 1)
	return time.Duration(delay + jitter)
}

// ========================================
// 5. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Peer 디스커버리 & 관리 패턴 ===")
	fmt.Println()
	fmt.Println("Relay의 Peer 관리:")
	fmt.Println("  1. Peer 서비스로 노드 목록 스트리밍 수신")
	fmt.Println("  2. 변경 알림: PEER_ADDED / PEER_UPDATED / PEER_DELETED")
	fmt.Println("  3. 연결 상태 추적: IDLE → CONNECTING → READY")
	fmt.Println("  4. 실패 시 지수 백오프로 재연결")
	fmt.Println()

	mgr := NewPeerManager()
	notifications := make(chan ChangeNotification, 10)
	done := make(chan struct{})

	go WatchNotifications(notifications, mgr, done)

	// ── 시나리오 1: 초기 Peer 발견 ──
	fmt.Println("━━━ 시나리오 1: 초기 Peer 발견 (3개 노드) ━━━")
	fmt.Println()

	initialPeers := []Peer{
		{Name: "k8s-node-1", Address: "10.0.0.1:4244", State: StateConnecting, TLSName: "hubble-peer.cilium.io"},
		{Name: "k8s-node-2", Address: "10.0.0.2:4244", State: StateConnecting, TLSName: "hubble-peer.cilium.io"},
		{Name: "k8s-node-3", Address: "10.0.0.3:4244", State: StateConnecting, TLSName: "hubble-peer.cilium.io"},
	}

	for _, p := range initialPeers {
		notifications <- ChangeNotification{Type: PeerAdded, Peer: p}
	}
	time.Sleep(100 * time.Millisecond)

	// ── 시나리오 2: 연결 상태 변화 ──
	fmt.Println("━━━ 시나리오 2: 연결 상태 변화 ━━━")
	fmt.Println()

	// 연결 성공
	mgr.UpdateState("k8s-node-1", StateReady)
	mgr.UpdateState("k8s-node-2", StateReady)
	// node-3은 연결 실패
	mgr.UpdateState("k8s-node-3", StateTransientFailure)
	fmt.Println()

	// 상태 보고
	status := mgr.ReportStatus()
	fmt.Println("  [연결 상태 분포]")
	for state, count := range status {
		bar := strings.Repeat("█", count*5)
		fmt.Printf("    %-20s: %s (%d)\n", state, bar, count)
	}
	fmt.Println()

	// ── 시나리오 3: Peer 추가/삭제 ──
	fmt.Println("━━━ 시나리오 3: Peer 추가/삭제 (동적 변경) ━━━")
	fmt.Println()

	// 새 노드 추가
	notifications <- ChangeNotification{
		Type: PeerAdded,
		Peer: Peer{Name: "k8s-node-4", Address: "10.0.0.4:4244", State: StateConnecting, TLSName: "hubble-peer.cilium.io"},
	}
	time.Sleep(50 * time.Millisecond)

	// 노드 삭제 (스케일 다운)
	notifications <- ChangeNotification{
		Type: PeerDeleted,
		Peer: Peer{Name: "k8s-node-3"},
	}
	time.Sleep(50 * time.Millisecond)

	// 현재 Peer 목록
	fmt.Println("  [현재 Peer 목록]")
	for _, p := range mgr.List() {
		fmt.Printf("    %-15s addr=%-18s state=%-12s tls=%s\n",
			p.Name, p.Address, p.State, p.TLSName)
	}
	fmt.Println()

	// ── 시나리오 4: 지수 백오프 ──
	fmt.Println("━━━ 시나리오 4: 연결 실패 시 지수 백오프 ━━━")
	fmt.Println()

	backoff := DefaultBackoff()
	fmt.Printf("  설정: base=%v, max=%v, multiplier=%.1f, jitter=%.1f\n",
		backoff.BaseDelay, backoff.MaxDelay, backoff.Multiplier, backoff.Jitter)
	fmt.Println()

	fmt.Println("  시도  대기 시간")
	fmt.Println("  ───  ─────────")
	for attempt := 0; attempt < 10; attempt++ {
		delay := backoff.NextDelay(attempt)
		bar := strings.Repeat("▓", int(delay.Milliseconds()/100))
		fmt.Printf("  #%-3d  %8v  %s\n", attempt+1, delay.Round(time.Millisecond), bar)
	}

	// 채널 정리
	close(notifications)
	<-done

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - gRPC 스트리밍으로 실시간 Peer 변경 알림")
	fmt.Println("  - PeerManager: 동시성 안전한 Peer 풀 관리 (sync.RWMutex)")
	fmt.Println("  - 연결 상태 머신: IDLE → CONNECTING → READY / TRANSIENT_FAILURE")
	fmt.Println("  - 지수 백오프: 재연결 시 서버 과부하 방지")
	fmt.Println("  - TLS ServerName: Peer 인증에 사용")
}
