// poc-04-stream: 멀티플렉스 스트림 관리
//
// HTTP/2 위에서 동작하는 gRPC 스트림 멀티플렉싱을 시뮬레이션한다.
// - 하나의 TCP 연결에서 여러 스트림을 동시에 처리
// - 스트림 ID 기반 멀티플렉싱 (홀수=클라이언트, 짝수=서버)
// - 4가지 RPC 패턴: Unary, Server Streaming, Client Streaming, Bidi
// - 스트림 상태 관리 (Open → HalfClosed → Closed)
//
// 실제 gRPC 참조:
//   internal/transport/http2_server.go → operateHeaders, handleData
//   internal/transport/transport.go   → Stream, ServerTransport
//   stream.go                         → ClientStream, ServerStream

package main

import (
	"fmt"
	"sync"
	"time"
)

// ──────────────────────────────────────────────
// 1. 스트림 상태 및 타입 정의
// ──────────────────────────────────────────────

// StreamState는 HTTP/2 스트림 상태 (RFC 7540 Section 5.1)
type StreamState int

const (
	StreamOpen       StreamState = iota // 양방향 데이터 전송 가능
	StreamHalfClosed                    // 한쪽이 END_STREAM 전송
	StreamClosed                        // 완전히 종료
)

func (s StreamState) String() string {
	switch s {
	case StreamOpen:
		return "Open"
	case StreamHalfClosed:
		return "HalfClosed"
	case StreamClosed:
		return "Closed"
	default:
		return "Unknown"
	}
}

// RPCType은 gRPC RPC 패턴 유형.
type RPCType int

const (
	Unary           RPCType = iota // 1:1
	ServerStreaming                // 1:N (서버가 스트림)
	ClientStreaming                // N:1 (클라이언트가 스트림)
	BidiStreaming                  // N:M (양방향 스트림)
)

func (r RPCType) String() string {
	switch r {
	case Unary:
		return "Unary"
	case ServerStreaming:
		return "ServerStreaming"
	case ClientStreaming:
		return "ClientStreaming"
	case BidiStreaming:
		return "BidiStreaming"
	default:
		return "Unknown"
	}
}

// ──────────────────────────────────────────────
// 2. Stream 구조체 (transport.go 참조)
// ──────────────────────────────────────────────

// Message는 스트림을 통해 전달되는 메시지.
type Message struct {
	StreamID uint32
	Data     string
	EndFlag  bool // END_STREAM 플래그
}

// Stream은 하나의 HTTP/2 스트림.
// 실제: internal/transport/transport.go의 Stream 구조체
type Stream struct {
	id       uint32
	method   string      // /service/method
	state    StreamState
	rpcType  RPCType
	recvBuf  chan Message // 수신 버퍼
	sendBuf  chan Message // 송신 버퍼
	done     chan struct{}
	mu       sync.Mutex
}

func newStream(id uint32, method string, rpcType RPCType) *Stream {
	return &Stream{
		id:      id,
		method:  method,
		state:   StreamOpen,
		rpcType: rpcType,
		recvBuf: make(chan Message, 10),
		sendBuf: make(chan Message, 10),
		done:    make(chan struct{}),
	}
}

// SendMsg는 메시지를 스트림으로 전송한다.
func (s *Stream) SendMsg(data string, end bool) {
	s.mu.Lock()
	if s.state == StreamClosed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	msg := Message{StreamID: s.id, Data: data, EndFlag: end}
	s.sendBuf <- msg

	if end {
		s.mu.Lock()
		if s.state == StreamOpen {
			s.state = StreamHalfClosed
		} else {
			s.state = StreamClosed
			close(s.done)
		}
		s.mu.Unlock()
	}
}

// RecvMsg는 스트림에서 메시지를 수신한다.
func (s *Stream) RecvMsg() (Message, bool) {
	select {
	case msg := <-s.recvBuf:
		if msg.EndFlag {
			s.mu.Lock()
			if s.state == StreamOpen {
				s.state = StreamHalfClosed
			} else {
				s.state = StreamClosed
				close(s.done)
			}
			s.mu.Unlock()
		}
		return msg, true
	case <-s.done:
		return Message{}, false
	}
}

// ──────────────────────────────────────────────
// 3. Transport: 멀티플렉서 (http2_server.go 참조)
// ──────────────────────────────────────────────

// Transport는 하나의 TCP 연결 위에서 여러 스트림을 관리한다.
// 실제: internal/transport/http2_server.go의 http2Server
type Transport struct {
	mu            sync.Mutex
	streams       map[uint32]*Stream
	nextStreamID  uint32 // 클라이언트: 홀수(1,3,5...), 서버: 짝수(2,4,6...)
	maxStreams     int
	activeStreams  int
	side          string
	messageLog    []string // 시뮬레이션용 메시지 로그
}

func newTransport(side string, startID uint32, maxStreams int) *Transport {
	return &Transport{
		streams:      make(map[uint32]*Stream),
		nextStreamID: startID,
		maxStreams:    maxStreams,
		side:         side,
	}
}

// CreateStream은 새 스트림을 생성한다.
// 실제: http2Client의 NewStream → operateHeaders
func (t *Transport) CreateStream(method string, rpcType RPCType) (*Stream, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.activeStreams >= t.maxStreams {
		return nil, fmt.Errorf("최대 동시 스트림 수 초과 (max=%d)", t.maxStreams)
	}

	id := t.nextStreamID
	t.nextStreamID += 2 // 홀수/짝수 유지
	s := newStream(id, method, rpcType)
	t.streams[id] = s
	t.activeStreams++

	msg := fmt.Sprintf("[%s] 스트림 #%d 생성: %s [%s] (활성: %d/%d)",
		t.side, id, method, rpcType, t.activeStreams, t.maxStreams)
	t.messageLog = append(t.messageLog, msg)
	fmt.Println(msg)
	return s, nil
}

// CloseStream은 스트림을 닫는다.
// 실제: deleteStream / closeStream
func (t *Transport) CloseStream(id uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if s, ok := t.streams[id]; ok {
		s.mu.Lock()
		s.state = StreamClosed
		s.mu.Unlock()
		delete(t.streams, id)
		t.activeStreams--

		msg := fmt.Sprintf("[%s] 스트림 #%d 종료: %s (활성: %d/%d)",
			t.side, id, s.method, t.activeStreams, t.maxStreams)
		t.messageLog = append(t.messageLog, msg)
		fmt.Println(msg)
	}
}

// GetStream은 ID로 스트림을 조회한다.
func (t *Transport) GetStream(id uint32) *Stream {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streams[id]
}

// ActiveCount는 활성 스트림 수를 반환한다.
func (t *Transport) ActiveCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.activeStreams
}

// ──────────────────────────────────────────────
// 4. RPC 패턴 시뮬레이션 함수
// ──────────────────────────────────────────────

// simulateUnary: 1개 요청 → 1개 응답
func simulateUnary(t *Transport) {
	s, err := t.CreateStream("/greeter/SayHello", Unary)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	// 클라이언트: 요청 1개 + END_STREAM
	s.SendMsg("Hello", true)
	fmt.Printf("  [stream#%d] 클라이언트 → 서버: 'Hello' (END_STREAM)\n", s.id)

	// 서버: 응답 1개 + END_STREAM (시뮬레이션)
	s.recvBuf <- Message{StreamID: s.id, Data: "Hi!", EndFlag: true}
	msg, _ := s.RecvMsg()
	fmt.Printf("  [stream#%d] 서버 → 클라이언트: '%s' (END_STREAM)\n", s.id, msg.Data)

	t.CloseStream(s.id)
}

// simulateServerStreaming: 1개 요청 → N개 응답
func simulateServerStreaming(t *Transport) {
	s, err := t.CreateStream("/stock/Subscribe", ServerStreaming)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	// 클라이언트: 요청 1개 + END_STREAM
	s.SendMsg("AAPL", true)
	fmt.Printf("  [stream#%d] 클라이언트 → 서버: 구독 'AAPL' (END_STREAM)\n", s.id)

	// 서버: 여러 응답 스트리밍
	prices := []string{"$150.00", "$151.20", "$149.80"}
	go func() {
		for i, price := range prices {
			isLast := i == len(prices)-1
			s.recvBuf <- Message{StreamID: s.id, Data: price, EndFlag: isLast}
		}
	}()

	for {
		msg, ok := s.RecvMsg()
		if !ok {
			break
		}
		endStr := ""
		if msg.EndFlag {
			endStr = " (END_STREAM)"
		}
		fmt.Printf("  [stream#%d] 서버 → 클라이언트: %s%s\n", s.id, msg.Data, endStr)
		if msg.EndFlag {
			break
		}
	}
	t.CloseStream(s.id)
}

// simulateClientStreaming: N개 요청 → 1개 응답
func simulateClientStreaming(t *Transport) {
	s, err := t.CreateStream("/upload/UploadFile", ClientStreaming)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}

	// 클라이언트: 여러 청크 전송
	chunks := []string{"chunk-1", "chunk-2", "chunk-3"}
	for i, chunk := range chunks {
		isLast := i == len(chunks)-1
		s.SendMsg(chunk, isLast)
		endStr := ""
		if isLast {
			endStr = " (END_STREAM)"
		}
		fmt.Printf("  [stream#%d] 클라이언트 → 서버: %s%s\n", s.id, chunk, endStr)
	}

	// 서버: 요약 응답
	s.recvBuf <- Message{StreamID: s.id, Data: "3 chunks received", EndFlag: true}
	msg, _ := s.RecvMsg()
	fmt.Printf("  [stream#%d] 서버 → 클라이언트: '%s' (END_STREAM)\n", s.id, msg.Data)

	t.CloseStream(s.id)
}

// simulateBidiStreaming: N개 요청 ↔ M개 응답 (동시)
func simulateBidiStreaming(t *Transport) {
	s, err := t.CreateStream("/chat/LiveChat", BidiStreaming)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}

	var wg sync.WaitGroup

	// 클라이언트 → 서버 (송신)
	wg.Add(1)
	go func() {
		defer wg.Done()
		messages := []string{"안녕!", "잘 지내?", "끝!"}
		for i, msg := range messages {
			isLast := i == len(messages)-1
			s.SendMsg(msg, isLast)
			endStr := ""
			if isLast {
				endStr = " (END_STREAM)"
			}
			fmt.Printf("  [stream#%d] 클라이언트 → 서버: '%s'%s\n", s.id, msg, endStr)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// 서버 → 클라이언트 (수신 시뮬레이션)
	wg.Add(1)
	go func() {
		defer wg.Done()
		replies := []string{"반가워!", "나도 잘 지내!", "안녕!"}
		for i, reply := range replies {
			isLast := i == len(replies)-1
			s.recvBuf <- Message{StreamID: s.id, Data: reply, EndFlag: isLast}
			time.Sleep(30 * time.Millisecond)
		}
	}()

	// 수신 측
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			msg, ok := s.RecvMsg()
			if !ok {
				break
			}
			endStr := ""
			if msg.EndFlag {
				endStr = " (END_STREAM)"
			}
			fmt.Printf("  [stream#%d] 서버 → 클라이언트: '%s'%s\n", s.id, msg.Data, endStr)
			if msg.EndFlag {
				break
			}
		}
	}()

	wg.Wait()
	t.CloseStream(s.id)
}

// ──────────────────────────────────────────────
// 5. main
// ──────────────────────────────────────────────

func main() {
	fmt.Println("=== 멀티플렉스 스트림 관리 시뮬레이션 ===")
	fmt.Println()

	// 클라이언트 트랜스포트: 홀수 ID, 최대 4 동시 스트림
	transport := newTransport("클라이언트", 1, 4)

	// 1. Unary RPC
	fmt.Println("── 1. Unary RPC (1:1) ──")
	simulateUnary(transport)
	fmt.Println()

	// 2. Server Streaming RPC
	fmt.Println("── 2. Server Streaming RPC (1:N) ──")
	simulateServerStreaming(transport)
	fmt.Println()

	// 3. Client Streaming RPC
	fmt.Println("── 3. Client Streaming RPC (N:1) ──")
	simulateClientStreaming(transport)
	fmt.Println()

	// 4. Bidi Streaming RPC
	fmt.Println("── 4. Bidi Streaming RPC (N:M) ──")
	simulateBidiStreaming(transport)
	fmt.Println()

	// 5. 동시 스트림 멀티플렉싱
	fmt.Println("── 5. 동시 스트림 멀티플렉싱 ──")
	transport2 := newTransport("클라이언트", 1, 3)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		method := fmt.Sprintf("/service/Method%d", i+1)
		go func(m string) {
			defer wg.Done()
			s, err := transport2.CreateStream(m, Unary)
			if err != nil {
				fmt.Printf("  %s: %v\n", m, err)
				return
			}
			time.Sleep(50 * time.Millisecond) // 작업 시뮬레이션
			transport2.CloseStream(s.id)
		}(method)
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()

	fmt.Printf("\n  최종 활성 스트림: %d\n", transport2.ActiveCount())
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
