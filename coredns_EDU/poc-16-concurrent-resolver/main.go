package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ============================================================================
// CoreDNS 동시 DNS 리졸버 (경쟁 패턴) PoC
// ============================================================================
//
// CoreDNS forward 플러그인에서 여러 업스트림에 동시 쿼리를 보내고
// 가장 빠른 응답을 채택하는 "경쟁(race)" 패턴을 시뮬레이션한다.
//
// 실제 CoreDNS 구현 참조:
//   - plugin/forward/forward.go → ServeDNS()에서 프록시 순회, 타임아웃 처리
//   - plugin/forward/policy.go  → random, round_robin, sequential 정책
//   - plugin/pkg/proxy/proxy.go → Connect()를 통한 업스트림 연결
//
// 이 PoC에서 구현하는 패턴:
//   1. 여러 업스트림에 동시에 쿼리 전송 (goroutine + channel)
//   2. 가장 빠른 응답 채택 (select + context)
//   3. 나머지 쿼리 취소 (context.WithCancel)
//   4. 전체 타임아웃 처리 (context.WithTimeout)
//
// CoreDNS의 기본 동작은 순차적 시도이지만, 이 PoC는 동시 쿼리의
// 이점과 구현 패턴을 보여주기 위한 확장 시뮬레이션이다.
// ============================================================================

// DNSResponse는 업스트림의 DNS 응답을 나타낸다.
type DNSResponse struct {
	Server   string        // 응답한 서버 주소
	Answer   string        // 응답 내용
	Latency  time.Duration // 응답 지연시간
	Error    error         // 오류
}

// UpstreamServer는 업스트림 DNS 서버를 시뮬레이션한다.
type UpstreamServer struct {
	Addr       string
	BaseDelay  time.Duration // 기본 지연시간
	Jitter     time.Duration // 지연시간 변동폭
	FailRate   float64       // 실패 확률 (0.0 ~ 1.0)
	mu         sync.Mutex
	queryCount int
}

// Query는 DNS 쿼리를 시뮬레이션한다. context 취소를 존중한다.
func (us *UpstreamServer) Query(ctx context.Context, domain string) (*DNSResponse, error) {
	us.mu.Lock()
	us.queryCount++
	us.mu.Unlock()

	// 응답 지연시간 계산 (기본 + 랜덤 지터)
	delay := us.BaseDelay
	if us.Jitter > 0 {
		delay += time.Duration(rand.Int63n(int64(us.Jitter)))
	}

	// 지연시간 시뮬레이션 (context 취소 존중)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(delay):
	}

	// 실패 시뮬레이션
	if us.FailRate > 0 && rand.Float64() < us.FailRate {
		return nil, fmt.Errorf("서버 %s: 쿼리 실패 (시뮬레이션)", us.Addr)
	}

	return &DNSResponse{
		Server:  us.Addr,
		Answer:  fmt.Sprintf("%s → 93.184.216.34 (from %s)", domain, us.Addr),
		Latency: delay,
	}, nil
}

// ConcurrentResolver는 동시 쿼리 리졸버이다.
type ConcurrentResolver struct {
	Servers []*UpstreamServer
	Timeout time.Duration // 전체 타임아웃
}

// RaceResult는 경쟁 쿼리의 결과를 나타낸다.
type RaceResult struct {
	Winner     *DNSResponse   // 가장 빠른 응답
	Cancelled  int            // 취소된 쿼리 수
	Failed     int            // 실패한 쿼리 수
	TotalTime  time.Duration  // 전체 소요시간
}

// ResolveRace는 모든 업스트림에 동시에 쿼리를 보내고 가장 빠른 응답을 채택한다.
//
// 패턴:
//   1. context.WithTimeout으로 전체 타임아웃 설정
//   2. context.WithCancel로 취소 컨텍스트 생성
//   3. 각 업스트림에 대해 goroutine 실행
//   4. 첫 번째 성공 응답 수신 시 cancel() 호출 → 나머지 goroutine 취소
//   5. 모든 goroutine 종료 대기 (WaitGroup)
func (cr *ConcurrentResolver) ResolveRace(domain string) RaceResult {
	start := time.Now()

	// 전체 타임아웃 컨텍스트
	ctx, timeoutCancel := context.WithTimeout(context.Background(), cr.Timeout)
	defer timeoutCancel()

	// 취소 컨텍스트 - 첫 응답 수신 시 나머지 취소
	ctx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	type queryResult struct {
		resp *DNSResponse
		err  error
	}

	resultCh := make(chan queryResult, len(cr.Servers))
	var wg sync.WaitGroup

	// 모든 서버에 동시 쿼리 전송
	for _, server := range cr.Servers {
		wg.Add(1)
		go func(s *UpstreamServer) {
			defer wg.Done()
			resp, err := s.Query(ctx, domain)
			resultCh <- queryResult{resp: resp, err: err}
		}(server)
	}

	// WaitGroup 완료 시 채널 닫기
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// 결과 수집 - 첫 번째 성공 응답을 winner로 선택
	var winner *DNSResponse
	cancelled := 0
	failed := 0

	for qr := range resultCh {
		if qr.err != nil {
			if qr.err == context.Canceled {
				cancelled++
			} else {
				failed++
			}
			continue
		}

		if winner == nil {
			winner = qr.resp
			// 첫 번째 성공 응답 수신 → 나머지 취소
			raceCancel()
		}
	}

	return RaceResult{
		Winner:    winner,
		Cancelled: cancelled,
		Failed:    failed,
		TotalTime: time.Since(start),
	}
}

// ResolveSequential은 순차적 쿼리를 수행한다 (비교용).
// CoreDNS forward의 기본 동작에 가까움.
func (cr *ConcurrentResolver) ResolveSequential(domain string) RaceResult {
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), cr.Timeout)
	defer cancel()

	for _, server := range cr.Servers {
		resp, err := server.Query(ctx, domain)
		if err != nil {
			continue
		}
		return RaceResult{
			Winner:    resp,
			Cancelled: 0,
			Failed:    0,
			TotalTime: time.Since(start),
		}
	}

	return RaceResult{TotalTime: time.Since(start)}
}

func main() {
	fmt.Println("=== CoreDNS 동시 DNS 리졸버 (경쟁 패턴) PoC ===")
	fmt.Println()
	fmt.Println("여러 업스트림에 동시 쿼리를 보내고 가장 빠른 응답을 채택합니다.")
	fmt.Println("참조: plugin/forward/forward.go, plugin/pkg/proxy/proxy.go")
	fmt.Println()

	// --- 데모 1: 3개 서버에 동시 쿼리 ---
	fmt.Println("--- 데모 1: 동시 쿼리 (경쟁 패턴) ---")
	fmt.Println()

	servers := []*UpstreamServer{
		{Addr: "8.8.8.8:53", BaseDelay: 50 * time.Millisecond, Jitter: 30 * time.Millisecond},
		{Addr: "8.8.4.4:53", BaseDelay: 80 * time.Millisecond, Jitter: 40 * time.Millisecond},
		{Addr: "1.1.1.1:53", BaseDelay: 30 * time.Millisecond, Jitter: 20 * time.Millisecond},
	}

	resolver := &ConcurrentResolver{
		Servers: servers,
		Timeout: 2 * time.Second,
	}

	fmt.Println("서버 설정:")
	for _, s := range servers {
		fmt.Printf("  %s: 지연=%v, 지터=%v\n", s.Addr, s.BaseDelay, s.Jitter)
	}
	fmt.Println()

	// 5회 반복하여 경쟁 패턴 시연
	fmt.Println("5회 경쟁 쿼리 결과:")
	for i := 1; i <= 5; i++ {
		result := resolver.ResolveRace("example.com")
		if result.Winner != nil {
			fmt.Printf("  [%d] 승자: %s (지연: %v), 취소: %d개, 전체: %v\n",
				i, result.Winner.Server, result.Winner.Latency,
				result.Cancelled, result.TotalTime.Truncate(time.Millisecond))
		} else {
			fmt.Printf("  [%d] 모든 서버 실패, 전체: %v\n", i, result.TotalTime)
		}
	}

	// --- 데모 2: 순차 vs 동시 성능 비교 ---
	fmt.Println()
	fmt.Println("--- 데모 2: 순차 vs 동시 쿼리 성능 비교 ---")
	fmt.Println()

	// 순차 쿼리
	seqTimes := make([]time.Duration, 0, 5)
	raceTimes := make([]time.Duration, 0, 5)

	for i := 0; i < 5; i++ {
		// 순차
		seqResult := resolver.ResolveSequential("example.com")
		seqTimes = append(seqTimes, seqResult.TotalTime)

		// 동시
		raceResult := resolver.ResolveRace("example.com")
		raceTimes = append(raceTimes, raceResult.TotalTime)
	}

	seqAvg := avgDuration(seqTimes)
	raceAvg := avgDuration(raceTimes)

	fmt.Printf("  순차 쿼리 평균: %v\n", seqAvg.Truncate(time.Millisecond))
	fmt.Printf("  동시 쿼리 평균: %v\n", raceAvg.Truncate(time.Millisecond))
	if raceAvg < seqAvg {
		improvement := float64(seqAvg-raceAvg) / float64(seqAvg) * 100
		fmt.Printf("  → 동시 쿼리가 약 %.0f%% 빠름\n", improvement)
	}

	// --- 데모 3: 일부 서버 실패 시 ---
	fmt.Println()
	fmt.Println("--- 데모 3: 일부 서버 실패 시 경쟁 패턴 ---")
	fmt.Println()

	failServers := []*UpstreamServer{
		{Addr: "fail-1:53", BaseDelay: 20 * time.Millisecond, FailRate: 1.0}, // 항상 실패
		{Addr: "slow-ok:53", BaseDelay: 200 * time.Millisecond, Jitter: 50 * time.Millisecond},
		{Addr: "fast-ok:53", BaseDelay: 40 * time.Millisecond, Jitter: 10 * time.Millisecond},
	}

	failResolver := &ConcurrentResolver{
		Servers: failServers,
		Timeout: 2 * time.Second,
	}

	fmt.Println("서버 설정:")
	fmt.Printf("  fail-1:53  → 항상 실패 (FailRate=100%%)\n")
	fmt.Printf("  slow-ok:53 → 느린 정상 (200ms+지터)\n")
	fmt.Printf("  fast-ok:53 → 빠른 정상 (40ms+지터)\n")
	fmt.Println()

	for i := 1; i <= 3; i++ {
		result := failResolver.ResolveRace("example.com")
		if result.Winner != nil {
			fmt.Printf("  [%d] 승자: %s (지연: %v), 실패: %d개, 취소: %d개\n",
				i, result.Winner.Server, result.Winner.Latency,
				result.Failed, result.Cancelled)
		}
	}

	// --- 데모 4: 타임아웃 처리 ---
	fmt.Println()
	fmt.Println("--- 데모 4: 타임아웃 처리 ---")
	fmt.Println()

	slowServers := []*UpstreamServer{
		{Addr: "very-slow-1:53", BaseDelay: 3 * time.Second},
		{Addr: "very-slow-2:53", BaseDelay: 5 * time.Second},
	}

	timeoutResolver := &ConcurrentResolver{
		Servers: slowServers,
		Timeout: 500 * time.Millisecond, // 500ms 타임아웃
	}

	fmt.Printf("서버 지연: 3s, 5s / 타임아웃: 500ms\n")

	result := timeoutResolver.ResolveRace("example.com")
	if result.Winner != nil {
		fmt.Printf("  결과: 승자=%s\n", result.Winner.Server)
	} else {
		fmt.Printf("  결과: 타임아웃! (모든 서버 응답 전 %v 초과)\n", timeoutResolver.Timeout)
		fmt.Printf("  취소된 쿼리: %d개, 전체 소요: %v\n",
			result.Cancelled, result.TotalTime.Truncate(time.Millisecond))
	}

	// --- 데모 5: Context 취소 전파 확인 ---
	fmt.Println()
	fmt.Println("--- 데모 5: Context 취소 전파 확인 ---")
	fmt.Println()

	// 쿼리 카운트 초기화
	countServers := []*UpstreamServer{
		{Addr: "fast:53", BaseDelay: 10 * time.Millisecond},
		{Addr: "medium:53", BaseDelay: 100 * time.Millisecond},
		{Addr: "slow:53", BaseDelay: 500 * time.Millisecond},
	}

	countResolver := &ConcurrentResolver{
		Servers: countServers,
		Timeout: 2 * time.Second,
	}

	result = countResolver.ResolveRace("example.com")

	fmt.Println("경쟁 쿼리 후 서버별 상태:")
	for _, s := range countServers {
		s.mu.Lock()
		count := s.queryCount
		s.mu.Unlock()
		fmt.Printf("  %s: 쿼리 시작=%d회\n", s.Addr, count)
	}
	if result.Winner != nil {
		fmt.Printf("  → 승자: %s, 취소된 쿼리: %d개\n", result.Winner.Server, result.Cancelled)
		fmt.Println("  → context.Cancel()로 느린 서버의 쿼리가 조기 종료됨")
	}

	fmt.Println()
	fmt.Println("=== 동시 DNS 리졸버 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 정리:")
	fmt.Println("1. 경쟁 패턴: 모든 업스트림에 동시 쿼리 → 가장 빠른 응답 채택")
	fmt.Println("2. Context 취소: 첫 응답 수신 시 cancel() → 나머지 goroutine 종료")
	fmt.Println("3. 타임아웃: context.WithTimeout으로 전체 쿼리 시간 제한")
	fmt.Println("4. 일부 서버 실패해도 다른 서버가 응답하면 정상 처리")
	fmt.Println("5. 동시 쿼리는 순차 쿼리보다 p99 지연시간을 크게 줄일 수 있음")
}

func avgDuration(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range durations {
		total += d
	}
	return total / time.Duration(len(durations))
}
