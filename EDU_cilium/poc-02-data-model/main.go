// Cilium의 Identity 기반 정책 결정 매커니즘을 재현
//
// 핵심 흐름:
//   1. Pod 라벨 → Identity 할당 (해시 기반)
//   2. IP → Identity 매핑 (IPCache)
//   3. 정책 평가: srcIdentity → dstIdentity:port 허용 여부
//
// 실행: go run main.go
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// -----------------------------------------------------------
// 1. Label — Pod의 라벨 (Cilium 실제: pkg/labels/labels.go)
// -----------------------------------------------------------
type Label struct {
	Key    string
	Value  string
	Source string // "k8s", "reserved" 등
}

func (l Label) String() string {
	return fmt.Sprintf("%s:%s=%s", l.Source, l.Key, l.Value)
}

// -----------------------------------------------------------
// 2. Identity — 라벨 집합의 숫자 ID (Cilium 실제: pkg/identity/numericidentity.go)
// -----------------------------------------------------------
type NumericIdentity uint32

const (
	IdentityUnknown    NumericIdentity = 0
	IdentityHost       NumericIdentity = 1
	IdentityWorld      NumericIdentity = 2
	IdentityUnmanaged  NumericIdentity = 3
	IdentityHealth     NumericIdentity = 4
	IdentityInit       NumericIdentity = 5
	IdentityRemoteNode NumericIdentity = 6
)

type Identity struct {
	ID     NumericIdentity
	Labels []Label
}

// labelsToIdentity — 라벨 집합을 해싱하여 Identity ID를 생성
// Cilium 실제 동작: 라벨의 SHA256 해시로 중복 검사 후 etcd에서 ID 할당
func labelsToIdentity(labels []Label) NumericIdentity {
	// 라벨을 정렬하여 순서에 관계없이 같은 해시를 보장
	strs := make([]string, len(labels))
	for i, l := range labels {
		strs[i] = l.String()
	}
	sort.Strings(strs)

	h := sha256.Sum256([]byte(strings.Join(strs, ",")))
	// 상위 4바이트를 Identity로 사용 (실제로는 etcd에서 순차 할당)
	id := binary.BigEndian.Uint32(h[:4])
	// 예약된 범위(0~255)를 피하기 위해 오프셋
	return NumericIdentity(id%50000 + 10000)
}

// -----------------------------------------------------------
// 3. IPCache — IP → Identity 매핑 (Cilium 실제: pkg/ipcache/ipcache.go)
// -----------------------------------------------------------
type IPCacheEntry struct {
	CIDR     string
	Identity NumericIdentity
	HostIP   string
}

type IPCache struct {
	entries map[string]IPCacheEntry
}

func newIPCache() *IPCache {
	return &IPCache{entries: make(map[string]IPCacheEntry)}
}

func (c *IPCache) Upsert(cidr string, id NumericIdentity, hostIP string) {
	c.entries[cidr] = IPCacheEntry{CIDR: cidr, Identity: id, HostIP: hostIP}
}

func (c *IPCache) LookupByIP(ip string) (NumericIdentity, bool) {
	if e, ok := c.entries[ip+"/32"]; ok {
		return e.Identity, true
	}
	return IdentityUnknown, false
}

// -----------------------------------------------------------
// 4. Policy — 네트워크 정책 (Cilium 실제: pkg/policy/repository.go)
// -----------------------------------------------------------
type L4Filter struct {
	Port     uint16
	Protocol string
}

type PolicyRule struct {
	Name               string
	EndpointSelector   NumericIdentity   // 이 정책의 대상 Endpoint
	AllowedFromIdentities []NumericIdentity // 인그레스에서 허용할 Identity
	AllowedPorts       []L4Filter        // 허용 포트
}

type PolicyRepository struct {
	rules []PolicyRule
}

func (r *PolicyRepository) Add(rule PolicyRule) {
	r.rules = append(r.rules, rule)
}

// Evaluate — srcIdentity가 dstIdentity의 port에 접근 가능한지 평가
// Cilium 실제: pkg/policy/distillery.go → PolicyMapState 계산
func (r *PolicyRepository) Evaluate(srcID, dstID NumericIdentity, port uint16, proto string) string {
	for _, rule := range r.rules {
		if rule.EndpointSelector != dstID {
			continue
		}

		srcAllowed := false
		for _, allowed := range rule.AllowedFromIdentities {
			if allowed == srcID {
				srcAllowed = true
				break
			}
		}
		if !srcAllowed {
			continue
		}

		for _, p := range rule.AllowedPorts {
			if p.Port == port && p.Protocol == proto {
				return "ALLOW"
			}
		}
	}
	return "DROP"
}

// -----------------------------------------------------------
// 5. Endpoint — Pod에 대응하는 네트워크 엔드포인트
// -----------------------------------------------------------
type Endpoint struct {
	ID       uint16
	PodName  string
	Identity NumericIdentity
	IPv4     string
}

func main() {
	fmt.Println("=== Cilium Identity 기반 정책 결정 시뮬레이터 ===")
	fmt.Println()

	// ----- Step 1: Label → Identity 매핑 -----
	fmt.Println("[1] Label → Identity 매핑")
	fmt.Println("    Cilium 실제: pkg/identity/numericidentity.go")
	fmt.Println()

	frontendLabels := []Label{{Key: "app", Value: "frontend", Source: "k8s"}}
	backendLabels := []Label{{Key: "app", Value: "backend", Source: "k8s"}}
	redisLabels := []Label{{Key: "app", Value: "redis", Source: "k8s"}}

	frontendID := labelsToIdentity(frontendLabels)
	backendID := labelsToIdentity(backendLabels)
	redisID := labelsToIdentity(redisLabels)

	fmt.Printf("    Labels: {app:frontend}  → Identity %d\n", frontendID)
	fmt.Printf("    Labels: {app:backend}   → Identity %d\n", backendID)
	fmt.Printf("    Labels: {app:redis}     → Identity %d\n", redisID)
	fmt.Println()
	fmt.Println("    핵심: 같은 라벨 집합은 항상 같은 Identity를 반환한다.")
	fmt.Printf("    검증: {app:frontend} 재계산 → Identity %d (동일)\n", labelsToIdentity(frontendLabels))
	fmt.Println()

	// ----- Step 2: IPCache 구성 -----
	fmt.Println("[2] IPCache 구성 (IP → Identity)")
	fmt.Println("    Cilium 실제: pkg/ipcache/ipcache.go")
	fmt.Println()

	ipcache := newIPCache()
	ipcache.Upsert("10.0.1.5/32", frontendID, "192.168.1.1")
	ipcache.Upsert("10.0.1.10/32", backendID, "192.168.1.1")
	ipcache.Upsert("10.0.1.15/32", redisID, "192.168.1.2")

	for cidr, entry := range ipcache.entries {
		fmt.Printf("    %-16s → Identity %d (host: %s)\n", cidr, entry.Identity, entry.HostIP)
	}
	fmt.Println()

	// 패킷 도착 시 IP로 Identity를 찾는 과정
	fmt.Println("    패킷 수신 시 IP→Identity 변환:")
	if id, ok := ipcache.LookupByIP("10.0.1.5"); ok {
		fmt.Printf("    10.0.1.5 → Identity %d (frontend)\n", id)
	}
	fmt.Println()

	// ----- Step 3: 정책 정의 및 평가 -----
	fmt.Println("[3] 정책 평가")
	fmt.Println("    Cilium 실제: pkg/policy/repository.go")
	fmt.Println()

	repo := &PolicyRepository{}

	// backend는 frontend의 80 접속을 허용
	repo.Add(PolicyRule{
		Name:                  "allow-frontend-to-backend",
		EndpointSelector:      backendID,
		AllowedFromIdentities: []NumericIdentity{frontendID},
		AllowedPorts:          []L4Filter{{Port: 80, Protocol: "TCP"}},
	})

	// redis는 backend의 6379 접속을 허용
	repo.Add(PolicyRule{
		Name:                  "allow-backend-to-redis",
		EndpointSelector:      redisID,
		AllowedFromIdentities: []NumericIdentity{backendID},
		AllowedPorts:          []L4Filter{{Port: 6379, Protocol: "TCP"}},
	})

	// frontend는 world(외부)의 80 접속을 허용
	repo.Add(PolicyRule{
		Name:                  "allow-world-to-frontend",
		EndpointSelector:      frontendID,
		AllowedFromIdentities: []NumericIdentity{IdentityWorld},
		AllowedPorts:          []L4Filter{{Port: 80, Protocol: "TCP"}},
	})

	// 평가 실행
	tests := []struct {
		srcName string
		srcID   NumericIdentity
		dstName string
		dstID   NumericIdentity
		port    uint16
		proto   string
	}{
		{"frontend", frontendID, "backend", backendID, 80, "TCP"},
		{"frontend", frontendID, "redis", redisID, 6379, "TCP"},
		{"backend", backendID, "redis", redisID, 6379, "TCP"},
		{"world", IdentityWorld, "frontend", frontendID, 80, "TCP"},
		{"world", IdentityWorld, "backend", backendID, 80, "TCP"},
		{"frontend", frontendID, "backend", backendID, 443, "TCP"},
	}

	for _, t := range tests {
		result := repo.Evaluate(t.srcID, t.dstID, t.port, t.proto)
		symbol := ">>"
		reason := ""
		if result == "DROP" {
			symbol = "xx"
			reason = " (정책 없음)"
		}
		fmt.Printf("    %s %s(%d) → %s(%d):%d/%s  = %s%s\n",
			symbol, t.srcName, t.srcID, t.dstName, t.dstID, t.port, t.proto, result, reason)
	}

	fmt.Println()
	fmt.Println("[요약]")
	fmt.Println("  - IP가 바뀌어도 라벨이 같으면 Identity가 동일 → 정책 유지")
	fmt.Println("  - 정책 평가는 srcIdentity + dstIdentity + port로 결정")
	fmt.Println("  - 이 결과가 BPF PolicyMap에 기록되어 커널에서 패킷 단위로 적용됨")
}
