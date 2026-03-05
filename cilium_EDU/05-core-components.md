# 05. Cilium 핵심 컴포넌트

## 개요

Cilium Agent의 핵심 컴포넌트 5개를 소스코드 기반으로 분석한다:
1. **Daemon** - 에이전트 전체 관리
2. **Endpoint Manager** - 엔드포인트 CRUD, GC, 동기화
3. **Datapath Loader** - BPF 프로그램 컴파일/로드
4. **Policy Repository** - 정책 CRUD, Revision 관리, SelectorCache
5. **BPF Maps** - 데이터플레인 상태 저장소 (34종)

## 1. Daemon (에이전트 핵심)

### 파일 위치
- `daemon/cmd/daemon.go`: 설정 검증 및 초기화
- `daemon/cmd/cells.go`: Agent 셀 모듈 정의

### 역할

Daemon은 cilium-agent의 중심이다. Hive 셀 시스템에서 `daemonCell`로 래핑되어
레거시 초기화 로직을 관리한다. 핵심 책임:

- 설정 검증 (`initAndValidateDaemonConfig()`)
- kube-proxy 대체 초기화
- WireGuard/IPsec 상호 배제 검증
- BPF 맵 생성 전제조건 확인

### initAndValidateDaemonConfig()

```go
// daemon/cmd/daemon.go:29
func initAndValidateDaemonConfig(params daemonConfigParams) error {
    // 1. WireGuard와 IPSec 상호 배제 검증
    if params.IPSecConfig.Enabled() && params.WireguardConfig.Enabled() {
        return fmt.Errorf("WireGuard cannot be used with IPsec")
    }

    // 2. IPSec + DNS 프록시 투명 모드 필수
    if params.IPSecConfig.Enabled() && params.DaemonConfig.EnableL7Proxy &&
       !params.DaemonConfig.DNSProxyEnableTransparentMode {
        return fmt.Errorf("IPSec requires DNS proxy transparent mode")
    }

    // 3. kube-proxy 대체 초기화
    if err := params.KPRInitializer.InitKubeProxyReplacementOptions(); err != nil {
        return fmt.Errorf("unable to initialize kube-proxy replacement: %w", err)
    }

    // 4. NodePort/BPF masquerade는 iptables 규칙 필요
    if (params.KPRConfig.KubeProxyReplacement ||
        params.DaemonConfig.EnableBPFMasquerade) &&
       !params.DaemonConfig.InstallIptRules {
        return fmt.Errorf("requires iptables rules")
    }

    // 5. K8s 모드에서 localhost 정책 자동 설정
    if params.K8sClientConfig.IsEnabled() {
        if params.DaemonConfig.AllowLocalhost == option.AllowLocalhostAuto {
            params.DaemonConfig.AllowLocalhost = option.AllowLocalhostAlways
        }
    }
    // ... 추가 검증
}
```

### 왜 이런 검증이 필요한가?

Cilium은 수많은 기능 조합을 지원하며, 일부 조합은 상호 충돌한다.
시작 시점에서 잘못된 설정을 조기 감지하여 런타임 오류를 방지한다.
예를 들어:
- WireGuard와 IPsec는 둘 다 노드 간 암호화를 제공하므로 동시 사용 불가
- BPF masquerade는 로컬 생성 트래픽을 식별하기 위해 iptables 마킹이 필요

## 2. Endpoint Manager

### 파일 위치
- `pkg/endpointmanager/manager.go`: 핵심 구현
- `pkg/endpointmanager/cell.go`: EndpointManager 인터페이스 정의

### 구조체

```go
// pkg/endpointmanager/manager.go:51
type endpointManager struct {
    logger *slog.Logger
    health cell.Health

    mutex lock.RWMutex

    // 엔드포인트 저장소 (ID -> *Endpoint)
    endpoints    map[uint16]*endpoint.Endpoint
    endpointsAux map[string]*endpoint.Endpoint  // 보조 인덱스

    // IPv6 멀티캐스트 그룹 관리
    mcastManager *mcastmanager.MCastManager

    // K8s CiliumEndpoint 동기화
    EndpointResourceSynchronizer

    // 이벤트 구독자
    subscribers map[Subscriber]struct{}

    // GC용 헬스 체크 함수
    checkHealth EndpointCheckerFunc

    // 삭제 함수 (테스트 모킹용 추상화)
    deleteEndpoint endpointDeleteFunc

    // Mark-and-Sweep GC 대상
    markedEndpoints []uint16

    // 컨트롤러 매니저
    controllers *controller.Manager

    // BPF 정책 맵 압력 모니터
    policyMapPressure *policyMapPressure

    // 로컬 노드 정보
    localNodeStore *node.LocalNodeStore

    // 엔드포인트 ID 할당기
    epIDAllocator *epIDAllocator

    // 모니터 에이전트
    monitorAgent monitoragent.Agent
}
```

### EndpointManager 인터페이스

```go
// pkg/endpointmanager/cell.go:124
type EndpointManager interface {
    EndpointsLookup   // Endpoint 조회
    EndpointsModify   // Endpoint 추가/삭제

    Subscribe(s Subscriber)     // 이벤트 구독
    Unsubscribe(s Subscriber)   // 이벤트 구독 해제

    // 모든 엔드포인트의 PolicyMap 업데이트
    UpdatePolicyMaps(ctx context.Context, notifyWg *sync.WaitGroup) *sync.WaitGroup

    // 모든 엔드포인트 재생성
    RegenerateAllEndpoints(regenMetadata *regeneration.ExternalRegenerationMetadata) *sync.WaitGroup

    // 배치 재생성 트리거 (즉시 반환)
    TriggerRegenerateAllEndpoints()

    // 옵션 일괄 적용
    OverrideEndpointOpts(om option.OptionMap)

    // 호스트 엔드포인트 라벨 초기화
    InitHostEndpointLabels(ctx context.Context)

    // 정책 업데이트 (영향받는 Identity에 대해)
    UpdatePolicy(idsToRegen *set.Set[identity.NumericIdentity], fromRev, toRev uint64)
}
```

### Endpoint GC (Mark-and-Sweep)

```go
// pkg/endpointmanager/manager.go:132
func (mgr *endpointManager) WithPeriodicEndpointGC(
    ctx context.Context,
    checkHealth EndpointCheckerFunc,
    interval time.Duration,
) *endpointManager {
    mgr.checkHealth = checkHealth
    mgr.controllers.UpdateController("endpoint-gc",
        controller.ControllerParams{
            Group:       endpointGCControllerGroup,
            DoFunc:      mgr.markAndSweep,    // mark-and-sweep GC
            RunInterval: interval,
            Context:     ctx,
        })
    return mgr
}
```

GC는 두 단계로 동작한다:
1. **Mark**: 비정상 엔드포인트를 `markedEndpoints`에 추가
2. **Sweep**: 다음 주기에 여전히 비정상인 엔드포인트 삭제

이 2단계 접근법은 일시적 장애로 인한 오삭제를 방지한다.

### 주기적 Regeneration

```go
// pkg/endpointmanager/manager.go:147
func (mgr *endpointManager) WithPeriodicEndpointRegeneration(
    ctx context.Context,
    interval time.Duration,
) *endpointManager {
    mgr.controllers.UpdateController(regenEndpointControllerName,
        controller.ControllerParams{
            DoFunc: func(ctx context.Context) error {
                wg := mgr.RegenerateAllEndpoints(
                    &regeneration.ExternalRegenerationMetadata{
                        Reason:            "periodic endpoint regeneration",
                        RegenerationLevel: regeneration.RegenerateWithoutDatapath,
                    })
                // ... 완료 대기
            },
            RunInterval: interval,
        })
    return mgr
}
```

`RegenerateWithoutDatapath` 수준은 BPF 프로그램을 재컴파일하지 않고
정책/설정만 재계산한다. 데이터플레인 변경이 필요한 경우에만
`RegenerateWithDatapath`를 사용한다.

## 3. Datapath Loader

### 파일 위치
- `pkg/datapath/loader/loader.go`: loader 구조체
- `pkg/datapath/loader/compile.go`: BPF 컴파일
- `pkg/datapath/loader/tc.go`: TC/TCX 프로그램 어태치

### 구조체

```go
// pkg/datapath/loader/loader.go:37
type loader struct {
    logger *slog.Logger

    // 컴파일된 BPF 오브젝트 캐시
    templateCache *objectCache

    // BPF 맵 레지스트리
    registry *registry.MapRegistry

    // 호스트 데이터플레인 초기화 완료 시그널
    hostDpInitializedOnce sync.Once
    hostDpInitialized     chan struct{}

    // 시스템 설정
    sysctl          sysctl.Sysctl
    prefilter       datapath.PreFilter
    compilationLock datapath.CompilationLock
    configWriter    datapath.ConfigWriter

    // StateDB
    db      *statedb.DB
    devices statedb.Table[*tables.Device]
}
```

### BPF 컴파일 프로세스

```go
// pkg/datapath/loader/compile.go
const (
    compiler = "clang"

    endpointPrefix = "bpf_lxc"
    endpointProg   = "bpf_lxc.c"
    endpointObj    = "bpf_lxc.o"

    hostEndpointPrefix = "bpf_host"
    hostEndpointProg   = "bpf_host.c"
    hostEndpointObj    = "bpf_host.o"

    xdpPrefix     = "bpf_xdp"
    overlayPrefix = "bpf_overlay"
    wireguardPrefix = "bpf_wireguard"
)
```

컴파일 파이프라인:

```
1. 헤더 생성 (ep_config.h)
   └── 엔드포인트별 설정 (#define LXC_ID, SECLABEL, ...)

2. clang 컴파일
   └── bpf_lxc.c + ep_config.h + lib/*.h → bpf_lxc.o

3. 오브젝트 캐시 확인
   └── templateCache: 같은 설정의 오브젝트 재사용

4. 프로그램 로드
   └── cilium/ebpf 라이브러리로 커널에 로드
```

### TC/TCX 어태치

```go
// pkg/datapath/loader/tc.go:27
func attachSKBProgram(
    logger *slog.Logger,
    device netlink.Link,
    prog *ebpf.Program,
    progName, bpffsDir string,
    parent uint32,
    tcxEnabled bool,
) error {
    if tcxEnabled {
        // netkit 디바이스면 netkit 링크 사용
        if device.Type() == "netkit" {
            return upsertNetkitProgram(...)
        }
        // 그 외 tcx 사용 (레거시 tc 대체)
        err := upsertTCXProgram(...)
        if err == nil {
            // tcx 성공 시 레거시 tc 정리
            removeTCFilters(device, parent)
            return nil
        }
        // tcx 미지원 시 폴백
        if !errors.Is(err, link.ErrNotSupported) {
            return err
        }
    }
    // 레거시 TC 사용
    return upsertTCFilter(...)
}
```

### 왜 TCX를 사용하는가?

TCX(TC eXpress)는 Linux 6.6+에서 도입된 새로운 BPF 어태치 포인트이다.
기존 TC(Traffic Control) 대비 장점:
- **원자적 교체**: 프로그램을 중단 없이 교체 가능
- **BPF 링크 기반**: bpffs에 핀닝하여 프로그램 수명 관리 용이
- **netkit 통합**: veth 대체 인터페이스인 netkit과 자연스럽게 통합

## 4. Policy Repository

### 파일 위치
- `pkg/policy/repository.go`: Repository 구조체 및 인터페이스

### 구조체

```go
// pkg/policy/repository.go:63
type Repository struct {
    logger *slog.Logger
    mutex  lock.RWMutex

    // 정책 규칙 저장소
    rules            map[ruleKey]*rule
    rulesByNamespace map[string]sets.Set[ruleKey]
    rulesByResource  map[ipcachetypes.ResourceID]map[ruleKey]*rule

    nextID uint  // 테스트용 규칙 키 생성기

    // 리비전: 정책 변경 시마다 증가 (항상 >0)
    revision atomic.Uint64

    // SelectorCache: 정책 셀렉터 → Identity 매핑 캐시
    selectorCache *SelectorCache

    // Subject SelectorCache: 정책이 적용되는 대상 셀렉터
    subjectSelectorCache *SelectorCache

    // 계산된 SelectorPolicy 캐시
    policyCache *policyCache

    certManager    certificatemanager.CertificateManager
    metricsManager types.PolicyMetrics
}
```

### 핵심 인터페이스

```go
// pkg/policy/repository.go:34
type PolicyRepository interface {
    // 리비전 증가
    BumpRevision() uint64

    // 인증 타입 조회
    GetAuthTypes(localID, remoteID identity.NumericIdentity) AuthTypes

    // Identity에 대한 SelectorPolicy 계산
    // skipRevision >= 현재 계산 버전이면 nil 반환 (불필요한 재계산 방지)
    GetSelectorPolicy(id *identity.Identity, skipRevision uint64, ...) (SelectorPolicy, uint64, error)

    // 정책 스냅샷
    GetPolicySnapshot() map[identity.NumericIdentity]SelectorPolicy

    // 현재 리비전 조회
    GetRevision() uint64

    // 리소스 기반 정책 교체 (K8s CRD 업데이트 시)
    ReplaceByResource(rules types.PolicyEntries, resource ipcachetypes.ResourceID) (
        affectedIDs *set.Set[identity.NumericIdentity], rev uint64, oldRevCnt int)
}
```

### 정책 업데이트 흐름

```
K8s CiliumNetworkPolicy 변경
    │
    ▼
policyK8s.Cell (pkg/policy/k8s/)
    │ CRD → types.PolicyEntries 변환
    ▼
Repository.ReplaceByResource(rules, resource)
    │
    ├── 기존 규칙 제거 (rulesByResource[resource])
    ├── 새 규칙 추가
    ├── revision++ (atomic)
    ├── SelectorCache 업데이트
    │   └── 영향받는 Identity 집합 계산
    └── return affectedIDs
    │
    ▼
EndpointManager.UpdatePolicy(affectedIDs, fromRev, toRev)
    │
    ├── 영향받는 Endpoint: Regeneration 트리거
    │   └── GetSelectorPolicy() → BPF policymap 업데이트
    └── 비영향 Endpoint: policyRevision만 bump
```

### SelectorCache의 역할과 구조

SelectorCache는 정책 규칙의 라벨 셀렉터를 미리 평가하여
어떤 Identity가 어떤 셀렉터에 매칭되는지를 캐싱한다.

```
정책: "from: app=frontend to: app=backend allow TCP/80"
    │
    ▼
SelectorCache 엔트리:
    셀렉터 "app=frontend" → {ID:1001, ID:1002, ID:1003}
    셀렉터 "app=backend"  → {ID:2001, ID:2002}
```

새 Identity가 추가되거나 기존 Identity의 라벨이 변경되면
SelectorCache가 업데이트되고, 영향받는 엔드포인트의 BPF policymap이
갱신된다.

## 5. BPF Maps (34종)

### 파일 위치
- `pkg/maps/`: 각 맵 타입별 하위 디렉토리

### 맵 종류와 역할

#### 핵심 맵 (데이터플레인 필수)

| 맵 이름 | 패키지 | 타입 | 용도 |
|---------|--------|------|------|
| `cilium_lxc` | `lxcmap/` | Hash | 엔드포인트 정보 (ID, MAC, ifIndex) |
| `cilium_ipcache` | `ipcache/` | LPM Trie | IP → Identity 매핑 |
| `cilium_policy_v2_*` | `policymap/` | Hash (per-EP) | 정책 허용/거부 (Identity+Port+Dir) |
| `cilium_ct4_global` | `ctmap/` | LRU Hash | IPv4 ConnTrack 테이블 |
| `cilium_ct6_global` | `ctmap/` | LRU Hash | IPv6 ConnTrack 테이블 |
| `cilium_snat_v4_external` | `nat/` | LRU Hash | IPv4 SNAT 매핑 |
| `cilium_snat_v6_external` | `nat/` | LRU Hash | IPv6 SNAT 매핑 |

#### 로드밸런싱 맵

| 맵 이름 | 타입 | 용도 |
|---------|------|------|
| `cilium_lb4_services_v2` | Hash | IPv4 서비스 프론트엔드 |
| `cilium_lb4_backends_v3` | Hash | IPv4 서비스 백엔드 |
| `cilium_lb4_reverse_nat` | Hash | IPv4 역방향 NAT |
| `cilium_lb4_source_range` | LPM Trie | 소스 IP 범위 제한 |
| `cilium_lb4_maglev` | Array | Maglev 해시 테이블 |

#### 인프라 맵

| 맵 이름 | 패키지 | 용도 |
|---------|--------|------|
| `cilium_call_policy` | `callsmap/` | 정책 프로그램 Tail Call |
| `cilium_signals` | `signalmap/` | BPF → 사용자 공간 시그널 |
| `cilium_metrics` | `metricsmap/` | 데이터플레인 메트릭 |
| `cilium_config` | `configmap/` | 런타임 설정 |
| `cilium_encrypt_state` | `encrypt/` | 암호화 상태 |
| `cilium_node_map_v2` | `nodemap/` | 노드 IP 매핑 |
| `cilium_tunnel_map` | | 터널 엔드포인트 |
| `cilium_bandwidth` | `bwmap/` | 대역폭 제한 (EDT) |

### PolicyMap 상세

```go
// pkg/maps/policymap/policymap.go
const (
    // 정책 맵 이름 접두사 (엔드포인트별 생성)
    MapName = "cilium_policy_v2_"

    // Tail Call 맵 이름
    PolicyCallMapName       = "cilium_call_policy"
    PolicyEgressCallMapName = "cilium_egresscall_policy"

    // 최대 엔트리 (엔드포인트 ID 최대값)
    PolicyCallMaxEntries = ^uint16(0)  // 65535

    // 모든 포트 허용 (포트 필드 0)
    AllPorts = uint16(0)
)
```

PolicyMap 키 구조:
```
키: (Identity, DestPort, Proto, TrafficDirection)
    │
    ├── Identity: 소스/대상 보안 Identity (32bit)
    ├── DestPort: 대상 포트 (16bit), 0이면 전체 포트
    ├── Proto: 프로토콜 (TCP/UDP/ICMP/Any)
    └── TrafficDirection: Ingress/Egress

값: (Flags, ProxyPort, AuthType)
    │
    ├── Flags: Allow/Deny
    ├── ProxyPort: L7 프록시 리다이렉트 포트 (0이면 직접 통과)
    └── AuthType: 인증 요구 사항
```

### 맵 간 상호작용

```
패킷 도착 (bpf_lxc.c)
    │
    ├── 1. cilium_ipcache 조회
    │   └── 소스 IP → 소스 Identity 확인
    │
    ├── 2. cilium_ct4_global 조회
    │   └── 기존 연결이면 → CT_ESTABLISHED 반환 → 정책 스킵
    │   └── 새 연결이면 → CT_NEW → 정책 확인 필요
    │
    ├── 3. cilium_policy_v2_{EP_ID} 조회
    │   └── (소스Identity, 대상Port, Proto, Ingress)
    │   └── Allow → 통과, Deny → 드롭
    │   └── ProxyPort != 0 → L7 프록시로 리다이렉트
    │
    ├── 4. cilium_lb4_services_v2 조회 (서비스 트래픽인 경우)
    │   └── VIP:Port → 백엔드 선택
    │   └── cilium_lb4_backends_v3에서 백엔드 IP 조회
    │   └── DNAT 수행
    │
    └── 5. cilium_lxc 조회 (로컬 배달 시)
        └── 대상 IP → 엔드포인트 정보 → veth로 리다이렉트
```

### 왜 이렇게 많은 맵이 필요한가?

BPF 프로그램은 성능상 이유로 단일 맵에 모든 데이터를 넣지 않는다:
- **관심사 분리**: 각 맵은 하나의 책임 (CT는 연결 추적, Policy는 정책 판단)
- **맵 타입 최적화**: LPM Trie(CIDR), LRU Hash(CT), Hash(정책) 등 용도에 맞는 타입 선택
- **per-Endpoint 맵**: 정책 맵은 엔드포인트별로 분리하여 맵 크기 제한
- **Tail Call 맵**: 프로그램 크기 제한 우회를 위한 프로그램 배열
