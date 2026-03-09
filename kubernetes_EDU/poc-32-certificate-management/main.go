// Kubernetes 인증서 관리 및 Bootstrap 인증 PoC
//
// Kubernetes의 인증서 관리 시스템 핵심 알고리즘을 Go 표준 라이브러리만으로 시뮬레이션한다.
//
// 시뮬레이션 항목:
//   1. CSR 생성 및 승인 흐름 (CertificateSigningRequest 상태 머신)
//   2. CA 인증서 서명 (x509.CreateCertificate 기반)
//   3. Bootstrap Token 인증 (constant-time 비교, 만료 체크)
//   4. 인증서 로테이션 메커니즘 (주기적 변경 감지 + 연결 재설정)
//   5. Signer Name 기반 라우팅 (signer별 검증 함수 매핑)
//
// 실행:
//   go run main.go
//
// 참고 소스:
//   - staging/src/k8s.io/api/certificates/v1/types.go
//   - pkg/controller/certificates/approver/sarapprove.go
//   - pkg/controller/certificates/signer/signer.go
//   - plugin/pkg/auth/authenticator/token/bootstrap/bootstrap.go
//   - pkg/kubelet/certificate/bootstrap/bootstrap.go
//   - staging/src/k8s.io/client-go/transport/cert_rotation.go

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. CSR 타입 정의 (CertificateSigningRequest 상태 머신)
// =============================================================================
// 실제 소스: staging/src/k8s.io/api/certificates/v1/types.go

// RequestConditionType은 CSR 조건의 타입이다.
type RequestConditionType string

const (
	CertificateApproved RequestConditionType = "Approved"
	CertificateDenied   RequestConditionType = "Denied"
	CertificateFailed   RequestConditionType = "Failed"
)

// KeyUsage는 키 용도를 나타낸다.
type KeyUsage string

const (
	UsageDigitalSignature KeyUsage = "digital signature"
	UsageKeyEncipherment  KeyUsage = "key encipherment"
	UsageClientAuth       KeyUsage = "client auth"
	UsageServerAuth       KeyUsage = "server auth"
)

// Well-Known Signer Names
const (
	KubeAPIServerClientSignerName        = "kubernetes.io/kube-apiserver-client"
	KubeAPIServerClientKubeletSignerName = "kubernetes.io/kube-apiserver-client-kubelet"
	KubeletServingSignerName             = "kubernetes.io/kubelet-serving"
	LegacyUnknownSignerName              = "kubernetes.io/legacy-unknown"
)

// CertificateSigningRequestCondition은 CSR의 조건을 나타낸다.
type CertificateSigningRequestCondition struct {
	Type    RequestConditionType
	Status  bool
	Reason  string
	Message string
	Time    time.Time
}

// CertificateSigningRequest는 인증서 서명 요청을 나타낸다.
type CertificateSigningRequest struct {
	Name string

	// Spec (생성 후 불변)
	Request           []byte // PEM 인코딩된 CSR
	SignerName        string
	ExpirationSeconds *int32
	Usages            []KeyUsage

	// 요청자 정보 (API 서버가 설정)
	Username string
	Groups   []string

	// Status
	Conditions  []CertificateSigningRequestCondition
	Certificate []byte // 발급된 인증서 (PEM)
}

// IsApproved는 CSR이 승인되었는지 확인한다.
func (csr *CertificateSigningRequest) IsApproved() bool {
	for _, c := range csr.Conditions {
		if c.Type == CertificateApproved && c.Status {
			return true
		}
	}
	return false
}

// IsDenied는 CSR이 거부되었는지 확인한다.
func (csr *CertificateSigningRequest) IsDenied() bool {
	for _, c := range csr.Conditions {
		if c.Type == CertificateDenied && c.Status {
			return true
		}
	}
	return false
}

// HasFailed는 CSR이 실패했는지 확인한다.
func (csr *CertificateSigningRequest) HasFailed() bool {
	for _, c := range csr.Conditions {
		if c.Type == CertificateFailed && c.Status {
			return true
		}
	}
	return false
}

// =============================================================================
// 2. CSR 승인기 (sarApprover 패턴)
// =============================================================================
// 실제 소스: pkg/controller/certificates/approver/sarapprove.go

// csrRecognizer는 CSR을 인식하고 승인 여부를 결정하는 패턴이다.
type csrRecognizer struct {
	recognize      func(csr *CertificateSigningRequest) bool
	permissionName string // SubjectAccessReview에서 확인할 권한 이름
	successMessage string
}

// sarApprover는 SubjectAccessReview 기반 CSR 승인기이다.
type sarApprover struct {
	recognizers   []csrRecognizer
	allowedUsers  map[string][]string // username -> 허용된 서브리소스 목록
}

// newSarApprover는 새 sarApprover를 생성한다.
func newSarApprover() *sarApprover {
	return &sarApprover{
		recognizers: []csrRecognizer{
			{
				recognize:      isSelfNodeClientCert,
				permissionName: "selfnodeclient",
				successMessage: "Auto approving self kubelet client certificate after SubjectAccessReview.",
			},
			{
				recognize:      isNodeClientCert,
				permissionName: "nodeclient",
				successMessage: "Auto approving kubelet client certificate after SubjectAccessReview.",
			},
		},
		allowedUsers: map[string][]string{
			"system:node:worker-1":         {"selfnodeclient", "nodeclient"},
			"system:node:worker-2":         {"selfnodeclient", "nodeclient"},
			"system:bootstrap:abcdef":      {"nodeclient"},
			"admin":                        {"nodeclient"},
		},
	}
}

// isSelfNodeClientCert는 자기 자신의 노드 클라이언트 인증서 요청인지 확인한다.
func isSelfNodeClientCert(csr *CertificateSigningRequest) bool {
	if csr.SignerName != KubeAPIServerClientKubeletSignerName {
		return false
	}
	// CSR 생성자(Username)와 요청된 CN이 같은지 확인
	// 실제로는 x509.CertificateRequest를 파싱하여 CN을 비교한다
	return csr.Username == csr.Name || strings.HasPrefix(csr.Username, "system:node:")
}

// isNodeClientCert는 노드 클라이언트 인증서 요청인지 확인한다.
func isNodeClientCert(csr *CertificateSigningRequest) bool {
	return csr.SignerName == KubeAPIServerClientKubeletSignerName
}

// authorize는 SubjectAccessReview를 시뮬레이션한다.
func (a *sarApprover) authorize(csr *CertificateSigningRequest, permissionName string) bool {
	allowedPerms, exists := a.allowedUsers[csr.Username]
	if !exists {
		return false
	}
	for _, perm := range allowedPerms {
		if perm == permissionName {
			return true
		}
	}
	return false
}

// handle은 CSR 승인 처리를 수행한다.
// 실제 소스: pkg/controller/certificates/approver/sarapprove.go handle() (line 78-118)
func (a *sarApprover) handle(csr *CertificateSigningRequest) (string, error) {
	// 이미 처리된 CSR 무시
	if len(csr.Certificate) != 0 {
		return "skip: already has certificate", nil
	}
	if csr.IsApproved() || csr.IsDenied() {
		return "skip: already approved or denied", nil
	}

	tried := []string{}

	for _, r := range a.recognizers {
		if !r.recognize(csr) {
			continue
		}

		tried = append(tried, r.permissionName)

		// SubjectAccessReview 시뮬레이션
		if a.authorize(csr, r.permissionName) {
			csr.Conditions = append(csr.Conditions, CertificateSigningRequestCondition{
				Type:    CertificateApproved,
				Status:  true,
				Reason:  "AutoApproved",
				Message: r.successMessage,
				Time:    time.Now(),
			})
			return fmt.Sprintf("approved via %s", r.permissionName), nil
		}
	}

	if len(tried) > 0 {
		return fmt.Sprintf("recognized as %v but SAR not approved", tried), nil
	}

	return "not recognized by any approver", nil
}

// =============================================================================
// 3. CSR 서명기 (Signer 패턴)
// =============================================================================
// 실제 소스: pkg/controller/certificates/signer/signer.go

// CA는 인증서 서명 기관을 나타낸다.
type CA struct {
	Certificate *x509.Certificate
	PrivateKey  *ecdsa.PrivateKey
	CertPEM     []byte
}

// newCA는 새 CA를 생성한다.
func newCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("CA 키 생성 실패: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "Kubernetes CA",
			Organization: []string{"kubernetes"},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute), // backdate
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("CA 인증서 생성 실패: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("CA 인증서 파싱 실패: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	return &CA{
		Certificate: cert,
		PrivateKey:  key,
		CertPEM:     certPEM,
	}, nil
}

// isRequestForSignerFunc는 signer별 CSR 검증 함수 타입이다.
type isRequestForSignerFunc func(usages []KeyUsage, signerName string) (bool, error)

// csrSigner는 CSR 서명기이다.
type csrSigner struct {
	ca                   *CA
	certTTL              time.Duration
	signerName           string
	isRequestForSignerFn isRequestForSignerFunc
}

// newSigner는 새 CSR 서명기를 생성한다.
func newSigner(ca *CA, signerName string, certTTL time.Duration) (*csrSigner, error) {
	verifyFn, err := getCSRVerificationFunc(signerName)
	if err != nil {
		return nil, err
	}
	return &csrSigner{
		ca:                   ca,
		certTTL:              certTTL,
		signerName:           signerName,
		isRequestForSignerFn: verifyFn,
	}, nil
}

// getCSRVerificationFunc는 signer name에 따른 검증 함수를 반환한다.
// 실제 소스: pkg/controller/certificates/signer/signer.go getCSRVerificationFuncForSignerName()
func getCSRVerificationFunc(signerName string) (isRequestForSignerFunc, error) {
	switch signerName {
	case KubeletServingSignerName:
		return isKubeletServing, nil
	case KubeAPIServerClientKubeletSignerName:
		return isKubeletClient, nil
	case KubeAPIServerClientSignerName:
		return isKubeAPIServerClient, nil
	case LegacyUnknownSignerName:
		return isLegacyUnknown, nil
	default:
		return nil, fmt.Errorf("unrecognized signerName: %q", signerName)
	}
}

func isKubeletServing(usages []KeyUsage, signerName string) (bool, error) {
	if signerName != KubeletServingSignerName {
		return false, nil
	}
	// server auth가 반드시 있어야 함
	for _, u := range usages {
		if u == UsageServerAuth {
			return true, nil
		}
	}
	return false, fmt.Errorf("kubelet serving CSR에 server auth usage 누락")
}

func isKubeletClient(usages []KeyUsage, signerName string) (bool, error) {
	if signerName != KubeAPIServerClientKubeletSignerName {
		return false, nil
	}
	return true, nil
}

func isKubeAPIServerClient(usages []KeyUsage, signerName string) (bool, error) {
	if signerName != KubeAPIServerClientSignerName {
		return false, nil
	}
	// client auth가 반드시 있어야 함
	hasClientAuth := false
	for _, u := range usages {
		switch u {
		case UsageDigitalSignature, UsageKeyEncipherment:
			// 허용
		case UsageClientAuth:
			hasClientAuth = true
		default:
			return false, fmt.Errorf("잘못된 usage: %s", u)
		}
	}
	if !hasClientAuth {
		return false, fmt.Errorf("client auth usage 필수")
	}
	return true, nil
}

func isLegacyUnknown(usages []KeyUsage, signerName string) (bool, error) {
	if signerName != LegacyUnknownSignerName {
		return false, nil
	}
	return true, nil // 제한 없음
}

// duration은 인증서 유효 기간을 결정한다.
// 실제 소스: pkg/controller/certificates/signer/signer.go duration() (line 219-237)
func (s *csrSigner) duration(expirationSeconds *int32) time.Duration {
	if expirationSeconds == nil {
		return s.certTTL
	}
	const minDuration = 10 * time.Minute
	requested := time.Duration(*expirationSeconds) * time.Second
	switch {
	case requested > s.certTTL:
		return s.certTTL
	case requested < minDuration:
		return minDuration
	default:
		return requested
	}
}

// handle은 CSR 서명 처리를 수행한다.
// 실제 소스: pkg/controller/certificates/signer/signer.go handle() (line 157-199)
func (s *csrSigner) handle(csr *CertificateSigningRequest) (string, error) {
	// 미승인/실패 CSR 무시
	if !csr.IsApproved() || csr.HasFailed() {
		return "skip: not approved or already failed", nil
	}

	// SignerName 일치 확인 (fast-path)
	if csr.SignerName != s.signerName {
		return "skip: signer name mismatch", nil
	}

	// Signer별 검증
	recognized, err := s.isRequestForSignerFn(csr.Usages, csr.SignerName)
	if err != nil {
		csr.Conditions = append(csr.Conditions, CertificateSigningRequestCondition{
			Type:    CertificateFailed,
			Status:  true,
			Reason:  "SignerValidationFailure",
			Message: err.Error(),
			Time:    time.Now(),
		})
		return fmt.Sprintf("failed: %v", err), nil
	}
	if !recognized {
		return "skip: not recognized by this signer", nil
	}

	// 서명 수행
	certPEM, err := s.sign(csr)
	if err != nil {
		return "", fmt.Errorf("서명 실패: %v", err)
	}

	csr.Certificate = certPEM
	return "signed successfully", nil
}

// sign은 CA 키로 인증서를 서명한다.
// 실제 소스: pkg/controller/certificates/signer/signer.go sign() (line 201-217)
func (s *csrSigner) sign(csr *CertificateSigningRequest) ([]byte, error) {
	// 클라이언트 키 생성 (시뮬레이션용)
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	ttl := s.duration(csr.ExpirationSeconds)
	backdate := 5 * time.Minute // 시계 오차 보정

	// 키 용도 변환
	keyUsage := x509.KeyUsageDigitalSignature
	var extKeyUsage []x509.ExtKeyUsage
	for _, u := range csr.Usages {
		switch u {
		case UsageKeyEncipherment:
			keyUsage |= x509.KeyUsageKeyEncipherment
		case UsageClientAuth:
			extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageClientAuth)
		case UsageServerAuth:
			extKeyUsage = append(extKeyUsage, x509.ExtKeyUsageServerAuth)
		}
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   csr.Username,
			Organization: []string{"system:nodes"},
		},
		NotBefore:   time.Now().Add(-backdate),
		NotAfter:    time.Now().Add(ttl),
		KeyUsage:    keyUsage,
		ExtKeyUsage: extKeyUsage,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, s.ca.Certificate,
		&clientKey.PublicKey, s.ca.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("인증서 서명 실패: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), nil
}

// =============================================================================
// 4. Bootstrap Token 인증
// =============================================================================
// 실제 소스: plugin/pkg/auth/authenticator/token/bootstrap/bootstrap.go

const (
	bootstrapTokenSecretPrefix = "bootstrap-token-"
	bootstrapUserPrefix        = "system:bootstrap:"
	bootstrapDefaultGroup      = "system:bootstrappers"
)

// BootstrapTokenSecret은 Bootstrap Token Secret을 나타낸다.
type BootstrapTokenSecret struct {
	Name         string
	TokenID      string
	TokenSecret  string
	Expiration   *time.Time
	UseAuth      bool
	ExtraGroups  []string
	Deleted      bool
}

// TokenAuthenticator는 Bootstrap Token 인증기이다.
// 실제 소스: line 52-54
type TokenAuthenticator struct {
	secrets map[string]*BootstrapTokenSecret // secretName -> secret
}

// AuthResponse는 인증 응답이다.
type AuthResponse struct {
	Username string
	Groups   []string
}

// AuthenticateToken은 Bootstrap Token을 인증한다.
// 실제 소스: line 90-151
func (t *TokenAuthenticator) AuthenticateToken(token string) (*AuthResponse, bool, error) {
	// 1단계: 토큰 형식 파싱 ("<id>.<secret>")
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || len(parts[0]) != 6 || len(parts[1]) != 16 {
		return nil, false, nil // 형식 불일치 -> 다른 인증기에 위임
	}
	tokenID := parts[0]
	tokenSecret := parts[1]

	// 2단계: Secret 조회
	secretName := bootstrapTokenSecretPrefix + tokenID
	secret, exists := t.secrets[secretName]
	if !exists {
		return nil, false, nil
	}

	// 3단계: 삭제 대기 중인 Secret 거부
	if secret.Deleted {
		return nil, false, nil
	}

	// 4단계: 토큰 비밀번호 비교 (constant-time comparison으로 타이밍 공격 방지)
	if subtle.ConstantTimeCompare([]byte(secret.TokenSecret), []byte(tokenSecret)) != 1 {
		return nil, false, nil
	}

	// 5단계: 토큰 ID 교차 확인
	if secret.TokenID != tokenID {
		return nil, false, nil
	}

	// 6단계: 만료 확인
	if secret.Expiration != nil && time.Now().After(*secret.Expiration) {
		return nil, false, nil
	}

	// 7단계: 인증 용도 확인
	if !secret.UseAuth {
		return nil, false, nil
	}

	// 8단계: 그룹 구성
	groups := []string{bootstrapDefaultGroup}
	groups = append(groups, secret.ExtraGroups...)

	// 9단계: 인증 응답 생성
	return &AuthResponse{
		Username: bootstrapUserPrefix + tokenID,
		Groups:   groups,
	}, true, nil
}

// =============================================================================
// 5. 인증서 로테이션
// =============================================================================
// 실제 소스: staging/src/k8s.io/client-go/transport/cert_rotation.go

// dynamicClientCert는 동적 클라이언트 인증서 관리기이다.
// 실제 소스: line 41-51
type dynamicClientCert struct {
	mu          sync.RWMutex
	currentCert []byte    // 현재 인증서 PEM
	certVersion int       // 변경 추적용 버전
	reloadFn    func() ([]byte, error) // 인증서 로드 콜백

	rotationCount int // 로테이션 횟수 추적
}

// loadClientCert는 인증서를 로드하고 변경을 감지한다.
// 실제 소스: line 68-96
func (c *dynamicClientCert) loadClientCert() ([]byte, bool, error) {
	newCert, err := c.reloadFn()
	if err != nil {
		return nil, false, err
	}

	// 변경 감지 (RLock으로 읽기 전용 확인)
	c.mu.RLock()
	haveCert := c.currentCert != nil
	// 실제로는 certsEqual()로 비교하지만 여기서는 바이트 비교
	same := string(c.currentCert) == string(newCert)
	c.mu.RUnlock()

	if same {
		return c.currentCert, false, nil // 변경 없음
	}

	// 새 인증서 저장 (Lock으로 쓰기)
	c.mu.Lock()
	c.currentCert = newCert
	c.certVersion++
	c.mu.Unlock()

	// 최초 인증서가 아닌 경우에만 연결 재설정
	if haveCert {
		c.rotationCount++
		// 실제: c.connDialer.CloseAll()
		return newCert, true, nil // 로테이션 발생
	}

	return newCert, false, nil // 최초 로드
}

// =============================================================================
// 6. 결정론적 CSR 이름 생성
// =============================================================================
// 실제 소스: pkg/kubelet/certificate/bootstrap/bootstrap.go digestedName() (line 368-398)

func digestedName(publicKey []byte, commonName string, org []string, usages []KeyUsage) string {
	hash := sha256.New()
	delimiter := byte('|')
	encode := base64.RawURLEncoding.EncodeToString

	write := func(data []byte) {
		hash.Write([]byte(encode(data)))
		hash.Write([]byte{delimiter})
	}

	write(publicKey)
	write([]byte(commonName))
	for _, o := range org {
		write([]byte(o))
	}
	for _, u := range usages {
		write([]byte(u))
	}

	return fmt.Sprintf("node-csr-%s", encode(hash.Sum(nil)))
}

// =============================================================================
// 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Kubernetes 인증서 관리 및 Bootstrap 인증 PoC ===")
	fmt.Println()

	// 데모 1: CSR 생성 및 승인 흐름
	demoCSRApproval()

	// 데모 2: CSR 서명/발급
	demoCSRSigning()

	// 데모 3: Bootstrap Token 인증
	demoBootstrapAuth()

	// 데모 4: 인증서 로테이션
	demoCertRotation()

	// 데모 5: Signer Name 기반 라우팅
	demoSignerRouting()

	// 데모 6: 결정론적 CSR 이름
	demoDigestedName()
}

func demoCSRApproval() {
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║  데모 1: CSR 승인 흐름 (sarApprover 패턴)    ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	approver := newSarApprover()

	testCases := []struct {
		name string
		csr  *CertificateSigningRequest
	}{
		{
			name: "kubelet 자체 인증서 갱신 (selfnodeclient)",
			csr: &CertificateSigningRequest{
				Name:       "node-csr-abc123",
				SignerName: KubeAPIServerClientKubeletSignerName,
				Usages:     []KeyUsage{UsageDigitalSignature, UsageClientAuth},
				Username:   "system:node:worker-1",
				Groups:     []string{"system:nodes", "system:authenticated"},
			},
		},
		{
			name: "관리자의 노드 인증서 생성 (nodeclient)",
			csr: &CertificateSigningRequest{
				Name:       "node-csr-def456",
				SignerName: KubeAPIServerClientKubeletSignerName,
				Usages:     []KeyUsage{UsageDigitalSignature, UsageClientAuth},
				Username:   "admin",
				Groups:     []string{"system:masters"},
			},
		},
		{
			name: "권한 없는 사용자의 요청",
			csr: &CertificateSigningRequest{
				Name:       "node-csr-ghi789",
				SignerName: KubeAPIServerClientKubeletSignerName,
				Usages:     []KeyUsage{UsageDigitalSignature, UsageClientAuth},
				Username:   "unauthorized-user",
				Groups:     []string{"system:authenticated"},
			},
		},
		{
			name: "비-kubelet CSR (일반 클라이언트)",
			csr: &CertificateSigningRequest{
				Name:       "user-csr-xyz",
				SignerName: KubeAPIServerClientSignerName,
				Usages:     []KeyUsage{UsageDigitalSignature, UsageClientAuth},
				Username:   "developer",
				Groups:     []string{"developers"},
			},
		},
	}

	for _, tc := range testCases {
		result, err := approver.handle(tc.csr)
		status := "PENDING"
		if tc.csr.IsApproved() {
			status = "APPROVED"
		}
		fmt.Printf("  [%s] %s\n", status, tc.name)
		fmt.Printf("    SignerName: %s\n", tc.csr.SignerName)
		fmt.Printf("    Username:   %s\n", tc.csr.Username)
		fmt.Printf("    결과:       %s\n", result)
		if err != nil {
			fmt.Printf("    에러:       %v\n", err)
		}
		fmt.Println()
	}
}

func demoCSRSigning() {
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║  데모 2: CSR 서명/발급 (CA Signer)           ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	// CA 생성
	ca, err := newCA()
	if err != nil {
		fmt.Printf("  CA 생성 실패: %v\n", err)
		return
	}
	fmt.Printf("  CA 생성됨: CN=%s\n", ca.Certificate.Subject.CommonName)
	fmt.Printf("  CA 유효기간: %s ~ %s\n",
		ca.Certificate.NotBefore.Format("2006-01-02"),
		ca.Certificate.NotAfter.Format("2006-01-02"))
	fmt.Println()

	// Signer 생성 (kubelet client용)
	signer, err := newSigner(ca, KubeAPIServerClientKubeletSignerName, 365*24*time.Hour)
	if err != nil {
		fmt.Printf("  Signer 생성 실패: %v\n", err)
		return
	}

	// 승인된 CSR 서명
	csr := &CertificateSigningRequest{
		Name:       "node-csr-sign-test",
		SignerName: KubeAPIServerClientKubeletSignerName,
		Usages:     []KeyUsage{UsageDigitalSignature, UsageClientAuth},
		Username:   "system:node:worker-1",
		Conditions: []CertificateSigningRequestCondition{
			{Type: CertificateApproved, Status: true, Reason: "AutoApproved"},
		},
	}

	result, err := signer.handle(csr)
	fmt.Printf("  CSR 서명 결과: %s\n", result)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
	}

	if len(csr.Certificate) > 0 {
		// 발급된 인증서 검증
		block, _ := pem.Decode(csr.Certificate)
		if block != nil {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err == nil {
				fmt.Printf("  발급된 인증서:\n")
				fmt.Printf("    CN:         %s\n", cert.Subject.CommonName)
				fmt.Printf("    O:          %v\n", cert.Subject.Organization)
				fmt.Printf("    NotBefore:  %s\n", cert.NotBefore.Format(time.RFC3339))
				fmt.Printf("    NotAfter:   %s\n", cert.NotAfter.Format(time.RFC3339))
				fmt.Printf("    발급자:     %s\n", cert.Issuer.CommonName)
				fmt.Printf("    시리얼번호: %s\n", cert.SerialNumber.String())
			}
		}
	}
	fmt.Println()

	// ExpirationSeconds 테스트
	fmt.Println("  --- 유효 기간 결정 테스트 ---")
	testDurations := []struct {
		name    string
		seconds *int32
	}{
		{"nil (기본값)", nil},
		{"600초 (최소)", int32Ptr(600)},
		{"300초 (최소 미만 -> 10분)", int32Ptr(300)},
		{"3600초 (1시간)", int32Ptr(3600)},
		{"99999999초 (최대 초과 -> certTTL)", int32Ptr(99999999)},
	}
	for _, td := range testDurations {
		d := signer.duration(td.seconds)
		fmt.Printf("  %-35s -> %v\n", td.name, d)
	}
	fmt.Println()
}

func demoBootstrapAuth() {
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║  데모 3: Bootstrap Token 인증                ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	// 만료 시간 설정
	futureExpiry := time.Now().Add(24 * time.Hour)
	pastExpiry := time.Now().Add(-1 * time.Hour)

	authenticator := &TokenAuthenticator{
		secrets: map[string]*BootstrapTokenSecret{
			"bootstrap-token-abcdef": {
				Name:        "bootstrap-token-abcdef",
				TokenID:     "abcdef",
				TokenSecret: "0123456789abcdef",
				Expiration:  &futureExpiry,
				UseAuth:     true,
				ExtraGroups: []string{"system:bootstrappers:worker"},
			},
			"bootstrap-token-xyz123": {
				Name:        "bootstrap-token-xyz123",
				TokenID:     "xyz123",
				TokenSecret: "secretsecretabcd",
				Expiration:  &pastExpiry, // 만료됨
				UseAuth:     true,
			},
			"bootstrap-token-nosign": {
				Name:        "bootstrap-token-nosign",
				TokenID:     "nosign",
				TokenSecret: "signsecretsecret",
				UseAuth:     false, // 인증용이 아님
			},
			"bootstrap-token-delete": {
				Name:        "bootstrap-token-delete",
				TokenID:     "delete",
				TokenSecret: "deletesecretabcd",
				UseAuth:     true,
				Deleted:     true, // 삭제 대기 중
			},
		},
	}

	testTokens := []struct {
		name  string
		token string
	}{
		{"유효한 토큰", "abcdef.0123456789abcdef"},
		{"만료된 토큰", "xyz123.secretsecretabcd"},
		{"인증 비활성 토큰", "nosign.signsecretsecret"},
		{"삭제 대기 토큰", "delete.deletesecretabcd"},
		{"잘못된 비밀번호", "abcdef.wrong_password!!"},
		{"존재하지 않는 토큰", "noexst.0123456789abcdef"},
		{"형식 오류 토큰", "not-a-bootstrap-token"},
	}

	for _, tt := range testTokens {
		resp, ok, err := authenticator.AuthenticateToken(tt.token)
		if ok && resp != nil {
			fmt.Printf("  [SUCCESS] %-25s -> User: %s, Groups: %v\n",
				tt.name, resp.Username, resp.Groups)
		} else {
			reason := "인증 실패"
			if err != nil {
				reason = err.Error()
			}
			fmt.Printf("  [REJECT ] %-25s -> %s\n", tt.name, reason)
		}
	}
	fmt.Println()
}

func demoCertRotation() {
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║  데모 4: 인증서 로테이션 메커니즘             ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	// CA 생성
	ca, err := newCA()
	if err != nil {
		fmt.Printf("  CA 생성 실패: %v\n", err)
		return
	}

	certVersion := 0
	currentCertPEM := generateTestCert(ca, "cert-v0", certVersion)

	rotator := &dynamicClientCert{
		reloadFn: func() ([]byte, error) {
			return currentCertPEM, nil
		},
	}

	// 1단계: 최초 로드
	cert, rotated, err := rotator.loadClientCert()
	fmt.Printf("  1단계 (최초 로드):\n")
	fmt.Printf("    인증서 로드: %d bytes\n", len(cert))
	fmt.Printf("    로테이션:    %v (최초 로드이므로 false)\n", rotated)
	fmt.Printf("    버전:        %d\n", rotator.certVersion)
	fmt.Println()

	// 2단계: 동일 인증서 재확인 (5분 후 시뮬레이션)
	cert, rotated, err = rotator.loadClientCert()
	fmt.Printf("  2단계 (변경 없음 - 5분 후):\n")
	fmt.Printf("    인증서 로드: %d bytes\n", len(cert))
	fmt.Printf("    로테이션:    %v (변경 없으므로 false)\n", rotated)
	fmt.Printf("    버전:        %d\n", rotator.certVersion)
	fmt.Println()

	// 3단계: 인증서 갱신 (새 인증서 발급됨)
	certVersion++
	currentCertPEM = generateTestCert(ca, "cert-v1", certVersion)
	cert, rotated, err = rotator.loadClientCert()
	fmt.Printf("  3단계 (인증서 갱신됨):\n")
	fmt.Printf("    인증서 로드: %d bytes\n", len(cert))
	fmt.Printf("    로테이션:    %v (새 인증서 감지 -> 연결 재설정)\n", rotated)
	fmt.Printf("    버전:        %d\n", rotator.certVersion)
	fmt.Printf("    로테이션 횟수: %d\n", rotator.rotationCount)
	fmt.Println()

	// 4단계: 다시 확인 (변경 없음)
	cert, rotated, err = rotator.loadClientCert()
	fmt.Printf("  4단계 (갱신 후 재확인):\n")
	fmt.Printf("    인증서 로드: %d bytes\n", len(cert))
	fmt.Printf("    로테이션:    %v\n", rotated)
	fmt.Printf("    버전:        %d (변경 없음)\n", rotator.certVersion)
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
	}
	fmt.Println()
}

func demoSignerRouting() {
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║  데모 5: Signer Name 기반 라우팅              ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	ca, err := newCA()
	if err != nil {
		fmt.Printf("  CA 생성 실패: %v\n", err)
		return
	}

	// 4개의 독립적인 signer 인스턴스 생성
	signerNames := []string{
		KubeAPIServerClientSignerName,
		KubeAPIServerClientKubeletSignerName,
		KubeletServingSignerName,
		LegacyUnknownSignerName,
	}

	signers := make([]*csrSigner, 0)
	for _, name := range signerNames {
		s, err := newSigner(ca, name, 365*24*time.Hour)
		if err != nil {
			fmt.Printf("  Signer [%s] 생성 실패: %v\n", name, err)
			continue
		}
		signers = append(signers, s)
		fmt.Printf("  Signer 생성: %s\n", name)
	}
	fmt.Println()

	// 테스트 CSR들
	testCSRs := []struct {
		name   string
		csr    *CertificateSigningRequest
	}{
		{
			"kubelet 클라이언트 CSR",
			&CertificateSigningRequest{
				Name:       "node-csr-1",
				SignerName: KubeAPIServerClientKubeletSignerName,
				Usages:     []KeyUsage{UsageDigitalSignature, UsageClientAuth},
				Username:   "system:node:test",
				Conditions: []CertificateSigningRequestCondition{
					{Type: CertificateApproved, Status: true},
				},
			},
		},
		{
			"API 서버 클라이언트 CSR",
			&CertificateSigningRequest{
				Name:       "client-csr-1",
				SignerName: KubeAPIServerClientSignerName,
				Usages:     []KeyUsage{UsageDigitalSignature, UsageClientAuth},
				Username:   "developer",
				Conditions: []CertificateSigningRequestCondition{
					{Type: CertificateApproved, Status: true},
				},
			},
		},
		{
			"kubelet 서빙 CSR (server auth 포함)",
			&CertificateSigningRequest{
				Name:       "serving-csr-1",
				SignerName: KubeletServingSignerName,
				Usages:     []KeyUsage{UsageDigitalSignature, UsageServerAuth},
				Username:   "system:node:test",
				Conditions: []CertificateSigningRequestCondition{
					{Type: CertificateApproved, Status: true},
				},
			},
		},
		{
			"kubelet 서빙 CSR (server auth 없음 -> 실패)",
			&CertificateSigningRequest{
				Name:       "serving-csr-bad",
				SignerName: KubeletServingSignerName,
				Usages:     []KeyUsage{UsageDigitalSignature, UsageClientAuth}, // server auth 없음!
				Username:   "system:node:test",
				Conditions: []CertificateSigningRequestCondition{
					{Type: CertificateApproved, Status: true},
				},
			},
		},
	}

	for _, tc := range testCSRs {
		fmt.Printf("  --- %s ---\n", tc.name)
		fmt.Printf("  요청 SignerName: %s\n", tc.csr.SignerName)

		handled := false
		for _, s := range signers {
			result, err := s.handle(tc.csr)
			if result != "skip: signer name mismatch" && result != "skip: not recognized by this signer" {
				fmt.Printf("  처리한 Signer: %s\n", s.signerName)
				fmt.Printf("  결과:          %s\n", result)
				if err != nil {
					fmt.Printf("  에러:          %v\n", err)
				}
				if len(tc.csr.Certificate) > 0 {
					fmt.Printf("  인증서 발급:   %d bytes\n", len(tc.csr.Certificate))
				}
				if tc.csr.HasFailed() {
					fmt.Printf("  상태:          FAILED\n")
				}
				handled = true
				break
			}
		}
		if !handled {
			fmt.Printf("  결과: 어떤 signer도 처리하지 않음\n")
		}
		fmt.Println()
	}
}

func demoDigestedName() {
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║  데모 6: 결정론적 CSR 이름 생성               ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	// 같은 입력 -> 같은 이름 (멱등성)
	pubKey := []byte("fake-public-key-data-for-worker-1")
	cn := "system:node:worker-1"
	org := []string{"system:nodes"}
	usages := []KeyUsage{UsageDigitalSignature, UsageClientAuth}

	name1 := digestedName(pubKey, cn, org, usages)
	name2 := digestedName(pubKey, cn, org, usages)

	fmt.Printf("  같은 입력으로 2회 생성:\n")
	fmt.Printf("    1회차: %s\n", name1)
	fmt.Printf("    2회차: %s\n", name2)
	fmt.Printf("    동일?  %v (멱등성 보장)\n", name1 == name2)
	fmt.Println()

	// 다른 노드 -> 다른 이름
	name3 := digestedName([]byte("fake-public-key-for-worker-2"),
		"system:node:worker-2", org, usages)
	fmt.Printf("  다른 노드:\n")
	fmt.Printf("    worker-1: %s\n", name1)
	fmt.Printf("    worker-2: %s\n", name3)
	fmt.Printf("    다름?     %v (노드별 고유)\n", name1 != name3)
	fmt.Println()

	// 구분자(|) 없이 충돌 가능한 입력 테스트
	nameA := digestedName(pubKey, "CN:foo", []string{"bar"}, usages)
	nameB := digestedName(pubKey, "CN:foob", []string{"ar"}, usages)
	fmt.Printf("  충돌 방지 테스트:\n")
	fmt.Printf("    입력A (CN:foo, O:bar):   %s\n", nameA)
	fmt.Printf("    입력B (CN:foob, O:ar):   %s\n", nameB)
	fmt.Printf("    다름?  %v (구분자로 충돌 방지)\n", nameA != nameB)
	fmt.Println()
}

// =============================================================================
// 헬퍼 함수
// =============================================================================

func int32Ptr(i int32) *int32 {
	return &i
}

func generateTestCert(ca *CA, cn string, version int) []byte {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(int64(version + 100)),
		Subject: pkix.Name{
			CommonName: fmt.Sprintf("%s-v%d", cn, version),
		},
		NotBefore: time.Now().Add(-5 * time.Minute),
		NotAfter:  time.Now().Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, template, ca.Certificate,
		&key.PublicKey, ca.PrivateKey)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}
