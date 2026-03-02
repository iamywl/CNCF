# PoC 16: Cilium 인증/암호화 서브시스템 시뮬레이션

## 개요

이 PoC는 Cilium의 인증 및 암호화 서브시스템의 핵심 메커니즘을 순수 Go 표준 라이브러리로 시뮬레이션합니다. 외부 의존성 없이 실행 가능하며, 실제 Cilium 코드의 동작 원리를 이해하는 데 목적이 있습니다.

## 실행 방법

```bash
cd EDU/poc-16-auth-encryption
go run main.go
```

## 시뮬레이션 구성

### 1. WireGuard 터널 시뮬레이션

**참조 코드**: `pkg/wireguard/agent/agent.go`

- 키 쌍 생성 (실제: `wgtypes.GeneratePrivateKey()`)
- 피어 교환 (실제: CiliumNode CRD를 통한 공개키 교환)
- AES-GCM 기반 암호화/복호화 (실제: ChaCha20-Poly1305)
- AllowedIPs 관리 시뮬레이션

시뮬레이션된 흐름:
```
Node A: 키 생성 → 공개키를 CiliumNode에 게시
                                    ↓ K8s Watch
Node B: 피어 등록 → updatePeer() → cilium_wg0 설정
                                    ↓
양방향 암호화 터널 수립 (UDP:51871)
```

### 2. IPsec SA 수명주기 시뮬레이션

**참조 코드**: `pkg/datapath/linux/ipsec/ipsec_linux.go`

- XFRM Security Association (SA) 협상 및 설정
- 노드별 키 파생: `computeNodeIPsecKey()` (SHA-256 해시)
- XFRM 상태/정책 설치: `UpsertIPsecEndpoint()`
- ESP 패킷 구조 시뮬레이션
- 마크 값 생성: `generateEncryptMark()`, `generateDecryptMark()`

마크 구조:
```
Encrypt: 0x{NodeID:16bit}{SPI:4bit}0E00
Decrypt: 0x{NodeID:16bit}0D00
```

### 3. SPIFFE Identity 시뮬레이션

**참조 코드**: `pkg/auth/spire/delegate.go`, `pkg/auth/spire/certificate_provider.go`

- Trust Domain 설정 (기본: `spiffe.cilium`)
- SPIFFE ID 구조: `spiffe://{trust-domain}/identity/{numeric-id}`
- X.509 SVID(SPIFFE Verifiable Identity Document) 발급
- SNI 변환: `NumericIdentityToSNI()`, `SNIToNumericIdentity()`
- URI SAN 기반 Identity 검증: `ValidateIdentity()`
- SVID 자동 갱신 및 이벤트 전파

### 4. mTLS 상호 인증 핸드셰이크 시뮬레이션

**참조 코드**: `pkg/auth/mutual_authhandler.go`, `pkg/auth/manager.go`

- TLS 1.3 기반 상호 인증서 검증
- 클라이언트/서버 양방향 인증서 교환
- Trust Bundle 기반 인증서 체인 검증
- SPIFFE ID 매칭 검증
- BPF Auth Map 캐싱 (`pkg/auth/authmap.go`)
- 인증서 갱신 시 재인증 트리거

핸드셰이크 시퀀스:
```
Client (Identity 1234)              Server (Identity 5678)
    |-- TCP SYN ----------------------->|
    |<- TCP SYN-ACK --------------------|
    |-- TLS ClientHello (SNI) --------->|
    |<- TLS ServerHello + Certificate --|
    |<- CertificateRequest -------------|
    |-- Certificate + CertificateVerify->|
    |-- Finished ----------------------->|
    |<- Finished ------------------------|
```

### 5. 투명 암호화 시뮬레이션

**참조 코드**: `bpf/lib/encrypt.h`, `pkg/datapath/linux/linux_defaults/mark.go`

- BPF 패킷 마킹: `MARK_MAGIC_ENCRYPT (0x0E00)`, `MARK_MAGIC_DECRYPT (0x0D00)`
- WireGuard 모드: 패킷 마킹 -> cilium_wg0 -> 암호화 -> UDP 전송
- IPsec 모드: 패킷 마킹 -> XFRM -> ESP 암호화 -> IP 전송
- Strict Mode 검사: 암호화 마크 없는 트래픽 차단
- 수신 처리: 복호화 -> 마킹 -> Pod 전달

```
송신: Pod → BPF(마킹 0x0E00) → 커널(암호화) → 네트워크
수신: 네트워크 → 커널(복호화) → BPF(마킹 0x0D00) → Pod
```

### 6. 무중단 키 로테이션 시뮬레이션

**참조 코드**: `pkg/datapath/linux/ipsec/ipsec_linux.go` - `keyfileWatcher()`, `ipSecSPICanBeReclaimed()`

- fswatcher 기반 키 파일 변경 감지
- SPI 증가를 통한 키 버전 관리
- 이전 키와 새 키 동시 유지 (IPsecKeyRotationDuration)
- 이전 SPI 안전 회수 타이밍 검증
- 연결 중단 없는 키 교체 보장

로테이션 타임라인:
```
t=0:     키 파일 변경 감지
t=0:     새 XFRM 상태 설치 (SPI=N+1)
t=0:     BPF 맵 업데이트 → 새 트래픽은 SPI=N+1
t=0~5m:  이전 SPI=N 트래픽도 복호화 가능
t=5m:    이전 XFRM 상태 제거 (안전 회수)
```

## 핵심 Cilium 소스 파일 매핑

| PoC 컴포넌트 | Cilium 소스 파일 |
|-------------|-----------------|
| `WireGuardAgent` | `pkg/wireguard/agent/agent.go` |
| `WireGuardPeer` | `pkg/wireguard/agent/agent.go` (peerConfig) |
| `IPsecSA` | `pkg/datapath/linux/ipsec/ipsec_linux.go` (Agent) |
| `XfrmState` | `pkg/datapath/linux/ipsec/ipsec_linux.go` (ipSecNewState) |
| `SPIREAgent` | `pkg/auth/spire/delegate.go` (SpireDelegateClient) |
| `SVID` | `pkg/auth/spire/certificate_provider.go` |
| `MutualAuthHandler` | `pkg/auth/mutual_authhandler.go` |
| `AuthMapCache` | `pkg/auth/authmap_cache.go` |
| `AuthKey` | `pkg/auth/authmap.go` |
| `TransparentEncryption` | `bpf/lib/encrypt.h` |
| `PacketMark` 상수들 | `pkg/datapath/linux/linux_defaults/mark.go` |

## 시뮬레이션 vs 실제 구현 차이점

| 항목 | 시뮬레이션 | 실제 Cilium |
|------|-----------|-------------|
| WireGuard 암호화 | AES-GCM (시뮬레이션) | ChaCha20-Poly1305 (Noise Protocol) |
| 키 교환 | SHA-256 해시 기반 | Curve25519 ECDH |
| IPsec 커널 인터페이스 | 메모리 내 맵 | netlink XFRM API |
| SPIRE 통신 | 로컬 함수 호출 | gRPC Unix Socket (Delegated Identity API) |
| BPF 마킹 | 구조체 필드 설정 | skb->mark 설정 (커널) |
| Auth Map | Go map | BPF hash map (커널) |
| 인증서 관리 | 메모리 내 | 파일 시스템 감시 (fswatcher) |
| 키 로테이션 | 즉시 시뮬레이션 | fswatcher + nodeHandler 연동 |

## 참고 문서

- `EDU/16-auth-encryption.md` - 상세 기술 문서
- [Cilium Transparent Encryption](https://docs.cilium.io/en/stable/security/network/encryption/)
- [Cilium Mutual Authentication](https://docs.cilium.io/en/stable/network/servicemesh/mutual-authentication/)
- [SPIFFE 표준](https://spiffe.io/)
