# PoC 01: 모듈 시스템 (Module System)

## 개요

Loki의 의존성 그래프 기반 모듈 초기화/종료 시스템을 시뮬레이션한다.

Loki는 모놀리식(single-binary)과 마이크로서비스 모드를 동시에 지원하기 위해
각 컴포넌트를 독립적인 "모듈"로 정의하고, 의존성 그래프에 따라 초기화/종료한다.

## 시뮬레이션하는 Loki 컴포넌트

| 컴포넌트 | Loki 실제 위치 | 설명 |
|----------|---------------|------|
| ModuleManager | `pkg/loki/loki.go` | 모듈 등록/초기화/종료 관리 |
| Module | `pkg/loki/modules.go` | 개별 모듈 (Distributor, Ingester 등) |
| 위상 정렬 | dskit/services | 의존성 순서대로 서비스 시작 |

## 핵심 알고리즘

### 위상 정렬 (Kahn's Algorithm)

```
1. 각 노드의 진입 차수(in-degree) 계산
2. 진입 차수 0인 노드를 큐에 추가
3. 큐에서 노드를 꺼내어:
   - 결과에 추가
   - 해당 노드에 의존하는 노드의 진입 차수 감소
   - 진입 차수가 0이 된 노드를 큐에 추가
4. 모든 노드 처리까지 반복
```

### 의존성 그래프 예시

```
server (기반)     ring (기반)     store (기반)
                    │                │
              ┌─────┴─────┐    ┌─────┴─────┐
              ▼           ▼    ▼           ▼
          distributor  ingester        compactor
                          │
                    ┌─────┴─────┐
                    ▼           ▼
                querier       (다른 모듈)
                    │
              ┌─────┴─────┐
              ▼           ▼
        query-frontend  ruler
```

## 시나리오

1. **all 모드**: 모든 모듈을 위상 정렬 순서로 초기화
2. **read 모드**: query-frontend만 타겟 지정 → 의존성 자동 해결
3. **순환 의존성 감지**: DFS로 사이클 감지 및 에러 보고

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

- 의존성 그래프를 DAG(Directed Acyclic Graph)로 관리하는 방법
- Kahn's Algorithm을 이용한 위상 정렬
- `-target` 플래그에 따른 부분 그래프 초기화
- 초기화 역순의 graceful shutdown이 왜 안전한지
- DFS 기반 순환 의존성 감지
