# PoC 06: 알림 상태 머신

## 개요

Grafana Alerting의 상태 전이 머신을 시뮬레이션한다.
알림 규칙의 평가 결과에 따라 상태가 전이되며,
`For` 기간, `KeepFiringFor`, `ResendDelay` 등의 메커니즘이 적용된다.

## 상태 전이 다이어그램

```
                    condition=true
          ┌─────────────────────────────┐
          │                             ▼
     ┌─────────┐  condition=true   ┌──────────┐  For elapsed  ┌───────────┐
     │ Normal  │──────────────────▶│ Pending  │──────────────▶│ Alerting  │
     └─────────┘                   └──────────┘               └───────────┘
          ▲                             │                          │
          │         condition=false     │   condition=false        │
          └─────────────────────────────┘   (or KeepFiringFor     │
          │                                  elapsed)             │
          └───────────────────────────────────────────────────────┘

          Any ──── no data ────▶ NoData
          Any ──── error ──────▶ Error
```

## 핵심 메커니즘

| 메커니즘 | 설명 |
|----------|------|
| For | Pending 상태를 유지해야 하는 최소 시간. 이 시간이 지나야 Alerting으로 전환 |
| KeepFiringFor | 조건이 해소된 후에도 일정 시간 Alerting 유지 |
| Stale Resolution | 시리즈가 사라지면 N번의 평가 후 자동 해소 |
| ResendDelay | Alertmanager로 재전송하는 최소 간격 (기본 30초) |

## 상태 전이 규칙

1. **Normal -> Pending**: 조건 충족, For 타이머 시작
2. **Pending -> Alerting**: For 기간 동안 계속 조건 충족
3. **Pending -> Normal**: For 기간 내에 조건 해소
4. **Alerting -> Normal**: 조건 해소 (KeepFiringFor 후)
5. **Any -> NoData**: 데이터 없음
6. **Any -> Error**: 평가 에러

## 실행

```bash
go run main.go
```

## 참고

- Grafana 소스: `pkg/services/ngalert/eval/eval.go`
- 상태 관리: `pkg/services/ngalert/state/manager.go`
