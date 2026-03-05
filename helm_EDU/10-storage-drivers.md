# 10. 스토리지 드라이버 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Driver 인터페이스](#2-driver-인터페이스)
3. [Storage 계층](#3-storage-계층)
4. [Secrets 드라이버](#4-secrets-드라이버)
5. [ConfigMaps 드라이버](#5-configmaps-드라이버)
6. [Memory 드라이버](#6-memory-드라이버)
7. [SQL 드라이버 (PostgreSQL)](#7-sql-드라이버-postgresql)
8. [인코딩/디코딩과 유틸리티](#8-인코딩디코딩과-유틸리티)
9. [레이블 시스템](#9-레이블-시스템)
10. [Records와 메모리 관리](#10-records와-메모리-관리)
11. [히스토리 관리와 MaxHistory](#11-히스토리-관리와-maxhistory)
12. [설계 결정과 Why 분석](#12-설계-결정과-why-분석)

---

## 1. 개요

Helm의 스토리지 드라이버는 Release 정보를 영속적으로 저장하고 조회하는 백엔드 시스템이다.
Helm은 별도의 데이터베이스 서버 없이, 배포 대상인 Kubernetes 클러스터 자체를 스토리지로 활용하는
독특한 접근 방식을 채택하고 있다.

### 왜 Kubernetes를 스토리지로 사용하는가?

1. **별도 인프라 불필요**: Helm 사용을 위한 추가 데이터베이스가 필요 없음
2. **RBAC 통합**: Kubernetes의 기존 권한 체계로 Release 접근 제어
3. **네임스페이스 격리**: Release가 배포된 네임스페이스에 메타데이터도 함께 저장
4. **장애 격리**: 클러스터가 가용하면 Release 정보도 가용
5. **도구 호환성**: kubectl로 Release 정보를 직접 조회/백업 가능

### 드라이버 종류

| 드라이버 | 저장소 | 기본값 | 용도 |
|----------|--------|--------|------|
| Secrets | Kubernetes Secret | **기본** | 프로덕션 (데이터 암호화) |
| ConfigMaps | Kubernetes ConfigMap | - | 레거시 호환 |
| Memory | 인메모리 맵 | - | 테스트 |
| SQL | PostgreSQL | - | 대규모/외부 DB 필요 시 |

### 관련 소스 파일

```
pkg/storage/
├── storage.go               # Storage 구조체, Init, Get/Create/Update/Delete
└── driver/
    ├── driver.go             # Driver 인터페이스, Creator/Updator/Deletor/Queryor
    ├── secrets.go            # Secrets 드라이버
    ├── cfgmaps.go            # ConfigMaps 드라이버
    ├── memory.go             # Memory 드라이버
    ├── sql.go                # SQL 드라이버 (PostgreSQL)
    ├── util.go               # encodeRelease/decodeRelease, 시스템 레이블
    ├── records.go            # records 타입 (Memory 드라이버용)
    └── labels.go             # labels 타입 (메타데이터 관리)
```

---

## 2. Driver 인터페이스

### 2.1 인터페이스 구성

```go
// pkg/storage/driver/driver.go

// Creator - 새 Release 저장
type Creator interface {
    Create(key string, rls release.Releaser) error
}

// Updator - 기존 Release 업데이트
type Updator interface {
    Update(key string, rls release.Releaser) error
}

// Deletor - Release 삭제
type Deletor interface {
    Delete(key string) (release.Releaser, error)
}

// Queryor - Release 조회
type Queryor interface {
    Get(key string) (release.Releaser, error)
    List(filter func(release.Releaser) bool) ([]release.Releaser, error)
    Query(labels map[string]string) ([]release.Releaser, error)
}

// Driver - 위 4개 인터페이스의 합성
type Driver interface {
    Creator
    Updator
    Deletor
    Queryor
    Name() string
}
```

### 2.2 인터페이스 설계 다이어그램

```
                    ┌──────────┐
                    │  Driver  │
                    └────┬─────┘
          ┌──────────────┼──────────────┐──────────┐
          │              │              │          │
    ┌─────┴─────┐ ┌─────┴─────┐ ┌─────┴─────┐ ┌──┴────────┐
    │  Creator  │ │  Updator  │ │  Deletor  │ │  Queryor  │
    ├───────────┤ ├───────────┤ ├───────────┤ ├───────────┤
    │ Create()  │ │ Update()  │ │ Delete()  │ │ Get()     │
    └───────────┘ └───────────┘ └───────────┘ │ List()    │
                                              │ Query()   │
                                              └───────────┘

    구현체:
    ┌──────────┐  ┌────────────┐  ┌──────────┐  ┌────────┐
    │ Secrets  │  │ ConfigMaps │  │  Memory  │  │  SQL   │
    └──────────┘  └────────────┘  └──────────┘  └────────┘
```

### 2.3 에러 타입

```go
// pkg/storage/driver/driver.go

var (
    ErrReleaseNotFound    = errors.New("release: not found")
    ErrReleaseExists      = errors.New("release: already exists")
    ErrInvalidKey         = errors.New("release: invalid key")
    ErrNoDeployedReleases = errors.New("has no deployed releases")
)

type StorageDriverError struct {
    ReleaseName string
    Err         error
}

func (e *StorageDriverError) Error() string {
    return fmt.Sprintf("%q %s", e.ReleaseName, e.Err.Error())
}

func (e *StorageDriverError) Unwrap() error { return e.Err }

func NewErrNoDeployedReleases(releaseName string) error {
    return &StorageDriverError{
        ReleaseName: releaseName,
        Err:         ErrNoDeployedReleases,
    }
}
```

**왜 4개의 하위 인터페이스로 분리하는가?**
- 인터페이스 분리 원칙(ISP): 읽기만 필요한 코드는 Queryor만 의존
- 테스트에서 필요한 메서드만 구현 가능
- 읽기 전용 드라이버 같은 특수 케이스 지원 가능

---

## 3. Storage 계층

### 3.1 Storage 구조체

```go
// pkg/storage/storage.go

const HelmStorageType = "sh.helm.release.v1"

type Storage struct {
    driver.Driver           // 드라이버 임베딩
    MaxHistory int          // 최대 히스토리 보관 수
    logging.LogHolder       // 로거
}
```

### 3.2 초기화

```go
// pkg/storage/storage.go

func Init(d driver.Driver) *Storage {
    if d == nil {
        d = driver.NewMemory()  // 기본: 인메모리
    }
    s := &Storage{
        Driver: d,
    }

    // 드라이버에서 로거 상속
    var h slog.Handler
    if ls, ok := d.(logging.LoggerSetterGetter); ok {
        h = ls.Logger().Handler()
    } else {
        h = slog.Default().Handler()
    }
    s.SetLogger(h)
    return s
}
```

### 3.3 키 생성 규칙

```go
// pkg/storage/storage.go

func makeKey(rlsname string, version int) string {
    return fmt.Sprintf("%s.%s.v%d", HelmStorageType, rlsname, version)
}
```

**키 형식: `sh.helm.release.v1.{name}.v{version}`**

예시:
- `sh.helm.release.v1.my-app.v1` (첫 번째 설치)
- `sh.helm.release.v1.my-app.v2` (첫 번째 업그레이드)
- `sh.helm.release.v1.my-app.v3` (두 번째 업그레이드)

**왜 이 키 형식인가?**
- `sh.helm.release.v1` 접두사로 Helm 관리 객체를 구분
- Release 이름과 버전을 조합하여 유일성 보장
- Kubernetes 리소스 이름 규칙에 맞는 형식 (소문자, 점, 하이픈)
- 다른 Helm 스토리지 타입과 이름 충돌 방지 (GitHub issue #6435)

### 3.4 CRUD 메서드

```go
// pkg/storage/storage.go

func (s *Storage) Get(name string, version int) (release.Releaser, error) {
    return s.Driver.Get(makeKey(name, version))
}

func (s *Storage) Create(rls release.Releaser) error {
    rac, _ := release.NewAccessor(rls)
    // MaxHistory 적용: 공간 확보
    if s.MaxHistory > 0 {
        s.removeLeastRecent(rac.Name(), s.MaxHistory-1)
    }
    return s.Driver.Create(makeKey(rac.Name(), rac.Version()), rls)
}

func (s *Storage) Update(rls release.Releaser) error {
    rac, _ := release.NewAccessor(rls)
    return s.Driver.Update(makeKey(rac.Name(), rac.Version()), rls)
}

func (s *Storage) Delete(name string, version int) (release.Releaser, error) {
    return s.Driver.Delete(makeKey(name, version))
}
```

### 3.5 조회 메서드

```go
// pkg/storage/storage.go

// 모든 릴리스 조회
func (s *Storage) ListReleases() ([]release.Releaser, error) {
    return s.List(func(_ release.Releaser) bool { return true })
}

// 삭제된 릴리스만 조회
func (s *Storage) ListUninstalled() ([]release.Releaser, error) {
    return s.List(func(rls release.Releaser) bool {
        rel, _ := releaserToV1Release(rls)
        return relutil.StatusFilter(common.StatusUninstalled).Check(rel)
    })
}

// 배포된 릴리스만 조회
func (s *Storage) ListDeployed() ([]release.Releaser, error) {
    return s.List(func(rls release.Releaser) bool {
        rel, _ := releaserToV1Release(rls)
        return relutil.StatusFilter(common.StatusDeployed).Check(rel)
    })
}

// 마지막 배포된 릴리스 조회
func (s *Storage) Deployed(name string) (release.Releaser, error) {
    ls, _ := s.DeployedAll(name)
    if len(ls) == 0 {
        return nil, driver.NewErrNoDeployedReleases(name)
    }
    rls, _ := releaseListToV1List(ls)
    // 동시 실행으로 여러 deployed가 있을 수 있으므로 최신 선택
    relutil.Reverse(rls, relutil.SortByRevision)
    return rls[0], nil
}

// 릴리스 히스토리 조회
func (s *Storage) History(name string) ([]release.Releaser, error) {
    return s.Query(map[string]string{"name": name, "owner": "helm"})
}

// 마지막 리비전 조회
func (s *Storage) Last(name string) (release.Releaser, error) {
    h, _ := s.History(name)
    rls, _ := releaseListToV1List(h)
    relutil.Reverse(rls, relutil.SortByRevision)
    return rls[0], nil
}
```

---

## 4. Secrets 드라이버

### 4.1 구조체와 초기화

```go
// pkg/storage/driver/secrets.go

var _ Driver = (*Secrets)(nil)  // 컴파일 타임 인터페이스 검증

const SecretsDriverName = "Secret"

type Secrets struct {
    impl corev1.SecretInterface
    logging.LogHolder
}

func NewSecrets(impl corev1.SecretInterface) *Secrets {
    s := &Secrets{impl: impl}
    s.SetLogger(slog.Default().Handler())
    return s
}
```

### 4.2 Get - Release 조회

```go
// pkg/storage/driver/secrets.go

func (secrets *Secrets) Get(key string) (release.Releaser, error) {
    // 1. Kubernetes Secret 조회
    obj, err := secrets.impl.Get(context.Background(), key, metav1.GetOptions{})
    if err != nil {
        if apierrors.IsNotFound(err) {
            return nil, ErrReleaseNotFound
        }
        return nil, fmt.Errorf("get: failed to get %q: %w", key, err)
    }

    // 2. Secret의 "release" 데이터 필드를 디코딩
    r, err := decodeRelease(string(obj.Data["release"]))
    if err != nil {
        return r, fmt.Errorf("get: failed to decode data %q: %w", key, err)
    }

    // 3. 시스템 레이블 필터링 후 사용자 레이블 주입
    r.Labels = filterSystemLabels(obj.Labels)

    return r, nil
}
```

### 4.3 List - 필터 기반 목록 조회

```go
// pkg/storage/driver/secrets.go

func (secrets *Secrets) List(filter func(release.Releaser) bool) ([]release.Releaser, error) {
    // owner=helm 레이블로 Helm 관리 Secret만 조회
    lsel := kblabels.Set{"owner": "helm"}.AsSelector()
    opts := metav1.ListOptions{LabelSelector: lsel.String()}

    list, err := secrets.impl.List(context.Background(), opts)
    // ...

    var results []release.Releaser
    for _, item := range list.Items {
        rls, err := decodeRelease(string(item.Data["release"]))
        if err != nil { continue }  // 디코딩 실패 건너뛰기
        rls.Labels = item.Labels
        if filter(rls) {
            results = append(results, rls)
        }
    }
    return results, nil
}
```

### 4.4 Create - Release 저장

```go
// pkg/storage/driver/secrets.go

func (secrets *Secrets) Create(key string, rel release.Releaser) error {
    var lbs labels
    rls, _ := releaserToV1Release(rel)

    // 레이블 초기화
    lbs.init()
    lbs.fromMap(rls.Labels)
    lbs.set("createdAt", fmt.Sprintf("%v", time.Now().Unix()))

    // Secret 객체 생성
    obj, err := newSecretsObject(key, rls, lbs)
    if err != nil {
        return fmt.Errorf("create: failed to encode release %q: %w", rls.Name, err)
    }

    // Kubernetes API로 전송
    if _, err := secrets.impl.Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
        if apierrors.IsAlreadyExists(err) {
            return ErrReleaseExists
        }
        return fmt.Errorf("create: failed to create: %w", err)
    }
    return nil
}
```

### 4.5 Secret 객체 구성

```go
// pkg/storage/driver/secrets.go

func newSecretsObject(key string, rls *rspb.Release, lbs labels) (*v1.Secret, error) {
    const owner = "helm"

    // Release를 base64+gzip으로 인코딩
    s, err := encodeRelease(rls)
    if err != nil { return nil, err }

    // 레이블 설정
    lbs.fromMap(rls.Labels)   // 사용자 커스텀 레이블
    lbs.set("name", rls.Name)
    lbs.set("owner", owner)
    lbs.set("status", rls.Info.Status.String())
    lbs.set("version", strconv.Itoa(rls.Version))

    return &v1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:   key,         // sh.helm.release.v1.{name}.v{version}
            Labels: lbs.toMap(),
        },
        Type: "helm.sh/release.v1",  // Secret 타입 식별자
        Data: map[string][]byte{
            "release": []byte(s),     // 인코딩된 Release 데이터
        },
    }, nil
}
```

### 4.6 Secret 객체 구조

```
┌──────────────────────────────────────────────┐
│          Kubernetes Secret                    │
├──────────────────────────────────────────────┤
│ metadata:                                     │
│   name: sh.helm.release.v1.my-app.v3         │
│   labels:                                     │
│     name: my-app                              │
│     owner: helm                               │
│     status: deployed                          │
│     version: "3"                              │
│     createdAt: "1709312400"                   │
│     team: backend            ← 사용자 레이블  │
│ type: helm.sh/release.v1                      │
│ data:                                         │
│   release: H4sIAAAAAAAAA...  ← base64+gzip   │
└──────────────────────────────────────────────┘
```

### 4.7 Update와 Delete

```go
// pkg/storage/driver/secrets.go

func (secrets *Secrets) Update(key string, rel release.Releaser) error {
    var lbs labels
    rls, _ := releaserToV1Release(rel)
    lbs.init()
    lbs.fromMap(rls.Labels)
    lbs.set("modifiedAt", fmt.Sprintf("%v", time.Now().Unix()))

    obj, _ := newSecretsObject(key, rls, lbs)
    _, err = secrets.impl.Update(context.Background(), obj, metav1.UpdateOptions{})
    return err
}

func (secrets *Secrets) Delete(key string) (rls release.Releaser, err error) {
    // 먼저 조회하여 존재 확인 + 반환용 데이터 확보
    if rls, err = secrets.Get(key); err != nil {
        return nil, err
    }
    // 삭제 실행
    err = secrets.impl.Delete(context.Background(), key, metav1.DeleteOptions{})
    return rls, err
}
```

---

## 5. ConfigMaps 드라이버

### 5.1 구조체

```go
// pkg/storage/driver/cfgmaps.go

var _ Driver = (*ConfigMaps)(nil)

const ConfigMapsDriverName = "ConfigMap"

type ConfigMaps struct {
    impl corev1.ConfigMapInterface
    logging.LogHolder
}

func NewConfigMaps(impl corev1.ConfigMapInterface) *ConfigMaps {
    c := &ConfigMaps{impl: impl}
    c.SetLogger(slog.Default().Handler())
    return c
}
```

### 5.2 Secrets와의 차이점

| 항목 | Secrets | ConfigMaps |
|------|---------|------------|
| Kubernetes 리소스 | Secret | ConfigMap |
| 데이터 필드 | `Data` ([]byte) | `Data` (string) |
| 타입 필드 | `Type: "helm.sh/release.v1"` | 없음 |
| at-rest 암호화 | 지원 (etcd encryption) | 미지원 |
| RBAC 분리 | Secret 권한으로 분리 가능 | ConfigMap과 같은 권한 |

### 5.3 ConfigMap 객체 구성

```go
// pkg/storage/driver/cfgmaps.go

func newConfigMapsObject(key string, rls *rspb.Release, lbs labels) (*v1.ConfigMap, error) {
    const owner = "helm"

    s, err := encodeRelease(rls)
    if err != nil { return nil, err }

    lbs.fromMap(rls.Labels)
    lbs.set("name", rls.Name)
    lbs.set("owner", owner)
    lbs.set("status", rls.Info.Status.String())
    lbs.set("version", strconv.Itoa(rls.Version))

    return &v1.ConfigMap{
        ObjectMeta: metav1.ObjectMeta{
            Name:   key,
            Labels: lbs.toMap(),
        },
        Data: map[string]string{   // string 타입 (Secret은 []byte)
            "release": s,
        },
    }, nil
}
```

**왜 Secrets가 기본인가?**
- Release에는 Values(설정값)가 포함되어 있어 민감 정보를 담을 수 있음
- Kubernetes의 etcd 암호화를 활용하면 Secret은 at-rest 암호화됨
- ConfigMap은 평문으로 저장되어 보안에 취약
- Helm 3부터 Secrets가 기본 드라이버로 변경됨

---

## 6. Memory 드라이버

### 6.1 구조체

```go
// pkg/storage/driver/memory.go

var _ Driver = (*Memory)(nil)

const MemoryDriverName = "Memory"
const defaultNamespace = "default"

type memReleases map[string]records  // release이름 → records

type Memory struct {
    sync.RWMutex
    namespace string
    cache     map[string]memReleases  // 네임스페이스 → memReleases
    logging.LogHolder
}

func NewMemory() *Memory {
    m := &Memory{
        cache:     map[string]memReleases{},
        namespace: "default",
    }
    m.SetLogger(slog.Default().Handler())
    return m
}
```

### 6.2 데이터 구조

```
Memory.cache:
  ┌──────────────────────────────────────────────────┐
  │ "default" (namespace)                             │
  │   ├── "my-app" → records:                        │
  │   │     ├── record{key: "...v1", rls: Release}   │
  │   │     ├── record{key: "...v2", rls: Release}   │
  │   │     └── record{key: "...v3", rls: Release}   │
  │   └── "other-app" → records:                     │
  │         └── record{key: "...v1", rls: Release}   │
  ├──────────────────────────────────────────────────┤
  │ "production" (namespace)                          │
  │   └── "my-app" → records:                        │
  │         └── record{key: "...v1", rls: Release}   │
  └──────────────────────────────────────────────────┘
```

### 6.3 동시성 제어

```go
// pkg/storage/driver/memory.go

func (mem *Memory) Get(key string) (release.Releaser, error) {
    defer unlock(mem.rlock())  // 읽기 락
    // ...
}

func (mem *Memory) Create(key string, rel release.Releaser) error {
    defer unlock(mem.wlock())  // 쓰기 락
    // ...
}

// 락 헬퍼 함수들
func (mem *Memory) wlock() func() {
    mem.Lock()
    return func() { mem.Unlock() }
}

func (mem *Memory) rlock() func() {
    mem.RLock()
    return func() { mem.RUnlock() }
}

func unlock(fn func()) { fn() }
```

**왜 이런 락 패턴을 사용하는가?**
- `defer unlock(mem.rlock())`는 락 획득과 해제를 한 줄로 표현
- `rlock()`은 `RLock()`을 호출하고 `RUnlock` 함수를 반환
- `defer`가 블록 종료 시 자동으로 `RUnlock()` 호출
- 읽기(RLock)/쓰기(Lock) 구분으로 동시 읽기 허용

### 6.4 Get - 키 파싱

```go
// pkg/storage/driver/memory.go

func (mem *Memory) Get(key string) (release.Releaser, error) {
    defer unlock(mem.rlock())

    // "sh.helm.release.v1.my-app.v3" → "my-app.v3" → ["my-app", "3"]
    keyWithoutPrefix := strings.TrimPrefix(key, "sh.helm.release.v1.")
    switch elems := strings.Split(keyWithoutPrefix, ".v"); len(elems) {
    case 2:
        name, ver := elems[0], elems[1]
        if _, err := strconv.Atoi(ver); err != nil {
            return nil, ErrInvalidKey
        }
        if recs, ok := mem.cache[mem.namespace][name]; ok {
            if r := recs.Get(key); r != nil {
                return r.rls, nil
            }
        }
        return nil, ErrReleaseNotFound
    default:
        return nil, ErrInvalidKey
    }
}
```

### 6.5 Create - 네임스페이스 초기화

```go
// pkg/storage/driver/memory.go

func (mem *Memory) Create(key string, rel release.Releaser) error {
    defer unlock(mem.wlock())

    rls, _ := releaserToV1Release(rel)
    namespace := rls.Namespace
    if namespace == "" {
        namespace = defaultNamespace
    }
    mem.SetNamespace(namespace)

    // 네임스페이스 맵 초기화
    if _, ok := mem.cache[namespace]; !ok {
        mem.cache[namespace] = memReleases{}
    }

    // 기존 records가 있으면 추가, 없으면 새로 생성
    if recs, ok := mem.cache[namespace][rls.Name]; ok {
        if err := recs.Add(newRecord(key, rls)); err != nil {
            return err  // ErrReleaseExists
        }
        mem.cache[namespace][rls.Name] = recs
        return nil
    }
    mem.cache[namespace][rls.Name] = records{newRecord(key, rls)}
    return nil
}
```

### 6.6 List - 네임스페이스 범위 조회

```go
// pkg/storage/driver/memory.go

func (mem *Memory) List(filter func(release.Releaser) bool) ([]release.Releaser, error) {
    defer unlock(mem.rlock())

    var ls []release.Releaser
    for namespace := range mem.cache {
        if mem.namespace != "" {
            namespace = mem.namespace  // 특정 네임스페이스만
        }
        for _, recs := range mem.cache[namespace] {
            recs.Iter(func(_ int, rec *record) bool {
                if filter(rec.rls) {
                    ls = append(ls, rec.rls)
                }
                return true
            })
        }
        if mem.namespace != "" {
            break  // 특정 네임스페이스만 처리 후 종료
        }
    }
    return ls, nil
}
```

---

## 7. SQL 드라이버 (PostgreSQL)

### 7.1 구조체

```go
// pkg/storage/driver/sql.go

var _ Driver = (*SQL)(nil)

const SQLDriverName = "SQL"
const sqlReleaseTableName = "releases_v1"
const sqlCustomLabelsTableName = "custom_labels_v1"

type SQL struct {
    db               *sqlx.DB
    namespace        string
    statementBuilder sq.StatementBuilderType  // squirrel 쿼리 빌더
    logging.LogHolder
}
```

### 7.2 테이블 스키마

```go
// pkg/storage/driver/sql.go - ensureDBSetup()

// releases_v1 테이블
CREATE TABLE releases_v1 (
    key         VARCHAR(90),       -- sh.helm.release.v1.{name}.v{version}
    type        VARCHAR(64) NOT NULL,  -- helm.sh/release.v1
    body        TEXT NOT NULL,     -- base64+gzip 인코딩된 Release
    name        VARCHAR(64) NOT NULL,
    namespace   VARCHAR(64) NOT NULL,
    version     INTEGER NOT NULL,
    status      TEXT NOT NULL,
    owner       TEXT NOT NULL,
    createdAt   INTEGER NOT NULL,
    modifiedAt  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(key, namespace)
);

// 인덱스
CREATE INDEX ON releases_v1 (key, namespace);
CREATE INDEX ON releases_v1 (version);
CREATE INDEX ON releases_v1 (status);
CREATE INDEX ON releases_v1 (owner);
CREATE INDEX ON releases_v1 (createdAt);
CREATE INDEX ON releases_v1 (modifiedAt);

// custom_labels_v1 테이블
CREATE TABLE custom_labels_v1 (
    releaseKey       VARCHAR(64),
    releaseNamespace VARCHAR(67),
    key              VARCHAR(317),  -- 253 + 1 + 63
    value            VARCHAR(63)
);
CREATE INDEX ON custom_labels_v1 (releaseKey, releaseNamespace);

// Row Level Security 활성화
ALTER TABLE releases_v1 ENABLE ROW LEVEL SECURITY;
ALTER TABLE custom_labels_v1 ENABLE ROW LEVEL SECURITY;
```

### 7.3 SQLReleaseWrapper

```go
// pkg/storage/driver/sql.go

type SQLReleaseWrapper struct {
    Key        string `db:"key"`
    Type       string `db:"type"`
    Body       string `db:"body"`        // base64+gzip Release
    Name       string `db:"name"`
    Namespace  string `db:"namespace"`
    Version    int    `db:"version"`
    Status     string `db:"status"`
    Owner      string `db:"owner"`
    CreatedAt  int    `db:"createdAt"`
    ModifiedAt int    `db:"modifiedAt"`
}

type SQLReleaseCustomLabelWrapper struct {
    ReleaseKey       string `db:"release_key"`
    ReleaseNamespace string `db:"release_namespace"`
    Key              string `db:"key"`
    Value            string `db:"value"`
}
```

### 7.4 초기화와 마이그레이션

```go
// pkg/storage/driver/sql.go

func NewSQL(connectionString string, namespace string) (*SQL, error) {
    db, _ := sqlx.Connect("postgres", connectionString)
    driver := &SQL{
        db:               db,
        statementBuilder: sq.StatementBuilder.PlaceholderFormat(sq.Dollar),
    }
    driver.ensureDBSetup()  // 마이그레이션 실행
    driver.namespace = namespace
    return driver, nil
}

func (s *SQL) ensureDBSetup() error {
    migrations := &migrate.MemoryMigrationSource{
        Migrations: []*migrate.Migration{
            {Id: "init", Up: []string{/* releases_v1 CREATE */}},
            {Id: "custom_labels", Up: []string{/* custom_labels_v1 CREATE */}},
        },
    }
    // 이미 적용된 마이그레이션은 건너뛰기
    if s.checkAlreadyApplied(migrations.Migrations) {
        return nil
    }
    _, err := migrate.Exec(s.db.DB, "postgres", migrations, migrate.Up)
    return err
}
```

### 7.5 Create - 트랜잭션 기반 저장

```go
// pkg/storage/driver/sql.go

func (s *SQL) Create(key string, rel release.Releaser) error {
    rls, _ := releaserToV1Release(rel)
    body, _ := encodeRelease(rls)

    transaction, _ := s.db.Beginx()

    // 1. Release 레코드 삽입
    insertQuery, args, _ := s.statementBuilder.
        Insert(sqlReleaseTableName).
        Columns("key", "type", "body", "name", "namespace",
                "version", "status", "owner", "createdAt").
        Values(key, sqlReleaseDefaultType, body, rls.Name,
               namespace, rls.Version, rls.Info.Status.String(),
               sqlReleaseDefaultOwner, time.Now().Unix()).
        ToSql()

    if _, err := transaction.Exec(insertQuery, args...); err != nil {
        defer transaction.Rollback()
        // 이미 존재하는지 확인
        // ...
        return ErrReleaseExists  // 또는 원래 에러
    }

    // 2. 커스텀 레이블 삽입 (시스템 레이블 제외)
    for k, v := range filterSystemLabels(rls.Labels) {
        insertLabelsQuery, args, _ := s.statementBuilder.
            Insert(sqlCustomLabelsTableName).
            Columns("releaseKey", "releaseNamespace", "key", "value").
            Values(key, namespace, k, v).
            ToSql()

        if _, err := transaction.Exec(insertLabelsQuery, args...); err != nil {
            defer transaction.Rollback()
            return err
        }
    }
    defer transaction.Commit()
    return nil
}
```

### 7.6 Query - 레이블 기반 조회

```go
// pkg/storage/driver/sql.go

func (s *SQL) Query(labels map[string]string) ([]release.Releaser, error) {
    sb := s.statementBuilder.
        Select("key", "namespace", "body").
        From(sqlReleaseTableName)

    // 알려진 레이블만 WHERE 조건으로 추가
    for _, key := range sortedKeys {
        if _, ok := labelMap[key]; ok {
            sb = sb.Where(sq.Eq{key: labels[key]})
        } else {
            return nil, fmt.Errorf("unknown label %s", key)
        }
    }

    if s.namespace != "" {
        sb = sb.Where(sq.Eq{"namespace": s.namespace})
    }
    // ...
}
```

### 7.7 Delete - 트랜잭션 기반 삭제

```go
// pkg/storage/driver/sql.go

func (s *SQL) Delete(key string) (release.Releaser, error) {
    transaction, _ := s.db.Beginx()

    // 1. Release 조회 (존재 확인)
    var record SQLReleaseWrapper
    transaction.Get(&record, selectQuery, args...)

    release, _ := decodeRelease(record.Body)

    // 2. Release 레코드 삭제
    transaction.Exec(deleteQuery, args...)

    // 3. 커스텀 레이블 조회 (반환용)
    release.Labels, _ = s.getReleaseCustomLabels(key, s.namespace)

    // 4. 커스텀 레이블 삭제
    transaction.Exec(deleteCustomLabelsQuery, args...)

    defer transaction.Commit()
    return release, nil
}
```

### 7.8 SQL vs Kubernetes 드라이버 비교

| 항목 | Kubernetes (Secrets/ConfigMaps) | SQL (PostgreSQL) |
|------|-------------------------------|-----------------|
| 저장 위치 | etcd (Kubernetes 내부) | 외부 PostgreSQL |
| 레이블 쿼리 | Kubernetes LabelSelector | SQL WHERE 절 |
| 트랜잭션 | Kubernetes API 원자성 | SQL 트랜잭션 |
| 커스텀 레이블 | ObjectMeta.Labels에 직접 저장 | 별도 테이블 |
| 확장성 | etcd 크기 제한 | DB 확장 가능 |
| Row Level Security | Kubernetes RBAC | PostgreSQL RLS |
| 마이그레이션 | 필요 없음 | sql-migrate 사용 |

---

## 8. 인코딩/디코딩과 유틸리티

### 8.1 Release 인코딩

```go
// pkg/storage/driver/util.go

var b64 = base64.StdEncoding
var magicGzip = []byte{0x1f, 0x8b, 0x08}

func encodeRelease(rls *rspb.Release) (string, error) {
    // 1. JSON 직렬화
    b, err := json.Marshal(rls)
    if err != nil { return "", err }

    // 2. Gzip 압축 (최고 압축률)
    var buf bytes.Buffer
    w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
    w.Write(b)
    w.Close()

    // 3. Base64 인코딩
    return b64.EncodeToString(buf.Bytes()), nil
}
```

### 8.2 Release 디코딩

```go
// pkg/storage/driver/util.go

func decodeRelease(data string) (*rspb.Release, error) {
    // 1. Base64 디코딩
    b, err := b64.DecodeString(data)
    if err != nil { return nil, err }

    // 2. Gzip 매직 넘버 확인 (하위 호환성)
    if len(b) > 3 && bytes.Equal(b[0:3], magicGzip) {
        r, _ := gzip.NewReader(bytes.NewReader(b))
        defer r.Close()
        b, _ = io.ReadAll(r)
    }
    // 압축 도입 전 릴리스와의 호환성: gzip 헤더가 없으면 그대로 사용

    // 3. JSON 역직렬화
    var rls rspb.Release
    json.Unmarshal(b, &rls)
    return &rls, nil
}
```

### 8.3 인코딩 파이프라인

```
Release 구조체
    │
    ▼ json.Marshal
JSON 바이트열
    │
    ▼ gzip.BestCompression
압축된 바이트열
    │
    ▼ base64.StdEncoding
Base64 문자열 ──→ Secret.Data["release"] 또는 ConfigMap.Data["release"]
```

**왜 이 인코딩을 사용하는가?**
- JSON: Go 구조체의 표준 직렬화 포맷
- Gzip: Release에 차트 전체가 포함되므로 크기 절감 필수
- Base64: Kubernetes Secret의 Data 필드는 바이너리를 base64로 저장
- `gzip.BestCompression`: 저장 공간이 중요하므로 최대 압축

### 8.4 시스템 레이블 관리

```go
// pkg/storage/driver/util.go

var systemLabels = []string{
    "name", "owner", "status", "version", "createdAt", "modifiedAt",
}

func isSystemLabel(key string) bool {
    return slices.Contains(GetSystemLabels(), key)
}

func filterSystemLabels(lbs map[string]string) map[string]string {
    result := make(map[string]string)
    for k, v := range lbs {
        if !isSystemLabel(k) {
            result[k] = v
        }
    }
    return result
}

func ContainsSystemLabels(lbs map[string]string) bool {
    for k := range lbs {
        if isSystemLabel(k) { return true }
    }
    return false
}
```

**시스템 레이블 vs 사용자 레이블:**

| 레이블 | 타입 | 예시 |
|--------|------|------|
| `name` | 시스템 | `"my-app"` |
| `owner` | 시스템 | `"helm"` |
| `status` | 시스템 | `"deployed"` |
| `version` | 시스템 | `"3"` |
| `createdAt` | 시스템 | `"1709312400"` |
| `modifiedAt` | 시스템 | `"1709398800"` |
| `team` | 사용자 | `"backend"` |
| `env` | 사용자 | `"production"` |

---

## 9. 레이블 시스템

### 9.1 labels 타입

```go
// pkg/storage/driver/labels.go

type labels map[string]string

func (lbs *labels) init()                { *lbs = labels(make(map[string]string)) }
func (lbs labels) get(key string) string { return lbs[key] }
func (lbs labels) set(key, val string)   { lbs[key] = val }

func (lbs labels) keys() (ls []string) {
    for key := range lbs {
        ls = append(ls, key)
    }
    return
}

func (lbs labels) match(set labels) bool {
    for _, key := range set.keys() {
        if lbs.get(key) != set.get(key) {
            return false
        }
    }
    return true
}

func (lbs labels) toMap() map[string]string { return lbs }

func (lbs *labels) fromMap(kvs map[string]string) {
    for k, v := range kvs {
        lbs.set(k, v)
    }
}
```

### 9.2 레이블 활용

**Kubernetes 드라이버에서:**
- `LabelSelector`로 서버 사이드 필터링
- `owner=helm`으로 Helm 관리 리소스만 조회
- `name=my-app`으로 특정 릴리스의 모든 리비전 조회
- `status=deployed`로 배포된 릴리스만 조회

**Memory 드라이버에서:**
- `labels.match()`로 인메모리 필터링

**SQL 드라이버에서:**
- 시스템 레이블은 테이블 컬럼으로 매핑 (WHERE 절에서 사용)
- 사용자 레이블은 `custom_labels_v1` 테이블에 별도 저장

---

## 10. Records와 메모리 관리

### 10.1 record 구조체

```go
// pkg/storage/driver/records.go

type record struct {
    key string
    lbs labels
    rls *rspb.Release
}

func newRecord(key string, rls *rspb.Release) *record {
    var lbs labels
    lbs.init()
    lbs.set("name", rls.Name)
    lbs.set("owner", "helm")
    lbs.set("status", rls.Info.Status.String())
    lbs.set("version", strconv.Itoa(rls.Version))
    return &record{key: key, lbs: lbs, rls: rls}
}
```

### 10.2 records 컬렉션

```go
// pkg/storage/driver/records.go

type records []*record

// 정렬: Version 기준 오름차순
func (rs records) Less(i, j int) bool {
    return rs[i].rls.Version < rs[j].rls.Version
}

// 추가 (중복 검사 + 자동 정렬)
func (rs *records) Add(r *record) error {
    if r == nil { return nil }
    if rs.Exists(r.key) {
        return ErrReleaseExists
    }
    *rs = append(*rs, r)
    sort.Sort(*rs)  // Version 순 정렬 유지
    return nil
}

// 조회
func (rs records) Get(key string) *record {
    if i, ok := rs.Index(key); ok {
        return rs[i]
    }
    return nil
}

// 순회 (복사본으로 안전한 순회)
func (rs *records) Iter(fn func(int, *record) bool) {
    cp := make([]*record, len(*rs))
    copy(cp, *rs)
    for i, r := range cp {
        if !fn(i, r) { return }
    }
}

// 존재 확인
func (rs records) Exists(key string) bool {
    _, ok := rs.Index(key)
    return ok
}

// 삭제
func (rs *records) Remove(key string) (r *record) {
    if i, ok := rs.Index(key); ok {
        return rs.removeAt(i)
    }
    return nil
}

// 교체
func (rs *records) Replace(key string, rec *record) *record {
    if i, ok := rs.Index(key); ok {
        old := (*rs)[i]
        (*rs)[i] = rec
        return old
    }
    return nil
}

// 내부: 인덱스 기반 삭제
func (rs *records) removeAt(index int) *record {
    r := (*rs)[index]
    (*rs)[index] = nil
    copy((*rs)[index:], (*rs)[index+1:])
    *rs = (*rs)[:len(*rs)-1]
    return r
}
```

**왜 Iter에서 복사본을 사용하는가?**
- 순회 중에 records가 수정되면 인덱스가 어긋남
- 복사본으로 순회하면 원본 수정에 안전
- 메모리 드라이버에서 RWMutex와 함께 사용하여 동시성 안전 보장

---

## 11. 히스토리 관리와 MaxHistory

### 11.1 removeLeastRecent

```go
// pkg/storage/storage.go

func (s *Storage) removeLeastRecent(name string, maximum int) error {
    if maximum < 0 { return nil }

    h, _ := s.History(name)
    if len(h) <= maximum { return nil }

    rls, _ := releaseListToV1List(h)
    relutil.SortByRevision(rls)  // 오래된 것부터

    // 현재 deployed 릴리스 확인
    lastDeployed, _ := s.Deployed(name)

    var toDelete []release.Releaser
    for _, rel := range rls {
        if len(rls)-len(toDelete) == maximum { break }

        if lastDeployed != nil {
            ldac, _ := release.NewAccessor(lastDeployed)
            // 현재 deployed 릴리스는 삭제하지 않음
            if rel.Version != ldac.Version() {
                toDelete = append(toDelete, rel)
            }
        } else {
            toDelete = append(toDelete, rel)
        }
    }

    // 가능한 만큼 삭제 (API 제한 시 다음 호출에서 나머지 삭제)
    errs := []error{}
    for _, rel := range toDelete {
        rac, _ := release.NewAccessor(rel)
        err = s.deleteReleaseVersion(name, rac.Version())
        if err != nil {
            errs = append(errs, err)
        }
    }
    return ...
}
```

### 11.2 MaxHistory 동작 흐름

```
MaxHistory = 3 설정 시:

Create 호출 전:
  v1 (superseded) ← 삭제 대상
  v2 (superseded) ← 삭제 대상
  v3 (superseded)
  v4 (deployed)    ← 보호됨

Create 호출 (v5 생성):
  1. removeLeastRecent(name, 3-1=2) 호출
  2. v1, v2 삭제 (deployed v4는 보호)
  3. v5 생성

결과:
  v3 (superseded)
  v4 (deployed)    ← 이전 deployed도 보호
  v5 (deployed)    ← 새로 생성
```

**왜 deployed 릴리스를 보호하는가?**
- 롤백 시 deployed 상태의 릴리스가 필요할 수 있음
- 동시 실행으로 여러 deployed가 있을 수 있음 (Deployed() 메서드에서 최신 선택)
- deployed를 삭제하면 "마지막 성공 배포"에 대한 정보가 소실

---

## 12. 설계 결정과 Why 분석

### 12.1 왜 Kubernetes 네이티브 스토리지를 사용하는가?

Helm 2는 서버(Tiller) 내부 메모리와 ConfigMap을 사용했다.
Helm 3에서 Tiller를 제거하면서 클라이언트만으로 동작해야 했고,
Kubernetes API 서버가 유일한 공유 상태 저장소가 되었다.

대안으로 로컬 파일 시스템이나 외부 DB를 고려할 수 있었지만:
- 파일 시스템: 여러 클라이언트 간 공유 불가
- 외부 DB: 추가 인프라 필요
- etcd 직접 접근: Kubernetes API를 우회하므로 보안 문제

### 12.2 왜 Secret 타입에 helm.sh/release.v1을 사용하는가?

```go
Type: "helm.sh/release.v1",
```

- Kubernetes Secret은 `Type` 필드로 용도를 구분
- `helm.sh/release.v1`은 Helm Release 전용 Secret임을 명시
- 향후 Release 메타데이터 변경 시 `v2`로 버전업하여 호환성 관리
- `kubectl get secrets --field-selector type=helm.sh/release.v1`로 필터링 가능

### 12.3 왜 base64 + gzip을 사용하는가?

Release 객체에는 차트 전체(템플릿, 기본 Values, 메타데이터)와 렌더링된 매니페스트가 포함된다.
이는 수 MB에 달할 수 있어 압축 없이는 etcd 크기 제한(기본 1.5MB)에 금방 도달한다.

```
압축 효과 예시:
  원본 JSON:  500KB
  Gzip:       ~50KB (90% 감소)
  Base64:     ~67KB (base64 오버헤드 ~33%)

  최종:       원본의 ~13%
```

### 12.4 왜 SQL 드라이버가 PostgreSQL만 지원하는가?

```go
const postgreSQLDialect = "postgres"
```

- PostgreSQL은 Row Level Security를 지원하여 멀티테넌트 환경에서 안전
- JSONB 타입으로 Release 데이터에 대한 추가 쿼리 가능
- 대규모 환경(수천 개 릴리스)에서 etcd보다 확장성이 좋음
- 커뮤니티에서 가장 요청이 많았던 외부 DB

### 12.5 왜 커스텀 레이블을 별도 테이블에 저장하는가?

SQL 드라이버에서 시스템 레이블은 `releases_v1` 테이블의 컬럼으로,
사용자 레이블은 `custom_labels_v1` 테이블에 저장한다.

- 시스템 레이블: SQL WHERE 절로 효율적 필터링 (인덱스 활용)
- 사용자 레이블: 동적 키-값이므로 별도 테이블이 유연
- Kubernetes 레이블의 키 길이(최대 317자)와 값 길이(최대 63자) 제한 준수

### 12.6 왜 Driver 인터페이스에 Name()이 있는가?

```go
type Driver interface {
    // ...
    Name() string
}
```

- 로깅에서 어떤 드라이버를 사용 중인지 표시
- 에러 메시지에 드라이버 정보 포함
- 런타임에 드라이버 타입을 확인하는 용도

### 12.7 전체 아키텍처 요약

```
┌──────────────────────────────────────────────────────┐
│                    pkg/action                         │
│  (install, upgrade, rollback, uninstall, list, ...)  │
└───────────────────────┬──────────────────────────────┘
                        │
┌───────────────────────▼──────────────────────────────┐
│                  pkg/storage                          │
│  Storage 구조체                                       │
│  ├── Get(name, version) → Release                    │
│  ├── Create(release) → error                         │
│  ├── Update(release) → error                         │
│  ├── Delete(name, version) → Release                 │
│  ├── History(name) → []Release                       │
│  ├── Last(name) → Release                            │
│  ├── Deployed(name) → Release                        │
│  └── removeLeastRecent(name, max) → error            │
│       (MaxHistory 기반 히스토리 정리)                   │
└───────────────────────┬──────────────────────────────┘
                        │ Driver 인터페이스
┌───────────────────────▼──────────────────────────────┐
│              pkg/storage/driver                       │
│                                                       │
│  ┌──────────┐  ┌────────────┐  ┌────────┐  ┌───────┐│
│  │ Secrets  │  │ ConfigMaps │  │ Memory │  │  SQL  ││
│  │          │  │            │  │        │  │       ││
│  │ K8s      │  │ K8s        │  │ In-    │  │ Post- ││
│  │ Secret   │  │ ConfigMap  │  │ Memory │  │ greSQL││
│  └────┬─────┘  └─────┬──────┘  └────┬───┘  └───┬───┘│
│       │              │              │           │    │
│  ┌────▼──────────────▼──────────────▼───────────▼──┐ │
│  │               util.go                           │ │
│  │  encodeRelease() / decodeRelease()              │ │
│  │  JSON → gzip → base64 / base64 → gzip → JSON   │ │
│  └─────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────┘
```

---

## 참고: 핵심 소스 파일 경로

| 파일 | 경로 |
|------|------|
| Storage 구조체 | `pkg/storage/storage.go` |
| Driver 인터페이스 | `pkg/storage/driver/driver.go` |
| Secrets 드라이버 | `pkg/storage/driver/secrets.go` |
| ConfigMaps 드라이버 | `pkg/storage/driver/cfgmaps.go` |
| Memory 드라이버 | `pkg/storage/driver/memory.go` |
| SQL 드라이버 | `pkg/storage/driver/sql.go` |
| 인코딩/디코딩 | `pkg/storage/driver/util.go` |
| records (메모리용) | `pkg/storage/driver/records.go` |
| labels 유틸리티 | `pkg/storage/driver/labels.go` |
