# PoC-15: VM 아카이브(내보내기/가져오기) 시뮬레이션

## 개요

Tart의 `VMDirectory+Archive.swift`에서 구현된 VM 내보내기/가져오기 아카이브 파이프라인을 Go로 재현한다. Apple Archive의 LZFSE 스트리밍 압축을 flate로 시뮬레이션하고, 메타데이터 보존과 무결성 검증을 구현한다.

## Tart 소스코드 매핑

| Tart 소스 | PoC 대응 | 설명 |
|-----------|---------|------|
| `VMDirectory+Archive.swift` — `exportToArchive()` | `ExportToArchive()` | VM 번들을 압축 아카이브로 내보내기 |
| `VMDirectory+Archive.swift` — `importFromArchive()` | `ImportFromArchive()` | 아카이브에서 VM 번들 복원 |
| `ArchiveByteStream.fileStream()` | `os.Create/Open` | 파일 스트림 |
| `ArchiveByteStream.compressionStream(using: .lzfse)` | `flate.NewWriter()` | LZFSE 압축 시뮬레이션 |
| `ArchiveStream.encodeStream()` | `tar.NewWriter()` | 인코딩 스트림 |
| `ArchiveByteStream.decompressionStream()` | `flate.NewReader()` | 압축 해제 |
| `ArchiveStream.decodeStream()` | `tar.NewReader()` | 디코딩 스트림 |
| `ArchiveStream.extractStream(flags: [.ignoreOperationNotPermitted])` | 추출 로직 | 권한 에러 무시 추출 |
| `ArchiveHeader.FieldKeySet("TYP,PAT,...")` | 데모 7 테이블 | 메타데이터 필드 정의 |

## 핵심 개념

### 1. 스트리밍 파이프라인

```
내보내기: VM Directory -> EncodeStream -> CompressionStream(LZFSE) -> FileStream(.aar)
가져오기: FileStream(.aar) -> DecompressionStream -> DecodeStream -> ExtractStream -> VM Directory
```

### 2. LZFSE 압축

Apple이 iOS 9/macOS 10.11에서 도입한 압축 알고리즘. zlib과 유사한 압축률을 제공하면서 더 빠른 압축/해제 속도를 가진다. Tart는 Apple 문서에서 권장하는 대로 LZFSE를 사용한다.

### 3. 메타데이터 필드

```
TYP: 파일 타입  PAT: 경로  LNK: 심링크  DEV: 디바이스
DAT: 데이터    UID: 소유자  GID: 그룹    MOD: 권한
FLG: 플래그    MTM: 수정시간  BTM: 생성시간  CTM: 변경시간
```

## 실행 방법

```bash
cd poc-15-archive
go run main.go
```

## 학습 포인트

1. **스트리밍 파이프라인**: 메모리 효율적인 다단계 스트림 처리
2. **압축 알고리즘 선택**: 플랫폼 특화 알고리즘 (LZFSE) 활용
3. **메타데이터 보존**: 12개 필드로 파일 속성 완전 보존
4. **에러 전파**: 각 스트림 단계별 Errno 기반 에러 처리
5. **무결성 검증**: SHA-256 체크섬으로 원본/복원 데이터 일치 확인
