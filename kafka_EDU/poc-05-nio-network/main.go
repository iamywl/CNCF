package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Kafka NIO-Style Network Architecture Simulation
// Based on: SocketServer.scala, Selector.java
//
// Kafka의 네트워크 레이어는 Java NIO 기반의 Reactor 패턴을 사용한다.
// 구조: Acceptor(1개) → Processor(N개) → RequestChannel → Handler(M개)
//
// 이 PoC는 Go의 goroutine과 channel을 사용하여 동일한 아키텍처를 시뮬레이션한다.
// - Acceptor: 새 연결을 수락하고 라운드로빈으로 Processor에 분배
// - Processor: 각자의 연결 집합을 관리, 크기 프리픽스 메시지 프레이밍 수행
// - RequestChannel: Processor와 Handler 사이의 버퍼링된 큐
// - 크기 프리픽스 프레이밍: 4바이트 길이 헤더 + 페이로드 (Kafka 와이어 프로토콜 동일)
// =============================================================================

const (
	NumProcessors     = 3
	RequestQueueSize  = 100
	ResponseQueueSize = 50
	NumHandlers       = 2
	ListenAddr        = "127.0.0.1:0" // OS가 포트를 할당
)

// --- Message Framing ---
// Kafka는 모든 메시지에 4바이트 크기 프리픽스를 사용한다.
// [4 bytes: message length][N bytes: payload]
// SocketServer.scala의 Processor.processCompletedReceives()에서 이 형식으로 읽기를 수행한다.

// Request는 Processor가 수신한 요청을 나타낸다.
// RequestChannel.Request와 대응된다.
type Request struct {
	ProcessorID   int
	ConnectionID  string
	CorrelationID int32
	Payload       []byte
	ReceivedAt    time.Time
}

// Response는 Handler가 생성한 응답을 나타낸다.
// RequestChannel.Response와 대응된다.
type Response struct {
	ConnectionID  string
	CorrelationID int32
	Payload       []byte
}

// RequestChannel은 Processor와 Handler 사이의 요청 큐이다.
// Kafka의 RequestChannel.scala와 대응된다.
// maxQueuedRequests로 큐 크기를 제한하여 백프레셔를 구현한다.
type RequestChannel struct {
	requestQueue  chan *Request
	responseQueue map[int]chan *Response // processorID → response channel
	mu            sync.RWMutex
}

func NewRequestChannel(maxRequests int) *RequestChannel {
	return &RequestChannel{
		requestQueue:  make(chan *Request, maxRequests),
		responseQueue: make(map[int]chan *Response),
	}
}

func (rc *RequestChannel) AddProcessor(id int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.responseQueue[id] = make(chan *Response, ResponseQueueSize)
}

func (rc *RequestChannel) SendRequest(req *Request) {
	rc.requestQueue <- req
}

func (rc *RequestChannel) ReceiveRequest(timeoutMs int) *Request {
	select {
	case req := <-rc.requestQueue:
		return req
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil
	}
}

func (rc *RequestChannel) SendResponse(processorID int, resp *Response) {
	rc.mu.RLock()
	ch, ok := rc.responseQueue[processorID]
	rc.mu.RUnlock()
	if ok {
		ch <- resp
	}
}

func (rc *RequestChannel) ResponseQueue(processorID int) chan *Response {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.responseQueue[processorID]
}

// Processor는 Kafka의 Processor 클래스에 대응된다.
// 각 Processor는 자체 연결 목록을 관리하고, NIO Selector 대신
// Go의 goroutine per connection + channel을 사용하여 I/O 다중화를 시뮬레이션한다.
//
// 실제 Kafka에서는 하나의 Processor가 Java NIO Selector를 통해
// 수백 개의 연결을 단일 스레드에서 다중화한다.
// Go에서는 goroutine이 경량이므로 연결당 goroutine으로 유사하게 구현한다.
type Processor struct {
	id             int
	requestChannel *RequestChannel
	connections    sync.Map // connectionID → net.Conn
	newConnQueue   chan net.Conn
	stats          ProcessorStats
	wg             sync.WaitGroup
	stopCh         chan struct{}
}

type ProcessorStats struct {
	receivedRequests  atomic.Int64
	sentResponses     atomic.Int64
	activeConnections atomic.Int64
}

func NewProcessor(id int, rc *RequestChannel) *Processor {
	rc.AddProcessor(id)
	return &Processor{
		id:             id,
		requestChannel: rc,
		newConnQueue:   make(chan net.Conn, 20),
		stopCh:         make(chan struct{}),
	}
}

// Start는 Processor의 메인 루프를 시작한다.
// Kafka Processor.run()과 대응:
//   configureNewConnections() → 새 연결 등록
//   processNewResponses()     → 응답 전송
//   poll()                    → NIO Selector.poll()
//   processCompletedReceives() → 수신 완료 처리
func (p *Processor) Start() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.stopCh:
				return
			case conn := <-p.newConnQueue:
				// configureNewConnections(): 새 연결을 Selector에 등록
				connID := fmt.Sprintf("conn-%d-%d", p.id, p.stats.activeConnections.Load())
				p.connections.Store(connID, conn)
				p.stats.activeConnections.Add(1)
				fmt.Printf("  [Processor-%d] 새 연결 등록: %s (원격: %s)\n", p.id, connID, conn.RemoteAddr())

				// 연결별 읽기 goroutine 시작 (NIO Selector.poll() + processCompletedReceives() 시뮬레이션)
				p.wg.Add(1)
				go p.readLoop(connID, conn)
			}
		}
	}()

	// 응답 전송 goroutine (processNewResponses() 시뮬레이션)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		respCh := p.requestChannel.ResponseQueue(p.id)
		for {
			select {
			case <-p.stopCh:
				return
			case resp := <-respCh:
				p.sendResponse(resp)
			}
		}
	}()
}

// readLoop은 하나의 연결에서 크기 프리픽스 메시지를 읽는다.
// Kafka의 Size-Prefixed 프레이밍: [4 bytes: length][length bytes: payload]
// 이는 Kafka 와이어 프로토콜의 기본 프레이밍 방식이다.
func (p *Processor) readLoop(connID string, conn net.Conn) {
	defer p.wg.Done()
	defer func() {
		conn.Close()
		p.connections.Delete(connID)
		p.stats.activeConnections.Add(-1)
		fmt.Printf("  [Processor-%d] 연결 종료: %s\n", p.id, connID)
	}()

	for {
		// 4바이트 크기 헤더 읽기
		sizeBuf := make([]byte, 4)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := io.ReadFull(conn, sizeBuf)
		if err != nil {
			return
		}
		msgSize := int(binary.BigEndian.Uint32(sizeBuf))
		if msgSize <= 0 || msgSize > 1024*1024 {
			return
		}

		// 페이로드 읽기
		payload := make([]byte, msgSize)
		_, err = io.ReadFull(conn, payload)
		if err != nil {
			return
		}

		// 페이로드에서 correlationID 추출 (처음 4바이트)
		var correlationID int32
		if len(payload) >= 4 {
			correlationID = int32(binary.BigEndian.Uint32(payload[:4]))
		}

		req := &Request{
			ProcessorID:   p.id,
			ConnectionID:  connID,
			CorrelationID: correlationID,
			Payload:       payload,
			ReceivedAt:    time.Now(),
		}

		p.stats.receivedRequests.Add(1)
		// RequestChannel에 요청 전달 (Processor → Handler)
		p.requestChannel.SendRequest(req)
	}
}

// sendResponse는 연결에 응답을 전송한다.
// 크기 프리픽스 프레이밍으로 응답을 작성한다.
func (p *Processor) sendResponse(resp *Response) {
	val, ok := p.connections.Load(resp.ConnectionID)
	if !ok {
		return
	}
	conn := val.(net.Conn)

	// 크기 프리픽스 + 페이로드 전송
	sizeBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(sizeBuf, uint32(len(resp.Payload)))
	conn.Write(sizeBuf)
	conn.Write(resp.Payload)
	p.stats.sentResponses.Add(1)
}

func (p *Processor) Accept(conn net.Conn) {
	p.newConnQueue <- conn
}

func (p *Processor) Stop() {
	close(p.stopCh)
}

// Acceptor는 Kafka의 Acceptor 클래스에 대응된다.
// 서버 소켓에서 새 연결을 수락하고 라운드로빈으로 Processor에 분배한다.
//
// SocketServer.scala의 Acceptor.run():
//   serverChannel.register(nioSelector, SelectionKey.OP_ACCEPT)
//   while (shouldRun.get()) { acceptNewConnections() }
//
// acceptNewConnections()에서 라운드로빈으로 Processor를 선택:
//   currentProcessorIndex = currentProcessorIndex % processors.size
type Acceptor struct {
	listener   net.Listener
	processors []*Processor
	currentIdx int
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

func NewAcceptor(listener net.Listener, processors []*Processor) *Acceptor {
	return &Acceptor{
		listener:   listener,
		processors: processors,
		stopCh:     make(chan struct{}),
	}
}

// Start는 Acceptor의 수락 루프를 시작한다.
// 새 연결을 수락하면 라운드로빈으로 Processor를 선택하여 전달한다.
func (a *Acceptor) Start() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			conn, err := a.listener.Accept()
			if err != nil {
				select {
				case <-a.stopCh:
					return
				default:
					continue
				}
			}

			// 라운드로빈으로 Processor 선택 (Kafka의 currentProcessorIndex와 동일)
			processor := a.processors[a.currentIdx%len(a.processors)]
			a.currentIdx++
			fmt.Printf("[Acceptor] 연결 수락 → Processor-%d로 할당 (원격: %s)\n", processor.id, conn.RemoteAddr())
			processor.Accept(conn)
		}
	}()
}

func (a *Acceptor) Stop() {
	close(a.stopCh)
	a.listener.Close()
}

// --- Client: 테스트용 클라이언트 ---
// 크기 프리픽스 프레이밍으로 메시지를 전송하고 응답을 수신한다.
func sendMessage(conn net.Conn, correlationID int32, message string) error {
	// 페이로드: [4 bytes correlationID][message bytes]
	msgBytes := []byte(message)
	payload := make([]byte, 4+len(msgBytes))
	binary.BigEndian.PutUint32(payload[:4], uint32(correlationID))
	copy(payload[4:], msgBytes)

	// 크기 프리픽스 전송
	sizeBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(sizeBuf, uint32(len(payload)))
	if _, err := conn.Write(sizeBuf); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

func receiveMessage(conn net.Conn) (int32, string, error) {
	sizeBuf := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, sizeBuf); err != nil {
		return 0, "", err
	}
	msgSize := int(binary.BigEndian.Uint32(sizeBuf))

	payload := make([]byte, msgSize)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, "", err
	}

	correlationID := int32(binary.BigEndian.Uint32(payload[:4]))
	message := string(payload[4:])
	return correlationID, message, nil
}

func main() {
	fmt.Println("=== Kafka NIO-Style Network Architecture PoC ===")
	fmt.Println()
	fmt.Println("Kafka 네트워크 레이어 구조:")
	fmt.Println("  Client → [Acceptor] → [Processor-0..N] → [RequestChannel] → [Handler-0..M]")
	fmt.Println("                                 ↑                                    |")
	fmt.Println("                                 └──────── Response Queue ←────────────┘")
	fmt.Println()

	// --- 1. 네트워크 레이어 초기화 ---
	fmt.Println("--- 1단계: 네트워크 레이어 초기화 ---")

	requestChannel := NewRequestChannel(RequestQueueSize)

	// Processor 생성 (Kafka의 num.network.threads 설정)
	processors := make([]*Processor, NumProcessors)
	for i := 0; i < NumProcessors; i++ {
		processors[i] = NewProcessor(i, requestChannel)
		processors[i].Start()
		fmt.Printf("  Processor-%d 시작\n", i)
	}

	// 리스너 생성
	listener, err := net.Listen("tcp", ListenAddr)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  서버 리스닝: %s\n", listener.Addr())

	// Acceptor 시작
	acceptor := NewAcceptor(listener, processors)
	acceptor.Start()
	fmt.Println("  Acceptor 시작")
	fmt.Println()

	// --- 2. Handler 시작 (RequestChannel에서 요청을 꺼내 처리) ---
	fmt.Println("--- 2단계: Handler Pool 시작 ---")

	var handlerWg sync.WaitGroup
	handlerStop := make(chan struct{})

	for i := 0; i < NumHandlers; i++ {
		handlerWg.Add(1)
		handlerID := i
		go func() {
			defer handlerWg.Done()
			for {
				select {
				case <-handlerStop:
					return
				default:
				}

				req := requestChannel.ReceiveRequest(200)
				if req == nil {
					continue
				}

				// 요청 처리: correlationID를 추출하고 응답을 생성
				var correlationID int32
				var msgContent string
				if len(req.Payload) >= 4 {
					correlationID = int32(binary.BigEndian.Uint32(req.Payload[:4]))
					msgContent = string(req.Payload[4:])
				}

				fmt.Printf("  [Handler-%d] 요청 처리: conn=%s, correlationID=%d, 내용=%q\n",
					handlerID, req.ConnectionID, correlationID, msgContent)

				// 응답 생성 (correlationID 매칭 유지)
				respMsg := fmt.Sprintf("ACK:%s", msgContent)
				respPayload := make([]byte, 4+len(respMsg))
				binary.BigEndian.PutUint32(respPayload[:4], uint32(correlationID))
				copy(respPayload[4:], respMsg)

				resp := &Response{
					ConnectionID:  req.ConnectionID,
					CorrelationID: correlationID,
					Payload:       respPayload,
				}

				// 응답을 원래 Processor의 응답 큐로 전달
				requestChannel.SendResponse(req.ProcessorID, resp)
			}
		}()
		fmt.Printf("  Handler-%d 시작\n", i)
	}
	fmt.Println()

	// --- 3. 클라이언트 시뮬레이션 ---
	fmt.Println("--- 3단계: 클라이언트 연결 및 메시지 전송 ---")
	time.Sleep(100 * time.Millisecond)

	var clientWg sync.WaitGroup
	numClients := 5
	messagesPerClient := 3

	for c := 0; c < numClients; c++ {
		clientWg.Add(1)
		clientID := c
		go func() {
			defer clientWg.Done()

			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				fmt.Printf("  [Client-%d] 연결 실패: %v\n", clientID, err)
				return
			}
			defer conn.Close()

			time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)

			for m := 0; m < messagesPerClient; m++ {
				correlationID := int32(clientID*100 + m)
				message := fmt.Sprintf("client%d-msg%d", clientID, m)

				err := sendMessage(conn, correlationID, message)
				if err != nil {
					fmt.Printf("  [Client-%d] 전송 실패: %v\n", clientID, err)
					return
				}

				// 응답 수신 및 correlationID 매칭 확인
				respCorrelation, respMsg, err := receiveMessage(conn)
				if err != nil {
					fmt.Printf("  [Client-%d] 수신 실패: %v\n", clientID, err)
					return
				}

				matched := "OK"
				if respCorrelation != correlationID {
					matched = "MISMATCH!"
				}
				fmt.Printf("  [Client-%d] 응답 수신: correlationID=%d, 내용=%q, 매칭=%s\n",
					clientID, respCorrelation, respMsg, matched)
			}
		}()
	}

	clientWg.Wait()
	time.Sleep(200 * time.Millisecond)
	fmt.Println()

	// --- 4. 통계 출력 ---
	fmt.Println("--- 4단계: 통계 ---")
	for i, p := range processors {
		fmt.Printf("  Processor-%d: 수신=%d, 응답=%d, 활성연결=%d\n",
			i, p.stats.receivedRequests.Load(), p.stats.sentResponses.Load(), p.stats.activeConnections.Load())
	}
	fmt.Println()

	// --- 5. 정리 ---
	fmt.Println("--- 5단계: 정리 ---")
	close(handlerStop)
	handlerWg.Wait()
	acceptor.Stop()
	for _, p := range processors {
		p.Stop()
	}
	fmt.Println("  모든 컴포넌트 종료 완료")
	fmt.Println()

	// --- 아키텍처 요약 ---
	fmt.Println("=== 아키텍처 요약 ===")
	fmt.Println()
	fmt.Println("Kafka SocketServer 스레딩 모델 (SocketServer.scala):")
	fmt.Println("  1 Acceptor thread  → 새 연결 수락, 라운드로빈으로 Processor에 분배")
	fmt.Println("  N Processor threads → 각자의 NIO Selector로 연결 I/O 다중화")
	fmt.Println("  M Handler threads  → RequestChannel에서 요청을 꺼내 KafkaApis로 라우팅")
	fmt.Println()
	fmt.Println("메시지 프레이밍 (Kafka Wire Protocol):")
	fmt.Println("  [4 bytes: message size][N bytes: payload]")
	fmt.Println("  페이로드 첫 4바이트가 correlationID → 요청-응답 매칭에 사용")
	fmt.Println()
	fmt.Println("Go 시뮬레이션 매핑:")
	fmt.Println("  Java NIO Selector     → Go goroutine per connection + channel")
	fmt.Println("  ArrayBlockingQueue    → Go buffered channel")
	fmt.Println("  Thread                → Go goroutine")
	fmt.Println("  SelectionKey.OP_READ  → Go io.ReadFull() in goroutine")
}
