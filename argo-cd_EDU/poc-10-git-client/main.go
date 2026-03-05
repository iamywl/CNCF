// poc-10-git-client/main.go
//
// Argo CD Git 클라이언트 시뮬레이션
//
// 참조 소스:
//   - util/git/client.go     : Client 인터페이스, nativeGitClient 구조체
//   - util/git/creds.go      : Creds 인터페이스, HTTPSCreds, SSHCreds, GitHubAppCreds, CredsStore
//   - util/io/paths.go       : TempPaths 인터페이스, RandomizedTempPaths
//   - reposerver/repository/lock.go   : repositoryLock (per-repo 직렬화)
//   - reposerver/repository/repository.go : manifest-generate-paths 최적화
//
// 핵심 개념:
//   1. Client 인터페이스: Init, Fetch, Checkout, LsRefs, CommitSHA, RevisionMetadata
//   2. nativeGitClient: git CLI 명령어를 래핑하는 구체 구현
//   3. Creds 타입: SSH, HTTPS (username/password), GitHub App
//   4. CredsStore: 자격증명 캐싱 (Add/Remove/Environ)
//   5. repositoryLock: 동일 저장소 동시 접근 직렬화
//   6. TempPaths: UUID 기반 임시 체크아웃 경로 관리
//   7. 리비전 해석: branch → SHA, tag → SHA, HEAD
//   8. ChangedFiles: 두 리비전 간 변경 파일 감지
//   9. manifest-generate-paths: 변경 없으면 캐시 활용

package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================
// Creds 인터페이스 및 구현
// 참조: util/git/creds.go
//
// type Creds interface {
//     Environ() (io.Closer, []string, error)
//     GetUserInfo(ctx context.Context) (string, string, error)
// }
// ============================================================

// Creds — 저장소 자격증명 인터페이스
type Creds interface {
	Type() string
	Environ() (io.Closer, []string, error)
}

// NopCloser — 자원 해제가 필요 없는 경우
// 실제 소스: util/git/creds.go
type NopCloser struct{}

func (c NopCloser) Close() error { return nil }

// NopCreds — 자격증명 없음 (공개 저장소)
// 실제 소스: type NopCreds struct{} / func (c NopCreds) Environ() ...
type NopCreds struct{}

func (c NopCreds) Type() string { return "none" }
func (c NopCreds) Environ() (io.Closer, []string, error) {
	return NopCloser{}, nil, nil
}

// HTTPSCreds — HTTPS 자격증명 (username/password 또는 bearer token)
// 실제 소스: util/git/creds.go type HTTPSCreds struct { username, password, bearerToken, ... }
type HTTPSCreds struct {
	Username string
	Password string
	Store    CredsStore
}

func (c HTTPSCreds) Type() string { return "https" }
func (c HTTPSCreds) Environ() (io.Closer, []string, error) {
	// 실제 코드: CredsStore.Add(username, password) → credID
	// GIT_ASKPASS 환경변수로 자격증명 주입
	var envVars []string
	if c.Store != nil {
		credID := c.Store.Add(c.Username, c.Password)
		envVars = c.Store.Environ(credID)
		closer := NopCloser{}
		return closer, envVars, nil
	}
	return NopCloser{}, envVars, nil
}

// SSHCreds — SSH 키 기반 자격증명
// 실제 소스: util/git/creds.go type SSHCreds struct { sshPrivateKey, caPath, insecureIgnoreHostKey, proxy }
type SSHCreds struct {
	PrivateKey            string
	InsecureIgnoreHostKey bool
}

func (c SSHCreds) Type() string { return "ssh" }
func (c SSHCreds) Environ() (io.Closer, []string, error) {
	// 실제 코드: SSH 키를 임시 파일에 저장하고 GIT_SSH_COMMAND 설정
	var envVars []string
	sshArgs := "ssh -o StrictHostKeyChecking=no"
	if c.InsecureIgnoreHostKey {
		sshArgs += " -o UserKnownHostsFile=/dev/null"
	}
	envVars = append(envVars, "GIT_SSH_COMMAND="+sshArgs)
	// 실제 코드: GIT_SSH_COMMAND에 -i <keyfile> 추가
	envVars = append(envVars, "GIT_TERMINAL_PROMPT=0")
	return NopCloser{}, envVars, nil
}

// GitHubAppCreds — GitHub App 인증
// 실제 소스: util/git/creds.go type GitHubAppCreds struct { appID, appInstallId, privateKey, baseURL, ... }
type GitHubAppCreds struct {
	AppID         int64
	InstallID     int64
	PrivateKey    string
	BaseURL       string
	// 실제 코드: githubAppTokenCache를 사용하여 토큰을 캐싱
	cachedToken   string
	tokenExpiry   time.Time
}

func (c *GitHubAppCreds) Type() string { return "github-app" }
func (c *GitHubAppCreds) Environ() (io.Closer, []string, error) {
	// 실제 코드: github App JWT → installation token 교환
	// githubAppTokenCache에 토큰을 캐싱하여 재사용
	token := c.getOrFetchToken()
	envVars := []string{
		"GIT_ASKPASS=true",
		// 실제 코드: username=x-access-token, password=<token>
		fmt.Sprintf("ARGOCD_GIT_AUTH_HEADER=Authorization: Bearer %s", token),
	}
	return NopCloser{}, envVars, nil
}

func (c *GitHubAppCreds) getOrFetchToken() string {
	if c.cachedToken != "" && time.Now().Before(c.tokenExpiry) {
		return c.cachedToken
	}
	// 시뮬레이션: 실제 코드는 GitHub API로 installation token 발급
	c.cachedToken = fmt.Sprintf("ghs_simulated_%d_%d", c.AppID, c.InstallID)
	c.tokenExpiry = time.Now().Add(55 * time.Minute)
	return c.cachedToken
}

// ============================================================
// CredsStore — 자격증명 캐시 저장소
// 참조: util/git/creds.go type CredsStore interface { Add/Remove/Environ }
// ============================================================

type CredsStore interface {
	Add(username, password string) string
	Remove(id string)
	Environ(id string) []string
}

// inMemoryCredsStore — 메모리 기반 CredsStore 구현
type inMemoryCredsStore struct {
	mu    sync.Mutex
	creds map[string][2]string // id → [username, password]
}

func newCredsStore() CredsStore {
	return &inMemoryCredsStore{creds: make(map[string][2]string)}
}

func (s *inMemoryCredsStore) Add(username, password string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 실제 코드: uuid.NewRandom() → credID
	id := fmt.Sprintf("cred-%d", rand.Intn(100000))
	s.creds[id] = [2]string{username, password}
	return id
}

func (s *inMemoryCredsStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.creds, id)
}

func (s *inMemoryCredsStore) Environ(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cred, ok := s.creds[id]
	if !ok {
		return nil
	}
	return []string{
		fmt.Sprintf("GIT_USERNAME=%s", cred[0]),
		fmt.Sprintf("GIT_PASSWORD=%s", cred[1]),
	}
}

// ============================================================
// TempPaths — 임시 체크아웃 경로 관리
// 참조: util/io/paths.go
//
// type TempPaths interface {
//     Add(key string, value string)
//     GetPath(key string) (string, error)
//     GetPathIfExists(key string) string
//     GetPaths() map[string]string
// }
//
// RandomizedTempPaths: key → UUID 기반 임시 경로 (멱등성 보장)
// ============================================================

type TempPaths interface {
	Add(key, value string)
	GetPath(key string) (string, error)
	GetPathIfExists(key string) string
	GetPaths() map[string]string
}

type RandomizedTempPaths struct {
	root  string
	paths map[string]string
	lock  sync.RWMutex
}

func NewRandomizedTempPaths(root string) *RandomizedTempPaths {
	return &RandomizedTempPaths{root: root, paths: map[string]string{}}
}

func (p *RandomizedTempPaths) Add(key, value string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.paths[key] = value
}

// GetPath: 기존 경로 반환 또는 UUID 기반 새 경로 생성
// 실제 코드: uuid.NewRandom() → repoPath := filepath.Join(p.root, uniqueId.String())
func (p *RandomizedTempPaths) GetPath(key string) (string, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if val, ok := p.paths[key]; ok {
		return val, nil
	}
	// 시뮬레이션: UUID 대신 sha256 사용
	h := sha256.Sum256([]byte(key + fmt.Sprint(time.Now().UnixNano())))
	uniqueID := fmt.Sprintf("%x", h[:8])
	repoPath := filepath.Join(p.root, uniqueID)
	p.paths[key] = repoPath
	return repoPath, nil
}

func (p *RandomizedTempPaths) GetPathIfExists(key string) string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.paths[key]
}

func (p *RandomizedTempPaths) GetPaths() map[string]string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	result := make(map[string]string, len(p.paths))
	for k, v := range p.paths {
		result[k] = v
	}
	return result
}

// ============================================================
// repositoryLock — 저장소별 직렬화 락
// 참조: reposerver/repository/lock.go
//
// Lock(path, revision, allowConcurrent, init func()) (io.Closer, error)
// - 동일 path+revision이고 allowConcurrent=true이면 병렬 허용
// - 다른 revision이면 현재 작업 완료까지 대기
// ============================================================

type repositoryLock struct {
	mu         sync.Mutex
	stateByKey map[string]*repoState
}

type repoState struct {
	cond            *sync.Cond
	revision        string
	initCloser      io.Closer
	processCount    int
	allowConcurrent bool
}

func newRepositoryLock() *repositoryLock {
	return &repositoryLock{stateByKey: make(map[string]*repoState)}
}

// Lock: 저장소 경로 + 리비전 기반 직렬화
func (r *repositoryLock) Lock(path, revision string, allowConcurrent bool, init func() (io.Closer, error)) (io.Closer, error) {
	r.mu.Lock()
	state, ok := r.stateByKey[path]
	if !ok {
		state = &repoState{cond: sync.NewCond(&sync.Mutex{})}
		r.stateByKey[path] = state
	}
	r.mu.Unlock()

	// Closer: 프로세스 카운트 감소 및 마지막 프로세스가 락 해제
	closer := &lockCloser{state: state}

	for {
		state.cond.L.Lock()
		if state.revision == "" {
			// 진행 중인 작업 없음 → 새 작업 시작
			initCloser, err := init()
			if err != nil {
				state.cond.L.Unlock()
				return nil, fmt.Errorf("저장소 초기화 실패: %w", err)
			}
			state.initCloser = initCloser
			state.revision = revision
			state.processCount = 1
			state.allowConcurrent = allowConcurrent
			state.cond.L.Unlock()
			return closer, nil
		} else if state.revision == revision && state.allowConcurrent && allowConcurrent {
			// 동일 리비전 + 병렬 허용 → 카운트 증가 후 진행
			state.processCount++
			state.cond.L.Unlock()
			return closer, nil
		}
		// 다른 리비전 처리 중 → 완료 대기
		state.cond.Wait()
		state.cond.L.Unlock()
	}
}

type lockCloser struct {
	state *repoState
	once  sync.Once
}

func (c *lockCloser) Close() error {
	var err error
	c.once.Do(func() {
		c.state.cond.L.Lock()
		c.state.processCount--
		notify := false
		if c.state.processCount == 0 {
			notify = true
			c.state.revision = ""
			if c.state.initCloser != nil {
				err = c.state.initCloser.Close()
			}
		}
		c.state.cond.L.Unlock()
		if notify {
			c.state.cond.Broadcast()
		}
	})
	return err
}

// ============================================================
// RevisionMetadata — 커밋 메타데이터
// 참조: util/git/client.go type RevisionMetadata struct
// ============================================================

type RevisionMetadata struct {
	Author  string
	Date    time.Time
	Tags    []string
	Message string
}

// Refs — 저장소 브랜치/태그 목록
// 참조: util/git/client.go type Refs struct { Branches, Tags []string }
type Refs struct {
	Branches []string
	Tags     []string
}

// ============================================================
// Git Client 인터페이스 및 시뮬레이션 구현
// 참조: util/git/client.go type Client interface
//
// type Client interface {
//     Root() string
//     Init() error
//     Fetch(revision string, depth int64) error
//     Checkout(revision string, submoduleEnabled bool) (string, error)
//     LsRefs() (*Refs, error)
//     LsRemote(revision string) (string, error)
//     CommitSHA() (string, error)
//     RevisionMetadata(revision string) (*RevisionMetadata, error)
//     VerifyCommitSignature(string) (string, error)
//     ChangedFiles(revision string, targetRevision string) ([]string, error)
//     IsRevisionPresent(revision string) bool
// }
// ============================================================

type Client interface {
	Root() string
	Init() error
	Fetch(revision string, depth int64) error
	Checkout(revision string, submoduleEnabled bool) (string, error)
	LsRefs() (*Refs, error)
	LsRemote(revision string) (string, error)
	CommitSHA() (string, error)
	RevisionMetadata(revision string) (*RevisionMetadata, error)
	VerifyCommitSignature(revision string) (string, error)
	ChangedFiles(revision, targetRevision string) ([]string, error)
	IsRevisionPresent(revision string) bool
}

// simulatedGitClient — nativeGitClient 시뮬레이션
// 실제 코드: util/git/client.go type nativeGitClient struct
type simulatedGitClient struct {
	repoURL   string
	root      string
	creds     Creds
	insecure  bool
	// 시뮬레이션용 내부 상태
	refs      *Refs
	commits   map[string]*RevisionMetadata // SHA → metadata
	shaByRef  map[string]string            // branch/tag → SHA
	files     map[string][]string          // SHA → 파일 목록
	checkedOut string                      // 현재 체크아웃된 SHA
}

// NewClient — 저장소 URL + Creds로 클라이언트 생성
// 실제 코드: util/git/client.go func NewClient(rawRepoURL, creds, ...)
func NewClient(repoURL string, creds Creds) Client {
	// 실제 코드: NormalizeGitURL(rawRepoURL)로 정규화
	// root := filepath.Join(os.TempDir(), r.ReplaceAllString(normalizedGitURL, "_"))
	root := filepath.Join("/tmp/argocd", strings.ReplaceAll(
		strings.TrimPrefix(strings.TrimPrefix(repoURL, "https://"), "git@"),
		"/", "_",
	))

	// 시뮬레이션용 저장소 데이터
	return &simulatedGitClient{
		repoURL: repoURL,
		root:    root,
		creds:   creds,
		refs: &Refs{
			Branches: []string{"main", "develop", "feature/new-ui"},
			Tags:     []string{"v1.0.0", "v1.1.0", "v2.0.0"},
		},
		commits: map[string]*RevisionMetadata{
			"abc123def456": {
				Author:  "Alice <alice@example.com>",
				Date:    time.Now().Add(-48 * time.Hour),
				Tags:    []string{"v1.0.0"},
				Message: "feat: initial release",
			},
			"bcd234ef5678": {
				Author:  "Bob <bob@example.com>",
				Date:    time.Now().Add(-24 * time.Hour),
				Tags:    []string{"v1.1.0"},
				Message: "fix: resolve memory leak",
			},
			"cde345fg6789": {
				Author:  "Carol <carol@example.com>",
				Date:    time.Now().Add(-1 * time.Hour),
				Tags:    []string{},
				Message: "feat: add new dashboard",
			},
			"def456gh7890": {
				Author:  "Dave <dave@example.com>",
				Date:    time.Now(),
				Tags:    []string{"v2.0.0"},
				Message: "chore: bump version to v2.0.0",
			},
		},
		shaByRef: map[string]string{
			"main":            "def456gh7890",
			"develop":         "cde345fg6789",
			"feature/new-ui":  "bcd234ef5678",
			"HEAD":            "def456gh7890",
			"v1.0.0":          "abc123def456",
			"v1.1.0":          "bcd234ef5678",
			"v2.0.0":          "def456gh7890",
		},
		files: map[string][]string{
			"abc123def456": {"app/main.go", "app/handler.go", "config/config.yaml"},
			"bcd234ef5678": {"app/main.go", "app/handler.go", "config/config.yaml", "app/memory.go"},
			"cde345fg6789": {"app/main.go", "app/handler.go", "app/memory.go", "ui/dashboard.tsx"},
			"def456gh7890": {"app/main.go", "app/handler.go", "app/memory.go", "ui/dashboard.tsx", "VERSION"},
		},
	}
}

func (c *simulatedGitClient) Root() string { return c.root }

// Init — 저장소 초기화 (git init + remote add)
// 실제 코드: git.go에서 git init, git remote add origin <url> 실행
func (c *simulatedGitClient) Init() error {
	fmt.Printf("  [git] Init: %s → %s\n", c.repoURL, c.root)
	// 실제 코드: gitConfigEnv (GIT_CONFIG_COUNT 등 builtin 설정) 적용
	return nil
}

// Fetch — 원격 저장소에서 최신 내용 가져오기
// 실제 코드: git fetch origin <revision> --depth <depth>
func (c *simulatedGitClient) Fetch(revision string, depth int64) error {
	_, envVars, err := c.creds.Environ()
	if err != nil {
		return fmt.Errorf("자격증명 환경변수 설정 실패: %w", err)
	}
	fmt.Printf("  [git] Fetch: revision=%q depth=%d creds-type=%s env-vars=%d개\n",
		revision, depth, c.creds.Type(), len(envVars))
	return nil
}

// Checkout — 특정 리비전으로 체크아웃
// 실제 코드: Checkout(revision, submoduleEnabled) returns (actualSHA, error)
func (c *simulatedGitClient) Checkout(revision string, submoduleEnabled bool) (string, error) {
	sha, err := c.LsRemote(revision)
	if err != nil {
		return "", err
	}
	c.checkedOut = sha
	fmt.Printf("  [git] Checkout: revision=%q → sha=%s submodule=%v\n",
		revision, sha[:12], submoduleEnabled)
	return sha, nil
}

// LsRefs — 저장소 브랜치/태그 목록 조회
// 실제 코드: git ls-remote --heads --tags <url>
func (c *simulatedGitClient) LsRefs() (*Refs, error) {
	return c.refs, nil
}

// LsRemote — 리비전(브랜치/태그/SHA)을 실제 커밋 SHA로 변환
// 실제 코드: git ls-remote <url> <revision> → SHA 파싱
func (c *simulatedGitClient) LsRemote(revision string) (string, error) {
	// 이미 full SHA인 경우
	if len(revision) == 12 || len(revision) == 40 {
		for sha := range c.commits {
			if strings.HasPrefix(sha, revision) {
				return sha, nil
			}
		}
	}
	// 브랜치/태그/HEAD 조회
	if sha, ok := c.shaByRef[revision]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("리비전 %q 을 찾을 수 없음", revision)
}

// CommitSHA — 현재 체크아웃된 커밋의 SHA 반환
// 실제 코드: git rev-parse HEAD
func (c *simulatedGitClient) CommitSHA() (string, error) {
	if c.checkedOut == "" {
		return "", fmt.Errorf("체크아웃되지 않은 상태")
	}
	return c.checkedOut, nil
}

// RevisionMetadata — 커밋 메타데이터 조회
// 실제 코드: git log -n 1 --pretty='format:%an <%ae>|%ad|%s' <revision>
func (c *simulatedGitClient) RevisionMetadata(revision string) (*RevisionMetadata, error) {
	sha, err := c.LsRemote(revision)
	if err != nil {
		return nil, err
	}
	meta, ok := c.commits[sha]
	if !ok {
		return nil, fmt.Errorf("커밋 메타데이터 없음: %s", sha)
	}
	return meta, nil
}

// VerifyCommitSignature — GPG 서명 검증
// 실제 코드: git verify-commit <revision>
// util/gpg 패키지를 통해 신뢰된 키로 서명 확인
func (c *simulatedGitClient) VerifyCommitSignature(revision string) (string, error) {
	sha, err := c.LsRemote(revision)
	if err != nil {
		return "", err
	}
	// 시뮬레이션: 실제 코드는 gpg.VerifyCommitSignature() 호출
	fmt.Printf("  [git] GPG 서명 검증: sha=%s\n", sha[:12])
	return fmt.Sprintf("[GNUPG:] GOODSIG ABCDEF1234567890 Alice <alice@example.com>"), nil
}

// ChangedFiles — 두 리비전 간 변경된 파일 목록
// 실제 코드: git diff --name-only <revision>..<targetRevision>
// manifest-generate-paths 최적화에 사용
func (c *simulatedGitClient) ChangedFiles(revision, targetRevision string) ([]string, error) {
	fromSHA, err := c.LsRemote(revision)
	if err != nil {
		return nil, err
	}
	toSHA, err := c.LsRemote(targetRevision)
	if err != nil {
		return nil, err
	}

	fromFiles := toSet(c.files[fromSHA])
	toFiles := toSet(c.files[toSHA])

	// 변경된 파일: 추가되거나 삭제된 파일
	var changed []string
	for f := range toFiles {
		if !fromFiles[f] {
			changed = append(changed, f)
		}
	}
	for f := range fromFiles {
		if !toFiles[f] {
			changed = append(changed, "DELETED:"+f)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// IsRevisionPresent — 로컬에 해당 리비전이 있는지 확인
// 실제 코드: git cat-file -t <revision> 2>/dev/null
func (c *simulatedGitClient) IsRevisionPresent(revision string) bool {
	_, ok := c.commits[revision]
	return ok
}

func toSet(files []string) map[string]bool {
	result := make(map[string]bool, len(files))
	for _, f := range files {
		result[f] = true
	}
	return result
}

// ============================================================
// manifest-generate-paths 최적화
// 참조: reposerver/repository/repository.go:1495
//
// 'argocd.argoproj.io/manifest-generate-paths' 어노테이션:
// 지정된 경로에 변경이 없으면 Git fetch를 건너뜀 → 캐시 활용
// ============================================================

type ManifestGeneratePathsOpt struct {
	Paths []string // e.g. [".", "config/"]
}

// ShouldSkipGeneration — 변경된 파일이 manifest-generate-paths에 포함되지 않으면 생성 스킵
func ShouldSkipGeneration(changedFiles []string, opt ManifestGeneratePathsOpt) bool {
	if len(opt.Paths) == 0 || len(changedFiles) == 0 {
		return false
	}
	for _, changed := range changedFiles {
		if strings.HasPrefix(changed, "DELETED:") {
			changed = strings.TrimPrefix(changed, "DELETED:")
		}
		for _, watchPath := range opt.Paths {
			if watchPath == "." || strings.HasPrefix(changed, watchPath) {
				return false // 관련 경로가 변경됨 → 생성 필요
			}
		}
	}
	return true // 관련 경로 변경 없음 → 스킵
}

// ============================================================
// Main: 시나리오 시연
// ============================================================

func main() {
	fmt.Println("=======================================================")
	fmt.Println("Argo CD Git 클라이언트 시뮬레이션")
	fmt.Println("=======================================================")

	// ─── 시나리오 1: 자격증명 타입별 환경변수 ───────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 1: Creds 타입별 환경변수")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	credsStore := newCredsStore()

	httpsCreds := HTTPSCreds{Username: "argocd-user", Password: "secret123", Store: credsStore}
	sshCreds := SSHCreds{PrivateKey: "-----BEGIN RSA PRIVATE KEY-----...", InsecureIgnoreHostKey: false}
	ghAppCreds := &GitHubAppCreds{AppID: 123456, InstallID: 789012, PrivateKey: "pem-key", BaseURL: "https://github.com"}
	nopCreds := NopCreds{}

	for _, creds := range []Creds{nopCreds, httpsCreds, sshCreds, ghAppCreds} {
		_, envVars, _ := creds.Environ()
		fmt.Printf("  [%12s] 환경변수 %d개: %v\n", creds.Type(), len(envVars), envVars)
	}

	// ─── 시나리오 2: TempPaths (UUID 기반 경로 관리) ────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 2: RandomizedTempPaths — 저장소 경로 관리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	tempPaths := NewRandomizedTempPaths("/tmp/argocd-repos")

	repos := []string{
		"https://github.com/argoproj/argocd-example-apps",
		"https://github.com/example/my-charts",
	}
	for _, repo := range repos {
		path1, _ := tempPaths.GetPath(repo)
		path2, _ := tempPaths.GetPath(repo) // 동일 키 → 동일 경로 반환 (멱등성)
		fmt.Printf("  repo=%q\n    path1=%s\n    path2=%s\n    동일=%v\n",
			repo, path1, path2, path1 == path2)
	}

	// ─── 시나리오 3: repositoryLock (직렬화) ────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 3: repositoryLock — 동일 저장소 직렬화")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	repoLock := newRepositoryLock()
	repoPath := "/tmp/argocd-repos/example-app"
	var wg sync.WaitGroup

	// 동일 리비전 + allowConcurrent=true → 병렬 허용
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			closer, err := repoLock.Lock(repoPath, "abc123", true, func() (io.Closer, error) {
				fmt.Printf("  [Lock] goroutine-%d: 초기화 실행 (revision=abc123)\n", id)
				return NopCloser{}, nil
			})
			if err != nil {
				fmt.Printf("  [Lock] goroutine-%d 실패: %v\n", id, err)
				return
			}
			fmt.Printf("  [Lock] goroutine-%d: 작업 시작 (allowConcurrent=true)\n", id)
			time.Sleep(10 * time.Millisecond)
			closer.Close()
			fmt.Printf("  [Lock] goroutine-%d: 완료\n", id)
		}(i)
	}
	wg.Wait()

	// ─── 시나리오 4: 전체 Git 클라이언트 플로우 ─────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 4: Git 클라이언트 전체 플로우")
	fmt.Println("  init → fetch → checkout → resolve → metadata → changed files")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	client := NewClient("https://github.com/argoproj/argocd-example-apps", httpsCreds)

	fmt.Printf("\n  Root: %s\n", client.Root())

	// 1. Init
	if err := client.Init(); err != nil {
		fmt.Printf("  Init 실패: %v\n", err)
	}

	// 2. LsRefs: 브랜치/태그 목록
	refs, _ := client.LsRefs()
	fmt.Printf("  LsRefs: branches=%v tags=%v\n", refs.Branches, refs.Tags)

	// 3. Fetch
	client.Fetch("HEAD", 0)

	// 4. 리비전 해석: branch → SHA
	for _, rev := range []string{"main", "v1.0.0", "v2.0.0", "HEAD"} {
		sha, _ := client.LsRemote(rev)
		fmt.Printf("  LsRemote(%q) → %s\n", rev, sha)
	}

	// 5. Checkout
	sha, _ := client.Checkout("main", false)
	fmt.Printf("  Checkout(main) → %s\n", sha)

	// 6. CommitSHA
	currentSHA, _ := client.CommitSHA()
	fmt.Printf("  CommitSHA() → %s\n", currentSHA)

	// 7. RevisionMetadata
	meta, _ := client.RevisionMetadata("v1.1.0")
	fmt.Printf("  RevisionMetadata(v1.1.0): author=%q msg=%q\n", meta.Author, meta.Message)

	// 8. VerifyCommitSignature
	gpgResult, _ := client.VerifyCommitSignature("v1.0.0")
	fmt.Printf("  VerifyCommitSignature(v1.0.0): %s\n", gpgResult[:40])

	// 9. ChangedFiles: v1.0.0 → v1.1.0
	changed, _ := client.ChangedFiles("v1.0.0", "v1.1.0")
	fmt.Printf("  ChangedFiles(v1.0.0→v1.1.0): %v\n", changed)

	// ─── 시나리오 5: manifest-generate-paths 최적화 ─────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 5: manifest-generate-paths 최적화")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// v1.0.0 → v1.1.0 변경 파일
	changed10to11, _ := client.ChangedFiles("v1.0.0", "v1.1.0")
	// v1.1.0 → v2.0.0 변경 파일
	changed11to20, _ := client.ChangedFiles("v1.1.0", "v2.0.0")

	tests := []struct {
		label   string
		changed []string
		opt     ManifestGeneratePathsOpt
	}{
		{
			label:   "app/ 경로 감시, app/memory.go 변경",
			changed: changed10to11,
			opt:     ManifestGeneratePathsOpt{Paths: []string{"app/"}},
		},
		{
			label:   "ui/ 경로 감시, app/memory.go만 변경",
			changed: changed10to11,
			opt:     ManifestGeneratePathsOpt{Paths: []string{"ui/"}},
		},
		{
			label:   "ui/ 경로 감시, ui/dashboard.tsx 변경",
			changed: changed11to20,
			opt:     ManifestGeneratePathsOpt{Paths: []string{"ui/"}},
		},
		{
			label:   "경로 미설정 (모든 변경 감지)",
			changed: changed10to11,
			opt:     ManifestGeneratePathsOpt{Paths: nil},
		},
	}

	for _, tt := range tests {
		skip := ShouldSkipGeneration(tt.changed, tt.opt)
		action := "생성 필요"
		if skip {
			action = "스킵 (캐시 사용)"
		}
		fmt.Printf("  [%s]\n    변경파일=%v\n    → %s\n", tt.label, tt.changed, action)
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("\n핵심 개념 요약:")
	fmt.Println("  - nativeGitClient: git CLI 명령어를 exec.Command로 실행")
	fmt.Println("  - Creds.Environ(): GIT_SSH_COMMAND / GIT_ASKPASS 등 주입")
	fmt.Println("  - CredsStore: 자격증명을 ID로 캐싱, 다수 저장소에서 재사용")
	fmt.Println("  - RandomizedTempPaths: UUID로 저장소별 고유 경로, 키-경로 멱등 매핑")
	fmt.Println("  - repositoryLock: 동일 저장소 동시 fetch/checkout 직렬화")
	fmt.Println("  - manifest-generate-paths: 관련 경로 변경 없으면 캐시 활용")
}
