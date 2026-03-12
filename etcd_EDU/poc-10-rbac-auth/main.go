// poc-10-rbac-auth: etcd RBAC 인증/인가 시뮬레이션
//
// etcd는 Role-Based Access Control(RBAC)로 키 공간에 대한 접근을 제어한다.
// 실제 구현 (server/auth/store.go):
// - AuthStore 인터페이스: UserAdd, RoleAdd, UserGrantRole, Authenticate, IsPutPermitted 등
// - User: name + passwordHash (bcrypt)
// - Role: name + Permission[] (key range + PermType)
// - Permission: key~rangeEnd 범위에 대한 READ/WRITE/READWRITE
// - 인증 시 simple token 또는 JWT 발급
//
// 이 PoC는 bcrypt 대신 SHA256으로 패스워드 해싱하고,
// 키 범위 기반 권한 검사를 재현한다.
//
// 사용법: go run main.go

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ===== 에러 정의 =====
// server/auth/store.go의 에러 정의 참조

var (
	ErrUserAlreadyExist = errors.New("auth: 사용자가 이미 존재함")
	ErrUserNotFound     = errors.New("auth: 사용자를 찾을 수 없음")
	ErrUserEmpty        = errors.New("auth: 사용자 이름이 비어있음")
	ErrRoleAlreadyExist = errors.New("auth: 역할이 이미 존재함")
	ErrRoleNotFound     = errors.New("auth: 역할을 찾을 수 없음")
	ErrRoleNotGranted   = errors.New("auth: 역할이 사용자에게 부여되지 않음")
	ErrAuthFailed       = errors.New("auth: 인증 실패, 잘못된 사용자 ID 또는 비밀번호")
	ErrPermissionDenied = errors.New("auth: 권한 거부")
	ErrAuthNotEnabled   = errors.New("auth: 인증이 활성화되지 않음")
	ErrInvalidToken     = errors.New("auth: 유효하지 않은 인증 토큰")
)

// ===== Permission =====
// etcd의 authpb.Permission 재현
// 키 범위(key~rangeEnd)에 대한 접근 권한 유형을 정의한다.

type PermType int

const (
	PermRead      PermType = 0
	PermWrite     PermType = 1
	PermReadWrite PermType = 2
)

func (p PermType) String() string {
	switch p {
	case PermRead:
		return "READ"
	case PermWrite:
		return "WRITE"
	case PermReadWrite:
		return "READWRITE"
	default:
		return "UNKNOWN"
	}
}

// Permission은 키 범위에 대한 접근 권한이다.
// Key: 시작 키, RangeEnd: 끝 키 (빈 값이면 단일 키, "\x00"이면 전체)
type Permission struct {
	PermType PermType
	Key      string
	RangeEnd string // 빈 문자열: 단일 키, "\x00": 모든 키
}

// covers는 주어진 키가 이 권한의 범위에 포함되는지 확인한다.
// etcd의 range_perm_cache.go의 키 범위 검사 로직 재현
func (p *Permission) covers(key string) bool {
	if p.RangeEnd == "" {
		// 단일 키 매칭
		return p.Key == key
	}
	if p.RangeEnd == "\x00" {
		// 모든 키
		return true
	}
	// 범위 매칭: key <= target < rangeEnd
	return key >= p.Key && key < p.RangeEnd
}

// hasReadPerm은 읽기 권한이 있는지 확인한다.
func (p *Permission) hasReadPerm() bool {
	return p.PermType == PermRead || p.PermType == PermReadWrite
}

// hasWritePerm은 쓰기 권한이 있는지 확인한다.
func (p *Permission) hasWritePerm() bool {
	return p.PermType == PermWrite || p.PermType == PermReadWrite
}

// ===== Role =====
// etcd의 authpb.Role 재현

type Role struct {
	Name        string
	Permissions []Permission
}

// ===== User =====
// etcd의 authpb.User 재현
// 실제 etcd는 bcrypt로 패스워드 해싱하지만, 이 PoC에서는 SHA256을 사용한다.

type User struct {
	Name         string
	PasswordHash string   // SHA256 해시
	Roles        []string // 부여된 역할 이름 목록
}

// ===== SimpleToken =====
// etcd의 simple_token.go 재현
// 인증 성공 시 발급되는 간단한 토큰

type SimpleToken struct {
	Token    string
	Username string
	ExpireAt time.Time
}

// ===== AuthStore =====
// etcd의 AuthStore 인터페이스 재현 (server/auth/store.go)

type AuthStore struct {
	mu        sync.RWMutex
	enabled   bool
	users     map[string]*User
	roles     map[string]*Role
	tokens    map[string]*SimpleToken
	revision  uint64
}

func NewAuthStore() *AuthStore {
	return &AuthStore{
		users:  make(map[string]*User),
		roles:  make(map[string]*Role),
		tokens: make(map[string]*SimpleToken),
	}
}

// hashPassword는 SHA256으로 패스워드를 해싱한다.
// 실제 etcd는 bcrypt를 사용한다 (server/auth/store.go).
func hashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

// generateToken은 간단한 인증 토큰을 생성한다.
// etcd의 simple_token.go에서는 랜덤 문자열을 생성한다.
func generateToken() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// ===== AuthStore 메서드 =====

// AuthEnable은 인증을 활성화한다.
// etcd에서는 root 사용자와 root 역할이 반드시 존재해야 한다.
func (as *AuthStore) AuthEnable() error {
	as.mu.Lock()
	defer as.mu.Unlock()

	// root 사용자 존재 확인
	rootUser, ok := as.users["root"]
	if !ok {
		return errors.New("auth: root 사용자가 존재하지 않음")
	}

	// root 사용자에게 root 역할이 부여되었는지 확인
	hasRootRole := false
	for _, role := range rootUser.Roles {
		if role == "root" {
			hasRootRole = true
			break
		}
	}
	if !hasRootRole {
		return errors.New("auth: root 사용자에게 root 역할이 없음")
	}

	as.enabled = true
	as.revision++
	return nil
}

// UserAdd는 새 사용자를 추가한다.
func (as *AuthStore) UserAdd(name, password string) error {
	as.mu.Lock()
	defer as.mu.Unlock()

	if name == "" {
		return ErrUserEmpty
	}
	if _, exists := as.users[name]; exists {
		return ErrUserAlreadyExist
	}

	as.users[name] = &User{
		Name:         name,
		PasswordHash: hashPassword(password),
		Roles:        []string{},
	}
	as.revision++
	return nil
}

// RoleAdd는 새 역할을 추가한다.
func (as *AuthStore) RoleAdd(name string) error {
	as.mu.Lock()
	defer as.mu.Unlock()

	if _, exists := as.roles[name]; exists {
		return ErrRoleAlreadyExist
	}

	as.roles[name] = &Role{
		Name:        name,
		Permissions: []Permission{},
	}

	// root 역할은 자동으로 전체 읽기/쓰기 권한을 가진다
	// etcd의 rootPerm 정의: Key: [], RangeEnd: [0] → 모든 키
	if name == "root" {
		as.roles[name].Permissions = append(as.roles[name].Permissions, Permission{
			PermType: PermReadWrite,
			Key:      "",
			RangeEnd: "\x00", // 모든 키
		})
	}

	as.revision++
	return nil
}

// RoleGrantPermission은 역할에 권한을 부여한다.
func (as *AuthStore) RoleGrantPermission(roleName string, perm Permission) error {
	as.mu.Lock()
	defer as.mu.Unlock()

	role, ok := as.roles[roleName]
	if !ok {
		return ErrRoleNotFound
	}

	role.Permissions = append(role.Permissions, perm)
	as.revision++
	return nil
}

// UserGrantRole은 사용자에게 역할을 부여한다.
func (as *AuthStore) UserGrantRole(userName, roleName string) error {
	as.mu.Lock()
	defer as.mu.Unlock()

	user, ok := as.users[userName]
	if !ok {
		return ErrUserNotFound
	}

	if _, ok := as.roles[roleName]; !ok {
		return ErrRoleNotFound
	}

	// 중복 부여 방지
	for _, r := range user.Roles {
		if r == roleName {
			return nil // 이미 부여됨
		}
	}

	user.Roles = append(user.Roles, roleName)
	as.revision++
	return nil
}

// Authenticate는 사용자 인증을 수행하고 토큰을 반환한다.
// etcd의 Authenticate() (server/auth/store.go) 재현
func (as *AuthStore) Authenticate(username, password string) (string, error) {
	as.mu.Lock()
	defer as.mu.Unlock()

	if !as.enabled {
		return "", ErrAuthNotEnabled
	}

	user, ok := as.users[username]
	if !ok {
		return "", ErrAuthFailed
	}

	// 패스워드 검증
	if user.PasswordHash != hashPassword(password) {
		return "", ErrAuthFailed
	}

	// 토큰 생성 (etcd simple_token.go 방식)
	token := generateToken()
	as.tokens[token] = &SimpleToken{
		Token:    token,
		Username: username,
		ExpireAt: time.Now().Add(5 * time.Minute),
	}

	return token, nil
}

// TokenToUsername은 토큰에서 사용자 이름을 추출한다.
func (as *AuthStore) TokenToUsername(token string) (string, error) {
	as.mu.RLock()
	defer as.mu.RUnlock()

	st, ok := as.tokens[token]
	if !ok {
		return "", ErrInvalidToken
	}

	if time.Now().After(st.ExpireAt) {
		delete(as.tokens, token)
		return "", ErrInvalidToken
	}

	return st.Username, nil
}

// IsPermitted는 사용자가 주어진 키에 대해 지정된 작업을 수행할 수 있는지 확인한다.
// etcd의 IsPutPermitted/IsRangePermitted 재현
func (as *AuthStore) IsPermitted(username, key string, needWrite bool) error {
	as.mu.RLock()
	defer as.mu.RUnlock()

	user, ok := as.users[username]
	if !ok {
		return ErrUserNotFound
	}

	for _, roleName := range user.Roles {
		role, ok := as.roles[roleName]
		if !ok {
			continue
		}

		for _, perm := range role.Permissions {
			if !perm.covers(key) {
				continue
			}

			if needWrite && perm.hasWritePerm() {
				return nil
			}
			if !needWrite && perm.hasReadPerm() {
				return nil
			}
		}
	}

	return ErrPermissionDenied
}

// UserList는 모든 사용자 목록을 반환한다.
func (as *AuthStore) UserList() []string {
	as.mu.RLock()
	defer as.mu.RUnlock()

	users := make([]string, 0, len(as.users))
	for name := range as.users {
		users = append(users, name)
	}
	return users
}

// RoleList는 모든 역할 목록을 반환한다.
func (as *AuthStore) RoleList() []string {
	as.mu.RLock()
	defer as.mu.RUnlock()

	roles := make([]string, 0, len(as.roles))
	for name := range as.roles {
		roles = append(roles, name)
	}
	return roles
}

// ===== 메인 =====

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║  etcd RBAC 인증/인가 PoC                                ║")
	fmt.Println("║  역할 기반 접근 제어 시뮬레이션                         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")

	store := NewAuthStore()

	// ========================================
	// 1. 사용자 및 역할 생성
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("1단계: 사용자 및 역할 생성")
	fmt.Println(strings.Repeat("─", 55))

	// root 사용자/역할 생성 (etcd 필수 요구사항)
	store.UserAdd("root", "rootpassword")
	store.RoleAdd("root")
	store.UserGrantRole("root", "root")
	fmt.Println("  ✓ root 사용자 생성 + root 역할 부여 (전체 권한)")

	// 일반 사용자 생성
	store.UserAdd("alice", "alice123")
	store.UserAdd("bob", "bob456")
	store.UserAdd("charlie", "charlie789")
	fmt.Println("  ✓ 일반 사용자 생성: alice, bob, charlie")

	// 역할 생성
	store.RoleAdd("app-reader")
	store.RoleAdd("app-writer")
	store.RoleAdd("infra-admin")
	fmt.Println("  ✓ 역할 생성: app-reader, app-writer, infra-admin")

	// ========================================
	// 2. 역할에 권한 부여
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("2단계: 역할에 권한 부여")
	fmt.Println(strings.Repeat("─", 55))

	// app-reader: /app/ 프리픽스 읽기 전용
	store.RoleGrantPermission("app-reader", Permission{
		PermType: PermRead,
		Key:      "/app/",
		RangeEnd: "/app0", // /app/ 프리픽스 매칭 (사전순으로 '/' 다음이 '0')
	})
	fmt.Println("  app-reader: /app/* 읽기 권한")

	// app-writer: /app/ 프리픽스 읽기+쓰기
	store.RoleGrantPermission("app-writer", Permission{
		PermType: PermReadWrite,
		Key:      "/app/",
		RangeEnd: "/app0",
	})
	fmt.Println("  app-writer: /app/* 읽기+쓰기 권한")

	// infra-admin: /infra/ 프리픽스 전체 권한 + /config 단일 키 쓰기
	store.RoleGrantPermission("infra-admin", Permission{
		PermType: PermReadWrite,
		Key:      "/infra/",
		RangeEnd: "/infra0",
	})
	store.RoleGrantPermission("infra-admin", Permission{
		PermType: PermWrite,
		Key:      "/config",
		RangeEnd: "", // 단일 키
	})
	fmt.Println("  infra-admin: /infra/* 읽기+쓰기, /config 쓰기 권한")

	// ========================================
	// 3. 사용자에게 역할 부여
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("3단계: 사용자에게 역할 부여")
	fmt.Println(strings.Repeat("─", 55))

	store.UserGrantRole("alice", "app-reader")
	fmt.Println("  alice ← app-reader (읽기 전용)")

	store.UserGrantRole("bob", "app-writer")
	fmt.Println("  bob ← app-writer (읽기+쓰기)")

	store.UserGrantRole("charlie", "app-reader")
	store.UserGrantRole("charlie", "infra-admin")
	fmt.Println("  charlie ← app-reader + infra-admin (다중 역할)")

	// ========================================
	// 4. 인증 활성화 및 토큰 발급
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("4단계: 인증 활성화 및 토큰 발급")
	fmt.Println(strings.Repeat("─", 55))

	if err := store.AuthEnable(); err != nil {
		fmt.Printf("  인증 활성화 실패: %v\n", err)
	} else {
		fmt.Println("  ✓ 인증 활성화 완료")
	}

	// root 인증
	rootToken, err := store.Authenticate("root", "rootpassword")
	if err != nil {
		fmt.Printf("  root 인증 실패: %v\n", err)
	} else {
		fmt.Printf("  ✓ root 인증 성공, 토큰: %s\n", rootToken)
	}

	// alice 인증
	aliceToken, err := store.Authenticate("alice", "alice123")
	if err != nil {
		fmt.Printf("  alice 인증 실패: %v\n", err)
	} else {
		fmt.Printf("  ✓ alice 인증 성공, 토큰: %s\n", aliceToken)
	}

	// 잘못된 비밀번호
	_, err = store.Authenticate("bob", "wrongpass")
	fmt.Printf("  bob 잘못된 비밀번호: %v\n", err)

	// 존재하지 않는 사용자
	_, err = store.Authenticate("unknown", "pass")
	fmt.Printf("  unknown 사용자: %v\n", err)

	// ========================================
	// 5. 토큰 → 사용자 확인
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("5단계: 토큰 검증")
	fmt.Println(strings.Repeat("─", 55))

	if username, err := store.TokenToUsername(aliceToken); err == nil {
		fmt.Printf("  토큰 %s → 사용자: %s\n", aliceToken, username)
	}

	_, err = store.TokenToUsername("invalid-token-xyz")
	fmt.Printf("  유효하지 않은 토큰: %v\n", err)

	// ========================================
	// 6. 권한 검사 — 허용/거부 테스트
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("6단계: 권한 검사 (허용/거부)")
	fmt.Println(strings.Repeat("─", 55))

	type permTest struct {
		user      string
		key       string
		needWrite bool
		desc      string
	}

	tests := []permTest{
		// root: 모든 권한
		{"root", "/app/config", false, "root 읽기 /app/config"},
		{"root", "/app/config", true, "root 쓰기 /app/config"},
		{"root", "/any/key", true, "root 쓰기 /any/key"},

		// alice: app-reader (읽기 전용)
		{"alice", "/app/data", false, "alice 읽기 /app/data"},
		{"alice", "/app/data", true, "alice 쓰기 /app/data"},     // 거부
		{"alice", "/infra/db", false, "alice 읽기 /infra/db"},     // 거부

		// bob: app-writer (읽기+쓰기)
		{"bob", "/app/service", false, "bob 읽기 /app/service"},
		{"bob", "/app/service", true, "bob 쓰기 /app/service"},
		{"bob", "/infra/db", true, "bob 쓰기 /infra/db"},         // 거부

		// charlie: app-reader + infra-admin
		{"charlie", "/app/data", false, "charlie 읽기 /app/data"},
		{"charlie", "/app/data", true, "charlie 쓰기 /app/data"},   // 거부 (app-reader)
		{"charlie", "/infra/db", true, "charlie 쓰기 /infra/db"},   // 허용 (infra-admin)
		{"charlie", "/config", true, "charlie 쓰기 /config"},       // 허용 (infra-admin 단일키)
		{"charlie", "/config", false, "charlie 읽기 /config"},       // 거부 (쓰기만 허용)
	}

	for _, t := range tests {
		action := "읽기"
		if t.needWrite {
			action = "쓰기"
		}
		err := store.IsPermitted(t.user, t.key, t.needWrite)
		if err == nil {
			fmt.Printf("  ✓ 허용: %s — %s %s %s\n", t.desc, t.user, action, t.key)
		} else {
			fmt.Printf("  ✗ 거부: %s — %v\n", t.desc, err)
		}
	}

	// ========================================
	// 7. 키 범위 권한 매칭
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("7단계: 키 범위 권한 매칭 원리")
	fmt.Println(strings.Repeat("─", 55))

	fmt.Println("  etcd 권한의 키 범위 매칭:")
	fmt.Println("  ┌─────────────────────────────────────────────────────┐")
	fmt.Println("  │ Key=\"/app/\", RangeEnd=\"\"     → 단일 키 \"/app/\"    │")
	fmt.Println("  │ Key=\"/app/\", RangeEnd=\"/app0\" → 프리픽스 /app/*    │")
	fmt.Println("  │ Key=\"\",     RangeEnd=\"\\x00\"  → 모든 키 (root)     │")
	fmt.Println("  └─────────────────────────────────────────────────────┘")

	// 범위 매칭 테스트
	perm := Permission{PermType: PermRead, Key: "/app/", RangeEnd: "/app0"}
	testKeys := []string{"/app/config", "/app/data/db", "/application", "/ap", "/infra/db"}
	fmt.Println("\n  범위 [/app/, /app0) 매칭 테스트:")
	for _, key := range testKeys {
		matched := perm.covers(key)
		marker := "✗"
		if matched {
			marker = "✓"
		}
		fmt.Printf("    %s %s\n", marker, key)
	}

	// ========================================
	// 8. 에러 케이스
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("8단계: 에러 케이스")
	fmt.Println(strings.Repeat("─", 55))

	err = store.UserAdd("alice", "newpass")
	fmt.Printf("  중복 사용자 생성: %v\n", err)

	err = store.RoleAdd("root")
	fmt.Printf("  중복 역할 생성: %v\n", err)

	err = store.UserGrantRole("alice", "nonexistent")
	fmt.Printf("  존재하지 않는 역할 부여: %v\n", err)

	err = store.UserGrantRole("unknown", "app-reader")
	fmt.Printf("  존재하지 않는 사용자에 역할 부여: %v\n", err)

	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("✓ RBAC 인증/인가 PoC 완료")
	fmt.Println("  - 사용자/역할 생성 및 권한 부여")
	fmt.Println("  - 패스워드 기반 인증 + 토큰 발급")
	fmt.Println("  - 키 범위 기반 권한 검사 (허용/거부)")
	fmt.Println("  - root 역할의 전체 권한, 다중 역할 지원")
}
