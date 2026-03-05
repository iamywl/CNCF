// poc-04-manifest-generation/main.go
//
// Argo CD Repo Server 매니페스트 생성 시뮬레이션
//
// 핵심 개념:
//   - 소스 타입 감지: Git, Helm, Kustomize, Directory, Plugin
//   - 세마포어 기반 병렬성 제어
//   - 더블체크 락킹 캐시
//   - 에러 캐싱: PauseGenerationAfterFailedAttempts, PauseGenerationOnFailureForMinutes
//   - .argocd-source.yaml 오버라이드 메커니즘
//   - ARGOCD_APP_* 환경변수 주입
//   - 리소스 추적 레이블 주입 (SetAppInstance)
//   - 다양한 소스 타입별 매니페스트 생성
//
// 실행: go run main.go

package main

import (
	"crypto/md5"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. 소스 타입 감지
//    소스: reposerver/repository/repository.go — getSourceType()
// =============================================================================

// SourceType은 배포 도구 타입이다.
type SourceType string

const (
	SourceTypeHelm      SourceType = "Helm"
	SourceTypeKustomize SourceType = "Kustomize"
	SourceTypeDirectory SourceType = "Directory"
	SourceTypePlugin    SourceType = "Plugin"
)

// AppSource는 매니페스트 생성 요청의 소스 정보다.
type AppSource struct {
	RepoURL        string
	Path           string
	TargetRevision string
	// Helm 설정 (nil이면 Helm 미사용)
	Helm *HelmSource
	// Kustomize 설정 (nil이면 Kustomize 미사용)
	Kustomize *KustomizeSource
	// Plugin 설정 (nil이면 Plugin 미사용)
	Plugin *PluginSource
	// Chart (Helm repo의 차트 이름, 설정 시 Helm으로 처리)
	Chart string
}

// HelmSource는 Helm 특화 소스 설정이다.
type HelmSource struct {
	ReleaseName string
	Values      string
	ValueFiles  []string
	Parameters  map[string]string
}

// KustomizeSource는 Kustomize 특화 소스 설정이다.
type KustomizeSource struct {
	Version     string
	NamePrefix  string
	NameSuffix  string
	Images      []string
}

// PluginSource는 Config Management Plugin(CMP) 설정이다.
type PluginSource struct {
	Name string
	Env  map[string]string
}

// detectSourceType은 파일 시스템 구조를 기반으로 소스 타입을 결정한다.
// 소스: reposerver/repository/repository.go — getSourceType()
//
//	func getSourceType(appSourcePath string, appSrc *v1alpha1.ApplicationSource) v1alpha1.ApplicationSourceType {
//	    if appSrc.Chart != "" {
//	        return v1alpha1.ApplicationSourceTypeHelm
//	    }
//	    if appSrc.Kustomize != nil || appSrc.HasKustomize() {
//	        return v1alpha1.ApplicationSourceTypeKustomize
//	    }
//	    if appSrc.Plugin != nil || ...:
//	        return v1alpha1.ApplicationSourceTypePlugin
//	    if appSrc.Helm != nil || fileExists("Chart.yaml"):
//	        return v1alpha1.ApplicationSourceTypeHelm
//	    if fileExists("kustomization.yaml"):
//	        return v1alpha1.ApplicationSourceTypeKustomize
//	    return v1alpha1.ApplicationSourceTypeDirectory
//	}
func detectSourceType(source *AppSource, repoFiles []string) SourceType {
	// 1. Helm chart 레포지토리 (Chart 필드 있으면 무조건 Helm)
	if source.Chart != "" {
		return SourceTypeHelm
	}

	// 2. 명시적 Plugin 설정
	if source.Plugin != nil {
		return SourceTypePlugin
	}

	// 3. 명시적 Helm 설정
	if source.Helm != nil {
		return SourceTypeHelm
	}

	// 4. 명시적 Kustomize 설정
	if source.Kustomize != nil {
		return SourceTypeKustomize
	}

	// 5. 파일 시스템 기반 자동 감지
	fileSet := make(map[string]bool)
	for _, f := range repoFiles {
		fileSet[f] = true
	}

	if fileSet["Chart.yaml"] || fileSet["Chart.yml"] {
		return SourceTypeHelm
	}
	if fileSet["kustomization.yaml"] || fileSet["kustomization.yml"] || fileSet["Kustomization"] {
		return SourceTypeKustomize
	}

	return SourceTypeDirectory
}

// =============================================================================
// 2. .argocd-source.yaml 오버라이드 메커니즘
//    소스: reposerver/repository/repository.go — getSourceType()
//    실제: argocd-source.yaml 파일이 있으면 App의 source 설정을 오버라이드
// =============================================================================

// ArgoCDSourceOverride는 .argocd-source.yaml 파일의 내용을 나타낸다.
// 소스: reposerver/repository/repository.go — getApplicationSource()
// 이 파일은 App의 Spec.Source를 레포지토리 수준에서 오버라이드할 수 있다.
// 주 용도: 멀티 서비스 레포지토리에서 각 서비스의 고유 설정 분리
type ArgoCDSourceOverride struct {
	// Helm 오버라이드
	Helm *HelmOverride `yaml:"helm,omitempty"`
	// Kustomize 오버라이드
	Kustomize *KustomizeOverride `yaml:"kustomize,omitempty"`
}

// HelmOverride는 .argocd-source.yaml에서 Helm 설정 오버라이드다.
type HelmOverride struct {
	ReleaseName string            `yaml:"releaseName,omitempty"`
	Values      string            `yaml:"values,omitempty"`
	Parameters  map[string]string `yaml:"parameters,omitempty"`
}

// KustomizeOverride는 .argocd-source.yaml에서 Kustomize 설정 오버라이드다.
type KustomizeOverride struct {
	NamePrefix string   `yaml:"namePrefix,omitempty"`
	Images     []string `yaml:"images,omitempty"`
}

// applySourceOverride는 .argocd-source.yaml 오버라이드를 적용한다.
// 실제: reposerver/repository/repository.go — merge 로직
// 원본 source를 변경하지 않도록 깊은 복사를 수행한다.
func applySourceOverride(base *AppSource, override *ArgoCDSourceOverride) *AppSource {
	if override == nil {
		return base
	}
	// AppSource 얕은 복사
	merged := *base
	// Helm 포인터도 깊은 복사 (원본 Parameters map 보호)
	if base.Helm != nil {
		helmCopy := *base.Helm
		paramsCopy := make(map[string]string, len(base.Helm.Parameters))
		for k, v := range base.Helm.Parameters {
			paramsCopy[k] = v
		}
		helmCopy.Parameters = paramsCopy
		merged.Helm = &helmCopy
	}
	if override.Helm != nil && merged.Helm != nil {
		if override.Helm.ReleaseName != "" {
			merged.Helm.ReleaseName = override.Helm.ReleaseName
		}
		if override.Helm.Values != "" {
			merged.Helm.Values = override.Helm.Values
		}
		for k, v := range override.Helm.Parameters {
			if merged.Helm.Parameters == nil {
				merged.Helm.Parameters = make(map[string]string)
			}
			merged.Helm.Parameters[k] = v
		}
	}
	return &merged
}

// =============================================================================
// 3. ARGOCD_APP_* 환경변수 주입
//    소스: reposerver/repository/repository.go — populateHelmApp()
//          util/app/app.go — getAppEnvs()
// =============================================================================

// AppEnvVars는 CMP와 Helm hooks에 주입되는 App 환경변수다.
// 소스: util/app/app.go — getAppEnvs()
//
//	func getAppEnvs(app *v1alpha1.Application) []corev1.EnvVar {
//	    return []corev1.EnvVar{
//	        {Name: "ARGOCD_APP_NAME", Value: app.Name},
//	        {Name: "ARGOCD_APP_NAMESPACE", Value: app.Namespace},
//	        {Name: "ARGOCD_APP_REVISION", Value: revision},
//	        {Name: "ARGOCD_APP_SOURCE_REPO_URL", Value: app.Spec.GetSource().RepoURL},
//	        {Name: "ARGOCD_APP_SOURCE_PATH", Value: app.Spec.GetSource().Path},
//	        {Name: "ARGOCD_APP_SOURCE_TARGET_REVISION", Value: app.Spec.GetSource().TargetRevision},
//	    }
//	}
func buildAppEnvVars(appName, namespace, revision string, source *AppSource) map[string]string {
	return map[string]string{
		"ARGOCD_APP_NAME":                     appName,
		"ARGOCD_APP_NAMESPACE":                namespace,
		"ARGOCD_APP_REVISION":                 revision,
		"ARGOCD_APP_SOURCE_REPO_URL":          source.RepoURL,
		"ARGOCD_APP_SOURCE_PATH":              source.Path,
		"ARGOCD_APP_SOURCE_TARGET_REVISION":   source.TargetRevision,
	}
}

// =============================================================================
// 4. 리소스 추적 레이블 주입 (SetAppInstance)
//    소스: util/app/app.go — SetAppInstanceLabel()
//          pkg/apis/application/v1alpha1/app_instance_value.go
// =============================================================================

// TrackingMethod는 리소스 추적 방식이다.
// 소스: pkg/apis/application/v1alpha1/app_instance_value.go
type TrackingMethod string

const (
	// TrackingMethodLabel: app.kubernetes.io/instance 레이블로 추적 (기본)
	TrackingMethodLabel TrackingMethod = "label"
	// TrackingMethodAnnotation: argocd.argoproj.io/tracking-id 어노테이션으로 추적
	TrackingMethodAnnotation TrackingMethod = "annotation"
	// TrackingMethodAnnotationAndLabel: 둘 다 사용
	TrackingMethodAnnotationAndLabel TrackingMethod = "annotation+label"
)

// Resource는 K8s 매니페스트를 단순화한 구조체다.
type Resource struct {
	APIVersion  string
	Kind        string
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	// 실제 K8s 오브젝트 내용 (여기서는 단순 문자열)
	Spec string
}

// setAppInstance는 매니페스트에 추적 레이블/어노테이션을 주입한다.
// 소스: util/app/app.go — SetAppInstanceLabel()
//
//	func SetAppInstanceLabel(target *unstructured.Unstructured, app, trackingMethod, trackingID string) error {
//	    switch trackingMethod {
//	    case TrackingMethodLabel:
//	        target.SetLabels(map[string]string{"app.kubernetes.io/instance": app})
//	    case TrackingMethodAnnotation:
//	        target.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": trackingID})
//	    }
//	}
func setAppInstance(resources []*Resource, appName string, method TrackingMethod) {
	for _, res := range resources {
		if res.Labels == nil {
			res.Labels = make(map[string]string)
		}
		if res.Annotations == nil {
			res.Annotations = make(map[string]string)
		}

		trackingID := fmt.Sprintf("%s:%s/%s:%s/%s",
			appName, res.APIVersion, res.Kind, res.Namespace, res.Name)

		switch method {
		case TrackingMethodLabel:
			res.Labels["app.kubernetes.io/instance"] = appName
		case TrackingMethodAnnotation:
			res.Annotations["argocd.argoproj.io/tracking-id"] = trackingID
		case TrackingMethodAnnotationAndLabel:
			res.Labels["app.kubernetes.io/instance"] = appName
			res.Annotations["argocd.argoproj.io/tracking-id"] = trackingID
		}
	}
}

// =============================================================================
// 5. 캐시 — 더블체크 락킹 패턴
//    소스: reposerver/cache/cache.go
//          reposerver/repository/repository.go — generateManifests()
// =============================================================================

// cacheEntry는 캐시에 저장되는 매니페스트 생성 결과다.
type cacheEntry struct {
	resources  []*Resource
	revision   string
	cachedAt   time.Time
	// 에러 캐싱: 실패한 경우 에러도 캐싱
	err        error
	failCount  int
	// 에러 캐시 만료 시각
	pauseUntil *time.Time
}

// manifestCache는 더블체크 락킹으로 구현된 매니페스트 캐시다.
// 소스: reposerver/repository/repository.go — generateManifests()
//
//	// 1차 체크 (락 없음)
//	if cached, hit := cache.GetManifests(key); hit { return cached }
//	// 작업 단위 락 획득
//	lock.Lock(key)
//	defer lock.Unlock(key)
//	// 2차 체크 (락 획득 후, 다른 goroutine이 이미 생성했을 수 있음)
//	if cached, hit := cache.GetManifests(key); hit { return cached }
//	// 실제 생성
//	result := generateManifests(...)
//	cache.SetManifests(key, result)
type manifestCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
	// 에러 캐싱 설정
	// 소스: reposerver/repository/repository.go — PauseGenerationAfterFailedAttempts
	pauseAfterFailedAttempts  int           // 이 횟수 실패 시 생성 일시 중지
	pauseForMinutes           time.Duration // 일시 중지 기간
}

func newManifestCache() *manifestCache {
	return &manifestCache{
		entries:                  make(map[string]*cacheEntry),
		pauseAfterFailedAttempts: 3,
		pauseForMinutes:          2 * time.Minute,
	}
}

func (c *manifestCache) cacheKey(repoURL, revision, path string, source *AppSource) string {
	// 실제: 레포 URL + 리비전 + 소스 설정을 해시로 캐시 키 생성
	h := md5.New()
	fmt.Fprintf(h, "%s:%s:%s", repoURL, revision, path)
	if source.Helm != nil {
		fmt.Fprintf(h, ":helm:%s", source.Helm.ReleaseName)
		keys := make([]string, 0, len(source.Helm.Parameters))
		for k := range source.Helm.Parameters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, ",%s=%s", k, source.Helm.Parameters[k])
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// get은 캐시에서 항목을 조회한다.
func (c *manifestCache) get(key string) (*cacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	// 에러 캐시 만료 확인
	if entry.pauseUntil != nil && time.Now().After(*entry.pauseUntil) {
		return nil, false // 만료됨
	}
	return entry, true
}

// set은 캐시에 항목을 저장한다.
func (c *manifestCache) set(key string, entry *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry
}

// recordFailure는 실패를 기록하고 필요 시 에러 캐싱을 활성화한다.
// 소스: reposerver/repository/repository.go — PauseGenerationAfterFailedAttempts
//
//	if repoErrorCache.failureCount >= pauseAfterFailedAttempts {
//	    repoErrorCache.pauseUntil = now.Add(pauseForMinutes)
//	}
func (c *manifestCache) recordFailure(key string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		entry = &cacheEntry{}
		c.entries[key] = entry
	}
	entry.err = err
	entry.failCount++

	if entry.failCount >= c.pauseAfterFailedAttempts {
		pauseUntil := time.Now().Add(c.pauseForMinutes)
		entry.pauseUntil = &pauseUntil
		fmt.Printf("[Cache] 에러 캐싱 활성: %d회 실패 → %v 동안 생성 중단\n",
			entry.failCount, c.pauseForMinutes)
	}
}

// isPaused는 에러 캐시로 인해 생성이 중단된 상태인지 확인한다.
func (c *manifestCache) isPaused(key string) (bool, *time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok {
		return false, nil
	}
	if entry.pauseUntil != nil && time.Now().Before(*entry.pauseUntil) {
		return true, entry.pauseUntil
	}
	return false, nil
}

// =============================================================================
// 6. 세마포어 기반 병렬성 제어
//    소스: reposerver/repository/repository.go — parallelismLimitSemaphore
//          reposerver/server.go — concurrencyAllowed
// =============================================================================

// semaphore는 최대 동시 매니페스트 생성 수를 제한한다.
// 소스: reposerver/repository/repository.go
//
//	parallelismLimitSemaphore = semaphore.NewWeighted(parallelismLimit)
//	...
//	parallelismLimitSemaphore.Acquire(ctx, 1)
//	defer parallelismLimitSemaphore.Release(1)
type semaphore struct {
	ch chan struct{}
}

func newSemaphore(max int) *semaphore {
	ch := make(chan struct{}, max)
	for i := 0; i < max; i++ {
		ch <- struct{}{}
	}
	return &semaphore{ch: ch}
}

func (s *semaphore) acquire() {
	<-s.ch
}

func (s *semaphore) release() {
	s.ch <- struct{}{}
}

func (s *semaphore) available() int {
	return len(s.ch)
}

// =============================================================================
// 7. 소스 타입별 매니페스트 생성
// =============================================================================

// ManifestRequest는 매니페스트 생성 요청이다.
// 소스: reposerver/apiclient/repository.pb.go — ManifestRequest
type ManifestRequest struct {
	AppName    string
	Namespace  string
	AppProject string
	Source     *AppSource
	Revision   string
	// 추적 방식
	TrackingMethod TrackingMethod
	// 어노테이션/레이블 추가
	AppLabelKey   string
	AppLabelValue string
}

// ManifestResult는 매니페스트 생성 결과다.
type ManifestResult struct {
	Resources  []*Resource
	Revision   string
	SourceType SourceType
	// 생성 소요 시간
	Duration time.Duration
}

// generateHelmManifests는 Helm 소스에서 매니페스트를 생성한다.
// 소스: reposerver/repository/repository.go — helmTemplate()
func generateHelmManifests(req *ManifestRequest, envVars map[string]string) []*Resource {
	helm := req.Source.Helm
	releaseName := req.AppName
	if helm != nil && helm.ReleaseName != "" {
		releaseName = helm.ReleaseName
	}

	fmt.Printf("[Helm] helm template %s --namespace %s", releaseName, req.Namespace)
	if helm != nil {
		for _, vf := range helm.ValueFiles {
			fmt.Printf(" -f %s", vf)
		}
		for k, v := range helm.Parameters {
			fmt.Printf(" --set %s=%s", k, v)
		}
	}
	fmt.Println()

	// ARGOCD_APP_* 환경변수는 Helm hooks에서 사용 가능
	for k, v := range envVars {
		_ = k
		_ = v
	}

	time.Sleep(60 * time.Millisecond) // Helm template 실행 시뮬레이션

	return []*Resource{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: releaseName, Namespace: req.Namespace,
			Spec: fmt.Sprintf("replicas: 2, image: myapp:latest (helm release: %s)", releaseName)},
		{APIVersion: "v1", Kind: "Service", Name: releaseName, Namespace: req.Namespace,
			Spec: "type: ClusterIP, port: 8080"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: releaseName + "-config", Namespace: req.Namespace,
			Spec: "data: {config.yaml: ...}"},
		{APIVersion: "autoscaling/v2", Kind: "HorizontalPodAutoscaler", Name: releaseName, Namespace: req.Namespace,
			Spec: "minReplicas: 2, maxReplicas: 10"},
	}
}

// generateKustomizeManifests는 Kustomize 소스에서 매니페스트를 생성한다.
// 소스: reposerver/repository/repository.go — kustomizeBuild()
func generateKustomizeManifests(req *ManifestRequest) []*Resource {
	kust := req.Source.Kustomize
	prefix := ""
	if kust != nil {
		prefix = kust.NamePrefix
	}

	fmt.Printf("[Kustomize] kustomize build %s", req.Source.Path)
	if kust != nil && len(kust.Images) > 0 {
		fmt.Printf(" --load-restrictor LoadRestrictionsNone")
		for _, img := range kust.Images {
			fmt.Printf(" --image %s", img)
		}
	}
	fmt.Println()

	time.Sleep(40 * time.Millisecond)

	return []*Resource{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: prefix + "myapp", Namespace: req.Namespace,
			Spec: fmt.Sprintf("(kustomize overlay: %s)", req.Source.Path)},
		{APIVersion: "v1", Kind: "Service", Name: prefix + "myapp", Namespace: req.Namespace,
			Spec: "type: ClusterIP"},
		{APIVersion: "v1", Kind: "Namespace", Name: req.Namespace,
			Spec: ""},
	}
}

// generateDirectoryManifests는 일반 YAML 디렉토리에서 매니페스트를 생성한다.
// 소스: reposerver/repository/repository.go — findManifests()
func generateDirectoryManifests(req *ManifestRequest) []*Resource {
	fmt.Printf("[Directory] find %s -name '*.yaml' -o -name '*.json' | sort\n", req.Source.Path)
	time.Sleep(20 * time.Millisecond)

	return []*Resource{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "myapp", Namespace: req.Namespace,
			Spec: "replicas: 1"},
		{APIVersion: "v1", Kind: "Service", Name: "myapp", Namespace: req.Namespace,
			Spec: "type: LoadBalancer"},
	}
}

// generatePluginManifests는 CMP(Config Management Plugin)에서 매니페스트를 생성한다.
// 소스: reposerver/repository/repository.go — generateManifestsFromPlugin()
// 실제: gRPC 스트림으로 플러그인 컨테이너와 통신
func generatePluginManifests(req *ManifestRequest, envVars map[string]string) []*Resource {
	plugin := req.Source.Plugin
	pluginName := "unknown"
	if plugin != nil {
		pluginName = plugin.Name
	}

	fmt.Printf("[Plugin] CMP '%s' 실행\n", pluginName)
	fmt.Printf("[Plugin] 환경변수 주입: ARGOCD_APP_NAME=%s, ARGOCD_APP_NAMESPACE=%s\n",
		envVars["ARGOCD_APP_NAME"], envVars["ARGOCD_APP_NAMESPACE"])
	if plugin != nil {
		for k, v := range plugin.Env {
			fmt.Printf("[Plugin]   %s=%s\n", k, v)
		}
	}

	time.Sleep(120 * time.Millisecond) // 플러그인 실행 시간 (가장 느림)

	return []*Resource{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "myapp", Namespace: req.Namespace,
			Spec: fmt.Sprintf("(generated by plugin: %s)", pluginName)},
	}
}

// =============================================================================
// 8. Repo Server 핵심 — generateManifests()
//    소스: reposerver/repository/repository.go — (s *Service) GenerateManifests()
// =============================================================================

// RepoServer는 argocd-repo-server를 시뮬레이션한다.
type RepoServer struct {
	cache     *manifestCache
	semaphore *semaphore
	// 각 레포별 락 (동일 레포 동시 생성 방지)
	repoLocks map[string]*sync.Mutex
	repoLocksMu sync.Mutex
}

func newRepoServer(maxParallel int) *RepoServer {
	return &RepoServer{
		cache:     newManifestCache(),
		semaphore: newSemaphore(maxParallel),
		repoLocks: make(map[string]*sync.Mutex),
	}
}

func (s *RepoServer) getRepoLock(repoURL string) *sync.Mutex {
	s.repoLocksMu.Lock()
	defer s.repoLocksMu.Unlock()
	if _, ok := s.repoLocks[repoURL]; !ok {
		s.repoLocks[repoURL] = &sync.Mutex{}
	}
	return s.repoLocks[repoURL]
}

// generateManifests는 매니페스트를 생성하는 핵심 함수다.
// 소스: reposerver/repository/repository.go — GenerateManifests()
//
//	func (s *Service) GenerateManifests(ctx context.Context, q *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error) {
//	    // 1. 세마포어 획득
//	    // 2. 에러 캐시 확인
//	    // 3. 캐시 1차 확인 (락 없음)
//	    // 4. 레포 락 획득
//	    // 5. 캐시 2차 확인 (더블체크)
//	    // 6. Git clone/fetch
//	    // 7. .argocd-source.yaml 오버라이드 적용
//	    // 8. 소스 타입 감지
//	    // 9. 매니페스트 생성
//	    // 10. 추적 레이블 주입
//	    // 11. 캐시 저장
//	}
func (s *RepoServer) generateManifests(req *ManifestRequest) (*ManifestResult, error) {
	startTime := time.Now()
	cacheKey := s.cache.cacheKey(req.Source.RepoURL, req.Revision, req.Source.Path, req.Source)

	fmt.Printf("[RepoServer] 요청 수신: app=%s, repo=%s, path=%s\n",
		req.AppName, req.Source.RepoURL, req.Source.Path)

	// 1. 에러 캐시 확인 (생성이 일시 중단된 경우)
	if paused, until := s.cache.isPaused(cacheKey); paused {
		return nil, fmt.Errorf("매니페스트 생성 일시 중단 (until %v): 이전 실패 누적",
			until.Format(time.RFC3339))
	}

	// 2. 세마포어 획득 (최대 병렬 생성 수 제한)
	fmt.Printf("[RepoServer] 세마포어 획득 시도 (사용 가능: %d/%d)\n",
		s.semaphore.available(), cap(s.semaphore.ch))
	s.semaphore.acquire()
	defer s.semaphore.release()
	fmt.Printf("[RepoServer] 세마포어 획득 완료\n")

	// 3. 캐시 1차 확인 (락 없이 빠른 조회 — 더블체크 락킹 1단계)
	if cached, hit := s.cache.get(cacheKey); hit {
		fmt.Printf("[RepoServer] 캐시 HIT (1차): key=%s\n", cacheKey)
		return &ManifestResult{
			Resources:  cached.resources,
			Revision:   cached.revision,
			SourceType: SourceTypeDirectory,
			Duration:   time.Since(startTime),
		}, nil
	}
	fmt.Printf("[RepoServer] 캐시 MISS (1차): key=%s\n", cacheKey)

	// 4. 레포별 락 획득 (동일 레포 동시 생성 방지)
	repoLock := s.getRepoLock(req.Source.RepoURL)
	repoLock.Lock()
	defer repoLock.Unlock()

	// 5. 캐시 2차 확인 (락 획득 후 — 더블체크 락킹 2단계)
	// 다른 goroutine이 락을 들고 있는 동안 이미 생성했을 수 있음
	if cached, hit := s.cache.get(cacheKey); hit {
		fmt.Printf("[RepoServer] 캐시 HIT (2차, 더블체크): key=%s\n", cacheKey)
		return &ManifestResult{
			Resources:  cached.resources,
			Revision:   cached.revision,
			SourceType: SourceTypeDirectory,
			Duration:   time.Since(startTime),
		}, nil
	}

	// 6. Git clone/fetch 시뮬레이션
	fmt.Printf("[RepoServer] Git fetch: %s@%s\n", req.Source.RepoURL, req.Revision)
	time.Sleep(30 * time.Millisecond)
	resolvedRevision := "a1b2c3d4e5f6" // 실제: Git rev-parse 결과

	// 7. .argocd-source.yaml 오버라이드 적용
	source := req.Source
	// 실제: 레포지토리 루트에서 .argocd-source.yaml 읽기
	override := loadArgoCDSourceOverride(req.Source.Path)
	if override != nil {
		fmt.Printf("[RepoServer] .argocd-source.yaml 오버라이드 적용\n")
		source = applySourceOverride(source, override)
	}

	// 8. 소스 타입 감지
	// 실제: 실제 파일 시스템 조회
	repoFiles := getSimulatedRepoFiles(req.Source.Path)
	sourceType := detectSourceType(source, repoFiles)
	fmt.Printf("[RepoServer] 소스 타입 감지: %s\n", sourceType)

	// 9. ARGOCD_APP_* 환경변수 빌드
	envVars := buildAppEnvVars(req.AppName, req.Namespace, resolvedRevision, source)

	// 10. 소스 타입별 매니페스트 생성
	var resources []*Resource
	var genErr error

	switch sourceType {
	case SourceTypeHelm:
		resources = generateHelmManifests(req, envVars)
	case SourceTypeKustomize:
		resources = generateKustomizeManifests(req)
	case SourceTypePlugin:
		resources = generatePluginManifests(req, envVars)
	default: // Directory
		resources = generateDirectoryManifests(req)
	}

	if genErr != nil {
		s.cache.recordFailure(cacheKey, genErr)
		return nil, genErr
	}

	// 11. 추적 레이블 주입 (SetAppInstance)
	setAppInstance(resources, req.AppName, req.TrackingMethod)
	fmt.Printf("[RepoServer] 추적 레이블 주입: method=%s, %d개 리소스\n",
		req.TrackingMethod, len(resources))

	// 12. 캐시 저장
	entry := &cacheEntry{
		resources: resources,
		revision:  resolvedRevision,
		cachedAt:  time.Now(),
	}
	s.cache.set(cacheKey, entry)

	return &ManifestResult{
		Resources:  resources,
		Revision:   resolvedRevision,
		SourceType: sourceType,
		Duration:   time.Since(startTime),
	}, nil
}

// loadArgoCDSourceOverride는 .argocd-source.yaml을 로드한다.
// 실제: reposerver/repository/repository.go — getApplicationSource()
func loadArgoCDSourceOverride(path string) *ArgoCDSourceOverride {
	// 시뮬레이션: helm/myapp 경로에만 오버라이드 있음
	if strings.HasPrefix(path, "helm/") {
		return &ArgoCDSourceOverride{
			Helm: &HelmOverride{
				Parameters: map[string]string{
					"global.environment": "production",
				},
			},
		}
	}
	return nil
}

// getSimulatedRepoFiles는 레포지토리 파일 목록을 시뮬레이션한다.
func getSimulatedRepoFiles(path string) []string {
	if strings.HasPrefix(path, "helm/") {
		return []string{"Chart.yaml", "values.yaml", "templates/deployment.yaml"}
	}
	if strings.HasPrefix(path, "kustomize/") {
		return []string{"kustomization.yaml", "deployment.yaml", "service.yaml"}
	}
	if strings.HasPrefix(path, "plugin/") {
		return []string{".argocd-source.yaml", "config.json"}
	}
	return []string{"deployment.yaml", "service.yaml"}
}

// =============================================================================
// 9. 전체 시뮬레이션
// =============================================================================

func runManifestGenerationDemo() {
	fmt.Println("=================================================================")
	fmt.Println(" Argo CD Repo Server 매니페스트 생성 시뮬레이션")
	fmt.Println("=================================================================")
	fmt.Println()

	server := newRepoServer(3) // 최대 3개 동시 생성

	// --- 테스트 케이스 1: Helm ---
	fmt.Println("[ 케이스 1: Helm 소스 ]")
	fmt.Println("-----------------------------------------------------------------")
	req1 := &ManifestRequest{
		AppName:    "myapp",
		Namespace:  "production",
		AppProject: "myproject",
		Source: &AppSource{
			RepoURL:        "https://github.com/myorg/myapp.git",
			Path:           "helm/myapp",
			TargetRevision: "HEAD",
			Helm: &HelmSource{
				ReleaseName: "myapp-prod",
				ValueFiles:  []string{"values-prod.yaml"},
				Parameters:  map[string]string{"image.tag": "v1.2.3", "replicaCount": "3"},
			},
		},
		Revision:       "HEAD",
		TrackingMethod: TrackingMethodAnnotationAndLabel,
	}
	result1, err := server.generateManifests(req1)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("\n  결과: %d개 리소스, revision=%s, sourceType=%s, 소요=%v\n",
			len(result1.Resources), result1.Revision, result1.SourceType, result1.Duration.Round(time.Millisecond))
		for _, r := range result1.Resources {
			fmt.Printf("    %-25s %-15s labels=%v\n",
				r.Kind+"/"+r.Name, "", r.Labels)
		}
	}

	// --- 테스트 케이스 2: 캐시 HIT ---
	fmt.Println()
	fmt.Println("[ 케이스 2: 캐시 HIT (동일 요청 재시도) ]")
	fmt.Println("-----------------------------------------------------------------")
	result2, _ := server.generateManifests(req1)
	if result2 != nil {
		fmt.Printf("  캐시 결과: %d개 리소스, 소요=%v\n",
			len(result2.Resources), result2.Duration.Round(time.Millisecond))
	}

	// --- 테스트 케이스 3: Kustomize ---
	fmt.Println()
	fmt.Println("[ 케이스 3: Kustomize 소스 ]")
	fmt.Println("-----------------------------------------------------------------")
	req3 := &ManifestRequest{
		AppName:   "myapp-kust",
		Namespace: "staging",
		Source: &AppSource{
			RepoURL:        "https://github.com/myorg/myapp.git",
			Path:           "kustomize/overlays/staging",
			TargetRevision: "main",
			Kustomize: &KustomizeSource{
				NamePrefix: "staging-",
				Images:     []string{"myapp:v1.3.0"},
			},
		},
		Revision:       "main",
		TrackingMethod: TrackingMethodLabel,
	}
	result3, _ := server.generateManifests(req3)
	if result3 != nil {
		fmt.Printf("\n  결과: %d개 리소스, sourceType=%s\n",
			len(result3.Resources), result3.SourceType)
		for _, r := range result3.Resources {
			fmt.Printf("    %s/%s (namespace=%s) labels=%v\n",
				r.Kind, r.Name, r.Namespace, r.Labels)
		}
	}

	// --- 테스트 케이스 4: Directory ---
	fmt.Println()
	fmt.Println("[ 케이스 4: Directory 소스 (일반 YAML) ]")
	fmt.Println("-----------------------------------------------------------------")
	req4 := &ManifestRequest{
		AppName:   "simple-app",
		Namespace: "default",
		Source: &AppSource{
			RepoURL:        "https://github.com/myorg/simple.git",
			Path:           "k8s/",
			TargetRevision: "v2.0.0",
		},
		Revision:       "v2.0.0",
		TrackingMethod: TrackingMethodAnnotation,
	}
	result4, _ := server.generateManifests(req4)
	if result4 != nil {
		fmt.Printf("\n  결과: %d개 리소스, sourceType=%s\n",
			len(result4.Resources), result4.SourceType)
		for _, r := range result4.Resources {
			fmt.Printf("    %s/%s annotations=%v\n",
				r.Kind, r.Name, r.Annotations)
		}
	}

	// --- 테스트 케이스 5: Plugin (CMP) ---
	fmt.Println()
	fmt.Println("[ 케이스 5: Config Management Plugin (CMP) ]")
	fmt.Println("-----------------------------------------------------------------")
	req5 := &ManifestRequest{
		AppName:   "plugin-app",
		Namespace: "custom",
		Source: &AppSource{
			RepoURL:        "https://github.com/myorg/plugin-app.git",
			Path:           "plugin/config",
			TargetRevision: "HEAD",
			Plugin: &PluginSource{
				Name: "my-custom-plugin",
				Env:  map[string]string{"DEPLOY_ENV": "production", "REGION": "ap-northeast-2"},
			},
		},
		Revision:       "HEAD",
		TrackingMethod: TrackingMethodLabel,
	}
	result5, _ := server.generateManifests(req5)
	if result5 != nil {
		fmt.Printf("\n  결과: %d개 리소스, sourceType=%s\n",
			len(result5.Resources), result5.SourceType)
	}

	// --- 테스트 케이스 6: 병렬 요청 ---
	fmt.Println()
	fmt.Println("[ 케이스 6: 병렬 매니페스트 생성 (세마포어 제어) ]")
	fmt.Println("-----------------------------------------------------------------")
	fmt.Printf("  세마포어 최대: %d\n", cap(server.semaphore.ch))

	var wg sync.WaitGroup
	results := make([]*ManifestResult, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := &ManifestRequest{
				AppName:   fmt.Sprintf("parallel-app-%d", idx),
				Namespace: fmt.Sprintf("ns-%d", idx),
				Source: &AppSource{
					RepoURL:        fmt.Sprintf("https://github.com/myorg/app%d.git", idx),
					Path:           "k8s/",
					TargetRevision: "HEAD",
				},
				Revision:       "HEAD",
				TrackingMethod: TrackingMethodLabel,
			}
			result, _ := server.generateManifests(req)
			results[idx] = result
		}(i)
	}
	wg.Wait()

	successCount := 0
	for _, r := range results {
		if r != nil {
			successCount++
		}
	}
	fmt.Printf("  병렬 요청 5개 중 %d개 완료\n", successCount)

	// --- 테스트 케이스 7: 에러 캐싱 시연 ---
	fmt.Println()
	fmt.Println("[ 케이스 7: 에러 캐싱 (PauseGenerationAfterFailedAttempts) ]")
	fmt.Println("-----------------------------------------------------------------")
	fmt.Printf("  pauseAfterFailedAttempts: %d\n", server.cache.pauseAfterFailedAttempts)
	fmt.Printf("  pauseForMinutes: %v\n", server.cache.pauseForMinutes)

	// 실패 시뮬레이션
	errorKey := "error-test-key"
	for i := 1; i <= 3; i++ {
		server.cache.recordFailure(errorKey, fmt.Errorf("git clone failed"))
		paused, until := server.cache.isPaused(errorKey)
		if paused {
			fmt.Printf("  %d회 실패 후 → 생성 중단 (until: %s)\n", i, until.Format("15:04:05"))
		} else {
			fmt.Printf("  %d회 실패 → 아직 계속 허용\n", i)
		}
	}

	// --- 소스 타입 감지 요약 ---
	fmt.Println()
	fmt.Println("[ 소스 타입 감지 규칙 ]")
	fmt.Println("-----------------------------------------------------------------")
	cases := []struct {
		desc    string
		source  *AppSource
		files   []string
		expect  SourceType
	}{
		{"chart 필드 설정", &AppSource{Chart: "nginx"}, nil, SourceTypeHelm},
		{"Plugin 설정", &AppSource{Plugin: &PluginSource{Name: "cmp"}}, nil, SourceTypePlugin},
		{"Helm 명시", &AppSource{Helm: &HelmSource{ReleaseName: "app"}}, nil, SourceTypeHelm},
		{"Kustomize 명시", &AppSource{Kustomize: &KustomizeSource{}}, nil, SourceTypeKustomize},
		{"Chart.yaml 파일 존재", &AppSource{}, []string{"Chart.yaml"}, SourceTypeHelm},
		{"kustomization.yaml 파일 존재", &AppSource{}, []string{"kustomization.yaml"}, SourceTypeKustomize},
		{"YAML 파일만 존재", &AppSource{}, []string{"deploy.yaml"}, SourceTypeDirectory},
	}
	for _, tc := range cases {
		detected := detectSourceType(tc.source, tc.files)
		match := "OK"
		if detected != tc.expect {
			match = fmt.Sprintf("MISMATCH(got %s)", detected)
		}
		fmt.Printf("  %-40s → %-12s [%s]\n", tc.desc, detected, match)
	}
}

func main() {
	runManifestGenerationDemo()
}
