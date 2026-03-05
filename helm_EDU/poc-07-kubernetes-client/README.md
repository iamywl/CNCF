# PoC-07: Helm v4 Kubernetes 클라이언트

## 개요

Helm v4의 Kubernetes 클라이언트 추상화(kube.Interface, ResourceList, Waiter)를 인메모리 클러스터로 시뮬레이션합니다.

## 시뮬레이션하는 패턴

| 패턴 | 실제 소스 | 설명 |
|------|----------|------|
| kube.Interface | `pkg/kube/interface.go` | Create/Update/Delete/Build/Wait |
| ResourceList | `pkg/kube/result.go` | []*resource.Info 리소스 목록 |
| WaitStrategy | `pkg/kube/interface.go` | Watcher/Legacy 대기 전략 |
| Build | `pkg/kube/client.go` | YAML → resource.Info 변환 |
| Update (3-way) | `pkg/kube/client.go` | original/target 비교 → CRUD |

## 실행 방법

```bash
go run main.go
```

## kube.Interface 메서드

```
Interface
  ├── Build(reader, validate) → ResourceList
  │     YAML 스트림 파싱
  ├── Create(resources) → Result
  │     리소스 생성
  ├── Update(original, target) → Result
  │     3-way merge: 생성+업데이트+삭제
  ├── Delete(resources) → Result, []error
  │     리소스 삭제
  ├── Get(resources, related) → map
  │     리소스 상세 조회
  ├── IsReachable() → error
  │     클러스터 연결 확인
  └── GetWaiter(strategy) → Waiter
        ├── Wait()           리소스 준비 대기
        ├── WaitWithJobs()   Job 포함 대기
        └── WaitForDelete()  삭제 완료 대기
```

## Update 3-way merge

```
Original (v1):  [Deployment, Service, ConfigMap]
Target (v2):    [Deployment, Service, Ingress]

결과:
  Updated: Deployment, Service (둘 다 존재)
  Created: Ingress (대상에만 존재)
  Deleted: ConfigMap (원본에만 존재)
```
