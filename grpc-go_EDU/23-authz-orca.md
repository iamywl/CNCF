# 23. Authz(인가) & ORCA(백엔드 로드 보고) 심화

## 목차
1. [개요](#1-개요)
2. [Authz 아키텍처](#2-authz-아키텍처)
3. [인가 정책 JSON 구조](#3-인가-정책-json-구조)
4. [RBAC 정책 변환 엔진](#4-rbac-정책-변환-엔진)
5. [StaticInterceptor 동작 원리](#5-staticinterceptor-동작-원리)
6. [FileWatcherInterceptor와 핫 리로드](#6-filewatcherinterceptor와-핫-리로드)
7. [감사 로깅(Audit Logging)](#7-감사-로깅audit-logging)
8. [ORCA 아키텍처](#8-orca-아키텍처)
9. [Per-RPC 메트릭 보고 (CallMetrics)](#9-per-rpc-메트릭-보고-callmetrics)
10. [Out-of-Band 메트릭 보고 (OOB Service)](#10-out-of-band-메트릭-보고-oob-service)
11. [ServerMetrics 데이터 모델](#11-servermetrics-데이터-모델)
12. [설계 철학과 Why](#12-설계-철학과-why)

---

## 1. 개요

**Authz**는 gRPC 서버에 정책 기반 접근 제어를 제공하며, **ORCA**(Open Request Cost Aggregation)는 백엔드 서버가 자신의 부하 상태를 로드 밸런서에 보고하는 메커니즘이다.

### 핵심 소스 경로

| 컴포넌트 | 소스 경로 |
|----------|----------|
| Authz 인터셉터 | `authz/grpc_authz_server_interceptors.go` |
| RBAC 변환기 | `authz/rbac_translator.go` |
| 감사 로거 인터페이스 | `authz/audit/audit_logger.go` |
| stdout 감사 로거 | `authz/audit/stdout/stdout_logger.go` |
| ORCA 메인 | `orca/orca.go` |
| ORCA 서비스 | `orca/service.go` |
| ORCA CallMetrics | `orca/call_metrics.go` |
| ORCA ServerMetrics | `orca/server_metrics.go` |
| ORCA Producer | `orca/producer.go` |

---

## 2. Authz 아키텍처

### 2.1 전체 구조

```
┌─────────────────────────────────────────────────────────┐
│                    gRPC Server                           │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │            Authz Interceptor Layer                  │  │
│  │                                                     │  │
│  │  Incoming RPC                                       │  │
│  │      │                                              │  │
│  │      ▼                                              │  │
│  │  ┌─────────────────────────────────────────────┐   │  │
│  │  │  StaticInterceptor / FileWatcherInterceptor  │   │  │
│  │  │                                              │   │  │
│  │  │  ┌─────────────────────────────────────────┐ │   │  │
│  │  │  │         ChainEngine                      │ │   │  │
│  │  │  │                                          │ │   │  │
│  │  │  │  DENY RBAC  ──→ ALLOW RBAC              │ │   │  │
│  │  │  │  (있으면)         (필수)                   │ │   │  │
│  │  │  │                                          │ │   │  │
│  │  │  │  IsAuthorized(ctx) → allow/deny/error    │ │   │  │
│  │  │  └─────────────────────────────────────────┘ │   │  │
│  │  └─────────────────────────────────────────────┘   │  │
│  │      │                                              │  │
│  │      ▼ (허용된 경우만)                               │  │
│  │  ┌─────────────────┐                                │  │
│  │  │ Service Handler  │                                │  │
│  │  └─────────────────┘                                │  │
│  └────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

### 2.2 두 가지 인터셉터

| 타입 | 클래스 | 특징 |
|------|--------|------|
| 정적 | `StaticInterceptor` | 생성 시 정책 고정, 변경 불가 |
| 파일 감시 | `FileWatcherInterceptor` | 파일 변경 감지 시 정책 자동 리로드 |

---

## 3. 인가 정책 JSON 구조

### 3.1 정책 모델

```go
type authorizationPolicy struct {
    Name                string
    DenyRules           []rule              `json:"deny_rules"`
    AllowRules          []rule              `json:"allow_rules"`
    AuditLoggingOptions auditLoggingOptions `json:"audit_logging_options"`
}

type rule struct {
    Name    string
    Source  peer
    Request request
}

type peer struct {
    Principals []string  // TLS 인증서의 Subject Alternative Name
}

type request struct {
    Paths   []string   // RPC 경로 (예: "/package.Service/Method")
    Headers []header   // 메타데이터 헤더 매칭
}
```

### 3.2 정책 JSON 예시

```json
{
    "name": "my-policy",
    "deny_rules": [
        {
            "name": "block-internal",
            "source": {
                "principals": ["spiffe://untrusted.example.com/*"]
            },
            "request": {
                "paths": ["/admin.Service/*"]
            }
        }
    ],
    "allow_rules": [
        {
            "name": "allow-users",
            "source": {
                "principals": ["spiffe://example.com/*"]
            },
            "request": {
                "paths": ["/api.Service/*"],
                "headers": [
                    {
                        "key": "x-role",
                        "values": ["admin", "editor"]
                    }
                ]
            }
        }
    ],
    "audit_logging_options": {
        "audit_condition": "ON_DENY_AND_ALLOW",
        "audit_loggers": [
            {
                "name": "stdout_logger",
                "config": {},
                "is_optional": false
            }
        ]
    }
}
```

### 3.3 와일드카드 패턴 매칭

```go
func getStringMatcher(value string) *v3matcherpb.StringMatcher {
    switch {
    case value == "*":
        return &v3matcherpb.StringMatcher{
            MatchPattern: &v3matcherpb.StringMatcher_SafeRegex{
                SafeRegex: &v3matcherpb.RegexMatcher{Regex: ".+"}},
        }
    case strings.HasSuffix(value, "*"):
        prefix := strings.TrimSuffix(value, "*")
        return &v3matcherpb.StringMatcher{
            MatchPattern: &v3matcherpb.StringMatcher_Prefix{Prefix: prefix},
        }
    case strings.HasPrefix(value, "*"):
        suffix := strings.TrimPrefix(value, "*")
        return &v3matcherpb.StringMatcher{
            MatchPattern: &v3matcherpb.StringMatcher_Suffix{Suffix: suffix},
        }
    default:
        return &v3matcherpb.StringMatcher{
            MatchPattern: &v3matcherpb.StringMatcher_Exact{Exact: value},
        }
    }
}
```

| 패턴 | 매칭 타입 | 예시 |
|------|----------|------|
| `*` | 정규식 `.+` | 모든 값 |
| `prefix*` | Prefix | `spiffe://example.com/*` |
| `*suffix` | Suffix | `*.example.com` |
| `exact` | Exact | `spiffe://example.com/my-service` |

---

## 4. RBAC 정책 변환 엔진

### 4.1 translatePolicy 흐름

```go
func translatePolicy(policyStr string) ([]*v3rbacpb.RBAC, string, error) {
    policy := &authorizationPolicy{}
    json.Decode(policyStr, policy)

    rbacs := make([]*v3rbacpb.RBAC, 0, 2)

    if len(policy.DenyRules) > 0 {
        denyPolicies := parseRules(policy.DenyRules, policy.Name)
        denyRBAC := &v3rbacpb.RBAC{
            Action:   v3rbacpb.RBAC_DENY,
            Policies: denyPolicies,
        }
        rbacs = append(rbacs, denyRBAC)
    }

    allowPolicies := parseRules(policy.AllowRules, policy.Name)
    allowRBAC := &v3rbacpb.RBAC{
        Action:   v3rbacpb.RBAC_ALLOW,
        Policies: allowPolicies,
    }
    return append(rbacs, allowRBAC), policy.Name, nil
}
```

### 4.2 Envoy RBAC 프로토 변환

SDK JSON 정책이 Envoy의 RBAC protobuf로 변환되는 과정:

```
SDK JSON Policy
    │
    ├─ DenyRules → v3rbacpb.RBAC (Action: DENY)
    │   └─ rule → v3rbacpb.Policy
    │       ├─ Principals (소스 인증)
    │       │   └─ principal → Authenticated{PrincipalName: StringMatcher}
    │       └─ Permissions (요청 매칭)
    │           ├─ paths → UrlPath{PathMatcher{StringMatcher}}
    │           └─ headers → Header{HeaderMatcher}
    │
    └─ AllowRules → v3rbacpb.RBAC (Action: ALLOW)
        └─ (동일 구조)
```

### 4.3 DENY → ALLOW 체인

```
들어오는 RPC
    │
    ▼
[DENY RBAC 평가]
    │
    ├─ 매칭됨 → PermissionDenied (거부)
    │
    └─ 매칭 안됨 → 다음 단계로
        │
        ▼
[ALLOW RBAC 평가]
    │
    ├─ 매칭됨 → 허용 (핸들러 실행)
    │
    └─ 매칭 안됨 → PermissionDenied (거부)
```

**왜 DENY를 먼저 평가하는가?** 보안 원칙상 "deny first" 접근이 더 안전하다. 허용 규칙에 해당하더라도 명시적 거부 규칙이 있으면 항상 거부해야 한다.

---

## 5. StaticInterceptor 동작 원리

### 5.1 Unary 인터셉터

```go
func (i *StaticInterceptor) UnaryInterceptor(
    ctx context.Context, req any,
    _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
    err := i.engines.IsAuthorized(ctx)
    if err != nil {
        if status.Code(err) == codes.PermissionDenied {
            return nil, status.Errorf(codes.PermissionDenied,
                "unauthorized RPC request rejected")
        }
        return nil, err
    }
    return handler(ctx, req)
}
```

### 5.2 Stream 인터셉터

```go
func (i *StaticInterceptor) StreamInterceptor(
    srv any, ss grpc.ServerStream,
    _ *grpc.StreamServerInfo, handler grpc.StreamHandler,
) error {
    err := i.engines.IsAuthorized(ss.Context())
    if err != nil {
        if status.Code(err) == codes.PermissionDenied {
            return status.Errorf(codes.PermissionDenied,
                "unauthorized RPC request rejected")
        }
        return err
    }
    return handler(srv, ss)
}
```

### 5.3 컨텍스트에서 인증 정보 추출

`IsAuthorized`는 컨텍스트에서 다음 정보를 추출하여 RBAC 정책과 매칭:

| 정보 | 소스 | 용도 |
|------|------|------|
| Peer 인증서 | `peer.FromContext(ctx)` | Principal 매칭 |
| RPC 메서드 | `grpc.Method(ctx)` | Path 매칭 |
| 메타데이터 | `metadata.FromIncomingContext(ctx)` | Header 매칭 |

---

## 6. FileWatcherInterceptor와 핫 리로드

### 6.1 초기화와 백그라운드 갱신

```go
func NewFileWatcher(file string, duration time.Duration) (*FileWatcherInterceptor, error) {
    i := &FileWatcherInterceptor{
        policyFile:      file,
        refreshDuration: duration,
    }
    if err := i.updateInternalInterceptor(); err != nil {
        return nil, err  // 초기 로드 실패 시 에러
    }
    ctx, cancel := context.WithCancel(context.Background())
    i.cancel = cancel
    go i.run(ctx)  // 백그라운드 갱신 루프 시작
    return i, nil
}
```

### 6.2 정책 리로드 루프

```go
func (i *FileWatcherInterceptor) run(ctx context.Context) {
    ticker := time.NewTicker(i.refreshDuration)
    for {
        if err := i.updateInternalInterceptor(); err != nil {
            logger.Warningf("authorization policy reload status err: %v", err)
        }
        select {
        case <-ctx.Done():
            ticker.Stop()
            return
        case <-ticker.C:
        }
    }
}
```

### 6.3 원자적 교체

```go
func (i *FileWatcherInterceptor) updateInternalInterceptor() error {
    policyContents, err := os.ReadFile(i.policyFile)
    if err != nil {
        return err
    }
    if bytes.Equal(i.policyContents, policyContents) {
        return nil  // 내용 변경 없으면 스킵
    }
    i.policyContents = policyContents
    interceptor, err := NewStatic(string(policyContents))
    if err != nil {
        return err  // 파싱 실패 시 기존 정책 유지
    }
    atomic.StorePointer(&i.internalInterceptor, unsafe.Pointer(interceptor))
    return nil
}
```

**왜 `unsafe.Pointer`와 `atomic`을 사용하는가?** 정책 교체는 백그라운드 고루틴에서 일어나고, RPC 처리는 여러 고루틴에서 동시에 일어난다. `atomic.StorePointer`/`LoadPointer`는 락 없이 안전하게 포인터를 교체/읽기 할 수 있어 RPC 처리 성능에 영향을 주지 않는다.

### 6.4 안전한 리로드 보장

```
┌──────────────────────────────────────────────┐
│            FileWatcher 안전성 보장             │
│                                               │
│  1. 파일 읽기 실패 → 기존 정책 유지 (에러 로그) │
│  2. 파싱 실패 → 기존 정책 유지 (에러 로그)      │
│  3. 내용 동일 → 불필요한 교체 방지              │
│  4. 원자적 교체 → RPC 처리 중단 없음            │
│  5. Close() → 갱신 루프 중단 + 리소스 정리      │
└──────────────────────────────────────────────┘
```

---

## 7. 감사 로깅(Audit Logging)

### 7.1 AuditCondition 매핑

| 정책 조건 | DENY RBAC | ALLOW RBAC |
|----------|-----------|------------|
| NONE | NONE | NONE |
| ON_DENY | ON_DENY | ON_DENY |
| ON_ALLOW | NONE | ON_ALLOW |
| ON_DENY_AND_ALLOW | ON_DENY | ON_DENY_AND_ALLOW |

```go
func toDenyCondition(condition v3rbacpb.RBAC_AuditLoggingOptions_AuditCondition) ... {
    switch condition {
    case NONE:       return NONE
    case ON_DENY:    return ON_DENY
    case ON_ALLOW:   return NONE        // DENY RBAC에서는 감사 안함
    case ON_DENY_AND_ALLOW: return ON_DENY  // DENY 결과만 감사
    }
}
```

### 7.2 감사 로거 설정

```go
type auditLoggingOptions struct {
    AuditCondition string         `json:"audit_condition"`
    AuditLoggers   []*auditLogger `json:"audit_loggers"`
}

type auditLogger struct {
    Name       string           `json:"name"`
    Config     *structpb.Struct `json:"config"`
    IsOptional bool             `json:"is_optional"`
}
```

---

## 8. ORCA 아키텍처

### 8.1 ORCA란?

**Open Request Cost Aggregation**은 백엔드 서버가 자신의 부하 상태(CPU, 메모리, 요청 비용 등)를 L7 로드 밸런서에 보고하는 [CNCF xDS 표준](https://github.com/cncf/xds/blob/main/xds/service/orca/v3/orca.proto)이다.

### 8.2 두 가지 보고 경로

```
┌─────────────────────────────────────────────────────────┐
│                     Backend Server                       │
│                                                          │
│  ┌───────────────────┐    ┌────────────────────────┐    │
│  │  Per-RPC Metrics   │    │  OOB (Out-of-Band)     │    │
│  │  (CallMetrics)     │    │  Metrics (ORCA Service) │    │
│  │                    │    │                         │    │
│  │  트레일러 메타데이터 │    │  전용 스트리밍 RPC      │    │
│  │  에 포함            │    │  주기적 푸시             │    │
│  │                    │    │                         │    │
│  │  요청별 비용/지연   │    │  서버 전체 부하          │    │
│  └────────┬───────────┘    └──────────┬─────────────┘    │
│           │                           │                  │
│           ▼                           ▼                  │
│  ┌──────────────────────────────────────────────────┐   │
│  │     gRPC Client (Load Balancer)                   │   │
│  │     - 트레일러에서 Per-RPC 메트릭 파싱             │   │
│  │     - OOB 스트림에서 서버 부하 수신                │   │
│  │     - 밸런싱 결정에 활용                           │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

---

## 9. Per-RPC 메트릭 보고 (CallMetrics)

### 9.1 CallMetricsServerOption

```go
func CallMetricsServerOption(smp ServerMetricsProvider) grpc.ServerOption {
    return joinServerOptions(
        grpc.ChainUnaryInterceptor(unaryInt(smp)),
        grpc.ChainStreamInterceptor(streamInt(smp)),
    )
}
```

### 9.2 Unary 인터셉터 흐름

```go
func unaryInt(smp ServerMetricsProvider) func(...) (any, error) {
    return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler) (any, error) {
        // 1. recorderWrapper 생성 (lazy initialization)
        rw := &recorderWrapper{smp: smp}
        ctxWithRecorder := newContextWithRecorderWrapper(ctx, rw)

        // 2. 핸들러 실행 (사용자 코드)
        resp, err := handler(ctxWithRecorder, req)

        // 3. 메트릭이 기록되었으면 트레일러에 설정
        if rw.r != nil {
            rw.setTrailerMetadata(ctx)
        }
        return resp, err
    }
}
```

### 9.3 지연 초기화(Lazy Initialization)

```go
type recorderWrapper struct {
    once sync.Once
    r    CallMetricsRecorder
    smp  ServerMetricsProvider
}

func (rw *recorderWrapper) recorder() CallMetricsRecorder {
    rw.once.Do(func() {
        rw.r = newServerMetricsRecorder()
    })
    return rw.r
}
```

**왜 지연 초기화인가?** 모든 RPC가 메트릭을 기록하는 것은 아니다. `CallMetricsRecorderFromContext`를 호출하지 않는 핸들러에서는 레코더를 할당하지 않아 메모리를 절약한다.

### 9.4 트레일러를 통한 메트릭 전달

```go
func (rw *recorderWrapper) setTrailerMetadata(ctx context.Context) {
    var sm *ServerMetrics
    if rw.smp != nil {
        sm = rw.smp.ServerMetrics()
        sm.merge(rw.r.ServerMetrics())  // 서버 전체 + per-RPC 병합
    } else {
        sm = rw.r.ServerMetrics()
    }

    b, _ := proto.Marshal(sm.toLoadReportProto())
    grpc.SetTrailer(ctx,
        metadata.Pairs(internal.TrailerMetadataKey, string(b)))
}
```

메트릭은 `endpoint-load-metrics-bin` 키로 트레일러 메타데이터에 설정된다.

### 9.5 클라이언트 측 파싱

```go
// orca/orca.go
type loadParser struct{}

func (loadParser) Parse(md metadata.MD) any {
    lr, err := internal.ToLoadReport(md)
    return lr
}

func init() {
    balancerload.SetParser(loadParser{})
}
```

**왜 `init()`에서 등록하는가?** grpc 패키지에서 orca를 직접 import하면 순환 의존이 발생한다. `balancerload.SetParser`를 통해 orca 패키지가 import될 때 자동으로 파서가 등록되는 방식으로 이 문제를 해결한다.

---

## 10. Out-of-Band 메트릭 보고 (OOB Service)

### 10.1 ORCA Service 구조

```go
type Service struct {
    v3orcaservicegrpc.UnimplementedOpenRcaServiceServer
    minReportingInterval time.Duration
    smProvider           ServerMetricsProvider
}
```

### 10.2 StreamCoreMetrics RPC

```go
func (s *Service) StreamCoreMetrics(
    req *v3orcaservicepb.OrcaLoadReportRequest,
    stream v3orcaservicegrpc.OpenRcaService_StreamCoreMetricsServer,
) error {
    ticker := time.NewTicker(s.determineReportingInterval(req))
    defer ticker.Stop()

    for {
        if err := s.sendMetricsResponse(stream); err != nil {
            return err
        }
        select {
        case <-stream.Context().Done():
            return status.Error(codes.Canceled, "Stream has ended.")
        case <-ticker.C:
        }
    }
}
```

### 10.3 보고 주기 결정

```go
func (s *Service) determineReportingInterval(
    req *v3orcaservicepb.OrcaLoadReportRequest,
) time.Duration {
    if req.GetReportInterval() == nil {
        return s.minReportingInterval  // 기본: 30초
    }
    dur := req.GetReportInterval().AsDuration()
    if dur < s.minReportingInterval {
        return s.minReportingInterval  // 최소값 보장
    }
    return dur
}
```

### 10.4 최소 보고 주기

```go
const minReportingInterval = 30 * time.Second
```

**왜 30초가 최소인가?** OOB 메트릭은 서버의 전반적인 부하 상태를 보고하므로 너무 자주 보고하면 불필요한 네트워크 오버헤드가 발생한다. 30초는 부하 변화를 반영하면서도 오버헤드를 최소화하는 균형점이다.

---

## 11. ServerMetrics 데이터 모델

### 11.1 ServerMetrics 구조

```go
type ServerMetrics struct {
    CPUUtilization float64
    MemUtilization float64
    AppUtilization float64
    QPS            float64
    EPS            float64  // Errors Per Second
    Utilization    map[string]float64
    RequestCost    map[string]float64
    NamedMetrics   map[string]float64
}
```

### 11.2 CallMetricsRecorder 인터페이스

```go
type CallMetricsRecorder interface {
    ServerMetricsRecorder

    SetRequestCost(name string, val float64)
    DeleteRequestCost(name string)

    SetNamedMetric(name string, val float64)
    DeleteNamedMetric(name string)
}
```

### 11.3 사용자 코드에서의 활용

```go
func (s *myServer) SayHello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloReply, error) {
    // Per-RPC 메트릭 레코더 가져오기
    recorder := orca.CallMetricsRecorderFromContext(ctx)
    if recorder != nil {
        recorder.SetCPUUtilization(0.75)
        recorder.SetMemUtilization(0.60)
        recorder.SetRequestCost("db_queries", 5.0)
        recorder.SetNamedMetric("cache_hit_ratio", 0.85)
    }
    return &pb.HelloReply{Message: "Hello " + req.Name}, nil
}
```

---

## 12. 설계 철학과 Why

### 12.1 Authz 설계 원칙

1. **Envoy RBAC 재사용**: 자체 규칙 엔진 대신 검증된 Envoy RBAC 프로토 활용
2. **핫 리로드**: 서버 재시작 없이 정책 변경 가능 (FileWatcher)
3. **감사 로깅 분리**: 접근 제어와 감사 관심사 분리
4. **gRPC A43 준수**: [proposal A43](https://github.com/grpc/proposal/blob/master/A43-grpc-authorization-api.md) 스펙 구현

### 12.2 ORCA 설계 원칙

1. **이중 경로**: Per-RPC(즉각적) + OOB(주기적) 이중 보고로 정밀도와 효율 균형
2. **지연 초기화**: 메트릭을 사용하지 않는 RPC에서는 할당 없음
3. **역방향 의존성 회피**: `init()` + `SetParser()`로 순환 import 방지
4. **CNCF xDS 표준**: ORCA는 CNCF의 표준으로 Envoy와 호환
5. **gRPC A51 준수**: [proposal A51](https://github.com/grpc/proposal/blob/master/A51-custom-backend-metrics.md)

### 12.3 Authz와 ORCA의 보완성

```
┌──────────────────────────────────────────────────────┐
│            보안 + 성능 최적화 스택                      │
│                                                       │
│  1. Authz: "이 요청을 처리해야 하는가?"                 │
│     → 정책 기반 접근 제어                               │
│     → 비인가 요청은 여기서 차단                         │
│                                                       │
│  2. ORCA: "이 요청을 어디서 처리해야 하는가?"            │
│     → 부하 기반 라우팅 결정                             │
│     → 가장 여유 있는 백엔드로 전달                      │
│                                                       │
│  결합 효과: 인가된 요청만 최적의 백엔드로 라우팅         │
└──────────────────────────────────────────────────────┘
```

---

*본 문서 위치: `grpc-go_EDU/23-authz-orca.md`*
*소스코드 기준: `grpc-go/authz/`, `grpc-go/orca/`*
