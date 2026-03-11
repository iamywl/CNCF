package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// gRPC Advanced TLS 시뮬레이션
// =============================================================================
//
// gRPC advancedtls 패키지는 고급 TLS 기능을 제공한다:
//   - CRL(Certificate Revocation List): 인증서 폐기 목록 검증
//   - Custom Verification: 사용자 정의 인증서 검증 로직
//   - Certificate Reloading: 런타임 인증서 교체 (무중단)
//   - mTLS: 상호 TLS 인증
//
// 실제 코드 참조:
//   - security/advancedtls/: Advanced TLS 패키지
//   - security/advancedtls/crl.go: CRL 검증
// =============================================================================

// --- 인증서 모델 ---

type CertInfo struct {
	Subject      string
	Issuer       string
	SerialNumber *big.Int
	NotBefore    time.Time
	NotAfter     time.Time
	IsCA         bool
	DNSNames     []string
	PrivateKey   *ecdsa.PrivateKey
	Certificate  *x509.Certificate
	PEMData      []byte
}

func (c CertInfo) String() string {
	return fmt.Sprintf("Subject=%s Issuer=%s Serial=%d CA=%v DNS=%v Expires=%s",
		c.Subject, c.Issuer, c.SerialNumber, c.IsCA, c.DNSNames, c.NotAfter.Format("2006-01-02"))
}

// --- 인증서 생성 ---

func generateCA(name string) (*CertInfo, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, _ := rand.Int(rand.Reader, big.NewInt(1000000))

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name, Organization: []string{"TestOrg"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}

	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	return &CertInfo{
		Subject:      name,
		Issuer:       name,
		SerialNumber: serial,
		NotBefore:    template.NotBefore,
		NotAfter:     template.NotAfter,
		IsCA:         true,
		PrivateKey:   key,
		Certificate:  cert,
		PEMData:      pemData,
	}, nil
}

func generateCert(name string, dnsNames []string, ca *CertInfo) (*CertInfo, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, _ := rand.Int(rand.Reader, big.NewInt(1000000))

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		DNSNames:     dnsNames,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		return nil, err
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}

	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	return &CertInfo{
		Subject:      name,
		Issuer:       ca.Subject,
		SerialNumber: serial,
		NotBefore:    template.NotBefore,
		NotAfter:     template.NotAfter,
		DNSNames:     dnsNames,
		PrivateKey:   key,
		Certificate:  cert,
		PEMData:      pemData,
	}, nil
}

// --- CRL (Certificate Revocation List) ---

type CRLEntry struct {
	SerialNumber *big.Int
	RevokedAt    time.Time
	Reason       string
}

type CRL struct {
	mu       sync.RWMutex
	Issuer   string
	Entries  []CRLEntry
	Updated  time.Time
}

func NewCRL(issuer string) *CRL {
	return &CRL{Issuer: issuer, Updated: time.Now()}
}

func (c *CRL) Revoke(serial *big.Int, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Entries = append(c.Entries, CRLEntry{
		SerialNumber: serial,
		RevokedAt:    time.Now(),
		Reason:       reason,
	})
	c.Updated = time.Now()
}

func (c *CRL) IsRevoked(serial *big.Int) (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, entry := range c.Entries {
		if entry.SerialNumber.Cmp(serial) == 0 {
			return true, entry.Reason
		}
	}
	return false, ""
}

// --- Custom Verification ---

type VerificationFunc func(cert *CertInfo) error

type CustomVerifier struct {
	checks []struct {
		name string
		fn   VerificationFunc
	}
}

func NewCustomVerifier() *CustomVerifier {
	return &CustomVerifier{}
}

func (v *CustomVerifier) AddCheck(name string, fn VerificationFunc) {
	v.checks = append(v.checks, struct {
		name string
		fn   VerificationFunc
	}{name, fn})
}

func (v *CustomVerifier) Verify(cert *CertInfo) []string {
	var errors []string
	for _, check := range v.checks {
		if err := check.fn(cert); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", check.name, err))
		}
	}
	return errors
}

// --- Certificate Reloader ---

type CertReloader struct {
	mu      sync.RWMutex
	current *CertInfo
	version int
	history []struct {
		version int
		loaded  time.Time
		subject string
	}
}

func NewCertReloader(initial *CertInfo) *CertReloader {
	cr := &CertReloader{current: initial, version: 1}
	cr.history = append(cr.history, struct {
		version int
		loaded  time.Time
		subject string
	}{1, time.Now(), initial.Subject})
	return cr
}

func (r *CertReloader) Reload(newCert *CertInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
	r.current = newCert
	r.history = append(r.history, struct {
		version int
		loaded  time.Time
		subject string
	}{r.version, time.Now(), newCert.Subject})
}

func (r *CertReloader) GetCert() (*CertInfo, int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current, r.version
}

// --- mTLS Handshake 시뮬레이션 ---

type HandshakeResult struct {
	Success       bool
	ServerCert    string
	ClientCert    string
	Error         string
	Checks        []string
}

func simulateMTLSHandshake(
	serverCert, clientCert *CertInfo,
	crl *CRL,
	verifier *CustomVerifier,
) HandshakeResult {
	result := HandshakeResult{
		ServerCert: serverCert.Subject,
		ClientCert: clientCert.Subject,
	}

	// 1. CRL 검사 (서버 인증서)
	if revoked, reason := crl.IsRevoked(serverCert.SerialNumber); revoked {
		result.Error = fmt.Sprintf("server cert revoked: %s", reason)
		return result
	}
	result.Checks = append(result.Checks, "Server CRL check: OK")

	// 2. CRL 검사 (클라이언트 인증서)
	if revoked, reason := crl.IsRevoked(clientCert.SerialNumber); revoked {
		result.Error = fmt.Sprintf("client cert revoked: %s", reason)
		return result
	}
	result.Checks = append(result.Checks, "Client CRL check: OK")

	// 3. 만료 검사
	now := time.Now()
	if now.After(serverCert.NotAfter) {
		result.Error = "server cert expired"
		return result
	}
	if now.After(clientCert.NotAfter) {
		result.Error = "client cert expired"
		return result
	}
	result.Checks = append(result.Checks, "Expiration check: OK")

	// 4. Custom verification
	serverErrors := verifier.Verify(serverCert)
	if len(serverErrors) > 0 {
		result.Error = strings.Join(serverErrors, "; ")
		return result
	}
	clientErrors := verifier.Verify(clientCert)
	if len(clientErrors) > 0 {
		result.Error = strings.Join(clientErrors, "; ")
		return result
	}
	result.Checks = append(result.Checks, "Custom verification: OK")

	// 5. PEM 해시 (무결성)
	serverHash := sha256.Sum256(serverCert.PEMData)
	clientHash := sha256.Sum256(clientCert.PEMData)
	result.Checks = append(result.Checks, fmt.Sprintf("Server cert hash: %x...", serverHash[:4]))
	result.Checks = append(result.Checks, fmt.Sprintf("Client cert hash: %x...", clientHash[:4]))

	result.Success = true
	return result
}

func main() {
	fmt.Println("=== gRPC Advanced TLS 시뮬레이션 ===")
	fmt.Println()

	// --- CA 및 인증서 생성 ---
	fmt.Println("[1] CA 및 인증서 생성")
	fmt.Println(strings.Repeat("-", 60))

	ca, _ := generateCA("Test Root CA")
	fmt.Printf("  CA: %s\n", ca)

	serverCert, _ := generateCert("server-1", []string{"server.example.com", "localhost"}, ca)
	fmt.Printf("  Server: %s\n", serverCert)

	clientCert1, _ := generateCert("client-frontend", []string{"frontend.example.com"}, ca)
	fmt.Printf("  Client1: %s\n", clientCert1)

	clientCert2, _ := generateCert("client-backend", []string{"backend.example.com"}, ca)
	fmt.Printf("  Client2: %s\n", clientCert2)

	clientCertBad, _ := generateCert("client-revoked", []string{"revoked.example.com"}, ca)
	fmt.Printf("  Client3: %s (will be revoked)\n", clientCertBad)
	fmt.Println()

	// --- CRL ---
	fmt.Println("[2] CRL (Certificate Revocation List)")
	fmt.Println(strings.Repeat("-", 60))

	crl := NewCRL(ca.Subject)
	crl.Revoke(clientCertBad.SerialNumber, "key compromise")
	fmt.Printf("  Revoked: serial=%d reason=key compromise\n", clientCertBad.SerialNumber)
	fmt.Printf("  CRL entries: %d\n", len(crl.Entries))
	fmt.Println()

	// --- Custom Verifier ---
	fmt.Println("[3] Custom Verification 규칙")
	fmt.Println(strings.Repeat("-", 60))

	verifier := NewCustomVerifier()
	verifier.AddCheck("dns-check", func(cert *CertInfo) error {
		if len(cert.DNSNames) == 0 && !cert.IsCA {
			return fmt.Errorf("cert has no DNS names")
		}
		return nil
	})
	verifier.AddCheck("key-strength", func(cert *CertInfo) error {
		if cert.PrivateKey != nil && cert.PrivateKey.Curve.Params().BitSize < 256 {
			return fmt.Errorf("key too weak: %d bits", cert.PrivateKey.Curve.Params().BitSize)
		}
		return nil
	})
	fmt.Println("  + dns-check: DNS 이름 필수")
	fmt.Println("  + key-strength: P-256 이상")
	fmt.Println()

	// --- mTLS Handshake 시뮬레이션 ---
	fmt.Println("[4] mTLS Handshake 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	handshakes := []struct {
		server *CertInfo
		client *CertInfo
		desc   string
	}{
		{serverCert, clientCert1, "정상 mTLS (frontend)"},
		{serverCert, clientCert2, "정상 mTLS (backend)"},
		{serverCert, clientCertBad, "폐기된 클라이언트 인증서"},
	}

	for _, hs := range handshakes {
		fmt.Printf("\n  >> %s\n", hs.desc)
		result := simulateMTLSHandshake(hs.server, hs.client, crl, verifier)
		if result.Success {
			fmt.Printf("    [OK] Handshake 성공 (server=%s, client=%s)\n", result.ServerCert, result.ClientCert)
		} else {
			fmt.Printf("    [FAIL] %s\n", result.Error)
		}
		for _, check := range result.Checks {
			fmt.Printf("    - %s\n", check)
		}
	}
	fmt.Println()

	// --- Certificate Reloading ---
	fmt.Println("[5] Certificate Reloading (무중단 교체)")
	fmt.Println(strings.Repeat("-", 60))

	reloader := NewCertReloader(serverCert)
	cert, ver := reloader.GetCert()
	fmt.Printf("  현재: v%d %s (expires: %s)\n", ver, cert.Subject, cert.NotAfter.Format("2006-01-02"))

	// 새 인증서로 교체
	newServerCert, _ := generateCert("server-1-renewed", []string{"server.example.com", "localhost"}, ca)
	reloader.Reload(newServerCert)
	cert, ver = reloader.GetCert()
	fmt.Printf("  교체: v%d %s (expires: %s)\n", ver, cert.Subject, cert.NotAfter.Format("2006-01-02"))

	// 두 번째 교체
	newServerCert2, _ := generateCert("server-1-renewed-2", []string{"server.example.com", "localhost", "server2.example.com"}, ca)
	reloader.Reload(newServerCert2)
	cert, ver = reloader.GetCert()
	fmt.Printf("  교체: v%d %s (expires: %s)\n", ver, cert.Subject, cert.NotAfter.Format("2006-01-02"))

	fmt.Println("\n  교체 이력:")
	for _, h := range reloader.history {
		fmt.Printf("    v%d: %s (%s)\n", h.version, h.subject, h.loaded.Format("15:04:05"))
	}
	fmt.Println()

	// --- 교체 후 Handshake ---
	fmt.Println("[6] 교체 후 mTLS Handshake")
	fmt.Println(strings.Repeat("-", 60))

	currentCert, _ := reloader.GetCert()
	result := simulateMTLSHandshake(currentCert, clientCert1, crl, verifier)
	if result.Success {
		fmt.Printf("  [OK] 새 인증서로 Handshake 성공\n")
	}
	for _, check := range result.Checks {
		fmt.Printf("    - %s\n", check)
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
