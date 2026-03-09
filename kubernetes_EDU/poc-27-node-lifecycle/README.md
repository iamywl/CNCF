# PoC 27: Node Lifecycle Controller 및 Taint/Toleration 시스템

## 개요

이 PoC는 Kubernetes **Node Lifecycle Controller**와 **Taint/Toleration** 시스템의 핵심 알고리즘을
Go 표준 라이브러리만으로 시뮬레이션한다.

노드 장애 감지부터 Pod 퇴거까지의 전체 파이프라인을 단일 프로그램으로 재현하여,
각 단계의 동작 원리를 직접 확인할 수 있다.

## 시뮬레이션하는 메커니즘

| 메커니즘 | 실제 Kubernetes 소스 | PoC 구현 |
|---------|---------------------|---------|
| Taint/Toleration 매칭 | `FindMatchingUntoleratedTaint()` | `FindUntoleratedTaint()` |
| 전체 Taint 매칭 (AND) | `GetMatchingTolerations()` | `GetMatchingTolerations()` |
| 최소 허용 시간 계산 | `getMinTolerationTime()` | `GetMinTolerationTime()` |
| 노드 건강 상태 판단 | `tryUpdateNodeHealth()` | `TryUpdateNodeHealth()` |
| Taint 기반 Eviction 결정 | `processTaintBaseEviction()` | `ProcessTaintBaseEviction()` |
| NoSchedule Taint 적용 | `doNoScheduleTaintingPass()` | `DoNoScheduleTaintingPass()` |
| Zone 상태 계산 | `ComputeZoneState()` | `ComputeZoneState()` |
| Zone별 Rate Limiter 조정 | `handleDisruption()` + `setLimiterInZone()` | `HandleDisruption()` |
| Rate-Limited 큐 | `RateLimitedTimedQueue.Try()` | `RateLimitedTimedQueue.Try()` |
| Token Bucket Rate Limiter | `flowcontrol.TokenBucketRateLimiter` | `tryAccept()` (내장 구현) |
| Pod Eviction 처리 | `processPodOnNode()` | `ProcessPodEvictions()` |

## 실행 방법

```bash
cd kubernetes_EDU/poc-27-node-lifecycle
go run main.go
```

외부 의존성이 없으므로 Go 1.18 이상이 설치되어 있으면 바로 실행 가능하다.

## 데모 구성

### 데모 1: Taint/Toleration 매칭 알고리즘

노드에 여러 Taint가 있을 때, 각 Pod의 Toleration이 어떻게 매칭되는지 보여준다.

- **Equal 연산자**: Key + Value + Effect가 모두 일치해야 매칭
- **Exists 연산자**: Key + Effect만 일치하면 매칭 (Value 무시)
- **빈 Key + Exists**: 모든 Taint에 매칭 (와일드카드)
- **TolerationSeconds**: NoExecute Taint를 견딜 수 있는 시간

### 데모 2: Zone 상태 계산 (ComputeZoneState)

Zone 내 Ready/NotReady 노드 비율에 따른 Zone 상태 결정을 시뮬레이션한다.

| Zone 상태 | 조건 |
|-----------|------|
| Normal | 기본 상태 (notReady <= 2 또는 비율 < 55%) |
| PartialDisruption | notReady > 2 AND unhealthy 비율 >= 55% |
| FullDisruption | 모든 노드가 NotReady (readyNodes == 0) |

핵심 포인트:
- `notReady > 2` 조건이 있어, 노드 2개 이하 장애는 항상 Normal (통계적 유의성 확보)
- 기본 임계값 55%는 과반수 이상 장애를 의미

### 데모 3: Rate-Limited Eviction Queue

Token Bucket + Priority Heap 기반의 속도 제한 큐를 시뮬레이션한다.

- **중복 방지**: 동일 노드가 큐에 두 번 들어가지 않음
- **QPS=0**: Eviction 완전 중단 (Master Disruption Mode)
- **QPS 변경**: `SwapLimiter()`로 런타임에 속도 조절 가능

### 데모 4: Node Lifecycle Controller 전체 시뮬레이션

5개 노드 (Zone-A: 3개, Zone-B: 2개) 클러스터에서 점진적 장애 확산을 시뮬레이션한다.

**단계 1**: 정상 상태 - 모든 노드 Ready, Taint 없음

**단계 2**: node-a1 장애 (NotReady)
- NoSchedule + NoExecute Taint 자동 부여
- Pod별 Toleration에 따른 eviction 결정:
  - `batch-1`: Toleration 없음 -> 즉시 eviction
  - `web-2`: tolerationSeconds=60 -> 60초 후 eviction 예약
  - `web-1`: tolerationSeconds=300 -> 300초 후 eviction 예약
  - `critical-1`: TolerationSeconds 없음(무한) -> eviction 안 함
- Zone 상태: Normal (1/3만 장애)

**단계 3**: Zone-A 전체 장애 (FullDisruption)
- Zone-A: 3/3 NotReady -> FullDisruption
- Zone-B: 정상 -> Normal
- Zone-A의 eviction rate는 정상 QPS 유지 (다른 Zone이 정상이므로)

**단계 4**: 모든 Zone 장애 (Master Disruption Mode)
- 모든 Zone이 FullDisruption
- Eviction Rate = 0 (전체 중단)
- 마스터/네트워크 장애로 판단하여 잘못된 대규모 eviction 방지

### 데모 5: getMinTolerationTime 알고리즘

여러 Toleration의 TolerationSeconds 값에서 최소값을 선택하는 알고리즘을 보여준다.

- Toleration이 없으면: 0 (즉시 eviction)
- TolerationSeconds 미설정: -1 (무한 허용)
- 여러 값이 있으면: 최소값 선택 (가장 먼저 만료되는 Toleration 기준)

## 예상 출력 해석

### 노드 상태 테이블

```
  노드         Zone       상태           Taints
  -----------------------------------------------------------------
  node-a1      zone-a     False          not-ready:NoSchedule, not-ready:NoExecute
  node-a2      zone-a     True           (없음)
```

- **상태**: True(정상), False(비정상), Unknown(응답 없음)
- **Taints**: `key:Effect` 형식으로 현재 부여된 Taint 표시

### Pod Eviction 결과

```
  Pod Eviction 결과 (node-a1의 Pod):
    - batch-1 (즉시: toleration 없음)
    - web-2 (1m0s 후 예약됨)
    - web-1 (5m0s 후 예약됨)
```

- **즉시**: 해당 NoExecute Taint에 대한 Toleration이 없거나 TolerationSeconds=0
- **N초 후 예약됨**: TolerationSeconds에 의해 지연 eviction
- critical-1은 TolerationSeconds 없이 Tolerate하므로 목록에 나타나지 않음 (영구 허용)

## 실제 Kubernetes 소스 코드 매핑

| PoC 함수/구조체 | 실제 소스 파일 | 라인 |
|----------------|---------------|------|
| `TaintEffect`, `Toleration` | `staging/src/k8s.io/api/core/v1/types.go` | 4031-4098 |
| `TolerationMatchesTaint()` | `staging/src/k8s.io/component-helpers/scheduling/corev1/helpers.go` | - |
| `FindUntoleratedTaint()` | `staging/src/k8s.io/component-helpers/scheduling/corev1/helpers.go` | - |
| `GetMinTolerationTime()` | `pkg/controller/tainteviction/taint_eviction.go` | 160-182 |
| `NodeLifecycleController` | `pkg/controller/nodelifecycle/node_lifecycle_controller.go` | 218-303 |
| `TryUpdateNodeHealth()` | 위와 동일 | 813-978 |
| `ProcessTaintBaseEviction()` | 위와 동일 | 764-798 |
| `DoNoScheduleTaintingPass()` | 위와 동일 | 523-576 |
| `HandleDisruption()` | 위와 동일 | 979-1068 |
| `ComputeZoneState()` | 위와 동일 | 1264-1282 |
| `ReducedQPSFunc()` | 위와 동일 | 1197-1204 |
| `RateLimitedTimedQueue` | `pkg/controller/nodelifecycle/scheduler/rate_limited_queue.go` | 198-214 |
| `Try()` | 위와 동일 | 231-256 |
| `ProcessPodEvictions()` | `pkg/controller/tainteviction/taint_eviction.go` | 451-490 |

## 핵심 설계 원칙 (Why)

1. **NotReady vs Unreachable 상호 배제**: kubelet이 명시적으로 비정상을 보고하는 경우(NotReady)와
   아예 응답이 없는 경우(Unreachable)는 장애 원인이 다르므로 별도 Taint로 구분. Pod는 각 상황에
   맞는 별도의 Toleration 정책을 적용할 수 있다.

2. **Zone-Aware Rate Limiting**: 단일 노드 장애와 Zone 전체 장애는 완전히 다른 시나리오이다.
   Zone별로 독립적인 Rate Limiter를 유지하여, 장애 규모에 비례하는 대응이 가능하다.

3. **Master Disruption Mode**: 모든 Zone이 동시에 FullDisruption이면, 실제 노드 장애가 아니라
   Control Plane 네트워크 장애일 가능성이 높다. 이 경우 eviction을 전면 중단하여 잘못된
   대규모 Pod 삭제를 방지한다.

4. **소규모 클러스터 PartialDisruption 보호**: 노드 50개 이하의 소규모 클러스터에서
   PartialDisruption이 발생하면 eviction을 완전 중단한다. 적은 수의 노드에서는 통계적
   유의성이 낮아 일시적 문제일 가능성을 고려한 설계이다.

5. **TolerationSeconds로 점진적 eviction**: 모든 Pod를 동시에 퇴거시키면 서비스 중단이
   발생한다. TolerationSeconds를 Pod 종류에 따라 다르게 설정하여 중요도 기반
   점진적 이동이 가능하다 (예: 시스템 Pod는 무한 허용, 일반 Pod는 300초).
