# PoC-13: containerd 이미지 전송(Transfer) 서비스 시뮬레이션

## 목적

containerd v2의 Transfer 서비스가 이미지 Pull/Push/Import/Export를 통합 처리하는
방식을 시뮬레이션한다. 소스와 대상의 타입 조합에 따라 적절한 전송 로직이 선택되는
패턴과, 레이어 병렬 다운로드 + Progress 콜백 메커니즘을 이해한다.

## 핵심 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| Transferrer | `core/transfer/transfer.go` | Transfer(src, dest) 타입 매칭 |
| ImageResolver | `core/transfer/transfer.go` | Resolve: 이미지 참조 → Descriptor |
| Fetcher | `core/transfer/transfer.go` | Fetch: blob 다운로드 (HTTP GET 모방) |
| Pusher | `core/transfer/transfer.go` | Push: blob 업로드 |
| ProgressFunc | `core/transfer/transfer.go` | 진행률 콜백 |
| localTransferService | `core/transfer/local/transfer.go` | type switch 기반 라우팅 |
| pull 흐름 | `core/transfer/local/pull.go` | Resolve → Fetch → Store |
| Dispatch | `core/images/handlers.go` | 레이어 병렬 다운로드 |

## 소스 참조

| 파일 | 핵심 내용 |
|------|----------|
| `core/transfer/transfer.go` | Transferrer, ImageResolver, Fetcher, Pusher, Progress 인터페이스 |
| `core/transfer/local/transfer.go` | localTransferService 구현, type switch로 src/dest 조합 판별 |
| `core/transfer/local/pull.go` | pull 흐름: Resolve → Fetch → Dispatch → Store |
| `core/images/handlers.go` | Handler, Dispatch (errgroup + semaphore 병렬 처리) |
| `core/images/image.go` | Children(): 매니페스트에서 config + layers 추출 |

## 시뮬레이션 흐름

```
1. 레지스트리에 테스트 이미지 등록 (manifest + config + layers)
2. Transfer.pull() 호출:
   a) Resolve: 이미지 참조 → Descriptor
   b) Fetcher 생성
   c) Children: 매니페스트에서 레이어 목록 추출
   d) Dispatch: 세마포어로 동시성 제한하며 병렬 다운로드
   e) Content Store에 blob 저장
   f) ImageStorer.Store: 이미지 레코드 저장
3. Progress 콜백으로 진행 상황 출력
4. 재 Pull: content-addressable이므로 이미 존재하는 blob 스킵
5. Transfer 타입 매칭 테이블 출력
```

## 실행 방법

```bash
cd containerd_EDU/poc-13-transfer
go run main.go
```

## 예상 출력

```
=== containerd Transfer Service 시뮬레이션 ===

[레지스트리] 이미지 등록: docker.io/library/nginx:1.25
  Config:  {application/vnd.oci.image.config.v1+json sha256:xxxxxxxx 2048}
  Layer 1: {application/vnd.oci.image.layer.v1.tar+gzip sha256:xxxxxxxx ...} (50MB)
  Layer 2: {application/vnd.oci.image.layer.v1.tar+gzip sha256:xxxxxxxx ...} (30MB)
  Layer 3: {application/vnd.oci.image.layer.v1.tar+gzip sha256:xxxxxxxx ...} (10MB)

[Pull 시작] docker.io/library/nginx:1.25
------------------------------------------------------------
  [Resolving from docker.io/library/nginx:1.25]
  [pulling image content] docker.io/library/nginx:1.25
  [downloading] sha256:xxxxxxxx: application/vnd.oci.image.config.v1+json...
  [downloading] sha256:xxxxxxxx: application/vnd.oci.image.layer.v1.tar+gzip...
  ...
  [done] sha256:xxxxxxxx: 2048 bytes 완료
  [done] sha256:xxxxxxxx: 52428800 bytes 완료
  ...
  [saved] docker.io/library/image@sha256:xxxxxxxx
  [Completed pull from docker.io/library/nginx:1.25]
------------------------------------------------------------
[Pull 완료] 소요 시간: ~150ms

[재 Pull 시도] — 이미 존재하는 이미지는 스킵
  [exists] sha256:xxxxxxxx: 이미 존재 (스킵)
  ...
[재 Pull 완료] 소요 시간: ~0ms (이미 존재하여 빠름)

[Transfer 타입 매칭 — type switch 기반]
  src \ dest       | ImageStorer     | ImagePusher     | ImageExporter
  -----------------+-----------------+-----------------+----------------
  ImageFetcher     | pull (Pull)     |                 |
  ImageGetter      | tag (Tag)       | push (Push)     | export (Save)
  ImageImporter    | import (Load)   |                 |
```

## 핵심 포인트

- **통합 인터페이스**: Transfer 서비스는 Pull/Push/Tag/Import/Export를 하나의 `Transfer(src, dest)` 메서드로 통합한다. 소스와 대상 타입의 조합을 type switch로 판별하여 적절한 내부 메서드를 호출한다.
- **병렬 다운로드**: `images.Dispatch()`는 `errgroup`과 `semaphore.Weighted`를 사용하여 동시 다운로드 수를 제한하면서 레이어를 병렬로 가져온다. `MaxConcurrentDownloads` 설정으로 제어한다.
- **Content-Addressable**: Content Store는 digest 기반이므로 이미 존재하는 blob은 재다운로드하지 않는다. 같은 레이어를 공유하는 이미지 간에 저장 공간을 절약한다.
- **Progress 추적**: `ProgressFunc` 콜백으로 다운로드 진행 상황을 실시간 보고한다. CLI, GUI 등 다양한 UI에서 활용할 수 있다.
