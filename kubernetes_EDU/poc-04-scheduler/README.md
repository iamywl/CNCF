# PoC-04: 쿠버네티스 스케줄링 프레임워크

## 개요

K8s 스케줄러의 **플러그인 기반 스케줄링 프레임워크**를 구현한다.
스케줄링 사이클은 `Queue → Filter → Score → SelectHost → Bind` 순서로 진행된다.

## 구현 내용

| 컴포넌트 | 역할 | 실제 소스 위치 |
|---------|------|-------------|
| SchedulingQueue | 우선순위 기반 파드 정렬 | `pkg/scheduler/internal/queue/scheduling_queue.go` |
| NodeResourcesFit | 리소스 충분 여부 확인 (Filter) | `pkg/scheduler/framework/plugins/noderesources/fit.go` |
| NodeAffinity | 필수 라벨 매칭 (Filter) | `pkg/scheduler/framework/plugins/nodeaffinity/` |
| LeastAllocated | 리소스 사용률 기반 점수 (Score) | `pkg/scheduler/framework/plugins/noderesources/least_allocated.go` |
| NodeAffinityScore | 선호 라벨 매칭 보너스 (Score) | `pkg/scheduler/framework/plugins/nodeaffinity/` |

## 스케줄링 사이클

```
1. Queue에서 우선순위 높은 파드 Pop
2. Filter: 모든 노드에 대해 Filter 플러그인 병렬 실행 → 부적합 노드 제거
3. Score: 적합한 노드에 Score 플러그인 실행 → 가중 합계 산출
4. SelectHost: 최고 점수 노드 선택 (동점 시 랜덤)
5. Bind: 파드를 선택된 노드에 배정
```

## 테스트 시나리오

- gpu-training: GPU 라벨 필수 → node-1만 통과
- web-frontend: SSD+us-east 선호 → Score에서 보너스
- resource-hungry: 대용량 리소스 요청 → 대부분 노드 필터링
- low-priority-job: 마지막에 스케줄링 → 남은 리소스에 배치

## 실행

```bash
go run main.go
```
