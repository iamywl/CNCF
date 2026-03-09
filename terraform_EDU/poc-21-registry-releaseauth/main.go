// Package main은 Terraform의 Registry 클라이언트와 릴리즈 인증 시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. 서비스 디스커버리 (.well-known/terraform.json)
// 2. Registry 클라이언트 (모듈 버전 조회, 다운로드 위치 해석)
// 3. 재시도 HTTP 클라이언트 (지수 백오프)
// 4. 인증 자격 증명 관리 (토큰 기반)
// 5. 에러 분류 체계 (ServiceUnreachable, VersionNotFound 등)
// 6. SHA-256 체크섬 검증
// 7. GPG 서명 검증 시뮬레이션
// 8. All Authenticator 패턴 (다중 인증기 체이닝)
// 9. 체크섬 파일 파싱
// 10. 릴리즈 인증 결과 집계
//
// 실제 소스 참조:
//   - internal/registry/client.go       (Client 구조체, API 메서드)
//   - internal/registry/errors.go       (에러 타입 정의)
//   - internal/releaseauth/all.go       (All 메타 Authenticator)
//   - internal/releaseauth/checksum.go  (SHA-256 체크섬 검증)
//   - internal/releaseauth/signature.go (GPG 서명 검증)
//   - internal/releaseauth/hash.go      (SHA256Hash 타입)
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"strings"
	"time"
)

// ============================================================================
// 1. 서비스 디스커버리 (disco 시뮬레이션)
// ============================================================================

// ServiceURLs는 서비스 디스커버리에서 반환하는 URL 맵이다.
// 실제로는 GET /.well-known/terraform.json에서 가져온다.
type ServiceURLs struct {
	ModulesV1   string `json:"modules.v1"`
	ProvidersV1 string `json:"providers.v1"`
}

// Disco는 서비스 디스커버리 클라이언트다.
type Disco struct {
	cache map[string]*ServiceURLs
}

// NewDisco는 새 Disco 인스턴스를 생성한다.
func NewDisco() *Disco {
	return &Disco{
		cache: map[string]*ServiceURLs{
			"registry.terraform.io": {
				ModulesV1:   "https://registry.terraform.io/v1/modules/",
				ProvidersV1: "https://registry.terraform.io/v1/providers/",
			},
			"app.terraform.io": {
				ModulesV1:   "https://app.terraform.io/api/registry/v1/modules/",
				ProvidersV1: "https://app.terraform.io/api/registry/v1/providers/",
			},
		},
	}
}

// DiscoverServiceURLs는 호스트의 서비스 URL을 반환한다.
func (d *Disco) DiscoverServiceURLs(host string) (*ServiceURLs, error) {
	urls, ok := d.cache[host]
	if !ok {
		return nil, fmt.Errorf("서비스 디스커버리 실패: %s (GET %s/.well-known/terraform.json)", host, host)
	}
	return urls, nil
}

// ============================================================================
// 2. 에러 분류 체계 (internal/registry/errors.go 시뮬레이션)
// ============================================================================

// RegistryError는 레지스트리 에러의 기본 인터페이스다.
type RegistryError interface {
	error
	IsRetryable() bool
}

// ServiceUnreachableError는 레지스트리 서비스에 접근할 수 없는 에러다.
type ServiceUnreachableError struct {
	Host string
	Err  error
}

func (e *ServiceUnreachableError) Error() string {
	return fmt.Sprintf("레지스트리 접근 불가: %s (%v)", e.Host, e.Err)
}
func (e *ServiceUnreachableError) IsRetryable() bool { return true }

// VersionNotFoundError는 요청한 버전이 없는 에러다.
type VersionNotFoundError struct {
	Module  string
	Version string
}

func (e *VersionNotFoundError) Error() string {
	return fmt.Sprintf("버전 없음: %s@%s", e.Module, e.Version)
}
func (e *VersionNotFoundError) IsRetryable() bool { return false }

// ModuleNotFoundError는 모듈 자체가 없는 에러다.
type ModuleNotFoundError struct {
	Module string
}

func (e *ModuleNotFoundError) Error() string {
	return fmt.Sprintf("모듈 없음: %s", e.Module)
}
func (e *ModuleNotFoundError) IsRetryable() bool { return false }

// ============================================================================
// 3. 재시도 HTTP 클라이언트 (retryablehttp 시뮬레이션)
// ============================================================================

// RetryConfig는 재시도 설정이다.
type RetryConfig struct {
	MaxRetries int
	MinWait    time.Duration
	MaxWait    time.Duration
}

// DefaultRetryConfig는 기본 재시도 설정이다.
var DefaultRetryConfig = RetryConfig{
	MaxRetries: 3,
	MinWait:    1 * time.Second,
	MaxWait:    30 * time.Second,
}

// RetryableClient는 재시도 가능한 HTTP 클라이언트다.
type RetryableClient struct {
	Config RetryConfig
}

// CalculateBackoff는 지수 백오프 대기 시간을 계산한다.
func (c *RetryableClient) CalculateBackoff(attempt int) time.Duration {
	// 지수 백오프: min * 2^attempt, 최대 max
	backoff := float64(c.Config.MinWait) * math.Pow(2, float64(attempt))
	if backoff > float64(c.Config.MaxWait) {
		backoff = float64(c.Config.MaxWait)
	}
	// 지터 추가 (±25%)
	jitter := backoff * 0.25 * (rand.Float64()*2 - 1)
	return time.Duration(backoff + jitter)
}

// ============================================================================
// 4. 인증 자격 증명 (credentials 시뮬레이션)
// ============================================================================

// Credentials는 레지스트리 인증 자격 증명이다.
type Credentials struct {
	Token string
}

// CredentialsStore는 자격 증명 저장소다.
type CredentialsStore struct {
	tokens map[string]string // host → token
}

// NewCredentialsStore는 새 자격 증명 저장소를 생성한다.
func NewCredentialsStore() *CredentialsStore {
	return &CredentialsStore{
		tokens: map[string]string{
			"app.terraform.io": "team-xxxxxxxxxxxxxxxx",
		},
	}
}

// ForHost는 호스트에 대한 자격 증명을 반환한다.
func (s *CredentialsStore) ForHost(host string) (*Credentials, error) {
	token, ok := s.tokens[host]
	if !ok {
		return nil, nil // 인증 불필요 (공개 레지스트리)
	}
	return &Credentials{Token: token}, nil
}

// ============================================================================
// 5. Registry 클라이언트 (internal/registry/client.go 시뮬레이션)
// ============================================================================

// ModuleVersion은 모듈 버전 정보다.
type ModuleVersion struct {
	Version string
}

// ModuleVersionsResponse는 버전 목록 응답이다.
type ModuleVersionsResponse struct {
	Modules []struct {
		Versions []ModuleVersion
	}
}

// RegistryClient는 Terraform Registry API 클라이언트다.
type RegistryClient struct {
	disco       *Disco
	creds       *CredentialsStore
	retryClient *RetryableClient

	// 시뮬레이션용 모듈 데이터베이스
	modules map[string][]ModuleVersion
}

// NewRegistryClient는 새 Registry 클라이언트를 생성한다.
func NewRegistryClient() *RegistryClient {
	return &RegistryClient{
		disco:       NewDisco(),
		creds:       NewCredentialsStore(),
		retryClient: &RetryableClient{Config: DefaultRetryConfig},
		modules: map[string][]ModuleVersion{
			"hashicorp/consul/aws": {
				{Version: "0.11.0"},
				{Version: "0.10.1"},
				{Version: "0.10.0"},
				{Version: "0.9.3"},
			},
			"hashicorp/vpc/aws": {
				{Version: "5.4.0"},
				{Version: "5.3.0"},
				{Version: "5.2.0"},
			},
		},
	}
}

// ModuleVersions는 모듈의 사용 가능한 버전 목록을 조회한다.
// GET /v1/modules/{namespace}/{name}/{provider}/versions
func (c *RegistryClient) ModuleVersions(host, module string) ([]ModuleVersion, error) {
	// 1. 서비스 디스커버리
	_, err := c.disco.DiscoverServiceURLs(host)
	if err != nil {
		return nil, &ServiceUnreachableError{Host: host, Err: err}
	}

	// 2. 인증 확인
	cred, _ := c.creds.ForHost(host)
	authStr := "(인증 없음)"
	if cred != nil {
		authStr = fmt.Sprintf("Bearer %s...%s", cred.Token[:5], cred.Token[len(cred.Token)-4:])
	}
	_ = authStr

	// 3. 버전 조회
	versions, ok := c.modules[module]
	if !ok {
		return nil, &ModuleNotFoundError{Module: module}
	}

	return versions, nil
}

// ModuleLocation은 모듈의 다운로드 위치를 반환한다.
// GET /v1/modules/{namespace}/{name}/{provider}/{version}/download
func (c *RegistryClient) ModuleLocation(host, module, version string) (string, error) {
	versions, err := c.ModuleVersions(host, module)
	if err != nil {
		return "", err
	}

	for _, v := range versions {
		if v.Version == version {
			// X-Terraform-Get 헤더 시뮬레이션
			return fmt.Sprintf("https://github.com/%s/archive/v%s.tar.gz",
				module, version), nil
		}
	}

	return "", &VersionNotFoundError{Module: module, Version: version}
}

// ============================================================================
// 6. SHA-256 체크섬 (internal/releaseauth/hash.go + checksum.go 시뮬레이션)
// ============================================================================

// SHA256Hash는 SHA-256 해시값이다.
type SHA256Hash [32]byte

// ParseSHA256Hash는 16진수 문자열에서 해시값을 파싱한다.
func ParseSHA256Hash(s string) (SHA256Hash, error) {
	var h SHA256Hash
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("잘못된 SHA-256 해시: %s", s)
	}
	if len(b) != 32 {
		return h, fmt.Errorf("SHA-256 해시 길이 불일치: %d != 32", len(b))
	}
	copy(h[:], b)
	return h, nil
}

func (h SHA256Hash) String() string {
	return hex.EncodeToString(h[:])
}

// ParseChecksumFile은 SHA256SUMS 파일을 파싱한다.
// 형식: <sha256hex>  <filename>
func ParseChecksumFile(content string) (map[string]SHA256Hash, error) {
	result := make(map[string]SHA256Hash)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("잘못된 체크섬 행: %s", line)
		}
		hash, err := ParseSHA256Hash(parts[0])
		if err != nil {
			return nil, err
		}
		result[parts[1]] = hash
	}
	return result, nil
}

// ============================================================================
// 7. Authenticator 인터페이스 (internal/releaseauth 시뮬레이션)
// ============================================================================

// AuthResult는 인증 결과를 나타낸다.
type AuthResult struct {
	AuthType    string
	Description string
	Success     bool
}

func (r AuthResult) String() string {
	status := "PASS"
	if !r.Success {
		status = "FAIL"
	}
	return fmt.Sprintf("[%s] %s: %s", status, r.AuthType, r.Description)
}

// Authenticator는 릴리즈를 인증하는 인터페이스다.
type Authenticator interface {
	Authenticate(filename string, data []byte) (AuthResult, error)
}

// ============================================================================
// 8. 체크섬 Authenticator (internal/releaseauth/checksum.go 시뮬레이션)
// ============================================================================

// ChecksumAuthenticator는 SHA-256 체크섬으로 릴리즈를 인증한다.
type ChecksumAuthenticator struct {
	ExpectedHashes map[string]SHA256Hash
}

// NewChecksumAuthenticator는 SHA256SUMS 파일 내용으로 인증기를 생성한다.
func NewChecksumAuthenticator(checksumFile string) (*ChecksumAuthenticator, error) {
	hashes, err := ParseChecksumFile(checksumFile)
	if err != nil {
		return nil, err
	}
	return &ChecksumAuthenticator{ExpectedHashes: hashes}, nil
}

// Authenticate는 파일의 SHA-256 체크섬을 검증한다.
func (a *ChecksumAuthenticator) Authenticate(filename string, data []byte) (AuthResult, error) {
	expected, ok := a.ExpectedHashes[filename]
	if !ok {
		return AuthResult{
			AuthType:    "checksum",
			Description: fmt.Sprintf("체크섬 파일에 %s 없음", filename),
			Success:     false,
		}, nil
	}

	actual := sha256.Sum256(data)
	if actual != expected {
		return AuthResult{
			AuthType:    "checksum",
			Description: fmt.Sprintf("SHA-256 불일치: expected=%s, actual=%s", expected, SHA256Hash(actual)),
			Success:     false,
		}, nil
	}

	return AuthResult{
		AuthType:    "checksum",
		Description: fmt.Sprintf("SHA-256 일치: %s", expected),
		Success:     true,
	}, nil
}

// ============================================================================
// 9. 서명 Authenticator (internal/releaseauth/signature.go 시뮬레이션)
// ============================================================================

// SignatureAuthenticator는 GPG 서명으로 릴리즈를 인증한다.
// 실제로는 openpgp 패키지를 사용하지만, 여기서는 간단한 HMAC 시뮬레이션.
type SignatureAuthenticator struct {
	TrustedKeys map[string]string // keyID → fingerprint
}

// NewSignatureAuthenticator는 새 서명 인증기를 생성한다.
func NewSignatureAuthenticator() *SignatureAuthenticator {
	return &SignatureAuthenticator{
		TrustedKeys: map[string]string{
			"72D7468F":   "C874 011F 0AB4 0511 0D02 1055 3436 5D94 72D7 468F",
			"HashiCorp":  "91A6 E7F8 5D05 C656 30BE F189 5185 2D87 348F FC4C",
		},
	}
}

// Authenticate는 서명을 검증한다 (시뮬레이션).
func (a *SignatureAuthenticator) Authenticate(filename string, data []byte) (AuthResult, error) {
	// 실제로는 GPG 서명 검증을 수행한다.
	// 여기서는 파일 크기로 간단히 시뮬레이션.
	if len(data) > 0 {
		return AuthResult{
			AuthType:    "signature",
			Description: "HashiCorp GPG 키로 서명 검증 성공 (키: 72D7468F)",
			Success:     true,
		}, nil
	}
	return AuthResult{
		AuthType:    "signature",
		Description: "빈 파일에 대한 서명 검증 불가",
		Success:     false,
	}, nil
}

// ============================================================================
// 10. All Authenticator (internal/releaseauth/all.go 시뮬레이션)
// ============================================================================

// AllAuthenticator는 여러 인증기를 체이닝하여 모두 통과해야 인증 성공으로 처리한다.
// 실제 구현: internal/releaseauth/all.go
type AllAuthenticator struct {
	Authenticators []Authenticator
}

// Authenticate는 모든 인증기를 실행하고 결과를 집계한다.
func (a *AllAuthenticator) Authenticate(filename string, data []byte) ([]AuthResult, error) {
	var results []AuthResult
	allPassed := true

	for _, auth := range a.Authenticators {
		result, err := auth.Authenticate(filename, data)
		if err != nil {
			return results, fmt.Errorf("인증기 실행 오류: %w", err)
		}
		results = append(results, result)
		if !result.Success {
			allPassed = false
		}
	}

	if !allPassed {
		return results, fmt.Errorf("하나 이상의 인증이 실패했습니다")
	}
	return results, nil
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Terraform Registry 클라이언트 & 릴리즈 인증 시뮬레이션 PoC  ║")
	fmt.Println("║  실제 소스: internal/registry/, internal/releaseauth/       ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. 서비스 디스커버리 ===
	fmt.Println("=== 1. 서비스 디스커버리 ===")
	disco := NewDisco()

	urls, err := disco.DiscoverServiceURLs("registry.terraform.io")
	if err == nil {
		fmt.Printf("  registry.terraform.io:\n")
		fmt.Printf("    modules.v1:   %s\n", urls.ModulesV1)
		fmt.Printf("    providers.v1: %s\n", urls.ProvidersV1)
	}

	_, err = disco.DiscoverServiceURLs("unknown.example.com")
	fmt.Printf("  unknown.example.com: %v\n", err)
	fmt.Println()

	// === 2. Registry 클라이언트 ===
	fmt.Println("=== 2. Registry 클라이언트 ===")
	client := NewRegistryClient()

	// 모듈 버전 조회
	versions, err := client.ModuleVersions("registry.terraform.io", "hashicorp/consul/aws")
	if err == nil {
		fmt.Printf("  hashicorp/consul/aws 버전 목록:\n")
		for _, v := range versions {
			fmt.Printf("    - %s\n", v.Version)
		}
	}

	// 모듈 다운로드 위치
	loc, err := client.ModuleLocation("registry.terraform.io", "hashicorp/vpc/aws", "5.4.0")
	if err == nil {
		fmt.Printf("  hashicorp/vpc/aws@5.4.0 다운로드: %s\n", loc)
	}

	// 존재하지 않는 모듈
	_, err = client.ModuleVersions("registry.terraform.io", "nonexistent/module/aws")
	fmt.Printf("  없는 모듈: %v (재시도=%v)\n", err, err.(RegistryError).IsRetryable())

	// 존재하지 않는 버전
	_, err = client.ModuleLocation("registry.terraform.io", "hashicorp/consul/aws", "99.0.0")
	fmt.Printf("  없는 버전: %v (재시도=%v)\n", err, err.(RegistryError).IsRetryable())
	fmt.Println()

	// === 3. 재시도 백오프 ===
	fmt.Println("=== 3. 재시도 백오프 계산 ===")
	retryClient := &RetryableClient{Config: DefaultRetryConfig}
	for i := 0; i < 5; i++ {
		backoff := retryClient.CalculateBackoff(i)
		fmt.Printf("  시도 %d: 대기 시간 ~%.1f초\n", i+1, backoff.Seconds())
	}
	fmt.Println()

	// === 4. 인증 자격 증명 ===
	fmt.Println("=== 4. 인증 자격 증명 ===")
	creds := NewCredentialsStore()

	cred, _ := creds.ForHost("app.terraform.io")
	if cred != nil {
		fmt.Printf("  app.terraform.io: Bearer %s...%s\n", cred.Token[:5], cred.Token[len(cred.Token)-4:])
	}

	cred, _ = creds.ForHost("registry.terraform.io")
	if cred == nil {
		fmt.Printf("  registry.terraform.io: 인증 불필요 (공개 레지스트리)\n")
	}
	fmt.Println()

	// === 5. SHA-256 체크섬 검증 ===
	fmt.Println("=== 5. SHA-256 체크섬 검증 ===")

	// 가상 바이너리 데이터
	binaryData := []byte("terraform v1.10.0 linux_amd64 binary content simulation")
	actualHash := sha256.Sum256(binaryData)

	checksumFile := fmt.Sprintf("%s  terraform_1.10.0_linux_amd64.zip\n"+
		"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890  terraform_1.10.0_darwin_arm64.zip\n",
		hex.EncodeToString(actualHash[:]))

	fmt.Printf("  체크섬 파일:\n")
	for _, line := range strings.Split(strings.TrimSpace(checksumFile), "\n") {
		fmt.Printf("    %s\n", line)
	}

	checksumAuth, err := NewChecksumAuthenticator(checksumFile)
	if err != nil {
		fmt.Printf("  체크섬 파싱 오류: %v\n", err)
		return
	}

	// 올바른 체크섬 검증
	result, _ := checksumAuth.Authenticate("terraform_1.10.0_linux_amd64.zip", binaryData)
	fmt.Printf("  검증 결과: %s\n", result)

	// 변조된 데이터
	tampered := append([]byte{}, binaryData...)
	tampered[0] = 'X'
	result, _ = checksumAuth.Authenticate("terraform_1.10.0_linux_amd64.zip", tampered)
	fmt.Printf("  변조 검증: %s\n", result)

	// 없는 파일명
	result, _ = checksumAuth.Authenticate("terraform_1.10.0_windows_amd64.zip", binaryData)
	fmt.Printf("  없는 파일: %s\n", result)
	fmt.Println()

	// === 6. GPG 서명 검증 ===
	fmt.Println("=== 6. GPG 서명 검증 (시뮬레이션) ===")
	sigAuth := NewSignatureAuthenticator()
	result, _ = sigAuth.Authenticate("terraform_1.10.0_SHA256SUMS", binaryData)
	fmt.Printf("  서명 검증: %s\n", result)

	result, _ = sigAuth.Authenticate("empty_file", []byte{})
	fmt.Printf("  빈 파일:   %s\n", result)
	fmt.Println()

	// === 7. All Authenticator (체이닝) ===
	fmt.Println("=== 7. All Authenticator (체크섬 + 서명) ===")

	allAuth := &AllAuthenticator{
		Authenticators: []Authenticator{checksumAuth, sigAuth},
	}

	results, err := allAuth.Authenticate("terraform_1.10.0_linux_amd64.zip", binaryData)
	for _, r := range results {
		fmt.Printf("  %s\n", r)
	}
	if err != nil {
		fmt.Printf("  최종: 실패 — %v\n", err)
	} else {
		fmt.Printf("  최종: 모든 인증 통과\n")
	}
	fmt.Println()

	// 변조된 데이터로 All Authenticator 테스트
	fmt.Println("=== 8. 변조된 데이터 All Authenticator ===")
	results, err = allAuth.Authenticate("terraform_1.10.0_linux_amd64.zip", tampered)
	for _, r := range results {
		fmt.Printf("  %s\n", r)
	}
	if err != nil {
		fmt.Printf("  최종: %v\n", err)
	}
	fmt.Println()

	// === 9. URL 파싱 데모 ===
	fmt.Println("=== 9. 레지스트리 소스 주소 파싱 ===")
	testSources := []string{
		"hashicorp/consul/aws",
		"app.terraform.io/myorg/vpc/aws",
		"registry.terraform.io/hashicorp/consul/aws",
	}
	for _, src := range testSources {
		parts := strings.Split(src, "/")
		if len(parts) == 3 {
			fmt.Printf("  %s → host=registry.terraform.io ns=%s name=%s provider=%s\n",
				src, parts[0], parts[1], parts[2])
		} else if len(parts) == 4 {
			fmt.Printf("  %s → host=%s ns=%s name=%s provider=%s\n",
				src, parts[0], parts[1], parts[2], parts[3])
		}
	}
	fmt.Println()

	// === 10. 전체 흐름 데모 ===
	fmt.Println("=== 10. 전체 흐름: 모듈 다운로드 + 인증 ===")
	module := "hashicorp/consul/aws"
	version := "0.11.0"

	fmt.Printf("  1. 모듈 버전 조회: %s\n", module)
	versions, _ = client.ModuleVersions("registry.terraform.io", module)
	fmt.Printf("     사용 가능: %v\n", func() []string {
		var vs []string
		for _, v := range versions {
			vs = append(vs, v.Version)
		}
		return vs
	}())

	fmt.Printf("  2. 다운로드 위치 해석: @%s\n", version)
	downloadURL, _ := client.ModuleLocation("registry.terraform.io", module, version)
	fmt.Printf("     URL: %s\n", downloadURL)

	parsedURL, _ := url.Parse(downloadURL)
	fmt.Printf("  3. 다운로드: %s%s\n", parsedURL.Host, parsedURL.Path)

	fmt.Printf("  4. 체크섬 검증: SHA-256 확인\n")
	fmt.Printf("  5. 서명 검증: GPG 서명 확인\n")
	fmt.Printf("  6. 모듈 사용 준비 완료\n")
}
