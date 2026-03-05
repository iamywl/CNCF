# PoC 08 — RBAC 시스템

## 개요

Argo CD의 Casbin 기반 RBAC 시스템을 시뮬레이션한다.
실제 소스: `util/rbac/rbac.go`, `assets/model.conf`, `assets/builtin-policy.csv`

## 핵심 개념

### Casbin 모델

```
assets/model.conf
```

```ini
[request_definition]
r = sub, res, act, obj

[policy_definition]
p = sub, res, act, obj, eft

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow)) && !some(where (p.eft == deny))

[matchers]
m = g(r.sub, p.sub) && globOrRegexMatch(r.res, p.res) && globOrRegexMatch(r.act, p.act) && globOrRegexMatch(r.obj, p.obj)
```

| 필드 | 의미 |
|------|------|
| `sub` | 주체 (사용자, 그룹, 역할) |
| `res` | 리소스 (applications, clusters, ...) |
| `act` | 액션 (get, create, delete, sync, ...) |
| `obj` | 오브젝트 (project/name 또는 *) |
| `eft` | 효과 (allow / deny) |

### 정책 형식

```
# 프로젝트 범위 리소스 (applications, applicationsets, logs, exec, clusters, repositories)
p, <sub>, <resource>, <action>, <project>/<object>, <allow/deny>

# 글로벌 리소스 (certificates, gpgkeys, accounts, projects)
p, <sub>, <resource>, <action>, <object>, <allow/deny>

# 역할 상속
g, <sub>, <role>
```

### 내장 역할

```
assets/builtin-policy.csv
```

**role:readonly** — get 전용:
```
p, role:readonly, applications, get, */*, allow
p, role:readonly, clusters, get, *, allow
p, role:readonly, repositories, get, *, allow
p, role:readonly, projects, get, *, allow
# ... (모든 리소스 get)
```

**role:admin** — 전체 권한:
```
p, role:admin, applications, create, */*, allow
p, role:admin, applications, sync, */*, allow
p, role:admin, clusters, delete, *, allow
p, role:admin, exec, create, */*, allow
# ... (모든 리소스 모든 액션)

g, role:admin, role:readonly   # readonly 상속
g, admin, role:admin           # admin 사용자
```

### ProjectScoped 리소스

```
util/rbac/rbac.go:112-119
```

| 리소스 | 프로젝트 범위 |
|--------|------------|
| applications | O (`project/name`) |
| applicationsets | O |
| logs | O |
| exec | O |
| clusters | O |
| repositories | O |
| certificates | X (`name` 만) |
| accounts | X |
| projects | X |
| gpgkeys | X |

### enforce() 알고리즘

```
util/rbac/rbac.go:380-407
```

```
1. defaultRole 체크 — 설정된 경우, defaultRole로 먼저 허용 확인
2. casbin 평가:
   a. sub의 모든 역할(g 상속 포함) 수집
   b. 각 정책 p에 대해: roles에 p.sub 포함 && glob(res) && glob(act) && glob(obj)
   c. 결과: some(allow) && !some(deny)
```

### Glob 매칭

```
util/rbac/rbac.go:282-298  globMatchFunc()
util/glob/glob.go
```

| 패턴 | 예시 | 매칭 |
|------|------|------|
| `*` | `*` | 모든 단일 값 |
| `*/*` | `myproject/myapp` | 모든 프로젝트/앱 |
| `myproject/*` | `myproject/myapp` | myproject의 모든 앱 |
| `team-?/*` | `team-a/myapp` | team- + 단일문자 |
| `[abc]*/*` | `admin/myapp` | a, b, c로 시작하는 프로젝트 |

### defaultRole

```
util/rbac/rbac.go:311-315  SetDefaultRole()
util/rbac/rbac.go:383-387  enforce()에서 defaultRole 체크
```

ConfigMap의 `policy.default` 키로 설정:
```yaml
# argocd-rbac-cm
data:
  policy.default: role:readonly
```

모든 인증 사용자에게 기본 역할을 부여한다. 특정 정책이 없어도 기본 접근 가능.

### 프로젝트 런타임 정책

```
util/rbac/rbac.go:363-366  EnforceRuntimePolicy()
```

`AppProject.spec.roles`로 프로젝트별 RBAC를 정의한다:
- 전역 정책의 deny는 프로젝트 정책보다 우선
- 프로젝트 정책은 전역 정책을 보완 (추가 허용)

### 캐시

```
util/rbac/rbac.go:127-146  Enforcer.enforcerCache
```

- `gocache.New(time.Hour, time.Hour)` — 1시간 TTL
- 정책 변경 시 `cache.Flush()` 호출
- project별로 별도 캐시 엔트리 (`project` 키)

## 실행

```bash
go run main.go
```

## 시나리오

| 시나리오 | 내용 |
|----------|------|
| 1 | role:readonly: get 허용, 쓰기 거부 |
| 2 | role:admin: 전체 권한 (readonly 상속 포함) |
| 3 | admin 사용자 (g, admin, role:admin) |
| 4 | defaultRole: 모든 사용자 기본 role:readonly |
| 5 | 사용자 정의 정책 (팀별 역할, 직접 정책) |
| 6 | Glob 패턴 매칭 (\*, ?, [...]) |
| 7 | Deny 정책 (allow + deny 동시 적용) |
| 8 | 프로젝트 런타임 정책 |
| 9 | EnforceErr 상세 에러 메시지 |

## 실제 코드와의 대응

| 시뮬레이션 | 실제 소스 |
|-----------|-----------|
| Casbin 모델 | `assets/model.conf` |
| 내장 정책 | `assets/builtin-policy.csv` |
| `Enforcer` struct | `util/rbac/rbac.go:121` |
| `enforce()` | `util/rbac/rbac.go:380` |
| `globMatchFunc()` | `util/rbac/rbac.go:282` |
| `SetDefaultRole()` | `util/rbac/rbac.go:311` |
| `EnforceRuntimePolicy()` | `util/rbac/rbac.go:363` |
| `EnforceErr()` | `util/rbac/rbac.go:331` |
| `ProjectScoped` 맵 | `util/rbac/rbac.go:112` |
| Resource/Action 상수 | `util/rbac/rbac.go:58` |
