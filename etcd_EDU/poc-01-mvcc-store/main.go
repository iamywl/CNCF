// poc-01-mvcc-store: etcd MVCC(Multi-Version Concurrency Control) 저장소 시뮬레이션
//
// etcd의 MVCC 저장소는 모든 키-값 변경을 리비전(Revision) 단위로 관리한다.
// 각 리비전은 {Main, Sub} 쌍으로 구성되며, Main은 트랜잭션 번호,
// Sub는 트랜잭션 내 개별 변경 순서를 나타낸다.
//
// 실제 구현 참조:
//   - server/storage/mvcc/revision.go: Revision 구조체
//   - server/storage/mvcc/kvstore.go: store 구조체 (currentRev 관리)
//   - server/storage/mvcc/kvstore_txn.go: put/delete 트랜잭션 처리
//
// 이 PoC는 리비전 기반 다중 버전 KV 저장소의 핵심 원리를 재현한다.
package main

import (
	"fmt"
	"strings"
	"sync"
)

// ============================================================
// Revision: etcd의 server/storage/mvcc/revision.go 기반
// Main은 트랜잭션 단위, Sub는 트랜잭션 내 개별 변경 순서
// ============================================================

// Revision은 변경의 논리적 시점을 나타낸다.
// etcd 소스: type Revision struct { Main int64; Sub int64 }
type Revision struct {
	Main int64 // 트랜잭션 번호 (단조 증가)
	Sub  int64 // 트랜잭션 내 변경 순서
}

// GreaterThan은 리비전 비교. Main 우선, 같으면 Sub 비교.
// etcd 소스: func (a Revision) GreaterThan(b Revision) bool
func (a Revision) GreaterThan(b Revision) bool {
	if a.Main > b.Main {
		return true
	}
	if a.Main < b.Main {
		return false
	}
	return a.Sub > b.Sub
}

func (r Revision) String() string {
	return fmt.Sprintf("{Main:%d, Sub:%d}", r.Main, r.Sub)
}

// ============================================================
// KeyValue: 리비전에 대응하는 실제 키-값 데이터
// etcd 소스: api/v3/mvccpb/kv.proto → KeyValue 메시지
// ============================================================

type KeyValue struct {
	Key            string
	Value          string
	CreateRevision int64 // 키가 처음 생성된 리비전
	ModRevision    int64 // 마지막 수정 리비전
	Version        int64 // 현재 세대에서의 버전 (1부터 시작)
}

// ============================================================
// BucketKey: 리비전 + 톰스톤 마커
// etcd 소스: type BucketKey struct { Revision; tombstone bool }
// 삭제된 키는 tombstone=true로 마킹하여 Watch에서 DELETE 이벤트 생성
// ============================================================

type BucketKey struct {
	Revision
	Tombstone bool // 삭제 마커 (etcd: markTombstone byte = 't')
}

// ============================================================
// MVCCStore: 리비전 기반 다중 버전 저장소
// etcd의 store 구조체를 단순화. 실제로는 BoltDB 백엔드 + treeIndex를 사용하지만
// 여기서는 맵 기반으로 핵심 로직만 재현한다.
//
// 데이터 흐름:
//   Put(key, value) → currentRev++ → revisions[rev] = KV → index[key] = append(rev)
//   Get(key)        → index[key]에서 최신 비-톰스톤 리비전 조회 → revisions[rev]
//   Delete(key)     → currentRev++ → revisions[rev] = KV(tombstone) → index[key] = append(rev)
// ============================================================

type MVCCStore struct {
	mu sync.RWMutex

	// currentRev: 현재 리비전 (단조 증가)
	// etcd 소스: store.currentRev int64 (초기값 1)
	currentRev int64

	// revisions: 리비전 → 키-값 데이터 매핑 (etcd에서는 BoltDB의 Key 버킷)
	// 키: BucketKey, 값: KeyValue
	revisions map[int64]BucketKey
	kvdata    map[int64]KeyValue

	// keyIndex: 키 → 리비전 목록 매핑 (etcd에서는 treeIndex)
	// 각 키의 리비전 이력을 추적
	keyIndex map[string][]int64
}

// NewMVCCStore는 새 MVCC 저장소를 생성한다.
// etcd 소스: NewStore()에서 currentRev=1로 초기화
func NewMVCCStore() *MVCCStore {
	return &MVCCStore{
		currentRev: 1, // etcd와 동일하게 1부터 시작
		revisions:  make(map[int64]BucketKey),
		kvdata:     make(map[int64]KeyValue),
		keyIndex:   make(map[string][]int64),
	}
}

// Put은 키-값 쌍을 저장하고 새 리비전을 생성한다.
// etcd 소스: kvstore_txn.go → put() 메서드
// 실제 흐름: currentRev++ → BucketKey 생성 → BoltDB에 저장 → treeIndex 업데이트
func (s *MVCCStore) Put(key, value string) Revision {
	s.mu.Lock()
	defer s.mu.Unlock()

	rev := s.currentRev
	s.currentRev++

	// 키의 버전 계산 (현재 세대에서 몇 번째 수정인지)
	version := int64(1)
	createRev := rev
	if revs, ok := s.keyIndex[key]; ok {
		// 기존 키: 마지막 리비전이 톰스톤이 아닌지 확인
		for i := len(revs) - 1; i >= 0; i-- {
			bk := s.revisions[revs[i]]
			if bk.Tombstone {
				// 이전 세대가 삭제됨 → 새 세대 시작 (version=1)
				createRev = rev
				break
			}
			version++
			createRev = s.kvdata[revs[i]].CreateRevision
		}
	}

	// BucketKey 저장 (etcd: RevToBytes → BoltDB Key 버킷에 저장)
	bk := BucketKey{
		Revision:  Revision{Main: rev, Sub: 0},
		Tombstone: false,
	}
	s.revisions[rev] = bk

	// KeyValue 저장 (etcd: mvccpb.KeyValue → BoltDB Value로 직렬화)
	kv := KeyValue{
		Key:            key,
		Value:          value,
		CreateRevision: createRev,
		ModRevision:    rev,
		Version:        version,
	}
	s.kvdata[rev] = kv

	// 인덱스 업데이트 (etcd: treeIndex.Put)
	s.keyIndex[key] = append(s.keyIndex[key], rev)

	return Revision{Main: rev, Sub: 0}
}

// Get은 특정 리비전의 키 값을 조회한다. rev=0이면 최신 값을 반환한다.
// etcd 소스: kv_view.go → RangeRev() 메서드
// 실제 흐름: treeIndex에서 리비전 조회 → BoltDB에서 KeyValue 읽기
func (s *MVCCStore) Get(key string, rev int64) (KeyValue, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	revs, ok := s.keyIndex[key]
	if !ok {
		return KeyValue{}, false
	}

	if rev == 0 {
		// 최신 리비전 조회
		rev = s.currentRev - 1
	}

	// 지정된 리비전 이하에서 가장 최신 리비전 찾기
	// etcd 소스: keyIndex.get() → findGeneration() → walk()
	for i := len(revs) - 1; i >= 0; i-- {
		r := revs[i]
		if r > rev {
			continue
		}
		bk := s.revisions[r]
		if bk.Tombstone {
			// 톰스톤이면 해당 시점에서 키가 삭제된 상태
			return KeyValue{}, false
		}
		return s.kvdata[r], true
	}
	return KeyValue{}, false
}

// Delete은 키를 톰스톤으로 마킹한다. 실제 데이터는 남아있고 리비전만 추가된다.
// etcd 소스: kvstore_txn.go → delete() 메서드
// 실제 흐름: currentRev++ → BucketKey(tombstone=true) 생성 → treeIndex에 톰스톤 추가
func (s *MVCCStore) Delete(key string) (Revision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	revs, ok := s.keyIndex[key]
	if !ok {
		return Revision{}, false
	}

	// 이미 삭제된 키인지 확인
	lastRev := revs[len(revs)-1]
	if s.revisions[lastRev].Tombstone {
		return Revision{}, false
	}

	rev := s.currentRev
	s.currentRev++

	// 톰스톤 BucketKey 저장
	// etcd: isTombstone(b []byte) → markTombstone byte = 't'
	bk := BucketKey{
		Revision:  Revision{Main: rev, Sub: 0},
		Tombstone: true,
	}
	s.revisions[rev] = bk

	// 삭제된 KV 정보 저장 (Watch에서 DELETE 이벤트로 활용)
	prevKV := s.kvdata[lastRev]
	kv := KeyValue{
		Key:            key,
		Value:          "", // 삭제 시 값은 비움
		CreateRevision: prevKV.CreateRevision,
		ModRevision:    rev,
		Version:        prevKV.Version + 1,
	}
	s.kvdata[rev] = kv

	// 인덱스에 톰스톤 리비전 추가
	// etcd: keyIndex.tombstone() → 현재 세대에 리비전 추가 + 새 빈 세대 생성
	s.keyIndex[key] = append(s.keyIndex[key], rev)

	return Revision{Main: rev, Sub: 0}, true
}

// History는 키의 전체 리비전 히스토리를 반환한다.
// etcd에서는 compaction 전까지 모든 히스토리가 유지된다.
func (s *MVCCStore) History(key string) []HistoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	revs, ok := s.keyIndex[key]
	if !ok {
		return nil
	}

	var history []HistoryEntry
	for _, rev := range revs {
		bk := s.revisions[rev]
		kv := s.kvdata[rev]
		entry := HistoryEntry{
			Revision:  bk.Revision,
			Tombstone: bk.Tombstone,
			KV:        kv,
		}
		history = append(history, entry)
	}
	return history
}

type HistoryEntry struct {
	Revision  Revision
	Tombstone bool
	KV        KeyValue
}

// CurrentRev는 현재 리비전을 반환한다.
func (s *MVCCStore) CurrentRev() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentRev
}

// ============================================================
// 메인: MVCC 저장소의 핵심 동작을 시연
// ============================================================

func main() {
	fmt.Println("=== etcd MVCC 저장소 시뮬레이션 ===")
	fmt.Println()

	store := NewMVCCStore()

	// ----------------------------------------
	// 시나리오 1: 기본 Put/Get
	// ----------------------------------------
	fmt.Println("--- 시나리오 1: 기본 Put/Get ---")
	rev1 := store.Put("name", "Alice")
	fmt.Printf("Put(\"name\", \"Alice\") → 리비전: %s\n", rev1)

	rev2 := store.Put("age", "30")
	fmt.Printf("Put(\"age\", \"30\")     → 리비전: %s\n", rev2)

	rev3 := store.Put("name", "Bob")
	fmt.Printf("Put(\"name\", \"Bob\")   → 리비전: %s\n", rev3)

	kv, ok := store.Get("name", 0) // 최신값
	fmt.Printf("Get(\"name\", 최신)    → 값: %q, 버전: %d, 생성리비전: %d, 수정리비전: %d\n",
		kv.Value, kv.Version, kv.CreateRevision, kv.ModRevision)
	_ = ok

	fmt.Println()

	// ----------------------------------------
	// 시나리오 2: 과거 리비전 조회 (Time Travel)
	// etcd의 핵심 기능: 과거 시점의 데이터를 조회할 수 있음
	// ----------------------------------------
	fmt.Println("--- 시나리오 2: 과거 리비전 조회 (Time Travel) ---")
	kv1, ok1 := store.Get("name", rev1.Main) // rev1 시점
	if ok1 {
		fmt.Printf("Get(\"name\", rev=%d) → 값: %q (과거 시점)\n", rev1.Main, kv1.Value)
	}
	kv3, ok3 := store.Get("name", rev3.Main) // rev3 시점
	if ok3 {
		fmt.Printf("Get(\"name\", rev=%d) → 값: %q (최신 시점)\n", rev3.Main, kv3.Value)
	}

	// age는 rev1 시점에 아직 없음
	_, okAge := store.Get("age", rev1.Main)
	fmt.Printf("Get(\"age\",  rev=%d) → 존재: %v (아직 생성 전)\n", rev1.Main, okAge)

	kv2, okAge2 := store.Get("age", rev2.Main)
	if okAge2 {
		fmt.Printf("Get(\"age\",  rev=%d) → 값: %q\n", rev2.Main, kv2.Value)
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 3: Delete (톰스톤 마킹)
	// etcd에서 삭제는 실제 데이터를 지우지 않고 톰스톤 리비전을 추가
	// ----------------------------------------
	fmt.Println("--- 시나리오 3: Delete (톰스톤) ---")
	delRev, _ := store.Delete("name")
	fmt.Printf("Delete(\"name\")       → 톰스톤 리비전: %s\n", delRev)

	_, okDel := store.Get("name", 0)
	fmt.Printf("Get(\"name\", 최신)    → 존재: %v (삭제됨)\n", okDel)

	// 삭제 전 시점으로 조회하면 여전히 값이 보임
	kvPast, okPast := store.Get("name", rev3.Main)
	if okPast {
		fmt.Printf("Get(\"name\", rev=%d) → 값: %q (삭제 전 시점)\n", rev3.Main, kvPast.Value)
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 4: 삭제 후 재생성 (새 세대)
	// etcd: keyIndex에 새 generation이 시작됨
	// ----------------------------------------
	fmt.Println("--- 시나리오 4: 삭제 후 재생성 (새 세대) ---")
	rev5 := store.Put("name", "Charlie")
	fmt.Printf("Put(\"name\", \"Charlie\") → 리비전: %s\n", rev5)

	kvNew, _ := store.Get("name", 0)
	fmt.Printf("Get(\"name\", 최신)      → 값: %q, 버전: %d (새 세대, 버전 1부터 재시작)\n",
		kvNew.Value, kvNew.Version)
	fmt.Println()

	// ----------------------------------------
	// 시나리오 5: 리비전 히스토리 전체 조회
	// ----------------------------------------
	fmt.Println("--- 시나리오 5: 리비전 히스토리 ---")
	fmt.Println("키 \"name\"의 전체 히스토리:")
	history := store.History("name")
	fmt.Println(strings.Repeat("-", 75))
	fmt.Printf("%-15s %-10s %-10s %-8s %-8s %-8s\n",
		"리비전", "값", "톰스톤", "버전", "생성Rev", "수정Rev")
	fmt.Println(strings.Repeat("-", 75))
	for _, h := range history {
		tombStr := "false"
		if h.Tombstone {
			tombStr = "TRUE"
		}
		fmt.Printf("%-15s %-10s %-10s %-8d %-8d %-8d\n",
			h.Revision, h.KV.Value, tombStr, h.KV.Version,
			h.KV.CreateRevision, h.KV.ModRevision)
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 6: 다중 키 시나리오
	// ----------------------------------------
	fmt.Println("--- 시나리오 6: 다중 키 + 현재 리비전 ---")
	store.Put("server/addr", "10.0.0.1")
	store.Put("server/port", "8080")
	store.Put("server/addr", "10.0.0.2")

	kvAddr, _ := store.Get("server/addr", 0)
	kvPort, _ := store.Get("server/port", 0)
	fmt.Printf("현재 리비전: %d\n", store.CurrentRev())
	fmt.Printf("server/addr = %q (수정리비전: %d)\n", kvAddr.Value, kvAddr.ModRevision)
	fmt.Printf("server/port = %q (수정리비전: %d)\n", kvPort.Value, kvPort.ModRevision)

	fmt.Println()
	fmt.Println("=== MVCC 저장소 핵심 원리 ===")
	fmt.Println("1. 모든 변경은 리비전(Revision)을 생성 — 데이터를 덮어쓰지 않음")
	fmt.Println("2. 삭제는 톰스톤 마킹 — 과거 데이터는 보존됨")
	fmt.Println("3. 과거 리비전 조회 가능 — Time Travel 쿼리")
	fmt.Println("4. 삭제 후 재생성 시 새 세대(generation) 시작")
	fmt.Println("5. Compaction이 오래된 리비전을 정리할 때까지 모든 히스토리 유지")
}
