# PoC-01: Prometheus TSDB 시계열 저장소

## 개요

Prometheus의 **Head Block**(인메모리 시계열 저장소)의 핵심 구조를 Go 표준 라이브러리만으로 시뮬레이션한다.

Prometheus TSDB는 시계열 데이터를 효율적으로 저장하고 조회하기 위해 **역색인(Inverted Index)** 기반의 구조를 사용한다. 이 PoC는 다음 세 가지 핵심 개념을 재현한다:

1. **Labels** — 정렬된 key-value 쌍으로 시계열을 고유하게 식별
2. **MemSeries** — 레이블 + 샘플(타임스탬프, 값)을 보유하는 인메모리 시계열
3. **MemPostings** — `map[labelName]map[labelValue][]SeriesRef` 구조의 역색인

## 실제 소스코드 참조

| 구성 요소 | 실제 파일 | 설명 |
|-----------|----------|------|
| Labels | `model/labels/labels_stringlabels.go` | 단일 string에 인코딩된 정렬 레이블 |
| memSeries | `tsdb/head.go` | ref, lset, headChunks, mmappedChunks 등 |
| MemPostings | `tsdb/index/postings.go` | `map[string]map[string][]SeriesRef` 역색인 |
| Matcher | `model/labels/matcher.go` | Equal, NotEqual, Regexp, NotRegexp |

## 핵심 동작 원리

### 데이터 삽입 (Append)

```
Append(labels, timestamp, value)
  │
  ├─ labels.Hash()로 기존 시계열 확인
  │   ├─ 있으면 → 샘플만 추가
  │   └─ 없으면 → 새 MemSeries 생성
  │
  └─ MemPostings.Add(ref, labels)
      └─ 각 label pair마다 역색인에 등록
          __name__="http_requests_total" → [ref]
          method="GET"                   → [ref]
          status="200"                   → [ref]
```

### 쿼리 실행 (Select)

```
Select(__name__="http_requests_total", method="GET")
  │
  ├─ 1단계: 각 매처의 posting list 조회
  │   postings[__name__][http_requests_total] → [1, 2, 3, 4, 5]
  │   postings[method][GET]                   → [1, 2]
  │
  ├─ 2단계: 교집합 계산
  │   intersection([1,2,3,4,5], [1,2]) → [1, 2]
  │
  └─ 3단계: SeriesRef로 MemSeries 조회 → 결과 반환
```

### 매처 타입별 동작

| 매처 | 동작 |
|------|------|
| `=` (Equal) | 역색인에서 O(1) 직접 룩업 |
| `!=` (NotEqual) | 해당 label name의 모든 값 순회, 불일치만 포함 |
| `=~` (Regexp) | 해당 label name의 모든 값을 정규식 매칭 |
| `!~` (NotRegexp) | 해당 label name의 모든 값 중 정규식 불일치만 포함 |

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 예상 출력

```
╔══════════════════════════════════════════════════════════════════════╗
║     Prometheus TSDB 시계열 저장소 PoC (In-Memory Head Block)        ║
╚══════════════════════════════════════════════════════════════════════╝

[1] 데이터 삽입: http_requests_total
======================================================================
  시계열 생성: ref=1 {__name__="http_requests_total", handler="/api/v1/query", method="GET", status="200"}
  시계열 생성: ref=2 {__name__="http_requests_total", handler="/api/v1/query", method="GET", status="404"}
  시계열 생성: ref=3 {__name__="http_requests_total", handler="/api/v1/write", method="POST", status="200"}
  시계열 생성: ref=4 {__name__="http_requests_total", handler="/api/v1/write", method="POST", status="500"}
  시계열 생성: ref=5 {__name__="http_requests_total", handler="/api/v1/series", method="DELETE", status="200"}

[2] 데이터 삽입: cpu_usage
======================================================================
  시계열 생성: ref=6 {__name__="cpu_usage", cpu="0", instance="node-1", mode="user"}
  시계열 생성: ref=7 {__name__="cpu_usage", cpu="0", instance="node-1", mode="system"}
  시계열 생성: ref=8 {__name__="cpu_usage", cpu="1", instance="node-1", mode="user"}
  시계열 생성: ref=9 {__name__="cpu_usage", cpu="0", instance="node-2", mode="user"}

역색인 (MemPostings) 내부 구조
======================================================================
구조: map[labelName] → map[labelValue] → []SeriesRef

  [__name__]
    "cpu_usage" → refs=[6 7 8 9]
    "http_requests_total" → refs=[1 2 3 4 5]
  [cpu]
    "0" → refs=[6 7 9]
    "1" → refs=[8]
  [handler]
    "/api/v1/query" → refs=[1 2]
    "/api/v1/series" → refs=[5]
    "/api/v1/write" → refs=[3 4]
  ...

쿼리 1: __name__="http_requests_total" AND method="GET"
  → 역색인 룩업: postings[__name__][http_requests_total] ∩ postings[method][GET]

  결과 (2개 시계열)
----------------------------------------------------------------------
  ref=1  {__name__="http_requests_total", handler="/api/v1/query", method="GET", status="200"}
    @ HH:MM:SS.mmm → 100.00
    ...
  ref=2  {__name__="http_requests_total", handler="/api/v1/query", method="GET", status="404"}
    ...

  총 시계열 수:     9
  총 샘플 수:       27
  고유 label 이름:  7
  역색인 엔트리:    17 (label name-value 쌍)
```

7개의 쿼리를 실행하며, Equal/NotEqual/Regexp/NotRegexp 4가지 매처 타입의 동작을 확인할 수 있다.

## 실제 Prometheus와의 차이점

| 항목 | 이 PoC | 실제 Prometheus |
|------|--------|----------------|
| Labels 인코딩 | `[]Label` 슬라이스 | 단일 string에 길이 접두사 인코딩 |
| 샘플 저장 | `[]Sample` 슬라이스 | XOR/Gorilla 청크 인코딩 (`chunkenc`) |
| 해시 함수 | 문자열 연결 | xxhash |
| Posting list 교집합 | `map[SeriesRef]struct{}` set 연산 | 정렬된 리스트의 병합 순회 |
| 동시성 제어 | 없음 | `sync.RWMutex`, stripe sharding |
| WAL | 없음 | Write-Ahead Log로 장애 복구 |
| 청크 관리 | 없음 | memChunk → mmap → block 컴팩션 |
