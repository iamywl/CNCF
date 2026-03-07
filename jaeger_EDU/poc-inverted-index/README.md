# PoC 5: Badger 스타일 역인덱스(Inverted Index) 시뮬레이션

## 개요

Jaeger의 Badger 스토리지 백엔드가 사용하는 **역인덱스(Inverted Index)** 시스템을 시뮬레이션한다. Jaeger는 span 데이터를 저장할 때 다양한 보조 인덱스를 함께 생성하여 빠른 트레이스 검색을 지원한다.

## 실제 Jaeger 소스 참조

| 파일 | 핵심 함수/구조체 | 역할 |
|------|-----------------|------|
| `internal/storage/v1/badger/spanstore/writer.go` | `WriteSpan()`, `createIndexKey()` | span 저장 시 인덱스 키 생성 |
| `internal/storage/v1/badger/spanstore/reader.go` | `FindTraceIDs()`, `mergeJoinIds()`, `indexSeeksToTraceIDs()` | 인덱스 스캔 및 교차 검색 |

## 인덱스 키 스키마

Badger는 정렬된 KV 스토어(LSM-tree 기반)이므로, 키의 바이트 순서가 곧 정렬 순서이다.

```
+---------+-----------+------------+-----------+
| Prefix  |  Value    | StartTime  |  TraceID  |
| (1byte) | (가변)    | (8 bytes)  | (16 bytes)|
+---------+-----------+------------+-----------+
```

### 인덱스 타입

| 프리픽스 | 이름 | Value 내용 |
|---------|------|-----------|
| `0x81` | serviceNameIndexKey | serviceName |
| `0x82` | operationNameIndexKey | serviceName + operationName |
| `0x83` | tagIndexKey | serviceName + tagKey + tagValue |
| `0x84` | durationIndexKey | duration (BigEndian 8bytes) |

## 시뮬레이션 내용

1. **인덱스 구축**: 10,000개 span에 대해 4종류 보조 인덱스 생성
2. **단일 인덱스 검색**: 서비스명 기반 빠른 검색
3. **다중 인덱스 교차(AND)**: merge-join 알고리즘으로 O(n+m) 교집합
4. **인덱스 유니온(OR)**: 여러 서비스의 결과 합집합
5. **Duration 필터**: 기간 범위 인덱스 스캔 + 해시 조인
6. **인덱스 vs 풀스캔 성능 비교**: 실질적 성능 차이 측정

## merge-join 알고리즘

Jaeger는 다중 인덱스 결과를 교차시킬 때 **정렬된 merge-join**을 사용한다:

```
sorted_left:  [A, B, C, D, E]
sorted_right: [B, D, F]

→ 두 포인터를 동시에 전진시키며 일치하는 것만 수집
→ 결과: [B, D]
→ 시간 복잡도: O(n + m)
```

## 실행 방법

```bash
cd poc-inverted-index
go run main.go
```

## 핵심 학습 포인트

- Badger의 키 스키마 설계: 프리픽스 바이트로 인덱스 타입 구분
- BigEndian 인코딩으로 사전순 정렬 = 시간순 정렬 보장
- merge-join으로 효율적인 다중 조건 AND 검색
- 인덱스 검색이 풀스캔 대비 수배~수십배 빠른 이유
