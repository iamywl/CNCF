// containerd PoC-03: Content-Addressable Storage 시뮬레이션
//
// 실제 소스 참조:
//   - core/content/content.go              : Store, Provider, Ingester, IngestManager, Manager 인터페이스
//   - core/content/content.go:146          : Writer 인터페이스 — WriteCloser + Digest, Commit, Status, Truncate
//   - core/content/content.go:49           : ReaderAt 인터페이스 — io.ReaderAt + io.Closer + Size
//   - core/content/content.go:90           : Info{Digest, Size, CreatedAt, UpdatedAt, Labels}
//   - core/content/content.go:99           : Status{Ref, Offset, Total, Expected, StartedAt, UpdatedAt}
//   - plugins/content/local/store.go       : store{root, ls, locks} — 로컬 파일시스템 기반 구현
//   - plugins/content/local/store.go:79    : NewStore(root) — blobs/ + ingest/ 디렉토리 구성
//   - plugins/content/local/store.go:475   : Writer(ctx, opts) — ref 기반 트랜잭션 시작
//   - plugins/content/local/store.go:641   : blobPath(dgst) — blobs/{algorithm}/{encoded} 경로 계산
//   - plugins/content/local/store.go:649   : ingestRoot(ref) — ingest/{hash(ref)} 경로 계산
//   - plugins/content/local/writer.go      : writer{s, fp, path, ref, offset, total, digester}
//   - plugins/content/local/writer.go:71   : Write(p) — fp.Write + digester.Hash().Write + offset 갱신
//   - plugins/content/local/writer.go:79   : Commit(ctx, size, expected) — 크기/digest 검증 → rename → 권한설정
//
// 핵심 개념:
//   1. Content Store는 digest(SHA256)를 키로 사용하는 불변 blob 저장소
//   2. 쓰기(Ingestion): Writer(ref) → Write(data) → Commit(size, expectedDigest)
//   3. 읽기: ReaderAt(desc) — digest로 blob 파일에 직접 접근
//   4. 파일시스템 구조: root/blobs/sha256/{digest}, root/ingest/{hash(ref)}/data
//   5. 동일 digest의 blob은 자동으로 중복 제거 (content-addressable)
//   6. Writer는 ref로 잠금(lock) — 동시에 같은 ref로 쓰기 불가
//
// 실행: go run main.go

package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. 기본 타입 정의
// =============================================================================

// Digest는 "sha256:{hex}" 형식의 콘텐츠 해시
type Digest string

// Descriptor는 OCI 콘텐츠 디스크립터
type Descriptor struct {
	MediaType string
	Digest    Digest
	Size      int64
}

// Info는 blob의 메타데이터
// 실제: content.Info{Digest, Size, CreatedAt, UpdatedAt, Labels}
type Info struct {
	Digest    Digest
	Size      int64
	CreatedAt time.Time
	UpdatedAt time.Time
	Labels    map[string]string
}

// Status는 진행 중인 쓰기 작업의 상태
// 실제: content.Status{Ref, Offset, Total, Expected, StartedAt, UpdatedAt}
type Status struct {
	Ref       string
	Offset    int64
	Total     int64
	Expected  Digest
	StartedAt time.Time
	UpdatedAt time.Time
}

// =============================================================================
// 2. Content Store (plugins/content/local/store.go 참조)
// =============================================================================

// Store는 로컬 파일시스템 기반 Content Store
// 실제: local.store{root, ls, integritySupported, locksMu, locks, ensureIngestRootOnce}
//
// 디렉토리 구조:
//   root/
//   ├── blobs/
//   │   └── sha256/
//   │       └── {encoded_digest}     ← 커밋된 blob (읽기 전용)
//   └── ingest/
//       └── {hash(ref)}/
//           ├── ref                   ← ref 이름
//           ├── data                  ← 쓰기 중인 데이터
//           ├── startedat             ← 시작 시간
//           ├── updatedat             ← 갱신 시간
//           └── total                 ← 예상 크기 (선택)
type Store struct {
	root string

	// 실제: locksMu sync.Mutex + locks map[string]*lock
	// ref 기반 Writer 잠금 — 동시에 같은 ref로 쓰기 방지
	locksMu sync.Mutex
	locks   map[string]bool
}

// NewStore는 Content Store를 생성
// 실제: local.NewStore(root) → NewLabeledStore(root, nil)
//   - root 디렉토리 생성
//   - fsverity 지원 확인
//   - ensureIngestRootOnce 설정
func NewStore(root string) (*Store, error) {
	// blobs/sha256 디렉토리 생성
	blobDir := filepath.Join(root, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		return nil, fmt.Errorf("blobs 디렉토리 생성 실패: %w", err)
	}

	// ingest 디렉토리 생성
	ingestDir := filepath.Join(root, "ingest")
	if err := os.MkdirAll(ingestDir, 0777); err != nil {
		return nil, fmt.Errorf("ingest 디렉토리 생성 실패: %w", err)
	}

	return &Store{
		root:  root,
		locks: make(map[string]bool),
	}, nil
}

// blobPath는 digest에 대한 blob 파일 경로를 계산
// 실제: store.blobPath(dgst) → root/blobs/{algorithm}/{encoded}
func (s *Store) blobPath(dgst Digest) string {
	// "sha256:abcdef..." → "blobs/sha256/abcdef..."
	parts := strings.SplitN(string(dgst), ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return filepath.Join(s.root, "blobs", parts[0], parts[1])
}

// ingestRoot는 ref에 대한 ingest 디렉토리 경로를 계산
// 실제: store.ingestRoot(ref) → root/ingest/{digest(ref).Encoded()}
// ref를 한 번 더 해시하여 경로 길이를 일정하게 유지
func (s *Store) ingestRoot(ref string) string {
	h := sha256.Sum256([]byte(ref))
	return filepath.Join(s.root, "ingest", fmt.Sprintf("%x", h))
}

// tryLock은 ref에 대한 잠금을 시도
// 실제: store.tryLock(ref) — 실패 시 ErrUnavailable 반환
func (s *Store) tryLock(ref string) error {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if s.locks[ref] {
		return fmt.Errorf("ref %q is already locked", ref)
	}
	s.locks[ref] = true
	return nil
}

// unlock은 ref에 대한 잠금을 해제
func (s *Store) unlock(ref string) {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	delete(s.locks, ref)
}

// =============================================================================
// 3. Writer (plugins/content/local/writer.go 참조)
// =============================================================================

// Writer는 Content Store에 blob을 쓰는 트랜잭션
// 실제: local.writer{s, fp, path, ref, offset, total, digester, startedAt, updatedAt}
//
// 생명주기:
//   1. Store.Writer(ref) → Writer 생성, ingest/{hash(ref)} 디렉토리 생성
//   2. Writer.Write(data) → ingest/{hash(ref)}/data에 쓰기 + SHA256 해시 누적
//   3. Writer.Commit(size, expected) → 크기/digest 검증 → blobs/sha256/{digest}로 rename
//
// 핵심 설계:
//   - digester로 실시간 해시 계산 (별도 패스 불필요)
//   - Commit 시 ingest/data → blobs/sha256/{digest} 원자적 이동 (os.Rename)
//   - 이미 같은 digest가 있으면 ErrAlreadyExists (중복 제거)
type Writer struct {
	store     *Store
	fp        *os.File   // ingest/{hash(ref)}/data 파일
	path      string     // ingest/{hash(ref)} 디렉토리
	ref       string     // ref 식별자
	offset    int64      // 현재까지 쓴 바이트 수
	total     int64      // 예상 총 크기
	digester  *Digester  // SHA256 해시 누적기
	startedAt time.Time
	updatedAt time.Time
}

// Digester는 SHA256 해시를 누적 계산
type Digester struct {
	hash *sha256Hash
}

type sha256Hash struct {
	h     [sha256.Size]byte
	data  []byte
}

func newDigester() *Digester {
	return &Digester{hash: &sha256Hash{}}
}

func (d *Digester) Write(p []byte) {
	d.hash.data = append(d.hash.data, p...)
}

func (d *Digester) Digest() Digest {
	h := sha256.Sum256(d.hash.data)
	return Digest(fmt.Sprintf("sha256:%x", h))
}

// Writer 메서드들

// Write는 데이터를 쓴다
// 실제: writer.Write(p) — fp.Write(p) + digester.Hash().Write(p[:n]) + offset 갱신
func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.fp.Write(p)
	w.digester.Write(p[:n])
	w.offset += int64(n)
	w.updatedAt = time.Now()
	return n, err
}

// Digest는 현재까지의 digest를 반환
// 실제: writer.Digest() → digester.Digest()
func (w *Writer) Digest() Digest {
	return w.digester.Digest()
}

// Status는 현재 쓰기 상태를 반환
// 실제: writer.Status() → content.Status{Ref, Offset, Total, StartedAt, UpdatedAt}
func (w *Writer) Status() Status {
	return Status{
		Ref:       w.ref,
		Offset:    w.offset,
		Total:     w.total,
		StartedAt: w.startedAt,
		UpdatedAt: w.updatedAt,
	}
}

// Commit은 blob을 확정한다
// 실제 흐름 (writer.go:79 Commit):
//   1. fp.Sync() → 디스크에 플러시
//   2. fp.Stat() → 실제 크기 확인
//   3. size 검증: size > 0 && size != fi.Size() → 에러
//   4. digest 검증: expected != "" && expected != computed → 에러
//   5. os.MkdirAll(blobDir) → blob 디렉토리 보장
//   6. os.Stat(target) → 이미 존재하면 ErrAlreadyExists (중복 제거)
//   7. os.Rename(ingest/data, blobs/sha256/{digest}) → 원자적 이동
//   8. os.Chmod(target, readonly) → 읽기 전용으로 변경
//   9. os.RemoveAll(ingestPath) → ingest 디렉토리 정리
func (w *Writer) Commit(size int64, expected Digest) error {
	defer w.store.unlock(w.ref)

	// 파일 플러시 및 닫기
	w.fp.Sync()
	fi, err := w.fp.Stat()
	w.fp.Close()
	w.fp = nil

	if err != nil {
		return fmt.Errorf("stat 실패: %w", err)
	}

	// 크기 검증
	// 실제: size > 0 && size != fi.Size() → ErrFailedPrecondition
	if size > 0 && size != fi.Size() {
		return fmt.Errorf("크기 불일치: expected=%d, actual=%d", size, fi.Size())
	}

	// digest 계산 및 검증
	dgst := w.digester.Digest()
	if expected != "" && expected != dgst {
		return fmt.Errorf("digest 불일치: expected=%s, actual=%s", expected, dgst)
	}

	// blob 경로 계산
	ingestData := filepath.Join(w.path, "data")
	target := w.store.blobPath(dgst)

	// blob 디렉토리 생성
	// 실제: os.MkdirAll(filepath.Dir(target), 0755)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}

	// 중복 체크 — 이미 같은 digest가 있으면 ingest 정리 후 에러
	// 실제: os.Stat(target) → err == nil → ErrAlreadyExists
	if _, err := os.Stat(target); err == nil {
		os.RemoveAll(w.path) // ingest 디렉토리 정리
		return fmt.Errorf("content %s: already exists", dgst)
	}

	// 원자적 이동: ingest/data → blobs/sha256/{digest}
	// 실제: os.Rename(ingest, target)
	if err := os.Rename(ingestData, target); err != nil {
		return fmt.Errorf("rename 실패: %w", err)
	}

	// 읽기 전용으로 변경
	// 실제: os.Chmod(target, (fi.Mode()&os.ModePerm)&^0333)
	os.Chmod(target, 0444)

	// ingest 디렉토리 정리
	// 실제: os.RemoveAll(w.path)
	os.RemoveAll(w.path)

	fmt.Printf("    Committed: %s (%d bytes)\n", dgst, fi.Size())
	return nil
}

// Close는 Writer를 닫는다 (Commit 없이)
// 실제: writer.Close() → fp.Sync() + fp.Close() + unlock
func (w *Writer) Close() error {
	if w.fp != nil {
		w.fp.Sync()
		err := w.fp.Close()
		w.fp = nil
		w.store.unlock(w.ref)
		return err
	}
	return nil
}

// =============================================================================
// 4. Store의 Writer/ReaderAt/Info 메서드
// =============================================================================

// Writer는 새 쓰기 트랜잭션을 시작
// 실제: store.Writer(ctx, opts...WriterOpt) → Writer
//
// 흐름:
//   1. ref 추출 (WriterOpts.Ref — 필수)
//   2. tryLock(ref) — 이미 사용 중이면 에러
//   3. expected digest가 이미 존재하면 ErrAlreadyExists
//   4. ingest 디렉토리 생성 (mkdir) — 이미 있으면 resume 시도
//   5. ref, startedat, updatedat 파일 작성
//   6. data 파일 열기 + Writer 구조체 반환
func (s *Store) Writer(ref string, total int64, expected Digest) (*Writer, error) {
	if ref == "" {
		return nil, fmt.Errorf("ref must not be empty")
	}

	// ref 잠금
	if err := s.tryLock(ref); err != nil {
		return nil, err
	}

	// expected digest가 이미 존재하는지 확인
	// 실제: store.writer() — os.Stat(blobPath(expected)) → ErrAlreadyExists
	if expected != "" {
		if _, err := os.Stat(s.blobPath(expected)); err == nil {
			s.unlock(ref)
			return nil, fmt.Errorf("content %s: already exists", expected)
		}
	}

	// ingest 디렉토리 생성
	ingestPath := s.ingestRoot(ref)
	if err := os.MkdirAll(ingestPath, 0755); err != nil {
		s.unlock(ref)
		return nil, err
	}

	// ref 파일 작성
	// 실제: os.WriteFile(refp, []byte(ref), 0666)
	os.WriteFile(filepath.Join(ingestPath, "ref"), []byte(ref), 0666)

	// 시간 기록
	now := time.Now()
	os.WriteFile(filepath.Join(ingestPath, "startedat"), []byte(now.Format(time.RFC3339Nano)), 0666)
	os.WriteFile(filepath.Join(ingestPath, "updatedat"), []byte(now.Format(time.RFC3339Nano)), 0666)

	// total 기록
	if total > 0 {
		os.WriteFile(filepath.Join(ingestPath, "total"), []byte(fmt.Sprint(total)), 0666)
	}

	// data 파일 열기
	dataPath := filepath.Join(ingestPath, "data")
	fp, err := os.OpenFile(dataPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		s.unlock(ref)
		return nil, fmt.Errorf("data 파일 생성 실패: %w", err)
	}

	return &Writer{
		store:     s,
		fp:        fp,
		path:      ingestPath,
		ref:       ref,
		offset:    0,
		total:     total,
		digester:  newDigester(),
		startedAt: now,
		updatedAt: now,
	}, nil
}

// ReaderAt는 digest로 blob을 읽는 ReaderAt을 반환
// 실제: store.ReaderAt(ctx, desc) → OpenReader(blobPath(desc.Digest))
func (s *Store) ReaderAt(dgst Digest) (*os.File, int64, error) {
	p := s.blobPath(dgst)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("content %s: not found", dgst)
		}
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// GetInfo는 blob의 메타데이터를 반환
// 실제: store.Info(ctx, dgst) → os.Stat(blobPath(dgst)) → Info
func (s *Store) GetInfo(dgst Digest) (Info, error) {
	p := s.blobPath(dgst)
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Info{}, fmt.Errorf("content %s: not found", dgst)
		}
		return Info{}, err
	}
	return Info{
		Digest:    dgst,
		Size:      fi.Size(),
		CreatedAt: fi.ModTime(),
		UpdatedAt: fi.ModTime(),
	}, nil
}

// Delete는 blob을 삭제
// 실제: store.Delete(ctx, dgst) → os.RemoveAll(blobPath(dgst))
func (s *Store) Delete(dgst Digest) error {
	p := s.blobPath(dgst)
	return os.RemoveAll(p)
}

// Walk는 모든 blob을 순회
// 실제: store.Walk(ctx, fn, filters...) → filepath.Walk(blobs/)
func (s *Store) Walk(fn func(Info) error) error {
	blobRoot := filepath.Join(s.root, "blobs", "sha256")
	entries, err := os.ReadDir(blobRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fi, _ := entry.Info()
		dgst := Digest("sha256:" + entry.Name())
		info := Info{
			Digest:    dgst,
			Size:      fi.Size(),
			CreatedAt: fi.ModTime(),
			UpdatedAt: fi.ModTime(),
		}
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

// ListStatuses는 진행 중인 쓰기 작업을 나열
// 실제: store.ListStatuses(ctx, filters...) → os.Open(ingest/) + Readdirnames
func (s *Store) ListStatuses() ([]Status, error) {
	ingestDir := filepath.Join(s.root, "ingest")
	entries, err := os.ReadDir(ingestDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var statuses []Status
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		p := filepath.Join(ingestDir, entry.Name())
		refBytes, err := os.ReadFile(filepath.Join(p, "ref"))
		if err != nil {
			continue
		}
		dataFi, err := os.Stat(filepath.Join(p, "data"))
		if err != nil {
			continue
		}
		statuses = append(statuses, Status{
			Ref:       string(refBytes),
			Offset:    dataFi.Size(),
			UpdatedAt: dataFi.ModTime(),
		})
	}
	return statuses, nil
}

// Abort는 진행 중인 쓰기를 취소
// 실제: store.Abort(ctx, ref) → os.RemoveAll(ingestRoot(ref))
func (s *Store) Abort(ref string) error {
	root := s.ingestRoot(ref)
	return os.RemoveAll(root)
}

// =============================================================================
// 5. 시뮬레이션
// =============================================================================

func computeDigest(data []byte) Digest {
	h := sha256.Sum256(data)
	return Digest(fmt.Sprintf("sha256:%x", h))
}

func main() {
	fmt.Println("=" + strings.Repeat("=", 69))
	fmt.Println("containerd Content-Addressable Storage 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 69))

	// 임시 디렉토리에 Content Store 생성
	tmpDir, err := os.MkdirTemp("", "containerd-content-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(tmpDir)
	if err != nil {
		fmt.Printf("Store 생성 실패: %v\n", err)
		return
	}

	// =====================================================================
	// 1. 파일시스템 구조 출력
	// =====================================================================
	fmt.Println("\n[1] Content Store 파일시스템 구조")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("  root: %s\n", tmpDir)
	fmt.Println("  실제 containerd 구조:")
	fmt.Println("  root/")
	fmt.Println("  ├── blobs/")
	fmt.Println("  │   └── sha256/")
	fmt.Println("  │       ├── {manifest_digest}   (readonly)")
	fmt.Println("  │       ├── {config_digest}     (readonly)")
	fmt.Println("  │       └── {layer_digest}      (readonly)")
	fmt.Println("  └── ingest/")
	fmt.Println("      └── {hash(ref)}/")
	fmt.Println("          ├── ref        (참조 이름)")
	fmt.Println("          ├── data       (쓰기 중인 데이터)")
	fmt.Println("          ├── startedat  (시작 시간)")
	fmt.Println("          ├── updatedat  (갱신 시간)")
	fmt.Println("          └── total      (예상 크기)")

	// =====================================================================
	// 2. Blob 쓰기 — Writer 생명주기
	// =====================================================================
	fmt.Println("\n[2] Blob 쓰기 — Writer 생명주기")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  흐름: Writer(ref) → Write(data) → Commit(size, expected)")

	// 이미지 config 쓰기
	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:abc123"]}}`)
	configDigest := computeDigest(configData)
	fmt.Printf("\n  [2a] Config blob 쓰기 (ref='config-upload')\n")
	fmt.Printf("    데이터: %s\n", string(configData[:50])+"...")
	fmt.Printf("    예상 digest: %s\n", configDigest)

	w, err := store.Writer("config-upload", int64(len(configData)), "")
	if err != nil {
		fmt.Printf("    Writer 생성 실패: %v\n", err)
		return
	}

	// 진행 중 상태 확인
	fmt.Printf("    Writer 상태 (쓰기 전): offset=%d, total=%d\n", w.Status().Offset, w.Status().Total)

	// 청크 단위 쓰기 (실제에서는 io.CopyBuffer 사용 권장)
	chunkSize := 40
	for i := 0; i < len(configData); i += chunkSize {
		end := i + chunkSize
		if end > len(configData) {
			end = len(configData)
		}
		w.Write(configData[i:end])
	}
	fmt.Printf("    Writer 상태 (쓰기 후): offset=%d, digest=%s\n", w.Status().Offset, w.Digest())

	// Commit
	fmt.Printf("    Commit(size=%d, expected=%s)\n", len(configData), configDigest)
	if err := w.Commit(int64(len(configData)), configDigest); err != nil {
		fmt.Printf("    Commit 실패: %v\n", err)
		return
	}

	// 이미지 레이어 쓰기
	layerData := []byte("simulated layer tarball content: /usr/share/nginx/html/index.html\nHello from containerd!")
	layerDigest := computeDigest(layerData)
	fmt.Printf("\n  [2b] Layer blob 쓰기 (ref='layer-0-upload')\n")
	fmt.Printf("    데이터 크기: %d bytes\n", len(layerData))

	w2, err := store.Writer("layer-0-upload", int64(len(layerData)), "")
	if err != nil {
		fmt.Printf("    Writer 생성 실패: %v\n", err)
		return
	}
	w2.Write(layerData)
	if err := w2.Commit(int64(len(layerData)), layerDigest); err != nil {
		fmt.Printf("    Commit 실패: %v\n", err)
		return
	}

	// Manifest 쓰기
	manifestJSON := fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"%s","size":%d},"layers":[{"digest":"%s","size":%d}]}`,
		configDigest, len(configData), layerDigest, len(layerData))
	manifestData := []byte(manifestJSON)
	manifestDigest := computeDigest(manifestData)
	fmt.Printf("\n  [2c] Manifest blob 쓰기 (ref='manifest-upload')\n")

	w3, err := store.Writer("manifest-upload", int64(len(manifestData)), "")
	if err != nil {
		fmt.Printf("    Writer 생성 실패: %v\n", err)
		return
	}
	w3.Write(manifestData)
	if err := w3.Commit(int64(len(manifestData)), manifestDigest); err != nil {
		fmt.Printf("    Commit 실패: %v\n", err)
		return
	}

	// =====================================================================
	// 3. Blob 읽기 — ReaderAt
	// =====================================================================
	fmt.Println("\n[3] Blob 읽기 — ReaderAt")
	fmt.Println(strings.Repeat("-", 60))

	for _, tc := range []struct {
		name   string
		digest Digest
	}{
		{"Config", configDigest},
		{"Layer", layerDigest},
		{"Manifest", manifestDigest},
	} {
		f, size, err := store.ReaderAt(tc.digest)
		if err != nil {
			fmt.Printf("  %s 읽기 실패: %v\n", tc.name, err)
			continue
		}

		data := make([]byte, size)
		n, _ := f.ReadAt(data, 0)
		f.Close()

		preview := string(data[:n])
		if len(preview) > 60 {
			preview = preview[:60] + "..."
		}
		fmt.Printf("  %s: size=%d, digest=%s\n", tc.name, size, tc.digest[:50]+"...")
		fmt.Printf("    내용: %s\n", preview)
	}

	// =====================================================================
	// 4. Info 조회
	// =====================================================================
	fmt.Println("\n[4] Info 조회")
	fmt.Println(strings.Repeat("-", 60))

	info, err := store.GetInfo(configDigest)
	if err != nil {
		fmt.Printf("  Info 조회 실패: %v\n", err)
	} else {
		fmt.Printf("  Config Info:\n")
		fmt.Printf("    Digest:    %s\n", info.Digest)
		fmt.Printf("    Size:      %d bytes\n", info.Size)
		fmt.Printf("    CreatedAt: %s\n", info.CreatedAt.Format(time.RFC3339))
	}

	// =====================================================================
	// 5. Walk — 모든 blob 순회
	// =====================================================================
	fmt.Println("\n[5] Walk — 모든 blob 순회")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println("  ┌────┬──────────┬──────────────────────────────────────────────────┐")
	fmt.Println("  │ #  │ Size     │ Digest                                           │")
	fmt.Println("  ├────┼──────────┼──────────────────────────────────────────────────┤")
	count := 0
	store.Walk(func(info Info) error {
		count++
		dgst := string(info.Digest)
		if len(dgst) > 48 {
			dgst = dgst[:48] + "..."
		}
		fmt.Printf("  │ %d  │ %8d │ %-48s │\n", count, info.Size, dgst)
		return nil
	})
	fmt.Println("  └────┴──────────┴──────────────────────────────────────────────────┘")

	// =====================================================================
	// 6. 중복 제거 (Deduplication) 시연
	// =====================================================================
	fmt.Println("\n[6] 중복 제거 (Deduplication)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  같은 내용의 blob을 다시 쓰기 시도:")

	w4, err := store.Writer("duplicate-config", int64(len(configData)), configDigest)
	if err != nil {
		// expected digest가 이미 존재하면 Writer 생성 단계에서 거부
		fmt.Printf("  → Writer 생성 거부: %v\n", err)
		fmt.Println("  → 중복 감지: expected digest가 이미 blob에 존재")
	} else {
		// Writer는 생성되었지만 Commit 시 중복 감지될 수 있음
		w4.Write(configData)
		if err := w4.Commit(int64(len(configData)), configDigest); err != nil {
			fmt.Printf("  → Commit 거부: %v\n", err)
		}
	}

	fmt.Println("\n  Content-Addressable 저장의 장점:")
	fmt.Println("  - 같은 내용 → 같은 digest → 자동 중복 제거")
	fmt.Println("  - 여러 이미지가 같은 base layer를 공유 가능")
	fmt.Println("  - digest로 무결성 검증 내장")

	// =====================================================================
	// 7. Digest 검증 실패 시나리오
	// =====================================================================
	fmt.Println("\n[7] Digest 검증 실패 시나리오")
	fmt.Println(strings.Repeat("-", 60))

	badData := []byte("corrupted data")
	w5, _ := store.Writer("bad-upload", int64(len(badData)), "")
	w5.Write(badData)

	// 잘못된 expected digest로 Commit 시도
	wrongDigest := Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")
	err = w5.Commit(int64(len(badData)), wrongDigest)
	if err != nil {
		fmt.Printf("  Commit 거부: %v\n", err)
		fmt.Println("  → digest 불일치: 데이터 무결성 보호")
	}

	// =====================================================================
	// 8. 크기 검증 실패 시나리오
	// =====================================================================
	fmt.Println("\n[8] 크기 검증 실패 시나리오")
	fmt.Println(strings.Repeat("-", 60))

	smallData := []byte("small")
	w6, _ := store.Writer("size-mismatch", int64(len(smallData)), "")
	w6.Write(smallData)

	// 잘못된 크기로 Commit 시도
	err = w6.Commit(9999, "") // 실제 크기는 5
	if err != nil {
		fmt.Printf("  Commit 거부: %v\n", err)
		fmt.Println("  → 크기 불일치: 전송 중 손실 감지")
	}

	// =====================================================================
	// 9. Abort — 쓰기 취소
	// =====================================================================
	fmt.Println("\n[9] Abort — 쓰기 취소")
	fmt.Println(strings.Repeat("-", 60))

	w7, _ := store.Writer("abort-test", 1000, "")
	w7.Write([]byte("partial data"))
	w7.Close() // Commit 없이 닫기

	fmt.Println("  Writer를 Commit 없이 Close")
	statuses, _ := store.ListStatuses()
	fmt.Printf("  활성 ingestion 수: %d\n", len(statuses))

	// Abort로 정리
	store.Abort("abort-test")
	statuses, _ = store.ListStatuses()
	fmt.Printf("  Abort 후 활성 ingestion 수: %d\n", len(statuses))

	// =====================================================================
	// 10. Delete — blob 삭제
	// =====================================================================
	fmt.Println("\n[10] Delete — blob 삭제")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Printf("  삭제 전 blob 수: ")
	blobCount := 0
	store.Walk(func(Info) error { blobCount++; return nil })
	fmt.Printf("%d\n", blobCount)

	store.Delete(layerDigest)
	fmt.Printf("  Layer blob 삭제 후: ")
	blobCount = 0
	store.Walk(func(Info) error { blobCount++; return nil })
	fmt.Printf("%d\n", blobCount)

	// 삭제된 blob 읽기 시도
	_, _, err = store.ReaderAt(layerDigest)
	if err != nil {
		fmt.Printf("  삭제된 blob 읽기: %v\n", err)
	}

	// =====================================================================
	// 11. 동시 쓰기 잠금 시연
	// =====================================================================
	fmt.Println("\n[11] 동시 쓰기 잠금 (ref 기반)")
	fmt.Println(strings.Repeat("-", 60))

	w8, _ := store.Writer("concurrent-ref", 100, "")

	// 같은 ref로 두 번째 Writer 시도
	_, err = store.Writer("concurrent-ref", 100, "")
	if err != nil {
		fmt.Printf("  두 번째 Writer 거부: %v\n", err)
		fmt.Println("  → 같은 ref로 동시 쓰기 방지 (실제: sync.Mutex 기반 lock)")
	}
	w8.Close()

	// 잠금 해제 후 재시도
	w9, err := store.Writer("concurrent-ref", 100, "")
	if err != nil {
		fmt.Printf("  잠금 해제 후에도 실패: %v\n", err)
	} else {
		fmt.Println("  잠금 해제 후 재시도: 성공")
		w9.Close()
	}

	// =====================================================================
	// 12. 실제 파일시스템 확인
	// =====================================================================
	fmt.Println("\n[12] 실제 파일시스템 상태")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println("  blobs/sha256/ 디렉토리:")
	blobDir := filepath.Join(tmpDir, "blobs", "sha256")
	entries, _ := os.ReadDir(blobDir)
	for _, e := range entries {
		fi, _ := e.Info()
		fmt.Printf("    %s  %6d bytes  %s\n", fi.Mode(), fi.Size(), e.Name()[:32]+"...")
	}

	fmt.Println("\n  ingest/ 디렉토리:")
	ingestDir := filepath.Join(tmpDir, "ingest")
	entries, _ = os.ReadDir(ingestDir)
	if len(entries) == 0 {
		fmt.Println("    (비어있음 — 모든 ingestion 완료/정리됨)")
	}
	for _, e := range entries {
		fmt.Printf("    %s/\n", e.Name())
		subEntries, _ := os.ReadDir(filepath.Join(ingestDir, e.Name()))
		for _, se := range subEntries {
			fi, _ := se.Info()
			content, _ := os.ReadFile(filepath.Join(ingestDir, e.Name(), se.Name()))
			preview := string(content)
			if len(preview) > 40 {
				preview = preview[:40] + "..."
			}
			fmt.Printf("      %-12s %6d bytes  %s\n", se.Name(), fi.Size(), preview)
		}
	}

	// =====================================================================
	// 요약
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("Content Store 인터페이스 요약")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println(`
  실제 content.Store 인터페이스 (core/content/content.go:41):
  ┌─────────────────────────────────────────────────────────┐
  │ Store = Manager + Provider + IngestManager + Ingester   │
  ├─────────────────────────────────────────────────────────┤
  │ Provider:                                               │
  │   ReaderAt(ctx, desc) → (ReaderAt, error)               │
  │                                                         │
  │ Ingester:                                               │
  │   Writer(ctx, opts) → (Writer, error)                   │
  │                                                         │
  │ IngestManager:                                          │
  │   Status(ctx, ref) → (Status, error)                    │
  │   ListStatuses(ctx, filters) → ([]Status, error)        │
  │   Abort(ctx, ref) → error                               │
  │                                                         │
  │ Manager (= InfoProvider + ...):                         │
  │   Info(ctx, dgst) → (Info, error)                       │
  │   Update(ctx, info, fieldpaths) → (Info, error)         │
  │   Walk(ctx, fn, filters) → error                        │
  │   Delete(ctx, dgst) → error                             │
  ├─────────────────────────────────────────────────────────┤
  │ Writer:                                                 │
  │   io.WriteCloser                                        │
  │   Digest() → digest.Digest                              │
  │   Commit(ctx, size, expected, opts) → error             │
  │   Status() → (Status, error)                            │
  │   Truncate(size) → error                                │
  └─────────────────────────────────────────────────────────┘`)

	// 다이어그램으로 완전한 라이프사이클 시각화
	fmt.Println()
	printLifecycle()

	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("containerd Content Store PoC 완료")
	fmt.Println(strings.Repeat("=", 70))
}

func printLifecycle() {
	fmt.Println("  Blob 라이프사이클:")
	fmt.Println()
	fmt.Println("  [Client]")
	fmt.Println("     │")
	fmt.Println("     │ Writer(ref='pull-layer-0')")
	fmt.Println("     ▼")
	fmt.Println("  [Ingest]  ingest/{hash(ref)}/")
	fmt.Println("     │      ├── ref       = 'pull-layer-0'")
	fmt.Println("     │      ├── data      = <쓰기 중>")
	fmt.Println("     │      ├── startedat = 2024-01-01T...")
	fmt.Println("     │      └── total     = 1048576")
	fmt.Println("     │")
	fmt.Println("     │ Write(chunk1) → Write(chunk2) → ...")
	fmt.Println("     │ (digester가 실시간 SHA256 해시 누적)")
	fmt.Println("     │")
	fmt.Println("     │ Commit(size=1048576, expected='sha256:abc...')")
	fmt.Println("     │   1. fp.Sync() → 디스크 플러시")
	fmt.Println("     │   2. size 검증, digest 검증")
	fmt.Println("     │   3. os.Rename(ingest/data → blobs/sha256/{digest})")
	fmt.Println("     │   4. os.Chmod(0444) → 읽기 전용")
	fmt.Println("     │   5. os.RemoveAll(ingest/) → 정리")
	fmt.Println("     ▼")
	fmt.Println("  [Blob]    blobs/sha256/{digest}  (immutable, readonly)")
	fmt.Println("     │")
	fmt.Println("     │ ReaderAt(desc{Digest}) → io.ReaderAt")
	fmt.Println("     ▼")
	fmt.Println("  [Reader]  직접 파일 읽기")

	// io.Copy 패턴 설명
	fmt.Println()
	fmt.Println("  실제 사용 패턴 (io.Copy):")
	fmt.Println("    w, _ := store.Writer(ctx, content.WithRef(ref))")
	fmt.Println("    io.Copy(w, httpResponse.Body)  // 네트워크 → Writer")
	fmt.Println("    w.Commit(ctx, size, expected)   // 검증 + 확정")
}

// io.ReadAt 인터페이스 시연을 위한 도우미
func readAt(f io.ReaderAt, offset int64, size int) string {
	buf := make([]byte, size)
	n, _ := f.ReadAt(buf, offset)
	return string(buf[:n])
}
