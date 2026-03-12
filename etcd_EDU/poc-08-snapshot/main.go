// poc-08-snapshot: 상태 스냅샷 생성/복구 시뮬레이션
//
// etcd의 스냅샷 시스템(server/etcdserver/api/snap/snapshotter.go)을 기반으로
// KV 상태 직렬화, CRC32 무결성 검증, WAL+스냅샷 기반 복구를 시뮬레이션한다.
//
// 참조: server/etcdserver/api/snap/snapshotter.go - 스냅샷 저장/로드
//       server/etcdserver/api/snap/snappb/snap.pb.go - 스냅샷 메시지
//       server/storage/wal/ - WAL (Write-Ahead Log)
//
// 실행: go run main.go

package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"time"
)

// ========== 데이터 모델 ==========

// KeyValue는 KV 저장소의 엔트리
type KeyValue struct {
	Key            string
	Value          string
	Version        int64
	CreateRevision int64
	ModRevision    int64
}

// WALEntry는 WAL 로그 엔트리
// etcd의 wal.Record에 해당
type WALEntry struct {
	Index   uint64
	Term    uint64
	Type    string // "put" | "delete"
	Key     string
	Value   string
}

// SnapshotMetadata는 스냅샷 메타데이터
// etcd의 raftpb.SnapshotMetadata에 해당
type SnapshotMetadata struct {
	Term  uint64
	Index uint64
}

// SnapshotData는 직렬화된 스냅샷 데이터
// etcd의 snappb.Snapshot에 해당
type SnapshotData struct {
	CRC      uint32
	Data     []byte
	Metadata SnapshotMetadata
}

// ========== KV 저장소 ==========

// KVStore는 MVCC 키-값 저장소 (단순화)
type KVStore struct {
	data     map[string]*KeyValue
	revision int64
}

func NewKVStore() *KVStore {
	return &KVStore{
		data:     make(map[string]*KeyValue),
		revision: 0,
	}
}

func (s *KVStore) Put(key, value string) {
	s.revision++
	kv, exists := s.data[key]
	if exists {
		kv.Value = value
		kv.Version++
		kv.ModRevision = s.revision
	} else {
		s.data[key] = &KeyValue{
			Key:            key,
			Value:          value,
			Version:        1,
			CreateRevision: s.revision,
			ModRevision:    s.revision,
		}
	}
}

func (s *KVStore) Delete(key string) {
	if _, exists := s.data[key]; exists {
		s.revision++
		delete(s.data, key)
	}
}

func (s *KVStore) Get(key string) (*KeyValue, bool) {
	kv, ok := s.data[key]
	return kv, ok
}

func (s *KVStore) Size() int {
	return len(s.data)
}

// Serialize는 전체 KV 상태를 바이트로 직렬화한다
func (s *KVStore) Serialize() ([]byte, error) {
	// gob 인코딩으로 직렬화
	type storeSnapshot struct {
		Data     map[string]*KeyValue
		Revision int64
	}
	ss := storeSnapshot{
		Data:     s.data,
		Revision: s.revision,
	}
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(ss); err != nil {
		return nil, fmt.Errorf("직렬화 실패: %w", err)
	}
	return buf.Bytes(), nil
}

// Deserialize는 바이트에서 KV 상태를 복원한다
func (s *KVStore) Deserialize(data []byte) error {
	type storeSnapshot struct {
		Data     map[string]*KeyValue
		Revision int64
	}
	var ss storeSnapshot
	dec := gob.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&ss); err != nil {
		return fmt.Errorf("역직렬화 실패: %w", err)
	}
	s.data = ss.Data
	s.revision = ss.Revision
	return nil
}

// ========== WAL (Write-Ahead Log) ==========

// WAL은 변경 로그를 순서대로 기록한다
// etcd의 storage/wal/wal.go를 단순화
type WAL struct {
	entries []WALEntry
	dir     string
}

func NewWAL(dir string) *WAL {
	return &WAL{
		entries: make([]WALEntry, 0),
		dir:     dir,
	}
}

// Append는 WAL에 엔트리를 추가한다
func (w *WAL) Append(entry WALEntry) {
	w.entries = append(w.entries, entry)
}

// EntriesAfter는 특정 인덱스 이후의 엔트리를 반환한다
func (w *WAL) EntriesAfter(index uint64) []WALEntry {
	var result []WALEntry
	for _, e := range w.entries {
		if e.Index > index {
			result = append(result, e)
		}
	}
	return result
}

// Save는 WAL을 파일에 저장한다
func (w *WAL) Save() error {
	path := filepath.Join(w.dir, "wal.log")
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(w.entries); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

// Load는 파일에서 WAL을 로드한다
func (w *WAL) Load() error {
	path := filepath.Join(w.dir, "wal.log")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := gob.NewDecoder(bytes.NewReader(data))
	return dec.Decode(&w.entries)
}

func (w *WAL) LastIndex() uint64 {
	if len(w.entries) == 0 {
		return 0
	}
	return w.entries[len(w.entries)-1].Index
}

// ========== Snapshotter ==========

// Snapshotter는 스냅샷의 저장과 로드를 관리한다
// etcd의 snap.Snapshotter에 해당
type Snapshotter struct {
	dir string
}

// crcTable은 CRC32 체크섬용 테이블
// etcd의 snap.crcTable (Castagnoli)에 해당
var crcTable = crc32.MakeTable(crc32.Castagnoli)

func NewSnapshotter(dir string) *Snapshotter {
	os.MkdirAll(dir, 0755)
	return &Snapshotter{dir: dir}
}

// Save는 스냅샷을 파일에 저장한다
// etcd의 Snapshotter.save()에 해당:
//   fname = "%016x-%016x.snap" (term-index)
//   CRC32(data) 계산 → snappb.Snapshot{CRC, Data} → 파일 저장
func (s *Snapshotter) Save(metadata SnapshotMetadata, kvData []byte) (string, error) {
	start := time.Now()

	// CRC32 체크섬 계산 (etcd는 Castagnoli CRC32 사용)
	crc := crc32.Update(0, crcTable, kvData)

	snap := SnapshotData{
		CRC:      crc,
		Data:     kvData,
		Metadata: metadata,
	}

	// 직렬화
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(snap); err != nil {
		return "", fmt.Errorf("스냅샷 직렬화 실패: %w", err)
	}

	// 파일명: term-index.snap (etcd와 동일한 형식)
	fname := fmt.Sprintf("%016x-%016x.snap", metadata.Term, metadata.Index)
	path := filepath.Join(s.dir, fname)

	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return "", fmt.Errorf("스냅샷 저장 실패: %w", err)
	}

	elapsed := time.Since(start)
	fmt.Printf("  [Snapshotter] 저장 완료: %s (%d bytes, CRC=%08x, %v)\n",
		fname, len(buf.Bytes()), crc, elapsed)

	return path, nil
}

// Load는 스냅샷을 파일에서 로드한다
// etcd의 Snapshotter.Load()에 해당:
//   파일 읽기 → snappb.Snapshot 디코딩 → CRC 검증 → 데이터 반환
func (s *Snapshotter) Load(path string) (*SnapshotData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("스냅샷 파일 읽기 실패: %w", err)
	}

	var snap SnapshotData
	dec := gob.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("스냅샷 디코딩 실패: %w", err)
	}

	// CRC 무결성 검증 (etcd의 ErrCRCMismatch 검사에 해당)
	expectedCRC := crc32.Update(0, crcTable, snap.Data)
	if snap.CRC != expectedCRC {
		return nil, fmt.Errorf("CRC 불일치: 기대=%08x, 실제=%08x", expectedCRC, snap.CRC)
	}

	fmt.Printf("  [Snapshotter] 로드 완료: CRC 검증 성공 (%08x)\n", snap.CRC)
	return &snap, nil
}

// ========== 복구 엔진 ==========

// RecoveryEngine은 스냅샷 + WAL 기반 복구를 수행한다
// etcd의 서버 시작 시 복구 흐름을 단순화
type RecoveryEngine struct {
	snapshotter *Snapshotter
	wal         *WAL
}

func NewRecoveryEngine(snapshotter *Snapshotter, wal *WAL) *RecoveryEngine {
	return &RecoveryEngine{
		snapshotter: snapshotter,
		wal:         wal,
	}
}

// Recover는 스냅샷에서 상태를 복원하고 WAL을 재생한다
// etcd의 복구 흐름:
//   1. 스냅샷 로드 → KV 상태 복원
//   2. 스냅샷 이후의 WAL 엔트리 재생 → 최신 상태 도달
func (r *RecoveryEngine) Recover(snapPath string) (*KVStore, error) {
	fmt.Println("  [Recovery] 복구 시작...")

	// 1단계: 스냅샷 로드
	snap, err := r.snapshotter.Load(snapPath)
	if err != nil {
		return nil, fmt.Errorf("스냅샷 로드 실패: %w", err)
	}

	store := NewKVStore()
	if err := store.Deserialize(snap.Data); err != nil {
		return nil, fmt.Errorf("상태 복원 실패: %w", err)
	}

	fmt.Printf("  [Recovery] 스냅샷 복원: term=%d, index=%d, 키 수=%d, 리비전=%d\n",
		snap.Metadata.Term, snap.Metadata.Index, store.Size(), store.revision)

	// 2단계: WAL 재생
	walEntries := r.wal.EntriesAfter(snap.Metadata.Index)
	fmt.Printf("  [Recovery] WAL 재생: %d개 엔트리\n", len(walEntries))

	for _, entry := range walEntries {
		switch entry.Type {
		case "put":
			store.Put(entry.Key, entry.Value)
			fmt.Printf("  [Recovery]   WAL 재생: PUT %s = %q (index=%d)\n",
				entry.Key, entry.Value, entry.Index)
		case "delete":
			store.Delete(entry.Key)
			fmt.Printf("  [Recovery]   WAL 재생: DELETE %s (index=%d)\n",
				entry.Key, entry.Index)
		}
	}

	fmt.Printf("  [Recovery] 복구 완료: 키 수=%d, 리비전=%d\n", store.Size(), store.revision)
	return store, nil
}

// ========== 유틸리티 ==========

func printStore(store *KVStore) {
	if store.Size() == 0 {
		fmt.Println("  (비어 있음)")
		return
	}
	fmt.Println("  ┌──────────────────────┬──────────────────┬─────────┬────────┐")
	fmt.Println("  │ 키                   │ 값               │ 버전    │ 수정Rev│")
	fmt.Println("  ├──────────────────────┼──────────────────┼─────────┼────────┤")
	for _, kv := range store.data {
		fmt.Printf("  │ %-20s │ %-16s │   %3d   │  %3d   │\n",
			kv.Key, kv.Value, kv.Version, kv.ModRevision)
	}
	fmt.Println("  └──────────────────────┴──────────────────┴─────────┴────────┘")
	fmt.Printf("  리비전: %d\n", store.revision)
}

func printFileInfo(path string) {
	info, err := os.Stat(path)
	if err != nil {
		fmt.Printf("  파일 없음: %s\n", path)
		return
	}
	fmt.Printf("  파일: %s (%d bytes)\n", filepath.Base(path), info.Size())
}

// ========== CRC 손상 시연 ==========

func demonstrateCRCCorruption(snapshotter *Snapshotter, validPath string) {
	// 정상 스냅샷 로드하여 내부 데이터를 손상시킨 후 재저장
	snap, err := snapshotter.Load(validPath)
	if err != nil {
		fmt.Printf("  파일 읽기 실패: %v\n", err)
		return
	}

	// Data 내용을 변조 (CRC는 원래 값 유지 → 불일치 발생)
	corruptedSnap := SnapshotData{
		CRC:      snap.CRC, // 원래 CRC 유지
		Data:     append([]byte{}, snap.Data...), // 복사
		Metadata: snap.Metadata,
	}
	// 데이터 변조
	if len(corruptedSnap.Data) > 10 {
		corruptedSnap.Data[10] ^= 0xFF
	}

	// 손상된 스냅샷을 파일로 직접 저장
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	enc.Encode(corruptedSnap)
	corruptPath := filepath.Join(filepath.Dir(validPath), "corrupted.snap")
	os.WriteFile(corruptPath, buf.Bytes(), 0644)

	// 손상된 스냅샷 로드 시도 → CRC 불일치 감지
	_, err = snapshotter.Load(corruptPath)
	if err != nil {
		fmt.Printf("  손상 감지 성공: %v\n", err)
	} else {
		fmt.Println("  경고: 손상이 감지되지 않음")
	}

	os.Remove(corruptPath)
}

// ========== 메인 ==========

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" etcd PoC-08: 상태 스냅샷 생성/복구")
	fmt.Println("==========================================================")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "etcd-snap-poc-")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	snapDir := filepath.Join(tmpDir, "snap")
	walDir := filepath.Join(tmpDir, "wal")
	os.MkdirAll(snapDir, 0755)
	os.MkdirAll(walDir, 0755)

	snapshotter := NewSnapshotter(snapDir)
	wal := NewWAL(walDir)

	// 1. 초기 데이터 입력
	fmt.Println("[1] 초기 데이터 입력")
	fmt.Println("──────────────────────────────────────")

	store := NewKVStore()
	walIndex := uint64(0)

	// 데이터 추가 + WAL 기록
	ops := []struct{ key, value string }{
		{"/cluster/name", "production"},
		{"/cluster/size", "3"},
		{"/config/db_host", "10.0.1.5"},
		{"/config/db_port", "5432"},
		{"/config/max_conn", "100"},
	}

	for _, op := range ops {
		store.Put(op.key, op.value)
		walIndex++
		wal.Append(WALEntry{
			Index: walIndex,
			Term:  1,
			Type:  "put",
			Key:   op.key,
			Value: op.value,
		})
	}

	fmt.Println("  초기 KV 상태:")
	printStore(store)

	// 2. 스냅샷 생성
	fmt.Println("\n[2] 스냅샷 생성")
	fmt.Println("──────────────────────────────────────")

	kvData, err := store.Serialize()
	if err != nil {
		fmt.Printf("  직렬화 실패: %v\n", err)
		return
	}

	snapMeta := SnapshotMetadata{Term: 1, Index: walIndex}
	snapPath, err := snapshotter.Save(snapMeta, kvData)
	if err != nil {
		fmt.Printf("  스냅샷 저장 실패: %v\n", err)
		return
	}
	printFileInfo(snapPath)

	// 3. 스냅샷 이후 추가 데이터 입력 (WAL에만 기록)
	fmt.Println("\n[3] 스냅샷 이후 추가 데이터 (WAL에 기록)")
	fmt.Println("──────────────────────────────────────")

	postSnapOps := []struct {
		opType string
		key    string
		value  string
	}{
		{"put", "/config/timeout", "30s"},
		{"put", "/config/max_conn", "200"},
		{"delete", "/config/db_port", ""},
		{"put", "/services/web-1", "10.0.2.10:8080"},
	}

	for _, op := range postSnapOps {
		walIndex++
		wal.Append(WALEntry{
			Index: walIndex,
			Term:  1,
			Type:  op.opType,
			Key:   op.key,
			Value: op.value,
		})

		switch op.opType {
		case "put":
			store.Put(op.key, op.value)
			fmt.Printf("  WAL[%d] PUT %s = %q\n", walIndex, op.key, op.value)
		case "delete":
			store.Delete(op.key)
			fmt.Printf("  WAL[%d] DELETE %s\n", walIndex, op.key)
		}
	}

	// WAL 저장
	wal.Save()

	fmt.Println("\n  현재 KV 상태 (스냅샷 + WAL 적용 후):")
	printStore(store)

	// 4. 크래시 시뮬레이션
	fmt.Println("\n[4] 크래시 시뮬레이션!")
	fmt.Println("──────────────────────────────────────")
	fmt.Println("  *** 서버 크래시 발생 ***")
	fmt.Println("  메모리 상의 KV 상태가 모두 손실됨")
	fmt.Printf("  남아있는 것: 스냅샷 파일 + WAL 파일\n")

	// 메모리 상태 폐기
	store = nil

	// 5. 스냅샷 + WAL 기반 복구
	fmt.Println("\n[5] 스냅샷 + WAL 기반 복구")
	fmt.Println("──────────────────────────────────────")

	// WAL 다시 로드
	recoveryWAL := NewWAL(walDir)
	recoveryWAL.Load()

	recovery := NewRecoveryEngine(snapshotter, recoveryWAL)
	recoveredStore, err := recovery.Recover(snapPath)
	if err != nil {
		fmt.Printf("  복구 실패: %v\n", err)
		return
	}

	fmt.Println("\n  복구된 KV 상태:")
	printStore(recoveredStore)

	// 6. CRC 무결성 검증 시연
	fmt.Println("\n[6] CRC 무결성 검증 - 손상 감지")
	fmt.Println("──────────────────────────────────────")
	demonstrateCRCCorruption(snapshotter, snapPath)

	// 7. 대량 데이터 스냅샷 성능
	fmt.Println("\n[7] 대량 데이터 스냅샷 성능 측정")
	fmt.Println("──────────────────────────────────────")

	largeStore := NewKVStore()
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("/data/key-%05d", i)
		value := fmt.Sprintf("value-%05d-padding-data-for-size", i)
		largeStore.Put(key, value)
	}

	start := time.Now()
	largeData, _ := largeStore.Serialize()
	serializeTime := time.Since(start)

	largeMeta := SnapshotMetadata{Term: 2, Index: 10000}
	start = time.Now()
	largePath, _ := snapshotter.Save(largeMeta, largeData)
	saveTime := time.Since(start)

	start = time.Now()
	_, err = snapshotter.Load(largePath)
	loadTime := time.Since(start)

	fmt.Printf("  키 수: 10,000\n")
	fmt.Printf("  데이터 크기: %d bytes (%.1f KB)\n", len(largeData), float64(len(largeData))/1024)
	fmt.Printf("  직렬화: %v\n", serializeTime)
	fmt.Printf("  저장: %v\n", saveTime)
	fmt.Printf("  로드+CRC검증: %v\n", loadTime)

	// 8. 스냅샷 파일 형식 분석
	fmt.Println("\n[8] 스냅샷 파일 구조")
	fmt.Println("──────────────────────────────────────")
	fmt.Println("  etcd 스냅샷 파일 구조:")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────┐")
	fmt.Println("  │ snappb.Snapshot                      │")
	fmt.Println("  ├─────────────────────────────────────┤")
	fmt.Println("  │ CRC32 (4 bytes, Castagnoli)         │")
	fmt.Println("  ├─────────────────────────────────────┤")
	fmt.Println("  │ Data (직렬화된 raftpb.Snapshot)     │")
	fmt.Println("  │  ├─ Metadata                        │")
	fmt.Println("  │  │   ├─ Term                        │")
	fmt.Println("  │  │   ├─ Index                       │")
	fmt.Println("  │  │   └─ ConfState                   │")
	fmt.Println("  │  └─ Data (KV 상태 바이트)           │")
	fmt.Println("  └─────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  파일명 형식: {term:016x}-{index:016x}.snap")

	// CRC32 값 표시
	crc := crc32.Update(0, crcTable, largeData)
	fmt.Printf("  CRC32 예시: %08x\n", crc)

	// 요약
	fmt.Println("\n==========================================================")
	fmt.Println(" 시뮬레이션 요약")
	fmt.Println("==========================================================")
	fmt.Println()
	fmt.Println("  etcd 스냅샷 시스템의 핵심 동작:")
	fmt.Println("  1. 직렬화: 전체 KV 상태를 바이트 배열로 변환")
	fmt.Println("  2. CRC32: Castagnoli 알고리즘으로 무결성 체크섬 생성")
	fmt.Println("  3. 저장: term-index.snap 파일로 디스크에 기록")
	fmt.Println("  4. 복구: 스냅샷 로드 → CRC 검증 → 상태 복원")
	fmt.Println("  5. WAL 재생: 스냅샷 이후의 WAL 엔트리를 순서대로 적용")
	fmt.Println("  6. 손상 감지: CRC 불일치 시 ErrCRCMismatch 에러")
	fmt.Println()
	fmt.Println("  참조 소스:")
	fmt.Println("  - server/etcdserver/api/snap/snapshotter.go  (스냅샷 관리)")
	fmt.Println("  - server/etcdserver/api/snap/snappb/snap.pb.go (메시지)")
	fmt.Println("  - server/storage/wal/wal.go                  (WAL)")
	fmt.Println()

	// CRC 테이블 알고리즘 확인
	fmt.Println("  CRC32 알고리즘: Castagnoli (etcd와 동일)")
}
