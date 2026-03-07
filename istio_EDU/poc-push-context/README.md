# PoC 10: PushContext 스냅샷과 인덱싱

## 핵심 개념

Istio Pilot(istiod)의 **PushContext**는 xDS 푸시 시점의 모든 설정을 사전 계산한 **불변 스냅샷**이다. 설정 변경이 발생하면 새 PushContext를 생성하고, 서비스/VirtualService/DestinationRule을 인덱싱하여 수천 개 프록시에 동시에 일관된 설정을 전달한다.

이 PoC는 PushContext의 핵심 메커니즘을 재현한다:

1. **불변 스냅샷 생성**: 설정 변경 시 새 PushContext를 생성하고 모든 인덱스를 재구축
2. **서비스 인덱싱**: `serviceIndex`를 통한 가시성(exportTo) 기반 네임스페이스별 필터링
3. **VirtualService/DestinationRule 인덱싱**: 호스트명 기반 빠른 조회
4. **SidecarScope 필터링**: 프록시에 필요한 서비스만 선택
5. **동시성 안전**: 불변 스냅샷으로 동시 읽기 보장

## 실행 방법

```bash
cd poc-push-context
go run main.go
```

## 예상 출력

- PushContext 생성 및 서비스 인덱싱 과정
- exportTo 가시성에 따른 서비스 필터링 결과
- VirtualService/DestinationRule 인덱스 조회
- SidecarScope 기반 서비스 선택
- 불변 스냅샷의 동시 읽기 검증

## Istio 소스코드 참조

| 파일 | 핵심 구조체/함수 |
|------|-----------------|
| `pilot/pkg/model/push_context.go` | `PushContext`, `serviceIndex`, `virtualServiceIndex`, `destinationRuleIndex` |
| `pilot/pkg/model/push_context.go` | `initServiceRegistry()`, `privateByNamespace`, `public`, `exportedToNamespace` |
| `pilot/pkg/model/sidecar.go` | `SidecarScope`, `services()` |
| `pkg/config/model.go` | `Config`, `Meta`, `GroupVersionKind` |
