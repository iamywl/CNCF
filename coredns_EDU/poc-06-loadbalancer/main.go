// poc-06-loadbalancer: CoreDNS 로드밸런스 플러그인의 DNS 레코드 셔플링 시뮬레이션
//
// CoreDNS loadbalance 플러그인(plugin/loadbalance/)의 라운드 로빈 셔플과
// 가중치 기반 로드밸런싱 알고리즘을 재현한다.
//
// 실제 소스 참조:
//   - plugin/loadbalance/loadbalance.go: roundRobin(), roundRobinShuffle()
//   - plugin/loadbalance/handler.go: ServeDNS(), LoadBalanceResponseWriter
//   - plugin/loadbalance/weighted.go: weightedRoundRobin(), topAddressIndex()
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// ============================================================================
// 1. DNS 레코드 타입 정의
// ============================================================================

// DNS 레코드 타입 상수
const (
	TypeA     uint16 = 1
	TypeAAAA  uint16 = 28
	TypeCNAME uint16 = 5
	TypeMX    uint16 = 15
	TypeSRV   uint16 = 33
)

// RR은 DNS 리소스 레코드를 나타낸다
type RR struct {
	Name   string
	Type   uint16
	Value  string
	TTL    uint32
	Weight uint8 // 가중치 (weighted 정책용)
}

// TypeString은 레코드 타입을 문자열로 반환한다
func (r RR) TypeString() string {
	switch r.Type {
	case TypeA:
		return "A"
	case TypeAAAA:
		return "AAAA"
	case TypeCNAME:
		return "CNAME"
	case TypeMX:
		return "MX"
	case TypeSRV:
		return "SRV"
	default:
		return fmt.Sprintf("TYPE%d", r.Type)
	}
}

// DNSMessage는 DNS 응답 메시지를 나타낸다
type DNSMessage struct {
	Rcode    int  // 응답 코드
	Question string
	QType    uint16
	Answer   []RR
	Ns       []RR
	Extra    []RR
}

// ============================================================================
// 2. 라운드 로빈 셔플 - plugin/loadbalance/loadbalance.go 재현
// ============================================================================

// randID는 dns.Id()를 시뮬레이션한다 (CoreDNS는 이를 난수 소스로 사용)
func randID() uint16 {
	return uint16(rand.Intn(65536))
}

// roundRobinShuffle은 레코드 슬라이스를 Fisher-Yates 변형으로 셔플한다
// 실제 소스: plugin/loadbalance/loadbalance.go의 func roundRobinShuffle()
// CoreDNS는 dns.Id()를 난수 소스로 사용하는 것이 특징이다
func roundRobinShuffle(records []RR) {
	l := len(records)
	switch l {
	case 0, 1:
		// 0~1개는 셔플 불필요
		return
	case 2:
		// 2개일 때는 50% 확률로 스왑
		if randID()%2 == 0 {
			records[0], records[1] = records[1], records[0]
		}
	default:
		// Fisher-Yates 변형 셔플
		// 실제 소스에서 p = j + (dns.Id() % (l - j))
		for j := 0; j < l; j++ {
			p := j + (int(randID()) % (l - j))
			if j == p {
				continue
			}
			records[j], records[p] = records[p], records[j]
		}
	}
}

// roundRobin은 레코드를 타입별로 분류하고, A/AAAA/MX만 셔플한다
// 실제 소스: plugin/loadbalance/loadbalance.go의 func roundRobin()
// 핵심: CNAME은 항상 순서 유지, A/AAAA/MX만 셔플
func roundRobin(in []RR) []RR {
	var cname []RR
	var address []RR
	var mx []RR
	var rest []RR

	for _, r := range in {
		switch r.Type {
		case TypeCNAME:
			cname = append(cname, r)
		case TypeA, TypeAAAA:
			address = append(address, r)
		case TypeMX:
			mx = append(mx, r)
		default:
			rest = append(rest, r)
		}
	}

	// A/AAAA와 MX만 셔플
	roundRobinShuffle(address)
	roundRobinShuffle(mx)

	// 결합 순서: CNAME → rest → address → MX
	out := append(cname, rest...)
	out = append(out, address...)
	out = append(out, mx...)
	return out
}

// randomShuffle은 Answer, Ns, Extra 모두에 라운드 로빈을 적용한다
// 실제 소스: plugin/loadbalance/loadbalance.go의 func randomShuffle()
func randomShuffle(msg *DNSMessage) *DNSMessage {
	msg.Answer = roundRobin(msg.Answer)
	msg.Ns = roundRobin(msg.Ns)
	msg.Extra = roundRobin(msg.Extra)
	return msg
}

// ============================================================================
// 3. 가중치 기반 셔플 - plugin/loadbalance/weighted.go 재현
// ============================================================================

// WeightItem은 주소별 가중치를 나타낸다
// 실제 소스: plugin/loadbalance/weighted.go의 weightItem 구조체
type WeightItem struct {
	Address string
	Value   uint8
}

// WeightConfig는 도메인별 가중치 설정을 관리한다
// 실제 소스: plugin/loadbalance/weighted.go의 weightedRR 구조체
type WeightConfig struct {
	Domains map[string][]WeightItem
	rng     *rand.Rand
}

// NewWeightConfig는 새 가중치 설정을 생성한다
func NewWeightConfig() *WeightConfig {
	return &WeightConfig{
		Domains: make(map[string][]WeightItem),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// weightedRoundRobin은 가중치 기반으로 레코드를 재배열한다
// 실제 소스: plugin/loadbalance/weighted.go의 func (w *weightedRR) weightedRoundRobin()
func (w *WeightConfig) weightedRoundRobin(in []RR) []RR {
	var cname []RR
	var address []RR
	var mx []RR
	var rest []RR

	for _, r := range in {
		switch r.Type {
		case TypeCNAME:
			cname = append(cname, r)
		case TypeA, TypeAAAA:
			address = append(address, r)
		case TypeMX:
			mx = append(mx, r)
		default:
			rest = append(rest, r)
		}
	}

	if len(address) == 0 {
		return in
	}

	// 가중치 기반으로 첫 번째 레코드 선택
	w.setTopRecord(address)

	out := append(cname, rest...)
	out = append(out, address...)
	out = append(out, mx...)
	return out
}

// setTopRecord는 가중치에 따라 첫 번째 레코드를 선택한다
// 실제 소스: plugin/loadbalance/weighted.go의 func (w *weightedRR) setTopRecord()
func (w *WeightConfig) setTopRecord(address []RR) {
	itop := w.topAddressIndex(address)
	if itop < 0 || itop == 0 {
		return
	}
	address[0], address[itop] = address[itop], address[0]
}

// topAddressIndex는 가중치 확률 분포에 따라 첫 번째 주소의 인덱스를 계산한다
// 실제 소스: plugin/loadbalance/weighted.go의 func (w *weightedRR) topAddressIndex()
// 알고리즘:
//  1. 각 주소의 가중치를 조회 (기본값=1)
//  2. 가중치 합계 계산
//  3. 가중치 내림차순 정렬
//  4. [0, wsum) 범위의 난수로 누적 확률 비교하여 선택
func (w *WeightConfig) topAddressIndex(address []RR) int {
	type waddr struct {
		index  int
		weight uint8
	}

	var wsum uint
	weighted := make([]waddr, len(address))

	for i, ar := range address {
		wa := &weighted[i]
		wa.index = i
		wa.weight = 1 // 기본 가중치

		// 도메인별 가중치 조회
		if ws, ok := w.Domains[ar.Name]; ok {
			for _, witem := range ws {
				if witem.Address == ar.Value {
					wa.weight = witem.Value
					break
				}
			}
		}
		wsum += uint(wa.weight)
	}

	// 가중치 내림차순 정렬
	sort.Slice(weighted, func(i, j int) bool {
		return weighted[i].weight > weighted[j].weight
	})

	// 가중치 확률 분포에 따라 선택
	v := uint(w.rng.Intn(int(wsum)))
	var psum uint
	for _, wa := range weighted {
		psum += uint(wa.weight)
		if v < psum {
			return wa.index
		}
	}

	return -1
}

// ============================================================================
// 4. 로드밸런서 - plugin/loadbalance/handler.go 재현
// ============================================================================

// LoadBalancer는 CoreDNS의 LoadBalance 플러그인을 재현한다
type LoadBalancer struct {
	Policy  string // "round_robin" 또는 "weighted"
	shuffle func(*DNSMessage) *DNSMessage
}

// NewLoadBalancer는 로드밸런서를 생성한다
func NewLoadBalancer(policy string, weightConfig *WeightConfig) *LoadBalancer {
	lb := &LoadBalancer{Policy: policy}

	switch policy {
	case "weighted":
		lb.shuffle = func(msg *DNSMessage) *DNSMessage {
			// 가중치 셔플은 A, AAAA, SRV에만 적용
			// 실제 소스: weighted.go의 weightedShuffle()
			switch msg.QType {
			case TypeA, TypeAAAA, TypeSRV:
				msg.Answer = weightConfig.weightedRoundRobin(msg.Answer)
				msg.Extra = weightConfig.weightedRoundRobin(msg.Extra)
			}
			return msg
		}
	default: // round_robin
		lb.shuffle = randomShuffle
	}

	return lb
}

// ProcessResponse는 응답을 가로채서 레코드를 셔플한다
// 실제 소스: plugin/loadbalance/loadbalance.go의 func (r *LoadBalanceResponseWriter) WriteMsg()
func (lb *LoadBalancer) ProcessResponse(msg *DNSMessage) *DNSMessage {
	// 에러 응답은 셔플하지 않음
	if msg.Rcode != 0 {
		return msg
	}

	// AXFR/IXFR은 셔플하지 않음 (존 전송)
	if msg.QType == 252 || msg.QType == 251 {
		return msg
	}

	return lb.shuffle(msg)
}

// ============================================================================
// 5. 데모 실행
// ============================================================================

func main() {
	fmt.Println("=== CoreDNS 로드밸런서 PoC ===")
	fmt.Println()
	fmt.Println("CoreDNS loadbalance 플러그인의 레코드 셔플링 알고리즘을 시뮬레이션합니다.")
	fmt.Println("참조: plugin/loadbalance/loadbalance.go, handler.go, weighted.go")
	fmt.Println()

	rand.Seed(time.Now().UnixNano())

	// ── 데모 1: 라운드 로빈 셔플 기본 동작 ──
	fmt.Println("── 1. 라운드 로빈 셔플 ──")
	fmt.Println()
	fmt.Println("  CoreDNS는 A/AAAA 레코드를 Fisher-Yates 변형으로 셔플한다.")
	fmt.Println("  CNAME은 항상 첫 번째 위치를 유지한다.")
	fmt.Println()

	lb := NewLoadBalancer("round_robin", nil)

	baseMsg := func() *DNSMessage {
		return &DNSMessage{
			Rcode:    0,
			Question: "web.example.com.",
			QType:    TypeA,
			Answer: []RR{
				{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.1", TTL: 300},
				{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.2", TTL: 300},
				{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.3", TTL: 300},
				{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.4", TTL: 300},
			},
		}
	}

	fmt.Println("  web.example.com A 레코드 4개를 10회 셔플:")
	orderCount := make(map[string]int)
	for i := 0; i < 10; i++ {
		msg := lb.ProcessResponse(baseMsg())
		var ips []string
		for _, r := range msg.Answer {
			ips = append(ips, r.Value)
		}
		order := strings.Join(ips, ", ")
		orderCount[order]++
		if i < 5 {
			fmt.Printf("  시도 %d: [%s]\n", i+1, order)
		}
	}
	fmt.Printf("  ... (총 10회, %d개 고유 순서 발생)\n", len(orderCount))
	fmt.Println()

	// ── 데모 2: 첫 번째 레코드 분포 ──
	fmt.Println("── 2. 첫 번째 레코드 분포 (1000회 시뮬레이션) ──")
	fmt.Println()
	fmt.Println("  균등 분포에 가까울수록 좋은 로드밸런싱:")

	firstCount := make(map[string]int)
	for i := 0; i < 1000; i++ {
		msg := lb.ProcessResponse(baseMsg())
		firstCount[msg.Answer[0].Value]++
	}

	for _, ip := range []string{"10.0.1.1", "10.0.1.2", "10.0.1.3", "10.0.1.4"} {
		count := firstCount[ip]
		bar := strings.Repeat("█", count/20)
		fmt.Printf("  %-12s: %3d회 (%4.1f%%) %s\n", ip, count, float64(count)/10, bar)
	}
	fmt.Println()

	// ── 데모 3: CNAME 순서 유지 ──
	fmt.Println("── 3. CNAME 순서 유지 ──")
	fmt.Println()
	fmt.Println("  CoreDNS roundRobin()은 CNAME을 항상 첫 번째에 배치한다.")
	fmt.Println("  이는 CNAME 체인 해석에 필수적이다.")
	fmt.Println()

	cnameMsg := &DNSMessage{
		Rcode:    0,
		Question: "www.example.com.",
		QType:    TypeA,
		Answer: []RR{
			{Name: "www.example.com.", Type: TypeCNAME, Value: "web.example.com.", TTL: 300},
			{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.1", TTL: 300},
			{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.2", TTL: 300},
			{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.3", TTL: 300},
		},
	}

	cnameFirst := true
	for i := 0; i < 20; i++ {
		msg := lb.ProcessResponse(cnameMsg)
		if msg.Answer[0].Type != TypeCNAME {
			cnameFirst = false
			break
		}
		cnameMsg = &DNSMessage{ // 원본 복구
			Rcode:    0,
			Question: "www.example.com.",
			QType:    TypeA,
			Answer: []RR{
				{Name: "www.example.com.", Type: TypeCNAME, Value: "web.example.com.", TTL: 300},
				{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.1", TTL: 300},
				{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.2", TTL: 300},
				{Name: "web.example.com.", Type: TypeA, Value: "10.0.1.3", TTL: 300},
			},
		}
	}
	result := lb.ProcessResponse(cnameMsg)
	fmt.Printf("  결과 예시: ")
	for i, r := range result.Answer {
		if i > 0 {
			fmt.Print(" → ")
		}
		fmt.Printf("[%s %s]", r.TypeString(), r.Value)
	}
	fmt.Println()
	fmt.Printf("  CNAME 항상 첫 번째: %v (20회 테스트)\n", cnameFirst)
	fmt.Println()

	// ── 데모 4: MX 레코드 셔플 ──
	fmt.Println("── 4. MX 레코드 셔플 ──")
	fmt.Println()

	mxMsg := func() *DNSMessage {
		return &DNSMessage{
			Rcode:    0,
			Question: "example.com.",
			QType:    TypeMX,
			Answer: []RR{
				{Name: "example.com.", Type: TypeMX, Value: "mail1.example.com.", TTL: 300},
				{Name: "example.com.", Type: TypeMX, Value: "mail2.example.com.", TTL: 300},
				{Name: "example.com.", Type: TypeMX, Value: "mail3.example.com.", TTL: 300},
			},
		}
	}

	fmt.Println("  MX 레코드 5회 셔플:")
	for i := 0; i < 5; i++ {
		msg := lb.ProcessResponse(mxMsg())
		var mxs []string
		for _, r := range msg.Answer {
			mxs = append(mxs, r.Value)
		}
		fmt.Printf("  시도 %d: [%s]\n", i+1, strings.Join(mxs, ", "))
	}
	fmt.Println()

	// ── 데모 5: 가중치 기반 로드밸런싱 ──
	fmt.Println("── 5. 가중치 기반 로드밸런싱 ──")
	fmt.Println()
	fmt.Println("  CoreDNS weighted 정책은 주소별 가중치에 따라 선택 확률을 조정한다.")
	fmt.Println("  실제 소스: plugin/loadbalance/weighted.go topAddressIndex()")
	fmt.Println()

	wc := NewWeightConfig()
	wc.Domains["api.example.com."] = []WeightItem{
		{Address: "10.0.1.1", Value: 5},  // 가중치 5 (50%)
		{Address: "10.0.1.2", Value: 3},  // 가중치 3 (30%)
		{Address: "10.0.1.3", Value: 2},  // 가중치 2 (20%)
	}

	wlb := NewLoadBalancer("weighted", wc)

	fmt.Println("  설정:")
	for _, w := range wc.Domains["api.example.com."] {
		total := 5 + 3 + 2
		fmt.Printf("    %s: 가중치=%d (기대 비율=%.0f%%)\n",
			w.Address, w.Value, float64(w.Value)/float64(total)*100)
	}
	fmt.Println()

	weightedApiMsg := func() *DNSMessage {
		return &DNSMessage{
			Rcode:    0,
			Question: "api.example.com.",
			QType:    TypeA,
			Answer: []RR{
				{Name: "api.example.com.", Type: TypeA, Value: "10.0.1.1", TTL: 60},
				{Name: "api.example.com.", Type: TypeA, Value: "10.0.1.2", TTL: 60},
				{Name: "api.example.com.", Type: TypeA, Value: "10.0.1.3", TTL: 60},
			},
		}
	}

	wFirstCount := make(map[string]int)
	iterations := 1000
	for i := 0; i < iterations; i++ {
		msg := wlb.ProcessResponse(weightedApiMsg())
		wFirstCount[msg.Answer[0].Value]++
	}

	fmt.Printf("  %d회 시뮬레이션 결과 (첫 번째 레코드 선택 빈도):\n", iterations)
	for _, ip := range []string{"10.0.1.1", "10.0.1.2", "10.0.1.3"} {
		count := wFirstCount[ip]
		bar := strings.Repeat("█", count/20)
		fmt.Printf("    %-12s: %3d회 (%4.1f%%) %s\n", ip, count, float64(count)/10, bar)
	}
	fmt.Println()

	// ── 데모 6: 레코드 2개일 때의 특수 처리 ──
	fmt.Println("── 6. 레코드 2개: 50% 스왑 확률 ──")
	fmt.Println()
	fmt.Println("  CoreDNS는 레코드가 2개일 때 dns.Id()%2로 50% 확률 스왑을 한다.")

	twoMsg := func() *DNSMessage {
		return &DNSMessage{
			Rcode:    0,
			Question: "dual.example.com.",
			QType:    TypeA,
			Answer: []RR{
				{Name: "dual.example.com.", Type: TypeA, Value: "10.0.2.1", TTL: 300},
				{Name: "dual.example.com.", Type: TypeA, Value: "10.0.2.2", TTL: 300},
			},
		}
	}

	swapCount := 0
	total := 1000
	for i := 0; i < total; i++ {
		msg := lb.ProcessResponse(twoMsg())
		if msg.Answer[0].Value == "10.0.2.2" {
			swapCount++
		}
	}
	fmt.Printf("  스왑 발생: %d/%d (%.1f%%, 이상적=50%%)\n", swapCount, total, float64(swapCount)/float64(total)*100)
	fmt.Println()

	// ── 데모 7: 에러 응답은 셔플하지 않음 ──
	fmt.Println("── 7. 에러 응답 바이패스 ──")
	fmt.Println()
	fmt.Println("  Rcode != 0 (에러)인 응답은 셔플하지 않고 그대로 전달한다.")

	errMsg := &DNSMessage{
		Rcode:    3, // NXDOMAIN
		Question: "missing.example.com.",
		QType:    TypeA,
		Answer:   nil,
	}
	result2 := lb.ProcessResponse(errMsg)
	fmt.Printf("  NXDOMAIN 응답: Rcode=%d, Answer=%v (셔플 없음)\n", result2.Rcode, result2.Answer)
	fmt.Println()

	fmt.Println("=== PoC 완료 ===")
}
