# PoC-22: FQDN 기반 네트워크 정책 시뮬레이션

## 개요

이 PoC는 Cilium의 FQDN(Fully Qualified Domain Name) 기반 네트워크 정책 엔진을 시뮬레이션한다.
Kubernetes 환경에서 Pod의 외부 통신을 IP가 아닌 **도메인 이름** 기준으로 제어하는 메커니즘을
Go 표준 라이브러리만으로 재현한다.

핵심 시뮬레이션 대상:
- DNS 캐시(TTL 기반 만료)
- DNS 프록시(정규식 기반 정책 검사)
- NameManager(FQDN 셀렉터에서 IP 매핑)
- 좀비 메커니즘(TTL 만료 후 활성 연결 보호)
- 정규식 캐시(참조 카운트 기반 중복 컴파일 방지)

## 배경 지식

### IP 기반 정책의 한계

전통적인 Kubernetes NetworkPolicy는 IP CIDR 블록으로 이그레스(egress) 트래픽을 제어한다.
그러나 현대 인프라에서는 이 방식에 근본적인 한계가 있다.

- **CDN/클라우드 서비스의 동적 IP**: `api.github.com`의 IP는 수시로 변경된다. AWS CloudFront, Akamai 등 CDN 뒤에 있는 서비스는 수백 개의 IP를 로테이션한다.
- **공유 IP**: 여러 도메인이 동일 IP를 사용하는 경우, IP 허용만으로는 의도하지 않은 도메인까지 접근을 허용하게 된다.
- **관리 복잡성**: IP가 변경될 때마다 정책을 수동으로 갱신해야 하며, 이는 운영 부담을 크게 증가시킨다.

### FQDN 기반 정책의 동작 원리

Cilium은 이 문제를 **DNS 프록시**를 통해 해결한다.

1. Pod에서 나가는 모든 DNS 쿼리를 Cilium의 L7 DNS 프록시가 투명하게 가로챈다.
2. DNS 쿼리가 FQDN 정책에 허용된 도메인인지 정규식으로 검사한다.
3. 허용된 경우 업스트림 DNS 서버로 쿼리를 전달하고, 응답에서 도메인-IP 매핑을 학습한다.
4. 학습된 IP를 eBPF 데이터패스의 PolicyMap에 자동으로 주입하여 L3/L4 수준에서 허용한다.
5. TTL이 만료되면 캐시에서 제거하되, 활성 연결이 있는 IP는 "좀비"로 보호한다.

## 시뮬레이션 개념 매핑

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|-----------------|----------------|
| DNS 캐시 | `pkg/fqdn/cache.go` - `DNSCache` 구조체 | `DNSCache` 구조체로 forward/reverse 맵 관리, TTL 기반 GC |
| DNS 프록시 | `pkg/fqdn/dnsproxy/proxy.go` - L7 투명 프록시 | `DNSProxy` 구조체로 정책 검사 + 캐시 연동 시뮬레이션 |
| NameManager | `pkg/fqdn/namemanager/manager.go` | `NameManager` 구조체로 FQDN 셀렉터-IP 매핑 관리 |
| 좀비 메커니즘 | `pkg/fqdn` - TTL 만료 후 CT 기반 보호 | `ZombieTracker`로 활성/비활성 연결 분류 시뮬레이션 |
| 정규식 캐시 | `dnsproxy/proxy.go:156` - 참조 카운트 캐시 | `RegexCache`로 Acquire/Release 패턴 구현 |
| FQDN 규칙 변환 | `matchpattern` 패키지 - 와일드카드 to 정규식 | `fqdnRuleToRegex` 함수로 `*.example.com` -> `^.*\.example\.com$` 변환 |

## 아키텍처 다이어그램

```
                        FQDN 기반 정책 처리 흐름
 ============================================================================

  Pod (EP-1001)                    Cilium Agent
 +-------------+     DNS Query    +------------------------------------------+
 | curl        |  ------------->  |  DNSProxy                                |
 | api.github. |                  |  +------------------------------------+  |
 | com         |                  |  | 1. CheckAllowed(EP, queryName)     |  |
 +-------------+                  |  |    - allowed[EP] → []*regexp.Regexp|  |
                                  |  |    - 정규식 매칭으로 허용/거부 판단    |  |
                                  |  +----+-----------+-------------------+  |
                                  |       |           |                      |
                                  |  [허용]|      [거부]| → REFUSED 응답       |
                                  |       v           |                      |
                                  |  +----+--------+  |                      |
                                  |  | Upstream DNS|  |  RegexCache          |
                                  |  | Lookup      |  |  +-----------------+ |
                                  |  +----+--------+  |  | pattern → regex | |
                                  |       |           |  | (참조 카운트)     | |
                                  |       v           |  +-----------------+ |
                                  |  +----+--------+  |                      |
                                  |  | DNSCache    |  |                      |
                                  |  | Update()    |  |                      |
                                  |  | - forward   |  |                      |
                                  |  |   맵 갱신    |  |                      |
                                  |  | - reverse   |  |                      |
                                  |  |   맵 갱신    |  |                      |
                                  |  +----+--------+  |                      |
                                  |       |           |                      |
                                  |       v           |                      |
                                  |  +----+--------+  |                      |
                                  |  | NameManager |  |                      |
                                  |  | UpdateDNS() |  |                      |
                                  |  | FQDN셀렉터  |  |                      |
                                  |  | → IP 매핑   |  |                      |
                                  |  +----+--------+  |                      |
                                  |       |           |                      |
                                  |       v           |                      |
                                  |  BPF PolicyMap    |                      |
                                  |  자동 갱신         |                      |
                                  +------------------------------------------+

  TTL 만료 시:
  +----------+     GC()      +---------------+     CT 조회     +---------+
  | DNSCache | ------------> | ZombieTracker | ------------->  | Alive?  |
  | (만료됨)  |              | (좀비로 보호)   |               | Y→유지   |
  +----------+               +---------------+                | N→삭제   |
                                                              +---------+
```

## 코드 해설

### 1. `DNSCache` 구조체와 `Update` 메서드

```go
type DNSCache struct {
    forward  map[string][]*cacheEntry // name → entries
    reverse  map[string][]string      // IP string → names
    minTTL   int
}
```

**무엇을 하는가**: DNS 응답을 forward(이름->IP) / reverse(IP->이름) 양방향 맵으로 캐시한다. `Update`는 새 DNS 응답이 도착하면 기존 엔트리와 비교하여 Added/Kept/Removed를 계산한다.

**실제 Cilium 대응**: `pkg/fqdn/cache.go`의 `DNSCache` 구조체. Cilium에서는 forward 맵의 각 이름에 여러 cacheEntry(불변)를 저장하고, GC 시 TTL 만료 여부로 개별 엔트리를 제거한다. MinTTL은 TTL이 너무 짧은 응답에 대한 하한선을 보장한다.

**왜 이렇게 구현했는가**: forward/reverse 양방향 맵은 이름 기반 조회(정책 적용)와 IP 기반 역조회(CT 연결 추적 시 이름 확인) 모두를 O(1)에 처리하기 위함이다.

### 2. `RegexCache` - 참조 카운트 기반 정규식 캐시

```go
func (rc *RegexCache) Acquire(pattern string) (*regexp.Regexp, error)
func (rc *RegexCache) Release(pattern string)
```

**무엇을 하는가**: 동일한 FQDN 패턴(예: `*.github.com`)을 여러 엔드포인트가 참조할 때, 정규식 컴파일을 한 번만 수행하고 참조 카운트로 공유한다. 모든 참조가 해제되면 캐시에서 삭제한다.

**실제 Cilium 대응**: `dnsproxy/proxy.go:156`의 정규식 캐시. 수백 개의 Pod이 동일한 FQDN 정책을 사용하는 클러스터에서 메모리를 절약한다.

**왜 이렇게 구현했는가**: 정규식 컴파일은 비용이 큰 연산이다. Kubernetes 클러스터에서 동일 정책이 수많은 엔드포인트에 적용되므로, 참조 카운트 패턴은 메모리와 CPU를 모두 절약하는 핵심 최적화이다.

### 3. `DNSProxy.HandleDNSQuery` - DNS 쿼리 처리 파이프라인

**무엇을 하는가**: Pod에서 발생한 DNS 쿼리를 처리하는 전체 파이프라인을 구현한다. (1) 정규식 기반 정책 검사, (2) 업스트림 DNS 조회, (3) 캐시 업데이트, (4) NameManager 콜백 호출의 4단계로 동작한다.

**실제 Cilium 대응**: `dnsproxy/proxy.go`의 `ServeDNS` 메서드. 실제로는 iptables/eBPF로 DNS 트래픽을 투명하게 프록시로 리다이렉트하고, UDP 패킷을 파싱하여 처리한다.

**왜 이렇게 구현했는가**: DNS 프록시는 FQDN 정책의 핵심 구성요소다. 정책 검사 -> DNS 해석 -> 캐시 갱신 -> IP 학습의 파이프라인 구조가 실제 Cilium의 데이터 흐름을 그대로 반영한다.

### 4. `ZombieTracker` - 활성 연결 보호

**무엇을 하는가**: TTL이 만료된 DNS 엔트리의 IP에 대해, Connection Tracking(CT) 테이블에서 활성 연결이 있는지 확인하여 활성 연결이 있으면 IP를 유지(좀비 상태)하고, 없으면 삭제한다.

**실제 Cilium 대응**: `pkg/fqdn`의 좀비 메커니즘. Cilium은 eBPF CT 맵을 실제로 조회하여 활성 TCP 연결 여부를 확인한다.

**왜 이렇게 구현했는가**: TTL 만료 즉시 IP를 제거하면 진행 중인 HTTP 요청이 끊어진다. 좀비 메커니즘은 DNS TTL의 짧은 수명과 TCP 연결의 긴 수명 사이의 불일치를 해결하는 Cilium의 핵심 설계이다.

### 5. `NameManager.RegisterSelector` - FQDN 셀렉터 관리

**무엇을 하는가**: CiliumNetworkPolicy의 `toFQDNs` 셀렉터를 등록하고, 기존 DNS 캐시에서 매칭되는 이름의 IP를 즉시 수집한다. 이후 새로운 DNS 응답이 도착할 때마다 `UpdateDNS`로 셀렉터-IP 매핑을 갱신한다.

**실제 Cilium 대응**: `namemanager/manager.go`의 `RegisterFQDNSelector`. 정책이 추가/삭제될 때 셀렉터를 등록/해제하고, DNS 응답마다 매칭 결과를 업데이트하여 eBPF PolicyMap에 반영한다.

**왜 이렇게 구현했는가**: 정책 적용 시점과 DNS 응답 도착 시점이 다를 수 있다. 셀렉터 등록 시 기존 캐시를 검색하고, 이후 콜백으로 실시간 갱신하는 구조는 정책 적용의 연속성을 보장한다.

## 실행 방법 및 예상 출력

```bash
go run main.go
```

주요 출력 발췌:

```
=======================================================================
 Cilium FQDN 기반 정책 시뮬레이션
 소스: pkg/fqdn/cache.go, pkg/fqdn/dnsproxy/proxy.go
=======================================================================

[1] DNS 캐시 기본 동작
--------------------------------------------------
  Update 'api.github.com': Added=[140.82.114.5, 140.82.114.6], Kept=[], Removed=[]
  Update 'api.github.com': Added=[140.82.114.7], Kept=[140.82.114.5], Removed=[140.82.114.6]
  캐시 상태: 1 names, 2 IPs

[2] 정규식 캐시 (참조 카운트)
--------------------------------------------------
  패턴 '^.*\.github\.com$'
  캐시 항목 수: 1, 참조 카운트: 3
  Release 1회 후 참조 카운트: 2
  모두 Release 후 캐시 항목 수: 0 (삭제됨)

[3] DNS 프록시 정책 검사
--------------------------------------------------
  EP-1001 쿼리:
  [DNS Proxy] Query: api.github.com → IPs: [...] (TTL: ...s)
  [DNS Proxy] Query: raw.githubusercontent.com → IPs: [...] (TTL: ...s)
  [DNS Proxy] DENIED: DNS query for "evil.com" denied by policy

  EP-1002 쿼리:
  [DNS Proxy] Query: maps.google.com → IPs: [...] (TTL: ...s)
  [DNS Proxy] DENIED: DNS query for "api.github.com" denied by policy

  프록시 통계: 총 5 쿼리, 허용 3, 거부 2

[5] TTL 기반 GC (Garbage Collection)
--------------------------------------------------
  GC 전 캐시: 2 names
  2초 후 GC: 영향받은 이름: [short-lived.example.com], 남은 캐시: 1 names
  'short-lived.example.com' IPs: [] (만료됨)
  'long-lived.example.com' IPs: [5.6.7.8] (유지)

[7] FQDN 패턴 매칭
--------------------------------------------------
  규칙: {MatchName:api.github.com MatchPattern:}
  정규식: ^api\.github\.com$
    api.github.com                                → HIT
    www.github.com                                → MISS
    github.com                                    → MISS
```

(IP 값과 TTL은 시뮬레이션마다 랜덤 요소로 인해 다를 수 있다.)

## 핵심 포인트

1. **DNS 프록시를 통한 투명한 도메인 학습**: Pod은 자신의 DNS 트래픽이 가로채지는 것을 인지하지 못한다. Cilium은 이 투명성을 유지하면서 도메인-IP 매핑을 실시간으로 학습한다.

2. **정규식 기반 정책 매칭과 참조 카운트 최적화**: `*.github.com` 같은 와일드카드 패턴은 정규식으로 변환되어 검사되며, 동일 패턴을 수백 개 엔드포인트가 공유할 때 참조 카운트 캐시로 메모리를 절약한다.

3. **좀비 메커니즘으로 연결 안정성 보장**: DNS TTL은 보통 60-300초이지만 TCP 연결은 수 시간 지속될 수 있다. TTL 만료 즉시 IP를 차단하면 활성 연결이 끊어지므로, CT 테이블 기반 좀비 메커니즘이 이 문제를 해결한다.

4. **Forward/Reverse 양방향 캐시 설계**: 이름->IP(정책 적용용)와 IP->이름(역추적용) 두 방향의 조회를 모두 O(1)에 지원하여, 대규모 클러스터에서도 성능 저하 없이 동작한다.

5. **MinTTL을 통한 캐시 안정성 확보**: 일부 DNS 응답은 TTL이 0이거나 극히 짧아서, 그대로 적용하면 캐시가 지나치게 빈번하게 갱신된다. MinTTL 하한선으로 불필요한 DNS 재조회와 정책 재계산을 방지한다.

## 실제 Cilium과의 차이점

| 항목 | 이 시뮬레이션 | 실제 Cilium |
|------|-------------|-------------|
| DNS 패킷 처리 | 함수 호출로 직접 시뮬레이션 | iptables/eBPF로 UDP 53 트래픽을 투명 리다이렉트, DNS 와이어 프로토콜 파싱 |
| IP 학습 후 적용 | 콘솔 출력으로 확인 | eBPF PolicyMap에 학습된 IP를 직접 삽입하여 커널 레벨에서 L3/L4 필터링 |
| CT 조회 | 70% 확률의 랜덤 시뮬레이션 | eBPF CT 맵에서 실제 TCP/UDP 연결 상태를 조회 |
| 동시성 | sync.RWMutex 기반 | eBPF 맵의 per-CPU 해시맵 + Agent 측 잠금 조합 |
| 업스트림 DNS | 결정적 IP 생성 함수 | 실제 UDP 패킷을 업스트림 DNS 서버로 전달하고 응답 수신 |
| 정규식 변환 | 단순 `*` -> `.*` 치환 | `matchpattern` 패키지의 정교한 와일드카드-정규식 변환 (DNS 라벨 경계 등 처리) |
| GC 주기 | 수동 호출 | `fqdn-gc-interval` 설정에 따라 주기적 자동 실행 (기본 0초 = 매 DNS 이벤트마다) |
| 스케일 | 소수 도메인 데모 | 수만 개 도메인, 수천 엔드포인트 규모에서 동작하도록 최적화 |
