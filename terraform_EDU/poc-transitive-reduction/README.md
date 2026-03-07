# PoC 13: 이행적 축소(Transitive Reduction) 시뮬레이션

## 개요

Terraform DAG에서 사용되는 이행적 축소(Transitive Reduction) 알고리즘을 시뮬레이션합니다. 이 알고리즘은 도달 가능성(reachability)을 유지하면서 불필요한 간선을 제거하여 그래프를 최적화합니다.

## 학습 목표

1. **이행적 축소 정의**: 도달 가능성 보존 + 간선 최소화
2. **알고리즘 구현**: BFS 기반 도달 가능성 검사와 간선 제거
3. **정확성 검증**: 축소 전후 도달 가능성 동일성 확인
4. **Terraform에서의 활용**: 의존성 그래프 최적화, 병렬 실행 개선

## Terraform 실제 코드 참조

| 개념 | 실제 파일 |
|------|----------|
| TransitiveReduction | `internal/dag/dag.go` |
| DAG 구조체 | `internal/dag/dag.go` |
| 의존성 그래프 | `internal/terraform/graph.go` |

## 이행적 축소란?

```
축소 전:           축소 후:
A ──→ B            A ──→ B
│     │                  │
│     ▼                  ▼
└───→ C                  C

A→C 간선은 A→B→C 경로로 이미 도달 가능하므로 제거됩니다.
```

## 알고리즘

```
for each vertex u:
  for each direct successor v of u:
    for each vertex w reachable from v (via BFS):
      if edge(u, w) exists:
        remove edge(u, w)  // u→v→...→w로 이미 도달 가능
```

시간 복잡도: O(V * E) where V=정점 수, E=간선 수

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. **간단한 삼각형**: A→B→C, A→C -- 기본 이행적 간선 제거
2. **다이아몬드 패턴**: 두 경로를 통한 도달 가능성
3. **긴 체인**: A→B→C→D→E + 다수의 이행적 간선
4. **Terraform 인프라 그래프**: provider→VPC→subnet→instance→output 계층
5. **대규모 그래프**: 20개 노드, 이행적 간선 대량 제거 + 통계

## 핵심 설계 원리

- **도달 가능성 보존**: 축소 후에도 모든 정점 쌍의 도달 가능성이 동일
- **최소 간선**: 직접 의존성만 남기고 간접 의존성 제거
- **병렬 실행 개선**: 불필요한 순서 제약 제거로 더 많은 작업 병렬 수행 가능
- **Plan 가독성**: 사용자에게 직접 의존성만 표시
