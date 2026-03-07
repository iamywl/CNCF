# PoC 13: Endpoint Builder & Locality-Aware 로드밸런싱

## 개요

Istio의 EndpointBuilder가 서비스 엔드포인트를 수집하고, locality(region/zone/subzone) 기반으로 우선순위를 결정하여 로드밸런싱하는 과정을 시뮬레이션한다.

## Istio 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pilot/pkg/xds/endpoints/endpoint_builder.go` | EndpointBuilder 구조체, generate(), filterIstioEndpoint() |
| `pilot/pkg/model/endpointshards.go` | EndpointShards, ShardKey, EndpointIndex |
| `pilot/pkg/networking/core/loadbalancer/loadbalancer.go` | ApplyLocalityLoadBalancer(), applyLocalityFailover() |
| `pilot/pkg/networking/util/util.go` | LbPriority() 함수 |
| `pilot/pkg/serviceregistry/kube/controller/endpoint_builder.go` | buildIstioEndpoint() |

## 핵심 알고리즘

### EndpointShards (멀티클러스터 샤딩)
- 서비스별로 여러 클러스터의 엔드포인트를 `ShardKey(Provider/Cluster)` 기준으로 분리 보관
- 각 레지스트리가 독립적으로 자신의 샤드를 업데이트
- `snapshotShards()`에서 모든 샤드를 합쳐 전체 엔드포인트 목록 생성

### 건강 상태 필터링
- `filterIstioEndpoint()`에서 Unhealthy, Terminating, Draining 엔드포인트 제거
- Draining은 persistent session 서비스에서만 유지

### LbPriority (Locality 우선순위)
```
region+zone+subzone 일치 → priority 0
region+zone 일치         → priority 1
region만 일치            → priority 2
locality 불일치          → priority 3
```

### Failover 동작
- 가장 높은 우선순위(낮은 번호) 엔드포인트에 트래픽 전송
- 해당 우선순위 엔드포인트 모두 장애 시 다음 우선순위로 failover
- Outlier detection이 활성화되어야 실제 failover 작동

## 시뮬레이션 시나리오

1. 3개 클러스터(us-west-1, us-east-1, eu-west-1)에 걸친 엔드포인트 샤딩
2. 건강 상태 필터링 (Unhealthy/Terminating/Draining 제거)
3. Locality별 그룹화 및 우선순위 계산
4. 정상 상태 요청 분산 (같은 zone+subzone 우선)
5. 장애 시 failover 동작
6. 서브셋 레이블 필터링 (version=v1)
7. 클러스터-로컬 서비스 (같은 클러스터만 허용)

## 실행

```bash
go run main.go
```
