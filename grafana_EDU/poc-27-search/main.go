// poc-27-search: Grafana 검색 시스템 시뮬레이션
//
// 핵심 개념:
//   - 대시보드/폴더 검색
//   - 폴더 우선 정렬 (dash-folder > dash-db)
//   - 즐겨찾기(Star) 필터
//   - 확장 가능한 정렬 옵션
//   - 태그 필터링
//
// 실행: go run main.go

package main

import (
	"fmt"
	"sort"
	"strings"
)

// --- 모델 ---

type HitType string

const (
	DashDB     HitType = "dash-db"
	DashFolder HitType = "dash-folder"
)

type Hit struct {
	UID         string
	Title       string
	Type        HitType
	Tags        []string
	FolderUID   string
	FolderTitle string
	IsStarred   bool
	SortMeta    int64
}

type HitList []*Hit

func (s HitList) Len() int      { return len(s) }
func (s HitList) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s HitList) Less(i, j int) bool {
	// 폴더가 항상 먼저
	if s[i].Type == DashFolder && s[j].Type == DashDB {
		return true
	}
	if s[i].Type == DashDB && s[j].Type == DashFolder {
		return false
	}
	return strings.ToLower(s[i].Title) < strings.ToLower(s[j].Title)
}

// --- 정렬 옵션 시스템 ---

type SortOption struct {
	Name        string
	DisplayName string
	SortFunc    func(a, b *Hit) bool
}

type SortService struct {
	options map[string]SortOption
}

func NewSortService() *SortService {
	s := &SortService{options: make(map[string]SortOption)}
	s.RegisterSortOption(SortOption{
		Name: "alpha-asc", DisplayName: "알파벳 (A-Z)",
		SortFunc: func(a, b *Hit) bool {
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		},
	})
	s.RegisterSortOption(SortOption{
		Name: "alpha-desc", DisplayName: "알파벳 (Z-A)",
		SortFunc: func(a, b *Hit) bool {
			return strings.ToLower(a.Title) > strings.ToLower(b.Title)
		},
	})
	return s
}

func (s *SortService) RegisterSortOption(opt SortOption) {
	s.options[opt.Name] = opt
}

func (s *SortService) GetSortOption(name string) (SortOption, bool) {
	opt, ok := s.options[name]
	return opt, ok
}

func (s *SortService) SortOptions() []SortOption {
	var opts []SortOption
	for _, o := range s.options {
		opts = append(opts, o)
	}
	return opts
}

// --- Star 서비스 ---

type StarService struct {
	stars map[int64]map[string]bool // userID -> {dashboardUID: true}
}

func NewStarService() *StarService {
	return &StarService{stars: make(map[int64]map[string]bool)}
}

func (s *StarService) Star(userID int64, uid string) {
	if s.stars[userID] == nil {
		s.stars[userID] = make(map[string]bool)
	}
	s.stars[userID][uid] = true
}

func (s *StarService) GetUserStars(userID int64) map[string]bool {
	return s.stars[userID]
}

// --- 검색 서비스 ---

type SearchQuery struct {
	Title     string
	Tags      []string
	IsStarred bool
	Sort      string
	UserID    int64
	Type      string
}

type SearchService struct {
	dashboards  HitList
	starService *StarService
	sortService *SortService
}

func NewSearchService(sortSvc *SortService, starSvc *StarService) *SearchService {
	return &SearchService{sortService: sortSvc, starService: starSvc}
}

func (s *SearchService) AddDashboard(hit *Hit) {
	s.dashboards = append(s.dashboards, hit)
}

func (s *SearchService) Search(query SearchQuery) HitList {
	// 1. 즐겨찾기 조회
	userStars := s.starService.GetUserStars(query.UserID)

	// 즐겨찾기만 보기인데 즐겨찾기가 없으면 빈 결과
	if query.IsStarred && len(userStars) == 0 {
		return HitList{}
	}

	// 2. 필터링
	var filtered HitList
	for _, hit := range s.dashboards {
		// 제목 필터
		if query.Title != "" && !strings.Contains(
			strings.ToLower(hit.Title), strings.ToLower(query.Title)) {
			continue
		}
		// 태그 필터
		if len(query.Tags) > 0 && !hasAnyTag(hit.Tags, query.Tags) {
			continue
		}
		// 타입 필터
		if query.Type != "" && string(hit.Type) != query.Type {
			continue
		}

		// 즐겨찾기 표시
		hitCopy := *hit
		if userStars[hit.UID] {
			hitCopy.IsStarred = true
		}
		filtered = append(filtered, &hitCopy)
	}

	// 3. 정렬
	if query.Sort != "" {
		if opt, ok := s.sortService.GetSortOption(query.Sort); ok {
			sort.Slice(filtered, func(i, j int) bool {
				return opt.SortFunc(filtered[i], filtered[j])
			})
		}
	} else {
		sort.Sort(filtered) // 기본 정렬: 폴더 우선 + 알파벳
	}

	// 4. 태그 정렬
	for _, hit := range filtered {
		sort.Strings(hit.Tags)
	}

	// 5. 즐겨찾기 필터
	if query.IsStarred {
		var starred HitList
		for _, hit := range filtered {
			if hit.IsStarred {
				starred = append(starred, hit)
			}
		}
		return starred
	}

	return filtered
}

func hasAnyTag(hitTags, queryTags []string) bool {
	tagSet := make(map[string]bool)
	for _, t := range hitTags {
		tagSet[t] = true
	}
	for _, t := range queryTags {
		if tagSet[t] {
			return true
		}
	}
	return false
}

// --- 메인 ---

func main() {
	fmt.Println("=== Grafana 검색 시스템 시뮬레이션 ===")
	fmt.Println()

	sortSvc := NewSortService()
	starSvc := NewStarService()
	searchSvc := NewSearchService(sortSvc, starSvc)

	// 데이터 추가
	searchSvc.AddDashboard(&Hit{UID: "f-infra", Title: "Infrastructure", Type: DashFolder})
	searchSvc.AddDashboard(&Hit{UID: "f-app", Title: "Applications", Type: DashFolder})
	searchSvc.AddDashboard(&Hit{UID: "d-srv", Title: "서버 모니터링", Type: DashDB, Tags: []string{"server", "production"}, FolderUID: "f-infra", FolderTitle: "Infrastructure"})
	searchSvc.AddDashboard(&Hit{UID: "d-net", Title: "네트워크", Type: DashDB, Tags: []string{"network", "production"}, FolderUID: "f-infra"})
	searchSvc.AddDashboard(&Hit{UID: "d-app", Title: "앱 성능", Type: DashDB, Tags: []string{"app", "production"}, FolderUID: "f-app"})
	searchSvc.AddDashboard(&Hit{UID: "d-db", Title: "데이터베이스", Type: DashDB, Tags: []string{"database"}, FolderUID: "f-infra"})
	searchSvc.AddDashboard(&Hit{UID: "d-ci", Title: "CI/CD 파이프라인", Type: DashDB, Tags: []string{"ci", "development"}, FolderUID: "f-app"})

	starSvc.Star(1, "d-srv")
	starSvc.Star(1, "d-app")

	// 1. 기본 검색 (폴더 우선 정렬)
	fmt.Println("--- 1. 기본 검색 (폴더 우선 + 알파벳) ---")
	results := searchSvc.Search(SearchQuery{UserID: 1})
	printResults(results)

	// 2. 제목 검색
	fmt.Println("--- 2. 제목 검색 ('네트') ---")
	results = searchSvc.Search(SearchQuery{Title: "네트", UserID: 1})
	printResults(results)

	// 3. 태그 검색
	fmt.Println("--- 3. 태그 검색 ('production') ---")
	results = searchSvc.Search(SearchQuery{Tags: []string{"production"}, UserID: 1})
	printResults(results)

	// 4. 즐겨찾기만
	fmt.Println("--- 4. 즐겨찾기만 ---")
	results = searchSvc.Search(SearchQuery{IsStarred: true, UserID: 1})
	printResults(results)

	// 5. 역방향 정렬
	fmt.Println("--- 5. 역방향 알파벳 정렬 ---")
	results = searchSvc.Search(SearchQuery{Sort: "alpha-desc", UserID: 1})
	printResults(results)

	// 6. 타입 필터 (폴더만)
	fmt.Println("--- 6. 폴더만 검색 ---")
	results = searchSvc.Search(SearchQuery{Type: "dash-folder", UserID: 1})
	printResults(results)

	// 7. 정렬 옵션 목록
	fmt.Println("--- 7. 사용 가능한 정렬 옵션 ---")
	for _, opt := range sortSvc.SortOptions() {
		fmt.Printf("  - %s: %s\n", opt.Name, opt.DisplayName)
	}

	// 8. 커스텀 정렬 옵션 등록
	fmt.Println()
	fmt.Println("--- 8. 커스텀 정렬 등록 (태그 수) ---")
	sortSvc.RegisterSortOption(SortOption{
		Name: "tag-count-desc", DisplayName: "태그 많은 순",
		SortFunc: func(a, b *Hit) bool {
			return len(a.Tags) > len(b.Tags)
		},
	})
	results = searchSvc.Search(SearchQuery{Sort: "tag-count-desc", UserID: 1})
	for _, h := range results {
		fmt.Printf("  [%s] %s (태그 %d개)\n", h.Type, h.Title, len(h.Tags))
	}

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}

func printResults(results HitList) {
	for _, h := range results {
		star := " "
		if h.IsStarred {
			star = "*"
		}
		fmt.Printf("  %s [%s] %s", star, h.Type, h.Title)
		if len(h.Tags) > 0 {
			fmt.Printf(" {%s}", strings.Join(h.Tags, ", "))
		}
		fmt.Println()
	}
	fmt.Println()
}
