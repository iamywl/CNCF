package main

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// etcd Linearizable Read (선형 읽기) PoC - ReadIndex 프로토콜 시뮬레이션
// ============================================================================
//
// etcd의 linearizableReadLoop (server/etcdserver/v3_server.go)을 시뮬레이션한다.
//
// ReadIndex 프로토콜 흐름:
// 1. 클라이언트가 Linearizable 읽기 요청
// 2. linearizableReadNotify()가 readwaitc 채널로 신호
// 3. linearizableReadLoop가 requestCurrentIndex() 호출
// 4. 리더가 과반수에 heartbeat 전송 → 리더 확인
// 5. 리더가 현재 commitIndex 반환
// 6. appliedIndex >= commitIndex 대기
// 7. 읽기 허용 → notifier로 결과 전파
//
// 핵심 최적화: 여러 읽기 요청을 하나의 ReadIndex 호출로 배칭
//
// 참조:
//   server/etcdserver/v3_server.go - linearizableReadLoop(), linearizableReadNotify()
//   server/etcdserver/v3_server.go - requestCurrentIndex()
// ============================================================================

// ---- Notifier (etcd의 notifier) ----

// notifier는 읽기 요청자에게 결과를 전달하는 채널 래퍼이다.
// etcd의 notifier 구조체와 동일:
//   type notifier struct {
//       c   chan struct{}
//       err error
//   }
type notifier struct {
	c   chan struct{}
	err error
}

func newNotifier() *notifier {
	return &notifier{c: make(chan struct{})}
}

func (n *notifier) notify(err error) {
	n.err = err
	close(n.c)
}

// ---- Node (Raft 노드) ----

type NodeState int

const (
	Follower NodeState = iota
	Leader
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "팔로워"
	case Leader:
		return "리더"
	default:
		return "알 수 없음"
	}
}

// Node는 Raft 클러스터의 노드이다.
type Node struct {
	id           int
	state        NodeState
	commitIndex  uint64       // 커밋된 로그 인덱스
	appliedIndex uint64       // 적용된 로그 인덱스
	mu           sync.RWMutex
	data         map[string]string // KV 저장소
	alive        bool
}

func NewNode(id int, state NodeState) *Node {
	return &Node{
		id:    id,
		state: state,
		data:  make(map[string]string),
		alive: true,
	}
}

// ---- Cluster ----

type Cluster struct {
	nodes  []*Node
	leader *Node

	// linearizable read 인프라
	readwaitc    chan struct{}       // 읽기 요청 신호
	readMu       sync.RWMutex
	readNotifier *notifier

	stopC chan struct{}
	doneC chan struct{}

	// 통계
	readIndexCount  int64 // ReadIndex 호출 횟수
	batchedReads    int64 // 배칭된 총 읽기 수
}

func NewCluster(size int) *Cluster {
	nodes := make([]*Node, size)
	for i := 0; i < size; i++ {
		state := Follower
		if i == 0 {
			state = Leader
		}
		nodes[i] = NewNode(i+1, state)
	}

	c := &Cluster{
		nodes:        nodes,
		leader:       nodes[0],
		readwaitc:    make(chan struct{}, 1),
		readNotifier: newNotifier(),
		stopC:        make(chan struct{}),
		doneC:        make(chan struct{}),
	}

	go c.linearizableReadLoop()
	return c
}

// Put은 리더를 통해 데이터를 쓴다 (Raft 복제 시뮬레이션).
func (c *Cluster) Put(key, value string) error {
	leader := c.leader

	// commitIndex 증가
	newIndex := atomic.AddUint64(&leader.commitIndex, 1)

	// 모든 노드에 복제 (데이터 + commitIndex)
	for _, n := range c.nodes {
		if n.alive {
			n.mu.Lock()
			n.data[key] = value
			atomic.StoreUint64(&n.commitIndex, newIndex)
			n.mu.Unlock()
		}
	}

	// appliedIndex 비동기 갱신 (약간의 지연 시뮬레이션)
	// 실제 etcd에서도 Raft apply 루프에 의해 비동기적으로 appliedIndex가 증가
	c.applyAsync(newIndex)

	return nil
}

// applyAsync는 appliedIndex를 비동기적으로 갱신한다.
func (c *Cluster) applyAsync(idx uint64) {
	go func() {
		time.Sleep(time.Duration(1+rand.Intn(3)) * time.Millisecond)
		for _, n := range c.nodes {
			if n.alive {
				for {
					old := atomic.LoadUint64(&n.appliedIndex)
					if old >= idx {
						break
					}
					if atomic.CompareAndSwapUint64(&n.appliedIndex, old, idx) {
						break
					}
				}
			}
		}
	}()
}

// PutSync는 데이터 쓰기 + appliedIndex 즉시 갱신 (데모용).
func (c *Cluster) PutSync(key, value string) error {
	leader := c.leader
	newIndex := atomic.AddUint64(&leader.commitIndex, 1)
	for _, n := range c.nodes {
		if n.alive {
			n.mu.Lock()
			n.data[key] = value
			atomic.StoreUint64(&n.commitIndex, newIndex)
			atomic.StoreUint64(&n.appliedIndex, newIndex)
			n.mu.Unlock()
		}
	}
	return nil
}

// ---- Serializable Read (직렬화 가능 읽기) ----

// SerializableRead는 로컬 노드에서 즉시 읽는다.
// 리더 확인 없이 읽으므로 stale 데이터 가능.
func (c *Cluster) SerializableRead(nodeIdx int, key string) (string, uint64) {
	n := c.nodes[nodeIdx]
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.data[key], atomic.LoadUint64(&n.appliedIndex)
}

// ---- Linearizable Read (선형 읽기) ----

// linearizableReadNotify는 readLoop에 신호를 보내고 결과를 기다린다.
// etcd의 linearizableReadNotify()와 동일한 패턴:
//   func (s *EtcdServer) linearizableReadNotify(ctx context.Context) error {
//       s.readMu.RLock()
//       nc := s.readNotifier
//       s.readMu.RUnlock()
//       select {
//       case s.readwaitc <- struct{}{}:
//       default:
//       }
//       select {
//       case <-nc.c:
//           return nc.err
//       case <-ctx.Done():
//           return ctx.Err()
//       }
//   }
func (c *Cluster) linearizableReadNotify() error {
	c.readMu.RLock()
	nc := c.readNotifier
	c.readMu.RUnlock()

	// readLoop에 신호 전송
	select {
	case c.readwaitc <- struct{}{}:
	default:
	}

	// 결과 대기
	select {
	case <-nc.c:
		return nc.err
	case <-time.After(2 * time.Second):
		return fmt.Errorf("timeout")
	}
}

// linearizableReadLoop는 ReadIndex 요청을 처리하는 메인 루프이다.
// etcd의 linearizableReadLoop()를 시뮬레이션:
//   func (s *EtcdServer) linearizableReadLoop() {
//       for {
//           select {
//           case <-s.readwaitc:
//           case <-s.stopping: return
//           }
//           nextnr := newNotifier()
//           s.readMu.Lock()
//           nr := s.readNotifier
//           s.readNotifier = nextnr
//           s.readMu.Unlock()
//           confirmedIndex, err := s.requestCurrentIndex(...)
//           if appliedIndex < confirmedIndex {
//               <-s.applyWait.Wait(confirmedIndex)
//           }
//           nr.notify(nil)
//       }
//   }
func (c *Cluster) linearizableReadLoop() {
	defer close(c.doneC)

	for {
		select {
		case <-c.readwaitc:
		case <-c.stopC:
			return
		}

		// 새 notifier 교체 (배칭: 현재 대기 중인 모든 읽기를 하나로 묶음)
		nextnr := newNotifier()
		c.readMu.Lock()
		nr := c.readNotifier
		c.readNotifier = nextnr
		c.readMu.Unlock()

		atomic.AddInt64(&c.readIndexCount, 1)

		// requestCurrentIndex: 리더에게 현재 commitIndex 확인
		confirmedIndex, err := c.requestCurrentIndex()
		if err != nil {
			nr.notify(err)
			continue
		}

		// appliedIndex >= confirmedIndex 대기
		deadline := time.After(time.Second)
		waitDone := false
		for !waitDone {
			applied := atomic.LoadUint64(&c.leader.appliedIndex)
			if applied >= confirmedIndex {
				waitDone = true
				break
			}
			select {
			case <-deadline:
				waitDone = true
			case <-c.stopC:
				nr.notify(fmt.Errorf("stopped"))
				return
			default:
				time.Sleep(time.Millisecond)
			}
		}

		// 모든 대기 중인 읽기 요청에 결과 전파
		nr.notify(nil)
	}
}

// requestCurrentIndex는 리더의 commitIndex를 확인한다.
// 과반수 heartbeat 응답으로 리더 자격 확인.
func (c *Cluster) requestCurrentIndex() (uint64, error) {
	leader := c.leader

	// 과반수 heartbeat 전송 및 응답 확인
	quorum := len(c.nodes)/2 + 1
	acks := 1 // 리더 자신

	for _, n := range c.nodes {
		if n.id == leader.id {
			continue
		}
		if n.alive {
			// heartbeat 시뮬레이션 (네트워크 지연)
			time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
			acks++
		}
		if acks >= quorum {
			break
		}
	}

	if acks < quorum {
		return 0, fmt.Errorf("과반수 확인 실패: %d/%d", acks, len(c.nodes))
	}

	return atomic.LoadUint64(&leader.commitIndex), nil
}

// LinearizableRead는 선형 읽기를 수행한다.
func (c *Cluster) LinearizableRead(key string) (string, error) {
	// ReadIndex 프로토콜 수행
	err := c.linearizableReadNotify()
	if err != nil {
		return "", err
	}

	// 리더에서 읽기 (일관성 보장됨)
	c.leader.mu.RLock()
	defer c.leader.mu.RUnlock()
	return c.leader.data[key], nil
}

func (c *Cluster) Close() {
	close(c.stopC)
	<-c.doneC
}

// ============================================================================
// 데모
// ============================================================================

func main() {
	fmt.Println("=== etcd Linearizable Read (선형 읽기) PoC ===")
	fmt.Println()

	// ---- 1. Serializable vs Linearizable 읽기 차이 ----
	fmt.Println("--- 1. Serializable vs Linearizable 읽기 차이 ---")
	cluster := NewCluster(3)

	// 데이터 쓰기 (동기 방식으로 안정적 초기화)
	cluster.PutSync("leader", "node-1")
	cluster.PutSync("version", "3.5.0")

	fmt.Println("  쓰기 완료: leader=node-1, version=3.5.0")
	fmt.Println()

	// Serializable Read (로컬 노드에서 즉시)
	fmt.Println("  [Serializable Read] - 로컬 노드에서 즉시 읽기 (stale 가능)")
	for i := 0; i < 3; i++ {
		val, applied := cluster.SerializableRead(i, "leader")
		fmt.Printf("    노드#%d: leader=%s, appliedIndex=%d\n",
			i+1, val, applied)
	}
	fmt.Println()

	// Linearizable Read (리더 확인 후 읽기)
	fmt.Println("  [Linearizable Read] - ReadIndex 프로토콜 후 읽기 (최신 보장)")
	val, err := cluster.LinearizableRead("leader")
	fmt.Printf("    결과: leader=%s, err=%v\n", val, err)
	fmt.Printf("    ReadIndex 호출 횟수: %d\n", atomic.LoadInt64(&cluster.readIndexCount))
	fmt.Println()

	cluster.Close()

	// ---- 2. Stale Read 시연 ----
	fmt.Println("--- 2. Stale Read 시연 ---")
	cluster2 := NewCluster(3)

	// 초기 쓰기
	cluster2.PutSync("counter", "100")

	// 팔로워의 appliedIndex를 인위적으로 뒤처지게 만들기
	// Put은 비동기 apply → 팔로워가 아직 적용 안 했을 수 있음
	cluster2.Put("counter", "200")

	// 즉시 읽기 (비동기 apply 완료 전)
	val1, idx1 := cluster2.SerializableRead(1, "counter") // 팔로워
	val2, _ := cluster2.SerializableRead(0, "counter")    // 리더

	fmt.Printf("  쓰기 직후 (apply 완료 전):\n")
	fmt.Printf("    Serializable (팔로워#2): counter=%s (appliedIdx=%d) ← stale 가능\n", val1, idx1)
	fmt.Printf("    Serializable (리더#1):   counter=%s\n", val2)

	time.Sleep(20 * time.Millisecond)
	linVal, _ := cluster2.LinearizableRead("counter")
	fmt.Printf("    Linearizable:           counter=%s ← 항상 최신\n", linVal)
	fmt.Println()

	cluster2.Close()

	// ---- 3. 배칭 (여러 읽기를 하나의 ReadIndex로) ----
	fmt.Println("--- 3. 읽기 요청 배칭 ---")
	cluster3 := NewCluster(3)

	cluster3.PutSync("key-1", "value-1")
	cluster3.PutSync("key-2", "value-2")
	cluster3.PutSync("key-3", "value-3")

	// 동시에 여러 읽기 요청
	var wg sync.WaitGroup
	results := make([]string, 10)
	errors := make([]error, 10)

	readStart := time.Now()
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", (idx%3)+1)
			val, err := cluster3.LinearizableRead(key)
			results[idx] = val
			errors[idx] = err
		}(i)
	}
	wg.Wait()
	readDur := time.Since(readStart)

	successCount := 0
	for i := 0; i < 10; i++ {
		if errors[i] == nil {
			successCount++
		}
	}

	readIndexCalls := atomic.LoadInt64(&cluster3.readIndexCount)
	fmt.Printf("  동시 읽기 요청: 10개\n")
	fmt.Printf("  성공: %d개\n", successCount)
	fmt.Printf("  ReadIndex 호출 횟수: %d (배칭 효과: 10개 요청 → %d회 호출)\n",
		readIndexCalls, readIndexCalls)
	fmt.Printf("  총 소요 시간: %v\n", readDur)
	fmt.Println()

	cluster3.Close()

	// ---- 4. 노드 장애 시 선형 읽기 ----
	fmt.Println("--- 4. 노드 장애 시 선형 읽기 ---")
	cluster4 := NewCluster(5) // 5노드 클러스터

	cluster4.PutSync("status", "healthy")

	// 1개 노드 장애 → 과반수 유지 (4/5 ≥ 3)
	cluster4.nodes[4].alive = false
	fmt.Printf("  노드#5 장애 → 4/5 노드 정상\n")
	val, err = cluster4.LinearizableRead("status")
	fmt.Printf("  Linearizable Read: status=%s, err=%v ← 과반수 유지\n", val, err)

	// 2개 노드 추가 장애 → 과반수 미유지 (2/5 < 3)
	cluster4.nodes[3].alive = false
	cluster4.nodes[2].alive = false
	fmt.Printf("  노드#3,#4 추가 장애 → 2/5 노드 정상\n")
	val, err = cluster4.LinearizableRead("status")
	fmt.Printf("  Linearizable Read: status=%s, err=%v ← 과반수 미달\n", val, err)
	fmt.Println()

	cluster4.Close()

	// ---- 5. ReadIndex 프로토콜 단계별 추적 ----
	fmt.Println("--- 5. ReadIndex 프로토콜 단계별 추적 ---")
	cluster5 := NewCluster(3)
	cluster5.PutSync("trace-key", "trace-value")

	fmt.Println("  1단계: 클라이언트 → linearizableReadNotify() 호출")
	fmt.Println("  2단계: readwaitc 채널로 readLoop에 신호")
	fmt.Println("  3단계: readNotifier 교체 (배칭 경계)")
	fmt.Println("  4단계: requestCurrentIndex() → 리더에 ReadIndex 전송")
	fmt.Println("  5단계: 리더가 과반수 heartbeat 전송 → 확인")

	commitIdx := atomic.LoadUint64(&cluster5.leader.commitIndex)
	appliedIdx := atomic.LoadUint64(&cluster5.leader.appliedIndex)
	fmt.Printf("  6단계: commitIndex=%d 반환\n", commitIdx)
	fmt.Printf("  7단계: appliedIndex(%d) >= commitIndex(%d) 확인\n", appliedIdx, commitIdx)

	val, _ = cluster5.LinearizableRead("trace-key")
	fmt.Printf("  8단계: 읽기 허용 → trace-key=%s\n", val)
	fmt.Println()

	cluster5.Close()

	fmt.Println("=== 핵심 정리 ===")
	fmt.Println("1. Serializable Read: 로컬에서 즉시 읽기 (빠르지만 stale 가능)")
	fmt.Println("2. Linearizable Read: ReadIndex로 리더 확인 후 읽기 (최신 보장)")
	fmt.Println("3. ReadIndex 프로토콜: 리더가 과반수 heartbeat로 자격 확인 → commitIndex 반환")
	fmt.Println("4. appliedIndex >= commitIndex 대기 후 읽기 허용")
	fmt.Println("5. 배칭: 동시 읽기 요청을 하나의 ReadIndex 호출로 묶어 효율화")
	fmt.Println("6. 과반수 미달 시 읽기 실패 → 일관성 보장")
}
