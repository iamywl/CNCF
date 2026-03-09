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

*검증 도구: Claude Code (Opus 4.6)*
