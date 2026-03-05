package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// =============================================================================
// Hubble Peer Discovery 시뮬레이션
//
// 실제 구현 참조:
//   cilium/pkg/hubble/peer/service.go  - Service.Notify()
//   cilium/pkg/hubble/peer/handler.go  - handler (NodeHandler 구현)
//   cilium/pkg/hubble/peer/buffer.go   - buffer (Push/Pop, 최대 크기 제한)
//
// 핵심 개념:
//   1. handler: NodeHandler 인터페이스 구현, NodeAdd/Update/Delete → ChangeNotification
//   2. buffer: 고정 크기 버퍼, Push/Pop (조건변수 기반 블로킹)
//   3. Service.Notify: handler → buffer → stream.Send 파이프라인
//   4. 느린 클라이언트 보호: 버퍼 초과 시 ErrStreamSendBlocked
//   5. TLS 서버 이름: nodeName.clusterName.hubble-grpc.cilium.io
// =============================================================================

// --- ChangeNotification ---
// 실제: peerpb.ChangeNotification

type ChangeNotificationType int

const (
	PeerAdded   ChangeNotificationType = iota
	PeerUpdated
	PeerDeleted
)

func (t ChangeNotificationType) String() string {
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

type TLSInfo struct {
	ServerName string
}

type ChangeNotification struct {
	Name    string
	Address string
	Type    ChangeNotificationType
	TLS     *TLSInfo
}

// --- Node 모델 ---
// 실제: nodeTypes.Node

type Node struct {
	Name    string
	Cluster string
	IP      string
}

func (n Node) Fullname() string {
	if n.Cluster != "" {
		return n.Cluster + "/" + n.Name
	}
	return n.Name
}

// --- handler ---
// 실제: peer.handler
// NodeHandler 인터페이스를 구현하여 노드 매니저로부터 이벤트 수신
// 채널 C는 unbuffered → 수신측이 준비되어야 전달 가능

type handler struct {
	stop       chan struct{}
	C          chan *ChangeNotification // unbuffered 채널
	hubblePort int
}

func newHandler(hubblePort int) *handler {
	return &handler{
		stop:       make(chan struct{}),
		C:          make(chan *ChangeNotification), // unbuffered
		hubblePort: hubblePort,
	}
}

// newChangeNotification: Node → ChangeNotification 변환
// 실제: handler.newChangeNotification()
func (h *handler) newChangeNotification(n Node, t ChangeNotificationType) *ChangeNotification {
	addr := n.IP
	if h.hubblePort != 0 {
		addr = fmt.Sprintf("%s:%d", n.IP, h.hubblePort)
	}

	// TLS 서버 이름 생성
	// 실제: peer.TLSServerName()
	// 형식: nodeName.clusterName.hubble-grpc.cilium.io
	serverName := ""
	if n.Name != "" {
		cluster := n.Cluster
		if cluster == "" {
			cluster = "default"
		}
		serverName = fmt.Sprintf("%s.%s.hubble-grpc.cilium.io", n.Name, cluster)
	}

	return &ChangeNotification{
		Name:    n.Fullname(),
		Address: addr,
		Type:    t,
		TLS:     &TLSInfo{ServerName: serverName},
	}
}

// NodeAdd: 노드 추가 시 호출
// 실제: handler.NodeAdd()
func (h *handler) NodeAdd(n Node) {
	cn := h.newChangeNotification(n, PeerAdded)
	select {
	case h.C <- cn: // unbuffered → 수신측이 받을 때까지 블로킹
	case <-h.stop:
	}
}

// NodeUpdate: 노드 업데이트 시 호출
// 실제: handler.NodeUpdate()
// 이름이 같고 주소가 같으면 알림 불필요
// 이름이 변경되면 old DELETE + new ADD
func (h *handler) NodeUpdate(old, new Node) {
	if old.Fullname() == new.Fullname() {
		if old.IP == new.IP {
			// 동일 피어, 변경 없음 → 알림 불필요
			return
		}
		// 주소만 변경됨
		cn := h.newChangeNotification(new, PeerUpdated)
		select {
		case h.C <- cn:
		case <-h.stop:
		}
		return
	}
	// 이름이 변경됨 → old DELETE + new ADD
	oldCn := h.newChangeNotification(old, PeerDeleted)
	select {
	case h.C <- oldCn:
	case <-h.stop:
		return
	}
	newCn := h.newChangeNotification(new, PeerAdded)
	select {
	case h.C <- newCn:
	case <-h.stop:
	}
}

// NodeDelete: 노드 삭제 시 호출
// 실제: handler.NodeDelete()
func (h *handler) NodeDelete(n Node) {
	cn := h.newChangeNotification(n, PeerDeleted)
	select {
	case h.C <- cn:
	case <-h.stop:
	}
}

// Close: handler 리소스 해제
func (h *handler) Close() {
	close(h.stop)
}

// --- buffer ---
// 실제: peer.buffer
// ChangeNotification을 버퍼링하여 느린 클라이언트를 보호
// Push: 버퍼 풀이면 에러 반환 (느린 클라이언트 감지)
// Pop: 비어있으면 새 알림이 올 때까지 블로킹

// ErrStreamSendBlocked: 느린 클라이언트 감지 에러
// 실제: peer.ErrStreamSendBlocked
var ErrStreamSendBlocked = errors.New("server stream send was blocked for too long")

type buffer struct {
	max    int
	buf    []*ChangeNotification
	mu     sync.Mutex
	notify chan struct{} // Pop이 대기할 때 알림 채널
	stop   chan struct{}
}

// newBuffer: 최대 크기 max의 버퍼 생성
// 실제: peer.newBuffer()
func newBuffer(max int) *buffer {
	return &buffer{
		max:  max,
		stop: make(chan struct{}),
	}
}

// Push: 알림을 버퍼에 추가
// 실제: buffer.Push()
// 버퍼가 가득 차면 에러 반환 → 느린 클라이언트 연결 종료
func (b *buffer) Push(cn *ChangeNotification) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	select {
	case <-b.stop:
		return errors.New("buffer closed")
	default:
		if len(b.buf) == b.max {
			return fmt.Errorf("max buffer size=%d reached", b.max)
		}
	}

	b.buf = append(b.buf, cn)

	// Pop에서 대기 중인 goroutine에 알림
	if b.notify != nil {
		close(b.notify)
		b.notify = nil
	}
	return nil
}

// Pop: 버퍼에서 첫 번째 요소를 제거하고 반환
// 실제: buffer.Pop()
// 비어있으면 새 알림이 Push될 때까지 블로킹
func (b *buffer) Pop() (*ChangeNotification, error) {
	b.mu.Lock()

	if len(b.buf) == 0 {
		// 비어있으면 알림 채널 생성 후 대기
		if b.notify == nil {
			b.notify = make(chan struct{})
		}
		notify := b.notify
		b.mu.Unlock()

		select {
		case <-notify:
			b.mu.Lock()
		case <-b.stop:
			return nil, io.EOF
		}
	}

	// 버퍼가 닫혔는지 재확인
	select {
	case <-b.stop:
		b.mu.Unlock()
		return nil, io.EOF
	default:
	}

	cn := b.buf[0]
	b.buf[0] = nil // GC 지원
	b.buf = b.buf[1:]
	b.mu.Unlock()
	return cn, nil
}

// Len: 현재 버퍼 크기
func (b *buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// Close: 버퍼 닫기
// 실제: buffer.Close()
func (b *buffer) Close() {
	close(b.stop)
	b.mu.Lock()
	b.buf = nil
	b.mu.Unlock()
}

// --- NodeManager (시뮬레이션) ---
// 실제: node/manager.Manager
// handler를 Subscribe하면 기존 노드 정보를 즉시 전달하고,
// 이후 변경사항을 handler에 알려줌

type NodeHandler interface {
	NodeAdd(n Node)
	NodeUpdate(old, new Node)
	NodeDelete(n Node)
}

type NodeManager struct {
	mu       sync.Mutex
	nodes    map[string]Node
	handlers []NodeHandler
}

func NewNodeManager() *NodeManager {
	return &NodeManager{
		nodes: make(map[string]Node),
	}
}

func (m *NodeManager) AddNode(n Node) {
	m.mu.Lock()
	m.nodes[n.Fullname()] = n
	handlers := make([]NodeHandler, len(m.handlers))
	copy(handlers, m.handlers)
	m.mu.Unlock()

	for _, h := range handlers {
		h.NodeAdd(n)
	}
}

func (m *NodeManager) UpdateNode(old, new Node) {
	m.mu.Lock()
	delete(m.nodes, old.Fullname())
	m.nodes[new.Fullname()] = new
	handlers := make([]NodeHandler, len(m.handlers))
	copy(handlers, m.handlers)
	m.mu.Unlock()

	for _, h := range handlers {
		h.NodeUpdate(old, new)
	}
}

func (m *NodeManager) DeleteNode(n Node) {
	m.mu.Lock()
	delete(m.nodes, n.Fullname())
	handlers := make([]NodeHandler, len(m.handlers))
	copy(handlers, m.handlers)
	m.mu.Unlock()

	for _, h := range handlers {
		h.NodeDelete(n)
	}
}

// Subscribe: 핸들러를 등록하고 기존 노드를 즉시 알림
// 실제: manager.Subscribe(handler)
func (m *NodeManager) Subscribe(h NodeHandler) {
	m.mu.Lock()
	m.handlers = append(m.handlers, h)
	// 기존 노드 정보를 즉시 전달
	nodes := make([]Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		nodes = append(nodes, n)
	}
	m.mu.Unlock()

	for _, n := range nodes {
		h.NodeAdd(n)
	}
}

func (m *NodeManager) Unsubscribe(h NodeHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, handler := range m.handlers {
		if handler == h {
			m.handlers = append(m.handlers[:i], m.handlers[i+1:]...)
			return
		}
	}
}

// --- Service ---
// 실제: peer.Service
// Notify RPC: handler → buffer → stream.Send 파이프라인

type PeerService struct {
	stop           chan struct{}
	notifier       *NodeManager
	maxBufferSize  int
}

func NewPeerService(notifier *NodeManager, maxBufferSize int) *PeerService {
	return &PeerService{
		stop:          make(chan struct{}),
		notifier:      notifier,
		maxBufferSize: maxBufferSize,
	}
}

// Notify: 클라이언트에게 피어 변경 알림 스트리밍
// 실제: Service.Notify()
//
// 파이프라인 구조:
//   goroutine 1: handler.C → buffer.Push (handler 채널에서 읽어 버퍼에 쓰기)
//   goroutine 2: buffer.Pop → sendFn (버퍼에서 읽어 클라이언트에 전송)
//   goroutine 3: stop 신호 감시
func (s *PeerService) Notify(ctx context.Context, sendFn func(*ChangeNotification) error) error {
	ctx, cancel := context.WithCancel(ctx)

	h := newHandler(4244)
	buf := newBuffer(s.maxBufferSize)

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	// goroutine 1: stop 신호 감시
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer h.Close()
		select {
		case <-s.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	// goroutine 2: handler.C → buffer.Push
	// 실제: handler 채널에서 읽어 버퍼에 쓰기
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer buf.Close()
		for {
			select {
			case cn, ok := <-h.C:
				if !ok {
					return
				}
				if err := buf.Push(cn); err != nil {
					errCh <- ErrStreamSendBlocked
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// goroutine 3: buffer.Pop → sendFn
	// 실제: buffer에서 읽어 클라이언트에 전송
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			cn, err := buf.Pop()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				errCh <- err
				cancel()
				return
			}
			if err := sendFn(cn); err != nil {
				errCh <- err
				cancel()
				return
			}
		}
	}()

	// NodeManager에 구독 등록
	s.notifier.Subscribe(h)
	defer s.notifier.Unsubscribe(h)

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *PeerService) Close() {
	close(s.stop)
}

func main() {
	fmt.Println("=== Hubble Peer Discovery 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: cilium/pkg/hubble/peer/service.go  - Service.Notify()")
	fmt.Println("참조: cilium/pkg/hubble/peer/handler.go  - handler (NodeAdd/Update/Delete)")
	fmt.Println("참조: cilium/pkg/hubble/peer/buffer.go   - buffer (Push/Pop)")
	fmt.Println()

	// === 테스트 1: handler 기본 동작 ===
	fmt.Println("--- 테스트 1: handler 기본 동작 (Node → ChangeNotification) ---")
	fmt.Println()

	h := newHandler(4244)
	go func() {
		h.NodeAdd(Node{Name: "worker-1", Cluster: "prod", IP: "10.0.0.1"})
		h.NodeAdd(Node{Name: "worker-2", Cluster: "prod", IP: "10.0.0.2"})
	}()

	for i := 0; i < 2; i++ {
		cn := <-h.C
		fmt.Printf("  [수신] %s: name=%s, addr=%s, tls=%s\n",
			cn.Type, cn.Name, cn.Address, cn.TLS.ServerName)
	}
	h.Close()
	fmt.Println()

	// === 테스트 2: NodeUpdate 동작 ===
	fmt.Println("--- 테스트 2: NodeUpdate (주소 변경 / 이름 변경) ---")
	fmt.Println()

	h2 := newHandler(4244)

	go func() {
		// 주소만 변경 → PEER_UPDATED
		h2.NodeUpdate(
			Node{Name: "worker-1", Cluster: "prod", IP: "10.0.0.1"},
			Node{Name: "worker-1", Cluster: "prod", IP: "10.0.0.100"},
		)
		// 이름 변경 → old DELETE + new ADD
		h2.NodeUpdate(
			Node{Name: "worker-old", Cluster: "prod", IP: "10.0.0.2"},
			Node{Name: "worker-new", Cluster: "prod", IP: "10.0.0.2"},
		)
	}()

	for i := 0; i < 3; i++ {
		cn := <-h2.C
		fmt.Printf("  [수신] %s: name=%s, addr=%s\n", cn.Type, cn.Name, cn.Address)
	}
	h2.Close()
	fmt.Println()

	// === 테스트 3: buffer Push/Pop ===
	fmt.Println("--- 테스트 3: buffer Push/Pop (블로킹 Pop) ---")
	fmt.Println()

	buf := newBuffer(5)

	// Push 3개
	for i := 0; i < 3; i++ {
		cn := &ChangeNotification{
			Name: fmt.Sprintf("node-%d", i),
			Type: PeerAdded,
		}
		err := buf.Push(cn)
		fmt.Printf("  Push: %s (err=%v, len=%d)\n", cn.Name, err, buf.Len())
	}

	// Pop 3개
	for i := 0; i < 3; i++ {
		cn, err := buf.Pop()
		fmt.Printf("  Pop: %s (err=%v, len=%d)\n", cn.Name, err, buf.Len())
	}
	fmt.Println()

	// === 테스트 4: 느린 클라이언트 보호 ===
	fmt.Println("--- 테스트 4: 느린 클라이언트 보호 (버퍼 오버플로우) ---")
	fmt.Println()

	smallBuf := newBuffer(3) // 최대 3개

	for i := 0; i < 4; i++ {
		cn := &ChangeNotification{
			Name: fmt.Sprintf("node-%d", i),
			Type: PeerAdded,
		}
		err := smallBuf.Push(cn)
		if err != nil {
			fmt.Printf("  Push: %s -> 오류: %v (느린 클라이언트 감지)\n", cn.Name, err)
		} else {
			fmt.Printf("  Push: %s -> 성공 (len=%d)\n", cn.Name, smallBuf.Len())
		}
	}
	smallBuf.Close()
	fmt.Println()

	// === 테스트 5: 전체 Notify 파이프라인 ===
	fmt.Println("--- 테스트 5: 전체 Notify 파이프라인 ---")
	fmt.Println("  NodeManager → handler → buffer → sendFn")
	fmt.Println()

	mgr := NewNodeManager()

	// 기존 노드 2개 미리 등록
	mgr.AddNode(Node{Name: "master-0", Cluster: "prod", IP: "10.0.0.10"})
	mgr.AddNode(Node{Name: "worker-0", Cluster: "prod", IP: "10.0.0.20"})

	svc := NewPeerService(mgr, 10)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 수신된 알림 수집
	var received []*ChangeNotification
	var mu sync.Mutex

	// Notify 시작 (별도 goroutine)
	notifyDone := make(chan error, 1)
	go func() {
		notifyDone <- svc.Notify(ctx, func(cn *ChangeNotification) error {
			mu.Lock()
			received = append(received, cn)
			mu.Unlock()
			fmt.Printf("  [Notify] %s: name=%s, addr=%s\n", cn.Type, cn.Name, cn.Address)
			return nil
		})
	}()

	// Subscribe 후 기존 노드 알림이 올 때까지 대기
	time.Sleep(500 * time.Millisecond)

	// 새 노드 추가
	fmt.Println()
	fmt.Println("  --- 새 노드 이벤트 발생 ---")
	mgr.AddNode(Node{Name: "worker-1", Cluster: "prod", IP: "10.0.0.30"})
	time.Sleep(100 * time.Millisecond)

	// 노드 주소 업데이트
	mgr.UpdateNode(
		Node{Name: "worker-0", Cluster: "prod", IP: "10.0.0.20"},
		Node{Name: "worker-0", Cluster: "prod", IP: "10.0.0.21"},
	)
	time.Sleep(100 * time.Millisecond)

	// 노드 삭제
	mgr.DeleteNode(Node{Name: "worker-1", Cluster: "prod", IP: "10.0.0.30"})
	time.Sleep(100 * time.Millisecond)

	cancel()
	<-notifyDone

	mu.Lock()
	fmt.Printf("\n  총 수신된 알림: %d개\n", len(received))
	mu.Unlock()
	fmt.Println()

	// === 테스트 6: TLS 서버 이름 생성 ===
	fmt.Println("--- 테스트 6: TLS 서버 이름 생성 ---")
	fmt.Println()

	testH := newHandler(4244)
	testCases := []Node{
		{Name: "moseisley", Cluster: "tatooine", IP: "10.0.0.1"},
		{Name: "worker.node.1", Cluster: "prod.cluster", IP: "10.0.0.2"},
		{Name: "simple", Cluster: "", IP: "10.0.0.3"},
	}

	for _, n := range testCases {
		cn := testH.newChangeNotification(n, PeerAdded)
		fmt.Printf("  node=%q, cluster=%q → TLS ServerName=%q\n",
			n.Name, n.Cluster, cn.TLS.ServerName)
	}
	testH.Close()
	fmt.Println()

	// === 테스트 7: 동시성 안전성 ===
	fmt.Println("--- 테스트 7: 동시성 안전성 (버퍼 동시 접근) ---")
	fmt.Println()

	concBuf := newBuffer(100)
	var wg sync.WaitGroup

	// 동시 Push
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				concBuf.Push(&ChangeNotification{
					Name: fmt.Sprintf("node-%d-%d", id, j),
					Type: PeerAdded,
				})
			}
		}(i)
	}

	// 동시 Pop
	popCount := 0
	var popMu sync.Mutex
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				cn, err := concBuf.Pop()
				if err != nil {
					return
				}
				_ = cn
				popMu.Lock()
				popCount++
				popMu.Unlock()
			}
		}()
	}

	wg.Wait()
	popMu.Lock()
	fmt.Printf("  Push 100개, Pop %d개, 남은 버퍼: %d개 (데이터 경합 없음)\n", popCount, concBuf.Len())
	popMu.Unlock()
	concBuf.Close()
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. handler: NodeHandler 인터페이스 구현 (NodeAdd/Update/Delete)")
	fmt.Println("     → unbuffered 채널로 ChangeNotification 전달")
	fmt.Println("  2. buffer: 최대 크기 제한, 초과 시 ErrStreamSendBlocked")
	fmt.Println("     → Pop은 비어있으면 블로킹 (조건변수 패턴)")
	fmt.Println("  3. Service.Notify: 3개 goroutine 파이프라인")
	fmt.Println("     → handler.C → buffer → stream.Send")
	fmt.Println("  4. NodeUpdate: 이름 변경 → DELETE + ADD, 주소 변경 → UPDATED")
	fmt.Println("  5. Subscribe 시 기존 노드 정보 즉시 전달 (초기 동기화)")
	fmt.Println("  6. TLS: nodeName.clusterName.hubble-grpc.cilium.io 형식")
}
