// poc-03-reconciliation/main.go
//
// Argo CD Application Controller Reconciliation Loop 시뮬레이션
//
// 핵심 개념:
//   - WorkQueue with rate limiting (appRefreshQueue, appOperationQueue)
//   - CompareWith levels: Nothing(0), Recent(1), Latest(2), LatestForceResolve(3)
//   - needRefreshAppStatus() 로직: 어노테이션 갱신, 소스 변경, 타임아웃
//   - processAppRefreshQueueItem(): CompareAppState → autoSync → persistStatus
//   - processAppOperationQueueItem(): 인포머가 아닌 API에서 직접 앱 조회
//   - autoSync() 7가지 가드 조건
//   - Self-heal 백오프
//   - 전체 reconciliation 사이클 타이밍
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// =============================================================================
// 1. CompareWith 레벨
//    소스: controller/appcontroller.go — CompareWith type
// =============================================================================

// CompareWith는 앱 상태 비교의 깊이를 나타낸다.
// 소스: controller/appcontroller.go
//
//	type CompareWith int
//	const (
//	    CompareWithNothing          CompareWith = iota // 0 — 비교 생략
//	    CompareWithRecent                              // 1 — 캐시된 최신 매니페스트 사용
//	    CompareWithLatest                              // 2 — 항상 최신 Git 조회
//	    CompareWithLatestForceResolve                  // 3 — Git 태그/브랜치 강제 재해석
//	)
type CompareWith int

const (
	CompareWithNothing             CompareWith = iota // 0
	CompareWithRecent                                 // 1 — 캐시된 매니페스트 사용
	CompareWithLatest                                 // 2 — Git HEAD 최신 조회
	CompareWithLatestForceResolve                     // 3 — 브랜치/태그 강제 재해석
)

func (c CompareWith) String() string {
	switch c {
	case CompareWithNothing:
		return "Nothing(0)"
	case CompareWithRecent:
		return "Recent(1)"
	case CompareWithLatest:
		return "Latest(2)"
	case CompareWithLatestForceResolve:
		return "LatestForceResolve(3)"
	default:
		return fmt.Sprintf("Unknown(%d)", int(c))
	}
}

// =============================================================================
// 2. Rate Limiting WorkQueue
//    소스: controller/appcontroller.go — appRefreshQueue, appOperationQueue
//    실제: k8s.io/client-go/util/workqueue.NewRateLimitingQueue()
// =============================================================================

// rateLimitedItem은 워크큐에서 처리할 항목이다.
type rateLimitedItem struct {
	key       string      // 앱 식별자 (namespace/name)
	compareWith CompareWith // 비교 레벨
	enqueueAt time.Time   // 큐에 넣은 시각
	attempt   int         // 재시도 횟수
}

// rateLimitingQueue는 rate limiting 기능이 있는 워크큐를 시뮬레이션한다.
// 실제 client-go의 RateLimitingQueue는 토큰버킷 알고리즘을 사용한다.
// 소스: controller/appcontroller.go
//
//	appRefreshQueue:  workqueue.NewRateLimitingQueueWithConfig(...)
//	appOperationQueue: workqueue.NewRateLimitingQueueWithConfig(...)
type rateLimitingQueue struct {
	name    string
	items   chan rateLimitedItem
	mu      sync.Mutex
	// 처리 통계
	processed int
	retries   int
}

func newRateLimitingQueue(name string, size int) *rateLimitingQueue {
	return &rateLimitingQueue{
		name:  name,
		items: make(chan rateLimitedItem, size),
	}
}

// add는 항목을 즉시 큐에 추가한다.
func (q *rateLimitingQueue) add(key string, compareWith CompareWith) {
	q.mu.Lock()
	defer q.mu.Unlock()
	select {
	case q.items <- rateLimitedItem{key: key, compareWith: compareWith, enqueueAt: time.Now()}:
	default:
		// 큐가 가득 찼으면 버린다 (실제 client-go는 deduplication 수행)
		fmt.Printf("[%s] 큐 가득 참, %s 드랍\n", q.name, key)
	}
}

// addAfter는 지연 후 큐에 추가한다 (재시도 백오프).
func (q *rateLimitingQueue) addAfter(key string, compareWith CompareWith, delay time.Duration, attempt int) {
	go func() {
		time.Sleep(delay)
		q.mu.Lock()
		defer q.mu.Unlock()
		select {
		case q.items <- rateLimitedItem{
			key:         key,
			compareWith: compareWith,
			enqueueAt:   time.Now(),
			attempt:     attempt,
		}:
		default:
		}
	}()
}

func (q *rateLimitingQueue) get() (rateLimitedItem, bool) {
	select {
	case item := <-q.items:
		q.mu.Lock()
		q.processed++
		q.mu.Unlock()
		return item, true
	default:
		return rateLimitedItem{}, false
	}
}

func (q *rateLimitingQueue) len() int {
	return len(q.items)
}

// =============================================================================
// 3. 앱 상태 모델 (간소화)
// =============================================================================

// SyncStatusCode는 동기화 상태다.
type SyncStatusCode string

const (
	SyncStatusSynced    SyncStatusCode = "Synced"
	SyncStatusOutOfSync SyncStatusCode = "OutOfSync"
	SyncStatusUnknown   SyncStatusCode = "Unknown"
)

// HealthStatusCode는 헬스 상태다.
type HealthStatusCode string

const (
	HealthHealthy     HealthStatusCode = "Healthy"
	HealthProgressing HealthStatusCode = "Progressing"
	HealthDegraded    HealthStatusCode = "Degraded"
	HealthMissing     HealthStatusCode = "Missing"
	HealthUnknown     HealthStatusCode = "Unknown"
)

// AppState는 Application의 현재 상태를 시뮬레이션한다.
type AppState struct {
	Name            string
	Namespace       string
	Project         string
	SyncStatus      SyncStatusCode
	HealthStatus    HealthStatusCode
	// 마지막 성공 sync 리비전
	ObservedRevision string
	// Git의 현재 HEAD 리비전
	GitRevision      string
	// 자동 동기화 정책
	AutoSync         bool
	SelfHeal         bool
	Prune            bool
	// 마지막 상태 갱신 시각
	ReconciledAt     time.Time
	// 어노테이션 기반 강제 갱신 마킹
	RefreshAnnotation string
	// 자동 sync 간격 (0이면 비활성)
	RefreshInterval  time.Duration
	// self-heal 마지막 시도 시각
	SelfHealAt       *time.Time
	// Sync 실패 재시도 카운터
	SyncRetryCount   int
	// Operation 대기 여부
	HasOperation     bool
}

// =============================================================================
// 4. needRefreshAppStatus() 로직
//    소스: controller/appcontroller.go — needRefreshAppStatus()
// =============================================================================

// RefreshReason은 앱 갱신이 필요한 이유다.
type RefreshReason string

const (
	RefreshReasonAnnotation    RefreshReason = "forced-refresh-annotation"
	RefreshReasonSourceChanged RefreshReason = "git-source-changed"
	RefreshReasonTimeout       RefreshReason = "refresh-timeout"
	RefreshReasonNeverReconciled RefreshReason = "never-reconciled"
	RefreshReasonOperationSet  RefreshReason = "operation-set"
)

// needRefreshAppStatus는 앱 상태 갱신이 필요한지 판단한다.
// 소스: controller/appcontroller.go — needRefreshAppStatus()
//
//	func (ctrl *ApplicationController) needRefreshAppStatus(app *v1alpha1.Application, statusRefreshTimeout, statusHardRefreshTimeout time.Duration) (bool, CompareWith, v1alpha1.RefreshType) {
//	    // 1. 강제 갱신 어노테이션 확인
//	    // 2. 소스 변경 확인 (Spec vs ObservedHash)
//	    // 3. 타임아웃 확인
//	    // 4. 처음 조정이면 무조건 갱신
//	}
func needRefreshAppStatus(app *AppState, refreshTimeout time.Duration) (bool, CompareWith, RefreshReason) {
	// 1. 강제 갱신 어노테이션 확인
	// 실제: annotations[common.AnnotationKeyRefresh] == "hard" or "normal"
	if app.RefreshAnnotation == "hard" {
		return true, CompareWithLatestForceResolve, RefreshReasonAnnotation
	}
	if app.RefreshAnnotation == "normal" {
		return true, CompareWithLatest, RefreshReasonAnnotation
	}

	// 2. 처음 reconcile이면 무조건 갱신
	if app.ReconciledAt.IsZero() {
		return true, CompareWithLatest, RefreshReasonNeverReconciled
	}

	// 3. Operation이 설정되어 있으면 갱신
	if app.HasOperation {
		return true, CompareWithLatest, RefreshReasonOperationSet
	}

	// 4. Git 리비전이 변경되었으면 갱신 (webhook/polling으로 감지)
	if app.GitRevision != app.ObservedRevision && app.GitRevision != "" {
		return true, CompareWithLatest, RefreshReasonSourceChanged
	}

	// 5. 타임아웃 초과 시 갱신 (기본: 3분)
	if time.Since(app.ReconciledAt) > refreshTimeout {
		return true, CompareWithRecent, RefreshReasonTimeout
	}

	return false, CompareWithNothing, ""
}

// =============================================================================
// 5. autoSync() 가드 조건
//    소스: controller/appcontroller.go — autoSync()
// =============================================================================

// AutoSyncGuardResult는 autoSync 가드 검사 결과다.
type AutoSyncGuardResult struct {
	Blocked bool
	Reason  string
}

// checkAutoSyncGuards는 자동 동기화를 차단하는 7가지 조건을 검사한다.
// 소스: controller/appcontroller.go — autoSync()
//
// 실제 코드의 가드 조건:
//  1. autoSync 정책이 비활성화됨
//  2. 이미 Synced 상태 (+ 리비전 동일)
//  3. Operation이 이미 진행 중
//  4. SyncWindow가 허용 안 함
//  5. selfHeal 없는데 self-heal 상황
//  6. selfHeal 있는데 백오프 시간이 지나지 않음
//  7. allowEmpty=false인데 매니페스트가 비어있음
func checkAutoSyncGuards(app *AppState, syncWindow string, manifestCount int) []AutoSyncGuardResult {
	guards := []AutoSyncGuardResult{}

	// 가드 1: autoSync 정책 비활성화
	if !app.AutoSync {
		guards = append(guards, AutoSyncGuardResult{
			Blocked: true,
			Reason:  "[Guard 1] autoSync 정책이 비활성화됨",
		})
		return guards // 이후 가드 불필요
	}
	guards = append(guards, AutoSyncGuardResult{Blocked: false, Reason: "[Guard 1] autoSync 활성화 — 통과"})

	// 가드 2: 이미 Synced 상태이고 리비전이 동일함
	if app.SyncStatus == SyncStatusSynced && app.GitRevision == app.ObservedRevision {
		guards = append(guards, AutoSyncGuardResult{
			Blocked: true,
			Reason:  "[Guard 2] 이미 Synced 상태, 리비전 동일 → skip",
		})
		return guards
	}
	guards = append(guards, AutoSyncGuardResult{Blocked: false, Reason: "[Guard 2] OutOfSync 또는 리비전 변경 — 통과"})

	// 가드 3: Operation 이미 진행 중
	if app.HasOperation {
		guards = append(guards, AutoSyncGuardResult{
			Blocked: true,
			Reason:  "[Guard 3] Operation 이미 진행 중 → skip",
		})
		return guards
	}
	guards = append(guards, AutoSyncGuardResult{Blocked: false, Reason: "[Guard 3] Operation 없음 — 통과"})

	// 가드 4: SyncWindow 확인
	if syncWindow == "deny" {
		guards = append(guards, AutoSyncGuardResult{
			Blocked: true,
			Reason:  "[Guard 4] SyncWindow deny 활성 — 차단",
		})
		return guards
	}
	guards = append(guards, AutoSyncGuardResult{Blocked: false, Reason: "[Guard 4] SyncWindow 허용 — 통과"})

	// 가드 5: selfHeal 없는데 클러스터가 직접 변경된 상황
	// 실제: isSelfHealTriggered() 함수로 판단
	if !app.SelfHeal && app.SyncStatus == SyncStatusOutOfSync && app.GitRevision == app.ObservedRevision {
		guards = append(guards, AutoSyncGuardResult{
			Blocked: true,
			Reason:  "[Guard 5] selfHeal 비활성, 클러스터 직접 변경 — 차단",
		})
		return guards
	}
	guards = append(guards, AutoSyncGuardResult{Blocked: false, Reason: "[Guard 5] selfHeal 조건 — 통과"})

	// 가드 6: selfHeal 백오프 (마지막 시도 후 5초 경과 필요)
	// 소스: controller/appcontroller.go — selfHealBackoff
	if app.SelfHeal && app.SelfHealAt != nil {
		elapsed := time.Since(*app.SelfHealAt)
		if elapsed < 5*time.Second {
			guards = append(guards, AutoSyncGuardResult{
				Blocked: true,
				Reason:  fmt.Sprintf("[Guard 6] selfHeal 백오프: %v 전 시도, 5s 대기 필요 — 차단", elapsed.Round(time.Millisecond)),
			})
			return guards
		}
	}
	guards = append(guards, AutoSyncGuardResult{Blocked: false, Reason: "[Guard 6] selfHeal 백오프 — 통과"})

	// 가드 7: allowEmpty=false인데 매니페스트가 비어있음
	// 소스: 실제 코드에서 len(targetObjs) == 0 && !app.Spec.SyncPolicy.Automated.AllowEmpty
	if manifestCount == 0 {
		guards = append(guards, AutoSyncGuardResult{
			Blocked: true,
			Reason:  "[Guard 7] 매니페스트 비어있음, allowEmpty=false — 차단",
		})
		return guards
	}
	guards = append(guards, AutoSyncGuardResult{Blocked: false, Reason: "[Guard 7] 매니페스트 있음 — 통과"})

	return guards
}

// =============================================================================
// 6. Application Controller
// =============================================================================

// ApplicationController는 Argo CD의 핵심 조정 루프를 시뮬레이션한다.
// 소스: controller/appcontroller.go — ApplicationController struct
type ApplicationController struct {
	// 상태 갱신이 필요한 앱 큐
	appRefreshQueue *rateLimitingQueue
	// Sync 작업이 필요한 앱 큐
	appOperationQueue *rateLimitingQueue
	// 앱 상태 저장소 (실제: K8s Informer 캐시)
	apps map[string]*AppState
	mu   sync.RWMutex
	// 통계
	reconciledCount int
	autoSyncCount   int
	// 타임아웃 설정
	refreshTimeout time.Duration // 기본: 3분, 시뮬레이션: 짧게
}

func newApplicationController() *ApplicationController {
	return &ApplicationController{
		appRefreshQueue:   newRateLimitingQueue("appRefreshQueue", 100),
		appOperationQueue: newRateLimitingQueue("appOperationQueue", 100),
		apps:              make(map[string]*AppState),
		refreshTimeout:    200 * time.Millisecond, // 시뮬레이션용 짧은 타임아웃
	}
}

func (ctrl *ApplicationController) addApp(app *AppState) {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	ctrl.apps[app.Namespace+"/"+app.Name] = app
}

func (ctrl *ApplicationController) getApp(key string) *AppState {
	ctrl.mu.RLock()
	defer ctrl.mu.RUnlock()
	return ctrl.apps[key]
}

// processAppRefreshQueueItem은 appRefreshQueue의 항목을 처리한다.
// 소스: controller/appcontroller.go — processAppRefreshQueueItem()
//
//	func (ctrl *ApplicationController) processAppRefreshQueueItem() (processNext bool) {
//	    // 1. 큐에서 app key 꺼내기
//	    // 2. appLister에서 앱 조회 (informer 캐시)
//	    // 3. needRefreshAppStatus() 확인
//	    // 4. CompareAppState() 실행 (Git vs live 비교)
//	    // 5. autoSync() 실행
//	    // 6. persistAppStatus() 저장
//	}
func (ctrl *ApplicationController) processAppRefreshQueueItem(item rateLimitedItem) {
	app := ctrl.getApp(item.key)
	if app == nil {
		fmt.Printf("[Refresh] 앱 없음: %s\n", item.key)
		return
	}

	waitTime := time.Since(item.enqueueAt)
	fmt.Printf("[Refresh] 처리 시작: %s | compareWith=%s | 큐 대기: %v\n",
		item.key, item.compareWith, waitTime.Round(time.Millisecond))

	// needRefreshAppStatus 재확인 (큐에서 꺼낼 때 상태가 바뀌었을 수 있음)
	needRefresh, compareWith, reason := needRefreshAppStatus(app, ctrl.refreshTimeout)
	if !needRefresh && item.compareWith == CompareWithNothing {
		fmt.Printf("[Refresh] 갱신 불필요: %s — skip\n", item.key)
		return
	}
	if item.compareWith > compareWith {
		compareWith = item.compareWith // 더 강한 비교 레벨 사용
	}
	fmt.Printf("[Refresh] 갱신 이유: %s, 비교 레벨: %s\n", reason, compareWith)

	// CompareAppState 실행 (Git 매니페스트 vs 클러스터 live state 비교)
	compareResult := ctrl.compareAppState(app, compareWith)

	// 상태 갱신
	ctrl.mu.Lock()
	app.SyncStatus = compareResult.syncStatus
	app.HealthStatus = compareResult.healthStatus
	app.ObservedRevision = compareResult.revision
	now := time.Now()
	app.ReconciledAt = now
	app.RefreshAnnotation = "" // 어노테이션 초기화
	ctrl.reconciledCount++
	ctrl.mu.Unlock()

	fmt.Printf("[Refresh] CompareAppState 완료: sync=%s, health=%s, revision=%s\n",
		app.SyncStatus, app.HealthStatus, app.ObservedRevision)

	// autoSync 실행
	if app.AutoSync {
		ctrl.autoSync(app, compareResult.manifestCount)
	}

	// persistAppStatus (실제: Kubernetes API 서버에 Status 업데이트)
	fmt.Printf("[Refresh] persistAppStatus: %s → sync=%s, health=%s\n",
		app.Name, app.SyncStatus, app.HealthStatus)
}

// compareResult는 CompareAppState의 결과다.
type compareResult struct {
	syncStatus    SyncStatusCode
	healthStatus  HealthStatusCode
	revision      string
	manifestCount int
	diffs         []string
}

// compareAppState는 Git 매니페스트와 클러스터 live state를 비교한다.
// 소스: controller/appcontroller.go — compareAppState()
// 실제: Repo Server에서 매니페스트를 받아 kubectl get으로 조회한 live state와 비교
func (ctrl *ApplicationController) compareAppState(app *AppState, compareWith CompareWith) compareResult {
	// Repo Server 호출 시뮬레이션 (매니페스트 생성)
	switch compareWith {
	case CompareWithLatestForceResolve:
		time.Sleep(50 * time.Millisecond) // 브랜치 재해석 + Git 조회
		fmt.Printf("[Compare] Git HEAD 강제 재해석 + 최신 매니페스트 조회\n")
	case CompareWithLatest:
		time.Sleep(30 * time.Millisecond) // Git HEAD 조회
		fmt.Printf("[Compare] 최신 Git HEAD 조회\n")
	case CompareWithRecent:
		time.Sleep(10 * time.Millisecond) // 캐시 사용
		fmt.Printf("[Compare] 캐시된 매니페스트 사용\n")
	}

	// 시뮬레이션: Git 리비전이 다르면 OutOfSync
	if app.GitRevision != "" && app.GitRevision != app.ObservedRevision {
		return compareResult{
			syncStatus:    SyncStatusOutOfSync,
			healthStatus:  HealthHealthy,
			revision:      app.GitRevision,
			manifestCount: 4,
			diffs:         []string{"Deployment/myapp: image tag changed"},
		}
	}
	return compareResult{
		syncStatus:    SyncStatusSynced,
		healthStatus:  HealthHealthy,
		revision:      app.ObservedRevision,
		manifestCount: 4,
	}
}

// autoSync는 자동 동기화를 시도한다.
// 소스: controller/appcontroller.go — autoSync()
func (ctrl *ApplicationController) autoSync(app *AppState, manifestCount int) {
	// 가드 조건 검사
	guards := checkAutoSyncGuards(app, "allow", manifestCount)

	blocked := false
	for _, g := range guards {
		if g.Blocked {
			fmt.Printf("[AutoSync] %s\n", g.Reason)
			blocked = true
			break
		}
	}
	if blocked {
		return
	}

	// 모든 가드 통과 — sync 실행
	fmt.Printf("[AutoSync] 모든 가드 통과 → Sync 트리거\n")
	ctrl.mu.Lock()
	app.HasOperation = true
	ctrl.autoSyncCount++
	ctrl.mu.Unlock()

	// Operation 큐에 추가
	ctrl.appOperationQueue.add(app.Namespace+"/"+app.Name, CompareWithLatest)
}

// processAppOperationQueueItem은 appOperationQueue의 항목을 처리한다.
// 소스: controller/appcontroller.go — processAppOperationQueueItem()
//
// 핵심: 인포머 캐시가 아닌 API 서버에서 직접 최신 앱 상태를 조회한다.
// 이유: sync 도중 상태 변경이 생길 수 있으므로, 항상 최신 상태로 작업해야 한다.
//
//	// 실제 소스 코드:
//	app, err := ctrl.applicationClientset.ArgoprojV1alpha1().
//	    Applications(appNs).Get(ctx, appName, metav1.GetOptions{})
func (ctrl *ApplicationController) processAppOperationQueueItem(item rateLimitedItem) {
	// API 서버에서 직접 조회 (인포머 캐시 사용 안 함)
	fmt.Printf("[Operation] API에서 최신 앱 상태 직접 조회: %s\n", item.key)
	app := ctrl.getApp(item.key) // 시뮬레이션: 동일 소스 사용
	if app == nil {
		return
	}

	if !app.HasOperation {
		fmt.Printf("[Operation] Operation 없음 — skip: %s\n", item.key)
		return
	}

	fmt.Printf("[Operation] Sync 실행: %s → revision=%s\n", app.Name, app.GitRevision)
	time.Sleep(80 * time.Millisecond) // kubectl apply 시뮬레이션

	// 성공
	ctrl.mu.Lock()
	app.SyncStatus = SyncStatusSynced
	app.HealthStatus = HealthProgressing // apply 후 배포 중
	app.ObservedRevision = app.GitRevision
	app.HasOperation = false
	now := time.Now()
	app.SelfHealAt = &now
	ctrl.mu.Unlock()

	fmt.Printf("[Operation] Sync 완료: %s → sync=Synced, health=Progressing (배포 중)\n", app.Name)

	// 배포 완료 시뮬레이션 (실제: Deployment rollout watch)
	time.Sleep(100 * time.Millisecond)
	ctrl.mu.Lock()
	app.HealthStatus = HealthHealthy
	ctrl.mu.Unlock()
	fmt.Printf("[Operation] 배포 완료: %s → health=Healthy\n", app.Name)
}

// =============================================================================
// 7. Self-heal 백오프 시뮬레이션
// =============================================================================

// selfHealBackoff는 지수 백오프 계산을 시뮬레이션한다.
// 실제: controller/appcontroller.go — selfHealBackoff 설정
//   - 기본: 5s × 2^n (최대 3분)
func selfHealBackoff(attempt int) time.Duration {
	const (
		base    = 5 * time.Second
		maxWait = 3 * time.Minute
		factor  = 2.0
	)
	wait := time.Duration(float64(base) * math.Pow(factor, float64(attempt)))
	if wait > maxWait {
		wait = maxWait
	}
	return wait
}

// =============================================================================
// 8. 전체 Reconciliation 사이클 시뮬레이션
// =============================================================================

func runReconciliationDemo() {
	fmt.Println("=================================================================")
	fmt.Println(" Argo CD Application Controller Reconciliation 시뮬레이션")
	fmt.Println("=================================================================")
	fmt.Println()

	ctrl := newApplicationController()

	// 테스트 앱 등록
	apps := []*AppState{
		{
			Name:             "myapp",
			Namespace:        "argocd",
			Project:          "default",
			SyncStatus:       SyncStatusUnknown,
			HealthStatus:     HealthUnknown,
			ObservedRevision: "",
			GitRevision:      "a1b2c3d",
			AutoSync:         true,
			SelfHeal:         true,
			Prune:            true,
			RefreshInterval:  200 * time.Millisecond,
		},
		{
			Name:             "legacy-app",
			Namespace:        "argocd",
			Project:          "default",
			SyncStatus:       SyncStatusSynced,
			HealthStatus:     HealthHealthy,
			ObservedRevision: "x9y8z7w",
			GitRevision:      "x9y8z7w",
			AutoSync:         false, // 수동 sync
		},
		{
			Name:             "broken-app",
			Namespace:        "argocd",
			Project:          "default",
			SyncStatus:       SyncStatusOutOfSync,
			HealthStatus:     HealthDegraded,
			ObservedRevision: "old123",
			GitRevision:      "new456",
			AutoSync:         true,
			SelfHeal:         false, // selfHeal 없음
		},
	}

	for _, app := range apps {
		ctrl.addApp(app)
	}

	// --- 1단계: needRefreshAppStatus 시연 ---
	fmt.Println("[ 1단계: needRefreshAppStatus() — 갱신 필요 여부 판단 ]")
	fmt.Println("-----------------------------------------------------------------")

	testCases := []struct {
		appName    string
		annotation string
		desc       string
	}{
		{"myapp", "", "처음 reconcile (ReconciledAt=zero)"},
		{"legacy-app", "", "이미 Synced, 타임아웃 미경과"},
		{"myapp", "hard", "hard refresh 어노테이션"},
		{"broken-app", "", "OutOfSync 상태"},
	}

	for _, tc := range testCases {
		app := ctrl.getApp("argocd/" + tc.appName)
		app.RefreshAnnotation = tc.annotation
		needRefresh, compareWith, reason := needRefreshAppStatus(app, ctrl.refreshTimeout)
		fmt.Printf("  %-50s → needRefresh=%v, %s, reason=%s\n",
			tc.desc, needRefresh, compareWith, reason)
		app.RefreshAnnotation = "" // 초기화
	}
	fmt.Println()

	// --- 2단계: autoSync 가드 조건 시연 ---
	fmt.Println("[ 2단계: autoSync() 7가지 가드 조건 ]")
	fmt.Println("-----------------------------------------------------------------")

	guardTests := []struct {
		app         *AppState
		syncWindow  string
		manifests   int
		scenario    string
	}{
		{
			app:        &AppState{AutoSync: false},
			syncWindow: "allow", manifests: 4,
			scenario: "Guard 1: autoSync 비활성",
		},
		{
			app: &AppState{
				AutoSync: true, SelfHeal: true,
				SyncStatus:       SyncStatusSynced,
				GitRevision:      "abc", ObservedRevision: "abc",
			},
			syncWindow: "allow", manifests: 4,
			scenario: "Guard 2: 이미 Synced + 리비전 동일",
		},
		{
			app: &AppState{
				AutoSync: true, SelfHeal: true,
				SyncStatus:       SyncStatusOutOfSync,
				GitRevision:      "new", ObservedRevision: "old",
				HasOperation: true,
			},
			syncWindow: "allow", manifests: 4,
			scenario: "Guard 3: Operation 이미 진행 중",
		},
		{
			app: &AppState{
				AutoSync: true, SelfHeal: true,
				SyncStatus:   SyncStatusOutOfSync,
				GitRevision:  "new", ObservedRevision: "old",
			},
			syncWindow: "deny", manifests: 4,
			scenario: "Guard 4: SyncWindow deny",
		},
		{
			app: &AppState{
				AutoSync: true, SelfHeal: false,
				SyncStatus:       SyncStatusOutOfSync,
				GitRevision:      "abc", ObservedRevision: "abc", // 리비전 동일 = 클러스터 직접 변경
			},
			syncWindow: "allow", manifests: 4,
			scenario: "Guard 5: selfHeal 없음, 클러스터 직접 변경",
		},
		{
			app: func() *AppState {
				recently := time.Now().Add(-2 * time.Second) // 2초 전 시도
				return &AppState{
					AutoSync: true, SelfHeal: true,
					SyncStatus:       SyncStatusOutOfSync,
					GitRevision:      "new", ObservedRevision: "old",
					SelfHealAt: &recently,
				}
			}(),
			syncWindow: "allow", manifests: 4,
			scenario: "Guard 6: selfHeal 백오프 (2초 전 시도, 5초 대기)",
		},
		{
			app: &AppState{
				AutoSync: true, SelfHeal: true,
				SyncStatus:   SyncStatusOutOfSync,
				GitRevision:  "new", ObservedRevision: "old",
			},
			syncWindow: "allow", manifests: 0,
			scenario: "Guard 7: 빈 매니페스트",
		},
		{
			app: &AppState{
				AutoSync: true, SelfHeal: true,
				SyncStatus:   SyncStatusOutOfSync,
				GitRevision:  "new", ObservedRevision: "old",
			},
			syncWindow: "allow", manifests: 4,
			scenario: "모든 가드 통과 → Sync 허용",
		},
	}

	for _, gt := range guardTests {
		guards := checkAutoSyncGuards(gt.app, gt.syncWindow, gt.manifests)
		lastGuard := guards[len(guards)-1]
		blocked := "차단"
		if !lastGuard.Blocked {
			blocked = "허용"
		}
		fmt.Printf("  %-50s → %s\n", gt.scenario, blocked)
		fmt.Printf("    마지막 가드: %s\n", lastGuard.Reason)
	}
	fmt.Println()

	// --- 3단계: 전체 Reconciliation 사이클 ---
	fmt.Println("[ 3단계: 전체 Reconciliation 사이클 ]")
	fmt.Println("-----------------------------------------------------------------")

	// myapp을 갱신 큐에 추가
	myapp := ctrl.getApp("argocd/myapp")
	ctrl.appRefreshQueue.add("argocd/myapp", CompareWithLatest)
	fmt.Printf("\n[큐] appRefreshQueue에 추가: argocd/myapp (compareWith=Latest)\n")
	fmt.Printf("[큐] 현재 큐 길이: %d\n\n", ctrl.appRefreshQueue.len())

	// Refresh 처리
	if item, ok := ctrl.appRefreshQueue.get(); ok {
		ctrl.processAppRefreshQueueItem(item)
	}
	fmt.Println()

	// Operation 큐 처리
	fmt.Printf("[큐] appOperationQueue 길이: %d\n", ctrl.appOperationQueue.len())
	if item, ok := ctrl.appOperationQueue.get(); ok {
		ctrl.processAppOperationQueueItem(item)
	}
	fmt.Println()

	// --- 4단계: Self-heal 백오프 시연 ---
	fmt.Println("[ 4단계: Self-heal 백오프 계산 ]")
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println("  attempt | 대기 시간")
	for i := 0; i <= 5; i++ {
		wait := selfHealBackoff(i)
		fmt.Printf("    %d     | %v\n", i, wait)
	}
	fmt.Println()

	// --- 5단계: 타임아웃 갱신 시뮬레이션 ---
	fmt.Println("[ 5단계: 주기적 타임아웃 갱신 ]")
	fmt.Println("-----------------------------------------------------------------")
	myapp.ReconciledAt = time.Now().Add(-300 * time.Millisecond) // 300ms 전
	needRefresh, compareWith, reason := needRefreshAppStatus(myapp, ctrl.refreshTimeout)
	fmt.Printf("  refreshTimeout=%v, lastReconcile=%v 전\n",
		ctrl.refreshTimeout, time.Since(myapp.ReconciledAt).Round(time.Millisecond))
	fmt.Printf("  needRefresh=%v, compareWith=%s, reason=%s\n", needRefresh, compareWith, reason)
	fmt.Println()

	// --- 최종 통계 ---
	fmt.Println("[ 최종 통계 ]")
	fmt.Println("-----------------------------------------------------------------")
	fmt.Printf("  총 Reconcile 횟수: %d\n", ctrl.reconciledCount)
	fmt.Printf("  총 AutoSync 트리거: %d\n", ctrl.autoSyncCount)
	fmt.Printf("  appRefreshQueue 처리: %d\n", ctrl.appRefreshQueue.processed)
	fmt.Printf("  appOperationQueue 처리: %d\n", ctrl.appOperationQueue.processed)
	fmt.Printf("  최종 앱 상태:\n")
	ctrl.mu.RLock()
	for key, app := range ctrl.apps {
		fmt.Printf("    %-30s sync=%-12s health=%s\n",
			key, app.SyncStatus, app.HealthStatus)
	}
	ctrl.mu.RUnlock()
}

func main() {
	runReconciliationDemo()
}
