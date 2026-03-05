package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ============================================================
// Grafana 피처 토글 시뮬레이션
// 기능 플래그의 등록, 평가, 의존성, 컨텍스트 오버라이드 구현
// ============================================================

// --- 상수 및 타입 ---

// Stage는 기능의 성숙도 단계를 나타낸다.
type Stage string

const (
	StageAlpha      Stage = "alpha"
	StageBeta       Stage = "beta"
	StageGA         Stage = "GA"
	StageDeprecated Stage = "deprecated"
)

// 컨텍스트 키 타입
type ctxKey string

const (
	ctxKeyOrgID  ctxKey = "orgID"
	ctxKeyUserID ctxKey = "userID"
)

// --- 데이터 구조 ---

// FeatureFlag는 하나의 기능 플래그 정의를 나타낸다.
// 실제 구현: pkg/services/featuremgmt/registry.go
type FeatureFlag struct {
	Name            string   // 기능 이름 (고유 식별자)
	Description     string   // 기능 설명
	Stage           Stage    // 성숙도 단계
	RequiresDevMode bool     // 개발 모드에서만 사용 가능
	Dependencies    []string // 의존하는 다른 기능 목록
	DefaultEnabled  bool     // 기본 활성화 여부 (GA는 기본 true)
}

// Override는 특정 범위(조직/사용자)에 대한 기능 오버라이드를 나타낸다.
type Override struct {
	OrgID   int64  // 0이면 전체 적용
	UserID  int64  // 0이면 전체 적용
	Flag    string
	Enabled bool
}

// --- FeatureManager ---

// FeatureManager는 Grafana의 FeatureManager를 시뮬레이션한다.
// 실제 구현: pkg/services/featuremgmt/manager.go
type FeatureManager struct {
	mu           sync.RWMutex
	flags        map[string]*FeatureFlag   // 등록된 플래그 정의
	enabled      map[string]bool           // 전역 활성화 상태
	startup      map[string]bool           // 시작 시 설정된 상태 (불변)
	overrides    []Override                // 컨텍스트 오버라이드
	warnings     map[string][]string       // 플래그별 경고
	devMode      bool                      // 개발 모드 여부
	changeLog    []string                  // 변경 이력
}

func NewFeatureManager(devMode bool) *FeatureManager {
	return &FeatureManager{
		flags:     make(map[string]*FeatureFlag),
		enabled:   make(map[string]bool),
		startup:   make(map[string]bool),
		warnings:  make(map[string][]string),
		devMode:   devMode,
		changeLog: make([]string, 0),
	}
}

// RegisterFlag는 새 기능 플래그를 등록한다.
func (m *FeatureManager) RegisterFlag(flag FeatureFlag) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// GA는 기본 활성화
	if flag.Stage == StageGA && !flag.DefaultEnabled {
		flag.DefaultEnabled = true
	}

	// alpha는 dev mode 필수
	if flag.Stage == StageAlpha {
		flag.RequiresDevMode = true
	}

	m.flags[flag.Name] = &flag

	// 기본값으로 초기 상태 설정
	m.enabled[flag.Name] = flag.DefaultEnabled
	m.startup[flag.Name] = flag.DefaultEnabled
}

// SetEnabled는 런타임에 기능을 활성화/비활성화한다.
func (m *FeatureManager) SetEnabled(flagName string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	flag, exists := m.flags[flagName]
	if !exists {
		return fmt.Errorf("알 수 없는 기능 플래그: %s", flagName)
	}

	// dev mode 제약 확인
	if enabled && flag.RequiresDevMode && !m.devMode {
		m.addWarning(flagName, "alpha 기능은 개발 모드에서만 활성화 가능")
		return fmt.Errorf("기능 '%s'는 개발 모드에서만 활성화 가능 (stage=%s)", flagName, flag.Stage)
	}

	// deprecated 경고
	if enabled && flag.Stage == StageDeprecated {
		m.addWarning(flagName, "폐기 예정 기능이 활성화됨")
	}

	prev := m.enabled[flagName]
	m.enabled[flagName] = enabled

	action := "비활성화"
	if enabled {
		action = "활성화"
	}
	m.changeLog = append(m.changeLog, fmt.Sprintf("[%s] %s → %s (이전: %v)", flagName, action, fmt.Sprint(enabled), prev))

	return nil
}

// AddOverride는 특정 조직/사용자에 대한 오버라이드를 추가한다.
func (m *FeatureManager) AddOverride(override Override) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overrides = append(m.overrides, override)
}

// IsEnabled는 주어진 컨텍스트에서 기능이 활성화되었는지 확인한다.
// 평가 순서: 컨텍스트 오버라이드 → 전역 설정 → 기본값
func (m *FeatureManager) IsEnabled(ctx context.Context, flagName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	flag, exists := m.flags[flagName]
	if !exists {
		return false
	}

	// 1단계: 기본 전역 상태 확인
	result := m.enabled[flagName]

	// 2단계: 단계 제약 확인
	if result && flag.RequiresDevMode && !m.devMode {
		return false
	}

	// 3단계: 컨텍스트 오버라이드 확인
	orgID, _ := ctx.Value(ctxKeyOrgID).(int64)
	userID, _ := ctx.Value(ctxKeyUserID).(int64)

	for _, ov := range m.overrides {
		if ov.Flag != flagName {
			continue
		}
		// 사용자 오버라이드 (가장 높은 우선순위)
		if ov.UserID != 0 && ov.UserID == userID {
			result = ov.Enabled
			break
		}
		// 조직 오버라이드
		if ov.OrgID != 0 && ov.OrgID == orgID && ov.UserID == 0 {
			result = ov.Enabled
		}
	}

	// 4단계: 의존성 확인
	if result && len(flag.Dependencies) > 0 {
		for _, dep := range flag.Dependencies {
			if !m.enabled[dep] {
				return false
			}
		}
	}

	return result
}

// IsEnabledGlobally는 컨텍스트 없이 전역 상태만 확인한다.
func (m *FeatureManager) IsEnabledGlobally(flagName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	flag, exists := m.flags[flagName]
	if !exists {
		return false
	}

	result := m.enabled[flagName]

	// dev mode 제약
	if result && flag.RequiresDevMode && !m.devMode {
		return false
	}

	// 의존성 확인
	if result && len(flag.Dependencies) > 0 {
		for _, dep := range flag.Dependencies {
			if !m.enabled[dep] {
				return false
			}
		}
	}

	return result
}

// GetEnabled는 현재 활성화된 모든 기능 목록을 반환한다.
func (m *FeatureManager) GetEnabled(ctx context.Context) map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]bool)
	for name := range m.flags {
		// RLock 내에서 IsEnabled를 직접 호출하면 데드락 → 로직 인라인
		result[name] = m.isEnabledInternal(ctx, name)
	}
	return result
}

func (m *FeatureManager) isEnabledInternal(ctx context.Context, flagName string) bool {
	flag, exists := m.flags[flagName]
	if !exists {
		return false
	}

	result := m.enabled[flagName]

	if result && flag.RequiresDevMode && !m.devMode {
		return false
	}

	orgID, _ := ctx.Value(ctxKeyOrgID).(int64)
	userID, _ := ctx.Value(ctxKeyUserID).(int64)

	for _, ov := range m.overrides {
		if ov.Flag != flagName {
			continue
		}
		if ov.UserID != 0 && ov.UserID == userID {
			result = ov.Enabled
			break
		}
		if ov.OrgID != 0 && ov.OrgID == orgID && ov.UserID == 0 {
			result = ov.Enabled
		}
	}

	if result && len(flag.Dependencies) > 0 {
		for _, dep := range flag.Dependencies {
			if !m.enabled[dep] {
				return false
			}
		}
	}

	return result
}

// GetWarnings는 특정 플래그의 경고를 반환한다.
func (m *FeatureManager) GetWarnings(flagName string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.warnings[flagName]
}

func (m *FeatureManager) addWarning(flagName, msg string) {
	m.warnings[flagName] = append(m.warnings[flagName], msg)
}

// --- 출력 헬퍼 ---

func boolToStatus(b bool) string {
	if b {
		return "ON "
	}
	return "OFF"
}

func stageStr(s Stage) string {
	switch s {
	case StageAlpha:
		return "alpha"
	case StageBeta:
		return "beta "
	case StageGA:
		return "GA   "
	case StageDeprecated:
		return "depr "
	default:
		return string(s)
	}
}

func (m *FeatureManager) PrintFlagTable(ctx context.Context, title string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 이름순 정렬
	names := make([]string, 0, len(m.flags))
	for name := range m.flags {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Printf("\n%s\n", title)
	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("%-22s %-6s %-8s %-6s %-8s %s\n",
		"기능", "단계", "기본", "현재", "DevMode", "의존성")
	fmt.Println(strings.Repeat("-", 80))

	for _, name := range names {
		flag := m.flags[name]
		enabled := m.isEnabledInternal(ctx, name)
		deps := "-"
		if len(flag.Dependencies) > 0 {
			deps = strings.Join(flag.Dependencies, ", ")
		}
		devReq := "  -  "
		if flag.RequiresDevMode {
			devReq = " yes "
		}
		fmt.Printf("%-22s %-6s %-8v %-6s %-8s %s\n",
			name, stageStr(flag.Stage), flag.DefaultEnabled,
			boolToStatus(enabled), devReq, deps)
	}
	fmt.Println(strings.Repeat("-", 80))
}

// --- 메인: 시뮬레이션 ---

func main() {
	fmt.Println("=== Grafana 피처 토글 시뮬레이션 ===")

	// ------------------------------------------
	// 1. FeatureManager 생성 (프로덕션 모드)
	// ------------------------------------------
	fmt.Println("\n--- 1. 기능 플래그 등록 (프로덕션 모드) ---")

	mgr := NewFeatureManager(false) // devMode = false

	// 기능 플래그 등록
	flags := []FeatureFlag{
		{Name: "newNavigation", Description: "새로운 네비게이션 UI", Stage: StageGA},
		{Name: "dashboardScene", Description: "씬 기반 대시보드", Stage: StageBeta, Dependencies: []string{"newNavigation"}},
		{Name: "exploreMetrics", Description: "메트릭 탐색 신규 UI", Stage: StageAlpha},
		{Name: "correlations", Description: "데이터 소스 간 상관관계", Stage: StageGA},
		{Name: "nestedFolders", Description: "중첩 폴더 구조", Stage: StageBeta},
		{Name: "publicDashboards", Description: "대시보드 공개 공유", Stage: StageGA},
		{Name: "oldAlerts", Description: "레거시 알림 시스템", Stage: StageDeprecated},
	}

	for _, f := range flags {
		mgr.RegisterFlag(f)
		fmt.Printf("  등록: %-22s (stage=%s, default=%v)\n",
			f.Name, f.Stage, f.Stage == StageGA)
	}

	ctx := context.Background()
	mgr.PrintFlagTable(ctx, "[전역 상태 - 프로덕션 모드]")

	// ------------------------------------------
	// 2. 런타임 토글
	// ------------------------------------------
	fmt.Println("\n--- 2. 런타임 토글 ---")

	// beta 기능 활성화
	fmt.Println("\n[토글] nestedFolders → ON")
	if err := mgr.SetEnabled("nestedFolders", true); err != nil {
		fmt.Printf("  실패: %v\n", err)
	} else {
		fmt.Printf("  결과: %s\n", boolToStatus(mgr.IsEnabledGlobally("nestedFolders")))
	}

	// alpha 기능 활성화 시도 (프로덕션 모드 → 실패)
	fmt.Println("\n[토글] exploreMetrics → ON (alpha, 프로덕션 모드)")
	if err := mgr.SetEnabled("exploreMetrics", true); err != nil {
		fmt.Printf("  실패: %v\n", err)
		warnings := mgr.GetWarnings("exploreMetrics")
		for _, w := range warnings {
			fmt.Printf("  경고: %s\n", w)
		}
	}

	// deprecated 기능 활성화 시도
	fmt.Println("\n[토글] oldAlerts → ON (deprecated)")
	if err := mgr.SetEnabled("oldAlerts", true); err != nil {
		fmt.Printf("  실패: %v\n", err)
	} else {
		fmt.Printf("  결과: %s\n", boolToStatus(mgr.IsEnabledGlobally("oldAlerts")))
		warnings := mgr.GetWarnings("oldAlerts")
		for _, w := range warnings {
			fmt.Printf("  경고: %s\n", w)
		}
	}

	// 의존성 테스트: dashboardScene은 newNavigation에 의존
	fmt.Println("\n[토글] dashboardScene → ON (의존성: newNavigation)")
	if err := mgr.SetEnabled("dashboardScene", true); err != nil {
		fmt.Printf("  실패: %v\n", err)
	} else {
		fmt.Printf("  결과: %s (newNavigation=%s)\n",
			boolToStatus(mgr.IsEnabledGlobally("dashboardScene")),
			boolToStatus(mgr.IsEnabledGlobally("newNavigation")))
	}

	// newNavigation 비활성화 → dashboardScene도 비활성화
	fmt.Println("\n[토글] newNavigation → OFF (dashboardScene 의존성 영향)")
	mgr.SetEnabled("newNavigation", false)
	fmt.Printf("  newNavigation: %s\n", boolToStatus(mgr.IsEnabledGlobally("newNavigation")))
	fmt.Printf("  dashboardScene: %s (의존성 미충족으로 비활성화)\n", boolToStatus(mgr.IsEnabledGlobally("dashboardScene")))

	// 복구
	mgr.SetEnabled("newNavigation", true)

	mgr.PrintFlagTable(ctx, "[전역 상태 - 런타임 토글 후]")

	// ------------------------------------------
	// 3. 컨텍스트 오버라이드 (조직/사용자별)
	// ------------------------------------------
	fmt.Println("\n--- 3. 컨텍스트 오버라이드 ---")

	// 조직 1: nestedFolders 비활성화
	mgr.AddOverride(Override{OrgID: 1, Flag: "nestedFolders", Enabled: false})
	fmt.Println("[오버라이드] 조직 1: nestedFolders → OFF")

	// 조직 2: publicDashboards 비활성화
	mgr.AddOverride(Override{OrgID: 2, Flag: "publicDashboards", Enabled: false})
	fmt.Println("[오버라이드] 조직 2: publicDashboards → OFF")

	// 사용자 42: nestedFolders 강제 활성화 (조직 오버라이드보다 우선)
	mgr.AddOverride(Override{OrgID: 1, UserID: 42, Flag: "nestedFolders", Enabled: true})
	fmt.Println("[오버라이드] 사용자 42 (조직 1): nestedFolders → ON (사용자 우선)")

	// 다양한 컨텍스트에서 평가
	fmt.Println()
	contexts := []struct {
		label  string
		orgID  int64
		userID int64
	}{
		{"전역 (컨텍스트 없음)", 0, 0},
		{"조직 1, 일반 사용자", 1, 100},
		{"조직 1, 사용자 42", 1, 42},
		{"조직 2, 일반 사용자", 2, 200},
	}

	testFlags := []string{"nestedFolders", "publicDashboards", "dashboardScene"}

	fmt.Println(strings.Repeat("-", 70))
	fmt.Printf("%-28s", "컨텍스트")
	for _, f := range testFlags {
		fmt.Printf("%-16s", f)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("-", 70))

	for _, c := range contexts {
		evalCtx := context.Background()
		if c.orgID != 0 {
			evalCtx = context.WithValue(evalCtx, ctxKeyOrgID, c.orgID)
		}
		if c.userID != 0 {
			evalCtx = context.WithValue(evalCtx, ctxKeyUserID, c.userID)
		}

		fmt.Printf("%-28s", c.label)
		for _, f := range testFlags {
			fmt.Printf("%-16s", boolToStatus(mgr.IsEnabled(evalCtx, f)))
		}
		fmt.Println()
	}
	fmt.Println(strings.Repeat("-", 70))

	// ------------------------------------------
	// 4. 개발 모드에서 alpha 기능 활성화
	// ------------------------------------------
	fmt.Println("\n--- 4. 개발 모드: alpha 기능 활성화 ---")

	devMgr := NewFeatureManager(true) // devMode = true
	for _, f := range flags {
		devMgr.RegisterFlag(f)
	}

	fmt.Println("\n[토글] exploreMetrics → ON (alpha, 개발 모드)")
	if err := devMgr.SetEnabled("exploreMetrics", true); err != nil {
		fmt.Printf("  실패: %v\n", err)
	} else {
		fmt.Printf("  결과: %s (개발 모드에서 alpha 기능 활성화 성공)\n",
			boolToStatus(devMgr.IsEnabledGlobally("exploreMetrics")))
	}

	devMgr.PrintFlagTable(context.Background(), "[개발 모드 상태]")

	// ------------------------------------------
	// 5. GetEnabled — 활성 기능 목록 조회
	// ------------------------------------------
	fmt.Println("\n--- 5. 활성 기능 목록 조회 ---")

	orgCtx := context.WithValue(context.Background(), ctxKeyOrgID, int64(1))
	enabledFlags := mgr.GetEnabled(orgCtx)

	fmt.Println("\n조직 1 컨텍스트에서 활성화된 기능:")
	for name, enabled := range enabledFlags {
		if enabled {
			fmt.Printf("  [ON]  %s\n", name)
		}
	}
	fmt.Println("\n조직 1 컨텍스트에서 비활성화된 기능:")
	for name, enabled := range enabledFlags {
		if !enabled {
			fmt.Printf("  [OFF] %s\n", name)
		}
	}

	// ------------------------------------------
	// 요약
	// ------------------------------------------
	fmt.Println("\n--- 시뮬레이션 요약 ---")
	fmt.Println()
	fmt.Println("피처 토글 시스템 구성요소:")
	fmt.Println("  1. FeatureFlag: 기능 메타데이터 (이름, 단계, 의존성)")
	fmt.Println("  2. FeatureManager: 전역/컨텍스트별 기능 상태 관리")
	fmt.Println("  3. Stage: alpha → beta → GA → deprecated 수명주기")
	fmt.Println("  4. Override: 조직/사용자별 기능 활성화 제어")
	fmt.Println("  5. Dependency: 기능 간 의존성 체인 관리")
	fmt.Println()
	fmt.Println("평가 우선순위: 사용자 오버라이드 > 조직 오버라이드 > 전역 설정 > 기본값")
	fmt.Println("보안 제약: alpha 기능은 개발 모드에서만 활성화 가능")
}
