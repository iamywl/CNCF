package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Hubble gRPC 스트리밍 시뮬레이션
//
// 실제 구현 참조:
//   cilium/pkg/hubble/observer/local_observer.go - GetFlows() 메서드
//   cilium/pkg/hubble/container/ring_reader.go   - NextFollow(), Next()
//
// 핵심 개념:
//   1. 서버 스트리밍 RPC: 클라이언트 1번 요청 → 서버가 연속적으로 Flow 전송
//   2. Follow 모드: 새 이벤트가 올 때까지 블로킹 (ring_reader.NextFollow)
//   3. Context Cancellation: 클라이언트 종료 시 서버 스트림 정리
//   4. Number 제한: --last N 플래그로 최대 반환 수 제한
// =============================================================================

// --- Flow 데이터 모델 ---

// Verdict는 Flow의 판정 결과
type Verdict string

const (
	VerdictForwarded Verdict = "FORWARDED"
	VerdictDropped   Verdict = "DROPPED"
	VerdictAudit     Verdict = "AUDIT"
)

// Flow는 네트워크 이벤트 하나를 나타낸다
type Flow struct {
	Time        time.Time `json:"time"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	Verdict     Verdict   `json:"verdict"`
	Type        string    `json:"type"`
	Summary     string    `json:"summary"`
	NodeName    string    `json:"node_name"`
}

// GetFlowsRequest는 클라이언트의 Flow 조회 요청
// 실제: observerpb.GetFlowsRequest
type GetFlowsRequest struct {
	Number uint64 `json:"number"` // 최대 반환 개수 (--last)
	Follow bool   `json:"follow"` // follow 모드 (--follow)
	First  bool   `json:"first"`  // 처음부터 읽기 (--first)
}

// GetFlowsResponse는 서버가 스트리밍하는 응답
// 실제: observerpb.GetFlowsResponse
type GetFlowsResponse struct {
	Flow     *Flow  `json:"flow,omitempty"`
	NodeName string `json:"node_name"`
	Time     string `json:"time"`
}

// --- 링 버퍼 (간소화) ---

// Ring은 Flow를 저장하는 원형 버퍼
// 실제: container.Ring
type Ring struct {
	mu        sync.RWMutex
	data      []*Flow
	write     uint64
	cap       uint64
	notifyChs []chan struct{} // follow 모드 알림 채널
}

func NewRing(capacity uint64) *Ring {
	return &Ring{
		data: make([]*Flow, capacity),
		cap:  capacity,
	}
}

// Write는 Flow를 링 버퍼에 쓴다
// 실제: container.Ring.Write()
func (r *Ring) Write(f *Flow) {
	r.mu.Lock()
	idx := r.write % r.cap
	r.data[idx] = f
	r.write++
	// follow 모드의 대기자들에게 알림
	for _, ch := range r.notifyChs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	r.mu.Unlock()
}

func (r *Ring) LastWrite() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.write == 0 {
		return 0
	}
	return r.write - 1
}

func (r *Ring) OldestWrite() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.write <= r.cap {
		return 0
	}
	return r.write - r.cap
}

func (r *Ring) Read(idx uint64) (*Flow, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.write == 0 {
		return nil, fmt.Errorf("EOF: 버퍼 비어있음")
	}
	if idx >= r.write {
		return nil, fmt.Errorf("EOF: 아직 쓰여지지 않은 위치")
	}
	oldest := uint64(0)
	if r.write > r.cap {
		oldest = r.write - r.cap
	}
	if idx < oldest {
		return nil, fmt.Errorf("ErrInvalidRead: 이미 덮어쓰여진 위치")
	}
	return r.data[idx%r.cap], nil
}

func (r *Ring) RegisterNotify() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan struct{}, 1)
	r.notifyChs = append(r.notifyChs, ch)
	return ch
}

// --- RingReader ---
// 실제: container.RingReader

type RingReader struct {
	ring *Ring
	idx  uint64
}

func NewRingReader(ring *Ring, start uint64) *RingReader {
	return &RingReader{ring: ring, idx: start}
}

// Next는 현재 위치의 이벤트를 읽고 포인터를 전진시킨다
// 실제: container.RingReader.Next()
func (r *RingReader) Next() (*Flow, error) {
	f, err := r.ring.Read(r.idx)
	if err != nil {
		return nil, err
	}
	r.idx++
	return f, nil
}

// NextFollow는 이벤트가 없으면 새 이벤트가 올 때까지 블로킹
// 실제: container.RingReader.NextFollow()
// ring.readFrom() 내부에서 conditional variable로 대기
func (r *RingReader) NextFollow(ctx context.Context) *Flow {
	// 먼저 즉시 읽을 수 있는지 확인
	f, err := r.ring.Read(r.idx)
	if err == nil {
		r.idx++
		return f
	}

	// 읽을 수 없으면 알림 대기
	notify := r.ring.RegisterNotify()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-notify:
			f, err := r.ring.Read(r.idx)
			if err == nil {
				r.idx++
				return f
			}
			// 아직 읽을 수 없으면 계속 대기
		}
	}
}

// --- Observer 서버 ---
// 실제: LocalObserverServer

type ObserverServer struct {
	ring             *Ring
	events           chan *Flow
	numObservedFlows atomic.Uint64
	startTime        time.Time
}

func NewObserverServer(maxFlows uint64, monitorBuffer int) *ObserverServer {
	return &ObserverServer{
		ring:      NewRing(maxFlows),
		events:    make(chan *Flow, monitorBuffer),
		startTime: time.Now(),
	}
}

// Start는 이벤트 채널에서 Flow를 읽어 링 버퍼에 쓴다
// 실제: LocalObserverServer.Start()
func (s *ObserverServer) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case flow, ok := <-s.events:
			if !ok {
				return
			}
			s.numObservedFlows.Add(1)
			s.ring.Write(flow)
		}
	}
}

// GetFlows는 서버 스트리밍 RPC를 시뮬레이션
// 실제: LocalObserverServer.GetFlows()
//
// 핵심 흐름:
// 1. 요청 검증 (first + follow 동시 불가)
// 2. RingReader 생성 (시작 위치 계산)
// 3. 이벤트 루프: Next/NextFollow로 읽고 → Send로 전송
func (s *ObserverServer) GetFlows(ctx context.Context, req *GetFlowsRequest, sendFn func(*GetFlowsResponse) error) error {
	// 요청 검증
	// 실제: validateRequest()
	if req.First && req.Follow {
		return fmt.Errorf("first cannot be specified with follow")
	}

	// RingReader 시작 위치 결정
	// 실제: newRingReader()
	var reader *RingReader
	if req.First {
		reader = NewRingReader(s.ring, s.ring.OldestWrite())
	} else if req.Follow && req.Number == 0 {
		reader = NewRingReader(s.ring, s.ring.LastWrite()+1)
	} else {
		// --last N: 뒤에서부터 N개
		start := s.ring.LastWrite() + 1
		if req.Number > 0 && start >= req.Number {
			start = start - req.Number
		} else {
			start = s.ring.OldestWrite()
		}
		reader = NewRingReader(s.ring, start)
	}

	// 이벤트 읽기 루프
	// 실제: GetFlows의 nextEvent 루프
	var eventCount uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var flow *Flow
		if req.Follow {
			// Follow 모드: 새 이벤트까지 블로킹 대기
			// 실제: eventsReader.Next() → ringReader.NextFollow(ctx)
			flow = reader.NextFollow(ctx)
			if flow == nil {
				return nil // context cancelled
			}
		} else {
			// 비 Follow 모드: 최대 개수까지만 읽기
			if req.Number > 0 && eventCount >= req.Number {
				return nil // 요청된 개수 전달 완료
			}
			var err error
			flow, err = reader.Next()
			if err != nil {
				return nil // EOF - 더 이상 읽을 이벤트 없음
			}
		}

		eventCount++
		resp := &GetFlowsResponse{
			Flow:     flow,
			NodeName: flow.NodeName,
			Time:     flow.Time.Format(time.RFC3339Nano),
		}

		if err := sendFn(resp); err != nil {
			return err
		}
	}
}

// --- TCP 기반 스트리밍 서버/클라이언트 ---

// handleConnection은 TCP 연결 하나를 처리 (gRPC 서버 스트리밍 시뮬레이션)
func handleConnection(ctx context.Context, conn net.Conn, server *ObserverServer) {
	defer conn.Close()

	// 요청 읽기 (실제: gRPC 프레임 디코딩)
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}

	var req GetFlowsRequest
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		fmt.Fprintf(conn, "ERROR: %v\n", err)
		return
	}
	fmt.Printf("[서버] 요청 수신: follow=%v, number=%d, first=%v\n", req.Follow, req.Number, req.First)

	// 연결별 context (클라이언트 종료 시 취소)
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 클라이언트 연결 종료 감지
	go func() {
		buf := make([]byte, 1)
		for {
			_, err := conn.Read(buf)
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// 서버 스트리밍: Flow를 연속적으로 전송
	encoder := json.NewEncoder(conn)
	err := server.GetFlows(connCtx, &req, func(resp *GetFlowsResponse) error {
		return encoder.Encode(resp)
	})
	if err != nil {
		fmt.Printf("[서버] 스트림 종료: %v\n", err)
	}
}

// --- Flow 생성기 ---

func generateFlows(ctx context.Context, events chan<- *Flow) {
	namespaces := []string{"default", "kube-system", "monitoring", "prod"}
	services := []string{"frontend", "backend", "db", "cache", "api-gateway"}
	verdicts := []Verdict{VerdictForwarded, VerdictForwarded, VerdictForwarded, VerdictDropped, VerdictAudit}
	types := []string{"L3/L4", "L7/HTTP", "L7/DNS", "to-endpoint", "from-endpoint"}

	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		srcNs := namespaces[rand.Intn(len(namespaces))]
		dstNs := namespaces[rand.Intn(len(namespaces))]
		flow := &Flow{
			Time:        time.Now(),
			Source:      fmt.Sprintf("%s/%s-%d", srcNs, services[rand.Intn(len(services))], rand.Intn(3)),
			Destination: fmt.Sprintf("%s/%s-%d", dstNs, services[rand.Intn(len(services))], rand.Intn(3)),
			Verdict:     verdicts[rand.Intn(len(verdicts))],
			Type:        types[rand.Intn(len(types))],
			Summary:     fmt.Sprintf("TCP SYN seq=%d", rand.Intn(10000)),
			NodeName:    fmt.Sprintf("node-%d", rand.Intn(3)),
		}

		events <- flow
		time.Sleep(time.Duration(100+rand.Intn(200)) * time.Millisecond)
	}
}

// --- 클라이언트 ---

func runClient(addr string, req GetFlowsRequest, duration time.Duration) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Printf("[클라이언트] 연결 실패: %v\n", err)
		return
	}
	defer conn.Close()

	// 요청 전송
	reqBytes, _ := json.Marshal(req)
	fmt.Fprintf(conn, "%s\n", reqBytes)

	// 응답 스트림 수신 (타임아웃 설정)
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	scanner := bufio.NewScanner(conn)
	count := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			fmt.Printf("[클라이언트] 타임아웃 (%v), 총 %d개 Flow 수신\n", duration, count)
			return
		default:
		}

		var resp GetFlowsResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			continue
		}
		count++
		if resp.Flow != nil {
			fmt.Printf("[클라이언트] #%d %s: %s -> %s [%s] %s (%s)\n",
				count,
				resp.Flow.Time.Format("15:04:05.000"),
				resp.Flow.Source,
				resp.Flow.Destination,
				resp.Flow.Verdict,
				resp.Flow.Type,
				resp.Flow.Summary,
			)
		}
	}
	fmt.Printf("[클라이언트] 스트림 종료, 총 %d개 Flow 수신\n", count)
}

func main() {
	fmt.Println("=== Hubble gRPC 스트리밍 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: cilium/pkg/hubble/observer/local_observer.go - GetFlows()")
	fmt.Println("참조: cilium/pkg/hubble/container/ring_reader.go - NextFollow()")
	fmt.Println()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 서버 생성
	server := NewObserverServer(1024, 100)

	// 이벤트 처리 고루틴 시작 (실제: LocalObserverServer.Start())
	go server.Start(ctx)

	// Flow 생성기 시작
	go generateFlows(ctx, server.events)

	// 사전에 일부 Flow 적재 대기
	time.Sleep(2 * time.Second)

	// TCP 서버 시작
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("리스너 생성 실패: %v\n", err)
		return
	}
	defer listener.Close()
	addr := listener.Addr().String()
	fmt.Printf("[서버] %s에서 대기 중\n\n", addr)

	// 서버 연결 수락 고루틴
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleConnection(ctx, conn, server)
		}
	}()

	// === 테스트 1: --last 5 (최근 5개만) ===
	fmt.Println("--- 테스트 1: --last 5 (최근 5개 Flow 조회) ---")
	fmt.Println("  실제 CLI: hubble observe --last 5")
	runClient(addr, GetFlowsRequest{Number: 5, Follow: false}, 5*time.Second)
	fmt.Println()

	time.Sleep(500 * time.Millisecond)

	// === 테스트 2: --follow (스트리밍 모드, 3초 후 종료) ===
	fmt.Println("--- 테스트 2: --follow (실시간 스트리밍, 3초) ---")
	fmt.Println("  실제 CLI: hubble observe --follow")
	runClient(addr, GetFlowsRequest{Follow: true}, 3*time.Second)
	fmt.Println()

	time.Sleep(500 * time.Millisecond)

	// === 테스트 3: --follow --last 3 (최근 3개 + 이후 스트리밍, 3초) ===
	fmt.Println("--- 테스트 3: --follow --last 3 (최근 3개 + 실시간 스트리밍, 3초) ---")
	fmt.Println("  실제 CLI: hubble observe --follow --last 3")
	runClient(addr, GetFlowsRequest{Follow: true, Number: 3}, 3*time.Second)
	fmt.Println()

	cancel()
	time.Sleep(200 * time.Millisecond)

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. 서버 스트리밍: 클라이언트 1회 요청 → 서버가 지속적으로 Flow 전송")
	fmt.Println("  2. Follow 모드: NextFollow()로 새 이벤트 대기 (블로킹)")
	fmt.Println("  3. Number 제한: --last N으로 반환 개수 제한")
	fmt.Println("  4. Context 취소: 클라이언트 종료 시 서버 스트림도 정리")
}
