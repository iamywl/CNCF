// poc-18-web-ui: Alertmanager Web UI 서버 시뮬레이터
//
// 이 PoC는 Alertmanager Web UI의 핵심 아키텍처를 Go 표준 라이브러리만으로 재현한다.
//
// 시뮬레이션 대상:
//   1. 임베딩된 정적 파일 서빙 (Go embed.FS 패턴)
//   2. API v2 엔드포인트 (alerts, silences, status)
//   3. 캐싱 비활성화 미들웨어
//   4. 동시성 제한 (세마포어 기반 GET 요청 제한)
//   5. 타임아웃 핸들러
//   6. CORS 설정
//   7. 설정 리로드 채널 패턴
//   8. 헬스체크 / 레디니스 프로브
//   9. 요청 계측 (메트릭)
//  10. SPA 라우팅 (index.html 폴백)
//
// 실행: go run main.go
// 브라우저에서 확인: http://localhost:19093

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ============================================================
// 1. 데이터 모델 (api/v2/models 참조)
// ============================================================

// Alert는 알림 데이터 모델이다.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt"`
	State       string            `json:"state"`
	Fingerprint string            `json:"fingerprint"`
}

// AlertGroup은 알림 그룹이다.
type AlertGroup struct {
	Labels   map[string]string `json:"labels"`
	Receiver Receiver          `json:"receiver"`
	Alerts   []Alert           `json:"alerts"`
}

// Receiver는 리시버 정보이다.
type Receiver struct {
	Name string `json:"name"`
}

// SilenceMatcher는 사일런스 매처이다.
type SilenceMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// Silence는 사일런스 데이터 모델이다.
type Silence struct {
	ID        string           `json:"id"`
	Status    SilenceStatus    `json:"status"`
	Matchers  []SilenceMatcher `json:"matchers"`
	StartsAt  string           `json:"startsAt"`
	EndsAt    string           `json:"endsAt"`
	UpdatedAt string           `json:"updatedAt"`
	CreatedBy string           `json:"createdBy"`
	Comment   string           `json:"comment"`
}

// SilenceStatus는 사일런스 상태이다.
type SilenceStatus struct {
	State string `json:"state"` // active, expired, pending
}

// PeerStatus는 클러스터 피어 정보이다.
type PeerStatus struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// ClusterStatus는 클러스터 상태이다.
type ClusterStatus struct {
	Status string       `json:"status"`
	Name   string       `json:"name"`
	Peers  []PeerStatus `json:"peers"`
}

// VersionInfo는 빌드 정보이다.
type VersionInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

// AlertmanagerConfig는 설정 정보이다.
type AlertmanagerConfig struct {
	Original string `json:"original"`
}

// AlertmanagerStatus는 전체 상태 응답이다.
// 실제 코드: api/v2/models/alertmanager_status.go
type AlertmanagerStatus struct {
	Cluster     ClusterStatus      `json:"cluster"`
	Config      AlertmanagerConfig `json:"config"`
	Uptime      string             `json:"uptime"`
	VersionInfo VersionInfo        `json:"versionInfo"`
}

// ============================================================
// 2. 모의 데이터 저장소
// ============================================================

// DataStore는 Web UI에 표시할 모의 데이터를 관리한다.
type DataStore struct {
	mu       sync.RWMutex
	alerts   []AlertGroup
	silences []Silence
	config   string
	uptime   time.Time
}

func NewDataStore() *DataStore {
	return &DataStore{
		alerts: []AlertGroup{
			{
				Labels:   map[string]string{"alertname": "HighMemory"},
				Receiver: Receiver{Name: "slack-alerts"},
				Alerts: []Alert{
					{
						Labels:      map[string]string{"alertname": "HighMemory", "severity": "critical", "instance": "web-1"},
						Annotations: map[string]string{"summary": "메모리 사용량 90% 초과", "description": "web-1 인스턴스 메모리 위험"},
						StartsAt:    time.Now().Add(-30 * time.Minute).Format(time.RFC3339),
						EndsAt:      "0001-01-01T00:00:00Z",
						State:       "active",
						Fingerprint: "fp-001",
					},
					{
						Labels:      map[string]string{"alertname": "HighMemory", "severity": "warning", "instance": "web-2"},
						Annotations: map[string]string{"summary": "메모리 사용량 80% 초과"},
						StartsAt:    time.Now().Add(-15 * time.Minute).Format(time.RFC3339),
						EndsAt:      "0001-01-01T00:00:00Z",
						State:       "active",
						Fingerprint: "fp-002",
					},
				},
			},
			{
				Labels:   map[string]string{"alertname": "DiskFull"},
				Receiver: Receiver{Name: "pagerduty"},
				Alerts: []Alert{
					{
						Labels:      map[string]string{"alertname": "DiskFull", "severity": "critical", "instance": "db-1"},
						Annotations: map[string]string{"summary": "디스크 사용량 95%"},
						StartsAt:    time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
						EndsAt:      "0001-01-01T00:00:00Z",
						State:       "active",
						Fingerprint: "fp-003",
					},
				},
			},
		},
		silences: []Silence{
			{
				ID:     "sil-001",
				Status: SilenceStatus{State: "active"},
				Matchers: []SilenceMatcher{
					{Name: "alertname", Value: "CPUHigh", IsRegex: false, IsEqual: true},
				},
				StartsAt:  time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
				EndsAt:    time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				UpdatedAt: time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
				CreatedBy: "admin",
				Comment:   "서버 점검 중",
			},
			{
				ID:     "sil-002",
				Status: SilenceStatus{State: "expired"},
				Matchers: []SilenceMatcher{
					{Name: "severity", Value: "info", IsRegex: false, IsEqual: true},
				},
				StartsAt:  time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
				EndsAt:    time.Now().Add(-12 * time.Hour).Format(time.RFC3339),
				UpdatedAt: time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
				CreatedBy: "oncall",
				Comment:   "info 알림 무시",
			},
		},
		config: `global:
  resolve_timeout: 5m
  smtp_smarthost: 'smtp.example.com:587'

route:
  receiver: default
  group_by: ['alertname', 'cluster']
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  routes:
  - match:
      severity: critical
    receiver: pagerduty
  - match:
      team: frontend
    receiver: slack-frontend

receivers:
- name: default
  email_configs:
  - to: 'alerts@example.com'
- name: pagerduty
  pagerduty_configs:
  - service_key: '<key>'
- name: slack-frontend
  slack_configs:
  - channel: '#frontend-alerts'`,
		uptime: time.Now().Add(-24 * time.Hour),
	}
}

// ============================================================
// 3. 캐싱 비활성화 미들웨어 (ui/web.go 참조)
// ============================================================

// disableCaching은 응답에 캐시 비활성화 헤더를 설정한다.
// 실제 코드: ui/web.go의 disableCaching
func disableCaching(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// ============================================================
// 4. CORS 미들웨어 (api/v2/api.go 참조)
// ============================================================

// corsMiddleware는 CORS 헤더를 설정한다.
// 실제 코드: api/v2/api.go에서 cors.Default().Handler 사용
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ============================================================
// 5. 동시성 제한 핸들러 (api/api.go 참조)
// ============================================================

// ConcurrencyLimiter는 GET 요청의 동시성을 세마포어로 제한한다.
// 실제 코드: api/api.go의 limitHandler
type ConcurrencyLimiter struct {
	sem              chan struct{}
	inFlight         int64
	limitExceeded    int64
	timeout          time.Duration
}

func NewConcurrencyLimiter(maxConcurrent int, timeout time.Duration) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		sem:     make(chan struct{}, maxConcurrent),
		timeout: timeout,
	}
}

func (cl *ConcurrencyLimiter) Wrap(next http.Handler) http.Handler {
	concLimiter := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET 요청만 동시성 제한 적용 (실제 코드와 동일)
		if r.Method == http.MethodGet {
			select {
			case cl.sem <- struct{}{}:
				// 세마포어 획득 성공
				atomic.AddInt64(&cl.inFlight, 1)
				defer func() {
					<-cl.sem
					atomic.AddInt64(&cl.inFlight, -1)
				}()
			default:
				// 동시성 제한 초과 → 503 반환
				atomic.AddInt64(&cl.limitExceeded, 1)
				http.Error(w, fmt.Sprintf(
					"Limit of concurrent GET requests reached (%d), try again later.\n",
					cap(cl.sem)),
					http.StatusServiceUnavailable)
				return
			}
		}
		next.ServeHTTP(w, r)
	})

	if cl.timeout <= 0 {
		return concLimiter
	}
	return http.TimeoutHandler(concLimiter, cl.timeout, fmt.Sprintf(
		"Exceeded configured timeout of %v.\n", cl.timeout))
}

// ============================================================
// 6. 요청 계측 미들웨어 (api/api.go 참조)
// ============================================================

// RequestMetrics는 요청 지표를 수집한다.
type RequestMetrics struct {
	mu       sync.Mutex
	counts   map[string]int
	durations map[string]time.Duration
}

func NewRequestMetrics() *RequestMetrics {
	return &RequestMetrics{
		counts:    make(map[string]int),
		durations: make(map[string]time.Duration),
	}
}

// instrumentHandler는 요청의 경로별 지표를 수집한다.
// 실제 코드: api/api.go의 instrumentHandler
// 높은 카디널리티 방지를 위해 사일런스 ID를 플레이스홀더로 교체
func (rm *RequestMetrics) instrumentHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// 카디널리티 정규화 (실제 코드: api/api.go)
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/v2/silence/") {
			path = "/api/v2/silence/{silenceID}"
		}

		next.ServeHTTP(w, r)

		duration := time.Since(start)
		rm.mu.Lock()
		rm.counts[path]++
		rm.durations[path] += duration
		rm.mu.Unlock()
	})
}

func (rm *RequestMetrics) PrintStats() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	fmt.Println("\n  [요청 지표]")
	for path, count := range rm.counts {
		avgDuration := rm.durations[path] / time.Duration(count)
		fmt.Printf("    %-40s 요청수: %d, 평균 응답시간: %v\n", path, count, avgDuration)
	}
}

// ============================================================
// 7. API v2 핸들러 (api/v2/api.go 참조)
// ============================================================

// APIServer는 Web UI가 호출하는 REST API를 구현한다.
type APIServer struct {
	store   *DataStore
	metrics *RequestMetrics
}

func NewAPIServer(store *DataStore, metrics *RequestMetrics) *APIServer {
	return &APIServer{store: store, metrics: metrics}
}

// getStatusHandler는 상태 정보를 반환한다.
// 실제 코드: api/v2/api.go의 getStatusHandler
func (api *APIServer) getStatusHandler(w http.ResponseWriter, r *http.Request) {
	api.store.mu.RLock()
	defer api.store.mu.RUnlock()

	status := AlertmanagerStatus{
		Uptime: api.store.uptime.Format(time.RFC3339),
		VersionInfo: VersionInfo{
			Version:   "0.27.0",
			Revision:  "abc1234def",
			Branch:    "main",
			BuildUser: "ci@build.local",
			BuildDate: "2024-01-15T10:30:00Z",
			GoVersion: "go1.21.5",
		},
		Config: AlertmanagerConfig{
			Original: api.store.config,
		},
		Cluster: ClusterStatus{
			Status: "ready",
			Name:   "alertmanager-0",
			Peers: []PeerStatus{
				{Name: "alertmanager-0", Address: "10.0.0.1:9094"},
				{Name: "alertmanager-1", Address: "10.0.0.2:9094"},
				{Name: "alertmanager-2", Address: "10.0.0.3:9094"},
			},
		},
	}

	// 캐싱 비활성화 (실제 코드: api/v2/api.go의 setResponseHeaders)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// getAlertGroupsHandler는 알림 그룹을 반환한다.
// 실제 코드: api/v2/api.go의 getAlertGroupsHandler
func (api *APIServer) getAlertGroupsHandler(w http.ResponseWriter, r *http.Request) {
	api.store.mu.RLock()
	defer api.store.mu.RUnlock()

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(api.store.alerts)
}

// getSilencesHandler는 사일런스 목록을 반환한다.
// 실제 코드: api/v2/api.go의 getSilencesHandler
func (api *APIServer) getSilencesHandler(w http.ResponseWriter, r *http.Request) {
	api.store.mu.RLock()
	defer api.store.mu.RUnlock()

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(api.store.silences)
}

// getReceiversHandler는 리시버 목록을 반환한다.
// 실제 코드: api/v2/api.go의 getReceiversHandler
func (api *APIServer) getReceiversHandler(w http.ResponseWriter, r *http.Request) {
	receivers := []Receiver{
		{Name: "default"},
		{Name: "pagerduty"},
		{Name: "slack-alerts"},
		{Name: "slack-frontend"},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(receivers)
}

// ============================================================
// 8. SPA 정적 파일 서빙 (ui/web.go 참조)
// ============================================================

// 임베딩된 HTML (실제로는 //go:embed로 app/ 디렉토리를 임베딩)
// 이 PoC에서는 인라인 HTML로 시뮬레이션
const indexHTML = `<!DOCTYPE html>
<html lang="ko">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Alertmanager - Web UI PoC</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #f5f5f5; }
        .header { background: #e6522c; color: white; padding: 0 20px; height: 60px; display: flex; align-items: center; gap: 30px; }
        .header h1 { font-size: 20px; font-weight: normal; }
        .nav { display: flex; gap: 5px; }
        .nav a { color: white; text-decoration: none; padding: 8px 16px; border-radius: 4px; font-size: 14px; }
        .nav a:hover, .nav a.active { background: rgba(255,255,255,0.2); }
        .content { max-width: 1200px; margin: 20px auto; padding: 0 20px; }
        .card { background: white; border-radius: 8px; box-shadow: 0 1px 3px rgba(0,0,0,0.1); margin-bottom: 16px; overflow: hidden; }
        .card-header { background: #fafafa; padding: 12px 16px; font-weight: 600; border-bottom: 1px solid #eee; font-size: 14px; }
        .card-body { padding: 16px; }
        table { width: 100%; border-collapse: collapse; font-size: 13px; }
        th, td { padding: 8px 12px; text-align: left; border-bottom: 1px solid #eee; }
        th { font-weight: 600; color: #555; }
        .badge { display: inline-block; padding: 2px 8px; border-radius: 3px; font-size: 12px; font-weight: 500; }
        .badge-active { background: #fce4ec; color: #c62828; }
        .badge-silenced { background: #e3f2fd; color: #1565c0; }
        .badge-expired { background: #f5f5f5; color: #757575; }
        .badge-ready { background: #e8f5e9; color: #2e7d32; }
        .page { display: none; }
        .page.active { display: block; }
        .config-block { background: #263238; color: #eeffff; padding: 16px; border-radius: 4px; font-family: monospace; font-size: 13px; white-space: pre; overflow-x: auto; }
        .loader { text-align: center; padding: 40px; color: #999; }
        #status-indicator { font-size: 12px; margin-left: auto; opacity: 0.8; }
    </style>
</head>
<body>
    <div class="header">
        <h1>Alertmanager</h1>
        <div class="nav">
            <a href="#" onclick="showPage('alerts')" id="nav-alerts" class="active">Alerts</a>
            <a href="#" onclick="showPage('silences')" id="nav-silences">Silences</a>
            <a href="#" onclick="showPage('status')" id="nav-status">Status</a>
            <a href="#" onclick="showPage('config')" id="nav-config">Config</a>
        </div>
        <span id="status-indicator">Connected</span>
    </div>
    <div class="content">
        <!-- Alerts Page -->
        <div id="page-alerts" class="page active">
            <div class="loader" id="alerts-loader">Loading alerts...</div>
            <div id="alerts-content"></div>
        </div>
        <!-- Silences Page -->
        <div id="page-silences" class="page">
            <div class="loader" id="silences-loader">Loading silences...</div>
            <div id="silences-content"></div>
        </div>
        <!-- Status Page -->
        <div id="page-status" class="page">
            <div class="loader" id="status-loader">Loading status...</div>
            <div id="status-content"></div>
        </div>
        <!-- Config Page -->
        <div id="page-config" class="page">
            <div class="loader" id="config-loader">Loading config...</div>
            <div id="config-content"></div>
        </div>
    </div>
    <script>
    // SPA 라우팅 (실제 Elm UI는 /#/ 기반, Mantine UI는 BrowserRouter 기반)
    const API_PATH = 'api/v2';

    function showPage(name) {
        document.querySelectorAll('.page').forEach(p => p.classList.remove('active'));
        document.querySelectorAll('.nav a').forEach(a => a.classList.remove('active'));
        document.getElementById('page-' + name).classList.add('active');
        document.getElementById('nav-' + name).classList.add('active');
        loadPageData(name);
    }

    // API 호출 함수 (실제: ui/mantine-ui/src/data/api.ts의 createQueryFn)
    async function fetchAPI(path) {
        const res = await fetch('/' + API_PATH + path, {
            cache: 'no-store',
            credentials: 'same-origin'
        });
        return res.json();
    }

    async function loadPageData(page) {
        try {
            switch(page) {
                case 'alerts':
                    const groups = await fetchAPI('/alerts/groups');
                    renderAlerts(groups);
                    break;
                case 'silences':
                    const silences = await fetchAPI('/silences');
                    renderSilences(silences);
                    break;
                case 'status':
                    const status = await fetchAPI('/status');
                    renderStatus(status);
                    break;
                case 'config':
                    const cfg = await fetchAPI('/status');
                    renderConfig(cfg);
                    break;
            }
        } catch(e) {
            console.error('API error:', e);
        }
    }

    function renderAlerts(groups) {
        let html = '';
        groups.forEach(group => {
            html += '<div class="card"><div class="card-header">';
            html += Object.entries(group.labels).map(([k,v]) => k+'='+v).join(', ');
            html += ' (receiver: ' + group.receiver.name + ')</div><div class="card-body">';
            html += '<table><tr><th>Labels</th><th>Annotations</th><th>State</th><th>Started</th></tr>';
            group.alerts.forEach(alert => {
                const labels = Object.entries(alert.labels).map(([k,v]) => k+'="'+v+'"').join(' ');
                const annotations = alert.annotations.summary || '';
                const badgeClass = alert.state === 'active' ? 'badge-active' : 'badge-silenced';
                html += '<tr><td style="font-family:monospace;font-size:12px">' + labels + '</td>';
                html += '<td>' + annotations + '</td>';
                html += '<td><span class="badge ' + badgeClass + '">' + alert.state + '</span></td>';
                html += '<td>' + new Date(alert.startsAt).toLocaleString() + '</td></tr>';
            });
            html += '</table></div></div>';
        });
        document.getElementById('alerts-content').innerHTML = html;
        document.getElementById('alerts-loader').style.display = 'none';
    }

    function renderSilences(silences) {
        let html = '<div class="card"><div class="card-header">Silences</div><div class="card-body">';
        html += '<table><tr><th>ID</th><th>Matchers</th><th>State</th><th>Created By</th><th>Comment</th><th>Ends At</th></tr>';
        silences.forEach(sil => {
            const matchers = sil.matchers.map(m => m.name + (m.isEqual ? '=' : '!=') + '"' + m.value + '"').join(' ');
            const badgeClass = sil.status.state === 'active' ? 'badge-active' : 'badge-expired';
            html += '<tr><td style="font-family:monospace">' + sil.id + '</td>';
            html += '<td style="font-family:monospace">' + matchers + '</td>';
            html += '<td><span class="badge ' + badgeClass + '">' + sil.status.state + '</span></td>';
            html += '<td>' + sil.createdBy + '</td>';
            html += '<td>' + sil.comment + '</td>';
            html += '<td>' + new Date(sil.endsAt).toLocaleString() + '</td></tr>';
        });
        html += '</table></div></div>';
        document.getElementById('silences-content').innerHTML = html;
        document.getElementById('silences-loader').style.display = 'none';
    }

    function renderStatus(status) {
        let html = '<div class="card"><div class="card-header">Build Information</div><div class="card-body">';
        html += '<table>';
        html += '<tr><th>Version</th><td>' + status.versionInfo.version + '</td></tr>';
        html += '<tr><th>Revision</th><td>' + status.versionInfo.revision + '</td></tr>';
        html += '<tr><th>Branch</th><td>' + status.versionInfo.branch + '</td></tr>';
        html += '<tr><th>Build User</th><td>' + status.versionInfo.buildUser + '</td></tr>';
        html += '<tr><th>Build Date</th><td>' + status.versionInfo.buildDate + '</td></tr>';
        html += '<tr><th>Go Version</th><td>' + status.versionInfo.goVersion + '</td></tr>';
        html += '</table></div></div>';

        html += '<div class="card"><div class="card-header">Runtime Information</div><div class="card-body">';
        html += '<table>';
        html += '<tr><th>Uptime</th><td>' + status.uptime + '</td></tr>';
        html += '<tr><th>Cluster Name</th><td>' + status.cluster.name + '</td></tr>';
        html += '<tr><th>Cluster Status</th><td><span class="badge badge-ready">' + status.cluster.status + '</span></td></tr>';
        html += '<tr><th>Number of Peers</th><td>' + status.cluster.peers.length + '</td></tr>';
        html += '</table></div></div>';

        html += '<div class="card"><div class="card-header">Cluster Peers</div><div class="card-body">';
        html += '<table><tr><th>Peer Name</th><th>Address</th></tr>';
        status.cluster.peers.forEach(peer => {
            html += '<tr><td>' + peer.name + '</td><td>' + peer.address + '</td></tr>';
        });
        html += '</table></div></div>';
        document.getElementById('status-content').innerHTML = html;
        document.getElementById('status-loader').style.display = 'none';
    }

    function renderConfig(status) {
        let html = '<div class="card"><div class="card-body">';
        html += '<div class="config-block">' + status.config.original + '</div>';
        html += '</div></div>';
        document.getElementById('config-content').innerHTML = html;
        document.getElementById('config-loader').style.display = 'none';
    }

    // 초기 페이지 로드
    loadPageData('alerts');
    </script>
</body>
</html>`

// ============================================================
// 9. 설정 리로드 채널 패턴 (ui/web.go 참조)
// ============================================================

// reloadHandler는 설정 리로드 요청을 처리한다.
// 실제 코드: ui/web.go의 /-/reload 핸들러
// 채널 기반 동기 리로드 패턴 구현
func reloadHandler(reloadCh chan<- chan error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		errc := make(chan error)
		defer close(errc)

		reloadCh <- errc
		if err := <-errc; err != nil {
			http.Error(w, fmt.Sprintf("failed to reload config: %s", err),
				http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "Config reloaded successfully")
	}
}

// ============================================================
// 메인 - Web UI 서버 구성
// ============================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║     Alertmanager Web UI 시뮬레이터 (PoC-18)            ║")
	fmt.Println("║     내장 웹 인터페이스의 핵심 아키텍처 재현            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 데이터 저장소 초기화
	store := NewDataStore()
	metrics := NewRequestMetrics()
	api := NewAPIServer(store, metrics)

	// 동시성 제한기 (실제: api/api.go의 inFlightSem)
	// 최대 8개 동시 GET 요청, 30초 타임아웃
	limiter := NewConcurrencyLimiter(8, 30*time.Second)

	// 리로드 채널 (실제: cmd/alertmanager/main.go의 webReload)
	reloadCh := make(chan chan error)

	// 리로드 워커 (실제로는 main goroutine에서 처리)
	go func() {
		for errc := range reloadCh {
			fmt.Println("  [리로드] 설정 리로드 요청 수신")
			// 실제로는 config.LoadFile() 호출
			time.Sleep(100 * time.Millisecond) // 시뮬레이션
			fmt.Println("  [리로드] 설정 리로드 완료")
			errc <- nil // 성공
		}
	}()

	// ─── 라우터 구성 ──────────────────────────────────────
	mux := http.NewServeMux()

	// 1. SPA 진입점 (실제: ui/web.go의 r.Get("/", ...))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// SPA 폴백: 알려진 경로가 아니면 index.html 반환
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/api/") &&
			r.URL.Path != "/-/reload" && r.URL.Path != "/-/healthy" &&
			r.URL.Path != "/-/ready" && r.URL.Path != "/metrics" {
			// SPA 라우팅: 모든 경로를 index.html로 폴백
			disableCaching(w)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, indexHTML)
			return
		}
		disableCaching(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})

	// 2. API v2 엔드포인트 (실제: api/v2/api.go의 핸들러들)
	mux.HandleFunc("/api/v2/status", api.getStatusHandler)
	mux.HandleFunc("/api/v2/alerts/groups", api.getAlertGroupsHandler)
	mux.HandleFunc("/api/v2/silences", api.getSilencesHandler)
	mux.HandleFunc("/api/v2/receivers", api.getReceiversHandler)

	// 3. 운영 엔드포인트 (실제: ui/web.go)
	mux.HandleFunc("/-/reload", reloadHandler(reloadCh))

	// 4. 헬스체크 (실제: ui/web.go의 /-/healthy, /-/ready)
	mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})
	mux.HandleFunc("/-/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})

	// 5. 메트릭 엔드포인트 (실제: promhttp.Handler())
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "# HELP alertmanager_http_requests_in_flight Current number of HTTP requests being processed.\n")
		fmt.Fprintf(w, "# TYPE alertmanager_http_requests_in_flight gauge\n")
		fmt.Fprintf(w, "alertmanager_http_requests_in_flight{method=\"get\"} %d\n",
			atomic.LoadInt64(&limiter.inFlight))
		fmt.Fprintf(w, "# HELP alertmanager_http_concurrency_limit_exceeded_total Total number of times concurrency limit was reached.\n")
		fmt.Fprintf(w, "# TYPE alertmanager_http_concurrency_limit_exceeded_total counter\n")
		fmt.Fprintf(w, "alertmanager_http_concurrency_limit_exceeded_total{method=\"get\"} %d\n",
			atomic.LoadInt64(&limiter.limitExceeded))
	})

	// ─── 미들웨어 체인 구성 ───────────────────────────────
	// 실제: api.Register()에서 mux 생성 후 CORS + 계측 + 동시성제한 적용
	handler := corsMiddleware(metrics.instrumentHandler(limiter.Wrap(mux)))

	// ─── 서버 시작 ────────────────────────────────────────
	addr := ":19093"
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	fmt.Println("서버 구성 요약:")
	fmt.Println("  ┌────────────────────────────────────────────┐")
	fmt.Println("  │  CORS Middleware                           │")
	fmt.Println("  │  └─ Request Metrics (계측)                │")
	fmt.Println("  │     └─ Concurrency Limiter (세마포어)      │")
	fmt.Println("  │        └─ Timeout Handler (30s)            │")
	fmt.Println("  │           └─ HTTP Mux                      │")
	fmt.Println("  │              ├─ / → SPA (index.html)       │")
	fmt.Println("  │              ├─ /api/v2/status             │")
	fmt.Println("  │              ├─ /api/v2/alerts/groups      │")
	fmt.Println("  │              ├─ /api/v2/silences           │")
	fmt.Println("  │              ├─ /api/v2/receivers          │")
	fmt.Println("  │              ├─ /-/reload                  │")
	fmt.Println("  │              ├─ /-/healthy                 │")
	fmt.Println("  │              ├─ /-/ready                   │")
	fmt.Println("  │              └─ /metrics                   │")
	fmt.Println("  └────────────────────────────────────────────┘")
	fmt.Println()

	// ─── 기능 테스트 ──────────────────────────────────────
	fmt.Println("━━━ 자동 기능 테스트 ━━━")
	fmt.Println()

	// 테스트를 위해 서버를 고루틴에서 잠시 시작
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("서버 에러: %v", err)
		}
	}()

	// 서버 시작 대기
	time.Sleep(200 * time.Millisecond)

	client := &http.Client{Timeout: 5 * time.Second}

	// 테스트 1: 헬스체크
	fmt.Print("  1. GET /-/healthy → ")
	resp, err := client.Get("http://localhost" + addr + "/-/healthy")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		fmt.Printf("%d OK\n", resp.StatusCode)
		resp.Body.Close()
	}

	// 테스트 2: 레디니스
	fmt.Print("  2. GET /-/ready → ")
	resp, err = client.Get("http://localhost" + addr + "/-/ready")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		fmt.Printf("%d OK\n", resp.StatusCode)
		resp.Body.Close()
	}

	// 테스트 3: Status API
	fmt.Print("  3. GET /api/v2/status → ")
	resp, err = client.Get("http://localhost" + addr + "/api/v2/status")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		var status AlertmanagerStatus
		json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()
		fmt.Printf("%d OK (version=%s, cluster=%s, peers=%d)\n",
			resp.StatusCode, status.VersionInfo.Version, status.Cluster.Status, len(status.Cluster.Peers))
	}

	// 테스트 4: Alert Groups API
	fmt.Print("  4. GET /api/v2/alerts/groups → ")
	resp, err = client.Get("http://localhost" + addr + "/api/v2/alerts/groups")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		var groups []AlertGroup
		json.NewDecoder(resp.Body).Decode(&groups)
		resp.Body.Close()
		totalAlerts := 0
		for _, g := range groups {
			totalAlerts += len(g.Alerts)
		}
		fmt.Printf("%d OK (groups=%d, total_alerts=%d)\n",
			resp.StatusCode, len(groups), totalAlerts)
	}

	// 테스트 5: Silences API
	fmt.Print("  5. GET /api/v2/silences → ")
	resp, err = client.Get("http://localhost" + addr + "/api/v2/silences")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		var silences []Silence
		json.NewDecoder(resp.Body).Decode(&silences)
		resp.Body.Close()
		active := 0
		for _, s := range silences {
			if s.Status.State == "active" {
				active++
			}
		}
		fmt.Printf("%d OK (total=%d, active=%d)\n",
			resp.StatusCode, len(silences), active)
	}

	// 테스트 6: Receivers API
	fmt.Print("  6. GET /api/v2/receivers → ")
	resp, err = client.Get("http://localhost" + addr + "/api/v2/receivers")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		var receivers []Receiver
		json.NewDecoder(resp.Body).Decode(&receivers)
		resp.Body.Close()
		fmt.Printf("%d OK (receivers=%d)\n", resp.StatusCode, len(receivers))
	}

	// 테스트 7: 설정 리로드
	fmt.Print("  7. POST /-/reload → ")
	resp, err = client.Post("http://localhost"+addr+"/-/reload", "", nil)
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		fmt.Printf("%d OK\n", resp.StatusCode)
		resp.Body.Close()
	}

	// 테스트 8: CORS 헤더 확인
	fmt.Print("  8. OPTIONS (CORS) → ")
	req, _ := http.NewRequest("OPTIONS", "http://localhost"+addr+"/api/v2/status", nil)
	resp, err = client.Do(req)
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		cors := resp.Header.Get("Access-Control-Allow-Origin")
		fmt.Printf("%d OK (CORS: %s)\n", resp.StatusCode, cors)
		resp.Body.Close()
	}

	// 테스트 9: 캐싱 비활성화 헤더 확인
	fmt.Print("  9. Cache-Control 헤더 → ")
	resp, err = client.Get("http://localhost" + addr + "/api/v2/status")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		cc := resp.Header.Get("Cache-Control")
		fmt.Printf("'%s'\n", cc)
		resp.Body.Close()
	}

	// 테스트 10: SPA 페이지
	fmt.Print("  10. GET / (SPA) → ")
	resp, err = client.Get("http://localhost" + addr + "/")
	if err != nil {
		fmt.Printf("에러: %v\n", err)
	} else {
		ct := resp.Header.Get("Content-Type")
		fmt.Printf("%d OK (Content-Type: %s)\n", resp.StatusCode, ct)
		resp.Body.Close()
	}

	// 요청 지표 출력
	metrics.PrintStats()

	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("Web UI 시뮬레이션 완료!")
	fmt.Println()
	fmt.Println("핵심 아키텍처 요약:")
	fmt.Println("  1. embed.FS: 프론트엔드 정적 파일을 Go 바이너리에 임베딩")
	fmt.Println("  2. API v2: OpenAPI(Swagger) 기반 REST API 제공")
	fmt.Println("  3. 동시성 제한: 세마포어로 GET 요청 동시 처리 수 제한")
	fmt.Println("  4. 캐싱 비활성화: 실시간 데이터를 위한 no-cache 헤더")
	fmt.Println("  5. CORS: 개발/외부 도구 연동 지원")
	fmt.Println("  6. 리로드 채널: 동기 설정 리로드 패턴")
	fmt.Println("  7. 헬스/레디니스: 로드밸런서 통합 엔드포인트")
	fmt.Println("  8. 요청 계측: 경로별 요청 수/응답 시간 메트릭")
	fmt.Println()

	// 서버 종료
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	// 시그널 대기 없이 종료 (테스트 완료)
	_ = syscall.SIGTERM
	_ = os.Interrupt
	_ = signal.Notify
}
