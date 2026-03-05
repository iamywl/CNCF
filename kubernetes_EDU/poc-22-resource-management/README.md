# PoC 22: Resource Management 시뮬레이션

## 개요
Kubernetes의 리소스 관리 메커니즘(LimitRange, ResourceQuota)을 시뮬레이션합니다.

## 다루는 개념
- **LimitRange Defaulting**: 컨테이너 기본 requests/limits 자동 설정
- **LimitRange Validation**: Min/Max 제약 검증
- **ResourceQuota**: 네임스페이스별 리소스 총량 제한
- **Admission 흐름**: LimitRange Admit → Validate → ResourceQuota Check

## 실행
```bash
go run main.go
```

## 참조 소스코드
| 기능 | 파일 |
|------|------|
| LimitRange | `plugin/pkg/admission/limitranger/admission.go` |
| ResourceQuota | `staging/src/k8s.io/apiserver/pkg/admission/plugin/resourcequota/controller.go` |
| Pod 평가자 | `pkg/quota/v1/evaluator/core/pods.go` |
