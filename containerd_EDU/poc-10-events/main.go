package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// containerd 이벤트 Publisher/Subscriber 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - core/events/events.go                  : Envelope, Publisher, Forwarder, Subscriber 인터페이스
//   - core/events/exchange/exchange.go        : Exchange (Broadcaster 기반 이벤트 분배)
//   - pkg/namespaces/context.go              : WithNamespace, NamespaceRequired
//
// containerd 이벤트 시스템 설계:
//   1. Exchange: 중앙 이벤트 허브 — Publisher, Forwarder, Subscriber 인터페이스 모두 구현
//   2. Envelope: 이벤트 패키지 — Timestamp, Namespace, Topic, Event 포함
//   3. Broadcaster: 등록된 모든 Sink에 이벤트를 방송 (go-events 라이브러리)
//   4. Filter: 토픽/네임스페이스 기반 필터링으로 관심 이벤트만 수신
//   5. Queue: 비동기 큐를 통해 발행자와 구독자 디커플링
//   6. Forward: shim → containerd로 외부 이벤트 전달

// =============================================================================
// 1. 네임스페이스 컨텍스트
// =============================================================================

type contextKey string

const namespaceKey contextKey = "containerd.namespace"

// WithNamespace는 context에 네임스페이스를 설정한다.
// 실제: pkg/namespaces/context.go
func WithNamespace(ctx context.Context, ns string) context.Context {
	return context.WithValue(ctx, namespaceKey, ns)
}

// NamespaceRequired는 context에서 네임스페이스를 추출한다.
// 실제: pkg/namespaces/context.go
func NamespaceRequired(ctx context.Context) (string, error) {
	ns, ok := ctx.Value(namespaceKey).(string)
	if !ok || ns == "" {
		return "", fmt.Errorf("namespace is required")
	}
	return ns, nil
}

// =============================================================================
// 2. Envelope — 이벤트 패키지
// =============================================================================

// Envelope는 이벤트를 감싸는 봉투이다.
// 실제: core/events/events.go — Envelope 구조체
//
//	type Envelope struct {
//	    Timestamp time.Time
//	    Namespace string
//	    Topic     string
//	    Event     typeurl.Any
//	}
//
// Field() 메서드로 필터링 시 필드 접근을 제공한다.
type Envelope struct {
	Timestamp time.Time
	Namespace string
	Topic     string
	Event     interface{} // 실제는 typeurl.Any (protobuf 직렬화)
}

// Field는 필터링에 사용되는 필드 값을 반환한다.
// 실제: core/events/events.go — Envelope.Field(fieldpath)
// 필터 표현식 "topic==/containers/create" 등을 지원
func (e *Envelope) Field(fieldpath []string) (string, bool) {
	if len(fieldpath) == 0 {
		return "", false
	}
	switch fieldpath[0] {
	case "namespace":
		return e.Namespace, len(e.Namespace) > 0
	case "topic":
		return e.Topic, len(e.Topic) > 0
	}
	return "", false
}

// =============================================================================
// 3. 인터페이스 정의 — Publisher, Forwarder, Subscriber
// =============================================================================

// Publisher는 이벤트를 발행하는 인터페이스이다.
// 실제: core/events/events.go — Publisher interface
type Publisher interface {
	Publish(ctx context.Context, topic string, event interface{}) error
}

// Forwarder는 외부 이벤트를 전달하는 인터페이스이다.
// 실제: core/events/events.go — Forwarder interface
// shim 프로세스에서 containerd로 이벤트를 전달할 때 사용
type Forwarder interface {
	Forward(ctx context.Context, envelope *Envelope) error
}

// Subscriber는 이벤트를 구독하는 인터페이스이다.
// 실제: core/events/events.go — Subscriber interface
type Subscriber interface {
	Subscribe(ctx context.Context, filters ...string) (ch <-chan *Envelope, errs <-chan error)
}

// =============================================================================
// 4. Filter — 토픽 기반 필터링
// =============================================================================

// TopicFilter는 토픽 패턴으로 이벤트를 필터링한다.
// 실제: pkg/filters 패키지에서 containerd 필터 문법 파싱
// 예: "topic==/containers/create", "topic~=/container/*"
//
// 여기서는 간단한 글로브 매칭으로 시뮬레이션:
//   - 정확한 일치: "/containers/create"
//   - 접두사 와일드카드: "/containers/*" → "/containers/"로 시작하는 모든 토픽
type TopicFilter struct {
	pattern string
}

// NewTopicFilter는 토픽 필터를 생성한다.
func NewTopicFilter(pattern string) *TopicFilter {
	return &TopicFilter{pattern: pattern}
}

// Match는 Envelope이 필터 조건에 맞는지 확인한다.
func (f *TopicFilter) Match(env *Envelope) bool {
	if strings.HasSuffix(f.pattern, "/*") {
		prefix := strings.TrimSuffix(f.pattern, "*")
		return strings.HasPrefix(env.Topic, prefix)
	}
	return env.Topic == f.pattern
}

// =============================================================================
// 5. Subscription — 구독자
// =============================================================================

// Subscription은 하나의 구독을 나타낸다.
// 실제: exchange.go의 Subscribe() 내부에서 생성
//   - goevents.NewChannel(0) → 이벤트 채널
//   - goevents.NewQueue(channel) → 비동기 큐
//   - goevents.NewFilter(queue, matcher) → 필터 적용
type Subscription struct {
	ch      chan *Envelope
	filters []*TopicFilter
	done    chan struct{}
}

func newSubscription(filters []*TopicFilter) *Subscription {
	return &Subscription{
		ch:      make(chan *Envelope, 64), // 버퍼링된 채널
		filters: filters,
		done:    make(chan struct{}),
	}
}

// matches는 이벤트가 구독 필터에 매칭되는지 확인한다.
// 필터가 없으면 모든 이벤트 수신, 있으면 하나라도 매칭되면 수신
// 실제: goevents.NewFilter에서 MatcherFunc로 필터 적용
func (s *Subscription) matches(env *Envelope) bool {
	if len(s.filters) == 0 {
		return true // 필터 없으면 모든 이벤트 수신
	}
	for _, f := range s.filters {
		if f.Match(env) {
			return true // 하나라도 매칭되면 수신
		}
	}
	return false
}

// =============================================================================
// 6. Exchange — 이벤트 허브 (Broadcaster)
// =============================================================================

// Exchange는 중앙 이벤트 교환소이다.
// 실제: core/events/exchange/exchange.go — Exchange 구조체
//
// Publisher, Forwarder, Subscriber 인터페이스를 모두 구현한다.
//
//	type Exchange struct {
//	    broadcaster *goevents.Broadcaster
//	}
//
// Broadcaster는 등록된 모든 Sink에 Write() 호출로 이벤트를 방송한다.
type Exchange struct {
	mu          sync.RWMutex
	subscribers []*Subscription
}

// NewExchange는 새 이벤트 교환소를 생성한다.
// 실제: exchange.go — NewExchange()
func NewExchange() *Exchange {
	return &Exchange{}
}

// Publish는 이벤트를 발행한다.
// 실제: exchange.go — Publish(ctx, topic, event)
//
// 프로세스:
//  1. context에서 네임스페이스 추출 (NamespaceRequired)
//  2. 토픽 유효성 검증 ('/'로 시작, 최소 하나의 컴포넌트)
//  3. 이벤트를 typeurl.MarshalAny로 직렬화
//  4. Envelope 생성 (Timestamp, Namespace, Topic, Event)
//  5. broadcaster.Write(envelope)로 모든 구독자에게 전달
func (e *Exchange) Publish(ctx context.Context, topic string, event interface{}) error {
	namespace, err := NamespaceRequired(ctx)
	if err != nil {
		return fmt.Errorf("failed publishing event: %w", err)
	}

	if err := validateTopic(topic); err != nil {
		return fmt.Errorf("envelope topic %q: %w", topic, err)
	}

	envelope := &Envelope{
		Timestamp: time.Now().UTC(),
		Namespace: namespace,
		Topic:     topic,
		Event:     event,
	}

	return e.broadcast(envelope)
}

// Forward는 외부에서 생성된 이벤트를 전달한다.
// 실제: exchange.go — Forward(ctx, envelope)
//
// Publish와 달리 이미 완성된 Envelope을 받는다.
// 주 용도: shim 프로세스 → containerd 이벤트 전파
// 예: 컨테이너 OOM 이벤트, 태스크 종료 이벤트
func (e *Exchange) Forward(ctx context.Context, envelope *Envelope) error {
	if err := validateEnvelope(envelope); err != nil {
		return err
	}
	return e.broadcast(envelope)
}

// Subscribe는 이벤트를 구독한다.
// 실제: exchange.go — Subscribe(ctx, filters...)
//
// 프로세스:
//  1. 필터 파싱 (토픽 패턴 매칭)
//  2. Channel + Queue + Filter 체인 생성
//  3. Broadcaster에 Sink 등록
//  4. goroutine으로 이벤트 수신 루프 시작
//  5. context 취소 시 정리 (Broadcaster에서 Sink 제거)
func (e *Exchange) Subscribe(ctx context.Context, filterPatterns ...string) (<-chan *Envelope, <-chan error) {
	var filters []*TopicFilter
	for _, p := range filterPatterns {
		filters = append(filters, NewTopicFilter(p))
	}

	sub := newSubscription(filters)

	e.mu.Lock()
	e.subscribers = append(e.subscribers, sub)
	e.mu.Unlock()

	evch := make(chan *Envelope, 64)
	errch := make(chan error, 1)

	// 구독 goroutine — context 취소까지 이벤트 전달
	// 실제: exchange.go의 Subscribe 내부 goroutine
	go func() {
		defer func() {
			// Broadcaster에서 Sink 제거
			// 실제: closeAll() → e.broadcaster.Remove(dst); close(errq)
			e.mu.Lock()
			for i, s := range e.subscribers {
				if s == sub {
					e.subscribers = append(e.subscribers[:i], e.subscribers[i+1:]...)
					break
				}
			}
			e.mu.Unlock()
			close(evch)
			close(errch)
		}()

		for {
			select {
			case env := <-sub.ch:
				select {
				case evch <- env:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return evch, errch
}

// broadcast는 모든 구독자에게 이벤트를 전달한다.
// 실제: goevents.Broadcaster.Write(event) — 등록된 모든 Sink에 전달
func (e *Exchange) broadcast(env *Envelope) error {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, sub := range e.subscribers {
		if sub.matches(env) {
			// 비블로킹 전송 (큐가 가득 차면 스킵)
			select {
			case sub.ch <- env:
			default:
				// 실제: goevents.Queue가 비동기로 버퍼링
			}
		}
	}
	return nil
}

// =============================================================================
// 7. 유효성 검증
// =============================================================================

// validateTopic은 토픽의 유효성을 검증한다.
// 실제: exchange.go — validateTopic(topic)
// 규칙: '/'로 시작, 비어있지 않음, 최소 하나의 컴포넌트
func validateTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("topic must not be empty")
	}
	if topic[0] != '/' {
		return fmt.Errorf("topic must start with '/'")
	}
	if len(topic) == 1 {
		return fmt.Errorf("topic must have at least one component")
	}
	return nil
}

// validateEnvelope은 Envelope의 유효성을 검증한다.
// 실제: exchange.go — validateEnvelope(envelope)
func validateEnvelope(envelope *Envelope) error {
	if envelope.Namespace == "" {
		return fmt.Errorf("namespace is required in envelope")
	}
	if err := validateTopic(envelope.Topic); err != nil {
		return err
	}
	if envelope.Timestamp.IsZero() {
		return fmt.Errorf("timestamp must be set on forwarded event")
	}
	return nil
}

// =============================================================================
// 8. 이벤트 타입 — containerd 이벤트 모델
// =============================================================================

// 실제 이벤트 타입: api/events/container.go, api/events/image.go 등
// protobuf로 정의되어 typeurl.MarshalAny로 직렬화

// ContainerCreate는 컨테이너 생성 이벤트이다.
type ContainerCreate struct {
	ID    string
	Image string
}

func (e ContainerCreate) String() string {
	return fmt.Sprintf("ContainerCreate{id=%s, image=%s}", e.ID, e.Image)
}

// ContainerDelete는 컨테이너 삭제 이벤트이다.
type ContainerDelete struct {
	ID string
}

func (e ContainerDelete) String() string {
	return fmt.Sprintf("ContainerDelete{id=%s}", e.ID)
}

// TaskStart는 태스크 시작 이벤트이다.
type TaskStart struct {
	ContainerID string
	PID         uint32
}

func (e TaskStart) String() string {
	return fmt.Sprintf("TaskStart{container=%s, pid=%d}", e.ContainerID, e.PID)
}

// TaskExit는 태스크 종료 이벤트이다.
type TaskExit struct {
	ContainerID string
	PID         uint32
	ExitStatus  uint32
}

func (e TaskExit) String() string {
	return fmt.Sprintf("TaskExit{container=%s, pid=%d, status=%d}", e.ContainerID, e.PID, e.ExitStatus)
}

// ImageCreate는 이미지 생성(pull) 이벤트이다.
type ImageCreate struct {
	Name string
}

func (e ImageCreate) String() string {
	return fmt.Sprintf("ImageCreate{name=%s}", e.Name)
}

// ImageDelete는 이미지 삭제 이벤트이다.
type ImageDelete struct {
	Name string
}

func (e ImageDelete) String() string {
	return fmt.Sprintf("ImageDelete{name=%s}", e.Name)
}

// SnapshotRemove는 스냅샷 제거 이벤트이다.
type SnapshotRemove struct {
	Key         string
	Snapshotter string
}

func (e SnapshotRemove) String() string {
	return fmt.Sprintf("SnapshotRemove{key=%s, snapshotter=%s}", e.Key, e.Snapshotter)
}

// =============================================================================
// 9. 메인 — 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== containerd 이벤트 Publisher/Subscriber 시뮬레이션 ===")
	fmt.Println()

	exchange := NewExchange()
	ctx := context.Background()

	// --- 데모 1: 기본 Publish/Subscribe ---
	fmt.Println("--- 데모 1: 기본 Publish/Subscribe ---")
	subCtx1, cancel1 := context.WithCancel(ctx)

	// 모든 이벤트를 수신하는 구독자
	allCh, _ := exchange.Subscribe(subCtx1)
	fmt.Println("구독자 1: 필터 없음 (모든 이벤트 수신)")

	var wg sync.WaitGroup
	var received1 []*Envelope
	var mu1 sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		for env := range allCh {
			mu1.Lock()
			received1 = append(received1, env)
			mu1.Unlock()
		}
	}()

	// 이벤트 발행
	nsCtx := WithNamespace(ctx, "default")
	exchange.Publish(nsCtx, "/containers/create", ContainerCreate{ID: "c1", Image: "nginx"})
	exchange.Publish(nsCtx, "/containers/delete", ContainerDelete{ID: "c1"})
	exchange.Publish(nsCtx, "/images/create", ImageCreate{Name: "nginx:latest"})

	time.Sleep(50 * time.Millisecond) // 이벤트 전달 대기
	cancel1()
	time.Sleep(20 * time.Millisecond) // goroutine 정리

	mu1.Lock()
	fmt.Printf("수신된 이벤트: %d건\n", len(received1))
	for _, env := range received1 {
		fmt.Printf("  [%s] ns=%s topic=%s event=%v\n",
			env.Timestamp.Format("15:04:05.000"), env.Namespace, env.Topic, env.Event)
	}
	mu1.Unlock()
	fmt.Println()

	// --- 데모 2: 토픽 기반 필터링 ---
	fmt.Println("--- 데모 2: 토픽 기반 필터링 ---")
	subCtx2, cancel2 := context.WithCancel(ctx)

	// /containers/* 만 수신
	containerCh, _ := exchange.Subscribe(subCtx2, "/containers/*")
	fmt.Println("구독자 2: 필터 \"/containers/*\" (컨테이너 이벤트만)")

	// /images/delete 만 수신
	subCtx3, cancel3 := context.WithCancel(ctx)
	imgDeleteCh, _ := exchange.Subscribe(subCtx3, "/images/delete")
	fmt.Println("구독자 3: 필터 \"/images/delete\" (이미지 삭제만)")

	var received2, received3 []*Envelope
	var mu2, mu3 sync.Mutex

	wg.Add(2)
	go func() {
		defer wg.Done()
		for env := range containerCh {
			mu2.Lock()
			received2 = append(received2, env)
			mu2.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for env := range imgDeleteCh {
			mu3.Lock()
			received3 = append(received3, env)
			mu3.Unlock()
		}
	}()

	// 다양한 토픽으로 이벤트 발행
	exchange.Publish(nsCtx, "/containers/create", ContainerCreate{ID: "c2", Image: "redis"})
	exchange.Publish(nsCtx, "/containers/delete", ContainerDelete{ID: "c2"})
	exchange.Publish(nsCtx, "/images/create", ImageCreate{Name: "redis:7"})
	exchange.Publish(nsCtx, "/images/delete", ImageDelete{Name: "nginx:latest"})
	exchange.Publish(nsCtx, "/tasks/start", TaskStart{ContainerID: "c3", PID: 1234})

	time.Sleep(50 * time.Millisecond)
	cancel2()
	cancel3()
	time.Sleep(20 * time.Millisecond)

	mu2.Lock()
	fmt.Printf("\n구독자 2 수신 (/containers/*): %d건\n", len(received2))
	for _, env := range received2 {
		fmt.Printf("  topic=%s event=%v\n", env.Topic, env.Event)
	}
	mu2.Unlock()

	mu3.Lock()
	fmt.Printf("구독자 3 수신 (/images/delete): %d건\n", len(received3))
	for _, env := range received3 {
		fmt.Printf("  topic=%s event=%v\n", env.Topic, env.Event)
	}
	mu3.Unlock()
	fmt.Println()

	// --- 데모 3: Forward (외부 이벤트 수신) ---
	fmt.Println("--- 데모 3: Forward — shim에서 containerd로 이벤트 전달 ---")
	subCtx4, cancel4 := context.WithCancel(ctx)
	forwardCh, _ := exchange.Subscribe(subCtx4, "/tasks/*")

	var received4 []*Envelope
	var mu4 sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		for env := range forwardCh {
			mu4.Lock()
			received4 = append(received4, env)
			mu4.Unlock()
		}
	}()

	// Forward: shim 프로세스에서 생성한 이벤트를 전달
	// 실제: shim이 TTRPC로 containerd에 이벤트 전달
	// → ForwardRequest → exchange.Forward()
	fmt.Println("shim에서 태스크 이벤트 Forward:")
	exchange.Forward(ctx, &Envelope{
		Timestamp: time.Now().UTC(),
		Namespace: "default",
		Topic:     "/tasks/start",
		Event:     TaskStart{ContainerID: "c5", PID: 5678},
	})
	exchange.Forward(ctx, &Envelope{
		Timestamp: time.Now().UTC(),
		Namespace: "default",
		Topic:     "/tasks/exit",
		Event:     TaskExit{ContainerID: "c5", PID: 5678, ExitStatus: 0},
	})

	// Forward 유효성 검증 실패 케이스
	err := exchange.Forward(ctx, &Envelope{
		Namespace: "default",
		Topic:     "/tasks/start",
		// Timestamp 누락 — Forward는 반드시 타임스탬프 필요
	})
	fmt.Printf("타임스탬프 누락 Forward: err=%v\n", err)

	err = exchange.Forward(ctx, &Envelope{
		Timestamp: time.Now(),
		Topic:     "/tasks/start",
		// Namespace 누락
	})
	fmt.Printf("네임스페이스 누락 Forward: err=%v\n", err)

	time.Sleep(50 * time.Millisecond)
	cancel4()
	time.Sleep(20 * time.Millisecond)

	mu4.Lock()
	fmt.Printf("\nForward 수신 (/tasks/*): %d건\n", len(received4))
	for _, env := range received4 {
		fmt.Printf("  topic=%s ns=%s event=%v\n", env.Topic, env.Namespace, env.Event)
	}
	mu4.Unlock()
	fmt.Println()

	// --- 데모 4: 네임스페이스 격리 ---
	fmt.Println("--- 데모 4: 네임스페이스별 이벤트 ---")
	subCtx5, cancel5 := context.WithCancel(ctx)
	allCh2, _ := exchange.Subscribe(subCtx5)

	var received5 []*Envelope
	var mu5 sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		for env := range allCh2 {
			mu5.Lock()
			received5 = append(received5, env)
			mu5.Unlock()
		}
	}()

	// 서로 다른 네임스페이스에서 이벤트 발행
	defaultCtx := WithNamespace(ctx, "default")
	prodCtx := WithNamespace(ctx, "production")

	exchange.Publish(defaultCtx, "/containers/create", ContainerCreate{ID: "d1", Image: "nginx"})
	exchange.Publish(prodCtx, "/containers/create", ContainerCreate{ID: "p1", Image: "myapp:v1"})
	exchange.Publish(defaultCtx, "/images/create", ImageCreate{Name: "nginx:latest"})
	exchange.Publish(prodCtx, "/images/delete", ImageDelete{Name: "myapp:v0.9"})

	time.Sleep(50 * time.Millisecond)
	cancel5()
	time.Sleep(20 * time.Millisecond)

	mu5.Lock()
	fmt.Println("모든 이벤트 (네임스페이스 포함):")
	for _, env := range received5 {
		fmt.Printf("  ns=%-12s topic=%-20s event=%v\n", env.Namespace, env.Topic, env.Event)
	}
	mu5.Unlock()
	fmt.Println()

	// --- 데모 5: Publish 유효성 검증 ---
	fmt.Println("--- 데모 5: Publish 유효성 검증 ---")

	// 네임스페이스 없이 Publish
	err = exchange.Publish(ctx, "/test", "event")
	fmt.Printf("네임스페이스 없는 Publish: %v\n", err)

	// 잘못된 토픽
	err = exchange.Publish(nsCtx, "", "event")
	fmt.Printf("빈 토픽 Publish: %v\n", err)

	err = exchange.Publish(nsCtx, "no-slash", "event")
	fmt.Printf("'/'로 시작하지 않는 토픽: %v\n", err)

	err = exchange.Publish(nsCtx, "/", "event")
	fmt.Printf("'/'만 있는 토픽: %v\n", err)

	// 올바른 Publish
	err = exchange.Publish(nsCtx, "/snapshots/remove", SnapshotRemove{Key: "snap-1", Snapshotter: "overlayfs"})
	fmt.Printf("올바른 Publish: err=%v\n", err)
	fmt.Println()

	// --- 데모 6: 다중 구독자 동시 수신 ---
	fmt.Println("--- 데모 6: 다중 구독자 동시 수신 ---")
	const numSubscribers = 3
	ctxMulti, cancelMulti := context.WithCancel(ctx)

	counts := make([]int, numSubscribers)
	var countMu sync.Mutex

	for i := 0; i < numSubscribers; i++ {
		ch, _ := exchange.Subscribe(ctxMulti)
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range ch {
				countMu.Lock()
				counts[idx]++
				countMu.Unlock()
			}
		}()
	}

	// 이벤트 10개 발행
	for j := 0; j < 10; j++ {
		exchange.Publish(nsCtx, "/containers/create",
			ContainerCreate{ID: fmt.Sprintf("multi-%d", j), Image: "test"})
	}

	time.Sleep(50 * time.Millisecond)
	cancelMulti()
	time.Sleep(20 * time.Millisecond)

	countMu.Lock()
	for i, c := range counts {
		fmt.Printf("구독자 %d: %d건 수신\n", i+1, c)
	}
	countMu.Unlock()
	fmt.Println("=> 모든 구독자가 동일한 이벤트를 수신 (Broadcast)")
	fmt.Println()

	// --- 데모 7: 이벤트 흐름 아키텍처 ---
	fmt.Println("--- 데모 7: containerd 이벤트 아키텍처 ---")
	fmt.Println(`
  ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
  │  API Server │     │ Image Store │     │    Shim     │
  │  (Publish)  │     │  (Publish)  │     │  (Forward)  │
  └──────┬──────┘     └──────┬──────┘     └──────┬──────┘
         │                   │                    │
         └───────────────────┼────────────────────┘
                             │
                      ┌──────▼──────┐
                      │  Exchange   │
                      │ (Broadcast) │
                      └──────┬──────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
       ┌──────▼──────┐ ┌────▼────┐ ┌───────▼───────┐
       │ Subscriber  │ │ Filter  │ │   GC/Events   │
       │ (gRPC 클라이언트)│ │(/tasks/*)│ │(metadata 변경) │
       └─────────────┘ └─────────┘ └───────────────┘
`)

	wg.Wait()
}
