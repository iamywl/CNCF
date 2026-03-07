# PoC 9: 서비스 의존성 DAG 분석 시뮬레이터

## 개요

Jaeger가 트레이스 데이터로부터 서비스 간 의존성 그래프(DAG, Directed Acyclic Graph)를 구축하고 분석하는 과정을 시뮬레이션한다.

Jaeger의 `/api/dependencies` 엔드포인트와 spark-dependencies 잡이 수행하는 핵심 분석 로직을 Go 표준 라이브러리만으로 재현한다.

## Jaeger 소스코드 대응

| 시뮬레이션 개념 | Jaeger 소스 위치 |
|---|---|
| `DependencyLink` 구조체 | `model/dependency.go` - `DependencyLink` |
| 의존성 추출 로직 | `cmd/query/app/handler.go` - `GetDependencies()` |
| 트레이스 → 의존성 변환 | `plugin/storage/memory/memory.go` - `GetDependencies()` |
| 의존성 그래프 시각화 | Jaeger UI의 DAG 페이지 |

## 시뮬레이션 내용

### 1. 의존성 그래프 구축
- 트레이스 데이터에서 parent-child 스팬의 서비스가 다를 때만 의존성으로 기록
- 같은 서비스 내 호출은 내부 호출이므로 제외
- `DependencyLink`에 호출 횟수와 에러 횟수를 누적

### 2. 순환 의존성 감지
- DFS(깊이 우선 탐색)로 back-edge를 탐지하여 순환 식별
- 순환 의존성은 마이크로서비스 아키텍처의 안티패턴
- 장애 전파, 데드락 위험 경고

### 3. 크리티컬 패스 분석
- 위상 정렬(Kahn's Algorithm) + DP로 최장 지연 경로 계산
- 서비스 호출 체인에서 성능 병목이 되는 경로 식별
- 경로 역추적으로 전체 경로 출력

### 4. ASCII 시각화
- 인접 행렬: 서비스 간 호출 관계를 행렬로 표현
- 트리 구조: 루트 서비스부터 종단 서비스까지 계층적 표현
- 순환 참조는 별도 표시

### 5. 서비스 통계
- 인입/발신 호출 횟수
- 에러율
- 평균 지연시간

## 핵심 알고리즘

```
의존성 추출:
  for each trace:
    for each span:
      if span.parentID exists AND parent.service != span.service:
        graph[parent.service][span.service].callCount++

순환 감지 (DFS):
  visited = {}, inStack = {}
  for each unvisited service:
    DFS(service):
      mark visited, add to stack
      for each child:
        if child in stack → CYCLE detected!
        if child not visited → DFS(child)
      remove from stack

크리티컬 패스 (위상정렬 + DP):
  1. Kahn's algorithm으로 위상 정렬
  2. 정렬 순서대로 dist[child] = max(dist[child], dist[parent] + latency(parent,child))
  3. max(dist)를 가진 노드에서 역추적
```

## 테스트 시나리오

| 시나리오 | 설명 | 기대 결과 |
|---|---|---|
| 1 | 정상 마이크로서비스 (6개 서비스) | 순환 없음, 크리티컬 패스 식별 |
| 2 | 순환 의존성 (A→B→C→A) | 순환 감지, 경고 출력 |
| 3 | 대규모 (10개 서비스, 20 트레이스) | 복잡한 DAG 분석 |

## 실행

```bash
go run main.go
```

## 출력 예시

```
=== 의존성 링크 목록 ===

부모 서비스          -->  자식 서비스          호출수     에러율      평균지연
--------------------------------------------------------------------
api-gateway        -->  order-service           1     0.0%     350.0ms
api-gateway        -->  user-service            7    14.3%     132.9ms
frontend           -->  api-gateway             8     0.0%     230.0ms
...

=== 순환 의존성 감지 ===
  순환 의존성 없음 (정상)

=== 크리티컬 패스 ===
  경로: frontend -> api-gateway -> order-service -> payment-service
  총 지연시간: 735.7ms
```
