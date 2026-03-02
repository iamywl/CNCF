// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble 바이너리 패킷 파싱 패턴
//
// Hubble Parser는 eBPF가 수집한 raw 바이트를 구조화된 Flow로 변환합니다:
//   - MonitorEvent → PerfEvent (raw bytes) → 구조화된 Flow
//   - L2(Ethernet) → L3(IP) → L4(TCP/UDP) 레이어별 파싱
//   - 메시지 타입에 따라 다른 Parser로 디스패치
//
// 이 PoC는 바이너리 데이터 파싱 패턴을 시뮬레이션합니다.
//
// 실행: go run main.go

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

// ========================================
// 1. 메시지 타입 상수 (Hubble의 monitorAPI)
// ========================================

// 실제 Hubble: pkg/monitor/api/types.go
const (
	MessageTypeDrop    byte = 0x00 // 패킷 드롭 이벤트
	MessageTypeTrace   byte = 0x01 // 패킷 트레이스 이벤트
	MessageTypeDebug   byte = 0x04 // 디버그 메시지
	MessageTypeCapture byte = 0x05 // 패킷 캡처
)

// 이더넷 타입
const (
	EtherTypeIPv4 uint16 = 0x0800
	EtherTypeIPv6 uint16 = 0x86DD
	EtherTypeARP  uint16 = 0x0806
)

// IP 프로토콜
const (
	ProtoTCP  byte = 6
	ProtoUDP  byte = 17
	ProtoICMP byte = 1
)

func messageTypeName(t byte) string {
	switch t {
	case MessageTypeDrop:
		return "DROP"
	case MessageTypeTrace:
		return "TRACE"
	case MessageTypeDebug:
		return "DEBUG"
	case MessageTypeCapture:
		return "CAPTURE"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", t)
	}
}

func protoName(p byte) string {
	switch p {
	case ProtoTCP:
		return "TCP"
	case ProtoUDP:
		return "UDP"
	case ProtoICMP:
		return "ICMP"
	default:
		return fmt.Sprintf("Proto(%d)", p)
	}
}

// ========================================
// 2. 파싱 결과 구조체
// ========================================

type EthernetHeader struct {
	DstMAC    net.HardwareAddr
	SrcMAC    net.HardwareAddr
	EtherType uint16
}

type IPv4Header struct {
	Version  byte
	IHL      byte // Header Length (in 32-bit words)
	TTL      byte
	Protocol byte
	SrcIP    net.IP
	DstIP    net.IP
}

type TCPHeader struct {
	SrcPort uint16
	DstPort uint16
	SeqNum  uint32
	Flags   byte // SYN, ACK, FIN 등
}

type UDPHeader struct {
	SrcPort uint16
	DstPort uint16
	Length  uint16
}

type ParsedPacket struct {
	MessageType byte
	Ethernet    *EthernetHeader
	IPv4        *IPv4Header
	TCP         *TCPHeader
	UDP         *UDPHeader
}

// ========================================
// 3. Decoder 인터페이스 (Hubble의 Parser 디스패치 패턴)
// ========================================

// Decoder는 raw 바이트를 구조화된 데이터로 변환합니다.
// 실제 Hubble: type Decoder interface { Decode(monitorEvent) (*v1.Event, error) }
type Decoder interface {
	Decode(data []byte) (*ParsedPacket, error)
	Name() string
}

// L34Parser는 L3/L4 프로토콜을 파싱합니다.
// 실제 Hubble: pkg/hubble/parser/threefour/parser.go
type L34Parser struct{}

func (p *L34Parser) Name() string { return "L3/L4 Parser" }

func (p *L34Parser) Decode(data []byte) (*ParsedPacket, error) {
	pkt := &ParsedPacket{}

	// 최소 크기 검증
	if len(data) < 1 {
		return nil, fmt.Errorf("data too short")
	}

	// 1. 메시지 타입 (첫 바이트)
	pkt.MessageType = data[0]
	offset := 1

	// 2. Ethernet 파싱 (14 bytes)
	if len(data) < offset+14 {
		return pkt, nil
	}
	pkt.Ethernet = &EthernetHeader{
		DstMAC:    net.HardwareAddr(data[offset : offset+6]),
		SrcMAC:    net.HardwareAddr(data[offset+6 : offset+12]),
		EtherType: binary.BigEndian.Uint16(data[offset+12 : offset+14]),
	}
	offset += 14

	// 3. IPv4 파싱 (최소 20 bytes)
	if pkt.Ethernet.EtherType != EtherTypeIPv4 || len(data) < offset+20 {
		return pkt, nil
	}

	versionIHL := data[offset]
	pkt.IPv4 = &IPv4Header{
		Version:  (versionIHL >> 4) & 0x0F,
		IHL:      versionIHL & 0x0F,
		TTL:      data[offset+8],
		Protocol: data[offset+9],
		SrcIP:    net.IP(data[offset+12 : offset+16]),
		DstIP:    net.IP(data[offset+16 : offset+20]),
	}
	offset += int(pkt.IPv4.IHL) * 4

	// 4. TCP 파싱 (최소 20 bytes)
	if pkt.IPv4.Protocol == ProtoTCP && len(data) >= offset+20 {
		pkt.TCP = &TCPHeader{
			SrcPort: binary.BigEndian.Uint16(data[offset : offset+2]),
			DstPort: binary.BigEndian.Uint16(data[offset+2 : offset+4]),
			SeqNum:  binary.BigEndian.Uint32(data[offset+4 : offset+8]),
			Flags:   data[offset+13],
		}
	}

	// 5. UDP 파싱 (8 bytes)
	if pkt.IPv4.Protocol == ProtoUDP && len(data) >= offset+8 {
		pkt.UDP = &UDPHeader{
			SrcPort: binary.BigEndian.Uint16(data[offset : offset+2]),
			DstPort: binary.BigEndian.Uint16(data[offset+2 : offset+4]),
			Length:  binary.BigEndian.Uint16(data[offset+4 : offset+6]),
		}
	}

	return pkt, nil
}

// DebugParser는 디버그 메시지를 처리합니다.
type DebugParser struct{}

func (p *DebugParser) Name() string { return "Debug Parser" }

func (p *DebugParser) Decode(data []byte) (*ParsedPacket, error) {
	return &ParsedPacket{MessageType: MessageTypeDebug}, nil
}

// ========================================
// 4. Parser Dispatcher (Hubble의 parser.go 패턴)
// ========================================

// ParserDispatcher는 메시지 타입에 따라 적절한 파서로 디스패치합니다.
// 실제 Hubble: pkg/hubble/parser/parser.go
type ParserDispatcher struct {
	l34 *L34Parser
	dbg *DebugParser
}

func NewParserDispatcher() *ParserDispatcher {
	return &ParserDispatcher{
		l34: &L34Parser{},
		dbg: &DebugParser{},
	}
}

func (p *ParserDispatcher) Decode(data []byte) (*ParsedPacket, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}

	msgType := data[0]

	switch msgType {
	case MessageTypeDrop, MessageTypeTrace, MessageTypeCapture:
		fmt.Printf("    → %s 파서로 디스패치\n", p.l34.Name())
		return p.l34.Decode(data)
	case MessageTypeDebug:
		fmt.Printf("    → %s 파서로 디스패치\n", p.dbg.Name())
		return p.dbg.Decode(data)
	default:
		return nil, fmt.Errorf("unknown message type: 0x%02x", msgType)
	}
}

// ========================================
// 5. 패킷 빌더 (테스트용)
// ========================================

func buildPacket(msgType byte, srcMAC, dstMAC [6]byte, srcIP, dstIP [4]byte, proto byte, srcPort, dstPort uint16) []byte {
	var pkt []byte

	// Message type (1 byte)
	pkt = append(pkt, msgType)

	// Ethernet (14 bytes)
	pkt = append(pkt, dstMAC[:]...)
	pkt = append(pkt, srcMAC[:]...)
	etherType := make([]byte, 2)
	binary.BigEndian.PutUint16(etherType, EtherTypeIPv4)
	pkt = append(pkt, etherType...)

	// IPv4 (20 bytes)
	ipHeader := make([]byte, 20)
	ipHeader[0] = 0x45 // Version=4, IHL=5
	ipHeader[8] = 64   // TTL
	ipHeader[9] = proto
	copy(ipHeader[12:16], srcIP[:])
	copy(ipHeader[16:20], dstIP[:])
	pkt = append(pkt, ipHeader...)

	// TCP (20 bytes) or UDP (8 bytes)
	if proto == ProtoTCP {
		tcpHeader := make([]byte, 20)
		binary.BigEndian.PutUint16(tcpHeader[0:2], srcPort)
		binary.BigEndian.PutUint16(tcpHeader[2:4], dstPort)
		binary.BigEndian.PutUint32(tcpHeader[4:8], 12345) // SeqNum
		tcpHeader[13] = 0x02                               // SYN flag
		pkt = append(pkt, tcpHeader...)
	} else if proto == ProtoUDP {
		udpHeader := make([]byte, 8)
		binary.BigEndian.PutUint16(udpHeader[0:2], srcPort)
		binary.BigEndian.PutUint16(udpHeader[2:4], dstPort)
		binary.BigEndian.PutUint16(udpHeader[4:6], 42) // Length
		pkt = append(pkt, udpHeader...)
	}

	return pkt
}

// ========================================
// 6. 출력 헬퍼
// ========================================

func printParsedPacket(pkt *ParsedPacket) {
	fmt.Printf("    메시지 타입: %s\n", messageTypeName(pkt.MessageType))

	if pkt.Ethernet != nil {
		fmt.Printf("    L2 Ethernet: %s → %s (Type: 0x%04X)\n",
			pkt.Ethernet.SrcMAC, pkt.Ethernet.DstMAC, pkt.Ethernet.EtherType)
	}

	if pkt.IPv4 != nil {
		fmt.Printf("    L3 IPv4:     %s → %s (TTL=%d, Proto=%s)\n",
			pkt.IPv4.SrcIP, pkt.IPv4.DstIP, pkt.IPv4.TTL, protoName(pkt.IPv4.Protocol))
	}

	if pkt.TCP != nil {
		flags := []string{}
		if pkt.TCP.Flags&0x02 != 0 {
			flags = append(flags, "SYN")
		}
		if pkt.TCP.Flags&0x10 != 0 {
			flags = append(flags, "ACK")
		}
		if pkt.TCP.Flags&0x01 != 0 {
			flags = append(flags, "FIN")
		}
		fmt.Printf("    L4 TCP:      :%d → :%d (Seq=%d, Flags=[%s])\n",
			pkt.TCP.SrcPort, pkt.TCP.DstPort, pkt.TCP.SeqNum, strings.Join(flags, ","))
	}

	if pkt.UDP != nil {
		fmt.Printf("    L4 UDP:      :%d → :%d (Len=%d)\n",
			pkt.UDP.SrcPort, pkt.UDP.DstPort, pkt.UDP.Length)
	}
}

// ========================================
// 7. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble 바이너리 패킷 파싱 패턴 ===")
	fmt.Println()
	fmt.Println("eBPF가 수집한 raw 바이트를 구조화된 Flow로 변환하는 과정:")
	fmt.Println("  raw bytes → [MsgType] → [Ethernet] → [IPv4] → [TCP/UDP]")
	fmt.Println()
	fmt.Println("바이트 오프셋:")
	fmt.Println("  [0]      : 메시지 타입 (DROP=0x00, TRACE=0x01, DEBUG=0x04)")
	fmt.Println("  [1..14]  : Ethernet (DstMAC 6B + SrcMAC 6B + EtherType 2B)")
	fmt.Println("  [15..34] : IPv4 (Version/IHL + TTL + Protocol + SrcIP + DstIP)")
	fmt.Println("  [35+]    : TCP(20B) 또는 UDP(8B)")
	fmt.Println()

	parser := NewParserDispatcher()

	srcMAC := [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x01}
	dstMAC := [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x02}

	// ── 시나리오 1: TCP TRACE 이벤트 ──
	fmt.Println("━━━ 시나리오 1: TCP TRACE 이벤트 ━━━")
	fmt.Println()
	data1 := buildPacket(MessageTypeTrace, srcMAC, dstMAC,
		[4]byte{10, 244, 0, 5}, [4]byte{10, 244, 0, 10},
		ProtoTCP, 45678, 8080)

	fmt.Printf("  Raw 데이터 (%d bytes):\n", len(data1))
	fmt.Printf("    %s\n", formatHexDump(data1))
	fmt.Println()

	pkt1, _ := parser.Decode(data1)
	printParsedPacket(pkt1)
	fmt.Println()

	// ── 시나리오 2: UDP DROP 이벤트 ──
	fmt.Println("━━━ 시나리오 2: UDP DROP 이벤트 (DNS) ━━━")
	fmt.Println()
	data2 := buildPacket(MessageTypeDrop, srcMAC, dstMAC,
		[4]byte{10, 244, 1, 100}, [4]byte{10, 96, 0, 10},
		ProtoUDP, 52345, 53)

	fmt.Printf("  Raw 데이터 (%d bytes):\n", len(data2))
	fmt.Printf("    %s\n", formatHexDump(data2))
	fmt.Println()

	pkt2, _ := parser.Decode(data2)
	printParsedPacket(pkt2)
	fmt.Println()

	// ── 시나리오 3: DEBUG 메시지 ──
	fmt.Println("━━━ 시나리오 3: DEBUG 메시지 ━━━")
	fmt.Println()
	data3 := []byte{MessageTypeDebug, 0x01, 0x02, 0x03}

	fmt.Printf("  Raw 데이터 (%d bytes):\n", len(data3))
	fmt.Printf("    %s\n", formatHexDump(data3))
	fmt.Println()

	pkt3, _ := parser.Decode(data3)
	printParsedPacket(pkt3)
	fmt.Println()

	// ── 바이트 오프셋 시각화 ──
	fmt.Println("━━━ 바이트 오프셋 시각화 (TCP TRACE 패킷) ━━━")
	fmt.Println()
	visualizeOffsets(data1)

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 첫 바이트로 메시지 타입 결정 → 적절한 파서로 디스패치")
	fmt.Println("  - 레이어별 순차 파싱: Ethernet → IPv4 → TCP/UDP")
	fmt.Println("  - Big-Endian 바이트 순서 (네트워크 바이트 오더)")
	fmt.Println("  - 실제 Hubble: gopacket.DecodingLayerParser로 제로카피 파싱")
	fmt.Println("  - IHL 필드로 IP 헤더 가변 길이 처리")
}

func formatHexDump(data []byte) string {
	var parts []string
	for _, b := range data {
		parts = append(parts, fmt.Sprintf("%02X", b))
	}

	// 16바이트씩 줄바꿈
	var lines []string
	for i := 0; i < len(parts); i += 16 {
		end := i + 16
		if end > len(parts) {
			end = len(parts)
		}
		lines = append(lines, strings.Join(parts[i:end], " "))
	}
	return strings.Join(lines, "\n    ")
}

func visualizeOffsets(data []byte) {
	sections := []struct {
		name  string
		start int
		end   int
	}{
		{"MsgType", 0, 1},
		{"Dst MAC", 1, 7},
		{"Src MAC", 7, 13},
		{"EthType", 13, 15},
		{"IP Hdr", 15, 35},
		{"TCP Hdr", 35, 55},
	}

	for _, s := range sections {
		if s.start >= len(data) {
			break
		}
		end := s.end
		if end > len(data) {
			end = len(data)
		}
		var hexBytes []string
		for _, b := range data[s.start:end] {
			hexBytes = append(hexBytes, fmt.Sprintf("%02X", b))
		}
		fmt.Printf("  [%2d..%2d] %-8s: %s\n", s.start, end-1, s.name, strings.Join(hexBytes, " "))
	}
}
