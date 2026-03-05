package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ============================================================
// Grafana 이벤트 버스 시뮬레이션
// 이벤트 발행/구독, 비동기 전달, 와일드카드, 필터링 구현
// ============================================================

// --- Event 인터페이스 ---

// Event는 시스템 내 발생한 사건을 나타내는 인터페이스.
// 실제 구현: pkg/bus/bus.go
type Event interface {
	Type() string          // 이벤트 타입 (고유 식별자)
	Timestamp() time.Time  // 이벤트 발생 시각
	Source() string        // 이벤트 소스 (발행자 식별)
}

// --- 이벤트 타입 구현 ---

// BaseEvent는 공통 이벤트 필드를 포함하는 기본 구조체.
type BaseEvent struct {
	EventType string
	EventTime time.Time
	EventSrc  string
}

func (e BaseEvent) Type() string          { return e.EventType }
func (e BaseEvent) Timestamp() time.Time  { return e.EventTime }
func (e BaseEvent) Source() string        { return e.EventSrc }

// DashboardSavedEvent는 대시보드가 저장되었을 때 발생한다.
type DashboardSavedEvent struct {
	BaseEvent
	DashboardID    int64
	DashboardTitle string
	UserName       string
	Version        int
}

// PanelDataChangedEvent는 패널 데이터가 변경되었을 때 발생한다.
type PanelDataChangedEvent struct {
	BaseEvent
	DashboardID int64
	PanelID     int64
	PanelTitle  string
	QueryCount  int
}

// TimeRangeChangedEvent는 시간 범위가 변경되었을 때 발생한다.
type TimeRangeChangedEvent struct {
	BaseEvent
	DashboardID int64
	From        string
	To          string
}

// AlertStateChangedEvent는 알림 상태가 변경되었을 때 발생한다.
type AlertStateChangedEvent struct {
	BaseEvent
	AlertName  string
	PrevState  string
	NewState   string
	EvalValue  float64
}

// --- EventHandler ---

// EventHandler는 이벤트를 처리하는 함수 타입.
type EventHandler func(event Event)

// Subscription은 구독 정보를 나타낸다.
type Subscription struct {
	ID        int
	EventType string        // 이벤트 타입 ("*"이면 와일드카드)
	Handler   EventHandler
	Name      string        // 핸들러 이름 (디버깅용)
	Filter    func(Event) bool // 이벤트 필터 (nil이면 전체 수신)
}

// --- EventBus ---

// EventBus는 이벤트 발행/구독을 중개하는 중앙 버스.
// 실제 구현: pkg/bus/bus.go (InProcBus)
type EventBus struct {
	mu            sync.RWMutex
	subscriptions map[string][]*Subscription // eventType → subscriptions
	wildcards     []*Subscription            // 와일드카드 구독
	nextID        int
	deliveryLog   []DeliveryRecord           // 전달 기록
	logMu         sync.Mutex
}

// DeliveryRecord는 이벤트 전달 기록.
type DeliveryRecord struct {
	EventType   string
	HandlerName string
	Timestamp   time.Time
	Async       bool
}

func NewEventBus() *EventBus {
	return &EventBus{
		subscriptions: make(map[string][]*Subscription),
		deliveryLog:   make([]DeliveryRecord, 0),
	}
}

// Subscribe는 특정 이벤트 타입에 대한 핸들러를 등록한다.
// eventType이 "*"이면 와일드카드 구독 (모든 이벤트 수신).
func (bus *EventBus) Subscribe(eventType string, name string, handler EventHandler, filter func(Event) bool) int {
	bus.mu.Lock()
	defer bus.mu.Unlock()

	bus.nextID++
	sub := &Subscription{
		ID:        bus.nextID,
		EventType: eventType,
		Handler:   handler,
		Name:      name,
		Filter:    filter,
	}

	if eventType == "*" {
		bus.wildcards = append(bus.wildcards, sub)
	} else {
		bus.subscriptions[eventType] = append(bus.subscriptions[eventType], sub)
	}

	return sub.ID
}

// Unsubscribe는 구독을 해제한다.
func (bus *EventBus) Unsubscribe(subID int) bool {
	bus.mu.Lock()
	defer bus.mu.Unlock()

	// 타입별 구독에서 제거
	for eventType, subs := range bus.subscriptions {
		for i, sub := range subs {
			if sub.ID == subID {
				bus.subscriptions[eventType] = append(subs[:i], subs[i+1:]...)
				return true
			}
		}
	}

	// 와일드카드에서 제거
	for i, sub := range bus.wildcards {
		if sub.ID == subID {
			bus.wildcards = append(bus.wildcards[:i], bus.wildcards[i+1:]...)
			return true
		}
	}

	return false
}

// Publish는 이벤트를 비동기적으로 모든 구독자에게 전달한다.
func (bus *EventBus) Publish(event Event) {
	bus.mu.RLock()
	// 매칭되는 핸들러 수집
	var handlers []*Subscription

	// 타입별 구독
	if subs, ok := bus.subscriptions[event.Type()]; ok {
		handlers = append(handlers, subs...)
	}

	// 와일드카드 구독
	handlers = append(handlers, bus.wildcards...)

	bus.mu.RUnlock()

	// 비동기 전달
	var wg sync.WaitGroup
	for _, sub := range handlers {
		// 필터 확인
		if sub.Filter != nil && !sub.Filter(event) {
			continue
		}

		wg.Add(1)
		go func(s *Subscription) {
			defer wg.Done()
			s.Handler(event)
			bus.logDelivery(event.Type(), s.Name, true)
		}(sub)
	}

	wg.Wait()
}

// PublishSync는 이벤트를 동기적으로 모든 구독자에게 전달한다.
func (bus *EventBus) PublishSync(event Event) {
	bus.mu.RLock()
	var handlers []*Subscription

	if subs, ok := bus.subscriptions[event.Type()]; ok {
		handlers = append(handlers, subs...)
	}
	handlers = append(handlers, bus.wildcards...)
	bus.mu.RUnlock()

	for _, sub := range handlers {
		if sub.Filter != nil && !sub.Filter(event) {
			continue
		}
		sub.Handler(event)
		bus.logDelivery(event.Type(), sub.Name, false)
	}
}

func (bus *EventBus) logDelivery(eventType, handlerName string, async bool) {
	bus.logMu.Lock()
	defer bus.logMu.Unlock()
	bus.deliveryLog = append(bus.deliveryLog, DeliveryRecord{
		EventType:   eventType,
		HandlerName: handlerName,
		Timestamp:   time.Now(),
		Async:       async,
	})
}

// GetDeliveryLog는 전달 기록을 반환한다.
func (bus *EventBus) GetDeliveryLog() []DeliveryRecord {
	bus.logMu.Lock()
	defer bus.logMu.Unlock()
	result := make([]DeliveryRecord, len(bus.deliveryLog))
	copy(result, bus.deliveryLog)
	return result
}

// SubscriptionCount는 현재 구독 수를 반환한다.
func (bus *EventBus) SubscriptionCount() int {
	bus.mu.RLock()
	defer bus.mu.RUnlock()
	count := len(bus.wildcards)
	for _, subs := range bus.subscriptions {
		count += len(subs)
	}
	return count
}

// --- 핸들러 구현 (시뮬레이션) ---

func createAuditLogger() EventHandler {
	return func(event Event) {
		fmt.Printf("    [AuditLogger] %s: type=%s, source=%s, time=%s\n",
			"이벤트 기록", event.Type(), event.Source(),
			event.Timestamp().Format("15:04:05.000"))
	}
}

func createCacheInvalidator() EventHandler {
	return func(event Event) {
		switch e := event.(type) {
		case *DashboardSavedEvent:
			fmt.Printf("    [CacheInvalidator] 대시보드 캐시 무효화: ID=%d, title=%s\n",
				e.DashboardID, e.DashboardTitle)
		default:
			fmt.Printf("    [CacheInvalidator] 캐시 갱신: type=%s\n", event.Type())
		}
	}
}

func createNotificationService() EventHandler {
	return func(event Event) {
		switch e := event.(type) {
		case *AlertStateChangedEvent:
			fmt.Printf("    [NotificationSvc] 알림 발송: alert=%s, %s→%s, value=%.2f\n",
				e.AlertName, e.PrevState, e.NewState, e.EvalValue)
		default:
			fmt.Printf("    [NotificationSvc] 알림 처리: type=%s\n", event.Type())
		}
	}
}

func createMetricsCollector() EventHandler {
	return func(event Event) {
		fmt.Printf("    [MetricsCollector] 메트릭 수집: type=%s, source=%s\n",
			event.Type(), event.Source())
	}
}

func createPanelRefresher() EventHandler {
	return func(event Event) {
		switch e := event.(type) {
		case *TimeRangeChangedEvent:
			fmt.Printf("    [PanelRefresher] 패널 새로고침: dashboard=%d, range=%s~%s\n",
				e.DashboardID, e.From, e.To)
		case *PanelDataChangedEvent:
			fmt.Printf("    [PanelRefresher] 패널 데이터 갱신: panel=%s, queries=%d\n",
				e.PanelTitle, e.QueryCount)
		default:
			fmt.Printf("    [PanelRefresher] 패널 갱신: type=%s\n", event.Type())
		}
	}
}

// --- 이벤트 생성 헬퍼 ---

func newDashboardSavedEvent(dashID int64, title, user string, version int) *DashboardSavedEvent {
	return &DashboardSavedEvent{
		BaseEvent: BaseEvent{
			EventType: "DashboardSaved",
			EventTime: time.Now(),
			EventSrc:  "DashboardService",
		},
		DashboardID:    dashID,
		DashboardTitle: title,
		UserName:       user,
		Version:        version,
	}
}

func newPanelDataChangedEvent(dashID, panelID int64, title string, queries int) *PanelDataChangedEvent {
	return &PanelDataChangedEvent{
		BaseEvent: BaseEvent{
			EventType: "PanelDataChanged",
			EventTime: time.Now(),
			EventSrc:  "QueryService",
		},
		DashboardID: dashID,
		PanelID:     panelID,
		PanelTitle:  title,
		QueryCount:  queries,
	}
}

func newTimeRangeChangedEvent(dashID int64, from, to string) *TimeRangeChangedEvent {
	return &TimeRangeChangedEvent{
		BaseEvent: BaseEvent{
			EventType: "TimeRangeChanged",
			EventTime: time.Now(),
			EventSrc:  "TimePicker",
		},
		DashboardID: dashID,
		From:        from,
		To:          to,
	}
}

func newAlertStateChangedEvent(name, prev, next string, value float64) *AlertStateChangedEvent {
	return &AlertStateChangedEvent{
		BaseEvent: BaseEvent{
			EventType: "AlertStateChanged",
			EventTime: time.Now(),
			EventSrc:  "AlertScheduler",
		},
		AlertName: name,
		PrevState: prev,
		NewState:  next,
		EvalValue: value,
	}
}

// --- 메인: 시뮬레이션 ---

func main() {
	fmt.Println("=== Grafana 이벤트 버스 시뮬레이션 ===")
	fmt.Println()

	bus := NewEventBus()

	// ------------------------------------------
	// 1. 구독 등록
	// ------------------------------------------
	fmt.Println("--- 1. 구독 등록 ---")
	fmt.Println()

	// 와일드카드 구독 (모든 이벤트)
	auditID := bus.Subscribe("*", "AuditLogger", createAuditLogger(), nil)
	fmt.Printf("[구독] AuditLogger → * (와일드카드, ID=%d)\n", auditID)

	metricsID := bus.Subscribe("*", "MetricsCollector", createMetricsCollector(), nil)
	fmt.Printf("[구독] MetricsCollector → * (와일드카드, ID=%d)\n", metricsID)

	// 타입별 구독
	cacheID := bus.Subscribe("DashboardSaved", "CacheInvalidator", createCacheInvalidator(), nil)
	fmt.Printf("[구독] CacheInvalidator → DashboardSaved (ID=%d)\n", cacheID)

	notifID := bus.Subscribe("AlertStateChanged", "NotificationSvc", createNotificationService(), nil)
	fmt.Printf("[구독] NotificationSvc → AlertStateChanged (ID=%d)\n", notifID)

	refreshID := bus.Subscribe("TimeRangeChanged", "PanelRefresher", createPanelRefresher(), nil)
	fmt.Printf("[구독] PanelRefresher → TimeRangeChanged (ID=%d)\n", refreshID)

	_ = bus.Subscribe("PanelDataChanged", "PanelRefresher", createPanelRefresher(), nil)
	fmt.Printf("[구독] PanelRefresher → PanelDataChanged\n")

	fmt.Printf("\n총 구독 수: %d\n", bus.SubscriptionCount())

	// ------------------------------------------
	// 2. 이벤트 발행 (비동기)
	// ------------------------------------------
	fmt.Println("\n--- 2. 비동기 이벤트 발행 ---")

	fmt.Println("\n[발행] DashboardSaved (Production Overview)")
	bus.Publish(newDashboardSavedEvent(1, "Production Overview", "admin", 5))

	fmt.Println("\n[발행] AlertStateChanged (HighCPU: ok → firing)")
	bus.Publish(newAlertStateChangedEvent("HighCPU", "ok", "firing", 95.3))

	fmt.Println("\n[발행] TimeRangeChanged (now-6h ~ now)")
	bus.Publish(newTimeRangeChangedEvent(1, "now-6h", "now"))

	fmt.Println("\n[발행] PanelDataChanged (CPU Usage panel)")
	bus.Publish(newPanelDataChangedEvent(1, 10, "CPU Usage", 3))

	// ------------------------------------------
	// 3. 소스 기반 필터링
	// ------------------------------------------
	fmt.Println("\n--- 3. 소스 기반 필터링 ---")

	// DashboardService에서 발생한 이벤트만 수신하는 필터
	dashFilter := func(e Event) bool {
		return e.Source() == "DashboardService"
	}

	bus.Subscribe("*", "DashboardWatcher", func(event Event) {
		fmt.Printf("    [DashboardWatcher] 대시보드 관련 이벤트: type=%s\n", event.Type())
	}, dashFilter)

	fmt.Println("\n[발행] DashboardSaved (필터 테스트)")
	bus.Publish(newDashboardSavedEvent(2, "Dev Dashboard", "editor", 1))

	fmt.Println("\n[발행] AlertStateChanged (DashboardWatcher에 전달되지 않음)")
	bus.Publish(newAlertStateChangedEvent("LowDisk", "ok", "pending", 85.0))

	// ------------------------------------------
	// 4. 구독 해제
	// ------------------------------------------
	fmt.Println("\n--- 4. 구독 해제 ---")

	fmt.Printf("\n구독 해제 전 총 구독 수: %d\n", bus.SubscriptionCount())

	removed := bus.Unsubscribe(refreshID)
	fmt.Printf("[해제] PanelRefresher(TimeRangeChanged) → 성공=%v\n", removed)
	fmt.Printf("구독 해제 후 총 구독 수: %d\n", bus.SubscriptionCount())

	fmt.Println("\n[발행] TimeRangeChanged (PanelRefresher 해제 후)")
	bus.Publish(newTimeRangeChangedEvent(1, "now-24h", "now"))

	// ------------------------------------------
	// 5. 동기 발행
	// ------------------------------------------
	fmt.Println("\n--- 5. 동기 이벤트 발행 ---")
	fmt.Println()
	fmt.Println("[동기 발행] DashboardSaved — 모든 핸들러 순차 실행")
	bus.PublishSync(newDashboardSavedEvent(3, "Sync Dashboard", "viewer", 1))

	// ------------------------------------------
	// 6. 전달 기록 요약
	// ------------------------------------------
	fmt.Println("\n--- 6. 전달 기록 요약 ---")
	fmt.Println()

	log := bus.GetDeliveryLog()

	// 이벤트 타입별 전달 횟수 집계
	typeCounts := make(map[string]int)
	handlerCounts := make(map[string]int)
	asyncCount := 0

	for _, record := range log {
		typeCounts[record.EventType]++
		handlerCounts[record.HandlerName]++
		if record.Async {
			asyncCount++
		}
	}

	fmt.Printf("총 전달 횟수: %d (비동기: %d, 동기: %d)\n\n",
		len(log), asyncCount, len(log)-asyncCount)

	fmt.Println("이벤트 타입별 전달 횟수:")
	fmt.Println(strings.Repeat("-", 40))
	for eventType, count := range typeCounts {
		fmt.Printf("  %-25s %d회\n", eventType, count)
	}

	fmt.Println("\n핸들러별 전달 횟수:")
	fmt.Println(strings.Repeat("-", 40))
	for handler, count := range handlerCounts {
		fmt.Printf("  %-25s %d회\n", handler, count)
	}

	// ------------------------------------------
	// 요약
	// ------------------------------------------
	fmt.Println("\n--- 시뮬레이션 요약 ---")
	fmt.Println()
	fmt.Println("이벤트 버스 구성요소:")
	fmt.Println("  1. Event 인터페이스: Type(), Timestamp(), Source()")
	fmt.Println("  2. EventBus: 구독 등록, 이벤트 발행, 전달 중개")
	fmt.Println("  3. Subscribe: 타입별 구독 또는 와일드카드(*) 구독")
	fmt.Println("  4. Publish: 비동기 전달 (고루틴), PublishSync: 동기 전달")
	fmt.Println("  5. Filter: 소스/조건 기반 이벤트 필터링")
	fmt.Println("  6. Unsubscribe: 동적 구독 해제")
	fmt.Println()
	fmt.Println("설계 패턴:")
	fmt.Println("  - Observer 패턴: 발행자와 구독자 간 느슨한 결합")
	fmt.Println("  - 비동기 전달: 발행자가 구독자의 처리를 기다리지 않음")
	fmt.Println("  - 와일드카드: 감사 로그, 메트릭 수집 등 횡단 관심사 처리")
}
