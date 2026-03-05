# PoC-09: BoltDB 메타데이터 스토어 시뮬레이션

## 개요

containerd의 BoltDB 기반 메타데이터 스토어를 메모리 KV 스토어로 시뮬레이션한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| DB 구조체 | `core/metadata/db.go` (DB, wlock, dirty, mutationCallbacks) | RWMutex + atomic 카운터 + 콜백 목록 |
| 버킷 스키마 | `core/metadata/buckets.go` (v1/namespace/object/key) | 중첩 map 기반 Bucket 구조체 |
| View/Update | `core/metadata/db.go` (View, Update) | RLock 기반 트랜잭션 시뮬레이션 |
| 네임스페이스 격리 | `pkg/namespaces/context.go` (WithNamespace, NamespaceRequired) | context.WithValue 기반 |
| GC wlock | `core/metadata/db.go` (GarbageCollect — wlock.Lock) | GC 중 쓰기 차단 데모 |
| 메타데이터 플러그인 | `plugins/metadata/plugin.go` (BoltConfig, NewDB) | 초기화 흐름 재현 |

## 핵심 개념

### BoltDB 버킷 스키마
```
v1/                                    ← 스키마 버전
  ├── version: 4                       ← DB 버전
  └── {namespace}/                     ← 네임스페이스 격리
      ├── images/{name}/               ← 이미지 메타데이터
      │   ├── createdat
      │   ├── target/digest
      │   └── labels/{key}: {value}
      ├── containers/{id}/             ← 컨테이너 메타데이터
      ├── snapshots/{snapshotter}/{key}/
      ├── content/blob/{digest}/
      └── leases/{id}/
```

### GC wlock 메커니즘
```
Update()              GarbageCollect()
  │                       │
  ├── wlock.RLock()       ├── wlock.Lock()     ← 쓰기 차단
  ├── 트랜잭션 실행         ├── Mark (읽기만)
  ├── mutationCallbacks    ├── Sweep (삭제)
  └── wlock.RUnlock()     ├── dirty.Store(0)
                          └── wlock.Unlock()   ← 쓰기 재허용
```

### mutationCallbacks 흐름
```
Update 완료
  → dirty 체크 (삭제 발생 여부)
  → mutationCallbacks 순회 호출
  → GC 스케줄러가 mutation/deletion 카운트 갱신
  → 임계값 초과 시 GC 스케줄
```

## 소스 참조

| 파일 | 핵심 구조체/함수 |
|------|----------------|
| `core/metadata/db.go` | `DB`, `View()`, `Update()`, `GarbageCollect()`, `RegisterMutationCallback()` |
| `core/metadata/buckets.go` | 버킷 스키마 주석, `getBucket()`, `createBucketIfNotExists()` |
| `core/metadata/gc.go` | `ResourceType` 정의, `scanRoots()`, `references()`, `scanAll()` |
| `plugins/metadata/plugin.go` | `BoltConfig`, `SharingPolicy`, 플러그인 초기화 |
| `pkg/namespaces/context.go` | `WithNamespace()`, `NamespaceRequired()` |

## 실행

```bash
go run main.go
```

## 예상 출력

```
=== containerd BoltDB 메타데이터 스토어 시뮬레이션 ===

--- 데모 1: DB 초기화 및 버킷 스키마 ---
DB 생성 완료 (스키마 v1, 버전 4)

네임스페이스 없는 context: namespace is required
네임스페이스 설정 후: "default"

--- 데모 2: 네임스페이스 격리된 이미지 CRUD ---
default NS 이미지 생성: err=<nil>
default NS 이미지 생성: err=<nil>
production NS 이미지 생성: err=<nil>

default NS 이미지: [docker.io/library/nginx:latest docker.io/library/redis:7]
production NS 이미지: [gcr.io/myapp:v1.0]
=> 네임스페이스 간 이미지가 격리됨

--- 데모 3: View(읽기) / Update(쓰기) 트랜잭션 ---
View — 이미지 조회: name=docker.io/library/nginx:latest, target=sha256:abc123
         labels=map[containerd.io/gc.ref.content:sha256:abc123]
View — production에서 default 이미지 조회: image "docker.io/library/nginx:latest" not found
=> production NS에서 default NS의 이미지에 접근 불가

--- 데모 5: mutationCallback — GC 스케줄러 연동 ---
이미지 삭제 (Update 트랜잭션):
  [콜백] mutation #1, dirty=true
이미지 추가 (Update 트랜잭션):
  [콜백] mutation #2, dirty=true

--- 데모 6: GC wlock — 쓰기 차단 시뮬레이션 ---
[GC] wlock.Lock() 획득 — 쓰기 트랜잭션 차단
[Writer] Update 시도 (wlock.RLock 대기)...
[GC] Mark/Sweep 진행...
[GC] wlock.Unlock() — 쓰기 트랜잭션 재허용
[Writer] Update 완료 (GC 완료 후 진행됨)
```
