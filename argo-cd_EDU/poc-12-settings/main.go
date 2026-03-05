// poc-12-settings/main.go
//
// Argo CD 설정 관리 시뮬레이션
//
// 참조 소스:
//   - util/settings/settings.go : ArgoCDSettings, SettingsManager, Subscribe/Unsubscribe
//   - util/db/secrets.go        : URIToSecretName (FNV-32a 해시)
//   - util/db/repository.go     : RepoURLToSecretName (FNV-32a 해시)
//   - common/common.go          : ArgoCDConfigMapName("argocd-cm"), ArgoCDSecretName("argocd-secret")
//                                  LabelKeySecretType, LabelValueSecretTypeCluster/Repository
//
// 핵심 개념:
//   1. ArgoCDSettings: URL, DexConfig, OIDCConfig, ServerSignature, TrackingMethod
//   2. SettingsManager: ConfigMap + Secret Watch 기반 설정 관리
//   3. K8s Secret 기반 스토리지:
//      - 클러스터: URIToSecretName (FNV-32a, "cluster-<host>-<hash>")
//      - 저장소: RepoURLToSecretName (FNV-32a, "repo-<hash>")
//      - 레이블: argocd.argoproj.io/secret-type: cluster/repository
//   4. 설정 구독 패턴: Subscribe(chan<- *ArgoCDSettings) / Unsubscribe
//   5. 리소스 추적 방식: annotation, label, annotation+label
//   6. 리소스 커스터마이제이션: health check Lua 스크립트 경로
//   7. ConfigMap 주요 키 상수 표

package main

import (
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ============================================================
// ConfigMap 키 상수 (argocd-cm)
// 참조: util/settings/settings.go const 블록
// ============================================================

const (
	// argocd-cm ConfigMap 이름 (common.ArgoCDConfigMapName)
	ArgoCDConfigMapName = "argocd-cm"
	// argocd-secret Secret 이름 (common.ArgoCDSecretName)
	ArgoCDSecretName = "argocd-secret"

	// ConfigMap 키 상수들 (실제 소스 그대로)
	SettingURLKey                    = "url"
	SettingDexConfigKey              = "dex.config"
	SettingsOIDCConfigKey            = "oidc.config"
	SettingApplicationInstanceLabelKey = "application.instanceLabelKey"
	SettingResourceTrackingMethodKey = "application.resourceTrackingMethod"
	ResourceCustomizationsKey        = "resource.customizations"
	ResourceExclusionsKey            = "resource.exclusions"
	ResourceInclusionsKey            = "resource.inclusions"
	KustomizeBuildOptionsKey         = "kustomize.buildOptions"
	StatusBadgeEnabledKey            = "statusbadge.enabled"
	AnonymousUserEnabledKey          = "users.anonymous.enabled"
	UserSessionDurationKey           = "users.session.duration"
	SettingServerCertificate         = "tls.crt"
	SettingServerPrivateKey          = "tls.key"
	SettingServerSignatureKey        = "server.secretkey"    // argocd-secret 키
	HelpChatURL                      = "help.chatUrl"
	HelpChatText                     = "help.chatText"

	// 리소스 추적 방식 (application.resourceTrackingMethod 값)
	TrackingMethodAnnotation         = "annotation"      // 기본값 (어노테이션만)
	TrackingMethodLabel              = "label"           // 레이블만
	TrackingMethodAnnotationAndLabel = "annotation+label" // 둘 다

	// K8s Secret 레이블 (common.LabelKeySecretType)
	LabelKeySecretType             = "argocd.argoproj.io/secret-type"
	LabelValueSecretTypeCluster    = "cluster"
	LabelValueSecretTypeRepository = "repository"
	LabelValueSecretTypeRepoCreds  = "repo-creds"
)

// ============================================================
// ArgoCDSettings — 런타임 설정 구조체
// 참조: util/settings/settings.go type ArgoCDSettings struct
// ============================================================

type ArgoCDSettings struct {
	// Argo CD 외부 URL (SSO 설정에 사용)
	URL string `json:"url,omitempty"`
	// Dex 설정 (YAML 형식)
	DexConfig string `json:"dexConfig,omitempty"`
	// OIDC 설정 (원시 문자열)
	OIDCConfigRAW string `json:"oidcConfig,omitempty"`
	// JWT 토큰 서명 키
	ServerSignature []byte `json:"serverSignature,omitempty"`
	// 리소스 추적 방식
	TrackingMethod string `json:"application.resourceTrackingMethod,omitempty"`
	// Application 인스턴스 레이블 키
	ApplicationInstanceLabelKey string `json:"application.instanceLabelKey,omitempty"`
	// 상태 뱃지 활성화
	StatusBadgeEnabled bool `json:"statusBadgeEnable"`
	// 익명 사용자 허용
	AnonymousUserEnabled bool `json:"anonymousUserEnabled,omitempty"`
	// 세션 유효 기간
	UserSessionDuration time.Duration `json:"userSessionDuration,omitempty"`
	// GitHub 웹훅 시크릿
	WebhookGitHubSecret string `json:"webhookGitHubSecret,omitempty"`
	// Kustomize 빌드 옵션
	KustomizeBuildOptions string `json:"kustomizeBuildOptions,omitempty"`
	// 리소스 커스터마이제이션 (kind → 설정)
	ResourceOverrides map[string]ResourceOverride `json:"resourceOverrides,omitempty"`
}

// ResourceOverride — 리소스 타입별 커스터마이제이션
// 참조: pkg/apis/application/v1alpha1/types.go type ResourceOverride struct
type ResourceOverride struct {
	// Health 체크 Lua 스크립트
	HealthLuaScript string
	// Ignore Differences 설정
	IgnoreDifferences string
	// Actions 정의
	Actions string
}

// ============================================================
// K8s Secret 기반 스토리지
// 참조: util/db/secrets.go, util/db/repository.go
// ============================================================

// URIToSecretName — 클러스터 URI → Secret 이름
// 참조: util/db/secrets.go func URIToSecretName(uriType, uri string) (string, error)
//
// 알고리즘:
//   1. URI 파싱 → host 추출 (포트 제거, IPv6 정규화)
//   2. FNV-32a 해시로 URI 전체 해시
//   3. "<uriType>-<host>-<hash>" 형식으로 시크릿 이름 생성
//
// 예: URIToSecretName("cluster", "https://k8s.example.com:6443")
//     → "cluster-k8s.example.com-1234567890"
func URIToSecretName(uriType, uri string) (string, error) {
	parsedURI, err := url.ParseRequestURI(uri)
	if err != nil {
		return "", fmt.Errorf("URI 파싱 오류: %w", err)
	}

	host := parsedURI.Host
	// IPv6 주소 처리: [fe80::1ff:fe23:4567:890a] → fe80--1ff-fe23-4567-890a
	if strings.HasPrefix(host, "[") {
		last := strings.Index(host, "]")
		if last >= 0 {
			ipv6 := host[1:last]
			host = strings.ReplaceAll(ipv6, ":", "-")
		}
	} else {
		// 포트 제거
		if idx := strings.Index(host, ":"); idx >= 0 {
			host = host[:idx]
		}
	}

	// FNV-32a 해시 (실제 코드 그대로)
	h := fnv.New32a()
	_, _ = h.Write([]byte(uri))
	host = strings.ToLower(host)
	return fmt.Sprintf("%s-%s-%v", uriType, host, h.Sum32()), nil
}

// RepoURLToSecretName — 저장소 URL → Secret 이름
// 참조: util/db/repository.go func RepoURLToSecretName(prefix, repo, project string) string
//
// 알고리즘:
//   1. FNV-32a로 repo + project 해시
//   2. "<prefix>-<hash>" 형식으로 시크릿 이름 생성
//
// 주석 경고: "NOTE: this formula should not be considered stable and may change in future releases."
func RepoURLToSecretName(prefix, repo, project string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(repo))
	_, _ = h.Write([]byte(project))
	return fmt.Sprintf("%s-%v", prefix, h.Sum32())
}

// ClusterSecret — K8s Secret으로 저장되는 클러스터 정보
// 레이블: argocd.argoproj.io/secret-type=cluster
type ClusterSecret struct {
	Name   string            // Secret 이름 (URIToSecretName 생성)
	Labels map[string]string // {LabelKeySecretType: "cluster"}
	Data   map[string]string // name, server, project, config (JSON)
}

// RepoSecret — K8s Secret으로 저장되는 저장소 정보
// 레이블: argocd.argoproj.io/secret-type=repository
type RepoSecret struct {
	Name   string
	Labels map[string]string
	Data   map[string]string // url, username, password, sshPrivateKey, type
}

// ============================================================
// SettingsManager — ConfigMap + Secret Watch 기반 설정 관리자
// 참조: util/settings/settings.go type SettingsManager struct
//
// 핵심 필드:
//   - subscribers: 설정 변경 구독자 채널 목록
//   - mutex: 구독자 목록 보호
//   - clusterSecrets: 클러스터 Secret 캐시 (시뮬레이션)
//   - repoSecrets: 저장소 Secret 캐시 (시뮬레이션)
// ============================================================

type SettingsManager struct {
	mu sync.Mutex

	// 현재 설정 (ConfigMap + Secret에서 로드)
	settings *ArgoCDSettings

	// 설정 변경 구독자 목록
	// 실제 코드: subscribers []chan<- *ArgoCDSettings
	subscribers []chan<- *ArgoCDSettings

	// 클러스터/저장소 Secret 캐시 (시뮬레이션용)
	clusterSecrets map[string]*ClusterSecret
	repoSecrets    map[string]*RepoSecret
}

func NewSettingsManager() *SettingsManager {
	return &SettingsManager{
		settings:       &ArgoCDSettings{},
		clusterSecrets: make(map[string]*ClusterSecret),
		repoSecrets:    make(map[string]*RepoSecret),
	}
}

// LoadSettings — ConfigMap + Secret에서 설정 로드
// 실제 코드: mgr.getSettings() → 내부적으로 GetConfigMapByName("argocd-cm") 호출
func (m *SettingsManager) LoadSettings(configMapData, secretData map[string]string) (*ArgoCDSettings, error) {
	settings := &ArgoCDSettings{}

	// URL (ConfigMap: "url" 키)
	if url, ok := configMapData[SettingURLKey]; ok {
		settings.URL = url
	}
	// Dex 설정 (ConfigMap: "dex.config" 키)
	if dex, ok := configMapData[SettingDexConfigKey]; ok {
		settings.DexConfig = dex
	}
	// OIDC 설정 (ConfigMap: "oidc.config" 키)
	if oidc, ok := configMapData[SettingsOIDCConfigKey]; ok {
		settings.OIDCConfigRAW = oidc
	}
	// 리소스 추적 방식 (ConfigMap: "application.resourceTrackingMethod" 키)
	if tm, ok := configMapData[SettingResourceTrackingMethodKey]; ok {
		settings.TrackingMethod = tm
	} else {
		// 실제 코드: 기본값은 TrackingMethodAnnotation
		settings.TrackingMethod = TrackingMethodAnnotation
	}
	// Application 인스턴스 레이블 (ConfigMap: "application.instanceLabelKey" 키)
	if ilk, ok := configMapData[SettingApplicationInstanceLabelKey]; ok {
		settings.ApplicationInstanceLabelKey = ilk
	} else {
		settings.ApplicationInstanceLabelKey = "app.kubernetes.io/instance"
	}
	// 상태 뱃지 (ConfigMap: "statusbadge.enabled" 키)
	settings.StatusBadgeEnabled = configMapData[StatusBadgeEnabledKey] == "true"
	// 익명 사용자 (ConfigMap: "users.anonymous.enabled" 키)
	settings.AnonymousUserEnabled = configMapData[AnonymousUserEnabledKey] == "true"
	// Kustomize 빌드 옵션
	if kbo, ok := configMapData[KustomizeBuildOptionsKey]; ok {
		settings.KustomizeBuildOptions = kbo
	}

	// 서버 서명 키 (Secret: "server.secretkey" 키)
	if sig, ok := secretData[SettingServerSignatureKey]; ok {
		decoded, err := base64.StdEncoding.DecodeString(sig)
		if err == nil {
			settings.ServerSignature = decoded
		}
	}
	// GitHub 웹훅 시크릿 (Secret: "webhook.github.secret" 키)
	if whSec, ok := secretData["webhook.github.secret"]; ok {
		settings.WebhookGitHubSecret = whSec
	}

	m.mu.Lock()
	m.settings = settings
	m.mu.Unlock()

	return settings, nil
}

// GetSettings — 현재 설정 반환
func (m *SettingsManager) GetSettings() *ArgoCDSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// UpdateSettings — 설정 업데이트 + 구독자 알림
// 실제 코드: mgr.notifySubscribers(newSettings) 호출
func (m *SettingsManager) UpdateSettings(newSettings *ArgoCDSettings) {
	m.mu.Lock()
	m.settings = newSettings
	subs := make([]chan<- *ArgoCDSettings, len(m.subscribers))
	copy(subs, m.subscribers)
	m.mu.Unlock()

	// 실제 코드: go func() { for _, sub := range subscribers { sub <- newSettings } }()
	// 별도 고루틴에서 알림 (데드락 방지)
	go func() {
		for _, sub := range subs {
			select {
			case sub <- newSettings:
			case <-time.After(100 * time.Millisecond):
				// 구독자가 응답 없으면 건너뜀
			}
		}
	}()
}

// Subscribe — 설정 변경 구독 채널 등록
// 참조: util/settings/settings.go func (mgr *SettingsManager) Subscribe(subCh chan<- *ArgoCDSettings)
func (m *SettingsManager) Subscribe(subCh chan<- *ArgoCDSettings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subscribers = append(m.subscribers, subCh)
	fmt.Printf("  [SettingsManager] 구독 등록: 총 %d개 구독자\n", len(m.subscribers))
}

// Unsubscribe — 설정 변경 구독 취소
// 참조: util/settings/settings.go func (mgr *SettingsManager) Unsubscribe(subCh chan<- *ArgoCDSettings)
func (m *SettingsManager) Unsubscribe(subCh chan<- *ArgoCDSettings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, ch := range m.subscribers {
		if ch == subCh {
			m.subscribers = append(m.subscribers[:i], m.subscribers[i+1:]...)
			fmt.Printf("  [SettingsManager] 구독 해제: 총 %d개 구독자\n", len(m.subscribers))
			return
		}
	}
}

// ============================================================
// 클러스터 CRUD (argocd.argoproj.io/secret-type=cluster)
// ============================================================

// AddCluster — 클러스터 Secret 생성
// 실제 코드: util/db/cluster.go → CreateCluster() → URIToSecretName() → k8s Secret 생성
func (m *SettingsManager) AddCluster(name, serverURL, project string, labels map[string]string) (*ClusterSecret, error) {
	secretName, err := URIToSecretName("cluster", serverURL)
	if err != nil {
		return nil, fmt.Errorf("클러스터 시크릿 이름 생성 실패: %w", err)
	}

	secretLabels := map[string]string{
		LabelKeySecretType: LabelValueSecretTypeCluster,
	}
	for k, v := range labels {
		secretLabels[k] = v
	}

	secret := &ClusterSecret{
		Name:   secretName,
		Labels: secretLabels,
		Data: map[string]string{
			"name":    name,
			"server":  serverURL,
			"project": project,
		},
	}

	m.mu.Lock()
	m.clusterSecrets[secretName] = secret
	m.mu.Unlock()

	fmt.Printf("  [Cluster] 추가: name=%q server=%q → secretName=%q\n", name, serverURL, secretName)
	return secret, nil
}

// GetCluster — 클러스터 조회
func (m *SettingsManager) GetCluster(serverURL string) (*ClusterSecret, error) {
	secretName, err := URIToSecretName("cluster", serverURL)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	secret, ok := m.clusterSecrets[secretName]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("클러스터를 찾을 수 없음: %s", serverURL)
	}
	return secret, nil
}

// ListClusters — 모든 클러스터 조회
// 실제 코드: labelSelector로 argocd.argoproj.io/secret-type=cluster 시크릿 목록 조회
func (m *SettingsManager) ListClusters() []*ClusterSecret {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*ClusterSecret, 0, len(m.clusterSecrets))
	for _, secret := range m.clusterSecrets {
		result = append(result, secret)
	}
	return result
}

// DeleteCluster — 클러스터 삭제
func (m *SettingsManager) DeleteCluster(serverURL string) error {
	secretName, err := URIToSecretName("cluster", serverURL)
	if err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.clusterSecrets, secretName)
	m.mu.Unlock()
	fmt.Printf("  [Cluster] 삭제: secretName=%q\n", secretName)
	return nil
}

// ============================================================
// 저장소 CRUD (argocd.argoproj.io/secret-type=repository)
// ============================================================

const (
	repoSecretPrefix = "repo"
	credSecretPrefix = "repocreds"
)

// AddRepo — 저장소 Secret 생성
// 실제 코드: util/db/repository.go → createOrUpdateRepositorySecret()
// → RepoURLToSecretName(repoSecretPrefix, repo, project)
func (m *SettingsManager) AddRepo(repoURL, username, password, project string) (*RepoSecret, error) {
	secretName := RepoURLToSecretName(repoSecretPrefix, repoURL, project)

	secret := &RepoSecret{
		Name: secretName,
		Labels: map[string]string{
			LabelKeySecretType: LabelValueSecretTypeRepository,
		},
		Data: map[string]string{
			"url":      repoURL,
			"username": username,
			"password": password,
			"type":     "git",
			"project":  project,
		},
	}

	m.mu.Lock()
	m.repoSecrets[secretName] = secret
	m.mu.Unlock()

	fmt.Printf("  [Repo] 추가: url=%q project=%q → secretName=%q\n", repoURL, project, secretName)
	return secret, nil
}

// GetRepo — 저장소 조회
func (m *SettingsManager) GetRepo(repoURL, project string) (*RepoSecret, error) {
	secretName := RepoURLToSecretName(repoSecretPrefix, repoURL, project)
	m.mu.Lock()
	secret, ok := m.repoSecrets[secretName]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("저장소를 찾을 수 없음: %s", repoURL)
	}
	return secret, nil
}

// ListRepos — 모든 저장소 조회
func (m *SettingsManager) ListRepos() []*RepoSecret {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*RepoSecret, 0, len(m.repoSecrets))
	for _, secret := range m.repoSecrets {
		result = append(result, secret)
	}
	return result
}

// ============================================================
// 리소스 추적 방식 설명
// 참조: util/settings/settings.go GetTrackingMethod()
// ============================================================

func describeTrackingMethod(method string) string {
	switch method {
	case TrackingMethodAnnotation:
		return "argocd.argoproj.io/app 어노테이션만 사용 (기본값)"
	case TrackingMethodLabel:
		return "app.kubernetes.io/instance 레이블만 사용"
	case TrackingMethodAnnotationAndLabel:
		return "어노테이션 + 레이블 모두 사용"
	default:
		return "알 수 없는 추적 방식"
	}
}

// ============================================================
// Main: 시나리오 시연
// ============================================================

func main() {
	fmt.Println("=======================================================")
	fmt.Println("Argo CD 설정 관리 시뮬레이션")
	fmt.Println("=======================================================")

	// ─── ConfigMap 키 상수 표 ────────────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("ConfigMap 키 상수 (argocd-cm)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	configKeys := []struct{ key, desc string }{
		{SettingURLKey, "Argo CD 외부 URL (SSO 용)"},
		{SettingDexConfigKey, "Dex 설정 (YAML)"},
		{SettingsOIDCConfigKey, "OIDC 설정 (YAML)"},
		{SettingResourceTrackingMethodKey, "리소스 추적 방식"},
		{SettingApplicationInstanceLabelKey, "앱 인스턴스 레이블 키"},
		{ResourceCustomizationsKey, "리소스 타입별 커스터마이제이션"},
		{ResourceExclusionsKey, "제외할 리소스 목록"},
		{ResourceInclusionsKey, "포함할 리소스 목록"},
		{KustomizeBuildOptionsKey, "Kustomize 빌드 옵션"},
		{StatusBadgeEnabledKey, "상태 뱃지 활성화"},
		{AnonymousUserEnabledKey, "익명 사용자 허용"},
		{UserSessionDurationKey, "세션 유효 기간"},
	}

	secretKeys := []struct{ key, desc string }{
		{SettingServerSignatureKey, "JWT 서명 키 (argocd-secret)"},
		{"webhook.github.secret", "GitHub 웹훅 시크릿 (argocd-secret)"},
		{"webhook.gitlab.secret", "GitLab 웹훅 시크릿 (argocd-secret)"},
	}

	fmt.Printf("\n  %-45s %s\n", "ConfigMap 키", "설명")
	fmt.Println("  " + strings.Repeat("-", 70))
	for _, k := range configKeys {
		fmt.Printf("  %-45s %s\n", k.key, k.desc)
	}
	fmt.Printf("\n  %-45s %s\n", "Secret 키", "설명")
	fmt.Println("  " + strings.Repeat("-", 70))
	for _, k := range secretKeys {
		fmt.Printf("  %-45s %s\n", k.key, k.desc)
	}

	// ─── SettingsManager: 설정 로드 ─────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("SettingsManager: 설정 로드")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	mgr := NewSettingsManager()

	configMapData := map[string]string{
		SettingURLKey:                    "https://argocd.example.com",
		SettingDexConfigKey:              "connectors:\n  - type: github\n    id: github\n",
		SettingResourceTrackingMethodKey: TrackingMethodAnnotation,
		SettingApplicationInstanceLabelKey: "app.kubernetes.io/instance",
		KustomizeBuildOptionsKey:         "--enable-alpha-plugins",
		StatusBadgeEnabledKey:            "true",
		AnonymousUserEnabledKey:          "false",
	}

	secretData := map[string]string{
		SettingServerSignatureKey: base64.StdEncoding.EncodeToString([]byte("super-secret-jwt-key-32-bytes-long")),
		"webhook.github.secret":  "github-webhook-secret-abc123",
	}

	settings, err := mgr.LoadSettings(configMapData, secretData)
	if err != nil {
		fmt.Printf("설정 로드 실패: %v\n", err)
		return
	}

	fmt.Printf("\n  로드된 설정:\n")
	fmt.Printf("  URL:              %s\n", settings.URL)
	fmt.Printf("  TrackingMethod:   %s (%s)\n", settings.TrackingMethod, describeTrackingMethod(settings.TrackingMethod))
	fmt.Printf("  InstanceLabel:    %s\n", settings.ApplicationInstanceLabelKey)
	fmt.Printf("  StatusBadge:      %v\n", settings.StatusBadgeEnabled)
	fmt.Printf("  AnonymousUser:    %v\n", settings.AnonymousUserEnabled)
	fmt.Printf("  KustomizeBuild:   %s\n", settings.KustomizeBuildOptions)
	fmt.Printf("  ServerSignature:  %d bytes\n", len(settings.ServerSignature))
	fmt.Printf("  WebhookGitHub:    %s\n", settings.WebhookGitHubSecret)

	// ─── 설정 구독 패턴 ─────────────────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("설정 구독 패턴 (Subscribe → Update → Notify → Unsubscribe)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// 구독자 1: Application Controller
	appCtrlCh := make(chan *ArgoCDSettings, 1)
	mgr.Subscribe(appCtrlCh)

	// 구독자 2: API Server
	apiServerCh := make(chan *ArgoCDSettings, 1)
	mgr.Subscribe(apiServerCh)

	// 설정 변경 (실제: ConfigMap Watch 이벤트 → notifySubscribers)
	fmt.Println("\n  설정 변경 (TrackingMethod: annotation → annotation+label)")
	updatedSettings := *settings
	updatedSettings.TrackingMethod = TrackingMethodAnnotationAndLabel
	updatedSettings.AnonymousUserEnabled = true
	mgr.UpdateSettings(&updatedSettings)

	// 구독자들이 알림 수신
	time.Sleep(50 * time.Millisecond)
	select {
	case newSettings := <-appCtrlCh:
		fmt.Printf("\n  [AppController] 설정 변경 수신:\n")
		fmt.Printf("    TrackingMethod: %s\n", newSettings.TrackingMethod)
		fmt.Printf("    AnonymousUser: %v\n", newSettings.AnonymousUserEnabled)
	case <-time.After(200 * time.Millisecond):
		fmt.Println("  [AppController] 알림 타임아웃")
	}

	select {
	case <-apiServerCh:
		fmt.Printf("  [APIServer] 설정 변경 수신 완료\n")
	case <-time.After(200 * time.Millisecond):
		fmt.Println("  [APIServer] 알림 타임아웃")
	}

	// 구독 해제
	mgr.Unsubscribe(appCtrlCh)
	mgr.Unsubscribe(apiServerCh)

	// ─── 리소스 추적 방식 설명 ───────────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("리소스 추적 방식 (application.resourceTrackingMethod)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	for _, method := range []string{TrackingMethodAnnotation, TrackingMethodLabel, TrackingMethodAnnotationAndLabel} {
		fmt.Printf("\n  방식: %q\n  설명: %s\n", method, describeTrackingMethod(method))
	}

	// ─── 클러스터 Secret 관리 (URIToSecretName) ──────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("클러스터 Secret 관리 (URIToSecretName - FNV-32a)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	clusterURLs := []struct {
		name   string
		url    string
		project string
	}{
		{"in-cluster", "https://kubernetes.default.svc", ""},
		{"us-west-2", "https://k8s-usw2.example.com:6443", ""},
		{"eu-central-1", "https://k8s-euc1.example.com", ""},
		{"IPv6 클러스터", "http://[fe80::1ff:fe23:4567:890a]:8000", ""},
	}

	fmt.Printf("\n  %-20s %-45s %s\n", "이름", "서버 URL", "Secret 이름 (FNV-32a)")
	fmt.Println("  " + strings.Repeat("-", 90))

	for _, c := range clusterURLs {
		secretName, err := URIToSecretName("cluster", c.url)
		if err != nil {
			fmt.Printf("  %-20s %-45s 오류: %v\n", c.name, c.url, err)
			continue
		}
		fmt.Printf("  %-20s %-45s %s\n", c.name, c.url, secretName)
	}

	// 클러스터 등록 및 조회
	fmt.Println("\n  클러스터 등록:")
	mgr.AddCluster("us-west-2", "https://k8s-usw2.example.com:6443", "", map[string]string{"env": "prod", "region": "us-west-2"})
	mgr.AddCluster("eu-central-1", "https://k8s-euc1.example.com", "", map[string]string{"env": "prod", "region": "eu-central-1"})
	mgr.AddCluster("dev-cluster", "https://k8s-dev.example.com", "dev-team", nil)

	clusters := mgr.ListClusters()
	fmt.Printf("\n  등록된 클러스터: %d개\n", len(clusters))
	for _, c := range clusters {
		fmt.Printf("    - name=%q server=%q type=%s\n",
			c.Data["name"], c.Data["server"], c.Labels[LabelKeySecretType])
	}

	// 클러스터 조회
	cluster, err := mgr.GetCluster("https://k8s-usw2.example.com:6443")
	if err == nil {
		fmt.Printf("\n  GetCluster(us-west-2): name=%q labels=%v\n", cluster.Data["name"], cluster.Labels)
	}

	// 클러스터 삭제
	mgr.DeleteCluster("https://k8s-dev.example.com")
	fmt.Printf("  삭제 후 클러스터 수: %d개\n", len(mgr.ListClusters()))

	// ─── 저장소 Secret 관리 (RepoURLToSecretName) ────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("저장소 Secret 관리 (RepoURLToSecretName - FNV-32a)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	repoURLs := []struct {
		url, project string
	}{
		{"https://github.com/argoproj/argocd-example-apps", ""},
		{"git@github.com:argoproj/argo-cd.git", ""},
		{"git@github.com:argoproj/argo-cd.git", "my-project"}, // project별 별도 Secret
		{"https://gitlab.com/example/charts.git", ""},
	}

	fmt.Printf("\n  %-50s %-15s %s\n", "저장소 URL", "Project", "Secret 이름 (FNV-32a)")
	fmt.Println("  " + strings.Repeat("-", 90))
	for _, r := range repoURLs {
		secretName := RepoURLToSecretName(repoSecretPrefix, r.url, r.project)
		proj := r.project
		if proj == "" {
			proj = "(없음)"
		}
		fmt.Printf("  %-50s %-15s %s\n", r.url, proj, secretName)
	}

	// 저장소 등록 및 조회
	fmt.Println("\n  저장소 등록:")
	mgr.AddRepo("https://github.com/argoproj/argocd-example-apps", "myuser", "mypassword", "")
	mgr.AddRepo("https://github.com/example/private-charts", "deploy-bot", "gh-token-123", "my-project")
	mgr.AddRepo("git@github.com:example/infra.git", "", "", "")

	repos := mgr.ListRepos()
	fmt.Printf("\n  등록된 저장소: %d개\n", len(repos))
	for _, r := range repos {
		fmt.Printf("    - url=%q project=%q type=%s\n",
			r.Data["url"], r.Data["project"], r.Labels[LabelKeySecretType])
	}

	// 저장소 조회
	repo, err := mgr.GetRepo("https://github.com/argoproj/argocd-example-apps", "")
	if err == nil {
		fmt.Printf("\n  GetRepo: url=%q username=%q\n", repo.Data["url"], repo.Data["username"])
	}

	// ─── 리소스 커스터마이제이션 예시 ───────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("리소스 커스터마이제이션 (resource.customizations)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// 실제 argocd-cm ConfigMap 예시
	fmt.Println(`
  argocd-cm ConfigMap 예시:

  data:
    resource.customizations: |
      apps/Deployment:
        health.lua: |
          hs = {}
          if obj.status ~= nil then
            if obj.status.availableReplicas ~= nil then
              if obj.status.updatedReplicas == obj.spec.replicas and
                 obj.status.availableReplicas == obj.spec.replicas then
                hs.status = "Healthy"
                return hs
              end
            end
          end
          hs.status = "Progressing"
          hs.message = "Waiting for rollout to finish"
          return hs
      networking.k8s.io/Ingress:
        health.lua: |
          hs = {}
          hs.status = "Healthy"
          return hs`)

	customizations := map[string]ResourceOverride{
		"apps/Deployment": {
			HealthLuaScript: `hs = {}
if obj.status.availableReplicas == obj.spec.replicas then
  hs.status = "Healthy"
else
  hs.status = "Progressing"
end
return hs`,
		},
		"networking.k8s.io/Ingress": {
			HealthLuaScript: `hs = {}
hs.status = "Healthy"
return hs`,
		},
	}

	fmt.Println("\n  로드된 리소스 커스터마이제이션:")
	for kind, override := range customizations {
		lines := strings.Count(override.HealthLuaScript, "\n") + 1
		fmt.Printf("    - %s: health.lua (%d줄)\n", kind, lines)
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("\n핵심 개념 요약:")
	fmt.Println("  - ArgoCDSettings: argocd-cm + argocd-secret에서 런타임 로드")
	fmt.Println("  - URIToSecretName: FNV-32a(URI) → 'cluster-<host>-<hash>' 형식")
	fmt.Println("  - RepoURLToSecretName: FNV-32a(url+project) → 'repo-<hash>' 형식")
	fmt.Println("  - 레이블: argocd.argoproj.io/secret-type=cluster/repository")
	fmt.Println("  - 구독 패턴: Subscribe(chan) → 설정 변경 시 별도 고루틴으로 알림")
	fmt.Println("  - TrackingMethod: annotation(기본) | label | annotation+label")
	fmt.Println("  - 리소스 커스터마이제이션: Lua 스크립트로 health 체크 오버라이드")
}
