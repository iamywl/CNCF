package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ============================================================================
// etcd BatchTx PoC - 배치 트랜잭션 시뮬레이션
// ============================================================================
//
// etcd의 backend/batch_tx.go에서 영감을 받은 구현.
// etcd는 BoltDB 위에 BatchTx를 두어 여러 쓰기를 버퍼링하고,
// batchInterval(100ms) 또는 batchLimit(10000개) 초과 시 한 번에 커밋한다.
//
// 핵심 설계 원리:
// 1. pending 카운터로 미커밋 쓰기 추적
// 2. Unlock() 시 batchLimit 초과하면 자동 커밋
// 3. 주기적(batchInterval) 타이머로 강제 커밋
// 4. ReadTx는 커밋된 데이터 + 버퍼 데이터 모두 읽기 가능
//
// 참조: server/storage/backend/batch_tx.go, server/storage/backend/backend.go
// ============================================================================

const (
	batchLimit    = 100         // 커밋 트리거 임계값 (실제 etcd: 10000)
	batchInterval = 100 * time.Millisecond // 주기적 커밋 간격 (실제 etcd: 100ms)
)

// ---- 파일 기반 저장소 ----

// FileStore는 파일 기반의 단순 KV 저장소이다.
// etcd의 BoltDB 역할을 대신한다.
type FileStore struct {
	mu       sync.RWMutex
	path     string
	data     map[string]map[string]string // bucket -> key -> value
	commitN  int                           // 총 커밋 횟수
}

func NewFileStore(path string) *FileStore {
	return &FileStore{
		path: path,
		data: make(map[string]map[string]string),
	}
}

// Persist는 현재 데이터를 파일에 저장한다.
func (fs *FileStore) Persist() error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	f, err := os.Create(fs.path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(fs.data)
}

// Load는 파일에서 데이터를 복원한다.
func (fs *FileStore) Load() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f, err := os.Open(fs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(&fs.data)
}

// ---- 쓰기 버퍼 ----

// writeOp는 하나의 쓰기 연산을 나타낸다.
type writeOp struct {
	bucket string
	key    string
	value  string
	delete bool
}

// ---- BatchTx (etcd의 batchTx 모방) ----

// BatchTx는 쓰기를 버퍼링하고 한 번에 커밋하는 트랜잭션이다.
// etcd의 batchTx 구조체를 시뮬레이션한다.
//
// 실제 etcd 코드:
//   type batchTx struct {
//       sync.Mutex
//       tx      *bolt.Tx
//       backend *backend
//       pending int
//   }
type BatchTx struct {
	mu      sync.Mutex
	store   *FileStore
	buffer  []writeOp          // 미커밋 쓰기 버퍼
	pending int                // 미커밋 쓰기 수
	commits int                // 이 BatchTx의 커밋 횟수

	// 인메모리 캐시 (ReadTx가 버퍼 데이터도 읽을 수 있도록)
	cache   map[string]map[string]string
}

func NewBatchTx(store *FileStore) *BatchTx {
	cache := make(map[string]map[string]string)
	// 기존 저장소 데이터를 캐시에 복사
	store.mu.RLock()
	for b, kv := range store.data {
		cache[b] = make(map[string]string)
		for k, v := range kv {
			cache[b][k] = v
		}
	}
	store.mu.RUnlock()

	return &BatchTx{
		store:  store,
		buffer: make([]writeOp, 0, batchLimit),
		cache:  cache,
	}
}

// Put은 쓰기를 버퍼에 추가한다 (즉시 커밋하지 않음).
// etcd의 unsafePut()에 해당: bucket.Put(key, value) 후 t.pending++ 수행.
func (tx *BatchTx) Put(bucket, key, value string) {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	tx.buffer = append(tx.buffer, writeOp{bucket: bucket, key: key, value: value})
	tx.pending++

	// 인메모리 캐시 즉시 갱신 (ReadTx가 읽을 수 있도록)
	if _, ok := tx.cache[bucket]; !ok {
		tx.cache[bucket] = make(map[string]string)
	}
	tx.cache[bucket][key] = value
}

// Delete는 삭제를 버퍼에 추가한다.
func (tx *BatchTx) Delete(bucket, key string) {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	tx.buffer = append(tx.buffer, writeOp{bucket: bucket, key: key, delete: true})
	tx.pending++

	if bkt, ok := tx.cache[bucket]; ok {
		delete(bkt, key)
	}
}

// Unlock은 batchLimit 초과 시 자동 커밋한다.
// etcd의 batchTx.Unlock()과 동일:
//   func (t *batchTx) Unlock() {
//       if t.pending >= t.backend.batchLimit {
//           t.commit(false)
//       }
//       t.Mutex.Unlock()
//   }
func (tx *BatchTx) UnlockAndMaybeCommit() {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	if tx.pending >= batchLimit {
		tx.commitLocked()
	}
}

// Commit은 버퍼의 모든 쓰기를 저장소에 반영한다.
func (tx *BatchTx) Commit() {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.commitLocked()
}

func (tx *BatchTx) commitLocked() {
	if tx.pending == 0 {
		return
	}

	// 버퍼의 쓰기를 저장소에 반영
	tx.store.mu.Lock()
	for _, op := range tx.buffer {
		if op.delete {
			if bkt, ok := tx.store.data[op.bucket]; ok {
				delete(bkt, op.key)
			}
		} else {
			if _, ok := tx.store.data[op.bucket]; !ok {
				tx.store.data[op.bucket] = make(map[string]string)
			}
			tx.store.data[op.bucket][op.key] = op.value
		}
	}
	tx.store.commitN++
	tx.store.mu.Unlock()

	// 파일에 영속화
	tx.store.Persist()

	tx.commits++
	tx.pending = 0
	tx.buffer = tx.buffer[:0]
}

// Pending은 미커밋 쓰기 수를 반환한다.
func (tx *BatchTx) Pending() int {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.pending
}

// ---- ReadTx (커밋된 + 버퍼 데이터 읽기) ----

// ReadTx는 커밋된 저장소와 BatchTx의 인메모리 캐시를 모두 읽는다.
// etcd의 ConcurrentReadTx는 readBuffer + txBuffer를 merge하여 읽기를 제공한다.
type ReadTx struct {
	store *FileStore
	btx   *BatchTx
}

func NewReadTx(store *FileStore, btx *BatchTx) *ReadTx {
	return &ReadTx{store: store, btx: btx}
}

// Get은 키를 읽는다. 버퍼(캐시) 우선, 없으면 저장소에서 읽는다.
func (rtx *ReadTx) Get(bucket, key string) (string, bool) {
	// 1. BatchTx 캐시에서 먼저 검색 (미커밋 데이터 포함)
	rtx.btx.mu.Lock()
	if bkt, ok := rtx.btx.cache[bucket]; ok {
		if val, ok := bkt[key]; ok {
			rtx.btx.mu.Unlock()
			return val, true
		}
	}
	rtx.btx.mu.Unlock()

	// 2. 커밋된 저장소에서 검색
	rtx.store.mu.RLock()
	defer rtx.store.mu.RUnlock()
	if bkt, ok := rtx.store.data[bucket]; ok {
		if val, ok := bkt[key]; ok {
			return val, true
		}
	}
	return "", false
}

// Range는 버킷의 모든 키를 읽는다 (캐시 + 저장소 merge).
func (rtx *ReadTx) Range(bucket string) map[string]string {
	result := make(map[string]string)

	// 커밋된 저장소에서 읽기
	rtx.store.mu.RLock()
	if bkt, ok := rtx.store.data[bucket]; ok {
		for k, v := range bkt {
			result[k] = v
		}
	}
	rtx.store.mu.RUnlock()

	// 캐시(미커밋 포함)로 덮어쓰기
	rtx.btx.mu.Lock()
	if bkt, ok := rtx.btx.cache[bucket]; ok {
		for k, v := range bkt {
			result[k] = v
		}
	}
	rtx.btx.mu.Unlock()

	return result
}

// ---- Backend (주기적 커밋 루프) ----

// Backend는 etcd의 backend 구조체를 시뮬레이션한다.
// 주기적으로 batchInterval마다 커밋을 강제한다.
type Backend struct {
	store   *FileStore
	btx     *BatchTx
	stopC   chan struct{}
	doneC   chan struct{}
}

func NewBackend(store *FileStore) *Backend {
	btx := NewBatchTx(store)
	b := &Backend{
		store: store,
		btx:   btx,
		stopC: make(chan struct{}),
		doneC: make(chan struct{}),
	}
	go b.run()
	return b
}

// run은 주기적 커밋 루프이다.
// etcd의 backend.run():
//   for {
//       select {
//       case <-t.C:
//           s.batchTx.Commit()
//       case <-s.stopc:
//           return
//       }
//   }
func (b *Backend) run() {
	defer close(b.doneC)
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.btx.Commit()
		case <-b.stopC:
			// 종료 전 마지막 커밋
			b.btx.Commit()
			return
		}
	}
}

func (b *Backend) Close() {
	close(b.stopC)
	<-b.doneC
}

// ============================================================================
// 데모
// ============================================================================

func main() {
	fmt.Println("=== etcd BatchTx (배치 트랜잭션) PoC ===")
	fmt.Println()

	tmpDir, _ := os.MkdirTemp("", "batchtx-poc-*")
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "store.json")

	// ---- 1. 배치 커밋 vs 즉시 커밋 비교 ----
	fmt.Println("--- 1. 배치 커밋 성능 비교 ---")

	// 즉시 커밋 (매 쓰기마다 커밋)
	store1 := NewFileStore(filepath.Join(tmpDir, "immediate.json"))
	btx1 := NewBatchTx(store1)
	start := time.Now()
	for i := 0; i < 1000; i++ {
		btx1.Put("data", fmt.Sprintf("key-%04d", i), fmt.Sprintf("value-%d", i))
		btx1.Commit() // 매번 커밋
	}
	immediateDur := time.Since(start)
	fmt.Printf("  즉시 커밋 (1000회 커밋): %v, 커밋 횟수: %d\n", immediateDur, store1.commitN)

	// 배치 커밋 (batchLimit 도달 시 커밋)
	store2 := NewFileStore(filepath.Join(tmpDir, "batch.json"))
	btx2 := NewBatchTx(store2)
	start = time.Now()
	for i := 0; i < 1000; i++ {
		btx2.Put("data", fmt.Sprintf("key-%04d", i), fmt.Sprintf("value-%d", i))
		if btx2.Pending() >= batchLimit {
			btx2.Commit()
		}
	}
	btx2.Commit() // 잔여 커밋
	batchDur := time.Since(start)
	fmt.Printf("  배치 커밋 (limit=%d):   %v, 커밋 횟수: %d\n", batchLimit, batchDur, store2.commitN)
	fmt.Printf("  성능 향상: %.1fx 빠름\n", float64(immediateDur)/float64(batchDur))
	fmt.Println()

	// ---- 2. batchLimit 초과 시 자동 커밋 ----
	fmt.Println("--- 2. batchLimit 초과 시 자동 커밋 ---")
	store3 := NewFileStore(filepath.Join(tmpDir, "autobatch.json"))
	btx3 := NewBatchTx(store3)

	fmt.Printf("  batchLimit = %d\n", batchLimit)
	for i := 0; i < 250; i++ {
		btx3.Put("users", fmt.Sprintf("user-%04d", i), fmt.Sprintf("data-%d", i))
		if (i+1)%batchLimit == 0 {
			btx3.UnlockAndMaybeCommit()
			fmt.Printf("  %d개 쓰기 후 → pending=%d, 커밋 횟수=%d\n",
				i+1, btx3.Pending(), store3.commitN)
		}
	}
	btx3.Commit()
	fmt.Printf("  최종: 총 250개 쓰기, 커밋 횟수=%d\n", store3.commitN)
	fmt.Println()

	// ---- 3. ReadTx - 커밋되지 않은 데이터도 읽기 ----
	fmt.Println("--- 3. ReadTx: 미커밋 데이터 읽기 ---")
	store4 := NewFileStore(dbPath)
	btx4 := NewBatchTx(store4)

	// 일부 데이터 커밋
	btx4.Put("config", "setting-a", "committed-value")
	btx4.Commit()

	// 미커밋 데이터 추가
	btx4.Put("config", "setting-b", "uncommitted-value")
	btx4.Put("config", "setting-a", "updated-value")

	rtx := NewReadTx(store4, btx4)

	// 커밋된 데이터 읽기
	val, ok := rtx.Get("config", "setting-a")
	fmt.Printf("  setting-a (캐시에서 갱신): %s (found=%v)\n", val, ok)

	// 미커밋 데이터 읽기
	val, ok = rtx.Get("config", "setting-b")
	fmt.Printf("  setting-b (미커밋 버퍼):   %s (found=%v)\n", val, ok)

	// Range 읽기
	all := rtx.Range("config")
	fmt.Printf("  Range('config'): %v\n", all)
	fmt.Println()

	// ---- 4. 주기적 커밋 (batchInterval) ----
	fmt.Println("--- 4. 주기적 커밋 (batchInterval) ---")
	store5 := NewFileStore(filepath.Join(tmpDir, "periodic.json"))
	backend := NewBackend(store5)

	fmt.Printf("  batchInterval = %v\n", batchInterval)

	// 쓰기 (batchLimit 미만이므로 자동 커밋 안 됨)
	for i := 0; i < 50; i++ {
		backend.btx.Put("metrics", fmt.Sprintf("m-%02d", i), "1.0")
	}
	fmt.Printf("  50개 쓰기 직후 → 저장소 커밋 횟수: %d, pending: %d\n",
		store5.commitN, backend.btx.Pending())

	// batchInterval 대기
	time.Sleep(batchInterval + 20*time.Millisecond)
	fmt.Printf("  %v 후 → 저장소 커밋 횟수: %d (타이머에 의해 커밋됨)\n",
		batchInterval, store5.commitN)

	backend.Close()
	fmt.Printf("  Backend 종료 후 → 최종 커밋 횟수: %d\n", store5.commitN)
	fmt.Println()

	// ---- 5. 파일 영속화 검증 ----
	fmt.Println("--- 5. 파일 영속화 및 복원 검증 ---")
	store6 := NewFileStore(dbPath)
	store6.Load()
	fmt.Printf("  파일에서 복원된 데이터:\n")
	for bucket, kv := range store6.data {
		fmt.Printf("    버킷[%s]: %d개 키\n", bucket, len(kv))
		for k, v := range kv {
			fmt.Printf("      %s = %s\n", k, v)
		}
	}
	fmt.Println()

	// ---- 6. 동시 쓰기 + 배치 커밋 ----
	fmt.Println("--- 6. 동시 쓰기와 배치 커밋 ---")
	store7 := NewFileStore(filepath.Join(tmpDir, "concurrent.json"))
	btx7 := NewBatchTx(store7)

	var wg sync.WaitGroup
	for g := 0; g < 5; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				btx7.Put("concurrent",
					fmt.Sprintf("g%d-key-%04d", gid, i),
					fmt.Sprintf("val-%d", i))
			}
		}(g)
	}
	wg.Wait()
	btx7.Commit()

	rtx7 := NewReadTx(store7, btx7)
	all7 := rtx7.Range("concurrent")
	fmt.Printf("  5 goroutine × 200 쓰기 = 1000개 예상\n")
	fmt.Printf("  실제 저장된 키 수: %d\n", len(all7))
	fmt.Printf("  커밋 횟수: %d\n", store7.commitN)
	fmt.Println()

	fmt.Println("=== 핵심 정리 ===")
	fmt.Println("1. BatchTx는 쓰기를 버퍼에 모아 한 번에 커밋하여 I/O를 줄인다")
	fmt.Println("2. batchLimit 초과 시 Unlock()에서 자동 커밋 (etcd: 10000)")
	fmt.Println("3. batchInterval 타이머로 주기적 강제 커밋 (etcd: 100ms)")
	fmt.Println("4. ReadTx는 인메모리 캐시를 통해 미커밋 데이터도 읽을 수 있다")
	fmt.Println("5. 커밋 시 파일(BoltDB)에 영속화하여 crash recovery 보장")
}
