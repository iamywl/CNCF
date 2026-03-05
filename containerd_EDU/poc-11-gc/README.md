# PoC-11: Tricolor GC 알고리즘 시뮬레이션

## 개요

containerd의 Tricolor Mark-Sweep GC 알고리즘, 리소스 참조 추적, GC 스케줄러를 시뮬레이션한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| Tricolor GC | `pkg/gc/gc.go` (Tricolor, Sweep) | 동일 알고리즘 Go 구현 |
| 리소스 타입 | `core/metadata/gc.go` (ResourceType iota) | Content, Snapshot, Container, Image, Lease 등 |
| Root 수집 | `core/metadata/gc.go` (scanRoots) | Image, Container, Lease를 root로 수집 |
| 참조 추적 | `core/metadata/gc.go` (references) | gc.ref.* 레이블 기반 참조 해석 |
| GC 스케줄러 | `plugins/gc/scheduler.go` (gcScheduler) | mutation/deletion 카운트 기반 트리거 |
| wlock | `core/metadata/db.go` (GarbageCollect) | RWMutex로 GC 중 쓰기 차단 |

## 핵심 알고리즘

### Tricolor Mark-Sweep
```
초기 상태:
  White = {모든 리소스}     ← 수거 후보
  Gray  = {root 노드}     ← 탐색 대기 (스택)
  Black = {}              ← 도달 가능 확인됨

반복 (gray 스택이 빌 때까지):
  1. gray에서 노드 꺼냄 (depth-first: 마지막 원소)
  2. 해당 노드의 참조를 조회 (gc.ref.* 레이블)
  3. 아직 안 본 참조를 gray에 추가
  4. 현재 노드를 black으로 이동

Sweep:
  White에 남은 노드 = 도달 불가 → 삭제
```

### 리소스 참조 그래프
```
  Image ─── target.digest ──→ Content (manifest)
    │                            │
    │                            ├── gc.ref.content.0 → Content (layer1)
    │                            └── gc.ref.content.1 → Content (layer2)
    │
    └── gc.ref.snapshot.overlayfs → Snapshot
                                      │
                                      └── parent → Snapshot (parent)

  Container ─── snapshotter/snapshotKey ──→ Snapshot

  Lease ─── lease.resource.content/* ──→ Content
        ─── lease.resource.snapshot/* ──→ Snapshot
```

### Root 선정 규칙
```
리소스 타입        Root 조건
Image             gc.expire 없음 또는 미만료
Container         항상 root
Lease             항상 root (만료 체크는 별도)
Content/Snapshot  gc.root 레이블이 있을 때만
```

### GC 스케줄러 동작
```
DB.Update() 완료
  → mutationCallback(dirty)
  → mutations++, if dirty: deletions++
  → 임계값 확인:
     mutations >= mutationThreshold (기본 100) → 다음 스케줄된 GC에서 실행
     deletions >= deletionThreshold (기본 0)   → 즉시 GC 스케줄
  → GC 실행: wlock.Lock() → Mark → Sweep → dirty.Store(0) → wlock.Unlock()
```

## 소스 참조

| 파일 | 핵심 구조체/함수 |
|------|----------------|
| `pkg/gc/gc.go` | `Tricolor()` — gray 스택 기반 DFS, `Sweep()` — 미마크 노드 삭제 |
| `core/metadata/gc.go` | `ResourceType` 상수, `scanRoots()`, `references()`, `scanAll()`, `remove()` |
| `core/metadata/db.go` | `GarbageCollect()` — wlock + getMarked + sweep, `dirty` atomic |
| `plugins/gc/scheduler.go` | `gcScheduler`, `mutationCallback()`, `run()` 루프 |

## 실행

```bash
go run main.go
```

## 예상 출력

```
=== containerd Tricolor GC 알고리즘 시뮬레이션 ===

--- 데모 1: 기본 Tricolor Mark-Sweep ---
리소스 그래프:
  Image:nginx → Content:manifest → Content:layer1, Content:layer2
  Container:c1 → Snapshot:overlayfs/snap-1
  Content:sha256:orphan (고아)

  [GC] Mark 단계:
  [GC]   Root 노드: Image:nginx, Container:c1
  [GC]   도달 가능 노드 7개
  [GC] Sweep 단계:
  [GC]   삭제: Content:sha256:orphan
  [GC]   삭제: Snapshot:overlayfs/snap-orphan

--- 데모 2: 이미지 만료 (gc.expire) ---
  만료된 이미지와 그 콘텐트가 GC됨

--- 데모 3: gc.root 레이블로 콘텐트 보호 ---
  gc.root=true인 콘텐트는 root로 취급되어 보호됨

--- 데모 4: GC 스케줄러 (mutation 기반 트리거) ---
  mutation 5회 후 GC 자동 트리거

--- 데모 5: Tricolor 색상 변화 추적 ---
  Step 1: root를 Gray에서 꺼냄 → 참조: A, B → Black으로 이동
  Step 2: B를 Gray에서 꺼냄 → 참조: (없음) → Black으로 이동
  Step 3: A를 Gray에서 꺼냄 → 참조: C → Black으로 이동
  Step 4: C를 Gray에서 꺼냄 → 참조: (없음) → Black으로 이동
  White (수거 대상): {sha256:D}
```
