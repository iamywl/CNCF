// poc-13-scm-polling: Jenkins SCM 변경 감지 및 폴링 스케줄 시뮬레이션
//
// Jenkins 소스코드 참조:
//   - hudson.scm.SCM (abstract, Describable, ExtensionPoint)
//   - hudson.scm.PollingResult / PollingResult.Change
//   - hudson.scm.SCMRevisionState
//   - hudson.scm.ChangeLogSet / ChangeLogSet.Entry
//   - hudson.scm.NullSCM
//   - hudson.triggers.SCMTrigger
//   - jenkins.triggers.SCMTriggerItem
//
// 핵심 원리:
//   1) SCM은 추상 클래스로 checkout(), calcRevisionsFromBuild(),
//      compareRemoteRevisionWith(), createChangeLogParser()를 정의
//   2) SCMTrigger는 cron 기반 스케줄로 주기적으로 SCM.poll()을 호출
//   3) poll()은 calcRevisionsFromBuild()로 베이스라인을 얻고,
//      compareRemoteRevisionWith()로 원격 변경을 감지하여 PollingResult를 반환
//   4) PollingResult.Change는 NONE, INSIGNIFICANT, SIGNIFICANT, INCOMPARABLE 4단계
//   5) SIGNIFICANT 이상이면 빌드를 스케줄링
//   6) NullSCM은 SCM 미설정 시 기본값 (항상 NO_CHANGES 반환)
//
// 실행: go run main.go

package main

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. SCMRevisionState: 리비전 상태 (불변)
// =============================================================================
// Jenkins: hudson.scm.SCMRevisionState (abstract class, Action)
// - 리포지토리의 특정 시점 상태를 표현
// - 빌드 완료 후 Action으로 저장되어 다음 폴링의 baseline이 됨
// - NONE: 초기 상태 (아직 빌드가 없음)

// RevisionState는 SCM 리포지토리의 특정 시점 상태를 나타낸다.
// Jenkins의 SCMRevisionState에 대응한다.
type RevisionState struct {
	RevisionID string    // 리비전 식별자 (Git: commit hash, SVN: revision number)
	Timestamp  time.Time // 이 리비전의 타임스탬프
	Branch     string    // 브랜치 이름
}

// String은 리비전 상태를 문자열로 표현한다.
func (r *RevisionState) String() string {
	if r == nil {
		return "NONE"
	}
	return fmt.Sprintf("Rev[%s@%s (%s)]", r.RevisionID[:8], r.Branch,
		r.Timestamp.Format("15:04:05"))
}

// NoneRevisionState는 Jenkins의 SCMRevisionState.NONE에 대응한다.
// 아직 빌드가 한 번도 수행되지 않은 초기 상태를 나타낸다.
var NoneRevisionState *RevisionState = nil

// =============================================================================
// 2. PollingResult: 폴링 결과
// =============================================================================
// Jenkins: hudson.scm.PollingResult
// - baseline: 이전 폴링의 리비전 상태
// - remote:   현재 원격 리포지토리의 상태
// - change:   변경의 정도 (Change enum)
//
// Jenkins: PollingResult.Change enum
// - NONE: 변경 없음 (두 상태가 동일)
// - INSIGNIFICANT: 변경은 있으나 빌드 불필요 (예: 무시 패턴에 해당하는 파일만 변경)
// - SIGNIFICANT: 빌드가 필요한 변경
// - INCOMPARABLE: 상태를 비교할 수 없음 (즉시 빌드)

// Change는 폴링 결과의 변경 정도를 나타낸다.
type Change int

const (
	ChangeNone          Change = iota // 변경 없음
	ChangeInsignificant               // 변경은 있으나 빌드 불필요
	ChangeSignificant                  // 빌드 필요한 유의미한 변경
	ChangeIncomparable                 // 비교 불가능 (즉시 빌드)
)

// String은 Change를 문자열로 반환한다.
func (c Change) String() string {
	switch c {
	case ChangeNone:
		return "NONE"
	case ChangeInsignificant:
		return "INSIGNIFICANT"
	case ChangeSignificant:
		return "SIGNIFICANT"
	case ChangeIncomparable:
		return "INCOMPARABLE"
	default:
		return "UNKNOWN"
	}
}

// PollingResult는 SCM 폴링의 결과를 나타낸다.
// Jenkins의 PollingResult에 대응한다.
type PollingResult struct {
	Baseline *RevisionState // 비교 기준 (이전 폴링 결과)
	Remote   *RevisionState // 현재 원격 상태
	Change   Change         // 변경 정도
}

// HasChanges는 빌드를 트리거할 만한 변경이 있는지 확인한다.
// Jenkins에서 change.ordinal() > Change.INSIGNIFICANT.ordinal()과 동일.
func (pr *PollingResult) HasChanges() bool {
	return pr.Change > ChangeInsignificant
}

// String은 PollingResult를 문자열로 표현한다.
func (pr *PollingResult) String() string {
	return fmt.Sprintf("PollingResult{baseline=%v, remote=%v, change=%s}",
		pr.Baseline, pr.Remote, pr.Change)
}

// 미리 정의된 폴링 결과 상수들
// Jenkins: PollingResult.NO_CHANGES, PollingResult.SIGNIFICANT, PollingResult.BUILD_NOW
var (
	NoChanges      = &PollingResult{Change: ChangeNone}
	Significant    = &PollingResult{Change: ChangeSignificant}
	BuildNow       = &PollingResult{Change: ChangeIncomparable}
)

// =============================================================================
// 3. ChangeLogEntry: 변경 로그 항목
// =============================================================================
// Jenkins: hudson.scm.ChangeLogSet.Entry
// - getMsg(): 커밋 메시지
// - getAuthor(): 작성자
// - getAffectedPaths(): 변경된 파일 경로
// - getCommitId(): 커밋 ID
// - getTimestamp(): 타임스탬프
//
// Jenkins: ChangeLogSet.AffectedFile
// - getPath(): 파일 경로
// - getEditType(): ADD/EDIT/DELETE

// EditType은 파일 변경 유형을 나타낸다.
type EditType int

const (
	EditAdd    EditType = iota // 파일 추가
	EditModify                 // 파일 수정
	EditDelete                 // 파일 삭제
)

// String은 EditType을 문자열로 반환한다.
func (e EditType) String() string {
	switch e {
	case EditAdd:
		return "ADD"
	case EditModify:
		return "MODIFY"
	case EditDelete:
		return "DELETE"
	default:
		return "UNKNOWN"
	}
}

// AffectedFile은 변경된 파일 정보를 나타낸다.
// Jenkins의 ChangeLogSet.AffectedFile 인터페이스에 대응한다.
type AffectedFile struct {
	Path     string   // 파일 경로 (예: "src/main/java/Foo.java")
	EditType EditType // 변경 유형
}

// ChangeLogEntry는 단일 커밋을 나타낸다.
// Jenkins의 ChangeLogSet.Entry에 대응한다.
type ChangeLogEntry struct {
	CommitID      string         // 커밋 해시 (Jenkins: getCommitId())
	Author        string         // 커밋 작성자 (Jenkins: getAuthor())
	Message       string         // 커밋 메시지 (Jenkins: getMsg())
	Timestamp     time.Time      // 커밋 시간 (Jenkins: getTimestamp())
	AffectedFiles []AffectedFile // 변경된 파일 목록 (Jenkins: getAffectedFiles())
}

// AffectedPaths는 변경된 파일의 경로 목록을 반환한다.
// Jenkins의 Entry.getAffectedPaths()에 대응한다.
func (e *ChangeLogEntry) AffectedPaths() []string {
	paths := make([]string, len(e.AffectedFiles))
	for i, f := range e.AffectedFiles {
		paths[i] = f.Path
	}
	return paths
}

// String은 엔트리를 문자열로 표현한다.
func (e *ChangeLogEntry) String() string {
	return fmt.Sprintf("[%s] %s: %s (%d files)",
		e.CommitID[:8], e.Author, e.Message, len(e.AffectedFiles))
}

// =============================================================================
// 4. ChangeLogSet: 빌드의 변경 로그 집합
// =============================================================================
// Jenkins: hudson.scm.ChangeLogSet<T extends Entry>
// - getRun(): 이 변경로그가 속한 빌드
// - isEmptySet(): 변경 없음 여부
// - getItems(): Entry 배열
// - getKind(): SCM 종류 식별자 ("git", "svn" 등)

// ChangeLogSet은 한 빌드에서의 소스 변경 목록이다.
// Jenkins의 ChangeLogSet에 대응한다.
type ChangeLogSet struct {
	BuildNumber int               // 이 변경로그가 속한 빌드 번호
	Kind        string            // SCM 종류 ("git", "svn", "mercurial")
	Entries     []*ChangeLogEntry // 커밋 목록 (최신 순)
}

// IsEmptySet은 변경 로그가 비어있는지 확인한다.
func (cls *ChangeLogSet) IsEmptySet() bool {
	return len(cls.Entries) == 0
}

// Items는 모든 엔트리를 반환한다.
func (cls *ChangeLogSet) Items() []*ChangeLogEntry {
	return cls.Entries
}

// =============================================================================
// 5. SCM 인터페이스: 소스 코드 관리 추상화
// =============================================================================
// Jenkins: hudson.scm.SCM (abstract class)
// 핵심 메서드:
//   - checkout(): 소스 코드 체크아웃
//   - calcRevisionsFromBuild(): 빌드에서 리비전 상태 계산
//   - compareRemoteRevisionWith(): 원격 변경 감지
//   - createChangeLogParser(): 변경 로그 파서 생성
//   - supportsPolling(): 폴링 지원 여부
//   - requiresWorkspaceForPolling(): 폴링에 워크스페이스 필요 여부
//   - getType(): SCM 타입 이름
//   - getKey(): SCM 설정 구분 키

// SCM은 소스 코드 관리 시스템의 추상 인터페이스이다.
// Jenkins의 abstract class SCM에 대응한다.
type SCM interface {
	// GetType은 SCM 타입 이름을 반환한다. (Jenkins: getType())
	GetType() string

	// GetKey는 SCM 설정을 구분하는 키를 반환한다. (Jenkins: getKey())
	GetKey() string

	// SupportsPolling은 폴링 지원 여부를 반환한다. (Jenkins: supportsPolling())
	SupportsPolling() bool

	// RequiresWorkspaceForPolling은 폴링에 워크스페이스가 필요한지 반환한다.
	RequiresWorkspaceForPolling() bool

	// Checkout은 소스 코드를 체크아웃한다. (Jenkins: checkout())
	// 반환값: 변경 로그 셋
	Checkout(buildNumber int, workspace string) (*ChangeLogSet, error)

	// CalcRevisionsFromBuild는 빌드의 리비전 상태를 계산한다.
	// Jenkins: calcRevisionsFromBuild(Run, FilePath, Launcher, TaskListener)
	CalcRevisionsFromBuild(buildNumber int) *RevisionState

	// CompareRemoteRevisionWith는 원격 리포지토리와 베이스라인을 비교한다.
	// Jenkins: compareRemoteRevisionWith(Job, Launcher, FilePath, TaskListener, SCMRevisionState)
	CompareRemoteRevisionWith(baseline *RevisionState) *PollingResult

	// Poll은 SCM 변경을 확인하는 편의 메서드이다.
	// Jenkins: SCM.poll() - calcRevisionsFromBuild + compareRemoteRevisionWith 조합
	Poll(lastBuildNumber int) *PollingResult
}

// =============================================================================
// 6. NullSCM: SCM 미설정 기본값
// =============================================================================
// Jenkins: hudson.scm.NullSCM
// - SCM이 설정되지 않은 Job의 기본값
// - calcRevisionsFromBuild()는 nil 반환
// - compareRemoteRevisionWith()는 항상 NO_CHANGES 반환
// - checkout()은 빈 변경 로그만 생성

// NullSCM은 SCM이 설정되지 않았을 때의 기본 구현이다.
type NullSCM struct{}

func (n *NullSCM) GetType() string                          { return "hudson.scm.NullSCM" }
func (n *NullSCM) GetKey() string                           { return n.GetType() }
func (n *NullSCM) SupportsPolling() bool                    { return true }
func (n *NullSCM) RequiresWorkspaceForPolling() bool        { return false }
func (n *NullSCM) CalcRevisionsFromBuild(buildNum int) *RevisionState { return nil }

func (n *NullSCM) CompareRemoteRevisionWith(baseline *RevisionState) *PollingResult {
	return NoChanges
}

func (n *NullSCM) Checkout(buildNumber int, workspace string) (*ChangeLogSet, error) {
	// NullSCM은 빈 변경 로그를 생성한다
	return &ChangeLogSet{
		BuildNumber: buildNumber,
		Kind:        "none",
		Entries:     nil,
	}, nil
}

func (n *NullSCM) Poll(lastBuildNumber int) *PollingResult {
	return NoChanges
}

// =============================================================================
// 7. GitSCM: Git SCM 시뮬레이션
// =============================================================================
// 실제 Jenkins에서는 Git 플러그인(jenkins-plugin/git)이 SCM을 구현한다.
// 여기서는 Git의 핵심 동작을 시뮬레이션한다:
//   - 리포지토리 URL, 브랜치, 인증 정보를 설정
//   - checkout 시 git clone/pull 시뮬레이션
//   - 리비전은 SHA-256 해시 기반
//   - 변경 감지는 로컬 리비전 vs 원격 리비전 비교

// GitRepository는 시뮬레이션용 Git 리포지토리이다.
type GitRepository struct {
	URL       string
	Commits   []*GitCommit
	mu        sync.Mutex
}

// GitCommit은 Git 커밋을 나타낸다.
type GitCommit struct {
	Hash          string
	Author        string
	Message       string
	Timestamp     time.Time
	Branch        string
	AffectedFiles []AffectedFile
}

// NewGitRepository는 새 Git 리포지토리를 생성한다.
func NewGitRepository(url string) *GitRepository {
	return &GitRepository{
		URL:     url,
		Commits: make([]*GitCommit, 0),
	}
}

// AddCommit은 리포지토리에 새 커밋을 추가한다.
func (r *GitRepository) AddCommit(author, message, branch string, files []AffectedFile) *GitCommit {
	r.mu.Lock()
	defer r.mu.Unlock()

	// SHA-256 해시 생성 (실제 Git과 유사)
	data := fmt.Sprintf("%s:%s:%s:%d", author, message, branch, time.Now().UnixNano())
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(data)))

	commit := &GitCommit{
		Hash:          hash,
		Author:        author,
		Message:       message,
		Timestamp:     time.Now(),
		Branch:        branch,
		AffectedFiles: files,
	}
	r.Commits = append(r.Commits, commit)
	return commit
}

// GetLatestCommit은 특정 브랜치의 최신 커밋을 반환한다.
func (r *GitRepository) GetLatestCommit(branch string) *GitCommit {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := len(r.Commits) - 1; i >= 0; i-- {
		if r.Commits[i].Branch == branch {
			return r.Commits[i]
		}
	}
	return nil
}

// GetCommitsSince는 특정 커밋 이후의 변경 목록을 반환한다.
func (r *GitRepository) GetCommitsSince(sinceHash string, branch string) []*GitCommit {
	r.mu.Lock()
	defer r.mu.Unlock()

	var result []*GitCommit
	found := sinceHash == ""
	for _, c := range r.Commits {
		if c.Branch != branch {
			continue
		}
		if found {
			result = append(result, c)
		}
		if c.Hash == sinceHash {
			found = true
		}
	}
	return result
}

// GitSCM은 Git SCM 구현이다.
type GitSCM struct {
	RepositoryURL string
	Branch        string
	Repo          *GitRepository

	// 제외 패턴: 이 패턴에 매칭되는 파일만 변경된 경우 INSIGNIFICANT로 처리
	ExcludedPaths []string

	// 마지막으로 알려진 리비전 (각 빌드별)
	buildRevisions map[int]string
}

// NewGitSCM은 새 GitSCM을 생성한다.
func NewGitSCM(repoURL, branch string, repo *GitRepository) *GitSCM {
	return &GitSCM{
		RepositoryURL:  repoURL,
		Branch:         branch,
		Repo:           repo,
		ExcludedPaths:  make([]string, 0),
		buildRevisions: make(map[int]string),
	}
}

func (g *GitSCM) GetType() string { return "hudson.plugins.git.GitSCM" }
func (g *GitSCM) GetKey() string {
	return fmt.Sprintf("%s:%s@%s", g.GetType(), g.RepositoryURL, g.Branch)
}
func (g *GitSCM) SupportsPolling() bool             { return true }
func (g *GitSCM) RequiresWorkspaceForPolling() bool  { return false } // Git은 워크스페이스 없이 폴링 가능

// CalcRevisionsFromBuild는 빌드에서 체크아웃된 리비전 상태를 계산한다.
// Jenkins에서 빌드 완료 후 호출되어 Action으로 저장된다.
func (g *GitSCM) CalcRevisionsFromBuild(buildNumber int) *RevisionState {
	hash, ok := g.buildRevisions[buildNumber]
	if !ok {
		return nil
	}
	latest := g.Repo.GetLatestCommit(g.Branch)
	if latest == nil {
		return nil
	}
	return &RevisionState{
		RevisionID: hash,
		Timestamp:  latest.Timestamp,
		Branch:     g.Branch,
	}
}

// isExcludedChange는 변경이 제외 패턴에만 해당하는지 확인한다.
// Jenkins Git 플러그인에서는 excludedRegions 설정으로 특정 경로의 변경을
// 무시할 수 있다. 해당 변경만 있으면 INSIGNIFICANT를 반환한다.
func (g *GitSCM) isExcludedChange(files []AffectedFile) bool {
	if len(g.ExcludedPaths) == 0 {
		return false
	}
	for _, f := range files {
		excluded := false
		for _, pattern := range g.ExcludedPaths {
			if strings.Contains(f.Path, pattern) {
				excluded = true
				break
			}
		}
		if !excluded {
			return false // 제외되지 않는 파일이 하나라도 있으면 false
		}
	}
	return true // 모든 파일이 제외 패턴에 해당
}

// CompareRemoteRevisionWith는 원격 리포지토리와 베이스라인을 비교한다.
// Jenkins: SCM.compareRemoteRevisionWith()
//
// 비교 로직:
//   1) baseline이 nil이면 INCOMPARABLE (처음 폴링)
//   2) 원격 리비전과 baseline 리비전이 같으면 NONE
//   3) 변경된 파일이 모두 제외 패턴에 해당하면 INSIGNIFICANT
//   4) 그 외 SIGNIFICANT
func (g *GitSCM) CompareRemoteRevisionWith(baseline *RevisionState) *PollingResult {
	latest := g.Repo.GetLatestCommit(g.Branch)

	// 원격에 커밋이 없는 경우
	if latest == nil {
		return &PollingResult{
			Baseline: baseline,
			Remote:   nil,
			Change:   ChangeNone,
		}
	}

	remoteState := &RevisionState{
		RevisionID: latest.Hash,
		Timestamp:  latest.Timestamp,
		Branch:     g.Branch,
	}

	// baseline이 nil이면 비교 불가능 — 즉시 빌드
	// Jenkins: SCMRevisionState.NONE과의 비교 시
	if baseline == nil {
		return &PollingResult{
			Baseline: baseline,
			Remote:   remoteState,
			Change:   ChangeIncomparable,
		}
	}

	// 같은 리비전이면 변경 없음
	if baseline.RevisionID == latest.Hash {
		return &PollingResult{
			Baseline: baseline,
			Remote:   remoteState,
			Change:   ChangeNone,
		}
	}

	// 변경이 있음 — 제외 패턴 확인
	newCommits := g.Repo.GetCommitsSince(baseline.RevisionID, g.Branch)
	allExcluded := true
	for _, c := range newCommits {
		if !g.isExcludedChange(c.AffectedFiles) {
			allExcluded = false
			break
		}
	}

	change := ChangeSignificant
	if allExcluded {
		change = ChangeInsignificant
	}

	return &PollingResult{
		Baseline: baseline,
		Remote:   remoteState,
		Change:   change,
	}
}

// Checkout은 소스 코드 체크아웃을 시뮬레이션한다.
func (g *GitSCM) Checkout(buildNumber int, workspace string) (*ChangeLogSet, error) {
	latest := g.Repo.GetLatestCommit(g.Branch)
	if latest == nil {
		g.buildRevisions[buildNumber] = ""
		return &ChangeLogSet{
			BuildNumber: buildNumber,
			Kind:        "git",
			Entries:     nil,
		}, nil
	}

	// 이전 빌드의 리비전 이후 커밋들을 변경 로그로 생성
	prevHash := ""
	for bn := buildNumber - 1; bn >= 1; bn-- {
		if h, ok := g.buildRevisions[bn]; ok {
			prevHash = h
			break
		}
	}

	commits := g.Repo.GetCommitsSince(prevHash, g.Branch)
	entries := make([]*ChangeLogEntry, len(commits))
	for i, c := range commits {
		entries[i] = &ChangeLogEntry{
			CommitID:      c.Hash,
			Author:        c.Author,
			Message:       c.Message,
			Timestamp:     c.Timestamp,
			AffectedFiles: c.AffectedFiles,
		}
	}

	g.buildRevisions[buildNumber] = latest.Hash

	return &ChangeLogSet{
		BuildNumber: buildNumber,
		Kind:        "git",
		Entries:     entries,
	}, nil
}

// Poll은 전체 폴링 프로세스를 수행한다.
// Jenkins: SCM.poll() 메서드의 동작을 재현.
//   1) 마지막 빌드에서 baseline을 계산 (calcRevisionsFromBuild)
//   2) 원격과 비교 (compareRemoteRevisionWith)
func (g *GitSCM) Poll(lastBuildNumber int) *PollingResult {
	var baseline *RevisionState
	if lastBuildNumber > 0 {
		baseline = g.CalcRevisionsFromBuild(lastBuildNumber)
	}

	// baseline이 nil인데 NONE이 아닌 경우 (빌드는 있었지만 리비전 정보가 없음)
	// Jenkins에서는 이 경우 calcRevisionsFromBuild를 다시 호출하여 처리
	if baseline == nil && lastBuildNumber > 0 {
		baseline = NoneRevisionState // nil, INCOMPARABLE 발생
	}

	return g.CompareRemoteRevisionWith(baseline)
}

// =============================================================================
// 8. SubversionSCM: SVN SCM 시뮬레이션
// =============================================================================
// SVN은 Git과 다른 리비전 모델을 사용한다:
//   - 숫자 기반 리비전 번호 (r1, r2, r3, ...)
//   - 중앙 집중형이므로 워크스페이스가 폴링에 필요할 수 있음
//   - 변경 감지는 서버의 최신 리비전 번호와 로컬 리비전 비교

// SVNRepository는 SVN 리포지토리를 시뮬레이션한다.
type SVNRepository struct {
	URL       string
	Revision  int
	Changes   []*SVNChange
	mu        sync.Mutex
}

// SVNChange는 SVN 변경을 나타낸다.
type SVNChange struct {
	Revision      int
	Author        string
	Message       string
	Timestamp     time.Time
	AffectedFiles []AffectedFile
}

// NewSVNRepository는 새 SVN 리포지토리를 생성한다.
func NewSVNRepository(url string) *SVNRepository {
	return &SVNRepository{
		URL:      url,
		Revision: 0,
		Changes:  make([]*SVNChange, 0),
	}
}

// Commit은 SVN에 새 변경을 커밋한다.
func (r *SVNRepository) Commit(author, message string, files []AffectedFile) *SVNChange {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.Revision++
	change := &SVNChange{
		Revision:      r.Revision,
		Author:        author,
		Message:       message,
		Timestamp:     time.Now(),
		AffectedFiles: files,
	}
	r.Changes = append(r.Changes, change)
	return change
}

// GetChangesSince는 특정 리비전 이후의 변경 목록을 반환한다.
func (r *SVNRepository) GetChangesSince(sinceRevision int) []*SVNChange {
	r.mu.Lock()
	defer r.mu.Unlock()

	var result []*SVNChange
	for _, c := range r.Changes {
		if c.Revision > sinceRevision {
			result = append(result, c)
		}
	}
	return result
}

// SubversionSCM은 SVN SCM 구현이다.
type SubversionSCM struct {
	RepositoryURL  string
	Repo           *SVNRepository
	buildRevisions map[int]int // buildNumber -> svn revision
}

// NewSubversionSCM은 새 SubversionSCM을 생성한다.
func NewSubversionSCM(repoURL string, repo *SVNRepository) *SubversionSCM {
	return &SubversionSCM{
		RepositoryURL:  repoURL,
		Repo:           repo,
		buildRevisions: make(map[int]int),
	}
}

func (s *SubversionSCM) GetType() string { return "hudson.scm.SubversionSCM" }
func (s *SubversionSCM) GetKey() string {
	return fmt.Sprintf("%s:%s", s.GetType(), s.RepositoryURL)
}
func (s *SubversionSCM) SupportsPolling() bool             { return true }
func (s *SubversionSCM) RequiresWorkspaceForPolling() bool  { return true } // SVN은 전통적으로 필요

func (s *SubversionSCM) CalcRevisionsFromBuild(buildNumber int) *RevisionState {
	rev, ok := s.buildRevisions[buildNumber]
	if !ok {
		return nil
	}
	return &RevisionState{
		RevisionID: fmt.Sprintf("r%d", rev),
		Timestamp:  time.Now(),
		Branch:     "trunk",
	}
}

func (s *SubversionSCM) CompareRemoteRevisionWith(baseline *RevisionState) *PollingResult {
	s.Repo.mu.Lock()
	currentRev := s.Repo.Revision
	s.Repo.mu.Unlock()

	remoteState := &RevisionState{
		RevisionID: fmt.Sprintf("r%d", currentRev),
		Timestamp:  time.Now(),
		Branch:     "trunk",
	}

	if baseline == nil {
		return &PollingResult{
			Baseline: baseline,
			Remote:   remoteState,
			Change:   ChangeIncomparable,
		}
	}

	if baseline.RevisionID == remoteState.RevisionID {
		return &PollingResult{
			Baseline: baseline,
			Remote:   remoteState,
			Change:   ChangeNone,
		}
	}

	return &PollingResult{
		Baseline: baseline,
		Remote:   remoteState,
		Change:   ChangeSignificant,
	}
}

func (s *SubversionSCM) Checkout(buildNumber int, workspace string) (*ChangeLogSet, error) {
	s.Repo.mu.Lock()
	currentRev := s.Repo.Revision
	s.Repo.mu.Unlock()

	prevRev := 0
	for bn := buildNumber - 1; bn >= 1; bn-- {
		if r, ok := s.buildRevisions[bn]; ok {
			prevRev = r
			break
		}
	}

	changes := s.Repo.GetChangesSince(prevRev)
	entries := make([]*ChangeLogEntry, len(changes))
	for i, c := range changes {
		entries[i] = &ChangeLogEntry{
			CommitID:      fmt.Sprintf("r%d", c.Revision),
			Author:        c.Author,
			Message:       c.Message,
			Timestamp:     c.Timestamp,
			AffectedFiles: c.AffectedFiles,
		}
	}

	s.buildRevisions[buildNumber] = currentRev

	return &ChangeLogSet{
		BuildNumber: buildNumber,
		Kind:        "svn",
		Entries:     entries,
	}, nil
}

func (s *SubversionSCM) Poll(lastBuildNumber int) *PollingResult {
	var baseline *RevisionState
	if lastBuildNumber > 0 {
		baseline = s.CalcRevisionsFromBuild(lastBuildNumber)
	}
	return s.CompareRemoteRevisionWith(baseline)
}

// =============================================================================
// 9. SCMTrigger: cron 기반 폴링 트리거
// =============================================================================
// Jenkins: hudson.triggers.SCMTrigger
// - cron 스케줄로 폴링 주기 설정 (예: "H/5 * * * *")
// - DescriptorImpl에서 SequentialExecutionQueue를 사용하여
//   동일 프로젝트에 대한 중복 폴링 방지
// - Runner는 실제 폴링을 수행하고, 변경 감지 시 빌드를 스케줄링
// - 동기/비동기 폴링 모드 지원
// - ignorePostCommitHooks: post-commit 훅 무시 옵션
// - pollingLog: 폴링 로그 기록

// CronSchedule은 cron 스케줄을 나타낸다.
type CronSchedule struct {
	Spec     string        // cron 스펙 (예: "H/5 * * * *")
	Interval time.Duration // 파싱된 폴링 간격
}

// ParseCronSchedule은 cron 스펙을 파싱한다.
// 실제 Jenkins는 복잡한 cron 파서를 사용하지만, 여기서는 간단히 처리한다.
func ParseCronSchedule(spec string) *CronSchedule {
	interval := 5 * time.Minute // 기본값

	if strings.Contains(spec, "/2") {
		interval = 2 * time.Minute
	} else if strings.Contains(spec, "/5") {
		interval = 5 * time.Minute
	} else if strings.Contains(spec, "/10") {
		interval = 10 * time.Minute
	} else if strings.Contains(spec, "/15") {
		interval = 15 * time.Minute
	} else if strings.Contains(spec, "/30") {
		interval = 30 * time.Minute
	}

	return &CronSchedule{
		Spec:     spec,
		Interval: interval,
	}
}

// SCMTrigger는 SCM 폴링 트리거이다.
// Jenkins: hudson.triggers.SCMTrigger
type SCMTrigger struct {
	Schedule             *CronSchedule // 폴링 스케줄
	IgnorePostCommitHooks bool         // post-commit 훅 무시 여부
	Job                  *Job          // 이 트리거가 속한 Job

	pollingLog []string   // 폴링 로그
	mu         sync.Mutex // 로그 보호
	stopCh     chan struct{}
	running    bool
}

// NewSCMTrigger는 새 SCMTrigger를 생성한다.
func NewSCMTrigger(spec string, ignorePostCommitHooks bool) *SCMTrigger {
	return &SCMTrigger{
		Schedule:              ParseCronSchedule(spec),
		IgnorePostCommitHooks: ignorePostCommitHooks,
		pollingLog:            make([]string, 0),
		stopCh:                make(chan struct{}),
	}
}

// SetJob은 트리거에 Job을 설정한다.
func (t *SCMTrigger) SetJob(job *Job) {
	t.Job = job
}

// AddLog는 폴링 로그를 추가한다.
func (t *SCMTrigger) AddLog(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05.000"), msg)
	t.pollingLog = append(t.pollingLog, entry)
}

// GetLog는 폴링 로그를 반환한다.
func (t *SCMTrigger) GetLog() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]string, len(t.pollingLog))
	copy(result, t.pollingLog)
	return result
}

// RunOnce는 한 번의 폴링을 수행한다.
// Jenkins: SCMTrigger.Runner.runPolling()
func (t *SCMTrigger) RunOnce() bool {
	if t.Job == nil {
		return false
	}

	t.AddLog(fmt.Sprintf("Started on %s", time.Now().Format("2006-01-02 15:04:05")))

	start := time.Now()
	result := t.Job.PollSCM()
	elapsed := time.Since(start)

	t.AddLog(fmt.Sprintf("Done. Took %v", elapsed))

	if result.HasChanges() {
		t.AddLog(fmt.Sprintf("Changes found (change=%s)", result.Change))
		t.Job.ScheduleBuild(fmt.Sprintf("SCM 변경 감지 (%s)", result.Change))
		return true
	}

	t.AddLog(fmt.Sprintf("No changes (change=%s)", result.Change))
	return false
}

// Start는 주기적 폴링을 시작한다.
// Jenkins: SCMTrigger는 Trigger<Item>을 상속하여 Trigger.Cron에 의해 주기적으로 실행됨
func (t *SCMTrigger) Start() {
	t.running = true
	go func() {
		for {
			select {
			case <-t.stopCh:
				return
			case <-time.After(t.Schedule.Interval):
				t.RunOnce()
			}
		}
	}()
}

// Stop은 주기적 폴링을 중지한다.
func (t *SCMTrigger) Stop() {
	if t.running {
		close(t.stopCh)
		t.running = false
	}
}

// =============================================================================
// 10. Job: 빌드 작업 (SCM 통합)
// =============================================================================

// BuildRecord는 빌드 기록이다.
type BuildRecord struct {
	Number    int
	Timestamp time.Time
	Cause     string
	ChangeLog *ChangeLogSet
	Result    string
}

// Job은 SCM과 트리거를 가진 빌드 작업이다.
type Job struct {
	Name          string
	SCM           SCM
	Trigger       *SCMTrigger
	Builds        []*BuildRecord
	NextBuildNum  int
	QuietPeriod   time.Duration // 빌드 대기 시간
	mu            sync.Mutex
}

// NewJob은 새 Job을 생성한다.
func NewJob(name string, scm SCM) *Job {
	job := &Job{
		Name:         name,
		SCM:          scm,
		Builds:       make([]*BuildRecord, 0),
		NextBuildNum: 1,
		QuietPeriod:  5 * time.Second,
	}
	return job
}

// SetTrigger는 SCM 트리거를 설정한다.
func (j *Job) SetTrigger(trigger *SCMTrigger) {
	j.Trigger = trigger
	trigger.SetJob(j)
}

// PollSCM은 SCM 변경을 확인한다.
func (j *Job) PollSCM() *PollingResult {
	if j.SCM == nil {
		return NoChanges
	}

	if !j.SCM.SupportsPolling() {
		return NoChanges
	}

	lastBuildNum := 0
	j.mu.Lock()
	if len(j.Builds) > 0 {
		lastBuildNum = j.Builds[len(j.Builds)-1].Number
	}
	j.mu.Unlock()

	return j.SCM.Poll(lastBuildNum)
}

// ScheduleBuild는 빌드를 스케줄링한다.
func (j *Job) ScheduleBuild(cause string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	buildNum := j.NextBuildNum
	j.NextBuildNum++

	// 체크아웃 및 빌드 수행
	changeLog, err := j.SCM.Checkout(buildNum, fmt.Sprintf("/workspace/%s", j.Name))
	if err != nil {
		fmt.Printf("  [ERROR] 빌드 #%d 체크아웃 실패: %v\n", buildNum, err)
		return
	}

	record := &BuildRecord{
		Number:    buildNum,
		Timestamp: time.Now(),
		Cause:     cause,
		ChangeLog: changeLog,
		Result:    "SUCCESS",
	}
	j.Builds = append(j.Builds, record)

	fmt.Printf("  [빌드] %s #%d 완료 (원인: %s)\n", j.Name, buildNum, cause)
	if changeLog != nil && !changeLog.IsEmptySet() {
		for _, entry := range changeLog.Entries {
			fmt.Printf("    변경: %s\n", entry)
		}
	}
}

// =============================================================================
// 11. SCMDecisionHandler: 폴링 거부권 (veto)
// =============================================================================
// Jenkins: jenkins.scm.SCMDecisionHandler
// - shouldPoll() 메서드로 폴링 실행 여부를 결정
// - 여러 핸들러 중 하나라도 false를 반환하면 폴링 중단
// - 예: 특정 시간대에는 폴링 금지, 특정 조건에서 폴링 비활성화

// SCMDecisionHandler는 폴링 결정 핸들러이다.
type SCMDecisionHandler interface {
	ShouldPoll(jobName string) bool
	String() string
}

// MaintenanceWindowHandler는 유지보수 시간대에 폴링을 금지하는 핸들러이다.
type MaintenanceWindowHandler struct {
	StartHour int
	EndHour   int
}

func (h *MaintenanceWindowHandler) ShouldPoll(jobName string) bool {
	hour := time.Now().Hour()
	if h.StartHour <= h.EndHour {
		return hour < h.StartHour || hour >= h.EndHour
	}
	// 자정 경계 (예: 22시~6시)
	return hour >= h.EndHour && hour < h.StartHour
}

func (h *MaintenanceWindowHandler) String() string {
	return fmt.Sprintf("MaintenanceWindow(%d:00-%d:00)", h.StartHour, h.EndHour)
}

// =============================================================================
// 12. SequentialExecutionQueue: 동일 Job 중복 폴링 방지
// =============================================================================
// Jenkins: hudson.util.SequentialExecutionQueue
// - 동일 Job에 대한 폴링이 이미 실행 중이면 새 폴링 요청을 무시
// - Runner의 equals/hashCode가 job을 기준으로 하여 중복 판별

// PollingQueue는 폴링 실행 큐이다.
type PollingQueue struct {
	inProgress map[string]bool // jobName -> 실행 중 여부
	mu         sync.Mutex
}

// NewPollingQueue는 새 폴링 큐를 생성한다.
func NewPollingQueue() *PollingQueue {
	return &PollingQueue{
		inProgress: make(map[string]bool),
	}
}

// TryExecute는 폴링 실행을 시도한다.
// 이미 실행 중이면 false를 반환한다.
func (q *PollingQueue) TryExecute(jobName string, task func()) bool {
	q.mu.Lock()
	if q.inProgress[jobName] {
		q.mu.Unlock()
		fmt.Printf("  [큐] %s 폴링이 이미 진행 중 - 건너뜀\n", jobName)
		return false
	}
	q.inProgress[jobName] = true
	q.mu.Unlock()

	defer func() {
		q.mu.Lock()
		delete(q.inProgress, jobName)
		q.mu.Unlock()
	}()

	task()
	return true
}

// IsStarving은 큐가 정체되었는지 확인한다.
// Jenkins: STARVATION_THRESHOLD = 1시간
func (q *PollingQueue) IsStarving(threshold int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.inProgress) > threshold
}

// =============================================================================
// 13. SCMPollListener: 폴링 이벤트 리스너
// =============================================================================
// Jenkins: hudson.model.listeners.SCMPollListener
// - onBeforePolling(): 폴링 시작 전
// - onPollingSuccess(): 폴링 성공 후
// - onPollingFailed(): 폴링 실패 후

// SCMPollListener는 폴링 이벤트를 수신하는 리스너이다.
type SCMPollListener struct {
	Name string
}

// OnBeforePolling은 폴링 시작 전에 호출된다.
func (l *SCMPollListener) OnBeforePolling(jobName string) {
	fmt.Printf("    [리스너:%s] 폴링 시작: %s\n", l.Name, jobName)
}

// OnPollingSuccess는 폴링 성공 후 호출된다.
func (l *SCMPollListener) OnPollingSuccess(jobName string, result *PollingResult) {
	fmt.Printf("    [리스너:%s] 폴링 완료: %s (변경=%s)\n", l.Name, jobName, result.Change)
}

// =============================================================================
// 데모 실행
// =============================================================================

func printSeparator(title string) {
	fmt.Println()
	fmt.Printf("===== %s =====\n", title)
	fmt.Println()
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jenkins SCM 폴링 시뮬레이션                                ║")
	fmt.Println("║  (SCM, PollingResult, ChangeLogSet, SCMTrigger)             ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	// =========================================================================
	// 데모 1: NullSCM — SCM 미설정 기본값
	// =========================================================================
	printSeparator("데모 1: NullSCM (SCM 미설정)")

	nullSCM := &NullSCM{}
	fmt.Printf("  SCM 타입: %s\n", nullSCM.GetType())
	fmt.Printf("  폴링 지원: %v\n", nullSCM.SupportsPolling())

	result := nullSCM.Poll(0)
	fmt.Printf("  폴링 결과: %s\n", result)
	fmt.Printf("  변경 있음: %v\n", result.HasChanges())

	cls, _ := nullSCM.Checkout(1, "/workspace/null-job")
	fmt.Printf("  체크아웃 결과: kind=%s, empty=%v\n", cls.Kind, cls.IsEmptySet())

	// =========================================================================
	// 데모 2: GitSCM — Git 기반 폴링
	// =========================================================================
	printSeparator("데모 2: GitSCM 폴링")

	gitRepo := NewGitRepository("https://github.com/example/project.git")
	gitSCM := NewGitSCM("https://github.com/example/project.git", "main", gitRepo)

	// 2-1) 초기 폴링 (커밋 없는 상태)
	fmt.Println("--- 2-1) 초기 폴링 (커밋 없음) ---")
	result = gitSCM.Poll(0)
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())

	// 2-2) 첫 번째 커밋 추가 후 폴링
	fmt.Println("\n--- 2-2) 첫 번째 커밋 후 폴링 ---")
	gitRepo.AddCommit("alice", "Initial commit", "main", []AffectedFile{
		{Path: "src/main/java/App.java", EditType: EditAdd},
		{Path: "pom.xml", EditType: EditAdd},
		{Path: "README.md", EditType: EditAdd},
	})

	result = gitSCM.Poll(0) // 아직 빌드가 없으므로 lastBuild=0
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())
	fmt.Printf("  -> INCOMPARABLE: 첫 폴링이므로 즉시 빌드\n")

	// 2-3) 빌드 수행 후 변경 없이 폴링
	fmt.Println("\n--- 2-3) 빌드 후 변경 없이 폴링 ---")
	job := NewJob("my-project", gitSCM)
	job.ScheduleBuild("첫 번째 빌드")

	result = gitSCM.Poll(1) // 빌드 #1의 리비전과 비교
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())

	// 2-4) 새 커밋 추가 후 폴링
	fmt.Println("\n--- 2-4) 새 커밋 후 폴링 ---")
	gitRepo.AddCommit("bob", "Add feature X", "main", []AffectedFile{
		{Path: "src/main/java/FeatureX.java", EditType: EditAdd},
		{Path: "src/test/java/FeatureXTest.java", EditType: EditAdd},
	})

	result = gitSCM.Poll(1)
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())
	fmt.Printf("  baseline: %v\n", result.Baseline)
	fmt.Printf("  remote:   %v\n", result.Remote)

	// =========================================================================
	// 데모 3: Git 제외 패턴 (excludedRegions)
	// =========================================================================
	printSeparator("데모 3: Git 제외 패턴")

	gitRepo2 := NewGitRepository("https://github.com/example/docs.git")
	gitSCM2 := NewGitSCM("https://github.com/example/docs.git", "main", gitRepo2)
	gitSCM2.ExcludedPaths = []string{"README.md", "docs/", ".gitignore"}

	// 초기 커밋 + 빌드
	gitRepo2.AddCommit("alice", "Init", "main", []AffectedFile{
		{Path: "src/App.java", EditType: EditAdd},
	})
	job2 := NewJob("docs-project", gitSCM2)
	job2.ScheduleBuild("초기 빌드")

	// 3-1) 제외 패턴에만 해당하는 변경
	fmt.Println("--- 3-1) 제외 패턴 파일만 변경 ---")
	gitRepo2.AddCommit("bob", "Update README", "main", []AffectedFile{
		{Path: "README.md", EditType: EditModify},
		{Path: "docs/guide.md", EditType: EditModify},
	})
	result = gitSCM2.Poll(1)
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())
	fmt.Printf("  -> INSIGNIFICANT: README.md, docs/ 변경은 무시됨\n")

	// 3-2) 실제 코드 변경 포함
	fmt.Println("\n--- 3-2) 실제 코드 변경 포함 ---")
	gitRepo2.AddCommit("bob", "Fix critical bug", "main", []AffectedFile{
		{Path: "src/App.java", EditType: EditModify},
		{Path: "README.md", EditType: EditModify},
	})
	result = gitSCM2.Poll(1)
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())
	fmt.Printf("  -> SIGNIFICANT: src/App.java가 제외 패턴에 해당하지 않음\n")

	// =========================================================================
	// 데모 4: SubversionSCM 폴링
	// =========================================================================
	printSeparator("데모 4: SubversionSCM 폴링")

	svnRepo := NewSVNRepository("svn://svn.example.com/project/trunk")
	svnSCM := NewSubversionSCM("svn://svn.example.com/project/trunk", svnRepo)

	// 초기 커밋
	svnRepo.Commit("alice", "Initial import", []AffectedFile{
		{Path: "trunk/src/Main.java", EditType: EditAdd},
	})

	fmt.Println("--- 4-1) SVN 초기 폴링 ---")
	result = svnSCM.Poll(0)
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())

	// 빌드 수행
	job3 := NewJob("svn-project", svnSCM)
	job3.ScheduleBuild("초기 빌드")

	// 변경 없이 폴링
	fmt.Println("\n--- 4-2) SVN 변경 없이 폴링 ---")
	result = svnSCM.Poll(1)
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())

	// SVN 커밋 추가 후 폴링
	fmt.Println("\n--- 4-3) SVN 새 커밋 후 폴링 ---")
	svnRepo.Commit("bob", "Fix SVN issue", []AffectedFile{
		{Path: "trunk/src/Main.java", EditType: EditModify},
	})
	result = svnSCM.Poll(1)
	fmt.Printf("  결과: %s (hasChanges=%v)\n", result.Change, result.HasChanges())
	fmt.Printf("  baseline: %v\n", result.Baseline)
	fmt.Printf("  remote:   %v\n", result.Remote)

	// =========================================================================
	// 데모 5: SCMTrigger — cron 기반 폴링 트리거
	// =========================================================================
	printSeparator("데모 5: SCMTrigger 동작")

	gitRepo3 := NewGitRepository("https://github.com/example/auto.git")
	gitSCM3 := NewGitSCM("https://github.com/example/auto.git", "main", gitRepo3)

	// 초기 빌드
	gitRepo3.AddCommit("alice", "Init auto project", "main", []AffectedFile{
		{Path: "app.go", EditType: EditAdd},
	})
	autoJob := NewJob("auto-build", gitSCM3)
	autoJob.ScheduleBuild("수동 빌드")

	trigger := NewSCMTrigger("H/5 * * * *", false)
	autoJob.SetTrigger(trigger)

	fmt.Printf("  Job: %s\n", autoJob.Name)
	fmt.Printf("  SCM: %s\n", gitSCM3.GetType())
	fmt.Printf("  폴링 스케줄: %s (간격: %v)\n", trigger.Schedule.Spec, trigger.Schedule.Interval)
	fmt.Printf("  PostCommitHooks 무시: %v\n", trigger.IgnorePostCommitHooks)

	// 5-1) 변경 없이 폴링
	fmt.Println("\n--- 5-1) 변경 없이 폴링 ---")
	changed := trigger.RunOnce()
	fmt.Printf("  빌드 트리거됨: %v\n", changed)

	// 5-2) 커밋 추가 후 폴링
	fmt.Println("\n--- 5-2) 커밋 추가 후 폴링 ---")
	gitRepo3.AddCommit("charlie", "Add new API endpoint", "main", []AffectedFile{
		{Path: "api/handler.go", EditType: EditAdd},
		{Path: "api/handler_test.go", EditType: EditAdd},
	})
	changed = trigger.RunOnce()
	fmt.Printf("  빌드 트리거됨: %v\n", changed)

	// 5-3) 또 한 번 폴링 (이미 빌드됨)
	fmt.Println("\n--- 5-3) 이미 빌드 후 폴링 ---")
	changed = trigger.RunOnce()
	fmt.Printf("  빌드 트리거됨: %v\n", changed)

	// 폴링 로그 출력
	fmt.Println("\n--- 폴링 로그 ---")
	for _, log := range trigger.GetLog() {
		fmt.Printf("  %s\n", log)
	}

	// =========================================================================
	// 데모 6: PollingResult.Change 4단계 비교
	// =========================================================================
	printSeparator("데모 6: PollingResult.Change 4단계")

	changes := []struct {
		change      Change
		description string
	}{
		{ChangeNone, "변경 없음 — 두 상태가 동일"},
		{ChangeInsignificant, "사소한 변경 — 빌드 불필요 (예: 문서만 변경)"},
		{ChangeSignificant, "유의미한 변경 — 빌드 스케줄링"},
		{ChangeIncomparable, "비교 불가 — 즉시 빌드 (quiet period 무시)"},
	}

	for _, c := range changes {
		pr := &PollingResult{Change: c.change}
		fmt.Printf("  %-15s | hasChanges=%-5v | %s\n",
			c.change, pr.HasChanges(), c.description)
	}

	fmt.Println()
	fmt.Println("  HasChanges() 판단 기준:")
	fmt.Println("    change.ordinal() > Change.INSIGNIFICANT.ordinal()")
	fmt.Println("    즉, SIGNIFICANT 또는 INCOMPARABLE일 때만 true")

	// =========================================================================
	// 데모 7: ChangeLogSet 구조
	// =========================================================================
	printSeparator("데모 7: ChangeLogSet 구조")

	changeLogDemo := &ChangeLogSet{
		BuildNumber: 42,
		Kind:        "git",
		Entries: []*ChangeLogEntry{
			{
				CommitID:  "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
				Author:    "alice",
				Message:   "Refactor authentication module",
				Timestamp: time.Now().Add(-30 * time.Minute),
				AffectedFiles: []AffectedFile{
					{Path: "src/auth/AuthService.java", EditType: EditModify},
					{Path: "src/auth/TokenManager.java", EditType: EditModify},
					{Path: "src/auth/OAuth2Provider.java", EditType: EditAdd},
				},
			},
			{
				CommitID:  "b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1",
				Author:    "bob",
				Message:   "Fix null pointer in user service",
				Timestamp: time.Now().Add(-15 * time.Minute),
				AffectedFiles: []AffectedFile{
					{Path: "src/user/UserService.java", EditType: EditModify},
					{Path: "test/user/UserServiceTest.java", EditType: EditModify},
				},
			},
			{
				CommitID:  "c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2",
				Author:    "charlie",
				Message:   "Delete deprecated API endpoints",
				Timestamp: time.Now().Add(-5 * time.Minute),
				AffectedFiles: []AffectedFile{
					{Path: "src/api/v1/OldController.java", EditType: EditDelete},
					{Path: "src/api/v1/LegacyHandler.java", EditType: EditDelete},
				},
			},
		},
	}

	fmt.Printf("  빌드 #%d의 변경 로그 (kind=%s):\n", changeLogDemo.BuildNumber, changeLogDemo.Kind)
	fmt.Printf("  비어있음: %v\n", changeLogDemo.IsEmptySet())
	fmt.Printf("  커밋 수:  %d\n\n", len(changeLogDemo.Items()))

	for i, entry := range changeLogDemo.Items() {
		fmt.Printf("  [커밋 %d] %s\n", i+1, entry)
		fmt.Printf("    작성자:   %s\n", entry.Author)
		fmt.Printf("    메시지:   %s\n", entry.Message)
		fmt.Printf("    커밋ID:   %s\n", entry.CommitID[:12])
		fmt.Printf("    시간:     %s\n", entry.Timestamp.Format("15:04:05"))
		fmt.Printf("    변경파일:\n")
		for _, f := range entry.AffectedFiles {
			fmt.Printf("      %-6s %s\n", f.EditType, f.Path)
		}
		fmt.Println()
	}

	// =========================================================================
	// 데모 8: SCMDecisionHandler — 폴링 거부권 (veto)
	// =========================================================================
	printSeparator("데모 8: SCMDecisionHandler (폴링 Veto)")

	handlers := []SCMDecisionHandler{
		&MaintenanceWindowHandler{StartHour: 2, EndHour: 4},  // 새벽 2~4시 유지보수
	}

	for _, h := range handlers {
		shouldPoll := h.ShouldPoll("my-project")
		fmt.Printf("  핸들러: %s\n", h)
		fmt.Printf("  현재 폴링 허용: %v (현재 시각: %s)\n", shouldPoll,
			time.Now().Format("15:04"))
	}

	fmt.Println()
	fmt.Println("  Jenkins SCMDecisionHandler 패턴:")
	fmt.Println("    1) 여러 핸들러를 체인으로 등록")
	fmt.Println("    2) SCMTrigger.Runner.run()에서 firstShouldPollVeto() 호출")
	fmt.Println("    3) 하나라도 shouldPoll()=false이면 폴링 건너뜀")
	fmt.Println("    4) 폴링 로그에 '거부 사유' 기록")

	// =========================================================================
	// 데모 9: SequentialExecutionQueue — 중복 폴링 방지
	// =========================================================================
	printSeparator("데모 9: SequentialExecutionQueue (중복 폴링 방지)")

	queue := NewPollingQueue()
	var wg sync.WaitGroup

	fmt.Println("  동일 Job에 대한 동시 폴링 요청 3개 발생:")
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			executed := queue.TryExecute("concurrent-job", func() {
				fmt.Printf("    [스레드 %d] 폴링 실행 중...\n", id)
				time.Sleep(100 * time.Millisecond) // 폴링 시뮬레이션
				fmt.Printf("    [스레드 %d] 폴링 완료\n", id)
			})
			if !executed {
				fmt.Printf("    [스레드 %d] 이미 실행 중이므로 건너뜀\n", id)
			}
		}(i)
		time.Sleep(10 * time.Millisecond) // 약간의 지연
	}
	wg.Wait()

	fmt.Println()
	fmt.Println("  Jenkins SequentialExecutionQueue 특성:")
	fmt.Println("    - Runner.equals()가 job 기준으로 동등성 판단")
	fmt.Println("    - 동일 Job의 중복 폴링 요청은 하나로 합침")
	fmt.Println("    - 다른 Job의 폴링은 동시 실행 가능")

	// =========================================================================
	// 데모 10: 다중 SCM 타입 비교
	// =========================================================================
	printSeparator("데모 10: 다중 SCM 타입 비교")

	scmTypes := []SCM{
		&NullSCM{},
		NewGitSCM("https://github.com/example/app.git", "main",
			NewGitRepository("https://github.com/example/app.git")),
		NewSubversionSCM("svn://svn.example.com/app/trunk",
			NewSVNRepository("svn://svn.example.com/app/trunk")),
	}

	fmt.Printf("  %-30s | %-8s | %-10s | %-15s\n",
		"SCM 타입", "폴링", "워크스페이스", "키")
	fmt.Printf("  %s\n", strings.Repeat("-", 75))

	for _, scm := range scmTypes {
		typeName := scm.GetType()
		if len(typeName) > 30 {
			typeName = typeName[:27] + "..."
		}
		fmt.Printf("  %-30s | %-8v | %-10v | %-15s\n",
			typeName,
			scm.SupportsPolling(),
			scm.RequiresWorkspaceForPolling(),
			truncate(scm.GetKey(), 15),
		)
	}

	// =========================================================================
	// 데모 11: 전체 폴링 시나리오 (통합 데모)
	// =========================================================================
	printSeparator("데모 11: 전체 폴링 시나리오 (통합)")

	fmt.Println("  시나리오: 개발팀이 Git 리포에 연속 푸시하고 Jenkins가 감지")
	fmt.Println()

	intRepo := NewGitRepository("https://github.com/team/service.git")
	intSCM := NewGitSCM("https://github.com/team/service.git", "develop", intRepo)
	intJob := NewJob("team-service", intSCM)
	intTrigger := NewSCMTrigger("H/2 * * * *", false)
	intJob.SetTrigger(intTrigger)
	listener := &SCMPollListener{Name: "audit"}

	// 시뮬레이션: 개발자들이 커밋을 푸시
	developers := []struct {
		name    string
		message string
		files   []AffectedFile
		delay   time.Duration
	}{
		{"alice", "Add user registration endpoint", []AffectedFile{
			{Path: "api/user.go", EditType: EditAdd},
			{Path: "api/user_test.go", EditType: EditAdd},
		}, 0},
		{"bob", "Fix database connection pool", []AffectedFile{
			{Path: "db/pool.go", EditType: EditModify},
		}, 50 * time.Millisecond},
		{"charlie", "Update API documentation", []AffectedFile{
			{Path: "docs/api.md", EditType: EditModify},
			{Path: "README.md", EditType: EditModify},
		}, 50 * time.Millisecond},
	}

	// 초기 빌드
	intRepo.AddCommit("admin", "Initial setup", "develop", []AffectedFile{
		{Path: "main.go", EditType: EditAdd},
	})
	intJob.ScheduleBuild("초기 설정")
	fmt.Println()

	// 개발자 커밋 + 폴링 사이클
	for _, dev := range developers {
		time.Sleep(dev.delay)
		intRepo.AddCommit(dev.name, dev.message, "develop", dev.files)
		fmt.Printf("  [Git] %s pushed: %s\n", dev.name, dev.message)
	}

	fmt.Println()
	fmt.Println("  --- 폴링 실행 ---")
	listener.OnBeforePolling(intJob.Name)
	changed = intTrigger.RunOnce()
	pollResult := intJob.PollSCM()
	listener.OnPollingSuccess(intJob.Name, pollResult)

	fmt.Println()
	fmt.Println("  --- 빌드 결과 ---")
	intJob.mu.Lock()
	for _, build := range intJob.Builds {
		fmt.Printf("  빌드 #%d: %s (원인: %s)\n", build.Number, build.Result, build.Cause)
		if build.ChangeLog != nil {
			fmt.Printf("    변경 로그 (%d 커밋):\n", len(build.ChangeLog.Entries))
			for _, e := range build.ChangeLog.Entries {
				fmt.Printf("      - %s: %s (%d files)\n", e.Author, e.Message, len(e.AffectedFiles))
			}
		}
	}
	intJob.mu.Unlock()

	// =========================================================================
	// 데모 12: 폴링 흐름 아키텍처 요약
	// =========================================================================
	printSeparator("데모 12: Jenkins SCM 폴링 아키텍처 요약")

	fmt.Println(`
  Jenkins SCM 폴링 아키텍처
  ─────────────────────────

  ┌────────────────────────────────────────────────────────────────┐
  │                      Trigger.Cron                              │
  │  (Jenkins 내부 cron 엔진이 SCMTrigger.run()을 주기적으로 호출)  │
  └──────────┬─────────────────────────────────────────────────────┘
             │
             ▼
  ┌──────────────────────┐     ┌──────────────────────────────┐
  │   SCMDecisionHandler │────▶│  거부(veto)하면 폴링 건너뜀    │
  │   shouldPoll() 확인  │     │  (유지보수 윈도우 등)          │
  └──────────┬───────────┘     └──────────────────────────────┘
             │ 허용
             ▼
  ┌──────────────────────────────────────────────────┐
  │   SequentialExecutionQueue                        │
  │   - 동일 Job의 중복 폴링 방지                      │
  │   - Runner.equals()가 job 기준으로 동등성 판단     │
  └──────────┬───────────────────────────────────────┘
             │
             ▼
  ┌──────────────────────────────────────────────────┐
  │   SCMTrigger.Runner.runPolling()                  │
  │                                                   │
  │   1) calcRevisionsFromBuild(lastBuild)            │
  │      → 마지막 빌드의 리비전 상태(baseline) 계산    │
  │                                                   │
  │   2) compareRemoteRevisionWith(baseline)           │
  │      → 원격 리포지토리와 baseline 비교             │
  │      → PollingResult 반환                          │
  └──────────┬───────────────────────────────────────┘
             │
             ▼
  ┌──────────────────────────────────────────────────┐
  │   PollingResult.hasChanges() 확인                  │
  │                                                   │
  │   NONE          → 변경 없음, 빌드 안 함            │
  │   INSIGNIFICANT → 사소한 변경, 빌드 안 함          │
  │   SIGNIFICANT   → 빌드 스케줄링 (quiet period 적용)│
  │   INCOMPARABLE  → 즉시 빌드 (quiet period 무시)    │
  └──────────┬───────────────────────────────────────┘
             │ SIGNIFICANT or INCOMPARABLE
             ▼
  ┌──────────────────────────────────────────────────┐
  │   scheduleBuild2(quietPeriod, CauseAction)        │
  │   → 빌드 큐에 추가                                │
  │   → SCMTriggerCause로 폴링 로그 첨부              │
  └──────────────────────────────────────────────────┘

  주요 소스 파일:
    hudson/scm/SCM.java              → SCM 추상 클래스 (핵심)
    hudson/scm/PollingResult.java    → 폴링 결과 (Change enum)
    hudson/scm/SCMRevisionState.java → 리비전 상태 (NONE 포함)
    hudson/scm/ChangeLogSet.java     → 변경 로그 (Entry, AffectedFile)
    hudson/scm/NullSCM.java          → 기본 SCM (항상 NO_CHANGES)
    hudson/triggers/SCMTrigger.java  → cron 폴링 트리거 (Runner, SCMAction)
    jenkins/scm/SCMDecisionHandler.java → 폴링 veto 핸들러
`)

	// 마지막 요약 통계
	fmt.Println("  실행 결과 요약:")
	fmt.Printf("    NullSCM 폴링:      항상 NONE\n")
	fmt.Printf("    GitSCM 폴링:       %d번 실행\n", 6)
	fmt.Printf("    SubversionSCM 폴링: %d번 실행\n", 3)
	fmt.Printf("    트리거 폴링:        %d번 실행\n", 3)
	fmt.Printf("    총 빌드 수:         %d건\n",
		len(job.Builds)+len(job2.Builds)+len(job3.Builds)+len(autoJob.Builds)+len(intJob.Builds))
}

// truncate는 문자열을 최대 길이로 자른다.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// randomHash는 랜덤 해시 문자열을 생성한다.
func randomHash() string {
	b := make([]byte, 20)
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	return fmt.Sprintf("%x", b)
}
