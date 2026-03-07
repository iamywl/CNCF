# HotROD 데모 애플리케이션

## 목차
1. [개요](#1-개요)
2. [아키텍처](#2-아키텍처)
3. [서비스 구성](#3-서비스-구성)
4. [Frontend 서비스](#4-frontend-서비스)
5. [Customer 서비스](#5-customer-서비스)
6. [Driver 서비스](#6-driver-서비스)
7. [Route 서비스](#7-route-서비스)
8. [트레이싱 초기화와 전파](#8-트레이싱-초기화와-전파)
9. [의도적 성능 문제와 진단](#9-의도적-성능-문제와-진단)
10. [공유 유틸리티 패키지](#10-공유-유틸리티-패키지)
11. [Web UI](#11-web-ui)
12. [Docker Compose 배포](#12-docker-compose-배포)
13. [Kubernetes 배포](#13-kubernetes-배포)
14. [CLI 플래그와 설정](#14-cli-플래그와-설정)
15. [트레이싱 개념 시연 분석](#15-트레이싱-개념-시연-분석)

---

## 1. 개요

HotROD(Hot Rides On Demand)는 Jaeger 프로젝트에서 제공하는 공식 데모 애플리케이션이다. 가상의 차량 호출(ride-sharing) 서비스를 구현하여 분산 트레이싱의 핵심 개념을 실제로 체험할 수 있도록 설계되었다.

### 핵심 특징

| 특징 | 설명 |
|------|------|
| 4개 마이크로서비스 | frontend, customer, driver, route |
| 혼합 프로토콜 | HTTP (frontend, customer, route) + gRPC (driver) |
| 의도적 성능 문제 | 뮤텍스 경합, 워커 풀 제한 등 버그를 CLI로 수정 가능 |
| OpenTelemetry SDK | Jaeger v1.42.0부터 OTEL SDK로 전환 |
| Baggage 전파 | customer/session 정보를 서비스 간 전파 |
| 메트릭 | expvar + Prometheus 포맷 지원 |

### 소스 코드 경로

```
examples/hotrod/
├── main.go                    # 진입점
├── cmd/                       # cobra 서브커맨드
│   ├── root.go                # 루트 커맨드, 초기화
│   ├── all.go                 # 4개 서비스 동시 실행
│   ├── frontend.go            # frontend 서브커맨드
│   ├── customer.go            # customer 서브커맨드
│   ├── driver.go              # driver 서브커맨드
│   ├── route.go               # route 서브커맨드
│   └── flags.go               # CLI 플래그 정의
├── services/
│   ├── config/config.go       # 전역 설정 (지연 시간 등)
│   ├── frontend/
│   │   ├── server.go          # HTTP 서버, dispatch 핸들러
│   │   ├── best_eta.go        # ETA 계산 오케스트레이션
│   │   └── web_assets/        # 정적 웹 파일
│   ├── customer/
│   │   ├── server.go          # HTTP 서버
│   │   ├── database.go        # 인메모리 MySQL 시뮬레이션
│   │   ├── client.go          # HTTP 클라이언트
│   │   └── interface.go       # Interface, Customer 타입
│   ├── driver/
│   │   ├── server.go          # gRPC 서버
│   │   ├── redis.go           # Redis 시뮬레이션
│   │   ├── client.go          # gRPC 클라이언트
│   │   ├── interface.go       # Interface, Driver 타입
│   │   └── driver.proto       # protobuf 정의
│   └── route/
│       ├── server.go          # HTTP 서버
│       ├── client.go          # HTTP 클라이언트
│       ├── interface.go       # Interface, Route 타입
│       └── stats.go           # expvar 통계
├── pkg/
│   ├── delay/delay.go         # 정규분포 지연 시뮬레이션
│   ├── httperr/httperr.go     # HTTP 에러 핸들링
│   ├── log/                   # 구조화된 로깅 팩토리
│   ├── pool/pool.go           # 워커 풀
│   └── tracing/               # OTEL 초기화, HTTP 클라이언트 등
├── docker-compose.yml         # Docker Compose 설정
├── Dockerfile                 # 빌드 이미지
└── kubernetes/                # K8s 배포 설정
```

---

## 2. 아키텍처

### 전체 아키텍처 다이어그램

```
                         +------------------+
                         |   웹 브라우저     |
                         |   (index.html)   |
                         +--------+---------+
                                  |
                          HTTP (포트 8080)
                                  |
                                  v
                     +------------+------------+
                     |   Frontend Service      |
                     |   (HTTP 서버)           |
                     |                         |
                     |   /dispatch             |
                     |   /config               |
                     |   /debug/vars           |
                     |   /metrics              |
                     +----+------+------+------+
                          |      |      |
              +-----------+      |      +----------+
              |                  |                 |
        HTTP (8081)        gRPC (8082)       HTTP (8083)
              |                  |                 |
              v                  v                 v
    +---------+------+  +--------+-------+  +-----+----------+
    | Customer       |  | Driver         |  | Route          |
    | Service        |  | Service        |  | Service        |
    | (HTTP)         |  | (gRPC)         |  | (HTTP)         |
    |                |  |                |  |                |
    | MySQL 시뮬     |  | Redis 시뮬     |  | 경로 계산 시뮬  |
    +----------------+  +----------------+  +----------------+
```

### 서비스 간 통신 프로토콜

| 호출 경로 | 프로토콜 | 설명 |
|-----------|----------|------|
| 브라우저 -> Frontend | HTTP | AJAX GET /dispatch |
| Frontend -> Customer | HTTP | GET /customer?customer=ID |
| Frontend -> Driver | gRPC | FindNearest(location) |
| Frontend -> Route | HTTP | GET /route?pickup=X&dropoff=Y |

### Why: Driver만 gRPC를 사용하는 이유

Driver 서비스만 gRPC를 사용하는 것은 의도적인 설계이다. HotROD는 분산 트레이싱 데모로서, HTTP와 gRPC 모두에서 트레이싱이 작동함을 보여주기 위해 두 프로토콜을 혼합하여 사용한다.

---

## 3. 서비스 구성

### 진입점

소스 경로: `examples/hotrod/main.go`

```go
package main

import (
    "github.com/jaegertracing/jaeger/examples/hotrod/cmd"
)

func main() {
    cmd.Execute()
}
```

### Root Command 초기화

소스 경로: `examples/hotrod/cmd/root.go`

```go
var RootCmd = &cobra.Command{
    Use:   "examples-hotrod",
    Short: "HotR.O.D. - A tracing demo application",
    Long:  `HotR.O.D. - A tracing demo application.`,
}

func Execute() {
    if err := RootCmd.Execute(); err != nil {
        logger.Fatal("We bowled a googly", zap.Error(err))
        os.Exit(-1)
    }
}
```

### 초기화 흐름 (onInitialize)

```go
func onInitialize() {
    // 1. 로거 설정
    zapOptions := []zap.Option{
        zap.AddStacktrace(zapcore.FatalLevel),
        zap.AddCallerSkip(1),
    }
    logger, _ = zap.NewDevelopment(zapOptions...)

    // 2. Jaeger 환경 변수 -> OTEL 환경 변수 매핑
    jaegerclientenv2otel.MapJaegerToOtelEnvVars(logger)

    // 3. Prometheus 메트릭 팩토리
    metricsFactory = prometheus.New().Namespace(
        metrics.NSOptions{Name: "hotrod", Tags: nil})

    // 4. "fix" 플래그 적용
    if config.MySQLGetDelay != fixDBConnDelay {
        config.MySQLGetDelay = fixDBConnDelay
    }
    if fixDBConnDisableMutex {
        config.MySQLMutexDisabled = true
    }
    if config.RouteWorkerPoolSize != fixRouteWorkerPoolSize {
        config.RouteWorkerPoolSize = fixRouteWorkerPoolSize
    }
}
```

### all 커맨드: 4개 서비스 동시 실행

소스 경로: `examples/hotrod/cmd/all.go`

```go
var allCmd = &cobra.Command{
    Use:   "all",
    Short: "Starts all services",
    RunE: func(_ *cobra.Command, args []string) error {
        logger.Info("Starting all services")
        go customerCmd.RunE(customerCmd, args)
        go driverCmd.RunE(driverCmd, args)
        go routeCmd.RunE(routeCmd, args)
        return frontendCmd.RunE(frontendCmd, args)  // 메인 goroutine
    },
}
```

customer, driver, route는 별도 goroutine에서, frontend는 메인 goroutine에서 실행된다. frontend가 메인에서 실행되는 이유는 `ListenAndServe`가 블로킹이므로 프로세스가 종료되지 않도록 하기 위해서이다.

---

## 4. Frontend 서비스

### Server 구조체

소스 경로: `examples/hotrod/services/frontend/server.go`

```go
type Server struct {
    hostPort string
    tracer   trace.TracerProvider
    logger   log.Factory
    bestETA  *bestETA
    assetFS  http.FileSystem
    basepath string
    jaegerUI string
}

type ConfigOptions struct {
    FrontendHostPort string
    DriverHostPort   string
    CustomerHostPort string
    RouteHostPort    string
    Basepath         string
    JaegerUI         string
}
```

### 서버 생성과 라우팅

```go
func NewServer(options ConfigOptions, tracer trace.TracerProvider,
               logger log.Factory) *Server {
    return &Server{
        hostPort: options.FrontendHostPort,
        tracer:   tracer,
        logger:   logger,
        bestETA:  newBestETA(tracer, logger, options),
        assetFS:  httpfs.PrefixedFS("web_assets", http.FS(assetFS)),
        basepath: options.Basepath,
        jaegerUI: options.JaegerUI,
    }
}

func (s *Server) createServeMux() http.Handler {
    mux := tracing.NewServeMux(true, s.tracer, s.logger)
    p := path.Join("/", s.basepath)
    mux.Handle(p, http.StripPrefix(p, http.FileServer(s.assetFS)))
    mux.Handle(path.Join(p, "/dispatch"), http.HandlerFunc(s.dispatch))
    mux.Handle(path.Join(p, "/config"), http.HandlerFunc(s.config))
    mux.Handle(path.Join(p, "/debug/vars"), expvar.Handler())
    mux.Handle(path.Join(p, "/metrics"), promhttp.Handler())
    return mux
}
```

### dispatch 핸들러

```go
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    s.logger.For(ctx).Info("HTTP request received",
        zap.String("method", r.Method), zap.Stringer("url", r.URL))

    // 파라미터 파싱
    customer := r.Form.Get("customer")
    customerID, err := strconv.Atoi(customer)
    // ...

    // ETA 계산 (핵심 비즈니스 로직)
    response, err := s.bestETA.Get(ctx, customerID)
    // ...

    s.writeResponse(response, w, r)
}
```

### bestETA: 핵심 오케스트레이션

소스 경로: `examples/hotrod/services/frontend/best_eta.go`

```go
type bestETA struct {
    customer customer.Interface
    driver   driver.Interface
    route    route.Interface
    pool     *pool.Pool
    logger   log.Factory
}

type Response struct {
    Driver string
    ETA    time.Duration
}
```

### ETA 계산 흐름 (Get 메서드)

```go
func (eta *bestETA) Get(ctx context.Context, customerID int) (*Response, error) {
    // Step 1: 고객 정보 조회 (Customer 서비스 호출)
    cust, err := eta.customer.Get(ctx, customerID)
    if err != nil {
        return nil, err
    }

    // Step 2: Baggage에 고객 이름 추가
    m, _ := baggage.NewMember("customer", cust.Name)
    bag := baggage.FromContext(ctx)
    bag, _ = bag.SetMember(m)
    ctx = baggage.ContextWithBaggage(ctx, bag)

    // Step 3: 가까운 드라이버 검색 (Driver 서비스 호출)
    drivers, err := eta.driver.FindNearest(ctx, cust.Location)
    if err != nil {
        return nil, err
    }

    // Step 4: 각 드라이버에 대한 경로 계산 (Route 서비스, 병렬)
    results := eta.getRoutes(ctx, cust, drivers)

    // Step 5: 최소 ETA를 가진 드라이버 선택
    resp := &Response{ETA: math.MaxInt64}
    for _, result := range results {
        if result.route.ETA < resp.ETA {
            resp.ETA = result.route.ETA
            resp.Driver = result.driver
        }
    }
    return resp, nil
}
```

### 경로 병렬 계산

```go
func (eta *bestETA) getRoutes(ctx context.Context, cust *customer.Customer,
                               drivers []driver.Driver) []routeResult {
    results := make([]routeResult, 0, len(drivers))
    wg := sync.WaitGroup{}
    routesLock := sync.Mutex{}

    for _, dd := range drivers {
        wg.Add(1)
        drv := dd  // 루프 변수 캡처
        eta.pool.Execute(func() {
            route, err := eta.route.FindRoute(ctx, drv.Location, cust.Location)
            routesLock.Lock()
            results = append(results, routeResult{
                driver: drv.DriverID,
                route:  route,
                err:    err,
            })
            routesLock.Unlock()
            wg.Done()
        })
    }
    wg.Wait()
    return results
}
```

### dispatch 시퀀스 다이어그램

```
브라우저          Frontend         Customer         Driver           Route
  |                 |                |                |                |
  |  GET /dispatch  |                |                |                |
  |  customer=123   |                |                |                |
  +---------------->|                |                |                |
  |                 |  GET /customer |                |                |
  |                 |  ?customer=123 |                |                |
  |                 +--------------->|                |                |
  |                 |                | MySQL SELECT   |                |
  |                 |                | (300ms 지연)   |                |
  |                 |<---------------+                |                |
  |                 |                                 |                |
  |                 | gRPC FindNearest                |                |
  |                 | (location)                      |                |
  |                 +-------------------------------->|                |
  |                 |                                 | Redis 조회     |
  |                 |                                 | (10개 드라이버)|
  |                 |<--------------------------------+                |
  |                 |                                                  |
  |                 | [Worker Pool: 3 workers, 10 tasks]              |
  |                 |  GET /route?pickup=X&dropoff=Y  (x10, 병렬)    |
  |                 +------------------------------------------------>|
  |                 |                                                  |
  |                 |<------------------------------------------------+
  |                 |                                                  |
  |  {Driver, ETA}  |                                                 |
  |<----------------+                                                 |
```

---

## 5. Customer 서비스

### Server 구조체

소스 경로: `examples/hotrod/services/customer/server.go`

```go
type Server struct {
    hostPort string
    tracer   trace.TracerProvider
    logger   log.Factory
    database *database
}

func NewServer(hostPort string, otelExporter string,
               metricsFactory metrics.Factory, logger log.Factory) *Server {
    return &Server{
        hostPort: hostPort,
        tracer:   tracing.InitOTEL("customer", otelExporter, metricsFactory, logger),
        logger:   logger,
        database: newDatabase(
            tracing.InitOTEL("mysql", otelExporter, metricsFactory, logger).Tracer("mysql"),
            logger.With(zap.String("component", "mysql")),
        ),
    }
}
```

Customer 서비스는 두 개의 TracerProvider를 생성한다:
- `"customer"`: 서비스 자체의 트레이서
- `"mysql"`: MySQL 시뮬레이션용 별도 트레이서 (종속성 그래프에서 별도 서비스로 표시)

### 인메모리 데이터베이스

소스 경로: `examples/hotrod/services/customer/database.go`

```go
type database struct {
    tracer    trace.Tracer
    logger    log.Factory
    customers map[int]*Customer
    lock      *tracing.Mutex    // 트레이싱 인식 뮤텍스
}
```

### 하드코딩된 고객 데이터

```go
customers: map[int]*Customer{
    123: {ID: "123", Name: "Rachel's_Floral_Designs", Location: "115,277"},
    567: {ID: "567", Name: "Amazing_Coffee_Roasters", Location: "211,653"},
    392: {ID: "392", Name: "Trom_Chocolatier",        Location: "577,322"},
    731: {ID: "731", Name: "Japanese_Desserts",        Location: "728,326"},
},
```

이 4개의 고객은 웹 UI의 4개 버튼에 대응한다.

### 데이터 인터페이스

소스 경로: `examples/hotrod/services/customer/interface.go`

```go
type Customer struct {
    ID       string
    Name     string
    Location string
}

type Interface interface {
    Get(ctx context.Context, customerID int) (*Customer, error)
}
```

### Database.Get: 의도적 성능 문제

```go
func (d *database) Get(ctx context.Context, customerID int) (*Customer, error) {
    d.logger.For(ctx).Info("Loading customer", zap.Int("customer_id", customerID))

    // 트레이싱: "SQL SELECT" 스팬 생성
    ctx, span := d.tracer.Start(ctx, "SQL SELECT",
        trace.WithSpanKind(trace.SpanKindClient))
    span.SetAttributes(
        otelsemconv.PeerServiceAttribute("mysql"),
        attribute.Key("sql.query").String(
            fmt.Sprintf("SELECT * FROM customer WHERE customer_id=%d", customerID)),
    )
    defer span.End()

    // 의도적 성능 문제 1: 커넥션 풀 뮤텍스
    if !config.MySQLMutexDisabled {
        d.lock.Lock(ctx)
        defer d.lock.Unlock()
    }

    // 의도적 성능 문제 2: 쿼리 지연
    delay.Sleep(config.MySQLGetDelay, config.MySQLGetDelayStdDev)

    if customer, ok := d.customers[customerID]; ok {
        return customer, nil
    }
    return nil, errors.New("invalid customer ID")
}
```

### HTTP 클라이언트

소스 경로: `examples/hotrod/services/customer/client.go`

```go
type Client struct {
    logger   log.Factory
    client   *tracing.HTTPClient
    hostPort string
}

func (c *Client) Get(ctx context.Context, customerID int) (*Customer, error) {
    url := fmt.Sprintf("http://"+c.hostPort+"/customer?customer=%d", customerID)
    var customer Customer
    if err := c.client.GetJSON(ctx, "/customer", url, &customer); err != nil {
        return nil, err
    }
    return &customer, nil
}
```

---

## 6. Driver 서비스

### Server 구조체 (gRPC)

소스 경로: `examples/hotrod/services/driver/server.go`

```go
type Server struct {
    hostPort string
    logger   log.Factory
    redis    *Redis
    server   *grpc.Server
}

var _ DriverServiceServer = (*Server)(nil)  // 인터페이스 구현 검증

func NewServer(hostPort string, otelExporter string,
               metricsFactory metrics.Factory, logger log.Factory) *Server {
    tracerProvider := tracing.InitOTEL("driver", otelExporter, metricsFactory, logger)
    server := grpc.NewServer(
        grpc.StatsHandler(otelgrpc.NewServerHandler(
            otelgrpc.WithTracerProvider(tracerProvider),
            otelgrpc.WithMeterProvider(noop.NewMeterProvider()),
        )),
    )
    return &Server{
        hostPort: hostPort,
        logger:   logger,
        server:   server,
        redis:    newRedis(otelExporter, metricsFactory, logger),
    }
}
```

### Protobuf 정의

소스 경로: `examples/hotrod/services/driver/driver.proto`

```protobuf
syntax="proto3";
package driver;

message DriverLocationRequest {
  string location = 1;
}

message DriverLocation {
  string driverID = 1;
  string location = 2;
}

message DriverLocationResponse {
  repeated DriverLocation locations = 1;
}

service DriverService {
  rpc FindNearest(DriverLocationRequest) returns (DriverLocationResponse);
}
```

### FindNearest 구현

```go
func (s *Server) FindNearest(ctx context.Context,
                              location *DriverLocationRequest) (*DriverLocationResponse, error) {
    s.logger.For(ctx).Info("Searching for nearby drivers",
        zap.String("location", location.Location))

    // Redis에서 드라이버 ID 목록 조회
    driverIDs := s.redis.FindDriverIDs(ctx, location.Location)

    // 각 드라이버의 상세 정보 조회 (최대 3회 재시도)
    locations := make([]*DriverLocation, len(driverIDs))
    for i, driverID := range driverIDs {
        var drv Driver
        var err error
        for i := range 3 {
            drv, err = s.redis.GetDriver(ctx, driverID)
            if err == nil {
                break
            }
            s.logger.For(ctx).Error("Retrying GetDriver after error",
                zap.Int("retry_no", i+1), zap.Error(err))
        }
        if err != nil {
            return nil, err
        }
        locations[i] = &DriverLocation{
            DriverID: drv.DriverID,
            Location: drv.Location,
        }
    }
    return &DriverLocationResponse{Locations: locations}, nil
}
```

### Redis 시뮬레이션

소스 경로: `examples/hotrod/services/driver/redis.go`

```go
type Redis struct {
    tracer trace.Tracer
    logger log.Factory
    errorSimulator
}

func newRedis(otelExporter string, metricsFactory metrics.Factory,
              logger log.Factory) *Redis {
    tp := tracing.InitOTEL("redis-manual", otelExporter, metricsFactory, logger)
    return &Redis{
        tracer: tp.Tracer("redis-manual"),
        logger: logger,
    }
}
```

### FindDriverIDs: 드라이버 검색

```go
func (r *Redis) FindDriverIDs(ctx context.Context, location string) []string {
    ctx, span := r.tracer.Start(ctx, "FindDriverIDs",
        trace.WithSpanKind(trace.SpanKindClient))
    span.SetAttributes(attribute.Key("param.driver.location").String(location))
    defer span.End()

    delay.Sleep(config.RedisFindDelay, config.RedisFindDelayStdDev)

    // 10개의 랜덤 드라이버 ID 생성
    drivers := make([]string, 10)
    for i := range drivers {
        drivers[i] = fmt.Sprintf("T7%05dC", rand.Int()%100000)
    }
    return drivers
}
```

### GetDriver: 드라이버 상세 조회 (에러 시뮬레이션)

```go
func (r *Redis) GetDriver(ctx context.Context, driverID string) (Driver, error) {
    ctx, span := r.tracer.Start(ctx, "GetDriver",
        trace.WithSpanKind(trace.SpanKindClient))
    span.SetAttributes(attribute.Key("param.driverID").String(driverID))
    defer span.End()

    delay.Sleep(config.RedisGetDelay, config.RedisGetDelayStdDev)

    // 의도적 에러: 매 5번째 호출에서 타임아웃
    if err := r.checkError(); err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, "An error occurred")
        return Driver{}, err
    }
    // ...
}
```

### Error Simulator

```go
type errorSimulator struct {
    sync.Mutex
    countTillError int
}

func (es *errorSimulator) checkError() error {
    es.Lock()
    es.countTillError--
    if es.countTillError > 0 {
        es.Unlock()
        return nil
    }
    es.countTillError = 5  // 매 5번째 호출에서 에러
    es.Unlock()
    delay.Sleep(2*config.RedisGetDelay, 0)  // 타임아웃 지연 추가
    return errTimeout  // errors.New("redis timeout")
}
```

### gRPC 클라이언트

소스 경로: `examples/hotrod/services/driver/client.go`

```go
func NewClient(tracerProvider trace.TracerProvider,
               logger log.Factory, hostPort string) *Client {
    conn, err := grpc.NewClient(hostPort,
        grpc.WithTransportCredentials(insecure.NewCredentials()),
        grpc.WithStatsHandler(otelgrpc.NewClientHandler(
            otelgrpc.WithTracerProvider(tracerProvider),
            otelgrpc.WithMeterProvider(noop.NewMeterProvider()),
        )),
    )
    // ...
    client := NewDriverServiceClient(conn)
    return &Client{logger: logger, client: client}
}

func (c *Client) FindNearest(ctx context.Context, location string) ([]Driver, error) {
    ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
    defer cancel()
    response, err := c.client.FindNearest(ctx,
        &DriverLocationRequest{Location: location})
    // ...
    return fromProto(response), nil
}
```

---

## 7. Route 서비스

### Server 구조체

소스 경로: `examples/hotrod/services/route/server.go`

```go
type Server struct {
    hostPort string
    tracer   trace.TracerProvider
    logger   log.Factory
}
```

### 경로 계산

```go
func computeRoute(ctx context.Context, pickup, dropoff string) *Route {
    start := time.Now()
    defer func() {
        updateCalcStats(ctx, time.Since(start))
    }()

    // 50ms 평균 지연 시뮬레이션
    delay.Sleep(config.RouteCalcDelay, config.RouteCalcDelayStdDev)

    // 정규분포 기반 ETA 생성 (2분 이상)
    eta := math.Max(2, rand.NormFloat64()*3+5)
    return &Route{
        Pickup:  pickup,
        Dropoff: dropoff,
        ETA:     time.Duration(eta) * time.Minute,
    }
}
```

### 통계 수집 (expvar + Baggage)

소스 경로: `examples/hotrod/services/route/stats.go`

```go
var (
    routeCalcByCustomer = expvar.NewMap("route.calc.by.customer.sec")
    routeCalcBySession  = expvar.NewMap("route.calc.by.session.sec")
)

var stats = []struct {
    expvar     *expvar.Map
    baggageKey string
}{
    {expvar: routeCalcByCustomer, baggageKey: "customer"},
    {expvar: routeCalcBySession,  baggageKey: "session"},
}

func updateCalcStats(ctx context.Context, delay time.Duration) {
    delaySec := float64(delay/time.Millisecond) / 1000.0
    for _, s := range stats {
        key := tracing.BaggageItem(ctx, s.baggageKey)
        if key != "" {
            s.expvar.AddFloat(key, delaySec)
        }
    }
}
```

이 코드는 Baggage에서 `customer`와 `session` 값을 추출하여, 각 고객별/세션별로 경로 계산에 소요된 시간을 집계한다.

### 데이터 모델

소스 경로: `examples/hotrod/services/route/interface.go`

```go
type Route struct {
    Pickup  string
    Dropoff string
    ETA     time.Duration
}

type Interface interface {
    FindRoute(ctx context.Context, pickup, dropoff string) (*Route, error)
}
```

---

## 8. 트레이싱 초기화와 전파

### OTEL 초기화

소스 경로: `examples/hotrod/pkg/tracing/init.go`

```go
func InitOTEL(serviceName string, exporterType string,
              metricsFactory metrics.Factory, logger log.Factory) trace.TracerProvider {
    once.Do(func() {
        otel.SetTextMapPropagator(
            propagation.NewCompositeTextMapPropagator(
                propagation.TraceContext{},
                propagation.Baggage{},
            ))
    })

    exp, err := createOtelExporter(exporterType)
    // ...

    rpcmetricsObserver := rpcmetrics.NewObserver(metricsFactory,
        rpcmetrics.DefaultNameNormalizer)

    res, err := resource.New(
        context.Background(),
        resource.WithSchemaURL(otelsemconv.SchemaURL),
        resource.WithAttributes(otelsemconv.ServiceNameAttribute(serviceName)),
        resource.WithTelemetrySDK(),
        resource.WithHost(),
        resource.WithOSType(),
    )

    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exp,
            sdktrace.WithBatchTimeout(1000*time.Millisecond)),
        sdktrace.WithSpanProcessor(rpcmetricsObserver),
        sdktrace.WithResource(res),
    )
    return tp
}
```

### 트레이서 프로바이더 생성 맵

```
서비스           TracerProvider           서비스 이름
-----------      ----------------         -----------
Frontend    -->  InitOTEL("frontend")     "frontend"
Customer    -->  InitOTEL("customer")     "customer"
                 InitOTEL("mysql")        "mysql"
Driver      -->  InitOTEL("driver")       "driver"
                 InitOTEL("redis-manual") "redis-manual"
Route       -->  InitOTEL("route")        "route"
```

Jaeger UI의 종속성 그래프에서 6개의 서비스가 표시된다: frontend, customer, mysql, driver, redis-manual, route.

### Propagator 설정

```go
otel.SetTextMapPropagator(
    propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{},  // W3C Trace Context (traceparent 헤더)
        propagation.Baggage{},       // W3C Baggage (baggage 헤더)
    ))
```

두 가지 전파기가 설정된다:
- **TraceContext**: 트레이스 ID, 스팬 ID, 샘플링 플래그 전파
- **Baggage**: 요청 범위 메타데이터 (customer 이름, session ID) 전파

### HTTP 트레이싱 클라이언트

소스 경로: `examples/hotrod/pkg/tracing/http.go`

```go
type HTTPClient struct {
    TracerProvider trace.TracerProvider
    Client         *http.Client
}

func NewHTTPClient(tp trace.TracerProvider) *HTTPClient {
    return &HTTPClient{
        TracerProvider: tp,
        Client: &http.Client{
            Transport: otelhttp.NewTransport(
                http.DefaultTransport,
                otelhttp.WithTracerProvider(tp),
            ),
        },
    }
}
```

`otelhttp.NewTransport`를 사용하여 HTTP 요청을 자동으로 트레이싱한다.

### TracedServeMux: HTTP 서버 트레이싱

소스 경로: `examples/hotrod/pkg/tracing/mux.go`

```go
type TracedServeMux struct {
    mux         *http.ServeMux
    copyBaggage bool
    tracer      trace.TracerProvider
    logger      log.Factory
}

func (tm *TracedServeMux) Handle(pattern string, handler http.Handler) {
    middleware := otelhttp.NewHandler(
        traceResponseHandler(handler),
        pattern,
        otelhttp.WithTracerProvider(tm.tracer))
    tm.mux.Handle(pattern, middleware)
}
```

### traceResponseHandler: traceresponse 헤더

```go
func traceResponseHandler(handler http.Handler) http.Handler {
    var prop propagation.TraceContext
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        carrier := make(map[string]string)
        prop.Inject(r.Context(), propagation.MapCarrier(carrier))
        w.Header().Add("traceresponse", carrier["traceparent"])
        handler.ServeHTTP(w, r)
    })
}
```

W3C Trace Context 응답 헤더(`traceresponse`)를 추가하여, 브라우저가 트레이스 ID를 알 수 있게 한다. 이를 통해 UI에서 "open trace" 링크를 생성한다.

### Baggage 유틸리티

소스 경로: `examples/hotrod/pkg/tracing/baggage.go`

```go
func BaggageItem(ctx context.Context, key string) string {
    b := baggage.FromContext(ctx)
    m := b.Member(key)
    return m.Value()
}
```

---

## 9. 의도적 성능 문제와 진단

HotROD는 분산 트레이싱으로 진단 가능한 3가지 성능 문제를 의도적으로 포함한다.

### 문제 1: MySQL 커넥션 풀 뮤텍스

소스 경로: `examples/hotrod/pkg/tracing/mutex.go`

```go
type Mutex struct {
    SessionBaggageKey string
    LogFactory        log.Factory
    realLock          sync.Mutex
    holder            string
    waiters           []string
    waitersLock       sync.Mutex
}

func (sm *Mutex) Lock(ctx context.Context) {
    logger := sm.LogFactory.For(ctx)
    session := BaggageItem(ctx, sm.SessionBaggageKey)
    activeSpan := trace.SpanFromContext(ctx)
    activeSpan.SetAttributes(attribute.String(sm.SessionBaggageKey, session))

    // 대기 중인 트랜잭션 로깅
    sm.waitersLock.Lock()
    if waiting := len(sm.waiters); waiting > 0 && activeSpan != nil {
        logger.Info(
            fmt.Sprintf("Waiting for lock behind %d transactions", waiting),
            zap.String("blockers", fmt.Sprintf("%v", sm.waiters)),
        )
    }
    sm.waiters = append(sm.waiters, session)
    sm.waitersLock.Unlock()

    sm.realLock.Lock()  // 실제 잠금 대기
    sm.holder = session
    // ...
}
```

**현상**: 동시에 여러 요청이 오면, MySQL 커넥션 풀이 1개뿐이라 직렬화된다.
**진단**: Jaeger UI에서 긴 스팬 지속 시간과 "Waiting for lock" 로그를 확인할 수 있다.
**수정**: `--fix-disable-db-conn-mutex` (또는 `-M`) 플래그로 뮤텍스를 비활성화한다.

### 문제 2: MySQL 쿼리 지연

소스 경로: `examples/hotrod/services/config/config.go`

```go
var (
    MySQLGetDelay       = 300 * time.Millisecond
    MySQLGetDelayStdDev = MySQLGetDelay / 10
)
```

**현상**: 각 고객 조회에 평균 300ms가 소요된다.
**진단**: Jaeger UI에서 "SQL SELECT" 스팬의 지속 시간이 300ms임을 확인할 수 있다.
**수정**: `--fix-db-query-delay` (또는 `-D`) 플래그로 지연 시간을 줄인다.

### 문제 3: Route 워커 풀 크기

```go
var RouteWorkerPoolSize = 3
```

**현상**: 10개 드라이버에 대한 경로를 3개 워커로 처리하므로, 병렬성이 제한된다.
**진단**: Jaeger UI에서 route 호출이 3개씩 묶여서 실행되는 것을 확인할 수 있다.
**수정**: `--fix-route-worker-pool-size` (또는 `-W`) 플래그로 워커 수를 늘린다.

### 성능 문제 시각화

```
기본 설정 (3개 워커, 10개 route 요청):

Worker 1: [route-1][route-4][route-7][route-10]
Worker 2: [route-2][route-5][route-8]
Worker 3: [route-3][route-6][route-9]
           |------- 총 4배치 시간 -------|

수정 후 (10개 워커):

Worker 1: [route-1]
Worker 2: [route-2]
...
Worker 10: [route-10]
           |-- 1배치 시간 --|
```

### Redis 에러와 재시도

```
정상 호출:
  GetDriver(driver-1) -> OK      (10ms)
  GetDriver(driver-2) -> OK      (10ms)
  GetDriver(driver-3) -> OK      (10ms)
  GetDriver(driver-4) -> OK      (10ms)
  GetDriver(driver-5) -> TIMEOUT (30ms) + 재시도
    retry 1: GetDriver(driver-5) -> OK (10ms)
```

Jaeger UI에서 재시도 스팬과 에러 마크를 확인할 수 있다.

---

## 10. 공유 유틸리티 패키지

### delay 패키지

소스 경로: `examples/hotrod/pkg/delay/delay.go`

```go
func Sleep(mean time.Duration, stdDev time.Duration) {
    fMean := float64(mean)
    fStdDev := float64(stdDev)
    delay := time.Duration(math.Max(1, rand.NormFloat64()*fStdDev+fMean))
    time.Sleep(delay)
}
```

정규분포 기반의 랜덤 지연을 생성한다. 실제 서비스의 불규칙한 응답 시간을 시뮬레이션한다.

### pool 패키지

소스 경로: `examples/hotrod/pkg/pool/pool.go`

```go
type Pool struct {
    jobs chan func()
    stop chan struct{}
}

func New(workers int) *Pool {
    jobs := make(chan func())
    stop := make(chan struct{})
    for range workers {
        go func() {
            for {
                select {
                case job := <-jobs:
                    job()
                case <-stop:
                    return
                }
            }
        }()
    }
    return &Pool{jobs: jobs, stop: stop}
}

func (p *Pool) Execute(job func()) {
    p.jobs <- job
}
```

버퍼 없는 채널(`make(chan func())`)을 사용하므로, `Execute`는 워커가 작업을 가져갈 때까지 블록된다. 이것이 워커 풀 크기가 병렬성에 직접 영향을 미치는 이유이다.

### rpcmetrics 패키지

소스 경로: `examples/hotrod/pkg/tracing/rpcmetrics/`

```
rpcmetrics/
├── observer.go      # SpanProcessor 구현
├── metrics.go       # 메트릭 정의
├── endpoints.go     # 엔드포인트 이름 추출
└── normalizer.go    # 이름 정규화
```

RPC 메트릭 관찰자는 `sdktrace.SpanProcessor` 인터페이스를 구현하여 스팬 완료 시 자동으로 요청 수, 지연 시간, 에러율 등의 메트릭을 수집한다.

---

## 11. Web UI

### index.html 구조

소스 경로: `examples/hotrod/services/frontend/web_assets/index.html`

```html
<div class="container">
    <div class="uuid alert alert-info"></div>
    <center>
        <h1>Hot R.O.D.</h1>
        <h4>Rides On Demand</h4>
        <div class="row">
            <span class="btn hotrod-button" data-customer="123">
                Rachel's Floral Designs</span>
            <span class="btn hotrod-button" data-customer="392">
                Trom Chocolatier</span>
            <span class="btn hotrod-button" data-customer="731">
                Japanese Desserts</span>
            <span class="btn hotrod-button" data-customer="567">
                Amazing Coffee Roasters</span>
        </div>
    </center>
</div>
```

### JavaScript AJAX 요청

```javascript
$(".hotrod-button").click(function(evt) {
    const customer = evt.target.dataset.customer;
    const headers = {
        'baggage': 'session=' + clientUUID + ', request=' + requestID
    };

    $.ajax(pathPrefix + '/dispatch?customer=' + customer, {
        headers: headers,
        method: 'GET',
        success: function(data, textStatus, xhr) {
            // traceresponse 헤더에서 traceID 추출
            const traceResponse = xhr.getResponseHeader('traceresponse');
            const traceID = parseTraceResponse(traceResponse);

            // Jaeger UI 링크 생성
            if (traceID) {
                traceLink = `<a href="${jaeger}/trace/${traceID}">open trace</a>`;
            }
        },
    });
});
```

### Baggage 전파

브라우저에서 `baggage` 헤더를 통해 `session`과 `request` 값을 전송한다:

```
baggage: session=4567, request=4567-1
```

이 값들은 OTEL의 Baggage Propagator에 의해 모든 다운스트림 서비스에 자동으로 전파된다. Route 서비스에서는 이를 활용하여 고객별/세션별 통계를 집계한다.

### traceresponse 헤더 파싱

```javascript
function parseTraceResponse(value) {
    const TRACE_PARENT_REGEX = new RegExp(
        `^\\s?(${VERSION_PART})-(${TRACE_ID_PART})-(${PARENT_ID_PART})-` +
        `(${FLAGS_PART})(-.*)?\\s?$`
    );
    const match = TRACE_PARENT_REGEX.exec(value);
    return (match) ? match[2] : null;  // traceID 반환
}
```

W3C Trace Context 형식 (`00-traceId-spanId-flags`)에서 traceId를 추출한다.

---

## 12. Docker Compose 배포

### docker-compose.yml

소스 경로: `examples/hotrod/docker-compose.yml`

```yaml
services:
  jaeger:
    image: ${REGISTRY:-}jaegertracing/jaeger:${JAEGER_VERSION:-latest}
    ports:
      - "16686:16686"    # Jaeger UI
      - "4317:4317"      # OTLP gRPC
      - "4318:4318"      # OTLP HTTP
    environment:
      - LOG_LEVEL=debug
    networks:
      - jaeger-example

  hotrod:
    image: ${REGISTRY:-}jaegertracing/example-hotrod:${HOTROD_VERSION:-latest}
    ports:
      - "8080:8080"      # Frontend
      - "8083:8083"      # Route
    command: ["all"]
    environment:
      - OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318
    networks:
      - jaeger-example
    depends_on:
      - jaeger

networks:
  jaeger-example:
```

### 환경 변수

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `JAEGER_VERSION` | `latest` | Jaeger 이미지 태그 |
| `HOTROD_VERSION` | `latest` | HotROD 이미지 태그 |
| `REGISTRY` | (비어 있음) | Docker Registry 접두사 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | - | OTLP 엔드포인트 (HotROD -> Jaeger) |

### 실행 방법

```bash
# 기본 실행
docker compose up

# 특정 버전 지정
JAEGER_VERSION=2.14.0 HOTROD_VERSION=1.63.0 docker compose up

# 종료
docker compose down
```

### 포트 맵핑

```
+-------------------+         +-------------------+
| hotrod 컨테이너   |         | jaeger 컨테이너   |
|                   |         |                   |
| 8080: Frontend    |  OTLP   | 4318: OTLP HTTP   |
| 8081: Customer    |-------->| 4317: OTLP gRPC   |
| 8082: Driver      |  HTTP   | 16686: Jaeger UI  |
| 8083: Route       |         |                   |
+-------------------+         +-------------------+
       |                              |
       | 외부 노출                    | 외부 노출
       v                              v
  localhost:8080                  localhost:16686
  localhost:8083                  localhost:4317
                                  localhost:4318
```

---

## 13. Kubernetes 배포

### README 가이드

소스 경로: `examples/hotrod/kubernetes/README.md`

```bash
# 배포
kustomize build ./kubernetes | kubectl apply -f -

# 포트 포워딩
kubectl port-forward -n example-hotrod service/example-hotrod 8080:frontend
kubectl port-forward -n example-hotrod service/jaeger 16686:frontend

# 정리
kustomize build ./kubernetes | kubectl delete -f -
```

---

## 14. CLI 플래그와 설정

### 전체 플래그 목록

소스 경로: `examples/hotrod/cmd/flags.go`

| 플래그 | 축약 | 기본값 | 설명 |
|--------|------|--------|------|
| `--otel-exporter` | `-x` | `otlp` | OTEL 익스포터 (otlp/stdout) |
| `--fix-db-query-delay` | `-D` | `300ms` | MySQL 쿼리 평균 지연 |
| `--fix-disable-db-conn-mutex` | `-M` | `false` | DB 커넥션 뮤텍스 비활성화 |
| `--fix-route-worker-pool-size` | `-W` | `3` | Route 워커 풀 크기 |
| `--customer-service-hostname` | - | `0.0.0.0` | Customer 서비스 호스트 |
| `--driver-service-hostname` | - | `0.0.0.0` | Driver 서비스 호스트 |
| `--frontend-service-hostname` | - | `0.0.0.0` | Frontend 서비스 호스트 |
| `--route-service-hostname` | - | `0.0.0.0` | Route 서비스 호스트 |
| `--customer-service-port` | `-c` | `8081` | Customer 서비스 포트 |
| `--driver-service-port` | `-d` | `8082` | Driver 서비스 포트 |
| `--frontend-service-port` | `-f` | `8080` | Frontend 서비스 포트 |
| `--route-service-port` | `-r` | `8083` | Route 서비스 포트 |
| `--basepath` | `-b` | `""` | Frontend 기본 경로 |
| `--jaeger-ui` | `-j` | `http://localhost:16686` | Jaeger UI 주소 |
| `--verbose` | `-v` | `false` | 디버그 로깅 활성화 |

### 성능 문제 수정 시뮬레이션

```bash
# 모든 문제 수정하여 실행
go run ./examples/hotrod/main.go all \
    -D 10ms \      # MySQL 지연 10ms로 감소
    -M \           # 뮤텍스 비활성화
    -W 10          # 워커 10개로 증가
```

### 설정 변수

소스 경로: `examples/hotrod/services/config/config.go`

```go
var (
    // Frontend
    RouteWorkerPoolSize = 3

    // Customer (MySQL 시뮬레이션)
    MySQLGetDelay       = 300 * time.Millisecond
    MySQLGetDelayStdDev = MySQLGetDelay / 10
    MySQLMutexDisabled  = false

    // Driver (Redis 시뮬레이션)
    RedisFindDelay       = 20 * time.Millisecond
    RedisFindDelayStdDev = RedisFindDelay / 4
    RedisGetDelay        = 10 * time.Millisecond
    RedisGetDelayStdDev  = RedisGetDelay / 4

    // Route
    RouteCalcDelay       = 50 * time.Millisecond
    RouteCalcDelayStdDev = RouteCalcDelay / 4
)
```

---

## 15. 트레이싱 개념 시연 분석

### 1. 분산 트레이싱 기본

HotROD가 시연하는 분산 트레이싱의 핵심 개념:

```
하나의 요청(dispatch)이 생성하는 트레이스 구조:

[frontend: HTTP GET /dispatch]  -------- 루트 스팬
  |
  +-- [customer: HTTP GET /customer]  -- 자식 스팬 (HTTP)
  |     |
  |     +-- [mysql: SQL SELECT]  ------- 손자 스팬 (DB)
  |
  +-- [driver: gRPC FindNearest]  ------ 자식 스팬 (gRPC)
  |     |
  |     +-- [redis-manual: FindDriverIDs] -- Redis 스팬
  |     +-- [redis-manual: GetDriver] x10 -- Redis 스팬들
  |
  +-- [route: HTTP GET /route] x10  ---- 자식 스팬들 (병렬)
```

### 2. 서비스 종속성 발견

Jaeger UI의 종속성 다이어그램에서 6개 서비스와 그 관계를 확인할 수 있다:

```
frontend --> customer --> mysql
         |
         --> driver --> redis-manual
         |
         --> route
```

### 3. Baggage 전파

```
브라우저: baggage: session=4567, request=4567-1
    |
    v
Frontend (baggage에 customer=Rachel's_Floral_Designs 추가)
    |
    +-> Customer (baggage 자동 전파)
    +-> Driver (baggage 자동 전파)
    +-> Route (baggage에서 customer/session 추출 -> expvar 통계)
```

### 4. 에러 진단

Redis 타임아웃이 발생하면 Jaeger UI에서:
- 에러 마크가 표시된 스팬을 확인
- `span.RecordError(err)` + `span.SetStatus(codes.Error, ...)` 결과
- 재시도 패턴 (3회 시도)을 스팬 트리에서 관찰

### 5. 지연 시간 분석

Jaeger UI에서 각 스팬의 지속 시간을 통해:
- MySQL 쿼리 지연 (300ms)이 어디서 발생하는지 확인
- 뮤텍스 대기로 인한 추가 지연 확인
- 워커 풀 크기에 의한 배치 효과 확인

### 6. 동시성 문제 진단

뮤텍스가 활성화된 상태에서 여러 요청을 보내면:
- 로그에 "Waiting for lock behind N transactions" 메시지가 표시
- Baggage의 `request` 값으로 어떤 요청이 블로킹하는지 식별
- 이것이 Baggage의 실용적 활용 사례

---

## 요약

HotROD는 단순한 데모를 넘어, 분산 트레이싱의 핵심 개념을 실전적으로 학습할 수 있는 교육 도구이다.

### 학습 포인트 정리

| 개념 | HotROD에서의 구현 |
|------|-------------------|
| 스팬 생성 | 각 서비스의 tracer.Start() |
| Context 전파 | HTTP: otelhttp, gRPC: otelgrpc |
| Baggage | 브라우저 -> 모든 서비스로 session/customer 전파 |
| 에러 기록 | Redis의 span.RecordError() |
| 서비스 종속성 | 6개 서비스의 호출 그래프 |
| 성능 진단 | 3가지 의도적 문제 + CLI 수정 |
| 혼합 프로토콜 | HTTP + gRPC 트레이싱 |
| 메트릭 연동 | Prometheus + expvar |

### 핵심 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `examples/hotrod/main.go` | 진입점 |
| `examples/hotrod/cmd/all.go` | 4개 서비스 동시 실행 |
| `examples/hotrod/cmd/flags.go` | CLI 플래그 정의 |
| `examples/hotrod/services/frontend/best_eta.go` | 핵심 오케스트레이션 |
| `examples/hotrod/services/customer/database.go` | MySQL 시뮬레이션 |
| `examples/hotrod/services/driver/redis.go` | Redis 시뮬레이션 |
| `examples/hotrod/services/route/stats.go` | Baggage 기반 통계 |
| `examples/hotrod/pkg/tracing/init.go` | OTEL 초기화 |
| `examples/hotrod/pkg/tracing/mutex.go` | 트레이싱 인식 뮤텍스 |
| `examples/hotrod/pkg/pool/pool.go` | 워커 풀 |
| `examples/hotrod/services/config/config.go` | 지연 시간 설정 |
| `examples/hotrod/docker-compose.yml` | Docker Compose 배포 |
