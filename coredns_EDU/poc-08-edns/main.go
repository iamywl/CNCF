// poc-08-edns: CoreDNS EDNS0 처리 메커니즘 시뮬레이션
//
// CoreDNS의 EDNS0(Extension mechanisms for DNS) 처리를 재현한다.
// OPT RR 파싱, UDP 버퍼 크기 협상, DO 플래그, Scrub 패턴(응답 크기 제한)을 구현한다.
//
// 실제 소스 참조:
//   - request/request.go: Size(), SizeAndDo(), Scrub(), Do()
//   - request/writer.go: ScrubWriter
//   - request/edns0.go: supportedOptions()
//
// 실행: go run main.go

package main

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// ============================================================================
// 1. DNS 메시지 구조 (EDNS 관련)
// ============================================================================

// DNS 레코드 타입 상수
const (
	TypeA   uint16 = 1
	TypeOPT uint16 = 41 // EDNS OPT 레코드
	TypeAAAA uint16 = 28
)

// DNS 헤더 플래그
const (
	FlagQR = 1 << 15 // Query/Response
	FlagAA = 1 << 10 // Authoritative Answer
	FlagTC = 1 << 9  // TrunCated
	FlagRD = 1 << 8  // Recursion Desired
	FlagRA = 1 << 7  // Recursion Available
	FlagAD = 1 << 5  // Authenticated Data
	FlagCD = 1 << 4  // Checking Disabled
)

// EDNS0 옵션 코드
const (
	EDNS0NSID         = 3   // NSID
	EDNS0EXPIRE       = 9   // EXPIRE
	EDNS0COOKIE       = 10  // DNS Cookie
	EDNS0TCPKEEPALIVE = 11  // TCP Keepalive
	EDNS0PADDING      = 12  // Padding
	EDNS0ECS          = 8   // Client Subnet (EDNS Client Subnet)
)

// EDNS0Option은 EDNS0 옵션을 나타낸다
type EDNS0Option struct {
	Code uint16
	Data []byte
}

// OptionName은 옵션 코드의 이름을 반환한다
func (o EDNS0Option) OptionName() string {
	switch o.Code {
	case EDNS0NSID:
		return "NSID"
	case EDNS0EXPIRE:
		return "EXPIRE"
	case EDNS0COOKIE:
		return "COOKIE"
	case EDNS0TCPKEEPALIVE:
		return "TCP_KEEPALIVE"
	case EDNS0PADDING:
		return "PADDING"
	case EDNS0ECS:
		return "CLIENT_SUBNET"
	default:
		return fmt.Sprintf("OPT_%d", o.Code)
	}
}

// OPTRecord는 EDNS OPT RR을 나타낸다 (RFC 6891)
// OPT RR 형식:
//   - Name: "." (루트)
//   - Type: 41 (OPT)
//   - Class: UDP 페이로드 크기 (바이트)
//   - TTL: 확장 RCODE (8비트) + 버전 (8비트) + DO 플래그 (1비트) + Z (15비트)
//   - RDATA: EDNS0 옵션들
type OPTRecord struct {
	UDPSize uint16        // UDP 버퍼 크기 (Class 필드에 인코딩)
	ExtRcode uint8        // 확장 RCODE (TTL 상위 8비트)
	Version  uint8        // EDNS 버전 (TTL 9~16비트)
	DO       bool         // DNSSEC OK 플래그 (TTL 17번째 비트)
	Options  []EDNS0Option // EDNS0 옵션들
}

// EncodeTTL은 OPT RR의 TTL 필드를 인코딩한다
func (o *OPTRecord) EncodeTTL() uint32 {
	var ttl uint32
	ttl |= uint32(o.ExtRcode) << 24
	ttl |= uint32(o.Version) << 16
	if o.DO {
		ttl |= 0x8000 // 비트 15 (DO 플래그)
	}
	return ttl
}

// DecodeTTL은 TTL 필드에서 EDNS 정보를 추출한다
func (o *OPTRecord) DecodeTTL(ttl uint32) {
	o.ExtRcode = uint8(ttl >> 24)
	o.Version = uint8(ttl >> 16)
	o.DO = (ttl & 0x8000) != 0
}

// RR은 DNS 리소스 레코드
type RR struct {
	Name  string
	Type  uint16
	Value string
	TTL   uint32
	Size  int // 바이트 크기 (시뮬레이션)
}

// DNSMessage는 DNS 메시지
type DNSMessage struct {
	ID       uint16
	Flags    uint16
	Question struct {
		Name  string
		Type  uint16
	}
	Answer []RR
	Ns     []RR
	Extra  []RR
	OPT    *OPTRecord // EDNS OPT 레코드 (Extra에서 추출)

	Truncated bool
	Compress  bool
}

// IsEdns0은 메시지에서 OPT 레코드를 찾아 반환한다
// 실제 소스: miekg/dns의 (*Msg).IsEdns0()
func (m *DNSMessage) IsEdns0() *OPTRecord {
	return m.OPT
}

// MessageSize는 메시지의 대략적인 크기를 계산한다 (바이트)
func (m *DNSMessage) MessageSize() int {
	size := 12 // DNS 헤더
	size += len(m.Question.Name) + 4 // Question 섹션

	for _, rr := range m.Answer {
		size += rr.Size
		if rr.Size == 0 {
			size += len(rr.Name) + len(rr.Value) + 12 // 기본 추정
		}
	}
	for _, rr := range m.Ns {
		size += rr.Size
		if rr.Size == 0 {
			size += len(rr.Name) + len(rr.Value) + 12
		}
	}
	for _, rr := range m.Extra {
		size += rr.Size
		if rr.Size == 0 {
			size += len(rr.Name) + len(rr.Value) + 12
		}
	}
	if m.OPT != nil {
		size += 11 + len(m.OPT.Options)*8
	}
	return size
}

// ============================================================================
// 2. EDNS 크기 결정 - request/request.go의 Size() 재현
// ============================================================================

// ednsSize는 EDNS 광고 크기를 정규화한다
// 실제 소스: plugin/pkg/edns/edns.go의 Size()
// TCP이면 65535, EDNS 없으면 512, EDNS 있으면 max(512, min(advertised, 4096))
func ednsSize(proto string, size uint16) uint16 {
	if proto == "tcp" {
		return 65535
	}

	if size == 0 {
		return 512 // EDNS 없음 → 표준 DNS 최대 크기
	}

	// 최소 512, 최대 4096으로 정규화
	if size < 512 {
		size = 512
	}
	if size > 4096 {
		size = 4096
	}
	return size
}

// ============================================================================
// 3. 지원 옵션 필터링 - request/edns0.go의 supportedOptions() 재현
// ============================================================================

// isSupportedOption은 EDNS0 옵션이 지원되는지 확인한다
// 실제 소스: request/edns0.go의 supportedOptions()
// CoreDNS가 기본 지원하는 옵션: NSID, EXPIRE, COOKIE, TCP_KEEPALIVE, PADDING
func isSupportedOption(code uint16) bool {
	switch code {
	case EDNS0NSID, EDNS0EXPIRE, EDNS0COOKIE, EDNS0TCPKEEPALIVE, EDNS0PADDING:
		return true
	default:
		return false
	}
}

// filterSupportedOptions는 지원되는 옵션만 필터링한다
func filterSupportedOptions(options []EDNS0Option) []EDNS0Option {
	supported := make([]EDNS0Option, 0, len(options))
	for _, opt := range options {
		if isSupportedOption(opt.Code) {
			supported = append(supported, opt)
		}
	}
	return supported
}

// ============================================================================
// 4. SizeAndDo - request/request.go의 SizeAndDo() 재현
// ============================================================================

// sizeAndDo는 요청의 EDNS 정보를 응답에 반영한다
// 실제 소스: request/request.go의 func (r *Request) SizeAndDo(m *dns.Msg) bool
// 동작:
//   1. 요청에 OPT가 없으면 false 반환
//   2. 응답에 기존 OPT가 있으면 재사용 (버전=0, UDP 크기 복사, DO 복사)
//   3. 없으면 요청의 OPT를 복사하여 응답 Extra에 추가
func sizeAndDo(req *DNSMessage, resp *DNSMessage) bool {
	o := req.IsEdns0()
	if o == nil {
		return false
	}

	if resp.OPT != nil {
		// 응답에 기존 OPT가 있으면 재사용
		resp.OPT.Version = 0
		resp.OPT.UDPSize = o.UDPSize
		resp.OPT.ExtRcode = 0
		if o.DO {
			resp.OPT.DO = true
		}
		return true
	}

	// 요청의 OPT를 복사하여 응답에 추가
	newOPT := &OPTRecord{
		UDPSize: o.UDPSize,
		Version: 0,
		DO:      o.DO,
		Options: filterSupportedOptions(o.Options),
	}
	resp.OPT = newOPT
	return true
}

// ============================================================================
// 5. Scrub 패턴 - request/request.go의 Scrub() 재현
// ============================================================================

// scrub은 응답을 클라이언트 버퍼 크기에 맞게 자른다
// 실제 소스: request/request.go의 func (r *Request) Scrub()
// 동작:
//   1. reply.Truncate(size)로 메시지를 자름
//   2. UDP일 때 크기가 1480(IPv4) 또는 1220(IPv6)을 초과하면 압축 활성화
//   3. TC (Truncated) 비트 설정
func scrub(reply *DNSMessage, maxSize int, proto string, family int) *DNSMessage {
	// 1. 크기에 맞게 레코드 제거 (Truncate 시뮬레이션)
	truncated := truncateMessage(reply, maxSize)

	if truncated {
		reply.Truncated = true
		reply.Flags |= FlagTC
	}

	// 2. 이미 압축이 활성화되어 있으면 반환
	if reply.Compress {
		return reply
	}

	// 3. UDP에서 단편화 방지를 위한 압축 활성화
	// 실제 소스에서 NSD의 권장값을 따름:
	//   IPv4: 1480, IPv6: 1220
	if proto == "udp" {
		size := reply.MessageSize()
		if size > 1480 && family == 1 { // IPv4
			reply.Compress = true
		}
		if size > 1220 && family == 2 { // IPv6
			reply.Compress = true
		}
	}

	return reply
}

// truncateMessage는 메시지를 최대 크기에 맞게 자른다
func truncateMessage(msg *DNSMessage, maxSize int) bool {
	truncated := false

	// Extra부터 제거, 그 다음 Ns, 마지막으로 Answer
	for msg.MessageSize() > maxSize && len(msg.Extra) > 0 {
		msg.Extra = msg.Extra[:len(msg.Extra)-1]
		truncated = true
	}
	for msg.MessageSize() > maxSize && len(msg.Ns) > 0 {
		msg.Ns = msg.Ns[:len(msg.Ns)-1]
		truncated = true
	}
	for msg.MessageSize() > maxSize && len(msg.Answer) > 0 {
		msg.Answer = msg.Answer[:len(msg.Answer)-1]
		truncated = true
	}

	return truncated
}

// ============================================================================
// 6. OPT RR 바이트 인코딩/디코딩
// ============================================================================

// encodeOPTRR은 OPT 레코드를 와이어 형식으로 인코딩한다
func encodeOPTRR(opt *OPTRecord) []byte {
	// OPT RR 와이어 형식:
	// Name: 0x00 (1 바이트, 루트)
	// Type: 0x0029 (2 바이트, OPT=41)
	// Class: UDP 페이로드 크기 (2 바이트)
	// TTL: ExtRcode(8) + Version(8) + DO(1) + Z(15) (4 바이트)
	// RDLENGTH: 옵션 데이터 길이 (2 바이트)
	// RDATA: 옵션들

	var buf []byte

	// Name: "." (root)
	buf = append(buf, 0x00)

	// Type: OPT (41)
	typeBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(typeBuf, TypeOPT)
	buf = append(buf, typeBuf...)

	// Class: UDP payload size
	classBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(classBuf, opt.UDPSize)
	buf = append(buf, classBuf...)

	// TTL: Extended RCODE + Version + Flags
	ttlBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(ttlBuf, opt.EncodeTTL())
	buf = append(buf, ttlBuf...)

	// RDATA: Options
	var rdata []byte
	for _, o := range opt.Options {
		optBuf := make([]byte, 4)
		binary.BigEndian.PutUint16(optBuf[0:2], o.Code)
		binary.BigEndian.PutUint16(optBuf[2:4], uint16(len(o.Data)))
		rdata = append(rdata, optBuf...)
		rdata = append(rdata, o.Data...)
	}

	rdlenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(rdlenBuf, uint16(len(rdata)))
	buf = append(buf, rdlenBuf...)
	buf = append(buf, rdata...)

	return buf
}

// decodeOPTRR은 바이트에서 OPT 레코드를 디코딩한다
func decodeOPTRR(data []byte) (*OPTRecord, error) {
	if len(data) < 11 { // 최소 크기: name(1) + type(2) + class(2) + ttl(4) + rdlen(2)
		return nil, fmt.Errorf("OPT RR이 너무 짧음: %d 바이트", len(data))
	}

	opt := &OPTRecord{}

	offset := 0

	// Name (1 바이트, 루트)
	if data[offset] != 0x00 {
		return nil, fmt.Errorf("OPT RR Name이 루트(.)가 아님")
	}
	offset++

	// Type (2 바이트)
	rrType := binary.BigEndian.Uint16(data[offset : offset+2])
	if rrType != TypeOPT {
		return nil, fmt.Errorf("OPT RR Type이 41이 아님: %d", rrType)
	}
	offset += 2

	// Class = UDP payload size (2 바이트)
	opt.UDPSize = binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2

	// TTL = ExtRcode + Version + Flags (4 바이트)
	ttl := binary.BigEndian.Uint32(data[offset : offset+4])
	opt.DecodeTTL(ttl)
	offset += 4

	// RDLENGTH (2 바이트)
	rdlen := binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2

	// RDATA: Options
	end := offset + int(rdlen)
	if end > len(data) {
		end = len(data)
	}

	for offset+4 <= end {
		optCode := binary.BigEndian.Uint16(data[offset : offset+2])
		optLen := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		offset += 4

		var optData []byte
		if offset+int(optLen) <= end {
			optData = data[offset : offset+int(optLen)]
			offset += int(optLen)
		}

		opt.Options = append(opt.Options, EDNS0Option{
			Code: optCode,
			Data: optData,
		})
	}

	return opt, nil
}

// ============================================================================
// 7. 데모 실행
// ============================================================================

func main() {
	fmt.Println("=== CoreDNS EDNS 처리 PoC ===")
	fmt.Println()
	fmt.Println("CoreDNS의 EDNS0 (Extension mechanisms for DNS) 처리를 시뮬레이션합니다.")
	fmt.Println("참조: request/request.go, request/writer.go, request/edns0.go")
	fmt.Println()

	// ── 데모 1: OPT RR 파싱 ──
	fmt.Println("── 1. OPT RR 구조 ──")
	fmt.Println()
	fmt.Println("  RFC 6891에 정의된 OPT RR은 EDNS 확장을 위한 메타 레코드이다.")
	fmt.Println("  OPT RR은 Extra 섹션에 위치하며, TTL 필드를 확장 플래그로 재사용한다.")
	fmt.Println()
	fmt.Println("  OPT RR 레이아웃:")
	fmt.Println("  ┌──────────────────────────────────────────────────┐")
	fmt.Println("  │ Name:  \".\" (루트, 1바이트)                       │")
	fmt.Println("  │ Type:  41 (OPT)                                 │")
	fmt.Println("  │ Class: UDP 페이로드 크기 (바이트)                  │")
	fmt.Println("  │ TTL:   [ExtRcode:8][Version:8][DO:1][Z:15]      │")
	fmt.Println("  │ RDATA: [OptCode:16][OptLen:16][OptData:N]...    │")
	fmt.Println("  └──────────────────────────────────────────────────┘")
	fmt.Println()

	// OPT 레코드 생성 및 인코딩
	opt := &OPTRecord{
		UDPSize:  4096,
		Version:  0,
		DO:       true,
		Options: []EDNS0Option{
			{Code: EDNS0COOKIE, Data: []byte("abcdefgh")}, // 8바이트 쿠키
			{Code: EDNS0NSID, Data: []byte("coredns1")},
		},
	}

	encoded := encodeOPTRR(opt)
	fmt.Printf("  인코딩된 OPT RR (%d 바이트):\n", len(encoded))
	fmt.Print("  ")
	for i, b := range encoded {
		if i > 0 && i%16 == 0 {
			fmt.Print("\n  ")
		}
		fmt.Printf("%02x ", b)
	}
	fmt.Println()
	fmt.Println()

	// 디코딩
	decoded, err := decodeOPTRR(encoded)
	if err != nil {
		fmt.Printf("  디코딩 에러: %v\n", err)
		return
	}
	fmt.Printf("  디코딩 결과:\n")
	fmt.Printf("    UDP 버퍼 크기: %d 바이트\n", decoded.UDPSize)
	fmt.Printf("    EDNS 버전:    %d\n", decoded.Version)
	fmt.Printf("    DO (DNSSEC):  %v\n", decoded.DO)
	fmt.Printf("    ExtRcode:     %d\n", decoded.ExtRcode)
	fmt.Printf("    옵션 수:      %d\n", len(decoded.Options))
	for _, o := range decoded.Options {
		fmt.Printf("      - %s (code=%d, len=%d): %q\n",
			o.OptionName(), o.Code, len(o.Data), string(o.Data))
	}
	fmt.Println()

	// ── 데모 2: TTL 필드 인코딩 ──
	fmt.Println("── 2. OPT TTL 필드 비트 레이아웃 ──")
	fmt.Println()
	fmt.Println("  OPT RR의 TTL 필드는 일반 TTL이 아닌 확장 플래그를 저장한다:")
	fmt.Println("  비트 31-24: Extended RCODE (상위 8비트)")
	fmt.Println("  비트 23-16: EDNS Version")
	fmt.Println("  비트 15:    DO (DNSSEC OK) 플래그")
	fmt.Println("  비트 14-0:  Z (예약, 0이어야 함)")
	fmt.Println()

	testOpts := []OPTRecord{
		{UDPSize: 4096, Version: 0, DO: false, ExtRcode: 0},
		{UDPSize: 4096, Version: 0, DO: true, ExtRcode: 0},
		{UDPSize: 1232, Version: 0, DO: true, ExtRcode: 0},
		{UDPSize: 4096, Version: 1, DO: true, ExtRcode: 3},
	}

	for _, o := range testOpts {
		ttl := o.EncodeTTL()
		fmt.Printf("  UDP=%d, Version=%d, DO=%v, ExtRcode=%d\n", o.UDPSize, o.Version, o.DO, o.ExtRcode)
		fmt.Printf("    TTL = 0x%08x (이진: %032b)\n", ttl, ttl)
		fmt.Println()
	}

	// ── 데모 3: UDP 버퍼 크기 협상 ──
	fmt.Println("── 3. UDP 버퍼 크기 협상 ──")
	fmt.Println()
	fmt.Println("  실제 소스: plugin/pkg/edns/edns.go의 Size()")
	fmt.Println("  규칙: TCP=65535, EDNS 없음=512, EDNS=max(512, min(advertised, 4096))")
	fmt.Println()

	sizeTests := []struct {
		proto string
		size  uint16
		desc  string
	}{
		{"udp", 0, "EDNS 없음 (레거시 DNS)"},
		{"udp", 256, "256 (최소 미만)"},
		{"udp", 512, "512 (표준)"},
		{"udp", 1232, "1232 (DNS Flag Day 권장)"},
		{"udp", 4096, "4096 (최대)"},
		{"udp", 8192, "8192 (4096 초과)"},
		{"tcp", 0, "TCP (항상 65535)"},
		{"tcp", 4096, "TCP + EDNS"},
	}

	for _, t := range sizeTests {
		result := ednsSize(t.proto, t.size)
		fmt.Printf("  proto=%-3s, advertised=%-5d → 실제 버퍼=%d  (%s)\n",
			t.proto, t.size, result, t.desc)
	}
	fmt.Println()

	// ── 데모 4: SizeAndDo ──
	fmt.Println("── 4. SizeAndDo (요청 → 응답 EDNS 전파) ──")
	fmt.Println()
	fmt.Println("  실제 소스: request/request.go의 SizeAndDo()")
	fmt.Println("  요청의 EDNS 정보를 응답에 복사하고, 지원하지 않는 옵션은 제거한다.")
	fmt.Println()

	// EDNS 요청
	req := &DNSMessage{
		ID: 0x1234,
		Question: struct {
			Name string
			Type uint16
		}{Name: "example.com.", Type: TypeA},
		OPT: &OPTRecord{
			UDPSize: 4096,
			Version: 0,
			DO:      true,
			Options: []EDNS0Option{
				{Code: EDNS0COOKIE, Data: []byte("client-cookie")},
				{Code: EDNS0ECS, Data: []byte{0, 1, 24, 0, 192, 168, 1}}, // ECS 옵션
				{Code: EDNS0NSID, Data: nil},
			},
		},
	}

	resp := &DNSMessage{
		ID: req.ID,
		Answer: []RR{
			{Name: "example.com.", Type: TypeA, Value: "93.184.216.34", TTL: 300},
		},
	}

	fmt.Printf("  요청 옵션: ")
	for _, o := range req.OPT.Options {
		fmt.Printf("[%s] ", o.OptionName())
	}
	fmt.Println()

	applied := sizeAndDo(req, resp)
	fmt.Printf("  SizeAndDo 적용: %v\n", applied)

	if resp.OPT != nil {
		fmt.Printf("  응답 OPT: UDP=%d, DO=%v, Version=%d\n",
			resp.OPT.UDPSize, resp.OPT.DO, resp.OPT.Version)
		fmt.Printf("  응답 옵션 (지원되는 것만): ")
		for _, o := range resp.OPT.Options {
			fmt.Printf("[%s] ", o.OptionName())
		}
		fmt.Println()
		fmt.Println()
		fmt.Println("  주의: CLIENT_SUBNET(ECS)은 기본 지원 옵션이 아니므로 제거됨")
	}
	fmt.Println()

	// ── 데모 5: Scrub 패턴 ──
	fmt.Println("── 5. Scrub 패턴 (응답 크기 제한) ──")
	fmt.Println()
	fmt.Println("  실제 소스: request/request.go의 Scrub()")
	fmt.Println("  클라이언트 버퍼 크기를 초과하면 레코드를 제거하고 TC 비트를 설정한다.")
	fmt.Println()

	// 큰 응답 생성
	largeResp := &DNSMessage{
		ID: 0x5678,
		Question: struct {
			Name string
			Type uint16
		}{Name: "many-records.example.com.", Type: TypeA},
	}

	// 20개의 A 레코드 추가 (각각 ~40바이트)
	for i := 0; i < 20; i++ {
		largeResp.Answer = append(largeResp.Answer, RR{
			Name:  "many-records.example.com.",
			Type:  TypeA,
			Value: fmt.Sprintf("10.0.%d.%d", i/256, i%256),
			TTL:   300,
			Size:  40,
		})
	}

	// 5개의 NS 레코드
	for i := 0; i < 5; i++ {
		largeResp.Ns = append(largeResp.Ns, RR{
			Name:  "example.com.",
			Type:  2, // NS
			Value: fmt.Sprintf("ns%d.example.com.", i+1),
			TTL:   3600,
			Size:  35,
		})
	}

	// 5개의 Extra 레코드
	for i := 0; i < 5; i++ {
		largeResp.Extra = append(largeResp.Extra, RR{
			Name:  fmt.Sprintf("ns%d.example.com.", i+1),
			Type:  TypeA,
			Value: fmt.Sprintf("198.51.100.%d", i+1),
			TTL:   3600,
			Size:  35,
		})
	}

	originalSize := largeResp.MessageSize()
	originalAnswer := len(largeResp.Answer)
	originalNs := len(largeResp.Ns)
	originalExtra := len(largeResp.Extra)

	fmt.Printf("  원본 응답: %d 바이트 (Answer=%d, Ns=%d, Extra=%d)\n",
		originalSize, originalAnswer, originalNs, originalExtra)

	// 다양한 버퍼 크기로 Scrub
	bufferSizes := []struct {
		size   int
		proto  string
		family int
		desc   string
	}{
		{512, "udp", 1, "레거시 DNS (512B)"},
		{1232, "udp", 1, "DNS Flag Day (1232B)"},
		{4096, "udp", 1, "EDNS 최대 (4096B)"},
	}

	for _, bs := range bufferSizes {
		// 원본 복사
		msg := &DNSMessage{
			ID: largeResp.ID,
			Question: largeResp.Question,
		}
		msg.Answer = make([]RR, len(largeResp.Answer))
		copy(msg.Answer, largeResp.Answer)
		msg.Ns = make([]RR, len(largeResp.Ns))
		copy(msg.Ns, largeResp.Ns)
		msg.Extra = make([]RR, len(largeResp.Extra))
		copy(msg.Extra, largeResp.Extra)

		scrub(msg, bs.size, bs.proto, bs.family)

		tcStr := ""
		if msg.Truncated {
			tcStr = " [TC=1]"
		}
		compStr := ""
		if msg.Compress {
			compStr = " [압축]"
		}

		fmt.Printf("  버퍼=%4d: Answer=%2d, Ns=%d, Extra=%d, 크기≈%d%s%s  (%s)\n",
			bs.size, len(msg.Answer), len(msg.Ns), len(msg.Extra),
			msg.MessageSize(), tcStr, compStr, bs.desc)
	}
	fmt.Println()

	// ── 데모 6: DO 플래그와 AD 비트 ──
	fmt.Println("── 6. DO 플래그와 AD 비트 상호작용 ──")
	fmt.Println()
	fmt.Println("  실제 소스: plugin/cache/cache.go WriteMsg()에서:")
	fmt.Println("    if !do && !ad { res.AuthenticatedData = false }")
	fmt.Println()
	fmt.Println("  요청에 DO=false, AD=false이면 응답의 AD 비트를 제거한다.")
	fmt.Println("  RFC 6840 5.7-5.8: 요청에 AD가 설정된 경우 응답에도 유지한다.")
	fmt.Println()

	doCases := []struct {
		do       bool
		ad       bool
		respAD   bool
		expected bool
	}{
		{false, false, true, false},  // DO=0, AD=0 → AD 제거
		{true, false, true, true},   // DO=1 → AD 유지
		{false, true, true, true},   // AD=1 → AD 유지 (RFC 6840)
		{true, true, true, true},    // DO=1, AD=1 → AD 유지
	}

	fmt.Println("  요청 DO  요청 AD  응답 AD  결과 AD")
	fmt.Println("  ──────  ──────  ──────  ──────")
	for _, tc := range doCases {
		resultAD := tc.respAD
		if !tc.do && !tc.ad {
			resultAD = false
		}
		match := ""
		if resultAD == tc.expected {
			match = "OK"
		} else {
			match = "FAIL"
		}
		fmt.Printf("  %-7v %-7v %-7v %-7v %s\n",
			tc.do, tc.ad, tc.respAD, resultAD, match)
	}
	fmt.Println()

	// ── 데모 7: 단편화 임계값 ──
	fmt.Println("── 7. UDP 단편화 방지 임계값 ──")
	fmt.Println()
	fmt.Println("  실제 소스: request/request.go Scrub()에서 NSD 권장값 사용:")
	fmt.Println("    IPv4: 1480 바이트 초과 시 압축 활성화")
	fmt.Println("    IPv6: 1220 바이트 초과 시 압축 활성화")
	fmt.Println()

	fragTests := []struct {
		size   int
		family int
		desc   string
	}{
		{1200, 1, "IPv4, 1200B"},
		{1481, 1, "IPv4, 1481B (임계값 초과)"},
		{1000, 2, "IPv6, 1000B"},
		{1221, 2, "IPv6, 1221B (임계값 초과)"},
	}

	for _, ft := range fragTests {
		msg := &DNSMessage{}
		// 크기를 맞추기 위해 레코드 추가
		for msg.MessageSize() < ft.size {
			msg.Answer = append(msg.Answer, RR{
				Name: "test.example.com.", Type: TypeA,
				Value: "10.0.0.1", TTL: 300, Size: 40,
			})
		}

		scrub(msg, 65535, "udp", ft.family) // 최대 버퍼로 Scrub (자르지 않고 압축만)
		compress := "아니오"
		if msg.Compress {
			compress = "예"
		}
		familyStr := "IPv4"
		if ft.family == 2 {
			familyStr = "IPv6"
		}
		fmt.Printf("  %-5s %5dB → 압축: %s  (%s)\n",
			familyStr, ft.size, compress, ft.desc)
	}
	fmt.Println()

	// ── 데모 8: ScrubWriter 패턴 ──
	fmt.Println("── 8. ScrubWriter 패턴 ──")
	fmt.Println()
	fmt.Println("  실제 소스: request/writer.go의 ScrubWriter")
	fmt.Println("  ScrubWriter는 ResponseWriter를 래핑하여 WriteMsg() 시 자동으로")
	fmt.Println("  SizeAndDo + Scrub를 적용하는 데코레이터 패턴이다.")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │ WriteMsg(m) 호출                                        │")
	fmt.Println("  │   1. SizeAndDo(m) → 요청 EDNS를 응답에 반영             │")
	fmt.Println("  │   2. Scrub(m) → 버퍼 크기에 맞게 자르기                  │")
	fmt.Println("  │   3. ResponseWriter.WriteMsg(m) → 실제 전송             │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Println()

	// ScrubWriter 시뮬레이션
	scrubReq := &DNSMessage{
		OPT: &OPTRecord{UDPSize: 1232, DO: true},
	}

	scrubResp := &DNSMessage{
		Answer: make([]RR, 0, 15),
	}
	for i := 0; i < 15; i++ {
		scrubResp.Answer = append(scrubResp.Answer, RR{
			Name: "big.example.com.", Type: TypeA,
			Value: fmt.Sprintf("10.0.%d.%d", i/256, i%256), TTL: 300, Size: 40,
		})
	}

	fmt.Printf("  요청: UDP=%d, DO=%v\n", scrubReq.OPT.UDPSize, scrubReq.OPT.DO)
	fmt.Printf("  응답 (Scrub 전): Answer=%d, 크기≈%d\n",
		len(scrubResp.Answer), scrubResp.MessageSize())

	// Step 1: SizeAndDo
	sizeAndDo(scrubReq, scrubResp)
	// Step 2: Scrub
	maxSize := int(ednsSize("udp", scrubReq.OPT.UDPSize))
	scrub(scrubResp, maxSize, "udp", 1)

	tcStr := "아니오"
	if scrubResp.Truncated {
		tcStr = "예"
	}
	fmt.Printf("  응답 (Scrub 후): Answer=%d, 크기≈%d, TC=%s\n",
		len(scrubResp.Answer), scrubResp.MessageSize(), tcStr)
	if scrubResp.OPT != nil {
		fmt.Printf("  응답 OPT: UDP=%d, DO=%v\n", scrubResp.OPT.UDPSize, scrubResp.OPT.DO)
	}
	fmt.Println()

	// ── 요약 ──
	fmt.Println("── EDNS0 지원 옵션 요약 ──")
	fmt.Println()
	allOptions := []uint16{EDNS0NSID, EDNS0EXPIRE, EDNS0COOKIE, EDNS0TCPKEEPALIVE, EDNS0PADDING, EDNS0ECS, 100}
	fmt.Println("  코드  이름            지원")
	fmt.Println("  ────  ──────────────  ────")
	for _, code := range allOptions {
		opt := EDNS0Option{Code: code}
		supported := "예"
		if !isSupportedOption(code) {
			supported = strings.Repeat(" ", 0) + "아니오"
		}
		fmt.Printf("  %-5d %-15s %s\n", code, opt.OptionName(), supported)
	}
	fmt.Println()

	fmt.Println("=== PoC 완료 ===")
}
