# 23. Kubernetes Ingress 호환 + VM/WorkloadEntry 지원 Deep-Dive

> Istio의 Kubernetes Ingress 리소스 자동 변환 시스템과 VM 워크로드 자동 등록(AutoRegistration) 메커니즘

---

## 1. 개요

Istio는 두 가지 독립적이지만 상호 보완적인 서브시스템을 통해 Kubernetes 네이티브 리소스와 비-Kubernetes 워크로드를 모두 서비스 메시에 통합한다:

1. **Ingress 호환 계층 (`pilot/pkg/config/kube/ingress/`)**: Kubernetes `networking.k8s.io/v1` Ingress 리소스를 Istio의 `Gateway` + `VirtualService`로 **자동 변환**하여, 기존 Ingress 사용자가 Istio 마이그레이션 시 설정을 변경하지 않아도 되도록 한다.

2. **WorkloadEntry 자동 등록 (`pilot/pkg/autoregistration/`)**: VM이나 베어메탈에서 실행되는 비-Kubernetes 워크로드가 xDS 연결을 통해 **자동으로 WorkloadEntry를 생성**하고, 연결 해제 시 일정 유예 기간 후 **자동 정리**되는 생명주기를 관리한다.

```
┌─────────────────────────────────────────────────────────────────────┐
│                      Istio Control Plane (istiod)                   │
│                                                                     │
│  ┌──────────────────────────┐  ┌─────────────────────────────────┐ │
│  │  Ingress Controller      │  │  AutoRegistration Controller    │ │
│  │                          │  │                                 │ │
│  │  K8s Ingress             │  │  VM/Bare-metal                  │ │
│  │    ↓ (krt 변환)           │  │    ↓ (xDS Connect)              │ │
│  │  Gateway + VirtualService│  │  WorkloadEntry 생성/갱신/삭제    │ │
│  │    ↓                     │  │    ↓                            │ │
│  │  xDS Push (CDS/EDS/LDS) │  │  Health Check + Cleanup         │ │
│  └──────────────────────────┘  └─────────────────────────────────┘ │
│                                                                     │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │              krt (Kubernetes Resource Transform)              │  │
│  │  Ingress → IngressRule → VirtualService (reactive pipeline)  │  │
│  └──────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 2. Ingress 호환 계층 아키텍처

### 2.1 설계 철학

Istio는 Kubernetes의 표준 `Ingress` 리소스를 **읽기 전용(read-only) 뷰**로 관찰하고, 이를 Istio 내부 모델(`Gateway`, `VirtualService`)로 변환한다. 변환은 **krt(Kubernetes Resource Transform)** 프레임워크 위에 구현되어, Ingress 변경 시 자동으로 파생 리소스가 갱신된다.

```
// pilot/pkg/config/kube/ingress/controller.go

package ingress  // read-only view of Kubernetes ingress resources

var errUnsupportedOp = errors.New(
    "unsupported operation: the ingress config store is a read-only view")
```

**왜 읽기 전용인가?** Ingress 컨트롤러는 Ingress 리소스를 **소비**만 하고 **변경하지 않는다**. Istio가 직접 Ingress를 수정하면 다른 Ingress 컨트롤러(nginx 등)와 충돌할 수 있기 때문이다.

### 2.2 컨트롤러 구조

```go
// pilot/pkg/config/kube/ingress/controller.go

type Controller struct {
    client     kube.Client
    stop       chan struct{}
    xdsUpdater xdsConfigUpdater
    handlers   []krt.HandlerRegistration
    inputs     Inputs
    outputs    Outputs
    status     *status.StatusCollections
    tagWatcher krt.RecomputeProtected[revisions.TagWatcher]
}

type Inputs struct {
    Ingresses      krt.Collection[*knetworking.Ingress]
    IngressClasses krt.Collection[*knetworking.IngressClass]
    Services       krt.Collection[*corev1.Service]
    Nodes          krt.Collection[*corev1.Node]
    Pods           krt.Collection[*corev1.Pod]
    MeshConfig     krt.Collection[meshwatcher.MeshConfigResource]
}

type Outputs struct {
    VirtualServices krt.Collection[config.Config]
    Gateways        krt.Collection[config.Config]
}
```

**핵심 포인트**: Controller는 6개의 입력(Inputs)을 관찰하고 2개의 출력(Outputs)을 생성하는 **순수 변환 파이프라인**이다. Create/Update/Delete/Patch 메서드는 모두 `errUnsupportedOp`을 반환한다.

### 2.3 krt 기반 변환 파이프라인

```
Ingress → SupportedIngresses → RuleCollection → VirtualServices
                                                  Gateways
```

각 단계를 상세히 살펴보자:

#### 단계 1: SupportedIngresses 필터링

```go
// pilot/pkg/config/kube/ingress/ingress.go

func SupportedIngresses(
    ingressClass krt.Collection[*knetworking.IngressClass],
    ingress krt.Collection[*knetworking.Ingress],
    meshConfig meshwatcher.WatcherCollection,
    services krt.Collection[*corev1.Service],
    nodes krt.Collection[*corev1.Node],
    pods krt.Collection[*corev1.Pod],
    opts krt.OptionsBuilder,
) (krt.Collection[krt.ObjectWithStatus[...]],
   krt.Collection[*knetworking.Ingress]) {
    // ...
    if !shouldProcessIngressWithClass(mesh.MeshConfig, i, class) {
        return nil, nil  // Istio가 처리할 대상이 아님
    }
    // LoadBalancer IP/Hostname 계산
    wantIPs := sliceToStatus(runningAddresses(...))
    return &knetworking.IngressStatus{
        LoadBalancer: knetworking.IngressLoadBalancerStatus{
            Ingress: wantIPs,
        },
    }, &i
}
```

**Ingress 필터링 로직**:

```go
// pilot/pkg/config/kube/ingress/virtualservices.go

func shouldProcessIngressWithClass(mesh, ingress, ingressClass) bool {
    if class, exists := ingress.Annotations["kubernetes.io/ingress.class"]; exists {
        switch mesh.IngressControllerMode {
        case OFF:     return false
        case STRICT:  return class == mesh.IngressClass
        case DEFAULT: return class == mesh.IngressClass
        }
    } else if ingressClass != nil {
        return ingressClass.Spec.Controller == "istio.io/ingress-controller"
    } else {
        switch mesh.IngressControllerMode {
        case OFF:     return false
        case STRICT:  return false     // 명시적 클래스 필수
        case DEFAULT: return true      // 클래스 없으면 Istio가 처리
        }
    }
}
```

| 모드 | 어노테이션 있음 | IngressClass 있음 | 어노테이션/클래스 없음 |
|------|----------------|-------------------|---------------------|
| OFF | 무시 | 무시 | 무시 |
| STRICT | 일치 시만 | Controller 일치 시만 | **무시** |
| DEFAULT | 일치 시만 | Controller 일치 시만 | **처리** |

#### 단계 2: RuleCollection 인덱싱

```go
// pilot/pkg/config/kube/ingress/ingress.go

type IngressRule struct {
    IngressName       string
    IngressNamespace  string
    RuleIndex         int
    CreationTimestamp time.Time
    Rule              *knetworking.IngressRule
}

func RuleCollection(
    ingress krt.Collection[*knetworking.Ingress],
    opts krt.OptionsBuilder,
) (krt.Collection[*IngressRule], krt.Index[string, *IngressRule]) {
    collection := krt.NewManyCollection(ingress, func(ctx, i) []*IngressRule {
        if i.Spec.DefaultBackend != nil {
            log.Infof("Ignore default wildcard ingress...")
        }
        var rules []*IngressRule
        for idx, rule := range i.Spec.Rules {
            if rule.HTTP == nil { continue }
            rules = append(rules, &IngressRule{...})
        }
        return rules
    })
    // 호스트별 인덱스: 같은 호스트를 가진 여러 Ingress의 규칙을 그룹핑
    index := krt.NewIndex(collection, "host", func(rule) []string {
        host := rule.Rule.Host
        if host == "" { host = "*" }
        return []string{host}
    })
    return collection, index
}
```

**왜 호스트별 인덱스를 사용하는가?** 여러 Ingress 리소스가 동일한 호스트에 대한 규칙을 정의할 수 있다. 이들을 하나의 VirtualService로 병합하려면 호스트별로 그룹핑해야 한다.

#### 단계 3: VirtualService 생성

```go
// pilot/pkg/config/kube/ingress/virtualservices.go

func VirtualServices(
    ruleHostIndex krt.Index[string, *IngressRule],
    services krt.Collection[ServiceWithPorts],
    domainSuffix string,
    opts krt.OptionsBuilder,
) krt.Collection[config.Config] {
    return krt.NewCollection(
        ruleHostIndex.AsCollection(),
        func(ctx, hostRules krt.IndexObject[string, *IngressRule]) *config.Config {
            host, rules := hostRules.Key, hostRules.Objects
            sortRulesByCreationTimestamp(rules)  // 생성 시간순 정렬

            httpRoutes := make([]*networking.HTTPRoute, 0)
            for _, rule := range rules {
                httpRoutes = append(httpRoutes,
                    convertIngressRule(rule, domainSuffix, ctx, services)...)
            }
            sortHTTPRoutes(httpRoutes)  // 경로 길이/정확도순 정렬

            virtualService := &networking.VirtualService{
                Hosts:    []string{host},
                Gateways: []string{gatewayName},
                Http:     httpRoutes,
            }
            return &config.Config{
                Meta: config.Meta{
                    GroupVersionKind: gvk.VirtualService,
                    Name: host + "-" + ingressName + "-istio-autogenerated-k8s-ingress",
                },
                Spec: virtualService,
            }
        })
}
```

**경로 정렬 규칙** (Kubernetes Ingress 스펙 준수):

```go
func sortHTTPRoutes(httpRoutes []*networking.HTTPRoute) {
    sort.SliceStable(httpRoutes, func(i, j int) bool {
        r1Len, r1Ex := getMatchURILength(httpRoutes[i].Match[0])
        r2Len, r2Ex := getMatchURILength(httpRoutes[j].Match[0])
        if r1Len == r2Len {
            return r1Ex && !r2Ex  // Exact > Prefix (같은 길이일 때)
        }
        return r1Len > r2Len  // 긴 경로 우선
    })
}
```

| 우선순위 | 매칭 유형 | 경로 | 이유 |
|---------|----------|------|------|
| 1 | Exact | `/api/v1/users` (14) | 가장 긴 Exact |
| 2 | Prefix | `/api/v1/users` (14) | 같은 길이, Prefix < Exact |
| 3 | Prefix | `/api/v1` (7) | 더 짧은 Prefix |
| 4 | Prefix | `/` (1) | 가장 짧은 Prefix |

#### 단계 4: Gateway 생성

```go
// pilot/pkg/config/kube/ingress/gateways.go

func Gateways(
    ingress krt.Collection[*knetworking.Ingress],
    meshConfig meshwatcher.WatcherCollection,
    domainSuffix string,
    opts krt.OptionsBuilder,
) krt.Collection[config.Config] {
    return krt.NewCollection(ingress, func(ctx, i) *config.Config {
        gateway := &networking.Gateway{}
        gateway.Selector = getIngressGatewaySelector(
            mesh.IngressSelector, mesh.IngressService)

        // TLS 설정: Ingress의 tls[] 섹션을 Server로 변환
        for _, tls := range i.Spec.TLS {
            gateway.Servers = append(gateway.Servers, &networking.Server{
                Port: &networking.Port{Number: 443, Protocol: "HTTPS"},
                Hosts: tls.Hosts,
                Tls: &networking.ServerTLSSettings{
                    Mode:           SIMPLE,
                    CredentialName: tls.SecretName,
                },
            })
        }
        // 기본 HTTP 포트 추가
        gateway.Servers = append(gateway.Servers, &networking.Server{
            Port: &networking.Port{Number: 80, Protocol: "HTTP"},
            Hosts: []string{"*"},
        })
        return &config.Config{Spec: gateway}
    })
}
```

**게이트웨이 셀렉터 결정 로직**:

```go
func getIngressGatewaySelector(ingressSelector, ingressService string) map[string]string {
    if ingressSelector != "" {
        return {"istio": ingressSelector}           // 명시적 설정
    } else if ingressService != "istio-ingressgateway" && ingressService != "" {
        return {"istio": ingressService}             // 서비스명으로 추론
    }
    return {"istio": "ingressgateway"}               // 기본 설치 기본값
}
```

### 2.4 Ingress Status 업데이트

Ingress 리소스의 `.status.loadBalancer.ingress[]`에 게이트웨이의 외부 IP를 설정한다:

```go
// pilot/pkg/config/kube/ingress/ingress.go

func runningAddresses(ctx, meshConfig, services, nodes, pods, podsByNamespace) []string {
    // 방법 1: IngressService 이름으로 Service의 LoadBalancer IP 조회
    if ingressService != "" {
        svc := fetchService(IngressNamespace, ingressService)
        if svc.Spec.Type == ExternalName {
            return []string{svc.Spec.ExternalName}
        }
        for _, ip := range svc.Status.LoadBalancer.Ingress {
            addrs = append(addrs, ip.IP or ip.Hostname)
        }
        return append(addrs, svc.Spec.ExternalIPs...)
    }

    // 방법 2: IngressGateway Pod가 실행 중인 Node의 ExternalIP 조회
    igPods := fetchPods(igSelector, IngressNamespace, phase=Running)
    for _, pod := range igPods {
        node := fetchNode(pod.Spec.NodeName)
        for _, addr := range node.Status.Addresses {
            if addr.Type == NodeExternalIP { addrs = append(addrs, addr.Address) }
        }
    }
    return addrs
}
```

| 방법 | 조건 | IP 소스 |
|------|------|---------|
| Service LB | `meshConfig.ingressService` 설정됨 | Service.Status.LoadBalancer.Ingress |
| ExternalName | Service.Type == ExternalName | Service.Spec.ExternalName |
| Node External | Service 미설정 | Node.Status.Addresses[ExternalIP] |

---

## 3. WorkloadEntry 자동 등록 (AutoRegistration) 아키텍처

### 3.1 설계 목적

VM이나 물리 서버에서 실행되는 워크로드를 Kubernetes 기반 서비스 메시에 등록하려면, 전통적으로 수동으로 `WorkloadEntry` CRD를 작성해야 했다. AutoRegistration은 이 과정을 자동화한다:

```
VM Proxy (istio-agent)                     istiod
    │                                        │
    │ ── xDS Connect (AutoRegisterGroup) ──→ │
    │                                        │ ← WorkloadEntry 자동 생성
    │ ←── xDS Config Push ────────────────── │
    │                                        │
    │    ... 운영 중 ...                      │
    │                                        │
    │ ── Disconnect ──────────────────────→  │
    │                                        │ ← DisconnectedAt 설정
    │                                        │ ← Grace Period 대기
    │                                        │ ← WorkloadEntry 삭제
```

### 3.2 컨트롤러 구조

```go
// pilot/pkg/autoregistration/controller.go

type Controller struct {
    instanceID       string
    store            model.ConfigStoreController  // K8s 또는 메모리 저장소

    queue            controllers.Queue             // 연결 해제 이벤트 처리
    cleanupLimit     *rate.Limiter                 // K8s API 호출 제한 (20/s)
    cleanupQueue     queue.Delayed                 // 유예 기간 후 정리

    adsConnections   *adsConnections               // 활성 프록시 연결 추적
    lateRegistrationQueue controllers.Queue        // WG 추가 시 기존 연결 처리

    maxConnectionAge time.Duration
    stateStore       *state.Store
    healthController *health.Controller
}
```

**핵심 컴포넌트 역할**:

| 컴포넌트 | 역할 | 왜 필요한가 |
|---------|------|-----------|
| `adsConnections` | 프록시별 활성 연결 추적 | 한 프록시가 여러 연결을 가질 수 있음 (재접속) |
| `cleanupQueue` | 지연 정리 큐 | 유예 기간 동안 재접속 기회 제공 |
| `cleanupLimit` | Rate Limiter (20/s) | K8s API 서버 부하 방지 |
| `lateRegistrationQueue` | WorkloadGroup 추가 이벤트 처리 | WG가 나중에 생성되어도 기존 연결된 프록시 등록 |

### 3.3 연결 생명주기

#### 3.3.1 OnConnect: 연결 시 처리

```go
// pilot/pkg/autoregistration/controller.go

func (c *Controller) OnConnect(conn connection) error {
    proxy := conn.Proxy()
    var entryName string
    var autoCreate bool

    // 경로 1: 자동 등록 (AutoRegisterGroup 설정됨)
    if features.WorkloadEntryAutoRegistration &&
       proxy.Metadata.AutoRegisterGroup != "" {
        entryName = autoregisteredWorkloadEntryName(proxy)
        autoCreate = true

    // 경로 2: 수동 등록 + 헬스체크 (WorkloadEntry 이미 존재)
    } else if features.WorkloadEntryHealthChecks &&
              proxy.Metadata.WorkloadEntry != "" {
        wle := c.store.Get(gvk.WorkloadEntry, proxy.Metadata.WorkloadEntry, ...)
        if wle == nil { return error }  // WLE 없으면 에러
        if health.IsEligibleForHealthStatusUpdates(wle) {
            if err := ensureProxyCanControlEntry(proxy, wle); err != nil {
                return err  // ID 검증 실패
            }
            entryName = wle.Name
        }
    }

    proxy.SetWorkloadEntry(entryName, autoCreate)
    c.adsConnections.Connect(conn)
    return c.onWorkloadConnect(entryName, proxy, conn.ConnectedAt(), autoCreate)
}
```

**WorkloadEntry 이름 생성 규칙**:

```go
func autoregisteredWorkloadEntryName(proxy *model.Proxy) string {
    p := []string{
        proxy.Metadata.AutoRegisterGroup,  // WorkloadGroup 이름
        sanitizeIP(proxy.IPAddresses[0]),   // IP (IPv6 ':'→'-')
    }
    if proxy.Metadata.Network != "" {
        p = append(p, string(proxy.Metadata.Network))
    }
    name := strings.Join(p, "-")
    if len(name) > 253 { name = name[len(name)-253:] }  // K8s 이름 제한
    return name
}
```

예시: WorkloadGroup "my-app", IP "192.168.1.10", Network "vpc-1" → `my-app-192.168.1.10-vpc-1`

#### 3.3.2 registerWorkload: WorkloadEntry 생성

```go
func (c *Controller) registerWorkload(entryName string, proxy *model.Proxy,
    conTime time.Time) error {

    wle := c.store.Get(gvk.WorkloadEntry, entryName, proxy.Metadata.Namespace)
    if wle != nil {
        // 기존 WLE가 있으면 연결 상태만 갱신
        changed, err := c.changeWorkloadEntryStateToConnected(entryName, proxy, conTime)
        if changed { autoRegistrationUpdates.Increment() }
        return err
    }

    // WorkloadGroup에서 템플릿 가져오기
    groupCfg := c.store.Get(gvk.WorkloadGroup, proxy.Metadata.AutoRegisterGroup, ...)
    if groupCfg == nil {
        return grpcstatus.Errorf(codes.FailedPrecondition,
            "cannot find WorkloadGroup %s/%s", ...)
    }

    // WorkloadGroup 템플릿 + 프록시 메타데이터로 WorkloadEntry 생성
    entry := workloadEntryFromGroup(entryName, proxy, groupCfg)
    setConnectMeta(entry, c.instanceID, conTime)
    _, err := c.store.Create(*entry)

    autoRegistrationSuccess.Increment()
    return err
}
```

#### 3.3.3 WorkloadEntry 생성 시 데이터 병합

```go
// pilot/pkg/autoregistration/controller.go

func workloadEntryFromGroup(name string, proxy *model.Proxy,
    groupCfg *config.Config) *config.Config {

    group := groupCfg.Spec.(*v1alpha3.WorkloadGroup)
    entry := group.Template.DeepCopy()
    entry.Address = proxy.IPAddresses[0]

    // 레이블 병합 우선순위: proxy.Metadata > WorkloadGroup.Metadata > Template
    if group.Metadata != nil && group.Metadata.Labels != nil {
        entry.Labels = mergeLabels(entry.Labels, group.Metadata.Labels)
    }
    if proxy.Metadata.Labels != nil {
        entry.Labels = mergeLabels(entry.Labels, proxy.Metadata.Labels)
        delete(entry.Labels, pm.LocalityLabel)  // locality는 별도 필드 사용
    }

    // Network와 Locality 설정
    if proxy.Metadata.Network != "" {
        entry.Network = string(proxy.Metadata.Network)
    }
    if proxy.XdsNode != nil && proxy.XdsNode.Locality != nil {
        entry.Locality = util.LocalityToString(proxy.XdsNode.Locality)
    }

    // OwnerReference: WorkloadGroup이 삭제되면 WLE도 삭제
    return &config.Config{
        Meta: config.Meta{
            OwnerReferences: []metav1.OwnerReference{{
                APIVersion: groupCfg.GroupVersionKind.GroupVersion(),
                Kind:       groupCfg.GroupVersionKind.Kind,
                Name:       groupCfg.Name,
                UID:        kubetypes.UID(groupCfg.UID),
                Controller: &workloadGroupIsController,  // true
            }},
        },
        Spec: entry,
    }
}
```

### 3.4 연결 해제 처리

#### 3.4.1 OnDisconnect 흐름

```go
func (c *Controller) OnDisconnect(conn connection) {
    proxy := conn.Proxy()
    entryName, autoCreate := proxy.WorkloadEntry()
    if entryName == "" { return }

    // 아직 다른 연결이 남아있으면 정리하지 않음
    if remainingConnections := c.adsConnections.Disconnect(conn); remainingConnections {
        return
    }

    // 연결 해제 작업 큐에 추가
    c.queue.Add(&workItem{
        entryName:   entryName,
        autoCreated: autoCreate,
        proxy:       conn.Proxy(),
        disConTime:  time.Now(),
        origConTime: conn.ConnectedAt(),
    })
}
```

**왜 즉시 삭제하지 않는가?**
1. 프록시가 **여러 연결**을 가질 수 있다 (하나만 끊겨도 다른 연결이 살아 있음)
2. 네트워크 불안정으로 인한 **일시적 끊김** 후 재접속 가능
3. 큐 기반 처리로 **순서 보장** 및 **재시도** 가능 (maxRetries=5)

#### 3.4.2 연결 해제 상태 업데이트

```go
func (c *Controller) changeWorkloadEntryStateToDisconnected(
    entryName string, proxy *model.Proxy,
    disconTime, origConnTime time.Time) (bool, error) {

    cfg := c.store.Get(gvk.WorkloadEntry, entryName, proxy.Metadata.Namespace)
    if cfg == nil { return false, error }

    // 연결 시간 검증: 이 연결 해제가 마지막 연결 이후인지 확인
    if mostRecentConn, err := time.Parse(timeFormat,
        cfg.Annotations["istio.io/connectedAt"]); err == nil {
        if mostRecentConn.After(origConnTime) {
            return false, nil  // 이미 재접속됨
        }
    }

    // 다른 istiod가 이미 인수했는지 확인
    if cfg.Annotations["istio.io/workloadController"] != c.instanceID {
        return false, nil
    }

    wle := cfg.DeepCopy()
    delete(wle.Annotations, "istio.io/connectedAt")
    wle.Annotations["istio.io/disconnectedAt"] = disconTime.Format(timeFormat)
    _, err := c.store.Update(wle)
    return true, err
}
```

#### 3.4.3 유예 기간 후 정리

```go
func (c *Controller) unregisterWorkload(item any) error {
    workItem := item.(*workItem)

    changed, err := c.changeWorkloadEntryStateToDisconnected(
        workItem.entryName, workItem.proxy,
        workItem.disConTime, workItem.origConTime)
    if !changed { return nil }

    // 유예 기간(WorkloadEntryCleanupGracePeriod) 후 정리 예약
    c.cleanupQueue.PushDelayed(func() error {
        wle := c.store.Get(gvk.WorkloadEntry, workItem.entryName, ns)
        if wle != nil && c.shouldCleanupEntry(*wle) {
            c.cleanupEntry(*wle, false)
        }
        return nil
    }, features.WorkloadEntryCleanupGracePeriod)
    return nil
}
```

### 3.5 정리 판단 로직

```go
func (c *Controller) shouldCleanupEntry(wle config.Config) bool {
    // 자동 등록도 헬스체크도 아니면 정리 대상 아님
    if !isAutoRegisteredWorkloadEntry(&wle) &&
       !(isHealthCheckedWorkloadEntry(&wle) && health.HasHealthCondition(&wle)) {
        return false
    }

    // connectedAt이 있으면 아직 연결된 것
    connTime := wle.Annotations["istio.io/connectedAt"]
    if connTime != "" {
        connAt, _ := time.Parse(timeFormat, connTime)
        // maxConnectionAge 초과 시 연결 누수로 판단
        if time.Since(connAt) > c.maxConnectionAge {
            return true  // 누수 정리
        }
        return false  // 정상 연결
    }

    // disconnectedAt 이후 유예 기간 경과 확인
    disconnTime := wle.Annotations["istio.io/disconnectedAt"]
    if disconnTime == "" { return false }
    disconnAt, _ := time.Parse(timeFormat, disconnTime)
    return time.Since(disconnAt) >= features.WorkloadEntryCleanupGracePeriod
}
```

**정리 동작**:

| WLE 유형 | 정리 방식 |
|---------|---------|
| 자동 등록 (autoRegistered) | **삭제** (store.Delete) |
| 수동 등록 + 헬스체크 | 헬스 Condition만 제거 (WLE 유지) |

### 3.6 ADS 연결 추적

```go
// pilot/pkg/autoregistration/connections.go

type adsConnections struct {
    sync.Mutex
    byProxy map[proxyKey]map[string]connection  // proxyKey → connID → connection
}

type proxyKey struct {
    Network   string
    IP        string
    GroupName string
    Namespace string
}

func (m *adsConnections) Connect(conn connection) {
    k := makeProxyKey(conn.Proxy())
    connections := m.byProxy[k]
    if connections == nil {
        connections = make(map[string]connection)
        m.byProxy[k] = connections
    }
    connections[conn.ID()] = conn  // 같은 프록시의 여러 연결 추적
}

func (m *adsConnections) Disconnect(conn connection) bool {
    k := makeProxyKey(conn.Proxy())
    connections := m.byProxy[k]
    delete(connections, conn.ID())
    if len(connections) == 0 {
        delete(m.byProxy, k)
        return false  // 더 이상 연결 없음 → 정리 진행
    }
    return true  // 아직 다른 연결 남아있음 → 정리 보류
}
```

**왜 다중 연결을 지원하는가?** 프록시가 재접속할 때 이전 연결이 아직 정리되지 않은 경우가 있다. 이를 올바르게 추적하지 않으면 조기 정리로 인한 서비스 중단이 발생한다.

### 3.7 주기적 정리 (Periodic Cleanup)

```go
func (c *Controller) periodicWorkloadEntryCleanup(stopCh <-chan struct{}) {
    ticker := time.NewTicker(10 * features.WorkloadEntryCleanupGracePeriod)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            wles := c.store.List(gvk.WorkloadEntry, metav1.NamespaceAll)
            for _, wle := range wles {
                if c.shouldCleanupEntry(wle) {
                    c.cleanupQueue.Push(func() error {
                        c.cleanupEntry(wle, true)
                        return nil
                    })
                }
            }
        case <-stopCh:
            return
        }
    }
}
```

**왜 주기적 정리가 필요한가?** istiod가 재시작되면 메모리 내 연결 추적 정보가 사라진다. 이때 `disconnectedAt` 어노테이션이 설정되었지만 정리되지 않은 WLE가 남을 수 있다. 주기적 정리가 이런 고아(orphan) WLE를 처리한다.

---

## 4. ID 검증과 보안

### 4.1 프록시 신원 확인

```go
func ensureProxyCanControlEntry(proxy *model.Proxy, wle *config.Config) error {
    if !features.ValidateWorkloadEntryIdentity {
        return nil  // 검증 비활성화
    }
    if proxy.VerifiedIdentity == nil {
        return fmt.Errorf("registration requires a verified identity")
    }
    if proxy.VerifiedIdentity.Namespace != wle.Namespace {
        return fmt.Errorf("namespace mismatch: %q vs %q", ...)
    }
    spec := wle.Spec.(*v1alpha3.WorkloadEntry)
    if spec.ServiceAccount != "" &&
       proxy.VerifiedIdentity.ServiceAccount != spec.ServiceAccount {
        return fmt.Errorf("service account mismatch: %q vs %q", ...)
    }
    return nil
}
```

| 검증 항목 | 검증 내용 | 실패 시 |
|---------|---------|--------|
| VerifiedIdentity | mTLS 인증서에서 추출한 신원 존재 여부 | 등록 거부 |
| Namespace | 프록시 신원의 네임스페이스 == WLE 네임스페이스 | 등록 거부 |
| ServiceAccount | WLE에 SA가 지정된 경우 프록시 SA와 일치 여부 | 등록 거부 |

### 4.2 연결 타임스탬프 기반 경합 방지

```
시나리오: istiod-1과 istiod-2가 동시에 WLE를 관리하려 할 때

시간 →  t1        t2        t3
istiod-1: Connect   ---(처리중)---  DisconnectAt 설정
istiod-2:           Connect        connectedAt > t1 확인 → istiod-1의 disconnect 무시
```

어노테이션 기반 분산 잠금:
- `istio.io/connectedAt`: 마지막 연결 시간
- `istio.io/disconnectedAt`: 마지막 연결 해제 시간
- `istio.io/workloadController`: 현재 관리 중인 istiod 인스턴스 ID

---

## 5. Late Registration: WorkloadGroup 후행 생성

```go
func (c *Controller) setupAutoRecreate() {
    c.lateRegistrationQueue = controllers.NewQueue("auto-register existing connections",
        controllers.WithReconciler(func(key types.NamespacedName) error {
            if c.store.Get(gvk.WorkloadGroup, key.Name, key.Namespace) == nil {
                return nil  // WG 삭제됨, 무시
            }
            conns := c.adsConnections.ConnectionsForGroup(key)
            for _, conn := range conns {
                proxy := conn.Proxy()
                entryName := autoregisteredWorkloadEntryName(proxy)
                if entryName == "" { continue }
                c.registerWorkload(entryName, proxy, conn.ConnectedAt())
                proxy.SetWorkloadEntry(entryName, true)
            }
            return nil
        }))

    c.store.RegisterEventHandler(gvk.WorkloadGroup,
        func(_, cfg config.Config, event model.Event) {
            if event == model.EventAdd {
                c.lateRegistrationQueue.Add(cfg.NamespacedName())
            }
        })
}
```

**시나리오**: VM 프록시가 먼저 연결된 후 WorkloadGroup이 나중에 생성되는 경우
1. VM 프록시 연결 → AutoRegisterGroup 설정되어 있으나 WG가 없어 등록 실패
2. 관리자가 WorkloadGroup 생성 → `EventAdd` 이벤트 발생
3. `lateRegistrationQueue`가 해당 WG에 해당하는 모든 기존 연결을 조회
4. 각 연결에 대해 `registerWorkload` 호출하여 WLE 생성

---

## 6. Ingress 변환 예시

### 6.1 Ingress → Gateway + VirtualService

입력 Ingress:
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: my-app
  namespace: default
  annotations:
    kubernetes.io/ingress.class: "istio"
spec:
  tls:
  - hosts: ["app.example.com"]
    secretName: app-tls
  rules:
  - host: app.example.com
    http:
      paths:
      - path: /api
        pathType: Prefix
        backend:
          service:
            name: api-svc
            port:
              number: 8080
      - path: /api/v1/users
        pathType: Exact
        backend:
          service:
            name: users-svc
            port:
              number: 8080
```

생성되는 Istio Gateway:
```yaml
apiVersion: networking.istio.io/v1alpha3
kind: Gateway
metadata:
  name: my-app-istio-autogenerated-k8s-ingress-default
  namespace: istio-system
spec:
  selector:
    istio: ingressgateway
  servers:
  - port: {number: 443, protocol: HTTPS, name: https-443-ingress-my-app-default-0}
    hosts: ["app.example.com"]
    tls: {mode: SIMPLE, credentialName: app-tls}
  - port: {number: 80, protocol: HTTP, name: http-80-ingress-my-app-default}
    hosts: ["*"]
```

생성되는 VirtualService:
```yaml
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: app-example-com-my-app-istio-autogenerated-k8s-ingress
  namespace: default
  annotations:
    internal.istio.io/route-semantics: ingress
spec:
  hosts: ["app.example.com"]
  gateways: ["istio-system/my-app-istio-autogenerated-k8s-ingress-default"]
  http:
  - match:                                    # Exact 14자: 최우선
    - uri: {exact: "/api/v1/users"}
    route:
    - destination: {host: users-svc.default.svc.cluster.local, port: {number: 8080}}
      weight: 100
  - match:                                    # Prefix 4자: 차순위
    - uri: {prefix: "/api"}
    route:
    - destination: {host: api-svc.default.svc.cluster.local, port: {number: 8080}}
      weight: 100
```

---

## 7. 모니터링 메트릭

### 7.1 AutoRegistration 메트릭

```go
// pilot/pkg/autoregistration/controller.go

auto_registration_success_total       // 성공적 자동 등록 수
auto_registration_updates_total       // 기존 WLE 갱신 수
auto_registration_unregister_total    // 연결 해제 수
auto_registration_deletes_total       // WLE 삭제 수 (주기적 + 즉시)
auto_registration_errors_total        // 오류 발생 수
```

### 7.2 핵심 알림 규칙

```yaml
# 자동 등록 실패율 증가
- alert: IstioAutoRegistrationErrors
  expr: rate(auto_registration_errors_total[5m]) > 0.1
  for: 5m
  annotations:
    summary: "VM 자동 등록 오류율 증가"

# 정리되지 않은 고아 WLE
- alert: IstioOrphanWorkloadEntries
  expr: |
    count(kube_customresource_annotations{
      customresource_kind="WorkloadEntry",
      annotation_istio_io_disconnectedAt!=""
    }) > 10
  for: 30m
```

---

## 8. 운영 가이드

### 8.1 Ingress 호환 모드 설정

```yaml
# MeshConfig
meshConfig:
  ingressControllerMode: DEFAULT    # OFF, STRICT, DEFAULT
  ingressClass: "istio"             # Ingress 클래스 이름
  ingressService: "istio-ingressgateway"
  ingressSelector: ""               # 비어있으면 ingressService로 추론
```

| 모드 | 사용 시나리오 |
|------|------------|
| OFF | Istio Ingress 호환 비활성화 (Gateway API만 사용) |
| STRICT | 명시적으로 `istio` 클래스가 지정된 Ingress만 처리 |
| DEFAULT | 클래스 미지정 Ingress도 Istio가 처리 (마이그레이션 시) |

### 8.2 WorkloadEntry 자동 등록 설정

```yaml
# istiod 환경변수
PILOT_ENABLE_WORKLOAD_ENTRY_AUTOREGISTRATION: "true"
PILOT_ENABLE_WORKLOAD_ENTRY_HEALTHCHECKS: "true"
PILOT_WORKLOAD_ENTRY_CLEANUP_GRACE_PERIOD: "10m"
```

### 8.3 문제 해결

| 증상 | 원인 | 해결 |
|------|------|------|
| Ingress에 IP가 안 나옴 | ingressService 설정 오류 | `meshConfig.ingressService` 확인 |
| VM이 서비스 메시에 안 나옴 | WG 미생성 / 네임스페이스 불일치 | WorkloadGroup CRD 확인 |
| WLE가 계속 남아 있음 | cleanupGracePeriod가 너무 길거나 istiod 재시작 | 주기적 정리 주기 확인 |
| 403 등록 거부 | SA 불일치 | proxy의 ServiceAccount와 WLE SA 확인 |
| 연결 해제 후 즉시 삭제됨 | gracePeriod=0 | PILOT_WORKLOAD_ENTRY_CLEANUP_GRACE_PERIOD 증가 |

---

## 9. 설계 결정 분석

### 9.1 왜 Ingress를 직접 변환하는가?

**대안**: Ingress 컨트롤러를 별도 프로세스로 구현
**선택**: istiod 내장 변환 파이프라인

| 기준 | 별도 프로세스 | istiod 내장 |
|------|------------|-----------|
| 배포 복잡도 | 추가 컴포넌트 | 추가 없음 |
| 일관성 | xDS Push 지연 가능 | 즉각 반영 |
| 리소스 | 별도 메모리/CPU | istiod 공유 |
| 장애 도메인 | 별도 | istiod와 동일 |

**결론**: Ingress는 Kubernetes 네이티브 사용자를 위한 **호환성 계층**이므로 가능한 투명하게 통합하는 것이 중요하다.

### 9.2 왜 어노테이션 기반 분산 잠금인가?

**대안**: Kubernetes Lease 기반 분산 잠금
**선택**: CRD 어노테이션 기반 낙관적 동시성 제어

이유:
1. **WLE별 세밀한 제어**: Lease는 전역적이지만, 어노테이션은 WLE 개별 제어 가능
2. **추가 리소스 불필요**: 별도 Lease 오브젝트 생성 없이 WLE 자체에 상태 저장
3. **Kubernetes 낙관적 동시성**: ResourceVersion 기반 Update 충돌 시 자동 재시도

### 9.3 왜 krt를 사용하는가?

krt는 Istio의 반응형 프로그래밍 프레임워크로, 입력 리소스 변경 시 파생 리소스를 자동으로 갱신한다:

```
전통적 접근: Informer → EventHandler → 수동 캐시 관리 → 수동 Push
krt 접근:    Collection → Map/Filter/Index → 자동 갱신 → 자동 Push
```

Ingress 컨트롤러에서 krt의 이점:
- Ingress 변경 → VirtualService/Gateway **자동** 갱신
- Service 포트 변경 → 참조하는 VirtualService **자동** 갱신
- MeshConfig 변경 → 필터링/셀렉터 **자동** 재계산

---

## 10. 전체 아키텍처 요약

```
┌──────────────── Ingress 호환 계층 ────────────────┐
│                                                    │
│  K8s Ingress ─→ SupportedIngresses                 │
│                    │                               │
│                    ├─→ RuleCollection (호스트 인덱스) │
│                    │      │                        │
│                    │      └─→ VirtualService        │
│                    │                               │
│                    └─→ Gateway                     │
│                                                    │
│  [읽기 전용] Ingress → [자동 생성] Gateway + VS      │
└────────────────────────────────────────────────────┘

┌────────── WorkloadEntry 자동 등록 ──────────────────┐
│                                                     │
│  VM Proxy ──OnConnect──→ registerWorkload            │
│    │                        │                       │
│    │                        ├─ WLE 존재: 상태 갱신    │
│    │                        └─ WLE 없음: WG → WLE 생성│
│    │                                                │
│    └──OnDisconnect──→ unregisterWorkload             │
│                          │                          │
│                          ├─ disconnectedAt 설정      │
│                          └─ Grace Period 후 cleanupEntry│
│                              ├─ 자동등록: WLE 삭제    │
│                              └─ 헬스체크: Condition 제거│
│                                                     │
│  주기적 정리: 10 × gracePeriod 간격으로 고아 WLE 스캔    │
└─────────────────────────────────────────────────────┘
```

---

## 참조 소스 파일

| 파일 | 역할 |
|------|------|
| `pilot/pkg/config/kube/ingress/controller.go` | Ingress 컨트롤러 메인 |
| `pilot/pkg/config/kube/ingress/ingress.go` | SupportedIngresses, RuleCollection |
| `pilot/pkg/config/kube/ingress/virtualservices.go` | VirtualService 변환 |
| `pilot/pkg/config/kube/ingress/gateways.go` | Gateway 변환 |
| `pilot/pkg/autoregistration/controller.go` | AutoRegistration 컨트롤러 메인 |
| `pilot/pkg/autoregistration/connections.go` | ADS 연결 추적 |
| `pilot/pkg/autoregistration/internal/health/` | 헬스체크 컨트롤러 |
| `pilot/pkg/autoregistration/internal/state/` | 상태 저장소 |
