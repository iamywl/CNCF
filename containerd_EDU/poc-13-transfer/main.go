package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// containerd 이미지 전송(Transfer) 서비스 시뮬레이션
// =============================================================================
//
// containerd v2의 Transfer 서비스는 이미지 Pull/Push/Import/Export를 통합하는
// 추상 계층이다. 소스와 대상의 타입 조합에 따라 적절한 전송 로직을 선택한다.
//
// 핵심 인터페이스:
//   - Transferrer: Transfer(src, dest) — 소스/대상 조합으로 전송 수행
//   - ImageResolver: Resolve(ctx) → (name, Descriptor) — 이미지 참조를 디스크립터로 변환
//   - ImageFetcher: Fetcher(ctx, ref) → Fetcher — blob 다운로더 생성
//   - ImagePusher: Pusher(ctx, desc) → Pusher — blob 업로더 생성
//   - ProgressFunc: 진행률 콜백
//
// 실제 코드 참조:
//   - core/transfer/transfer.go        — Transferrer, ImageResolver, Fetcher, Pusher 인터페이스
//   - core/transfer/local/transfer.go  — localTransferService 구현체
//   - core/transfer/local/pull.go      — pull 흐름 (Resolve → Fetch → Store)
//   - core/images/handlers.go          — Handler, Dispatch (병렬 처리)
// =============================================================================

// --- OCI Descriptor 시뮬레이션 ---
// 실제 코드: github.com/opencontainers/image-spec/specs-go/v1.Descriptor

// Descriptor는 OCI 콘텐츠 디스크립터를 시뮬레이션한다.
// containerd에서 모든 콘텐츠는 Descriptor(mediaType, digest, size)로 식별된다.
type Descriptor struct {
	MediaType string
	Digest    string
	Size      int64
	Platform  *Platform
}

// Platform은 OCI 플랫폼 정보를 나타낸다.
type Platform struct {
	OS           string
	Architecture string
}

func (d Descriptor) String() string {
	return fmt.Sprintf("{%s %s %d}", d.MediaType, d.Digest[:16], d.Size)
}

// --- Progress 구조체 ---
// 실제 코드: core/transfer/transfer.go - Progress struct
// 전송 진행 상황을 추적하기 위한 이벤트 구조체이다.

type Progress struct {
	Event    string      // 이벤트 종류: "resolving", "downloading", "uploading", "saved" 등
	Name     string      // 대상 이름 (이미지 참조)
	Parents  []string    // 부모 객체 이름 목록
	Progress int64       // 현재까지 전송된 바이트
	Total    int64       // 전체 바이트
	Desc     *Descriptor // 대상 디스크립터
}

type ProgressFunc func(Progress)

// --- Transferrer 인터페이스 ---
// 실제 코드: core/transfer/transfer.go - Transferrer interface
// Transfer(src, dest)로 소스/대상 타입 조합에 따라 적절한 전송 로직을 선택한다.
// 실제 구현에서는 type switch로 ImageFetcher→ImageStorer (pull),
// ImageGetter→ImagePusher (push) 등의 조합을 처리한다.

type Transferrer interface {
	Transfer(ctx context.Context, source interface{}, destination interface{}, opts ...TransferOpt) error
}

// TransferConfig는 전송 옵션을 담는 설정 구조체이다.
type TransferConfig struct {
	Progress ProgressFunc
}

type TransferOpt func(*TransferConfig)

// WithProgress는 진행률 콜백을 설정한다.
// 실제 코드: core/transfer/transfer.go - WithProgress
func WithProgress(f ProgressFunc) TransferOpt {
	return func(c *TransferConfig) {
		c.Progress = f
	}
}

// --- ImageResolver 인터페이스 ---
// 실제 코드: core/transfer/transfer.go - ImageResolver interface
// 이미지 참조(docker.io/library/nginx:latest)를 OCI Descriptor로 해석한다.

type ImageResolver interface {
	Resolve(ctx context.Context) (name string, desc Descriptor, err error)
}

// --- Fetcher / Pusher 인터페이스 ---
// 실제 코드: core/transfer/transfer.go - Fetcher, Pusher interface
// Fetcher는 원격 레지스트리에서 blob을 읽어오고,
// Pusher는 원격 레지스트리에 blob을 쓴다.

type Fetcher interface {
	Fetch(ctx context.Context, desc Descriptor) (io.ReadCloser, error)
}

type Pusher interface {
	Push(ctx context.Context, desc Descriptor) (io.WriteCloser, error)
}

// --- ImageFetcher / ImagePusher 인터페이스 ---
// 실제 코드: core/transfer/transfer.go - ImageFetcher, ImagePusher interface

type ImageFetcher interface {
	ImageResolver
	Fetcher(ctx context.Context, ref string) (Fetcher, error)
}

type ImagePusher interface {
	Pusher(ctx context.Context, desc Descriptor) (Pusher, error)
}

// --- ImageStorer 인터페이스 ---
// 실제 코드: core/transfer/transfer.go - ImageStorer interface
// Pull 결과를 이미지 스토어에 저장한다.

type ImageStorer interface {
	Store(ctx context.Context, desc Descriptor) (string, error)
}

// --- Content Store 시뮬레이션 ---
// 실제 코드: core/content/store.go
// containerd의 Content Store는 content-addressable storage이다.
// Blob은 digest로 식별되며, Writer로 쓰고 ReaderAt으로 읽는다.

type ContentStore struct {
	mu    sync.RWMutex
	blobs map[string][]byte // digest → data
}

func NewContentStore() *ContentStore {
	return &ContentStore{blobs: make(map[string][]byte)}
}

func (cs *ContentStore) Exists(digest string) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	_, ok := cs.blobs[digest]
	return ok
}

func (cs *ContentStore) Write(digest string, data []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.blobs[digest] = data
}

func (cs *ContentStore) Get(digest string) ([]byte, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	data, ok := cs.blobs[digest]
	return data, ok
}

// --- 레지스트리 시뮬레이션 ---
// HTTP 기반 OCI 레지스트리를 시뮬레이션한다.
// 실제로는 remotes.Fetch()가 HTTP GET으로 blob을 다운로드한다.
// 참조: core/remotes/docker/fetcher.go

type Registry struct {
	mu     sync.RWMutex
	images map[string]ImageManifest // ref → manifest
	blobs  map[string][]byte       // digest → data
}

type ImageManifest struct {
	Name      string
	Config    Descriptor
	Layers    []Descriptor
	MediaType string
}

func NewRegistry() *Registry {
	return &Registry{
		images: make(map[string]ImageManifest),
		blobs:  make(map[string][]byte),
	}
}

// AddImage는 레지스트리에 테스트용 이미지를 등록한다.
func (r *Registry) AddImage(ref string, manifest ImageManifest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images[ref] = manifest

	// Config blob 등록
	configData := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`))
	r.blobs[manifest.Config.Digest] = configData

	// Layer blob 등록 (시뮬레이션 데이터)
	for _, layer := range manifest.Layers {
		data := make([]byte, layer.Size)
		rand.Read(data)
		r.blobs[layer.Digest] = data
	}
}

// --- RegistryFetcher 구현 ---
// ImageFetcher를 구현하여 레지스트리에서 이미지를 가져오는 역할을 한다.
// 실제 코드에서는 docker.NewResolver()가 레지스트리 접근을 처리한다.

type RegistryFetcher struct {
	registry *Registry
	ref      string
}

func NewRegistryFetcher(registry *Registry, ref string) *RegistryFetcher {
	return &RegistryFetcher{registry: registry, ref: ref}
}

func (rf *RegistryFetcher) String() string {
	return rf.ref
}

// Resolve는 이미지 참조를 Descriptor로 변환한다.
// 실제 코드: core/transfer/local/pull.go 라인 60 — ir.Resolve(ctx)
func (rf *RegistryFetcher) Resolve(ctx context.Context) (string, Descriptor, error) {
	rf.registry.mu.RLock()
	defer rf.registry.mu.RUnlock()

	manifest, ok := rf.registry.images[rf.ref]
	if !ok {
		return "", Descriptor{}, fmt.Errorf("image not found: %s", rf.ref)
	}

	// Manifest 자체의 digest 생성
	manifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(rf.ref)))
	desc := Descriptor{
		MediaType: manifest.MediaType,
		Digest:    manifestDigest,
		Size:      1024,
	}

	return manifest.Name, desc, nil
}

// Fetcher는 blob을 가져오는 Fetcher를 반환한다.
// 실제 코드: core/transfer/local/pull.go 라인 106 — ir.Fetcher(ctx, name)
func (rf *RegistryFetcher) Fetcher(ctx context.Context, ref string) (Fetcher, error) {
	return &blobFetcher{registry: rf.registry}, nil
}

// blobFetcher는 개별 blob을 가져오는 Fetcher 구현체이다.
type blobFetcher struct {
	registry *Registry
}

// Fetch는 Descriptor에 해당하는 blob을 읽어온다.
// 실제 코드: core/transfer/transfer.go - Fetcher.Fetch
// HTTP GET 요청을 시뮬레이션하며, 네트워크 지연을 모방한다.
func (bf *blobFetcher) Fetch(ctx context.Context, desc Descriptor) (io.ReadCloser, error) {
	bf.registry.mu.RLock()
	data, ok := bf.registry.blobs[desc.Digest]
	bf.registry.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("blob not found: %s", desc.Digest)
	}

	// 네트워크 지연 시뮬레이션 (실제로는 HTTP GET)
	time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)

	return io.NopCloser(strings.NewReader(string(data))), nil
}

// --- RegistryPusher 구현 ---
// ImagePusher를 구현하여 레지스트리에 이미지를 올린다.

type RegistryPusher struct {
	registry *Registry
}

func (rp *RegistryPusher) Pusher(ctx context.Context, desc Descriptor) (Pusher, error) {
	return &blobPusher{registry: rp.registry}, nil
}

type blobPusher struct {
	registry *Registry
}

func (bp *blobPusher) Push(ctx context.Context, desc Descriptor) (io.WriteCloser, error) {
	return &pushWriter{registry: bp.registry, digest: desc.Digest}, nil
}

type pushWriter struct {
	registry *Registry
	digest   string
	buf      []byte
}

func (pw *pushWriter) Write(p []byte) (int, error) {
	pw.buf = append(pw.buf, p...)
	return len(p), nil
}

func (pw *pushWriter) Close() error {
	pw.registry.mu.Lock()
	defer pw.registry.mu.Unlock()
	pw.registry.blobs[pw.digest] = pw.buf
	return nil
}

// --- LocalImageStore 구현 ---
// ImageStorer를 구현하여 로컬 Content Store에 이미지를 저장한다.

type LocalImageStore struct {
	store *ContentStore
}

func (lis *LocalImageStore) Store(ctx context.Context, desc Descriptor) (string, error) {
	name := fmt.Sprintf("docker.io/library/image@%s", desc.Digest[:16])
	return name, nil
}

// --- localTransferService 구현 ---
// 실제 코드: core/transfer/local/transfer.go - localTransferService
// Transfer(src, dest)에서 소스/대상 타입을 type switch로 판별하여
// pull, push, tag, importStream, exportStream 등의 메서드를 호출한다.

type localTransferService struct {
	content      *ContentStore
	maxDownloads int // 동시 다운로드 수 제한 (세마포어)
}

func NewLocalTransferService(cs *ContentStore, maxDownloads int) Transferrer {
	return &localTransferService{
		content:      cs,
		maxDownloads: maxDownloads,
	}
}

// Transfer는 소스/대상 타입 조합에 따라 적절한 전송 로직을 선택한다.
// 실제 코드: core/transfer/local/transfer.go 라인 68-99
// switch s := src.(type) {
//   case transfer.ImageFetcher:   → pull
//   case transfer.ImageGetter:    → push / exportStream / tag
//   case transfer.ImageImporter:  → importStream
// }
func (ts *localTransferService) Transfer(ctx context.Context, src interface{}, dest interface{}, opts ...TransferOpt) error {
	cfg := &TransferConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	switch s := src.(type) {
	case ImageFetcher:
		switch d := dest.(type) {
		case ImageStorer:
			return ts.pull(ctx, s, d, cfg)
		default:
			return fmt.Errorf("unsupported destination type for ImageFetcher: %T", dest)
		}
	default:
		return fmt.Errorf("unsupported source type: %T", src)
	}
}

// pull은 이미지를 원격 레지스트리에서 가져오는 핵심 로직이다.
// 실제 코드: core/transfer/local/pull.go
// 흐름: Resolve → Fetch layers (병렬) → Content.Writer → Commit → Store
func (ts *localTransferService) pull(ctx context.Context, ir ImageFetcher, is ImageStorer, cfg *TransferConfig) error {
	// 1단계: Resolve — 이미지 참조를 Descriptor로 변환
	// 실제 코드: core/transfer/local/pull.go 라인 60
	if cfg.Progress != nil {
		cfg.Progress(Progress{Event: fmt.Sprintf("Resolving from %s", ir.(fmt.Stringer))})
	}

	name, desc, err := ir.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve image: %w", err)
	}

	if cfg.Progress != nil {
		cfg.Progress(Progress{
			Event: "pulling image content",
			Name:  name,
			Desc:  &desc,
		})
	}

	// 2단계: Fetcher 생성
	// 실제 코드: core/transfer/local/pull.go 라인 106
	fetcher, err := ir.Fetcher(ctx, name)
	if err != nil {
		return fmt.Errorf("failed to get fetcher: %w", err)
	}

	// 3단계: 매니페스트에서 레이어 목록 추출 (Children 핸들러)
	// 실제 코드: core/images/image.go - Children() 함수
	// 매니페스트를 파싱하여 config + layers 디스크립터 목록을 반환한다.
	rf := ir.(*RegistryFetcher)
	rf.registry.mu.RLock()
	manifest := rf.registry.images[rf.ref]
	rf.registry.mu.RUnlock()

	allDescs := append([]Descriptor{manifest.Config}, manifest.Layers...)

	// 4단계: 레이어 병렬 다운로드 (Dispatch)
	// 실제 코드: core/images/handlers.go - Dispatch()
	// errgroup과 semaphore를 사용하여 동시성을 제한하며 병렬 다운로드한다.
	// images.Dispatch(ctx, handler, limiter, desc)
	sem := make(chan struct{}, ts.maxDownloads)
	var wg sync.WaitGroup
	errCh := make(chan error, len(allDescs))

	for _, d := range allDescs {
		wg.Add(1)
		go func(desc Descriptor) {
			defer wg.Done()

			// 세마포어 획득 — 동시 다운로드 수 제한
			// 실제 코드: semaphore.Weighted를 사용
			sem <- struct{}{}
			defer func() { <-sem }()

			// 이미 존재하면 스킵 (content-addressable)
			if ts.content.Exists(desc.Digest) {
				if cfg.Progress != nil {
					cfg.Progress(Progress{
						Event:    "exists",
						Name:     desc.Digest[:16],
						Progress: desc.Size,
						Total:    desc.Size,
						Desc:     &desc,
					})
				}
				return
			}

			// Fetch — blob 다운로드
			if cfg.Progress != nil {
				cfg.Progress(Progress{
					Event: "downloading",
					Name:  desc.Digest[:16],
					Total: desc.Size,
					Desc:  &desc,
				})
			}

			rc, err := fetcher.Fetch(ctx, desc)
			if err != nil {
				errCh <- fmt.Errorf("fetch %s: %w", desc.Digest[:16], err)
				return
			}
			defer rc.Close()

			data, err := io.ReadAll(rc)
			if err != nil {
				errCh <- fmt.Errorf("read %s: %w", desc.Digest[:16], err)
				return
			}

			// Content Store에 저장 (Writer → Commit)
			// 실제 코드: content.OpenWriter → writer.Write → writer.Commit
			ts.content.Write(desc.Digest, data)

			if cfg.Progress != nil {
				cfg.Progress(Progress{
					Event:    "done",
					Name:     desc.Digest[:16],
					Progress: desc.Size,
					Total:    desc.Size,
					Desc:     &desc,
				})
			}
		}(d)
	}

	wg.Wait()
	close(errCh)

	// 에러 수집
	for err := range errCh {
		if err != nil {
			return err
		}
	}

	// 5단계: 이미지 레코드 저장 (ImageStorer.Store)
	// 실제 코드: core/transfer/local/pull.go 라인 260
	imgName, err := is.Store(ctx, desc)
	if err != nil {
		return fmt.Errorf("failed to store image: %w", err)
	}

	if cfg.Progress != nil {
		cfg.Progress(Progress{
			Event: "saved",
			Name:  imgName,
			Desc:  &desc,
		})
		cfg.Progress(Progress{
			Event: fmt.Sprintf("Completed pull from %s", ir.(fmt.Stringer)),
		})
	}

	return nil
}

// =============================================================================
// 메인 함수 — 이미지 Pull 흐름 시뮬레이션
// =============================================================================

func main() {
	fmt.Println("=== containerd Transfer Service 시뮬레이션 ===")
	fmt.Println()

	// 1. 레지스트리 준비 — 테스트용 이미지 등록
	registry := NewRegistry()

	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("config-data")))
	layers := []Descriptor{
		{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("layer-1-base-os"))),
			Size:      52428800, // 50MB
		},
		{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("layer-2-packages"))),
			Size:      31457280, // 30MB
		},
		{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("layer-3-app"))),
			Size:      10485760, // 10MB
		},
	}

	manifest := ImageManifest{
		Name: "docker.io/library/nginx:1.25",
		Config: Descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      2048,
		},
		Layers:    layers,
		MediaType: "application/vnd.oci.image.manifest.v1+json",
	}

	registry.AddImage("docker.io/library/nginx:1.25", manifest)
	fmt.Println("[레지스트리] 이미지 등록: docker.io/library/nginx:1.25")
	fmt.Printf("  Config:  %s\n", manifest.Config)
	for i, l := range manifest.Layers {
		fmt.Printf("  Layer %d: %s (%dMB)\n", i+1, l, l.Size/1024/1024)
	}
	fmt.Println()

	// 2. Transfer Service 생성
	// 실제 코드: core/transfer/local/transfer.go - NewTransferService
	// MaxConcurrentDownloads=3 → 최대 3개 레이어 동시 다운로드
	contentStore := NewContentStore()
	transferService := NewLocalTransferService(contentStore, 3)

	// 3. 이미지 Pull 실행
	// 실제 흐름: Resolve → Fetch layers (병렬) → Content.Writer → Commit → Store
	fmt.Println("[Pull 시작] docker.io/library/nginx:1.25")
	fmt.Println(strings.Repeat("-", 60))

	ctx := context.Background()
	src := NewRegistryFetcher(registry, "docker.io/library/nginx:1.25")
	dst := &LocalImageStore{store: contentStore}

	start := time.Now()
	err := transferService.Transfer(ctx, src, dst,
		WithProgress(func(p Progress) {
			switch p.Event {
			case "downloading":
				fmt.Printf("  [%s] %s: %s...\n", p.Event, p.Name, p.Desc.MediaType)
			case "done":
				fmt.Printf("  [%s] %s: %d bytes 완료\n", p.Event, p.Name, p.Total)
			case "exists":
				fmt.Printf("  [%s] %s: 이미 존재 (스킵)\n", p.Event, p.Name)
			case "saved":
				fmt.Printf("  [%s] %s\n", p.Event, p.Name)
			default:
				if p.Name != "" {
					fmt.Printf("  [%s] %s\n", p.Event, p.Name)
				} else {
					fmt.Printf("  [%s]\n", p.Event)
				}
			}
		}),
	)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("\nPull 실패: %v\n", err)
		return
	}

	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("[Pull 완료] 소요 시간: %v\n", elapsed)
	fmt.Println()

	// 4. Content Store 상태 확인
	fmt.Println("[Content Store 상태]")
	contentStore.mu.RLock()
	for digest := range contentStore.blobs {
		fmt.Printf("  %s...\n", digest[:32])
	}
	contentStore.mu.RUnlock()
	fmt.Println()

	// 5. 같은 이미지 재 Pull — content-addressable이므로 스킵
	fmt.Println("[재 Pull 시도] — 이미 존재하는 이미지는 스킵")
	fmt.Println(strings.Repeat("-", 60))

	start2 := time.Now()
	err = transferService.Transfer(ctx, src, dst,
		WithProgress(func(p Progress) {
			if p.Event == "exists" {
				fmt.Printf("  [exists] %s: 이미 존재 (스킵)\n", p.Name)
			}
		}),
	)
	elapsed2 := time.Since(start2)

	if err != nil {
		fmt.Printf("\n재 Pull 실패: %v\n", err)
		return
	}

	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("[재 Pull 완료] 소요 시간: %v (이미 존재하여 빠름)\n", elapsed2)
	fmt.Println()

	// 6. Transfer 타입 매칭 설명
	fmt.Println("[Transfer 타입 매칭 — type switch 기반]")
	fmt.Println("실제 코드: core/transfer/local/transfer.go")
	fmt.Println()
	fmt.Println("  src \\ dest       | ImageStorer     | ImagePusher     | ImageExporter")
	fmt.Println("  -----------------+-----------------+-----------------+----------------")
	fmt.Println("  ImageFetcher     | pull (Pull)     |                 |")
	fmt.Println("  ImageGetter      | tag (Tag)       | push (Push)     | export (Save)")
	fmt.Println("  ImageImporter    | import (Load)   |                 |")
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
