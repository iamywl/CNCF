// Prometheus Config Reload PoC
//
// Prometheus의 설정 리로드 메커니즘을 시뮬레이션한다.
// 실제 구현: cmd/prometheus/main.go의 reloadConfig() 함수와 reloader 구조체
//
// 핵심 개념:
// 1. reloader 체인: 설정 변경 시 정해진 순서로 각 서브시스템에 적용
// 2. 3가지 리로드 트리거: SIGHUP, HTTP POST /-/reload, 자동 체크섬 감지
// 3. 체크섬 기반 변경 감지: SHA256으로 설정 파일 변경 여부 판단
// 4. 메트릭: config_last_reload_successful, config_last_reload_success_timestamp
//
// 사용법: go run main.go

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ============================================================================
// Config: Prometheus 설정 구조체
// 실제 구현: config/config.go의 Config 구조체
// ============================================================================

type ScrapeConfig struct {
	JobName        string `json:"job_name"`
	ScrapeInterval string `json:"scrape_interval"`
	StaticTargets  []string `json:"static_targets"`
}

type Config struct {
	GlobalScrapeInterval string         `json:"global_scrape_interval"`
	EvaluationInterval   string         `json:"evaluation_interval"`
	ScrapeConfigs        []ScrapeConfig `json:"scrape_configs"`
	RuleFiles            []string       `json:"rule_files"`
}

// ============================================================================
// Reloader: 설정을 적용받는 서브시스템 인터페이스
// 실제 구현: cmd/prometheus/main.go의 reloader 구조체
//
//	type reloader struct {
//	    name     string
//	    reloader func(*config.Config) error
//	}
//
// ============================================================================

type Reloader struct {
	Name   string
	Apply  func(*Config) error
}

// ============================================================================
// ConfigLoader: 설정 파일 로드 및 체크섬 생성
// 실제 구현: config/config.go의 LoadFile(), config/reload.go의 GenerateChecksum()
// ============================================================================

// LoadFile은 JSON 형식의 설정 파일을 로드한다.
// 실제 Prometheus는 YAML을 사용하지만, 표준 라이브러리만 사용하기 위해 JSON을 사용.
func LoadFile(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("couldn't load configuration (--config.file=%q): %w", filename, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("couldn't parse configuration file: %w", err)
	}

	return &cfg, nil
}

// GenerateChecksum은 설정 파일의 SHA256 체크섬을 생성한다.
// 실제 구현: config/reload.go의 GenerateChecksum()
// 실제로는 YAML 파일 + 참조하는 rule_files, scrape_config_files까지 포함해서 해싱.
func GenerateChecksum(filename string) (string, error) {
	hash := sha256.New()

	content, err := os.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("error reading config file: %w", err)
	}

	if _, err := hash.Write(content); err != nil {
		return "", fmt.Errorf("error writing config to hash: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ============================================================================
// ReloadMetrics: 리로드 성공/실패 메트릭
// 실제 구현: cmd/prometheus/main.go의 configSuccess, configSuccessTime 게이지
// ============================================================================

type ReloadMetrics struct {
	mu                        sync.RWMutex
	ConfigLastReloadSuccessful float64   // 1=성공, 0=실패
	ConfigLastReloadTimestamp   time.Time // 마지막 성공 시각
	TotalReloads               int
	FailedReloads              int
}

func (m *ReloadMetrics) SetSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ConfigLastReloadSuccessful = 1
	m.ConfigLastReloadTimestamp = time.Now()
	m.TotalReloads++
}

func (m *ReloadMetrics) SetFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ConfigLastReloadSuccessful = 0
	m.TotalReloads++
	m.FailedReloads++
}

func (m *ReloadMetrics) Print() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fmt.Printf("  [메트릭] config_last_reload_successful=%.0f\n", m.ConfigLastReloadSuccessful)
	if !m.ConfigLastReloadTimestamp.IsZero() {
		fmt.Printf("  [메트릭] config_last_reload_success_timestamp_seconds=%d\n",
			m.ConfigLastReloadTimestamp.Unix())
	}
	fmt.Printf("  [메트릭] total_reloads=%d, failed_reloads=%d\n", m.TotalReloads, m.FailedReloads)
}

// ============================================================================
// reloadConfig: 핵심 리로드 함수
// 실제 구현: cmd/prometheus/main.go의 reloadConfig()
//
// 동작 순서:
// 1. 설정 파일 로드
// 2. 각 reloader를 순서대로 실행 (하나 실패해도 나머지 계속 실행)
// 3. 하나라도 실패하면 전체 실패로 마킹
// 4. 메트릭 업데이트
// ============================================================================

func reloadConfig(filename string, metrics *ReloadMetrics, reloaders []Reloader) error {
	start := time.Now()
	fmt.Printf("\n  설정 파일 로드 중... (filename=%s)\n", filename)

	conf, err := LoadFile(filename)
	if err != nil {
		metrics.SetFailure()
		return fmt.Errorf("couldn't load configuration: %w", err)
	}

	fmt.Printf("  설정 로드 완료: scrape_interval=%s, jobs=%d, rule_files=%d\n",
		conf.GlobalScrapeInterval, len(conf.ScrapeConfigs), len(conf.RuleFiles))

	// 실제 Prometheus와 동일: 실패한 reloader가 있어도 나머지는 계속 실행
	failed := false
	for _, rl := range reloaders {
		rstart := time.Now()
		if err := rl.Apply(conf); err != nil {
			fmt.Printf("  [실패] %s: %v (소요: %v)\n", rl.Name, err, time.Since(rstart))
			failed = true
		} else {
			fmt.Printf("  [성공] %s 적용 완료 (소요: %v)\n", rl.Name, time.Since(rstart))
		}
	}

	if failed {
		metrics.SetFailure()
		return fmt.Errorf("one or more errors occurred while applying the new configuration")
	}

	metrics.SetSuccess()
	fmt.Printf("  리로드 완료 (총 소요: %v)\n", time.Since(start))
	return nil
}

// ============================================================================
// 시뮬레이션된 서브시스템 Reloader들
// 실제 구현에서의 순서 (cmd/prometheus/main.go:1028):
//   1. db_storage      - localStorage.ApplyConfig
//   2. remote_storage   - remoteStorage.ApplyConfig
//   3. web_handler      - webHandler.ApplyConfig
//   4. query_engine     - queryEngine 설정 적용
//   5. scrape           - scrapeManager.ApplyConfig
//   6. scrape_sd        - discoveryManagerScrape.ApplyConfig
//   7. notify           - notifierManager.ApplyConfig
//   8. notify_sd        - discoveryManagerNotify.ApplyConfig
//   9. rules            - ruleManager.Update
// ============================================================================

func createReloaders(simulateRuleFailure bool) []Reloader {
	return []Reloader{
		{
			Name: "db_storage",
			Apply: func(cfg *Config) error {
				// 스토리지 엔진에 retention, exemplar 설정 반영
				return nil
			},
		},
		{
			Name: "remote_storage",
			Apply: func(cfg *Config) error {
				// remote_write/remote_read 엔드포인트 갱신
				return nil
			},
		},
		{
			Name: "web_handler",
			Apply: func(cfg *Config) error {
				// 외부 URL, CORS 등 웹 핸들러 설정 반영
				return nil
			},
		},
		{
			Name: "scrape_manager",
			Apply: func(cfg *Config) error {
				// 스크레이프 설정 반영: 잡 목록, 간격, 타겟 등
				for _, sc := range cfg.ScrapeConfigs {
					fmt.Printf("    → scrape job 적용: %s (interval=%s, targets=%v)\n",
						sc.JobName, sc.ScrapeInterval, sc.StaticTargets)
				}
				return nil
			},
		},
		{
			Name: "notifier",
			Apply: func(cfg *Config) error {
				// Alertmanager 알림 설정 반영
				return nil
			},
		},
		{
			Name: "rule_manager",
			Apply: func(cfg *Config) error {
				if simulateRuleFailure {
					return fmt.Errorf("failed to load rule file: rule_files 경로가 존재하지 않음")
				}
				for _, rf := range cfg.RuleFiles {
					fmt.Printf("    → rule file 적용: %s\n", rf)
				}
				return nil
			},
		},
	}
}

// ============================================================================
// 데모 실행
// ============================================================================

func main() {
	fmt.Println("=== Prometheus Config Reload PoC ===")
	fmt.Println()
	fmt.Println("실제 구현 위치:")
	fmt.Println("  - reloadConfig():    cmd/prometheus/main.go:1604")
	fmt.Println("  - reloader 목록:     cmd/prometheus/main.go:1028")
	fmt.Println("  - GenerateChecksum(): config/reload.go:33")
	fmt.Println("  - SIGHUP/HTTP/Auto:  cmd/prometheus/main.go:1264-1347")
	fmt.Println()

	// 임시 디렉토리에 설정 파일 생성
	tmpDir, err := os.MkdirTemp("", "prometheus-config-reload-poc")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	configFile := filepath.Join(tmpDir, "prometheus.json")
	metrics := &ReloadMetrics{}

	// ── 1단계: 초기 설정 파일 생성 및 로드 ──
	fmt.Println("━━━ 1단계: 초기 설정 로드 ━━━")

	initialConfig := Config{
		GlobalScrapeInterval: "15s",
		EvaluationInterval:   "15s",
		ScrapeConfigs: []ScrapeConfig{
			{
				JobName:        "prometheus",
				ScrapeInterval: "15s",
				StaticTargets:  []string{"localhost:9090"},
			},
		},
		RuleFiles: []string{"/etc/prometheus/rules/*.yml"},
	}

	data, _ := json.MarshalIndent(initialConfig, "", "  ")
	os.WriteFile(configFile, data, 0644)

	reloaders := createReloaders(false)
	if err := reloadConfig(configFile, metrics, reloaders); err != nil {
		fmt.Printf("  리로드 실패: %v\n", err)
	}
	metrics.Print()

	// 초기 체크섬 계산
	checksum, _ := GenerateChecksum(configFile)
	fmt.Printf("  초기 체크섬: %s...\n", checksum[:16])

	// ── 2단계: 설정 파일 변경 → 자동 리로드 감지 (체크섬 기반) ──
	fmt.Println()
	fmt.Println("━━━ 2단계: 자동 리로드 (체크섬 기반 변경 감지) ━━━")
	fmt.Println("  실제 Prometheus: time.Tick(autoReloadInterval)로 주기적 폴링")
	fmt.Println("  체크섬이 달라지면 reloadConfig() 호출")
	fmt.Println()

	// 설정 파일 수정: 새 잡 추가
	updatedConfig := Config{
		GlobalScrapeInterval: "10s",
		EvaluationInterval:   "10s",
		ScrapeConfigs: []ScrapeConfig{
			{
				JobName:        "prometheus",
				ScrapeInterval: "10s",
				StaticTargets:  []string{"localhost:9090"},
			},
			{
				JobName:        "node_exporter",
				ScrapeInterval: "10s",
				StaticTargets:  []string{"node1:9100", "node2:9100"},
			},
		},
		RuleFiles: []string{"/etc/prometheus/rules/*.yml", "/etc/prometheus/alerts/*.yml"},
	}
	data, _ = json.MarshalIndent(updatedConfig, "", "  ")
	os.WriteFile(configFile, data, 0644)

	// 체크섬 비교 (실제에서는 time.Tick 루프)
	newChecksum, _ := GenerateChecksum(configFile)
	fmt.Printf("  이전 체크섬: %s...\n", checksum[:16])
	fmt.Printf("  현재 체크섬: %s...\n", newChecksum[:16])

	if newChecksum != checksum {
		fmt.Println("  → 설정 파일 변경 감지! 리로드 시작...")
		if err := reloadConfig(configFile, metrics, reloaders); err != nil {
			fmt.Printf("  리로드 실패: %v\n", err)
		} else {
			checksum = newChecksum // 성공 시에만 체크섬 업데이트
		}
	}
	metrics.Print()

	// ── 3단계: HTTP /-/reload 엔드포인트 시뮬레이션 ──
	fmt.Println()
	fmt.Println("━━━ 3단계: HTTP /-/reload 엔드포인트 시뮬레이션 ━━━")
	fmt.Println("  실제 Prometheus: webHandler.Reload() 채널을 통해 리로드 트리거")
	fmt.Println("  POST /-/reload → 핸들러가 채널에 신호 → 리로드 goroutine이 수신")
	fmt.Println()

	// HTTP 리로드 채널 시뮬레이션
	// 실제: rc := <-webHandler.Reload()  →  reloadConfig()  →  rc <- err
	reloadCh := make(chan chan error, 1)

	// HTTP 핸들러 시뮬레이션
	go func() {
		rc := make(chan error, 1)
		reloadCh <- rc
		if err := <-rc; err != nil {
			fmt.Printf("  HTTP 응답: 500 - %v\n", err)
		} else {
			fmt.Println("  HTTP 응답: 200 OK - 리로드 성공")
		}
	}()

	// 리로드 goroutine이 채널에서 수신 (실제 main.go:1304-1316)
	time.Sleep(10 * time.Millisecond)
	select {
	case rc := <-reloadCh:
		fmt.Println("  /-/reload 요청 수신, 리로드 시작...")
		if err := reloadConfig(configFile, metrics, reloaders); err != nil {
			rc <- err
		} else {
			rc <- nil
		}
	default:
		fmt.Println("  (리로드 요청 없음)")
	}
	time.Sleep(10 * time.Millisecond)
	metrics.Print()

	// ── 4단계: Reloader 실패 시 동작 ──
	fmt.Println()
	fmt.Println("━━━ 4단계: Reloader 실패 시 동작 (Partial Reload) ━━━")
	fmt.Println("  실제 Prometheus: 하나의 reloader가 실패해도 나머지는 계속 실행")
	fmt.Println("  전체 실패로 마킹되어 configSuccess 메트릭이 0이 됨")
	fmt.Println()

	failReloaders := createReloaders(true) // rule_manager가 실패하도록 설정
	if err := reloadConfig(configFile, metrics, failReloaders); err != nil {
		fmt.Printf("  전체 리로드 결과: 실패 - %v\n", err)
	}
	metrics.Print()

	// ── 5단계: SIGHUP 신호 기반 리로드 (실제 시그널 핸들링) ──
	fmt.Println()
	fmt.Println("━━━ 5단계: SIGHUP 신호 기반 리로드 ━━━")
	fmt.Println("  실제 Prometheus: signal.Notify(hup, syscall.SIGHUP)")
	fmt.Println("  kill -HUP <pid> 또는 systemctl reload prometheus")
	fmt.Println()

	// SIGHUP 핸들러 등록
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	done := make(chan struct{})

	go func() {
		defer close(done)
		select {
		case <-hup:
			fmt.Println("  SIGHUP 수신! 리로드 시작...")
			if err := reloadConfig(configFile, metrics, reloaders); err != nil {
				fmt.Printf("  리로드 실패: %v\n", err)
			}
		case <-time.After(100 * time.Millisecond):
			fmt.Println("  (SIGHUP 시뮬레이션: 자기 자신에게 SIGHUP 전송)")
		}
	}()

	// 자기 자신에게 SIGHUP 전송
	proc, _ := os.FindProcess(os.Getpid())
	proc.Signal(syscall.SIGHUP)
	<-done
	time.Sleep(10 * time.Millisecond)
	metrics.Print()

	signal.Stop(hup)

	// ── 6단계: 전체 리로드 루프 시뮬레이션 ──
	fmt.Println()
	fmt.Println("━━━ 6단계: 통합 리로드 루프 (3가지 트리거 통합) ━━━")
	fmt.Println("  실제 Prometheus의 리로드 goroutine 구조 재현")
	fmt.Println()

	demonstrateReloadLoop(configFile, tmpDir, metrics, reloaders)

	// ── 7단계: HTTP 서버 시작 (/-/reload 엔드포인트) ──
	fmt.Println()
	fmt.Println("━━━ 7단계: HTTP /-/reload 엔드포인트 실제 동작 ━━━")

	httpReloadCh := make(chan chan error, 1)
	srv := startHTTPServer(httpReloadCh)
	defer srv.Close()

	// HTTP 리로드 핸들러 goroutine
	go func() {
		for rc := range httpReloadCh {
			fmt.Println("  HTTP 리로드 요청 수신, 처리 중...")
			if err := reloadConfig(configFile, metrics, reloaders); err != nil {
				rc <- err
			} else {
				rc <- nil
			}
		}
	}()

	// HTTP 리로드 요청 전송
	fmt.Println("  POST http://localhost:9191/-/reload 전송...")
	resp, err := http.Post("http://localhost:9191/-/reload", "", nil)
	if err != nil {
		fmt.Printf("  HTTP 요청 실패: %v\n", err)
	} else {
		fmt.Printf("  HTTP 응답: %d %s\n", resp.StatusCode, resp.Status)
		resp.Body.Close()
	}
	time.Sleep(50 * time.Millisecond)

	// ── 최종 메트릭 출력 ──
	fmt.Println()
	fmt.Println("━━━ 최종 메트릭 ━━━")
	metrics.Print()

	fmt.Println()
	fmt.Println("=== 주요 설계 포인트 ===")
	fmt.Println()
	fmt.Println("1. Reloader 순서가 중요:")
	fmt.Println("   - storage → scrape → notify → rules 순서로 적용")
	fmt.Println("   - scrape/notify가 discovery보다 먼저 적용되어야 함")
	fmt.Println("   - 새로운 타겟 목록을 받기 전에 최신 설정이 반영되어야 하기 때문")
	fmt.Println()
	fmt.Println("2. Partial failure 허용:")
	fmt.Println("   - 하나의 reloader가 실패해도 나머지는 계속 실행")
	fmt.Println("   - 전체 결과는 실패로 마킹 → configSuccess 메트릭 = 0")
	fmt.Println()
	fmt.Println("3. 체크섬 업데이트 타이밍:")
	fmt.Println("   - 리로드 성공 시에만 체크섬 업데이트")
	fmt.Println("   - 실패하면 다음 폴링에서 다시 시도")
	fmt.Println()
	fmt.Println("4. 3가지 트리거 모두 같은 reloadConfig() 함수를 호출:")
	fmt.Println("   - 코드 중복 없이 일관된 리로드 동작 보장")
}

// demonstrateReloadLoop는 실제 Prometheus의 리로드 goroutine 구조를 재현한다.
// 실제 구현: cmd/prometheus/main.go:1289-1347
func demonstrateReloadLoop(configFile, tmpDir string, metrics *ReloadMetrics, reloaders []Reloader) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	httpReload := make(chan chan error, 1)
	cancel := make(chan struct{})

	checksum, _ := GenerateChecksum(configFile)
	autoReloadInterval := 500 * time.Millisecond // 데모용으로 짧게

	// 리로드 루프 (실제 Prometheus 구조와 동일)
	go func() {
		ticker := time.NewTicker(autoReloadInterval)
		defer ticker.Stop()

		for {
			select {
			case <-hup:
				fmt.Println("  [루프] SIGHUP 수신 → 리로드")
				if err := reloadConfig(configFile, metrics, reloaders); err != nil {
					fmt.Printf("  [루프] 리로드 실패: %v\n", err)
				}

			case rc := <-httpReload:
				fmt.Println("  [루프] HTTP /-/reload 수신 → 리로드")
				if err := reloadConfig(configFile, metrics, reloaders); err != nil {
					rc <- err
				} else {
					rc <- nil
				}

			case <-ticker.C:
				currentChecksum, err := GenerateChecksum(configFile)
				if err != nil {
					continue
				}
				if currentChecksum == checksum {
					continue
				}
				fmt.Println("  [루프] 설정 파일 변경 감지 → 자동 리로드")
				if err := reloadConfig(configFile, metrics, reloaders); err != nil {
					fmt.Printf("  [루프] 리로드 실패: %v\n", err)
				} else {
					checksum = currentChecksum
				}

			case <-cancel:
				fmt.Println("  [루프] 리로드 루프 종료")
				return
			}
		}
	}()

	// 설정 파일 변경으로 자동 리로드 트리거
	time.Sleep(100 * time.Millisecond)
	fmt.Println("  → 설정 파일 변경 (자동 리로드 트리거)")
	modifiedConfig := Config{
		GlobalScrapeInterval: "5s",
		EvaluationInterval:   "5s",
		ScrapeConfigs: []ScrapeConfig{
			{
				JobName:        "prometheus",
				ScrapeInterval: "5s",
				StaticTargets:  []string{"localhost:9090"},
			},
			{
				JobName:        "cadvisor",
				ScrapeInterval: "5s",
				StaticTargets:  []string{"cadvisor:8080"},
			},
		},
		RuleFiles: []string{"/etc/prometheus/rules/*.yml"},
	}
	data, _ := json.MarshalIndent(modifiedConfig, "", "  ")
	os.WriteFile(configFile, data, 0644)

	// 자동 리로드 감지 대기
	time.Sleep(800 * time.Millisecond)

	// 루프 종료
	close(cancel)
	time.Sleep(50 * time.Millisecond)
}

// startHTTPServer는 /-/reload 엔드포인트를 제공하는 HTTP 서버를 시작한다.
func startHTTPServer(reloadCh chan chan error) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/-/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed. Use POST.", http.StatusMethodNotAllowed)
			return
		}
		rc := make(chan error, 1)
		reloadCh <- rc
		if err := <-rc; err != nil {
			http.Error(w, fmt.Sprintf("Reload failed: %v", err), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "Reload successful")
	})

	srv := &http.Server{Addr: ":9191", Handler: mux}
	go srv.ListenAndServe()
	time.Sleep(50 * time.Millisecond)
	fmt.Println("  HTTP 서버 시작: http://localhost:9191/-/reload")
	return srv
}
