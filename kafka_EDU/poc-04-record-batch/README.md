# PoC-04: Kafka RecordBatch Encoding/Decoding

## 개요

Kafka의 RecordBatch(magic v2) 직렬화/역직렬화를 시뮬레이션한다. Varint 인코딩, Delta 인코딩, CRC32C 체크섬 등 Kafka가 디스크/네트워크 효율을 극대화하기 위해 사용하는 인코딩 기법을 구현한다.

## 실제 Kafka 소스 참조

| 클래스 | 경로 | 역할 |
|--------|------|------|
| `DefaultRecord.java` | `clients/src/main/java/.../record/internal/DefaultRecord.java` | 개별 레코드 인코딩 (varint, delta) |
| `DefaultRecordBatch.java` | `clients/src/main/java/.../record/internal/DefaultRecordBatch.java` | 배치 헤더 (61B), CRC32C |
| `ByteUtils.java` | `clients/src/main/java/.../common/utils/ByteUtils.java` | varint/varlong 인코딩 |

## 시뮬레이션하는 핵심 알고리즘

### 1. Varint/Varlong 인코딩
- ZigZag 인코딩: `(value << 1) ^ (value >> 31)` (음수도 효율적으로 표현)
- Variable-Length: 7비트씩 사용, MSB가 1이면 다음 바이트 존재
- 작은 값(0~63)은 1바이트, 큰 값만 여러 바이트

### 2. Delta 인코딩
- `TimestampDelta = timestamp - baseTimestamp`
- `OffsetDelta = offset - baseOffset`
- 절대값 대비 훨씬 작은 값 -> varint로 적은 바이트 사용

### 3. RecordBatch 헤더 (61 bytes)
- `DefaultRecordBatch.RECORD_BATCH_OVERHEAD = 61`
- 레이아웃: BaseOffset(8B) + Length(4B) + PartitionLeaderEpoch(4B) + Magic(1B) + CRC(4B) + Attributes(2B) + LastOffsetDelta(4B) + BaseTimestamp(8B) + MaxTimestamp(8B) + ProducerId(8B) + ProducerEpoch(2B) + BaseSequence(4B) + RecordsCount(4B)

### 4. CRC32C 체크섬
- Castagnoli 다항식 (하드웨어 가속 지원)
- Attributes부터 배치 끝까지 커버
- PartitionLeaderEpoch는 브로커에서 매번 갱신하므로 CRC에서 제외

### 5. 개별 Record 구조
- `DefaultRecord.writeTo()`: Length(varint) + Attributes(1B) + TsDelta(varlong) + OffsetDelta(varint) + Key + Value + Headers

## 실행 방법

```bash
go run main.go
```

## 출력 내용

1. Varint 인코딩: 다양한 값의 인코딩 크기 비교
2. Delta 인코딩 효과: 절대값 vs 상대값 크기 절약
3. 개별 Record 인코딩 + 라운드트립 검증
4. RecordBatch 헤더 레이아웃 상세
5. CRC32C 검증 + 변조 감지 테스트
6. 100개 레코드 배치의 압축 효과 추정

## RecordBatch 바이트 레이아웃

```
Offset  Size  Field
------  ----  -----
0       8     BaseOffset
8       4     Length (CRC 이후 전체 크기)
12      4     PartitionLeaderEpoch (CRC 제외)
16      1     Magic (= 2)
17      4     CRC32C (Attributes ~ 끝)
21      2     Attributes
23      4     LastOffsetDelta
27      8     BaseTimestamp
35      8     MaxTimestamp
43      8     ProducerId
51      2     ProducerEpoch
53      4     BaseSequence
57      4     RecordsCount
61      ...   Records (varint encoded)
```
