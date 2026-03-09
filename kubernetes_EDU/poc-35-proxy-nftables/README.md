# PoC 35: nftables 프록시 모드 및 Topology Aware Routing 시뮬레이션

## 개요

kube-proxy의 nftables 프록시 모드와 Topology Aware Routing의 핵심 알고리즘을
Go 표준 라이브러리만으로 시뮬레이션한다. 실제 Kubernetes 소스코드의 데이터 구조와
알고리즘을 충실하게 재현하여 동작 원리를 이해할 수 있도록 한다.

## 실행 방법

```bash
cd poc-35-proxy-nftables
go run main.go
```

## 시뮬레이션 항목

### 1. nftables 테이블/체인/규칙 구조 (데모 1, 8)

`kube-proxy` 테이블의 전체 구조를 모델링한다:
- **8개 Base Chain**: netfilter 훅에 직접 연결되는 체인 (filter 5 + nat 3)
- **7개 Regular Chain**: jump으로 호출되는 체인 (services, masquerading 등)
- **Map/Set**: O(1) 해시 기반 서비스 디스패치

대응 소스: `pkg/proxy/nftables/proxier.go` (라인 55~97, 342~385)

### 2. Service-to-Endpoint DNAT 매핑 (데모 2)

`service-ips` Map 기반 서비스 디스패치와 `numgen random mod N vmap` 로드밸런싱을
시뮬레이션한다. 10,000개 요청에 대한 균등 분배 결과를 확인할 수 있다.

대응 소스: `pkg/proxy/nftables/proxier.go` (라인 1773~1819)

### 3. Topology-Aware 엔드포인트 선택 (데모 3, 4)

- **Zone Hint 기반** (데모 3): `ForZones` hint로 같은 zone의 엔드포인트만 선택
- **Node Hint 기반** (데모 4): `ForNodes` hint로 같은 node의 엔드포인트만 선택

대응 소스: `pkg/proxy/topology.go` (라인 164~234)

### 4. CategorizeEndpoints 알고리즘 (데모 3~6, 9)

`CategorizeEndpoints()` 함수의 핵심 로직을 재현한다:
- Cluster 트래픽 정책: Ready + 토폴로지 필터링
- Local 트래픽 정책: 로컬 Ready 엔드포인트만
- Fallback: Ready 없으면 Serving+Terminating 사용
- allReachable = union(cluster, local) 합집합

대응 소스: `pkg/proxy/topology.go` (라인 48~154)

### 5. Fallback 시나리오 (데모 5, 6)

- **일부 EP에 hint 누락** (데모 5): "전부 또는 전무" 정책으로 토폴로지 무시
- **Ready EP 없음** (데모 6): Serving+Terminating 엔드포인트 사용

### 6. 증분 동기화 (데모 7)

`nftElementStorage`의 증분 동기화 메커니즘을 시뮬레이션한다:
- 현재 상태 캐싱 후 변경분만 트랜잭션에 포함
- `leftoverKeys`로 삭제된 서비스 자동 감지/정리

대응 소스: `pkg/proxy/nftables/proxier.go` (라인 899~998)

### 7. Local 트래픽 정책 + Topology 조합 (데모 9)

`externalTrafficPolicy: Local`과 Topology Aware Routing이 동시에 적용되는
시나리오를 시뮬레이션한다.

## 핵심 소스코드 참조

| 파일 | 내용 |
|------|------|
| `pkg/proxy/nftables/proxier.go` | Proxier 구조체, syncProxyRules, setupNFTables |
| `pkg/proxy/topology.go` | CategorizeEndpoints, topologyModeFromHints |
| `pkg/proxy/endpoint.go` | Endpoint 인터페이스, BaseEndpointInfo |
| `pkg/proxy/endpointslicecache.go` | EndpointSlice hints 추출 |
| `staging/src/k8s.io/api/discovery/v1/types.go` | EndpointHints, ForZone, ForNode |
