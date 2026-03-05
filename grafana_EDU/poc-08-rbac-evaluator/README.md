# PoC 08: RBAC 평가기

## 개요

Grafana의 역할 기반 접근 제어(RBAC) 권한 평가 시스템을 시뮬레이션한다.
Grafana는 v8부터 세분화된 RBAC를 도입하여 Action + Scope 기반으로
리소스 접근을 제어한다. 기존의 Org Role(Viewer/Editor/Admin) 위에
세분화된 권한을 추가 부여할 수 있다.

## Grafana 실제 구조

RBAC 시스템은 `pkg/services/accesscontrol/` 디렉토리에 구현되어 있다.

핵심 파일:
- `pkg/services/accesscontrol/evaluator/evaluator.go` - 평가기 인터페이스
- `pkg/services/accesscontrol/models.go` - Permission, Role 모델
- `pkg/services/accesscontrol/scope.go` - Scope 매칭 로직
- `pkg/services/accesscontrol/middleware.go` - HTTP 미들웨어 연동

## 시뮬레이션 내용

1. **Permission 모델**: Action + Scope 기반 권한 정의
2. **Evaluator 인터페이스**: 권한 평가 계약
3. **EvalPermission**: 단일 Action + Scope 매칭
4. **EvalAll / EvalAny**: AND/OR 조합 로직
5. **Scope 와일드카드 매칭**: `dashboards:*`가 `dashboards:uid:abc`를 매칭
6. **ScopeAttributeResolver**: UID → 리소스 속성 변환
7. **팀 기반 권한**: 팀 소속에 따른 추가 권한

## 핵심 패턴

```
Permission = Action + Scope
  예: dashboards:read + dashboards:uid:abc123

EvalPermission("dashboards:read", "dashboards:uid:abc123")
  → 사용자 권한에서 Action이 일치하고, Scope가 매칭되는지 확인

EvalAll(eval1, eval2)  → eval1 AND eval2
EvalAny(eval1, eval2)  → eval1 OR eval2
```

## 실행

```bash
go run main.go
```

## 학습 포인트

- Action + Scope 기반 세분화된 접근 제어
- 인터페이스 기반 Evaluator 조합 패턴 (Composite 패턴)
- 와일드카드 Scope 매칭 알고리즘
- Org Role과 세분화된 RBAC의 공존 방식
