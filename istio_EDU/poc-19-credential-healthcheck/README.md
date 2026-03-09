# PoC-19: Credential 관리 + 헬스체크 프록시 시뮬레이션

## 관련 문서
- [22-credential-healthcheck.md](../../istio_EDU/22-credential-healthcheck.md)

## 시뮬레이션 내용
1. **CredentialsController**: Kubernetes Secret에서 TLS 인증서 추출 (TLS/Opaque 유형)
2. **AggregateController**: 멀티클러스터 인증서 통합 조회
3. **SubjectAccessReview**: ServiceAccount 기반 Secret 접근 권한 검증
4. **WorkloadHealthChecker**: HTTP/TCP 프로브 실행 + 연속 성공/실패 임계값
5. **헬스 이벤트**: 상태 변경 시만 이벤트 발생 (플래핑 방지)

## 참조 소스
- `pilot/pkg/credentials/kube/secrets.go`
- `pilot/pkg/credentials/kube/multicluster.go`
- `pkg/istio-agent/health/health_probers.go`

## 실행
```bash
go run main.go
```
