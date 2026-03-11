# 25. 주석 시스템 (Annotations) Deep Dive

> Grafana 소스 기준: `pkg/services/annotations/`
> 작성일: 2026-03-08

---

## 1. 개요

주석(Annotation)은 Grafana 대시보드 그래프 위에 **이벤트를 시각적으로 표시**하는 기능이다. 배포, 장애, 설정 변경 등의 이벤트를 시계열 데이터와 함께 볼 수 있어 상관 분석에 필수적이다.

### 핵심 특징

| 특징 | 설명 |
|------|------|
| 시간 범위 | 단일 시점 또는 시간 범위(region) |
| 태그 | 필터링을 위한 태그 시스템 |
| 유형 | 조직 주석 vs 대시보드 주석 |
| 출처 | 사용자 수동 생성 / 알림 자동 생성 / API |
| 복합 저장소 | SQL(xorm) + Loki(알림 히스토리) |

---

## 2. 아키텍처

```
pkg/services/annotations/
├── annotations.go           # Repository, Cleaner 인터페이스
├── models.go               # Item, ItemDTO, ItemQuery, Tag 등
├── accesscontrol/
│   ├── accesscontrol.go     # AuthService (RBAC 기반 필터링)
│   └── models.go
├── annotationsimpl/
│   ├── annotations.go       # RepositoryImpl (서비스 구현)
│   ├── composite_store.go   # CompositeStore (다중 저장소 병렬 조회)
│   ├── xorm_store.go        # SQL Store
│   ├── cleanup.go           # CleanupServiceImpl
│   └── loki/
│       └── historian_store.go  # Loki 기반 알림 히스토리
└── annotationstest/
    └── fake.go
```

### 저장소 전략

```
                    ┌──────────────────┐
                    │  RepositoryImpl  │
                    └────────┬─────────┘
                             │
              ┌──────────────┴──────────────┐
              │                             │
    ┌─────────▼──────────┐     ┌────────────▼──────────┐
    │  CompositeStore    │     │   XormStore (writer)   │
    │  (readStore)       │     │   SQL → DB 저장         │
    │  병렬 조회          │     └────────────────────────┘
    └─────────┬──────────┘
              │
    ┌─────────┴──────────┐
    │                    │
  XormStore          LokiHistorianStore
  (SQL 주석)          (알림 히스토리)
```

**왜 CompositeStore인가?**

Grafana의 NG Alert 시스템은 알림 상태 변화 히스토리를 Loki에 저장한다. 사용자가 주석을 조회할 때, SQL에 저장된 수동 주석과 Loki에 저장된 알림 히스토리를 **병렬로 조회하여 병합**한다.

---

## 3. 핵심 데이터 모델

### Item (주석 레코드)

```go
// 소스: pkg/services/annotations/models.go (86-108행)
type Item struct {
    ID           int64            `xorm:"pk autoincr 'id'"`
    OrgID        int64            `xorm:"org_id"`
    UserID       int64            `xorm:"user_id"`
    DashboardID  int64            `xorm:"dashboard_id"`  // deprecated
    DashboardUID string           `xorm:"dashboard_uid"`
    PanelID      int64            `xorm:"panel_id"`
    Text         string
    AlertID      int64            `xorm:"alert_id"`
    PrevState    string           // 알림 상태 전이 기록
    NewState     string
    Epoch        int64            // 시작 시간 (Unix ms)
    EpochEnd     int64            // 종료 시간 (region용)
    Tags         []string
    Data         *simplejson.Json
}
```

### 주석 유형 분류

```go
// 소스: pkg/services/annotations/models.go (157-180행)
type annotationType int

const (
    Organization annotationType = iota  // 조직 전체 주석
    Dashboard                           // 대시보드 특정 주석
)

func (annotation *ItemDTO) GetType() annotationType {
    if annotation.DashboardUID != nil && *annotation.DashboardUID != "" {
        return Dashboard
    }
    return Organization
}
```

| 유형 | 조건 | 표시 범위 |
|------|------|---------|
| Organization | DashboardUID가 비어있음 | 모든 대시보드에 표시 |
| Dashboard | DashboardUID가 있음 | 해당 대시보드에만 표시 |

### ItemQuery (조회 쿼리)

```go
// 소스: pkg/services/annotations/models.go (8-29행)
type ItemQuery struct {
    OrgID        int64
    From         int64      // 시간 범위 시작
    To           int64      // 시간 범위 끝
    DashboardUID string     // 특정 대시보드 필터
    PanelID      int64      // 특정 패널 필터
    Tags         []string   // 태그 필터
    MatchAny     bool       // true: OR, false: AND
    Limit        int64      // 기본 100
    Offset       int64      // 페이지네이션
}
```

---

## 4. Repository 인터페이스

```go
// 소스: pkg/services/annotations/annotations.go (17-24행)
type Repository interface {
    Save(ctx context.Context, item *Item) error
    SaveMany(ctx context.Context, items []Item) error
    Update(ctx context.Context, item *Item) error
    Find(ctx context.Context, query *ItemQuery) ([]*ItemDTO, error)
    Delete(ctx context.Context, params *DeleteParams) error
    FindTags(ctx context.Context, query *TagsQuery) (FindTagsResult, error)
}
```

---

## 5. Find 흐름 — 접근 제어 통합 페이지네이션

```go
// 소스: pkg/services/annotations/annotationsimpl/annotations.go (79-126행)
func (r *RepositoryImpl) Find(ctx context.Context, query *annotations.ItemQuery) ([]*annotations.ItemDTO, error) {
    if query.Limit == 0 {
        query.Limit = 100
    }

    // 최적화: 대시보드 필터 없는 조회는 먼저 접근 제어 없이 확인
    if query.DashboardID == 0 && query.DashboardUID == "" {
        res, err := r.reader.Get(ctx, *query, &accesscontrol.AccessResources{
            SkipAccessControlFilter: true,
        })
        if err != nil || len(res) == 0 {
            return []*annotations.ItemDTO{}, err
        }
        query.Limit = int64(len(res))
    }

    results := make([]*annotations.ItemDTO, 0, query.Limit)
    query.Page = 1

    // 페이지네이션 루프: 제한에 도달하거나 모든 대시보드 확인 시 종료
    for len(results) < int(query.Limit) {
        // 접근 제어: 현재 사용자가 볼 수 있는 대시보드 목록 조회
        resources, _ := r.authZ.Authorize(ctx, *query)

        // 해당 대시보드들의 주석 조회
        res, _ := r.reader.Get(ctx, *query, resources)
        results = append(results, res...)

        query.Page++ // 다음 대시보드 페이지
        if len(resources.Dashboards) < int(query.Limit) {
            break // 모든 대시보드 확인 완료
        }
    }

    return results, nil
}
```

**왜 이중 페이지네이션인가?**

주석 접근 제어는 대시보드 단위로 적용된다. 사용자가 접근 가능한 대시보드를 먼저 찾고, 그 대시보드의 주석만 반환해야 한다. 대시보드 수가 많을 수 있으므로 대시보드도 페이지 단위로 처리한다.

---

## 6. CompositeStore — 병렬 조회

```go
// 소스: pkg/services/annotations/annotationsimpl/composite_store.go (35-57행)
func (c *CompositeStore) Get(ctx context.Context, query annotations.ItemQuery,
    accessResources *accesscontrol.AccessResources) ([]*annotations.ItemDTO, error) {

    itemCh := make(chan []*annotations.ItemDTO, len(c.readers))

    // 모든 리더에 병렬 조회
    err := concurrency.ForEachJob(ctx, len(c.readers), len(c.readers),
        func(ctx context.Context, i int) (err error) {
            defer handleJobPanic(c.logger, c.readers[i].Type(), &err)
            items, err := c.readers[i].Get(ctx, query, accessResources)
            itemCh <- items
            return err
        })

    // 결과 병합 후 시간순 정렬
    close(itemCh)
    res := make([]*annotations.ItemDTO, 0)
    for items := range itemCh {
        res = append(res, items...)
    }
    sort.Sort(annotations.SortedItems(res))
    return res, nil
}
```

### 패닉 복구

```go
// 소스: pkg/services/annotations/annotationsimpl/composite_store.go (86-99행)
func handleJobPanic(logger log.Logger, storeType string, jobErr *error) {
    if r := recover(); r != nil {
        logger.Error("Annotation store panic", "store", storeType, "stack", log.Stack(1))
        if jobErr != nil {
            *jobErr = fmt.Errorf("concurrent job panic: %w", panicErr)
        }
    }
}
```

**왜 패닉 복구가 필요한가?** Loki 저장소가 외부 서비스이므로 예기치 않은 에러가 발생할 수 있다. 한 저장소의 실패가 전체 조회를 중단시키지 않도록 패닉을 복구한다.

---

## 7. 정렬 — 종료 시간 우선

```go
// 소스: pkg/services/annotations/models.go (141-155행)
type SortedItems []*ItemDTO

func (s SortedItems) Less(i, j int) bool {
    if s[i].TimeEnd != s[j].TimeEnd {
        return s[i].TimeEnd > s[j].TimeEnd  // 종료 시간 내림차순
    }
    return s[i].Time > s[j].Time            // 시작 시간 내림차순
}
```

---

## 8. 정리 서비스 (Cleanup)

```go
// 소스: pkg/services/annotations/annotationsimpl/cleanup.go (23-26행)
const (
    alertAnnotationType     = "alert_id <> 0"
    dashboardAnnotationType = "dashboard_id <> 0 AND alert_id = 0"
    apiAnnotationType       = "alert_id = 0 AND dashboard_id = 0"
)
```

정리는 세 가지 유형별로 독립적으로 수행된다:

```go
func (cs *CleanupServiceImpl) Run(ctx context.Context, cfg *setting.Cfg) (int64, int64, error) {
    // 1. 알림 주석 정리
    affected, _ = cs.store.CleanAnnotations(ctx, cfg.AlertingAnnotationCleanupSetting, alertAnnotationType)

    // 2. API 주석 정리
    affected, _ = cs.store.CleanAnnotations(ctx, cfg.APIAnnotationCleanupSettings, apiAnnotationType)

    // 3. 대시보드 주석 정리
    affected, _ = cs.store.CleanAnnotations(ctx, cfg.DashboardAnnotationCleanupSettings, dashboardAnnotationType)

    // 4. 고아 태그 정리
    if totalCleanedAnnotations > 0 {
        affected, _ = cs.store.CleanOrphanedAnnotationTags(ctx)
    }
}
```

---

## 9. 태그 시스템

```go
// 소스: pkg/services/annotations/models.go (32-70행)
type TagsQuery struct {
    OrgID int64
    Tag   string
    Limit int64
}

type Tag struct {
    Key   string
    Value string
    Count int64
}

type TagsDTO struct {
    Tag   string `json:"tag"`
    Count int64  `json:"count"`
}
```

태그는 `annotation_tag` 테이블에 별도 저장되며, 주석 검색 시 태그 필터링에 사용된다. `MatchAny` 옵션으로 OR/AND 매칭을 선택할 수 있다.

---

## 10. PoC

→ [poc-26-annotations/main.go](./poc-26-annotations/main.go): 주석 시스템 + CompositeStore 병렬 조회 시뮬레이션

---

---

## 11. 실제 소스 코드 심화 분석

### 11.1 RepositoryImpl — Reader/Writer 분리

```go
// 소스: pkg/services/annotations/annotationsimpl/annotations.go
type RepositoryImpl struct {
    reader ReadStore      // CompositeStore (병렬 조회)
    writer WriteStore     // XormStore (SQL 쓰기)
    authZ  AuthService    // RBAC 접근 제어
}
```

**왜 Reader와 Writer를 분리하는가?**

읽기는 CompositeStore를 통해 SQL + Loki를 병렬로 조회하지만, 쓰기는 SQL만 대상으로 한다. Loki에는 알림 히스토리가 NG Alert 시스템에 의해 자동으로 기록되므로, Annotations API에서 직접 쓰지 않는다. 이 분리는 읽기/쓰기 경로가 근본적으로 다른 비대칭 아키텍처를 반영한다.

### 11.2 Find()의 접근 제어 최적화

```go
// 소스: pkg/services/annotations/annotationsimpl/annotations.go (79-126행)
func (r *RepositoryImpl) Find(ctx context.Context, query *annotations.ItemQuery) ([]*annotations.ItemDTO, error) {
    // 최적화: 대시보드 필터 없는 조회는 먼저 접근 제어 없이 카운트
    if query.DashboardID == 0 && query.DashboardUID == "" {
        res, err := r.reader.Get(ctx, *query, &accesscontrol.AccessResources{
            SkipAccessControlFilter: true,
        })
        if err != nil || len(res) == 0 {
            return []*annotations.ItemDTO{}, err
        }
        query.Limit = int64(len(res))  // 실제 존재하는 만큼만 조회
    }
    // ...
}
```

**왜 먼저 접근 제어 없이 조회하는가?**

접근 제어 필터링은 비용이 높다(대시보드 권한 테이블 JOIN 필요). 대시보드 특정 필터가 없는 경우(조직 전체 주석 조회), 먼저 접근 제어 없이 존재 여부와 수를 확인한다. 결과가 없으면 빈 배열을 바로 반환하여 불필요한 권한 검사를 건너뛴다.

### 11.3 CompositeStore — 병렬 조회의 에러 처리

```go
// 소스: pkg/services/annotations/annotationsimpl/composite_store.go (35-57행)
func (c *CompositeStore) Get(ctx context.Context, query annotations.ItemQuery,
    accessResources *accesscontrol.AccessResources) ([]*annotations.ItemDTO, error) {

    itemCh := make(chan []*annotations.ItemDTO, len(c.readers))
    err := concurrency.ForEachJob(ctx, len(c.readers), len(c.readers),
        func(ctx context.Context, i int) (err error) {
            defer handleJobPanic(c.logger, c.readers[i].Type(), &err)
            items, err := c.readers[i].Get(ctx, query, accessResources)
            itemCh <- items  // 에러가 있어도 결과는 전송
            return err
        })
    // ...
}
```

**왜 에러가 있어도 결과를 전송하는가?**

Loki가 일시적으로 불가용해도 SQL 주석은 정상적으로 반환해야 한다. `concurrency.ForEachJob`은 모든 job의 에러를 수집하지만, 각 job은 에러 발생 시에도 부분 결과를 채널에 보낼 수 있다.

### 11.4 Cleaner 인터페이스

```go
// 소스: pkg/services/annotations/annotations.go
type Cleaner interface {
    Run(ctx context.Context, cfg *setting.Cfg) (int64, int64, error)
}
```

정리 서비스는 Grafana의 스케줄러에 의해 주기적으로 호출된다. 반환값은 `(삭제된 주석 수, 삭제된 태그 수, 에러)`이다.

### 11.5 정리 설정 (grafana.ini)

```ini
[unified_alerting]
# 알림 히스토리 주석 보존 기간
min_interval = 10s

[annotations.alert]
# 알림 주석 정리 설정
max_age = 0        # 0 = 무제한
max_count = 0      # 0 = 무제한

[annotations.dashboard]
# 대시보드 주석 정리 설정
max_age = 0
max_count = 0

[annotations.api]
# API 주석 정리 설정
max_age = 0
max_count = 0
```

세 가지 주석 유형(alert, dashboard, api)에 대해 독립적으로 보존 정책을 설정할 수 있다.

---

## 12. 태그 시스템 상세

### 12.1 태그 저장 구조 (SQL)

```
annotation 테이블:
    id | org_id | dashboard_id | text | epoch | epoch_end | ...

annotation_tag 테이블:
    id | annotation_id | tag_id

tag 테이블:
    id | term | key | value

관계:
    annotation 1 ──── N annotation_tag N ──── 1 tag
```

태그는 `key:value` 형식으로 저장되며, `key`와 `value`가 별도 컬럼에 분리된다. `term`은 `key:value` 전체 문자열이다.

### 12.2 태그 기반 필터링

```go
// ItemQuery에서 태그 필터링
query := &ItemQuery{
    Tags:     []string{"deploy", "team:backend"},
    MatchAny: false,  // AND 매칭 (모든 태그 포함)
}
```

| MatchAny | SQL 조건 | 의미 |
|----------|---------|------|
| false | `HAVING COUNT(DISTINCT tag.id) = len(tags)` | 모든 태그를 포함하는 주석 |
| true | `tag.term IN (...)` | 하나 이상의 태그를 포함하는 주석 |

---

## 13. SortedItems — 정렬의 이유

```go
// 소스: pkg/services/annotations/models.go (141-155행)
func (s SortedItems) Less(i, j int) bool {
    if s[i].TimeEnd != s[j].TimeEnd {
        return s[i].TimeEnd > s[j].TimeEnd  // 종료 시간 내림차순
    }
    return s[i].Time > s[j].Time            // 시작 시간 내림차순
}
```

**왜 종료 시간 우선 내림차순인가?**

CompositeStore가 여러 소스의 결과를 병합할 때 통일된 정렬이 필요하다. 종료 시간(TimeEnd)이 같으면 시작 시간으로 비교한다. 내림차순이므로 최신 주석이 먼저 나온다. 이는 대시보드에서 "최근 이벤트"를 먼저 표시하는 UX 요구사항을 반영한다.

---

## 14. 에러 처리 패턴

```go
// 소스: pkg/services/dashboardsnapshots/errors.go
var (
    ErrBaseNotFound = errors.New("annotations.base-not-found")
)
```

```
에러 전파 경로:
    Store → Repository → HTTP Handler

    Store 에러:
    ├── SQL 에러 → 그대로 전파
    ├── 권한 없음 → accesscontrol 에러
    └── 데이터 없음 → ErrBaseNotFound

    CompositeStore 에러:
    ├── 모든 Reader 실패 → 에러 반환
    ├── 일부 Reader 실패 → 부분 결과 + 경고 로그
    └── 패닉 → handleJobPanic으로 복구 + 에러 변환
```

---

## 15. 성능 고려사항

| 최적화 | 구현 | 효과 |
|--------|------|------|
| 접근 제어 사전 검사 | 필터 없는 조회 시 먼저 카운트 | 불필요한 JOIN 방지 |
| 병렬 조회 | `concurrency.ForEachJob` | SQL + Loki 동시 조회 |
| 이중 페이지네이션 | 대시보드 → 주석 순차 페이징 | 대량 대시보드 환경 대응 |
| 고아 태그 정리 | 주석 삭제 후 참조 없는 태그 제거 | 태그 테이블 비대화 방지 |
| 만료 주석 배치 삭제 | `DELETE ... WHERE expires < ?` | 전체 스캔 대신 인덱스 활용 |

---

*검증 도구: Claude Code (Opus 4.6)*
