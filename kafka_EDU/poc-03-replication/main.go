package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Kafka Leader-Follower Replication with ISR PoC
//
// 실제 Kafka 소스 참조:
//   - Partition.scala: ISR 관리 (maybeExpandIsr, maybeShrinkIsr, updateFollowerFetchState)
//   - ReplicaManager.scala: 복제 관리, high watermark 계산
//
// 핵심 설계:
//   1) Leader가 쓰기를 수락하고 로컬 로그에 기록
//   2) Follower가 주기적으로 Leader에서 Fetch하여 복제
//   3) ISR: 리더의 LEO를 일정 시간 내에 따라잡은 팔로워 집합
//   4) High Watermark: ISR 내 모든 레플리카의 LEO 중 최솟값
//      -> HW 이하의 메시지만 컨슈머에게 노출

const (
	replicaLagTimeMaxMs = 3000 // ISR에서 제거되는 최대 지연 시간 (ms)
	numReplicas         = 4    // 총 레플리카 수 (리더 1 + 팔로워 3)
	fetchIntervalMs     = 200  // 팔로워 fetch 주기 (ms)
)

// LogEntry는 로그의 한 항목을 나타낸다.
type LogEntry struct {
	Offset    int64
	Timestamp time.Time
	Value     string
}

// ReplicaLog는 한 레플리카의 로그를 나타낸다.
type ReplicaLog struct {
	mu      sync.RWMutex
	entries []LogEntry
}

func (rl *ReplicaLog) append(entry LogEntry) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.entries = append(rl.entries, entry)
}

func (rl *ReplicaLog) logEndOffset() int64 {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return int64(len(rl.entries))
}

func (rl *ReplicaLog) entriesFrom(offset int64) []LogEntry {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	if offset >= int64(len(rl.entries)) {
		return nil
	}
	result := make([]LogEntry, len(rl.entries)-int(offset))
	copy(result, rl.entries[offset:])
	return result
}

// ReplicaState는 Partition.scala의 Replica 상태를 시뮬레이션한다.
// Kafka의 Replica.stateSnapshot: logEndOffset, lastCaughtUpTimeMs, brokerEpoch
type ReplicaState struct {
	BrokerID         int
	LogEndOffset     int64
	LastCaughtUpTime time.Time
	LastFetchTime    time.Time
}

// Partition은 Kafka의 Partition.scala를 시뮬레이션한다.
type Partition struct {
	mu sync.RWMutex

	topic        string
	partitionID  int
	leaderID     int
	leaderLog    *ReplicaLog
	followerLogs map[int]*ReplicaLog // brokerID -> log

	// ISR 관리 (Partition.scala의 partitionState)
	isr          map[int]bool // ISR에 속한 brokerID 집합
	replicaState map[int]*ReplicaState

	// High Watermark
	highWatermark int64

	// 이벤트 로그
	events []string
}

func newPartition(topic string, partitionID int, leaderID int, replicaIDs []int) *Partition {
	p := &Partition{
		topic:        topic,
		partitionID:  partitionID,
		leaderID:     leaderID,
		leaderLog:    &ReplicaLog{},
		followerLogs: make(map[int]*ReplicaLog),
		isr:          make(map[int]bool),
		replicaState: make(map[int]*ReplicaState),
	}

	now := time.Now()
	for _, id := range replicaIDs {
		p.isr[id] = true
		p.replicaState[id] = &ReplicaState{
			BrokerID:         id,
			LogEndOffset:     0,
			LastCaughtUpTime: now,
			LastFetchTime:    now,
		}
		if id != leaderID {
			p.followerLogs[id] = &ReplicaLog{}
		}
	}

	return p
}

// appendToLeader는 리더에 레코드를 추가한다.
func (p *Partition) appendToLeader(value string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	offset := p.leaderLog.logEndOffset()
	entry := LogEntry{
		Offset:    offset,
		Timestamp: time.Now(),
		Value:     value,
	}
	p.leaderLog.append(entry)

	// 리더 자신의 상태 업데이트
	state := p.replicaState[p.leaderID]
	state.LogEndOffset = p.leaderLog.logEndOffset()
	state.LastCaughtUpTime = time.Now()

	return offset
}

// fetchFromLeader는 팔로워가 리더에서 데이터를 가져오는 것을 시뮬레이션한다.
// Partition.scala의 updateFollowerFetchState()와 동일한 흐름:
//   1) 팔로워의 LEO 업데이트
//   2) maybeExpandIsr() 호출
//   3) maybeIncrementLeaderHW() 호출
func (p *Partition) fetchFromLeader(followerID int) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	followerLog := p.followerLogs[followerID]
	if followerLog == nil {
		return 0
	}

	followerLEO := followerLog.logEndOffset()
	entries := p.leaderLog.entriesFrom(followerLEO)

	if len(entries) == 0 {
		// 이미 따라잡은 상태
		state := p.replicaState[followerID]
		state.LastFetchTime = time.Now()
		if followerLEO >= p.leaderLog.logEndOffset() {
			state.LastCaughtUpTime = time.Now()
		}
		return 0
	}

	// 팔로워 로그에 복제
	for _, entry := range entries {
		followerLog.append(entry)
	}

	// 팔로워 상태 업데이트 (updateFollowerFetchState)
	state := p.replicaState[followerID]
	prevLEO := state.LogEndOffset
	state.LogEndOffset = followerLog.logEndOffset()
	state.LastFetchTime = time.Now()

	leaderLEO := p.leaderLog.logEndOffset()
	if state.LogEndOffset >= leaderLEO {
		state.LastCaughtUpTime = time.Now()
	}

	// ISR 확장 확인 (maybeExpandIsr)
	// Partition.scala:876-894: ISR에 없는 팔로워가 HW를 따라잡으면 ISR에 추가
	if !p.isr[followerID] {
		p.maybeExpandIsr(followerID)
	}

	// HW 업데이트 (maybeIncrementLeaderHW)
	if prevLEO != state.LogEndOffset {
		p.maybeIncrementLeaderHW()
	}

	return len(entries)
}

// maybeExpandIsr는 Partition.scala의 maybeExpandIsr()를 구현한다.
// 조건: followerLEO >= leaderHW && followerLEO >= leaderEpochStartOffset
// Kafka 원본 (Partition.scala:907-911):
//   followerEndOffset >= leaderLog.highWatermark &&
//   leaderEpochStartOffsetOpt.exists(followerEndOffset >= _)
func (p *Partition) maybeExpandIsr(followerID int) {
	state := p.replicaState[followerID]
	if state.LogEndOffset >= p.highWatermark {
		p.isr[followerID] = true
		event := fmt.Sprintf("[ISR EXPAND] broker-%d 추가 (LEO=%d >= HW=%d), ISR=%v",
			followerID, state.LogEndOffset, p.highWatermark, p.isrList())
		p.events = append(p.events, event)
	}
}

// maybeShrinkIsr는 Partition.scala의 maybeShrinkIsr()를 구현한다.
// Kafka 원본 (Partition.scala:1133-1139):
//   followerReplica.stateSnapshot.isCaughtUp(leaderEndOffset, currentTimeMs, maxLagMs)
// -> lastCaughtUpTimeMs + maxLagMs > currentTimeMs 이면 in-sync
func (p *Partition) maybeShrinkIsr() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	leaderLEO := p.leaderLog.logEndOffset()
	outOfSync := make([]int, 0)

	for brokerID := range p.isr {
		if brokerID == p.leaderID {
			continue
		}
		state := p.replicaState[brokerID]

		// Kafka의 isCaughtUp 로직:
		// 팔로워가 리더의 LEO와 같으면 in-sync
		// 그렇지 않으면 lastCaughtUpTime + maxLagTime > now인지 확인
		if state.LogEndOffset < leaderLEO {
			lagMs := now.Sub(state.LastCaughtUpTime).Milliseconds()
			if lagMs > replicaLagTimeMaxMs {
				outOfSync = append(outOfSync, brokerID)
			}
		}
	}

	for _, brokerID := range outOfSync {
		delete(p.isr, brokerID)
		state := p.replicaState[brokerID]
		lagMs := now.Sub(state.LastCaughtUpTime).Milliseconds()
		event := fmt.Sprintf("[ISR SHRINK] broker-%d 제거 (LEO=%d, leaderLEO=%d, lag=%dms > %dms), ISR=%v",
			brokerID, state.LogEndOffset, leaderLEO, lagMs, replicaLagTimeMaxMs, p.isrList())
		p.events = append(p.events, event)
	}

	if len(outOfSync) > 0 {
		p.maybeIncrementLeaderHW()
	}
}

// maybeIncrementLeaderHW는 High Watermark를 업데이트한다.
// HW = ISR 내 모든 레플리카의 LEO 중 최솟값
// Kafka에서 HW 이하의 메시지만 컨슈머에게 노출된다.
func (p *Partition) maybeIncrementLeaderHW() {
	minLEO := p.leaderLog.logEndOffset()

	for brokerID := range p.isr {
		state := p.replicaState[brokerID]
		if state.LogEndOffset < minLEO {
			minLEO = state.LogEndOffset
		}
	}

	if minLEO > p.highWatermark {
		oldHW := p.highWatermark
		p.highWatermark = minLEO
		event := fmt.Sprintf("[HW UPDATE] %d -> %d (ISR=%v)", oldHW, minLEO, p.isrList())
		p.events = append(p.events, event)
	}
}

func (p *Partition) isrList() []int {
	list := make([]int, 0, len(p.isr))
	for id := range p.isr {
		list = append(list, id)
	}
	return list
}

func (p *Partition) status() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("  Leader: broker-%d (LEO=%d)\n", p.leaderID, p.leaderLog.logEndOffset()))
	sb.WriteString(fmt.Sprintf("  High Watermark: %d\n", p.highWatermark))
	sb.WriteString(fmt.Sprintf("  ISR: %v\n", p.isrList()))

	for brokerID, state := range p.replicaState {
		role := "Follower"
		if brokerID == p.leaderID {
			role = "Leader  "
		}
		inISR := "  "
		if p.isr[brokerID] {
			inISR = "ISR"
		}
		lagMs := time.Since(state.LastCaughtUpTime).Milliseconds()
		sb.WriteString(fmt.Sprintf("  broker-%d [%s] [%s] LEO=%d, lastCaughtUp=%dms ago\n",
			brokerID, role, inISR, state.LogEndOffset, lagMs))
	}
	return sb.String()
}

func main() {
	fmt.Println("========================================")
	fmt.Println(" Kafka Leader-Follower Replication PoC")
	fmt.Println(" Based on: Partition.scala, ReplicaManager.scala")
	fmt.Println("========================================")

	replicaIDs := []int{0, 1, 2, 3}
	partition := newPartition("test-topic", 0, 0, replicaIDs)

	// --- 1. 정상 복제 ---
	fmt.Println("\n[1] 정상 복제: 리더에 5개 레코드 추가 -> 팔로워 fetch")
	for i := 0; i < 5; i++ {
		offset := partition.appendToLeader(fmt.Sprintf("message-%d", i))
		fmt.Printf("  Leader append: offset=%d\n", offset)
	}

	fmt.Printf("\n  Fetch 전 상태:\n%s\n", partition.status())

	// 모든 팔로워가 fetch
	for _, fid := range []int{1, 2, 3} {
		n := partition.fetchFromLeader(fid)
		fmt.Printf("  broker-%d fetched %d records\n", fid, n)
	}
	fmt.Printf("\n  Fetch 후 상태:\n%s\n", partition.status())

	// --- 2. 느린 팔로워 시나리오 ---
	fmt.Println("[2] 느린 팔로워 시나리오: broker-3이 fetch를 멈춤")
	fmt.Println("    -> replicaLagTimeMaxMs 초과 시 ISR에서 제거")
	fmt.Println()

	// broker-3의 lastCaughtUpTime을 과거로 조작하여 지연 시뮬레이션
	partition.mu.Lock()
	partition.replicaState[3].LastCaughtUpTime = time.Now().Add(-4 * time.Second) // 4초 전
	partition.mu.Unlock()

	// 리더에 추가 레코드
	for i := 5; i < 10; i++ {
		partition.appendToLeader(fmt.Sprintf("message-%d", i))
	}

	// broker-1, 2만 fetch (broker-3은 느려서 fetch 안 함)
	for _, fid := range []int{1, 2} {
		partition.fetchFromLeader(fid)
	}

	// ISR 축소 확인
	partition.maybeShrinkIsr()

	fmt.Printf("  ISR 축소 후 상태:\n%s\n", partition.status())

	// --- 3. 팔로워 복구 ---
	fmt.Println("[3] 팔로워 복구: broker-3이 다시 fetch 시작 -> ISR 재진입")

	// broker-3이 따라잡기 fetch
	for {
		n := partition.fetchFromLeader(3)
		if n == 0 {
			break
		}
		fmt.Printf("  broker-3 fetched %d records\n", n)
	}

	fmt.Printf("\n  복구 후 상태:\n%s\n", partition.status())

	// --- 4. 동시 쓰기 + 복제 시뮬레이션 ---
	fmt.Println("[4] 동시 쓰기 + 비동기 복제 시뮬레이션 (2초)")
	fmt.Println("    리더: 100ms마다 쓰기, 팔로워: 200ms마다 fetch")
	fmt.Println("    broker-2: 50% 확률로 느려짐 (fetch 건너뜀)")
	fmt.Println()

	var wg sync.WaitGroup
	done := make(chan struct{})

	// 리더 쓰기 고루틴
	wg.Add(1)
	go func() {
		defer wg.Done()
		msgID := 10
		for {
			select {
			case <-done:
				return
			default:
				partition.appendToLeader(fmt.Sprintf("async-msg-%d", msgID))
				msgID++
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	// 팔로워 fetch 고루틴
	for _, fid := range []int{1, 2, 3} {
		wg.Add(1)
		go func(brokerID int) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					if brokerID == 2 && rand.Float64() < 0.5 {
						// broker-2 느린 시뮬레이션
						time.Sleep(time.Duration(fetchIntervalMs*3) * time.Millisecond)
						continue
					}
					partition.fetchFromLeader(brokerID)
					time.Sleep(time.Duration(fetchIntervalMs) * time.Millisecond)
				}
			}
		}(fid)
	}

	// ISR 모니터 고루틴
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				partition.maybeShrinkIsr()
				time.Sleep(500 * time.Millisecond)
			}
		}
	}()

	// 2초 후 종료
	time.Sleep(2 * time.Second)
	close(done)
	wg.Wait()

	fmt.Printf("  최종 상태:\n%s\n", partition.status())

	// --- 5. 이벤트 로그 ---
	fmt.Println("[5] ISR/HW 변경 이벤트 로그")
	partition.mu.RLock()
	for i, event := range partition.events {
		fmt.Printf("  %3d: %s\n", i+1, event)
		if i >= 29 {
			fmt.Printf("  ... (총 %d개 이벤트 중 30개만 표시)\n", len(partition.events))
			break
		}
	}
	partition.mu.RUnlock()

	// --- 6. 설계 요약 ---
	fmt.Println("\n[6] Kafka Replication 설계 요약")
	fmt.Println("  +----------+     fetch     +----------+")
	fmt.Println("  | Leader   |<--------------| Follower |")
	fmt.Println("  | (broker) |               | (broker) |")
	fmt.Println("  +----------+               +----------+")
	fmt.Println("  | LEO=100  |               | LEO=98   |")
	fmt.Println("  | HW=98   |               |          |")
	fmt.Println("  +----------+               +----------+")
	fmt.Println()
	fmt.Println("  HW 계산: min(ISR 내 모든 replica의 LEO)")
	fmt.Println("  ISR 축소: lastCaughtUpTime + replicaLagTimeMaxMs < now")
	fmt.Println("  ISR 확장: followerLEO >= leaderHW")
	fmt.Println("  컨슈머: offset < HW인 메시지만 읽기 가능")
}
