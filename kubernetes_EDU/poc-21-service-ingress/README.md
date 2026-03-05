# PoC 21: Service & Ingress 시뮬레이션

## 개요
Kubernetes Service 네트워킹과 Ingress 라우팅의 핵심 메커니즘을 시뮬레이션합니다.

## 다루는 개념
- **ClusterIP Allocator**: ServiceCIDR 범위에서 IP 할당
- **NodePort Allocator**: 30000-32767 포트 범위 관리
- **Service 타입**: ClusterIP, NodePort, LoadBalancer, ExternalName, Headless
- **EndpointSlice**: 최대 100 endpoints/slice 분할
- **Ingress**: PathType(Exact/Prefix), Host 기반 라우팅
- **ExternalTrafficPolicy**: Cluster vs Local

## 실행
```bash
go run main.go
```

## 참조 소스코드
| 기능 | 파일 |
|------|------|
| IP 할당 | `pkg/registry/core/service/ipallocator/ipallocator.go` |
| Port 할당 | `pkg/registry/core/service/portallocator/allocator.go` |
| Service 할당 | `pkg/registry/core/service/storage/alloc.go` |
| EndpointSlice | `pkg/controller/endpointslice/endpointslice_controller.go` |
| Ingress 타입 | `pkg/apis/networking/types.go` |
