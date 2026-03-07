# PoC: SPIFFE 기반 mTLS 핸드셰이크

## 개요

이 PoC는 Istio의 **mTLS(mutual TLS) 핸드셰이크** 메커니즘을 시뮬레이션한다.
Istio에서 모든 워크로드 간 통신은 SPIFFE identity 기반의 mTLS로 자동 암호화되며,
이 PoC는 그 핵심 알고리즘을 Go 표준 라이브러리만으로 재현한다.

## 시뮬레이션하는 핵심 개념

### 1. SPIFFE Identity
- `spiffe://trust-domain/ns/namespace/sa/service-account` 형식의 워크로드 식별자
- x509 인증서의 URI SAN(Subject Alternative Name)에 삽입
- trust domain별 격리를 통한 보안 경계 설정

### 2. Self-Signed CA (Citadel)
- Istio의 istiod에 내장된 자체 서명 CA 시뮬레이션
- ECDSA P256 키를 사용한 인증서 생성 및 서명
- CSR(Certificate Signing Request) 기반 워크로드 인증서 발급

### 3. PeerCertVerifier
- trust domain별 CA cert pool을 관리하는 인증서 검증기
- 피어 인증서의 URI SAN에서 trust domain을 추출하여 적절한 pool로 검증
- trust domain 연합(federation)을 통한 cross-cluster mTLS 지원

### 4. mTLS 핸드셰이크 흐름
- 서버 프록시와 클라이언트 프록시가 상호 인증서를 교환
- 양쪽 모두 SPIFFE identity를 검증하여 워크로드 신원 확인
- crypto/tls 패키지의 `VerifyPeerCertificate` 콜백 활용

## 실행 방법

```bash
cd istio_EDU/poc-mtls
go run main.go
```

## 예상 출력

```
==========================================================
 Istio mTLS 핸드셰이크 시뮬레이션 (SPIFFE 기반)
==========================================================

[단계 1] Self-Signed CA 생성 (Citadel/istiod CA 시뮬레이션)
----------------------------------------------------------
  CA 'cluster.local' 생성 완료
    [CA 인증서 정보]
      Subject:    Istio CA Root - cluster.local
      IsCA:       true
      KeyUsage:   CertSign, CRLSign

[단계 2] 워크로드 인증서 발급 (SPIFFE URI SAN 포함)
----------------------------------------------------------
  서버 SPIFFE ID: spiffe://cluster.local/ns/default/sa/reviews
  클라이언트 SPIFFE ID: spiffe://cluster.local/ns/default/sa/productpage
    [서버 워크로드 인증서 정보]
      URI SAN: spiffe://cluster.local/ns/default/sa/reviews
    [클라이언트 워크로드 인증서 정보]
      URI SAN: spiffe://cluster.local/ns/default/sa/productpage

[단계 4] mTLS 핸드셰이크 수행
----------------------------------------------------------
    [서버 프록시] 클라이언트 SPIFFE ID 확인: spiffe://cluster.local/ns/default/sa/productpage
    [클라이언트 프록시] 서버 SPIFFE ID 확인: spiffe://cluster.local/ns/default/sa/reviews

[단계 6] Cross Trust Domain 테스트 (실패 케이스)
----------------------------------------------------------
    [결과] 서버 측 핸드셰이크 실패 (예상대로): ...
    [결과] 클라이언트 측 핸드셰이크 실패 (예상대로): ...

[단계 7] Trust Domain 연합 (Federation)
----------------------------------------------------------
    [서버 프록시] 클라이언트 SPIFFE ID 확인: spiffe://remote-cluster.example.com/ns/prod/sa/web-client
    [클라이언트 프록시] 서버 SPIFFE ID 확인: spiffe://cluster.local/ns/prod/sa/api-server
```

## 참조 Istio 소스 코드

| 소스 파일 | 설명 |
|-----------|------|
| `pkg/spiffe/spiffe.go` | SPIFFE Identity 구조체, ParseIdentity(), PeerCertVerifier |
| `security/pkg/pki/ca/ca.go` | IstioCA 구조체, Sign(), GenKeyCert(), self-signed CA 생성 |
| `security/pkg/pki/util/generate_cert.go` | 인증서 생성 유틸리티, GenCertFromCSR(), genCertTemplateFromCSR() |
| `security/pkg/server/ca/server.go` | CA 서버, CreateCertificate() - CSR 처리 및 인증서 발급 |

### 주요 Istio 함수 매핑

| PoC 함수 | Istio 원본 | 설명 |
|----------|-----------|------|
| `ParseIdentity()` | `spiffe.ParseIdentity()` | SPIFFE URI 파싱 |
| `Identity.String()` | `Identity.String()` | SPIFFE URI 생성 |
| `NewCA()` | `NewSelfSignedDebugIstioCAOptions()` | 자체 서명 CA 생성 |
| `SignWorkloadCert()` | `IstioCA.sign()` + `GenCertFromCSR()` | CSR 서명 |
| `PeerCertVerifier` | `spiffe.PeerCertVerifier` | trust domain 기반 인증서 검증 |
| `VerifyPeerCert()` | `PeerCertVerifier.VerifyPeerCert()` | 피어 인증서 검증 |

## 아키텍처 다이어그램

```
┌──────────────────────────────────────────────────────────────┐
│                    istiod (Control Plane)                     │
│  ┌─────────────────────────────────────────────────────────┐ │
│  │            Citadel CA (Self-Signed)                     │ │
│  │  ┌──────────┐  ┌──────────┐  ┌────────────────────┐    │ │
│  │  │ CA Cert  │  │ CA Key   │  │ Trust Domain:      │    │ │
│  │  │ (Root)   │  │ (ECDSA)  │  │ cluster.local      │    │ │
│  │  └──────────┘  └──────────┘  └────────────────────┘    │ │
│  └──────────────┬──────────────────────────┬───────────────┘ │
│                 │ SDS (인증서 발급)          │                 │
└─────────────────┼──────────────────────────┼─────────────────┘
                  │                          │
    ┌─────────────▼──────────┐  ┌────────────▼─────────────┐
    │  Workload A (Client)   │  │  Workload B (Server)     │
    │  ┌──────────────────┐  │  │  ┌──────────────────┐    │
    │  │ Envoy Proxy      │  │  │  │ Envoy Proxy      │    │
    │  │                  │  │  │  │                  │    │
    │  │ SPIFFE ID:       │  │  │  │ SPIFFE ID:       │    │
    │  │ spiffe://cluster │  │  │  │ spiffe://cluster │    │
    │  │ .local/ns/default│──┼──┼──│ .local/ns/default│    │
    │  │ /sa/productpage  │ mTLS│  │ /sa/reviews      │    │
    │  └──────────────────┘  │  │  └──────────────────┘    │
    └────────────────────────┘  └──────────────────────────┘
```

## mTLS 핸드셰이크 시퀀스

```
  Client Proxy                           Server Proxy
       │                                       │
       │──── ClientHello ─────────────────────>│
       │                                       │
       │<─── ServerHello + Server Certificate ─│
       │     (SPIFFE URI SAN 포함)              │
       │                                       │
       │──── Client Certificate ──────────────>│
       │     (SPIFFE URI SAN 포함)              │
       │                                       │
       │  ┌──────────────────────────────────┐ │
       │  │ 양쪽 PeerCertVerifier 검증:       │ │
       │  │ 1. URI SAN에서 trust domain 추출  │ │
       │  │ 2. trust domain cert pool 조회    │ │
       │  │ 3. x509 인증서 체인 검증          │ │
       │  └──────────────────────────────────┘ │
       │                                       │
       │<─── Finished ────────────────────────>│
       │                                       │
       │  === 암호화된 애플리케이션 데이터 ===    │
       │<────────────────────────────────────>│
```
