package main

import (
	"container/heap"
	"fmt"
	"sync"
	"time"
)

// ============================================================================
// etcd Lease Expiry (키 만료 알림) PoC - 힙 기반 만료 스케줄러
// ============================================================================
//
// etcd의 lease_queue.go (server/lease/lease_queue.go)를 시뮬레이션한다.
// etcd는 container/heap 기반의 최소 힙으로 Lease 만료를 관리한다.
//
// 핵심 구조체:
//   type LeaseWithTime struct {
//       id    LeaseID
//       time  time.Time   // 만료 시간
//       index int         // 힙 인덱스
//   }
//
//   type LeaseQueue []*LeaseWithTime    // 최소 힙
//
//   type LeaseExpiredNotifier struct {
//       m     map[LeaseID]*LeaseWithTime
//       queue LeaseQueue
//   }
//
// 메서드:
//   RegisterOrUpdate() - 새 Lease 등록 또는 TTL 갱신 (heap.Fix)
//   Unregister()       - 만료된 Lease 제거 (heap.Pop)
//   Peek()             - 가장 빨리 만료되는 Lease 확인
//
// 참조: server/lease/lease_queue.go, server/lease/lessor.go
// ============================================================================

type LeaseID int64

// ---- LeaseWithTime (etcd와 동일) ----

// LeaseWithTime은 만료 시간이 포함된 Lease이다.
// etcd의 LeaseWithTime 구조체를 그대로 재현:
//   type LeaseWithTime struct {
//       id    LeaseID
//       time  time.Time
//       index int
//   }
type LeaseWithTime struct {
	id    LeaseID
	time  time.Time // 만료 시간
	index int       // 힙에서의 위치 인덱스
}

// ---- LeaseQueue (최소 힙, etcd와 동일) ----

// LeaseQueue는 만료 시간 기준 최소 힙이다.
// etcd의 LeaseQueue 구현을 그대로 재현.
type LeaseQueue []*LeaseWithTime

func (pq LeaseQueue) Len() int { return len(pq) }

func (pq LeaseQueue) Less(i, j int) bool {
	return pq[i].time.Before(pq[j].time)
}

func (pq LeaseQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *LeaseQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*LeaseWithTime)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *LeaseQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	item.index = -1
	*pq = old[0 : n-1]
	return item
}

// ---- LeaseExpiredNotifier (etcd와 동일) ----

// LeaseExpiredNotifier는 만료된 Lease를 감지하는 큐이다.
// etcd의 LeaseExpiredNotifier를 그대로 재현:
//   type LeaseExpiredNotifier struct {
//       m     map[LeaseID]*LeaseWithTime
//       queue LeaseQueue
//   }
type LeaseExpiredNotifier struct {
	m     map[LeaseID]*LeaseWithTime
	queue LeaseQueue
}

func newLeaseExpiredNotifier() *LeaseExpiredNotifier {
	return &LeaseExpiredNotifier{
		m:     make(map[LeaseID]*LeaseWithTime),
		queue: make(LeaseQueue, 0),
	}
}

// RegisterOrUpdate는 Lease를 등록하거나 TTL을 갱신한다.
// etcd의 RegisterOrUpdate와 동일:
//   func (mq *LeaseExpiredNotifier) RegisterOrUpdate(item *LeaseWithTime) {
//       if old, ok := mq.m[item.id]; ok {
//           old.time = item.time
//           heap.Fix(&mq.queue, old.index)
//       } else {
//           heap.Push(&mq.queue, item)
//           mq.m[item.id] = item
//       }
//   }
func (mq *LeaseExpiredNotifier) RegisterOrUpdate(item *LeaseWithTime) {
	if old, ok := mq.m[item.id]; ok {
		old.time = item.time
		heap.Fix(&mq.queue, old.index)
	} else {
		heap.Push(&mq.queue, item)
		mq.m[item.id] = item
	}
}

// Unregister는 힙에서 가장 빨리 만료되는 Lease를 제거한다.
func (mq *LeaseExpiredNotifier) Unregister() *LeaseWithTime {
	item := heap.Pop(&mq.queue).(*LeaseWithTime)
	delete(mq.m, item.id)
	return item
}

// Peek는 가장 빨리 만료되는 Lease를 확인한다 (제거하지 않음).
func (mq *LeaseExpiredNotifier) Peek() *LeaseWithTime {
	if mq.Len() == 0 {
		return nil
	}
	return mq.queue[0]
}

// Len은 큐의 크기를 반환한다.
func (mq *LeaseExpiredNotifier) Len() int {
	return len(mq.m)
}

// ---- Lease ----

// Lease는 TTL이 있는 임대를 나타낸다.
type Lease struct {
	ID       LeaseID
	TTL      int64         // 초 단위
	Keys     []string      // 연결된 키들
	GrantAt  time.Time     // 부여 시간
	ExpireAt time.Time     // 만료 시간
}

// ---- Lessor (Lease 관리자) ----

// Lessor는 Lease의 생명주기를 관리한다.
// etcd의 lessor 구조체 핵심 기능을 시뮬레이션.
type Lessor struct {
	mu       sync.Mutex
	leases   map[LeaseID]*Lease
	notifier *LeaseExpiredNotifier

	// 만료 콜백
	onExpire func(lease *Lease)

	stopC    chan struct{}
	doneC    chan struct{}

	// 통계
	expired  int
	renewed  int
}

func NewLessor(checkInterval time.Duration, onExpire func(*Lease)) *Lessor {
	l := &Lessor{
		leases:   make(map[LeaseID]*Lease),
		notifier: newLeaseExpiredNotifier(),
		onExpire: onExpire,
		stopC:    make(chan struct{}),
		doneC:    make(chan struct{}),
	}
	go l.expireLoop(checkInterval)
	return l
}

// Grant는 새 Lease를 부여한다.
func (l *Lessor) Grant(id LeaseID, ttl int64) *Lease {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	lease := &Lease{
		ID:       id,
		TTL:      ttl,
		GrantAt:  now,
		ExpireAt: now.Add(time.Duration(ttl) * time.Second),
	}
	l.leases[id] = lease

	l.notifier.RegisterOrUpdate(&LeaseWithTime{
		id:   id,
		time: lease.ExpireAt,
	})

	return lease
}

// AttachKey는 Lease에 키를 연결한다.
func (l *Lessor) AttachKey(id LeaseID, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	lease, ok := l.leases[id]
	if !ok {
		return fmt.Errorf("lease %d not found", id)
	}
	lease.Keys = append(lease.Keys, key)
	return nil
}

// Renew는 Lease의 TTL을 갱신한다.
// etcd의 Renew()는 만료 시간을 TTL만큼 연장하고 힙을 재정렬한다.
func (l *Lessor) Renew(id LeaseID) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	lease, ok := l.leases[id]
	if !ok {
		return fmt.Errorf("lease %d not found", id)
	}

	now := time.Now()
	lease.ExpireAt = now.Add(time.Duration(lease.TTL) * time.Second)
	l.renewed++

	// 힙 재정렬 (핵심: heap.Fix로 위치 갱신)
	l.notifier.RegisterOrUpdate(&LeaseWithTime{
		id:   id,
		time: lease.ExpireAt,
	})

	return nil
}

// Revoke는 Lease를 즉시 취소한다.
func (l *Lessor) Revoke(id LeaseID) (*Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	lease, ok := l.leases[id]
	if !ok {
		return nil, fmt.Errorf("lease %d not found", id)
	}
	delete(l.leases, id)
	return lease, nil
}

// expireLoop는 주기적으로 만료된 Lease를 감지한다.
// etcd의 lessor.revokeExpiredLeases()와 유사한 동작:
//   func (le *lessor) revokeExpiredLeases() {
//       for {
//           item := le.expiredC()  // LeaseExpiredNotifier에서 만료 Lease 가져오기
//           le.Revoke(item.id)
//       }
//   }
func (l *Lessor) expireLoop(interval time.Duration) {
	defer close(l.doneC)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.checkExpired()
		case <-l.stopC:
			return
		}
	}
}

func (l *Lessor) checkExpired() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	for {
		item := l.notifier.Peek()
		if item == nil || item.time.After(now) {
			break
		}

		// 만료된 Lease 제거
		expired := l.notifier.Unregister()
		lease, ok := l.leases[expired.id]
		if !ok {
			continue
		}

		delete(l.leases, expired.id)
		l.expired++

		// 콜백 호출 (lock 해제 후 호출해야 하지만 데모에서는 간소화)
		if l.onExpire != nil {
			l.onExpire(lease)
		}
	}
}

// Stats는 현재 Lessor 상태를 반환한다.
func (l *Lessor) Stats() (active, expired, renewed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.leases), l.expired, l.renewed
}

func (l *Lessor) Close() {
	close(l.stopC)
	<-l.doneC
}

// ============================================================================
// 데모
// ============================================================================

func main() {
	fmt.Println("=== etcd Lease Expiry (키 만료 알림) PoC ===")
	fmt.Println()

	// ---- 1. LeaseExpiredNotifier 기본 동작 ----
	fmt.Println("--- 1. LeaseExpiredNotifier 기본 동작 ---")
	notifier := newLeaseExpiredNotifier()

	now := time.Now()
	// 다양한 만료 시간으로 등록 (역순으로 등록해도 힙이 정렬)
	notifier.RegisterOrUpdate(&LeaseWithTime{id: 3, time: now.Add(3 * time.Second)})
	notifier.RegisterOrUpdate(&LeaseWithTime{id: 1, time: now.Add(1 * time.Second)})
	notifier.RegisterOrUpdate(&LeaseWithTime{id: 5, time: now.Add(5 * time.Second)})
	notifier.RegisterOrUpdate(&LeaseWithTime{id: 2, time: now.Add(2 * time.Second)})
	notifier.RegisterOrUpdate(&LeaseWithTime{id: 4, time: now.Add(4 * time.Second)})

	fmt.Printf("  등록된 Lease 수: %d\n", notifier.Len())
	fmt.Printf("  가장 빨리 만료: Lease#%d (%.1f초 후)\n",
		notifier.Peek().id, notifier.Peek().time.Sub(now).Seconds())

	// 만료 순서대로 Pop
	fmt.Printf("  만료 순서 (힙 Pop):\n")
	for notifier.Len() > 0 {
		item := notifier.Unregister()
		fmt.Printf("    Lease#%d (%.1f초 후 만료)\n",
			item.id, item.time.Sub(now).Seconds())
	}
	fmt.Println()

	// ---- 2. TTL 갱신 시 힙 재정렬 ----
	fmt.Println("--- 2. TTL 갱신 시 힙 재정렬 (heap.Fix) ---")
	notifier2 := newLeaseExpiredNotifier()

	now = time.Now()
	notifier2.RegisterOrUpdate(&LeaseWithTime{id: 1, time: now.Add(1 * time.Second)})
	notifier2.RegisterOrUpdate(&LeaseWithTime{id: 2, time: now.Add(2 * time.Second)})
	notifier2.RegisterOrUpdate(&LeaseWithTime{id: 3, time: now.Add(3 * time.Second)})

	fmt.Printf("  갱신 전 최소: Lease#%d\n", notifier2.Peek().id)

	// Lease#1의 TTL을 10초로 갱신 → 더 이상 먼저 만료되지 않음
	notifier2.RegisterOrUpdate(&LeaseWithTime{id: 1, time: now.Add(10 * time.Second)})
	fmt.Printf("  Lease#1 TTL 갱신(10초) 후 최소: Lease#%d\n", notifier2.Peek().id)

	// Lease#3의 TTL을 0.5초로 갱신 → 가장 먼저 만료
	notifier2.RegisterOrUpdate(&LeaseWithTime{id: 3, time: now.Add(500 * time.Millisecond)})
	fmt.Printf("  Lease#3 TTL 갱신(0.5초) 후 최소: Lease#%d\n", notifier2.Peek().id)
	fmt.Println()

	// ---- 3. Lessor 통합 데모 ----
	fmt.Println("--- 3. Lessor 통합 데모: Lease 생성 → 만료 → 알림 ---")

	var expiredMu sync.Mutex
	var expiredLeases []string

	lessor := NewLessor(50*time.Millisecond, func(lease *Lease) {
		expiredMu.Lock()
		msg := fmt.Sprintf("Lease#%d (TTL=%ds, 키=%v)", lease.ID, lease.TTL, lease.Keys)
		expiredLeases = append(expiredLeases, msg)
		fmt.Printf("    [만료 알림] %s\n", msg)
		expiredMu.Unlock()
	})

	// 다양한 TTL의 Lease 생성
	fmt.Println("  Lease 생성:")
	l1 := lessor.Grant(100, 1) // 1초 후 만료
	lessor.AttachKey(100, "/session/user-1")
	fmt.Printf("    Lease#%d: TTL=%d초, 키=%v\n", l1.ID, l1.TTL, l1.Keys)

	l2 := lessor.Grant(200, 2) // 2초 후 만료
	lessor.AttachKey(200, "/lock/resource-A")
	fmt.Printf("    Lease#%d: TTL=%d초, 키=%v\n", l2.ID, l2.TTL, l2.Keys)

	l3 := lessor.Grant(300, 3) // 3초 후 만료
	lessor.AttachKey(300, "/service/api-server")
	lessor.AttachKey(300, "/service/api-server/health")
	fmt.Printf("    Lease#%d: TTL=%d초, 키=%v\n", l3.ID, l3.TTL, l3.Keys)

	fmt.Println()
	fmt.Println("  만료 대기 중 (순서대로 만료될 예정)...")

	// 만료 대기
	time.Sleep(3500 * time.Millisecond)

	active, expired, renewed := lessor.Stats()
	fmt.Printf("\n  최종 통계: 활성=%d, 만료=%d, 갱신=%d\n", active, expired, renewed)
	fmt.Println()

	lessor.Close()

	// ---- 4. TTL 갱신 (Renew) ----
	fmt.Println("--- 4. TTL 갱신 (Lease KeepAlive) ---")

	lessor2 := NewLessor(50*time.Millisecond, func(lease *Lease) {
		fmt.Printf("    [만료 알림] Lease#%d 만료됨\n", lease.ID)
	})

	// 1초 TTL Lease 생성
	lessor2.Grant(500, 1)
	lessor2.AttachKey(500, "/ephemeral/node-1")
	fmt.Printf("  Lease#500 생성: TTL=1초\n")

	// 3회 갱신 (각각 0.8초마다)
	for i := 0; i < 3; i++ {
		time.Sleep(800 * time.Millisecond)
		err := lessor2.Renew(500)
		if err != nil {
			fmt.Printf("  갱신 실패: %v\n", err)
			break
		}
		fmt.Printf("  %.1f초 경과: Lease#500 갱신 성공 (TTL 1초 재시작)\n",
			float64(i+1)*0.8)
	}

	// 갱신 중단 → 만료 대기
	fmt.Println("  갱신 중단 → 1초 후 만료 예정...")
	time.Sleep(1500 * time.Millisecond)

	active, expired, renewed = lessor2.Stats()
	fmt.Printf("  최종: 활성=%d, 만료=%d, 갱신=%d\n", active, expired, renewed)
	fmt.Println()

	lessor2.Close()

	// ---- 5. 대량 Lease 만료 순서 검증 ----
	fmt.Println("--- 5. 대량 Lease 만료 순서 검증 ---")
	var orderMu sync.Mutex
	var expireOrder []LeaseID

	lessor3 := NewLessor(20*time.Millisecond, func(lease *Lease) {
		orderMu.Lock()
		expireOrder = append(expireOrder, lease.ID)
		orderMu.Unlock()
	})

	// 10개 Lease를 다양한 TTL로 생성 (역순)
	fmt.Println("  생성 순서 (역순 TTL):")
	for i := 10; i >= 1; i-- {
		id := LeaseID(i * 10)
		ttl := i // i초 후 만료
		lessor3.Grant(id, int64(ttl))
		fmt.Printf("    Lease#%d: TTL=%d초\n", id, ttl)
	}

	// 최대 TTL + 여유 시간만큼 대기
	fmt.Println("  만료 대기 중...")
	time.Sleep(11 * time.Second)

	orderMu.Lock()
	fmt.Printf("  만료 순서: ")
	for i, id := range expireOrder {
		if i > 0 {
			fmt.Print(" → ")
		}
		fmt.Printf("#%d", id)
	}
	fmt.Println()

	// 순서 검증
	ordered := true
	for i := 1; i < len(expireOrder); i++ {
		if expireOrder[i] < expireOrder[i-1] {
			ordered = false
			break
		}
	}
	fmt.Printf("  TTL 순서대로 만료: %v\n", ordered)
	orderMu.Unlock()

	lessor3.Close()
	fmt.Println()

	fmt.Println("=== 핵심 정리 ===")
	fmt.Println("1. LeaseQueue: container/heap 기반 최소 힙으로 만료 시간 정렬")
	fmt.Println("2. RegisterOrUpdate: 기존 Lease면 heap.Fix, 새 Lease면 heap.Push")
	fmt.Println("3. Peek: O(1)으로 가장 빨리 만료되는 Lease 확인")
	fmt.Println("4. 갱신(Renew): 만료 시간 변경 후 heap.Fix로 힙 재정렬")
	fmt.Println("5. 만료 감지 루프: 주기적으로 Peek → 만료 확인 → Unregister → 콜백")
}
