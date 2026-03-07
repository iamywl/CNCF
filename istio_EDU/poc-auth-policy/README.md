# PoC 14: Authorization Policy 평가 시뮬레이션

## 개요

Istio의 AuthorizationPolicy가 요청을 평가하는 과정을 시뮬레이션한다. CUSTOM/DENY/ALLOW 평가 순서, AND/OR 로직 결합, 와일드카드 매칭, CIDR 블록 매칭을 포함한다.

## Istio 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pilot/pkg/security/authz/model/model.go` | Model, rule, ruleList, Generate() |
| `pilot/pkg/security/authz/builder/builder.go` | Builder, build(), CUSTOM/DENY/ALLOW 순서 |
| `pilot/pkg/security/authz/model/generator.go` | 조건별 generator (srcIP, destPort 등) |
| `pilot/pkg/security/authz/model/permission.go` | permission AND/OR/NOT 조합 |
| `pilot/pkg/security/authz/model/principal.go` | principal AND/OR/NOT 조합 |
| `pilot/pkg/security/authz/matcher/string.go` | 문자열 패턴 매칭 |

## 핵심 알고리즘

### 평가 순서 (Envoy RBAC 필터 체인)
```
CUSTOM -> DENY -> ALLOW -> 기본(deny/allow)
```

1. CUSTOM 정책 매칭 시: ext_authz로 외부 인가 서비스에 위임
2. DENY 정책 매칭 시: 즉시 403 거부
3. ALLOW 정책 매칭 시: 허용
4. ALLOW 정책 존재하나 미매칭: 기본 거부
5. ALLOW 정책 없음: 기본 허용

### AND/OR 로직 구조
```
Rules[] (OR)
  From[] (OR) -> Source 내부 필드 (AND) -> 값 목록 (OR)
  To[] (OR)   -> Operation 내부 필드 (AND) -> 값 목록 (OR)
  When[] (AND) -> Condition 내 values (OR), notValues (AND)
```

### 특수 케이스
- `rules: []` (빈 규칙): `rbacPolicyMatchNever`로 변환되어 절대 매칭 안 됨
- DENY 정책에서의 오류: 무시되어 더 넓은 거부 정책 생성 (안전 방향)
- ALLOW 정책에서의 오류: 해당 규칙 제거로 더 좁은 허용 정책 생성 (안전 방향)

## 시뮬레이션 시나리오

- CUSTOM 정책: `/admin/*` 경로를 외부 OAuth2 서비스에 위임
- DENY 정책: `untrusted` 네임스페이스 차단, 외부 IP의 내부 경로 차단
- ALLOW 정책: productpage/gateway 서비스 API 접근, 토큰 기반 POST 허용, 헬스체크 허용
- 10개 다양한 테스트 요청으로 정책 평가 결과 확인

## 실행

```bash
go run main.go
```
