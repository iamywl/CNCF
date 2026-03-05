# 08. OCI 레지스트리 프로토콜 심화

## 목차

1. [개요](#1-개요)
2. [Registry 클래스 아키텍처](#2-registry-클래스-아키텍처)
3. [HTTP 메서드: Pull/Push Manifest와 Blob](#3-http-메서드-pullpush-manifest와-blob)
4. [인증 흐름: HTTP 401 → 토큰 획득](#4-인증-흐름-http-401--토큰-획득)
5. [TokenResponse: Bearer 토큰 관리](#5-tokenresponse-bearer-토큰-관리)
6. [AuthenticationKeeper: Actor 기반 토큰 캐시](#6-authenticationkeeper-actor-기반-토큰-캐시)
7. [WWW-Authenticate 헤더 파싱](#7-www-authenticate-헤더-파싱)
8. [CredentialsProvider: 자격 증명 체계](#8-credentialsprovider-자격-증명-체계)
9. [청크 업로드 vs 모놀리식 업로드](#9-청크-업로드-vs-모놀리식-업로드)
10. [Range 기반 재개 가능 Pull](#10-range-기반-재개-가능-pull)
11. [Fetcher 클래스: URLSession과 AsyncThrowingStream](#11-fetcher-클래스-urlsession과-asyncthrowingstream)
12. [OCIManifest 구조](#12-ocimanifest-구조)
13. [커스텀 미디어 타입](#13-커스텀-미디어-타입)
14. [RemoteName 파싱](#14-remotename-파싱)
15. [VM Push/Pull 전체 흐름](#15-vm-pushpull-전체-흐름)
16. [Digest: SHA256 해시 계산](#16-digest-sha256-해시-계산)
17. [설계 결정과 트레이드오프](#17-설계-결정과-트레이드오프)

---

## 1. 개요

Tart는 VM 이미지를 OCI(Open Container Initiative) 레지스트리에 저장하고 배포한다. Docker Hub, GitHub Container Registry(GHCR), AWS ECR 등 OCI 호환 레지스트리를 모두 지원한다. OCI 레지스트리 프로토콜 관련 코드는 `Sources/tart/OCI/` 디렉토리에 집중되어 있다.

```
Sources/tart/OCI/
├── Authentication.swift          # Authentication 프로토콜, BasicAuthentication
├── AuthenticationKeeper.swift    # Actor 기반 토큰 캐시
├── Digest.swift                  # SHA256 해시 유틸리티
├── Layerizer/                    # 디스크 레이어 분할/병합 (DiskV2)
├── Manifest.swift                # OCIManifest, OCIManifestLayer, OCIConfig
├── Reference/                    # Antlr4 기반 참조 파서 (태그/다이제스트)
├── Registry.swift                # 핵심: OCI 레지스트리 HTTP 클라이언트
├── RemoteName.swift              # host/namespace:tag 파싱
├── URL+Absolutize.swift          # 상대 URL → 절대 URL 변환
└── WWWAuthenticate.swift         # WWW-Authenticate 헤더 파서
```

### OCI Distribution Spec 기반 아키텍처

```
+-----------------------------------------------------------+
|                     Tart CLI                               |
|  tart pull / tart push                                     |
+-----------------------------------------------------------+
           |
           v
+-----------------------------------------------------------+
|              VMDirectory+OCI.swift                         |
|  pullFromRegistry() / pushToRegistry()                     |
|  레이어별 Pull/Push 조율                                   |
+-----------------------------------------------------------+
           |
           v
+-----------------------------------------------------------+
|              Registry.swift                                 |
|  pullManifest / pushManifest / pullBlob / pushBlob         |
|  인증 / 청크 업로드 / 재개 다운로드                         |
+-----------------------------------------------------------+
           |
           v
+-----------------------------------------------------------+
|              Fetcher.swift                                  |
|  URLSession + AsyncThrowingStream                          |
|  16MB 버퍼, 쿠키 비활성화                                   |
+-----------------------------------------------------------+
           |
           v
+-----------------------------------------------------------+
|         OCI Distribution Specification v2                   |
|  /v2/{name}/manifests/{reference}                          |
|  /v2/{name}/blobs/{digest}                                 |
|  /v2/{name}/blobs/uploads/                                 |
+-----------------------------------------------------------+
```

---

## 2. Registry 클래스 아키텍처

**파일 경로**: `Sources/tart/OCI/Registry.swift` 113~158행

```swift
class Registry {
  private let baseURL: URL
  let namespace: String
  let credentialsProviders: [CredentialsProvider]
  let authenticationKeeper = AuthenticationKeeper()

  var host: String? {
    guard let host = baseURL.host else { return nil }
    if let port = baseURL.port {
      return "\(host):\(port)"
    }
    return host
  }

  init(baseURL: URL, namespace: String,
       credentialsProviders: [CredentialsProvider] = [
         EnvironmentCredentialsProvider(),
         DockerConfigCredentialsProvider(),
         KeychainCredentialsProvider()
       ]) throws {
    self.baseURL = baseURL
    self.namespace = namespace
    self.credentialsProviders = credentialsProviders
  }
}
```

### 핵심 프로퍼티

| 프로퍼티 | 타입 | 역할 |
|---------|------|------|
| `baseURL` | `URL` | 레지스트리 기본 URL (예: `https://ghcr.io/v2/`) |
| `namespace` | `String` | 이미지 네임스페이스 (예: `cirruslabs/macos-ventura-vanilla`) |
| `credentialsProviders` | `[CredentialsProvider]` | 인증 정보 제공자 체인 |
| `authenticationKeeper` | `AuthenticationKeeper` | 토큰 캐시 (actor) |

### 편의 생성자

```swift
convenience init(host: String, namespace: String, insecure: Bool = false,
                 credentialsProviders: [CredentialsProvider] = [...]) throws {
  let proto = insecure ? "http" : "https"
  let baseURLComponents = URLComponents(string: proto + "://" + host + "/v2/")!
  ...
  try self.init(baseURL: baseURL, namespace: namespace, credentialsProviders: credentialsProviders)
}
```

`insecure` 플래그가 `true`이면 HTTP를 사용하고, 아니면 HTTPS를 사용한다. URL은 항상 `/v2/` 경로를 포함한다 (OCI Distribution Spec의 기본 경로).

---

## 3. HTTP 메서드: Pull/Push Manifest와 Blob

### pullManifest

**파일 경로**: `Sources/tart/OCI/Registry.swift` 181~192행

```swift
public func pullManifest(reference: String) async throws -> (OCIManifest, Data) {
  let (data, response) = try await dataRequest(.GET,
    endpointURL("\(namespace)/manifests/\(reference)"),
    headers: ["Accept": ociManifestMediaType])
  if response.statusCode != HTTPCode.Ok.rawValue {
    throw RegistryError.UnexpectedHTTPStatusCode(when: "pulling manifest",
      code: response.statusCode, details: data.asTextPreview())
  }
  let manifest = try OCIManifest(fromJSON: data)
  return (manifest, data)
}
```

- `GET /v2/{namespace}/manifests/{reference}` 호출
- `Accept: application/vnd.oci.image.manifest.v1+json` 헤더 필수
- reference는 태그(예: `latest`) 또는 다이제스트(예: `sha256:abc...`)
- 파싱된 `OCIManifest`와 원본 `Data`를 모두 반환 (다이제스트 계산에 원본 필요)

### pushManifest

**파일 경로**: `Sources/tart/OCI/Registry.swift` 167~179행

```swift
func pushManifest(reference: String, manifest: OCIManifest) async throws -> String {
  let manifestJSON = try manifest.toJSON()
  let (data, response) = try await dataRequest(.PUT,
    endpointURL("\(namespace)/manifests/\(reference)"),
    headers: ["Content-Type": manifest.mediaType],
    body: manifestJSON)
  if response.statusCode != HTTPCode.Created.rawValue {
    throw RegistryError.UnexpectedHTTPStatusCode(when: "pushing manifest",
      code: response.statusCode, details: data.asTextPreview())
  }
  return Digest.hash(manifestJSON)
}
```

- `PUT /v2/{namespace}/manifests/{reference}` 호출
- Content-Type에 매니페스트 미디어 타입 지정
- HTTP 201 Created 응답 기대
- 매니페스트 JSON의 SHA256 다이제스트를 반환

### pushBlob

**파일 경로**: `Sources/tart/OCI/Registry.swift` 206~264행

```swift
public func pushBlob(fromData: Data, chunkSizeMb: Int = 0, digest: String? = nil) async throws -> String {
  // 1. POST /v2/{namespace}/blobs/uploads/ → HTTP 202 Accepted
  let (data, postResponse) = try await dataRequest(.POST,
    endpointURL("\(namespace)/blobs/uploads/"),
    headers: ["Content-Length": "0"])

  // 2. Location 헤더에서 업로드 URL 추출
  var uploadLocation = try uploadLocationFromResponse(postResponse)
  let digest = digest ?? Digest.hash(fromData)

  if chunkSizeMb == 0 {
    // 모놀리식 업로드: PUT
    ...
  }

  // 청크 업로드: PATCH + 마지막 PUT
  ...
}
```

### pullBlob

**파일 경로**: `Sources/tart/OCI/Registry.swift` 279~304행

```swift
public func pullBlob(_ digest: String, rangeStart: Int64 = 0,
                     handler: (Data) async throws -> Void) async throws {
  var expectedStatusCode = HTTPCode.Ok
  var headers: [String: String] = [:]

  if rangeStart != 0 {
    expectedStatusCode = HTTPCode.PartialContent
    headers["Range"] = "bytes=\(rangeStart)-"
  }

  let (channel, response) = try await channelRequest(.GET,
    endpointURL("\(namespace)/blobs/\(digest)"), headers: headers, viaFile: true)
  ...
  for try await part in channel {
    try Task.checkCancellation()
    try await handler(part)
  }
}
```

### blobExists

**파일 경로**: `Sources/tart/OCI/Registry.swift` 266~277행

```swift
public func blobExists(_ digest: String) async throws -> Bool {
  let (data, response) = try await dataRequest(.HEAD,
    endpointURL("\(namespace)/blobs/\(digest)"))
  switch response.statusCode {
  case HTTPCode.Ok.rawValue: return true
  case HTTPCode.NotFound.rawValue: return false
  default: throw RegistryError.UnexpectedHTTPStatusCode(...)
  }
}
```

`HEAD` 요청으로 blob의 존재 여부만 확인한다. 중복 업로드를 방지하기 위해 push 전에 호출한다.

### HTTP 메서드 요약

```
OCI Distribution API 엔드포인트 매핑
=====================================

Pull Manifest   GET    /v2/{namespace}/manifests/{reference}
Push Manifest   PUT    /v2/{namespace}/manifests/{reference}
Pull Blob       GET    /v2/{namespace}/blobs/{digest}
Blob Exists     HEAD   /v2/{namespace}/blobs/{digest}
Start Upload    POST   /v2/{namespace}/blobs/uploads/
Upload Chunk    PATCH  {upload-location}
Finish Upload   PUT    {upload-location}?digest={digest}
Ping            GET    /v2/
```

---

## 4. 인증 흐름: HTTP 401 -> 토큰 획득

**파일 경로**: `Sources/tart/OCI/Registry.swift` 326~362행

Tart의 인증은 "lazy" 방식이다. 처음에는 인증 없이 요청하고, HTTP 401을 받으면 인증을 수행한다.

```
OCI 레지스트리 인증 흐름
=========================

1. 클라이언트 ──→ GET /v2/{namespace}/manifests/{tag}
                   (인증 헤더 없음)

2. 서버 ──→ HTTP 401 Unauthorized
            WWW-Authenticate: Bearer realm="https://auth.example.com/token",
                              service="registry.example.com",
                              scope="repository:org/repo:pull"

3. 클라이언트가 WWW-Authenticate 파싱
   ├─ scheme = "Bearer"
   ├─ realm  = "https://auth.example.com/token"
   ├─ service = "registry.example.com"
   └─ scope  = "repository:org/repo:pull"

4. 클라이언트 ──→ GET https://auth.example.com/token
                   ?service=registry.example.com
                   &scope=repository:org/repo:pull
                   Authorization: Basic base64(user:password)

5. 인증 서버 ──→ HTTP 200
                  {"token": "eyJ...", "expires_in": 300}

6. 클라이언트 ──→ GET /v2/{namespace}/manifests/{tag}
                   Authorization: Bearer eyJ...

7. 서버 ──→ HTTP 200 (매니페스트 데이터)
```

### channelRequest 구현

```swift
private func channelRequest(
  _ method: HTTPMethod,
  _ urlComponents: URLComponents,
  headers: Dictionary<String, String> = Dictionary(),
  parameters: Dictionary<String, String> = Dictionary(),
  body: Data? = nil,
  doAuth: Bool = true,
  viaFile: Bool = false
) async throws -> (AsyncThrowingStream<Data, Error>, HTTPURLResponse) {
  ...
  var (channel, response) = try await authAwareRequest(request: request, viaFile: viaFile, doAuth: doAuth)

  if doAuth && response.statusCode == HTTPCode.Unauthorized.rawValue {
    try await auth(response: response)
    (channel, response) = try await authAwareRequest(request: request, viaFile: viaFile, doAuth: doAuth)
  }

  return (channel, response)
}
```

핵심 로직:
1. 첫 번째 요청에 기존 인증 정보가 있으면 포함한다 (`authAwareRequest`)
2. 401이 반환되면 `auth()` 메서드로 새 토큰을 획득한다
3. 동일 요청을 새 토큰으로 재시도한다

### auth 메서드

**파일 경로**: `Sources/tart/OCI/Registry.swift` 364~422행

```swift
private func auth(response: HTTPURLResponse) async throws {
  guard let wwwAuthenticateRaw = response.value(forHTTPHeaderField: "WWW-Authenticate") else {
    throw RegistryError.AuthFailed(why: "got HTTP 401, but WWW-Authenticate header is missing")
  }

  let wwwAuthenticate = try WWWAuthenticate(rawHeaderValue: wwwAuthenticateRaw)

  if wwwAuthenticate.scheme.lowercased() == "basic" {
    if let (user, password) = try lookupCredentials() {
      await authenticationKeeper.set(BasicAuthentication(user: user, password: password))
    }
    return
  }

  if wwwAuthenticate.scheme.lowercased() != "bearer" {
    throw RegistryError.AuthFailed(why: "... unsupported, expected \"Bearer\" scheme")
  }

  guard let realm = wwwAuthenticate.kvs["realm"] else {
    throw RegistryError.AuthFailed(why: "... missing a \"realm\" directive")
  }

  // Token 요청
  guard var authenticateURL = URLComponents(string: realm) else { ... }

  authenticateURL.queryItems = ["scope", "service"].compactMap { key in
    if let value = wwwAuthenticate.kvs[key] {
      return URLQueryItem(name: key, value: value)
    } else { return nil }
  }

  var headers: Dictionary<String, String> = Dictionary()
  if let (user, password) = try lookupCredentials() {
    let encodedCredentials = "\(user):\(password)".data(using: .utf8)?.base64EncodedString()
    headers["Authorization"] = "Basic \(encodedCredentials!)"
  }

  let (data, response) = try await dataRequest(.GET, authenticateURL, headers: headers, doAuth: false)
  ...
  await authenticationKeeper.set(try TokenResponse.parse(fromData: data))
}
```

인증 방식 분기:

```
WWW-Authenticate 스킴 확인
        |
+-------+--------+
| Basic           | Bearer
v                 v
lookupCredentials()    realm URL 추출
    |                    |
    v                    v
BasicAuthentication  토큰 서버에 GET 요청
저장                 (scope, service 파라미터)
                        |
                        v
                   TokenResponse 파싱
                   authenticationKeeper에 저장
```

---

## 5. TokenResponse: Bearer 토큰 관리

**파일 경로**: `Sources/tart/OCI/Registry.swift` 58~111행

```swift
struct TokenResponse: Decodable, Authentication {
  var token: String?
  var accessToken: String?
  var expiresIn: Int?
  var issuedAt: Date?

  static func parse(fromData: Data) throws -> Self {
    let decoder = Config.jsonDecoder()
    decoder.keyDecodingStrategy = .convertFromSnakeCase
    ...
    var response = try decoder.decode(TokenResponse.self, from: fromData)
    response.issuedAt = response.issuedAt ?? Date()
    guard response.token != nil || response.accessToken != nil else {
      throw DecodingError.keyNotFound(...)
    }
    return response
  }

  var tokenExpiresAt: Date {
    get {
      (issuedAt ?? Date()) + TimeInterval(expiresIn ?? 60)
    }
  }

  func header() -> (String, String) {
    return ("Authorization", "Bearer \(token ?? accessToken ?? "")")
  }

  func isValid() -> Bool {
    Date() < tokenExpiresAt
  }
}
```

### 토큰 필드 설명

| 필드 | 설명 |
|------|------|
| `token` | Docker Registry v2 표준 토큰 필드 |
| `accessToken` | 대체 토큰 필드 (일부 레지스트리 호환성) |
| `expiresIn` | 토큰 만료까지 남은 시간(초), 기본 60초 |
| `issuedAt` | 토큰 발급 시각, 없으면 현재 시각 사용 |
| `tokenExpiresAt` | 계산 프로퍼티: `issuedAt + expiresIn` |

### 토큰 유효성 검사

```
토큰 생명주기
=============

발급 시각                     만료 시각
(issuedAt)                    (tokenExpiresAt)
   |<------- expiresIn -------->|
   |           (초)              |
   |                            |
   ├── isValid() = true ────────┤── isValid() = false
   |                            |
   현재 시각 < tokenExpiresAt     현재 시각 >= tokenExpiresAt
```

`expiresIn`이 없으면 Docker Token Authentication Specification에 따라 60초를 기본값으로 사용한다. 이는 호환성을 위한 보수적인 기본값이다.

---

## 6. AuthenticationKeeper: Actor 기반 토큰 캐시

**파일 경로**: `Sources/tart/OCI/AuthenticationKeeper.swift`

```swift
actor AuthenticationKeeper {
  var authentication: Authentication? = nil

  func set(_ authentication: Authentication) {
    self.authentication = authentication
  }

  func header() -> (String, String)? {
    if let authentication = authentication {
      if !authentication.isValid() {
        return nil
      }
      return authentication.header()
    }
    return nil
  }
}
```

### 왜 Actor인가?

`AuthenticationKeeper`는 Swift의 `actor` 타입으로 선언되어 있다. 이유:

1. **동시성 안전성**: OCI 레지스트리와의 통신은 비동기적으로 이루어지며, 여러 Task가 동시에 인증 토큰에 접근할 수 있다
2. **데이터 레이스 방지**: `authentication` 프로퍼티의 읽기/쓰기가 actor 격리(isolation)를 통해 직렬화된다
3. **자동 동기화**: `set()`과 `header()` 호출이 actor의 직렬 실행기(serial executor)를 통해 순서대로 실행된다

### Authentication 프로토콜

**파일 경로**: `Sources/tart/OCI/Authentication.swift`

```swift
protocol Authentication {
  func header() -> (String, String)
  func isValid() -> Bool
}

struct BasicAuthentication: Authentication {
  let user: String
  let password: String

  func header() -> (String, String) {
    let creds = Data("\(user):\(password)".utf8).base64EncodedString()
    return ("Authorization", "Basic \(creds)")
  }

  func isValid() -> Bool {
    true  // Basic 인증은 만료되지 않는다
  }
}
```

두 가지 구현체:
- `BasicAuthentication`: Basic 인증, 항상 유효
- `TokenResponse`: Bearer 토큰, 시간 기반 만료

---

## 7. WWW-Authenticate 헤더 파싱

**파일 경로**: `Sources/tart/OCI/WWWAuthenticate.swift`

```swift
class WWWAuthenticate {
  var scheme: String
  var kvs: Dictionary<String, String> = Dictionary()

  init(rawHeaderValue: String) throws {
    let splits = rawHeaderValue.split(separator: " ", maxSplits: 1)
    if splits.count == 2 {
      scheme = String(splits[0])
    } else {
      throw RegistryError.MalformedHeader(...)
    }

    let rawDirectives = contextAwareCommaSplit(rawDirectives: String(splits[1]))

    try rawDirectives.forEach { sequence in
      let parts = sequence.split(separator: "=", maxSplits: 1)
      ...
      let key = String(parts[0])
      var value = String(parts[1])
      value = value.trimmingCharacters(in: CharacterSet(charactersIn: "\""))
      kvs[key] = value
    }
  }
}
```

### 파싱 로직

입력 예시:
```
Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/ubuntu:pull"
```

파싱 결과:
```
scheme = "Bearer"
kvs = {
  "realm":   "https://auth.docker.io/token"
  "service": "registry.docker.io"
  "scope":   "repository:library/ubuntu:pull"
}
```

### contextAwareCommaSplit

이 헬퍼 함수는 쉼표로 구분된 디렉티브를 분할하되, 따옴표 안의 쉼표는 무시한다:

```swift
private func contextAwareCommaSplit(rawDirectives: String) -> Array<String> {
  var result: Array<String> = Array()
  var inQuotation: Bool = false
  var accumulator: Array<Character> = Array()

  for ch in rawDirectives {
    if ch == "," && !inQuotation {
      result.append(String(accumulator))
      accumulator.removeAll()
      continue
    }
    accumulator.append(ch)
    if ch == "\"" {
      inQuotation.toggle()
    }
  }
  if !accumulator.isEmpty {
    result.append(String(accumulator))
  }
  return result
}
```

```
입력: realm="https://auth.example.com/token",service="svc,name",scope="repo:pull"

처리:
  "realm=\"https://auth.example.com/token\""  ← 쉼표에서 분할
  "service=\"svc,name\""                       ← 따옴표 안 쉼표 보존
  "scope=\"repo:pull\""                        ← 쉼표에서 분할
```

RFC 2617과 RFC 6850을 기반으로 구현되었으며, 소스코드 주석에 해당 RFC 참조가 명시되어 있다.

---

## 8. CredentialsProvider: 자격 증명 체계

**파일 경로**: `Sources/tart/Credentials/CredentialsProvider.swift`

```swift
protocol CredentialsProvider {
  var userFriendlyName: String { get }
  func retrieve(host: String) throws -> (String, String)?
  func store(host: String, user: String, password: String) throws
}
```

### 자격 증명 검색 우선순위

**파일 경로**: `Sources/tart/OCI/Registry.swift` 424~441행

```swift
private func lookupCredentials() throws -> (String, String)? {
  var host = baseURL.host!
  if let port = baseURL.port {
    host += ":\(port)"
  }

  for provider in credentialsProviders {
    do {
      if let (user, password) = try provider.retrieve(host: host) {
        return (user, password)
      }
    } catch (let e) {
      print("Failed to retrieve credentials using \(provider.userFriendlyName), ...")
    }
  }
  return nil
}
```

검색 순서 (기본값):

```
1. EnvironmentCredentialsProvider
   └─ TART_REGISTRY_USERNAME / TART_REGISTRY_PASSWORD 환경변수

2. DockerConfigCredentialsProvider
   └─ ~/.docker/config.json 파일의 auths 섹션

3. KeychainCredentialsProvider
   └─ macOS Keychain에서 host 기반 검색
```

각 Provider를 순서대로 시도하며, 첫 번째로 자격 증명을 반환하는 Provider의 결과를 사용한다. 에러가 발생하면 경고를 출력하고 다음 Provider로 넘어간다.

---

## 9. 청크 업로드 vs 모놀리식 업로드

**파일 경로**: `Sources/tart/OCI/Registry.swift` 206~264행

### 모놀리식 업로드 (chunkSizeMb == 0)

```swift
if chunkSizeMb == 0 {
  let (data, response) = try await dataRequest(
    .PUT, uploadLocation,
    headers: ["Content-Type": "application/octet-stream"],
    parameters: ["digest": digest],
    body: fromData)
  if response.statusCode != HTTPCode.Created.rawValue { ... }
  return digest
}
```

```
모놀리식 업로드 흐름
====================

Client                          Registry
  |                                |
  |── POST /blobs/uploads/ ──────→|
  |←──── 202 Accepted ───────────|
  |     Location: /upload/uuid    |
  |                                |
  |── PUT /upload/uuid ──────────→|
  |   ?digest=sha256:abc...       |
  |   Content-Type: octet-stream  |
  |   Body: [전체 데이터]          |
  |←──── 201 Created ────────────|
```

### 청크 업로드 (chunkSizeMb > 0)

```swift
var uploadedBytes = 0
let chunks = fromData.chunks(ofCount: chunkSizeMb * 1_000_000)
for (index, chunk) in chunks.enumerated() {
  let lastChunk = index == (chunks.count - 1)
  let (data, response) = try await dataRequest(
    lastChunk ? .PUT : .PATCH,
    uploadLocation,
    headers: [
      "Content-Type": "application/octet-stream",
      "Content-Range": "\(uploadedBytes)-\(uploadedBytes + chunk.count - 1)",
    ],
    parameters: lastChunk ? ["digest": digest] : [:],
    body: chunk)
  ...
  uploadedBytes += chunk.count
  uploadLocation = try uploadLocationFromResponse(response)
}
```

```
청크 업로드 흐름
================

Client                          Registry
  |                                |
  |── POST /blobs/uploads/ ──────→|
  |←──── 202 Accepted ───────────|
  |     Location: /upload/uuid    |
  |                                |
  |── PATCH /upload/uuid ────────→|  Chunk 1
  |   Content-Range: 0-999999     |
  |   Body: [청크 1 데이터]        |
  |←──── 202 Accepted ───────────|
  |     Location: /upload/uuid    |
  |                                |
  |── PATCH /upload/uuid ────────→|  Chunk 2
  |   Content-Range: 1000000-...  |
  |   Body: [청크 2 데이터]        |
  |←──── 202 Accepted ───────────|
  |                                |
  |── PUT /upload/uuid ──────────→|  마지막 Chunk
  |   ?digest=sha256:abc...       |
  |   Content-Range: ...          |
  |   Body: [마지막 청크 데이터]   |
  |←──── 201 Created ────────────|
```

핵심 포인트:
- 중간 청크는 `PATCH`, 마지막 청크만 `PUT`을 사용한다
- `PUT` 요청에만 `digest` 쿼리 파라미터를 포함한다
- 각 응답의 `Location` 헤더에서 다음 업로드 URL을 가져온다
- AWS ECR 호환성: `201 Created`와 `202 Accepted` 모두 수락한다 (254행 주석)

---

## 10. Range 기반 재개 가능 Pull

**파일 경로**: `Sources/tart/OCI/Registry.swift` 279~304행

```swift
public func pullBlob(_ digest: String, rangeStart: Int64 = 0,
                     handler: (Data) async throws -> Void) async throws {
  var expectedStatusCode = HTTPCode.Ok
  var headers: [String: String] = [:]

  // rangeStart가 0이 아닐 때만 Range 헤더를 보낸다
  if rangeStart != 0 {
    expectedStatusCode = HTTPCode.PartialContent
    headers["Range"] = "bytes=\(rangeStart)-"
  }

  let (channel, response) = try await channelRequest(.GET,
    endpointURL("\(namespace)/blobs/\(digest)"), headers: headers, viaFile: true)

  if response.statusCode != expectedStatusCode.rawValue { ... }

  for try await part in channel {
    try Task.checkCancellation()
    try await handler(part)
  }
}
```

### Range 요청 동작

```
rangeStart = 0 (기본):
  GET /v2/{ns}/blobs/{digest}
  → HTTP 200 OK (전체 데이터)

rangeStart = 1048576 (1MB 이후부터):
  GET /v2/{ns}/blobs/{digest}
  Range: bytes=1048576-
  → HTTP 206 Partial Content (부분 데이터)
```

왜 `rangeStart == 0`일 때 Range 헤더를 보내지 않는가? 소스코드 주석(285~287행):

```swift
// However, do not send Range header at all when rangeStart is 0,
// because it makes no sense and we might get HTTP 200 in return
```

일부 레지스트리가 `Range: bytes=0-`에 대해 HTTP 200을 반환할 수 있으므로, 불필요한 Range 헤더를 아예 보내지 않는 것이 안전하다.

### 스트리밍 처리

`handler` 클로저를 사용하여 데이터를 스트리밍으로 처리한다. 대용량 blob(수 GB의 디스크 이미지)을 메모리에 전부 올리지 않고 청크 단위로 파일에 쓸 수 있다. 각 청크 처리 전에 `Task.checkCancellation()`으로 취소를 확인한다.

---

## 11. Fetcher 클래스: URLSession과 AsyncThrowingStream

**파일 경로**: `Sources/tart/Fetcher.swift`

```swift
fileprivate var urlSession: URLSession = {
  let config = URLSessionConfiguration.default
  config.httpShouldSetCookies = false  // Harbor CSRF 문제 해결
  return URLSession(configuration: config)
}()

class Fetcher {
  static func fetch(_ request: URLRequest, viaFile: Bool = false)
    async throws -> (AsyncThrowingStream<Data, Error>, HTTPURLResponse) {
    let task = urlSession.dataTask(with: request)
    let delegate = Delegate()
    task.delegate = delegate

    let stream = AsyncThrowingStream<Data, Error> { continuation in
      delegate.streamContinuation = continuation
    }

    let response = try await withCheckedThrowingContinuation { continuation in
      delegate.responseContinuation = continuation
      task.resume()
    }

    return (stream, response as! HTTPURLResponse)
  }
}
```

### Delegate 내부 구현

```swift
fileprivate class Delegate: NSObject, URLSessionDataDelegate {
  var responseContinuation: CheckedContinuation<URLResponse, Error>?
  var streamContinuation: AsyncThrowingStream<Data, Error>.Continuation?

  private var buffer: Data = Data()
  private let bufferFlushSize = 16 * 1024 * 1024  // 16MB

  func urlSession(_ session: URLSession, dataTask: URLSessionDataTask,
                   didReceive response: URLResponse,
                   completionHandler: @escaping (URLSession.ResponseDisposition) -> Void) {
    let capacity = min(response.expectedContentLength, Int64(bufferFlushSize))
    buffer = Data(capacity: Int(capacity))
    responseContinuation?.resume(returning: response)
    responseContinuation = nil
    completionHandler(.allow)
  }

  func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
    buffer.append(data)
    if buffer.count >= bufferFlushSize {
      streamContinuation?.yield(buffer)
      buffer.removeAll(keepingCapacity: true)
    }
  }

  func urlSession(_ session: URLSession, task: URLSessionTask,
                   didCompleteWithError error: Error?) {
    if let error = error {
      responseContinuation?.resume(throwing: error)
      streamContinuation?.finish(throwing: error)
    } else {
      if !buffer.isEmpty {
        streamContinuation?.yield(buffer)
        buffer.removeAll(keepingCapacity: true)
      }
      streamContinuation?.finish()
    }
  }
}
```

### 데이터 흐름

```
URLSession의 dataTask
        |
        v
  +------------------+
  | Delegate         |
  |                  |
  | didReceive       |──→ responseContinuation.resume()
  | (response)       |    → 호출자에게 HTTPURLResponse 전달
  |                  |
  | didReceive       |──→ buffer에 축적
  | (data)           |    buffer >= 16MB → streamContinuation.yield()
  |                  |    → AsyncThrowingStream으로 청크 전달
  |                  |
  | didComplete      |──→ 잔여 buffer yield → streamContinuation.finish()
  +------------------+
```

### 16MB 버퍼의 이유

URLSession은 네트워크에서 데이터를 소량씩 (보통 수 KB~수십 KB) 전달한다. 매번 yield하면 소비자 측에서 과도한 컨텍스트 스위칭이 발생한다. 16MB로 버퍼링하여 스트림 소비자의 효율을 높인다.

### 쿠키 비활성화 이유

소스코드 주석(3~16행):

```swift
// Harbor expects a CSRF token to be present if the HTTP client
// carries a session cookie between its requests and fails if
// it was not present.
//
// To fix that, we disable the automatic cookies carry in URLSession.
config.httpShouldSetCookies = false
```

Harbor 레지스트리는 세션 쿠키가 있으면 CSRF 토큰을 요구하는데, Tart는 브라우저가 아니므로 CSRF 토큰을 제공할 수 없다. 쿠키를 아예 비활성화하여 이 문제를 회피한다.

---

## 12. OCIManifest 구조

**파일 경로**: `Sources/tart/OCI/Manifest.swift`

```swift
struct OCIManifest: Codable, Equatable {
  var schemaVersion: Int = 2
  var mediaType: String = ociManifestMediaType
  var config: OCIManifestConfig
  var layers: [OCIManifestLayer] = Array()
  var annotations: Dictionary<String, String>?
}
```

### 매니페스트 JSON 예시

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "size": 156,
    "digest": "sha256:abc123..."
  },
  "layers": [
    {
      "mediaType": "application/vnd.cirruslabs.tart.config.v1",
      "size": 512,
      "digest": "sha256:config..."
    },
    {
      "mediaType": "application/vnd.cirruslabs.tart.disk.v2",
      "size": 4294967296,
      "digest": "sha256:disk1...",
      "annotations": {
        "org.cirruslabs.tart.uncompressed-size": "8589934592",
        "org.cirruslabs.tart.uncompressed-content-digest": "sha256:raw1..."
      }
    },
    {
      "mediaType": "application/vnd.cirruslabs.tart.nvram.v1",
      "size": 262144,
      "digest": "sha256:nvram..."
    }
  ],
  "annotations": {
    "org.cirruslabs.tart.uncompressed-disk-size": "8589934592",
    "org.cirruslabs.tart.upload-time": "2024-01-15T10:30:00Z"
  }
}
```

### OCIManifestConfig

```swift
struct OCIManifestConfig: Codable, Equatable {
  var mediaType: String = ociConfigMediaType
  var size: Int
  var digest: String
}
```

OCI 표준에서 config는 이미지의 메타데이터를 담는다. Tart에서는 `OCIConfig` 구조체를 직렬화한 JSON이다.

### OCIManifestLayer

```swift
struct OCIManifestLayer: Codable, Equatable, Hashable {
  var mediaType: String
  var size: Int
  var digest: String
  var annotations: Dictionary<String, String>?
}
```

레이어의 `Hashable` 구현은 `digest`만으로 동등성을 판단한다:

```swift
static func == (lhs: Self, rhs: Self) -> Bool {
  return lhs.digest == rhs.digest
}

func hash(into hasher: inout Hasher) {
  hasher.combine(digest)
}
```

이것은 LocalLayerCache에서 중복 레이어를 Set 연산으로 효율적으로 감지하기 위한 설계이다.

### OCIConfig

```swift
struct OCIConfig: Codable {
  var architecture: Architecture = .arm64
  var os: OS = .darwin
  var config: ConfigContainer?

  struct ConfigContainer: Codable {
    var Labels: [String: String]?
  }
}
```

Docker Hub 호환성을 위한 스텁(stub) OCI config이다. Docker Hub는 OCI config에 `architecture`와 `os` 필드를 요구한다.

---

## 13. 커스텀 미디어 타입

**파일 경로**: `Sources/tart/OCI/Manifest.swift` 1~22행

```swift
// OCI 표준 미디어 타입
let ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
let ociConfigMediaType = "application/vnd.oci.image.config.v1+json"

// Tart 커스텀 레이어 미디어 타입
let configMediaType = "application/vnd.cirruslabs.tart.config.v1"
let diskV2MediaType = "application/vnd.cirruslabs.tart.disk.v2"
let nvramMediaType = "application/vnd.cirruslabs.tart.nvram.v1"

// 매니페스트 어노테이션
let uncompressedDiskSizeAnnotation = "org.cirruslabs.tart.uncompressed-disk-size"
let uploadTimeAnnotation = "org.cirruslabs.tart.upload-time"

// 레이블
let diskFormatLabel = "org.cirruslabs.tart.disk.format"

// 레이어 어노테이션
let uncompressedSizeAnnotation = "org.cirruslabs.tart.uncompressed-size"
let uncompressedContentDigestAnnotation = "org.cirruslabs.tart.uncompressed-content-digest"
```

### 미디어 타입 체계

```
OCI Manifest
├── config: application/vnd.oci.image.config.v1+json
│   └─ OCIConfig (arch, os, labels)
│
├── layer[0]: application/vnd.cirruslabs.tart.config.v1
│   └─ VMConfig JSON (CPU, 메모리, MAC 주소 등)
│
├── layer[1..N]: application/vnd.cirruslabs.tart.disk.v2
│   └─ 압축된 디스크 이미지 청크
│   └─ annotations:
│       ├─ org.cirruslabs.tart.uncompressed-size
│       └─ org.cirruslabs.tart.uncompressed-content-digest
│
└── layer[N+1]: application/vnd.cirruslabs.tart.nvram.v1
    └─ NVRAM 바이너리 데이터
```

레거시 미디어 타입(`VMDirectory+OCI.swift` 5행):

```swift
let legacyDiskV1MediaType = "application/vnd.cirruslabs.tart.disk.v1"
```

V1 미디어 타입은 더 이상 지원하지 않으며, pull 시도 시 에러를 발생시킨다:

```swift
if manifest.layers.contains(where: { $0.mediaType == legacyDiskV1MediaType }) {
  throw RuntimeError.Generic("Pulling OCI images with legacy disk media type ... is no longer supported")
}
```

---

## 14. RemoteName 파싱

**파일 경로**: `Sources/tart/OCI/RemoteName.swift`

```swift
struct RemoteName: Comparable, Hashable, CustomStringConvertible {
  var host: String
  var namespace: String
  var reference: Reference

  init(_ name: String) throws {
    let errorCollector = ErrorCollector()
    let inputStream = ANTLRInputStream(Array(name.unicodeScalars), name.count)
    let lexer = ReferenceLexer(inputStream)
    lexer.removeErrorListeners()
    lexer.addErrorListener(errorCollector)

    let tokenStream = CommonTokenStream(lexer)
    let parser = try ReferenceParser(tokenStream)
    parser.removeErrorListeners()
    parser.addErrorListener(errorCollector)

    let referenceCollector = ReferenceCollector()
    try ParseTreeWalker().walk(referenceCollector, try parser.root())
    ...
  }

  var description: String {
    "\(host)/\(namespace)\(reference.fullyQualified)"
  }
}
```

### Reference 타입

```swift
struct Reference: Comparable, Hashable, CustomStringConvertible {
  enum ReferenceType: Comparable {
    case Tag
    case Digest
  }

  let type: ReferenceType
  let value: String

  var fullyQualified: String {
    get {
      switch type {
      case .Tag: return ":" + value
      case .Digest: return "@" + value
      }
    }
  }
}
```

### 파싱 예시

```
입력: "ghcr.io/cirruslabs/macos-ventura-vanilla:latest"
      ^^^^^^^^  ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^  ^^^^^^
      host      namespace                           tag

결과:
  host = "ghcr.io"
  namespace = "cirruslabs/macos-ventura-vanilla"
  reference = Reference(tag: "latest")
  description = "ghcr.io/cirruslabs/macos-ventura-vanilla:latest"


입력: "ghcr.io/cirruslabs/macos:sha256@sha256:abc123..."
      ^^^^^^^^  ^^^^^^^^^^^^^^^^^^^^^  ^^^^^^^^^^^^^^^^^^
      host      namespace              digest

결과:
  host = "ghcr.io"
  namespace = "cirruslabs/macos"
  reference = Reference(digest: "sha256:abc123...")
  description = "ghcr.io/cirruslabs/macos@sha256:abc123..."


입력: "localhost:5000/test/image"  (태그 생략)

결과:
  host = "localhost:5000"
  namespace = "test/image"
  reference = Reference(tag: "latest")  // 기본값
```

Tart는 Antlr4 파서 라이브러리를 사용하여 OCI 이미지 참조를 정확하게 파싱한다. 이는 정규식보다 강력하며, OCI Distribution Spec의 문법을 충실히 구현한다.

---

## 15. VM Push/Pull 전체 흐름

### Pull 흐름

**파일 경로**: `Sources/tart/VMDirectory+OCI.swift` 16~89행

```
tart pull ghcr.io/org/vm-image:latest
=============================================

1. registry.pullManifest(reference: "latest")
   └─ GET /v2/{ns}/manifests/latest
   └─ OCIManifest + raw Data 수신

2. config 레이어 Pull
   └─ manifest.layers.filter { mediaType == configMediaType }
   └─ registry.pullBlob(configDigest) → configURL에 저장

3. disk 레이어 Pull
   └─ manifest.layers.filter { mediaType == diskV2MediaType }
   └─ 레거시 V1 미디어 타입 감지 시 에러
   └─ DiskV2.pull() → 압축 해제 + diskURL에 저장
   └─ LocalLayerCache로 중복 레이어 건너뛰기

4. NVRAM 레이어 Pull
   └─ manifest.layers.filter { mediaType == nvramMediaType }
   └─ registry.pullBlob(nvramDigest) → nvramURL에 저장

5. manifest.json 저장 (후속 pull 최적화용)
   └─ manifest.toJSON().write(to: manifestURL)
```

### Push 흐름

**파일 경로**: `Sources/tart/VMDirectory+OCI.swift` 91~142행

```
tart push ghcr.io/org/vm-image:latest
=============================================

1. VMConfig 읽기 + 직렬화
   └─ config = VMConfig(fromURL: configURL)
   └─ configJSON = JSONEncoder().encode(config)

2. config 레이어 Push
   └─ registry.pushBlob(fromData: configJSON)
   └─ OCIManifestLayer(mediaType: configMediaType, ...) 생성

3. disk 레이어 Push
   └─ DiskV2.push(diskURL, registry, ...)
   └─ 디스크를 청크로 분할 + 압축 + pushBlob
   └─ 각 청크에 대해 OCIManifestLayer 생성

4. NVRAM 레이어 Push
   └─ registry.pushBlob(fromData: nvram)
   └─ OCIManifestLayer(mediaType: nvramMediaType, ...) 생성

5. OCI Config Push (Docker Hub 호환용 스텁)
   └─ OCIConfig(architecture: config.arch, os: config.os, ...)
   └─ registry.pushBlob(fromData: ociConfigJSON)

6. Manifest Push
   └─ OCIManifest(config: ociConfigDigest, layers: [...])
   └─ registry.pushManifest(reference: tag, manifest: manifest)
```

### Labels 자동 추가

```swift
var labels = labels
labels[diskFormatLabel] = config.diskFormat.rawValue
```

Push 시 디스크 포맷(raw/asif)을 OCI config의 label로 자동 추가한다.

---

## 16. Digest: SHA256 해시 계산

**파일 경로**: `Sources/tart/OCI/Digest.swift`

```swift
class Digest {
  var hash: SHA256 = SHA256()

  func update(_ data: Data) {
    hash.update(data: data)
  }

  func finalize() -> String {
    hash.finalize().hexdigest()
  }

  static func hash(_ data: Data) -> String {
    SHA256.hash(data: data).hexdigest()
  }

  static func hash(_ url: URL) throws -> String {
    hash(try Data(contentsOf: url))
  }

  static func hash(_ url: URL, offset: UInt64, size: UInt64) throws -> String {
    // 파일의 특정 범위에 대한 해시 계산
    ...
  }
}

extension SHA256.Digest {
  func hexdigest() -> String {
    "sha256:" + self.map { String(format: "%02x", $0) }.joined()
  }
}
```

OCI 표준에서 모든 content-addressable 참조는 `sha256:` 접두사를 가진 해시 값이다. `Digest` 클래스는 두 가지 사용 패턴을 지원한다:

1. **일회성 해시**: `Digest.hash(data)` 정적 메서드
2. **스트리밍 해시**: `Digest()` 인스턴스 생성 → `update()` 반복 호출 → `finalize()`

IPSW 다운로드(`VM.swift` 121~131행)에서 스트리밍 해시 패턴을 사용한다:

```swift
let digest = Digest()
for try await chunk in channel {
  try fileHandle.write(contentsOf: chunk)
  digest.update(chunk)
  progress.completedUnitCount += Int64(chunk.count)
}
let finalLocation = try IPSWCache().locationFor(fileName: digest.finalize() + ".ipsw")
```

---

## 17. 설계 결정과 트레이드오프

### 왜 OCI 레지스트리를 사용하는가?

Tart는 VM 이미지 배포를 위해 자체 프로토콜을 만들지 않고 OCI Distribution Spec을 채택했다. 이유:

1. **기존 인프라 재사용**: Docker Hub, GHCR, ECR 등 이미 운영 중인 레지스트리를 그대로 사용
2. **인증 체계 통합**: Docker `config.json`, 환경변수, Keychain 등 기존 자격 증명 체계 활용
3. **Content Addressable**: SHA256 다이제스트 기반으로 레이어 중복 제거가 자연스럽게 가능
4. **표준 도구 호환**: `crane`, `skopeo` 등 OCI 도구로 이미지 검사/복사 가능

### 왜 Lazy 인증인가?

첫 요청을 인증 없이 보내고 401을 받은 후에야 인증하는 이유:

1. 일부 레지스트리는 public 이미지에 대해 인증이 불필요하다
2. WWW-Authenticate 헤더에서 realm, scope, service 정보를 얻어야 정확한 토큰을 요청할 수 있다
3. OCI Distribution Spec이 이 흐름을 표준으로 정의한다

### 왜 Antlr4로 RemoteName을 파싱하는가?

이미지 참조(예: `ghcr.io:443/org/repo@sha256:abc123`)의 문법은 단순한 정규식으로 처리하기 어렵다:
- 호스트에 포트가 포함될 수 있다 (콜론이 태그 구분자와 겹침)
- 네임스페이스가 여러 레벨일 수 있다 (`org/suborg/repo`)
- 태그와 다이제스트 참조가 다른 구분자를 사용한다 (`:` vs `@`)

Antlr4 파서를 사용하면 OCI Distribution Spec의 정확한 문법을 구현할 수 있다.

### 왜 AsyncThrowingStream + 16MB 버퍼인가?

대용량 디스크 이미지(수십 GB)를 다운로드할 때:
- 전체를 메모리에 올릴 수 없다 → 스트리밍 필수
- URLSession의 콜백은 수 KB 단위 → 과도한 yield 방지를 위해 16MB 버퍼링
- AsyncThrowingStream은 Swift Concurrency와 자연스럽게 통합된다
- `for try await`로 소비하면 backpressure가 자동으로 적용된다
