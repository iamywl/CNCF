# PoC 17: Cilium CRD/Kubernetes 통합 서브시스템 시뮬레이션

## 개요

이 PoC는 Cilium이 Kubernetes CRD(Custom Resource Definition)와 통합하는 핵심 메커니즘을 순수 Go 표준 라이브러리만으로 시뮬레이션한다. 외부 의존성 없이 K8s Informer, WorkQueue, Controller Reconciliation Loop, Optimistic Concurrency Control 등을 구현한다.

## 실행 방법

```bash
cd EDU/poc-17-crd-k8s
go run main.go
```

외부 패키지 의존성이 없으며, Go 1.21 이상에서 동작한다.

## 시뮬레이션 구성 요소

### 1. CRD 타입 정의

실제 Cilium 코드의 CRD 타입을 축소하여 구현한다:

| 시뮬레이션 타입 | 실제 소스 | 설명 |
|---------------|----------|------|
| `CiliumNetworkPolicy` | `pkg/k8s/apis/cilium.io/v2/cnp_types.go` | 네트워크 정책 |
| `CiliumEndpoint` | `pkg/k8s/apis/cilium.io/v2/types.go` | 엔드포인트 상태 |
| `CiliumIdentity` | `pkg/k8s/apis/cilium.io/v2/types.go` | 보안 Identity |
| `CiliumNode` | `pkg/k8s/apis/cilium.io/v2/types.go` | 노드 설정/상태 |

### 2. CRD Scheme 등록

20개의 Cilium CRD를 Scheme에 등록한다.

| 실제 소스 | 시뮬레이션 |
|----------|----------|
| `pkg/k8s/apis/cilium.io/v2/register.go` | `registerCiliumCRDs()` |
| `pkg/k8s/apis/cilium.io/v2alpha1/register.go` | 동일 함수 내 |

### 3. K8s API Server

etcd 기반 저장소와 Watch 메커니즘을 시뮬레이션한다:

- **Create**: 객체 생성, ResourceVersion 할당, Watch 이벤트 발행
- **Update**: Optimistic Concurrency Control (ResourceVersion 체크)
- **Delete**: 객체 삭제, 삭제 이벤트 발행
- **Watch**: 변경 스트림 구독

### 4. SharedInformer

`k8s.io/client-go/tools/cache` 의 SharedInformer를 시뮬레이션한다:

| 실제 소스 | 시뮬레이션 |
|----------|----------|
| `pkg/k8s/informer/informer.go` | `SharedInformer` |
| `cache.Store` | `Store` (Indexer 포함) |
| `cache.TransformFunc` | `TransformFunc` |

### 5. Resource[T] 추상화

Cilium 고유의 Resource 추상화를 시뮬레이션한다:

| 실제 소스 | 시뮬레이션 |
|----------|----------|
| `pkg/k8s/resource/resource.go` | `Resource` |
| `resource.Event[T]` | `ResourceEvent` |
| `event.Done()` | `ResourceEvent.Done()` |

### 6. WorkQueue

Rate-Limiting WorkQueue를 시뮬레이션한다:

- 중복 방지 (같은 키가 큐에 두 번 들어가지 않음)
- 에러 시 재큐잉 (최대 재시도 제한)
- Dirty 마킹 (처리 중 새 이벤트 발생 시)

### 7. Controller (Reconciler)

K8s 표준 컨트롤러 패턴을 시뮬레이션한다:

| 구현 | 역할 |
|------|------|
| `cnp-reconciler` | CNP를 내부 PolicyRepository에 반영 |
| `cep-reconciler` | CEP를 IPCache에 반영 |
| `endpoint-gc` | 고아 CEP 삭제 (Operator) |
| `identity-gc` | 미참조 Identity 삭제 (Operator) |

### 8. 내부 데이터 구조

| 구조 | 역할 |
|------|------|
| `PolicyRepository` | 정책 저장소 (revision 관리) |
| `IdentityAllocator` | Identity 번호 할당 |
| `IPCache` | IP-Identity 매핑 캐시 |

## 시뮬레이션 단계 (Phase)

### Phase 1: CRD 스키마 등록
- 20개 CRD 타입을 Scheme에 등록
- v2 (안정) 및 v2alpha1 (알파) 버전 포함

### Phase 2: 인프라 시작
- API Server, Agent, Operator 시작
- Informer/Resource/Controller 초기화
- 캐시 동기화 대기

### Phase 3: CiliumIdentity 생성
- 정상 Identity 2개 생성
- 고아 Identity 1개 생성 (GC 대상)

### Phase 4: CiliumEndpoint 생성 및 Status 업데이트
- CEP 생성 (state=creating)
- Status 업데이트 (creating -> ready)
- IPCache 자동 반영 확인
- 고아 CEP 생성 (state=disconnected, GC 대상)

### Phase 5: CiliumNetworkPolicy 생성 및 Reconcile
- 유효한 CNP 생성 -> PolicyRepository 반영
- 유효하지 않은 CNP 생성 -> 에러 후 최대 재시도까지 재큐잉
- 이그레스 규칙 CNP 생성

### Phase 6: Optimistic Concurrency Control
- 동일 객체를 두 클라이언트가 동시 업데이트 시도
- ResourceVersion 충돌 발생
- 최신 버전 재조회 후 재시도 성공

### Phase 7: CiliumNode 생성
- 노드 생성 (InstanceID, IPAM, 주소 등)

### Phase 8: Operator GC 실행
- **Endpoint GC**: disconnected 상태의 고아 CEP 삭제
- **Identity GC**: 어떤 CEP에서도 참조하지 않는 Identity 삭제

### Phase 9: CRD 삭제 및 이벤트 전파
- CNP 삭제 -> PolicyRepository에서 제거
- CEP 삭제 -> IPCache 정리

### Phase 10: 최종 상태 확인
- API Server 저장소 상태
- PolicyRepository revision
- IPCache 매핑 상태
- 등록된 CRD 요약

## 실제 Cilium 코드와의 대응

```
시뮬레이션                     실제 Cilium 코드
-----------                    ----------------
APIServer                  ->  K8s API Server (etcd)
CRDScheme                  ->  k8s.io/apimachinery/pkg/runtime.Scheme
SharedInformer             ->  k8s.io/client-go/tools/cache.SharedInformer
Store                      ->  k8s.io/client-go/tools/cache.Indexer
TransformFunc              ->  cache.TransformFunc
Resource                   ->  pkg/k8s/resource.Resource[T]
ResourceEvent              ->  resource.Event[T]
RateLimitingQueue          ->  k8s.io/client-go/util/workqueue.RateLimitingInterface
Controller                 ->  pkg/controller.Manager
CiliumAgent                ->  daemon/cmd (cilium-agent)
CiliumOperator             ->  operator/cmd (cilium-operator)
PolicyRepository           ->  pkg/policy.Repository
IdentityAllocator          ->  pkg/identity/cache
IPCache                    ->  pkg/ipcache.IPCache
reconcileCNP               ->  pkg/policy/k8s (정책 import)
reconcileCEP               ->  pkg/k8s/watchers/cilium_endpoint.go endpointUpdated
reconcileEndpointGC        ->  operator/endpointgc/gc.go
reconcileIdentityGC        ->  operator/identitygc/gc.go
registerCiliumCRDs         ->  pkg/k8s/apis/cilium.io/v2/register.go addKnownTypes
```

## 핵심 학습 포인트

1. **Informer 패턴**: List+Watch로 로컬 캐시를 유지하며, 네트워크 단절 시에도 캐시 기반으로 동작
2. **Resource[T] 추상화**: event.Done() 호출이 필수이며, 미호출 시 해당 키의 새 이벤트가 발행되지 않음
3. **Optimistic Concurrency**: ResourceVersion을 통한 충돌 감지 및 재시도 패턴
4. **Transform 함수**: 메모리 최적화를 위해 불필요한 필드를 제거하는 변환 레이어
5. **Agent/Operator 분리**: 노드별 작업(Agent)과 클러스터별 작업(Operator) 분리
6. **GC 메커니즘**: 고아 리소스를 주기적으로 정리하는 Operator 역할
7. **재큐잉과 에러 처리**: 최대 재시도 제한이 있는 Rate-Limiting WorkQueue
