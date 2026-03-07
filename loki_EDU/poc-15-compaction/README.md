# PoC #15: 압축(Compaction) - 인덱스/청크 압축 및 보존 정책

## 개요

Loki의 Compactor(`pkg/compactor/`)가 수행하는 인덱스 파일 병합, TTL 기반 보존 정책, Mark-Sweep 삭제 패턴을 시뮬레이션한다.

## 아키텍처

```
Compactor Ring (리더 선출)
     │
     ▼
┌─────────────────────────────────────┐
│         Tables Manager              │
│                                     │
│  ┌─────────┐ ┌─────────┐          │
│  │ Table   │ │ Table   │  ...      │
│  │ Day 1   │ │ Day 2   │          │
│  │         │ │         │          │
│  │ file-1  │ │ file-1  │          │
│  │ file-2  │ │ file-2  │ ── Compact ──→ compacted-file
│  │ file-3  │ │ file-3  │          │
│  └─────────┘ └─────────┘          │
│                                     │
│  ┌──────────────────────────────┐  │
│  │  Retention (Mark-Sweep)      │  │
│  │                              │  │
│  │  Phase 1: Mark expired       │  │
│  │  Phase 2: Sweep (delete)     │  │
│  └──────────────────────────────┘  │
└─────────────────────────────────────┘
```

## Mark-Sweep 삭제 흐름

```
Phase 1: Mark (마킹)
  ┌─────────────┐
  │ 인덱스 스캔  │ → 보존 기간 초과? → 삭제 마커 기록
  └─────────────┘                    (markers 디렉토리)

Phase 2: Sweep (스위핑)
  ┌─────────────┐
  │ 마커 읽기    │ → 청크 삭제 → 인덱스에서 제거 → 마커 삭제
  └─────────────┘
```

## 실행 방법

```bash
go run main.go
```

## 시뮬레이션 내용

1. **Ring 기반 리더 선출**: 단일 Compactor만 실행
2. **인덱스 파일 병합**: 여러 작은 파일 → 하나로 합침, 중복 제거
3. **보존 정책**: 테넌트별 TTL 기반 만료 판단
4. **Mark-Sweep**: 2단계 삭제 (마킹 → 스위핑)
5. **다중 테이블 압축**: 여러 날짜의 테이블을 최신 순으로 압축
6. **통계 요약**: 압축/마킹/삭제 카운트

## Loki 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/compactor/compactor.go` | Compactor 구조체, Ring 리더 선출, loop() |
| `pkg/compactor/table.go` | 테이블별 인덱스 압축 |
| `pkg/compactor/tables_manager.go` | 테이블 관리, 최신 먼저 압축 |
| `pkg/compactor/retention/` | Mark-Sweep 보존 정책 |
| `pkg/compactor/config.go` | 설정 (RetentionEnabled, WorkingDirectory 등) |
