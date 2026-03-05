# PoC 10: Git 클라이언트

## 개요

Argo CD의 Git 클라이언트 레이어를 Go 표준 라이브러리만으로 시뮬레이션한다.
Argo CD는 저장소에서 매니페스트를 가져오기 위해 git CLI를 exec.Command로 호출하는 `nativeGitClient`를 사용한다.

## 참조 소스 코드

| 파일 | 역할 |
|------|------|
| `util/git/client.go` | Client 인터페이스, nativeGitClient 구조체 |
| `util/git/creds.go` | Creds 인터페이스, HTTPSCreds, SSHCreds, GitHubAppCreds, CredsStore |
| `util/io/paths.go` | TempPaths 인터페이스, RandomizedTempPaths |
| `reposerver/repository/lock.go` | repositoryLock (per-repo 직렬화 락) |
| `reposerver/repository/repository.go` | manifest-generate-paths 최적화 |

## 핵심 개념

### Git Client 인터페이스

```go
// 실제 소스: util/git/client.go
type Client interface {
    Root() string
    Init() error
    Fetch(revision string, depth int64) error
    Checkout(revision string, submoduleEnabled bool) (string, error)
    LsRefs() (*Refs, error)
    LsRemote(revision string) (string, error)
    CommitSHA() (string, error)
    RevisionMetadata(revision string) (*RevisionMetadata, error)
    VerifyCommitSignature(string) (string, error)
    ChangedFiles(revision string, targetRevision string) ([]string, error)
    IsRevisionPresent(revision string) bool
}
```

### nativeGitClient 구조

```go
// 실제 소스: util/git/client.go
type nativeGitClient struct {
    repoURL        string
    root           string   // 로컬 체크아웃 경로
    creds          Creds
    insecure       bool
    enableLfs      bool
    gitRefCache    gitRefCache
    loadRefFromCache bool
    gitConfigEnv   []string  // GIT_CONFIG_COUNT 등 builtin 설정
}
```

### 자격증명(Creds) 타입

| 타입 | 구조체 | 환경변수 |
|------|--------|----------|
| 없음 | `NopCreds` | 없음 |
| HTTPS | `HTTPSCreds` | `GIT_USERNAME`, `GIT_PASSWORD` |
| SSH | `SSHCreds` | `GIT_SSH_COMMAND=-i <keyfile>` |
| GitHub App | `GitHubAppCreds` | `ARGOCD_GIT_AUTH_HEADER=Bearer <token>` |

### CredsStore

```go
// 실제 소스: util/git/creds.go
type CredsStore interface {
    Add(username string, password string) string  // → credID
    Remove(id string)
    Environ(id string) []string  // GIT_USERNAME/GIT_PASSWORD 등
}
```

자격증명을 ID로 캐싱하여 여러 저장소에서 재사용한다.
실제 코드에서 HTTPSCreds는 CredsStore에 자격증명을 저장하고 GIT_ASKPASS 환경변수를 통해 git에 주입한다.

### RandomizedTempPaths

```go
// 실제 소스: util/io/paths.go
type RandomizedTempPaths struct {
    root  string
    paths map[string]string  // key → UUID 기반 경로
    lock  sync.RWMutex
}

// GetPath: 기존 경로 반환 또는 UUID로 새 경로 생성 (멱등성)
func (p *RandomizedTempPaths) GetPath(key string) (string, error) {
    uniqueId, _ := uuid.NewRandom()
    repoPath := filepath.Join(p.root, uniqueId.String())
    p.paths[key] = repoPath
    return repoPath, nil
}
```

저장소 URL(key)을 UUID 기반 임시 경로로 매핑한다. 동일 URL은 항상 동일 경로를 반환한다.

### repositoryLock (per-repo 직렬화)

```go
// 실제 소스: reposerver/repository/lock.go
func (r *repositoryLock) Lock(path, revision string, allowConcurrent bool, init func() (io.Closer, error)) (io.Closer, error) {
    // 동일 path + 동일 revision + allowConcurrent=true → 병렬 허용
    // 동일 path + 다른 revision → 완료 대기
    // state.cond.Wait()로 대기, Broadcast()로 깨움
}
```

`reposerver/repository/repository.go`에서 manifest 생성 전에 항상 호출:
```go
closer, err := s.repoLock.Lock(gitClient.Root(), commitSHA, true, func() (io.Closer, error) {
    return s.checkoutRevision(gitClient, commitSHA, s.initConstants.SubmoduleEnabled)
})
```

### manifest-generate-paths 최적화

어노테이션 `argocd.argoproj.io/manifest-generate-paths: config/`를 설정하면
해당 경로 외의 파일만 변경된 경우 Git fetch와 매니페스트 생성을 건너뛴다.

```
참조: reposerver/repository/repository.go:1495
'argocd.argoproj.io/manifest-generate-paths' annotation for manifest generation
instead of transmit the whole repository.
```

### 전체 플로우

```
NewClient(repoURL, creds)
    ↓
Init() — git init + git remote add origin <url>
    ↓
LsRefs() — git ls-remote --heads --tags
    ↓
LsRemote(branch/tag) — SHA 해석
    ↓
Fetch(revision, depth) — git fetch origin <rev>
    ↓
Checkout(revision) — git checkout <SHA>
    ↓
CommitSHA() — git rev-parse HEAD
    ↓
RevisionMetadata() — git log -n 1 --pretty='format:...'
    ↓
ChangedFiles(from, to) — git diff --name-only <from>..<to>
```

## 실행 방법

```bash
go run main.go
```

## 실행 결과 요약

```
시나리오 1: Creds 타입별 환경변수
  none: 0개 / https: 2개 / ssh: 2개 / github-app: 2개

시나리오 2: RandomizedTempPaths — 동일 키 → 동일 경로 (멱등성)

시나리오 3: repositoryLock — 동일 리비전 동시 접근 허용

시나리오 4: 전체 Git 플로우
  Init → LsRefs → LsRemote → Fetch → Checkout → CommitSHA
  → RevisionMetadata → VerifyCommitSignature → ChangedFiles

시나리오 5: manifest-generate-paths 최적화
  app/ 감시 + app/memory.go 변경 → 생성 필요
  ui/  감시 + app/memory.go 변경 → 스킵 (캐시 사용)
```

## 핵심 설계 선택의 이유 (Why)

**왜 git CLI를 exec.Command로 호출하는가?**
git의 모든 기능을 순수 Go로 구현하는 것보다 검증된 git CLI를 활용하는 것이 안정성과 호환성 측면에서 유리하다.
go-git 라이브러리는 일부 고급 기능 지원이 불완전하여 nativeGitClient가 기본 구현으로 사용된다.

**왜 repositoryLock이 revision 기반으로 동시성을 제어하는가?**
동일 리비전에 대한 여러 요청(예: 다수 앱이 같은 커밋 사용)은 공유 체크아웃을 허용하여 효율성을 높인다.
다른 리비전은 파일시스템 충돌 방지를 위해 직렬화한다.

**왜 RandomizedTempPaths가 UUID를 사용하는가?**
저장소 URL에 포함된 특수문자(`/`, `:` 등)를 경로명으로 안전하게 변환하고,
동시에 저장소별 격리된 작업 디렉토리를 보장하기 위해 UUID 기반 경로를 생성한다.
