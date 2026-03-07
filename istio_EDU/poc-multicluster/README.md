# PoC: 멀티클러스터 서비스 디스커버리

## 개요

Istio는 여러 Kubernetes 클러스터에 분산된 서비스를 **하나의 메시**로 통합하는
멀티클러스터 아키텍처를 지원한다. 이 PoC는 Istio의 **Aggregate Controller**가
다중 클러스터 레지스트리를 통합하고, **locality-aware 로드 밸런싱**으로
최적의 엔드포인트를 선택하며, **Network Gateway**를 통해 크로스 네트워크
트래픽을 라우팅하는 과정을 Go 표준 라이브러리만으로 시뮬레이션한다.

### 멀티클러스터 모델

Istio의 멀티클러스터는 두 가지 차원으로 구분된다:

```
단일 네트워크 (flat network):     다중 네트워크 (multi-network):
  Pod IP 직접 통신 가능              Pod IP 직접 통신 불가
  게이트웨이 불필요                    East-West Gateway 필요

  cluster-1  cluster-2            cluster-1          cluster-3
  ┌────────┐┌────────┐           ┌────────┐         ┌────────┐
  │ Pod A  ││ Pod B  │           │ Pod A  │  GW←→GW │ Pod C  │
  │10.0.1.x││10.0.2.x│           │10.0.1.x│:15443   │10.0.3.x│
  └────────┘└────────┘           └────────┘         └────────┘
  (net-west) (net-west)          (net-west)          (net-east)
```

## 시뮬레이션 구성 요소

### 1. Aggregate Controller

모든 클러스터 레지스트리를 하나의 통합 뷰로 제공하는 핵심 컨트롤러이다.

- **Istio 소스**: `pilot/pkg/serviceregistry/aggregate/controller.go`
- **`Services()`**: 모든 레지스트리에서 서비스를 조회하고 hostname 기준으로 병합
- **`mergeService()`**: 동일 hostname의 서비스가 여러 클러스터에 존재하면:
  - `ClusterVIPs` 병합: 각 클러스터의 서비스 ClusterIP를 보존
  - `ServiceAccounts` 병합: 중복 제거 후 통합
- **`AddRegistry()`**: Kubernetes 레지스트리를 앞에 배치하는 우선순위 로직

### 2. 클러스터 레지스트리 (KubeRegistry)

각 Kubernetes 클러스터의 서비스/엔드포인트 정보를 관리한다.

- **Istio 소스**: `pilot/pkg/serviceregistry/kube/controller/controller.go`
- 클러스터별 서비스, 엔드포인트, 네트워크 게이트웨이 보유
- 서비스 변경 이벤트를 Aggregate Controller에 전파

### 3. 서비스 모델

Istio의 서비스 모델은 멀티클러스터를 위해 `ClusterVIPs`를 핵심 필드로 가진다.

- **Istio 소스**: `pilot/pkg/model/service.go`
- **`ClusterVIPs`** (`AddressMap`): `map[ClusterID][]string` -- 클러스터별 서비스 IP
- **`ServiceAccounts`**: 여러 클러스터의 서비스 어카운트를 병합

### 4. Locality-Aware 로드 밸런싱

프록시의 위치(Region/Zone/Subzone)와 엔드포인트의 위치를 비교하여 최적의 엔드포인트를 선택한다.

- **Istio 소스**: `pkg/workloadapi/workload.proto`의 `LoadBalancing` message
- **우선순위** (낮을수록 우선):
  - 0: 동일 서브존
  - 1: 동일 존
  - 2: 동일 리전
  - 3: 동일 네트워크
  - 4: 원격 네트워크
- **FAILOVER 모드**: 가장 높은 우선순위 그룹부터 시도, 없으면 다음으로 폴백

### 5. Network Gateway

서로 다른 네트워크의 클러스터 간 트래픽을 중계하는 East-West Gateway이다.

- **Istio 소스**: `pilot/pkg/model/network.go`의 `NetworkGateway` struct
- 크로스 네트워크 엔드포인트 접근 시 Pod IP 대신 게이트웨이 주소로 라우팅
- HBONE 포트(15008) 또는 mTLS 포트(15443)를 통해 터널링

### 6. 동적 클러스터 관리

런타임에 클러스터를 추가/삭제할 수 있다.

- **Istio 소스**: `pilot/pkg/serviceregistry/kube/controller/multicluster.go`
- `istio-system` 네임스페이스의 kubeconfig Secret을 감시
- Secret 추가 → `AddRegistryAndRun()`, Secret 삭제 → `DeleteRegistry()`

## 시뮬레이션 환경

```
cluster-1 (Primary, config cluster)
  Network: net-west
  Region:  us-west / Zone: us-west-1a
  서비스:  reviews, productpage, ratings
  Gateway: 35.192.0.1:15443

cluster-2 (Remote)
  Network: net-west (cluster-1과 동일 네트워크)
  Region:  us-west / Zone: us-west-1b
  서비스:  reviews

cluster-3 (Remote)
  Network: net-east (다른 네트워크 → 게이트웨이 필요)
  Region:  us-east / Zone: us-east-1a
  서비스:  reviews, ratings
  Gateway: 35.194.0.1:15443
```

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 예상 출력

```
[1단계] 멀티클러스터 환경 구성
[2단계] 각 클러스터에 서비스 등록
[3단계] 엔드포인트(Pod) 등록 및 locality 설정
[4단계] 네트워크 게이트웨이 설정
[5단계] Aggregate Controller로 멀티클러스터 서비스 통합
  → reviews 서비스: 3개 클러스터의 ClusterVIPs 병합
  → ServiceAccounts 병합 (reviews-sa + reviews-sa-east)

[6단계] 크로스 클러스터 엔드포인트 수집
  → 모든 클러스터의 엔드포인트를 하나의 목록으로 통합

[7단계] Locality-aware 엔드포인트 선택
  시나리오 1: cluster-1(us-west-1a)에서 → 로컬 엔드포인트 우선
  시나리오 2: cluster-3(us-east-1a)에서 → 로컬 엔드포인트 우선
  시나리오 3: cluster-2에서 ratings 접근 → 같은 리전 cluster-1로 폴백

[8단계] 크로스 네트워크 게이트웨이 라우팅
  → net-east 엔드포인트: 35.194.0.1:15443 경유
  → net-west 엔드포인트: 35.192.0.1:15443 경유

[9단계] 동적 클러스터 관리
  → cluster-4 추가: ClusterVIPs에 자동 병합
  → cluster-2 삭제: 엔드포인트에서 자동 제거

[10단계] 아키텍처 다이어그램 출력
```

## Istio 소스코드 참조

| 구성 요소 | 소스 파일 | 핵심 내용 |
|-----------|----------|----------|
| Aggregate Controller | `pilot/pkg/serviceregistry/aggregate/controller.go` | `Services()`, `mergeService()`, `AddRegistry()`, `GetRegistries()` |
| Multicluster Controller | `pilot/pkg/serviceregistry/kube/controller/multicluster.go` | `Multicluster` 구조체, `initializeCluster()` |
| Service 모델 | `pilot/pkg/model/service.go` | `Service`, `ClusterVIPs`, `AddressMap`, `ShallowCopy()` |
| Network 모델 | `pilot/pkg/model/network.go` | `NetworkGateway`, `HBONEPort` |
| AddressMap | `pilot/pkg/model/addressmap.go` | `map[ClusterID][]string`, `GetAddressesFor()`, `SetAddressesFor()` |
| LoadBalancing | `pkg/workloadapi/workload.proto` | `LoadBalancing.Scope`, `LoadBalancing.Mode`, `Locality` |

## 핵심 학습 포인트

1. **hostname 기준 병합**: 동일 FQDN의 서비스는 여러 클러스터에 존재해도 하나의 서비스로 병합된다. 각 클러스터의 ClusterIP는 `ClusterVIPs`에 보존된다.

2. **FAILOVER 로드 밸런싱**: Locality 우선순위(Subzone > Zone > Region > Network)에 따라 가장 가까운 건강한 엔드포인트를 선택한다. 로컬 엔드포인트가 없으면 원격으로 폴백한다.

3. **Network Gateway**: 서로 다른 네트워크의 클러스터 간에는 Pod IP로 직접 통신할 수 없다. East-West Gateway(포트 15443)를 경유하여 mTLS 터널로 트래픽을 전달한다.

4. **동적 클러스터 관리**: Primary 클러스터의 Istiod가 `istio-system` 네임스페이스의 kubeconfig Secret을 감시하여 원격 클러스터를 자동으로 추가/삭제한다.

5. **Aggregate Controller 패턴**: 여러 이종 레지스트리(Kubernetes, ServiceEntry 등)를 하나의 인터페이스로 추상화하여 상위 계층에 통합된 서비스 뷰를 제공한다.
