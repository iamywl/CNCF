# 20. 캐싱 시스템 (LRU) + Iterator 최적화 (jiter) Deep-Dive

> Jaeger 소스코드 기반 분석 문서 (P2 심화)
> 분석 대상: `internal/cache/`, `internal/jiter/`

---

## 1. 개요

### 1.1 왜 캐싱이 필요한가?

분산 트레이싱 시스템은 초당 수만~수십만 건의 스팬을 처리한다. 각 스팬에는 서비스 이름, 오퍼레이션 이름 등의
메타데이터가 포함되며, 이 정보를 매번 스토리지에서 조회하면 I/O 병목이 발생한다.

```
캐시 없는 경우:
  스팬 100,000개/초 × 서비스 이름 조회 = 100,000 DB 요청/초

캐시 있는 경우:
  스팬 100,000개/초 × 캐시 히트율 99% = 1,000 DB 요청/초
  (100배 감소)
```

### 1.2 왜 Iterator 최적화가 필요한가?

Go 1.23에서 공식 `iter` 패키지가 도입되었으나, Jaeger는 이전 버전도 지원해야 했다.
`jiter` 패키지는 에러 처리가 포함된 이터레이터 유틸리티를 제공하여, 스토리지 백엔드에서
대량의 스팬을 효율적으로 반복 처리한다.

### 1.3 소스 구조

```
internal/cache/
├── cache.go        # Cache 인터페이스, Options 구조체
├── lru.go          # LRU 캐시 구현체
└── lru_test.go     # LRU 캐시 테스트

internal/jiter/
├── iter.go         # CollectWithErrors, FlattenWithErrors
├── iter_test.go    # 이터레이터 유틸리티 테스트
└── package_test.go # 패키지 테스트 유틸리티
```

---

## 2. Cache 인터페이스 설계

### 2.1 인터페이스 정의

```go
// internal/cache/cache.go
type Cache interface {
    // Get -- 키로 요소 조회, 없으면 nil
    Get(key string) any

    // Put -- 요소 추가, 이전 값 반환
    Put(key string, value any) any

    // Delete -- 요소 삭제
    Delete(key string)

    // Size -- 현재 저장된 항목 수
    Size() int

    // CompareAndSwap -- 원자적 조건부 교체
    CompareAndSwap(key string, oldEntry, newEntry any) (any, bool)
}
```

**왜 이런 인터페이스인가?**

1. **`Get`이 `any`를 반환**: 다양한 타입의 캐시 값을 지원 (서비스 이름, 오퍼레이션 이름, 인덱스 정보 등)
2. **`Put`이 이전 값을 반환**: 호출자가 교체된 값에 대한 정리 작업을 할 수 있다
3. **`CompareAndSwap`**: 동시성 환경에서 안전한 업데이트 (외부 잠금 없이 원자적 비교-교체)
4. **`Size`**: 캐시 크기 모니터링, 워밍업 상태 확인

### 2.2 Options 구조체

```go
// internal/cache/cache.go
type Options struct {
    TTL             time.Duration   // 항목 만료 시간
    InitialCapacity int             // 초기 맵 용량
    OnEvict         EvictCallback   // 퇴출 콜백
    TimeNow         func() time.Time // 시간 함수 오버라이드 (테스트용)
}

type EvictCallback func(key string, value any)
```

| 옵션 | 용도 | 설명 |
|------|------|------|
| TTL | 데이터 신선도 | 서비스 이름 캐시 5분, 인덱스 정보 1시간 등 |
| InitialCapacity | 메모리 최적화 | 리해싱 방지를 위한 초기 할당 |
| OnEvict | 리소스 정리 | 퇴출 시 로깅, 메트릭 기록 등 |
| TimeNow | 테스트 가능성 | 시간을 제어하여 TTL 만료 테스트 |

---

## 3. LRU 캐시 구현

### 3.1 데이터 구조

```go
// internal/cache/lru.go
type LRU struct {
    mux      sync.Mutex         // 동시 접근 보호
    byAccess *list.List         // 접근 순서 (이중 연결 리스트)
    byKey    map[string]*list.Element  // 키 → 리스트 요소 맵
    maxSize  int                // 최대 항목 수
    ttl      time.Duration      // TTL
    TimeNow  func() time.Time   // 시간 함수
    onEvict  EvictCallback       // 퇴출 콜백
}

type cacheEntry struct {
    key        string
    expiration time.Time    // 만료 시각
    value      any
}
```

**왜 이중 연결 리스트 + 해시맵 조합인가?**

```
해시맵 (byKey)                     이중 연결 리스트 (byAccess)
+-------+-------+                  HEAD (최근) ←→ ... ←→ TAIL (최고)
| "svc1"| *elem |  ----→           [svc3] ←→ [svc1] ←→ [svc5] ←→ [svc2]
| "svc2"| *elem |  ----→              ↑                             |
| "svc3"| *elem |  ----→              +-------- 최근 접근 순서 ------+
| "svc5"| *elem |  ----→
+-------+-------+

O(1) 조회: byKey[key]
O(1) 갱신: MoveToFront(elem)
O(1) 퇴출: Remove(byAccess.Back())
```

이 조합은 모든 캐시 연산을 O(1)에 수행할 수 있게 해준다.

### 3.2 Get 연산

```go
// internal/cache/lru.go:43
func (c *LRU) Get(key string) any {
    c.mux.Lock()
    defer c.mux.Unlock()

    elt := c.byKey[key]
    if elt == nil {
        return nil  // 캐시 미스
    }

    cacheEntry := elt.Value.(*cacheEntry)

    // TTL 만료 검사
    if !cacheEntry.expiration.IsZero() && c.TimeNow().After(cacheEntry.expiration) {
        if c.onEvict != nil {
            c.onEvict(cacheEntry.key, cacheEntry.value)
        }
        c.byAccess.Remove(elt)
        delete(c.byKey, cacheEntry.key)
        return nil  // 만료됨, 캐시 미스 취급
    }

    c.byAccess.MoveToFront(elt)  // 접근 순서 갱신
    return cacheEntry.value
}
```

**Get 연산의 흐름:**

```
Get("svc1")
  |
  +-- byKey["svc1"] 조회
  |     |
  |     +-- nil → return nil (MISS)
  |     |
  |     +-- 존재 → expiration 검사
  |               |
  |               +-- 만료됨 → 삭제 + onEvict + return nil (EXPIRED)
  |               |
  |               +-- 유효 → MoveToFront + return value (HIT)
```

**왜 만료된 항목을 Get 시점에 삭제하는가? (Lazy Eviction)**

1. 별도의 만료 타이머/고루틴이 필요 없어 구현이 단순하다
2. 접근되지 않는 만료 항목은 LRU 퇴출로 자연스럽게 제거된다
3. 메모리 사용량이 maxSize로 상한이 정해져 있어 만료 항목이 쌓여도 문제가 없다
4. 백그라운드 스캔 없이도 Get 호출 시 항상 최신 데이터를 보장한다

### 3.3 Put 연산

```go
// internal/cache/lru.go:68
func (c *LRU) Put(key string, value any) any {
    c.mux.Lock()
    defer c.mux.Unlock()
    elt := c.byKey[key]
    return c.putWithMutexHold(key, value, elt)
}
```

```go
// internal/cache/lru.go:100
func (c *LRU) putWithMutexHold(key string, value any, elt *list.Element) any {
    if elt != nil {
        // 기존 항목 업데이트
        entry := elt.Value.(*cacheEntry)
        existing := entry.value
        entry.value = value
        if c.ttl != 0 {
            entry.expiration = c.TimeNow().Add(c.ttl)
        }
        c.byAccess.MoveToFront(elt)
        return existing  // 이전 값 반환
    }

    // 새 항목 삽입
    entry := &cacheEntry{key: key, value: value}
    if c.ttl != 0 {
        entry.expiration = c.TimeNow().Add(c.ttl)
    }
    c.byKey[key] = c.byAccess.PushFront(entry)

    // 용량 초과 시 가장 오래된 항목 퇴출
    for len(c.byKey) > c.maxSize {
        oldest := c.byAccess.Remove(c.byAccess.Back()).(*cacheEntry)
        if c.onEvict != nil {
            c.onEvict(oldest.key, oldest.value)
        }
        delete(c.byKey, oldest.key)
    }

    return nil  // 새 항목이므로 이전 값 없음
}
```

**Put 연산의 흐름:**

```
Put("svc1", "value1")
  |
  +-- byKey["svc1"] 조회
  |     |
  |     +-- 존재 → 값 업데이트 + TTL 갱신 + MoveToFront
  |     |          return 이전 값
  |     |
  |     +-- 없음 → 새 entry 생성 + PushFront
  |               |
  |               +-- len > maxSize?
  |                     |
  |                     +-- YES → Back() 제거 (반복)
  |                     |          onEvict 콜백 호출
  |                     |
  |                     +-- NO → return nil
```

### 3.4 CompareAndSwap 연산

```go
// internal/cache/lru.go:77
func (c *LRU) CompareAndSwap(key string, oldValue, newValue any) (itemInCache any, replaced bool) {
    c.mux.Lock()
    defer c.mux.Unlock()

    elt := c.byKey[key]

    // 항목 없음 + oldValue가 nil이 아님 → 불일치
    if elt == nil && oldValue != nil {
        return nil, false
    }

    if elt != nil {
        entry := elt.Value.(*cacheEntry)
        // 현재 값과 기대 값 비교
        if entry.value != oldValue {
            return entry.value, false  // 불일치 → 교체하지 않음
        }
    }

    c.putWithMutexHold(key, newValue, elt)
    return newValue, true
}
```

**왜 CompareAndSwap이 필요한가?**

동시에 여러 고루틴이 같은 캐시 항목을 업데이트할 때, 단순한 Get→업데이트→Put 시퀀스는
경쟁 조건(race condition)을 발생시킨다:

```
CAS 없이 (위험):                    CAS 사용 (안전):
Goroutine A: Get("svc1") → v1      Goroutine A: CAS("svc1", v1, v2) → ok
Goroutine B: Get("svc1") → v1      Goroutine B: CAS("svc1", v1, v3) → fail (v1 != v2)
Goroutine A: Put("svc1", v2)       Goroutine B: CAS("svc1", v2, v3) → ok (재시도)
Goroutine B: Put("svc1", v3)
→ A의 업데이트가 B에 의해 덮어씌워짐  → 순서가 보장됨
```

### 3.5 Delete 연산

```go
// internal/cache/lru.go:133
func (c *LRU) Delete(key string) {
    c.mux.Lock()
    defer c.mux.Unlock()

    elt := c.byKey[key]
    if elt != nil {
        entry := c.byAccess.Remove(elt).(*cacheEntry)
        if c.onEvict != nil {
            c.onEvict(entry.key, entry.value)
        }
        delete(c.byKey, key)
    }
}
```

삭제 시에도 `onEvict` 콜백을 호출한다. 이는 퇴출과 명시적 삭제를 동일하게 취급하여,
리소스 정리 로직을 한 곳에 모을 수 있게 해준다.

---

## 4. LRU 캐시 성능 특성

### 4.1 시간 복잡도

| 연산 | 시간 복잡도 | 설명 |
|------|------------|------|
| Get | O(1) | 해시맵 조회 + 리스트 이동 |
| Put | O(1) | 해시맵 삽입 + 리스트 삽입 |
| Delete | O(1) | 해시맵 삭제 + 리스트 제거 |
| CompareAndSwap | O(1) | Get + 조건부 Put |
| Size | O(1) | 해시맵 길이 반환 |

### 4.2 공간 복잡도

```
메모리 사용량 = maxSize × (cacheEntry + list.Element + map entry)

cacheEntry:
  key (string)     : ~50 bytes (평균 서비스 이름 길이)
  expiration (Time): 24 bytes
  value (any)      : 16 bytes (인터페이스 헤더) + 실제 값

list.Element:
  prev, next 포인터: 16 bytes
  Value (any)      : 16 bytes

map entry:
  키 (string)      : ~50 bytes
  값 (*Element)    : 8 bytes

총 항목당: ~180 bytes
maxSize=10,000 → ~1.8 MB
```

### 4.3 동시성 특성

```
잠금 전략: 단일 Mutex

장점:
  - 구현이 단순
  - 데드락 불가 (단일 잠금)
  - 모든 연산의 원자성 보장

단점:
  - 읽기-읽기 경합 (RWMutex가 아님)
  - 높은 동시성에서 잠금 경합

왜 RWMutex를 사용하지 않았는가?
  - Get이 MoveToFront를 수행하므로 읽기도 쓰기 연산
  - RWMutex를 사용하면 Get도 쓰기 잠금이 필요
  - 결국 RWMutex를 사용해도 성능 이득 없음
```

---

## 5. Jaeger에서 LRU 캐시의 활용

### 5.1 서비스 이름 캐시

```
스팬 입수 흐름:
  Receiver → [서비스 이름 캐시] → Storage

캐시 미스 시:
  Storage에 서비스 이름 쓰기 (중복 방지 위해 INSERT IF NOT EXISTS)

캐시 히트 시:
  Storage 쓰기 스킵 (이미 등록된 서비스)
```

### 5.2 오퍼레이션 이름 캐시

```
서비스별 오퍼레이션 이름을 캐싱:
  key: "serviceA" → value: Set{"GET /api", "POST /api", "GET /health"}

새 오퍼레이션 발견 시 CompareAndSwap으로 원자적 업데이트:
  1. Get("serviceA") → oldSet
  2. newSet = oldSet + "PUT /api"
  3. CompareAndSwap("serviceA", oldSet, newSet)
```

### 5.3 쿼리 서비스 결과 캐시

```
Query Service가 반복적인 트레이스 조회를 캐싱:
  key: "traceID:abc123" → value: *TraceData

TTL: 30초 (트레이스 데이터는 한 번 기록되면 변하지 않지만,
         진행 중인 트레이스는 새 스팬이 추가될 수 있으므로)
```

---

## 6. jiter 패키지 설계

### 6.1 배경: Go의 Iterator 패턴 진화

```
Go 1.22 이전:        Go 1.23 이후:
for i := range slice  for v, err := range seq  // iter.Seq2[V, error]
```

Jaeger의 `jiter` 패키지는 `iter.Seq2`를 활용한 유틸리티 함수를 제공한다.

### 6.2 CollectWithErrors

```go
// internal/jiter/iter.go:11
func CollectWithErrors[V any](seq iter.Seq2[V, error]) ([]V, error) {
    var result []V
    for v, err := range seq {
        if err != nil {
            return nil, err  // 첫 에러에서 즉시 중단
        }
        result = append(result, v)
    }
    return result, nil
}
```

**왜 이 함수가 필요한가?**

스토리지 백엔드에서 스팬을 읽을 때, 각 항목마다 에러가 발생할 수 있다.
표준 `slices.Collect`는 에러 처리를 지원하지 않으므로, Jaeger는 자체 구현이 필요했다.

```
사용 예시:

스토리지에서 트레이스 읽기:
  spans, err := jiter.CollectWithErrors(store.ReadSpans(ctx, traceID))

내부 동작:
  iter.Seq2[*Span, error] 순회:
    span1, nil  → result = [span1]
    span2, nil  → result = [span1, span2]
    span3, nil  → result = [span1, span2, span3]
    nil, ErrIO  → return nil, ErrIO  (즉시 중단)
```

### 6.3 FlattenWithErrors

```go
// internal/jiter/iter.go:22
func FlattenWithErrors[V any](seq iter.Seq2[[]V, error]) ([]V, error) {
    var result []V
    for v, err := range seq {
        if err != nil {
            return nil, err  // 첫 에러에서 즉시 중단
        }
        result = append(result, v...)  // 슬라이스를 평탄화
    }
    return result, nil
}
```

**왜 Flatten이 필요한가?**

```
배치 읽기 시나리오:

Elasticsearch에서 스팬을 페이지 단위로 읽을 때:
  Page 1: [span1, span2, span3]
  Page 2: [span4, span5]
  Page 3: ErrTimeout

FlattenWithErrors 사용:
  spans, err := jiter.FlattenWithErrors(store.ReadSpanPages(ctx, traceID))

  Page 1 → result = [span1, span2, span3]
  Page 2 → result = [span1, span2, span3, span4, span5]
  Page 3 → return nil, ErrTimeout
```

### 6.4 CollectWithErrors vs FlattenWithErrors 비교

```
입력 타입                      출력
+----------------------------+---------------------------+
| CollectWithErrors           |                          |
| iter.Seq2[V, error]        | ([]V, error)             |
|                             |                          |
| V1 → V2 → V3 → err        | [V1,V2,V3] 또는 err     |
+----------------------------+---------------------------+

+----------------------------+---------------------------+
| FlattenWithErrors           |                          |
| iter.Seq2[[]V, error]      | ([]V, error)             |
|                             |                          |
| [V1,V2] → [V3] → err      | [V1,V2,V3] 또는 err     |
+----------------------------+---------------------------+
```

| 함수 | 입력 원소 | 출력 | 사용 시나리오 |
|------|----------|------|-------------|
| CollectWithErrors | 단일 값 + 에러 | 슬라이스 | 한 번에 하나씩 읽기 |
| FlattenWithErrors | 슬라이스 + 에러 | 평탄화된 슬라이스 | 배치/페이지 단위 읽기 |

---

## 7. 제네릭과 에러 처리 전략

### 7.1 Go 제네릭 활용

```go
func CollectWithErrors[V any](seq iter.Seq2[V, error]) ([]V, error)
```

`[V any]` 타입 파라미터를 사용하여 다양한 타입에 적용 가능하다:

```
사용 예시:
  spans, err := jiter.CollectWithErrors[*Span](spanIterator)
  deps, err := jiter.CollectWithErrors[*Dependency](depIterator)
  services, err := jiter.CollectWithErrors[string](svcIterator)
```

### 7.2 Fail-Fast 에러 전략

**왜 첫 에러에서 즉시 중단하는가?**

1. **일관성**: 부분 결과를 반환하면 호출자가 불완전한 트레이스를 표시할 수 있다
2. **효율성**: 에러 발생 후 남은 항목을 읽어도 유효한 결과를 만들 수 없다
3. **단순성**: 에러 발생 시 즉시 nil을 반환하여 호출자의 에러 처리를 단순하게 만든다

```
대안적 접근과 비교:

Fail-Fast (Jaeger 선택):
  에러 → 즉시 nil, err 반환
  장점: 단순, 일관적
  단점: 부분 결과 없음

Best-Effort:
  에러 → 건너뛰고 계속 처리
  장점: 부분 결과 반환
  단점: 불완전한 데이터, 에러 누적 복잡

Collect-All-Errors:
  에러 → 수집하고 마지막에 반환
  장점: 모든 에러 정보 확보
  단점: 메모리 사용, 복잡성 증가
```

---

## 8. 캐시와 이터레이터의 상호작용

### 8.1 캐시된 결과의 이터레이터 변환

```
쿼리 흐름:
  1. 캐시 확인 → HIT → 즉시 반환
  2. 캐시 MISS → 스토리지 이터레이터 실행
  3. CollectWithErrors로 결과 수집
  4. 결과를 캐시에 저장
  5. 다음 동일 쿼리에서 캐시 HIT
```

```
코드 패턴 (개념적):

func (s *Service) FindTraces(ctx context.Context, query *Query) ([]*Trace, error) {
    cacheKey := query.CacheKey()

    // 1. 캐시 확인
    if cached := s.cache.Get(cacheKey); cached != nil {
        return cached.([]*Trace), nil
    }

    // 2. 스토리지에서 이터레이터로 읽기
    traces, err := jiter.CollectWithErrors(s.store.FindTraces(ctx, query))
    if err != nil {
        return nil, err
    }

    // 3. 캐시에 저장
    s.cache.Put(cacheKey, traces)

    return traces, nil
}
```

### 8.2 캐시 워밍업과 이터레이터

```
시스템 시작 시 캐시 워밍업:

1. 서비스 목록 로드
   services, err := jiter.CollectWithErrors(store.GetServices(ctx))
   for _, svc := range services {
       cache.Put(svc, true)
   }

2. 오퍼레이션 목록 로드
   for _, svc := range services {
       ops, err := jiter.CollectWithErrors(store.GetOperations(ctx, svc))
       cache.Put("ops:"+svc, ops)
   }
```

---

## 9. 성능 최적화 패턴

### 9.1 캐시 크기 튜닝

```
서비스 이름 캐시:
  일반적인 서비스 수: 50~500
  권장 maxSize: 1000
  권장 TTL: 5분

오퍼레이션 이름 캐시:
  서비스당 오퍼레이션: 20~200
  500 서비스 × 100 오퍼레이션 = 50,000 항목
  권장 maxSize: 100,000
  권장 TTL: 5분

쿼리 결과 캐시:
  활성 트레이스 수: 변동 크다
  권장 maxSize: 10,000
  권장 TTL: 30초
```

### 9.2 이터레이터 메모리 패턴

```
CollectWithErrors의 메모리 할당:
  var result []V              // 초기 nil 슬라이스
  result = append(result, v)  // append가 필요에 따라 확장

  항목 10개:  ~10 × sizeof(V) + 슬라이스 오버헤드
  항목 1000개: ~1000 × sizeof(V) + 슬라이스 오버헤드

FlattenWithErrors의 메모리 패턴:
  result = append(result, v...)  // 배치를 한번에 추가

  배치 3개 × 1000개 = 3000개
  - 중간 슬라이스 재할당 가능
  - append가 2의 거듭제곱으로 확장
```

### 9.3 OnEvict 콜백 활용

```go
// 퇴출 시 메트릭 기록
cache := cache.NewLRUWithOptions(1000, &cache.Options{
    TTL: 5 * time.Minute,
    OnEvict: func(key string, value any) {
        metrics.CacheEvictions.Inc()
        logger.Debug("cache entry evicted", zap.String("key", key))
    },
})
```

---

## 10. 다른 캐시 구현과의 비교

### 10.1 Jaeger LRU vs Go 표준 라이브러리

| 기능 | Jaeger LRU | sync.Map | map + RWMutex |
|------|-----------|----------|---------------|
| LRU 퇴출 | O | X | X |
| TTL | O | X | X |
| CAS | O | O | 수동 구현 |
| 고정 크기 | O | X | 수동 구현 |
| 퇴출 콜백 | O | X | X |
| 스레드 안전 | O | O | O |

### 10.2 Jaeger LRU vs 외부 라이브러리

| 기능 | Jaeger LRU | groupcache/lru | hashicorp/lru |
|------|-----------|---------------|---------------|
| TTL | O | X | X |
| CAS | O | X | X |
| 퇴출 콜백 | O | O | O |
| Sharded Lock | X | X | O |
| 제네릭 지원 | O (any) | O | O |

**왜 자체 구현을 선택했는가?**

1. TTL + LRU 조합이 필요하지만 대부분의 라이브러리는 둘 중 하나만 지원
2. CompareAndSwap이 동시 업데이트에 필수적
3. 외부 의존성 최소화 정책
4. 코드가 160줄로 작아서 유지보수 부담이 적음

---

## 11. 정리

### 11.1 핵심 설계 원칙

| 원칙 | LRU Cache | jiter |
|------|-----------|-------|
| 단순성 | 160줄 구현 | 31줄 구현 |
| 일반성 | any 타입 지원 | 제네릭 타입 파라미터 |
| 안전성 | Mutex 보호, CAS | Fail-Fast 에러 처리 |
| 테스트 가능성 | TimeNow 주입 | 표준 iter.Seq2 |
| 성능 | 모든 연산 O(1) | 최소 할당 |

### 11.2 관련 소스 파일 요약

| 파일 | 줄수 | 핵심 함수/타입 |
|------|------|---------------|
| `internal/cache/cache.go` | 51줄 | `Cache` 인터페이스, `Options`, `EvictCallback` |
| `internal/cache/lru.go` | 160줄 | `LRU`, `Get`, `Put`, `CompareAndSwap`, `Delete` |
| `internal/jiter/iter.go` | 31줄 | `CollectWithErrors`, `FlattenWithErrors` |

### 11.3 PoC 참조

- `poc-19-lru-cache/` -- LRU 캐시의 O(1) 연산, TTL 만료, 퇴출 콜백 시뮬레이션

---

*본 문서는 Jaeger 소스코드의 `internal/cache/` 및 `internal/jiter/` 디렉토리를 직접 분석하여 작성되었다.*
