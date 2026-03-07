// poc-config-debounce: Istio 설정 변경 디바운싱과 병합 시뮬레이션
//
// Istio Pilot(istiod)은 Kubernetes로부터 설정 변경 이벤트를 수신할 때마다 xDS 푸시를 트리거한다.
// 그러나 대규모 클러스터에서는 짧은 시간에 수백 개의 설정 변경이 발생할 수 있다.
// 매번 푸시하면 성능이 급격히 저하되므로, Pilot은 "디바운싱" 메커니즘을 사용하여
// 여러 변경을 하나의 푸시로 병합한다.
//
// 핵심 참조:
//   - pilot/pkg/xds/discovery.go — debounce() 함수, DebounceOptions 구조체
//   - pkg/util/concurrent/debouncer.go — 범용 디바운서 (sets.Set 기반 이벤트 병합)
//   - pilot/pkg/model/push_context.go — PushRequest.Merge() 함수
//
// 디바운스 알고리즘:
//   1. 첫 번째 이벤트 도착 → DebounceAfter(quiet period) 타이머 시작
//   2. 타이머 만료 전 새 이벤트 도착 → 이벤트 병합, 타이머 리셋
//   3. quiet period 동안 새 이벤트 없음 → 병합된 푸시 실행
//   4. 단, debounceMax 초과 → quiet period 무시하고 강제 푸시

package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. 설정 타입과 트리거 (Istio 모델 시뮬레이션)
// ============================================================================

// TriggerReason은 푸시를 트리거한 이유를 나타낸다.
// Istio 소스: pilot/pkg/model/push_context.go
//
//	type TriggerReason string
//	const (
//	    EndpointUpdate TriggerReason = "endpoint"
//	    ConfigUpdate   TriggerReason = "config"
//	    ServiceUpdate  TriggerReason = "service"
//	    ...
//	)
type TriggerReason string

const (
	EndpointUpdate TriggerReason = "endpoint"
	ConfigUpdate   TriggerReason = "config"
	ServiceUpdate  TriggerReason = "service"
	SecretTrigger  TriggerReason = "secret"
	ProxyRequest   TriggerReason = "proxyrequest"
)

// ConfigKey는 변경된 설정의 식별자를 나타낸다.
// Istio 소스: pilot/pkg/model/push_context.go
//
//	type ConfigKey struct {
//	    Kind      kind.Kind
//	    Name      string
//	    Namespace string
//	}
type ConfigKey struct {
	Kind      string
	Name      string
	Namespace string
}

func (ck ConfigKey) String() string {
	return fmt.Sprintf("%s/%s/%s", ck.Kind, ck.Namespace, ck.Name)
}

// ReasonStats는 트리거 이유별 카운트를 추적한다.
// Istio 소스: pilot/pkg/model/push_context.go — type ReasonStats map[TriggerReason]int
type ReasonStats map[TriggerReason]int

// Merge는 다른 ReasonStats를 병합한다.
func (rs ReasonStats) Merge(other ReasonStats) {
	for k, v := range other {
		rs[k] += v
	}
}

// ============================================================================
// 2. PushRequest (xDS 푸시 요청)
// ============================================================================

// PushRequest는 Istio의 model.PushRequest를 시뮬레이션한다.
// 여러 설정 변경이 디바운싱 동안 하나의 PushRequest로 병합된다.
//
// Istio 소스: pilot/pkg/model/push_context.go:358-394
//
//	type PushRequest struct {
//	    Full            bool
//	    ConfigsUpdated  sets.Set[ConfigKey]
//	    Push            *PushContext
//	    Start           time.Time
//	    Reason          ReasonStats
//	    Forced          bool
//	}
type PushRequest struct {
	Full           bool                // 전체 푸시 필요 여부
	Forced         bool                // 강제 푸시 여부
	ConfigsUpdated map[ConfigKey]bool  // 변경된 설정 키 집합
	Reason         ReasonStats         // 트리거 이유별 카운트
	Start          time.Time           // 첫 번째 이벤트 시간
}

// Merge는 두 PushRequest를 병합한다.
// Istio 소스: pilot/pkg/model/push_context.go:492-534
//
// 핵심 병합 규칙:
//   - Start: 더 이른(오래된) 시간 유지
//   - Full: 둘 중 하나라도 Full이면 Full
//   - Forced: 둘 중 하나라도 Forced이면 Forced
//   - ConfigsUpdated: 합집합
//   - Reason: 카운트 합산 (중복 제거하지 않음 — 과소 집계 방지)
func (pr *PushRequest) Merge(other *PushRequest) *PushRequest {
	if pr == nil {
		return other
	}
	if other == nil {
		return pr
	}

	// 더 이른 시작 시간 유지
	// Istio 소스: "Keep the first (older) start time"

	// Reason 병합 — "Note that we shouldn't deduplicate here, or we would under count"
	if len(other.Reason) > 0 {
		if pr.Reason == nil {
			pr.Reason = make(ReasonStats)
		}
		pr.Reason.Merge(other.Reason)
	}

	// Full 병합 — "If either is full we need a full push"
	pr.Full = pr.Full || other.Full

	// Forced 병합 — "If either is forced we need a forced push"
	pr.Forced = pr.Forced || other.Forced

	// ConfigsUpdated 병합 (합집합)
	if pr.ConfigsUpdated == nil {
		pr.ConfigsUpdated = other.ConfigsUpdated
	} else if other.ConfigsUpdated != nil {
		for k, v := range other.ConfigsUpdated {
			pr.ConfigsUpdated[k] = v
		}
	}

	return pr
}

// String은 PushRequest의 요약 문자열을 반환한다.
func (pr *PushRequest) String() string {
	if pr == nil {
		return "<nil>"
	}
	configs := make([]string, 0, len(pr.ConfigsUpdated))
	for k := range pr.ConfigsUpdated {
		configs = append(configs, k.String())
	}
	reasons := make([]string, 0, len(pr.Reason))
	for r, cnt := range pr.Reason {
		reasons = append(reasons, fmt.Sprintf("%s(%d)", r, cnt))
	}
	return fmt.Sprintf("Full=%v, Forced=%v, Configs=[%s], Reasons=[%s]",
		pr.Full, pr.Forced, strings.Join(configs, ", "), strings.Join(reasons, ", "))
}

// ============================================================================
// 3. DebounceOptions (디바운스 설정)
// ============================================================================

// DebounceOptions는 Istio의 xds.DebounceOptions를 시뮬레이션한다.
// Istio 소스: pilot/pkg/xds/discovery.go:46-62
//
//	type DebounceOptions struct {
//	    DebounceAfter     time.Duration  // quiet period
//	    debounceMax       time.Duration  // max wait
//	    enableEDSDebounce bool
//	}
type DebounceOptions struct {
	DebounceAfter     time.Duration // quiet period — 마지막 이벤트 후 이 시간 동안 새 이벤트 없으면 푸시
	DebounceMax       time.Duration // max wait — 이 시간을 초과하면 무조건 푸시
	EnableEDSDebounce bool          // EDS 푸시도 디바운스할지 여부
}

// ============================================================================
// 4. 디바운서 (핵심 알고리즘)
// ============================================================================

// Debouncer는 Istio의 debounce() 함수를 시뮬레이션한다.
// Istio 소스: pilot/pkg/xds/discovery.go:343-425
//
// 알고리즘 흐름:
//   1. pushChannel에서 PushRequest 수신
//   2. 첫 이벤트 시 DebounceAfter 타이머 시작 (startDebounce 기록)
//   3. 새 이벤트 도착 → req.Merge(r)로 병합, lastConfigUpdateTime 갱신
//   4. 타이머 만료 시 pushWorker() 호출:
//      - eventDelay >= debounceMax → 강제 푸시
//      - quietTime >= DebounceAfter → 안정 상태, 푸시 실행
//      - 그 외 → 타이머 재설정 (DebounceAfter - quietTime)
//   5. 푸시 완료 후 freeCh로 알림 → free=true → 대기 중인 이벤트 처리
type Debouncer struct {
	opts DebounceOptions

	// 통계
	mu              sync.Mutex
	totalEvents     int
	totalPushes     int
	totalMerged     int
	pushHistory     []PushRecord
}

// PushRecord는 실행된 푸시의 기록이다.
type PushRecord struct {
	PushNumber     int
	EventCount     int
	WaitDuration   time.Duration
	Request        *PushRequest
	Trigger        string // "quiet" or "maxwait"
}

// NewDebouncer는 디바운서를 생성한다.
func NewDebouncer(opts DebounceOptions) *Debouncer {
	return &Debouncer{
		opts: opts,
	}
}

// Run은 디바운스 루프를 실행한다.
// Istio 소스: pilot/pkg/xds/discovery.go:343-425의 debounce() 함수를 충실히 재현
//
// 핵심 변수:
//   - timeChan: DebounceAfter 타이머
//   - startDebounce: 디바운스 시작 시간 (첫 이벤트 도착 시각)
//   - lastConfigUpdateTime: 마지막 이벤트 도착 시각
//   - req: 병합된 PushRequest
//   - free: 이전 푸시가 완료되었는지 여부
//   - freeCh: 푸시 완료 알림 채널
func (d *Debouncer) Run(
	ch chan *PushRequest,
	stopCh <-chan struct{},
	pushFn func(req *PushRequest),
) {
	var timeChan <-chan time.Time
	var startDebounce time.Time
	var lastConfigUpdateTime time.Time

	pushCounter := 0
	debouncedEvents := 0

	// 병합된 푸시 요청 추적
	// Istio 소스: "Keeps track of the push requests. If updates are debounce they will be merged."
	var req *PushRequest

	// free: 이전 푸시가 완료되었는지 추적
	// 푸시가 진행 중이면 free=false → 타이머 만료되어도 대기
	free := true
	freeCh := make(chan struct{}, 1)

	// push: 실제 푸시 실행 (고루틴으로 비동기 실행)
	push := func(req *PushRequest, debouncedEvents int, startDebounce time.Time, trigger string) {
		d.mu.Lock()
		d.totalPushes++
		d.totalMerged += debouncedEvents
		record := PushRecord{
			PushNumber:   d.totalPushes,
			EventCount:   debouncedEvents,
			WaitDuration: time.Since(startDebounce),
			Request:      req,
			Trigger:      trigger,
		}
		d.pushHistory = append(d.pushHistory, record)
		d.mu.Unlock()

		pushFn(req)
		freeCh <- struct{}{}
	}

	// pushWorker: 푸시 조건 평가 및 실행
	// Istio 소스: discovery.go:364-388
	pushWorker := func() {
		eventDelay := time.Since(startDebounce)
		quietTime := time.Since(lastConfigUpdateTime)

		// "it has been too long or quiet enough"
		// Istio 소스: if eventDelay >= opts.debounceMax || quietTime >= opts.DebounceAfter
		if eventDelay >= d.opts.DebounceMax || quietTime >= d.opts.DebounceAfter {
			if req != nil {
				pushCounter++

				trigger := "quiet"
				if eventDelay >= d.opts.DebounceMax {
					trigger = "maxwait"
				}

				configCount := len(req.ConfigsUpdated)
				fmt.Printf("  [푸시 #%d] %d개 이벤트 병합 (trigger=%s, 대기=%v, configs=%d, full=%v)\n",
					pushCounter, debouncedEvents, trigger,
					time.Since(startDebounce).Round(time.Millisecond),
					configCount, req.Full)

				free = false
				go push(req, debouncedEvents, startDebounce, trigger)
				req = nil
				debouncedEvents = 0
			}
		} else {
			// quiet period 아직 안 끝남 → 남은 시간만큼 타이머 재설정
			// Istio 소스: timeChan = time.After(opts.DebounceAfter - quietTime)
			remaining := d.opts.DebounceAfter - quietTime
			timeChan = time.After(remaining)
		}
	}

	for {
		select {
		case <-freeCh:
			// 이전 푸시 완료 → 대기 중인 이벤트가 있으면 처리
			free = true
			pushWorker()

		case r := <-ch:
			// 새 이벤트 도착
			// Istio 소스: discovery.go:395-416
			d.mu.Lock()
			d.totalEvents++
			d.mu.Unlock()

			// EDS 전용 이벤트: 디바운스하지 않고 즉시 푸시
			// Istio 소스: if !opts.enableEDSDebounce && !r.Full { ... go func() { pushFn(req) }() }
			if !d.opts.EnableEDSDebounce && !r.Full {
				go func(req *PushRequest) {
					fmt.Printf("  [EDS 즉시푸시] 디바운스 없이 즉시 실행 (configs=%d)\n",
						len(req.ConfigsUpdated))
					pushFn(req)
				}(r)
				continue
			}

			lastConfigUpdateTime = time.Now()
			if debouncedEvents == 0 {
				// 첫 이벤트 → DebounceAfter 타이머 시작
				// Istio 소스: timeChan = time.After(opts.DebounceAfter)
				timeChan = time.After(d.opts.DebounceAfter)
				startDebounce = lastConfigUpdateTime
			}
			debouncedEvents++

			// 이벤트 병합
			// Istio 소스: req = req.Merge(r)
			req = req.Merge(r)

		case <-timeChan:
			// quiet period 타이머 만료
			if free {
				pushWorker()
			}
			// free가 false면 이전 푸시 진행 중 → freeCh 대기

		case <-stopCh:
			return
		}
	}
}

// GetStats는 디바운서 통계를 반환한다.
func (d *Debouncer) GetStats() (events, pushes, merged int, history []PushRecord) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.totalEvents, d.totalPushes, d.totalMerged, d.pushHistory
}

// ============================================================================
// 5. 시뮬레이션 헬퍼
// ============================================================================

func printHeader(title string) {
	sep := strings.Repeat("=", 80)
	fmt.Println()
	fmt.Println(sep)
	fmt.Printf("  %s\n", title)
	fmt.Println(sep)
}

func printSubHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

func makePushRequest(full bool, reason TriggerReason, configs ...ConfigKey) *PushRequest {
	configSet := make(map[ConfigKey]bool)
	for _, c := range configs {
		configSet[c] = true
	}
	return &PushRequest{
		Full:           full,
		ConfigsUpdated: configSet,
		Reason:         ReasonStats{reason: 1},
		Start:          time.Now(),
	}
}

// ============================================================================
// 6. 시뮬레이션 실행
// ============================================================================

func main() {
	printHeader("Istio 설정 변경 디바운싱 & 병합 PoC")
	fmt.Println()
	fmt.Println("  Pilot(istiod)은 Kubernetes 설정 변경 이벤트를 수신하면 xDS 푸시를 트리거한다.")
	fmt.Println("  대규모 클러스터에서 짧은 시간에 수백 개 변경이 발생할 수 있으므로,")
	fmt.Println("  디바운싱으로 여러 변경을 하나의 푸시로 병합한다.")
	fmt.Println()
	fmt.Println("  참조: pilot/pkg/xds/discovery.go — debounce() 함수")
	fmt.Println("        pilot/pkg/model/push_context.go — PushRequest.Merge()")
	fmt.Println("        pkg/util/concurrent/debouncer.go — 범용 Debouncer")

	// ========================================================================
	// 시나리오 1: 기본 디바운싱 — quiet period 동안 이벤트 병합
	// ========================================================================
	printHeader("시나리오 1: 기본 디바운싱 — quiet period 동안 이벤트 병합")
	fmt.Println()
	fmt.Println("  설정: DebounceAfter=100ms (quiet period), DebounceMax=10s (max wait)")
	fmt.Println("  이벤트: 50ms 간격으로 5개 설정 변경 전송")
	fmt.Println("  기대 결과: quiet period(100ms) 내에 모든 이벤트 도착 → 1회 푸시로 병합")
	fmt.Println()

	pushCh1 := make(chan *PushRequest, 100)
	stopCh1 := make(chan struct{})
	pushResults1 := make([]*PushRequest, 0)
	var mu1 sync.Mutex

	debouncer1 := NewDebouncer(DebounceOptions{
		DebounceAfter:     100 * time.Millisecond,
		DebounceMax:       10 * time.Second,
		EnableEDSDebounce: true,
	})

	go debouncer1.Run(pushCh1, stopCh1, func(req *PushRequest) {
		mu1.Lock()
		pushResults1 = append(pushResults1, req)
		mu1.Unlock()
	})

	// 50ms 간격으로 5개 설정 변경 전송
	configs1 := []ConfigKey{
		{Kind: "VirtualService", Name: "reviews-vs", Namespace: "default"},
		{Kind: "DestinationRule", Name: "reviews-dr", Namespace: "default"},
		{Kind: "Gateway", Name: "bookinfo-gw", Namespace: "default"},
		{Kind: "ServiceEntry", Name: "external-api", Namespace: "default"},
		{Kind: "VirtualService", Name: "ratings-vs", Namespace: "default"},
	}

	for i, cfg := range configs1 {
		req := makePushRequest(true, ConfigUpdate, cfg)
		fmt.Printf("  [t=%3dms] 이벤트 %d: %s 변경\n", i*50, i+1, cfg.String())
		pushCh1 <- req
		if i < len(configs1)-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	// 푸시 완료 대기
	time.Sleep(300 * time.Millisecond)
	close(stopCh1)

	events1, pushes1, merged1, history1 := debouncer1.GetStats()
	fmt.Println()
	fmt.Printf("  결과: 이벤트 %d개 → 푸시 %d회 (병합된 이벤트: %d개)\n", events1, pushes1, merged1)
	if len(history1) > 0 {
		fmt.Printf("  푸시 내용: %s\n", history1[0].Request.String())
	}

	// ========================================================================
	// 시나리오 2: 타이머 리셋 — 새 이벤트가 quiet period을 연장
	// ========================================================================
	printHeader("시나리오 2: 타이머 리셋 — 새 이벤트가 quiet period을 연장")
	fmt.Println()
	fmt.Println("  설정: DebounceAfter=150ms (quiet period), DebounceMax=10s")
	fmt.Println("  이벤트: 100ms 간격으로 4개 전송 (매번 quiet period 내에 도착)")
	fmt.Println("  기대 결과: 각 이벤트가 타이머를 리셋 → 마지막 이벤트 후 150ms에 1회 푸시")
	fmt.Println()
	fmt.Println("  타임라인:")
	fmt.Println("    t=0ms    이벤트1 → 타이머 시작 (150ms)")
	fmt.Println("    t=100ms  이벤트2 → quiet period 미충족, 타이머 재설정")
	fmt.Println("    t=200ms  이벤트3 → quiet period 미충족, 타이머 재설정")
	fmt.Println("    t=300ms  이벤트4 → quiet period 미충족, 타이머 재설정")
	fmt.Println("    t=450ms  타이머 만료 → 4개 이벤트 병합 푸시")
	fmt.Println()

	pushCh2 := make(chan *PushRequest, 100)
	stopCh2 := make(chan struct{})

	debouncer2 := NewDebouncer(DebounceOptions{
		DebounceAfter:     150 * time.Millisecond,
		DebounceMax:       10 * time.Second,
		EnableEDSDebounce: true,
	})

	go debouncer2.Run(pushCh2, stopCh2, func(req *PushRequest) {
		// 푸시 실행
	})

	reasons := []TriggerReason{ConfigUpdate, ServiceUpdate, EndpointUpdate, SecretTrigger}
	for i := 0; i < 4; i++ {
		cfg := ConfigKey{Kind: "Config", Name: fmt.Sprintf("resource-%d", i+1), Namespace: "ns"}
		req := makePushRequest(true, reasons[i], cfg)
		fmt.Printf("  [t=%3dms] 이벤트 %d: %s (reason=%s)\n", i*100, i+1, cfg.String(), reasons[i])
		pushCh2 <- req
		if i < 3 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	time.Sleep(400 * time.Millisecond)
	close(stopCh2)

	events2, pushes2, merged2, history2 := debouncer2.GetStats()
	fmt.Println()
	fmt.Printf("  결과: 이벤트 %d개 → 푸시 %d회 (병합된 이벤트: %d개)\n", events2, pushes2, merged2)
	if len(history2) > 0 {
		fmt.Printf("  병합된 Reason: %s\n", history2[0].Request.String())
		fmt.Printf("  총 대기 시간: %v\n", history2[0].WaitDuration.Round(time.Millisecond))
	}

	// ========================================================================
	// 시나리오 3: DebounceMax 강제 푸시 — 이벤트가 계속 들어와도 최대 대기 시간 초과 시 강제 푸시
	// ========================================================================
	printHeader("시나리오 3: DebounceMax 강제 푸시")
	fmt.Println()
	fmt.Println("  설정: DebounceAfter=100ms, DebounceMax=500ms")
	fmt.Println("  이벤트: 80ms 간격으로 계속 전송 (quiet period 충족 불가)")
	fmt.Println("  기대 결과: 500ms(debounceMax) 경과 시 강제 푸시, 이후 남은 이벤트 별도 푸시")
	fmt.Println()
	fmt.Println("  Istio 소스: discovery.go:368")
	fmt.Println("    if eventDelay >= opts.debounceMax || quietTime >= opts.DebounceAfter {")
	fmt.Println("        // 강제 푸시 실행")
	fmt.Println("    }")
	fmt.Println()

	pushCh3 := make(chan *PushRequest, 100)
	stopCh3 := make(chan struct{})

	debouncer3 := NewDebouncer(DebounceOptions{
		DebounceAfter:     100 * time.Millisecond,
		DebounceMax:       500 * time.Millisecond,
		EnableEDSDebounce: true,
	})

	go debouncer3.Run(pushCh3, stopCh3, func(req *PushRequest) {
		// 푸시 실행 시뮬레이션 (약간의 처리 시간)
		time.Sleep(10 * time.Millisecond)
	})

	// 80ms 간격으로 10개 이벤트 전송 (총 720ms — debounceMax 500ms 초과)
	startTime := time.Now()
	for i := 0; i < 10; i++ {
		elapsed := time.Since(startTime).Milliseconds()
		cfg := ConfigKey{
			Kind:      "VirtualService",
			Name:      fmt.Sprintf("svc-%d", i+1),
			Namespace: "production",
		}
		req := makePushRequest(true, ConfigUpdate, cfg)
		fmt.Printf("  [t=%3dms] 이벤트 %02d: %s\n", elapsed, i+1, cfg.String())
		pushCh3 <- req
		time.Sleep(80 * time.Millisecond)
	}

	time.Sleep(300 * time.Millisecond)
	close(stopCh3)

	events3, pushes3, merged3, history3 := debouncer3.GetStats()
	fmt.Println()
	fmt.Printf("  결과: 이벤트 %d개 → 푸시 %d회 (병합된 이벤트: %d개)\n", events3, pushes3, merged3)
	for _, h := range history3 {
		fmt.Printf("    푸시 #%d: %d개 이벤트, trigger=%s, 대기=%v\n",
			h.PushNumber, h.EventCount, h.Trigger, h.WaitDuration.Round(time.Millisecond))
	}

	// ========================================================================
	// 시나리오 4: EDS 이벤트 즉시 푸시 (디바운스 비활성화)
	// ========================================================================
	printHeader("시나리오 4: EDS 이벤트 즉시 푸시 (디바운스 비활성화)")
	fmt.Println()
	fmt.Println("  설정: EnableEDSDebounce=false")
	fmt.Println("  이벤트: Full=true(설정변경) + Full=false(EDS) 혼합 전송")
	fmt.Println("  기대 결과: Full=false(EDS)는 즉시 푸시, Full=true는 디바운싱")
	fmt.Println()
	fmt.Println("  Istio 소스: discovery.go:400-407")
	fmt.Println("    if !opts.enableEDSDebounce && !r.Full {")
	fmt.Println("        go func(req *model.PushRequest) {")
	fmt.Println("            pushFn(req)")
	fmt.Println("            updateSent.Inc()")
	fmt.Println("        }(r)")
	fmt.Println("        continue  // 디바운스 로직 스킵")
	fmt.Println("    }")
	fmt.Println()

	pushCh4 := make(chan *PushRequest, 100)
	stopCh4 := make(chan struct{})
	var edsPushCount, fullPushCount int32
	var countMu sync.Mutex

	debouncer4 := NewDebouncer(DebounceOptions{
		DebounceAfter:     100 * time.Millisecond,
		DebounceMax:       10 * time.Second,
		EnableEDSDebounce: false, // EDS 디바운스 비활성화
	})

	go debouncer4.Run(pushCh4, stopCh4, func(req *PushRequest) {
		countMu.Lock()
		if req.Full {
			fullPushCount++
		} else {
			edsPushCount++
		}
		countMu.Unlock()
	})

	// EDS(incremental)와 Full 이벤트 혼합 전송
	events := []struct {
		full   bool
		reason TriggerReason
		name   string
	}{
		{true, ConfigUpdate, "vs-update"},
		{false, EndpointUpdate, "eds-1"},  // EDS → 즉시 푸시
		{true, ConfigUpdate, "dr-update"},
		{false, EndpointUpdate, "eds-2"},  // EDS → 즉시 푸시
		{false, EndpointUpdate, "eds-3"},  // EDS → 즉시 푸시
		{true, ServiceUpdate, "svc-update"},
	}

	for i, e := range events {
		cfg := ConfigKey{Kind: "Config", Name: e.name, Namespace: "default"}
		req := makePushRequest(e.full, e.reason, cfg)
		pushType := "Full"
		if !e.full {
			pushType = "EDS(incremental)"
		}
		fmt.Printf("  이벤트 %d: %s (%s, reason=%s)\n", i+1, e.name, pushType, e.reason)
		pushCh4 <- req
		time.Sleep(30 * time.Millisecond)
	}

	time.Sleep(300 * time.Millisecond)
	close(stopCh4)

	events4, pushes4, _, _ := debouncer4.GetStats()
	countMu.Lock()
	edsCount := edsPushCount
	fullCount := fullPushCount
	countMu.Unlock()

	fmt.Println()
	fmt.Printf("  결과: 총 이벤트 %d개 → 디바운싱 대상 %d개\n", len(events), events4)
	fmt.Printf("    EDS 즉시 푸시: %d회 (디바운스 스킵)\n", edsCount)
	fmt.Printf("    Full 디바운스 푸시: %d회\n", fullCount)
	fmt.Printf("    총 푸시: %d회\n", pushes4+int(edsCount))

	// ========================================================================
	// 시나리오 5: PushRequest.Merge() 동작 검증
	// ========================================================================
	printHeader("시나리오 5: PushRequest.Merge() 동작 검증")
	fmt.Println()
	fmt.Println("  Istio 소스: pilot/pkg/model/push_context.go:492-534")
	fmt.Println()

	req1 := &PushRequest{
		Full:   false,
		Forced: false,
		ConfigsUpdated: map[ConfigKey]bool{
			{Kind: "VirtualService", Name: "vs-1", Namespace: "ns"}: true,
			{Kind: "DestinationRule", Name: "dr-1", Namespace: "ns"}: true,
		},
		Reason: ReasonStats{ConfigUpdate: 1, ServiceUpdate: 1},
		Start:  time.Now(),
	}

	req2 := &PushRequest{
		Full:   true, // Full push 요청
		Forced: false,
		ConfigsUpdated: map[ConfigKey]bool{
			{Kind: "Gateway", Name: "gw-1", Namespace: "ns"}:        true,
			{Kind: "VirtualService", Name: "vs-1", Namespace: "ns"}: true, // 중복
		},
		Reason: ReasonStats{ConfigUpdate: 1, EndpointUpdate: 1},
		Start:  time.Now(),
	}

	req3 := &PushRequest{
		Full:   false,
		Forced: true, // 강제 푸시
		ConfigsUpdated: map[ConfigKey]bool{
			{Kind: "ServiceEntry", Name: "se-1", Namespace: "ns"}: true,
		},
		Reason: ReasonStats{SecretTrigger: 1},
		Start:  time.Now(),
	}

	fmt.Println("  Request 1:")
	fmt.Printf("    %s\n", req1.String())
	fmt.Println("  Request 2:")
	fmt.Printf("    %s\n", req2.String())
	fmt.Println("  Request 3:")
	fmt.Printf("    %s\n", req3.String())

	// 순차 병합
	merged := req1.Merge(req2)
	merged = merged.Merge(req3)

	fmt.Println()
	fmt.Println("  병합 결과 (req1.Merge(req2).Merge(req3)):")
	fmt.Printf("    %s\n", merged.String())
	fmt.Println()
	fmt.Println("  검증:")
	fmt.Printf("    Full = %v (req2가 Full=true이므로 전체 병합 결과도 true)\n", merged.Full)
	fmt.Printf("    Forced = %v (req3가 Forced=true이므로 전체 병합 결과도 true)\n", merged.Forced)
	fmt.Printf("    ConfigsUpdated 수 = %d (중복 vs-1은 한 번만 포함)\n", len(merged.ConfigsUpdated))
	fmt.Printf("    Reason: config=%d, service=%d, endpoint=%d, secret=%d\n",
		merged.Reason[ConfigUpdate], merged.Reason[ServiceUpdate],
		merged.Reason[EndpointUpdate], merged.Reason[SecretTrigger])
	fmt.Println("    → Reason 카운트가 합산됨 (config: 1+1=2, 중복 제거하지 않음)")

	// ========================================================================
	// 디바운스 알고리즘 다이어그램
	// ========================================================================
	printHeader("디바운스 알고리즘 동작 원리")
	fmt.Println()
	fmt.Println("  시간 →")
	fmt.Println()
	fmt.Println("  이벤트:    E1      E2    E3         E4")
	fmt.Println("            │       │     │          │")
	fmt.Println("            v       v     v          v")
	fmt.Println("  ──────────┼───────┼─────┼──────────┼────────────────────")
	fmt.Println("            │       │     │          │")
	fmt.Println("  타이머:   [===]   │     │          │")
	fmt.Println("            리셋→  [===]  │          │")
	fmt.Println("                   리셋→ [===]       │")
	fmt.Println("                         리셋→ [====]│")
	fmt.Println("                              리셋→ [=======]→ PUSH!")
	fmt.Println("                                              │")
	fmt.Println("  병합:     E1 ──→ E1+E2 ──→ E1+E2+E3 ──→ E1+E2+E3+E4")
	fmt.Println("                                              │")
	fmt.Println("                                              v")
	fmt.Println("                                     병합된 PushRequest로")
	fmt.Println("                                     한 번에 xDS Push 실행")
	fmt.Println()
	fmt.Println("  quiet period (DebounceAfter) = [===]")
	fmt.Println()
	fmt.Println("  DebounceMax 강제 푸시:")
	fmt.Println()
	fmt.Println("  이벤트:  E1  E2  E3  E4  E5  E6  E7  E8  E9  ...")
	fmt.Println("           │   │   │   │   │   │   │   │   │")
	fmt.Println("  ─────────┼───┼───┼───┼───┼───┼───┼───┼───┼───────")
	fmt.Println("           │   │   │   │   │   │   │   │   │")
	fmt.Println("           [========= debounceMax =========]→ 강제 PUSH!")
	fmt.Println("           │                               │")
	fmt.Println("           startDebounce          이벤트가 계속 들어와")
	fmt.Println("                                  quiet period 충족 불가")
	fmt.Println("                                  → debounceMax 초과 시 강제 푸시")

	// ========================================================================
	// 요약
	// ========================================================================
	printHeader("요약: Istio 디바운싱 메커니즘")
	fmt.Println()
	fmt.Println("  1. DebounceAfter (quiet period, 기본 100ms)")
	fmt.Println("     - 마지막 이벤트 후 이 시간 동안 새 이벤트 없으면 푸시 실행")
	fmt.Println("     - 새 이벤트가 도착하면 타이머 리셋 (discovery.go:386)")
	fmt.Println("       timeChan = time.After(opts.DebounceAfter - quietTime)")
	fmt.Println()
	fmt.Println("  2. DebounceMax (최대 대기 시간, 기본 10s)")
	fmt.Println("     - 이벤트가 계속 들어와도 이 시간을 넘기면 강제 푸시")
	fmt.Println("     - eventDelay >= opts.debounceMax → 푸시 트리거 (discovery.go:368)")
	fmt.Println()
	fmt.Println("  3. PushRequest.Merge()")
	fmt.Println("     - ConfigsUpdated: 합집합 (변경된 모든 설정 포함)")
	fmt.Println("     - Full: OR 연산 (하나라도 Full이면 전체 푸시)")
	fmt.Println("     - Forced: OR 연산")
	fmt.Println("     - Reason: 카운트 합산 (과소 집계 방지)")
	fmt.Println("     - Start: 더 이른 시간 유지")
	fmt.Println()
	fmt.Println("  4. EDS 즉시 푸시")
	fmt.Println("     - enableEDSDebounce=false일 때 Full=false(EDS) 이벤트는 디바운스 없이 즉시 푸시")
	fmt.Println("     - 엔드포인트 변경은 지연 없이 빠르게 반영해야 하므로")
	fmt.Println()
	fmt.Println("  5. free 플래그와 직렬화")
	fmt.Println("     - 이전 푸시가 완료될 때까지 다음 푸시 대기 (freeCh 채널)")
	fmt.Println("     - 동시 푸시로 인한 레이스 컨디션 방지")
	fmt.Println()
}
