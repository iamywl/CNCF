// Package main은 Jenkins Setup Wizard 시스템의 핵심 개념을 시뮬레이션한다.
//
// Jenkins Setup Wizard는 다음 핵심 메커니즘으로 동작한다:
// 1. InstallState: 상태 기계 — UNKNOWN → NEW → INITIAL_SECURITY_SETUP → ... → RUNNING
// 2. SetupWizard: 초기 보안 설정, admin 계정 생성, 플러그인 설치 안내
// 3. FORCE_SETUP_WIZARD_FILTER: 설정 미완료 시 모든 요청을 위자드로 리다이렉트
// 4. 초기 비밀번호: UUID 기반 랜덤 비밀번호 → 파일 저장 → 콘솔 출력
//
// 이 PoC는 Go 표준 라이브러리만으로 이 전체 워크플로우를 재현한다.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// =============================================================================
// 1. InstallState — 상태 기계
// =============================================================================

// InstallState는 Jenkins 설치 상태
// Jenkins 원본: jenkins/install/InstallState.java
type InstallState struct {
	Name            string
	IsSetupComplete bool
	InitFunc        func(wizard *SetupWizard) // 상태 초기화 함수
}

// 모든 설치 상태 정의
var (
	StateUnknown = &InstallState{
		Name:            "UNKNOWN",
		IsSetupComplete: true,
		InitFunc: func(w *SetupWizard) {
			fmt.Println("  [InstallState] UNKNOWN → 설치 유형 판별 중...")
			// 신규 설치인지 기존 설치인지 판별
			if w.isNewInstall() {
				w.SetInstallState(StateNew)
			} else if w.isUpgrade() {
				w.SetInstallState(StateUpgrade)
			} else {
				w.SetInstallState(StateRestart)
			}
		},
	}

	StateNew = &InstallState{
		Name:            "NEW",
		IsSetupComplete: false,
	}

	StateInitialSecuritySetup = &InstallState{
		Name:            "INITIAL_SECURITY_SETUP",
		IsSetupComplete: false,
		InitFunc: func(w *SetupWizard) {
			fmt.Println("  [InstallState] INITIAL_SECURITY_SETUP 시작")
			w.Init(true)
			w.ProceedToNextState()
		},
	}

	StateInitialPluginsInstalling = &InstallState{
		Name:            "INITIAL_PLUGINS_INSTALLING",
		IsSetupComplete: false,
	}

	StateCreateAdminUser = &InstallState{
		Name:            "CREATE_ADMIN_USER",
		IsSetupComplete: false,
		InitFunc: func(w *SetupWizard) {
			// 보안 기본값 사용 중이 아니면 건너뛰기
			if !w.isUsingSecurityDefaults() {
				fmt.Println("  [InstallState] CREATE_ADMIN_USER 건너뜀 (보안 기본값 아님)")
				w.ProceedToNextState()
			}
		},
	}

	StateConfigureInstance = &InstallState{
		Name:            "CONFIGURE_INSTANCE",
		IsSetupComplete: false,
		InitFunc: func(w *SetupWizard) {
			// 이미 URL이 설정되어 있으면 건너뛰기
			if w.RootURL != "" {
				fmt.Println("  [InstallState] CONFIGURE_INSTANCE 건너뜀 (URL 이미 설정)")
				w.ProceedToNextState()
			}
		},
	}

	StateInitialSetupCompleted = &InstallState{
		Name:            "INITIAL_SETUP_COMPLETED",
		IsSetupComplete: true,
		InitFunc: func(w *SetupWizard) {
			fmt.Println("  [InstallState] INITIAL_SETUP_COMPLETED → 설정 완료 처리")
			w.CompleteSetup()
			w.SetInstallState(StateRunning)
		},
	}

	StateRunning = &InstallState{
		Name:            "RUNNING",
		IsSetupComplete: true,
	}

	StateRestart = &InstallState{
		Name:            "RESTART",
		IsSetupComplete: true,
		InitFunc: func(w *SetupWizard) {
			fmt.Println("  [InstallState] RESTART → 버전 저장")
			w.saveLastExecVersion()
		},
	}

	StateUpgrade = &InstallState{
		Name:            "UPGRADE",
		IsSetupComplete: true,
		InitFunc: func(w *SetupWizard) {
			fmt.Println("  [InstallState] UPGRADE → 업데이트 사이트 갱신 + 버전 저장")
			w.saveLastExecVersion()
		},
	}
)

// 상태 전이 맵: 현재 상태 → 다음 상태
// init() 함수에서 초기화 — Go의 초기화 순환 참조 방지
// (InstallState.InitFunc → ProceedToNextState → stateTransitions → InstallState)
var stateTransitions map[string]*InstallState

func init() {
	stateTransitions = map[string]*InstallState{
		"UNKNOWN":                    StateNew,
		"NEW":                        StateInitialSecuritySetup,
		"INITIAL_SECURITY_SETUP":     StateInitialPluginsInstalling,
		"INITIAL_PLUGINS_INSTALLING": StateCreateAdminUser,
		"CREATE_ADMIN_USER":          StateConfigureInstance,
		"CONFIGURE_INSTANCE":         StateInitialSetupCompleted,
		"INITIAL_SETUP_COMPLETED":    StateRunning,
	}
}

// =============================================================================
// 2. SecurityRealm — 보안 영역
// =============================================================================

// User는 Jenkins 사용자
type User struct {
	Username string
	Password string // SHA-256 해시
	FullName string
	Email    string
}

// SecurityRealm은 인증 시스템
// Jenkins 원본: hudson/security/HudsonPrivateSecurityRealm.java
type SecurityRealm struct {
	Users                []*User
	AllowSignup          bool
	AllowAnonymousRead   bool
}

func (r *SecurityRealm) CreateAccount(username, password string) *User {
	hashedPassword := hashPassword(password)
	user := &User{
		Username: username,
		Password: hashedPassword,
	}
	r.Users = append(r.Users, user)
	return user
}

func (r *SecurityRealm) Authenticate(username, password string) bool {
	hashedPassword := hashPassword(password)
	for _, u := range r.Users {
		if u.Username == username && u.Password == hashedPassword {
			return true
		}
	}
	return false
}

func hashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

// =============================================================================
// 3. SetupWizard — 설정 위자드
// =============================================================================

// SetupWizard는 최초 설치 위자드
// Jenkins 원본: jenkins/install/SetupWizard.java
type SetupWizard struct {
	CurrentState    *InstallState
	SecurityRealm   *SecurityRealm
	JenkinsHome     string
	JenkinsVersion  string
	LastExecVersion string
	RootURL         string
	InitialPassword string
	CsrfEnabled     bool
	FilterActive    bool
	InstalledPlugins []string
}

func NewSetupWizard(jenkinsHome, version string) *SetupWizard {
	return &SetupWizard{
		CurrentState:   StateUnknown,
		JenkinsHome:    jenkinsHome,
		JenkinsVersion: version,
	}
}

// Init은 초기 보안 설정을 수행한다
// Jenkins 원본: SetupWizard.init()
func (w *SetupWizard) Init(newInstall bool) {
	if !newInstall {
		return
	}

	fmt.Println("  [SetupWizard] 초기 보안 설정 시작...")

	// 1. SecurityRealm 설정
	w.SecurityRealm = &SecurityRealm{
		AllowSignup:        false,
		AllowAnonymousRead: false,
	}

	// 2. 랜덤 비밀번호 생성 (UUID 기반)
	w.InitialPassword = generateRandomPassword()

	// 3. admin 계정 생성
	w.SecurityRealm.CreateAccount("admin", w.InitialPassword)
	fmt.Println("  [SetupWizard] admin 계정 생성 완료")

	// 4. 비밀번호를 파일에 저장
	passwordFile := filepath.Join(w.JenkinsHome, "secrets", "initialAdminPassword")
	os.MkdirAll(filepath.Dir(passwordFile), 0750)
	os.WriteFile(passwordFile, []byte(w.InitialPassword+"\n"), 0640)
	fmt.Printf("  [SetupWizard] 초기 비밀번호 저장: %s\n", passwordFile)

	// 5. CSRF 보호 활성화
	w.CsrfEnabled = true
	fmt.Println("  [SetupWizard] CSRF 보호 활성화")

	// 6. 콘솔에 비밀번호 출력
	fmt.Println()
	fmt.Println("  *************************************************************")
	fmt.Println("  *************************************************************")
	fmt.Println()
	fmt.Println("  Jenkins initial setup is required.")
	fmt.Println("  An admin user has been created and a password generated.")
	fmt.Println("  Please use the following password to proceed to installation:")
	fmt.Println()
	fmt.Printf("  %s\n", w.InitialPassword)
	fmt.Println()
	fmt.Printf("  This may also be found at: %s\n", passwordFile)
	fmt.Println()
	fmt.Println("  *************************************************************")
	fmt.Println("  *************************************************************")
	fmt.Println()
}

// SetInstallState는 설치 상태를 변경한다
func (w *SetupWizard) SetInstallState(state *InstallState) {
	w.CurrentState = state
	fmt.Printf("  [SetupWizard] 상태 변경: → %s (setupComplete=%v)\n",
		state.Name, state.IsSetupComplete)

	// 필터 관리
	if !state.IsSetupComplete {
		w.setUpFilter()
	} else {
		w.tearDownFilter()
	}

	// 상태 초기화 로직 실행
	if state.InitFunc != nil {
		state.InitFunc(w)
	}
}

// ProceedToNextState는 다음 상태로 진행한다
func (w *SetupWizard) ProceedToNextState() {
	next, ok := stateTransitions[w.CurrentState.Name]
	if ok {
		w.SetInstallState(next)
	}
}

// CompleteSetup은 설정을 완료한다
func (w *SetupWizard) CompleteSetup() {
	w.saveLastExecVersion()
	fmt.Println("  [SetupWizard] 설정 완료!")
}

func (w *SetupWizard) setUpFilter() {
	if !w.FilterActive {
		w.FilterActive = true
		fmt.Println("  [SetupWizard] FORCE_SETUP_WIZARD_FILTER 활성화")
	}
}

func (w *SetupWizard) tearDownFilter() {
	if w.FilterActive {
		w.FilterActive = false
		fmt.Println("  [SetupWizard] FORCE_SETUP_WIZARD_FILTER 비활성화")
	}
}

func (w *SetupWizard) isNewInstall() bool {
	return w.LastExecVersion == ""
}

func (w *SetupWizard) isUpgrade() bool {
	return w.LastExecVersion != "" && w.LastExecVersion != w.JenkinsVersion
}

func (w *SetupWizard) isUsingSecurityDefaults() bool {
	if w.SecurityRealm == nil {
		return false
	}
	// 단순화: admin 하나 + 초기 비밀번호 파일 존재 시 기본값 사용 중
	return len(w.SecurityRealm.Users) == 1 &&
		w.SecurityRealm.Users[0].Username == "admin" &&
		w.InitialPassword != ""
}

func (w *SetupWizard) saveLastExecVersion() {
	w.LastExecVersion = w.JenkinsVersion
	stateFile := filepath.Join(w.JenkinsHome, "jenkins.install.UpgradeWizard.state")
	os.WriteFile(stateFile, []byte(w.JenkinsVersion), 0644)
}

// CreateAdminUser는 관리자 계정 생성 API를 시뮬레이션
// Jenkins 원본: SetupWizard.doCreateAdminUser()
func (w *SetupWizard) CreateAdminUser(username, password, fullName, email string) error {
	fmt.Printf("  [SetupWizard] 관리자 계정 생성: %s (%s)\n", username, fullName)

	// 기존 admin 삭제
	w.SecurityRealm.Users = nil

	// 새 사용자 생성
	user := w.SecurityRealm.CreateAccount(username, password)
	user.FullName = fullName
	user.Email = email

	// 초기 비밀번호 파일 삭제
	passwordFile := filepath.Join(w.JenkinsHome, "secrets", "initialAdminPassword")
	os.Remove(passwordFile)
	w.InitialPassword = ""

	fmt.Println("  [SetupWizard] 초기 비밀번호 파일 삭제 완료")

	// 다음 상태로
	w.ProceedToNextState()
	return nil
}

// ConfigureInstance는 인스턴스 URL 설정을 시뮬레이션
// Jenkins 원본: SetupWizard.doConfigureInstance()
func (w *SetupWizard) ConfigureInstance(rootURL string) error {
	if rootURL == "" {
		return fmt.Errorf("URL이 비어있습니다")
	}
	if !strings.HasPrefix(rootURL, "http://") && !strings.HasPrefix(rootURL, "https://") {
		return fmt.Errorf("유효하지 않은 URL: %s", rootURL)
	}

	w.RootURL = rootURL
	fmt.Printf("  [SetupWizard] Jenkins URL 설정: %s\n", rootURL)

	w.ProceedToNextState()
	return nil
}

// InstallPlugins는 추천 플러그인 설치를 시뮬레이션
func (w *SetupWizard) InstallPlugins(plugins []string) {
	fmt.Println("  [SetupWizard] 플러그인 설치 시작...")
	for i, plugin := range plugins {
		time.Sleep(50 * time.Millisecond) // 설치 시뮬레이션
		w.InstalledPlugins = append(w.InstalledPlugins, plugin)
		status := "완료"
		fmt.Printf("    [%d/%d] %s ... %s\n", i+1, len(plugins), plugin, status)
	}
	fmt.Println("  [SetupWizard] 플러그인 설치 완료")
	w.ProceedToNextState()
}

// SimulateHTTPRequest는 HTTP 요청 필터링을 시뮬레이션
func (w *SetupWizard) SimulateHTTPRequest(path string) string {
	if w.FilterActive && path == "/" {
		return "/setupWizard/" // 위자드로 리다이렉트
	}
	return path // 정상 처리
}

// =============================================================================
// 유틸리티
// =============================================================================

func generateRandomPassword() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// =============================================================================
// 메인 데모
// =============================================================================

func main() {
	fmt.Println("=== Jenkins Setup Wizard 시스템 시뮬레이션 ===")
	fmt.Println()

	// 임시 Jenkins 홈 디렉토리 생성
	jenkinsHome, _ := os.MkdirTemp("", "jenkins-poc-")
	defer os.RemoveAll(jenkinsHome)

	// ─────────────────────────────────────────────────
	// 시나리오 1: 신규 설치
	// ─────────────────────────────────────────────────
	fmt.Println("=== 시나리오 1: 신규 설치 ===")
	fmt.Println()

	wizard := NewSetupWizard(jenkinsHome, "2.462.1")

	// Step 0: 상태 판별 (UNKNOWN → NEW)
	fmt.Println("--- Step 0: 설치 유형 판별 ---")
	wizard.SetInstallState(StateUnknown)
	fmt.Println()

	// HTTP 요청 필터링 테스트
	fmt.Println("--- HTTP 요청 필터링 테스트 ---")
	redirect := wizard.SimulateHTTPRequest("/")
	fmt.Printf("  GET / → %s (필터 활성=%v)\n", redirect, wizard.FilterActive)
	redirect = wizard.SimulateHTTPRequest("/api/json")
	fmt.Printf("  GET /api/json → %s (정상 통과)\n", redirect)
	fmt.Println()

	// Step 1: 초기 보안 설정
	fmt.Println("--- Step 1: 초기 보안 설정 (INITIAL_SECURITY_SETUP) ---")
	wizard.SetInstallState(StateInitialSecuritySetup)
	fmt.Println()

	// Step 2: 추천 플러그인 설치
	fmt.Println("--- Step 2: 플러그인 설치 (INITIAL_PLUGINS_INSTALLING) ---")
	suggestedPlugins := []string{
		"git", "pipeline", "docker-workflow",
		"blueocean", "credentials", "ssh-slaves",
	}
	wizard.InstallPlugins(suggestedPlugins)
	fmt.Println()

	// Step 3: 관리자 계정 생성
	fmt.Println("--- Step 3: 관리자 계정 생성 (CREATE_ADMIN_USER) ---")
	wizard.CreateAdminUser("myuser", "MyStr0ngP@ss!", "My User", "myuser@example.com")
	fmt.Println()

	// Step 4: 인스턴스 URL 설정
	fmt.Println("--- Step 4: 인스턴스 URL 설정 (CONFIGURE_INSTANCE) ---")
	wizard.ConfigureInstance("http://jenkins.example.com:8080/")
	fmt.Println()

	// 최종 상태 확인
	fmt.Println("--- 최종 상태 ---")
	fmt.Printf("  현재 상태: %s\n", wizard.CurrentState.Name)
	fmt.Printf("  Setup Complete: %v\n", wizard.CurrentState.IsSetupComplete)
	fmt.Printf("  필터 활성: %v\n", wizard.FilterActive)
	fmt.Printf("  Jenkins URL: %s\n", wizard.RootURL)
	fmt.Printf("  CSRF 보호: %v\n", wizard.CsrfEnabled)
	fmt.Printf("  설치된 플러그인: %d개\n", len(wizard.InstalledPlugins))
	fmt.Printf("  저장된 버전: %s\n", wizard.LastExecVersion)

	// 필터 비활성화 확인
	redirect = wizard.SimulateHTTPRequest("/")
	fmt.Printf("  GET / → %s (필터 비활성화 확인)\n", redirect)
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 시나리오 2: 기존 설치 재시작
	// ─────────────────────────────────────────────────
	fmt.Println("=== 시나리오 2: 기존 설치 재시작 ===")
	fmt.Println()

	restartWizard := NewSetupWizard(jenkinsHome, "2.462.1")
	restartWizard.LastExecVersion = "2.462.1" // 같은 버전
	restartWizard.SetInstallState(StateUnknown)
	fmt.Printf("  최종 상태: %s\n", restartWizard.CurrentState.Name)
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 시나리오 3: 업그레이드
	// ─────────────────────────────────────────────────
	fmt.Println("=== 시나리오 3: 업그레이드 (2.462.1 → 2.463.0) ===")
	fmt.Println()

	upgradeWizard := NewSetupWizard(jenkinsHome, "2.463.0")
	upgradeWizard.LastExecVersion = "2.462.1" // 이전 버전
	upgradeWizard.SetInstallState(StateUnknown)
	fmt.Printf("  최종 상태: %s\n", upgradeWizard.CurrentState.Name)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
