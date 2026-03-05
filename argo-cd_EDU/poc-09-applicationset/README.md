# PoC 09: ApplicationSet 컨트롤러

## 개요

Argo CD ApplicationSet 컨트롤러의 핵심 동작을 Go 표준 라이브러리만으로 시뮬레이션한다.
ApplicationSet은 하나의 선언으로 여러 Argo CD Application을 자동으로 생성/관리하는 Argo CD의 확장 기능이다.

## 참조 소스 코드

| 파일 | 역할 |
|------|------|
| `applicationset/generators/interface.go` | Generator 인터페이스 정의 |
| `applicationset/generators/list.go` | ListGenerator 구현 |
| `applicationset/generators/cluster.go` | ClusterGenerator 구현 |
| `applicationset/generators/git.go` | GitGenerator 구현 |
| `applicationset/generators/matrix.go` | MatrixGenerator (데카르트 곱) |
| `applicationset/generators/merge.go` | MergeGenerator (mergeKey 병합) |
| `applicationset/controllers/applicationset_controller.go` | Reconcile 루프 |

## 핵심 개념

### Generator 인터페이스

```go
// 실제 소스: applicationset/generators/interface.go
type Generator interface {
    GenerateParams(appSetGenerator, applicationSetInfo, client) ([]map[string]any, error)
    GetRequeueAfter(appSetGenerator) time.Duration
    GetTemplate(appSetGenerator) *ApplicationSetTemplate
}
```

모든 Generator는 `GenerateParams()`를 통해 파라미터 세트 목록을 반환한다.
각 파라미터 세트는 하나의 Application 생성에 사용된다.

### Generator 종류

| Generator | 동작 | 사용 예 |
|-----------|------|---------|
| **ListGenerator** | 정적 파라미터 목록 | 고정된 환경(dev/staging/prod) 목록 |
| **ClusterGenerator** | `argocd.argoproj.io/secret-type=cluster` 시크릿 기반 | 등록된 모든 클러스터 |
| **GitGenerator** | 저장소 디렉토리 탐색 | `apps/*/` 패턴으로 앱 디렉토리 |
| **MatrixGenerator** | 두 Generator의 데카르트 곱 | 클러스터 × 환경 조합 |
| **MergeGenerator** | mergeKey로 파라미터 병합 | 기본값 + 환경별 오버라이드 |

### MatrixGenerator 동작 원리

```go
// 실제 코드: applicationset/generators/matrix.go
for _, a := range g0 {           // 첫 번째 Generator 파라미터
    for _, b := range g1 {       // 두 번째 Generator 파라미터
        combined := merge(a, b)  // utils.CombineStringMaps (키 충돌 시 에러)
        res = append(res, combined)
    }
}
```

2개 클러스터 × 2개 환경 = 4개 Application 생성.

### MergeGenerator 동작 원리

```go
// 실제 코드: applicationset/generators/merge.go
baseParamSetsByMergeKey = getParamSetsByMergeKey(mergeKeys, paramSets[0])
// mergeKey를 JSON으로 직렬화하여 복합 키 생성
// override Generator의 파라미터로 기본값 덮어쓰기
maps.Copy(baseParamSet, overrideParamSet)
```

### SyncPolicy

| 정책 | 생성 | 수정 | 삭제 |
|------|------|------|------|
| `create-only` | O | X | X |
| `create-update` | O | O | X |
| `create-delete` | O | X | O |
| `sync` | O | O | O |

### Reconcile 루프

```
ApplicationSet 조회
    ↓
Generator 실행 → params 목록
    ↓
각 params로 Template 렌더링 → 희망 Application 목록
    ↓
현재 Application 목록과 비교 (diff)
    ↓
SyncPolicy에 따라 create / update / delete
```

### ClusterGenerator의 파라미터 구조

```go
// 실제 코드: applicationset/generators/cluster.go
params["name"]           = string(cluster.Data["name"])
params["nameNormalized"] = utils.SanitizeName(name)  // 소문자, 특수문자 → '-'
params["server"]         = string(cluster.Data["server"])
params["project"]        = string(cluster.Data["project"])
// 어노테이션/레이블도 파라미터로 노출
params["metadata.labels.<key>"] = value
```

## 실행 방법

```bash
go run main.go
```

## 실행 결과 요약

```
시나리오 1: ListGenerator  → dev/staging/prod 3개 Application 생성
시나리오 2: ClusterGenerator → 2개 클러스터 각각 Application 생성
시나리오 3: GitGenerator   → 3개 디렉토리 기반 Application 생성
시나리오 4: MatrixGenerator → 2×2=4개 Application 생성 (데카르트 곱)
시나리오 5: MergeGenerator → 기본값 + prod 오버라이드 (replicas/cpu)
시나리오 6: SyncPolicy     → sync 정책으로 staging 제거 시 자동 삭제
```

## 핵심 설계 선택의 이유 (Why)

**왜 Generator가 `[]map[string]any`를 반환하는가?**
템플릿 렌더링 엔진(text/template 또는 fasttemplate)이 임의의 키-값 쌍을 받아 Application 스펙의 모든 문자열 필드에 적용할 수 있도록, 제네릭한 인터페이스가 필요하기 때문이다.

**왜 MatrixGenerator는 정확히 2개 Generator만 지원하는가?**
코드에 `ErrMoreThanTwoGenerators`, `ErrLessThanTwoGenerators` 에러가 정의되어 있다. 중첩 Matrix/Merge를 통해 더 복잡한 조합을 구성할 수 있어 2개 제한으로 충분하다.

**왜 MergeGenerator가 JSON으로 mergeKey를 직렬화하는가?**
복합 키(여러 필드)를 단일 map 키로 표현하면서 순서 독립적 비교를 보장하기 위해 JSON 직렬화를 사용한다.
