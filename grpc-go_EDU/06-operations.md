# 06. gRPC-Go 운영 가이드

## 개요

gRPC-Go는 **라이브러리**이므로 독립 실행형 서버가 아니다. 사용자 애플리케이션에 임베드되어
동작한다. 이 문서에서는 gRPC-Go 기반 서비스의 설정, 배포, 모니터링, 트러블슈팅을 다룬다.

---

## 1. 서버 설정

### 기본 서버 생성

```go
import "google.golang.org/grpc"

s := grpc.NewServer(
    grpc.MaxRecvMsgSize(16 * 1024 * 1024),    // 수신 메시지 최대 16MB
    grpc.MaxSendMsgSize(16 * 1024 * 1024),    // 송신 메시지 최대 16MB
    grpc.MaxConcurrentStreams(1000),            // 동시 스트림 제한
    grpc.NumStreamWorkers(32),                 // 워커 풀 크기
    grpc.ConnectionTimeout(30 * time.Second),  // 핸드셰이크 타임아웃
)
```

### 서버 옵션 상세

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| `MaxRecvMsgSize` | 4MB | 수신 메시지 최대 크기 |
| `MaxSendMsgSize` | MaxInt32 | 송신 메시지 최대 크기 |
| `MaxConcurrentStreams` | MaxUint32 | 커넥션당 동시 스트림 수 |
| `NumStreamWorkers` | 0 (무제한) | 스트림 핸들러 워커 풀 크기 |
| `ConnectionTimeout` | 120초 | TLS 핸드셰이크 타임아웃 |
| `WriteBufferSize` | 32KB | 전송 버퍼 크기 |
| `ReadBufferSize` | 32KB | 수신 버퍼 크기 |
| `InitialWindowSize` | 65535 | 스트림 초기 윈도우 |
| `InitialConnWindowSize` | 65535 | 커넥션 초기 윈도우 |
| `SharedWriteBuffer` | false | 전송 버퍼 공유 (메모리 절약) |

### Keepalive 서버 설정

```go
import "google.golang.org/grpc/keepalive"

s := grpc.NewServer(
    grpc.KeepaliveParams(keepalive.ServerParameters{
        MaxConnectionIdle:     15 * time.Minute, // 유휴 커넥션 종료
        MaxConnectionAge:      30 * time.Minute, // 커넥션 최대 수명
        MaxConnectionAgeGrace: 5 * time.Second,  // 종료 유예 기간
        Time:                  5 * time.Minute,  // 핑 간격
        Timeout:               1 * time.Second,  // 핑 타임아웃
    }),
    grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
        MinTime:             5 * time.Minute,  // 클라이언트 핑 최소 간격
        PermitWithoutStream: false,            // 스트림 없이 핑 거부
    }),
)
```

**왜 MaxConnectionAge를 설정하는가?**

DNS 기반 로드 밸런싱에서 서버를 추가/제거할 때, 기존 연결은 DNS 갱신을 반영하지 않는다.
MaxConnectionAge로 주기적으로 연결을 갱신하면 클라이언트가 새 DNS 결과를 반영할 수 있다.

---

## 2. 클라이언트 설정

### 기본 클라이언트 생성

```go
import "google.golang.org/grpc"

conn, err := grpc.NewClient("dns:///my-service:8080",
    grpc.WithTransportCredentials(insecure.NewCredentials()),
    grpc.WithDefaultCallOptions(
        grpc.MaxCallRecvMsgSize(16 * 1024 * 1024),
        grpc.MaxCallSendMsgSize(16 * 1024 * 1024),
    ),
    grpc.WithDefaultServiceConfig(`{
        "loadBalancingConfig": [{"round_robin": {}}],
        "methodConfig": [{
            "name": [{"service": ""}],
            "retryPolicy": {
                "maxAttempts": 3,
                "initialBackoff": "0.1s",
                "maxBackoff": "1s",
                "backoffMultiplier": 2.0,
                "retryableStatusCodes": ["UNAVAILABLE"]
            }
        }]
    }`),
)
```

### 클라이언트 옵션 상세

| 옵션 | 설명 |
|------|------|
| `WithTransportCredentials` | TLS/insecure 설정 (필수) |
| `WithDefaultCallOptions` | 모든 RPC에 적용할 기본 옵션 |
| `WithDefaultServiceConfig` | 리졸버가 설정을 제공하지 않을 때 사용할 기본 서비스 설정 |
| `WithUnaryInterceptor` | Unary 인터셉터 등록 |
| `WithStreamInterceptor` | Stream 인터셉터 등록 |
| `WithChainUnaryInterceptor` | Unary 인터셉터 체이닝 |
| `WithChainStreamInterceptor` | Stream 인터셉터 체이닝 |
| `WithStatsHandler` | 통계 핸들러 등록 |
| `WithIdleTimeout` | 유휴 타임아웃 (기본 30분) |
| `WithConnectParams` | 연결 파라미터 (백오프 등) |

### Keepalive 클라이언트 설정

```go
conn, err := grpc.NewClient(target,
    grpc.WithKeepaliveParams(keepalive.ClientParameters{
        Time:                10 * time.Second, // 핑 간격
        Timeout:             3 * time.Second,  // 핑 응답 타임아웃
        PermitWithoutStream: true,             // 스트림 없어도 핑 전송
    }),
)
```

---

## 3. TLS 설정

### 서버 TLS

```go
creds, err := credentials.NewServerTLSFromFile("server.crt", "server.key")
s := grpc.NewServer(grpc.Creds(creds))
```

### 클라이언트 TLS

```go
creds, err := credentials.NewClientTLSFromFile("ca.crt", "server-name")
conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
```

### mTLS (상호 인증)

```go
// 서버
cert, _ := tls.LoadX509KeyPair("server.crt", "server.key")
certPool := x509.NewCertPool()
certPool.AppendCertsFromPEM(caCert)
creds := credentials.NewTLS(&tls.Config{
    Certificates: []tls.Certificate{cert},
    ClientAuth:   tls.RequireAndVerifyClientCert,
    ClientCAs:    certPool,
})

// 클라이언트
cert, _ := tls.LoadX509KeyPair("client.crt", "client.key")
certPool := x509.NewCertPool()
certPool.AppendCertsFromPEM(caCert)
creds := credentials.NewTLS(&tls.Config{
    Certificates: []tls.Certificate{cert},
    RootCAs:      certPool,
    ServerName:   "my-server",
})
```

---

## 4. 로깅

### 환경 변수 기반 로깅

```bash
# 로그 레벨 설정
export GRPC_GO_LOG_SEVERITY_LEVEL=info    # info, warning, error
export GRPC_GO_LOG_VERBOSITY_LEVEL=99     # 0 ~ 99 (높을수록 상세)
```

### 프로그래밍 방식

```go
import "google.golang.org/grpc/grpclog"

// 커스텀 로거 설정
grpclog.SetLoggerV2(grpclog.NewLoggerV2(os.Stdout, os.Stderr, os.Stderr))
```

### 로그 컴포넌트

gRPC-Go는 컴포넌트별 로거를 제공한다:

```go
// grpclog/component.go
var (
    logger = grpclog.Component("core")        // 핵심 로직
    // transport, channelz, balancer 등 별도 컴포넌트
)
```

---

## 5. 모니터링

### Health Check 프로토콜

```go
import (
    "google.golang.org/grpc/health"
    healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// 서버에 Health 서비스 등록
healthServer := health.NewServer()
healthpb.RegisterHealthServer(s, healthServer)

// 서비스 상태 설정
healthServer.SetServingStatus("my.service.Name", healthpb.HealthCheckResponse_SERVING)
healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING) // 전체 서버

// 상태 변경
healthServer.SetServingStatus("my.service.Name", healthpb.HealthCheckResponse_NOT_SERVING)

// 셧다운 시
healthServer.Shutdown()
```

### Channelz — 런타임 진단

```go
import (
    "google.golang.org/grpc/channelz/service"
)

// Channelz 서비스 등록
channelz.RegisterChannelzServiceToServer(s)

// 이후 grpc_channelz_v1.GetChannel, GetServers 등으로 조회 가능
```

Channelz가 제공하는 정보:

| 리소스 | 제공 정보 |
|--------|----------|
| Channel | 상태, 타겟, 호출 수(성공/실패), 최종 호출 시각 |
| SubChannel | 연결 상태, 주소, 호출 통계 |
| Socket | 바이트 송수신, 스트림 수, 로컬/원격 주소 |
| Server | 리스너 수, 호출 통계 |

### Stats Handler — 메트릭 수집

```go
type myStatsHandler struct{}

func (h *myStatsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
    return ctx
}

func (h *myStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
    switch st := s.(type) {
    case *stats.Begin:
        log.Printf("RPC 시작: %s", st.BeginTime)
    case *stats.InPayload:
        log.Printf("수신 메시지: %d bytes", st.Length)
    case *stats.End:
        log.Printf("RPC 완료: error=%v", st.Error)
    }
}

func (h *myStatsHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
    return ctx
}

func (h *myStatsHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
    switch s.(type) {
    case *stats.ConnBegin:
        log.Println("새 연결")
    case *stats.ConnEnd:
        log.Println("연결 종료")
    }
}

// 사용
s := grpc.NewServer(grpc.StatsHandler(&myStatsHandler{}))
```

### OpenTelemetry 연동

```go
import "google.golang.org/grpc/stats/opentelemetry"

// 서버
s := grpc.NewServer(opentelemetry.ServerOption(opentelemetry.Options{
    MetricsOptions: opentelemetry.MetricsOptions{
        MeterProvider: provider,
    },
}))

// 클라이언트
conn, err := grpc.NewClient(target, opentelemetry.DialOption(opentelemetry.Options{
    MetricsOptions: opentelemetry.MetricsOptions{
        MeterProvider: provider,
    },
}))
```

---

## 6. 서비스 설정 (Service Config)

Service Config는 클라이언트 동작을 서버/DNS에서 제어할 수 있게 한다.

### JSON 형식

```json
{
    "loadBalancingConfig": [
        {"round_robin": {}}
    ],
    "methodConfig": [
        {
            "name": [
                {"service": "helloworld.Greeter", "method": "SayHello"}
            ],
            "waitForReady": true,
            "timeout": "5s",
            "maxRequestMessageBytes": 1048576,
            "maxResponseMessageBytes": 1048576,
            "retryPolicy": {
                "maxAttempts": 3,
                "initialBackoff": "0.1s",
                "maxBackoff": "1s",
                "backoffMultiplier": 2.0,
                "retryableStatusCodes": ["UNAVAILABLE", "RESOURCE_EXHAUSTED"]
            }
        }
    ],
    "healthCheckConfig": {
        "serviceName": "helloworld.Greeter"
    }
}
```

### DNS TXT 레코드로 Service Config 제공

```
# DNS TXT 레코드 (서비스 이름: _grpc_config.my-service.example.com)
"grpc_config=[{\"serviceConfig\":{\"loadBalancingConfig\":[{\"round_robin\":{}}]}}]"
```

### methodConfig 필드

| 필드 | 설명 |
|------|------|
| `name` | 적용 대상 서비스/메서드 (빈 서비스명 = 전체 적용) |
| `waitForReady` | Ready 상태까지 대기 |
| `timeout` | RPC 타임아웃 |
| `maxRequestMessageBytes` | 요청 최대 크기 |
| `maxResponseMessageBytes` | 응답 최대 크기 |
| `retryPolicy` | 재시도 정책 |

---

## 7. 재시도 (Retry)

### 재시도 정책

```
retryPolicy:
  maxAttempts: 3                         # 최대 시도 횟수
  initialBackoff: "0.1s"                 # 초기 대기
  maxBackoff: "1s"                       # 최대 대기
  backoffMultiplier: 2.0                 # 대기 배율
  retryableStatusCodes: ["UNAVAILABLE"]  # 재시도 대상 코드
```

### 재시도 동작

```
RPC 전송 ──▶ UNAVAILABLE 응답
              │
              ├── 시도 1/3 → 0.1s ± jitter 대기
              │
              ├── 재전송 ──▶ UNAVAILABLE 응답
              │              │
              │              ├── 시도 2/3 → 0.2s ± jitter 대기
              │              │
              │              └── 재전송 ──▶ OK
              │                             └── 성공 반환
              │
              └── maxAttempts 초과 → 마지막 에러 반환
```

### 재시도 스로틀링

서버가 과부하일 때 재시도가 부하를 가중시키는 것을 방지:

```json
{
    "retryThrottling": {
        "maxTokens": 10,
        "tokenRatio": 0.1
    }
}
```

- 성공 시: `tokenRatio`만큼 토큰 추가
- 재시도 시: 1 토큰 차감
- 토큰이 `maxTokens/2` 미만: 재시도 중단

---

## 8. 배포 패턴

### Kubernetes에서 gRPC 로드 밸런싱

```
방법 1: 클라이언트 사이드 밸런싱 (headless service)
──────────────────────────────────────────────
  Client (round_robin)
    ├── dns:///my-svc.ns.svc.cluster.local:8080
    ├── Pod 1 (10.0.0.1:8080)
    ├── Pod 2 (10.0.0.2:8080)
    └── Pod 3 (10.0.0.3:8080)

  # Kubernetes Service (headless)
  apiVersion: v1
  kind: Service
  spec:
    clusterIP: None    # headless → DNS가 Pod IP 직접 반환
    ports:
    - port: 8080


방법 2: L7 프록시 (Envoy/Istio)
──────────────────────────────────────────────
  Client ──▶ Envoy Sidecar ──▶ Server Pods
           (HTTP/2 인식 LB)

  # Istio DestinationRule
  trafficPolicy:
    loadBalancer:
      simple: ROUND_ROBIN


방법 3: xDS 기반 (프록시리스 서비스 메시)
──────────────────────────────────────────────
  Client (xDS balancer) ──▶ xDS 서버 (Istiod)
    │                         └── 엔드포인트 목록 제공
    ├── Pod 1
    ├── Pod 2
    └── Pod 3
```

### Graceful Shutdown (Kubernetes)

```go
func main() {
    s := grpc.NewServer(...)
    // 서비스 등록 ...

    // SIGTERM 수신 시 graceful shutdown
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

    go func() {
        <-sigCh
        // 1. Health 상태를 NOT_SERVING으로 변경
        healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

        // 2. 새 요청 거부, 기존 요청 완료 대기
        s.GracefulStop()
    }()

    lis, _ := net.Listen("tcp", ":8080")
    s.Serve(lis)
}
```

**Kubernetes terminationGracePeriodSeconds와 맞추기:**

```yaml
spec:
  terminationGracePeriodSeconds: 30  # GracefulStop 완료 대기
  containers:
  - name: grpc-server
    livenessProbe:
      grpc:
        port: 8080
      periodSeconds: 10
    readinessProbe:
      grpc:
        port: 8080
      periodSeconds: 5
```

---

## 9. 트러블슈팅

### 자주 발생하는 에러

#### "transport is closing"

```
code = Unavailable desc = transport is closing
```

**원인:**
1. 서버 종료 (GracefulStop/Stop)
2. Keepalive 정책으로 연결 종료
3. TLS 핸드셰이크 실패
4. 프록시가 연결 종료

**해결:**
```bash
# 양쪽 로깅 활성화
export GRPC_GO_LOG_SEVERITY_LEVEL=info
export GRPC_GO_LOG_VERBOSITY_LEVEL=99
```

#### "connection refused"

```
code = Unavailable desc = connection error: ... connection refused
```

**원인:** 서버가 해당 포트에서 리스닝하지 않음

**해결:** 서버 주소/포트 확인, 방화벽 규칙 확인

#### "deadline exceeded"

```
code = DeadlineExceeded desc = context deadline exceeded
```

**원인:**
1. 서버 처리 시간이 클라이언트 데드라인 초과
2. 네트워크 지연
3. 클라이언트 데드라인이 너무 짧음

**해결:**
```go
// 적절한 타임아웃 설정
ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
defer cancel()
reply, err := client.SayHello(ctx, req)
```

#### "no transport security set"

```
grpc: no transport security set (use grpc.WithTransportCredentials(...))
```

**원인:** 클라이언트에 TransportCredentials를 설정하지 않음

**해결:**
```go
// TLS
conn, _ := grpc.NewClient(target, grpc.WithTransportCredentials(creds))

// 또는 비보안 (개발용만)
conn, _ := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
```

#### "too many pings"

```
code = Unavailable desc = transport: received GOAWAY with ENHANCE_YOUR_CALM
```

**원인:** 클라이언트 핑 간격이 서버 EnforcementPolicy.MinTime보다 짧음

**해결:**
```go
// 서버: 핑 간격 허용 범위 완화
grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
    MinTime: 10 * time.Second,
    PermitWithoutStream: true,
})

// 클라이언트: 핑 간격 늘리기
grpc.WithKeepaliveParams(keepalive.ClientParameters{
    Time: 30 * time.Second,
})
```

### 진단 도구

#### Channelz 웹 UI

```bash
# grpcdebug 도구 (별도 설치)
grpcdebug localhost:8080 channelz channels
grpcdebug localhost:8080 channelz channel <id>
grpcdebug localhost:8080 channelz subchannel <id>
grpcdebug localhost:8080 channelz socket <id>
```

#### Server Reflection

```go
import "google.golang.org/grpc/reflection"

// 서버에 리플렉션 등록
reflection.Register(s)
```

```bash
# grpcurl로 서비스 조회
grpcurl -plaintext localhost:8080 list
grpcurl -plaintext localhost:8080 describe helloworld.Greeter
grpcurl -plaintext -d '{"name":"world"}' localhost:8080 helloworld.Greeter/SayHello
```

---

## 10. 성능 튜닝

### 윈도우 크기 조정

```go
// 고대역폭 링크
s := grpc.NewServer(
    grpc.InitialWindowSize(1 << 20),      // 스트림 윈도우 1MB
    grpc.InitialConnWindowSize(1 << 20),  // 커넥션 윈도우 1MB
)
```

### 버퍼 크기 조정

```go
// 대량 데이터 전송
s := grpc.NewServer(
    grpc.WriteBufferSize(64 * 1024),  // 쓰기 버퍼 64KB
    grpc.ReadBufferSize(64 * 1024),   // 읽기 버퍼 64KB
    grpc.SharedWriteBuffer(true),     // 쓰기 버퍼 재사용
)
```

### 메시지 압축

```go
import "google.golang.org/grpc/encoding/gzip"

// 클라이언트: 요청 압축
reply, err := client.SayHello(ctx, req, grpc.UseCompressor(gzip.Name))

// 서버: 자동 압축 해제 (gzip import만 하면 됨)
import _ "google.golang.org/grpc/encoding/gzip"
```

### 워커 풀 사용

```go
// 대규모 서버: 워커 풀로 goroutine 생성 오버헤드 감소
s := grpc.NewServer(grpc.NumStreamWorkers(uint32(runtime.NumCPU())))
```

### 벤치마크

```bash
cd benchmark/benchmain
go run main.go \
    -benchtime=10s \
    -workloads=unary \
    -reqSizeBytes=100 \
    -respSizeBytes=100 \
    -maxConcurrentCalls=100
```

---

## 11. 보안 모범 사례

| 항목 | 권장 설정 |
|------|----------|
| TLS | 프로덕션에서 항상 TLS 사용 |
| mTLS | 서비스 간 통신에서 상호 인증 |
| 메시지 크기 | `MaxRecvMsgSize`를 적절히 제한 (기본 4MB) |
| 동시 스트림 | `MaxConcurrentStreams`로 리소스 보호 |
| 인터셉터 | 인증/인가 인터셉터 추가 |
| Keepalive | 서버에 EnforcementPolicy 설정 |
| 데드라인 | 모든 RPC에 적절한 데드라인 설정 |
| 재시도 | RetryThrottling으로 과부하 방지 |

---

## 12. 환경 변수 요약

| 환경 변수 | 설명 | 기본값 |
|-----------|------|--------|
| `GRPC_GO_LOG_SEVERITY_LEVEL` | 로그 레벨 (info/warning/error) | error |
| `GRPC_GO_LOG_VERBOSITY_LEVEL` | 상세도 (0~99) | 0 |
| `GRPC_GO_RETRY` | 재시도 활성화 | on |
| `GRPC_XDS_BOOTSTRAP` | xDS 부트스트랩 파일 경로 | - |
| `GRPC_XDS_BOOTSTRAP_CONFIG` | xDS 부트스트랩 JSON 직접 지정 | - |
