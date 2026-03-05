package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// =============================================================================
// Grafana 프로비저닝 시스템 시뮬레이션
//
// Grafana는 pkg/services/provisioning/ 에서 파일 기반 프로비저닝을 구현한다.
// YAML 설정 파일을 읽어 데이터소스, 대시보드, 알림 규칙을 자동으로 생성/수정한다.
// 시작 시 한 번 실행되며, 파일 변경을 감시하여 재적용할 수 있다.
// =============================================================================

// ResourceState는 프로비저닝된 리소스의 상태이다.
type ResourceState string

const (
	StateCreated ResourceState = "created"
	StateUpdated ResourceState = "updated"
	StateDeleted ResourceState = "deleted"
	StateSkipped ResourceState = "skipped"
)

// ProvisionedDataSource는 프로비저닝된 데이터소스이다.
type ProvisionedDataSource struct {
	Name      string
	Type      string
	URL       string
	Access    string
	IsDefault bool
	OrgID     int64
	Version   int
	JSONData  map[string]string
}

// ProvisionedDashboard는 프로비저닝된 대시보드이다.
type ProvisionedDashboard struct {
	UID       string
	Title     string
	Folder    string
	FilePath  string
	Version   int
	Panels    int
}

// ProvisionLog는 프로비저닝 로그 엔트리이다.
type ProvisionLog struct {
	Timestamp    time.Time
	Phase        string // datasources, dashboards, alerting
	ResourceType string
	ResourceName string
	State        ResourceState
	Message      string
}

// =============================================================================
// 설정 파일 파서 (YAML-유사 포맷)
// =============================================================================

// ConfigEntry는 파싱된 설정 항목이다.
type ConfigEntry struct {
	Key   string
	Value string
	Level int // 들여쓰기 레벨 (0=루트, 1=섹션, 2=항목 등)
}

// ParseConfig는 간단한 YAML-유사 포맷을 파싱한다.
// 표준 라이브러리만 사용하므로 yaml 패키지 대신 직접 구현한다.
func ParseConfig(content string) []ConfigEntry {
	var entries []ConfigEntry
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		// 빈 줄, 주석 무시
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// 들여쓰기 레벨 계산
		level := 0
		for _, ch := range line {
			if ch == ' ' {
				level++
			} else {
				break
			}
		}
		level = level / 2 // 2칸 들여쓰기 기준

		// "- " 접두사 제거 (리스트 아이템)
		if strings.HasPrefix(trimmed, "- ") {
			trimmed = trimmed[2:]
		}

		// key: value 파싱
		colonIdx := strings.Index(trimmed, ":")
		if colonIdx < 0 {
			continue
		}

		key := strings.TrimSpace(trimmed[:colonIdx])
		value := strings.TrimSpace(trimmed[colonIdx+1:])

		entries = append(entries, ConfigEntry{
			Key:   key,
			Value: value,
			Level: level,
		})
	}
	return entries
}

// =============================================================================
// 데이터소스 프로비저닝
// Grafana: pkg/services/provisioning/datasources/
// =============================================================================

// DataSourceStore는 데이터소스 저장소이다.
type DataSourceStore struct {
	dataSources map[string]*ProvisionedDataSource
}

func NewDataSourceStore() *DataSourceStore {
	return &DataSourceStore{
		dataSources: make(map[string]*ProvisionedDataSource),
	}
}

func (s *DataSourceStore) Upsert(ds *ProvisionedDataSource) ResourceState {
	existing, ok := s.dataSources[ds.Name]
	if !ok {
		ds.Version = 1
		s.dataSources[ds.Name] = ds
		return StateCreated
	}

	// 변경 감지 (간단한 비교)
	if existing.URL == ds.URL && existing.Type == ds.Type && existing.Access == ds.Access {
		return StateSkipped
	}

	ds.Version = existing.Version + 1
	s.dataSources[ds.Name] = ds
	return StateUpdated
}

func (s *DataSourceStore) Delete(name string) ResourceState {
	if _, ok := s.dataSources[name]; !ok {
		return StateSkipped
	}
	delete(s.dataSources, name)
	return StateDeleted
}

func (s *DataSourceStore) All() []*ProvisionedDataSource {
	var result []*ProvisionedDataSource
	for _, ds := range s.dataSources {
		result = append(result, ds)
	}
	return result
}

// ProvisionDatasources는 설정 파일에서 데이터소스를 프로비저닝한다.
func ProvisionDatasources(configContent string, store *DataSourceStore) []ProvisionLog {
	var logs []ProvisionLog
	entries := ParseConfig(configContent)

	var currentDS *ProvisionedDataSource
	var deleteNames []string

	for _, entry := range entries {
		switch entry.Key {
		case "name":
			if currentDS != nil {
				state := store.Upsert(currentDS)
				logs = append(logs, ProvisionLog{
					Timestamp:    time.Now(),
					Phase:        "datasources",
					ResourceType: "datasource",
					ResourceName: currentDS.Name,
					State:        state,
					Message:      fmt.Sprintf("type=%s url=%s version=%d", currentDS.Type, currentDS.URL, currentDS.Version),
				})
			}
			currentDS = &ProvisionedDataSource{
				Name:     entry.Value,
				OrgID:    1,
				Access:   "proxy",
				JSONData: make(map[string]string),
			}
		case "type":
			if currentDS != nil {
				currentDS.Type = entry.Value
			}
		case "url":
			if currentDS != nil {
				currentDS.URL = entry.Value
			}
		case "access":
			if currentDS != nil {
				currentDS.Access = entry.Value
			}
		case "isDefault":
			if currentDS != nil {
				currentDS.IsDefault = entry.Value == "true"
			}
		case "deleteDatasource":
			// 삭제할 데이터소스 이름 수집
		case "deleteName":
			deleteNames = append(deleteNames, entry.Value)
		}

		// jsonData 하위 키
		if currentDS != nil && entry.Level >= 3 {
			currentDS.JSONData[entry.Key] = entry.Value
		}
	}

	// 마지막 데이터소스 처리
	if currentDS != nil {
		state := store.Upsert(currentDS)
		logs = append(logs, ProvisionLog{
			Timestamp:    time.Now(),
			Phase:        "datasources",
			ResourceType: "datasource",
			ResourceName: currentDS.Name,
			State:        state,
			Message:      fmt.Sprintf("type=%s url=%s version=%d", currentDS.Type, currentDS.URL, currentDS.Version),
		})
	}

	// 삭제 처리
	for _, name := range deleteNames {
		state := store.Delete(name)
		logs = append(logs, ProvisionLog{
			Timestamp:    time.Now(),
			Phase:        "datasources",
			ResourceType: "datasource",
			ResourceName: name,
			State:        state,
			Message:      "deleted by provisioning config",
		})
	}

	return logs
}

// =============================================================================
// 대시보드 프로비저닝
// Grafana: pkg/services/provisioning/dashboards/
// =============================================================================

// DashboardStore는 대시보드 저장소이다.
type DashboardStore struct {
	dashboards map[string]*ProvisionedDashboard
}

func NewDashboardStore() *DashboardStore {
	return &DashboardStore{
		dashboards: make(map[string]*ProvisionedDashboard),
	}
}

func (s *DashboardStore) Upsert(db *ProvisionedDashboard) ResourceState {
	existing, ok := s.dashboards[db.UID]
	if !ok {
		db.Version = 1
		s.dashboards[db.UID] = db
		return StateCreated
	}

	if existing.Title == db.Title && existing.Folder == db.Folder && existing.Panels == db.Panels {
		return StateSkipped
	}

	db.Version = existing.Version + 1
	s.dashboards[db.UID] = db
	return StateUpdated
}

func (s *DashboardStore) All() []*ProvisionedDashboard {
	var result []*ProvisionedDashboard
	for _, db := range s.dashboards {
		result = append(result, db)
	}
	return result
}

// ProvisionDashboardsFromDir는 디렉토리에서 대시보드 파일을 읽어 프로비저닝한다.
func ProvisionDashboardsFromDir(dir, folder string, store *DashboardStore) []ProvisionLog {
	var logs []ProvisionLog

	entries, err := os.ReadDir(dir)
	if err != nil {
		logs = append(logs, ProvisionLog{
			Timestamp:    time.Now(),
			Phase:        "dashboards",
			ResourceType: "directory",
			ResourceName: dir,
			State:        StateSkipped,
			Message:      fmt.Sprintf("read directory error: %v", err),
		})
		return logs
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		// 간단한 JSON 파싱 (표준 라이브러리의 json 대신 문자열 파싱)
		contentStr := string(content)
		uid := extractJSONValue(contentStr, "uid")
		title := extractJSONValue(contentStr, "title")
		panels := strings.Count(contentStr, "\"type\":")

		if uid == "" || title == "" {
			continue
		}

		db := &ProvisionedDashboard{
			UID:      uid,
			Title:    title,
			Folder:   folder,
			FilePath: filePath,
			Panels:   panels,
		}

		state := store.Upsert(db)
		logs = append(logs, ProvisionLog{
			Timestamp:    time.Now(),
			Phase:        "dashboards",
			ResourceType: "dashboard",
			ResourceName: title,
			State:        state,
			Message:      fmt.Sprintf("uid=%s folder=%s panels=%d file=%s", uid, folder, panels, entry.Name()),
		})
	}

	return logs
}

func extractJSONValue(json, key string) string {
	search := fmt.Sprintf(`"%s":`, key)
	idx := strings.Index(json, search)
	if idx < 0 {
		search = fmt.Sprintf(`"%s" :`, key)
		idx = strings.Index(json, search)
		if idx < 0 {
			return ""
		}
	}

	rest := strings.TrimSpace(json[idx+len(search):])
	if len(rest) == 0 {
		return ""
	}

	if rest[0] == '"' {
		endIdx := strings.Index(rest[1:], "\"")
		if endIdx < 0 {
			return ""
		}
		return rest[1 : endIdx+1]
	}

	endIdx := strings.IndexAny(rest, ",}")
	if endIdx < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:endIdx])
}

// =============================================================================
// 파일 감시 시뮬레이션
// =============================================================================

// FileWatcher는 디렉토리의 변경을 폴링으로 감지한다.
type FileWatcher struct {
	dir      string
	interval time.Duration
	lastSeen map[string]time.Time
}

func NewFileWatcher(dir string, interval time.Duration) *FileWatcher {
	return &FileWatcher{
		dir:      dir,
		interval: interval,
		lastSeen: make(map[string]time.Time),
	}
}

type FileChange struct {
	Path   string
	Action string // created, modified, deleted
}

func (w *FileWatcher) Scan() []FileChange {
	var changes []FileChange
	currentFiles := make(map[string]time.Time)

	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return changes
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}

		filePath := filepath.Join(w.dir, entry.Name())
		modTime := info.ModTime()
		currentFiles[filePath] = modTime

		lastMod, seen := w.lastSeen[filePath]
		if !seen {
			changes = append(changes, FileChange{Path: filePath, Action: "created"})
		} else if modTime.After(lastMod) {
			changes = append(changes, FileChange{Path: filePath, Action: "modified"})
		}
	}

	// 삭제된 파일 감지
	for path := range w.lastSeen {
		if _, ok := currentFiles[path]; !ok {
			changes = append(changes, FileChange{Path: path, Action: "deleted"})
		}
	}

	w.lastSeen = currentFiles
	return changes
}

// =============================================================================
// ProvisioningService - 전체 오케스트레이션
// Grafana: pkg/services/provisioning/provisioning.go
// =============================================================================

type ProvisioningService struct {
	dsStore   *DataSourceStore
	dbStore   *DashboardStore
	allLogs   []ProvisionLog
}

func NewProvisioningService() *ProvisioningService {
	return &ProvisioningService{
		dsStore: NewDataSourceStore(),
		dbStore: NewDashboardStore(),
	}
}

func (s *ProvisioningService) Run(dsConfigContent string, dashboardDir string) {
	fmt.Println("━━━ 프로비저닝 시작 ━━━")
	fmt.Println()

	// 1단계: 데이터소스 프로비저닝
	fmt.Println("  [1/4] 데이터소스 프로비저닝...")
	dsLogs := ProvisionDatasources(dsConfigContent, s.dsStore)
	s.allLogs = append(s.allLogs, dsLogs...)
	for _, log := range dsLogs {
		fmt.Printf("    %-10s %-25s %s\n", log.State, log.ResourceName, log.Message)
	}

	// 2단계: 플러그인 프로비저닝 (시뮬레이션)
	fmt.Println("\n  [2/4] 플러그인 프로비저닝... (건너뜀 - 시뮬레이션)")

	// 3단계: 알림 프로비저닝 (시뮬레이션)
	fmt.Println("\n  [3/4] 알림 프로비저닝... (건너뜀 - 시뮬레이션)")

	// 4단계: 대시보드 프로비저닝
	fmt.Println("\n  [4/4] 대시보드 프로비저닝...")
	dbLogs := ProvisionDashboardsFromDir(dashboardDir, "Infrastructure", s.dbStore)
	s.allLogs = append(s.allLogs, dbLogs...)
	for _, log := range dbLogs {
		fmt.Printf("    %-10s %-25s %s\n", log.State, log.ResourceName, log.Message)
	}

	fmt.Println()
	fmt.Println("━━━ 프로비저닝 완료 ━━━")
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== Grafana 프로비저닝 시스템 시뮬레이션 ===")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "grafana-provision-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	dashboardDir := filepath.Join(tmpDir, "dashboards")
	os.MkdirAll(dashboardDir, 0755)

	// ─── 데이터소스 설정 파일 작성 ───
	dsConfig := `# Grafana 데이터소스 프로비저닝 설정
# conf/provisioning/datasources/default.yaml

apiVersion: 1

# 삭제할 데이터소스
deleteDatasources:
  deleteName: Old-Prometheus

# 프로비저닝할 데이터소스
datasources:
  name: Prometheus
  type: prometheus
  url: http://prometheus:9090
  access: proxy
  isDefault: true
  jsonData:
    timeInterval: 15s
    httpMethod: POST

  name: Loki
  type: loki
  url: http://loki:3100
  access: proxy
  isDefault: false
  jsonData:
    maxLines: 1000

  name: Tempo
  type: tempo
  url: http://tempo:3200
  access: proxy
  isDefault: false
  jsonData:
    tracesToLogs: true
    lokiSearch: true

  name: InfluxDB
  type: influxdb
  url: http://influxdb:8086
  access: proxy
  isDefault: false
  jsonData:
    dbName: telegraf
    httpMode: POST`

	// ─── 대시보드 JSON 파일 작성 ───
	dashboard1 := `{
  "uid": "system-overview",
  "title": "System Overview",
  "tags": ["infrastructure", "system"],
  "panels": [
    {"type": "timeseries", "title": "CPU Usage"},
    {"type": "timeseries", "title": "Memory Usage"},
    {"type": "stat", "title": "Uptime"},
    {"type": "table", "title": "Top Processes"}
  ]
}`
	os.WriteFile(filepath.Join(dashboardDir, "system-overview.json"), []byte(dashboard1), 0644)

	dashboard2 := `{
  "uid": "network-monitoring",
  "title": "Network Monitoring",
  "tags": ["infrastructure", "network"],
  "panels": [
    {"type": "timeseries", "title": "Network In/Out"},
    {"type": "gauge", "title": "Bandwidth Usage"},
    {"type": "table", "title": "Active Connections"}
  ]
}`
	os.WriteFile(filepath.Join(dashboardDir, "network-monitoring.json"), []byte(dashboard2), 0644)

	dashboard3 := `{
  "uid": "app-metrics",
  "title": "Application Metrics",
  "tags": ["application", "metrics"],
  "panels": [
    {"type": "timeseries", "title": "Request Rate"},
    {"type": "timeseries", "title": "Error Rate"},
    {"type": "heatmap", "title": "Response Time Distribution"},
    {"type": "stat", "title": "Active Users"},
    {"type": "table", "title": "Slow Endpoints"}
  ]
}`
	os.WriteFile(filepath.Join(dashboardDir, "app-metrics.json"), []byte(dashboard3), 0644)

	// ─── 1차 프로비저닝 (초기 설정) ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("1차 프로비저닝: 초기 설정 적용")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	service := NewProvisioningService()
	service.Run(dsConfig, dashboardDir)

	// ─── 현재 상태 출력 ───
	fmt.Println()
	printCurrentState(service)

	// ─── 2차 프로비저닝 (변경 없음 - 멱등성 테스트) ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("2차 프로비저닝: 동일 설정 재적용 (멱등성 테스트)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	service.allLogs = nil // 로그 초기화
	service.Run(dsConfig, dashboardDir)

	// ─── 3차 프로비저닝 (설정 변경) ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("3차 프로비저닝: 설정 변경 감지")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 데이터소스 URL 변경
	updatedDsConfig := strings.Replace(dsConfig,
		"url: http://prometheus:9090",
		"url: http://prometheus-ha:9090",
		1)
	// InfluxDB 제거, Mimir 추가
	updatedDsConfig = strings.Replace(updatedDsConfig,
		`  name: InfluxDB
  type: influxdb
  url: http://influxdb:8086
  access: proxy
  isDefault: false
  jsonData:
    dbName: telegraf
    httpMode: POST`,
		`  name: Mimir
  type: prometheus
  url: http://mimir:9009
  access: proxy
  isDefault: false
  jsonData:
    httpMethod: POST`,
		1)

	// 대시보드 변경: app-metrics에 패널 추가
	dashboard3Updated := `{
  "uid": "app-metrics",
  "title": "Application Metrics v2",
  "tags": ["application", "metrics"],
  "panels": [
    {"type": "timeseries", "title": "Request Rate"},
    {"type": "timeseries", "title": "Error Rate"},
    {"type": "heatmap", "title": "Response Time Distribution"},
    {"type": "stat", "title": "Active Users"},
    {"type": "table", "title": "Slow Endpoints"},
    {"type": "logs", "title": "Recent Errors"},
    {"type": "traces", "title": "Trace Explorer"}
  ]
}`
	os.WriteFile(filepath.Join(dashboardDir, "app-metrics.json"), []byte(dashboard3Updated), 0644)

	service.allLogs = nil
	service.Run(updatedDsConfig, dashboardDir)

	// ─── 최종 상태 출력 ───
	fmt.Println()
	printCurrentState(service)

	// ─── 파일 감시 시뮬레이션 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("파일 감시 시뮬레이션")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	watcher := NewFileWatcher(dashboardDir, 1*time.Second)

	// 초기 스캔
	fmt.Println("  [스캔 1] 초기 스캔")
	changes := watcher.Scan()
	for _, ch := range changes {
		fmt.Printf("    %-10s %s\n", ch.Action, filepath.Base(ch.Path))
	}

	// 파일 추가
	newDashboard := `{
  "uid": "kubernetes-cluster",
  "title": "Kubernetes Cluster",
  "panels": [
    {"type": "stat", "title": "Node Count"},
    {"type": "table", "title": "Pod Status"}
  ]
}`
	os.WriteFile(filepath.Join(dashboardDir, "k8s-cluster.json"), []byte(newDashboard), 0644)

	// 파일 수정 (modtime 변경을 위해 약간 대기)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(dashboardDir, "system-overview.json"), []byte(dashboard1+"\n"), 0644)

	// 파일 삭제
	os.Remove(filepath.Join(dashboardDir, "network-monitoring.json"))

	fmt.Println("\n  [스캔 2] 변경 후 스캔")
	changes = watcher.Scan()
	if len(changes) == 0 {
		fmt.Println("    변경 없음")
	}
	for _, ch := range changes {
		fmt.Printf("    %-10s %s\n", ch.Action, filepath.Base(ch.Path))
	}

	// ─── 프로비저닝 순서 다이어그램 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("프로비저닝 실행 순서")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  Grafana 시작")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  ┌─────────────────────────┐")
	fmt.Println("  │ 1. 데이터소스 프로비저닝  │  ← conf/provisioning/datasources/*.yaml")
	fmt.Println("  │    Prometheus, Loki, ... │")
	fmt.Println("  └────────────┬────────────┘")
	fmt.Println("               │")
	fmt.Println("               ▼")
	fmt.Println("  ┌─────────────────────────┐")
	fmt.Println("  │ 2. 플러그인 프로비저닝    │  ← conf/provisioning/plugins/*.yaml")
	fmt.Println("  │    패널, 앱 플러그인     │")
	fmt.Println("  └────────────┬────────────┘")
	fmt.Println("               │")
	fmt.Println("               ▼")
	fmt.Println("  ┌─────────────────────────┐")
	fmt.Println("  │ 3. 알림 프로비저닝       │  ← conf/provisioning/alerting/*.yaml")
	fmt.Println("  │    알림 규칙, 연락처     │")
	fmt.Println("  └────────────┬────────────┘")
	fmt.Println("               │")
	fmt.Println("               ▼")
	fmt.Println("  ┌─────────────────────────┐")
	fmt.Println("  │ 4. 대시보드 프로비저닝    │  ← conf/provisioning/dashboards/*.yaml")
	fmt.Println("  │    JSON 파일 로드        │     + 지정된 디렉토리의 *.json")
	fmt.Println("  └────────────┬────────────┘")
	fmt.Println("               │")
	fmt.Println("               ▼")
	fmt.Println("  ┌─────────────────────────┐")
	fmt.Println("  │ 5. 파일 감시 시작        │  ← 변경 감지 시 자동 재프로비저닝")
	fmt.Println("  └─────────────────────────┘")
	fmt.Println()
	fmt.Println("=== 프로비저닝 시뮬레이션 완료 ===")
}

func printCurrentState(service *ProvisioningService) {
	fmt.Println("━━━ 현재 프로비저닝 상태 ━━━")

	fmt.Println("\n  [데이터소스]")
	fmt.Printf("  %-15s %-12s %-30s %-8s %-10s %s\n",
		"Name", "Type", "URL", "Access", "Default", "Version")
	fmt.Println("  " + strings.Repeat("-", 90))
	for _, ds := range service.dsStore.All() {
		def := ""
		if ds.IsDefault {
			def = "O"
		}
		fmt.Printf("  %-15s %-12s %-30s %-8s %-10s v%d\n",
			ds.Name, ds.Type, ds.URL, ds.Access, def, ds.Version)
	}

	fmt.Println("\n  [대시보드]")
	fmt.Printf("  %-25s %-20s %-15s %-8s %s\n",
		"Title", "UID", "Folder", "Panels", "Version")
	fmt.Println("  " + strings.Repeat("-", 80))
	for _, db := range service.dbStore.All() {
		fmt.Printf("  %-25s %-20s %-15s %-8d v%d\n",
			db.Title, db.UID, db.Folder, db.Panels, db.Version)
	}
}
