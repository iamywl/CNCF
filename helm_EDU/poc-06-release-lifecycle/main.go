// Helm v4 Release 라이프사이클 PoC: Install→Upgrade→Rollback→Uninstall
//
// 이 PoC는 Helm v4의 릴리스 상태 전이와 리비전 관리를 시뮬레이션합니다:
//   1. Install - 새 릴리스 생성 (pending-install → deployed/failed)
//   2. Upgrade - 리비전 증가 (pending-upgrade → deployed/failed, 이전=superseded)
//   3. Rollback - 이전 리비전으로 복원 (pending-rollback → deployed)
//   4. Uninstall - 릴리스 삭제 (uninstalling → uninstalled)
//   5. 리비전 히스토리 관리
//
// 참조: pkg/action/install.go, upgrade.go, rollback.go, uninstall.go
//       pkg/release/v1/release.go, pkg/release/common/status.go
//
// 실행: go run main.go

package main

import (
	"fmt"
	"sort"
	"time"
)

// =============================================================================
// 상태 정의: pkg/release/common/status.go
// =============================================================================

type Status string

const (
	StatusUnknown         Status = "unknown"
	StatusDeployed        Status = "deployed"
	StatusUninstalled     Status = "uninstalled"
	StatusSuperseded      Status = "superseded"
	StatusFailed          Status = "failed"
	StatusPendingInstall  Status = "pending-install"
	StatusPendingUpgrade  Status = "pending-upgrade"
	StatusPendingRollback Status = "pending-rollback"
	StatusUninstalling    Status = "uninstalling"
)

func (s Status) IsPending() bool {
	return s == StatusPendingInstall || s == StatusPendingUpgrade || s == StatusPendingRollback
}

// =============================================================================
// Release 데이터 모델
// =============================================================================

type Info struct {
	FirstDeployed time.Time
	LastDeployed  time.Time
	Deleted       time.Time
	Status        Status
	Description   string
	Notes         string
}

type Release struct {
	Name      string
	Info      *Info
	Chart     string
	Config    map[string]any
	Manifest  string
	Version   int
	Namespace string
}

func (r *Release) SetStatus(status Status, msg string) {
	r.Info.Status = status
	r.Info.Description = msg
}

// =============================================================================
// 인메모리 스토리지
// =============================================================================

type Storage struct {
	releases   []*Release
	maxHistory int
}

func NewStorage() *Storage {
	return &Storage{maxHistory: 10}
}

func (s *Storage) Create(rls *Release) error {
	s.releases = append(s.releases, rls)
	return nil
}

func (s *Storage) Update(rls *Release) error {
	for i, r := range s.releases {
		if r.Name == rls.Name && r.Version == rls.Version {
			s.releases[i] = rls
			return nil
		}
	}
	return fmt.Errorf("릴리스 %s v%d 를 찾을 수 없습니다", rls.Name, rls.Version)
}

func (s *Storage) Get(name string, version int) (*Release, error) {
	for _, r := range s.releases {
		if r.Name == name && r.Version == version {
			return r, nil
		}
	}
	return nil, fmt.Errorf("릴리스 %s v%d 를 찾을 수 없습니다", name, version)
}

func (s *Storage) History(name string) []*Release {
	var result []*Release
	for _, r := range s.releases {
		if r.Name == name {
			result = append(result, r)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Version < result[j].Version
	})
	return result
}

func (s *Storage) Last(name string) *Release {
	history := s.History(name)
	if len(history) == 0 {
		return nil
	}
	return history[len(history)-1]
}

func (s *Storage) Deployed(name string) *Release {
	history := s.History(name)
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Info.Status == StatusDeployed {
			return history[i]
		}
	}
	return nil
}

func (s *Storage) Delete(name string, version int) {
	for i, r := range s.releases {
		if r.Name == name && r.Version == version {
			s.releases = append(s.releases[:i], s.releases[i+1:]...)
			return
		}
	}
}

// =============================================================================
// Action 구현: Install, Upgrade, Rollback, Uninstall
// 각 Action은 실제 Helm의 pkg/action/*.go 패턴을 따른다.
// =============================================================================

// Install은 새 릴리스를 생성한다.
// 실제 Helm: action.Install.RunWithContext()
// 흐름: pending-install → 리소스 생성 → deployed (실패 시 failed)
type Install struct {
	store      *Storage
	Name       string
	Chart      string
	Namespace  string
	Config     map[string]any
	DryRun     bool
	SimFailure bool // 시뮬레이션: 설치 실패
}

func (i *Install) Run() (*Release, error) {
	if i.Name == "" {
		return nil, fmt.Errorf("릴리스 이름이 필요합니다")
	}

	// 이미 배포된 릴리스 확인
	existing := i.store.Deployed(i.Name)
	if existing != nil {
		return nil, fmt.Errorf("릴리스 %q 가 이미 배포되어 있습니다", i.Name)
	}

	now := time.Now()

	// 1) pending-install 상태로 릴리스 생성
	rel := &Release{
		Name:      i.Name,
		Chart:     i.Chart,
		Namespace: i.Namespace,
		Config:    i.Config,
		Version:   1,
		Manifest:  fmt.Sprintf("# Rendered manifest for %s", i.Chart),
		Info: &Info{
			FirstDeployed: now,
			LastDeployed:  now,
			Status:        StatusPendingInstall,
			Description:   "Install started",
		},
	}

	fmt.Printf("    [Install] %s: pending-install (v%d)\n", rel.Name, rel.Version)

	if err := i.store.Create(rel); err != nil {
		return nil, err
	}

	// 2) 리소스 생성 시뮬레이션
	if i.SimFailure {
		// 실패 시: failed 상태로 전이
		rel.SetStatus(StatusFailed, "Install failed: simulated error")
		i.store.Update(rel)
		fmt.Printf("    [Install] %s: failed\n", rel.Name)
		return rel, fmt.Errorf("설치 실패 시뮬레이션")
	}

	// 3) 성공: deployed 상태로 전이
	rel.SetStatus(StatusDeployed, "Install complete")
	rel.Info.Notes = fmt.Sprintf("Application %s is deployed!", i.Name)
	i.store.Update(rel)
	fmt.Printf("    [Install] %s: deployed (v%d)\n", rel.Name, rel.Version)

	return rel, nil
}

// Upgrade는 기존 릴리스를 새 버전으로 업그레이드한다.
// 실제 Helm: action.Upgrade.RunWithContext()
// 흐름: 이전 릴리스 → superseded, 새 릴리스 → pending-upgrade → deployed
type Upgrade struct {
	store      *Storage
	Name       string
	Chart      string
	Config     map[string]any
	SimFailure bool
}

func (u *Upgrade) Run() (*Release, error) {
	// 1) 현재 배포된 릴리스 조회
	current := u.store.Last(u.Name)
	if current == nil {
		return nil, fmt.Errorf("릴리스 %q 를 찾을 수 없습니다", u.Name)
	}

	now := time.Now()
	newVersion := current.Version + 1

	// 2) 이전 릴리스를 superseded로 변경
	// 실제 Helm: previousRelease.SetStatus(release.StatusSuperseded, ...)
	current.SetStatus(StatusSuperseded, fmt.Sprintf("Superseded by v%d", newVersion))
	u.store.Update(current)
	fmt.Printf("    [Upgrade] %s v%d: superseded\n", current.Name, current.Version)

	// 3) 새 릴리스 생성 (pending-upgrade)
	rel := &Release{
		Name:      u.Name,
		Chart:     u.Chart,
		Namespace: current.Namespace,
		Config:    u.Config,
		Version:   newVersion,
		Manifest:  fmt.Sprintf("# Rendered manifest for %s (upgrade)", u.Chart),
		Info: &Info{
			FirstDeployed: current.Info.FirstDeployed,
			LastDeployed:  now,
			Status:        StatusPendingUpgrade,
			Description:   "Upgrade started",
		},
	}

	fmt.Printf("    [Upgrade] %s: pending-upgrade (v%d)\n", rel.Name, rel.Version)
	u.store.Create(rel)

	// 4) 리소스 업데이트 시뮬레이션
	if u.SimFailure {
		rel.SetStatus(StatusFailed, "Upgrade failed: simulated error")
		u.store.Update(rel)
		fmt.Printf("    [Upgrade] %s v%d: failed\n", rel.Name, rel.Version)
		return rel, fmt.Errorf("업그레이드 실패 시뮬레이션")
	}

	// 5) 성공: deployed
	rel.SetStatus(StatusDeployed, "Upgrade complete")
	u.store.Update(rel)
	fmt.Printf("    [Upgrade] %s: deployed (v%d)\n", rel.Name, rel.Version)

	return rel, nil
}

// Rollback은 이전 리비전으로 롤백한다.
// 실제 Helm: action.Rollback.Run()
// 흐름: 현재 → superseded, 대상 리비전 복제 → pending-rollback → deployed
type Rollback struct {
	store         *Storage
	Name          string
	TargetVersion int // 0이면 바로 이전 deployed 버전으로
}

func (rb *Rollback) Run() (*Release, error) {
	// 1) 현재 릴리스 조회
	current := rb.store.Last(rb.Name)
	if current == nil {
		return nil, fmt.Errorf("릴리스 %q 를 찾을 수 없습니다", rb.Name)
	}

	// 2) 대상 리비전 결정
	targetVersion := rb.TargetVersion
	if targetVersion == 0 {
		// 바로 이전 deployed 또는 superseded 리비전 찾기
		history := rb.store.History(rb.Name)
		for i := len(history) - 2; i >= 0; i-- {
			if history[i].Info.Status == StatusSuperseded || history[i].Info.Status == StatusDeployed {
				targetVersion = history[i].Version
				break
			}
		}
	}

	if targetVersion == 0 {
		return nil, fmt.Errorf("롤백할 대상 리비전을 찾을 수 없습니다")
	}

	target, err := rb.store.Get(rb.Name, targetVersion)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	newVersion := current.Version + 1

	// 3) 현재 릴리스를 superseded
	current.SetStatus(StatusSuperseded, fmt.Sprintf("Superseded by rollback to v%d", targetVersion))
	rb.store.Update(current)
	fmt.Printf("    [Rollback] %s v%d: superseded\n", current.Name, current.Version)

	// 4) 대상 리비전을 복제하여 새 리비전 생성
	// 실제 Helm: 대상 릴리스의 Chart/Config/Manifest를 복사하여 새 버전으로 생성
	rel := &Release{
		Name:      rb.Name,
		Chart:     target.Chart,
		Namespace: target.Namespace,
		Config:    target.Config,
		Manifest:  target.Manifest,
		Version:   newVersion,
		Info: &Info{
			FirstDeployed: target.Info.FirstDeployed,
			LastDeployed:  now,
			Status:        StatusPendingRollback,
			Description:   fmt.Sprintf("Rollback to v%d", targetVersion),
		},
	}

	fmt.Printf("    [Rollback] %s: pending-rollback (v%d ← v%d 복제)\n", rel.Name, rel.Version, targetVersion)
	rb.store.Create(rel)

	// 5) 성공: deployed
	rel.SetStatus(StatusDeployed, fmt.Sprintf("Rollback to v%d complete", targetVersion))
	rb.store.Update(rel)
	fmt.Printf("    [Rollback] %s: deployed (v%d)\n", rel.Name, rel.Version)

	return rel, nil
}

// Uninstall은 릴리스를 삭제한다.
// 실제 Helm: action.Uninstall.Run()
// 흐름: uninstalling → 리소스 삭제 → uninstalled (KeepHistory에 따라 이력 보존)
type Uninstall struct {
	store       *Storage
	Name        string
	KeepHistory bool
}

func (u *Uninstall) Run() (*Release, error) {
	current := u.store.Last(u.Name)
	if current == nil {
		return nil, fmt.Errorf("릴리스 %q 를 찾을 수 없습니다", u.Name)
	}

	// 1) uninstalling 상태
	current.SetStatus(StatusUninstalling, "Uninstall started")
	u.store.Update(current)
	fmt.Printf("    [Uninstall] %s: uninstalling\n", current.Name)

	// 2) 리소스 삭제 시뮬레이션
	fmt.Printf("    [Uninstall] 클러스터에서 리소스 삭제 중...\n")

	// 3) uninstalled 상태
	now := time.Now()
	current.Info.Deleted = now
	current.SetStatus(StatusUninstalled, "Uninstall complete")
	u.store.Update(current)
	fmt.Printf("    [Uninstall] %s: uninstalled\n", current.Name)

	// 4) KeepHistory가 false이면 이력도 삭제
	if !u.KeepHistory {
		history := u.store.History(u.Name)
		for _, r := range history {
			u.store.Delete(r.Name, r.Version)
		}
		fmt.Printf("    [Uninstall] 이력 삭제 완료 (%d개 리비전)\n", len(history))
	} else {
		fmt.Printf("    [Uninstall] 이력 보존 (--keep-history)\n")
	}

	return current, nil
}

// =============================================================================
// 히스토리 출력 유틸리티
// =============================================================================

func printHistory(store *Storage, name string) {
	history := store.History(name)
	if len(history) == 0 {
		fmt.Printf("  (이력 없음)\n")
		return
	}

	fmt.Printf("  %-10s %-18s %-15s %-25s %s\n",
		"REVISION", "STATUS", "CHART", "DESCRIPTION", "DEPLOYED")
	for _, r := range history {
		fmt.Printf("  %-10d %-18s %-15s %-25s %s\n",
			r.Version, r.Info.Status, r.Chart,
			truncate(r.Info.Description, 25),
			r.Info.LastDeployed.Format("15:04:05"))
	}
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}

// =============================================================================
// main: 전체 라이프사이클 시연
// =============================================================================

func main() {
	fmt.Println("=== Helm v4 Release 라이프사이클 PoC ===")
	fmt.Println()

	store := NewStorage()

	// 1) Install
	demoInstall(store)

	// 2) Upgrade (2회)
	demoUpgrade(store)

	// 3) Rollback
	demoRollback(store)

	// 4) 최종 히스토리
	fmt.Println("--- 5. 전체 히스토리 ---")
	printHistory(store, "myapp")
	fmt.Println()

	// 5) Uninstall (keep-history)
	demoUninstallKeepHistory(store)

	// 6) 새 릴리스 + 실패 시나리오
	demoFailedInstall(store)

	// 7) 상태 전이 다이어그램
	printStateDiagram()
}

func demoInstall(store *Storage) {
	fmt.Println("--- 1. Install ---")

	inst := &Install{
		store:     store,
		Name:      "myapp",
		Chart:     "myapp-1.0.0",
		Namespace: "production",
		Config:    map[string]any{"replicaCount": 1},
	}

	_, err := inst.Run()
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
	}

	fmt.Println("\n  히스토리:")
	printHistory(store, "myapp")
	fmt.Println()
}

func demoUpgrade(store *Storage) {
	fmt.Println("--- 2. Upgrade (2회) ---")

	// 첫 번째 업그레이드
	up1 := &Upgrade{
		store:  store,
		Name:   "myapp",
		Chart:  "myapp-1.1.0",
		Config: map[string]any{"replicaCount": 2},
	}
	up1.Run()

	// 두 번째 업그레이드
	up2 := &Upgrade{
		store:  store,
		Name:   "myapp",
		Chart:  "myapp-2.0.0",
		Config: map[string]any{"replicaCount": 3, "image": "myapp:v2"},
	}
	up2.Run()

	fmt.Println("\n  히스토리:")
	printHistory(store, "myapp")
	fmt.Println()
}

func demoRollback(store *Storage) {
	fmt.Println("--- 3. Rollback (v2로 롤백) ---")

	rb := &Rollback{
		store:         store,
		Name:          "myapp",
		TargetVersion: 2, // v2로 롤백
	}
	rb.Run()

	fmt.Println("\n  히스토리:")
	printHistory(store, "myapp")
	fmt.Println()

	fmt.Println("--- 4. Rollback (바로 이전으로) ---")

	rb2 := &Rollback{
		store: store,
		Name:  "myapp",
	}
	rb2.Run()

	fmt.Println("\n  히스토리:")
	printHistory(store, "myapp")
	fmt.Println()
}

func demoUninstallKeepHistory(store *Storage) {
	fmt.Println("--- 6. Uninstall (keep-history) ---")

	uninst := &Uninstall{
		store:       store,
		Name:        "myapp",
		KeepHistory: true,
	}
	uninst.Run()

	fmt.Println("\n  히스토리 (보존됨):")
	printHistory(store, "myapp")
	fmt.Println()
}

func demoFailedInstall(store *Storage) {
	fmt.Println("--- 7. 실패 시나리오 ---")

	inst := &Install{
		store:      store,
		Name:       "failing-app",
		Chart:      "broken-chart-1.0.0",
		Namespace:  "default",
		Config:     map[string]any{},
		SimFailure: true,
	}

	_, err := inst.Run()
	fmt.Printf("  결과: %v\n", err)

	fmt.Println("\n  failing-app 히스토리:")
	printHistory(store, "failing-app")
	fmt.Println()
}

func printStateDiagram() {
	fmt.Println("=== 상태 전이 다이어그램 ===")
	fmt.Println()
	diagram := `
  Install:
    (없음) → pending-install → deployed
                              → failed

  Upgrade:
    deployed(현재) → superseded
    (새 리비전) → pending-upgrade → deployed
                                  → failed

  Rollback:
    deployed(현재) → superseded
    (이전 복제)    → pending-rollback → deployed

  Uninstall:
    deployed → uninstalling → uninstalled (이력 보존)
                             → (삭제됨)    (이력 미보존)

  상태 요약:
    deployed        ─ 정상 배포됨 (하나만 active)
    superseded      ─ 이전 리비전 (업그레이드/롤백으로 대체됨)
    failed          ─ 설치/업그레이드 실패
    uninstalled     ─ 삭제됨 (이력은 보존)
    pending-*       ─ 진행 중 (전이 상태)
`
	fmt.Println(diagram)

	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. 리비전은 항상 증가 (롤백도 새 리비전 번호)")
	fmt.Println("  2. 한 릴리스에서 deployed 상태는 최대 1개")
	fmt.Println("  3. 업그레이드/롤백 시 이전 릴리스는 superseded")
	fmt.Println("  4. 롤백은 대상 리비전의 Chart/Config/Manifest를 복제")
	fmt.Println("  5. KeepHistory로 uninstall 후에도 이력 보존 가능")
}
