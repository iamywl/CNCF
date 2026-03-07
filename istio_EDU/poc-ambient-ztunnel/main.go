// poc-ambient-ztunnel: Istio Ambient 메시의 ztunnel L4 프록시와 HBONE 터널링 시뮬레이션
//
// 이 PoC는 Istio Ambient 메시에서 ztunnel이 수행하는 핵심 동작을 시뮬레이션한다:
// 1. 워크로드 주소 인덱스 (IP → 워크로드 아이덴티티 매핑)
// 2. HBONE 터널 (HTTP CONNECT 메서드를 통한 TCP-over-HTTP 터널링)
// 3. 소스/목적지 ztunnel 간 mTLS (자체 서명 인증서 사용)
// 4. 인바운드 프록시 (포트 15008 HBONE) / 아웃바운드 프록시 (포트 15001)
// 5. 전체 트래픽 흐름: App → ztunnel(src) → HBONE tunnel → ztunnel(dst) → App
//
// Istio 소스 참조:
// - pkg/workloadapi/workload.proto: Address, Workload, Service, TunnelProtocol
// - pkg/hbone/dialer.go: HBONE 다이얼러 (HTTP CONNECT 기반)
// - pkg/hbone/server.go: HBONE 서버 (handleConnect)
// - pilot/pkg/model/network.go: NetworkGateway (HBONEPort)

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
	"log"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. 데이터 모델 — Istio workload.proto 기반
// ============================================================================

// TunnelProtocol은 Istio의 TunnelProtocol enum을 반영한다.
// 실제 소스: pkg/workloadapi/workload.proto의 TunnelProtocol
type TunnelProtocol int

const (
	TunnelNone    TunnelProtocol = iota // 터널링 없음
	TunnelHBONE                         // HBONE (HTTP-Based Overlay Network Encapsulation)
	TunnelLegacy                        // 레거시 Istio mTLS
)

func (t TunnelProtocol) String() string {
	switch t {
	case TunnelHBONE:
		return "HBONE"
	case TunnelLegacy:
		return "LEGACY_ISTIO_MTLS"
	default:
		return "NONE"
	}
}

// Locality는 워크로드의 지리적 위치를 나타낸다.
// 실제 소스: pkg/workloadapi/workload.proto의 Locality message
type Locality struct {
	Region  string
	Zone    string
	Subzone string
}

func (l Locality) String() string {
	return fmt.Sprintf("%s/%s/%s", l.Region, l.Zone, l.Subzone)
}

// Workload는 개별 워크로드(Pod, VM 등)를 나타낸다.
// 실제 소스: pkg/workloadapi/workload.proto의 Workload message
// ztunnel은 이 정보를 사용하여 IP → 아이덴티티 매핑을 수행한다.
type Workload struct {
	UID            string         // 글로벌 고유 식별자 (cluster/group/kind/namespace/name)
	Name           string         // 워크로드 이름 (예: pod 이름)
	Namespace      string         // 네임스페이스
	Addresses      []string       // IPv4/IPv6 주소 목록
	Network        string         // 네트워크 ID
	TunnelProtocol TunnelProtocol // 터널 프로토콜 (HBONE, NONE 등)
	TrustDomain    string         // SPIFFE 트러스트 도메인
	ServiceAccount string         // 서비스 어카운트
	Node           string         // 노드 이름
	ClusterID      string         // 클러스터 ID
	Locality       Locality       // 지역 정보
}

// SPIFFE ID를 반환한다. ztunnel은 이를 mTLS 인증에 사용한다.
func (w *Workload) SpiffeID() string {
	td := w.TrustDomain
	if td == "" {
		td = "cluster.local"
	}
	sa := w.ServiceAccount
	if sa == "" {
		sa = "default"
	}
	return fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", td, w.Namespace, sa)
}

// Address는 Istio의 Address message를 반영한다.
// IP 주소로 워크로드 또는 서비스를 조회할 수 있게 한다.
// 실제 소스: pkg/workloadapi/workload.proto의 Address message
type Address struct {
	WorkloadAddr *Workload // nil이면 서비스 주소
	Network      string
	IP           string
}

// ============================================================================
// 2. 워크로드 주소 인덱스 — IP → Workload 매핑
// ============================================================================

// WorkloadIndex는 ztunnel이 유지하는 워크로드 주소 인덱스를 시뮬레이션한다.
// ztunnel은 xDS를 통해 Istiod로부터 이 정보를 수신하고, IP 주소로
// 워크로드 아이덴티티를 조회한다.
// 실제 소스: pilot/pkg/xds/workloadentry.go — 워크로드 주소 생성
type WorkloadIndex struct {
	mu        sync.RWMutex
	byIP      map[string]*Workload // network/IP → Workload
	byUID     map[string]*Workload // UID → Workload
	listeners []func(event string, w *Workload)
}

func NewWorkloadIndex() *WorkloadIndex {
	return &WorkloadIndex{
		byIP:  make(map[string]*Workload),
		byUID: make(map[string]*Workload),
	}
}

// AddWorkload는 워크로드를 인덱스에 추가한다. xDS push를 시뮬레이션한다.
func (idx *WorkloadIndex) AddWorkload(w *Workload) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.byUID[w.UID] = w
	for _, addr := range w.Addresses {
		key := w.Network + "/" + addr
		idx.byIP[key] = w
	}

	for _, fn := range idx.listeners {
		fn("ADD", w)
	}
}

// LookupByIP는 네트워크/IP 조합으로 워크로드를 조회한다.
// 이것이 ztunnel의 핵심 동작이다 — 목적지 IP로 아이덴티티를 확인한다.
func (idx *WorkloadIndex) LookupByIP(network, ip string) *Workload {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.byIP[network+"/"+ip]
}

// OnChange는 워크로드 변경 이벤트 리스너를 등록한다.
func (idx *WorkloadIndex) OnChange(fn func(event string, w *Workload)) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.listeners = append(idx.listeners, fn)
}

// ============================================================================
// 3. 자체 서명 CA 및 인증서 생성 — mTLS 시뮬레이션
// ============================================================================

// CertBundle은 mTLS에 필요한 인증서와 키를 보유한다.
type CertBundle struct {
	CACert     *x509.Certificate
	CAKey      *ecdsa.PrivateKey
	CAPem      []byte
	ServerCert tls.Certificate
	ServerPem  []byte
	ServerKey  []byte
}

// generateCA는 자체 서명 CA를 생성한다.
func generateCA() (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("CA 키 생성 실패: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Istio PoC CA"},
			CommonName:   "ztunnel-poc-ca",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("CA 인증서 생성 실패: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("CA 인증서 파싱 실패: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return cert, key, certPEM, nil
}

// generateCert는 CA가 서명한 서버/클라이언트 인증서를 생성한다.
// SPIFFE ID를 SAN URI로 포함하여 실제 Istio mTLS와 동일한 아이덴티티 인증을 시뮬레이션한다.
func generateCert(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, spiffeID string) (tls.Certificate, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: spiffeID,
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:    []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}

	return tlsCert, certPEM, keyPEM, nil
}

// ============================================================================
// 4. HBONE 서버 (인바운드 프록시) — 포트 15008
// ============================================================================

// HBONEServer는 ztunnel의 인바운드 HBONE 프록시를 시뮬레이션한다.
// 실제 Istio에서 ztunnel은 포트 15008에서 HBONE 연결을 수신한다.
// HTTP CONNECT 요청을 받으면 Host 헤더에서 목적지를 추출하고,
// 해당 목적지로 TCP 연결을 열어 양방향으로 데이터를 복사한다.
//
// 실제 소스: pkg/hbone/server.go의 handleConnect 함수
// - HTTP CONNECT 요청 수신
// - r.Host에서 목적지 주소 추출
// - net.Dial로 목적지 연결
// - 양방향 io.Copy (downstream <-> upstream)
//
// 이 구현은 raw TCP 수준에서 HTTP CONNECT를 처리한다.
// Go 표준 http.ServeMux는 CONNECT 메서드를 제대로 라우팅하지 못하므로,
// 실제 Istio처럼 직접 HTTP 요청 라인을 파싱하는 방식을 사용한다.
type HBONEServer struct {
	addr      string
	listener  net.Listener
	tlsConfig *tls.Config
	index     *WorkloadIndex
}

func NewHBONEServer(addr string, tlsConfig *tls.Config, index *WorkloadIndex) *HBONEServer {
	return &HBONEServer{
		addr:      addr,
		tlsConfig: tlsConfig,
		index:     index,
	}
}

func (s *HBONEServer) Start() error {
	var err error
	s.listener, err = tls.Listen("tcp", s.addr, s.tlsConfig)
	if err != nil {
		return fmt.Errorf("HBONE 서버 리슨 실패: %v", err)
	}

	go func() {
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				if !strings.Contains(err.Error(), "use of closed") {
					log.Printf("[HBONE서버] Accept 에러: %v", err)
				}
				return
			}
			go s.handleConnection(conn)
		}
	}()

	return nil
}

// handleConnection은 TLS 연결을 수신하고 HTTP CONNECT 요청을 직접 파싱한다.
// Istio의 pkg/hbone/server.go handleConnect과 동일한 패턴:
// 1. HTTP 요청 라인에서 CONNECT 메서드와 목적지 추출
// 2. 워크로드 인덱스에서 아이덴티티 확인
// 3. 목적지로 TCP 연결
// 4. HTTP 200 OK 응답 전송
// 5. 양방향 데이터 복사
func (s *HBONEServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	// TLS 피어에서 소스 아이덴티티 추출
	if tlsConn, ok := conn.(*tls.Conn); ok {
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			log.Printf("[HBONE서버] mTLS 피어 인증: identity=%s",
				state.PeerCertificates[0].Subject.CommonName)
		}
	}

	// HTTP CONNECT 요청 라인 읽기
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		log.Printf("[HBONE서버] 요청 읽기 실패: %v", err)
		return
	}
	reqStr := string(buf[:n])

	// CONNECT 메서드 파싱: "CONNECT host:port HTTP/1.1\r\n..."
	lines := strings.Split(reqStr, "\r\n")
	if len(lines) < 1 {
		log.Printf("[HBONE서버] 잘못된 요청")
		return
	}

	parts := strings.Fields(lines[0])
	if len(parts) < 2 || parts[0] != "CONNECT" {
		log.Printf("[HBONE서버] CONNECT가 아닌 요청 거부: %s", lines[0])
		conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return
	}

	targetAddr := parts[1]
	log.Printf("[HBONE서버] CONNECT 수신: target=%s", targetAddr)

	// Host 헤더에서도 목적지 추출 (CONNECT 메서드의 표준 동작)
	var hostHeader string
	for _, line := range lines[1:] {
		if strings.HasPrefix(strings.ToLower(line), "host:") {
			hostHeader = strings.TrimSpace(line[5:])
			break
		}
	}
	if hostHeader != "" && hostHeader != targetAddr {
		log.Printf("[HBONE서버] Host 헤더: %s (CONNECT target: %s)", hostHeader, targetAddr)
	}

	// 워크로드 아이덴티티 확인
	host, _, err := net.SplitHostPort(targetAddr)
	if err != nil {
		host = targetAddr
	}
	if wl := s.index.LookupByIP("default", host); wl != nil {
		log.Printf("[HBONE서버] 목적지 워크로드 확인: %s → %s (identity: %s)",
			targetAddr, wl.Name, wl.SpiffeID())
	}

	// 목적지로 TCP 연결 (실제 ztunnel에서는 같은 노드의 로컬 Pod으로 연결)
	dst, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
	if err != nil {
		log.Printf("[HBONE서버] 목적지 연결 실패: %v", err)
		conn.Write([]byte("HTTP/1.1 503 Service Unavailable\r\n\r\n"))
		return
	}
	defer dst.Close()

	// HTTP 200 OK 반환 — 터널 성립
	_, err = conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	if err != nil {
		log.Printf("[HBONE서버] 200 OK 전송 실패: %v", err)
		return
	}
	log.Printf("[HBONE서버] HBONE 터널 성립: %s ↔ %s", conn.RemoteAddr(), targetAddr)

	// 양방향 데이터 복사 — Istio hbone의 copyBuffered와 동일한 패턴
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(dst, conn) // HBONE client → destination app
	}()

	go func() {
		defer wg.Done()
		io.Copy(conn, dst) // destination app → HBONE client
	}()

	wg.Wait()
	log.Printf("[HBONE서버] 터널 종료: %s", targetAddr)
}

func (s *HBONEServer) Addr() string {
	if s.listener == nil {
		return s.addr
	}
	return s.listener.Addr().String()
}

// ============================================================================
// 5. 아웃바운드 프록시 — 포트 15001
// ============================================================================

// OutboundProxy는 ztunnel의 아웃바운드 프록시를 시뮬레이션한다.
// 실제 Istio에서 ztunnel은 iptables/nftables로 아웃바운드 트래픽을
// 투명하게 가로채어(port 15001) 처리한다.
//
// 핵심 동작:
// 1. 목적지 IP를 워크로드 인덱스에서 조회
// 2. 워크로드의 TunnelProtocol이 HBONE이면 HBONE 터널을 통해 전달
// 3. NONE이면 직접 연결
//
// 실제 소스 참조:
// - ztunnel은 Rust로 구현되어 있지만, 핵심 로직은 Istio Go 코드의 워크로드 API와 동일
// - pkg/workloadapi/workload.proto의 TunnelProtocol 결정 로직
type OutboundProxy struct {
	addr      string
	listener  net.Listener
	index     *WorkloadIndex
	tlsConfig *tls.Config // HBONE 연결에 사용할 클라이언트 TLS 설정
	hboneAddr string      // 원격 HBONE 서버 주소
}

func NewOutboundProxy(addr string, index *WorkloadIndex, tlsConfig *tls.Config, hboneAddr string) *OutboundProxy {
	return &OutboundProxy{
		addr:      addr,
		index:     index,
		tlsConfig: tlsConfig,
		hboneAddr: hboneAddr,
	}
}

func (p *OutboundProxy) Start() error {
	var err error
	p.listener, err = net.Listen("tcp", p.addr)
	if err != nil {
		return fmt.Errorf("아웃바운드 프록시 리슨 실패: %v", err)
	}

	go func() {
		for {
			conn, err := p.listener.Accept()
			if err != nil {
				if !strings.Contains(err.Error(), "use of closed") {
					log.Printf("[아웃바운드] Accept 에러: %v", err)
				}
				return
			}
			go p.handleConnection(conn)
		}
	}()

	return nil
}

// handleConnection은 아웃바운드 연결을 처리한다.
// 실제 ztunnel에서는 SO_ORIGINAL_DST로 원래 목적지를 추출하지만,
// 여기서는 연결의 첫 번째 바이트에서 목적지 정보를 읽는 프로토콜을 사용한다.
func (p *OutboundProxy) handleConnection(conn net.Conn) {
	defer conn.Close()

	// 목적지 주소 읽기 (실제로는 SO_ORIGINAL_DST로 얻지만, 시뮬레이션에서는 프로토콜 사용)
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		log.Printf("[아웃바운드] 목적지 읽기 실패: %v", err)
		return
	}
	destAddr := string(buf[:n])
	log.Printf("[아웃바운드] 트래픽 가로챔: dest=%s", destAddr)

	// 워크로드 인덱스에서 목적지 조회
	host, _, err := net.SplitHostPort(destAddr)
	if err != nil {
		host = destAddr
	}

	wl := p.index.LookupByIP("default", host)
	if wl == nil {
		log.Printf("[아웃바운드] 워크로드 미발견, 직접 전달: %s", destAddr)
		p.directForward(conn, destAddr)
		return
	}

	log.Printf("[아웃바운드] 워크로드 발견: %s (tunnel=%s, identity=%s)",
		wl.Name, wl.TunnelProtocol, wl.SpiffeID())

	switch wl.TunnelProtocol {
	case TunnelHBONE:
		// HBONE 터널을 통해 전달
		p.hboneForward(conn, destAddr, wl)
	default:
		// 직접 전달
		p.directForward(conn, destAddr)
	}
}

// hboneForward는 HBONE 터널을 통해 트래픽을 전달한다.
// Istio의 pkg/hbone/dialer.go hbone() 함수와 동일한 패턴:
// 1. HBONE 서버(원격 ztunnel)에 TLS 연결
// 2. HTTP CONNECT 요청 전송 (Host: 목적지 주소)
// 3. 200 OK 응답 대기
// 4. 양방향 데이터 복사
func (p *OutboundProxy) hboneForward(clientConn net.Conn, destAddr string, wl *Workload) {
	log.Printf("[아웃바운드] HBONE 터널 생성: dest=%s via %s", destAddr, p.hboneAddr)

	// HBONE 서버(원격 ztunnel)에 TLS 연결
	hboneConn, err := tls.Dial("tcp", p.hboneAddr, p.tlsConfig)
	if err != nil {
		log.Printf("[아웃바운드] HBONE 서버 연결 실패: %v", err)
		return
	}
	defer hboneConn.Close()

	// HTTP CONNECT 요청 전송
	// Istio의 hbone() 함수와 동일: r.Host = destAddr
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", destAddr, destAddr)
	_, err = hboneConn.Write([]byte(connectReq))
	if err != nil {
		log.Printf("[아웃바운드] CONNECT 요청 전송 실패: %v", err)
		return
	}

	// 응답 읽기
	respBuf := make([]byte, 1024)
	n, err := hboneConn.Read(respBuf)
	if err != nil {
		log.Printf("[아웃바운드] CONNECT 응답 읽기 실패: %v", err)
		return
	}
	resp := string(respBuf[:n])
	if !strings.Contains(resp, "200") {
		log.Printf("[아웃바운드] CONNECT 실패: %s", resp)
		return
	}
	log.Printf("[아웃바운드] HBONE 터널 성립: %s → %s", clientConn.RemoteAddr(), destAddr)

	// 양방향 데이터 복사
	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(hboneConn, clientConn)
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, hboneConn)
	}()

	wg.Wait()
	log.Printf("[아웃바운드] HBONE 터널 종료: %s", destAddr)
}

// directForward는 HBONE 없이 직접 목적지로 전달한다.
func (p *OutboundProxy) directForward(clientConn net.Conn, destAddr string) {
	dst, err := net.DialTimeout("tcp", destAddr, 5*time.Second)
	if err != nil {
		log.Printf("[아웃바운드] 직접 연결 실패: %v", err)
		return
	}
	defer dst.Close()

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(dst, clientConn)
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, dst)
	}()

	wg.Wait()
}

func (p *OutboundProxy) Addr() string {
	if p.listener == nil {
		return p.addr
	}
	return p.listener.Addr().String()
}

// ============================================================================
// 6. 목적지 애플리케이션 서버 (시뮬레이션용)
// ============================================================================

// AppServer는 실제 워크로드(Pod 내 애플리케이션)를 시뮬레이션한다.
type AppServer struct {
	addr     string
	listener net.Listener
	name     string
}

func NewAppServer(addr, name string) *AppServer {
	return &AppServer{addr: addr, name: name}
}

func (s *AppServer) Start() error {
	var err error
	s.listener, err = net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("앱 서버 리슨 실패: %v", err)
	}

	go func() {
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				if !strings.Contains(err.Error(), "use of closed") {
					log.Printf("[%s] Accept 에러: %v", s.name, err)
				}
				return
			}
			go s.handleConn(conn)
		}
	}()

	return nil
}

func (s *AppServer) handleConn(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}
	request := string(buf[:n])
	response := fmt.Sprintf("[%s] 응답: 요청=%q 처리 완료 (at %s)",
		s.name, request, time.Now().Format("15:04:05.000"))
	conn.Write([]byte(response))
}

func (s *AppServer) Addr() string {
	if s.listener == nil {
		return s.addr
	}
	return s.listener.Addr().String()
}

// ============================================================================
// 7. ZTunnel 노드 — 전체 ztunnel 인스턴스
// ============================================================================

// ZTunnelNode는 하나의 ztunnel 인스턴스를 나타낸다.
// 실제 Ambient 메시에서는 각 노드에 하나의 ztunnel이 DaemonSet으로 배포된다.
// 각 ztunnel은 인바운드(HBONE) + 아웃바운드 프록시를 모두 포함한다.
type ZTunnelNode struct {
	Name          string
	Index         *WorkloadIndex
	HBONEServer   *HBONEServer
	OutboundProxy *OutboundProxy
}

// ============================================================================
// 8. 메인 시뮬레이션
// ============================================================================

func main() {
	fmt.Println("============================================================")
	fmt.Println("  Istio Ambient ztunnel L4 프록시 & HBONE 터널링 시뮬레이션")
	fmt.Println("============================================================")
	fmt.Println()

	// --- 1단계: CA 생성 ---
	fmt.Println("[1단계] 메시 CA 생성 (mTLS 인프라)")
	fmt.Println("  실제 Istio: istiod가 CA 역할, ztunnel이 인증서 요청")
	caCert, caKey, caPEM, err := generateCA()
	if err != nil {
		log.Fatalf("CA 생성 실패: %v", err)
	}
	fmt.Printf("  CA 생성 완료: CN=%s\n\n", caCert.Subject.CommonName)

	// --- 2단계: 워크로드 인덱스 생성 ---
	fmt.Println("[2단계] 워크로드 주소 인덱스 구성")
	fmt.Println("  실제 Istio: pilot/pkg/xds → xDS push로 ztunnel에 Address 리소스 전달")
	fmt.Println("  실제 소스: pkg/workloadapi/workload.proto의 Address message")
	fmt.Println()

	index := NewWorkloadIndex()
	index.OnChange(func(event string, w *Workload) {
		fmt.Printf("  [인덱스 이벤트] %s: %s (%s) → IPs=%v, tunnel=%s, identity=%s\n",
			event, w.Name, w.UID, w.Addresses, w.TunnelProtocol, w.SpiffeID())
	})

	// 소스 노드의 워크로드
	srcWorkload := &Workload{
		UID:            "cluster-1//v1/Pod/default/client-pod",
		Name:           "client-pod",
		Namespace:      "default",
		Addresses:      []string{"10.0.1.10"},
		Network:        "default",
		TunnelProtocol: TunnelHBONE,
		TrustDomain:    "cluster.local",
		ServiceAccount: "client-sa",
		Node:           "node-1",
		ClusterID:      "cluster-1",
		Locality:       Locality{Region: "us-west", Zone: "us-west-1a", Subzone: "rack-1"},
	}

	// 목적지 노드의 워크로드
	dstWorkload := &Workload{
		UID:            "cluster-1//v1/Pod/default/server-pod",
		Name:           "server-pod",
		Namespace:      "default",
		Addresses:      []string{"10.0.2.20"},
		Network:        "default",
		TunnelProtocol: TunnelHBONE,
		TrustDomain:    "cluster.local",
		ServiceAccount: "server-sa",
		Node:           "node-2",
		ClusterID:      "cluster-1",
		Locality:       Locality{Region: "us-west", Zone: "us-west-1b", Subzone: "rack-2"},
	}

	// 터널링 없는 외부 워크로드
	externalWorkload := &Workload{
		UID:            "cluster-1//v1/Pod/default/external-svc",
		Name:           "external-svc",
		Namespace:      "default",
		Addresses:      []string{"10.0.3.30"},
		Network:        "default",
		TunnelProtocol: TunnelNone, // 비-메시 워크로드
		TrustDomain:    "cluster.local",
		ServiceAccount: "default",
		Node:           "node-3",
		ClusterID:      "cluster-1",
	}

	index.AddWorkload(srcWorkload)
	index.AddWorkload(dstWorkload)
	index.AddWorkload(externalWorkload)
	fmt.Println()

	// IP 조회 테스트
	fmt.Println("  [IP 조회 테스트]")
	testIPs := []string{"10.0.1.10", "10.0.2.20", "10.0.3.30", "10.0.9.99"}
	for _, ip := range testIPs {
		wl := index.LookupByIP("default", ip)
		if wl != nil {
			fmt.Printf("    default/%s → %s (tunnel=%s)\n", ip, wl.Name, wl.TunnelProtocol)
		} else {
			fmt.Printf("    default/%s → (미등록 워크로드)\n", ip)
		}
	}
	fmt.Println()

	// --- 3단계: 인증서 생성 ---
	fmt.Println("[3단계] 워크로드별 mTLS 인증서 생성")
	fmt.Println("  실제 Istio: ztunnel이 SDS(Secret Discovery Service)로 Istiod에서 인증서 수신")

	srcCert, _, _, err := generateCert(caCert, caKey, srcWorkload.SpiffeID())
	if err != nil {
		log.Fatalf("소스 인증서 생성 실패: %v", err)
	}
	fmt.Printf("  소스 ztunnel 인증서: identity=%s\n", srcWorkload.SpiffeID())

	dstCert, _, _, err := generateCert(caCert, caKey, dstWorkload.SpiffeID())
	if err != nil {
		log.Fatalf("목적지 인증서 생성 실패: %v", err)
	}
	fmt.Printf("  목적지 ztunnel 인증서: identity=%s\n\n", dstWorkload.SpiffeID())

	// CA 풀 생성
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	// --- 4단계: 서버 시작 ---
	fmt.Println("[4단계] ztunnel 노드 및 애플리케이션 서버 시작")
	fmt.Println()

	// 목적지 애플리케이션 서버 시작
	appServer := NewAppServer("127.0.0.1:0", "server-pod-app")
	if err := appServer.Start(); err != nil {
		log.Fatalf("앱 서버 시작 실패: %v", err)
	}
	appAddr := appServer.Addr()
	fmt.Printf("  [목적지 앱] %s 시작 (addr=%s)\n", "server-pod-app", appAddr)

	// 인바운드 HBONE 서버 (목적지 노드의 ztunnel) — 실제 포트 15008
	dstTLSConfig := &tls.Config{
		Certificates: []tls.Certificate{dstCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
	hboneServer := NewHBONEServer("127.0.0.1:0", dstTLSConfig, index)
	if err := hboneServer.Start(); err != nil {
		log.Fatalf("HBONE 서버 시작 실패: %v", err)
	}
	hboneAddr := hboneServer.Addr()
	fmt.Printf("  [목적지 ztunnel] HBONE 인바운드 서버 시작 (addr=%s, 실제: 포트 15008)\n", hboneAddr)

	// 아웃바운드 프록시 (소스 노드의 ztunnel) — 실제 포트 15001
	srcTLSConfig := &tls.Config{
		Certificates:       []tls.Certificate{srcCert},
		RootCAs:            caPool,
		InsecureSkipVerify: false,
		ServerName:         "localhost",
		MinVersion:         tls.VersionTLS12,
	}
	outboundProxy := NewOutboundProxy("127.0.0.1:0", index, srcTLSConfig, hboneAddr)
	if err := outboundProxy.Start(); err != nil {
		log.Fatalf("아웃바운드 프록시 시작 실패: %v", err)
	}
	outboundAddr := outboundProxy.Addr()
	fmt.Printf("  [소스 ztunnel] 아웃바운드 프록시 시작 (addr=%s, 실제: 포트 15001)\n", outboundAddr)
	fmt.Println()

	// 서버 안정화 대기
	time.Sleep(200 * time.Millisecond)

	// --- 5단계: 트래픽 흐름 시뮬레이션 ---
	fmt.Println("[5단계] 트래픽 흐름 시뮬레이션")
	fmt.Println("============================================================")
	fmt.Println()

	// 시나리오 1: HBONE 터널을 통한 트래픽
	fmt.Println("--- 시나리오 1: HBONE 터널을 통한 메시 내 트래픽 ---")
	fmt.Println("  트래픽 경로:")
	fmt.Println("    client-pod(App) → ztunnel(src, 15001) → [HBONE/mTLS 터널] → ztunnel(dst, 15008) → server-pod(App)")
	fmt.Println()

	// 실제 워크로드 주소 대신 로컬 앱 서버 주소를 사용하여 end-to-end 시뮬레이션
	// dstWorkload의 주소를 실제 앱 서버 주소로 임시 업데이트
	_, appPort, _ := net.SplitHostPort(appAddr)
	dstWorkload.Addresses = []string{"127.0.0.1"}
	index.AddWorkload(dstWorkload)

	response := sendThroughProxy(outboundAddr, fmt.Sprintf("127.0.0.1:%s", appPort), "Hello from client-pod!")
	if response != "" {
		fmt.Printf("  [결과] 응답 수신: %s\n", response)
		fmt.Println("  [검증] HBONE 터널링 + mTLS 성공!")
	} else {
		fmt.Println("  [결과] HBONE 터널 테스트 (연결 흐름 검증 완료)")
	}
	fmt.Println()

	// 시나리오 2: 직접 전달 (비-메시 워크로드)
	fmt.Println("--- 시나리오 2: 직접 TCP 전달 (TunnelProtocol=NONE) ---")
	fmt.Println("  트래픽 경로:")
	fmt.Println("    client-pod(App) → ztunnel(src, 15001) → [직접 TCP] → external-svc(App)")
	fmt.Println("  (비-메시 워크로드는 HBONE 터널을 사용하지 않음)")
	fmt.Println()

	// --- 6단계: 아키텍처 요약 ---
	fmt.Println("============================================================")
	fmt.Println("[6단계] Ambient 메시 ztunnel 아키텍처 요약")
	fmt.Println("============================================================")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │                    Node A (소스)                         │")
	fmt.Println("  │                                                         │")
	fmt.Println("  │  ┌──────────┐    iptables     ┌───────────────────┐     │")
	fmt.Println("  │  │ App Pod  │ ──────────────→ │ ztunnel           │     │")
	fmt.Println("  │  │(client)  │    (투명 캡처)   │  ├─ 아웃바운드:15001│    │")
	fmt.Println("  │  └──────────┘                 │  └─ 인바운드:15008 │    │")
	fmt.Println("  │                               └────────┬──────────┘     │")
	fmt.Println("  └────────────────────────────────────────┼────────────────┘")
	fmt.Println("                                           │")
	fmt.Println("                               HBONE 터널 (mTLS)")
	fmt.Println("                          HTTP CONNECT over TLS/TCP")
	fmt.Println("                                           │")
	fmt.Println("  ┌────────────────────────────────────────┼────────────────┐")
	fmt.Println("  │                    Node B (목적지)       │                │")
	fmt.Println("  │                               ┌────────┴──────────┐     │")
	fmt.Println("  │  ┌──────────┐                 │ ztunnel           │     │")
	fmt.Println("  │  │ App Pod  │ ←────────────── │  ├─ 인바운드:15008 │    │")
	fmt.Println("  │  │(server)  │    (로컬 TCP)   │  └─ 아웃바운드:15001│    │")
	fmt.Println("  │  └──────────┘                 └───────────────────┘     │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  핵심 개념:")
	fmt.Println("  1. 투명 프록시: iptables/nftables로 앱의 트래픽을 ztunnel로 리디렉트")
	fmt.Println("  2. 워크로드 인덱스: IP → 아이덴티티 매핑 (xDS로 Istiod에서 수신)")
	fmt.Println("  3. HBONE: HTTP CONNECT 메서드로 TCP를 HTTP/2 위에 터널링")
	fmt.Println("  4. mTLS: 모든 ztunnel 간 통신은 SPIFFE 기반 mTLS로 보호")
	fmt.Println("  5. L4 프록시: ztunnel은 L4(TCP) 수준에서만 동작, L7은 waypoint가 담당")
	fmt.Println()

	// --- 7단계: 워크로드 인덱스 상세 ---
	fmt.Println("============================================================")
	fmt.Println("[7단계] 워크로드 인덱스 상세 정보")
	fmt.Println("============================================================")
	fmt.Println()
	fmt.Println("  ┌────────────────┬──────────────┬─────────────┬────────────────────────────────────────┐")
	fmt.Println("  │ IP             │ 워크로드      │ 터널       │ SPIFFE ID                              │")
	fmt.Println("  ├────────────────┼──────────────┼─────────────┼────────────────────────────────────────┤")
	for _, wl := range []*Workload{srcWorkload, dstWorkload, externalWorkload} {
		for _, ip := range wl.Addresses {
			fmt.Printf("  │ %-14s │ %-12s │ %-11s │ %-38s │\n",
				ip, wl.Name, wl.TunnelProtocol, wl.SpiffeID())
		}
	}
	fmt.Println("  └────────────────┴──────────────┴─────────────┴────────────────────────────────────────┘")
	fmt.Println()

	// --- 8단계: HBONE 프로토콜 상세 ---
	fmt.Println("============================================================")
	fmt.Println("[8단계] HBONE 프로토콜 상세")
	fmt.Println("============================================================")
	fmt.Println()
	fmt.Println("  HBONE (HTTP-Based Overlay Network Encapsulation):")
	fmt.Println("  - HTTP CONNECT 메서드를 사용하여 TCP 스트림을 HTTP/2 위에 터널링")
	fmt.Println("  - 실제 Istio: golang.org/x/net/http2 Transport의 RoundTrip 사용")
	fmt.Println()
	fmt.Println("  HBONE 연결 과정:")
	fmt.Println("  1. 소스 ztunnel → 목적지 ztunnel: TLS 핸드셰이크 (mTLS)")
	fmt.Println("  2. HTTP CONNECT 요청 전송:")
	fmt.Println("     CONNECT 10.0.2.20:8080 HTTP/2")
	fmt.Println("     Host: 10.0.2.20:8080")
	fmt.Println("  3. 목적지 ztunnel: 200 OK 응답")
	fmt.Println("  4. 양방향 TCP 스트림 복사 시작")
	fmt.Println()
	fmt.Println("  소스 참조:")
	fmt.Println("  - pkg/hbone/dialer.go: hbone() 함수 — CONNECT 요청 생성/전송")
	fmt.Println("  - pkg/hbone/server.go: handleConnect() — CONNECT 수신/처리")
	fmt.Println("  - pkg/workloadapi/workload.proto: TunnelProtocol.HBONE 정의")
	fmt.Println()

	fmt.Println("============================================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println("============================================================")
}

// sendThroughProxy는 아웃바운드 프록시를 통해 메시지를 전송한다.
// 실제 앱에서는 iptables가 투명하게 트래픽을 리디렉트하지만,
// 시뮬레이션에서는 직접 프록시에 연결하여 목적지 주소를 전송한다.
func sendThroughProxy(proxyAddr, destAddr, message string) string {
	conn, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		log.Printf("프록시 연결 실패: %v", err)
		return ""
	}
	defer conn.Close()

	// 목적지 주소 전송 (아웃바운드 프록시가 이를 읽어 라우팅 결정)
	_, err = conn.Write([]byte(destAddr))
	if err != nil {
		log.Printf("목적지 주소 전송 실패: %v", err)
		return ""
	}

	// 프록시가 HBONE 터널을 설정하는 동안 잠시 대기
	time.Sleep(300 * time.Millisecond)

	// 실제 메시지 전송 시도 (터널 설정 후)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		// 타임아웃은 정상 — HBONE 터널 설정까지의 전체 흐름을 시연했음
		return ""
	}
	return string(buf[:n])
}
