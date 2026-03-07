# PoC-10: Log Compaction (키 기반 중복 제거) 시뮬레이션

## 개요

Kafka의 **로그 컴팩션(Log Compaction)** 메커니즘을 시뮬레이션한다. 로그 컴팩션은 동일 키의 레코드 중 가장 최신 값만 유지하고 이전 값을 제거하는 프로세스로, changelog 스타일의 데이터(설정 변경, 사용자 프로필 업데이트 등)를 효율적으로 저장하는 데 사용된다.

## 기반 소스코드

| 파일 | 역할 |
|------|------|
| `Cleaner.java` | 로그 컴팩션의 핵심 로직 (buildOffsetMap, cleanInto, shouldRetainRecord) |
| `OffsetMap` (SkimpyOffsetMap) | key 해시 -> latest offset 매핑 (메모리 효율적 해시맵) |
| `LogCleaner.java` | Cleaner 스레드 관리, 컴팩션 대상 선택 |
| `CleanerStats.java` | 컴팩션 통계 수집 |

## 시뮬레이션 내용

### 시나리오 1: 기본 컴팩션
- 동일 키의 여러 버전 중 최신 값만 유지
- OffsetMap 구축 과정 시각화

### 시나리오 2: Tombstone 처리
- `value=null`인 레코드는 삭제 마커(tombstone)
- `delete.retention.ms` 이내의 tombstone은 유지, 이후는 제거
- 컨슈머가 삭제를 인지할 수 있는 시간 보장

### 시나리오 3: OffsetMap 해시 기반 저장
- 키 자체가 아닌 해시값만 저장하여 메모리 효율 극대화
- 선형 탐색(linear probing) 충돌 해결

### 시나리오 4: 대규모 데이터 컴팩션 효과
- 100개 키 x 10번 업데이트 = 1000 레코드 -> 100 레코드로 압축 (90% 공간 절약)

## 핵심 알고리즘

```
doClean(log):
  1. buildOffsetMap(dirty segments)
     -> key 해시 -> latest offset 매핑 구축

  2. cleanInto(source, dest, offsetMap):
     for each record in source:
       latestOffset = offsetMap.get(record.key)
       if record.offset >= latestOffset:
         if record.hasValue() OR withinDeleteRetention:
           dest.append(record)  // 유지
       else:
         skip  // 이전 값, 제거

  3. tombstone lifecycle:
     produce(key, null) -> 생성
     -> delete.retention.ms 유지
     -> 이후 컴팩션에서 제거
```

## 실행 방법

```bash
go run main.go
```
