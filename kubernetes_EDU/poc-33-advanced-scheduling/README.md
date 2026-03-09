# PoC 33: 고급 스케줄링 - Scheduling Gates + Dynamic Resource Allocation

## 개요

Kubernetes의 두 가지 고급 스케줄링 메커니즘을 Go 표준 라이브러리만으로 시뮬레이션한다.

### 시뮬레이션 범위

1. **Scheduling Gates 차단/해제**: PreEnqueue에서 Gate 확인, QueueingHint 기반 재큐잉
2. **ResourceClaim 라이프사이클**: 생성 -> 할당 -> 예약 -> 바인딩 전체 흐름
3. **DRA 스케줄러 플러그인 플로우**: PreFilter -> Filter -> Reserve -> PreBind 사이클
4. **DeviceRequest 매칭**: Exactly 모드, FirstAvailable 모드, AllocationMode=All
5. **Claim 템플릿 인스턴스화**: ResourceClaimTemplate에서 ResourceClaim 자동 생성

## 실행 방법

```bash
go run main.go
```

## 데모 구성

### Demo 1: Scheduling Gates 차단/해제

Gate가 있는 Pod가 Unschedulable 큐에 들어가고, 외부 컨트롤러가 Gate를 제거하면
Active 큐로 이동하는 과정을 시뮬레이션한다.

- Gate 2개가 있는 Pod 생성 -> Unschedulable 큐
- Gate 하나 제거 -> 아직 Gate 남아있음
- Gate 모두 제거 -> Active 큐로 이동
- 다른 Pod의 이벤트 -> QueueSkip (UID 비교)

### Demo 2: ResourceClaim 템플릿 인스턴스화

ResourceClaimController가 Pod의 `resourceClaimTemplateName`을 보고
ResourceClaimTemplate에서 실제 ResourceClaim을 생성하는 과정을 시뮬레이션한다.

- Pod 생성 (Template 참조)
- Controller.SyncPod() -> handleClaim()
- 템플릿에서 Claim 생성 (Owner: Pod, GenerateName 패턴)
- Pod의 ResourceClaimRef 업데이트

### Demo 3: DRA 스케줄링 사이클 (전체 흐름)

DynamicResources 플러그인의 PreFilter -> Filter -> Reserve -> PreBind 전체 사이클을
시뮬레이션한다. 16Gi A100 GPU 2개를 요청하는 시나리오.

- PreFilter: Claim 수집, DeviceClass 검증, Allocator 초기화
- Filter: 3개 노드에서 할당 시도 (node-1만 PASS)
- Reserve: 할당 확정, SignalClaimPendingAllocation
- 동시성 테스트: 다른 Pod가 같은 Claim에 접근 시 Unschedulable
- PreBind: 할당 결과 기록, AssumeCache 저장, inFlight 제거

### Demo 4: DeviceRequest 매칭

다양한 DeviceRequest 모드를 시뮬레이션한다.

- **Scenario A**: Exactly 요청 - 원하는 디바이스가 노드에 없는 경우 (실패)
- **Scenario B**: FirstAvailable - 우선순위 순서로 시도 (H100 -> A100 -> T4)
- **Scenario C**: AllocationMode=All - 매칭되는 모든 디바이스 할당

### Demo 5: 통합 시나리오 (Gates + 템플릿 + DRA)

Scheduling Gates와 DRA를 결합한 엔드투엔드 시나리오:

1. Pod 생성 (Gate: driver-installed, Template: gpu-claim-tmpl)
2. Controller가 템플릿에서 Claim 생성
3. 드라이버 설치 완료 -> Gate 제거
4. 스케줄링 큐 진입
5. DRA 플러그인이 GPU 할당
6. Pod 배치 완료

## 시뮬레이션하는 Kubernetes 소스 코드

| 구현 요소 | 실제 소스 경로 |
|----------|-------------|
| SchedulingGates 플러그인 | `pkg/scheduler/framework/plugins/schedulinggates/scheduling_gates.go` |
| PreEnqueue (Gate 확인) | `scheduling_gates.go:48-57` |
| QueueingHint (UID 비교) | `scheduling_gates.go:82-94` |
| DynamicResources 플러그인 | `pkg/scheduler/framework/plugins/dynamicresources/dynamicresources.go` |
| DynamicResources 구조체 | `dynamicresources.go:137-147` |
| PreEnqueue (Claim 존재 확인) | `dynamicresources.go:246-255` |
| PreFilter (Claim 수집 + Allocator) | `dynamicresources.go:402-574` |
| Filter (노드별 할당) | `dynamicresources.go:631-774` |
| Reserve (낙관적 예약) | `dynamicresources.go:907-1001` |
| ResourceClaim Controller | `pkg/controller/resourceclaim/controller.go` |
| syncPod (템플릿 처리) | `controller.go:556-636` |
| handleClaim (Claim 생성) | `controller.go:638-717` |
| PodSchedulingGate 타입 | `staging/src/k8s.io/api/core/v1/types.go:4577-4581` |
| PodResourceClaim 타입 | `staging/src/k8s.io/api/core/v1/types.go:4480-4512` |
| ResourceClaim 타입 | `staging/src/k8s.io/api/resource/v1/types.go:743-757` |
| DeviceClaim 타입 | `staging/src/k8s.io/api/resource/v1/types.go:772-810` |
| DeviceRequest 타입 | `staging/src/k8s.io/api/resource/v1/types.go:831-880` |
| ResourceClaimStatus 타입 | `staging/src/k8s.io/api/resource/v1/types.go:1445-1507` |
| AllocationResult 타입 | `staging/src/k8s.io/api/resource/v1/types.go:1534-1560` |
| DeviceClass 타입 | `staging/src/k8s.io/api/resource/v1/types.go:1772-1834` |
| ResourceClaimTemplate 타입 | `staging/src/k8s.io/api/resource/v1/types.go:1860-1886` |

## 핵심 동작 흐름

### Scheduling Gates

```
Pod 생성 (Gate 포함)
  -> PreEnqueue: Gate 있으면 UnschedulableAndUnresolvable 반환
  -> Unschedulable 큐에서 대기
  -> 외부 컨트롤러가 Gate 제거
  -> UpdatePodSchedulingGatesEliminated 이벤트 발생
  -> QueueingHint: UID 비교로 대상 Pod 확인
  -> Active 큐로 이동
  -> 스케줄링 시작
```

### DRA 스케줄링 사이클

```
PreFilter
  -> Pod의 ResourceClaim 수집
  -> DeviceClass 존재 확인
  -> 이미 할당된 장치 목록 조회
  -> Allocator 초기화

Filter (노드별 병렬 실행)
  -> 이미 할당된 claim의 NodeSelector 매칭 확인
  -> 미할당 claim에 대해 allocator.Allocate(node, claims) 호출
  -> 결과를 nodeAllocations에 캐싱 (mutex 보호)

Reserve
  -> 선택된 노드의 할당 결과를 informationsForClaim에 저장
  -> SignalClaimPendingAllocation으로 inFlight 마킹
  -> 다른 Pod가 같은 claim 접근 시 Unschedulable

PreBind
  -> API 서버에 Allocation + ReservedFor 기록
  -> AssumeCache에 최신 claim 저장
  -> allocatedDevices에 장치 추가
  -> inFlight 마킹 제거
```

### DeviceRequest 매칭

```
Exactly 모드:
  -> DeviceClass.Selectors + Request.Selectors 모두 매칭
  -> Count개의 장치 선택

FirstAvailable 모드:
  -> 서브요청을 순서대로 시도
  -> 첫 번째 성공한 서브요청의 장치 반환

AllocationMode=All:
  -> 매칭되는 모든 장치 할당 (최소 1개 필요)
```

## 출력 예시

```
Demo 1: Scheduling Gates
- Gate 2개 Pod -> Unschedulable
- Gate 제거 -> Active 큐 이동

Demo 2: 템플릿 인스턴스화
- Template 'gpu-template' -> Claim 'ml-job-gpu-0001' 생성

Demo 3: DRA 스케줄링 사이클
- 16Gi A100 GPU 2개 요청
- node-1 (A100 x2): PASS
- node-2 (T4 x2): FAIL
- node-3 (A100 x1): FAIL (수량 부족)
- Reserve: inFlight 마킹
- 동시성: 다른 Pod -> Unschedulable
- PreBind: 할당 확정

Demo 4: DeviceRequest 매칭
- Exactly A100: FAIL (없음)
- FirstAvailable H100>A100>T4: T4 할당
- AllocationMode=All: T4 x2 할당

Demo 5: 통합
- Gate 대기 -> 템플릿 Claim 생성 -> Gate 해제 -> DRA 할당 -> 배치
```

## 외부 의존성

없음 (Go 표준 라이브러리만 사용)
