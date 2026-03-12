# PoC-03: Write-Ahead Log (WAL)

## 핵심 개념

etcd의 WAL은 모든 상태 변경을 디스크에 먼저 기록하여 장애 복구를 보장하는 핵심 컴포넌트다. Raft 합의 후의 모든 엔트리와 HardState가 WAL에 기록되며, 프로세스 크래시 후 재시작 시 WAL을 재생하여 상태를 복구한다.

### 레코드 구조

```
Record {
    Type int64   // MetadataType(1), EntryType(2), StateType(3), CrcType(4), SnapshotType(5)
    CRC  uint32  // 데이터의 CRC32 체크섬 (Castagnoli)
    Data []byte  // 직렬화된 데이터
}
```

### 프레임 형식 (8바이트 정렬)

```
[8바이트: 프레임크기]  ← 하위 56비트=데이터크기, 상위 8비트=패딩정보
[N바이트: 레코드 데이터]
[P바이트: 8바이트 정렬 패딩]  ← padBytes = (8 - N%8) % 8
```

8바이트 정렬은 torn write를 감지하기 위한 설계다. 디스크 섹터(512바이트) 경계에서 부분 기록이 발생해도 0으로 채워진 영역을 통해 감지할 수 있다.

### CRC32 연쇄

각 레코드의 CRC는 이전 레코드의 CRC를 시드로 사용하여 계산된다. 이를 통해 중간 레코드가 누락되거나 순서가 바뀌면 이후 모든 CRC 검증이 실패한다.

```
Record 1: CRC = hash(seed=0, data1)
Record 2: CRC = hash(seed=CRC1, data2)
Record 3: CRC = hash(seed=CRC2, data3)
```

### 세그먼트 파일 관리

WAL은 단일 파일이 아닌 여러 세그먼트 파일로 구성된다:
- 기본 세그먼트 크기: 64MB (`SegmentSizeBytes`)
- 초과 시 새 세그먼트 파일 생성 (`cut()` 메서드)
- 새 세그먼트 시작 시 이전 CRC를 CrcType 레코드로 기록
- 스냅샷 이전의 세그먼트 파일은 삭제 가능

## 구현 설명

### Append 흐름

1. 데이터의 CRC32 계산 (이전 CRC와 연쇄)
2. 레코드 직렬화 → 프레임 형식으로 파일에 기록
3. 현재 세그먼트 크기 확인 → 초과 시 새 세그먼트 생성

### ReadAll 흐름

1. WAL 디렉토리에서 모든 `.wal` 파일 정렬
2. 각 파일에서 프레임 단위로 레코드 읽기
3. CrcType 레코드로 CRC 체인 복원
4. 각 레코드의 CRC 검증 → 불일치 시 손상 감지

### 크래시 복구

1. 프로세스 크래시 발생
2. 재시작 시 WAL ReadAll 호출
3. 모든 레코드 순서대로 재생
4. 마지막 유효 상태 복원

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `server/storage/wal/wal.go` | WAL 구조체, Create, ReadAll, Save, cut |
| `server/storage/wal/encoder.go` | 레코드 인코딩, CRC 계산, 8바이트 정렬 |
| `server/storage/wal/decoder.go` | 레코드 디코딩, CRC 검증, torn write 감지 |
| `server/storage/wal/walpb/record.proto` | Record protobuf 정의 |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
=== etcd Write-Ahead Log (WAL) 시뮬레이션 ===

--- 시나리오 1: WAL 레코드 기록 ---
  기록: [Entry #1] PUT key1=value1 (CRC: xxxxxxxx)
  기록: [Entry #2] PUT key2=value2 (CRC: xxxxxxxx)
  ...
  기록: [State] Term:5 Vote:1 Commit:8 (CRC: xxxxxxxx)

세그먼트 파일 수: N
  00000000.wal: XXX 바이트
  00000001.wal: XXX 바이트

--- 시나리오 2: WAL 재생 (크래시 복구) ---
읽은 세그먼트: N개
읽은 레코드: 9개
CRC 오류: 0개

복구된 KV 상태:
  key2 = value2_updated
  key3 = value3
  ...

--- 시나리오 3: CRC 손상 감지 ---
  CRC 오류 감지: N개
  → CRC 체크섬 불일치로 데이터 손상 감지!

--- 시나리오 4: 세그먼트 파일 관리 ---
세그먼트 최대 크기: 128 바이트
생성된 세그먼트 파일: N개
전체 재생: N개 세그먼트에서 15개 레코드 복구
```
