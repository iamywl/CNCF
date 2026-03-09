# 19. LogCLI & Query Tee — 쿼리 도구 Deep Dive

## 목차

1. [개요](#1-개요)
2. [LogCLI 아키텍처](#2-logcli-아키텍처)
3. [LogCLI 커맨드 구조](#3-logcli-커맨드-구조)
4. [LogCLI 클라이언트 시스템](#4-logcli-클라이언트-시스템)
5. [LogCLI 쿼리 실행 흐름](#5-logcli-쿼리-실행-흐름)
6. [LogCLI 병렬 쿼리](#6-logcli-병렬-쿼리)
7. [LogCLI 출력 시스템](#7-logcli-출력-시스템)
8. [Query Tee 아키텍처](#8-query-tee-아키텍처)
9. [Query Tee 라우팅 설계](#9-query-tee-라우팅-설계)
10. [Query Tee 응답 비교](#10-query-tee-응답-비교)
11. [통합 설계 분석](#11-통합-설계-분석)
12. [성능 및 운영 고려사항](#12-성능-및-운영-고려사항)

---

## 1. 개요

Loki 생태계에는 두 가지 핵심 쿼리 도구가 존재한다.

**LogCLI**(`cmd/logcli/`)는 Loki에 대한 커맨드라인 인터페이스(CLI)로, 사용자가 터미널에서 직접 LogQL 쿼리를 실행하고 결과를 확인할 수 있다. Grafana UI에 의존하지 않고도 로그를 검색, 분석, 스트리밍할 수 있는 경량 도구이다.

**Query Tee**(`cmd/querytee/`)는 두 개의 Loki 인스턴스에 동일한 쿼리를 동시에 전달하고 응답을 비교하는 프록시 도구이다. Loki 업그레이드, 설정 변경, 성능 비교 테스트에 사용된다.

### 왜 이 두 도구가 중요한가

```
┌─────────────┐      LogQL       ┌──────────┐
│   사용자     │ ───────────────→ │  LogCLI  │ → Loki API
│   (터미널)   │                  └──────────┘
└─────────────┘

┌─────────────┐                  ┌────────────┐     ┌─────────────┐
│  클라이언트  │ ─── 쿼리 ──────→│ Query Tee  │────→│ Loki A (기존)│
│  (Grafana)  │                  │  (프록시)   │────→│ Loki B (신규)│
└─────────────┘                  └────────────┘     └─────────────┘
                                       │
                                  응답 비교 + 메트릭
```

LogCLI는 **개발/디버깅 시** 필수적이고, Query Tee는 **프로덕션 마이그레이션/검증** 시 필수적이다.

---

## 2. LogCLI 아키텍처

### 소스 코드 구조

LogCLI의 진입점은 `cmd/logcli/main.go`이며, 핵심 로직은 `pkg/logcli/` 패키지에 분산되어 있다.

```
cmd/logcli/
├── main.go           ← 진입점, kingpin CLI 파서, 커맨드 등록

pkg/logcli/
├── client/           ← Loki API 클라이언트 (HTTP, File)
│   ├── client.go     ← Client 인터페이스 정의
│   └── file.go       ← stdin/파일 기반 클라이언트
├── query/            ← range/instant 쿼리 실행
│   └── query.go      ← DoQuery, DoQueryParallel, TailQuery
├── labelquery/       ← 레이블 쿼리
├── seriesquery/      ← 시리즈 쿼리
├── volume/           ← 볼륨 쿼리
├── index/            ← 인덱스 통계/볼륨
├── detected/         ← 필드 자동 감지
├── delete/           ← 삭제 요청 관리
└── output/           ← 출력 포매터 (default, raw, jsonl)
```

### 설계 원칙

LogCLI의 핵심 설계 원칙은 다음과 같다:

1. **단일 바이너리 CLI**: Kingpin 프레임워크 기반의 서브커맨드 패턴
2. **Loki HTTP API 직접 호출**: gRPC가 아닌 HTTP API를 사용하여 어디서든 접근 가능
3. **stdin 지원**: 파일이나 파이프라인으로부터 로그를 읽어 LogQL 처리 가능
4. **병렬 다운로드**: 대량 로그 추출 시 시간 범위를 분할하여 병렬 처리

---

## 3. LogCLI 커맨드 구조

### 커맨드 트리

`cmd/logcli/main.go`에서 kingpin을 사용하여 다음 커맨드를 등록한다:

```
logcli
├── query              ← 범위 쿼리 (로그/메트릭)
├── instant-query       ← 인스턴트 쿼리 (단일 시점 메트릭)
├── labels             ← 레이블 이름/값 조회
├── series             ← 시리즈 매처 조회
├── fmt                ← LogQL 포맷팅
├── stats              ← 인덱스 통계 조회
├── volume             ← 볼륨 집계 조회
├── volume_range       ← 시계열 볼륨 조회
├── detected-fields    ← 자동 감지 필드 조회
└── delete             ← 로그 삭제 관리
    ├── create
    ├── list
    └── cancel
```

### 글로벌 플래그 설계

소스 코드(`cmd/logcli/main.go` 라인 34-61)에서 글로벌 플래그를 정의한다:

```go
var (
    app = kingpin.New("logcli", "A command-line for loki.").
        Version(version.Print("logcli"))
    quiet      = app.Flag("quiet", "Suppress query metadata").Default("false").Short('q').Bool()
    logLevel   = app.Flag("log.level", "Log level").Default("error").Enum(...)
    statistics = app.Flag("stats", "Show query statistics").Default("false").Bool()
    outputMode = app.Flag("output", "Specify output mode [default, raw, jsonl]").
                Default("default").Short('o').Enum("default", "raw", "jsonl")
    timezone   = app.Flag("timezone", "...").Default("Local").Short('z').Enum("Local", "UTC")
    cpuProfile = app.Flag("cpuprofile", "...").Default("").String()
    memProfile = app.Flag("memprofile", "...").Default("").String()
    stdin      = app.Flag("stdin", "Take input logs from stdin").Bool()
)
```

**왜 kingpin을 사용하는가?**

kingpin은 Go CLI 프레임워크 중에서 서브커맨드와 플래그를 **선언적으로** 정의할 수 있고, `Action` 콜백을 통해 플래그 파싱 후 초기화 로직을 실행할 수 있다. LogCLI는 각 쿼리 타입마다 시간 범위 파싱(from/to/since)이 필요한데, kingpin의 Action 패턴이 이를 깔끔하게 처리한다.

### 쿼리 생성 패턴

`newQuery()` 함수(`cmd/logcli/main.go` 라인 656-717)는 Action 콜백 패턴을 사용한다:

```go
func newQuery(instant bool, cmd *kingpin.CmdClause) *query.Query {
    var now, from, to string
    var since time.Duration
    q := &query.Query{}

    // Action 콜백: 모든 플래그가 파싱된 후 실행
    cmd.Action(func(_ *kingpin.ParseContext) error {
        if instant {
            q.SetInstant(mustParse(now, time.Now()))
        } else {
            defaultEnd := time.Now()
            defaultStart := defaultEnd.Add(-since)
            q.Start = mustParse(from, defaultStart)
            q.End = mustParse(to, defaultEnd)
        }
        q.Quiet = *quiet
        return nil
    })
    // ... 플래그 등록
}
```

**왜 Action 콜백인가?** 플래그 파싱과 초기화를 분리함으로써, `--since 1h`와 `--from/--to`가 같은 로직으로 처리되고 기본값 계산이 한 곳에서 이루어진다.

---

## 4. LogCLI 클라이언트 시스템

### Client 인터페이스

`pkg/logcli/client/client.go`에서 정의하는 Client 인터페이스:

```
┌───────────────────────────┐
│     Client 인터페이스       │
├───────────────────────────┤
│ Query(queryStr, limit, ...)│
│ QueryRange(...)            │
│ ListLabelNames(...)        │
│ ListLabelValues(...)       │
│ Series(...)                │
│ LiveTailQueryConn(...)     │
│ GetOrgID()                 │
└────────────┬──────────────┘
             │
     ┌───────┴───────┐
     │               │
┌────▼────┐   ┌──────▼──────┐
│Default   │   │ FileClient  │
│Client    │   │ (stdin)     │
│(HTTP)    │   │             │
└──────────┘   └─────────────┘
```

### DefaultClient 구성

`newQueryClient()` 함수(`cmd/logcli/main.go` 라인 554-599)에서 HTTP 클라이언트를 구성한다:

```go
func newQueryClient(app *kingpin.Application) client.Client {
    client := &client.DefaultClient{
        TLSConfig: config.TLSConfig{},
    }

    app.Flag("addr", "Server address.").Default("http://localhost:3100").
        Envar("LOKI_ADDR").Action(addressAction).StringVar(&client.Address)
    app.Flag("username", "...").Envar("LOKI_USERNAME").StringVar(&client.Username)
    app.Flag("password", "...").Envar("LOKI_PASSWORD").StringVar(&client.Password)
    app.Flag("org-id", "...").Envar("LOKI_ORG_ID").StringVar(&client.OrgID)
    app.Flag("bearer-token", "...").Envar("LOKI_BEARER_TOKEN").StringVar(&client.BearerToken)
    // ... TLS, 프록시, 재시도 설정 등
    return client
}
```

**환경 변수 우선 패턴**: 모든 연결 설정은 `LOKI_*` 환경 변수로도 설정 가능하다. 이는 CI/CD 파이프라인이나 컨테이너 환경에서 별도의 설정 파일 없이 사용할 수 있게 한다.

### 연결 설정 테이블

| 설정 | 플래그 | 환경 변수 | 기본값 |
|------|--------|----------|--------|
| 서버 주소 | `--addr` | `LOKI_ADDR` | `http://localhost:3100` |
| 사용자명 | `--username` | `LOKI_USERNAME` | (없음) |
| 비밀번호 | `--password` | `LOKI_PASSWORD` | (없음) |
| 테넌트 ID | `--org-id` | `LOKI_ORG_ID` | (없음) |
| CA 인증서 | `--ca-cert` | `LOKI_CA_CERT_PATH` | (없음) |
| TLS 건너뛰기 | `--tls-skip-verify` | `LOKI_TLS_SKIP_VERIFY` | `false` |
| 클라이언트 인증서 | `--cert` | `LOKI_CLIENT_CERT_PATH` | (없음) |
| 재시도 횟수 | `--retries` | `LOKI_CLIENT_RETRIES` | `0` |
| 압축 사용 | `--compress` | `LOKI_HTTP_COMPRESSION` | `false` |

### FileClient (stdin 모드)

`--stdin` 플래그가 설정되면 Loki 서버 대신 stdin에서 로그를 읽는다(`cmd/logcli/main.go` 라인 393-417):

```go
if *stdin {
    queryClient = client.NewFileClient(os.Stdin)
    if rangeQuery.Step.Seconds() == 0 {
        // stdin 모드에서는 서버측 step 계산이 없으므로 직접 계산
        rangeQuery.Step = defaultQueryRangeStep(rangeQuery.Start, rangeQuery.End)
    }
    // 스트림 셀렉터가 없으면 더미 셀렉터 주입
    qs := strings.TrimSpace(rangeQuery.QueryString)
    if strings.HasPrefix(qs, "|") || strings.HasPrefix(qs, "!") {
        rangeQuery.QueryString = `{source="logcli"}` + rangeQuery.QueryString
    }
    rangeQuery.Limit = 0  // stdin에서는 limit 무의미
}
```

**왜 더미 셀렉터를 주입하는가?** LogQL 파서는 `{label="value"}` 형식의 스트림 셀렉터를 반드시 요구한다. 하지만 stdin 모드에서는 이미 로그가 주어져 있으므로, 사용자가 `|="error"` 같은 필터만 입력할 수 있도록 `{source="logcli"}`라는 더미 셀렉터를 자동으로 주입한다.

---

## 5. LogCLI 쿼리 실행 흐름

### 메인 디스패치

`main()` 함수(`cmd/logcli/main.go` 라인 359-535)에서 파싱된 커맨드에 따라 분기한다:

```go
switch cmd {
case queryCmd.FullCommand():
    // 범위 쿼리: tail/단일/병렬
    if *tail || *follow {
        rangeQuery.TailQuery(...)     // WebSocket 기반 라이브 스트리밍
    } else if rangeQuery.ParallelMaxWorkers == 1 {
        rangeQuery.DoQuery(...)       // 단일 스레드 범위 쿼리
    } else {
        rangeQuery.Limit = 0
        rangeQuery.DoQueryParallel(...)  // 병렬 범위 쿼리
    }
case instantQueryCmd.FullCommand():
    instantQuery.DoQuery(...)         // 인스턴트 메트릭 쿼리
case labelsCmd.FullCommand():
    labelsQuery.DoLabels(...)         // 레이블 조회
case seriesCmd.FullCommand():
    seriesQuery.DoSeries(...)         // 시리즈 조회
case fmtCmd.FullCommand():
    formatLogQL(os.Stdin, os.Stdout)  // LogQL 포맷팅
// ...
}
```

### 쿼리 실행 시퀀스

```
사용자 입력                    LogCLI                           Loki
    │                           │                                │
    │  logcli query '{app="web"}'                                │
    │──────────────────────────→│                                │
    │                           │  mustParse(from, to)           │
    │                           │  → 시간 범위 계산               │
    │                           │                                │
    │                           │  GET /loki/api/v1/query_range  │
    │                           │──────────────────────────────→ │
    │                           │                                │
    │                           │  ← JSON 응답                   │
    │                           │←────────────────────────────── │
    │                           │                                │
    │                           │  output.NewLogOutput()         │
    │                           │  → 포매터로 결과 출력           │
    │  ← 포맷된 로그 출력        │                                │
    │←─────────────────────────│                                │
```

### 출력 타임스탬프 포맷

`cmd/logcli/main.go` 라인 432-449에서 타임스탬프 포맷을 설정한다:

```go
switch *outputTimestampFmt {
case "rfc3339nano":
    outputOptions.TimestampFormat = time.RFC3339Nano
case "rfc822z":
    outputOptions.TimestampFormat = time.RFC822Z
case "stampmilli":
    outputOptions.TimestampFormat = time.StampMilli
// ... 기타 포맷
default:
    outputOptions.TimestampFormat = time.RFC3339
}
```

### 프로파일링 지원

LogCLI는 자체적으로 CPU/메모리 프로파일링을 지원한다(`cmd/logcli/main.go` 라인 368-391):

```go
if cpuProfile != nil && *cpuProfile != "" {
    cpuFile, _ := os.Create(*cpuProfile)
    defer cpuFile.Close()
    pprof.StartCPUProfile(cpuFile)
    defer pprof.StopCPUProfile()
}

if memProfile != nil && *memProfile != "" {
    memFile, _ := os.Create(*memProfile)
    defer memFile.Close()
    defer func() {
        pprof.WriteHeapProfile(memFile)
    }()
}
```

**왜 CLI에 프로파일링을 내장하는가?** 대량 로그를 병렬 다운로드할 때 클라이언트 측에서도 병목이 발생할 수 있다. `--cpuprofile`과 `--memprofile` 플래그로 클라이언트 자체의 성능 문제를 진단할 수 있다.

---

## 6. LogCLI 병렬 쿼리

### 병렬 다운로드 설계

LogCLI의 가장 강력한 기능 중 하나는 **시간 범위 분할 기반 병렬 쿼리**이다.

```
전체 시간 범위: 10:00 ─────────────────────────── 20:00
                 │                                    │
                 ├──15m──┤──15m──┤──15m──┤... (40개 작업)
                 │       │       │       │
              Worker 1  Worker 2  Worker 3  Worker 4
              (큐에서   (큐에서   (큐에서   (큐에서
               작업 가져옴) 작업 가져옴) 작업 가져옴) 작업 가져옴)
```

### 관련 플래그

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--parallel-duration` | `1h` | 각 작업의 시간 범위 |
| `--parallel-max-workers` | `1` | 병렬 워커 수 (1이면 비활성) |
| `--part-path-prefix` | (없음) | 파트 파일 저장 경로 접두사 |
| `--overwrite-completed-parts` | `false` | 완료된 파트 재다운로드 |
| `--merge-parts` | `false` | 파트 파일을 순서대로 병합 출력 |
| `--keep-parts` | `false` | 병합 후 파트 파일 유지 |

### 파트 파일 시스템

```
/tmp/my_query_20210119T183000_20210119T184500.part.tmp   ← 다운로드 중
/tmp/my_query_20210119T184500_20210119T190000.part.tmp   ← 다운로드 중
/tmp/my_query_20210119T190000_20210119T191500.part       ← 완료
/tmp/my_query_20210119T191500_20210119T193000.part       ← 완료
```

**왜 파트 파일을 사용하는가?**
1. **재개 가능**: 네트워크 오류 시 완료된 파트는 다시 다운로드하지 않음
2. **순서 보장**: 파일명에 시간 범위가 포함되어 있어 나중에 정렬 가능
3. **진행률 추적**: `.part.tmp`과 `.part` 확장자로 상태 구분

### step 자동 계산

서버 사이드와 동일한 로직으로 `step`을 계산한다(`cmd/logcli/main.go` 라인 736-739):

```go
func defaultQueryRangeStep(start, end time.Time) time.Duration {
    step := int(math.Max(math.Floor(end.Sub(start).Seconds()/250), 1))
    return time.Duration(step) * time.Second
}
```

이는 Loki 서버의 `pkg/loghttp/params.go`와 동일한 로직이다. **왜 250으로 나누는가?** Grafana에서 그래프 해상도가 일반적으로 250 데이터 포인트 정도이기 때문이다.

---

## 7. LogCLI 출력 시스템

### 출력 모드

| 모드 | 설명 | 사용 시나리오 |
|------|------|-------------|
| `default` | 타임스탬프 + 레이블 + 로그 라인 | 대화형 디버깅 |
| `raw` | 로그 라인만 | 파이프라인 처리, 다른 도구 연동 |
| `jsonl` | Loki API JSON 응답 그대로 | 프로그래밍 처리, 자동화 |

### 출력 파이프라인

```
Loki API 응답
    │
    ▼
output.NewLogOutput(os.Stdout, mode, options)
    │
    ├─ mode="default" → TimestampFormatter → LabelFormatter → LineWriter
    ├─ mode="raw"     → LineWriter (레이블, 타임스탬프 생략)
    └─ mode="jsonl"   → JSONEncoder → LineWriter
```

### 레이블 관련 플래그

```
--no-labels              레이블 출력 안 함
--exclude-label KEY      특정 레이블 제외
--include-label KEY      특정 레이블만 포함
--include-common-labels  공통 레이블도 포함
--labels-length N        레이블 고정 너비 패딩
--colored-output         레이블 색상 출력
```

---

## 8. Query Tee 아키텍처

### 소스 코드 구조

```
cmd/querytee/
├── main.go           ← 진입점, Config 정의, 라우트 등록

pkg/querytee/
├── proxy.go          ← HTTP 리버스 프록시 핵심 로직
├── proxy_endpoint.go ← 개별 엔드포인트 프록시 핸들러
├── instrumentation.go← 메트릭 서버
└── comparator/       ← 응답 비교기
    └── samples_comparator.go  ← 샘플 값 비교 로직
```

### 전체 아키텍처

```
                              ┌──────────────────────────────────────┐
                              │           Query Tee                   │
                              │                                      │
   ┌──────────┐   HTTP 요청   │  ┌──────────┐    ┌───────────────┐  │
   │ Grafana  │──────────────→│  │  Proxy   │───→│ Backend A     │  │
   │ LogCLI   │               │  │  Router  │    │ (preferred)   │  │
   │ etc.     │               │  │          │───→│ Backend B     │  │
   └──────────┘               │  └──────────┘    │ (comparison)  │  │
       ▲                      │       │          └───────────────┘  │
       │                      │  ┌────▼─────┐                      │
       │   응답 (Backend A)    │  │ Response │                      │
       │←─────────────────────│  │Comparator│→ 메트릭 기록          │
                              │  └──────────┘                      │
                              │       │                             │
                              │  ┌────▼──────────┐                 │
                              │  │ Metrics Server │ :9900           │
                              │  └───────────────┘                 │
                              └──────────────────────────────────────┘
```

### Config 구조

`cmd/querytee/main.go` 라인 22-27에서 정의한 설정:

```go
type Config struct {
    ServerMetricsPort int
    LogLevel          log.Level
    ProxyConfig       querytee.ProxyConfig
    Tracing           loki_tracing.Config
}
```

### 초기화 흐름

`main()` 함수(`cmd/querytee/main.go` 라인 29-82)의 실행 순서:

```
1. CLI 플래그 파싱 (flag.Parse)
     │
2. 로거 초기화 (util_log.InitLogger)
     │
3. 트레이싱 초기화 (조건부)
     │  if cfg.Tracing.Enabled:
     │    tracing.NewOTelOrJaegerFromEnv("loki-querytee", ...)
     │
4. 메트릭 레지스트리 생성
     │  prometheus.NewRegistry()
     │
5. Instrumentation 서버 시작 (:9900)
     │  querytee.NewInstrumentationServer(...)
     │
6. 프록시 생성 및 시작
     │  querytee.NewProxy(cfg.ProxyConfig, ..., lokiReadRoutes(), lokiWriteRoutes(), ...)
     │
7. proxy.Await() — 종료 대기
```

---

## 9. Query Tee 라우팅 설계

### Read 라우트

`lokiReadRoutes()` 함수(`cmd/querytee/main.go` 라인 89-109)에서 정의:

```go
func lokiReadRoutes(cfg Config) []querytee.Route {
    samplesComparator := comparator.NewSamplesComparator(
        comparator.SampleComparisonOptions{
            Tolerance:         cfg.ProxyConfig.ValueComparisonTolerance,
            UseRelativeError:  cfg.ProxyConfig.UseRelativeError,
            SkipRecentSamples: cfg.ProxyConfig.SkipRecentSamples,
            SkipSamplesBefore: time.Time(cfg.ProxyConfig.SkipSamplesBefore),
        })

    return []querytee.Route{
        {Path: "/loki/api/v1/query_range", RouteName: "api_v1_query_range",
         Methods: []string{"GET", "POST"}, ResponseComparator: samplesComparator},
        {Path: "/loki/api/v1/query", RouteName: "api_v1_query",
         Methods: []string{"GET", "POST"}, ResponseComparator: samplesComparator},
        {Path: "/loki/api/v1/label", RouteName: "api_v1_label",
         Methods: []string{"GET"}, ResponseComparator: nil},
        // ... 기타 라우트
    }
}
```

### Write 라우트

```go
func lokiWriteRoutes() []querytee.Route {
    return []querytee.Route{
        {Path: "/loki/api/v1/push", RouteName: "api_v1_push",
         Methods: []string{"POST"}, ResponseComparator: nil},
        {Path: "/api/prom/push", RouteName: "api_prom_push",
         Methods: []string{"POST"}, ResponseComparator: nil},
    }
}
```

### 라우트 매핑 테이블

| 라우트 | 메서드 | 비교기 | 설명 |
|--------|--------|--------|------|
| `/loki/api/v1/query_range` | GET, POST | SamplesComparator | 범위 쿼리 |
| `/loki/api/v1/query` | GET, POST | SamplesComparator | 인스턴트 쿼리 |
| `/loki/api/v1/label` | GET | nil | 레이블 이름 |
| `/loki/api/v1/labels` | GET | nil | 레이블 목록 |
| `/loki/api/v1/label/{name}/values` | GET | nil | 레이블 값 |
| `/loki/api/v1/series` | GET | nil | 시리즈 조회 |
| `/loki/api/v1/push` | POST | nil | 로그 쓰기 |

**왜 일부 라우트에는 비교기가 없는가?** 레이블/시리즈 조회는 순서가 다를 수 있어 단순 비교가 의미 없고, 쓰기 라우트는 응답이 단순한 성공/실패이므로 비교가 불필요하다. 반면 쿼리 라우트는 결과 값의 정확성이 중요하므로 SamplesComparator를 사용한다.

---

## 10. Query Tee 응답 비교

### SamplesComparator

`pkg/querytee/comparator/samples_comparator.go`에서 구현하는 비교 옵션:

```
SampleComparisonOptions
├── Tolerance          ← 절대 오차 허용 범위
├── UseRelativeError   ← 상대 오차 사용 여부
├── SkipRecentSamples  ← 최근 N 시간 샘플 제외
└── SkipSamplesBefore  ← 특정 시점 이전 샘플 제외
```

### 비교 프로세스

```
Backend A 응답 ──┐
                 ├──→ SamplesComparator.Compare()
Backend B 응답 ──┘
                        │
                        ├─ 응답 포맷 파싱 (JSON)
                        │
                        ├─ 시리즈 매칭
                        │   (레이블셋으로 동일 시리즈 찾기)
                        │
                        ├─ 샘플 값 비교
                        │   ├─ 절대 오차: |a - b| < tolerance
                        │   └─ 상대 오차: |a - b| / max(|a|, |b|) < tolerance
                        │
                        └─ 결과: match / mismatch → 메트릭 기록
```

### 왜 Tolerance가 필요한가?

분산 시스템에서는 두 인스턴스가 정확히 같은 시점의 데이터를 가지고 있지 않을 수 있다. 특히:

1. **부동소수점 연산 차이**: rate/avg 같은 메트릭 쿼리에서 미세한 차이 발생
2. **타이밍 차이**: 인제스트 타이밍이 약간 다르면 최근 데이터에서 차이 발생
3. **샤딩 차이**: 다른 샤딩 전략을 사용하면 집계 결과가 미세하게 다를 수 있음

`SkipRecentSamples` 옵션은 아직 완전히 복제되지 않은 최근 데이터를 비교에서 제외하는 데 사용된다.

---

## 11. 통합 설계 분석

### LogCLI와 Query Tee의 관계

두 도구는 Loki의 **HTTP API**를 공통 인터페이스로 사용한다:

```
                    ┌─────────────────┐
                    │  Loki HTTP API  │
                    │  /loki/api/v1/* │
                    └────────┬────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
        ┌─────▼─────┐ ┌─────▼─────┐ ┌─────▼──────┐
        │  LogCLI   │ │ Query Tee │ │  Grafana   │
        │  (직접)   │ │  (프록시)  │ │  (UI)      │
        └───────────┘ └───────────┘ └────────────┘
```

### 운영 워크플로우

```
[평상시]
LogCLI → Loki (디버깅, 애드혹 쿼리)

[마이그레이션 준비]
Grafana → Query Tee → Loki A (기존) + Loki B (신규)
                       │
                  메트릭/비교 결과 확인

[마이그레이션 완료]
LogCLI → Loki B (새 인스턴스)
```

### API 경로 매핑

| 용도 | LogCLI 커맨드 | Loki API 경로 | Query Tee 라우트 |
|------|--------------|---------------|------------------|
| 범위 쿼리 | `query` | `/loki/api/v1/query_range` | 비교 활성 |
| 인스턴트 쿼리 | `instant-query` | `/loki/api/v1/query` | 비교 활성 |
| 레이블 조회 | `labels` | `/loki/api/v1/labels` | 비교 비활성 |
| 시리즈 조회 | `series` | `/loki/api/v1/series` | 비교 비활성 |
| 볼륨 조회 | `volume` | `/loki/api/v1/index/volume` | 라우트 없음 |
| 로그 푸시 | N/A | `/loki/api/v1/push` | 비교 비활성 |

---

## 12. 성능 및 운영 고려사항

### LogCLI 성능 최적화

| 기법 | 설명 | 관련 플래그 |
|------|------|-----------|
| 병렬 다운로드 | 시간 범위를 분할하여 동시 쿼리 | `--parallel-max-workers` |
| 배치 크기 조절 | 한 번에 가져오는 로그 수 | `--batch` |
| 압축 전송 | HTTP 압축으로 네트워크 절약 | `--compress` |
| 재시도 | 일시적 오류에 대한 자동 재시도 | `--retries`, `--min/max-backoff` |
| 프로파일링 | 클라이언트 성능 병목 진단 | `--cpuprofile`, `--memprofile` |

### Query Tee 운영 지침

```
┌─────────────────────────────────────────────────┐
│                Query Tee 배포 토폴로지             │
│                                                  │
│  Grafana ─→ Query Tee (:80) ─┬→ Loki A (:3100)  │
│                              └→ Loki B (:3200)   │
│                                                  │
│  Prometheus ←── Metrics (:9900)                  │
│                                                  │
│  모니터링 대시보드:                                │
│  - querytee_response_time_seconds                │
│  - querytee_comparison_result                    │
│  - querytee_backend_errors_total                 │
└─────────────────────────────────────────────────┘
```

### 주의 사항

1. **Query Tee는 두 배의 쿼리 부하**: 동일 쿼리가 양쪽 백엔드에 전달되므로, 각 백엔드의 용량을 사전에 확인해야 한다
2. **응답 시간은 느린 쪽을 따름**: 비교를 위해 양쪽 응답을 모두 기다리므로, 전체 응답 시간은 느린 백엔드에 의해 결정된다
3. **쓰기 라우트 주의**: push 라우트가 활성화되면 양쪽에 동일 데이터가 기록되므로, 의도적인 것인지 확인해야 한다
4. **LogCLI의 `--limit 0`은 전체 데이터**: 병렬 모드와 stdin 모드에서 자동으로 설정되지만, 대량 데이터에 주의

### 삭제 기능 (LogCLI)

LogCLI는 로그 삭제 기능도 제공한다(`cmd/logcli/main.go` 라인 324-357):

```
logcli delete create '{job="app"}' --from="2023-01-01T00:00:00Z" --to="2023-01-02T00:00:00Z"
logcli delete list
logcli delete cancel --request-id="abc123"
```

삭제 요청은 즉시 실행되지 않고, Compactor가 주기적으로 Mark-Sweep을 실행할 때 처리된다. `delete list`로 진행 상태를 확인하고, 필요시 `delete cancel`로 취소할 수 있다.

---

## 부록: 주요 소스 파일 참조

| 파일 | 설명 |
|------|------|
| `cmd/logcli/main.go` | LogCLI 진입점, 커맨드/플래그 등록, 디스패치 (897줄) |
| `cmd/querytee/main.go` | Query Tee 진입점, 라우트 등록 (117줄) |
| `pkg/logcli/client/client.go` | Client 인터페이스 정의 |
| `pkg/logcli/query/query.go` | DoQuery, DoQueryParallel, TailQuery |
| `pkg/logcli/output/` | 출력 포매터 |
| `pkg/querytee/proxy.go` | 프록시 핵심 로직 |
| `pkg/querytee/comparator/samples_comparator.go` | 응답 비교기 |
