# PoC 15: 이벤트 버스

## 목적

Grafana의 이벤트 발행/구독(Publish/Subscribe) 패턴을 시뮬레이션한다.
대시보드 저장, 패널 데이터 변경, 시간 범위 변경, 알림 상태 변경 등
시스템 내부 이벤트를 비동기적으로 전달하고 처리하는 구조를 구현한다.

## Grafana 실제 구현 참조

Grafana는 `pkg/bus/` 및 `pkg/services/` 패키지에서 이벤트 기반 통신을 사용한다:

- **Bus**: 이벤트/명령의 발행과 구독을 중개하는 중앙 버스
- **InProcBus**: 프로세스 내 동기/비동기 메시지 전달
- **Event**: 시스템 내 발생한 사건을 나타내는 인터페이스

## 핵심 개념

### 이벤트 흐름

```
┌──────────────┐     Publish     ┌──────────────┐     Deliver     ┌──────────────┐
│   Publisher   │ ──────────────► │   EventBus   │ ──────────────► │  Subscriber  │
│              │                 │              │                 │  (Handler)   │
│ DashboardSvc │                 │ ┌──────────┐ │                 ├──────────────┤
│ AlertSvc     │                 │ │ handlers │ │                 │ Audit Logger │
│ PanelSvc     │                 │ │ map[type] │ │                 │ Notification │
│              │                 │ │ []handler │ │                 │ Cache Update │
└──────────────┘                 │ └──────────┘ │                 └──────────────┘
                                 │              │
                                 │ ┌──────────┐ │
                                 │ │ wildcard │ │ ← 모든 이벤트 수신
                                 │ │ handlers │ │
                                 │ └──────────┘ │
                                 └──────────────┘
```

### 이벤트 타입 계층

```
Event (interface)
  ├── DashboardSavedEvent     (대시보드 저장됨)
  ├── PanelDataChangedEvent   (패널 데이터 변경됨)
  ├── TimeRangeChangedEvent   (시간 범위 변경됨)
  └── AlertStateChangedEvent  (알림 상태 변경됨)
```

### 전달 모드

| 모드 | 설명 | 사용 사례 |
|------|------|----------|
| 동기 | Publish가 모든 핸들러 완료까지 대기 | 트랜잭션 내 이벤트 |
| 비동기 | Publish 즉시 반환, 고루틴으로 전달 | 알림, 로깅, 캐시 갱신 |
| 와일드카드 | 모든 이벤트 타입 수신 | 감사 로그, 메트릭 수집 |

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== Grafana 이벤트 버스 시뮬레이션 ===

[구독] AuditLogger → *(와일드카드)
[구독] CacheInvalidator → DashboardSaved
[구독] NotificationSvc → AlertStateChanged
[구독] MetricsCollector → *(와일드카드)

[발행] DashboardSaved (dashboard=Production Overview, user=admin)
  → [비동기] AuditLogger: DashboardSaved 처리
  → [비동기] CacheInvalidator: DashboardSaved 처리
  → [비동기] MetricsCollector: DashboardSaved 처리

[발행] AlertStateChanged (alert=HighCPU, state=firing)
  → [비동기] AuditLogger: AlertStateChanged 처리
  → [비동기] NotificationSvc: AlertStateChanged 처리
  → [비동기] MetricsCollector: AlertStateChanged 처리
```

## 학습 포인트

1. **이벤트 인터페이스**: Type()과 Timestamp()로 통일된 이벤트 계약
2. **비동기 전달**: 고루틴 기반으로 발행자가 구독자 처리를 기다리지 않음
3. **와일드카드 구독**: 모든 이벤트를 수신하여 감사 로그/메트릭 수집
4. **이벤트 필터링**: 소스별 필터로 관심 있는 이벤트만 처리
5. **구독 해제**: Unsubscribe로 동적 구독 관리
