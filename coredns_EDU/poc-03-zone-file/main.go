// poc-03-zone-file: CoreDNS Zone 파일 파서 시뮬레이션
//
// CoreDNS file 플러그인의 Zone 파일 파싱/검색을 재현한다:
//   - RFC 1035 형식 Zone 파일 파싱 (A, AAAA, CNAME, MX, NS, SOA)
//   - Zone 데이터 구조 (맵 기반 트리)
//   - Lookup(name, qtype): 레코드 검색
//   - 와일드카드 매칭 (*.example.com)
//
// 참조:
//   - plugin/file/file.go: File 플러그인, Parse 함수
//   - plugin/file/lookup.go: Zone.Lookup 메서드
//   - plugin/file/tree/tree.go: LLRB 트리 기반 레코드 저장
//
// 사용법: go run main.go

package main

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// =============================================================================
// DNS 레코드 타입
// =============================================================================

const (
	TypeA     = "A"
	TypeAAAA  = "AAAA"
	TypeCNAME = "CNAME"
	TypeMX    = "MX"
	TypeNS    = "NS"
	TypeSOA   = "SOA"
	TypeTXT   = "TXT"
)

// =============================================================================
// 레코드 구조체
// =============================================================================

// RR은 DNS Resource Record를 나타낸다.
type RR struct {
	Name  string // 소유자 이름 (FQDN)
	TTL   uint32 // Time To Live
	Class string // 보통 "IN"
	Type  string // A, AAAA, CNAME, MX, NS, SOA, TXT
	Data  string // 레코드 데이터
}

func (r RR) String() string {
	return fmt.Sprintf("%-30s %d\t%s\t%s\t%s", r.Name, r.TTL, r.Class, r.Type, r.Data)
}

// SOARecord는 SOA 레코드의 구조화된 데이터이다.
type SOARecord struct {
	MName   string // 주 네임서버
	RName   string // 관리자 이메일
	Serial  uint32
	Refresh uint32
	Retry   uint32
	Expire  uint32
	Minimum uint32
}

// MXRecord는 MX 레코드의 구조화된 데이터이다.
type MXRecord struct {
	Preference uint16
	Exchange   string
}

// =============================================================================
// Zone 구조체 (plugin/file/file.go, plugin/file/tree 재현)
// =============================================================================

// Zone은 DNS 존의 모든 레코드를 저장한다.
// CoreDNS에서는 LLRB 트리(plugin/file/tree/tree.go)를 사용하지만,
// 여기서는 맵 기반으로 단순화한다.
type Zone struct {
	Origin  string              // 존 원점 (예: "example.com.")
	SOA     *RR                 // SOA 레코드
	records map[string][]RR     // 이름 → 레코드 목록
	// CoreDNS의 Zone.Apex와 유사: SOA, NS 등 존 정점 레코드
}

// NewZone은 새 Zone을 생성한다.
func NewZone(origin string) *Zone {
	return &Zone{
		Origin:  ensureFQDN(origin),
		records: make(map[string][]RR),
	}
}

// Insert는 레코드를 존에 추가한다.
// CoreDNS plugin/file/file.go:165의 z.Insert(rr) 재현.
func (z *Zone) Insert(rr RR) {
	rr.Name = ensureFQDN(rr.Name)
	key := strings.ToLower(rr.Name)

	if rr.Type == TypeSOA {
		z.SOA = &rr
	}

	z.records[key] = append(z.records[key], rr)
}

// Lookup은 이름과 타입으로 레코드를 검색한다.
// CoreDNS plugin/file/lookup.go:33의 Zone.Lookup 로직을 단순화하여 재현.
//
// 검색 순서:
// 1. 정확한 이름 매칭
// 2. 와일드카드 매칭 (*.zone)
// 3. CNAME 추적
func (z *Zone) Lookup(name, qtype string) ([]RR, LookupResult) {
	name = ensureFQDN(strings.ToLower(name))

	// 1. 정확한 이름으로 레코드 검색
	if records, ok := z.records[name]; ok {
		// 요청 타입과 일치하는 레코드 필터링
		var matched []RR
		var cnames []RR
		for _, rr := range records {
			if rr.Type == qtype {
				matched = append(matched, rr)
			}
			if rr.Type == TypeCNAME && qtype != TypeCNAME {
				cnames = append(cnames, rr)
			}
		}

		if len(matched) > 0 {
			return matched, Success
		}

		// CNAME 체이싱: A 쿼리인데 CNAME만 있으면 CNAME 대상을 추적
		// CoreDNS lookup.go에서도 CNAME을 만나면 대상을 재귀 조회한다.
		if len(cnames) > 0 && qtype != TypeCNAME {
			result := cnames
			target := ensureFQDN(strings.ToLower(cnames[0].Data))
			targetRecords, lookupResult := z.Lookup(target, qtype)
			if lookupResult == Success {
				result = append(result, targetRecords...)
			}
			return result, Success
		}

		// 이름은 존재하지만 요청 타입이 없으면 NODATA
		return nil, NoData
	}

	// 2. 와일드카드 매칭
	// CoreDNS lookup.go:80: 와일드카드 레이블 검색
	// "app.example.com." → "*.example.com." 시도
	wildcardName := replaceFirstLabelWithWildcard(name, z.Origin)
	if wildcardName != "" {
		if records, ok := z.records[wildcardName]; ok {
			var matched []RR
			for _, rr := range records {
				if rr.Type == qtype {
					// 와일드카드 매칭 시 실제 이름으로 치환하여 반환
					synth := rr
					synth.Name = name
					matched = append(matched, synth)
				}
			}
			if len(matched) > 0 {
				return matched, Success
			}
			return nil, NoData
		}
	}

	// 3. 이름이 존재하지 않으면 NXDOMAIN
	return nil, NameError
}

// LookupResult는 조회 결과를 나타낸다 (plugin/file/lookup.go:16 재현).
type LookupResult int

const (
	Success     LookupResult = iota // 성공
	NameError                       // NXDOMAIN
	NoData                          // 이름 존재, 타입 없음
	ServerError                     // 서버 오류
)

func (r LookupResult) String() string {
	switch r {
	case Success:
		return "NOERROR"
	case NameError:
		return "NXDOMAIN"
	case NoData:
		return "NODATA"
	case ServerError:
		return "SERVFAIL"
	default:
		return "UNKNOWN"
	}
}

// =============================================================================
// Zone 파일 파서 (RFC 1035 형식)
// =============================================================================

// ParseZoneFile은 RFC 1035 형식의 Zone 파일을 파싱한다.
// CoreDNS plugin/file/file.go:148의 Parse 함수를 단순화하여 재현.
//
// 지원 지시문: $ORIGIN, $TTL
// 지원 레코드: SOA, NS, A, AAAA, CNAME, MX, TXT
func ParseZoneFile(content string, origin string) (*Zone, error) {
	zone := NewZone(origin)
	defaultTTL := uint32(3600) // 기본 TTL
	currentOrigin := ensureFQDN(origin)
	lastName := currentOrigin

	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// 주석 제거
		if idx := strings.Index(line, ";"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// $ORIGIN 지시문
		if strings.HasPrefix(line, "$ORIGIN") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				currentOrigin = ensureFQDN(parts[1])
			}
			continue
		}

		// $TTL 지시문
		if strings.HasPrefix(line, "$TTL") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				if ttl, err := strconv.ParseUint(parts[1], 10, 32); err == nil {
					defaultTTL = uint32(ttl)
				}
			}
			continue
		}

		// 레코드 파싱
		rr, err := parseRRLine(line, lastName, currentOrigin, defaultTTL)
		if err != nil {
			fmt.Printf("  [파서] 경고: 라인 %d 파싱 실패: %v (%s)\n", lineNum, err, line)
			continue
		}

		lastName = rr.Name
		zone.Insert(rr)
	}

	// SOA 레코드 확인 (CoreDNS file.go:172: 필수)
	if zone.SOA == nil {
		return nil, fmt.Errorf("Zone %s에 SOA 레코드가 없습니다", origin)
	}

	return zone, nil
}

// parseRRLine은 한 줄의 레코드를 파싱한다.
func parseRRLine(line, lastName, origin string, defaultTTL uint32) (RR, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return RR{}, fmt.Errorf("필드 부족: %d", len(fields))
	}

	rr := RR{
		TTL:   defaultTTL,
		Class: "IN",
	}

	idx := 0

	// 이름 필드 판단: 공백으로 시작하면 이전 이름 사용 (@ 또는 빈칸)
	if fields[0] == "@" {
		rr.Name = origin
		idx++
	} else if isRecordType(fields[0]) || isNumber(fields[0]) || fields[0] == "IN" {
		// 이름 생략 (이전 이름 사용)
		rr.Name = lastName
	} else {
		name := fields[0]
		if !strings.HasSuffix(name, ".") {
			name = name + "." + origin
		}
		rr.Name = ensureFQDN(name)
		idx++
	}

	// TTL과 Class 파싱 (순서 유연)
	for idx < len(fields) {
		if isNumber(fields[idx]) {
			ttl, _ := strconv.ParseUint(fields[idx], 10, 32)
			rr.TTL = uint32(ttl)
			idx++
		} else if strings.ToUpper(fields[idx]) == "IN" {
			rr.Class = "IN"
			idx++
		} else {
			break
		}
	}

	if idx >= len(fields) {
		return RR{}, fmt.Errorf("타입 필드 없음")
	}

	// 타입
	rr.Type = strings.ToUpper(fields[idx])
	idx++

	if idx >= len(fields) {
		return RR{}, fmt.Errorf("데이터 필드 없음")
	}

	// 데이터
	switch rr.Type {
	case TypeSOA:
		// SOA: mname rname serial refresh retry expire minimum
		if idx+6 >= len(fields) {
			// SOA가 여러 줄에 걸쳐 있을 수 있으나, 여기서는 한 줄 가정
			// 데이터를 나머지 필드로 결합
			rr.Data = strings.Join(fields[idx:], " ")
		} else {
			rr.Data = strings.Join(fields[idx:idx+7], " ")
		}
	case TypeMX:
		// MX: preference exchange
		if idx+1 >= len(fields) {
			return RR{}, fmt.Errorf("MX 데이터 부족")
		}
		exchange := fields[idx+1]
		if !strings.HasSuffix(exchange, ".") {
			exchange = exchange + "." + origin
		}
		rr.Data = fields[idx] + " " + ensureFQDN(exchange)
	case TypeCNAME, TypeNS:
		target := fields[idx]
		if !strings.HasSuffix(target, ".") {
			target = target + "." + origin
		}
		rr.Data = ensureFQDN(target)
	case TypeTXT:
		// TXT: 따옴표 포함된 문자열
		rr.Data = strings.Join(fields[idx:], " ")
	default:
		rr.Data = fields[idx]
	}

	return rr, nil
}

// =============================================================================
// 유틸리티 함수
// =============================================================================

func ensureFQDN(name string) string {
	if !strings.HasSuffix(name, ".") {
		return name + "."
	}
	return name
}

func isRecordType(s string) bool {
	types := map[string]bool{
		"A": true, "AAAA": true, "CNAME": true, "MX": true,
		"NS": true, "SOA": true, "TXT": true, "SRV": true, "PTR": true,
	}
	return types[strings.ToUpper(s)]
}

func isNumber(s string) bool {
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}

// replaceFirstLabelWithWildcard는 이름의 첫 레이블을 *로 치환한다.
// "app.example.com." → "*.example.com."
func replaceFirstLabelWithWildcard(name, origin string) string {
	if name == origin {
		return ""
	}
	idx := strings.Index(name, ".")
	if idx == -1 {
		return ""
	}
	wildcard := "*" + name[idx:]
	return wildcard
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== CoreDNS Zone 파일 파서 시뮬레이션 ===")
	fmt.Println()

	// -------------------------------------------------------------------------
	// 1. Zone 파일 정의 (RFC 1035 형식)
	// -------------------------------------------------------------------------
	zoneFileContent := `
$ORIGIN example.com.
$TTL 3600

; SOA 레코드
@   IN  SOA  ns1.example.com. admin.example.com. 2024010101 7200 3600 1209600 86400

; NS 레코드
@       IN  NS   ns1.example.com.
@       IN  NS   ns2.example.com.

; A 레코드
@       IN  A    93.184.216.34
ns1     IN  A    93.184.216.1
ns2     IN  A    93.184.216.2
www     IN  A    93.184.216.34
mail    IN  A    93.184.216.35
ftp     IN  A    93.184.216.36

; AAAA 레코드
@       IN  AAAA 2606:2800:220:1:248:1893:25c8:1946
www     IN  AAAA 2606:2800:220:1:248:1893:25c8:1946

; CNAME 레코드
blog    IN  CNAME www.example.com.
shop    IN  CNAME www.example.com.
cdn     IN  CNAME cdn-provider.example.net.

; MX 레코드
@       IN  MX   10 mail.example.com.
@       IN  MX   20 mail2.example.com.

; TXT 레코드
@       IN  TXT  "v=spf1 include:_spf.example.com ~all"

; 와일드카드 레코드
*.dev   IN  A    10.0.0.1
*       IN  A    93.184.216.99
`

	// -------------------------------------------------------------------------
	// 2. Zone 파일 파싱
	// -------------------------------------------------------------------------
	fmt.Println("--- Zone 파일 파싱 ---")
	zone, err := ParseZoneFile(zoneFileContent, "example.com")
	if err != nil {
		fmt.Printf("파싱 실패: %v\n", err)
		return
	}
	fmt.Printf("존 원점: %s\n", zone.Origin)
	fmt.Printf("SOA: %s\n", zone.SOA)
	fmt.Printf("총 레코드 수: ")
	totalRecords := 0
	for _, rrs := range zone.records {
		totalRecords += len(rrs)
	}
	fmt.Printf("%d개\n", totalRecords)
	fmt.Println()

	// -------------------------------------------------------------------------
	// 3. 모든 레코드 출력
	// -------------------------------------------------------------------------
	fmt.Println("--- 전체 레코드 목록 ---")
	for _, rrs := range zone.records {
		for _, rr := range rrs {
			fmt.Printf("  %s\n", rr)
		}
	}
	fmt.Println()

	// -------------------------------------------------------------------------
	// 4. Lookup 테스트
	// -------------------------------------------------------------------------
	testCases := []struct {
		name  string
		qtype string
		desc  string
	}{
		// 정확한 A 레코드
		{"example.com.", TypeA, "존 정점 A 레코드"},
		{"www.example.com.", TypeA, "www A 레코드"},
		{"mail.example.com.", TypeA, "mail A 레코드"},

		// AAAA 레코드
		{"example.com.", TypeAAAA, "존 정점 AAAA 레코드"},
		{"www.example.com.", TypeAAAA, "www AAAA 레코드"},

		// NS 레코드
		{"example.com.", TypeNS, "NS 레코드"},

		// SOA 레코드
		{"example.com.", TypeSOA, "SOA 레코드"},

		// MX 레코드
		{"example.com.", TypeMX, "MX 레코드"},

		// TXT 레코드
		{"example.com.", TypeTXT, "TXT 레코드"},

		// CNAME 체이싱
		{"blog.example.com.", TypeA, "CNAME → A 체이싱 (blog → www)"},
		{"shop.example.com.", TypeCNAME, "CNAME 직접 조회"},
		{"cdn.example.com.", TypeA, "외부 CNAME (체이싱 실패 예상)"},

		// 와일드카드 매칭
		{"anything.example.com.", TypeA, "와일드카드 *.example.com 매칭"},
		{"random.example.com.", TypeA, "와일드카드 *.example.com 매칭 (다른 이름)"},
		{"test.dev.example.com.", TypeA, "와일드카드 *.dev.example.com 매칭"},

		// NXDOMAIN
		{"nonexist.other.com.", TypeA, "존 외부 이름 (NXDOMAIN)"},

		// NODATA (이름 있지만 타입 없음)
		{"ftp.example.com.", TypeAAAA, "ftp AAAA (NODATA - A만 존재)"},
	}

	fmt.Println("--- Lookup 테스트 ---")
	for _, tc := range testCases {
		fmt.Printf("\n쿼리: %s %s (%s)\n", tc.name, tc.qtype, tc.desc)
		records, result := zone.Lookup(tc.name, tc.qtype)

		fmt.Printf("  결과: %s\n", result)
		if len(records) > 0 {
			for _, rr := range records {
				fmt.Printf("  %s\n", rr)
			}
		} else {
			fmt.Println("  (레코드 없음)")
		}
	}

	// -------------------------------------------------------------------------
	// 5. 와일드카드 매칭 로직 상세 설명
	// -------------------------------------------------------------------------
	fmt.Println("\n--- 와일드카드 매칭 로직 ---")
	fmt.Println("CoreDNS lookup.go의 와일드카드 처리:")
	fmt.Println("  1. 정확한 이름으로 먼저 검색")
	fmt.Println("  2. 찾지 못하면 첫 레이블을 *로 치환하여 검색")
	fmt.Println("  3. 와일드카드 매칭 시 응답의 이름을 원래 쿼리 이름으로 치환")
	fmt.Println()

	// 와일드카드 치환 예시
	names := []string{"test.example.com.", "app.dev.example.com.", "example.com."}
	for _, name := range names {
		wc := replaceFirstLabelWithWildcard(name, "example.com.")
		if wc != "" {
			fmt.Printf("  %s → %s\n", name, wc)
		} else {
			fmt.Printf("  %s → (와일드카드 불가 - 존 정점)\n", name)
		}
	}

	// -------------------------------------------------------------------------
	// 6. IP 검증
	// -------------------------------------------------------------------------
	fmt.Println("\n--- IP 주소 검증 ---")
	ips := []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "invalid"}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip != nil {
			if ip.To4() != nil {
				fmt.Printf("  %s → IPv4\n", ipStr)
			} else {
				fmt.Printf("  %s → IPv6\n", ipStr)
			}
		} else {
			fmt.Printf("  %s → 유효하지 않은 IP\n", ipStr)
		}
	}

	fmt.Println("\n완료.")
}
