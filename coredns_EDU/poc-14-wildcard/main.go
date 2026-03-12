package main

import (
	"fmt"
	"strings"
)

// ============================================================================
// CoreDNS DNS 와일드카드 매칭 PoC
// ============================================================================
//
// CoreDNS file 플러그인의 와일드카드 DNS 레코드 처리를 시뮬레이션한다.
//
// 실제 CoreDNS 구현 참조:
//   - plugin/file/wildcard.go  → replaceWithAsteriskLabel() 함수
//   - plugin/file/lookup.go    → 와일드카드 조회 로직
//   - plugin/file/tree/elem.go → 트리 기반 레코드 검색
//
// RFC 4592 (와일드카드 DNS 레코드) 핵심 규칙:
//   1. 정확 매칭(exact match)이 항상 와일드카드보다 우선
//   2. *.example.com은 any.example.com에 매칭되지만 example.com에는 안됨
//   3. *.example.com은 a.b.example.com에도 매칭됨 (다단계)
//   4. Empty Non-Terminal이 존재하면 와일드카드 매칭 차단
//
// CoreDNS 와일드카드 해석 코드 (plugin/file/wildcard.go):
//   func replaceWithAsteriskLabel(qname string) (wildcard string) {
//       i, shot := dns.NextLabel(qname, 0)
//       if shot { return "" }
//       return "*." + qname[i:]
//   }
// ============================================================================

// RecordType은 DNS 레코드 타입을 나타낸다.
type RecordType string

const (
	TypeA     RecordType = "A"
	TypeAAAA  RecordType = "AAAA"
	TypeCNAME RecordType = "CNAME"
	TypeTXT   RecordType = "TXT"
)

// DNSRecord는 DNS 레코드를 나타낸다.
type DNSRecord struct {
	Name  string     // 소유자 이름 (예: "*.example.com.")
	Type  RecordType // 레코드 타입
	Value string     // 레코드 값
	TTL   uint32     // TTL
}

// Zone은 DNS 존을 나타낸다. 트리 구조 대신 맵을 사용하여 단순화.
type Zone struct {
	name    string                         // 존 이름 (예: "example.com.")
	records map[string]map[RecordType][]DNSRecord // name → type → records
}

// NewZone은 새 존을 생성한다.
func NewZone(name string) *Zone {
	return &Zone{
		name:    ensureDot(name),
		records: make(map[string]map[RecordType][]DNSRecord),
	}
}

// AddRecord는 레코드를 존에 추가한다.
func (z *Zone) AddRecord(name string, rtype RecordType, value string, ttl uint32) {
	name = ensureDot(name)
	if _, ok := z.records[name]; !ok {
		z.records[name] = make(map[RecordType][]DNSRecord)
	}
	z.records[name][rtype] = append(z.records[name][rtype], DNSRecord{
		Name:  name,
		Type:  rtype,
		Value: value,
		TTL:   ttl,
	})
}

// Lookup은 DNS 조회를 수행한다.
// CoreDNS의 조회 우선순위를 시뮬레이션:
//   1. 정확 매칭 (exact match)
//   2. Empty Non-Terminal 체크 (있으면 와일드카드 차단)
//   3. 와일드카드 매칭 (상위 레벨로 올라가며 검색)
func (z *Zone) Lookup(qname string, qtype RecordType) ([]DNSRecord, string) {
	qname = ensureDot(qname)

	// 1단계: 정확 매칭 (exact match) - 항상 최우선
	if typeMap, ok := z.records[qname]; ok {
		if records, ok := typeMap[qtype]; ok {
			return records, "정확 매칭(exact match)"
		}
		// 이름은 존재하지만 요청한 타입이 없음 → NODATA
		return nil, fmt.Sprintf("NODATA (이름 '%s' 존재, %s 레코드 없음)", qname, qtype)
	}

	// 2단계: Empty Non-Terminal 체크
	// 존에 하위 레코드가 존재하면 이 이름은 ENT이며 와일드카드가 매칭되지 않음
	if z.isEmptyNonTerminal(qname) {
		return nil, fmt.Sprintf("Empty Non-Terminal ('%s'에 하위 레코드 존재 → 와일드카드 차단)", qname)
	}

	// 3단계: 와일드카드 매칭
	// CoreDNS replaceWithAsteriskLabel()과 동일한 로직
	// 가장 가까운 와일드카드부터 상위로 올라가며 검색
	wildcard := replaceWithAsteriskLabel(qname)
	for wildcard != "" {
		if typeMap, ok := z.records[wildcard]; ok {
			if records, ok := typeMap[qtype]; ok {
				// 와일드카드 매칭 시 실제 이름으로 치환하여 반환
				result := make([]DNSRecord, len(records))
				for i, r := range records {
					result[i] = r
					result[i].Name = qname // 응답에서는 원래 질의 이름 사용
				}
				return result, fmt.Sprintf("와일드카드 매칭 ('%s' → '%s')", wildcard, qname)
			}
		}
		// 한 단계 위로 올라가서 다시 시도
		wildcard = replaceWithAsteriskLabel(wildcard[2:]) // "*." 제거 후 재시도
	}

	return nil, "NXDOMAIN (매칭 없음)"
}

// replaceWithAsteriskLabel은 가장 왼쪽 레이블을 '*'로 교체한다.
// CoreDNS 실제 코드 (plugin/file/wildcard.go):
//
//	func replaceWithAsteriskLabel(qname string) (wildcard string) {
//	    i, shot := dns.NextLabel(qname, 0)
//	    if shot { return "" }
//	    return "*." + qname[i:]
//	}
func replaceWithAsteriskLabel(qname string) string {
	// 첫 번째 '.'을 찾아서 그 이후를 "*."와 결합
	idx := strings.Index(qname, ".")
	if idx == -1 || idx == len(qname)-1 {
		return "" // 최상위 도메인이면 와일드카드 없음
	}
	rest := qname[idx+1:]
	if rest == "" {
		return ""
	}
	return "*." + rest
}

// isEmptyNonTerminal은 qname이 Empty Non-Terminal인지 확인한다.
// ENT: 해당 이름에 직접 레코드는 없지만, 하위 이름에 레코드가 있는 경우
// 예: b.example.com.에 레코드가 없지만 a.b.example.com.에 레코드가 있으면
//
//	b.example.com.은 ENT → 와일드카드 매칭 차단
func (z *Zone) isEmptyNonTerminal(qname string) bool {
	// qname에 직접 레코드가 있으면 ENT가 아님 (이미 위에서 처리됨)
	if _, ok := z.records[qname]; ok {
		return false
	}

	// 하위 이름에 레코드가 있는지 확인
	suffix := "." + qname
	for name := range z.records {
		if strings.HasSuffix(name, suffix) && name != qname {
			return true
		}
	}
	return false
}

// ensureDot은 FQDN 끝에 점을 추가한다.
func ensureDot(name string) string {
	if !strings.HasSuffix(name, ".") {
		return name + "."
	}
	return name
}

func main() {
	fmt.Println("=== CoreDNS DNS 와일드카드 매칭 PoC ===")
	fmt.Println()
	fmt.Println("CoreDNS file 플러그인의 와일드카드 처리를 시뮬레이션합니다.")
	fmt.Println("참조: plugin/file/wildcard.go, plugin/file/lookup.go")
	fmt.Println()

	// 존 생성 및 레코드 등록
	zone := NewZone("example.com")

	// 정확 매칭 레코드
	zone.AddRecord("example.com", TypeA, "93.184.216.34", 300)
	zone.AddRecord("www.example.com", TypeA, "93.184.216.35", 300)
	zone.AddRecord("mail.example.com", TypeA, "93.184.216.36", 300)

	// 와일드카드 레코드
	zone.AddRecord("*.example.com", TypeA, "10.0.0.1", 60)
	zone.AddRecord("*.example.com", TypeTXT, "wildcard-catch-all", 60)

	// 다단계 와일드카드
	zone.AddRecord("*.staging.example.com", TypeA, "10.1.0.1", 120)

	// Empty Non-Terminal 시나리오
	// internal.example.com에는 레코드 없음, 하지만 하위에 있음
	zone.AddRecord("db.internal.example.com", TypeA, "10.2.0.1", 300)

	fmt.Println("=== 등록된 레코드 ===")
	fmt.Println("  example.com.           A   93.184.216.34")
	fmt.Println("  www.example.com.       A   93.184.216.35")
	fmt.Println("  mail.example.com.      A   93.184.216.36")
	fmt.Println("  *.example.com.         A   10.0.0.1")
	fmt.Println("  *.example.com.         TXT wildcard-catch-all")
	fmt.Println("  *.staging.example.com. A   10.1.0.1")
	fmt.Println("  db.internal.example.com. A 10.2.0.1")
	fmt.Println()

	// --- 테스트 케이스 실행 ---
	testCases := []struct {
		desc  string
		qname string
		qtype RecordType
	}{
		// 1. 정확 매칭이 와일드카드보다 우선
		{"정확 매칭 우선 (www.example.com)", "www.example.com", TypeA},
		{"정확 매칭 우선 (mail.example.com)", "mail.example.com", TypeA},

		// 2. 와일드카드 매칭
		{"와일드카드 매칭 (unknown.example.com)", "unknown.example.com", TypeA},
		{"와일드카드 TXT 매칭 (test.example.com)", "test.example.com", TypeTXT},
		{"와일드카드 매칭 (random123.example.com)", "random123.example.com", TypeA},

		// 3. 다단계 와일드카드 매칭
		// a.b.example.com → *.example.com에 매칭 (직접 와일드카드 우선)
		{"다단계: app.staging.example.com", "app.staging.example.com", TypeA},
		{"다단계: v2.staging.example.com", "v2.staging.example.com", TypeA},

		// 4. 상위 와일드카드로 폴백
		{"상위 폴백: deep.nested.example.com", "deep.nested.example.com", TypeA},

		// 5. Empty Non-Terminal
		{"ENT: internal.example.com (하위에 db.internal 존재)", "internal.example.com", TypeA},
		{"ENT 하위 직접 조회: db.internal.example.com", "db.internal.example.com", TypeA},

		// 6. 존 apex는 와일드카드에 매칭 안됨
		{"존 apex (example.com)", "example.com", TypeA},

		// 7. NODATA: 이름 존재하지만 타입 없음
		{"NODATA: www.example.com AAAA", "www.example.com", TypeAAAA},

		// 8. NXDOMAIN: 다른 존의 이름
		{"다른 존: test.other.com", "test.other.com", TypeA},
	}

	fmt.Println("=== 쿼리 결과 ===")
	fmt.Println()

	for i, tc := range testCases {
		records, reason := zone.Lookup(tc.qname, tc.qtype)
		fmt.Printf("[%2d] %s\n", i+1, tc.desc)
		fmt.Printf("     쿼리: %s %s\n", ensureDot(tc.qname), tc.qtype)
		fmt.Printf("     판정: %s\n", reason)
		if len(records) > 0 {
			for _, r := range records {
				fmt.Printf("     응답: %s %s %s (TTL=%d)\n", r.Name, r.Type, r.Value, r.TTL)
			}
		} else {
			fmt.Println("     응답: (없음)")
		}
		fmt.Println()
	}

	// --- replaceWithAsteriskLabel 동작 시연 ---
	fmt.Println("=== replaceWithAsteriskLabel() 동작 ===")
	fmt.Println()
	fmt.Println("CoreDNS가 와일드카드를 찾을 때 사용하는 레이블 교체 함수:")
	fmt.Println()

	testLabels := []string{
		"www.example.com.",
		"a.b.example.com.",
		"deep.nested.staging.example.com.",
		"example.com.",
		"com.",
	}

	for _, label := range testLabels {
		result := replaceWithAsteriskLabel(label)
		if result == "" {
			fmt.Printf("  %-40s → (매칭 불가)\n", label)
		} else {
			fmt.Printf("  %-40s → %s\n", label, result)
		}
	}

	fmt.Println()
	fmt.Println("=== 와일드카드 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 정리:")
	fmt.Println("1. 정확 매칭(exact match)이 항상 와일드카드보다 우선")
	fmt.Println("2. *.example.com은 unknown.example.com에 매칭")
	fmt.Println("3. Empty Non-Terminal이 있으면 와일드카드 매칭 차단")
	fmt.Println("4. 가장 가까운 와일드카드부터 상위로 올라가며 검색")
	fmt.Println("5. 존 apex(example.com)는 와일드카드에 매칭 안됨")
}
