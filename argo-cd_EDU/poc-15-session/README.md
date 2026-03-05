# PoC 15: 세션 관리 (Session Management)

## 개요

Argo CD의 세션 관리 시스템(`server/session/sessionmanager.go`)을 Go 표준 라이브러리만으로 시뮬레이션합니다.

HS256 JWT 생성/검증, 로그인 보안(타이밍 공격 방지, 실패 횟수 제한), 토큰 취소, 자동 갱신, OIDC 디스패치 전체 파이프라인을 구현합니다.

## 실행

```bash
go run main.go
```

## 핵심 개념

### 1. JWT 구조 (HS256)

```
Header.Payload.Signature

Header:  {"alg":"HS256","typ":"JWT"}
Payload: {
  "iss": "argocd",         // 발급자 (issuer)
  "sub": "admin",          // 사용자명 (subject)
  "iat": 1709123456,       // 발급 시각
  "exp": 1709209856,       // 만료 시각 (iat + 24h)
  "jti": "uuid-v4...",     // 고유 ID (dedup, revocation용)
  "argocd.io/auth": true   // 승인 여부
}
Signature: HMAC-SHA256(Header.Payload, serverSecret)
```

### 2. Parse() 검증 순서

```
JWT 토큰 문자열
        │
        ▼
① 헤더 파싱 → alg == "HS256" 확인 (HMAC만 허용)
        │
        ▼
② 서명 검증 (HMAC-SHA256)
        │
        ▼
③ 클레임 파싱
        │
        ▼
④ 만료 확인 (exp > now)
        │
        ▼
⑤ 계정 활성화 확인 (account.Enabled)
        │
        ▼
⑥ PasswordMtime vs IssuedAt 비교
   (PasswordMtime > IssuedAt → 무효화)
        │
        ▼
⑦ 토큰 취소 목록 확인 (JTI 기반)
        │
        ▼
⑧ 자동 갱신 (remaining < 5분)
        │
        ▼
ParseResult{Claims, NewToken, Renewed}
```

### 3. 알고리즘 검증 보안

`alg: none` 공격 또는 RS256/HS256 혼용 공격을 방지하기 위해 **HS256만 허용**합니다.

```go
if header.Algorithm != "HS256" {
    return nil, fmt.Errorf("허용되지 않은 알고리즘: %s", header.Algorithm)
}
```

### 4. 비밀번호 변경 → 토큰 자동 무효화

```
비밀번호 변경 시 account.PasswordMtime = time.Now()

Parse() 시 확인:
  if account.PasswordMtime.After(issuedAt) → 토큰 무효
```

이 메커니즘으로 비밀번호 변경 시 기존 발급된 모든 토큰이 자동으로 무효화됩니다.

### 5. 자동 갱신 (Auto-Renewal)

```
남은 시간 < 5분 → 새 토큰 자동 발급

상수:
  tokenExpiry     = 24 * time.Hour
  autoRenewBefore = 5 * time.Minute
```

### 6. VerifyToken() — issuer 기반 디스패치

```go
switch issuer {
case "argocd":
    // 로컬 HS256 JWT 검증 (Parse() 호출)
    return sm.Parse(tokenString)
default:
    // OIDC 공급자 검증 (JWKS 엔드포인트 조회)
    return oidcProvider.VerifyOIDC(tokenString)
}
```

### 7. 로그인 보안 (VerifyUsernamePassword)

| 보안 기능 | 구현 내용 |
|-----------|-----------|
| 빈 비밀번호 거부 | `password == ""` → 즉시 오류 |
| 사용자명 길이 제한 | `len(username) > 32` → 거부 |
| 타이밍 노이즈 | 500~1000ms 랜덤 대기 (타이밍 공격 방지) |
| 존재하지 않는 계정 | 비밀번호 해시 수행 후 동일 오류 반환 |
| 실패 횟수 제한 | 5분 윈도우 내 5회 초과 시 잠금 |

```
상수:
  maxLoginFailures = 5
  failureWindow    = 300s (5분)
```

### 8. 타이밍 공격 방지

존재하지 않는 계정에 대해서도 비밀번호 해시를 수행하여 응답 시간을 일정하게 유지합니다.

```go
if !exists {
    _ = hashPassword(password)  // 일정한 응답 시간 유지
    sm.updateFailureCount(username)
    return fmt.Errorf("사용자명 또는 비밀번호가 잘못되었습니다")
}
```

## 시뮬레이션 시나리오

| 시나리오 | 내용 |
|----------|------|
| 1 | JWT 생성 및 VerifyToken() 검증 |
| 2 | 토큰 취소 (RevokeToken + JTI 블랙리스트) |
| 3 | 잘못된 알고리즘(RS256) 토큰 거부 |
| 4 | 비밀번호 변경으로 기존 토큰 무효화 |
| 5 | 자동 갱신 — 만료 3분 전 새 토큰 발급 |
| 6 | 로그인 보안 — 빈 비밀번호, 길이 초과, 실패 횟수 제한 |
| 7 | OIDC 토큰 — issuer 기반 외부 공급자 디스패치 |

## 실제 Argo CD 코드 참조

| 구성요소 | 소스 위치 |
|----------|-----------|
| SessionManager | `server/session/sessionmanager.go` |
| Create() | `server/session/sessionmanager.go:Create()` |
| Parse() | `server/session/sessionmanager.go:Parse()` |
| VerifyToken() | `server/session/sessionmanager.go:VerifyToken()` |
| VerifyUsernamePassword() | `server/session/sessionmanager.go:VerifyUsernamePassword()` |
| updateFailureCount() | `server/session/sessionmanager.go:updateFailureCount()` |
