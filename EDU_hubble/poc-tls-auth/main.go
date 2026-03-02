// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble mTLS/TLS 인증 패턴
//
// Hubble Relay는 gRPC 통신에 mTLS를 사용합니다:
//   - Server ↔ Client 양방향 인증 (mutual TLS)
//   - 최소 TLS 1.3 강제
//   - 인증서 동적 리로딩 (certloader 패턴)
//
// 이 PoC는 자체 서명 인증서를 생성하여 TLS/mTLS 동작을 시연합니다.
//
// 실행: go run main.go

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
	"sync"
	"time"
)

// ========================================
// 1. 인증서 생성 (CA, Server, Client)
// ========================================

// CertBundle은 인증서와 개인키를 담습니다.
type CertBundle struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte
}

// generateCA는 자체 서명 CA 인증서를 생성합니다.
// 실제 Hubble에서는 Cilium의 CA 또는 cert-manager를 사용합니다.
func generateCA() (*CertBundle, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Hubble CA"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	cert, _ := x509.ParseCertificate(certDER)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return &CertBundle{Cert: cert, Key: key, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// generateCert는 CA로 서명된 인증서를 생성합니다.
func generateCert(ca *CertBundle, cn string, isServer bool) (*CertBundle, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
	}

	if isServer {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		template.DNSNames = []string{"localhost", "hubble-relay.cilium.io"}
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}

	cert, _ := x509.ParseCertificate(certDER)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return &CertBundle{Cert: cert, Key: key, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// ========================================
// 2. TLS Config 빌더 (Hubble의 certloader 패턴)
// ========================================

// MinTLSVersion은 Hubble이 강제하는 최소 TLS 버전입니다.
// 실제 코드: var MinTLSVersion uint16 = tls.VersionTLS13
var MinTLSVersion uint16 = tls.VersionTLS13

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("Unknown(0x%04x)", v)
	}
}

// buildServerTLSConfig는 서버용 TLS 설정을 구성합니다.
// mTLS=true이면 클라이언트 인증서도 검증합니다.
func buildServerTLSConfig(ca *CertBundle, server *CertBundle, mTLS bool) *tls.Config {
	serverCert, _ := tls.X509KeyPair(server.CertPEM, server.KeyPEM)

	caPool := x509.NewCertPool()
	caPool.AddCert(ca.Cert)

	cfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   MinTLSVersion,
	}

	if mTLS {
		cfg.ClientCAs = caPool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return cfg
}

// buildClientTLSConfig는 클라이언트용 TLS 설정을 구성합니다.
func buildClientTLSConfig(ca *CertBundle, client *CertBundle, serverName string) *tls.Config {
	caPool := x509.NewCertPool()
	caPool.AddCert(ca.Cert)

	cfg := &tls.Config{
		RootCAs:    caPool,
		ServerName: serverName,
		MinVersion: MinTLSVersion,
	}

	if client != nil {
		clientCert, _ := tls.X509KeyPair(client.CertPEM, client.KeyPEM)
		cfg.Certificates = []tls.Certificate{clientCert}
	}

	return cfg
}

// ========================================
// 3. TLS 서버/클라이언트 시뮬레이션
// ========================================

func runTLSServer(listener net.Listener, wg *sync.WaitGroup, label string) {
	defer wg.Done()

	conn, err := listener.Accept()
	if err != nil {
		fmt.Printf("    [Server] Accept 에러: %v\n", err)
		return
	}
	defer conn.Close()

	tlsConn := conn.(*tls.Conn)
	if err := tlsConn.Handshake(); err != nil {
		fmt.Printf("    [Server] TLS Handshake 실패: %v\n", err)
		return
	}

	state := tlsConn.ConnectionState()
	fmt.Printf("    [Server] TLS 연결 성공!\n")
	fmt.Printf("    [Server] TLS 버전: %s\n", tlsVersionName(state.Version))
	fmt.Printf("    [Server] 암호 스위트: %s\n", tls.CipherSuiteName(state.CipherSuite))

	if len(state.PeerCertificates) > 0 {
		fmt.Printf("    [Server] 클라이언트 인증서: CN=%s (mTLS 성공)\n",
			state.PeerCertificates[0].Subject.CommonName)
	} else {
		fmt.Printf("    [Server] 클라이언트 인증서: 없음 (단방향 TLS)\n")
	}

	// 간단한 메시지 교환
	conn.Write([]byte("Hello from Hubble Relay!"))
}

func runTLSClient(addr string, tlsConfig *tls.Config, label string) (string, error) {
	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	state := conn.ConnectionState()
	fmt.Printf("    [Client] TLS 연결 성공!\n")
	fmt.Printf("    [Client] TLS 버전: %s\n", tlsVersionName(state.Version))
	fmt.Printf("    [Client] 서버 인증서: CN=%s\n",
		state.PeerCertificates[0].Subject.CommonName)

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(buf[:n]), nil
}

// ========================================
// 4. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble mTLS/TLS 인증 패턴 ===")
	fmt.Println()
	fmt.Println("Hubble의 TLS 계층:")
	fmt.Println("  CLI ──TLS──→ Relay ──mTLS──→ Server")
	fmt.Println("  - CLI→Relay: 단방향 TLS (서버 인증서만 검증)")
	fmt.Println("  - Relay→Server: mTLS (양방향 인증)")
	fmt.Println("  - 최소 TLS 1.3 강제")
	fmt.Println()

	// 인증서 생성
	fmt.Println("── 1단계: 인증서 생성 ──")
	fmt.Println()

	ca, _ := generateCA()
	fmt.Printf("  CA 생성: CN=%s\n", ca.Cert.Subject.CommonName)

	serverCert, _ := generateCert(ca, "hubble-relay", true)
	fmt.Printf("  Server 인증서: CN=%s (SAN: localhost, 127.0.0.1)\n", serverCert.Cert.Subject.CommonName)

	clientCert, _ := generateCert(ca, "hubble-cli", false)
	fmt.Printf("  Client 인증서: CN=%s\n", clientCert.Cert.Subject.CommonName)
	fmt.Println()

	// ── 시나리오 1: 단방향 TLS ──
	fmt.Println("━━━ 시나리오 1: 단방향 TLS (CLI → Relay) ━━━")
	fmt.Println("  서버 인증서만 검증, 클라이언트 인증서 불요")
	fmt.Println()

	serverTLS1 := buildServerTLSConfig(ca, serverCert, false)
	listener1, _ := tls.Listen("tcp", "127.0.0.1:0", serverTLS1)
	defer listener1.Close()

	var wg1 sync.WaitGroup
	wg1.Add(1)
	go runTLSServer(listener1, &wg1, "TLS")

	clientTLS1 := buildClientTLSConfig(ca, nil, "localhost")
	msg, err := runTLSClient(listener1.Addr().String(), clientTLS1, "TLS")
	if err != nil {
		fmt.Printf("    [Client] 에러: %v\n", err)
	} else {
		fmt.Printf("    [Client] 수신: %q\n", msg)
	}
	wg1.Wait()
	fmt.Println()

	// ── 시나리오 2: mTLS ──
	fmt.Println("━━━ 시나리오 2: mTLS (Relay ↔ Server) ━━━")
	fmt.Println("  양방향 인증: 서버/클라이언트 인증서 모두 검증")
	fmt.Println()

	serverTLS2 := buildServerTLSConfig(ca, serverCert, true)
	listener2, _ := tls.Listen("tcp", "127.0.0.1:0", serverTLS2)
	defer listener2.Close()

	var wg2 sync.WaitGroup
	wg2.Add(1)
	go runTLSServer(listener2, &wg2, "mTLS")

	clientTLS2 := buildClientTLSConfig(ca, clientCert, "localhost")
	msg2, err := runTLSClient(listener2.Addr().String(), clientTLS2, "mTLS")
	if err != nil {
		fmt.Printf("    [Client] 에러: %v\n", err)
	} else {
		fmt.Printf("    [Client] 수신: %q\n", msg2)
	}
	wg2.Wait()
	fmt.Println()

	// ── 시나리오 3: 인증서 없는 클라이언트 → mTLS 거부 ──
	fmt.Println("━━━ 시나리오 3: mTLS 인증 실패 (클라이언트 인증서 없음) ━━━")
	fmt.Println("  mTLS 서버에 인증서 없이 접속 → 거부됨")
	fmt.Println()

	serverTLS3 := buildServerTLSConfig(ca, serverCert, true)
	listener3, _ := tls.Listen("tcp", "127.0.0.1:0", serverTLS3)
	defer listener3.Close()

	var wg3 sync.WaitGroup
	wg3.Add(1)
	go func() {
		defer wg3.Done()
		conn, err := listener3.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tlsConn := conn.(*tls.Conn)
		if err := tlsConn.Handshake(); err != nil {
			fmt.Printf("    [Server] Handshake 거부: %v\n", err)
		}
	}()

	clientTLS3 := buildClientTLSConfig(ca, nil, "localhost") // 클라이언트 인증서 없음
	_, err = runTLSClient(listener3.Addr().String(), clientTLS3, "no-cert")
	if err != nil {
		fmt.Printf("    [Client] 예상대로 연결 실패! (mTLS 인증서 필요)\n")
	}
	wg3.Wait()
	fmt.Println()

	// ── 시나리오 4: TLS 버전 검증 ──
	fmt.Println("━━━ 시나리오 4: TLS 버전 강제 ━━━")
	fmt.Println()
	fmt.Printf("  Hubble 최소 TLS 버전: %s\n", tlsVersionName(MinTLSVersion))
	fmt.Println("  TLS 1.2 이하 클라이언트 → 연결 거부")
	fmt.Println()

	serverTLS4 := buildServerTLSConfig(ca, serverCert, false)
	listener4, _ := tls.Listen("tcp", "127.0.0.1:0", serverTLS4)
	defer listener4.Close()

	var wg4 sync.WaitGroup
	wg4.Add(1)
	go func() {
		defer wg4.Done()
		conn, _ := listener4.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	oldClientTLS := buildClientTLSConfig(ca, nil, "localhost")
	oldClientTLS.MaxVersion = tls.VersionTLS12 // TLS 1.2로 제한
	_, err = runTLSClient(listener4.Addr().String(), oldClientTLS, "old-tls")
	if err != nil {
		fmt.Printf("    TLS 1.2 연결 시도 → 거부됨 (최소 TLS 1.3 필요)\n")
	}
	wg4.Wait()

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 단방향 TLS: CLI→Relay (서버 인증서만 검증)")
	fmt.Println("  - mTLS: Relay→Server (양방향 인증)")
	fmt.Println("  - 최소 TLS 1.3 강제: 이전 버전 연결 거부")
	fmt.Println("  - 실제 Hubble: certloader 패턴으로 인증서 동적 리로딩")
	fmt.Println("  - ServerName 검증: hubble-relay.cilium.io")
}
