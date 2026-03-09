// gRPC Health Check 시뮬레이션
//
// 이 PoC는 gRPC Health Checking Protocol의 핵심 메커니즘을 시뮬레이션한다:
//   1. Health Server — 서비스별 상태 관리 (statusMap + Watch 구독)
//   2. Check RPC — 동기식 상태 조회
//   3. Watch RPC — 스트리밍 상태 구독 (Fan-out 패턴)
//   4. 클라이언트 Health Check — 상태 기반 연결 상태 전이
//   5. Shutdown/Resume — 그레이스풀 생명주기 관리
//
// 실제 코드 참조:
//   - health/server.go      — Server 구조체, Check/Watch/SetServingStatus
//   - health/client.go      — clientHealthCheck 루프
//   - health/producer.go    — 로드 밸런서 통합
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────
// 1. 상태 정의
// ─────────────────────────────────────────────────

// ServingStatus는 gRPC Health Checking Protocol의 상태 열거형이다.
// 실제 코드: health/grpc_health_v1/health.pb.go:41-48
type ServingStatus int32

const (
	StatusUnknown        ServingStatus = 0 // 초기/미정의
	StatusServing        ServingStatus = 1 // 정상 동작
	StatusNotServing     ServingStatus = 2 // 서비스 중단
	StatusServiceUnknown ServingStatus = 3 // Watch 전용: 서비스 미등록
)

func (s ServingStatus) String() string {
	switch s {
	case StatusUnknown:
		return "UNKNOWN"
	case StatusServing:
		return "SERVING"
	case StatusNotServing:
		return "NOT_SERVING"
	case StatusServiceUnknown:
		return "SERVICE_UNKNOWN"
	default:
		return fmt.Sprintf("STATUS(%d)", s)
	}
}

// ─────────────────────────────────────────────────
// 2. Health Check 요청/응답
// ─────────────────────────────────────────────────

type HealthCheckRequest struct {
	Service string // 빈 문자열 = 전체 서버
}

type HealthCheckResponse struct {
	Status ServingStatus
}

// ─────────────────────────────────────────────────
// 3. Watch 스트림 (서버 → 클라이언트)
// ─────────────────────────────────────────────────

// WatchStream은 Watch RPC의 서버 스트리밍을 시뮬레이션한다.
type WatchStream struct {
	id     int
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan HealthCheckResponse
}

func newWatchStream(parentCtx context.Context, id int) *WatchStream {
	ctx, cancel := context.WithCancel(parentCtx)
	return &WatchStream{
		id:     id,
		ctx:    ctx,
		cancel: cancel,
		ch:     make(chan HealthCheckResponse, 1), // 버퍼=1: 최신 상태만 유지
	}
}

// Recv는 다음 상태 변화를 수신한다 (블로킹).
func (ws *WatchStream) Recv() (*HealthCheckResponse, error) {
	select {
	case resp := <-ws.ch:
		return &resp, nil
	case <-ws.ctx.Done():
		return nil, fmt.Errorf("stream canceled")
	}
}

// Close는 스트림을 종료한다.
func (ws *WatchStream) Close() {
	ws.cancel()
}

// ─────────────────────────────────────────────────
// 4. Health Server
// ─────────────────────────────────────────────────

// HealthServer는 gRPC Health Checking Protocol 서버를 시뮬레이션한다.
// 실제 코드: health/server.go:41-50
type HealthServer struct {
	mu        sync.RWMutex
	shutdown  bool
	statusMap map[string]ServingStatus
	// updates: [서비스명][스트림ID] → 상태 변화 채널 (버퍼=1)
	updates map[string]map[int]chan ServingStatus
	nextID  int
}

// NewHealthServer는 Health Server를 생성한다.
// 실제 코드: health/server.go:52-58
func NewHealthServer() *HealthServer {
	return &HealthServer{
		statusMap: map[string]ServingStatus{
			"": StatusServing, // 기본: 전체 서버 SERVING
		},
		updates: make(map[string]map[int]chan ServingStatus),
	}
}

// Check는 동기식 상태 조회 (Unary RPC).
// 실제 코드: health/server.go:60-70
func (s *HealthServer) Check(req *HealthCheckRequest) (*HealthCheckResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if status, ok := s.statusMap[req.Service]; ok {
		return &HealthCheckResponse{Status: status}, nil
	}
	return nil, fmt.Errorf("unknown service: %q", req.Service)
}

// Watch는 스트리밍 상태 구독 (Server Streaming RPC).
// 실제 코드: health/server.go:89-132
func (s *HealthServer) Watch(ctx context.Context, service string) *WatchStream {
	s.mu.Lock()

	s.nextID++
	stream := newWatchStream(ctx, s.nextID)

	// 초기 상태를 채널에 전송
	if status, ok := s.statusMap[service]; ok {
		stream.ch <- HealthCheckResponse{Status: status}
	} else {
		stream.ch <- HealthCheckResponse{Status: StatusServiceUnknown} // 미등록 서비스도 스트림 유지
	}

	// 구독 등록
	update := make(chan ServingStatus, 1) // 버퍼=1
	if _, ok := s.updates[service]; !ok {
		s.updates[service] = make(map[int]chan ServingStatus)
	}
	s.updates[service][stream.id] = update
	s.mu.Unlock()

	// Watch 이벤트 루프 (goroutine)
	go func() {
		defer func() {
			// 구독 해제
			s.mu.Lock()
			delete(s.updates[service], stream.id)
			if len(s.updates[service]) == 0 {
				delete(s.updates, service)
			}
			s.mu.Unlock()
		}()

		var lastSent ServingStatus = -1 // 중복 필터링용

		for {
			select {
			case status := <-update:
				if lastSent == status {
					continue // 중복 상태 무시
				}
				lastSent = status
				// 클라이언트에 전송 (논블로킹)
				select {
				case stream.ch <- HealthCheckResponse{Status: status}:
				default:
					// 이전 미소비 응답 폐기 후 전송
					select {
					case <-stream.ch:
					default:
					}
					stream.ch <- HealthCheckResponse{Status: status}
				}
			case <-stream.ctx.Done():
				return
			}
		}
	}()

	return stream
}

// SetServingStatus는 서비스 상태를 변경하고 모든 구독자에게 알린다.
// 실제 코드: health/server.go:136-159
func (s *HealthServer) SetServingStatus(service string, status ServingStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.shutdown {
		fmt.Printf("  [Health] 종료 상태 — %q 상태 변경 무시\n", service)
		return
	}

	s.setServingStatusLocked(service, status)
}

func (s *HealthServer) setServingStatusLocked(service string, status ServingStatus) {
	s.statusMap[service] = status

	// 모든 Watch 구독자에게 알림 (Fan-out)
	for _, update := range s.updates[service] {
		// 이전 미소비 상태 폐기 (논블로킹)
		select {
		case <-update:
		default:
		}
		// 최신 상태 전송
		update <- status
	}
}

// Shutdown은 서버를 종료 상태로 전환한다.
// 실제 코드: health/server.go:166-173
func (s *HealthServer) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.shutdown = true
	for service := range s.statusMap {
		s.setServingStatusLocked(service, StatusNotServing)
	}
}

// Resume은 서버를 정상 상태로 복구한다.
// 실제 코드: health/server.go:180-187
func (s *HealthServer) Resume() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.shutdown = false
	for service := range s.statusMap {
		s.setServingStatusLocked(service, StatusServing)
	}
}

// ─────────────────────────────────────────────────
// 5. 클라이언트 연결 상태
// ─────────────────────────────────────────────────

type ConnectivityState int

const (
	StateIdle ConnectivityState = iota
	StateConnecting
	StateReady
	StateTransientFailure
	StateShutdown
)

func (s ConnectivityState) String() string {
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
		return fmt.Sprintf("STATE(%d)", s)
	}
}

// ─────────────────────────────────────────────────
// 6. 클라이언트 Health Check
// ─────────────────────────────────────────────────

// ClientHealthChecker는 클라이언트 측 헬스체크를 시뮬레이션한다.
// 실제 코드: health/client.go:59-117
type ClientHealthChecker struct {
	serverName string
	state      ConnectivityState
	mu         sync.Mutex
	stateLog   []string
}

func NewClientHealthChecker(serverName string) *ClientHealthChecker {
	return &ClientHealthChecker{
		serverName: serverName,
		state:      StateIdle,
	}
}

func (c *ClientHealthChecker) setConnectivityState(state ConnectivityState, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.state = state
	entry := fmt.Sprintf("%s → %s (%s)", c.serverName, state, reason)
	c.stateLog = append(c.stateLog, entry)
}

func (c *ClientHealthChecker) getState() ConnectivityState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// RunHealthCheck는 Watch 스트림을 통해 서버 상태를 모니터링한다.
func (c *ClientHealthChecker) RunHealthCheck(ctx context.Context, stream *WatchStream) {
	c.setConnectivityState(StateConnecting, "Watch 스트림 시작")

	for {
		resp, err := stream.Recv()
		if err != nil {
			c.setConnectivityState(StateTransientFailure, err.Error())
			return
		}

		switch resp.Status {
		case StatusServing:
			c.setConnectivityState(StateReady, "SERVING")
		case StatusNotServing:
			c.setConnectivityState(StateTransientFailure, "NOT_SERVING")
		case StatusServiceUnknown:
			c.setConnectivityState(StateTransientFailure, "SERVICE_UNKNOWN")
		}
	}
}

// ─────────────────────────────────────────────────
// 7. 시뮬레이션
// ─────────────────────────────────────────────────

func main() {
	fmt.Println("========================================")
	fmt.Println("gRPC Health Check 시뮬레이션")
	fmt.Println("========================================")

	// ── 1. Check RPC (동기식 상태 조회) ──
	fmt.Println("\n[1] Check RPC — 동기식 상태 조회")
	fmt.Println("────────────────────────────────")

	hs := NewHealthServer()
	hs.SetServingStatus("user-service", StatusServing)
	hs.SetServingStatus("order-service", StatusNotServing)

	services := []string{"", "user-service", "order-service", "unknown-service"}
	for _, svc := range services {
		name := svc
		if name == "" {
			name = "(전체 서버)"
		}
		resp, err := hs.Check(&HealthCheckRequest{Service: svc})
		if err != nil {
			fmt.Printf("  Check(%s): 에러 — %v\n", name, err)
		} else {
			fmt.Printf("  Check(%s): %s\n", name, resp.Status)
		}
	}

	// ── 2. Watch RPC (스트리밍 상태 구독) ──
	fmt.Println("\n[2] Watch RPC — 스트리밍 상태 구독")
	fmt.Println("──────────────────────────────────")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2개 클라이언트가 동시에 Watch 구독
	checker1 := NewClientHealthChecker("Server-A")
	checker2 := NewClientHealthChecker("Server-B")

	stream1 := hs.Watch(ctx, "user-service")
	stream2 := hs.Watch(ctx, "user-service")

	go checker1.RunHealthCheck(ctx, stream1)
	go checker2.RunHealthCheck(ctx, stream2)

	time.Sleep(100 * time.Millisecond) // 초기 상태 수신 대기

	fmt.Printf("  초기 상태: Checker1=%s, Checker2=%s\n",
		checker1.getState(), checker2.getState())

	// 상태 변경 → 모든 구독자에게 Fan-out
	fmt.Println("  SetServingStatus(user-service, NOT_SERVING)")
	hs.SetServingStatus("user-service", StatusNotServing)
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  변경 후: Checker1=%s, Checker2=%s\n",
		checker1.getState(), checker2.getState())

	// 복구
	fmt.Println("  SetServingStatus(user-service, SERVING)")
	hs.SetServingStatus("user-service", StatusServing)
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  복구 후: Checker1=%s, Checker2=%s\n",
		checker1.getState(), checker2.getState())

	// ── 3. SERVICE_UNKNOWN (미등록 서비스 Watch) ──
	fmt.Println("\n[3] SERVICE_UNKNOWN — 미등록 서비스 Watch")
	fmt.Println("────────────────────────────────────────")

	checker3 := NewClientHealthChecker("Server-C")
	stream3 := hs.Watch(ctx, "new-service")
	go checker3.RunHealthCheck(ctx, stream3)
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  미등록 서비스 Watch 시: %s (스트림 유지됨!)\n", checker3.getState())

	// 나중에 서비스 등록
	fmt.Println("  SetServingStatus(new-service, SERVING)")
	hs.SetServingStatus("new-service", StatusServing)
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  서비스 등록 후: %s\n", checker3.getState())

	// ── 4. Shutdown / Resume ──
	fmt.Println("\n[4] Shutdown / Resume — 그레이스풀 생명주기")
	fmt.Println("──────────────────────────────────────────")

	resp, _ := hs.Check(&HealthCheckRequest{Service: ""})
	fmt.Printf("  Shutdown 전: 전체 서버 = %s\n", resp.Status)

	hs.Shutdown()
	time.Sleep(100 * time.Millisecond)

	resp, _ = hs.Check(&HealthCheckRequest{Service: ""})
	fmt.Printf("  Shutdown 후: 전체 서버 = %s\n", resp.Status)

	// Shutdown 상태에서 상태 변경 시도
	fmt.Println("  Shutdown 상태에서 SetServingStatus 시도:")
	hs.SetServingStatus("user-service", StatusServing)

	hs.Resume()
	time.Sleep(100 * time.Millisecond)

	resp, _ = hs.Check(&HealthCheckRequest{Service: ""})
	fmt.Printf("  Resume 후: 전체 서버 = %s\n", resp.Status)

	// ── 5. 로드 밸런서 통합 시나리오 ──
	fmt.Println("\n[5] 로드 밸런서 통합 시나리오")
	fmt.Println("───────────────────────────")

	servers := make([]*HealthServer, 3)
	checkers := make([]*ClientHealthChecker, 3)
	streams := make([]*WatchStream, 3)

	for i := 0; i < 3; i++ {
		servers[i] = NewHealthServer()
		checkers[i] = NewClientHealthChecker(fmt.Sprintf("Server-%d", i))
		streams[i] = servers[i].Watch(ctx, "")
		go checkers[i].RunHealthCheck(ctx, streams[i])
	}
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  초기: ")
	for i := 0; i < 3; i++ {
		fmt.Printf("Server-%d=%s ", i, checkers[i].getState())
	}
	fmt.Println()

	// Server-1 장애
	fmt.Println("  Server-1 장애 발생!")
	servers[1].SetServingStatus("", StatusNotServing)
	time.Sleep(100 * time.Millisecond)

	// 로드 밸런싱: Ready 서버에만 요청
	fmt.Printf("  장애 후: ")
	readyCount := 0
	for i := 0; i < 3; i++ {
		state := checkers[i].getState()
		fmt.Printf("Server-%d=%s ", i, state)
		if state == StateReady {
			readyCount++
		}
	}
	fmt.Println()

	// 트래픽 분배 시뮬레이션
	fmt.Printf("  트래픽 분배: %d개 서버에 각 %.0f%% 분배\n",
		readyCount, 100.0/float64(readyCount))

	totalRequests := 10
	requests := make([]int, 3)
	for i := 0; i < totalRequests; i++ {
		// Ready 서버에만 라우팅 (round-robin)
		for {
			idx := rand.Intn(3)
			if checkers[idx].getState() == StateReady {
				requests[idx]++
				break
			}
		}
	}

	for i := 0; i < 3; i++ {
		state := checkers[i].getState()
		if state == StateReady {
			fmt.Printf("    Server-%d: %d 요청 처리 ✓\n", i, requests[i])
		} else {
			fmt.Printf("    Server-%d: 트래픽 차단 ✗\n", i)
		}
	}

	// Server-1 복구
	fmt.Println("  Server-1 복구!")
	servers[1].SetServingStatus("", StatusServing)
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  복구 후: ")
	for i := 0; i < 3; i++ {
		fmt.Printf("Server-%d=%s ", i, checkers[i].getState())
	}
	fmt.Println()

	// 정리
	cancel()
	time.Sleep(100 * time.Millisecond)

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
