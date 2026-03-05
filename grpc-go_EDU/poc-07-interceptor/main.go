// poc-07-interceptor: 인터셉터 체이닝
//
// gRPC의 인터셉터(Interceptor) 체이닝 메커니즘을 시뮬레이션한다.
// - UnaryServerInterceptor, UnaryClientInterceptor 타입 정의
// - 체이닝: 여러 인터셉터를 순차적으로 실행하는 메커니즘
// - 로깅, 인증, 메트릭, 복구(recovery) 인터셉터 구현
// - 인터셉터가 요청을 거부하는 케이스
//
// 실제 gRPC 참조:
//   interceptor.go        → UnaryClientInterceptor, UnaryServerInterceptor 타입
//   server.go             → chainUnaryServerInterceptors
//   clientconn.go         → chainUnaryClientInterceptors
//   internal/transport/   → 실제 인터셉터 호출 흐름

package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ──────────────────────────────────────────────
// 1. 핵심 타입 정의 (interceptor.go 참조)
// ──────────────────────────────────────────────

// UnaryServerInfo는 서버 인터셉터에 전달되는 RPC 정보.
// 실제: interceptor.go의 UnaryServerInfo
type UnaryServerInfo struct {
	FullMethod string // "/package.service/method"
	Server     any    // 서비스 구현체
}

// UnaryHandler는 실제 RPC 핸들러.
// 실제: interceptor.go의 UnaryHandler
type UnaryHandler func(ctx context.Context, req any) (any, error)

// UnaryServerInterceptor는 서버 사이드 Unary RPC 인터셉터.
// 실제: interceptor.go:87
// func(ctx, req, info, handler) (resp, err)
type UnaryServerInterceptor func(
	ctx context.Context,
	req any,
	info *UnaryServerInfo,
	handler UnaryHandler,
) (any, error)

// UnaryInvoker는 클라이언트의 실제 RPC 호출자.
// 실제: interceptor.go의 UnaryInvoker
type UnaryInvoker func(ctx context.Context, method string, req, reply any) error

// UnaryClientInterceptor는 클라이언트 사이드 Unary RPC 인터셉터.
// 실제: interceptor.go:43
// func(ctx, method, req, reply, cc, invoker, opts...) error
type UnaryClientInterceptor func(
	ctx context.Context,
	method string,
	req, reply any,
	invoker UnaryInvoker,
) error

// ──────────────────────────────────────────────
// 2. 체이닝 메커니즘 (server.go, clientconn.go 참조)
// ──────────────────────────────────────────────

// chainUnaryServerInterceptors는 여러 서버 인터셉터를 하나로 체이닝한다.
// 실제: server.go의 chainUnaryServerInterceptors
// 핵심: 각 인터셉터의 handler 파라미터를 다음 인터셉터를 감싼 함수로 교체
func chainUnaryServerInterceptors(interceptors []UnaryServerInterceptor) UnaryServerInterceptor {
	switch len(interceptors) {
	case 0:
		return nil
	case 1:
		return interceptors[0]
	default:
		return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (any, error) {
			// 마지막 인터셉터부터 역순으로 핸들러를 감싼다
			// interceptor[0] → interceptor[1] → ... → interceptor[n-1] → handler
			currHandler := handler
			for i := len(interceptors) - 1; i > 0; i-- {
				// 클로저 캡처를 위해 지역 변수 사용
				interceptor := interceptors[i]
				next := currHandler
				currHandler = func(ctx context.Context, req any) (any, error) {
					return interceptor(ctx, req, info, next)
				}
			}
			return interceptors[0](ctx, req, info, currHandler)
		}
	}
}

// chainUnaryClientInterceptors는 여러 클라이언트 인터셉터를 하나로 체이닝한다.
// 실제: clientconn.go의 chainUnaryClientInterceptors
func chainUnaryClientInterceptors(interceptors []UnaryClientInterceptor) UnaryClientInterceptor {
	switch len(interceptors) {
	case 0:
		return nil
	case 1:
		return interceptors[0]
	default:
		return func(ctx context.Context, method string, req, reply any, invoker UnaryInvoker) error {
			currInvoker := invoker
			for i := len(interceptors) - 1; i > 0; i-- {
				interceptor := interceptors[i]
				next := currInvoker
				currInvoker = func(ctx context.Context, method string, req, reply any) error {
					return interceptor(ctx, method, req, reply, next)
				}
			}
			return interceptors[0](ctx, method, req, reply, currInvoker)
		}
	}
}

// ──────────────────────────────────────────────
// 3. 서버 인터셉터 구현
// ──────────────────────────────────────────────

// loggingServerInterceptor는 요청/응답을 로깅한다.
func loggingServerInterceptor() UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (any, error) {
		start := time.Now()
		fmt.Printf("    [서버/로깅] ▶ 요청: method=%s, req=%v\n", info.FullMethod, req)

		resp, err := handler(ctx, req)

		elapsed := time.Since(start)
		if err != nil {
			fmt.Printf("    [서버/로깅] ◀ 에러: %v (소요: %v)\n", err, elapsed)
		} else {
			fmt.Printf("    [서버/로깅] ◀ 응답: %v (소요: %v)\n", resp, elapsed)
		}
		return resp, err
	}
}

// authServerInterceptor는 인증을 검증한다.
func authServerInterceptor(validToken string) UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (any, error) {
		token, ok := ctx.Value(ctxKeyToken).(string)
		if !ok || token != validToken {
			fmt.Printf("    [서버/인증] ✗ 인증 실패 (token=%q)\n", token)
			return nil, fmt.Errorf("인증 실패: 유효하지 않은 토큰")
		}
		fmt.Printf("    [서버/인증] ✓ 인증 성공\n")
		return handler(ctx, req)
	}
}

// metricsServerInterceptor는 RPC 메트릭을 수집한다.
func metricsServerInterceptor(metrics *Metrics) UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (any, error) {
		metrics.totalRequests++
		fmt.Printf("    [서버/메트릭] 요청 카운트: %d\n", metrics.totalRequests)

		resp, err := handler(ctx, req)

		if err != nil {
			metrics.totalErrors++
		}
		return resp, err
	}
}

// recoveryServerInterceptor는 패닉을 복구한다.
func recoveryServerInterceptor() UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("    [서버/복구] 패닉 복구: %v\n", r)
				err = fmt.Errorf("내부 서버 오류")
			}
		}()
		return handler(ctx, req)
	}
}

// Metrics는 간단한 메트릭 저장소.
type Metrics struct {
	totalRequests int
	totalErrors   int
}

// 컨텍스트 키 타입
type contextKey string

const ctxKeyToken contextKey = "auth-token"

// ──────────────────────────────────────────────
// 4. 클라이언트 인터셉터 구현
// ──────────────────────────────────────────────

// loggingClientInterceptor는 클라이언트 측 로깅.
func loggingClientInterceptor() UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, invoker UnaryInvoker) error {
		fmt.Printf("    [클라이언트/로깅] ▶ 호출: method=%s, req=%v\n", method, req)
		err := invoker(ctx, method, req, reply)
		if err != nil {
			fmt.Printf("    [클라이언트/로깅] ◀ 에러: %v\n", err)
		} else {
			fmt.Printf("    [클라이언트/로깅] ◀ 성공\n")
		}
		return err
	}
}

// retryClientInterceptor는 실패 시 재시도한다.
func retryClientInterceptor(maxRetries int) UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, invoker UnaryInvoker) error {
		var lastErr error
		for attempt := 0; attempt <= maxRetries; attempt++ {
			if attempt > 0 {
				fmt.Printf("    [클라이언트/재시도] 재시도 #%d\n", attempt)
			}
			lastErr = invoker(ctx, method, req, reply)
			if lastErr == nil {
				return nil
			}
		}
		fmt.Printf("    [클라이언트/재시도] %d회 재시도 후 최종 실패\n", maxRetries)
		return lastErr
	}
}

// timeoutClientInterceptor는 타임아웃을 설정한다.
func timeoutClientInterceptor(timeout time.Duration) UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, invoker UnaryInvoker) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		fmt.Printf("    [클라이언트/타임아웃] timeout=%v 설정\n", timeout)
		return invoker(ctx, method, req, reply)
	}
}

// ──────────────────────────────────────────────
// 5. 시뮬레이션용 핸들러/인보커
// ──────────────────────────────────────────────

// echoHandler는 요청을 그대로 응답하는 테스트 핸들러.
func echoHandler(ctx context.Context, req any) (any, error) {
	return fmt.Sprintf("Echo: %v", req), nil
}

// panicHandler는 패닉을 발생시키는 테스트 핸들러.
func panicHandler(ctx context.Context, req any) (any, error) {
	panic("예기치 않은 오류!")
}

// mockInvoker는 서버 호출을 시뮬레이션하는 인보커.
func mockInvoker(ctx context.Context, method string, req, reply any) error {
	fmt.Printf("    [실제 호출] method=%s 실행\n", method)
	return nil
}

// failingInvoker는 실패하는 인보커 (재시도 테스트용).
var failCount int

func failingInvoker(ctx context.Context, method string, req, reply any) error {
	failCount++
	if failCount <= 2 {
		return fmt.Errorf("일시적 오류 (#%d)", failCount)
	}
	fmt.Printf("    [실제 호출] method=%s 성공 (#%d)\n", method, failCount)
	return nil
}

// ──────────────────────────────────────────────
// 6. main
// ──────────────────────────────────────────────

func main() {
	fmt.Println("=== 인터셉터 체이닝 시뮬레이션 ===")
	fmt.Println()

	metrics := &Metrics{}

	// === 서버 인터셉터 체이닝 ===
	fmt.Println("── 1. 서버 인터셉터 체인 (로깅 → 메트릭 → 인증 → 핸들러) ──")
	serverChain := chainUnaryServerInterceptors([]UnaryServerInterceptor{
		loggingServerInterceptor(),
		metricsServerInterceptor(metrics),
		authServerInterceptor("valid-secret-token"),
	})

	info := &UnaryServerInfo{FullMethod: "/greeter.Greeter/SayHello"}

	// 케이스 1: 인증 성공
	fmt.Println()
	fmt.Println("  [케이스 1] 인증 성공:")
	ctx := context.WithValue(context.Background(), ctxKeyToken, "valid-secret-token")
	resp, err := serverChain(ctx, "World", info, echoHandler)
	if err != nil {
		fmt.Printf("  결과: 에러=%v\n", err)
	} else {
		fmt.Printf("  결과: %v\n", resp)
	}

	// 케이스 2: 인증 실패 (인터셉터가 요청 거부)
	fmt.Println()
	fmt.Println("  [케이스 2] 인증 실패 (요청 거부):")
	ctx2 := context.WithValue(context.Background(), ctxKeyToken, "wrong-token")
	resp2, err2 := serverChain(ctx2, "World", info, echoHandler)
	if err2 != nil {
		fmt.Printf("  결과: 에러=%v\n", err2)
	} else {
		fmt.Printf("  결과: %v\n", resp2)
	}

	// 케이스 3: 패닉 복구
	fmt.Println()
	fmt.Println("── 2. 패닉 복구 인터셉터 ──")
	recoveryChain := chainUnaryServerInterceptors([]UnaryServerInterceptor{
		loggingServerInterceptor(),
		recoveryServerInterceptor(),
	})
	resp3, err3 := recoveryChain(context.Background(), "panic-test", info, panicHandler)
	if err3 != nil {
		fmt.Printf("  결과: 에러=%v\n", err3)
	} else {
		fmt.Printf("  결과: %v\n", resp3)
	}

	// === 클라이언트 인터셉터 체이닝 ===
	fmt.Println()
	fmt.Println("── 3. 클라이언트 인터셉터 체인 (타임아웃 → 로깅 → 인보커) ──")
	clientChain := chainUnaryClientInterceptors([]UnaryClientInterceptor{
		timeoutClientInterceptor(5 * time.Second),
		loggingClientInterceptor(),
	})
	clientChain(context.Background(), "/greeter.Greeter/SayHello", "Hello", nil, mockInvoker)

	// 재시도 인터셉터 테스트
	fmt.Println()
	fmt.Println("── 4. 재시도 인터셉터 (2회 실패 후 성공) ──")
	failCount = 0
	retryChain := chainUnaryClientInterceptors([]UnaryClientInterceptor{
		loggingClientInterceptor(),
		retryClientInterceptor(3),
	})
	err4 := retryChain(context.Background(), "/service/Method", "data", nil, failingInvoker)
	if err4 != nil {
		fmt.Printf("  최종 결과: 에러=%v\n", err4)
	} else {
		fmt.Printf("  최종 결과: 성공\n")
	}

	// 인터셉터 실행 순서 확인
	fmt.Println()
	fmt.Println("── 5. 실행 순서 확인 ──")
	orderChain := chainUnaryServerInterceptors([]UnaryServerInterceptor{
		makeOrderInterceptor("A"),
		makeOrderInterceptor("B"),
		makeOrderInterceptor("C"),
	})
	orderChain(context.Background(), "test", info, func(ctx context.Context, req any) (any, error) {
		fmt.Println("    [핸들러] 실행")
		return "ok", nil
	})

	// 메트릭 요약
	fmt.Println()
	fmt.Printf("── 메트릭 요약 ──\n")
	fmt.Printf("  총 요청: %d, 총 에러: %d\n", metrics.totalRequests, metrics.totalErrors)

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}

// makeOrderInterceptor는 실행 순서를 보여주는 인터셉터를 생성한다.
func makeOrderInterceptor(name string) UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (any, error) {
		fmt.Printf("    [인터셉터 %s] 진입 ▶\n", name)
		resp, err := handler(ctx, req)
		fmt.Printf("    [인터셉터 %s] 복귀 ◀\n", name)
		_ = strings.TrimSpace("") // import 사용
		return resp, err
	}
}
