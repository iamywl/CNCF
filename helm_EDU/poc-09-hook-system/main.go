package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Helm Hook 시스템 PoC
// =============================================================================
//
// 참조: pkg/release/v1/hook.go, pkg/action/hooks.go
//
// Helm Hook은 릴리스 라이프사이클의 특정 시점에 실행되는 리소스이다.
// 이 PoC는 다음을 시뮬레이션한다:
//   1. HookEvent: pre-install, post-install, pre-upgrade 등 라이프사이클 이벤트
//   2. Weight 기반 정렬: 동일 이벤트 내에서 실행 순서 결정
//   3. HookDeletePolicy: 훅 리소스의 삭제 정책 (before-hook-creation, hook-succeeded, hook-failed)
//   4. 훅 실행 엔진: 이벤트별 훅 필터링 → Weight 정렬 → 순차 실행 → 삭제 정책 적용
// =============================================================================

// --- HookEvent: 훅이 실행되는 라이프사이클 이벤트 ---
// Helm 소스: pkg/release/v1/hook.go의 HookEvent 타입
type HookEvent string

const (
	HookPreInstall   HookEvent = "pre-install"
	HookPostInstall  HookEvent = "post-install"
	HookPreDelete    HookEvent = "pre-delete"
	HookPostDelete   HookEvent = "post-delete"
	HookPreUpgrade   HookEvent = "pre-upgrade"
	HookPostUpgrade  HookEvent = "post-upgrade"
	HookPreRollback  HookEvent = "pre-rollback"
	HookPostRollback HookEvent = "post-rollback"
	HookTest         HookEvent = "test"
)

func (e HookEvent) String() string { return string(e) }

// --- HookDeletePolicy: 훅 리소스를 언제 삭제할지 결정 ---
// Helm 소스: pkg/release/v1/hook.go의 HookDeletePolicy 타입
type HookDeletePolicy string

const (
	HookSucceeded          HookDeletePolicy = "hook-succeeded"
	HookFailed             HookDeletePolicy = "hook-failed"
	HookBeforeHookCreation HookDeletePolicy = "before-hook-creation"
)

func (p HookDeletePolicy) String() string { return string(p) }

// --- HookPhase: 훅 실행 상태 ---
// Helm 소스: pkg/release/v1/hook.go의 HookPhase 타입
type HookPhase string

const (
	HookPhaseUnknown   HookPhase = "Unknown"
	HookPhaseRunning   HookPhase = "Running"
	HookPhaseSucceeded HookPhase = "Succeeded"
	HookPhaseFailed    HookPhase = "Failed"
)

func (p HookPhase) String() string { return string(p) }

// --- HookExecution: 훅 실행 결과 기록 ---
// Helm 소스: pkg/release/v1/hook.go의 HookExecution 구조체
type HookExecution struct {
	StartedAt   time.Time
	CompletedAt time.Time
	Phase       HookPhase
}

// --- Hook: 훅 리소스 정의 ---
// Helm 소스: pkg/release/v1/hook.go의 Hook 구조체
// 실제 Helm에서는 Kubernetes 매니페스트 annotation으로 훅을 정의한다:
//   helm.sh/hook: pre-install
//   helm.sh/hook-weight: "5"
//   helm.sh/hook-delete-policy: hook-succeeded
type Hook struct {
	Name           string             // 훅 리소스 이름
	Kind           string             // Kubernetes 리소스 종류 (Job, Pod 등)
	Path           string             // 차트 내 템플릿 경로
	Manifest       string             // 렌더링된 매니페스트 내용
	Events         []HookEvent        // 이 훅이 실행될 이벤트 목록
	LastRun        HookExecution      // 마지막 실행 결과
	Weight         int                // 실행 순서 (낮을수록 먼저 실행)
	DeletePolicies []HookDeletePolicy // 훅 삭제 정책
}

// --- Release: 릴리스 정보 (훅 보유) ---
type Release struct {
	Name      string
	Namespace string
	Hooks     []*Hook
	Manifest  string
}

// --- hookByWeight: 훅을 Weight로 정렬하는 소터 ---
// Helm 소스: pkg/action/hooks.go의 hookByWeight
// Weight가 같으면 이름 순서로 정렬 (안정 정렬)
type hookByWeight []*Hook

func (x hookByWeight) Len() int      { return len(x) }
func (x hookByWeight) Swap(i, j int) { x[i], x[j] = x[j], x[i] }
func (x hookByWeight) Less(i, j int) bool {
	if x[i].Weight == x[j].Weight {
		return x[i].Name < x[j].Name
	}
	return x[i].Weight < x[j].Weight
}

// --- HookEngine: 훅 실행 엔진 ---
// Helm 소스: pkg/action/hooks.go의 execHook/execHookWithDelayedShutdown
type HookEngine struct {
	// 시뮬레이션을 위한 실패 확률 (0.0 ~ 1.0)
	FailureRate float64
	// 삭제된 리소스 추적
	DeletedHooks []string
	// 실행 로그
	ExecutionLog []string
}

// NewHookEngine은 새로운 훅 엔진을 생성한다.
func NewHookEngine(failureRate float64) *HookEngine {
	return &HookEngine{
		FailureRate:  failureRate,
		DeletedHooks: make([]string, 0),
		ExecutionLog: make([]string, 0),
	}
}

// log는 실행 로그를 기록한다.
func (e *HookEngine) log(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e.ExecutionLog = append(e.ExecutionLog, msg)
	fmt.Println("  " + msg)
}

// hookHasDeletePolicy는 훅이 특정 삭제 정책을 갖고 있는지 확인한다.
// Helm 소스: pkg/action/hooks.go의 hookHasDeletePolicy
func (e *HookEngine) hookHasDeletePolicy(h *Hook, policy HookDeletePolicy) bool {
	for _, p := range h.DeletePolicies {
		if p == policy {
			return true
		}
	}
	return false
}

// hookSetDefaultDeletePolicy는 삭제 정책이 없으면 기본값을 설정한다.
// Helm 소스: pkg/action/hooks.go의 hookSetDeletePolicy
// 기본 정책: before-hook-creation — 이전 훅 리소스를 삭제 후 새로 생성
func (e *HookEngine) hookSetDefaultDeletePolicy(h *Hook) {
	if len(h.DeletePolicies) == 0 {
		h.DeletePolicies = []HookDeletePolicy{HookBeforeHookCreation}
	}
}

// deleteHookByPolicy는 정책에 따라 훅 리소스를 삭제한다.
// Helm 소스: pkg/action/hooks.go의 deleteHookByPolicy
// CustomResourceDefinition은 cascading garbage collection 방지를 위해 삭제하지 않는다.
func (e *HookEngine) deleteHookByPolicy(h *Hook, policy HookDeletePolicy) error {
	// CRD는 절대 삭제하지 않는다 (Helm 실제 동작)
	if h.Kind == "CustomResourceDefinition" {
		e.log("[삭제 건너뜀] %s — CRD는 삭제하지 않음", h.Name)
		return nil
	}
	if e.hookHasDeletePolicy(h, policy) {
		e.log("[삭제] %s (정책: %s)", h.Name, policy)
		e.DeletedHooks = append(e.DeletedHooks, h.Name)
	}
	return nil
}

// simulateHookExecution은 훅 실행을 시뮬레이션한다.
func (e *HookEngine) simulateHookExecution(h *Hook) error {
	// 랜덤 실패 시뮬레이션
	if rand.Float64() < e.FailureRate {
		return fmt.Errorf("hook %q 실행 실패 (시뮬레이션)", h.Name)
	}
	return nil
}

// ExecHook은 주어진 이벤트에 해당하는 모든 훅을 실행한다.
// Helm 소스: pkg/action/hooks.go의 execHookWithDelayedShutdown 핵심 로직:
//
//  1. 릴리스의 모든 훅에서 해당 이벤트를 가진 훅만 필터링
//  2. Weight로 안정 정렬 (sort.Stable)
//  3. 각 훅에 대해:
//     a. 기본 삭제 정책 설정 (before-hook-creation)
//     b. before-hook-creation 정책이면 기존 리소스 삭제
//     c. 리소스 생성 및 실행
//     d. 실행 결과(성공/실패)에 따라 삭제 정책 적용
func (e *HookEngine) ExecHook(rl *Release, event HookEvent) error {
	fmt.Printf("\n{'='*60}\n")
	fmt.Printf("훅 실행: 이벤트=%s, 릴리스=%s\n", event, rl.Name)
	fmt.Println(strings.Repeat("=", 60))

	// 1단계: 이벤트에 해당하는 훅 필터링
	// Helm 소스에서는 rl.Hooks를 순회하며 Events에 해당 이벤트가 있는지 확인
	executingHooks := []*Hook{}
	for _, h := range rl.Hooks {
		for _, evt := range h.Events {
			if evt == event {
				executingHooks = append(executingHooks, h)
			}
		}
	}

	if len(executingHooks) == 0 {
		e.log("이벤트 %s에 해당하는 훅 없음", event)
		return nil
	}

	// 2단계: Weight로 안정 정렬
	// Helm 소스: sort.Stable(hookByWeight(executingHooks))
	// Weight가 같으면 이름 순서 유지 (안정 정렬의 핵심)
	sort.Stable(hookByWeight(executingHooks))

	e.log("실행할 훅 %d개 (정렬 후):", len(executingHooks))
	for i, h := range executingHooks {
		e.log("  [%d] %s (weight=%d, kind=%s)", i, h.Name, h.Weight, h.Kind)
	}

	// 3단계: 각 훅 순차 실행
	for i, h := range executingHooks {
		fmt.Printf("\n--- 훅 실행 [%d/%d]: %s ---\n", i+1, len(executingHooks), h.Name)

		// 3a: 기본 삭제 정책 설정
		e.hookSetDefaultDeletePolicy(h)

		// 3b: before-hook-creation 정책 처리
		// 이전에 같은 이름의 훅 리소스가 있으면 먼저 삭제
		if err := e.deleteHookByPolicy(h, HookBeforeHookCreation); err != nil {
			return err
		}

		// 3c: 실행 시작 기록
		h.LastRun = HookExecution{
			StartedAt: time.Now(),
			Phase:     HookPhaseRunning,
		}
		e.log("[실행] %s 시작 (phase=%s)", h.Name, h.LastRun.Phase)

		// 3d: 훅 실행 (시뮬레이션)
		err := e.simulateHookExecution(h)
		h.LastRun.CompletedAt = time.Now()

		if err != nil {
			// 실패 시
			h.LastRun.Phase = HookPhaseFailed
			e.log("[실패] %s: %v", h.Name, err)

			// 실패한 훅에 대해 hook-failed 정책 적용
			if delErr := e.deleteHookByPolicy(h, HookFailed); delErr != nil {
				e.log("[경고] 실패한 훅 삭제 중 오류: %v", delErr)
			}

			// 이전에 성공한 훅들에 대해 hook-succeeded 정책 적용
			for _, prevHook := range executingHooks[:i] {
				if delErr := e.deleteHookByPolicy(prevHook, HookSucceeded); delErr != nil {
					return delErr
				}
			}

			return fmt.Errorf("훅 %s %s 실행 실패: %w", event, h.Path, err)
		}

		// 성공 시
		h.LastRun.Phase = HookPhaseSucceeded
		e.log("[성공] %s (소요시간: %v)", h.Name, h.LastRun.CompletedAt.Sub(h.LastRun.StartedAt))
	}

	// 4단계: 모든 훅 성공 시, 역순으로 hook-succeeded 삭제 정책 적용
	// Helm 소스: 성공 시 역순(len-1 → 0)으로 삭제 처리
	fmt.Println("\n--- 후처리: 성공한 훅 삭제 정책 적용 ---")
	for i := len(executingHooks) - 1; i >= 0; i-- {
		h := executingHooks[i]
		if err := e.deleteHookByPolicy(h, HookSucceeded); err != nil {
			return err
		}
	}

	return nil
}

// =============================================================================
// 시나리오 데모
// =============================================================================

// createSampleRelease는 다양한 훅이 포함된 샘플 릴리스를 생성한다.
func createSampleRelease() *Release {
	return &Release{
		Name:      "my-app",
		Namespace: "default",
		Hooks: []*Hook{
			{
				Name:     "db-init-job",
				Kind:     "Job",
				Path:     "templates/db-init-job.yaml",
				Manifest: "apiVersion: batch/v1\nkind: Job\n...",
				Events:   []HookEvent{HookPreInstall},
				Weight:   0,
				DeletePolicies: []HookDeletePolicy{
					HookBeforeHookCreation,
					HookSucceeded,
				},
			},
			{
				Name:     "migration-job",
				Kind:     "Job",
				Path:     "templates/migration-job.yaml",
				Manifest: "apiVersion: batch/v1\nkind: Job\n...",
				Events:   []HookEvent{HookPreInstall, HookPreUpgrade},
				Weight:   5, // db-init-job 이후 실행
				DeletePolicies: []HookDeletePolicy{
					HookBeforeHookCreation,
					HookSucceeded,
				},
			},
			{
				Name:     "smoke-test",
				Kind:     "Pod",
				Path:     "templates/smoke-test.yaml",
				Manifest: "apiVersion: v1\nkind: Pod\n...",
				Events:   []HookEvent{HookPostInstall, HookPostUpgrade},
				Weight:   0,
				DeletePolicies: []HookDeletePolicy{
					HookSucceeded,
					HookFailed,
				},
			},
			{
				Name:           "setup-crd",
				Kind:           "CustomResourceDefinition",
				Path:           "templates/crd.yaml",
				Manifest:       "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n...",
				Events:         []HookEvent{HookPreInstall},
				Weight:         -10, // 가장 먼저 실행
				DeletePolicies: []HookDeletePolicy{HookSucceeded},
			},
			{
				Name:     "cache-warmup",
				Kind:     "Job",
				Path:     "templates/cache-warmup.yaml",
				Manifest: "apiVersion: batch/v1\nkind: Job\n...",
				Events:   []HookEvent{HookPostInstall},
				Weight:   10, // smoke-test 이후
				DeletePolicies: []HookDeletePolicy{
					HookBeforeHookCreation,
				},
			},
			{
				Name:     "cleanup-job",
				Kind:     "Job",
				Path:     "templates/cleanup-job.yaml",
				Manifest: "apiVersion: batch/v1\nkind: Job\n...",
				Events:   []HookEvent{HookPreDelete},
				Weight:   0,
				DeletePolicies: []HookDeletePolicy{
					HookBeforeHookCreation,
					HookSucceeded,
				},
			},
			{
				Name:           "notify-slack",
				Kind:           "Job",
				Path:           "templates/notify-slack.yaml",
				Manifest:       "apiVersion: batch/v1\nkind: Job\n...",
				Events:         []HookEvent{HookPostInstall, HookPostUpgrade, HookPostRollback},
				Weight:         100, // 항상 마지막
				DeletePolicies: nil, // 기본 정책(before-hook-creation) 적용됨
			},
		},
	}
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              Helm Hook 시스템 PoC                            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("참조: pkg/release/v1/hook.go, pkg/action/hooks.go")
	fmt.Println()

	// =================================================================
	// 1. Hook 이벤트 종류 설명
	// =================================================================
	fmt.Println("1. Hook 이벤트 종류")
	fmt.Println(strings.Repeat("-", 60))
	events := []struct {
		event HookEvent
		desc  string
	}{
		{HookPreInstall, "install 전 — DB 스키마 생성, 시크릿 설정 등"},
		{HookPostInstall, "install 후 — smoke test, 알림 전송 등"},
		{HookPreDelete, "delete 전 — 데이터 백업, 연결 해제 등"},
		{HookPostDelete, "delete 후 — 리소스 정리, 알림 등"},
		{HookPreUpgrade, "upgrade 전 — DB 마이그레이션 등"},
		{HookPostUpgrade, "upgrade 후 — 검증, 알림 등"},
		{HookPreRollback, "rollback 전 — 상태 저장 등"},
		{HookPostRollback, "rollback 후 — 복구 검증 등"},
		{HookTest, "helm test 실행 시"},
	}
	for _, e := range events {
		fmt.Printf("  %-16s : %s\n", e.event, e.desc)
	}

	// =================================================================
	// 2. Hook 삭제 정책 설명
	// =================================================================
	fmt.Println("\n2. Hook 삭제 정책 (HookDeletePolicy)")
	fmt.Println(strings.Repeat("-", 60))
	policies := []struct {
		policy HookDeletePolicy
		desc   string
	}{
		{HookBeforeHookCreation, "새 훅 생성 전 이전 리소스 삭제 (기본값)"},
		{HookSucceeded, "훅 실행 성공 시 리소스 삭제"},
		{HookFailed, "훅 실행 실패 시 리소스 삭제"},
	}
	for _, p := range policies {
		fmt.Printf("  %-25s : %s\n", p.policy, p.desc)
	}

	// =================================================================
	// 3. Weight 기반 정렬 데모
	// =================================================================
	fmt.Println("\n3. Weight 기반 정렬 데모")
	fmt.Println(strings.Repeat("-", 60))

	hooks := []*Hook{
		{Name: "beta-job", Weight: 5},
		{Name: "alpha-job", Weight: 5},
		{Name: "gamma-job", Weight: -1},
		{Name: "delta-job", Weight: 10},
		{Name: "epsilon-job", Weight: 0},
	}

	fmt.Println("정렬 전:")
	for _, h := range hooks {
		fmt.Printf("  %s (weight=%d)\n", h.Name, h.Weight)
	}

	// Helm은 sort.Stable을 사용한다 — 같은 Weight일 때 이름으로 결정
	sort.Stable(hookByWeight(hooks))

	fmt.Println("정렬 후 (Weight 오름차순, 동일 Weight는 이름순):")
	for i, h := range hooks {
		fmt.Printf("  [%d] %s (weight=%d)\n", i, h.Name, h.Weight)
	}

	// =================================================================
	// 4. 전체 훅 실행 시나리오: helm install
	// =================================================================
	fmt.Println("\n4. helm install 시나리오 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("릴리스 라이프사이클:")
	fmt.Println("  pre-install 훅 실행 → 리소스 생성 → post-install 훅 실행")

	rel := createSampleRelease()
	engine := NewHookEngine(0.0) // 실패 없음

	// pre-install 훅 실행
	if err := engine.ExecHook(rel, HookPreInstall); err != nil {
		fmt.Printf("pre-install 훅 실패: %v\n", err)
	}

	fmt.Println("\n  [메인 리소스 배포 시뮬레이션...]")

	// post-install 훅 실행
	if err := engine.ExecHook(rel, HookPostInstall); err != nil {
		fmt.Printf("post-install 훅 실패: %v\n", err)
	}

	// =================================================================
	// 5. 훅 실패 시나리오
	// =================================================================
	fmt.Println("\n5. 훅 실패 시나리오 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	failRel := &Release{
		Name:      "fail-app",
		Namespace: "staging",
		Hooks: []*Hook{
			{
				Name:     "setup-job",
				Kind:     "Job",
				Path:     "templates/setup.yaml",
				Events:   []HookEvent{HookPreInstall},
				Weight:   0,
				DeletePolicies: []HookDeletePolicy{
					HookBeforeHookCreation,
					HookSucceeded,
				},
			},
			{
				Name:     "failing-job",
				Kind:     "Job",
				Path:     "templates/failing.yaml",
				Events:   []HookEvent{HookPreInstall},
				Weight:   5,
				DeletePolicies: []HookDeletePolicy{
					HookFailed, // 실패 시 삭제
				},
			},
		},
	}

	failEngine := NewHookEngine(0.0)
	// 두 번째 훅(failing-job)만 강제 실패시키기 위해 실패율을 높임
	// 실제로는 모든 훅에 동일한 실패율이 적용되므로, 직접 에러를 주입
	failEngine.FailureRate = 1.0 // 100% 실패

	// Weight 0인 setup-job이 먼저 실행되는데, 1.0 실패율이면 이것도 실패한다
	// 따라서 실제 Helm의 동작처럼 순차 실행 중 첫 실패에서 중단됨을 보여줌
	if err := failEngine.ExecHook(failRel, HookPreInstall); err != nil {
		fmt.Printf("\n  훅 실행 오류 (예상된 실패): %v\n", err)
	}

	// =================================================================
	// 6. CRD 훅은 삭제되지 않는 것을 보여줌
	// =================================================================
	fmt.Println("\n6. CRD 훅 보호 메커니즘")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("CustomResourceDefinition 타입의 훅은 cascading garbage collection")
	fmt.Println("방지를 위해 삭제 정책이 있어도 삭제하지 않는다.")

	crdEngine := NewHookEngine(0.0)
	crdHook := &Hook{
		Name:           "my-crd",
		Kind:           "CustomResourceDefinition",
		DeletePolicies: []HookDeletePolicy{HookSucceeded},
	}
	jobHook := &Hook{
		Name:           "my-job",
		Kind:           "Job",
		DeletePolicies: []HookDeletePolicy{HookSucceeded},
	}

	fmt.Println("\n  CRD 훅 삭제 시도:")
	crdEngine.deleteHookByPolicy(crdHook, HookSucceeded)
	fmt.Println("\n  Job 훅 삭제 시도:")
	crdEngine.deleteHookByPolicy(jobHook, HookSucceeded)

	// =================================================================
	// 7. 실행 결과 요약
	// =================================================================
	fmt.Println("\n7. 실행 결과 요약")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("삭제된 리소스:")
	for _, name := range engine.DeletedHooks {
		fmt.Printf("  - %s\n", name)
	}

	fmt.Println("\n훅 최종 상태:")
	for _, h := range rel.Hooks {
		if h.LastRun.Phase != "" {
			fmt.Printf("  %-20s : phase=%-10s weight=%-3d events=%v\n",
				h.Name, h.LastRun.Phase, h.Weight, h.Events)
		}
	}

	// =================================================================
	// 8. Helm 훅 시스템 아키텍처 다이어그램
	// =================================================================
	fmt.Println("\n8. Helm Hook 실행 흐름")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  helm install/upgrade/delete
         |
         v
  ┌─────────────────────────┐
  │  Release.Hooks 순회      │
  │  이벤트 매칭 훅 필터링    │
  └────────┬────────────────┘
           |
           v
  ┌─────────────────────────┐
  │  sort.Stable(Weight)     │
  │  Weight 오름차순 정렬     │
  │  동일 Weight → 이름순     │
  └────────┬────────────────┘
           |
           v
  ┌─────────────────────────────────────────┐
  │  각 훅에 대해 순차 실행:                  │
  │                                          │
  │  1. 기본 삭제 정책 설정                   │
  │     (없으면 before-hook-creation)         │
  │                                          │
  │  2. before-hook-creation 정책 시          │
  │     → 이전 리소스 삭제                    │
  │                                          │
  │  3. 리소스 생성 (KubeClient.Create)       │
  │                                          │
  │  4. 완료 대기 (WatchUntilReady)           │
  │                                          │
  │  5-a. 성공 → Phase=Succeeded              │
  │       모든 훅 완료 후 hook-succeeded 삭제  │
  │                                          │
  │  5-b. 실패 → Phase=Failed                 │
  │       hook-failed 정책 적용               │
  │       이전 성공 훅에 hook-succeeded 적용   │
  │       에러 반환 (실행 중단)                │
  └─────────────────────────────────────────┘
`)
}
