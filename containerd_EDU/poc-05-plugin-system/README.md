# PoC-05: 플러그인 의존성 그래프

## 목적

containerd의 플러그인 시스템 핵심 동작을 시뮬레이션한다. Registration 구조체 기반의 플러그인 등록, Graph() DFS 알고리즘을 통한 의존성 순서 계산, InitContext를 통한 플러그인 간 참조 메커니즘을 재현한다.

## 핵심 개념

### 1. Registration과 Registry

containerd의 모든 플러그인은 `Registration` 구조체로 등록된다. 각 Registration은 Type과 ID로 구성된 고유 URI(`Type.ID`)를 갖고, `Requires` 필드로 의존하는 플러그인 타입을 선언한다.

```
Registration{
    Type:     "io.containerd.snapshotter.v1",
    ID:       "overlayfs",
    Requires: []Type{"io.containerd.content.v1"},
    InitFn:   func(ic *InitContext) (interface{}, error) { ... },
}
```

`Registry`는 `[]*Registration` 타입으로, 불변(immutable) 설계이다. `Register()` 호출 시 새 슬라이스를 반환한다.

### 2. Graph() DFS 의존성 정렬

`Graph()` 함수는 DFS(깊이 우선 탐색) 알고리즘으로 의존성 순서를 계산한다.

```
Graph(filter) → []Registration (초기화 순서)
  1. 비활성화 플러그인 마킹
  2. 각 플러그인에 대해 children() 재귀 호출
  3. 의존성이 먼저 ordered에 추가됨
  4. 자기 자신 추가
```

`children()` 함수의 핵심 로직:
- 현재 플러그인의 `Requires` 타입을 순회
- Registry에서 해당 타입의 플러그인을 찾아 재귀적으로 처리
- 와일드카드(`"*"`)인 경우 모든 타입의 플러그인에 의존

### 3. InitContext와 조회 메서드

`InitContext`는 플러그인 초기화 시 전달되는 컨텍스트로, 이미 초기화된 플러그인을 조회하는 세 가지 메서드를 제공한다:

| 메서드 | 용도 |
|--------|------|
| `GetSingle(type)` | 해당 타입의 유일한 인스턴스 조회 (복수 시 에러) |
| `GetByID(type, id)` | 특정 Type+ID 조합의 인스턴스 조회 |
| `GetByType(type)` | 해당 타입의 모든 인스턴스를 map으로 반환 |

### 4. Meta: Capabilities와 Exports

플러그인은 초기화 시 `Meta`를 통해 자신의 기능을 선언한다:
- `Capabilities`: 플러그인 기능 스위치 (예: overlay, hardlink)
- `Exports`: 키-값 쌍으로 외부에 노출하는 정보 (예: root 경로)

### 5. DisableFilter

`DisableFilter`는 특정 플러그인을 비활성화하는 필터 함수이다. `Graph()` 호출 시 전달되어 비활성화된 플러그인과 그에 의존하는 플러그인을 초기화 목록에서 제외한다.

## 소스 참조

| 파일 | 설명 |
|------|------|
| `vendor/github.com/containerd/plugin/plugin.go` | Registration, Registry, Graph(), children(), DisableFilter, Register() |
| `vendor/github.com/containerd/plugin/context.go` | InitContext, Meta, Plugin, Set, GetSingle, GetByType, GetByID |
| `pkg/shim/shim.go` | 실제 Graph 사용 예시 (shim 서버 초기화) |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
containerd 플러그인 의존성 그래프 시뮬레이션
========================================

--- 시나리오 1: 플러그인 등록 및 Graph() 의존성 정렬 ---

  등록된 플러그인 수: 8

  Graph() DFS 의존성 정렬 결과 (초기화 순서):
     1. io.containerd.content.v1.content                        [의존: 없음]
     2. io.containerd.event.v1.exchange                         [의존: 없음]
     3. io.containerd.snapshotter.v1.overlayfs                  [의존: io.containerd.content.v1]
     4. io.containerd.lease.v1.manager                          [의존: io.containerd.content.v1]
     5. io.containerd.metadata.v1.bolt                          [의존: io.containerd.content.v1, ...]
     6. io.containerd.gc.v1.scheduler                           [의존: io.containerd.metadata.v1]
     7. io.containerd.runtime.v2.task                           [의존: io.containerd.metadata.v1, ...]
     8. io.containerd.service.v1.tasks-service                  [의존: io.containerd.runtime.v2, ...]

  순서대로 플러그인 초기화 실행:
    ...각 플러그인이 의존성을 참조하며 초기화...

--- 시나리오 2: DisableFilter로 GC, Lease 플러그인 비활성화 ---
  비활성화 후 초기화 순서: (GC, Lease 제외된 6개)

--- 시나리오 3: 와일드카드 의존성 ("*") ---
  와일드카드 의존 플러그인이 가장 마지막에 배치

--- 시나리오 4: InitContext 조회 메서드 ---
  GetSingle, GetByID, GetByType 동작 확인
```
