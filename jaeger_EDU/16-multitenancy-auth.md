# 멀티테넌시 & 인증

## 목차
1. [개요](#1-개요)
2. [멀티테넌시 아키텍처](#2-멀티테넌시-아키텍처)
3. [Manager 구조체와 Guard 패턴](#3-manager-구조체와-guard-패턴)
4. [Context 전파 메커니즘](#4-context-전파-메커니즘)
5. [gRPC 인터셉터](#5-grpc-인터셉터)
6. [HTTP 미들웨어](#6-http-미들웨어)
7. [MetadataAnnotator](#7-metadataannotator)
8. [Bearer Token 인증](#8-bearer-token-인증)
9. [Token Loader (파일 기반 캐시)](#9-token-loader-파일-기반-캐시)
10. [RoundTripper (HTTP 전송 래퍼)](#10-roundtripper-http-전송-래퍼)
11. [gRPC Bearer Token 인터셉터](#11-grpc-bearer-token-인터셉터)
12. [API Key 인증](#12-api-key-인증)
13. [보안 확장 기능](#13-보안-확장-기능)
14. [멀티테넌시와 인증의 통합](#14-멀티테넌시와-인증의-통합)
15. [설계 원칙과 Why 분석](#15-설계-원칙과-why-분석)

---

## 1. 개요

Jaeger는 대규모 조직에서 여러 팀이나 서비스가 하나의 Jaeger 인스턴스를 공유해야 하는 상황을 위해 멀티테넌시(multi-tenancy) 기능을 제공한다. 또한 다양한 인증(authentication) 메커니즘을 통해 API 접근을 보호한다.

이 문서에서는 Jaeger의 멀티테넌시 시스템(`internal/tenancy/`)과 인증 시스템(`internal/auth/`)의 설계와 구현을 소스코드 수준에서 분석한다.

### 핵심 설계 목표

| 목표 | 설명 |
|------|------|
| 테넌트 격리 | 각 테넌트의 트레이스 데이터를 논리적으로 분리 |
| 투명한 전파 | HTTP/gRPC 경계를 넘어 테넌트/토큰 정보를 자동 전파 |
| 선택적 활성화 | 멀티테넌시/인증을 필요에 따라 켜고 끌 수 있음 |
| 확장 가능한 인증 | Bearer token, API key 등 다양한 인증 방식 지원 |

### 전체 아키텍처

```
+------------------+     +------------------+     +------------------+
|  HTTP Client     |     |  gRPC Client     |     |  OpenTelemetry   |
|  (x-tenant 헤더) |     |  (metadata)      |     |  Collector       |
+--------+---------+     +--------+---------+     +--------+---------+
         |                        |                        |
         v                        v                        v
+--------+---------+     +--------+---------+     +--------+---------+
| ExtractTenant    |     | Guarding         |     | MetadataAnnotator|
| HTTPHandler      |     | Interceptor      |     | (HTTP->gRPC)     |
+--------+---------+     +--------+---------+     +--------+---------+
         |                        |                        |
         +----------+-------------+----------+-------------+
                    |                        |
                    v                        v
            +-------+--------+      +-------+--------+
            | WithTenant()   |      | ContextWith    |
            | GetTenant()    |      | BearerToken()  |
            | (context.Value)|      | (context.Value)|
            +-------+--------+      +-------+--------+
                    |                        |
                    v                        v
            +-------+------------------------+--------+
            |           Storage Layer                  |
            |  (테넌트별 데이터 격리 / 인증된 접근)    |
            +------------------------------------------+
```

---

## 2. 멀티테넌시 아키텍처

### 소스 파일 구조

```
internal/tenancy/
├── manager.go       # Manager 구조체, guard 인터페이스, Options
├── context.go       # WithTenant/GetTenant (context 전파)
├── grpc.go          # gRPC 서버/클라이언트 인터셉터
├── http.go          # HTTP 미들웨어, MetadataAnnotator
├── flags.go         # CLI 플래그 정의
├── *_test.go        # 테스트 파일들
└── package_test.go
```

### CLI 플래그

멀티테넌시는 `--multi-tenancy.*` 플래그로 설정한다.

소스 경로: `internal/tenancy/flags.go`

```go
const (
    flagPrefix         = "multi-tenancy"
    flagTenancyEnabled = flagPrefix + ".enabled"
    flagTenancyHeader  = flagPrefix + ".header"
    flagValidTenants   = flagPrefix + ".tenants"
)

func AddFlags(flags *flag.FlagSet) {
    flags.Bool(flagTenancyEnabled, false, "Enable tenancy header when receiving or querying")
    flags.String(flagTenancyHeader, "x-tenant", "HTTP header carrying tenant")
    flags.String(flagValidTenants, "",
        fmt.Sprintf("comma-separated list of allowed values for --%s header.  "+
            "(If not supplied, tenants are not restricted)", flagTenancyHeader))
}
```

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--multi-tenancy.enabled` | `false` | 멀티테넌시 활성화 여부 |
| `--multi-tenancy.header` | `x-tenant` | 테넌트 정보를 전달하는 HTTP 헤더 이름 |
| `--multi-tenancy.tenants` | `""` (빈 문자열) | 허용된 테넌트 목록 (콤마 구분) |

### Options 구조체

```go
// internal/tenancy/manager.go
type Options struct {
    Enabled bool
    Header  string
    Tenants []string
}
```

`InitFromViper()` 함수에서 Viper 설정으로부터 Options를 초기화한다:

```go
func InitFromViper(v *viper.Viper) Options {
    var p Options
    p.Enabled = v.GetBool(flagTenancyEnabled)
    p.Header = v.GetString(flagTenancyHeader)
    tenants := v.GetString(flagValidTenants)
    if tenants != "" {
        p.Tenants = strings.Split(tenants, ",")
    } else {
        p.Tenants = []string{}
    }
    return p
}
```

---

## 3. Manager 구조체와 Guard 패턴

### Manager 구조체

소스 경로: `internal/tenancy/manager.go`

```go
type Manager struct {
    Enabled bool
    Header  string
    guard   guard
}

type guard interface {
    Valid(candidate string) bool
}
```

Manager는 멀티테넌시의 핵심 제어 객체이다. `Enabled` 필드로 기능 활성화 여부를, `Header` 필드로 테넌트 헤더 이름을, `guard` 인터페이스로 테넌트 유효성 검증 로직을 관리한다.

### NewManager 팩토리

```go
func NewManager(options *Options) *Manager {
    header := options.Header
    if header == "" && options.Enabled {
        header = "x-tenant"
    }
    return &Manager{
        Enabled: options.Enabled,
        Header:  header,
        guard:   tenancyGuardFactory(options),
    }
}
```

헤더가 지정되지 않았지만 멀티테넌시가 활성화된 경우, 기본값 `"x-tenant"`를 사용한다.

### Guard 패턴: 3가지 모드

`tenancyGuardFactory()` 함수는 설정에 따라 3가지 guard 구현을 반환한다:

```go
func tenancyGuardFactory(options *Options) guard {
    // 세 가지 경우:
    // - 테넌시 비활성 (tenancy disabled)
    // - 테넌시 활성, 가드 없음 (tenancy enabled, no guard)
    // - 테넌시 활성, 화이트리스트 가드 (tenancy enabled, list guard)

    if !options.Enabled || len(options.Tenants) == 0 {
        return tenantDontCare(true)
    }
    return newTenantList(options.Tenants)
}
```

#### Guard 구현 1: tenantDontCare

```go
type tenantDontCare bool

func (tenantDontCare) Valid(string) bool {
    return true
}
```

테넌시가 비활성이거나 테넌트 목록이 비어 있으면 모든 테넌트를 허용한다. 이 구현은 테넌시 비활성 시와 테넌시 활성이지만 화이트리스트가 없는 경우 모두에 사용된다.

#### Guard 구현 2: tenantList (화이트리스트)

```go
type tenantList struct {
    tenants map[string]bool
}

func (tl *tenantList) Valid(candidate string) bool {
    _, ok := tl.tenants[candidate]
    return ok
}

func newTenantList(tenants []string) *tenantList {
    tenantMap := make(map[string]bool)
    for _, tenant := range tenants {
        tenantMap[tenant] = true
    }
    return &tenantList{tenants: tenantMap}
}
```

허용된 테넌트 목록을 `map[string]bool`로 변환하여 O(1) 조회를 수행한다.

### 모드별 동작 비교

```
+-----------------------------+----------------------------+-----------------------------+
|   Mode 1: Disabled          |   Mode 2: Enabled, No List |   Mode 3: Enabled + List    |
+-----------------------------+----------------------------+-----------------------------+
|   Enabled: false            |   Enabled: true            |   Enabled: true             |
|   Guard: tenantDontCare     |   Guard: tenantDontCare    |   Guard: tenantList         |
|   결과: 모든 요청 통과      |   결과: 헤더 필수,         |   결과: 헤더 필수,          |
|                             |   모든 테넌트 허용         |   화이트리스트만 허용       |
+-----------------------------+----------------------------+-----------------------------+
```

### Why: guard 인터페이스를 사용하는 이유

guard 인터페이스를 통해 유효성 검증 로직을 추상화함으로써:

1. **조건 분기 제거**: 매 요청마다 `if enabled && len(tenants) > 0`를 체크하는 대신, 한번 생성된 guard 객체의 `Valid()` 호출만으로 충분하다
2. **확장성**: 새로운 검증 방식 (예: LDAP 기반, DB 기반)을 guard 인터페이스를 구현하는 것만으로 추가할 수 있다
3. **테스트 용이성**: guard를 모킹하여 Manager의 다른 로직을 독립적으로 테스트할 수 있다

---

## 4. Context 전파 메커니즘

### 테넌트 정보의 Context 저장

소스 경로: `internal/tenancy/context.go`

```go
type tenantKeyType string

const (
    tenantKey = tenantKeyType("tenant")
)

func WithTenant(ctx context.Context, tenant string) context.Context {
    return context.WithValue(ctx, tenantKey, tenant)
}

func GetTenant(ctx context.Context) string {
    tenant := ctx.Value(tenantKey)
    if tenant == nil {
        return ""
    }
    if s, ok := tenant.(string); ok {
        return s
    }
    return ""
}
```

### Context 전파 흐름

```
  HTTP 요청 수신           gRPC 요청 수신           OTel Collector
       |                       |                       |
       v                       v                       v
  헤더에서 추출            메타데이터에서 추출       client.Metadata에서 추출
  r.Header.Get()           metadata.FromIncoming()   client.FromContext()
       |                       |                       |
       v                       v                       v
  +----+----+              +---+---+              +----+----+
  |WithTenant|             |WithTenant|            |WithTenant|
  |(ctx, t)  |             |(ctx, t)  |            |(ctx, t)  |
  +----+-----+             +----+----+             +----+----+
       |                       |                       |
       +----------+------------+----------+------------+
                  |                       |
                  v                       v
           GetTenant(ctx)           gRPC 클라이언트
           (스토리지 계층)          인터셉터에서
                                   metadata.AppendToOutgoing()
```

### Why: `tenantKeyType`을 커스텀 타입으로 정의하는 이유

```go
type tenantKeyType string
```

Go의 `context.WithValue`는 키의 타입과 값 모두로 비교한다. 단순 `string` 대신 커스텀 타입을 사용하면:

1. **키 충돌 방지**: 다른 패키지에서 동일한 문자열 `"tenant"`를 키로 사용해도 타입이 다르므로 충돌하지 않는다
2. **Go 컨벤션 준수**: `context.WithValue` 문서에서 권장하는 패턴이다
3. **캡슐화**: `tenantKeyType`이 exported 되지 않으므로 외부 패키지에서 직접 context 값을 조작할 수 없다

---

## 5. gRPC 인터셉터

### 테넌트 추출 공통 로직

소스 경로: `internal/tenancy/grpc.go`

gRPC에서 테넌트를 추출할 때, 3가지 소스를 순서대로 확인한다:

```go
func extractTenantFromSources(ctx context.Context, header string) (string, error) {
    // 1. context에 직접 부착된 테넌트 (이미 업스트림에서 처리된 경우)
    if tenant := GetTenant(ctx); tenant != "" {
        return tenant, nil
    }

    // 2. OTel Collector의 client.Metadata
    if cli := client.FromContext(ctx); cli.Metadata.Get(header) != nil {
        if tenants := cli.Metadata.Get(header); len(tenants) > 0 {
            return extractSingleTenant(tenants)
        }
    }

    // 3. gRPC incoming metadata
    md, ok := metadata.FromIncomingContext(ctx)
    if !ok {
        return "", status.Errorf(codes.PermissionDenied, "missing tenant header")
    }
    return extractSingleTenant(md.Get(header))
}
```

### 단일 테넌트 보장

```go
func extractSingleTenant(tenants []string) (string, error) {
    switch len(tenants) {
    case 0:
        return "", status.Errorf(codes.Unauthenticated, "missing tenant header")
    case 1:
        return tenants[0], nil
    default:
        return "", status.Errorf(codes.PermissionDenied, "extra tenant header")
    }
}
```

헤더에 여러 테넌트 값이 있으면 `PermissionDenied` 에러를 반환한다. 이는 의도적인 보안 결정으로, 테넌트 혼동 공격을 방지한다.

### GetValidTenant: 추출 + 유효성 검사

```go
func GetValidTenant(ctx context.Context, tm *Manager) (string, error) {
    tenant, err := extractTenantFromSources(ctx, tm.Header)
    if err != nil {
        return "", err
    }
    if !tm.Valid(tenant) {
        return "", status.Errorf(codes.PermissionDenied, "unknown tenant")
    }
    return tenant, nil
}
```

### Guarding Stream Interceptor (서버 측)

```go
func NewGuardingStreamInterceptor(tc *Manager) grpc.StreamServerInterceptor {
    return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo,
                handler grpc.StreamHandler) error {
        tenant, err := GetValidTenant(ss.Context(), tc)
        if err != nil {
            return err
        }

        if directlyAttachedTenant(ss.Context()) {
            return handler(srv, ss)
        }

        // 메타데이터에서 context 값으로 "업그레이드"
        return handler(srv, &tenantedServerStream{
            ServerStream: ss,
            context:      WithTenant(ss.Context(), tenant),
        })
    }
}
```

### tenantedServerStream 래퍼

```go
type tenantedServerStream struct {
    grpc.ServerStream
    context context.Context
}

func (tss *tenantedServerStream) Context() context.Context {
    return tss.context
}
```

`grpc.ServerStream`의 `Context()` 메서드를 오버라이드하여, 테넌트가 주입된 새 context를 반환한다. 기존 ServerStream의 다른 모든 메서드는 임베딩을 통해 그대로 위임된다.

### Guarding Unary Interceptor (서버 측)

```go
func NewGuardingUnaryInterceptor(tc *Manager) grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo,
                handler grpc.UnaryHandler) (any, error) {
        tenant, err := GetValidTenant(ctx, tc)
        if err != nil {
            return nil, err
        }

        if directlyAttachedTenant(ctx) {
            return handler(ctx, req)
        }

        return handler(WithTenant(ctx, tenant), req)
    }
}
```

Unary RPC는 Stream과 달리 context를 직접 교체할 수 있으므로, 래퍼 없이 `WithTenant(ctx, tenant)`를 사용한다.

### Client Unary Interceptor

```go
func NewClientUnaryInterceptor(tc *Manager) grpc.UnaryClientInterceptor {
    return grpc.UnaryClientInterceptor(func(
        ctx context.Context, method string, req, reply any,
        cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
        opts ...grpc.CallOption,
    ) error {
        if tenant := GetTenant(ctx); tenant != "" {
            ctx = metadata.AppendToOutgoingContext(ctx, tc.Header, tenant)
        }
        return invoker(ctx, method, req, reply, cc, opts...)
    })
}
```

클라이언트 인터셉터는 context에 저장된 테넌트 정보를 gRPC outgoing metadata에 주입한다. 이를 통해 서비스 간 호출 시 테넌트 정보가 자동으로 전파된다.

### Client Stream Interceptor

```go
func NewClientStreamInterceptor(tc *Manager) grpc.StreamClientInterceptor {
    return grpc.StreamClientInterceptor(func(
        ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
        method string, streamer grpc.Streamer, opts ...grpc.CallOption,
    ) (grpc.ClientStream, error) {
        if tenant := GetTenant(ctx); tenant != "" {
            ctx = metadata.AppendToOutgoingContext(ctx, tc.Header, tenant)
        }
        return streamer(ctx, desc, cc, method, opts...)
    })
}
```

### 인터셉터 체인 구성도

```
클라이언트 요청
    |
    v
+---+-------------------+      +--+------------------+
| Client Unary          |      | Client Stream       |
| Interceptor           |      | Interceptor         |
| GetTenant(ctx) ->     |      | GetTenant(ctx) ->   |
| metadata.Append()     |      | metadata.Append()   |
+-----------+-----------+      +-----------+----------+
            |                              |
            v                              v
     gRPC 네트워크 전송 (metadata에 x-tenant 포함)
            |                              |
            v                              v
+-----------+-----------+      +-----------+----------+
| Server Unary          |      | Server Stream       |
| Guarding Interceptor  |      | Guarding Interceptor|
| GetValidTenant() ->   |      | GetValidTenant() -> |
| WithTenant(ctx, t)    |      | tenantedServerStream|
+-----------+-----------+      +-----------+----------+
            |                              |
            v                              v
      핸들러 (context에서 GetTenant() 사용)
```

---

## 6. HTTP 미들웨어

### ExtractTenantHTTPHandler

소스 경로: `internal/tenancy/http.go`

```go
func ExtractTenantHTTPHandler(tc *Manager, h http.Handler) http.Handler {
    if !tc.Enabled {
        return h  // 멀티테넌시 비활성 시 원본 핸들러 반환
    }

    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        tenant := r.Header.Get(tc.Header)
        if tenant == "" {
            w.WriteHeader(http.StatusUnauthorized)
            w.Write([]byte("missing tenant header"))
            return
        }

        if !tc.Valid(tenant) {
            w.WriteHeader(http.StatusUnauthorized)
            w.Write([]byte("unknown tenant"))
            return
        }

        ctx := WithTenant(r.Context(), tenant)
        h.ServeHTTP(w, r.WithContext(ctx))
    })
}
```

### 처리 흐름

```
HTTP 요청: GET /api/traces  (Header: x-tenant: team-a)
    |
    v
ExtractTenantHTTPHandler
    |
    +-- tc.Enabled == false? --> 원본 핸들러 호출 (테넌시 무시)
    |
    +-- tenant == ""? --> 401 Unauthorized ("missing tenant header")
    |
    +-- tc.Valid(tenant) == false? --> 401 Unauthorized ("unknown tenant")
    |
    +-- 성공:
        ctx = WithTenant(r.Context(), "team-a")
        h.ServeHTTP(w, r.WithContext(ctx))
```

### Why: HTTP에서는 401(Unauthorized)을, gRPC에서는 PermissionDenied를 사용하는 이유

HTTP와 gRPC는 각각의 프로토콜에 맞는 에러 코드를 사용한다:
- **HTTP**: `401 Unauthorized`는 인증 실패를 나타내는 표준 HTTP 상태 코드
- **gRPC**: `codes.PermissionDenied`와 `codes.Unauthenticated`는 gRPC의 표준 상태 코드

이는 각 프로토콜의 관용적(idiomatic) 에러 처리를 따르기 위한 것이다.

---

## 7. MetadataAnnotator

### HTTP-to-gRPC 변환

grpc-gateway를 사용하여 HTTP 요청을 gRPC로 변환할 때, HTTP 헤더의 테넌트 정보를 gRPC metadata로 전파한다.

```go
func (tc *Manager) MetadataAnnotator() func(context.Context, *http.Request) metadata.MD {
    return func(_ context.Context, req *http.Request) metadata.MD {
        tenant := req.Header.Get(tc.Header)
        if tenant == "" {
            // 테넌트 헤더 없으면 빈 metadata 반환
            // gRPC 쿼리 서비스에서 나중에 거부
            return metadata.Pairs()
        }
        return metadata.New(map[string]string{
            tc.Header: tenant,
        })
    }
}
```

### MetadataAnnotator 흐름도

```
HTTP 클라이언트
    |
    |  GET /api/traces
    |  Header: x-tenant: team-a
    v
+---+-------------------+
| grpc-gateway           |
|                        |
| MetadataAnnotator:     |
|   tenant = "team-a"   |
|   -> metadata.New(     |
|        {"x-tenant":   |
|         "team-a"})     |
+---+-------------------+
    |
    | gRPC 호출
    | metadata: x-tenant=team-a
    v
+---+-------------------+
| gRPC 쿼리 서비스       |
| Guarding Interceptor   |
|   -> GetValidTenant()  |
+------------------------+
```

### Why: MetadataAnnotator에서 빈 tenant를 에러로 처리하지 않는 이유

MetadataAnnotator는 gRPC 서버 측의 Guarding Interceptor에서 검증을 수행하기 때문에, 여기서는 단순히 빈 metadata를 전달한다. 이는 **단일 책임 원칙(SRP)**을 따르는 것으로:
- MetadataAnnotator: HTTP -> gRPC metadata 변환만 담당
- Guarding Interceptor: 테넌트 유효성 검사 담당

---

## 8. Bearer Token 인증

### 소스 파일 구조

```
internal/auth/
├── bearertoken/
│   ├── context.go     # ContextWithBearerToken/GetBearerToken
│   ├── grpc.go        # gRPC 서버/클라이언트 인터셉터
│   ├── http.go        # HTTP 미들웨어 (PropagationHandler)
│   └── *_test.go
├── apikey/
│   └── apikey-context.go  # API Key context 연산
├── tokenloader.go         # 파일 기반 토큰 로더 (캐시/리로드)
├── transport.go           # RoundTripper (HTTP 전송 래퍼)
└── *_test.go
```

### Bearer Token Context 연산

소스 경로: `internal/auth/bearertoken/context.go`

```go
type contextKeyType int

const contextKey = contextKeyType(iota)

const StoragePropagationKey = "storage.propagate.token"

func ContextWithBearerToken(ctx context.Context, token string) context.Context {
    if token == "" {
        return ctx  // 빈 토큰은 context에 저장하지 않음
    }
    return context.WithValue(ctx, contextKey, token)
}

func GetBearerToken(ctx context.Context) (string, bool) {
    val, ok := ctx.Value(contextKey).(string)
    return val, ok
}
```

### HTTP PropagationHandler

소스 경로: `internal/auth/bearertoken/http.go`

```go
func PropagationHandler(logger *zap.Logger, h http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := r.Context()
        authHeaderValue := r.Header.Get("Authorization")
        // Fallback: X-Forwarded-Access-Token 헤더
        if authHeaderValue == "" {
            authHeaderValue = r.Header.Get("X-Forwarded-Access-Token")
        }
        if authHeaderValue != "" {
            headerValue := strings.Split(authHeaderValue, " ")
            token := ""
            switch {
            case len(headerValue) == 2:
                // "Bearer <token>" 형식만 캡처
                if headerValue[0] == "Bearer" {
                    token = headerValue[1]
                }
            case len(headerValue) == 1:
                // 전체 값을 토큰으로 처리
                token = authHeaderValue
            default:
                logger.Warn("Invalid authorization header value, skipping token propagation")
            }
            h.ServeHTTP(w, r.WithContext(ContextWithBearerToken(ctx, token)))
        } else {
            h.ServeHTTP(w, r.WithContext(ctx))
        }
    })
}
```

### 토큰 추출 로직 상세

```
Authorization 헤더 값              처리 결과
-----------------------------------+---------------------------
"Bearer abc123"                    | token = "abc123"
"abc123"                           | token = "abc123" (단일 값)
"Basic dXNlcjpwYXNz"              | token = "" (Bearer가 아님)
"Bearer abc 123"                   | Warn 로그, token 무시
(헤더 없음)                        | X-Forwarded-Access-Token 확인
```

### Why: X-Forwarded-Access-Token Fallback을 지원하는 이유

OAuth2 프록시(oauth2-proxy, Pomerium 등) 뒤에서 Jaeger를 운영할 때, 프록시가 원본 Authorization 헤더를 제거하고 대신 `X-Forwarded-Access-Token` 헤더에 토큰을 넣는 경우가 많다. 이 fallback으로 프록시 환경에서도 토큰 전파가 가능하다.

---

## 9. Token Loader (파일 기반 캐시)

### cachedFileTokenLoader

소스 경로: `internal/auth/tokenloader.go`

```go
func cachedFileTokenLoader(path string, interval time.Duration,
                           timeFn func() time.Time) func() (string, error) {
    var (
        mu          sync.Mutex
        cachedToken string
        lastRead    time.Time
    )

    return func() (string, error) {
        mu.Lock()
        defer mu.Unlock()

        now := timeFn()

        // interval == 0: 최초 로드 후 재로드하지 않음
        // 그 외: interval 시간이 경과한 경우에만 재로드
        if !lastRead.IsZero() && (interval == 0 || now.Sub(lastRead) < interval) {
            return cachedToken, nil
        }

        // 파일에서 토큰 읽기
        b, err := os.ReadFile(filepath.Clean(path))
        if err != nil {
            return "", fmt.Errorf("failed to read token file: %w", err)
        }

        cachedToken = strings.TrimRight(string(b), "\r\n")
        lastRead = now
        return cachedToken, nil
    }
}
```

### TokenProvider 래퍼

```go
func TokenProvider(path string, interval time.Duration,
                   logger *zap.Logger) (func() string, error) {
    return TokenProviderWithTime(path, interval, logger, time.Now)
}

func TokenProviderWithTime(path string, interval time.Duration,
                           logger *zap.Logger,
                           timeFn func() time.Time) (func() string, error) {
    loader := cachedFileTokenLoader(path, interval, timeFn)

    // 초기 토큰 로드 (실패 시 에러 반환)
    currentToken, err := loader()
    if err != nil {
        return nil, fmt.Errorf("failed to get token from file: %w", err)
    }

    return func() string {
        newToken, err := loader()
        if err != nil {
            logger.Warn("Token reload failed", zap.Error(err))
            return currentToken  // 실패 시 마지막 성공 토큰 반환
        }
        currentToken = newToken
        return currentToken
    }, nil
}
```

### 토큰 로더 동작 타임라인

```
시간 -->

t=0      t=30s    t=60s    t=90s    t=120s   t=150s
|        |        |        |        |        |
v        v        v        v        v        v
[로드]   [캐시]   [리로드]  [캐시]   [리로드]  [캐시]
 |        |        |        |        |        |
 +-파일-+ +-캐시-+ +-파일-+ +-캐시-+ +-파일-+ +-캐시-+
 |읽기  | |반환  | |읽기  | |반환  | |읽기  | |반환  |
 +------+ +------+ +------+ +------+ +------+ +------+

(interval = 60s 가정)
```

### 토큰 로더 에러 내성 (Fault Tolerance)

```
정상 상태:
  loader() 호출 -> 파일 읽기 성공 -> cachedToken 갱신 -> 반환

파일 삭제/권한 에러:
  loader() 호출 -> 파일 읽기 실패 -> err 반환
  TokenProvider: logger.Warn() + currentToken 반환 (이전 성공 토큰)
```

### Why: interval=0이 "리로드 비활성"을 의미하는 이유

파일 기반 토큰은 보통 Kubernetes Secret으로 마운트된다. 토큰이 짧은 수명을 가지면 `interval > 0`으로 주기적 재로드가 필요하지만, 긴 수명 토큰의 경우 불필요한 파일 I/O를 피하기 위해 `interval=0`으로 최초 로드만 수행한다.

### Why: `timeFn`을 주입하는 이유

`time.Now`를 직접 호출하는 대신 함수로 주입받아 테스트에서 시간을 제어할 수 있도록 한다. 이는 Go에서 시간 의존 코드를 테스트하는 표준 패턴이다.

---

## 10. RoundTripper (HTTP 전송 래퍼)

### Method 구조체

소스 경로: `internal/auth/transport.go`

```go
type Method struct {
    // 인증 스킴 (예: "Bearer")
    Scheme string
    // 토큰을 반환하는 함수
    TokenFn func() string
    // context에서 토큰을 추출하는 함수
    FromCtx func(context.Context) (string, bool)
}
```

Method 구조체는 하나의 인증 방식을 표현한다. `Scheme`은 HTTP Authorization 헤더의 접두사를, `TokenFn`은 정적/동적 토큰 제공 함수를, `FromCtx`는 요청 context에서 토큰을 추출하는 함수를 나타낸다.

### RoundTripper 구조체

```go
type RoundTripper struct {
    Transport http.RoundTripper
    Auths     []Method
}
```

### RoundTrip 메서드

```go
func (tr RoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
    if tr.Transport == nil {
        return nil, errors.New("no http.RoundTripper provided")
    }

    req := r.Clone(r.Context())

    for _, auth := range tr.Auths {
        token := ""

        // 1. context에서 토큰 추출 시도
        if auth.FromCtx != nil {
            if t, ok := auth.FromCtx(r.Context()); ok {
                token = t
            }
        }

        // 2. 실패 시 TokenFn으로 폴백
        if token == "" && auth.TokenFn != nil {
            token = auth.TokenFn()
        }

        // 3. 토큰이 있으면 Authorization 헤더에 추가
        if token != "" {
            req.Header.Add("Authorization",
                fmt.Sprintf("%s %s", auth.Scheme, token))
        }
    }

    return tr.Transport.RoundTrip(req)
}
```

### RoundTripper 동작 흐름

```
HTTP 클라이언트 (Jaeger -> Elasticsearch/Kafka 등)
    |
    v
+---+-------------------+
| auth.RoundTripper      |
|                        |
| for each Method:       |
|   1. FromCtx(ctx)     |
|      -> bearer token  |
|   2. TokenFn()        |
|      -> static token  |
|   3. Header.Add(      |
|      "Authorization", |
|      "Bearer token")  |
+---+-------------------+
    |
    v
+---+-------------------+
| http.DefaultTransport  |
| (실제 HTTP 전송)       |
+------------------------+
```

### 다중 인증 방식 지원

`Auths` 슬라이스를 통해 여러 인증 방식을 동시에 적용할 수 있다:

```go
rt := &auth.RoundTripper{
    Transport: http.DefaultTransport,
    Auths: []auth.Method{
        {
            Scheme:  "Bearer",
            FromCtx: bearertoken.GetBearerToken,
            TokenFn: tokenProvider,
        },
        {
            Scheme:  "ApiKey",
            FromCtx: apikey.GetAPIKey,
        },
    },
}
```

### Why: Request를 Clone하는 이유

```go
req := r.Clone(r.Context())
```

`http.Request`는 동시에 여러 goroutine에서 사용될 수 있다. 원본 요청을 수정하면 데이터 레이스가 발생할 수 있으므로, Clone하여 안전하게 헤더를 추가한다.

---

## 11. gRPC Bearer Token 인터셉터

### metadata 키

소스 경로: `internal/auth/bearertoken/grpc.go`

```go
const Key = "bearer.token"
```

gRPC metadata에서 bearer token을 전달하는 키는 `"bearer.token"`이다.

### 토큰 추출 함수

```go
func ValidTokenFromGRPCMetadata(ctx context.Context, bearerHeader string) (string, error) {
    md, ok := metadata.FromIncomingContext(ctx)
    if !ok {
        return "", nil  // metadata 없으면 빈 토큰 (에러 아님)
    }
    tokens := md.Get(bearerHeader)
    if len(tokens) < 1 {
        return "", nil
    }
    if len(tokens) > 1 {
        return "", errors.New("malformed token: multiple tokens found")
    }
    return tokens[0], nil
}
```

### Stream Server Interceptor

```go
func NewStreamServerInterceptor() grpc.StreamServerInterceptor {
    return func(srv any, ss grpc.ServerStream,
                _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
        // 이미 context에 토큰이 있으면 바로 진행
        if token, _ := GetBearerToken(ss.Context()); token != "" {
            return handler(srv, ss)
        }

        // gRPC metadata에서 토큰 추출
        bearerToken, err := ValidTokenFromGRPCMetadata(ss.Context(), Key)
        if err != nil {
            return err
        }

        // 토큰을 context에 주입하여 downstream에서 사용
        return handler(srv, &tokenatedServerStream{
            ServerStream: ss,
            context:      ContextWithBearerToken(ss.Context(), bearerToken),
        })
    }
}
```

### tokenatedServerStream 래퍼

```go
type tokenatedServerStream struct {
    grpc.ServerStream
    context context.Context
}

func (tss *tokenatedServerStream) Context() context.Context {
    return tss.context
}
```

tenancy의 `tenantedServerStream`과 동일한 패턴이다. Stream의 context를 교체하기 위해 래퍼를 사용한다.

### Unary Server Interceptor

```go
func NewUnaryServerInterceptor() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req any,
                _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
        if token, _ := GetBearerToken(ctx); token != "" {
            return handler(ctx, req)
        }

        bearerToken, err := ValidTokenFromGRPCMetadata(ctx, Key)
        if err != nil {
            return nil, err
        }

        return handler(ContextWithBearerToken(ctx, bearerToken), req)
    }
}
```

### Client Unary Interceptor (토큰 전파)

```go
func NewUnaryClientInterceptor() grpc.UnaryClientInterceptor {
    return grpc.UnaryClientInterceptor(func(
        ctx context.Context, method string, req, reply any,
        cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
        opts ...grpc.CallOption,
    ) error {
        var token string
        // 1. gRPC metadata에서 먼저 확인
        token, err := ValidTokenFromGRPCMetadata(ctx, Key)
        if err != nil {
            return err
        }

        // 2. context.Value에서 확인 (HTTP에서 주입된 경우)
        if token == "" {
            bearerToken, ok := GetBearerToken(ctx)
            if ok && bearerToken != "" {
                token = bearerToken
            }
        }

        // 3. outgoing metadata에 추가
        if token != "" {
            ctx = metadata.AppendToOutgoingContext(ctx, Key, token)
        }
        return invoker(ctx, method, req, reply, cc, opts...)
    })
}
```

### Client Stream Interceptor

```go
func NewStreamClientInterceptor() grpc.StreamClientInterceptor {
    return grpc.StreamClientInterceptor(func(
        ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
        method string, streamer grpc.Streamer, opts ...grpc.CallOption,
    ) (grpc.ClientStream, error) {
        var token string
        token, err := ValidTokenFromGRPCMetadata(ctx, Key)
        if err != nil {
            return nil, err
        }
        if token == "" {
            bearerToken, ok := GetBearerToken(ctx)
            if ok && bearerToken != "" {
                token = bearerToken
            }
        }
        if token != "" {
            ctx = metadata.AppendToOutgoingContext(ctx, Key, token)
        }
        return streamer(ctx, desc, cc, method, opts...)
    })
}
```

### 서버-클라이언트 인터셉터 조합

Remote Storage 서버에서 인터셉터가 조합되는 실제 예:

```go
// cmd/remote-storage/app/server.go
func createGRPCServer(...) (*grpc.Server, error) {
    unaryInterceptors := []grpc.UnaryServerInterceptor{
        bearertoken.NewUnaryServerInterceptor(),  // 인증 먼저
    }
    streamInterceptors := []grpc.StreamServerInterceptor{
        bearertoken.NewStreamServerInterceptor(),
    }
    if tm.Enabled {
        unaryInterceptors = append(unaryInterceptors,
            tenancy.NewGuardingUnaryInterceptor(tm))   // 그 다음 테넌시
        streamInterceptors = append(streamInterceptors,
            tenancy.NewGuardingStreamInterceptor(tm))
    }
    // ...
}
```

### 인터셉터 실행 순서

```
요청 수신
    |
    v
bearertoken.UnaryServerInterceptor  (Step 1: 토큰 추출 -> context)
    |
    v
tenancy.GuardingUnaryInterceptor    (Step 2: 테넌트 검증 -> context)
    |
    v
실제 핸들러 (Step 3: context에서 토큰/테넌트 접근)
```

---

## 12. API Key 인증

### API Key Context 연산

소스 경로: `internal/auth/apikey/apikey-context.go`

```go
type apiKeyContextKey struct{}

func GetAPIKey(ctx context.Context) (string, bool) {
    val := ctx.Value(apiKeyContextKey{})
    if val == nil {
        return "", false
    }
    if apiKey, ok := val.(string); ok {
        return apiKey, true
    }
    return "", false
}

func ContextWithAPIKey(ctx context.Context, apiKey string) context.Context {
    if apiKey == "" {
        return ctx
    }
    return context.WithValue(ctx, apiKeyContextKey{}, apiKey)
}
```

### Why: 빈 구조체를 context 키로 사용하는 이유

```go
type apiKeyContextKey struct{}
```

Bearer Token의 `contextKeyType int`과 달리 API Key는 빈 구조체를 사용한다. 두 방식 모두 유효하지만:

| 방식 | 장점 | 사용 예 |
|------|------|---------|
| `type keyType int` | iota로 여러 키 정의 가능 | bearertoken |
| `type keyType struct{}` | 메모리 할당 없음 (zero-size) | apikey |

API Key 패키지는 단일 키만 필요하므로 zero-size 구조체가 적합하다.

### API Key의 현재 상태

API Key 모듈은 현재 context 연산(저장/조회)만 구현되어 있다. HTTP 미들웨어나 gRPC 인터셉터는 아직 구현되지 않았으며, 향후 인증 메커니즘 확장을 위한 기반 코드이다.

---

## 13. 보안 확장 기능

### TLS 설정

Jaeger는 HTTP와 gRPC 엔드포인트 모두에 TLS를 설정할 수 있다:

```yaml
# remote-storage/config.yaml 예시
grpc:
  tls:
    cert_file: /path/to/cert.pem
    key_file: /path/to/key.pem
    ca_file: /path/to/ca.pem
```

ES Index Cleaner와 ES Rollover에서도 TLS를 지원한다:

```go
// cmd/es-index-cleaner/app/flags.go
var tlsFlagsCfg = tlscfg.ClientFlagsConfig{Prefix: "es"}

// --es.tls.ca
// --es.tls.cert
// --es.tls.key
// --es.tls.server-name
// --es.tls.skip-host-verify
```

### Basic Authentication

ES 클라이언트에서 Basic Auth를 지원한다:

```go
// cmd/es-index-cleaner/main.go
func basicAuth(username, password string) string {
    if username == "" || password == "" {
        return ""
    }
    return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}
```

### OpenTelemetry Collector 인증 확장

Jaeger v2는 OpenTelemetry Collector 기반이므로 OTEL의 인증 확장을 활용한다:

| 확장 | 설명 |
|------|------|
| `basicauthextension` | 사용자명/비밀번호 인증 |
| `sigv4authextension` | AWS SigV4 서명 (ES on AWS) |
| `bearertokenauthextension` | Bearer 토큰 인증 |
| `oidcauthextension` | OpenID Connect 인증 |

---

## 14. 멀티테넌시와 인증의 통합

### Remote Storage에서의 통합 예

소스 경로: `cmd/remote-storage/app/server.go`

Remote Storage 서비스는 멀티테넌시와 인증을 모두 사용하는 대표적인 컴포넌트이다:

```go
func NewServer(
    ctx context.Context,
    grpcCfg configgrpc.ServerConfig,
    ts tracestore.Factory,
    ds depstore.Factory,
    tm *tenancy.Manager,    // 테넌시 매니저
    telset telemetry.Settings,
) (*Server, error) {
    // ...
    grpcServer, err := createGRPCServer(ctx, grpcCfg, tm, v2Handler, telset)
    // ...
}
```

### 설정 파일 구조

```go
// cmd/remote-storage/app/config.go
type Config struct {
    GRPC    configgrpc.ServerConfig `mapstructure:"grpc"`
    Tenancy tenancy.Options         `mapstructure:"multi_tenancy"`
    Storage storageconfig.Config    `mapstructure:"storage"`
}
```

### 완전한 요청 흐름

```
클라이언트 (Collector/Query)
    |
    | gRPC 호출
    | metadata:
    |   bearer.token = "eyJhbG..."
    |   x-tenant = "team-a"
    |
    v
Remote Storage Server
    |
    v
[1] bearertoken.UnaryServerInterceptor
    |   metadata에서 "bearer.token" 추출
    |   -> ContextWithBearerToken(ctx, token)
    v
[2] tenancy.GuardingUnaryInterceptor
    |   metadata에서 "x-tenant" 추출
    |   -> Manager.Valid("team-a") 검증
    |   -> WithTenant(ctx, "team-a")
    v
[3] gRPC Handler (grpcstorage.Handler)
    |   ctx에서 tenant/token 접근 가능
    |   -> 스토리지에 tenant 정보 전달
    |   -> 스토리지가 토큰으로 백엔드 인증
    v
[4] Storage Backend (ES, Cassandra 등)
    |   auth.RoundTripper가 HTTP 요청에 Authorization 헤더 주입
    |   "Authorization: Bearer eyJhbG..."
```

### 인증 체인 조합 테이블

| 구성 요소 | HTTP 경로 | gRPC 경로 |
|-----------|-----------|-----------|
| 토큰 추출 | PropagationHandler | Server Interceptor |
| 테넌트 추출 | ExtractTenantHTTPHandler | Guarding Interceptor |
| 토큰 전파 | RoundTripper | Client Interceptor |
| 테넌트 전파 | (해당 없음) | Client Interceptor |

---

## 15. 설계 원칙과 Why 분석

### 1. Context 기반 전파의 이유

Go의 `context.Context`를 사용하여 테넌트/토큰 정보를 전파하는 이유:
- **표준 패턴**: Go 생태계에서 요청 범위(request-scoped) 데이터를 전달하는 표준 방식
- **goroutine 안전**: context는 불변(immutable)이고 동시성에 안전
- **자동 전파**: HTTP 핸들러와 gRPC 인터셉터가 자동으로 context를 전달

### 2. 인터셉터 패턴의 이유

gRPC 인터셉터를 사용하는 이유:
- **관심사 분리**: 인증/테넌시 로직이 비즈니스 로직과 분리
- **재사용성**: 여러 서비스에서 동일한 인터셉터를 공유
- **투명성**: 핸들러는 인증/테넌시 세부사항을 알 필요가 없음

### 3. Guard 인터페이스의 이유

guard 인터페이스를 통한 전략 패턴:
- **런타임 결정**: 설정에 따라 검증 로직을 동적으로 선택
- **성능**: 매 요청마다 설정 체크 대신, 한번 생성된 guard 객체 사용
- **테스트**: 커스텀 guard 구현으로 다양한 시나리오 테스트 가능

### 4. 다층 토큰 조회의 이유

Client Interceptor에서 메타데이터 -> context 순서로 토큰을 조회하는 이유:
- **gRPC-to-gRPC**: 업스트림 gRPC 서비스에서 전달된 metadata가 우선
- **HTTP-to-gRPC**: HTTP 미들웨어가 context에 저장한 토큰을 fallback으로 사용
- **프로토콜 독립성**: 어떤 경로로든 토큰이 전파되면 동작

### 5. 보안 고려사항

| 항목 | 구현 |
|------|------|
| 다중 테넌트 헤더 | `PermissionDenied` 에러 (혼동 공격 방지) |
| 빈 토큰 | context에 저장하지 않음 (null check 대신 존재 여부로 판단) |
| 토큰 파일 권한 | `filepath.Clean`으로 경로 정규화 |
| TLS | HTTP/gRPC 모두 지원 |
| 토큰 재로드 실패 | 이전 유효 토큰 유지 (서비스 중단 방지) |

---

## 요약

Jaeger의 멀티테넌시와 인증 시스템은 다음과 같은 특징을 가진다:

1. **멀티테넌시**: Manager + Guard 패턴으로 선택적 테넌트 격리를 구현하고, HTTP/gRPC 미들웨어를 통해 투명하게 테넌트 정보를 전파한다.

2. **인증**: Bearer Token, API Key 등 다양한 인증 방식을 지원하며, RoundTripper 패턴으로 백엔드 스토리지 접근 시 자동으로 인증 헤더를 주입한다.

3. **통합**: Remote Storage와 같은 서비스에서 멀티테넌시와 인증이 인터셉터 체인을 통해 자연스럽게 통합된다.

4. **확장성**: OpenTelemetry Collector의 인증 확장을 활용하여 basicauth, SigV4 등 다양한 인증 메커니즘을 추가로 지원할 수 있다.

### 핵심 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `internal/tenancy/manager.go` | Manager, Options, guard 인터페이스 |
| `internal/tenancy/context.go` | WithTenant/GetTenant |
| `internal/tenancy/grpc.go` | gRPC 서버/클라이언트 인터셉터 |
| `internal/tenancy/http.go` | HTTP 미들웨어, MetadataAnnotator |
| `internal/tenancy/flags.go` | CLI 플래그 정의 |
| `internal/auth/bearertoken/context.go` | Bearer Token context 연산 |
| `internal/auth/bearertoken/grpc.go` | gRPC Bearer Token 인터셉터 |
| `internal/auth/bearertoken/http.go` | HTTP PropagationHandler |
| `internal/auth/tokenloader.go` | 파일 기반 토큰 로더 |
| `internal/auth/transport.go` | RoundTripper, Method 구조체 |
| `internal/auth/apikey/apikey-context.go` | API Key context 연산 |
| `cmd/remote-storage/app/server.go` | 인터셉터 통합 예시 |
