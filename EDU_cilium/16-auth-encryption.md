# 16. Cilium 인증 및 암호화 서브시스템

---

## 개요

Cilium은 클러스터 내 노드 간 통신을 투명하게 암호화하고, 워크로드 간 상호 인증(Mutual Authentication)을 수행하는 종합적인 보안 서브시스템을 제공한다. 이 문서에서는 WireGuard 암호화, IPsec 암호화, mTLS 기반 상호 인증, SPIFFE/SPIRE 통합, 인증서 관리 등 핵심 구성 요소를 코드 수준에서 분석한다.

```
┌─────────────────────────────────────────────────────────────────────────┐
│                     Cilium 인증/암호화 서브시스템                         │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────────┐  │
│  │   WireGuard       │  │    IPsec          │  │  Mutual Auth (mTLS) │  │
│  │   투명 암호화      │  │    투명 암호화     │  │  SPIFFE/SPIRE       │  │
│  │                   │  │                   │  │                     │  │
│  │  Curve25519       │  │  AES-GCM/CBC      │  │  X.509 SVID         │  │
│  │  ChaCha20-Poly    │  │  XFRM SA/SP       │  │  Trust Bundle       │  │
│  │  Noise Protocol   │  │  SPI 기반 키관리    │  │  mTLS Handshake     │  │
│  └────────┬─────────┘  └────────┬─────────┘  └──────────┬──────────┘  │
│           │                     │                        │             │
│  ┌────────▼─────────────────────▼────────────────────────▼──────────┐  │
│  │                    BPF Datapath (패킷 마킹)                       │  │
│  │  MARK_MAGIC_ENCRYPT (0x0E00)  |  MARK_MAGIC_DECRYPT (0x0D00)    │  │
│  │  패킷에 암호화 마크 설정 → 커널 XFRM/WireGuard로 전달              │  │
│  └──────────────────────────────────────────────────────────────────┘  │
│                                                                         │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │                    인증서 관리 (certloader)                       │   │
│  │  파일 감시(fswatcher) → 자동 리로드 → 무중단 인증서 교체            │   │
│  └─────────────────────────────────────────────────────────────────┘   │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 1. WireGuard 투명 암호화

### 1.1 아키텍처 개요

WireGuard는 Cilium에서 노드 간 투명 암호화를 위한 기본 옵션이다. Linux 커널의 WireGuard 모듈을 사용하여 Curve25519 키 교환, ChaCha20-Poly1305 대칭 암호화, Noise Protocol Framework 기반 핸드셰이크를 수행한다.

```
Node A                                          Node B
┌──────────────────────┐                       ┌──────────────────────┐
│  Pod 10.0.1.5        │                       │  Pod 10.0.2.8        │
│       │              │                       │       ▲              │
│       ▼              │                       │       │              │
│  ┌─────────────┐     │                       │  ┌─────────────┐    │
│  │ BPF Program │     │                       │  │ BPF Program │    │
│  │ (마킹:0x0E00)│    │                       │  │ (복호화확인)  │    │
│  └──────┬──────┘     │                       │  └──────▲──────┘    │
│         ▼            │                       │         │           │
│  ┌─────────────┐     │   Encrypted Tunnel    │  ┌─────────────┐   │
│  │ cilium_wg0  │     │◄═════════════════════▶│  │ cilium_wg0  │   │
│  │ (WireGuard) │     │   UDP:51871           │  │ (WireGuard) │   │
│  │ PrivKey: A  │     │   ChaCha20-Poly1305   │  │ PrivKey: B  │   │
│  │ PubKey: A'  │     │                       │  │ PubKey: B'  │   │
│  └─────────────┘     │                       │  └─────────────┘   │
└──────────────────────┘                       └──────────────────────┘
```

### 1.2 핵심 타입과 상수

WireGuard 관련 상수는 `pkg/wireguard/types/types.go`에 정의되어 있다:

```go
// 파일: pkg/wireguard/types/types.go

const (
    // ListenPort는 WireGuard 터널 디바이스가 수신하는 포트
    ListenPort = 51871
    // IfaceName은 WireGuard 터널 디바이스 이름
    IfaceName = "cilium_wg0"
    // PrivKeyFilename은 WireGuard 개인키 파일명
    PrivKeyFilename = "cilium_wg0.key"
    // StaticEncryptKey는 IPCache에서 WireGuard 암호화를 표시하는 키
    StaticEncryptKey = uint8(0xFF)
)
```

`WireguardAgent` 인터페이스는 에이전트의 핵심 기능을 정의한다:

```go
// 파일: pkg/wireguard/types/types.go

type WireguardAgent interface {
    Enabled() bool
    Status(withPeers bool) (*models.WireguardStatus, error)
    IfaceIndex() (uint32, error)
    IfaceBufferMargins() (uint16, uint16, error)
}
```

### 1.3 Agent 구조체 (핵심 구현)

`pkg/wireguard/agent/agent.go`의 `Agent` 구조체가 WireGuard 동작의 핵심이다:

```go
// 파일: pkg/wireguard/agent/agent.go

type Agent struct {
    lock.RWMutex

    logger            *slog.Logger
    config            Config
    ipCache           *ipcache.IPCache
    sysctl            sysctl.Sysctl

    listenPort       int                          // UDP 수신 포트 (51871)
    privKeyPath      string                       // 개인키 파일 경로
    peerByNodeName   map[string]*peerConfig        // 노드명 → 피어 설정
    nodeNameByNodeIP map[string]string             // 노드IP → 노드명
    nodeNameByPubKey map[wgtypes.Key]string         // 공개키 → 노드명

    optOut   bool                                  // 노드 암호화 비활성화 여부
    privKey  wgtypes.Key                           // 로컬 개인키
    wgClient wireguardClient                       // WireGuard 커널 클라이언트
}
```

### 1.4 키 생성 및 초기화

에이전트 시작 시 개인키를 파일에서 로드하거나 새로 생성한다:

```go
// 파일: pkg/wireguard/agent/agent.go

func loadOrGeneratePrivKey(filePath string) (key wgtypes.Key, err error) {
    bytes, err := os.ReadFile(filePath)
    if os.IsNotExist(err) {
        // 키가 없으면 새로 생성
        key, err = wgtypes.GeneratePrivateKey()
        if err != nil {
            return wgtypes.Key{}, fmt.Errorf("failed to generate wg private key: %w", err)
        }
        // 0600 퍼미션으로 저장 (소유자만 읽기/쓰기)
        err = os.WriteFile(filePath, key[:], 0600)
        return key, nil
    }
    return wgtypes.NewKey(bytes)
}
```

초기화 과정에서 WireGuard 디바이스를 생성하고 설정한다:

```go
// 파일: pkg/wireguard/agent/agent.go

func (a *Agent) init() error {
    // 1. 개인키 로드 또는 생성
    a.privKey, err = loadOrGeneratePrivKey(a.privKeyPath)

    // 2. WireGuard 커널 디바이스 생성
    link = &netlink.Wireguard{
        LinkAttrs: netlink.LinkAttrs{Name: types.IfaceName},
    }
    netlink.LinkAdd(link)

    // 3. wgctrl 클라이언트 생성
    a.wgClient, err = wgctrl.New()

    // 4. 디바이스 설정 (개인키, 포트, 방화벽 마크)
    fwMark := linux_defaults.MagicMarkWireGuardEncrypted  // 0x0E00
    cfg := wgtypes.Config{
        PrivateKey:   &a.privKey,
        ListenPort:   &a.listenPort,    // 51871
        FirewallMark: &fwMark,
    }
    a.wgClient.ConfigureDevice(types.IfaceName, cfg)

    // 5. 링크 활성화
    netlink.LinkSetUp(link)
}
```

### 1.5 피어 관리 (노드 간 키 교환)

피어 관리는 CiliumNode CRD를 통해 이루어진다. 노드가 발견되면 `nodeUpsert()`를 거쳐 `updatePeer()`가 호출된다:

```go
// 파일: pkg/wireguard/agent/node_handler.go

func (a *Agent) nodeUpsert(node nodeTypes.Node) error {
    if node.IsLocal() || node.WireguardPubKey == "" {
        return nil  // 로컬 노드이거나 공개키가 없으면 스킵
    }
    newIP4 := node.GetNodeIP(false)
    newIP6 := node.GetNodeIP(true)
    return a.updatePeer(node.Fullname(), node.WireguardPubKey, newIP4, newIP6)
}
```

`updatePeer()`는 공개키, 엔드포인트, AllowedIPs를 커널에 설정한다:

```
키 교환 흐름:
  Node A 부팅 → 개인키 생성 → 공개키 = privKey.PublicKey()
       │
       ▼
  CiliumNode CRD에 공개키 게시 (K8s API)
       │
       ▼
  Node B가 CiliumNode Watch로 A의 공개키 수신
       │
       ▼
  Node B의 WireGuard Agent가 updatePeer() 호출
       │
       ▼
  커널의 WireGuard 피어 테이블에 등록:
    - PublicKey: Node A의 공개키
    - Endpoint: Node A의 IP:51871
    - AllowedIPs: Node A의 Pod CIDR + Node IP
```

### 1.6 AllowedIPs 관리

WireGuard의 AllowedIPs는 어떤 트래픽을 어떤 피어를 통해 보낼지 결정한다. Cilium은 IPCache 이벤트를 통해 AllowedIPs를 동적으로 관리한다:

```go
// 파일: pkg/wireguard/agent/agent.go

type peerConfig struct {
    pubKey             wgtypes.Key        // 피어의 공개키
    endpoint           *net.UDPAddr       // 피어의 엔드포인트 (IP:51871)
    nodeIPv4, nodeIPv6 net.IP             // 피어의 노드 IP
    allowedIPs         map[netip.Prefix]net.IPNet  // 동기화 완료된 IP
    needsInsert        map[netip.Prefix]net.IPNet  // 추가 예정 IP
    needsRemove        map[netip.Prefix]net.IPNet  // 제거 예정 IP
}
```

AllowedIPs 제거 시에는 커널 API의 한계로 인해 "더미 피어" 트릭을 사용한다:

```
AllowedIP 제거 과정 (더미 피어 트릭):
  1. 제거할 IP를 더미 피어(zero key)로 이동
  2. 더미 피어의 모든 AllowedIPs를 ReplaceAllowedIPs로 비움
  3. 더미 피어 삭제

  이 트릭이 필요한 이유:
  - WireGuard netlink API는 개별 AllowedIP 직접 제거를 지원하지 않음
  - ReplaceAllowedIPs로 전체 교체하면 비원자적 → 패킷 유실 발생
  - IP를 먼저 더미 피어로 "steal"하면 원본 피어의 다른 IP에 영향 없음
```

### 1.7 노드 암호화 옵트아웃

특정 노드(예: 컨트롤 플레인)를 암호화에서 제외할 수 있다:

```go
// 파일: pkg/wireguard/agent/agent.go

func (a *Agent) initLocalNodeFromWireGuard(localNode *node.LocalNode, sel k8sLabels.Selector) {
    localNode.EncryptionKey = types.StaticEncryptKey  // 0xFF
    localNode.WireguardPubKey = a.privKey.PublicKey().String()

    // 옵트아웃 레이블 확인 (기본: "node-role.kubernetes.io/control-plane")
    if a.config.EncryptNode && sel.Matches(k8sLabels.Set(localNode.Labels)) {
        localNode.Local.OptOutNodeEncryption = true
        localNode.EncryptionKey = 0  // 암호화 비활성화 신호
    }
}
```

---

## 2. IPsec (AES-GCM) 투명 암호화

### 2.1 아키텍처 개요

IPsec은 리눅스 커널의 XFRM(Transform) 프레임워크를 사용한다. Cilium은 XFRM 정책(SPD)과 상태(SAD)를 관리하여 노드 간 ESP(Encapsulating Security Payload) 터널을 구성한다.

```
IPsec 암호화 아키텍처:

  ┌─────────────────────────────────────────────────────┐
  │                     커널 (XFRM)                      │
  │                                                      │
  │  SPD (Security Policy Database)                      │
  │  ┌────────────────────────────────────────────────┐  │
  │  │ Policy IN:  src=10.0.2.0/24 → dst=10.0.1.0/24 │  │
  │  │ Policy OUT: src=10.0.1.0/24 → dst=10.0.2.0/24 │  │
  │  │             mark=0x0E00 (encrypt)               │  │
  │  │ Policy FWD: 포워딩 트래픽 허용                    │  │
  │  │ Default DROP: mark=0x0E00인데 정책 없으면 차단   │  │
  │  └────────────────────────────────────────────────┘  │
  │                                                      │
  │  SAD (Security Association Database)                  │
  │  ┌────────────────────────────────────────────────┐  │
  │  │ State IN:  SPI=#spi, dst=local_ip              │  │
  │  │           algo=aes-gcm, key=per-node-key       │  │
  │  │           mark=DECRYPT|nodeID                   │  │
  │  │                                                 │  │
  │  │ State OUT: SPI=#spi, dst=remote_ip             │  │
  │  │           algo=aes-gcm, key=per-node-key       │  │
  │  │           mark=ENCRYPT|SPI|nodeID               │  │
  │  └────────────────────────────────────────────────┘  │
  └─────────────────────────────────────────────────────┘
```

### 2.2 핵심 데이터 구조

```go
// 파일: pkg/datapath/linux/ipsec/ipsec_linux.go

type ipSecKey struct {
    Spi    uint8                     // Security Parameter Index (키 버전)
    KeyLen int                       // 키 길이
    ReqID  int                       // 요청 ID (기본: 1)
    Auth   *netlink.XfrmStateAlgo    // 인증 알고리즘 (hmac-sha256 등)
    Crypt  *netlink.XfrmStateAlgo    // 암호화 알고리즘 (aes-cbc 등)
    Aead   *netlink.XfrmStateAlgo    // AEAD 알고리즘 (rfc4106-gcm-aes 등)
}
```

IPsec Agent는 키 관리와 XFRM 상태를 관리한다:

```go
// 파일: pkg/datapath/linux/ipsec/ipsec_linux.go

type Agent struct {
    ipSecLock lock.RWMutex

    authKeySize int                            // 인증 키 크기
    spi         uint8                          // 현재 SPI
    ipSecKeysGlobal map[string]*ipSecKey        // 전역 키 맵 (IP → 키)
    ipSecCurrentKeySPI uint8                    // 현재 활성 SPI
    ipSecKeysRemovalTime map[uint8]time.Time    // SPI별 제거 시점
    xfrmStateCache *xfrmStateListCache          // XFRM 상태 캐시
}
```

### 2.3 키 파일 형식 및 로딩

IPsec 키는 파일에서 로드된다. 두 가지 형식을 지원한다:

```
형식 1 (AEAD): [spi] aead-algo aead-key icv-len
  예: 3 rfc4106(gcm(aes)) 0x... 128

형식 2 (Auth+Enc): [spi] auth-algo auth-key enc-algo enc-key
  예: 3 hmac(sha256) 0x... cbc(aes) 0x...
```

```go
// 파일: pkg/datapath/linux/ipsec/ipsec_linux.go

func (a *Agent) LoadIPSecKeys(r io.Reader) (int, uint8, error) {
    scanner := bufio.NewScanner(r)
    for scanner.Scan() {
        s := strings.Split(scanner.Text(), " ")
        spi, offsetBase, err = parseSPI(s[offsetSPI])

        if len(s) == offsetBase+offsetICV+1 {
            // AEAD 모드: rfc4106(gcm(aes))
            ipSecKey.Aead = &netlink.XfrmStateAlgo{
                Name: aeadName, Key: aeadKey, ICVLen: icvLen,
            }
        } else {
            // Auth+Enc 모드
            ipSecKey.Auth = &netlink.XfrmStateAlgo{Name: authAlgo, Key: authKey}
            ipSecKey.Crypt = &netlink.XfrmStateAlgo{Name: encAlgo, Key: encKey}
        }

        // SPI가 변경되면 이전 키의 제거 시간 기록
        if oldKey, ok := a.ipSecKeysGlobal[""]; ok {
            if oldKey.Spi == spi {
                return 0, 0, fmt.Errorf("SPI must be incremented for key rotation")
            }
            a.ipSecKeysRemovalTime[oldKey.Spi] = time.Now()
        }
        a.ipSecKeysGlobal[""] = ipSecKey
        a.ipSecCurrentKeySPI = spi
    }
}
```

### 2.4 노드별 키 파생 (Per-Node Key Derivation)

전역 PSK(Pre-Shared Key)에서 노드 쌍별 고유 키를 SHA-256 해시로 파생한다:

```go
// 파일: pkg/datapath/linux/ipsec/ipsec_linux.go

func computeNodeIPsecKey(globalKey, srcNodeIP, dstNodeIP, srcBootID, dstBootID []byte) []byte {
    input := make([]byte, 0, len(globalKey)+len(srcNodeIP)+len(dstNodeIP)+72)
    input = append(input, globalKey...)
    input = append(input, srcNodeIP...)
    input = append(input, dstNodeIP...)
    input = append(input, srcBootID[:36]...)
    input = append(input, dstBootID[:36]...)

    if len(globalKey) <= 32 {
        h := sha256.Sum256(input)
        return h[:len(globalKey)]
    }
    h := sha512.Sum512(input)
    return h[:len(globalKey)]
}
```

```
노드별 키 파생 다이어그램:

  Node A (IP: 10.0.1.1, BootID: abc...)    Node B (IP: 10.0.2.1, BootID: xyz...)
  ┌────────────────────────────────────┐
  │ XFRM OUT: key = hash(              │
  │   globalKey + 10.0.1.1 + 10.0.2.1  │ ──▶ Node B의 XFRM IN 키와 동일
  │   + abc... + xyz...)                │
  │                                     │
  │ XFRM IN:  key = hash(              │
  │   globalKey + 10.0.2.1 + 10.0.1.1  │ ◀── Node B의 XFRM OUT 키와 동일
  │   + xyz... + abc...)                │
  └────────────────────────────────────┘

  * BootID 포함 → 노드 재부팅 시 키 자동 갱신
```

### 2.5 XFRM 상태 및 정책 설치

```go
// 파일: pkg/datapath/linux/ipsec/ipsec_linux.go

func (a *Agent) ipSecReplaceStateOut(params *types.IPSecParameters) (uint8, error) {
    key, err := a.getNodeIPsecKey(srcIP, dstIP, localBootID, remoteBootID)
    state := ipSecNewState(key)
    state.Src = srcIP
    state.Dst = dstIP
    state.Mark = generateEncryptMark(key.Spi, remoteNodeID)  // SPI+NodeID 인코딩
    state.OutputMark = &netlink.XfrmMark{
        Value: linux_defaults.RouteMarkEncrypt,  // 0x0E00
    }
    return key.Spi, a.xfrmStateReplace(state, params.RemoteRebooted)
}
```

마크 값 구조:

```
Encrypt Mark: 0x{NodeID}{SPI}0E00
  ├── bits 31-16: Remote Node ID
  ├── bits 15-12: SPI (Security Parameter Index)
  └── bits 11-0:  0x0E00 (ENCRYPT 시그널)

Decrypt Mark: 0x{NodeID}0D00
  ├── bits 31-16: Remote Node ID (발신 노드)
  └── bits 11-0:  0x0D00 (DECRYPT 시그널)
```

### 2.6 키 로테이션

키 로테이션은 파일 감시(`fswatcher`)를 통해 수행된다:

```go
// 파일: pkg/datapath/linux/ipsec/ipsec_linux.go

func (a *Agent) keyfileWatcher(ctx context.Context, watcher *fswatcher.Watcher, ...) error {
    for {
        select {
        case event := <-watcher.Events:
            // 1. 새 키 파일 로드 (SPI 증가 필수)
            _, spi, err := a.loadIPSecKeysFile(keyfilePath)

            // 2. 로컬 노드의 EncryptionKey 업데이트 → K8s/kvstore에 전파
            a.localNode.Update(func(ln *node.LocalNode) {
                ln.EncryptionKey = spi
            })

            // 3. 모든 노드의 XFRM 상태/정책 갱신
            nodeHandler.AllNodeValidateImplementation()

            // 4. BPF 맵에 새 SPI 반영
            a.setIPSecSPI(spi)
        }
    }
}
```

```
키 로테이션 타임라인:

  t=0    키 파일 업데이트 (SPI: 3 → 4)
  t=0    loadIPSecKeysFile(): 새 키 로드, 이전 키(SPI=3) 제거 시간 기록
  t=0    모든 노드에 새 XFRM 상태(SPI=4) 설치
  t=0    BPF 맵에 SPI=4 기록 → 새 트래픽은 SPI=4로 암호화
  ...
  t=5m   ipSecSPICanBeReclaimed(SPI=3): IPsecKeyRotationDuration(5분) 경과 확인
  t=5m   이전 XFRM 상태(SPI=3) 제거 → 이전 키 완전 폐기
```

Catch-all 드롭 정책으로 암호화 전환 중 평문 유출을 방지한다:

```go
// 파일: pkg/datapath/linux/ipsec/ipsec_linux.go

func IPsecDefaultDropPolicy(ipv6 bool) error {
    // mark=0x0E00인 트래픽에 매칭하는 BLOCK 정책
    // 암호화 마크가 붙었지만 해당 XFRM 정책이 없는 트래픽을 차단
    // → XFRM 정책 교체 중 평문 유출 방지
    return netlink.XfrmPolicyUpdate(defaultDropPolicy)
}
```

---

## 3. WireGuard vs IPsec 비교

```
┌──────────────────────┬──────────────────────────┬──────────────────────────┐
│        항목           │      WireGuard            │         IPsec            │
├──────────────────────┼──────────────────────────┼──────────────────────────┤
│ 암호화 알고리즘        │ ChaCha20-Poly1305        │ AES-GCM, AES-CBC+HMAC   │
│ 키 교환               │ Curve25519 (자동)         │ PSK 파생 (수동 파일)      │
│ 키 관리               │ 자동 (CiliumNode CRD)     │ 수동 (키 파일 + SPI 관리) │
│ 커널 인터페이스        │ WireGuard 모듈            │ XFRM (netlink)           │
│ 터널 디바이스          │ cilium_wg0               │ 없음 (route mode)        │
│ 노드별 키             │ 피어별 고유 키 쌍          │ PSK에서 SHA-256 파생     │
│ 성능 (일반)           │ 매우 우수                  │ 우수 (AES-NI 활용)       │
│ 코드 복잡도           │ 낮음                       │ 높음 (XFRM 관리)         │
│ 키 로테이션           │ 자동 (Noise Protocol)      │ 수동 (파일 갱신 필요)     │
│ 노드 재부팅 대응       │ 자동 재연결               │ BootID로 키 자동 갱신    │
│ UDP 포트              │ 51871                     │ 해당 없음 (IP 프로토콜)   │
│ 설정 플래그           │ --enable-wireguard        │ --enable-ipsec           │
│ Strict Mode          │ 지원                       │ 지원                     │
│ 노드 암호화           │ --encrypt-node            │ --encrypt-node           │
└──────────────────────┴──────────────────────────┴──────────────────────────┘
```

---

## 4. Mutual Authentication (mTLS)

### 4.1 아키텍처 개요

Cilium의 상호 인증(Mutual Authentication)은 SPIFFE/SPIRE 기반으로 워크로드 간 신원을 검증한다. BPF 데이터패스에서 "인증 필요" 신호를 발생시키면, 사용자 공간의 AuthManager가 mTLS 핸드셰이크를 수행하고 결과를 BPF authmap에 캐싱한다.

```
mTLS 인증 흐름:

  ┌──────────┐        ┌──────────────────┐        ┌──────────────┐
  │ BPF      │ signal │  AuthManager      │        │ SPIRE Agent  │
  │ Datapath │───────▶│  (pkg/auth/)      │        │ (Delegate    │
  │          │        │                   │        │  API)         │
  │ authmap  │◀──────│  ┌───────────────┐│        │              │
  │ 결과캐싱  │ update │  │MutualAuth    ││───────▶│  SVID 발급    │
  └──────────┘        │  │Handler       ││ GetCert│  Trust Bundle│
                      │  └───────┬───────┘│        └──────────────┘
                      │          │ TLS 1.3 │
                      │          ▼         │
                      │   Remote Node의    │
                      │   MutualAuth       │
                      │   Listener         │
                      └──────────────────┘
```

### 4.2 AuthManager 핵심 구현

```go
// 파일: pkg/auth/manager.go

type AuthManager struct {
    authHandlers          map[policyTypes.AuthType]authHandler
    authmap               authMapCacher
    authSignalBackoffTime time.Duration
    pending               map[authKey]struct{}  // 중복 인증 방지
}

type authHandler interface {
    authenticate(*authRequest) (*authResponse, error)
    authType() policyTypes.AuthType
    subscribeToRotatedIdentities() <-chan certs.CertificateRotationEvent
    certProviderStatus() *models.Status
}
```

인증 요청 처리 흐름:

```go
// 파일: pkg/auth/manager.go

func (a *AuthManager) authenticate(key authKey) error {
    // 1. 인증 핸들러 조회 (authType에 따라)
    h := a.authHandlers[key.authType]

    // 2. 원격 노드 IP 조회
    nodeIP := a.nodeIDHandler.GetNodeIP(key.remoteNodeID)

    // 3. 인증 수행
    authResp, err := h.authenticate(&authRequest{
        localIdentity:  key.localIdentity,
        remoteIdentity: key.remoteIdentity,
        remoteNodeIP:   nodeIP,
    })

    // 4. 결과를 BPF authmap에 기록 (만료 시간 포함)
    a.updateAuthMap(key, authResp.expirationTime)
}
```

### 4.3 BPF Auth Map

```go
// 파일: pkg/auth/authmap.go

type authKey struct {
    localIdentity  identity.NumericIdentity  // 로컬 워크로드 ID
    remoteIdentity identity.NumericIdentity  // 원격 워크로드 ID
    remoteNodeID   uint16                    // 원격 노드 ID
    authType       policyTypes.AuthType      // 인증 유형 (SPIRE 등)
}

type authInfo struct {
    expiration time.Time  // 인증 만료 시간
}
```

```
BPF Auth Map 구조:

  Key: (localIdentity=1234, remoteIdentity=5678, remoteNodeID=0x0A, authType=spire)
  Value: (expiration=2024-01-15T10:30:00Z)

  BPF 프로그램이 패킷 처리 시:
  1. 정책에 auth 요구사항이 있는지 확인
  2. authmap에서 유효한 인증 항목 조회
  3. 유효한 항목 있음 → 통과
  4. 없음 → "auth required" 신호 발생, 패킷 드롭
  5. 사용자 공간에서 인증 수행 후 authmap 업데이트
  6. 재전송된 패킷은 authmap 히트 → 통과
```

### 4.4 mTLS 핸드셰이크

`mutualAuthHandler`가 TLS 1.3 기반 상호 인증을 수행한다:

```go
// 파일: pkg/auth/mutual_authhandler.go

func (m *mutualAuthHandler) authenticate(ar *authRequest) (*authResponse, error) {
    // 1. 로컬 Identity에 대한 X.509 인증서 획득
    clientCert, err := m.cert.GetCertificateForIdentity(ar.localIdentity)

    // 2. CA Trust Bundle 획득
    caBundle, err := m.cert.GetTrustBundle()

    // 3. TCP 연결 수립
    conn, err := net.DialTimeout("tcp",
        net.JoinHostPort(ar.remoteNodeIP, strconv.Itoa(m.cfg.MutualAuthListenerPort)),
        m.cfg.MutualAuthConnectTimeout)

    // 4. TLS 1.3 핸드셰이크 (상호 인증서 검증)
    tlsConn := tls.Client(conn, &tls.Config{
        ServerName:   m.cert.NumericIdentityToSNI(ar.remoteIdentity),
        MinVersion:   tls.VersionTLS13,
        GetClientCertificate: func(...) { return clientCert, nil },
        VerifyPeerCertificate: func(rawCerts [][]byte, ...) error {
            // SPIFFE ID 검증 포함
            return m.verifyPeerCertificate(&ar.remoteIdentity, caBundle, chains)
        },
    })
    tlsConn.Handshake()

    return &authResponse{expirationTime: *expirationTime}, nil
}
```

서버 측 리스너:

```go
// 파일: pkg/auth/mutual_authhandler.go

func (m *mutualAuthHandler) handleConnection(ctx context.Context, conn net.Conn) {
    caBundle, _ := m.cert.GetTrustBundle()
    tlsConn := tls.Server(conn, &tls.Config{
        ClientAuth:     tls.RequireAndVerifyClientCert,  // 클라이언트 인증서 필수
        GetCertificate: m.GetCertificateForIncomingConnection,
        MinVersion:     tls.VersionTLS13,
        ClientCAs:      caBundle,
    })
    tlsConn.HandshakeContext(ctx)
}
```

```
mTLS 핸드셰이크 시퀀스:

  Client (Node A, Identity 1234)          Server (Node B, Identity 5678)
       │                                        │
       │──── TCP SYN ──────────────────────────▶│
       │◀─── TCP SYN-ACK ─────────────────────│
       │──── TCP ACK ──────────────────────────▶│
       │                                        │
       │──── TLS ClientHello ──────────────────▶│  SNI: "5678.spiffe.cilium"
       │◀─── TLS ServerHello ─────────────────│
       │◀─── TLS Certificate (Server) ────────│  SVID: spiffe://spiffe.cilium/identity/5678
       │◀─── TLS CertificateRequest ──────────│
       │──── TLS Certificate (Client) ────────▶│  SVID: spiffe://spiffe.cilium/identity/1234
       │──── TLS CertificateVerify ────────────▶│
       │──── TLS Finished ─────────────────────▶│
       │◀─── TLS Finished ────────────────────│
       │                                        │
       │  상호 인증 완료!                         │
       │  authmap에 결과 기록                     │
       │  (만료 시간 = min(client cert, server cert) 만료)
```

---

## 5. SPIFFE/SPIRE 통합

### 5.1 SPIFFE ID 구조

Cilium은 SPIFFE(Secure Production Identity Framework For Everyone) 표준을 사용하여 워크로드 ID를 관리한다.

```
SPIFFE ID 형식:
  spiffe://{trust-domain}/identity/{numeric-identity}

예시:
  spiffe://spiffe.cilium/identity/1234

  ├── spiffe://           : SPIFFE 스키마
  ├── spiffe.cilium       : Trust Domain (설정 가능)
  └── /identity/1234      : Cilium Numeric Identity
```

```go
// 파일: pkg/auth/spire/certificate_provider.go

func (s *SpireDelegateClient) sniToSPIFFEID(id identity.NumericIdentity) string {
    return "spiffe://" + s.cfg.SpiffeTrustDomain + "/identity/" + id.String()
}

// SNI 형식: "{identity}.{trust-domain}"
func (s *SpireDelegateClient) NumericIdentityToSNI(id identity.NumericIdentity) string {
    return id.String() + "." + s.cfg.SpiffeTrustDomain
}
```

### 5.2 SpireDelegateClient

SPIRE Agent와의 통신은 Delegated Identity API를 통해 이루어진다:

```go
// 파일: pkg/auth/spire/delegate.go

type SpireDelegateClient struct {
    cfg SpireDelegateConfig

    svidStore      map[string]*delegatedidentityv1.X509SVIDWithKey  // SPIFFE ID → SVID
    trustBundle    *x509.CertPool                                   // CA 인증서 풀

    rotatedIdentitiesChan chan certs.CertificateRotationEvent       // 인증서 갱신 알림
}

type SpireDelegateConfig struct {
    SpireAdminSocketPath string  // SPIRE 관리 소켓 경로 (기본: /run/spire/sockets/admin.sock)
    SpiffeTrustDomain    string  // Trust Domain (기본: "spiffe.cilium")
    RotatedQueueSize     int     // 인증서 갱신 큐 크기 (기본: 1024)
}
```

### 5.3 X.509 SVID 관리

SVID(SPIFFE Verifiable Identity Document)는 X.509 인증서 형태로 발급된다:

```go
// 파일: pkg/auth/spire/certificate_provider.go

func (s *SpireDelegateClient) GetCertificateForIdentity(id identity.NumericIdentity) (*tls.Certificate, error) {
    spiffeID := s.sniToSPIFFEID(id)  // "spiffe://spiffe.cilium/identity/1234"

    svid := s.svidStore[spiffeID]    // SPIRE에서 발급받은 SVID 조회

    // X.509 인증서 체인에서 리프 인증서 추출
    var leafCert *x509.Certificate
    for _, cert := range svid.X509Svid.CertChain {
        cert, _ := x509.ParseCertificate(cert)
        if !cert.IsCA {
            leafCert = cert
            break
        }
    }

    // PKCS#8 개인키 파싱
    privKey, _ := x509.ParsePKCS8PrivateKey(svid.X509SvidKey)

    return &tls.Certificate{
        Certificate: svid.X509Svid.CertChain,
        PrivateKey:  privKey,
        Leaf:        leafCert,
    }, nil
}
```

### 5.4 Identity 검증

```go
// 파일: pkg/auth/spire/certificate_provider.go

func (s *SpireDelegateClient) ValidateIdentity(id identity.NumericIdentity, cert *x509.Certificate) (bool, error) {
    spiffeID := s.sniToSPIFFEID(id)

    // SPIFFE 표준: URI SAN이 정확히 하나여야 함
    if len(cert.URIs) != 1 {
        return false, errors.New("SPIFFE IDs must have exactly one URI SAN")
    }

    // URI SAN이 예상 SPIFFE ID와 일치하는지 확인
    return cert.URIs[0].String() == spiffeID, nil
}
```

### 5.5 SVID 자동 갱신

SPIRE는 SVID를 주기적으로 갱신하며, Cilium은 이를 감지하여 재인증을 트리거한다:

```go
// 파일: pkg/auth/spire/delegate.go

func (s *SpireDelegateClient) handleX509SVIDUpdate(svids []*delegatedidentityv1.X509SVIDWithKey) {
    // 변경된 키 감지
    for _, svid := range svids {
        key := fmt.Sprintf("spiffe://%s%s", svid.X509Svid.Id.TrustDomain, svid.X509Svid.Id.Path)
        if old, exists := s.svidStore[key]; exists {
            if old.X509Svid.ExpiresAt != svid.X509Svid.ExpiresAt {
                updatedKeys = append(updatedKeys, key)
            }
        }
    }

    // 갱신된 Identity에 대해 재인증 트리거
    for _, key := range updatedKeys {
        id, _ := s.spiffeIDToNumericIdentity(key)
        s.rotatedIdentitiesChan <- certs.CertificateRotationEvent{Identity: id}
    }
}
```

```
// 파일: pkg/auth/manager.go

func (a *AuthManager) handleCertificateRotationEvent(event certs.CertificateRotationEvent) error {
    // authmap의 모든 항목을 순회
    all, _ := a.authmap.All()
    for k := range all {
        if k.localIdentity == event.Identity || k.remoteIdentity == event.Identity {
            if event.Deleted {
                a.authmap.Delete(k)      // 인증서 삭제됨 → authmap 항목 삭제
            } else {
                a.handleAuthenticationFunc(a, k, true)  // 재인증 트리거
            }
        }
    }
}
```

---

## 6. CertificateProvider 인터페이스

### 6.1 인터페이스 정의

```go
// 파일: pkg/auth/certs/provider.go

type CertificateProvider interface {
    GetTrustBundle() (*x509.CertPool, error)
    GetCertificateForIdentity(id identity.NumericIdentity) (*tls.Certificate, error)
    ValidateIdentity(id identity.NumericIdentity, cert *x509.Certificate) (bool, error)
    NumericIdentityToSNI(id identity.NumericIdentity) string
    SNIToNumericIdentity(sni string) (identity.NumericIdentity, error)
    SubscribeToRotatedIdentities() <-chan CertificateRotationEvent
    Status() *models.Status
}
```

### 6.2 인증서 갱신 이벤트

```go
// 파일: pkg/auth/certs/provider.go

type CertificateRotationEvent struct {
    Identity identity.NumericIdentity  // 갱신된 Identity
    Deleted  bool                      // 삭제 여부
}
```

---

## 7. 인증서 관리 (certloader)

### 7.1 Watcher 기반 자동 리로드

`pkg/crypto/certloader/` 패키지는 TLS 인증서 파일을 감시하고 자동으로 리로드한다:

```go
// 파일: pkg/crypto/certloader/watcher.go

type Watcher struct {
    *FileReloader
    fswatcher *fswatcher.Watcher  // 파일 시스템 감시자
    stop      chan struct{}
}

func NewWatcher(log *slog.Logger, caFiles []string, certFile, privkeyFile string) (*Watcher, error) {
    r, _ := NewFileReloaderReady(caFiles, certFile, privkeyFile)
    fswatcher, _ := newFsWatcher(log, caFiles, certFile, privkeyFile)
    w := &Watcher{FileReloader: r, fswatcher: fswatcher}
    w.Watch()
    return w, nil
}
```

파일 변경 감지 시 이벤트 코얼레싱(100ms)을 통해 불필요한 리로드를 방지한다:

```go
// 파일: pkg/crypto/certloader/watcher.go

func (w *Watcher) Watch() <-chan struct{} {
    go func() {
        for {
            select {
            case event := <-w.fswatcher.Events:
                if keypairUpdated {
                    keypairReload = time.After(100 * time.Millisecond)  // 코얼레싱
                }
            case <-keypairReload:
                keypair, err := w.ReloadKeypair()
                if w.Ready() { markReady() }
            case <-caReload:
                w.ReloadCA()
                if w.Ready() { markReady() }
            }
        }
    }()
}
```

### 7.2 mTLS 클라이언트 설정

```go
// 파일: pkg/crypto/certloader/client.go

type WatchedClientConfig struct {
    *Watcher
}

func (c *WatchedClientConfig) ClientConfig(base *tls.Config) *tls.Config {
    keypair, caCertPool := c.KeypairAndCACertPool()
    tlsConfig := base.Clone()
    tlsConfig.RootCAs = caCertPool
    if c.IsMutualTLS() {
        tlsConfig.GetClientCertificate = func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
            return keypair, nil  // 항상 최신 인증서 반환
        }
    }
    return tlsConfig
}
```

### 7.3 FutureWatcher 패턴

인증서 파일이 아직 존재하지 않는 경우에도 대기하며 감시를 시작할 수 있다:

```go
// 파일: pkg/crypto/certloader/watcher.go

func FutureWatcher(ctx context.Context, ...) (<-chan *Watcher, error) {
    r, _ := NewFileReloader(caFiles, certFile, privkeyFile)  // 아직 로드 안 됨
    fswatcher, _ := newFsWatcher(log, caFiles, certFile, privkeyFile)

    go func() {
        // 초기 로드 시도
        w.ReloadKeypair()
        w.ReloadCA()
        ready := w.Watch()

        if 초기 로드 실패 {
            log.Warn(InitialLoadWarn)
            <-ready  // 파일이 생성될 때까지 대기
        }
        res <- w
    }()
}
```

---

## 8. BPF에서의 투명 암호화

### 8.1 패킷 마킹

BPF 프로그램은 패킷에 암호화 마크를 설정하여 커널의 암호화 처리를 트리거한다:

```c
// 파일: bpf/lib/encrypt.h

static __always_inline void
set_decrypt_mark(struct __ctx_buff *ctx, __u16 node_id)
{
    // Decrypt "key"는 SPI와 발신 노드에 의해 결정
    ctx->mark = MARK_MAGIC_DECRYPT | node_id << 16;
}
```

```
투명 암호화 데이터패스 흐름:

  송신 (Encrypt):
  ┌──────────┐    ┌──────────┐    ┌───────────┐    ┌──────────┐
  │ Pod      │───▶│ BPF      │───▶│ 커널      │───▶│ 네트워크  │
  │ 트래픽    │    │ 마킹      │    │ XFRM/WG   │    │ (암호화됨)│
  │          │    │ 0x0E00   │    │ 암호화     │    │          │
  └──────────┘    └──────────┘    └───────────┘    └──────────┘

  수신 (Decrypt):
  ┌──────────┐    ┌───────────┐    ┌──────────┐    ┌──────────┐
  │ 네트워크  │───▶│ 커널      │───▶│ BPF      │───▶│ Pod      │
  │ (암호화됨)│    │ XFRM/WG   │    │ 마킹확인  │    │ 트래픽    │
  │          │    │ 복호화     │    │ 0x0D00   │    │          │
  └──────────┘    └───────────┘    └──────────┘    └──────────┘
```

### 8.2 마크 상수

```
// 파일: pkg/datapath/linux/linux_defaults/mark.go

MagicMarkDecrypt            = 0x0D00   // 복호화가 필요한 패킷
MagicMarkEncrypt            = 0x0E00   // 암호화가 필요한 패킷
MagicMarkWireGuardEncrypted = 0x0E00   // WireGuard 암호화 완료 (= MagicMarkEncrypt)
MagicMarkDecryptedOverlay   = 0x1D00   // 오버레이에서 복호화된 패킷
```

### 8.3 Strict Mode

Strict Mode는 암호화 없이 나가는 트래픽을 차단한다:

```c
// 파일: bpf/lib/encrypt.h

#ifdef ENCRYPTION_STRICT_MODE_EGRESS
static __always_inline bool
strict_allow(struct __ctx_buff *ctx, __be16 proto) {
    // 노드에서 직접 발생한 트래픽은 허용
    if (ip4->saddr == IPV4_GATEWAY || ip4->saddr == IPV4_ENCRYPT_IFACE)
        return true;

    // Strict CIDR 내의 소스/목적지 트래픽인지 확인
    in_strict_cidr = ipv4_is_in_subnet(ip4->daddr, STRICT_IPV4_NET, STRICT_IPV4_NET_SIZE);
    in_strict_cidr &= ipv4_is_in_subnet(ip4->saddr, STRICT_IPV4_NET, STRICT_IPV4_NET_SIZE);

    return !in_strict_cidr;  // CIDR 내부 트래픽이면 차단 (암호화 필수)
}
#endif
```

---

## 9. Hive Cell 등록

### 9.1 WireGuard Cell

```go
// 파일: pkg/wireguard/agent/cell.go

var Cell = cell.Module(
    "wireguard-agent",
    "Manages WireGuard device and peers",
    cell.Config(defaultUserConfig),
    cell.Provide(newWireguardAgent, newWireguardConfig),
)
```

### 9.2 IPsec Cell

```go
// 파일: pkg/datapath/linux/ipsec/cell.go

var Cell = cell.Module(
    "ipsec-agent",
    "Handles initial key setup and knows the key size",
    cell.Config(defaultUserConfig),
    cell.Provide(newIPsecAgent, newIPsecConfig),
)
```

### 9.3 Auth Cell

```go
// 파일: pkg/auth/cell.go

var Cell = cell.Module(
    "auth",
    "Authenticates requests as demanded by policy",
    spire.Cell,                          // SPIRE 서브모듈
    cell.Provide(registerAuthManager),
    cell.ProvidePrivate(
        newMutualAuthHandler,            // mTLS 핸들러
        newAlwaysFailAuthHandler,        // 항상 실패 핸들러 (테스트용)
    ),
)
```

---

## 10. 설정 플래그 요약

```
# WireGuard 활성화
--enable-wireguard                        # WireGuard 투명 암호화 활성화
--encrypt-node                            # 노드 간 트래픽도 암호화
--node-encryption-opt-out-labels          # 암호화 제외 노드 라벨 (기본: control-plane)
--wireguard-persistent-keepalive          # 킵얼라이브 간격

# IPsec 활성화
--enable-ipsec                            # IPsec 투명 암호화 활성화
--ipsec-key-file                          # IPsec 키 파일 경로
--enable-ipsec-key-watcher                # 키 파일 변경 감시 (기본: true)
--ipsec-key-rotation-duration             # 키 로테이션 대기 시간 (기본: 5m)

# Mutual Authentication
--mesh-auth-enabled                       # 상호 인증 활성화
--mesh-auth-mutual-listener-port          # mTLS 리스너 포트
--mesh-auth-mutual-connect-timeout        # mTLS 연결 타임아웃 (기본: 5s)
--mesh-auth-spire-admin-socket            # SPIRE 관리 소켓 경로
--mesh-auth-spiffe-trust-domain           # SPIFFE Trust Domain (기본: spiffe.cilium)
--mesh-auth-gc-interval                   # 인증 캐시 GC 간격 (기본: 5m)
```

---

## 11. 주요 소스 파일 참조

| 영역 | 파일 경로 | 설명 |
|------|-----------|------|
| WireGuard Agent | `pkg/wireguard/agent/agent.go` | WireGuard 에이전트 핵심 구현 |
| WireGuard Cell | `pkg/wireguard/agent/cell.go` | Hive Cell 등록 및 설정 |
| WireGuard Types | `pkg/wireguard/types/types.go` | 상수 및 인터페이스 정의 |
| WireGuard Node | `pkg/wireguard/agent/node_handler.go` | 노드 이벤트 처리 |
| IPsec Agent | `pkg/datapath/linux/ipsec/ipsec_linux.go` | IPsec XFRM 관리 핵심 |
| IPsec Cell | `pkg/datapath/linux/ipsec/cell.go` | Hive Cell 등록 |
| IPsec XFRM Cache | `pkg/datapath/linux/ipsec/xfrm_state_cache.go` | XFRM 상태 캐시 |
| Auth Manager | `pkg/auth/manager.go` | 인증 관리자 |
| Auth Cell | `pkg/auth/cell.go` | 인증 Hive Cell |
| Auth Map | `pkg/auth/authmap.go` | BPF auth map 추상화 |
| Mutual Auth | `pkg/auth/mutual_authhandler.go` | mTLS 핸드셰이크 구현 |
| SPIRE Client | `pkg/auth/spire/delegate.go` | SPIRE Delegate API 클라이언트 |
| SPIRE Cert | `pkg/auth/spire/certificate_provider.go` | SVID/인증서 제공자 |
| Cert Provider | `pkg/auth/certs/provider.go` | CertificateProvider 인터페이스 |
| Certloader | `pkg/crypto/certloader/watcher.go` | 인증서 파일 감시/리로드 |
| Certloader Client | `pkg/crypto/certloader/client.go` | mTLS 클라이언트 설정 |
| BPF Encrypt | `bpf/lib/encrypt.h` | BPF 암호화 마킹 헤더 |
| Mark Defaults | `pkg/datapath/linux/linux_defaults/mark.go` | 마크 상수 정의 |

---

## 12. 핵심 설계 원칙

1. **투명성**: 애플리케이션 수정 없이 모든 트래픽을 암호화한다. BPF에서 패킷을 마킹하고 커널이 자동으로 암호화/복호화한다.

2. **무중단 키 로테이션**: WireGuard는 Noise Protocol이 자동으로 키를 갱신하고, IPsec은 SPI 증가와 이전 키 유지(5분)를 통해 연결 중단 없이 키를 교체한다.

3. **Identity 기반 보안**: Kubernetes 네트워크 주소가 아닌 SPIFFE Identity를 기반으로 워크로드를 인증한다. IP 주소 변경에 무관하게 보안을 유지한다.

4. **계층적 보안**: 투명 암호화(L3/L4)와 상호 인증(L7)을 결합하여 다층 보안을 제공한다.

5. **평문 유출 방지**: IPsec의 Catch-all Drop Policy와 WireGuard/IPsec의 Strict Mode를 통해 암호화 전환 중에도 평문 트래픽이 유출되지 않도록 보장한다.
