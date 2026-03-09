package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Istio Wasm 확장 + 웹훅 관리 시뮬레이션
//
// 실제 소스 참조:
//   - pkg/wasm/cache.go          → LocalFileCache (Wasm 바이너리 캐시)
//   - pkg/wasm/imagefetcher.go   → ImageFetcher (OCI/HTTP 다운로드)
//   - pkg/wasm/convert.go        → MaybeConvertWasmPluginFromRemoteToLocal
//   - pkg/webhooks/webhookpatch.go → WebhookCertPatcher
//
// 핵심 알고리즘:
//   1. WasmPlugin CRD → OCI/HTTP URL에서 Wasm 다운로드 → 로컬 캐시
//   2. Envoy ECDS 원격 참조 → 로컬 파일 참조 변환
//   3. 캐시 만료 + SHA256 검증 + 동시 다운로드 중복 방지
//   4. MutatingWebhookConfiguration의 CA Bundle 자동 패칭
// =============================================================================

// --- Wasm 바이너리 캐시 ---

// CacheEntry는 캐시된 Wasm 모듈이다.
type CacheEntry struct {
	Key        string // URL 또는 OCI 이미지 참조
	LocalPath  string // 로컬 파일 경로
	Checksum   string // SHA256
	FetchedAt  time.Time
	ExpireAt   time.Time
	Size       int64
}

// LocalFileCache는 Wasm 바이너리를 로컬에 캐시한다.
// 실제: pkg/wasm/cache.go의 LocalFileCache
type LocalFileCache struct {
	mu         sync.Mutex
	cacheDir   string
	entries    map[string]*CacheEntry
	maxSize    int64 // 바이트
	currentSize int64
	ttl        time.Duration

	// 동시 다운로드 중복 방지
	inflight   map[string]chan struct{}
	hits       int64
	misses     int64
	evictions  int64
}

func NewLocalFileCache(cacheDir string, maxSize int64, ttl time.Duration) *LocalFileCache {
	os.MkdirAll(cacheDir, 0755)
	return &LocalFileCache{
		cacheDir: cacheDir,
		entries:  make(map[string]*CacheEntry),
		inflight: make(map[string]chan struct{}),
		maxSize:  maxSize,
		ttl:      ttl,
	}
}

// Get은 캐시에서 Wasm 모듈을 조회한다.
// 만료되었으면 nil을 반환하여 재다운로드를 트리거한다.
func (c *LocalFileCache) Get(key string) *CacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil
	}

	// 만료 확인
	if time.Now().After(entry.ExpireAt) {
		fmt.Printf("  [Cache] 만료됨: %s (TTL 초과)\n", key)
		c.removeEntry(key)
		c.misses++
		return nil
	}

	c.hits++
	return entry
}

// Put은 Wasm 모듈을 캐시에 저장한다.
func (c *LocalFileCache) Put(key string, data []byte, checksum string) (*CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// SHA256 검증
	computed := sha256sum(data)
	if checksum != "" && computed != checksum {
		return nil, fmt.Errorf("checksum mismatch: expected %s, got %s", checksum, computed)
	}

	// 용량 확인 및 필요시 eviction
	size := int64(len(data))
	for c.currentSize+size > c.maxSize && len(c.entries) > 0 {
		c.evictOldest()
	}

	// 로컬 파일 저장
	localPath := filepath.Join(c.cacheDir, computed+".wasm")
	if err := os.WriteFile(localPath, data, 0644); err != nil {
		return nil, fmt.Errorf("failed to write cache file: %v", err)
	}

	entry := &CacheEntry{
		Key:       key,
		LocalPath: localPath,
		Checksum:  computed,
		FetchedAt: time.Now(),
		ExpireAt:  time.Now().Add(c.ttl),
		Size:      size,
	}
	c.entries[key] = entry
	c.currentSize += size
	return entry, nil
}

func (c *LocalFileCache) removeEntry(key string) {
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	os.Remove(entry.LocalPath)
	c.currentSize -= entry.Size
	delete(c.entries, key)
}

func (c *LocalFileCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.FetchedAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.FetchedAt
		}
	}
	if oldestKey != "" {
		fmt.Printf("  [Cache] Eviction: %s\n", oldestKey)
		c.removeEntry(oldestKey)
		c.evictions++
	}
}

// FetchOrGet은 캐시에 있으면 반환, 없으면 다운로드 후 캐시한다.
// 동시 다운로드 중복 방지: inflight map을 사용한다.
func (c *LocalFileCache) FetchOrGet(key string, fetcher func() ([]byte, string, error)) (*CacheEntry, error) {
	// 1. 캐시 확인
	if entry := c.Get(key); entry != nil {
		fmt.Printf("  [Cache] 히트: %s → %s\n", key, entry.LocalPath)
		return entry, nil
	}

	// 2. 동시 다운로드 중복 방지
	c.mu.Lock()
	if ch, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		fmt.Printf("  [Cache] 대기 중 (다른 고루틴이 다운로드 중): %s\n", key)
		<-ch
		return c.Get(key), nil
	}
	ch := make(chan struct{})
	c.inflight[key] = ch
	c.mu.Unlock()

	// 3. 다운로드
	fmt.Printf("  [Cache] 미스 → 다운로드 중: %s\n", key)
	data, checksum, err := fetcher()
	if err != nil {
		c.mu.Lock()
		delete(c.inflight, key)
		close(ch)
		c.mu.Unlock()
		return nil, err
	}

	// 4. 캐시 저장
	entry, err := c.Put(key, data, checksum)
	c.mu.Lock()
	delete(c.inflight, key)
	close(ch)
	c.mu.Unlock()

	return entry, err
}

func (c *LocalFileCache) Stats() (hits, misses, evictions int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.evictions
}

func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// --- WasmPlugin CRD 모델 ---

type WasmPlugin struct {
	Name      string
	Namespace string
	URL       string // OCI 이미지 또는 HTTP URL
	SHA256    string
	Phase     string // "AUTHN", "AUTHZ", "STATS", "UNSPECIFIED"
	Priority  int
}

// --- Envoy Extension Config 변환 ---

type ExtensionConfig struct {
	Name       string
	WasmConfig WasmConfig
}

type WasmConfig struct {
	Name       string
	VMConfig   VMConfig
}

type VMConfig struct {
	Code CodeSource
}

type CodeSource struct {
	Remote *RemoteSource
	Local  *LocalSource
}

type RemoteSource struct {
	URL    string
	SHA256 string
}

type LocalSource struct {
	Filename string
}

// MaybeConvertRemoteToLocal은 원격 Wasm 참조를 로컬 파일 참조로 변환한다.
// 실제: pkg/wasm/convert.go의 MaybeConvertWasmPluginFromRemoteToLocal
func MaybeConvertRemoteToLocal(ec *ExtensionConfig, cache *LocalFileCache) error {
	remote := ec.WasmConfig.VMConfig.Code.Remote
	if remote == nil {
		return nil // 이미 로컬
	}

	entry, err := cache.FetchOrGet(remote.URL, func() ([]byte, string, error) {
		// 실제로는 OCI/HTTP에서 다운로드
		data := generateFakeWasm(remote.URL)
		return data, remote.SHA256, nil
	})
	if err != nil {
		return fmt.Errorf("wasm fetch failed: %v", err)
	}

	// 원격 → 로컬 변환
	ec.WasmConfig.VMConfig.Code.Remote = nil
	ec.WasmConfig.VMConfig.Code.Local = &LocalSource{
		Filename: entry.LocalPath,
	}
	fmt.Printf("  [Convert] %s → %s\n", remote.URL, entry.LocalPath)
	return nil
}

func generateFakeWasm(url string) []byte {
	size := 1024 + rand.Intn(4096)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(rand.Intn(256))
	}
	return data
}

// --- WebhookCertPatcher: CA Bundle 자동 패칭 ---
// 실제: pkg/webhooks/webhookpatch.go

type WebhookConfig struct {
	Name     string
	Webhooks []WebhookEntry
}

type WebhookEntry struct {
	Name     string
	CABundle string // Base64 인코딩된 CA 인증서
	Service  string
}

type WebhookCertPatcher struct {
	mu             sync.Mutex
	webhookName    string
	currentCABundle string
	patchCount     int
	configs        map[string]*WebhookConfig
}

func NewWebhookCertPatcher(webhookName string) *WebhookCertPatcher {
	return &WebhookCertPatcher{
		webhookName: webhookName,
		configs:     make(map[string]*WebhookConfig),
	}
}

func (wcp *WebhookCertPatcher) AddConfig(config *WebhookConfig) {
	wcp.mu.Lock()
	defer wcp.mu.Unlock()
	wcp.configs[config.Name] = config
}

// PatchCABundle은 현재 CA 번들을 모든 웹훅에 패치한다.
// 실제: 인증서 로테이션 시 호출
func (wcp *WebhookCertPatcher) PatchCABundle(newCABundle string) {
	wcp.mu.Lock()
	defer wcp.mu.Unlock()

	if wcp.currentCABundle == newCABundle {
		fmt.Printf("  [WebhookPatcher] CA Bundle 변경 없음, 스킵\n")
		return
	}

	wcp.currentCABundle = newCABundle
	for _, config := range wcp.configs {
		for i := range config.Webhooks {
			config.Webhooks[i].CABundle = newCABundle
			fmt.Printf("  [WebhookPatcher] 패치: %s/%s (CA Bundle 갱신)\n",
				config.Name, config.Webhooks[i].Name)
			wcp.patchCount++
		}
	}
}

// Run은 주기적으로 CA Bundle을 확인하고 패치한다.
func (wcp *WebhookCertPatcher) Run(stop <-chan struct{}, caProvider func() string) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			newCA := caProvider()
			wcp.PatchCABundle(newCA)
		}
	}
}

// --- 메인 함수 ---

func main() {
	fmt.Println("=== Istio Wasm 확장 + 웹훅 관리 시뮬레이션 ===")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, _ := os.MkdirTemp("", "istio-wasm-cache-*")
	defer os.RemoveAll(tmpDir)

	// 1. Wasm 캐시 생성 및 다운로드
	fmt.Println("--- 1단계: Wasm 바이너리 캐시 ---")
	cache := NewLocalFileCache(tmpDir, 100*1024, 500*time.Millisecond)

	// 첫 번째 다운로드 (캐시 미스)
	plugin1 := WasmPlugin{
		Name: "auth-filter", Namespace: "istio-system",
		URL: "oci://registry.example.com/wasm/auth:v1",
	}
	ec1 := &ExtensionConfig{
		Name: plugin1.Name,
		WasmConfig: WasmConfig{
			Name: plugin1.Name,
			VMConfig: VMConfig{
				Code: CodeSource{
					Remote: &RemoteSource{URL: plugin1.URL},
				},
			},
		},
	}
	MaybeConvertRemoteToLocal(ec1, cache)

	// 두 번째 조회 (캐시 히트)
	fmt.Println()
	ec1b := &ExtensionConfig{
		Name: plugin1.Name,
		WasmConfig: WasmConfig{
			Name: plugin1.Name,
			VMConfig: VMConfig{
				Code: CodeSource{
					Remote: &RemoteSource{URL: plugin1.URL},
				},
			},
		},
	}
	MaybeConvertRemoteToLocal(ec1b, cache)

	hits, misses, _ := cache.Stats()
	fmt.Printf("  캐시 통계: 히트=%d, 미스=%d\n", hits, misses)

	// 2. 체크섬 검증
	fmt.Println()
	fmt.Println("--- 2단계: 체크섬(SHA256) 검증 ---")
	data := generateFakeWasm("test")
	correctChecksum := sha256sum(data)
	wrongChecksum := "0000000000000000000000000000000000000000000000000000000000000000"

	_, err := cache.Put("test-correct", data, correctChecksum)
	if err != nil {
		fmt.Printf("  올바른 체크섬: 실패 - %v\n", err)
	} else {
		fmt.Printf("  올바른 체크섬: 성공 (SHA256=%s...)\n", correctChecksum[:16])
	}

	_, err = cache.Put("test-wrong", data, wrongChecksum)
	if err != nil {
		fmt.Printf("  잘못된 체크섬: 실패 - %v\n", err)
	} else {
		fmt.Printf("  잘못된 체크섬: 성공 (예상치 못함)\n")
	}

	// 3. 캐시 만료 + Eviction
	fmt.Println()
	fmt.Println("--- 3단계: 캐시 만료(TTL) ---")
	shortCache := NewLocalFileCache(tmpDir, 3*1024, 100*time.Millisecond)

	shortCache.Put("expire-test", generateFakeWasm("exp"), "")
	entry := shortCache.Get("expire-test")
	fmt.Printf("  저장 직후: %v (nil이 아님)\n", entry != nil)

	time.Sleep(150 * time.Millisecond)
	entry = shortCache.Get("expire-test")
	fmt.Printf("  TTL 경과 후: %v (nil이어야 함)\n", entry != nil)

	// 4. Eviction (용량 초과)
	fmt.Println()
	fmt.Println("--- 4단계: 캐시 Eviction (용량 초과) ---")
	smallCache := NewLocalFileCache(tmpDir, 3*1024, 10*time.Second)
	smallCache.Put("item-1", make([]byte, 1024), "")
	smallCache.Put("item-2", make([]byte, 1024), "")
	fmt.Println("  item-1, item-2 저장 (각 1KB, 총 2KB/3KB)")
	smallCache.Put("item-3", make([]byte, 2048), "")
	fmt.Println("  item-3 저장 (2KB) → item-1 eviction 예상")

	_, _, evictions := smallCache.Stats()
	fmt.Printf("  Eviction 횟수: %d\n", evictions)

	// 5. 동시 다운로드 중복 방지
	fmt.Println()
	fmt.Println("--- 5단계: 동시 다운로드 중복 방지 ---")
	concurrentCache := NewLocalFileCache(tmpDir, 100*1024, 10*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			concurrentCache.FetchOrGet("concurrent-url", func() ([]byte, string, error) {
				time.Sleep(50 * time.Millisecond) // 다운로드 지연
				return generateFakeWasm("concurrent"), "", nil
			})
		}(i)
	}
	wg.Wait()
	hits, misses, _ = concurrentCache.Stats()
	fmt.Printf("  결과: 히트=%d, 미스=%d (미스=1이면 중복 방지 성공)\n", hits, misses)

	// 6. WebhookCertPatcher: CA Bundle 자동 패칭
	fmt.Println()
	fmt.Println("--- 6단계: WebhookCertPatcher - CA Bundle 자동 패칭 ---")
	patcher := NewWebhookCertPatcher("istio-sidecar-injector")

	patcher.AddConfig(&WebhookConfig{
		Name: "istio-sidecar-injector",
		Webhooks: []WebhookEntry{
			{Name: "sidecar-injector.istio.io", Service: "istiod"},
			{Name: "namespace.sidecar-injector.istio.io", Service: "istiod"},
		},
	})

	// 초기 CA Bundle 패칭
	caBundle := "initial-ca-bundle-certificate-data"
	patcher.PatchCABundle(caBundle)

	// 동일한 CA → 스킵
	patcher.PatchCABundle(caBundle)

	// 인증서 로테이션 → 새 CA Bundle
	newCABundle := "rotated-ca-bundle-certificate-data"
	patcher.PatchCABundle(newCABundle)

	fmt.Printf("  총 패치 횟수: %d\n", patcher.patchCount)

	// 7. 주기적 CA 감시 시뮬레이션
	fmt.Println()
	fmt.Println("--- 7단계: 주기적 CA 감시 ---")
	stop := make(chan struct{})
	caVersion := 0
	go patcher.Run(stop, func() string {
		caVersion++
		if caVersion <= 2 {
			return newCABundle // 변경 없음
		}
		return fmt.Sprintf("ca-bundle-v%d", caVersion)
	})

	time.Sleep(200 * time.Millisecond)
	close(stop)

	// 요약
	fmt.Println()
	fmt.Println("=== 요약 ===")
	fmt.Println("  Wasm 확장:")
	fmt.Println("  - LocalFileCache: OCI/HTTP → 로컬 파일 캐시 (SHA256 검증)")
	fmt.Println("  - TTL 기반 만료 + LRU Eviction")
	fmt.Println("  - 동시 다운로드 중복 방지 (inflight map)")
	fmt.Println("  - Remote → Local 참조 변환 (ECDS)")
	fmt.Println()
	fmt.Println("  웹훅 관리:")
	fmt.Println("  - WebhookCertPatcher: MutatingWebhookConfiguration CA Bundle 자동 패칭")
	fmt.Println("  - 인증서 로테이션 시 무중단 업데이트")
	fmt.Println("  - 변경 없으면 스킵 (불필요한 API 호출 방지)")

	_ = strings.Join(nil, "")
}
