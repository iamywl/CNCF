# PoC-16: Stripe HashMap (Lock Striping 패턴)

## 개요

Prometheus TSDB의 Head 블록은 활성 시리즈를 메모리에 보관하며, 동시에 수백 개의 scrape goroutine이 시리즈를 읽고 쓴다. 단일 `sync.RWMutex`로 전체 맵을 보호하면 **lock contention**이 병목이 되어 처리량이 급락한다. 이를 해결하기 위해 Prometheus는 `stripeSeries` 패턴을 사용한다.

**소스 위치**: `tsdb/head.go` — `type stripeSeries struct`

## Lock Contention 문제

```
                 ┌──────────────────┐
goroutine 1 ────>│                  │
goroutine 2 ────>│  Single RWMutex  │──── 하나의 lock에 모든 goroutine이 경쟁
goroutine 3 ────>│                  │     → contention 증가 → 처리량 저하
goroutine N ────>│                  │
                 └──────────────────┘
```

N개의 goroutine이 동시에 접근하면:
- **Writer 간**: 상호 배제 (하나만 진입)
- **Writer-Reader 간**: Writer가 lock 잡으면 모든 Reader가 대기
- **Reader 간**: 동시 접근 가능하지만, Writer가 있으면 역시 대기

CPU 코어가 많을수록 contention이 심해지고, lock 획득 대기 시간이 전체 지연의 대부분을 차지하게 된다.

## Stripe 패턴의 해결 방식

```
                 ┌── stripe[0] ── lock[0] ── map[0]
goroutine 1 ────>│
                 ├── stripe[1] ── lock[1] ── map[1]
goroutine 2 ────>│
                 ├── stripe[2] ── lock[2] ── map[2]
goroutine 3 ────>│
                 ├── ...
                 │
                 └── stripe[N-1] ── lock[N-1] ── map[N-1]
```

하나의 큰 맵을 **N개의 작은 맵(stripe)**으로 분할하고, 각 stripe마다 독립적인 lock을 부여한다. 서로 다른 stripe에 접근하는 goroutine은 **lock 경쟁 없이 동시에** 작업할 수 있다.

## 핵심 설계 결정

### 1. Stripe 크기는 2의 거듭제곱

Prometheus의 실제 기본값: `DefaultStripeSize = 1 << 14` (16384)

```go
// tsdb/head.go
const DefaultStripeSize = 1 << 14
```

2의 거듭제곱을 사용하면 stripe 인덱스 계산에 **비트 AND** 연산을 쓸 수 있다:

```
stripe_index = ref & (size - 1)    // 비트 AND: 1 CPU 사이클
stripe_index = ref % size          // modulo:  수십 CPU 사이클
```

예시 (`size = 512`, `mask = 0x1FF`):
```
ref = 1024  → 1024 & 511 = 0    (stripe[0])
ref = 1025  → 1025 & 511 = 1    (stripe[1])
ref = 99999 → 99999 & 511 = 31  (stripe[31])
```

### 2. 이중 Sharding: ref와 hash

Prometheus의 `stripeSeries`는 두 가지 맵을 유지한다:

| 맵 | Sharding 기준 | 용도 |
|---|---|---|
| `series[]` | `ref & (size-1)` | ref(고유 ID)로 시리즈 조회 |
| `hashes[]` | `hash & (size-1)` | label hash로 시리즈 조회/생성 |

```go
// tsdb/head.go
type stripeSeries struct {
    size   int
    series []map[HeadSeriesRef]*memSeries  // ref 기준 sharding
    hashes []seriesHashmap                 // label hash 기준 sharding
    locks  []stripeLock                    // 공유 lock 배열
}
```

`setUnlessAlreadySet()`에서 새 시리즈 생성 시 **두 개의 stripe lock**을 순차적으로 잡는다:
1. `hash stripe lock` → hashes 맵에 등록
2. `ref stripe lock` → series 맵에 등록

### 3. Cache Line Padding

```go
// tsdb/head.go
type stripeLock struct {
    sync.RWMutex
    _ [40]byte  // padding
}
```

`sync.RWMutex`는 약 24바이트이다. 40바이트 padding을 추가하여 총 64바이트로 맞추면, 인접한 lock이 같은 CPU cache line(보통 64바이트)에 위치하지 않는다. 이는 **false sharing**을 방지한다:

- False sharing: 서로 다른 코어가 같은 cache line의 다른 변수를 수정하면, 실제로는 독립적인 데이터인데도 cache invalidation이 발생
- Padding으로 각 lock을 별도 cache line에 배치하면 이 문제를 제거

### 4. GC는 stripe 단위로 lock 획득

```go
// tsdb/head.go — gc() 메서드 패턴
for i := 0; i < s.size; i++ {
    s.locks[i].Lock()
    // stripe[i]의 시리즈만 검사/삭제
    s.locks[i].Unlock()
}
```

전체 맵을 한 번에 잠그지 않고, stripe 하나씩 lock을 잡고 처리한다. GC가 `stripe[3]`을 처리하는 동안 다른 goroutine은 `stripe[0]`, `stripe[1]`, ... 에 자유롭게 접근할 수 있다.

## 이 PoC에서 구현한 것

| 구조체 | 설명 |
|-------|------|
| `SingleLockMap` | 단일 `RWMutex` 기반 — 성능 비교 기준선 |
| `StripeSeries` | 512개 stripe, cache line padding, 이중 sharding |

| 데모 | 내용 |
|-----|------|
| [1] 삽입 | 100K 시리즈 삽입 시간 비교 |
| [2] 벤치마크 | 8 writers + 8 readers 동시 처리량 비교 |
| [3] 분포 | stripe별 시리즈 분포 균등성 확인 |
| [4] GC | stale 시리즈 마킹 후 GC 실행, 정확성 검증 |
| [5] 원리 | 비트 AND 인덱스 계산 과정 시각화 |

## 실행

```bash
go run main.go
```

## 실제 Prometheus에서의 효과

- 수백만 시리즈 환경에서 16384개 stripe를 사용하면, 평균적으로 각 stripe에 ~60-120개 시리즈만 존재
- 동시 scrape goroutine이 서로 다른 시리즈를 처리할 확률이 높으므로, lock 충돌 확률이 `1/16384`로 감소
- CPU 코어가 많은 서버일수록 stripe 패턴의 효과가 극대화됨
