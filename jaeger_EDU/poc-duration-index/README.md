# PoC 13: Cassandra Duration Index 전략 시뮬레이션

## 개요

Jaeger의 Cassandra 스토리지 백엔드에서 사용하는 **duration_index** 파티셔닝 전략을 시뮬레이션합니다.
이 인덱스는 "느린 요청 찾기"와 같은 duration 기반 트레이스 검색을 효율적으로 지원하기 위해 설계되었습니다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `internal/storage/v1/cassandra/spanstore/writer.go` | `indexByDuration()` - 인덱스 쓰기 |
| `internal/storage/v1/cassandra/spanstore/reader.go` | `queryByDuration()` - 인덱스 읽기 |
| `docs/adr/001-cassandra-find-traces-duration.md` | 설계 결정 문서 (ADR) |

## 핵심 설계 원리

### 1시간 버킷 파티셔닝
```
durationBucketSize = time.Hour  // writer.go L57
timeBucket := startTime.Round(durationBucketSize)  // writer.go L231
```

파티션 키: `(service_name, operation_name, bucket)` 조합으로 데이터를 분산시켜 핫 파티션을 방지합니다.

### 스팬당 2개 인덱스 엔트리
```go
indexByOperationName("")                 // 서비스 전체 검색용
indexByOperationName(span.OperationName) // 특정 오퍼레이션 검색용
```

### Duration과 태그 쿼리 교차 불가
```
ErrDurationAndTagQueryNotSupported = "cannot query for duration and tags simultaneously"
```

Cassandra의 파티션 키 기반 아키텍처로 인해 서로 다른 인덱스 간 서버사이드 조인이 불가능합니다.

## 시뮬레이션 내용

1. **인덱스 쓰기**: 다양한 서비스/오퍼레이션의 스팬에 대해 duration_index 엔트리 생성
2. **파티션 분포 분석**: 시간 버킷별 파티션 키 분포 확인
3. **Duration 범위 쿼리**: endTime부터 startTime까지 역순 버킷 반복
4. **교차 쿼리 불가 시연**: `ValidateQuery`에서 duration+태그 동시 쿼리 거부
5. **2개 엔트리 전략**: operation_name="" vs 실제 오퍼레이션명의 효과 비교
6. **쿼리 분포 효과**: 시간 범위에 따른 버킷 스캔 수 분석

## 실행 방법

```bash
go run main.go
```

## 주요 출력

- 파티션 키별 엔트리 분포 테이블
- Duration 범위 쿼리 결과 및 버킷 스캔 통계
- Duration+태그 동시 쿼리 시 오류 메시지
- 시간 범위별 성능 비교

## 핵심 인사이트

- 1시간 버킷은 파티션 크기 제한과 쿼리 효율의 균형점
- 쿼리는 endTime부터 역순 → 최신 데이터 우선 반환, `NumTraces` 도달 시 조기 종료
- Badger 같은 임베디드 스토리지는 교차 쿼리 가능하지만, Cassandra의 분산 아키텍처에서는 불가능
- 우회 방법: 별도 쿼리 후 클라이언트 사이드 교차
