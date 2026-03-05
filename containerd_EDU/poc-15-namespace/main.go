package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// containerd 네임스페이스 격리 시뮬레이션
// =============================================================================
//
// containerd의 네임스페이스는 Linux 네임스페이스(pid, net, mnt)와 다른 개념이다.
// containerd 네임스페이스는 멀티 테넌트 격리를 위한 논리적 파티셔닝으로,
// Docker, Kubernetes, BuildKit 등 서로 다른 클라이언트가 같은 containerd를
// 공유하면서도 리소스가 충돌하지 않도록 한다.
//
// 핵심 설계:
//   - context.Context에 네임스페이스를 주입 (WithNamespace)
//   - 모든 API 호출에서 네임스페이스를 추출하여 리소스 범위를 결정
//   - BoltDB에서 v1/<namespace>/... 경로로 데이터를 격리
//   - 같은 이미지도 네임스페이스별로 독립 관리
//
// 실제 코드 참조:
//   - pkg/namespaces/context.go        — WithNamespace, Namespace, NamespaceRequired
//   - core/metadata/namespaces.go      — namespaceStore (BoltDB CRUD)
//   - core/metadata/buckets.go         — BoltDB 스키마 (v1/<ns>/images, containers 등)
//   - api/services/namespaces/v1/      — gRPC 서비스 정의
// =============================================================================

// --- 네임스페이스 Context 키 ---
// 실제 코드: pkg/namespaces/context.go
// containerd는 context.Context에 네임스페이스를 저장하여 모든 API 호출에서 사용한다.
// 환경변수 CONTAINERD_NAMESPACE 또는 gRPC 헤더를 통해 설정할 수 있다.

type namespaceKey struct{}

const (
	// NamespaceEnvVar는 환경변수 키이다.
	// 실제 코드: pkg/namespaces/context.go 라인 30
	NamespaceEnvVar = "CONTAINERD_NAMESPACE"

	// DefaultNamespace는 기본 네임스페이스이다.
	// 실제 코드: pkg/namespaces/context.go 라인 32 — Default = "default"
	DefaultNamespace = "default"
)

// WithNamespace는 context에 네임스페이스를 설정한다.
// 실제 코드: pkg/namespaces/context.go - WithNamespace
// 내부적으로 gRPC/tTRPC 메타데이터 헤더에도 네임스페이스를 함께 설정한다.
func WithNamespace(ctx context.Context, namespace string) context.Context {
	return context.WithValue(ctx, namespaceKey{}, namespace)
}

// Namespace는 context에서 네임스페이스를 추출한다.
// 실제 코드: pkg/namespaces/context.go - Namespace
// context.Value → gRPC 헤더 → tTRPC 헤더 순으로 확인한다.
func Namespace(ctx context.Context) (string, bool) {
	ns, ok := ctx.Value(namespaceKey{}).(string)
	return ns, ok
}

// NamespaceRequired는 유효한 네임스페이스를 반환하거나 에러를 반환한다.
// 실제 코드: pkg/namespaces/context.go - NamespaceRequired
// 네임스페이스가 없거나 유효하지 않으면 ErrFailedPrecondition을 반환한다.
func NamespaceRequired(ctx context.Context) (string, error) {
	ns, ok := Namespace(ctx)
	if !ok || ns == "" {
		return "", fmt.Errorf("namespace is required")
	}
	return ns, nil
}

// --- 네임스페이스별 리소스 격리 모델 ---
// 실제 BoltDB 스키마:
//   v1/<namespace>/images/<image-name>/...
//   v1/<namespace>/containers/<container-id>/...
//   v1/<namespace>/content/blob/<digest>/...
//   v1/<namespace>/snapshots/<snapshotter>/<key>/...
//   v1/<namespace>/sandboxes/<sandbox-id>/...
//   v1/<namespace>/leases/<lease-id>/...
//
// 참조: core/metadata/buckets.go 주석 (라인 29~138)

// --- 리소스 타입 정의 ---

// Image는 네임스페이스별로 관리되는 이미지 메타데이터이다.
type Image struct {
	Name      string
	Digest    string
	MediaType string
	Size      int64
	Labels    map[string]string
	CreatedAt time.Time
}

// Container는 네임스페이스별로 관리되는 컨테이너 메타데이터이다.
type Container struct {
	ID          string
	Image       string
	Snapshotter string
	SnapshotKey string
	Runtime     string
	Labels      map[string]string
	CreatedAt   time.Time
}

// Snapshot은 네임스페이스별로 관리되는 스냅샷 참조이다.
type Snapshot struct {
	Key         string
	Parent      string
	Snapshotter string
	Labels      map[string]string
}

// --- 네임스페이스 Store ---
// 실제 코드: core/metadata/namespaces.go - namespaceStore
// BoltDB의 최상위 버킷(v1)에서 네임스페이스별 서브 버킷을 관리한다.

type NamespaceStore struct {
	mu         sync.RWMutex
	namespaces map[string]*NamespaceData
}

// NamespaceData는 한 네임스페이스의 모든 리소스를 담는다.
// BoltDB에서는 v1/<namespace>/ 아래 각 리소스 버킷으로 구분된다.
type NamespaceData struct {
	Labels     map[string]string
	Images     map[string]Image
	Containers map[string]Container
	Snapshots  map[string]Snapshot
	CreatedAt  time.Time
}

func NewNamespaceStore() *NamespaceStore {
	return &NamespaceStore{
		namespaces: make(map[string]*NamespaceData),
	}
}

// Create는 네임스페이스를 생성한다.
// 실제 코드: core/metadata/namespaces.go - namespaceStore.Create
// BoltDB에서 v1/<namespace> 버킷을 생성한다.
func (ns *NamespaceStore) Create(name string, labels map[string]string) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	if _, exists := ns.namespaces[name]; exists {
		return fmt.Errorf("namespace %q: already exists", name)
	}

	ns.namespaces[name] = &NamespaceData{
		Labels:     labels,
		Images:     make(map[string]Image),
		Containers: make(map[string]Container),
		Snapshots:  make(map[string]Snapshot),
		CreatedAt:  time.Now(),
	}

	return nil
}

// List는 모든 네임스페이스를 반환한다.
// 실제 코드: core/metadata/namespaces.go - namespaceStore.List
// v1 버킷의 모든 서브 버킷 이름을 반환한다.
func (ns *NamespaceStore) List() []string {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	var result []string
	for name := range ns.namespaces {
		result = append(result, name)
	}
	return result
}

// Delete는 네임스페이스를 삭제한다.
// 실제 코드: core/metadata/namespaces.go - namespaceStore.Delete
// 네임스페이스 안에 리소스가 남아 있으면 삭제를 거부한다 (ErrFailedPrecondition).
func (ns *NamespaceStore) Delete(name string) error {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	data, ok := ns.namespaces[name]
	if !ok {
		return fmt.Errorf("namespace %q: not found", name)
	}

	// 남아 있는 리소스 확인
	// 실제 코드: core/metadata/namespaces.go - listNs()
	// images, blobs, containers, snapshots 중 하나라도 비어있지 않으면 거부한다.
	var remaining []string
	if len(data.Images) > 0 {
		remaining = append(remaining, "images")
	}
	if len(data.Containers) > 0 {
		remaining = append(remaining, "containers")
	}
	if len(data.Snapshots) > 0 {
		remaining = append(remaining, "snapshots")
	}

	if len(remaining) > 0 {
		return fmt.Errorf("namespace %q must be empty, but it still has %s",
			name, strings.Join(remaining, ", "))
	}

	delete(ns.namespaces, name)
	return nil
}

// GetNamespace는 네임스페이스 데이터를 반환한다.
func (ns *NamespaceStore) GetNamespace(name string) (*NamespaceData, error) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	data, ok := ns.namespaces[name]
	if !ok {
		return nil, fmt.Errorf("namespace %q: not found", name)
	}
	return data, nil
}

// --- 네임스페이스 격리 API 시뮬레이션 ---
// containerd의 모든 API는 context에서 네임스페이스를 추출하여
// 해당 네임스페이스의 리소스에만 접근한다.

type MetadataService struct {
	store *NamespaceStore
}

func NewMetadataService(store *NamespaceStore) *MetadataService {
	return &MetadataService{store: store}
}

// CreateImage는 네임스페이스에 이미지를 생성한다.
// context에서 네임스페이스를 추출하여 해당 네임스페이스의 이미지 맵에 저장한다.
func (ms *MetadataService) CreateImage(ctx context.Context, img Image) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	ms.store.mu.Lock()
	defer ms.store.mu.Unlock()

	data, ok := ms.store.namespaces[ns]
	if !ok {
		return fmt.Errorf("namespace %q not found", ns)
	}

	img.CreatedAt = time.Now()
	data.Images[img.Name] = img
	return nil
}

// ListImages는 네임스페이스의 이미지를 조회한다.
func (ms *MetadataService) ListImages(ctx context.Context) ([]Image, error) {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	ms.store.mu.RLock()
	defer ms.store.mu.RUnlock()

	data, ok := ms.store.namespaces[ns]
	if !ok {
		return nil, fmt.Errorf("namespace %q not found", ns)
	}

	var result []Image
	for _, img := range data.Images {
		result = append(result, img)
	}
	return result, nil
}

// DeleteImage는 네임스페이스의 이미지를 삭제한다.
func (ms *MetadataService) DeleteImage(ctx context.Context, name string) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	ms.store.mu.Lock()
	defer ms.store.mu.Unlock()

	data, ok := ms.store.namespaces[ns]
	if !ok {
		return fmt.Errorf("namespace %q not found", ns)
	}

	if _, ok := data.Images[name]; !ok {
		return fmt.Errorf("image %q not found in namespace %q", name, ns)
	}

	delete(data.Images, name)
	return nil
}

// CreateContainer는 네임스페이스에 컨테이너를 생성한다.
func (ms *MetadataService) CreateContainer(ctx context.Context, ctr Container) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	ms.store.mu.Lock()
	defer ms.store.mu.Unlock()

	data, ok := ms.store.namespaces[ns]
	if !ok {
		return fmt.Errorf("namespace %q not found", ns)
	}

	ctr.CreatedAt = time.Now()
	data.Containers[ctr.ID] = ctr
	return nil
}

// ListContainers는 네임스페이스의 컨테이너를 조회한다.
func (ms *MetadataService) ListContainers(ctx context.Context) ([]Container, error) {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	ms.store.mu.RLock()
	defer ms.store.mu.RUnlock()

	data, ok := ms.store.namespaces[ns]
	if !ok {
		return nil, fmt.Errorf("namespace %q not found", ns)
	}

	var result []Container
	for _, ctr := range data.Containers {
		result = append(result, ctr)
	}
	return result, nil
}

// DeleteContainer는 네임스페이스의 컨테이너를 삭제한다.
func (ms *MetadataService) DeleteContainer(ctx context.Context, id string) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	ms.store.mu.Lock()
	defer ms.store.mu.Unlock()

	data, ok := ms.store.namespaces[ns]
	if !ok {
		return fmt.Errorf("namespace %q not found", ns)
	}

	delete(data.Containers, id)
	return nil
}

// =============================================================================
// 메인 함수 — 네임스페이스 격리 시뮬레이션
// =============================================================================

func main() {
	fmt.Println("=== containerd 네임스페이스 격리 시뮬레이션 ===")
	fmt.Println()

	store := NewNamespaceStore()
	svc := NewMetadataService(store)

	// --- 1. 네임스페이스 생성 ---
	// containerd는 기본적으로 "default" 네임스페이스를 사용한다.
	// Docker는 "moby", Kubernetes는 "k8s.io" 네임스페이스를 사용한다.
	fmt.Println("[1] 네임스페이스 생성")
	fmt.Println(strings.Repeat("-", 60))

	namespaces := []struct {
		name   string
		labels map[string]string
		desc   string
	}{
		{
			name:   "default",
			labels: map[string]string{"description": "default namespace"},
			desc:   "기본 네임스페이스 (ctr 명령어 등)",
		},
		{
			name:   "moby",
			labels: map[string]string{"owner": "docker"},
			desc:   "Docker 엔진 전용",
		},
		{
			name:   "k8s.io",
			labels: map[string]string{"owner": "kubernetes", "managed-by": "kubelet"},
			desc:   "Kubernetes CRI 전용",
		},
		{
			name:   "buildkit",
			labels: map[string]string{"owner": "buildkit"},
			desc:   "BuildKit 빌더 전용",
		},
	}

	for _, ns := range namespaces {
		err := store.Create(ns.name, ns.labels)
		if err != nil {
			fmt.Printf("  [오류] %s: %v\n", ns.name, err)
			continue
		}
		fmt.Printf("  [생성] %-12s — %s\n", ns.name, ns.desc)
	}
	fmt.Println()

	// --- 2. 같은 이미지를 다른 네임스페이스에서 독립 관리 ---
	// containerd에서 네임스페이스가 다르면 같은 이미지 이름이어도 별개의 리소스이다.
	// BoltDB 경로: v1/moby/images/nginx:latest vs v1/k8s.io/images/nginx:latest
	fmt.Println("[2] 같은 이미지를 다른 네임스페이스에서 독립 관리")
	fmt.Println(strings.Repeat("-", 60))

	// Docker (moby 네임스페이스)에서 nginx 이미지 풀
	dockerCtx := WithNamespace(context.Background(), "moby")
	err := svc.CreateImage(dockerCtx, Image{
		Name:      "docker.io/library/nginx:1.25",
		Digest:    "sha256:aaa111",
		MediaType: "application/vnd.docker.distribution.manifest.v2+json",
		Size:      1024,
		Labels:    map[string]string{"docker.pulled": "true"},
	})
	if err != nil {
		fmt.Printf("  [오류] %v\n", err)
	} else {
		fmt.Println("  [moby]   이미지 추가: docker.io/library/nginx:1.25 (digest: sha256:aaa111)")
	}

	// Kubernetes (k8s.io 네임스페이스)에서 같은 이미지를 다른 digest로 풀
	k8sCtx := WithNamespace(context.Background(), "k8s.io")
	err = svc.CreateImage(k8sCtx, Image{
		Name:      "docker.io/library/nginx:1.25",
		Digest:    "sha256:bbb222",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Size:      2048,
		Labels:    map[string]string{"io.cri-containerd.image": "managed"},
	})
	if err != nil {
		fmt.Printf("  [오류] %v\n", err)
	} else {
		fmt.Println("  [k8s.io] 이미지 추가: docker.io/library/nginx:1.25 (digest: sha256:bbb222)")
	}

	// BuildKit에서 빌드 중간 이미지 추가
	buildkitCtx := WithNamespace(context.Background(), "buildkit")
	err = svc.CreateImage(buildkitCtx, Image{
		Name:      "docker.io/library/nginx:1.25",
		Digest:    "sha256:ccc333",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Size:      3072,
	})
	if err != nil {
		fmt.Printf("  [오류] %v\n", err)
	} else {
		fmt.Println("  [buildkit] 이미지 추가: docker.io/library/nginx:1.25 (digest: sha256:ccc333)")
	}
	fmt.Println()

	// --- 3. 네임스페이스별 이미지 조회 ---
	// 각 네임스페이스는 자신의 이미지만 볼 수 있다.
	fmt.Println("[3] 네임스페이스별 이미지 조회 (격리 확인)")
	fmt.Println(strings.Repeat("-", 60))

	for _, nsName := range []string{"moby", "k8s.io", "buildkit", "default"} {
		ctx := WithNamespace(context.Background(), nsName)
		imgs, err := svc.ListImages(ctx)
		if err != nil {
			fmt.Printf("  [%s] 오류: %v\n", nsName, err)
			continue
		}
		fmt.Printf("  [%-8s] 이미지 수: %d\n", nsName, len(imgs))
		for _, img := range imgs {
			fmt.Printf("             %s (digest: %s)\n", img.Name, img.Digest)
		}
	}
	fmt.Println()

	// --- 4. 네임스페이스별 컨테이너 격리 ---
	fmt.Println("[4] 네임스페이스별 컨테이너 격리")
	fmt.Println(strings.Repeat("-", 60))

	// Docker 컨테이너
	_ = svc.CreateContainer(dockerCtx, Container{
		ID:          "docker-nginx-001",
		Image:       "docker.io/library/nginx:1.25",
		Runtime:     "io.containerd.runc.v2",
		Snapshotter: "overlayfs",
	})
	_ = svc.CreateContainer(dockerCtx, Container{
		ID:          "docker-redis-002",
		Image:       "docker.io/library/redis:7",
		Runtime:     "io.containerd.runc.v2",
		Snapshotter: "overlayfs",
	})
	fmt.Println("  [moby]   컨테이너 2개 생성: docker-nginx-001, docker-redis-002")

	// Kubernetes 컨테이너
	_ = svc.CreateContainer(k8sCtx, Container{
		ID:      "k8s-nginx-pod-abc123",
		Image:   "docker.io/library/nginx:1.25",
		Runtime: "io.containerd.runc.v2",
		Labels: map[string]string{
			"io.kubernetes.pod.name": "nginx-deployment-7fb96c846b-x4k2n",
		},
	})
	fmt.Println("  [k8s.io] 컨테이너 1개 생성: k8s-nginx-pod-abc123")
	fmt.Println()

	// 각 네임스페이스에서 컨테이너 조회
	for _, nsName := range []string{"moby", "k8s.io", "default"} {
		ctx := WithNamespace(context.Background(), nsName)
		ctrs, _ := svc.ListContainers(ctx)
		fmt.Printf("  [%-8s] 컨테이너 수: %d\n", nsName, len(ctrs))
		for _, ctr := range ctrs {
			fmt.Printf("             ID=%s  Image=%s\n", ctr.ID, ctr.Image)
		}
	}
	fmt.Println()

	// --- 5. NamespaceRequired 에러 처리 ---
	// 네임스페이스가 없는 context로 API를 호출하면 에러가 발생한다.
	fmt.Println("[5] NamespaceRequired 에러 처리")
	fmt.Println(strings.Repeat("-", 60))

	emptyCtx := context.Background()
	_, err = svc.ListImages(emptyCtx)
	fmt.Printf("  네임스페이스 없는 context: %v\n", err)

	ns, ok := Namespace(emptyCtx)
	fmt.Printf("  Namespace(emptyCtx): ns=%q, ok=%v\n", ns, ok)
	fmt.Println()

	// --- 6. 네임스페이스 삭제 (리소스 있으면 거부) ---
	// 실제 코드: core/metadata/namespaces.go - namespaceStore.Delete
	// 네임스페이스 안에 images, containers, snapshots 등이 남아 있으면
	// ErrFailedPrecondition을 반환하여 삭제를 거부한다.
	fmt.Println("[6] 네임스페이스 삭제 — 리소스 잔존 시 거부")
	fmt.Println(strings.Repeat("-", 60))

	err = store.Delete("moby")
	fmt.Printf("  [moby] 삭제 시도 (리소스 있음): %v\n", err)

	err = store.Delete("default")
	if err != nil {
		fmt.Printf("  [default] 삭제 시도 (비어 있음): %v\n", err)
	} else {
		fmt.Println("  [default] 삭제 성공 (비어 있음)")
	}
	fmt.Println()

	// --- 7. moby 네임스페이스 정리 후 삭제 ---
	fmt.Println("[7] 리소스 정리 후 네임스페이스 삭제")
	fmt.Println(strings.Repeat("-", 60))

	_ = svc.DeleteImage(dockerCtx, "docker.io/library/nginx:1.25")
	_ = svc.DeleteContainer(dockerCtx, "docker-nginx-001")
	_ = svc.DeleteContainer(dockerCtx, "docker-redis-002")
	fmt.Println("  [moby] 이미지/컨테이너 모두 삭제")

	err = store.Delete("moby")
	if err != nil {
		fmt.Printf("  [moby] 삭제 재시도: %v\n", err)
	} else {
		fmt.Println("  [moby] 네임스페이스 삭제 성공")
	}

	fmt.Println()

	// --- 8. BoltDB 스키마 시각화 ---
	fmt.Println("[8] BoltDB 네임스페이스 스키마 (실제 저장 구조)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  실제 BoltDB 경로 (core/metadata/buckets.go):

  v1/                                    ← 스키마 버전
  ├── version: <varint>                  ← 마이그레이션 버전
  ├── moby/                              ← Docker 네임스페이스
  │   ├── labels/
  │   │   └── owner: "docker"
  │   ├── images/
  │   │   └── docker.io/library/nginx:1.25/
  │   │       ├── createdat: <binary>
  │   │       └── target/
  │   │           ├── digest: sha256:aaa111
  │   │           ├── mediatype: ...docker...
  │   │           └── size: 1024
  │   ├── containers/
  │   │   └── docker-nginx-001/
  │   │       ├── image: docker.io/library/nginx:1.25
  │   │       └── runtime/name: io.containerd.runc.v2
  │   ├── content/blob/
  │   │   └── sha256:aaa111/...
  │   └── snapshots/overlayfs/...
  │
  ├── k8s.io/                            ← Kubernetes 네임스페이스
  │   ├── labels/
  │   │   ├── owner: "kubernetes"
  │   │   └── managed-by: "kubelet"
  │   ├── images/
  │   │   └── docker.io/library/nginx:1.25/
  │   │       └── target/
  │   │           ├── digest: sha256:bbb222  ← 같은 이미지, 다른 digest!
  │   │           └── mediatype: ...oci...
  │   └── containers/
  │       └── k8s-nginx-pod-abc123/...
  │
  └── buildkit/                          ← BuildKit 네임스페이스
      └── images/
          └── docker.io/library/nginx:1.25/
              └── target/digest: sha256:ccc333  ← 또 다른 digest!
	`)

	// --- 남은 네임스페이스 출력 ---
	remaining := store.List()
	fmt.Printf("  현재 네임스페이스 목록: %v\n", remaining)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
