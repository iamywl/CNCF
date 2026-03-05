# PoC-09: TLS 핸드셰이크 시뮬레이션

## 개념

gRPC는 `TransportCredentials` 인터페이스를 통해 전송 계층 보안을 추상화한다. 이 PoC는 grpc-go의 `credentials/credentials.go`에 정의된 핵심 인터페이스와 TLS 핸드셰이크 과정을 시뮬레이션한다.

### 핵심 구조

```
TransportCredentials 인터페이스
├── ClientHandshake(conn, authority) → (conn, AuthInfo, error)
├── ServerHandshake(conn) → (conn, AuthInfo, error)
├── Info() → ProtocolInfo
└── Clone() → TransportCredentials

SecurityLevel 열거형
├── NoSecurity (0)         ← insecure
├── IntegrityOnly (1)      ← 서명만
└── PrivacyAndIntegrity (2) ← TLS (암호화+서명)
```

### TLS 핸드셰이크 흐름

```
클라이언트                           서버
    │                                 │
    │─── ClientHello (nonce) ────────→│
    │                                 │
    │←── ServerHello + Certificate ───│
    │                                 │
    │─── ECDHE 키 교환 ──────────────→│
    │                                 │
    │←── ServerFinished ──────────────│
    │                                 │
    │─── ClientFinished ─────────────→│
    │                                 │
    [=== 암호화된 채널 수립 완료 ===]
```

### Insecure vs TLS

| 항목 | Insecure | TLS |
|------|----------|-----|
| SecurityLevel | NoSecurity | PrivacyAndIntegrity |
| 핸드셰이크 | 없음 (패스스루) | 인증서 검증 + 키 교환 |
| 인증 정보 | InsecureAuthInfo | TLSAuthInfo (인증서, 암호 스위트) |
| 용도 | 개발/테스트 | 프로덕션 |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
gRPC Credentials 시뮬레이션
========================================

[1] Insecure Credentials 핸드셰이크
────────────────────────────────────
  프로토콜: insecure, 보안수준: NoSecurity
  [insecure] 서버 핸드셰이크: 보안 없음
  서버 결과: authType=insecure, securityLevel=NoSecurity
  [insecure] 클라이언트 핸드셰이크: 보안 없음 (authority=localhost:50051)
  클라이언트 결과: authType=insecure, securityLevel=NoSecurity

[2] TLS Credentials 핸드셰이크 (자체서명)
────────────────────────────────────────
  인증서 생성: CN=grpc-test-server (PEM ...)
  프로토콜: tls 1.3, 서버: grpc-test-server, 보안수준: PrivacyAndIntegrity
  ...
  [TLS] 핸드셰이크 완료: TLS 1.3 / TLS_AES_128_GCM_SHA256

[3] SecurityLevel 검증
──────────────────────
  insecure (level=1) vs 요구수준 NoSecurity (level=1): 허용
  insecure (level=1) vs 요구수준 PrivacyAndIntegrity (level=3): 거부
  TLS (level=3) vs 요구수준 PrivacyAndIntegrity (level=3): 허용
```

## 관련 소스

| 파일 | 설명 |
|------|------|
| `credentials/credentials.go` | TransportCredentials 인터페이스, SecurityLevel 정의 |
| `credentials/tls.go` | TLS credentials 구현 |
| `credentials/insecure/insecure.go` | Insecure credentials 구현 |
