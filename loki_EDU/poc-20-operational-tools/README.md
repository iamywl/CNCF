# PoC-20: 운영 도구 시뮬레이션

## 개요

Loki의 Chunks Inspector(`cmd/chunks-inspect/`), Migration Tool(`cmd/migrate/`), Loki Tool(`cmd/lokitool/`)의 핵심 개념을 시뮬레이션한다.

## 시뮬레이션 항목

| 개념 | 소스 참조 | 시뮬레이션 방법 |
|------|----------|---------------|
| 청크 바이너리 포맷 | `cmd/chunks-inspect/loki.go` | Varint/BigEndian 인코딩 구현 |
| CRC32 체크섬 | `cmd/chunks-inspect/loki.go` | Castagnoli 테이블 체크섬 |
| 청크 디코딩/검사 | `cmd/chunks-inspect/main.go` | 블록 메타데이터 파싱 |
| 시간 범위 분할 | `cmd/migrate/main.go` | calcSyncRanges() 구현 |
| 병렬 마이그레이션 | `cmd/migrate/main.go` | ChunkMover + 워커 풀 |
| 규칙 관리 | `cmd/lokitool/main.go` | sync/diff 커맨드 |

## 실행

```bash
go run main.go
```

## 핵심 출력

- 청크 바이너리 인코딩 및 디코딩
- 체크섬 무결성 검증 (정상/변조 비교)
- 시간 범위 분할 기반 병렬 마이그레이션
- 테넌트 변경 마이그레이션
- 규칙 동기화 및 차이 비교
