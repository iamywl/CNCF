// containerd PoC-02: 핵심 데이터 구조
//
// 실제 소스 참조:
//   - core/containers/containers.go   : Container{ID, Image, Runtime, Spec, SnapshotKey, Snapshotter, SandboxID}
//   - core/images/image.go            : Image{Name, Target(Descriptor), Labels}
//   - core/runtime/task.go            : Task 인터페이스, Status(Created/Running/Stopped), State
//   - core/runtime/runtime.go         : PlatformRuntime 인터페이스, CreateOpts, Exit
//   - core/content/content.go         : Store, Info{Digest, Size}, Writer, ReaderAt, Status
//   - core/snapshots/snapshotter.go   : Snapshotter 인터페이스, Info{Kind, Name, Parent}, Kind(View/Active/Committed)
//   - core/mount/mount.go             : Mount{Type, Source, Target, Options}
//
// 핵심 개념:
//   containerd의 데이터 모델은 OCI 스펙을 기반으로 5개의 핵심 엔티티로 구성된다:
//   1. Container — 실행 가능한 컨테이너의 메타데이터 (Task를 생성하기 위한 템플릿)
//   2. Image — OCI 이미지 참조 (Name + Target Descriptor로 Content Store의 blob을 가리킴)
//   3. Task — 실행 중인 프로세스 (Container에서 생성, 상태 머신으로 관리)
//   4. Content — digest 기반 불변 blob 저장소 (이미지 레이어, manifest, config)
//   5. Snapshot — CoW 파일시스템 레이어 (이미지 언팩 + 컨테이너 writable 레이어)
//
// 실행: go run main.go

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// 1. Content Descriptor (OCI Image Spec 기반)
// =============================================================================

// Descriptor는 OCI 콘텐츠 디스크립터
// 실제: github.com/opencontainers/image-spec/specs-go/v1.Descriptor
// containerd 전체에서 콘텐츠를 참조하는 기본 단위
type Descriptor struct {
	// MediaType은 콘텐츠의 MIME 타입
	// 실제 값: "application/vnd.oci.image.manifest.v1+json",
	//          "application/vnd.oci.image.layer.v1.tar+gzip" 등
	MediaType string `json:"mediaType"`

	// Digest는 SHA256 해시 ("sha256:..." 형식)
	// Content Store에서 blob을 조회하는 키
	Digest string `json:"digest"`

	// Size는 바이트 단위 크기
	// 다운로드 전 검증 및 진행률 계산에 사용
	Size int64 `json:"size"`

	// Platform은 멀티 아키텍처 이미지에서 사용
	Platform *Platform `json:"platform,omitempty"`
}

// Platform은 OCI 플랫폼 스펙
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// =============================================================================
// 2. Image (core/images/image.go 참조)
// =============================================================================

// Image는 containerd의 이미지 모델
// 실제: images.Image{Name, Labels, Target, CreatedAt, UpdatedAt}
//
// 주요 특성:
// - Name은 docker.io/library/nginx:latest 같은 참조
// - Target은 manifest 또는 index를 가리키는 Descriptor
// - Labels로 런타임 메타데이터 장식
type Image struct {
	// Name은 이미지 참조 (레지스트리 + 리포지토리 + 태그/다이제스트)
	// 실제: images.Image.Name — "필수 필드, resolver와 호환되는 참조여야 함"
	Name string `json:"name"`

	// Labels는 런타임 메타데이터
	// 실제: images.Image.Labels — "선택적, 완전 변경 가능"
	Labels map[string]string `json:"labels,omitempty"`

	// Target은 루트 콘텐츠 디스크립터 (보통 manifest 또는 index)
	// 실제: images.Image.Target — ocispec.Descriptor 타입
	Target Descriptor `json:"target"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// =============================================================================
// 3. Container (core/containers/containers.go 참조)
// =============================================================================

// RuntimeInfo는 컨테이너 런타임 정보
// 실제: containers.RuntimeInfo{Name, Options}
type RuntimeInfo struct {
	// Name은 런타임 식별자 (예: "io.containerd.runc.v2")
	Name string `json:"name"`
	// Options는 런타임별 설정
	Options map[string]interface{} `json:"options,omitempty"`
}

// Container는 컨테이너 메타데이터
// 실제: containers.Container — Task 생성의 "템플릿" 역할
//
// 핵심 설계:
// - Container 자체는 실행 상태가 없음 (그건 Task의 역할)
// - Container는 "어떻게 실행할 것인가"를 정의
// - Task는 Container로부터 생성되어 "실제 실행"을 담당
type Container struct {
	// ID는 네임스페이스 내 고유 식별자
	// 실제: "필수, 생성 후 변경 불가"
	ID string `json:"id"`

	// Labels는 메타데이터 확장
	// 실제: "선택적, 완전 변경 가능"
	Labels map[string]string `json:"labels,omitempty"`

	// Image는 이미지 참조
	// 실제: "선택적, 변경 가능"
	Image string `json:"image"`

	// Runtime은 컨테이너 실행에 사용할 런타임
	// 실제: "필수, 불변"
	Runtime RuntimeInfo `json:"runtime"`

	// Spec은 OCI 런타임 스펙
	// 실제: typeurl.Any 타입 — 직렬화된 OCI 스펙
	Spec map[string]interface{} `json:"spec"`

	// SnapshotKey는 루트 파일시스템의 스냅샷 키
	// 실제: "Task 생성 시 이 키로 마운트를 조회"
	SnapshotKey string `json:"snapshotKey"`

	// Snapshotter는 사용할 스냅샷터 이름
	// 실제: "선택적이지만 불변"
	Snapshotter string `json:"snapshotter"`

	// SandboxID는 이 컨테이너가 속한 샌드박스 ID
	// 실제: "선택적, 생성 후 변경 불가" — CRI (Kubernetes)에서 Pod 개념 지원
	SandboxID string `json:"sandboxID,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// Extensions는 클라이언트 지정 메타데이터
	// 실제: map[string]typeurl.Any
	Extensions map[string]interface{} `json:"extensions,omitempty"`
}

// =============================================================================
// 4. Task 상태 머신 (core/runtime/task.go 참조)
// =============================================================================

// TaskStatus는 Task의 런타임 상태
// 실제: runtime.Status = int (iota+1 부터 시작)
type TaskStatus int

const (
	// 실제: runtime.CreatedStatus — 프로세스가 생성됨
	StatusCreated TaskStatus = iota + 1
	// 실제: runtime.RunningStatus — 프로세스가 실행 중
	StatusRunning
	// 실제: runtime.StoppedStatus — 프로세스가 중지됨
	StatusStopped
	// 실제: runtime.DeletedStatus — 프로세스가 삭제됨
	StatusDeleted
	// 실제: runtime.PausedStatus — 프로세스가 일시정지됨
	StatusPaused
	// 실제: runtime.PausingStatus — 일시정지 진행 중
	StatusPausing
)

func (s TaskStatus) String() string {
	switch s {
	case StatusCreated:
		return "Created"
	case StatusRunning:
		return "Running"
	case StatusStopped:
		return "Stopped"
	case StatusDeleted:
		return "Deleted"
	case StatusPaused:
		return "Paused"
	case StatusPausing:
		return "Pausing"
	default:
		return "Unknown"
	}
}

// TaskState는 프로세스 상태 정보
// 실제: runtime.State{Status, Pid, ExitStatus, ExitedAt, Stdin, Stdout, Stderr, Terminal}
type TaskState struct {
	Status     TaskStatus `json:"status"`
	Pid        uint32     `json:"pid"`
	ExitStatus uint32     `json:"exitStatus"`
	ExitedAt   time.Time  `json:"exitedAt,omitempty"`
}

// Task는 실행 중인 컨테이너 프로세스를 시뮬레이션
// 실제 인터페이스: runtime.Task — Process 임베딩 + PID, Namespace, Pause, Resume, Exec 등
type Task struct {
	ID          string    `json:"id"`
	ContainerID string    `json:"containerID"`
	State       TaskState `json:"state"`
	Namespace   string    `json:"namespace"`

	// 상태 전이 이력 (실제에는 없지만 데모용)
	transitions []string
}

// 상태 전이 메서드들
// 실제: runtime.Process 인터페이스의 Start(), Kill() 등

func (t *Task) Create(pid uint32) {
	t.State = TaskState{Status: StatusCreated, Pid: pid}
	t.transitions = append(t.transitions, fmt.Sprintf("→ Created (pid=%d)", pid))
}

func (t *Task) Start() error {
	if t.State.Status != StatusCreated {
		return fmt.Errorf("cannot start task in %s state", t.State.Status)
	}
	t.State.Status = StatusRunning
	t.transitions = append(t.transitions, "→ Running")
	return nil
}

func (t *Task) Pause() error {
	if t.State.Status != StatusRunning {
		return fmt.Errorf("cannot pause task in %s state", t.State.Status)
	}
	t.State.Status = StatusPausing
	t.transitions = append(t.transitions, "→ Pausing")
	t.State.Status = StatusPaused
	t.transitions = append(t.transitions, "→ Paused")
	return nil
}

func (t *Task) Resume() error {
	if t.State.Status != StatusPaused {
		return fmt.Errorf("cannot resume task in %s state", t.State.Status)
	}
	t.State.Status = StatusRunning
	t.transitions = append(t.transitions, "→ Running (resumed)")
	return nil
}

func (t *Task) Stop(exitStatus uint32) {
	t.State.Status = StatusStopped
	t.State.ExitStatus = exitStatus
	t.State.ExitedAt = time.Now()
	t.transitions = append(t.transitions, fmt.Sprintf("→ Stopped (exit=%d)", exitStatus))
}

func (t *Task) Delete() {
	t.State.Status = StatusDeleted
	t.transitions = append(t.transitions, "→ Deleted")
}

// =============================================================================
// 5. Content Info (core/content/content.go 참조)
// =============================================================================

// ContentInfo는 Content Store의 blob 메타데이터
// 실제: content.Info{Digest, Size, CreatedAt, UpdatedAt, Labels}
type ContentInfo struct {
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// ContentStatus는 진행 중인 쓰기 작업의 상태
// 실제: content.Status{Ref, Offset, Total, Expected, StartedAt, UpdatedAt}
type ContentStatus struct {
	Ref       string    `json:"ref"`
	Offset    int64     `json:"offset"`
	Total     int64     `json:"total"`
	Expected  string    `json:"expected"` // 실제: digest.Digest
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// =============================================================================
// 6. Snapshot Info (core/snapshots/snapshotter.go 참조)
// =============================================================================

// SnapshotKind는 스냅샷의 종류
// 실제: snapshots.Kind = uint8
type SnapshotKind uint8

const (
	KindUnknown   SnapshotKind = iota
	KindView                          // 읽기 전용 스냅샷
	KindActive                        // 읽기/쓰기 가능한 활성 스냅샷
	KindCommitted                     // 커밋된 불변 스냅샷
)

func (k SnapshotKind) String() string {
	switch k {
	case KindView:
		return "View"
	case KindActive:
		return "Active"
	case KindCommitted:
		return "Committed"
	default:
		return "Unknown"
	}
}

// SnapshotInfo는 스냅샷 메타데이터
// 실제: snapshots.Info{Kind, Name, Parent, Labels, Created, Updated}
type SnapshotInfo struct {
	Kind    SnapshotKind      `json:"kind"`
	Name    string            `json:"name"`
	Parent  string            `json:"parent,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Created time.Time         `json:"created"`
	Updated time.Time         `json:"updated"`
}

// SnapshotUsage는 스냅샷의 디스크 사용량
// 실제: snapshots.Usage{Inodes, Size}
type SnapshotUsage struct {
	Inodes int64 `json:"inodes"`
	Size   int64 `json:"size"`
}

// Mount는 마운트 정보
// 실제: mount.Mount{Type, Source, Target, Options}
type Mount struct {
	Type    string   `json:"type"`
	Source  string   `json:"source"`
	Target  string   `json:"target,omitempty"`
	Options []string `json:"options"`
}

// =============================================================================
// 7. 데이터 흐름 시뮬레이션
// =============================================================================

func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h)
}

func prettyJSON(v interface{}) string {
	b, _ := json.MarshalIndent(v, "  ", "  ")
	return string(b)
}

func main() {
	fmt.Println("=" + strings.Repeat("=", 69))
	fmt.Println("containerd 핵심 데이터 구조 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 69))

	// =====================================================================
	// 1. Content Descriptor — 이미지 구성 요소
	// =====================================================================
	fmt.Println("\n[1] Content Descriptor (OCI Image Spec)")
	fmt.Println(strings.Repeat("-", 60))

	// 이미지 레이어 (compressed tar)
	layerData := []byte("simulated layer data: base filesystem content")
	layerDescriptor := Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    computeDigest(layerData),
		Size:      int64(len(layerData)),
	}

	// 이미지 config
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDescriptor := Descriptor{
		MediaType: "application/vnd.oci.image.config.v1+json",
		Digest:    computeDigest(configData),
		Size:      int64(len(configData)),
	}

	// Manifest (config + layers 참조)
	manifestContent := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        configDescriptor,
		"layers":        []Descriptor{layerDescriptor},
	}
	manifestData, _ := json.Marshal(manifestContent)
	manifestDescriptor := Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    computeDigest(manifestData),
		Size:      int64(len(manifestData)),
		Platform:  &Platform{OS: "linux", Architecture: "amd64"},
	}

	fmt.Println("\n  이미지 콘텐츠 그래프 (Image → Manifest → Config + Layers):")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────────┐")
	fmt.Printf("  │ Image Target (Manifest)                                     │\n")
	fmt.Printf("  │   mediaType: %s  │\n", manifestDescriptor.MediaType)
	fmt.Printf("  │   digest:    %.50s...│\n", manifestDescriptor.Digest)
	fmt.Printf("  │   size:      %d bytes                                       │\n", manifestDescriptor.Size)
	fmt.Println("  ├─────────────────────────────────────────────────────────────┤")
	fmt.Printf("  │ Config                                                      │\n")
	fmt.Printf("  │   mediaType: %s│\n", configDescriptor.MediaType)
	fmt.Printf("  │   digest:    %.50s...│\n", configDescriptor.Digest)
	fmt.Println("  ├─────────────────────────────────────────────────────────────┤")
	fmt.Printf("  │ Layer[0]                                                    │\n")
	fmt.Printf("  │   mediaType: %s │\n", layerDescriptor.MediaType)
	fmt.Printf("  │   digest:    %.50s...│\n", layerDescriptor.Digest)
	fmt.Println("  └─────────────────────────────────────────────────────────────┘")

	// =====================================================================
	// 2. Image
	// =====================================================================
	fmt.Println("\n[2] Image (core/images/image.go)")
	fmt.Println(strings.Repeat("-", 60))

	now := time.Now()
	image := Image{
		Name: "docker.io/library/nginx:1.25",
		Labels: map[string]string{
			"io.containerd.image.name": "docker.io/library/nginx:1.25",
		},
		Target:    manifestDescriptor,
		CreatedAt: now,
		UpdatedAt: now,
	}

	fmt.Printf("\n  Image:\n  %s\n", prettyJSON(image))
	fmt.Println("\n  핵심 포인트:")
	fmt.Println("  - Name: 레지스트리 참조 (pull에 사용)")
	fmt.Println("  - Target: Content Store의 manifest blob을 가리키는 Descriptor")
	fmt.Println("  - Target.Digest로 Content Store에서 manifest를 조회")

	// =====================================================================
	// 3. Container
	// =====================================================================
	fmt.Println("\n\n[3] Container (core/containers/containers.go)")
	fmt.Println(strings.Repeat("-", 60))

	container := Container{
		ID: "nginx-web-001",
		Labels: map[string]string{
			"app": "web",
			"env": "production",
		},
		Image: image.Name,
		Runtime: RuntimeInfo{
			Name: "io.containerd.runc.v2",
			Options: map[string]interface{}{
				"SystemdCgroup": true,
			},
		},
		Spec: map[string]interface{}{
			"ociVersion": "1.0.2",
			"process": map[string]interface{}{
				"args": []string{"nginx", "-g", "daemon off;"},
				"cwd":  "/",
			},
			"root": map[string]interface{}{
				"path":     "rootfs",
				"readonly": false,
			},
		},
		SnapshotKey: "nginx-web-001-snapshot",
		Snapshotter: "overlayfs",
		SandboxID:   "pod-abc-123", // Kubernetes Pod
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	fmt.Printf("\n  Container:\n  %s\n", prettyJSON(container))
	fmt.Println("\n  Container vs Task 관계:")
	fmt.Println("  ┌────────────────────┐         ┌──────────────────────┐")
	fmt.Println("  │ Container          │ create  │ Task                 │")
	fmt.Println("  │                    │ ──────> │                      │")
	fmt.Println("  │ - ID               │         │ - ID (= Container ID)│")
	fmt.Println("  │ - Image            │         │ - PID                │")
	fmt.Println("  │ - Runtime          │         │ - Status             │")
	fmt.Println("  │ - Spec (OCI)       │         │ - Exec processes     │")
	fmt.Println("  │ - SnapshotKey      │         │ - Pause/Resume       │")
	fmt.Println("  │ - Snapshotter      │         │                      │")
	fmt.Println("  │ - SandboxID (Pod)  │         │ Created→Running      │")
	fmt.Println("  │                    │         │   →Paused→Stopped    │")
	fmt.Println("  └────────────────────┘         └──────────────────────┘")
	fmt.Println("  Container = 메타데이터 (어떻게 실행할 것인가)")
	fmt.Println("  Task      = 런타임 상태 (실제 프로세스 실행)")

	// =====================================================================
	// 4. Task 상태 머신
	// =====================================================================
	fmt.Println("\n\n[4] Task 상태 머신 (core/runtime/task.go)")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println("\n  Task 상태 전이 다이어그램:")
	fmt.Println()
	fmt.Println("  ┌─────────┐  Start()  ┌─────────┐  Kill()   ┌─────────┐  Delete() ┌─────────┐")
	fmt.Println("  │ Created │ ────────> │ Running │ ────────> │ Stopped │ ────────> │ Deleted │")
	fmt.Println("  └─────────┘           └────┬────┘           └─────────┘           └─────────┘")
	fmt.Println("                              │   ▲")
	fmt.Println("                    Pause()   │   │ Resume()")
	fmt.Println("                              ▼   │")
	fmt.Println("                        ┌──────────┐")
	fmt.Println("                        │  Paused  │")
	fmt.Println("                        └──────────┘")

	// Task 시뮬레이션
	task := &Task{
		ID:          container.ID,
		ContainerID: container.ID,
		Namespace:   "default",
	}

	fmt.Println("\n  Task 상태 전이 시뮬레이션:")

	// Create → Start → Pause → Resume → Stop → Delete
	task.Create(12345)
	fmt.Printf("    %s\n", task.transitions[len(task.transitions)-1])

	task.Start()
	fmt.Printf("    %s\n", task.transitions[len(task.transitions)-1])

	task.Pause()
	fmt.Printf("    %s\n", task.transitions[len(task.transitions)-2])
	fmt.Printf("    %s\n", task.transitions[len(task.transitions)-1])

	task.Resume()
	fmt.Printf("    %s\n", task.transitions[len(task.transitions)-1])

	task.Stop(0)
	fmt.Printf("    %s\n", task.transitions[len(task.transitions)-1])

	task.Delete()
	fmt.Printf("    %s\n", task.transitions[len(task.transitions)-1])

	fmt.Printf("\n  전체 전이 이력: %v\n", task.transitions)

	// 비정상 전이 시도
	fmt.Println("\n  비정상 전이 시도:")
	task2 := &Task{ID: "bad-task", Namespace: "test"}
	task2.Create(99999)
	if err := task2.Pause(); err != nil {
		fmt.Printf("    Created→Pause: %v (차단됨)\n", err)
	}
	task2.Start()
	if err := task2.Resume(); err != nil {
		fmt.Printf("    Running→Resume: %v (차단됨)\n", err)
	}

	// =====================================================================
	// 5. Content Info
	// =====================================================================
	fmt.Println("\n\n[5] Content Info (core/content/content.go)")
	fmt.Println(strings.Repeat("-", 60))

	contentInfos := []ContentInfo{
		{
			Digest:    manifestDescriptor.Digest,
			Size:      manifestDescriptor.Size,
			CreatedAt: now,
			UpdatedAt: now,
			Labels:    map[string]string{"containerd.io/gc.ref.content.0": configDescriptor.Digest},
		},
		{
			Digest:    configDescriptor.Digest,
			Size:      configDescriptor.Size,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			Digest:    layerDescriptor.Digest,
			Size:      layerDescriptor.Size,
			CreatedAt: now,
			UpdatedAt: now,
			Labels:    map[string]string{"containerd.io/uncompressed": computeDigest(layerData)},
		},
	}

	fmt.Println("\n  Content Store에 저장된 blob 목록:")
	fmt.Println("  ┌────┬──────────────────────┬───────┬────────────────────────────────────┐")
	fmt.Println("  │ #  │ Type                 │ Size  │ Digest (앞 40자)                   │")
	fmt.Println("  ├────┼──────────────────────┼───────┼────────────────────────────────────┤")
	types := []string{"Manifest", "Config", "Layer"}
	for i, ci := range contentInfos {
		digestShort := ci.Digest
		if len(digestShort) > 47 {
			digestShort = digestShort[:47] + "..."
		}
		fmt.Printf("  │ %d  │ %-20s │ %5d │ %-34s │\n", i+1, types[i], ci.Size, digestShort)
	}
	fmt.Println("  └────┴──────────────────────┴───────┴────────────────────────────────────┘")

	// Content 쓰기 상태
	writeStatus := ContentStatus{
		Ref:       "pull-nginx-layer-0",
		Offset:    1024 * 512,
		Total:     1024 * 1024,
		Expected:  layerDescriptor.Digest,
		StartedAt: now.Add(-5 * time.Second),
		UpdatedAt: now,
	}
	fmt.Printf("\n  진행 중인 쓰기 작업 (Ingestion):\n  %s\n", prettyJSON(writeStatus))

	// =====================================================================
	// 6. Snapshot Info + Mount
	// =====================================================================
	fmt.Println("\n\n[6] Snapshot Info (core/snapshots/snapshotter.go)")
	fmt.Println(strings.Repeat("-", 60))

	snapshots := []SnapshotInfo{
		{
			Kind:    KindCommitted,
			Name:    "sha256:base-layer-digest",
			Parent:  "",
			Created: now.Add(-10 * time.Minute),
			Updated: now.Add(-10 * time.Minute),
			Labels:  map[string]string{"containerd.io/snapshot.ref": "layer-0"},
		},
		{
			Kind:    KindCommitted,
			Name:    "sha256:app-layer-digest",
			Parent:  "sha256:base-layer-digest",
			Created: now.Add(-5 * time.Minute),
			Updated: now.Add(-5 * time.Minute),
			Labels:  map[string]string{"containerd.io/snapshot.ref": "layer-1"},
		},
		{
			Kind:    KindActive,
			Name:    "nginx-web-001-snapshot",
			Parent:  "sha256:app-layer-digest",
			Created: now,
			Updated: now,
		},
		{
			Kind:    KindView,
			Name:    "nginx-inspect-view",
			Parent:  "sha256:app-layer-digest",
			Created: now,
			Updated: now,
		},
	}

	fmt.Println("\n  스냅샷 레이어 스택 (Parent-Child 관계):")
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────────────────────────────┐")
	fmt.Println("  │ Active: nginx-web-001-snapshot (writable, 컨테이너 레이어)│")
	fmt.Println("  │   └─ Parent: sha256:app-layer-digest (Committed)        │")
	fmt.Println("  │       └─ Parent: sha256:base-layer-digest (Committed)   │")
	fmt.Println("  │           └─ Parent: (none) — 빈 루트                   │")
	fmt.Println("  ├──────────────────────────────────────────────────────────┤")
	fmt.Println("  │ View: nginx-inspect-view (readonly, 검사용)              │")
	fmt.Println("  │   └─ Parent: sha256:app-layer-digest (Committed)        │")
	fmt.Println("  └──────────────────────────────────────────────────────────┘")

	fmt.Println("\n  스냅샷 목록:")
	fmt.Println("  ┌────┬────────────┬─────────────────────────┬─────────────────────────┐")
	fmt.Println("  │ #  │ Kind       │ Name                    │ Parent                  │")
	fmt.Println("  ├────┼────────────┼─────────────────────────┼─────────────────────────┤")
	for i, s := range snapshots {
		parent := s.Parent
		if parent == "" {
			parent = "(none)"
		}
		if len(s.Name) > 23 {
			s.Name = s.Name[:23]
		}
		if len(parent) > 23 {
			parent = parent[:23]
		}
		fmt.Printf("  │ %d  │ %-10s │ %-23s │ %-23s │\n", i+1, s.Kind, s.Name, parent)
	}
	fmt.Println("  └────┴────────────┴─────────────────────────┴─────────────────────────┘")

	// Kind 전이
	fmt.Println("\n  Snapshot Kind 전이:")
	fmt.Println("    Prepare(key, parent) → Active (읽기/쓰기)")
	fmt.Println("    Commit(name, key)    → Committed (불변, 새 parent가 될 수 있음)")
	fmt.Println("    View(key, parent)    → View (읽기 전용, commit 불가)")

	// Overlay Mount 예시
	fmt.Println("\n  Overlay 마운트 구성 (Active 스냅샷):")
	overlayMount := Mount{
		Type:   "overlay",
		Source: "overlay",
		Options: []string{
			"workdir=/var/lib/containerd/snapshots/100/work",
			"upperdir=/var/lib/containerd/snapshots/100/fs",
			"lowerdir=/var/lib/containerd/snapshots/50/fs:/var/lib/containerd/snapshots/10/fs",
			"index=off",
		},
	}
	fmt.Printf("  %s\n", prettyJSON(overlayMount))

	// =====================================================================
	// 7. 전체 데이터 흐름 요약
	// =====================================================================
	fmt.Println("\n\n[7] 전체 데이터 흐름")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println(`
  이미지 Pull → 컨테이너 실행 전체 흐름:

  1. Image Pull
     Registry ──pull──> Content Store (blobs/sha256/{digest})
     - manifest, config, layers를 digest-keyed blob으로 저장

  2. Image Unpack
     Content Store ──unpack──> Snapshotter
     - 각 layer를 Prepare → mount → 압축해제 → Commit
     - Parent-Child 체인 형성

  3. Container Create
     Container{Image, Runtime, Spec, SnapshotKey, Snapshotter}
     - Snapshotter.Prepare(containerKey, imageChainID) → writable layer
     - 메타데이터를 boltdb에 저장

  4. Task Create
     Container ──create task──> Runtime(runc)
     - Snapshotter.Mounts(containerKey) → overlay mount
     - OCI 스펙 + mount로 runc create 호출
     - Task 상태: Created

  5. Task Start
     Task.Start() → runc start
     - 상태: Created → Running
     - 컨테이너 프로세스 실행

  6. Task Stop + Delete
     Task.Kill() → runc kill → Stopped
     Task.Delete() → runc delete + Snapshotter.Remove()

  데이터 저장소 매핑:
  ┌─────────────────┬──────────────────────────────────┐
  │ 데이터           │ 저장 위치                        │
  ├─────────────────┼──────────────────────────────────┤
  │ Content blobs   │ /var/lib/containerd/io.containerd│
  │                 │ .content.v1/blobs/sha256/        │
  │ Snapshots       │ /var/lib/containerd/io.containerd│
  │                 │ .snapshotter.v1.overlayfs/       │
  │ Metadata (bolt) │ /var/lib/containerd/io.containerd│
  │                 │ .metadata.v1/meta.db             │
  │ Runtime state   │ /run/containerd/runc/default/    │
  └─────────────────┴──────────────────────────────────┘`)

	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("containerd 데이터 모델 PoC 완료")
	fmt.Println(strings.Repeat("=", 70))
}
