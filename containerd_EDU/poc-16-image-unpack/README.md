# PoC-16: containerd 이미지 매니페스트 파싱 + 레이어 언팩 시뮬레이션

## 목적

OCI 이미지의 계층 구조(Index → Manifest → Config + Layers)를 파싱하고,
레이어를 Snapshotter를 통해 파일시스템으로 언팩하는 과정을 시뮬레이션한다.
ChainID 계산, 플랫폼 선택, 미디어 타입 판별 등 containerd 이미지 처리의 핵심 로직을 이해한다.

## 핵심 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| OCI Descriptor | image-spec `v1.Descriptor` | mediaType, digest, size |
| Image Index | `core/images/image.go` Children() | 멀티 아키텍처 매니페스트 목록 |
| Image Manifest | `core/images/image.go` Manifest() | Config + Layers 디스크립터 |
| Image Config | `core/images/image.go` RootFS() | DiffIDs, 히스토리, 실행 설정 |
| ChainID | `image-spec/identity` | sha256(parent + " " + diffID) |
| 미디어 타입 | `core/images/mediatypes.go` | IsManifestType, IsIndexType 등 |
| 레이어 언팩 | `core/unpack/unpacker.go` | Prepare → Apply → Commit |

## 소스 참조

| 파일 | 핵심 내용 |
|------|----------|
| `core/images/image.go` | Manifest(), Children(), RootFS(), Config(), Platforms(), Check() |
| `core/images/mediatypes.go` | IsManifestType, IsIndexType, IsLayerType, IsConfigType, 미디어 타입 상수 |
| `core/images/handlers.go` | Walk, Dispatch, ChildrenHandler, FilterPlatforms, LimitManifests |
| `core/images/diffid.go` | GetDiffID: 압축 해제 후 sha256 계산 |
| `core/unpack/unpacker.go` | Unpacker: Snapshot.Prepare → Applier.Apply → Snapshot.Commit |
| `image-spec/identity` | ChainID 계산 알고리즘 |

## 시뮬레이션 흐름

```
1. Image Index 파싱: 멀티 아키텍처 (linux/amd64, linux/arm64)
2. 플랫폼 선택: linux/amd64 매니페스트 선택
3. Manifest 파싱: Children() → Config + Layers
4. Config 파싱: DiffIDs, 히스토리, Cmd 추출
5. ChainID 계산:
   - ChainID[0] = DiffID[0]
   - ChainID[n] = sha256(ChainID[n-1] + " " + DiffID[n])
6. 레이어 언팩:
   - Layer 0: Prepare("") → Apply(tar) → Commit(ChainID[0])
   - Layer 1: Prepare(ChainID[0]) → Apply → Commit(ChainID[1])
   - Layer 2: Prepare(ChainID[1]) → Apply → Commit(ChainID[2])
7. 스냅샷 체인 확인 (overlay 계층 구조)
8. 미디어 타입 판별 테이블
```

## 실행 방법

```bash
cd containerd_EDU/poc-16-image-unpack
go run main.go
```

## 예상 출력

```
=== containerd 이미지 매니페스트 파싱 + 레이어 언팩 시뮬레이션 ===

[1] Image Index 파싱 (멀티 아키텍처)
  MediaType: application/vnd.oci.image.index.v1+json
  플랫폼 수: 2
  [0] Platform: linux/amd64, Digest: sha256:xxxxxxxx...
  [1] Platform: linux/arm64, Digest: sha256:xxxxxxxx...

[2] 플랫폼 선택: linux/amd64
  선택된 매니페스트: sha256:xxxxxxxx...

[3] Manifest 파싱 — Children()
  Children 수: 4 (Config 1 + Layers 3)
  [0] Config MediaType=application/vnd.oci.image.config.v1+json
  [1] Layer  MediaType=application/vnd.oci.image.layer.v1.tar+gzip
  ...

[4] Config 파싱 → DiffIDs + 히스토리
  Architecture: amd64
  OS: linux
  DiffIDs (3개):
    [0] sha256:xxxxxxxx...
    ...

[5] ChainID 계산
  ChainID[0]: sha256:xxxxxxxx... (= DiffID, 첫 레이어)
  ChainID[1]: sha256:xxxxxxxx...
  ChainID[2]: sha256:xxxxxxxx...

[6] 레이어 언팩 (Snapshot.Prepare → Apply → Commit)
  레이어 0 언팩:
    추출된 파일: bin/sh, etc/os-release, lib/libc.so
  레이어 1 언팩:
    추출된 파일: usr/sbin/nginx, etc/nginx/nginx.conf
  레이어 2 언팩:
    추출된 파일: usr/share/nginx/html/index.html, etc/nginx/conf.d/default.conf

[8] 미디어 타입 판별
  MediaType                     Index  Manifest  Config  Layer
  ----------------------------------------------------------
  ...oci.image.index.v1+json    true   false     false   false
  ...oci.image.manifest.v1+json false  true      false   false
  ...
```

## 핵심 포인트

- **계층적 콘텐츠 모델**: OCI 이미지는 Index(선택적) → Manifest → Config + Layers의 계층 구조이다. `Children()` 함수가 미디어 타입에 따라 하위 디스크립터를 반환한다.
- **ChainID 누적 해시**: 각 레이어의 ChainID는 이전 레이어의 ChainID와 현재 DiffID의 조합으로 계산된다. 이를 통해 Snapshotter가 부모-자식 관계를 결정한다. 같은 레이어 스택을 가진 이미지는 스냅샷을 공유할 수 있다.
- **Prepare-Apply-Commit**: 레이어 언팩은 3단계로 진행된다. Prepare로 쓰기 가능한 스냅샷을 만들고, Apply로 tar를 추출하고, Commit으로 읽기 전용으로 확정한다. overlay 파일시스템의 lower/upper 개념과 대응된다.
- **멀티 아키텍처**: Image Index를 통해 하나의 이미지 태그가 여러 플랫폼(amd64, arm64 등)을 지원한다. `platform.Match()`로 현재 호스트에 맞는 매니페스트를 선택한다.
- **미디어 타입 기반 분기**: `IsManifestType()`, `IsIndexType()`, `IsLayerType()`, `IsConfigType()` 함수로 Descriptor의 종류를 판별하여 처리 방식을 결정한다.
