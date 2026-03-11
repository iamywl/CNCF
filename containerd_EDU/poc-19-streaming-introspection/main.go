package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// containerd gRPC Streaming + Introspection 시뮬레이션
// =============================================================================
//
// containerd는 gRPC 서비스를 통해 클라이언트와 통신하며, TTRPC도 지원한다.
// 주요 기능:
//   - Streaming: 이벤트/로그를 실시간 스트리밍
//   - Introspection: 등록된 플러그인과 서비스 상태 조회
//
// 실제 코드 참조:
//   - services/events/service.go: 이벤트 스트리밍 서비스
//   - services/introspection/service.go: 인트로스펙션 서비스
//   - api/services/events/v1/events.proto: 이벤트 API 정의
// =============================================================================

// --- 이벤트 시스템 ---

type EventTopic string

const (
	TopicContainerCreate EventTopic = "/containers/create"
	TopicContainerDelete EventTopic = "/containers/delete"
	TopicTaskStart       EventTopic = "/tasks/start"
	TopicTaskExit        EventTopic = "/tasks/exit"
	TopicImagePull       EventTopic = "/images/pull"
	TopicSnapshotCommit  EventTopic = "/snapshots/commit"
)

// Event는 containerd 이벤트를 표현한다.
type Event struct {
	Timestamp time.Time  `json:"timestamp"`
	Namespace string     `json:"namespace"`
	Topic     EventTopic `json:"topic"`
	Event     string     `json:"event"` // 직렬화된 이벤트 데이터
}

// EventFilter는 구독 시 필터 조건이다.
type EventFilter struct {
	Topics     []EventTopic
	Namespaces []string
}

func (f EventFilter) Match(e Event) bool {
	if len(f.Topics) > 0 {
		matched := false
		for _, t := range f.Topics {
			if t == e.Topic {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(f.Namespaces) > 0 {
		matched := false
		for _, ns := range f.Namespaces {
			if ns == e.Namespace {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// --- Streaming 서비스 ---

// Subscriber는 이벤트 스트림 구독자이다.
type Subscriber struct {
	ID     string
	Filter EventFilter
	Ch     chan Event
	Done   chan struct{}
}

// EventService는 containerd의 이벤트 스트리밍 서비스를 시뮬레이션한다.
type EventService struct {
	mu          sync.RWMutex
	subscribers map[string]*Subscriber
	history     []Event
	maxHistory  int
}

func NewEventService(maxHistory int) *EventService {
	return &EventService{
		subscribers: make(map[string]*Subscriber),
		maxHistory:  maxHistory,
	}
}

// Publish는 이벤트를 발행한다 (서버 측).
func (es *EventService) Publish(ctx context.Context, event Event) {
	es.mu.Lock()
	es.history = append(es.history, event)
	if len(es.history) > es.maxHistory {
		es.history = es.history[1:]
	}
	subs := make([]*Subscriber, 0, len(es.subscribers))
	for _, sub := range es.subscribers {
		subs = append(subs, sub)
	}
	es.mu.Unlock()

	for _, sub := range subs {
		if sub.Filter.Match(event) {
			select {
			case sub.Ch <- event:
			default:
				// 버퍼 가득 차면 오래된 이벤트 드롭 (실제와 동일)
				fmt.Printf("    [WARN] subscriber %s buffer full, dropping event\n", sub.ID)
			}
		}
	}
}

// Subscribe는 이벤트 스트림을 구독한다 (gRPC server-side streaming 시뮬레이션).
func (es *EventService) Subscribe(id string, filter EventFilter) *Subscriber {
	sub := &Subscriber{
		ID:     id,
		Filter: filter,
		Ch:     make(chan Event, 64),
		Done:   make(chan struct{}),
	}
	es.mu.Lock()
	es.subscribers[id] = sub
	es.mu.Unlock()
	fmt.Printf("  [SUB]  %s subscribed (topics: %v, namespaces: %v)\n",
		id, filter.Topics, filter.Namespaces)
	return sub
}

func (es *EventService) Unsubscribe(id string) {
	es.mu.Lock()
	if sub, ok := es.subscribers[id]; ok {
		close(sub.Done)
		delete(es.subscribers, id)
	}
	es.mu.Unlock()
}

// --- Introspection 서비스 ---

type PluginType string

const (
	PluginTypeRuntime    PluginType = "io.containerd.runtime.v2"
	PluginTypeSnapshotter PluginType = "io.containerd.snapshotter.v1"
	PluginTypeContent    PluginType = "io.containerd.content.v1"
	PluginTypeGRPC       PluginType = "io.containerd.grpc.v1"
	PluginTypeDiffer     PluginType = "io.containerd.differ.v1"
	PluginTypeService    PluginType = "io.containerd.service.v1"
)

type PluginInfo struct {
	Type     PluginType `json:"type"`
	ID       string     `json:"id"`
	Status   string     `json:"status"`
	Requires []string   `json:"requires,omitempty"`
	Caps     []string   `json:"capabilities,omitempty"`
}

type ServerInfo struct {
	UUID    string       `json:"uuid"`
	Plugins []PluginInfo `json:"plugins"`
}

// IntrospectionService는 containerd 인트로스펙션 서비스를 시뮬레이션한다.
type IntrospectionService struct {
	serverInfo ServerInfo
}

func NewIntrospectionService() *IntrospectionService {
	return &IntrospectionService{
		serverInfo: ServerInfo{
			UUID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			Plugins: []PluginInfo{
				{PluginTypeContent, "content", "ok", nil, []string{"content"}},
				{PluginTypeSnapshotter, "overlayfs", "ok", []string{"content"}, []string{"snapshots"}},
				{PluginTypeSnapshotter, "native", "ok", []string{"content"}, []string{"snapshots"}},
				{PluginTypeRuntime, "task", "ok", nil, []string{"runtime"}},
				{PluginTypeRuntime, "shim", "ok", nil, nil},
				{PluginTypeDiffer, "walking", "ok", nil, nil},
				{PluginTypeGRPC, "containers", "ok", nil, nil},
				{PluginTypeGRPC, "content", "ok", nil, nil},
				{PluginTypeGRPC, "images", "ok", nil, nil},
				{PluginTypeGRPC, "namespaces", "ok", nil, nil},
				{PluginTypeGRPC, "tasks", "ok", nil, nil},
				{PluginTypeGRPC, "events", "ok", nil, nil},
				{PluginTypeGRPC, "introspection", "ok", nil, nil},
				{PluginTypeService, "containers-service", "ok", nil, nil},
				{PluginTypeService, "images-service", "ok", nil, nil},
			},
		},
	}
}

func (is *IntrospectionService) Server() ServerInfo {
	return is.serverInfo
}

func (is *IntrospectionService) Plugins(filters ...PluginType) []PluginInfo {
	if len(filters) == 0 {
		return is.serverInfo.Plugins
	}
	var result []PluginInfo
	for _, p := range is.serverInfo.Plugins {
		for _, f := range filters {
			if p.Type == f {
				result = append(result, p)
				break
			}
		}
	}
	return result
}

// --- gRPC 서비스 레지스트리 시뮬레이션 ---

type ServiceMethod struct {
	Name          string
	ServerStream  bool
	ClientStream  bool
}

type GRPCService struct {
	Name    string
	Methods []ServiceMethod
}

func getRegisteredServices() []GRPCService {
	return []GRPCService{
		{"containerd.services.containers.v1.Containers", []ServiceMethod{
			{"Get", false, false},
			{"List", false, false},
			{"Create", false, false},
			{"Update", false, false},
			{"Delete", false, false},
		}},
		{"containerd.services.tasks.v1.Tasks", []ServiceMethod{
			{"Create", false, false},
			{"Start", false, false},
			{"Delete", false, false},
			{"Kill", false, false},
			{"Exec", false, false},
			{"Checkpoint", false, false},
		}},
		{"containerd.services.events.v1.Events", []ServiceMethod{
			{"Publish", false, false},
			{"Forward", false, false},
			{"Subscribe", true, false}, // server-side streaming
		}},
		{"containerd.services.images.v1.Images", []ServiceMethod{
			{"Get", false, false},
			{"List", false, false},
			{"Create", false, false},
			{"Update", false, false},
			{"Delete", false, false},
		}},
		{"containerd.services.content.v1.Content", []ServiceMethod{
			{"Info", false, false},
			{"Update", false, false},
			{"List", true, false},
			{"Read", true, false},  // server streaming
			{"Write", true, true},  // bidi streaming
		}},
	}
}

func main() {
	fmt.Println("=== containerd gRPC Streaming + Introspection 시뮬레이션 ===")
	fmt.Println()

	// --- Introspection ---
	fmt.Println("[1] Introspection: 서버 정보")
	fmt.Println(strings.Repeat("-", 65))
	intro := NewIntrospectionService()
	info := intro.Server()
	fmt.Printf("  Server UUID: %s\n", info.UUID)
	fmt.Printf("  Total Plugins: %d\n", len(info.Plugins))
	fmt.Println()

	fmt.Println("[2] Introspection: 플러그인 목록 (타입별)")
	fmt.Println(strings.Repeat("-", 65))
	types := []PluginType{PluginTypeGRPC, PluginTypeSnapshotter, PluginTypeRuntime, PluginTypeContent, PluginTypeDiffer, PluginTypeService}
	for _, t := range types {
		plugins := intro.Plugins(t)
		fmt.Printf("  [%s] (%d개)\n", t, len(plugins))
		for _, p := range plugins {
			caps := ""
			if len(p.Caps) > 0 {
				caps = " caps=" + strings.Join(p.Caps, ",")
			}
			reqs := ""
			if len(p.Requires) > 0 {
				reqs = " requires=" + strings.Join(p.Requires, ",")
			}
			fmt.Printf("    - %-25s status=%s%s%s\n", p.ID, p.Status, caps, reqs)
		}
	}
	fmt.Println()

	// --- gRPC 서비스 목록 ---
	fmt.Println("[3] 등록된 gRPC 서비스/메서드")
	fmt.Println(strings.Repeat("-", 65))
	for _, svc := range getRegisteredServices() {
		fmt.Printf("  %s\n", svc.Name)
		for _, m := range svc.Methods {
			streamType := "unary"
			if m.ServerStream && m.ClientStream {
				streamType = "bidi-stream"
			} else if m.ServerStream {
				streamType = "server-stream"
			} else if m.ClientStream {
				streamType = "client-stream"
			}
			fmt.Printf("    %-20s [%s]\n", m.Name, streamType)
		}
	}
	fmt.Println()

	// --- Event Streaming ---
	fmt.Println("[4] 이벤트 스트리밍 시뮬레이션")
	fmt.Println(strings.Repeat("-", 65))

	eventSvc := NewEventService(100)

	// 구독자 등록
	sub1 := eventSvc.Subscribe("ctr-watcher", EventFilter{
		Topics: []EventTopic{TopicContainerCreate, TopicContainerDelete},
	})
	sub2 := eventSvc.Subscribe("task-monitor", EventFilter{
		Topics: []EventTopic{TopicTaskStart, TopicTaskExit},
	})
	sub3 := eventSvc.Subscribe("all-events", EventFilter{
		Namespaces: []string{"default"},
	})
	fmt.Println()

	// 구독자별 수신 고루틴
	var wg sync.WaitGroup
	receivedCounts := make(map[string]int)
	var countMu sync.Mutex

	startReceiver := func(sub *Subscriber) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case evt := <-sub.Ch:
					countMu.Lock()
					receivedCounts[sub.ID]++
					countMu.Unlock()
					fmt.Printf("    [RECV:%s] %s %s/%s\n",
						sub.ID, evt.Topic, evt.Namespace, evt.Event)
				case <-sub.Done:
					return
				}
			}
		}()
	}

	startReceiver(sub1)
	startReceiver(sub2)
	startReceiver(sub3)

	// 이벤트 발행
	ctx := context.Background()
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	namespaces := []string{"default", "kube-system"}
	containerIDs := []string{"abc123", "def456", "ghi789"}
	topics := []EventTopic{
		TopicContainerCreate, TopicContainerDelete,
		TopicTaskStart, TopicTaskExit,
		TopicImagePull, TopicSnapshotCommit,
	}

	fmt.Println("  이벤트 발행 중...")
	for i := 0; i < 15; i++ {
		topic := topics[r.Intn(len(topics))]
		ns := namespaces[r.Intn(len(namespaces))]
		cid := containerIDs[r.Intn(len(containerIDs))]

		event := Event{
			Timestamp: time.Now(),
			Namespace: ns,
			Topic:     topic,
			Event:     fmt.Sprintf("container_id=%s", cid),
		}
		eventSvc.Publish(ctx, event)
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond) // 수신 대기

	// 구독 해제
	eventSvc.Unsubscribe("ctr-watcher")
	eventSvc.Unsubscribe("task-monitor")
	eventSvc.Unsubscribe("all-events")
	wg.Wait()

	fmt.Println()
	fmt.Println("[5] 수신 통계")
	fmt.Println(strings.Repeat("-", 65))
	countMu.Lock()
	for id, count := range receivedCounts {
		fmt.Printf("  %-20s: %d events\n", id, count)
	}
	countMu.Unlock()
	fmt.Println()

	// --- 이벤트 히스토리 (JSON) ---
	fmt.Println("[6] 이벤트 히스토리 (최근 5개, JSON)")
	fmt.Println(strings.Repeat("-", 65))
	eventSvc.mu.RLock()
	start := len(eventSvc.history) - 5
	if start < 0 {
		start = 0
	}
	for _, evt := range eventSvc.history[start:] {
		data, _ := json.Marshal(evt)
		fmt.Printf("  %s\n", data)
	}
	eventSvc.mu.RUnlock()
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
