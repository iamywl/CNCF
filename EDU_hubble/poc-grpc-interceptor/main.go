// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble gRPC Interceptor (미들웨어) 패턴
//
// gRPC Interceptor는 HTTP 미들웨어와 유사한 패턴입니다:
//   - Unary Interceptor: 단일 요청/응답 (Status, GetNodes)
//   - Stream Interceptor: 스트리밍 (GetFlows)
//   - 체이닝: 여러 Interceptor를 순서대로 실행
//
// Hubble에서의 사용:
//   - Prometheus 메트릭 (요청 수, 지연시간)
//   - 버전 정보 주입 (gRPC metadata)
//   - 인증/인가
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ========================================
// 1. gRPC 타입 시뮬레이션
// ========================================

// ServerInfo는 호출되는 서버 메서드 정보입니다.
type ServerInfo struct {
	FullMethod string // 예: "/observer.Observer/GetFlows"
}

// Metadata는 gRPC 메타데이터 (HTTP/2 헤더)입니다.
type Metadata map[string]string

// Request는 gRPC 요청을 시뮬레이션합니다.
type Request struct {
	Method   string
	Payload  string
	Metadata Metadata
}

// Response는 gRPC 응답을 시뮬레이션합니다.
type Response struct {
	Payload  string
	Metadata Metadata
	Error    error
}

// ========================================
// 2. Interceptor 타입 정의
// ========================================

// UnaryHandler는 실제 RPC 핸들러입니다.
type UnaryHandler func(ctx context.Context, req *Request) (*Response, error)

// UnaryServerInterceptor는 Unary RPC 인터셉터입니다.
// 실제 gRPC: grpc.UnaryServerInterceptor
type UnaryServerInterceptor func(
	ctx context.Context,
	req *Request,
	info *ServerInfo,
	handler UnaryHandler,
) (*Response, error)

// StreamHandler는 스트리밍 RPC 핸들러입니다.
type StreamHandler func(ctx context.Context, stream *ServerStream) error

// StreamServerInterceptor는 Stream RPC 인터셉터입니다.
// 실제 gRPC: grpc.StreamServerInterceptor
type StreamServerInterceptor func(
	ctx context.Context,
	stream *ServerStream,
	info *ServerInfo,
	handler StreamHandler,
) error

// ServerStream은 서버 스트림을 시뮬레이션합니다.
type ServerStream struct {
	Method   string
	Messages []string
	Metadata Metadata
}

func (s *ServerStream) Send(msg string) {
	s.Messages = append(s.Messages, msg)
}

// ========================================
// 3. Interceptor 구현들
// ========================================

// LoggingInterceptor는 요청/응답을 로깅합니다.
func LoggingUnaryInterceptor() UnaryServerInterceptor {
	return func(ctx context.Context, req *Request, info *ServerInfo, handler UnaryHandler) (*Response, error) {
		start := time.Now()
		fmt.Printf("      [Logging] → %s 요청 시작\n", info.FullMethod)

		// 다음 핸들러 호출
		resp, err := handler(ctx, req)

		duration := time.Since(start)
		status := "OK"
		if err != nil {
			status = fmt.Sprintf("ERROR: %v", err)
		}
		fmt.Printf("      [Logging] ← %s 완료 (%v) status=%s\n", info.FullMethod, duration, status)

		return resp, err
	}
}

// MetricsInterceptor는 요청 메트릭을 수집합니다.
type MetricsCollector struct {
	RequestCount  map[string]int
	ErrorCount    map[string]int
	TotalDuration map[string]time.Duration
}

func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		RequestCount:  make(map[string]int),
		ErrorCount:    make(map[string]int),
		TotalDuration: make(map[string]time.Duration),
	}
}

func (m *MetricsCollector) UnaryInterceptor() UnaryServerInterceptor {
	return func(ctx context.Context, req *Request, info *ServerInfo, handler UnaryHandler) (*Response, error) {
		start := time.Now()
		m.RequestCount[info.FullMethod]++

		resp, err := handler(ctx, req)

		m.TotalDuration[info.FullMethod] += time.Since(start)
		if err != nil {
			m.ErrorCount[info.FullMethod]++
		}

		fmt.Printf("      [Metrics] %s count=%d\n", info.FullMethod, m.RequestCount[info.FullMethod])
		return resp, err
	}
}

// VersionInterceptor는 gRPC 메타데이터에 버전 정보를 추가합니다.
// 실제 Hubble: relayVersionUnaryInterceptor
func VersionUnaryInterceptor(version string) UnaryServerInterceptor {
	return func(ctx context.Context, req *Request, info *ServerInfo, handler UnaryHandler) (*Response, error) {
		// 요청 메타데이터에 버전 추가
		if req.Metadata == nil {
			req.Metadata = make(Metadata)
		}
		req.Metadata["x-hubble-relay-version"] = version

		resp, err := handler(ctx, req)

		// 응답 메타데이터에도 버전 추가
		if resp != nil && resp.Metadata == nil {
			resp.Metadata = make(Metadata)
		}
		if resp != nil {
			resp.Metadata["x-hubble-relay-version"] = version
		}

		fmt.Printf("      [Version] 버전 메타데이터 주입: %s\n", version)
		return resp, err
	}
}

// AuthInterceptor는 인증을 검증합니다.
func AuthUnaryInterceptor(validTokens []string) UnaryServerInterceptor {
	return func(ctx context.Context, req *Request, info *ServerInfo, handler UnaryHandler) (*Response, error) {
		token := req.Metadata["authorization"]
		authorized := false
		for _, valid := range validTokens {
			if token == valid {
				authorized = true
				break
			}
		}

		if !authorized && token != "" {
			fmt.Printf("      [Auth] 인증 실패: 잘못된 토큰\n")
			return nil, fmt.Errorf("unauthenticated: invalid token")
		}

		if token == "" {
			fmt.Printf("      [Auth] 토큰 없음 (공개 메서드로 허용)\n")
		} else {
			fmt.Printf("      [Auth] 인증 성공\n")
		}

		return handler(ctx, req)
	}
}

// ========================================
// 4. Interceptor 체이닝
// ========================================

// ChainUnaryInterceptors는 여러 Interceptor를 체이닝합니다.
// 실제 gRPC: grpc.ChainUnaryInterceptor()
func ChainUnaryInterceptors(interceptors ...UnaryServerInterceptor) UnaryServerInterceptor {
	return func(ctx context.Context, req *Request, info *ServerInfo, handler UnaryHandler) (*Response, error) {
		// 체인을 역순으로 구성 (마지막 인터셉터가 핸들러에 가장 가까움)
		current := handler
		for i := len(interceptors) - 1; i >= 0; i-- {
			interceptor := interceptors[i]
			next := current
			current = func(ctx context.Context, req *Request) (*Response, error) {
				return interceptor(ctx, req, info, next)
			}
		}
		return current(ctx, req)
	}
}

// ========================================
// 5. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble gRPC Interceptor 패턴 ===")
	fmt.Println()
	fmt.Println("Interceptor 체인 (HTTP 미들웨어와 동일한 패턴):")
	fmt.Println("  요청 → [Auth] → [Logging] → [Metrics] → [Version] → Handler")
	fmt.Println("  응답 ← [Auth] ← [Logging] ← [Metrics] ← [Version] ← Handler")
	fmt.Println()

	metrics := NewMetricsCollector()

	// Interceptor 체인 구성
	chain := ChainUnaryInterceptors(
		AuthUnaryInterceptor([]string{"valid-token-123"}),
		LoggingUnaryInterceptor(),
		metrics.UnaryInterceptor(),
		VersionUnaryInterceptor("v0.13.0"),
	)

	// 실제 핸들러
	statusHandler := func(ctx context.Context, req *Request) (*Response, error) {
		time.Sleep(5 * time.Millisecond) // 처리 시뮬레이션
		return &Response{
			Payload: fmt.Sprintf("ServerStatus: num_flows=4096, max_flows=8192"),
		}, nil
	}

	info := &ServerInfo{FullMethod: "/observer.Observer/ServerStatus"}

	// ── 시나리오 1: 정상 요청 ──
	fmt.Println("━━━ 시나리오 1: 정상 요청 (인증 성공) ━━━")
	fmt.Println()

	req1 := &Request{
		Method:   "ServerStatus",
		Payload:  "{}",
		Metadata: Metadata{"authorization": "valid-token-123"},
	}

	resp1, err := chain(context.Background(), req1, info, statusHandler)
	fmt.Println()
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
	} else {
		fmt.Printf("  응답: %s\n", resp1.Payload)
		fmt.Printf("  메타데이터: %v\n", resp1.Metadata)
	}
	fmt.Println()

	// ── 시나리오 2: 인증 실패 ──
	fmt.Println("━━━ 시나리오 2: 인증 실패 ━━━")
	fmt.Println()

	req2 := &Request{
		Method:   "ServerStatus",
		Payload:  "{}",
		Metadata: Metadata{"authorization": "invalid-token"},
	}

	_, err = chain(context.Background(), req2, info, statusHandler)
	fmt.Println()
	if err != nil {
		fmt.Printf("  예상대로 실패: %v\n", err)
		fmt.Printf("  → Auth Interceptor가 체인을 중단, 이후 Interceptor 실행 안 됨\n")
	}
	fmt.Println()

	// ── 시나리오 3: 여러 메서드 호출 ──
	fmt.Println("━━━ 시나리오 3: 여러 메서드 호출 (메트릭 누적) ━━━")
	fmt.Println()

	methods := []struct {
		method string
		info   *ServerInfo
	}{
		{"GetFlows", &ServerInfo{FullMethod: "/observer.Observer/GetFlows"}},
		{"ServerStatus", &ServerInfo{FullMethod: "/observer.Observer/ServerStatus"}},
		{"GetFlows", &ServerInfo{FullMethod: "/observer.Observer/GetFlows"}},
		{"GetNodes", &ServerInfo{FullMethod: "/observer.Observer/GetNodes"}},
	}

	for _, m := range methods {
		req := &Request{
			Method:   m.method,
			Metadata: Metadata{},
		}
		chain(context.Background(), req, m.info, statusHandler)
		fmt.Println()
	}

	// ── 메트릭 리포트 ──
	fmt.Println("━━━ Interceptor 메트릭 리포트 ━━━")
	fmt.Println()

	for method, count := range metrics.RequestCount {
		bar := strings.Repeat("█", count*3)
		errCount := metrics.ErrorCount[method]
		avgDuration := metrics.TotalDuration[method] / time.Duration(count)
		fmt.Printf("  %-40s calls=%d errors=%d avg=%v %s\n",
			method, count, errCount, avgDuration.Round(time.Millisecond), bar)
	}

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - Unary Interceptor: 단일 요청/응답 (ServerStatus, GetNodes)")
	fmt.Println("  - Stream Interceptor: 스트리밍 RPC (GetFlows)")
	fmt.Println("  - 체이닝: grpc.ChainUnaryInterceptor()로 순서 보장")
	fmt.Println("  - 실제 Hubble: Prometheus 메트릭 + 버전 주입 Interceptor 사용")
	fmt.Println("  - Auth 실패 시 체인 중단: 이후 Interceptor/Handler 실행 안 됨")
}
