package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Kafka Request Processing Pipeline Simulation
// Based on: KafkaApis.scala, KafkaRequestHandler.scala, RequestChannel.scala
//
// Kafka의 요청 처리 파이프라인:
//   Acceptor → Processor → RequestChannel → KafkaRequestHandler → KafkaApis
//
// KafkaRequestHandler는 RequestChannel에서 요청을 꺼내고,
// KafkaApis.handle()이 API 키에 따라 적절한 핸들러로 라우팅한다.
//
// 이 PoC는 전체 파이프라인을 시뮬레이션하며 다음을 포함한다:
// - API 키별 라우팅 (PRODUCE=0, FETCH=1, METADATA=3)
// - CorrelationID 기반 요청-응답 매칭
// - 설정 가능한 Handler Pool 크기
// =============================================================================

// --- API Keys (Kafka 프로토콜 정의) ---
// common/src/main/java/org/apache/kafka/common/protocol/ApiKeys.java
const (
	ApiKeyProduce  int16 = 0
	ApiKeyFetch    int16 = 1
	ApiKeyMetadata int16 = 3
)

func apiKeyName(key int16) string {
	switch key {
	case ApiKeyProduce:
		return "PRODUCE"
	case ApiKeyFetch:
		return "FETCH"
	case ApiKeyMetadata:
		return "METADATA"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", key)
	}
}

// RequestHeader는 Kafka 요청 헤더를 나타낸다.
// common/src/main/java/org/apache/kafka/common/requests/RequestHeader.java
// 구조: [apiKey: 2B][apiVersion: 2B][correlationId: 4B][clientId length: 2B][clientId: NB]
type RequestHeader struct {
	ApiKey        int16
	ApiVersion    int16
	CorrelationID int32
	ClientID      string
}

// Request는 RequestChannel을 통해 전달되는 요청이다.
// core/src/main/scala/kafka/network/RequestChannel.scala의 Request 클래스
type Request struct {
	Header       RequestHeader
	Payload      []byte
	ProcessorID  int
	ConnectionID string
	EnqueueTime  time.Time
	DequeueTime  time.Time
}

// Response는 Handler가 생성하여 Processor에 전달하는 응답이다.
type Response struct {
	CorrelationID int32
	ProcessorID   int
	ConnectionID  string
	ApiKey        int16
	Payload       []byte
}

// RequestChannel은 Processor와 Handler 사이의 큐이다.
// core/src/main/scala/kafka/network/RequestChannel.scala
// queuedMaxRequests로 최대 큐 크기를 제한한다 (백프레셔).
type RequestChannel struct {
	requestQueue   chan *Request
	responseQueues map[int]chan *Response
	mu             sync.RWMutex
	stats          RequestChannelStats
}

type RequestChannelStats struct {
	totalRequests  atomic.Int64
	totalResponses atomic.Int64
}

func NewRequestChannel(maxQueuedRequests int, numProcessors int) *RequestChannel {
	rc := &RequestChannel{
		requestQueue:   make(chan *Request, maxQueuedRequests),
		responseQueues: make(map[int]chan *Response),
	}
	for i := 0; i < numProcessors; i++ {
		rc.responseQueues[i] = make(chan *Response, 50)
	}
	return rc
}

func (rc *RequestChannel) SendRequest(req *Request) {
	req.EnqueueTime = time.Now()
	rc.requestQueue <- req
	rc.stats.totalRequests.Add(1)
}

func (rc *RequestChannel) ReceiveRequest(timeoutMs int) *Request {
	select {
	case req := <-rc.requestQueue:
		req.DequeueTime = time.Now()
		return req
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil
	}
}

func (rc *RequestChannel) SendResponse(resp *Response) {
	rc.mu.RLock()
	ch := rc.responseQueues[resp.ProcessorID]
	rc.mu.RUnlock()
	ch <- resp
	rc.stats.totalResponses.Add(1)
}

// --- KafkaApis: API 키에 따른 요청 라우팅 ---
// core/src/main/scala/kafka/server/KafkaApis.scala
//
// handle() 메서드가 request.header.apiKey를 match하여 적절한 핸들러를 호출한다:
//   ApiKeys.PRODUCE  → handleProduceRequest()
//   ApiKeys.FETCH    → handleFetchRequest()
//   ApiKeys.METADATA → handleTopicMetadataRequest()
type KafkaApis struct {
	brokerId int
	stats    ApiStats
}

type ApiStats struct {
	produceCount  atomic.Int64
	fetchCount    atomic.Int64
	metadataCount atomic.Int64
}

func NewKafkaApis(brokerId int) *KafkaApis {
	return &KafkaApis{brokerId: brokerId}
}

// Handle은 KafkaApis.handle()에 대응한다.
// apiKey에 따라 적절한 핸들러로 분기한다.
func (ka *KafkaApis) Handle(req *Request) *Response {
	switch req.Header.ApiKey {
	case ApiKeyProduce:
		return ka.handleProduceRequest(req)
	case ApiKeyFetch:
		return ka.handleFetchRequest(req)
	case ApiKeyMetadata:
		return ka.handleTopicMetadataRequest(req)
	default:
		return ka.handleUnknownRequest(req)
	}
}

// handleProduceRequest는 PRODUCE 요청을 처리한다.
// KafkaApis.handleProduceRequest() → ReplicaManager.appendRecords()
func (ka *KafkaApis) handleProduceRequest(req *Request) *Response {
	ka.stats.produceCount.Add(1)

	// 토픽-파티션 정보 추출 (시뮬레이션)
	topic := "unknown"
	partition := 0
	if len(req.Payload) > 0 {
		topic = string(req.Payload)
	}

	// 응답 생성: baseOffset, errorCode, timestamp
	respPayload := fmt.Sprintf("PRODUCE_ACK: topic=%s, partition=%d, baseOffset=%d, errorCode=0",
		topic, partition, rand.Int63n(10000))

	return &Response{
		CorrelationID: req.Header.CorrelationID,
		ProcessorID:   req.ProcessorID,
		ConnectionID:  req.ConnectionID,
		ApiKey:        ApiKeyProduce,
		Payload:       []byte(respPayload),
	}
}

// handleFetchRequest는 FETCH 요청을 처리한다.
// KafkaApis.handleFetchRequest() → ReplicaManager.fetchMessages()
func (ka *KafkaApis) handleFetchRequest(req *Request) *Response {
	ka.stats.fetchCount.Add(1)

	topic := "unknown"
	if len(req.Payload) > 0 {
		topic = string(req.Payload)
	}

	// 시뮬레이션된 레코드 반환
	respPayload := fmt.Sprintf("FETCH_RESP: topic=%s, records=[rec1,rec2], hwm=%d",
		topic, rand.Int63n(10000))

	return &Response{
		CorrelationID: req.Header.CorrelationID,
		ProcessorID:   req.ProcessorID,
		ConnectionID:  req.ConnectionID,
		ApiKey:        ApiKeyFetch,
		Payload:       []byte(respPayload),
	}
}

// handleTopicMetadataRequest는 METADATA 요청을 처리한다.
// KafkaApis.handleTopicMetadataRequest() → MetadataCache 조회
func (ka *KafkaApis) handleTopicMetadataRequest(req *Request) *Response {
	ka.stats.metadataCount.Add(1)

	// 시뮬레이션된 메타데이터 응답
	respPayload := fmt.Sprintf("METADATA_RESP: brokers=[{id=%d,host=localhost,port=9092}], topics=[test-topic(partitions=3)]",
		ka.brokerId)

	return &Response{
		CorrelationID: req.Header.CorrelationID,
		ProcessorID:   req.ProcessorID,
		ConnectionID:  req.ConnectionID,
		ApiKey:        ApiKeyMetadata,
		Payload:       []byte(respPayload),
	}
}

func (ka *KafkaApis) handleUnknownRequest(req *Request) *Response {
	return &Response{
		CorrelationID: req.Header.CorrelationID,
		ProcessorID:   req.ProcessorID,
		ConnectionID:  req.ConnectionID,
		ApiKey:        req.Header.ApiKey,
		Payload:       []byte("ERROR: UNSUPPORTED_API_KEY"),
	}
}

// --- KafkaRequestHandler: 요청 처리 스레드 ---
// core/src/main/scala/kafka/server/KafkaRequestHandler.scala
//
// run() 메서드:
//   while (!stopped) {
//     req = requestChannel.receiveRequest(300)  // 300ms 타임아웃
//     apis.handle(req, requestLocal)            // KafkaApis로 처리 위임
//   }
type KafkaRequestHandler struct {
	id             int
	requestChannel *RequestChannel
	apis           *KafkaApis
	stopCh         chan struct{}
	wg             sync.WaitGroup
	processedCount atomic.Int64
}

func NewKafkaRequestHandler(id int, rc *RequestChannel, apis *KafkaApis) *KafkaRequestHandler {
	return &KafkaRequestHandler{
		id:             id,
		requestChannel: rc,
		apis:           apis,
		stopCh:         make(chan struct{}),
	}
}

func (h *KafkaRequestHandler) Start() {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		for {
			select {
			case <-h.stopCh:
				return
			default:
			}

			// Kafka: requestChannel.receiveRequest(300)
			req := h.requestChannel.ReceiveRequest(300)
			if req == nil {
				continue
			}

			queueTime := req.DequeueTime.Sub(req.EnqueueTime)
			fmt.Printf("  [Handler-%d] 요청 처리: apiKey=%s, correlationID=%d, client=%s, 큐대기=%v\n",
				h.id, apiKeyName(req.Header.ApiKey), req.Header.CorrelationID,
				req.Header.ClientID, queueTime.Round(time.Microsecond))

			// KafkaApis.handle()로 요청 처리 위임
			resp := h.apis.Handle(req)

			fmt.Printf("  [Handler-%d] 응답 생성: correlationID=%d → Processor-%d, 내용=%s\n",
				h.id, resp.CorrelationID, resp.ProcessorID, string(resp.Payload))

			// 응답을 RequestChannel을 통해 원래 Processor로 전달
			h.requestChannel.SendResponse(resp)
			h.processedCount.Add(1)
		}
	}()
}

func (h *KafkaRequestHandler) Stop() {
	close(h.stopCh)
	h.wg.Wait()
}

// --- Processor (시뮬레이션) ---
// 실제 TCP I/O 없이 요청을 생성하는 시뮬레이션 Processor
type SimProcessor struct {
	id             int
	requestChannel *RequestChannel
	responseCh     chan *Response
}

func NewSimProcessor(id int, rc *RequestChannel) *SimProcessor {
	return &SimProcessor{
		id:             id,
		requestChannel: rc,
		responseCh:     rc.responseQueues[id],
	}
}

// --- 시뮬레이션 클라이언트 요청 생성 ---
func encodeRequestHeader(apiKey int16, apiVersion int16, correlationID int32, clientID string) []byte {
	buf := make([]byte, 8+2+len(clientID))
	binary.BigEndian.PutUint16(buf[0:2], uint16(apiKey))
	binary.BigEndian.PutUint16(buf[2:4], uint16(apiVersion))
	binary.BigEndian.PutUint32(buf[4:8], uint32(correlationID))
	binary.BigEndian.PutUint16(buf[8:10], uint16(len(clientID)))
	copy(buf[10:], clientID)
	return buf
}

func main() {
	fmt.Println("=== Kafka Request Processing Pipeline PoC ===")
	fmt.Println()
	fmt.Println("파이프라인 구조:")
	fmt.Println("  [Acceptor] → [Processor] → [RequestChannel] → [KafkaRequestHandler] → [KafkaApis]")
	fmt.Println("                    ↑                                                       |")
	fmt.Println("                    └──────────────── Response Queue ←───────────────────────┘")
	fmt.Println()

	numProcessors := 3
	numHandlers := 4
	brokerId := 1

	// --- 1. 컴포넌트 초기화 ---
	fmt.Println("--- 1단계: 컴포넌트 초기화 ---")
	fmt.Printf("  RequestChannel: maxQueuedRequests=100\n")
	fmt.Printf("  Processors: %d개\n", numProcessors)
	fmt.Printf("  Handlers: %d개 (num.io.threads=%d)\n", numHandlers, numHandlers)
	fmt.Println()

	requestChannel := NewRequestChannel(100, numProcessors)
	kafkaApis := NewKafkaApis(brokerId)

	// Handler Pool 시작 (KafkaRequestHandlerPool과 대응)
	handlers := make([]*KafkaRequestHandler, numHandlers)
	for i := 0; i < numHandlers; i++ {
		handlers[i] = NewKafkaRequestHandler(i, requestChannel, kafkaApis)
		handlers[i].Start()
	}

	// Processor 생성
	processors := make([]*SimProcessor, numProcessors)
	for i := 0; i < numProcessors; i++ {
		processors[i] = NewSimProcessor(i, requestChannel)
	}

	// --- 2. 요청 시뮬레이션 ---
	fmt.Println("--- 2단계: 요청 처리 시뮬레이션 ---")
	fmt.Println()

	// 시뮬레이션할 요청 목록
	type SimRequest struct {
		apiKey   int16
		clientID string
		topic    string
	}

	requests := []SimRequest{
		{ApiKeyProduce, "producer-1", "orders"},
		{ApiKeyFetch, "consumer-1", "orders"},
		{ApiKeyMetadata, "admin-client", ""},
		{ApiKeyProduce, "producer-2", "events"},
		{ApiKeyFetch, "consumer-2", "events"},
		{ApiKeyProduce, "producer-1", "logs"},
		{ApiKeyMetadata, "consumer-1", ""},
		{ApiKeyFetch, "consumer-3", "logs"},
		{ApiKeyProduce, "producer-3", "metrics"},
		{ApiKeyFetch, "consumer-1", "metrics"},
		{ApiKeyProduce, "producer-1", "orders"},
		{ApiKeyFetch, "consumer-2", "orders"},
	}

	// 응답 수집 goroutine
	var responseMu sync.Mutex
	responseMap := make(map[int32]*Response) // correlationID → Response

	var responseWg sync.WaitGroup
	responseStop := make(chan struct{})
	for i := 0; i < numProcessors; i++ {
		responseWg.Add(1)
		procID := i
		go func() {
			defer responseWg.Done()
			for {
				select {
				case <-responseStop:
					return
				case resp := <-processors[procID].responseCh:
					responseMu.Lock()
					responseMap[resp.CorrelationID] = resp
					responseMu.Unlock()
				}
			}
		}()
	}

	// 요청 전송
	var sendWg sync.WaitGroup
	for i, simReq := range requests {
		sendWg.Add(1)
		correlationID := int32(i + 1)
		processorID := i % numProcessors
		apiKey := simReq.apiKey
		clientID := simReq.clientID
		topic := simReq.topic
		connID := fmt.Sprintf("conn-%s-%d", clientID, i)

		go func() {
			defer sendWg.Done()
			time.Sleep(time.Duration(rand.Intn(20)) * time.Millisecond)

			req := &Request{
				Header: RequestHeader{
					ApiKey:        apiKey,
					ApiVersion:    1,
					CorrelationID: correlationID,
					ClientID:      clientID,
				},
				Payload:      []byte(topic),
				ProcessorID:  processorID,
				ConnectionID: connID,
			}

			fmt.Printf("  [Processor-%d] 요청 큐잉: apiKey=%s, correlationID=%d, client=%s\n",
				processorID, apiKeyName(apiKey), correlationID, clientID)

			requestChannel.SendRequest(req)
		}()
	}

	sendWg.Wait()
	time.Sleep(500 * time.Millisecond) // Handler가 모든 요청을 처리할 시간

	close(responseStop)
	responseWg.Wait()
	fmt.Println()

	// --- 3. CorrelationID 매칭 검증 ---
	fmt.Println("--- 3단계: CorrelationID 매칭 검증 ---")
	responseMu.Lock()
	matchCount := 0
	for i, simReq := range requests {
		correlationID := int32(i + 1)
		resp, found := responseMap[correlationID]
		status := "MISSING"
		respInfo := ""
		if found {
			if resp.CorrelationID == correlationID {
				status = "MATCHED"
				matchCount++
			} else {
				status = "MISMATCH"
			}
			respInfo = string(resp.Payload)
		}
		fmt.Printf("  correlationID=%d, apiKey=%s, client=%s → %s %s\n",
			correlationID, apiKeyName(simReq.apiKey), simReq.clientID, status, respInfo)
	}
	responseMu.Unlock()
	fmt.Printf("\n  매칭 결과: %d/%d 성공\n", matchCount, len(requests))
	fmt.Println()

	// --- 4. 통계 ---
	fmt.Println("--- 4단계: 처리 통계 ---")
	fmt.Printf("  RequestChannel: 총 요청=%d, 총 응답=%d\n",
		requestChannel.stats.totalRequests.Load(), requestChannel.stats.totalResponses.Load())
	fmt.Printf("  KafkaApis: PRODUCE=%d, FETCH=%d, METADATA=%d\n",
		kafkaApis.stats.produceCount.Load(), kafkaApis.stats.fetchCount.Load(),
		kafkaApis.stats.metadataCount.Load())
	for _, h := range handlers {
		fmt.Printf("  Handler-%d: 처리한 요청=%d\n", h.id, h.processedCount.Load())
	}
	fmt.Println()

	// --- 5. 정리 ---
	for _, h := range handlers {
		h.Stop()
	}

	// --- 아키텍처 요약 ---
	fmt.Println("=== 아키텍처 요약 ===")
	fmt.Println()
	fmt.Println("KafkaApis.handle() API 라우팅 (KafkaApis.scala:150):")
	fmt.Println("  request.header.apiKey match {")
	fmt.Println("    case ApiKeys.PRODUCE  => handleProduceRequest()")
	fmt.Println("    case ApiKeys.FETCH    => handleFetchRequest()")
	fmt.Println("    case ApiKeys.METADATA => handleTopicMetadataRequest()")
	fmt.Println("    case ...              => 40+ 다른 API 핸들러")
	fmt.Println("  }")
	fmt.Println()
	fmt.Println("KafkaRequestHandler.run() 루프 (KafkaRequestHandler.scala:107):")
	fmt.Println("  while (!stopped) {")
	fmt.Println("    req = requestChannel.receiveRequest(300)  // 300ms 타임아웃")
	fmt.Println("    apis.handle(req, requestLocal)")
	fmt.Println("  }")
	fmt.Println()
	fmt.Println("KafkaRequestHandlerPool (KafkaRequestHandler.scala:223):")
	fmt.Println("  num.io.threads 설정으로 Handler 수를 결정한다.")
	fmt.Println("  모든 Handler가 하나의 RequestChannel 큐에서 요청을 경쟁적으로 가져간다.")
	fmt.Println("  resizeThreadPool()로 런타임에 Handler 수를 동적으로 변경 가능하다.")
	fmt.Println()
	fmt.Println("CorrelationID 매칭:")
	fmt.Println("  클라이언트가 요청에 correlationID를 설정하고,")
	fmt.Println("  서버는 응답에 동일한 correlationID를 포함한다.")
	fmt.Println("  이를 통해 파이프라이닝된 비동기 요청-응답을 매칭한다.")
}
