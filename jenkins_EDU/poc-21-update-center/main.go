// Package main은 Jenkins UpdateCenter 시스템의 핵심 개념을 시뮬레이션한다.
//
// Jenkins UpdateCenter는 다음 핵심 메커니즘으로 동작한다:
// 1. UpdateSite: 업데이트 메타데이터 소스 (update-center.json)
// 2. UpdateCenter: 중앙 관리자 — 다운로드/설치 작업 조율
// 3. UpdateCenterJob 계층: ConnectionCheck → Download → Install → Restart
// 4. AtmostOneThreadExecutor: 순차적 설치
// 5. 서명/체크섬 검증으로 보안 보장
//
// 이 PoC는 Go 표준 라이브러리만으로 이 전체 파이프라인을 재현한다.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. 데이터 모델
// =============================================================================

// PluginInfo는 플러그인 메타데이터
// Jenkins 원본: hudson/model/UpdateSite.java의 Plugin
type PluginInfo struct {
	Name         string
	Title        string
	Version      string
	URL          string
	SHA256       string
	Excerpt      string
	Dependencies []Dependency
	Installed    bool   // 이미 설치됨
	InstalledVer string // 설치된 버전
}

func (p *PluginInfo) HasUpdate() bool {
	return p.Installed && p.InstalledVer != p.Version
}

func (p *PluginInfo) IsCompatible() bool {
	return true // 단순화
}

// Dependency는 플러그인 의존성
type Dependency struct {
	Name     string
	Version  string
	Optional bool
}

// CoreInfo는 Jenkins 코어 정보
type CoreInfo struct {
	Version string
	URL     string
	SHA256  string
}

// Warning은 보안 경고
type Warning struct {
	ID       string
	Message  string
	URL      string
	Versions []string // 영향받는 플러그인 버전
}

// =============================================================================
// 2. UpdateSite
// =============================================================================

// UpdateSite는 업데이트 메타데이터 소스
// Jenkins 원본: hudson/model/UpdateSite.java
type UpdateSite struct {
	ID            string
	URL           string
	Plugins       map[string]*PluginInfo
	Core          *CoreInfo
	Warnings      []Warning
	DataTimestamp time.Time
	LastAttempt   time.Time
	RetryWindow   time.Duration
}

func NewUpdateSite(id, url string) *UpdateSite {
	return &UpdateSite{
		ID:      id,
		URL:     url,
		Plugins: make(map[string]*PluginInfo),
	}
}

// UpdateDirectly는 메타데이터를 직접 갱신한다
// Jenkins 원본: UpdateSite.updateDirectly()
func (s *UpdateSite) UpdateDirectly() error {
	fmt.Printf("  [UpdateSite:%s] 메타데이터 갱신 중: %s\n", s.ID, s.URL)

	// 서버에서 update-center.json 다운로드 시뮬레이션
	time.Sleep(100 * time.Millisecond)

	// 서명 검증 시뮬레이션
	fmt.Printf("  [UpdateSite:%s] 서명 검증 완료\n", s.ID)

	// 메타데이터 파싱 시뮬레이션
	s.loadSampleData()
	s.DataTimestamp = time.Now()

	fmt.Printf("  [UpdateSite:%s] %d개 플러그인 정보 로드 완료\n",
		s.ID, len(s.Plugins))
	return nil
}

// loadSampleData는 샘플 플러그인 데이터를 로드
func (s *UpdateSite) loadSampleData() {
	plugins := []PluginInfo{
		{
			Name: "git", Title: "Git plugin", Version: "5.3.0",
			URL:     "https://updates.jenkins.io/download/plugins/git/5.3.0/git.hpi",
			SHA256:  "a1b2c3d4e5f6",
			Excerpt: "Jenkins에 Git 통합을 제공하는 플러그인",
			Dependencies: []Dependency{
				{Name: "git-client", Version: "4.0.0", Optional: false},
				{Name: "credentials", Version: "2.6.0", Optional: false},
			},
		},
		{
			Name: "pipeline", Title: "Pipeline", Version: "2.7",
			URL:     "https://updates.jenkins.io/download/plugins/pipeline/2.7/pipeline.hpi",
			SHA256:  "b2c3d4e5f6a1",
			Excerpt: "Jenkins Pipeline 기능을 제공하는 플러그인",
		},
		{
			Name: "docker-workflow", Title: "Docker Pipeline", Version: "1.30",
			URL:     "https://updates.jenkins.io/download/plugins/docker-workflow/1.30/docker-workflow.hpi",
			SHA256:  "c3d4e5f6a1b2",
			Excerpt: "Pipeline에서 Docker를 사용할 수 있게 해주는 플러그인",
			Dependencies: []Dependency{
				{Name: "pipeline", Version: "2.5", Optional: false},
			},
		},
		{
			Name: "credentials", Title: "Credentials", Version: "2.8.0",
			URL:     "https://updates.jenkins.io/download/plugins/credentials/2.8.0/credentials.hpi",
			SHA256:  "d4e5f6a1b2c3",
			Excerpt: "자격 증명 관리 플러그인",
		},
		{
			Name: "git-client", Title: "Git Client", Version: "4.2.0",
			URL:     "https://updates.jenkins.io/download/plugins/git-client/4.2.0/git-client.hpi",
			SHA256:  "e5f6a1b2c3d4",
			Excerpt: "Git 클라이언트 라이브러리 플러그인",
		},
		{
			Name: "blueocean", Title: "Blue Ocean", Version: "1.27.0",
			URL:     "https://updates.jenkins.io/download/plugins/blueocean/1.27.0/blueocean.hpi",
			SHA256:  "f6a1b2c3d4e5",
			Excerpt: "현대적 Jenkins UI",
			Dependencies: []Dependency{
				{Name: "pipeline", Version: "2.6", Optional: false},
			},
		},
	}

	for _, p := range plugins {
		pi := p // 복사
		s.Plugins[p.Name] = &pi
	}

	s.Core = &CoreInfo{
		Version: "2.463.0",
		URL:     "https://updates.jenkins.io/download/war/2.463.0/jenkins.war",
		SHA256:  "core-sha256-hash",
	}

	s.Warnings = []Warning{
		{
			ID:       "SECURITY-3400",
			Message:  "Git plugin XSS 취약점",
			URL:      "https://www.jenkins.io/security/advisory/2024-01-01/",
			Versions: []string{"5.2.0", "5.1.0"},
		},
	}
}

// GetUpdates는 업데이트 가능한 플러그인 목록을 반환
func (s *UpdateSite) GetUpdates() []*PluginInfo {
	var updates []*PluginInfo
	for _, p := range s.Plugins {
		if p.HasUpdate() {
			updates = append(updates, p)
		}
	}
	return updates
}

// IsDue는 갱신이 필요한지 확인
func (s *UpdateSite) IsDue() bool {
	if s.DataTimestamp.IsZero() {
		return true
	}
	return time.Since(s.DataTimestamp) > 24*time.Hour
}

// =============================================================================
// 3. UpdateCenterJob 계층
// =============================================================================

// JobStatus는 작업 상태
type JobStatus string

const (
	StatusPending    JobStatus = "Pending"
	StatusRunning    JobStatus = "Running"
	StatusSuccess    JobStatus = "Success"
	StatusFailure    JobStatus = "Failure"
	StatusCancelled  JobStatus = "Cancelled"
)

// UpdateCenterJob는 업데이트 센터 작업의 기본 인터페이스
type UpdateCenterJob interface {
	GetID() int
	GetType() string
	GetStatus() JobStatus
	Run()
}

// ConnectionCheckJob은 연결 상태 확인 작업
type ConnectionCheckJob struct {
	ID          int
	Status      JobStatus
	SiteID      string
	Internet    string // "OK" or "FAILED"
	UpdateSite  string // "OK" or "FAILED"
}

func (j *ConnectionCheckJob) GetID() int         { return j.ID }
func (j *ConnectionCheckJob) GetType() string     { return "ConnectionCheck" }
func (j *ConnectionCheckJob) GetStatus() JobStatus { return j.Status }

func (j *ConnectionCheckJob) Run() {
	j.Status = StatusRunning
	fmt.Printf("  [ConnectionCheck] 연결 확인 중...\n")

	// 인터넷 연결 확인 시뮬레이션
	time.Sleep(50 * time.Millisecond)
	j.Internet = "OK"
	fmt.Printf("  [ConnectionCheck] 인터넷: %s\n", j.Internet)

	// UpdateSite 연결 확인 시뮬레이션
	time.Sleep(50 * time.Millisecond)
	j.UpdateSite = "OK"
	fmt.Printf("  [ConnectionCheck] 업데이트 사이트: %s\n", j.UpdateSite)

	j.Status = StatusSuccess
}

// InstallationJob은 플러그인 설치 작업
// Jenkins 원본: hudson/model/UpdateCenter.java의 InstallationJob
type InstallationJob struct {
	ID            int
	Status        JobStatus
	Plugin        *PluginInfo
	DynamicLoad   bool
	DownloadPct   int
	ErrorMessage  string
	RequiresRestart bool
}

func (j *InstallationJob) GetID() int         { return j.ID }
func (j *InstallationJob) GetType() string     { return "Installation" }
func (j *InstallationJob) GetStatus() JobStatus { return j.Status }

func (j *InstallationJob) Run() {
	j.Status = StatusRunning

	// 1단계: 다운로드
	fmt.Printf("  [Install:%s] 다운로드 중: %s\n", j.Plugin.Name, j.Plugin.URL)
	for i := 0; i <= 100; i += 20 {
		j.DownloadPct = i
		time.Sleep(30 * time.Millisecond)
	}

	// 2단계: SHA-256 체크섬 검증
	fmt.Printf("  [Install:%s] SHA-256 체크섬 검증...\n", j.Plugin.Name)
	simulatedData := fmt.Sprintf("plugin-data-%s-%d", j.Plugin.Name, rand.Intn(1000))
	hash := sha256.Sum256([]byte(simulatedData))
	computedHash := hex.EncodeToString(hash[:])
	fmt.Printf("  [Install:%s] 체크섬: %s...\n", j.Plugin.Name, computedHash[:16])

	// 3단계: 설치 (파일 이동)
	fmt.Printf("  [Install:%s] 설치 중 (plugins/ 디렉토리로 이동)...\n", j.Plugin.Name)
	time.Sleep(20 * time.Millisecond)

	// 4단계: 동적 로드 또는 재시작 필요
	if j.DynamicLoad {
		fmt.Printf("  [Install:%s] 동적 로드 완료 (재시작 불필요)\n", j.Plugin.Name)
		j.RequiresRestart = false
	} else {
		fmt.Printf("  [Install:%s] 재시작 필요\n", j.Plugin.Name)
		j.RequiresRestart = true
	}

	j.Plugin.Installed = true
	j.Plugin.InstalledVer = j.Plugin.Version
	j.Status = StatusSuccess
	fmt.Printf("  [Install:%s] v%s 설치 완료!\n", j.Plugin.Name, j.Plugin.Version)
}

// =============================================================================
// 4. UpdateCenter
// =============================================================================

// UpdateCenter는 플러그인 업데이트/설치의 중앙 관리자
// Jenkins 원본: hudson/model/UpdateCenter.java
type UpdateCenter struct {
	Sites           []*UpdateSite
	Jobs            []UpdateCenterJob
	RequiresRestart bool
	nextJobID       int
	installChan     chan UpdateCenterJob // AtmostOneThreadExecutor 시뮬레이션
	mu              sync.Mutex
}

func NewUpdateCenter() *UpdateCenter {
	uc := &UpdateCenter{
		installChan: make(chan UpdateCenterJob, 100),
	}

	// AtmostOneThreadExecutor: 한 번에 하나의 설치만 처리
	go func() {
		for job := range uc.installChan {
			job.Run()
		}
	}()

	return uc
}

// AddSite는 UpdateSite를 추가
func (uc *UpdateCenter) AddSite(site *UpdateSite) {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	uc.Sites = append(uc.Sites, site)
}

// addJob은 작업을 추가
func (uc *UpdateCenter) addJob(job UpdateCenterJob) {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	uc.Jobs = append(uc.Jobs, job)
}

// UpdateAllSites는 모든 사이트의 메타데이터를 갱신
func (uc *UpdateCenter) UpdateAllSites() {
	fmt.Println("  [UpdateCenter] 모든 사이트 메타데이터 갱신 시작")
	var wg sync.WaitGroup
	for _, site := range uc.Sites {
		wg.Add(1)
		go func(s *UpdateSite) {
			defer wg.Done()
			s.UpdateDirectly()
		}(site)
	}
	wg.Wait()
	fmt.Println("  [UpdateCenter] 모든 사이트 갱신 완료")
}

// CheckConnection은 연결 상태를 확인
func (uc *UpdateCenter) CheckConnection(siteID string) *ConnectionCheckJob {
	uc.nextJobID++
	job := &ConnectionCheckJob{
		ID:     uc.nextJobID,
		Status: StatusPending,
		SiteID: siteID,
	}
	uc.addJob(job)
	job.Run()
	return job
}

// Install은 플러그인을 설치
func (uc *UpdateCenter) Install(plugin *PluginInfo, dynamicLoad bool) *InstallationJob {
	uc.nextJobID++
	job := &InstallationJob{
		ID:          uc.nextJobID,
		Status:      StatusPending,
		Plugin:      plugin,
		DynamicLoad: dynamicLoad,
	}
	uc.addJob(job)

	// AtmostOneThreadExecutor로 전달 (순차 실행)
	uc.installChan <- job

	return job
}

// InstallWithDependencies는 의존성을 포함하여 플러그인을 설치
func (uc *UpdateCenter) InstallWithDependencies(pluginName string) {
	site := uc.Sites[0] // 첫 번째 사이트에서 검색

	plugin, ok := site.Plugins[pluginName]
	if !ok {
		fmt.Printf("  [UpdateCenter] 플러그인 '%s'을(를) 찾을 수 없음\n", pluginName)
		return
	}

	// 의존성 해결 (DFS)
	resolved := make(map[string]bool)
	uc.resolveDependencies(plugin, site, resolved)
}

func (uc *UpdateCenter) resolveDependencies(plugin *PluginInfo, site *UpdateSite, resolved map[string]bool) {
	if resolved[plugin.Name] {
		return
	}

	// 의존성 먼저 설치
	for _, dep := range plugin.Dependencies {
		if dep.Optional {
			continue
		}
		depPlugin, ok := site.Plugins[dep.Name]
		if ok && !depPlugin.Installed {
			uc.resolveDependencies(depPlugin, site, resolved)
		}
	}

	// 자신을 설치
	if !plugin.Installed {
		resolved[plugin.Name] = true
		fmt.Printf("  [UpdateCenter] 의존성 해결: %s v%s\n", plugin.Name, plugin.Version)
		uc.Install(plugin, true)
	}
}

// GetUpdates는 업데이트 가능한 플러그인 목록
func (uc *UpdateCenter) GetUpdates() []*PluginInfo {
	var updates []*PluginInfo
	for _, site := range uc.Sites {
		updates = append(updates, site.GetUpdates()...)
	}
	return updates
}

// GetPlugin은 이름으로 플러그인 검색
func (uc *UpdateCenter) GetPlugin(name string) *PluginInfo {
	for _, site := range uc.Sites {
		if p, ok := site.Plugins[name]; ok {
			return p
		}
	}
	return nil
}

// GetBadge는 업데이트 알림 배지를 생성
// Jenkins 원본: UpdateCenter.getBadge()
func (uc *UpdateCenter) GetBadge() string {
	updates := uc.GetUpdates()
	if len(updates) == 0 {
		return ""
	}

	securityFixes := 0
	for _, u := range updates {
		for _, site := range uc.Sites {
			for _, w := range site.Warnings {
				for _, v := range w.Versions {
					if v == u.InstalledVer {
						securityFixes++
					}
				}
			}
		}
	}

	severity := "WARNING"
	if securityFixes > 0 {
		severity = "DANGER"
	}

	return fmt.Sprintf("[%s] %d개 업데이트 가능 (보안: %d개)",
		severity, len(updates), securityFixes)
}

// PrintJobs는 작업 목록을 출력
func (uc *UpdateCenter) PrintJobs() {
	uc.mu.Lock()
	defer uc.mu.Unlock()

	if len(uc.Jobs) == 0 {
		fmt.Println("  (작업 없음)")
		return
	}

	fmt.Println("  ┌─────┬───────────────────┬──────────────────┬──────────┐")
	fmt.Println("  │ ID  │ 유형               │ 대상             │ 상태     │")
	fmt.Println("  ├─────┼───────────────────┼──────────────────┼──────────┤")
	for _, job := range uc.Jobs {
		target := ""
		switch j := job.(type) {
		case *ConnectionCheckJob:
			target = j.SiteID
		case *InstallationJob:
			target = j.Plugin.Name + " v" + j.Plugin.Version
		}
		fmt.Printf("  │ %-3d │ %-17s │ %-16s │ %-8s │\n",
			job.GetID(), job.GetType(), target, job.GetStatus())
	}
	fmt.Println("  └─────┴───────────────────┴──────────────────┴──────────┘")
}

// =============================================================================
// 메인 데모
// =============================================================================

func main() {
	fmt.Println("=== Jenkins UpdateCenter 시스템 시뮬레이션 ===")
	fmt.Println()

	// UpdateCenter 생성
	uc := NewUpdateCenter()

	// ─────────────────────────────────────────────────
	// 1. UpdateSite 등록 및 메타데이터 갱신
	// ─────────────────────────────────────────────────
	fmt.Println("--- 1. UpdateSite 등록 및 메타데이터 갱신 ---")

	defaultSite := NewUpdateSite("default", "https://updates.jenkins.io/update-center.json")
	uc.AddSite(defaultSite)

	uc.UpdateAllSites()
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 2. 연결 상태 확인
	// ─────────────────────────────────────────────────
	fmt.Println("--- 2. 연결 상태 확인 ---")
	uc.CheckConnection("default")
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 3. 플러그인 검색
	// ─────────────────────────────────────────────────
	fmt.Println("--- 3. 사용 가능한 플러그인 ---")
	for name, p := range defaultSite.Plugins {
		deps := ""
		if len(p.Dependencies) > 0 {
			depNames := make([]string, len(p.Dependencies))
			for i, d := range p.Dependencies {
				depNames[i] = d.Name
			}
			deps = " [의존: " + strings.Join(depNames, ", ") + "]"
		}
		fmt.Printf("  %s v%s - %s%s\n", name, p.Version, p.Excerpt, deps)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 4. 의존성 포함 플러그인 설치
	// ─────────────────────────────────────────────────
	fmt.Println("--- 4. 의존성 해결 + 플러그인 설치 ---")
	fmt.Println("  git 플러그인 설치 요청 (의존성: git-client, credentials)")
	uc.InstallWithDependencies("git")

	// 순차 설치 완료 대기
	time.Sleep(1 * time.Second)
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 5. 추가 플러그인 설치
	// ─────────────────────────────────────────────────
	fmt.Println("--- 5. 추가 플러그인 설치 ---")
	fmt.Println("  blueocean 설치 요청 (의존성: pipeline)")
	uc.InstallWithDependencies("blueocean")
	time.Sleep(800 * time.Millisecond)
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 6. 업데이트 확인
	// ─────────────────────────────────────────────────
	fmt.Println("--- 6. 업데이트 확인 ---")

	// 설치된 플러그인의 버전을 이전 버전으로 변경 (업데이트 가능 시뮬레이션)
	if gitPlugin := uc.GetPlugin("git"); gitPlugin != nil {
		gitPlugin.InstalledVer = "5.2.0" // 이전 버전
	}

	updates := uc.GetUpdates()
	if len(updates) > 0 {
		fmt.Println("  업데이트 가능한 플러그인:")
		for _, u := range updates {
			fmt.Printf("    %s: %s → %s\n", u.Name, u.InstalledVer, u.Version)
		}
	}

	badge := uc.GetBadge()
	if badge != "" {
		fmt.Printf("  알림 배지: %s\n", badge)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 7. 보안 경고 확인
	// ─────────────────────────────────────────────────
	fmt.Println("--- 7. 보안 경고 ---")
	for _, site := range uc.Sites {
		for _, w := range site.Warnings {
			fmt.Printf("  [%s] %s\n", w.ID, w.Message)
			fmt.Printf("  URL: %s\n", w.URL)
			fmt.Printf("  영향받는 버전: %v\n", w.Versions)
		}
	}
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 8. 작업 이력
	// ─────────────────────────────────────────────────
	fmt.Println("--- 8. 작업 이력 ---")
	uc.PrintJobs()

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}
