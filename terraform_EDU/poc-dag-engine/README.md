# PoC: Terraform DAG 엔진 시뮬레이션

## 개요

Terraform의 핵심인 DAG(Directed Acyclic Graph) 엔진을 시뮬레이션한다.
Terraform은 인프라 리소스 간의 의존 관계를 DAG로 표현하고,
위상 정렬을 통해 올바른 순서로 리소스를 생성/삭제한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `DAG` 구조체 | `internal/dag/dag.go` → `AcyclicGraph` | DAG 그래프 구현 |
| `TopologicalSort()` | `AcyclicGraph.TopologicalOrder()` | 위상 정렬 |
| `DetectCycles()` | `internal/dag/tarjan.go` → `StronglyConnected()` | 순환 탐지 |
| `ParallelWalk()` | `internal/dag/walk.go` → `Walker` | 병렬 그래프 순회 |
| `ResourceVertex` | `internal/terraform/node_resource_*` | 리소스 노드 |

## 구현 내용

### 1. 인접 리스트 기반 DAG
- 정점(Vertex)과 간선(Edge)으로 그래프 구성
- downEdges(의존 대상), upEdges(의존하는 노드) 관리

### 2. DFS 기반 위상 정렬
- 후위 순회(post-order)로 의존성 순서 결정
- 리프 노드(의존성 없는 노드)부터 처리

### 3. 순환 탐지
- 3-color DFS 알고리즘 (white/gray/black)
- 순환 발견 시 경로 추적

### 4. 병렬 워커
- `sync.WaitGroup`과 `goroutine`을 활용한 병렬 실행
- in-degree 기반: 의존성이 0인 노드부터 동시 실행
- 노드 완료 시 의존하는 노드들의 카운트 감소

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

AWS 인프라 의존 관계 그래프:

```
             aws_eip.web
                 │
                 ▼
           aws_instance.web
            /           \
           ▼             ▼
 aws_subnet.main   aws_security_group.web
           \             /
            ▼           ▼
           aws_vpc.main
```

처리 순서:
1. `aws_vpc.main` (의존성 없음, 먼저 생성)
2. `aws_subnet.main` + `aws_security_group.web` (VPC에만 의존, 병렬 생성)
3. `aws_instance.web` (Subnet, SG 모두 완료 후 생성)
4. `aws_eip.web` (Instance 완료 후 생성)

## 핵심 포인트

- Terraform은 `terraform plan` 시 리소스 간 의존 관계를 분석하여 DAG를 구축한다
- `terraform apply` 시 DAG 워커가 병렬로 리소스를 처리하여 배포 시간을 단축한다
- 순환 의존성이 있으면 Terraform이 오류를 발생시킨다
