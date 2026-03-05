// poc-14-stats: gRPC 메트릭 수집 (stats.Handler) 시뮬레이션
//
// grpc-go의 stats 패키지 핵심 개념을 표준 라이브러리만으로 재현한다.
// - Handler 인터페이스 (TagRPC, HandleRPC, TagConn, HandleConn)
// - RPC 이벤트: Begin, InPayload, OutPayload, End
// - 커넥션 이벤트: ConnBegin, ConnEnd
// - context에 태그 부착하여 이벤트 간 상관관계
// - 지연 시간, 메시지 크기 수집
//
// 실제 grpc-go 소스: stats/handlers.go, stats/stats.go
package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ========== RPC 이벤트 타입 ==========
// grpc-go: stats/stats.go에 정의된 RPC 통계 이벤트들

// RPCStats는 RPC 통계 이벤트의 인터페이스이다.
type RPCStats interface {
	isRPCStats()
	String() string
}

// Begin은 RPC 시작 이벤트이다.
// grpc-go: stats/stats.go Begin struct
type Begin struct {
	BeginTime  time.Time
	IsClient   bool
	FullMethod string // /service/method 형식
}

func (b *Begin) isRPCStats() {}
func (b *Begin) String() string {
	side := "server"
	if b.IsClient {
		side = "client"
	}
	return fmt.Sprintf("Begin{method=%s, side=%s}", b.FullMethod, side)
}

// InPayload는 수신 페이로드 이벤트이다.
// grpc-go: stats/stats.go InPayload struct
type InPayload struct {
	RecvTime time.Time
	Length   int // 압축 전 크기
	WireLength int // 실제 전송 크기 (압축 후)
}

func (p *InPayload) isRPCStats() {}
func (p *InPayload) String() string {
	return fmt.Sprintf("InPayload{len=%d, wire=%d}", p.Length, p.WireLength)
}

// OutPayload는 송신 페이로드 이벤트이다.
// grpc-go: stats/stats.go OutPayload struct
type OutPayload struct {
	SentTime   time.Time
	Length     int
	WireLength int
}

func (p *OutPayload) isRPCStats() {}
func (p *OutPayload) String() string {
	return fmt.Sprintf("OutPayload{len=%d, wire=%d}", p.Length, p.WireLength)
}

// End는 RPC 종료 이벤트이다.
// grpc-go: stats/stats.go End struct
type End struct {
	EndTime time.Time
	Error   error
}

func (e *End) isRPCStats() {}
func (e *End) String() string {
	errStr := "<nil>"
	if e.Error != nil {
		errStr = e.Error.Error()
	}
	return fmt.Sprintf("End{error=%s}", errStr)
}

// ========== 커넥션 이벤트 ==========
// grpc-go: stats/stats.go ConnStats 인터페이스

type ConnStats interface {
	isConnStats()
	String() string
}

// ConnBegin은 연결 시작 이벤트이다.
type ConnBegin struct {
	IsClient bool
}

func (c *ConnBegin) isConnStats() {}
func (c *ConnBegin) String() string {
	side := "server"
	if c.IsClient {
		side = "client"
	}
	return fmt.Sprintf("ConnBegin{side=%s}", side)
}

// ConnEnd는 연결 종료 이벤트이다.
type ConnEnd struct{}

func (c *ConnEnd) isConnStats() {}
func (c *ConnEnd) String() string { return "ConnEnd{}" }

// ========== 태그 정보 ==========
// grpc-go: stats/stats.go RPCTagInfo, ConnTagInfo

type RPCTagInfo struct {
	FullMethodName string
}

type ConnTagInfo struct {
	RemoteAddr string
	LocalAddr  string
}

// ========== Handler 인터페이스 ==========
// grpc-go: stats/handlers.go 53행
// gRPC 전송 계층이 RPC/커넥션 이벤트를 발생시킬 때 호출되는 핸들러.
type Handler interface {
	TagRPC(ctx context.Context, info *RPCTagInfo) context.Context
	HandleRPC(ctx context.Context, stats RPCStats)
	TagConn(ctx context.Context, info *ConnTagInfo) context.Context
	HandleConn(ctx context.Context, stats ConnStats)
}

// ========== Context 키 ==========
type ctxKey string

const (
	rpcTagKey  ctxKey = "rpc-tag"
	connTagKey ctxKey = "conn-tag"
)

// RPCTag는 context에 저장되는 RPC 태그이다.
type RPCTag struct {
	Method    string
	StartTime time.Time
	ConnID    string
}

// ConnTag는 context에 저장되는 연결 태그이다.
type ConnTag struct {
	RemoteAddr string
	ConnID     string
}

// ========== MetricsHandler 구현 ==========
// grpc-go 사용자가 구현하는 stats.Handler의 예시.
// 실제로는 OpenTelemetry, Prometheus 등의 메트릭 시스템과 연동한다.
type MetricsHandler struct {
	mu          sync.Mutex
	rpcCount    int
	rpcErrors   int
	totalBytes  int64
	latencies   []time.Duration
	connCount   int
	events      []string
}

func NewMetricsHandler() *MetricsHandler {
	return &MetricsHandler{}
}

// TagRPC는 RPC 시작 시 context에 태그를 부착한다.
// grpc-go: stats/handlers.go — 이 메서드로 RPC 추적에 필요한 정보를 context에 넣는다.
func (h *MetricsHandler) TagRPC(ctx context.Context, info *RPCTagInfo) context.Context {
	tag := &RPCTag{
		Method:    info.FullMethodName,
		StartTime: time.Now(),
	}

	// 연결 태그에서 ConnID 가져오기
	if connTag, ok := ctx.Value(connTagKey).(*ConnTag); ok {
		tag.ConnID = connTag.ConnID
	}

	h.mu.Lock()
	h.events = append(h.events, fmt.Sprintf("TagRPC: method=%s", info.FullMethodName))
	h.mu.Unlock()

	return context.WithValue(ctx, rpcTagKey, tag)
}

// HandleRPC는 RPC 이벤트를 처리한다.
func (h *MetricsHandler) HandleRPC(ctx context.Context, s RPCStats) {
	tag, _ := ctx.Value(rpcTagKey).(*RPCTag)

	h.mu.Lock()
	defer h.mu.Unlock()

	switch v := s.(type) {
	case *Begin:
		h.rpcCount++
		h.events = append(h.events, fmt.Sprintf("HandleRPC[Begin]: %s (conn=%s)", tag.Method, tag.ConnID))

	case *InPayload:
		h.totalBytes += int64(v.WireLength)
		h.events = append(h.events, fmt.Sprintf("HandleRPC[InPayload]: %s len=%d wire=%d",
			tag.Method, v.Length, v.WireLength))

	case *OutPayload:
		h.totalBytes += int64(v.WireLength)
		h.events = append(h.events, fmt.Sprintf("HandleRPC[OutPayload]: %s len=%d wire=%d",
			tag.Method, v.Length, v.WireLength))

	case *End:
		latency := v.EndTime.Sub(tag.StartTime)
		h.latencies = append(h.latencies, latency)
		if v.Error != nil {
			h.rpcErrors++
			h.events = append(h.events, fmt.Sprintf("HandleRPC[End]: %s ERROR=%v latency=%v",
				tag.Method, v.Error, latency.Truncate(time.Microsecond)))
		} else {
			h.events = append(h.events, fmt.Sprintf("HandleRPC[End]: %s OK latency=%v",
				tag.Method, latency.Truncate(time.Microsecond)))
		}
	}
}

// TagConn은 연결 시작 시 context에 태그를 부착한다.
func (h *MetricsHandler) TagConn(ctx context.Context, info *ConnTagInfo) context.Context {
	tag := &ConnTag{
		RemoteAddr: info.RemoteAddr,
		ConnID:     fmt.Sprintf("conn-%d", time.Now().UnixNano()%10000),
	}

	h.mu.Lock()
	h.events = append(h.events, fmt.Sprintf("TagConn: remote=%s, id=%s", info.RemoteAddr, tag.ConnID))
	h.mu.Unlock()

	return context.WithValue(ctx, connTagKey, tag)
}

// HandleConn은 연결 이벤트를 처리한다.
func (h *MetricsHandler) HandleConn(ctx context.Context, s ConnStats) {
	tag, _ := ctx.Value(connTagKey).(*ConnTag)
	connID := ""
	if tag != nil {
		connID = tag.ConnID
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	switch s.(type) {
	case *ConnBegin:
		h.connCount++
		h.events = append(h.events, fmt.Sprintf("HandleConn[Begin]: id=%s", connID))
	case *ConnEnd:
		h.connCount--
		h.events = append(h.events, fmt.Sprintf("HandleConn[End]: id=%s", connID))
	}
}

func (h *MetricsHandler) PrintSummary() {
	h.mu.Lock()
	defer h.mu.Unlock()

	fmt.Printf("  총 RPC 수: %d (에러: %d)\n", h.rpcCount, h.rpcErrors)
	fmt.Printf("  총 전송 바이트: %d\n", h.totalBytes)
	fmt.Printf("  활성 연결: %d\n", h.connCount)

	if len(h.latencies) > 0 {
		var total time.Duration
		min, max := h.latencies[0], h.latencies[0]
		for _, l := range h.latencies {
			total += l
			if l < min {
				min = l
			}
			if l > max {
				max = l
			}
		}
		avg := total / time.Duration(len(h.latencies))
		fmt.Printf("  지연 시간: min=%v, avg=%v, max=%v\n",
			min.Truncate(time.Microsecond),
			avg.Truncate(time.Microsecond),
			max.Truncate(time.Microsecond))
	}
}

func (h *MetricsHandler) PrintEvents() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range h.events {
		fmt.Printf("    %s\n", ev)
	}
}

// ========== RPC 시뮬레이션 ==========
// gRPC 전송 계층이 Handler 메서드를 호출하는 순서를 재현한다.

func simulateRPC(ctx context.Context, handler Handler, method string, success bool) {
	// 1. TagRPC — RPC 시작 전 태그 부착
	rpcCtx := handler.TagRPC(ctx, &RPCTagInfo{FullMethodName: method})

	// 2. Begin 이벤트
	handler.HandleRPC(rpcCtx, &Begin{
		BeginTime:  time.Now(),
		IsClient:   true,
		FullMethod: method,
	})

	// 3. OutPayload — 요청 전송
	reqSize := 100 + rand.Intn(500)
	handler.HandleRPC(rpcCtx, &OutPayload{
		SentTime:   time.Now(),
		Length:     reqSize,
		WireLength: reqSize + 5, // gRPC 프레임 오버헤드
	})

	// 처리 시간 시뮬레이션
	time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)

	if success {
		// 4. InPayload — 응답 수신
		respSize := 200 + rand.Intn(1000)
		handler.HandleRPC(rpcCtx, &InPayload{
			RecvTime:   time.Now(),
			Length:     respSize,
			WireLength: respSize + 5,
		})

		// 5. End — RPC 완료 (성공)
		handler.HandleRPC(rpcCtx, &End{
			EndTime: time.Now(),
			Error:   nil,
		})
	} else {
		// 5. End — RPC 완료 (실패)
		handler.HandleRPC(rpcCtx, &End{
			EndTime: time.Now(),
			Error:   fmt.Errorf("UNAVAILABLE: connection refused"),
		})
	}
}

func main() {
	fmt.Println("========================================")
	fmt.Println("gRPC Stats Handler 시뮬레이션")
	fmt.Println("========================================")

	handler := NewMetricsHandler()

	// 1. Handler 인터페이스 구조
	fmt.Println("\n[1] Handler 인터페이스 구조")
	fmt.Println("───────────────────────────")
	fmt.Println("  Handler 인터페이스:")
	fmt.Println("    TagRPC(ctx, *RPCTagInfo) ctx    — RPC 시작 시 태그 부착")
	fmt.Println("    HandleRPC(ctx, RPCStats)        — RPC 이벤트 처리")
	fmt.Println("    TagConn(ctx, *ConnTagInfo) ctx   — 연결 시작 시 태그 부착")
	fmt.Println("    HandleConn(ctx, ConnStats)       — 연결 이벤트 처리")
	fmt.Println()
	fmt.Println("  RPC 이벤트 순서:")
	fmt.Println("    TagRPC → Begin → OutPayload → InPayload → End")
	fmt.Println()
	fmt.Println("  커넥션 이벤트 순서:")
	fmt.Println("    TagConn → ConnBegin → ... → ConnEnd")

	// 2. 연결 이벤트 시뮬레이션
	fmt.Println("\n[2] 연결 이벤트")
	fmt.Println("────────────────")

	connCtx := handler.TagConn(context.Background(), &ConnTagInfo{
		RemoteAddr: "10.0.0.1:443",
		LocalAddr:  "192.168.1.100:54321",
	})
	handler.HandleConn(connCtx, &ConnBegin{IsClient: true})

	// 두 번째 연결
	connCtx2 := handler.TagConn(context.Background(), &ConnTagInfo{
		RemoteAddr: "10.0.0.2:443",
		LocalAddr:  "192.168.1.100:54322",
	})
	handler.HandleConn(connCtx2, &ConnBegin{IsClient: true})

	// 3. RPC 이벤트 시뮬레이션
	fmt.Println("\n[3] RPC 이벤트 시뮬레이션")
	fmt.Println("──────────────────────────")

	methods := []string{
		"/myservice.UserService/GetUser",
		"/myservice.UserService/ListUsers",
		"/myservice.OrderService/CreateOrder",
		"/myservice.OrderService/GetOrder",
		"/myservice.UserService/DeleteUser",
	}

	fmt.Println("  RPC 호출 시뮬레이션 중...")
	for i := 0; i < 8; i++ {
		method := methods[i%len(methods)]
		ctx := connCtx
		if i%3 == 0 {
			ctx = connCtx2
		}
		success := i != 3 && i != 6 // 3번째, 6번째 RPC 실패
		simulateRPC(ctx, handler, method, success)
	}

	// 4. 연결 종료
	handler.HandleConn(connCtx2, &ConnEnd{})

	// 5. 전체 이벤트 로그
	fmt.Println("\n[4] 전체 이벤트 로그")
	fmt.Println("─────────────────────")
	handler.PrintEvents()

	// 6. 메트릭 요약
	fmt.Println("\n[5] 메트릭 요약")
	fmt.Println("────────────────")
	handler.PrintSummary()

	// 7. context를 통한 이벤트 상관관계
	fmt.Println("\n[6] Context 기반 이벤트 상관관계")
	fmt.Println("──────────────────────────────────")
	fmt.Println("  이벤트 흐름:")
	fmt.Println()
	fmt.Println("    TagConn (ConnTag{ConnID, RemoteAddr})")
	fmt.Println("       │")
	fmt.Println("       ▼ context에 ConnTag 저장")
	fmt.Println("    ConnBegin")
	fmt.Println("       │")
	fmt.Println("       ▼ RPC 시작")
	fmt.Println("    TagRPC (RPCTag{Method, StartTime, ConnID})")
	fmt.Println("       │    ← ConnTag에서 ConnID를 복사하여 RPC와 연결을 연관")
	fmt.Println("       ▼ context에 RPCTag 저장")
	fmt.Println("    Begin → OutPayload → InPayload → End")
	fmt.Println("       │    각 이벤트에서 RPCTag를 context로부터 추출")
	fmt.Println("       │    → method, latency, connID로 메트릭 기록")
	fmt.Println("       ▼")
	fmt.Println("    ConnEnd")

	// 8. 다중 핸들러
	fmt.Println("\n[7] 다중 핸들러 패턴")
	fmt.Println("─────────────────────")
	fmt.Println("  gRPC는 여러 Handler를 체인으로 등록할 수 있다:")
	fmt.Println("    grpc.Dial(target,")
	fmt.Println("      grpc.WithStatsHandler(&LoggingHandler{}),")
	fmt.Println("      grpc.WithStatsHandler(&MetricsHandler{}),")
	fmt.Println("      grpc.WithStatsHandler(&TracingHandler{}),")
	fmt.Println("    )")
	fmt.Println()
	fmt.Println("  각 핸들러가 순서대로 호출된다:")
	fmt.Println("    LoggingHandler.HandleRPC()  → 로그 기록")
	fmt.Println("    MetricsHandler.HandleRPC()  → Prometheus 카운터 증가")
	fmt.Println("    TracingHandler.HandleRPC()  → OpenTelemetry 스팬 생성")

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
