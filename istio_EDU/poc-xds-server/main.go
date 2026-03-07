package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Istio xDS Discovery Service 시뮬레이션
//
// 실제 소스 참조:
//   - pilot/pkg/xds/discovery.go   → DiscoveryServer, debounce(), Push()
//   - pilot/pkg/xds/pushqueue.go   → PushQueue (Enqueue/Dequeue/MarkDone)
//   - pilot/pkg/xds/ads.go         → Connection, PushOrder, AdsPushAll()
//
// 핵심 알고리즘:
//   1. ConfigUpdate → pushChannel로 이벤트 전달
//   2. debounce 고루틴: 100ms quiet + 10s max 후 Push() 호출
//   3. Push() → PushQueue에 모든 연결된 프록시를 Enqueue
//   4. sendPushes 고루틴: PushQueue에서 Dequeue하여 각 프록시에 전달
//   5. 각 프록시에 CDS→EDS→LDS→RDS 순서로 설정 Push
// =============================================================================

// --- PushRequest: 설정 변경 요청 (pilot/pkg/model/push_request.go 참조) ---

// PushRequest는 설정 변경 이벤트를 나타낸다.
// 실제 Istio에서는 Full push와 Incremental push를 구분하며,
// 여러 이벤트를 병합(Merge)하여 불필요한 push를 줄인다.
type PushRequest struct {
	Full           bool              // 전체 push인지 증분 push인지
	ConfigsUpdated map[string]bool   // 변경된 설정 종류 (예: VirtualService, DestinationRule)
	Reason         map[string]int    // push 사유 및 횟수
	Start          time.Time         // push 시작 시간
}

// Merge는 두 PushRequest를 병합한다.
// 실제 Istio의 PushRequest.Merge() 메서드와 동일한 로직:
// - Full이 하나라도 true면 결과도 Full
// - ConfigsUpdated는 합집합
// - Reason은 카운트 합산
func (pr *PushRequest) Merge(other *PushRequest) *PushRequest {
	if pr == nil {
		return other
	}
	if other == nil {
		return pr
	}

	merged := &PushRequest{
		Full:           pr.Full || other.Full,
		Start:          pr.Start,
		ConfigsUpdated: make(map[string]bool),
		Reason:         make(map[string]int),
	}

	// ConfigsUpdated 합집합
	for k, v := range pr.ConfigsUpdated {
		merged.ConfigsUpdated[k] = v
	}
	for k, v := range other.ConfigsUpdated {
		merged.ConfigsUpdated[k] = v
	}

	// Reason 카운트 합산
	for k, v := range pr.Reason {
		merged.Reason[k] += v
	}
	for k, v := range other.Reason {
		merged.Reason[k] += v
	}

	return merged
}

// CopyMerge는 기존 요청에 새 요청을 병합한 사본을 반환한다.
// PushQueue에서 이미 pending 또는 processing 중인 연결에
// 새 이벤트가 오면 이 메서드로 병합한다.
func (pr *PushRequest) CopyMerge(other *PushRequest) *PushRequest {
	if pr == nil {
		return other
	}
	return pr.Merge(other)
}

// --- Connection: Envoy 프록시 연결 (pilot/pkg/xds/ads.go 참조) ---

// Connection은 하나의 Envoy 프록시와의 gRPC 스트림 연결을 나타낸다.
// 실제 Istio에서는 gRPC 양방향 스트림을 사용하지만,
// 여기서는 Go 채널로 시뮬레이션한다.
type Connection struct {
	id       string           // 연결 ID (예: "sidecar~10.0.0.1~pod-a~ns.svc.cluster.local-1")
	proxyID  string           // 프록시 식별자
	pushCh   chan *Event       // push 이벤트 채널 (gRPC 스트림 시뮬레이션)
	streamDone chan struct{}   // 스트림 종료 시그널
	watchedResources []string // 구독 중인 리소스 타입 (CDS, EDS, LDS, RDS)
}

// Event는 프록시에 전달되는 push 이벤트이다.
// 실제 Istio의 Event 구조체(ads.go)와 동일한 패턴:
// pushRequest + done 콜백으로 구성된다.
type Event struct {
	pushRequest *PushRequest
	done        func() // PushQueue.MarkDone()을 호출하는 콜백
}

// --- PushQueue: push 대기열 (pilot/pkg/xds/pushqueue.go 참조) ---

// PushQueue는 프록시별 push 요청을 관리하는 큐이다.
// 핵심 설계:
//   - pending: 아직 처리되지 않은 연결들 (같은 연결이 다시 Enqueue되면 병합)
//   - processing: Dequeue되었지만 아직 MarkDone되지 않은 연결들
//   - queue: pending 연결들의 순서를 유지하는 슬라이스
//
// 이 3가지 상태 머신으로 동일 프록시에 대한 중복 push를 방지하고,
// processing 중에 새 이벤트가 오면 MarkDone 후 자동 재큐잉한다.
type PushQueue struct {
	cond       *sync.Cond
	pending    map[*Connection]*PushRequest // 대기 중인 push
	queue      []*Connection               // 순서 유지용 큐
	processing map[*Connection]*PushRequest // 처리 중인 push (nil이면 새 이벤트 없음)
	shuttingDown bool
}

func NewPushQueue() *PushQueue {
	return &PushQueue{
		cond:       sync.NewCond(&sync.Mutex{}),
		pending:    make(map[*Connection]*PushRequest),
		processing: make(map[*Connection]*PushRequest),
	}
}

// Enqueue는 연결에 대한 push를 큐에 추가한다.
// 이미 pending이면 기존 요청에 병합하고,
// processing 중이면 processing 맵에 저장하여 MarkDone 시 재큐잉한다.
func (p *PushQueue) Enqueue(con *Connection, pushRequest *PushRequest) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.shuttingDown {
		return
	}

	// processing 중인 연결이면 병합하여 저장 (MarkDone 시 재큐잉됨)
	if request, f := p.processing[con]; f {
		p.processing[con] = request.CopyMerge(pushRequest)
		return
	}

	// 이미 pending이면 기존 요청에 병합
	if request, f := p.pending[con]; f {
		p.pending[con] = request.CopyMerge(pushRequest)
		return
	}

	// 새 연결: pending에 추가하고 큐에 삽입
	p.pending[con] = pushRequest
	p.queue = append(p.queue, con)
	p.cond.Signal()
}

// Dequeue는 다음 처리할 연결과 push 요청을 반환한다.
// 큐가 비어있으면 Signal이 올 때까지 블로킹한다.
func (p *PushQueue) Dequeue() (*Connection, *PushRequest, bool) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	// 큐가 비어있으면 대기
	for len(p.queue) == 0 && !p.shuttingDown {
		p.cond.Wait()
	}

	if len(p.queue) == 0 {
		return nil, nil, true // 종료
	}

	con := p.queue[0]
	p.queue[0] = nil // GC를 위해 참조 제거 (grpc-go#4758 참조)
	p.queue = p.queue[1:]

	request := p.pending[con]
	delete(p.pending, con)

	// processing 상태로 전환 (nil = 새 이벤트 없음)
	p.processing[con] = nil

	return con, request, false
}

// MarkDone은 push 처리 완료를 알린다.
// processing 중에 새 이벤트가 왔으면 (processing[con] != nil)
// 자동으로 pending에 다시 추가한다.
func (p *PushQueue) MarkDone(con *Connection) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	request := p.processing[con]
	delete(p.processing, con)

	// processing 중에 새 이벤트가 왔으면 재큐잉
	if request != nil {
		p.pending[con] = request
		p.queue = append(p.queue, con)
		p.cond.Signal()
	}
}

// Pending은 대기 중인 프록시 수를 반환한다.
func (p *PushQueue) Pending() int {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return len(p.queue)
}

// ShutDown은 큐를 종료한다.
func (p *PushQueue) ShutDown() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	p.shuttingDown = true
	p.cond.Broadcast()
}

// --- DebounceOptions: 디바운싱 설정 (pilot/pkg/xds/discovery.go 참조) ---

// DebounceOptions는 설정 변경 이벤트의 디바운싱을 제어한다.
// Istio 기본값: DebounceAfter=100ms, DebounceMax=10s
type DebounceOptions struct {
	DebounceAfter time.Duration // quiet 시간 (마지막 이벤트 이후 대기)
	DebounceMax   time.Duration // 최대 대기 시간
}

// --- DiscoveryServer: xDS 서버 (pilot/pkg/xds/discovery.go 참조) ---

// PushOrder는 xDS 리소스의 push 순서를 정의한다.
// Envoy는 CDS→EDS→LDS→RDS 순서로 설정을 받아야
// 참조 무결성이 보장된다 (예: Route가 참조하는 Cluster가 먼저 존재해야 함).
var PushOrder = []string{
	"CDS", // Cluster Discovery Service
	"EDS", // Endpoint Discovery Service
	"LDS", // Listener Discovery Service
	"RDS", // Route Discovery Service
}

// DiscoveryServer는 Istio Pilot의 xDS 서버를 시뮬레이션한다.
type DiscoveryServer struct {
	pushChannel chan *PushRequest // 디바운싱 입력 채널
	pushQueue   *PushQueue       // 디바운싱 후 프록시별 push 큐

	// 연결된 프록시 관리
	adsClients      map[string]*Connection
	adsClientsMutex sync.RWMutex

	// 동시 push 제한 (semaphore 패턴)
	concurrentPushLimit chan struct{}

	debounceOpts DebounceOptions

	// 메트릭
	inboundUpdates  atomic.Int64
	committedUpdates atomic.Int64
	pushVersion     atomic.Uint64
}

func NewDiscoveryServer() *DiscoveryServer {
	return &DiscoveryServer{
		pushChannel:         make(chan *PushRequest, 10),
		pushQueue:           NewPushQueue(),
		adsClients:          make(map[string]*Connection),
		concurrentPushLimit: make(chan struct{}, 3), // 동시 push 3개 제한
		debounceOpts: DebounceOptions{
			DebounceAfter: 100 * time.Millisecond, // Istio 기본값
			DebounceMax:   10 * time.Second,        // Istio 기본값
		},
	}
}

// ConfigUpdate는 설정 변경을 알린다.
// 실제 Istio의 ConfigUpdate 메서드와 동일한 패턴:
// pushChannel에 이벤트를 보내 디바운싱을 시작한다.
func (s *DiscoveryServer) ConfigUpdate(req *PushRequest) {
	s.inboundUpdates.Add(1)
	s.pushChannel <- req
}

// Start는 서버의 모든 고루틴을 시작한다.
func (s *DiscoveryServer) Start(stopCh <-chan struct{}) {
	go s.handleUpdates(stopCh)  // 디바운싱
	go s.sendPushes(stopCh)      // push 큐 처리
}

// handleUpdates는 pushChannel에서 이벤트를 받아 디바운싱한다.
func (s *DiscoveryServer) handleUpdates(stopCh <-chan struct{}) {
	debounce(s.pushChannel, stopCh, s.debounceOpts, s.Push, &s.committedUpdates)
}

// debounce는 Istio의 핵심 디바운싱 알고리즘을 구현한다.
// (pilot/pkg/xds/discovery.go의 debounce 함수 직접 참조)
//
// 동작 원리:
//   1. 첫 이벤트 도착 시 DebounceAfter 타이머 시작
//   2. 타이머 만료 전에 새 이벤트가 오면 타이머 리셋 + 이벤트 병합
//   3. DebounceMax 초과 또는 quiet 시간 달성 시 병합된 요청으로 Push 실행
//   4. 이전 Push가 완료되기 전에는 다음 Push를 시작하지 않음 (free 플래그)
func debounce(ch chan *PushRequest, stopCh <-chan struct{}, opts DebounceOptions,
	pushFn func(req *PushRequest), updateSent *atomic.Int64) {

	var timeChan <-chan time.Time
	var startDebounce time.Time
	var lastConfigUpdateTime time.Time

	pushCounter := 0
	debouncedEvents := 0

	var req *PushRequest

	// free 플래그: 이전 push가 완료되었는지 추적
	// 실제 Istio에서도 동일한 패턴으로 push 직렬화를 보장한다
	free := true
	freeCh := make(chan struct{}, 1)

	push := func(req *PushRequest, debouncedEvents int, startDebounce time.Time) {
		pushFn(req)
		updateSent.Add(int64(debouncedEvents))
		freeCh <- struct{}{}
	}

	pushWorker := func() {
		eventDelay := time.Since(startDebounce)
		quietTime := time.Since(lastConfigUpdateTime)

		// 최대 대기 시간 초과 또는 충분히 quiet
		if eventDelay >= opts.DebounceMax || quietTime >= opts.DebounceAfter {
			if req != nil {
				pushCounter++
				fmt.Printf("  [디바운스] stable[%d] %d개 이벤트 병합, quiet=%v, delay=%v, full=%v\n",
					pushCounter, debouncedEvents, quietTime.Round(time.Millisecond),
					eventDelay.Round(time.Millisecond), req.Full)
				free = false
				go push(req, debouncedEvents, startDebounce)
				req = nil
				debouncedEvents = 0
			}
		} else {
			// 아직 quiet하지 않으면 남은 시간만큼 타이머 재설정
			timeChan = time.After(opts.DebounceAfter - quietTime)
		}
	}

	for {
		select {
		case <-freeCh:
			free = true
			pushWorker()
		case r := <-ch:
			lastConfigUpdateTime = time.Now()
			if debouncedEvents == 0 {
				timeChan = time.After(opts.DebounceAfter)
				startDebounce = lastConfigUpdateTime
			}
			debouncedEvents++
			req = req.Merge(r)

		case <-timeChan:
			if free {
				pushWorker()
			}
		case <-stopCh:
			return
		}
	}
}

// Push는 디바운싱 후 호출되는 실제 push 함수이다.
// 모든 연결된 프록시의 PushQueue에 Enqueue한다.
func (s *DiscoveryServer) Push(req *PushRequest) {
	version := s.NextVersion()
	req.Start = time.Now()

	fmt.Printf("  [Push] version=%s, full=%v, configs=%v\n",
		version, req.Full, configKeys(req))

	// AdsPushAll: 모든 연결된 프록시에 push 요청을 Enqueue
	s.adsClientsMutex.RLock()
	for _, con := range s.adsClients {
		s.pushQueue.Enqueue(con, req)
	}
	s.adsClientsMutex.RUnlock()
}

// sendPushes는 PushQueue에서 프록시를 Dequeue하여 push를 실행한다.
// concurrentPushLimit 세마포어로 동시 push 수를 제한한다.
func (s *DiscoveryServer) sendPushes(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		default:
			// 세마포어 획득 (동시성 제한)
			s.concurrentPushLimit <- struct{}{}

			client, push, shuttingDown := s.pushQueue.Dequeue()
			if shuttingDown {
				<-s.concurrentPushLimit
				return
			}

			doneFunc := func() {
				s.pushQueue.MarkDone(client)
				<-s.concurrentPushLimit
			}

			go func() {
				pushEv := &Event{
					pushRequest: push,
					done:        doneFunc,
				}

				select {
				case client.pushCh <- pushEv:
				case <-client.streamDone:
					doneFunc()
				}
			}()
		}
	}
}

// NextVersion은 다음 push 버전 문자열을 생성한다.
func (s *DiscoveryServer) NextVersion() string {
	return fmt.Sprintf("%s/%d",
		time.Now().Format(time.RFC3339),
		s.pushVersion.Add(1))
}

// AddConnection은 새 프록시 연결을 등록한다.
func (s *DiscoveryServer) AddConnection(con *Connection) {
	s.adsClientsMutex.Lock()
	defer s.adsClientsMutex.Unlock()
	s.adsClients[con.id] = con
}

// RemoveConnection은 프록시 연결을 제거한다.
func (s *DiscoveryServer) RemoveConnection(conID string) {
	s.adsClientsMutex.Lock()
	defer s.adsClientsMutex.Unlock()
	delete(s.adsClients, conID)
}

// --- 프록시 시뮬레이션 ---

// runProxy는 Envoy 프록시의 xDS 클라이언트를 시뮬레이션한다.
// pushCh에서 이벤트를 받아 watchedResourcesByOrder() 순서로 처리한다.
func runProxy(con *Connection, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case ev := <-con.pushCh:
			// watchedResourcesByOrder: PushOrder 순서로 리소스 처리
			// 실제 Istio의 Connection.watchedResourcesByOrder() 참조
			for _, resType := range PushOrder {
				found := false
				for _, w := range con.watchedResources {
					if w == resType {
						found = true
						break
					}
				}
				if found {
					fmt.Printf("    [%s] %s push: full=%v\n", con.proxyID, resType, ev.pushRequest.Full)
				}
			}
			ev.done()
		case <-con.streamDone:
			return
		}
	}
}

// --- 유틸리티 ---

func configKeys(req *PushRequest) string {
	if len(req.ConfigsUpdated) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(req.ConfigsUpdated))
	for k := range req.ConfigsUpdated {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}

var connectionNumber atomic.Int64

func connectionID(proxyID string) string {
	id := connectionNumber.Add(1)
	return fmt.Sprintf("%s-%d", proxyID, id)
}

// =============================================================================
// main: 시나리오 실행
// =============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("Istio xDS Discovery Service 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	server := NewDiscoveryServer()
	stopCh := make(chan struct{})
	var proxyWg sync.WaitGroup

	// 서버 시작
	server.Start(stopCh)
	fmt.Println("\n[1] DiscoveryServer 시작됨 (디바운싱: 100ms quiet / 10s max)")

	// --- 시나리오 1: 프록시 연결 ---
	fmt.Println("\n--- 시나리오 1: 3개 프록시 연결 ---")

	proxies := make([]*Connection, 3)
	for i := 0; i < 3; i++ {
		proxyID := fmt.Sprintf("sidecar~10.0.0.%d~pod-%c~default", i+1, 'a'+rune(i))
		con := &Connection{
			id:               connectionID(proxyID),
			proxyID:          proxyID,
			pushCh:           make(chan *Event, 10),
			streamDone:       make(chan struct{}),
			watchedResources: []string{"CDS", "EDS", "LDS", "RDS"},
		}
		proxies[i] = con
		server.AddConnection(con)

		proxyWg.Add(1)
		go runProxy(con, &proxyWg)

		fmt.Printf("  프록시 연결: %s (구독: CDS, EDS, LDS, RDS)\n", proxyID)
	}

	time.Sleep(50 * time.Millisecond)

	// --- 시나리오 2: 단일 설정 변경 → 디바운싱 → push ---
	fmt.Println("\n--- 시나리오 2: VirtualService 변경 (단일 이벤트) ---")
	server.ConfigUpdate(&PushRequest{
		Full:           true,
		ConfigsUpdated: map[string]bool{"VirtualService/default/reviews": true},
		Reason:         map[string]int{"config": 1},
	})

	time.Sleep(300 * time.Millisecond) // 디바운싱 + push 완료 대기

	// --- 시나리오 3: 빠른 연속 변경 → 디바운싱 병합 ---
	fmt.Println("\n--- 시나리오 3: 빠른 연속 변경 3개 (병합 테스트) ---")
	configs := []string{
		"DestinationRule/default/reviews",
		"VirtualService/default/ratings",
		"ServiceEntry/default/external-api",
	}
	for i, cfg := range configs {
		server.ConfigUpdate(&PushRequest{
			Full:           i == 0, // 첫 번째만 Full
			ConfigsUpdated: map[string]bool{cfg: true},
			Reason:         map[string]int{"config": 1},
		})
		// 짧은 간격으로 연속 전송 (디바운싱 quiet 시간 내)
		time.Sleep(time.Duration(10+rand.Intn(30)) * time.Millisecond)
	}

	time.Sleep(500 * time.Millisecond) // 디바운싱 + push 완료 대기

	// --- 시나리오 4: PushQueue 병합 테스트 ---
	fmt.Println("\n--- 시나리오 4: PushQueue 동작 검증 ---")
	pq := NewPushQueue()

	testCon := &Connection{
		id:      "test-con",
		proxyID: "test-proxy",
		pushCh:  make(chan *Event, 10),
	}

	// 같은 연결에 두 번 Enqueue → 병합되어야 함
	pq.Enqueue(testCon, &PushRequest{
		Full:           false,
		ConfigsUpdated: map[string]bool{"A": true},
		Reason:         map[string]int{"r1": 1},
	})
	pq.Enqueue(testCon, &PushRequest{
		Full:           true,
		ConfigsUpdated: map[string]bool{"B": true},
		Reason:         map[string]int{"r2": 2},
	})

	fmt.Printf("  Pending 수: %d (중복 Enqueue 후, 기대값: 1)\n", pq.Pending())

	con, req, _ := pq.Dequeue()
	fmt.Printf("  Dequeue: con=%s, full=%v, configs=%v\n", con.proxyID, req.Full, configKeys(req))

	// processing 중에 새 이벤트 → MarkDone 후 자동 재큐잉
	pq.Enqueue(testCon, &PushRequest{
		Full:           false,
		ConfigsUpdated: map[string]bool{"C": true},
		Reason:         map[string]int{"r3": 1},
	})
	fmt.Printf("  Processing 중 Enqueue → Pending: %d (아직 MarkDone 전이므로 0)\n", pq.Pending())

	pq.MarkDone(testCon)
	fmt.Printf("  MarkDone 후 → Pending: %d (자동 재큐잉되어 1)\n", pq.Pending())

	con2, req2, _ := pq.Dequeue()
	fmt.Printf("  재큐잉된 Dequeue: con=%s, full=%v, configs=%v\n", con2.proxyID, req2.Full, configKeys(req2))
	pq.MarkDone(testCon)
	pq.ShutDown()

	// --- 시나리오 5: Push 순서 검증 ---
	fmt.Println("\n--- 시나리오 5: Push 순서 검증 (CDS→EDS→LDS→RDS) ---")
	fmt.Printf("  PushOrder: %v\n", PushOrder)
	fmt.Println("  Envoy는 이 순서로 설정을 받아야 참조 무결성 보장:")
	fmt.Println("    - CDS: Cluster 정의 (서비스 백엔드)")
	fmt.Println("    - EDS: Endpoint 정의 (클러스터 멤버)")
	fmt.Println("    - LDS: Listener 정의 (포트별 필터 체인)")
	fmt.Println("    - RDS: Route 정의 (HTTP 라우팅 규칙)")

	server.ConfigUpdate(&PushRequest{
		Full:           true,
		ConfigsUpdated: map[string]bool{"Gateway/istio-system/ingressgateway": true},
		Reason:         map[string]int{"config": 1},
	})

	time.Sleep(300 * time.Millisecond)

	// --- 시나리오 6: 프록시 연결 해제 ---
	fmt.Println("\n--- 시나리오 6: 프록시 1개 연결 해제 후 push ---")
	close(proxies[2].streamDone)
	server.RemoveConnection(proxies[2].id)
	fmt.Printf("  프록시 해제: %s\n", proxies[2].proxyID)

	time.Sleep(100 * time.Millisecond)

	server.ConfigUpdate(&PushRequest{
		Full:           true,
		ConfigsUpdated: map[string]bool{"VirtualService/default/productpage": true},
		Reason:         map[string]int{"config": 1},
	})

	time.Sleep(300 * time.Millisecond)

	// 정리
	fmt.Println("\n--- 종료 ---")
	for i := 0; i < 2; i++ {
		close(proxies[i].streamDone)
		server.RemoveConnection(proxies[i].id)
	}
	close(stopCh)
	server.pushQueue.ShutDown()

	proxyWg.Wait()

	fmt.Printf("\n[통계] 수신 이벤트: %d, 커밋 이벤트: %d, Push 버전: %d\n",
		server.inboundUpdates.Load(),
		server.committedUpdates.Load(),
		server.pushVersion.Load())

	fmt.Println("\n시뮬레이션 완료.")
}
