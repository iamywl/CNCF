// poc-02-data-model: ServiceDesc/MethodDesc 등록 및 디스패치
//
// gRPC의 서비스 등록 데이터 모델을 심층적으로 시뮬레이션한다.
// - ServiceDesc → serviceInfo 변환 과정
// - StreamDesc를 포함한 완전한 메서드 분류 (Unary vs Stream)
// - handleStream 로직: fullMethod 파싱 → 서비스/메서드 찾기 → 호출
// - 서비스 메타데이터 조회 (GetServiceInfo)
//
// 실제 gRPC 참조:
//   server.go:99    → MethodDesc, StreamDesc
//   server.go:105   → ServiceDesc
//   server.go:117   → serviceInfo
//   server.go       → RegisterService, GetServiceInfo, handleStream

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ──────────────────────────────────────────────
// 1. 핵심 데이터 모델 (server.go 참조)
// ──────────────────────────────────────────────

// MethodHandler는 Unary RPC 핸들러.
// 실제: type methodHandler func(srv any, ctx context.Context, dec func(any) error, ...) (any, error)
type MethodHandler func(srv any, ctx context.Context, dec func(any) error) (any, error)

// MethodDesc는 Unary RPC 메서드를 기술한다.
// 실제: server.go:99
type MethodDesc struct {
	MethodName string
	Handler    MethodHandler
}

// StreamHandler는 Streaming RPC 핸들러.
// 실제: type StreamHandler func(srv any, stream ServerStream) error
type StreamHandler func(srv any, stream *MockStream) error

// StreamDesc는 Streaming RPC 메서드를 기술한다.
// 실제: server.go:88
type StreamDesc struct {
	StreamName    string
	Handler       StreamHandler
	ServerStreams  bool // 서버가 스트림을 보내는지
	ClientStreams  bool // 클라이언트가 스트림을 보내는지
}

// ServiceDesc는 하나의 gRPC 서비스를 완전히 기술한다.
// 실제: server.go:105
type ServiceDesc struct {
	ServiceName string
	HandlerType any        // 서비스 인터페이스 타입 (타입 검증용)
	Methods     []MethodDesc
	Streams     []StreamDesc
	Metadata    string     // protobuf 파일 이름
}

// serviceInfo는 서버 내부에서 서비스를 효율적으로 관리하는 구조체.
// 실제: server.go:117 — ServiceDesc에서 변환되어, 맵 기반 O(1) 조회를 제공한다.
type serviceInfo struct {
	serviceImpl any
	methods     map[string]*MethodDesc  // 메서드명 → MethodDesc
	streams     map[string]*StreamDesc  // 스트림명 → StreamDesc
	mdata       any                     // 서비스 메타데이터
}

// ServiceInfo는 외부에 노출되는 서비스 정보. (GetServiceInfo 반환용)
// 실제: server.go의 ServiceInfo 구조체
type ServiceInfo struct {
	Methods  []MethodInfo
	Metadata any
}

// MethodInfo는 외부에 노출되는 메서드 정보.
type MethodInfo struct {
	Name           string
	IsClientStream bool
	IsServerStream bool
}

// ──────────────────────────────────────────────
// 2. Server 핵심: 등록, 조회, 디스패치
// ──────────────────────────────────────────────

// Server는 서비스 레지스트리 역할.
type Server struct {
	services map[string]*serviceInfo
}

func NewServer() *Server {
	return &Server{services: make(map[string]*serviceInfo)}
}

// RegisterService는 ServiceDesc를 serviceInfo로 변환하여 등록한다.
// 실제: server.go의 register 메서드
// 핵심: 배열을 맵으로 변환하여 O(1) 조회를 가능하게 한다.
func (s *Server) RegisterService(sd *ServiceDesc, impl any) {
	if _, ok := s.services[sd.ServiceName]; ok {
		panic(fmt.Sprintf("서비스 '%s'가 이미 등록됨", sd.ServiceName))
	}

	info := &serviceInfo{
		serviceImpl: impl,
		methods:     make(map[string]*MethodDesc),
		streams:     make(map[string]*StreamDesc),
		mdata:       sd.Metadata,
	}

	// Methods 배열 → methods 맵 변환
	for i := range sd.Methods {
		md := &sd.Methods[i]
		info.methods[md.MethodName] = md
	}

	// Streams 배열 → streams 맵 변환
	for i := range sd.Streams {
		sd := &sd.Streams[i]
		info.streams[sd.StreamName] = sd
	}

	s.services[sd.ServiceName] = info
	fmt.Printf("[등록] %s: unary=%d, stream=%d\n",
		sd.ServiceName, len(info.methods), len(info.streams))
}

// GetServiceInfo는 등록된 서비스 정보를 반환한다.
// 실제: server.go의 GetServiceInfo
func (s *Server) GetServiceInfo() map[string]ServiceInfo {
	result := make(map[string]ServiceInfo)

	for name, info := range s.services {
		var methods []MethodInfo

		// Unary 메서드 추가
		for mName := range info.methods {
			methods = append(methods, MethodInfo{
				Name:           mName,
				IsClientStream: false,
				IsServerStream: false,
			})
		}

		// Stream 메서드 추가
		for sName, sd := range info.streams {
			methods = append(methods, MethodInfo{
				Name:           sName,
				IsClientStream: sd.ClientStreams,
				IsServerStream: sd.ServerStreams,
			})
		}

		result[name] = ServiceInfo{Methods: methods, Metadata: info.mdata}
	}
	return result
}

// handleStream은 fullMethod 경로를 파싱하여 적절한 핸들러를 찾고 호출한다.
// 실제: server.go의 handleStream → processUnaryRPC / processStreamingRPC
// fullMethod 형식: "/package.service/method" (HTTP/2 :path 헤더)
func (s *Server) handleStream(fullMethod string, payload []byte) {
	fmt.Printf("\n[디스패치] fullMethod=%s\n", fullMethod)

	// "/" 접두사 제거 후 서비스/메서드 분리
	// 실제: 서비스명과 메서드명을 마지막 "/" 기준으로 분리
	sm := strings.TrimPrefix(fullMethod, "/")
	pos := strings.LastIndex(sm, "/")
	if pos < 0 {
		fmt.Printf("  [에러] 잘못된 메서드 경로: %s\n", fullMethod)
		return
	}
	service := sm[:pos]
	method := sm[pos+1:]
	fmt.Printf("  서비스=%s, 메서드=%s\n", service, method)

	// 서비스 조회
	info, ok := s.services[service]
	if !ok {
		fmt.Printf("  [에러] 서비스 '%s' 미등록\n", service)
		return
	}

	// 1) Unary 메서드에서 찾기
	if md, ok := info.methods[method]; ok {
		fmt.Printf("  [Unary] 핸들러 호출\n")
		dec := func(v any) error { return json.Unmarshal(payload, v) }
		result, err := md.Handler(info.serviceImpl, context.Background(), dec)
		if err != nil {
			fmt.Printf("  [에러] %v\n", err)
			return
		}
		out, _ := json.Marshal(result)
		fmt.Printf("  [응답] %s\n", string(out))
		return
	}

	// 2) Stream 메서드에서 찾기
	if sd, ok := info.streams[method]; ok {
		streamType := classifyStream(sd)
		fmt.Printf("  [Stream:%s] 핸들러 호출\n", streamType)
		stream := &MockStream{data: payload}
		if err := sd.Handler(info.serviceImpl, stream); err != nil {
			fmt.Printf("  [에러] %v\n", err)
			return
		}
		fmt.Printf("  [완료] 스트림 처리 종료\n")
		return
	}

	fmt.Printf("  [에러] 메서드 '%s' 미등록\n", method)
}

// classifyStream은 스트림 타입을 분류한다.
func classifyStream(sd *StreamDesc) string {
	switch {
	case sd.ServerStreams && sd.ClientStreams:
		return "Bidi"
	case sd.ServerStreams:
		return "ServerStream"
	case sd.ClientStreams:
		return "ClientStream"
	default:
		return "Unary" // 스트림 핸들러지만 양쪽 모두 false
	}
}

// MockStream은 스트리밍 테스트용 모의 스트림.
type MockStream struct {
	data []byte
}

// ──────────────────────────────────────────────
// 3. 예시 서비스: Calculator + ChatService
// ──────────────────────────────────────────────

// --- Calculator 서비스 (Unary 메서드만) ---

type CalcRequest struct {
	A, B int
}
type CalcResponse struct {
	Result int
}

type CalculatorService struct{}

func (c *CalculatorService) Add(a, b int) int { return a + b }
func (c *CalculatorService) Mul(a, b int) int { return a * b }

var CalculatorDesc = ServiceDesc{
	ServiceName: "math.Calculator",
	HandlerType: (*CalculatorService)(nil),
	Methods: []MethodDesc{
		{
			MethodName: "Add",
			Handler: func(srv any, ctx context.Context, dec func(any) error) (any, error) {
				var req CalcRequest
				if err := dec(&req); err != nil {
					return nil, err
				}
				calc := srv.(*CalculatorService)
				return CalcResponse{Result: calc.Add(req.A, req.B)}, nil
			},
		},
		{
			MethodName: "Multiply",
			Handler: func(srv any, ctx context.Context, dec func(any) error) (any, error) {
				var req CalcRequest
				if err := dec(&req); err != nil {
					return nil, err
				}
				calc := srv.(*CalculatorService)
				return CalcResponse{Result: calc.Mul(req.A, req.B)}, nil
			},
		},
	},
	Metadata: "math/calculator.proto",
}

// --- Chat 서비스 (Stream 메서드 포함) ---

type ChatService struct{}

var ChatDesc = ServiceDesc{
	ServiceName: "chat.ChatService",
	HandlerType: (*ChatService)(nil),
	Methods: []MethodDesc{
		{
			MethodName: "GetHistory",
			Handler: func(srv any, ctx context.Context, dec func(any) error) (any, error) {
				return map[string]string{"status": "최근 채팅 내역 반환"}, nil
			},
		},
	},
	Streams: []StreamDesc{
		{
			StreamName:   "Subscribe",
			ServerStreams: true,
			ClientStreams: false,
			Handler: func(srv any, stream *MockStream) error {
				fmt.Printf("    → 서버 스트리밍: 실시간 메시지 전송 중...\n")
				return nil
			},
		},
		{
			StreamName:   "Upload",
			ServerStreams: false,
			ClientStreams: true,
			Handler: func(srv any, stream *MockStream) error {
				fmt.Printf("    → 클라이언트 스트리밍: 파일 수신 중...\n")
				return nil
			},
		},
		{
			StreamName:   "LiveChat",
			ServerStreams: true,
			ClientStreams: true,
			Handler: func(srv any, stream *MockStream) error {
				fmt.Printf("    → 양방향 스트리밍: 실시간 채팅 진행 중...\n")
				return nil
			},
		},
	},
	Metadata: "chat/service.proto",
}

// ──────────────────────────────────────────────
// 4. main: 등록 → 조회 → 디스패치 시연
// ──────────────────────────────────────────────

func main() {
	fmt.Println("=== ServiceDesc/MethodDesc 등록 및 디스패치 시뮬레이션 ===")
	fmt.Println()

	server := NewServer()

	// 서비스 등록
	fmt.Println("── 1. 서비스 등록 ──")
	server.RegisterService(&CalculatorDesc, &CalculatorService{})
	server.RegisterService(&ChatDesc, &ChatService{})

	// 서비스 정보 조회 (GetServiceInfo)
	fmt.Println("\n── 2. 등록된 서비스 조회 (GetServiceInfo) ──")
	for name, info := range server.GetServiceInfo() {
		fmt.Printf("\n서비스: %s (metadata: %v)\n", name, info.Metadata)
		for _, m := range info.Methods {
			streamInfo := "Unary"
			if m.IsServerStream && m.IsClientStream {
				streamInfo = "Bidi Streaming"
			} else if m.IsServerStream {
				streamInfo = "Server Streaming"
			} else if m.IsClientStream {
				streamInfo = "Client Streaming"
			}
			fmt.Printf("  - %s [%s]\n", m.Name, streamInfo)
		}
	}

	// handleStream 디스패치 테스트
	fmt.Println("\n── 3. handleStream 디스패치 ──")

	// Unary 호출
	payload, _ := json.Marshal(CalcRequest{A: 15, B: 27})
	server.handleStream("/math.Calculator/Add", payload)
	server.handleStream("/math.Calculator/Multiply", payload)

	// Stream 호출
	server.handleStream("/chat.ChatService/Subscribe", nil)
	server.handleStream("/chat.ChatService/Upload", nil)
	server.handleStream("/chat.ChatService/LiveChat", nil)

	// Unary in Chat
	server.handleStream("/chat.ChatService/GetHistory", nil)

	// 에러 케이스
	fmt.Println("\n── 4. 에러 케이스 ──")
	server.handleStream("/unknown.Service/Method", nil)
	server.handleStream("/math.Calculator/Subtract", nil)
	server.handleStream("invalid-path", nil)

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}
