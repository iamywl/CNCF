# containerd 네임스페이스 격리 시스템

## 1. 개요

containerd의 네임스페이스(Namespace)는 단일 containerd 데몬 내에서 다중 테넌트(multi-tenancy)를 구현하는 격리 메커니즘이다. 각 네임스페이스는 독립적인 이미지, 컨테이너, 스냅샷, 리스 등의 리소스 공간을 가지며, 서로 다른 클라이언트가 동일한 리소스 이름을 사용해도 충돌하지 않는다.

### 왜 네임스페이스가 필요한가?

containerd는 여러 시스템이 동시에 사용하는 공유 인프라다:

```
단일 containerd 데몬을 사용하는 클라이언트들:

  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐
  │ kubelet  │  │ Docker   │  │ nerdctl  │  │ BuildKit │
  │ (CRI)    │  │ Engine   │  │          │  │          │
  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘
       │              │              │              │
  NS: "k8s.io"   NS: "moby"    NS: "default"  NS: "buildkit"
       │              │              │              │
       └──────────────┴──────────────┴──────────────┘
                          │
                ┌─────────▼──────────┐
                │ containerd daemon   │
                │                     │
                │ ┌───────────────┐  │
                │ │ 단일 BoltDB    │  │
                │ │ 단일 Content   │  │
                │ │   Store       │  │
                │ └───────────────┘  │
                └─────────────────────┘
```

네임스페이스가 없다면:
- kubelet이 생성한 "redis" 컨테이너와 Docker가 생성한 "redis" 컨테이너가 충돌
- 한 클라이언트가 다른 클라이언트의 이미지를 삭제할 수 있음
- 시스템 컨테이너와 사용자 컨테이너의 구분 불가

### 중요한 주의사항

네임스페이스는 **관리적 격리(administrative isolation)** 이지 **보안 격리(security isolation)** 가 아니다. 클라이언트가 네임스페이스를 전환하는 것은 사소한 일이다. 보안 격리가 필요하면 별도의 containerd 인스턴스나 OS 수준 격리를 사용해야 한다.

---

## 2. 아키텍처

### 2.1 네임스페이스의 위치

```
┌──────────────────────────────────────────────────────────┐
│                  containerd 요청 흐름                      │
│                                                          │
│  Client                                                  │
│    │                                                     │
│    ├─ context.WithValue(namespaceKey{}, "k8s.io")       │
│    │  + gRPC Header: "containerd-namespace: k8s.io"     │
│    │  + ttrpc Header: "containerd-namespace-ttrpc: ..." │
│    │                                                     │
│    v                                                     │
│  gRPC Server                                             │
│    │                                                     │
│    ├─ Interceptor: context에서 namespace 추출            │
│    │                                                     │
│    v                                                     │
│  Service Layer                                           │
│    │                                                     │
│    ├─ namespaces.NamespaceRequired(ctx)                  │
│    │  → "k8s.io"                                        │
│    │                                                     │
│    v                                                     │
│  Metadata DB (BoltDB)                                    │
│    │                                                     │
│    └─ v1/k8s.io/images/...   ← 이 네임스페이스의 데이터만│
│       v1/k8s.io/containers/... 접근                      │
│       v1/k8s.io/leases/...                               │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 2.2 BoltDB에서의 네임스페이스 구조

```
소스코드: core/metadata/buckets.go 참조

└── v1                              ← 스키마 버전
    ├── version : <varint>          ← DB 버전
    │
    ├── k8s.io                      ← 네임스페이스 1
    │   ├── labels                  ← 네임스페이스 레이블
    │   │   ├── "containerd.io/defaults/snapshotter" : "overlayfs"
    │   │   └── "containerd.io/defaults/runtime" : "runc"
    │   ├── images                  ← 이미지 (이 NS 전용)
    │   ├── containers              ← 컨테이너 (이 NS 전용)
    │   ├── snapshots               ← 스냅샷 참조 (이 NS 전용)
    │   ├── content                 ← 콘텐츠 참조 (이 NS 전용)
    │   ├── leases                  ← 리스 (이 NS 전용)
    │   └── sandboxes               ← 샌드박스 (이 NS 전용)
    │
    ├── moby                        ← 네임스페이스 2 (Docker)
    │   ├── labels
    │   ├── images
    │   ├── containers
    │   └── ...
    │
    └── default                     ← 네임스페이스 3 (기본)
        ├── labels
        ├── images
        └── ...
```

핵심: 모든 리소스(images, containers, snapshots, leases, sandboxes)가 네임스페이스 버킷 아래에 중첩된다. 동일한 이름의 리소스도 다른 네임스페이스에서는 독립적이다.

---

## 3. Context 기반 네임스페이스 전파

### 3.1 핵심 패키지: pkg/namespaces

```go
// 소스코드: pkg/namespaces/context.go

const (
    // 환경변수로 기본 네임스페이스 설정 가능
    NamespaceEnvVar = "CONTAINERD_NAMESPACE"
    // 기본 네임스페이스 이름
    Default = "default"
)

type namespaceKey struct{}
```

### 3.2 네임스페이스 설정

```go
// 소스코드: pkg/namespaces/context.go

// WithNamespace: context에 네임스페이스 설정
func WithNamespace(ctx context.Context, namespace string) context.Context {
    // 1. Go context에 값으로 저장
    ctx = context.WithValue(ctx, namespaceKey{}, namespace)
    // 2. gRPC 헤더에도 설정 (원격 호출 시 전파)
    // 3. ttrpc 헤더에도 설정 (shim 통신 시 전파)
    return withTTRPCNamespaceHeader(
        withGRPCNamespaceHeader(ctx, namespace), namespace)
}
```

왜 3곳에 저장하는가?

```
네임스페이스 전파 경로:

  1. context.Value → 동일 프로세스 내 함수 호출
     Client ──→ Service ──→ Metadata

  2. gRPC Header → containerd 데몬과의 원격 호출
     Client Process ──gRPC──→ containerd daemon

  3. ttrpc Header → shim과의 통신
     containerd daemon ──ttrpc──→ shim process

  세 경로 모두 동일한 네임스페이스를 전달해야 일관성 유지
```

### 3.3 네임스페이스 추출

```go
// 소스코드: pkg/namespaces/context.go

// Namespace: context에서 네임스페이스 추출 (유효성 미검증)
func Namespace(ctx context.Context) (string, bool) {
    namespace, ok := ctx.Value(namespaceKey{}).(string)
    if !ok {
        // context에 없으면 gRPC 헤더 확인
        if namespace, ok = fromGRPCHeader(ctx); !ok {
            // gRPC에도 없으면 ttrpc 헤더 확인
            return fromTTRPCHeader(ctx)
        }
    }
    return namespace, ok
}

// NamespaceRequired: 유효한 네임스페이스 필수 (대부분의 API가 사용)
func NamespaceRequired(ctx context.Context) (string, error) {
    namespace, ok := Namespace(ctx)
    if !ok || namespace == "" {
        return "", fmt.Errorf("namespace is required: %w",
            errdefs.ErrFailedPrecondition)
    }
    // 네임스페이스 이름 유효성 검증 (identifiers.Validate)
    if err := identifiers.Validate(namespace); err != nil {
        return "", fmt.Errorf("namespace validation: %w", err)
    }
    return namespace, nil
}
```

### 3.4 환경변수를 통한 기본 네임스페이스

```go
// 소스코드: pkg/namespaces/context.go

func NamespaceFromEnv(ctx context.Context) context.Context {
    namespace := os.Getenv(NamespaceEnvVar) // CONTAINERD_NAMESPACE
    if namespace == "" {
        namespace = Default // "default"
    }
    return WithNamespace(ctx, namespace)
}
```

---

## 4. Namespace Store (메타데이터 관리)

### 4.1 Store 인터페이스

```go
// pkg/namespaces 패키지의 Store 인터페이스

type Store interface {
    Create(ctx context.Context, namespace string, labels map[string]string) error
    Labels(ctx context.Context, namespace string) (map[string]string, error)
    SetLabel(ctx context.Context, namespace, key, value string) error
    List(ctx context.Context) ([]string, error)
    Delete(ctx context.Context, namespace string, opts ...DeleteOpts) error
}
```

### 4.2 BoltDB 기반 구현

```go
// 소스코드: core/metadata/namespaces.go

type namespaceStore struct {
    tx *bolt.Tx
}

func NewNamespaceStore(tx *bolt.Tx) namespaces.Store {
    return &namespaceStore{tx: tx}
}
```

### 4.3 Create: 네임스페이스 생성

```go
// 소스코드: core/metadata/namespaces.go

func (s *namespaceStore) Create(ctx context.Context,
    namespace string, labels map[string]string) error {

    topbkt, _ := createBucketIfNotExists(s.tx, bucketKeyVersion)

    // 네임스페이스 이름 유효성 검증
    identifiers.Validate(namespace)

    // 레이블 유효성 검증
    for k, v := range labels {
        l.Validate(k, v)
    }

    // 새 네임스페이스 버킷 생성
    bkt, err := topbkt.CreateBucket([]byte(namespace))
    if err == errbolt.ErrBucketExists {
        return errdefs.ErrAlreadyExists
    }

    // 레이블 저장
    lbkt, _ := bkt.CreateBucketIfNotExists(bucketKeyObjectLabels)
    for k, v := range labels {
        lbkt.Put([]byte(k), []byte(v))
    }
    return nil
}
```

### 4.4 List: 모든 네임스페이스 조회

```go
// 소스코드: core/metadata/namespaces.go

func (s *namespaceStore) List(ctx context.Context) ([]string, error) {
    bkt := getBucket(s.tx, bucketKeyVersion)
    if bkt == nil {
        return nil, nil // 네임스페이스 없음
    }

    var namespaces []string
    bkt.ForEach(func(k, v []byte) error {
        if v != nil {
            return nil // 버킷이 아닌 값은 스킵 (예: version)
        }
        namespaces = append(namespaces, string(k))
        return nil
    })
    return namespaces, nil
}
```

### 4.5 Delete: 네임스페이스 삭제

```go
// 소스코드: core/metadata/namespaces.go

func (s *namespaceStore) Delete(ctx context.Context,
    namespace string, opts ...namespaces.DeleteOpts) error {

    // 네임스페이스 내 리소스 확인
    types, _ := s.listNs(namespace)
    if len(types) > 0 {
        return fmt.Errorf(
            "namespace %q must be empty, but it still has %s: %w",
            namespace, strings.Join(types, ", "),
            errdefs.ErrFailedPrecondition,
        )
    }

    // 버킷 삭제
    bkt.DeleteBucket([]byte(namespace))
    return nil
}
```

핵심: 네임스페이스 삭제 전 **반드시 비어 있어야** 한다. 이미지, 컨테이너, blob, 스냅샷이 남아있으면 삭제 불가.

### 4.6 listNs: 네임스페이스 내 리소스 확인

```go
// 소스코드: core/metadata/namespaces.go

func (s *namespaceStore) listNs(namespace string) ([]string, error) {
    var out []string

    // 각 리소스 유형의 버킷이 비어있는지 확인
    if !isBucketEmpty(getImagesBucket(s.tx, namespace)) {
        out = append(out, "images")
    }
    if !isBucketEmpty(getBlobsBucket(s.tx, namespace)) {
        out = append(out, "blobs")
    }
    if !isBucketEmpty(getContainersBucket(s.tx, namespace)) {
        out = append(out, "containers")
    }

    // 스냅샷터별 확인
    if snbkt := getSnapshottersBucket(s.tx, namespace); snbkt != nil {
        snbkt.ForEach(func(k, v []byte) error {
            if v == nil {
                if !isBucketEmpty(snbkt.Bucket(k)) {
                    out = append(out, fmt.Sprintf(
                        "snapshots on %q snapshotter", k))
                }
            }
            return nil
        })
    }
    return out, nil
}

// 효율적인 빈 버킷 확인 (첫 번째 키만 확인)
func isBucketEmpty(bkt *bolt.Bucket) bool {
    if bkt == nil {
        return true
    }
    k, _ := bkt.Cursor().First()
    return k == nil
}
```

---

## 5. 네임스페이스 레이블

### 5.1 레이블의 역할

네임스페이스 레이블은 해당 네임스페이스의 기본 동작을 설정하는 데 사용된다.

```bash
# 네임스페이스의 기본 스냅샷터 설정
sudo ctr namespaces label k8s.io containerd.io/defaults/snapshotter=btrfs

# 네임스페이스의 기본 런타임 설정
sudo ctr namespaces label k8s.io containerd.io/defaults/runtime=testRuntime
```

### 5.2 지원되는 기본값 레이블

| 레이블 키 | 용도 |
|-----------|------|
| `containerd.io/defaults/snapshotter` | 기본 스냅샷터 |
| `containerd.io/defaults/runtime` | 기본 런타임 |
| `containerd.io/namespace.shareable` | 콘텐츠 공유 허용 (isolated 모드) |

### 5.3 SetLabel 구현

```go
// 소스코드: core/metadata/namespaces.go

func (s *namespaceStore) SetLabel(ctx context.Context,
    namespace, key, value string) error {

    // 레이블 키/값 유효성 검증
    l.Validate(key, value)

    return withNamespacesLabelsBucket(s.tx, namespace,
        func(bkt *bolt.Bucket) error {
            if value == "" {
                return bkt.Delete([]byte(key))  // 빈 값 = 삭제
            }
            return bkt.Put([]byte(key), []byte(value))
        })
}
```

---

## 6. 콘텐츠 공유 정책

### 6.1 Shared vs Isolated

containerd의 콘텐츠 스토어는 네임스페이스 간 콘텐츠 공유 정책을 지원한다.

```toml
# config.toml
[plugins."io.containerd.metadata.v1.bolt"]
  content_sharing_policy = "shared"  # 또는 "isolated"
```

```go
// 소스코드: core/metadata/db.go

// WithPolicyIsolated: 네임스페이스 간 콘텐츠 격리
func WithPolicyIsolated(o *dbOptions) {
    o.shared = false
}
```

### 6.2 Shared 모드 (기본)

```
Shared 모드 동작:

  네임스페이스 A가 이미지를 Pull:
    ┌────────────┐
    │ NS: k8s.io │
    │ Pull redis  │──→ Content Store에 blob 저장
    │ sha256:abc  │      └── blob은 전역
    └────────────┘

  네임스페이스 B에서 같은 blob 접근:
    ┌────────────┐
    │ NS: moby   │
    │ Pull redis  │──→ sha256:abc 이미 존재!
    │ sha256:abc  │    → 다운로드 스킵
    └────────────┘

  장점: 중복 다운로드 방지, 디스크 절약
  단점: digest만 알면 다른 NS의 blob 접근 가능
```

### 6.3 Isolated 모드

```
Isolated 모드 동작:

  네임스페이스 A가 이미지를 Pull:
    ┌────────────┐
    │ NS: k8s.io │
    │ Pull redis  │──→ Content Store에 blob 저장
    │ sha256:abc  │    + NS에 blob 참조 등록
    └────────────┘

  네임스페이스 B에서 접근 시도:
    ┌────────────┐
    │ NS: moby   │
    │ sha256:abc  │──→ 접근 거부!
    │             │    B가 직접 콘텐츠를 제공해야 함
    └────────────┘

  예외: "containerd.io/namespace.shareable=true" 레이블
    이 레이블이 있는 NS의 blob은 다른 NS에서도 접근 가능
```

### 6.4 왜 기본이 Shared인가?

대부분의 사용 사례에서 콘텐츠 공유가 이점이 더 크기 때문:

1. **Kubernetes 환경**: kubelet과 다른 도구가 같은 이미지를 사용
2. **CI/CD**: 빌드 캐시 공유로 시간 절약
3. **디스크 효율성**: 동일 blob 중복 저장 방지

Isolated 모드는 다중 테넌트 SaaS 환경처럼 보안이 중요한 경우에 사용한다.

---

## 7. 각 서비스에서의 네임스페이스 적용

### 7.1 Images 서비스

```go
// core/metadata/images.go 참조

func (s *imageStore) Get(ctx context.Context, name string) (images.Image, error) {
    ns, _ := namespaces.NamespaceRequired(ctx)

    view(ctx, s.db, func(tx *bolt.Tx) error {
        bkt := getImagesBucket(tx, ns)  // v1/<ns>/images
        // ns에 속한 이미지만 조회
    })
}
```

### 7.2 Containers 서비스

```go
// core/metadata/containers.go 참조

func getContainersBucket(tx *bolt.Tx, namespace string) *bolt.Bucket {
    return getBucket(tx,
        bucketKeyVersion,          // v1
        []byte(namespace),         // <namespace>
        bucketKeyObjectContainers) // containers
}
```

### 7.3 Leases 서비스

```go
// 소스코드: core/metadata/leases.go

func (lm *leaseManager) Create(ctx context.Context,
    opts ...leases.Opt) (leases.Lease, error) {
    // 네임스페이스 필수
    namespace, _ := namespaces.NamespaceRequired(ctx)

    update(ctx, lm.db, func(tx *bolt.Tx) error {
        // v1/<namespace>/leases 버킷에 생성
        topbkt, _ := createBucketIfNotExists(tx,
            bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases)
        // ...
    })
}
```

### 7.4 Sandboxes 서비스

```go
// 소스코드: core/metadata/sandbox.go

func (s *sandboxStore) Create(ctx context.Context,
    sandbox api.Sandbox) (api.Sandbox, error) {
    ns, _ := namespaces.NamespaceRequired(ctx)

    update(ctx, s.db, func(tx *bbolt.Tx) error {
        parent, _ := createSandboxBucket(tx, ns) // v1/<ns>/sandboxes
        return s.write(parent, &sandbox, false)
    })
}
```

### 7.5 Content 서비스 (특수 케이스)

콘텐츠 스토어는 네임스페이스 간 공유가 가능하므로 특수한 처리가 필요하다:

```
Content Store의 네임스페이스 처리:

  ┌──────────────────┐
  │ Metadata (BoltDB) │
  │                    │
  │ v1/k8s.io/content │  ← 네임스페이스별 참조 (어떤 blob을 보유?)
  │   └── blob        │
  │       ├── sha256:a │  → 메타데이터 (레이블, 타임스탬프)
  │       └── sha256:b │
  │                    │
  │ v1/moby/content    │  ← 다른 NS의 참조
  │   └── blob        │
  │       └── sha256:a │  → 같은 blob, 다른 메타데이터
  │                    │
  └────────┬───────────┘
           │
  ┌────────▼───────────┐
  │ Backend Store      │
  │ (파일시스템)        │
  │                    │
  │ /blobs/sha256/aa.. │  ← 실제 데이터는 하나만 저장
  │ /blobs/sha256/bb.. │
  └────────────────────┘

  공유 모드에서 blob 데이터는 하나만 저장되지만,
  각 NS의 메타데이터(레이블 등)는 독립적이다.
```

---

## 8. 파일시스템에서의 네임스페이스

### 8.1 영구 데이터 (root)

```
/var/lib/containerd/
├── io.containerd.content.v1.content/
│   └── blobs/               ← 콘텐츠는 네임스페이스 공유 (파일 레벨)
│       └── sha256/
│
├── io.containerd.metadata.v1.bolt/
│   └── meta.db              ← 메타데이터에서 NS별 격리
│
├── io.containerd.runtime.v2.task/
│   ├── k8s.io/              ← 네임스페이스별 디렉토리
│   │   ├── pod-abc/
│   │   └── pod-def/
│   ├── moby/
│   │   └── container-xyz/
│   └── default/
│       └── test/
│
└── io.containerd.snapshotter.v1.overlayfs/
    └── snapshots/            ← 스냅샷은 이름으로 격리
```

### 8.2 임시 데이터 (state)

```
/run/containerd/
├── containerd.sock
├── io.containerd.runtime.v2.task/
│   ├── k8s.io/              ← 네임스페이스별
│   │   └── pod-abc/
│   │       ├── config.json
│   │       ├── init.pid
│   │       └── rootfs/
│   └── default/
│       └── test/
│           └── ...
```

---

## 9. 주요 네임스페이스 사용 사례

### 9.1 Kubernetes (k8s.io)

```go
// Kubernetes CRI 플러그인은 "k8s.io" 네임스페이스 사용
ctx := namespaces.WithNamespace(context.Background(), "k8s.io")

// kubelet이 관리하는 모든 Pod와 컨테이너가 이 NS에 속함
```

### 9.2 Docker Engine (moby)

```go
// Docker는 "moby" 네임스페이스 사용
ctx := namespaces.WithNamespace(context.Background(), "moby")

// docker run, docker build 등의 결과물이 이 NS에 속함
```

### 9.3 기본 (default)

```go
// ctr, nerdctl 등은 기본적으로 "default" 네임스페이스 사용
ctx := namespaces.WithNamespace(context.Background(), "default")

// CONTAINERD_NAMESPACE 환경변수로 변경 가능
```

### 9.4 커스텀 네임스페이스

```go
// 멀티테넌트 시나리오에서 테넌트별 네임스페이스
tenants := []string{"tenant-a", "tenant-b", "tenant-c"}

for _, tenant := range tenants {
    ctx := namespaces.WithNamespace(context.Background(), tenant)
    // 각 테넌트는 독립적인 이미지/컨테이너 공간
}
```

---

## 10. CLI에서의 네임스페이스 관리

### 10.1 ctr 명령어

```bash
# 네임스페이스 목록
ctr namespaces list

# 네임스페이스 생성
ctr namespaces create myapp

# 네임스페이스 레이블 설정
ctr namespaces label myapp containerd.io/defaults/snapshotter=overlayfs

# 특정 네임스페이스에서 작업
ctr -n k8s.io images list
ctr -n k8s.io containers list
ctr -n k8s.io tasks list

# 네임스페이스 삭제 (비어있어야 함)
ctr namespaces remove myapp

# 환경변수로 기본 네임스페이스 설정
export CONTAINERD_NAMESPACE=k8s.io
ctr images list  # k8s.io NS의 이미지 출력
```

### 10.2 crictl과 네임스페이스

```bash
# crictl은 자동으로 k8s.io 네임스페이스 사용
crictl images
crictl pods

# ctr로 같은 리소스 확인
ctr -n k8s.io images list
```

---

## 11. 네임스페이스 격리의 범위

### 11.1 격리되는 리소스

| 리소스 | 격리 여부 | 설명 |
|--------|----------|------|
| Images | 완전 격리 | NS별 독립적 이미지 목록 |
| Containers | 완전 격리 | NS별 독립적 컨테이너 |
| Tasks | 완전 격리 | NS별 독립적 태스크 |
| Snapshots | 메타데이터 격리 | 참조는 격리, 데이터는 공유 가능 |
| Content | 정책에 따라 | shared/isolated 모드 |
| Leases | 완전 격리 | NS별 독립적 리스 |
| Sandboxes | 완전 격리 | NS별 독립적 샌드박스 |
| Events | 완전 격리 | NS별 필터링된 이벤트 |

### 11.2 격리되지 않는 것

```
네임스페이스 경계를 넘는 공유 리소스:

  1. Content Store 백엔드 (shared 모드)
     → 파일시스템의 실제 blob 데이터

  2. 스냅샷터 백엔드
     → overlayfs/btrfs의 실제 파일시스템 데이터

  3. 런타임 바이너리
     → runc, containerd-shim-runc-v2

  4. CNI 설정
     → /etc/cni/net.d/

  5. 플러그인 설정
     → config.toml의 전역 설정

  6. 시스템 리소스
     → CPU, 메모리, 디스크 (cgroup/quota로 별도 관리 필요)
```

---

## 12. GC와 네임스페이스

### 12.1 네임스페이스별 GC

GC는 각 네임스페이스 내에서 독립적으로 참조 그래프를 추적한다:

```
GC 참조 그래프 (네임스페이스별):

  [NS: k8s.io]
    Image "redis:7" ──→ Manifest ──→ Config + Layers
         (root)          (참조)        (참조)

  [NS: moby]
    Image "redis:7" ──→ Manifest ──→ Config + Layers
         (root)          (참조)        (참조)

  두 NS에서 같은 blob을 참조하더라도:
  - k8s.io에서 이미지 삭제 → k8s.io의 참조만 제거
  - moby에서 여전히 참조 중이면 blob은 삭제되지 않음
  - 모든 NS에서 참조 없어야 실제 blob 삭제
```

### 12.2 네임스페이스 삭제 시 GC

```
네임스페이스 삭제 제약:

  Delete("myapp") 호출
    │
    ├── listNs("myapp") 확인
    │   ├── images 버킷 비어있는가?
    │   ├── blobs 버킷 비어있는가?
    │   ├── containers 버킷 비어있는가?
    │   └── snapshots 버킷 비어있는가?
    │
    ├── 하나라도 비어있지 않으면:
    │   → "namespace must be empty" 에러
    │
    └── 모두 비어있으면:
        → 네임스페이스 버킷 삭제

  따라서 삭제 전에 모든 리소스를 수동으로 정리해야 함:
    1. ctr -n myapp tasks delete ...
    2. ctr -n myapp containers delete ...
    3. ctr -n myapp images remove ...
    4. ctr -n myapp leases delete ...
    5. (GC 실행으로 orphan 정리)
    6. ctr namespaces remove myapp
```

---

## 13. 네임스페이스 이름 규칙

### 13.1 유효성 검증

```go
// pkg/identifiers 패키지에서 검증

// 규칙:
// - 비어있지 않아야 함
// - "version"이라는 이름 불가 (BoltDB 키와 충돌)
// - 식별자 형식 준수 (alphanumeric, -, _, .)
```

### 13.2 예약된 이름

| 이름 | 용도 | 사용자 |
|------|------|--------|
| `default` | 기본 네임스페이스 | ctr, nerdctl |
| `k8s.io` | Kubernetes CRI | kubelet |
| `moby` | Docker Engine | dockerd |
| `buildkit` | BuildKit | buildkitd |
| `version` | 예약 (사용 불가) | BoltDB 내부 |

---

## 14. Go 클라이언트에서의 네임스페이스 사용

### 14.1 기본 사용 패턴

```go
package main

import (
    "context"
    containerd "github.com/containerd/containerd/v2/client"
    "github.com/containerd/containerd/v2/pkg/namespaces"
)

func main() {
    client, _ := containerd.New("/run/containerd/containerd.sock")
    defer client.Close()

    // 네임스페이스 설정
    ctx := namespaces.WithNamespace(context.Background(), "example")

    // 이 ctx로 수행하는 모든 작업은 "example" NS에 속함
    image, _ := client.Pull(ctx, "docker.io/library/redis:alpine",
        containerd.WithPullUnpack)

    // 다른 네임스페이스의 이미지에는 접근 불가
    // namespaces.WithNamespace(ctx, "other") 로 전환 필요
}
```

### 14.2 다중 네임스페이스 관리

```go
// 여러 네임스페이스를 동시에 관리
var (
    docker = namespaces.WithNamespace(ctx, "docker")
    vmware = namespaces.WithNamespace(ctx, "vmware")
    ecs    = namespaces.WithNamespace(ctx, "aws-ecs")
    cri    = namespaces.WithNamespace(ctx, "cri")
)

// 각 context는 독립적인 리소스 공간
dockerImages, _ := client.ListImages(docker)
vmwareImages, _ := client.ListImages(vmware)
```

---

## 15. 네임스페이스 격리의 한계와 보완

### 15.1 한계

```
네임스페이스가 제공하지 않는 것:

  1. 인증/인가 (Authentication/Authorization)
     - 어떤 클라이언트든 네임스페이스 전환 가능
     - 소켓 접근 권한이 있으면 모든 NS에 접근 가능

  2. 리소스 할당량 (Resource Quota)
     - NS별 이미지 수, 디스크 사용량 제한 없음
     - cgroup으로 컨테이너 단위 제한은 가능

  3. 네트워크 격리
     - NS는 메타데이터 격리만 제공
     - 네트워크 격리는 CNI/네트워크 정책으로 별도 처리

  4. 보안 경계
     - 악의적 클라이언트 차단 불가
     - 공유 모드에서 다른 NS의 blob 접근 가능
```

### 15.2 보완 방법

| 한계 | 보완 방법 |
|------|----------|
| 인증 부재 | 소켓 파일 권한 제한, mTLS 설정 |
| 리소스 무제한 | Kubernetes ResourceQuota, GC 설정 |
| 보안 격리 부족 | 별도 containerd 인스턴스 운영 |
| 콘텐츠 공유 위험 | `content_sharing_policy = "isolated"` |

---

## 16. 설계 원리 요약

| 설계 결정 | 이유 |
|-----------|------|
| Context 기반 전파 | 모든 함수에 명시적 NS 파라미터 불필요 |
| gRPC + ttrpc 헤더 | 원격 호출 시 자동 전파 |
| BoltDB 버킷 중첩 | 네임스페이스 단위 원자적 트랜잭션 |
| 관리적 격리 (비보안) | 성능 우선, 보안은 다른 레이어에서 처리 |
| Shared 콘텐츠 기본 | 대부분의 환경에서 디스크 효율 우선 |
| 빈 NS만 삭제 가능 | 실수로 인한 데이터 손실 방지 |
| 환경변수 지원 | CLI 사용 편의성 |
| 레이블 기반 기본값 | NS별 커스텀 설정 지원 |

---

## 17. 네임스페이스 격리 다이어그램 요약

```
┌─────────────────────────────────────────────────────────────┐
│                    containerd 네임스페이스 격리               │
│                                                             │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────────────┐   │
│  │ NS: k8s.io  │ │ NS: moby    │ │ NS: default         │   │
│  │             │ │             │ │                     │   │
│  │ Images:     │ │ Images:     │ │ Images:             │   │
│  │  redis:7    │ │  nginx:1    │ │  ubuntu:22.04       │   │
│  │  pause:3.9  │ │  redis:7    │ │                     │   │
│  │             │ │             │ │                     │   │
│  │ Containers: │ │ Containers: │ │ Containers:         │   │
│  │  pod-abc    │ │  web-1      │ │  test-1             │   │
│  │  pod-def    │ │  cache-1    │ │                     │   │
│  │             │ │             │ │                     │   │
│  │ Leases:     │ │ Leases:     │ │ Leases:             │   │
│  │  pull-001   │ │  build-001  │ │  (none)             │   │
│  └──────┬──────┘ └──────┬──────┘ └──────┬──────────────┘   │
│         │               │               │                   │
│         └───────────────┼───────────────┘                   │
│                         │                                   │
│              ┌──────────▼──────────┐                        │
│              │ Shared Content Store │ ← blob 데이터 공유     │
│              │ (shared 모드)        │                        │
│              └─────────────────────┘                        │
└─────────────────────────────────────────────────────────────┘
```

---

## 참고 자료

- 소스코드: `pkg/namespaces/context.go` - 네임스페이스 Context 전파
- 소스코드: `core/metadata/namespaces.go` - BoltDB 기반 Namespace Store
- 소스코드: `core/metadata/buckets.go` - BoltDB 스키마 (네임스페이스별 버킷)
- 소스코드: `core/metadata/db.go` - DB 옵션 (shared/isolated 정책)
- 소스코드: `core/metadata/leases.go` - 네임스페이스별 리스 관리
- 소스코드: `core/metadata/sandbox.go` - 네임스페이스별 샌드박스 관리
- 소스코드: `docs/namespaces.md` - 네임스페이스 공식 문서
