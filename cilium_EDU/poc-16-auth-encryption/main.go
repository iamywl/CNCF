package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium 인증 및 암호화 시뮬레이션
// =============================================================================
// 실제 소스: pkg/auth/manager.go, pkg/auth/mutual_authhandler.go,
//           pkg/auth/authmap.go, pkg/auth/authmap_cache.go,
//           pkg/auth/authmap_gc.go, pkg/auth/certs/provider.go
//
// Cilium 인증 시스템 핵심:
// 1. AuthManager: 인증 요청을 관리하는 중앙 컨트롤러
//    - signalmap에서 인증 필요 신호를 수신
//    - authHandler별로 인증 수행 후 authmap에 결과 캐시
//
// 2. mutualAuthHandler: SPIFFE 기반 Mutual TLS 핸드셰이크 구현
//    - CertificateProvider에서 인증서 획득
//    - TLS 1.3 필수, VerifyPeerCertificate로 커스텀 검증
//
// 3. authMapCache: BPF authmap의 유저스페이스 캐시
//    - authKey(localID, remoteID, remoteNodeID, authType) -> authInfo(expiration)
//    - storedAt 필드로 backoff 판단
//
// 4. authMapGarbageCollector: 만료/삭제된 항목 정리
//    - 노드/Identity/엔드포인트/정책 변경에 따른 GC
//
// 실행: go run main.go
// =============================================================================

// ============================================================================
// Auth Types (실제: pkg/policy/types/auth.go)
// ============================================================================

// AuthType은 인증 유형이다. 실제 Cilium에서는 policyTypes.AuthType.
type AuthType uint8

const (
	AuthTypeDisabled   AuthType = 0
	AuthTypeSpire      AuthType = 1 // SPIFFE mutual TLS (실제: AuthTypeSpire)
	AuthTypeAlwaysFail AuthType = 2 // 테스트용 (실제: AuthTypeAlwaysFail)
)

func (a AuthType) String() string {
	switch a {
	case AuthTypeDisabled:
		return "disabled"
	case AuthTypeSpire:
		return "spire"
	case AuthTypeAlwaysFail:
		return "always-fail"
	default:
		return fmt.Sprintf("unknown(%d)", a)
	}
}

// ============================================================================
// AuthKey / AuthInfo (실제: pkg/auth/authmap.go)
// ============================================================================

// authKey는 BPF authmap의 키이다.
// 실제 구조: localIdentity + remoteIdentity + remoteNodeID + authType
type authKey struct {
	localIdentity  uint32
	remoteIdentity uint32
	remoteNodeID   uint16
	authType       AuthType
}

func (k authKey) String() string {
	return fmt.Sprintf("local=%d, remote=%d, nodeID=%d, auth=%s",
		k.localIdentity, k.remoteIdentity, k.remoteNodeID, k.authType)
}

// authInfo는 BPF authmap의 값이다.
type authInfo struct {
	expiration time.Time
}

// authInfoCache는 캐시에 저장 시점(storedAt)을 추가한 확장이다.
// 실제 구현(authmap_cache.go)에서 storedAt는 backoff 판단에 사용:
// 인증 신호를 받았을 때, storedAt + authSignalBackoffTime 이내면 재인증 건너뜀.
type authInfoCache struct {
	authInfo
	storedAt time.Time
}

// ============================================================================
// AuthMap 인터페이스 (실제: pkg/auth/authmap.go - authMap interface)
// ============================================================================

// authMap은 BPF "cilium_auth_map" 추상화이다.
type authMap interface {
	Update(key authKey, info authInfo) error
	Delete(key authKey) error
	DeleteIf(predicate func(key authKey, info authInfo) bool) error
	Get(key authKey) (authInfo, error)
	All() (map[authKey]authInfo, error)
	MaxEntries() uint32
}

// ============================================================================
// AuthMapCache (실제: pkg/auth/authmap_cache.go)
// ============================================================================

// authMapCacheImpl은 BPF authmap의 유저스페이스 캐시이다.
// 실제 구현에서는 authmap(BPF맵) 위에 cacheEntries(Go맵)를 두어
// 읽기 성능을 높이고, 업데이트 시 BPF맵과 캐시를 동시에 갱신한다.
type authMapCacheImpl struct {
	mu           sync.RWMutex
	entries      map[authKey]authInfoCache
	underlying   *memAuthMap // BPF 맵 시뮬레이션
	maxEntries   uint32
}

func newAuthMapCache(maxEntries uint32) *authMapCacheImpl {
	underlying := &memAuthMap{entries: make(map[authKey]authInfo)}
	return &authMapCacheImpl{
		entries:    make(map[authKey]authInfoCache),
		underlying: underlying,
		maxEntries: maxEntries,
	}
}

func (c *authMapCacheImpl) Update(key authKey, info authInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// BPF 맵에 먼저 쓰기 (실제: r.authmap.Update)
	if err := c.underlying.Update(key, info); err != nil {
		return err
	}
	// 캐시에 storedAt와 함께 저장
	c.entries[key] = authInfoCache{
		authInfo: info,
		storedAt: time.Now(),
	}
	return nil
}

func (c *authMapCacheImpl) Delete(key authKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.underlying.Delete(key)
	delete(c.entries, key)
	return nil
}

func (c *authMapCacheImpl) DeleteIf(predicate func(authKey, authInfo) bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.entries {
		if predicate(k, v.authInfo) {
			_ = c.underlying.Delete(k)
			delete(c.entries, k)
		}
	}
	return nil
}

func (c *authMapCacheImpl) Get(key authKey) (authInfo, error) {
	info, err := c.GetCacheInfo(key)
	return info.authInfo, err
}

// GetCacheInfo는 캐시 정보(storedAt 포함)를 반환한다. (실제: authMapCache.GetCacheInfo)
func (c *authMapCacheImpl) GetCacheInfo(key authKey) (authInfoCache, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.entries[key]
	if !ok {
		return authInfoCache{}, fmt.Errorf("키 없음: %s", key)
	}
	return info, nil
}

func (c *authMapCacheImpl) All() (map[authKey]authInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[authKey]authInfo, len(c.entries))
	for k, v := range c.entries {
		result[k] = v.authInfo
	}
	return result, nil
}

func (c *authMapCacheImpl) MaxEntries() uint32 { return c.maxEntries }

// Pressure는 맵 사용률(BPFMapPressure 게이지)을 반환한다.
func (c *authMapCacheImpl) Pressure() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return float64(len(c.entries)) / float64(c.maxEntries)
}

// memAuthMap은 BPF 맵의 인메모리 시뮬레이션이다.
type memAuthMap struct {
	entries map[authKey]authInfo
}

func (m *memAuthMap) Update(key authKey, info authInfo) error {
	m.entries[key] = info
	return nil
}

func (m *memAuthMap) Delete(key authKey) error {
	delete(m.entries, key)
	return nil
}

// ============================================================================
// CertificateProvider (실제: pkg/auth/certs/provider.go)
// ============================================================================

// CertificateProvider는 인증서를 제공하는 인터페이스이다.
// 실제 구현에서는 SPIRE 또는 내부 CA가 이 인터페이스를 구현하며,
// GetCertificateForIdentity, GetTrustBundle, ValidateIdentity,
// NumericIdentityToSNI, SNIToNumericIdentity, SubscribeToRotatedIdentities를 제공.
type CertificateProvider struct {
	ca      *CA
	certs   map[uint32]*tls.Certificate // identityID -> cert
	mu      sync.Mutex
	rotChan chan CertificateRotationEvent
}

// CertificateRotationEvent는 인증서 로테이션 이벤트이다.
// 실제 구현: pkg/auth/certs/provider.go - CertificateRotationEvent
type CertificateRotationEvent struct {
	Identity uint32
	Deleted  bool
}

func NewCertificateProvider(ca *CA) *CertificateProvider {
	return &CertificateProvider{
		ca:      ca,
		certs:   make(map[uint32]*tls.Certificate),
		rotChan: make(chan CertificateRotationEvent, 10),
	}
}

func (cp *CertificateProvider) GetCertificateForIdentity(id uint32) (*tls.Certificate, error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cert, ok := cp.certs[id]; ok {
		return cert, nil
	}
	cert, err := cp.ca.IssueIdentityCert(id)
	if err != nil {
		return nil, err
	}
	cp.certs[id] = &cert
	return &cert, nil
}

func (cp *CertificateProvider) GetTrustBundle() (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(cp.ca.certPEM)
	return pool, nil
}

// NumericIdentityToSNI는 Cilium Identity를 SNI로 변환한다.
// 실제: spiffe://cluster.local/identity/<id>
func (cp *CertificateProvider) NumericIdentityToSNI(id uint32) string {
	return fmt.Sprintf("identity-%d.cilium.local", id)
}

// RotateCertificate는 특정 Identity의 인증서를 갱신한다.
func (cp *CertificateProvider) RotateCertificate(id uint32) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cert, err := cp.ca.IssueIdentityCert(id)
	if err != nil {
		return err
	}
	cp.certs[id] = &cert
	cp.rotChan <- CertificateRotationEvent{Identity: id, Deleted: false}
	return nil
}

func (cp *CertificateProvider) SubscribeToRotatedIdentities() <-chan CertificateRotationEvent {
	return cp.rotChan
}

// ============================================================================
// CA (실제: SPIRE 또는 Cilium 내부 CA가 역할 수행)
// ============================================================================

type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"Cilium"}, CommonName: "Cilium CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	pem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &CA{cert: cert, key: key, certPEM: pem}, nil
}

// IssueIdentityCert는 Cilium Identity용 인증서를 발급한다.
// SPIFFE URI SAN 형식: spiffe://cluster.local/identity/<id>
func (ca *CA) IssueIdentityCert(identity uint32) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	sni := fmt.Sprintf("identity-%d.cilium.local", identity)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: sni},
		DNSNames:     []string{sni},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}

// ============================================================================
// AuthManager (실제: pkg/auth/manager.go)
// ============================================================================

// AuthManager는 인증 요청을 관리하는 중앙 컨트롤러이다.
// 실제 구현(manager.go)에서는:
// - signalmap에서 인증 필요 신호(signalAuthKey)를 수신
// - pending 맵으로 중복 인증 방지 (markPendingAuth/clearPendingAuth)
// - authHandlers 맵에서 AuthType별 핸들러 선택
// - 인증 성공 시 authmap에 결과(expiration) 캐시
// - authSignalBackoffTime으로 빈번한 재인증 방지
type AuthManager struct {
	handlers          map[AuthType]authHandler
	authmap           *authMapCacheImpl
	pending           map[authKey]struct{}
	pendingMu         sync.Mutex
	backoffTime       time.Duration
	nodeIPs           map[uint16]string // nodeID -> IP (실제: NodeIDHandler)
}

// authHandler는 특정 AuthType의 인증을 수행하는 인터페이스이다.
// 실제: pkg/auth/manager.go - authHandler interface
type authHandler interface {
	authenticate(req *authRequest) (*authResponse, error)
	authType() AuthType
}

type authRequest struct {
	localIdentity  uint32
	remoteIdentity uint32
	remoteNodeIP   string
}

type authResponse struct {
	expirationTime time.Time
}

func NewAuthManager(handlers []authHandler, amap *authMapCacheImpl, backoff time.Duration, nodeIPs map[uint16]string) *AuthManager {
	hm := make(map[AuthType]authHandler)
	for _, h := range handlers {
		hm[h.authType()] = h
	}
	return &AuthManager{
		handlers:    hm,
		authmap:     amap,
		pending:     make(map[authKey]struct{}),
		backoffTime: backoff,
		nodeIPs:     nodeIPs,
	}
}

// HandleAuthRequest는 데이터패스에서 인증 필요 신호를 받아 처리한다.
// 실제 구현(manager.go:handleAuthRequest):
// 1. Reserved identity이면 스킵
// 2. markPendingAuth()로 중복 방지
// 3. goroutine에서 비동기 인증 수행
// 4. backoff 확인: storedAt + backoffTime 이내면 재인증 건너뜀
// 5. authenticate() -> updateAuthMap()
func (am *AuthManager) HandleAuthRequest(key authKey, reAuth bool) error {
	// 중복 인증 방지 (실제: markPendingAuth)
	am.pendingMu.Lock()
	if _, exists := am.pending[key]; exists {
		am.pendingMu.Unlock()
		fmt.Printf("    [AuthManager] 이미 인증 진행 중: %s\n", key)
		return nil
	}
	am.pending[key] = struct{}{}
	am.pendingMu.Unlock()

	defer func() {
		am.pendingMu.Lock()
		delete(am.pending, key)
		am.pendingMu.Unlock()
	}()

	// Backoff 확인 (실제: storedAt + authSignalBackoffTime 이내면 건너뜀)
	if !reAuth {
		if info, err := am.authmap.GetCacheInfo(key); err == nil {
			if info.expiration.After(time.Now()) && time.Now().Before(info.storedAt.Add(am.backoffTime)) {
				fmt.Printf("    [AuthManager] Backoff 이내 - 재인증 건너뜀: %s\n", key)
				return nil
			}
		}
	}

	// 핸들러 선택 (실제: a.authHandlers[key.authType])
	handler, ok := am.handlers[key.authType]
	if !ok {
		return fmt.Errorf("미지원 인증 타입: %s", key.authType)
	}

	// 노드 IP 조회 (실제: nodeIDHandler.GetNodeIP)
	nodeIP, ok := am.nodeIPs[key.remoteNodeID]
	if !ok {
		return fmt.Errorf("노드 ID %d의 IP를 찾을 수 없음", key.remoteNodeID)
	}

	// 인증 수행
	req := &authRequest{
		localIdentity:  key.localIdentity,
		remoteIdentity: key.remoteIdentity,
		remoteNodeIP:   nodeIP,
	}
	resp, err := handler.authenticate(req)
	if err != nil {
		return fmt.Errorf("인증 실패 [%s]: %w", key.authType, err)
	}

	// authmap 갱신 (실제: a.updateAuthMap)
	if err := am.authmap.Update(key, authInfo{expiration: resp.expirationTime}); err != nil {
		return fmt.Errorf("authmap 갱신 실패: %w", err)
	}

	fmt.Printf("    [AuthManager] 인증 성공: %s (만료: %s)\n", key, resp.expirationTime.Format("15:04:05"))
	return nil
}

// HandleCertRotation은 인증서 로테이션 이벤트를 처리한다.
// 실제 구현(manager.go:handleCertificateRotationEvent):
// - 모든 authmap 항목을 순회
// - 해당 identity가 포함된 항목을 찾아 재인증 또는 삭제
func (am *AuthManager) HandleCertRotation(event CertificateRotationEvent) {
	all, _ := am.authmap.All()
	for k := range all {
		if k.localIdentity == event.Identity || k.remoteIdentity == event.Identity {
			if event.Deleted {
				_ = am.authmap.Delete(k)
				fmt.Printf("    [CertRotation] 삭제된 Identity %d에 대한 authmap 항목 제거: %s\n", event.Identity, k)
			} else {
				fmt.Printf("    [CertRotation] 로테이션된 Identity %d에 대해 재인증 트리거: %s\n", event.Identity, k)
				am.HandleAuthRequest(k, true)
			}
		}
	}
}

// ============================================================================
// MutualAuthHandler (실제: pkg/auth/mutual_authhandler.go)
// ============================================================================

// mutualAuthHandlerImpl은 SPIFFE 기반 Mutual TLS 인증 핸들러이다.
// 실제 구현(mutual_authhandler.go)에서는:
// - CertificateProvider에서 로컬 Identity의 인증서 획득
// - TCP 연결 후 TLS 1.3으로 업그레이드
// - VerifyPeerCertificate로 커스텀 인증서 검증
// - 가장 빠른 만료 시간(클라이언트/서버 인증서 중)을 반환
type mutualAuthHandlerImpl struct {
	cert     *CertificateProvider
	listener net.Listener
	port     int
}

func newMutualAuthHandler(cert *CertificateProvider, port int) (*mutualAuthHandlerImpl, error) {
	h := &mutualAuthHandlerImpl{cert: cert, port: port}
	if err := h.startListener(); err != nil {
		return nil, err
	}
	return h, nil
}

func (m *mutualAuthHandlerImpl) authType() AuthType { return AuthTypeSpire }

func (m *mutualAuthHandlerImpl) authenticate(req *authRequest) (*authResponse, error) {
	// 1. 로컬 Identity의 인증서 획득 (실제: m.cert.GetCertificateForIdentity)
	clientCert, err := m.cert.GetCertificateForIdentity(req.localIdentity)
	if err != nil {
		return nil, fmt.Errorf("로컬 인증서 획득 실패: %w", err)
	}

	// 2. Trust Bundle 획득
	caBundle, err := m.cert.GetTrustBundle()
	if err != nil {
		return nil, fmt.Errorf("trust bundle 획득 실패: %w", err)
	}

	expirationTime := clientCert.Leaf.NotAfter

	// 3. TCP 연결 후 TLS 핸드셰이크 (실제: net.DialTimeout -> tls.Client)
	conn, err := net.DialTimeout("tcp",
		net.JoinHostPort(req.remoteNodeIP, fmt.Sprintf("%d", m.port)),
		5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("TCP 연결 실패: %w", err)
	}
	defer conn.Close()

	// 4. TLS 설정 (실제: tls.VersionTLS13 필수, InsecureSkipVerify + VerifyPeerCertificate)
	sni := m.cert.NumericIdentityToSNI(req.remoteIdentity)
	//nolint:gosec
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: sni,
		GetClientCertificate: func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return clientCert, nil
		},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			// 실제: mutualAuthHandler.verifyPeerCertificate
			for _, rawCert := range rawCerts {
				cert, err := x509.ParseCertificate(rawCert)
				if err != nil {
					return err
				}
				if cert.NotAfter.Before(expirationTime) {
					expirationTime = cert.NotAfter
				}
			}
			return nil
		},
		RootCAs:   caBundle,
		ClientCAs: caBundle,
	})
	defer tlsConn.Close()

	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("TLS 핸드셰이크 실패: %w", err)
	}

	return &authResponse{expirationTime: expirationTime}, nil
}

// startListener는 Mutual Auth 리스너를 시작한다.
// 실제 구현(mutual_authhandler.go:listenForConnections):
// - TCP 리스너 시작
// - 각 연결에 대해 tls.Server로 핸드셰이크 수행
// - GetCertificate에서 SNI로 Identity를 매핑하여 인증서 반환
func (m *mutualAuthHandlerImpl) startListener() error {
	caBundle, _ := m.cert.GetTrustBundle()

	config := &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		GetCertificate: func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// 실제: GetCertificateForIncomingConnection
			// SNI에서 Identity를 추출하고 해당 인증서 반환
			return m.cert.GetCertificateForIdentity(1001) // 시뮬레이션: 고정 identity
		},
		MinVersion: tls.VersionTLS13,
		ClientCAs:  caBundle,
	}

	l, err := tls.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", m.port), config)
	if err != nil {
		return err
	}
	m.listener = l
	// OS가 할당한 실제 포트 번호로 갱신 (port=0 시 랜덤 포트 할당)
	m.port = l.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tlsConn := c.(*tls.Conn)
				tlsConn.Handshake()
			}(conn)
		}
	}()

	return nil
}

func (m *mutualAuthHandlerImpl) Close() {
	if m.listener != nil {
		m.listener.Close()
	}
}

// ============================================================================
// AuthMap GC (실제: pkg/auth/authmap_gc.go)
// ============================================================================

// cleanupExpiredEntries는 만료된 항목을 제거한다.
// 실제 구현(authmap_gc.go:cleanupExpiredEntries)
func cleanupExpiredEntries(amap *authMapCacheImpl) int {
	now := time.Now()
	removed := 0
	amap.DeleteIf(func(key authKey, info authInfo) bool {
		if info.expiration.Before(now) {
			removed++
			return true
		}
		return false
	})
	return removed
}

// cleanupDeletedNode는 삭제된 노드의 항목을 제거한다.
// 실제 구현(authmap_gc.go:cleanupDeletedNode)
func cleanupDeletedNode(amap *authMapCacheImpl, nodeID uint16) int {
	removed := 0
	amap.DeleteIf(func(key authKey, info authInfo) bool {
		if key.remoteNodeID == nodeID {
			removed++
			return true
		}
		return false
	})
	return removed
}

// cleanupDeletedIdentity는 삭제된 Identity의 항목을 제거한다.
// 실제 구현(authmap_gc.go:cleanupDeletedIdentity)
func cleanupDeletedIdentity(amap *authMapCacheImpl, id uint32) int {
	removed := 0
	amap.DeleteIf(func(key authKey, info authInfo) bool {
		if key.localIdentity == id || key.remoteIdentity == id {
			removed++
			return true
		}
		return false
	})
	return removed
}

// cleanupEntriesWithoutAuthPolicy는 정책이 없는 항목을 제거한다.
// 실제 구현(authmap_gc.go:cleanupEntriesWithoutAuthPolicy)
func cleanupEntriesWithoutAuthPolicy(amap *authMapCacheImpl, policyCheck func(local, remote uint32, at AuthType) bool) int {
	removed := 0
	amap.DeleteIf(func(key authKey, info authInfo) bool {
		if !policyCheck(key.localIdentity, key.remoteIdentity, key.authType) {
			removed++
			return true
		}
		return false
	})
	return removed
}

// ============================================================================
// 시뮬레이션 실행
// ============================================================================

func main() {
	fmt.Println("=== Cilium 인증 및 암호화 시뮬레이션 ===")
	fmt.Println("소스: pkg/auth/manager.go, mutual_authhandler.go, authmap_cache.go, authmap_gc.go")
	fmt.Println()

	// ─── 1. CA 및 CertificateProvider 설정 ─────────────────────
	fmt.Println("[1] CA 및 CertificateProvider 초기화")

	ca, err := NewCA()
	if err != nil {
		fmt.Printf("  CA 생성 실패: %v\n", err)
		return
	}
	fmt.Println("  CA 생성 완료: Cilium CA (ECDSA P-256)")

	certProvider := NewCertificateProvider(ca)
	fmt.Println("  CertificateProvider 초기화 (SPIFFE 인증서 관리)")
	fmt.Println()

	// ─── 2. Mutual TLS 핸드셰이크 ──────────────────────────────
	fmt.Println("[2] Mutual TLS 핸드셰이크 (mutualAuthHandler)")
	fmt.Println("  실제: pkg/auth/mutual_authhandler.go - authenticate()")

	mHandler, err := newMutualAuthHandler(certProvider, 0) // OS가 포트 선택
	if err != nil {
		// 포트 0이 아닌 다른 방식 시도
		fmt.Printf("  리스너 시작 실패: %v\n", err)
		fmt.Println("  mTLS 시뮬레이션을 건너뛰고 AuthMap 시뮬레이션으로 진행")
		fmt.Println()
	} else {
		defer mHandler.Close()
		port := mHandler.listener.Addr().(*net.TCPAddr).Port
		fmt.Printf("  Mutual Auth 리스너: 127.0.0.1:%d (TLS 1.3, ClientAuth=Required)\n", port)

		// 인증 수행
		resp, err := mHandler.authenticate(&authRequest{
			localIdentity:  2001,
			remoteIdentity: 1001,
			remoteNodeIP:   "127.0.0.1",
		})
		if err != nil {
			fmt.Printf("  핸드셰이크 실패: %v\n", err)
		} else {
			fmt.Printf("  핸드셰이크 성공! 만료: %s\n", resp.expirationTime.Format("2006-01-02 15:04:05"))
		}
		fmt.Println()
	}

	// ─── 3. AuthMapCache ────────────────────────────────────────
	fmt.Println("[3] AuthMapCache (BPF authmap 캐시)")
	fmt.Println("  실제: pkg/auth/authmap_cache.go")
	fmt.Println()

	authCache := newAuthMapCache(524288) // 512K 엔트리 (실제 기본값)

	// 인증 엔트리 추가
	entries := []struct {
		key authKey
		ttl time.Duration
	}{
		{authKey{1001, 2001, 1, AuthTypeSpire}, 10 * time.Second},
		{authKey{2001, 1001, 2, AuthTypeSpire}, 10 * time.Second},
		{authKey{1001, 3001, 3, AuthTypeSpire}, 500 * time.Millisecond}, // 곧 만료
		{authKey{3001, 4001, 1, AuthTypeSpire}, 10 * time.Second},       // 삭제 대상
	}

	for _, e := range entries {
		authCache.Update(e.key, authInfo{expiration: time.Now().Add(e.ttl)})
		fmt.Printf("  추가: %s (TTL=%v)\n", e.key, e.ttl)
	}
	fmt.Printf("  맵 사용률: %.4f%%\n", authCache.Pressure()*100)
	fmt.Println()

	// ─── 4. AuthManager 인증 흐름 ───────────────────────────────
	fmt.Println("[4] AuthManager 인증 흐름")
	fmt.Println("  실제: pkg/auth/manager.go - handleAuthRequest()")
	fmt.Println()

	nodeIPs := map[uint16]string{1: "127.0.0.1", 2: "127.0.0.2", 3: "127.0.0.3"}

	// AlwaysPass 핸들러 (시뮬레이션용)
	passHandler := &alwaysPassHandler{}

	mgr := NewAuthManager([]authHandler{passHandler}, authCache, 5*time.Second, nodeIPs)

	// 인증 요청 처리
	fmt.Println("  --- 새로운 인증 요청 ---")
	err = mgr.HandleAuthRequest(authKey{5001, 6001, 1, AuthTypeSpire}, false)
	if err != nil {
		fmt.Printf("    실패: %v\n", err)
	}

	// Backoff 테스트 (같은 키로 즉시 재요청)
	fmt.Println()
	fmt.Println("  --- Backoff 테스트 (같은 키 즉시 재요청) ---")
	mgr.HandleAuthRequest(authKey{5001, 6001, 1, AuthTypeSpire}, false)

	// 강제 재인증 (reAuth=true, 인증서 로테이션 시)
	fmt.Println()
	fmt.Println("  --- 강제 재인증 (reAuth=true) ---")
	mgr.HandleAuthRequest(authKey{5001, 6001, 1, AuthTypeSpire}, true)
	fmt.Println()

	// ─── 5. 인증서 로테이션 이벤트 처리 ─────────────────────────
	fmt.Println("[5] 인증서 로테이션 이벤트 처리")
	fmt.Println("  실제: pkg/auth/manager.go - handleCertificateRotationEvent()")
	fmt.Println()

	fmt.Println("  Identity 1001의 인증서 로테이션:")
	mgr.HandleCertRotation(CertificateRotationEvent{Identity: 1001, Deleted: false})
	fmt.Println()

	// ─── 6. AuthMap GC ──────────────────────────────────────────
	fmt.Println("[6] AuthMap Garbage Collection")
	fmt.Println("  실제: pkg/auth/authmap_gc.go")
	fmt.Println()

	// 만료 대기
	fmt.Println("  [600ms 대기 - Identity 3001 항목 만료 예상]")
	time.Sleep(600 * time.Millisecond)

	removed := cleanupExpiredEntries(authCache)
	fmt.Printf("  cleanupExpiredEntries: %d개 제거\n", removed)

	// 노드 삭제 GC
	removed = cleanupDeletedNode(authCache, 3)
	fmt.Printf("  cleanupDeletedNode(nodeID=3): %d개 제거\n", removed)

	// Identity 삭제 GC
	removed = cleanupDeletedIdentity(authCache, 3001)
	fmt.Printf("  cleanupDeletedIdentity(identity=3001): %d개 제거\n", removed)

	// 정책 없는 항목 GC
	removed = cleanupEntriesWithoutAuthPolicy(authCache, func(local, remote uint32, at AuthType) bool {
		// Identity 5001-6001 쌍만 정책 존재한다고 가정
		return (local == 5001 && remote == 6001) || (local == 1001 && remote == 2001) || (local == 2001 && remote == 1001)
	})
	fmt.Printf("  cleanupEntriesWithoutAuthPolicy: %d개 제거\n", removed)
	fmt.Println()

	// ─── 7. 최종 AuthMap 상태 ───────────────────────────────────
	fmt.Println("[7] 최종 AuthMap 상태")
	all, _ := authCache.All()
	fmt.Printf("  %-8s %-8s %-6s %-8s %-25s\n", "LocalID", "RemoteID", "NodeID", "Auth", "Expiration")
	fmt.Println("  " + strings.Repeat("-", 60))
	for k, v := range all {
		status := "VALID"
		if v.expiration.Before(time.Now()) {
			status = "EXPIRED"
		}
		_ = status
		fmt.Printf("  %-8d %-8d %-6d %-8s %s\n",
			k.localIdentity, k.remoteIdentity, k.remoteNodeID, k.authType,
			v.expiration.Format("15:04:05.000"))
	}
	fmt.Printf("  맵 사용률: %.6f%%\n", authCache.Pressure()*100)
	fmt.Println()

	// ─── 8. 데이터패스 인증 확인 시뮬레이션 ─────────────────────
	fmt.Println("[8] 데이터패스 인증 확인 (BPF auth_map 조회)")
	fmt.Println("  실제: BPF 프로그램에서 auth_map을 조회하여 패킷 PASS/DROP 결정")
	fmt.Println()

	checks := []authKey{
		{1001, 2001, 1, AuthTypeSpire},
		{5001, 6001, 1, AuthTypeSpire},
		{1001, 9999, 1, AuthTypeSpire},
		{3001, 4001, 1, AuthTypeSpire}, // GC로 삭제됨
	}

	for _, k := range checks {
		info, err := authCache.Get(k)
		if err != nil {
			fmt.Printf("  %d -> %d (auth=%s): DROP (인증 필요 - 항목 없음)\n",
				k.localIdentity, k.remoteIdentity, k.authType)
		} else if info.expiration.Before(time.Now()) {
			fmt.Printf("  %d -> %d (auth=%s): DROP (만료됨 - 재인증 필요)\n",
				k.localIdentity, k.remoteIdentity, k.authType)
		} else {
			fmt.Printf("  %d -> %d (auth=%s): PASS (인증됨, 만료=%s)\n",
				k.localIdentity, k.remoteIdentity, k.authType,
				info.expiration.Format("15:04:05"))
		}
	}

	// ─── 구조 요약 ─────────────────────────────────────────────
	fmt.Println()
	fmt.Println("=== Cilium 인증 아키텍처 요약 ===")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────┐")
	fmt.Println("  │           BPF Datapath (패킷 경로)                  │")
	fmt.Println("  │  패킷 도착 → cilium_auth_map 조회                  │")
	fmt.Println("  │    ├── VALID & 미만료 → PASS (fast path)           │")
	fmt.Println("  │    └── 없음/만료 → signalmap에 인증 요청 기록      │")
	fmt.Println("  └───────────────────┬─────────────────────────────────┘")
	fmt.Println("                      │ (signal)")
	fmt.Println("  ┌───────────────────▼─────────────────────────────────┐")
	fmt.Println("  │           AuthManager (userspace)                   │")
	fmt.Println("  │  1. markPendingAuth() - 중복 방지                  │")
	fmt.Println("  │  2. backoff 확인 (storedAt + backoffTime)          │")
	fmt.Println("  │  3. authHandler.authenticate() 호출                │")
	fmt.Println("  │     └── mutualAuthHandler: SPIFFE mTLS 핸드셰이크  │")
	fmt.Println("  │  4. authmap.Update() - 결과 캐시                   │")
	fmt.Println("  └───────────────────┬─────────────────────────────────┘")
	fmt.Println("                      │")
	fmt.Println("  ┌───────────────────▼─────────────────────────────────┐")
	fmt.Println("  │        authMapGarbageCollector                      │")
	fmt.Println("  │  - cleanupExpiredEntries (만료)                     │")
	fmt.Println("  │  - cleanupDeletedNode (노드 삭제)                  │")
	fmt.Println("  │  - cleanupDeletedIdentity (Identity 삭제)          │")
	fmt.Println("  │  - cleanupEntriesWithoutAuthPolicy (정책 제거)     │")
	fmt.Println("  └────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}

// alwaysPassHandler는 항상 인증 성공하는 핸들러이다.
// 실제: pkg/auth/always_pass_authhandler.go
type alwaysPassHandler struct{}

func (h *alwaysPassHandler) authenticate(req *authRequest) (*authResponse, error) {
	// SHA256으로 인증 토큰 시뮬레이션
	data := fmt.Sprintf("%d:%d:%s", req.localIdentity, req.remoteIdentity, req.remoteNodeIP)
	hash := sha256.Sum256([]byte(data))
	_ = hex.EncodeToString(hash[:8])

	return &authResponse{
		expirationTime: time.Now().Add(1 * time.Hour),
	}, nil
}

func (h *alwaysPassHandler) authType() AuthType { return AuthTypeSpire }
