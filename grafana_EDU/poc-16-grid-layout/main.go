package main

import (
	"fmt"
	"sort"
	"strings"
)

// ============================================================
// Grafana 대시보드 24컬럼 그리드 레이아웃 시뮬레이션
// 충돌 감지, 자동 배치, 행 패널, ASCII 렌더링 구현
// ============================================================

// --- 상수 ---

const (
	GridColumnCount = 24 // 그리드 컬럼 수
	GridCellHeight  = 36 // 셀 높이 (px)
	GridCellVMargin = 10 // 셀 수직 간격 (px)
	GridCellWidth   = 60 // 셀 너비 (px, 1440px / 24)
)

// --- 데이터 구조 ---

// GridPos는 그리드 내 패널의 위치와 크기를 나타낸다.
// 실제 구현: public/app/types/dashboard.ts
type GridPos struct {
	X int // 시작 컬럼 (0-23)
	Y int // 시작 행
	W int // 너비 (컬럼 수)
	H int // 높이 (행 수)
}

// PixelRect는 픽셀 좌표로 변환된 영역.
type PixelRect struct {
	X      int
	Y      int
	Width  int
	Height int
}

// Panel은 대시보드 패널을 나타낸다.
type Panel struct {
	ID       int
	Title    string
	Type     string // graph, stat, table, row, text, logs
	GridPos  GridPos
	IsRow    bool   // 행 패널 여부
	Collapsed bool  // 행 패널 접힘 여부
	Panels   []*Panel // 행 패널에 속한 하위 패널
	Repeat   string // 반복 변수 (빈 문자열이면 반복 안 함)
}

// Dashboard는 대시보드를 나타낸다.
type Dashboard struct {
	Title  string
	Panels []*Panel
}

// --- GridPos 유틸리티 ---

// ToPixels는 그리드 좌표를 픽셀 좌표로 변환한다.
func (g GridPos) ToPixels() PixelRect {
	return PixelRect{
		X:      g.X * GridCellWidth,
		Y:      g.Y * (GridCellHeight + GridCellVMargin),
		Width:  g.W * GridCellWidth,
		Height: g.H*GridCellHeight + (g.H-1)*GridCellVMargin,
	}
}

// Right는 패널의 오른쪽 끝 컬럼(exclusive)을 반환한다.
func (g GridPos) Right() int { return g.X + g.W }

// Bottom은 패널의 아래쪽 끝 행(exclusive)을 반환한다.
func (g GridPos) Bottom() int { return g.Y + g.H }

// Overlaps는 두 GridPos가 겹치는지 확인한다 (AABB 충돌 검출).
func (g GridPos) Overlaps(other GridPos) bool {
	if g.X >= other.Right() || other.X >= g.Right() {
		return false
	}
	if g.Y >= other.Bottom() || other.Y >= g.Bottom() {
		return false
	}
	return true
}

// IsValid는 GridPos가 유효한지 확인한다.
func (g GridPos) IsValid() bool {
	return g.X >= 0 && g.Y >= 0 &&
		g.W > 0 && g.H > 0 &&
		g.X+g.W <= GridColumnCount
}

// --- 레이아웃 엔진 ---

// LayoutEngine은 대시보드 패널 배치를 관리한다.
type LayoutEngine struct{}

// DetectCollisions는 패널 간 충돌을 검출한다.
func (le *LayoutEngine) DetectCollisions(panels []*Panel) [][2]int {
	var collisions [][2]int
	for i := 0; i < len(panels); i++ {
		for j := i + 1; j < len(panels); j++ {
			if panels[i].GridPos.Overlaps(panels[j].GridPos) {
				collisions = append(collisions, [2]int{panels[i].ID, panels[j].ID})
			}
		}
	}
	return collisions
}

// AutoLayout은 충돌이 없도록 패널을 자동 배치한다.
// 알고리즘: Y,X 순서로 정렬 후 겹침 발생 시 아래로 밀어냄.
func (le *LayoutEngine) AutoLayout(panels []*Panel) {
	// Y, X 순으로 정렬
	sort.Slice(panels, func(i, j int) bool {
		if panels[i].GridPos.Y == panels[j].GridPos.Y {
			return panels[i].GridPos.X < panels[j].GridPos.X
		}
		return panels[i].GridPos.Y < panels[j].GridPos.Y
	})

	// 각 패널에 대해 이전 패널들과의 충돌 검사 및 해결
	for i := 1; i < len(panels); i++ {
		for j := 0; j < i; j++ {
			if panels[i].GridPos.Overlaps(panels[j].GridPos) {
				// 충돌 발생: 아래로 밀어냄
				panels[i].GridPos.Y = panels[j].GridPos.Bottom()
			}
		}
	}
}

// PlacePanel은 새 패널을 빈 공간에 자동 배치한다.
func (le *LayoutEngine) PlacePanel(panels []*Panel, newPanel *Panel) {
	// Y=0부터 순서대로 빈 공간 탐색
	for y := 0; ; y++ {
		for x := 0; x <= GridColumnCount-newPanel.GridPos.W; x++ {
			candidate := GridPos{X: x, Y: y, W: newPanel.GridPos.W, H: newPanel.GridPos.H}
			fits := true
			for _, p := range panels {
				if candidate.Overlaps(p.GridPos) {
					fits = false
					break
				}
			}
			if fits {
				newPanel.GridPos.X = x
				newPanel.GridPos.Y = y
				return
			}
		}
	}
}

// RepeatPanels는 템플릿 변수 값에 따라 패널을 반복 생성한다.
func (le *LayoutEngine) RepeatPanels(panel *Panel, values []string, nextID *int) []*Panel {
	var result []*Panel
	for i, val := range values {
		p := &Panel{
			ID:    *nextID,
			Title: fmt.Sprintf("%s (%s)", panel.Title, val),
			Type:  panel.Type,
			GridPos: GridPos{
				X: (i * panel.GridPos.W) % GridColumnCount,
				Y: panel.GridPos.Y + (i*panel.GridPos.W)/GridColumnCount*panel.GridPos.H,
				W: panel.GridPos.W,
				H: panel.GridPos.H,
			},
		}
		*nextID++
		result = append(result, p)
	}
	return result
}

// CollapseRow는 행 패널을 접는다 (하위 패널 숨김).
func (le *LayoutEngine) CollapseRow(row *Panel, allPanels []*Panel) []*Panel {
	if !row.IsRow {
		return allPanels
	}
	row.Collapsed = true

	// 행 패널 아래에 있는 패널들을 행에 소속시킴
	rowBottom := row.GridPos.Bottom()
	nextRowY := -1

	// 다음 행 패널 찾기
	for _, p := range allPanels {
		if p.IsRow && p.ID != row.ID && p.GridPos.Y > row.GridPos.Y {
			if nextRowY == -1 || p.GridPos.Y < nextRowY {
				nextRowY = p.GridPos.Y
			}
		}
	}

	var remaining []*Panel
	for _, p := range allPanels {
		if p.ID == row.ID {
			remaining = append(remaining, p)
			continue
		}
		if p.GridPos.Y >= rowBottom && (nextRowY == -1 || p.GridPos.Y < nextRowY) {
			// 이 패널은 접힌 행에 속함 → 숨김
			row.Panels = append(row.Panels, p)
		} else {
			remaining = append(remaining, p)
		}
	}

	return remaining
}

// ExpandRow는 행 패널을 펼친다.
func (le *LayoutEngine) ExpandRow(row *Panel) []*Panel {
	if !row.IsRow || !row.Collapsed {
		return nil
	}
	row.Collapsed = false
	expanded := row.Panels
	row.Panels = nil
	return expanded
}

// --- ASCII 그리드 렌더링 ---

// RenderGrid는 대시보드를 ASCII 아트로 렌더링한다.
func RenderGrid(dash *Dashboard) {
	// 그리드 최대 높이 계산
	maxY := 0
	for _, p := range dash.Panels {
		if p.GridPos.Bottom() > maxY {
			maxY = p.GridPos.Bottom()
		}
	}

	if maxY == 0 {
		fmt.Println("(빈 대시보드)")
		return
	}

	// 그리드 배열 생성 (각 셀에 패널 ID 저장, 0=빈 칸)
	grid := make([][]int, maxY)
	for y := 0; y < maxY; y++ {
		grid[y] = make([]int, GridColumnCount)
	}

	// 패널을 그리드에 배치
	for _, p := range dash.Panels {
		for y := p.GridPos.Y; y < p.GridPos.Bottom() && y < maxY; y++ {
			for x := p.GridPos.X; x < p.GridPos.Right() && x < GridColumnCount; x++ {
				grid[y][x] = p.ID
			}
		}
	}

	// 패널 ID → 제목 맵
	titleMap := make(map[int]string)
	for _, p := range dash.Panels {
		titleMap[p.ID] = p.Title
	}

	// 렌더링: 2문자 단위로 각 컬럼 표현
	cellW := 3 // 각 컬럼당 문자 수
	totalW := GridColumnCount * cellW + 1

	fmt.Printf("\nASCII 그리드 (%d columns x %d rows):\n", GridColumnCount, maxY)

	// 패널별 표시 문자 (0-9, A-Z)
	panelChar := func(id int) byte {
		if id == 0 {
			return ' '
		}
		if id <= 9 {
			return byte('0' + id)
		}
		if id <= 35 {
			return byte('A' + id - 10)
		}
		return '#'
	}

	// 상단 경계
	fmt.Println(strings.Repeat("-", totalW))

	for y := 0; y < maxY; y++ {
		line := "|"
		for x := 0; x < GridColumnCount; x++ {
			ch := panelChar(grid[y][x])
			line += string([]byte{ch, ch, ch})
		}
		line += "|"

		// 행 레이블: 이 Y에서 시작하는 패널 표시
		var labels []string
		for _, p := range dash.Panels {
			if p.GridPos.Y == y {
				labels = append(labels, fmt.Sprintf("%c=%s(%dx%d)",
					panelChar(p.ID), p.Title, p.GridPos.W, p.GridPos.H))
			}
		}

		if len(labels) > 0 {
			line += "  " + strings.Join(labels, ", ")
		}

		fmt.Println(line)
	}

	// 하단 경계
	fmt.Println(strings.Repeat("-", totalW))

	// 범례
	fmt.Println("\n범례:")
	for _, p := range dash.Panels {
		px := p.GridPos.ToPixels()
		fmt.Printf("  %c: %-20s GridPos(X=%d, Y=%d, W=%d, H=%d) → Pixel(%dpx, %dpx, %dpx x %dpx)\n",
			panelChar(p.ID), p.Title,
			p.GridPos.X, p.GridPos.Y, p.GridPos.W, p.GridPos.H,
			px.X, px.Y, px.Width, px.Height)
	}
}

// --- 메인: 시뮬레이션 ---

func main() {
	fmt.Println("=== Grafana 그리드 레이아웃 시뮬레이션 ===")

	engine := LayoutEngine{}

	// ------------------------------------------
	// 1. 기본 대시보드 생성
	// ------------------------------------------
	fmt.Println("\n--- 1. 대시보드 생성 (6 panels) ---")

	dash := &Dashboard{
		Title: "Production Overview",
		Panels: []*Panel{
			{ID: 1, Title: "CPU Usage", Type: "graph", GridPos: GridPos{X: 0, Y: 0, W: 12, H: 4}},
			{ID: 2, Title: "Memory", Type: "graph", GridPos: GridPos{X: 12, Y: 0, W: 12, H: 4}},
			{ID: 3, Title: "Disk I/O", Type: "stat", GridPos: GridPos{X: 0, Y: 4, W: 8, H: 4}},
			{ID: 4, Title: "Network", Type: "graph", GridPos: GridPos{X: 8, Y: 4, W: 8, H: 4}},
			{ID: 5, Title: "Logs", Type: "logs", GridPos: GridPos{X: 16, Y: 4, W: 8, H: 4}},
			{ID: 6, Title: "Alerts", Type: "table", GridPos: GridPos{X: 0, Y: 8, W: 24, H: 6}},
		},
	}

	fmt.Printf("[대시보드] %s (%d panels)\n", dash.Title, len(dash.Panels))
	RenderGrid(dash)

	// ------------------------------------------
	// 2. 충돌 감지 및 해결
	// ------------------------------------------
	fmt.Println("\n--- 2. 충돌 감지 및 자동 해결 ---")

	conflictDash := &Dashboard{
		Title: "Conflict Test",
		Panels: []*Panel{
			{ID: 1, Title: "Panel A", Type: "graph", GridPos: GridPos{X: 0, Y: 0, W: 12, H: 4}},
			{ID: 2, Title: "Panel B", Type: "graph", GridPos: GridPos{X: 6, Y: 0, W: 12, H: 4}}, // A와 충돌!
			{ID: 3, Title: "Panel C", Type: "graph", GridPos: GridPos{X: 0, Y: 2, W: 8, H: 4}},  // A와 충돌!
		},
	}

	collisions := engine.DetectCollisions(conflictDash.Panels)
	fmt.Printf("\n충돌 감지: %d건\n", len(collisions))
	for _, c := range collisions {
		fmt.Printf("  패널 %d ↔ 패널 %d 충돌\n", c[0], c[1])
	}

	fmt.Println("\n[자동 배치 전]")
	RenderGrid(conflictDash)

	engine.AutoLayout(conflictDash.Panels)

	collisions = engine.DetectCollisions(conflictDash.Panels)
	fmt.Printf("\n자동 배치 후 충돌: %d건\n", len(collisions))
	fmt.Println("\n[자동 배치 후]")
	RenderGrid(conflictDash)

	// ------------------------------------------
	// 3. 패널 자동 배치
	// ------------------------------------------
	fmt.Println("\n--- 3. 새 패널 자동 배치 ---")

	newPanel := &Panel{ID: 7, Title: "New Panel", Type: "stat", GridPos: GridPos{W: 8, H: 3}}
	fmt.Printf("\n새 패널 추가: %s (W=%d, H=%d) → 빈 공간 탐색\n",
		newPanel.Title, newPanel.GridPos.W, newPanel.GridPos.H)

	engine.PlacePanel(dash.Panels, newPanel)
	dash.Panels = append(dash.Panels, newPanel)

	fmt.Printf("배치 결과: X=%d, Y=%d\n", newPanel.GridPos.X, newPanel.GridPos.Y)
	RenderGrid(dash)

	// ------------------------------------------
	// 4. 행(Row) 패널
	// ------------------------------------------
	fmt.Println("\n--- 4. 행(Row) 패널 ---")

	rowDash := &Dashboard{
		Title: "Row Dashboard",
		Panels: []*Panel{
			{ID: 1, Title: "== Overview ==", Type: "row", IsRow: true, GridPos: GridPos{X: 0, Y: 0, W: 24, H: 1}},
			{ID: 2, Title: "CPU", Type: "graph", GridPos: GridPos{X: 0, Y: 1, W: 12, H: 4}},
			{ID: 3, Title: "Memory", Type: "graph", GridPos: GridPos{X: 12, Y: 1, W: 12, H: 4}},
			{ID: 4, Title: "== Details ==", Type: "row", IsRow: true, GridPos: GridPos{X: 0, Y: 5, W: 24, H: 1}},
			{ID: 5, Title: "Disk", Type: "stat", GridPos: GridPos{X: 0, Y: 6, W: 12, H: 3}},
			{ID: 6, Title: "Network", Type: "graph", GridPos: GridPos{X: 12, Y: 6, W: 12, H: 3}},
		},
	}

	fmt.Println("\n[펼침 상태]")
	RenderGrid(rowDash)

	// Overview 행 접기
	fmt.Println("\n[Overview 행 접기]")
	row := rowDash.Panels[0] // == Overview ==
	rowDash.Panels = engine.CollapseRow(row, rowDash.Panels)
	fmt.Printf("접힌 행에 소속된 패널: %d개\n", len(row.Panels))
	for _, p := range row.Panels {
		fmt.Printf("  - %s\n", p.Title)
	}
	RenderGrid(rowDash)

	// Overview 행 펼치기
	fmt.Println("\n[Overview 행 펼치기]")
	expanded := engine.ExpandRow(row)
	fmt.Printf("펼쳐진 패널: %d개\n", len(expanded))
	// 원래 위치에 삽입
	newPanels := []*Panel{row}
	newPanels = append(newPanels, expanded...)
	for _, p := range rowDash.Panels[1:] {
		newPanels = append(newPanels, p)
	}
	rowDash.Panels = newPanels
	RenderGrid(rowDash)

	// ------------------------------------------
	// 5. 반복(Repeat) 패널
	// ------------------------------------------
	fmt.Println("\n--- 5. 반복(Repeat) 패널 ---")

	templatePanel := &Panel{
		ID:     100,
		Title:  "Server Stats",
		Type:   "graph",
		Repeat: "server",
		GridPos: GridPos{X: 0, Y: 0, W: 8, H: 4},
	}

	servers := []string{"web-01", "web-02", "web-03", "db-01", "db-02", "cache-01"}
	nextID := 101
	repeated := engine.RepeatPanels(templatePanel, servers, &nextID)

	repeatDash := &Dashboard{
		Title:  "Repeated Panels",
		Panels: repeated,
	}

	fmt.Printf("\n템플릿: %s (repeat=$server, W=%d)\n", templatePanel.Title, templatePanel.GridPos.W)
	fmt.Printf("변수 값: %v\n", servers)
	fmt.Printf("생성된 패널: %d개\n", len(repeated))
	RenderGrid(repeatDash)

	// ------------------------------------------
	// 6. 반응형 레이아웃 (모바일 vs 데스크탑)
	// ------------------------------------------
	fmt.Println("\n--- 6. 픽셀 크기 계산 ---")
	fmt.Println()

	fmt.Println("그리드 상수:")
	fmt.Printf("  GRID_COLUMN_COUNT = %d\n", GridColumnCount)
	fmt.Printf("  GRID_CELL_HEIGHT  = %dpx\n", GridCellHeight)
	fmt.Printf("  GRID_CELL_VMARGIN = %dpx\n", GridCellVMargin)
	fmt.Printf("  GRID_CELL_WIDTH   = %dpx (1440px / 24)\n", GridCellWidth)

	fmt.Println("\n패널별 픽셀 크기:")
	fmt.Println(strings.Repeat("-", 75))
	fmt.Printf("%-16s %-20s %-20s\n", "패널", "GridPos", "Pixel")
	fmt.Println(strings.Repeat("-", 75))

	samplePanels := []*Panel{
		{Title: "Half Width", GridPos: GridPos{X: 0, Y: 0, W: 12, H: 4}},
		{Title: "Full Width", GridPos: GridPos{X: 0, Y: 4, W: 24, H: 6}},
		{Title: "Quarter", GridPos: GridPos{X: 0, Y: 10, W: 6, H: 3}},
		{Title: "Third", GridPos: GridPos{X: 0, Y: 13, W: 8, H: 4}},
	}

	for _, p := range samplePanels {
		px := p.GridPos.ToPixels()
		fmt.Printf("%-16s (%2d,%2d,%2d,%2d)       (%4dpx, %4dpx, %4dpx x %3dpx)\n",
			p.Title,
			p.GridPos.X, p.GridPos.Y, p.GridPos.W, p.GridPos.H,
			px.X, px.Y, px.Width, px.Height)
	}
	fmt.Println(strings.Repeat("-", 75))

	// ------------------------------------------
	// 요약
	// ------------------------------------------
	fmt.Println("\n--- 시뮬레이션 요약 ---")
	fmt.Println()
	fmt.Println("그리드 레이아웃 구성요소:")
	fmt.Println("  1. GridPos: X, Y, W, H — 24컬럼 기준 패널 위치/크기")
	fmt.Println("  2. 충돌 감지: AABB 알고리즘으로 패널 간 겹침 검출")
	fmt.Println("  3. 자동 배치: 충돌 시 아래로 밀어내는 정렬 알고리즘")
	fmt.Println("  4. 행 패널: 접기/펼치기로 패널 그룹 관리")
	fmt.Println("  5. 반복 패널: 템플릿 변수 기반 동적 패널 생성")
	fmt.Println("  6. 픽셀 변환: 그리드 좌표 → 실제 렌더링 좌표")
}
