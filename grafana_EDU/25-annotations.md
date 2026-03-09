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

*검증 도구: Claude Code (Opus 4.6)*
