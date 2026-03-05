// poc-08-encoding: 코덱 레지스트리 & 압축
//
// gRPC의 인코딩/압축 시스템을 시뮬레이션한다.
// - Codec 인터페이스: Marshal/Unmarshal/Name
// - Compressor 인터페이스: Compress/Decompress/Name
// - 레지스트리: RegisterCodec, GetCodec, RegisterCompressor, GetCompressor
// - JSON 코덱 구현
// - gzip 압축기 구현
// - 5바이트 메시지 프레이밍 (1바이트 플래그 + 4바이트 길이)
//
// 실제 gRPC 참조:
//   encoding/encoding.go          → Codec, Compressor, RegisterCodec, GetCodec
//   encoding/proto/proto.go       → protobuf 코덱
//   encoding/gzip/gzip.go         → gzip 압축기
//   internal/transport/handler_server.go → 메시지 프레이밍

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ──────────────────────────────────────────────
// 1. Codec 인터페이스 (encoding/encoding.go 참조)
// ──────────────────────────────────────────────

// Codec은 메시지 직렬화/역직렬화를 담당한다.
// 실제: encoding/encoding.go:102
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
	Name() string
}

// ──────────────────────────────────────────────
// 2. Compressor 인터페이스 (encoding/encoding.go 참조)
// ──────────────────────────────────────────────

// Compressor는 메시지 압축/해제를 담당한다.
// 실제: encoding/encoding.go:61
type Compressor interface {
	Compress(w io.Writer) (io.WriteCloser, error)
	Decompress(r io.Reader) (io.Reader, error)
	Name() string
}

// ──────────────────────────────────────────────
// 3. 레지스트리 (encoding/encoding.go 참조)
// ──────────────────────────────────────────────

// 실제: encoding/encoding.go의 전역 맵 + Register/Get 함수
var (
	codecMu     sync.RWMutex
	codecReg    = make(map[string]Codec)
	compMu      sync.RWMutex
	compReg     = make(map[string]Compressor)
)

// RegisterCodec은 코덱을 이름으로 등록한다.
// 실제: encoding/encoding.go의 RegisterCodec
func RegisterCodec(c Codec) {
	codecMu.Lock()
	defer codecMu.Unlock()
	codecReg[c.Name()] = c
	fmt.Printf("[레지스트리] 코덱 등록: %s\n", c.Name())
}

// GetCodec은 이름으로 코덱을 조회한다.
// 실제: encoding/encoding.go의 GetCodec
func GetCodec(name string) Codec {
	codecMu.RLock()
	defer codecMu.RUnlock()
	return codecReg[name]
}

// RegisterCompressor는 압축기를 이름으로 등록한다.
// 실제: encoding/encoding.go의 RegisterCompressor
func RegisterCompressor(c Compressor) {
	compMu.Lock()
	defer compMu.Unlock()
	compReg[c.Name()] = c
	fmt.Printf("[레지스트리] 압축기 등록: %s\n", c.Name())
}

// GetCompressor는 이름으로 압축기를 조회한다.
// 실제: encoding/encoding.go의 GetCompressor
func GetCompressor(name string) Compressor {
	compMu.RLock()
	defer compMu.RUnlock()
	return compReg[name]
}

// ──────────────────────────────────────────────
// 4. JSON 코덱 구현
// ──────────────────────────────────────────────

// JSONCodec은 JSON 기반 코덱.
// gRPC는 기본적으로 protobuf을 사용하지만, 커스텀 코덱으로 JSON도 가능하다.
type JSONCodec struct{}

func (c *JSONCodec) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (c *JSONCodec) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func (c *JSONCodec) Name() string {
	return "json"
}

// ──────────────────────────────────────────────
// 5. 간단한 바이너리 코덱 (protobuf 시뮬레이션)
// ──────────────────────────────────────────────

// SimpleMessage는 간단한 메시지 구조체.
type SimpleMessage struct {
	ID      uint32
	Content string
}

// SimpleBinaryCodec은 간단한 바이너리 직렬화 코덱 (protobuf 개념 시뮬레이션).
// 형식: [4바이트 ID] [2바이트 문자열 길이] [문자열 데이터]
type SimpleBinaryCodec struct{}

func (c *SimpleBinaryCodec) Marshal(v any) ([]byte, error) {
	msg, ok := v.(*SimpleMessage)
	if !ok {
		return nil, fmt.Errorf("SimpleBinaryCodec: SimpleMessage 타입만 지원")
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, msg.ID)
	binary.Write(&buf, binary.BigEndian, uint16(len(msg.Content)))
	buf.WriteString(msg.Content)
	return buf.Bytes(), nil
}

func (c *SimpleBinaryCodec) Unmarshal(data []byte, v any) error {
	msg, ok := v.(*SimpleMessage)
	if !ok {
		return fmt.Errorf("SimpleBinaryCodec: SimpleMessage 타입만 지원")
	}
	r := bytes.NewReader(data)
	if err := binary.Read(r, binary.BigEndian, &msg.ID); err != nil {
		return err
	}
	var strLen uint16
	if err := binary.Read(r, binary.BigEndian, &strLen); err != nil {
		return err
	}
	strBuf := make([]byte, strLen)
	if _, err := io.ReadFull(r, strBuf); err != nil {
		return err
	}
	msg.Content = string(strBuf)
	return nil
}

func (c *SimpleBinaryCodec) Name() string {
	return "proto-sim"
}

// ──────────────────────────────────────────────
// 6. Gzip 압축기 구현 (encoding/gzip/gzip.go 참조)
// ──────────────────────────────────────────────

// GzipCompressor는 gzip 기반 압축기.
// 실제: encoding/gzip/gzip.go
type GzipCompressor struct{}

func (c *GzipCompressor) Compress(w io.Writer) (io.WriteCloser, error) {
	return gzip.NewWriterLevel(w, gzip.BestSpeed)
}

func (c *GzipCompressor) Decompress(r io.Reader) (io.Reader, error) {
	return gzip.NewReader(r)
}

func (c *GzipCompressor) Name() string {
	return "gzip"
}

// IdentityCompressor는 압축하지 않는 (identity) 압축기.
type IdentityCompressor struct{}

func (c *IdentityCompressor) Compress(w io.Writer) (io.WriteCloser, error) {
	return &nopCloserWriter{w}, nil
}

func (c *IdentityCompressor) Decompress(r io.Reader) (io.Reader, error) {
	return r, nil
}

func (c *IdentityCompressor) Name() string {
	return "identity"
}

type nopCloserWriter struct{ io.Writer }

func (w *nopCloserWriter) Close() error { return nil }

// ──────────────────────────────────────────────
// 7. 메시지 프레이밍 (5바이트 헤더)
// ──────────────────────────────────────────────

// gRPC 메시지 프레이밍 형식:
//   [1바이트 플래그] [4바이트 메시지 길이 (big-endian)] [메시지 데이터]
//
// 플래그:
//   0x00 = 비압축
//   0x01 = 압축됨
//
// 실제: internal/transport/handler_server.go의 msgHeader 처리

// EncodeFrame은 gRPC 메시지 프레임을 생성한다.
func EncodeFrame(data []byte, compressed bool) []byte {
	frame := make([]byte, 5+len(data))
	if compressed {
		frame[0] = 1 // 압축 플래그
	} else {
		frame[0] = 0 // 비압축 플래그
	}
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	return frame
}

// DecodeFrame은 gRPC 메시지 프레임을 파싱한다.
func DecodeFrame(frame []byte) (compressed bool, data []byte, err error) {
	if len(frame) < 5 {
		return false, nil, fmt.Errorf("프레임이 너무 짧음: %d bytes", len(frame))
	}
	compressed = frame[0] == 1
	length := binary.BigEndian.Uint32(frame[1:5])
	if int(length) > len(frame)-5 {
		return false, nil, fmt.Errorf("데이터 길이 불일치: header=%d, actual=%d", length, len(frame)-5)
	}
	return compressed, frame[5 : 5+length], nil
}

// ──────────────────────────────────────────────
// 8. 엔드투엔드 파이프라인: 직렬화 → 압축 → 프레이밍
// ──────────────────────────────────────────────

// encodePipeline은 메시지를 코덱으로 직렬화 → 압축 → 프레이밍한다.
func encodePipeline(msg any, codec Codec, comp Compressor) ([]byte, error) {
	// 1단계: 코덱으로 직렬화
	data, err := codec.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("직렬화 실패: %w", err)
	}
	fmt.Printf("    직렬화 (%s): %d bytes\n", codec.Name(), len(data))

	// 2단계: 압축
	compressed := false
	if comp != nil && comp.Name() != "identity" {
		var buf bytes.Buffer
		w, err := comp.Compress(&buf)
		if err != nil {
			return nil, fmt.Errorf("압축 초기화 실패: %w", err)
		}
		w.Write(data)
		w.Close()
		data = buf.Bytes()
		compressed = true
		fmt.Printf("    압축 (%s): %d bytes\n", comp.Name(), len(data))
	}

	// 3단계: 5바이트 프레이밍
	frame := EncodeFrame(data, compressed)
	fmt.Printf("    프레이밍: [flag=%d][len=%d] → 총 %d bytes\n",
		boolToInt(compressed), len(data), len(frame))
	return frame, nil
}

// decodePipeline은 프레이밍 → 압축 해제 → 역직렬화한다.
func decodePipeline(frame []byte, result any, codec Codec, comp Compressor) error {
	// 1단계: 프레임 파싱
	compressed, data, err := DecodeFrame(frame)
	if err != nil {
		return err
	}
	fmt.Printf("    프레임 파싱: compressed=%v, data=%d bytes\n", compressed, len(data))

	// 2단계: 압축 해제
	if compressed && comp != nil {
		r, err := comp.Decompress(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("압축 해제 실패: %w", err)
		}
		data, err = io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("압축 해제 읽기 실패: %w", err)
		}
		fmt.Printf("    압축 해제 (%s): %d bytes\n", comp.Name(), len(data))
	}

	// 3단계: 역직렬화
	if err := codec.Unmarshal(data, result); err != nil {
		return fmt.Errorf("역직렬화 실패: %w", err)
	}
	fmt.Printf("    역직렬화 (%s): 성공\n", codec.Name())
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ──────────────────────────────────────────────
// 9. main
// ──────────────────────────────────────────────

func main() {
	fmt.Println("=== 코덱 레지스트리 & 압축 시뮬레이션 ===")
	fmt.Println()

	// 코덱 & 압축기 등록
	fmt.Println("── 1. 레지스트리 등록 ──")
	RegisterCodec(&JSONCodec{})
	RegisterCodec(&SimpleBinaryCodec{})
	RegisterCompressor(&GzipCompressor{})
	RegisterCompressor(&IdentityCompressor{})

	// 레지스트리 조회
	fmt.Println()
	fmt.Println("── 2. 레지스트리 조회 ──")
	jsonCodec := GetCodec("json")
	binaryCodec := GetCodec("proto-sim")
	gzipComp := GetCompressor("gzip")
	identityComp := GetCompressor("identity")
	fmt.Printf("  GetCodec('json'): %s\n", jsonCodec.Name())
	fmt.Printf("  GetCodec('proto-sim'): %s\n", binaryCodec.Name())
	fmt.Printf("  GetCompressor('gzip'): %s\n", gzipComp.Name())
	fmt.Printf("  GetCompressor('identity'): %s\n", identityComp.Name())

	nilCodec := GetCodec("msgpack")
	fmt.Printf("  GetCodec('msgpack'): %v (미등록)\n", nilCodec)

	// JSON 코덱 테스트
	fmt.Println()
	fmt.Println("── 3. JSON 코덱 직렬화/역직렬화 ──")
	type Request struct {
		Name    string `json:"name"`
		Age     int    `json:"age"`
		Message string `json:"message"`
	}
	req := &Request{Name: "gRPC", Age: 10, Message: "JSON 코덱 테스트입니다"}

	data, _ := jsonCodec.Marshal(req)
	fmt.Printf("  원본: %+v\n", req)
	fmt.Printf("  직렬화: %s (%d bytes)\n", string(data), len(data))

	var decoded Request
	jsonCodec.Unmarshal(data, &decoded)
	fmt.Printf("  역직렬화: %+v\n", decoded)

	// 바이너리 코덱 테스트
	fmt.Println()
	fmt.Println("── 4. 바이너리 코덱 (protobuf 시뮬레이션) ──")
	binMsg := &SimpleMessage{ID: 42, Content: "안녕하세요 gRPC!"}
	binData, _ := binaryCodec.Marshal(binMsg)
	fmt.Printf("  원본: ID=%d, Content='%s'\n", binMsg.ID, binMsg.Content)
	fmt.Printf("  직렬화: %d bytes (hex: %x)\n", len(binData), binData)

	var binDecoded SimpleMessage
	binaryCodec.Unmarshal(binData, &binDecoded)
	fmt.Printf("  역직렬화: ID=%d, Content='%s'\n", binDecoded.ID, binDecoded.Content)

	// 압축 테스트
	fmt.Println()
	fmt.Println("── 5. Gzip 압축/해제 ──")
	original := []byte("gRPC는 고성능 원격 프로시저 호출 프레임워크입니다. " +
		"HTTP/2 기반으로 양방향 스트리밍을 지원합니다. " +
		"Protocol Buffers를 기본 직렬화 형식으로 사용합니다.")
	fmt.Printf("  원본: %d bytes\n", len(original))

	// 압축
	var compBuf bytes.Buffer
	w, _ := gzipComp.Compress(&compBuf)
	w.Write(original)
	w.Close()
	compressed := compBuf.Bytes()
	fmt.Printf("  압축: %d bytes (%.1f%% 감소)\n",
		len(compressed), (1-float64(len(compressed))/float64(len(original)))*100)

	// 해제
	r, _ := gzipComp.Decompress(bytes.NewReader(compressed))
	decompressed, _ := io.ReadAll(r)
	fmt.Printf("  해제: %d bytes\n", len(decompressed))
	fmt.Printf("  일치: %v\n", bytes.Equal(original, decompressed))

	// 5바이트 메시지 프레이밍
	fmt.Println()
	fmt.Println("── 6. 5바이트 메시지 프레이밍 ──")
	testData := []byte("Hello gRPC")

	// 비압축 프레임
	frame1 := EncodeFrame(testData, false)
	fmt.Printf("  비압축 프레임: [%02x %02x%02x%02x%02x] + %d bytes data = 총 %d bytes\n",
		frame1[0], frame1[1], frame1[2], frame1[3], frame1[4],
		len(testData), len(frame1))

	c1, d1, _ := DecodeFrame(frame1)
	fmt.Printf("  디코딩: compressed=%v, data='%s'\n", c1, string(d1))

	// 압축 프레임
	frame2 := EncodeFrame(compressed, true)
	fmt.Printf("  압축 프레임: flag=%d, length=%d, 총 %d bytes\n",
		frame2[0], binary.BigEndian.Uint32(frame2[1:5]), len(frame2))

	// 엔드투엔드 파이프라인
	fmt.Println()
	fmt.Println("── 7. 엔드투엔드 파이프라인 ──")

	pipeMsg := &Request{Name: "gRPC-Go", Age: 9, Message: "엔드투엔드 테스트"}

	// 파이프라인 1: JSON + gzip
	fmt.Println()
	fmt.Println("  [JSON + gzip]")
	fmt.Println("  인코딩:")
	frame, _ := encodePipeline(pipeMsg, jsonCodec, gzipComp)

	fmt.Println("  디코딩:")
	var result1 Request
	decodePipeline(frame, &result1, jsonCodec, gzipComp)
	fmt.Printf("  결과: %+v\n", result1)

	// 파이프라인 2: JSON + 비압축
	fmt.Println()
	fmt.Println("  [JSON + identity (비압축)]")
	fmt.Println("  인코딩:")
	frame2Data, _ := encodePipeline(pipeMsg, jsonCodec, identityComp)

	fmt.Println("  디코딩:")
	var result2 Request
	decodePipeline(frame2Data, &result2, jsonCodec, identityComp)
	fmt.Printf("  결과: %+v\n", result2)

	// 파이프라인 3: 바이너리 + gzip
	fmt.Println()
	fmt.Println("  [proto-sim + gzip]")
	binMsg2 := &SimpleMessage{ID: 100, Content: "바이너리 코덱 + gzip 압축 테스트"}
	fmt.Println("  인코딩:")
	frame3, _ := encodePipeline(binMsg2, binaryCodec, gzipComp)

	fmt.Println("  디코딩:")
	var result3 SimpleMessage
	decodePipeline(frame3, &result3, binaryCodec, gzipComp)
	fmt.Printf("  결과: ID=%d, Content='%s'\n", result3.ID, result3.Content)

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
