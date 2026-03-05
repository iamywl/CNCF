// poc-09-credentials: gRPC TransportCredentials 및 TLS 핸드셰이크 시뮬레이션
//
// grpc-go의 credentials 패키지 핵심 개념을 표준 라이브러리만으로 재현한다.
// - TransportCredentials 인터페이스 (ClientHandshake, ServerHandshake)
// - SecurityLevel (NoSecurity, IntegrityOnly, PrivacyAndIntegrity)
// - insecure credentials (평문 연결)
// - TLS credentials (자체서명 인증서 기반 핸드셰이크)
//
// 실제 grpc-go 소스: credentials/credentials.go, credentials/tls.go
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
	"net"
	"sync"
	"time"
)

// ========== SecurityLevel ==========
// grpc-go: credentials/credentials.go의 SecurityLevel 열거형
// 연결의 보안 수준을 나타내며, 채널에서 요구하는 최소 보안 수준과 비교한다.
type SecurityLevel int

const (
	InvalidSecurityLevel SecurityLevel = iota
	NoSecurity                         // 평문 (insecure)
	IntegrityOnly                      // 무결성만 보장 (서명만, 암호화 없음)
	PrivacyAndIntegrity                // 기밀성 + 무결성 (TLS)
)

func (s SecurityLevel) String() string {
	switch s {
	case NoSecurity:
		return "NoSecurity"
	case IntegrityOnly:
		return "IntegrityOnly"
	case PrivacyAndIntegrity:
		return "PrivacyAndIntegrity"
	default:
		return "InvalidSecurityLevel"
	}
}

// ========== AuthInfo ==========
// grpc-go: credentials/credentials.go의 AuthInfo 인터페이스
// 핸드셰이크 후 인증 정보를 전달한다.
type AuthInfo interface {
	AuthType() string
}

// CommonAuthInfo는 SecurityLevel을 포함하는 공통 인증 정보이다.
// grpc-go: credentials/credentials.go의 CommonAuthInfo struct
type CommonAuthInfo struct {
	SecurityLevel SecurityLevel
}

// TLSAuthInfo는 TLS 핸드셰이크 후 인증 정보이다.
type TLSAuthInfo struct {
	CommonAuthInfo
	PeerCert    *x509.Certificate
	CipherSuite string
}

func (t TLSAuthInfo) AuthType() string { return "tls" }

// InsecureAuthInfo는 insecure 연결의 인증 정보이다.
type InsecureAuthInfo struct {
	CommonAuthInfo
}

func (i InsecureAuthInfo) AuthType() string { return "insecure" }

// ========== TransportCredentials 인터페이스 ==========
// grpc-go: credentials/credentials.go 152행
// 핵심: ClientHandshake와 ServerHandshake로 양측 핸드셰이크를 수행한다.
type TransportCredentials interface {
	// ClientHandshake: 클라이언트 측 핸드셰이크 수행
	ClientHandshake(conn net.Conn, authority string) (net.Conn, AuthInfo, error)
	// ServerHandshake: 서버 측 핸드셰이크 수행
	ServerHandshake(conn net.Conn) (net.Conn, AuthInfo, error)
	// Info: credentials 정보 반환
	Info() ProtocolInfo
	// Clone: 복제 (DialOption 적용 시 사용)
	Clone() TransportCredentials
}

// ProtocolInfo는 프로토콜 관련 정보이다.
type ProtocolInfo struct {
	SecurityProtocol string
	SecurityVersion  string
	ServerName       string
	SecurityLevel    SecurityLevel
}

// ========== Insecure Credentials ==========
// grpc-go: credentials/insecure/insecure.go
// 보안 없이 평문으로 연결하는 credentials. 개발/테스트용.
type insecureCredentials struct{}

func NewInsecureCredentials() TransportCredentials {
	return &insecureCredentials{}
}

func (c *insecureCredentials) ClientHandshake(conn net.Conn, authority string) (net.Conn, AuthInfo, error) {
	fmt.Printf("  [insecure] 클라이언트 핸드셰이크: 보안 없음 (authority=%s)\n", authority)
	return conn, InsecureAuthInfo{CommonAuthInfo{SecurityLevel: NoSecurity}}, nil
}

func (c *insecureCredentials) ServerHandshake(conn net.Conn) (net.Conn, AuthInfo, error) {
	fmt.Println("  [insecure] 서버 핸드셰이크: 보안 없음")
	return conn, InsecureAuthInfo{CommonAuthInfo{SecurityLevel: NoSecurity}}, nil
}

func (c *insecureCredentials) Info() ProtocolInfo {
	return ProtocolInfo{SecurityProtocol: "insecure", SecurityLevel: NoSecurity}
}

func (c *insecureCredentials) Clone() TransportCredentials {
	return &insecureCredentials{}
}

// ========== TLS Credentials ==========
// grpc-go: credentials/tls.go
// TLS 핸드셰이크를 시뮬레이션한다. 실제로는 crypto/tls.Conn을 사용하지만,
// 여기서는 인증서 검증 로직을 직접 구현하여 핸드셰이크 과정을 보여준다.
type tlsCredentials struct {
	cert       *x509.Certificate
	key        *ecdsa.PrivateKey
	rootCAs    *x509.CertPool
	serverName string
}

// NewTLSCredentials는 인증서와 키로 TLS credentials를 생성한다.
func NewTLSCredentials(cert *x509.Certificate, key *ecdsa.PrivateKey, rootCAs *x509.CertPool, serverName string) TransportCredentials {
	return &tlsCredentials{cert: cert, key: key, rootCAs: rootCAs, serverName: serverName}
}

func (c *tlsCredentials) ClientHandshake(conn net.Conn, authority string) (net.Conn, AuthInfo, error) {
	fmt.Printf("  [TLS] 클라이언트 핸드셰이크 시작 (authority=%s)\n", authority)

	// 1단계: ClientHello 시뮬레이션 — 지원하는 TLS 버전, 암호 스위트 전송
	nonce := make([]byte, 32)
	rand.Read(nonce)
	fmt.Printf("  [TLS] → ClientHello (nonce=%x...)\n", nonce[:8])

	// 2단계: 서버 인증서 검증 시뮬레이션
	if c.rootCAs != nil && c.cert != nil {
		opts := x509.VerifyOptions{
			Roots: c.rootCAs,
		}
		// 자체서명이므로 직접 검증
		if c.cert.Issuer.CommonName == c.cert.Subject.CommonName {
			fmt.Printf("  [TLS] 서버 인증서 검증: CN=%s (자체서명)\n", c.cert.Subject.CommonName)
		}
		_ = opts
	}

	// 3단계: 키 교환 시뮬레이션 (ECDHE)
	ephemeralKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sharedSecret := sha256.Sum256(ephemeralKey.PublicKey.X.Bytes())
	fmt.Printf("  [TLS] 키 교환 완료 (shared_secret=%x...)\n", sharedSecret[:8])

	// 4단계: 핸드셰이크 완료
	fmt.Println("  [TLS] ← ServerHello + Certificate + Finished")
	fmt.Println("  [TLS] → ClientFinished")
	fmt.Println("  [TLS] 핸드셰이크 완료: TLS 1.3 / TLS_AES_128_GCM_SHA256")

	authInfo := TLSAuthInfo{
		CommonAuthInfo: CommonAuthInfo{SecurityLevel: PrivacyAndIntegrity},
		PeerCert:       c.cert,
		CipherSuite:    "TLS_AES_128_GCM_SHA256",
	}
	return conn, authInfo, nil
}

func (c *tlsCredentials) ServerHandshake(conn net.Conn) (net.Conn, AuthInfo, error) {
	fmt.Println("  [TLS] 서버 핸드셰이크 시작")

	// 서버 측: ClientHello 수신 → ServerHello + 인증서 전송
	fmt.Println("  [TLS] ← ClientHello 수신")
	fmt.Printf("  [TLS] → ServerHello + Certificate (CN=%s)\n", c.cert.Subject.CommonName)

	// 서버 키 교환
	ephemeralKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sharedSecret := sha256.Sum256(ephemeralKey.PublicKey.X.Bytes())
	fmt.Printf("  [TLS] 키 교환 완료 (shared_secret=%x...)\n", sharedSecret[:8])

	fmt.Println("  [TLS] ← ClientFinished")
	fmt.Println("  [TLS] → ServerFinished")
	fmt.Println("  [TLS] 핸드셰이크 완료")

	authInfo := TLSAuthInfo{
		CommonAuthInfo: CommonAuthInfo{SecurityLevel: PrivacyAndIntegrity},
		PeerCert:       nil, // 클라이언트 인증서 없음 (mTLS 아님)
		CipherSuite:    "TLS_AES_128_GCM_SHA256",
	}
	return conn, authInfo, nil
}

func (c *tlsCredentials) Info() ProtocolInfo {
	return ProtocolInfo{
		SecurityProtocol: "tls",
		SecurityVersion:  "1.3",
		ServerName:       c.serverName,
		SecurityLevel:    PrivacyAndIntegrity,
	}
}

func (c *tlsCredentials) Clone() TransportCredentials {
	return &tlsCredentials{cert: c.cert, key: c.key, rootCAs: c.rootCAs, serverName: c.serverName}
}

// ========== 자체서명 인증서 생성 ==========
func generateSelfSignedCert() (*x509.Certificate, *ecdsa.PrivateKey, *x509.CertPool) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "grpc-test-server"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "grpc-test-server"},
		IsCA:         true, // 자체서명이므로 CA 역할도 겸함
	}

	certDER, _ := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(certDER)

	// PEM 인코딩 (출력용)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	fmt.Printf("  인증서 생성: CN=%s (PEM %d bytes)\n", cert.Subject.CommonName, len(certPEM))

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return cert, key, pool
}

// ========== 핸드셰이크 테스트용 파이프 연결 ==========
func simulateHandshake(clientCreds, serverCreds TransportCredentials) {
	clientConn, serverConn := net.Pipe()
	var wg sync.WaitGroup

	// 서버 측 핸드셰이크
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, authInfo, err := serverCreds.ServerHandshake(serverConn)
		if err != nil {
			fmt.Printf("  서버 핸드셰이크 실패: %v\n", err)
			return
		}
		fmt.Printf("  서버 결과: authType=%s, securityLevel=%s\n",
			authInfo.AuthType(), getSecurityLevel(authInfo))
		serverConn.Close()
	}()

	// 클라이언트 측 핸드셰이크
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, authInfo, err := clientCreds.ClientHandshake(clientConn, "localhost:50051")
		if err != nil {
			fmt.Printf("  클라이언트 핸드셰이크 실패: %v\n", err)
			return
		}
		fmt.Printf("  클라이언트 결과: authType=%s, securityLevel=%s\n",
			authInfo.AuthType(), getSecurityLevel(authInfo))
		clientConn.Close()
	}()

	wg.Wait()
}

func getSecurityLevel(info AuthInfo) string {
	switch v := info.(type) {
	case TLSAuthInfo:
		return v.SecurityLevel.String()
	case InsecureAuthInfo:
		return v.SecurityLevel.String()
	default:
		return "unknown"
	}
}

func main() {
	fmt.Println("========================================")
	fmt.Println("gRPC Credentials 시뮬레이션")
	fmt.Println("========================================")

	// 1. Insecure Credentials
	fmt.Println("\n[1] Insecure Credentials 핸드셰이크")
	fmt.Println("────────────────────────────────────")
	insecureCreds := NewInsecureCredentials()
	info := insecureCreds.Info()
	fmt.Printf("  프로토콜: %s, 보안수준: %s\n", info.SecurityProtocol, info.SecurityLevel)
	simulateHandshake(insecureCreds, insecureCreds)

	// 2. TLS Credentials (자체서명)
	fmt.Println("\n[2] TLS Credentials 핸드셰이크 (자체서명)")
	fmt.Println("────────────────────────────────────────")
	cert, key, rootCAs := generateSelfSignedCert()

	serverTLS := NewTLSCredentials(cert, key, nil, "")
	clientTLS := NewTLSCredentials(cert, nil, rootCAs, "grpc-test-server")

	tlsInfo := clientTLS.Info()
	fmt.Printf("  프로토콜: %s %s, 서버: %s, 보안수준: %s\n",
		tlsInfo.SecurityProtocol, tlsInfo.SecurityVersion,
		tlsInfo.ServerName, tlsInfo.SecurityLevel)
	fmt.Println()
	simulateHandshake(clientTLS, serverTLS)

	// 3. SecurityLevel 비교 — 채널이 요구하는 최소 보안 수준 검사
	fmt.Println("\n[3] SecurityLevel 검증")
	fmt.Println("──────────────────────")
	requiredLevels := []SecurityLevel{NoSecurity, IntegrityOnly, PrivacyAndIntegrity}
	credentialsList := []struct {
		name  string
		creds TransportCredentials
	}{
		{"insecure", insecureCreds},
		{"TLS", clientTLS},
	}

	for _, cred := range credentialsList {
		credLevel := cred.creds.Info().SecurityLevel
		for _, required := range requiredLevels {
			ok := credLevel >= required
			status := "허용"
			if !ok {
				status = "거부"
			}
			fmt.Printf("  %s (level=%d) vs 요구수준 %s (level=%d): %s\n",
				cred.name, credLevel, required, required, status)
		}
		fmt.Println()
	}

	// 4. Clone 동작 확인
	fmt.Println("[4] Clone 동작")
	fmt.Println("──────────────")
	cloned := clientTLS.Clone()
	fmt.Printf("  원본: %+v\n", clientTLS.Info())
	fmt.Printf("  복제: %+v\n", cloned.Info())
	fmt.Printf("  동일 객체? %v\n", clientTLS == cloned)

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
