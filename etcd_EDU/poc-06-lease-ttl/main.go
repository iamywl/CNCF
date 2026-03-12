// poc-06-lease-ttl: Lease TTL 기반 키 자동 만료 시뮬레이션
//
// etcd의 Lease 시스템(server/lease/lease.go, lessor.go)을 기반으로
// TTL 기반 키 자동 만료, KeepAlive, Revoke를 시뮬레이션한다.
//
// 참조: server/lease/lease.go       - Lease 구조체
//       server/lease/lessor.go      - Lessor (Lease 관리자)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"sync"
	"time"
)

// ========== 데이터 모델 ==========

// LeaseID는 Lease 고유 식별자
// etcd의 lease.LeaseID에 해당
type LeaseID int64

// LeaseItem은 Lease에 연결된 키
// etcd의 lease.LeaseItem에 해당
type LeaseItem struct {
	Key string
}

// Lease는 TTL 기반 임대 계약
// etcd의 lease.Lease 구조체를 재현
type Lease struct {
	ID           LeaseID
	ttl          time.Duration // 원래 TTL
	remainingTTL time.Duration // 남은 TTL (체크포인트용)
	expiry       time.Time     // 만료 시각
	itemSet      map[string]struct{}
	revokec      chan struct{} // 폐기 알림 채널

	mu sync.RWMutex
}

// NewLease는 새 Lease를 생성한다
func NewLease(id LeaseID, ttl time.Duration) *Lease {
	return &Lease{
		ID:      id,
		ttl:     ttl,
		expiry:  time.Now().Add(ttl),
		itemSet: make(map[string]struct{}),
		revokec: make(chan struct{}),
	}
}

// Expired는 Lease가 만료되었는지 확인
// etcd의 Lease.expired()에 해당
func (l *Lease) Expired() bool {
	return l.Remaining() <= 0
}

// Remaining은 남은 시간을 반환
// etcd의 Lease.Remaining()에 해당
func (l *Lease) Remaining() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.expiry.IsZero() {
		return time.Duration(1<<63 - 1) // 영구
	}
	return time.Until(l.expiry)
}

// Refresh는 만료 시각을 갱신한다
// etcd의 Lease.refresh()에 해당
func (l *Lease) Refresh() {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := l.remainingTTL
	if remaining <= 0 {
		remaining = l.ttl
	}
	l.expiry = time.Now().Add(remaining)
}

// AttachKey는 키를 Lease에 연결한다
// etcd의 Lease.SetLeaseItem()에 해당
func (l *Lease) AttachKey(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.itemSet[key] = struct{}{}
}

// DetachKey는 키를 Lease에서 분리한다
func (l *Lease) DetachKey(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.itemSet, key)
}

// Keys는 연결된 모든 키를 반환
// etcd의 Lease.Keys()에 해당
func (l *Lease) Keys() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	keys := make([]string, 0, len(l.itemSet))
	for k := range l.itemSet {
		keys = append(keys, k)
	}
	return keys
}

// ========== KV 저장소 ==========

// KVStore는 간단한 키-값 저장소
type KVStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewKVStore() *KVStore {
	return &KVStore{data: make(map[string]string)}
}

func (s *KVStore) Put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *KVStore) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *KVStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}

func (s *KVStore) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]string)
	for k, v := range s.data {
		result[k] = v
	}
	return result
}

// ========== Lessor (Lease 관리자) ==========

// Lessor는 Lease의 생명주기를 관리한다
// etcd의 lease.Lessor 인터페이스와 lessor 구현체에 해당
type Lessor struct {
	mu     sync.RWMutex
	leases map[LeaseID]*Lease
	nextID LeaseID
	kv     *KVStore

	// 만료 콜백
	onExpire func(leaseID LeaseID, keys []string)

	// 만료 감지 루프 제어
	stopCh chan struct{}

	// 로그
	events []string
}

func NewLessor(kv *KVStore) *Lessor {
	return &Lessor{
		leases: make(map[LeaseID]*Lease),
		nextID: 1,
		kv:     kv,
		stopCh: make(chan struct{}),
	}
}

// Grant는 새 Lease를 생성한다
// etcd의 lessor.Grant()에 해당
func (ls *Lessor) Grant(ttl time.Duration) *Lease {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	id := ls.nextID
	ls.nextID++

	lease := NewLease(id, ttl)
	ls.leases[id] = lease

	msg := fmt.Sprintf("Lease 생성: ID=%d, TTL=%v", id, ttl)
	ls.events = append(ls.events, msg)
	fmt.Printf("  [Lessor] %s\n", msg)

	return lease
}

// Revoke는 Lease를 즉시 폐기하고 연결된 키를 삭제한다
// etcd의 lessor.Revoke()에 해당
func (ls *Lessor) Revoke(id LeaseID) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	lease, ok := ls.leases[id]
	if !ok {
		return fmt.Errorf("lease %d not found", id)
	}

	// 연결된 키 삭제
	keys := lease.Keys()
	for _, key := range keys {
		ls.kv.Delete(key)
	}

	// revokec 채널 닫기 (대기 중인 KeepAlive에 알림)
	select {
	case <-lease.revokec:
	default:
		close(lease.revokec)
	}

	delete(ls.leases, id)

	msg := fmt.Sprintf("Lease 폐기: ID=%d, 삭제된 키=%v", id, keys)
	ls.events = append(ls.events, msg)
	fmt.Printf("  [Lessor] %s\n", msg)

	return nil
}

// Renew는 Lease의 TTL을 갱신한다
// etcd의 lessor.Renew()에 해당 (KeepAlive 요청 시 호출)
func (ls *Lessor) Renew(id LeaseID) error {
	ls.mu.RLock()
	lease, ok := ls.leases[id]
	ls.mu.RUnlock()

	if !ok {
		return fmt.Errorf("lease %d not found", id)
	}

	lease.Refresh()
	return nil
}

// Attach는 키를 Lease에 연결한다
// etcd의 lessor.Attach()에 해당
func (ls *Lessor) Attach(id LeaseID, key string) error {
	ls.mu.RLock()
	lease, ok := ls.leases[id]
	ls.mu.RUnlock()

	if !ok {
		return fmt.Errorf("lease %d not found", id)
	}

	lease.AttachKey(key)

	msg := fmt.Sprintf("키 연결: key=%q → Lease %d", key, id)
	ls.events = append(ls.events, msg)
	fmt.Printf("  [Lessor] %s\n", msg)

	return nil
}

// GetLease는 Lease를 조회한다
func (ls *Lessor) GetLease(id LeaseID) *Lease {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.leases[id]
}

// ExpireLoop는 만료된 Lease를 주기적으로 감지하고 폐기한다
// etcd의 lessor.revokeExpiredLeases()에 해당
func (ls *Lessor) ExpireLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ls.stopCh:
			return
		case <-ticker.C:
			ls.revokeExpiredLeases()
		}
	}
}

// revokeExpiredLeases는 만료된 Lease를 찾아 폐기한다
func (ls *Lessor) revokeExpiredLeases() {
	ls.mu.RLock()
	var expired []LeaseID
	for id, lease := range ls.leases {
		if lease.Expired() {
			expired = append(expired, id)
		}
	}
	ls.mu.RUnlock()

	for _, id := range expired {
		fmt.Printf("  [Lessor] 만료 감지: Lease %d\n", id)
		ls.Revoke(id)
	}
}

func (ls *Lessor) Stop() {
	close(ls.stopCh)
}

// ========== KeepAlive 클라이언트 ==========

// KeepAliveClient는 주기적으로 Lease TTL을 갱신하는 클라이언트
// etcd의 clientv3.Lease.KeepAlive()에 해당
type KeepAliveClient struct {
	lessor   *Lessor
	leaseID  LeaseID
	interval time.Duration
	stopCh   chan struct{}
	stopped  bool
	mu       sync.Mutex
}

func NewKeepAliveClient(lessor *Lessor, leaseID LeaseID, interval time.Duration) *KeepAliveClient {
	return &KeepAliveClient{
		lessor:   lessor,
		leaseID:  leaseID,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start는 KeepAlive 루프를 시작한다
func (ka *KeepAliveClient) Start() {
	go func() {
		ticker := time.NewTicker(ka.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ka.stopCh:
				fmt.Printf("  [KeepAlive] Lease %d 갱신 중단\n", ka.leaseID)
				return
			case <-ticker.C:
				err := ka.lessor.Renew(ka.leaseID)
				if err != nil {
					fmt.Printf("  [KeepAlive] Lease %d 갱신 실패: %v\n", ka.leaseID, err)
					return
				}
				lease := ka.lessor.GetLease(ka.leaseID)
				if lease != nil {
					fmt.Printf("  [KeepAlive] Lease %d 갱신 성공 (남은 TTL: %v)\n",
						ka.leaseID, lease.Remaining().Round(time.Millisecond))
				}
			}
		}
	}()
}

// Stop은 KeepAlive를 중단한다
func (ka *KeepAliveClient) Stop() {
	ka.mu.Lock()
	defer ka.mu.Unlock()
	if !ka.stopped {
		ka.stopped = true
		close(ka.stopCh)
	}
}

// ========== 유틸리티 ==========

func printKV(kv *KVStore) {
	data := kv.All()
	if len(data) == 0 {
		fmt.Println("  (비어 있음)")
		return
	}
	for k, v := range data {
		fmt.Printf("    %s = %s\n", k, v)
	}
}

func printLeaseInfo(lease *Lease) {
	if lease == nil {
		fmt.Println("  (Lease 없음)")
		return
	}
	fmt.Printf("    ID: %d\n", lease.ID)
	fmt.Printf("    TTL: %v\n", lease.ttl)
	fmt.Printf("    남은 시간: %v\n", lease.Remaining().Round(time.Millisecond))
	fmt.Printf("    연결된 키: %v\n", lease.Keys())
	fmt.Printf("    만료됨: %v\n", lease.Expired())
}

// ========== 메인 ==========

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" etcd PoC-06: Lease TTL 기반 키 자동 만료")
	fmt.Println("==========================================================")
	fmt.Println()

	kv := NewKVStore()
	lessor := NewLessor(kv)
	go lessor.ExpireLoop()
	defer lessor.Stop()

	// 1. Lease 생성 및 키 연결
	fmt.Println("[1] Lease 생성 및 키 연결")
	fmt.Println("──────────────────────────────────────")

	lease1 := lessor.Grant(2 * time.Second)
	kv.Put("/services/web-1", "10.0.1.10:8080")
	lessor.Attach(lease1.ID, "/services/web-1")
	kv.Put("/services/web-1/health", "healthy")
	lessor.Attach(lease1.ID, "/services/web-1/health")

	lease2 := lessor.Grant(5 * time.Second)
	kv.Put("/services/api-1", "10.0.2.20:9090")
	lessor.Attach(lease2.ID, "/services/api-1")

	fmt.Println("\n  현재 KV 상태:")
	printKV(kv)
	fmt.Println("\n  Lease 1 정보:")
	printLeaseInfo(lease1)
	fmt.Println("\n  Lease 2 정보:")
	printLeaseInfo(lease2)

	// 2. KeepAlive로 Lease 2 유지
	fmt.Println("\n[2] KeepAlive 시작 (Lease 2만)")
	fmt.Println("──────────────────────────────────────")
	fmt.Println("  Lease 1: KeepAlive 없음 (2초 후 만료 예정)")
	fmt.Println("  Lease 2: KeepAlive 활성 (1초마다 갱신)")

	ka := NewKeepAliveClient(lessor, lease2.ID, 1*time.Second)
	ka.Start()

	// 3. Lease 1 만료 대기
	fmt.Println("\n[3] Lease 1 만료 대기 (2초)...")
	fmt.Println("──────────────────────────────────────")
	time.Sleep(2500 * time.Millisecond)

	fmt.Println("\n  현재 KV 상태 (Lease 1 만료 후):")
	printKV(kv)

	l1 := lessor.GetLease(lease1.ID)
	if l1 == nil {
		fmt.Println("\n  Lease 1: 만료되어 삭제됨")
	}
	fmt.Println("\n  Lease 2 정보 (KeepAlive 유지 중):")
	l2 := lessor.GetLease(lease2.ID)
	printLeaseInfo(l2)

	// 4. KeepAlive 중단 → Lease 2 만료
	fmt.Println("\n[4] KeepAlive 중단 → Lease 2 만료 대기")
	fmt.Println("──────────────────────────────────────")

	ka.Stop()
	fmt.Println("  KeepAlive 중단됨. Lease 2 만료 대기 중...")

	// Lease 2의 남은 TTL 확인
	if l2 != nil {
		fmt.Printf("  Lease 2 남은 TTL: %v\n", l2.Remaining().Round(time.Millisecond))
	}

	time.Sleep(6 * time.Second)

	fmt.Println("\n  현재 KV 상태 (Lease 2 만료 후):")
	printKV(kv)

	l2after := lessor.GetLease(lease2.ID)
	if l2after == nil {
		fmt.Println("  Lease 2: 만료되어 삭제됨")
	}

	// 5. 수동 Revoke 시연
	fmt.Println("\n[5] 수동 Revoke 시연")
	fmt.Println("──────────────────────────────────────")

	lease3 := lessor.Grant(30 * time.Second)
	kv.Put("/config/timeout", "30s")
	lessor.Attach(lease3.ID, "/config/timeout")

	fmt.Println("\n  Revoke 전 KV 상태:")
	printKV(kv)

	lessor.Revoke(lease3.ID)

	fmt.Println("\n  Revoke 후 KV 상태:")
	printKV(kv)

	// 요약
	fmt.Println("\n==========================================================")
	fmt.Println(" 시뮬레이션 요약")
	fmt.Println("==========================================================")
	fmt.Println()
	fmt.Println("  etcd Lease TTL 시스템의 핵심 동작:")
	fmt.Println("  1. Grant: Lease 생성 (ID + TTL)")
	fmt.Println("  2. Attach: 키를 Lease에 연결 → TTL 공유")
	fmt.Println("  3. KeepAlive: 주기적으로 Renew하여 만료 방지")
	fmt.Println("  4. 만료 감지: 고루틴이 주기적으로 만료된 Lease 탐색")
	fmt.Println("  5. 자동 삭제: 만료된 Lease의 모든 연결 키 삭제")
	fmt.Println("  6. Revoke: 수동 폐기 시 연결된 키 즉시 삭제")
	fmt.Println()
	fmt.Println("  참조 소스:")
	fmt.Println("  - server/lease/lease.go    (Lease 구조체)")
	fmt.Println("  - server/lease/lessor.go   (Lessor - Lease 관리자)")
	fmt.Println()
}
