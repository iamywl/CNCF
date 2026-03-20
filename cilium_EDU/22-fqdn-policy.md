# 22. FQDN 기반 정책 (DNS-aware Network Policy)

> Cilium 소스 기준: `pkg/fqdn/`, `pkg/fqdn/dnsproxy/`, `pkg/fqdn/namemanager/`

---

## 1. 개요

### 1.1 FQDN 정책이란?

전통적인 네트워크 정책은 IP 주소 기반으로 트래픽을 제어한다. 그러나 클라우드 네이티브 환경에서 외부 서비스의 IP는 동적으로 변경되므로, **도메인 이름(FQDN) 기반 정책**이 필수적이다.

Cilium의 FQDN 정책 엔진은 다음 문제를 해결한다:

1. **동적 IP 추적**: `api.github.com`처럼 IP가 수시로 변하는 외부 서비스에 대한 정책 적용
2. **DNS 투명 프록시**: Pod의 DNS 요청을 가로채 정책 규칙과 매칭
3. **실시간 정책 업데이트**: DNS 응답에서 새 IP를 학습하면 BPF 맵을 즉시 갱신

### 1.2 아키텍처 개요

```
+------------------+     DNS 요청      +----------------+     DNS 전달      +-------------+
|   Pod/Endpoint   | ──────────────── > |   DNS Proxy    | ──────────────── > |  Upstream   |
|                  |                    | (L7 Proxy)     |                    |  DNS Server |
+------------------+                    +----------------+                    +-------------+
                                              │
                                              │ DNS 응답 파싱
                                              ▼
                                        +----------------+
                                        | NameManager    |
                                        | (FQDN→IP 매핑) |
                                        +----------------+
                                              │
                                     ┌────────┴────────┐
                                     ▼                  ▼
                              +------------+     +-------------+
                              | DNS Cache  |     |  IPCache    |
                              | (TTL 관리)  |     | (Identity)  |
                              +------------+     +-------------+
                                                       │
                                                       ▼
                                                +-------------+
                                                |  BPF Maps   |
                                                | (PolicyMap)  |
                                                +-------------+
```

### 1.3 소스 디렉토리 구조

```
pkg/fqdn/
├── cache.go           # DNSCache - FQDN→IP 매핑 캐시 (TTL 관리)
├── cache_test.go
├── doc.go
├── bootstrap/         # 에이전트 시작 시 FQDN 데이터 복원
├── dns/               # DNS 패킷 파싱/직렬화
│   └── dns.go
├── dnsproxy/          # DNS 투명 프록시 (L7)
│   ├── proxy.go       # DNSProxy 구조체 - 핵심 프록시 로직
│   ├── helpers.go     # DNS 서버 타깃 룩업
│   ├── shared_client.go # DNS 클라이언트 연결 풀
│   ├── types.go       # 인터페이스 정의
│   └── udp.go         # UDP 소켓 관리
├── lookup/            # 엔드포인트 룩업 인터페이스
│   └── lookup.go
├── matchpattern/      # FQDN 와일드카드 패턴 → 정규식 변환
├── messagehandler/    # DNS 메시지 핸들링
├── namemanager/       # FQDN 셀렉터 관리 + GC
│   ├── manager.go     # NameManager - 셀렉터→IP 매핑 관리
│   ├── gc.go          # DNS 캐시 GC (TTL 만료 처리)
│   ├── preallocator.go # Identity 사전 할당
│   ├── api.go         # 외부 인터페이스
│   └── cell.go        # Hive DI 셀
├── proxy/             # 프록시 관련 유틸리티
├── re/                # 정규식 캐시 관리
├── restore/           # 에이전트 재시작 시 규칙 복원
├── rules/             # FQDN 규칙 관리
└── service/           # 서비스 통합
```

---

## 2. 핵심 데이터 구조

### 2.1 DNS 캐시 (DNSCache)

`pkg/fqdn/cache.go`에 정의된 `DNSCache`는 FQDN 정책의 핵심 데이터 저장소이다.

```
DNSCache 내부 구조:
┌──────────────────────────────────────────────────────┐
│ DNSCache                                              │
│                                                        │
│  forward: map[string][]*cacheEntry                     │
│    "api.github.com" → [                               │
│      {Name:"api.github.com", IPs:[140.82.114.5],     │
│       LookupTime:T1, ExpirationTime:T1+300s, TTL:300}│
│    ]                                                   │
│                                                        │
│  reverse: map[netip.Addr][]string                      │
│    140.82.114.5 → ["api.github.com"]                  │
│                                                        │
│  overLimit: map[string]bool                            │
│    (항목 수 제한 초과된 이름 추적)                        │
│                                                        │
│  cleanup: map[string]*cacheEntry                       │
│    (TTL 만료 예정 엔트리 추적)                           │
└──────────────────────────────────────────────────────┘
```

`cacheEntry` 구조체 (`pkg/fqdn/cache.go:34`):

```go
type cacheEntry struct {
    Name           string      `json:"fqdn,omitempty"`
    LookupTime     time.Time   `json:"lookup-time,omitempty"`
    ExpirationTime time.Time   `json:"expiration-time,omitempty"`
    TTL            int         `json:"ttl,omitempty"`
    IPs            []netip.Addr `json:"ips,omitempty"`
}
```

**왜 `cacheEntry`가 불변(immutable)인가?**

소스 주석에서 "cacheEntry objects are immutable once created"라고 명시한다. 그 이유는:

1. **주소를 고유 식별자로 사용**: 포인터 주소가 곧 ID이므로 수정 불가
2. **동시성 안전**: 여러 고루틴이 동시에 읽어도 레이스 컨디션 없음
3. **GC 추적 단순화**: `cleanup` 맵에서 포인터로 추적하므로 값 변경 시 추적 깨짐

### 2.2 DNS 프록시 (DNSProxy)

`pkg/fqdn/dnsproxy/proxy.go:61`의 `DNSProxy` 구조체:

```go
type DNSProxy struct {
    cfg        DNSProxyConfig
    BindPort   uint16

    // 엔드포인트 보안 아이덴티티 조회
    proxyLookupHandler lookup.ProxyLookupHandler

    // DNS 응답 콜백 (캐시 + NameManager로 전달)
    NotifyOnDNSMsg NotifyOnDNSMsgFunc

    // DNS 서버 인스턴스 (TCP/UDP × IPv4/IPv6)
    DNSServers []*dns.Server

    // 동시성 제한
    ConcurrencyLimit       *semaphore.Weighted
    ConcurrencyGracePeriod time.Duration

    // 정책 허용 규칙 (엔드포인트별)
    allowed      perEPAllow
    currentRules perEPPolicy

    // 복원된 규칙 (재시작 시)
    restored perEPRestored

    // 정규식 캐시 (참조 카운트 기반)
    cache regexCache
}
```

**정규식 캐시의 참조 카운트**:

```go
type regexCache map[string]*regexCacheEntry

type regexCacheEntry struct {
    regex          *regexp.Regexp
    referenceCount int
}
```

**왜 참조 카운트를 사용하는가?**

여러 엔드포인트가 동일한 FQDN 패턴(예: `*.github.com`)을 사용하면, 정규식 컴파일 비용을 공유한다. 참조 카운트가 0이 되면 캐시에서 제거한다.

### 2.3 NameManager

`pkg/fqdn/namemanager/manager.go:36`의 `manager` 구조체:

```go
type manager struct {
    // 모든 FQDNSelector → 컴파일된 정규식
    allSelectors map[api.FQDNSelector]*regexp.Regexp

    // DNS 캐시 인스턴스
    cache *fqdn.DNSCache

    // 셀렉터 변경 스트림 (사전 할당용)
    selectorChanges chan selectorChange

    // 사전 할당된 Identity ID
    selectorIDs map[api.FQDNSelector][]identity.NumericIdentity

    // 이름별 잠금 (동시 업데이트 조율)
    nameLocks []*lock.Mutex
}
```

**왜 `nameLocks` 배열을 사용하는가?**

DNS 이름별로 잠금을 분리하여 병렬성을 극대화한다. FNV 해시로 이름을 버킷에 매핑:

```go
func (n *manager) LockName(name string) {
    h := fnv.New32a()
    h.Write([]byte(name))
    idx := h.Sum32() % uint32(len(n.nameLocks))
    n.nameLocks[idx].Lock()
}
```

---

## 3. DNS 프록시 동작 흐름

### 3.1 DNS 요청 처리 파이프라인

```
┌─────────────────────────────────────────────────────────────────┐
│                    DNS 요청 처리 파이프라인                        │
├─────────────────────────────────────────────────────────────────┤
│                                                                   │
│  1. Pod → DNS 요청 (UDP/TCP)                                     │
│     │                                                             │
│  2. BPF 프로그램이 DNS 패킷을 프록시 포트로 리다이렉트              │
│     │  (bpf_host.c → L7 proxy redirect)                          │
│     │                                                             │
│  3. DNSProxy.ServeDNS() 호출                                     │
│     │                                                             │
│  4. 동시성 제어 (Semaphore)                                       │
│     │  ConcurrencyLimit.Acquire()                                 │
│     │                                                             │
│  5. 소스 엔드포인트 식별                                           │
│     │  proxyLookupHandler.GetEndpointByAddr()                     │
│     │                                                             │
│  6. 정책 검사 (allowed 맵 조회)                                   │
│     │  DNS 이름이 정규식과 매치하는지 확인                          │
│     │                                                             │
│  7. 허용 시: 업스트림 DNS 서버로 전달                              │
│     │  SharedClients.Exchange()                                   │
│     │                                                             │
│  8. DNS 응답 수신                                                  │
│     │                                                             │
│  9. NotifyOnDNSMsg 콜백 호출                                      │
│     │  → NameManager.UpdateGenerateDNS()                          │
│     │  → DNSCache.Update()                                        │
│     │  → IPCache 메타데이터 업데이트                                │
│     │  → BPF PolicyMap 갱신                                       │
│     │                                                             │
│  10. 응답을 Pod에 전달                                             │
│                                                                   │
└─────────────────────────────────────────────────────────────────┘
```

### 3.2 정책 검사 로직

DNS 프록시는 `perEPAllow` 맵을 사용하여 계층적으로 정책을 검사한다:

```
perEPAllow 검사 체인:
┌──────────────────┐
│ Endpoint ID      │ ← 1단계: 어떤 Pod에서 온 요청?
├──────────────────┤
│ Port + Protocol  │ ← 2단계: 어떤 포트/프로토콜? (보통 53/UDP)
├──────────────────┤
│ CachedSelector   │ ← 3단계: 어떤 보안 Identity에 대한 규칙?
├──────────────────┤
│ Regexp Match     │ ← 4단계: DNS 이름이 정규식과 매치?
└──────────────────┘
```

```go
// perEPAllow 계층 구조
type perEPAllow map[uint64]portProtoToSelectorAllow

type portProtoToSelectorAllow map[restore.PortProto]CachedSelectorREEntry

type CachedSelectorREEntry map[policy.CachedSelector]*regexp.Regexp
```

### 3.3 공유 DNS 클라이언트 (SharedClients)

`pkg/fqdn/dnsproxy/shared_client.go`에서 구현된 `SharedClients`는 업스트림 DNS 서버로의 연결을 관리한다:

```
SharedClients 연결 풀:
┌─────────────────────────────────────────────┐
│ SharedClients                                │
│                                               │
│  clients: map[ServerID]*SharedClient          │
│                                               │
│  ServerID = (network, address)                │
│    ("udp", "8.8.8.8:53") → SharedClient{     │
│      conn: *dns.Conn,                         │
│      pending: map[uint16]chan *dns.Msg,        │
│      refCount: 5,                              │
│    }                                           │
│                                               │
│  장점:                                         │
│  - 동일 서버로의 연결 재사용                     │
│  - 요청 다중화 (ID별 응답 라우팅)               │
│  - 연결 장애 시 자동 재생성                      │
└─────────────────────────────────────────────┘
```

---

## 4. DNS 캐시 관리

### 4.1 캐시 업데이트 흐름

`DNSCache.Update()` 메서드는 DNS 응답을 캐시에 반영한다:

```
Update 흐름:
1. 이름 정규화 (소문자 변환, FQDN "." 제거)
2. 새 cacheEntry 생성 (불변)
3. forward 맵에 추가
4. reverse 맵 업데이트 (IP → 이름 역방향)
5. 항목 수 제한 확인 (overLimit)
6. cleanup 맵에 TTL 추적 등록
7. UpdateStatus 반환 (추가/변경된 IP 목록)
```

`UpdateStatus` 구조체 (`pkg/fqdn/cache.go:54`):

```go
type UpdateStatus struct {
    // 새로 추가된 IP
    Added []netip.Addr
    // 이전에 있었지만 여전히 유효한 IP
    Kept []netip.Addr
    // 제거된 IP (TTL 만료 또는 응답에서 사라짐)
    Removed []netip.Addr
}
```

### 4.2 TTL 기반 GC (Garbage Collection)

`pkg/fqdn/namemanager/gc.go`의 `doGC()` 함수가 1분 주기로 실행:

```
DNS GC 파이프라인:
┌──────────────────────────────────────────────────┐
│ 1. 각 엔드포인트의 DNSHistory.GC(now) 호출        │
│    → TTL 만료 엔트리를 DNSZombies로 이동           │
│                                                    │
│ 2. DNSZombies.GC() 호출                            │
│    → alive: CT(Connection Tracking)에서 아직 사용중 │
│    → dead: CT에서도 사용 안함                       │
│                                                    │
│ 3. alive 좀비를 activeConnections 캐시에 재삽입     │
│    → TTL = 2 × GC 주기(2분)로 연장                  │
│    → CT GC와의 타이밍 불일치 대응                    │
│                                                    │
│ 4. 글로벌 캐시 ReplaceFromCacheByNames() 호출       │
│    → 엔드포인트 캐시들의 합집합으로 갱신             │
│                                                    │
│ 5. 완전히 제거된 IP의 IPCache 메타데이터 삭제        │
│    → maybeRemoveMetadata()                          │
└──────────────────────────────────────────────────┘
```

**왜 "좀비(Zombie)" 메커니즘이 필요한가?**

DNS TTL이 만료되어도 활성 TCP 연결은 계속 유효할 수 있다. IP를 즉시 삭제하면 기존 연결이 끊긴다. 좀비 메커니즘은:

1. TTL 만료된 엔트리를 "좀비 목록"으로 이동
2. CT(Connection Tracking) 테이블과 교차 확인
3. 실제로 사용 중인 IP만 유지, 미사용 IP만 삭제

### 4.3 캐시 항목 수 제한

```go
const DefaultMinTTL = 3600 // 1시간
const DefaultMaxIPsPerHost = 50
```

하나의 FQDN에 대해 저장할 수 있는 최대 IP 수를 제한한다. CDN 서비스처럼 수천 개의 IP를 반환하는 경우 메모리를 보호하기 위함이다.

---

## 5. NameManager와 Identity 시스템 통합

### 5.1 FQDN 셀렉터 등록

`NameManager.RegisterFQDNSelector()` (`pkg/fqdn/namemanager/manager.go:140`):

```
RegisterFQDNSelector 흐름:
┌──────────────────────────────────────────┐
│ 1. FQDNSelector → 정규식 컴파일           │
│    matchName: "api.github.com"           │
│    → regex: "^api\\.github\\.com$"       │
│                                            │
│    matchPattern: "*.github.com"           │
│    → regex: "^.*\\.github\\.com$"        │
│                                            │
│ 2. allSelectors 맵에 등록                  │
│                                            │
│ 3. 기존 캐시에서 매칭되는 이름 검색         │
│    mapSelectorsToNamesLocked()             │
│                                            │
│ 4. 매칭된 이름+IP에 대한 Identity 라벨 계산 │
│    deriveLabelsForNames()                  │
│                                            │
│ 5. IPCache 메타데이터 업데이트              │
│    updateMetadata()                         │
│    → CIDR Identity 할당/해제               │
└──────────────────────────────────────────┘
```

### 5.2 Identity 사전 할당 (Pre-allocation)

`pkg/fqdn/namemanager/preallocator.go`에서 구현된 사전 할당:

**왜 사전 할당이 필요한가?**

FQDN 정책이 처음 적용될 때, 새로운 FQDNSelector에 대한 CIDR Identity를 할당해야 한다. 이 과정은 KVStore(etcd) 호출을 포함하므로 지연이 발생할 수 있다. 사전 할당은:

1. 셀렉터가 등록되면 비동기적으로 Identity를 미리 할당
2. DNS 응답 수신 시 이미 할당된 Identity를 즉시 사용
3. 첫 DNS 요청의 레이턴시를 줄임

### 5.3 매칭 패턴 시스템

`pkg/fqdn/matchpattern/` 디렉토리에서 FQDN 패턴을 정규식으로 변환:

| 패턴 타입 | 예시 | 변환 결과 |
|-----------|------|-----------|
| matchName | `api.github.com` | `^api\.github\.com$` |
| matchPattern | `*.github.com` | `^.*\.github\.com$` |
| matchPattern | `*.*.svc.cluster.local` | `^.*\..*\.svc\.cluster\.local$` |

---

## 6. BPF 데이터패스 통합

### 6.1 DNS 트래픽 리다이렉트

```
DNS 리다이렉트 BPF 경로:
┌──────────────┐     ┌────────────────┐     ┌──────────────┐
│   Pod (lxc)  │ ──> │  bpf_lxc.c     │ ──> │  DNS Proxy   │
│ DNS 요청     │     │ L7 정책 검사    │     │ (Port 8053)  │
│ → 8.8.8.8:53│     │ → proxy_port로  │     │              │
│              │     │   리다이렉트     │     │              │
└──────────────┘     └────────────────┘     └──────────────┘

BPF에서 DNS 프록시로 리다이렉트하는 조건:
1. L4Policy에 DNS 포트(53)가 L7 정책으로 등록됨
2. 패킷의 목적지 포트가 53 (DNS)
3. BPF는 패킷을 DNS 프록시 포트로 리다이렉트
```

### 6.2 정책 맵 업데이트

DNS 응답에서 새 IP를 학습하면:

```
IP 학습 → 정책 맵 업데이트 경로:
1. DNS 응답 파싱: "api.github.com" → 140.82.114.5
2. NameManager.UpdateGenerateDNS()
3. IPCache.Upsert(140.82.114.5, labels=["fqdn:api.github.com"])
4. CIDR Identity 할당 (예: ID=54321)
5. SelectorCache 재평가
6. BPF PolicyMap 업데이트:
   endpoint_id → {identity=54321, port=443, action=ALLOW}
```

---

## 7. 에이전트 재시작 시 복원

### 7.1 DNS 규칙 복원

`pkg/fqdn/restore/` 패키지가 에이전트 재시작 시 FQDN 규칙을 복원:

```
복원 흐름:
┌──────────────────────────────────────────────┐
│ 1. 엔드포인트 헤더 파일에서 DNS 캐시 복원      │
│    ep.DNSHistory (각 엔드포인트별 캐시)         │
│                                                │
│ 2. DNSZombies 복원                             │
│    활성 연결의 IP 유지                          │
│                                                │
│ 3. 복원된 데이터를 글로벌 캐시에 병합            │
│    RestorationNotify()                         │
│                                                │
│ 4. 복원된 규칙을 DNS 프록시에 설치              │
│    restored perEPRestored 맵 사용               │
│                                                │
│ 5. 새 정책이 로드될 때까지 복원된 규칙 사용      │
│    → 무중단 업그레이드 보장                      │
└──────────────────────────────────────────────┘
```

### 7.2 `perEPRestored` 구조체

```go
type perEPRestored map[uint64]map[restore.PortProto][]restoredIPRule

type restoredIPRule struct {
    restore.IPRule
    regex *regexp.Regexp  // 복원 시 재컴파일
}
```

복원된 규칙에는 이전에 학습한 IP 목록이 포함되어 있어, 새 DNS 룩업 없이도 기존 연결을 허용할 수 있다.

---

## 8. 성능 최적화

### 8.1 정규식 캐시

`regexCache`는 동일 정규식의 중복 컴파일을 방지:

```
정규식 캐시 동작:
┌────────────────────────────────────────────┐
│ 패턴: "^.*\\.github\\.com$"               │
│                                              │
│ EP-1 정책: toFQDNs: [{matchPattern: "*.github.com"}] │
│ EP-2 정책: toFQDNs: [{matchPattern: "*.github.com"}] │
│ EP-3 정책: toFQDNs: [{matchPattern: "*.github.com"}] │
│                                              │
│ regexCache:                                  │
│   "^.*\\.github\\.com$" → {                 │
│     regex: *regexp.Regexp (1회만 컴파일)      │
│     referenceCount: 3                         │
│   }                                           │
│                                              │
│ EP-3 삭제 시: referenceCount = 2             │
│ EP-1, EP-2 삭제 시: referenceCount = 0 → 삭제│
└────────────────────────────────────────────┘
```

### 8.2 동시성 제어

```go
ConcurrencyLimit       *semaphore.Weighted  // 병렬 DNS 처리 제한
ConcurrencyGracePeriod time.Duration        // 세마포어 대기 타임아웃
```

DNS 폭주(flood) 공격이나 대량 DNS 요청 시 에이전트 과부하를 방지한다.

### 8.3 이름별 잠금 (Name Locks)

```go
nameLocks []*lock.Mutex  // 기본 256개 버킷
```

전체 캐시에 대한 단일 잠금 대신, DNS 이름을 FNV 해시로 버킷에 분산하여 병렬성을 극대화한다. 서로 다른 도메인에 대한 업데이트는 동시에 진행 가능하다.

---

## 9. FQDN 정책 CRD 예시

### 9.1 CiliumNetworkPolicy FQDN 규칙

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: allow-github-api
spec:
  endpointSelector:
    matchLabels:
      app: myapp
  egress:
    - toFQDNs:
        - matchName: "api.github.com"
        - matchPattern: "*.githubusercontent.com"
      toPorts:
        - ports:
            - port: "443"
              protocol: TCP
    - toEndpoints:
        - matchLabels:
            k8s:io.kubernetes.pod.namespace: kube-system
            k8s:k8s-app: kube-dns
      toPorts:
        - ports:
            - port: "53"
              protocol: UDP
          rules:
            dns:
              - matchName: "api.github.com"
              - matchPattern: "*.githubusercontent.com"
```

**왜 DNS 규칙과 toFQDNs를 모두 지정하는가?**

1. `dns` 규칙: DNS 프록시에서 어떤 DNS 쿼리를 허용할지 결정
2. `toFQDNs` 규칙: 학습된 IP에 대한 실제 네트워크 트래픽 허용

두 단계가 모두 필요한 이유: DNS 쿼리 자체를 허용하지 않으면 IP를 학습할 수 없고, IP를 학습해도 egress 정책이 없으면 트래픽이 차단된다.

---

## 10. 모니터링과 메트릭

### 10.1 주요 Prometheus 메트릭

| 메트릭 | 설명 |
|--------|------|
| `cilium_fqdn_gc_deletions_total` | GC에서 삭제된 DNS 항목 수 |
| `cilium_fqdn_active_names` | 엔드포인트별 활성 DNS 이름 수 |
| `cilium_fqdn_active_ips` | 엔드포인트별 활성 IP 수 |
| `cilium_fqdn_alive_zombie_connections` | 좀비 상태의 활성 연결 수 |
| `cilium_fqdn_selectors` | 등록된 FQDN 셀렉터 수 |

### 10.2 디버깅 명령어

```bash
# DNS 캐시 조회
cilium-dbg fqdn cache list

# FQDN Identity 조회
cilium-dbg identity list --fqdn

# DNS 프록시 상태 확인
cilium-dbg status --verbose

# DNS 정책 규칙 확인
cilium-dbg policy get --selector-fqdn
```

---

## 11. 설계 결정의 이유 (Why)

### Q1: 왜 투명 DNS 프록시를 사용하는가?

**대안**: 주기적 DNS 폴링으로 IP를 수집할 수 있다.

**Cilium의 선택 이유**:
- **실시간성**: Pod가 실제로 사용하는 DNS 이름만 추적
- **보안**: DNS 쿼리 자체를 검사하여 비인가 도메인 접근 차단
- **정확성**: Pod가 받는 실제 DNS 응답의 IP를 정확히 캡처

### Q2: 왜 엔드포인트별 캐시와 글로벌 캐시를 분리하는가?

- **엔드포인트 캐시**: 각 Pod가 실제로 조회한 DNS 데이터
- **글로벌 캐시**: 모든 엔드포인트 캐시의 합집합

분리 이유:
1. Pod 삭제 시 해당 Pod의 DNS 데이터만 정리 가능
2. GC가 Pod별로 독립 실행 → 다른 Pod에 영향 없음
3. 글로벌 캐시는 정책 결정용, 엔드포인트 캐시는 GC용

### Q3: 왜 좀비 메커니즘이 CT(ConnTrack)과 연동되는가?

DNS TTL은 "이 IP가 유효한 시간"이지만, TCP 연결은 TTL과 무관하게 유지될 수 있다. 단순히 TTL 만료로 IP를 삭제하면 활성 연결이 끊긴다.

CT 테이블은 실제 네트워크 연결 상태를 추적하므로, "이 IP에 대한 활성 연결이 있는가?"를 판단할 수 있다.

---

## 12. 한계와 고려사항

| 항목 | 설명 |
|------|------|
| DNS-over-HTTPS | 투명 프록시가 가로챌 수 없음 (별도 설정 필요) |
| CNAME 체인 | CNAME을 따라가며 모든 중간 이름도 허용해야 함 |
| 와일드카드 성능 | `*.*.*` 같은 넓은 패턴은 정규식 매칭 비용 증가 |
| 캐시 메모리 | 대규모 클러스터에서 DNS 캐시 크기가 수십 MB 가능 |
| TTL 0 | TTL이 0인 응답은 즉시 만료 → 연결 안정성 문제 (MinTTL로 보완) |

---

*검증 기준: Cilium 소스코드 `pkg/fqdn/` 디렉토리 직접 분석*
