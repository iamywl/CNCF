// poc-01-architecture: 클라이언트-서버 기본 RPC 통신
//
// gRPC의 핵심 아키텍처를 시뮬레이션한다.
// - TCP 기반 RPC 서버/클라이언트
// - ServiceDesc, MethodDesc를 사용한 서비스 등록
// - 메서드 디스패치 (서비스명/메서드명 기반 라우팅)
// - 요청/응답 직렬화 (JSON 사용)
//
// 실제 gRPC 참조:
//   server.go    → Server, ServiceDesc, MethodDesc, RegisterService, Serve
//   call.go      → Invoke (클라이언트 호출)
//   stream.go    → ServerStream 처리

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
)

// ──────────────────────────────────────────────
// 1. 핵심 타입 정의 (server.go 참조)
// ──────────────────────────────────────────────

// MethodHandler는 gRPC의 methodHandler 타입에 대응한다.
// 실제: type methodHandler func(srv any, ctx context.Context, dec func(any) error, ...) (any, error)
type MethodHandler func(srv any, ctx context.Context, req []byte) ([]byte, error)

// MethodDesc는 단일 RPC 메서드를 기술한다.
// 실제: server.go:99 — MethodName + Handler
type MethodDesc struct {
	MethodName string
	Handler    MethodHandler
}

// ServiceDesc는 하나의 gRPC 서비스를 기술한다.
// 실제: server.go:105 — ServiceName, HandlerType, Methods, Streams
type ServiceDesc struct {
	ServiceName string
	HandlerType any
	Methods     []MethodDesc
}

// serviceInfo는 서버 내부에서 서비스를 관리하는 구조체.
// 실제: server.go:117 — serviceImpl + methods 맵
type serviceInfo struct {
	serviceImpl any
	methods     map[string]*MethodDesc
}

// ──────────────────────────────────────────────
// 2. RPC 프로토콜 메시지
// ──────────────────────────────────────────────

// RPCRequest는 와이어를 통해 전송되는 RPC 요청.
// gRPC에서는 HTTP/2 HEADERS + DATA 프레임에 해당한다.
type RPCRequest struct {
	ServiceName string `json:"service"`
	MethodName  string `json:"method"`
	Payload     []byte `json:"payload"`
}

// RPCResponse는 서버가 반환하는 RPC 응답.
type RPCResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ──────────────────────────────────────────────
// 3. Server 구현 (server.go 참조)
// ──────────────────────────────────────────────

// Server는 gRPC 서버의 간소화 버전이다.
// 실제: server.go:126 — opts, services 맵, serve, lis 등
type Server struct {
	mu       sync.Mutex
	services map[string]*serviceInfo // 서비스명 → serviceInfo
	lis      net.Listener
	quit     chan struct{}
}

// NewServer는 새 서버를 생성한다.
func NewServer() *Server {
	return &Server{
		services: make(map[string]*serviceInfo),
		quit:     make(chan struct{}),
	}
}

// RegisterService는 서비스를 서버에 등록한다.
// 실제: server.go의 RegisterService → register 메서드
// ServiceDesc의 Methods를 순회하며 메서드명 → MethodDesc 맵을 구성한다.
func (s *Server) RegisterService(sd *ServiceDesc, impl any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	info := &serviceInfo{
		serviceImpl: impl,
		methods:     make(map[string]*MethodDesc),
	}
	for i := range sd.Methods {
		md := &sd.Methods[i]
		info.methods[md.MethodName] = md
	}
	s.services[sd.ServiceName] = info
	log.Printf("[서버] 서비스 등록: %s (메서드 %d개)", sd.ServiceName, len(sd.Methods))
}

// Serve는 리스너에서 연결을 수락하고 RPC를 처리한다.
// 실제: server.go의 Serve → handleRawConn → serveStreams
func (s *Server) Serve(lis net.Listener) error {
	s.lis = lis
	log.Printf("[서버] %s에서 대기 중...", lis.Addr())

	for {
		conn, err := lis.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return nil
			default:
				return fmt.Errorf("accept 실패: %w", err)
			}
		}
		// 각 연결을 별도 고루틴에서 처리 (실제 gRPC는 HTTP/2 스트림 단위)
		go s.handleConn(conn)
	}
}

// handleConn은 단일 연결에서 RPC 요청을 읽고 처리한다.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		var req RPCRequest
		if err := decoder.Decode(&req); err != nil {
			return // 연결 종료
		}
		resp := s.processRequest(req)
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

// processRequest는 서비스명/메서드명으로 핸들러를 찾아 호출한다.
// 실제: server.go의 handleStream → processUnaryRPC
func (s *Server) processRequest(req RPCRequest) RPCResponse {
	s.mu.Lock()
	info, ok := s.services[req.ServiceName]
	s.mu.Unlock()

	if !ok {
		return RPCResponse{Error: fmt.Sprintf("서비스 '%s'를 찾을 수 없음", req.ServiceName)}
	}

	md, ok := info.methods[req.MethodName]
	if !ok {
		return RPCResponse{Error: fmt.Sprintf("메서드 '%s/%s'를 찾을 수 없음", req.ServiceName, req.MethodName)}
	}

	// 핸들러 호출 (서비스 구현체 + 요청 전달)
	result, err := md.Handler(info.serviceImpl, context.Background(), req.Payload)
	if err != nil {
		return RPCResponse{Error: err.Error()}
	}
	return RPCResponse{Payload: result}
}

// Stop은 서버를 종료한다.
func (s *Server) Stop() {
	close(s.quit)
	if s.lis != nil {
		s.lis.Close()
	}
}

// ──────────────────────────────────────────────
// 4. Client 구현 (clientconn.go, call.go 참조)
// ──────────────────────────────────────────────

// ClientConn은 gRPC 클라이언트 연결의 간소화 버전이다.
// 실제: clientconn.go — target, conn, transport 등
type ClientConn struct {
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
}

// Dial은 서버에 연결한다.
// 실제: grpc.Dial / grpc.NewClient
func Dial(addr string) (*ClientConn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("연결 실패: %w", err)
	}
	return &ClientConn{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		decoder: json.NewDecoder(conn),
	}, nil
}

// Invoke는 Unary RPC를 호출한다.
// 실제: call.go의 Invoke → invoke → newClientStream → SendMsg → RecvMsg
func (cc *ClientConn) Invoke(ctx context.Context, service, method string, req any, resp any) error {
	// 요청 직렬화
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("요청 직렬화 실패: %w", err)
	}

	rpcReq := RPCRequest{
		ServiceName: service,
		MethodName:  method,
		Payload:     payload,
	}

	// 전송
	if err := cc.encoder.Encode(rpcReq); err != nil {
		return fmt.Errorf("전송 실패: %w", err)
	}

	// 응답 수신
	var rpcResp RPCResponse
	if err := cc.decoder.Decode(&rpcResp); err != nil {
		return fmt.Errorf("수신 실패: %w", err)
	}

	if rpcResp.Error != "" {
		return fmt.Errorf("RPC 에러: %s", rpcResp.Error)
	}

	// 응답 역직렬화
	return json.Unmarshal(rpcResp.Payload, resp)
}

// Close는 연결을 닫는다.
func (cc *ClientConn) Close() {
	cc.conn.Close()
}

// ──────────────────────────────────────────────
// 5. 예시 서비스: Greeter (helloworld 예제 참조)
// ──────────────────────────────────────────────

// HelloRequest / HelloReply — 실제 protobuf 메시지에 대응
type HelloRequest struct {
	Name string `json:"name"`
}
type HelloReply struct {
	Message string `json:"message"`
}

// GreeterService는 서비스 구현체.
type GreeterService struct{}

func (g *GreeterService) SayHello(name string) string {
	return fmt.Sprintf("안녕하세요, %s님!", name)
}

// Greeter 서비스의 ServiceDesc 정의
// 실제: protoc-gen-go-grpc가 자동 생성하는 _Greeter_serviceDesc
var GreeterServiceDesc = ServiceDesc{
	ServiceName: "helloworld.Greeter",
	HandlerType: (*GreeterService)(nil),
	Methods: []MethodDesc{
		{
			MethodName: "SayHello",
			Handler: func(srv any, ctx context.Context, req []byte) ([]byte, error) {
				var in HelloRequest
				if err := json.Unmarshal(req, &in); err != nil {
					return nil, err
				}
				greeter := srv.(*GreeterService)
				reply := HelloReply{Message: greeter.SayHello(in.Name)}
				return json.Marshal(reply)
			},
		},
		{
			MethodName: "SayGoodbye",
			Handler: func(srv any, ctx context.Context, req []byte) ([]byte, error) {
				var in HelloRequest
				if err := json.Unmarshal(req, &in); err != nil {
					return nil, err
				}
				reply := HelloReply{Message: fmt.Sprintf("안녕히 가세요, %s님!", in.Name)}
				return json.Marshal(reply)
			},
		},
	},
}

// ──────────────────────────────────────────────
// 6. main: 서버 시작 → 클라이언트 호출
// ──────────────────────────────────────────────

func main() {
	fmt.Println("=== gRPC 아키텍처 시뮬레이션 ===")
	fmt.Println()

	// 서버 생성 및 서비스 등록
	server := NewServer()
	server.RegisterService(&GreeterServiceDesc, &GreeterService{})

	// 리스너 시작
	lis, err := net.Listen("tcp", "127.0.0.1:0") // 임의 포트
	if err != nil {
		log.Fatalf("리스너 생성 실패: %v", err)
	}
	addr := lis.Addr().String()

	// 서버를 고루틴에서 실행
	go server.Serve(lis)

	// 클라이언트 연결
	cc, err := Dial(addr)
	if err != nil {
		log.Fatalf("연결 실패: %v", err)
	}
	defer cc.Close()

	// RPC 호출 1: SayHello
	fmt.Println("[클라이언트] SayHello 호출...")
	var reply1 HelloReply
	err = cc.Invoke(context.Background(), "helloworld.Greeter", "SayHello",
		HelloRequest{Name: "gRPC"}, &reply1)
	if err != nil {
		log.Fatalf("RPC 실패: %v", err)
	}
	fmt.Printf("[클라이언트] 응답: %s\n", reply1.Message)

	// RPC 호출 2: SayGoodbye
	fmt.Println("[클라이언트] SayGoodbye 호출...")
	var reply2 HelloReply
	err = cc.Invoke(context.Background(), "helloworld.Greeter", "SayGoodbye",
		HelloRequest{Name: "World"}, &reply2)
	if err != nil {
		log.Fatalf("RPC 실패: %v", err)
	}
	fmt.Printf("[클라이언트] 응답: %s\n", reply2.Message)

	// RPC 호출 3: 존재하지 않는 메서드 (에러 케이스)
	fmt.Println("[클라이언트] 존재하지 않는 메서드 호출...")
	var reply3 HelloReply
	err = cc.Invoke(context.Background(), "helloworld.Greeter", "Unknown",
		HelloRequest{Name: "test"}, &reply3)
	if err != nil {
		fmt.Printf("[클라이언트] 예상된 에러: %v\n", err)
	}

	server.Stop()
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
