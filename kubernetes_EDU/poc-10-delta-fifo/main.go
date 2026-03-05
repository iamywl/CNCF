package main

import (
	"fmt"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes DeltaFIFO 큐 시뮬레이션
//
// 실제 구현 참조:
//   - staging/src/k8s.io/client-go/tools/cache/delta_fifo.go
//
// DeltaFIFO는 client-go의 핵심 데이터 구조로, Reflector가 생산자이고
// Informer의 processLoop가 소비자인 생산자-소비자 큐이다.
//
// 핵심 특성:
//   1. 키별 델타 축적: 같은 키의 변경사항이 Deltas 슬라이스에 누적된다
//   2. FIFO 순서: 키는 처음 등장한 순서대로 처리된다 (queue 슬라이스)
//   3. 중복 제거: 연속된 동일 삭제 이벤트는 병합된다 (dedupDeltas)
//   4. Replace: re-list 시나리오에서 전체 상태를 동기화한다
//   5. Pop은 블로킹: 큐가 비어있으면 아이템이 올 때까지 대기한다
// =============================================================================

// --- Delta 타입 ---

// DeltaType은 변경의 종류를 나타낸다.
// 실제 cache.DeltaType에 대응한다 (delta_fifo.go:179).
type DeltaType string

const (
	Added    DeltaType = "Added"
	Updated  DeltaType = "Updated"
	Deleted  DeltaType = "Deleted"
	Replaced DeltaType = "Replaced"
	Sync     DeltaType = "Sync"
)

// Delta는 하나의 변경 이벤트를 나타낸다.
// 실제 cache.Delta(delta_fifo.go:216)에 대응한다.
type Delta struct {
	Type   DeltaType
	Object interface{}
}

// Deltas는 하나의 키에 대한 변경 이벤트 목록이다.
// 가장 오래된 이벤트가 index 0, 가장 최신이 마지막이다.
type Deltas []Delta

func (d Deltas) Oldest() *Delta {
	if len(d) > 0 {
		return &d[0]
	}
	return nil
}

func (d Deltas) Newest() *Delta {
	if n := len(d); n > 0 {
		return &d[n-1]
	}
	return nil
}

// --- Pod (테스트용 객체) ---

type Pod struct {
	Namespace string
	Name      string
	Image     string
	Phase     string
}

func (p *Pod) Key() string {
	return p.Namespace + "/" + p.Name
}

func (p *Pod) String() string {
	return fmt.Sprintf("Pod{%s/%s, image=%s, phase=%s}", p.Namespace, p.Name, p.Image, p.Phase)
}

// --- DeletedFinalStateUnknown ---

// DeletedFinalStateUnknown은 Watch 이벤트를 놓쳤을 때 사용되는 tombstone 객체이다.
// re-list(Replace)에서 기존 객체가 사라진 경우, 마지막으로 알려진 상태와 함께 삭제 이벤트를 발행한다.
// 실제 cache.DeletedFinalStateUnknown(delta_fifo.go:797)에 대응한다.
type DeletedFinalStateUnknown struct {
	Key string
	Obj interface{}
}

func (d DeletedFinalStateUnknown) String() string {
	return fmt.Sprintf("DeletedFinalStateUnknown{key=%s}", d.Key)
}

// --- KeyFunc ---

// KeyFunc은 객체에서 키를 추출하는 함수이다.
type KeyFunc func(obj interface{}) (string, error)

// PodKeyFunc은 Pod 객체의 키를 추출한다.
func PodKeyFunc(obj interface{}) (string, error) {
	switch o := obj.(type) {
	case *Pod:
		return o.Key(), nil
	case DeletedFinalStateUnknown:
		return o.Key, nil
	default:
		return "", fmt.Errorf("unknown object type: %T", obj)
	}
}

// --- Indexer (KnownObjects 인터페이스) ---

// Indexer는 현재 알려진 객체 목록을 제공한다.
// DeltaFIFO의 knownObjects(KeyListerGetter)에 대응한다 (delta_fifo.go:749).
type Indexer struct {
	mu    sync.RWMutex
	items map[string]interface{}
}

func NewIndexer() *Indexer {
	return &Indexer{items: make(map[string]interface{})}
}

func (idx *Indexer) Add(key string, obj interface{}) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.items[key] = obj
}

func (idx *Indexer) Delete(key string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.items, key)
}

func (idx *Indexer) GetByKey(key string) (interface{}, bool, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	obj, ok := idx.items[key]
	return obj, ok, nil
}

func (idx *Indexer) ListKeys() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	keys := make([]string, 0, len(idx.items))
	for k := range idx.items {
		keys = append(keys, k)
	}
	return keys
}

// --- DeltaFIFO ---

// DeltaFIFO는 키별 Delta를 축적하는 FIFO 큐이다.
// 실제 cache.DeltaFIFO(delta_fifo.go:108)에 대응한다.
//
// 내부 구조:
//   - items: 키 → Deltas 매핑 (각 키의 변경 이력)
//   - queue: FIFO 순서의 키 목록 (중복 없음)
//   - knownObjects: 현재 알려진 객체 저장소 (Indexer)
//
// 불변식:
//   - queue에 있는 키는 반드시 items에도 존재한다
//   - items에 있는 키는 반드시 queue에도 존재한다
//   - items의 각 Deltas는 최소 하나의 Delta를 포함한다
type DeltaFIFO struct {
	lock sync.RWMutex
	cond sync.Cond

	// items는 키 → Deltas 매핑
	items map[string]Deltas

	// queue는 FIFO 순서를 유지하는 키 목록 (중복 없음)
	queue []string

	// keyFunc은 객체에서 키를 추출하는 함수
	keyFunc KeyFunc

	// knownObjects는 현재 알려진 객체 목록 (Delete, Replace에서 사용)
	knownObjects *Indexer

	// closed는 큐가 닫혔는지 나타낸다
	closed bool

	// populated는 첫 번째 Replace()가 완료되었거나 Add/Update/Delete가 호출되었는지 나타낸다
	populated bool

	// initialPopulationCount는 첫 번째 Replace()에서 삽입된 아이템 수
	initialPopulationCount int
}

// NewDeltaFIFO는 새 DeltaFIFO를 생성한다.
func NewDeltaFIFO(keyFunc KeyFunc, knownObjects *Indexer) *DeltaFIFO {
	f := &DeltaFIFO{
		items:        make(map[string]Deltas),
		queue:        []string{},
		keyFunc:      keyFunc,
		knownObjects: knownObjects,
	}
	f.cond.L = &f.lock
	return f
}

// KeyOf는 객체의 키를 반환한다.
// Deltas나 DeletedFinalStateUnknown 객체도 처리한다.
func (f *DeltaFIFO) KeyOf(obj interface{}) (string, error) {
	if d, ok := obj.(Deltas); ok {
		if len(d) == 0 {
			return "", fmt.Errorf("0 length Deltas")
		}
		obj = d.Newest().Object
	}
	if d, ok := obj.(DeletedFinalStateUnknown); ok {
		return d.Key, nil
	}
	return f.keyFunc(obj)
}

// Add는 객체를 Added 타입으로 큐에 추가한다.
// 실제 delta_fifo.go:386에 대응한다.
func (f *DeltaFIFO) Add(obj interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	return f.queueActionLocked(Added, obj)
}

// Update는 객체를 Updated 타입으로 큐에 추가한다.
func (f *DeltaFIFO) Update(obj interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	return f.queueActionLocked(Updated, obj)
}

// Delete는 객체를 Deleted 타입으로 큐에 추가한다.
// 실제 delta_fifo.go:408에 대응: knownObjects가 있으면 거기도 확인한다.
func (f *DeltaFIFO) Delete(obj interface{}) error {
	id, err := f.KeyOf(obj)
	if err != nil {
		return err
	}

	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true

	if f.knownObjects == nil {
		if _, exists := f.items[id]; !exists {
			return nil // 이미 삭제됨
		}
	} else {
		_, exists, _ := f.knownObjects.GetByKey(id)
		_, itemsExist := f.items[id]
		if !exists && !itemsExist {
			return nil // knownObjects에도 items에도 없음
		}
	}

	return f.queueActionLocked(Deleted, obj)
}

// queueActionLocked는 Delta를 키의 Deltas에 추가한다.
// 실제 delta_fifo.go:482의 핵심 로직에 대응한다.
//
// 동작:
//   1. 객체에서 키를 추출한다
//   2. 기존 Deltas에 새 Delta를 append한다
//   3. dedupDeltas로 연속 동일 삭제를 병합한다
//   4. 키가 queue에 없으면 추가한다 (FIFO 순서 유지)
//   5. cond.Broadcast()로 Pop() 대기자를 깨운다
func (f *DeltaFIFO) queueActionLocked(actionType DeltaType, obj interface{}) error {
	id, err := f.KeyOf(obj)
	if err != nil {
		return err
	}

	oldDeltas := f.items[id]
	newDeltas := append(oldDeltas, Delta{actionType, obj})
	newDeltas = dedupDeltas(newDeltas)

	if _, exists := f.items[id]; !exists {
		f.queue = append(f.queue, id)
	}
	f.items[id] = newDeltas
	f.cond.Broadcast()
	return nil
}

// dedupDeltas는 연속된 동일 삭제 이벤트를 병합한다.
// 실제 delta_fifo.go:443의 로직에 대응한다.
//
// 규칙: 마지막 두 Delta가 모두 Deleted이면,
// DeletedFinalStateUnknown이 아닌 쪽을 유지한다.
func dedupDeltas(deltas Deltas) Deltas {
	n := len(deltas)
	if n < 2 {
		return deltas
	}
	a := &deltas[n-1]
	b := &deltas[n-2]
	if out := isDeletionDup(a, b); out != nil {
		deltas[n-2] = *out
		return deltas[:n-1]
	}
	return deltas
}

// isDeletionDup는 두 Delta가 모두 Deleted인 경우 병합할 Delta를 반환한다.
func isDeletionDup(a, b *Delta) *Delta {
	if b.Type != Deleted || a.Type != Deleted {
		return nil
	}
	// DeletedFinalStateUnknown보다 실제 객체를 가진 Delta를 선호
	if _, ok := b.Object.(DeletedFinalStateUnknown); ok {
		return a
	}
	return b
}

// Pop은 큐에서 가장 오래된 키의 Deltas를 꺼낸다.
// 큐가 비어있으면 아이템이 올 때까지 블로킹한다.
// 실제 delta_fifo.go:562에 대응한다.
//
// process 함수가 에러를 반환하면, 호출자가 AddIfNotPresent로 재처리할 수 있다.
func (f *DeltaFIFO) Pop(process func(interface{}, bool) error) (interface{}, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	for {
		for len(f.queue) == 0 {
			if f.closed {
				return nil, fmt.Errorf("DeltaFIFO is closed")
			}
			f.cond.Wait()
		}

		isInInitialList := !f.hasSyncedLocked()
		id := f.queue[0]
		f.queue = f.queue[1:]

		if f.initialPopulationCount > 0 {
			f.initialPopulationCount--
		}

		item, ok := f.items[id]
		if !ok {
			// 이론적으로 발생하면 안 됨
			continue
		}
		delete(f.items, id)

		err := process(item, isInInitialList)
		return item, err
	}
}

// Replace는 전체 상태를 동기화한다.
// 실제 delta_fifo.go:619에 대응한다.
//
// 동작:
//   1. 새 목록의 각 아이템에 Replaced 타입 Delta를 추가한다
//   2. 새 목록에 없는 기존 아이템에 Deleted(DeletedFinalStateUnknown) Delta를 추가한다
//   이는 Watch 연결이 끊어졌다가 re-list할 때 발생한다.
func (f *DeltaFIFO) Replace(list []interface{}, resourceVersion string) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	keys := make(map[string]bool)

	// 새 목록의 아이템을 Replaced로 추가
	for _, item := range list {
		key, err := f.KeyOf(item)
		if err != nil {
			return err
		}
		keys[key] = true
		if err := f.queueActionLocked(Replaced, item); err != nil {
			return err
		}
	}

	// items에 있지만 새 목록에 없는 아이템 → 삭제됨
	for k, oldItem := range f.items {
		if keys[k] {
			continue
		}
		var deletedObj interface{}
		if newest := oldItem.Newest(); newest != nil {
			deletedObj = newest.Object
		}
		if err := f.queueActionLocked(Deleted, DeletedFinalStateUnknown{Key: k, Obj: deletedObj}); err != nil {
			return err
		}
	}

	// knownObjects에 있지만 새 목록에 없는 아이템 → 삭제됨
	if f.knownObjects != nil {
		for _, k := range f.knownObjects.ListKeys() {
			if keys[k] {
				continue
			}
			if len(f.items[k]) > 0 {
				continue // 이미 위에서 처리됨
			}
			deletedObj, exists, _ := f.knownObjects.GetByKey(k)
			if !exists {
				continue
			}
			if err := f.queueActionLocked(Deleted, DeletedFinalStateUnknown{Key: k, Obj: deletedObj}); err != nil {
				return err
			}
		}
	}

	if !f.populated {
		f.populated = true
		f.initialPopulationCount = len(list)
	}

	return nil
}

// HasSynced는 초기 동기화가 완료되었는지 반환한다.
func (f *DeltaFIFO) HasSynced() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()
	return f.hasSyncedLocked()
}

func (f *DeltaFIFO) hasSyncedLocked() bool {
	return f.populated && f.initialPopulationCount == 0
}

// Close는 큐를 닫는다.
func (f *DeltaFIFO) Close() {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.closed = true
	f.cond.Broadcast()
}

// Len은 큐의 길이를 반환한다.
func (f *DeltaFIFO) Len() int {
	f.lock.RLock()
	defer f.lock.RUnlock()
	return len(f.queue)
}

// --- 데모 실행 ---

func main() {
	fmt.Println("=== Kubernetes DeltaFIFO 큐 시뮬레이션 ===")
	fmt.Println()

	// -----------------------------------------------
	// 1. 기본 동작: Add, Update, Delete
	// -----------------------------------------------
	fmt.Println("--- 1. 기본 동작: Add, Update, Delete ---")

	indexer := NewIndexer()
	fifo := NewDeltaFIFO(PodKeyFunc, indexer)

	// Pod 추가
	nginx := &Pod{Namespace: "default", Name: "nginx", Image: "nginx:1.19", Phase: "Pending"}
	redis := &Pod{Namespace: "default", Name: "redis", Image: "redis:6", Phase: "Pending"}

	fifo.Add(nginx)
	fifo.Add(redis)
	fmt.Printf("  Add nginx, Add redis → queue 길이: %d\n", fifo.Len())

	// nginx 업데이트 (같은 키에 Delta가 축적됨)
	nginxUpdated := &Pod{Namespace: "default", Name: "nginx", Image: "nginx:1.21", Phase: "Running"}
	fifo.Update(nginxUpdated)
	fmt.Printf("  Update nginx → queue 길이: %d (여전히 2 - 키가 이미 queue에 있음)\n", fifo.Len())

	// Pop으로 nginx의 Deltas를 꺼냄
	item, _ := fifo.Pop(func(obj interface{}, isInInitialList bool) error {
		deltas := obj.(Deltas)
		key := ""
		if p, ok := deltas.Newest().Object.(*Pod); ok {
			key = p.Key()
		}
		fmt.Printf("  Pop: key=%s, delta 수=%d\n", key, len(deltas))
		for i, d := range deltas {
			if p, ok := d.Object.(*Pod); ok {
				fmt.Printf("    [%d] %s: %s\n", i, d.Type, p)
			}
		}
		return nil
	})
	_ = item

	// Pop으로 redis의 Deltas를 꺼냄
	fifo.Pop(func(obj interface{}, isInInitialList bool) error {
		deltas := obj.(Deltas)
		if p, ok := deltas.Newest().Object.(*Pod); ok {
			fmt.Printf("  Pop: key=%s, delta 수=%d\n", p.Key(), len(deltas))
		}
		return nil
	})

	fmt.Printf("  Pop 후 queue 길이: %d\n", fifo.Len())
	fmt.Println()

	// -----------------------------------------------
	// 2. 중복 제거 (dedupDeltas)
	// -----------------------------------------------
	fmt.Println("--- 2. 중복 삭제 이벤트 병합 (dedupDeltas) ---")

	fifo2 := NewDeltaFIFO(PodKeyFunc, nil)

	pg := &Pod{Namespace: "default", Name: "postgres", Image: "postgres:14", Phase: "Running"}
	fifo2.Add(pg)
	fifo2.Delete(pg)
	fifo2.Delete(pg) // 연속 삭제 → 병합되어야 함

	fifo2.Pop(func(obj interface{}, isInInitialList bool) error {
		deltas := obj.(Deltas)
		fmt.Printf("  postgres Deltas (Add + Delete + Delete → 병합):\n")
		for i, d := range deltas {
			fmt.Printf("    [%d] %s\n", i, d.Type)
		}
		fmt.Printf("  → 연속 Delete가 병합되어 총 %d개 Delta (Added, Deleted)\n", len(deltas))
		return nil
	})
	fmt.Println()

	// -----------------------------------------------
	// 3. Replace (re-list 시나리오)
	// -----------------------------------------------
	fmt.Println("--- 3. Replace (re-list 시나리오) ---")

	indexer3 := NewIndexer()
	fifo3 := NewDeltaFIFO(PodKeyFunc, indexer3)

	// 기존 상태: nginx, redis, postgres
	existingPods := []*Pod{
		{Namespace: "default", Name: "nginx", Image: "nginx:1.19", Phase: "Running"},
		{Namespace: "default", Name: "redis", Image: "redis:6", Phase: "Running"},
		{Namespace: "default", Name: "postgres", Image: "postgres:14", Phase: "Running"},
	}
	for _, p := range existingPods {
		indexer3.Add(p.Key(), p)
	}

	fmt.Println("  기존 상태: nginx, redis, postgres (indexer에 존재)")

	// re-list 결과: nginx(업데이트됨), redis(그대로), memcached(새로 추가됨)
	// → postgres가 사라짐 → DeletedFinalStateUnknown 발생
	newList := []interface{}{
		&Pod{Namespace: "default", Name: "nginx", Image: "nginx:1.21", Phase: "Running"},
		&Pod{Namespace: "default", Name: "redis", Image: "redis:6", Phase: "Running"},
		&Pod{Namespace: "default", Name: "memcached", Image: "memcached:1.6", Phase: "Pending"},
	}

	fmt.Println("  re-list 결과: nginx(갱신), redis(유지), memcached(신규)")
	fmt.Println("  → postgres가 사라짐 → DeletedFinalStateUnknown 발행")

	fifo3.Replace(newList, "100")

	fmt.Println("  Pop 결과:")
	for fifo3.Len() > 0 {
		fifo3.Pop(func(obj interface{}, isInInitialList bool) error {
			deltas := obj.(Deltas)
			for _, d := range deltas {
				switch o := d.Object.(type) {
				case *Pod:
					fmt.Printf("    [%s] %s (initialList=%v)\n", d.Type, o, isInInitialList)
				case DeletedFinalStateUnknown:
					fmt.Printf("    [%s] %s (tombstone, initialList=%v)\n", d.Type, o, isInInitialList)
				}
			}
			return nil
		})
	}
	fmt.Println()

	// -----------------------------------------------
	// 4. 블로킹 Pop 테스트
	// -----------------------------------------------
	fmt.Println("--- 4. 블로킹 Pop ---")

	fifo4 := NewDeltaFIFO(PodKeyFunc, nil)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		fmt.Println("  [소비자] Pop 대기 중...")
		fifo4.Pop(func(obj interface{}, isInInitialList bool) error {
			deltas := obj.(Deltas)
			if p, ok := deltas.Newest().Object.(*Pod); ok {
				fmt.Printf("  [소비자] 수신: %s %s\n", deltas.Newest().Type, p)
			}
			return nil
		})
	}()

	time.Sleep(100 * time.Millisecond)
	fmt.Println("  [생산자] 1초 후 아이템 추가...")
	time.Sleep(100 * time.Millisecond)
	fifo4.Add(&Pod{Namespace: "default", Name: "delayed", Image: "busybox", Phase: "Pending"})

	wg.Wait()
	fmt.Println()

	// -----------------------------------------------
	// 5. 키별 Delta 축적 확인
	// -----------------------------------------------
	fmt.Println("--- 5. 키별 Delta 축적 ---")

	fifo5 := NewDeltaFIFO(PodKeyFunc, nil)

	// 같은 키에 여러 변경을 빠르게 적용
	busybox := &Pod{Namespace: "default", Name: "busybox", Image: "busybox:1.34", Phase: "Pending"}
	fifo5.Add(busybox)

	busybox2 := &Pod{Namespace: "default", Name: "busybox", Image: "busybox:1.34", Phase: "Running"}
	fifo5.Update(busybox2)

	busybox3 := &Pod{Namespace: "default", Name: "busybox", Image: "busybox:1.35", Phase: "Running"}
	fifo5.Update(busybox3)

	fifo5.Delete(busybox3)

	fifo5.Pop(func(obj interface{}, isInInitialList bool) error {
		deltas := obj.(Deltas)
		fmt.Printf("  busybox의 전체 변경 이력 (Pop 한 번으로 모두 수신):\n")
		for i, d := range deltas {
			if p, ok := d.Object.(*Pod); ok {
				fmt.Printf("    [%d] %s: image=%s, phase=%s\n", i, d.Type, p.Image, p.Phase)
			}
		}
		fmt.Println("  → 컨트롤러는 이 이력을 보고 최신 상태만 처리하거나, 전체 이력을 활용할 수 있다")
		return nil
	})
	fmt.Println()

	// -----------------------------------------------
	// 6. HasSynced (초기 동기화 확인)
	// -----------------------------------------------
	fmt.Println("--- 6. HasSynced (초기 동기화 확인) ---")

	indexer6 := NewIndexer()
	fifo6 := NewDeltaFIFO(PodKeyFunc, indexer6)

	fmt.Printf("  Replace 전 HasSynced: %v\n", fifo6.HasSynced())

	fifo6.Replace([]interface{}{
		&Pod{Namespace: "default", Name: "a", Image: "img", Phase: "Running"},
		&Pod{Namespace: "default", Name: "b", Image: "img", Phase: "Running"},
	}, "1")

	fmt.Printf("  Replace 후 (Pop 전) HasSynced: %v (아직 Pop하지 않음)\n", fifo6.HasSynced())

	fifo6.Pop(func(obj interface{}, isInInitialList bool) error { return nil })
	fmt.Printf("  1번째 Pop 후 HasSynced: %v\n", fifo6.HasSynced())

	fifo6.Pop(func(obj interface{}, isInInitialList bool) error { return nil })
	fmt.Printf("  2번째 Pop 후 HasSynced: %v (모든 초기 아이템 처리 완료)\n", fifo6.HasSynced())

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 요약:")
	fmt.Println("  1. DeltaFIFO는 키별로 변경 이력(Deltas)을 축적한다")
	fmt.Println("  2. FIFO 순서를 유지하면서 같은 키의 이벤트를 하나로 묶는다")
	fmt.Println("  3. Pop은 키의 전체 변경 이력을 한 번에 반환한다")
	fmt.Println("  4. Replace는 re-list 시 누락된 삭제를 DeletedFinalStateUnknown으로 보상한다")
	fmt.Println("  5. dedupDeltas로 연속 동일 삭제 이벤트를 병합한다")
	fmt.Println("  6. HasSynced로 초기 목록의 처리 완료 여부를 추적한다")
}
