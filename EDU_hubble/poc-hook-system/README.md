# PoC: Hubble Hook/Plugin 패턴

> **관련 문서**: [07-CODE-GUIDE.md](../07-CODE-GUIDE.md) - Hook/Plugin 패턴

## 이 PoC가 보여주는 것

Hubble Observer의 **Hook 체인** 메커니즘을 실행해볼 수 있습니다.

```
Flow → [Metrics Hook] → [RateLimiter Hook] → [AuditLog Hook] → 전달
         stop=false        stop=true일 수 있음    stop=false
         (항상 통과)        (제한 초과 시 중단)     (감사 기록)
```

## 실행 방법

```bash
cd EDU/poc-hook-system
go run main.go
```

## 관찰할 수 있는 것

1. 5개 Flow가 3개 Hook 체인을 통과하는 과정
2. **Metrics Hook**: 모든 Flow를 카운트 (항상 stop=false)
3. **RateLimiter Hook**: 초당 3개 초과 시 stop=true → 이후 Hook 실행 안 됨
4. **AuditLog Hook**: DROPPED Flow만 감사 로그 기록

## 핵심 학습 포인트

- **stop=true 반환**: 이후 Hook이 **전부 스킵**됨 (체인 중단)
- **Hook 순서가 중요**: Metrics를 먼저 두면 차단된 Flow도 카운트됨
- **인터페이스 기반**: 새 기능 추가 시 Observer 코어 코드 변경 불필요
- **실제 Hubble**: 7개 확장 포인트 (OnServerInit, OnMonitorEvent, OnDecodedFlow, ...)
