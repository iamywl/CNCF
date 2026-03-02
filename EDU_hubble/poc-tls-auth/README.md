# PoC: Hubble mTLS/TLS 인증 패턴

## 관련 문서
- [02-ARCHITECTURE.md](../02-ARCHITECTURE.md) - 통신 보안 계층
- [06-OPERATIONS.md](../06-OPERATIONS.md) - TLS 설정 및 인증서 관리

## 개요

Hubble은 gRPC 통신에 TLS/mTLS를 사용하여 보안을 보장합니다:
- **CLI → Relay**: 단방향 TLS (서버 인증서만 검증)
- **Relay → Server**: mTLS (양방향 인증, 상호 인증서 검증)
- **최소 TLS 1.3 강제**: 이전 버전 연결 자동 거부

## 실행

```bash
go run main.go
```

## 시나리오

### 시나리오 1: 단방향 TLS
- 서버 인증서만 검증
- 클라이언트 인증서 불필요
- CLI → Relay 통신에 사용

### 시나리오 2: mTLS (양방향 인증)
- 서버/클라이언트 양쪽 인증서 검증
- Relay ↔ Server 통신에 사용

### 시나리오 3: mTLS 인증 실패
- 클라이언트 인증서 없이 mTLS 서버 접속 시도
- 예상대로 연결 거부됨

### 시나리오 4: TLS 버전 강제
- TLS 1.2 클라이언트가 TLS 1.3 서버에 접속 시도
- 최소 버전 요구사항 미달로 연결 거부

## 핵심 학습 내용
- Go의 `crypto/tls` 패키지로 TLS/mTLS 구현
- CA → Server/Client 인증서 체인 구성
- `tls.Config`의 `ClientAuth` 설정으로 mTLS 활성화
- `MinVersion` 설정으로 TLS 버전 강제
- 실제 Hubble: `certloader` 패턴으로 인증서 동적 리로딩 지원
