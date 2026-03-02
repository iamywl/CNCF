// poc-16-auth-encryption: Cilium 인증/암호화 서브시스템 시뮬레이션
//
// 이 PoC는 Cilium의 WireGuard, IPsec, mTLS/SPIFFE 기반 인증/암호화 메커니즘을
// 순수 Go 표준 라이브러리로 시뮬레이션합니다.
//
// 실행: go run main.go
//
// 시뮬레이션 항목:
//   1. WireGuard 터널: 키 쌍 생성 → 피어 교환 → 암호화/복호화
//   2. IPsec SA 수명주기: 협상 → 설정 → 리키 → 만료
//   3. SPIFFE Identity: Trust Domain → Workload Identity → SVID 발급
//   4. mTLS 핸드셰이크: 상호 인증서 검증
//   5. 투명 암호화: 패킷 마킹 → 커널 암호화 → 전송 → 수신 → 복호화
//   6. 무중단 키 로테이션
package main

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. WireGuard 시뮬레이션
// =============================================================================

// WireGuardKeyPair는 Curve25519 키 쌍을 시뮬레이션합니다.
// 실제 Cilium: pkg/wireguard/agent/agent.go - loadOrGeneratePrivKey()
type WireGuardKeyPair struct {
	PrivateKey [32]byte
	PublicKey  [32]byte
}

// generateWireGuardKeyPair는 WireGuard 키 쌍을 생성합니다.
// 실제 Cilium에서는 wgtypes.GeneratePrivateKey()를 사용합니다.
func generateWireGuardKeyPair() (*WireGuardKeyPair, error) {
	kp := &WireGuardKeyPair{}
	if _, err := rand.Read(kp.PrivateKey[:]); err != nil {
		return nil, fmt.Errorf("키 생성 실패: %w", err)
	}
	// 실제 Curve25519에서는 privKey에서 pubKey를 파생하지만,
	// 여기서는 SHA-256을 사용해 시뮬레이션합니다.
	pubHash := sha256.Sum256(kp.PrivateKey[:])
	copy(kp.PublicKey[:], pubHash[:])
	return kp, nil
}

// WireGuardPeer는 WireGuard 피어 설정을 나타냅니다.
// 실제 Cilium: pkg/wireguard/agent/agent.go - peerConfig
type WireGuardPeer struct {
	Name       string
	PublicKey  [32]byte
	Endpoint   string       // IP:Port
	AllowedIPs []string     // CIDR 목록
	LastHandshake time.Time
}

// WireGuardAgent는 WireGuard 에이전트를 시뮬레이션합니다.
// 실제 Cilium: pkg/wireguard/agent/agent.go - Agent
type WireGuardAgent struct {
	mu         sync.RWMutex
	name       string
	keyPair    *WireGuardKeyPair
	listenPort int
	fwMark     uint32          // MagicMarkWireGuardEncrypted
	peers      map[string]*WireGuardPeer
	sharedKeys map[string][]byte // peer name -> shared symmetric key (DH result)
}

// newWireGuardAgent는 새 WireGuard 에이전트를 생성합니다.
func newWireGuardAgent(name string) (*WireGuardAgent, error) {
	kp, err := generateWireGuardKeyPair()
	if err != nil {
		return nil, err
	}
	return &WireGuardAgent{
		name:       name,
		keyPair:    kp,
		listenPort: 51871,
		fwMark:     0x0E00, // MagicMarkWireGuardEncrypted
		peers:      make(map[string]*WireGuardPeer),
		sharedKeys: make(map[string][]byte),
	}, nil
}

// addPeer는 피어를 추가합니다.
// 실제 Cilium: pkg/wireguard/agent/agent.go - updatePeer()
func (a *WireGuardAgent) addPeer(peer *WireGuardPeer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.peers[peer.Name] = peer
	// ECDH 시뮬레이션: 양쪽 공개키를 결합하여 대칭키 생성
	combined := append(a.keyPair.PrivateKey[:], peer.PublicKey[:]...)
	sharedKey := sha256.Sum256(combined)
	a.sharedKeys[peer.Name] = sharedKey[:]
}

// encrypt는 데이터를 암호화합니다 (AES-GCM 사용).
func (a *WireGuardAgent) encrypt(peerName string, plaintext []byte) ([]byte, error) {
	a.mu.RLock()
	key, ok := a.sharedKeys[peerName]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("피어 %s에 대한 공유 키 없음", peerName)
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt는 데이터를 복호화합니다.
func (a *WireGuardAgent) decrypt(peerName string, ciphertext []byte) ([]byte, error) {
	a.mu.RLock()
	key, ok := a.sharedKeys[peerName]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("피어 %s에 대한 공유 키 없음", peerName)
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("암호문이 너무 짧음")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// =============================================================================
// 2. IPsec SA 수명주기 시뮬레이션
// =============================================================================

// IPsecDir은 IPsec 방향을 나타냅니다.
type IPsecDir int

const (
	IPsecDirIn  IPsecDir = 1 << iota // 수신
	IPsecDirOut                       // 송신
	IPsecDirFwd                       // 포워딩
)

func (d IPsecDir) String() string {
	switch d {
	case IPsecDirIn:
		return "IN"
	case IPsecDirOut:
		return "OUT"
	case IPsecDirFwd:
		return "FWD"
	default:
		return "UNKNOWN"
	}
}

// IPsecAlgorithm은 암호화 알고리즘 정보입니다.
type IPsecAlgorithm struct {
	Name   string
	Key    []byte
	ICVLen int // Integrity Check Value 길이
}

// XfrmState는 XFRM Security Association을 시뮬레이션합니다.
// 실제 Cilium: pkg/datapath/linux/ipsec/ipsec_linux.go - ipSecNewState()
type XfrmState struct {
	SrcIP        net.IP
	DstIP        net.IP
	SPI          uint8
	ReqID        int
	Mode         string // "tunnel"
	Proto        string // "esp"
	Aead         *IPsecAlgorithm
	Mark         uint32
	OutputMark   uint32
	ReplayWindow int
	ESN          bool
	CreatedAt    time.Time
}

// XfrmPolicy는 XFRM Security Policy를 시뮬레이션합니다.
type XfrmPolicy struct {
	SrcSubnet string
	DstSubnet string
	Dir       IPsecDir
	Mark      uint32
	Action    string // "allow" or "block"
	Priority  int
	Template  *XfrmState
}

// IPsecSA는 Security Association의 수명주기를 관리합니다.
// 실제 Cilium: pkg/datapath/linux/ipsec/ipsec_linux.go - Agent
type IPsecSA struct {
	mu               sync.RWMutex
	states           map[string]*XfrmState  // key: "src-dst-spi"
	policies         map[string]*XfrmPolicy // key: "src-dst-dir"
	currentSPI       uint8
	globalKey        []byte
	keyRemovalTimes  map[uint8]time.Time
	rotationDuration time.Duration
}

// newIPsecSA는 새 IPsec SA 관리자를 생성합니다.
func newIPsecSA() *IPsecSA {
	globalKey := make([]byte, 32)
	rand.Read(globalKey)
	return &IPsecSA{
		states:           make(map[string]*XfrmState),
		policies:         make(map[string]*XfrmPolicy),
		currentSPI:       1,
		globalKey:        globalKey,
		keyRemovalTimes:  make(map[uint8]time.Time),
		rotationDuration: 5 * time.Second, // 시뮬레이션을 위해 짧게 설정
	}
}

// computeNodeKey는 노드 쌍별 키를 파생합니다.
// 실제 Cilium: pkg/datapath/linux/ipsec/ipsec_linux.go - computeNodeIPsecKey()
func (sa *IPsecSA) computeNodeKey(srcIP, dstIP net.IP, srcBootID, dstBootID string) []byte {
	input := make([]byte, 0, len(sa.globalKey)+len(srcIP)+len(dstIP)+72)
	input = append(input, sa.globalKey...)
	input = append(input, srcIP.To4()...)
	input = append(input, dstIP.To4()...)
	input = append(input, []byte(srcBootID[:36])...)
	input = append(input, []byte(dstBootID[:36])...)
	h := sha256.Sum256(input)
	return h[:len(sa.globalKey)]
}

// generateEncryptMark는 암호화 마크를 생성합니다.
// 실제 Cilium: pkg/datapath/linux/ipsec/ipsec_linux.go - generateEncryptMark()
func generateEncryptMark(spi uint8, nodeID uint16) uint32 {
	val := uint32(0x0E00) // RouteMarkEncrypt
	val |= uint32(spi) << 12
	val |= uint32(nodeID) << 16
	return val
}

// generateDecryptMark는 복호화 마크를 생성합니다.
func generateDecryptMark(nodeID uint16) uint32 {
	val := uint32(0x0D00) // RouteMarkDecrypt
	val |= uint32(nodeID) << 16
	return val
}

// negotiateSA는 SA를 협상하고 설정합니다.
// 실제 Cilium: pkg/datapath/linux/ipsec/ipsec_linux.go - UpsertIPsecEndpoint()
func (sa *IPsecSA) negotiateSA(srcIP, dstIP net.IP, srcBootID, dstBootID string, nodeID uint16) error {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	nodeKey := sa.computeNodeKey(srcIP, dstIP, srcBootID, dstBootID)

	// OUT state
	outState := &XfrmState{
		SrcIP: srcIP,
		DstIP: dstIP,
		SPI:   sa.currentSPI,
		ReqID: 1,
		Mode:  "tunnel",
		Proto: "esp",
		Aead: &IPsecAlgorithm{
			Name:   "rfc4106(gcm(aes))",
			Key:    nodeKey,
			ICVLen: 128,
		},
		Mark:         generateEncryptMark(sa.currentSPI, nodeID),
		OutputMark:   0x0E00,
		ReplayWindow: 1024,
		ESN:          true,
		CreatedAt:    time.Now(),
	}
	outKey := fmt.Sprintf("%s-%s-%d-out", srcIP, dstIP, sa.currentSPI)
	sa.states[outKey] = outState

	// IN state (역방향 키)
	reverseKey := sa.computeNodeKey(dstIP, srcIP, dstBootID, srcBootID)
	inState := &XfrmState{
		SrcIP: dstIP,
		DstIP: srcIP,
		SPI:   sa.currentSPI,
		ReqID: 1,
		Mode:  "tunnel",
		Proto: "esp",
		Aead: &IPsecAlgorithm{
			Name:   "rfc4106(gcm(aes))",
			Key:    reverseKey,
			ICVLen: 128,
		},
		Mark:         generateDecryptMark(nodeID),
		OutputMark:   0x0D00,
		ReplayWindow: 1024,
		ESN:          true,
		CreatedAt:    time.Now(),
	}
	inKey := fmt.Sprintf("%s-%s-%d-in", dstIP, srcIP, sa.currentSPI)
	sa.states[inKey] = inState

	// Policies
	outPolicy := &XfrmPolicy{
		SrcSubnet: srcIP.String() + "/32",
		DstSubnet: dstIP.String() + "/32",
		Dir:       IPsecDirOut,
		Mark:      generateEncryptMark(sa.currentSPI, nodeID),
		Action:    "allow",
		Priority:  50,
	}
	sa.policies[fmt.Sprintf("%s-%s-out", srcIP, dstIP)] = outPolicy

	inPolicy := &XfrmPolicy{
		SrcSubnet: dstIP.String() + "/32",
		DstSubnet: srcIP.String() + "/32",
		Dir:       IPsecDirIn,
		Action:    "allow",
		Priority:  50,
	}
	sa.policies[fmt.Sprintf("%s-%s-in", dstIP, srcIP)] = inPolicy

	return nil
}

// rekey는 키 로테이션을 수행합니다.
// 실제 Cilium: pkg/datapath/linux/ipsec/ipsec_linux.go - keyfileWatcher()
func (sa *IPsecSA) rekey() (uint8, uint8) {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	oldSPI := sa.currentSPI
	sa.currentSPI++
	if sa.currentSPI > 15 { // IPsecMaxKeyVersion 시뮬레이션
		sa.currentSPI = 1
	}
	sa.keyRemovalTimes[oldSPI] = time.Now()

	// 새 전역 키 생성
	newKey := make([]byte, 32)
	rand.Read(newKey)
	sa.globalKey = newKey

	return oldSPI, sa.currentSPI
}

// canReclaimSPI는 이전 SPI를 회수할 수 있는지 확인합니다.
// 실제 Cilium: pkg/datapath/linux/ipsec/ipsec_linux.go - ipSecSPICanBeReclaimed()
func (sa *IPsecSA) canReclaimSPI(spi uint8) bool {
	sa.mu.RLock()
	defer sa.mu.RUnlock()

	if spi == sa.currentSPI {
		return false
	}
	removalTime, ok := sa.keyRemovalTimes[spi]
	if !ok {
		return false
	}
	return time.Since(removalTime) >= sa.rotationDuration
}

// encryptPacket은 IPsec으로 패킷을 암호화합니다.
func (sa *IPsecSA) encryptPacket(stateKey string, plaintext []byte) ([]byte, error) {
	sa.mu.RLock()
	state, ok := sa.states[stateKey]
	sa.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("SA 상태 없음: %s", stateKey)
	}

	block, err := aes.NewCipher(state.Aead.Key[:32])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)

	// ESP 헤더 시뮬레이션: SPI(4) + SeqNum(4) + Nonce + Ciphertext
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:4], uint32(state.SPI))
	binary.BigEndian.PutUint32(header[4:8], 1) // Sequence Number
	encrypted := gcm.Seal(nil, nonce, plaintext, header)
	return append(append(header, nonce...), encrypted...), nil
}

// decryptPacket은 IPsec 패킷을 복호화합니다.
func (sa *IPsecSA) decryptPacket(stateKey string, packet []byte) ([]byte, error) {
	sa.mu.RLock()
	state, ok := sa.states[stateKey]
	sa.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("SA 상태 없음: %s", stateKey)
	}

	if len(packet) < 8 {
		return nil, fmt.Errorf("패킷 너무 짧음")
	}

	header := packet[:8]
	spi := binary.BigEndian.Uint32(header[0:4])
	if uint8(spi) != state.SPI {
		return nil, fmt.Errorf("SPI 불일치: got %d, want %d", spi, state.SPI)
	}

	block, err := aes.NewCipher(state.Aead.Key[:32])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	rest := packet[8:]
	if len(rest) < nonceSize {
		return nil, fmt.Errorf("nonce 부분 짧음")
	}
	nonce := rest[:nonceSize]
	ciphertext := rest[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, header)
}

// =============================================================================
// 3. SPIFFE Identity 시뮬레이션
// =============================================================================

// SPIFFEIdentity는 SPIFFE ID를 나타냅니다.
// 실제 Cilium: pkg/auth/spire/certificate_provider.go - sniToSPIFFEID()
type SPIFFEIdentity struct {
	TrustDomain string
	Path        string // "/identity/{numeric-id}"
}

func (s SPIFFEIdentity) String() string {
	return fmt.Sprintf("spiffe://%s%s", s.TrustDomain, s.Path)
}

// SPIFFEIdentityToSNI는 SPIFFE ID를 SNI로 변환합니다.
// 실제 Cilium: pkg/auth/spire/certificate_provider.go - NumericIdentityToSNI()
func SPIFFEIdentityToSNI(numericID int, trustDomain string) string {
	return fmt.Sprintf("%d.%s", numericID, trustDomain)
}

// SNIToSPIFFEIdentity는 SNI에서 SPIFFE ID로 변환합니다.
// 실제 Cilium: pkg/auth/spire/certificate_provider.go - SNIToNumericIdentity()
func SNIToSPIFFEIdentity(sni string, trustDomain string) (SPIFFEIdentity, error) {
	suffix := "." + trustDomain
	if !strings.HasSuffix(sni, suffix) {
		return SPIFFEIdentity{}, fmt.Errorf("SNI %s가 trust domain에 속하지 않음", sni)
	}
	idStr := strings.TrimSuffix(sni, suffix)
	return SPIFFEIdentity{
		TrustDomain: trustDomain,
		Path:        "/identity/" + idStr,
	}, nil
}

// SVID는 SPIFFE Verifiable Identity Document (X.509)를 나타냅니다.
type SVID struct {
	SpiffeID    SPIFFEIdentity
	Certificate *x509.Certificate
	PrivateKey  crypto.PrivateKey
	CACert      *x509.Certificate
	ExpiresAt   time.Time
}

// SPIREAgent는 SPIRE 에이전트를 시뮬레이션합니다.
// 실제 Cilium: pkg/auth/spire/delegate.go - SpireDelegateClient
type SPIREAgent struct {
	mu          sync.RWMutex
	trustDomain string
	caKey       *ecdsa.PrivateKey
	caCert      *x509.Certificate
	svidStore   map[string]*SVID
	rotatedCh   chan CertRotationEvent
}

// CertRotationEvent는 인증서 갱신 이벤트입니다.
// 실제 Cilium: pkg/auth/certs/provider.go - CertificateRotationEvent
type CertRotationEvent struct {
	Identity int
	Deleted  bool
}

// newSPIREAgent는 새 SPIRE 에이전트를 생성합니다.
func newSPIREAgent(trustDomain string) (*SPIREAgent, error) {
	// CA 키 쌍 생성
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("CA 키 생성 실패: %w", err)
	}

	// CA 인증서 생성
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   trustDomain + " CA",
			Organization: []string{"SPIFFE"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("CA 인증서 생성 실패: %w", err)
	}
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("CA 인증서 파싱 실패: %w", err)
	}

	return &SPIREAgent{
		trustDomain: trustDomain,
		caKey:       caKey,
		caCert:      caCert,
		svidStore:   make(map[string]*SVID),
		rotatedCh:   make(chan CertRotationEvent, 100),
	}, nil
}

// IssueSVID는 워크로드에 대한 X.509 SVID를 발급합니다.
// 실제 Cilium: pkg/auth/spire/certificate_provider.go - GetCertificateForIdentity()
func (s *SPIREAgent) IssueSVID(numericID int) (*SVID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	spiffeID := SPIFFEIdentity{
		TrustDomain: s.trustDomain,
		Path:        fmt.Sprintf("/identity/%d", numericID),
	}

	// 워크로드 키 쌍 생성
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("워크로드 키 생성 실패: %w", err)
	}

	// SPIFFE URI SAN을 포함한 인증서 생성
	spiffeURI, _ := url.Parse(spiffeID.String())
	template := &x509.Certificate{
		SerialNumber: big.NewInt(int64(numericID) + 100),
		Subject: pkix.Name{
			CommonName: fmt.Sprintf("identity-%d", numericID),
		},
		URIs:      []*url.URL{spiffeURI},
		NotBefore: time.Now().Add(-1 * time.Minute),
		NotAfter:  time.Now().Add(1 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, s.caCert, &key.PublicKey, s.caKey)
	if err != nil {
		return nil, fmt.Errorf("SVID 생성 실패: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("SVID 파싱 실패: %w", err)
	}

	svid := &SVID{
		SpiffeID:    spiffeID,
		Certificate: cert,
		PrivateKey:  key,
		CACert:      s.caCert,
		ExpiresAt:   cert.NotAfter,
	}

	s.svidStore[spiffeID.String()] = svid
	return svid, nil
}

// RotateSVID는 SVID를 갱신합니다.
func (s *SPIREAgent) RotateSVID(numericID int) (*SVID, error) {
	newSVID, err := s.IssueSVID(numericID)
	if err != nil {
		return nil, err
	}
	// 갱신 이벤트 발생
	select {
	case s.rotatedCh <- CertRotationEvent{Identity: numericID, Deleted: false}:
	default:
	}
	return newSVID, nil
}

// ValidateIdentity는 SPIFFE ID를 검증합니다.
// 실제 Cilium: pkg/auth/spire/certificate_provider.go - ValidateIdentity()
func (s *SPIREAgent) ValidateIdentity(numericID int, cert *x509.Certificate) (bool, error) {
	expectedSPIFFEID := fmt.Sprintf("spiffe://%s/identity/%d", s.trustDomain, numericID)

	// SPIFFE 표준: URI SAN이 정확히 하나여야 함
	if len(cert.URIs) != 1 {
		return false, fmt.Errorf("SPIFFE ID는 정확히 하나의 URI SAN이 필요합니다. got: %d", len(cert.URIs))
	}

	return cert.URIs[0].String() == expectedSPIFFEID, nil
}

// GetTrustBundle는 CA 인증서 풀을 반환합니다.
func (s *SPIREAgent) GetTrustBundle() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.caCert)
	return pool
}

// =============================================================================
// 4. mTLS 핸드셰이크 시뮬레이션
// =============================================================================

// AuthKey는 BPF auth map의 키를 나타냅니다.
// 실제 Cilium: pkg/auth/authmap.go - authKey
type AuthKey struct {
	LocalIdentity  int
	RemoteIdentity int
	RemoteNodeID   uint16
	AuthType       string
}

func (k AuthKey) String() string {
	return fmt.Sprintf("local=%d, remote=%d, nodeID=%d, type=%s",
		k.LocalIdentity, k.RemoteIdentity, k.RemoteNodeID, k.AuthType)
}

// AuthInfo는 BPF auth map의 값을 나타냅니다.
type AuthInfo struct {
	Expiration time.Time
	StoredAt   time.Time
}

// AuthMapCache는 BPF auth map 캐시를 시뮬레이션합니다.
// 실제 Cilium: pkg/auth/authmap_cache.go
type AuthMapCache struct {
	mu      sync.RWMutex
	entries map[AuthKey]AuthInfo
}

func newAuthMapCache() *AuthMapCache {
	return &AuthMapCache{
		entries: make(map[AuthKey]AuthInfo),
	}
}

func (c *AuthMapCache) update(key AuthKey, info AuthInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info.StoredAt = time.Now()
	c.entries[key] = info
}

func (c *AuthMapCache) get(key AuthKey) (AuthInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.entries[key]
	return info, ok
}

func (c *AuthMapCache) delete(key AuthKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// MutualAuthHandler는 mTLS 핸드셰이크를 시뮬레이션합니다.
// 실제 Cilium: pkg/auth/mutual_authhandler.go - mutualAuthHandler
type MutualAuthHandler struct {
	spireAgent *SPIREAgent
	authMap    *AuthMapCache
}

// authenticate는 mTLS 핸드셰이크를 수행합니다.
// 실제 Cilium: pkg/auth/mutual_authhandler.go - authenticate()
func (m *MutualAuthHandler) authenticate(localID, remoteID int, remoteNodeIP string) (*AuthInfo, error) {
	// 1. 로컬 SVID 획득
	localSVID, err := m.spireAgent.IssueSVID(localID)
	if err != nil {
		return nil, fmt.Errorf("로컬 SVID 획득 실패: %w", err)
	}

	// 2. 원격 SVID 획득 (실제로는 TLS 핸드셰이크에서 수신)
	remoteSVID, err := m.spireAgent.IssueSVID(remoteID)
	if err != nil {
		return nil, fmt.Errorf("원격 SVID 획득 실패: %w", err)
	}

	// 3. Trust Bundle로 인증서 검증
	trustPool := m.spireAgent.GetTrustBundle()
	opts := x509.VerifyOptions{
		Roots: trustPool,
	}

	// 서버 인증서 검증
	if _, err := remoteSVID.Certificate.Verify(opts); err != nil {
		return nil, fmt.Errorf("서버 인증서 검증 실패: %w", err)
	}

	// 클라이언트 인증서 검증
	if _, err := localSVID.Certificate.Verify(opts); err != nil {
		return nil, fmt.Errorf("클라이언트 인증서 검증 실패: %w", err)
	}

	// 4. SPIFFE ID 검증
	valid, err := m.spireAgent.ValidateIdentity(remoteID, remoteSVID.Certificate)
	if err != nil || !valid {
		return nil, fmt.Errorf("SPIFFE ID 검증 실패: identity=%d, valid=%v, err=%v", remoteID, valid, err)
	}

	// 5. 만료 시간 결정 (두 인증서 중 더 빠른 만료 시간)
	expirationTime := localSVID.ExpiresAt
	if remoteSVID.ExpiresAt.Before(expirationTime) {
		expirationTime = remoteSVID.ExpiresAt
	}

	return &AuthInfo{
		Expiration: expirationTime,
		StoredAt:   time.Now(),
	}, nil
}

// =============================================================================
// 5. 투명 암호화 시뮬레이션
// =============================================================================

// PacketMark는 BPF 패킷 마크를 시뮬레이션합니다.
// 실제 Cilium: bpf/lib/encrypt.h - set_decrypt_mark()
const (
	MarkMagicDecrypt            = 0x0D00
	MarkMagicEncrypt            = 0x0E00
	MarkMagicWireGuardEncrypted = 0x0E00
	MarkMagicDecryptedOverlay   = 0x1D00
)

// Packet는 네트워크 패킷을 시뮬레이션합니다.
type Packet struct {
	SrcIP    net.IP
	DstIP    net.IP
	Payload  []byte
	Mark     uint32  // BPF 마크
	IsEncrypted bool
}

// TransparentEncryption은 투명 암호화를 시뮬레이션합니다.
type TransparentEncryption struct {
	mode      string // "wireguard" or "ipsec"
	wgAgent   *WireGuardAgent
	ipsecSA   *IPsecSA
	strictMode bool
}

// markForEncryption은 BPF에서 패킷에 암호화 마크를 설정합니다.
// 실제: bpf/lib/encrypt.h의 마크 설정 로직
func (te *TransparentEncryption) markForEncryption(pkt *Packet, nodeID uint16) {
	pkt.Mark = MarkMagicEncrypt | (uint32(nodeID) << 16)
}

// markForDecryption은 복호화 마크를 설정합니다.
func (te *TransparentEncryption) markForDecryption(pkt *Packet, nodeID uint16) {
	pkt.Mark = MarkMagicDecrypt | (uint32(nodeID) << 16)
}

// strictModeCheck는 Strict Mode 검사를 시뮬레이션합니다.
// 실제: bpf/lib/encrypt.h - strict_allow()
func (te *TransparentEncryption) strictModeCheck(pkt *Packet) bool {
	if !te.strictMode {
		return true // strict mode 비활성화 시 통과
	}
	// 패킷에 암호화 마크가 없으면 차단
	if pkt.Mark&0x0F00 != MarkMagicEncrypt {
		return false
	}
	return true
}

// processEgress는 송신 패킷을 처리합니다.
func (te *TransparentEncryption) processEgress(pkt *Packet, peerName string, nodeID uint16) (*Packet, error) {
	// Step 1: BPF 마킹
	te.markForEncryption(pkt, nodeID)

	// Step 2: Strict mode 검사
	if !te.strictModeCheck(pkt) {
		return nil, fmt.Errorf("strict mode: 암호화 없이 전송 차단")
	}

	// Step 3: 커널 암호화
	var encryptedPayload []byte
	var err error

	switch te.mode {
	case "wireguard":
		encryptedPayload, err = te.wgAgent.encrypt(peerName, pkt.Payload)
	case "ipsec":
		stateKey := fmt.Sprintf("%s-%s-%d-out", pkt.SrcIP, pkt.DstIP, te.ipsecSA.currentSPI)
		encryptedPayload, err = te.ipsecSA.encryptPacket(stateKey, pkt.Payload)
	}
	if err != nil {
		return nil, fmt.Errorf("암호화 실패: %w", err)
	}

	return &Packet{
		SrcIP:       pkt.SrcIP,
		DstIP:       pkt.DstIP,
		Payload:     encryptedPayload,
		Mark:        pkt.Mark,
		IsEncrypted: true,
	}, nil
}

// processIngress는 수신 패킷을 처리합니다.
func (te *TransparentEncryption) processIngress(pkt *Packet, peerName string, nodeID uint16) (*Packet, error) {
	// Step 1: 커널 복호화
	var decryptedPayload []byte
	var err error

	switch te.mode {
	case "wireguard":
		decryptedPayload, err = te.wgAgent.decrypt(peerName, pkt.Payload)
	case "ipsec":
		stateKey := fmt.Sprintf("%s-%s-%d-in", pkt.SrcIP, pkt.DstIP, te.ipsecSA.currentSPI)
		decryptedPayload, err = te.ipsecSA.decryptPacket(stateKey, pkt.Payload)
	}
	if err != nil {
		return nil, fmt.Errorf("복호화 실패: %w", err)
	}

	// Step 2: BPF 복호화 마크 설정
	result := &Packet{
		SrcIP:       pkt.SrcIP,
		DstIP:       pkt.DstIP,
		Payload:     decryptedPayload,
		IsEncrypted: false,
	}
	te.markForDecryption(result, nodeID)

	return result, nil
}

// =============================================================================
// 6. 무중단 키 로테이션 시뮬레이션
// =============================================================================

// KeyRotationSimulator는 무중단 키 로테이션을 시뮬레이션합니다.
type KeyRotationSimulator struct {
	ipsecSA *IPsecSA
	srcIP   net.IP
	dstIP   net.IP
}

func (kr *KeyRotationSimulator) simulateTraffic(stateKeySuffix string, data []byte) (bool, error) {
	stateKey := fmt.Sprintf("%s-%s-%d-%s", kr.srcIP, kr.dstIP, kr.ipsecSA.currentSPI, stateKeySuffix)
	encrypted, err := kr.ipsecSA.encryptPacket(stateKey, data)
	if err != nil {
		return false, err
	}
	decrypted, err := kr.ipsecSA.decryptPacket(stateKey, encrypted)
	if err != nil {
		return false, err
	}
	return bytes.Equal(data, decrypted), nil
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" Cilium 인증/암호화 서브시스템 PoC")
	fmt.Println(" 참조: pkg/wireguard/, pkg/datapath/linux/ipsec/, pkg/auth/")
	fmt.Println("==========================================================")
	fmt.Println()

	simulateWireGuard()
	simulateIPsec()
	simulateSPIFFE()
	simulateMutualAuth()
	simulateTransparentEncryption()
	simulateKeyRotation()

	fmt.Println()
	fmt.Println("==========================================================")
	fmt.Println(" 모든 시뮬레이션 완료")
	fmt.Println("==========================================================")
}

func simulateWireGuard() {
	fmt.Println("----------------------------------------------------------")
	fmt.Println(" [1] WireGuard 터널 시뮬레이션")
	fmt.Println("     참조: pkg/wireguard/agent/agent.go")
	fmt.Println("----------------------------------------------------------")

	// 1. 두 노드의 WireGuard 에이전트 생성
	nodeA, err := newWireGuardAgent("node-a")
	if err != nil {
		fmt.Printf("  [오류] Node A 에이전트 생성 실패: %v\n", err)
		return
	}
	nodeB, err := newWireGuardAgent("node-b")
	if err != nil {
		fmt.Printf("  [오류] Node B 에이전트 생성 실패: %v\n", err)
		return
	}

	fmt.Printf("  Node A 공개키: %s...\n", hex.EncodeToString(nodeA.keyPair.PublicKey[:8]))
	fmt.Printf("  Node B 공개키: %s...\n", hex.EncodeToString(nodeB.keyPair.PublicKey[:8]))
	fmt.Printf("  수신 포트: %d (cilium_wg0)\n", nodeA.listenPort)
	fmt.Printf("  방화벽 마크: 0x%04X (MagicMarkWireGuardEncrypted)\n", nodeA.fwMark)

	// 2. 피어 교환 (CiliumNode CRD를 통해)
	nodeA.addPeer(&WireGuardPeer{
		Name:       "node-b",
		PublicKey:  nodeB.keyPair.PublicKey,
		Endpoint:   "10.0.2.1:51871",
		AllowedIPs: []string{"10.0.2.0/24"},
	})
	nodeB.addPeer(&WireGuardPeer{
		Name:       "node-a",
		PublicKey:  nodeA.keyPair.PublicKey,
		Endpoint:   "10.0.1.1:51871",
		AllowedIPs: []string{"10.0.1.0/24"},
	})
	fmt.Println("  피어 교환 완료 (CiliumNode CRD 시뮬레이션)")

	// 3. 암호화/복호화 테스트
	plaintext := []byte("Hello from Pod on Node A to Pod on Node B")
	encrypted, err := nodeA.encrypt("node-b", plaintext)
	if err != nil {
		fmt.Printf("  [오류] 암호화 실패: %v\n", err)
		return
	}
	fmt.Printf("  원본 데이터: %q\n", string(plaintext))
	fmt.Printf("  암호화 결과: %s... (%d bytes)\n", hex.EncodeToString(encrypted[:16]), len(encrypted))

	// Node B에서 같은 공유키로 복호화
	// 실제로는 양쪽의 DH로 동일한 키가 파생되지만,
	// 시뮬레이션에서는 같은 키를 직접 설정
	combined := append(nodeB.keyPair.PrivateKey[:], nodeA.keyPair.PublicKey[:]...)
	sharedKey := sha256.Sum256(combined)
	nodeB.mu.Lock()
	nodeB.sharedKeys["node-a"] = sharedKey[:]
	nodeB.mu.Unlock()

	// Node A의 암호화 키와 동일하게 만들기 위해 동일한 방법 사용
	combined2 := append(nodeA.keyPair.PrivateKey[:], nodeB.keyPair.PublicKey[:]...)
	sharedKey2 := sha256.Sum256(combined2)
	nodeA.mu.Lock()
	nodeA.sharedKeys["node-b"] = sharedKey2[:]
	nodeA.mu.Unlock()
	nodeB.mu.Lock()
	nodeB.sharedKeys["node-a"] = sharedKey2[:]
	nodeB.mu.Unlock()

	// 동일한 키로 다시 암호화 후 복호화 테스트
	encrypted2, err := nodeA.encrypt("node-b", plaintext)
	if err != nil {
		fmt.Printf("  [오류] 재암호화 실패: %v\n", err)
		return
	}
	decrypted, err := nodeB.decrypt("node-a", encrypted2)
	if err != nil {
		fmt.Printf("  [오류] 복호화 실패: %v\n", err)
		return
	}
	fmt.Printf("  복호화 결과: %q\n", string(decrypted))
	fmt.Printf("  검증: 원본 == 복호화 → %v\n", bytes.Equal(plaintext, decrypted))
	fmt.Println()
}

func simulateIPsec() {
	fmt.Println("----------------------------------------------------------")
	fmt.Println(" [2] IPsec SA 수명주기 시뮬레이션")
	fmt.Println("     참조: pkg/datapath/linux/ipsec/ipsec_linux.go")
	fmt.Println("----------------------------------------------------------")

	sa := newIPsecSA()
	srcIP := net.ParseIP("10.0.1.1")
	dstIP := net.ParseIP("10.0.2.1")
	srcBootID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	dstBootID := "f0e1d2c3-b4a5-6789-0fed-cba987654321"

	// 1. SA 협상
	fmt.Printf("  [협상] SPI=%d, src=%s, dst=%s\n", sa.currentSPI, srcIP, dstIP)
	if err := sa.negotiateSA(srcIP, dstIP, srcBootID, dstBootID, 1); err != nil {
		fmt.Printf("  [오류] SA 협상 실패: %v\n", err)
		return
	}

	// 노드별 키 파생 표시
	nodeKey := sa.computeNodeKey(srcIP, dstIP, srcBootID, dstBootID)
	fmt.Printf("  [키파생] SHA-256(globalKey + srcIP + dstIP + srcBootID + dstBootID)\n")
	fmt.Printf("           = %s...\n", hex.EncodeToString(nodeKey[:8]))

	// 마크 값 표시
	encMark := generateEncryptMark(sa.currentSPI, 1)
	decMark := generateDecryptMark(1)
	fmt.Printf("  [마크] Encrypt: 0x%08X (SPI=%d, NodeID=%d)\n", encMark, sa.currentSPI, 1)
	fmt.Printf("  [마크] Decrypt: 0x%08X (NodeID=%d)\n", decMark, 1)

	// 2. SA 상태 및 정책 표시
	fmt.Printf("  [상태] XFRM States: %d개\n", len(sa.states))
	for key, state := range sa.states {
		fmt.Printf("         %s: SPI=%d, algo=%s, mark=0x%08X\n",
			key, state.SPI, state.Aead.Name, state.Mark)
	}
	fmt.Printf("  [정책] XFRM Policies: %d개\n", len(sa.policies))
	for key, policy := range sa.policies {
		fmt.Printf("         %s: %s→%s, dir=%s, action=%s\n",
			key, policy.SrcSubnet, policy.DstSubnet, policy.Dir, policy.Action)
	}

	// 3. 패킷 암호화/복호화 테스트
	payload := []byte("IPsec encrypted payload from Node A")
	outKey := fmt.Sprintf("%s-%s-%d-out", srcIP, dstIP, sa.currentSPI)
	encrypted, err := sa.encryptPacket(outKey, payload)
	if err != nil {
		fmt.Printf("  [오류] 암호화 실패: %v\n", err)
		return
	}
	fmt.Printf("  [암호화] 원본: %q → ESP 패킷: %d bytes\n", string(payload), len(encrypted))

	// ESP 헤더 파싱
	espSPI := binary.BigEndian.Uint32(encrypted[0:4])
	espSeq := binary.BigEndian.Uint32(encrypted[4:8])
	fmt.Printf("  [ESP] SPI=0x%08X, SeqNum=%d\n", espSPI, espSeq)

	// 복호화
	decrypted, err := sa.decryptPacket(outKey, encrypted)
	if err != nil {
		fmt.Printf("  [오류] 복호화 실패: %v\n", err)
		return
	}
	fmt.Printf("  [복호화] 결과: %q\n", string(decrypted))
	fmt.Printf("  [검증] 원본 == 복호화 → %v\n", bytes.Equal(payload, decrypted))

	// 4. 키 로테이션
	oldSPI, newSPI := sa.rekey()
	fmt.Printf("  [리키] SPI %d → %d (새 전역 키 생성)\n", oldSPI, newSPI)
	fmt.Printf("  [리키] 이전 SPI=%d 회수 가능: %v (대기 시간: %v)\n",
		oldSPI, sa.canReclaimSPI(oldSPI), sa.rotationDuration)
	fmt.Println()
}

func simulateSPIFFE() {
	fmt.Println("----------------------------------------------------------")
	fmt.Println(" [3] SPIFFE Identity 시뮬레이션")
	fmt.Println("     참조: pkg/auth/spire/delegate.go")
	fmt.Println("           pkg/auth/spire/certificate_provider.go")
	fmt.Println("----------------------------------------------------------")

	trustDomain := "spiffe.cilium"
	agent, err := newSPIREAgent(trustDomain)
	if err != nil {
		fmt.Printf("  [오류] SPIRE Agent 생성 실패: %v\n", err)
		return
	}

	fmt.Printf("  Trust Domain: %s\n", trustDomain)
	fmt.Printf("  CA Subject: %s\n", agent.caCert.Subject.CommonName)
	fmt.Printf("  CA 유효기간: %s ~ %s\n",
		agent.caCert.NotBefore.Format("2006-01-02 15:04:05"),
		agent.caCert.NotAfter.Format("2006-01-02 15:04:05"))

	// SVID 발급
	identities := []int{1234, 5678, 9012}
	for _, id := range identities {
		svid, err := agent.IssueSVID(id)
		if err != nil {
			fmt.Printf("  [오류] Identity %d SVID 발급 실패: %v\n", id, err)
			continue
		}

		spiffeID := svid.SpiffeID.String()
		sni := SPIFFEIdentityToSNI(id, trustDomain)

		fmt.Printf("\n  [SVID] Identity %d:\n", id)
		fmt.Printf("    SPIFFE ID:  %s\n", spiffeID)
		fmt.Printf("    SNI:        %s\n", sni)
		fmt.Printf("    URI SAN:    %s\n", svid.Certificate.URIs[0].String())
		fmt.Printf("    만료:       %s\n", svid.ExpiresAt.Format("2006-01-02 15:04:05"))
		fmt.Printf("    직렬 번호:  %s\n", svid.Certificate.SerialNumber.String())

		// SNI → Identity 역변환 테스트
		parsedID, err := SNIToSPIFFEIdentity(sni, trustDomain)
		if err != nil {
			fmt.Printf("    SNI 역변환 실패: %v\n", err)
		} else {
			fmt.Printf("    SNI 역변환: %s → %s\n", sni, parsedID.String())
		}

		// Identity 검증
		valid, err := agent.ValidateIdentity(id, svid.Certificate)
		if err != nil {
			fmt.Printf("    검증 오류: %v\n", err)
		} else {
			fmt.Printf("    검증 결과: %v\n", valid)
		}
	}

	// Trust Bundle 검증
	trustPool := agent.GetTrustBundle()
	svid, _ := agent.IssueSVID(1234)
	opts := x509.VerifyOptions{Roots: trustPool}
	chains, err := svid.Certificate.Verify(opts)
	fmt.Printf("\n  [Trust Bundle] 인증서 검증: err=%v, chains=%d\n", err, len(chains))

	// SVID 갱신 시뮬레이션
	newSVID, err := agent.RotateSVID(1234)
	if err != nil {
		fmt.Printf("  [오류] SVID 갱신 실패: %v\n", err)
	} else {
		fmt.Printf("  [갱신] Identity 1234 SVID 갱신됨 (새 직렬 번호: %s)\n",
			newSVID.Certificate.SerialNumber.String())
	}
	fmt.Println()
}

func simulateMutualAuth() {
	fmt.Println("----------------------------------------------------------")
	fmt.Println(" [4] mTLS 상호 인증 핸드셰이크 시뮬레이션")
	fmt.Println("     참조: pkg/auth/mutual_authhandler.go")
	fmt.Println("           pkg/auth/manager.go")
	fmt.Println("----------------------------------------------------------")

	// SPIRE Agent 생성
	agent, err := newSPIREAgent("spiffe.cilium")
	if err != nil {
		fmt.Printf("  [오류] SPIRE Agent 생성 실패: %v\n", err)
		return
	}

	authMap := newAuthMapCache()
	handler := &MutualAuthHandler{
		spireAgent: agent,
		authMap:    authMap,
	}

	localID := 1234
	remoteID := 5678
	remoteNodeIP := "10.0.2.1"

	fmt.Printf("  Client Identity: %d (spiffe://spiffe.cilium/identity/%d)\n", localID, localID)
	fmt.Printf("  Server Identity: %d (spiffe://spiffe.cilium/identity/%d)\n", remoteID, remoteID)
	fmt.Printf("  Remote Node IP: %s\n", remoteNodeIP)

	// mTLS 핸드셰이크 시뮬레이션
	fmt.Println("\n  [핸드셰이크 시퀀스]")
	fmt.Println("  1. Client → Server: TCP SYN")
	fmt.Println("  2. Server → Client: TCP SYN-ACK")
	fmt.Println("  3. Client → Server: TLS ClientHello (TLS 1.3)")
	fmt.Printf("     SNI: %s\n", SPIFFEIdentityToSNI(remoteID, "spiffe.cilium"))
	fmt.Println("  4. Server → Client: TLS ServerHello + Certificate")
	fmt.Println("  5. Server → Client: CertificateRequest (상호 인증)")
	fmt.Println("  6. Client → Server: Certificate + CertificateVerify")
	fmt.Println("  7. 양방향 인증서 검증:")

	authInfo, err := handler.authenticate(localID, remoteID, remoteNodeIP)
	if err != nil {
		fmt.Printf("  [오류] 인증 실패: %v\n", err)
		return
	}

	fmt.Printf("     - 클라이언트 인증서 검증: OK\n")
	fmt.Printf("     - 서버 인증서 검증: OK\n")
	fmt.Printf("     - SPIFFE ID 검증: OK\n")
	fmt.Printf("  8. 핸드셰이크 완료!\n")

	// Auth Map 업데이트
	authKey := AuthKey{
		LocalIdentity:  localID,
		RemoteIdentity: remoteID,
		RemoteNodeID:   1,
		AuthType:       "spire",
	}
	authMap.update(authKey, *authInfo)
	fmt.Printf("\n  [Auth Map 업데이트]\n")
	fmt.Printf("    Key: %s\n", authKey)
	fmt.Printf("    만료: %s\n", authInfo.Expiration.Format("2006-01-02 15:04:05"))

	// Auth Map 조회 시뮬레이션 (BPF에서의 조회)
	cachedInfo, found := authMap.get(authKey)
	fmt.Printf("\n  [BPF Auth Map 조회]\n")
	fmt.Printf("    캐시 히트: %v\n", found)
	fmt.Printf("    유효: %v (현재 시간 < 만료 시간)\n", time.Now().Before(cachedInfo.Expiration))
	fmt.Printf("    → 패킷 통과 허용\n")

	// 인증서 갱신으로 인한 재인증
	fmt.Println("\n  [인증서 갱신 → 재인증]")
	fmt.Printf("    SVID 갱신 이벤트 수신 (Identity %d)\n", localID)
	authMap.delete(authKey)
	fmt.Printf("    Auth Map 항목 삭제 → 재인증 트리거\n")

	authInfo2, err := handler.authenticate(localID, remoteID, remoteNodeIP)
	if err != nil {
		fmt.Printf("    재인증 실패: %v\n", err)
	} else {
		authMap.update(authKey, *authInfo2)
		fmt.Printf("    재인증 성공! 새 만료: %s\n", authInfo2.Expiration.Format("2006-01-02 15:04:05"))
	}
	fmt.Println()
}

func simulateTransparentEncryption() {
	fmt.Println("----------------------------------------------------------")
	fmt.Println(" [5] 투명 암호화 시뮬레이션 (BPF 마킹 → 커널 암호화)")
	fmt.Println("     참조: bpf/lib/encrypt.h")
	fmt.Println("           pkg/datapath/linux/linux_defaults/mark.go")
	fmt.Println("----------------------------------------------------------")

	// WireGuard 모드 시뮬레이션
	fmt.Println("\n  === WireGuard 모드 ===")

	wgAgentA, _ := newWireGuardAgent("node-a")
	wgAgentB, _ := newWireGuardAgent("node-b")

	// 동일한 공유키 설정
	sharedKey := sha256.Sum256(append(wgAgentA.keyPair.PrivateKey[:], wgAgentB.keyPair.PublicKey[:]...))
	wgAgentA.sharedKeys["node-b"] = sharedKey[:]
	wgAgentB.sharedKeys["node-a"] = sharedKey[:]

	teWG := &TransparentEncryption{
		mode:       "wireguard",
		wgAgent:    wgAgentA,
		strictMode: true,
	}

	pkt := &Packet{
		SrcIP:   net.ParseIP("10.0.1.5"),
		DstIP:   net.ParseIP("10.0.2.8"),
		Payload: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nHello World"),
	}

	fmt.Printf("  원본 패킷: %s → %s, %d bytes\n", pkt.SrcIP, pkt.DstIP, len(pkt.Payload))
	fmt.Printf("  BPF 프로그램 실행:\n")

	// 송신 처리
	encPkt, err := teWG.processEgress(pkt, "node-b", 1)
	if err != nil {
		fmt.Printf("    [오류] 송신 처리 실패: %v\n", err)
		return
	}
	fmt.Printf("    1. 마킹:   mark=0x%08X (ENCRYPT | NodeID=1)\n", encPkt.Mark)
	fmt.Printf("    2. Strict: 통과 (암호화 마크 확인)\n")
	fmt.Printf("    3. 암호화: %d bytes → cilium_wg0 → UDP:51871\n", len(encPkt.Payload))
	fmt.Printf("    암호화됨: %v\n", encPkt.IsEncrypted)

	// 수신측 처리
	teWGRecv := &TransparentEncryption{
		mode:    "wireguard",
		wgAgent: wgAgentB,
	}
	decPkt, err := teWGRecv.processIngress(encPkt, "node-a", 1)
	if err != nil {
		fmt.Printf("    [오류] 수신 처리 실패: %v\n", err)
		return
	}
	fmt.Printf("  수신 처리:\n")
	fmt.Printf("    4. 복호화: cilium_wg0 → %d bytes\n", len(decPkt.Payload))
	fmt.Printf("    5. 마킹:   mark=0x%08X (DECRYPT | NodeID=1)\n", decPkt.Mark)
	fmt.Printf("    6. 결과:   %q\n", string(decPkt.Payload))
	fmt.Printf("    검증: 원본 == 복호화 → %v\n", bytes.Equal(pkt.Payload, decPkt.Payload))

	// IPsec 모드 시뮬레이션
	fmt.Println("\n  === IPsec 모드 ===")

	ipsecSA := newIPsecSA()
	srcIP := net.ParseIP("10.0.1.1")
	dstIP := net.ParseIP("10.0.2.1")
	ipsecSA.negotiateSA(srcIP, dstIP,
		"a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"f0e1d2c3-b4a5-6789-0fed-cba987654321", 2)

	teIPsec := &TransparentEncryption{
		mode:       "ipsec",
		ipsecSA:    ipsecSA,
		strictMode: true,
	}

	pktIPsec := &Packet{
		SrcIP:   srcIP,
		DstIP:   dstIP,
		Payload: []byte("Encrypted via IPsec ESP tunnel"),
	}

	fmt.Printf("  원본 패킷: %s → %s, %d bytes\n", pktIPsec.SrcIP, pktIPsec.DstIP, len(pktIPsec.Payload))

	encPktIPsec, err := teIPsec.processEgress(pktIPsec, "", 2)
	if err != nil {
		fmt.Printf("    [오류] IPsec 송신 실패: %v\n", err)
		return
	}
	fmt.Printf("  IPsec 송신:\n")
	fmt.Printf("    마크:   0x%08X\n", encPktIPsec.Mark)
	fmt.Printf("    ESP 패킷: %d bytes (SPI=%d)\n", len(encPktIPsec.Payload), ipsecSA.currentSPI)
	fmt.Printf("    암호화됨: %v\n", encPktIPsec.IsEncrypted)

	// Strict Mode 위반 테스트
	fmt.Println("\n  === Strict Mode 위반 테스트 ===")
	unmarkedPkt := &Packet{
		SrcIP:   srcIP,
		DstIP:   dstIP,
		Payload: []byte("No encryption mark"),
		Mark:    0, // 마크 없음
	}
	allowed := teIPsec.strictModeCheck(unmarkedPkt)
	fmt.Printf("  마크 없는 패킷: strict_allow() → %v (차단)\n", allowed)

	markedPkt := &Packet{Mark: MarkMagicEncrypt}
	allowed = teIPsec.strictModeCheck(markedPkt)
	fmt.Printf("  마크 있는 패킷: strict_allow() → %v (통과)\n", allowed)
	fmt.Println()
}

func simulateKeyRotation() {
	fmt.Println("----------------------------------------------------------")
	fmt.Println(" [6] 무중단 키 로테이션 시뮬레이션")
	fmt.Println("     참조: pkg/datapath/linux/ipsec/ipsec_linux.go")
	fmt.Println("           keyfileWatcher(), ipSecSPICanBeReclaimed()")
	fmt.Println("----------------------------------------------------------")

	sa := newIPsecSA()
	sa.rotationDuration = 200 * time.Millisecond // 시뮬레이션을 위해 짧게
	srcIP := net.ParseIP("10.0.1.1")
	dstIP := net.ParseIP("10.0.2.1")

	bootIDA := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	bootIDB := "f0e1d2c3-b4a5-6789-0fed-cba987654321"

	// Phase 1: 초기 SA 설정
	fmt.Println("\n  [Phase 1] 초기 SA 설정")
	sa.negotiateSA(srcIP, dstIP, bootIDA, bootIDB, 1)
	fmt.Printf("    활성 SPI: %d\n", sa.currentSPI)

	// 초기 트래픽 테스트
	kr := &KeyRotationSimulator{ipsecSA: sa, srcIP: srcIP, dstIP: dstIP}
	testData := []byte("Pre-rotation traffic")
	ok, err := kr.simulateTraffic("out", testData)
	fmt.Printf("    트래픽 테스트 (SPI=%d): success=%v, err=%v\n", sa.currentSPI, ok, err)

	// Phase 2: 키 로테이션 (새 키 파일 감지 시뮬레이션)
	fmt.Println("\n  [Phase 2] 키 로테이션 시작")
	fmt.Println("    1. 키 파일 변경 감지 (fswatcher)")
	oldSPI, newSPI := sa.rekey()
	fmt.Printf("    2. 새 키 로드: SPI %d → %d\n", oldSPI, newSPI)

	// 새 SA 설정
	sa.negotiateSA(srcIP, dstIP, bootIDA, bootIDB, 1)
	fmt.Printf("    3. 새 XFRM 상태 설치 (SPI=%d)\n", newSPI)
	fmt.Printf("    4. BPF 맵에 새 SPI 반영\n")

	// 새 키로 트래픽 테스트
	testData2 := []byte("Post-rotation traffic")
	ok, err = kr.simulateTraffic("out", testData2)
	fmt.Printf("    5. 새 트래픽 (SPI=%d): success=%v, err=%v\n", sa.currentSPI, ok, err)

	// Phase 3: 이전 키 회수 대기
	fmt.Println("\n  [Phase 3] 이전 키 회수")
	canReclaim := sa.canReclaimSPI(oldSPI)
	fmt.Printf("    t=0ms: SPI=%d 회수 가능? %v\n", oldSPI, canReclaim)

	fmt.Printf("    대기 중 (%v)...\n", sa.rotationDuration)
	time.Sleep(sa.rotationDuration + 10*time.Millisecond)

	canReclaim = sa.canReclaimSPI(oldSPI)
	fmt.Printf("    t=%v: SPI=%d 회수 가능? %v\n", sa.rotationDuration, oldSPI, canReclaim)
	if canReclaim {
		fmt.Printf("    이전 XFRM 상태 (SPI=%d) 안전하게 제거\n", oldSPI)
	}

	// Phase 4: 전체 과정 요약
	fmt.Println("\n  [요약] 무중단 키 로테이션 타임라인:")
	fmt.Println("    t=0:      새 키 파일 감지 → loadIPSecKeysFile()")
	fmt.Println("    t=0:      새 XFRM 상태 설치 → xfrmStateReplace()")
	fmt.Println("    t=0:      BPF 맵 업데이트 → setIPSecSPI()")
	fmt.Println("    t=0:      새 트래픽은 새 SPI로 암호화")
	fmt.Println("    t=0~5m:   이전 SPI로 암호화된 수신 트래픽도 복호화 가능")
	fmt.Println("    t=5m:     IPsecKeyRotationDuration 경과")
	fmt.Println("    t=5m:     이전 XFRM 상태 제거 (ipSecSPICanBeReclaimed=true)")
	fmt.Println("    → 연결 중단 없이 키 교체 완료!")

	// HMAC-based key derivation 시연
	fmt.Println("\n  [보너스] HMAC 기반 노드별 키 파생 시연:")
	key1 := sa.computeNodeKey(srcIP, dstIP, bootIDA, bootIDB)
	key2 := sa.computeNodeKey(dstIP, srcIP, bootIDB, bootIDA)
	fmt.Printf("    Node A→B 키: %s...\n", hex.EncodeToString(key1[:8]))
	fmt.Printf("    Node B→A 키: %s...\n", hex.EncodeToString(key2[:8]))
	fmt.Printf("    키 동일? %v (방향별 고유 키)\n", bytes.Equal(key1, key2))

	// MAC 검증 시연
	mac := hmac.New(sha256.New, key1)
	mac.Write([]byte("test message"))
	macResult := mac.Sum(nil)
	fmt.Printf("    HMAC-SHA256(key, msg): %s...\n", hex.EncodeToString(macResult[:8]))

	mac2 := hmac.New(sha256.New, key1)
	mac2.Write([]byte("test message"))
	fmt.Printf("    HMAC 검증: %v\n", hmac.Equal(macResult, mac2.Sum(nil)))
	fmt.Println()
}
