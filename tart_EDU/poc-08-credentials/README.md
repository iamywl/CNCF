# PoC-08: 다중 인증 프로바이더 체인 시뮬레이션

## 개요

tart의 Credentials 서브시스템을 Go 표준 라이브러리만으로 재현한다.
환경변수, Docker config.json, macOS Keychain 세 가지 프로바이더를 체인으로 구성하여
순서대로 시도하고, Basic/Bearer 인증 흐름과 토큰 만료 관리를 시뮬레이션한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.
Keychain은 인메모리 map으로, Docker credential helper는 로그 출력으로 시뮬레이션한다.

## 핵심 시뮬레이션 포인트

### 1. CredentialsProvider 인터페이스 (tart CredentialsProvider.swift)
- `Retrieve(host)`: 호스트에 대한 (user, password) 반환
- `Store(host, user, password)`: 자격증명 저장
- `Name()`: 사용자 친화적 이름

### 2. EnvironmentCredentialsProvider (tart EnvironmentCredentialsProvider.swift)
- `TART_REGISTRY_USERNAME` / `TART_REGISTRY_PASSWORD` 환경변수
- `TART_REGISTRY_HOSTNAME` 필터: 설정 시 해당 호스트만 매칭

### 3. DockerConfigCredentialsProvider (tart DockerConfigCredentialsProvider.swift)
- `~/.docker/config.json` 파싱
- `auths[host].auth`: base64("user:password") 디코딩
- `credHelpers[host]`: 외부 `docker-credential-{helper}` 프로그램 매칭
- 와일드카드 패턴 지원 (tart: Regex 매칭)

### 4. KeychainCredentialsProvider (tart KeychainCredentialsProvider.swift)
- `SecItemCopyMatching`: 키체인 검색 (인메모리 map 시뮬레이션)
- `SecItemAdd`: 새 항목 추가
- `SecItemUpdate`: 기존 항목 갱신
- `SecItemDelete`: 항목 삭제

### 5. 프로바이더 체인 (tart Registry.lookupCredentials)
- 순서: 환경변수 -> Docker config -> Keychain
- 첫 번째 성공한 결과 반환
- 실패한 프로바이더는 경고 출력 후 다음으로 계속

### 6. Authentication + AuthenticationKeeper
- BasicAuthentication: base64 인코딩, 만료 없음
- BearerAuthentication: 토큰 + 만료시간, `isValid()` 검사
- AuthenticationKeeper: actor(mutex) 기반 인증 상태 관리

## tart 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `Sources/tart/Credentials/CredentialsProvider.swift` | CredentialsProvider 프로토콜 정의 |
| `Sources/tart/Credentials/EnvironmentCredentialsProvider.swift` | 환경변수 기반 자격증명 |
| `Sources/tart/Credentials/DockerConfigCredentialsProvider.swift` | Docker config.json 파싱 + credHelper |
| `Sources/tart/Credentials/KeychainCredentialsProvider.swift` | macOS Keychain (Security.framework) |
| `Sources/tart/OCI/Authentication.swift` | Authentication 프로토콜, BasicAuthentication |
| `Sources/tart/OCI/AuthenticationKeeper.swift` | actor 기반 인증 상태 관리 |
| `Sources/tart/OCI/Registry.swift` | lookupCredentials(), auth() 메서드 |

## 아키텍처

```
Registry.lookupCredentials(host)
  |
  +--> [1] EnvironmentCredentialsProvider
  |      TART_REGISTRY_USERNAME + PASSWORD
  |      TART_REGISTRY_HOSTNAME 필터
  |
  +--> [2] DockerConfigCredentialsProvider
  |      ~/.docker/config.json
  |      auths[host].auth (base64)
  |      credHelpers[host] -> docker-credential-{helper}
  |
  +--> [3] KeychainCredentialsProvider
  |      SecItemCopyMatching(kSecAttrServer=host)
  |
  +--> 결과: (user, password) 또는 nil

인증 흐름:
  lookupCredentials() --> Basic/Bearer --> AuthenticationKeeper
                                             |
                                        isValid()?
                                        /         \
                                      yes          no
                                    header()   -> 재인증
```
