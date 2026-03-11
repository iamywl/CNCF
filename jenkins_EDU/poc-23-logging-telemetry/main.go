package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Jenkins LogRecorder + Telemetry 시뮬레이션
// =============================================================================
//
// Jenkins는 LogRecorder로 특정 패키지/클래스의 로그를 수집하고,
// Telemetry 시스템으로 익명 사용 데이터를 수집한다.
//
// 핵심 개념:
//   - LogRecorder: java.util.logging 기반 커스텀 로그 수집기
//   - Log Level: SEVERE, WARNING, INFO, FINE, FINER, FINEST
//   - Telemetry: 플러그인 사용 통계, 에러 정보 등 익명 수집
//   - Ring Buffer: 로그를 원형 버퍼에 저장
//
// 실제 코드 참조:
//   - core/src/main/java/hudson/logging/LogRecorder.java
//   - core/src/main/java/jenkins/telemetry/Telemetry.java
// =============================================================================

// --- Log Level ---

type LogLevel int

const (
	FINEST  LogLevel = iota
	FINER
	FINE
	CONFIG
	INFO
	WARNING
	SEVERE
)

func (l LogLevel) String() string {
	return []string{"FINEST", "FINER", "FINE", "CONFIG", "INFO", "WARNING", "SEVERE"}[l]
}

// --- Log Record ---

type LogRecord struct {
	Timestamp time.Time
	Level     LogLevel
	Logger    string // 패키지/클래스 이름
	Message   string
	Thrown    string // 예외 정보
}

func (r LogRecord) String() string {
	ts := r.Timestamp.Format("15:04:05.000")
	thrown := ""
	if r.Thrown != "" {
		thrown = "\n      " + r.Thrown
	}
	return fmt.Sprintf("[%s] %-7s %s: %s%s", ts, r.Level, r.Logger, r.Message, thrown)
}

// --- Ring Buffer ---

type RingBuffer struct {
	mu       sync.Mutex
	records  []LogRecord
	capacity int
	head     int
	size     int
}

func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		records:  make([]LogRecord, capacity),
		capacity: capacity,
	}
}

func (rb *RingBuffer) Add(record LogRecord) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.records[rb.head] = record
	rb.head = (rb.head + 1) % rb.capacity
	if rb.size < rb.capacity {
		rb.size++
	}
}

func (rb *RingBuffer) GetAll() []LogRecord {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	result := make([]LogRecord, 0, rb.size)
	start := (rb.head - rb.size + rb.capacity) % rb.capacity
	for i := 0; i < rb.size; i++ {
		idx := (start + i) % rb.capacity
		result = append(result, rb.records[idx])
	}
	return result
}

// --- LogRecorder ---

type LogTarget struct {
	Logger string
	Level  LogLevel
}

type LogRecorder struct {
	Name    string
	Targets []LogTarget
	Buffer  *RingBuffer
}

func NewLogRecorder(name string, bufferSize int) *LogRecorder {
	return &LogRecorder{
		Name:   name,
		Buffer: NewRingBuffer(bufferSize),
	}
}

func (lr *LogRecorder) AddTarget(logger string, level LogLevel) {
	lr.Targets = append(lr.Targets, LogTarget{Logger: logger, Level: level})
}

// ShouldCapture는 로그 레코드가 이 LogRecorder에 의해 캡처되어야 하는지 판단한다.
func (lr *LogRecorder) ShouldCapture(record LogRecord) bool {
	for _, target := range lr.Targets {
		if strings.HasPrefix(record.Logger, target.Logger) && record.Level >= target.Level {
			return true
		}
	}
	return false
}

func (lr *LogRecorder) Process(record LogRecord) {
	if lr.ShouldCapture(record) {
		lr.Buffer.Add(record)
	}
}

// --- Telemetry ---

type TelemetryCollector struct {
	Name        string
	Description string
	StartDate   time.Time
	EndDate     time.Time
}

type TelemetryData struct {
	CollectorName string
	Timestamp     time.Time
	Data          map[string]interface{}
}

type TelemetryManager struct {
	mu         sync.Mutex
	collectors []TelemetryCollector
	data       []TelemetryData
	enabled    bool
	correlator string
}

func NewTelemetryManager(enabled bool) *TelemetryManager {
	return &TelemetryManager{
		enabled:    enabled,
		correlator: fmt.Sprintf("jenkins-%d", rand.Intn(999999)),
	}
}

func (tm *TelemetryManager) RegisterCollector(c TelemetryCollector) {
	tm.collectors = append(tm.collectors, c)
}

func (tm *TelemetryManager) Collect(collectorName string, data map[string]interface{}) {
	if !tm.enabled {
		return
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()

	data["correlator"] = tm.correlator
	tm.data = append(tm.data, TelemetryData{
		CollectorName: collectorName,
		Timestamp:     time.Now(),
		Data:          data,
	})
}

func (tm *TelemetryManager) GetData() []TelemetryData {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return append([]TelemetryData{}, tm.data...)
}

// --- 로그 생성기 (시뮬레이션) ---

func generateLogs(r *rand.Rand) []LogRecord {
	loggers := []string{
		"hudson.model.Run",
		"hudson.model.Queue",
		"hudson.slaves.NodeProvisioner",
		"jenkins.security.SecurityListener",
		"hudson.plugins.git.GitSCM",
		"org.jenkinsci.plugins.pipeline.StageView",
		"hudson.model.AbstractBuild",
		"jenkins.model.Jenkins",
		"hudson.remoting.Channel",
		"hudson.security.ACL",
	}

	messages := []struct {
		level   LogLevel
		message string
		thrown   string
	}{
		{INFO, "Build started for job: my-app #42", ""},
		{INFO, "Agent 'worker-1' connected", ""},
		{WARNING, "Slow SCM checkout: 45s", ""},
		{FINE, "Checking permissions for user: admin", ""},
		{SEVERE, "Build failed with exit code 1", "java.lang.RuntimeException: Process returned 1\n      at hudson.Launcher$ProcStarter.join()"},
		{INFO, "Plugin 'git' version 5.2.0 loaded", ""},
		{WARNING, "Queue blocked: no available executors", ""},
		{FINE, "Channel established to agent", ""},
		{FINER, "Deserializing build data", ""},
		{SEVERE, "Security violation detected", "java.security.AccessControlException: access denied"},
		{INFO, "Build completed: SUCCESS", ""},
		{CONFIG, "Setting system property: jenkins.install.state=NEW", ""},
		{WARNING, "Plugin 'deprecated-plugin' is deprecated", ""},
		{INFO, "Provisioning new agent node", ""},
		{FINE, "Git fetch from origin", ""},
	}

	var records []LogRecord
	for i := 0; i < 30; i++ {
		msg := messages[r.Intn(len(messages))]
		records = append(records, LogRecord{
			Timestamp: time.Now().Add(time.Duration(i) * 100 * time.Millisecond),
			Level:     msg.level,
			Logger:    loggers[r.Intn(len(loggers))],
			Message:   msg.message,
			Thrown:    msg.thrown,
		})
	}
	return records
}

func main() {
	fmt.Println("=== Jenkins LogRecorder + Telemetry 시뮬레이션 ===")
	fmt.Println()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	// --- LogRecorder 설정 ---
	fmt.Println("[1] LogRecorder 설정")
	fmt.Println(strings.Repeat("-", 60))

	// 보안 로그
	securityRecorder := NewLogRecorder("Security Logs", 100)
	securityRecorder.AddTarget("jenkins.security", FINE)
	securityRecorder.AddTarget("hudson.security", FINE)
	fmt.Printf("  '%s': jenkins.security(FINE), hudson.security(FINE)\n", securityRecorder.Name)

	// 빌드 로그
	buildRecorder := NewLogRecorder("Build Activity", 50)
	buildRecorder.AddTarget("hudson.model.Run", INFO)
	buildRecorder.AddTarget("hudson.model.Queue", INFO)
	buildRecorder.AddTarget("hudson.model.AbstractBuild", INFO)
	fmt.Printf("  '%s': hudson.model.Run(INFO), hudson.model.Queue(INFO)\n", buildRecorder.Name)

	// Git 플러그인 디버그
	gitRecorder := NewLogRecorder("Git Debug", 200)
	gitRecorder.AddTarget("hudson.plugins.git", FINE)
	fmt.Printf("  '%s': hudson.plugins.git(FINE)\n", gitRecorder.Name)

	// All errors
	errorRecorder := NewLogRecorder("All Errors", 100)
	errorRecorder.AddTarget("", SEVERE) // 빈 문자열 = 모든 로거
	fmt.Printf("  '%s': *(SEVERE)\n", errorRecorder.Name)
	fmt.Println()

	// --- 로그 처리 ---
	fmt.Println("[2] 로그 처리 (30개 레코드)")
	fmt.Println(strings.Repeat("-", 60))

	logs := generateLogs(r)
	recorders := []*LogRecorder{securityRecorder, buildRecorder, gitRecorder, errorRecorder}

	for _, log := range logs {
		for _, recorder := range recorders {
			recorder.Process(log)
		}
	}

	for _, recorder := range recorders {
		records := recorder.Buffer.GetAll()
		fmt.Printf("  %s: %d records captured\n", recorder.Name, len(records))
	}
	fmt.Println()

	// --- LogRecorder 내용 출력 ---
	fmt.Println("[3] 'All Errors' LogRecorder 내용")
	fmt.Println(strings.Repeat("-", 60))
	for _, record := range errorRecorder.Buffer.GetAll() {
		fmt.Printf("  %s\n", record)
	}
	fmt.Println()

	fmt.Println("[4] 'Security Logs' LogRecorder 내용")
	fmt.Println(strings.Repeat("-", 60))
	for _, record := range securityRecorder.Buffer.GetAll() {
		fmt.Printf("  %s\n", record)
	}
	fmt.Println()

	// --- Telemetry ---
	fmt.Println("[5] Telemetry 수집")
	fmt.Println(strings.Repeat("-", 60))

	telemetry := NewTelemetryManager(true)
	telemetry.RegisterCollector(TelemetryCollector{
		Name:        "plugin-usage",
		Description: "플러그인 사용 통계",
		StartDate:   time.Now(),
		EndDate:     time.Now().Add(30 * 24 * time.Hour),
	})
	telemetry.RegisterCollector(TelemetryCollector{
		Name:        "java-version",
		Description: "Java 버전 분포",
	})

	// 텔레메트리 데이터 수집
	telemetry.Collect("plugin-usage", map[string]interface{}{
		"installed_plugins": 45,
		"active_plugins":    38,
		"top_plugins":       []string{"git", "pipeline", "credentials", "docker"},
	})
	telemetry.Collect("java-version", map[string]interface{}{
		"java_version": "17.0.9",
		"java_vendor":  "Eclipse Adoptium",
		"os":           "Linux",
	})
	telemetry.Collect("plugin-usage", map[string]interface{}{
		"build_count_7d": 142,
		"agent_count":    5,
		"pipeline_count": 23,
	})

	for _, td := range telemetry.GetData() {
		fmt.Printf("  [%s] %s:\n", td.Timestamp.Format("15:04:05"), td.CollectorName)
		for k, v := range td.Data {
			fmt.Printf("    %s = %v\n", k, v)
		}
	}
	fmt.Println()

	// --- 통계 ---
	fmt.Println("[6] 로그 레벨 분포")
	fmt.Println(strings.Repeat("-", 60))
	levelCounts := make(map[LogLevel]int)
	for _, log := range logs {
		levelCounts[log.Level]++
	}
	for level := FINEST; level <= SEVERE; level++ {
		count := levelCounts[level]
		bar := strings.Repeat("#", count)
		fmt.Printf("  %-8s %2d %s\n", level, count, bar)
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
