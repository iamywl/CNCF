# PoC-05: Inverted Index (역인덱스 — MemPostings)

## 개요

Prometheus TSDB의 **역인덱스(Inverted Index)** 핵심 구조체인 `MemPostings`를 시뮬레이션하는 PoC이다.

Prometheus는 수십만~수백만 개의 시계열을 레이블 기반으로 빠르게 검색하기 위해 역인덱스를 사용한다. `MemPostings`는 `map[labelName]map[labelValue][]SeriesRef` 구조로, 각 레이블 쌍(name=value)에 해당하는 시리즈 참조(ref) 목록을 **정렬된 상태**로 유지한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 핵심 개념

### 역인덱스란?

일반적인 인덱스가 "문서 → 키워드"를 매핑한다면, 역인덱스는 **"키워드 → 문서 목록"**을 매핑한다. Prometheus에서는:

```
일반 인덱스:  시리즈 ref=42 → {job="api", method="GET", status="200"}
역인덱스:     job="api"      → [42, 87, 153, 298, ...]
              method="GET"   → [42, 55, 87, 120, ...]
              status="200"   → [42, 87, 201, 342, ...]
```

### MemPostings 구조

```
참조: tsdb/index/postings.go

type MemPostings struct {
    mtx     sync.RWMutex
    m       map[string]map[string][]storage.SeriesRef  // 핵심 인덱스
    lvs     map[string][]string                        // 레이블명별 값 목록
    ordered bool                                       // 정렬 완료 여부
}
```

메모리 구조 시각화:

```
m["job"]["api-server"]       → [3, 7, 12, 18, 25, ...]    ← 정렬된 SeriesRef
m["job"]["web-frontend"]     → [1, 5, 9, 15, 20, ...]
m["method"]["GET"]           → [1, 4, 8, 13, 18, ...]
m["method"]["POST"]          → [2, 6, 11, 17, 23, ...]
m["status"]["200"]           → [1, 3, 7, 12, 16, ...]
m["status"]["500"]           → [5, 18, 33, 47, 62, ...]
m[""][""] (allPostingsKey)   → [1, 2, 3, 4, 5, ..., N]    ← 전체 시리즈
```

### Postings 인터페이스

정렬된 SeriesRef 목록에 대한 이터레이터 패턴:

```go
type Postings interface {
    Next() bool              // 다음 원소로 전진
    Seek(v SeriesRef) bool   // v 이상의 위치로 건너뜀
    At() SeriesRef           // 현재 값 반환
    Err() error              // 에러 반환
}
```

`Seek()`이 핵심이다. 교집합 연산에서 작은 리스트의 값을 기준으로 큰 리스트를 `Seek`하면 불필요한 원소를 건너뛸 수 있다.

### 집합 연산

| 연산 | 용도 | 매처 예시 | 시간복잡도 |
|------|------|----------|-----------|
| **Intersect** | AND 조건 | `{job="api", method="GET"}` | O(n) — 투 포인터 + Seek |
| **Merge** | OR 조건 / 정규식 | `{status=~"5.."}` | O(n log k) — loser tree |
| **Without** | NOT 조건 | `{status!="500"}` | O(n) — 투 포인터 |

#### Intersect (교집합)

```
리스트A (job=api):    [3, 7, 12, 18, 25, 31, 38, ...]
리스트B (method=GET): [1, 4, 8, 13, 18, 22, 29, ...]
                                   ↑↑
                              두 포인터가 만남 → 결과에 포함

알고리즘:
1. A에서 Next() → target = 3
2. B에서 Seek(3) → B.At() = 4 > 3 → 새 target = 4
3. A에서 Seek(4) → A.At() = 7 > 4 → 새 target = 7
4. B에서 Seek(7) → B.At() = 8 > 7 → 새 target = 8
5. ... 반복하다가 둘 다 18을 가리키면 교집합에 포함
```

#### Merge (합집합)

```
리스트A: [3, 7, 12, 18, 25]
리스트B: [1, 4, 8, 13, 18]
결과:    [1, 3, 4, 7, 8, 12, 13, 18, 25]  (중복 제거)

실제 Prometheus: go-loser 라이브러리의 loser tree 사용
→ k개 리스트에서 최소값을 O(log k)에 추출
```

#### Without (차집합)

```
full:   [3, 7, 12, 18, 25, 31, 38, ...]
drop:   [7, 25, 38, ...]
결과:   [3, 12, 18, 31, ...]

full과 drop을 동시에 순회하며:
- full < drop → 결과에 포함
- full == drop → 건너뜀
- full > drop → drop을 Seek
```

## 성능 특성

### 시간복잡도

| 연산 | 시간복잡도 | 설명 |
|------|-----------|------|
| Add(ref, labels) | O(L) | L = 레이블 수, 각 쌍에 대해 insertion sort |
| Postings(name, value) | O(1) | map 조회 |
| Intersect(a, b) | O(min(a,b)) | 작은 쪽 기준으로 Seek |
| Merge(lists...) | O(N log k) | N = 전체 원소 수, k = 리스트 수 |
| Without(full, drop) | O(full) | 한 번의 순회 |
| LabelValues(name) | O(V) | V = 값의 개수, lvs에서 바로 반환 |
| AllPostings() | O(1) | allPostingsKey에서 조회 |

### 왜 역인덱스인가?

10,000개 시리즈에서 `{job="api-server", method="GET"}` 쿼리:

| 방식 | 과정 | 비교 횟수 |
|------|------|----------|
| **브루트포스** | 10,000개 시리즈 각각의 레이블을 하나씩 비교 | ~60,000회 (6 labels x 10,000) |
| **역인덱스** | job=api 포스팅(~2,000) + method=GET 포스팅(~2,000) 교집합 | ~4,000회 |

시리즈 수가 증가할수록 차이는 더 벌어진다. 100만 시리즈에서는 수십 배 이상의 성능 차이가 발생한다.

### 왜 정렬을 유지하는가?

정렬된 포스팅 리스트는 집합 연산의 핵심 전제조건이다:

1. **Intersect**: 투 포인터로 O(n) 교집합 (비정렬이면 O(n*m) 또는 해시셋 필요)
2. **Merge**: 병합 정렬로 O(n) 합집합 (비정렬이면 O(n log n) 정렬 필요)
3. **Without**: 투 포인터로 O(n) 차집합 (비정렬이면 O(n*m))
4. **Seek**: 순차 접근으로 빠른 건너뛰기 가능

### allPostingsKey의 역할

`m[""][""]`에 모든 시리즈의 ref를 저장한다. 용도:

- `{__name__=~".+"}` 같은 전체 시리즈 조회
- `Without(AllPostings, drop)` 패턴으로 NOT 조건 처리
- 시리즈 수 카운트

## 실제 코드 참조

| 파일 | 핵심 구조/함수 |
|------|---------------|
| `tsdb/index/postings.go` | `MemPostings` 구조체, `Add()`, `Postings()`, `AllPostings()` |
| `tsdb/index/postings.go` | `Postings` 인터페이스, `listPostings`, `ExpandPostings()` |
| `tsdb/index/postings.go` | `Intersect()`, `intersectPostings` — Seek 기반 교집합 |
| `tsdb/index/postings.go` | `Merge()`, `mergedPostings` — loser tree 기반 합집합 |
| `tsdb/index/postings.go` | `Without()`, `removedPostings` — 투 포인터 차집합 |
| `tsdb/index/postings.go` | `EnsureOrder()` — 벌크 로드 후 병렬 정렬 |
| `tsdb/index/postings.go` | `PostingsForLabelMatching()` — 정규식 매칭 최적화 |

## 데모 내용

1. **시리즈 등록**: 10,000개 시리즈를 다양한 레이블 조합으로 생성
2. **인덱스 통계**: 레이블별 카디널리티, 포스팅 리스트 크기
3. **LabelValues**: 레이블명으로 고유 값 목록 조회
4. **단일 매칭**: 개별 레이블 조건으로 시리즈 검색
5. **Intersect**: 2~4개 레이블 AND 조건 쿼리
6. **Merge**: OR 조건 및 정규식 매칭 시뮬레이션
7. **Without**: NOT 조건 쿼리
8. **복합 쿼리**: Intersect + Merge + Without 조합
9. **성능 프로파일링**: 카디널리티별, 연산별, 구현 방식별 비교
10. **브루트포스 비교**: 역인덱스의 성능 우위 정량적 측정
11. **카디널리티 분석**: 포스팅 리스트 크기 분포 시각화
12. **메모리 구조 시각화**: ASCII 다이어그램으로 인덱스 구조 표현
