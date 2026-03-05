# containerd 핵심 컴포넌트

## 1. 개요

containerd의 핵심 컴포넌트는 **플러그인 시스템을 통해 연결**된다.
각 컴포넌트는 `core/`에 인터페이스가 정의되고, `plugins/` 또는 `core/metadata/`에 구현이 위치한다.

```
+------------------------------------------------------------------+
|                  containerd 핵심 컴포넌트 관계                     |
+------------------------------------------------------------------+
|                                                                    |
|   Plugin Registry ─ DFS ─> 초기화 순서 결정                        |
|        │                                                           |
|        ├─> Content Store (digest 기반 blob 저장)                   |
|        │        │                                                  |
|        │        └─> Metadata DB (BoltDB 메타데이터)                |
|        │                │                                          |
|        ├─> Snapshotter (CoW 파일시스템 레이어)                     |
|        │        │                                                  |
|        │        └─> Metadata DB                                    |
|        │                                                           |
|        ├─> Runtime / Shim (컨테이너 프로세스 관리)                  |
|        │        │                                                  |
|        │        └─> TTRPC → runc                                   |
|        │                                                           |
|        ├─> Event Exchange (비동기 이벤트 버스)                      |
|        │                                                           |
|        ├─> GC Scheduler (리소스 정리)                               |
|        │        │                                                  |
|        │        └─> Tricolor Mark → Content/Snapshot Sweep         |
|        │                                                           |
|        └─> Lease Manager (GC 보호)                                 |
|                                                                    |
+------------------------------------------------------------------+
```

---

## 2. Plugin Registry

### 2.1 역할

Plugin Registry는 containerd의 **심장**이다.
모든 컴포넌트가 플러그인으로 등록되며, Registry가 의존성을 해석하고 초기화 순서를 결정한다.

### 2.2 핵심 구조

```
소스 참조: plugins/types.go (Line 25~84) - 플러그인 타입 상수
소스 참조: cmd/containerd/server/server.go (Line 156~157, 494~568) - LoadPlugins, Graph
```

**Registration (플러그인 등록 정보)**

```go
// github.com/containerd/plugin 패키지
type Registration struct {
    Type            Type                    // 플러그인 타입
    ID              string                  // 고유 ID
    Requires        []Type                  // 의존하는 플러그인 타입
    InitFn          func(*InitContext) (interface{}, error)  // 초기화 함수
    Config          interface{}             // 설정 구조체 (nil이면 설정 없음)
    ConfigMigration func(ctx, version, plugins) error  // 설정 마이그레이션
}
```

**InitContext (초기화 컨텍스트)**

플러그인 초기화 시 제공되는 정보:

```go
type InitContext struct {
    Context           context.Context
    Root              string    // 플러그인 영구 저장소 경로 (config.Root + pluginURI)
    State             string    // 플러그인 임시 저장소 경로 (config.State + pluginURI)
    Address           string    // gRPC 주소
    TTRPCAddress      string    // TTRPC 주소
    Config            interface{}   // 디코딩된 플러그인 설정
    RegisterReadiness func() func() // 준비 완료 알림 등록
    // Get(): 이미 초기화된 다른 플러그인 인스턴스 조회
}
```

### 2.3 DFS 의존성 해석 알고리즘

```
소스 참조: cmd/containerd/server/server.go (Line 567)
  return registry.Graph(filter(config.DisabledPlugins)), nil
```

```
registry.Graph(filter) 동작 과정:

입력: 등록된 모든 Registration[]
출력: 토폴로지 정렬된 Registration[]

알고리즘:
1. 모든 Registration을 타입별로 그룹화
   map[Type][]Registration

2. 각 Registration의 Requires를 기반으로 인접 리스트 생성
   예: GC → MetadataPlugin → ContentPlugin, SnapshotPlugin

3. DFS 후위 순회 (Post-order traversal)
   방문 안 한 노드부터 시작:
     - 해당 노드의 모든 의존성을 먼저 방문 (재귀)
     - 의존성이 모두 처리되면 자신을 결과에 추가
     - 순환 의존성 감지 시 에러

4. DisabledPlugins 필터 적용
   비활성화된 플러그인 제거

결과 예시 (초기화 순서):
  1. ContentPlugin       (의존성 없음)
  2. SnapshotPlugin      (의존성 없음)
  3. EventPlugin         (의존성 없음)
  4. MetadataPlugin      (Content, Snapshot 의존)
  5. LeasePlugin         (Metadata 의존)
  6. GCPlugin            (Metadata 의존)
  7. RuntimePluginV2     (Metadata, Event 의존)
  8. ServicePlugin       (Runtime, Metadata, Lease 등 의존)
  9. GRPCPlugin          (Service 의존)
```

### 2.4 플러그인 초기화 루프

```
소스 참조: cmd/containerd/server/server.go (Line 286~349)
```

```go
for _, p := range loaded {
    // 1. InitContext 생성
    initContext := plugin.NewContext(ctx, initialized, map[string]string{
        plugins.PropertyRootDir:      filepath.Join(config.Root, id),
        plugins.PropertyStateDir:     filepath.Join(config.State, id),
        plugins.PropertyGRPCAddress:  config.GRPC.Address,
        plugins.PropertyTTRPCAddress: config.TTRPC.Address,
    })

    // 2. 플러그인별 설정 디코딩
    if p.Config != nil {
        pc, _ := config.Decode(ctx, id, p.Config)
        initContext.Config = pc
    }

    // 3. 플러그인 초기화 실행
    result := p.Init(initContext)
    initialized.Add(result)

    // 4. 인스턴스 확인
    instance, err := result.Instance()
    if err != nil {
        if plugin.IsSkipPlugin(err) {
            // 건너뛰기 (플랫폼 미지원 등)
            continue
        }
        if _, ok := required[id]; ok {
            return nil, err  // 필수 플러그인 실패
        }
        continue
    }

    // 5. 서비스 인터페이스 확인
    if src, ok := instance.(grpcService); ok {
        grpcServices = append(grpcServices, src)
    }
    if src, ok := instance.(ttrpcService); ok {
        ttrpcServices = append(ttrpcServices, src)
    }
}
```

---

## 3. Content Store

### 3.1 역할

Content Store는 **Content Addressable Storage (CAS)**를 구현한다.
이미지 매니페스트, 설정, 레이어 등 모든 blob을 **SHA-256 digest**로 식별하여 저장한다.

### 3.2 설계 원칙

| 원칙 | 설명 |
|------|------|
| **Content Addressable** | 모든 blob은 내용의 해시(digest)로 식별됨 |
| **무결성 보장** | Commit 시 실제 해시와 기대 해시 비교 검증 |
| **중복 제거** | 동일 digest = 동일 blob, 자연스러운 중복 제거 |
| **불변성** | Commit된 콘텐츠는 변경 불가 (레이블만 mutable) |
| **이단계 쓰기** | Ingest(임시) → Commit(확정)으로 원자적 쓰기 |

### 3.3 파일시스템 레이아웃

```
{config.Root}/io.containerd.content.v1/
├── blobs/
│   └── sha256/
│       ├── aabbccdd...    ← 매니페스트 (JSON)
│       ├── eeff0011...    ← 이미지 설정 (JSON)
│       ├── 22334455...    ← 레이어 1 (tar+gzip)
│       └── 66778899...    ← 레이어 2 (tar+gzip)
│
└── ingest/
    └── ref-abc123/        ← 진행 중인 쓰기
        ├── data           ← 부분 쓰기된 데이터
        ├── ref            ← 참조 ID
        ├── startedat      ← 시작 시각
        └── updatedat      ← 최종 쓰기 시각
```

### 3.4 쓰기 흐름 (Ingest → Commit)

```
1. Writer 생성
   writer, _ := store.Writer(ctx, content.WithRef("my-download"))

2. 데이터 쓰기 (여러 번 가능, 재개 지원)
   writer.Write(data)    // {root}/ingest/my-download/data에 추가
   writer.Write(more)

3. Commit (원자적)
   writer.Commit(ctx, size, expectedDigest)
   ┌──────────────────────────────────────────┐
   │ 검증: 실제 크기 == size                   │
   │ 검증: 실제 digest == expectedDigest       │
   │ 이동: ingest/my-download → blobs/sha256/  │
   │ 정리: ingest/my-download 삭제             │
   └──────────────────────────────────────────┘

4. 읽기
   ra, _ := store.ReaderAt(ctx, descriptor)
   // ra는 io.ReaderAt + io.Closer + Size()
```

### 3.5 왜 이 설계인가?

**Q: 왜 Content Addressable Storage를 사용하는가?**

- **무결성**: 다운로드 중 손상 감지 (digest 불일치 시 거부)
- **중복 제거**: 같은 레이어를 사용하는 이미지가 여러 개여도 한 번만 저장
- **캐싱**: digest가 같으면 이미 있으므로 재다운로드 불필요
- **OCI 호환**: OCI Distribution Spec이 CAS 기반

**Q: 왜 이단계 쓰기(Ingest → Commit)를 사용하는가?**

- **원자성**: 부분 쓰기된 데이터가 정상 콘텐츠로 노출되지 않음
- **재개 가능**: 네트워크 중단 시 같은 ref로 Writer를 다시 열어 이어쓰기
- **정리 용이**: abort된 ingestion은 별도 관리 가능

---

## 4. Snapshotter

### 4.1 역할

Snapshotter는 **Copy-on-Write(CoW) 파일시스템 레이어**를 관리한다.
이미지의 각 레이어를 효율적으로 겹쳐서 컨테이너의 rootfs를 제공한다.

### 4.2 스냅샷 상태 머신

```
                  ┌──────────┐
                  │ (없음)   │
                  └─────┬────┘
                        │ Prepare(key, parent)
                        v
                  ┌──────────┐
            ┌─────│  Active  │─────┐
            │     └──────────┘     │
            │          │           │
            │   Commit(name, key)  │ Remove(key)
            │          │           │
            v          v           v
      ┌──────────┐  ┌──────────┐  ┌──────────┐
      │   View   │  │Committed │  │ (삭제됨) │
      └──────────┘  └──────────┘  └──────────┘
            │                │
            │ Remove()       │ Prepare(key, name)
            v                │  (새 Active 생성, name을 부모로)
      ┌──────────┐           │
      │ (삭제됨) │           v
      └──────────┘     ┌──────────┐
                       │  Active  │ (새 스냅샷)
                       └──────────┘

상태 전이 규칙:
  - Prepare() → Active (쓰기 가능)
  - View() → View (읽기 전용)
  - Commit() → Active가 Committed로 변환 (Active 삭제됨)
  - Committed만 부모가 될 수 있음
  - Active/View는 부모가 될 수 없음
```

### 4.3 overlay Snapshotter 예시

```
이미지: 3개 레이어 (layer-1, layer-2, layer-3)

파일시스템 구조:
{root}/io.containerd.snapshotter.v1.overlayfs/snapshots/
├── 1/fs/         ← layer-1 (Committed)
│   ├── bin/
│   └── etc/
├── 2/fs/         ← layer-2 (Committed, parent=1)
│   ├── usr/
│   └── var/
├── 3/fs/         ← layer-3 (Committed, parent=2)
│   └── app/
└── 4/fs/         ← container rootfs (Active, parent=3)
    └── (쓰기 가능)

overlay 마운트:
  mount -t overlay overlay \
    -o lowerdir=3/fs:2/fs:1/fs,upperdir=4/fs,workdir=4/work \
    /var/run/containerd/io.containerd.runtime.v2.task/.../rootfs
```

### 4.4 Snapshotter 구현체별 비교

| Snapshotter | CoW 메커니즘 | 장점 | 단점 |
|-------------|-------------|------|------|
| **overlayfs** | OverlayFS (커널) | 기본, 가장 빠름, 커널 지원 | 커널 4.x 이상 필요, inode 사용 |
| **native** | 디렉토리 복사 + 하드 링크 | 모든 FS 지원, 단순 | 느림, 공간 비효율 |
| **btrfs** | Btrfs subvolume snapshot | 블록 수준 CoW, 효율적 | Btrfs 전용 |
| **zfs** | ZFS snapshot/clone | 데이터 무결성, 압축 | ZFS 전용, 복잡 |
| **devmapper** | Device Mapper thin pool | 블록 수준, RHEL 기본 | 설정 복잡, 관리 오버헤드 |

### 4.5 왜 Snapshotter 추상화를 사용하는가?

- **파일시스템 독립**: 어떤 CoW FS든 동일한 API로 사용
- **이식성**: 커널/파일시스템에 따라 최적 구현 선택 가능
- **확장성**: 커스텀 Snapshotter 구현 가능 (원격 스냅샷, stargz 등)
- **테스트 용이**: native snapshotter로 특별한 FS 없이 테스트

---

## 5. Runtime / Shim

### 5.1 역할

Runtime은 **컨테이너 프로세스의 전체 수명 주기**를 관리한다.
containerd v2에서는 **Shim v2** 아키텍처를 사용하며,
각 컨테이너(또는 샌드박스)마다 별도의 Shim 프로세스가 관리한다.

### 5.2 Shim Manager 구조

```
소스 참조: core/runtime/v2/shim_manager.go
소스 참조: core/runtime/v2/task_manager.go
```

```
ShimManager
  │
  ├── 관리 대상: Shim 프로세스 목록
  │     ├── shim-1 (sandbox-abc) → TTRPC 연결
  │     ├── shim-2 (sandbox-def) → TTRPC 연결
  │     └── shim-3 (standalone)  → TTRPC 연결
  │
  ├── shim 시작: binary.go → fork/exec
  │     └── 입력: runtime 이름 → 바이너리 경로 해석
  │         "io.containerd.runc.v2" → containerd-shim-runc-v2
  │
  └── shim 로드: shim_load.go → 기존 shim 복구
        └── containerd 재시작 시 기존 shim 재연결

TaskManager
  │
  ├── Task CRUD: Create, Start, Kill, Delete
  ├── Exec: 추가 프로세스 실행
  ├── Checkpoint/Restore: CRIU를 통한 체크포인트
  └── I/O 관리: FIFO (stdin/stdout/stderr)
```

### 5.3 OCI Bundle 구조

Shim에게 전달되는 컨테이너 번들:

```
{state}/io.containerd.runtime.v2.task/{namespace}/{containerID}/
├── config.json        ← OCI Runtime Spec
├── rootfs/            ← 스냅샷에서 마운트된 rootfs
├── work/              ← overlay work 디렉토리
├── log.json           ← Shim 로그
├── init.pid           ← 초기 프로세스 PID
├── address             ← Shim TTRPC 주소
└── shim.sock          ← TTRPC Unix 소켓
```

### 5.4 Shim 프로세스 격리의 이유

**Q: 왜 containerd가 직접 runc를 실행하지 않고 Shim을 중간에 두는가?**

1. **데몬 독립**: containerd가 재시작되어도 컨테이너가 계속 실행
2. **리소스 격리**: Shim이 새 세션 리더(setsid)로 독립적 프로세스 그룹 형성
3. **I/O 릴레이**: Shim이 FIFO를 통해 stdin/stdout 관리, containerd 연결이 끊겨도 로그 보존
4. **좀비 방지**: Shim이 subreaper로 설정되어 컨테이너 자식 프로세스의 좀비 방지
5. **메모리 효율**: TTRPC 사용으로 gRPC 대비 50%+ 메모리 절감

### 5.5 TTRPC vs gRPC

| 특성 | gRPC | TTRPC |
|------|------|-------|
| 프레이밍 | HTTP/2 | 커스텀 (경량) |
| 직렬화 | Protobuf | Protobuf |
| 메모리 | 높음 (~30MB/연결) | 낮음 (~5MB/연결) |
| 용도 | 클라이언트 API | Shim 통신 |
| 전송 | TCP / Unix Socket | Unix Socket 전용 |
| 스트리밍 | 양방향 스트리밍 | 단방향 |

컨테이너가 수백 개 실행될 수 있으므로, 각 Shim에 gRPC를 사용하면
메모리 낭비가 심하다. TTRPC는 이 문제를 해결한다.

---

## 6. Metadata DB (BoltDB)

### 6.1 역할

Metadata DB는 containerd의 **모든 메타데이터를 통합 관리**하는 중앙 저장소이다.
BoltDB(bbolt) 기반의 키-값 저장소를 사용하며, **네임스페이스별 격리**를 제공한다.

```
소스 참조: core/metadata/db.go (Line 78~115)
```

### 6.2 DB 구조체

```go
type DB struct {
    db  Transactor                       // BoltDB 트랜잭터
    ss  map[string]*snapshotter          // 메타데이터 래핑된 스냅샷터
    cs  *contentStore                    // 메타데이터 래핑된 콘텐츠 스토어

    wlock sync.RWMutex                   // GC 중 쓰기 잠금
    dirty atomic.Uint32                  // GC 필요 표시

    dirtySS          map[string]struct{} // 삭제된 스냅샷 추적
    dirtyCS          bool                // 삭제된 콘텐츠 추적
    mutationCallbacks []func(bool)       // 변경 알림 콜백
    collectors       map[gc.ResourceType]Collector // GC 수집기

    dbopts dbOptions                     // 공유/격리 정책
}
```

### 6.3 메타데이터 래핑 패턴

```
Metadata DB는 Content Store와 Snapshotter를 "래핑"한다:

클라이언트 → metadata.contentStore → 실제 contentStore (파일시스템)
                    │
                    └─ BoltDB에 레이블, GC 참조 정보 저장

클라이언트 → metadata.snapshotter → 실제 snapshotter (overlay 등)
                    │
                    └─ BoltDB에 이름, 부모, 레이블 정보 저장

왜?
- 실제 데이터(blob, 스냅샷)는 각각의 백엔드가 관리
- 메타데이터(레이블, GC 참조, 네임스페이스)는 BoltDB가 관리
- 트랜잭션 보장: 메타데이터 변경이 원자적
```

### 6.4 트랜잭션 모델

BoltDB는 **MVCC(Multi-Version Concurrency Control)** 기반이다:

```
동시 접근 규칙:
  - 읽기 트랜잭션(View): 여러 개 동시 실행 가능
  - 쓰기 트랜잭션(Update): 한 번에 하나만 실행
  - GC 중: wlock.Lock()으로 쓰기 트랜잭션 차단

코드 패턴:
  // 읽기
  db.View(func(tx *bolt.Tx) error {
      bkt := tx.Bucket(bucketKey)
      value := bkt.Get(key)
      return nil
  })

  // 쓰기
  db.Update(func(tx *bolt.Tx) error {
      bkt := tx.Bucket(bucketKey)
      return bkt.Put(key, value)
  })
```

### 6.5 스키마 마이그레이션

```
소스 참조: core/metadata/db.go (Line 40~54)
  schemaVersion = "v1"
  dbVersion = 4
```

```
마이그레이션 전략:
  - schemaVersion(v1): 버킷 구조의 메이저 버전
  - dbVersion(4): 하위 호환 가능한 마이너 버전
  - Init() 시 현재 버전 확인 → 필요 시 마이그레이션 실행
  - 각 마이그레이션은 bolt.Tx 내에서 원자적 실행
```

---

## 7. Event Exchange

### 7.1 역할

Event Exchange는 containerd 내부의 **비동기 이벤트 버스**이다.
Publisher/Subscriber 패턴으로 동작하며, 모든 리소스 변경사항이 이벤트로 발행된다.

```
소스 참조: core/events/exchange/exchange.go (Line 36~49)
```

### 7.2 Exchange 구조

```go
type Exchange struct {
    broadcaster *goevents.Broadcaster   // docker/go-events 라이브러리
}

// 3개 인터페이스 동시 구현
var _ events.Publisher = &Exchange{}    // 이벤트 발행
var _ events.Forwarder = &Exchange{}   // 이벤트 전달 (다른 네임스페이스)
var _ events.Subscriber = &Exchange{}  // 이벤트 구독
```

### 7.3 이벤트 흐름

```
                    Exchange (Broadcaster)
                         │
         ┌───────────────┼───────────────┐
         │               │               │
    Publisher         Forwarder      Subscriber
         │               │               │
  Publish(topic, event)  │       Subscribe(filters...)
         │               │               │
         └───────┬───────┘               │
                 │                        │
                 v                        v
         Envelope 생성              <-chan *Envelope
         {                          필터 매칭:
           Timestamp,               - topic (경로 패턴)
           Namespace,               - namespace
           Topic,                   - event 필드
           Event (Any),
         }
```

### 7.4 Publish 과정

```go
func (e *Exchange) Publish(ctx context.Context, topic string, event Event) error {
    // 1. 네임스페이스 추출 (컨텍스트에서)
    namespace, _ := namespaces.NamespaceRequired(ctx)

    // 2. 토픽 유효성 검증
    validateTopic(topic)

    // 3. 이벤트를 protobuf Any로 마샬링
    encoded, _ := typeurl.MarshalAny(event)

    // 4. Envelope 생성
    envelope := Envelope{
        Timestamp: time.Now().UTC(),
        Namespace: namespace,
        Topic:     topic,
        Event:     encoded,
    }

    // 5. Broadcaster로 전파
    return e.broadcaster.Write(&envelope)
}
```

### 7.5 구독 필터링

```go
// 필터 문법 예시:
ch, errs := exchange.Subscribe(ctx,
    `topic=="/tasks/exit"`,            // 특정 토픽
    `topic~="/tasks/*"`,               // 토픽 패턴 매칭
    `namespace=="k8s.io"`,             // 특정 네임스페이스
    `event.container_id=="abc"`,       // 이벤트 필드 필터
)

for envelope := range ch {
    // 매칭된 이벤트 처리
}
```

### 7.6 왜 Event Exchange를 사용하는가?

- **디커플링**: 이벤트 생산자와 소비자가 서로 독립적
- **CRI 연동**: kubelet은 이벤트를 통해 컨테이너 상태 변화 감지
- **GC 트리거**: 리소스 삭제 이벤트가 GC Scheduler에 전달
- **모니터링**: 외부 도구가 이벤트를 구독하여 상태 추적
- **재시작 복구**: containerd 재시작 시 이벤트 기반으로 상태 재구성

---

## 8. GC Scheduler

### 8.1 역할

GC Scheduler는 **미사용 리소스(콘텐츠, 스냅샷)를 자동으로 정리**한다.
Tricolor Mark-and-Sweep 알고리즘을 사용하며, Lease 기반으로 진행 중 작업의 리소스를 보호한다.

```
소스 참조: plugins/gc/scheduler.go (Line 35~90) - 설정
소스 참조: core/metadata/gc.go (Line 35~58) - 리소스 타입
```

### 8.2 GC 리소스 타입

```go
const (
    ResourceUnknown   gc.ResourceType = iota
    ResourceContent                          // 콘텐츠 (blob)
    ResourceSnapshot                         // 스냅샷
    ResourceContainer                        // 컨테이너
    ResourceTask                             // 태스크
    ResourceImage                            // 이미지
    ResourceLease                            // 리스
    ResourceIngest                           // 진행 중 쓰기
    ResourceStream                           // 스트림
    ResourceMount                            // 마운트
)
```

### 8.3 GC 참조 레이블

```
소스 참조: core/metadata/gc.go (Line 67~76)
```

GC는 BoltDB의 **레이블을 통해 참조 관계를 추적**한다:

| 레이블 접두사 | 의미 | 예시 |
|--------------|------|------|
| `containerd.io/gc.root` | 이 리소스는 GC root | 이미지, 컨테이너 |
| `containerd.io/gc.ref.content` | 콘텐츠 참조 | 매니페스트 → 설정 digest |
| `containerd.io/gc.ref.snapshot.{name}` | 스냅샷 참조 | 컨테이너 → 스냅샷 키 |
| `containerd.io/gc.ref.image` | 이미지 참조 | 컨테이너 → 이미지 이름 |

```
참조 그래프 예시:

Image("nginx:latest")
  └─ gc.ref.content = sha256:manifest
        └─ manifest blob
            ├─ gc.ref.content.config = sha256:config
            │       └─ config blob
            ├─ gc.ref.content.layer.0 = sha256:layer1
            │       └─ layer1 blob
            └─ gc.ref.content.layer.1 = sha256:layer2
                    └─ layer2 blob

Container("my-nginx")
  ├─ gc.ref.snapshot.overlayfs = container-rootfs-key
  │       └─ Active snapshot
  │           └─ parent → Committed snapshot (layer chain)
  └─ gc.ref.image = nginx:latest
```

### 8.4 Tricolor Mark-and-Sweep 구현

```
Phase 1: Mark (읽기 트랜잭션)
  ┌─────────────────────────────────┐
  │ wlock.RLock()                   │
  │                                 │
  │ 1. 모든 리소스를 White로 초기화  │
  │                                 │
  │ 2. GC Root 식별:               │
  │    - 이미지 (gc.root 레이블)    │
  │    - 컨테이너                   │
  │    - 활성 Lease의 리소스        │
  │    - 진행 중 Ingestion          │
  │                                 │
  │ 3. BFS/DFS로 참조 추적:        │
  │    - White → Gray → Black      │
  │    - gc.ref.* 레이블 따라가기   │
  │                                 │
  │ 결과: Black(보존) / White(삭제) │
  │                                 │
  │ wlock.RUnlock()                 │
  └─────────────────────────────────┘

Phase 2: Sweep (쓰기 잠금)
  ┌─────────────────────────────────┐
  │ wlock.Lock()                    │
  │                                 │
  │ 1. White 콘텐츠 삭제            │
  │    - Content Store에서 blob 삭제│
  │                                 │
  │ 2. White 스냅샷 삭제            │
  │    - Snapshotter.Remove() 호출  │
  │                                 │
  │ 3. dirty 플래그 초기화          │
  │                                 │
  │ wlock.Unlock()                  │
  └─────────────────────────────────┘
```

### 8.5 GC 스케줄링 전략

```
이벤트 기반 스케줄링:

  mutationCallback(dirty=true)
         │
         v
  ┌──────────────────────────┐
  │ 조건 확인:               │
  │  deletion_threshold      │──> 삭제 횟수 초과 시 즉시 스케줄
  │  mutation_threshold      │──> 변경 횟수 초과 시 다음 GC 포함
  │  pause_threshold         │──> GC 일시정지 비율 기반 간격 조정
  └──────┬───────────────────┘
         │
         v
  ┌──────────────────────────┐
  │ schedule_delay 대기      │
  │ (기본: 0ms)              │
  └──────┬───────────────────┘
         │
         v
  ┌──────────────────────────┐
  │ GC 실행                  │
  │ Mark → Sweep             │
  └──────────────────────────┘
```

---

## 9. Lease Manager

### 9.1 역할

Lease Manager는 **진행 중인 작업의 리소스를 GC로부터 보호**한다.
이미지 Pull, 컨테이너 생성 등의 장기 작업 중에 중간 산물이 삭제되지 않도록 한다.

### 9.2 Lease의 필요성

```
문제 시나리오 (Lease 없이):

  시점 T1: 이미지 Pull 시작
    → layer-1 다운로드 완료, Content Store에 저장
    → layer-2 다운로드 중...

  시점 T2: GC 실행
    → layer-1이 아직 어떤 이미지/컨테이너에도 참조되지 않음
    → GC가 layer-1을 삭제함!

  시점 T3: layer-2 다운로드 완료
    → Unpack 시도 → layer-1이 없음 → 에러!

해결 (Lease 사용):

  시점 T1: Lease 생성 후 이미지 Pull 시작
    → layer-1 다운로드 → Lease에 리소스 등록 (보호)
    → layer-2 다운로드 중...

  시점 T2: GC 실행
    → layer-1이 Lease에 의해 보호됨
    → GC가 건너뜀

  시점 T3: Pull 완료, 이미지 레코드 생성
    → Lease 삭제 (이제 이미지가 직접 참조하므로 보호 불필요)
```

### 9.3 만료 기반 Lease

```go
// 24시간 후 만료되는 Lease 생성
lease, _ := manager.Create(ctx, leases.WithExpiration(24*time.Hour))
// 내부: Labels["containerd.io/gc.expire"] = "2026-03-05T12:00:00Z"

// 만료 시간이 지난 Lease는 GC에서 무시됨
// → Lease의 리소스가 보호되지 않음 → GC 대상이 됨
```

---

## 10. 컴포넌트 상호작용 요약

### 10.1 이미지 Pull 시 컴포넌트 상호작용

```
1. Lease Manager → Lease 생성 (리소스 보호)
2. Transfer Service → 레지스트리에서 콘텐츠 다운로드
3. Content Store → blob 저장 (Ingest → Commit)
4. Lease Manager → 각 blob을 Lease에 등록
5. Snapshotter → 레이어 언팩 (Prepare → Apply → Commit)
6. Lease Manager → 각 스냅샷을 Lease에 등록
7. Metadata DB → 이미지 레코드 생성
8. Lease Manager → Lease 삭제 (이미지가 참조하므로 보호 불필요)
9. Event Exchange → /images/create 이벤트 발행
```

### 10.2 컨테이너 실행 시 컴포넌트 상호작용

```
1. Metadata DB → 컨테이너 레코드 생성
2. Snapshotter → Prepare(rootfs, image-chain-id) → Active 스냅샷
3. Runtime (Shim Manager) → Shim 프로세스 시작
4. Runtime (Task Manager) → TTRPC로 Task Create → runc create
5. Runtime (Task Manager) → TTRPC로 Task Start → runc start
6. Event Exchange → /tasks/create, /tasks/start 이벤트 발행
```

### 10.3 GC 시 컴포넌트 상호작용

```
1. GC Scheduler → mutation 임계값 초과 감지
2. GC Scheduler → schedule_delay 후 GC 트리거
3. Metadata DB → wlock.Lock() (쓰기 차단)
4. Metadata DB → Tricolor Mark (BoltDB 읽기 트랜잭션)
   - GC Root: Image, Container, Active Lease, Ingestion
   - 참조 추적: gc.ref.* 레이블
5. Content Store → 미참조 blob 삭제
6. Snapshotter → 미참조 스냅샷 삭제
7. Metadata DB → dirty 초기화, wlock.Unlock()
8. GC Scheduler → 통계 기록 (소요 시간, 삭제 개수)
```
