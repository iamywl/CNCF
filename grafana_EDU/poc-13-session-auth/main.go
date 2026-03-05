package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ============================================================
// Grafana 세션 인증 시뮬레이션
// 토큰 발급 → 검증 → 회전 → 폐기 전체 수명주기 재현
// ============================================================

// --- 상수 ---

const (
	tokenRotationInterval  = 10 * time.Minute  // 토큰 회전 주기
	tokenGracePeriod       = 30 * time.Second   // 회전 후 이전 토큰 유예 기간
	tokenMaxLifetime       = 30 * 24 * time.Hour // 최대 수명 (30일)
	tokenMaxInactive       = 7 * 24 * time.Hour  // 비활성 만료 (7일)
	maxConcurrentSessions  = 3                    // 사용자당 최대 동시 세션
	tokenByteLength        = 32                   // 토큰 바이트 길이
)

// --- 데이터 구조 ---

// UserToken은 Grafana의 user_auth_token 테이블 레코드를 시뮬레이션한다.
// 실제 구현: pkg/services/auth/model.go
type UserToken struct {
	ID             int64
	UserID         int64
	AuthToken      string    // SHA256(rawToken) — DB에 저장되는 해시
	PrevAuthToken  string    // 회전 직전 토큰 해시 (유예 기간용)
	CreatedAt      time.Time
	RotatedAt      time.Time
	AuthTokenSeen  bool      // 현재 토큰이 한 번이라도 사용되었는지
	SeenAt         time.Time // 마지막 사용 시각
	RevokedAt      *time.Time
	UnhashedToken  string    // 시뮬레이션용: 원본 토큰 (실제로는 DB에 없음)
}

// LookupResult는 토큰 조회 결과를 나타낸다.
type LookupResult struct {
	Token       *UserToken
	NeedRotate  bool
	UsedPrev    bool   // 이전 토큰(유예 기간)으로 매칭됨
	Error       string
}

// Cookie는 HTTP 쿠키를 시뮬레이션한다.
type Cookie struct {
	Name     string
	Value    string
	Path     string
	HTTPOnly bool
	Secure   bool
	MaxAge   int
}

// --- TokenService ---

// TokenService는 Grafana의 UserAuthTokenService를 시뮬레이션한다.
// 실제 구현: pkg/services/auth/authtoken/service.go
type TokenService struct {
	mu       sync.RWMutex
	tokens   map[int64]*UserToken     // tokenID → token
	byHash   map[string]*UserToken    // tokenHash → token
	byUser   map[int64][]*UserToken   // userID → tokens
	nextID   int64
	now      func() time.Time         // 테스트/시뮬레이션용 시간 제공 함수
}

func NewTokenService(nowFn func() time.Time) *TokenService {
	return &TokenService{
		tokens: make(map[int64]*UserToken),
		byHash: make(map[string]*UserToken),
		byUser: make(map[int64][]*UserToken),
		nextID: 1,
		now:    nowFn,
	}
}

// generateToken은 암호학적으로 안전한 랜덤 토큰을 생성한다.
func generateToken() (raw string, hashed string, err error) {
	bytes := make([]byte, tokenByteLength)
	_, err = rand.Read(bytes)
	if err != nil {
		return "", "", fmt.Errorf("토큰 생성 실패: %w", err)
	}
	raw = hex.EncodeToString(bytes)
	hashed = hashToken(raw)
	return raw, hashed, nil
}

// hashToken은 원본 토큰을 SHA256으로 해싱한다.
func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// CreateToken은 새 세션 토큰을 생성한다.
// 동시 세션 수가 초과되면 가장 오래된 세션을 폐기한다.
func (s *TokenService) CreateToken(userID int64) (*UserToken, *Cookie, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	// 동시 세션 제한 확인
	userTokens := s.activeTokensForUser(userID)
	if len(userTokens) >= maxConcurrentSessions {
		// 가장 오래된 세션 폐기
		oldest := userTokens[0]
		for _, t := range userTokens[1:] {
			if t.CreatedAt.Before(oldest.CreatedAt) {
				oldest = t
			}
		}
		s.revokeTokenLocked(oldest, now)
		fmt.Printf("  [제한] 동시 세션 초과 → 가장 오래된 세션 폐기 (ID=%d)\n", oldest.ID)
	}

	// 새 토큰 생성
	rawToken, hashedToken, err := generateToken()
	if err != nil {
		return nil, nil, err
	}

	token := &UserToken{
		ID:            s.nextID,
		UserID:        userID,
		AuthToken:     hashedToken,
		PrevAuthToken: "",
		CreatedAt:     now,
		RotatedAt:     now,
		AuthTokenSeen: false,
		SeenAt:        now,
		UnhashedToken: rawToken,
	}
	s.nextID++

	// 인덱스에 등록
	s.tokens[token.ID] = token
	s.byHash[hashedToken] = token
	s.byUser[userID] = append(s.byUser[userID], token)

	// 쿠키 생성
	cookie := &Cookie{
		Name:     "grafana_session",
		Value:    rawToken,
		Path:     "/",
		HTTPOnly: true,
		Secure:   true,
		MaxAge:   int(tokenMaxLifetime.Seconds()),
	}

	return token, cookie, nil
}

// LookupToken은 쿠키의 원본 토큰으로 세션을 조회한다.
func (s *TokenService) LookupToken(rawToken string) *LookupResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	hashed := hashToken(rawToken)

	// 1. 현재 토큰 해시로 조회
	token, found := s.byHash[hashed]
	usedPrev := false

	if !found {
		// 2. 이전 토큰(유예 기간)으로 조회
		for _, t := range s.tokens {
			if t.PrevAuthToken == hashed && t.RevokedAt == nil {
				// 유예 기간 확인
				if now.Sub(t.RotatedAt) <= tokenGracePeriod {
					token = t
					usedPrev = true
					found = true
					break
				}
			}
		}
	}

	if !found {
		return &LookupResult{Error: "토큰을 찾을 수 없음"}
	}

	// 3. 폐기된 토큰인지 확인
	if token.RevokedAt != nil {
		return &LookupResult{Error: "폐기된 토큰"}
	}

	// 4. 최대 수명 확인
	if now.Sub(token.CreatedAt) > tokenMaxLifetime {
		return &LookupResult{Error: fmt.Sprintf("최대 수명 초과 (생성: %s)", token.CreatedAt.Format("2006-01-02"))}
	}

	// 5. 비활성 만료 확인
	if now.Sub(token.SeenAt) > tokenMaxInactive {
		return &LookupResult{Error: fmt.Sprintf("비활성 만료 (마지막 사용: %s)", token.SeenAt.Format("2006-01-02 15:04"))}
	}

	// 6. 사용 시각 갱신
	token.SeenAt = now
	if !token.AuthTokenSeen {
		token.AuthTokenSeen = true
	}

	// 7. 회전 필요 여부 확인
	needRotate := now.Sub(token.RotatedAt) > tokenRotationInterval

	return &LookupResult{
		Token:      token,
		NeedRotate: needRotate,
		UsedPrev:   usedPrev,
	}
}

// RotateToken은 토큰을 회전시킨다. 이전 토큰은 유예 기간 동안 유효하다.
func (s *TokenService) RotateToken(tokenID int64) (newRawToken string, cookie *Cookie, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	token, exists := s.tokens[tokenID]
	if !exists {
		return "", nil, fmt.Errorf("토큰 ID %d 없음", tokenID)
	}

	if token.RevokedAt != nil {
		return "", nil, fmt.Errorf("폐기된 토큰은 회전할 수 없음")
	}

	// 이전 해시 인덱스 제거
	delete(s.byHash, token.AuthToken)

	// 새 토큰 생성
	rawToken, hashedToken, err := generateToken()
	if err != nil {
		return "", nil, err
	}

	// 이전 토큰을 PrevAuthToken에 보관 (유예 기간용)
	token.PrevAuthToken = token.AuthToken
	token.AuthToken = hashedToken
	token.RotatedAt = now
	token.AuthTokenSeen = false
	token.UnhashedToken = rawToken

	// 새 해시로 인덱스 등록
	s.byHash[hashedToken] = token

	cookie = &Cookie{
		Name:     "grafana_session",
		Value:    rawToken,
		Path:     "/",
		HTTPOnly: true,
		Secure:   true,
		MaxAge:   int(tokenMaxLifetime.Seconds()),
	}

	return rawToken, cookie, nil
}

// RevokeToken은 토큰을 폐기한다 (로그아웃).
func (s *TokenService) RevokeToken(tokenID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	token, exists := s.tokens[tokenID]
	if !exists {
		return fmt.Errorf("토큰 ID %d 없음", tokenID)
	}

	s.revokeTokenLocked(token, now)
	return nil
}

// RevokeAllTokens는 사용자의 모든 토큰을 폐기한다.
func (s *TokenService) RevokeAllTokens(userID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	count := 0
	for _, token := range s.byUser[userID] {
		if token.RevokedAt == nil {
			s.revokeTokenLocked(token, now)
			count++
		}
	}
	return count
}

func (s *TokenService) revokeTokenLocked(token *UserToken, now time.Time) {
	token.RevokedAt = &now
	delete(s.byHash, token.AuthToken)
}

// activeTokensForUser는 사용자의 활성 토큰 목록을 반환한다.
func (s *TokenService) activeTokensForUser(userID int64) []*UserToken {
	var active []*UserToken
	for _, t := range s.byUser[userID] {
		if t.RevokedAt == nil {
			active = append(active, t)
		}
	}
	return active
}

// ActiveSessionCount는 사용자의 활성 세션 수를 반환한다.
func (s *TokenService) ActiveSessionCount(userID int64) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.activeTokensForUser(userID))
}

// --- 시뮬레이션 헬퍼 ---

func shortToken(token string) string {
	if len(token) > 12 {
		return token[:12] + "..."
	}
	return token
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f초", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.0f분", d.Minutes())
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%.1f시간", d.Hours())
	}
	return fmt.Sprintf("%.0f일", d.Hours()/24)
}

// --- 메인: 시뮬레이션 ---

func main() {
	fmt.Println("=== Grafana 세션 인증 시뮬레이션 ===")
	fmt.Println()

	// 시뮬레이션용 현재 시각 (조절 가능)
	simTime := time.Now()
	nowFn := func() time.Time { return simTime }

	svc := NewTokenService(nowFn)

	// ------------------------------------------
	// 1. 로그인 → 토큰 생성
	// ------------------------------------------
	fmt.Println("━━━ 1. 로그인: 토큰 생성 ━━━")
	fmt.Println()

	userID := int64(1)
	userName := "admin"

	fmt.Printf("[LOGIN] user=%s (ID=%d)\n", userName, userID)
	token, cookie, err := svc.CreateToken(userID)
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		return
	}
	fmt.Printf("  토큰 생성: %s\n", shortToken(token.UnhashedToken))
	fmt.Printf("  토큰 해시(DB 저장): %s\n", shortToken(token.AuthToken))
	fmt.Printf("  쿠키 설정: %s=%s\n", cookie.Name, shortToken(cookie.Value))
	fmt.Printf("  생성 시각: %s\n", token.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("  최대 수명: %s 후\n", formatDuration(tokenMaxLifetime))
	fmt.Printf("  비활성 만료: %s 후\n", formatDuration(tokenMaxInactive))
	fmt.Printf("  활성 세션: %d/%d\n", svc.ActiveSessionCount(userID), maxConcurrentSessions)
	fmt.Println()

	// 현재 쿠키 토큰 저장
	currentCookieToken := token.UnhashedToken
	currentTokenID := token.ID

	// ------------------------------------------
	// 2. 일반 요청 → 토큰 검증 (회전 불필요)
	// ------------------------------------------
	fmt.Println("━━━ 2. 요청: 토큰 검증 (3분 후) ━━━")
	fmt.Println()

	simTime = simTime.Add(3 * time.Minute)
	fmt.Printf("[REQUEST] GET /api/dashboards (로그인 후 3분)\n")
	result := svc.LookupToken(currentCookieToken)
	if result.Error != "" {
		fmt.Printf("  ERROR: %s\n", result.Error)
	} else {
		fmt.Printf("  토큰 유효 (남은 수명: %s)\n", formatDuration(tokenMaxLifetime-simTime.Sub(result.Token.CreatedAt)))
		fmt.Printf("  회전 필요: %v (마지막 회전: %s 전)\n", result.NeedRotate, formatDuration(simTime.Sub(result.Token.RotatedAt)))
	}
	fmt.Println()

	// ------------------------------------------
	// 3. 12분 후 요청 → 토큰 회전
	// ------------------------------------------
	fmt.Println("━━━ 3. 요청: 토큰 회전 (12분 후) ━━━")
	fmt.Println()

	simTime = simTime.Add(9 * time.Minute) // 총 12분
	fmt.Printf("[REQUEST] GET /api/datasources (로그인 후 12분)\n")
	result = svc.LookupToken(currentCookieToken)
	if result.Error != "" {
		fmt.Printf("  ERROR: %s\n", result.Error)
	} else {
		fmt.Printf("  토큰 유효\n")
		fmt.Printf("  회전 필요: %v (마지막 회전: %s 전 > %s)\n",
			result.NeedRotate, formatDuration(simTime.Sub(result.Token.RotatedAt)), formatDuration(tokenRotationInterval))

		if result.NeedRotate {
			oldToken := shortToken(currentCookieToken)
			newRaw, newCookie, err := svc.RotateToken(currentTokenID)
			if err != nil {
				fmt.Printf("  회전 실패: %v\n", err)
			} else {
				fmt.Printf("  이전 토큰: %s → 유예 기간 %s\n", oldToken, formatDuration(tokenGracePeriod))
				fmt.Printf("  새 토큰: %s\n", shortToken(newRaw))
				fmt.Printf("  쿠키 갱신: %s=%s\n", newCookie.Name, shortToken(newCookie.Value))

				// 이전 토큰 저장 (유예 기간 테스트용)
				prevCookieToken := currentCookieToken
				currentCookieToken = newRaw

				// 유예 기간 내 이전 토큰 사용 테스트
				fmt.Println()
				fmt.Printf("  [유예 기간 테스트] 이전 토큰으로 요청 (회전 직후)\n")
				simTime = simTime.Add(5 * time.Second)
				graceResult := svc.LookupToken(prevCookieToken)
				if graceResult.Error != "" {
					fmt.Printf("    결과: 실패 - %s\n", graceResult.Error)
				} else {
					fmt.Printf("    결과: 성공 (이전 토큰 유예 기간 내 사용, usedPrev=%v)\n", graceResult.UsedPrev)
				}

				// 유예 기간 만료 후 이전 토큰 사용 테스트
				fmt.Printf("  [유예 기간 만료 테스트] 이전 토큰으로 요청 (회전 후 35초)\n")
				simTime = simTime.Add(30 * time.Second)
				expiredResult := svc.LookupToken(prevCookieToken)
				if expiredResult.Error != "" {
					fmt.Printf("    결과: 실패 - %s\n", expiredResult.Error)
				} else {
					fmt.Printf("    결과: 성공 (예상 밖 — 유예 기간 검증 오류)\n")
				}
			}
		}
	}
	fmt.Println()

	// ------------------------------------------
	// 4. 동시 세션 제한 테스트
	// ------------------------------------------
	fmt.Println("━━━ 4. 동시 세션 제한 테스트 ━━━")
	fmt.Println()

	simTime = simTime.Add(1 * time.Minute)

	fmt.Printf("현재 활성 세션: %d/%d\n", svc.ActiveSessionCount(userID), maxConcurrentSessions)
	fmt.Println()

	// 추가 세션 생성 (최대 한도까지)
	for i := 0; i < maxConcurrentSessions; i++ {
		fmt.Printf("[LOGIN] user=%s 세션 #%d 생성\n", userName, i+2)
		t, _, err := svc.CreateToken(userID)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			continue
		}
		fmt.Printf("  토큰: %s, 활성 세션: %d/%d\n",
			shortToken(t.UnhashedToken), svc.ActiveSessionCount(userID), maxConcurrentSessions)
	}
	fmt.Println()

	// ------------------------------------------
	// 5. 비활성 만료 시뮬레이션
	// ------------------------------------------
	fmt.Println("━━━ 5. 비활성 만료 시뮬레이션 ━━━")
	fmt.Println()

	// 새 세션 생성
	simTime = simTime.Add(1 * time.Minute)
	fmt.Printf("[LOGIN] user=viewer (ID=2) — 비활성 만료 테스트용\n")
	viewerToken, _, _ := svc.CreateToken(2)
	viewerRaw := viewerToken.UnhashedToken
	fmt.Printf("  토큰: %s\n", shortToken(viewerRaw))

	// 7일 1시간 후 요청
	simTime = simTime.Add(7*24*time.Hour + 1*time.Hour)
	fmt.Printf("\n[REQUEST] GET /api/home (7일 1시간 후)\n")
	inactiveResult := svc.LookupToken(viewerRaw)
	if inactiveResult.Error != "" {
		fmt.Printf("  결과: 거부 - %s\n", inactiveResult.Error)
	} else {
		fmt.Printf("  결과: 허용 (예상 밖)\n")
	}
	fmt.Println()

	// ------------------------------------------
	// 6. 최대 수명 만료 시뮬레이션
	// ------------------------------------------
	fmt.Println("━━━ 6. 최대 수명 만료 시뮬레이션 ━━━")
	fmt.Println()

	// 시간 되돌리기
	simTime = time.Now()
	fmt.Printf("[LOGIN] user=editor (ID=3) — 최대 수명 테스트용\n")
	editorToken, _, _ := svc.CreateToken(3)
	editorRaw := editorToken.UnhashedToken
	fmt.Printf("  토큰: %s\n", shortToken(editorRaw))

	// 매일 사용하여 비활성 만료 방지, 하지만 30일 초과
	for day := 1; day <= 30; day++ {
		simTime = simTime.Add(24 * time.Hour)
		r := svc.LookupToken(editorRaw)
		if r.Error == "" && r.NeedRotate {
			newRaw, _, _ := svc.RotateToken(editorToken.ID)
			editorRaw = newRaw
		}
	}
	fmt.Printf("  30일간 매일 사용 및 회전 완료\n")

	// 31일째 — 최대 수명 초과
	simTime = simTime.Add(24 * time.Hour)
	maxResult := svc.LookupToken(editorRaw)
	if maxResult.Error != "" {
		fmt.Printf("  31일째 요청: 거부 - %s\n", maxResult.Error)
	} else {
		fmt.Printf("  31일째 요청: 허용 (예상 밖)\n")
	}
	fmt.Println()

	// ------------------------------------------
	// 7. 로그아웃 → 토큰 폐기
	// ------------------------------------------
	fmt.Println("━━━ 7. 로그아웃: 토큰 폐기 ━━━")
	fmt.Println()

	fmt.Printf("[LOGOUT] user=%s — 현재 세션 폐기\n", userName)
	err = svc.RevokeToken(currentTokenID)
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
	} else {
		fmt.Printf("  토큰 폐기 완료: %s\n", shortToken(currentCookieToken))
	}

	// 폐기된 토큰으로 요청
	fmt.Printf("\n[REQUEST] GET /api/dashboards (폐기된 토큰 사용)\n")
	revokedResult := svc.LookupToken(currentCookieToken)
	if revokedResult.Error != "" {
		fmt.Printf("  결과: 거부 - %s\n", revokedResult.Error)
	}

	// 전체 세션 폐기
	count := svc.RevokeAllTokens(userID)
	fmt.Printf("\n[REVOKE ALL] user=%s — %d개 세션 폐기\n", userName, count)
	fmt.Printf("  남은 활성 세션: %d\n", svc.ActiveSessionCount(userID))
	fmt.Println()

	// ------------------------------------------
	// 요약
	// ------------------------------------------
	fmt.Println("━━━ 시뮬레이션 요약 ━━━")
	fmt.Println()
	fmt.Println("토큰 수명주기:")
	fmt.Println("  1. 로그인 → CreateToken → 32바이트 랜덤 hex 생성")
	fmt.Println("  2. SHA256 해시를 DB에 저장 (원본 토큰 미저장)")
	fmt.Println("  3. 원본 토큰을 grafana_session 쿠키로 전달")
	fmt.Println("  4. 요청마다 LookupToken → 해시 비교로 검증")
	fmt.Println("  5. 10분마다 RotateToken → 새 토큰 발급, 이전 토큰 30초 유예")
	fmt.Println("  6. 30일 최대 수명 또는 7일 비활성 시 자동 만료")
	fmt.Println("  7. 로그아웃 → RevokeToken → 토큰 즉시 무효화")
	fmt.Println()
	fmt.Println("보안 설계:")
	fmt.Printf("  %-20s %s\n", "토큰 길이:", fmt.Sprintf("%d바이트 (%d hex chars)", tokenByteLength, tokenByteLength*2))
	fmt.Printf("  %-20s %s\n", "저장 방식:", "SHA256 해시")
	fmt.Printf("  %-20s %s\n", "회전 주기:", formatDuration(tokenRotationInterval))
	fmt.Printf("  %-20s %s\n", "유예 기간:", formatDuration(tokenGracePeriod))
	fmt.Printf("  %-20s %s\n", "최대 수명:", formatDuration(tokenMaxLifetime))
	fmt.Printf("  %-20s %s\n", "비활성 만료:", formatDuration(tokenMaxInactive))
	fmt.Printf("  %-20s %d\n", "동시 세션:", maxConcurrentSessions)

	_ = strings.Builder{} // strings 패키지 사용 확인
}
