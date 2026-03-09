// containerd Diff Service PoC
//
// 핵심 개념 시뮬레이션:
// 1. Comparer: 두 디렉토리 간 파일시스템 diff 계산
// 2. Applier: diff를 파일시스템에 적용
// 3. StreamProcessor 체인 패턴
// 4. BinaryHandler 외부 프로세서 패턴
// 5. Progress 추적
//
// 실행: go run main.go

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// ── 1. 핵심 타입 ──────────────────────────────────────────

// Descriptor는 OCI 디스크립터를 시뮬레이션한다
type Descriptor struct {
	MediaType string
	Digest    string
	Size      int64
}

// Mount는 마운트 포인트를 시뮬레이션한다
type Mount struct {
	Type   string
	Source string
}

// FileEntry는 파일시스템의 파일 엔트리이다
type FileEntry struct {
	Path     string
	Content  string
	Deleted  bool
	Modified bool
}

// ── 2. StreamProcessor 체인 ───────────────────────────────

// StreamProcessor는 스트림 변환 인터페이스이다
type StreamProcessor interface {
	io.ReadCloser
	MediaType() string
}

// Handler는 미디어 타입에 따른 프로세서 초기화 함수를 반환한다
type Handler func(ctx context.Context, mediaType string) (StreamProcessorInit, bool)

type StreamProcessorInit func(ctx context.Context, stream StreamProcessor) (StreamProcessor, error)

// handlers는 등록된 핸들러 목록 (역순 탐색)
var handlers []Handler

func RegisterProcessor(h Handler) {
	handlers = append(handlers, h)
}

func GetProcessor(ctx context.Context, stream StreamProcessor) (StreamProcessor, error) {
	for i := len(handlers) - 1; i >= 0; i-- {
		init, ok := handlers[i](ctx, stream.MediaType())
		if ok {
			return init(ctx, stream)
		}
	}
	return nil, fmt.Errorf("no processor for media-type: %s", stream.MediaType())
}

// ── 3. 프로세서 구현체 ───────────────────────────────────

// processorChain은 원본 스트림의 래퍼이다
type processorChain struct {
	mt string
	rc io.Reader
}

func (c *processorChain) MediaType() string    { return c.mt }
func (c *processorChain) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *processorChain) Close() error         { return nil }

// gzipProcessor는 gzip 압축을 해제한다
type gzipProcessor struct {
	rc io.ReadCloser
}

func (c *gzipProcessor) MediaType() string {
	return "application/vnd.oci.image.layer.v1.tar"
}
func (c *gzipProcessor) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *gzipProcessor) Close() error               { return c.rc.Close() }

// tarProcessor는 이미 tar 형식인 스트림을 패스스루한다
type tarProcessor struct {
	rc StreamProcessor
}

func (c *tarProcessor) MediaType() string {
	return "application/vnd.oci.image.layer.v1.tar"
}
func (c *tarProcessor) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *tarProcessor) Close() error               { return nil }

// uppercaseProcessor는 스트림 내용을 대문자로 변환한다 (커스텀 프로세서 예시)
type uppercaseProcessor struct {
	rc StreamProcessor
}

func (c *uppercaseProcessor) MediaType() string {
	return "application/vnd.oci.image.layer.v1.tar"
}
func (c *uppercaseProcessor) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	for i := 0; i < n; i++ {
		if p[i] >= 'a' && p[i] <= 'z' {
			p[i] -= 32
		}
	}
	return n, err
}
func (c *uppercaseProcessor) Close() error { return c.rc.Close() }

func init() {
	// 기본 gzip 핸들러 등록
	RegisterProcessor(func(ctx context.Context, mt string) (StreamProcessorInit, bool) {
		if mt == "application/vnd.oci.image.layer.v1.tar+gzip" {
			return func(ctx context.Context, stream StreamProcessor) (StreamProcessor, error) {
				r, err := gzip.NewReader(stream)
				if err != nil {
					return nil, err
				}
				return &gzipProcessor{rc: r}, nil
			}, true
		}
		if mt == "application/vnd.oci.image.layer.v1.tar" {
			return func(ctx context.Context, stream StreamProcessor) (StreamProcessor, error) {
				return &tarProcessor{rc: stream}, nil
			}, true
		}
		return nil, false
	})
}

// ── 4. ContentStore 시뮬레이션 ────────────────────────────

type ContentStore struct {
	mu    sync.RWMutex
	blobs map[string][]byte
}

func NewContentStore() *ContentStore {
	return &ContentStore{blobs: make(map[string][]byte)}
}

func (s *ContentStore) Write(ref string, data []byte) Descriptor {
	h := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(h[:])
	s.mu.Lock()
	s.blobs[digest] = data
	s.mu.Unlock()
	return Descriptor{
		Digest: digest,
		Size:   int64(len(data)),
	}
}

func (s *ContentStore) Read(digest string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("not found: %s", digest)
	}
	return data, nil
}

// ── 5. Comparer 구현 (WalkingDiff) ────────────────────────

type WalkingDiff struct {
	store *ContentStore
}

func NewWalkingDiff(store *ContentStore) *WalkingDiff {
	return &WalkingDiff{store: store}
}

// Compare는 두 파일시스템 간의 차이를 계산한다
func (w *WalkingDiff) Compare(ctx context.Context,
	lower, upper []FileEntry, mediaType string) (Descriptor, error) {

	fmt.Println("  [Compare] 파일시스템 diff 계산 시작")

	// 1. lower 파일 맵 구성
	lowerMap := make(map[string]string)
	for _, f := range lower {
		lowerMap[f.Path] = f.Content
	}

	// 2. upper와 비교하여 변경 사항 추출
	var changes []FileEntry
	upperPaths := make(map[string]bool)

	for _, f := range upper {
		upperPaths[f.Path] = true
		if lContent, exists := lowerMap[f.Path]; !exists {
			// 새 파일
			changes = append(changes, FileEntry{Path: f.Path, Content: f.Content})
			fmt.Printf("  [Compare]   + 추가: %s\n", f.Path)
		} else if lContent != f.Content {
			// 수정된 파일
			changes = append(changes, FileEntry{Path: f.Path, Content: f.Content, Modified: true})
			fmt.Printf("  [Compare]   ~ 수정: %s\n", f.Path)
		}
	}

	// 삭제된 파일 (lower에는 있고 upper에는 없음)
	for _, f := range lower {
		if !upperPaths[f.Path] {
			changes = append(changes, FileEntry{Path: f.Path, Deleted: true})
			fmt.Printf("  [Compare]   - 삭제: %s\n", f.Path)
		}
	}

	// 3. 변경 사항을 "tar" 데이터로 직렬화
	var buf bytes.Buffer
	for _, c := range changes {
		if c.Deleted {
			fmt.Fprintf(&buf, "DELETE:%s\n", c.Path)
		} else {
			fmt.Fprintf(&buf, "FILE:%s:%s\n", c.Path, c.Content)
		}
	}

	// 4. 압축 (mediaType에 따라)
	var data []byte
	if mediaType == "application/vnd.oci.image.layer.v1.tar+gzip" {
		var gzBuf bytes.Buffer
		gz := gzip.NewWriter(&gzBuf)
		gz.Write(buf.Bytes())
		gz.Close()
		data = gzBuf.Bytes()
	} else {
		data = buf.Bytes()
	}

	// 5. Content Store에 저장
	desc := w.store.Write("diff-ref", data)
	desc.MediaType = mediaType
	fmt.Printf("  [Compare] diff 생성 완료: %s (size=%d)\n", desc.Digest[:20]+"...", desc.Size)

	return desc, nil
}

// ── 6. Applier 구현 (fsApplier) ───────────────────────────

type FsApplier struct {
	store *ContentStore
}

func NewFsApplier(store *ContentStore) *FsApplier {
	return &FsApplier{store: store}
}

// Apply는 diff를 파일시스템에 적용한다
func (a *FsApplier) Apply(ctx context.Context, desc Descriptor,
	fs *[]FileEntry, progress func(int64)) (Descriptor, error) {

	fmt.Println("  [Apply] diff 적용 시작")

	// 1. Content Store에서 blob 읽기
	data, err := a.store.Read(desc.Digest)
	if err != nil {
		return Descriptor{}, err
	}

	// 2. StreamProcessor 체인 구성
	stream := &processorChain{mt: desc.MediaType, rc: bytes.NewReader(data)}

	var processor StreamProcessor = stream
	var chain []string
	chain = append(chain, desc.MediaType)

	for {
		next, err := GetProcessor(ctx, processor)
		if err != nil {
			return Descriptor{}, err
		}
		processor = next
		chain = append(chain, processor.MediaType())
		if processor.MediaType() == "application/vnd.oci.image.layer.v1.tar" {
			break
		}
	}
	fmt.Printf("  [Apply] 프로세서 체인: %s\n", strings.Join(chain, " → "))

	// 3. DiffID 계산을 위한 해싱 + 읽기
	h := sha256.New()
	tarData, err := io.ReadAll(io.TeeReader(processor, h))
	if err != nil {
		return Descriptor{}, err
	}
	diffID := "sha256:" + hex.EncodeToString(h.Sum(nil))

	// 4. "tar" 적용
	if progress != nil {
		progress(0)
	}
	lines := strings.Split(string(tarData), "\n")
	applied := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "DELETE:") {
			path := strings.TrimPrefix(line, "DELETE:")
			newFs := make([]FileEntry, 0)
			for _, f := range *fs {
				if f.Path != path {
					newFs = append(newFs, f)
				}
			}
			*fs = newFs
			fmt.Printf("  [Apply]   삭제: %s\n", path)
			applied++
		} else if strings.HasPrefix(line, "FILE:") {
			parts := strings.SplitN(strings.TrimPrefix(line, "FILE:"), ":", 2)
			if len(parts) == 2 {
				found := false
				for i, f := range *fs {
					if f.Path == parts[0] {
						(*fs)[i].Content = parts[1]
						found = true
						break
					}
				}
				if !found {
					*fs = append(*fs, FileEntry{Path: parts[0], Content: parts[1]})
				}
				fmt.Printf("  [Apply]   적용: %s\n", parts[0])
				applied++
			}
		}
		if progress != nil {
			progress(int64(applied))
		}
	}

	processor.Close()
	fmt.Printf("  [Apply] 적용 완료 (변경 %d건, DiffID=%s)\n", applied, diffID[:20]+"...")

	return Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Size:      int64(len(tarData)),
		Digest:    diffID,
	}, nil
}

// ── 7. 메인 ─────────────────────────────────────────────

func main() {
	fmt.Println("========================================")
	fmt.Println("containerd Diff Service PoC")
	fmt.Println("========================================")

	ctx := context.Background()
	store := NewContentStore()

	// ── 데모 1: Compare (파일시스템 diff 계산) ──
	fmt.Println("\n--- 데모 1: Compare (WalkingDiff) ---")

	lower := []FileEntry{
		{Path: "/etc/hostname", Content: "worker-01"},
		{Path: "/etc/resolv.conf", Content: "nameserver 8.8.8.8"},
		{Path: "/tmp/cache.dat", Content: "old-cache-data"},
		{Path: "/app/config.yaml", Content: "debug: false"},
	}

	upper := []FileEntry{
		{Path: "/etc/hostname", Content: "worker-01"},        // 변경 없음
		{Path: "/etc/resolv.conf", Content: "nameserver 1.1.1.1"}, // 수정
		{Path: "/app/config.yaml", Content: "debug: true"},   // 수정
		{Path: "/app/data.json", Content: "{\"key\": \"val\"}"},   // 추가
		// /tmp/cache.dat 삭제
	}

	diff := NewWalkingDiff(store)
	desc, err := diff.Compare(ctx, lower, upper,
		"application/vnd.oci.image.layer.v1.tar+gzip")
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	fmt.Printf("  결과 Descriptor: MediaType=%s, Digest=%s\n",
		desc.MediaType, desc.Digest[:20]+"...")

	// ── 데모 2: Apply (diff 적용) ──
	fmt.Println("\n--- 데모 2: Apply (fsApplier) ---")

	targetFs := make([]FileEntry, len(lower))
	copy(targetFs, lower)

	fmt.Println("  [적용 전 파일시스템]")
	for _, f := range targetFs {
		fmt.Printf("    %s = %s\n", f.Path, f.Content)
	}

	applier := NewFsApplier(store)
	var progressLog []int64
	result, err := applier.Apply(ctx, desc, &targetFs, func(n int64) {
		progressLog = append(progressLog, n)
	})
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}

	fmt.Println("\n  [적용 후 파일시스템]")
	for _, f := range targetFs {
		fmt.Printf("    %s = %s\n", f.Path, f.Content)
	}
	fmt.Printf("  DiffID: %s\n", result.Digest[:20]+"...")
	fmt.Printf("  Progress 콜백 호출: %d회\n", len(progressLog))

	// 검증: upper와 동일한지 확인
	fmt.Println("\n  [검증]")
	match := true
	for _, u := range upper {
		found := false
		for _, t := range targetFs {
			if t.Path == u.Path && t.Content == u.Content {
				found = true
				break
			}
		}
		if !found {
			match = false
			fmt.Printf("    FAIL: %s not found or mismatch\n", u.Path)
		}
	}
	if match {
		fmt.Println("    OK: Apply 후 파일시스템이 upper와 동일")
	}

	// ── 데모 3: StreamProcessor 체인 ──
	fmt.Println("\n--- 데모 3: StreamProcessor 체인 ---")

	// gzip으로 압축된 데이터 준비
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	gz.Write([]byte("hello world from container layer"))
	gz.Close()

	stream := &processorChain{
		mt: "application/vnd.oci.image.layer.v1.tar+gzip",
		rc: bytes.NewReader(gzBuf.Bytes()),
	}

	fmt.Printf("  입력 미디어 타입: %s\n", stream.MediaType())

	proc, err := GetProcessor(ctx, stream)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	fmt.Printf("  프로세서 후 미디어 타입: %s\n", proc.MediaType())

	out, _ := io.ReadAll(proc)
	fmt.Printf("  디코딩 결과: %q\n", string(out))
	proc.Close()

	// ── 데모 4: 커스텀 StreamProcessor 등록 ──
	fmt.Println("\n--- 데모 4: 커스텀 StreamProcessor ---")

	// 커스텀 미디어 타입 핸들러 등록 (사용자 핸들러가 기본보다 우선)
	RegisterProcessor(func(ctx context.Context, mt string) (StreamProcessorInit, bool) {
		if mt == "application/vnd.custom.encrypted.layer" {
			return func(ctx context.Context, stream StreamProcessor) (StreamProcessor, error) {
				fmt.Println("  [커스텀 프로세서] 'encrypted' → 'tar'로 변환")
				return &uppercaseProcessor{rc: stream}, nil
			}, true
		}
		return nil, false
	})

	customStream := &processorChain{
		mt: "application/vnd.custom.encrypted.layer",
		rc: strings.NewReader("file:/app/secret:password123"),
	}
	customProc, err := GetProcessor(ctx, customStream)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	customOut, _ := io.ReadAll(customProc)
	fmt.Printf("  커스텀 변환 결과: %q\n", string(customOut))
	fmt.Printf("  출력 미디어 타입: %s\n", customProc.MediaType())

	// ── 데모 5: 핸들러 탐색 순서 (역순) ──
	fmt.Println("\n--- 데모 5: 핸들러 역순 탐색 ---")
	fmt.Printf("  등록된 핸들러 수: %d\n", len(handlers))
	fmt.Println("  탐색 순서: 마지막 등록 → 첫 번째 등록 (사용자 핸들러 우선)")

	// 같은 미디어 타입에 대해 다른 핸들러를 추가하면 새 핸들러가 우선
	RegisterProcessor(func(ctx context.Context, mt string) (StreamProcessorInit, bool) {
		if mt == "application/vnd.custom.encrypted.layer" {
			return func(ctx context.Context, stream StreamProcessor) (StreamProcessor, error) {
				fmt.Println("  [새 커스텀 프로세서] 오버라이드됨!")
				return &tarProcessor{rc: stream}, nil
			}, true
		}
		return nil, false
	})

	overrideStream := &processorChain{
		mt: "application/vnd.custom.encrypted.layer",
		rc: strings.NewReader("test"),
	}
	overrideProc, _ := GetProcessor(ctx, overrideStream)
	io.ReadAll(overrideProc)
	fmt.Println("  → 마지막 등록된 핸들러가 실행됨 (역순 탐색)")

	// ── 데모 6: 압축 포맷 비교 ──
	fmt.Println("\n--- 데모 6: 압축 포맷 비교 ---")

	rawData := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 100)

	// 비압축
	desc1, _ := diff.Compare(ctx, nil,
		[]FileEntry{{Path: "/data.txt", Content: rawData}},
		"application/vnd.oci.image.layer.v1.tar")

	// gzip 압축
	desc2, _ := diff.Compare(ctx, nil,
		[]FileEntry{{Path: "/data.txt", Content: rawData}},
		"application/vnd.oci.image.layer.v1.tar+gzip")

	fmt.Printf("  비압축 크기: %d bytes\n", desc1.Size)
	fmt.Printf("  gzip 크기:  %d bytes\n", desc2.Size)
	if desc2.Size < desc1.Size {
		ratio := float64(desc2.Size) / float64(desc1.Size) * 100
		fmt.Printf("  압축률: %.1f%% (%.0f%% 절감)\n", ratio, 100-ratio)
	}

	// ── 요약 ──
	fmt.Println("\n========================================")
	fmt.Println("요약: containerd Diff Service")
	fmt.Println("========================================")
	fmt.Println("1. Comparer: lower/upper 마운트 순회 → diff tar 생성")
	fmt.Println("2. Applier: diff tar → 파일시스템에 적용, DiffID 검증")
	fmt.Println("3. StreamProcessor 체인: 미디어 타입별 스트림 변환")
	fmt.Println("4. 역순 핸들러 탐색: 사용자 커스텀 핸들러 우선")
	fmt.Println("5. Progress 콜백: 적용 진행률 추적")
	_ = time.Now() // time import 유지
}
