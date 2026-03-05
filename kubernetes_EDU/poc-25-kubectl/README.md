# PoC 25: kubectl 내부 구조 시뮬레이션

## 개요
kubectl의 핵심 내부 구조(Cobra, Factory, Apply, Printer, Plugin)를 시뮬레이션합니다.

## 다루는 개념
- **Cobra 명령 구조**: 계층적 CLI 명령 트리
- **Factory 패턴**: kubeconfig → REST Config → Client 추상화
- **kubectl apply**: Client-Side Apply vs Server-Side Apply
- **Resource Printer**: Table/JSON/CustomColumns 출력 형식
- **Plugin 메커니즘**: PATH 기반 kubectl-* 바이너리 검색

## 실행
```bash
go run main.go
```

## 참조 소스코드
| 기능 | 파일 |
|------|------|
| Cobra 구조 | `staging/src/k8s.io/kubectl/pkg/cmd/cmd.go` |
| Apply | `staging/src/k8s.io/kubectl/pkg/cmd/apply/apply.go` |
| Get | `staging/src/k8s.io/kubectl/pkg/cmd/get/get.go` |
| Plugin | `staging/src/k8s.io/kubectl/pkg/cmd/plugin.go` |
| Factory | `staging/src/k8s.io/kubectl/pkg/cmd/util/factory.go` |
