// poc-04-dns-forwarder: CoreDNS DNS 포워더 시뮬레이션
//
// CoreDNS forward 플러그인의 업스트림 포워딩을 재현한다:
//   - 다중 업스트림 서버 관리
//   - 라운드 로빈/랜덤 정책
//   - 타임아웃 및 재시도
//   - 헬스체크 기반 업스트림 선택
//   - 동시 요청 수 제한
//
// 참조:
//   - plugin/forward/forward.go: Forward 구조체, ServeDNS
//   - plugin/forward/forward.go:127: 업스트림 순회 루프 (deadline + retry)
//   - plugin/forward/forward.go:36: Forward 구조체 (proxies, policy, maxfails 등)
//
// 주의: 실제 네트워크 포워딩 대신 로컬 시뮬레이션으로 동작한다.
//
// 사용법: go run main.go

package main

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// DNS 요청/응답 (단순화)
// =============================================================================

// DNSRequest는 DNS 쿼리를 나타낸다.
type DNSRequest struct {
	Name  string
	Type  string
	ID    uint16
}

// DNSResponse는 DNS 응답을 나타낸다.
type DNSResponse struct {
	Name    string
	Type    string
	Data    string
	TTL     uint32
	Rcode   int
	From    string // 응답한 업스트림 서버
	Latency time.Duration
}

func (r *DNSResponse) String() string {
	if r.Rcode != 0 {
		return fmt.Sprintf("RCODE=%d from=%s latency=%v", r.Rcode, r.From, r.Latency)
	}
	return fmt.Sprintf("%s %s %s TTL=%d from=%s latency=%v",
		r.Name, r.Type, r.Data, r.TTL, r.From, r.Latency)
}

// =============================================================================
// 업스트림 프록시 (plugin/pkg/proxy 재현)
// =============================================================================

// Proxy는 하나의 업스트림 DNS 서버를 나타낸다.
// CoreDNS의 plugin/pkg/proxy/Proxy를 단순화한 버전이다.
type Proxy struct {
	addr     string
	fails    int64  // 원자적 실패 카운트
	healthy  bool
	mu       sync.RWMutex

	// 시뮬레이션 파라미터
	latency    time.Duration // 시뮬레이션 응답 지연
	errorRate  float64       // 오류 발생 확률 (0.0~1.0)
	records    map[string]string // 이 업스트림이 알고 있는 레코드
}

// NewProxy는 새 업스트림 프록시를 생성한다.
func NewProxy(addr string, latency time.Duration, errorRate float64, records map[string]string) *Proxy {
	return &Proxy{
		addr:      addr,
		healthy:   true,
		latency:   latency,
		errorRate: errorRate,
		records:   records,
	}
}

// Addr는 업스트림 주소를 반환한다.
func (p *Proxy) Addr() string { return p.addr }

// Down은 업스트림이 다운되었는지 확인한다.
// CoreDNS forward.go:136의 proxy.Down(f.maxfails) 재현.
func (p *Proxy) Down(maxfails int64) bool {
	if maxfails == 0 {
		return false
	}
	fails := atomic.LoadInt64(&p.fails)
	return fails >= maxfails
}

// Healthcheck는 즉시 헬스체크를 수행한다.
// CoreDNS에서는 실패 시 proxy.Healthcheck()를 호출한다 (forward.go:196).
func (p *Proxy) Healthcheck() {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 시뮬레이션: 50% 확률로 복구
	if rand.Float64() > 0.5 {
		p.healthy = true
		atomic.StoreInt64(&p.fails, 0)
		fmt.Printf("    [헬스체크] %s: 복구됨\n", p.addr)
	} else {
		fmt.Printf("    [헬스체크] %s: 여전히 다운\n", p.addr)
	}
}

// Connect는 업스트림에 연결하여 DNS 쿼리를 전송한다.
// CoreDNS에서는 proxy.Connect(ctx, state, opts)를 호출한다 (forward.go:170).
func (p *Proxy) Connect(req DNSRequest) (*DNSResponse, error) {
	// 응답 지연 시뮬레이션
	time.Sleep(p.latency)

	// 오류 시뮬레이션
	if p.errorRate > 0 && rand.Float64() < p.errorRate {
		atomic.AddInt64(&p.fails, 1)
		return nil, fmt.Errorf("업스트림 %s 연결 실패", p.addr)
	}

	// 레코드 검색
	key := strings.ToLower(req.Name + ":" + req.Type)
	if data, ok := p.records[key]; ok {
		return &DNSResponse{
			Name:    req.Name,
			Type:    req.Type,
			Data:    data,
			TTL:     300,
			Rcode:   0, // NOERROR
			From:    p.addr,
			Latency: p.latency,
		}, nil
	}

	// NXDOMAIN
	return &DNSResponse{
		Name:  req.Name,
		Type:  req.Type,
		Rcode: 3, // NXDOMAIN
		From:  p.addr,
		Latency: p.latency,
	}, nil
}

// =============================================================================
// 포워딩 정책 (plugin/forward/policy.go 재현)
// =============================================================================

// Policy는 업스트림 선택 정책 인터페이스이다.
type Policy interface {
	List(proxies []*Proxy) []*Proxy
	Name() string
}

// RoundRobinPolicy는 라운드 로빈 정책이다.
type RoundRobinPolicy struct {
	current uint64
}

func (r *RoundRobinPolicy) List(proxies []*Proxy) []*Proxy {
	if len(proxies) == 0 {
		return proxies
	}

	idx := atomic.AddUint64(&r.current, 1) - 1
	start := int(idx % uint64(len(proxies)))

	result := make([]*Proxy, len(proxies))
	for i := 0; i < len(proxies); i++ {
		result[i] = proxies[(start+i)%len(proxies)]
	}
	return result
}

func (r *RoundRobinPolicy) Name() string { return "round_robin" }

// RandomPolicy는 랜덤 정책이다.
// CoreDNS forward 플러그인의 기본 정책이다.
type RandomPolicy struct{}

func (r *RandomPolicy) List(proxies []*Proxy) []*Proxy {
	result := make([]*Proxy, len(proxies))
	copy(result, proxies)

	// Fisher-Yates 셔플
	for i := len(result) - 1; i > 0; i-- {
		j := rand.Intn(i + 1)
		result[i], result[j] = result[j], result[i]
	}
	return result
}

func (r *RandomPolicy) Name() string { return "random" }

// =============================================================================
// Forward 구조체 (plugin/forward/forward.go:36 재현)
// =============================================================================

// Forward는 DNS 포워딩 프록시를 나타낸다.
// CoreDNS forward.go:36의 Forward 구조체를 단순화하여 재현:
//
//	type Forward struct {
//	    concurrent int64
//	    proxies    []*Proxy
//	    p          Policy
//	    maxfails   uint32
//	    expire     time.Duration
//	    maxConcurrent int64
//	    ...
//	}
type Forward struct {
	concurrent int64 // 현재 동시 요청 수 (원자적)

	proxies  []*Proxy
	policy   Policy
	from     string   // 매칭 존 (예: ".")
	ignored  []string // 무시 존 목록

	maxfails      int64
	maxConcurrent int64
	timeout       time.Duration

	// 통계
	totalQueries int64
	totalErrors  int64

	// ErrLimitExceeded는 동시 요청 초과 시 반환된다 (forward.go:63).
	ErrLimitExceeded error
}

// NewForward는 새 Forward 인스턴스를 생성한다.
// CoreDNS forward.go:72의 New() 재현.
func NewForward() *Forward {
	return &Forward{
		maxfails:         2,
		timeout:          5 * time.Second,
		maxConcurrent:    0, // 0 = 제한 없음
		policy:           &RandomPolicy{},
		from:             ".",
		ErrLimitExceeded: errors.New("동시 요청 수 초과"),
	}
}

// AddProxy는 업스트림 프록시를 추가한다.
func (f *Forward) AddProxy(p *Proxy) {
	f.proxies = append(f.proxies, p)
}

// SetPolicy는 포워딩 정책을 설정한다.
func (f *Forward) SetPolicy(p Policy) {
	f.policy = p
}

// SetMaxConcurrent는 최대 동시 요청 수를 설정한다.
func (f *Forward) SetMaxConcurrent(n int64) {
	f.maxConcurrent = n
}

// match는 요청이 이 포워더의 존에 해당하는지 확인한다.
// CoreDNS forward.go:259의 match 메서드 재현.
func (f *Forward) match(name string) bool {
	name = strings.ToLower(name)

	// from이 "."이면 모든 쿼리 매칭
	if f.from == "." {
		return true
	}

	// 서브도메인 매칭
	from := strings.ToLower(f.from)
	if name == from || strings.HasSuffix(name, "."+from) {
		// 무시 목록 확인
		for _, ignored := range f.ignored {
			if name == ignored || strings.HasSuffix(name, "."+ignored) {
				return false
			}
		}
		return true
	}
	return false
}

// ServeDNS는 DNS 쿼리를 업스트림으로 포워딩한다.
// CoreDNS forward.go:102의 ServeDNS를 재현한다.
//
// 핵심 로직:
// 1. 존 매칭 확인
// 2. 동시 요청 수 확인
// 3. 정책에 따라 업스트림 목록 순회
// 4. 다운된 업스트림 건너뛰기
// 5. 타임아웃 내 재시도
func (f *Forward) ServeDNS(req DNSRequest) (*DNSResponse, error) {
	atomic.AddInt64(&f.totalQueries, 1)

	// 존 매칭 확인
	if !f.match(req.Name) {
		return nil, fmt.Errorf("존 매칭 실패: %s (from=%s)", req.Name, f.from)
	}

	// 동시 요청 수 제한 (forward.go:108-115)
	if f.maxConcurrent > 0 {
		count := atomic.AddInt64(&f.concurrent, 1)
		defer atomic.AddInt64(&f.concurrent, -1)
		if count > f.maxConcurrent {
			atomic.AddInt64(&f.totalErrors, 1)
			return nil, f.ErrLimitExceeded
		}
	}

	// 업스트림 순회 (forward.go:127 재현)
	fails := 0
	var upstreamErr error
	deadline := time.Now().Add(f.timeout)
	list := f.policy.List(f.proxies)
	i := 0

	for time.Now().Before(deadline) {
		if i >= len(list) {
			i = 0
			fails = 0
		}

		proxy := list[i]
		i++

		// 다운된 업스트림 건너뛰기 (forward.go:136)
		if proxy.Down(f.maxfails) {
			fails++
			if fails < len(f.proxies) {
				fmt.Printf("    [포워더] %s 다운 (fails=%d), 건너뜀\n",
					proxy.Addr(), atomic.LoadInt64(&proxy.fails))
				continue
			}

			// 모든 업스트림 다운 → 랜덤 선택 (forward.go:150)
			fmt.Println("    [포워더] 모든 업스트림 다운, 랜덤 선택")
			rp := &RandomPolicy{}
			proxy = rp.List(f.proxies)[0]
		}

		fmt.Printf("    [포워더] %s로 전달 중...\n", proxy.Addr())

		// 업스트림 연결 (forward.go:170)
		ret, err := proxy.Connect(req)

		if err != nil {
			upstreamErr = err
			fmt.Printf("    [포워더] %s 오류: %v\n", proxy.Addr(), err)

			// 헬스체크 트리거 (forward.go:196)
			if f.maxfails != 0 {
				proxy.Healthcheck()
			}

			if fails < len(f.proxies) {
				continue
			}
			break
		}

		// 성공
		return ret, nil
	}

	atomic.AddInt64(&f.totalErrors, 1)

	if upstreamErr != nil {
		return nil, upstreamErr
	}
	return nil, errors.New("정상 업스트림 없음")
}

// Stats는 포워더 통계를 반환한다.
func (f *Forward) Stats() string {
	return fmt.Sprintf("총 쿼리: %d, 총 오류: %d, 현재 동시: %d",
		atomic.LoadInt64(&f.totalQueries),
		atomic.LoadInt64(&f.totalErrors),
		atomic.LoadInt64(&f.concurrent))
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== CoreDNS DNS 포워더 시뮬레이션 ===")
	fmt.Println()

	// -------------------------------------------------------------------------
	// 1. 업스트림 서버 설정 (시뮬레이션)
	// -------------------------------------------------------------------------
	// 공유 레코드 (모든 업스트림이 동일 데이터 보유)
	sharedRecords := map[string]string{
		"example.com.:a":     "93.184.216.34",
		"www.example.com.:a": "93.184.216.34",
		"api.example.com.:a": "93.184.216.100",
	}

	upstream1 := NewProxy("8.8.8.8:53", 10*time.Millisecond, 0.0, sharedRecords)   // Google DNS (안정)
	upstream2 := NewProxy("8.8.4.4:53", 15*time.Millisecond, 0.0, sharedRecords)   // Google DNS 2 (안정)
	upstream3 := NewProxy("1.1.1.1:53", 5*time.Millisecond, 0.3, sharedRecords)    // Cloudflare (가끔 실패)

	// -------------------------------------------------------------------------
	// 2. 라운드 로빈 포워더
	// -------------------------------------------------------------------------
	fmt.Println("--- 시나리오 1: 라운드 로빈 정책 ---")
	fwd := NewForward()
	fwd.AddProxy(upstream1)
	fwd.AddProxy(upstream2)
	fwd.AddProxy(upstream3)
	fwd.SetPolicy(&RoundRobinPolicy{})

	queries := []DNSRequest{
		{Name: "example.com.", Type: "A", ID: 1},
		{Name: "www.example.com.", Type: "A", ID: 2},
		{Name: "api.example.com.", Type: "A", ID: 3},
		{Name: "example.com.", Type: "A", ID: 4},
		{Name: "www.example.com.", Type: "A", ID: 5},
	}

	for _, q := range queries {
		fmt.Printf("\n  쿼리: %s %s\n", q.Name, q.Type)
		resp, err := fwd.ServeDNS(q)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
		} else {
			fmt.Printf("  응답: %s\n", resp)
		}
	}

	fmt.Printf("\n  통계: %s\n", fwd.Stats())

	// -------------------------------------------------------------------------
	// 3. 랜덤 정책 포워더
	// -------------------------------------------------------------------------
	fmt.Println("\n--- 시나리오 2: 랜덤 정책 ---")
	fwd2 := NewForward()
	fwd2.AddProxy(NewProxy("dns1.test:53", 10*time.Millisecond, 0.0, sharedRecords))
	fwd2.AddProxy(NewProxy("dns2.test:53", 10*time.Millisecond, 0.0, sharedRecords))
	fwd2.SetPolicy(&RandomPolicy{})

	distribution := make(map[string]int)
	for i := 0; i < 10; i++ {
		resp, err := fwd2.ServeDNS(DNSRequest{Name: "example.com.", Type: "A", ID: uint16(i)})
		if err == nil {
			distribution[resp.From]++
		}
	}

	fmt.Println("  업스트림 분배:")
	for addr, count := range distribution {
		fmt.Printf("    %s: %d회\n", addr, count)
	}

	// -------------------------------------------------------------------------
	// 4. 장애 복구 시뮬레이션
	// -------------------------------------------------------------------------
	fmt.Println("\n--- 시나리오 3: 업스트림 장애 및 복구 ---")
	failProxy := NewProxy("failing.dns:53", 5*time.Millisecond, 1.0, sharedRecords) // 항상 실패
	okProxy := NewProxy("stable.dns:53", 10*time.Millisecond, 0.0, sharedRecords)   // 항상 성공

	fwd3 := NewForward()
	fwd3.AddProxy(failProxy)
	fwd3.AddProxy(okProxy)
	fwd3.SetPolicy(&RoundRobinPolicy{})
	fwd3.maxfails = 2

	for i := 0; i < 5; i++ {
		fmt.Printf("\n  요청 %d:\n", i+1)
		resp, err := fwd3.ServeDNS(DNSRequest{Name: "example.com.", Type: "A", ID: uint16(100 + i)})
		if err != nil {
			fmt.Printf("  최종 오류: %v\n", err)
		} else {
			fmt.Printf("  성공: %s\n", resp)
		}
	}

	fmt.Printf("\n  failing.dns 실패 카운트: %d\n", atomic.LoadInt64(&failProxy.fails))
	fmt.Printf("  stable.dns 실패 카운트: %d\n", atomic.LoadInt64(&okProxy.fails))

	// -------------------------------------------------------------------------
	// 5. 동시 요청 제한
	// -------------------------------------------------------------------------
	fmt.Println("\n--- 시나리오 4: 동시 요청 제한 (maxConcurrent=2) ---")
	slowProxy := NewProxy("slow.dns:53", 100*time.Millisecond, 0.0, sharedRecords)

	fwd4 := NewForward()
	fwd4.AddProxy(slowProxy)
	fwd4.SetMaxConcurrent(2)

	var wg sync.WaitGroup
	results := make([]string, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := fwd4.ServeDNS(DNSRequest{
				Name: "example.com.", Type: "A", ID: uint16(200 + idx),
			})
			if err != nil {
				results[idx] = fmt.Sprintf("  요청 %d: 오류 - %v", idx+1, err)
			} else {
				results[idx] = fmt.Sprintf("  요청 %d: 성공 - %s", idx+1, resp)
			}
		}(i)
		time.Sleep(10 * time.Millisecond) // 약간의 시차
	}

	wg.Wait()
	for _, r := range results {
		fmt.Println(r)
	}
	fmt.Printf("  통계: %s\n", fwd4.Stats())

	// -------------------------------------------------------------------------
	// 6. 존 매칭 테스트
	// -------------------------------------------------------------------------
	fmt.Println("\n--- 시나리오 5: 존 매칭 ---")
	fwd5 := NewForward()
	fwd5.from = "example.com."
	fwd5.ignored = []string{"internal.example.com."}
	fwd5.AddProxy(NewProxy("zone-dns:53", 5*time.Millisecond, 0.0, sharedRecords))

	matchTests := []struct {
		name string
		desc string
	}{
		{"www.example.com.", "example.com. 하위 → 매칭"},
		{"example.com.", "정확히 일치 → 매칭"},
		{"internal.example.com.", "무시 목록 → 매칭 안 됨"},
		{"app.internal.example.com.", "무시 목록 하위 → 매칭 안 됨"},
		{"other.org.", "다른 존 → 매칭 안 됨"},
	}

	for _, mt := range matchTests {
		matched := fwd5.match(mt.name)
		fmt.Printf("  match(%s) = %v (%s)\n", mt.name, matched, mt.desc)
	}

	fmt.Println("\n완료.")
}
