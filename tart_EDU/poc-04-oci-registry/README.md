# PoC-04: OCI 레지스트리 Pull/Push 프로토콜 시뮬레이션

## 개요

tart의 OCI 레지스트리 통신 프로토콜을 Go 표준 라이브러리만으로 재현한다.
OCI Distribution Specification의 핵심 엔드포인트(manifest, blob, upload)와
Bearer 토큰 인증 흐름을 HTTP 서버/클라이언트로 시뮬레이션한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다. `net/http/httptest`로 인메모리 HTTP 서버를 구동하여
실제 레지스트리 없이도 전체 Push/Pull 워크플로우를 검증할 수 있다.

## 핵심 시뮬레이션 포인트

### 1. Push 프로토콜 (tart pushBlob/pushManifest)
- **Blob Upload 2단계**: POST로 업로드 세션 시작(202 + Location) → PUT으로 monolithic upload(201)
- **Manifest Push**: PUT /v2/{ns}/manifests/{ref} → 201 Created
- **SHA256 다이제스트**: 업로드 시 digest 파라미터로 무결성 검증

### 2. Pull 프로토콜 (tart pullManifest/pullBlob)
- **Manifest Pull**: GET /v2/{ns}/manifests/{ref} → JSON 디코딩
- **Blob Pull**: manifest의 config.digest, layers[].digest로 각 blob GET
- **다이제스트 검증**: Pull 후 SHA256 해시 비교

### 3. Bearer 토큰 인증 (tart auth/WWWAuthenticate/TokenResponse)
- 최초 요청 시 401 + `WWW-Authenticate: Bearer realm=...,service=...,scope=...`
- realm URL로 Basic 인증 GET → TokenResponse(token, expires_in, issued_at) 수신
- 만료된 토큰 → 자동 재인증 흐름

### 4. WWW-Authenticate 헤더 파싱 (tart WWWAuthenticate.swift)
- scheme + key=value 디렉티브 파싱
- 따옴표 내부 쉼표 처리(contextAwareCommaSplit)

## tart 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `Sources/tart/OCI/Registry.swift` | Registry 클래스: pushBlob, pullBlob, pushManifest, pullManifest, auth, channelRequest |
| `Sources/tart/OCI/Manifest.swift` | OCIManifest, OCIManifestConfig, OCIManifestLayer 구조체 |
| `Sources/tart/OCI/Digest.swift` | SHA256 다이제스트 계산 (Digest.hash) |
| `Sources/tart/OCI/WWWAuthenticate.swift` | WWW-Authenticate 헤더 파서 |
| `Sources/tart/OCI/Authentication.swift` | Authentication 프로토콜, BasicAuthentication 구조체 |
| `Sources/tart/OCI/AuthenticationKeeper.swift` | actor 기반 인증 상태 관리 (토큰 유효성 검사) |

## 아키텍처

```
┌─────────────────────┐        ┌──────────────────────┐
│   RegistryClient    │        │   RegistryServer     │
│                     │        │                      │
│ PushBlob()          │──POST──▶ handleBlobUploadInit │
│                     │◀─202───│ (Location 헤더)       │
│                     │──PUT───▶ handleBlobUploadComplete
│                     │◀─201───│ (다이제스트 검증)      │
│                     │        │                      │
│ PushManifest()      │──PUT───▶ handleManifest       │
│                     │◀─201───│                      │
│                     │        │                      │
│ PullManifest()      │──GET───▶ handleManifest       │
│                     │◀─200───│                      │
│                     │        │                      │
│ PullBlob()          │──GET───▶ handleBlob           │
│                     │◀─200───│                      │
│                     │        │                      │
│ authenticate()      │──GET───▶ handleToken          │
│ (401 → Bearer)      │◀token──│ (Basic → Bearer)     │
└─────────────────────┘        └──────────────────────┘
```
