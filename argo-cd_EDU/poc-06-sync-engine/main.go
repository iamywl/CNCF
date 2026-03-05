// poc-06-sync-engine: Argo CD 싱크 엔진 상태 머신 시뮬레이션
//
// 실제 소스 참조:
//   - gitops-engine/pkg/sync/sync_context.go: syncContext, Sync() 메서드
//   - gitops-engine/pkg/sync/sync_context.go:599-620: phase/wave 반복 루프
//   - gitops-engine/pkg/sync/sync_context.go:569: syncFailTasks 분리
//   - gitops-engine/pkg/sync/sync_context.go:1031-1062: prune wave 역순 처리
//   - gitops-engine/pkg/sync/sync_context.go:923-935: generateName hook 이름 생성
//   - gitops-engine/pkg/sync/common/types.go: SyncPhase, SyncWave 상수
//
// go run main.go
package main

import (
	"fmt"
	"strings"
	"time"
)

// ─────────────────────────────────────────────
// Sync Phase 상수
// 실제: gitops-engine/pkg/sync/common/types.go
// ─────────────────────────────────────────────

type SyncPhase string

const (
	SyncPhasePreSync  SyncPhase = "PreSync"
	SyncPhaseSync     SyncPhase = "Sync"
	SyncPhasePostSync SyncPhase = "PostSync"
	SyncPhaseSyncFail SyncPhase = "SyncFail"
)

// phaseOrder는 Phase 실행 순서를 정의한다
// 실제: gitops-engine/pkg/sync/sync_phase.go
var phaseOrder = []SyncPhase{
	SyncPhasePreSync,
	SyncPhaseSync,
	SyncPhasePostSync,
}

// ─────────────────────────────────────────────
// Sync 상태 코드
// 실제: gitops-engine/pkg/sync/common/types.go
// ─────────────────────────────────────────────

type ResultCode string

const (
	ResultCodeSynced     ResultCode = "Synced"
	ResultCodeSyncFailed ResultCode = "SyncFailed"
	ResultCodePruned     ResultCode = "Pruned"
	ResultCodePruneSkipped ResultCode = "PruneSkipped"
)

type OperationPhase string

const (
	OperationRunning   OperationPhase = "Running"
	OperationFailed    OperationPhase = "Failed"
	OperationError     OperationPhase = "Error"
	OperationSucceeded OperationPhase = "Succeeded"
)

// ─────────────────────────────────────────────
// Delete Policy (Hook 삭제 정책)
// 실제: gitops-engine/pkg/sync/hook/hook.go
// ─────────────────────────────────────────────

type HookDeletePolicy string

const (
	HookDeletePolicyHookSucceeded      HookDeletePolicy = "HookSucceeded"
	HookDeletePolicyHookFailed         HookDeletePolicy = "HookFailed"
	HookDeletePolicyBeforeHookCreation HookDeletePolicy = "BeforeHookCreation"
)

// ─────────────────────────────────────────────
// SyncTask — 개별 sync 작업 단위
// 실제: gitops-engine/pkg/sync/sync_task.go
// ─────────────────────────────────────────────

type TaskType string

const (
	TaskTypeApply  TaskType = "Apply"
	TaskTypePrune  TaskType = "Prune"
	TaskTypeHook   TaskType = "Hook"
)

// SyncTask는 단일 리소스에 대한 sync 작업을 나타낸다
type SyncTask struct {
	// 리소스 식별
	Kind      string
	Name      string
	Namespace string

	// Sync 제어
	Phase        SyncPhase
	Wave         int
	WaveOverride *int // prune wave 역순 처리용
	Type         TaskType
	PruneLast    bool // argocd.argoproj.io/sync-wave-prune-last 어노테이션

	// Hook 속성
	IsHook       bool
	DeletePolicy HookDeletePolicy

	// 실행 상태
	SyncStatus  ResultCode
	Running     bool
	DryRunOK    bool

	// 실행 시뮬레이션용
	ShouldFail bool
	Duration   time.Duration
}

func (t *SyncTask) effectiveWave() int {
	if t.WaveOverride != nil {
		return *t.WaveOverride
	}
	return t.Wave
}

func (t *SyncTask) completed() bool {
	return t.SyncStatus == ResultCodeSynced ||
		t.SyncStatus == ResultCodeSyncFailed ||
		t.SyncStatus == ResultCodePruned
}

func (t *SyncTask) successful() bool {
	return t.SyncStatus == ResultCodeSynced || t.SyncStatus == ResultCodePruned
}

func (t *SyncTask) pending() bool {
	return !t.completed() && !t.Running
}

func (t *SyncTask) String() string {
	return fmt.Sprintf("[%s/%s wave=%d phase=%s type=%s]",
		t.Kind, t.Name, t.effectiveWave(), t.Phase, t.Type)
}

// ─────────────────────────────────────────────
// SyncContext — 싱크 상태 머신의 핵심
// 실제: gitops-engine/pkg/sync/sync_context.go
// ─────────────────────────────────────────────

type SyncContext struct {
	// 입력
	revision string
	tasks    []*SyncTask
	pruneLast bool

	// 상태
	phase   OperationPhase
	message string

	// 시작 시간 (hook 이름 생성용)
	startedAt time.Time
}

func NewSyncContext(revision string, tasks []*SyncTask) *SyncContext {
	return &SyncContext{
		revision:  revision,
		tasks:     tasks,
		startedAt: time.Now(),
	}
}

func (sc *SyncContext) WithPruneLast(enabled bool) *SyncContext {
	sc.pruneLast = enabled
	return sc
}

// ─────────────────────────────────────────────
// Hook 이름 생성
// 실제: gitops-engine/pkg/sync/sync_context.go:923-935
// generateName hook: name = generateName + revision[0:7] + phase + timestamp
// ─────────────────────────────────────────────

func (sc *SyncContext) generateHookName(task *SyncTask) string {
	if task.Name != "" {
		return task.Name
	}
	// generateName 기반 이름 생성
	// 실제: "postfix := strings.ToLower(fmt.Sprintf("%s-%s-%d", syncRevision, phase, sc.startedAt.UTC().Unix()))"
	syncRevision := sc.revision
	if len(syncRevision) >= 8 {
		syncRevision = syncRevision[0:7]
	}
	postfix := strings.ToLower(fmt.Sprintf("%s-%s-%d",
		syncRevision, string(task.Phase), sc.startedAt.UTC().Unix()))
	return fmt.Sprintf("hook-%s", postfix)
}

// ─────────────────────────────────────────────
// Prune Wave 역순 처리
// 실제: gitops-engine/pkg/sync/sync_context.go:1031-1062
// "prune tasks, modify the waves for proper cleanup i.e reverse of sync wave"
// ─────────────────────────────────────────────

func (sc *SyncContext) adjustPruneWaves() {
	// prune 태스크만 추출
	var pruneTasks []*SyncTask
	for _, t := range sc.tasks {
		if t.Type == TaskTypePrune {
			pruneTasks = append(pruneTasks, t)
		}
	}
	if len(pruneTasks) == 0 {
		return
	}

	// wave 별로 그룹화
	waveGroups := make(map[int][]*SyncTask)
	for _, t := range pruneTasks {
		waveGroups[t.Wave] = append(waveGroups[t.Wave], t)
	}

	// 최소/최대 wave 찾기
	minWave, maxWave := 999, -999
	for w := range waveGroups {
		if w < minWave {
			minWave = w
		}
		if w > maxWave {
			maxWave = w
		}
	}

	// wave 역순: symmetric swap
	// 실제: "waves to swap" → start/end wave를 교환
	// wave 0은 maxWave로, wave 1은 maxWave-1로, ...
	for _, t := range pruneTasks {
		if sc.pruneLast {
			// pruneLast: sync phase의 마지막 wave + 1로 설정
			// 실제: gitops-engine/pkg/sync/sync_context.go:1061-1062
			lastSyncWave := sc.lastSyncPhaseWave()
			overrideWave := lastSyncWave + 1
			t.WaveOverride = &overrideWave
		} else {
			// 역순 wave: endWave = maxWave - (t.Wave - minWave)
			endWave := maxWave - (t.Wave - minWave)
			t.WaveOverride = &endWave
		}
	}
}

// lastSyncPhaseWave는 Sync phase에서 가장 높은 wave를 반환한다
func (sc *SyncContext) lastSyncPhaseWave() int {
	maxWave := 0
	for _, t := range sc.tasks {
		if t.Phase == SyncPhaseSync && t.Wave > maxWave {
			maxWave = t.Wave
		}
	}
	return maxWave
}

// ─────────────────────────────────────────────
// Dry-run 검증
// sync 실제 적용 전 모든 태스크의 dry-run 먼저 실행
// ─────────────────────────────────────────────

func (sc *SyncContext) dryRun() error {
	fmt.Println("\n  [Dry-run] 모든 태스크 사전 검증...")
	for _, t := range sc.tasks {
		if t.ShouldFail {
			return fmt.Errorf("dry-run 실패: %s — 설정 오류 감지", t)
		}
		t.DryRunOK = true
		fmt.Printf("    OK: %s\n", t)
	}
	fmt.Println("  [Dry-run] 검증 완료, 실제 sync 진행")
	return nil
}

// ─────────────────────────────────────────────
// Sync() — 상태 머신 메인 루프
// 실제: gitops-engine/pkg/sync/sync_context.go:Sync() 메서드
// 반복 호출로 점진적 진행 (한 번에 한 wave씩)
// ─────────────────────────────────────────────

// Sync는 현재 pending 태스크를 phase/wave 순서로 처리한다.
// 완료되면 true를 반환한다.
// 실제 Argo CD는 이 메서드를 반복 호출하며 진행한다.
func (sc *SyncContext) Sync() bool {
	// 이미 완료/실패면 종료
	if sc.phase == OperationSucceeded || sc.phase == OperationFailed || sc.phase == OperationError {
		return true
	}

	// SyncFail 태스크 분리
	// 실제: gitops-engine/pkg/sync/sync_context.go:569
	var syncFailTasks []*SyncTask
	var regularTasks []*SyncTask
	for _, t := range sc.tasks {
		if t.Phase == SyncPhaseSyncFail {
			syncFailTasks = append(syncFailTasks, t)
		} else {
			regularTasks = append(regularTasks, t)
		}
	}

	// 실패한 태스크 확인
	for _, t := range regularTasks {
		if t.completed() && !t.successful() {
			fmt.Printf("\n  [FAIL] %s 실패! SyncFail 훅 실행...\n", t)
			sc.executeSyncFailPhase(syncFailTasks)
			sc.phase = OperationFailed
			sc.message = fmt.Sprintf("sync 실패: %s", t.Name)
			return true
		}
	}

	// Pending 태스크 필터링
	var pendingTasks []*SyncTask
	for _, t := range regularTasks {
		if t.pending() {
			pendingTasks = append(pendingTasks, t)
		}
	}

	// 모든 태스크 완료
	if len(pendingTasks) == 0 {
		sc.phase = OperationSucceeded
		sc.message = "successfully synced (no more tasks)"
		return true
	}

	// 현재 phase와 wave 결정
	// 실제: gitops-engine/pkg/sync/sync_context.go:598-603
	// tasks.phase() = 가장 낮은 phase의 가장 낮은 wave
	currentPhase, currentWave := sc.currentPhaseAndWave(pendingTasks)
	fmt.Printf("\n  [Wave 실행] phase=%s, wave=%d\n", currentPhase, currentWave)

	// 현재 phase+wave의 태스크만 실행
	var waveTasks []*SyncTask
	for _, t := range pendingTasks {
		if t.Phase == currentPhase && t.effectiveWave() == currentWave {
			waveTasks = append(waveTasks, t)
		}
	}

	sc.phase = OperationRunning
	sc.runWaveTasks(waveTasks)
	return false
}

// currentPhaseAndWave는 현재 처리할 phase와 wave를 반환한다.
// phaseOrder 순서로 가장 낮은 phase, 그 안에서 가장 낮은 wave.
func (sc *SyncContext) currentPhaseAndWave(pendingTasks []*SyncTask) (SyncPhase, int) {
	// phase 우선순위: PreSync < Sync < PostSync
	for _, phase := range phaseOrder {
		minWave := 999
		found := false
		for _, t := range pendingTasks {
			if t.Phase == phase {
				wave := t.effectiveWave()
				if !found || wave < minWave {
					minWave = wave
					found = true
				}
			}
		}
		if found {
			return phase, minWave
		}
	}
	return SyncPhaseSync, 0
}

// runWaveTasks는 한 wave의 태스크들을 실행한다
func (sc *SyncContext) runWaveTasks(tasks []*SyncTask) {
	for _, t := range tasks {
		sc.executeTask(t)
	}
}

// executeTask는 개별 태스크를 실행한다
func (sc *SyncContext) executeTask(t *SyncTask) {
	hookName := ""
	if t.IsHook {
		hookName = sc.generateHookName(t)
		fmt.Printf("    -> [Hook] 생성: %s (name=%s)\n", t, hookName)
	} else {
		fmt.Printf("    -> [Apply] 적용: %s\n", t)
	}

	if t.ShouldFail {
		t.SyncStatus = ResultCodeSyncFailed
		fmt.Printf("    !! [실패] %s\n", t)
		return
	}

	// Hook은 실행 중 상태를 거쳐 완료
	if t.IsHook {
		t.Running = true
		fmt.Printf("    .. [Running] %s (건강 체크 중...)\n", t)
		// 실제로는 k8s Pod/Job 상태를 폴링
		t.Running = false
		t.SyncStatus = ResultCodeSynced
		fmt.Printf("    OK [Hook 완료] %s → %s\n", hookName, t.DeletePolicy)
		// Hook delete policy 처리
		sc.handleHookDelete(t)
	} else if t.Type == TaskTypePrune {
		t.SyncStatus = ResultCodePruned
		fmt.Printf("    OK [Pruned] %s\n", t)
	} else {
		t.SyncStatus = ResultCodeSynced
		fmt.Printf("    OK [Synced] %s\n", t)
	}
}

// handleHookDelete는 hook delete policy에 따라 훅을 삭제한다
// 실제: gitops-engine/pkg/sync/sync_context.go:560-566
func (sc *SyncContext) handleHookDelete(t *SyncTask) {
	switch t.DeletePolicy {
	case HookDeletePolicyHookSucceeded:
		if t.successful() {
			fmt.Printf("    [Delete] %s (HookSucceeded policy)\n", t.Name)
		}
	case HookDeletePolicyHookFailed:
		if !t.successful() {
			fmt.Printf("    [Delete] %s (HookFailed policy)\n", t.Name)
		}
	case HookDeletePolicyBeforeHookCreation:
		fmt.Printf("    [Delete] %s (BeforeHookCreation policy: 다음 sync 전 삭제)\n", t.Name)
	}
}

// executeSyncFailPhase는 SyncFail phase의 훅을 실행한다
// 실제: gitops-engine/pkg/sync/sync_context.go:845-880
func (sc *SyncContext) executeSyncFailPhase(syncFailTasks []*SyncTask) {
	if len(syncFailTasks) == 0 {
		fmt.Println("  [SyncFail] SyncFail 훅 없음")
		return
	}
	for _, t := range syncFailTasks {
		if !t.pending() {
			continue
		}
		fmt.Printf("  [SyncFail 훅] 실행: %s\n", t)
		t.SyncStatus = ResultCodeSynced
	}
}

// ─────────────────────────────────────────────
// SyncRunner — 반복 Sync() 호출로 완료까지 진행
// ─────────────────────────────────────────────

func (sc *SyncContext) Run() {
	fmt.Printf("\n revision: %s\n", sc.revision)
	fmt.Printf(" 총 태스크 수: %d\n", len(sc.tasks))
	fmt.Println()

	// 1단계: Prune wave 역순 조정
	sc.adjustPruneWaves()

	// 2단계: Dry-run 검증 (실제 적용 전 사전 검사)
	if err := sc.dryRun(); err != nil {
		fmt.Printf("[Dry-run 실패] %v\n", err)
		sc.phase = OperationFailed
		return
	}

	// 3단계: 반복 Sync() 호출
	iteration := 0
	for {
		iteration++
		fmt.Printf("\n--- Sync 반복 #%d ---", iteration)
		done := sc.Sync()
		if done {
			break
		}
		if iteration > 20 {
			fmt.Println("\n[오류] 무한 루프 방지: 최대 반복 횟수 초과")
			break
		}
	}

	// 결과 출력
	fmt.Printf("\n\n=== Sync 완료 ===\n")
	fmt.Printf("  최종 상태: %s\n", sc.phase)
	fmt.Printf("  메시지: %s\n", sc.message)
	fmt.Println()
	fmt.Println("  태스크 결과:")
	for _, t := range sc.tasks {
		icon := "OK"
		if t.SyncStatus == ResultCodeSyncFailed {
			icon = "FAIL"
		} else if t.SyncStatus == ResultCodePruned {
			icon = "PRUNE"
		}
		fmt.Printf("    [%-5s] %s → %s\n", icon, t, t.SyncStatus)
	}
}

// ─────────────────────────────────────────────
// 태스크 빌더 헬퍼
// ─────────────────────────────────────────────

func applyTask(kind, name string, phase SyncPhase, wave int) *SyncTask {
	return &SyncTask{Kind: kind, Name: name, Phase: phase, Wave: wave, Type: TaskTypeApply}
}

func pruneTask(kind, name string, wave int) *SyncTask {
	return &SyncTask{Kind: kind, Name: name, Phase: SyncPhaseSync, Wave: wave, Type: TaskTypePrune}
}

func hookTask(kind, name string, phase SyncPhase, deletePolicy HookDeletePolicy) *SyncTask {
	return &SyncTask{Kind: kind, Name: name, Phase: phase, IsHook: true,
		Type: TaskTypeHook, DeletePolicy: deletePolicy}
}

// generateNameHook은 generateName 기반 훅 (이름 자동 생성)
func generateNameHook(kind string, phase SyncPhase) *SyncTask {
	return &SyncTask{Kind: kind, Name: "", Phase: phase, IsHook: true,
		Type: TaskTypeHook, DeletePolicy: HookDeletePolicyHookSucceeded}
}

// ─────────────────────────────────────────────
// 시나리오 출력 헬퍼
// ─────────────────────────────────────────────

func printScenario(title, description string) {
	fmt.Printf("\n%s\n", strings.Repeat("=", 65))
	fmt.Printf("시나리오: %s\n", title)
	fmt.Printf("%s\n", strings.Repeat("-", 65))
	lines := strings.Split(description, "\n")
	for _, l := range lines {
		if l != "" {
			fmt.Println(l)
		}
	}
}

// ─────────────────────────────────────────────
// main — 시나리오 실행
// ─────────────────────────────────────────────

func main() {
	fmt.Println("=== Argo CD Sync Engine 상태 머신 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: gitops-engine/pkg/sync/sync_context.go")
	fmt.Println()
	fmt.Println("Sync 실행 구조:")
	fmt.Println("  Sync() 반복 호출 → phase/wave 순서로 점진 처리")
	fmt.Println("  PreSync(wave 0,1,...) → Sync(wave 0,1,...) → PostSync(wave 0,1,...)")
	fmt.Println("  실패 시 → SyncFail phase 훅 실행")

	// ─────────────────────────────────────────────
	// 시나리오 1: 기본 Sync (PreSync + Sync + PostSync)
	// ─────────────────────────────────────────────
	printScenario("기본 Sync: PreSync Job → Sync Deployment → PostSync Test",
		`PreSync: DB 마이그레이션 Job (wave 0)
Sync: Deployment (wave 0), ConfigMap (wave 0)
PostSync: 연기 테스트 Job (wave 0)`)

	tasks1 := []*SyncTask{
		hookTask("Job", "db-migrate", SyncPhasePreSync, HookDeletePolicyHookSucceeded),
		applyTask("ConfigMap", "app-config", SyncPhaseSync, 0),
		applyTask("Deployment", "myapp", SyncPhaseSync, 0),
		hookTask("Job", "smoke-test", SyncPhasePostSync, HookDeletePolicyHookSucceeded),
	}

	ctx1 := NewSyncContext("a1b2c3d4e5f6", tasks1)
	ctx1.Run()

	// ─────────────────────────────────────────────
	// 시나리오 2: Sync Wave (여러 wave)
	// ─────────────────────────────────────────────
	printScenario("Sync Wave: wave 0 → wave 1 → wave 2 순차 실행",
		`wave 0: 인프라 (Namespace, ServiceAccount)
wave 1: 데이터베이스 (StatefulSet)  -- wave 0 완료 후
wave 2: 애플리케이션 (Deployment)   -- wave 1 완료 후`)

	tasks2 := []*SyncTask{
		applyTask("Namespace", "myns", SyncPhaseSync, 0),
		applyTask("ServiceAccount", "mysa", SyncPhaseSync, 0),
		applyTask("StatefulSet", "postgres", SyncPhaseSync, 1),
		applyTask("Deployment", "backend", SyncPhaseSync, 2),
		applyTask("Deployment", "frontend", SyncPhaseSync, 2),
	}

	ctx2 := NewSyncContext("b2c3d4e5f6a1", tasks2)
	ctx2.Run()

	// ─────────────────────────────────────────────
	// 시나리오 3: Prune Wave 역순
	// ─────────────────────────────────────────────
	printScenario("Prune Wave 역순: 높은 wave부터 삭제",
		`생성 순서: wave 0 (infra) → wave 1 (app) → wave 2 (frontend)
삭제 순서: wave 2 (frontend) → wave 1 (app) → wave 0 (infra)
의존성 역순으로 안전하게 제거`)

	tasks3 := []*SyncTask{
		pruneTask("Namespace", "old-ns", 0),
		pruneTask("StatefulSet", "old-db", 1),
		pruneTask("Deployment", "old-frontend", 2),
		// 새 리소스는 정상 적용
		applyTask("ConfigMap", "new-config", SyncPhaseSync, 0),
	}

	ctx3 := NewSyncContext("c3d4e5f6a1b2", tasks3)
	ctx3.Run()

	fmt.Println("\n  [설명] prune wave 역순 조정 결과:")
	for _, t := range tasks3 {
		if t.Type == TaskTypePrune {
			fmt.Printf("    %s: 원래 wave=%d → 실제 실행 wave=%d\n",
				t.Name, t.Wave, t.effectiveWave())
		}
	}

	// ─────────────────────────────────────────────
	// 시나리오 4: PruneLast 어노테이션
	// ─────────────────────────────────────────────
	printScenario("PruneLast: sync 마지막에 삭제",
		`argocd.argoproj.io/sync-wave-prune-last: "true" 어노테이션이 있는 리소스는
모든 sync가 완료된 후 마지막에 삭제됨.
예: 마이그레이션 후 제거할 레거시 ConfigMap`)

	tasks4 := []*SyncTask{
		applyTask("Deployment", "new-app", SyncPhaseSync, 0),
		{
			Kind:      "ConfigMap",
			Name:      "legacy-config",
			Phase:     SyncPhaseSync,
			Wave:      0,
			Type:      TaskTypePrune,
			PruneLast: true, // 마지막에 삭제
		},
	}

	ctx4 := NewSyncContext("d4e5f6a1b2c3", tasks4)
	ctx4.WithPruneLast(true)
	ctx4.Run()

	fmt.Println("\n  [설명] pruneLast 조정 결과:")
	for _, t := range tasks4 {
		if t.Type == TaskTypePrune {
			fmt.Printf("    %s: 원래 wave=%d → 실제 실행 wave=%d\n",
				t.Name, t.Wave, t.effectiveWave())
		}
	}

	// ─────────────────────────────────────────────
	// 시나리오 5: generateName Hook 이름 자동 생성
	// ─────────────────────────────────────────────
	printScenario("GenerateName Hook: 이름 자동 생성",
		`metadata.generateName이 설정된 hook은 매 sync마다 고유한 이름이 생성됨
이름 형식: generateName + revision[0:7] + phase + timestamp
이유: hook을 멱등적으로 실행 (매번 새 Job 생성)`)

	tasks5 := []*SyncTask{
		generateNameHook("Job", SyncPhasePreSync),
		applyTask("Deployment", "app", SyncPhaseSync, 0),
	}

	ctx5 := NewSyncContext("f1e2d3c4b5a6789", tasks5)
	fmt.Printf("\n  revision: %s\n", ctx5.revision)
	for _, t := range tasks5 {
		if t.IsHook && t.Name == "" {
			generatedName := ctx5.generateHookName(t)
			fmt.Printf("  [generateName] %s/%s → 생성된 이름: %s\n",
				t.Kind, t.Phase, generatedName)
		}
	}
	ctx5.Run()

	// ─────────────────────────────────────────────
	// 시나리오 6: Sync 실패 → SyncFail Phase
	// ─────────────────────────────────────────────
	printScenario("Sync 실패 → SyncFail Phase",
		`Sync 중 Deployment 적용 실패 →
SyncFail phase의 훅이 실행됨 (롤백, 알림 등)
실제: gitops-engine/pkg/sync/sync_context.go:569-577`)

	tasks6 := []*SyncTask{
		applyTask("ConfigMap", "app-config", SyncPhaseSync, 0),
		{
			Kind: "Deployment", Name: "broken-app",
			Phase: SyncPhaseSync, Wave: 0, Type: TaskTypeApply,
			// ShouldFail은 dry-run은 통과하되 실제 적용 시 실패
		},
		hookTask("Job", "rollback", SyncPhaseSyncFail, HookDeletePolicyHookSucceeded),
	}
	// broken-app을 dry-run 후 실제 실행 시 실패하도록 설정
	tasks6[1].DryRunOK = false // dry-run 통과
	tasks6[1].ShouldFail = false // dry-run에서는 성공
	// 실제 실행 시 실패 시뮬레이션을 위해 별도 플래그
	tasks6[1].ShouldFail = true // 실제 실행 시 실패

	ctx6 := NewSyncContext("e5f6a1b2c3d4", tasks6)
	// dry-run은 성공하도록 — ShouldFail을 임시로 false
	tasks6[1].ShouldFail = false
	fmt.Printf("\n revision: %s\n 총 태스크 수: %d\n\n", ctx6.revision, len(tasks6))
	if err := ctx6.dryRun(); err != nil {
		fmt.Printf("[Dry-run 실패] %v\n", err)
	}
	// dry-run 통과 후 실제 실행 시 실패 설정
	tasks6[1].ShouldFail = true
	fmt.Println("\n  [실제 실행] broken-app apply 실패 시뮬레이션...")

	// Sync() 반복 호출
	for i := 0; i < 10; i++ {
		done := ctx6.Sync()
		if done {
			break
		}
	}

	fmt.Printf("\n=== Sync 완료 ===\n")
	fmt.Printf("  최종 상태: %s\n", ctx6.phase)
	fmt.Printf("  메시지: %s\n", ctx6.message)
	fmt.Println("\n  태스크 결과:")
	for _, t := range tasks6 {
		icon := "OK"
		if t.SyncStatus == ResultCodeSyncFailed {
			icon = "FAIL"
		}
		fmt.Printf("    [%-5s] %s → %s\n", icon, t, t.SyncStatus)
	}

	// 요약
	fmt.Println()
	fmt.Println(strings.Repeat("=", 65))
	fmt.Println("Sync Engine 핵심 포인트:")
	fmt.Println("  1. Sync()는 반복 호출 — 한 번에 한 phase/wave씩 처리")
	fmt.Println("  2. Phase 순서: PreSync → Sync → PostSync")
	fmt.Println("  3. Wave: 각 phase 내에서 0, 1, 2... 순서로 실행")
	fmt.Println("  4. Prune: 생성 순서 역순으로 삭제 (의존성 안전)")
	fmt.Println("  5. PruneLast: 모든 sync 완료 후 마지막에 삭제")
	fmt.Println("  6. Hook generateName: revision+phase+timestamp로 고유 이름")
	fmt.Println("  7. SyncFail phase: sync 실패 시 롤백/알림 훅 실행")
}
