# 26. 검색 시스템 & 라이브 채널 Deep Dive

> Grafana 소스 기준: `pkg/services/search/`, `pkg/services/live/`
> 작성일: 2026-03-08

---

## Part A: 검색 시스템 (Search)

## 1. 개요

Grafana 검색 시스템은 **대시보드와 폴더를 검색**하는 기능이다. 제목, 태그, 즐겨찾기, 정렬 옵션 등을 지원하며, 대시보드 서비스에 검색을 위임하는 패턴을 사용한다.

### 아키텍처

```
pkg/services/search/
├── service.go           # SearchService, Query, Service 인터페이스
├── service_test.go
├── model/
│   └── model.go         # Hit, HitList, SortOption, 필터 인터페이스
└── sort/
    └── sorting.go       # Sort Service, 정렬 옵션 등록
```

## 2. 검색 흐름

```go
// 소스: pkg/services/search/service.go (70-141행)
func (s *SearchService) SearchHandler(ctx context.Context, query *Query) (model.HitList, error) {
    // 1. 사용자의 즐겨찾기(Star) 목록 조회
    staredDashIDs, _ := s.starService.GetByUser(ctx, &starQuery)

    // 2. 즐겨찾기 필터 최적화
    if query.IsStarred && len(staredDashIDs.UserStars) == 0 {
        return model.HitList{}, nil  // 즐겨찾기가 없으면 빈 결과
    }

    // 3. 대시보드 서비스에 검색 위임
    hits, _ := s.dashboardService.SearchDashboards(ctx, &dashboardQuery)

    // 4. 정렬 (미지정 시 기본 정렬: 폴더 우선 + 알파벳)
    if query.Sort == "" {
        hits = sortedHits(hits)
    }

    // 5. 즐겨찾기 표시
    for _, dashboard := range hits {
        if _, ok := staredDashIDs.UserStars[dashboard.UID]; ok {
            dashboard.IsStarred = true
        }
    }

    return hits, nil
}
```

### 기본 정렬 로직 — 폴더가 항상 먼저

```go
// 소스: pkg/services/search/model/model.go (89-101행)
func (s HitList) Less(i, j int) bool {
    if s[i].Type == "dash-folder" && s[j].Type == "dash-db" {
        return true   // 폴더가 대시보드보다 항상 먼저
    }
    if s[i].Type == "dash-db" && s[j].Type == "dash-folder" {
        return false
    }
    return strings.ToLower(s[i].Title) < strings.ToLower(s[j].Title)
}
```

### 정렬 옵션 시스템

```go
// 소스: pkg/services/search/sort/sorting.go
var SortAlphaAsc = model.SortOption{
    Name: "alpha-asc", DisplayName: "Alphabetically (A-Z)",
}
var SortAlphaDesc = model.SortOption{
    Name: "alpha-desc", DisplayName: "Alphabetically (Z-A)",
}

// 확장 가능한 정렬 등록
func (s *Service) RegisterSortOption(option model.SortOption) {
    s.sortOptions[option.Name] = option
}
```

### Hit 모델

```go
// 소스: pkg/services/search/model/model.go (65-85행)
type Hit struct {
    ID          int64    `json:"id"`
    UID         string   `json:"uid"`
    Title       string   `json:"title"`
    Type        HitType  `json:"type"`     // "dash-db", "dash-folder"
    Tags        []string `json:"tags"`
    IsStarred   bool     `json:"isStarred"`
    FolderUID   string   `json:"folderUid"`
    FolderTitle string   `json:"folderTitle"`
    SortMeta    int64    `json:"sortMeta"`
    IsDeleted   bool     `json:"isDeleted"`
}
```

---

## Part B: 라이브 채널 (Live Channel / WebSocket)

## 3. 개요

Grafana Live는 **WebSocket 기반 실시간 데이터 스트리밍** 시스템이다. 채널 규칙(Rule)을 기반으로 데이터를 변환, 처리, 출력하는 파이프라인을 구성한다.

### 핵심 개념

| 개념 | 설명 |
|------|------|
| Channel | `scope/namespace/path` 형식의 주소 |
| Rule | 채널 패턴에 매칭되는 처리 규칙 |
| Pipeline | 규칙에 따른 데이터 처리 파이프라인 |
| Converter | 원시 바이트 → DataFrame 변환 |
| FrameProcessor | DataFrame 가공 |
| FrameOutputter | DataFrame 출력 (publish, remote write 등) |

## 4. 파이프라인 아키텍처

```
입력 데이터 (bytes)
    │
    ▼
┌───────────────┐
│  Rule Getter  │  채널 패턴 매칭 → 규칙 조회
└───────┬───────┘
        │
        ▼
┌───────────────────────────────────────────────┐
│                 Pipeline                       │
│                                               │
│  DataOutputters → 원시 데이터 출력              │
│       │                                       │
│       ▼                                       │
│  Converter → bytes를 []ChannelFrame으로 변환    │
│       │                                       │
│       ▼                                       │
│  FrameProcessors → 프레임 가공 (필드 필터 등)    │
│       │                                       │
│       ▼                                       │
│  FrameOutputters → 최종 출력 (publish, 저장)    │
└───────────────────────────────────────────────┘
```

### LiveChannelRule 구조

```go
// 소스: pkg/services/live/pipeline/pipeline.go (126-166행)
type LiveChannelRule struct {
    Namespace   string              // K8s 네임스페이스
    Pattern     string              // 채널 패턴 (httprouter 스타일)
    SubscribeAuth  SubscribeAuthChecker  // 구독 인가
    Subscribers    []Subscriber          // 구독 핸들러
    PublishAuth    PublishAuthChecker    // 발행 인가
    DataOutputters []DataOutputter       // 원시 데이터 출력
    Converter      Converter             // 데이터 → 프레임 변환
    FrameProcessors []FrameProcessor     // 프레임 가공
    FrameOutputters []FrameOutputter     // 프레임 출력
}
```

### 재귀 방지 (채널 루핑)

```go
// 소스: pkg/services/live/pipeline/pipeline.go (315-341행)
var errChannelRecursion = errors.New("channel recursion")

func (p *Pipeline) processChannelDataList(ctx, ns, channelID, channelDataList, visitedChannels) error {
    for _, channelData := range channelDataList {
        if _, ok := visitedChannels[nextChannel]; ok {
            return fmt.Errorf("%w: %s", errChannelRecursion, nextChannel)
        }
        visitedChannels[nextChannel] = struct{}{}
        // ... 재귀 처리
    }
}
```

---

## 5. PoC

- [poc-27-search/main.go](./poc-27-search/main.go): 검색 + 정렬 + 즐겨찾기
- [poc-28-live-pipeline/main.go](./poc-28-live-pipeline/main.go): 라이브 파이프라인 시뮬레이션

---

---

## 6. 실제 소스 코드 심화 분석

### 6.1 SearchService 의존성 주입 (Wire)

```go
// 소스: pkg/services/search/service.go (23-34행)
func ProvideService(cfg *setting.Cfg, sqlstore db.DB,
    starService star.Service,
    dashboardService dashboards.DashboardService,
    folderService folder.Service,
    features featuremgmt.FeatureToggles,
    sortService grafanasort.Service) *SearchService {

    s := &SearchService{
        Cfg:              cfg,
        sqlstore:         sqlstore,
        starService:      starService,
        folderService:    folderService,
        features:         features,
        dashboardService: dashboardService,
        sortService:      sortService,
    }
    return s
}
```

Grafana의 Wire DI 시스템을 통해 SearchService가 생성된다. 핵심 의존성은 다음과 같다:

| 의존성 | 역할 |
|--------|------|
| `starService` | 사용자별 즐겨찾기 조회 |
| `dashboardService` | 실제 대시보드 검색 위임 대상 |
| `folderService` | 폴더 관련 조회 |
| `features` | Feature Toggle 확인 |
| `sortService` | 정렬 옵션 관리 |

### 6.2 Query 구조체 전체 필드

```go
// 소스: pkg/services/search/service.go (36-53행)
type Query struct {
    Title         string
    Tags          []string
    OrgId         int64
    SignedInUser  *user.SignedInUser
    Limit         int64
    Page          int64
    IsStarred     bool
    IsDeleted     bool
    Type          string
    DashboardUIDs []string
    DashboardIds  []int64
    // Deprecated: use FolderUID instead
    FolderIds  []int64
    FolderUIDs []string
    Permission dashboardaccess.PermissionType
    Sort       string
}
```

**왜 FolderIds가 Deprecated인가?**

Grafana는 숫자 기반 ID에서 UID(문자열) 기반 식별자로 전환 중이다. UID는 인스턴스 간 이식성이 높고, 데이터베이스 마이그레이션 시에도 안정적이다. `FolderUIDs`가 권장되며, `FolderIds`는 하위 호환성을 위해 유지된다.

### 6.3 Service 인터페이스

```go
// 소스: pkg/services/search/service.go (55-58행)
type Service interface {
    SearchHandler(context.Context, *Query) (model.HitList, error)
    SortOptions() []model.SortOption
}
```

검색 서비스는 두 개의 메서드만 노출한다. `SearchHandler`는 실제 검색을, `SortOptions`는 사용 가능한 정렬 옵션 목록을 반환한다.

### 6.4 OpenTelemetry 트레이싱 통합

```go
// 소스: pkg/services/search/service.go (21행, 70-72행)
var tracer = otel.Tracer("github.com/grafana/grafana/pkg/services/search")

func (s *SearchService) SearchHandler(ctx context.Context, query *Query) (model.HitList, error) {
    ctx, span := tracer.Start(ctx, "search.SearchHandler")
    defer span.End()
    // ...
}
```

**왜 OpenTelemetry를 사용하는가?**

검색 요청은 대시보드 서비스, 스타 서비스 등 여러 하위 서비스를 호출한다. 분산 트레이싱으로 각 단계의 지연시간을 측정하여 성능 병목을 식별할 수 있다. `tracer.Start`는 span을 생성하고, `defer span.End()`로 함수 종료 시 자동으로 span을 완료한다.

---

## 7. 즐겨찾기 필터 최적화

```go
// 소스: pkg/services/search/service.go (87-92행)
// filter by starred dashboard IDs when starred dashboards are requested
// and no UID or ID filters are specified to improve query performance
if query.IsStarred && len(query.DashboardIds) == 0 && len(query.DashboardUIDs) == 0 {
    for uid := range staredDashIDs.UserStars {
        query.DashboardUIDs = append(query.DashboardUIDs, uid)
    }
}
```

**왜 이 최적화가 필요한가?**

즐겨찾기 검색 시, 전체 대시보드를 조회한 후 필터링하면 성능이 나쁘다. 대신 즐겨찾기 UID 목록을 미리 `DashboardUIDs` 필터에 주입하여 SQL 쿼리 레벨에서 필터링한다. 이렇게 하면 DB에서 반환하는 결과 수가 크게 줄어든다.

단, UID나 ID 필터가 이미 지정된 경우에는 이 최적화를 적용하지 않는다. 기존 필터와 충돌할 수 있기 때문이다.

---

## 8. 정렬 시스템 상세

### 8.1 Sort Service 구조

```go
// 소스: pkg/services/search/sort/sorting.go (35-68행)
type Service struct {
    sortOptions map[string]model.SortOption
}

func ProvideService() Service {
    return Service{
        sortOptions: map[string]model.SortOption{
            SortAlphaAsc.Name:  SortAlphaAsc,
            SortAlphaDesc.Name: SortAlphaDesc,
        },
    }
}

func (s *Service) GetSortOption(sort string) (model.SortOption, bool) {
    option, ok := s.sortOptions[sort]
    return option, ok
}
```

**왜 정렬 서비스를 별도로 분리했는가?**

소스 코드 주석에 명시되어 있다:

> sort is separated into its own service to allow the dashboard service to use it
> in the k8s fallback, since search has a direct dependency on the dashboard service
> (and thus would create a circular dependency in wire)

Search → Dashboard → Sort 순환 의존성을 방지하기 위해 Sort를 독립 서비스로 분리했다.

### 8.2 SortOption 확장 구조

```go
// 소스: pkg/services/search/model/model.go (44-56행)
type SortOption struct {
    Name        string
    DisplayName string
    Description string
    Index       int              // 표시 순서
    MetaName    string           // SortMeta 필드 이름
    Filter      []SortOptionFilter
}

type SortOptionFilter interface {
    FilterOrderBy
}
```

`Filter` 필드는 SQL ORDER BY 절을 생성하는 인터페이스이다. `searchstore.TitleSorter{}`가 기본 구현으로, 대시보드 제목 기준 정렬을 수행한다.

### 8.3 SortOptions 정렬 순서

```go
// 소스: pkg/services/search/sort/sorting.go (54-63행)
func (s *Service) SortOptions() []model.SortOption {
    opts := make([]model.SortOption, 0, len(s.sortOptions))
    for _, o := range s.sortOptions {
        opts = append(opts, o)
    }
    sort.Slice(opts, func(i, j int) bool {
        return opts[i].Index < opts[j].Index ||
            (opts[i].Index == opts[j].Index && opts[i].Name < opts[j].Name)
    })
    return opts
}
```

옵션은 `Index` 기준으로 정렬되며, 동일 Index에서는 이름 알파벳순이다. 이 정렬 결과가 UI 드롭다운에 표시된다.

---

## 9. 필터 인터페이스 체계

```go
// 소스: pkg/services/search/model/model.go (8-42행)
type FilterWhere interface {
    Where() (string, []any)         // SQL WHERE 절
}
type FilterWith interface {
    With() (string, []any)          // CTE 쿼리 (재귀적 쿼리 지원)
}
type FilterGroupBy interface {
    GroupBy() (string, []any)       // GROUP BY 절
}
type FilterOrderBy interface {
    OrderBy() string                // ORDER BY 절
}
type FilterLeftJoin interface {
    LeftJoin() string               // LEFT OUTER JOIN 절
}
type FilterSelect interface {
    Select() string                 // SELECT 추가 컬럼
}
```

**왜 이렇게 세분화된 필터 인터페이스가 필요한가?**

Grafana의 검색 쿼리는 여러 조건(태그 필터, 권한 필터, 정렬)을 동적으로 조합해야 한다. 각 필터가 SQL의 다른 부분(WHERE, JOIN, ORDER BY 등)에 영향을 미치므로, 인터페이스를 세분화하여 각 필터가 필요한 SQL 구성만 제공하도록 설계했다.

---

## 10. Hit 모델 전체 필드

```go
// 소스: pkg/services/search/model/model.go (65-85행)
type Hit struct {
    ID                    int64      `json:"id"`
    UID                   string     `json:"uid"`
    OrgID                 int64      `json:"orgId"`
    Title                 string     `json:"title"`
    URI                   string     `json:"uri"`
    URL                   string     `json:"url"`
    Slug                  string     `json:"slug"`
    Type                  HitType    `json:"type"`
    Tags                  []string   `json:"tags"`
    IsStarred             bool       `json:"isStarred"`
    Description           string     `json:"description,omitempty"`
    FolderID              int64      `json:"folderId,omitempty"`    // Deprecated
    FolderUID             string     `json:"folderUid,omitempty"`
    FolderTitle           string     `json:"folderTitle,omitempty"`
    FolderURL             string     `json:"folderUrl,omitempty"`
    SortMeta              int64      `json:"sortMeta"`
    SortMetaName          string     `json:"sortMetaName,omitempty"`
    IsDeleted             bool       `json:"isDeleted"`
    PermanentlyDeleteDate *time.Time `json:"permanentlyDeleteDate,omitempty"`
}
```

`SortMeta`와 `SortMetaName`은 커스텀 정렬 시 정렬 기준 값을 반환하는 필드이다. 예를 들어, "최근 사용순" 정렬이면 `SortMeta`에 마지막 접근 타임스탬프가, `SortMetaName`에 "Last Used"가 들어간다.

### HitType 상수

```go
// 소스: pkg/services/search/model/model.go (59-63행)
const (
    DashHitDB     HitType = "dash-db"      // 일반 대시보드
    DashHitHome   HitType = "dash-home"     // 홈 대시보드
    DashHitFolder HitType = "dash-folder"   // 폴더
)
```

---

## 11. 태그 정렬

```go
// 소스: pkg/services/search/service.go (143-154행)
func sortedHits(unsorted model.HitList) model.HitList {
    hits := make(model.HitList, 0, len(unsorted))
    hits = append(hits, unsorted...)

    sort.Sort(hits)  // 폴더 우선 + 알파벳순

    for _, hit := range hits {
        sort.Strings(hit.Tags)  // 각 Hit의 태그도 알파벳순 정렬
    }

    return hits
}
```

**왜 태그도 정렬하는가?**

UI에서 태그가 일관된 순서로 표시되어야 사용자 경험이 좋다. 동일 대시보드의 태그가 매번 다른 순서로 나타나면 혼란을 준다.

---

## 12. 테스트 전략

### 12.1 정렬 테스트

```go
// 소스: pkg/services/search/service_test.go (21-63행)
func TestSearch_SortedResults(t *testing.T) {
    // 모의 대시보드 서비스가 정렬되지 않은 결과를 반환
    ds.On("SearchDashboards", ...).Return(model.HitList{
        &model.Hit{Title: "CCAA", Type: "dash-db", Tags: []string{"BB", "AA"}},
        &model.Hit{Title: "AABB", Type: "dash-db", Tags: []string{"CC", "AA"}},
        &model.Hit{Title: "BBAA", Type: "dash-db", Tags: []string{"EE", "AA", "BB"}},
        &model.Hit{Title: "bbAAa", Type: "dash-db", Tags: []string{"EE", "AA", "BB"}},
        &model.Hit{Title: "FOLDER", Type: "dash-folder"},
    }, nil)

    // 검증: 폴더가 항상 먼저, 대소문자 무시 알파벳순
    assert.Equal(t, "FOLDER", hits[0].Title)   // 폴더 우선
    assert.Equal(t, "AABB", hits[1].Title)      // 알파벳순
    assert.Equal(t, "BBAA", hits[2].Title)
    assert.Equal(t, "bbAAa", hits[3].Title)     // 대소문자 무시
    assert.Equal(t, "CCAA", hits[4].Title)

    // 태그도 알파벳순으로 정렬됨
    assert.Equal(t, "AA", hits[3].Tags[0])
    assert.Equal(t, "BB", hits[3].Tags[1])
    assert.Equal(t, "EE", hits[3].Tags[2])
}
```

### 12.2 즐겨찾기 필터링 테스트

```go
// 소스: pkg/services/search/service_test.go (65-97행)
func TestSearch_StarredResults(t *testing.T) {
    // 즐겨찾기: test, test3, test4 (test4는 대시보드에 없음)
    ss.ExpectedUserStars = &star.GetUserStarsResult{
        UserStars: map[string]bool{"test": true, "test3": true, "test4": true},
    }

    // IsStarred=true로 검색
    query := &Query{IsStarred: true, ...}
    hits, _ := svc.SearchHandler(ctx, query)

    // 즐겨찾기 중 실제 존재하는 대시보드만 반환
    assert.Equal(t, 2, hits.Len())    // test, test3만 (test4는 없음)
    assert.Equal(t, "A", hits[0].Title)
    assert.Equal(t, "C", hits[1].Title)
}
```

이 테스트는 즐겨찾기에 등록되었지만 실제로 존재하지 않는 대시보드(test4)가 결과에서 제외되는지 검증한다.

---

## 13. 성능 고려사항

| 최적화 | 구현 방식 | 효과 |
|--------|---------|------|
| 즐겨찾기 사전 필터링 | UID 목록을 SQL WHERE IN에 주입 | DB 반환 row 수 감소 |
| 빈 즐겨찾기 조기 반환 | `len(UserStars) == 0` → 즉시 빈 결과 | DB 쿼리 건너뜀 |
| 정렬 옵션 캐시 | `map[string]SortOption` | O(1) 조회 |
| 정규식 컴파일 캐시 | `regexp.Compile` 결과 재사용 | 매 요청 재컴파일 방지 |

**검색이 느린 경우 디버깅 순서:**

```
1. OpenTelemetry span으로 각 단계 지연시간 확인
2. dashboardService.SearchDashboards() 쿼리 시간 확인
3. starService.GetByUser() 응답 시간 확인
4. sort 단계의 결과 수 확인 (너무 많으면 Limit 조정)
```

---

*검증 도구: Claude Code (Opus 4.6)*
