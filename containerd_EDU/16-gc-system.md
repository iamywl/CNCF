# 16. 가비지 컬렉션 시스템 (GC System)

## 목차

1. [개요](#1-개요)
2. [Tricolor GC 알고리즘](#2-tricolor-gc-알고리즘)
3. [GC 리소스 타입](#3-gc-리소스-타입)
4. [참조 레이블 시스템](#4-참조-레이블-시스템)
5. [GC 스케줄러](#5-gc-스케줄러)
6. [GC 트리거 조건](#6-gc-트리거-조건)
7. [GC 실행 흐름](#7-gc-실행-흐름)
8. [Mark 단계: getMarked()](#8-mark-단계-getmarked)
9. [Sweep 단계: scanAll/remove](#9-sweep-단계-scanallremove)
10. [Lease 기반 수명 관리](#10-lease-기반-수명-관리)
11. [Back 참조 (bref)](#11-back-참조-bref)
12. [Flat 리스와 최적화](#12-flat-리스와-최적화)
13. [Collectible Resources](#13-collectible-resources)
14. [왜 이렇게 설계했는가](#14-왜-이렇게-설계했는가)

---

## 1. 개요

containerd의 가비지 컬렉션(GC) 시스템은 사용하지 않는 콘텐츠(blob), 스냅샷, 이미지 등의 리소스를 자동으로 정리한다. Tricolor Mark-Sweep 알고리즘을 사용하며, 레이블 기반 참조 추적과 Lease 기반 수명 관리를 결합한다.

**핵심 소스 파일:**

| 파일 | 역할 |
|------|------|
| `pkg/gc/gc.go` | Tricolor, ConcurrentMark, Sweep 알고리즘 |
| `plugins/gc/scheduler.go` | gcScheduler, 트리거/스케줄링 |
| `core/metadata/gc.go` | ResourceType, 참조 레이블, scanRoots/references/scanAll/remove |
| `core/metadata/db.go` | GarbageCollect(), getMarked(), wlock |

---

## 2. Tricolor GC 알고리즘

containerd는 고전적인 Tricolor(삼색) 마킹 알고리즘을 사용한다.

### 알고리즘 원리

```
+-------------------------------------------------------------------+
|                    Tricolor Mark-Sweep                              |
|                                                                    |
|  색상      의미              초기 상태                               |
|  ----      ----              --------                               |
|  White     미방문 (삭제 대상)  모든 노드                              |
|  Gray      방문 시작 (처리 중) 루트 노드들                            |
|  Black     방문 완료 (유지)    없음                                   |
|                                                                    |
|  알고리즘:                                                          |
|  1. 모든 루트를 Gray에 추가                                          |
|  2. Gray 스택에서 하나를 꺼냄                                        |
|  3. 참조하는 모든 노드를 Gray에 추가 (아직 방문 안한 것만)             |
|  4. 꺼낸 노드를 Black으로 이동                                       |
|  5. Gray가 빌 때까지 2-4 반복                                        |
|  6. 남은 White 노드들 → 삭제 대상                                    |
+-------------------------------------------------------------------+
```

### 구현 코드

```go
// 소스: pkg/gc/gc.go (64-100행)
func Tricolor(roots []Node, refs func(ref Node) ([]Node, error)) (map[Node]struct{}, error) {
    var (
        grays     []Node                // Gray 스택
        seen      = map[Node]struct{}{} // White가 아닌 것들 (방문함)
        reachable = map[Node]struct{}{} // Black (도달 가능)
    )

    // 루트를 Gray에 추가
    grays = append(grays, roots...)
    for _, root := range roots {
        seen[root] = struct{}{}
    }

    // Gray가 빌 때까지 반복
    for len(grays) > 0 {
        // 스택에서 하나 꺼냄 (depth-first)
        id := grays[len(grays)-1]
        grays = grays[:len(grays)-1]

        // 참조 노드들을 Gray에 추가
        rs, err := refs(id)
        for _, target := range rs {
            if _, ok := seen[target]; !ok {
                grays = append(grays, target)
                seen[target] = struct{}{}
            }
        }

        // 상위 비트 제거 후 Black으로 이동
        id.Type = id.Type & ResourceMax
        reachable[id] = struct{}{}
    }

    return reachable, nil
}
```

### Node 구조

```go
// 소스: pkg/gc/gc.go (40-44행)
type Node struct {
    Type      ResourceType   // 리소스 타입 (Content, Snapshot, Container 등)
    Namespace string         // 네임스페이스
    Key       string         // 리소스 식별자
}
```

### 시각적 예시

```
초기 상태:
  Roots: [Image-A, Container-X]

  Image-A ──ref──> Content-1 ──ref──> Content-2
                                       ↑
  Container-X ──snapshot──> Snap-1 ──parent──> Snap-0

  (미참조) Image-B ──ref──> Content-3

Mark 과정:
  Gray: [Image-A, Container-X]

  Step 1: Image-A → Black, refs → Content-1 → Gray
  Step 2: Container-X → Black, refs → Snap-1 → Gray
  Step 3: Content-1 → Black, refs → Content-2 → Gray
  Step 4: Snap-1 → Black, refs → Snap-0 → Gray
  Step 5: Content-2 → Black
  Step 6: Snap-0 → Black

결과:
  Black (유지): Image-A, Container-X, Content-1, Content-2, Snap-1, Snap-0
  White (삭제): Image-B, Content-3
```

---

## 3. GC 리소스 타입

```go
// 소스: core/metadata/gc.go (36-58행)
const (
    ResourceUnknown   gc.ResourceType = iota  // 0
    ResourceContent                           // 1: blob 콘텐츠
    ResourceSnapshot                          // 2: 파일시스템 스냅샷
    ResourceContainer                         // 3: 컨테이너
    ResourceTask                              // 4: 태스크
    ResourceImage                             // 5: 이미지
    ResourceLease                             // 6: 리스
    ResourceIngest                            // 7: 진행 중인 콘텐츠 쓰기
    resourceEnd                               // 내부 리소스 끝
    ResourceStream                            // 스트리밍 리소스
    ResourceMount                             // 마운트 리소스
)
```

### 리소스 간 참조 관계

```
ResourceImage
    ├──ref──> ResourceContent (이미지 manifest/config/layer blob)
    └──ref──> ResourceSnapshot (이미지 스냅샷)

ResourceContainer
    ├──ref──> ResourceSnapshot (루트 파일시스템)
    └──ref──> ResourceContent (이미지 참조)

ResourceSnapshot
    └──parent──> ResourceSnapshot (부모 스냅샷)

ResourceLease
    ├──ref──> ResourceContent
    ├──ref──> ResourceSnapshot
    ├──ref──> ResourceIngest
    └──ref──> ResourceImage

ResourceIngest
    └──expected──> ResourceContent (예상 다이제스트)
```

### Flat 변형 타입

```go
// 소스: core/metadata/gc.go (61-64행)
const (
    resourceContentFlat  = ResourceContent | 0x20
    resourceSnapshotFlat = ResourceSnapshot | 0x20
    resourceImageFlat    = ResourceImage | 0x20
)
```

Flat 타입은 참조를 따라가지 않는다. Flat 리스에서 사용되며, 리스된 객체 자체만 보호하고 그 하위 참조는 보호하지 않는다.

---

## 4. 참조 레이블 시스템

containerd는 레이블을 사용하여 오브젝트 간 참조 관계를 명시한다.

### Forward 참조 (부모 → 자식)

```go
// 소스: core/metadata/gc.go (73-77행)
labelGCRef        = []byte("containerd.io/gc.ref.")
labelGCSnapRef    = []byte("containerd.io/gc.ref.snapshot.")
labelGCContentRef = []byte("containerd.io/gc.ref.content")
labelGCImageRef   = []byte("containerd.io/gc.ref.image")
```

| 레이블 패턴 | 참조 대상 | 예시 |
|------------|---------|------|
| `containerd.io/gc.ref.content` | Content blob | `sha256:abc123...` |
| `containerd.io/gc.ref.content.config` | Content blob (설정) | `sha256:def456...` |
| `containerd.io/gc.ref.content.l.0` | Content blob (레이어 0) | `sha256:789...` |
| `containerd.io/gc.ref.snapshot.overlayfs` | 스냅샷 | `sha256:abc-layer-0` |
| `containerd.io/gc.ref.image` | 이미지 | `docker.io/library/nginx:latest` |

### Back 참조 (자식 → 부모)

```go
// 소스: core/metadata/gc.go (83-86행)
labelGCContainerBackRef = []byte("containerd.io/gc.bref.container")
labelGCContentBackRef   = []byte("containerd.io/gc.bref.content")
labelGCImageBackRef     = []byte("containerd.io/gc.bref.image")
labelGCSnapBackRef      = []byte("containerd.io/gc.bref.snapshot.")
```

### 특수 레이블

```go
// 소스: core/metadata/gc.go (67행, 93-101행)
labelGCRoot   = []byte("containerd.io/gc.root")    // 루트 오브젝트 표시
labelGCExpire = []byte("containerd.io/gc.expire")   // 만료 시간 (RFC3339)
labelGCFlat   = []byte("containerd.io/gc.flat")     // Flat 리스 표시
```

### 레이블 기반 참조 추적 예시

```
Image "docker.io/library/nginx:latest"
├── target.digest: sha256:manifest-abc
└── labels:
    ├── containerd.io/gc.ref.content.config: sha256:config-def
    ├── containerd.io/gc.ref.content.l.0: sha256:layer-001
    ├── containerd.io/gc.ref.content.l.1: sha256:layer-002
    ├── containerd.io/gc.ref.snapshot.overlayfs/0: sha256:snap-001
    └── containerd.io/gc.ref.snapshot.overlayfs/1: sha256:snap-002

GC가 이미지를 루트로 인식하면:
  → sha256:manifest-abc (target에서)
  → sha256:config-def (gc.ref.content에서)
  → sha256:layer-001, layer-002 (gc.ref.content에서)
  → sha256:snap-001, snap-002 (gc.ref.snapshot에서)
  모두 Black으로 마킹됨
```

---

## 5. GC 스케줄러

### gcScheduler 구조체

```go
// 소스: plugins/gc/scheduler.go (138-151행)
type gcScheduler struct {
    c collector                    // GarbageCollect() 호출 대상

    eventC chan mutationEvent      // 변경 이벤트 채널

    waiterL sync.Mutex             // 대기자 보호
    waiters []chan gc.Stats        // GC 완료 대기자들

    pauseThreshold    float64      // 기본 0.02 (2%)
    deletionThreshold int          // 기본 0 (비활성)
    mutationThreshold int          // 기본 100
    scheduleDelay     time.Duration // 기본 0ms
    startupDelay      time.Duration // 기본 100ms
}
```

### 설정 파라미터

```go
// 소스: plugins/gc/scheduler.go (35-84행)
type config struct {
    PauseThreshold    float64         `toml:"pause_threshold"`
    DeletionThreshold int             `toml:"deletion_threshold"`
    MutationThreshold int             `toml:"mutation_threshold"`
    ScheduleDelay     tomlext.Duration `toml:"schedule_delay"`
    StartupDelay      tomlext.Duration `toml:"startup_delay"`
}
```

| 파라미터 | 기본값 | 설명 |
|---------|-------|------|
| PauseThreshold | 0.02 | GC 일시 정지가 전체 시간의 최대 2% |
| DeletionThreshold | 0 | 삭제 N회 후 GC 트리거 (0=비활성) |
| MutationThreshold | 100 | 변경 100회 후 다음 스케줄 GC에서 실행 |
| ScheduleDelay | 0ms | 트리거 후 GC 시작까지 지연 |
| StartupDelay | 100ms | 시작 후 첫 GC까지 지연 |

### PauseThreshold 알고리즘

```go
// 소스: plugins/gc/scheduler.go (334-347행)
if s.pauseThreshold > 0.0 {
    avg := float64(gcTimeSum) / float64(collections)
    if avg < minimumGCTime {
        avg = minimumGCTime  // 최소 5ms
    }
    interval = time.Duration(avg/s.pauseThreshold - avg)
}
```

수학적 의미:
```
PauseThreshold = GC시간 / (GC시간 + 간격)

0.02 = avg / (avg + interval)
interval = avg/0.02 - avg
interval = avg * (1/0.02 - 1)
interval = avg * 49

예시:
  GC 평균 시간 = 10ms
  interval = 10ms * 49 = 490ms
  → 매 490ms마다 10ms GC 실행
  → 전체 시간의 약 2%가 GC에 사용됨
```

---

## 6. GC 트리거 조건

### mutationEvent

```go
// 소스: plugins/gc/scheduler.go (127-131행)
type mutationEvent struct {
    ts       time.Time   // 이벤트 발생 시각
    mutation bool        // DB 변경 여부
    dirty    bool        // 삭제 발생 여부
}
```

### 트리거 소스

```
1. 수동 트리거 (ScheduleAndWait)
   → mutationEvent{mutation: false, dirty: false}
   → triggered = true

2. DB 변경 콜백 (mutationCallback)
   → mutationEvent{mutation: true, dirty: dirty}
   → mutations++, dirty면 deletions++

3. 시작 시 자동 (startupDelay)
   → 100ms 후 첫 GC 실행
```

### 스케줄링 결정 로직

```go
// 소스: plugins/gc/scheduler.go (255-294행)
// run() 내부 select 이벤트 처리

case e := <-s.eventC:
    // 카운터 업데이트
    if e.dirty {
        deletions++
    }
    if e.mutation {
        mutations++
    } else {
        triggered = true   // 수동 트리거
    }

    // 즉시 스케줄링 조건 확인
    if triggered ||
        (s.deletionThreshold > 0 && deletions >= s.deletionThreshold) ||
        (nextCollection == nil && (
            (s.deletionThreshold == 0 && deletions > 0) ||
            (s.mutationThreshold > 0 && mutations >= s.mutationThreshold))) {

        schedC, nextCollection = schedule(s.scheduleDelay)
    }
```

### 스케줄링 상태 머신

```
                    +--------+
              start |        |
              ----->| 대기중  |
                    |        |
                    +---+----+
                        |
            startupDelay 후 또는
            mutationEvent 수신
                        |
                        v
                    +--------+
                    | 스케줄  |
                    | 예약됨  |
                    +---+----+
                        |
              scheduleDelay 후
                        |
                        v
                    +--------+     실행 불필요
                    | 실행   |------(삭제/변경 없음)----+
                    | 판단   |                          |
                    +---+----+                          |
                        |                               |
                  실행 필요                              |
                        |                               |
                        v                               v
                    +--------+                     +--------+
                    | GC     |                     | 다음   |
                    | 실행   |--interval 계산----->| 스케줄 |
                    +--------+                     +--------+
```

---

## 7. GC 실행 흐름

```go
// 소스: core/metadata/db.go (371-479행)
func (m *DB) GarbageCollect(ctx context.Context) (gc.Stats, error) {
```

### 전체 흐름

```
GarbageCollect()
    |
    +-- (1) m.wlock.Lock()          ← 배타적 잠금 (쓰기 차단)
    |
    +-- (2) startGCContext()         ← 외부 Collector 초기화
    |
    +-- (3) getMarked(ctx, c)        ← Phase 1: Mark
    |       └── View 트랜잭션
    |       └── scanRoots() → 루트 노드 수집
    |       └── Tricolor(roots, refs) → 도달 가능 노드 계산
    |
    +-- (4) db.Update()              ← Phase 2: Sweep
    |       └── scanAll() → 모든 노드 열거
    |       └── rm(node) → marked에 없으면 삭제
    |           └── dirty 스냅샷터/콘텐츠 스토어 추적
    |
    +-- (5) publishEvents()          ← 삭제 이벤트 비동기 발행
    |
    +-- (6) dirty.Store(0)           ← dirty 카운터 리셋
    |
    +-- (7) 백엔드 정리 (병렬)
    |       ├── cleanupSnapshotter() ← dirty 스냅샷터 GC
    |       └── cleanupContent()     ← dirty 콘텐츠 스토어 GC
    |
    +-- (8) c.finish()               ← 외부 Collector 정리
    |
    +-- (9) m.wlock.Unlock()         ← 잠금 해제
    |
    +-- (10) wg.Wait()               ← 백엔드 정리 완료 대기
    |
    └── return stats, nil
```

---

## 8. Mark 단계: getMarked()

```go
// 소스: core/metadata/db.go (482-528행)
func (m *DB) getMarked(ctx context.Context, c *gcContext) (map[gc.Node]struct{}, error) {
    var marked map[gc.Node]struct{}
    m.db.View(func(tx *bolt.Tx) error {
        // 루트 수집
        var nodes []gc.Node
        roots = make(chan gc.Node)
        go func() {
            for n := range roots {
                nodes = append(nodes, n)
            }
        }()
        c.scanRoots(ctx, tx, roots)
        close(roots)

        // 참조 함수
        refs := func(n gc.Node) ([]gc.Node, error) {
            var sn []gc.Node
            c.references(ctx, tx, n, func(nn gc.Node) {
                sn = append(sn, nn)
            })
            return sn, nil
        }

        // Tricolor 실행
        reachable, err := gc.Tricolor(nodes, refs)
        marked = reachable
    })
    return marked, nil
}
```

### scanRoots: 루트 노드 식별

```go
// 소스: core/metadata/gc.go (336-603행)
func (c *gcContext) scanRoots(ctx context.Context, tx *bolt.Tx, nc chan<- gc.Node) error
```

루트로 인식되는 오브젝트:

```
루트 노드 유형:

1. 만료되지 않은 Lease
   └── Lease에 포함된 Content, Snapshot, Ingest, Image
   └── Flat 리스면 Flat 타입으로 표시

2. 만료되지 않은 Image (gc.expire 레이블 없거나 아직 유효)
   └── Image 자체가 루트

3. gc.root 레이블이 있는 Content/Snapshot
   └── Content/Snapshot 자체가 루트

4. Back 참조(bref)가 있는 Content/Snapshot
   └── bref 대상이 존재하면 루트로 간주

5. 만료되지 않은 Ingest
   └── expireAt 이전인 Ingest가 루트

6. Container
   └── 모든 Container가 루트

7. Sandbox
   └── Sandbox의 레이블 참조

8. Active 외부 리소스
   └── collectors의 Active() 결과
```

### references: 참조 따라가기

```go
// 소스: core/metadata/gc.go (606-699행)
func (c *gcContext) references(ctx context.Context, tx *bolt.Tx,
    node gc.Node, fn func(gc.Node)) error
```

각 리소스 타입별 참조 해석:

```
ResourceContent:
  └── sendLabelRefs() → gc.ref.* 레이블에서 참조 추출

ResourceSnapshot:
  ├── parent → 부모 스냅샷 (같은 스냅샷터)
  └── sendLabelRefs() → gc.ref.* 레이블에서 참조 추출

ResourceImage:
  ├── target.digest → Content (이미지 매니페스트)
  └── sendLabelRefs() → gc.ref.* 레이블에서 참조 추출

ResourceIngest:
  └── expected → Content (기대되는 다이제스트)

ResourceContainer:
  ├── snapshotter + snapshotkey → Snapshot
  └── sendLabelRefs() → gc.ref.* 레이블에서 참조 추출
```

### sendLabelRefs: 레이블에서 참조 추출

```go
// 소스: core/metadata/gc.go (875-895행)
func (c *gcContext) sendLabelRefs(ns string, bkt *bolt.Bucket,
    fn func(gc.Node), bref func(gc.Node), root func()) error {

    lbkt := bkt.Bucket(bucketKeyObjectLabels)
    if lbkt != nil {
        lc := lbkt.Cursor()
        for i := range c.labelHandlers {
            // 커서를 핸들러 키로 Seek하여 해당 프리픽스의 레이블만 처리
            for k, v := lc.Seek(c.labelHandlers[i].key);
                k != nil && bytes.HasPrefix(k, c.labelHandlers[i].key);
                k, v = lc.Next() {

                if c.labelHandlers[i].fn != nil {
                    c.labelHandlers[i].fn(ns, k, v, fn)    // Forward ref
                } else if c.labelHandlers[i].bref != nil {
                    c.labelHandlers[i].bref(ns, k, v, bref) // Back ref
                } else if root != nil {
                    root()                                    // gc.root
                }
            }
        }
    }
}
```

레이블 핸들러는 키 순서로 정렬되어 있어, BoltDB 커서의 Seek 연산을 효율적으로 활용한다.

---

## 9. Sweep 단계: scanAll/remove

### scanAll: 모든 리소스 열거

```go
// 소스: core/metadata/gc.go (702-797행)
func (c *gcContext) scanAll(ctx context.Context, tx *bolt.Tx,
    fn func(ctx context.Context, n gc.Node) error) error
```

모든 네임스페이스를 순회하며 모든 리소스를 `fn`에 전달한다. `fn`은 `rm` 함수로, marked에 없는 노드를 삭제한다.

### remove: 리소스 삭제

```go
// 소스: core/metadata/gc.go (800-872행)
func (c *gcContext) remove(ctx context.Context, tx *bolt.Tx, node gc.Node) (interface{}, error) {
    switch node.Type {
    case ResourceContent:
        // v1/{ns}/content/blob/{digest} 버킷 삭제
        cbkt.DeleteBucket([]byte(node.Key))

    case ResourceSnapshot:
        // v1/{ns}/snapshots/{snapshotter}/{key} 버킷 삭제
        ssbkt.DeleteBucket([]byte(key))
        // → dirtySS[snapshotter] 기록
        // → SnapshotRemove 이벤트 반환

    case ResourceImage:
        // v1/{ns}/images/{name} 버킷 삭제
        ibkt.DeleteBucket([]byte(node.Key))
        // → ImageDelete 이벤트 반환

    case ResourceLease:
        // v1/{ns}/leases/{id} 버킷 삭제
        lbkt.DeleteBucket([]byte(node.Key))

    case ResourceIngest:
        // v1/{ns}/content/ingests/{ref} 버킷 삭제
        ibkt.DeleteBucket([]byte(node.Key))
        // → dirtyCS 기록
    }
}
```

### 메타데이터 삭제 vs 백엔드 삭제

```
Phase 2 (Sweep - wlock 보유 중):
  BoltDB에서 메타데이터만 삭제
  → 빠름, 트랜잭션 내에서 완료

Phase 3 (Backend Cleanup - wlock 보유 중, 병렬):
  실제 파일시스템 데이터 삭제
  ├── cleanupSnapshotter(): 파일시스템 스냅샷 삭제
  └── cleanupContent(): blob 파일 삭제
  → 느림, I/O 작업
```

---

## 10. Lease 기반 수명 관리

Lease는 GC로부터 리소스를 보호하는 메커니즘이다.

### Lease의 역할

```
이미지 풀 중:
  T=0  Lease 생성
  T=1  Layer 1 다운로드 시작 → Ingest 생성
  T=2  Layer 1 다운로드 완료 → Content에 커밋
  T=3  GC 실행!
       └── Layer 1은 아직 이미지에 연결 안 됨
       └── Lease가 없으면 삭제될 수 있음!
       └── Lease가 Content를 보호 → 삭제 안 됨
  T=4  Layer 2 다운로드
  T=5  이미지 메타데이터 저장 (모든 참조 연결)
  T=6  Lease 삭제 → 이제 참조가 보호함
```

### Lease 구조 (BoltDB)

```
v1/{namespace}/leases/{lease-id}/
├── labels/
│   ├── containerd.io/gc.expire: "2024-01-15T11:00:00Z"
│   └── containerd.io/gc.flat: ""  (선택)
├── content/
│   ├── sha256:abc...   → 리스된 콘텐츠
│   └── sha256:def...
├── snapshots/
│   └── overlayfs/
│       └── layer-001   → 리스된 스냅샷
├── ingests/
│   └── ref-001         → 리스된 인제스트
└── images/
    └── nginx:latest    → 리스된 이미지
```

### Lease와 GC의 상호작용

```
scanRoots() 중:

리스 순회:
  └── 리스 {id}:
      ├── expire 레이블 확인
      │   └── 만료되었으면 → 건너뜀 (루트 아님)
      │   └── 만료 안 되었으면 → 계속
      │
      ├── flat 레이블 확인
      │   └── flat이면 → resourceContentFlat 타입 사용
      │   └── flat 아니면 → ResourceContent 타입 사용
      │
      ├── Lease 자체를 루트로 등록
      │
      └── 리스된 리소스를 루트로 등록:
          ├── content/ 하위 → ContentFlat 또는 Content
          ├── snapshots/ 하위 → SnapshotFlat 또는 Snapshot
          ├── ingests/ 하위 → Ingest
          └── images/ 하위 → ImageFlat 또는 Image
```

---

## 11. Back 참조 (bref)

### Forward 참조 vs Back 참조

```
Forward 참조 (gc.ref.*):
  부모 → 자식 방향
  부모 오브젝트에 자식 참조 레이블 추가
  부모가 존재해야 자식이 보호됨

  Image ──gc.ref.content──> Content
  (부모)                    (자식)

Back 참조 (gc.bref.*):
  자식 → 부모 방향
  자식 오브젝트에 부모 참조 레이블 추가
  부모를 업데이트하지 않고도 참조 설정 가능

  Content ──gc.bref.image──> Image
  (자식)                     (부모)
```

### 왜 Back 참조가 필요한가

1. **부모가 아직 없는 경우**: 자식이 먼저 생성되고 부모가 나중에 생성될 때
2. **부모 업데이트 불가**: 다른 프로세스/네임스페이스가 소유한 부모를 수정할 수 없을 때
3. **원자적 참조**: 부모와 자식을 동시에 수정하지 않고 참조 설정

### Back 참조 처리

```go
// 소스: core/metadata/gc.go (606-612행)
func (c *gcContext) references(ctx context.Context, tx *bolt.Tx,
    node gc.Node, fn func(gc.Node)) error {
    if refs, ok := c.backRefs[node]; ok {
        for _, ref := range refs {
            fn(ref)
        }
    }
    // ... 일반 참조 처리
}
```

Back 참조는 scanRoots 단계에서 수집되어 `c.backRefs` 맵에 저장된다. references() 호출 시 일반 참조와 함께 처리된다.

---

## 12. Flat 리스와 최적화

### Flat 리스란

일반 리스는 리스된 오브젝트의 모든 참조를 재귀적으로 따라간다. Flat 리스는 리스된 오브젝트 자체만 보호하고, 그 참조는 보호하지 않는다.

```
일반 리스:
  Lease → Image → Content(manifest) → Content(config) → Content(layer)
  └── 모든 것 보호됨

Flat 리스:
  Lease → Image
  └── Image만 보호됨
  └── Content(manifest)는 다른 참조가 없으면 삭제 가능
```

### Flat 타입의 동작

```go
// references() 내부:

case ResourceImage:
    // target → Content로 참조
    ctype := ResourceContent

case resourceImageFlat:
    // target → resourceContentFlat로 참조
    ctype = resourceContentFlat
    // 레이블 참조는 보내지 않음 (return nil)
```

```go
case ResourceSnapshot:
    // parent → 같은 타입의 부모 스냅샷
    // + sendLabelRefs()

case resourceSnapshotFlat:
    // parent → 같은 타입의 부모 스냅샷 (체인 유지)
    // sendLabelRefs() 건너뜀 (참조 레이블 무시)
```

### Flat 리스 사용 시나리오

```
사용 사례: 이미지 태그 보호

이미지 태그만 보호하고 싶고, 실제 blob은 다른 경로로 관리될 때:
  Lease(flat) → Image "nginx:latest"
  └── 태그만 보호
  └── blob은 별도 GC 대상
```

---

## 13. Collectible Resources

### 외부 리소스 등록

```go
// 소스: core/metadata/db.go (310-328행)
func (m *DB) RegisterCollectibleResource(t gc.ResourceType, c Collector) {
    if t < resourceEnd {
        panic("cannot re-register metadata resource")
    }
    if t >= gc.ResourceMax {
        panic("resource type greater than max")
    }
    m.collectors[t] = c
}
```

### Collector 인터페이스

```go
// 소스: core/metadata/gc.go (138-143행)
type Collector interface {
    StartCollection(context.Context) (CollectionContext, error)
    ReferenceLabel() string
}
```

### CollectionContext 인터페이스

```go
// 소스: core/metadata/gc.go (110-130행)
type CollectionContext interface {
    All(func(gc.Node))                               // 모든 리소스
    Active(namespace string, fn func(gc.Node))       // 활성 리소스
    Leased(namespace, lease string, fn func(gc.Node)) // 리스된 리소스
    Remove(gc.Node)                                  // 삭제 표시
    Cancel() error                                   // 실패 시 정리
    Finish() error                                   // 성공 시 정리
}
```

### 사용 예시: Stream, Mount

```
ResourceStream (streaming 리소스):
  - 클라이언트 스트리밍 세션 추적
  - 활성 세션은 GC에서 보호
  - 세션 종료 후 GC 대상

ResourceMount (마운트 리소스):
  - 활성 마운트 추적
  - 마운트 중인 스냅샷은 GC에서 보호
  - 언마운트 후 GC 대상
```

---

## 14. 왜 이렇게 설계했는가

### Q1: 왜 Reference Counting 대신 Tricolor GC를 사용하는가?

Reference Counting의 문제:
1. **순환 참조**: A → B → A 같은 순환 참조를 처리할 수 없음
2. **원자성**: 참조 카운터 업데이트의 원자성 보장이 어려움
3. **오버헤드**: 모든 참조 변경마다 카운터 업데이트 필요

Tricolor GC의 장점:
1. **순환 참조 처리**: 도달 불가능한 순환 그래프도 삭제
2. **배치 처리**: 주기적으로 한 번에 처리 (성능 예측 가능)
3. **단순성**: 참조 카운터 관리 불필요

### Q2: 왜 Mark와 Sweep을 별도 트랜잭션으로 나누는가?

```
단일 트랜잭션 접근법:
  Update() {
      mark()  // 읽기 + 쓰기 트랜잭션
      sweep() // 같은 트랜잭션
  }
  문제: 대규모 DB에서 트랜잭션이 너무 오래 걸림
  → 모든 쓰기 차단 시간 증가

분리 접근법 (containerd):
  View() { mark() }   // 읽기 트랜잭션 (다른 쓰기 차단 안 함)
  Update() { sweep() } // 쓰기 트랜잭션 (빠름: 삭제만)

  wlock으로 Mark-Sweep 사이 쓰기 방지
  → Mark는 길어도 쓰기 차단 없음
  → Sweep은 짧음 (삭제만)
```

### Q3: 왜 레이블 기반 참조를 사용하는가?

대안: 별도의 참조 테이블

```
references 테이블:
  parent_type | parent_id | child_type | child_id

문제:
  - 별도 테이블 관리 오버헤드
  - 참조 추가/삭제 시 두 곳 업데이트
  - 스키마 변경 시 마이그레이션 필요
```

레이블 기반의 장점:
```
이미지 레이블:
  containerd.io/gc.ref.content.l.0: sha256:abc

장점:
  - 오브젝트와 참조가 같은 위치에 저장
  - 새 참조 타입 추가가 레이블만으로 가능 (스키마 변경 없음)
  - BoltDB 커서의 프리픽스 스캔으로 효율적 조회
  - 확장 가능: 외부 Collector가 자체 레이블 정의 가능
```

### Q4: 왜 GC 스케줄러가 PauseThreshold를 사용하는가?

```
고정 간격 (예: 매 분마다 GC):
  └── GC가 1초 걸리는 작은 DB: 과도한 GC
  └── GC가 30초 걸리는 큰 DB: 성능 저하

PauseThreshold (비율 기반):
  └── GC 시간에 비례하여 간격 자동 조정
  └── 작은 DB: 짧은 GC → 짧은 간격 → 자주 GC (괜찮음)
  └── 큰 DB: 긴 GC → 긴 간격 → 드물게 GC (성능 보호)

  항상 전체 시간의 ~2%만 GC에 사용
```

### Q5: 왜 백엔드 정리를 wlock 보유 중에 시작하는가?

```go
// GarbageCollect() 내부:
m.wlock.Lock()
// ... mark, sweep ...
// 백엔드 정리 시작 (goroutine)
wg.Add(len(m.dirtySS))
for name := range m.dirtySS {
    go func(name string) {
        m.cleanupSnapshotter(ctx, name)
        wg.Done()
    }(name)
}
stats.MetaD = time.Since(t1)
m.wlock.Unlock()

wg.Wait()  // wlock 해제 후 대기
```

wlock 보유 중에 goroutine을 시작하지만, 실제 정리 작업은 wlock 해제 후에도 계속된다. wlock.Unlock() 후 wg.Wait()로 완료를 기다린다. 이렇게 하는 이유:
1. dirtySS 맵은 wlock 보호 하에만 안전하게 읽을 수 있음
2. goroutine 시작은 빠르므로 wlock 보유 시간에 큰 영향 없음
3. 실제 I/O는 wlock 없이 진행되어 쓰기 차단 최소화

---

## 요약

containerd의 GC 시스템은 Tricolor Mark-Sweep을 기반으로 한 정교한 리소스 관리 체계다:

```
핵심 설계 원칙:
  1. Tricolor GC: 도달 가능성 기반 안전한 삭제
  2. 레이블 참조: 유연하고 확장 가능한 참조 추적
  3. Lease 보호: 진행 중인 작업의 리소스 보호
  4. PauseThreshold: GC 부하의 자동 조절
  5. 2단계 잠금: Mark는 읽기만, Sweep은 빠른 삭제만
  6. Back 참조: 부모 업데이트 없이 참조 설정

소스 경로 요약:
  pkg/gc/gc.go                  # Tricolor/ConcurrentMark/Sweep
  plugins/gc/scheduler.go       # gcScheduler (트리거/스케줄링)
  core/metadata/gc.go           # 리소스 타입, 참조 레이블, scan/remove
  core/metadata/db.go           # GarbageCollect(), getMarked(), wlock
```
