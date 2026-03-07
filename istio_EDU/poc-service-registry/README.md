# PoC 03: 서비스 레지스트리와 Aggregate 패턴

## 핵심 개념

Istio Pilot은 여러 소스(Kubernetes, ServiceEntry, Consul 등)에서 서비스 정보를 수집하여 **단일 통합 뷰**로 제공한다. **AggregateController**가 이 역할을 담당하며, 각 레지스트리를 `ServiceDiscovery` 인터페이스로 추상화한다.

이 PoC는 서비스 레지스트리의 핵심 패턴을 재현한다:

1. **ServiceDiscovery 인터페이스**: 레지스트리 추상화 (Services, GetService, InstancesByPort)
2. **KubernetesRegistry**: K8s Service/Endpoint를 읽어 서비스 제공
3. **ServiceEntryRegistry**: Istio ServiceEntry CRD로 외부 서비스 등록
4. **AggregateController**: 여러 레지스트리를 합쳐 통합 뷰 제공
5. **이벤트 전파**: 서비스 변경 이벤트를 핸들러로 상위에 전파
6. **ClusterVIP 병합**: 같은 hostname의 서비스가 여러 클러스터에 있으면 병합

## 실행 방법

```bash
cd poc-service-registry
go run main.go
```

## 예상 출력

- Kubernetes 레지스트리에서 서비스 등록/조회
- ServiceEntry 레지스트리에서 외부 서비스 등록
- AggregateController를 통한 통합 서비스 목록 조회
- 서비스 변경 이벤트 전파 시뮬레이션
- 포트별 인스턴스 조회 (InstancesByPort)

## Istio 소스코드 참조

| 파일 | 핵심 구조체/함수 |
|------|-----------------|
| `pilot/pkg/serviceregistry/aggregate/controller.go` | `Controller`, `Services()`, `GetService()` |
| `pilot/pkg/serviceregistry/instance.go` | `Instance` 인터페이스 |
| `pilot/pkg/model/service.go` | `Service`, `ServiceDiscovery`, `ServiceInstance` |
| `pilot/pkg/model/controller.go` | `Controller`, `ServiceHandler` |
| `pilot/pkg/serviceregistry/kube/controller/controller.go` | Kubernetes 서비스 레지스트리 |
