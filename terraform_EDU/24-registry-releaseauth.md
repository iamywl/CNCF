# 24. Registry 클라이언트 & 릴리즈 인증 심화

## 목차
1. [개요](#1-개요)
2. [Registry 클라이언트 아키텍처](#2-registry-클라이언트-아키텍처)
3. [서비스 디스커버리](#3-서비스-디스커버리)
4. [모듈 버전 조회](#4-모듈-버전-조회)
5. [모듈 다운로드 위치 해석](#5-모듈-다운로드-위치-해석)
6. [인증 & 자격 증명](#6-인증--자격-증명)
7. [재시도 & 타임아웃](#7-재시도--타임아웃)
8. [에러 분류 체계](#8-에러-분류-체계)
9. [릴리즈 인증 시스템](#9-릴리즈-인증-시스템)
10. [SHA-256 체크섬 검증](#10-sha-256-체크섬-검증)
11. [GPG 서명 검증](#11-gpg-서명-검증)
12. [All Authenticator 패턴](#12-all-authenticator-패턴)
13. [왜(Why) 이렇게 설계했나](#13-왜why-이렇게-설계했나)
14. [PoC 매핑](#14-poc-매핑)

---

## 1. 개요

### Registry 클라이언트

Terraform Registry는 모듈과 프로바이더의 중앙 저장소다. Registry 클라이언트는 레지스트리 API를 통해 모듈 버전을 조회하고 다운로드 위치를 결정한다.

### 릴리즈 인증

릴리즈 인증은 Terraform 바이너리와 프로바이더 아카이브가 HashiCorp에서 정상적으로 배포된 것인지 검증하는 보안 메커니즘이다. SHA-256 체크섬과 GPG 서명 두 단계로 검증한다.

```
소스 경로:
├── internal/registry/
│   ├── client.go              # Client 구조체, API 메서드 (331줄)
│   ├── errors.go              # 에러 타입 정의 (51줄)
│   ├── regsrc/                # 레지스트리 소스 주소 파싱
│   ├── response/              # API 응답 타입 정의
│   └── test/                  # 테스트 헬퍼
├── internal/releaseauth/
│   ├── all.go                 # All 메타 Authenticator (39줄)
│   ├── checksum.go            # SHA-256 체크섬 검증 (60줄)
│   ├── signature.go           # GPG 서명 검증 (185줄)
│   ├── hash.go                # SHA256Hash 타입, 체크섬 파싱 (77줄)
│   └── doc.go                 # 패키지 문서
```

---

## 2. Registry 클라이언트 아키텍처

### Client 구조체

```go
type Client struct {
    client   *retryablehttp.Client  // 재시도 가능한 HTTP 클라이언트
    services *disco.Disco           // 서비스 디스커버리
}
```

### 전체 흐름

```
┌─────────────────────────────────────────────────────────┐
│                  Registry Client                         │
│                                                          │
│  ┌──────────────┐  ┌─────────────┐  ┌────────────────┐ │
│  │ disco.Disco   │  │ retryable   │  │ credentials    │ │
│  │ (서비스 발견) │  │ http.Client │  │ (인증)          │ │
│  └──────┬───────┘  └──────┬──────┘  └───────┬────────┘ │
│         │                 │                  │          │
│         ▼                 ▼                  ▼          │
│  ┌─────────────────────────────────────────────────┐   │
│  │              Registry API 호출                   │   │
│  │  GET /v1/modules/{ns}/{name}/{provider}/versions│   │
│  │  GET /v1/modules/.../download                   │   │
│  └─────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### 클라이언트 생성

```go
func NewClient(services *disco.Disco, client *http.Client) *Client {
    if services == nil {
        services = disco.New()
    }
    if client == nil {
        client = httpclient.New()
        client.Timeout = requestTimeout  // 기본 10초
    }

    retryableClient := retryablehttp.NewClient()
    retryableClient.HTTPClient = client
    retryableClient.RetryMax = discoveryRetry  // 기본 1회
    retryableClient.RequestLogHook = requestLogHook
    retryableClient.ErrorHandler = maxRetryErrorHandler

    services.Transport = retryableClient.HTTPClient.Transport
    services.SetUserAgent(httpclient.TerraformUserAgent(version.String()))

    return &Client{
        client:   retryableClient,
        services: services,
    }
}
```

---

## 3. 서비스 디스커버리

### Discover 메서드

```go
func (c *Client) Discover(host svchost.Hostname, serviceID string) (*url.URL, error) {
    service, err := c.services.DiscoverServiceURL(host, serviceID)
    if err != nil {
        return nil, &ServiceUnreachableError{err}
    }
    if !strings.HasSuffix(service.Path, "/") {
        service.Path += "/"  // 경로 정규화
    }
    return service, nil
}
```

### 서비스 ID

```go
const (
    modulesServiceID   = "modules.v1"
    providersServiceID = "providers.v1"
)
```

### 디스커버리 프로토콜

```
1. GET https://registry.terraform.io/.well-known/terraform.json
2. 응답:
   {
     "modules.v1": "/v1/modules/",
     "providers.v1": "/v1/providers/"
   }
3. 기본 URL에 서비스 경로 결합
   → https://registry.terraform.io/v1/modules/
```

이 프로토콜 덕분에 사설 레지스트리(예: Artifactory, GitLab)도 동일한 디스커버리 엔드포인트를 구현하면 Terraform과 호환된다.

---

## 4. 모듈 버전 조회

### ModuleVersions 메서드

```go
func (c *Client) ModuleVersions(ctx context.Context, module *regsrc.Module) (*response.ModuleVersions, error) {
    host, _ := module.SvcHost()

    // 서비스 디스커버리
    service, _ := c.Discover(host, modulesServiceID)

    // URL 구성: /v1/modules/{namespace}/{name}/{provider}/versions
    p, _ := url.Parse(path.Join(module.Module(), "versions"))
    service = service.ResolveReference(p)

    req, _ := retryablehttp.NewRequest("GET", service.String(), nil)
    req = req.WithContext(ctx)

    // 인증 헤더 추가
    c.addRequestCreds(host, req.Request)
    req.Header.Set(xTerraformVersion, tfVersion)

    resp, _ := c.client.Do(req)

    switch resp.StatusCode {
    case http.StatusOK:
        // 정상
    case http.StatusNotFound:
        return nil, &errModuleNotFound{addr: module}
    default:
        return nil, fmt.Errorf("error looking up module versions: %s", resp.Status)
    }

    var versions response.ModuleVersions
    json.NewDecoder(resp.Body).Decode(&versions)
    return &versions, nil
}
```

### API 호출 예시

```
GET https://registry.terraform.io/v1/modules/hashicorp/consul/aws/versions
Headers:
  X-Terraform-Version: 1.9.0
  Authorization: Bearer <token>  (사설 레지스트리인 경우)

Response:
{
  "modules": [{
    "source": "hashicorp/consul/aws",
    "versions": [
      {"version": "0.11.0"},
      {"version": "0.10.0"},
      {"version": "0.9.0"}
    ]
  }]
}
```

---

## 5. 모듈 다운로드 위치 해석

### ModuleLocation 메서드

```go
func (c *Client) ModuleLocation(ctx context.Context, module *regsrc.Module, version string) (string, error) {
    // URL 구성
    if version == "" {
        p, _ = url.Parse(path.Join(module.Module(), "download"))
    } else {
        p, _ = url.Parse(path.Join(module.Module(), version, "download"))
    }
    download := service.ResolveReference(p)

    // API 호출
    resp, _ := c.client.Do(req)

    // X-Terraform-Get 헤더에서 다운로드 위치 추출
    location := resp.Header.Get(xTerraformGet)

    // 상대 URL 처리
    if strings.HasPrefix(location, "/") ||
       strings.HasPrefix(location, "./") ||
       strings.HasPrefix(location, "../") {
        locationURL, _ := url.Parse(location)
        locationURL = download.ResolveReference(locationURL)
        location = locationURL.String()
    }

    return location, nil
}
```

### X-Terraform-Get 헤더

```
GET /v1/modules/hashicorp/consul/aws/0.11.0/download
Response:
  Status: 204 No Content
  X-Terraform-Get: https://github.com/hashicorp/terraform-aws-consul/archive/v0.11.0.tar.gz
```

**왜 응답 본문이 아닌 헤더인가?** 다운로드 위치는 go-getter 문법을 사용할 수 있으며(예: `git::https://...`, `s3::https://...`), 이는 URL이 아닌 "소스 지정자"다. 헤더에 넣으면 본문 파싱 없이 즉시 리다이렉트할 수 있다.

### 상대 URL 지원

코드 주석에 명시:

> We are more liberal here because third-party registry implementations may
> not "know" their own absolute URLs if e.g. they are running behind a
> reverse proxy frontend.

리버스 프록시 뒤에서 실행되는 사설 레지스트리는 자신의 외부 URL을 모를 수 있다. 상대 URL을 허용하면 이런 환경에서도 동작한다.

---

## 6. 인증 & 자격 증명

### addRequestCreds

```go
func (c *Client) addRequestCreds(host svchost.Hostname, req *http.Request) {
    creds, err := c.services.CredentialsForHost(host)
    if err != nil {
        log.Printf("[WARN] Failed to get credentials for %s: %s (ignoring)", host, err)
        return
    }
    if creds != nil {
        creds.PrepareRequest(req)  // Authorization 헤더 설정
    }
}
```

자격 증명 소스:
- `~/.terraformrc` 또는 `terraform.rc`의 `credentials` 블록
- 환경 변수 `TF_TOKEN_<hostname>`
- 자격 증명 헬퍼 (external program)

공개 레지스트리(registry.terraform.io)는 인증 없이 접근 가능하지만, 사설 레지스트리는 토큰이 필요하다.

---

## 7. 재시도 & 타임아웃

### 환경 변수 설정

```go
const (
    registryDiscoveryRetryEnvName  = "TF_REGISTRY_DISCOVERY_RETRY"
    registryClientTimeoutEnvName   = "TF_REGISTRY_CLIENT_TIMEOUT"
    defaultRetry                   = 1
    defaultRequestTimeout          = 10 * time.Second
)

func configureDiscoveryRetry() {
    discoveryRetry = defaultRetry
    if v := os.Getenv(registryDiscoveryRetryEnvName); v != "" {
        retry, err := strconv.Atoi(v)
        if err == nil && retry > 0 {
            discoveryRetry = retry
        }
    }
}
```

### 재시도 실패 핸들러

```go
func maxRetryErrorHandler(resp *http.Response, err error, numTries int) (*http.Response, error) {
    if resp != nil { resp.Body.Close() }

    if numTries > 1 {
        return resp, fmt.Errorf("the request failed after %d attempts, please try again later%s",
            numTries, errMsg)
    }
    return resp, fmt.Errorf("the request failed, please try again later%s", errMsg)
}
```

---

## 8. 에러 분류 체계

### `internal/registry/errors.go`

```go
// 모듈을 찾을 수 없음
type errModuleNotFound struct {
    addr *regsrc.Module
}

// 레지스트리 서비스 도달 불가
type ServiceUnreachableError struct {
    err error
}
```

### 에러 분류 함수

```go
func IsModuleNotFound(err error) bool {
    _, ok := err.(*errModuleNotFound)
    return ok
}

func IsServiceNotProvided(err error) bool {
    _, ok := err.(*disco.ErrServiceNotProvided)
    return ok
}

func IsServiceUnreachable(err error) bool {
    _, ok := err.(*ServiceUnreachableError)
    return ok
}
```

### 왜 에러를 분류하는가?

호출자가 에러의 종류에 따라 다른 행동을 취해야 하기 때문이다:

| 에러 타입 | 호출자 행동 |
|----------|-----------|
| ModuleNotFound | "모듈이 존재하지 않습니다" 사용자 에러 표시 |
| ServiceNotProvided | "이 호스트는 레지스트리를 지원하지 않습니다" 표시 |
| ServiceUnreachable | "네트워크 연결을 확인하세요" 표시 + 재시도 권유 |

---

## 9. 릴리즈 인증 시스템

### Authenticator 인터페이스

```go
type Authenticator interface {
    Authenticate() error
}
```

### 인증 체인

```
┌─────────────────────────────────────────┐
│           All Authenticator              │
│                                          │
│  ┌──────────────────────────────────┐   │
│  │ 1. SignatureAuthentication       │   │
│  │    SHA256SUMS.sig + SHA256SUMS   │   │
│  │    → GPG 서명 검증               │   │
│  └──────────────────────────────────┘   │
│              │ 통과                      │
│              ▼                           │
│  ┌──────────────────────────────────┐   │
│  │ 2. ChecksumAuthentication       │   │
│  │    SHA256SUMS + 실제 파일        │   │
│  │    → SHA-256 해시 일치 검증      │   │
│  └──────────────────────────────────┘   │
│              │ 통과                      │
│              ▼                           │
│         인증 성공                        │
└─────────────────────────────────────────┘
```

---

## 10. SHA-256 체크섬 검증

### SHA256Hash 타입

```go
type SHA256Hash [sha256.Size]byte  // 32바이트 고정 크기 배열

func SHA256FromHex(hashHex string) (SHA256Hash, error) {
    var result [sha256.Size]byte
    hash, err := hex.DecodeString(hashHex)
    if err != nil || len(hash) != sha256.Size {
        return result, ErrInvalidSHA256Hash
    }
    copy(result[:], hash)
    return result, nil
}
```

### 체크섬 파일 파싱

```go
// SHA256SUMS 파일 형식:
// e3b0c44298fc1c14...  terraform_1.9.0_linux_amd64.zip
// a7ffc6f8bf1ed766...  terraform_1.9.0_darwin_arm64.zip

type SHA256Checksums map[string]SHA256Hash

func ParseChecksums(data []byte) (SHA256Checksums, error) {
    items := bytes.Split(data, []byte("\n"))
    result := make(map[string]SHA256Hash, len(items))

    for _, line := range items {
        parts := bytes.SplitN(line, []byte("  "), 2)  // 2칸 공백 구분
        if len(parts) != 2 { break }

        hash, _ := SHA256FromHex(string(parts[0]))
        result[string(parts[1])] = hash
    }
    return result, nil
}
```

### ChecksumAuthentication

```go
func (a ChecksumAuthentication) Authenticate() error {
    f, _ := os.Open(a.archiveLocation)
    defer f.Close()

    h := sha256.New()
    io.Copy(h, f)

    gotHash := h.Sum(nil)
    if !bytes.Equal(gotHash, a.expected[:]) {
        return ErrChecksumDoesNotMatch
    }
    return nil
}
```

---

## 11. GPG 서명 검증

### SignatureAuthentication

```go
type SignatureAuthentication struct {
    Authenticator
    PublicKey string      // HashiCorp GPG 공개키
    signature []byte      // SHA256SUMS.sig 파일 내용
    signed    []byte      // SHA256SUMS 파일 내용
}

func (a SignatureAuthentication) Authenticate() error {
    // HashiCorp 공개키로 키링 생성
    hashicorpKeyring, _ := openpgp.ReadArmoredKeyRing(
        strings.NewReader(a.PublicKey))

    // 분리 서명 검증
    _, err = openpgp.CheckDetachedSignature(
        hashicorpKeyring,
        bytes.NewReader(a.signed),
        bytes.NewReader(a.signature),
        nil)

    if err != nil {
        return ErrNotSignedByHashiCorp
    }
    return nil
}
```

### 검증 흐름

```
1. SHA256SUMS.sig (서명 파일) 다운로드
2. SHA256SUMS (체크섬 파일) 다운로드
3. HashiCorp GPG 공개키로 서명 검증
   ├── 성공 → SHA256SUMS가 HashiCorp가 작성한 것 확인
   └── 실패 → ErrNotSignedByHashiCorp
4. 다운로드한 아카이브의 SHA-256 해시 계산
5. SHA256SUMS의 해시와 비교
   ├── 일치 → 아카이브 무결성 확인
   └── 불일치 → ErrChecksumDoesNotMatch
```

### HashiCorp 공개키

```go
const HashiCorpPublicKeyID = "72D7468F"
const HashiCorpPublicKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----
// ... 4096-bit RSA 공개키
-----END PGP PUBLIC KEY BLOCK-----`
```

이 키는 https://www.hashicorp.com/security 에서도 확인 가능하다. 코드에 하드코딩되어 있어 TOFU(Trust On First Use) 문제를 방지한다.

---

## 12. All Authenticator 패턴

### 메타 Authenticator

```go
type All struct {
    Authenticator
    authenticators []Authenticator
}

func AllAuthenticators(authenticators ...Authenticator) All {
    return All{authenticators: authenticators}
}

func (a All) Authenticate() error {
    for _, auth := range a.authenticators {
        if err := auth.Authenticate(); err != nil {
            return err  // 첫 번째 실패 즉시 반환
        }
    }
    return nil
}
```

### 사용 패턴

```go
auth := AllAuthenticators(
    NewSignatureAuthentication(sigData, checksumData),
    NewChecksumAuthentication(expectedHash, "/path/to/archive.zip"),
)
if err := auth.Authenticate(); err != nil {
    // 인증 실패
}
```

**왜 순서가 중요한가?**

1. **서명 검증이 먼저**: SHA256SUMS 파일 자체의 진위를 확인
2. **체크섬 검증이 후**: 검증된 SHA256SUMS로 아카이브 무결성 확인

이 순서가 역전되면, 공격자가 조작한 SHA256SUMS로 조작된 아카이브를 검증할 수 있다.

---

## 13. 왜(Why) 이렇게 설계했나

### Q1: 왜 디스커버리 프로토콜이 필요한가?

레지스트리 API의 기본 경로가 호스트마다 다를 수 있기 때문이다:
- registry.terraform.io → `/v1/modules/`
- Artifactory → `/artifactory/api/terraform/v1/modules/`
- GitLab → `/-/terraform/modules/v1/`

`.well-known/terraform.json`으로 이를 통일한다.

### Q2: 왜 체크섬과 서명을 분리했는가?

- **체크섬**: 전송 중 변조/손상 감지 (무결성)
- **서명**: 배포 주체 확인 (인증)

두 가지가 별도로 필요한 이유는 각각 다른 위협에 대응하기 때문이다:
- 체크섬만 있으면: 공격자가 체크섬 파일도 같이 변조 가능
- 서명만 있으면: 전송 중 비트 에러를 감지할 수 없음

### Q3: 왜 X-Terraform-Version 헤더를 보내는가?

```go
req.Header.Set(xTerraformVersion, tfVersion)
```

레지스트리가 Terraform 버전에 따라 응답을 분기할 수 있게 한다. 예를 들어:
- 호환되지 않는 모듈 버전 필터링
- 버전별 사용 통계 수집
- 사용 중단(deprecation) 경고 전달

### Q4: 왜 GPG 키가 코드에 하드코딩되어 있는가?

```go
const HashiCorpPublicKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----...`
```

외부에서 키를 가져오면 키 서버가 공격당했을 때 조작된 키로 서명 검증이 성공할 수 있다. 하드코딩하면:
- Terraform 바이너리 자체의 무결성에 의존 (이미 신뢰해야 하는 대상)
- 네트워크 요청 없이 오프라인 검증 가능
- 키 서버 장애 시에도 검증 가능

---

## 14. PoC 매핑

| PoC | 시뮬레이션 대상 |
|-----|---------------|
| poc-24-registry | 서비스 디스커버리, 모듈 버전 조회, 재시도 |
| poc-25-releaseauth | SHA-256 체크섬, 서명 검증 체인, All 패턴 |

---

## 참조 소스 파일

| 파일 | 줄수 | 핵심 내용 |
|------|------|----------|
| `internal/registry/client.go` | 331 | Client, Discover, ModuleVersions, ModuleLocation |
| `internal/registry/errors.go` | 51 | errModuleNotFound, ServiceUnreachableError |
| `internal/releaseauth/all.go` | 39 | All Authenticator, 체인 패턴 |
| `internal/releaseauth/checksum.go` | 60 | ChecksumAuthentication, SHA-256 해시 검증 |
| `internal/releaseauth/signature.go` | 185 | SignatureAuthentication, GPG 서명 검증 |
| `internal/releaseauth/hash.go` | 77 | SHA256Hash, ParseChecksums, SHA256SUMS 파싱 |
