# PoC 12: 크리티컬 패스 분석 시뮬레이터

## 개요

Jaeger UI에 구현된 크리티컬 패스(Critical Path) 분석 알고리즘을 Go로 포팅하여 시뮬레이션한다.

크리티컬 패스란 트레이스의 전체 실행 시간에 직접적으로 기여하는 스팬들의 경로이다. 이 경로 위의 어떤 스팬이 느려지면 전체 트레이스 완료 시간이 늘어나지만, 비크리티컬 스팬이 느려져도 전체 시간에 영향이 없다.

Go 표준 라이브러리만으로 원래 TypeScript로 작성된 알고리즘을 재현한다.

## Jaeger 소스코드 대응

| 시뮬레이션 개념 | Jaeger UI 소스 위치 |
|---|---|
| 트리 구축 | `jaeger-ui/packages/jaeger-ui/src/utils/TreeNode.tsx` |
| `findLastFinishingChildSpan` | `jaeger-ui/src/TracePage/CriticalPath/utils.ts` |
| `sanitizeOverFlowingChildren` | 같은 파일 |
| `computeCriticalPath` | 같은 파일 |
| 크리티컬 패스 시각화 | `jaeger-ui/src/TracePage/CriticalPath/index.tsx` |

## 핵심 알고리즘

### 1. findLastFinishingChildSpan
```
입력: 부모 스팬
출력: 가장 늦게 끝나는(endTime이 가장 큰) 자식 스팬

for each child in parent.children:
    if child.endTime > latestEnd:
        latestEnd = child.endTime
        lastChild = child
return lastChild
```

병렬로 실행되는 자식 중 가장 늦게 끝나는 자식이 부모의 완료 시간을 결정하므로, 이 자식이 크리티컬 패스에 속한다.

### 2. sanitizeOverFlowingChildren
```
입력: 부모 스팬
동작: 부모의 [startTime, endTime] 범위를 벗어나는 자식의 duration을 클리핑

for each child in parent.children:
    if child.endTime > parent.endTime:
        child.duration = parent.endTime - child.startTime
    if child.startTime < parent.startTime:
        child.startTime = parent.startTime
        child.duration -= adjustment
    recurse(child)
```

실제 환경에서 클록 스큐나 비동기 작업으로 인해 자식이 부모 범위를 벗어나는 경우가 발생한다. 이를 보정해야 정확한 분석이 가능하다.

### 3. computeCriticalPath (핵심)
```
입력: 현재 스팬
출력: 크리티컬 패스 세그먼트 목록

function computeCriticalPath(span):
    mark span as critical

    if span has no children:
        record segment(span, selfTime=span.duration)
        return

    lastChild = findLastFinishingChildSpan(span)

    selfTimeBefore = lastChild.startTime - span.startTime
    selfTimeAfter = span.endTime - lastChild.endTime
    totalSelfTime = selfTimeBefore + selfTimeAfter

    if totalSelfTime > 0:
        record segment(span, selfTime=totalSelfTime)

    computeCriticalPath(lastChild)  // 재귀
```

### 4. Self-time 계산
```
Self-time = 스팬의 총 duration - 크리티컬 자식이 커버하는 시간

예시: A(300ms)의 자식 B(200ms)가 A 시작 50ms 후에 시작
  selfTimeBefore = 50ms (B 시작 전)
  selfTimeAfter  = 50ms (B 종료 후)
  A의 self-time  = 100ms
```

## 테스트 케이스

| 테스트 | 구조 | 검증 포인트 |
|---|---|---|
| 1. 직렬 실행 | A -> B -> C | 모든 스팬이 크리티컬 패스 |
| 2. 병렬 실행 | A -> B\|C (B 늦게 끝남) | B만 크리티컬, C는 비크리티컬 |
| 3. 중첩 병렬 | A -> B -> D\|E, A -> C | A -> B -> E 가 크리티컬 |
| 4. 오버플로우 | B가 A보다 길게 실행 | B의 duration 클리핑 |
| 5. 실제 트레이스 | 12개 스팬 e-commerce 흐름 | 결제 경로가 크리티컬 |
| 6. 깊은 체인 | 7단계 직렬 체인 | 전체가 크리티컬 패스 |

## ASCII 타임라인 시각화

```
  스팬                 서비스     |0%--------20%-------40%-------60%-------80%-------|
  ------------------------------------------------------------------------------------
 *handleRequest       gateway   |######################################            |
 *processCheckout     api       |  ####################################            |
  getItems            cart      |  .....                                           |
 *createOrder         order     |       #########################                  |
 *charge              payment   |          ###############                         |
 *processPayment      bank      |           ############                           |
  reserveStock        inventory |          ....                                    |
  sendNotif           notif     |                              ######              |

  범례: # = 크리티컬 패스, . = 비크리티컬, * = 크리티컬 패스 스팬
```

## 실행

```bash
go run main.go
```

## 활용

- **성능 병목 식별**: self-time이 큰 스팬이 진짜 병목
- **최적화 우선순위**: 비크리티컬 스팬 최적화는 전체 시간에 효과 없음
- **병렬화 기회**: 크리티컬 패스를 분할하거나 병렬화할 수 있는지 검토
- **SLO 분석**: 크리티컬 패스 길이가 SLO를 초과하는 경우 원인 분석
