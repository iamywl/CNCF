// poc-03-transport: HTTP/2 프레임 송수신 시뮬레이션
//
// gRPC 전송 계층의 핵심인 HTTP/2 프레임 처리를 시뮬레이션한다.
// - Frame 타입: HEADERS, DATA, SETTINGS, PING, GOAWAY, WINDOW_UPDATE
// - 프레임 인코딩/디코딩 (9바이트 헤더 + 페이로드)
// - controlBuffer: 프레임 큐잉 메커니즘
// - loopyWriter: 비동기 프레임 발신 루프
//
// 실제 gRPC 참조:
//   internal/transport/controlbuf.go → controlBuffer, loopyWriter, itemList
//   internal/transport/http2_server.go → 프레임 수신/처리
//   internal/transport/http2_client.go → 프레임 송신

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
)

// ──────────────────────────────────────────────
// 1. HTTP/2 프레임 타입 정의
// ──────────────────────────────────────────────

// FrameType은 HTTP/2 프레임 타입 (RFC 7540 Section 6)
type FrameType uint8

const (
	FrameData         FrameType = 0x0 // DATA: RPC 메시지 본문
	FrameHeaders      FrameType = 0x1 // HEADERS: RPC 메서드, 메타데이터
	FrameSettings     FrameType = 0x4 // SETTINGS: 연결 설정
	FramePing         FrameType = 0x6 // PING: 연결 상태 확인
	FrameGoAway       FrameType = 0x7 // GOAWAY: 연결 종료 시그널
	FrameWindowUpdate FrameType = 0x8 // WINDOW_UPDATE: 흐름 제어
)

func (ft FrameType) String() string {
	switch ft {
	case FrameData:
		return "DATA"
	case FrameHeaders:
		return "HEADERS"
	case FrameSettings:
		return "SETTINGS"
	case FramePing:
		return "PING"
	case FrameGoAway:
		return "GOAWAY"
	case FrameWindowUpdate:
		return "WINDOW_UPDATE"
	default:
		return fmt.Sprintf("UNKNOWN(0x%x)", uint8(ft))
	}
}

// ──────────────────────────────────────────────
// 2. Frame 구조체 및 인코딩/디코딩
// ──────────────────────────────────────────────

// Frame은 HTTP/2 프레임 구조.
// HTTP/2 프레임 형식 (9바이트 헤더):
//   Length(3) + Type(1) + Flags(1) + StreamID(4) + Payload(Length)
type Frame struct {
	Type     FrameType
	Flags    uint8
	StreamID uint32
	Payload  []byte
}

// Encode는 프레임을 바이트로 직렬화한다.
// HTTP/2 프레임 헤더: 3바이트 길이 + 1바이트 타입 + 1바이트 플래그 + 4바이트 스트림ID
func (f *Frame) Encode() []byte {
	length := len(f.Payload)
	buf := make([]byte, 9+length)

	// 3바이트 길이 (big-endian)
	buf[0] = byte(length >> 16)
	buf[1] = byte(length >> 8)
	buf[2] = byte(length)

	// 1바이트 타입
	buf[3] = byte(f.Type)

	// 1바이트 플래그
	buf[4] = f.Flags

	// 4바이트 스트림 ID (최상위 비트는 reserved)
	binary.BigEndian.PutUint32(buf[5:9], f.StreamID&0x7FFFFFFF)

	// 페이로드
	copy(buf[9:], f.Payload)
	return buf
}

// DecodeFrame은 바이트에서 프레임을 역직렬화한다.
func DecodeFrame(r io.Reader) (*Frame, error) {
	header := make([]byte, 9)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}

	// 3바이트 길이
	length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])

	frame := &Frame{
		Type:     FrameType(header[3]),
		Flags:    header[4],
		StreamID: binary.BigEndian.Uint32(header[5:9]) & 0x7FFFFFFF,
	}

	// 페이로드 읽기
	if length > 0 {
		frame.Payload = make([]byte, length)
		if _, err := io.ReadFull(r, frame.Payload); err != nil {
			return nil, err
		}
	}

	return frame, nil
}

// ──────────────────────────────────────────────
// 3. controlBuffer: 프레임 큐잉 (controlbuf.go 참조)
// ──────────────────────────────────────────────

// controlItem은 controlBuffer에 큐잉되는 항목.
// 실제: controlbuf.go의 cbItem 인터페이스
type controlItem struct {
	frame *Frame
	next  *controlItem
}

// controlBuffer는 프레임을 비동기적으로 큐잉하는 버퍼.
// 실제: controlbuf.go:307 — wakeupCh, mu, consumerWaiting, list 등
// 핵심 역할: 여러 고루틴이 프레임을 put하고, loopyWriter가 get하여 전송
type controlBuffer struct {
	mu              sync.Mutex
	consumerWaiting bool         // loopyWriter가 대기 중인지
	wakeupCh        chan struct{} // 대기 중인 consumer 깨우기
	head            *controlItem
	tail            *controlItem
	closed          bool
	done            chan struct{}
}

func newControlBuffer() *controlBuffer {
	return &controlBuffer{
		wakeupCh: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// put은 프레임을 버퍼에 추가한다.
// 실제: controlbuf.go의 put 메서드
func (cb *controlBuffer) put(f *Frame) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.closed {
		return false
	}

	item := &controlItem{frame: f}
	if cb.tail == nil {
		cb.head = item
	} else {
		cb.tail.next = item
	}
	cb.tail = item

	// consumer가 대기 중이면 깨우기
	if cb.consumerWaiting {
		cb.consumerWaiting = false
		select {
		case cb.wakeupCh <- struct{}{}:
		default:
		}
	}
	return true
}

// get은 버퍼에서 프레임을 꺼낸다. 비어있으면 대기한다.
// 실제: controlbuf.go의 get 메서드
func (cb *controlBuffer) get() (*Frame, bool) {
	cb.mu.Lock()
	if cb.head != nil {
		item := cb.head
		cb.head = item.next
		if cb.head == nil {
			cb.tail = nil
		}
		cb.mu.Unlock()
		return item.frame, true
	}
	// 비어있으면 대기 등록
	cb.consumerWaiting = true
	cb.mu.Unlock()

	select {
	case <-cb.wakeupCh:
		return cb.get() // 재시도
	case <-cb.done:
		return nil, false
	}
}

// close는 버퍼를 닫는다.
func (cb *controlBuffer) close() {
	cb.mu.Lock()
	cb.closed = true
	cb.mu.Unlock()
	close(cb.done)
}

// ──────────────────────────────────────────────
// 4. loopyWriter: 비동기 프레임 발신 (controlbuf.go 참조)
// ──────────────────────────────────────────────

// loopyWriter는 controlBuffer에서 프레임을 꺼내어 전송하는 루프.
// 실제: controlbuf.go:542 — newLoopyWriter → run()
// 핵심: controlBuffer.get() → 프레임 타입별 처리 → framer를 통해 전송
type loopyWriter struct {
	cbuf   *controlBuffer
	writer io.Writer
	side   string // "서버" or "클라이언트"
	sent   []*Frame
	mu     sync.Mutex
}

func newLoopyWriter(side string, w io.Writer, cbuf *controlBuffer) *loopyWriter {
	return &loopyWriter{
		cbuf:   cbuf,
		writer: w,
		side:   side,
	}
}

// run은 loopyWriter의 메인 루프.
// 실제: controlbuf.go:586 — for { get() → handle() → flush() }
func (lw *loopyWriter) run() {
	for {
		frame, ok := lw.cbuf.get()
		if !ok {
			return // 버퍼가 닫힘
		}

		// 프레임 인코딩 및 전송
		encoded := frame.Encode()
		if _, err := lw.writer.Write(encoded); err != nil {
			fmt.Printf("[%s/loopy] 전송 에러: %v\n", lw.side, err)
			return
		}

		lw.mu.Lock()
		lw.sent = append(lw.sent, frame)
		lw.mu.Unlock()

		fmt.Printf("[%s/loopy] 전송: %s (stream=%d, payload=%d bytes)\n",
			lw.side, frame.Type, frame.StreamID, len(frame.Payload))
	}
}

// getSent는 전송된 프레임 목록을 반환한다.
func (lw *loopyWriter) getSent() []*Frame {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	result := make([]*Frame, len(lw.sent))
	copy(result, lw.sent)
	return result
}

// ──────────────────────────────────────────────
// 5. main: 프레임 교환 시뮬레이션
// ──────────────────────────────────────────────

func main() {
	fmt.Println("=== HTTP/2 프레임 송수신 시뮬레이션 ===")
	fmt.Println()

	// --- Part 1: 프레임 인코딩/디코딩 테스트 ---
	fmt.Println("── 1. 프레임 인코딩/디코딩 ──")

	testFrames := []*Frame{
		{Type: FrameSettings, StreamID: 0, Payload: []byte{0, 3, 0, 0, 0, 100}},
		{Type: FrameHeaders, Flags: 0x4, StreamID: 1, Payload: []byte(":path=/service/Method")},
		{Type: FrameData, Flags: 0x1, StreamID: 1, Payload: []byte("Hello, gRPC!")},
		{Type: FramePing, StreamID: 0, Payload: []byte{0, 0, 0, 0, 0, 0, 0, 1}},
		{Type: FrameWindowUpdate, StreamID: 0, Payload: []byte{0, 0, 0xFF, 0xFF}},
		{Type: FrameGoAway, StreamID: 0, Payload: []byte{0, 0, 0, 1, 0, 0, 0, 0}},
	}

	var buf bytes.Buffer
	for _, f := range testFrames {
		encoded := f.Encode()
		buf.Write(encoded)
		fmt.Printf("  인코딩: %-15s stream=%-3d flags=0x%02x payload=%d bytes → 총 %d bytes\n",
			f.Type, f.StreamID, f.Flags, len(f.Payload), len(encoded))
	}

	fmt.Println()
	fmt.Println("  디코딩:")
	reader := bytes.NewReader(buf.Bytes())
	for {
		frame, err := DecodeFrame(reader)
		if err != nil {
			break
		}
		fmt.Printf("  디코딩: %-15s stream=%-3d flags=0x%02x payload=%d bytes\n",
			frame.Type, frame.StreamID, frame.Flags, len(frame.Payload))
	}

	// --- Part 2: controlBuffer + loopyWriter ---
	fmt.Println()
	fmt.Println("── 2. controlBuffer + loopyWriter 패턴 ──")
	fmt.Println()

	var wireBuf bytes.Buffer
	cbuf := newControlBuffer()
	lw := newLoopyWriter("클라이언트", &wireBuf, cbuf)

	// loopyWriter를 고루틴에서 실행
	go lw.run()

	// HTTP/2 연결 시작 시퀀스 시뮬레이션
	// 1) 클라이언트 → 서버: SETTINGS
	cbuf.put(&Frame{
		Type:     FrameSettings,
		StreamID: 0,
		Payload:  []byte{0, 1, 0, 0, 0x10, 0x00}, // HEADER_TABLE_SIZE=4096
	})

	// 2) 클라이언트 → 서버: HEADERS (RPC 요청 시작)
	cbuf.put(&Frame{
		Type:     FrameHeaders,
		Flags:    0x4, // END_HEADERS
		StreamID: 1,
		Payload:  []byte(":method=POST,:path=/grpc.health.v1.Health/Check"),
	})

	// 3) 클라이언트 → 서버: DATA (요청 본문)
	cbuf.put(&Frame{
		Type:     FrameData,
		Flags:    0x1, // END_STREAM
		StreamID: 1,
		Payload:  []byte{0, 0, 0, 0, 2, 0x0A, 0x00}, // gRPC 5바이트 헤더 + protobuf
	})

	// 4) PING (keepalive)
	cbuf.put(&Frame{
		Type:     FramePing,
		StreamID: 0,
		Payload:  []byte{0, 0, 0, 0, 0, 0, 0, 42},
	})

	// 5) WINDOW_UPDATE
	cbuf.put(&Frame{
		Type:     FrameWindowUpdate,
		StreamID: 0,
		Payload:  []byte{0, 0, 0xFF, 0xFF}, // 65535 증가
	})

	time.Sleep(100 * time.Millisecond)
	cbuf.close()
	time.Sleep(50 * time.Millisecond)

	// --- Part 3: 와이어 데이터 확인 ---
	fmt.Println()
	fmt.Println("── 3. 와이어 데이터 디코딩 ──")
	fmt.Println()

	wireReader := bytes.NewReader(wireBuf.Bytes())
	count := 0
	for {
		frame, err := DecodeFrame(wireReader)
		if err != nil {
			break
		}
		count++
		fmt.Printf("  프레임 #%d: %-15s stream=%-3d payload=%d bytes\n",
			count, frame.Type, frame.StreamID, len(frame.Payload))
	}

	fmt.Printf("\n  총 %d 프레임, %d 바이트 전송\n", count, wireBuf.Len())

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
