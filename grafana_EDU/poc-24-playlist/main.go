// poc-24-playlist: Grafana 재생목록 (Playlist) 시스템 시뮬레이션
//
// 핵심 개념:
//   - Playlist + PlaylistItem 관계 모델
//   - Item 타입 (dashboard_by_uid, dashboard_by_tag)
//   - 순서 보존 (Order 필드)
//   - 업데이트 시 Delete + Re-insert 패턴
//   - 조직당 최대 1000개 제한
//   - 대시보드 순환 재생 (간격 기반)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// --- 데이터 모델 ---

type Playlist struct {
	ID        int64
	UID       string
	Name      string
	Interval  string // "5m", "30s" 등
	OrgID     int64
	CreatedAt int64
	UpdatedAt int64
}

type PlaylistItem struct {
	ID         int64
	PlaylistID int64
	Type       string // dashboard_by_uid, dashboard_by_tag
	Value      string
	Order      int
	Title      string
}

type PlaylistDTO struct {
	UID      string
	Name     string
	Interval string
	Items    []PlaylistItemDTO
}

type PlaylistItemDTO struct {
	Type  string
	Value string
	Title string
}

// --- Store ---

const MaxPlaylists = 1000

type PlaylistStore struct {
	mu        sync.RWMutex
	playlists map[string]*Playlist     // UID -> Playlist
	items     map[int64][]PlaylistItem // PlaylistID -> Items
	nextID    int64
	nextItemID int64
}

func NewPlaylistStore() *PlaylistStore {
	return &PlaylistStore{
		playlists:  make(map[string]*Playlist),
		items:      make(map[int64][]PlaylistItem),
		nextID:     1,
		nextItemID: 1,
	}
}

func (s *PlaylistStore) Create(name, interval string, orgID int64, uid string, itemDefs []PlaylistItemDTO) (*Playlist, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 조직당 최대 개수 검사
	count := 0
	for _, p := range s.playlists {
		if p.OrgID == orgID {
			count++
		}
	}
	if count >= MaxPlaylists {
		return nil, fmt.Errorf("조직당 최대 %d개 재생목록 초과", MaxPlaylists)
	}

	now := time.Now().UnixMilli()
	p := &Playlist{
		ID:        s.nextID,
		UID:       uid,
		Name:      name,
		Interval:  interval,
		OrgID:     orgID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.nextID++
	s.playlists[uid] = p

	// Items 삽입 (순서 보존)
	items := make([]PlaylistItem, len(itemDefs))
	for i, def := range itemDefs {
		items[i] = PlaylistItem{
			ID:         s.nextItemID,
			PlaylistID: p.ID,
			Type:       def.Type,
			Value:      def.Value,
			Order:      i + 1,
			Title:      def.Title,
		}
		s.nextItemID++
	}
	s.items[p.ID] = items

	return p, nil
}

func (s *PlaylistStore) Update(uid string, orgID int64, name, interval string, newItems []PlaylistItemDTO) (*PlaylistDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.playlists[uid]
	if !ok || p.OrgID != orgID {
		return nil, fmt.Errorf("재생목록을 찾을 수 없음")
	}

	// Playlist 헤더 업데이트
	p.Name = name
	p.Interval = interval
	p.UpdatedAt = time.Now().UnixMilli()

	// Delete + Re-insert 패턴
	delete(s.items, p.ID)
	items := make([]PlaylistItem, len(newItems))
	for i, def := range newItems {
		items[i] = PlaylistItem{
			ID:         s.nextItemID,
			PlaylistID: p.ID,
			Type:       def.Type,
			Value:      def.Value,
			Order:      i + 1,
			Title:      def.Title,
		}
		s.nextItemID++
	}
	s.items[p.ID] = items

	// DTO 변환
	dto := &PlaylistDTO{
		UID:      p.UID,
		Name:     p.Name,
		Interval: p.Interval,
		Items:    newItems,
	}
	return dto, nil
}

func (s *PlaylistStore) Get(uid string, orgID int64) (*PlaylistDTO, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[uid]
	if !ok || p.OrgID != orgID {
		return nil, fmt.Errorf("재생목록을 찾을 수 없음")
	}

	items := s.items[p.ID]
	dtoItems := make([]PlaylistItemDTO, len(items))
	for i, item := range items {
		dtoItems[i] = PlaylistItemDTO{
			Type:  item.Type,
			Value: item.Value,
			Title: item.Title,
		}
	}

	return &PlaylistDTO{
		UID:      p.UID,
		Name:     p.Name,
		Interval: p.Interval,
		Items:    dtoItems,
	}, nil
}

func (s *PlaylistStore) ListAll(orgID int64) []PlaylistDTO {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []PlaylistDTO
	for _, p := range s.playlists {
		if p.OrgID != orgID {
			continue
		}
		items := s.items[p.ID]
		dtoItems := make([]PlaylistItemDTO, len(items))
		for i, item := range items {
			dtoItems[i] = PlaylistItemDTO{
				Type:  item.Type,
				Value: item.Value,
				Title: item.Title,
			}
		}
		result = append(result, PlaylistDTO{
			UID:      p.UID,
			Name:     p.Name,
			Interval: p.Interval,
			Items:    dtoItems,
		})
	}
	return result
}

// --- 대시보드 레지스트리 (태그 해석 지원) ---

type Dashboard struct {
	UID   string
	Title string
	Tags  []string
}

type DashboardRegistry struct {
	dashboards map[string]*Dashboard
}

func NewDashboardRegistry() *DashboardRegistry {
	return &DashboardRegistry{dashboards: make(map[string]*Dashboard)}
}

func (r *DashboardRegistry) Add(d *Dashboard) {
	r.dashboards[d.UID] = d
}

func (r *DashboardRegistry) Resolve(items []PlaylistItemDTO) []string {
	var result []string
	for _, item := range items {
		switch item.Type {
		case "dashboard_by_uid":
			if d, ok := r.dashboards[item.Value]; ok {
				result = append(result, d.Title)
			}
		case "dashboard_by_tag":
			for _, d := range r.dashboards {
				for _, tag := range d.Tags {
					if tag == item.Value {
						result = append(result, d.Title)
						break
					}
				}
			}
		}
	}
	return result
}

// --- 재생 엔진 ---

func parseInterval(interval string) time.Duration {
	interval = strings.TrimSpace(interval)
	if strings.HasSuffix(interval, "s") {
		val := interval[:len(interval)-1]
		var sec int
		fmt.Sscanf(val, "%d", &sec)
		return time.Duration(sec) * time.Second
	}
	if strings.HasSuffix(interval, "m") {
		val := interval[:len(interval)-1]
		var min int
		fmt.Sscanf(val, "%d", &min)
		return time.Duration(min) * time.Minute
	}
	return 5 * time.Second
}

func simulatePlayback(dashboards []string, interval time.Duration, cycles int) {
	idx := 0
	for i := 0; i < cycles; i++ {
		current := dashboards[idx%len(dashboards)]
		fmt.Printf("    [%s] 표시 중: %s\n", time.Now().Format("15:04:05.000"), current)
		idx++
		if i < cycles-1 {
			time.Sleep(interval)
		}
	}
}

// --- 메인 ---

func main() {
	fmt.Println("=== Grafana 재생목록 (Playlist) 시뮬레이션 ===")
	fmt.Println()

	store := NewPlaylistStore()
	registry := NewDashboardRegistry()

	// 대시보드 등록
	registry.Add(&Dashboard{UID: "srv-001", Title: "서버 모니터링", Tags: []string{"production", "server"}})
	registry.Add(&Dashboard{UID: "net-001", Title: "네트워크 상태", Tags: []string{"production", "network"}})
	registry.Add(&Dashboard{UID: "db-001", Title: "데이터베이스", Tags: []string{"production", "database"}})
	registry.Add(&Dashboard{UID: "app-001", Title: "앱 성능", Tags: []string{"production", "app"}})
	registry.Add(&Dashboard{UID: "dev-001", Title: "개발 환경", Tags: []string{"development"}})

	// 1. 재생목록 생성 (UID 기반)
	fmt.Println("--- 1. 재생목록 생성 (UID 기반) ---")
	_, err := store.Create("NOC 모니터링", "2s", 1, "noc-playlist-1", []PlaylistItemDTO{
		{Type: "dashboard_by_uid", Value: "srv-001", Title: "서버"},
		{Type: "dashboard_by_uid", Value: "net-001", Title: "네트워크"},
		{Type: "dashboard_by_uid", Value: "db-001", Title: "DB"},
	})
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}
	fmt.Println("  재생목록 'NOC 모니터링' 생성 완료")

	// 2. 재생목록 생성 (태그 기반)
	fmt.Println()
	fmt.Println("--- 2. 재생목록 생성 (태그 기반) ---")
	store.Create("프로덕션 전체", "3s", 1, "prod-playlist", []PlaylistItemDTO{
		{Type: "dashboard_by_tag", Value: "production", Title: "프로덕션"},
	})
	fmt.Println("  재생목록 '프로덕션 전체' 생성 완료")

	// 3. 재생목록 조회
	fmt.Println()
	fmt.Println("--- 3. 재생목록 조회 ---")
	dto, _ := store.Get("noc-playlist-1", 1)
	fmt.Printf("  이름: %s, 간격: %s\n", dto.Name, dto.Interval)
	fmt.Println("  아이템:")
	for i, item := range dto.Items {
		fmt.Printf("    %d. [%s] %s\n", i+1, item.Type, item.Value)
	}

	// 4. 대시보드 해석 & 순환 재생
	fmt.Println()
	fmt.Println("--- 4. NOC 재생목록 순환 재생 (2초 간격) ---")
	dashboards := registry.Resolve(dto.Items)
	interval := parseInterval(dto.Interval)
	simulatePlayback(dashboards, interval, 4)

	// 5. 태그 기반 재생
	fmt.Println()
	fmt.Println("--- 5. 태그 기반 재생목록 순환 재생 ---")
	tagDTO, _ := store.Get("prod-playlist", 1)
	tagDashboards := registry.Resolve(tagDTO.Items)
	fmt.Printf("  'production' 태그 대시보드 %d개 발견\n", len(tagDashboards))
	simulatePlayback(tagDashboards, 500*time.Millisecond, len(tagDashboards))

	// 6. 업데이트 (Delete + Re-insert)
	fmt.Println()
	fmt.Println("--- 6. 재생목록 업데이트 (순서 변경) ---")
	updated, _ := store.Update("noc-playlist-1", 1, "NOC 모니터링 v2", "1s", []PlaylistItemDTO{
		{Type: "dashboard_by_uid", Value: "db-001", Title: "DB"},
		{Type: "dashboard_by_uid", Value: "srv-001", Title: "서버"},
		{Type: "dashboard_by_uid", Value: "app-001", Title: "앱"},
	})
	fmt.Printf("  업데이트 후: %s\n", updated.Name)
	for i, item := range updated.Items {
		fmt.Printf("    %d. %s\n", i+1, item.Value)
	}

	// 7. 전체 목록 조회
	fmt.Println()
	fmt.Println("--- 7. 전체 재생목록 ---")
	all := store.ListAll(1)
	for _, p := range all {
		fmt.Printf("  - %s (UID: %s, 간격: %s, 아이템: %d개)\n",
			p.Name, p.UID, p.Interval, len(p.Items))
	}

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
