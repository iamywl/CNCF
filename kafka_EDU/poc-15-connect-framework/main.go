package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kafka Connect Source/Sink 런타임 시뮬레이션
//
// 참조: connect/runtime/src/main/java/org/apache/kafka/connect/runtime/Worker.java
//       connect/api/src/main/java/org/apache/kafka/connect/source/SourceTask.java
//       connect/api/src/main/java/org/apache/kafka/connect/sink/SinkTask.java
//       connect/runtime/src/main/java/org/apache/kafka/connect/runtime/TransformationChain.java
//
// Kafka Connect는 Connector + Task 구조로 외부 시스템과 Kafka 간 데이터를 이동한다.
// =============================================================================

// SourceRecord는 소스 커넥터가 생성하는 레코드이다.
// connect/api/src/main/java/org/apache/kafka/connect/source/SourceRecord.java에 해당한다.
type SourceRecord struct {
	// 소스 파티션/오프셋: 소스 시스템에서의 위치 추적용
	SourcePartition map[string]string
	SourceOffset    map[string]string
	// 대상 Kafka 토픽 정보
	Topic string
	Key   string
	Value string
	// 메타데이터
	Timestamp time.Time
}

func (r SourceRecord) String() string {
	return fmt.Sprintf("{topic=%s, key=%s, value=%s}", r.Topic, r.Key, r.Value)
}

// SinkRecord는 싱크 커넥터가 소비하는 레코드이다.
type SinkRecord struct {
	Topic     string
	Partition int
	Offset    int64
	Key       string
	Value     string
	Timestamp time.Time
}

func (r SinkRecord) String() string {
	return fmt.Sprintf("{topic=%s, partition=%d, offset=%d, key=%s, value=%s}",
		r.Topic, r.Partition, r.Offset, r.Key, r.Value)
}

// Connector는 커넥터 인터페이스이다.
// connect/api/src/main/java/org/apache/kafka/connect/connector/Connector.java에 해당한다.
type Connector interface {
	Start(config map[string]string)
	TaskConfigs(maxTasks int) []map[string]string
	Stop()
	TaskClass() string
}

// SourceTask는 외부 시스템에서 데이터를 가져오는 태스크이다.
// connect/api/src/main/java/org/apache/kafka/connect/source/SourceTask.java에 해당한다.
type SourceTask interface {
	Start(props map[string]string)
	Poll() []SourceRecord
	Stop()
	Commit()
}

// SinkTask는 Kafka 데이터를 외부 시스템에 쓰는 태스크이다.
// connect/api/src/main/java/org/apache/kafka/connect/sink/SinkTask.java에 해당한다.
type SinkTask interface {
	Start(props map[string]string)
	Put(records []SinkRecord)
	Flush(offsets map[string]int64)
	Stop()
}

// Transform은 단일 메시지 변환(SMT)을 나타낸다.
// TransformationChain.java의 apply 패턴에 기반한다.
type Transform interface {
	Apply(record SourceRecord) *SourceRecord // nil이면 필터링됨
	Name() string
}

// TransformationChain은 여러 SMT를 순서대로 적용하는 체인이다.
// connect/runtime/src/main/java/org/apache/kafka/connect/runtime/TransformationChain.java에 해당한다.
type TransformationChain struct {
	transforms []Transform
}

func NewTransformationChain(transforms ...Transform) *TransformationChain {
	return &TransformationChain{transforms: transforms}
}

// Apply는 체인의 모든 변환을 순서대로 적용한다.
// 원본: TransformationChain.java의 apply() 메서드
// - 각 TransformationStage를 순회하며 적용
// - null이 반환되면 중단 (레코드가 필터링됨)
func (tc *TransformationChain) Apply(record SourceRecord) *SourceRecord {
	current := &record
	for _, t := range tc.transforms {
		current = t.Apply(*current)
		if current == nil {
			return nil // 필터링됨
		}
	}
	return current
}

// --- SMT 구현체들 ---

// ValueToUpperCase는 값을 대문자로 변환하는 SMT이다.
type ValueToUpperCase struct{}

func (t *ValueToUpperCase) Name() string { return "ValueToUpperCase" }
func (t *ValueToUpperCase) Apply(record SourceRecord) *SourceRecord {
	record.Value = strings.ToUpper(record.Value)
	return &record
}

// FilterByKey는 특정 키 접두사를 가진 레코드만 통과시키는 SMT이다.
type FilterByKey struct {
	Prefix string
}

func (t *FilterByKey) Name() string { return "FilterByKey(" + t.Prefix + ")" }
func (t *FilterByKey) Apply(record SourceRecord) *SourceRecord {
	if strings.HasPrefix(record.Key, t.Prefix) {
		return &record
	}
	return nil
}

// AddPrefix는 값에 접두사를 추가하는 SMT이다.
type AddPrefix struct {
	Prefix string
}

func (t *AddPrefix) Name() string { return "AddPrefix(" + t.Prefix + ")" }
func (t *AddPrefix) Apply(record SourceRecord) *SourceRecord {
	record.Value = t.Prefix + record.Value
	return &record
}

// --- 소스 커넥터/태스크 구현: 파일 소스 시뮬레이션 ---

// FileSourceConnector는 파일에서 데이터를 읽는 커넥터이다.
type FileSourceConnector struct {
	config map[string]string
}

func (c *FileSourceConnector) Start(config map[string]string) {
	c.config = config
	fmt.Printf("  [FileSourceConnector] 시작: file=%s, topic=%s\n",
		config["file"], config["topic"])
}

func (c *FileSourceConnector) TaskConfigs(maxTasks int) []map[string]string {
	configs := make([]map[string]string, maxTasks)
	for i := 0; i < maxTasks; i++ {
		configs[i] = map[string]string{
			"file":    c.config["file"],
			"topic":   c.config["topic"],
			"task.id": fmt.Sprintf("%d", i),
		}
	}
	return configs
}

func (c *FileSourceConnector) Stop() {
	fmt.Println("  [FileSourceConnector] 중지")
}

func (c *FileSourceConnector) TaskClass() string { return "FileSourceTask" }

// FileSourceTask는 파일에서 줄 단위로 읽는 소스 태스크이다.
type FileSourceTask struct {
	topic    string
	lines    []string // 시뮬레이션된 파일 내용
	position int      // 현재 읽기 위치
	taskID   string
}

func (t *FileSourceTask) Start(props map[string]string) {
	t.topic = props["topic"]
	t.taskID = props["task.id"]
	// 시뮬레이션: 파일 내용을 미리 로드
	t.lines = []string{
		"user-1,login,2024-01-01",
		"user-2,purchase,2024-01-01",
		"admin-1,config_change,2024-01-01",
		"user-3,logout,2024-01-01",
		"admin-2,deploy,2024-01-01",
		"user-1,purchase,2024-01-02",
	}
	t.position = 0
	fmt.Printf("  [FileSourceTask-%s] 시작: topic=%s, 총 %d줄\n",
		t.taskID, t.topic, len(t.lines))
}

// Poll은 SourceTask.java의 poll() 메서드에 해당한다.
// 새 데이터가 있으면 SourceRecord 배치를 반환한다.
func (t *FileSourceTask) Poll() []SourceRecord {
	if t.position >= len(t.lines) {
		return nil // 더 이상 데이터 없음
	}

	// 한 번에 최대 2개씩 가져옴
	batchSize := 2
	end := t.position + batchSize
	if end > len(t.lines) {
		end = len(t.lines)
	}

	records := make([]SourceRecord, 0)
	for i := t.position; i < end; i++ {
		parts := strings.Split(t.lines[i], ",")
		key := parts[0]
		value := t.lines[i]

		records = append(records, SourceRecord{
			SourcePartition: map[string]string{"file": "data.csv"},
			SourceOffset:    map[string]string{"position": fmt.Sprintf("%d", i)},
			Topic:           t.topic,
			Key:             key,
			Value:           value,
			Timestamp:       time.Now(),
		})
	}
	t.position = end
	return records
}

func (t *FileSourceTask) Commit() {
	fmt.Printf("  [FileSourceTask-%s] 오프셋 커밋: position=%d\n", t.taskID, t.position)
}

func (t *FileSourceTask) Stop() {
	fmt.Printf("  [FileSourceTask-%s] 중지\n", t.taskID)
}

// --- 싱크 커넥터/태스크 구현: 콘솔 싱크 시뮬레이션 ---

// ConsoleSinkConnector는 데이터를 콘솔에 출력하는 싱크 커넥터이다.
type ConsoleSinkConnector struct {
	config map[string]string
}

func (c *ConsoleSinkConnector) Start(config map[string]string) {
	c.config = config
	fmt.Printf("  [ConsoleSinkConnector] 시작: topics=%s\n", config["topics"])
}

func (c *ConsoleSinkConnector) TaskConfigs(maxTasks int) []map[string]string {
	configs := make([]map[string]string, maxTasks)
	for i := 0; i < maxTasks; i++ {
		configs[i] = map[string]string{
			"topics":  c.config["topics"],
			"task.id": fmt.Sprintf("%d", i),
		}
	}
	return configs
}

func (c *ConsoleSinkConnector) Stop()           { fmt.Println("  [ConsoleSinkConnector] 중지") }
func (c *ConsoleSinkConnector) TaskClass() string { return "ConsoleSinkTask" }

// ConsoleSinkTask는 SinkTask.java의 put() 메서드를 시뮬레이션한다.
type ConsoleSinkTask struct {
	taskID    string
	buffer    []SinkRecord
	flushed   int
}

func (t *ConsoleSinkTask) Start(props map[string]string) {
	t.taskID = props["task.id"]
	t.buffer = make([]SinkRecord, 0)
	fmt.Printf("  [ConsoleSinkTask-%s] 시작: topics=%s\n", t.taskID, props["topics"])
}

// Put은 SinkTask.java의 put() 메서드에 해당한다.
func (t *ConsoleSinkTask) Put(records []SinkRecord) {
	for _, r := range records {
		fmt.Printf("  [ConsoleSinkTask-%s] PUT: %s\n", t.taskID, r)
		t.buffer = append(t.buffer, r)
	}
}

// Flush는 SinkTask.java의 flush() 메서드에 해당한다.
func (t *ConsoleSinkTask) Flush(offsets map[string]int64) {
	t.flushed += len(t.buffer)
	fmt.Printf("  [ConsoleSinkTask-%s] FLUSH: %d 레코드 (누적: %d)\n",
		t.taskID, len(t.buffer), t.flushed)
	t.buffer = t.buffer[:0]
}

func (t *ConsoleSinkTask) Stop() {
	fmt.Printf("  [ConsoleSinkTask-%s] 중지 (총 flush: %d 레코드)\n", t.taskID, t.flushed)
}

// Worker는 커넥터와 태스크의 생명주기를 관리한다.
// connect/runtime/src/main/java/org/apache/kafka/connect/runtime/Worker.java에 해당한다.
type Worker struct {
	mu              sync.Mutex
	connectors      map[string]Connector
	sourceTasks     map[string]SourceTask
	sinkTasks       map[string]SinkTask
	transforms      map[string]*TransformationChain
	offsetStore     map[string]map[string]string // connectorName -> offset
	internalTopic   []SinkRecord                 // Kafka 토픽 시뮬레이션
}

func NewWorker() *Worker {
	return &Worker{
		connectors:    make(map[string]Connector),
		sourceTasks:   make(map[string]SourceTask),
		sinkTasks:     make(map[string]SinkTask),
		transforms:    make(map[string]*TransformationChain),
		offsetStore:   make(map[string]map[string]string),
		internalTopic: make([]SinkRecord, 0),
	}
}

// StartSourceConnector는 소스 커넥터와 태스크를 시작한다.
func (w *Worker) StartSourceConnector(name string, connector Connector, task SourceTask,
	config map[string]string, chain *TransformationChain) {

	w.mu.Lock()
	defer w.mu.Unlock()

	fmt.Printf("\n--- Worker: 소스 커넥터 '%s' 시작 ---\n", name)

	connector.Start(config)
	w.connectors[name] = connector

	taskConfigs := connector.TaskConfigs(1)
	task.Start(taskConfigs[0])
	w.sourceTasks[name] = task

	if chain != nil {
		w.transforms[name] = chain
		fmt.Printf("  [Worker] SMT 체인 등록: %d개 변환\n", len(chain.transforms))
		for _, t := range chain.transforms {
			fmt.Printf("    - %s\n", t.Name())
		}
	}

	w.offsetStore[name] = make(map[string]string)
}

// StartSinkConnector는 싱크 커넥터와 태스크를 시작한다.
func (w *Worker) StartSinkConnector(name string, connector Connector, task SinkTask,
	config map[string]string) {

	w.mu.Lock()
	defer w.mu.Unlock()

	fmt.Printf("\n--- Worker: 싱크 커넥터 '%s' 시작 ---\n", name)

	connector.Start(config)
	w.connectors[name] = connector

	taskConfigs := connector.TaskConfigs(1)
	task.Start(taskConfigs[0])
	w.sinkTasks[name] = task
}

// PollSourceTask는 소스 태스크를 poll하고 SMT를 적용한다.
func (w *Worker) PollSourceTask(name string) int {
	w.mu.Lock()
	task, ok := w.sourceTasks[name]
	chain := w.transforms[name]
	w.mu.Unlock()

	if !ok {
		return 0
	}

	records := task.Poll()
	if len(records) == 0 {
		return 0
	}

	fmt.Printf("  [Worker] '%s' poll: %d 레코드 수신\n", name, len(records))

	produced := 0
	for _, record := range records {
		// SMT 체인 적용
		if chain != nil {
			transformed := chain.Apply(record)
			if transformed == nil {
				fmt.Printf("  [Worker] SMT 필터링됨: %s\n", record)
				continue
			}
			record = *transformed
			fmt.Printf("  [Worker] SMT 적용 후: %s\n", record)
		}

		// Kafka 토픽에 프로듀스 (시뮬레이션)
		sinkRecord := SinkRecord{
			Topic:     record.Topic,
			Partition: 0,
			Offset:    int64(len(w.internalTopic)),
			Key:       record.Key,
			Value:     record.Value,
			Timestamp: record.Timestamp,
		}
		w.internalTopic = append(w.internalTopic, sinkRecord)
		produced++

		// 소스 오프셋 저장
		w.mu.Lock()
		for k, v := range record.SourceOffset {
			w.offsetStore[name][k] = v
		}
		w.mu.Unlock()
	}

	// 오프셋 커밋
	task.Commit()
	return produced
}

// DeliverToSinkTask는 내부 토픽의 레코드를 싱크 태스크에 전달한다.
func (w *Worker) DeliverToSinkTask(name string, startOffset int) int {
	w.mu.Lock()
	task, ok := w.sinkTasks[name]
	w.mu.Unlock()

	if !ok {
		return 0
	}

	if startOffset >= len(w.internalTopic) {
		return 0
	}

	records := make([]SinkRecord, 0)
	for i := startOffset; i < len(w.internalTopic); i++ {
		records = append(records, w.internalTopic[i])
	}

	if len(records) > 0 {
		fmt.Printf("  [Worker] '%s'에 %d 레코드 전달\n", name, len(records))
		task.Put(records)
		task.Flush(map[string]int64{})
	}
	return len(records)
}

// StopAll은 모든 커넥터와 태스크를 중지한다.
func (w *Worker) StopAll() {
	fmt.Println("\n--- Worker: 모든 커넥터/태스크 중지 ---")
	for name, task := range w.sourceTasks {
		task.Stop()
		w.connectors[name].Stop()
	}
	for name, task := range w.sinkTasks {
		task.Stop()
		w.connectors[name].Stop()
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
	fmt.Println("║          Kafka Connect Source/Sink 런타임 시뮬레이션                ║")
	fmt.Println("║  참조: Worker.java, SourceTask.java, SinkTask.java,                ║")
	fmt.Println("║        TransformationChain.java                                     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	worker := NewWorker()

	// =========================================================================
	// 시나리오 1: 기본 Source -> Sink 파이프라인
	// =========================================================================
	printSeparator("시나리오 1: 기본 Source -> Sink 파이프라인")
	fmt.Println("FileSource에서 데이터를 읽어 Kafka를 거쳐 ConsoleSink로 전달한다.")

	// 소스 커넥터 시작
	worker.StartSourceConnector(
		"file-source",
		&FileSourceConnector{},
		&FileSourceTask{},
		map[string]string{
			"file":  "/data/input.csv",
			"topic": "raw-events",
		},
		nil, // SMT 없음
	)

	// 싱크 커넥터 시작
	worker.StartSinkConnector(
		"console-sink",
		&ConsoleSinkConnector{},
		&ConsoleSinkTask{},
		map[string]string{
			"topics": "raw-events",
		},
	)

	fmt.Println()
	fmt.Println("--- Poll 라운드 1 ---")
	offset := 0
	n := worker.PollSourceTask("file-source")
	worker.DeliverToSinkTask("console-sink", offset)
	offset += n

	fmt.Println()
	fmt.Println("--- Poll 라운드 2 ---")
	n = worker.PollSourceTask("file-source")
	worker.DeliverToSinkTask("console-sink", offset)
	offset += n

	fmt.Println()
	fmt.Println("--- Poll 라운드 3 ---")
	n = worker.PollSourceTask("file-source")
	worker.DeliverToSinkTask("console-sink", offset)
	offset += n

	// =========================================================================
	// 시나리오 2: SMT(Single Message Transform) 체인
	// =========================================================================
	printSeparator("시나리오 2: SMT 체인 적용")
	fmt.Println("TransformationChain으로 레코드를 필터링하고 변환한다.")
	fmt.Println("체인: FilterByKey('user-') -> AddPrefix('[PROCESSED] ') -> ValueToUpperCase")

	worker2 := NewWorker()

	// SMT 체인 구성
	chain := NewTransformationChain(
		&FilterByKey{Prefix: "user-"},
		&AddPrefix{Prefix: "[PROCESSED] "},
		&ValueToUpperCase{},
	)

	worker2.StartSourceConnector(
		"filtered-source",
		&FileSourceConnector{},
		&FileSourceTask{},
		map[string]string{
			"file":  "/data/input.csv",
			"topic": "filtered-events",
		},
		chain,
	)

	worker2.StartSinkConnector(
		"filtered-sink",
		&ConsoleSinkConnector{},
		&ConsoleSinkTask{},
		map[string]string{
			"topics": "filtered-events",
		},
	)

	fmt.Println()
	offset2 := 0
	for round := 1; round <= 3; round++ {
		fmt.Printf("--- Poll 라운드 %d ---\n", round)
		n := worker2.PollSourceTask("filtered-source")
		worker2.DeliverToSinkTask("filtered-sink", offset2)
		offset2 += n
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 3: 오프셋 추적
	// =========================================================================
	printSeparator("시나리오 3: 소스 오프셋 추적")
	fmt.Println("Source 커넥터는 소스 시스템의 위치를 추적하여 재시작 시 중복을 방지한다.")
	fmt.Println()

	fmt.Println("--- 소스 오프셋 상태 (worker1) ---")
	for name, offsets := range worker.offsetStore {
		fmt.Printf("  커넥터 '%s':\n", name)
		for k, v := range offsets {
			fmt.Printf("    %s = %s\n", k, v)
		}
	}

	fmt.Println()
	fmt.Println("--- 소스 오프셋 상태 (worker2, SMT 적용 후) ---")
	for name, offsets := range worker2.offsetStore {
		fmt.Printf("  커넥터 '%s':\n", name)
		for k, v := range offsets {
			fmt.Printf("    %s = %s\n", k, v)
		}
	}

	// =========================================================================
	// 정리
	// =========================================================================
	printSeparator("커넥터 정리")
	worker.StopAll()
	worker2.StopAll()

	// =========================================================================
	// 아키텍처 시각화
	// =========================================================================
	printSeparator("Kafka Connect 아키텍처")
	fmt.Println(`
  ┌──────────────────────────────────────────────────────────────────┐
  │                         Worker                                  │
  │                                                                  │
  │  ┌──────────────┐     ┌──────────────┐     ┌──────────────┐    │
  │  │ Source        │     │    Kafka      │     │ Sink         │    │
  │  │ Connector     │     │    Cluster    │     │ Connector    │    │
  │  │               │     │              │     │              │    │
  │  │ ┌──────────┐ │     │ ┌──────────┐ │     │ ┌──────────┐│    │
  │  │ │SourceTask│─┼──>──┼─│  Topic   │─┼──>──┼─│ SinkTask ││    │
  │  │ │ .poll()  │ │     │ │          │ │     │ │ .put()   ││    │
  │  │ └──────────┘ │     │ └──────────┘ │     │ └──────────┘│    │
  │  │       │      │     │              │     │              │    │
  │  │  ┌────┴────┐ │     │              │     │              │    │
  │  │  │   SMT   │ │     │              │     │              │    │
  │  │  │  Chain  │ │     │              │     │              │    │
  │  │  └─────────┘ │     │              │     │              │    │
  │  └──────────────┘     └──────────────┘     └──────────────┘    │
  │                                                                  │
  │  ┌──────────────────────────────────────────────────┐          │
  │  │             Offset Storage                        │          │
  │  │  source-partition -> source-offset                │          │
  │  └──────────────────────────────────────────────────┘          │
  └──────────────────────────────────────────────────────────────────┘

  SMT (Single Message Transform) 체인 처리 과정:
  ┌────────┐    ┌─────────┐    ┌───────────┐    ┌──────────┐
  │ Record │───>│ Filter  │───>│ AddPrefix │───>│ ToUpper  │──> 결과
  │ (원본) │    │(key검사)│    │([PROC])   │    │(대문자)  │
  └────────┘    └─────────┘    └───────────┘    └──────────┘
                     │
                   [null] → 필터링됨 (TransformationChain.apply에서 break)`)

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("핵심 요약")
	fmt.Println(`
  1. Connector = 설정 + TaskConfig 생성
     - start(config) -> taskConfigs(maxTasks) -> stop()
     - Connector 자체는 데이터를 처리하지 않음

  2. SourceTask.poll() = 외부 시스템에서 레코드 배치 가져오기
     - SourceRecord에 sourcePartition/sourceOffset 포함
     - 재시작 시 마지막 오프셋부터 재개 가능

  3. SinkTask.put(records) = Kafka에서 받은 레코드를 외부 시스템에 쓰기
     - flush()로 배치 커밋 보장

  4. TransformationChain = SMT 체인으로 레코드 변환
     - 순서대로 적용, null 반환 시 필터링 (체인 중단)
     - TransformationChain.java의 apply() 메서드 패턴

  5. Worker가 전체 생명주기 관리
     - Connector 시작/중지, Task 할당, 오프셋 커밋
     - Worker.java가 실제 런타임 오케스트레이션 수행`)
}
