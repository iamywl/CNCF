package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// containerd BoltDB 메타데이터 스토어 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - core/metadata/db.go           : DB 구조체, View/Update, wlock, mutationCallbacks
//   - core/metadata/buckets.go      : 버킷 스키마 (v1/<namespace>/<object>/<key>)
//   - plugins/metadata/plugin.go    : BoltDB 초기화, BoltConfig, NewDB
//   - core/metadata/gc.go           : ResourceType, GC용 scanRoots/references/scanAll
//
// BoltDB 메타데이터 설계 핵심:
//   1. 계층적 버킷 구조: v1/{namespace}/{object_type}/{key} → {field}
//   2. 네임스페이스 격리: context에서 namespace를 추출하여 데이터 분리
//   3. View(읽기) / Update(쓰기) 트랜잭션으로 동시성 제어
//   4. wlock (RWMutex): GC 진행 중 쓰기 차단, 읽기는 허용
//   5. mutationCallbacks: Update 후 GC 스케줄러에 변경 알림
//   6. dirty 카운터: 삭제 발생 시 원자적으로 증가, GC 필요성 판단

// =============================================================================
// 1. 네임스페이스 관리 — context 기반 격리
// =============================================================================

// 실제: pkg/namespaces/context.go의 WithNamespace, NamespaceRequired
// context.WithValue로 네임스페이스를 전파하여 모든 API 호출에서 격리 보장

type contextKey string

const namespaceKey contextKey = "containerd.namespace"

// WithNamespace는 context에 네임스페이스를 설정한다.
// 실제: pkg/namespaces/context.go — WithNamespace(ctx, ns)
func WithNamespace(ctx context.Context, ns string) context.Context {
	return context.WithValue(ctx, namespaceKey, ns)
}

// NamespaceRequired는 context에서 네임스페이스를 추출한다.
// 네임스페이스가 없으면 에러를 반환한다.
// 실제: pkg/namespaces/context.go — NamespaceRequired(ctx)
func NamespaceRequired(ctx context.Context) (string, error) {
	ns, ok := ctx.Value(namespaceKey).(string)
	if !ok || ns == "" {
		return "", fmt.Errorf("namespace is required")
	}
	return ns, nil
}

// =============================================================================
// 2. 버킷 구조 — BoltDB의 계층적 KV 스토어 시뮬레이션
// =============================================================================

// 실제 BoltDB 스키마 (core/metadata/buckets.go 주석에서 발췌):
//
//   v1                                         - 스키마 버전 버킷
//     ├── version : <varint>                   - DB 버전
//     └── *namespace*
//         ├── labels                           - 네임스페이스 레이블
//         ├── images/*image name*              - 이미지 메타데이터
//         ├── containers/*container id*        - 컨테이너 메타데이터
//         ├── snapshots/*snapshotter*/*key*    - 스냅샷 메타데이터
//         ├── content/blob/*digest*            - 콘텐트 블롭 메타데이터
//         └── leases/*lease id*                - 리스 메타데이터

// Bucket은 BoltDB 버킷을 메모리로 시뮬레이션한다.
// 실제 BoltDB는 B+ 트리 기반이지만, 여기서는 중첩 map으로 구현
type Bucket struct {
	// data는 키-값 쌍을 저장 (BoltDB의 단말 데이터)
	data map[string]string
	// children은 하위 버킷을 저장 (BoltDB의 중첩 버킷)
	children map[string]*Bucket
}

func newBucket() *Bucket {
	return &Bucket{
		data:     make(map[string]string),
		children: make(map[string]*Bucket),
	}
}

// CreateBucketIfNotExists는 하위 버킷을 생성하거나 기존 버킷을 반환한다.
// 실제: bolt.Bucket.CreateBucketIfNotExists(key)
func (b *Bucket) CreateBucketIfNotExists(name string) *Bucket {
	if child, ok := b.children[name]; ok {
		return child
	}
	child := newBucket()
	b.children[name] = child
	return child
}

// GetBucket은 하위 버킷을 반환한다 (없으면 nil).
// 실제: bolt.Bucket.Bucket(key)
func (b *Bucket) GetBucket(name string) *Bucket {
	return b.children[name]
}

// Put은 키-값 쌍을 저장한다.
// 실제: bolt.Bucket.Put(key, value)
func (b *Bucket) Put(key, value string) {
	b.data[key] = value
}

// Get은 키에 해당하는 값을 반환한다.
// 실제: bolt.Bucket.Get(key)
func (b *Bucket) Get(key string) (string, bool) {
	v, ok := b.data[key]
	return v, ok
}

// Delete는 키를 삭제한다.
// 실제: bolt.Bucket.Delete(key)
func (b *Bucket) Delete(key string) {
	delete(b.data, key)
}

// DeleteBucket은 하위 버킷을 삭제한다.
// 실제: bolt.Bucket.DeleteBucket(key)
func (b *Bucket) DeleteBucket(name string) error {
	if _, ok := b.children[name]; !ok {
		return fmt.Errorf("bucket not found: %s", name)
	}
	delete(b.children, name)
	return nil
}

// ForEach는 모든 키-값 쌍을 순회한다.
// 실제: bolt.Bucket.ForEach(fn)
func (b *Bucket) ForEach(fn func(key, value string) error) error {
	// 데이터 순회
	for k, v := range b.data {
		if err := fn(k, v); err != nil {
			return err
		}
	}
	// 하위 버킷도 키로 순회 (값은 빈 문자열)
	for k := range b.children {
		if err := fn(k, ""); err != nil {
			return err
		}
	}
	return nil
}

// =============================================================================
// 3. 트랜잭션 — View(읽기) / Update(쓰기)
// =============================================================================

// Tx는 BoltDB 트랜잭션을 시뮬레이션한다.
// 실제: bolt.Tx — Bucket(), CreateBucketIfNotExists() 등 제공
type Tx struct {
	root     *Bucket
	writable bool
}

// Bucket은 트랜잭션의 루트 버킷을 반환한다.
func (tx *Tx) Bucket(name string) *Bucket {
	return tx.root.GetBucket(name)
}

// CreateBucketIfNotExists는 루트에 버킷을 생성한다.
func (tx *Tx) CreateBucketIfNotExists(name string) *Bucket {
	return tx.root.CreateBucketIfNotExists(name)
}

// =============================================================================
// 4. DB — 메타데이터 데이터베이스 핵심
// =============================================================================

// DB는 containerd의 메타데이터 데이터베이스를 시뮬레이션한다.
// 실제: core/metadata/db.go — DB 구조체
//
// 핵심 필드:
//   - wlock: GC 중 쓰기 차단용 RWMutex
//   - dirty: 삭제 횟수 추적 (atomic)
//   - mutationCallbacks: 변경 후 콜백 (GC 스케줄러 알림)
type DB struct {
	root *Bucket // BoltDB의 루트 (메모리 시뮬레이션)

	// wlock은 GC 진행 중 쓰기를 차단한다.
	// 실제: core/metadata/db.go — "wlock is used to protect access to the
	// data structures during garbage collection"
	// GarbageCollect()에서 wlock.Lock()을 잡고,
	// Update()에서 wlock.RLock()을 잡아 GC와 쓰기를 상호 배제
	wlock sync.RWMutex

	// dirty는 삭제 발생 횟수를 추적한다 (GC 필요성 판단).
	// 실제: core/metadata/db.go — dirty atomic.Uint32
	dirty atomic.Uint32

	// mutationCallbacks는 Update 완료 후 호출되는 콜백 목록이다.
	// 실제: core/metadata/db.go — mutationCallbacks []func(bool)
	// GC 스케줄러가 RegisterMutationCallback으로 등록한다.
	mutationCallbacks []func(bool)

	mu sync.Mutex // mutationCallbacks 보호용
}

// NewDB는 새 메타데이터 데이터베이스를 생성한다.
// 실제: core/metadata/db.go — NewDB(db, cs, ss, opts...)
func NewDB() *DB {
	db := &DB{
		root: newBucket(),
	}
	// 스키마 버전 버킷 초기화
	v1 := db.root.CreateBucketIfNotExists("v1")
	v1.Put("version", "4") // dbVersion = 4
	return db
}

// View는 읽기 전용 트랜잭션을 실행한다.
// 실제: core/metadata/db.go — func (m *DB) View(fn func(*bolt.Tx) error) error
// BoltDB의 View는 공유 잠금으로 동시 읽기를 허용한다.
func (db *DB) View(fn func(tx *Tx) error) error {
	tx := &Tx{root: db.root, writable: false}
	return fn(tx)
}

// Update는 쓰기 트랜잭션을 실행한다.
// 실제: core/metadata/db.go — func (m *DB) Update(fn func(*bolt.Tx) error) error
//
// 핵심 동작:
//   1. wlock.RLock() — GC가 진행 중이면 대기
//   2. 트랜잭션 함수 실행
//   3. 성공 시 mutationCallbacks 호출
func (db *DB) Update(fn func(tx *Tx) error) error {
	db.wlock.RLock()
	defer db.wlock.RUnlock()

	tx := &Tx{root: db.root, writable: true}
	err := fn(tx)

	if err == nil {
		// 변경 후 콜백 호출 — GC 스케줄러에 알림
		// 실제: dirty := m.dirty.Load() > 0; for _, fn := range m.mutationCallbacks { fn(dirty) }
		dirty := db.dirty.Load() > 0
		db.mu.Lock()
		callbacks := make([]func(bool), len(db.mutationCallbacks))
		copy(callbacks, db.mutationCallbacks)
		db.mu.Unlock()

		for _, cb := range callbacks {
			cb(dirty)
		}
	}

	return err
}

// RegisterMutationCallback은 변경 후 호출될 콜백을 등록한다.
// 실제: core/metadata/db.go — RegisterMutationCallback(fn func(bool))
// GC 스케줄러가 이 콜백을 통해 mutation 이벤트를 수신한다.
func (db *DB) RegisterMutationCallback(fn func(bool)) {
	db.mu.Lock()
	db.mutationCallbacks = append(db.mutationCallbacks, fn)
	db.mu.Unlock()
}

// MarkDirty는 삭제가 발생했음을 표시한다.
// 실제: core/metadata/leases.go — lm.db.dirty.Add(1)
func (db *DB) MarkDirty() {
	db.dirty.Add(1)
}

// =============================================================================
// 5. 네임스페이스별 CRUD 작업
// =============================================================================

// getBucket은 트랜잭션에서 계층적 경로로 버킷을 탐색한다.
// 실제: core/metadata/buckets.go — getBucket(tx, keys...)
func getBucket(tx *Tx, keys ...string) *Bucket {
	if len(keys) == 0 {
		return nil
	}
	bkt := tx.Bucket(keys[0])
	for _, key := range keys[1:] {
		if bkt == nil {
			return nil
		}
		bkt = bkt.GetBucket(key)
	}
	return bkt
}

// createBucketIfNotExists는 트랜잭션에서 계층적 경로의 버킷을 생성한다.
// 실제: core/metadata/buckets.go — createBucketIfNotExists(tx, keys...)
func createBucketIfNotExists(tx *Tx, keys ...string) *Bucket {
	if len(keys) == 0 {
		return nil
	}
	bkt := tx.CreateBucketIfNotExists(keys[0])
	for _, key := range keys[1:] {
		bkt = bkt.CreateBucketIfNotExists(key)
	}
	return bkt
}

// ImageStore는 이미지 메타데이터 CRUD를 시뮬레이션한다.
// 실제: core/metadata/images.go — imageStore 구조체
type ImageStore struct {
	db *DB
}

// Image는 이미지 메타데이터를 나타낸다.
type Image struct {
	Name   string
	Target string // 대상 콘텐트 digest
	Labels map[string]string
}

// Create는 네임스페이스 내에 이미지를 생성한다.
// 실제 경로: v1/{namespace}/images/{image_name}/
func (s *ImageStore) Create(ctx context.Context, img Image) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	return s.db.Update(func(tx *Tx) error {
		// v1/{namespace}/images 버킷 생성
		bkt := createBucketIfNotExists(tx, "v1", ns, "images")
		// 이미지 이름 버킷 생성
		imgBkt := bkt.CreateBucketIfNotExists(img.Name)
		imgBkt.Put("createdat", time.Now().Format(time.RFC3339))

		// target 버킷 — digest 저장
		// 실제: bucketKeyTarget 하위에 digest, mediatype, size 저장
		targetBkt := imgBkt.CreateBucketIfNotExists("target")
		targetBkt.Put("digest", img.Target)

		// 레이블 저장
		if len(img.Labels) > 0 {
			lblBkt := imgBkt.CreateBucketIfNotExists("labels")
			for k, v := range img.Labels {
				lblBkt.Put(k, v)
			}
		}

		return nil
	})
}

// Get은 네임스페이스 내에서 이미지를 조회한다.
func (s *ImageStore) Get(ctx context.Context, name string) (*Image, error) {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	var img *Image
	err = s.db.View(func(tx *Tx) error {
		bkt := getBucket(tx, "v1", ns, "images", name)
		if bkt == nil {
			return fmt.Errorf("image %q not found", name)
		}

		img = &Image{Name: name}
		if targetBkt := bkt.GetBucket("target"); targetBkt != nil {
			if d, ok := targetBkt.Get("digest"); ok {
				img.Target = d
			}
		}

		img.Labels = make(map[string]string)
		if lblBkt := bkt.GetBucket("labels"); lblBkt != nil {
			lblBkt.ForEach(func(k, v string) error {
				img.Labels[k] = v
				return nil
			})
		}

		return nil
	})

	return img, err
}

// Delete는 네임스페이스 내에서 이미지를 삭제한다.
func (s *ImageStore) Delete(ctx context.Context, name string) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	return s.db.Update(func(tx *Tx) error {
		bkt := getBucket(tx, "v1", ns, "images")
		if bkt == nil {
			return fmt.Errorf("images bucket not found")
		}
		if err := bkt.DeleteBucket(name); err != nil {
			return err
		}
		// 삭제 시 dirty 마킹 — GC에게 정리 필요 알림
		// 실제: leases.go의 Delete에서 lm.db.dirty.Add(1)
		s.db.MarkDirty()
		return nil
	})
}

// List는 네임스페이스 내의 모든 이미지를 나열한다.
func (s *ImageStore) List(ctx context.Context) ([]string, error) {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	var names []string
	err = s.db.View(func(tx *Tx) error {
		bkt := getBucket(tx, "v1", ns, "images")
		if bkt == nil {
			return nil
		}
		for name := range bkt.children {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil
	})

	return names, err
}

// =============================================================================
// 6. GC 중 쓰기 차단 시뮬레이션
// =============================================================================

// SimulateGC는 GC 동작을 시뮬레이션한다.
// 실제: core/metadata/db.go — GarbageCollect(ctx)
//
// GC 프로세스:
//   1. wlock.Lock() — 모든 Update 차단
//   2. Mark: 사용 중인 리소스 식별
//   3. Sweep: 미사용 리소스 삭제
//   4. dirty 카운터 리셋
//   5. wlock.Unlock() — Update 재허용
func SimulateGC(db *DB) {
	fmt.Println("\n[GC] wlock.Lock() 획득 — 쓰기 트랜잭션 차단")
	db.wlock.Lock()

	fmt.Printf("[GC] dirty 카운터: %d\n", db.dirty.Load())
	fmt.Println("[GC] Mark 단계: 사용 중인 리소스 식별...")
	time.Sleep(100 * time.Millisecond) // Mark 시뮬레이션

	fmt.Println("[GC] Sweep 단계: 미사용 리소스 제거...")
	time.Sleep(50 * time.Millisecond) // Sweep 시뮬레이션

	// dirty 리셋
	// 실제: m.dirty.Store(0) — GC 완료 후 리셋
	db.dirty.Store(0)
	fmt.Printf("[GC] dirty 카운터 리셋: %d\n", db.dirty.Load())

	db.wlock.Unlock()
	fmt.Println("[GC] wlock.Unlock() — 쓰기 트랜잭션 재허용")
}

// =============================================================================
// 7. 메인 — 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== containerd BoltDB 메타데이터 스토어 시뮬레이션 ===")
	fmt.Println()

	// --- 데모 1: DB 초기화 및 버킷 구조 ---
	fmt.Println("--- 데모 1: DB 초기화 및 버킷 스키마 ---")
	db := NewDB()
	fmt.Println("DB 생성 완료 (스키마 v1, 버전 4)")
	fmt.Println()

	// 네임스페이스 확인
	ctx := context.Background()
	_, err := NamespaceRequired(ctx)
	fmt.Printf("네임스페이스 없는 context: %v\n", err)

	ctxDefault := WithNamespace(ctx, "default")
	ns, _ := NamespaceRequired(ctxDefault)
	fmt.Printf("네임스페이스 설정 후: %q\n", ns)
	fmt.Println()

	// --- 데모 2: 네임스페이스 격리 ---
	fmt.Println("--- 데모 2: 네임스페이스 격리된 이미지 CRUD ---")
	store := &ImageStore{db: db}

	// 네임스페이스 "default"에 이미지 생성
	ctxDefault = WithNamespace(ctx, "default")
	err = store.Create(ctxDefault, Image{
		Name:   "docker.io/library/nginx:latest",
		Target: "sha256:abc123",
		Labels: map[string]string{
			"containerd.io/gc.ref.content": "sha256:abc123",
		},
	})
	fmt.Printf("default NS 이미지 생성: err=%v\n", err)

	err = store.Create(ctxDefault, Image{
		Name:   "docker.io/library/redis:7",
		Target: "sha256:def456",
	})
	fmt.Printf("default NS 이미지 생성: err=%v\n", err)

	// 네임스페이스 "production"에 이미지 생성
	ctxProd := WithNamespace(ctx, "production")
	err = store.Create(ctxProd, Image{
		Name:   "gcr.io/myapp:v1.0",
		Target: "sha256:prod789",
	})
	fmt.Printf("production NS 이미지 생성: err=%v\n", err)

	// 각 네임스페이스에서 이미지 조회
	defaultImages, _ := store.List(ctxDefault)
	prodImages, _ := store.List(ctxProd)
	fmt.Printf("\ndefault NS 이미지: %v\n", defaultImages)
	fmt.Printf("production NS 이미지: %v\n", prodImages)
	fmt.Println("=> 네임스페이스 간 이미지가 격리됨")
	fmt.Println()

	// --- 데모 3: View/Update 트랜잭션 ---
	fmt.Println("--- 데모 3: View(읽기) / Update(쓰기) 트랜잭션 ---")
	img, err := store.Get(ctxDefault, "docker.io/library/nginx:latest")
	if err == nil {
		fmt.Printf("View — 이미지 조회: name=%s, target=%s\n", img.Name, img.Target)
		fmt.Printf("         labels=%v\n", img.Labels)
	}

	_, err = store.Get(ctxProd, "docker.io/library/nginx:latest")
	fmt.Printf("View — production에서 default 이미지 조회: %v\n", err)
	fmt.Println("=> production NS에서 default NS의 이미지에 접근 불가")
	fmt.Println()

	// --- 데모 4: 버킷 경로 탐색 ---
	fmt.Println("--- 데모 4: 버킷 경로 구조 (BoltDB 스키마) ---")
	printBucketTree(db.root, "", 0)
	fmt.Println()

	// --- 데모 5: mutationCallback 등록 및 동작 ---
	fmt.Println("--- 데모 5: mutationCallback — GC 스케줄러 연동 ---")
	mutationCount := 0
	db.RegisterMutationCallback(func(dirty bool) {
		mutationCount++
		fmt.Printf("  [콜백] mutation #%d, dirty=%v\n", mutationCount, dirty)
	})

	fmt.Println("이미지 삭제 (Update 트랜잭션):")
	err = store.Delete(ctxDefault, "docker.io/library/redis:7")
	fmt.Printf("  삭제 결과: err=%v, dirty=%d\n", err, db.dirty.Load())

	fmt.Println("이미지 추가 (Update 트랜잭션):")
	err = store.Create(ctxDefault, Image{
		Name:   "docker.io/library/alpine:3.18",
		Target: "sha256:alpine111",
	})
	fmt.Printf("  생성 결과: err=%v, dirty=%d (삭제 누적)\n", err, db.dirty.Load())
	fmt.Println()

	// --- 데모 6: GC 중 쓰기 차단 ---
	fmt.Println("--- 데모 6: GC wlock — 쓰기 차단 시뮬레이션 ---")
	var wg sync.WaitGroup

	// GC 시작 (wlock.Lock 획득)
	wg.Add(1)
	go func() {
		defer wg.Done()
		SimulateGC(db)
	}()

	// 약간의 지연 후 쓰기 시도 — GC가 끝날 때까지 대기
	time.Sleep(10 * time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		fmt.Println("\n[Writer] Update 시도 (wlock.RLock 대기)...")
		err := db.Update(func(tx *Tx) error {
			bkt := createBucketIfNotExists(tx, "v1", "default", "containers")
			bkt.CreateBucketIfNotExists("my-container-1")
			return nil
		})
		elapsed := time.Since(start)
		fmt.Printf("[Writer] Update 완료: err=%v, 대기시간=%v\n", err, elapsed.Round(time.Millisecond))
	}()

	// 읽기는 GC 중에도 가능
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		fmt.Println("[Reader] View 시도 (읽기는 GC 중에도 가능)...")
		err := db.View(func(tx *Tx) error {
			bkt := getBucket(tx, "v1", "default", "images")
			if bkt != nil {
				count := len(bkt.children)
				fmt.Printf("[Reader] default NS 이미지 수: %d\n", count)
			}
			return nil
		})
		if err != nil {
			fmt.Printf("[Reader] View 에러: %v\n", err)
		}
	}()

	wg.Wait()
	fmt.Println()

	// --- 데모 7: 최종 버킷 구조 ---
	fmt.Println("--- 데모 7: 최종 버킷 구조 ---")
	printBucketTree(db.root, "", 0)
}

// printBucketTree는 버킷 구조를 트리 형태로 출력한다.
func printBucketTree(b *Bucket, prefix string, depth int) {
	indent := strings.Repeat("  ", depth)

	// 데이터 출력
	keys := make([]string, 0, len(b.data))
	for k := range b.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := b.data[k]
		if len(v) > 30 {
			v = v[:30] + "..."
		}
		fmt.Printf("%s%s├── %s = %q\n", prefix, indent, k, v)
	}

	// 하위 버킷 출력
	childNames := make([]string, 0, len(b.children))
	for name := range b.children {
		childNames = append(childNames, name)
	}
	sort.Strings(childNames)

	for i, name := range childNames {
		connector := "├── "
		childPrefix := "│   "
		if i == len(childNames)-1 && len(keys) == 0 {
			connector = "└── "
			childPrefix = "    "
		}
		fmt.Printf("%s%s%s%s/\n", prefix, indent, connector, name)
		printBucketTree(b.children[name], prefix+indent+childPrefix, 0)
	}
}
