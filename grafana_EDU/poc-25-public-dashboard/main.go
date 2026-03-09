// poc-25-public-dashboard: Grafana 공개 대시보드 시스템 시뮬레이션
//
// 핵심 개념:
//   - AccessToken 기반 인증 없는 대시보드 접근
//   - ShareType (public, email)
//   - 실시간 쿼리 프록시 (스냅샷과 차이)
//   - 미들웨어 체인 (OrgID 주입, AccessToken 검증)
//   - 시간 선택/주석 활성화 설정
//
// 실행: go run main.go

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// --- 데이터 모델 ---

type ShareType string

const (
	PublicShare ShareType = "public"
	EmailShare  ShareType = "email"
)

type PublicDashboard struct {
	UID                  string
	DashboardUID         string
	OrgID                int64
	AccessToken          string
	IsEnabled            bool
	Share                ShareType
	TimeSelectionEnabled bool
	AnnotationsEnabled   bool
	CreatedBy            int64
	CreatedAt            time.Time
}

type Dashboard struct {
	UID   string
	Title string
	OrgID int64
}

// --- Store ---

type PublicDashboardStore struct {
	mu          sync.RWMutex
	pubDash     map[string]*PublicDashboard // UID -> PublicDashboard
	tokenIndex  map[string]string           // AccessToken -> UID
	dashboards  map[string]*Dashboard       // DashboardUID -> Dashboard
}

func NewStore() *PublicDashboardStore {
	return &PublicDashboardStore{
		pubDash:    make(map[string]*PublicDashboard),
		tokenIndex: make(map[string]string),
		dashboards: make(map[string]*Dashboard),
	}
}

func generateAccessToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateUID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)[:12]
}

func (s *PublicDashboardStore) AddDashboard(d *Dashboard) {
	s.dashboards[d.UID] = d
}

func (s *PublicDashboardStore) Create(dashboardUID string, orgID int64, userID int64, share ShareType) (*PublicDashboard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 대시보드 존재 확인
	if _, ok := s.dashboards[dashboardUID]; !ok {
		return nil, fmt.Errorf("대시보드를 찾을 수 없음: %s", dashboardUID)
	}

	// 중복 확인
	for _, pd := range s.pubDash {
		if pd.DashboardUID == dashboardUID && pd.OrgID == orgID {
			return nil, fmt.Errorf("이미 공개 대시보드가 존재함")
		}
	}

	pd := &PublicDashboard{
		UID:                  generateUID(),
		DashboardUID:         dashboardUID,
		OrgID:                orgID,
		AccessToken:          generateAccessToken(),
		IsEnabled:            true,
		Share:                share,
		TimeSelectionEnabled: false,
		AnnotationsEnabled:   false,
		CreatedBy:            userID,
		CreatedAt:            time.Now(),
	}

	s.pubDash[pd.UID] = pd
	s.tokenIndex[pd.AccessToken] = pd.UID

	return pd, nil
}

func (s *PublicDashboardStore) GetByAccessToken(accessToken string) (*PublicDashboard, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	uid, ok := s.tokenIndex[accessToken]
	if !ok {
		return nil, false
	}
	pd, ok := s.pubDash[uid]
	return pd, ok
}

func (s *PublicDashboardStore) GetOrgIDByAccessToken(accessToken string) (int64, bool) {
	pd, ok := s.GetByAccessToken(accessToken)
	if !ok {
		return 0, false
	}
	return pd.OrgID, true
}

func (s *PublicDashboardStore) ExistsEnabledByAccessToken(accessToken string) bool {
	pd, ok := s.GetByAccessToken(accessToken)
	return ok && pd.IsEnabled
}

func (s *PublicDashboardStore) List(orgID int64) []*PublicDashboard {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*PublicDashboard
	for _, pd := range s.pubDash {
		if pd.OrgID == orgID {
			result = append(result, pd)
		}
	}
	return result
}

func (s *PublicDashboardStore) SetEnabled(uid string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pd, ok := s.pubDash[uid]
	if !ok {
		return fmt.Errorf("공개 대시보드를 찾을 수 없음")
	}
	pd.IsEnabled = enabled
	return nil
}

func (s *PublicDashboardStore) Delete(uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pd, ok := s.pubDash[uid]
	if !ok {
		return fmt.Errorf("공개 대시보드를 찾을 수 없음")
	}
	delete(s.tokenIndex, pd.AccessToken)
	delete(s.pubDash, uid)
	return nil
}

// --- 미들웨어 체인 ---

type RequestContext struct {
	AccessToken string
	OrgID       int64
	StatusCode  int
	Body        string
}

type Middleware func(ctx *RequestContext, next func())

func SetOrgIDMiddleware(store *PublicDashboardStore) Middleware {
	return func(ctx *RequestContext, next func()) {
		orgID, ok := store.GetOrgIDByAccessToken(ctx.AccessToken)
		if !ok {
			ctx.StatusCode = http.StatusNotFound
			ctx.Body = "공개 대시보드를 찾을 수 없음"
			return
		}
		ctx.OrgID = orgID
		next()
	}
}

func RequiresExistingAccessTokenMiddleware(store *PublicDashboardStore) Middleware {
	return func(ctx *RequestContext, next func()) {
		if !store.ExistsEnabledByAccessToken(ctx.AccessToken) {
			ctx.StatusCode = http.StatusNotFound
			ctx.Body = "비활성화된 공개 대시보드"
			return
		}
		next()
	}
}

func CountRequestMiddleware(counter *int) Middleware {
	return func(ctx *RequestContext, next func()) {
		*counter++
		next()
	}
}

func runMiddlewares(ctx *RequestContext, middlewares []Middleware, handler func(ctx *RequestContext)) {
	idx := 0
	var next func()
	next = func() {
		if idx < len(middlewares) {
			mw := middlewares[idx]
			idx++
			mw(ctx, next)
		} else {
			handler(ctx)
		}
	}
	next()
}

// --- 메인 ---

func main() {
	fmt.Println("=== Grafana 공개 대시보드 시뮬레이션 ===")
	fmt.Println()

	store := NewStore()

	// 대시보드 등록
	store.AddDashboard(&Dashboard{UID: "srv-001", Title: "서버 모니터링", OrgID: 1})
	store.AddDashboard(&Dashboard{UID: "net-001", Title: "네트워크 상태", OrgID: 1})
	store.AddDashboard(&Dashboard{UID: "db-001", Title: "데이터베이스", OrgID: 2})

	// 1. 공개 대시보드 생성
	fmt.Println("--- 1. 공개 대시보드 생성 ---")
	pd1, _ := store.Create("srv-001", 1, 100, PublicShare)
	fmt.Printf("  대시보드: srv-001\n")
	fmt.Printf("  AccessToken: %s\n", pd1.AccessToken)
	fmt.Printf("  ShareType: %s\n", pd1.Share)
	fmt.Printf("  공개 URL: /public/dashboards/%s\n", pd1.AccessToken)

	pd2, _ := store.Create("net-001", 1, 100, PublicShare)
	fmt.Printf("\n  대시보드: net-001, Token: %s...\n", pd2.AccessToken[:16])

	// 2. 중복 생성 시도
	fmt.Println()
	fmt.Println("--- 2. 중복 생성 시도 ---")
	_, err := store.Create("srv-001", 1, 100, PublicShare)
	fmt.Printf("  결과: %v\n", err)

	// 3. 미들웨어 체인을 통한 공개 대시보드 접근
	fmt.Println()
	fmt.Println("--- 3. 미들웨어 체인 테스트 ---")

	requestCount := 0
	middlewares := []Middleware{
		CountRequestMiddleware(&requestCount),
		SetOrgIDMiddleware(store),
		RequiresExistingAccessTokenMiddleware(store),
	}

	handler := func(ctx *RequestContext) {
		pd, _ := store.GetByAccessToken(ctx.AccessToken)
		ctx.StatusCode = http.StatusOK
		ctx.Body = fmt.Sprintf("대시보드 표시: %s (OrgID: %d)", pd.DashboardUID, ctx.OrgID)
	}

	// 정상 접근
	ctx1 := &RequestContext{AccessToken: pd1.AccessToken}
	runMiddlewares(ctx1, middlewares, handler)
	fmt.Printf("  [%d] %s\n", ctx1.StatusCode, ctx1.Body)

	// 잘못된 토큰
	ctx2 := &RequestContext{AccessToken: "invalid-token"}
	runMiddlewares(ctx2, middlewares, handler)
	fmt.Printf("  [%d] %s\n", ctx2.StatusCode, ctx2.Body)

	// 4. 비활성화 후 접근
	fmt.Println()
	fmt.Println("--- 4. 비활성화 후 접근 ---")
	store.SetEnabled(pd1.UID, false)
	ctx3 := &RequestContext{AccessToken: pd1.AccessToken}
	runMiddlewares(ctx3, middlewares, handler)
	fmt.Printf("  [%d] %s\n", ctx3.StatusCode, ctx3.Body)

	// 다시 활성화
	store.SetEnabled(pd1.UID, true)
	ctx4 := &RequestContext{AccessToken: pd1.AccessToken}
	runMiddlewares(ctx4, middlewares, handler)
	fmt.Printf("  재활성화 후: [%d] %s\n", ctx4.StatusCode, ctx4.Body)

	// 5. 목록 조회
	fmt.Println()
	fmt.Println("--- 5. 조직별 공개 대시보드 목록 ---")
	list := store.List(1)
	fmt.Printf("  OrgID=1: %d개\n", len(list))
	for _, pd := range list {
		enabledStr := "활성"
		if !pd.IsEnabled {
			enabledStr = "비활성"
		}
		fmt.Printf("    - %s → %s [%s] (%s)\n",
			pd.DashboardUID, pd.AccessToken[:12]+"...",
			pd.Share, enabledStr)
	}

	// 6. 스냅샷 vs 공개 대시보드 비교
	fmt.Println()
	fmt.Println("--- 6. 스냅샷 vs 공개 대시보드 비교 ---")
	fmt.Println("  " + strings.Repeat("-", 60))
	fmt.Printf("  %-20s %-20s %-20s\n", "항목", "스냅샷", "공개 대시보드")
	fmt.Println("  " + strings.Repeat("-", 60))
	fmt.Printf("  %-20s %-20s %-20s\n", "데이터", "정적 캡처", "실시간 쿼리")
	fmt.Printf("  %-20s %-20s %-20s\n", "접근 방식", "Key", "AccessToken")
	fmt.Printf("  %-20s %-20s %-20s\n", "만료", "설정 가능", "수동 비활성화")
	fmt.Printf("  %-20s %-20s %-20s\n", "외부 서버", "지원", "미지원")
	fmt.Printf("  %-20s %-20s %-20s\n", "데이터 저장", "암호화 저장", "참조만 저장")
	fmt.Println("  " + strings.Repeat("-", 60))

	// 7. 요청 카운터
	fmt.Println()
	fmt.Printf("--- 7. 총 요청 수: %d ---\n", requestCount)

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
