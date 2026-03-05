# PoC 05 — GitOps Diff 시스템

## 개요

Argo CD GitOps Engine의 diff 시스템을 시뮬레이션한다.
실제 소스: `gitops-engine/pkg/diff/diff.go`

## 핵심 개념

### DiffResult 구조체

```
gitops-engine/pkg/diff/diff.go:42-50
```

| 필드 | 타입 | 설명 |
|------|------|------|
| `Modified` | bool | 리소스가 일치하지 않으면 true |
| `NormalizedLive` | []byte | 정규화된 실제 클러스터 상태 |
| `PredictedLive` | []byte | Git 설정 적용 시 기대 상태 |

### Diff 모드 결정 트리

```
gitops-engine/pkg/diff/diff.go:76-134  Diff() 함수
```

```
serverSideDiff=true?
  YES → ServerSideDiff  (k8s API dry-run, 가장 정확)
  NO  → config에 ServerSideApply=true 어노테이션?
          YES → StructuredMergeDiff  (SMD 라이브러리)
          NO  → live에 last-applied-configuration 어노테이션?
                  YES → ThreeWayDiff  (3-way merge)
                  NO  → TwoWayDiff    (fallback)
```

### ThreeWayDiff 알고리즘

```
gitops-engine/pkg/diff/diff.go:689-724  ThreeWayDiff()
```

3개의 상태를 비교하여 정확한 변경 추적:

```
orig   = last-applied-configuration 어노테이션 (Argo CD가 마지막으로 적용한 상태)
config = Git의 desired state
live   = 클러스터의 실제 상태

Argo CD 변경 = orig → config
외부 변경    = orig → live

predictedLive = live + Argo CD의 변경사항 (live가 아직 반영 안 한 것만)
```

#### 왜 ThreeWayDiff가 중요한가?

TwoWayDiff는 `config vs live`만 비교하므로, HPA가 replicas를 조정한 것도 OutOfSync로 잡는다.
ThreeWayDiff는 "Argo CD가 의도한 변경"과 "다른 주체의 변경"을 분리하므로, 불필요한 sync를 방지한다.

### Strategic Merge Patch

```
gitops-engine/pkg/diff/diff.go:794  strategicpatch 사용
```

`containers`, `volumes` 같은 배열은 단순 교체가 아니라 `name` 키로 항목을 매칭하여 병합한다:

```
orig:   [{name: app, image: v1}, {name: sidecar, image: proxy:v1}]
config: [{name: app, image: v2}]  ← sidecar 언급 없음

strategic merge 결과:
  [{name: app, image: v2}, {name: sidecar, image: proxy:v1}]
  ↑ app만 변경, sidecar는 live에서 보존
```

### Normalizer

```
gitops-engine/pkg/diff/diff.go:64-67  Normalizer 인터페이스
```

diff 전 알려진 volatile 필드를 제거하여 노이즈를 방지한다:

| 제거 필드 | 이유 |
|-----------|------|
| `metadata.resourceVersion` | k8s가 매 변경마다 갱신 |
| `metadata.uid` | 클러스터마다 다름 |
| `metadata.generation` | 자동 증가 |
| `metadata.creationTimestamp` | 생성 시 자동 설정 |
| `status` | 런타임 상태, Git에서 관리 안 함 |
| `kubectl.kubernetes.io/last-applied-configuration` | 어노테이션 자체는 diff 무관 |

## 실행

```bash
go run main.go
```

## 시나리오

| 시나리오 | Diff 모드 | Modified | 설명 |
|----------|-----------|----------|------|
| 1 | ThreeWayDiff | false | config=live=last-applied, 완전 동일 |
| 2 | ThreeWayDiff | true | Git에서 image/replicas 변경 |
| 3 | ThreeWayDiff | true | 외부에서 replicas 직접 변경 |
| 4 | ThreeWayDiff | true | Argo CD + 외부 동시 변경 |
| 5 | TwoWayDiff | true | last-applied 없음 (Helm 등) |
| 6 | ThreeWayDiff | true | Strategic merge: containers 배열 |

## 실제 코드와의 대응

| 시뮬레이션 | 실제 소스 |
|-----------|-----------|
| `DiffResult` struct | `gitops-engine/pkg/diff/diff.go:42` |
| `Diff()` 모드 결정 | `gitops-engine/pkg/diff/diff.go:76` |
| `ThreeWayDiff()` | `gitops-engine/pkg/diff/diff.go:689` |
| `TwoWayDiff()` | `gitops-engine/pkg/diff/diff.go:514` |
| `Normalizer` 인터페이스 | `gitops-engine/pkg/diff/diff.go:64` |
| `AnnotationLastAppliedConfig` 상수 | `gitops-engine/pkg/diff/diff.go:38` |
