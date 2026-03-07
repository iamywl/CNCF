// poc-mtls: SPIFFE 기반 mTLS 핸드셰이크 시뮬레이션
//
// Istio의 mTLS 핸드셰이크를 시뮬레이션한다.
// - Self-signed CA가 SPIFFE URI SAN이 포함된 워크로드 인증서를 발급
// - 클라이언트 프록시와 서버 프록시 간 mTLS 핸드셰이크 수행
// - PeerCertVerifier가 SPIFFE identity 기반으로 인증서를 검증
//
// 참조: istio/pkg/spiffe/spiffe.go, istio/security/pkg/pki/ca/ca.go,
//       istio/security/pkg/server/ca/server.go

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. SPIFFE Identity 모델
//    참조: istio/pkg/spiffe/spiffe.go - Identity struct, ParseIdentity(), String()
// ============================================================================

const (
	spiffeScheme          = "spiffe"
	spiffeURIPrefix       = spiffeScheme + "://"
	namespaceSegment      = "ns"
	serviceAccountSegment = "sa"
)

// Identity는 SPIFFE 워크로드 식별자를 나타낸다.
// Istio에서 모든 워크로드는 spiffe://trust-domain/ns/namespace/sa/service-account 형식의 ID를 가진다.
// 참조: istio/pkg/spiffe/spiffe.go:56-60
type Identity struct {
	TrustDomain    string
	Namespace      string
	ServiceAccount string
}

// String은 SPIFFE URI 형식의 문자열을 반환한다.
// 참조: istio/pkg/spiffe/spiffe.go:80-82
func (id Identity) String() string {
	return spiffeURIPrefix + id.TrustDomain + "/" + namespaceSegment + "/" +
		id.Namespace + "/" + serviceAccountSegment + "/" + id.ServiceAccount
}

// URL은 SPIFFE ID를 *url.URL로 변환한다. x509 인증서의 URI SAN에 사용된다.
func (id Identity) URL() *url.URL {
	return &url.URL{
		Scheme: spiffeScheme,
		Host:   id.TrustDomain,
		Path:   "/" + namespaceSegment + "/" + id.Namespace + "/" + serviceAccountSegment + "/" + id.ServiceAccount,
	}
}

// ParseIdentity는 SPIFFE URI 문자열에서 Identity를 파싱한다.
// 참조: istio/pkg/spiffe/spiffe.go:62-78
func ParseIdentity(s string) (Identity, error) {
	if !strings.HasPrefix(s, spiffeURIPrefix) {
		return Identity{}, fmt.Errorf("identity is not a spiffe format: %s", s)
	}
	parts := strings.Split(s[len(spiffeURIPrefix):], "/")
	if len(parts) != 5 {
		return Identity{}, fmt.Errorf("identity is not a spiffe format (expected 5 segments, got %d): %s", len(parts), s)
	}
	if parts[1] != namespaceSegment || parts[3] != serviceAccountSegment {
		return Identity{}, fmt.Errorf("identity is not a spiffe format (invalid segments): %s", s)
	}
	return Identity{
		TrustDomain:    parts[0],
		Namespace:      parts[2],
		ServiceAccount: parts[4],
	}, nil
}

// GetTrustDomainFromURISAN은 URI SAN에서 trust domain을 추출한다.
// 참조: istio/pkg/spiffe/spiffe.go:158-164
func GetTrustDomainFromURISAN(uriSAN string) (string, error) {
	parsed, err := ParseIdentity(uriSAN)
	if err != nil {
		return "", fmt.Errorf("failed to parse URI SAN %s: %v", uriSAN, err)
	}
	return parsed.TrustDomain, nil
}

// ============================================================================
// 2. 자체 서명 CA (Citadel/istiod CA 시뮬레이션)
//    참조: istio/security/pkg/pki/ca/ca.go - IstioCA, Sign(), GenKeyCert()
//    참조: istio/security/pkg/pki/util/generate_cert.go - GenCertFromCSR()
// ============================================================================

// CertificateAuthority는 Istio의 자체 서명 CA를 시뮬레이션한다.
// 실제 Istio에서는 istiod에 내장된 Citadel CA가 이 역할을 수행한다.
// 참조: istio/security/pkg/pki/ca/ca.go:356-366
type CertificateAuthority struct {
	caCert     *x509.Certificate
	caKey      *ecdsa.PrivateKey
	caCertPEM  []byte
	trustDomain string
}

// NewCA는 자체 서명 CA를 생성한다.
// Istio의 NewSelfSignedDebugIstioCAOptions()와 유사한 흐름:
// 1) ECDSA P256 키 쌍 생성
// 2) 자체 서명 CA 인증서 생성
// 참조: istio/security/pkg/pki/ca/ca.go:246-285
func NewCA(trustDomain string) (*CertificateAuthority, error) {
	// ECDSA P256 키 생성 (Istio 기본값)
	// 참조: istio/security/pkg/pki/util/generate_cert.go:131-135
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("CA 키 생성 실패: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("시리얼 넘버 생성 실패: %v", err)
	}

	// CA 인증서 템플릿 생성
	// 참조: istio/security/pkg/pki/util/generate_cert.go:346-404
	caTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Istio CA (시뮬레이션)"},
			CommonName:   "Istio CA Root - " + trustDomain,
		},
		NotBefore:             time.Now().Add(-2 * time.Minute), // ClockSkewGracePeriod
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            2,
	}

	// 자체 서명
	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("CA 인증서 생성 실패: %v", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("CA 인증서 파싱 실패: %v", err)
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})

	return &CertificateAuthority{
		caCert:      caCert,
		caKey:       caKey,
		caCertPEM:   caCertPEM,
		trustDomain: trustDomain,
	}, nil
}

// CertOpts는 인증서 발급 옵션이다.
// 참조: istio/security/pkg/pki/ca/ca.go:85-98
type CertOpts struct {
	SubjectIDs []string      // SPIFFE URI SAN 목록
	TTL        time.Duration // 인증서 유효기간
	ForCA      bool          // CA 인증서 여부
}

// SignWorkloadCert는 워크로드 인증서를 발급한다.
// CSR을 받아서 SPIFFE URI SAN을 포함한 인증서를 서명한다.
//
// Istio의 실제 흐름:
// 1) 워크로드(envoy)가 CSR을 생성하여 istiod에 전송
// 2) istiod가 호출자 인증 후 SPIFFE ID를 SAN에 삽입하여 서명
// 3) 서명된 인증서 + cert chain을 워크로드에 반환
//
// 참조: istio/security/pkg/pki/ca/ca.go:400-403 (Sign)
// 참조: istio/security/pkg/pki/ca/ca.go:478-516 (sign 내부)
// 참조: istio/security/pkg/server/ca/server.go:75-161 (CreateCertificate)
func (ca *CertificateAuthority) SignWorkloadCert(csrDER []byte, opts CertOpts) ([]byte, []byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, nil, fmt.Errorf("CSR 파싱 실패: %v", err)
	}

	// CSR 서명 검증
	// 참조: istio/security/pkg/pki/ca/ca.go:489-491
	if err := csr.CheckSignature(); err != nil {
		return nil, nil, fmt.Errorf("CSR 서명 검증 실패: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("시리얼 넘버 생성 실패: %v", err)
	}

	// SPIFFE URI SAN 파싱
	// 참조: istio/security/pkg/pki/util/generate_cert.go:305-309
	var uris []*url.URL
	for _, subjectID := range opts.SubjectIDs {
		if strings.HasPrefix(subjectID, spiffeURIPrefix) {
			u, err := url.Parse(subjectID)
			if err != nil {
				return nil, nil, fmt.Errorf("SPIFFE URI 파싱 실패: %v", err)
			}
			uris = append(uris, u)
		}
	}

	// 워크로드 인증서 템플릿 생성
	// 참조: istio/security/pkg/pki/util/generate_cert.go:289-343
	certTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      csr.Subject,
		NotBefore:    time.Now().Add(-2 * time.Minute), // ClockSkewGracePeriod
		NotAfter:     time.Now().Add(opts.TTL),
		// 워크로드 인증서는 서버+클라이언트 인증 모두 사용
		// 참조: istio/security/pkg/pki/util/generate_cert.go:299-302
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:        uris, // SPIFFE URI SAN
	}

	// CA가 인증서 서명
	// 참조: istio/security/pkg/pki/util/generate_cert.go:252
	certDER, err := x509.CreateCertificate(rand.Reader, certTemplate, ca.caCert, csr.PublicKey, ca.caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("인증서 서명 실패: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return certPEM, ca.caCertPEM, nil
}

// ============================================================================
// 3. PeerCertVerifier (SPIFFE 기반 인증서 검증)
//    참조: istio/pkg/spiffe/spiffe.go:259-354 - PeerCertVerifier
// ============================================================================

// PeerCertVerifier는 SPIFFE trust domain 기반으로 피어 인증서를 검증한다.
// Istio는 trust domain별로 별도의 CA cert pool을 유지하여,
// 서로 다른 trust domain 간에도 mTLS를 지원한다.
// 참조: istio/pkg/spiffe/spiffe.go:259-270
type PeerCertVerifier struct {
	generalCertPool *x509.CertPool
	certPools       map[string]*x509.CertPool // trust domain -> cert pool
}

// NewPeerCertVerifier는 새로운 PeerCertVerifier를 생성한다.
// 참조: istio/pkg/spiffe/spiffe.go:266-270
func NewPeerCertVerifier() *PeerCertVerifier {
	return &PeerCertVerifier{
		generalCertPool: x509.NewCertPool(),
		certPools:       make(map[string]*x509.CertPool),
	}
}

// AddMapping은 trust domain에 CA 인증서를 매핑한다.
// 참조: istio/pkg/spiffe/spiffe.go:278-287
func (v *PeerCertVerifier) AddMapping(trustDomain string, certs []*x509.Certificate) {
	if v.certPools[trustDomain] == nil {
		v.certPools[trustDomain] = x509.NewCertPool()
	}
	for _, cert := range certs {
		v.certPools[trustDomain].AddCert(cert)
		v.generalCertPool.AddCert(cert)
	}
	fmt.Printf("    [PeerCertVerifier] trust domain '%s'에 CA 인증서 %d개 등록\n", trustDomain, len(certs))
}

// VerifyPeerCert는 피어의 인증서 체인을 검증한다.
// 핵심 로직:
// 1) 피어 인증서에서 URI SAN 추출 (정확히 1개여야 함)
// 2) URI SAN에서 trust domain 추출
// 3) 해당 trust domain의 cert pool로 인증서 검증
//
// 참조: istio/pkg/spiffe/spiffe.go:319-354
func (v *PeerCertVerifier) VerifyPeerCert(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return nil // 인증서 없으면 스킵 (다른 인증 방식 사용 가능)
	}

	// 첫 번째가 피어 인증서, 나머지는 중간 CA
	var peerCert *x509.Certificate
	intCertPool := x509.NewCertPool()
	for i, rawCert := range rawCerts {
		cert, err := x509.ParseCertificate(rawCert)
		if err != nil {
			return err
		}
		if i == 0 {
			peerCert = cert
		} else {
			intCertPool.AddCert(cert)
		}
	}

	// SPIFFE 규격: URI SAN이 정확히 1개여야 함
	// 참조: istio/pkg/spiffe/spiffe.go:337-339
	if len(peerCert.URIs) != 1 {
		return fmt.Errorf("피어 인증서에 URI SAN이 정확히 1개가 아님 (발견: %d개)", len(peerCert.URIs))
	}

	// trust domain 추출
	trustDomain, err := GetTrustDomainFromURISAN(peerCert.URIs[0].String())
	if err != nil {
		return err
	}

	// trust domain에 해당하는 cert pool로 검증
	// 참조: istio/pkg/spiffe/spiffe.go:344-347
	rootCertPool, ok := v.certPools[trustDomain]
	if !ok {
		return fmt.Errorf("trust domain '%s'에 대한 cert pool이 없음", trustDomain)
	}

	// x509 표준 인증서 체인 검증
	// 참조: istio/pkg/spiffe/spiffe.go:349-353
	_, err = peerCert.Verify(x509.VerifyOptions{
		Roots:         rootCertPool,
		Intermediates: intCertPool,
	})
	return err
}

// ============================================================================
// 4. 워크로드 프록시 (Envoy Sidecar 시뮬레이션)
//    Istio에서 envoy 프록시는 SDS(Secret Discovery Service)를 통해
//    istiod로부터 인증서를 수신하고, mTLS 설정에 사용한다.
// ============================================================================

// WorkloadProxy는 Istio의 envoy sidecar 프록시를 시뮬레이션한다.
type WorkloadProxy struct {
	identity  Identity
	certPEM   []byte
	keyPEM    []byte
	caCertPEM []byte
	tlsCert   tls.Certificate
	caPool    *x509.CertPool
}

// NewWorkloadProxy는 CA에서 인증서를 발급받아 워크로드 프록시를 생성한다.
// Istio 실제 흐름:
// 1) pilot-agent가 CSR 생성
// 2) SDS API를 통해 istiod에 CSR 전송
// 3) istiod가 인증서 발급하여 반환
// 4) envoy가 해당 인증서로 mTLS 설정
func NewWorkloadProxy(ca *CertificateAuthority, id Identity) (*WorkloadProxy, error) {
	// ECDSA P256 키 생성
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("워크로드 키 생성 실패: %v", err)
	}

	// CSR 생성 (실제 Istio에서는 pilot-agent가 수행)
	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			Organization: []string{"Istio Workload"},
		},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, privKey)
	if err != nil {
		return nil, fmt.Errorf("CSR 생성 실패: %v", err)
	}

	// CA에 인증서 발급 요청 (SDS → istiod → Citadel CA)
	// 참조: istio/security/pkg/server/ca/server.go:75-161 (CreateCertificate)
	certPEM, caCertPEM, err := ca.SignWorkloadCert(csrDER, CertOpts{
		SubjectIDs: []string{id.String()}, // SPIFFE URI를 SAN에 포함
		TTL:        24 * time.Hour,
		ForCA:      false,
	})
	if err != nil {
		return nil, fmt.Errorf("인증서 발급 실패: %v", err)
	}

	// 개인키 PEM 인코딩
	privKeyDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("개인키 인코딩 실패: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privKeyDER})

	// TLS 인증서 파싱
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("TLS 키 페어 파싱 실패: %v", err)
	}

	// CA cert pool 구성
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCertPEM)

	return &WorkloadProxy{
		identity:  id,
		certPEM:   certPEM,
		keyPEM:    keyPEM,
		caCertPEM: caCertPEM,
		tlsCert:   tlsCert,
		caPool:    caPool,
	}, nil
}

// ============================================================================
// 5. mTLS 핸드셰이크 데모
// ============================================================================

// runMTLSDemo는 두 워크로드 간의 mTLS 핸드셰이크를 시뮬레이션한다.
func runMTLSDemo(serverProxy, clientProxy *WorkloadProxy, verifier *PeerCertVerifier) error {
	// 서버 TLS 설정
	// Istio에서 envoy는 DownstreamTlsContext에 이 설정을 사용한다
	serverTLSConfig := &tls.Config{
		Certificates: []tls.Certificate{serverProxy.tlsCert},
		ClientAuth:   tls.RequireAnyClientCert, // mTLS: 클라이언트 인증서 필수
		// PeerCertVerifier를 사용하여 SPIFFE 기반 검증
		VerifyPeerCertificate: verifier.VerifyPeerCert,
		MinVersion:            tls.VersionTLS12,
	}

	// 서버 리스너 생성
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig)
	if err != nil {
		return fmt.Errorf("TLS 리스너 생성 실패: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()
	fmt.Printf("    [서버 프록시] %s 에서 mTLS 리스닝 시작 (identity: %s)\n", addr, serverProxy.identity)

	var wg sync.WaitGroup
	wg.Add(1)

	// 서버 고루틴
	go func() {
		defer wg.Done()
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("    [서버 프록시] 연결 수락 실패: %v\n", err)
			return
		}
		defer conn.Close()

		// TLS 핸드셰이크 수행
		tlsConn := conn.(*tls.Conn)
		if err := tlsConn.Handshake(); err != nil {
			fmt.Printf("    [서버 프록시] TLS 핸드셰이크 실패: %v\n", err)
			return
		}

		// 피어 인증서에서 SPIFFE identity 추출
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 && len(state.PeerCertificates[0].URIs) > 0 {
			clientSPIFFE := state.PeerCertificates[0].URIs[0].String()
			fmt.Printf("    [서버 프록시] 클라이언트 SPIFFE ID 확인: %s\n", clientSPIFFE)
		}

		// 데이터 수신
		buf := make([]byte, 1024)
		n, err := tlsConn.Read(buf)
		if err != nil && err != io.EOF {
			fmt.Printf("    [서버 프록시] 데이터 수신 실패: %v\n", err)
			return
		}
		fmt.Printf("    [서버 프록시] 수신 데이터: %s\n", string(buf[:n]))

		// 응답 전송
		response := "mTLS 응답: Hello from " + serverProxy.identity.ServiceAccount
		_, _ = tlsConn.Write([]byte(response))
	}()

	// 클라이언트 TLS 설정
	// Istio에서 envoy는 UpstreamTlsContext에 이 설정을 사용한다
	clientTLSConfig := &tls.Config{
		Certificates:          []tls.Certificate{clientProxy.tlsCert},
		InsecureSkipVerify:    true, // 서버 이름 검증 스킵 (SPIFFE에서는 URI SAN으로 검증)
		VerifyPeerCertificate: verifier.VerifyPeerCert, // SPIFFE 기반 검증
		MinVersion:            tls.VersionTLS12,
	}

	// 클라이언트 연결
	fmt.Printf("    [클라이언트 프록시] %s 로 mTLS 연결 시도 (identity: %s)\n", addr, clientProxy.identity)
	conn, err := tls.Dial("tcp", addr, clientTLSConfig)
	if err != nil {
		return fmt.Errorf("클라이언트 연결 실패: %v", err)
	}
	defer conn.Close()

	// 피어 인증서에서 SPIFFE identity 추출
	state := conn.ConnectionState()
	if len(state.PeerCertificates) > 0 && len(state.PeerCertificates[0].URIs) > 0 {
		serverSPIFFE := state.PeerCertificates[0].URIs[0].String()
		fmt.Printf("    [클라이언트 프록시] 서버 SPIFFE ID 확인: %s\n", serverSPIFFE)
	}
	fmt.Printf("    [클라이언트 프록시] TLS 버전: %s, 암호화: %s\n",
		tlsVersionName(state.Version), tls.CipherSuiteName(state.CipherSuite))

	// 데이터 전송
	message := "mTLS 요청: Hello from " + clientProxy.identity.ServiceAccount
	_, err = conn.Write([]byte(message))
	if err != nil {
		return fmt.Errorf("데이터 전송 실패: %v", err)
	}

	// 응답 수신
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		return fmt.Errorf("응답 수신 실패: %v", err)
	}
	fmt.Printf("    [클라이언트 프록시] 수신 응답: %s\n", string(buf[:n]))

	wg.Wait()
	return nil
}

// tlsVersionName은 TLS 버전 번호를 이름으로 변환한다.
func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("Unknown(0x%04x)", version)
	}
}

// ============================================================================
// 6. Trust Domain 교차 검증 (Cross Trust Domain) 테스트
// ============================================================================

// runCrossTrustDomainTest는 서로 다른 trust domain 간의 mTLS 실패를 검증한다.
func runCrossTrustDomainTest(ca1, ca2 *CertificateAuthority) error {
	// ca1에서 발급받은 서버
	serverID := Identity{TrustDomain: ca1.trustDomain, Namespace: "prod", ServiceAccount: "api-server"}
	serverProxy, err := NewWorkloadProxy(ca1, serverID)
	if err != nil {
		return err
	}

	// ca2에서 발급받은 클라이언트 (다른 trust domain)
	clientID := Identity{TrustDomain: ca2.trustDomain, Namespace: "prod", ServiceAccount: "web-client"}
	clientProxy, err := NewWorkloadProxy(ca2, clientID)
	if err != nil {
		return err
	}

	// ca1의 cert만 등록한 verifier (ca2를 모르는 상태)
	verifier := NewPeerCertVerifier()
	verifier.AddMapping(ca1.trustDomain, []*x509.Certificate{ca1.caCert})
	// ca2의 trust domain은 등록하지 않음

	// 서버 시작
	serverTLSConfig := &tls.Config{
		Certificates:          []tls.Certificate{serverProxy.tlsCert},
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verifier.VerifyPeerCert,
		MinVersion:            tls.VersionTLS12,
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig)
	if err != nil {
		return err
	}
	defer listener.Close()

	addr := listener.Addr().String()

	var serverErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := listener.Accept()
		if err != nil {
			serverErr = err
			return
		}
		defer conn.Close()
		tlsConn := conn.(*tls.Conn)
		serverErr = tlsConn.Handshake()
	}()

	// 클라이언트 연결 시도
	clientVerifier := NewPeerCertVerifier()
	clientVerifier.AddMapping(ca2.trustDomain, []*x509.Certificate{ca2.caCert})

	clientTLSConfig := &tls.Config{
		Certificates:          []tls.Certificate{clientProxy.tlsCert},
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: clientVerifier.VerifyPeerCert,
		MinVersion:            tls.VersionTLS12,
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("TCP 연결 실패: %v", err)
	}

	tlsConn := tls.Client(conn, clientTLSConfig)
	handshakeErr := tlsConn.Handshake()
	tlsConn.Close()

	wg.Wait()

	// 양쪽 모두 핸드셰이크 실패가 예상됨
	if serverErr != nil {
		fmt.Printf("    [결과] 서버 측 핸드셰이크 실패 (예상대로): %v\n", serverErr)
	}
	if handshakeErr != nil {
		fmt.Printf("    [결과] 클라이언트 측 핸드셰이크 실패 (예상대로): %v\n", handshakeErr)
	}

	return nil
}

// ============================================================================
// 7. Trust Domain 연합 (Federation) 테스트
// ============================================================================

// runTrustDomainFederationTest는 trust domain 연합을 통한 cross-cluster mTLS를 시뮬레이션한다.
func runTrustDomainFederationTest(ca1, ca2 *CertificateAuthority) error {
	serverID := Identity{TrustDomain: ca1.trustDomain, Namespace: "prod", ServiceAccount: "api-server"}
	serverProxy, err := NewWorkloadProxy(ca1, serverID)
	if err != nil {
		return err
	}

	clientID := Identity{TrustDomain: ca2.trustDomain, Namespace: "prod", ServiceAccount: "web-client"}
	clientProxy, err := NewWorkloadProxy(ca2, clientID)
	if err != nil {
		return err
	}

	// 양쪽 trust domain의 CA를 모두 등록한 verifier (trust domain 연합)
	// 참조: istio/pkg/spiffe/spiffe.go:310-315 (AddMappings)
	federatedVerifier := NewPeerCertVerifier()
	federatedVerifier.AddMapping(ca1.trustDomain, []*x509.Certificate{ca1.caCert})
	federatedVerifier.AddMapping(ca2.trustDomain, []*x509.Certificate{ca2.caCert})

	return runMTLSDemo(serverProxy, clientProxy, federatedVerifier)
}

// ============================================================================
// 8. 인증서 정보 출력 유틸리티
// ============================================================================

func printCertInfo(label string, certPEM []byte) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		fmt.Printf("    [%s] PEM 디코딩 실패\n", label)
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		fmt.Printf("    [%s] 인증서 파싱 실패: %v\n", label, err)
		return
	}

	fmt.Printf("    [%s 인증서 정보]\n", label)
	fmt.Printf("      Subject:    %s\n", cert.Subject.CommonName)
	fmt.Printf("      Issuer:     %s\n", cert.Issuer.CommonName)
	fmt.Printf("      Serial:     %s\n", cert.SerialNumber.Text(16)[:16]+"...")
	fmt.Printf("      NotBefore:  %s\n", cert.NotBefore.Format(time.RFC3339))
	fmt.Printf("      NotAfter:   %s\n", cert.NotAfter.Format(time.RFC3339))
	fmt.Printf("      IsCA:       %v\n", cert.IsCA)
	fmt.Printf("      KeyUsage:   %v\n", keyUsageString(cert.KeyUsage))

	if len(cert.URIs) > 0 {
		fmt.Printf("      URI SAN:    ")
		for i, u := range cert.URIs {
			if i > 0 {
				fmt.Printf(", ")
			}
			fmt.Printf("%s", u.String())
		}
		fmt.Println()

		// SPIFFE identity 파싱
		id, err := ParseIdentity(cert.URIs[0].String())
		if err == nil {
			fmt.Printf("      SPIFFE ID:\n")
			fmt.Printf("        Trust Domain:    %s\n", id.TrustDomain)
			fmt.Printf("        Namespace:       %s\n", id.Namespace)
			fmt.Printf("        Service Account: %s\n", id.ServiceAccount)
		}
	}
	fmt.Println()
}

func keyUsageString(ku x509.KeyUsage) string {
	var usages []string
	if ku&x509.KeyUsageDigitalSignature != 0 {
		usages = append(usages, "DigitalSignature")
	}
	if ku&x509.KeyUsageKeyEncipherment != 0 {
		usages = append(usages, "KeyEncipherment")
	}
	if ku&x509.KeyUsageCertSign != 0 {
		usages = append(usages, "CertSign")
	}
	if ku&x509.KeyUsageCRLSign != 0 {
		usages = append(usages, "CRLSign")
	}
	if len(usages) == 0 {
		return "None"
	}
	return strings.Join(usages, ", ")
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" Istio mTLS 핸드셰이크 시뮬레이션 (SPIFFE 기반)")
	fmt.Println("==========================================================")
	fmt.Println()

	// --------------------------------------------------------
	// 단계 1: CA 생성
	// --------------------------------------------------------
	fmt.Println("[단계 1] Self-Signed CA 생성 (Citadel/istiod CA 시뮬레이션)")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  Istio에서 istiod는 자체 서명 CA를 내장하고 있으며,")
	fmt.Println("  모든 워크로드의 인증서를 발급하는 역할을 한다.")
	fmt.Println("  참조: security/pkg/pki/ca/ca.go - IstioCA 구조체")
	fmt.Println()

	ca1, err := NewCA("cluster.local")
	if err != nil {
		panic(err)
	}
	fmt.Println("  CA 'cluster.local' 생성 완료")
	printCertInfo("CA", ca1.caCertPEM)

	// --------------------------------------------------------
	// 단계 2: 워크로드 인증서 발급
	// --------------------------------------------------------
	fmt.Println("[단계 2] 워크로드 인증서 발급 (SPIFFE URI SAN 포함)")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  실제 Istio 흐름:")
	fmt.Println("  1) pilot-agent가 CSR 생성")
	fmt.Println("  2) SDS(Secret Discovery Service)를 통해 istiod에 CSR 전송")
	fmt.Println("  3) istiod가 호출자 인증 후 SPIFFE ID를 SAN에 삽입하여 서명")
	fmt.Println("  4) 서명된 인증서를 envoy에게 반환")
	fmt.Println("  참조: security/pkg/server/ca/server.go - CreateCertificate()")
	fmt.Println()

	serverID := Identity{
		TrustDomain:    "cluster.local",
		Namespace:      "default",
		ServiceAccount: "reviews",
	}
	clientID := Identity{
		TrustDomain:    "cluster.local",
		Namespace:      "default",
		ServiceAccount: "productpage",
	}

	fmt.Printf("  서버 SPIFFE ID: %s\n", serverID)
	fmt.Printf("  클라이언트 SPIFFE ID: %s\n\n", clientID)

	serverProxy, err := NewWorkloadProxy(ca1, serverID)
	if err != nil {
		panic(err)
	}
	printCertInfo("서버 워크로드", serverProxy.certPEM)

	clientProxy, err := NewWorkloadProxy(ca1, clientID)
	if err != nil {
		panic(err)
	}
	printCertInfo("클라이언트 워크로드", clientProxy.certPEM)

	// --------------------------------------------------------
	// 단계 3: PeerCertVerifier 구성
	// --------------------------------------------------------
	fmt.Println("[단계 3] PeerCertVerifier 구성 (Trust Domain 기반 검증)")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  Istio의 PeerCertVerifier는 trust domain별로 CA cert pool을 유지한다.")
	fmt.Println("  피어 인증서의 URI SAN에서 trust domain을 추출하여 해당 pool로 검증한다.")
	fmt.Println("  참조: pkg/spiffe/spiffe.go - PeerCertVerifier")
	fmt.Println()

	verifier := NewPeerCertVerifier()
	verifier.AddMapping("cluster.local", []*x509.Certificate{ca1.caCert})
	fmt.Println()

	// --------------------------------------------------------
	// 단계 4: mTLS 핸드셰이크
	// --------------------------------------------------------
	fmt.Println("[단계 4] mTLS 핸드셰이크 수행")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  양쪽 프록시가 인증서를 교환하고 SPIFFE identity를 검증한다.")
	fmt.Println("  Istio에서는 envoy가 이 과정을 자동으로 수행한다.")
	fmt.Println()

	if err := runMTLSDemo(serverProxy, clientProxy, verifier); err != nil {
		fmt.Printf("  mTLS 핸드셰이크 실패: %v\n", err)
	}
	fmt.Println()

	// --------------------------------------------------------
	// 단계 5: SPIFFE Identity 파싱 데모
	// --------------------------------------------------------
	fmt.Println("[단계 5] SPIFFE Identity 파싱 및 검증")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  참조: pkg/spiffe/spiffe.go - ParseIdentity(), ExpandWithTrustDomains()")
	fmt.Println()

	testURIs := []string{
		"spiffe://cluster.local/ns/default/sa/reviews",
		"spiffe://cluster.local/ns/istio-system/sa/istiod",
		"spiffe://remote-cluster.example.com/ns/prod/sa/payment-service",
		"invalid-uri",
		"spiffe://cluster.local/wrong/format",
	}

	for _, uri := range testURIs {
		id, err := ParseIdentity(uri)
		if err != nil {
			fmt.Printf("    [FAIL] %s\n           -> %v\n", uri, err)
		} else {
			fmt.Printf("    [OK]   %s\n           -> TrustDomain=%s, Namespace=%s, SA=%s\n",
				uri, id.TrustDomain, id.Namespace, id.ServiceAccount)
		}
	}
	fmt.Println()

	// --------------------------------------------------------
	// 단계 6: Cross Trust Domain 테스트 (실패 케이스)
	// --------------------------------------------------------
	fmt.Println("[단계 6] Cross Trust Domain 테스트 (실패 케이스)")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  서로 다른 trust domain의 CA에서 발급받은 인증서로 mTLS를 시도한다.")
	fmt.Println("  상대방 trust domain의 CA를 모르므로 핸드셰이크가 실패해야 한다.")
	fmt.Println()

	ca2, err := NewCA("remote-cluster.example.com")
	if err != nil {
		panic(err)
	}
	fmt.Println("  CA 'remote-cluster.example.com' 생성 완료")
	fmt.Println()

	if err := runCrossTrustDomainTest(ca1, ca2); err != nil {
		fmt.Printf("  테스트 실행 오류: %v\n", err)
	}
	fmt.Println()

	// --------------------------------------------------------
	// 단계 7: Trust Domain 연합 (Federation)
	// --------------------------------------------------------
	fmt.Println("[단계 7] Trust Domain 연합 (Federation)")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  양쪽 trust domain의 CA를 verifier에 모두 등록하면")
	fmt.Println("  서로 다른 trust domain 간에도 mTLS가 성공한다.")
	fmt.Println("  이것이 Istio의 multi-cluster 멀티 trust domain 연합 방식이다.")
	fmt.Println("  참조: pkg/spiffe/spiffe.go - RetrieveSpiffeBundleRootCerts()")
	fmt.Println()

	if err := runTrustDomainFederationTest(ca1, ca2); err != nil {
		fmt.Printf("  연합 mTLS 실패: %v\n", err)
	}
	fmt.Println()

	// --------------------------------------------------------
	// 요약
	// --------------------------------------------------------
	fmt.Println("==========================================================")
	fmt.Println(" 요약: Istio mTLS 핵심 메커니즘")
	fmt.Println("==========================================================")
	fmt.Println()
	fmt.Println("  1. SPIFFE Identity")
	fmt.Println("     - 형식: spiffe://trust-domain/ns/namespace/sa/service-account")
	fmt.Println("     - x509 인증서의 URI SAN에 SPIFFE ID를 삽입")
	fmt.Println("     - trust domain별로 CA cert pool을 분리하여 관리")
	fmt.Println()
	fmt.Println("  2. Certificate Authority (Citadel)")
	fmt.Println("     - istiod에 내장된 자체 서명 CA")
	fmt.Println("     - SDS API를 통해 워크로드에 인증서 발급")
	fmt.Println("     - CSR 기반 인증서 서명 (ECDSA P256 기본)")
	fmt.Println()
	fmt.Println("  3. mTLS 핸드셰이크")
	fmt.Println("     - envoy 프록시가 자동으로 mTLS 수행")
	fmt.Println("     - PeerCertVerifier가 SPIFFE URI SAN 기반 검증")
	fmt.Println("     - trust domain 매칭으로 교차 도메인 보안 확보")
	fmt.Println()
	fmt.Println("  4. Trust Domain Federation")
	fmt.Println("     - 복수 trust domain의 CA cert을 verifier에 등록")
	fmt.Println("     - multi-cluster 환경에서 cross-cluster mTLS 가능")
	fmt.Println()
}
