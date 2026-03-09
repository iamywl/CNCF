# PoC: Terraform Registry 클라이언트 & 릴리즈 인증 시뮬레이션

## 개요

Terraform Registry의 모듈 버전 조회/다운로드와 릴리즈 인증(SHA-256 체크섬 + GPG 서명)
시스템을 시뮬레이션한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `Disco` | `svchost/disco/disco.go` | 서비스 디스커버리 |
| `RegistryClient` | `internal/registry/client.go` | Registry API 클라이언트 |
| `RetryableClient` | `retryablehttp.Client` | 재시도 HTTP 클라이언트 |
| `CredentialsStore` | `internal/command/cliconfig/credentials.go` | 자격 증명 관리 |
| `ChecksumAuthenticator` | `internal/releaseauth/checksum.go` | SHA-256 체크섬 검증 |
| `SignatureAuthenticator` | `internal/releaseauth/signature.go` | GPG 서명 검증 |
| `AllAuthenticator` | `internal/releaseauth/all.go` | 다중 인증기 체이닝 |

## 구현 내용

### 1. 서비스 디스커버리
- `.well-known/terraform.json` 기반 서비스 URL 해석
- 공개/비공개 레지스트리 지원

### 2. 모듈 버전 조회 및 다운로드
- GET /v1/modules/{ns}/{name}/{provider}/versions
- X-Terraform-Get 헤더로 다운로드 위치 반환
- 에러 분류 (재시도 가능/불가능)

### 3. 릴리즈 인증
- SHA-256 체크섬 검증 (변조 탐지)
- GPG 서명 검증 (출처 확인)
- All Authenticator 패턴으로 모든 인증 필수

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- 서비스 디스커버리는 비공개 레지스트리를 투명하게 지원한다
- 체크섬과 서명 2단계 인증으로 공급망 공격을 방지한다
- All Authenticator는 하나라도 실패하면 전체 실패로 처리한다
