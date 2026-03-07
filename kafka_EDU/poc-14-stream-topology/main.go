package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kafka Streams 토폴로지 처리 시뮬레이션
//
// 참조: streams/src/main/java/org/apache/kafka/streams/Topology.java
//       streams/src/main/java/org/apache/kafka/streams/processor/internals/StreamThread.java
//       streams/src/main/java/org/apache/kafka/streams/processor/api/ProcessorContext.java
//
// Kafka Streams는 DAG 토폴로지로 스트림 처리를 수행한다.
// Source -> Processor(들) -> Sink 구조로 데이터가 흐른다.
// =============================================================================

// Record는 스트림을 통해 흐르는 하나의 레코드이다.
type Record struct {
	Key       string
	Value     string
	Timestamp time.Time
	Topic     string
	Partition int
	Offset    int64
}

func (r Record) String() string {
	return fmt.Sprintf("{key=%s, value=%s}", r.Key, r.Value)
}

// StateStore는 상태 유지 처리를 위한 인메모리 상태 저장소이다.
// Kafka Streams의 StateStore 인터페이스를 시뮬레이션한다.
type StateStore struct {
	mu   sync.RWMutex
	name string
	data map[string]string
}

func NewStateStore(name string) *StateStore {
	return &StateStore{
		name: name,
		data: make(map[string]string),
	}
}

func (s *StateStore) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *StateStore) Put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *StateStore) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copy := make(map[string]string)
	for k, v := range s.data {
		copy[k] = v
	}
	return copy
}

// ProcessorContext는 프로세서에게 제공되는 컨텍스트이다.
// streams/src/main/java/org/apache/kafka/streams/processor/api/ProcessorContext에 해당한다.
type ProcessorContext struct {
	// 현재 처리 중인 레코드 정보
	currentRecord *Record
	// forward()로 다음 노드에 전달
	forwardFunc func(record Record)
	// 상태 저장소 접근
	stateStores map[string]*StateStore
}

func (ctx *ProcessorContext) Forward(record Record) {
	if ctx.forwardFunc != nil {
		ctx.forwardFunc(record)
	}
}

func (ctx *ProcessorContext) GetStateStore(name string) *StateStore {
	if store, ok := ctx.stateStores[name]; ok {
		return store
	}
	return nil
}

// Node는 토폴로지 DAG의 노드 인터페이스이다.
type Node interface {
	Name() string
	Process(record Record)
	AddChild(node Node)
	Children() []Node
}

// SourceNode는 토픽에서 레코드를 읽어오는 소스 노드이다.
// Topology.java의 addSource()에 해당한다.
type SourceNode struct {
	name     string
	topics   []string
	children []Node
}

func NewSourceNode(name string, topics []string) *SourceNode {
	return &SourceNode{
		name:     name,
		topics:   topics,
		children: make([]Node, 0),
	}
}

func (s *SourceNode) Name() string      { return s.name }
func (s *SourceNode) Children() []Node   { return s.children }
func (s *SourceNode) AddChild(node Node) { s.children = append(s.children, node) }

func (s *SourceNode) Process(record Record) {
	fmt.Printf("    [%s] 소스에서 수신: %s\n", s.name, record)
	// 모든 자식 노드에 forward
	for _, child := range s.children {
		child.Process(record)
	}
}

// ProcessorNode는 변환/집계 등을 수행하는 프로세서 노드이다.
// Topology.java의 addProcessor()에 해당한다.
type ProcessorNode struct {
	name     string
	children []Node
	ctx      *ProcessorContext
	// 프로세서 로직 (함수로 주입)
	processFunc func(ctx *ProcessorContext, record Record)
}

func NewProcessorNode(name string, processFunc func(ctx *ProcessorContext, record Record)) *ProcessorNode {
	p := &ProcessorNode{
		name:        name,
		children:    make([]Node, 0),
		processFunc: processFunc,
	}
	p.ctx = &ProcessorContext{
		stateStores: make(map[string]*StateStore),
		forwardFunc: func(record Record) {
			for _, child := range p.children {
				child.Process(record)
			}
		},
	}
	return p
}

func (p *ProcessorNode) Name() string      { return p.name }
func (p *ProcessorNode) Children() []Node   { return p.children }
func (p *ProcessorNode) AddChild(node Node) { p.children = append(p.children, node) }

func (p *ProcessorNode) AddStateStore(store *StateStore) {
	p.ctx.stateStores[store.name] = store
}

func (p *ProcessorNode) Process(record Record) {
	p.ctx.currentRecord = &record
	p.processFunc(p.ctx, record)
}

// SinkNode는 결과를 출력 토픽에 쓰는 싱크 노드이다.
// Topology.java의 addSink()에 해당한다.
type SinkNode struct {
	name      string
	topic     string
	children  []Node
	collected []Record // 출력된 레코드 수집
}

func NewSinkNode(name string, topic string) *SinkNode {
	return &SinkNode{
		name:      name,
		topic:     topic,
		children:  make([]Node, 0),
		collected: make([]Record, 0),
	}
}

func (s *SinkNode) Name() string      { return s.name }
func (s *SinkNode) Children() []Node   { return s.children }
func (s *SinkNode) AddChild(node Node) { s.children = append(s.children, node) }

func (s *SinkNode) Process(record Record) {
	record.Topic = s.topic
	s.collected = append(s.collected, record)
	fmt.Printf("    [%s] 싱크 출력 -> '%s': %s\n", s.name, s.topic, record)
}

// Topology는 스트림 처리 토폴로지를 구성하는 빌더이다.
// streams/src/main/java/org/apache/kafka/streams/Topology.java에 해당한다.
type Topology struct {
	nodes       map[string]Node
	sourceNodes map[string]*SourceNode // topic -> source node
	order       []string              // 노드 추가 순서
}

func NewTopology() *Topology {
	return &Topology{
		nodes:       make(map[string]Node),
		sourceNodes: make(map[string]*SourceNode),
		order:       make([]string, 0),
	}
}

// AddSource는 소스 노드를 추가한다.
func (t *Topology) AddSource(name string, topics ...string) *Topology {
	node := NewSourceNode(name, topics)
	t.nodes[name] = node
	t.order = append(t.order, name)
	for _, topic := range topics {
		t.sourceNodes[topic] = node
	}
	return t
}

// AddProcessor는 프로세서 노드를 추가하고 부모 노드에 연결한다.
func (t *Topology) AddProcessor(name string, processFunc func(ctx *ProcessorContext, record Record), parentNames ...string) *Topology {
	node := NewProcessorNode(name, processFunc)
	t.nodes[name] = node
	t.order = append(t.order, name)
	for _, parentName := range parentNames {
		if parent, ok := t.nodes[parentName]; ok {
			parent.AddChild(node)
		}
	}
	return t
}

// AddSink는 싱크 노드를 추가하고 부모 노드에 연결한다.
func (t *Topology) AddSink(name string, topic string, parentNames ...string) *Topology {
	node := NewSinkNode(name, topic)
	t.nodes[name] = node
	t.order = append(t.order, name)
	for _, parentName := range parentNames {
		if parent, ok := t.nodes[parentName]; ok {
			parent.AddChild(node)
		}
	}
	return t
}

// AddStateStore는 프로세서에 상태 저장소를 연결한다.
func (t *Topology) AddStateStore(store *StateStore, processorNames ...string) *Topology {
	for _, procName := range processorNames {
		if node, ok := t.nodes[procName]; ok {
			if pn, ok := node.(*ProcessorNode); ok {
				pn.AddStateStore(store)
			}
		}
	}
	return t
}

// Describe는 토폴로지 구조를 문자열로 출력한다.
func (t *Topology) Describe() string {
	var sb strings.Builder
	sb.WriteString("토폴로지 구조:\n")
	for _, name := range t.order {
		node := t.nodes[name]
		switch n := node.(type) {
		case *SourceNode:
			sb.WriteString(fmt.Sprintf("  Source:    %s (topics: %v)\n", n.name, n.topics))
		case *ProcessorNode:
			childNames := make([]string, 0)
			for _, c := range n.children {
				childNames = append(childNames, c.Name())
			}
			stores := make([]string, 0)
			for sn := range n.ctx.stateStores {
				stores = append(stores, sn)
			}
			sb.WriteString(fmt.Sprintf("  Processor: %s -> [%s]", n.name, strings.Join(childNames, ", ")))
			if len(stores) > 0 {
				sb.WriteString(fmt.Sprintf(" (stores: %v)", stores))
			}
			sb.WriteString("\n")
		case *SinkNode:
			sb.WriteString(fmt.Sprintf("  Sink:     %s (topic: %s)\n", n.name, n.topic))
		}
	}
	return sb.String()
}

// StreamThread는 토폴로지를 실행하는 스레드를 시뮬레이션한다.
// StreamThread.java에 해당한다.
type StreamThread struct {
	topology *Topology
}

func NewStreamThread(topology *Topology) *StreamThread {
	return &StreamThread{topology: topology}
}

// ProcessRecord는 하나의 레코드를 해당 토픽의 소스 노드에 주입한다.
func (st *StreamThread) ProcessRecord(topic string, record Record) {
	record.Topic = topic
	if source, ok := st.topology.sourceNodes[topic]; ok {
		source.Process(record)
	} else {
		fmt.Printf("    [경고] 토픽 '%s'에 대한 소스 노드 없음\n", topic)
	}
}

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Kafka Streams 토폴로지 처리 시뮬레이션                     ║")
	fmt.Println("║  참조: Topology.java, StreamThread.java, ProcessorContext           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// =========================================================================
	// 시나리오 1: 기본 Filter + Map 파이프라인
	// =========================================================================
	printSeparator("시나리오 1: Filter -> Map 파이프라인")
	fmt.Println("Source -> Filter(금액 > 100) -> Map(대문자 변환) -> Sink")
	fmt.Println()

	topo1 := NewTopology()
	topo1.AddSource("source", "orders")

	// Filter: 금액 100 초과만 통과
	topo1.AddProcessor("filter", func(ctx *ProcessorContext, record Record) {
		// Value에 금액이 포함되어 있다고 가정
		if !strings.Contains(record.Value, "low") {
			fmt.Printf("    [filter] 통과: %s\n", record)
			ctx.Forward(record)
		} else {
			fmt.Printf("    [filter] 필터링됨: %s\n", record)
		}
	}, "source")

	// Map: 값을 대문자로 변환
	topo1.AddProcessor("mapper", func(ctx *ProcessorContext, record Record) {
		transformed := Record{
			Key:       record.Key,
			Value:     strings.ToUpper(record.Value),
			Timestamp: record.Timestamp,
		}
		fmt.Printf("    [mapper] 변환: %s -> %s\n", record.Value, transformed.Value)
		ctx.Forward(transformed)
	}, "filter")

	topo1.AddSink("sink", "processed-orders", "mapper")

	fmt.Println(topo1.Describe())
	fmt.Println("--- 레코드 처리 ---")

	thread1 := NewStreamThread(topo1)
	records := []Record{
		{Key: "order-1", Value: "laptop-high-500"},
		{Key: "order-2", Value: "pen-low-5"},
		{Key: "order-3", Value: "phone-high-300"},
		{Key: "order-4", Value: "eraser-low-2"},
		{Key: "order-5", Value: "monitor-high-800"},
	}

	for _, r := range records {
		fmt.Printf("\n  입력: %s\n", r)
		thread1.ProcessRecord("orders", r)
	}

	// =========================================================================
	// 시나리오 2: Stateful 집계 (Word Count)
	// =========================================================================
	printSeparator("시나리오 2: Stateful 처리 - Word Count")
	fmt.Println("Source -> WordSplitter -> Counter(StateStore) -> Sink")
	fmt.Println()

	wordCountStore := NewStateStore("word-counts")

	topo2 := NewTopology()
	topo2.AddSource("text-source", "sentences")

	// Word Splitter: 문장을 단어로 분리하여 forward
	topo2.AddProcessor("splitter", func(ctx *ProcessorContext, record Record) {
		words := strings.Fields(record.Value)
		for _, word := range words {
			word = strings.ToLower(strings.Trim(word, ".,!?"))
			ctx.Forward(Record{Key: word, Value: word, Timestamp: record.Timestamp})
		}
	}, "text-source")

	// Counter: 상태 저장소를 사용하여 단어 카운트
	topo2.AddProcessor("counter", func(ctx *ProcessorContext, record Record) {
		store := ctx.GetStateStore("word-counts")
		if store == nil {
			return
		}
		count := "1"
		if existing, ok := store.Get(record.Key); ok {
			n := 0
			fmt.Sscanf(existing, "%d", &n)
			count = fmt.Sprintf("%d", n+1)
		}
		store.Put(record.Key, count)
		ctx.Forward(Record{Key: record.Key, Value: count, Timestamp: record.Timestamp})
	}, "splitter")

	topo2.AddSink("count-sink", "word-count-output", "counter")
	topo2.AddStateStore(wordCountStore, "counter")

	fmt.Println(topo2.Describe())
	fmt.Println("--- 레코드 처리 ---")

	thread2 := NewStreamThread(topo2)
	sentences := []Record{
		{Key: "s1", Value: "kafka streams is great"},
		{Key: "s2", Value: "kafka is fast and kafka is reliable"},
		{Key: "s3", Value: "streams processing is great"},
	}

	for _, s := range sentences {
		fmt.Printf("\n  입력: %s\n", s)
		thread2.ProcessRecord("sentences", s)
	}

	fmt.Println("\n--- Word Count 상태 저장소 최종 결과 ---")
	for word, count := range wordCountStore.All() {
		fmt.Printf("  '%s' -> %s\n", word, count)
	}

	// =========================================================================
	// 시나리오 3: 분기(Branch) 토폴로지
	// =========================================================================
	printSeparator("시나리오 3: 분기(Branch) 토폴로지")
	fmt.Println("Source -> Router -> [HighPriority Sink, LowPriority Sink]")
	fmt.Println()

	topo3 := NewTopology()
	topo3.AddSource("event-source", "events")

	// 라우터: 이벤트 유형에 따라 다른 싱크로 분기
	topo3.AddProcessor("router", func(ctx *ProcessorContext, record Record) {
		fmt.Printf("    [router] 라우팅: %s\n", record)
		ctx.Forward(record) // 모든 자식 노드에 전달
	}, "event-source")

	// High priority 필터
	topo3.AddProcessor("high-filter", func(ctx *ProcessorContext, record Record) {
		if strings.Contains(record.Value, "ERROR") || strings.Contains(record.Value, "CRITICAL") {
			fmt.Printf("    [high-filter] 고우선순위: %s\n", record)
			ctx.Forward(record)
		}
	}, "router")

	// Low priority 필터
	topo3.AddProcessor("low-filter", func(ctx *ProcessorContext, record Record) {
		if strings.Contains(record.Value, "INFO") || strings.Contains(record.Value, "DEBUG") {
			fmt.Printf("    [low-filter] 저우선순위: %s\n", record)
			ctx.Forward(record)
		}
	}, "router")

	topo3.AddSink("high-sink", "alerts", "high-filter")
	topo3.AddSink("low-sink", "logs", "low-filter")

	fmt.Println(topo3.Describe())
	fmt.Println("--- 레코드 처리 ---")

	thread3 := NewStreamThread(topo3)
	events := []Record{
		{Key: "e1", Value: "INFO: User logged in"},
		{Key: "e2", Value: "ERROR: Disk full"},
		{Key: "e3", Value: "DEBUG: Query executed"},
		{Key: "e4", Value: "CRITICAL: Service down"},
		{Key: "e5", Value: "INFO: Config reloaded"},
	}

	for _, e := range events {
		fmt.Printf("\n  입력: %s\n", e)
		thread3.ProcessRecord("events", e)
	}

	// =========================================================================
	// 시나리오 4: 다중 소스 합류(Merge)
	// =========================================================================
	printSeparator("시나리오 4: 다중 소스 합류(Merge)")
	fmt.Println("Source-A (topic-a) -> Merger -> Sink")
	fmt.Println("Source-B (topic-b) -> Merger -> Sink")
	fmt.Println()

	topo4 := NewTopology()
	topo4.AddSource("source-a", "topic-a")
	topo4.AddSource("source-b", "topic-b")

	// Merger: 여러 소스의 레코드를 합치고 출처를 표시
	topo4.AddProcessor("merger", func(ctx *ProcessorContext, record Record) {
		merged := Record{
			Key:       record.Key,
			Value:     fmt.Sprintf("[from:%s] %s", record.Topic, record.Value),
			Timestamp: record.Timestamp,
		}
		fmt.Printf("    [merger] 합류: %s\n", merged)
		ctx.Forward(merged)
	}, "source-a", "source-b")

	topo4.AddSink("merged-sink", "merged-output", "merger")

	fmt.Println(topo4.Describe())
	fmt.Println("--- 레코드 처리 ---")

	thread4 := NewStreamThread(topo4)
	fmt.Println("\n  topic-a에서 수신:")
	thread4.ProcessRecord("topic-a", Record{Key: "a1", Value: "data-from-A"})
	fmt.Println("\n  topic-b에서 수신:")
	thread4.ProcessRecord("topic-b", Record{Key: "b1", Value: "data-from-B"})
	fmt.Println("\n  topic-a에서 수신:")
	thread4.ProcessRecord("topic-a", Record{Key: "a2", Value: "more-data-A"})

	// =========================================================================
	// DAG 시각화
	// =========================================================================
	printSeparator("토폴로지 DAG 시각화")
	fmt.Println(`
  시나리오 1: 선형 파이프라인
  ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐
  │ Source   │───>│ Filter  │───>│ Mapper  │───>│  Sink   │
  │(orders) │    │(>100)   │    │(toUpper)│    │(output) │
  └─────────┘    └─────────┘    └─────────┘    └─────────┘

  시나리오 2: Stateful 파이프라인
  ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐
  │ Source   │───>│Splitter │───>│Counter  │───>│  Sink   │
  │(text)   │    │(words)  │    │(count)  │    │(output) │
  └─────────┘    └─────────┘    └────┬────┘    └─────────┘
                                     │
                                ┌────┴────┐
                                │  State  │
                                │  Store  │
                                └─────────┘

  시나리오 3: 분기(Branch) 토폴로지
  ┌─────────┐    ┌─────────┐    ┌──────────┐    ┌─────────┐
  │ Source   │───>│ Router  │───>│HighFilter│───>│AlertSink│
  │(events) │    │         │    └──────────┘    └─────────┘
  └─────────┘    │         │    ┌──────────┐    ┌─────────┐
                 │         │───>│LowFilter │───>│ LogSink │
                 └─────────┘    └──────────┘    └─────────┘

  시나리오 4: 합류(Merge) 토폴로지
  ┌─────────┐
  │Source-A  │───┐
  │(topic-a) │   │    ┌─────────┐    ┌─────────┐
  └─────────┘   ├───>│ Merger  │───>│  Sink   │
  ┌─────────┐   │    └─────────┘    └─────────┘
  │Source-B  │───┘
  │(topic-b) │
  └─────────┘`)

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("핵심 요약")
	fmt.Println(`
  1. Topology는 Source -> Processor -> Sink의 DAG 구조
     - Topology.java의 addSource/addProcessor/addSink로 구성

  2. SourceNode는 입력 토픽에서 레코드를 읽어 하위 노드로 전달
     - addSource("name", "topic1", "topic2")

  3. ProcessorNode는 변환/필터링/집계 등 비즈니스 로직 수행
     - ProcessorContext.forward()로 하위 노드에 결과 전달
     - StateStore를 통해 상태 유지 처리 가능

  4. SinkNode는 처리 결과를 출력 토픽에 기록
     - addSink("name", "output-topic", "parent-processor")

  5. StreamThread가 토폴로지를 실행하며 레코드를 소스부터 주입
     - StreamThread.java의 runOnce()가 poll -> process -> commit 루프 수행`)
}
