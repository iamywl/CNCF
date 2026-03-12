# PoC-10: Alerting Rules 상태 머신

## 개요

Prometheus 알림 규칙(Alerting Rule)의 **상태 머신**을 시뮬레이션한다. 원본 `rules/alerting.go`의 핵심 로직인 `AlertState` 전이, `holdDuration`(for), `keepFiringFor`, `needsSending`을 Go 표준 라이브러리만으로 재현한다.

## 원본 소스 위치

| 구성 요소 | 파일 | 라인 |
|-----------|------|------|
| AlertState (Inactive/Pending/Firing) | `rules/alerting.go` | 54-67 |
| Alert 구조체 | `rules/alerting.go` | 84-100 |
| needsSending() | `rules/alerting.go` | 102-113 |
| AlertingRule 구조체 | `rules/alerting.go` | 116-157 |
| Eval() 평가 로직 | `rules/alerting.go` | 382-546 |
| sendAlerts() | `rules/alerting.go` | 613-628 |
| resolvedRetention (15분) | `rules/alerting.go` | 378 |

## 핵심 개념

### 1. Alert 상태 머신

Prometheus 알림은 3가지 상태를 가진다:

```
                              조건 참 지속
                   조건 참     + for 경과        조건 해소
  ┌──────────┐  ─────────►  ┌──────────┐  ─────────►  ┌──────────┐
  │ Inactive │              │ Pending  │              │ Firing   │
  └──────────┘  ◄─────────  └──────────┘              └──────────┘
                 조건 해소                                  │
                 (즉시 삭제)                     조건 해소   │
                                              ─────────►   │
                                  ┌──────────┐             │
                                  │ Inactive │ ◄───────────┘
                                  │(Resolved)│  ResolvedAt 설정
                                  └──────────┘
```

- **Inactive**: 조건이 충족되지 않은 상태. 알림이 존재하지 않거나 해소됨
- **Pending**: 조건이 참이지만 `for` 기간을 아직 채우지 못한 상태
- **Firing**: 조건이 `for` 기간 이상 지속되어 Alertmanager에 전송되는 상태

### 2. holdDuration (for)

YAML 규칙의 `for` 필드에 해당한다. Pending에서 Firing으로 전환하기까지의 **대기 시간**이다.

```yaml
groups:
  - name: example
    rules:
      - alert: HighErrorRate
        expr: rate(http_errors_total[5m]) > 0.05
        for: 30s          # ← holdDuration
```

**왜 필요한가?** 일시적인 스파이크로 인한 오탐(false positive)을 방지한다. 조건이 `for` 기간 동안 **연속으로** 참이어야 Firing으로 전환된다. Pending 상태에서 조건이 해소되면 알림은 즉시 삭제된다 (Alertmanager에 전송되지 않음).

원본 코드 (alerting.go:521-524):
```go
if a.State == StatePending && ts.Sub(a.ActiveAt) >= r.holdDuration {
    a.State = StateFiring
    a.FiredAt = ts
}
```

### 3. keepFiringFor

조건이 해소된 후에도 알림을 **Firing 상태로 유지**하는 기간이다.

```yaml
rules:
  - alert: DiskAlmostFull
    expr: disk_usage_percent > 90
    for: 10s
    keep_firing_for: 30s  # ← 조건 해소 후 30초간 Firing 유지
```

**왜 필요한가?** 조건이 경계값 주변에서 진동(flapping)할 때 알림이 반복적으로 Firing/Resolved를 오가는 것을 방지한다. `KeepFiringSince` 타임스탬프를 기록하고, 해당 시점부터 `keepFiringFor` 기간이 지나야 Inactive로 전환한다.

원본 코드 (alerting.go:484-493):
```go
if a.State == StateFiring && r.keepFiringFor > 0 {
    if a.KeepFiringSince.IsZero() {
        a.KeepFiringSince = ts
    }
    if ts.Sub(a.KeepFiringSince) < r.keepFiringFor {
        keepFiring = true
    }
}
```

조건이 다시 참이 되면 `KeepFiringSince`를 리셋한다 (alerting.go:516).

### 4. 라벨 Fingerprint 기반 알림 식별

동일한 규칙에서 **서로 다른 라벨셋**의 알림은 독립적인 인스턴스로 관리된다. `active` 맵은 `map[uint64]*Alert` 형태로 라벨셋의 해시(fingerprint)를 키로 사용한다.

예: `rate(http_errors_total[5m]) > 0.05` 결과가 3개 인스턴스를 반환하면:
- `{instance="web-1"}` → fingerprint A → Alert 인스턴스 A
- `{instance="web-2"}` → fingerprint B → Alert 인스턴스 B
- `{instance="web-3"}` → fingerprint C → Alert 인스턴스 C

각 인스턴스는 독립적으로 Pending → Firing → Resolved 전이를 거친다.

### 5. needsSending & resendDelay

알림을 Alertmanager에 전송해야 하는지 판단하는 로직이다:

| 조건 | 전송 여부 |
|------|-----------|
| Pending 상태 | 전송 안 함 |
| Firing + LastSentAt이 zero | 최초 전송 |
| Firing + resendDelay 경과 | 재전송 |
| Resolved + ResolvedAt > LastSentAt | 즉시 전송 (해소 알림) |
| Resolved + 이미 전송됨 + resendDelay 미경과 | 전송 안 함 |

### 6. resolvedRetention

Resolved된 알림은 `active` 맵에서 **15분간 유지**된다 (alerting.go:378). 이유:
1. 네트워크 문제로 인한 Resolved 알림 유실 방지 (재전송 가능)
2. Alertmanager 장애/재시작 시 해소 알림 누락 방지

## 실행 방법

```bash
go run main.go
```

## 시뮬레이션 시나리오

| 시나리오 | 설명 |
|----------|------|
| 1. 기본 상태 전이 | Inactive → Pending(3회) → Firing → Resolved (for: 30s) |
| 2. keepFiringFor | 조건 해소 후 30초간 Firing 유지 후 Inactive 전환 |
| 3. 다중 인스턴스 | 3개 인스턴스가 독립적으로 상태 전이 |
| 4. needsSending | resendDelay 기반 전송/재전송/해소 전송 판단 |

## 구현 매핑

| PoC 구현 | 원본 코드 |
|----------|-----------|
| `AlertState` (Inactive/Pending/Firing) | `rules/alerting.go:54-67` |
| `Alert.needsSending()` | `rules/alerting.go:102-113` |
| `AlertingRule.Eval()` | `rules/alerting.go:382-546` |
| `AlertingRule.sendAlerts()` | `rules/alerting.go:613-628` |
| `Labels.Fingerprint()` | `model/labels.Labels.Hash()` |
| `resolvedRetention = 15min` | `rules/alerting.go:378` |
