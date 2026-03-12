package main

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// CoreDNS 업스트림 헬스체크 PoC
// ============================================================================
//
// CoreDNS forward 플러그인의 업스트림 헬스체크 메커니즘을 시뮬레이션한다.
//
// 실제 CoreDNS 구현 참조:
//   - plugin/pkg/proxy/proxy.go  → Proxy 구조체, Down(), incrementFails()
//   - plugin/pkg/proxy/health.go → HealthChecker 인터페이스, Check()
//   - plugin/forward/forward.go  → Forward.ServeDNS()에서 proxy.Down(maxfails) 호출
//
// 핵심 동작:
//   1. 각 업스트림에 주기적으로 헬스체크 실행 (hcInterval 간격)
//   2. 실패 시 fails 카운터 증가 (atomic)
//   3. fails > maxfails면 Down() → true → 쿼리 제외
//   4. 성공 시 fails = 0으로 리셋 → 서버 복구
//
// CoreDNS 기본값: maxfails=2, hcInterval=500ms
// ============================================================================

// Upstream은 CoreDNS의 Proxy 구조체를 시뮬레이션한다.
// 실제 코드: plugin/pkg/proxy/proxy.go
//
//	type Proxy struct {
//	    fails     uint32     // atomic 카운터
//	    addr      string
//	    probe     *up.Probe  // 헬스체크 프로브
//	    health    HealthChecker
//	}
type Upstream struct {
	addr  string
	fails uint32 // atomic - 연속 실패 횟수

	// 시뮬레이션용: 서버 상태 (true=정상, false=다운)
	healthy atomic.Bool

	mu sync.Mutex
}

// NewUpstream은 새 업스트림 서버를 생성한다.
func NewUpstream(addr string) *Upstream {
	u := &Upstream{addr: addr}
	u.healthy.Store(true)
	return u
}

// Down은 이 업스트림이 다운되었는지 판정한다.
// 실제 CoreDNS: fails > maxfails이면 다운으로 판정
//
// 실제 코드 (plugin/pkg/proxy/proxy.go):
//
//	func (p *Proxy) Down(maxfails uint32) bool {
//	    if maxfails == 0 { return false }  // maxfails=0이면 헬스체크 비활성화
//	    fails := atomic.LoadUint32(&p.fails)
//	    return fails > maxfails
//	}
func (u *Upstream) Down(maxfails uint32) bool {
	if maxfails == 0 {
		return false // 헬스체크 비활성화
	}
	fails := atomic.LoadUint32(&u.fails)
	return fails > maxfails
}

// incrementFails는 실패 카운터를 안전하게 증가시킨다.
// 실제 코드에서는 오버플로우 검사도 수행한다.
func (u *Upstream) incrementFails() {
	curVal := atomic.LoadUint32(&u.fails)
	if curVal > curVal+1 {
		return // 오버플로우 방지
	}
	atomic.AddUint32(&u.fails, 1)
}

// resetFails는 성공 시 실패 카운터를 0으로 리셋한다.
// 실제 코드 (health.go Check 함수):
//
//	atomic.StoreUint32(&p.fails, 0)
func (u *Upstream) resetFails() {
	atomic.StoreUint32(&u.fails, 0)
}

// HealthChecker는 업스트림 헬스체크를 주기적으로 수행한다.
// CoreDNS는 ". IN NS" 쿼리를 보내 응답 여부로 건강 상태를 판정한다.
type HealthChecker struct {
	upstreams  []*Upstream
	maxfails   uint32
	interval   time.Duration
	stopCh     chan struct{}
	wg         sync.WaitGroup
	logMu      sync.Mutex
	logEntries []string
}

// NewHealthChecker는 새 헬스체커를 생성한다.
func NewHealthChecker(upstreams []*Upstream, maxfails uint32, interval time.Duration) *HealthChecker {
	return &HealthChecker{
		upstreams: upstreams,
		maxfails:  maxfails,
		interval:  interval,
		stopCh:    make(chan struct{}),
	}
}

// Start는 각 업스트림에 대해 헬스체크 고루틴을 시작한다.
// CoreDNS에서는 Proxy.Start(duration)이 up.Probe를 시작한다.
func (hc *HealthChecker) Start() {
	for _, u := range hc.upstreams {
		hc.wg.Add(1)
		go hc.checkLoop(u)
	}
}

// Stop은 모든 헬스체크 고루틴을 중지한다.
func (hc *HealthChecker) Stop() {
	close(hc.stopCh)
	hc.wg.Wait()
}

// checkLoop는 단일 업스트림에 대한 주기적 헬스체크를 수행한다.
func (hc *HealthChecker) checkLoop(u *Upstream) {
	defer hc.wg.Done()
	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-hc.stopCh:
			return
		case <-ticker.C:
			hc.check(u)
		}
	}
}

// check는 단일 헬스체크를 수행한다.
// 실제 CoreDNS (health.go):
//
//	func (h *dnsHc) Check(p *Proxy) error {
//	    err := h.send(p.addr)
//	    if err != nil {
//	        p.incrementFails()   // 실패 시 카운터 증가
//	        return err
//	    }
//	    atomic.StoreUint32(&p.fails, 0)  // 성공 시 리셋
//	    return nil
//	}
func (hc *HealthChecker) check(u *Upstream) {
	if u.healthy.Load() {
		// 서버 정상 → 실패 카운터 리셋
		u.resetFails()
	} else {
		// 서버 다운 → 실패 카운터 증가
		u.incrementFails()
		fails := atomic.LoadUint32(&u.fails)
		wasDown := fails > hc.maxfails

		if wasDown && fails == hc.maxfails+1 {
			hc.log("[헬스체크] %s 다운 판정! (fails=%d > maxfails=%d)",
				u.addr, fails, hc.maxfails)
		}
	}
}

func (hc *HealthChecker) log(format string, args ...interface{}) {
	hc.logMu.Lock()
	defer hc.logMu.Unlock()
	msg := fmt.Sprintf(format, args...)
	hc.logEntries = append(hc.logEntries, msg)
	fmt.Println(msg)
}

// ForwardBalancer는 CoreDNS forward 플러그인의 라운드로빈 로드밸런서를 시뮬레이션한다.
// Down(maxfails) 체크로 다운된 서버를 제외한다.
type ForwardBalancer struct {
	upstreams []*Upstream
	maxfails  uint32
}

// SelectUpstream은 사용 가능한 업스트림을 선택한다.
// CoreDNS forward.go의 ServeDNS 로직을 단순화:
//
//	for ... {
//	    proxy := list[i]
//	    if proxy.Down(f.maxfails) {
//	        fails++
//	        if fails < len(f.proxies) { continue }
//	        // 모두 다운 → SERVFAIL
//	        break
//	    }
//	    // 이 proxy로 쿼리 전송
//	}
func (fb *ForwardBalancer) SelectUpstream() *Upstream {
	// 사용 가능한 서버 중 랜덤 선택
	available := make([]*Upstream, 0)
	for _, u := range fb.upstreams {
		if !u.Down(fb.maxfails) {
			available = append(available, u)
		}
	}
	if len(available) == 0 {
		return nil // 모든 업스트림 다운 → SERVFAIL
	}
	return available[rand.Intn(len(available))]
}

// ResolveQuery는 DNS 쿼리를 시뮬레이션한다.
func (fb *ForwardBalancer) ResolveQuery(domain string) string {
	u := fb.SelectUpstream()
	if u == nil {
		return fmt.Sprintf("[쿼리] %s → SERVFAIL (사용 가능한 업스트림 없음)", domain)
	}
	return fmt.Sprintf("[쿼리] %s → %s 응답 성공", domain, u.addr)
}

func main() {
	fmt.Println("=== CoreDNS 업스트림 헬스체크 PoC ===")
	fmt.Println()
	fmt.Println("CoreDNS forward 플러그인의 헬스체크 메커니즘을 시뮬레이션합니다.")
	fmt.Println("참조: plugin/pkg/proxy/proxy.go, plugin/pkg/proxy/health.go")
	fmt.Println()

	// 3개 업스트림 서버 생성
	servers := []*Upstream{
		NewUpstream("8.8.8.8:53"),
		NewUpstream("8.8.4.4:53"),
		NewUpstream("1.1.1.1:53"),
	}

	maxfails := uint32(2)
	hcInterval := 200 * time.Millisecond // 데모용 (실제: 500ms)

	fmt.Printf("설정: maxfails=%d, hcInterval=%v\n", maxfails, hcInterval)
	fmt.Printf("업스트림: %s, %s, %s\n\n", servers[0].addr, servers[1].addr, servers[2].addr)

	// 헬스체커 시작
	hc := NewHealthChecker(servers, maxfails, hcInterval)
	hc.Start()

	balancer := &ForwardBalancer{upstreams: servers, maxfails: maxfails}

	// --- Phase 1: 모든 서버 정상 ---
	fmt.Println("--- Phase 1: 모든 서버 정상 ---")
	time.Sleep(300 * time.Millisecond)

	for i := 0; i < 5; i++ {
		result := balancer.ResolveQuery("example.com")
		fmt.Println(result)
	}
	printStatus(servers, maxfails)

	// --- Phase 2: 8.8.4.4 다운 ---
	fmt.Println("\n--- Phase 2: 8.8.4.4 다운 시뮬레이션 ---")
	servers[1].healthy.Store(false)
	fmt.Println("[이벤트] 8.8.4.4:53 서버 다운!")

	// maxfails(2)번 이상 실패해야 다운 판정 → 최소 3회 체크 대기
	time.Sleep(800 * time.Millisecond)

	fmt.Println("\n다운 판정 후 쿼리 결과:")
	queryResults := make(map[string]int)
	for i := 0; i < 10; i++ {
		u := balancer.SelectUpstream()
		if u != nil {
			queryResults[u.addr]++
		}
	}
	for addr, count := range queryResults {
		fmt.Printf("  %s → %d회 선택됨\n", addr, count)
	}
	if _, ok := queryResults["8.8.4.4:53"]; !ok {
		fmt.Println("  ✓ 8.8.4.4:53은 자동 제외됨 (Down=true)")
	}
	printStatus(servers, maxfails)

	// --- Phase 3: 서버 복구 ---
	fmt.Println("\n--- Phase 3: 8.8.4.4 복구 ---")
	servers[1].healthy.Store(true)
	fmt.Println("[이벤트] 8.8.4.4:53 서버 복구!")

	// 헬스체크가 성공하면 fails=0으로 리셋
	time.Sleep(500 * time.Millisecond)

	fmt.Println("\n복구 후 쿼리 결과:")
	queryResults = make(map[string]int)
	for i := 0; i < 12; i++ {
		u := balancer.SelectUpstream()
		if u != nil {
			queryResults[u.addr]++
		}
	}
	for addr, count := range queryResults {
		fmt.Printf("  %s → %d회 선택됨\n", addr, count)
	}
	if _, ok := queryResults["8.8.4.4:53"]; ok {
		fmt.Println("  ✓ 8.8.4.4:53이 다시 포함됨 (fails 리셋)")
	}
	printStatus(servers, maxfails)

	// --- Phase 4: 모든 서버 다운 ---
	fmt.Println("\n--- Phase 4: 모든 서버 다운 (SERVFAIL 시나리오) ---")
	for _, s := range servers {
		s.healthy.Store(false)
	}
	fmt.Println("[이벤트] 모든 서버 다운!")

	time.Sleep(800 * time.Millisecond)

	result := balancer.ResolveQuery("example.com")
	fmt.Println(result)
	printStatus(servers, maxfails)

	// 정리
	hc.Stop()

	fmt.Println("\n=== 헬스체크 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 정리:")
	fmt.Println("1. fails > maxfails일 때 Down() → true → 쿼리 대상에서 제외")
	fmt.Println("2. 헬스체크 성공 시 fails=0 리셋 → 자동 복구")
	fmt.Println("3. 모든 업스트림 다운 → SERVFAIL 반환")
	fmt.Println("4. maxfails=0이면 헬스체크 비활성화 (항상 Down=false)")
}

func printStatus(servers []*Upstream, maxfails uint32) {
	fmt.Println("\n  서버 상태:")
	for _, s := range servers {
		fails := atomic.LoadUint32(&s.fails)
		down := s.Down(maxfails)
		status := "UP"
		if down {
			status = "DOWN"
		}
		fmt.Printf("    %s: fails=%d, Down=%v → %s\n", s.addr, fails, down, status)
	}
}
