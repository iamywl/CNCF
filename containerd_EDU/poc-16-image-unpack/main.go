package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// =============================================================================
// containerd 이미지 매니페스트 파싱 + 레이어 언팩 시뮬레이션
// =============================================================================
//
// OCI 이미지는 Manifest → Config + Layers 구조로 되어 있으며,
// containerd는 이를 파싱하여 Content Store에서 레이어를 읽고,
// Snapshotter를 통해 파일시스템으로 언팩한다.
//
// 핵심 흐름:
//   1. Image Index (optional) → Platform에 맞는 Manifest 선택
//   2. Manifest → Config Descriptor + Layer Descriptors
//   3. Config → DiffIDs (압축 해제 후 해시)
//   4. ChainID 계산: sha256(parent_chainID + " " + layer_diffID)
//   5. 레이어 언팩: Content.ReaderAt → Snapshot.Prepare → tar 추출 → Commit
//
// 실제 코드 참조:
//   - core/images/image.go         — Manifest(), Children(), RootFS()
//   - core/images/mediatypes.go    — IsManifestType, IsIndexType, IsLayerType
//   - core/images/handlers.go      — Walk, Dispatch, ChildrenHandler
//   - core/images/diffid.go        — GetDiffID (압축 해제 → sha256)
//   - core/unpack/unpacker.go      — Unpacker (Snapshot.Prepare → Apply → Commit)
//   - github.com/opencontainers/image-spec/identity — ChainID 계산
// =============================================================================

// --- OCI 이미지 스펙 구조체 ---
// 실제 코드: github.com/opencontainers/image-spec/specs-go/v1

// Descriptor는 OCI 콘텐츠 디스크립터이다.
// mediaType, digest, size로 콘텐츠를 고유하게 식별한다.
type Descriptor struct {
	MediaType   string    `json:"mediaType"`
	Digest      string    `json:"digest"`
	Size        int64     `json:"size"`
	Platform    *Platform `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform은 OCI 플랫폼 사양이다.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Index는 멀티 아키텍처 이미지의 최상위 구조이다.
// 실제 코드: core/images/image.go - Children() 에서 IsIndexType 분기
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
}

// Manifest는 단일 플랫폼의 이미지 구조이다.
// Config(이미지 설정) + Layers(파일시스템 레이어)로 구성된다.
// 실제 코드: core/images/image.go - Children() 에서 IsManifestType 분기
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// ImageConfig는 이미지 설정 정보이다.
// RootFS.DiffIDs가 각 레이어의 압축 해제 후 해시값이다.
type ImageConfig struct {
	Architecture string        `json:"architecture"`
	OS           string        `json:"os"`
	Created      string        `json:"created,omitempty"`
	Config       ContainerConf `json:"config,omitempty"`
	RootFS       RootFS        `json:"rootfs"`
	History      []History     `json:"history,omitempty"`
}

type ContainerConf struct {
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

// RootFS는 이미지의 루트 파일시스템 정보이다.
// DiffIDs는 각 레이어의 비압축 콘텐츠 해시 (sha256)이다.
// 실제 코드: core/images/image.go - RootFS()
type RootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type History struct {
	Created   string `json:"created,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	Comment   string `json:"comment,omitempty"`
	EmptyLayer bool  `json:"empty_layer,omitempty"`
}

// --- 미디어 타입 판별 함수 ---
// 실제 코드: core/images/mediatypes.go

const (
	MediaTypeOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex        = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIConfig       = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayer        = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeOCILayerZstd    = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaTypeDockerManifest  = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerConfig    = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerLayer     = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// IsManifestType은 매니페스트 미디어 타입인지 확인한다.
// 실제 코드: core/images/mediatypes.go - IsManifestType
func IsManifestType(mt string) bool {
	return mt == MediaTypeOCIManifest || mt == MediaTypeDockerManifest
}

// IsIndexType은 인덱스(멀티 아키텍처) 미디어 타입인지 확인한다.
// 실제 코드: core/images/mediatypes.go - IsIndexType
func IsIndexType(mt string) bool {
	return mt == MediaTypeOCIIndex || mt == MediaTypeDockerManifestList
}

// IsLayerType은 레이어 미디어 타입인지 확인한다.
// 실제 코드: core/images/mediatypes.go - IsLayerType
func IsLayerType(mt string) bool {
	return strings.HasPrefix(mt, "application/vnd.oci.image.layer.") ||
		strings.HasPrefix(mt, "application/vnd.docker.image.rootfs.diff.")
}

// IsConfigType은 이미지 설정 미디어 타입인지 확인한다.
// 실제 코드: core/images/mediatypes.go - IsConfigType
func IsConfigType(mt string) bool {
	return mt == MediaTypeOCIConfig || mt == MediaTypeDockerConfig
}

// --- Content Store 시뮬레이션 ---
// digest → []byte 맵으로 content-addressable storage를 표현한다.

type ContentStore struct {
	blobs map[string][]byte
}

func NewContentStore() *ContentStore {
	return &ContentStore{blobs: make(map[string][]byte)}
}

func (cs *ContentStore) Put(digest string, data []byte) {
	cs.blobs[digest] = data
}

func (cs *ContentStore) Get(digest string) ([]byte, error) {
	data, ok := cs.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("blob not found: %s", digest)
	}
	return data, nil
}

// --- ChainID 계산 ---
// 실제 코드: github.com/opencontainers/image-spec/identity - ChainID
// ChainID는 레이어의 누적 해시이다.
//   - 첫 번째 레이어: ChainID = DiffID
//   - 이후 레이어: ChainID = sha256(parentChainID + " " + diffID)
// 이를 통해 스냅샷의 부모-자식 관계를 결정한다.

func ComputeChainID(diffIDs []string) []string {
	if len(diffIDs) == 0 {
		return nil
	}

	chainIDs := make([]string, len(diffIDs))
	// 첫 번째 레이어의 ChainID는 DiffID와 동일
	chainIDs[0] = diffIDs[0]

	// 이후 레이어는 누적 해시
	for i := 1; i < len(diffIDs); i++ {
		input := chainIDs[i-1] + " " + diffIDs[i]
		hash := sha256.Sum256([]byte(input))
		chainIDs[i] = fmt.Sprintf("sha256:%x", hash)
	}

	return chainIDs
}

// --- Children 함수 ---
// 실제 코드: core/images/image.go - Children()
// Descriptor의 미디어 타입에 따라 하위 Descriptor 목록을 반환한다.
// 매니페스트 → [Config, Layer1, Layer2, ...]
// 인덱스 → [Manifest1, Manifest2, ...] (플랫폼별)

func Children(cs *ContentStore, desc Descriptor) ([]Descriptor, error) {
	data, err := cs.Get(desc.Digest)
	if err != nil {
		return nil, err
	}

	if IsManifestType(desc.MediaType) {
		var manifest Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, err
		}
		// Config + Layers를 자식으로 반환
		// 실제 코드: core/images/image.go 라인 355
		children := append([]Descriptor{manifest.Config}, manifest.Layers...)
		return children, nil
	}

	if IsIndexType(desc.MediaType) {
		var index Index
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, err
		}
		return index.Manifests, nil
	}

	return nil, nil
}

// --- Snapshot 시뮬레이션 ---
// 실제 코드: core/unpack/unpacker.go
// 레이어 언팩 흐름:
//   1. Snapshot.Prepare(key, parent) — 쓰기 가능한 스냅샷 생성
//   2. tar 추출 (Applier.Apply) — 레이어 내용을 마운트된 경로에 풀기
//   3. Snapshot.Commit(name, key) — 읽기 전용 스냅샷으로 확정

type SnapshotManager struct {
	snapshots map[string]*SnapshotInfo
}

type SnapshotInfo struct {
	Key      string
	Parent   string
	Kind     string // "active" (쓰기 가능) 또는 "committed" (읽기 전용)
	Files    []string
	ChainID  string
}

func NewSnapshotManager() *SnapshotManager {
	return &SnapshotManager{snapshots: make(map[string]*SnapshotInfo)}
}

// Prepare는 쓰기 가능한 스냅샷을 생성한다.
// parent가 있으면 해당 스냅샷 위에 overlay 마운트를 준비한다.
func (sm *SnapshotManager) Prepare(key, parent string) error {
	sm.snapshots[key] = &SnapshotInfo{
		Key:    key,
		Parent: parent,
		Kind:   "active",
	}
	return nil
}

// Apply는 tar 스트림을 스냅샷에 적용한다.
func (sm *SnapshotManager) Apply(key string, files []string) error {
	snap, ok := sm.snapshots[key]
	if !ok {
		return fmt.Errorf("snapshot %q not found", key)
	}
	snap.Files = files
	return nil
}

// Commit은 스냅샷을 읽기 전용으로 확정한다.
func (sm *SnapshotManager) Commit(name, key string) error {
	snap, ok := sm.snapshots[key]
	if !ok {
		return fmt.Errorf("snapshot %q not found", key)
	}

	committed := &SnapshotInfo{
		Key:      name,
		Parent:   snap.Parent,
		Kind:     "committed",
		Files:    snap.Files,
		ChainID:  name,
	}

	sm.snapshots[name] = committed
	delete(sm.snapshots, key) // active 스냅샷 제거
	return nil
}

// --- 테스트용 이미지 데이터 생성 ---

func createTestImage() (*ContentStore, Descriptor) {
	cs := NewContentStore()

	// 1. 레이어 데이터 (tar 시뮬레이션)
	layer1Data := createTarData(map[string]string{
		"bin/sh":       "#!/bin/sh\necho hello",
		"etc/os-release": "ID=alpine\nVERSION=3.19",
		"lib/libc.so":  "<binary-data>",
	})
	layer1Digest := fmt.Sprintf("sha256:%x", sha256.Sum256(layer1Data))

	layer2Data := createTarData(map[string]string{
		"usr/sbin/nginx":    "<nginx-binary>",
		"etc/nginx/nginx.conf": "worker_processes auto;",
	})
	layer2Digest := fmt.Sprintf("sha256:%x", sha256.Sum256(layer2Data))

	layer3Data := createTarData(map[string]string{
		"usr/share/nginx/html/index.html": "<html><body>Welcome</body></html>",
		"etc/nginx/conf.d/default.conf":   "server { listen 80; }",
	})
	layer3Digest := fmt.Sprintf("sha256:%x", sha256.Sum256(layer3Data))

	cs.Put(layer1Digest, layer1Data)
	cs.Put(layer2Digest, layer2Data)
	cs.Put(layer3Digest, layer3Data)

	// DiffID = 비압축 콘텐츠의 sha256 (여기서는 이미 비압축이므로 동일)
	diffID1 := layer1Digest
	diffID2 := layer2Digest
	diffID3 := layer3Digest

	// 2. Image Config
	config := ImageConfig{
		Architecture: "amd64",
		OS:           "linux",
		Created:      time.Now().Format(time.RFC3339),
		Config: ContainerConf{
			Cmd:        []string{"nginx", "-g", "daemon off;"},
			Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			WorkingDir: "/",
		},
		RootFS: RootFS{
			Type:    "layers",
			DiffIDs: []string{diffID1, diffID2, diffID3},
		},
		History: []History{
			{CreatedBy: "ADD alpine-minirootfs-3.19.tar.gz / # buildkit", Created: "2024-01-01T00:00:00Z"},
			{CreatedBy: "RUN apk add --no-cache nginx # buildkit", Created: "2024-01-01T00:01:00Z"},
			{CreatedBy: "COPY --from=builder /app/html /usr/share/nginx/html # buildkit", Created: "2024-01-01T00:02:00Z"},
		},
	}

	configData, _ := json.Marshal(config)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(configData))
	cs.Put(configDigest, configData)

	// 3. Manifest (linux/amd64)
	amd64Manifest := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config: Descriptor{
			MediaType: MediaTypeOCIConfig,
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: []Descriptor{
			{MediaType: MediaTypeOCILayer, Digest: layer1Digest, Size: int64(len(layer1Data))},
			{MediaType: MediaTypeOCILayer, Digest: layer2Digest, Size: int64(len(layer2Data))},
			{MediaType: MediaTypeOCILayer, Digest: layer3Digest, Size: int64(len(layer3Data))},
		},
	}

	amd64ManifestData, _ := json.Marshal(amd64Manifest)
	amd64ManifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(amd64ManifestData))
	cs.Put(amd64ManifestDigest, amd64ManifestData)

	// 4. arm64 Manifest (더미)
	arm64ConfigData := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:arm64dummy"]}}`)
	arm64ConfigDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(arm64ConfigData))
	cs.Put(arm64ConfigDigest, arm64ConfigData)

	arm64Manifest := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        Descriptor{MediaType: MediaTypeOCIConfig, Digest: arm64ConfigDigest, Size: int64(len(arm64ConfigData))},
		Layers: []Descriptor{
			{MediaType: MediaTypeOCILayer, Digest: "sha256:arm64layer1", Size: 1000},
		},
	}
	arm64ManifestData, _ := json.Marshal(arm64Manifest)
	arm64ManifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(arm64ManifestData))
	cs.Put(arm64ManifestDigest, arm64ManifestData)

	// 5. Image Index (멀티 아키텍처)
	index := Index{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIIndex,
		Manifests: []Descriptor{
			{
				MediaType: MediaTypeOCIManifest,
				Digest:    amd64ManifestDigest,
				Size:      int64(len(amd64ManifestData)),
				Platform:  &Platform{Architecture: "amd64", OS: "linux"},
			},
			{
				MediaType: MediaTypeOCIManifest,
				Digest:    arm64ManifestDigest,
				Size:      int64(len(arm64ManifestData)),
				Platform:  &Platform{Architecture: "arm64", OS: "linux"},
			},
		},
	}

	indexData, _ := json.Marshal(index)
	indexDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(indexData))
	cs.Put(indexDigest, indexData)

	return cs, Descriptor{
		MediaType: MediaTypeOCIIndex,
		Digest:    indexDigest,
		Size:      int64(len(indexData)),
	}
}

// createTarData는 파일명→내용 맵에서 tar 아카이브를 생성한다.
func createTarData(files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write([]byte(content))
	}

	_ = tw.Close()
	return buf.Bytes()
}

// --- 레이어 언팩 시뮬레이션 ---

func unpackLayer(cs *ContentStore, sm *SnapshotManager, desc Descriptor, parentChainID string, chainID string) ([]string, error) {
	// 1. Content Store에서 레이어 데이터 읽기
	data, err := cs.Get(desc.Digest)
	if err != nil {
		return nil, err
	}

	// 2. Snapshot.Prepare: 쓰기 가능한 스냅샷 생성
	// 실제 코드: core/unpack/unpacker.go — snapshotter.Prepare(key, parent)
	activeKey := fmt.Sprintf("extract-%s", chainID[:20])
	if err := sm.Prepare(activeKey, parentChainID); err != nil {
		return nil, err
	}

	// 3. tar 추출 (Applier.Apply)
	// 실제 코드: diff.Applier.Apply(ctx, desc, mounts)
	tr := tar.NewReader(bytes.NewReader(data))
	var files []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		files = append(files, hdr.Name)
	}

	if err := sm.Apply(activeKey, files); err != nil {
		return nil, err
	}

	// 4. Snapshot.Commit: 읽기 전용 스냅샷으로 확정
	// 실제 코드: core/unpack/unpacker.go — snapshotter.Commit(ctx, chainID, key)
	if err := sm.Commit(chainID, activeKey); err != nil {
		return nil, err
	}

	return files, nil
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== containerd 이미지 매니페스트 파싱 + 레이어 언팩 시뮬레이션 ===")
	fmt.Println()

	cs, indexDesc := createTestImage()

	// --- 1. Image Index 파싱 (멀티 아키텍처) ---
	fmt.Println("[1] Image Index 파싱 (멀티 아키텍처)")
	fmt.Println(strings.Repeat("-", 60))

	indexData, _ := cs.Get(indexDesc.Digest)
	var index Index
	_ = json.Unmarshal(indexData, &index)

	fmt.Printf("  MediaType: %s\n", index.MediaType)
	fmt.Printf("  플랫폼 수: %d\n", len(index.Manifests))
	for i, m := range index.Manifests {
		fmt.Printf("  [%d] Platform: %s, Digest: %s...\n", i, m.Platform, m.Digest[:32])
	}
	fmt.Println()

	// --- 2. 플랫폼 선택 (linux/amd64) ---
	// 실제 코드: core/images/image.go - Manifest() 함수
	// FilterPlatforms 핸들러가 platform.Match()로 매칭되는 매니페스트를 선택한다.
	fmt.Println("[2] 플랫폼 선택: linux/amd64")
	fmt.Println(strings.Repeat("-", 60))

	targetPlatform := Platform{Architecture: "amd64", OS: "linux"}
	var selectedManifestDesc Descriptor
	for _, m := range index.Manifests {
		if m.Platform != nil && m.Platform.Architecture == targetPlatform.Architecture &&
			m.Platform.OS == targetPlatform.OS {
			selectedManifestDesc = m
			break
		}
	}
	fmt.Printf("  선택된 매니페스트: %s...\n", selectedManifestDesc.Digest[:32])
	fmt.Println()

	// --- 3. Manifest 파싱 ---
	// 실제 코드: core/images/image.go - Children()
	// IsManifestType → Config + Layers 반환
	fmt.Println("[3] Manifest 파싱 — Children()")
	fmt.Println(strings.Repeat("-", 60))

	children, _ := Children(cs, selectedManifestDesc)
	fmt.Printf("  Children 수: %d (Config 1 + Layers %d)\n", len(children), len(children)-1)
	for i, child := range children {
		typeStr := "Layer"
		if IsConfigType(child.MediaType) {
			typeStr = "Config"
		}
		fmt.Printf("  [%d] %-6s MediaType=%s\n", i, typeStr, child.MediaType)
		fmt.Printf("              Digest=%s...\n", child.Digest[:32])
		fmt.Printf("              Size=%d bytes\n", child.Size)
	}
	fmt.Println()

	// --- 4. Config 파싱 → DiffIDs 추출 ---
	// 실제 코드: core/images/image.go - RootFS()
	fmt.Println("[4] Config 파싱 → DiffIDs + 히스토리")
	fmt.Println(strings.Repeat("-", 60))

	configDesc := children[0]
	configData, _ := cs.Get(configDesc.Digest)
	var imgConfig ImageConfig
	_ = json.Unmarshal(configData, &imgConfig)

	fmt.Printf("  Architecture: %s\n", imgConfig.Architecture)
	fmt.Printf("  OS: %s\n", imgConfig.OS)
	fmt.Printf("  Cmd: %v\n", imgConfig.Config.Cmd)
	fmt.Printf("  RootFS Type: %s\n", imgConfig.RootFS.Type)
	fmt.Printf("  DiffIDs (%d개):\n", len(imgConfig.RootFS.DiffIDs))
	for i, diffID := range imgConfig.RootFS.DiffIDs {
		fmt.Printf("    [%d] %s...\n", i, diffID[:32])
	}
	fmt.Println()
	fmt.Println("  History:")
	for i, h := range imgConfig.History {
		fmt.Printf("    [%d] %s\n", i, h.CreatedBy)
	}
	fmt.Println()

	// --- 5. ChainID 계산 ---
	// 실제 코드: github.com/opencontainers/image-spec/identity - ChainID
	// ChainID(L0) = DiffID(L0)
	// ChainID(Ln) = sha256(ChainID(Ln-1) + " " + DiffID(Ln))
	fmt.Println("[5] ChainID 계산")
	fmt.Println(strings.Repeat("-", 60))

	chainIDs := ComputeChainID(imgConfig.RootFS.DiffIDs)
	fmt.Println("  공식: ChainID(n) = sha256(ChainID(n-1) + \" \" + DiffID(n))")
	fmt.Println()
	for i, chainID := range chainIDs {
		isFirst := ""
		if i == 0 {
			isFirst = " (= DiffID, 첫 레이어)"
		}
		fmt.Printf("  ChainID[%d]: %s...%s\n", i, chainID[:32], isFirst)
	}
	fmt.Println()

	// --- 6. 레이어 언팩 ---
	// 실제 코드: core/unpack/unpacker.go
	// 흐름: Content.ReaderAt → Snapshot.Prepare → tar 추출 → Commit
	fmt.Println("[6] 레이어 언팩 (Snapshot.Prepare → Apply → Commit)")
	fmt.Println(strings.Repeat("-", 60))

	sm := NewSnapshotManager()
	layerDescs := children[1:] // Config 제외

	for i, layerDesc := range layerDescs {
		parentChainID := ""
		if i > 0 {
			parentChainID = chainIDs[i-1]
		}

		fmt.Printf("\n  레이어 %d 언팩:\n", i)
		fmt.Printf("    Digest:  %s...\n", layerDesc.Digest[:32])
		fmt.Printf("    ChainID: %s...\n", chainIDs[i][:32])
		if parentChainID != "" {
			fmt.Printf("    Parent:  %s...\n", parentChainID[:32])
		} else {
			fmt.Printf("    Parent:  (없음 — 베이스 레이어)\n")
		}

		files, err := unpackLayer(cs, sm, layerDesc, parentChainID, chainIDs[i])
		if err != nil {
			fmt.Printf("    [오류] %v\n", err)
			continue
		}

		fmt.Printf("    추출된 파일:\n")
		for _, f := range files {
			fmt.Printf("      %s\n", f)
		}
	}
	fmt.Println()

	// --- 7. 스냅샷 체인 확인 ---
	fmt.Println("[7] 스냅샷 체인 (overlay 계층 구조)")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println("  Snapshotter가 관리하는 읽기 전용 스냅샷:")
	for _, chainID := range chainIDs {
		snap := sm.snapshots[chainID]
		parentStr := "(베이스)"
		if snap.Parent != "" {
			parentStr = snap.Parent[:20] + "..."
		}
		fmt.Printf("    %s...\n", chainID[:32])
		fmt.Printf("      Kind: %s, Parent: %s\n", snap.Kind, parentStr)
		fmt.Printf("      Files: %v\n", snap.Files)
	}
	fmt.Println()

	// --- 8. 미디어 타입 판별 테이블 ---
	fmt.Println("[8] 미디어 타입 판별 (core/images/mediatypes.go)")
	fmt.Println(strings.Repeat("-", 60))

	testTypes := []struct {
		mt   string
		desc string
	}{
		{MediaTypeOCIIndex, "OCI Image Index (멀티 아키텍처)"},
		{MediaTypeDockerManifestList, "Docker Manifest List (멀티 아키텍처)"},
		{MediaTypeOCIManifest, "OCI Image Manifest"},
		{MediaTypeDockerManifest, "Docker Image Manifest v2"},
		{MediaTypeOCIConfig, "OCI Image Config"},
		{MediaTypeDockerConfig, "Docker Image Config"},
		{MediaTypeOCILayer, "OCI Image Layer (tar+gzip)"},
		{MediaTypeDockerLayer, "Docker Image Layer (tar.gzip)"},
		{MediaTypeOCILayerZstd, "OCI Image Layer (tar+zstd)"},
	}

	fmt.Printf("  %-55s  Index  Manifest  Config  Layer\n", "MediaType")
	fmt.Printf("  %s  -----  --------  ------  -----\n", strings.Repeat("-", 55))
	for _, t := range testTypes {
		fmt.Printf("  %-55s  %-5v  %-8v  %-6v  %v\n",
			t.mt,
			IsIndexType(t.mt),
			IsManifestType(t.mt),
			IsConfigType(t.mt),
			IsLayerType(t.mt),
		)
	}
	fmt.Println()

	// --- 9. 전체 흐름 요약 ---
	fmt.Println("[전체 언팩 흐름 요약]")
	fmt.Println()
	fmt.Println("  Image Reference (docker.io/library/nginx:1.25)")
	fmt.Println("    |")
	fmt.Println("    v")
	fmt.Println("  [Index]  ← IsIndexType() → 플랫폼별 Manifest 디스크립터 목록")
	fmt.Println("    |")
	fmt.Println("    | platform.Match(linux/amd64)")
	fmt.Println("    v")
	fmt.Println("  [Manifest]  ← IsManifestType() → Config + Layers 디스크립터")
	fmt.Println("    |")
	fmt.Println("    +---> [Config]  ← IsConfigType() → DiffIDs, History, Cmd 등")
	fmt.Println("    |")
	fmt.Println("    +---> [Layer 0]  ← IsLayerType()")
	fmt.Println("    |       Snapshot.Prepare(\"\") → Apply(tar) → Commit(ChainID[0])")
	fmt.Println("    |")
	fmt.Println("    +---> [Layer 1]")
	fmt.Println("    |       Snapshot.Prepare(ChainID[0]) → Apply → Commit(ChainID[1])")
	fmt.Println("    |")
	fmt.Println("    +---> [Layer 2]")
	fmt.Println("            Snapshot.Prepare(ChainID[1]) → Apply → Commit(ChainID[2])")
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
