# PoC-12: Sparse Offset Index (이진 검색) 시뮬레이션

## 개요

Kafka의 **희소 오프셋 인덱스(Sparse Offset Index)**를 시뮬레이션한다. Kafka는 각 로그 세그먼트에 대해 오프셋을 물리적 파일 위치로 매핑하는 인덱스를 유지한다. 이 인덱스는 모든 메시지가 아닌 일정 바이트 간격(`index.interval.bytes`)마다 하나의 엔트리만 저장하는 "희소" 구조이며, mmap으로 메모리 매핑하여 빠른 이진 검색을 지원한다.

## 기반 소스코드

| 파일 | 역할 |
|------|------|
| `OffsetIndex.java` | 희소 오프셋 인덱스 구현 (append, lookup, parseEntry) |
| `AbstractIndex.java` | 캐시 친화적 이진 검색, warm section, mmap 관리 |
| `LogSegment` | 인덱스를 사용한 메시지 조회 |

## 시뮬레이션 내용

### 시나리오 1: 희소 인덱스 구축
- `index.interval.bytes`마다 하나의 인덱스 엔트리 생성
- 전체 메시지의 일부만 인덱싱 (메모리 절약)

### 시나리오 2: 이진 검색 조회
- targetOffset 이하의 가장 큰 lower bound를 찾아 파일 위치 반환
- 해당 위치부터 순차 스캔하여 정확한 오프셋 위치 결정

### 시나리오 3: 고정 크기 엔트리 포맷
- 8바이트 엔트리: 4바이트 상대 오프셋 + 4바이트 물리적 위치
- 상대 오프셋 사용으로 4바이트에 최대 2^31개 오프셋 표현

### 시나리오 4: 캐시 친화적 이진 검색
- warm section (마지막 ~1024 엔트리)을 우선 검색
- in-sync 팔로워/컨슈머의 조회는 대부분 warm section에서 완료
- 이진 검색 vs 순차 스캔 성능 비교

### 시나리오 5: 실제 메시지 조회 흐름
- FetchRequest -> 인덱스 조회 -> 파일 위치 -> 순차 스캔 -> 메시지 반환

## 핵심 알고리즘

```
캐시 친화적 이진 검색 (AbstractIndex):

warmEntries = 8192 / ENTRY_SIZE  // ~1024 entries

lookup(targetOffset):
  firstHot = entries - 1 - warmEntries
  if index[firstHot] < target:
    binarySearch(firstHot, entries-1)  // warm section
  elif index[0] > target:
    return baseOffset  // target이 너무 작음
  else:
    binarySearch(0, firstHot)          // cold section
```

## 실행 방법

```bash
go run main.go
```
