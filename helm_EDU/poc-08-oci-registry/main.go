// Helm v4 OCI 레지스트리 PoC: OCI manifest/layer, Push/Pull
//
// 이 PoC는 Helm v4의 OCI 기반 차트 레지스트리를 시뮬레이션합니다:
//   1. OCI manifest/layer 구조 (pkg/registry/constants.go)
//   2. Push: 차트 → 레이어(tar+gzip) → manifest → 레지스트리 업로드
//   3. Pull: manifest → 레이어 다운로드 → 차트 추출
//   4. net/http/httptest로 인메모리 OCI 레지스트리
//   5. 컨텐츠 주소 지정 (digest 기반)
//
// 참조: pkg/registry/client.go, pkg/registry/constants.go
//
// 실행: go run main.go

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// =============================================================================
// OCI 미디어 타입: Helm의 pkg/registry/constants.go
// =============================================================================

const (
	// ConfigMediaType은 Helm 차트 매니페스트 설정의 미디어 타입
	ConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
	// ChartLayerMediaType은 Helm 차트 패키지 컨텐츠의 미디어 타입
	ChartLayerMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	// ProvLayerMediaType은 Helm 차트 provenance 파일의 미디어 타입
	ProvLayerMediaType = "application/vnd.cncf.helm.chart.provenance.v1.prov"
	// OCIScheme은 OCI 기반 요청의 URL 스키마
	OCIScheme = "oci"
)

// =============================================================================
// OCI 데이터 모델: manifest, descriptor, layer
// OCI Image Manifest Specification에 기반
// =============================================================================

// Descriptor는 OCI 컨텐츠 디스크립터이다.
// digest로 컨텐츠를 고유하게 식별한다 (컨텐츠 주소 지정).
type Descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest는 OCI 이미지 매니페스트이다.
// Helm 차트는 OCI 아티팩트로 매니페스트에 config + layers로 구성된다.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ChartConfig는 Helm 차트 설정 (config blob)이다.
// 차트의 메타데이터를 담는다.
type ChartConfig struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	APIVersion  string `json:"apiVersion"`
	AppVersion  string `json:"appVersion,omitempty"`
	Type        string `json:"type,omitempty"`
}

// =============================================================================
// Chart: 간소화된 차트 구조체
// =============================================================================

type Chart struct {
	Name        string
	Version     string
	Description string
	Content     []byte // 차트 아카이브 컨텐츠 (시뮬레이션)
}

// =============================================================================
// Digest 유틸리티: 컨텐츠 해시 기반 주소 지정
// OCI에서는 SHA256 해시를 사용하여 blob을 고유하게 식별한다.
// =============================================================================

func computeDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", hash)
}

// =============================================================================
// InMemoryRegistry: net/http/httptest 기반 OCI 레지스트리
// OCI Distribution Spec의 핵심 API를 시뮬레이션:
//   - PUT /v2/<name>/blobs/uploads/ (blob 업로드)
//   - GET /v2/<name>/blobs/<digest> (blob 다운로드)
//   - PUT /v2/<name>/manifests/<reference> (매니페스트 업로드)
//   - GET /v2/<name>/manifests/<reference> (매니페스트 다운로드)
// =============================================================================

type InMemoryRegistry struct {
	mu        sync.RWMutex
	blobs     map[string][]byte            // digest → blob data
	manifests map[string][]byte            // "repo:tag" → manifest JSON
	tags      map[string]map[string]string // repo → tag → digest
}

func NewInMemoryRegistry() *InMemoryRegistry {
	return &InMemoryRegistry{
		blobs:     make(map[string][]byte),
		manifests: make(map[string][]byte),
		tags:      make(map[string]map[string]string),
	}
}

// PutBlob은 blob을 저장하고 digest를 반환한다.
func (r *InMemoryRegistry) PutBlob(data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	digest := computeDigest(data)
	r.blobs[digest] = data
	return digest
}

// GetBlob은 digest로 blob을 조회한다.
func (r *InMemoryRegistry) GetBlob(digest string) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	data, ok := r.blobs[digest]
	return data, ok
}

// PutManifest는 매니페스트를 저장한다.
func (r *InMemoryRegistry) PutManifest(repo, tag string, data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	digest := computeDigest(data)
	key := repo + ":" + tag
	r.manifests[key] = data

	if _, ok := r.tags[repo]; !ok {
		r.tags[repo] = make(map[string]string)
	}
	r.tags[repo][tag] = digest

	return digest
}

// GetManifest는 매니페스트를 조회한다.
func (r *InMemoryRegistry) GetManifest(repo, tag string) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := repo + ":" + tag
	data, ok := r.manifests[key]
	return data, ok
}

// ListTags는 저장소의 태그 목록을 반환한다.
func (r *InMemoryRegistry) ListTags(repo string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var tags []string
	if tagMap, ok := r.tags[repo]; ok {
		for tag := range tagMap {
			tags = append(tags, tag)
		}
	}
	return tags
}

// ServeHTTP는 OCI Distribution API를 처리한다.
func (r *InMemoryRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// GET /v2/ - 레지스트리 핑
	if path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Blob 업로드: POST /v2/<repo>/blobs/uploads
	if req.Method == http.MethodPost && strings.Contains(path, "/blobs/uploads") {
		data, _ := io.ReadAll(req.Body)
		digest := r.PutBlob(data)
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
		return
	}

	// Blob 다운로드: GET /v2/<repo>/blobs/<digest>
	if req.Method == http.MethodGet && strings.Contains(path, "/blobs/") {
		parts := strings.Split(path, "/blobs/")
		if len(parts) == 2 {
			digest := parts[1]
			if data, ok := r.GetBlob(digest); ok {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Write(data)
				return
			}
		}
		http.NotFound(w, req)
		return
	}

	// Manifest 업로드: PUT /v2/<repo>/manifests/<tag>
	if req.Method == http.MethodPut && strings.Contains(path, "/manifests/") {
		parts := strings.SplitN(path, "/manifests/", 2)
		if len(parts) == 2 {
			repo := strings.TrimPrefix(parts[0], "/v2/")
			tag := parts[1]
			data, _ := io.ReadAll(req.Body)
			digest := r.PutManifest(repo, tag, data)
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}

	// Manifest 다운로드: GET /v2/<repo>/manifests/<tag>
	if req.Method == http.MethodGet && strings.Contains(path, "/manifests/") {
		parts := strings.SplitN(path, "/manifests/", 2)
		if len(parts) == 2 {
			repo := strings.TrimPrefix(parts[0], "/v2/")
			tag := parts[1]
			if data, ok := r.GetManifest(repo, tag); ok {
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				w.Write(data)
				return
			}
		}
		http.NotFound(w, req)
		return
	}

	// Tags 목록: GET /v2/<repo>/tags/list
	if req.Method == http.MethodGet && strings.Contains(path, "/tags/list") {
		repo := strings.TrimPrefix(strings.TrimSuffix(path, "/tags/list"), "/v2/")
		tags := r.ListTags(repo)
		json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
		return
	}

	http.NotFound(w, req)
}

// =============================================================================
// RegistryClient: Helm의 pkg/registry/client.go 시뮬레이션
// Push/Pull 메서드를 제공.
// =============================================================================

type RegistryClient struct {
	httpClient *http.Client
	baseURL    string
	debug      bool
}

func NewRegistryClient(baseURL string) *RegistryClient {
	return &RegistryClient{
		httpClient: http.DefaultClient,
		baseURL:    baseURL,
	}
}

// Push는 차트를 OCI 레지스트리에 푸시한다.
// 실제 Helm: registry.Client.Push(...)
// 흐름: 차트 → config blob + chart layer → manifest 생성 → 업로드
func (c *RegistryClient) Push(chart *Chart, repo string) error {
	fmt.Printf("  [Push] 차트 %s v%s → %s/%s\n", chart.Name, chart.Version, c.baseURL, repo)

	// 1) Config blob 생성 및 업로드
	config := ChartConfig{
		Name:        chart.Name,
		Version:     chart.Version,
		Description: chart.Description,
		APIVersion:  "v2",
	}
	configJSON, _ := json.Marshal(config)
	configDigest, err := c.uploadBlob(repo, configJSON)
	if err != nil {
		return fmt.Errorf("config blob 업로드 실패: %w", err)
	}
	fmt.Printf("    Config blob: %s (%d bytes)\n", configDigest[:25]+"...", len(configJSON))

	// 2) Chart layer 생성 및 업로드 (tar+gzip 시뮬레이션)
	chartLayer, err := compressChart(chart.Content)
	if err != nil {
		return fmt.Errorf("차트 압축 실패: %w", err)
	}
	layerDigest, err := c.uploadBlob(repo, chartLayer)
	if err != nil {
		return fmt.Errorf("chart layer 업로드 실패: %w", err)
	}
	fmt.Printf("    Chart layer: %s (%d bytes)\n", layerDigest[:25]+"...", len(chartLayer))

	// 3) Manifest 생성 및 업로드
	manifest := Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: Descriptor{
			MediaType: ConfigMediaType,
			Digest:    configDigest,
			Size:      int64(len(configJSON)),
		},
		Layers: []Descriptor{
			{
				MediaType: ChartLayerMediaType,
				Digest:    layerDigest,
				Size:      int64(len(chartLayer)),
				Annotations: map[string]string{
					"org.opencontainers.image.title": chart.Name + "-" + chart.Version + ".tgz",
				},
			},
		},
		Annotations: map[string]string{
			"org.opencontainers.image.created": "2024-01-15T10:30:00Z",
		},
	}

	// 태그: SemVer의 +는 _로 치환 (OCI 태그 제한)
	// 실제 Helm: registryUnderscoreMessage
	tag := strings.ReplaceAll(chart.Version, "+", "_")

	err = c.uploadManifest(repo, tag, manifest)
	if err != nil {
		return fmt.Errorf("manifest 업로드 실패: %w", err)
	}
	fmt.Printf("    Manifest: %s:%s\n", repo, tag)
	fmt.Printf("  [Push] 완료!\n")

	return nil
}

// Pull은 OCI 레지스트리에서 차트를 다운로드한다.
// 실제 Helm: registry.Client.Pull(...)
// 흐름: manifest 다운로드 → chart layer 다운로드 → 차트 추출
func (c *RegistryClient) Pull(repo, tag string) (*Chart, error) {
	fmt.Printf("  [Pull] %s/%s:%s\n", c.baseURL, repo, tag)

	// 1) Manifest 다운로드
	manifest, err := c.downloadManifest(repo, tag)
	if err != nil {
		return nil, fmt.Errorf("manifest 다운로드 실패: %w", err)
	}
	fmt.Printf("    Manifest 다운로드: %d layers\n", len(manifest.Layers))

	// 2) Config blob 다운로드
	configData, err := c.downloadBlob(repo, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("config blob 다운로드 실패: %w", err)
	}

	var config ChartConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("config 파싱 실패: %w", err)
	}
	fmt.Printf("    Config: %s v%s\n", config.Name, config.Version)

	// 3) Chart layer 다운로드
	if len(manifest.Layers) == 0 {
		return nil, fmt.Errorf("차트 레이어가 없습니다")
	}

	chartLayer := manifest.Layers[0]
	if chartLayer.MediaType != ChartLayerMediaType {
		return nil, fmt.Errorf("예상하지 못한 미디어 타입: %s", chartLayer.MediaType)
	}

	layerData, err := c.downloadBlob(repo, chartLayer.Digest)
	if err != nil {
		return nil, fmt.Errorf("chart layer 다운로드 실패: %w", err)
	}
	fmt.Printf("    Chart layer: %d bytes (digest: %s...)\n", len(layerData), chartLayer.Digest[:25])

	// 4) 압축 해제
	content, err := decompressChart(layerData)
	if err != nil {
		return nil, fmt.Errorf("차트 압축 해제 실패: %w", err)
	}

	chart := &Chart{
		Name:        config.Name,
		Version:     config.Version,
		Description: config.Description,
		Content:     content,
	}

	fmt.Printf("  [Pull] 완료: %s v%s (%d bytes)\n", chart.Name, chart.Version, len(chart.Content))
	return chart, nil
}

// HTTP 유틸리티 메서드

func (c *RegistryClient) uploadBlob(repo string, data []byte) (string, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/uploads", c.baseURL, repo)
	resp, err := c.httpClient.Post(url, "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("blob 업로드 실패: status %d", resp.StatusCode)
	}

	return resp.Header.Get("Docker-Content-Digest"), nil
}

func (c *RegistryClient) downloadBlob(repo, digest string) ([]byte, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", c.baseURL, repo, digest)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("blob 다운로드 실패: status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (c *RegistryClient) uploadManifest(repo, tag string, manifest Manifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL, repo, tag)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("manifest 업로드 실패: status %d", resp.StatusCode)
	}

	return nil
}

func (c *RegistryClient) downloadManifest(repo, tag string) (*Manifest, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL, repo, tag)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest 다운로드 실패: status %d", resp.StatusCode)
	}

	var manifest Manifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, err
	}

	return &manifest, nil
}

// =============================================================================
// 압축/해제 유틸리티
// =============================================================================

func compressChart(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompressChart(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(gz)
}

// =============================================================================
// main: 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Helm v4 OCI 레지스트리 PoC ===")
	fmt.Println()

	// 1) 인메모리 OCI 레지스트리 서버 시작
	registry := NewInMemoryRegistry()
	server := httptest.NewServer(registry)
	defer server.Close()
	fmt.Printf("OCI 레지스트리 서버: %s\n\n", server.URL)

	client := NewRegistryClient(server.URL)

	// 2) Push: 차트 → 레지스트리
	demoPush(client)

	// 3) Pull: 레지스트리 → 차트
	demoPull(client)

	// 4) OCI 구조 상세 출력
	demoOCIStructure(registry)

	// 5) 여러 버전 Push
	demoMultiVersion(client, registry)

	// 6) 컨텐츠 무결성 검증
	demoIntegrity(registry)
}

func demoPush(client *RegistryClient) {
	fmt.Println("--- 1. Push: 차트 → OCI 레지스트리 ---")

	chart := &Chart{
		Name:        "myapp",
		Version:     "1.0.0",
		Description: "My Application Chart",
		Content:     []byte("# Simulated chart archive content\napiVersion: v2\nname: myapp\nversion: 1.0.0"),
	}

	err := client.Push(chart, "myrepo/myapp")
	if err != nil {
		fmt.Printf("  Push 실패: %v\n", err)
	}
	fmt.Println()
}

func demoPull(client *RegistryClient) {
	fmt.Println("--- 2. Pull: OCI 레지스트리 → 차트 ---")

	chart, err := client.Pull("myrepo/myapp", "1.0.0")
	if err != nil {
		fmt.Printf("  Pull 실패: %v\n", err)
		return
	}

	fmt.Printf("  복원된 차트: %s v%s\n", chart.Name, chart.Version)
	fmt.Printf("  설명: %s\n", chart.Description)
	fmt.Printf("  컨텐츠: %s\n", string(chart.Content))
	fmt.Println()
}

func demoOCIStructure(registry *InMemoryRegistry) {
	fmt.Println("--- 3. OCI 구조 상세 ---")

	// Manifest 내용 출력
	manifestData, ok := registry.GetManifest("myrepo/myapp", "1.0.0")
	if !ok {
		fmt.Println("  매니페스트를 찾을 수 없습니다")
		return
	}

	var manifest Manifest
	json.Unmarshal(manifestData, &manifest)

	fmt.Println("  Manifest:")
	fmt.Printf("    schemaVersion: %d\n", manifest.SchemaVersion)
	fmt.Printf("    mediaType: %s\n", manifest.MediaType)
	fmt.Printf("    config:\n")
	fmt.Printf("      mediaType: %s\n", manifest.Config.MediaType)
	fmt.Printf("      digest: %s...\n", manifest.Config.Digest[:30])
	fmt.Printf("      size: %d bytes\n", manifest.Config.Size)
	fmt.Printf("    layers:\n")
	for i, layer := range manifest.Layers {
		fmt.Printf("      [%d] mediaType: %s\n", i, layer.MediaType)
		fmt.Printf("          digest: %s...\n", layer.Digest[:30])
		fmt.Printf("          size: %d bytes\n", layer.Size)
		if title, ok := layer.Annotations["org.opencontainers.image.title"]; ok {
			fmt.Printf("          title: %s\n", title)
		}
	}

	// Config blob 내용 출력
	configData, _ := registry.GetBlob(manifest.Config.Digest)
	fmt.Printf("\n  Config blob (JSON):\n")
	var prettyConfig map[string]any
	json.Unmarshal(configData, &prettyConfig)
	prettyJSON, _ := json.MarshalIndent(prettyConfig, "    ", "  ")
	fmt.Printf("    %s\n", string(prettyJSON))

	fmt.Println()
}

func demoMultiVersion(client *RegistryClient, registry *InMemoryRegistry) {
	fmt.Println("--- 4. 여러 버전 Push ---")

	versions := []struct {
		version string
		desc    string
	}{
		{"1.1.0", "Bug fixes"},
		{"2.0.0", "Major update"},
		{"2.0.1+build.123", "Build metadata version"},
	}

	for _, v := range versions {
		chart := &Chart{
			Name:        "myapp",
			Version:     v.version,
			Description: v.desc,
			Content:     []byte(fmt.Sprintf("chart content v%s", v.version)),
		}
		err := client.Push(chart, "myrepo/myapp")
		if err != nil {
			fmt.Printf("  Push 실패 (%s): %v\n", v.version, err)
		}
	}

	// 태그 목록 출력
	tags := registry.ListTags("myrepo/myapp")
	fmt.Printf("\n  태그 목록:\n")
	for _, tag := range tags {
		fmt.Printf("    - %s\n", tag)
	}

	fmt.Println("\n  주의: v2.0.1+build.123 → 태그 2.0.1_build.123 (+ → _ 치환)")
	fmt.Println()
}

func demoIntegrity(registry *InMemoryRegistry) {
	fmt.Println("--- 5. 컨텐츠 무결성 검증 ---")

	// 특정 blob의 digest 검증
	manifestData, _ := registry.GetManifest("myrepo/myapp", "1.0.0")
	var manifest Manifest
	json.Unmarshal(manifestData, &manifest)

	for _, layer := range manifest.Layers {
		data, ok := registry.GetBlob(layer.Digest)
		if !ok {
			fmt.Printf("  [실패] 레이어 blob 없음: %s\n", layer.Digest[:30])
			continue
		}

		actualDigest := computeDigest(data)
		if actualDigest == layer.Digest {
			fmt.Printf("  [통과] 레이어 무결성 확인: %s...\n", layer.Digest[:30])
		} else {
			fmt.Printf("  [실패] 레이어 무결성 불일치!\n")
			fmt.Printf("    기대: %s\n", layer.Digest[:30])
			fmt.Printf("    실제: %s\n", actualDigest[:30])
		}
	}

	configData, _ := registry.GetBlob(manifest.Config.Digest)
	actualDigest := computeDigest(configData)
	if actualDigest == manifest.Config.Digest {
		fmt.Printf("  [통과] Config 무결성 확인: %s...\n", manifest.Config.Digest[:30])
	}

	fmt.Println()
	fmt.Println("=== OCI 레지스트리 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. OCI 구조: Manifest(설정+레이어) → Descriptor(digest+size+mediaType)")
	fmt.Println("  2. Push 흐름: 차트→config blob 업로드→chart layer(gzip) 업로드→manifest 업로드")
	fmt.Println("  3. Pull 흐름: manifest 다운로드→config 확인→chart layer 다운로드→압축 해제")
	fmt.Println("  4. 컨텐츠 주소 지정: SHA256 digest로 모든 blob을 고유하게 식별")
	fmt.Println("  5. 태그 규칙: SemVer의 + 는 _ 로 치환 (OCI 태그 제한)")
	fmt.Println("  6. 미디어 타입: config=vnd.cncf.helm.config, layer=vnd.cncf.helm.chart.content")
}
