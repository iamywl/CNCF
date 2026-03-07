package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kafka High Watermark (HW) 메커니즘 시뮬레이션
//
// 참조: core/src/main/scala/kafka/cluster/Partition.scala (maybeIncrementLeaderHW)
//       raft/src/main/java/org/apache/kafka/raft/LeaderState.java
//
// HW는 ISR 내 모든 레플리카가 복제를 완료한 최소 오프셋이다.
// 컨슈머는 HW까지만 읽을 수 있어 커밋되지 않은 데이터 노출을 방지한다.
// =============================================================================

// LogEntry는 로그에 저장되는 하나의 레코드를 나타낸다.
type LogEntry struct {
	Offset    int64
	Key       string
	Value     string
	Timestamp time.Time
}

// ReplicaLog는 각 브로커(레플리카)가 보유하는 로그 세그먼트이다.
type ReplicaLog struct {
	mu      sync.Mutex
	entries []LogEntry
	// LEO: Log End Offset - 다음에 쓸 오프셋 (마지막 엔트리 오프셋 + 1)
	leo int64
	// HW: 이 레플리카가 알고 있는 High Watermark
	hw int64
}

func NewReplicaLog() *ReplicaLog {
	return &ReplicaLog{
		entries: make([]LogEntry, 0),
		leo:     0,
		hw:      0,
	}
}

// Append는 리더 로그에 새 레코드를 추가한다.
func (r *ReplicaLog) Append(key, value string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := LogEntry{
		Offset:    r.leo,
		Key:       key,
		Value:     value,
		Timestamp: time.Now(),
	}
	r.entries = append(r.entries, entry)
	r.leo++
	return entry.Offset
}

// FetchFrom은 주어진 오프셋부터 로그 엔트리를 가져온다 (팔로워 fetch 시뮬레이션).
// maxEntries가 0이면 전부 가져오고, 양수면 최대 maxEntries개만 가져온다.
func (r *ReplicaLog) FetchFrom(offset int64, maxEntries int) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	if offset >= r.leo {
		return nil
	}
	result := make([]LogEntry, 0)
	for _, e := range r.entries {
		if e.Offset >= offset {
			result = append(result, e)
			if maxEntries > 0 && len(result) >= maxEntries {
				break
			}
		}
	}
	return result
}

// AppendFetched는 팔로워가 리더에서 가져온 엔트리를 자신의 로그에 기록한다.
func (r *ReplicaLog) AppendFetched(entries []LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, e := range entries {
		if e.Offset >= r.leo {
			r.entries = append(r.entries, e)
			r.leo = e.Offset + 1
		}
	}
}

// ReadUpTo는 HW까지만 읽을 수 있는 컨슈머 읽기를 시뮬레이션한다.
func (r *ReplicaLog) ReadUpTo(maxOffset int64) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]LogEntry, 0)
	for _, e := range r.entries {
		if e.Offset < maxOffset {
			result = append(result, e)
		}
	}
	return result
}

// ReplicaState는 리더가 관리하는 각 팔로워의 상태 정보이다.
// Partition.scala의 remoteReplicasMap에 해당한다.
type ReplicaState struct {
	BrokerID      int
	LEO           int64  // 이 팔로워의 Log End Offset
	LastFetchTime int64  // 마지막 fetch 시각 (밀리초)
	InISR         bool   // ISR 멤버 여부
}

// Partition은 Kafka 파티션의 리더 역할을 시뮬레이션한다.
// core/src/main/scala/kafka/cluster/Partition.scala에 기반한다.
type Partition struct {
	mu             sync.Mutex
	TopicPartition string
	LeaderBrokerID int
	LeaderLog      *ReplicaLog
	ReplicaStates  map[int]*ReplicaState // brokerID -> ReplicaState
	FollowerLogs   map[int]*ReplicaLog   // brokerID -> ReplicaLog

	// ISR 관련 설정
	ReplicaLagTimeMaxMs int64 // 팔로워가 이 시간 내에 fetch하지 않으면 ISR에서 제거
}

func NewPartition(topic string, leaderID int, followerIDs []int, lagTimeMaxMs int64) *Partition {
	p := &Partition{
		TopicPartition:      topic,
		LeaderBrokerID:      leaderID,
		LeaderLog:           NewReplicaLog(),
		ReplicaStates:       make(map[int]*ReplicaState),
		FollowerLogs:        make(map[int]*ReplicaLog),
		ReplicaLagTimeMaxMs: lagTimeMaxMs,
	}

	for _, id := range followerIDs {
		p.ReplicaStates[id] = &ReplicaState{
			BrokerID:      id,
			LEO:           0,
			LastFetchTime: time.Now().UnixMilli(),
			InISR:         true,
		}
		p.FollowerLogs[id] = NewReplicaLog()
	}
	return p
}

// ProduceRecord는 리더에 레코드를 추가하고 HW 갱신을 시도한다.
func (p *Partition) ProduceRecord(key, value string) int64 {
	offset := p.LeaderLog.Append(key, value)
	p.maybeIncrementLeaderHW()
	return offset
}

// maybeIncrementLeaderHW는 Partition.scala의 핵심 메서드를 시뮬레이션한다.
//
// 원본 알고리즘:
//   newHighWatermark = leaderLogEndOffset
//   for each replica in remoteReplicasMap:
//     if replica.leo < newHighWatermark AND replica is in ISR:
//       newHighWatermark = replica.leo
//   if newHighWatermark > currentHW:
//     update HW
//
// HW = ISR 내 모든 레플리카 LEO의 최솟값
func (p *Partition) maybeIncrementLeaderHW() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	leaderLEO := p.LeaderLog.leo
	newHW := leaderLEO // 리더 LEO에서 시작

	// ISR 내 팔로워의 LEO 중 최솟값을 구한다
	for _, rs := range p.ReplicaStates {
		if rs.InISR && rs.LEO < newHW {
			newHW = rs.LEO
		}
	}

	oldHW := p.LeaderLog.hw
	if newHW > oldHW {
		p.LeaderLog.hw = newHW
		fmt.Printf("  [HW 갱신] %s: HW %d -> %d (리더 LEO=%d)\n", p.TopicPartition, oldHW, newHW, leaderLEO)
		return true
	}
	return false
}

// FollowerFetch는 팔로워가 리더에게 fetch 요청을 보내는 것을 시뮬레이션한다.
// Partition.scala의 updateFollowerFetchState에 해당한다.
func (p *Partition) FollowerFetch(brokerID int) {
	p.FollowerFetchN(brokerID, 0)
}

// FollowerFetchN은 최대 n개의 엔트리만 fetch한다 (0이면 전부).
func (p *Partition) FollowerFetchN(brokerID int, maxEntries int) {
	p.mu.Lock()
	rs, ok := p.ReplicaStates[brokerID]
	if !ok {
		p.mu.Unlock()
		return
	}
	followerLog := p.FollowerLogs[brokerID]
	p.mu.Unlock()

	// 1) 팔로워가 자신의 LEO부터 리더 로그를 fetch
	entries := p.LeaderLog.FetchFrom(followerLog.leo, maxEntries)
	if len(entries) > 0 {
		followerLog.AppendFetched(entries)
	}

	// 2) 팔로워 상태 업데이트 (리더 측)
	p.mu.Lock()
	rs.LEO = followerLog.leo
	rs.LastFetchTime = time.Now().UnixMilli()
	p.mu.Unlock()

	// 3) 팔로워의 HW를 리더의 HW로 업데이트
	followerLog.mu.Lock()
	followerLog.hw = p.LeaderLog.hw
	followerLog.mu.Unlock()

	// 4) HW 갱신 시도
	p.maybeIncrementLeaderHW()
}

// MaybeShrinkISR은 지연된 팔로워를 ISR에서 제거한다.
// 실제 Kafka의 maybeShrinkIsr 로직에 해당한다.
func (p *Partition) MaybeShrinkISR() []int {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().UnixMilli()
	removed := make([]int, 0)

	for id, rs := range p.ReplicaStates {
		if rs.InISR && (now-rs.LastFetchTime) > p.ReplicaLagTimeMaxMs {
			rs.InISR = false
			removed = append(removed, id)
			fmt.Printf("  [ISR 축소] 브로커 %d가 ISR에서 제거됨 (마지막 fetch: %dms 전)\n",
				id, now-rs.LastFetchTime)
		}
	}

	// ISR 축소 후 HW 재계산 (ISR이 줄면 HW가 올라갈 수 있음)
	if len(removed) > 0 {
		leaderLEO := p.LeaderLog.leo
		newHW := leaderLEO
		for _, rs := range p.ReplicaStates {
			if rs.InISR && rs.LEO < newHW {
				newHW = rs.LEO
			}
		}
		if newHW > p.LeaderLog.hw {
			oldHW := p.LeaderLog.hw
			p.LeaderLog.hw = newHW
			fmt.Printf("  [HW 갱신] ISR 축소 후 HW %d -> %d\n", oldHW, newHW)
		}
	}
	return removed
}

// MaybeExpandISR은 충분히 따라잡은 팔로워를 ISR에 추가한다.
func (p *Partition) MaybeExpandISR() []int {
	p.mu.Lock()
	defer p.mu.Unlock()

	added := make([]int, 0)
	leaderLEO := p.LeaderLog.leo

	for id, rs := range p.ReplicaStates {
		if !rs.InISR && rs.LEO >= leaderLEO {
			rs.InISR = true
			added = append(added, id)
			fmt.Printf("  [ISR 확장] 브로커 %d가 ISR에 추가됨 (LEO=%d, 리더 LEO=%d)\n",
				id, rs.LEO, leaderLEO)
		}
	}
	return added
}

// GetISR은 현재 ISR 목록을 반환한다.
func (p *Partition) GetISR() []int {
	p.mu.Lock()
	defer p.mu.Unlock()

	isr := []int{p.LeaderBrokerID} // 리더는 항상 ISR에 포함
	for id, rs := range p.ReplicaStates {
		if rs.InISR {
			isr = append(isr, id)
		}
	}
	return isr
}

// GetHW는 현재 High Watermark를 반환한다.
func (p *Partition) GetHW() int64 {
	p.LeaderLog.mu.Lock()
	defer p.LeaderLog.mu.Unlock()
	return p.LeaderLog.hw
}

// ConsumerRead는 컨슈머가 HW까지만 읽을 수 있음을 시뮬레이션한다.
func (p *Partition) ConsumerRead() []LogEntry {
	hw := p.GetHW()
	return p.LeaderLog.ReadUpTo(hw)
}

func printISRStatus(p *Partition) {
	p.mu.Lock()
	defer p.mu.Unlock()

	fmt.Printf("  [상태] ISR={%d(리더)", p.LeaderBrokerID)
	for id, rs := range p.ReplicaStates {
		if rs.InISR {
			fmt.Printf(", %d", id)
		}
	}
	fmt.Printf("} | 리더 LEO=%d, HW=%d", p.LeaderLog.leo, p.LeaderLog.hw)
	for id, rs := range p.ReplicaStates {
		fmt.Printf(" | 브로커%d LEO=%d", id, rs.LEO)
	}
	fmt.Println()
}

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Kafka High Watermark (HW) 메커니즘 시뮬레이션              ║")
	fmt.Println("║  참조: Partition.scala (maybeIncrementLeaderHW)                     ║")
	fmt.Println("║        LeaderState.java                                             ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// 파티션 생성: 리더=브로커0, 팔로워=브로커1,2, lag 허용=500ms
	partition := NewPartition("test-topic-0", 0, []int{1, 2}, 500)

	// =========================================================================
	// 시나리오 1: 기본적인 HW 진행
	// =========================================================================
	printSeparator("시나리오 1: 기본적인 HW 진행 과정")
	fmt.Println("리더에 레코드를 쓰고, 팔로워가 순차적으로 fetch하면 HW가 올라간다.")
	fmt.Println()

	fmt.Println("--- 리더에 3개 레코드 추가 ---")
	partition.ProduceRecord("k1", "value-1")
	partition.ProduceRecord("k2", "value-2")
	partition.ProduceRecord("k3", "value-3")
	printISRStatus(partition)

	fmt.Println()
	fmt.Println("--- 팔로워 1만 fetch ---")
	partition.FollowerFetch(1)
	printISRStatus(partition)
	fmt.Printf("  컨슈머가 읽을 수 있는 레코드: %d개 (HW=%d)\n", len(partition.ConsumerRead()), partition.GetHW())

	fmt.Println()
	fmt.Println("--- 팔로워 2도 fetch ---")
	partition.FollowerFetch(2)
	printISRStatus(partition)
	fmt.Printf("  컨슈머가 읽을 수 있는 레코드: %d개 (HW=%d)\n", len(partition.ConsumerRead()), partition.GetHW())

	// =========================================================================
	// 시나리오 2: 컨슈머는 HW까지만 읽을 수 있음
	// =========================================================================
	printSeparator("시나리오 2: 컨슈머는 HW까지만 읽을 수 있음")
	fmt.Println("리더 LEO > HW인 상태에서 컨슈머는 커밋된 데이터만 읽을 수 있다.")
	fmt.Println()

	fmt.Println("--- 리더에 2개 추가 레코드 ---")
	partition.ProduceRecord("k4", "value-4")
	partition.ProduceRecord("k5", "value-5")
	printISRStatus(partition)

	records := partition.ConsumerRead()
	fmt.Printf("  컨슈머 읽기: %d개 (HW=%d, 리더 LEO=%d)\n",
		len(records), partition.GetHW(), partition.LeaderLog.leo)
	for _, r := range records {
		fmt.Printf("    offset=%d key=%s value=%s\n", r.Offset, r.Key, r.Value)
	}

	fmt.Println()
	fmt.Println("  --> 리더 LEO=5이지만 HW=3이므로 offset 0,1,2만 읽을 수 있다.")
	fmt.Println("      offset 3,4는 아직 모든 ISR에 복제되지 않았다.")

	// =========================================================================
	// 시나리오 3: 팔로워 지연으로 ISR 축소 -> HW 진행
	// =========================================================================
	printSeparator("시나리오 3: ISR 축소 (팔로워 지연)")
	fmt.Println("팔로워 2가 fetch를 멈추면 ISR에서 제거되고, HW가 올라갈 수 있다.")
	fmt.Println()

	// 팔로워 1만 fetch
	fmt.Println("--- 팔로워 1만 fetch (팔로워 2는 지연) ---")
	partition.FollowerFetch(1)
	printISRStatus(partition)

	fmt.Println()
	fmt.Println("--- 600ms 대기 (lag 허용 500ms 초과) ---")
	time.Sleep(600 * time.Millisecond)

	// ISR 축소 체크
	partition.MaybeShrinkISR()
	printISRStatus(partition)

	// ISR에서 제거된 후 HW 재계산 (리더 + 팔로워1의 LEO 기준)
	partition.maybeIncrementLeaderHW()
	printISRStatus(partition)

	records = partition.ConsumerRead()
	fmt.Printf("  컨슈머 읽기: %d개 (HW=%d)\n", len(records), partition.GetHW())

	// =========================================================================
	// 시나리오 4: 팔로워 복귀 -> ISR 확장
	// =========================================================================
	printSeparator("시나리오 4: ISR 확장 (팔로워 복귀)")
	fmt.Println("팔로워 2가 다시 fetch를 시작하여 리더를 따라잡으면 ISR에 재추가된다.")
	fmt.Println()

	fmt.Println("--- 팔로워 2 fetch 재개 ---")
	partition.FollowerFetch(2) // 3개 밀린 레코드 fetch
	partition.MaybeExpandISR()
	printISRStatus(partition)

	// =========================================================================
	// 시나리오 5: 점진적 HW 진행 시각화
	// =========================================================================
	printSeparator("시나리오 5: 점진적 HW 진행 시각화")
	fmt.Println("각 팔로워의 fetch 속도가 다를 때 HW가 어떻게 진행되는지 보여준다.")
	fmt.Println()

	// 새 파티션 (깨끗한 상태)
	p2 := NewPartition("viz-topic-0", 0, []int{1, 2}, 5000)

	// 10개 레코드 추가
	for i := 0; i < 10; i++ {
		p2.ProduceRecord(fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}
	fmt.Printf("리더에 10개 레코드 추가 완료 (LEO=10)\n\n")

	// 팔로워들이 다른 속도로 fetch (FollowerFetchN으로 부분 fetch)
	type fetchStep struct {
		broker1Target int // 팔로워1 목표 LEO
		broker2Target int // 팔로워2 목표 LEO
	}

	steps := []fetchStep{
		{3, 1},    // 팔로워1: 3, 팔로워2: 1 -> HW=1
		{5, 3},    // 팔로워1: 5, 팔로워2: 3 -> HW=3
		{7, 7},    // 팔로워1: 7, 팔로워2: 7 -> HW=7
		{10, 8},   // 팔로워1: 10, 팔로워2: 8 -> HW=8
		{10, 10},  // 둘 다 완료 -> HW=10
	}

	for round, step := range steps {
		// 팔로워 부분 fetch: 목표 LEO까지 1개씩 fetch하여 정밀 제어
		for p2.FollowerLogs[1].leo < int64(step.broker1Target) {
			p2.FollowerFetchN(1, 1)
		}
		for p2.FollowerLogs[2].leo < int64(step.broker2Target) {
			p2.FollowerFetchN(2, 1)
		}

		hw := p2.GetHW()
		leaderLEO := p2.LeaderLog.leo
		b1LEO := p2.FollowerLogs[1].leo
		b2LEO := p2.FollowerLogs[2].leo

		fmt.Printf("라운드 %d:\n", round+1)
		fmt.Printf("  브로커1 LEO=%d, 브로커2 LEO=%d\n", b1LEO, b2LEO)

		minFollower := b1LEO
		if b2LEO < minFollower {
			minFollower = b2LEO
		}
		fmt.Printf("  HW = min(ISR LEOs) = min(%d, %d, %d) = %d\n",
			leaderLEO, b1LEO, b2LEO, minFollower)

		// ASCII 시각화
		fmt.Print("  리더   : [")
		for i := int64(0); i < leaderLEO; i++ {
			if i < hw {
				fmt.Print("#") // HW 이하: 커밋됨
			} else {
				fmt.Print(".") // HW 초과: 미커밋
			}
		}
		fmt.Printf("] LEO=%d\n", leaderLEO)

		fmt.Print("  팔로워1: [")
		for i := int64(0); i < leaderLEO; i++ {
			if i < b1LEO {
				fmt.Print("#")
			} else {
				fmt.Print(" ")
			}
		}
		fmt.Printf("] LEO=%d\n", b1LEO)

		fmt.Print("  팔로워2: [")
		for i := int64(0); i < leaderLEO; i++ {
			if i < b2LEO {
				fmt.Print("#")
			} else {
				fmt.Print(" ")
			}
		}
		fmt.Printf("] LEO=%d\n", b2LEO)
		fmt.Printf("  HW=%d   (#=커밋됨, .=미커밋)\n\n", hw)
	}

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("핵심 요약")
	fmt.Println(`
  1. HW = ISR 내 모든 레플리카 LEO의 최솟값
     - Partition.scala의 maybeIncrementLeaderHW()가 이를 계산

  2. 컨슈머는 HW까지만 읽을 수 있음
     - 커밋되지 않은 데이터는 컨슈머에게 노출되지 않음
     - 이는 데이터 일관성을 보장하는 핵심 메커니즘

  3. ISR 축소 시 HW가 올라갈 수 있음
     - 느린 팔로워가 제거되면 남은 ISR의 최소 LEO가 올라감
     - replica.lag.time.max.ms 설정으로 제어

  4. ISR 확장은 팔로워가 리더 LEO를 따라잡을 때 발생
     - 팔로워의 LEO >= 리더 LEO이면 ISR에 재추가

  5. 팔로워 fetch 프로토콜:
     - 팔로워가 리더에게 fetch 요청 -> 리더가 데이터 반환
     - 리더가 팔로워의 LEO를 업데이트 -> HW 재계산
     - 팔로워가 리더의 HW를 fetch 응답으로 받아 자신의 HW 업데이트`)
}
