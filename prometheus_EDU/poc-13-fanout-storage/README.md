# PoC-13: Fanout Storage

## 개요

Prometheus의 **Fanout Storage** 패턴을 시뮬레이션한다. Fanout Storage는 하나의 `Storage` 인터페이스 뒤에 primary(로컬 TSDB)와 여러 secondary(원격 스토리지)를 배치하여, 쓰기는 모든 스토리지에 팬아웃하고 읽기는 결과를 병합(merge)하는 구조이다.

**원본 코드**: `storage/fanout.go`, `storage/merge.go`

## Fanout 아키텍처

```
                    ┌─────────────────┐
                    │  FanoutStorage   │
                    │                  │
                    │  Appender()      │──→ fanoutAppender
                    │  Querier()       │──→ MergeQuerier
                    └────────┬─────────┘
                             │
                ┌────────────┼────────────┐
                │            │            │
                ▼            ▼            ▼
        ┌──────────┐  ┌──────────┐  ┌──────────┐
        │ Primary   │  │Secondary │  │Secondary │
        │ (Local    │  │ (Remote  │  │ (Remote  │
        │  TSDB)    │  │  Write)  │  │  Read)   │
        └──────────┘  └──────────┘  └──────────┘
             │              │              │
         필수(MUST)     best-effort    best-effort
```

## Primary vs Secondary 시맨틱

이것이 Fanout Storage의 핵심 설계 결정이다:

| 구분 | Primary (로컬 TSDB) | Secondary (원격 스토리지) |
|------|---------------------|--------------------------|
| **역할** | 데이터의 신뢰할 수 있는 소스 | 장기 보관, 글로벌 뷰 |
| **쓰기 실패** | **전체 실패** — 즉시 에러 반환 | **경고만** — 계속 진행 |
| **읽기 실패** | **전체 실패** — Querier 생성 불가 | **경고만** — 해당 소스 건너뜀 |
| **Commit 실패** | 나머지 secondary Rollback | 로그 기록 후 계속 |

### 왜 이런 설계인가?

Prometheus는 **로컬 TSDB를 항상 사용 가능한 데이터 소스로 보장**한다. 원격 스토리지(Cortex, Thanos, Mimir 등)는 네트워크 장애에 취약하므로 best-effort로 처리한다. 이렇게 하면:

1. 원격 스토리지 장애가 메트릭 수집을 중단시키지 않음
2. 로컬에서 항상 최근 데이터를 조회할 수 있음
3. 원격이 복구되면 자동으로 다시 데이터를 받을 수 있음

## 쓰기 흐름 (Fanout Appender)

```
fanoutAppender.Append(labels, t, v)
    │
    ├─→ primary.Append(labels, t, v)
    │     └─ 실패 시 → return error (전체 실패)
    │
    ├─→ secondary[0].Append(labels, t, v)
    │     └─ 실패 시 → WARNING 출력 (계속 진행)
    │
    └─→ secondary[1].Append(labels, t, v)
          └─ 실패 시 → WARNING 출력 (계속 진행)

fanoutAppender.Commit()
    │
    ├─→ primary.Commit()
    │     └─ 실패 시 → secondary 전부 Rollback → return error
    │
    ├─→ secondary[0].Commit()
    │     └─ 실패 시 → WARNING 출력
    │
    └─→ secondary[1].Commit()
```

## 읽기 흐름 (MergeQuerier)

```
FanoutStorage.Querier(mint, maxt)
    │
    ├─→ primary.Querier(mint, maxt)
    │     └─ 실패 시 → return error (전체 실패)
    │
    └─→ secondary.Querier(mint, maxt)
          └─ 실패 시 → WARNING, 건너뜀
    │
    └─→ NewMergeQuerier(primary, secondaries)

MergeQuerier.Select(matchers...)
    │
    ├─→ primary.Select(matchers...)  → SeriesSet A
    ├─→ secondary.Select(matchers...) → SeriesSet B
    │
    └─→ 병합:
         - 동일 레이블 시리즈 → 샘플 합침
         - 동일 타임스탬프 → 중복 제거 (primary 우선)
         - 레이블 기준 정렬하여 반환
```

## 데모 시나리오

| # | 시나리오 | 검증 포인트 |
|---|---------|------------|
| 1 | 정상 쓰기 | local + remote 모두에 기록됨 |
| 2 | 정상 읽기 | MergeQuerier가 양쪽 결과를 병합, 중복 제거 |
| 3 | Remote 쓰기 실패 | 경고만 출력, local에는 정상 기록 |
| 4 | Local 쓰기/읽기 실패 | 전체 실패, 즉시 에러 반환 |
| 5 | Remote에만 있는 데이터 | MergeQuerier가 remote 결과도 반환 |
| 6 | 양쪽 데이터 중복 | 타임스탬프 기준 중복 제거 |
| 7 | Remote Querier 실패 | 경고 후 local 결과만 반환 |

## 실행 방법

```bash
cd poc-13-fanout-storage
go run main.go
```

## 원본 코드 참조

| 파일 | 설명 |
|------|------|
| `storage/fanout.go` | FanoutStorage, fanoutAppender 구현 |
| `storage/merge.go` | MergeQuerier, 시리즈 병합 로직 |
| `storage/storage.go` | Storage, Appender, Querier 인터페이스 정의 |
