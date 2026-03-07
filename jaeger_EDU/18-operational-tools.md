# 운영 도구

## 목차
1. [개요](#1-개요)
2. [Anonymizer (트레이스 익명화)](#2-anonymizer-트레이스-익명화)
3. [Anonymizer 내부 구현](#3-anonymizer-내부-구현)
4. [Anonymizer Writer와 출력 파일](#4-anonymizer-writer와-출력-파일)
5. [Anonymizer UI 변환](#5-anonymizer-ui-변환)
6. [Trace Generator (트레이스 생성기)](#6-trace-generator-트레이스-생성기)
7. [Trace Generator Worker 구현](#7-trace-generator-worker-구현)
8. [ES Index Cleaner](#8-es-index-cleaner)
9. [ES Index Cleaner 필터링 로직](#9-es-index-cleaner-필터링-로직)
10. [ES Rollover](#10-es-rollover)
11. [ES Rollover Init Action](#11-es-rollover-init-action)
12. [ES Rollover Rollover/Lookback Actions](#12-es-rollover-rolloverlookback-actions)
13. [Remote Storage](#13-remote-storage)
14. [Leader Election](#14-leader-election)
15. [운영 도구 통합 활용 가이드](#15-운영-도구-통합-활용-가이드)

---

## 1. 개요

Jaeger는 핵심 트레이싱 시스템 외에 다양한 운영 도구를 제공한다. 이 도구들은 트레이스 데이터의 관리, 테스트, 공유, 스토리지 유지보수 등 실운영에서 필요한 작업을 수행한다.

### 운영 도구 목록

| 도구 | 경로 | 목적 |
|------|------|------|
| Anonymizer | `cmd/anonymizer/` | 트레이스 데이터 익명화 (버그 리포트 공유용) |
| Trace Generator | `cmd/tracegen/` | 부하 테스트용 트레이스 생성 |
| ES Index Cleaner | `cmd/es-index-cleaner/` | 오래된 ES 인덱스 삭제 |
| ES Rollover | `cmd/es-rollover/` | ES 인덱스 롤오버 관리 |
| Remote Storage | `cmd/remote-storage/` | 로컬 스토리지를 gRPC로 공유 |
| Leader Election | `internal/leaderelection/` | 분산 리더 선출 |

### 아키텍처 위치

```
+-------------------+     +-------------------+     +-------------------+
|   Jaeger Core     |     |   운영 도구       |     |   스토리지        |
|                   |     |                   |     |                   |
| Collector         |     | Anonymizer        |     | Elasticsearch     |
| Query             |     | Trace Generator   |     | Cassandra         |
| Agent             |     | ES Index Cleaner  |     | Badger            |
| Ingester          |     | ES Rollover       |     | Memory            |
|                   |     | Remote Storage    |     |                   |
+-------------------+     +-------------------+     +-------------------+
```

---

## 2. Anonymizer (트레이스 익명화)

### 목적

Anonymizer는 Jaeger에서 수집한 트레이스 데이터를 익명화하는 도구이다. 서비스 이름, 오퍼레이션 이름, 태그 등 사이트 고유 정보를 해시하여, 보안에 민감한 트레이스를 외부에 공유하거나 버그 리포트에 첨부할 수 있게 한다.

### 소스 파일 구조

```
cmd/anonymizer/
├── main.go                              # 진입점
├── app/
│   ├── flags.go                         # CLI 플래그
│   ├── anonymizer/
│   │   └── anonymizer.go                # 익명화 핵심 로직
│   ├── writer/
│   │   └── writer.go                    # 파일 출력 (원본 + 익명화)
│   ├── query/
│   │   └── query.go                     # Jaeger Query 서비스 조회
│   └── uiconv/
│       ├── module.go                    # UI 변환 진입점
│       ├── extractor.go                 # 스팬 추출기
│       └── reader.go                    # JSON 스팬 리더
```

### CLI 플래그

소스 경로: `cmd/anonymizer/app/flags.go`

```go
type Options struct {
    QueryGRPCHostPort string
    MaxSpansCount     int
    TraceID           string
    OutputDir         string
    HashStandardTags  bool
    HashCustomTags    bool
    HashLogs          bool
    HashProcess       bool
    StartTime         int64
    EndTime           int64
}
```

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--query-host-port` | `localhost:16686` | Jaeger Query gRPC 엔드포인트 |
| `--output-dir` | `/tmp` | 출력 디렉토리 |
| `--trace-id` | (필수) | 익명화할 트레이스 ID |
| `--hash-standard-tags` | `false` | 표준 태그 해시 여부 |
| `--hash-custom-tags` | `false` | 커스텀 태그 해시 여부 |
| `--hash-logs` | `false` | 로그 해시 여부 |
| `--hash-process` | `false` | 프로세스 태그 해시 여부 |
| `--max-spans-count` | `-1` (무제한) | 최대 스팬 수 |
| `--start-time` | `0` | 검색 시작 시간 (Unix nano) |
| `--end-time` | `0` | 검색 종료 시간 (Unix nano) |

### 실행 흐름

소스 경로: `cmd/anonymizer/main.go`

```go
func main() {
    options := app.Options{}

    command := &cobra.Command{
        Use:   "jaeger-anonymizer",
        Short: "Jaeger anonymizer hashes fields of a trace for easy sharing",
        Run: func(_ *cobra.Command, _ []string) {
            prefix := options.OutputDir + "/" + options.TraceID

            // 1. Writer 생성 (원본 + 익명화 + 매핑 파일)
            conf := writer.Config{
                MaxSpansCount:  options.MaxSpansCount,
                CapturedFile:   prefix + ".original.json",
                AnonymizedFile: prefix + ".anonymized.json",
                MappingFile:    prefix + ".mapping.json",
                AnonymizerOpts: anonymizer.Options{...},
            }
            w, _ := writer.New(conf, logger)

            // 2. Query 서비스에서 트레이스 조회
            query, _ := query.New(options.QueryGRPCHostPort)
            spans, _ := query.QueryTrace(options.TraceID,
                initTime(options.StartTime), initTime(options.EndTime))
            query.Close()

            // 3. 각 스팬 익명화 후 파일에 기록
            for i := range spans {
                span := &spans[i]
                w.WriteSpan(span)
            }
            w.Close()

            // 4. UI 호환 형식으로 변환
            uiCfg := uiconv.Config{
                CapturedFile: conf.AnonymizedFile,
                UIFile:       prefix + ".anonymized-ui-trace.json",
                TraceID:      options.TraceID,
            }
            uiconv.Extract(uiCfg, logger)
        },
    }
}
```

### 출력 파일

```
/tmp/{trace-id}.original.json           # 원본 스팬 (JSON 배열)
/tmp/{trace-id}.anonymized.json         # 익명화된 스팬 (JSON 배열)
/tmp/{trace-id}.mapping.json            # 원본->해시 매핑
/tmp/{trace-id}.anonymized-ui-trace.json # Jaeger UI 호환 형식
```

### Query 클라이언트

소스 경로: `cmd/anonymizer/app/query/query.go`

```go
type Query struct {
    client api_v2.QueryServiceClient
    conn   *grpc.ClientConn
}

func New(addr string) (*Query, error) {
    conn, err := grpc.NewClient(addr,
        grpc.WithTransportCredentials(insecure.NewCredentials()))
    return &Query{
        client: api_v2.NewQueryServiceClient(conn),
        conn:   conn,
    }, nil
}

func (q *Query) QueryTrace(traceID string, startTime time.Time,
                            endTime time.Time) ([]model.Span, error) {
    mTraceID, _ := model.TraceIDFromString(traceID)
    request := api_v2.GetTraceRequest{
        TraceID:   mTraceID,
        StartTime: startTime,
        EndTime:   endTime,
    }

    stream, _ := q.client.GetTrace(context.Background(), &request)

    var spans []model.Span
    for received, err := stream.Recv(); !errors.Is(err, io.EOF);
        received, err = stream.Recv() {
        spans = append(spans, received.Spans...)
    }
    return spans, nil
}
```

---

## 3. Anonymizer 내부 구현

### Anonymizer 구조체

소스 경로: `cmd/anonymizer/app/anonymizer/anonymizer.go`

```go
type Anonymizer struct {
    mappingFile string
    logger      *zap.Logger
    lock        sync.Mutex
    mapping     mapping
    options     Options
    cancel      context.CancelFunc
    wg          sync.WaitGroup
}

type mapping struct {
    Services   map[string]string
    Operations map[string]string  // key=[service]:operation
}

type Options struct {
    HashStandardTags bool
    HashCustomTags   bool
    HashLogs         bool
    HashProcess      bool
}
```

### 허용 태그 목록

```go
var allowedTags = map[string]bool{
    "error":               true,
    "http.method":         true,
    "http.status_code":    true,
    model.SpanKindKey:     true,   // "span.kind"
    model.SamplerTypeKey:  true,   // "sampler.type"
    model.SamplerParamKey: true,   // "sampler.param"
}
```

이 6개 태그는 "허용 태그"로, 익명화 시에도 기본적으로 보존된다. 트레이스의 구조적 특성을 유지하면서 사이트 고유 정보만 제거하기 위한 것이다.

### FNV64 해시 함수

```go
func hash(value string) string {
    h := fnv.New64()
    _, _ = h.Write([]byte(value))
    return fmt.Sprintf("%016x", h.Sum64())
}
```

FNV-1a 64비트 해시를 사용하는 이유:
- **빠름**: 암호학적 해시(SHA-256 등)보다 훨씬 빠르다
- **일관성**: 같은 입력에 항상 같은 출력 (매핑 파일과 일치)
- **16자리 16진수**: 읽기 쉽고 인덱스로 사용 가능

### 서비스/오퍼레이션 이름 해시

```go
func (a *Anonymizer) mapServiceName(service string) string {
    return a.mapString(service, a.mapping.Services)
}

func (a *Anonymizer) mapOperationName(service, operation string) string {
    v := fmt.Sprintf("[%s]:%s", service, operation)
    return a.mapString(v, a.mapping.Operations)
}

func (a *Anonymizer) mapString(v string, m map[string]string) string {
    a.lock.Lock()
    defer a.lock.Unlock()
    if s, ok := m[v]; ok {
        return s
    }
    s := hash(v)
    m[v] = s
    return s
}
```

오퍼레이션 이름은 `[서비스명]:오퍼레이션명` 형식으로 결합하여 해시한다. 이는 서로 다른 서비스에서 동일한 이름의 오퍼레이션이 구별되도록 하기 위함이다.

### AnonymizeSpan: 핵심 변환 로직

```go
func (a *Anonymizer) AnonymizeSpan(span *model.Span) *uimodel.Span {
    service := span.Process.ServiceName

    // 1. 오퍼레이션 이름 해시
    span.OperationName = a.mapOperationName(service, span.OperationName)

    // 2. 표준 태그 필터링
    outputTags := filterStandardTags(span.Tags)

    // 3. HashStandardTags 옵션: 허용 태그도 해시
    if a.options.HashStandardTags {
        outputTags = hashTags(outputTags)
    }

    // 4. HashCustomTags 옵션: 비허용 태그를 해시하여 포함
    if a.options.HashCustomTags {
        customTags := hashTags(filterCustomTags(span.Tags))
        outputTags = append(outputTags, customTags...)
    }
    span.Tags = outputTags

    // 5. HashLogs 옵션: 로그 해시 또는 제거
    if a.options.HashLogs {
        for _, log := range span.Logs {
            log.Fields = hashTags(log.Fields)
        }
    } else {
        span.Logs = nil
    }

    // 6. 서비스 이름 해시
    span.Process.ServiceName = a.mapServiceName(service)

    // 7. HashProcess 옵션: 프로세스 태그 해시 또는 제거
    if a.options.HashProcess {
        span.Process.Tags = hashTags(span.Process.Tags)
    } else {
        span.Process.Tags = nil
    }

    span.Warnings = nil

    // 8. UI 형식으로 변환
    return uiconv.FromDomainEmbedProcess(span)
}
```

### 태그 필터링 함수

```go
// 허용 태그만 남기기
func filterStandardTags(tags []model.KeyValue) []model.KeyValue {
    out := make([]model.KeyValue, 0, len(tags))
    for _, tag := range tags {
        if !allowedTags[tag.Key] {
            continue
        }
        // "error" 태그는 bool로 정규화
        if tag.Key == "error" {
            switch tag.VType {
            case model.BoolType:
                // 허용
            case model.StringType:
                if tag.VStr != "true" && tag.VStr != "false" {
                    tag = model.Bool("error", true)
                }
            default:
                tag = model.Bool("error", true)
            }
        }
        out = append(out, tag)
    }
    return out
}

// 허용 태그가 아닌 것만 남기기
func filterCustomTags(tags []model.KeyValue) []model.KeyValue {
    out := make([]model.KeyValue, 0, len(tags))
    for _, tag := range tags {
        if !allowedTags[tag.Key] {
            out = append(out, tag)
        }
    }
    return out
}

// 태그 키와 값 모두 해시
func hashTags(tags []model.KeyValue) []model.KeyValue {
    out := make([]model.KeyValue, 0, len(tags))
    for _, tag := range tags {
        kv := model.String(hash(tag.Key), hash(tag.AsString()))
        out = append(out, kv)
    }
    return out
}
```

### 옵션별 동작 비교

```
원본 스팬:
  Service: "payment-service"
  Operation: "POST /api/v1/charge"
  Tags: {
    "error": true,
    "http.method": "POST",
    "http.url": "https://internal.company.com/charge",
    "customer.id": "12345"
  }
  Process.Tags: { "hostname": "prod-web-01" }
  Logs: [{ "message": "payment failed for user X" }]

기본 옵션 (모두 false):
  Service: "a1b2c3d4e5f6g7h8"
  Operation: "9876543210abcdef"
  Tags: {
    "error": true,
    "http.method": "POST"
  }
  Process.Tags: nil     (제거됨)
  Logs: nil             (제거됨)

HashCustomTags=true:
  Tags: {
    "error": true,
    "http.method": "POST",
    "f0e1d2c3b4a5": "1234567890abcdef",  (http.url 해시)
    "abcdef012345": "fedcba0987654321"   (customer.id 해시)
  }

HashStandardTags=true:
  Tags: {
    "abc...": "def...",   (error 해시)
    "ghi...": "jkl..."    (http.method 해시)
  }
```

### 매핑 파일과 주기적 저장

```go
func New(mappingFile string, options Options, logger *zap.Logger) *Anonymizer {
    // 이전 매핑 파일 로드 (있는 경우)
    if _, err := os.Stat(filepath.Clean(mappingFile)); err == nil {
        dat, _ := os.ReadFile(filepath.Clean(mappingFile))
        json.Unmarshal(dat, &a.mapping)
    }

    // 10초마다 매핑 파일 저장
    a.wg.Go(func() {
        ticker := time.NewTicker(10 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                a.SaveMapping()
            case <-ctx.Done():
                return
            }
        }
    })
    return a
}
```

### Why: 매핑 파일을 10초마다 저장하는 이유

대용량 트레이스 익명화는 시간이 오래 걸릴 수 있다. 프로세스가 중간에 종료되어도 그때까지의 매핑이 보존되어야 나중에 역추적(reverse lookup)이 가능하다.

### Why: 매핑 파일이 필요한 이유

FNV64는 단방향 해시이므로, 익명화된 트레이스에서 발견한 문제를 원본 서비스/오퍼레이션으로 역추적하려면 매핑이 필요하다. 연구자가 "서비스 `a1b2c3d4`에 문제가 있다"고 보고하면, 매핑 파일에서 원본 서비스 이름을 찾을 수 있다.

---

## 4. Anonymizer Writer와 출력 파일

### Writer 구조체

소스 경로: `cmd/anonymizer/app/writer/writer.go`

```go
type Config struct {
    MaxSpansCount  int
    CapturedFile   string
    AnonymizedFile string
    MappingFile    string
    AnonymizerOpts anonymizer.Options
}

type Writer struct {
    config         Config
    lock           sync.Mutex
    logger         *zap.Logger
    capturedFile   *os.File
    anonymizedFile *os.File
    anonymizer     *anonymizer.Anonymizer
    spanCount      int
}
```

### WriteSpan 메서드

```go
func (w *Writer) WriteSpan(msg *model.Span) error {
    w.lock.Lock()
    defer w.lock.Unlock()

    // 1. 원본 스팬을 JSON으로 마샬링하여 파일에 기록
    out := new(bytes.Buffer)
    new(jsonpb.Marshaler).Marshal(out, msg)
    if w.spanCount > 0 {
        w.capturedFile.WriteString(",\n")
    }
    w.capturedFile.Write(out.Bytes())
    w.capturedFile.Sync()

    // 2. 스팬 익명화
    span := w.anonymizer.AnonymizeSpan(msg)

    // 3. 익명화된 스팬을 파일에 기록
    dat, _ := json.Marshal(span)
    if w.spanCount > 0 {
        w.anonymizedFile.WriteString(",\n")
    }
    w.anonymizedFile.Write(dat)
    w.anonymizedFile.Sync()

    // 4. 카운터 및 제한 체크
    w.spanCount++
    if w.spanCount%100 == 0 {
        w.logger.Info("progress", zap.Int("numSpans", w.spanCount))
    }
    if w.config.MaxSpansCount > 0 && w.spanCount >= w.config.MaxSpansCount {
        w.Close()
        return ErrMaxSpansCountReached
    }
    return nil
}
```

### Close 메서드

```go
func (w *Writer) Close() {
    w.capturedFile.WriteString("\n]\n")
    w.capturedFile.Close()
    w.anonymizedFile.WriteString("\n]\n")
    w.anonymizedFile.Close()
    w.anonymizer.Stop()
    w.anonymizer.SaveMapping()  // 최종 매핑 저장
}
```

JSON 배열 형식을 유지하기 위해 시작 시 `[`, 종료 시 `]`를 작성한다.

---

## 5. Anonymizer UI 변환

### UI 변환 파이프라인

소스 경로: `cmd/anonymizer/app/uiconv/`

```go
// module.go
func Extract(config Config, logger *zap.Logger) error {
    reader, _ := newSpanReader(config.CapturedFile, logger)
    ext, _ := newExtractor(config.UIFile, config.TraceID, reader, logger)
    return ext.Run()
}
```

### spanReader: JSON 스팬 읽기

소스 경로: `cmd/anonymizer/app/uiconv/reader.go`

```go
type spanReader struct {
    logger       *zap.Logger
    capturedFile *os.File
    reader       *bufio.Reader
    spansRead    int
    eofReached   bool
}

func (r *spanReader) NextSpan() (*uimodel.Span, error) {
    if r.eofReached {
        return nil, errNoMoreSpans
    }
    if r.spansRead == 0 {
        b, _ := r.reader.ReadByte()
        if b != '[' {
            return nil, errors.New("file must begin with '['")
        }
    }
    s, _ := r.reader.ReadString('\n')
    if s[len(s)-2] == ',' {  // 콤마로 끝나면 제거
        s = s[0 : len(s)-2]
    } else {
        r.eofReached = true
    }
    var span uimodel.Span
    json.Unmarshal([]byte(s), &span)
    r.spansRead++
    return &span, nil
}
```

### extractor: UI 형식 변환

소스 경로: `cmd/anonymizer/app/uiconv/extractor.go`

```go
func (e *extractor) Run() error {
    var spans []uimodel.Span
    for span, err := e.reader.NextSpan(); err == nil;
        span, err = e.reader.NextSpan() {
        if string(span.TraceID) == e.traceID {
            spans = append(spans, *span)
        }
    }

    trace := uimodel.Trace{
        TraceID:   uimodel.TraceID(e.traceID),
        Spans:     spans,
        Processes: make(map[uimodel.ProcessID]uimodel.Process),
    }

    // 프로세스를 별도 맵으로 분리 (UI 형식 요구사항)
    for i := range spans {
        span := &spans[i]
        pid := uimodel.ProcessID(fmt.Sprintf("p%d", i))
        trace.Processes[pid] = *span.Process
        span.Process = nil
        span.ProcessID = pid
    }

    // {"data": [trace]} 형식으로 출력
    jsonBytes, _ := json.Marshal(trace)
    e.uiFile.WriteString(`{"data": [`)
    e.uiFile.Write(jsonBytes)
    e.uiFile.WriteString(`]}`)
    return nil
}
```

### 전체 Anonymizer 파이프라인

```
Jaeger Query                         Anonymizer
   |                                    |
   | gRPC GetTrace                      |
   +<-----------------------------------+
   |                                    |
   | 스팬 스트림 반환                   |
   +----------------------------------->+
                                        |
                                   +---------+
                                   | Writer  |
                                   +---------+
                                   |         |
                          원본 스팬|         |익명화 스팬
                                   v         v
                            .original.json  .anonymized.json
                                             |
                                   +---------+----------+
                                   | UI 변환기           |
                                   +--------------------+
                                             |
                                             v
                                   .anonymized-ui-trace.json
                                             |
                                             v
                                   Jaeger UI에서 열기
```

---

## 6. Trace Generator (트레이스 생성기)

### 목적

Trace Generator(tracegen)는 Jaeger 인프라의 부하 테스트와 검증을 위해 합성 트레이스를 생성하는 도구이다.

### 소스 파일 구조

```
cmd/tracegen/
├── main.go               # 진입점, OTEL 초기화
├── README.md              # 문서
├── Dockerfile             # 빌드 이미지
└── docker-compose.yml     # Docker Compose 설정

internal/tracegen/
├── config.go              # Config 구조체, Run() 함수
└── worker.go              # Worker 구현, 스팬 생성
```

### Config 구조체

소스 경로: `internal/tracegen/config.go`

```go
type Config struct {
    Workers       int
    Services      int
    Traces        int
    ChildSpans    int
    Attributes    int
    AttrKeys      int
    AttrValues    int
    Marshal       bool
    Debug         bool
    Firehose      bool
    Pause         time.Duration
    Duration      time.Duration
    Service       string
    TraceExporter string
}
```

### CLI 플래그

```go
func (c *Config) Flags(fs *flag.FlagSet) {
    fs.IntVar(&c.Workers, "workers", 1, "Number of workers (goroutines) to run")
    fs.IntVar(&c.Traces, "traces", 1, "Number of traces to generate per worker")
    fs.IntVar(&c.ChildSpans, "spans", 1, "Number of child spans per trace")
    fs.IntVar(&c.Attributes, "attrs", 11, "Number of attributes per child span")
    fs.IntVar(&c.AttrKeys, "attr-keys", 97, "Number of distinct attribute keys")
    fs.IntVar(&c.AttrValues, "attr-values", 1000, "Number of distinct values per attribute")
    fs.BoolVar(&c.Debug, "debug", false, "Set DEBUG flag on spans (force sampling)")
    fs.BoolVar(&c.Firehose, "firehose", false, "Set FIREHOSE flag (skip indexing)")
    fs.DurationVar(&c.Pause, "pause", time.Microsecond,
        "Sleep before finishing each span. 0 uses fake 123us duration.")
    fs.DurationVar(&c.Duration, "duration", 0,
        "How long to run the test (overrides -traces)")
    fs.StringVar(&c.Service, "service", "tracegen", "Service name prefix")
    fs.IntVar(&c.Services, "services", 1, "Number of unique service suffixes")
    fs.StringVar(&c.TraceExporter, "trace-exporter", "otlp-http",
        "Trace exporter (otlp-http|otlp-grpc|stdout)")
}
```

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--workers` | `1` | 워커 goroutine 수 |
| `--traces` | `1` | 워커당 생성할 트레이스 수 |
| `--spans` | `1` | 트레이스당 자식 스팬 수 |
| `--attrs` | `11` | 스팬당 속성 수 |
| `--attr-keys` | `97` | 고유 속성 키 수 |
| `--attr-values` | `1000` | 속성 값 범위 |
| `--debug` | `false` | 강제 샘플링 플래그 |
| `--firehose` | `false` | 인덱싱 건너뛰기 플래그 |
| `--pause` | `1us` | 스팬 완료 전 대기 시간 |
| `--duration` | `0` | 실행 지속 시간 (traces 대체) |
| `--service` | `tracegen` | 서비스 이름 접두사 |
| `--services` | `1` | 서비스 수 |
| `--trace-exporter` | `otlp-http` | 익스포터 타입 |

### Run 함수

```go
func Run(c *Config, tracers []trace.Tracer, logger *zap.Logger) error {
    if c.Duration > 0 {
        c.Traces = 0  // duration 모드에서는 traces 무시
    } else if c.Traces <= 0 {
        return errors.New("either `traces` or `duration` must be greater than 0")
    }

    wg := sync.WaitGroup{}
    var running uint32 = 1

    for i := 0; i < c.Workers; i++ {
        wg.Add(1)
        w := worker{
            id:      i,
            tracers: tracers,
            Config:  *c,
            running: &running,
            wg:      &wg,
            logger:  logger.With(zap.Int("worker", i)),
        }
        go w.simulateTraces()
    }

    if c.Duration > 0 {
        time.Sleep(c.Duration)
        atomic.StoreUint32(&running, 0)  // 모든 워커에 종료 신호
    }
    wg.Wait()
    return nil
}
```

### 익스포터 생성

소스 경로: `cmd/tracegen/main.go`

```go
func createOtelExporter(exporterType string) (sdktrace.SpanExporter, error) {
    switch exporterType {
    case "jaeger":
        return nil, errors.New("jaeger exporter is no longer supported, please use otlp")
    case "otlp", "otlp-http":
        client := otlptracehttp.NewClient(otlptracehttp.WithInsecure())
        return otlptrace.New(context.Background(), client)
    case "otlp-grpc":
        client := otlptracegrpc.NewClient(otlptracegrpc.WithInsecure())
        return otlptrace.New(context.Background(), client)
    case "stdout":
        return stdouttrace.New()
    default:
        return nil, fmt.Errorf("unrecognized exporter type %s", exporterType)
    }
}
```

### Adaptive Sampling 지원

```go
if flagAdaptiveSamplingEndpoint != "" {
    jaegerRemoteSampler := jaegerremote.New(
        svc,
        jaegerremote.WithSamplingServerURL(flagAdaptiveSamplingEndpoint),
        jaegerremote.WithSamplingRefreshInterval(5*time.Second),
        jaegerremote.WithInitialSampler(sdktrace.TraceIDRatioBased(0.5)),
    )
    opts = append(opts, sdktrace.WithSampler(jaegerRemoteSampler))
}
```

---

## 7. Trace Generator Worker 구현

### Worker 구조체

소스 경로: `internal/tracegen/worker.go`

```go
type worker struct {
    tracers []trace.Tracer
    running *uint32  // 공유 종료 플래그
    id      int
    Config
    wg     *sync.WaitGroup
    logger *zap.Logger

    // 내부 카운터
    traceNo   int
    attrKeyNo int
    attrValNo int
}

const fakeSpanDuration = 123 * time.Microsecond
```

### simulateTraces: 메인 루프

```go
func (w *worker) simulateTraces() {
    for atomic.LoadUint32(w.running) == 1 {
        svcNo := w.traceNo % len(w.tracers)
        w.simulateOneTrace(w.tracers[svcNo])
        w.traceNo++
        if w.Traces != 0 {
            if w.traceNo >= w.Traces {
                break
            }
        }
    }
    w.logger.Info(fmt.Sprintf("Worker %d generated %d traces", w.id, w.traceNo))
    w.wg.Done()
}
```

### simulateOneTrace: 트레이스 생성

```go
func (w *worker) simulateOneTrace(tracer trace.Tracer) {
    ctx := context.Background()
    attrs := []attribute.KeyValue{
        attribute.String("peer.service", "tracegen-server"),
        attribute.String("peer.host.ipv4", "1.1.1.1"),
    }
    if w.Debug {
        attrs = append(attrs, attribute.Bool("jaeger.debug", true))
    }
    if w.Firehose {
        attrs = append(attrs, attribute.Bool("jaeger.firehose", true))
    }

    start := time.Now()
    ctx, parent := tracer.Start(ctx, "lets-go",
        trace.WithSpanKind(trace.SpanKindServer),
        trace.WithAttributes(attrs...),
        trace.WithTimestamp(start),
    )

    w.simulateChildSpans(ctx, start, tracer)

    if w.Pause != 0 {
        parent.End()  // 실제 시간 사용
    } else {
        totalDuration := time.Duration(w.ChildSpans) * fakeSpanDuration
        parent.End(trace.WithTimestamp(start.Add(totalDuration)))
    }
}
```

### simulateChildSpans: 자식 스팬 생성

```go
func (w *worker) simulateChildSpans(ctx context.Context, start time.Time,
                                     tracer trace.Tracer) {
    for c := 0; c < w.ChildSpans; c++ {
        var attrs []attribute.KeyValue
        for a := 0; a < w.Attributes; a++ {
            key := fmt.Sprintf("attr_%02d", w.attrKeyNo)
            val := fmt.Sprintf("val_%02d", w.attrValNo)
            attrs = append(attrs, attribute.String(key, val))
            w.attrKeyNo = (w.attrKeyNo + 1) % w.AttrKeys
            w.attrValNo = (w.attrValNo + 1) % w.AttrValues
        }

        opts := []trace.SpanStartOption{
            trace.WithSpanKind(trace.SpanKindClient),
            trace.WithAttributes(attrs...),
        }
        childStart := start.Add(time.Duration(c) * fakeSpanDuration)

        if w.Pause == 0 {
            opts = append(opts, trace.WithTimestamp(childStart))
        }

        _, child := tracer.Start(ctx, fmt.Sprintf("child-span-%02d", c), opts...)

        if w.Pause != 0 {
            time.Sleep(w.Pause)
            child.End()
        } else {
            child.End(trace.WithTimestamp(childStart.Add(fakeSpanDuration)))
        }
    }
}
```

### 생성되는 트레이스 구조

```
[lets-go] (서버 스팬, 총 N*123us)
  |
  +-- [child-span-00] (클라이언트, 123us, attrs: attr_00..attr_10)
  +-- [child-span-01] (클라이언트, 123us, attrs: attr_11..attr_21)
  +-- [child-span-02] (클라이언트, 123us, attrs: attr_22..attr_32)
  ...
  +-- [child-span-N]  (클라이언트, 123us)
```

### Why: fakeSpanDuration이 123us인 이유

`pause=0`(기본이 아닌 명시적 0) 모드에서는 실제 sleep 없이 가짜 타임스탬프를 사용하여 최대 성능으로 트레이스를 생성한다. 123us는 식별하기 쉬운 매직 넘버로, Jaeger UI에서 생성된 트레이스를 실제 트레이스와 구별하는 데 도움이 된다.

### Why: 속성 키/값을 순환(rotate)하는 이유

```go
w.attrKeyNo = (w.attrKeyNo + 1) % w.AttrKeys    // 97개 키 순환
w.attrValNo = (w.attrValNo + 1) % w.AttrValues   // 1000개 값 순환
```

실제 시스템에서 속성의 카디널리티(고유 조합 수)는 다양하다. 순환 카운터를 통해 현실적인 카디널리티를 시뮬레이션하며, Elasticsearch 등 스토리지의 인덱싱 성능을 테스트할 수 있다.

---

## 8. ES Index Cleaner

### 목적

ES Index Cleaner는 Elasticsearch에 저장된 Jaeger 인덱스 중 N일보다 오래된 것을 자동으로 삭제하는 크론잡용 도구이다.

### 소스 파일 구조

```
cmd/es-index-cleaner/
├── main.go                # 진입점
├── app/
│   ├── flags.go           # Config, CLI 플래그
│   ├── index_filter.go    # 인덱스 필터링 로직
│   └── cutoff_time.go     # 삭제 기준 시간 계산
```

### 사용법

```bash
jaeger-es-index-cleaner NUM_OF_DAYS http://HOSTNAME:PORT
```

예: 7일보다 오래된 인덱스 삭제
```bash
jaeger-es-index-cleaner 7 http://elasticsearch:9200
```

### Config 구조체

소스 경로: `cmd/es-index-cleaner/app/flags.go`

```go
type Config struct {
    IndexPrefix              string
    Archive                  bool
    Rollover                 bool
    MasterNodeTimeoutSeconds int
    IndexDateSeparator       string
    Username                 string
    Password                 string
    TLSEnabled               bool
    TLSConfig                configtls.ClientConfig
}
```

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--index-prefix` | `""` | 인덱스 접두사 |
| `--archive` | `false` | 아카이브 인덱스 삭제 (롤오버 전용) |
| `--rollover` | `false` | 롤오버 인덱스 삭제 |
| `--timeout` | `120` | 마스터 노드 타임아웃 (초) |
| `--index-date-separator` | `-` | 날짜 구분자 |
| `--es.username` | `""` | ES 사용자명 |
| `--es.password` | `""` | ES 비밀번호 |

### Feature Gate: 상대 시간 삭제

```go
var relativeIndexCleaner *featuregate.Gate

func init() {
    relativeIndexCleaner = featuregate.GlobalRegistry().MustRegister(
        "es.index.relativeTimeIndexDeletion",
        featuregate.StageAlpha,
        featuregate.WithRegisterFromVersion("v2.5.0"),
        featuregate.WithRegisterDescription(
            "Controls whether the indices will be deleted relative to "+
            "the current time or tomorrow midnight."),
    )
}
```

### 삭제 기준 시간 계산

소스 경로: `cmd/es-index-cleaner/app/cutoff_time.go`

```go
func CalculateDeletionCutoff(currTime time.Time, numOfDays int,
                              relativeIndexEnabled bool) time.Time {
    year, month, day := currTime.Date()
    // 기본: 내일 자정 기준
    cutoffTime := time.Date(year, month, day, 0, 0, 0, 0, time.UTC).
        AddDate(0, 0, 1)

    // Feature gate 활성 시: 현재 시간 기준
    if relativeIndexEnabled {
        cutoffTime = currTime
    }

    return cutoffTime.Add(-time.Hour * 24 * time.Duration(numOfDays))
}
```

### 삭제 기준 시간 비교

```
현재 시간: 2024-03-15 14:30 UTC, NUM_OF_DAYS=7

기본 모드 (내일 자정 기준):
  기준점:  2024-03-16 00:00 UTC  (내일 자정)
  삭제 기준: 2024-03-09 00:00 UTC  (-7일)
  -> 3월 8일 이전 인덱스 삭제

상대 시간 모드:
  기준점:  2024-03-15 14:30 UTC  (현재)
  삭제 기준: 2024-03-08 14:30 UTC  (-7일)
  -> 3월 8일 14:30 이전 인덱스 삭제
```

### 실행 흐름

소스 경로: `cmd/es-index-cleaner/main.go`

```go
RunE: func(_ *cobra.Command, args []string) error {
    numOfDays, _ := strconv.Atoi(args[0])
    cfg.InitFromViper(v)

    // 1. TLS/HTTP 클라이언트 설정
    tlscfg, _ := cfg.TLSConfig.LoadTLSConfig(ctx)
    c := &http.Client{
        Timeout:   time.Duration(cfg.MasterNodeTimeoutSeconds) * time.Second,
        Transport: &http.Transport{TLSClientConfig: tlscfg},
    }

    // 2. ES 인덱스 목록 조회
    i := client.IndicesClient{Client: client.Client{
        Endpoint: args[1], Client: c,
        BasicAuth: basicAuth(cfg.Username, cfg.Password),
    }}
    indices, _ := i.GetJaegerIndices(cfg.IndexPrefix)

    // 3. 삭제 기준 시간 계산
    deleteIndicesBefore := app.CalculateDeletionCutoff(
        time.Now().UTC(), numOfDays, relativeIndexCleaner.IsEnabled())

    // 4. 필터링
    filter := &app.IndexFilter{
        IndexPrefix:          cfg.IndexPrefix,
        IndexDateSeparator:   cfg.IndexDateSeparator,
        Archive:              cfg.Archive,
        Rollover:             cfg.Rollover,
        DeleteBeforeThisDate: deleteIndicesBefore,
    }
    indices = filter.Filter(indices)

    // 5. 삭제
    if len(indices) == 0 {
        logger.Info("No indices to delete")
        return nil
    }
    return i.DeleteIndices(indices)
},
```

---

## 9. ES Index Cleaner 필터링 로직

### IndexFilter 구조체

소스 경로: `cmd/es-index-cleaner/app/index_filter.go`

```go
type IndexFilter struct {
    IndexPrefix          string
    IndexDateSeparator   string
    Archive              bool
    Rollover             bool
    DeleteBeforeThisDate time.Time
}

func (i *IndexFilter) Filter(indices []client.Index) []client.Index {
    indices = i.filterByPattern(indices)
    return filter.ByDate(indices, i.DeleteBeforeThisDate)
}
```

### 패턴 매칭

```go
func (i *IndexFilter) filterByPattern(indices []client.Index) []client.Index {
    var reg *regexp.Regexp
    switch {
    case i.Archive:
        // 아카이브: jaeger-span-archive-YYYYMM
        reg, _ = regexp.Compile(
            fmt.Sprintf("^%sjaeger-span-archive-\\d{6}", i.IndexPrefix))
    case i.Rollover:
        // 롤오버: jaeger-{span|service|dependencies|sampling}-YYYYMM
        reg, _ = regexp.Compile(
            fmt.Sprintf("^%sjaeger-(span|service|dependencies|sampling)-\\d{6}",
                i.IndexPrefix))
    default:
        // 일반: jaeger-{span|service|dependencies|sampling}-YYYY-MM-DD
        reg, _ = regexp.Compile(
            fmt.Sprintf("^%sjaeger-(span|service|dependencies|sampling)-"+
                "\\d{4}%s\\d{2}%s\\d{2}",
                i.IndexPrefix, i.IndexDateSeparator, i.IndexDateSeparator))
    }

    var filtered []client.Index
    for _, in := range indices {
        if reg.MatchString(in.Index) {
            // Write 별칭이 있는 인덱스는 삭제하지 않음
            if in.Aliases[i.IndexPrefix+"jaeger-span-write"] ||
               in.Aliases[i.IndexPrefix+"jaeger-service-write"] ||
               in.Aliases[i.IndexPrefix+"jaeger-span-archive-write"] ||
               in.Aliases[i.IndexPrefix+"jaeger-dependencies-write"] ||
               in.Aliases[i.IndexPrefix+"jaeger-sampling-write"] {
                continue
            }
            filtered = append(filtered, in)
        }
    }
    return filtered
}
```

### 인덱스 유형별 패턴

```
일반 모드:
  jaeger-span-2024-03-15           -> 매칭
  jaeger-service-2024-03-15        -> 매칭
  jaeger-dependencies-2024-03-15   -> 매칭

롤오버 모드:
  jaeger-span-000001               -> 매칭
  jaeger-service-000002            -> 매칭

아카이브 모드:
  jaeger-span-archive-000001       -> 매칭
```

### Write 별칭 보호

```
jaeger-span-000005 (aliases: jaeger-span-write)  -> 보호됨 (현재 쓰기 인덱스)
jaeger-span-000004 (aliases: jaeger-span-read)   -> 삭제 대상
jaeger-span-000003 (aliases: )                   -> 삭제 대상
```

### Why: Write 별칭이 있는 인덱스를 보호하는 이유

Write 별칭이 가리키는 인덱스는 현재 데이터가 기록되고 있는 활성 인덱스이다. 이를 삭제하면 데이터 유실이 발생하므로 절대 삭제하지 않는다.

---

## 10. ES Rollover

### 목적

ES Rollover는 Elasticsearch 인덱스의 생명주기를 관리하는 도구이다. 시간 기반 인덱싱 대신 롤오버 기반 인덱싱을 사용하여 인덱스 크기를 제어한다.

### 소스 파일 구조

```
cmd/es-rollover/
├── main.go                    # 진입점, 서브커맨드 등록
├── app/
│   ├── actions.go             # Action 인터페이스, ExecuteAction
│   ├── flags.go               # 전역 Config, CLI 플래그
│   ├── index_options.go       # IndexOption, 별칭 이름 생성
│   ├── init/
│   │   ├── action.go          # Init 액션 구현
│   │   └── flags.go           # Init 전용 플래그
│   ├── rollover/
│   │   ├── action.go          # Rollover 액션 구현
│   │   └── flags.go           # Rollover 전용 플래그
│   └── lookback/
│       ├── action.go          # Lookback 액션 구현
│       ├── flags.go           # Lookback 전용 플래그
│       └── time_reference.go  # 시간 참조 계산
```

### 3개의 서브커맨드

```bash
jaeger-es-rollover init     http://elasticsearch:9200
jaeger-es-rollover rollover http://elasticsearch:9200
jaeger-es-rollover lookback http://elasticsearch:9200
```

### 전역 Config

소스 경로: `cmd/es-rollover/app/flags.go`

```go
type Config struct {
    IndexPrefix      string
    Archive          bool
    Username         string
    Password         string
    TLSEnabled       bool
    ILMPolicyName    string
    UseILM           bool
    Timeout          int
    SkipDependencies bool
    AdaptiveSampling bool
    TLSConfig        configtls.ClientConfig
}
```

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--index-prefix` | `""` | 인덱스 접두사 |
| `--archive` | `false` | 아카이브 인덱스 처리 |
| `--es.use-ilm` | `false` | ILM 사용 여부 |
| `--es.ilm-policy-name` | `jaeger-ilm-policy` | ILM 정책 이름 |
| `--skip-dependencies` | `false` | 의존성 인덱스 롤오버 건너뛰기 |
| `--adaptive-sampling` | `false` | 적응형 샘플링 인덱스 포함 |
| `--timeout` | `120` | ES 타임아웃 (초) |

### Action 인터페이스

소스 경로: `cmd/es-rollover/app/actions.go`

```go
type Action interface {
    Do() error
}

func ExecuteAction(opts ActionExecuteOptions,
                   createAction ActionCreatorFunction) error {
    cfg := Config{}
    cfg.InitFromViper(opts.Viper)

    tlsCfg, _ := cfg.TLSConfig.LoadTLSConfig(ctx)
    esClient := newESClient(opts.Args[0], &cfg, tlsCfg)
    action := createAction(esClient, cfg)
    return action.Do()
}
```

### IndexOption: 인덱스 이름 규칙

소스 경로: `cmd/es-rollover/app/index_options.go`

```go
const (
    writeAliasFormat    = "%s-write"
    readAliasFormat     = "%s-read"
    rolloverIndexFormat = "%s-000001"
)

type IndexOption struct {
    prefix    string
    indexType string
    Mapping   string
}
```

### RolloverIndices: 인덱스 목록 생성

```go
func RolloverIndices(archive bool, skipDependencies bool,
                      adaptiveSampling bool, prefix string) []IndexOption {
    if archive {
        return []IndexOption{{
            prefix: prefix, indexType: "jaeger-span-archive",
            Mapping: "jaeger-span",
        }}
    }

    indexOptions := []IndexOption{
        {prefix: prefix, Mapping: "jaeger-span",    indexType: "jaeger-span"},
        {prefix: prefix, Mapping: "jaeger-service", indexType: "jaeger-service"},
    }

    if !skipDependencies {
        indexOptions = append(indexOptions, IndexOption{
            prefix: prefix, Mapping: "jaeger-dependencies",
            indexType: "jaeger-dependencies",
        })
    }

    if adaptiveSampling {
        indexOptions = append(indexOptions, IndexOption{
            prefix: prefix, Mapping: "jaeger-sampling",
            indexType: "jaeger-sampling",
        })
    }
    return indexOptions
}
```

### 인덱스 이름 생성 메서드

```go
func (i *IndexOption) IndexName() string {
    return strings.TrimLeft(fmt.Sprintf("%s%s", i.prefix, i.indexType), "-")
}

func (i *IndexOption) ReadAliasName() string {
    return fmt.Sprintf(readAliasFormat, i.IndexName())
}

func (i *IndexOption) WriteAliasName() string {
    return fmt.Sprintf(writeAliasFormat, i.IndexName())
}

func (i *IndexOption) InitialRolloverIndex() string {
    return fmt.Sprintf(rolloverIndexFormat, i.IndexName())
}
```

### 생성되는 이름 예시

```
prefix=""일 때:
  IndexName:           jaeger-span
  ReadAliasName:       jaeger-span-read
  WriteAliasName:      jaeger-span-write
  InitialRolloverIndex: jaeger-span-000001

prefix="prod-"일 때:
  IndexName:           prod-jaeger-span
  ReadAliasName:       prod-jaeger-span-read
  WriteAliasName:      prod-jaeger-span-write
  InitialRolloverIndex: prod-jaeger-span-000001
```

---

## 11. ES Rollover Init Action

### Init Action 구조체

소스 경로: `cmd/es-rollover/app/init/action.go`

```go
type Action struct {
    Config        Config
    ClusterClient client.ClusterAPI
    IndicesClient client.IndexAPI
    ILMClient     client.IndexManagementLifecycleAPI
}
```

### Init Action Do 메서드

```go
func (c Action) Do() error {
    // 1. ES 버전 확인
    version, _ := c.ClusterClient.Version()

    // 2. ILM 사용 시 검증
    if c.Config.UseILM {
        if version < ilmVersionSupport {  // 7
            return errors.New("ILM is supported only for ES version 7+")
        }
        policyExist, _ := c.ILMClient.Exists(c.Config.ILMPolicyName)
        if !policyExist {
            return fmt.Errorf("ILM policy %s doesn't exist", c.Config.ILMPolicyName)
        }
    }

    // 3. 각 인덱스 타입에 대해 초기화
    rolloverIndices := app.RolloverIndices(
        c.Config.Archive, c.Config.SkipDependencies,
        c.Config.AdaptiveSampling, c.Config.Config.IndexPrefix)
    for _, indexName := range rolloverIndices {
        c.init(version, indexName)
    }
    return nil
}
```

### init 메서드: 개별 인덱스 초기화

```go
func (c Action) init(version uint, indexopt app.IndexOption) error {
    // 1. 매핑 생성
    mappingType, _ := mappings.MappingTypeFromString(indexopt.Mapping)
    mapping, _ := c.getMapping(version, mappingType)

    // 2. 인덱스 템플릿 생성
    c.IndicesClient.CreateTemplate(mapping, indexopt.TemplateName())

    // 3. 초기 롤오버 인덱스 생성
    index := indexopt.InitialRolloverIndex()
    createIndexIfNotExist(c.IndicesClient, index)

    // 4. Read/Write 별칭 생성
    readAlias := indexopt.ReadAliasName()
    writeAlias := indexopt.WriteAliasName()
    aliases := []client.Alias{}

    if !filter.AliasExists(jaegerIndices, readAlias) {
        aliases = append(aliases, client.Alias{
            Index: index, Name: readAlias, IsWriteIndex: false,
        })
    }
    if !filter.AliasExists(jaegerIndices, writeAlias) {
        aliases = append(aliases, client.Alias{
            Index: index, Name: writeAlias,
            IsWriteIndex: c.Config.UseILM,
        })
    }

    if len(aliases) > 0 {
        c.IndicesClient.CreateAlias(aliases)
    }
    return nil
}
```

### Init 플래그

소스 경로: `cmd/es-rollover/app/init/flags.go`

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--shards` | `5` | 샤드 수 |
| `--replicas` | `1` | 레플리카 수 |
| `--priority-span-template` | `0` | 스팬 템플릿 우선순위 (ESv8) |
| `--priority-service-template` | `0` | 서비스 템플릿 우선순위 |
| `--priority-dependencies-template` | `0` | 의존성 템플릿 우선순위 |
| `--priority-sampling-template` | `0` | 샘플링 템플릿 우선순위 |

### Init 흐름도

```
jaeger-es-rollover init http://es:9200
    |
    v
[ES 클러스터 버전 확인]
    |
    +-- ILM 사용? --> [ILM 정책 존재 확인]
    |
    v
[각 인덱스 타입 (span, service, dependencies, sampling)]
    |
    +-- [1] 인덱스 템플릿 생성 (jaeger-span 등)
    +-- [2] 초기 인덱스 생성 (jaeger-span-000001)
    +-- [3] Read 별칭 생성 (jaeger-span-read -> jaeger-span-000001)
    +-- [4] Write 별칭 생성 (jaeger-span-write -> jaeger-span-000001)
```

---

## 12. ES Rollover Rollover/Lookback Actions

### Rollover Action

소스 경로: `cmd/es-rollover/app/rollover/action.go`

```go
type Action struct {
    Config
    IndicesClient client.IndexAPI
}

func (a *Action) Do() error {
    rolloverIndices := app.RolloverIndices(
        a.Config.Archive, a.Config.SkipDependencies,
        a.Config.AdaptiveSampling, a.Config.IndexPrefix)
    for _, indexName := range rolloverIndices {
        a.rollover(indexName)
    }
    return nil
}

func (a *Action) rollover(indexSet app.IndexOption) error {
    // 1. 롤오버 조건 파싱
    conditionsMap := map[string]any{}
    if a.Conditions != "" {
        json.Unmarshal([]byte(a.Config.Conditions), &conditionsMap)
    }

    // 2. Write 별칭으로 롤오버 실행
    writeAlias := indexSet.WriteAliasName()
    readAlias := indexSet.ReadAliasName()
    a.IndicesClient.Rollover(writeAlias, conditionsMap)

    // 3. 새 인덱스에 Read 별칭 추가
    jaegerIndex, _ := a.IndicesClient.GetJaegerIndices(a.Config.IndexPrefix)
    indicesWithWriteAlias := filter.ByAlias(jaegerIndex, []string{writeAlias})

    aliases := make([]client.Alias, 0, len(indicesWithWriteAlias))
    for _, index := range indicesWithWriteAlias {
        aliases = append(aliases, client.Alias{
            Index: index.Index, Name: readAlias,
        })
    }
    return a.IndicesClient.CreateAlias(aliases)
}
```

### Rollover 플래그

소스 경로: `cmd/es-rollover/app/rollover/flags.go`

```go
const (
    conditions               = "conditions"
    defaultRollbackCondition = "{\"max_age\": \"2d\"}"
)

type Config struct {
    app.Config
    Conditions string
}
```

기본 롤오버 조건: 인덱스 나이가 2일을 초과하면 롤오버한다.

### Rollover 흐름도

```
jaeger-es-rollover rollover http://es:9200
    |
    v
[각 인덱스 타입]
    |
    v
[ES _rollover API 호출]
    |
    +-- 조건 미충족 --> 아무 것도 하지 않음
    |
    +-- 조건 충족:
        |
        [1] ES가 새 인덱스 생성 (jaeger-span-000002)
        [2] Write 별칭 이동 (jaeger-span-write -> 000002)
        [3] Read 별칭 추가 (jaeger-span-read -> 000002)

결과:
  jaeger-span-000001: aliases [jaeger-span-read]
  jaeger-span-000002: aliases [jaeger-span-read, jaeger-span-write]
```

### Lookback Action

소스 경로: `cmd/es-rollover/app/lookback/action.go`

```go
type Action struct {
    Config
    IndicesClient client.IndexAPI
    Logger        *zap.Logger
}

func (a *Action) Do() error {
    rolloverIndices := app.RolloverIndices(
        a.Config.Archive, a.Config.SkipDependencies,
        a.Config.AdaptiveSampling, a.Config.IndexPrefix)
    for _, indexName := range rolloverIndices {
        a.lookback(indexName)
    }
    return nil
}

func (a *Action) lookback(indexSet app.IndexOption) error {
    jaegerIndex, _ := a.IndicesClient.GetJaegerIndices(a.Config.IndexPrefix)

    readAliasName := indexSet.ReadAliasName()
    readAliasIndices := filter.ByAlias(jaegerIndex, []string{readAliasName})

    // Write 별칭이 있는 인덱스 제외 (현재 활성 인덱스)
    excludedWriteIndex := filter.ByAliasExclude(
        readAliasIndices, []string{indexSet.WriteAliasName()})

    // 시간 기준으로 필터링
    finalIndices := filter.ByDate(excludedWriteIndex,
        getTimeReference(timeNow(), a.Unit, a.UnitCount))

    if len(finalIndices) == 0 {
        a.Logger.Info("No indices to remove from alias")
        return nil
    }

    // Read 별칭에서 제거
    aliases := make([]client.Alias, 0, len(finalIndices))
    for _, index := range finalIndices {
        aliases = append(aliases, client.Alias{
            Index: index.Index, Name: readAliasName,
        })
    }
    return a.IndicesClient.DeleteAlias(aliases)
}
```

### Lookback 플래그

소스 경로: `cmd/es-rollover/app/lookback/flags.go`

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--unit` | `days` | 시간 단위 (minutes/hours/days/weeks/months/years) |
| `--unit-count` | `1` | 단위 수 |

### 시간 참조 계산

소스 경로: `cmd/es-rollover/app/lookback/time_reference.go`

```go
func getTimeReference(currentTime time.Time, units string,
                       unitCount int) time.Time {
    switch units {
    case "minutes":
        return currentTime.Truncate(time.Minute).
            Add(-time.Duration(unitCount) * time.Minute)
    case "hours":
        return currentTime.Truncate(time.Hour).
            Add(-time.Duration(unitCount) * time.Hour)
    case "days":
        year, month, day := currentTime.Date()
        tomorrowMidnight := time.Date(year, month, day, 0, 0, 0, 0,
            currentTime.Location()).AddDate(0, 0, 1)
        return tomorrowMidnight.Add(
            -time.Hour * 24 * time.Duration(unitCount))
    case "weeks":
        // ... 7 * unitCount 일
    case "months":
        // ... AddDate(0, -unitCount, 0)
    case "years":
        // ... AddDate(-unitCount, 0, 0)
    default:
        return currentTime.Truncate(time.Second).
            Add(-time.Duration(unitCount) * time.Second)
    }
}
```

### Lookback 흐름도

```
jaeger-es-rollover lookback http://es:9200 --unit=days --unit-count=7
    |
    v
[Read 별칭이 가리키는 인덱스 목록 조회]
    |
    v
[Write 별칭이 있는 인덱스 제외]  (현재 활성 인덱스 보호)
    |
    v
[7일 이전 인덱스 필터링]
    |
    v
[해당 인덱스에서 Read 별칭 제거]

결과 (7일 lookback):
  jaeger-span-000001: aliases []          (read 별칭 제거됨, 인덱스는 남음)
  jaeger-span-000005: aliases [read]      (보존)
  jaeger-span-000006: aliases [read, write] (보존, 활성)
```

---

## 13. Remote Storage

### 목적

Remote Storage는 단일 노드 스토리지 구현(Memory, Badger)을 gRPC 서비스로 노출하여, 여러 Collector가 공유할 수 있게 하는 도구이다.

### 소스 파일 구조

```
cmd/remote-storage/
├── main.go              # 진입점
├── config.yaml          # 기본 설정 파일 (ES)
├── config-badger.yaml   # Badger 설정 파일
├── app/
│   ├── config.go        # Config 구조체, 로딩/검증
│   └── server.go        # gRPC 서버 구현
```

### Config 구조체

소스 경로: `cmd/remote-storage/app/config.go`

```go
type Config struct {
    GRPC    configgrpc.ServerConfig `mapstructure:"grpc"`
    Tenancy tenancy.Options         `mapstructure:"multi_tenancy"`
    Storage storageconfig.Config    `mapstructure:"storage"`
}

func DefaultConfig() *Config {
    return &Config{
        GRPC: configgrpc.ServerConfig{
            NetAddr: confignet.AddrConfig{
                Endpoint:  ":17271",
                Transport: confignet.TransportTypeTCP,
            },
        },
        Storage: storageconfig.Config{
            TraceBackends: map[string]storageconfig.TraceBackend{
                "memory": {
                    Memory: &memory.Configuration{
                        MaxTraces: 1_000_000,
                    },
                },
            },
        },
    }
}
```

### Server 구조체

소스 경로: `cmd/remote-storage/app/server.go`

```go
type Server struct {
    grpcCfg    configgrpc.ServerConfig
    grpcConn   net.Listener
    grpcServer *grpc.Server
    stopped    sync.WaitGroup
    telset     telemetry.Settings
}
```

### gRPC 서버 생성

```go
func createGRPCServer(ctx context.Context, cfg configgrpc.ServerConfig,
                       tm *tenancy.Manager, v2Handler *grpcstorage.Handler,
                       telset telemetry.Settings) (*grpc.Server, error) {
    unaryInterceptors := []grpc.UnaryServerInterceptor{
        bearertoken.NewUnaryServerInterceptor(),
    }
    streamInterceptors := []grpc.StreamServerInterceptor{
        bearertoken.NewStreamServerInterceptor(),
    }
    if tm.Enabled {
        unaryInterceptors = append(unaryInterceptors,
            tenancy.NewGuardingUnaryInterceptor(tm))
        streamInterceptors = append(streamInterceptors,
            tenancy.NewGuardingStreamInterceptor(tm))
    }

    server, _ := cfg.ToServer(ctx, extensions, telset.ToOtelComponent(),
        configgrpc.WithGrpcServerOption(
            grpc.ChainUnaryInterceptor(unaryInterceptors...)),
        configgrpc.WithGrpcServerOption(
            grpc.ChainStreamInterceptor(streamInterceptors...)),
    )

    // gRPC Health Checking Protocol
    healthServer := health.NewServer()
    reflection.Register(server)
    v2Handler.Register(server, healthServer)
    grpc_health_v1.RegisterHealthServer(server, healthServer)

    return server, nil
}
```

### 배포 아키텍처

```
+-------------------+     +-------------------+     +-------------------+
|  Collector 1      |     |  Collector 2      |     |  Collector 3      |
|                   |     |                   |     |                   |
+--------+----------+     +--------+----------+     +--------+----------+
         |                         |                         |
         +------------+------------+------------+------------+
                      |
                gRPC (:17271)
                      |
         +------------+------------+
         |   Remote Storage        |
         |                         |
         |   +------------------+  |
         |   | Memory/Badger    |  |
         |   | Storage Backend  |  |
         |   +------------------+  |
         +-------------------------+
```

### Why: Remote Storage가 필요한 이유

Memory와 Badger 스토리지는 단일 프로세스 내에서만 접근 가능하다. 여러 Collector가 동일한 스토리지를 공유하려면 네트워크를 통해 접근해야 하며, Remote Storage가 이 역할을 수행한다.

---

## 14. Leader Election

### 목적

Leader Election은 여러 Jaeger 인스턴스 중 하나를 리더로 선출하여, 적응형 샘플링 확률 계산 등 단일 실행이 필요한 작업을 조율한다.

### 소스 파일

```
internal/leaderelection/
├── leader_election.go       # DistributedElectionParticipant
├── leader_election_test.go
└── mocks/mocks.go
```

### ElectionParticipant 인터페이스

소스 경로: `internal/leaderelection/leader_election.go`

```go
type ElectionParticipant interface {
    io.Closer
    IsLeader() bool
    Start() error
}
```

### DistributedElectionParticipant 구조체

```go
type DistributedElectionParticipant struct {
    ElectionParticipantOptions
    lock         dl.Lock
    isLeader     atomic.Bool
    resourceName string
    closeChan    chan struct{}
    wg           sync.WaitGroup
}

type ElectionParticipantOptions struct {
    LeaderLeaseRefreshInterval   time.Duration
    FollowerLeaseRefreshInterval time.Duration
    Logger                       *zap.Logger
}
```

### 리더 선출 루프

```go
func (p *DistributedElectionParticipant) runAcquireLockLoop() {
    defer p.wg.Done()
    ticker := time.NewTicker(p.acquireLock())
    for {
        select {
        case <-ticker.C:
            ticker.Stop()
            ticker = time.NewTicker(p.acquireLock())
        case <-p.closeChan:
            ticker.Stop()
            return
        }
    }
}
```

### acquireLock: 차등 재시도 간격

```go
func (p *DistributedElectionParticipant) acquireLock() time.Duration {
    if acquiredLeaderLock, err := p.lock.Acquire(
        p.resourceName, p.FollowerLeaseRefreshInterval); err == nil {
        p.setLeader(acquiredLeaderLock)
    } else {
        p.Logger.Error(acquireLockErrMsg, zap.Error(err))
    }

    if p.IsLeader() {
        return p.LeaderLeaseRefreshInterval    // 리더: 빠른 갱신
    }
    return p.FollowerLeaseRefreshInterval       // 팔로워: 느린 갱신
}
```

### 리더/팔로워 갱신 간격 비교

```
리더:
  [Acquire]----[Acquire]----[Acquire]----[Acquire]
  |--- 짧은 간격 ---|--- 짧은 간격 ---|

팔로워:
  [Acquire]----------[Acquire]----------[Acquire]
  |------ 긴 간격 ------|------ 긴 간격 ------|
```

### Why: 리더와 팔로워의 갱신 간격이 다른 이유

1. **리더**: 짧은 간격으로 잠금을 갱신하여 리더십을 유지한다. 갱신이 늦으면 다른 인스턴스가 리더로 승격될 수 있다.

2. **팔로워**: 긴 간격으로 잠금을 시도하여 불필요한 부하를 줄인다. 리더가 실패하면 다음 시도에서 리더로 승격될 수 있다.

### 적응형 샘플링에서의 활용

```
+-------------------+     +-------------------+     +-------------------+
|  Collector 1      |     |  Collector 2      |     |  Collector 3      |
|  (Leader)         |     |  (Follower)       |     |  (Follower)       |
|                   |     |                   |     |                   |
|  확률 계산 수행   |     |  확률 조회만      |     |  확률 조회만      |
|  결과 저장        |     |                   |     |                   |
+-------------------+     +-------------------+     +-------------------+
         |
    분산 잠금 (Cassandra/ES)
         |
    +----+----+
    | Lock    |
    | Store   |
    +---------+
```

리더만 샘플링 확률을 계산하고 저장한다. 팔로워는 저장된 확률을 읽어서 사용한다. 이를 통해 여러 Collector가 동일한 샘플링 전략을 사용하면서도, 계산 비용을 하나의 인스턴스에만 집중시킬 수 있다.

---

## 15. 운영 도구 통합 활용 가이드

### 일반적인 운영 시나리오

#### 시나리오 1: 프로덕션 트레이스 공유

```bash
# 1. 트레이스 익명화
jaeger-anonymizer --trace-id abc123 \
    --query-host-port localhost:16686 \
    --hash-custom-tags \
    --output-dir /tmp/shared

# 2. 결과 파일 공유
#   /tmp/shared/abc123.anonymized-ui-trace.json -> Jaeger UI에 로드
#   /tmp/shared/abc123.mapping.json -> 내부 보관 (역추적용)
```

#### 시나리오 2: 부하 테스트

```bash
# 10개 워커, 서비스 3개, 각 1000개 트레이스
jaeger-tracegen \
    --workers 10 \
    --services 3 \
    --traces 1000 \
    --spans 5 \
    --trace-exporter otlp-http

# 또는 5분간 지속 실행
jaeger-tracegen \
    --workers 20 \
    --duration 5m \
    --spans 10
```

#### 시나리오 3: ES 인덱스 관리 (크론잡)

```bash
# 매일 실행: 초기화 -> 롤오버 -> Lookback -> 정리

# 1. 최초 한번: 인덱스/별칭 초기화
jaeger-es-rollover init http://es:9200

# 2. 매일: 롤오버 실행
jaeger-es-rollover rollover http://es:9200 \
    --conditions '{"max_age": "1d"}'

# 3. 매일: 7일 이전 인덱스를 Read 별칭에서 제거
jaeger-es-rollover lookback http://es:9200 \
    --unit days --unit-count 7

# 4. 매일: 14일 이전 인덱스 물리적 삭제
jaeger-es-index-cleaner 14 http://es:9200 --rollover
```

### 크론잡 설정 예시 (Kubernetes)

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: jaeger-es-rollover
spec:
  schedule: "0 0 * * *"  # 매일 자정
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: rollover
            image: jaegertracing/jaeger-es-rollover:latest
            args:
            - rollover
            - http://elasticsearch:9200
            - --conditions
            - '{"max_age": "1d"}'

---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: jaeger-es-index-cleaner
spec:
  schedule: "30 0 * * *"  # 매일 00:30
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: cleaner
            image: jaegertracing/jaeger-es-index-cleaner:latest
            args:
            - "14"
            - http://elasticsearch:9200
            - --rollover
```

### 핵심 소스 파일 참조

| 파일 | 역할 |
|------|------|
| `cmd/anonymizer/main.go` | Anonymizer 진입점 |
| `cmd/anonymizer/app/anonymizer/anonymizer.go` | 익명화 핵심 로직 (FNV64 해시) |
| `cmd/anonymizer/app/writer/writer.go` | 원본/익명화 파일 출력 |
| `cmd/anonymizer/app/query/query.go` | Jaeger Query gRPC 클라이언트 |
| `cmd/anonymizer/app/uiconv/extractor.go` | UI 형식 변환 |
| `cmd/tracegen/main.go` | Trace Generator 진입점, OTEL 초기화 |
| `internal/tracegen/config.go` | Config, Run() 함수 |
| `internal/tracegen/worker.go` | Worker, 스팬 생성 |
| `cmd/es-index-cleaner/main.go` | ES Index Cleaner 진입점 |
| `cmd/es-index-cleaner/app/index_filter.go` | 인덱스 필터링 |
| `cmd/es-index-cleaner/app/cutoff_time.go` | 삭제 기준 시간 계산 |
| `cmd/es-rollover/main.go` | ES Rollover 진입점 |
| `cmd/es-rollover/app/index_options.go` | 인덱스 이름/별칭 규칙 |
| `cmd/es-rollover/app/init/action.go` | Init 액션 |
| `cmd/es-rollover/app/rollover/action.go` | Rollover 액션 |
| `cmd/es-rollover/app/lookback/action.go` | Lookback 액션 |
| `cmd/remote-storage/app/server.go` | Remote Storage gRPC 서버 |
| `cmd/remote-storage/app/config.go` | Remote Storage 설정 |
| `internal/leaderelection/leader_election.go` | 분산 리더 선출 |
