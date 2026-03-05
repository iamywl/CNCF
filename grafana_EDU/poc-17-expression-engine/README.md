# PoC 17: 표현식 엔진

## 목적

Grafana의 서버사이드 표현식(SSE) 엔진을 시뮬레이션한다.
여러 데이터 소스 쿼리 결과를 DAG(방향 비순환 그래프)로 연결하여,
수학 연산, 리듀스, 임계값 판정을 파이프라인으로 실행하는 구조를 구현한다.

## Grafana 실제 구현 참조

Grafana는 `pkg/expr/` 패키지에서 SSE(Server-Side Expressions)를 처리한다:

- **DataPipeline**: 노드들의 순서 있는 목록 (DAG)
- **Node**: DatasourceNode, MathNode, ReduceNode, ThresholdNode 등
- **Service.ExecutePipeline()**: DAG를 위상 정렬하여 순서대로 실행

## 핵심 개념

### DAG 실행 흐름

```
┌─────────────────────────────────────────────────────────────┐
│                      Expression Pipeline                     │
│                                                              │
│  [A: Datasource]   [B: Datasource]                          │
│       │                 │                                    │
│       ▼                 ▼                                    │
│  ┌─────────────────────────┐                                │
│  │  C: Math ($A + $B)      │                                │
│  └────────────┬────────────┘                                │
│               │                                              │
│               ▼                                              │
│  ┌─────────────────────────┐                                │
│  │  D: Reduce mean($C)    │                                │
│  └────────────┬────────────┘                                │
│               │                                              │
│               ▼                                              │
│  ┌─────────────────────────┐                                │
│  │  E: Threshold $D > 50   │  → 1 (firing) / 0 (normal)    │
│  └─────────────────────────┘                                │
│                                                              │
│  실행 순서 (위상 정렬): A → B → C → D → E                    │
└─────────────────────────────────────────────────────────────┘
```

### 노드 타입

| 노드 | 입력 | 출력 | 설명 |
|------|------|------|------|
| DatasourceNode | 없음 | 시계열 데이터 | 데이터 소스에서 쿼리 실행 |
| MathNode | 1+ 시리즈 | 시계열 데이터 | 수학 연산 (A + B, A * 2, abs(A - B)) |
| ReduceNode | 1 시리즈 | 스칼라 값 | 집계 (mean, max, min, last) |
| ThresholdNode | 1 스칼라 | 0 또는 1 | 임계값 비교 (>, <, ==) |

### 의존성 그래프 → 위상 정렬

```
의존성:            위상 정렬:
  A ← C            [A, B, C, D, E]
  B ← C
  C ← D            실행: A → B → (A,B 완료 확인) → C → D → E
  D ← E
```

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== Grafana 표현식 엔진 시뮬레이션 ===

[DAG 구성]
  A: DatasourceNode (CPU 메트릭)
  B: DatasourceNode (Memory 메트릭)
  C: MathNode ($A + $B)
  D: ReduceNode mean($C)
  E: ThresholdNode ($D > 50)

[위상 정렬] A → B → C → D → E

[실행]
  A: 10개 포인트 생성 [45.2, 52.1, 48.7, ...]
  B: 10개 포인트 생성 [30.1, 28.5, 32.4, ...]
  C: $A + $B = [75.3, 80.6, 81.1, ...]
  D: mean($C) = 78.5
  E: 78.5 > 50 → 1 (firing)

[최종 결과] 임계값 초과 → 알림 트리거
```

## 학습 포인트

1. **DAG 기반 파이프라인**: 노드 간 의존성을 방향 비순환 그래프로 모델링
2. **위상 정렬**: 의존성 순서에 따른 올바른 실행 순서 결정
3. **노드 인터페이스**: Execute와 Dependencies로 통일된 노드 계약
4. **변수 전달**: vars map을 통한 노드 간 결과 공유
5. **실패 전파**: 의존 노드 실패 시 하위 노드 스킵
