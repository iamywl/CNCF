# PoC-28: Pod Disruption Budget (PDB) 및 Eviction API 핵심 알고리즘 시뮬레이션

## 개요

Kubernetes PDB(Pod Disruption Budget)와 Eviction API의 핵심 알고리즘을 Go 표준 라이브러리만으로 재현한다. DisruptionController의 상태 동기화 로직과 Eviction API의 예산 확인/차감 로직, DisruptedPods 맵의 2-Phase Commit 패턴, Unhealthy Pod Eviction Policy 등을 시뮬레이션한다.

## 실행 방법

```bash
cd poc-28-pod-disruption
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다. 시뮬레이션 3에서 DeletionTimeout(3초)을 대기하므로 총 실행 시간은 약 4-5초이다.

## 소스 참조

| 구성 요소 | Kubernetes 소스 경로 | 핵심 라인 |
|-----------|---------------------|----------|
| PDB 타입 정의 | `staging/src/k8s.io/api/policy/v1/types.go` | 28-224 |
| DisruptionController | `pkg/controller/disruption/disruption.go` | 81-120 |
| trySync | 위 파일 | 735-772 |
| getExpectedPodCount | 위 파일 | 818-858 |
| countHealthyPods | 위 파일 | 924-940 |
| buildDisruptedPodMap | 위 파일 | 944-976 |
| failSafe | 위 파일 | 983-999 |
| updatePdbStatus | 위 파일 | 1001-1037 |
| EvictionREST | `pkg/registry/core/pod/storage/eviction.go` | 70-74 |
| Create (Eviction) | 위 파일 | 129-315 |
| checkAndDecrement | 위 파일 | 424-484 |
| canIgnorePDB | 위 파일 | 389-397 |

## 시뮬레이션 항목

### 시뮬레이션 1: MinAvailable vs MaxUnavailable 비교

`getExpectedPodCount()` 함수의 동작을 재현한다. 다양한 PDB 사양(절대값, 백분율, 완전 차단)에 대해 `ExpectedPods`, `DesiredHealthy`, `DisruptionsAllowed`가 어떻게 계산되는지 확인한다.

**핵심 수식**: `DisruptionsAllowed = max(0, CurrentHealthy - DesiredHealthy)` (단, ExpectedPods > 0)

| 시나리오 | DesiredHealthy 계산 |
|---------|-------------------|
| minAvailable (절대값) | DesiredHealthy = minAvailable |
| minAvailable (백분율) | DesiredHealthy = ceil(minAvailable% * expectedCount) |
| maxUnavailable | DesiredHealthy = expectedCount - maxUnavailable |

### 시뮬레이션 2: Eviction API 흐름 (checkAndDecrement)

`EvictionREST.Create()` 및 `checkAndDecrement()` 함수의 동작을 재현한다. 연속적인 퇴거 요청에서 `DisruptionsAllowed`가 감소하고, 0이 되면 429 응답으로 거부되는 흐름을 시뮬레이션한다.

- DisruptionsAllowed > 0: 퇴거 허용, DisruptedPods에 등록
- DisruptionsAllowed == 0: 429 TooManyRequests 반환
- 컨트롤러 재동기화 후 상태 재계산

### 시뮬레이션 3: DisruptedPods 맵과 DeletionTimeout

`buildDisruptedPodMap()` 함수와 2-Phase Commit 패턴을 재현한다. DeletionTimeout(실제 2분, 시뮬레이션 3초)을 사용하여 다음 시나리오를 검증한다.

1. Pod가 DisruptedPods에 등록되면 CurrentHealthy에서 제외됨
2. DeletionTimeout 내에 Pod가 삭제되지 않으면 경고와 함께 맵에서 제거됨
3. 맵에서 제거된 Pod는 다시 Healthy로 카운트됨
4. 정상 삭제된 Pod는 DeletionTimestamp 기반으로 즉시 맵에서 제거됨

### 시뮬레이션 4: Unhealthy Pod Eviction Policy 비교

두 가지 정책(`IfHealthyBudget`, `AlwaysAllow`)의 동작 차이를 검증한다.

| 정책 | 예산 충분 시 Unhealthy Pod | 예산 부족 시 Unhealthy Pod |
|------|-------------------------|-------------------------|
| IfHealthyBudget | 퇴거 허용 (PDB 감소 없음) | 퇴거 거부 (429) |
| AlwaysAllow | 퇴거 허용 (PDB 감소 없음) | **퇴거 허용** (PDB 감소 없음) |

핵심: Unhealthy Pod는 countHealthyPods에서 이미 카운트되지 않으므로, 삭제 시 PDB를 감소시킬 필요가 없다.

### 시뮬레이션 5: Fail-Safe 메커니즘과 ObservedGeneration

- **failSafe()**: trySync 실패 시 DisruptionsAllowed=0으로 설정 (Fail Closed 원칙)
- **ObservedGeneration**: PDB Spec 변경 후 컨트롤러가 재동기화하기 전까지 퇴거 차단
- 복구 후 trySync 성공 시 정상 값으로 자동 복원

### 시뮬레이션 6: Terminal/Pending Pod의 PDB 무시

`canIgnorePDB()` 함수의 동작을 재현한다. Running 상태가 아닌 Pod(Pending, Succeeded, Failed)는 DisruptionsAllowed=0이어도 퇴거가 허용된다.

### 시뮬레이션 7: 동시 퇴거 경쟁 (Optimistic Concurrency)

여러 goroutine이 동시에 퇴거를 요청하는 시나리오이다. 뮤텍스를 통한 동시성 제어로 PDB 예산(DisruptionsAllowed=2)을 정확히 2개만 퇴거 허용함을 검증한다. 실제 Kubernetes에서는 etcd의 resourceVersion 기반 Optimistic Concurrency Control을 사용한다.

## 출력 구조

```
╔══════════════════════════════════════════════════════════════════════╗
║  Kubernetes PDB & Eviction API 핵심 알고리즘 시뮬레이션                ║
╚══════════════════════════════════════════════════════════════════════╝

======================================================================
  시뮬레이션 N: [제목]
======================================================================

--- [하위 시나리오] ---
  PDB "name" Status:
    ExpectedPods:       N    ← 전체 기대 Pod 수
    DesiredHealthy:     N    ← 최소 유지 정상 Pod 수
    CurrentHealthy:     N    ← 현재 정상 Pod 수
    DisruptionsAllowed: N    ← 허용 퇴거 수
    DisruptedPods:      {}   ← 퇴거 승인 후 삭제 대기 Pod
    ObservedGeneration: N    ← 컨트롤러가 처리한 세대
    Condition:          ...  ← 상태 조건

  [Eviction] Pod "name" 퇴거 성공/거부
```

## 핵심 알고리즘 매핑

| PoC 함수 | Kubernetes 함수 | 역할 |
|---------|----------------|------|
| `getExpectedPodCount()` | `disruption.go:getExpectedPodCount()` | expectedCount, desiredHealthy 계산 |
| `countHealthyPods()` | `disruption.go:countHealthyPods()` | 현재 정상 Pod 수 계산 |
| `buildDisruptedPodMap()` | `disruption.go:buildDisruptedPodMap()` | DisruptedPods 맵 정리 |
| `updatePdbStatus()` | `disruption.go:updatePdbStatus()` | DisruptionsAllowed 최종 계산 |
| `failSafe()` | `disruption.go:failSafe()` | 동기화 실패 시 예산 0 설정 |
| `checkAndDecrement()` | `eviction.go:checkAndDecrement()` | 예산 확인 및 원자적 감소 |
| `canIgnorePDB()` | `eviction.go:canIgnorePDB()` | Terminal Pod PDB 무시 판별 |
| `Evict()` | `eviction.go:Create()` | 전체 Eviction API 흐름 |
