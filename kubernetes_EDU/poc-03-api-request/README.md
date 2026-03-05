# PoC-03: API Server 핸들러 체인

## 개요

쿠버네티스 API Server가 요청을 처리할 때 거치는 **미들웨어 체인**을 시뮬레이션한다.

```
HTTP 요청 → Authentication → Authorization(RBAC) → Admission(Mutating → Validating) → Storage
```

## 구현 내용

| 단계 | 역할 | 실패 시 | 실제 소스 위치 |
|------|------|--------|-------------|
| Authentication | 토큰 검증, 사용자 식별 | 401 Unauthorized | `apiserver/pkg/endpoints/filters/authentication.go` |
| Authorization | RBAC 규칙으로 인가 | 403 Forbidden | `plugin/pkg/auth/authorizer/rbac/rbac.go` |
| Mutating Admission | 기본값 주입 (SA, DNS 등) | 403 Forbidden | `plugin/pkg/admission/serviceaccount/` |
| Validating Admission | 정책 검증 (NS, 이미지 등) | 403 Forbidden | `plugin/pkg/admission/namespace/exists/` |
| Storage | etcd에 저장 | 409 Conflict | `apiserver/pkg/registry/generic/registry/store.go` |

## 테스트 시나리오

1. admin이 파드 생성 (전체 체인 통과)
2. developer가 파드 생성 (RBAC 허용)
3. viewer가 파드 생성 시도 (RBAC 거부 → 403)
4. viewer가 파드 목록 조회 (RBAC 허용)
5. 잘못된 토큰 (인증 실패 → 401)
6. 존재하지 않는 네임스페이스 (Validating Admission 거부)
7. 차단된 이미지 (ImagePolicy Admission 거부)
8. 파드 삭제

## 실행

```bash
go run main.go
```
