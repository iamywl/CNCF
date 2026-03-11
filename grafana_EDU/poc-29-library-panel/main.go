package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Grafana Library Panel 시스템 시뮬레이션
// =============================================================================
//
// Library Panel은 여러 대시보드에서 공유 가능한 재사용 패널이다.
// 하나의 Library Panel을 수정하면 이를 사용하는 모든 대시보드에 반영된다.
//
// 핵심 개념:
//   - Library Element: 공유 패널의 정의 (모델 + 메타데이터)
//   - Connected Dashboards: 이 패널을 사용하는 대시보드 목록
//   - Version History: 패널 변경 이력
//   - Unlink: 공유 연결을 끊고 독립 패널로 전환
//
// 실제 코드 참조:
//   - pkg/services/libraryelements/: Library Element 서비스
//   - pkg/services/librarypanels/: Library Panel API
// =============================================================================

// --- 패널 모델 ---

type PanelType string

const (
	PanelGraph    PanelType = "graph"
	PanelStat     PanelType = "stat"
	PanelTable    PanelType = "table"
	PanelTimeSeries PanelType = "timeseries"
	PanelGauge    PanelType = "gauge"
)

type PanelModel struct {
	Type       PanelType         `json:"type"`
	Title      string            `json:"title"`
	DataSource string            `json:"datasource"`
	Targets    []QueryTarget     `json:"targets"`
	Options    map[string]interface{} `json:"options"`
}

type QueryTarget struct {
	RefID string `json:"refId"`
	Expr  string `json:"expr"`
}

// --- Library Element ---

type LibraryElement struct {
	UID       string       `json:"uid"`
	Name      string       `json:"name"`
	Kind      int          `json:"kind"` // 1 = panel
	Model     PanelModel   `json:"model"`
	FolderUID string       `json:"folderUid"`
	Version   int          `json:"version"`
	CreatedBy string       `json:"createdBy"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Versions  []ElementVersion `json:"-"`
}

type ElementVersion struct {
	Version   int       `json:"version"`
	Model     PanelModel `json:"model"`
	UpdatedBy string    `json:"updatedBy"`
	UpdatedAt time.Time `json:"updatedAt"`
	Message   string    `json:"message"`
}

// --- Dashboard 연결 ---

type DashboardRef struct {
	UID   string `json:"uid"`
	Title string `json:"title"`
}

type PanelInstance struct {
	DashboardUID string `json:"dashboardUid"`
	PanelID      int    `json:"panelId"`
	LibraryUID   string `json:"libraryUid"`
	IsLinked     bool   `json:"isLinked"`
}

// --- Library Panel Service ---

type LibraryPanelService struct {
	mu        sync.RWMutex
	elements  map[string]*LibraryElement   // UID -> element
	connected map[string][]PanelInstance    // libraryUID -> instances
	dashboards map[string]*DashboardRef     // dashboardUID -> ref
}

func NewLibraryPanelService() *LibraryPanelService {
	return &LibraryPanelService{
		elements:   make(map[string]*LibraryElement),
		connected:  make(map[string][]PanelInstance),
		dashboards: make(map[string]*DashboardRef),
	}
}

func (svc *LibraryPanelService) RegisterDashboard(uid, title string) {
	svc.dashboards[uid] = &DashboardRef{UID: uid, Title: title}
}

// CreateElement는 새 Library Element를 생성한다.
func (svc *LibraryPanelService) CreateElement(uid, name string, model PanelModel, folderUID, createdBy string) *LibraryElement {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	elem := &LibraryElement{
		UID:       uid,
		Name:      name,
		Kind:      1, // panel
		Model:     model,
		FolderUID: folderUID,
		Version:   1,
		CreatedBy: createdBy,
		UpdatedAt: time.Now(),
		Versions: []ElementVersion{
			{Version: 1, Model: model, UpdatedBy: createdBy, UpdatedAt: time.Now(), Message: "Initial creation"},
		},
	}
	svc.elements[uid] = elem
	return elem
}

// UpdateElement는 Library Element를 업데이트한다.
func (svc *LibraryPanelService) UpdateElement(uid string, model PanelModel, updatedBy, message string) error {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	elem, ok := svc.elements[uid]
	if !ok {
		return fmt.Errorf("library element %s not found", uid)
	}

	elem.Version++
	elem.Model = model
	elem.UpdatedAt = time.Now()

	elem.Versions = append(elem.Versions, ElementVersion{
		Version:   elem.Version,
		Model:     model,
		UpdatedBy: updatedBy,
		UpdatedAt: time.Now(),
		Message:   message,
	})

	// 연결된 모든 대시보드에 변경 전파
	instances := svc.connected[uid]
	if len(instances) > 0 {
		fmt.Printf("    [PROPAGATE] %d개 대시보드에 변경 전파:\n", len(instances))
		for _, inst := range instances {
			if inst.IsLinked {
				dash := svc.dashboards[inst.DashboardUID]
				if dash != nil {
					fmt.Printf("      -> %s (panel #%d)\n", dash.Title, inst.PanelID)
				}
			}
		}
	}

	return nil
}

// ConnectToDashboard는 대시보드에 Library Panel을 연결한다.
func (svc *LibraryPanelService) ConnectToDashboard(libraryUID, dashboardUID string, panelID int) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	instance := PanelInstance{
		DashboardUID: dashboardUID,
		PanelID:      panelID,
		LibraryUID:   libraryUID,
		IsLinked:     true,
	}
	svc.connected[libraryUID] = append(svc.connected[libraryUID], instance)
}

// UnlinkFromDashboard는 대시보드에서 Library Panel 연결을 해제한다.
func (svc *LibraryPanelService) UnlinkFromDashboard(libraryUID, dashboardUID string, panelID int) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	instances := svc.connected[libraryUID]
	for i, inst := range instances {
		if inst.DashboardUID == dashboardUID && inst.PanelID == panelID {
			instances[i].IsLinked = false
			svc.connected[libraryUID] = instances
			fmt.Printf("    [UNLINK] Dashboard %s, Panel #%d unlinked from %s\n",
				dashboardUID, panelID, libraryUID)
			return
		}
	}
}

// GetConnectedDashboards는 연결된 대시보드 목록을 반환한다.
func (svc *LibraryPanelService) GetConnectedDashboards(libraryUID string) []DashboardRef {
	svc.mu.RLock()
	defer svc.mu.RUnlock()

	var result []DashboardRef
	for _, inst := range svc.connected[libraryUID] {
		if inst.IsLinked {
			if dash := svc.dashboards[inst.DashboardUID]; dash != nil {
				result = append(result, *dash)
			}
		}
	}
	return result
}

// GetVersionHistory는 변경 이력을 반환한다.
func (svc *LibraryPanelService) GetVersionHistory(uid string) []ElementVersion {
	svc.mu.RLock()
	defer svc.mu.RUnlock()

	elem, ok := svc.elements[uid]
	if !ok {
		return nil
	}
	return elem.Versions
}

// DeleteElement는 Library Element를 삭제한다 (연결된 대시보드가 없어야 함).
func (svc *LibraryPanelService) DeleteElement(uid string) error {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	connectedCount := 0
	for _, inst := range svc.connected[uid] {
		if inst.IsLinked {
			connectedCount++
		}
	}
	if connectedCount > 0 {
		return fmt.Errorf("cannot delete: %d dashboard(s) still connected", connectedCount)
	}

	delete(svc.elements, uid)
	delete(svc.connected, uid)
	return nil
}

func prettyJSON(v interface{}) string {
	data, _ := json.MarshalIndent(v, "    ", "  ")
	return string(data)
}

func main() {
	fmt.Println("=== Grafana Library Panel 시뮬레이션 ===")
	fmt.Println()

	svc := NewLibraryPanelService()

	// --- 대시보드 등록 ---
	svc.RegisterDashboard("dash-001", "Production Overview")
	svc.RegisterDashboard("dash-002", "API Performance")
	svc.RegisterDashboard("dash-003", "Infrastructure Monitoring")

	// --- Library Panel 생성 ---
	fmt.Println("[1] Library Panel 생성")
	fmt.Println(strings.Repeat("-", 60))

	cpuPanel := svc.CreateElement("lib-cpu-001", "CPU Usage Panel", PanelModel{
		Type:       PanelTimeSeries,
		Title:      "CPU Usage",
		DataSource: "Prometheus",
		Targets: []QueryTarget{
			{RefID: "A", Expr: "rate(node_cpu_seconds_total{mode!=\"idle\"}[5m])"},
		},
		Options: map[string]interface{}{"legend": true, "tooltip": "all"},
	}, "folder-infra", "admin")
	fmt.Printf("  Created: %s (version: %d)\n", cpuPanel.Name, cpuPanel.Version)

	memPanel := svc.CreateElement("lib-mem-001", "Memory Usage Panel", PanelModel{
		Type:       PanelGauge,
		Title:      "Memory Usage",
		DataSource: "Prometheus",
		Targets: []QueryTarget{
			{RefID: "A", Expr: "node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes"},
		},
		Options: map[string]interface{}{"thresholds": []int{50, 80}},
	}, "folder-infra", "admin")
	fmt.Printf("  Created: %s (version: %d)\n", memPanel.Name, memPanel.Version)

	errorRatePanel := svc.CreateElement("lib-err-001", "Error Rate Panel", PanelModel{
		Type:       PanelStat,
		Title:      "Error Rate",
		DataSource: "Prometheus",
		Targets: []QueryTarget{
			{RefID: "A", Expr: "rate(http_requests_total{status=~\"5..\"}[5m])"},
		},
		Options: map[string]interface{}{"colorMode": "background"},
	}, "folder-api", "dev-lead")
	fmt.Printf("  Created: %s (version: %d)\n", errorRatePanel.Name, errorRatePanel.Version)
	fmt.Println()

	// --- 대시보드에 연결 ---
	fmt.Println("[2] 대시보드에 Library Panel 연결")
	fmt.Println(strings.Repeat("-", 60))

	connections := []struct {
		libUID, dashUID string
		panelID         int
	}{
		{"lib-cpu-001", "dash-001", 1},
		{"lib-cpu-001", "dash-003", 1},
		{"lib-mem-001", "dash-001", 2},
		{"lib-mem-001", "dash-003", 2},
		{"lib-err-001", "dash-001", 3},
		{"lib-err-001", "dash-002", 1},
	}

	for _, c := range connections {
		svc.ConnectToDashboard(c.libUID, c.dashUID, c.panelID)
		fmt.Printf("  Connected: %s -> dashboard %s (panel #%d)\n", c.libUID, c.dashUID, c.panelID)
	}
	fmt.Println()

	// --- 연결 현황 ---
	fmt.Println("[3] Library Panel 연결 현황")
	fmt.Println(strings.Repeat("-", 60))

	for _, uid := range []string{"lib-cpu-001", "lib-mem-001", "lib-err-001"} {
		elem := svc.elements[uid]
		dashes := svc.GetConnectedDashboards(uid)
		fmt.Printf("  %s (%d개 대시보드):\n", elem.Name, len(dashes))
		for _, d := range dashes {
			fmt.Printf("    - %s (%s)\n", d.Title, d.UID)
		}
	}
	fmt.Println()

	// --- 패널 업데이트 (변경 전파) ---
	fmt.Println("[4] Library Panel 업데이트 (변경 전파)")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println("  >> CPU Panel 쿼리 변경:")
	svc.UpdateElement("lib-cpu-001", PanelModel{
		Type:       PanelTimeSeries,
		Title:      "CPU Usage (Updated)",
		DataSource: "Prometheus",
		Targets: []QueryTarget{
			{RefID: "A", Expr: "100 - (avg by(instance)(rate(node_cpu_seconds_total{mode=\"idle\"}[5m])) * 100)"},
		},
		Options: map[string]interface{}{"legend": true, "tooltip": "all", "unit": "percent"},
	}, "admin", "CPU 쿼리를 퍼센트로 변경")
	fmt.Println()

	fmt.Println("  >> Error Rate Panel 임계값 변경:")
	svc.UpdateElement("lib-err-001", PanelModel{
		Type:       PanelStat,
		Title:      "Error Rate",
		DataSource: "Prometheus",
		Targets: []QueryTarget{
			{RefID: "A", Expr: "rate(http_requests_total{status=~\"5..\"}[5m]) / rate(http_requests_total[5m]) * 100"},
		},
		Options: map[string]interface{}{"colorMode": "background", "thresholds": []float64{1.0, 5.0}},
	}, "dev-lead", "에러율을 비율(%)로 변경")
	fmt.Println()

	// --- 버전 이력 ---
	fmt.Println("[5] 버전 이력")
	fmt.Println(strings.Repeat("-", 60))

	for _, uid := range []string{"lib-cpu-001", "lib-err-001"} {
		elem := svc.elements[uid]
		versions := svc.GetVersionHistory(uid)
		fmt.Printf("  %s (현재 v%d):\n", elem.Name, elem.Version)
		for _, v := range versions {
			fmt.Printf("    v%d - %s (by %s, %s)\n",
				v.Version, v.Message, v.UpdatedBy, v.UpdatedAt.Format("15:04:05"))
		}
	}
	fmt.Println()

	// --- Unlink ---
	fmt.Println("[6] Library Panel Unlink")
	fmt.Println(strings.Repeat("-", 60))
	svc.UnlinkFromDashboard("lib-cpu-001", "dash-003", 1)
	fmt.Printf("  dash-003에서 unlink 후 연결 수: %d\n", len(svc.GetConnectedDashboards("lib-cpu-001")))
	fmt.Println()

	// --- 삭제 시도 ---
	fmt.Println("[7] Library Panel 삭제")
	fmt.Println(strings.Repeat("-", 60))
	err := svc.DeleteElement("lib-cpu-001")
	if err != nil {
		fmt.Printf("  삭제 실패: %v\n", err)
	}

	// 모든 연결 해제 후 삭제
	svc.UnlinkFromDashboard("lib-cpu-001", "dash-001", 1)
	err = svc.DeleteElement("lib-cpu-001")
	if err != nil {
		fmt.Printf("  삭제 실패: %v\n", err)
	} else {
		fmt.Printf("  lib-cpu-001 삭제 성공\n")
	}
	fmt.Println()

	// --- 현재 Library Panel 모델 (JSON) ---
	fmt.Println("[8] Library Panel 모델 (JSON)")
	fmt.Println(strings.Repeat("-", 60))
	if elem, ok := svc.elements["lib-err-001"]; ok {
		fmt.Printf("    %s\n", prettyJSON(elem))
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
