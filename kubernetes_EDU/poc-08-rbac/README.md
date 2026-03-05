# PoC-08: RBAC 인가(Authorization)

## 개요

Kubernetes의 RBAC(Role-Based Access Control) 인가 메커니즘을 시뮬레이션한다.

RBAC는 Kubernetes API 서버에서 기본 인가 방식으로, 역할(Role)과 바인딩(Binding)을 통해 사용자/그룹/서비스 어카운트의 리소스 접근 권한을 제어한다.

## 실제 코드 참조

| 파일 | 역할 |
|------|------|
| `plugin/pkg/auth/authorizer/rbac/rbac.go` | RBACAuthorizer, RuleAllows 함수 |
| `staging/src/k8s.io/apiserver/pkg/authorization/authorizer` | Authorizer 인터페이스, Decision 타입 |

## 시뮬레이션하는 개념

### 1. RBAC 리소스 4종
- **Role**: 네임스페이스 범위의 권한 규칙 집합
- **ClusterRole**: 클러스터 범위의 권한 규칙 집합
- **RoleBinding**: Subject를 Role/ClusterRole에 연결 (네임스페이스 범위)
- **ClusterRoleBinding**: Subject를 ClusterRole에 연결 (클러스터 범위)

### 2. PolicyRule 매칭
- verb(동사) + apiGroup + resource + resourceName의 4중 AND 조건
- "*" 와일드카드로 모든 값에 매칭 가능
- ResourceNames가 비어있으면 모든 이름에 매칭

### 3. Subject 매칭
- User: username으로 직접 매칭
- Group: 사용자의 그룹 목록과 매칭
- ServiceAccount: `system:serviceaccount:<namespace>:<name>` 형식

### 4. 인가 체인
- Allow/Deny/NoOpinion 3단계 판정
- 체인 순서: Allow 또는 Deny면 즉시 결정, NoOpinion이면 다음 인가자

## 실행

```bash
go run main.go
```
