# Cilium Policy Engine PoC

## 개요

이 PoC는 Cilium 정책 엔진의 핵심 메커니즘을 순수 Go 표준 라이브러리만으로 시뮬레이션한다. 실제 Cilium 코드의 아키텍처와 설계를 따르면서, BPF나 Kubernetes 의존성 없이 정책 엔진의 동작 원리를 이해할 수 있도록 구성되었다.

## 실행 방법

```bash
cd EDU/poc-10-policy-engine
go run main.go
```

외부 의존성이 없으므로 별도의 `go mod` 설정이 필요하지 않다.

## 시뮬레이션 시나리오

### 시나리오 1: Default Deny 동작

정책이 없는 상태(allow all)에서 정책을 추가하면 default deny가 활성화되어, 명시적으로 허용된 트래픽만 통과한다.

- 정책 없음: BPF 맵 비어있음 (데이터경로에서 기본 허용)
- `allow-frontend-to-backend` 규칙 추가 후: frontend -> backend:8080/TCP만 허용, 나머지 모든 ingress 차단

실제 Cilium 참조:
- `pkg/policy/repository.go`의 `computePolicyEnforcementAndRules()`
- 정책 강제 모드: `default`, `always`, `never`

### 시나리오 2: L7 HTTP 정책

L7 HTTP 정책이 적용되면 BPF 맵에 프록시 포트가 설정되어 트래픽이 Envoy 프록시를 거치게 된다. 프록시에서 HTTP Method/Path 기반 세밀한 접근 제어를 수행한다.

- `GET /api/v1/*` 허용, `POST /api/v1/submit` 허용
- `DELETE /api/v1/*` 차단, `GET /admin/*` 차단
- 허용되지 않은 Identity(monitor)의 요청도 L7에서 차단

실제 Cilium 참조:
- `pkg/policy/api/http.go`의 `PortRuleHTTP`
- `pkg/proxy/envoyproxy.go`의 Envoy 프록시 리다이렉트
- `pkg/policy/l4.go`의 `L4Filter`, `PerSelectorPolicy`

### 시나리오 3: CIDR 기반 정책

클러스터 외부 IP 범위(CIDR)에 대한 정책. CIDR은 내부적으로 Identity로 변환되어 동일한 BPF 맵 메커니즘으로 처리된다.

- `203.0.113.0/24` -> `frontend:443/TCP` 허용
- 해당 CIDR에 고유한 NumericIdentity 할당

실제 Cilium 참조:
- `pkg/policy/api/cidr.go`의 `CIDRRule`
- `pkg/policy/types/selector.go`의 `CIDRSelector`
- CIDR -> 라벨(`cidr:203.0.113.0/24`) -> Identity 변환

### 시나리오 4: FQDN 정책 (DNS 프록시 연동)

FQDN 정책의 전체 생명주기를 시뮬레이션한다:

1. FQDN 규칙 추가 (DNS 응답 전이므로 BPF 맵에 항목 없음)
2. DNS 프록시가 DNS 응답을 관찰 (`api.example.com -> 93.184.216.34`)
3. IP에 FQDN Identity 할당 (`fqdn:api.example.com`)
4. 정책 재계산으로 BPF 맵에 해당 Identity+Port 항목 추가
5. DNS 레코드 변경 시 새 IP에도 자동으로 Identity 할당 및 정책 적용

실제 Cilium 참조:
- `pkg/fqdn/cache.go`의 `DNSCache`
- `pkg/proxy/dns.go`의 `dnsRedirect`
- `pkg/policy/api/fqdn.go`의 `FQDNSelector`

### 시나리오 5: Deny 규칙과 Tier 우선순위

Tier 시스템(Admin > Normal > Baseline)과 Deny > Allow 우선순위를 시뮬레이션한다.

- TierBaseline의 Allow 규칙: 모든 엔드포인트 -> backend:8080 허용
- TierNormal의 Deny 규칙: monitor -> backend:8080 차단
- 결과: monitor의 트래픽만 차단, 나머지는 허용 (상위 Tier의 Deny가 하위 Tier의 Allow를 오버라이드)

실제 Cilium 참조:
- `pkg/policy/types/policyentry.go`의 `Tier`, `Verdict`
- `pkg/policy/types/entry.go`의 `Precedence` 인코딩
- `pkg/policy/rule.go`의 `mergePortProto()` 충돌 해결 로직

### 시나리오 6: 정책 업데이트 흐름

정책 추가부터 BPF 맵 업데이트까지의 전체 흐름을 시뮬레이션한다:

1. `PolicyRepository.AddRule()` - 규칙 저장, revision 증가
2. `RegenerateAll()` - 영향받는 엔드포인트 정책 재계산
3. `computePolicyForEndpoint()` - Subject 매칭 규칙 수집 및 L4 정책 해석
4. `BPFPolicyMap.Update()` - BPF 맵에 최종 (Key, Entry) 기록

실제 Cilium 참조:
- `pkg/policy/repository.go`의 `ReplaceByResource()`, `resolvePolicyLocked()`
- `pkg/policy/resolve.go`의 `selectorPolicy.DistillPolicy()`
- `pkg/maps/policymap/policymap.go`의 `PolicyMap.Update()`

## 구현된 주요 구조체와 실제 Cilium 코드 매핑

| PoC 구조체 | 실제 Cilium 코드 |
|---|---|
| `NumericIdentity` | `pkg/identity/numeric_identity.go` |
| `PolicyKey` | `pkg/policy/types/types.go`의 `Key`, `pkg/maps/policymap/policymap.go`의 `PolicyKey` |
| `PolicyMapEntry` | `pkg/policy/types/entry.go`의 `MapStateEntry` |
| `EndpointSelector` | `pkg/policy/api/selector.go`의 `EndpointSelector` |
| `CIDRSelector` | `pkg/policy/types/selector.go`의 `CIDRSelector` |
| `HTTPRule` | `pkg/policy/api/http.go`의 `PortRuleHTTP` |
| `L7Rules` | `pkg/policy/api/l4.go`의 `L7Rules` |
| `PolicyRule` | `pkg/policy/types/policyentry.go`의 `PolicyEntry` |
| `PolicyRepository` | `pkg/policy/repository.go`의 `Repository` |
| `BPFPolicyMap` | `pkg/maps/policymap/policymap.go`의 `PolicyMap` |
| `BPFPolicyMap.Lookup()` | `bpf/lib/policy.h`의 LPM Trie 조회 로직 |
| `L7Proxy` | `pkg/proxy/envoyproxy.go`, `pkg/proxy/dns.go` |
| `DNSCache` | `pkg/fqdn/cache.go`의 `DNSCache` |
| `IdentityAllocator` | `pkg/identity/` 패키지 |

## 아키텍처 다이어그램

```
                    PolicyRepository
                    (규칙 저장소)
                         |
                         v
              computePolicyForEndpoint()
                    (정책 해석)
                    /          \
                   v            v
           Ingress 규칙    Egress 규칙
            수집/정렬        수집/정렬
                   \            /
                    v          v
              resolveDirectionRules()
               (L3 Identity 수집)
               (L4 Port 매칭)
               (L7 프록시 설정)
                         |
                         v
                  BPFPolicyMap.Update()
                   (맵 엔트리 기록)
                         |
                         v
                  BPFPolicyMap.Lookup()
                (LPM 우선순위 매칭)
```

## 제한사항

이 PoC는 교육 목적의 시뮬레이션이므로 실제 Cilium과 다음 차이점이 있다:

1. **BPF 없음**: 실제 LPM Trie BPF 맵 대신 Go map으로 시뮬레이션
2. **SelectorCache 없음**: 실제 Cilium의 점진적(incremental) 정책 업데이트 없음
3. **단순화된 Identity 할당**: 실제 Cilium의 글로벌 Identity 할당 프로토콜(KVStore) 없음
4. **Envoy 없음**: L7 프록시를 Go 함수로 시뮬레이션
5. **포트 범위 미지원**: 단일 포트만 지원 (실제 Cilium은 포트 범위의 LPM 매칭 지원)
6. **점진적 맵 업데이트 없음**: 매번 전체 재계산 (실제 Cilium은 `ConsumeMapChanges`로 차분 업데이트)
