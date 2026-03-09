// poc-26-annotations: Grafana 주석 시스템 + CompositeStore 시뮬레이션
//
// 핵심 개념:
//   - 조직/대시보드 주석 유형 분류
//   - 시간 범위 + 태그 기반 필터링
//   - CompositeStore 병렬 조회 및 결과 병합
//   - 종료 시간 우선 정렬
//   - 유형별 정리 (alert, api, dashboard)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- 데이터 모델 ---

type AnnotationType string

const (
	OrgAnnotation       AnnotationType = "organization"
	DashboardAnnotation AnnotationType = "dashboard"
)

type Annotation struct {
	ID           int64
	OrgID        int64
	DashboardUID string
	PanelID      int64
	Text         string
	AlertID      int64
	Tags         []string
	TimeStart    int64 // epoch ms
	TimeEnd      int64 // epoch ms (0 = 단일 시점)
	Source       string // "user", "alert", "api"
}

func (a *Annotation) GetType() AnnotationType {
	if a.DashboardUID != "" {
		return DashboardAnnotation
	}
	return OrgAnnotation
}

type AnnotationQuery struct {
	OrgID        int64
	DashboardUID string
	From         int64
	To           int64
	Tags         []string
	MatchAny     bool // true: OR, false: AND
	Limit        int
}

// 정렬: 종료 시간 내림차순, 시작 시간 내림차순
type SortedAnnotations []*Annotation

func (s SortedAnnotations) Len() int { return len(s) }
func (s SortedAnnotations) Less(i, j int) bool {
	if s[i].TimeEnd != s[j].TimeEnd {
		return s[i].TimeEnd > s[j].TimeEnd
	}
	return s[i].TimeStart > s[j].TimeStart
}
func (s SortedAnnotations) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// --- ReadStore 인터페이스 ---

type ReadStore interface {
	Name() string
	Get(query AnnotationQuery) []*Annotation
}

// --- SQL Store (수동 주석 + API 주석) ---

type SQLStore struct {
	mu          sync.RWMutex
	annotations []*Annotation
	nextID      int64
}

func NewSQLStore() *SQLStore {
	return &SQLStore{nextID: 1}
}

func (s *SQLStore) Name() string { return "sql" }

func (s *SQLStore) Save(ann *Annotation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ann.ID = s.nextID
	s.nextID++
	s.annotations = append(s.annotations, ann)
}

func (s *SQLStore) Get(query AnnotationQuery) []*Annotation {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Annotation
	for _, ann := range s.annotations {
		if ann.OrgID != query.OrgID {
			continue
		}
		if query.DashboardUID != "" && ann.DashboardUID != query.DashboardUID {
			continue
		}
		if query.From > 0 && ann.TimeStart < query.From {
			continue
		}
		if query.To > 0 && ann.TimeEnd > 0 && ann.TimeEnd > query.To {
			continue
		}
		if query.To > 0 && ann.TimeEnd == 0 && ann.TimeStart > query.To {
			continue
		}
		if len(query.Tags) > 0 && !matchTags(ann.Tags, query.Tags, query.MatchAny) {
			continue
		}
		result = append(result, ann)
		if query.Limit > 0 && len(result) >= query.Limit {
			break
		}
	}
	return result
}

func matchTags(annTags, queryTags []string, matchAny bool) bool {
	tagSet := make(map[string]bool)
	for _, t := range annTags {
		tagSet[t] = true
	}
	if matchAny {
		for _, t := range queryTags {
			if tagSet[t] {
				return true
			}
		}
		return false
	}
	for _, t := range queryTags {
		if !tagSet[t] {
			return false
		}
	}
	return true
}

func (s *SQLStore) Cleanup(annotationType string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []*Annotation
	deleted := 0
	for _, ann := range s.annotations {
		remove := false
		switch annotationType {
		case "alert":
			remove = ann.AlertID > 0
		case "api":
			remove = ann.AlertID == 0 && ann.DashboardUID == ""
		case "dashboard":
			remove = ann.DashboardUID != "" && ann.AlertID == 0
		}
		if remove {
			deleted++
		} else {
			kept = append(kept, ann)
		}
	}
	s.annotations = kept
	return deleted
}

// --- Loki Store (알림 히스토리) ---

type LokiStore struct {
	annotations []*Annotation
}

func NewLokiStore() *LokiStore {
	return &LokiStore{}
}

func (s *LokiStore) Name() string { return "loki" }

func (s *LokiStore) AddAlertHistory(ann *Annotation) {
	s.annotations = append(s.annotations, ann)
}

func (s *LokiStore) Get(query AnnotationQuery) []*Annotation {
	var result []*Annotation
	for _, ann := range s.annotations {
		if ann.OrgID != query.OrgID {
			continue
		}
		if query.From > 0 && ann.TimeStart < query.From {
			continue
		}
		if query.To > 0 && ann.TimeStart > query.To {
			continue
		}
		result = append(result, ann)
	}
	return result
}

// --- CompositeStore (병렬 조회) ---

type CompositeStore struct {
	readers []ReadStore
}

func NewCompositeStore(readers ...ReadStore) *CompositeStore {
	return &CompositeStore{readers: readers}
}

func (c *CompositeStore) Get(query AnnotationQuery) []*Annotation {
	ch := make(chan []*Annotation, len(c.readers))
	var wg sync.WaitGroup

	for _, reader := range c.readers {
		wg.Add(1)
		go func(r ReadStore) {
			defer wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					fmt.Printf("  [패닉 복구] %s store 패닉: %v\n", r.Name(), rv)
					ch <- nil
				}
			}()
			items := r.Get(query)
			ch <- items
		}(reader)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	var result []*Annotation
	for items := range ch {
		result = append(result, items...)
	}

	sort.Sort(SortedAnnotations(result))
	return result
}

// --- 메인 ---

func main() {
	fmt.Println("=== Grafana 주석 시스템 시뮬레이션 ===")
	fmt.Println()

	sqlStore := NewSQLStore()
	lokiStore := NewLokiStore()
	composite := NewCompositeStore(sqlStore, lokiStore)

	now := time.Now().UnixMilli()

	// 1. 다양한 유형의 주석 생성
	fmt.Println("--- 1. 주석 생성 ---")

	// 조직 주석 (API 생성)
	sqlStore.Save(&Annotation{
		OrgID: 1, Text: "v2.0 릴리스", Tags: []string{"release", "production"},
		TimeStart: now - 3600000, Source: "api",
	})

	// 대시보드 주석 (사용자 생성)
	sqlStore.Save(&Annotation{
		OrgID: 1, DashboardUID: "srv-001", PanelID: 1,
		Text: "서버 증설 완료", Tags: []string{"infra", "server"},
		TimeStart: now - 1800000, Source: "user",
	})

	// 시간 범위 주석 (region)
	sqlStore.Save(&Annotation{
		OrgID: 1, DashboardUID: "srv-001",
		Text: "점검 기간", Tags: []string{"maintenance"},
		TimeStart: now - 7200000, TimeEnd: now - 3600000, Source: "user",
	})

	// 알림 주석 (Loki 히스토리)
	lokiStore.AddAlertHistory(&Annotation{
		OrgID: 1, DashboardUID: "srv-001", AlertID: 1,
		Text: "CPU 90% 초과", Tags: []string{"alert", "cpu"},
		TimeStart: now - 900000, Source: "alert",
	})
	lokiStore.AddAlertHistory(&Annotation{
		OrgID: 1, DashboardUID: "srv-001", AlertID: 1,
		Text: "CPU 정상 복구", Tags: []string{"alert", "cpu"},
		TimeStart: now - 600000, Source: "alert",
	})

	fmt.Printf("  SQL 주석: 3개, Loki 주석: 2개\n")

	// 2. 유형 분류
	fmt.Println()
	fmt.Println("--- 2. 주석 유형 분류 ---")
	allAnnotations := composite.Get(AnnotationQuery{OrgID: 1})
	for _, ann := range allAnnotations {
		typeStr := string(ann.GetType())
		regionStr := ""
		if ann.TimeEnd > 0 {
			regionStr = " [region]"
		}
		fmt.Printf("  [%s] %s (출처: %s)%s\n", typeStr, ann.Text, ann.Source, regionStr)
	}

	// 3. CompositeStore 병렬 조회
	fmt.Println()
	fmt.Println("--- 3. CompositeStore 병렬 조회 ---")
	start := time.Now()
	results := composite.Get(AnnotationQuery{
		OrgID:        1,
		DashboardUID: "srv-001",
	})
	elapsed := time.Since(start)
	fmt.Printf("  srv-001 대시보드 주석: %d개 (소요: %v)\n", len(results), elapsed)
	for _, ann := range results {
		fmt.Printf("    - %s (%s)\n", ann.Text, ann.Source)
	}

	// 4. 태그 필터링
	fmt.Println()
	fmt.Println("--- 4. 태그 필터링 ---")

	// AND 매칭
	andResults := composite.Get(AnnotationQuery{
		OrgID:    1,
		Tags:     []string{"alert", "cpu"},
		MatchAny: false,
	})
	fmt.Printf("  AND 매칭 (alert + cpu): %d개\n", len(andResults))

	// OR 매칭
	orResults := composite.Get(AnnotationQuery{
		OrgID:    1,
		Tags:     []string{"release", "maintenance"},
		MatchAny: true,
	})
	fmt.Printf("  OR 매칭 (release | maintenance): %d개\n", len(orResults))
	for _, ann := range orResults {
		fmt.Printf("    - %s [%s]\n", ann.Text, strings.Join(ann.Tags, ", "))
	}

	// 5. 시간 범위 조회
	fmt.Println()
	fmt.Println("--- 5. 시간 범위 조회 ---")
	timeResults := composite.Get(AnnotationQuery{
		OrgID: 1,
		From:  now - 1000000,
		To:    now,
	})
	fmt.Printf("  최근 ~16분간 주석: %d개\n", len(timeResults))
	for _, ann := range timeResults {
		ago := (now - ann.TimeStart) / 1000
		fmt.Printf("    - %s (%d초 전)\n", ann.Text, ago)
	}

	// 6. 정리 서비스
	fmt.Println()
	fmt.Println("--- 6. 유형별 정리 ---")
	beforeCount := len(sqlStore.annotations)

	alertCleaned := sqlStore.Cleanup("alert")
	apiCleaned := sqlStore.Cleanup("api")
	dashCleaned := sqlStore.Cleanup("dashboard")

	fmt.Printf("  정리 전: %d개\n", beforeCount)
	fmt.Printf("  알림 주석 정리: %d개\n", alertCleaned)
	fmt.Printf("  API 주석 정리: %d개\n", apiCleaned)
	fmt.Printf("  대시보드 주석 정리: %d개\n", dashCleaned)
	fmt.Printf("  정리 후: %d개\n", len(sqlStore.annotations))

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
