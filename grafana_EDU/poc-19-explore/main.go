package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Grafana Explore 모드 핵심 개념 PoC
//
// 이 PoC는 Grafana Explore 모드의 핵심 동작을 시뮬레이션한다:
//   1. 쿼리 실행 및 데이터소스 라우팅
//   2. 쿼리 히스토리 관리 (생성, 검색, 삭제, 즐겨찾기)
//   3. Split View 상태 관리
//   4. 조직(Org) 기반 데이터 격리
//
// 실제 소스 참조:
//   - pkg/services/queryhistory/queryhistory.go  (QueryHistoryService)
//   - pkg/services/queryhistory/models.go        (QueryHistory, QueryHistoryStar)
//   - pkg/services/query/query.go                (Query Service)
// =============================================================================

// --- 데이터 모델 (pkg/services/queryhistory/models.go 참조) ---

// QueryHistory는 Explore에서 실행한 쿼리를 저장하는 모델이다.
// 실제 Grafana에서는 xorm 태그를 사용하여 DB 테이블에 매핑한다.
type QueryHistory struct {
	ID            int64           `json:"id"`
	UID           string          `json:"uid"`
	DatasourceUID string          `json:"datasourceUid"`
	OrgID         int64           `json:"orgId"`
	CreatedBy     int64           `json:"createdBy"`
	CreatedAt     int64           `json:"createdAt"`
	Comment       string          `json:"comment"`
	Queries       json.RawMessage `json:"queries"`
}

// QueryHistoryStar는 즐겨찾기 정보를 별도 테이블로 관리한다.
type QueryHistoryStar struct {
	ID       int64  `json:"id"`
	QueryUID string `json:"queryUid"`
	UserID   int64  `json:"userId"`
}

// QueryHistoryDTO는 API 응답용 DTO이다. Starred 필드가 추가된다.
type QueryHistoryDTO struct {
	UID           string          `json:"uid"`
	DatasourceUID string          `json:"datasourceUid"`
	CreatedBy     int64           `json:"createdBy"`
	CreatedAt     int64           `json:"createdAt"`
	Comment       string          `json:"comment"`
	Queries       json.RawMessage `json:"queries"`
	Starred       bool            `json:"starred"`
}

// SearchInQueryHistoryQuery는 검색 조건이다.
type SearchInQueryHistoryQuery struct {
	DatasourceUIDs []string `json:"datasourceUids"`
	SearchString   string   `json:"searchString"`
	OnlyStarred    bool     `json:"onlyStarred"`
	Sort           string   `json:"sort"`
	Page           int      `json:"page"`
	Limit          int      `json:"limit"`
	From           int64    `json:"from"`
	To             int64    `json:"to"`
}

// --- 데이터소스 모델 ---

// DataSource는 쿼리 대상 데이터소스를 나타낸다.
type DataSource struct {
	UID  string
	Name string
	Type string // "prometheus", "loki", "tempo" 등
}

// QueryRequest는 Explore에서의 쿼리 요청이다.
type QueryRequest struct {
	DatasourceUID string
	Expression    string
	TimeRange     TimeRange
}

// TimeRange는 쿼리의 시간 범위이다.
type TimeRange struct {
	From time.Time
	To   time.Time
}

// QueryResult는 쿼리 실행 결과이다.
type QueryResult struct {
	DatasourceUID string
	Data          []DataPoint
	Logs          []LogEntry
}

type DataPoint struct {
	Timestamp time.Time
	Value     float64
}

type LogEntry struct {
	Timestamp time.Time
	Line      string
	Labels    map[string]string
}

// --- Explore 상태 (Split View 지원) ---

// ExplorePane은 Explore의 한쪽 패인 상태이다.
type ExplorePane struct {
	DatasourceUID string
	Queries       []string
	TimeRange     TimeRange
}

// ExploreState는 Explore 전체 상태이다. Split View 시 두 개의 패인을 가진다.
type ExploreState struct {
	Left  ExplorePane
	Right *ExplorePane // nil이면 Split View 아님
}

// --- QueryHistory 서비스 구현 ---

// QueryHistoryService는 쿼리 히스토리 CRUD를 담당한다.
// 실제 Grafana에서는 DB(xorm)를 사용하지만, 여기서는 인메모리로 구현.
type QueryHistoryService struct {
	mu       sync.RWMutex
	queries  map[string]*QueryHistory    // UID -> QueryHistory
	stars    map[string]map[int64]bool   // QueryUID -> UserID set
	nextID   int64
	starID   int64
}

func NewQueryHistoryService() *QueryHistoryService {
	return &QueryHistoryService{
		queries: make(map[string]*QueryHistory),
		stars:   make(map[string]map[int64]bool),
	}
}

// generateUID는 고유 식별자를 생성한다.
func generateUID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateQuery는 쿼리 히스토리에 새 항목을 추가한다.
func (s *QueryHistoryService) CreateQuery(orgID, userID int64, dsUID string, queries json.RawMessage) QueryHistoryDTO {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	uid := generateUID()
	now := time.Now().Unix()

	qh := &QueryHistory{
		ID:            s.nextID,
		UID:           uid,
		DatasourceUID: dsUID,
		OrgID:         orgID,
		CreatedBy:     userID,
		CreatedAt:     now,
		Queries:       queries,
	}

	s.queries[uid] = qh

	return QueryHistoryDTO{
		UID:           uid,
		DatasourceUID: dsUID,
		CreatedBy:     userID,
		CreatedAt:     now,
		Queries:       queries,
		Starred:       false,
	}
}

// SearchQueries는 조건에 따라 쿼리 히스토리를 검색한다.
func (s *QueryHistoryService) SearchQueries(orgID, userID int64, query SearchInQueryHistoryQuery) ([]QueryHistoryDTO, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []QueryHistoryDTO

	for _, qh := range s.queries {
		// 조직 격리: 다른 Org의 히스토리는 보이지 않는다
		if qh.OrgID != orgID {
			continue
		}

		// 사용자 격리: 자신의 히스토리만
		if qh.CreatedBy != userID {
			continue
		}

		// 데이터소스 필터
		if len(query.DatasourceUIDs) > 0 {
			found := false
			for _, dsUID := range query.DatasourceUIDs {
				if qh.DatasourceUID == dsUID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// 검색 문자열 필터
		if query.SearchString != "" {
			queryStr := string(qh.Queries)
			if !strings.Contains(strings.ToLower(queryStr), strings.ToLower(query.SearchString)) {
				continue
			}
		}

		// 시간 범위 필터
		if query.From > 0 && qh.CreatedAt < query.From {
			continue
		}
		if query.To > 0 && qh.CreatedAt > query.To {
			continue
		}

		starred := false
		if users, ok := s.stars[qh.UID]; ok {
			starred = users[userID]
		}

		// 즐겨찾기 필터
		if query.OnlyStarred && !starred {
			continue
		}

		results = append(results, QueryHistoryDTO{
			UID:           qh.UID,
			DatasourceUID: qh.DatasourceUID,
			CreatedBy:     qh.CreatedBy,
			CreatedAt:     qh.CreatedAt,
			Comment:       qh.Comment,
			Queries:       qh.Queries,
			Starred:       starred,
		})
	}

	// 정렬: 최신순
	sort.Slice(results, func(i, j int) bool {
		return results[i].CreatedAt > results[j].CreatedAt
	})

	totalCount := len(results)

	// 페이지네이션
	if query.Limit > 0 {
		start := (query.Page - 1) * query.Limit
		if start < 0 {
			start = 0
		}
		end := start + query.Limit
		if end > len(results) {
			end = len(results)
		}
		if start < len(results) {
			results = results[start:end]
		} else {
			results = nil
		}
	}

	return results, totalCount
}

// StarQuery는 쿼리를 즐겨찾기에 추가한다.
func (s *QueryHistoryService) StarQuery(userID int64, uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.queries[uid]; !ok {
		return false
	}

	if _, ok := s.stars[uid]; !ok {
		s.stars[uid] = make(map[int64]bool)
	}
	s.stars[uid][userID] = true
	return true
}

// UnstarQuery는 즐겨찾기를 해제한다.
func (s *QueryHistoryService) UnstarQuery(userID int64, uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if users, ok := s.stars[uid]; ok {
		delete(users, userID)
		return true
	}
	return false
}

// DeleteQuery는 쿼리 히스토리에서 항목을 삭제한다.
func (s *QueryHistoryService) DeleteQuery(userID int64, uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	qh, ok := s.queries[uid]
	if !ok {
		return false
	}

	// 본인 쿼리만 삭제 가능
	if qh.CreatedBy != userID {
		return false
	}

	delete(s.queries, uid)
	delete(s.stars, uid)
	return true
}

// PatchComment는 쿼리 히스토리 항목에 코멘트를 추가/수정한다.
func (s *QueryHistoryService) PatchComment(userID int64, uid, comment string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	qh, ok := s.queries[uid]
	if !ok {
		return false
	}

	if qh.CreatedBy != userID {
		return false
	}

	qh.Comment = comment
	return true
}

// DeleteStaleQueries는 지정된 시간보다 오래된 쿼리를 삭제한다.
func (s *QueryHistoryService) DeleteStaleQueries(olderThan int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	deleted := 0
	for uid, qh := range s.queries {
		if qh.CreatedAt < olderThan {
			delete(s.queries, uid)
			delete(s.stars, uid)
			deleted++
		}
	}
	return deleted
}

// --- 쿼리 실행 서비스 ---

// QueryService는 데이터소스에 쿼리를 실행하는 서비스이다.
type QueryService struct {
	dataSources map[string]*DataSource
}

func NewQueryService(dataSources []*DataSource) *QueryService {
	dsMap := make(map[string]*DataSource)
	for _, ds := range dataSources {
		dsMap[ds.UID] = ds
	}
	return &QueryService{dataSources: dsMap}
}

// ExecuteQuery는 데이터소스에 쿼리를 실행하고 결과를 반환한다.
func (qs *QueryService) ExecuteQuery(req QueryRequest) (*QueryResult, error) {
	ds, ok := qs.dataSources[req.DatasourceUID]
	if !ok {
		return nil, fmt.Errorf("데이터소스를 찾을 수 없음: %s", req.DatasourceUID)
	}

	result := &QueryResult{
		DatasourceUID: req.DatasourceUID,
	}

	// 데이터소스 유형에 따라 시뮬레이션 결과 생성
	switch ds.Type {
	case "prometheus":
		// 메트릭 데이터 시뮬레이션
		now := time.Now()
		for i := 0; i < 5; i++ {
			result.Data = append(result.Data, DataPoint{
				Timestamp: now.Add(time.Duration(-i) * time.Minute),
				Value:     float64(100 + i*10),
			})
		}
	case "loki":
		// 로그 데이터 시뮬레이션
		now := time.Now()
		for i := 0; i < 3; i++ {
			result.Logs = append(result.Logs, LogEntry{
				Timestamp: now.Add(time.Duration(-i) * time.Second),
				Line:      fmt.Sprintf("[INFO] 요청 처리 완료 (응답시간: %dms)", 50+i*20),
				Labels:    map[string]string{"app": "web", "env": "prod"},
			})
		}
	}

	fmt.Printf("  [QueryService] 데이터소스 '%s' (%s)에서 쿼리 실행: %s\n",
		ds.Name, ds.Type, req.Expression)

	return result, nil
}

// --- 메인 실행 ---

func main() {
	fmt.Println("=== Grafana Explore 모드 PoC ===")
	fmt.Println()

	// 1. 데이터소스 설정
	dataSources := []*DataSource{
		{UID: "prom-1", Name: "Prometheus", Type: "prometheus"},
		{UID: "loki-1", Name: "Loki", Type: "loki"},
		{UID: "tempo-1", Name: "Tempo", Type: "tempo"},
	}

	queryService := NewQueryService(dataSources)
	historyService := NewQueryHistoryService()

	// 시뮬레이션용 사용자/조직
	orgID := int64(1)
	userID := int64(42)
	otherOrgID := int64(2)
	otherUserID := int64(99)

	// -------------------------------------------------------
	// 2. 쿼리 실행 시뮬레이션
	// -------------------------------------------------------
	fmt.Println("--- [1] 쿼리 실행 ---")

	queries := []QueryRequest{
		{DatasourceUID: "prom-1", Expression: `rate(http_requests_total{status="500"}[5m])`},
		{DatasourceUID: "loki-1", Expression: `{app="web"} |= "error"`},
		{DatasourceUID: "prom-1", Expression: `histogram_quantile(0.99, rate(http_duration_seconds_bucket[5m]))`},
	}

	for _, q := range queries {
		result, err := queryService.ExecuteQuery(q)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}

		// 쿼리 히스토리에 저장
		queryJSON, _ := json.Marshal(map[string]string{"expr": q.Expression})
		dto := historyService.CreateQuery(orgID, userID, q.DatasourceUID, queryJSON)
		fmt.Printf("  히스토리 저장됨: UID=%s\n", dto.UID)

		if len(result.Data) > 0 {
			fmt.Printf("  결과: %d개 데이터 포인트 (첫 번째 값: %.1f)\n", len(result.Data), result.Data[0].Value)
		}
		if len(result.Logs) > 0 {
			fmt.Printf("  결과: %d개 로그 라인 (첫 번째: %s)\n", len(result.Logs), result.Logs[0].Line)
		}
	}

	// 다른 조직의 쿼리도 생성 (격리 테스트용)
	otherQueryJSON, _ := json.Marshal(map[string]string{"expr": "up"})
	historyService.CreateQuery(otherOrgID, otherUserID, "prom-1", otherQueryJSON)
	fmt.Println()

	// -------------------------------------------------------
	// 3. 쿼리 히스토리 검색
	// -------------------------------------------------------
	fmt.Println("--- [2] 쿼리 히스토리 검색 ---")

	// 전체 검색
	results, total := historyService.SearchQueries(orgID, userID, SearchInQueryHistoryQuery{
		Page:  1,
		Limit: 10,
	})
	fmt.Printf("  전체 검색: %d개 결과 (총 %d건)\n", len(results), total)
	for _, r := range results {
		fmt.Printf("    - [%s] DS: %s, Starred: %v\n", r.UID, r.DatasourceUID, r.Starred)
	}

	// 데이터소스 필터 검색
	results, total = historyService.SearchQueries(orgID, userID, SearchInQueryHistoryQuery{
		DatasourceUIDs: []string{"loki-1"},
		Page:           1,
		Limit:          10,
	})
	fmt.Printf("  Loki 검색: %d개 결과 (총 %d건)\n", len(results), total)

	// 조직 격리 확인: Org2 사용자는 Org1의 히스토리를 볼 수 없다
	results, total = historyService.SearchQueries(otherOrgID, otherUserID, SearchInQueryHistoryQuery{
		Page:  1,
		Limit: 10,
	})
	fmt.Printf("  Org2 검색: %d개 결과 (Org1 히스토리는 보이지 않음)\n", len(results))
	fmt.Println()

	// -------------------------------------------------------
	// 4. 즐겨찾기 기능
	// -------------------------------------------------------
	fmt.Println("--- [3] 즐겨찾기 기능 ---")

	// 전체 결과에서 첫 번째 쿼리를 즐겨찾기
	allResults, _ := historyService.SearchQueries(orgID, userID, SearchInQueryHistoryQuery{
		Page:  1,
		Limit: 10,
	})
	if len(allResults) > 0 {
		targetUID := allResults[0].UID
		ok := historyService.StarQuery(userID, targetUID)
		fmt.Printf("  즐겨찾기 추가: UID=%s, 성공=%v\n", targetUID, ok)

		// 즐겨찾기만 검색
		starred, total := historyService.SearchQueries(orgID, userID, SearchInQueryHistoryQuery{
			OnlyStarred: true,
			Page:        1,
			Limit:       10,
		})
		fmt.Printf("  즐겨찾기만 검색: %d개 (총 %d건)\n", len(starred), total)

		// 즐겨찾기 해제
		historyService.UnstarQuery(userID, targetUID)
		fmt.Println("  즐겨찾기 해제 완료")
	}
	fmt.Println()

	// -------------------------------------------------------
	// 5. 코멘트 추가
	// -------------------------------------------------------
	fmt.Println("--- [4] 코멘트 기능 ---")

	if len(allResults) > 0 {
		targetUID := allResults[0].UID
		ok := historyService.PatchComment(userID, targetUID, "장애 원인 분석용 쿼리")
		fmt.Printf("  코멘트 추가: UID=%s, 성공=%v\n", targetUID, ok)
	}
	fmt.Println()

	// -------------------------------------------------------
	// 6. Split View 상태 시뮬레이션
	// -------------------------------------------------------
	fmt.Println("--- [5] Split View ---")

	state := ExploreState{
		Left: ExplorePane{
			DatasourceUID: "prom-1",
			Queries:       []string{`rate(http_requests_total[5m])`},
			TimeRange: TimeRange{
				From: time.Now().Add(-1 * time.Hour),
				To:   time.Now(),
			},
		},
		Right: &ExplorePane{
			DatasourceUID: "loki-1",
			Queries:       []string{`{app="web"} |= "error"`},
			TimeRange: TimeRange{
				From: time.Now().Add(-1 * time.Hour),
				To:   time.Now(),
			},
		},
	}

	fmt.Printf("  Left Pane: DS=%s, Query=%s\n", state.Left.DatasourceUID, state.Left.Queries[0])
	if state.Right != nil {
		fmt.Printf("  Right Pane: DS=%s, Query=%s\n", state.Right.DatasourceUID, state.Right.Queries[0])
		fmt.Println("  시간 범위 동기화: 양쪽 패인 동일한 1시간 범위 사용")
	}
	fmt.Println()

	// -------------------------------------------------------
	// 7. 오래된 쿼리 정리
	// -------------------------------------------------------
	fmt.Println("--- [6] 오래된 쿼리 정리 ---")

	// 1시간 전보다 오래된 쿼리 삭제 (현재 쿼리는 방금 생성했으므로 삭제 대상 없음)
	deleted := historyService.DeleteStaleQueries(time.Now().Add(-1 * time.Hour).Unix())
	fmt.Printf("  1시간 전 기준 정리: %d건 삭제\n", deleted)

	// 미래 시간으로 정리 (모든 쿼리 삭제 테스트)
	deleted = historyService.DeleteStaleQueries(time.Now().Add(1 * time.Hour).Unix())
	fmt.Printf("  전체 정리: %d건 삭제\n", deleted)

	// 최종 확인
	results, total = historyService.SearchQueries(orgID, userID, SearchInQueryHistoryQuery{
		Page:  1,
		Limit: 10,
	})
	fmt.Printf("  정리 후 남은 히스토리: %d건\n", total)
	fmt.Println()

	fmt.Println("=== Explore 모드 PoC 완료 ===")
}
