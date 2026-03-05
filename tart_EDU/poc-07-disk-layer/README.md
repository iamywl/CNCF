# PoC-07: LZ4 스타일 청크 분할 병렬 Push/Pull 시뮬레이션

## 개요

tart의 DiskV2 레이어라이저를 Go 표준 라이브러리만으로 재현한다.
디스크 이미지를 청크로 분할하여 압축 후 병렬 Push/Pull하고,
제로 스킵 최적화로 희소 파일의 I/O를 절감하는 핵심 알고리즘을 시뮬레이션한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다. `compress/flate`로 LZ4를 대체하고,
`crypto/sha256`으로 다이제스트를 계산한다.

## 핵심 시뮬레이션 포인트

### 1. 병렬 Push (tart DiskV2.push)
- 디스크 파일을 `layerLimitBytes`(512MB) 청크로 분할
- 각 청크를 LZ4/flate 압축 + SHA256 다이제스트 계산
- goroutine + semaphore(buffered channel)로 동시성 제한
- `blobExists`로 이미 Push된 blob 재전송 방지
- 인덱스 순서로 OCIManifestLayer 반환

### 2. 병렬 Pull (tart DiskV2.pull)
- `truncate(uncompressedDiskSize)`로 디스크 파일 크기 설정
- 각 레이어를 병렬로 Pull + 압축 해제
- 오프셋 기반으로 디스크 파일에 기록
- 비압축 다이제스트로 무결성 검증
- Resumable Pull: 이미 기록된 레이어 다이제스트 비교로 스킵

### 3. 제로 스킵 최적화 (tart zeroSkippingWrite)
- `holeGranularityBytes`(4MB) 단위로 데이터 분할
- 모든 바이트가 0인 청크는 쓰기 건너뜀 (truncate로 이미 0)
- 50%의 제로 영역이 있으면 I/O 50% 절약
- tart 벤치마크: `Data==zeroChunk`가 바이트별 비교보다 33배 빠름

### 4. LocalLayerCache
- 기존 Pull된 이미지의 동일 레이어를 로컬에서 복사
- 1GB 이상 절약 가능할 때만 활용
- deduplicate 모드: 기존 디스크 clone + 차이만 덮어쓰기

## tart 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `Sources/tart/OCI/Layerizer/DiskV2.swift` | 병렬 Push/Pull, 제로 스킵, 청크 분할 |
| `Sources/tart/OCI/Layerizer/Disk.swift` | Disk 프로토콜: push/pull 인터페이스 |
| `Sources/tart/OCI/Manifest.swift` | OCIManifestLayer: 레이어 메타데이터 |
| `Sources/tart/OCI/Digest.swift` | SHA256 다이제스트 계산 |
| `Sources/tart/LocalLayerCache.swift` | 로컬 레이어 캐시 (중복 제거) |

## 아키텍처

```
Push 흐름:
  디스크 파일 (N GB)
  |- 청크 0 (512MB) --compress--> blob --push--> 레지스트리
  |- 청크 1 (512MB) --compress--> blob --push--> 레지스트리  (병렬)
  |- 청크 2 (512MB) --compress--> blob --push--> 레지스트리  (병렬)
  +- ...
  -> OCIManifestLayer[] (인덱스 순서 정렬)

Pull 흐름:
  Manifest -> 레이어 목록
  |- truncate(totalSize)  <- 0으로 초기화
  |- 레이어 0 --pull--> decompress --zeroSkipWrite--> 디스크[오프셋 0]
  |- 레이어 1 --pull--> decompress --zeroSkipWrite--> 디스크[오프셋 512MB]  (병렬)
  +- ...
  -> 제로 청크는 쓰기 건너뜀 (truncate로 이미 0)
```
