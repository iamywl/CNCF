# PoC-04: Block Compaction (블록 컴팩션)

## 개요

Prometheus TSDB의 **블록 기반 저장**과 **LeveledCompactor의 레벨 컴팩션 전략**을 시뮬레이션하는 PoC이다.

Prometheus는 시계열 데이터를 시간 범위별 불변(immutable) 블록으로 나누어 저장한다. 작은 블록들은 주기적으로 더 큰 블록으로 병합(compaction)되며, 이를 통해 디스크 I/O를 줄이고 쿼리 성능을 향상시킨다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 핵심 개념

### 블록 라이프사이클

```
수집(Scrape)                    컴팩션(Compaction)              삭제(Retention)
    │                               │                              │
    ▼                               ▼                              ▼
┌────────┐    Flush     ┌─────────────────┐    Merge    ┌──────────────────┐
│  Head  │ ──────────→  │ Block (Level 0) │ ────────→   │ Block (Level 1+) │ ──→ 삭제
│ (메모리)│   2h 범위    │   2h, 불변       │  3개 병합   │  6h/18h, 불변     │
└────────┘              └─────────────────┘             └──────────────────┘
```

1. **Head (인메모리 버퍼)**: 수집된 샘플이 WAL과 함께 메모리에 보관됨
2. **Block (Level 0)**: Head 데이터가 `blockDuration`(기본 2시간)을 채우면 디스크 블록으로 플러시
3. **Block (Level 1+)**: LeveledCompactor가 인접 블록들을 병합하여 더 큰 블록 생성
4. **삭제**: `RetentionDuration`(기본 15일)을 초과한 블록은 통째로 삭제

### 컴팩션 전략 (LeveledCompactor)

Prometheus는 `ExponentialBlockRanges`로 컴팩션 범위를 결정한다:

```
ExponentialBlockRanges(minSize=2h, steps=3, stepSize=3)
→ ranges = [2h, 6h, 18h]
```

| 레벨 | 블록 범위 | 구성 | 설명 |
|------|----------|------|------|
| 0 | 2h | Head에서 직접 생성 | 기본 블록 |
| 1 | 6h | Level 0 블록 3개 병합 | 1차 컴팩션 |
| 2 | 18h | Level 1 블록 3개 병합 | 2차 컴팩션 |

### splitByRange 알고리즘

컴팩션 플래닝의 핵심은 `splitByRange` 함수이다 (`tsdb/compact.go:400`):

```
시간축을 범위(tr) 단위로 분할하고, 각 블록을 해당 슬롯에 배치:

t0 = tr * (minTime / tr)    ← 정렬된 시간 범위 시작점

시간축:  |---6h---|---6h---|---6h---|---6h---|
블록:    [2h][2h][2h] [2h][2h][2h] [2h][2h][2h] [2h][2h][2h]
그룹:    |--그룹1--| |--그룹2--| |--그룹3--| |--그룹4--|
              ↓            ↓            ↓
          Level 1      Level 1      Level 1
```

### selectDirs 전략

`selectDirs` (`tsdb/compact.go:332`)는 컴팩션 대상을 선정한다:

1. `ranges[1:]`부터 큰 범위 순으로 순회
2. `splitByRange`로 블록을 그룹핑
3. **그룹 내 2개 이상** 블록이 있고, **범위를 채우거나** 최고 시간 이전이면 컴팩션 대상
4. **최신 블록은 항상 제외** — 백업 윈도우를 보장하기 위함

### CompactBlockMetas 병합 규칙

블록 병합 시 (`tsdb/compact.go:441`):

```
새 블록:
  - Level   = max(원본 블록들의 Level) + 1
  - MinTime = min(원본 블록들의 MinTime)
  - MaxTime = max(원본 블록들의 MaxTime)
  - Sources = 원본 블록들의 Sources 합집합
  - Parents = 직접 부모 블록들의 목록
```

### Retention (보존 정책)

```
현재 시각: T
보존 기간: R (기본 15일)
기준선:   T - R

         삭제 대상              보존 대상
    ◄──────────────────►◄──────────────────────►
    |     MaxTime ≤ T-R  |                      |
    ├────────────────────┤──────────────────────►
                        T-R                     T
```

블록의 `MaxTime`이 `T - R` 이하이면 블록 전체를 삭제한다. 개별 샘플이 아닌 블록 단위 삭제이므로 매우 효율적이다.

## 실제 소스코드 참조

| 파일 | 함수/구조체 | 역할 |
|------|-----------|------|
| `tsdb/compact.go:41` | `ExponentialBlockRanges()` | 레벨별 컴팩션 범위 계산 |
| `tsdb/compact.go:80` | `LeveledCompactor` | 컴팩터 구현체 |
| `tsdb/compact.go:279` | `plan()` | 컴팩션 플래닝 (어떤 블록을 병합할지 결정) |
| `tsdb/compact.go:332` | `selectDirs()` | 병합 대상 블록 그룹 선정 |
| `tsdb/compact.go:400` | `splitByRange()` | 시간 범위별 블록 그룹핑 |
| `tsdb/compact.go:441` | `CompactBlockMetas()` | 블록 메타데이터 병합 |
| `tsdb/block.go:164` | `BlockMeta` | 블록 메타데이터 구조체 |
| `tsdb/block.go:313` | `Block` | 블록 구조체 |
| `tsdb/db.go:54` | `DefaultBlockDuration` | 기본 블록 범위 (2시간) |

## 데모 출력 예시

```
=== 1단계: 데이터 수집 ===
  [FLUSH] Head → BLOCK-0001 (범위: 01/01 00:00 ~ 01/01 02:00, 레벨: 0)
  [FLUSH] Head → BLOCK-0002 (범위: 01/01 02:00 ~ 01/01 04:00, 레벨: 0)
  ...

=== 2단계: 컴팩션 실행 ===
  [COMPACT] BLOCK-0001 + ... (3개) → BLOCK-0013 (레벨: 1, 6h)
  [COMPACT] BLOCK-0004 + ... (3개) → BLOCK-0014 (레벨: 1, 6h)
  [COMPACT] BLOCK-0013 + ... (2개) → BLOCK-0016 (레벨: 2, 12h)

타임라인 시각화:
L2 │[==================================]                                    │
L1 │                                   [=================]                  │
L0 │                                                     [=====[=====[=====]│
```

## 왜 이런 설계인가?

1. **불변 블록**: 블록이 불변이므로 동시성 제어가 단순하고, 스냅샷/백업이 쉬움
2. **레벨 컴팩션**: 작은 블록을 큰 블록으로 병합하여 블록 수를 줄이고 쿼리 시 스캔할 블록 수를 감소시킴
3. **최신 블록 제외**: 방금 생성된 블록을 컴팩션에서 제외하여 백업 윈도우를 보장
4. **블록 단위 Retention**: 개별 샘플이 아닌 블록 전체를 삭제하므로 O(1) 삭제 가능
5. **splitByRange 정렬**: 시간축을 범위 단위로 정렬하여 블록들이 자연스럽게 그룹핑되고, 범위를 채우지 않은 불완전 그룹은 병합하지 않음
