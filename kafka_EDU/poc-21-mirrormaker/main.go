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
// Kafka MirrorMaker 2.0 시뮬레이션
//
// 이 PoC는 MirrorMaker 2.0 (MM2)의 핵심 개념을 시뮬레이션한다:
//   1. MirrorSourceConnector: 원본 → 대상 클러스터 데이터 복제
//   2. MirrorCheckpointConnector: Consumer Group 오프셋 동기화
//   3. MirrorHeartbeatConnector: 클러스터 간 연결 상태 확인
//   4. RemoteTopicName 변환: source.topic → target.topic (alias prefix)
//   5. 오프셋 변환 (OffsetSync): 원본↔대상 오프셋 매핑
//
// 참조 소스:
//   connect/mirror/src/main/java/.../MirrorSourceConnector.java
//   connect/mirror/src/main/java/.../MirrorCheckpointConnector.java
//   connect/mirror/src/main/java/.../MirrorHeartbeatConnector.java
//   connect/mirror/src/main/java/.../OffsetSyncStore.java
// =============================================================================

// --- 클러스터 모델 ---

type Cluster struct {
	Name   string
	Topics map[string]*MirrorTopic
	mu     sync.RWMutex
}

type MirrorTopic struct {
	Name       string
	Partitions int
	Records    []Record
	mu         sync.Mutex
}

type Record struct {
	Key       string
	Value     string
	Offset    int64
	Timestamp time.Time
}

func NewCluster(name string) *Cluster {
	return &Cluster{
		Name:   name,
		Topics: make(map[string]*MirrorTopic),
	}
}

func (c *Cluster) CreateTopic(name string, partitions int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Topics[name] = &MirrorTopic{Name: name, Partitions: partitions}
}

func (c *Cluster) Produce(topic, key, value string) int64 {
	c.mu.RLock()
	t, ok := c.Topics[topic]
	c.mu.RUnlock()
	if !ok {
		c.CreateTopic(topic, 1)
		c.mu.RLock()
		t = c.Topics[topic]
		c.mu.RUnlock()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	offset := int64(len(t.Records))
	t.Records = append(t.Records, Record{
		Key: key, Value: value, Offset: offset, Timestamp: time.Now(),
	})
	return offset
}

func (c *Cluster) GetRecords(topic string) []Record {
	c.mu.RLock()
	t, ok := c.Topics[topic]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]Record, len(t.Records))
	copy(result, t.Records)
	return result
}

// --- OffsetSync: 오프셋 매핑 ---
// 실제: connect/mirror/src/main/java/.../OffsetSyncStore.java

type OffsetSync struct {
	SourceTopic  string
	TargetTopic  string
	SourceOffset int64
	TargetOffset int64
}

type OffsetSyncStore struct {
	mu    sync.RWMutex
	syncs map[string][]OffsetSync // sourceCluster.topic → syncs
}

func NewOffsetSyncStore() *OffsetSyncStore {
	return &OffsetSyncStore{
		syncs: make(map[string][]OffsetSync),
	}
}

func (oss *OffsetSyncStore) Record(sourceTopic, targetTopic string, sourceOffset, targetOffset int64) {
	oss.mu.Lock()
	defer oss.mu.Unlock()
	key := sourceTopic
	oss.syncs[key] = append(oss.syncs[key], OffsetSync{
		SourceTopic: sourceTopic, TargetTopic: targetTopic,
		SourceOffset: sourceOffset, TargetOffset: targetOffset,
	})
}

// TranslateOffset는 원본 오프셋을 대상 오프셋으로 변환한다.
func (oss *OffsetSyncStore) TranslateOffset(sourceTopic string, sourceOffset int64) (int64, bool) {
	oss.mu.RLock()
	defer oss.mu.RUnlock()

	syncs := oss.syncs[sourceTopic]
	if len(syncs) == 0 {
		return -1, false
	}

	// 이진 검색으로 가장 가까운 sync 찾기
	best := int64(-1)
	for _, sync := range syncs {
		if sync.SourceOffset <= sourceOffset {
			best = sync.TargetOffset + (sourceOffset - sync.SourceOffset)
		}
	}
	if best >= 0 {
		return best, true
	}
	return -1, false
}

// --- ReplicationPolicy: 토픽 이름 변환 ---
// 실제: connect/mirror/src/main/java/.../DefaultReplicationPolicy.java

type ReplicationPolicy struct {
	Separator string // 기본: "."
}

func NewReplicationPolicy() *ReplicationPolicy {
	return &ReplicationPolicy{Separator: "."}
}

// FormatRemoteTopicName은 원본 토픽 이름을 대상 클러스터용으로 변환한다.
// 예: sourceCluster="us-east", topic="orders" → "us-east.orders"
func (rp *ReplicationPolicy) FormatRemoteTopicName(sourceCluster, topic string) string {
	return sourceCluster + rp.Separator + topic
}

// IsRemoteTopic은 토픽이 원격 클러스터에서 복제된 것인지 확인한다.
func (rp *ReplicationPolicy) IsRemoteTopic(topic string) bool {
	return strings.Contains(topic, rp.Separator)
}

// SourceCluster는 원격 토픽에서 소스 클러스터 이름을 추출한다.
func (rp *ReplicationPolicy) SourceCluster(remoteTopic string) string {
	parts := strings.SplitN(remoteTopic, rp.Separator, 2)
	if len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// OriginalTopic은 원격 토픽에서 원래 토픽 이름을 추출한다.
func (rp *ReplicationPolicy) OriginalTopic(remoteTopic string) string {
	parts := strings.SplitN(remoteTopic, rp.Separator, 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return remoteTopic
}

// --- MirrorSourceConnector: 데이터 복제 ---
// 실제: connect/mirror/src/main/java/.../MirrorSourceConnector.java

type MirrorSourceConnector struct {
	sourceCluster *Cluster
	targetCluster *Cluster
	policy        *ReplicationPolicy
	offsetSync    *OffsetSyncStore
	topicFilter   func(string) bool
	replicated    int64
	running       atomic.Bool
}

func NewMirrorSourceConnector(source, target *Cluster, policy *ReplicationPolicy,
	offsetSync *OffsetSyncStore, filter func(string) bool) *MirrorSourceConnector {
	return &MirrorSourceConnector{
		sourceCluster: source,
		targetCluster: target,
		policy:        policy,
		offsetSync:    offsetSync,
		topicFilter:   filter,
	}
}

func (msc *MirrorSourceConnector) Run(stop <-chan struct{}) {
	msc.running.Store(true)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	// 토픽별 마지막 복제 오프셋
	lastOffset := make(map[string]int64)

	for {
		select {
		case <-stop:
			msc.running.Store(false)
			return
		case <-ticker.C:
			msc.sourceCluster.mu.RLock()
			for topicName, topic := range msc.sourceCluster.Topics {
				if !msc.topicFilter(topicName) {
					continue
				}
				remoteName := msc.policy.FormatRemoteTopicName(msc.sourceCluster.Name, topicName)

				topic.mu.Lock()
				fromOffset := lastOffset[topicName]
				for i := fromOffset; i < int64(len(topic.Records)); i++ {
					rec := topic.Records[i]
					targetOffset := msc.targetCluster.Produce(remoteName, rec.Key, rec.Value)
					msc.offsetSync.Record(topicName, remoteName, rec.Offset, targetOffset)
					atomic.AddInt64(&msc.replicated, 1)
				}
				lastOffset[topicName] = int64(len(topic.Records))
				topic.mu.Unlock()
			}
			msc.sourceCluster.mu.RUnlock()
		}
	}
}

// --- MirrorCheckpointConnector: Consumer Group 오프셋 동기화 ---

type ConsumerGroupOffset struct {
	Group  string
	Topic  string
	Offset int64
}

type MirrorCheckpointConnector struct {
	sourceCluster *Cluster
	targetCluster *Cluster
	policy        *ReplicationPolicy
	offsetSync    *OffsetSyncStore
	sourceOffsets []ConsumerGroupOffset
	checkpoints   []ConsumerGroupOffset // 변환된 오프셋
	mu            sync.Mutex
}

func NewMirrorCheckpointConnector(source, target *Cluster, policy *ReplicationPolicy,
	offsetSync *OffsetSyncStore) *MirrorCheckpointConnector {
	return &MirrorCheckpointConnector{
		sourceCluster: source,
		targetCluster: target,
		policy:        policy,
		offsetSync:    offsetSync,
	}
}

func (mcc *MirrorCheckpointConnector) AddSourceOffset(group, topic string, offset int64) {
	mcc.mu.Lock()
	defer mcc.mu.Unlock()
	mcc.sourceOffsets = append(mcc.sourceOffsets, ConsumerGroupOffset{
		Group: group, Topic: topic, Offset: offset,
	})
}

// EmitCheckpoints는 원본 오프셋을 대상 오프셋으로 변환하여 체크포인트를 생성한다.
func (mcc *MirrorCheckpointConnector) EmitCheckpoints() {
	mcc.mu.Lock()
	defer mcc.mu.Unlock()

	for _, src := range mcc.sourceOffsets {
		targetOffset, ok := mcc.offsetSync.TranslateOffset(src.Topic, src.Offset)
		if ok {
			remoteTopic := mcc.policy.FormatRemoteTopicName(mcc.sourceCluster.Name, src.Topic)
			checkpoint := ConsumerGroupOffset{
				Group:  src.Group,
				Topic:  remoteTopic,
				Offset: targetOffset,
			}
			mcc.checkpoints = append(mcc.checkpoints, checkpoint)
			fmt.Printf("  [Checkpoint] 그룹=%s: %s offset %d → %s offset %d\n",
				src.Group, src.Topic, src.Offset, remoteTopic, targetOffset)
		}
	}
}

// --- MirrorHeartbeatConnector: 연결 상태 확인 ---

type Heartbeat struct {
	SourceCluster string
	TargetCluster string
	Timestamp     time.Time
}

type MirrorHeartbeatConnector struct {
	sourceCluster string
	targetCluster string
	heartbeats    []Heartbeat
	mu            sync.Mutex
}

func NewMirrorHeartbeatConnector(source, target string) *MirrorHeartbeatConnector {
	return &MirrorHeartbeatConnector{
		sourceCluster: source,
		targetCluster: target,
	}
}

func (mhc *MirrorHeartbeatConnector) Emit() Heartbeat {
	mhc.mu.Lock()
	defer mhc.mu.Unlock()
	hb := Heartbeat{
		SourceCluster: mhc.sourceCluster,
		TargetCluster: mhc.targetCluster,
		Timestamp:     time.Now(),
	}
	mhc.heartbeats = append(mhc.heartbeats, hb)
	return hb
}

// --- 메인 함수 ---

func main() {
	fmt.Println("=== Kafka MirrorMaker 2.0 시뮬레이션 ===")
	fmt.Println()

	// 1. 클러스터 설정
	fmt.Println("--- 1단계: 클러스터 설정 ---")
	usEast := NewCluster("us-east")
	euWest := NewCluster("eu-west")
	policy := NewReplicationPolicy()
	offsetSync := NewOffsetSyncStore()

	usEast.CreateTopic("orders", 3)
	usEast.CreateTopic("users", 2)
	usEast.CreateTopic("internal-logs", 1) // 필터링 대상

	fmt.Printf("  소스 클러스터: %s (토픽: orders, users, internal-logs)\n", usEast.Name)
	fmt.Printf("  대상 클러스터: %s\n", euWest.Name)

	// 2. 토픽 이름 변환
	fmt.Println()
	fmt.Println("--- 2단계: ReplicationPolicy (토픽 이름 변환) ---")
	testTopics := []string{"orders", "users"}
	for _, t := range testTopics {
		remote := policy.FormatRemoteTopicName("us-east", t)
		fmt.Printf("  %s → %s\n", t, remote)
		fmt.Printf("    IsRemoteTopic: %v\n", policy.IsRemoteTopic(remote))
		fmt.Printf("    SourceCluster: %s\n", policy.SourceCluster(remote))
		fmt.Printf("    OriginalTopic: %s\n", policy.OriginalTopic(remote))
	}

	// 3. 원본 클러스터에 데이터 생산
	fmt.Println()
	fmt.Println("--- 3단계: 원본 클러스터에 데이터 생산 ---")
	for i := 0; i < 20; i++ {
		usEast.Produce("orders", fmt.Sprintf("order-%d", i), fmt.Sprintf("item-%d", rand.Intn(100)))
	}
	for i := 0; i < 10; i++ {
		usEast.Produce("users", fmt.Sprintf("user-%d", i), fmt.Sprintf("name-%d", i))
	}
	for i := 0; i < 5; i++ {
		usEast.Produce("internal-logs", fmt.Sprintf("log-%d", i), "debug message")
	}
	fmt.Printf("  orders: %d 레코드\n", len(usEast.GetRecords("orders")))
	fmt.Printf("  users: %d 레코드\n", len(usEast.GetRecords("users")))
	fmt.Printf("  internal-logs: %d 레코드\n", len(usEast.GetRecords("internal-logs")))

	// 4. MirrorSourceConnector 실행
	fmt.Println()
	fmt.Println("--- 4단계: MirrorSourceConnector (데이터 복제) ---")
	source := NewMirrorSourceConnector(usEast, euWest, policy, offsetSync,
		func(topic string) bool {
			return topic != "internal-logs" // internal-logs 제외
		})

	stop := make(chan struct{})
	go source.Run(stop)

	time.Sleep(100 * time.Millisecond)

	// 추가 생산 (실시간 복제 확인)
	for i := 20; i < 25; i++ {
		usEast.Produce("orders", fmt.Sprintf("order-%d", i), fmt.Sprintf("item-%d", rand.Intn(100)))
	}
	time.Sleep(50 * time.Millisecond)

	close(stop)
	time.Sleep(20 * time.Millisecond)

	fmt.Printf("  복제된 레코드 수: %d\n", atomic.LoadInt64(&source.replicated))
	fmt.Printf("  대상 us-east.orders: %d 레코드\n", len(euWest.GetRecords("us-east.orders")))
	fmt.Printf("  대상 us-east.users: %d 레코드\n", len(euWest.GetRecords("us-east.users")))

	// internal-logs가 복제되지 않았는지 확인
	internalRecs := euWest.GetRecords("us-east.internal-logs")
	fmt.Printf("  대상 us-east.internal-logs: %d 레코드 (필터링됨)\n", len(internalRecs))

	// 5. OffsetSync: 오프셋 변환
	fmt.Println()
	fmt.Println("--- 5단계: OffsetSync (오프셋 변환) ---")
	testOffsets := []int64{0, 5, 10, 15, 20}
	for _, srcOff := range testOffsets {
		targetOff, ok := offsetSync.TranslateOffset("orders", srcOff)
		if ok {
			fmt.Printf("  orders 소스 오프셋 %d → 대상 오프셋 %d\n", srcOff, targetOff)
		} else {
			fmt.Printf("  orders 소스 오프셋 %d → 변환 불가\n", srcOff)
		}
	}

	// 6. MirrorCheckpointConnector
	fmt.Println()
	fmt.Println("--- 6단계: MirrorCheckpointConnector (Consumer Group 오프셋 동기화) ---")
	checkpoint := NewMirrorCheckpointConnector(usEast, euWest, policy, offsetSync)
	checkpoint.AddSourceOffset("order-processor", "orders", 10)
	checkpoint.AddSourceOffset("order-processor", "orders", 20)
	checkpoint.AddSourceOffset("user-service", "users", 5)
	checkpoint.EmitCheckpoints()

	// 7. MirrorHeartbeatConnector
	fmt.Println()
	fmt.Println("--- 7단계: MirrorHeartbeatConnector (연결 상태) ---")
	heartbeat := NewMirrorHeartbeatConnector("us-east", "eu-west")
	for i := 0; i < 3; i++ {
		hb := heartbeat.Emit()
		fmt.Printf("  Heartbeat: %s → %s at %s\n",
			hb.SourceCluster, hb.TargetCluster,
			hb.Timestamp.Format("15:04:05.000"))
		time.Sleep(10 * time.Millisecond)
	}

	// 요약
	fmt.Println()
	fmt.Println("=== 요약 ===")
	fmt.Println("  - MirrorSourceConnector: 토픽 데이터 실시간 복제 (필터링 지원)")
	fmt.Println("  - ReplicationPolicy: source.topic → target.topic 이름 변환")
	fmt.Println("  - OffsetSyncStore: 원본↔대상 오프셋 매핑 및 변환")
	fmt.Println("  - MirrorCheckpointConnector: Consumer Group 오프셋 동기화")
	fmt.Println("  - MirrorHeartbeatConnector: 클러스터 간 연결 상태 확인")

	_ = strings.Join(nil, "")
}
