# PoC-03: Write-Ahead Log (WAL)

## 개요

Prometheus TSDB의 **Write-Ahead Log (WAL)**를 단순화하여 구현한 PoC이다.

WAL은 Prometheus가 수집한 메트릭 데이터의 **내구성(durability)**을 보장하는 핵심 메커니즘이다. 인메모리 Head 블록에 데이터를 쓰기 전에 먼저 WAL에 기록함으로써, 프로세스가 비정상 종료되어도 데이터를 복구할 수 있다.

## WAL이 필요한 이유

```
[Scrape] → [WAL 기록] → [Head 블록(인메모리)]
                ↓
          [디스크 파일]
                ↓
          [재시작 시 복구]
```

Prometheus의 Head 블록은 최근 2시간 분량의 시계열 데이터를 메모리에 보관한다. 메모리에만 존재하는 데이터는 프로세스 크래시 시 유실되므로, 모든 쓰기 연산은 먼저 WAL에 순차적으로 기록된다.

## 핵심 개념

### 1. 레코드 포맷

이 PoC의 레코드 구조:

```
+----------+------------------+----------------+-----------+
| Type(1B) | Length(4B, BE)   | Data(N bytes)  | CRC32(4B) |
+----------+------------------+----------------+-----------+
```

- **Type**: 레코드 종류 (1=Series, 2=Samples)
- **Length**: 데이터 길이 (Big-Endian 4바이트)
- **Data**: 실제 레코드 데이터
- **CRC32**: Castagnoli 다항식 기반 체크섬 (데이터 무결성 검증)

실제 Prometheus WAL은 32KB 페이지 기반으로 레코드를 분할하여 기록한다:
- 헤더: `[type(1B)] [length(2B)] [CRC32(4B)]` (7바이트)
- 레코드가 페이지 경계를 넘으면 First/Middle/Last로 분할

### 2. 레코드 타입

| 타입 | 값 | 내용 |
|------|------|------|
| Series | 1 | 시리즈 정의 (ref ID + 레이블 셋) |
| Samples | 2 | 샘플 데이터 (ref ID + 타임스탬프 + 값) |

실제 Prometheus에는 Tombstones(3), Exemplars(4), Metadata(6), HistogramSamples(7) 등 추가 타입이 있다.

### 3. 세그먼트 로테이션

```
wal/
├── 00000000    (4KB - 가득 참)
├── 00000001    (4KB - 가득 참)
├── 00000002    (4KB - 가득 참)
└── 00000003    (현재 활성 세그먼트)
```

- 세그먼트가 최대 크기에 도달하면 새 세그먼트 생성
- 실제 Prometheus: 기본 128MB (`DefaultSegmentSize`)
- 이 PoC: 데모를 위해 4KB로 설정
- 오래된 세그먼트는 체크포인트 후 삭제 가능

### 4. 크래시 복구 과정

```
1. Prometheus 프로세스 시작
2. WAL 디렉토리 스캔 → 세그먼트 목록 확인
3. 모든 세그먼트를 순서대로 읽기 (Replay)
4. 각 레코드의 CRC32 검증
5. Series 레코드 → Head 블록에 시리즈 등록
6. Sample 레코드 → Head 블록에 샘플 추가
7. 손상된 레코드 발견 시 → Repair() 호출
8. 복구 완료 후 새 세그먼트에 이어쓰기
```

실제 코드 경로: `tsdb/head_wal.go` → `Replay()` 함수

### 5. CRC32 체크섬

- Castagnoli 다항식 (`crc32.Castagnoli`) 사용
- 하드웨어 가속 가능 (SSE 4.2 CRC32C 명령어)
- 부분 쓰기, 비트 오류 등을 감지
- 실제 코드: `castagnoliTable = crc32.MakeTable(crc32.Castagnoli)`

## 실행 방법

```bash
cd prometheus_EDU/poc-03-wal
go run main.go
```

## 실행 결과 예시

```
Prometheus WAL (Write-Ahead Log) PoC
기반: tsdb/wlog/wlog.go, tsdb/record/record.go

======================================================================
  시나리오 1: WAL 쓰기 + 세그먼트 로테이션
======================================================================

[1] 10개 시리즈 레코드 기록 중...
  시리즈 레코드 10개 기록 완료

[2] 1500개 샘플 레코드 기록 중...
  샘플 레코드 1500개 기록 완료

WAL 통계:
  세그먼트 생성: 13개
  세그먼트 로테이션: 12회
  전체 레코드: 1510개 (시리즈: 10, 샘플: 1500)
  전체 바이트: 50.45 KB

[3] 세그먼트 파일 목록:
  세그먼트 00000000: 4.01 KB
  세그먼트 00000001: 3.99 KB
  ...

======================================================================
  시나리오 2: WAL 읽기 + CRC 검증 + 데이터 복원
======================================================================
  [OK] 시리즈 수 일치: 10 == 10
  [OK] 샘플 수 일치: 1500 == 1500
  [OK] CRC 오류 없음 — 모든 레코드 무결

======================================================================
  시나리오 3: 크래시 복구 시뮬레이션
======================================================================
  [OK] 크래시 복구 성공! 모든 데이터가 무결하게 복원됨
  [OK] 크래시 복구 + 재개 성공! 데이터 무결성 확인

======================================================================
  시나리오 4: CRC 손상 감지 (의도적 데이터 훼손)
======================================================================
  [OK] CRC32 체크섬으로 데이터 손상 감지 성공!
```

## 실제 Prometheus 코드 참조

| 이 PoC | 실제 Prometheus 코드 |
|--------|---------------------|
| `WAL` struct | `tsdb/wlog/wlog.go` → `WL` struct |
| `WAL.Log()` | `wlog.WL.Log()` → `wlog.WL.log()` |
| `WALReader.ReadAll()` | `tsdb/wlog/reader.go` → `Reader.Next()` |
| `rotateSegment()` | `wlog.WL.nextSegment()` |
| `RecordTypeSeries` | `tsdb/record/record.go` → `Series Type = 1` |
| `RecordTypeSamples` | `tsdb/record/record.go` → `Samples Type = 2` |
| CRC32 Castagnoli | `crc32.MakeTable(crc32.Castagnoli)` |
| 세그먼트 파일명 | `SegmentName()` → `fmt.Sprintf("%08d", i)` |
| 크래시 복구 | `tsdb/head_wal.go` → `Replay()` |

## 이 PoC에서 생략한 것

| 항목 | 실제 Prometheus | 이 PoC |
|------|----------------|--------|
| 페이지 버퍼링 | 32KB 페이지 단위 배치 쓰기 | 직접 파일 쓰기 |
| 레코드 분할 | First/Middle/Last 분할 | 레코드 단위 쓰기 |
| 체크포인트 | checkpoint.go로 WAL 압축 | 미구현 |
| 압축 | Snappy/Zstd 선택 가능 | 미구현 |
| 동시성 | sync.RWMutex + goroutine | 단일 스레드 |
| LiveReader | 실시간 WAL tail 읽기 | 미구현 |
| Watcher | WAL 변경 감지 (remote write) | 미구현 |
