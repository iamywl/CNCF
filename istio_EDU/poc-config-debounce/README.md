# PoC: 설정 변경 디바운싱과 병합

## 개요

Istio Pilot(istiod)은 Kubernetes로부터 설정 변경 이벤트(VirtualService, DestinationRule, Gateway 등)를 수신할 때마다 xDS 푸시를 트리거한다. 대규모 클러스터에서는 짧은 시간에 수백 개의 설정 변경이 발생할 수 있으므로, 매번 푸시하면 CPU/메모리 사용량이 급증하고 Envoy에 과부하가 걸린다.

이를 방지하기 위해 Pilot은 **디바운싱(debouncing)** 메커니즘을 사용하여 여러 설정 변경을 하나의 xDS 푸시로 병합한다.

이 PoC는 그 핵심 알고리즘을 Go 표준 라이브러리만으로 시뮬레이션한다.

## 디바운스 알고리즘

Istio의 `debounce()` 함수(`pilot/pkg/xds/discovery.go:343-425`)가 구현하는 핵심 알고리즘:

```
시간 →

이벤트:    E1      E2    E3         E4
          │       │     │          │
          v       v     v          v
──────────┼───────┼─────┼──────────┼────────────────
          │       │     │          │
타이머:   [===]   │     │          │
          리셋→  [===]  │          │
                 리셋→ [===]       │
                       리셋→ [=======]→ PUSH!
                                        │
병합:     E1 → E1+E2 → E1+E2+E3 → E1+E2+E3+E4
```

### 핵심 변수

| 변수 | 역할 |
|------|------|
| `DebounceAfter` | quiet period (기본 100ms) — 마지막 이벤트 후 이 시간 동안 새 이벤트 없으면 푸시 |
| `debounceMax` | 최대 대기 시간 (기본 10s) — 이벤트가 계속 들어와도 이 시간 초과 시 강제 푸시 |
| `timeChan` | DebounceAfter 타이머 채널 |
| `startDebounce` | 디바운스 시작 시간 (첫 이벤트 도착 시각) |
| `lastConfigUpdateTime` | 마지막 이벤트 도착 시각 |
| `req` | 병합된 PushRequest |
| `free` | 이전 푸시가 완료되었는지 여부 (동시 푸시 방지) |

### PushRequest.Merge() 규칙

`pilot/pkg/model/push_context.go:492-534`에서 정의된 병합 규칙:

| 필드 | 병합 방식 | 이유 |
|------|----------|------|
| `Start` | 더 이른(오래된) 시간 유지 | 첫 이벤트부터의 지연 시간 측정 |
| `Full` | OR 연산 | 하나라도 Full이면 전체 재계산 필요 |
| `Forced` | OR 연산 | 하나라도 강제 푸시면 강제 실행 |
| `ConfigsUpdated` | 합집합 | 모든 변경된 설정 포함 |
| `Reason` | 카운트 합산 | 과소 집계 방지 (중복 제거하지 않음) |

## 시뮬레이션 시나리오

### 시나리오 1: 기본 디바운싱

50ms 간격으로 5개 설정 변경 전송. quiet period(100ms) 내에 모든 이벤트 도착하므로 1회 푸시로 병합.

### 시나리오 2: 타이머 리셋

100ms 간격으로 4개 이벤트 전송. quiet period(150ms) 미충족 시 타이머가 리셋되어 마지막 이벤트 후 150ms에 1회 푸시.

### 시나리오 3: DebounceMax 강제 푸시

80ms 간격으로 10개 이벤트 연속 전송. quiet period 충족이 불가능하지만 debounceMax(500ms) 경과 시 강제 푸시. 이후 남은 이벤트는 별도 푸시.

### 시나리오 4: EDS 즉시 푸시

`enableEDSDebounce=false`일 때 `Full=false`(EDS/incremental) 이벤트는 디바운스를 건너뛰고 즉시 푸시. 엔드포인트 변경은 빠르게 반영해야 하므로 이렇게 설계되었다.

### 시나리오 5: Merge() 검증

3개의 PushRequest를 순차 병합하여 Full(OR), Forced(OR), ConfigsUpdated(합집합), Reason(카운트 합산) 동작을 검증.

## 실행 방법

```bash
cd istio_EDU/poc-config-debounce
go run main.go
```

## 예상 출력

5개 시나리오가 순차적으로 실행되며, 각각:

1. **기본 디바운싱**: 5개 이벤트 -> 1회 푸시 (5개 병합)
2. **타이머 리셋**: 4개 이벤트 -> 1회 푸시, 대기 시간 ~450ms
3. **DebounceMax 강제 푸시**: 10개 이벤트 -> 2회 푸시 (7개 maxwait + 3개 quiet)
4. **EDS 즉시 푸시**: 6개 이벤트 -> 4회 푸시 (EDS 3회 즉시 + Full 1회 디바운스)
5. **Merge 검증**: Full=true, Forced=true, ConfigsUpdated=4, Reason 카운트 합산

## Istio 소스 참조

| 파일 | 함수/구조체 | 역할 |
|------|-----------|------|
| `pilot/pkg/xds/discovery.go` | `DebounceOptions` | quiet period, max wait, EDS 디바운스 설정 |
| `pilot/pkg/xds/discovery.go` | `debounce()` | 핵심 디바운스 루프 (이벤트 수신, 타이머, 병합, 푸시) |
| `pilot/pkg/model/push_context.go` | `PushRequest` | 푸시 요청 구조체 (Full, ConfigsUpdated, Reason 등) |
| `pilot/pkg/model/push_context.go` | `PushRequest.Merge()` | 두 PushRequest를 병합하는 핵심 로직 |
| `pilot/pkg/model/push_context.go` | `PushRequest.CopyMerge()` | 입력을 변경하지 않는 안전한 병합 (프록시 컨텍스트용) |
| `pkg/util/concurrent/debouncer.go` | `Debouncer[T].Run()` | 범용 디바운서 (sets.Set 기반 이벤트 병합) |

## Istio 디바운스 관련 설정

Pilot 환경 변수로 디바운스 동작을 제어할 수 있다:

| 환경 변수 | 기본값 | 설명 |
|----------|--------|------|
| `PILOT_DEBOUNCE_AFTER` | 100ms | quiet period |
| `PILOT_DEBOUNCE_MAX` | 10s | 최대 대기 시간 |
| `PILOT_ENABLE_EDS_DEBOUNCE` | true | EDS 이벤트 디바운스 여부 |
