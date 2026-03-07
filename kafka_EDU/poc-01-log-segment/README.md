# PoC-01: Kafka Log Segment

## 개요

Kafka의 핵심 저장 구조인 **Log Segment**를 시뮬레이션한다. Kafka는 토픽 파티션 데이터를 `.log` 파일(실제 레코드)과 `.index` 파일(오프셋 -> 물리 위치 매핑)의 쌍으로 관리한다.

## 실제 Kafka 소스 참조

| 클래스 | 경로 | 역할 |
|--------|------|------|
| `LogSegment.java` | `storage/src/main/java/.../log/LogSegment.java` | 세그먼트 append/read/roll |
| `OffsetIndex.java` | `storage/src/main/java/.../log/OffsetIndex.java` | 8B 엔트리(relOffset+position), 이진 탐색 |
| `AbstractIndex.java` | `storage/src/main/java/.../log/AbstractIndex.java` | `largestLowerBoundSlotFor()` 이진 탐색 알고리즘 |

## 시뮬레이션하는 핵심 알고리즘

### 1. Append-Only 로그 기록
- `LogSegment.append()`: 레코드를 `.log` 파일 끝에 추가
- `indexIntervalBytes` 간격마다 인덱스 엔트리 생성 (sparse index)

### 2. Sparse Offset Index
- 모든 레코드가 아닌 일정 바이트 간격마다만 인덱스 엔트리 생성
- 엔트리 구조: `relativeOffset(4B) + physicalPosition(4B) = 8B`
- 상대 오프셋 사용으로 4바이트만으로 표현 가능

### 3. 이진 탐색 기반 읽기
- `OffsetIndex.lookup()` -> `AbstractIndex.binarySearch()`
- target 이하의 가장 큰 인덱스 엔트리를 찾은 후, 해당 위치에서 순차 스캔

### 4. 세그먼트 롤링
- `LogSegment.shouldRoll()`: 세그먼트 크기가 임계값 초과 시 새 세그먼트 생성

## 실행 방법

```bash
go run main.go
```

## 출력 내용

1. 30개 레코드 추가 과정 (세그먼트 롤링 포함)
2. 세그먼트별 현황 (크기, 인덱스 엔트리)
3. 파일 시스템에 생성된 `.log`/`.index` 파일 목록
4. 오프셋으로 읽기 (인덱스 이진 탐색 -> 순차 스캔)
5. 인덱스 이진 탐색 상세 동작

## 핵심 설계 원리

```
쓰기 흐름:
  Producer -> append(.log) -> bytesSinceLastIndex > interval? -> append(.index)
                                                                    |
                                                              size > max? -> roll()

읽기 흐름:
  Consumer(offset=N) -> .index 이진 탐색 -> 물리 위치 P -> .log에서 P부터 순차 스캔
```
