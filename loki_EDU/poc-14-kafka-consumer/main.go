package main

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Loki PoC #14: Kafka 소비자 - 파티션 기반 로그 소비 및 블록 빌더
// =============================================================================
//
// Loki는 Kafka를 분산 로그 수집의 중간 버퍼로 사용할 수 있다.
// Distributor가 로그를 Kafka 파티션에 쓰면, Block Builder(또는 Ingester)가
// 파티션에서 로그를 읽어 청크를 구성하고 스토리지에 플러시한다.
//
// 핵심 개념:
// 1. 파티션 링: 테넌트 ID를 해싱하여 파티션에 라우팅
// 2. 블록 빌더: 로그 엔트리를 누적 → 청크 빌드 → 플러시
// 3. 컨슈머 그룹: 파티션을 컨슈머에게 분배, 리밸런싱
// 4. 오프셋 관리: 각 컨슈머가 처리한 위치 추적
//
// 참조: distributor kafka path, pkg/dataobj/consumer/

// =============================================================================
// 로그 엔트리 및 레코드
// =============================================================================

// LogEntry 는 하나의 로그 메시지를 나타낸다
type LogEntry struct {
	Timestamp time.Time
	TenantID  string
	Labels    map[string]string
	Line      string
}

// Record 는 Kafka 레코드를 시뮬레이션한다
type Record struct {
	Key       string   // 파티션 키 (테넌트 ID)
	Value     LogEntry // 로그 엔트리
	Offset    int64    // 파티션 내 오프셋
	Partition int      // 파티션 번호
}

// =============================================================================
// Kafka 파티션: 순서가 보장되는 로그 큐
// =============================================================================

// Partition 은 Kafka 파티션을 시뮬레이션한다
// 각 파티션은 순서가 보장되는 추가 전용(append-only) 로그
type Partition struct {
	mu       sync.Mutex
	id       int
	records  []Record
	offset   int64 // 다음 쓰기 오프셋
}

// Append 는 레코드를 파티션에 추가한다
func (p *Partition) Append(entry LogEntry) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	record := Record{
		Key:       entry.TenantID,
		Value:     entry,
		Offset:    p.offset,
		Partition: p.id,
	}
	p.records = append(p.records, record)
	p.offset++
	return record.Offset
}

// Fetch 는 지정된 오프셋부터 maxRecords 개의 레코드를 반환한다
func (p *Partition) Fetch(fromOffset int64, maxRecords int) []Record {
	p.mu.Lock()
	defer p.mu.Unlock()

	var result []Record
	for _, r := range p.records {
		if r.Offset >= fromOffset {
			result = append(result, r)
			if len(result) >= maxRecords {
				break
			}
		}
	}
	return result
}

// HighWatermark 는 파티션의 최신 오프셋을 반환한다
func (p *Partition) HighWatermark() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.offset
}

// =============================================================================
// Kafka 토픽: 여러 파티션으로 구성
// =============================================================================

// Topic 은 Kafka 토픽을 시뮬레이션한다
type Topic struct {
	name       string
	partitions []*Partition
}

// NewTopic 은 지정된 파티션 수의 토픽을 생성한다
func NewTopic(name string, numPartitions int) *Topic {
	partitions := make([]*Partition, numPartitions)
	for i := 0; i < numPartitions; i++ {
		partitions[i] = &Partition{id: i}
	}
	return &Topic{
		name:       name,
		partitions: partitions,
	}
}

// =============================================================================
// Producer: 파티션 링 기반 테넌트 라우팅
// =============================================================================
// Loki에서 Distributor는 테넌트 ID를 해싱하여 Kafka 파티션에 라우팅한다.
// 같은 테넌트의 로그는 항상 같은 파티션에 기록되어 순서가 보장된다.

// Producer 는 Kafka 프로듀서를 시뮬레이션한다
type Producer struct {
	topic *Topic
}

// NewProducer 는 새 프로듀서를 생성한다
func NewProducer(topic *Topic) *Producer {
	return &Producer{topic: topic}
}

// Send 는 로그 엔트리를 적절한 파티션에 전송한다
// 파티션 선택: 테넌트 ID의 FNV 해시 → 파티션 수로 모듈러 연산
func (p *Producer) Send(entry LogEntry) (partition int, offset int64) {
	// 테넌트 ID 기반 파티션 라우팅
	// Loki 실제 코드: 테넌트 ID를 해싱하여 파티션 결정
	partitionID := hashToPartition(entry.TenantID, len(p.topic.partitions))
	offset = p.topic.partitions[partitionID].Append(entry)
	return partitionID, offset
}

// hashToPartition 은 키를 해싱하여 파티션 번호를 결정한다
func hashToPartition(key string, numPartitions int) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32()) % numPartitions
}

// =============================================================================
// Chunk: 블록 빌더가 생성하는 청크
// =============================================================================

// Chunk 는 빌드된 로그 청크를 나타낸다
type Chunk struct {
	TenantID string
	Labels   string // 직렬화된 레이블
	Entries  []LogEntry
	MinTime  time.Time
	MaxTime  time.Time
	Size     int
}

// =============================================================================
// BlockBuilder: 로그 엔트리를 누적하여 청크를 빌드하고 플러시
// =============================================================================
// Loki 실제 코드: pkg/dataobj/consumer/flush.go, processor.go
//
// 블록 빌더의 역할:
// 1. Kafka 파티션에서 로그 엔트리를 읽음
// 2. 테넌트+레이블 조합별로 엔트리를 그룹화하여 누적
// 3. 일정 크기 또는 시간 초과 시 청크를 빌드하여 스토리지에 플러시
// 4. 플러시 성공 후 Kafka 오프셋을 커밋

// BlockBuilder 는 블록 빌더를 시뮬레이션한다
type BlockBuilder struct {
	mu             sync.Mutex
	maxEntries     int                    // 청크당 최대 엔트리 수
	flushInterval  time.Duration          // 플러시 주기
	buffers        map[string][]LogEntry  // 테넌트+레이블 → 엔트리 버퍼
	flushedChunks  []Chunk                // 플러시된 청크들
	flushCount     int                    // 플러시 횟수
}

// NewBlockBuilder 는 새 블록 빌더를 생성한다
func NewBlockBuilder(maxEntries int, flushInterval time.Duration) *BlockBuilder {
	return &BlockBuilder{
		maxEntries:    maxEntries,
		flushInterval: flushInterval,
		buffers:       make(map[string][]LogEntry),
	}
}

// bufferKey 는 테넌트 ID와 레이블로 버퍼 키를 생성한다
func bufferKey(tenantID string, labels map[string]string) string {
	// 레이블을 정렬하여 일관된 키 생성
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(labels)+1)
	parts = append(parts, tenantID)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return strings.Join(parts, "|")
}

// Accumulate 은 로그 엔트리를 버퍼에 누적한다
// 최대 엔트리 수에 도달하면 자동으로 청크를 빌드한다
func (bb *BlockBuilder) Accumulate(entry LogEntry) *Chunk {
	bb.mu.Lock()
	defer bb.mu.Unlock()

	key := bufferKey(entry.TenantID, entry.Labels)
	bb.buffers[key] = append(bb.buffers[key], entry)

	// 최대 엔트리 수 도달 시 청크 빌드
	if len(bb.buffers[key]) >= bb.maxEntries {
		chunk := bb.buildChunk(key)
		return chunk
	}
	return nil
}

// buildChunk 는 버퍼의 엔트리들로 청크를 생성한다
func (bb *BlockBuilder) buildChunk(key string) *Chunk {
	entries := bb.buffers[key]
	if len(entries) == 0 {
		return nil
	}

	// 시간 범위 계산
	minTime := entries[0].Timestamp
	maxTime := entries[0].Timestamp
	for _, e := range entries[1:] {
		if e.Timestamp.Before(minTime) {
			minTime = e.Timestamp
		}
		if e.Timestamp.After(maxTime) {
			maxTime = e.Timestamp
		}
	}

	chunk := &Chunk{
		TenantID: entries[0].TenantID,
		Labels:   key,
		Entries:  make([]LogEntry, len(entries)),
		MinTime:  minTime,
		MaxTime:  maxTime,
		Size:     len(entries),
	}
	copy(chunk.Entries, entries)

	// 버퍼 초기화
	delete(bb.buffers, key)

	return chunk
}

// Flush 는 모든 버퍼의 잔여 엔트리를 청크로 빌드한다
// Loki 실제 코드: flush_manager.go → flushAll()
func (bb *BlockBuilder) Flush() []Chunk {
	bb.mu.Lock()
	defer bb.mu.Unlock()

	var chunks []Chunk
	for key := range bb.buffers {
		chunk := bb.buildChunk(key)
		if chunk != nil {
			chunks = append(chunks, *chunk)
			bb.flushCount++
		}
	}
	bb.flushedChunks = append(bb.flushedChunks, chunks...)
	return chunks
}

// =============================================================================
// ConsumerGroup: 파티션 할당 및 리밸런싱
// =============================================================================
// Loki에서 컨슈머 그룹은 여러 인스턴스가 파티션을 나누어 처리한다.
// 인스턴스가 추가/제거되면 리밸런싱이 발생하여 파티션이 재할당된다.

// ConsumerMember 는 컨슈머 그룹의 멤버를 나타낸다
type ConsumerMember struct {
	ID              string
	AssignedParts   []int             // 할당된 파티션 ID들
	PartitionOffset map[int]int64     // 파티션별 처리 오프셋
	Builder         *BlockBuilder     // 블록 빌더
}

// ConsumerGroup 은 컨슈머 그룹을 시뮬레이션한다
type ConsumerGroup struct {
	mu       sync.Mutex
	groupID  string
	topic    *Topic
	members  []*ConsumerMember
}

// NewConsumerGroup 은 새 컨슈머 그룹을 생성한다
func NewConsumerGroup(groupID string, topic *Topic) *ConsumerGroup {
	return &ConsumerGroup{
		groupID: groupID,
		topic:   topic,
	}
}

// AddMember 는 새 멤버를 추가하고 리밸런싱을 수행한다
func (cg *ConsumerGroup) AddMember(memberID string) *ConsumerMember {
	cg.mu.Lock()
	defer cg.mu.Unlock()

	member := &ConsumerMember{
		ID:              memberID,
		PartitionOffset: make(map[int]int64),
		Builder:         NewBlockBuilder(5, time.Second),
	}
	cg.members = append(cg.members, member)

	// 리밸런싱 수행
	cg.rebalance()

	return member
}

// RemoveMember 는 멤버를 제거하고 리밸런싱을 수행한다
func (cg *ConsumerGroup) RemoveMember(memberID string) {
	cg.mu.Lock()
	defer cg.mu.Unlock()

	newMembers := make([]*ConsumerMember, 0)
	for _, m := range cg.members {
		if m.ID != memberID {
			newMembers = append(newMembers, m)
		}
	}
	cg.members = newMembers

	// 리밸런싱 수행
	cg.rebalance()
}

// rebalance 는 파티션을 멤버들에게 균등하게 분배한다
// Kafka의 Range Assignor와 유사한 전략
func (cg *ConsumerGroup) rebalance() {
	numPartitions := len(cg.topic.partitions)
	numMembers := len(cg.members)

	if numMembers == 0 {
		return
	}

	// 모든 멤버의 파티션 할당 초기화
	for _, m := range cg.members {
		m.AssignedParts = nil
	}

	// Range 할당 전략: 파티션을 순서대로 균등 분배
	// Loki 실제 코드: partition ring을 사용하여 파티션을 분배
	partitionsPerMember := numPartitions / numMembers
	remaining := numPartitions % numMembers

	partitionIdx := 0
	for i, member := range cg.members {
		count := partitionsPerMember
		if i < remaining {
			count++ // 나머지 파티션을 앞쪽 멤버에 추가 할당
		}
		for j := 0; j < count && partitionIdx < numPartitions; j++ {
			member.AssignedParts = append(member.AssignedParts, partitionIdx)
			// 오프셋 초기화 (이미 있으면 유지)
			if _, ok := member.PartitionOffset[partitionIdx]; !ok {
				member.PartitionOffset[partitionIdx] = 0
			}
			partitionIdx++
		}
	}
}

// Consume 은 멤버가 할당된 파티션에서 레코드를 소비하고 블록 빌더에 전달한다
func (cg *ConsumerGroup) Consume(member *ConsumerMember, maxRecords int) (consumed int, chunks []Chunk) {
	for _, partID := range member.AssignedParts {
		partition := cg.topic.partitions[partID]
		offset := member.PartitionOffset[partID]
		records := partition.Fetch(offset, maxRecords)

		for _, record := range records {
			// 블록 빌더에 엔트리 누적
			chunk := member.Builder.Accumulate(record.Value)
			if chunk != nil {
				chunks = append(chunks, *chunk)
			}
			member.PartitionOffset[partID] = record.Offset + 1
			consumed++
		}
	}
	return consumed, chunks
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== Loki Kafka 소비자 / 블록 빌더 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 1단계: Kafka 토픽 생성
	// ─────────────────────────────────────────────────────────────
	numPartitions := 6
	topic := NewTopic("loki-logs", numPartitions)

	fmt.Println("--- [1] Kafka 토픽 생성 ---")
	fmt.Printf("  토픽: %s\n", topic.name)
	fmt.Printf("  파티션 수: %d\n", numPartitions)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 2단계: 파티션 링 기반 프로듀싱
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [2] 프로듀서: 테넌트 기반 파티션 라우팅 ---")
	fmt.Println()

	producer := NewProducer(topic)

	// 여러 테넌트의 로그 생성
	tenants := []string{"tenant-alpha", "tenant-beta", "tenant-gamma", "tenant-delta"}
	services := []string{"api-gateway", "user-service", "order-service"}
	levels := []string{"INFO", "WARN", "ERROR"}
	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// 테넌트별 파티션 매핑 표시
	fmt.Println("  테넌트 → 파티션 매핑:")
	for _, tenant := range tenants {
		part := hashToPartition(tenant, numPartitions)
		fmt.Printf("    %s → 파티션 %d\n", tenant, part)
	}
	fmt.Println()

	// 로그 생성 및 전송
	totalRecords := 0
	for i := 0; i < 30; i++ {
		tenant := tenants[rand.Intn(len(tenants))]
		service := services[rand.Intn(len(services))]
		level := levels[rand.Intn(len(levels))]

		entry := LogEntry{
			Timestamp: baseTime.Add(time.Duration(i) * time.Second),
			TenantID:  tenant,
			Labels: map[string]string{
				"service": service,
				"level":   level,
			},
			Line: fmt.Sprintf("[%s] %s: log message #%d from %s", level, service, i, tenant),
		}

		producer.Send(entry)
		totalRecords++
	}

	fmt.Printf("  총 %d개 레코드 전송 완료\n", totalRecords)
	fmt.Println()

	// 파티션별 레코드 분포
	fmt.Println("  파티션별 레코드 분포:")
	for _, p := range topic.partitions {
		watermark := p.HighWatermark()
		bar := strings.Repeat("█", int(watermark))
		fmt.Printf("    파티션 %d: %2d 레코드 %s\n", p.id, watermark, bar)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 3단계: 컨슈머 그룹 생성 및 파티션 할당
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [3] 컨슈머 그룹: 파티션 할당 ---")
	fmt.Println()

	group := NewConsumerGroup("loki-ingester-group", topic)

	// 2개의 컨슈머 멤버 추가
	member1 := group.AddMember("ingester-1")
	member2 := group.AddMember("ingester-2")

	fmt.Printf("  그룹: %s\n", group.groupID)
	fmt.Printf("  멤버 수: %d\n", len(group.members))
	fmt.Println()
	fmt.Println("  초기 파티션 할당:")
	for _, m := range group.members {
		fmt.Printf("    %s → 파티션 %v\n", m.ID, m.AssignedParts)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 4단계: 레코드 소비 및 블록 빌더 처리
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [4] 소비 및 블록 빌더 ---")
	fmt.Println()

	// 각 멤버가 레코드를 소비
	consumed1, chunks1 := group.Consume(member1, 100)
	consumed2, chunks2 := group.Consume(member2, 100)

	fmt.Printf("  %s: %d 레코드 소비, %d 청크 자동 빌드\n", member1.ID, consumed1, len(chunks1))
	fmt.Printf("  %s: %d 레코드 소비, %d 청크 자동 빌드\n", member2.ID, consumed2, len(chunks2))
	fmt.Println()

	// 자동 빌드된 청크 표시
	allChunks := append(chunks1, chunks2...)
	if len(allChunks) > 0 {
		fmt.Println("  자동 빌드된 청크 (maxEntries=5 도달):")
		for i, chunk := range allChunks {
			fmt.Printf("    청크 %d: 테넌트=%s, 엔트리=%d, 기간=%s~%s\n",
				i+1, chunk.TenantID, chunk.Size,
				chunk.MinTime.Format("15:04:05"),
				chunk.MaxTime.Format("15:04:05"))
		}
		fmt.Println()
	}

	// 잔여 엔트리 플러시
	fmt.Println("  잔여 엔트리 플러시:")
	flushed1 := member1.Builder.Flush()
	flushed2 := member2.Builder.Flush()
	allFlushed := append(flushed1, flushed2...)

	for i, chunk := range allFlushed {
		fmt.Printf("    플러시 청크 %d: 테넌트=%s, 엔트리=%d, 레이블=%s\n",
			i+1, chunk.TenantID, chunk.Size, chunk.Labels)
	}
	fmt.Println()

	// 오프셋 상태
	fmt.Println("  커밋된 오프셋:")
	for _, m := range group.members {
		for _, partID := range m.AssignedParts {
			hwm := topic.partitions[partID].HighWatermark()
			committed := m.PartitionOffset[partID]
			lag := hwm - committed
			fmt.Printf("    %s: 파티션 %d → 커밋=%d, HWM=%d, 랙=%d\n",
				m.ID, partID, committed, hwm, lag)
		}
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 5단계: 리밸런싱 시뮬레이션
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [5] 리밸런싱 시뮬레이션 ---")
	fmt.Println()

	// 새 멤버 추가 → 리밸런싱
	fmt.Println("  [이벤트] 새 멤버 'ingester-3' 추가")
	member3 := group.AddMember("ingester-3")
	_ = member3

	fmt.Println("  리밸런싱 후 파티션 할당:")
	for _, m := range group.members {
		fmt.Printf("    %s → 파티션 %v\n", m.ID, m.AssignedParts)
	}
	fmt.Println()

	// 멤버 제거 → 리밸런싱
	fmt.Println("  [이벤트] 멤버 'ingester-2' 제거 (장애 시뮬레이션)")
	group.RemoveMember("ingester-2")

	fmt.Println("  리밸런싱 후 파티션 할당:")
	for _, m := range group.members {
		fmt.Printf("    %s → 파티션 %v\n", m.ID, m.AssignedParts)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 6단계: 블록 빌더 동작 상세 시뮬레이션
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [6] 블록 빌더 상세 동작 ---")
	fmt.Println()

	builder := NewBlockBuilder(3, time.Second)

	// 동일 테넌트의 로그 엔트리를 순차적으로 누적
	testEntries := []LogEntry{
		{Timestamp: baseTime, TenantID: "tenant-x", Labels: map[string]string{"app": "web"}, Line: "request started"},
		{Timestamp: baseTime.Add(1 * time.Second), TenantID: "tenant-x", Labels: map[string]string{"app": "web"}, Line: "processing..."},
		{Timestamp: baseTime.Add(2 * time.Second), TenantID: "tenant-x", Labels: map[string]string{"app": "web"}, Line: "request completed"},
		{Timestamp: baseTime.Add(3 * time.Second), TenantID: "tenant-x", Labels: map[string]string{"app": "web"}, Line: "new request"},
		{Timestamp: baseTime.Add(4 * time.Second), TenantID: "tenant-y", Labels: map[string]string{"app": "api"}, Line: "health check"},
	}

	fmt.Println("  엔트리 누적 과정:")
	for i, entry := range testEntries {
		chunk := builder.Accumulate(entry)
		if chunk != nil {
			fmt.Printf("    [%d] 엔트리 추가 → 청크 빌드! (테넌트=%s, 엔트리=%d)\n",
				i+1, chunk.TenantID, chunk.Size)
			fmt.Printf("        청크 내용:\n")
			for j, e := range chunk.Entries {
				fmt.Printf("          %d. [%s] %s\n", j+1, e.Timestamp.Format("15:04:05"), e.Line)
			}
		} else {
			fmt.Printf("    [%d] 엔트리 추가 → 버퍼에 누적 (테넌트=%s, 라인=%s)\n",
				i+1, entry.TenantID, entry.Line)
		}
	}
	fmt.Println()

	// 잔여 플러시
	remaining := builder.Flush()
	if len(remaining) > 0 {
		fmt.Println("  잔여 버퍼 플러시:")
		for _, chunk := range remaining {
			fmt.Printf("    테넌트=%s, 엔트리=%d개\n", chunk.TenantID, chunk.Size)
			for j, e := range chunk.Entries {
				fmt.Printf("      %d. [%s] %s\n", j+1, e.Timestamp.Format("15:04:05"), e.Line)
			}
		}
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 동작 원리 요약
	// ─────────────────────────────────────────────────────────────
	fmt.Println("=== Kafka 소비자 / 블록 빌더 동작 원리 요약 ===")
	fmt.Println()
	fmt.Println("  1. 프로듀서: 테넌트 ID를 해싱하여 파티션에 라우팅")
	fmt.Println("     → 같은 테넌트의 로그는 동일 파티션에서 순서 보장")
	fmt.Println("  2. 컨슈머 그룹: 파티션을 멤버에게 균등 분배")
	fmt.Println("     → 멤버 추가/제거 시 리밸런싱 수행")
	fmt.Println("  3. 블록 빌더: 엔트리 누적 → 청크 빌드 → 스토리지 플러시")
	fmt.Println("     → 최대 엔트리 수 또는 시간 초과 시 자동 플러시")
	fmt.Println("  4. 오프셋 관리: 각 파티션별 처리 위치 추적")
	fmt.Println("     → 장애 복구 시 마지막 커밋 오프셋부터 재처리")
	fmt.Println()
	fmt.Println("  Loki 핵심 코드 경로:")
	fmt.Println("  - distributor → kafka 쓰기 경로")
	fmt.Println("  - pkg/dataobj/consumer/processor.go  → 레코드 처리")
	fmt.Println("  - pkg/dataobj/consumer/flush.go      → 청크 플러시")
	fmt.Println("  - pkg/dataobj/consumer/flush_manager.go → 플러시 관리")
}
