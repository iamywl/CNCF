// containerd 이미지 언팩(Unpacking) 시스템 PoC
//
// 핵심 개념 시뮬레이션:
// 1. ChainID 계산 알고리즘 (OCI 표준)
// 2. topHalf / bottomHalf 분리 패턴
// 3. Fetch-Unpack 파이프라인 병렬화
// 4. 중복 억제 (KeyedLocker)
// 5. Limiter 기반 동시성 제어
//
// 실행: go run main.go

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── 1. ChainID 계산 ──────────────────────────────────────────

// Digest는 SHA256 해시를 표현한다
type Digest string

// ChainIDs는 DiffID 목록으로부터 ChainID 목록을 계산한다
// OCI Image Spec 표준: ChainID(L0..Ln) = SHA256(ChainID(L0..Ln-1) + " " + DiffID(Ln))
func ChainIDs(diffIDs []Digest) []Digest {
	if len(diffIDs) == 0 {
		return nil
	}
	chainIDs := make([]Digest, len(diffIDs))
	chainIDs[0] = diffIDs[0]
	for i := 1; i < len(diffIDs); i++ {
		h := sha256.New()
		h.Write([]byte(string(chainIDs[i-1]) + " " + string(diffIDs[i])))
		chainIDs[i] = Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
	}
	return chainIDs
}

// ── 2. 핵심 인터페이스 ──────────────────────────────────────

// Limiter는 동시성을 제한한다
type Limiter struct {
	ch chan struct{}
}

func NewLimiter(n int) *Limiter {
	ch := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		ch <- struct{}{}
	}
	return &Limiter{ch: ch}
}

func (l *Limiter) Acquire(ctx context.Context) error {
	select {
	case <-l.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Limiter) Release() {
	l.ch <- struct{}{}
}

// KeyedLocker는 키 기반 중복 억제를 제공한다
type KeyedLocker struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func NewKeyedLocker() *KeyedLocker {
	return &KeyedLocker{locks: make(map[string]chan struct{})}
}

func (k *KeyedLocker) Lock(ctx context.Context, key string) error {
	k.mu.Lock()
	ch, exists := k.locks[key]
	if !exists {
		k.locks[key] = make(chan struct{})
		k.mu.Unlock()
		return nil
	}
	k.mu.Unlock()
	// 다른 goroutine이 처리 중, 완료 대기
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (k *KeyedLocker) Unlock(key string) {
	k.mu.Lock()
	if ch, ok := k.locks[key]; ok {
		close(ch)
		delete(k.locks, key)
	}
	k.mu.Unlock()
}

// ── 3. 시뮬레이션 타입 ──────────────────────────────────────

// Layer는 이미지 레이어를 시뮬레이션한다
type Layer struct {
	Digest   Digest
	DiffID   Digest
	Size     int64
	Fetched  bool
	Applied  bool
}

// Snapshot은 스냅샷 상태를 표현한다
type Snapshot struct {
	Key      string
	Parent   string
	ChainID  string
	Active   bool
}

// SnapshotStore는 인메모리 스냅샷 저장소이다
type SnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]*Snapshot
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{snapshots: make(map[string]*Snapshot)}
}

func (s *SnapshotStore) Prepare(key, parent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.snapshots[key]; exists {
		return fmt.Errorf("already exists: %s", key)
	}
	s.snapshots[key] = &Snapshot{
		Key:    key,
		Parent: parent,
		Active: true,
	}
	return nil
}

func (s *SnapshotStore) Commit(chainID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snapshots[key]
	if !ok {
		return fmt.Errorf("not found: %s", key)
	}
	if _, exists := s.snapshots[chainID]; exists {
		// 이미 커밋됨 (중복 언팩)
		delete(s.snapshots, key)
		return nil
	}
	snap.ChainID = chainID
	snap.Active = false
	s.snapshots[chainID] = snap
	delete(s.snapshots, key)
	return nil
}

func (s *SnapshotStore) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshots, key)
}

func (s *SnapshotStore) List() []*Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Snapshot
	for _, snap := range s.snapshots {
		result = append(result, snap)
	}
	return result
}

// ── 4. Unpacker 구현 ───────────────────────────────────────

type unpackStatus struct {
	err     error
	layer   int
	commitF func(abort bool) error
}

type Unpacker struct {
	snapshotStore   *SnapshotStore
	fetchLimiter    *Limiter
	unpackLimiter   *Limiter
	dedup           *KeyedLocker
	unpacked        atomic.Int32
	parallelSupport bool
}

func NewUnpacker(ss *SnapshotStore, fetchLimit, unpackLimit int, parallel bool) *Unpacker {
	return &Unpacker{
		snapshotStore:   ss,
		fetchLimiter:    NewLimiter(fetchLimit),
		unpackLimiter:   NewLimiter(unpackLimit),
		dedup:           NewKeyedLocker(),
		parallelSupport: parallel,
	}
}

func (u *Unpacker) Unpack(ctx context.Context, layers []Layer) error {
	fmt.Println("\n=== 언팩 시작 ===")

	// ChainID 계산
	diffIDs := make([]Digest, len(layers))
	for i, l := range layers {
		diffIDs[i] = l.DiffID
	}
	chainIDs := ChainIDs(diffIDs)

	fmt.Println("\n[ChainID 계산 결과]")
	for i, cid := range chainIDs {
		fmt.Printf("  Layer %d: DiffID=%s → ChainID=%s\n", i,
			string(diffIDs[i])[:20]+"...",
			string(cid)[:20]+"...")
	}

	// Fetch 채널 설정
	fetchDone := make([]chan struct{}, len(layers))
	for i := range fetchDone {
		fetchDone[i] = make(chan struct{})
	}

	// 비동기 Fetch 시작
	go func() {
		for i, layer := range layers {
			u.fetchLimiter.Acquire(ctx)
			fmt.Printf("  [Fetch] 레이어 %d 다운로드 시작 (size=%d)\n", i, layer.Size)
			time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)
			fmt.Printf("  [Fetch] 레이어 %d 다운로드 완료\n", i)
			u.fetchLimiter.Release()
			close(fetchDone[i])
		}
	}()

	if u.parallelSupport {
		return u.unpackParallel(ctx, layers, chainIDs, fetchDone)
	}
	return u.unpackSequential(ctx, layers, chainIDs, fetchDone)
}

func (u *Unpacker) unpackSequential(ctx context.Context, layers []Layer, chainIDs []Digest, fetchDone []chan struct{}) error {
	fmt.Println("\n[순차 언팩 모드]")
	for i := range layers {
		chainID := string(chainIDs[i])
		parent := ""
		if i > 0 {
			parent = string(chainIDs[i-1])
		}

		// 중복 억제
		dedupKey := fmt.Sprintf("sn://overlayfs/%s", chainID)
		if err := u.dedup.Lock(ctx, dedupKey); err != nil {
			return err
		}

		// topHalf: 스냅샷 준비
		key := fmt.Sprintf("unpack-%d-%s", time.Now().UnixNano(), chainID[:16])
		fmt.Printf("  [topHalf] 레이어 %d: Prepare(%s, parent=%s)\n", i, key[:20]+"...", shortID(parent))

		if err := u.snapshotStore.Prepare(key, parent); err != nil {
			u.dedup.Unlock(dedupKey)
			return fmt.Errorf("prepare failed: %w", err)
		}

		// Fetch 완료 대기
		<-fetchDone[i]

		// Unpack limiter
		u.unpackLimiter.Acquire(ctx)

		// Apply 시뮬레이션
		fmt.Printf("  [Apply]  레이어 %d: tar 추출 중...\n", i)
		time.Sleep(time.Duration(30+rand.Intn(50)) * time.Millisecond)

		u.unpackLimiter.Release()

		// bottomHalf: 커밋
		fmt.Printf("  [bottomHalf] 레이어 %d: Commit(%s)\n", i, shortID(chainID))
		if err := u.snapshotStore.Commit(chainID, key); err != nil {
			u.snapshotStore.Remove(key)
			u.dedup.Unlock(dedupKey)
			return fmt.Errorf("commit failed: %w", err)
		}

		u.dedup.Unlock(dedupKey)
		u.unpacked.Add(1)
		fmt.Printf("  [완료]   레이어 %d 언팩 완료\n", i)
	}
	return nil
}

func (u *Unpacker) unpackParallel(ctx context.Context, layers []Layer, chainIDs []Digest, fetchDone []chan struct{}) error {
	fmt.Println("\n[병렬 언팩 모드]")

	statusChs := make([]chan *unpackStatus, len(layers))

	for i := range layers {
		chainID := string(chainIDs[i])

		// topHalf: 각 레이어를 goroutine에서 병렬 처리
		key := fmt.Sprintf("unpack-%d-%s", time.Now().UnixNano(), chainID[:16])
		fmt.Printf("  [topHalf] 레이어 %d: Prepare (병렬)\n", i)

		if err := u.snapshotStore.Prepare(key, ""); err != nil {
			return err
		}

		statusCh := make(chan *unpackStatus, 1)
		statusChs[i] = statusCh

		go func(idx int, k, cid string) {
			// Fetch 완료 대기
			<-fetchDone[idx]

			u.unpackLimiter.Acquire(ctx)

			// Apply 시뮬레이션
			fmt.Printf("  [Apply]  레이어 %d: tar 추출 중 (병렬)...\n", idx)
			time.Sleep(time.Duration(30+rand.Intn(50)) * time.Millisecond)

			u.unpackLimiter.Release()

			status := &unpackStatus{
				layer: idx,
				commitF: func(abort bool) error {
					if abort {
						u.snapshotStore.Remove(k)
						return nil
					}
					parent := ""
					if idx > 0 {
						parent = string(chainIDs[idx-1])
					}
					// rebase: 커밋 시점에 parent 설정
					u.snapshotStore.mu.Lock()
					if snap, ok := u.snapshotStore.snapshots[k]; ok {
						snap.Parent = parent
					}
					u.snapshotStore.mu.Unlock()
					return u.snapshotStore.Commit(cid, k)
				},
			}
			statusCh <- status
		}(i, key, chainID)
	}

	// bottomHalf: 순차적으로 커밋
	fmt.Println("  [bottomHalf] 순차 커밋 시작")
	var prevErr error
	for i, ch := range statusChs {
		status := <-ch
		if status.err != nil {
			status.commitF(true)
			prevErr = status.err
			continue
		}
		if prevErr != nil {
			status.commitF(true)
			continue
		}
		if err := status.commitF(false); err != nil {
			prevErr = err
			continue
		}
		u.unpacked.Add(1)
		fmt.Printf("  [bottomHalf] 레이어 %d 커밋 완료\n", i)
	}

	return prevErr
}

func shortID(s string) string {
	if len(s) == 0 {
		return "(none)"
	}
	if len(s) > 16 {
		return s[:16] + "..."
	}
	return s
}

// ── 5. 메인 ───────────────────────────────────────────────

func main() {
	fmt.Println("========================================")
	fmt.Println("containerd Image Unpacking 시스템 PoC")
	fmt.Println("========================================")

	// ── 데모 1: ChainID 계산 ──
	fmt.Println("\n--- 데모 1: ChainID 계산 알고리즘 ---")
	diffIDs := []Digest{
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
	chainIDs := ChainIDs(diffIDs)
	for i, cid := range chainIDs {
		fmt.Printf("  Layer %d:\n", i)
		fmt.Printf("    DiffID  = %s\n", diffIDs[i])
		fmt.Printf("    ChainID = %s\n", cid)
		if i == 0 {
			fmt.Printf("    공식: ChainID = DiffID (첫 레이어)\n")
		} else {
			fmt.Printf("    공식: SHA256(ChainID[%d] + \" \" + DiffID[%d])\n", i-1, i)
		}
	}
	// 검증: 첫 레이어의 ChainID == DiffID
	if chainIDs[0] == diffIDs[0] {
		fmt.Println("  [검증 OK] 첫 레이어 ChainID == DiffID")
	}
	// 검증: 두 번째 레이어의 ChainID != DiffID
	if chainIDs[1] != diffIDs[1] {
		fmt.Println("  [검증 OK] 두 번째 레이어 ChainID != DiffID (스택 반영)")
	}

	// ── 데모 2: 순차 언팩 ──
	fmt.Println("\n--- 데모 2: 순차 언팩 모드 ---")
	ss1 := NewSnapshotStore()
	u1 := NewUnpacker(ss1, 2, 1, false) // fetch 2개, unpack 1개

	layers := []Layer{
		{Digest: "sha256:l0compressed", DiffID: diffIDs[0], Size: 50 * 1024 * 1024},
		{Digest: "sha256:l1compressed", DiffID: diffIDs[1], Size: 20 * 1024 * 1024},
		{Digest: "sha256:l2compressed", DiffID: diffIDs[2], Size: 5 * 1024 * 1024},
	}

	ctx := context.Background()
	start := time.Now()
	if err := u1.Unpack(ctx, layers); err != nil {
		fmt.Printf("  에러: %v\n", err)
	}
	seqDuration := time.Since(start)
	fmt.Printf("\n  순차 모드 소요 시간: %v\n", seqDuration)
	fmt.Printf("  언팩 완료 수: %d\n", u1.unpacked.Load())
	fmt.Printf("  스냅샷 수: %d\n", len(ss1.List()))

	// ── 데모 3: 병렬 언팩 ──
	fmt.Println("\n--- 데모 3: 병렬 언팩 모드 ---")
	ss2 := NewSnapshotStore()
	u2 := NewUnpacker(ss2, 3, 3, true) // fetch 3개, unpack 3개

	start = time.Now()
	if err := u2.Unpack(ctx, layers); err != nil {
		fmt.Printf("  에러: %v\n", err)
	}
	parDuration := time.Since(start)
	fmt.Printf("\n  병렬 모드 소요 시간: %v\n", parDuration)
	fmt.Printf("  언팩 완료 수: %d\n", u2.unpacked.Load())

	if parDuration < seqDuration {
		fmt.Printf("  [성능] 병렬 모드가 %.0f%% 더 빠름\n",
			float64(seqDuration-parDuration)/float64(seqDuration)*100)
	}

	// ── 데모 4: 중복 억제 ──
	fmt.Println("\n--- 데모 4: 중복 억제 (KeyedLocker) ---")
	kl := NewKeyedLocker()
	var wg sync.WaitGroup
	results := make([]string, 3)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "sn://overlayfs/sha256:aaa"
			start := time.Now()
			if err := kl.Lock(ctx, key); err != nil {
				results[id] = fmt.Sprintf("  goroutine %d: 에러 %v", id, err)
				return
			}
			elapsed := time.Since(start)
			if elapsed < time.Millisecond {
				results[id] = fmt.Sprintf("  goroutine %d: 즉시 획득 (첫 번째)", id)
				time.Sleep(100 * time.Millisecond)
				kl.Unlock(key)
			} else {
				results[id] = fmt.Sprintf("  goroutine %d: %v 대기 후 획득", id, elapsed.Truncate(time.Millisecond))
			}
		}(i)
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()
	for _, r := range results {
		fmt.Println(r)
	}

	// ── 데모 5: Limiter 동시성 제어 ──
	fmt.Println("\n--- 데모 5: Limiter 동시성 제어 ---")
	limiter := NewLimiter(2) // 최대 2개 동시

	var active atomic.Int32
	var maxActive atomic.Int32

	var wg2 sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg2.Add(1)
		go func(id int) {
			defer wg2.Done()
			limiter.Acquire(ctx)
			curr := active.Add(1)
			for {
				old := maxActive.Load()
				if curr <= old || maxActive.CompareAndSwap(old, curr) {
					break
				}
			}
			fmt.Printf("  작업 %d 시작 (현재 활성: %d)\n", id, curr)
			time.Sleep(50 * time.Millisecond)
			active.Add(-1)
			limiter.Release()
		}(i)
	}
	wg2.Wait()
	fmt.Printf("  최대 동시 활성 수: %d (제한: 2)\n", maxActive.Load())

	// ── 요약 ──
	fmt.Println("\n========================================")
	fmt.Println("요약: containerd 이미지 언팩 시스템")
	fmt.Println("========================================")
	fmt.Println("1. ChainID = SHA256(parent_chain + diffID) - 레이어 스택 식별")
	fmt.Println("2. topHalf(병렬) / bottomHalf(순차) 분리 패턴")
	fmt.Println("3. Fetch-Unpack 파이프라인으로 네트워크/디스크 I/O 중첩")
	fmt.Println("4. KeyedLocker로 동일 레이어 중복 언팩 방지")
	fmt.Println("5. Limiter로 Fetch/Unpack 동시성 독립 제어")
	fmt.Println()
	fmt.Println("핵심 설계:")
	fmt.Println("- Apply는 CPU/IO 바운드 → 병렬화 유리")
	fmt.Println("- Commit은 순서 보장 필요 → 순차 실행")
	fmt.Println("- rebase 기능으로 병렬 Apply + 순차 Commit 조합")
	_ = strings.NewReader("") // strings import 유지
}
