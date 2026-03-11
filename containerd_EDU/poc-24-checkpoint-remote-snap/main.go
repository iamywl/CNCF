package main

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// containerd Checkpoint/Restore + Remote Snapshotter 시뮬레이션
// =============================================================================
//
// Checkpoint/Restore: CRIU를 통해 실행 중인 컨테이너의 상태를 저장하고 복원한다.
// Remote Snapshotter: 이미지 레이어를 원격 스토리지에서 lazy-pull 할 수 있게 한다.
//
// 핵심 개념:
//   - CRIU (Checkpoint/Restore In Userspace): 프로세스 상태 덤프/복원
//   - Checkpoint: 메모리, 레지스터, 파일 디스크립터 등 저장
//   - Remote Snapshotter: Stargz/Nydus 등으로 원격 레이어 접근
//   - Lazy Pull: 전체 이미지 다운로드 없이 필요한 부분만 fetch
//
// 실제 코드 참조:
//   - pkg/cri/server/container_checkpoint.go: 체크포인트 API
//   - snapshots/: 스냅샷 인터페이스
//   - remotes/: 원격 스냅샷 통합
// =============================================================================

// --- 프로세스 상태 모델 (CRIU 시뮬레이션) ---

type ProcessState struct {
	PID        int
	Registers  map[string]uint64
	Memory     []MemoryRegion
	FDs        []FileDescriptor
	EnvVars    map[string]string
	CWD        string
}

type MemoryRegion struct {
	Start   uint64
	Size    uint64
	Perm    string // "rw-p", "r-xp" 등
	Content []byte // 시뮬레이션용 (실제로는 페이지 데이터)
}

type FileDescriptor struct {
	FD   int
	Path string
	Mode string
}

// --- Checkpoint/Restore Engine ---

type CheckpointData struct {
	ContainerID string
	Timestamp   time.Time
	ProcessState ProcessState
	Size        int64
	Checksum    string
}

type CheckpointStore struct {
	mu          sync.RWMutex
	checkpoints map[string]*CheckpointData
}

func NewCheckpointStore() *CheckpointStore {
	return &CheckpointStore{
		checkpoints: make(map[string]*CheckpointData),
	}
}

// Checkpoint는 컨테이너 상태를 저장한다 (CRIU dump 시뮬레이션).
func (cs *CheckpointStore) Checkpoint(containerID string, state ProcessState) (*CheckpointData, error) {
	fmt.Printf("    [CRIU] Freezing container %s (PID: %d)\n", containerID, state.PID)

	// 메모리 덤프 크기 계산
	var totalSize int64
	for _, region := range state.Memory {
		totalSize += int64(region.Size)
	}
	totalSize += int64(len(state.FDs)) * 256 // FD 메타데이터

	// 체크섬 계산
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%v", state)))
	checksum := fmt.Sprintf("sha256:%x", h.Sum(nil))

	data := &CheckpointData{
		ContainerID:  containerID,
		Timestamp:    time.Now(),
		ProcessState: state,
		Size:         totalSize,
		Checksum:     checksum[:32],
	}

	cs.mu.Lock()
	cs.checkpoints[containerID] = data
	cs.mu.Unlock()

	fmt.Printf("    [CRIU] Checkpoint saved: size=%d bytes, checksum=%s\n", totalSize, data.Checksum)
	fmt.Printf("    [CRIU] Dumped: %d memory regions, %d file descriptors\n",
		len(state.Memory), len(state.FDs))
	return data, nil
}

// Restore는 체크포인트에서 컨테이너를 복원한다 (CRIU restore 시뮬레이션).
func (cs *CheckpointStore) Restore(containerID string) (*ProcessState, error) {
	cs.mu.RLock()
	data, ok := cs.checkpoints[containerID]
	cs.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no checkpoint for container %s", containerID)
	}

	fmt.Printf("    [CRIU] Restoring container %s from checkpoint\n", containerID)
	fmt.Printf("    [CRIU] Checkpoint time: %s\n", data.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Printf("    [CRIU] Restoring %d memory regions\n", len(data.ProcessState.Memory))
	fmt.Printf("    [CRIU] Reopening %d file descriptors\n", len(data.ProcessState.FDs))
	fmt.Printf("    [CRIU] Setting registers (RIP=0x%x, RSP=0x%x)\n",
		data.ProcessState.Registers["RIP"], data.ProcessState.Registers["RSP"])
	fmt.Printf("    [CRIU] Restore complete! PID: %d\n", data.ProcessState.PID)

	return &data.ProcessState, nil
}

// --- Remote Snapshotter ---

type LayerInfo struct {
	Digest    string
	Size      int64
	MediaType string
}

type SnapshotLayer struct {
	Key       string
	Parent    string
	Digest    string
	Size      int64
	IsMounted bool
	IsPulled  bool
}

type RemoteSnapshotter struct {
	mu      sync.RWMutex
	layers  map[string]*SnapshotLayer // key -> layer
	fetched map[string]int64          // digest -> fetched bytes
	r       *rand.Rand
}

func NewRemoteSnapshotter() *RemoteSnapshotter {
	return &RemoteSnapshotter{
		layers:  make(map[string]*SnapshotLayer),
		fetched: make(map[string]int64),
		r:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Prepare는 스냅샷을 준비한다 (레이어를 마운트하지만 데이터는 아직 fetch하지 않음).
func (rs *RemoteSnapshotter) Prepare(key, parent string, layer LayerInfo) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	rs.layers[key] = &SnapshotLayer{
		Key:       key,
		Parent:    parent,
		Digest:    layer.Digest,
		Size:      layer.Size,
		IsMounted: true,
		IsPulled:  false, // lazy - 아직 pull 안 됨
	}
	fmt.Printf("    [SNAP] Prepared: %s (digest=%s, size=%d bytes, lazy=true)\n",
		key, layer.Digest[:20]+"...", layer.Size)
}

// FetchOnDemand는 파일 접근 시 필요한 부분만 fetch한다 (lazy pull).
func (rs *RemoteSnapshotter) FetchOnDemand(key, path string) (int64, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	layer, ok := rs.layers[key]
	if !ok {
		return 0, fmt.Errorf("snapshot %s not found", key)
	}

	// 파일 크기 시뮬레이션 (전체 레이어의 일부분만 fetch)
	fetchSize := int64(rs.r.Intn(int(layer.Size/10))) + 1024
	rs.fetched[layer.Digest] += fetchSize

	totalFetched := rs.fetched[layer.Digest]
	fetchPercent := float64(totalFetched) / float64(layer.Size) * 100
	if fetchPercent > 100 {
		fetchPercent = 100
		layer.IsPulled = true
	}

	fmt.Printf("    [FETCH] %s -> %s (fetched: %d bytes, total: %.1f%%)\n",
		key, path, fetchSize, fetchPercent)
	return fetchSize, nil
}

// PullComplete는 전체 레이어를 pull한다 (background pull).
func (rs *RemoteSnapshotter) PullComplete(key string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	layer, ok := rs.layers[key]
	if !ok {
		return
	}
	rs.fetched[layer.Digest] = layer.Size
	layer.IsPulled = true
	fmt.Printf("    [PULL] %s fully pulled (%d bytes)\n", key, layer.Size)
}

func (rs *RemoteSnapshotter) Stats() {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	totalSize := int64(0)
	totalFetched := int64(0)
	for _, layer := range rs.layers {
		totalSize += layer.Size
		totalFetched += rs.fetched[layer.Digest]
	}
	if totalFetched > totalSize {
		totalFetched = totalSize
	}

	fmt.Printf("    Layers: %d\n", len(rs.layers))
	fmt.Printf("    Total Size: %d bytes\n", totalSize)
	fmt.Printf("    Fetched: %d bytes (%.1f%%)\n", totalFetched, float64(totalFetched)/float64(totalSize)*100)
}

func main() {
	fmt.Println("=== containerd Checkpoint/Restore + Remote Snapshotter 시뮬레이션 ===")
	fmt.Println()

	// ===== Part 1: Checkpoint/Restore =====
	fmt.Println("[1] Container Checkpoint (CRIU dump)")
	fmt.Println(strings.Repeat("-", 65))

	store := NewCheckpointStore()

	// 컨테이너 프로세스 상태 시뮬레이션
	processState := ProcessState{
		PID: 12345,
		Registers: map[string]uint64{
			"RIP": 0x7f4a3c001234, // instruction pointer
			"RSP": 0x7ffc5a000100, // stack pointer
			"RAX": 0x0000000000000000,
			"RBX": 0x00005555555547a0,
		},
		Memory: []MemoryRegion{
			{Start: 0x400000, Size: 4096, Perm: "r-xp"},     // .text
			{Start: 0x600000, Size: 8192, Perm: "rw-p"},     // .data
			{Start: 0x7f0000000000, Size: 1048576, Perm: "rw-p"}, // heap
			{Start: 0x7ffc5a000000, Size: 131072, Perm: "rw-p"},  // stack
		},
		FDs: []FileDescriptor{
			{0, "/dev/null", "r"},
			{1, "/proc/self/fd/1", "w"},
			{2, "/proc/self/fd/2", "w"},
			{3, "/var/run/app.sock", "rw"},
			{4, "/var/log/app.log", "w"},
			{5, "socket:[12345]", "rw"},
		},
		EnvVars: map[string]string{
			"HOME": "/root",
			"PATH": "/usr/local/bin:/usr/bin",
			"APP_MODE": "production",
		},
		CWD: "/app",
	}

	checkpoint, _ := store.Checkpoint("ctr-web-server-001", processState)
	fmt.Println()

	// --- Restore ---
	fmt.Println("[2] Container Restore (CRIU restore)")
	fmt.Println(strings.Repeat("-", 65))

	restored, err := store.Restore("ctr-web-server-001")
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else {
		fmt.Printf("    Restored process environment:\n")
		for k, v := range restored.EnvVars {
			fmt.Printf("      %s=%s\n", k, v)
		}
		fmt.Printf("    Working directory: %s\n", restored.CWD)
	}
	fmt.Println()

	// Restore 실패 케이스
	fmt.Println("[3] Restore 실패 케이스 (체크포인트 없음)")
	fmt.Println(strings.Repeat("-", 65))
	_, err = store.Restore("nonexistent-container")
	fmt.Printf("    Error: %v\n", err)
	fmt.Println()

	// --- 마이그레이션 시나리오 ---
	fmt.Println("[4] 라이브 마이그레이션 시나리오")
	fmt.Println(strings.Repeat("-", 65))
	fmt.Printf("    1. 소스 노드에서 체크포인트:\n")
	fmt.Printf("       Container: %s\n", checkpoint.ContainerID)
	fmt.Printf("       Size: %d bytes\n", checkpoint.Size)
	fmt.Printf("    2. 체크포인트 데이터를 대상 노드로 전송 (시뮬레이션)\n")
	fmt.Printf("    3. 대상 노드에서 복원:\n")
	store.Restore("ctr-web-server-001")
	fmt.Println()

	// ===== Part 2: Remote Snapshotter =====
	fmt.Println("[5] Remote Snapshotter: Lazy Pull 시뮬레이션")
	fmt.Println(strings.Repeat("-", 65))

	snap := NewRemoteSnapshotter()

	// 이미지 레이어 등록 (stargz 형식 시뮬레이션)
	layers := []LayerInfo{
		{"sha256:aabb11223344556677889900aabbccddeeff00112233445566778899", 50 * 1024 * 1024, "application/vnd.oci.image.layer.v1.tar+gzip"},
		{"sha256:ccdd11223344556677889900aabbccddeeff00112233445566778899", 30 * 1024 * 1024, "application/vnd.oci.image.layer.v1.tar+gzip"},
		{"sha256:eeff11223344556677889900aabbccddeeff00112233445566778899", 15 * 1024 * 1024, "application/vnd.oci.image.layer.v1.tar+gzip"},
	}

	fmt.Println("\n  레이어 준비 (mount without pull):")
	parent := ""
	keys := make([]string, len(layers))
	for i, layer := range layers {
		key := fmt.Sprintf("layer-%d", i)
		keys[i] = key
		snap.Prepare(key, parent, layer)
		parent = key
	}
	fmt.Println()

	// --- On-demand fetch ---
	fmt.Println("[6] On-Demand Fetch (파일 접근 시)")
	fmt.Println(strings.Repeat("-", 65))

	fileAccesses := []struct {
		layer string
		path  string
	}{
		{keys[2], "/app/main"},
		{keys[2], "/app/config.yaml"},
		{keys[1], "/usr/lib/libc.so"},
		{keys[0], "/usr/bin/python3"},
		{keys[2], "/app/static/index.html"},
		{keys[1], "/usr/lib/libssl.so"},
	}

	for _, access := range fileAccesses {
		snap.FetchOnDemand(access.layer, access.path)
	}
	fmt.Println()

	// --- Background pull ---
	fmt.Println("[7] Background Pull (전체 레이어)")
	fmt.Println(strings.Repeat("-", 65))
	snap.PullComplete(keys[0])
	snap.PullComplete(keys[1])
	fmt.Println()

	// --- 통계 ---
	fmt.Println("[8] Snapshotter 통계")
	fmt.Println(strings.Repeat("-", 65))
	snap.Stats()
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
