# 14. 메타데이터 저장소 (Metadata Store)

## 목차

1. [개요](#1-개요)
2. [BoltDB 기반 메타데이터 아키텍처](#2-boltdb-기반-메타데이터-아키텍처)
3. [DB 구조체 분석](#3-db-구조체-분석)
4. [BoltDB 버킷 구조](#4-boltdb-버킷-구조)
5. [네임스페이스 격리](#5-네임스페이스-격리)
6. [트랜잭션 모델](#6-트랜잭션-모델)
7. [ContentSharingPolicy](#7-contentsharingpolicy)
8. [스키마 마이그레이션](#8-스키마-마이그레이션)
9. [containerStore 구현](#9-containerstore-구현)
10. [sandboxStore 구현](#10-sandboxstore-구현)
11. [wlock과 GC 동시성](#11-wlock과-gc-동시성)
12. [mutationCallbacks](#12-mutationcallbacks)
13. [플러그인 초기화](#13-플러그인-초기화)
14. [왜 이렇게 설계했는가](#14-왜-이렇게-설계했는가)

---

## 1. 개요

containerd의 메타데이터 저장소는 모든 오브젝트(이미지, 컨테이너, 스냅샷, 리스, 샌드박스 등)의 메타데이터를 관리하는 중앙 저장소다. 임베디드 키-값 데이터베이스인 BoltDB(bbolt)를 사용하며, 네임스페이스별 격리, 참조 기반 GC, 콘텐츠 공유 정책 등의 기능을 제공한다.

**핵심 소스 파일:**

| 파일 | 역할 |
|------|------|
| `core/metadata/db.go` | DB 구조체, Init(), GarbageCollect(), View/Update |
| `plugins/metadata/plugin.go` | BoltDB 플러그인 등록, BoltConfig |
| `core/metadata/containers.go` | containerStore (CRUD) |
| `core/metadata/sandbox.go` | sandboxStore (CRUD) |
| `core/metadata/buckets.go` | 버킷 키 정의, 경로 헬퍼 |
| `core/metadata/gc.go` | GC 리소스 타입, 참조 레이블, scanRoots/references |

---

## 2. BoltDB 기반 메타데이터 아키텍처

```
+-------------------------------------------------------------------+
|                     containerd Metadata Layer                      |
|                                                                    |
|  +--------------------+  +--------------------+  +---------------+ |
|  | containerStore     |  | imageStore         |  | sandboxStore  | |
|  | (containers.go)    |  | (images.go)        |  | (sandbox.go)  | |
|  +--------------------+  +--------------------+  +---------------+ |
|  +--------------------+  +--------------------+  +---------------+ |
|  | leaseStore         |  | snapshotStore       |  | contentStore  | |
|  | (leases.go)        |  | (snapshot.go)       |  | (content.go)  | |
|  +--------------------+  +--------------------+  +---------------+ |
|                            |                                       |
|                     +------+------+                                |
|                     |   DB struct |  wlock, dirty, mutationCallbacks|
|                     +------+------+                                |
|                            |                                       |
|                     +------+------+                                |
|                     |   BoltDB    |  meta.db 파일                  |
|                     +-------------+                                |
+-------------------------------------------------------------------+
          |                                    |
          v                                    v
+-------------------+              +-------------------+
| Content Store     |              | Snapshot Store    |
| (backend)         |              | (backend)         |
| 실제 blob 저장     |              | 실제 스냅샷 저장   |
+-------------------+              +-------------------+
```

### 설계 원칙

1. **메타데이터와 데이터의 분리**: BoltDB에는 메타데이터만 저장하고, 실제 blob/스냅샷은 별도 백엔드에 저장
2. **네임스페이스 격리**: 모든 오브젝트는 네임스페이스 내에서 관리
3. **프록시 패턴**: contentStore와 snapshotter는 BoltDB 메타데이터를 프록시하여 실제 백엔드에 접근
4. **참조 기반 GC**: 레이블로 오브젝트 간 참조를 추적하여 가비지 컬렉션

---

## 3. DB 구조체 분석

```go
// 소스: core/metadata/db.go (83-115행)
type DB struct {
    db Transactor                           // BoltDB 트랜잭션 인터페이스
    ss map[string]*snapshotter              // 스냅샷터 프록시
    cs *contentStore                        // 콘텐츠 스토어 프록시

    // GC 중 쓰기 차단을 위한 읽기-쓰기 잠금
    wlock sync.RWMutex

    // 참조 삭제 발생 여부 (GC 필요 여부)
    dirty atomic.Uint32

    // GC 이후 삭제가 발생한 데이터스토어 추적
    dirtySS map[string]struct{}             // dirty 스냅샷터
    dirtyCS bool                            // dirty 콘텐츠 스토어

    // 메타데이터 변경 시 호출되는 콜백
    mutationCallbacks []func(bool)

    // 수집 가능한 리소스 등록
    collectors map[gc.ResourceType]Collector

    dbopts dbOptions
}
```

### 핵심 필드 역할

```
DB
├── db (Transactor)
│   └── BoltDB 인스턴스의 View/Update 메서드
│
├── wlock (sync.RWMutex)
│   ├── RLock: 일반 쓰기 트랜잭션 (Update)
│   └── Lock: GC 실행 중 (GarbageCollect)
│   왜? → GC의 Mark-Sweep 사이에 쓰기가 발생하면 무결성 위반
│
├── dirty (atomic.Uint32)
│   └── 삭제 발생 시 +1, GC 완료 시 0으로 리셋
│   왜? → GC 스케줄러가 GC 필요 여부를 빠르게 판단
│
├── dirtySS / dirtyCS
│   └── GC 중 어떤 백엔드에 삭제가 발생했는지 추적
│   왜? → 해당 백엔드만 선택적으로 정리
│
├── mutationCallbacks
│   └── 모든 Update 트랜잭션 완료 후 호출
│   왜? → GC 스케줄러에 변경 알림 (mutationEvent)
│
└── collectors
    └── 외부 리소스 타입의 GC 참여
    왜? → Stream, Mount 등 메타데이터 외부 리소스도 GC 대상
```

---

## 4. BoltDB 버킷 구조

BoltDB는 계층적 버킷(bucket) 구조를 사용한다. containerd는 아래와 같은 스키마를 사용한다.

```
meta.db (BoltDB)
└── v1/                                    # schemaVersion 버킷
    ├── [DBVersion]                        # 현재 DB 버전 (4)
    │
    ├── {namespace}/                       # 네임스페이스 버킷
    │   ├── labels/                        # 네임스페이스 레이블
    │   │
    │   ├── images/                        # 이미지 오브젝트
    │   │   └── {image-name}/
    │   │       ├── createdat
    │   │       ├── updatedat
    │   │       ├── target/                # 이미지 대상 디스크립터
    │   │       │   ├── digest
    │   │       │   ├── mediatype
    │   │       │   └── size
    │   │       └── labels/
    │   │           ├── containerd.io/gc.ref.content.config → sha256:...
    │   │           └── containerd.io/gc.ref.snapshot.overlayfs/0 → ...
    │   │
    │   ├── containers/                    # 컨테이너 오브젝트
    │   │   └── {container-id}/
    │   │       ├── createdat
    │   │       ├── updatedat
    │   │       ├── image
    │   │       ├── snapshotter
    │   │       ├── snapshotkey
    │   │       ├── sandboxid
    │   │       ├── runtime/
    │   │       │   ├── name
    │   │       │   └── options
    │   │       ├── spec                   # OCI 스펙 (protobuf)
    │   │       ├── labels/
    │   │       └── extensions/
    │   │
    │   ├── snapshots/                     # 스냅샷 참조
    │   │   └── {snapshotter}/             # 예: overlayfs
    │   │       └── {snapshot-key}/
    │   │           ├── parent
    │   │           └── labels/
    │   │
    │   ├── content/                       # 콘텐츠 참조
    │   │   ├── blob/                      # 커밋된 blob
    │   │   │   └── {digest}/
    │   │   │       ├── createdat
    │   │   │       ├── updatedat
    │   │   │       ├── size
    │   │   │       └── labels/
    │   │   │           └── containerd.io/gc.ref.content.l.0 → sha256:...
    │   │   └── ingests/                   # 진행 중인 쓰기
    │   │       └── {ref}/
    │   │           ├── ref
    │   │           ├── expireat
    │   │           └── expected
    │   │
    │   ├── leases/                        # 리스 오브젝트
    │   │   └── {lease-id}/
    │   │       ├── labels/
    │   │       │   └── containerd.io/gc.expire → RFC3339 시간
    │   │       ├── content/               # 리스된 콘텐츠
    │   │       ├── snapshots/             # 리스된 스냅샷
    │   │       ├── ingests/               # 리스된 인제스트
    │   │       └── images/                # 리스된 이미지
    │   │
    │   └── sandboxes/                     # 샌드박스 오브젝트
    │       └── {sandbox-id}/
    │           ├── createdat
    │           ├── updatedat
    │           ├── sandboxer
    │           ├── runtime/
    │           ├── spec
    │           ├── labels/
    │           └── extensions/
    │
    └── {another-namespace}/               # 다른 네임스페이스
        └── ...
```

### 버킷 키 정의

```go
// 소스: core/metadata/buckets.go (149-157행)
bucketKeyObjectLabels     = []byte("labels")
bucketKeyObjectImages     = []byte("images")
bucketKeyObjectContainers = []byte("containers")
bucketKeyObjectSnapshots  = []byte("snapshots")
bucketKeyObjectContent    = []byte("content")
bucketKeyObjectBlob       = []byte("blob")
bucketKeyObjectIngests    = []byte("ingests")
bucketKeyObjectLeases     = []byte("leases")
bucketKeyObjectSandboxes  = []byte("sandboxes")
```

---

## 5. 네임스페이스 격리

containerd의 모든 오브젝트 접근은 네임스페이스가 필수다. 네임스페이스는 Go context에서 추출된다.

```go
// 소스: core/metadata/containers.go (56행)
func (s *containerStore) Get(ctx context.Context, id string) (containers.Container, error) {
    namespace, err := namespaces.NamespaceRequired(ctx)
    if err != nil {
        return containers.Container{}, err
    }
    // namespace를 사용하여 버킷 경로 결정
    bkt := getContainerBucket(tx, namespace, id)
    // → v1/{namespace}/containers/{id}
}
```

### 네임스페이스 격리의 영향

```
네임스페이스 "k8s.io"                네임스페이스 "default"
├── images/                        ├── images/
│   └── docker.io/nginx:latest     │   └── docker.io/nginx:latest  ← 별도 레코드
├── containers/                    ├── containers/
│   └── abc123                     │   └── xyz789
└── content/                       └── content/
    └── sha256:aaa...                  └── sha256:aaa...  ← shared 정책이면 같은 blob
```

네임스페이스 간 이미지/컨테이너/리스 메타데이터는 완전히 격리된다. 하지만 실제 콘텐츠(blob)와 스냅샷은 `ContentSharingPolicy`에 따라 공유될 수 있다.

---

## 6. 트랜잭션 모델

### View (읽기 트랜잭션)

```go
// 소스: core/metadata/db.go (255-257행)
func (m *DB) View(fn func(*bolt.Tx) error) error {
    return m.db.View(fn)
}
```

View는 BoltDB의 읽기 전용 트랜잭션이다:
- wlock을 잡지 않는다 (GC 중에도 읽기 가능)
- 여러 View가 동시 실행 가능
- 트랜잭션 시작 시점의 스냅샷을 읽는다 (MVCC)

### Update (쓰기 트랜잭션)

```go
// 소스: core/metadata/db.go (260-272행)
func (m *DB) Update(fn func(*bolt.Tx) error) error {
    m.wlock.RLock()                     // GC 중이면 대기
    defer m.wlock.RUnlock()
    err := m.db.Update(fn)
    if err == nil {
        dirty := m.dirty.Load() > 0
        for _, fn := range m.mutationCallbacks {
            fn(dirty)                   // GC 스케줄러에 알림
        }
    }
    return err
}
```

Update의 동시성 보장:
1. **wlock.RLock**: GC가 실행 중(wlock.Lock)이면 대기
2. **db.Update**: BoltDB 내부에서 쓰기 직렬화 (한 번에 하나의 쓰기 트랜잭션)
3. **mutationCallbacks**: 성공 시 GC 스케줄러에 변경 사실 알림

```
시간 →
[Thread A] Update ──RLock──[BoltDB Write]──RUnlock──callbacks
[Thread B] Update ──RLock──────[대기]──────[BoltDB Write]──RUnlock
[Thread C] GC     ──Lock 대기──────────────────────[Lock]──Mark──Sweep──Unlock
```

### 왜 wlock이 필요한가

BoltDB는 트랜잭션별 격리를 제공하지만, GC의 Mark-Sweep은 두 개의 트랜잭션에 걸쳐 실행된다:
1. View 트랜잭션에서 Mark (reachable 오브젝트 식별)
2. Update 트랜잭션에서 Sweep (unreachable 오브젝트 삭제)

이 두 단계 사이에 새 참조가 추가되면 아직 Mark되지 않은 오브젝트를 잘못 삭제할 수 있다. wlock은 이를 방지한다.

---

## 7. ContentSharingPolicy

```go
// 소스: plugins/metadata/plugin.go (49-75행)
type BoltConfig struct {
    ContentSharingPolicy string `toml:"content_sharing_policy"`
    NoSync               bool   `toml:"no_sync"`
}
```

### Shared 정책 (기본값)

```
네임스페이스 A가 이미지를 풀한 후:
  A/content/blob/sha256:abc → 존재

네임스페이스 B가 같은 다이제스트로 쓰기 시도:
  B가 Expected 다이제스트를 제공하면:
    → 백엔드에 이미 존재하는 blob을 B의 메타데이터에도 등록
    → 재다운로드 불필요 (대역폭 절약)

위험: B가 다이제스트를 알기만 하면 A의 콘텐츠에 접근 가능
```

### Isolated 정책

```
네임스페이스 A가 이미지를 풀한 후:
  A/content/blob/sha256:abc → 존재

네임스페이스 B가 같은 다이제스트로 쓰기 시도:
  → B가 전체 콘텐츠를 처음부터 인제스트해야 함
  → 모든 바이트를 실제로 제공해야만 등록 가능

안전: 다이제스트만으로는 접근 불가 (콘텐츠 소유 증명 필요)
```

### 정책 선택 기준

| 기준 | Shared | Isolated |
|------|--------|----------|
| 대역폭 | 절약 (중복 다운로드 없음) | 각 네임스페이스가 독립 다운로드 |
| 보안 | 다이제스트 알면 접근 가능 | 콘텐츠 소유 증명 필요 |
| 멀티테넌트 | 부적합 | 적합 |
| 단일 용도 (Kubernetes) | 적합 | 불필요한 오버헤드 |

---

## 8. 스키마 마이그레이션

```go
// 소스: core/metadata/db.go (41-53행)
const (
    schemaVersion = "v1"     // 스키마 버전 (구조적 변경)
    dbVersion     = 4        // DB 버전 (호환 가능한 변경)
)
```

### Init() 마이그레이션 흐름

```go
// 소스: core/metadata/db.go (144-224행)
func (m *DB) Init(ctx context.Context) error {
    err := m.db.Update(func(tx *bolt.Tx) error {
        // 1. 현재 스키마/버전 확인
        schema = "v0"
        version = 0

        // 2. migrations 배열에서 적용할 마이그레이션 탐색
        i := len(migrations)
        for ; i > 0; i-- {
            migration := migrations[i-1]
            bkt := tx.Bucket([]byte(migration.schema))
            // 스키마 버전 확인
            if schema == "v0" {
                schema = migration.schema
                v, _ := binary.Varint(vb)
                version = int(v)
            }
            if version >= migration.version {
                break
            }
        }

        // 3. 필요한 마이그레이션 순차 실행
        updates := migrations[i:]
        for _, m := range updates {
            if err := m.migrate(tx); err != nil {
                return err
            }
        }

        // 4. 현재 버전 기록
        bkt.Put(bucketKeyDBVersion, versionEncoded)
    })
}
```

### 마이그레이션 전략

```
DB 파일 없음 → 새 DB 생성 (v1, version 4)
DB version 1 → migration 2, 3, 4 순서 적용
DB version 4 → 마이그레이션 불필요 (errSkip → 즉시 롤백)
```

`errSkip`은 중요한 최적화다. 마이그레이션이 불필요한 경우 Update 트랜잭션을 커밋하지 않고 롤백하여 불필요한 디스크 쓰기를 방지한다.

---

## 9. containerStore 구현

```go
// 소스: core/metadata/containers.go (44-53행)
type containerStore struct {
    db *DB
}

func NewContainerStore(db *DB) containers.Store {
    return &containerStore{db: db}
}
```

### CRUD 구현

#### Get

```go
// 소스: core/metadata/containers.go (55-79행)
func (s *containerStore) Get(ctx context.Context, id string) (containers.Container, error) {
    namespace, err := namespaces.NamespaceRequired(ctx)
    // 경로: v1/{namespace}/containers/{id}
    bkt := getContainerBucket(tx, namespace, id)
    readContainer(&container, bkt)
}
```

#### Create

```go
// 소스: core/metadata/containers.go (123-172행)
func (s *containerStore) Create(ctx context.Context, container containers.Container) (containers.Container, error) {
    // 1. 네임스페이스 추출
    // 2. validateContainer() — ID, Runtime.Name, Spec 필수
    // 3. createContainersBucket() — 버킷 생성
    // 4. bkt.CreateBucket([]byte(container.ID)) — ID 버킷
    // 5. container.CreatedAt/UpdatedAt 설정
    // 6. writeContainer(cbkt, &container) — 필드 기록
}
```

#### Delete

```go
// 소스: core/metadata/containers.go (287-321행)
func (s *containerStore) Delete(ctx context.Context, id string) error {
    // 1. 네임스페이스의 containers 버킷 접근
    // 2. bkt.DeleteBucket([]byte(id)) — 컨테이너 버킷 삭제
    // 3. s.db.dirty.Add(1) — GC에 삭제 알림 ★
}
```

`dirty.Add(1)` 호출이 핵심이다. 이 호출이 GC 스케줄러에게 "삭제가 발생했으니 GC를 고려하라"고 알린다.

### readContainer 직렬화

```go
// 소스: core/metadata/containers.go (356-410행)
func readContainer(container *containers.Container, bkt *bolt.Bucket) error {
    labels, err := boltutil.ReadLabels(bkt)
    container.Labels = labels
    boltutil.ReadTimestamps(bkt, &container.CreatedAt, &container.UpdatedAt)

    return bkt.ForEach(func(k, v []byte) error {
        switch string(k) {
        case "image":        container.Image = string(v)
        case "runtime":      // 서브버킷에서 Name, Options 읽기
        case "spec":         proto.Unmarshal(v, &spec)
        case "snapshotkey":  container.SnapshotKey = string(v)
        case "snapshotter":  container.Snapshotter = string(v)
        case "extensions":   boltutil.ReadExtensions(bkt)
        case "sandboxid":    container.SandboxID = string(v)
        }
    })
}
```

### Container 유효성 검증

```go
// 소스: core/metadata/containers.go (323-354행)
func validateContainer(container *containers.Container) error {
    // ID: 식별자 유효성 (identifiers.Validate)
    // Extensions: 키가 빈 문자열이면 안 됨
    // Labels: 레이블 유효성
    // Runtime.Name: 필수
    // Spec: 필수
    // SnapshotKey 있으면 Snapshotter도 필수
}
```

---

## 10. sandboxStore 구현

```go
// 소스: core/metadata/sandbox.go (42-51행)
type sandboxStore struct {
    db *DB
}

func NewSandboxStore(db *DB) api.Store {
    return &sandboxStore{db: db}
}
```

sandboxStore는 containerStore와 유사한 패턴을 따르되, 추가 필드들을 관리한다:

```
Sandbox 메타데이터
├── ID           → 고유 식별자
├── Labels       → 키-값 메타데이터
├── Runtime
│   ├── Name     → 런타임 이름 (예: io.containerd.runc.v2)
│   └── Options  → 런타임 옵션 (typeurl.Any)
├── Spec         → 런타임 스펙 (typeurl.Any)
├── Sandboxer    → 샌드박스 컨트롤러 이름
├── CreatedAt    → 생성 시각
├── UpdatedAt    → 수정 시각
└── Extensions   → 확장 메타데이터 (map[string]typeurl.Any)
```

### Update의 fieldpaths

```go
// 소스: core/metadata/sandbox.go (96행)
func (s *sandboxStore) Update(ctx context.Context, sandbox api.Sandbox,
    fieldpaths ...string) (api.Sandbox, error)
```

fieldpaths를 사용하면 특정 필드만 업데이트할 수 있다:
- `"labels"` → 전체 레이블 교체
- `"labels.foo"` → 특정 레이블만 업데이트
- `"extensions"` → 전체 확장 교체
- `"extensions.bar"` → 특정 확장만 업데이트
- `"spec"` → 스펙 교체
- `"runtime"` → 런타임 옵션 교체

fieldpaths가 비어있으면 모든 가변 필드를 업데이트한다.

---

## 11. wlock과 GC 동시성

### 동시성 모델

```
                           일반 쓰기                GC
                           (Update)              (GarbageCollect)
                              |                      |
wlock                   RLock (공유)             Lock (배타적)
                              |                      |
                        다른 쓰기와 병렬          모든 쓰기 차단
                              |                      |
BoltDB                  직렬화된 쓰기            직렬화된 읽기/쓰기
```

### GarbageCollect에서의 wlock 사용

```go
// 소스: core/metadata/db.go (371-479행)
func (m *DB) GarbageCollect(ctx context.Context) (gc.Stats, error) {
    m.wlock.Lock()                          // ★ 배타적 잠금

    // Phase 1: Mark (View 트랜잭션)
    marked, err := m.getMarked(ctx, c)      // 도달 가능한 노드 식별

    // Phase 2: Sweep (Update 트랜잭션)
    m.db.Update(func(tx *bolt.Tx) error {
        // marked에 없는 노드 삭제
        c.scanAll(ctx, tx, rm)
    })

    // Phase 3: Backend Cleanup (병렬)
    // dirty 스냅샷터/콘텐츠 스토어 정리
    m.dirty.Store(0)                        // dirty 리셋
    m.dirtySS = map[string]struct{}{}

    stats.MetaD = time.Since(t1)
    m.wlock.Unlock()                        // ★ 잠금 해제

    wg.Wait()                               // 백엔드 정리 완료 대기
    return stats, err
}
```

### 타임라인

```
시간 →

[일반 쓰기]  ──RLock──Write──RUnlock──RLock──Write──RUnlock──
                                                              |← 여기서 대기
[GC]                                          Lock────Mark────Sweep────
                                                                      |
[일반 쓰기]                                                   ────────RLock──Write──
                                              ↑                ↑
                                          wlock.Lock()    wlock.Unlock()
```

---

## 12. mutationCallbacks

### 등록

```go
// 소스: core/metadata/db.go (293-297행)
func (m *DB) RegisterMutationCallback(fn func(bool)) {
    m.wlock.Lock()
    m.mutationCallbacks = append(m.mutationCallbacks, fn)
    m.wlock.Unlock()
}
```

### 호출 시점

```go
// 소스: core/metadata/db.go (260-272행)
func (m *DB) Update(fn func(*bolt.Tx) error) error {
    m.wlock.RLock()
    defer m.wlock.RUnlock()
    err := m.db.Update(fn)
    if err == nil {
        dirty := m.dirty.Load() > 0
        for _, fn := range m.mutationCallbacks {
            fn(dirty)    // ← 콜백 호출
        }
    }
    return err
}
```

### GC 스케줄러와의 연결

```go
// 소스: plugins/gc/scheduler.go (220-229행)
func (s *gcScheduler) mutationCallback(dirty bool) {
    e := mutationEvent{
        ts:       time.Now(),
        mutation: true,
        dirty:    dirty,
    }
    go func() {
        s.eventC <- e    // 스케줄러에 비동기 알림
    }()
}
```

```
흐름:
containerStore.Delete()
  → db.dirty.Add(1)
  → db.Update() 성공
    → mutationCallbacks 호출
      → gcScheduler.mutationCallback(dirty=true)
        → eventC <- mutationEvent{dirty: true}
          → GC 스케줄링 로직 트리거
```

---

## 13. 플러그인 초기화

```go
// 소스: plugins/metadata/plugin.go (94-200행)
func init() {
    registry.Register(&plugin.Registration{
        Type: plugins.MetadataPlugin,
        ID:   "bolt",
        Requires: []plugin.Type{
            plugins.ContentPlugin,
            plugins.EventPlugin,
            plugins.SnapshotPlugin,
        },
        Config: &BoltConfig{
            ContentSharingPolicy: SharingPolicyShared,
            NoSync:               false,
        },
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            // 1. 루트 디렉토리 생성
            root := ic.Properties[plugins.PropertyRootDir]
            os.MkdirAll(root, 0711)

            // 2. 의존 플러그인 로드
            cs := ic.GetSingle(plugins.ContentPlugin)  // Content Store
            snapshottersRaw := ic.GetByType(plugins.SnapshotPlugin)
            ep := ic.GetSingle(plugins.EventPlugin)    // Event Publisher

            // 3. BoltDB 옵션 설정
            options := *bolt.DefaultOptions
            options.NoFreelistSync = true               // 안정성 향상
            options.Timeout = timeout.Get(boltOpenTimeout)

            // 4. Content Sharing Policy 적용
            shared := true
            if cfg.ContentSharingPolicy == SharingPolicyIsolated {
                shared = false
            }

            // 5. BoltDB 열기 (10초 경고 타이머)
            path := filepath.Join(root, "meta.db")
            db, err := bolt.Open(path, 0644, &options)

            // 6. metadata.DB 생성 및 초기화
            dbopts := []metadata.DBOpt{
                metadata.WithEventsPublisher(ep.(events.Publisher)),
            }
            if !shared {
                dbopts = append(dbopts, metadata.WithPolicyIsolated)
            }
            mdb := metadata.NewDB(db, cs, snapshotters, dbopts...)
            mdb.Init(ic.Context)

            return mdb, nil
        },
    })
}
```

### BoltDB 열기 타임아웃

```go
// 소스: plugins/metadata/plugin.go (167-177행)
doneCh := make(chan struct{})
go func() {
    t := time.NewTimer(10 * time.Second)
    defer t.Stop()
    select {
    case <-t.C:
        log.G(ic.Context).WithField("plugin", "bolt").
            Warn("waiting for response from boltdb open")
    case <-doneCh:
        return
    }
}()
db, err := bolt.Open(path, 0644, &options)
close(doneCh)
```

BoltDB는 파일 잠금(flock)을 사용하므로, 이미 다른 프로세스가 DB를 열고 있으면 대기한다. 10초 후에도 열리지 않으면 경고 로그를 남긴다.

### NoFreelistSync 옵션

```go
options.NoFreelistSync = true
```

BoltDB의 freelist 동기화를 비활성화한다. 이는 데이터 손상 시 freelist 읽기 실패를 방지하기 위한 안정성 조치다. freelist는 매번 다시 스캔하므로 약간의 성능 비용이 있지만, 손상된 DB에서의 복구 가능성이 높아진다.

---

## 14. 왜 이렇게 설계했는가

### Q1: 왜 BoltDB를 선택했는가?

1. **임베디드**: 별도의 데이터베이스 서버 불필요 (운영 복잡성 감소)
2. **ACID 트랜잭션**: 메타데이터 무결성 보장
3. **읽기 성능**: mmap 기반으로 읽기가 빠름 (메타데이터는 읽기 위주)
4. **단일 파일**: 백업/복원이 간단 (meta.db 파일 하나)
5. **Go 네이티브**: CGo 없이 순수 Go로 동작

```
BoltDB 특성:
  쓰기: 트랜잭션당 1회 fsync → 느림 (NoSync로 완화 가능)
  읽기: mmap → 빠름 (OS 페이지 캐시 활용)
  동시성: 1 Writer + N Readers
```

### Q2: 왜 메타데이터와 실제 데이터를 분리했는가?

```
메타데이터 (BoltDB):        실제 데이터 (파일시스템):
├── 이미지 참조              ├── blob 파일들 (content store)
├── 컨테이너 설정            └── 스냅샷 디렉토리 (snapshotter)
├── 리스 정보
└── GC 참조 레이블
```

분리 이유:
1. **크기 차이**: 메타데이터는 KB 수준, blob은 GB 수준. 같은 저장소에 넣으면 DB가 비대해짐
2. **접근 패턴**: 메타데이터는 트랜잭션 필요, blob은 순차적 읽기/쓰기
3. **GC 전략**: 메타데이터에서 참조만 삭제하면 백엔드 정리는 나중에 처리 가능
4. **플러그인 확장**: 다양한 스냅샷터(overlayfs, zfs, stargz)를 독립적으로 교체 가능

### Q3: 왜 dirty 카운터가 atomic인가?

```go
dirty atomic.Uint32
```

dirty는 wlock 밖에서도 읽힐 수 있다:
- **쓰기(Add)**: Update 트랜잭션 내부(wlock.RLock 보유 중)
- **읽기(Load)**: mutationCallback에서 (wlock.RLock 보유 중)
- **리셋(Store(0))**: GarbageCollect에서 (wlock.Lock 보유 중)

wlock.RLock은 여러 goroutine이 동시에 잡을 수 있으므로, dirty의 Add/Load가 동시 발생 가능하다. atomic을 사용하여 데이터 레이스를 방지한다.

### Q4: 왜 Publisher를 트랜잭션 내에서 사용하지 않는가?

```go
// 소스: core/metadata/db.go (276-286행)
func (m *DB) Publisher(ctx context.Context) events.Publisher {
    _, ok := boltutil.Transaction(ctx)
    if ok {
        // 트랜잭션 내에서는 이벤트 발행하지 않음
        return nil
    }
    return m.dbopts.publisher
}
```

트랜잭션 내에서 이벤트를 발행하면:
1. **데드락 위험**: 이벤트 구독자가 다시 DB에 접근할 수 있음
2. **롤백 불일치**: 트랜잭션이 롤백되어도 이미 발행된 이벤트는 되돌릴 수 없음
3. **트랜잭션 지연**: 이벤트 발행의 네트워크 지연이 트랜잭션을 길어지게 함

대신 GC 후에 삭제된 이미지/스냅샷 이벤트를 비동기로 발행한다:

```go
// 소스: core/metadata/db.go (430-433행)
wg.Add(1)
go func() {
    m.publishEvents(events)  // 커밋 후 비동기 발행
    wg.Done()
}()
```

---

## 요약

containerd의 메타데이터 저장소는 BoltDB를 중심으로 설계된 핵심 인프라 계층이다:

```
핵심 설계 패턴:
  1. 프록시 패턴: BoltDB 메타데이터 ↔ 백엔드 데이터
  2. 네임스페이스 격리: context → namespace → 버킷 경로
  3. 2단계 잠금: wlock(GC/쓰기 조율) + BoltDB(쓰기 직렬화)
  4. 비동기 GC: dirty 카운터 → mutationCallback → 스케줄러
  5. 마이그레이션: 스키마 버전 + DB 버전 이중 추적

소스 경로 요약:
  core/metadata/db.go          # DB 핵심 (Init, View, Update, GC)
  core/metadata/containers.go  # 컨테이너 저장소
  core/metadata/sandbox.go     # 샌드박스 저장소
  core/metadata/buckets.go     # 버킷 키/경로 정의
  core/metadata/gc.go          # GC 리소스/참조/스캔
  plugins/metadata/plugin.go   # 플러그인 등록/초기화
```
