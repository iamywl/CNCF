# PoC #17: 컬럼나 포맷 - DataObj 컬럼나 데이터 포맷 인코딩/디코딩

## 개요

Loki의 차세대 스토리지 포맷인 DataObj(`pkg/dataobj/`)의 컬럼나 저장 방식을 시뮬레이션한다. 행 기반 저장 대비 압축률과 쿼리 성능이 크게 향상되는 원리를 설명한다.

## 파일 레이아웃

```
┌──────────────────────────────────┐
│ Header                           │
│  Magic: "DOBJ" (4 bytes)         │
│  Metadata Size (4 bytes)         │
│  Format Version (varint)         │
│  File Metadata (protobuf)        │
│    - Section Types + Dictionary  │
│    - Section Offsets/Lengths     │
├──────────────────────────────────┤
│ Body                             │
│  [Metadata Regions]              │ ← 파일 앞쪽에 배치 (prefetch)
│  [Data Regions]                  │ ← 실제 컬럼 데이터
├──────────────────────────────────┤
│ Tailer                           │
│  Magic: "DOBJ" (4 bytes)         │
└──────────────────────────────────┘
```

## 행 기반 vs 컬럼나 비교

```
[행 기반]  모든 필드가 하나의 행에 묶임
Row 1: {ts=10:00:00, service="api",    level="info",  line="Request received"}
Row 2: {ts=10:00:01, service="api",    level="error", line="Connection timeout"}
Row 3: {ts=10:00:02, service="worker", level="info",  line="Job completed"}

[컬럼나]   같은 타입의 데이터끼리 모음
timestamp: [10:00:00, 10:00:01, 10:00:02]
service:   ["api",    "api",    "worker"]   ← 딕셔너리 인코딩
level:     ["info",   "error",  "info"]     ← 딕셔너리 인코딩
line:      ["Request received", "Connection timeout", "Job completed"]
```

## 실행 방법

```bash
go run main.go
```

## 시뮬레이션 내용

1. **파일 레이아웃 설명**: Header, Body, Tailer 구조
2. **행 기반 vs 컬럼나 비교**: 같은 데이터의 두 가지 저장 방식
3. **딕셔너리 인코딩**: 반복 문자열을 정수 인덱스로 압축
4. **DataObj 인코딩**: 테넌트별 로그를 컬럼나 형식으로 인코딩
5. **컬럼 프루닝**: 쿼리에 필요한 컬럼만 선택적으로 읽기
6. **인코딩 기법**: 딕셔너리, 델타, 비트맵 인코딩 비교

## Loki 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/dataobj/encoder.go` | DataObj 인코더, 딕셔너리, 섹션 관리 |
| `pkg/dataobj/decoder.go` | DataObj 디코더, 메타데이터 파싱 |
| `pkg/dataobj/dataobj.go` | SectionType, Magic 바이트 |
| `pkg/dataobj/internal/dataset/column.go` | 컬럼 데이터 구조 |
| `pkg/dataobj/internal/dataset/value_encoding_*.go` | 인코딩 전략 |
