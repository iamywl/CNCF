# PoC-12: Lease 기반 리소스 수명 관리 시뮬레이션

## 개요

containerd의 Lease 시스템을 시뮬레이션한다. Lease는 리소스(Content, Snapshot, Image)를 GC에서 보호하는 참조 그룹으로, 이미지 pull 같은 다단계 작업 중 중간 리소스가 GC되는 것을 방지한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| Lease Manager | `core/metadata/leases.go` (Create, Delete, List) | 메모리 기반 Manager 구현 |
| Resource 관리 | `core/metadata/leases.go` (AddResource, DeleteResource) | Lease → Resource 참조 관리 |
| Manager 인터페이스 | `core/leases/lease.go` (Manager interface) | 동일 인터페이스 |
| GC 통합 | `core/metadata/gc.go` (scanRoots에서 Lease 처리) | Lease를 root로 사용하는 GC |
| Lease 만료 | `core/metadata/gc.go` (gc.expire 레이블) | 만료 시 root 제외 |
| Flat Lease | `core/metadata/gc.go` (gc.flat 레이블) | 직접 참조만 보호 |

## 핵심 개념

### Lease 생명주기
```
이미지 Pull 흐름:

  1. Pull 시작 → Lease 생성
     lease = Create(ctx, WithID("pull-xxx"))
     ctx = WithLease(ctx, lease.ID)

  2. 매니페스트 다운로드 → Lease에 콘텐트 추가
     AddResource(ctx, lease, {ID: "sha256:manifest", Type: "content"})

  3. 레이어 다운로드 → 각 레이어 추가
     AddResource(ctx, lease, {ID: "sha256:layer1", Type: "content"})

  4. 스냅샷 생성 → 스냅샷 추가
     AddResource(ctx, lease, {ID: "snap-1", Type: "snapshots/overlayfs"})

  5. 이미지 등록 (이미지가 root가 됨)

  6. Pull 완료 → Lease 삭제
     Delete(ctx, lease)  → dirty+1 → GC 알림
```

### GC에서 Lease 처리
```
scanRoots():
  v1/{ns}/leases 순회
    ├── gc.expire 확인 → 만료 시 skip (root 제외)
    ├── gc.flat 확인 → flat이면 resourceContentFlat 타입 사용
    ├── content/ 순회 → root로 등록
    ├── snapshots/{snapshotter}/ 순회 → root로 등록
    ├── images/ 순회 → root로 등록
    └── ingests/ 순회 → root로 등록

references() — Flat 타입 처리:
  ResourceSnapshot vs resourceSnapshotFlat:
    일반: parent → 재귀적 참조 추적
    Flat: parent 참조 무시 (직접 참조만 보호)
```

### BoltDB 버킷 구조
```
v1/{namespace}/leases/
└── {lease_id}/
    ├── createdat: <binary time>
    ├── labels/
    │   ├── containerd.io/gc.expire: "2024-01-01T00:00:00Z"
    │   └── containerd.io/gc.flat: ""
    ├── content/
    │   └── sha256:abc123: <nil>
    ├── snapshots/
    │   └── overlayfs/
    │       └── snap-1: <nil>
    ├── images/
    │   └── nginx:latest: <nil>
    └── ingests/
        └── upload-ref-1: <nil>
```

### Flat Lease vs Normal Lease
```
Normal Lease:
  Lease → Image → Content(manifest) → Content(layer1)
  전체 참조 트리가 보호됨 (재귀적 추적)

Flat Lease:
  Lease → Image → Content(manifest)   ← 여기까지만 보호
  Content(layer1)은 보호하지 않음 (참조 추적 중단)

실제 구현: resourceContentFlat = ResourceContent | 0x20
  references()에서 Flat 타입이면 자식 참조를 무시
```

## 소스 참조

| 파일 | 핵심 구조체/함수 |
|------|----------------|
| `core/leases/lease.go` | `Lease`, `Resource`, `Manager` 인터페이스 |
| `core/metadata/leases.go` | `leaseManager` — Create, Delete, AddResource, DeleteResource, ListResources |
| `core/metadata/gc.go` | `scanRoots()` — Lease 버킷 순회, gc.expire/gc.flat 처리 |
| `core/metadata/gc.go` | `labelGCExpire`, `labelGCFlat`, `resourceContentFlat` |
| `core/metadata/buckets.go` | Lease 버킷 스키마 (leases/{id}/content, snapshots 등) |
| `core/leases/context.go` | `WithLease()`, `FromContext()` |

## 실행

```bash
go run main.go
```

## 예상 출력

```
=== containerd Lease 기반 리소스 수명 관리 시뮬레이션 ===

--- 데모 1: Lease 기본 CRUD ---
Lease 생성: id=pull-nginx, err=<nil>
Lease 생성: id=build-myapp, err=<nil>
중복 Lease 생성: err=lease "pull-nginx": already exists

--- 데모 2: 리소스 참조 관리 ---
pull-nginx 리소스 (5개):
  content/sha256:manifest
  content/sha256:layer1
  content/sha256:layer2
  snapshots/overlayfs/snap-1
  images/nginx:latest

--- 데모 3: Lease가 GC에서 리소스 보호 ---
  Lease "pull-nginx": 5개 리소스 보호
  Lease "build-myapp": 2개 리소스 보호
보호된 리소스: 7개
삭제된 리소스: 3개 (고아 리소스)

--- 데모 4: Lease 삭제 후 GC ---
  pull-nginx 삭제 후: 이전에 보호되던 리소스도 GC 대상

--- 데모 5: Lease 만료 (gc.expire) ---
  만료된 Lease의 리소스는 보호되지 않음

--- 데모 6: Flat Lease ---
  Flat Lease: 직접 참조한 리소스만 보호
  자식 리소스 (flat-layer1, flat-layer2)는 GC 대상

--- 데모 7: 이미지 Pull 시나리오 ---
  Pull 중 Lease가 중간 리소스 보호
  Pull 완료 후 Lease 삭제 → 이미지 root가 보호
```
