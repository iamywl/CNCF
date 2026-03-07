# PoC: Terraform Provider 설치 시뮬레이션

## 개요

Terraform의 프로바이더 설치 과정을 시뮬레이션한다.
`terraform init` 시 프로바이더 레지스트리에서 버전을 해결하고,
다운로드하여 캐시에 저장하는 전체 과정을 재현한다.

## 대응하는 Terraform 소스코드

| 이 PoC | Terraform 소스 | 설명 |
|--------|---------------|------|
| `ProviderRegistry` | `internal/getproviders/registry_client.go` | 레지스트리 API |
| `ProviderCache` | `internal/providercache/dir.go` | 캐시 디렉토리 |
| `ProviderInstaller` | `internal/getproviders/installer.go` | 설치 관리자 |
| `LockFile` | `internal/depsfile/locks.go` | 잠금 파일 |
| `Version`, `Constraint` | Terraform 내부 버전 관리 | SemVer 처리 |
| `ResolveVersion()` | `internal/command/init.go` | 버전 해결 |

## 구현 내용

### 1. SemVer (Semantic Versioning)
- 버전 파싱 (Major.Minor.Patch-Prerelease)
- 버전 비교 (Compare)
- 제약 조건: `=`, `!=`, `>`, `>=`, `<`, `<=`, `~>`

### 2. ~> (패시미스틱 제약) 연산자
- `~> 5.1`: `>= 5.1.0` AND `< 6.0.0` (마이너까지 허용)
- `~> 3.70.0`: `>= 3.70.0` AND `< 3.71.0` (패치만 허용)
- Terraform에서 가장 많이 사용하는 버전 제약 연산자

### 3. 레지스트리
- Mock 프로바이더 레지스트리 (in-memory)
- 버전 목록 조회, 플랫폼 정보 제공
- 실제: registry.terraform.io API

### 4. 캐시 관리
- 다운로드된 프로바이더를 로컬 캐시에 저장
- 두 번째 init 시 캐시에서 사용 (다운로드 생략)
- 디렉토리 구조: `.terraform/providers/registry.terraform.io/{ns}/{type}/{ver}/{platform}/`

### 5. 잠금 파일
- `.terraform.lock.hcl` 형식으로 고정된 버전과 체크섬 기록
- VCS에 커밋하여 팀 전체가 동일 버전 사용

## 실행 방법

```bash
go run main.go
```

## 프로바이더 설치 흐름

```
required_providers 블록
         │
         ▼
┌─────────────────┐
│  버전 해결       │ → 제약 조건 매칭 → 최신 호환 버전 선택
└─────────────────┘
         │
         ▼
┌─────────────────┐
│  캐시 확인       │ → 이미 다운로드? → 캐시 사용
└─────────────────┘
         │ (캐시 미스)
         ▼
┌─────────────────┐
│  다운로드/설치    │ → 레지스트리에서 다운로드 → 캐시에 저장
└─────────────────┘
         │
         ▼
┌─────────────────┐
│  잠금 파일 갱신   │ → .terraform.lock.hcl 업데이트
└─────────────────┘
```

## 핵심 포인트

- `~>` 연산자를 사용하면 호환 가능한 마이너/패치 업데이트만 허용할 수 있다
- 잠금 파일을 VCS에 커밋하면 팀 전체가 동일한 프로바이더 버전을 사용한다
- 프로바이더 캐시로 불필요한 다운로드를 줄이고 오프라인 작업을 지원한다
