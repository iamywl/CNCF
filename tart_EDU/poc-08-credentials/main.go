package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 다중 인증 프로바이더 체인 시뮬레이션
//
// tart의 Credentials/ 디렉토리 구현을 Go로 재현한다.
//
// 핵심 개념:
//   - CredentialsProvider 프로토콜: Retrieve(host) -> (user, pass, error)
//   - EnvironmentProvider: 환경변수에서 자격증명 읽기
//   - DockerConfigProvider: ~/.docker/config.json 파싱
//   - KeychainProvider: 인메모리 키체인 시뮬레이션
//   - 체인 실행: 순서대로 시도, 첫 번째 성공 반환
//   - Bearer 토큰 요청 및 만료 관리
//
// 참조: tart/Sources/tart/Credentials/CredentialsProvider.swift
//       tart/Sources/tart/Credentials/EnvironmentCredentialsProvider.swift
//       tart/Sources/tart/Credentials/DockerConfigCredentialsProvider.swift
//       tart/Sources/tart/Credentials/KeychainCredentialsProvider.swift
//       tart/Sources/tart/OCI/Authentication.swift
//       tart/Sources/tart/OCI/AuthenticationKeeper.swift
// =============================================================================

// --- CredentialsProvider 인터페이스 (tart CredentialsProvider.swift 참조) ---

// CredentialsProvider는 호스트별 자격증명을 검색하는 인터페이스이다.
// tart: protocol CredentialsProvider {
//   var userFriendlyName: String { get }
//   func retrieve(host: String) throws -> (String, String)?
//   func store(host: String, user: String, password: String) throws
// }
type CredentialsProvider interface {
	Name() string
	Retrieve(host string) (string, string, error) // (user, password, error)
	Store(host, user, password string) error
}

// --- EnvironmentCredentialsProvider (tart EnvironmentCredentialsProvider.swift 참조) ---

// EnvironmentCredentialsProvider는 환경변수에서 자격증명을 읽는다.
// tart의 환경변수:
//   - TART_REGISTRY_HOSTNAME: 대상 호스트 필터 (설정 시 해당 호스트만 매칭)
//   - TART_REGISTRY_USERNAME: 사용자 이름
//   - TART_REGISTRY_PASSWORD: 비밀번호
type EnvironmentCredentialsProvider struct {
	// 테스트를 위해 실제 환경변수 대신 맵을 사용
	envVars map[string]string
}

func NewEnvironmentCredentialsProvider(envVars map[string]string) *EnvironmentCredentialsProvider {
	return &EnvironmentCredentialsProvider{envVars: envVars}
}

func (p *EnvironmentCredentialsProvider) Name() string {
	return "환경변수 인증 프로바이더"
}

// Retrieve는 환경변수에서 자격증명을 읽는다.
// tart: TART_REGISTRY_HOSTNAME이 설정되어 있고 host와 다르면 nil 반환.
// TART_REGISTRY_USERNAME + TART_REGISTRY_PASSWORD 둘 다 있어야 반환.
func (p *EnvironmentCredentialsProvider) Retrieve(host string) (string, string, error) {
	// tart: if let tartRegistryHostname = env["TART_REGISTRY_HOSTNAME"],
	//       tartRegistryHostname != host { return nil }
	if hostname, ok := p.envVars["TART_REGISTRY_HOSTNAME"]; ok {
		if hostname != host {
			return "", "", nil
		}
	}

	username := p.envVars["TART_REGISTRY_USERNAME"]
	password := p.envVars["TART_REGISTRY_PASSWORD"]

	if username != "" && password != "" {
		return username, password, nil
	}

	return "", "", nil
}

func (p *EnvironmentCredentialsProvider) Store(host, user, password string) error {
	// tart: no-op (환경변수는 저장 불가)
	return nil
}

// --- DockerConfigCredentialsProvider (tart DockerConfigCredentialsProvider.swift 참조) ---

// DockerConfig는 ~/.docker/config.json 구조이다.
// tart의 DockerConfig 구조체를 재현.
type DockerConfig struct {
	Auths       map[string]DockerAuthConfig `json:"auths"`
	CredHelpers map[string]string           `json:"credHelpers"`
}

// DockerAuthConfig는 개별 호스트의 인증 설정이다.
// tart의 DockerAuthConfig: auth 필드는 base64("user:password")
type DockerAuthConfig struct {
	Auth string `json:"auth,omitempty"`
}

// DecodeCredentials는 base64 인코딩된 auth 필드를 디코딩한다.
// tart: func decodeCredentials() -> (String, String)?
//   auth = base64("username:password") → split(":") → (username, password)
func (c *DockerAuthConfig) DecodeCredentials() (string, string, bool) {
	if c.Auth == "" {
		return "", "", false
	}

	decoded, err := base64.StdEncoding.DecodeString(c.Auth)
	if err != nil {
		return "", "", false
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}

	return parts[0], parts[1], true
}

// DockerConfigCredentialsProvider는 Docker config.json에서 자격증명을 읽는다.
// tart의 DockerConfigCredentialsProvider 클래스를 재현:
//   1) auths[host].auth → base64 디코딩 → (user, password)
//   2) credHelpers[host] → 외부 헬퍼 프로그램 실행 (여기서는 시뮬레이션)
type DockerConfigCredentialsProvider struct {
	configPath string
}

func NewDockerConfigCredentialsProvider(configPath string) *DockerConfigCredentialsProvider {
	return &DockerConfigCredentialsProvider{configPath: configPath}
}

func (p *DockerConfigCredentialsProvider) Name() string {
	return "Docker 설정 인증 프로바이더"
}

// Retrieve는 Docker config.json에서 자격증명을 읽는다.
// tart: 1) config.auths[host]?.decodeCredentials()
//       2) config.findCredHelper(host:) → executeHelper(binaryName:host:)
func (p *DockerConfigCredentialsProvider) Retrieve(host string) (string, string, error) {
	// config.json 읽기 (tart: FileManager.default.fileExists + Data(contentsOf:))
	data, err := os.ReadFile(p.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil // 파일 없으면 nil (tart: return nil)
		}
		return "", "", fmt.Errorf("config.json 읽기 실패: %w", err)
	}

	var config DockerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return "", "", fmt.Errorf("config.json 파싱 실패: %w", err)
	}

	// 1단계: auths에서 직접 자격증명 검색
	// tart: config.auths?[host]?.decodeCredentials()
	if authConfig, ok := config.Auths[host]; ok {
		if user, pass, ok := authConfig.DecodeCredentials(); ok {
			return user, pass, nil
		}
	}

	// 2단계: credHelpers에서 헬퍼 프로그램 검색
	// tart: config.findCredHelper(host:) → 정확 매칭 또는 정규식 매칭
	// 실제 tart에서는 docker-credential-{helper} 바이너리를 실행하지만
	// 여기서는 시뮬레이션만 수행
	if config.CredHelpers != nil {
		for pattern, helper := range config.CredHelpers {
			if pattern == host || matchWildcard(pattern, host) {
				fmt.Printf("    [DockerConfig] credHelper 발견: %s -> docker-credential-%s\n", pattern, helper)
				fmt.Printf("    [DockerConfig] (시뮬레이션: 실제로는 docker-credential-%s get 실행)\n", helper)
				return "", "", nil
			}
		}
	}

	return "", "", nil
}

func (p *DockerConfigCredentialsProvider) Store(host, user, password string) error {
	return fmt.Errorf("Docker 설정 프로바이더는 저장을 지원하지 않음")
}

// matchWildcard는 간단한 와일드카드 매칭을 수행한다.
// tart: compiledPattern = try? Regex(hostPattern); compiledPattern?.wholeMatch(in: host)
func matchWildcard(pattern, host string) bool {
	if strings.Contains(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(host, prefix)
	}
	return false
}

// --- KeychainCredentialsProvider (tart KeychainCredentialsProvider.swift 참조) ---

// KeychainEntry는 키체인의 개별 항목이다.
type KeychainEntry struct {
	User     string
	Password string
}

// KeychainCredentialsProvider는 macOS Keychain을 인메모리로 시뮬레이션한다.
// tart의 KeychainCredentialsProvider 클래스:
//   - SecItemCopyMatching: 키체인 항목 검색
//   - SecItemAdd: 새 항목 추가
//   - SecItemUpdate: 기존 항목 갱신
//   - SecItemDelete: 항목 삭제
//   - 검색 조건: kSecClass=InternetPassword, kSecAttrProtocol=HTTPS,
//     kSecAttrServer=host, kSecAttrLabel="Tart Credentials"
type KeychainCredentialsProvider struct {
	mu       sync.RWMutex
	keychain map[string]*KeychainEntry // host -> entry
}

func NewKeychainCredentialsProvider() *KeychainCredentialsProvider {
	return &KeychainCredentialsProvider{
		keychain: make(map[string]*KeychainEntry),
	}
}

func (p *KeychainCredentialsProvider) Name() string {
	return "Keychain 인증 프로바이더"
}

// Retrieve는 키체인에서 자격증명을 검색한다.
// tart: SecItemCopyMatching(query) → errSecSuccess: 반환, errSecItemNotFound: nil
func (p *KeychainCredentialsProvider) Retrieve(host string) (string, string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	entry, ok := p.keychain[host]
	if !ok {
		return "", "", nil // tart: errSecItemNotFound → return nil
	}

	return entry.User, entry.Password, nil
}

// Store는 키체인에 자격증명을 저장한다.
// tart: SecItemCopyMatching → errSecItemNotFound → SecItemAdd
//       errSecSuccess → SecItemUpdate
func (p *KeychainCredentialsProvider) Store(host, user, password string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.keychain[host]; ok {
		// 기존 항목 갱신 (tart: SecItemUpdate)
		p.keychain[host].User = user
		p.keychain[host].Password = password
		fmt.Printf("    [Keychain] 기존 항목 갱신: %s\n", host)
	} else {
		// 새 항목 추가 (tart: SecItemAdd)
		p.keychain[host] = &KeychainEntry{User: user, Password: password}
		fmt.Printf("    [Keychain] 새 항목 추가: %s\n", host)
	}

	return nil
}

// Remove는 키체인에서 항목을 삭제한다.
// tart: SecItemDelete → errSecSuccess/errSecItemNotFound
func (p *KeychainCredentialsProvider) Remove(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.keychain, host)
}

// --- Authentication 인터페이스 (tart Authentication.swift 참조) ---

// Authentication은 HTTP 요청에 인증 헤더를 추가하는 인터페이스이다.
// tart: protocol Authentication { func header() -> (String, String); func isValid() -> Bool }
type Authentication interface {
	Header() (string, string) // (헤더이름, 헤더값)
	IsValid() bool
}

// BasicAuthentication은 HTTP Basic 인증이다.
// tart: struct BasicAuthentication: Authentication
type BasicAuthentication struct {
	User     string
	Password string
}

func (a *BasicAuthentication) Header() (string, string) {
	// tart: Data("\(user):\(password)".utf8).base64EncodedString()
	creds := base64.StdEncoding.EncodeToString([]byte(a.User + ":" + a.Password))
	return "Authorization", "Basic " + creds
}

func (a *BasicAuthentication) IsValid() bool {
	return true // Basic 인증은 만료 없음
}

// BearerAuthentication은 Bearer 토큰 인증이다.
// tart의 TokenResponse를 기반으로 한 인증.
type BearerAuthentication struct {
	Token     string
	ExpiresAt time.Time
}

func (a *BearerAuthentication) Header() (string, string) {
	return "Authorization", "Bearer " + a.Token
}

// IsValid는 토큰 만료 여부를 확인한다.
// tart: func isValid() -> Bool { Date() < tokenExpiresAt }
func (a *BearerAuthentication) IsValid() bool {
	return time.Now().Before(a.ExpiresAt)
}

// --- AuthenticationKeeper (tart AuthenticationKeeper.swift 참조) ---

// AuthenticationKeeper는 현재 인증 상태를 관리한다.
// tart: actor AuthenticationKeeper { var authentication: Authentication? }
// Go에서는 mutex로 thread-safety 보장.
type AuthenticationKeeper struct {
	mu   sync.RWMutex
	auth Authentication
}

// Set은 인증 상태를 설정한다.
func (k *AuthenticationKeeper) Set(auth Authentication) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.auth = auth
}

// Header는 유효한 인증 헤더를 반환한다.
// tart: if let auth, auth.isValid() → auth.header(), else → nil
func (k *AuthenticationKeeper) Header() (string, string, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()

	if k.auth == nil {
		return "", "", false
	}

	if !k.auth.IsValid() {
		return "", "", false // 만료된 토큰은 헤더 제공하지 않음
	}

	name, value := k.auth.Header()
	return name, value, true
}

// --- CredentialsChain: 프로바이더 체인 (tart Registry.lookupCredentials 참조) ---

// CredentialsChain은 여러 CredentialsProvider를 순서대로 시도한다.
// tart의 Registry.lookupCredentials() 메서드를 재현:
//   for provider in credentialsProviders {
//     do { if let (user, password) = try provider.retrieve(host:) { return (user, password) } }
//     catch { print("Failed to retrieve credentials using \(provider.userFriendlyName)") }
//   }
//   return nil
type CredentialsChain struct {
	Providers []CredentialsProvider
}

// Lookup은 체인의 프로바이더를 순서대로 시도하여 첫 번째 성공한 자격증명을 반환한다.
func (c *CredentialsChain) Lookup(host string) (string, string, string) {
	for _, provider := range c.Providers {
		user, pass, err := provider.Retrieve(host)
		if err != nil {
			// tart: print("Failed to retrieve credentials using \(provider.userFriendlyName)...")
			fmt.Printf("    [체인] %s에서 자격증명 검색 실패: %v\n", provider.Name(), err)
			continue
		}
		if user != "" && pass != "" {
			return user, pass, provider.Name()
		}
	}
	return "", "", ""
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== 다중 인증 프로바이더 체인 시뮬레이션 ===")
	fmt.Println("(tart Credentials/ 디렉토리, Authentication.swift, AuthenticationKeeper.swift 기반)")
	fmt.Println()

	// --- 1. CredentialsProvider 인터페이스 ---
	fmt.Println("========== 1. CredentialsProvider 인터페이스 ==========")
	fmt.Println()
	fmt.Println("tart의 CredentialsProvider 프로토콜:")
	fmt.Println("  protocol CredentialsProvider {")
	fmt.Println("    var userFriendlyName: String { get }")
	fmt.Println("    func retrieve(host: String) throws -> (String, String)?")
	fmt.Println("    func store(host: String, user: String, password: String) throws")
	fmt.Println("  }")
	fmt.Println()
	fmt.Println("tart 기본 프로바이더 체인 (Registry.swift init):")
	fmt.Println("  [EnvironmentCredentialsProvider(),")
	fmt.Println("   DockerConfigCredentialsProvider(),")
	fmt.Println("   KeychainCredentialsProvider()]")
	fmt.Println()

	// --- 2. EnvironmentCredentialsProvider 테스트 ---
	fmt.Println("========== 2. EnvironmentCredentialsProvider ==========")
	fmt.Println()

	// 환경변수 설정 시뮬레이션
	envProvider := NewEnvironmentCredentialsProvider(map[string]string{
		"TART_REGISTRY_USERNAME": "env-user",
		"TART_REGISTRY_PASSWORD": "env-pass123",
	})

	fmt.Printf("[%s]\n", envProvider.Name())

	user, pass, err := envProvider.Retrieve("ghcr.io")
	if err == nil && user != "" {
		fmt.Printf("  ghcr.io: user=%s, pass=%s\n", user, pass)
	}

	// TART_REGISTRY_HOSTNAME 필터 테스트
	envProviderFiltered := NewEnvironmentCredentialsProvider(map[string]string{
		"TART_REGISTRY_HOSTNAME": "ghcr.io",
		"TART_REGISTRY_USERNAME": "ghcr-user",
		"TART_REGISTRY_PASSWORD": "ghcr-pass",
	})

	fmt.Printf("\n[호스트 필터 테스트]\n")
	user, pass, _ = envProviderFiltered.Retrieve("ghcr.io")
	fmt.Printf("  ghcr.io 요청: user=%s (매칭)\n", user)

	user, pass, _ = envProviderFiltered.Retrieve("docker.io")
	if user == "" {
		fmt.Printf("  docker.io 요청: 자격증명 없음 (호스트 불일치)\n")
	}
	fmt.Println()

	// --- 3. DockerConfigCredentialsProvider 테스트 ---
	fmt.Println("========== 3. DockerConfigCredentialsProvider ==========")
	fmt.Println()

	// 임시 Docker config.json 생성
	tmpDir, _ := os.MkdirTemp("", "creds-poc")
	defer os.RemoveAll(tmpDir)

	configPath := filepath.Join(tmpDir, "config.json")

	// base64("docker-user:docker-pass") = "ZG9ja2VyLXVzZXI6ZG9ja2VyLXBhc3M="
	dockerConfig := DockerConfig{
		Auths: map[string]DockerAuthConfig{
			"ghcr.io": {
				Auth: base64.StdEncoding.EncodeToString([]byte("docker-user:docker-pass")),
			},
			"registry.example.com": {
				Auth: base64.StdEncoding.EncodeToString([]byte("example-user:example-pass")),
			},
		},
		CredHelpers: map[string]string{
			"gcr.io":             "gcloud",
			"*.azurecr.io":      "azure",
			"public.ecr.aws":    "ecr-login",
		},
	}

	configJSON, _ := json.MarshalIndent(dockerConfig, "", "  ")
	os.WriteFile(configPath, configJSON, 0644)

	fmt.Println("[Docker config.json 내용]:")
	fmt.Println(string(configJSON))
	fmt.Println()

	dockerProvider := NewDockerConfigCredentialsProvider(configPath)

	// auths에서 자격증명 검색
	fmt.Printf("[%s]\n", dockerProvider.Name())
	user, pass, err = dockerProvider.Retrieve("ghcr.io")
	if err == nil && user != "" {
		fmt.Printf("  ghcr.io (auths): user=%s, pass=%s\n", user, pass)
	}

	user, pass, err = dockerProvider.Retrieve("registry.example.com")
	if err == nil && user != "" {
		fmt.Printf("  registry.example.com (auths): user=%s, pass=%s\n", user, pass)
	}

	// credHelpers 매칭 테스트
	fmt.Println()
	fmt.Println("[credHelpers 매칭 테스트]:")
	dockerProvider.Retrieve("gcr.io")         // 정확 매칭
	dockerProvider.Retrieve("public.ecr.aws") // 정확 매칭
	fmt.Println()

	// --- 4. KeychainCredentialsProvider 테스트 ---
	fmt.Println("========== 4. KeychainCredentialsProvider ==========")
	fmt.Println()

	keychainProvider := NewKeychainCredentialsProvider()
	fmt.Printf("[%s]\n\n", keychainProvider.Name())

	// 검색 (비어있음)
	user, pass, _ = keychainProvider.Retrieve("ghcr.io")
	fmt.Printf("  ghcr.io (저장 전): user='%s' (비어있음)\n\n", user)

	// 저장 (tart: SecItemAdd)
	fmt.Println("[Keychain 저장]:")
	keychainProvider.Store("ghcr.io", "keychain-user", "keychain-pass")
	keychainProvider.Store("docker.io", "docker-hub-user", "docker-hub-pass")
	fmt.Println()

	// 검색 (저장 후)
	user, pass, _ = keychainProvider.Retrieve("ghcr.io")
	fmt.Printf("  ghcr.io (저장 후): user=%s, pass=%s\n", user, pass)

	user, pass, _ = keychainProvider.Retrieve("docker.io")
	fmt.Printf("  docker.io (저장 후): user=%s, pass=%s\n\n", user, pass)

	// 갱신 (tart: SecItemUpdate)
	fmt.Println("[Keychain 갱신]:")
	keychainProvider.Store("ghcr.io", "updated-user", "updated-pass")
	user, pass, _ = keychainProvider.Retrieve("ghcr.io")
	fmt.Printf("  ghcr.io (갱신 후): user=%s, pass=%s\n\n", user, pass)

	// --- 5. 프로바이더 체인 테스트 ---
	fmt.Println("========== 5. 프로바이더 체인 (lookupCredentials) ==========")
	fmt.Println()

	// tart 기본 체인: [Environment, DockerConfig, Keychain]
	chain := &CredentialsChain{
		Providers: []CredentialsProvider{
			NewEnvironmentCredentialsProvider(map[string]string{}), // 환경변수 없음
			dockerProvider,
			keychainProvider,
		},
	}

	fmt.Println("[체인 순서]: 환경변수 -> Docker config -> Keychain")
	fmt.Println()

	// 테스트 1: ghcr.io (DockerConfig에서 매칭)
	fmt.Println("[테스트 1] ghcr.io 자격증명 검색:")
	user, pass, source := chain.Lookup("ghcr.io")
	if user != "" {
		fmt.Printf("  결과: user=%s, 출처=%s\n\n", user, source)
	}

	// 테스트 2: docker.io (Keychain에서 매칭)
	fmt.Println("[테스트 2] docker.io 자격증명 검색:")
	user, pass, source = chain.Lookup("docker.io")
	if user != "" {
		fmt.Printf("  결과: user=%s, 출처=%s\n\n", user, source)
	}

	// 테스트 3: unknown.io (매칭 없음)
	fmt.Println("[테스트 3] unknown.io 자격증명 검색:")
	user, _, source = chain.Lookup("unknown.io")
	if user == "" {
		fmt.Printf("  결과: 자격증명 없음 (모든 프로바이더 실패)\n\n")
	}

	// 테스트 4: 환경변수 우선순위 (환경변수가 DockerConfig보다 우선)
	fmt.Println("[테스트 4] 환경변수 우선순위:")
	chainWithEnv := &CredentialsChain{
		Providers: []CredentialsProvider{
			NewEnvironmentCredentialsProvider(map[string]string{
				"TART_REGISTRY_USERNAME": "env-priority-user",
				"TART_REGISTRY_PASSWORD": "env-priority-pass",
			}),
			dockerProvider,
			keychainProvider,
		},
	}
	user, _, source = chainWithEnv.Lookup("ghcr.io")
	fmt.Printf("  ghcr.io: user=%s, 출처=%s (환경변수가 우선)\n\n", user, source)

	// --- 6. Authentication + AuthenticationKeeper ---
	fmt.Println("========== 6. Authentication + AuthenticationKeeper ==========")
	fmt.Println()

	keeper := &AuthenticationKeeper{}

	// 초기 상태: 인증 없음
	_, _, ok := keeper.Header()
	fmt.Printf("[Keeper] 초기 상태: 인증 설정=%v\n\n", ok)

	// Basic 인증 설정
	fmt.Println("[Basic 인증 설정]:")
	keeper.Set(&BasicAuthentication{User: "admin", Password: "secret"})
	name, value, ok := keeper.Header()
	fmt.Printf("  헤더: %s: %s\n", name, value)
	fmt.Printf("  유효: %v (Basic은 만료 없음)\n\n", ok)

	// Bearer 토큰 설정 (유효)
	fmt.Println("[Bearer 토큰 설정 (유효)]:")
	keeper.Set(&BearerAuthentication{
		Token:     "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.valid-token",
		ExpiresAt: time.Now().Add(60 * time.Second),
	})
	name, value, ok = keeper.Header()
	fmt.Printf("  헤더: %s: %s...\n", name, value[:30])
	fmt.Printf("  유효: %v\n\n", ok)

	// Bearer 토큰 설정 (만료됨)
	fmt.Println("[Bearer 토큰 설정 (만료됨)]:")
	keeper.Set(&BearerAuthentication{
		Token:     "expired-token",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // 이미 만료
	})
	_, _, ok = keeper.Header()
	fmt.Printf("  유효: %v (만료된 토큰 -> 헤더 제공하지 않음)\n\n", ok)

	// --- 7. 전체 인증 흐름 시뮬레이션 ---
	fmt.Println("========== 7. 전체 인증 흐름 ==========")
	fmt.Println()
	fmt.Println("tart Registry 인증 흐름 (Registry.swift):")
	fmt.Println()
	fmt.Println("  1) 첫 번째 요청: Authorization 헤더 없이 전송")
	fmt.Println("  2) 서버 응답: 401 + WWW-Authenticate 헤더")
	fmt.Println()
	fmt.Println("  3) WWW-Authenticate 스킴 판별:")
	fmt.Println("     a) 'basic': lookupCredentials() -> BasicAuthentication 설정")
	fmt.Println("     b) 'bearer': realm URL로 토큰 요청")
	fmt.Println()
	fmt.Println("  4) Bearer 토큰 요청:")
	fmt.Println("     a) lookupCredentials() -> 체인에서 자격증명 검색")
	fmt.Println("     b) Basic 인증으로 realm URL에 GET 요청")
	fmt.Println("     c) TokenResponse 파싱: token + expires_in + issued_at")
	fmt.Println("     d) AuthenticationKeeper에 저장")
	fmt.Println()
	fmt.Println("  5) 재시도: Authorization: Bearer {token} 헤더로 원래 요청 재전송")
	fmt.Println()
	fmt.Println("  6) 토큰 만료 시: isValid() == false -> 헤더 미포함 -> 401 -> 재인증")
	fmt.Println()

	// 시뮬레이션 실행
	fmt.Println("[시뮬레이션] ghcr.io에 인증된 요청 전송:")
	fmt.Println()

	// 자격증명 체인으로 사용자 검색
	user, pass, source = chain.Lookup("ghcr.io")
	fmt.Printf("  1) 자격증명 검색: user=%s (출처: %s)\n", user, source)

	// Basic 인코딩 (토큰 요청용)
	basicCreds := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	fmt.Printf("  2) Basic 인코딩: %s\n", basicCreds)

	// 토큰 요청 시뮬레이션
	simulatedToken := "ghp_simulated_token_abc123"
	expiresIn := 3600
	fmt.Printf("  3) 토큰 수신: token=%s..., expires_in=%ds\n", simulatedToken[:20], expiresIn)

	// AuthenticationKeeper에 저장
	authKeeper := &AuthenticationKeeper{}
	authKeeper.Set(&BearerAuthentication{
		Token:     simulatedToken,
		ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
	})

	name, value, ok = authKeeper.Header()
	fmt.Printf("  4) 인증 헤더: %s: %s...\n", name, value[:30])
	fmt.Printf("  5) 토큰 유효: %v\n", ok)
	fmt.Println()

	// --- 요약 ---
	fmt.Println("========== 요약 ==========")
	fmt.Println()
	fmt.Println("tart 자격증명 프로바이더 체인:")
	fmt.Println("  1) EnvironmentCredentialsProvider: TART_REGISTRY_USERNAME/PASSWORD")
	fmt.Println("  2) DockerConfigCredentialsProvider: ~/.docker/config.json (auths + credHelpers)")
	fmt.Println("  3) KeychainCredentialsProvider: macOS Keychain (SecItemCopyMatching)")
	fmt.Println()
	fmt.Println("체인 실행 규칙:")
	fmt.Println("  - 순서대로 시도, 첫 번째 성공한 결과 반환")
	fmt.Println("  - 프로바이더 실패는 경고 출력 후 다음 프로바이더로 계속")
	fmt.Println("  - 모든 프로바이더 실패 시 nil 반환 (익명 접근)")
	fmt.Println()
	fmt.Println("인증 흐름:")
	fmt.Println("  - Basic: 자격증명을 base64 인코딩하여 매 요청마다 전송")
	fmt.Println("  - Bearer: 자격증명으로 토큰 획득 -> 토큰으로 요청 -> 만료 시 재발급")
	fmt.Println("  - AuthenticationKeeper: actor로 토큰 상태 관리 (thread-safe)")
	fmt.Println()
	fmt.Println("[완료] 다중 인증 프로바이더 체인 시뮬레이션 성공")
}
