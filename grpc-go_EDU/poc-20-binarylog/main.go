package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// gRPC Binary Logging 시뮬레이션
// =============================================================================
//
// gRPC Binary Logging은 gRPC 메시지를 바이너리 형식으로 기록한다.
// 디버깅, 감사, 재생에 활용된다. 환경변수 GRPC_BINARY_LOG_FILTER로 제어.
//
// 핵심 개념:
//   - BinaryLog Entry: 헤더/메시지/트레일러를 바이너리 직렬화
//   - Filter: 메서드 패턴 매칭 (*, service/*, service/method)
//   - Sink: 로그를 파일/네트워크로 출력
//   - Truncation: 큰 메시지는 설정된 크기로 잘라서 기록
//
// 실제 코드 참조:
//   - binarylog/binarylog.go: 바이너리 로그 구현
//   - binarylog/sink.go: 로그 출력
// =============================================================================

// --- Entry Types ---

type EntryType int

const (
	EntryClientHeader  EntryType = iota
	EntryServerHeader
	EntryClientMessage
	EntryServerMessage
	EntryClientHalfClose
	EntryServerTrailer
	EntryCancel
)

func (t EntryType) String() string {
	names := []string{
		"CLIENT_HEADER", "SERVER_HEADER",
		"CLIENT_MESSAGE", "SERVER_MESSAGE",
		"CLIENT_HALF_CLOSE", "SERVER_TRAILER", "CANCEL",
	}
	if int(t) < len(names) {
		return names[t]
	}
	return "UNKNOWN"
}

// --- Binary Log Entry ---

type Metadata struct {
	Key   string
	Value string
}

type BinaryLogEntry struct {
	Timestamp    time.Time
	CallID       uint64
	SequenceID   uint32
	Type         EntryType
	MethodName   string
	Metadata     []Metadata
	Message      []byte
	StatusCode   int
	StatusMsg    string
	PeerAddress  string
	PayloadTrunc bool
}

func (e BinaryLogEntry) Serialize() []byte {
	// 간단한 바이너리 직렬화 (실제로는 protobuf 사용)
	buf := make([]byte, 0, 256)

	// Header: type(1) + callID(8) + seqID(4) + timestamp(8)
	buf = append(buf, byte(e.Type))
	callIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(callIDBytes, e.CallID)
	buf = append(buf, callIDBytes...)
	seqBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(seqBytes, e.SequenceID)
	buf = append(buf, seqBytes...)
	tsBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(tsBytes, uint64(e.Timestamp.UnixNano()))
	buf = append(buf, tsBytes...)

	// Method name (length-prefixed)
	methodBytes := []byte(e.MethodName)
	lenBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBytes, uint16(len(methodBytes)))
	buf = append(buf, lenBytes...)
	buf = append(buf, methodBytes...)

	// Message payload (length-prefixed)
	payloadLen := make([]byte, 4)
	binary.BigEndian.PutUint32(payloadLen, uint32(len(e.Message)))
	buf = append(buf, payloadLen...)
	buf = append(buf, e.Message...)

	return buf
}

func (e BinaryLogEntry) String() string {
	trunc := ""
	if e.PayloadTrunc {
		trunc = " [TRUNCATED]"
	}
	msgPreview := ""
	if len(e.Message) > 0 {
		preview := hex.EncodeToString(e.Message)
		if len(preview) > 40 {
			preview = preview[:40] + "..."
		}
		msgPreview = fmt.Sprintf(" payload=%s%s", preview, trunc)
	}
	meta := ""
	if len(e.Metadata) > 0 {
		parts := make([]string, len(e.Metadata))
		for i, m := range e.Metadata {
			parts[i] = m.Key + "=" + m.Value
		}
		meta = " meta=[" + strings.Join(parts, ", ") + "]"
	}
	status := ""
	if e.Type == EntryServerTrailer {
		status = fmt.Sprintf(" status=%d(%s)", e.StatusCode, e.StatusMsg)
	}
	return fmt.Sprintf("[call=%d seq=%d] %s %s%s%s%s",
		e.CallID, e.SequenceID, e.Type, e.MethodName, meta, msgPreview, status)
}

// --- Filter ---

// LogFilter는 어떤 메서드를 로깅할지 결정한다.
// GRPC_BINARY_LOG_FILTER 환경변수 형식: {pattern}{hdr:len}{msg:len}
type LogFilter struct {
	patterns       []FilterPattern
	maxHeaderBytes int
	maxMsgBytes    int
}

type FilterPattern struct {
	Service string
	Method  string // "*" = all
}

func NewLogFilter(maxHeader, maxMsg int) *LogFilter {
	return &LogFilter{
		maxHeaderBytes: maxHeader,
		maxMsgBytes:    maxMsg,
	}
}

func (f *LogFilter) AddPattern(service, method string) {
	f.patterns = append(f.patterns, FilterPattern{Service: service, Method: method})
}

func (f *LogFilter) ShouldLog(methodName string) bool {
	if len(f.patterns) == 0 {
		return true // no filter = log everything
	}
	parts := strings.Split(strings.TrimPrefix(methodName, "/"), "/")
	if len(parts) != 2 {
		return false
	}
	service, method := parts[0], parts[1]
	for _, p := range f.patterns {
		if p.Service == "*" || p.Service == service {
			if p.Method == "*" || p.Method == method {
				return true
			}
		}
	}
	return false
}

func (f *LogFilter) TruncateMessage(msg []byte) ([]byte, bool) {
	if f.maxMsgBytes > 0 && len(msg) > f.maxMsgBytes {
		return msg[:f.maxMsgBytes], true
	}
	return msg, false
}

// --- Sink (로그 출력) ---

type LogSink interface {
	Write(entry BinaryLogEntry) error
	Close() error
}

type MemorySink struct {
	mu      sync.Mutex
	entries []BinaryLogEntry
}

func (s *MemorySink) Write(entry BinaryLogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *MemorySink) Close() error { return nil }

func (s *MemorySink) Entries() []BinaryLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]BinaryLogEntry{}, s.entries...)
}

// --- Binary Logger ---

type BinaryLogger struct {
	filter *LogFilter
	sink   LogSink
	nextID uint64
	mu     sync.Mutex
}

func NewBinaryLogger(filter *LogFilter, sink LogSink) *BinaryLogger {
	return &BinaryLogger{filter: filter, sink: sink}
}

func (bl *BinaryLogger) newCallID() uint64 {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	bl.nextID++
	return bl.nextID
}

// LogClientHeader는 클라이언트 헤더를 기록한다.
func (bl *BinaryLogger) LogClientHeader(callID uint64, method string, metadata []Metadata, peer string) {
	if !bl.filter.ShouldLog(method) {
		return
	}
	bl.sink.Write(BinaryLogEntry{
		Timestamp:   time.Now(),
		CallID:      callID,
		SequenceID:  1,
		Type:        EntryClientHeader,
		MethodName:  method,
		Metadata:    metadata,
		PeerAddress: peer,
	})
}

func (bl *BinaryLogger) LogServerHeader(callID uint64, method string, metadata []Metadata) {
	if !bl.filter.ShouldLog(method) {
		return
	}
	bl.sink.Write(BinaryLogEntry{
		Timestamp:  time.Now(),
		CallID:     callID,
		SequenceID: 2,
		Type:       EntryServerHeader,
		MethodName: method,
		Metadata:   metadata,
	})
}

func (bl *BinaryLogger) LogMessage(callID uint64, method string, seqID uint32, isClient bool, msg []byte) {
	if !bl.filter.ShouldLog(method) {
		return
	}
	entryType := EntryServerMessage
	if isClient {
		entryType = EntryClientMessage
	}
	truncMsg, truncated := bl.filter.TruncateMessage(msg)
	bl.sink.Write(BinaryLogEntry{
		Timestamp:    time.Now(),
		CallID:       callID,
		SequenceID:   seqID,
		Type:         entryType,
		MethodName:   method,
		Message:      truncMsg,
		PayloadTrunc: truncated,
	})
}

func (bl *BinaryLogger) LogServerTrailer(callID uint64, method string, seqID uint32, code int, msg string, metadata []Metadata) {
	if !bl.filter.ShouldLog(method) {
		return
	}
	bl.sink.Write(BinaryLogEntry{
		Timestamp:  time.Now(),
		CallID:     callID,
		SequenceID: seqID,
		Type:       EntryServerTrailer,
		MethodName: method,
		StatusCode: code,
		StatusMsg:  msg,
		Metadata:   metadata,
	})
}

func main() {
	fmt.Println("=== gRPC Binary Logging 시뮬레이션 ===")
	fmt.Println()

	// --- 필터 설정 ---
	fmt.Println("[1] 로그 필터 설정")
	fmt.Println(strings.Repeat("-", 60))

	filter := NewLogFilter(0, 64) // 메시지 최대 64 바이트
	filter.AddPattern("helloworld.Greeter", "*")
	filter.AddPattern("routeguide.RouteGuide", "GetFeature")

	fmt.Println("  Pattern: helloworld.Greeter/* (모든 메서드)")
	fmt.Println("  Pattern: routeguide.RouteGuide/GetFeature (특정 메서드)")
	fmt.Printf("  Max message bytes: %d\n", filter.maxMsgBytes)
	fmt.Println()

	// --- 필터 테스트 ---
	fmt.Println("[2] 필터 매칭 테스트")
	fmt.Println(strings.Repeat("-", 60))

	testMethods := []string{
		"/helloworld.Greeter/SayHello",
		"/helloworld.Greeter/SayHelloStream",
		"/routeguide.RouteGuide/GetFeature",
		"/routeguide.RouteGuide/ListFeatures",
		"/grpc.health.v1.Health/Check",
	}
	for _, m := range testMethods {
		fmt.Printf("  %-50s -> log=%v\n", m, filter.ShouldLog(m))
	}
	fmt.Println()

	// --- RPC 시뮬레이션 ---
	fmt.Println("[3] RPC 호출 바이너리 로깅")
	fmt.Println(strings.Repeat("-", 60))

	sink := &MemorySink{}
	logger := NewBinaryLogger(filter, sink)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Unary RPC: SayHello
	callID1 := logger.newCallID()
	method1 := "/helloworld.Greeter/SayHello"
	logger.LogClientHeader(callID1, method1, []Metadata{
		{"content-type", "application/grpc"},
		{":authority", "localhost:50051"},
		{"user-agent", "grpc-go/1.60.0"},
	}, "127.0.0.1:54321")

	msg1 := make([]byte, 20)
	r.Read(msg1)
	logger.LogMessage(callID1, method1, 3, true, msg1)

	logger.LogServerHeader(callID1, method1, []Metadata{
		{"content-type", "application/grpc"},
	})

	resp1 := make([]byte, 30)
	r.Read(resp1)
	logger.LogMessage(callID1, method1, 5, false, resp1)

	logger.LogServerTrailer(callID1, method1, 6, 0, "OK", []Metadata{
		{"grpc-status", "0"},
	})

	// Unary RPC: GetFeature
	callID2 := logger.newCallID()
	method2 := "/routeguide.RouteGuide/GetFeature"
	logger.LogClientHeader(callID2, method2, []Metadata{
		{"content-type", "application/grpc"},
	}, "127.0.0.1:54322")

	msg2 := make([]byte, 8)
	r.Read(msg2)
	logger.LogMessage(callID2, method2, 3, true, msg2)

	logger.LogServerHeader(callID2, method2, nil)

	resp2 := make([]byte, 100) // 100 bytes > 64 limit = truncated
	r.Read(resp2)
	logger.LogMessage(callID2, method2, 5, false, resp2)

	logger.LogServerTrailer(callID2, method2, 6, 0, "OK", nil)

	// 필터에 걸리지 않는 RPC
	callID3 := logger.newCallID()
	method3 := "/grpc.health.v1.Health/Check"
	logger.LogClientHeader(callID3, method3, nil, "127.0.0.1:54323")
	logger.LogMessage(callID3, method3, 3, true, []byte{0x01})

	fmt.Println("  3개 RPC 호출 완료")
	fmt.Println()

	// --- 로그 엔트리 출력 ---
	fmt.Println("[4] 기록된 바이너리 로그 엔트리")
	fmt.Println(strings.Repeat("-", 60))

	entries := sink.Entries()
	fmt.Printf("  총 엔트리 수: %d (health 체크는 필터로 제외됨)\n\n", len(entries))
	for _, entry := range entries {
		fmt.Printf("  %s\n", entry)
	}
	fmt.Println()

	// --- 바이너리 직렬화 ---
	fmt.Println("[5] 바이너리 직렬화 (hex dump)")
	fmt.Println(strings.Repeat("-", 60))

	for i, entry := range entries[:3] { // 처음 3개만
		serialized := entry.Serialize()
		fmt.Printf("  Entry %d (%d bytes):\n", i+1, len(serialized))
		// hex dump
		for j := 0; j < len(serialized); j += 16 {
			end := j + 16
			if end > len(serialized) {
				end = len(serialized)
			}
			hexStr := hex.EncodeToString(serialized[j:end])
			// 2바이트씩 공백
			spaced := ""
			for k := 0; k < len(hexStr); k += 2 {
				if k > 0 {
					spaced += " "
				}
				end := k + 2
				if end > len(hexStr) {
					end = len(hexStr)
				}
				spaced += hexStr[k:end]
			}
			fmt.Printf("    %04x: %s\n", j, spaced)
		}
		fmt.Println()
	}

	// --- 통계 ---
	fmt.Println("[6] 로깅 통계")
	fmt.Println(strings.Repeat("-", 60))
	typeCount := make(map[EntryType]int)
	totalBytes := 0
	truncCount := 0
	for _, e := range entries {
		typeCount[e.Type]++
		totalBytes += len(e.Serialize())
		if e.PayloadTrunc {
			truncCount++
		}
	}
	for t, c := range typeCount {
		fmt.Printf("  %-20s: %d\n", t, c)
	}
	fmt.Printf("  Total entries:       %d\n", len(entries))
	fmt.Printf("  Total bytes:         %d\n", totalBytes)
	fmt.Printf("  Truncated messages:  %d\n", truncCount)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
