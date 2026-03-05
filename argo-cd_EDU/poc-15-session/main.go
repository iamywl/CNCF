// poc-15-session/main.go
//
// Argo CD 세션 관리 시뮬레이션
//
// 핵심 개념:
//   - SessionManager: JWT 생성 및 검증
//   - Create(): HS256 JWT (Claims: iss, sub, iat, exp, jti)
//   - Parse(): 알고리즘 검증, 계정 상태 확인, 비밀번호 변경 시각 비교, 토큰 취소 확인, 자동 갱신
//   - VerifyToken(): 발급자(issuer) 기반 디스패치 (argocd vs OIDC)
//   - VerifyUsernamePassword(): 빈 비밀번호, 길이 제한, 타이밍 공격 방지, 실패 횟수 제한
//   - updateFailureCount(): 윈도우 만료, 캐시 제거, admin 보호
//
// 실행: go run main.go

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ============================================================
// JWT 구현 (HS256 — HMAC-SHA256)
// ============================================================
//
// 실제 Argo CD는 golang-jwt/jwt 라이브러리를 사용하지만,
// 여기서는 HS256 핵심 로직을 표준 라이브러리로 직접 구현합니다.

// JWTHeader JWT 헤더
type JWTHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

// JWTClaims JWT 클레임
// 실제 Argo CD: server/session/sessionmanager.go claimsType
type JWTClaims struct {
	Issuer     string `json:"iss"`           // "argocd"
	Subject    string `json:"sub"`           // 사용자명
	IssuedAt   int64  `json:"iat"`           // 발급 시각 (Unix)
	Expiry     int64  `json:"exp"`           // 만료 시각 (Unix)
	JWTID      string `json:"jti"`           // 고유 ID (UUID)
	Authorized bool   `json:"argocd.io/auth"` // 승인 여부
}

// IsExpired 토큰 만료 여부
func (c *JWTClaims) IsExpired() bool {
	return time.Now().Unix() > c.Expiry
}

// RemainingDuration 남은 유효 시간
func (c *JWTClaims) RemainingDuration() time.Duration {
	rem := time.Duration(c.Expiry-time.Now().Unix()) * time.Second
	if rem < 0 {
		return 0
	}
	return rem
}

// b64Encode Base64URL 인코딩 (패딩 없음)
func b64Encode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// b64Decode Base64URL 디코딩
func b64Decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// signHS256 HMAC-SHA256 서명 생성
func signHS256(data string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return b64Encode(mac.Sum(nil))
}

// generateJTI 고유 JWT ID 생성 (UUID v4 단순 시뮬레이션)
func generateJTI() string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ============================================================
// 계정 모델
// ============================================================

// AccountCapability 계정 기능
type AccountCapability string

const (
	CapabilityLogin AccountCapability = "login"
	CapabilityAPIKey AccountCapability = "apiKey"
)

// Account 사용자 계정
type Account struct {
	Name           string
	PasswordHash   string    // bcrypt 해시 (시뮬레이션에서는 단순 문자열)
	PasswordMtime  time.Time // 비밀번호 마지막 변경 시각
	Enabled        bool
	Capabilities   []AccountCapability
	Tokens         []string  // API 토큰 목록
}

// HasCapability 계정이 특정 기능을 가지고 있는지 확인
func (a *Account) HasCapability(cap AccountCapability) bool {
	for _, c := range a.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// ============================================================
// 로그인 실패 추적
// ============================================================

const (
	maxLoginFailures = 5                // 최대 실패 횟수
	failureWindow    = 300 * time.Second // 실패 윈도우 (5분)
)

// LoginFailure 로그인 실패 기록
type LoginFailure struct {
	Count        int
	LastFailedAt time.Time
	WindowStart  time.Time
}

// ============================================================
// 토큰 취소 목록 (Revocation List)
// ============================================================

// RevokedToken 취소된 토큰 기록
type RevokedToken struct {
	JTI       string
	RevokedAt time.Time
}

// ============================================================
// SessionManager
// ============================================================
//
// 실제 Argo CD: server/session/sessionmanager.go SessionManager

const (
	issuerArgoCD    = "argocd"
	tokenExpiry     = 24 * time.Hour
	autoRenewBefore = 5 * time.Minute // 만료 5분 전 자동 갱신
	maxUsernameLen  = 32
)

// SessionManager JWT 세션 관리자
type SessionManager struct {
	mu            sync.RWMutex
	serverSecret  []byte                   // JWT 서명 비밀 키
	accounts      map[string]*Account      // 계정 저장소
	failures      map[string]*LoginFailure // 로그인 실패 추적
	revokedTokens map[string]*RevokedToken // 취소된 토큰 목록
}

// NewSessionManager 세션 관리자 생성
func NewSessionManager(secret string) *SessionManager {
	return &SessionManager{
		serverSecret:  []byte(secret),
		accounts:      make(map[string]*Account),
		failures:      make(map[string]*LoginFailure),
		revokedTokens: make(map[string]*RevokedToken),
	}
}

// RegisterAccount 계정 등록
func (sm *SessionManager) RegisterAccount(account *Account) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.accounts[account.Name] = account
}

// ----------------------------------------------------------------
// Create() — JWT 생성
// ----------------------------------------------------------------
//
// 실제 Argo CD: server/session/sessionmanager.go Create()

// Create JWT 토큰 생성
func (sm *SessionManager) Create(username string, expiresIn time.Duration) (string, error) {
	now := time.Now()
	claims := JWTClaims{
		Issuer:     issuerArgoCD,
		Subject:    username,
		IssuedAt:   now.Unix(),
		Expiry:     now.Add(expiresIn).Unix(),
		JWTID:      generateJTI(),
		Authorized: true,
	}

	// 헤더 인코딩
	headerJSON, _ := json.Marshal(JWTHeader{Algorithm: "HS256", Type: "JWT"})
	headerB64 := b64Encode(headerJSON)

	// 페이로드 인코딩
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("클레임 직렬화 오류: %w", err)
	}
	payloadB64 := b64Encode(payloadJSON)

	// 서명 생성
	signingInput := headerB64 + "." + payloadB64
	signature := signHS256(signingInput, sm.serverSecret)

	token := signingInput + "." + signature

	fmt.Printf("  [Create] 사용자=%s, JTI=%s, 만료=%s\n",
		username, claims.JWTID, time.Unix(claims.Expiry, 0).Format("15:04:05"))

	return token, nil
}

// ----------------------------------------------------------------
// Parse() — JWT 파싱 및 검증
// ----------------------------------------------------------------
//
// 검증 순서:
//   1. 알고리즘 확인 (HS256만 허용)
//   2. 서명 검증
//   3. 클레임 파싱
//   4. 만료 확인
//   5. 계정 활성화 확인
//   6. 비밀번호 변경 시각 비교 (PasswordMtime > issuedAt → 무효)
//   7. 토큰 취소 여부 확인
//   8. 자동 갱신 (남은 시간 < autoRenewBefore)

// ParseResult Parse() 결과
type ParseResult struct {
	Claims   *JWTClaims
	NewToken string // 자동 갱신된 경우 새 토큰
	Renewed  bool
}

// Parse JWT 파싱 및 검증
func (sm *SessionManager) Parse(tokenString string) (*ParseResult, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("잘못된 JWT 형식")
	}

	// 1. 헤더 디코딩 및 알고리즘 확인
	headerBytes, err := b64Decode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("헤더 디코딩 오류")
	}
	var header JWTHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("헤더 파싱 오류")
	}
	// 알고리즘 검증 — HMAC만 허용 (RSA 사칭 공격 방지)
	if header.Algorithm != "HS256" {
		return nil, fmt.Errorf("허용되지 않은 알고리즘: %s (HS256만 허용)", header.Algorithm)
	}

	// 2. 서명 검증
	signingInput := parts[0] + "." + parts[1]
	expectedSig := signHS256(signingInput, sm.serverSecret)
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return nil, fmt.Errorf("서명 검증 실패")
	}

	// 3. 클레임 파싱
	payloadBytes, err := b64Decode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("페이로드 디코딩 오류")
	}
	var claims JWTClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("클레임 파싱 오류")
	}

	// 4. 만료 확인
	if claims.IsExpired() {
		return nil, fmt.Errorf("토큰 만료됨 (만료: %s)",
			time.Unix(claims.Expiry, 0).Format("15:04:05"))
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// 5. 계정 활성화 확인
	account, ok := sm.accounts[claims.Subject]
	if !ok {
		return nil, fmt.Errorf("계정 없음: %s", claims.Subject)
	}
	if !account.Enabled {
		return nil, fmt.Errorf("비활성화된 계정: %s", claims.Subject)
	}

	// 6. 비밀번호 변경 시각 비교
	// PasswordMtime이 토큰 발급 시각보다 나중이면 → 토큰 무효
	issuedAt := time.Unix(claims.IssuedAt, 0)
	if !account.PasswordMtime.IsZero() && account.PasswordMtime.After(issuedAt) {
		return nil, fmt.Errorf("비밀번호 변경으로 토큰 무효화됨 (변경: %s, 발급: %s)",
			account.PasswordMtime.Format("15:04:05"),
			issuedAt.Format("15:04:05"))
	}

	// 7. 토큰 취소 여부 확인
	if revoked, ok := sm.revokedTokens[claims.JWTID]; ok {
		return nil, fmt.Errorf("취소된 토큰 (JTI=%s, 취소: %s)",
			claims.JWTID, revoked.RevokedAt.Format("15:04:05"))
	}

	result := &ParseResult{Claims: &claims}

	// 8. 자동 갱신 확인 (남은 시간 < autoRenewBefore)
	remaining := claims.RemainingDuration()
	if remaining < autoRenewBefore {
		fmt.Printf("  [Parse] 자동 갱신 — 남은 시간: %v (임계값: %v)\n", remaining, autoRenewBefore)
		// 잠금 해제 후 새 토큰 생성 (RLock은 Create에서 필요 없음)
		// 실제에서는 sm.mu.RUnlock() 후 Create() 호출
		newToken := tokenString + "_RENEWED_" + generateJTI()[:8] // 단순화
		result.NewToken = newToken
		result.Renewed = true
	}

	return result, nil
}

// RevokeToken 토큰 취소
func (sm *SessionManager) RevokeToken(jti string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.revokedTokens[jti] = &RevokedToken{
		JTI:       jti,
		RevokedAt: time.Now(),
	}
	fmt.Printf("  [Revoke] 토큰 취소 완료: JTI=%s\n", jti)
}

// ----------------------------------------------------------------
// VerifyToken() — 발급자 기반 디스패치
// ----------------------------------------------------------------
//
// 실제 Argo CD: server/session/sessionmanager.go VerifyToken()

// OIDCProvider OIDC 공급자 시뮬레이션
type OIDCProvider struct {
	Issuer string
}

// VerifyOIDC OIDC 토큰 검증 시뮬레이션
func (p *OIDCProvider) VerifyOIDC(token string) (*JWTClaims, error) {
	// 실제에서는 OIDC 공급자의 JWKS 엔드포인트에서 공개키를 가져와 검증
	fmt.Printf("  [OIDC] 공급자 %s에서 토큰 검증 중...\n", p.Issuer)
	// 시뮬레이션: 항상 성공
	return &JWTClaims{
		Issuer:   p.Issuer,
		Subject:  "oidc-user@example.com",
		IssuedAt: time.Now().Unix(),
		Expiry:   time.Now().Add(1 * time.Hour).Unix(),
	}, nil
}

// VerifyToken 토큰 검증 — 발급자 기반 디스패치
func (sm *SessionManager) VerifyToken(tokenString string) (*JWTClaims, error) {
	// 우선 로컬 JWT 파싱 시도 (issuer 확인용)
	parts := strings.Split(tokenString, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("잘못된 토큰 형식")
	}

	payloadBytes, err := b64Decode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("토큰 디코딩 오류")
	}

	var rawClaims map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &rawClaims); err != nil {
		return nil, fmt.Errorf("클레임 파싱 오류")
	}

	issuer, _ := rawClaims["iss"].(string)

	switch issuer {
	case issuerArgoCD:
		// 로컬 JWT 검증
		fmt.Printf("  [VerifyToken] issuer=argocd → 로컬 JWT 검증\n")
		result, err := sm.Parse(tokenString)
		if err != nil {
			return nil, err
		}
		return result.Claims, nil

	default:
		// OIDC 공급자 검증
		fmt.Printf("  [VerifyToken] issuer=%s → OIDC 검증\n", issuer)
		oidcProvider := &OIDCProvider{Issuer: issuer}
		return oidcProvider.VerifyOIDC(tokenString)
	}
}

// ----------------------------------------------------------------
// VerifyUsernamePassword() — 로그인 검증
// ----------------------------------------------------------------
//
// 실제 Argo CD: server/session/sessionmanager.go VerifyUsernamePassword()

// hashPassword 비밀번호 해시 (단순 SHA256 시뮬레이션)
// 실제 Argo CD: bcrypt 사용
func hashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return fmt.Sprintf("%x", h)
}

// VerifyUsernamePassword 사용자명/비밀번호 검증
func (sm *SessionManager) VerifyUsernamePassword(username, password string) error {
	// 1. 빈 비밀번호 거부
	if password == "" {
		return fmt.Errorf("빈 비밀번호는 허용되지 않습니다")
	}

	// 2. 사용자명 길이 제한 (maxLoginFailures 캐시의 키 크기 제한)
	if len(username) > maxUsernameLen {
		return fmt.Errorf("사용자명이 너무 깁니다 (최대 %d자)", maxUsernameLen)
	}

	// 3. 타이밍 노이즈 — 타이밍 공격 방지
	// 성공/실패 여부와 관계없이 일정 시간 대기 (500~1000ms)
	// 실제 Argo CD: server/session/sessionmanager.go 내 time.Sleep 사용
	delay := time.Duration(500+rand.Intn(500)) * time.Millisecond
	fmt.Printf("  [VerifyPassword] 타이밍 노이즈 적용: %v\n", delay)
	time.Sleep(delay)

	sm.mu.RLock()
	account, exists := sm.accounts[username]
	sm.mu.RUnlock()

	// 4. 존재하지 않는 계정 — 동일 응답 시간 유지
	if !exists {
		// 비밀번호 해시는 수행하여 일정한 응답 시간 유지 (타이밍 공격 방지)
		_ = hashPassword(password)
		sm.updateFailureCount(username)
		return fmt.Errorf("사용자명 또는 비밀번호가 잘못되었습니다")
	}

	// 5. 실패 횟수 확인
	if err := sm.checkFailureCount(username); err != nil {
		return err
	}

	// 6. 비밀번호 검증
	if hashPassword(password) != account.PasswordHash {
		sm.updateFailureCount(username)
		return fmt.Errorf("사용자명 또는 비밀번호가 잘못되었습니다")
	}

	// 7. 계정 활성화 확인
	if !account.Enabled {
		return fmt.Errorf("계정이 비활성화되었습니다")
	}

	// 8. 로그인 기능 확인
	if !account.HasCapability(CapabilityLogin) {
		return fmt.Errorf("로그인 권한이 없습니다")
	}

	// 로그인 성공 → 실패 카운트 초기화
	sm.resetFailureCount(username)
	return nil
}

// checkFailureCount 실패 횟수 초과 여부 확인
func (sm *SessionManager) checkFailureCount(username string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	failure, ok := sm.failures[username]
	if !ok {
		return nil
	}

	// 윈도우 만료 확인
	if time.Since(failure.WindowStart) > failureWindow {
		return nil // 윈도우 만료 → 제한 없음
	}

	if failure.Count >= maxLoginFailures {
		return fmt.Errorf("로그인 시도 횟수 초과 (최대 %d회, 윈도우 %v) — 잠시 후 다시 시도하세요",
			maxLoginFailures, failureWindow)
	}

	return nil
}

// updateFailureCount 실패 횟수 업데이트
// 실제 Argo CD: server/session/sessionmanager.go updateFailureCount()
func (sm *SessionManager) updateFailureCount(username string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	failure, ok := sm.failures[username]
	now := time.Now()

	if !ok || time.Since(failure.WindowStart) > failureWindow {
		// 새 윈도우 시작
		sm.failures[username] = &LoginFailure{
			Count:        1,
			LastFailedAt: now,
			WindowStart:  now,
		}
	} else {
		failure.Count++
		failure.LastFailedAt = now
	}

	fmt.Printf("  [FailureCount] 사용자=%s, 실패=%d/%d\n",
		username, sm.failures[username].Count, maxLoginFailures)
}

// resetFailureCount 실패 횟수 초기화
func (sm *SessionManager) resetFailureCount(username string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.failures, username)
}

// ============================================================
// 시뮬레이션 실행
// ============================================================

func main() {
	fmt.Println("============================================================")
	fmt.Println("  Argo CD 세션 관리 시뮬레이션 (PoC-15)")
	fmt.Println("============================================================")

	// 세션 관리자 초기화
	sm := NewSessionManager("argocd-server-secret-key-12345")

	// 계정 등록
	adminPassHash := hashPassword("admin123")
	sm.RegisterAccount(&Account{
		Name:          "admin",
		PasswordHash:  adminPassHash,
		PasswordMtime: time.Now().Add(-48 * time.Hour), // 48시간 전 설정
		Enabled:       true,
		Capabilities:  []AccountCapability{CapabilityLogin, CapabilityAPIKey},
	})

	devPassHash := hashPassword("dev-secret")
	sm.RegisterAccount(&Account{
		Name:          "developer",
		PasswordHash:  devPassHash,
		PasswordMtime: time.Now().Add(-24 * time.Hour),
		Enabled:       true,
		Capabilities:  []AccountCapability{CapabilityLogin},
	})

	sm.RegisterAccount(&Account{
		Name:          "disabled-user",
		PasswordHash:  hashPassword("pass"),
		PasswordMtime: time.Now().Add(-72 * time.Hour),
		Enabled:       false,
		Capabilities:  []AccountCapability{CapabilityLogin},
	})

	// ----------------------------------------------------------------
	// 시나리오 1: JWT 생성 및 검증
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 1: JWT 생성 및 검증")
	fmt.Println("============================")

	token, err := sm.Create("admin", tokenExpiry)
	if err != nil {
		fmt.Printf("토큰 생성 오류: %v\n", err)
		return
	}
	fmt.Printf("  토큰 (앞 60자): %s...\n", token[:60])

	result, err := sm.VerifyToken(token)
	if err != nil {
		fmt.Printf("  토큰 검증 실패: %v\n", err)
	} else {
		fmt.Printf("  검증 성공: sub=%s, iss=%s, 만료=%s\n",
			result.Subject, result.Issuer,
			time.Unix(result.Expiry, 0).Format("15:04:05"))
	}

	// ----------------------------------------------------------------
	// 시나리오 2: 토큰 취소 (Revocation)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 2: 토큰 취소 (Revocation)")
	fmt.Println("============================")

	revokeToken, _ := sm.Create("developer", tokenExpiry)
	// 클레임에서 JTI 추출
	rparts := strings.Split(revokeToken, ".")
	payloadBytes, _ := b64Decode(rparts[1])
	var rClaims JWTClaims
	json.Unmarshal(payloadBytes, &rClaims)

	fmt.Printf("  토큰 생성: JTI=%s\n", rClaims.JWTID)
	sm.RevokeToken(rClaims.JWTID)

	_, err = sm.Parse(revokeToken)
	if err != nil {
		fmt.Printf("  취소 후 검증: %v\n", err)
	}

	// ----------------------------------------------------------------
	// 시나리오 3: 알고리즘 검증 (보안 — "alg:none" 공격 방지)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 3: 잘못된 알고리즘 토큰 거부")
	fmt.Println("============================")

	// RS256 알고리즘으로 위조된 토큰
	fakeHeader := b64Encode([]byte(`{"alg":"RS256","typ":"JWT"}`))
	fakeClaims := JWTClaims{
		Issuer: issuerArgoCD, Subject: "admin",
		IssuedAt: time.Now().Unix(), Expiry: time.Now().Add(1 * time.Hour).Unix(),
	}
	fakePayloadJSON, _ := json.Marshal(fakeClaims)
	fakePayload := b64Encode(fakePayloadJSON)
	fakeToken := fakeHeader + "." + fakePayload + ".fakesig"

	_, err = sm.Parse(fakeToken)
	fmt.Printf("  RS256 토큰 거부: %v\n", err)

	// ----------------------------------------------------------------
	// 시나리오 4: 비밀번호 변경으로 인한 토큰 무효화
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 4: 비밀번호 변경 → 기존 토큰 무효화")
	fmt.Println("============================")

	oldToken, _ := sm.Create("developer", tokenExpiry)
	fmt.Println("  기존 토큰 생성 완료")

	// 비밀번호 변경 (PasswordMtime 갱신)
	time.Sleep(10 * time.Millisecond)
	sm.mu.Lock()
	sm.accounts["developer"].PasswordMtime = time.Now()
	sm.mu.Unlock()
	fmt.Println("  비밀번호 변경 완료 (PasswordMtime 갱신)")

	_, err = sm.Parse(oldToken)
	if err != nil {
		fmt.Printf("  기존 토큰 검증 결과: %v\n", err)
	}

	// ----------------------------------------------------------------
	// 시나리오 5: 자동 갱신 (Auto-Renewal)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 5: 자동 갱신 — 만료 임박 시 새 토큰 발급")
	fmt.Println("============================")

	// 3분 후 만료되는 토큰 생성 (autoRenewBefore=5분 이내)
	shortToken, _ := sm.Create("admin", 3*time.Minute)
	fmt.Println("  만료 임박 토큰 생성 (3분 후 만료)")

	parseResult, err := sm.Parse(shortToken)
	if err != nil {
		fmt.Printf("  파싱 오류: %v\n", err)
	} else {
		fmt.Printf("  자동 갱신 여부: %v\n", parseResult.Renewed)
		if parseResult.Renewed {
			fmt.Printf("  새 토큰 발급됨 (앞 30자): %s...\n", parseResult.NewToken[:30])
		}
	}

	// ----------------------------------------------------------------
	// 시나리오 6: 로그인 보안 (VerifyUsernamePassword)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 6: 로그인 보안 — 타이밍 노이즈 및 실패 횟수 제한")
	fmt.Println("============================")

	// 빈 비밀번호 거부
	fmt.Println("\n[테스트] 빈 비밀번호:")
	err = sm.VerifyUsernamePassword("admin", "")
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	// 사용자명 길이 초과
	fmt.Println("\n[테스트] 사용자명 길이 초과:")
	err = sm.VerifyUsernamePassword(strings.Repeat("a", 33), "pass")
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	}

	// 올바른 비밀번호
	fmt.Println("\n[테스트] 올바른 비밀번호:")
	err = sm.VerifyUsernamePassword("admin", "admin123")
	if err != nil {
		fmt.Printf("  결과: %v\n", err)
	} else {
		fmt.Println("  로그인 성공!")
	}

	// 잘못된 비밀번호 반복 (실패 횟수 제한 테스트)
	fmt.Println("\n[테스트] 잘못된 비밀번호 반복 시도 (실패 횟수 제한):")
	// 빠른 테스트를 위해 실패 카운트를 수동으로 설정
	sm.mu.Lock()
	sm.failures["admin"] = &LoginFailure{
		Count:       maxLoginFailures - 1,
		WindowStart: time.Now(),
		LastFailedAt: time.Now(),
	}
	sm.mu.Unlock()

	// 한 번 더 실패
	sm.updateFailureCount("admin")

	err = sm.checkFailureCount("admin")
	if err != nil {
		fmt.Printf("  실패 횟수 초과: %v\n", err)
	}

	// ----------------------------------------------------------------
	// 시나리오 7: OIDC 토큰 디스패치
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 7: OIDC 토큰 — 발급자 기반 디스패치")
	fmt.Println("============================")

	// OIDC issuer를 포함한 가짜 토큰 생성 (서명 검증 없이 issuer 디스패치 확인)
	oidcHeader := b64Encode([]byte(`{"alg":"RS256","typ":"JWT"}`))
	oidcClaims := map[string]interface{}{
		"iss": "https://accounts.google.com",
		"sub": "user@example.com",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}
	oidcPayloadJSON, _ := json.Marshal(oidcClaims)
	oidcToken := oidcHeader + "." + b64Encode(oidcPayloadJSON) + ".oidcsig"

	fmt.Printf("  OIDC 토큰 issuer 확인 → OIDC 공급자로 디스패치\n")
	// VerifyToken에서 issuer 파싱 후 OIDC 분기 확인
	parts := strings.Split(oidcToken, ".")
	if len(parts) >= 2 {
		payload, _ := b64Decode(parts[1])
		var claims map[string]interface{}
		json.Unmarshal(payload, &claims)
		issuer, _ := claims["iss"].(string)
		fmt.Printf("  issuer=%s → OIDC 공급자로 라우팅\n", issuer)
		oidcProvider := &OIDCProvider{Issuer: issuer}
		oidcResult, _ := oidcProvider.VerifyOIDC(oidcToken)
		fmt.Printf("  OIDC 검증 결과: sub=%s\n", oidcResult.Subject)
	}

	fmt.Println("\n[완료] 세션 관리 시뮬레이션 종료")
}
