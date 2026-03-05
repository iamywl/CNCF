# PoC 24: Cloud Controller Manager 시뮬레이션

## 개요
Kubernetes Cloud Controller Manager(CCM)의 핵심 컨트롤러들을 시뮬레이션합니다.

## 다루는 개념
- **Plugin Registry**: Cloud Provider 등록/초기화
- **Cloud Node Controller**: 노드 초기화 (ProviderID, Zone/Region)
- **Node Lifecycle Controller**: 클라우드 인스턴스 존재/종료 확인
- **Service Controller**: LoadBalancer 타입 Service의 LB 프로비저닝
- **Route Controller**: Pod CIDR 라우트 관리

## 실행
```bash
go run main.go
```

## 참조 소스코드
| 기능 | 파일 |
|------|------|
| Interface | `staging/src/k8s.io/cloud-provider/cloud.go` |
| Node Controller | `staging/src/k8s.io/cloud-provider/controllers/node/node_controller.go` |
| Service Controller | `staging/src/k8s.io/cloud-provider/controllers/service/controller.go` |
| Route Controller | `staging/src/k8s.io/cloud-provider/controllers/route/route_controller.go` |
| Plugin | `staging/src/k8s.io/cloud-provider/plugins.go` |
