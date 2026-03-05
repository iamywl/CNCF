# PoC 05: 알림 스케줄러

## 개요

Grafana Alerting의 스케줄러를 시뮬레이션한다.
스케줄러는 등록된 알림 규칙(AlertRule)을 주기적으로 평가하여
메트릭 값이 조건을 만족하면 알림을 발생시킨다.

## Grafana 알림 스케줄러 동작 원리

### 기본 개념

- **baseInterval**: 스케줄러의 기본 틱 간격 (기본값 10초)
- **itemFrequency**: 규칙 실행 빈도 = `rule.IntervalSeconds / baseInterval`
- **Tick 처리**: 매 틱마다 `tickNum % itemFrequency == 0`인 규칙을 실행
- **Jitter**: 동시 실행을 방지하기 위한 랜덤 지연
- **Staggered Execution**: `time.AfterFunc`로 분산 실행

### 스케줄링 공식

```
tickNum = (currentTime - startTime) / baseInterval
shouldEvaluate = tickNum % (rule.IntervalSeconds / baseInterval) == 0
jitter = hash(ruleUID) % baseInterval
```

### 평가 결과 상태

| 상태 | 설명 |
|------|------|
| Normal | 조건 미충족, 정상 상태 |
| Alerting | 조건 충족, 알림 발생 |
| Pending | 조건 충족, For 기간 대기 중 |
| NoData | 데이터 없음 |
| Error | 평가 에러 |

## 시뮬레이션 내용

1. AlertRule 구조체 및 스케줄러 구현
2. baseInterval 기반 틱 처리
3. itemFrequency 계산 및 규칙 선택
4. Jitter를 활용한 부하 분산
5. 메트릭 시뮬레이션 및 조건 평가
6. 지수 백오프 재시도 (최대 3회)
7. 30초간 3개 규칙 실행

## 실행

```bash
go run main.go
```

## 참고

- Grafana 소스: `pkg/services/ngalert/schedule/schedule.go`
- 기본 baseInterval: 10초
- 최소 규칙 간격: baseInterval과 동일
