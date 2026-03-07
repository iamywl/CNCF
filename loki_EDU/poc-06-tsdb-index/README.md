# PoC 06: TSDB 인덱스 (Label-based Time Series Index)

## 개요

Loki의 TSDB 스타일 역인덱스(Inverted Index)를 시뮬레이션한다.
레이블 기반으로 로그 스트림을 빠르게 검색하는 핵심 메커니즘을 구현한다.

## 시뮬레이션하는 Loki 컴포넌트

| 컴포넌트 | Loki 실제 위치 | 설명 |
|----------|---------------|------|
| Index | `pkg/storage/stores/shipper/indexshipper/tsdb/index/index.go` | TSDB 인덱스 |
| Postings | `pkg/storage/stores/shipper/indexshipper/tsdb/index/postings.go` | Posting List |
| Matchers | `pkg/logql/syntax/matchers.go` | Label Matchers |
| Series | TSDB index | 시계열(스트림) 메타데이터 |

## 핵심 자료구조

### 역인덱스 (Inverted Index)

```
Label Pair          Posting List       Series
──────────          ────────────       ──────
app="nginx"     →  [fp1, fp2, fp3]
app="api"       →  [fp4, fp5]
level="error"   →  [fp2, fp5, fp7]
level="info"    →  [fp1, fp4, fp6]
namespace="prod"→  [fp1, fp2, fp4, fp5, fp6]
```

### 쿼리 처리 과정

```
{app="nginx", level="error"}

Step 1:  app="nginx"    →  Posting List A: [fp1, fp2, fp3]
Step 2:  level="error"  →  Posting List B: [fp2, fp5, fp7]
Step 3:  Intersect(A,B) →  Result: [fp2]
Step 4:  Lookup fp2     →  Series 메타데이터 반환
```

## 매처 유형

| 매처 | 기호 | 설명 | 검색 전략 |
|------|------|------|----------|
| Equal | `=` | 정확히 일치 | 직접 posting list 조회 |
| NotEqual | `!=` | 불일치 | 전체 - 일치 (차집합) |
| Regexp | `=~` | 정규식 매칭 | 매칭 값들의 합집합 |
| NotRegexp | `!~` | 정규식 불일치 | 전체 - 매칭 (차집합) |

## 시나리오

1. **역인덱스 구조**: 10개 시리즈의 인덱스 구축 및 구조 확인
2. **Equal 쿼리**: 단일/복합 레이블 매칭
3. **NotEqual 쿼리**: 특정 값 제외 검색
4. **정규식 쿼리**: `=~` / `!~` 패턴 매칭
5. **Posting List 연산**: 교집합/합집합 과정 시각화
6. **성능 특성**: 각 연산의 시간 복잡도 분석

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

- 역인덱스가 전문 검색(full-text search)과 다른 점 (구조화된 레이블 매칭)
- Posting List를 정렬 유지하여 집합 연산을 O(n+m)으로 수행하는 이유
- NotEqual/Regex 쿼리가 Equal보다 비용이 높은 이유
- Cardinality(고유 값 수)가 쿼리 성능에 미치는 영향
- Fingerprint를 통한 시리즈 O(1) 직접 접근
