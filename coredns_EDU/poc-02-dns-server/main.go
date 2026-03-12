// poc-02-dns-server: CoreDNS UDP/TCP DNS 서버 시뮬레이션
//
// CoreDNS의 DNS 서버 엔진을 재현한다:
//   - UDP/TCP 듀얼 프로토콜 DNS 서버
//   - DNS 메시지 바이너리 파싱 (RFC 1035 헤더 12바이트 + 질문 섹션)
//   - A 레코드 응답 생성
//   - Zone 기반 라우팅 (최장 매칭, core/dnsserver/server.go:295-330)
//   - 내장 dig 시뮬레이터로 쿼리 테스트
//
// 참조: core/dnsserver/server.go (Server 구조체, ServeDNS 메서드)
//
// 사용법: go run main.go

package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// DNS 프로토콜 상수 (RFC 1035)
// =============================================================================

const (
	// DNS 헤더 크기: 12바이트 고정
	dnsHeaderSize = 12

	// DNS 레코드 타입
	TypeA    uint16 = 1   // A 레코드 (IPv4 주소)
	TypeAAAA uint16 = 28  // AAAA 레코드 (IPv6 주소)
	TypeNS   uint16 = 2   // NS 레코드
	TypeSOA  uint16 = 6   // SOA 레코드

	// DNS 클래스
	ClassIN uint16 = 1 // Internet

	// DNS 응답 코드
	RcodeSuccess     = 0
	RcodeFormatError = 1
	RcodeServerFail  = 2
	RcodeNameError   = 3 // NXDOMAIN
	RcodeRefused     = 5
)

// =============================================================================
// DNS 메시지 구조체
// =============================================================================

// DNSHeader는 DNS 메시지 헤더를 나타낸다 (12바이트).
// RFC 1035 Section 4.1.1:
//
//	                                1  1  1  1  1  1
//	  0  1  2  3  4  5  6  7  8  9  0  1  2  3  4  5
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                      ID                       |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|QR|   Opcode  |AA|TC|RD|RA|   Z    |   RCODE   |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                    QDCOUNT                     |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                    ANCOUNT                     |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                    NSCOUNT                     |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                    ARCOUNT                     |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
type DNSHeader struct {
	ID      uint16
	Flags   uint16
	QDCount uint16 // 질문 섹션 레코드 수
	ANCount uint16 // 응답 섹션 레코드 수
	NSCount uint16 // 권한 섹션 레코드 수
	ARCount uint16 // 추가 섹션 레코드 수
}

// DNSQuestion은 DNS 질문 섹션을 나타낸다.
type DNSQuestion struct {
	Name  string
	Type  uint16
	Class uint16
}

// DNSRecord는 DNS 리소스 레코드를 나타낸다.
type DNSRecord struct {
	Name  string
	Type  uint16
	Class uint16
	TTL   uint32
	Data  []byte
}

// DNSMessage는 전체 DNS 메시지를 나타낸다.
type DNSMessage struct {
	Header    DNSHeader
	Questions []DNSQuestion
	Answers   []DNSRecord
}

// =============================================================================
// DNS 메시지 바이너리 파싱
// =============================================================================

// ParseHeader는 바이너리 데이터에서 DNS 헤더를 파싱한다.
func ParseHeader(data []byte) (DNSHeader, error) {
	if len(data) < dnsHeaderSize {
		return DNSHeader{}, fmt.Errorf("데이터가 너무 짧음: %d < %d", len(data), dnsHeaderSize)
	}

	return DNSHeader{
		ID:      binary.BigEndian.Uint16(data[0:2]),
		Flags:   binary.BigEndian.Uint16(data[2:4]),
		QDCount: binary.BigEndian.Uint16(data[4:6]),
		ANCount: binary.BigEndian.Uint16(data[6:8]),
		NSCount: binary.BigEndian.Uint16(data[8:10]),
		ARCount: binary.BigEndian.Uint16(data[10:12]),
	}, nil
}

// ParseName은 DNS 이름 인코딩을 파싱한다.
// RFC 1035: 각 레이블은 (길이 바이트 + 문자열)로 인코딩되며, 0으로 종료.
// 예: "example.com" → [7]example[3]com[0]
func ParseName(data []byte, offset int) (string, int, error) {
	var parts []string
	pos := offset

	for {
		if pos >= len(data) {
			return "", pos, fmt.Errorf("이름 파싱 중 데이터 끝 도달")
		}

		length := int(data[pos])
		if length == 0 {
			pos++
			break
		}

		// 포인터 압축 (11으로 시작하는 2바이트)은 이 시뮬레이션에서 미지원
		if length&0xC0 == 0xC0 {
			if pos+1 >= len(data) {
				return "", pos, fmt.Errorf("포인터 압축 오프셋 부족")
			}
			ptrOffset := int(binary.BigEndian.Uint16(data[pos:pos+2]) & 0x3FFF)
			name, _, err := ParseName(data, ptrOffset)
			if err != nil {
				return "", pos, err
			}
			if len(parts) > 0 {
				return strings.Join(parts, ".") + "." + name, pos + 2, nil
			}
			return name, pos + 2, nil
		}

		pos++
		if pos+length > len(data) {
			return "", pos, fmt.Errorf("레이블 길이 초과: pos=%d, len=%d", pos, length)
		}
		parts = append(parts, string(data[pos:pos+length]))
		pos += length
	}

	return strings.Join(parts, ".") + ".", pos, nil
}

// ParseQuestion은 질문 섹션을 파싱한다.
func ParseQuestion(data []byte, offset int) (DNSQuestion, int, error) {
	name, newOffset, err := ParseName(data, offset)
	if err != nil {
		return DNSQuestion{}, offset, err
	}

	if newOffset+4 > len(data) {
		return DNSQuestion{}, offset, fmt.Errorf("질문 섹션 데이터 부족")
	}

	qtype := binary.BigEndian.Uint16(data[newOffset : newOffset+2])
	qclass := binary.BigEndian.Uint16(data[newOffset+2 : newOffset+4])

	return DNSQuestion{
		Name:  name,
		Type:  qtype,
		Class: qclass,
	}, newOffset + 4, nil
}

// ParseMessage는 전체 DNS 메시지를 파싱한다.
func ParseMessage(data []byte) (DNSMessage, error) {
	header, err := ParseHeader(data)
	if err != nil {
		return DNSMessage{}, err
	}

	msg := DNSMessage{Header: header}
	offset := dnsHeaderSize

	for i := 0; i < int(header.QDCount); i++ {
		q, newOffset, err := ParseQuestion(data, offset)
		if err != nil {
			return DNSMessage{}, err
		}
		msg.Questions = append(msg.Questions, q)
		offset = newOffset
	}

	return msg, nil
}

// =============================================================================
// DNS 메시지 직렬화
// =============================================================================

// EncodeName은 도메인 이름을 DNS 인코딩으로 변환한다.
func EncodeName(name string) []byte {
	var result []byte
	// "example.com." → ["example", "com"]
	name = strings.TrimSuffix(name, ".")
	parts := strings.Split(name, ".")

	for _, part := range parts {
		result = append(result, byte(len(part)))
		result = append(result, []byte(part)...)
	}
	result = append(result, 0) // 종료 바이트
	return result
}

// SerializeHeader는 DNS 헤더를 바이너리로 직렬화한다.
func SerializeHeader(h DNSHeader) []byte {
	buf := make([]byte, dnsHeaderSize)
	binary.BigEndian.PutUint16(buf[0:2], h.ID)
	binary.BigEndian.PutUint16(buf[2:4], h.Flags)
	binary.BigEndian.PutUint16(buf[4:6], h.QDCount)
	binary.BigEndian.PutUint16(buf[6:8], h.ANCount)
	binary.BigEndian.PutUint16(buf[8:10], h.NSCount)
	binary.BigEndian.PutUint16(buf[10:12], h.ARCount)
	return buf
}

// SerializeQuestion은 질문 섹션을 바이너리로 직렬화한다.
func SerializeQuestion(q DNSQuestion) []byte {
	result := EncodeName(q.Name)
	buf := make([]byte, 4)
	binary.BigEndian.PutUint16(buf[0:2], q.Type)
	binary.BigEndian.PutUint16(buf[2:4], q.Class)
	return append(result, buf...)
}

// SerializeRecord는 리소스 레코드를 바이너리로 직렬화한다.
func SerializeRecord(r DNSRecord) []byte {
	result := EncodeName(r.Name)
	buf := make([]byte, 10)
	binary.BigEndian.PutUint16(buf[0:2], r.Type)
	binary.BigEndian.PutUint16(buf[2:4], r.Class)
	binary.BigEndian.PutUint32(buf[4:8], r.TTL)
	binary.BigEndian.PutUint16(buf[8:10], uint16(len(r.Data)))
	result = append(result, buf...)
	result = append(result, r.Data...)
	return result
}

// SerializeMessage는 전체 DNS 메시지를 바이너리로 직렬화한다.
func SerializeMessage(msg DNSMessage) []byte {
	result := SerializeHeader(msg.Header)
	for _, q := range msg.Questions {
		result = append(result, SerializeQuestion(q)...)
	}
	for _, a := range msg.Answers {
		result = append(result, SerializeRecord(a)...)
	}
	return result
}

// =============================================================================
// Zone 기반 라우팅 (CoreDNS server.go:295-330 재현)
// =============================================================================

// ZoneConfig는 존별 설정을 나타낸다.
type ZoneConfig struct {
	Zone    string            // 존 이름 (예: "example.com.")
	Records map[string]net.IP // 레코드 맵: "www.example.com." → IP
}

// DNSServer는 CoreDNS Server 구조체의 단순화 버전이다.
// 실제 CoreDNS(core/dnsserver/server.go:36):
//
//	type Server struct {
//	    Addr  string
//	    zones map[string][]*Config
//	    ...
//	}
type DNSServer struct {
	Addr    string
	zones   map[string]*ZoneConfig // 존 이름 → 설정
	udpConn *net.UDPConn
	tcpLn   net.Listener
	mu      sync.Mutex
	stopped bool
}

// NewDNSServer는 새 DNS 서버를 생성한다.
func NewDNSServer(addr string) *DNSServer {
	return &DNSServer{
		Addr:  addr,
		zones: make(map[string]*ZoneConfig),
	}
}

// AddZone은 서버에 존을 추가한다.
func (s *DNSServer) AddZone(zone string, records map[string]net.IP) {
	s.zones[zone] = &ZoneConfig{
		Zone:    zone,
		Records: records,
	}
}

// matchZone은 쿼리 이름에 대해 최장 매칭 존을 찾는다.
// CoreDNS server.go:295-330의 존 라우팅 로직 재현:
//
//	q := strings.ToLower(r.Question[0].Name)
//	for {
//	    if z, ok := s.zones[q[off:]]; ok { ... }
//	    off, end = dns.NextLabel(q, off)
//	    if end { break }
//	}
func (s *DNSServer) matchZone(qname string) *ZoneConfig {
	qname = strings.ToLower(qname)

	// 레이블별로 순회하며 최장 매칭 검색
	off := 0
	for {
		suffix := qname[off:]
		if zc, ok := s.zones[suffix]; ok {
			return zc
		}

		// 다음 레이블로 이동 (다음 '.' 이후)
		idx := strings.Index(qname[off:], ".")
		if idx == -1 {
			break
		}
		off += idx + 1
		if off >= len(qname) {
			break
		}
	}
	return nil
}

// handleQuery는 DNS 쿼리를 처리하여 응답을 생성한다.
func (s *DNSServer) handleQuery(data []byte) ([]byte, error) {
	msg, err := ParseMessage(data)
	if err != nil {
		return nil, fmt.Errorf("메시지 파싱 실패: %w", err)
	}

	if len(msg.Questions) == 0 {
		return nil, fmt.Errorf("질문 섹션 없음")
	}

	q := msg.Questions[0]

	// 응답 헤더 구성
	response := DNSMessage{
		Header: DNSHeader{
			ID:      msg.Header.ID,
			Flags:   0x8400, // QR=1, AA=1, RD=0, RA=0
			QDCount: 1,
		},
		Questions: msg.Questions,
	}

	// 존 매칭
	zc := s.matchZone(q.Name)
	if zc == nil {
		// 존을 찾지 못함 → REFUSED
		response.Header.Flags = 0x8005 // QR=1, RCODE=REFUSED
		fmt.Printf("  [서버] 존 매칭 실패: %s → REFUSED\n", q.Name)
		return SerializeMessage(response), nil
	}

	fmt.Printf("  [서버] 존 매칭 성공: %s → zone=%s\n", q.Name, zc.Zone)

	// A 레코드 조회
	if q.Type == TypeA {
		if ip, ok := zc.Records[strings.ToLower(q.Name)]; ok {
			response.Header.ANCount = 1
			response.Answers = []DNSRecord{{
				Name:  q.Name,
				Type:  TypeA,
				Class: ClassIN,
				TTL:   300,
				Data:  ip.To4(),
			}}
			fmt.Printf("  [서버] A 레코드 응답: %s → %s\n", q.Name, ip)
		} else {
			// NXDOMAIN
			response.Header.Flags = 0x8403 // QR=1, AA=1, RCODE=NXDOMAIN
			fmt.Printf("  [서버] 레코드 없음: %s → NXDOMAIN\n", q.Name)
		}
	} else {
		// 지원하지 않는 타입
		fmt.Printf("  [서버] 미지원 타입: %d\n", q.Type)
	}

	return SerializeMessage(response), nil
}

// serveUDP는 UDP DNS 서버를 시작한다.
func (s *DNSServer) serveUDP(wg *sync.WaitGroup, ready chan<- struct{}) {
	defer wg.Done()

	addr, err := net.ResolveUDPAddr("udp", s.Addr)
	if err != nil {
		fmt.Printf("UDP 주소 해석 실패: %v\n", err)
		return
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Printf("UDP 리스닝 실패: %v\n", err)
		return
	}

	s.mu.Lock()
	s.udpConn = conn
	s.mu.Unlock()

	ready <- struct{}{}
	fmt.Printf("[UDP 서버] %s에서 수신 대기 중\n", s.Addr)

	buf := make([]byte, 512) // DNS UDP 최대 크기
	for {
		s.mu.Lock()
		stopped := s.stopped
		s.mu.Unlock()
		if stopped {
			return
		}

		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}

		fmt.Printf("[UDP 서버] %s에서 %d바이트 수신\n", remoteAddr, n)
		response, err := s.handleQuery(buf[:n])
		if err != nil {
			fmt.Printf("[UDP 서버] 처리 오류: %v\n", err)
			continue
		}

		conn.WriteToUDP(response, remoteAddr)
	}
}

// serveTCP는 TCP DNS 서버를 시작한다.
func (s *DNSServer) serveTCP(wg *sync.WaitGroup, ready chan<- struct{}) {
	defer wg.Done()

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		fmt.Printf("TCP 리스닝 실패: %v\n", err)
		return
	}

	s.mu.Lock()
	s.tcpLn = ln
	s.mu.Unlock()

	ready <- struct{}{}
	fmt.Printf("[TCP 서버] %s에서 수신 대기 중\n", s.Addr)

	for {
		s.mu.Lock()
		stopped := s.stopped
		s.mu.Unlock()
		if stopped {
			return
		}

		ln.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
		conn, err := ln.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}

		go s.handleTCPConn(conn)
	}
}

// handleTCPConn은 TCP 연결을 처리한다.
// TCP DNS는 2바이트 길이 프리픽스 + 메시지 형식이다.
func (s *DNSServer) handleTCPConn(conn net.Conn) {
	defer conn.Close()

	// 2바이트 길이 읽기
	lenBuf := make([]byte, 2)
	_, err := conn.Read(lenBuf)
	if err != nil {
		return
	}

	msgLen := binary.BigEndian.Uint16(lenBuf)
	msgBuf := make([]byte, msgLen)
	_, err = conn.Read(msgBuf)
	if err != nil {
		return
	}

	fmt.Printf("[TCP 서버] %s에서 %d바이트 수신\n", conn.RemoteAddr(), msgLen)

	response, err := s.handleQuery(msgBuf)
	if err != nil {
		fmt.Printf("[TCP 서버] 처리 오류: %v\n", err)
		return
	}

	// TCP 응답: 2바이트 길이 + 메시지
	respLen := make([]byte, 2)
	binary.BigEndian.PutUint16(respLen, uint16(len(response)))
	conn.Write(respLen)
	conn.Write(response)
}

// Stop은 서버를 종료한다.
func (s *DNSServer) Stop() {
	s.mu.Lock()
	s.stopped = true
	if s.udpConn != nil {
		s.udpConn.Close()
	}
	if s.tcpLn != nil {
		s.tcpLn.Close()
	}
	s.mu.Unlock()
}

// =============================================================================
// 내장 dig 시뮬레이터
// =============================================================================

// DigQuery는 DNS 쿼리를 전송하고 결과를 파싱한다.
func DigQuery(addr, qname string, qtype uint16, proto string) {
	fmt.Printf("\n; <<>> dig 시뮬레이터 <<>> %s %s @%s (%s)\n", qname, qtypeToString(qtype), addr, proto)

	// 쿼리 메시지 구성
	query := DNSMessage{
		Header: DNSHeader{
			ID:      uint16(rand.Intn(65535)),
			Flags:   0x0100, // RD=1 (Recursion Desired)
			QDCount: 1,
		},
		Questions: []DNSQuestion{{
			Name:  qname,
			Type:  qtype,
			Class: ClassIN,
		}},
	}

	data := SerializeMessage(query)

	var responseData []byte

	if proto == "udp" {
		conn, err := net.DialTimeout("udp", addr, 2*time.Second)
		if err != nil {
			fmt.Printf(";; 연결 실패: %v\n", err)
			return
		}
		defer conn.Close()

		conn.SetDeadline(time.Now().Add(2 * time.Second))
		conn.Write(data)

		buf := make([]byte, 512)
		n, err := conn.Read(buf)
		if err != nil {
			fmt.Printf(";; 응답 수신 실패: %v\n", err)
			return
		}
		responseData = buf[:n]
	} else {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			fmt.Printf(";; 연결 실패: %v\n", err)
			return
		}
		defer conn.Close()

		conn.SetDeadline(time.Now().Add(2 * time.Second))
		// TCP: 2바이트 길이 프리픽스
		lenBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBuf, uint16(len(data)))
		conn.Write(lenBuf)
		conn.Write(data)

		// 응답 읽기
		_, err = conn.Read(lenBuf)
		if err != nil {
			fmt.Printf(";; 응답 길이 읽기 실패: %v\n", err)
			return
		}
		respLen := binary.BigEndian.Uint16(lenBuf)
		responseData = make([]byte, respLen)
		_, err = conn.Read(responseData)
		if err != nil {
			fmt.Printf(";; 응답 데이터 읽기 실패: %v\n", err)
			return
		}
	}

	// 응답 파싱
	resp, err := ParseMessage(responseData)
	if err != nil {
		fmt.Printf(";; 응답 파싱 실패: %v\n", err)
		return
	}

	rcode := resp.Header.Flags & 0x000F
	fmt.Printf(";; 응답 헤더: id=%d, flags=0x%04x, RCODE=%s\n",
		resp.Header.ID, resp.Header.Flags, rcodeToString(int(rcode)))
	fmt.Printf(";; QUESTION: %d, ANSWER: %d, AUTHORITY: %d, ADDITIONAL: %d\n",
		resp.Header.QDCount, resp.Header.ANCount, resp.Header.NSCount, resp.Header.ARCount)

	if resp.Header.ANCount > 0 {
		fmt.Println(";; ANSWER SECTION:")
		// 응답 섹션 파싱 (질문 섹션 이후 위치)
		offset := dnsHeaderSize
		// 질문 섹션 건너뛰기
		for i := 0; i < int(resp.Header.QDCount); i++ {
			_, newOff, _ := ParseName(responseData, offset)
			offset = newOff + 4 // QTYPE(2) + QCLASS(2)
		}

		for i := 0; i < int(resp.Header.ANCount); i++ {
			name, newOff, err := ParseName(responseData, offset)
			if err != nil {
				break
			}
			offset = newOff
			if offset+10 > len(responseData) {
				break
			}

			rtype := binary.BigEndian.Uint16(responseData[offset : offset+2])
			// rclass := binary.BigEndian.Uint16(responseData[offset+2 : offset+4])
			ttl := binary.BigEndian.Uint32(responseData[offset+4 : offset+8])
			rdlen := binary.BigEndian.Uint16(responseData[offset+8 : offset+10])
			offset += 10

			if rtype == TypeA && rdlen == 4 {
				ip := net.IP(responseData[offset : offset+4])
				fmt.Printf("%s\t%d\tIN\tA\t%s\n", name, ttl, ip)
			}
			offset += int(rdlen)
		}
	}
}

func qtypeToString(qtype uint16) string {
	switch qtype {
	case TypeA:
		return "A"
	case TypeAAAA:
		return "AAAA"
	case TypeNS:
		return "NS"
	default:
		return fmt.Sprintf("TYPE%d", qtype)
	}
}

func rcodeToString(rcode int) string {
	switch rcode {
	case RcodeSuccess:
		return "NOERROR"
	case RcodeFormatError:
		return "FORMERR"
	case RcodeServerFail:
		return "SERVFAIL"
	case RcodeNameError:
		return "NXDOMAIN"
	case RcodeRefused:
		return "REFUSED"
	default:
		return fmt.Sprintf("RCODE%d", rcode)
	}
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== CoreDNS DNS 서버 시뮬레이션 ===")
	fmt.Println()

	// -------------------------------------------------------------------------
	// 1. 서버 설정
	// -------------------------------------------------------------------------
	port := 15353 + rand.Intn(1000) // 랜덤 포트 (충돌 방지)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	server := NewDNSServer(addr)

	// 존 1: example.com
	server.AddZone("example.com.", map[string]net.IP{
		"example.com.":      net.ParseIP("93.184.216.34"),
		"www.example.com.":  net.ParseIP("93.184.216.34"),
		"mail.example.com.": net.ParseIP("93.184.216.35"),
	})

	// 존 2: internal.example.com (더 긴 매칭 우선)
	server.AddZone("internal.example.com.", map[string]net.IP{
		"app.internal.example.com.": net.ParseIP("10.0.1.1"),
		"db.internal.example.com.":  net.ParseIP("10.0.1.2"),
	})

	// 존 3: test.org
	server.AddZone("test.org.", map[string]net.IP{
		"test.org.":     net.ParseIP("203.0.113.1"),
		"api.test.org.": net.ParseIP("203.0.113.2"),
	})

	// -------------------------------------------------------------------------
	// 2. 서버 시작 (UDP + TCP)
	// -------------------------------------------------------------------------
	var wg sync.WaitGroup
	udpReady := make(chan struct{}, 1)
	tcpReady := make(chan struct{}, 1)

	wg.Add(2)
	go server.serveUDP(&wg, udpReady)
	go server.serveTCP(&wg, tcpReady)

	<-udpReady
	<-tcpReady
	fmt.Println()

	// -------------------------------------------------------------------------
	// 3. dig 시뮬레이터로 쿼리 테스트
	// -------------------------------------------------------------------------

	// 테스트 1: UDP로 A 레코드 쿼리
	fmt.Println("=== 테스트 1: example.com A 레코드 (UDP) ===")
	DigQuery(addr, "example.com.", TypeA, "udp")

	// 테스트 2: TCP로 A 레코드 쿼리
	fmt.Println("\n=== 테스트 2: www.example.com A 레코드 (TCP) ===")
	DigQuery(addr, "www.example.com.", TypeA, "tcp")

	// 테스트 3: 존 라우팅 - internal.example.com (최장 매칭)
	fmt.Println("\n=== 테스트 3: 존 라우팅 - internal.example.com (최장 매칭) ===")
	DigQuery(addr, "app.internal.example.com.", TypeA, "udp")

	// 테스트 4: 다른 존 쿼리
	fmt.Println("\n=== 테스트 4: test.org 쿼리 ===")
	DigQuery(addr, "api.test.org.", TypeA, "udp")

	// 테스트 5: 존재하지 않는 레코드 (NXDOMAIN)
	fmt.Println("\n=== 테스트 5: 존재하지 않는 레코드 (NXDOMAIN) ===")
	DigQuery(addr, "nonexist.example.com.", TypeA, "udp")

	// 테스트 6: 존재하지 않는 존 (REFUSED)
	fmt.Println("\n=== 테스트 6: 존재하지 않는 존 (REFUSED) ===")
	DigQuery(addr, "unknown.org.", TypeA, "udp")

	// -------------------------------------------------------------------------
	// 4. 바이너리 파싱 데모
	// -------------------------------------------------------------------------
	fmt.Println("\n=== DNS 메시지 바이너리 구조 ===")
	demoMsg := DNSMessage{
		Header: DNSHeader{
			ID:      0x1234,
			Flags:   0x0100,
			QDCount: 1,
		},
		Questions: []DNSQuestion{{
			Name:  "example.com.",
			Type:  TypeA,
			Class: ClassIN,
		}},
	}
	serialized := SerializeMessage(demoMsg)
	fmt.Printf("직렬화된 DNS 쿼리 (%d바이트):\n", len(serialized))
	for i, b := range serialized {
		if i > 0 && i%16 == 0 {
			fmt.Println()
		}
		fmt.Printf("%02x ", b)
	}
	fmt.Println()

	// 재파싱 검증
	parsed, err := ParseMessage(serialized)
	if err != nil {
		fmt.Printf("파싱 오류: %v\n", err)
	} else {
		fmt.Printf("파싱 결과: ID=0x%04x, QD=%d, 이름=%s, 타입=%s\n",
			parsed.Header.ID, parsed.Header.QDCount,
			parsed.Questions[0].Name, qtypeToString(parsed.Questions[0].Type))
	}

	// -------------------------------------------------------------------------
	// 5. 서버 종료
	// -------------------------------------------------------------------------
	fmt.Println("\n서버 종료 중...")
	server.Stop()
	wg.Wait()
	fmt.Println("완료.")
}
