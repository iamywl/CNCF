# 15. 인증과 RBAC (Role-Based Access Control)

## 개요

etcd의 인증/인가 시스템은 gRPC API에 대한 접근 제어를 제공한다. 사용자(User)에게 역할(Role)을 부여하고, 각 역할에 키 범위 기반의 권한(Permission)을 설정하는 RBAC 모델을 사용한다.

인증 시스템의 핵심 설계 목표:
1. **성능 최소화**: 인증이 활성화되어도 읽기/쓰기 성능에 미치는 영향을 최소화
2. **일관성**: 인증 상태 변경이 RAFT를 통해 모든 노드에 일관되게 전파
3. **유연한 토큰**: Simple Token과 JWT 두 가지 토큰 방식 지원

---

## 데이터 모델: auth.proto

`api/authpb/auth.proto`에 인증 시스템의 핵심 데이터 모델이 정의되어 있다.

### User 메시지

```protobuf
message User {
  bytes name = 1;
  bytes password = 2;
  repeated string roles = 3;
  UserAddOptions options = 4;
}
```

| 필드 | 타입 | 설명 |
|------|------|------|
| name | bytes | 사용자 이름 |
| password | bytes | bcrypt 해시된 비밀번호 |
| roles | repeated string | 부여된 역할 목록 (정렬됨) |
| options | UserAddOptions | 옵션 (NoPassword 등) |

**왜 roles가 정렬되어 있는가?**

`UserGrantRole()`에서 `sort.Strings(user.Roles)`로 정렬한다. 이진 탐색(`sort.SearchStrings`)을 통해 O(log N)로 역할 존재 여부를 확인할 수 있다. `hasRootRole()`이 이 최적화를 활용한다:

```go
func hasRootRole(u *authpb.User) bool {
    idx := sort.SearchStrings(u.Roles, rootRole)
    return idx != len(u.Roles) && u.Roles[idx] == rootRole
}
```

### Permission 메시지

```protobuf
message Permission {
  enum Type {
    READ = 0;
    WRITE = 1;
    READWRITE = 2;
  }
  Type permType = 1;
  bytes key = 2;
  bytes range_end = 3;
}
```

| 필드 | 타입 | 설명 |
|------|------|------|
| permType | Type | 읽기, 쓰기, 읽기/쓰기 |
| key | bytes | 키 또는 범위 시작 |
| range_end | bytes | 범위 끝 (비어있으면 단일 키) |

### Permission 범위 규칙

```
범위 표현 규칙:
├── rangeEnd = nil 또는 []     → 단일 키 (key만)
├── rangeEnd = 일반 바이트열    → [key, rangeEnd) 범위
└── rangeEnd = []byte{0x00}    → key 이상의 모든 키 (open-ended)
    └── 내부적으로 []byte{}로 변환 → BytesAffineComparable의 최대 원소
```

**왜 `[]byte{0x00}`이 open-ended를 나타내는가?**

소스코드 주석에 상세히 설명되어 있다:
> `[]byte{0x00}` is the minimum valid etcd key. So `(X, []byte{0x00})` would represent an empty range. Since such a range makes no sense as a permission, it is repurposed to represent an open-ended range.

etcd의 최소 유효 키는 `[]byte{0x00}`이다. 따라서 `(X, []byte{0x00})`은 빈 범위가 되어 권한으로 의미가 없으므로, 이를 open-ended 범위의 표현으로 재활용한다. 이는 Range() API와 Watch() API의 규칙과 동일하다.

### Role 메시지

```protobuf
message Role {
  bytes name = 1;
  repeated Permission keyPermission = 2;
}
```

| 필드 | 타입 | 설명 |
|------|------|------|
| name | bytes | 역할 이름 |
| keyPermission | repeated Permission | 키 범위 권한 목록 |

---

## AuthStore 인터페이스

`server/auth/store.go`에 정의된 `AuthStore` 인터페이스는 인증/인가 시스템의 전체 계약이다.

```
AuthStore 인터페이스:
├── 인증 활성화/비활성화
│   ├── AuthEnable()  → 인증 활성화
│   ├── AuthDisable() → 인증 비활성화
│   └── IsAuthEnabled() → 활성 상태 확인
│
├── 인증
│   ├── Authenticate(ctx, username, password) → 토큰 발급
│   ├── CheckPassword(username, password) → 비밀번호 검증
│   └── GenTokenPrefix() → 토큰 접두사 생성
│
├── 사용자 관리
│   ├── UserAdd, UserDelete, UserGet, UserList
│   ├── UserChangePassword
│   ├── UserGrantRole, UserRevokeRole
│   └── HasRole(user, role) → 역할 보유 확인
│
├── 역할 관리
│   ├── RoleAdd, RoleDelete, RoleGet, RoleList
│   ├── RoleGrantPermission
│   └── RoleRevokePermission
│
├── 권한 검사
│   ├── IsPutPermitted(authInfo, key) → 쓰기 권한 확인
│   ├── IsRangePermitted(authInfo, key, rangeEnd) → 읽기 권한 확인
│   ├── IsDeleteRangePermitted(authInfo, key, rangeEnd) → 삭제 권한 확인
│   └── IsAdminPermitted(authInfo) → 관리자 권한 확인
│
├── 컨텍스트 처리
│   ├── AuthInfoFromCtx(ctx) → gRPC 메타데이터에서 토큰 추출
│   ├── AuthInfoFromTLS(ctx) → TLS 인증서에서 정보 추출
│   └── WithRoot(ctx) → root 토큰이 포함된 컨텍스트 생성
│
├── 상태
│   ├── Revision() → 현재 인증 리비전
│   ├── Recover(be) → 백엔드에서 복원
│   ├── Close() → 정리
│   └── BcryptCost() → bcrypt 비용 조회
```

---

## authStore 구조체

```go
type authStore struct {
    revision       uint64              // 원자적 인증 리비전 (64비트 정렬 필수)
    lg             *zap.Logger
    be             AuthBackend          // 백엔드 저장소
    enabled        bool                 // 인증 활성 상태
    enabledMu      sync.RWMutex         // enabled 보호
    rangePermCache map[string]*unifiedRangePermissions  // 사용자별 권한 캐시
    rangePermCacheMu sync.RWMutex       // 캐시 보호
    tokenProvider  TokenProvider         // 토큰 관리자
    bcryptCost     int                   // bcrypt 해싱 비용
}
```

**왜 revision 필드가 구조체 최상단에 있는가?**

주석에 명시:
> atomic operations; need 64-bit align, or 32-bit tests will crash

Go에서 `atomic.AddUint64`와 `atomic.LoadUint64`는 64비트 정렬이 필요하다. 32비트 시스템에서 구조체 필드가 8바이트 경계에 정렬되지 않으면 패닉이 발생한다. 구조체의 첫 번째 필드는 항상 자연 정렬되므로 최상단에 배치한다.

---

## AuthEnable(): 인증 활성화

```
AuthEnable() 흐름:
1. enabledMu.Lock()
2. 이미 활성화되어 있으면 → 무시 후 반환
3. BatchTx().Lock()
4. root 사용자 존재 확인 → 없으면 ErrRootUserNotExist
5. root 사용자에게 root 역할 확인 → 없으면 ErrRootRoleNotExist
6. UnsafeSaveAuthEnabled(true) → 백엔드에 활성 상태 저장
7. as.enabled = true
8. tokenProvider.enable() → 토큰 시스템 활성화
9. refreshRangePermCache(tx) → 권한 캐시 재구축
10. setRevision(tx.UnsafeReadAuthRevision())
11. tx.Unlock()
12. be.ForceCommit()
```

**왜 root 사용자와 root 역할이 반드시 필요한가?**

인증이 활성화되면 모든 요청에 인증이 필요하다. root 사용자/역할이 없으면 누구도 시스템을 관리할 수 없게 된다. 따라서 인증 활성화 전에 반드시 root 사용자가 root 역할을 보유하고 있어야 한다.

---

## AuthDisable(): 인증 비활성화

```
AuthDisable() 흐름:
1. enabledMu.Lock()
2. 이미 비활성화되어 있으면 → 무시 후 반환
3. BatchTx().Lock()
4. UnsafeSaveAuthEnabled(false) → 백엔드에 비활성 상태 저장
5. commitRevision(tx) → 인증 리비전 증가
6. tx.Unlock()
7. be.ForceCommit()
8. as.enabled = false
9. tokenProvider.disable() → 모든 토큰 무효화
```

**왜 비활성화 시 인증 리비전을 증가시키는가?**

인증 리비전은 인증 설정이 변경될 때마다 증가한다. 클라이언트가 이전 리비전의 토큰으로 요청하면, 리비전이 현재보다 작아 `ErrAuthOldRevision`으로 거부된다. 이를 통해 인증 설정 변경 후 이전 토큰이 더 이상 유효하지 않음을 보장한다.

---

## Authenticate(): 비밀번호 검증 → 토큰 발급

인증 흐름은 두 단계로 분리되어 있다.

### 1단계: CheckPassword() - 비밀번호 검증

```
CheckPassword(username, password) 흐름:
1. IsAuthEnabled() 확인
2. 클로저 내에서:
   ├── ReadTx().RLock() → 읽기 트랜잭션
   ├── UnsafeGetUser(username) → 사용자 조회
   ├── user == nil → ErrAuthFailed
   ├── NoPassword 옵션 → ErrNoPasswordUser
   ├── UnsafeReadAuthRevision() → 리비전 획득
   └── tx.RUnlock()
3. bcrypt.CompareHashAndPassword(user.Password, password)
   └── 불일치 → ErrAuthFailed
4. (revision, nil) 반환
```

**왜 bcrypt 비교를 트랜잭션 밖에서 수행하는가?**

소스코드 주석:
> CompareHashAndPassword is very expensive, so we use closures to avoid putting it in the critical section of the tx lock.

bcrypt는 의도적으로 느린 해싱 알고리즘이다. 트랜잭션 잠금을 잡은 상태에서 수행하면 다른 모든 읽기/쓰기가 블로킹된다. 사용자 데이터를 빠르게 읽고 잠금을 해제한 후, 잠금 밖에서 느린 해싱 비교를 수행한다.

### 2단계: Authenticate() - 토큰 발급

```
Authenticate(ctx, username, password) 흐름:
1. IsAuthEnabled() 확인 → 비활성화면 ErrAuthNotEnabled
2. be.GetUser(username) → 사용자 존재 확인
3. NoPassword 옵션 확인 → ErrAuthFailed
4. tokenProvider.assign(ctx, username, Revision()) → 토큰 생성
5. AuthenticateResponse{Token: token} 반환
```

**왜 Authenticate에서 비밀번호를 재확인하지 않는가?**

소스코드 주석:
> Password checking is already performed in the API layer, so we don't need to check for now. Staleness of password can be detected with OCC in the API layer, too.

비밀번호 검증은 API 계층(etcdserver)에서 이미 수행된다. authStore의 Authenticate는 검증된 사용자에 대한 토큰 발급만 담당한다.

---

## IsPutPermitted(), IsRangePermitted(): 권한 검사

### isOpPermitted(): 공통 권한 검사 로직

```
isOpPermitted(userName, revision, key, rangeEnd, permTyp) 흐름:
1. IsAuthEnabled() → 비활성화면 허용
2. revision == 0 → ErrUserEmpty (인증 정보 없음)
3. revision < as.Revision() → ErrAuthOldRevision
   └── 인증 설정이 변경된 후의 오래된 토큰
4. ReadTx().RLock()
5. UnsafeGetUser(userName) → nil이면 ErrPermissionDenied
6. hasRootRole(user) → root 역할이면 무조건 허용
7. isRangeOpPermitted(userName, key, rangeEnd, permtyp)
   └── rangePermCache에서 캐시된 권한으로 확인
8. 모두 실패 → ErrPermissionDenied
```

### 권한 검사 위임 관계

```go
func (as *authStore) IsPutPermitted(authInfo *AuthInfo, key []byte) error {
    return as.isOpPermitted(authInfo.Username, authInfo.Revision, key, nil, authpb.Permission_WRITE)
}

func (as *authStore) IsRangePermitted(authInfo *AuthInfo, key, rangeEnd []byte) error {
    return as.isOpPermitted(authInfo.Username, authInfo.Revision, key, rangeEnd, authpb.Permission_READ)
}

func (as *authStore) IsDeleteRangePermitted(authInfo *AuthInfo, key, rangeEnd []byte) error {
    return as.isOpPermitted(authInfo.Username, authInfo.Revision, key, rangeEnd, authpb.Permission_WRITE)
}
```

| 연산 | 필요 권한 | 범위 |
|------|----------|------|
| Put | WRITE | 단일 키 (rangeEnd=nil) |
| Range | READ | 키 또는 범위 |
| DeleteRange | WRITE | 키 또는 범위 |
| Admin 연산 | root 역할 | - |

### IsAdminPermitted(): 관리자 권한

```go
func (as *authStore) IsAdminPermitted(authInfo *AuthInfo) error {
    if !as.IsAuthEnabled() {
        return nil
    }
    if authInfo == nil || authInfo.Username == "" {
        return ErrUserEmpty
    }
    // ReadTx에서 사용자 조회
    u := tx.UnsafeGetUser(authInfo.Username)
    if u == nil {
        return ErrUserNotFound
    }
    if !hasRootRole(u) {
        return ErrPermissionDenied
    }
    return nil
}
```

관리자 권한은 `root` 역할을 가진 사용자에게만 부여된다. 사용자/역할 관리, 인증 활성화/비활성화 등의 관리 연산에 필요하다.

---

## rangePermCache: 범위 권한 캐시

### unifiedRangePermissions 구조체

```go
type unifiedRangePermissions struct {
    readPerms  adt.IntervalTree  // 읽기 권한 구간 트리
    writePerms adt.IntervalTree  // 쓰기 권한 구간 트리
}
```

**왜 IntervalTree를 사용하는가?**

권한은 키 범위로 정의된다. 특정 키(또는 범위)에 대한 권한을 확인하려면 해당 키가 어떤 권한 범위에 포함되는지 확인해야 한다. IntervalTree는 구간 포함 관계를 효율적으로 판단한다.

### rangePermCache 맵

```go
rangePermCache map[string]*unifiedRangePermissions  // username → permissions
```

### 캐시 구축: refreshRangePermCache()

```
refreshRangePermCache(tx) 흐름:
1. rangePermCacheMu.Lock() → 쓰기 잠금
2. rangePermCache = make(새 맵) → 전체 무효화
3. 모든 사용자 순회:
   └── getMergedPerms(tx, userName):
       ├── 사용자의 모든 역할 순회
       ├── 각 역할의 모든 권한 순회
       ├── Permission.Type별 IntervalTree에 삽입:
       │   ├── READ  → readPerms에 삽입
       │   ├── WRITE → writePerms에 삽입
       │   └── READWRITE → readPerms, writePerms 양쪽에 삽입
       └── unifiedRangePermissions 반환
4. rangePermCache[userName] = perms
5. rangePermCacheMu.Unlock()
```

**왜 전체를 무효화하고 재구축하는가?**

소스코드 주석:
> Note that every authentication configuration update calls this method and it invalidates the entire rangePermCache and reconstruct it based on information of users and roles stored in the backend. This can be a costly operation.

부분 업데이트는 역할 변경이 여러 사용자에게 영향을 미치는 복잡한 의존성 때문에 구현이 어렵다. 전체 재구축이 단순하고 정확하다. 인증 설정 변경은 자주 발생하지 않으므로 이 비용은 수용 가능하다.

### 캐시 잠금 분리

```go
// rangePermCache needs to be protected by rangePermCacheMu
// rangePermCacheMu needs to be write locked only in initialization phase or configuration changes
// Hot paths like Range(), needs to acquire read lock for improving performance
//
// Note that BatchTx and ReadTx cannot be a mutex for rangePermCache because they are independent resources
```

`rangePermCacheMu`는 캐시 전용 뮤텍스이다. 핫 패스(Range, Put 등의 권한 검사)에서는 읽기 잠금만 필요하고, 설정 변경 시에만 쓰기 잠금이 필요하다. 백엔드 트랜잭션 잠금과 분리하여 성능을 최적화한다.

### 권한 확인 경로

```
isRangeOpPermitted(userName, key, rangeEnd, permtyp) 흐름:
1. rangePermCacheMu.RLock() → 읽기 잠금
2. rangePermCache[userName] 조회
   └── 없으면 false 반환
3. rangeEnd가 비어있으면 (단일 키):
   └── checkKeyPoint(rangePerm, key, permtyp)
       └── IntervalTree.Intersects(point) → 포함 여부
4. rangeEnd가 있으면 (범위):
   └── checkKeyInterval(rangePerm, key, rangeEnd, permtyp)
       └── IntervalTree.Contains(interval) → 포함 여부
5. rangePermCacheMu.RUnlock()
```

**Intersects vs Contains의 차이:**

- `Intersects(point)`: 특정 점이 어떤 구간과 겹치는지 확인 (단일 키 권한)
- `Contains(interval)`: 특정 구간이 어떤 구간에 완전히 포함되는지 확인 (범위 권한)

범위 요청(Range)은 요청된 전체 범위가 권한 범위 내에 있어야 한다. 부분 겹침으로는 권한이 부여되지 않는다.

---

## TokenProvider: Simple Token vs JWT

### TokenProvider 인터페이스

```go
type TokenProvider interface {
    info(ctx context.Context, token string, revision uint64) (*AuthInfo, bool)
    assign(ctx context.Context, username string, revision uint64) (string, error)
    enable()
    disable()
    invalidateUser(string)
    genTokenPrefix() (string, error)
}
```

| 메서드 | 설명 |
|--------|------|
| info | 토큰에서 인증 정보 추출 |
| assign | 사용자에게 토큰 발급 |
| enable | 토큰 시스템 활성화 |
| disable | 토큰 시스템 비활성화, 모든 토큰 무효화 |
| invalidateUser | 특정 사용자의 토큰 무효화 |
| genTokenPrefix | 토큰 접두사 생성 (Simple용) |

### NewTokenProvider(): 토큰 제공자 생성

```go
func NewTokenProvider(lg *zap.Logger, tokenOpts string, indexWaiter func(uint64) <-chan struct{}, TokenTTL time.Duration) (TokenProvider, error) {
    tokenType, typeSpecificOpts, err := decomposeOpts(lg, tokenOpts)
    switch tokenType {
    case "simple":
        return newTokenProviderSimple(lg, indexWaiter, TokenTTL), nil
    case "jwt":
        return newTokenProviderJWT(lg, typeSpecificOpts)
    case "":
        return newTokenProviderNop()
    default:
        return nil, ErrInvalidAuthOpts
    }
}
```

### Simple Token: server/auth/simple_token.go

Simple Token은 서버 측에서 랜덤 문자열을 생성하고 메모리에 저장하는 방식이다.

#### tokenSimple 구조체

```go
type tokenSimple struct {
    lg                *zap.Logger
    indexWaiter       func(uint64) <-chan struct{}
    simpleTokenKeeper *simpleTokenTTLKeeper
    simpleTokensMu    sync.Mutex
    simpleTokens      map[string]string  // token → username
    simpleTokenTTL    time.Duration      // 기본 300초
}
```

#### 토큰 생성 (assign)

```go
func (t *tokenSimple) assign(ctx context.Context, username string, rev uint64) (string, error) {
    index := ctx.Value(AuthenticateParamIndex{}).(uint64)
    simpleTokenPrefix := ctx.Value(AuthenticateParamSimpleTokenPrefix{}).(string)
    token := fmt.Sprintf("%s.%d", simpleTokenPrefix, index)
    t.assignSimpleTokenToUser(username, token)
    return token, nil
}
```

토큰 형식: `{16자 랜덤 접두사}.{RAFT 인덱스}`

**왜 RAFT 인덱스를 포함하는가?**

`isValidSimpleToken()`에서 `indexWaiter(index)`를 사용하여 해당 인덱스가 적용될 때까지 대기한다. 이는 인증 요청이 RAFT를 통해 합의된 후에만 토큰이 유효해지도록 보장한다.

```go
func (t *tokenSimple) isValidSimpleToken(ctx context.Context, token string) bool {
    splitted := strings.Split(token, ".")
    index, err := strconv.ParseUint(splitted[1], 10, 0)
    select {
    case <-t.indexWaiter(index):
        return true
    case <-ctx.Done():
    }
    return false
}
```

#### simpleTokenTTLKeeper: TTL 관리

```go
type simpleTokenTTLKeeper struct {
    tokens          map[string]time.Time  // token → 만료 시각
    donec           chan struct{}
    stopc           chan struct{}
    deleteTokenFunc func(string)
    mu              *sync.Mutex
    simpleTokenTTL  time.Duration         // 기본 300초
}
```

1초 간격으로 만료된 토큰을 삭제하는 백그라운드 고루틴을 운영한다:

```go
func (tm *simpleTokenTTLKeeper) run() {
    tokenTicker := time.NewTicker(simpleTokenTTLResolution)  // 1초
    for {
        select {
        case <-tokenTicker.C:
            for t, tokenendtime := range tm.tokens {
                if nowtime.After(tokenendtime) {
                    tm.deleteTokenFunc(t)
                    delete(tm.tokens, t)
                }
            }
        case <-tm.stopc:
            return
        }
    }
}
```

#### info(): 토큰 검증

```go
func (t *tokenSimple) info(ctx context.Context, token string, revision uint64) (*AuthInfo, bool) {
    if !t.isValidSimpleToken(ctx, token) {
        return nil, false
    }
    t.simpleTokensMu.Lock()
    username, ok := t.simpleTokens[token]
    if ok && t.simpleTokenKeeper != nil {
        t.simpleTokenKeeper.resetSimpleToken(token)  // TTL 갱신
    }
    t.simpleTokensMu.Unlock()
    return &AuthInfo{Username: username, Revision: revision}, ok
}
```

Simple Token은 사용될 때마다 TTL이 리셋된다 (`resetSimpleToken`). 이는 활발히 사용되는 토큰이 만료되지 않도록 한다.

#### invalidateUser(): 사용자 토큰 무효화

```go
func (t *tokenSimple) invalidateUser(username string) {
    t.simpleTokensMu.Lock()
    for token, name := range t.simpleTokens {
        if name == username {
            delete(t.simpleTokens, token)
            t.simpleTokenKeeper.deleteSimpleToken(token)
        }
    }
    t.simpleTokensMu.Unlock()
}
```

사용자 삭제나 비밀번호 변경 시 해당 사용자의 모든 토큰을 무효화한다.

### JWT Token: server/auth/jwt.go

JWT는 서명된 토큰으로, 서버가 상태를 유지하지 않아도 검증할 수 있다.

#### tokenJWT 구조체

```go
type tokenJWT struct {
    lg         *zap.Logger
    signMethod jwt.SigningMethod   // ECDSA, RSA, Ed25519 등
    key        any                  // 서명 키
    ttl        time.Duration       // 토큰 유효 기간
    verifyOnly bool                // 공개 키만 있는 경우
}
```

#### 토큰 발급 (assign)

```go
func (t *tokenJWT) assign(ctx context.Context, username string, revision uint64) (string, error) {
    if t.verifyOnly {
        return "", ErrVerifyOnly  // 공개 키만으로는 서명 불가
    }
    tk := jwt.NewWithClaims(t.signMethod, jwt.MapClaims{
        "username": username,
        "revision": revision,
        "exp":      time.Now().Add(t.ttl).Unix(),
    })
    token, err := tk.SignedString(t.key)
    return token, err
}
```

JWT 클레임에는 `username`, `revision`, `exp`(만료 시각)이 포함된다.

#### 토큰 검증 (info)

```go
func (t *tokenJWT) info(ctx context.Context, token string, rev uint64) (*AuthInfo, bool) {
    parsed, err := jwt.Parse(token, func(token *jwt.Token) (any, error) {
        if token.Method.Alg() != t.signMethod.Alg() {
            return nil, errors.New("invalid signing method")
        }
        switch k := t.key.(type) {
        case *rsa.PrivateKey:
            return &k.PublicKey, nil
        case *ecdsa.PrivateKey:
            return &k.PublicKey, nil
        case ed25519.PrivateKey:
            return k.Public(), nil
        default:
            return t.key, nil
        }
    })
    // 클레임에서 username, revision 추출
    return &AuthInfo{Username: username, Revision: uint64(revision)}, true
}
```

**JWT에서 revision 파라미터를 무시하는 이유:**

Simple Token은 서버 측 상태(map)에 리비전을 저장하므로 `info()` 호출 시 외부에서 전달된 revision을 사용한다. JWT는 토큰 자체에 리비전이 포함되어 있으므로 파라미터를 무시하고 토큰 내의 값을 사용한다.

#### JWT에서 무시되는 메서드들

```go
func (t *tokenJWT) enable()                         {}
func (t *tokenJWT) disable()                        {}
func (t *tokenJWT) invalidateUser(string)           {}
func (t *tokenJWT) genTokenPrefix() (string, error) { return "", nil }
```

JWT는 상태가 없으므로 enable/disable과 invalidateUser가 no-op이다. 토큰 무효화는 서명 키 교체나 리비전 검사(`ErrAuthOldRevision`)로 간접적으로 수행된다.

### Simple Token vs JWT 비교

```
┌─────────────────┬──────────────────────┬──────────────────────┐
│ 특성             │ Simple Token          │ JWT                  │
├─────────────────┼──────────────────────┼──────────────────────┤
│ 상태             │ 서버 측 상태 필요     │ 상태 없음 (stateless)│
│ 저장소           │ 메모리 맵             │ 없음                 │
│ TTL 관리         │ 서버 측 타이머        │ 토큰 내 exp 클레임   │
│ 사용자 무효화    │ 즉시 가능             │ 불가 (만료 대기)     │
│ 확장성           │ 단일 노드             │ 멀티 노드            │
│ 보안             │ 암호학적 서명 없음    │ 서명된 토큰          │
│ 사용 시 TTL 리셋│ 예                    │ 아니오               │
│ 노드 재시작 시   │ 모든 토큰 소멸        │ 토큰 유효 유지       │
│ 기본 TTL         │ 300초                │ 설정에 따라 다름     │
└─────────────────┴──────────────────────┴──────────────────────┘
```

소스코드에서 Simple Token에 대한 경고:
> simple token is not cryptographically signed

프로덕션에서는 JWT를 권장한다.

---

## AuthInfoFromCtx(): gRPC 메타데이터에서 토큰 추출

```go
func (as *authStore) AuthInfoFromCtx(ctx context.Context) (*AuthInfo, error) {
    if !as.IsAuthEnabled() {
        return nil, nil  // 인증 비활성화 시 nil 반환
    }

    md, ok := metadata.FromIncomingContext(ctx)
    if !ok {
        return nil, nil
    }

    // 두 가지 토큰 필드 확인
    ts, ok := md[rpctypes.TokenFieldNameGRPC]     // "token"
    if !ok {
        ts, ok = md[rpctypes.TokenFieldNameSwagger]  // "authorization"
    }
    if !ok {
        return nil, nil
    }

    token := ts[0]
    authInfo, uok := as.authInfoFromToken(ctx, token)
    if !uok {
        return nil, ErrInvalidAuthToken
    }
    return authInfo, nil
}
```

gRPC 메타데이터(HTTP/2 헤더)에서 토큰을 추출한다. `TokenFieldNameGRPC`("token")과 `TokenFieldNameSwagger`("authorization") 두 가지 필드를 순서대로 확인한다.

### AuthInfoFromTLS(): TLS 인증서 기반 인증

```go
func (as *authStore) AuthInfoFromTLS(ctx context.Context) (ai *AuthInfo) {
    peer, ok := peer.FromContext(ctx)
    // TLS 인증서의 CommonName을 username으로 사용
    for _, chains := range tlsInfo.State.VerifiedChains {
        ai = &AuthInfo{
            Username: chains[0].Subject.CommonName,
            Revision: as.Revision(),
        }
        // gRPC-gateway 프록시 요청 감지 및 무시
        if gw := md["grpcgateway-accept"]; len(gw) > 0 {
            return nil
        }
    }
    return ai
}
```

TLS 클라이언트 인증서의 CommonName(CN)을 사용자 이름으로 사용한다. gRPC-gateway 프록시 요청은 서버 인증서의 CN을 사용하므로 이를 무시한다.

---

## AuthInfo 구조체

```go
type AuthInfo struct {
    Username string   // 인증된 사용자 이름
    Revision uint64   // 인증 시점의 auth 리비전
}
```

`Revision` 필드는 토큰 발급 시점의 인증 리비전을 기록한다. 이후 인증 설정이 변경되면(리비전 증가), 이전 리비전의 토큰은 `ErrAuthOldRevision`으로 거부된다.

---

## bcrypt 비밀번호 해싱

### 해시 생성

```go
func (as *authStore) selectPassword(password string, hashedPassword string) ([]byte, error) {
    if password != "" && hashedPassword == "" {
        // 이전 버전(3.5 미만) 호환: 평문 비밀번호를 bcrypt로 해싱
        return bcrypt.GenerateFromPassword([]byte(password), as.bcryptCost)
    }
    // 최신 버전: 클라이언트가 이미 해시한 비밀번호
    return base64.StdEncoding.DecodeString(hashedPassword)
}
```

v3.5부터 클라이언트가 bcrypt 해시를 미리 계산하여 `hashedPassword` 필드로 전송한다. 이전 버전과의 호환을 위해 평문 비밀번호도 지원한다.

### 비용 설정

```go
func NewAuthStore(lg *zap.Logger, be AuthBackend, tp TokenProvider, bcryptCost int) AuthStore {
    if bcryptCost < bcrypt.MinCost || bcryptCost > bcrypt.MaxCost {
        bcryptCost = bcrypt.DefaultCost  // 기본값 10
    }
    // ...
}
```

bcrypt 비용은 `MinCost`(4)에서 `MaxCost`(31) 사이여야 한다. 높을수록 해싱이 느려지지만 보안이 강화된다.

### 비밀번호 검증

```go
if bcrypt.CompareHashAndPassword(user.Password, []byte(password)) != nil {
    as.lg.Info("invalid password", zap.String("user-name", username))
    return 0, ErrAuthFailed
}
```

---

## WithRoot(): 내부 root 인증 컨텍스트

```go
func (as *authStore) WithRoot(ctx context.Context) context.Context {
    if !as.IsAuthEnabled() {
        return ctx
    }
    // Simple Token인 경우 특별 처리
    if ts, ok := as.tokenProvider.(*tokenSimple); ok && ts != nil {
        ctx1 := context.WithValue(ctx, AuthenticateParamIndex{}, uint64(0))
        prefix, _ := ts.genTokenPrefix()
        ctxForAssign = context.WithValue(ctx1, AuthenticateParamSimpleTokenPrefix{}, prefix)
    }
    // root 토큰 발급
    token, _ := as.tokenProvider.assign(ctxForAssign, "root", as.Revision())
    // gRPC 메타데이터에 토큰 삽입
    mdMap := map[string]string{rpctypes.TokenFieldNameGRPC: token}
    tokenMD := metadata.New(mdMap)
    return metadata.NewIncomingContext(ctx, tokenMD)
}
```

**왜 필요한가?**

Lease 만료 시 키 삭제 등 서버 내부 작업도 인증이 필요하다. `WithRoot()`는 root 권한의 토큰을 포함한 컨텍스트를 생성하여, 내부 작업이 인증 검사를 통과할 수 있게 한다.

---

## 인증 리비전 관리

```go
func (as *authStore) commitRevision(tx UnsafeAuthWriter) {
    atomic.AddUint64(&as.revision, 1)
    tx.UnsafeSaveAuthRevision(as.Revision())
}

func (as *authStore) setRevision(rev uint64) {
    atomic.StoreUint64(&as.revision, rev)
}

func (as *authStore) Revision() uint64 {
    return atomic.LoadUint64(&as.revision)
}
```

인증 리비전은 다음 상황에서 증가한다:
- AuthEnable/AuthDisable
- UserAdd/UserDelete/UserChangePassword
- UserGrantRole/UserRevokeRole
- RoleAdd/RoleDelete
- RoleGrantPermission/RoleRevokePermission

모든 인증 설정 변경은 리비전을 증가시키고, 이전 리비전의 토큰을 간접적으로 무효화한다.

---

## Recover(): 백엔드에서 인증 상태 복원

```go
func (as *authStore) Recover(be AuthBackend) {
    as.be = be
    tx := be.ReadTx()
    tx.RLock()
    enabled := tx.UnsafeReadAuthEnabled()
    as.setRevision(tx.UnsafeReadAuthRevision())
    as.refreshRangePermCache(tx)
    tx.RUnlock()

    as.enabledMu.Lock()
    as.enabled = enabled
    if enabled {
        as.tokenProvider.enable()
    }
    as.enabledMu.Unlock()
}
```

서버 재시작이나 스냅샷 복원 시 호출된다. 백엔드에서 인증 활성 상태, 리비전, 사용자/역할 정보를 읽어 rangePermCache를 재구축한다.

---

## 전체 인증 흐름 다이어그램

```
┌──────────────────────────────────────────────────────────────┐
│                      인증 흐름                                │
│                                                              │
│  1. 인증 (Authenticate)                                      │
│  ┌─────────┐  username/password  ┌──────────────┐            │
│  │클라이언트│──────────────────►│ etcdserver    │            │
│  └────┬────┘                    │ CheckPassword │            │
│       │                         │ (bcrypt 검증) │            │
│       │                         └──────┬───────┘            │
│       │                                │                     │
│       │                         ┌──────▼───────┐            │
│       │                         │ authStore     │            │
│       │   ◄── token ───────────│ Authenticate  │            │
│       │                         │ (토큰 발급)   │            │
│       │                         └──────────────┘            │
│                                                              │
│  2. 인가된 요청                                               │
│  ┌─────────┐  token + request   ┌──────────────┐            │
│  │클라이언트│──────────────────►│ gRPC 인터셉터 │            │
│  └────┬────┘                    └──────┬───────┘            │
│       │                                │                     │
│       │                         ┌──────▼───────┐            │
│       │                         │AuthInfoFromCtx│            │
│       │                         │(메타데이터에서 │            │
│       │                         │ 토큰 추출)    │            │
│       │                         └──────┬───────┘            │
│       │                                │                     │
│       │                         ┌──────▼───────┐            │
│       │                         │ TokenProvider │            │
│       │                         │ .info()       │            │
│       │                         │ (토큰→AuthInfo)│            │
│       │                         └──────┬───────┘            │
│       │                                │                     │
│       │                         ┌──────▼───────┐            │
│       │                         │IsPutPermitted│            │
│       │                         │IsRangePermitted│           │
│       │                         │(rangePermCache)│           │
│       │                         └──────┬───────┘            │
│       │                                │                     │
│       │                         ┌──────▼───────┐            │
│       │   ◄── response ────────│ etcdserver    │            │
│       │                         │ (요청 처리)   │            │
│       │                         └──────────────┘            │
└──────────────────────────────────────────────────────────────┘
```

---

## 소스 파일 참조

| 파일 경로 | 핵심 내용 |
|----------|----------|
| `server/auth/store.go` | AuthStore 인터페이스, authStore 구현, 사용자/역할 관리 |
| `server/auth/range_perm_cache.go` | rangePermCache, unifiedRangePermissions, IntervalTree 기반 권한 확인 |
| `server/auth/jwt.go` | tokenJWT, JWT 토큰 생성/검증 |
| `server/auth/simple_token.go` | tokenSimple, simpleTokenTTLKeeper, 랜덤 토큰 관리 |
| `api/authpb/auth.proto` | User, Role, Permission protobuf 정의 |
| `server/etcdserver/api/v3rpc/auth.go` | AuthServer gRPC 핸들러, AuthGetter 인터페이스 |

---

## 설계 원칙 정리

1. **RBAC 모델**: User → Role → Permission 3계층 구조로 유연한 접근 제어
2. **캐시 우선 권한 검사**: rangePermCache로 핫 패스의 성능 최적화, 설정 변경 시에만 재구축
3. **리비전 기반 토큰 무효화**: 인증 설정 변경 시 리비전 증가로 이전 토큰 간접 무효화
4. **bcrypt 잠금 분리**: 비용이 큰 bcrypt 비교를 트랜잭션 잠금 밖에서 수행
5. **이중 토큰 방식**: Simple Token(개발/테스트)과 JWT(프로덕션) 선택 가능
6. **root 역할 보호**: root 사용자/역할 삭제 방지, 인증 활성화 전 필수 검증
7. **IntervalTree 기반 범위 권한**: 키 범위 권한을 효율적으로 판단
