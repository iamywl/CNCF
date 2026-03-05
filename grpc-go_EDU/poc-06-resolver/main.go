// poc-06-resolver: 이름 해석 및 주소 갱신
//
// gRPC의 이름 해석(Name Resolution) 시스템을 시뮬레이션한다.
// - Builder, Resolver 인터페이스 구현
// - DNS 리졸버: 주기적으로 주소를 갱신
// - Passthrough 리졸버: 주소를 그대로 전달
// - 스킴 기반 Builder 레지스트리
// - 리졸버 → 밸런서 업데이트 흐름
//
// 실제 gRPC 참조:
//   resolver/resolver.go               → Builder, Resolver, Target, ClientConn 인터페이스
//   internal/resolver/dns/dns_resolver.go → DNS 리졸버 구현
//   internal/resolver/passthrough/      → Passthrough 리졸버
//   resolver/map.go                     → resolverBuilder 레지스트리

package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ──────────────────────────────────────────────
// 1. 핵심 인터페이스 (resolver/resolver.go 참조)
// ──────────────────────────────────────────────

// Address는 백엔드 서버 주소.
// 실제: resolver/resolver.go의 Address 구조체
type Address struct {
	Addr       string
	ServerName string // TLS용 서버명
}

// Target은 파싱된 대상 URI.
// 실제: resolver/resolver.go의 Target — Scheme + Authority + Endpoint
type Target struct {
	Scheme    string // "dns", "passthrough", "xds" 등
	Authority string // DNS 서버 등
	Endpoint  string // 실제 서비스 이름/주소
}

// ParseTarget은 URI를 파싱한다.
// 형식: scheme://authority/endpoint
func ParseTarget(uri string) Target {
	// scheme:// 분리
	if idx := strings.Index(uri, "://"); idx >= 0 {
		scheme := uri[:idx]
		rest := uri[idx+3:]
		// authority/endpoint 분리
		if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
			return Target{
				Scheme:    scheme,
				Authority: rest[:slashIdx],
				Endpoint:  rest[slashIdx+1:],
			}
		}
		return Target{Scheme: scheme, Endpoint: rest}
	}
	// scheme이 없으면 passthrough
	return Target{Scheme: "passthrough", Endpoint: uri}
}

// State는 리졸버가 밸런서에게 전달하는 해석 결과.
// 실제: resolver/resolver.go의 State
type State struct {
	Addresses []Address
}

// ClientConn은 리졸버가 결과를 보고하는 인터페이스.
// 실제: resolver/resolver.go의 ClientConn 인터페이스
type ClientConn interface {
	UpdateState(State) error
	ReportError(error)
}

// ResolveNowOptions는 ResolveNow 호출 옵션.
type ResolveNowOptions struct{}

// Resolver는 이름을 주소로 해석하는 인터페이스.
// 실제: resolver/resolver.go:319
type Resolver interface {
	ResolveNow(ResolveNowOptions)
	Close()
}

// Builder는 Resolver를 생성하는 팩토리 인터페이스.
// 실제: resolver/resolver.go:301
type Builder interface {
	Build(target Target, cc ClientConn) (Resolver, error)
	Scheme() string
}

// ──────────────────────────────────────────────
// 2. Builder 레지스트리 (resolver/map.go 참조)
// ──────────────────────────────────────────────

// 실제: resolver/resolver.go의 Register/Get 함수와 m(맵)
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Builder)
)

func Register(b Builder) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[b.Scheme()] = b
	fmt.Printf("[레지스트리] Builder 등록: scheme=%s\n", b.Scheme())
}

func Get(scheme string) Builder {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[scheme]
}

// ──────────────────────────────────────────────
// 3. DNS 리졸버 (dns_resolver.go 참조)
// ──────────────────────────────────────────────

// dnsBuilder는 DNS 리졸버를 생성하는 Builder.
// 실제: internal/resolver/dns/dns_resolver.go의 dnsBuilder
type dnsBuilder struct{}

func (b *dnsBuilder) Scheme() string { return "dns" }

func (b *dnsBuilder) Build(target Target, cc ClientConn) (Resolver, error) {
	r := &dnsResolver{
		host:     target.Endpoint,
		cc:       cc,
		freq:     500 * time.Millisecond, // 시뮬레이션용 짧은 주기
		done:     make(chan struct{}),
		dnsTable: simulatedDNS, // 시뮬레이션용 DNS 테이블
	}
	// 최초 해석
	r.resolve()
	// 주기적 갱신 시작
	go r.watcher()
	return r, nil
}

// dnsResolver는 DNS 기반 리졸버.
// 실제: dns_resolver.go의 dnsResolver
type dnsResolver struct {
	host     string
	cc       ClientConn
	freq     time.Duration // 갱신 주기 (실제 기본값: 30분)
	done     chan struct{}
	dnsTable map[string][]string // 시뮬레이션용 DNS 레코드
	mu       sync.Mutex
}

func (r *dnsResolver) resolve() {
	r.mu.Lock()
	ips, ok := r.dnsTable[r.host]
	r.mu.Unlock()

	if !ok {
		r.cc.ReportError(fmt.Errorf("DNS 해석 실패: %s", r.host))
		return
	}

	var addrs []Address
	for _, ip := range ips {
		addrs = append(addrs, Address{Addr: ip, ServerName: r.host})
	}
	r.cc.UpdateState(State{Addresses: addrs})
}

// watcher는 주기적으로 DNS를 다시 해석한다.
// 실제: dns_resolver.go의 watcher 메서드
func (r *dnsResolver) watcher() {
	ticker := time.NewTicker(r.freq)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.resolve()
		case <-r.done:
			return
		}
	}
}

func (r *dnsResolver) ResolveNow(ResolveNowOptions) {
	// 즉시 해석 트리거
	go r.resolve()
}

func (r *dnsResolver) Close() {
	close(r.done)
}

// UpdateDNS는 시뮬레이션용 DNS 레코드를 업데이트한다.
func (r *dnsResolver) UpdateDNS(ips []string) {
	r.mu.Lock()
	r.dnsTable[r.host] = ips
	r.mu.Unlock()
}

// ──────────────────────────────────────────────
// 4. Passthrough 리졸버 (passthrough.go 참조)
// ──────────────────────────────────────────────

// passthroughBuilder는 주소를 그대로 전달하는 Builder.
// 실제: internal/resolver/passthrough/passthrough.go
type passthroughBuilder struct{}

func (b *passthroughBuilder) Scheme() string { return "passthrough" }

func (b *passthroughBuilder) Build(target Target, cc ClientConn) (Resolver, error) {
	// 주소를 그대로 전달 (해석 없음)
	addr := target.Endpoint
	cc.UpdateState(State{Addresses: []Address{{Addr: addr}}})
	return &passthroughResolver{}, nil
}

type passthroughResolver struct{}

func (r *passthroughResolver) ResolveNow(ResolveNowOptions) {} // no-op
func (r *passthroughResolver) Close()                        {} // no-op

// ──────────────────────────────────────────────
// 5. 시뮬레이션용 밸런서 (리졸버 결과 수신)
// ──────────────────────────────────────────────

// mockBalancerCC는 ClientConn을 구현하여 리졸버 결과를 수신한다.
// 실제: balancer_wrapper.go의 ccBalancerWrapper
type mockBalancerCC struct {
	name    string
	mu      sync.Mutex
	updates int
}

func (m *mockBalancerCC) UpdateState(state State) error {
	m.mu.Lock()
	m.updates++
	count := m.updates
	m.mu.Unlock()

	addrs := make([]string, len(state.Addresses))
	for i, a := range state.Addresses {
		addrs[i] = a.Addr
	}
	fmt.Printf("  [%s] 업데이트 #%d: 주소=%v\n", m.name, count, addrs)
	return nil
}

func (m *mockBalancerCC) ReportError(err error) {
	fmt.Printf("  [%s] 에러: %v\n", m.name, err)
}

// ──────────────────────────────────────────────
// 6. 시뮬레이션용 DNS 테이블
// ──────────────────────────────────────────────

var simulatedDNS = map[string][]string{
	"myservice.example.com": {
		"10.0.1.1:8080",
		"10.0.1.2:8080",
		"10.0.1.3:8080",
	},
}

// ──────────────────────────────────────────────
// 7. main
// ──────────────────────────────────────────────

func main() {
	fmt.Println("=== 이름 해석 및 주소 갱신 시뮬레이션 ===")
	fmt.Println()

	// Builder 등록
	fmt.Println("── 1. Builder 레지스트리 ──")
	Register(&dnsBuilder{})
	Register(&passthroughBuilder{})

	// Target 파싱 테스트
	fmt.Println()
	fmt.Println("── 2. Target 파싱 ──")
	targets := []string{
		"dns:///myservice.example.com",
		"passthrough:///10.0.0.1:8080",
		"10.0.0.2:9090",
	}
	for _, uri := range targets {
		t := ParseTarget(uri)
		fmt.Printf("  '%s' → scheme=%s, authority='%s', endpoint='%s'\n",
			uri, t.Scheme, t.Authority, t.Endpoint)
	}

	// Passthrough 리졸버
	fmt.Println()
	fmt.Println("── 3. Passthrough 리졸버 ──")
	ptTarget := ParseTarget("passthrough:///10.0.0.1:8080")
	ptCC := &mockBalancerCC{name: "passthrough"}
	ptBuilder := Get(ptTarget.Scheme)
	ptResolver, _ := ptBuilder.Build(ptTarget, ptCC)
	ptResolver.Close()

	// DNS 리졸버
	fmt.Println()
	fmt.Println("── 4. DNS 리졸버 (초기 해석) ──")
	dnsTarget := ParseTarget("dns:///myservice.example.com")
	dnsCC := &mockBalancerCC{name: "DNS"}
	dnsBuilder := Get(dnsTarget.Scheme)
	resolver, _ := dnsBuilder.Build(dnsTarget, dnsCC)
	dnsResolver := resolver.(*dnsResolver)

	// DNS 레코드 변경 시뮬레이션
	fmt.Println()
	fmt.Println("── 5. DNS 레코드 변경 (스케일 아웃) ──")
	time.Sleep(100 * time.Millisecond)

	dnsResolver.UpdateDNS([]string{
		"10.0.1.1:8080",
		"10.0.1.2:8080",
		"10.0.1.3:8080",
		"10.0.1.4:8080", // 신규 서버 추가
		"10.0.1.5:8080", // 신규 서버 추가
	})

	// 주기적 갱신 대기 (500ms 주기)
	time.Sleep(600 * time.Millisecond)

	// ResolveNow 호출 (힌트: 즉시 재해석)
	fmt.Println()
	fmt.Println("── 6. ResolveNow (즉시 재해석) ──")
	dnsResolver.UpdateDNS([]string{
		"10.0.1.1:8080",
		"10.0.1.5:8080",
		// 10.0.1.2~4 제거 (스케일 인)
	})
	dnsResolver.ResolveNow(ResolveNowOptions{})
	time.Sleep(100 * time.Millisecond)

	// DNS 해석 실패 시뮬레이션
	fmt.Println()
	fmt.Println("── 7. DNS 해석 실패 ──")
	failTarget := ParseTarget("dns:///unknown-host.example.com")
	failCC := &mockBalancerCC{name: "DNS-fail"}
	failBuilder := Get(failTarget.Scheme)
	failResolver, _ := failBuilder.Build(failTarget, failCC)
	failResolver.Close()

	// 커스텀 리졸버 (static 주소 목록)
	fmt.Println()
	fmt.Println("── 8. 커스텀 리졸버 (static) ──")
	Register(&staticBuilder{})
	staticTarget := ParseTarget("static:///server1:8080,server2:8080,server3:8080")
	staticCC := &mockBalancerCC{name: "static"}
	staticBuilder := Get(staticTarget.Scheme)
	staticResolver, _ := staticBuilder.Build(staticTarget, staticCC)
	staticResolver.Close()

	dnsResolver.Close()
	time.Sleep(50 * time.Millisecond)

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}

// ──────────────────────────────────────────────
// 8. 커스텀 리졸버: static (고정 주소 목록)
// ──────────────────────────────────────────────

// staticBuilder는 커스텀 리졸버를 보여주기 위한 예시.
// gRPC는 Builder를 등록하면 어떤 이름 해석 전략이든 구현 가능하다.
type staticBuilder struct{}

func (b *staticBuilder) Scheme() string { return "static" }

func (b *staticBuilder) Build(target Target, cc ClientConn) (Resolver, error) {
	// 콤마로 분리된 주소 목록 파싱
	parts := strings.Split(target.Endpoint, ",")
	var addrs []Address

	_ = rand.Int() // 사용하지 않는 import 방지

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			addrs = append(addrs, Address{Addr: p})
		}
	}
	cc.UpdateState(State{Addresses: addrs})
	return &passthroughResolver{}, nil
}
