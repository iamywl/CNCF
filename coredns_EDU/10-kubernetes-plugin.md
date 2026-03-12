# 10. Kubernetes 플러그인 Deep-Dive

## 개요

CoreDNS의 Kubernetes 플러그인은 Kubernetes 클러스터 내부에서 서비스 디스커버리를 위한 DNS를 제공하는 핵심 플러그인이다. Kubernetes DNS 스펙(https://github.com/kubernetes/dns/blob/master/docs/specification.md)을 구현하여 `<service>.<namespace>.svc.cluster.local` 형태의 DNS 쿼리를 처리한다.

소스코드 경로: `plugin/kubernetes/`

---

## 1. 핵심 데이터 구조

### 1.1 Kubernetes 구조체

`plugin/kubernetes/kubernetes.go`에 정의된 `Kubernetes` 구조체는 플러그인의 중심이다.

```
// plugin/kubernetes/kubernetes.go:34-57
type Kubernetes struct {
    Next             plugin.Handler
    Zones            []string
    Upstream         Upstreamer
    APIServerList    []string
    APICertAuth      string
    APIClientCert    string
    APIClientKey     string
    ClientConfig     clientcmd.ClientConfig
    APIConn          dnsController
    Namespaces       map[string]struct{}
    podMode          string
    endpointNameMode bool
    Fall             fall.F
    ttl              uint32
    opts             dnsControlOpts
    primaryZoneIndex int
    localIPs         []net.IP
    autoPathSearch   []string
    startupTimeout   time.Duration
    apiQPS           float32
    apiBurst         int
    apiMaxInflight   int
}
```

주요 필드 분석:

| 필드 | 타입 | 역할 |
|------|------|------|
| `Zones` | `[]string` | 이 플러그인이 담당하는 DNS 존 목록 (예: `cluster.local.`) |
| `APIConn` | `dnsController` | Kubernetes API 서버와의 연결 및 캐시를 관리하는 컨트롤러 인터페이스 |
| `Namespaces` | `map[string]struct{}` | 노출할 네임스페이스를 제한하는 화이트리스트 |
| `podMode` | `string` | Pod DNS 레코드 처리 모드 (disabled/verified/insecure) |
| `Fall` | `fall.F` | Fall-through 메커니즘 설정 |
| `ttl` | `uint32` | 응답에 적용할 기본 TTL (기본값 5초) |
| `Upstream` | `Upstreamer` | 외부 CNAME 해석을 위한 업스트림 리졸버 |
| `apiMaxInflight` | `int` | API 서버에 대한 동시 요청 수 제한 |

### 1.2 Upstreamer 인터페이스

```
// plugin/kubernetes/kubernetes.go:60-62
type Upstreamer interface {
    Lookup(ctx context.Context, state request.Request, name string, typ uint16) (*dns.Msg, error)
}
```

ExternalName 서비스의 CNAME 타겟을 해석하기 위해 사용된다. `setup.go`에서 `upstream.New()`로 초기화된다.

### 1.3 recordRequest 구조체

`plugin/kubernetes/parse.go`에 정의된 DNS 쿼리 파싱 결과를 담는 구조체이다.

```
// plugin/kubernetes/parse.go:11-26
type recordRequest struct {
    port      string    // SRV 레코드의 포트 부분 (_https)
    protocol  string    // SRV 레코드의 프로토콜 부분 (_tcp, _udp)
    endpoint  string    // 엔드포인트 이름
    cluster   string    // 멀티클러스터의 클러스터 ID
    service   string    // 서비스 이름
    namespace string    // 네임스페이스
    podOrSvc  string    // "pod" 또는 "svc"
}
```

---

## 2. dnsController 인터페이스와 dnsControl 구현

### 2.1 dnsController 인터페이스

`plugin/kubernetes/controller.go`에 정의된 이 인터페이스는 Kubernetes API와의 상호작용을 추상화한다.

```
// plugin/kubernetes/controller.go:45-67
type dnsController interface {
    ServiceList() []*object.Service
    EndpointsList() []*object.Endpoints
    ServiceImportList() []*object.ServiceImport
    SvcIndex(string) []*object.Service
    SvcIndexReverse(string) []*object.Service
    SvcExtIndexReverse(string) []*object.Service
    SvcImportIndex(string) []*object.ServiceImport
    PodIndex(string) []*object.Pod
    EpIndex(string) []*object.Endpoints
    EpIndexReverse(string) []*object.Endpoints
    McEpIndex(string) []*object.MultiClusterEndpoints
    GetNodeByName(context.Context, string) (*api.Node, error)
    GetNamespaceByName(string) (*object.Namespace, error)
    Run()
    HasSynced() bool
    Stop() error
    Modified(ModifiedMode) int64
}
```

이 인터페이스가 제공하는 메서드들은 크게 세 카테고리로 나뉜다:

1. **인덱스 조회**: `SvcIndex`, `PodIndex`, `EpIndex` 등 -- 키로 빠른 조회
2. **역방향 조회**: `SvcIndexReverse`, `EpIndexReverse` -- IP로 서비스/엔드포인트 역조회
3. **생명주기**: `Run`, `HasSynced`, `Stop`, `Modified`

### 2.2 dnsControl 구조체

```
// plugin/kubernetes/controller.go:69-111
type dnsControl struct {
    modified             int64    // 내부 서비스 변경 타임스탬프
    multiClusterModified int64    // 멀티클러스터 서비스 변경 타임스탬프
    extModified          int64    // 외부 IP 서비스 변경 타임스탬프

    client    kubernetes.Interface
    mcsClient mcsClientset.MulticlusterV1alpha1Interface

    selector          labels.Selector    // 서비스 레이블 셀렉터
    namespaceSelector labels.Selector    // 네임스페이스 레이블 셀렉터

    svcController       cache.Controller
    podController       cache.Controller
    epController        cache.Controller
    nsController        cache.Controller
    svcImportController cache.Controller
    mcEpController      cache.Controller

    svcLister       cache.Indexer
    podLister       cache.Indexer
    epLister        cache.Indexer
    nsLister        cache.Store
    svcImportLister cache.Indexer
    mcEpLister      cache.Indexer

    stopLock sync.Mutex
    shutdown bool
    stopCh   chan struct{}

    zones             []string
    endpointNameMode  bool
    multiclusterZones []string
}
```

핵심 설계 포인트:

- `modified` 필드가 구조체 맨 앞에 위치하는 이유는 **8바이트 정렬을 보장**하기 위해서이다. `sync/atomic` 패키지의 `LoadInt64`/`StoreInt64`는 64비트 정렬이 필요하다.
- Controller와 Lister가 쌍으로 존재한다. Controller는 Watch 루프를 실행하고, Lister(Indexer)는 로컬 캐시된 데이터를 조회한다.

### 2.3 dnsControlOpts 구조체

```
// plugin/kubernetes/controller.go:113-127
type dnsControlOpts struct {
    initPodCache       bool
    initEndpointsCache bool
    ignoreEmptyService bool
    labelSelector          *meta.LabelSelector
    selector               labels.Selector
    namespaceLabelSelector *meta.LabelSelector
    namespaceSelector      labels.Selector
    zones             []string
    endpointNameMode  bool
    multiclusterZones []string
}
```

이 옵션 구조체는 `setup.go`에서 Corefile 파싱 결과를 담아 `newdnsController`에 전달된다.

---

## 3. 인덱스 시스템

### 3.1 인덱스 상수

```
// plugin/kubernetes/controller.go:27-35
const (
    podIPIndex                  = "PodIP"
    svcNameNamespaceIndex       = "ServiceNameNamespace"
    svcIPIndex                  = "ServiceIP"
    svcExtIPIndex               = "ServiceExternalIP"
    epNameNamespaceIndex        = "EndpointNameNamespace"
    epIPIndex                   = "EndpointsIP"
    svcImportNameNamespaceIndex = "ServiceImportNameNamespace"
    mcEpNameNamespaceIndex      = "MultiClusterEndpointsImportNameNamespace"
)
```

### 3.2 인덱스 함수

각 인덱스에 대한 인덱싱 함수가 정의되어 있다:

```
// plugin/kubernetes/controller.go:260-266
func podIPIndexFunc(obj any) ([]string, error) {
    p, ok := obj.(*object.Pod)
    if !ok {
        return nil, errObj
    }
    return []string{p.PodIP}, nil
}
```

```
// plugin/kubernetes/controller.go:268-276
func svcIPIndexFunc(obj any) ([]string, error) {
    svc, ok := obj.(*object.Service)
    if !ok {
        return nil, errObj
    }
    idx := make([]string, len(svc.ClusterIPs))
    copy(idx, svc.ClusterIPs)
    return idx, nil
}
```

### 3.3 인덱스 사용 흐름

```
DNS 쿼리: my-svc.my-ns.svc.cluster.local
                    │
                    ▼
        parseRequest()로 파싱
        service="my-svc", namespace="my-ns"
                    │
                    ▼
        object.ServiceKey("my-svc", "my-ns")
        → "my-svc.my-ns" (인덱스 키 생성)
                    │
                    ▼
        k.APIConn.SvcIndex("my-svc.my-ns")
        → svcLister.ByIndex(svcNameNamespaceIndex, "my-svc.my-ns")
                    │
                    ▼
        일치하는 *object.Service 반환
```

역방향 조회 (PTR 쿼리용):

```
PTR 쿼리: 10.96.0.1
          │
          ▼
    k.APIConn.SvcIndexReverse("10.96.0.1")
    → svcLister.ByIndex(svcIPIndex, "10.96.0.1")
          │
          ▼
    ClusterIP가 10.96.0.1인 서비스 반환
```

---

## 4. Informer 설정

### 4.1 newdnsController 함수

`plugin/kubernetes/controller.go:130-242`에서 각 리소스 타입별 Informer를 설정한다.

#### Service Informer

```
// plugin/kubernetes/controller.go:142-154
dns.svcLister, dns.svcController = object.NewIndexerInformer(
    cache.ToListWatcherWithWatchListSemantics(
        &cache.ListWatch{
            ListFunc:  serviceListFunc(ctx, dns.client, api.NamespaceAll, dns.selector),
            WatchFunc: serviceWatchFunc(ctx, dns.client, api.NamespaceAll, dns.selector),
        },
        kubeClient,
    ),
    &api.Service{},
    cache.ResourceEventHandlerFuncs{AddFunc: dns.Add, UpdateFunc: dns.Update, DeleteFunc: dns.Delete},
    cache.Indexers{svcNameNamespaceIndex: svcNameNamespaceIndexFunc, svcIPIndex: svcIPIndexFunc, svcExtIPIndex: svcExtIPIndexFunc},
    object.DefaultProcessor(object.ToService, nil),
)
```

Service Informer의 특징:
- **3개의 인덱스**를 동시에 유지한다: 이름+네임스페이스, ClusterIP, ExternalIP
- `object.ToService` 변환기를 사용해 k8s API 객체를 경량화된 내부 객체로 변환한다

#### Pod Informer

```
// plugin/kubernetes/controller.go:156-172
podLister, podController := object.NewIndexerInformer(...)
dns.podLister = podLister
if opts.initPodCache {
    dns.podController = podController
}
```

**핵심 설계**: Pod Controller는 `podMode`가 `verified`일 때만 활성화된다. `disabled` 모드에서는 불필요한 API 감시를 피하기 위해 Controller를 nil로 둔다.

Pod 목록 조회 시 필터:
```
opts.FieldSelector = "status.phase!=Succeeded,status.phase!=Failed,status.phase!=Unknown"
```
완료(Succeeded), 실패(Failed), 알 수 없음(Unknown) 상태의 Pod는 제외된다.

#### EndpointSlice Informer

```
// plugin/kubernetes/controller.go:174-190
epLister, epController := object.NewIndexerInformer(
    ...
    &discovery.EndpointSlice{},
    ...
    cache.Indexers{epNameNamespaceIndex: epNameNamespaceIndexFunc, epIPIndex: epIPIndexFunc},
    object.DefaultProcessor(object.EndpointSliceToEndpoints, dns.EndpointSliceLatencyRecorder()),
)
```

EndpointSlice API(discovery.k8s.io/v1)를 사용하며, `EndpointSliceToEndpoints` 변환기로 내부 Endpoints 객체로 변환한다.

### 4.2 Controller 실행

```
// plugin/kubernetes/controller.go:443-461
func (dns *dnsControl) Run() {
    go dns.svcController.Run(dns.stopCh)
    if dns.epController != nil {
        go func() {
            dns.epController.Run(dns.stopCh)
        }()
    }
    if dns.podController != nil {
        go dns.podController.Run(dns.stopCh)
    }
    go dns.nsController.Run(dns.stopCh)
    if dns.svcImportController != nil {
        go dns.svcImportController.Run(dns.stopCh)
    }
    if dns.mcEpController != nil {
        go dns.mcEpController.Run(dns.stopCh)
    }
    <-dns.stopCh
}
```

각 Controller가 별도 고루틴에서 실행되며, `stopCh`가 닫힐 때까지 Watch 루프를 유지한다.

### 4.3 HasSynced

```
// plugin/kubernetes/controller.go:464-484
func (dns *dnsControl) HasSynced() bool {
    a := dns.svcController.HasSynced()
    b := true
    if dns.epController != nil {
        b = dns.epController.HasSynced()
    }
    c := true
    if dns.podController != nil {
        c = dns.podController.HasSynced()
    }
    d := dns.nsController.HasSynced()
    // ... svcImportController, mcEpController
    return a && b && c && d && e && f
}
```

모든 활성 Controller가 최초 동기화를 완료해야 `true`를 반환한다. 서버 시작 시 이 값이 `true`가 될 때까지 대기한다.

---

## 5. ServeDNS 흐름

### 5.1 handler.go의 ServeDNS

`plugin/kubernetes/handler.go`가 DNS 요청의 진입점이다.

```
// plugin/kubernetes/handler.go:13-91
func (k Kubernetes) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}
    qname := state.QName()
    zone := plugin.Zones(k.Zones).Matches(qname)
    if zone == "" {
        return plugin.NextOrFailure(k.Name(), k.Next, ctx, w, r)
    }
    zone = qname[len(qname)-len(zone):]  // 원본 쿼리의 대소문자 유지
    state.Zone = zone
    // ... 쿼리 타입별 처리 ...
}
```

### 5.2 Zone 매칭

```
zone := plugin.Zones(k.Zones).Matches(qname)
if zone == "" {
    return plugin.NextOrFailure(k.Name(), k.Next, ctx, w, r)
}
```

쿼리 이름이 설정된 Zone에 속하지 않으면 다음 플러그인으로 전달한다.

### 5.3 쿼리 타입별 처리

```
switch state.QType() {
case dns.TypeA:
    records, truncated, err = plugin.A(ctx, &k, zone, state, nil, plugin.Options{})
case dns.TypeAAAA:
    records, truncated, err = plugin.AAAA(ctx, &k, zone, state, nil, plugin.Options{})
case dns.TypeTXT:
    records, truncated, err = plugin.TXT(ctx, &k, zone, state, nil, plugin.Options{})
case dns.TypeCNAME:
    records, err = plugin.CNAME(ctx, &k, zone, state, plugin.Options{})
case dns.TypePTR:
    records, err = plugin.PTR(ctx, &k, zone, state, plugin.Options{})
case dns.TypeMX:
    records, extra, err = plugin.MX(ctx, &k, zone, state, plugin.Options{})
case dns.TypeSRV:
    records, extra, err = plugin.SRV(ctx, &k, zone, state, plugin.Options{})
case dns.TypeSOA:
    if qname == zone {
        records, err = plugin.SOA(ctx, &k, zone, state, plugin.Options{})
    }
case dns.TypeAXFR, dns.TypeIXFR:
    return dns.RcodeRefused, nil  // Zone Transfer 거부
case dns.TypeNS:
    // zone apex에서만 NS 레코드 응답
    if state.Name() == zone {
        records, extra, err = plugin.NS(ctx, &k, zone, state, plugin.Options{})
        break
    }
    fallthrough
default:
    // 지원하지 않는 타입: 가짜 A 조회로 NODATA vs NXDOMAIN 구분
    fake := state.NewWithQuestion(state.QName(), dns.TypeA)
    fake.Zone = state.Zone
    _, _, err = plugin.A(ctx, &k, zone, fake, nil, plugin.Options{})
}
```

**왜 default에서 가짜 A 조회를 하는가?**

지원하지 않는 쿼리 타입이라도, 해당 이름이 존재하는지(NODATA) 아니면 존재하지 않는지(NXDOMAIN)를 구분해야 한다. DNS 스펙에 따라 이 두 경우의 응답 코드가 다르다.

### 5.4 에러 처리와 Fall-through

```
if k.IsNameError(err) {
    if k.Fall.Through(state.Name()) {
        return plugin.NextOrFailure(k.Name(), k.Next, ctx, w, r)
    }
    if !k.APIConn.HasSynced() {
        return plugin.BackendError(ctx, &k, zone, dns.RcodeServerFailure, state, nil, plugin.Options{})
    }
    return plugin.BackendError(ctx, &k, zone, dns.RcodeNameError, state, nil, plugin.Options{})
}
```

**Fall-through 메커니즘**: Kubernetes에서 이름을 찾지 못했을 때, `fallthrough` 옵션이 설정되어 있으면 다음 플러그인(예: forward)으로 쿼리를 전달한다. 이를 통해 클러스터 외부 도메인도 처리할 수 있다.

**동기화 미완료 시**: API가 아직 동기화되지 않았으면 SERVFAIL을 반환한다. 이는 거짓 NXDOMAIN을 방지하기 위한 안전장치이다.

### 5.5 전체 ServeDNS 흐름도

```
DNS 쿼리 수신
    │
    ▼
Zone 매칭 ──── 불일치 ──→ 다음 플러그인
    │
    │ 일치
    ▼
쿼리 타입 분기
    │
    ├── A/AAAA ──→ plugin.A/AAAA() ──→ k.Services() ──→ k.Records()
    ├── SRV    ──→ plugin.SRV()    ──→ k.Services() ──→ k.Records()
    ├── PTR    ──→ plugin.PTR()    ──→ k.Reverse()
    ├── TXT    ──→ plugin.TXT()    ──→ k.Services()
    ├── SOA    ──→ plugin.SOA()    ──→ k.Serial(), k.MinTTL()
    ├── NS     ──→ plugin.NS()     ──→ k.nsAddrs()
    └── Other  ──→ Fake A 조회 (NODATA vs NXDOMAIN 판별)
    │
    ▼
에러 확인
    │
    ├── NameError + Fall-through ──→ 다음 플러그인
    ├── NameError + 미동기화     ──→ SERVFAIL
    ├── NameError               ──→ NXDOMAIN
    ├── 기타 에러                ──→ SERVFAIL
    └── 성공
         │
         ▼
    응답 메시지 구성
    (Authoritative=true, Truncated 설정)
         │
         ▼
    w.WriteMsg(m) → 클라이언트에 응답
```

---

## 6. DNS 스키마 파싱

### 6.1 parseRequest 함수

`plugin/kubernetes/parse.go:31-89`에서 DNS 쿼리 이름을 파싱한다.

```
func parseRequest(name, zone string, multicluster bool) (r recordRequest, err error) {
    // 4가지 케이스:
    // 1. _port._protocol.service.namespace.pod|svc.zone
    // 2. endpoint.service.namespace.pod|svc.zone
    // 3. service.namespace.pod|svc.zone
    // 4. endpoint.cluster.service.namespace.pod|svc.zone (멀티클러스터)

    base, _ := dnsutil.TrimZone(name, zone)
    if base == "" || base == Svc || base == Pod {
        return r, nil  // apex 쿼리 → NODATA
    }
    segs := dns.SplitDomainName(base)
    // ...
}
```

### 6.2 DNS 이름 스키마

Kubernetes DNS 스펙 버전 1.1.0을 구현한다:

```
┌─────────────────────────────────────────────────────────────────────┐
│                    Kubernetes DNS 스키마                            │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  서비스 (ClusterIP):                                                │
│    <service>.<namespace>.svc.<zone>                                 │
│    예: my-svc.default.svc.cluster.local                            │
│    → ClusterIP A/AAAA 레코드                                       │
│                                                                     │
│  서비스 (Headless):                                                 │
│    <service>.<namespace>.svc.<zone>                                 │
│    → 모든 엔드포인트 IP의 A/AAAA 레코드                             │
│                                                                     │
│  엔드포인트:                                                        │
│    <hostname>.<service>.<namespace>.svc.<zone>                     │
│    예: 10-244-0-5.my-svc.default.svc.cluster.local                 │
│    → 개별 엔드포인트의 A/AAAA 레코드                                │
│                                                                     │
│  SRV 레코드:                                                       │
│    _<port>._<protocol>.<service>.<namespace>.svc.<zone>            │
│    예: _https._tcp.my-svc.default.svc.cluster.local                │
│    → SRV 레코드 (포트/프로토콜 정보 포함)                            │
│                                                                     │
│  Pod:                                                               │
│    <pod-ip-dashed>.<namespace>.pod.<zone>                          │
│    예: 10-244-0-5.default.pod.cluster.local                        │
│    → Pod IP의 A 레코드                                             │
│                                                                     │
│  ExternalName 서비스:                                               │
│    <service>.<namespace>.svc.<zone>                                │
│    → CNAME 레코드 (외부 도메인으로)                                  │
│                                                                     │
│  멀티클러스터:                                                      │
│    <endpoint>.<cluster>.<service>.<namespace>.svc.<zone>           │
│    → 다른 클러스터의 엔드포인트 A/AAAA 레코드                        │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 6.3 파싱 분기 (label 수에 따라)

```
switch last {
case 0: // label이 1개 남음 → 엔드포인트만
    r.endpoint = segs[last]
case 1: // label이 2개 남음
    if !multicluster || strings.HasPrefix(segs[last], "_") || strings.HasPrefix(segs[last-1], "_") {
        // SRV: _port._protocol
        r.protocol = stripUnderscore(segs[last])
        r.port = stripUnderscore(segs[last-1])
    } else {
        // 멀티클러스터: endpoint.cluster
        r.cluster = segs[last]
        r.endpoint = segs[last-1]
    }
default: // 너무 긴 쿼리
    return r, errInvalidRequest
}
```

**왜 밑줄(_) 접두사로 구분하는가?**

DNS SRV 레코드 표준(RFC 2782)에서 포트와 프로토콜 이름 앞에 밑줄을 붙이도록 규정하고 있다. 이를 활용하여 SRV 쿼리와 멀티클러스터 엔드포인트 쿼리를 구분한다.

---

## 7. Records 함수 - 서비스 레코드 조회

### 7.1 Records 진입점

```
// plugin/kubernetes/kubernetes.go:332-363
func (k *Kubernetes) Records(ctx context.Context, state request.Request, exact bool) ([]msg.Service, error) {
    multicluster := k.isMultiClusterZone(state.Zone)
    r, e := parseRequest(state.Name(), state.Zone, multicluster)
    if e != nil {
        return nil, e
    }
    if r.podOrSvc == "" {
        return nil, nil
    }
    if dnsutil.IsReverse(state.Name()) > 0 {
        return nil, errNoItems
    }
    if !k.namespaceExposed(r.namespace) {
        return nil, errNsNotExposed
    }
    if r.podOrSvc == Pod {
        pods, err := k.findPods(r, state.Zone)
        return pods, err
    }
    var services []msg.Service
    var err error
    if !multicluster {
        services, err = k.findServices(r, state.Zone)
    } else {
        services, err = k.findMultiClusterServices(r, state.Zone)
    }
    return services, err
}
```

### 7.2 findServices 흐름

`plugin/kubernetes/kubernetes.go:445-562`에서 서비스를 찾는 과정:

```
서비스 찾기 흐름:
    │
    ▼
1. 네임스페이스 노출 확인
    │
    ▼
2. 인덱스로 서비스 목록 조회
   idx := object.ServiceKey(r.service, r.namespace)
   serviceList = k.APIConn.SvcIndex(idx)
    │
    ▼
3. 각 서비스에 대해:
    │
    ├── ExternalName 서비스?
    │   → CNAME 레코드 생성 (endpoint/port/protocol 무시)
    │   → s.Host = svc.ExternalName
    │
    ├── Headless 서비스 또는 endpoint 지정?
    │   → EndpointSlice에서 엔드포인트 조회
    │   → 각 엔드포인트 IP + 포트로 레코드 생성
    │   → 포트/프로토콜 매칭 적용
    │
    └── ClusterIP 서비스?
        → ClusterIP + 포트로 레코드 생성
        → 듀얼 스택: 여러 ClusterIP 지원
```

### 7.3 빈 서비스 무시 (ignoreEmptyService)

```
if k.opts.ignoreEmptyService && svc.Type != api.ServiceTypeExternalName && !svc.Headless() {
    podsCount := 0
    for _, ep := range endpointsListFunc() {
        for _, eps := range ep.Subsets {
            podsCount += len(eps.Addresses)
        }
    }
    if podsCount == 0 {
        continue  // 엔드포인트가 없으면 NXDOMAIN
    }
}
```

`ignore empty_service` 옵션이 설정되면, 백엔드 Pod가 없는 서비스에 대해 NXDOMAIN을 반환한다.

---

## 8. Pod 모드

### 8.1 세 가지 Pod 모드

`plugin/kubernetes/kubernetes.go:76-82`에 정의:

| 모드 | 상수 | 동작 |
|------|------|------|
| `disabled` | `podModeDisabled` | Pod DNS 요청을 완전히 무시 (기본값) |
| `verified` | `podModeVerified` | Pod가 실제로 존재하는지 확인 후 응답 |
| `insecure` | `podModeInsecure` | 존재 여부 확인 없이 응답 |

### 8.2 findPods 구현

```
// plugin/kubernetes/kubernetes.go:385-442
func (k *Kubernetes) findPods(r recordRequest, zone string) (pods []msg.Service, err error) {
    if k.podMode == podModeDisabled {
        return nil, errNoItems
    }
    // ...
    // IP 파싱: 10-244-0-5 → 10.244.0.5
    if strings.Count(podname, "-") == 3 && !strings.Contains(podname, "--") {
        ip = strings.ReplaceAll(podname, "-", ".")  // IPv4
    } else {
        ip = strings.ReplaceAll(podname, "-", ":")   // IPv6
    }

    if k.podMode == podModeInsecure {
        // IP 유효성만 확인, Pod 존재 여부 미확인
        if net.ParseIP(ip) == nil {
            return nil, errNoItems
        }
        return []msg.Service{{Key: ..., Host: ip, TTL: k.ttl}}, err
    }

    // PodModeVerified: Pod 캐시에서 실제 존재 확인
    for _, p := range k.APIConn.PodIndex(ip) {
        if ip == p.PodIP && match(namespace, p.Namespace) {
            // 매칭되는 Pod 존재 → 응답
        }
    }
}
```

**왜 insecure 모드가 존재하는가?**

kube-dns 호환성을 위해서이다. kube-dns는 Pod 존재 여부를 확인하지 않았다. 하지만 이 모드는 DNS 리바인딩 공격에 취약할 수 있어 프로덕션에서는 `verified` 모드를 권장한다.

**왜 disabled가 기본값인가?**

대부분의 경우 Pod DNS 레코드(`10-244-0-5.namespace.pod.cluster.local`)는 필요하지 않다. 서비스 DNS만으로 충분하며, Pod 모드를 활성화하면 Pod Informer가 추가로 실행되어 메모리와 API 부하가 증가한다.

---

## 9. 외부 CNAME 해석 (Upstream)

### 9.1 ExternalName 서비스의 CNAME

ExternalName 타입의 서비스는 CNAME 레코드를 반환한다:

```
// plugin/kubernetes/kubernetes.go:494-507
if svc.Type == api.ServiceTypeExternalName {
    if r.endpoint != "" || r.port != "" || r.protocol != "" {
        continue  // ExternalName에는 endpoint/port 하위 도메인 불가
    }
    s := msg.Service{
        Key:  strings.Join([]string{zonePath, Svc, svc.Namespace, svc.Name}, "/"),
        Host: svc.ExternalName,
        TTL:  k.ttl,
    }
    if t, _ := s.HostType(); t == dns.TypeCNAME {
        services = append(services, s)
    }
}
```

### 9.2 Upstream Lookup

```
// plugin/kubernetes/kubernetes.go:178-180
func (k *Kubernetes) Lookup(ctx context.Context, state request.Request, name string, typ uint16) (*dns.Msg, error) {
    return k.Upstream.Lookup(ctx, state, name, typ)
}
```

CNAME 타겟이 클러스터 외부 도메인인 경우, `plugin/pkg/upstream`을 통해 다른 플러그인 체인으로 재귀 조회를 수행한다.

---

## 10. 역방향 DNS (PTR)

### 10.1 Reverse 함수

```
// plugin/kubernetes/reverse.go:14-26
func (k *Kubernetes) Reverse(ctx context.Context, state request.Request, exact bool, opt plugin.Options) ([]msg.Service, error) {
    ip := dnsutil.ExtractAddressFromReverse(state.Name())
    if ip == "" {
        _, e := k.Records(ctx, state, exact)
        return nil, e
    }
    records := k.serviceRecordForIP(ip, state.Name())
    if len(records) == 0 {
        return records, errNoItems
    }
    return records, nil
}
```

### 10.2 serviceRecordForIP

```
// plugin/kubernetes/reverse.go:30-55
func (k *Kubernetes) serviceRecordForIP(ip, name string) []msg.Service {
    // 1. ClusterIP로 서비스 역조회
    for _, service := range k.APIConn.SvcIndexReverse(ip) {
        domain := strings.Join([]string{service.Name, service.Namespace, Svc, k.primaryZone()}, ".")
        return []msg.Service{{Host: domain, TTL: k.ttl}}
    }
    // 2. ClusterIP 매칭 실패 시 EndpointSlice에서 역조회
    for _, ep := range k.APIConn.EpIndexReverse(ip) {
        for _, eps := range ep.Subsets {
            for _, addr := range eps.Addresses {
                if addr.IP == ip {
                    domain := strings.Join([]string{
                        endpointHostname(addr, k.endpointNameMode),
                        ep.Index, Svc, k.primaryZone(),
                    }, ".")
                    svcs = append(svcs, msg.Service{Host: domain, TTL: k.ttl})
                }
            }
        }
    }
    return svcs
}
```

---

## 11. 변경 감지 메커니즘

### 11.1 이벤트 핸들러

```
// plugin/kubernetes/controller.go:668-670
func (dns *dnsControl) Add(obj any)               { dns.updateModified() }
func (dns *dnsControl) Delete(obj any)            { dns.updateModified() }
func (dns *dnsControl) Update(oldObj, newObj any) { dns.detectChanges(oldObj, newObj) }
```

Add와 Delete는 항상 변경으로 간주하지만, Update는 `detectChanges`로 실제 변경 여부를 확인한다.

### 11.2 detectChanges

```
// plugin/kubernetes/controller.go:673-708
func (dns *dnsControl) detectChanges(oldObj, newObj any) {
    // ResourceVersion이 같으면 동일 객체
    if newObj != nil && oldObj != nil && (oldObj.(meta.Object).GetResourceVersion() == newObj.(meta.Object).GetResourceVersion()) {
        return
    }
    switch ob := obj.(type) {
    case *object.Service:
        imod, emod := serviceModified(oldObj, newObj)
        // 내부/외부 변경을 별도로 추적
    case *object.Endpoints:
        if !endpointsEquivalent(oldObj, newObj) {
            dns.updateModified()
        }
    // ...
    }
}
```

**왜 ResourceVersion을 먼저 확인하는가?**

Informer는 re-list 시 동일한 객체에 대해 Update 이벤트를 발생시킬 수 있다. ResourceVersion이 같으면 실제 변경이 아니므로 불필요한 SOA serial 업데이트를 방지한다.

### 11.3 endpointsEquivalent

엔드포인트 변경 감지는 IP, 호스트명, 포트 등 DNS에 영향을 미치는 필드만 비교한다:

```
// plugin/kubernetes/controller.go:713-747
func subsetsEquivalent(sa, sb object.EndpointSubset) bool {
    if len(sa.Addresses) != len(sb.Addresses) { return false }
    if len(sa.Ports) != len(sb.Ports) { return false }
    for addr, aaddr := range sa.Addresses {
        baddr := sb.Addresses[addr]
        if aaddr.IP != baddr.IP { return false }
        if aaddr.Hostname != baddr.Hostname { return false }
    }
    for port, aport := range sa.Ports {
        bport := sb.Ports[port]
        if aport.Name != bport.Name { return false }
        if aport.Port != bport.Port { return false }
        if aport.Protocol != bport.Protocol { return false }
    }
    return true
}
```

---

## 12. setup.go - 플러그인 초기화

### 12.1 등록과 초기화 흐름

```
// plugin/kubernetes/setup.go:27-31
const pluginName = "kubernetes"

func init() { plugin.Register(pluginName, setup) }

func setup(c *caddy.Controller) error {
    klog.SetLogger(logr.New(&loggerAdapter{log}))
    k, err := kubernetesParse(c)
    if err != nil {
        return plugin.Error(pluginName, err)
    }
    onStart, onShut, err := k.InitKubeCache(context.Background())
    // ...
    c.OnStartup(onStart)
    c.OnShutdown(onShut)
    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        k.Next = next
        return k
    })
    c.OnStartup(func() error {
        k.localIPs = boundIPs(c)
        return nil
    })
}
```

### 12.2 Corefile 파싱 (ParseStanza)

```
// plugin/kubernetes/setup.go:89-303 (주요 부분)
func ParseStanza(c *caddy.Controller) (*Kubernetes, error) {
    k8s := New([]string{""})
    k8s.Zones = plugin.OriginsFromArgsOrServerBlock(c.RemainingArgs(), c.ServerBlockKeys)
    k8s.Upstream = upstream.New()
    k8s.startupTimeout = time.Second * 5

    for c.NextBlock() {
        switch c.Val() {
        case "pods":           // pods disabled|verified|insecure
        case "namespaces":     // namespaces ns1 ns2 ...
        case "endpoint":       // endpoint URL
        case "tls":            // tls cert key cacert
        case "labels":         // labels key=value
        case "namespace_labels": // namespace_labels key=value
        case "fallthrough":    // fallthrough [zones...]
        case "ttl":            // ttl 5
        case "noendpoints":    // EndpointSlice 캐시 비활성화
        case "ignore":         // ignore empty_service
        case "kubeconfig":     // kubeconfig path [context]
        case "multicluster":   // multicluster zone1 zone2
        case "startup_timeout": // startup_timeout 5s
        case "apiserver_qps":   // apiserver_qps 100
        case "apiserver_burst": // apiserver_burst 200
        case "apiserver_max_inflight": // apiserver_max_inflight 50
        case "endpoint_pod_names": // 엔드포인트에 Pod 이름 사용
        }
    }
}
```

### 12.3 Corefile 설정 예시

```
cluster.local:53 {
    kubernetes cluster.local in-addr.arpa ip6.arpa {
        pods verified
        namespaces default kube-system
        ttl 30
        fallthrough in-addr.arpa ip6.arpa
        endpoint https://api.example.com
        tls /path/to/cert /path/to/key /path/to/ca
        labels app=backend
        ignore empty_service
        startup_timeout 10s
        apiserver_qps 100
        apiserver_burst 200
        multicluster clusterset.local
    }
}
```

### 12.4 InitKubeCache

```
// plugin/kubernetes/kubernetes.go:235-329
func (k *Kubernetes) InitKubeCache(ctx context.Context) (onStart func() error, onShut func() error, err error) {
    config, err := k.getClientConfig()
    // ...
    kubeClient, err := kubernetes.NewForConfig(config)
    // ...
    k.opts.initPodCache = k.podMode == podModeVerified
    k.APIConn = newdnsController(ctx, kubeClient, mcsClient, k.opts)

    onStart = func() error {
        go func() { k.APIConn.Run() }()
        // 동기화 대기 (100ms 간격 체크)
        timeoutTicker := time.NewTicker(k.startupTimeout)
        for {
            select {
            case <-checkSyncTicker.C:
                if k.APIConn.HasSynced() { return nil }
            case <-timeoutTicker.C:
                log.Warning("starting server with unsynced Kubernetes API")
                return nil
            }
        }
    }
}
```

**시작 타임아웃**: 기본 5초. API 동기화가 이 시간 내에 완료되지 않으면 경고를 출력하고 서버를 시작한다. 이 경우 동기화 완료 전까지 SERVFAIL을 반환한다.

---

## 13. 멀티클러스터 지원

### 13.1 ServiceImport와 MultiClusterEndpoints

Kubernetes Multi-Cluster Services API(mcs-api)를 지원한다.

```
// plugin/kubernetes/controller.go:206-238
if len(opts.multiclusterZones) > 0 {
    // MultiCluster EndpointSlice 감시 (LabelServiceName 레이블 필수)
    mcsEpReq, _ := labels.NewRequirement(mcs.LabelServiceName, selection.Exists, []string{})
    // ...
    dns.mcEpLister, dns.mcEpController = object.NewIndexerInformer(...)
    dns.svcImportLister, dns.svcImportController = object.NewIndexerInformer(...)
}
```

### 13.2 findMultiClusterServices

```
// plugin/kubernetes/kubernetes.go:565-666
func (k *Kubernetes) findMultiClusterServices(r recordRequest, zone string) (services []msg.Service, err error) {
    idx := object.ServiceImportKey(r.service, r.namespace)
    serviceList = k.APIConn.SvcImportIndex(idx)
    endpointsListFunc = func() []*object.MultiClusterEndpoints { return k.APIConn.McEpIndex(idx) }
    // ...
    for _, ep := range endpointsList {
        for _, eps := range ep.Subsets {
            for _, addr := range eps.Addresses {
                if r.endpoint != "" {
                    // 클러스터 ID와 엔드포인트 이름 모두 매칭 필요
                    if !match(r.cluster, ep.ClusterId) || !match(r.endpoint, endpointHostname(addr, k.endpointNameMode)) {
                        continue
                    }
                }
                // Key에 ClusterId 포함
                s.Key = strings.Join([]string{zonePath, Svc, svc.Namespace, svc.Name, ep.ClusterId, endpointHostname(addr, k.endpointNameMode)}, "/")
            }
        }
    }
}
```

멀티클러스터 DNS 스키마:
```
<endpoint>.<cluster-id>.<service>.<namespace>.svc.<multicluster-zone>
```

---

## 14. API Rate Limiting

### 14.1 maxInflight RoundTripper

```
// plugin/kubernetes/kubernetes.go:705-720
func newMaxInflightRoundTripper(next http.RoundTripper, max int) http.RoundTripper {
    if max <= 0 {
        return next
    }
    sem := make(chan struct{}, max)
    return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
        select {
        case sem <- struct{}{}:
            defer func() { <-sem }()
            return next.RoundTrip(r)
        case <-r.Context().Done():
            return nil, r.Context().Err()
        }
    })
}
```

**세마포어 패턴**: 채널을 세마포어로 사용하여 동시 API 요청 수를 제한한다. 대기 중 컨텍스트가 취소되면 즉시 에러를 반환한다.

### 14.2 QPS와 Burst 설정

```
// plugin/kubernetes/kubernetes.go:272-278
if k.apiQPS > 0 {
    config.QPS = k.apiQPS
}
if k.apiBurst > 0 {
    config.Burst = k.apiBurst
}
```

client-go의 내장 레이트 리미터를 활용한다. `apiserver_qps`는 초당 쿼리 수, `apiserver_burst`는 버스트 허용량이다.

---

## 15. NS 레코드와 SOA 레코드

### 15.1 nsAddrs 함수

```
// plugin/kubernetes/ns.go:16-101
func (k *Kubernetes) nsAddrs(external, headless bool, zone string) []dns.RR {
    // 1. localIPs에서 CoreDNS 엔드포인트 찾기
    for _, localIP := range k.localIPs {
        endpoints := k.APIConn.EpIndexReverse(localIP.String())
        for _, endpoint := range endpoints {
            svcs := k.APIConn.SvcIndex(endpoint.Index)
            // 서비스의 ClusterIP 또는 엔드포인트 IP를 NS 레코드로 사용
        }
    }
    // 2. 엔드포인트를 찾지 못하면 localIPs 직접 사용
    if !foundEndpoint {
        for i, localIP := range k.localIPs {
            svcNames[i] = defaultNSName + zone  // "ns.dns.<zone>"
            svcIPs[i] = localIP
        }
    }
}
```

NS 레코드의 이름은 기본적으로 `ns.dns.<zone>` (예: `ns.dns.cluster.local.`)이다.

### 15.2 SOA Serial

```
// plugin/kubernetes/kubernetes.go:669-675
func (k *Kubernetes) Serial(state request.Request) uint32 {
    if !k.isMultiClusterZone(state.Zone) {
        return uint32(k.APIConn.Modified(ModifiedInternal))
    } else {
        return uint32(k.APIConn.Modified(ModifiedMultiCluster))
    }
}
```

SOA serial은 마지막 리소스 변경의 Unix 타임스탬프를 사용한다. 이는 클라이언트가 Zone 데이터의 변경을 감지할 수 있게 한다.

---

## 16. endpointHostname 함수

```
// plugin/kubernetes/kubernetes.go:365-383
func endpointHostname(addr object.EndpointAddress, endpointNameMode bool) string {
    if addr.Hostname != "" {
        return addr.Hostname
    }
    if endpointNameMode && addr.TargetRefName != "" {
        return addr.TargetRefName  // Pod 이름 사용
    }
    if strings.Contains(addr.IP, ".") {
        return strings.ReplaceAll(addr.IP, ".", "-")  // IPv4: 10.0.0.1 → 10-0-0-1
    }
    if strings.Contains(addr.IP, ":") {
        ipv6Hostname := strings.ReplaceAll(addr.IP, ":", "-")
        if strings.HasSuffix(ipv6Hostname, "-") {
            return ipv6Hostname + "0"  // :: → -0
        }
        return ipv6Hostname
    }
    return ""
}
```

**엔드포인트 호스트명 결정 우선순위**:
1. 명시적 Hostname 필드
2. `endpoint_pod_names` 모드에서 Pod 이름 (TargetRefName)
3. IP 주소를 대시(-)로 변환한 문자열

---

## 17. 성능 최적화 설계

### 17.1 Lazy Endpoint 로딩

```
endpointsListFunc = func() []*object.Endpoints { return k.APIConn.EpIndex(idx) }
```

EndpointSlice 목록은 함수로 감싸서 **실제로 필요할 때만 조회**한다. ClusterIP 서비스는 엔드포인트를 조회할 필요가 없으므로 불필요한 캐시 조회를 피한다.

### 17.2 경량 객체 변환

`object.ToService`, `object.ToPod` 등의 변환기는 Kubernetes API 객체에서 DNS에 필요한 필드만 추출하여 메모리 사용량을 최소화한다.

### 17.3 Protobuf 전송

```
cc.ContentType = "application/vnd.kubernetes.protobuf"
```

API 서버와의 통신에 JSON 대신 Protobuf를 사용하여 직렬화/역직렬화 오버헤드를 줄인다.

---

## 18. 정리

Kubernetes 플러그인은 CoreDNS에서 가장 복잡하고 중요한 플러그인이다.

```
┌──────────────────────────────────────────────────────────────┐
│                     Kubernetes 플러그인 아키텍처              │
│                                                              │
│  Corefile 파싱                                               │
│       │                                                      │
│       ▼                                                      │
│  setup() → kubernetesParse() → ParseStanza()                │
│       │                                                      │
│       ▼                                                      │
│  InitKubeCache() → getClientConfig()                        │
│       │              → newdnsController()                     │
│       │                   │                                  │
│       │                   ├── svc Informer (3개 인덱스)       │
│       │                   ├── ep  Informer (2개 인덱스)       │
│       │                   ├── pod Informer (1개 인덱스)       │
│       │                   ├── ns  Informer                   │
│       │                   ├── svcImport Informer (멀티클러스터)│
│       │                   └── mcEp Informer (멀티클러스터)    │
│       │                                                      │
│       ▼                                                      │
│  onStart() → APIConn.Run() + HasSynced() 대기               │
│                                                              │
│  DNS 쿼리 처리:                                              │
│  ServeDNS() → Zone 매칭                                      │
│       │ → 쿼리 타입 분기                                      │
│       │ → Services()/Records()                               │
│       │     → parseRequest()                                 │
│       │     → findServices() / findPods() / findMultiCluster │
│       │ → 에러 처리 (Fall-through / SERVFAIL / NXDOMAIN)     │
│       └ → 응답 작성                                          │
└──────────────────────────────────────────────────────────────┘
```

핵심 설계 결정:
1. **Informer 패턴**: Watch 기반 캐시로 API 서버 부하 최소화
2. **다중 인덱스**: 정방향(이름), 역방향(IP) 조회 모두 O(1)
3. **선택적 Controller**: Pod/Endpoint Controller는 필요할 때만 활성화
4. **Fall-through**: 클러스터 외부 도메인을 다른 플러그인에 위임
5. **경량 객체**: API 객체를 DNS에 필요한 필드만 추출하여 메모리 절약
