# 11. Hubble 필터 체인 (Filter Chain)

## 목차
1. [개요](#1-개요)
2. [필터 타입 시스템](#2-필터-타입-시스템)
3. [Apply 로직 (Whitelist OR + Blacklist NOR)](#3-apply-로직-whitelist-or--blacklist-nor)
4. [BuildFilter / BuildFilterList](#4-buildfilter--buildfilterlist)
5. [OnBuildFilter 인터페이스](#5-onbuildfilter-인터페이스)
6. [기본 필터 타입 24종 상세](#6-기본-필터-타입-24종-상세)
7. [IP 필터 (CIDR 지원)](#7-ip-필터-cidr-지원)
8. [라벨 필터 (Selector 기반)](#8-라벨-필터-selector-기반)
9. [HTTP 필터](#9-http-필터)
10. [CLI에서 FlowFilter 변환](#10-cli에서-flowfilter-변환)
11. [CEL 표현식 필터](#11-cel-표현식-필터)
12. [필터 조합 패턴](#12-필터-조합-패턴)
13. [성능 고려사항](#13-성능-고려사항)

---

## 1. 개요

Hubble 필터 체인은 관찰할 네트워크 흐름(Flow)을 선택적으로 필터링하는 시스템이다.
gRPC `GetFlowsRequest`에 포함된 `FlowFilter` protobuf 메시지를 기반으로 런타임에
필터 함수를 빌드하고, 각 이벤트에 적용하여 화이트리스트/블랙리스트 로직으로
포함 여부를 결정한다.

```
  CLI 플래그                  gRPC 요청              서버 내부
  ──────────                 ────────────            ─────────
  --from-pod X    ──>   FlowFilter{         ──>   BuildFilterList()
  --to-pod Y              SourcePod: ["X"]          │
  --verdict DROPPED        DestinationPod: ["Y"]    v
                           Verdict: [DROPPED]     FilterFuncs
                         }                          │
                                                    v
                                              Apply(whitelist, blacklist, ev)
                                                    │
                                              true/false
```

소스 경로:
- 핵심 로직: `cilium/pkg/hubble/filters/filters.go`
- IP 필터: `cilium/pkg/hubble/filters/ip.go`
- 라벨 필터: `cilium/pkg/hubble/filters/labels.go`
- HTTP 필터: `cilium/pkg/hubble/filters/http.go`

---

## 2. 필터 타입 시스템

### 2.1 FilterFunc

```go
// FilterFunc는 데이터를 필터링하는 함수이다.
// 필터가 매치되면 true, 아니면 false를 반환한다.
type FilterFunc func(ev *v1.Event) bool
```

모든 필터는 이 함수 시그니처를 따른다. 입력으로 `*v1.Event`를 받아 boolean을 반환하며,
이벤트가 필터 조건에 부합하면 true를 반환한다.

### 2.2 FilterFuncs

```go
// FilterFuncs는 여러 필터의 조합으로, 보통 함께 적용된다.
type FilterFuncs []FilterFunc
```

FilterFuncs는 세 가지 매칭 연산을 제공한다:

```go
// MatchAll: 모든 필터가 매치해야 함 (AND)
func (fs FilterFuncs) MatchAll(ev *v1.Event) bool {
    for _, f := range fs {
        if !f(ev) {
            return false
        }
    }
    return true
}

// MatchOne: 하나라도 매치하면 됨 (OR)
// 필터가 비어있으면 true 반환
func (fs FilterFuncs) MatchOne(ev *v1.Event) bool {
    if len(fs) == 0 {
        return true
    }
    for _, f := range fs {
        if f(ev) {
            return true
        }
    }
    return false
}

// MatchNone: 아무것도 매치하지 않아야 함 (NOR)
// 필터가 비어있으면 true 반환
func (fs FilterFuncs) MatchNone(ev *v1.Event) bool {
    if len(fs) == 0 {
        return true
    }
    for _, f := range fs {
        if f(ev) {
            return false
        }
    }
    return true
}
```

### 2.3 매칭 연산 비교

| 연산 | 빈 필터 | 하나 매치 | 모두 매치 | 아무것도 안 매치 |
|------|---------|----------|----------|---------------|
| MatchAll (AND) | true | false (나머지 불일치 시) | true | false |
| MatchOne (OR) | true | true | true | false |
| MatchNone (NOR) | true | false | false | true |

---

## 3. Apply 로직 (Whitelist OR + Blacklist NOR)

### 3.1 Apply 함수

```go
// Apply는 화이트리스트와 블랙리스트로 흐름을 필터링한다.
// 흐름이 결과에 포함되어야 하면 true를 반환한다.
func Apply(whitelist, blacklist FilterFuncs, ev *v1.Event) bool {
    return whitelist.MatchOne(ev) && blacklist.MatchNone(ev)
}
```

### 3.2 필터링 논리

```
  이벤트가 결과에 포함되려면:
  1. 화이트리스트 중 하나라도 매치 (OR)  → true
     AND
  2. 블랙리스트 중 아무것도 매치하지 않음 (NOR)  → true

  화이트리스트가 비어있으면: 모든 이벤트 허용 (vacuous truth)
  블랙리스트가 비어있으면: 차단 없음 (vacuous truth)
```

### 3.3 예시 시나리오

```
  화이트리스트: [podFilter("frontend"), podFilter("backend")]
  블랙리스트:   [verdictFilter(FORWARDED)]

  이벤트 A: source=frontend, verdict=DROPPED
  → whitelist.MatchOne: podFilter("frontend") → true (OR 성공)
  → blacklist.MatchNone: verdictFilter(FORWARDED) → true (NOR: FORWARDED 아님)
  → 결과: true (포함)

  이벤트 B: source=frontend, verdict=FORWARDED
  → whitelist.MatchOne: true
  → blacklist.MatchNone: verdictFilter(FORWARDED) → false (NOR: FORWARDED 매치!)
  → 결과: false (제외)

  이벤트 C: source=database, verdict=DROPPED
  → whitelist.MatchOne: podFilter("frontend") false, podFilter("backend") false
  → 결과: false (제외, 화이트리스트 불일치)
```

### 3.4 화이트리스트/블랙리스트 시각화

```
  GetFlowsRequest {
      allow: [FlowFilter{...}, FlowFilter{...}]   // 화이트리스트 (OR)
      deny:  [FlowFilter{...}]                     // 블랙리스트 (NOR)
  }

  ┌──────────────────────────────────────┐
  │          모든 이벤트                    │
  │  ┌──────────────────────────────┐    │
  │  │   화이트리스트 통과 (OR)        │    │
  │  │  ┌───────────────────────┐   │    │
  │  │  │  블랙리스트 통과 (NOR)  │   │    │
  │  │  │                       │   │    │
  │  │  │  *** 최종 결과 ***     │   │    │
  │  │  │                       │   │    │
  │  │  └───────────────────────┘   │    │
  │  └──────────────────────────────┘    │
  └──────────────────────────────────────┘
```

---

## 4. BuildFilter / BuildFilterList

### 4.1 BuildFilter

단일 FlowFilter에서 FilterFuncs를 빌드한다:

```go
func BuildFilter(ctx context.Context, ff *flowpb.FlowFilter,
                 auxFilters []OnBuildFilter) (FilterFuncs, error) {
    var fs []FilterFunc

    // 등록된 모든 보조 필터에게 FlowFilter를 전달
    for _, f := range auxFilters {
        fl, err := f.OnBuildFilter(ctx, ff)
        if err != nil {
            return nil, err
        }
        if fl != nil {
            fs = append(fs, fl...)
        }
    }

    return fs, nil
}
```

하나의 FlowFilter에서 생성된 FilterFuncs는 **AND로 결합**된다. 즉, 하나의
FlowFilter 내의 모든 조건이 동시에 만족해야 한다.

### 4.2 BuildFilterList

FlowFilter 배열에서 FilterFuncs를 빌드한다:

```go
func BuildFilterList(ctx context.Context, ff []*flowpb.FlowFilter,
                     auxFilters []OnBuildFilter) (FilterFuncs, error) {
    filterList := make([]FilterFunc, 0, len(ff))

    for _, flowFilter := range ff {
        // 각 FlowFilter의 모든 조건은 AND
        tf, err := BuildFilter(ctx, flowFilter, auxFilters)
        if err != nil {
            return nil, err
        }

        // 각 FlowFilter 간에는 OR
        filterFunc := func(ev *v1.Event) bool {
            return tf.MatchAll(ev)  // AND: 하나의 FlowFilter 내 모든 조건
        }

        filterList = append(filterList, filterFunc)
    }

    return filterList, nil
}
```

### 4.3 필터 조합 계층

```
  GetFlowsRequest
  ├── allow (whitelist): [FF1, FF2]     ← OR (MatchOne)
  │   ├── FF1: {srcPod="A", verdict=DROPPED}  ← AND (MatchAll)
  │   │   ├── srcPodFilter("A")
  │   │   └── verdictFilter(DROPPED)
  │   └── FF2: {dstPod="B"}           ← AND (MatchAll)
  │       └── dstPodFilter("B")
  └── deny (blacklist): [FF3]          ← NOR (MatchNone)
      └── FF3: {protocol="ICMP"}       ← AND (MatchAll)
          └── protocolFilter("ICMP")

  최종 로직:
  (srcPod="A" AND verdict=DROPPED) OR (dstPod="B")  ← whitelist
  AND NOT (protocol="ICMP")                           ← blacklist
```

---

## 5. OnBuildFilter 인터페이스

### 5.1 인터페이스 정의

```go
// OnBuildFilter는 플로우 필터를 빌드하는 중에 호출된다
type OnBuildFilter interface {
    OnBuildFilter(context.Context, *flowpb.FlowFilter) ([]FilterFunc, error)
}

// OnBuildFilterFunc는 단일 함수로 OnBuildFilter를 구현한다
type OnBuildFilterFunc func(context.Context, *flowpb.FlowFilter) ([]FilterFunc, error)

func (f OnBuildFilterFunc) OnBuildFilter(ctx context.Context,
    flow *flowpb.FlowFilter) ([]FilterFunc, error) {
    return f(ctx, flow)
}
```

### 5.2 플러그인 아키텍처

각 필터 타입은 `OnBuildFilter` 인터페이스를 구현하는 독립적인 구조체이다.
`BuildFilter`는 등록된 모든 필터에게 `FlowFilter`를 전달하고, 각 필터는 해당 필드가
설정되어 있으면 `FilterFunc`를 반환한다.

```
  BuildFilter(ctx, FlowFilter{SourceIp: ["10.0.0.1"], Verdict: [DROPPED]}, filters)
      │
      ├── UUIDFilter.OnBuildFilter()        → nil (UUID 미설정)
      ├── EventTypeFilter.OnBuildFilter()   → nil (미설정)
      ├── VerdictFilter.OnBuildFilter()     → [verdictFilter([DROPPED])]
      ├── IPFilter.OnBuildFilter()          → [filterByIPs(["10.0.0.1"], sourceIP)]
      ├── PodFilter.OnBuildFilter()         → nil (미설정)
      └── ...

      결과: [verdictFilter, ipFilter]  (AND 결합)
```

---

## 6. 기본 필터 타입 24종 상세

### 6.1 DefaultFilters 함수

소스 경로: `cilium/pkg/hubble/filters/filters.go`

```go
func DefaultFilters(log *slog.Logger) []OnBuildFilter {
    return []OnBuildFilter{
        &UUIDFilter{},
        &EventTypeFilter{},
        &VerdictFilter{},
        &DropReasonDescFilter{},
        &ReplyFilter{},
        &EncryptedFilter{},
        &IdentityFilter{},
        &ProtocolFilter{},
        &IPFilter{},
        &PodFilter{},
        &WorkloadFilter{},
        &ServiceFilter{},
        &FQDNFilter{},
        &LabelsFilter{},
        &PortFilter{},
        &HTTPFilter{},
        &TCPFilter{},
        &NodeNameFilter{},
        &ClusterNameFilter{},
        &IPVersionFilter{},
        &TraceIDFilter{},
        &TrafficDirectionFilter{},
        &CELExpressionFilter{log: log},
        &NetworkInterfaceFilter{},
        &IPTraceIDFilter{},
    }
}
```

### 6.2 전체 필터 목록

| # | 필터 | FlowFilter 필드 | CLI 플래그 | 설명 |
|---|------|-----------------|-----------|------|
| 1 | UUIDFilter | `uuid` | `--uuid` | Flow UUID 매칭 |
| 2 | EventTypeFilter | `event_type` | `--type` | 이벤트 타입 (l7, trace, drop) |
| 3 | VerdictFilter | `verdict` | `--verdict` | 판정 결과 (FORWARDED, DROPPED) |
| 4 | DropReasonDescFilter | `drop_reason_desc` | `--drop-reason` | 드롭 사유 |
| 5 | ReplyFilter | `reply` | `--reply` | 응답 패킷 여부 |
| 6 | EncryptedFilter | `encrypted` | - | 암호화 여부 |
| 7 | IdentityFilter | `source_identity`, `destination_identity` | `--identity` | 보안 ID |
| 8 | ProtocolFilter | `protocol` | `--protocol` | 프로토콜 (TCP, UDP) |
| 9 | IPFilter | `source_ip`, `destination_ip`, `source_ip_xlated` | `--ip-src`, `--ip-dst` | IP 주소/CIDR |
| 10 | PodFilter | `source_pod`, `destination_pod` | `--from-pod`, `--to-pod` | 파드 이름 |
| 11 | WorkloadFilter | `source_workload`, `destination_workload` | `--from-workload`, `--to-workload` | 워크로드 |
| 12 | ServiceFilter | `source_service`, `destination_service` | `--from-service`, `--to-service` | 서비스 |
| 13 | FQDNFilter | `source_fqdn`, `destination_fqdn`, `dns_query` | `--from-fqdn`, `--to-fqdn` | FQDN/DNS |
| 14 | LabelsFilter | `source_label`, `destination_label`, `node_labels` | `--label` | 라벨 셀렉터 |
| 15 | PortFilter | `source_port`, `destination_port` | `--from-port`, `--to-port` | 포트 |
| 16 | HTTPFilter | `http_status_code`, `http_method`, `http_path`, `http_url`, `http_header` | `--http-status`, `--http-method` | HTTP 메타데이터 |
| 17 | TCPFilter | `tcp_flags` | `--tcp-flags` | TCP 플래그 |
| 18 | NodeNameFilter | `node_name` | `--node-name` | 노드 이름 |
| 19 | ClusterNameFilter | `cluster_name` | - | 클러스터 이름 |
| 20 | IPVersionFilter | `ip_version` | `--ipv4`, `--ipv6` | IP 버전 |
| 21 | TraceIDFilter | `trace_id` | `--trace-id` | 분산 추적 ID |
| 22 | TrafficDirectionFilter | `traffic_direction` | `--traffic-direction` | 트래픽 방향 |
| 23 | CELExpressionFilter | `experimental_cel_expression` | `--cel-expression` | CEL 표현식 |
| 24 | NetworkInterfaceFilter | `interface` | `--interface` | 네트워크 인터페이스 |
| 25 | IPTraceIDFilter | `ip_trace_id` | - | IP 추적 ID |

---

## 7. IP 필터 (CIDR 지원)

### 7.1 IPFilter 구현

소스 경로: `cilium/pkg/hubble/filters/ip.go`

```go
type IPFilter struct{}

func (f *IPFilter) OnBuildFilter(ctx context.Context, ff *flowpb.FlowFilter) ([]FilterFunc, error) {
    var fs []FilterFunc

    if ff.GetSourceIp() != nil {
        ipf, _ := filterByIPs(ff.GetSourceIp(), sourceIP)
        fs = append(fs, ipf)
    }
    if ff.GetDestinationIp() != nil {
        ipf, _ := filterByIPs(ff.GetDestinationIp(), destinationIP)
        fs = append(fs, ipf)
    }
    if ff.GetSourceIpXlated() != nil {
        ipf, _ := filterByIPs(ff.GetSourceIpXlated(), sourceIPXlated)
        fs = append(fs, ipf)
    }

    return fs, nil
}
```

### 7.2 filterByIPs (정확 매치 + CIDR)

```go
func filterByIPs(ips []string, getIP func(*v1.Event) string) (FilterFunc, error) {
    var addresses []string       // 정확 매치용
    var prefixes []netip.Prefix  // CIDR 매치용

    for _, ip := range ips {
        if strings.Contains(ip, "/") {
            // CIDR 범위: "10.0.0.0/24"
            prefix, err := netip.ParsePrefix(ip)
            prefixes = append(prefixes, prefix)
        } else {
            // 정확 매치: "10.0.0.1"
            _, err := netip.ParseAddr(ip)
            addresses = append(addresses, ip)
        }
    }

    return func(ev *v1.Event) bool {
        eventIP := getIP(ev)
        if eventIP == "" {
            return false
        }

        // 1. 정확 매치 확인
        if slices.Contains(addresses, eventIP) {
            return true
        }

        // 2. CIDR 범위 확인
        if len(prefixes) > 0 {
            addr, err := netip.ParseAddr(eventIP)
            if err != nil {
                return false
            }
            return slices.ContainsFunc(prefixes, func(prefix netip.Prefix) bool {
                return prefix.Contains(addr)
            })
        }

        return false
    }, nil
}
```

### 7.3 IP 추출 헬퍼 함수

```go
func sourceIP(ev *v1.Event) string {
    return ev.GetFlow().GetIP().GetSource()
}

func destinationIP(ev *v1.Event) string {
    return ev.GetFlow().GetIP().GetDestination()
}

func sourceIPXlated(ev *v1.Event) string {
    return ev.GetFlow().GetIP().GetSourceXlated()  // SNAT 전 원본 IP
}
```

### 7.4 IPVersionFilter

```go
type IPVersionFilter struct{}

func (f *IPVersionFilter) OnBuildFilter(ctx context.Context,
    ff *flowpb.FlowFilter) ([]FilterFunc, error) {
    var fs []FilterFunc
    if ipv := ff.GetIpVersion(); ipv != nil {
        fs = append(fs, filterByIPVersion(ipv))
    }
    return fs, nil
}

func filterByIPVersion(ipver []flowpb.IPVersion) FilterFunc {
    return func(ev *v1.Event) bool {
        flow := ev.GetFlow()
        if flow == nil {
            return false
        }
        return slices.Contains(ipver, flow.GetIP().GetIpVersion())
    }
}
```

---

## 8. 라벨 필터 (Selector 기반)

### 8.1 LabelsFilter 구현

소스 경로: `cilium/pkg/hubble/filters/labels.go`

```go
type LabelsFilter struct{}

func (l *LabelsFilter) OnBuildFilter(ctx context.Context,
    ff *flowpb.FlowFilter) ([]FilterFunc, error) {
    var fs []FilterFunc

    if ff.GetSourceLabel() != nil {
        slf, _ := FilterByLabelSelectors(ff.GetSourceLabel(), sourceLabels)
        fs = append(fs, slf)
    }
    if ff.GetDestinationLabel() != nil {
        dlf, _ := FilterByLabelSelectors(ff.GetDestinationLabel(), destinationLabels)
        fs = append(fs, dlf)
    }
    if ff.GetNodeLabels() != nil {
        nlf, _ := FilterByLabelSelectors(ff.GetNodeLabels(), nodeLabels)
        fs = append(fs, nlf)
    }

    return fs, nil
}
```

### 8.2 FilterByLabelSelectors

```go
func FilterByLabelSelectors(labelSelectors []string,
    getLabels func(*v1.Event) k8sLabels.Labels) (FilterFunc, error) {

    selectors := make([]k8sLabels.Selector, 0, len(labelSelectors))
    for _, selector := range labelSelectors {
        s, _ := parseSelector(selector)
        selectors = append(selectors, s)
    }

    return func(ev *v1.Event) bool {
        labels := getLabels(ev)
        // 셀렉터 중 하나라도 매치하면 true (OR)
        return slices.ContainsFunc(selectors, func(selector k8sLabels.Selector) bool {
            return selector.Matches(labels)
        })
    }, nil
}
```

### 8.3 라벨 추출 헬퍼

```go
func sourceLabels(ev *v1.Event) k8sLabels.Labels {
    labels := ev.GetFlow().GetSource().GetLabels()
    return ciliumLabels.ParseK8sLabelArrayFromArray(labels)
}

func destinationLabels(ev *v1.Event) k8sLabels.Labels {
    labels := ev.GetFlow().GetDestination().GetLabels()
    return ciliumLabels.ParseK8sLabelArrayFromArray(labels)
}

func nodeLabels(ev *v1.Event) k8sLabels.Labels {
    labels := ev.GetFlow().GetNodeLabels()
    return ciliumLabels.ParseK8sLabelArrayFromArray(labels)
}
```

### 8.4 Cilium 소스 접두사 변환

```go
func parseSelector(selector string) (k8sLabels.Selector, error) {
    // Cilium 라벨 소스 접두사를 K8s 셀렉터 호환 형식으로 변환
    // "k8s:app=nginx" → "k8s.app=nginx"
    // "any:app in (a, b)" → "any.app in (a, b)"
    translated, _ := translateSelector(selector)
    return k8sLabels.Parse(translated)
}
```

Cilium은 라벨에 소스 접두사(k8s:, any: 등)를 사용하는데, K8s 라벨 셀렉터는
콜론을 지원하지 않으므로 점(.)으로 변환한다.

### 8.5 라벨 필터 사용 예시

```bash
# 정확한 라벨 매칭
hubble observe --from-label "k8s:app=nginx"

# 셋 멤버십
hubble observe --from-label "app in (frontend, backend)"

# 존재 여부
hubble observe --from-label "app"

# 부정
hubble observe --from-label "app!=legacy"

# 복합 조건 (AND)
hubble observe --from-label "app=nginx,tier=web"
```

---

## 9. HTTP 필터

### 9.1 HTTPFilter 구현

소스 경로: `cilium/pkg/hubble/filters/http.go`

```go
type HTTPFilter struct{}

func (h *HTTPFilter) OnBuildFilter(ctx context.Context,
    ff *flowpb.FlowFilter) ([]FilterFunc, error) {
    var fs []FilterFunc

    if ff.GetHttpStatusCode() != nil {
        if !httpMatchCompatibleEventFilter(ff.GetEventType()) {
            return nil, errors.New("filtering by http status code requires " +
                "the event type filter to only match 'l7' events")
        }
        hsf, _ := filterByHTTPStatusCode(ff.GetHttpStatusCode())
        fs = append(fs, hsf)
    }

    if ff.GetHttpMethod() != nil {
        fs = append(fs, filterByHTTPMethods(ff.GetHttpMethod()))
    }

    if ff.GetHttpPath() != nil {
        pathf, _ := filterByHTTPPaths(ff.GetHttpPath())
        fs = append(fs, pathf)
    }

    if ff.GetHttpUrl() != nil {
        pathf, _ := filterByHTTPUrls(ff.GetHttpUrl())
        fs = append(fs, pathf)
    }

    if ff.GetHttpHeader() != nil {
        fs = append(fs, filterByHTTPHeaders(ff.GetHttpHeader()))
    }

    return fs, nil
}
```

### 9.2 HTTP 상태 코드 필터

```go
var (
    httpStatusCodeFull   = regexp.MustCompile(`^[1-5][0-9]{2}$`)    // 정확: "200"
    httpStatusCodePrefix = regexp.MustCompile(`^[1-5][0-9]?\+$`)    // 접두사: "5+"
)

func filterByHTTPStatusCode(statusCodePrefixes []string) (FilterFunc, error) {
    var full, prefix []string
    for _, s := range statusCodePrefixes {
        switch {
        case httpStatusCodeFull.MatchString(s):
            full = append(full, s)       // "200", "404"
        case httpStatusCodePrefix.MatchString(s):
            prefix = append(prefix, strings.TrimSuffix(s, "+"))  // "5+" → "5"
        default:
            return nil, fmt.Errorf("invalid status code prefix: %q", s)
        }
    }

    return func(ev *v1.Event) bool {
        http := ev.GetFlow().GetL7().GetHttp()
        if http == nil || http.Code == 0 {
            return false
        }

        httpStatusCode := fmt.Sprintf("%03d", http.Code)
        // 정확 매치
        if slices.Contains(full, httpStatusCode) {
            return true
        }
        // 접두사 매치
        return slices.ContainsFunc(prefix, func(p string) bool {
            return strings.HasPrefix(httpStatusCode, p)
        })
    }, nil
}
```

### 9.3 HTTP 메서드 필터

```go
func filterByHTTPMethods(methods []string) FilterFunc {
    return func(ev *v1.Event) bool {
        http := ev.GetFlow().GetL7().GetHttp()
        if http == nil || http.Method == "" {
            return false
        }
        return slices.ContainsFunc(methods, func(method string) bool {
            return strings.EqualFold(http.Method, method)  // 대소문자 무관
        })
    }
}
```

### 9.4 HTTP URL/Path 필터 (정규식)

```go
func filterByHTTPUrls(urlRegexpStrs []string) (FilterFunc, error) {
    urlRegexps := make([]*regexp.Regexp, 0, len(urlRegexpStrs))
    for _, urlRegexpStr := range urlRegexpStrs {
        urlRegexp, _ := regexp.Compile(urlRegexpStr)
        urlRegexps = append(urlRegexps, urlRegexp)
    }

    return func(ev *v1.Event) bool {
        http := ev.GetFlow().GetL7().GetHttp()
        if http == nil || http.Url == "" {
            return false
        }
        return slices.ContainsFunc(urlRegexps, func(urlRegexp *regexp.Regexp) bool {
            return urlRegexp.MatchString(http.Url)
        })
    }, nil
}

func filterByHTTPPaths(pathRegexpStrs []string) (FilterFunc, error) {
    // URL에서 Path만 추출하여 매칭
    return func(ev *v1.Event) bool {
        uri, err := url.ParseRequestURI(http.Url)
        if err != nil {
            return false  // 잘못된 URI는 무시
        }
        return slices.ContainsFunc(pathRegexps, func(pathRegexp *regexp.Regexp) bool {
            return pathRegexp.MatchString(uri.Path)  // Path만 비교
        })
    }, nil
}
```

**URL vs Path 필터 차이:**
- URL 필터: 전체 URL 문자열에 정규식 매칭
- Path 필터: `url.ParseRequestURI`로 Path만 추출한 후 매칭

### 9.5 HTTP 헤더 필터

```go
func filterByHTTPHeaders(headers []*flowpb.HTTPHeader) FilterFunc {
    return func(ev *v1.Event) bool {
        http := ev.GetFlow().GetL7().GetHttp()
        if http == nil || http.GetHeaders() == nil {
            return false
        }

        for _, httpHeader := range http.GetHeaders() {
            // 요청된 헤더 중 하나라도 매치하면 true
            if slices.ContainsFunc(headers, func(header *flowpb.HTTPHeader) bool {
                return header.Key == httpHeader.Key && header.Value == httpHeader.Value
            }) {
                return true
            }
        }
        return false
    }
}
```

### 9.6 이벤트 타입 호환성 검사

HTTP 필터는 L7 이벤트에만 적용 가능하다:

```go
func httpMatchCompatibleEventFilter(types []*flowpb.EventTypeFilter) bool {
    if len(types) == 0 {
        return true  // 이벤트 타입 필터 없으면 허용
    }
    return slices.ContainsFunc(types, func(t *flowpb.EventTypeFilter) bool {
        return t.GetType() == api.MessageTypeAccessLog  // L7 이벤트만
    })
}
```

---

## 10. CLI에서 FlowFilter 변환

### 10.1 변환 흐름

```
  CLI 플래그                      FlowFilter protobuf
  ──────────                     ─────────────────────

  --from-pod default/nginx  →   SourcePod: ["default/nginx"]
  --to-pod default/backend  →   DestinationPod: ["default/backend"]
  --verdict DROPPED         →   Verdict: [DROPPED]
  --ip-src 10.0.0.0/24     →   SourceIp: ["10.0.0.0/24"]
  --http-status 5+          →   HttpStatusCode: ["5+"]
  --http-method GET         →   HttpMethod: ["GET"]
  --label "app=nginx"       →   SourceLabel: ["app=nginx"]
  --protocol TCP            →   Protocol: ["TCP"]
  --from-port 80            →   SourcePort: ["80"]
  --type l7                 →   EventType: [{Type: 129}]
  --node-name "node-1"      →   NodeName: ["node-1"]
  --traffic-direction ingress→  TrafficDirection: [INGRESS]
```

### 10.2 다중 값 처리

같은 플래그를 여러 번 지정하면 OR로 처리된다:

```bash
# 소스가 10.0.0.1 OR 10.0.0.2
hubble observe --ip-src 10.0.0.1 --ip-src 10.0.0.2

# HTTP 상태코드가 404 OR 5xx
hubble observe --http-status 404 --http-status 5+
```

---

## 11. CEL 표현식 필터

### 11.1 CEL (Common Expression Language) 필터

CEL 표현식 필터는 Hubble의 가장 유연한 필터링 메커니즘이다. Google의 CEL 언어를
사용하여 임의의 Flow 필드에 대한 복잡한 필터를 작성할 수 있다.

```go
type CELExpressionFilter struct {
    log *slog.Logger
}

func (f *CELExpressionFilter) OnBuildFilter(ctx context.Context,
    ff *flowpb.FlowFilter) ([]FilterFunc, error) {
    if exprs := ff.GetExperimentalCelExpression(); len(exprs) > 0 {
        return []FilterFunc{celFilter}, nil
    }
    return nil, nil
}
```

### 11.2 CEL 사용 예시

```bash
# 소스 포트가 ephemeral 범위
hubble observe --cel-expression 'flow.l4.TCP.source_port > 32767'

# 특정 HTTP 경로와 상태코드 조합
hubble observe --cel-expression \
  'flow.l7.http.url.contains("/api/") && flow.l7.http.code >= 500'

# 복합 라벨 조건
hubble observe --cel-expression \
  'flow.source.labels.exists(l, l == "k8s:app=frontend") && flow.verdict == 2'
```

---

## 12. 필터 조합 패턴

### 12.1 일반적인 필터 조합

```bash
# 패턴 1: 특정 파드 간 드롭된 트래픽 관찰
hubble observe \
  --from-pod default/frontend \
  --to-pod default/backend \
  --verdict DROPPED

# 패턴 2: 특정 네임스페이스의 HTTP 오류
hubble observe \
  --namespace production \
  --type l7 \
  --http-status 5+

# 패턴 3: DNS 쿼리 모니터링
hubble observe \
  --protocol UDP \
  --to-port 53

# 패턴 4: 외부 트래픽 모니터링
hubble observe \
  --to-identity reserved:world \
  --verdict DROPPED

# 패턴 5: 특정 CIDR에서 오는 트래픽
hubble observe \
  --ip-src 10.0.0.0/8 \
  --ip-dst 172.16.0.0/12 \
  --protocol TCP

# 패턴 6: Blacklist 활용 (ICMP 제외)
# (gRPC API에서 deny 필드를 통해 사용)
```

### 12.2 필터 계층 구조 요약

```
  GetFlowsRequest
  │
  ├── allow (whitelist)
  │   ├── FlowFilter[0] ── BuildFilter ── [F1, F2, F3] ── MatchAll (AND)
  │   ├── FlowFilter[1] ── BuildFilter ── [F4, F5]     ── MatchAll (AND)
  │   └── FlowFilter[2] ── BuildFilter ── [F6]         ── MatchAll (AND)
  │                                                         │
  │   BuildFilterList: [FF0, FF1, FF2] ───── MatchOne (OR) ─┘
  │
  └── deny (blacklist)
      ├── FlowFilter[0] ── BuildFilter ── [F7] ── MatchAll (AND)
      │                                              │
      BuildFilterList: [FF0] ─────────── MatchNone (NOR) ─┘

  Apply(whitelist, blacklist, event):
  whitelist.MatchOne(event) && blacklist.MatchNone(event)
```

---

## 13. 성능 고려사항

### 13.1 필터 평가 순서

`DefaultFilters`의 순서는 성능에 영향을 미친다. 현재 구현에서는:

1. UUID, EventType, Verdict 등 간단한 필터가 먼저 평가
2. IP, Pod, Label 등 데이터 접근이 필요한 필터가 중간
3. HTTP, CEL 등 복잡한 필터가 마지막

MatchAll(AND)에서는 첫 번째 false에서 단락 평가(short-circuit)되므로, 가장 선별력이
높은 필터를 먼저 배치하면 성능이 향상된다.

### 13.2 정규식 캐싱

HTTP URL/Path 필터에서 정규식은 `BuildFilter` 시점에 한 번만 컴파일되고,
이후 매 이벤트마다 재사용된다:

```go
func filterByHTTPPaths(pathRegexpStrs []string) (FilterFunc, error) {
    // 빌드 시점: 정규식 컴파일 (한 번)
    pathRegexps := make([]*regexp.Regexp, 0, len(pathRegexpStrs))
    for _, str := range pathRegexpStrs {
        pathRegexp, _ := regexp.Compile(str)
        pathRegexps = append(pathRegexps, pathRegexp)
    }

    // 런타임: 컴파일된 정규식 재사용 (매 이벤트)
    return func(ev *v1.Event) bool {
        // ...
        return pathRegexp.MatchString(uri.Path)
    }, nil
}
```

### 13.3 서버 vs 클라이언트 필터링

필터링은 서버측(Hubble Server/Relay)에서 수행되는 것이 이상적이다:

```
  서버측 필터링 (권장):                 클라이언트측 필터링 (비효율):
  ┌──────────┐                         ┌──────────┐
  │  Server  │                         │  Server  │
  │ 100K f/s │                         │ 100K f/s │
  │ ──filter─│── 1K f/s ──→ Client     │          │── 100K f/s ──→ Client
  └──────────┘                         └──────────┘       (filter)
                                                          ── 1K f/s
  네트워크 대역폭: 낮음                    네트워크 대역폭: 높음
  클라이언트 부하: 낮음                    클라이언트 부하: 높음
```

gRPC `GetFlowsRequest`의 `allow`/`deny` 필드를 사용하면 서버측에서 필터링되므로,
네트워크 대역폭과 클라이언트 처리 비용을 절약할 수 있다.

---

## 요약

| 항목 | 내용 |
|------|------|
| 핵심 타입 | `FilterFunc func(ev *v1.Event) bool` |
| 화이트리스트 | `MatchOne` (OR) |
| 블랙리스트 | `MatchNone` (NOR) |
| 단일 FlowFilter 내 | `MatchAll` (AND) |
| 기본 필터 수 | 24종 (`DefaultFilters`) |
| 플러그인 인터페이스 | `OnBuildFilter` |
| CIDR 지원 | `netip.Prefix` 기반 |
| 정규식 지원 | HTTP URL/Path |
| CEL 지원 | `experimental_cel_expression` |
| 핵심 파일 | `filters/filters.go` |
