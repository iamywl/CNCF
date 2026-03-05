package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Grafana 데이터소스 프록시 시뮬레이션
//
// Grafana는 pkg/api/datasource/proxy.go에서 데이터소스 프록시를 구현한다.
// 프론트엔드는 /api/datasources/proxy/:id/* 로 요청하고,
// Grafana 백엔드가 실제 데이터소스 URL로 요청을 전달한다.
// =============================================================================

// AccessType은 데이터소스 접근 방식이다.
type AccessType string

const (
	AccessProxy  AccessType = "proxy"  // Grafana 백엔드를 통한 프록시
	AccessDirect AccessType = "direct" // 브라우저에서 직접 접근
)

// DataSource는 데이터소스 설정이다.
// Grafana: pkg/services/datasources/models.go
type DataSource struct {
	UID           string
	Name          string
	Type          string // prometheus, loki, influxdb, etc.
	URL           string
	Access        AccessType
	BasicAuth     bool
	BasicAuthUser string
	BasicAuthPass string
	OAuthToken    string
	WhitelistRoutes []RouteRule
}

// RouteRule은 프록시 경로 화이트리스트 규칙이다.
type RouteRule struct {
	Path    string
	Methods []string
}

// ProxyAccessLog는 프록시 접근 로그 엔트리이다.
type ProxyAccessLog struct {
	Timestamp   time.Time
	DataSource  string
	Method      string
	OrigPath    string
	ProxiedURL  string
	StatusCode  int
	Duration    time.Duration
	AuthMethod  string
}

// AccessLogger는 프록시 접근 로그를 수집한다.
type AccessLogger struct {
	mu   sync.Mutex
	logs []ProxyAccessLog
}

func (l *AccessLogger) Log(entry ProxyAccessLog) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logs = append(l.logs, entry)
}

func (l *AccessLogger) All() []ProxyAccessLog {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := make([]ProxyAccessLog, len(l.logs))
	copy(result, l.logs)
	return result
}

// =============================================================================
// DataSourceProxy - 프록시 핸들러
// Grafana: pkg/api/datasource/proxy.go
// =============================================================================

// DataSourceProxy는 데이터소스로의 리버스 프록시이다.
type DataSourceProxy struct {
	ds       *DataSource
	logger   *AccessLogger
	targetURL *url.URL
}

func NewDataSourceProxy(ds *DataSource, logger *AccessLogger) (*DataSourceProxy, error) {
	target, err := url.Parse(ds.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid datasource URL: %w", err)
	}
	return &DataSourceProxy{
		ds:        ds,
		logger:    logger,
		targetURL: target,
	}, nil
}

// Director는 요청을 대상 URL로 재작성한다.
// httputil.ReverseProxy의 Director 함수에 해당한다.
func (p *DataSourceProxy) Director(req *http.Request) {
	// 원본 경로에서 프록시 접두사 제거
	// /api/datasources/proxy/:uid/api/v1/query → /api/v1/query
	proxyPrefix := "/proxy/" + p.ds.UID + "/"
	idx := strings.Index(req.URL.Path, proxyPrefix)
	if idx >= 0 {
		req.URL.Path = "/" + req.URL.Path[idx+len(proxyPrefix):]
	}

	// 대상 URL 설정
	req.URL.Scheme = p.targetURL.Scheme
	req.URL.Host = p.targetURL.Host
	req.Host = p.targetURL.Host

	// 인증 헤더 주입
	if p.ds.BasicAuth {
		auth := base64.StdEncoding.EncodeToString(
			[]byte(p.ds.BasicAuthUser + ":" + p.ds.BasicAuthPass))
		req.Header.Set("Authorization", "Basic "+auth)
	} else if p.ds.OAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.ds.OAuthToken)
	}

	// Grafana 관련 헤더 제거 (보안)
	req.Header.Del("Cookie")
	req.Header.Del("X-Grafana-Org-Id")

	// 프록시 식별 헤더 추가
	req.Header.Set("X-Grafana-Proxy", "true")
	req.Header.Set("X-Forwarded-For", req.RemoteAddr)
}

// ValidateRoute는 요청 경로가 화이트리스트에 있는지 확인한다.
func (p *DataSourceProxy) ValidateRoute(method, path string) bool {
	if len(p.ds.WhitelistRoutes) == 0 {
		return true // 화이트리스트 없으면 모두 허용
	}

	for _, rule := range p.ds.WhitelistRoutes {
		if strings.HasPrefix(path, rule.Path) {
			if len(rule.Methods) == 0 {
				return true // 메서드 제한 없음
			}
			for _, m := range rule.Methods {
				if m == method {
					return true
				}
			}
		}
	}
	return false
}

// ServeHTTP는 프록시 요청을 처리한다.
func (p *DataSourceProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()

	// 경로 추출
	proxyPrefix := "/proxy/" + p.ds.UID + "/"
	idx := strings.Index(req.URL.Path, proxyPrefix)
	proxyPath := "/"
	if idx >= 0 {
		proxyPath = "/" + req.URL.Path[idx+len(proxyPrefix):]
	}

	// 라우트 검증
	if !p.ValidateRoute(req.Method, proxyPath) {
		http.Error(w, `{"error":"route not allowed"}`, http.StatusForbidden)
		p.logger.Log(ProxyAccessLog{
			Timestamp:  time.Now(),
			DataSource: p.ds.Name,
			Method:     req.Method,
			OrigPath:   proxyPath,
			StatusCode: 403,
			Duration:   time.Since(start),
			AuthMethod: "blocked",
		})
		return
	}

	// 인증 방법 결정
	authMethod := "none"
	if p.ds.BasicAuth {
		authMethod = "basic-auth"
	} else if p.ds.OAuthToken != "" {
		authMethod = "oauth-token"
	}

	// 리버스 프록시 생성 및 실행
	proxy := &httputil.ReverseProxy{
		Director: p.Director,
		ModifyResponse: func(resp *http.Response) error {
			// 응답 로깅
			p.logger.Log(ProxyAccessLog{
				Timestamp:  time.Now(),
				DataSource: p.ds.Name,
				Method:     req.Method,
				OrigPath:   proxyPath,
				ProxiedURL: p.targetURL.String() + proxyPath,
				StatusCode: resp.StatusCode,
				Duration:   time.Since(start),
				AuthMethod: authMethod,
			})
			return nil
		},
	}

	proxy.ServeHTTP(w, req)
}

// =============================================================================
// 목 타겟 서버 (Prometheus 시뮬레이션)
// =============================================================================

func startMockTargetServer(listener net.Listener) {
	mux := http.NewServeMux()

	// Prometheus /api/v1/query 시뮬레이션
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		auth := r.Header.Get("Authorization")

		response := map[string]interface{}{
			"status": "success",
			"data": map[string]interface{}{
				"resultType": "vector",
				"result": []map[string]interface{}{
					{
						"metric": map[string]string{
							"__name__": "up",
							"job":      "prometheus",
							"instance": "localhost:9090",
						},
						"value": []interface{}{1709654400, "1"},
					},
				},
			},
			"_debug": map[string]interface{}{
				"received_query":  query,
				"received_auth":   auth != "",
				"received_method": r.Method,
				"proxy_header":    r.Header.Get("X-Grafana-Proxy"),
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// Prometheus /api/v1/label 시뮬레이션
	mux.HandleFunc("/api/v1/labels", func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"status": "success",
			"data":   []string{"__name__", "instance", "job"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// /metrics 엔드포인트
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# HELP up The number of targets up\nup{job=\"prometheus\"} 1\n"))
	})

	// 기본 핸들러
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		response := map[string]string{
			"message": "Mock Prometheus server",
			"path":    r.URL.Path,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	server := &http.Server{Handler: mux}
	server.Serve(listener)
}

// =============================================================================
// 프록시 서버
// =============================================================================

func startProxyServer(listener net.Listener, dataSources map[string]*DataSource, logger *AccessLogger) {
	mux := http.NewServeMux()

	mux.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		// UID 추출: /proxy/:uid/...
		path := strings.TrimPrefix(r.URL.Path, "/proxy/")
		slashIdx := strings.Index(path, "/")
		var uid string
		if slashIdx > 0 {
			uid = path[:slashIdx]
		} else {
			uid = path
		}

		ds, ok := dataSources[uid]
		if !ok {
			http.Error(w, `{"error":"datasource not found"}`, http.StatusNotFound)
			return
		}

		if ds.Access != AccessProxy {
			http.Error(w, `{"error":"datasource is not configured for proxy access"}`, http.StatusForbidden)
			return
		}

		proxy, err := NewDataSourceProxy(ds, logger)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
			return
		}

		proxy.ServeHTTP(w, r)
	})

	server := &http.Server{Handler: mux}
	server.Serve(listener)
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== Grafana 데이터소스 프록시 시뮬레이션 ===")
	fmt.Println()

	// ─── 목 타겟 서버 시작 ───
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("타겟 서버 시작 실패: %v\n", err)
		return
	}
	targetAddr := targetListener.Addr().String()
	go startMockTargetServer(targetListener)
	fmt.Printf("  목 타겟 서버 (Prometheus) 시작: http://%s\n", targetAddr)

	// ─── 데이터소스 설정 ───
	dataSources := map[string]*DataSource{
		"prometheus": {
			UID:           "prometheus",
			Name:          "Prometheus",
			Type:          "prometheus",
			URL:           "http://" + targetAddr,
			Access:        AccessProxy,
			BasicAuth:     true,
			BasicAuthUser: "admin",
			BasicAuthPass: "secret123",
			WhitelistRoutes: []RouteRule{
				{Path: "/api/v1/query", Methods: []string{"GET", "POST"}},
				{Path: "/api/v1/labels", Methods: []string{"GET"}},
				{Path: "/api/v1/query_range", Methods: []string{"GET", "POST"}},
				{Path: "/api/v1/series", Methods: []string{"GET", "POST"}},
			},
		},
		"loki": {
			UID:        "loki",
			Name:       "Loki",
			Type:       "loki",
			URL:        "http://" + targetAddr,
			Access:     AccessProxy,
			OAuthToken: "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.mock_token",
		},
		"direct-ds": {
			UID:    "direct-ds",
			Name:   "Direct Access DS",
			Type:   "influxdb",
			URL:    "http://" + targetAddr,
			Access: AccessDirect,
		},
	}

	// ─── 프록시 서버 시작 ───
	logger := &AccessLogger{}

	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("프록시 서버 시작 실패: %v\n", err)
		return
	}
	proxyAddr := proxyListener.Addr().String()
	go startProxyServer(proxyListener, dataSources, logger)
	fmt.Printf("  프록시 서버 시작: http://%s\n", proxyAddr)
	fmt.Println()

	// 서버 시작 대기
	time.Sleep(100 * time.Millisecond)

	client := &http.Client{Timeout: 5 * time.Second}

	// ─── 테스트 요청 ───
	testCases := []struct {
		desc   string
		method string
		path   string
	}{
		{
			desc:   "Prometheus 쿼리 (Basic Auth 프록시)",
			method: "GET",
			path:   "/proxy/prometheus/api/v1/query?query=up",
		},
		{
			desc:   "Prometheus 레이블 조회",
			method: "GET",
			path:   "/proxy/prometheus/api/v1/labels",
		},
		{
			desc:   "Loki 쿼리 (OAuth 토큰 프록시)",
			method: "GET",
			path:   "/proxy/loki/api/v1/query?query=up",
		},
		{
			desc:   "화이트리스트 외 경로 (Prometheus /metrics 차단)",
			method: "GET",
			path:   "/proxy/prometheus/metrics",
		},
		{
			desc:   "Direct Access 데이터소스 (프록시 거부)",
			method: "GET",
			path:   "/proxy/direct-ds/api/v1/query",
		},
		{
			desc:   "존재하지 않는 데이터소스",
			method: "GET",
			path:   "/proxy/nonexistent/api/v1/query",
		},
	}

	for i, tc := range testCases {
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("테스트 %d: %s\n", i+1, tc.desc)
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

		reqURL := fmt.Sprintf("http://%s%s", proxyAddr, tc.path)
		fmt.Printf("  요청: %s %s\n", tc.method, tc.path)

		req, _ := http.NewRequest(tc.method, reqURL, nil)
		req.Header.Set("X-Grafana-Org-Id", "1")
		req.Header.Set("Cookie", "grafana_session=abc123")

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("  오류: %v\n\n", err)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		fmt.Printf("  응답 코드: %d\n", resp.StatusCode)
		fmt.Printf("  Content-Type: %s\n", resp.Header.Get("Content-Type"))

		// JSON 정리 출력
		var prettyJSON map[string]interface{}
		if err := json.Unmarshal(body, &prettyJSON); err == nil {
			jsonBytes, _ := json.MarshalIndent(prettyJSON, "  ", "  ")
			bodyStr := string(jsonBytes)
			if len(bodyStr) > 500 {
				bodyStr = bodyStr[:500] + "\n  ..."
			}
			fmt.Printf("  응답 본문:\n  %s\n", bodyStr)
		} else {
			bodyStr := string(body)
			if len(bodyStr) > 200 {
				bodyStr = bodyStr[:200] + "..."
			}
			fmt.Printf("  응답 본문: %s\n", bodyStr)
		}
		fmt.Println()
	}

	// ─── 접근 로그 출력 ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("프록시 접근 로그")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	logs := logger.All()
	if len(logs) > 0 {
		fmt.Printf("\n  %-12s %-8s %-30s %-8s %-12s %s\n",
			"DataSource", "Method", "Path", "Status", "Duration", "Auth")
		fmt.Println("  " + strings.Repeat("-", 100))

		for _, log := range logs {
			fmt.Printf("  %-12s %-8s %-30s %-8d %-12v %s\n",
				log.DataSource, log.Method, log.OrigPath,
				log.StatusCode, log.Duration, log.AuthMethod)
		}
	} else {
		fmt.Println("  (로그 없음)")
	}

	// ─── 데이터소스 설정 요약 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("데이터소스 설정 요약")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	fmt.Printf("\n  %-15s %-12s %-10s %-12s %-15s %s\n",
		"Name", "Type", "Access", "Auth", "BasicAuthUser", "Whitelist")
	fmt.Println("  " + strings.Repeat("-", 85))

	for _, ds := range dataSources {
		auth := "none"
		if ds.BasicAuth {
			auth = "basic-auth"
		} else if ds.OAuthToken != "" {
			auth = "oauth"
		}
		whitelist := "all"
		if len(ds.WhitelistRoutes) > 0 {
			paths := make([]string, len(ds.WhitelistRoutes))
			for i, r := range ds.WhitelistRoutes {
				paths[i] = r.Path
			}
			whitelist = strings.Join(paths, ", ")
			if len(whitelist) > 30 {
				whitelist = whitelist[:30] + "..."
			}
		}
		fmt.Printf("  %-15s %-12s %-10s %-12s %-15s %s\n",
			ds.Name, ds.Type, ds.Access, auth, ds.BasicAuthUser, whitelist)
	}

	fmt.Println()
	fmt.Println("=== 프록시 흐름 다이어그램 ===")
	fmt.Println()
	fmt.Println("  브라우저                    Grafana Proxy                    데이터소스")
	fmt.Println("  ─────────                   ─────────────                    ──────────")
	fmt.Println("     │                             │                              │")
	fmt.Println("     │  GET /proxy/prom/api/query  │                              │")
	fmt.Println("     │ ──────────────────────────> │                              │")
	fmt.Println("     │                             │  1. 데이터소스 조회           │")
	fmt.Println("     │                             │  2. URL 재작성               │")
	fmt.Println("     │                             │  3. 인증 헤더 주입           │")
	fmt.Println("     │                             │  4. 보안 헤더 제거           │")
	fmt.Println("     │                             │                              │")
	fmt.Println("     │                             │  GET /api/v1/query           │")
	fmt.Println("     │                             │  Authorization: Basic xxx    │")
	fmt.Println("     │                             │ ──────────────────────────>  │")
	fmt.Println("     │                             │                              │")
	fmt.Println("     │                             │  200 OK {data: ...}          │")
	fmt.Println("     │                             │ <──────────────────────────  │")
	fmt.Println("     │  200 OK {data: ...}         │                              │")
	fmt.Println("     │ <────────────────────────── │                              │")
	fmt.Println("     │                             │                              │")
	fmt.Println()
	fmt.Println("=== 데이터소스 프록시 시뮬레이션 완료 ===")
}
