package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 1. 데이터소스 모델
// ---------------------------------------------------------------------------

// AccessMode — 데이터소스 접근 모드
type AccessMode string

const (
	AccessProxy  AccessMode = "proxy"  // Grafana 서버를 통해 접근
	AccessDirect AccessMode = "direct" // 브라우저에서 직접 접근
)

// DataSource — Grafana 데이터소스
type DataSource struct {
	UID      string     `json:"uid"`
	Name     string     `json:"name"`
	Type     string     `json:"type"`     // prometheus, loki, mysql, ...
	URL      string     `json:"url"`
	Access   AccessMode `json:"access"`
	IsDefault bool     `json:"isDefault"`
}

// ---------------------------------------------------------------------------
// 2. 패널 모델
// ---------------------------------------------------------------------------

// GridPos — 24컬럼 그리드에서의 패널 위치
// Grafana는 24컬럼 레이아웃을 사용하며, GRID_CELL_HEIGHT는 30px
type GridPos struct {
	X int `json:"x"` // 0~23
	Y int `json:"y"` // 행 (자동 증가)
	W int `json:"w"` // 너비 (1~24)
	H int `json:"h"` // 높이 (그리드 셀 단위)
}

// Target — 패널의 데이터 쿼리
type Target struct {
	RefID         string `json:"refId"`         // "A", "B", ...
	DatasourceUID string `json:"datasourceUid"` // 데이터소스 UID 참조
	Expr          string `json:"expr"`          // 쿼리 표현식
	LegendFormat  string `json:"legendFormat,omitempty"`
}

// Panel — 대시보드 패널
type Panel struct {
	ID          int                    `json:"id"`
	Title       string                 `json:"title"`
	Type        string                 `json:"type"` // timeseries, stat, table, gauge, ...
	GridPos     GridPos                `json:"gridPos"`
	Targets     []Target               `json:"targets"`
	Options     map[string]interface{} `json:"options,omitempty"`
	Description string                 `json:"description,omitempty"`
}

// ---------------------------------------------------------------------------
// 3. 템플릿 변수 모델
// ---------------------------------------------------------------------------

// TemplateVarType — 템플릿 변수 타입
type TemplateVarType string

const (
	VarTypeQuery    TemplateVarType = "query"
	VarTypeCustom   TemplateVarType = "custom"
	VarTypeInterval TemplateVarType = "interval"
	VarTypeConstant TemplateVarType = "constant"
)

// TemplateVar — 대시보드 템플릿 변수
type TemplateVar struct {
	Name    string          `json:"name"`
	Type    TemplateVarType `json:"type"`
	Query   string          `json:"query,omitempty"`
	Current struct {
		Text  string `json:"text"`
		Value string `json:"value"`
	} `json:"current"`
	Options []struct {
		Text     string `json:"text"`
		Value    string `json:"value"`
		Selected bool   `json:"selected"`
	} `json:"options,omitempty"`
	Multi   bool `json:"multi"`
	Refresh int  `json:"refresh"` // 0: never, 1: on load, 2: on time range change
}

// ---------------------------------------------------------------------------
// 4. 대시보드 모델
// ---------------------------------------------------------------------------

// TimeRange — 대시보드 시간 범위
type TimeRange struct {
	From string `json:"from"` // "now-6h", "2024-01-01T00:00:00Z"
	To   string `json:"to"`   // "now"
}

// Dashboard — Grafana 대시보드
type Dashboard struct {
	UID           string        `json:"uid"`
	Title         string        `json:"title"`
	Description   string        `json:"description,omitempty"`
	Tags          []string      `json:"tags,omitempty"`
	SchemaVersion int           `json:"schemaVersion"` // 현재 39
	Version       int           `json:"version"`       // 낙관적 잠금용
	Time          TimeRange     `json:"time"`
	Panels        []Panel       `json:"panels"`
	Templating    struct {
		List []TemplateVar `json:"list"`
	} `json:"templating"`
	Editable  bool      `json:"editable"`
	Refresh   string    `json:"refresh,omitempty"` // "5s", "10s", "1m"
	UpdatedAt time.Time `json:"updatedAt"`
}

// ---------------------------------------------------------------------------
// 5. 대시보드 저장소 (낙관적 잠금)
// ---------------------------------------------------------------------------

// DashboardStore — 대시보드 CRUD 저장소
type DashboardStore struct {
	dashboards map[string]*Dashboard
}

// NewDashboardStore — 저장소 생성
func NewDashboardStore() *DashboardStore {
	return &DashboardStore{
		dashboards: make(map[string]*Dashboard),
	}
}

// Save — 대시보드 저장 (낙관적 잠금 적용)
func (s *DashboardStore) Save(d *Dashboard) error {
	existing, exists := s.dashboards[d.UID]

	if exists {
		// 버전 충돌 감지: 현재 저장된 버전보다 낮으면 충돌
		if d.Version != existing.Version {
			return fmt.Errorf(
				"버전 충돌: 현재 버전=%d, 요청 버전=%d (다른 사용자가 이미 수정함)",
				existing.Version, d.Version,
			)
		}
	}

	// 버전 증가 및 저장
	d.Version++
	d.UpdatedAt = time.Now()
	saved := *d // 복사본 저장
	s.dashboards[d.UID] = &saved
	return nil
}

// Get — 대시보드 조회
func (s *DashboardStore) Get(uid string) (*Dashboard, bool) {
	d, ok := s.dashboards[uid]
	if !ok {
		return nil, false
	}
	copy := *d
	return &copy, true
}

// ---------------------------------------------------------------------------
// 6. 템플릿 변수 확장
// ---------------------------------------------------------------------------

// ExpandVariables — 쿼리 내 $variable을 실제 값으로 치환
func ExpandVariables(query string, vars []TemplateVar) string {
	result := query
	for _, v := range vars {
		placeholder := "$" + v.Name
		result = strings.ReplaceAll(result, placeholder, v.Current.Value)
		// ${var} 형태도 지원
		bracketPlaceholder := "${" + v.Name + "}"
		result = strings.ReplaceAll(result, bracketPlaceholder, v.Current.Value)
	}
	return result
}

// ---------------------------------------------------------------------------
// 7. 그리드 레이아웃 시각화
// ---------------------------------------------------------------------------

// RenderGrid — 24컬럼 그리드에 패널 배치를 ASCII로 시각화
func RenderGrid(panels []Panel) string {
	// 최대 Y 좌표 계산
	maxY := 0
	for _, p := range panels {
		if end := p.GridPos.Y + p.GridPos.H; end > maxY {
			maxY = end
		}
	}

	// 그리드 생성 (24컬럼 × maxY행)
	grid := make([][]rune, maxY)
	for i := range grid {
		grid[i] = make([]rune, 24)
		for j := range grid[i] {
			grid[i][j] = '.'
		}
	}

	// 패널 배치
	panelChars := []rune{'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H'}
	for idx, p := range panels {
		ch := panelChars[idx%len(panelChars)]
		for y := p.GridPos.Y; y < p.GridPos.Y+p.GridPos.H && y < maxY; y++ {
			for x := p.GridPos.X; x < p.GridPos.X+p.GridPos.W && x < 24; x++ {
				grid[y][x] = ch
			}
		}
	}

	// 렌더링
	var sb strings.Builder
	sb.WriteString("  24-Column Grid Layout:\n")
	sb.WriteString("  ")
	for i := 0; i < 24; i++ {
		sb.WriteString(fmt.Sprintf("%d", i%10))
	}
	sb.WriteString("\n")
	sb.WriteString("  " + strings.Repeat("-", 24) + "\n")
	for y := 0; y < maxY; y++ {
		sb.WriteString(fmt.Sprintf("%2d|", y))
		for x := 0; x < 24; x++ {
			sb.WriteRune(grid[y][x])
		}
		sb.WriteString("|\n")
	}
	sb.WriteString("  " + strings.Repeat("-", 24) + "\n")

	// 범례
	sb.WriteString("  범례: ")
	for idx, p := range panels {
		ch := panelChars[idx%len(panelChars)]
		sb.WriteString(fmt.Sprintf("%c=%s  ", ch, p.Title))
	}
	sb.WriteString("\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// 8. 메인
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("==================================================")
	fmt.Println("  Grafana Dashboard Data Model Simulation")
	fmt.Println("==================================================")

	// --- 데이터소스 정의 ---
	promDS := DataSource{
		UID:       "prom-001",
		Name:      "Prometheus",
		Type:      "prometheus",
		URL:       "http://prometheus:9090",
		Access:    AccessProxy,
		IsDefault: true,
	}

	lokiDS := DataSource{
		UID:    "loki-001",
		Name:   "Loki",
		Type:   "loki",
		URL:    "http://loki:3100",
		Access: AccessProxy,
	}

	fmt.Println("\n--- 데이터소스 ---")
	for _, ds := range []DataSource{promDS, lokiDS} {
		fmt.Printf("  [%s] %s (%s) @ %s (access: %s)\n",
			ds.UID, ds.Name, ds.Type, ds.URL, ds.Access)
	}

	// --- 템플릿 변수 ---
	namespaceVar := TemplateVar{
		Name:  "namespace",
		Type:  VarTypeQuery,
		Query: "label_values(kube_pod_info, namespace)",
	}
	namespaceVar.Current.Text = "production"
	namespaceVar.Current.Value = "production"

	intervalVar := TemplateVar{
		Name: "interval",
		Type: VarTypeInterval,
	}
	intervalVar.Current.Text = "1m"
	intervalVar.Current.Value = "1m"

	// --- 대시보드 생성 ---
	dashboard := Dashboard{
		UID:           "abc-123-def",
		Title:         "Kubernetes Cluster Overview",
		Description:   "K8s 클러스터 전체 현황 대시보드",
		Tags:          []string{"kubernetes", "production"},
		SchemaVersion: 39,
		Version:       0,
		Time: TimeRange{
			From: "now-6h",
			To:   "now",
		},
		Editable: true,
		Refresh:  "30s",
		Panels: []Panel{
			{
				ID:    1,
				Title: "CPU Usage",
				Type:  "timeseries",
				GridPos: GridPos{X: 0, Y: 0, W: 12, H: 8},
				Targets: []Target{
					{
						RefID:         "A",
						DatasourceUID: promDS.UID,
						Expr:          `rate(container_cpu_usage_seconds_total{namespace="$namespace"}[$interval])`,
						LegendFormat:  "{{pod}}",
					},
				},
				Options: map[string]interface{}{
					"tooltip": map[string]interface{}{"mode": "multi"},
					"legend":  map[string]interface{}{"displayMode": "table"},
				},
			},
			{
				ID:    2,
				Title: "Memory Usage",
				Type:  "timeseries",
				GridPos: GridPos{X: 12, Y: 0, W: 12, H: 8},
				Targets: []Target{
					{
						RefID:         "A",
						DatasourceUID: promDS.UID,
						Expr:          `container_memory_working_set_bytes{namespace="$namespace"}`,
						LegendFormat:  "{{pod}}",
					},
				},
			},
			{
				ID:    3,
				Title: "Pod Count",
				Type:  "stat",
				GridPos: GridPos{X: 0, Y: 8, W: 24, H: 4},
				Targets: []Target{
					{
						RefID:         "A",
						DatasourceUID: promDS.UID,
						Expr:          `count(kube_pod_info{namespace="$namespace"})`,
					},
				},
				Options: map[string]interface{}{
					"colorMode": "value",
					"graphMode": "area",
				},
			},
		},
	}
	dashboard.Templating.List = []TemplateVar{namespaceVar, intervalVar}

	// --- JSON 직렬화 ---
	fmt.Println("\n--- 대시보드 JSON (축약) ---")
	jsonBytes, _ := json.MarshalIndent(dashboard, "  ", "  ")
	jsonStr := string(jsonBytes)
	// 처음 40줄만 출력
	lines := strings.Split(jsonStr, "\n")
	maxLines := 40
	if len(lines) < maxLines {
		maxLines = len(lines)
	}
	for _, line := range lines[:maxLines] {
		fmt.Println("  " + line)
	}
	if len(lines) > maxLines {
		fmt.Printf("  ... (총 %d줄, %d줄 생략)\n", len(lines), len(lines)-maxLines)
	}

	// --- JSON 역직렬화 ---
	fmt.Println("\n--- JSON 역직렬화 검증 ---")
	var parsed Dashboard
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		fmt.Printf("  역직렬화 실패: %v\n", err)
	} else {
		fmt.Printf("  UID: %s\n", parsed.UID)
		fmt.Printf("  Title: %s\n", parsed.Title)
		fmt.Printf("  Panels: %d개\n", len(parsed.Panels))
		fmt.Printf("  SchemaVersion: %d\n", parsed.SchemaVersion)
		fmt.Printf("  Variables: %d개\n", len(parsed.Templating.List))
	}

	// --- 그리드 레이아웃 시각화 ---
	fmt.Println("\n--- 24컬럼 그리드 레이아웃 ---")
	fmt.Print(RenderGrid(dashboard.Panels))

	// --- 템플릿 변수 확장 ---
	fmt.Println("\n--- 템플릿 변수 확장 ---")
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			expanded := ExpandVariables(target.Expr, dashboard.Templating.List)
			fmt.Printf("  패널 [%s] (RefID=%s):\n", panel.Title, target.RefID)
			fmt.Printf("    원본: %s\n", target.Expr)
			fmt.Printf("    확장: %s\n", expanded)
		}
	}

	// --- 낙관적 잠금 시뮬레이션 ---
	fmt.Println("\n--- 낙관적 잠금 (버전 충돌 감지) ---")
	store := NewDashboardStore()

	// 첫 번째 저장 (version: 0 → 1)
	fmt.Println("  [User A] 대시보드 저장...")
	if err := store.Save(&dashboard); err != nil {
		fmt.Printf("  저장 실패: %v\n", err)
	} else {
		fmt.Printf("  저장 성공 (version: %d)\n", dashboard.Version)
	}

	// User A와 User B가 동시에 수정
	userA, _ := store.Get(dashboard.UID)
	userB, _ := store.Get(dashboard.UID)
	fmt.Printf("  [User A] 대시보드 로드 (version: %d)\n", userA.Version)
	fmt.Printf("  [User B] 대시보드 로드 (version: %d)\n", userB.Version)

	// User A가 먼저 저장 (version: 1 → 2)
	userA.Title = "Updated by User A"
	fmt.Println("  [User A] 대시보드 저장...")
	if err := store.Save(userA); err != nil {
		fmt.Printf("  저장 실패: %v\n", err)
	} else {
		fmt.Printf("  저장 성공 (version: %d)\n", userA.Version)
	}

	// User B가 나중에 저장 시도 (version: 1이지만, 이미 2로 변경됨)
	userB.Title = "Updated by User B"
	fmt.Println("  [User B] 대시보드 저장 시도...")
	if err := store.Save(userB); err != nil {
		fmt.Printf("  저장 실패: %v\n", err)
	} else {
		fmt.Printf("  저장 성공 (version: %d)\n", userB.Version)
	}

	// --- 대시보드 모델 구조 요약 ---
	fmt.Println("\n--- Grafana 대시보드 모델 구조 요약 ---")
	fmt.Println(`
  Dashboard (pkg/components/simplejson)
  ├── Meta                   # 퍼미션, provisioning 정보
  │   ├── IsStarred
  │   ├── CanSave / CanEdit
  │   └── ProvisionedExternalId
  ├── Model (JSON)           # 실제 대시보드 데이터
  │   ├── uid, title, tags
  │   ├── schemaVersion      # 스키마 버전 (마이그레이션)
  │   ├── version            # 낙관적 잠금
  │   ├── time { from, to }  # 시간 범위
  │   ├── templating.list[]  # 변수
  │   ├── panels[]           # 패널 목록
  │   │   ├── gridPos        # 24컬럼 레이아웃
  │   │   ├── targets[]      # 쿼리
  │   │   └── options        # 패널별 옵션
  │   └── annotations        # 이벤트 주석
  └── Storage
      ├── dashboard table    # MySQL/PostgreSQL/SQLite
      ├── dashboard_version  # 버전 이력
      └── dashboard_provisioning  # 프로비저닝 출처
`)
}
