# 16. 로깅과 에러 처리 (Logging & Error Handling)

## 개요

CoreDNS의 로깅과 에러 처리 시스템은 DNS 요청/응답의 가시성을 확보하고, 운영 환경에서 문제를 신속하게 진단할 수 있도록 설계되었다. 핵심 컴포넌트는 다음과 같다.

| 컴포넌트 | 소스 경로 | 역할 |
|---------|----------|------|
| log 플러그인 | `plugin/log/log.go` | 접근(access) 로깅 |
| errors 플러그인 | `plugin/errors/errors.go` | 에러 로깅 및 통합 |
| dnstap 플러그인 | `plugin/dnstap/handler.go` | 구조화된 DNS 메시지 로깅 |
| pkg/log | `plugin/pkg/log/log.go` | 기본 로그 라이브러리 |
| pkg/replacer | `plugin/pkg/replacer/replacer.go` | 포맷 문자열 치환 |
| pkg/response | `plugin/pkg/response/typify.go` | 응답 분류 |

---

## 1. 기본 로그 라이브러리 (plugin/pkg/log)

CoreDNS의 모든 로깅은 `plugin/pkg/log` 패키지를 기반으로 한다. Go 표준 라이브러리의 `log` 패키지를 감싸서 로그 레벨 접두사를 붙이는 경량 래퍼이다.

### 1.1 로그 레벨 상수

```
소스: plugin/pkg/log/log.go (99~105행)

const (
    debug   = "[DEBUG] "
    err     = "[ERROR] "
    fatal   = "[FATAL] "
    info    = "[INFO] "
    warning = "[WARNING] "
)
```

5가지 로그 레벨을 제공한다:
- **DEBUG**: 디버그 모드에서만 출력
- **INFO**: 일반 정보 메시지
- **WARNING**: 경고 메시지
- **ERROR**: 에러 메시지
- **FATAL**: 치명적 에러 (프로세스 종료)

### 1.2 디버그 모드 제어

```
소스: plugin/pkg/log/log.go (19~40행)

var D = &d{}

type d struct {
    on atomic.Bool
}

func (d *d) Set()       { d.on.Store(true) }
func (d *d) Clear()     { d.on.Store(false) }
func (d *d) Value() bool { return d.on.Load() }
```

디버그 로깅은 `atomic.Bool`로 스레드 안전하게 제어된다. `debug` 플러그인이 로드되면 `D.Set()`이 호출되어 디버그 출력이 활성화된다. Debug/Debugf 함수는 `D.Value()`가 true일 때만 실제 출력을 수행한다.

```
소스: plugin/pkg/log/log.go (53~68행)

func Debug(v ...any) {
    if !D.Value() {
        return
    }
    log(debug, v...)
}
```

### 1.3 플러그인별 로거

각 플러그인은 `P` 구조체를 통해 자체 로거를 생성한다.

```
소스: plugin/pkg/log/plugin.go (8~15행)

type P struct {
    plugin string
}

func NewWithPlugin(name string) P { return P{"plugin/" + name + ": "} }
```

이렇게 생성된 로거의 출력 예시:
```
[INFO] plugin/errors: 2 example.com. A: i/o timeout
[ERROR] plugin/forward: no upstreams available
```

플러그인명이 자동으로 포함되므로, 로그 메시지만으로 어떤 플러그인에서 발생한 이벤트인지 즉시 파악할 수 있다.

---

## 2. log 플러그인 (접근 로깅)

### 2.1 Logger 구조체

```
소스: plugin/log/log.go (20~25행)

type Logger struct {
    Next  plugin.Handler
    Rules []Rule

    repl replacer.Replacer
}
```

Logger는 플러그인 체인의 한 요소로, 다음 플러그인을 호출한 뒤 결과를 로깅한다. `Rules` 배열로 복수의 로깅 규칙을 지원하며, `repl`은 포맷 문자열 치환기이다.

### 2.2 Rule 구조체

```
소스: plugin/log/log.go (70~74행)

type Rule struct {
    NameScope string
    Class     map[response.Class]struct{}
    Format    string
}
```

| 필드 | 설명 |
|------|------|
| NameScope | 로깅 대상 도메인 범위 (예: "example.com.", "." 전체) |
| Class | 로깅할 응답 클래스 (All, Success, Denial, Error) |
| Format | 로그 출력 포맷 문자열 |

### 2.3 로그 포맷 상수

```
소스: plugin/log/log.go (76~83행)

const (
    CommonLogFormat = `{remote}:{port} - {>id} "{type} {class} {name} {proto} {size} {>do} {>bufsize}" {rcode} {>rflags} {rsize} {duration}`
    CombinedLogFormat = CommonLogFormat + ` "{>opcode}"`
    DefaultLogFormat = CommonLogFormat
)
```

#### CommonLogFormat 출력 예시

```
10.0.0.1:43210 - 12345 "A IN example.com. udp 50 false 4096" NOERROR qr,aa,rd 128 0.001234s
```

각 필드의 의미:

| 치환자 | 의미 | 예시 |
|-------|------|------|
| `{remote}` | 클라이언트 IP | 10.0.0.1 |
| `{port}` | 클라이언트 포트 | 43210 |
| `{>id}` | DNS 메시지 ID | 12345 |
| `{type}` | 쿼리 타입 | A, AAAA, MX |
| `{class}` | 쿼리 클래스 | IN |
| `{name}` | 쿼리 이름 | example.com. |
| `{proto}` | 프로토콜 | udp, tcp |
| `{size}` | 요청 크기 | 50 |
| `{>do}` | DNSSEC OK 플래그 | true, false |
| `{>bufsize}` | EDNS 버퍼 크기 | 4096 |
| `{rcode}` | 응답 코드 | NOERROR, NXDOMAIN |
| `{>rflags}` | 응답 플래그 | qr,aa,rd |
| `{rsize}` | 응답 크기 | 128 |
| `{duration}` | 처리 시간 | 0.001234s |

#### CombinedLogFormat

CommonLogFormat에 `{>opcode}` (DNS opcode)를 추가한 확장 포맷이다.

### 2.4 ServeDNS 처리 흐름

```
소스: plugin/log/log.go (28~64행)

func (l Logger) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}
    name := state.Name()
    for _, rule := range l.Rules {
        if !plugin.Name(rule.NameScope).Matches(name) {
            continue
        }

        rrw := dnstest.NewRecorder(w)
        rc, err := plugin.NextOrFailure(l.Name(), l.Next, ctx, rrw, r)

        tpe, _ := response.Typify(rrw.Msg, time.Now().UTC())
        class := response.Classify(tpe)

        _, ok := rule.Class[response.All]
        var ok1 bool
        if !ok {
            _, ok1 = rule.Class[class]
        }
        if ok || ok1 {
            logstr := l.repl.Replace(ctx, state, rrw, rule.Format)
            clog.Info(logstr)
        }

        return rc, err
    }
    return plugin.NextOrFailure(l.Name(), l.Next, ctx, w, r)
}
```

처리 흐름을 단계별로 분석하면:

```
┌──────────────────────────────────────────────────────┐
│                   Logger.ServeDNS                     │
├──────────────────────────────────────────────────────┤
│ 1. 요청에서 도메인명 추출                              │
│ 2. Rules 순회 → NameScope 매칭 확인                   │
│ 3. dnstest.NewRecorder로 응답 기록기 생성              │
│ 4. plugin.NextOrFailure로 다음 플러그인 호출           │
│ 5. response.Typify()로 응답 타입 분류                  │
│ 6. response.Classify()로 응답 클래스 분류              │
│ 7. Rule.Class 매칭 확인                               │
│ 8. replacer.Replace()로 포맷 문자열 치환               │
│ 9. clog.Info()로 로그 출력                            │
└──────────────────────────────────────────────────────┘
```

핵심 설계: **Recorder 패턴**을 사용한다. `dnstest.NewRecorder(w)`는 원래 ResponseWriter를 감싸서 응답 메시지를 가로채 기록한다. 이를 통해 다음 플러그인이 실제로 응답을 쓴 뒤에 그 내용을 로깅할 수 있다.

### 2.5 setup 파싱

```
소스: plugin/log/setup.go (30~102행)
```

Corefile에서 log 플러그인을 설정하는 방법:

```
# 기본 설정 (모든 쿼리를 기본 포맷으로 로깅)
log

# 특정 도메인만 로깅
log example.com

# 커스텀 포맷
log example.com "{remote} - {type} {name} {rcode}"

# 응답 클래스별 필터링
log {
    class denial error
}
```

`class` 블록에서 지정 가능한 클래스:
- `all`: 모든 응답 (기본값)
- `success`: 성공 응답 (NoError, Delegation)
- `denial`: 부정 응답 (NXDOMAIN, NoData)
- `error`: 에러 응답 (ServerError, OtherError)

---

## 3. 응답 분류 시스템 (response 패키지)

### 3.1 response.Typify() - 응답 타입 분류

```
소스: plugin/pkg/response/typify.go (57~125행)

func Typify(m *dns.Msg, t time.Time) (Type, *dns.OPT) {
    if m == nil {
        return OtherError, nil
    }
    // ...
}
```

Typify는 DNS 응답 메시지를 분석하여 8가지 타입 중 하나로 분류한다:

```
┌─────────────────────────────────────────────────────────────┐
│                     Typify 분류 로직                         │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  Opcode == Update         → Update                          │
│  Opcode == Notify         → Meta                            │
│  Qtype == AXFR/IXFR      → Meta                            │
│  DNSSEC 서명 만료         → OtherError                       │
│  Answer > 0 & NOERROR     → NoError (정상 응답)              │
│  SOA in Auth & NOERROR    → NoData (이름은 있지만 타입 없음)   │
│  SOA in Auth & NXDOMAIN   → NameError (이름 없음)            │
│  SERVFAIL/NOTIMPL         → ServerError                     │
│  NS in Auth & NOERROR     → Delegation (위임)                │
│  NOERROR (기타)           → NoError                          │
│  그 외                    → OtherError                       │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### 3.2 Type 상수 정의

```
소스: plugin/pkg/response/typify.go (13~30행)

const (
    NoError     Type = iota  // 정상 응답
    NameError                // NXDOMAIN
    ServerError              // SERVFAIL, NOTIMPL
    NoData                   // 이름은 있지만 타입 없음
    Delegation               // NS 위임
    Meta                     // NOTIFY, AXFR/IXFR
    Update                   // 동적 업데이트
    OtherError               // 기타 에러
)
```

### 3.3 response.Classify() - 클래스 분류

```
소스: plugin/pkg/response/classify.go (49~61행)

func Classify(t Type) Class {
    switch t {
    case NoError, Delegation:
        return Success
    case NameError, NoData:
        return Denial
    case OtherError:
        fallthrough
    default:
        return Error
    }
}
```

Type을 상위 Class로 매핑한다:

| Type | Class | 설명 |
|------|-------|------|
| NoError | Success | 정상 응답 |
| Delegation | Success | 위임 응답 |
| NameError | Denial | NXDOMAIN |
| NoData | Denial | 타입 미존재 |
| ServerError | Error | 서버 에러 |
| OtherError | Error | 기타 에러 |
| Meta | Error | 메타 메시지 |
| Update | Error | 동적 업데이트 |

### 3.4 Class 상수 정의

```
소스: plugin/pkg/response/classify.go (8~17행)

const (
    All     Class = iota  // 메타 클래스: 모든 클래스 포함
    Success               // 성공 응답
    Denial                // 존재 거부 응답
    Error                 // 에러 응답
)
```

---

## 4. replacer 패키지 (포맷 문자열 치환)

### 4.1 Replacer 구조체

```
소스: plugin/pkg/replacer/replacer.go (17~24행)

type Replacer struct{}

func New() Replacer {
    return Replacer{}
}

func (r Replacer) Replace(ctx context.Context, state request.Request, rr *dnstest.Recorder, s string) string {
    return loadFormat(s).Replace(ctx, state, rr)
}
```

Replacer는 상태 없는(stateless) 구조체로, 한 번 생성하면 동시에 안전하게 사용할 수 있다.

### 4.2 지원 레이블

```
소스: plugin/pkg/replacer/replacer.go (38~57행)

var labels = map[string]struct{}{
    "{type}":            {},
    "{name}":            {},
    "{class}":           {},
    "{proto}":           {},
    "{size}":            {},
    "{remote}":          {},
    "{port}":            {},
    "{local}":           {},
    "{>id}":             {},
    "{>opcode}":         {},
    "{>do}":             {},
    "{>bufsize}":        {},
    "{rcode}":           {},
    "{rsize}":           {},
    "{duration}":        {},
    "{>rflags}":         {},
}
```

레이블은 세 카테고리로 분류된다:

**요청 정보 레이블:**
| 레이블 | 소스 | 설명 |
|--------|------|------|
| `{type}` | `state.Type()` | 쿼리 레코드 타입 (A, AAAA 등) |
| `{name}` | `state.Name()` | 쿼리 도메인 이름 |
| `{class}` | `state.Class()` | DNS 클래스 (IN 등) |
| `{proto}` | `state.Proto()` | 전송 프로토콜 (udp, tcp) |
| `{size}` | `state.Req.Len()` | 요청 메시지 크기 |
| `{remote}` | `state.IP()` | 클라이언트 IP |
| `{port}` | `state.Port()` | 클라이언트 포트 |
| `{local}` | `state.LocalIP()` | 서버 로컬 IP |

**헤더 레이블 (`{>` 접두사):**
| 레이블 | 소스 | 설명 |
|--------|------|------|
| `{>id}` | `state.Req.Id` | DNS 메시지 ID |
| `{>opcode}` | `state.Req.Opcode` | DNS opcode |
| `{>do}` | `state.Do()` | DNSSEC OK 플래그 |
| `{>bufsize}` | `state.Size()` | EDNS 버퍼 크기 |
| `{>rflags}` | MsgHdr 플래그 | 응답 플래그 (qr,aa,rd 등) |

**응답 레이블:**
| 레이블 | 소스 | 설명 |
|--------|------|------|
| `{rcode}` | `rr.Rcode` | 응답 코드 |
| `{rsize}` | `rr.Len` | 응답 크기 |
| `{duration}` | `time.Since(rr.Start)` | 처리 소요 시간 |

### 4.3 메타데이터 레이블

`{/metadata_name}` 형식으로 메타데이터 플러그인이 설정한 값을 참조할 수 있다.

```
소스: plugin/pkg/replacer/replacer.go (270~279행)

case typeMetadata:
    if fm := metadata.ValueFunc(ctx, s.value); fm != nil {
        b = append(b, fm()...)
    } else {
        b = append(b, EmptyValue...)
    }
```

### 4.4 포맷 파싱과 캐싱

```
소스: plugin/pkg/replacer/replacer.go (247~255행)

var replacerCache sync.Map // map[string]replacer

func loadFormat(s string) replacer {
    if v, ok := replacerCache.Load(s); ok {
        return v.(replacer)
    }
    v, _ := replacerCache.LoadOrStore(s, parseFormat(s))
    return v.(replacer)
}
```

포맷 문자열은 한 번 파싱되면 `sync.Map`에 캐싱된다. 파싱 결과는 `node` 슬라이스로, 각 노드는 세 가지 타입 중 하나이다:

```
소스: plugin/pkg/replacer/replacer.go (170~177행)

const (
    typeLabel    nodeType = iota  // "{type}" - DNS 레이블
    typeLiteral                   // "foo" - 리터럴 문자열
    typeMetadata                  // "{/metadata}" - 메타데이터
)
```

### 4.5 응답 플래그 포맷팅

```
소스: plugin/pkg/replacer/replacer.go (126~156행)

func appendFlags(b []byte, h dns.MsgHdr) []byte {
    origLen := len(b)
    if h.Response {
        b = append(b, "qr,"...)
    }
    if h.Authoritative {
        b = append(b, "aa,"...)
    }
    // ... tc, rd, ra, z, ad, cd
    if n := len(b); n > origLen {
        return b[:n-1] // trim trailing ','
    }
    return b
}
```

응답 플래그를 쉼표로 구분된 약어 문자열로 변환한다: `qr,aa,rd,ra` 등.

### 4.6 성능 최적화: 버퍼 풀

```
소스: plugin/pkg/replacer/replacer.go (258~263행)

var bufPool = sync.Pool{
    New: func() any {
        b := make([]byte, 0, 256)
        return &b
    },
}
```

`sync.Pool`을 사용하여 문자열 빌드용 바이트 슬라이스를 재사용한다. 각 Replace 호출마다 풀에서 버퍼를 가져와 사용 후 반환하므로 GC 부담을 줄인다.

---

## 5. errors 플러그인 (에러 처리)

### 5.1 errorHandler 구조체

```
소스: plugin/errors/errors.go (38~42행)

type errorHandler struct {
    patterns []*pattern
    stopFlag uint32
    Next     plugin.Handler
}
```

errors 플러그인은 플러그인 체인에서 발생하는 에러를 포착하여 로깅한다. `patterns`는 에러 통합(consolidation) 패턴 목록이다.

### 5.2 pattern 구조체 (에러 통합)

```
소스: plugin/errors/errors.go (20~27행)

type pattern struct {
    ptimer      unsafe.Pointer
    count       uint32
    period      time.Duration
    pattern     *regexp.Regexp
    logCallback func(format string, v ...any)
    showFirst   bool
}
```

| 필드 | 설명 |
|------|------|
| ptimer | 타이머 포인터 (atomic 접근) |
| count | 현재 기간 내 에러 발생 횟수 |
| period | 통합 기간 |
| pattern | 에러 메시지 매칭 정규식 |
| logCallback | 로그 출력 함수 (레벨별) |
| showFirst | 첫 번째 에러를 즉시 출력할지 여부 |

### 5.3 에러 통합 메커니즘

에러 통합(consolidation)은 같은 패턴의 에러가 짧은 시간 내에 반복 발생할 때, 매번 로깅하지 않고 주기적으로 집계하여 출력하는 기능이다.

```
소스: plugin/errors/errors.go (62~81행)

func (h *errorHandler) consolidateError(i int) bool {
    if atomic.LoadUint32(&h.stopFlag) > 0 {
        return false
    }
    cnt := atomic.AddUint32(&h.patterns[i].count, 1)
    if cnt == 1 {
        ind := i
        t := time.AfterFunc(h.patterns[ind].period, func() {
            h.logPattern(ind)
        })
        h.patterns[ind].setTimer(t)
        if atomic.LoadUint32(&h.stopFlag) > 0 && t.Stop() {
            h.logPattern(ind)
        }
        return !h.patterns[i].showFirst
    }
    return true
}
```

동작 흐름:

```
┌─────────────────────────────────────────────────────────┐
│              에러 통합 (Consolidation) 흐름               │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  에러 발생 → pattern 매칭 확인                            │
│       │                                                 │
│       ├─ 매칭 실패 → 일반 로깅 (logFunc 호출)             │
│       │                                                 │
│       └─ 매칭 성공 → consolidateError(i) 호출            │
│            │                                            │
│            ├─ count == 1 (첫 발생)                       │
│            │    ├─ 타이머 시작 (period 후 집계 출력)       │
│            │    └─ showFirst? → false 반환 (즉시 출력)    │
│            │                                            │
│            └─ count > 1 (반복 발생)                       │
│                 └─ true 반환 (출력 억제, 카운트만 증가)     │
│                                                         │
│  타이머 만료 → logPattern(i) 호출                         │
│       → "N errors like 'pattern' occurred in last Xs"   │
│       → count 초기화                                     │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

### 5.4 logPattern 출력

```
소스: plugin/errors/errors.go (48~57행)

func (h *errorHandler) logPattern(i int) {
    cnt := atomic.SwapUint32(&h.patterns[i].count, 0)
    if cnt == 0 {
        return
    }
    if cnt > 1 || !h.patterns[i].showFirst {
        h.patterns[i].logCallback("%d errors like '%s' occurred in last %s",
            cnt, h.patterns[i].pattern.String(), h.patterns[i].period)
    }
}
```

출력 예시:
```
[ERROR] plugin/errors: 42 errors like 'i/o timeout' occurred in last 30s
```

### 5.5 ServeDNS 에러 처리

```
소스: plugin/errors/errors.go (94~122행)

func (h *errorHandler) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    rcode, err := plugin.NextOrFailure(h.Name(), h.Next, ctx, w, r)

    if err != nil {
        strErr := err.Error()
        state := request.Request{W: w, Req: r}

        logFunc := log.Errorf

        for i := range h.patterns {
            if h.patterns[i].pattern.MatchString(strErr) {
                if h.consolidateError(i) {
                    return rcode, err
                }
                logFunc = h.patterns[i].logCallback
                break
            }
        }

        logFunc("%d %s %s: %s", rcode, state.Name(), state.Type(), strErr)
    }

    return rcode, err
}
```

에러 로깅 출력 형식: `{rcode} {domain} {type}: {error_message}`

예시:
```
[ERROR] plugin/errors: 2 example.com. A: i/o timeout
```

### 5.6 에러 통합 설정 파싱

```
소스: plugin/errors/setup.go (76~99행)

func parseConsolidate(c *caddy.Controller) (*pattern, error) {
    args := c.RemainingArgs()
    // args: period regex [log_level] [show_first]
    p, err := time.ParseDuration(args[0])
    re, err := regexp.Compile(args[1])
    lc, showFirst, err := parseOptionalParams(c, args[2:])
    return &pattern{period: p, pattern: re, logCallback: lc, showFirst: showFirst}, nil
}
```

Corefile 설정 예시:

```
errors {
    # 30초 동안 "i/o timeout" 에러를 통합
    consolidate 30s ".*i/o timeout.*"

    # warning 레벨로 통합, 첫 에러 즉시 출력
    consolidate 1m ".*connection refused.*" warning show_first

    # stacktrace 활성화
    stacktrace
}
```

### 5.7 로그 레벨 설정

```
소스: plugin/errors/setup.go (103~136행)

func parseOptionalParams(c *caddy.Controller, args []string) (func(format string, v ...any), bool, error) {
    logLevels := map[string]func(format string, v ...any){
        "warning": log.Warningf,
        "error":   log.Errorf,
        "info":    log.Infof,
        "debug":   log.Debugf,
    }
    // ...
}
```

통합 에러의 로그 레벨을 별도로 지정할 수 있다. 기본값은 `error`이다.

### 5.8 정규식 길이 제한

```
소스: plugin/errors/setup.go (13행)

const maxRegexpLen = 10000
```

악의적 입력에 의한 OOM을 방지하기 위해 정규식 패턴 길이를 10,000자로 제한한다.

### 5.9 graceful shutdown

```
소스: plugin/errors/errors.go (83~91행)

func (h *errorHandler) stop() {
    atomic.StoreUint32(&h.stopFlag, 1)
    for i := range h.patterns {
        t := h.patterns[i].timer()
        if t != nil && t.Stop() {
            h.logPattern(i)
        }
    }
}
```

셧다운 시 모든 타이머를 정지하고, 아직 출력하지 않은 통합 에러를 즉시 출력한다. `stopFlag`를 통해 새로운 에러 통합이 시작되지 않도록 한다.

---

## 6. dnstap 플러그인 (구조화된 DNS 로깅)

### 6.1 개요

dnstap은 DNS 서버에서 발생하는 쿼리와 응답을 Protocol Buffers 형식으로 구조화하여 기록하는 프로토콜이다. 텍스트 로그와 달리 파싱 없이 프로그래밍적으로 처리할 수 있다.

### 6.2 Dnstap 구조체

```
소스: plugin/dnstap/handler.go (17~29행)

type Dnstap struct {
    Next plugin.Handler
    io   tapper
    repl replacer.Replacer

    IncludeRawMessage   bool
    Identity            []byte
    Version             []byte
    ExtraFormat         string
    MultipleTcpWriteBuf int
    MultipleQueue       int
}
```

| 필드 | 설명 |
|------|------|
| io | dnstap 출력 인터페이스 (tapper) |
| IncludeRawMessage | 원시 DNS 메시지 포함 여부 |
| Identity | 서버 식별 정보 (기본값: hostname) |
| Version | 버전 정보 (CoreDNS 버전) |
| ExtraFormat | 추가 메타데이터 포맷 문자열 |
| MultipleTcpWriteBuf | TCP 쓰기 버퍼 크기 배수 (MiB 단위) |
| MultipleQueue | 큐 크기 배수 (10,000 메시지 단위) |

### 6.3 ServeDNS 처리 흐름

```
소스: plugin/dnstap/handler.go (70~84행)

func (h *Dnstap) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    rw := &ResponseWriter{
        ResponseWriter: w,
        Dnstap:         h,
        query:          r,
        ctx:            ctx,
        queryTime:      time.Now(),
    }

    h.tapQuery(ctx, w, r, rw.queryTime)

    return plugin.NextOrFailure(h.Name(), h.Next, ctx, rw, r)
}
```

핵심 설계: 쿼리 탭 메시지를 **먼저** 전송한 후, ResponseWriter를 래핑하여 응답 탭 메시지를 나중에 전송한다. 이렇게 하면 탭 메시지가 올바른 순서(쿼리 -> 응답)로 출력된다.

### 6.4 ResponseWriter

```
소스: plugin/dnstap/writer.go (15~44행)

type ResponseWriter struct {
    queryTime time.Time
    query     *dns.Msg
    ctx       context.Context
    dns.ResponseWriter
    *Dnstap
}

func (w *ResponseWriter) WriteMsg(resp *dns.Msg) error {
    err := w.ResponseWriter.WriteMsg(resp)
    if err != nil {
        return err
    }
    // 응답 후 탭 메시지 전송
    r := new(tap.Message)
    msg.SetQueryTime(r, w.queryTime)
    msg.SetResponseTime(r, time.Now())
    msg.SetQueryAddress(r, w.RemoteAddr())
    msg.SetType(r, tap.Message_CLIENT_RESPONSE)
    w.TapMessageWithMetadata(w.ctx, r, state)
    return nil
}
```

### 6.5 I/O 전송 계층

```
소스: plugin/dnstap/io.go (34~47행)

type dio struct {
    endpoint           string
    proto              string
    enc                *encoder
    queue              chan *tap.Dnstap
    dropped            uint32
    quit               chan struct{}
    flushTimeout       time.Duration
    tcpTimeout         time.Duration
    skipVerify         bool
    tcpWriteBufSize    int
    logger             WarnLogger
    errorCheckInterval time.Duration
}
```

#### 전송 프로토콜 지원

| 프로토콜 | 엔드포인트 형식 | 설명 |
|---------|--------------|------|
| Unix Socket | `/var/run/dnstap.sock` | 로컬 소켓 |
| TCP | `tcp://host:port` | 원격 TCP |
| TLS | `tls://host:port` | 원격 TLS (암호화) |

### 6.6 비동기 큐 기반 전송

```
소스: plugin/dnstap/io.go (107~113행)

func (d *dio) Dnstap(payload *tap.Dnstap) {
    select {
    case d.queue <- payload:
    default:
        atomic.AddUint32(&d.dropped, 1)
    }
}
```

비블로킹 전송: 큐가 가득 차면 메시지를 드롭하고 드롭 카운트를 증가시킨다. DNS 처리 성능에 영향을 주지 않기 위한 설계이다.

### 6.7 serve 루프

```
소스: plugin/dnstap/io.go (128~164행)

func (d *dio) serve() {
    flushTicker := time.NewTicker(d.flushTimeout)       // 1초
    errorCheckTicker := time.NewTicker(d.errorCheckInterval) // 10초
    for {
        select {
        case <-d.quit:
            d.enc.flush()
            d.enc.close()
            return
        case payload := <-d.queue:
            d.write(payload)
        case <-flushTicker.C:
            d.enc.flush()
        case <-errorCheckTicker.C:
            // 드롭된 메시지 경고 출력
            if dropped := atomic.SwapUint32(&d.dropped, 0); dropped > 0 {
                d.logger.Warningf("Dropped dnstap messages: %d\n", dropped)
            }
            // 연결 복구 시도
            if d.enc == nil {
                d.dial()
            }
        }
    }
}
```

서브 루프의 주요 타이머:

| 타이머 | 주기 | 역할 |
|--------|------|------|
| flushTicker | 1초 | 버퍼링된 메시지 플러시 |
| errorCheckTicker | 10초 | 드롭 경고 + 재연결 시도 |

### 6.8 msg 패키지 (메시지 구성)

```
소스: plugin/dnstap/msg/msg.go (19~47행)
```

dnstap 메시지의 주소 정보를 설정하는 유틸리티 함수들:

| 함수 | 역할 |
|------|------|
| `SetQueryAddress()` | 쿼리 송신 주소 (클라이언트 IP/포트) |
| `SetResponseAddress()` | 응답 송신 주소 |
| `SetQueryTime()` | 쿼리 수신 시각 (초+나노초) |
| `SetResponseTime()` | 응답 송신 시각 |
| `SetType()` | 메시지 타입 (CLIENT_QUERY, CLIENT_RESPONSE 등) |

IPv4와 IPv6를 자동 감지하여 `SocketFamily`를 설정하고, TCP/UDP를 구분하여 `SocketProtocol`을 설정한다.

### 6.9 Corefile 설정

```
dnstap tcp://collector:6000 full {
    identity "dns-server-01"
    version "CoreDNS-1.12"
    extra "{remote}:{port} {type} {name}"
}
```

| 옵션 | 설명 |
|------|------|
| `full` | 원시 DNS 메시지 포함 |
| `identity` | 서버 식별 문자열 |
| `version` | 버전 문자열 |
| `extra` | 추가 메타데이터 (replacer 포맷) |
| `skipverify` | TLS 인증서 검증 건너뛰기 |

---

## 7. panic recovery와 디버그 모드

### 7.1 서버 레벨 panic recovery

```
소스: core/dnsserver/server.go (259~273행)

if !s.debug {
    defer func() {
        if rec := recover(); rec != nil {
            if s.stacktrace {
                log.Errorf("Recovered from panic in server: %q %v\n%s", s.Addr, rec, string(debug.Stack()))
            } else {
                log.Errorf("Recovered from panic in server: %q %v", s.Addr, rec)
            }
            vars.Panic.Inc()
            errorAndMetricsFunc(s.Addr, w, r, dns.RcodeServerFailure)
        }
    }()
}
```

핵심 설계 포인트:

1. **debug 모드 비활성화 시에만** recovery가 동작한다. debug 모드에서는 panic이 그대로 전파되어 즉시 디버깅할 수 있다.
2. **stacktrace 옵션**: `errors { stacktrace }` 설정 시 `runtime/debug.Stack()`으로 전체 스택 트레이스를 출력한다.
3. **메트릭**: `vars.Panic.Inc()`로 panic 발생 횟수를 카운트한다.
4. **응답**: SERVFAIL을 클라이언트에 반환한다.

### 7.2 디버그 모드 활성화

```
소스: core/dnsserver/server.go (80~83행)

for _, site := range group {
    if site.Debug {
        s.debug = true
        log.D.Set()
    }
```

`debug` 플러그인이 로드되면:
- `s.debug = true`: panic recovery 비활성화
- `log.D.Set()`: DEBUG 레벨 로그 출력 활성화

### 7.3 stacktrace 설정

```
소스: plugin/errors/setup.go (60~61행)

case "stacktrace":
    dnsserver.GetConfig(c).Stacktrace = true
```

errors 플러그인 블록에서 `stacktrace`를 선언하면 서버 설정에 반영된다.

---

## 8. 로깅과 메트릭의 통합

### 8.1 errorAndMetricsFunc

```
소스: core/dnsserver/server.go (419~429행)

func errorAndMetricsFunc(server string, w dns.ResponseWriter, r *dns.Msg, rc int) {
    state := request.Request{W: w, Req: r}
    answer := new(dns.Msg)
    answer.SetRcode(r, rc)
    state.SizeAndDo(answer)
    vars.Report(server, state, vars.Dropped, "", rcode.ToString(rc), "", answer.Len(), time.Now())
    w.WriteMsg(answer)
}
```

에러 응답 시 Prometheus 메트릭(`vars.Report`)과 함께 에러 응답을 전송한다. `vars.Dropped`로 표시되어 드롭된 쿼리로 카운트된다.

### 8.2 메타데이터 연동

log 플러그인은 `metadata` 패키지를 통해 응답 타입과 클래스를 컨텍스트에 저장한다:

```
소스: plugin/log/log.go (40~47행)

tpe, _ := response.Typify(rrw.Msg, time.Now().UTC())
metadata.SetValueFunc(ctx, "log/type", func() string {
    return tpe.String()
})

class := response.Classify(tpe)
metadata.SetValueFunc(ctx, "log/class", func() string {
    return class.String()
})
```

다른 플러그인에서 `{/log/type}`, `{/log/class}`로 이 값을 참조할 수 있다.

---

## 9. 로깅 아키텍처 전체 흐름

```
 클라이언트 요청
       │
       ▼
 ┌──────────────┐
 │   Server     │──── panic recovery (debug 모드 아닌 경우)
 │  ServeDNS    │
 └──────┬───────┘
        │
        ▼
 ┌──────────────┐
 │  dnstap      │──── 쿼리 탭 메시지 전송 (비동기 큐)
 │  플러그인     │
 └──────┬───────┘
        │
        ▼
 ┌──────────────┐
 │   log        │──── Recorder로 응답 기록
 │  플러그인     │
 └──────┬───────┘
        │
        ▼
 ┌──────────────┐
 │  errors      │──── 에러 포착 및 통합
 │  플러그인     │
 └──────┬───────┘
        │
        ▼
 ┌──────────────┐
 │  실제 처리    │──── forward, file, hosts 등
 │  플러그인     │
 └──────┴───────┘
        │
        ▼
 응답 전송 (역순)
        │
 ┌──────────────┐
 │  errors      │──── 에러 있으면 로깅 (패턴 매칭 → 통합 또는 즉시 출력)
 │              │
 ├──────────────┤
 │   log        │──── Typify → Classify → Class 필터 → 포맷 치환 → 출력
 │              │
 ├──────────────┤
 │  dnstap      │──── 응답 탭 메시지 전송 (ResponseWriter.WriteMsg)
 │              │
 ├──────────────┤
 │  Server      │──── 메트릭 기록 (Prometheus)
 └──────────────┘
```

---

## 10. 운영 시 로깅 설정 가이드

### 10.1 개발 환경

```
.:53 {
    debug
    log
    errors {
        stacktrace
    }
}
```

### 10.2 프로덕션 환경 (에러만 로깅)

```
.:53 {
    log {
        class error denial
    }
    errors {
        consolidate 30s ".*i/o timeout.*" warning
        consolidate 1m ".*connection refused.*" warning show_first
    }
}
```

### 10.3 프로덕션 환경 (전체 로깅 + dnstap)

```
.:53 {
    log . "{remote} {type} {name} {rcode} {duration}"
    errors {
        consolidate 30s ".*" warning
    }
    dnstap tcp://collector:6000 full {
        identity "prod-dns-01"
    }
}
```

### 10.4 성능 고려사항

| 설정 | 영향 | 권장 |
|------|------|------|
| `log` (기본) | 중간 (모든 쿼리 로깅) | 프로덕션에서는 class 필터 사용 |
| `errors` (기본) | 낮음 (에러만 로깅) | 항상 활성화 |
| `errors consolidate` | 매우 낮음 | 반복 에러가 많은 환경에서 필수 |
| `dnstap full` | 높음 (원시 메시지 직렬화) | 필요 시에만 사용 |
| `dnstap` (full 없이) | 중간 | 모니터링 용도로 적합 |
| `debug` | 높음 (모든 DEBUG 출력) | 프로덕션 사용 금지 |

---

## 요약

CoreDNS의 로깅과 에러 처리 시스템은 세 가지 핵심 원칙을 따른다:

1. **계층적 분류**: Typify() -> Classify()를 통해 응답을 의미 있는 카테고리로 분류하고, 카테고리별로 로깅 여부를 제어한다.

2. **비침입적 기록**: Recorder 패턴과 ResponseWriter 래핑을 통해 플러그인 체인의 실제 처리에 영향을 주지 않으면서 요청/응답을 기록한다.

3. **운영 친화적 통합**: 에러 통합(consolidation)으로 로그 홍수를 방지하고, dnstap으로 구조화된 데이터를 외부 분석 시스템에 전송한다.
