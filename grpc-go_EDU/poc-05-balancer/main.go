// poc-05-balancer: pick_first/round_robin 밸런서
//
// gRPC의 클라이언트 사이드 로드 밸런싱을 시뮬레이션한다.
// - Balancer, Picker, SubConn 인터페이스
// - pick_first: 첫 번째 연결된 주소만 사용
// - round_robin: 활성 SubConn을 순환 선택
// - 주소 목록 변경 시 밸런서 업데이트
//
// 실제 gRPC 참조:
//   balancer/balancer.go              → Balancer, Picker, SubConn 인터페이스
//   balancer/pickfirst/pickfirst.go   → pick_first 구현
//   balancer/roundrobin/roundrobin.go → round_robin 구현
//   balancer_wrapper.go               → ccBalancerWrapper

package main

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
)

// ──────────────────────────────────────────────
// 1. 핵심 인터페이스 (balancer.go 참조)
// ──────────────────────────────────────────────

// ConnectivityState는 SubConn 연결 상태.
// 실제: connectivity/connectivity.go
type ConnectivityState int

const (
	Idle       ConnectivityState = iota
	Connecting
	Ready
	TransientFailure
	Shutdown
)

func (s ConnectivityState) String() string {
	switch s {
	case Idle:
		return "IDLE"
	case Connecting:
		return "CONNECTING"
	case Ready:
		return "READY"
	case TransientFailure:
		return "TRANSIENT_FAILURE"
	case Shutdown:
		return "SHUTDOWN"
	default:
		return "UNKNOWN"
	}
}

// Address는 백엔드 서버 주소.
// 실제: resolver/resolver.go의 Address
type Address struct {
	Addr string
}

// SubConn은 하나의 백엔드 서버에 대한 연결.
// 실제: balancer/balancer.go의 SubConn 인터페이스
type SubConn struct {
	addr  Address
	state ConnectivityState
	id    int
}

func (sc *SubConn) String() string {
	return fmt.Sprintf("SubConn{%s, %s}", sc.addr.Addr, sc.state)
}

// PickResult는 Pick의 결과.
// 실제: balancer/balancer.go의 PickResult
type PickResult struct {
	SubConn *SubConn
}

// Picker는 RPC 요청을 어느 SubConn으로 보낼지 결정한다.
// 실제: balancer/balancer.go:313
type Picker interface {
	Pick() (PickResult, error)
}

// ClientConnState는 리졸버가 밸런서에게 전달하는 주소 목록.
// 실제: balancer/balancer.go의 ClientConnState
type ClientConnState struct {
	ResolverState ResolverState
}

type ResolverState struct {
	Addresses []Address
}

// Balancer는 로드 밸런싱 정책을 구현하는 인터페이스.
// 실제: balancer/balancer.go:344
type Balancer interface {
	UpdateClientConnState(ClientConnState) error
	Close()
	Name() string
}

// ClientConn은 밸런서가 SubConn을 관리하기 위해 사용하는 인터페이스.
// 실제: balancer/balancer.go의 ClientConn 인터페이스
type ClientConn struct {
	mu      sync.Mutex
	subConns []*SubConn
	picker   Picker
	nextID   int
}

func (cc *ClientConn) NewSubConn(addr Address) *SubConn {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.nextID++
	sc := &SubConn{addr: addr, state: Idle, id: cc.nextID}
	cc.subConns = append(cc.subConns, sc)
	return sc
}

func (cc *ClientConn) RemoveSubConn(sc *SubConn) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for i, s := range cc.subConns {
		if s.id == sc.id {
			cc.subConns = append(cc.subConns[:i], cc.subConns[i+1:]...)
			s.state = Shutdown
			return
		}
	}
}

func (cc *ClientConn) UpdatePicker(p Picker) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.picker = p
}

func (cc *ClientConn) Pick() (PickResult, error) {
	cc.mu.Lock()
	p := cc.picker
	cc.mu.Unlock()
	if p == nil {
		return PickResult{}, fmt.Errorf("picker가 없음")
	}
	return p.Pick()
}

// ──────────────────────────────────────────────
// 2. pick_first 밸런서 (pickfirst.go 참조)
// ──────────────────────────────────────────────

// pickFirstBalancer는 첫 번째 연결 가능한 주소만 사용한다.
// 실제: balancer/pickfirst/pickfirst.go
// 동작: 주소 목록의 첫 번째부터 순서대로 연결 시도, 성공하면 해당 SubConn만 사용
type pickFirstBalancer struct {
	cc     *ClientConn
	subConn *SubConn
}

func newPickFirst(cc *ClientConn) *pickFirstBalancer {
	return &pickFirstBalancer{cc: cc}
}

func (b *pickFirstBalancer) Name() string { return "pick_first" }

func (b *pickFirstBalancer) UpdateClientConnState(state ClientConnState) error {
	addrs := state.ResolverState.Addresses
	if len(addrs) == 0 {
		return fmt.Errorf("주소 목록이 비어있음")
	}

	// 기존 SubConn이 있으면 제거
	if b.subConn != nil {
		b.cc.RemoveSubConn(b.subConn)
	}

	// 첫 번째 주소로 SubConn 생성
	sc := b.cc.NewSubConn(addrs[0])
	b.subConn = sc

	// 연결 시뮬레이션
	sc.state = Connecting
	fmt.Printf("  [pick_first] %s에 연결 시도...\n", sc.addr.Addr)
	sc.state = Ready
	fmt.Printf("  [pick_first] %s 연결 성공 → READY\n", sc.addr.Addr)

	// Picker 업데이트: 항상 이 SubConn만 반환
	b.cc.UpdatePicker(&pickFirstPicker{sc: sc})
	return nil
}

func (b *pickFirstBalancer) Close() {
	if b.subConn != nil {
		b.cc.RemoveSubConn(b.subConn)
	}
}

// pickFirstPicker는 항상 동일한 SubConn을 반환한다.
type pickFirstPicker struct {
	sc *SubConn
}

func (p *pickFirstPicker) Pick() (PickResult, error) {
	if p.sc.state != Ready {
		return PickResult{}, fmt.Errorf("SubConn이 READY 상태가 아님")
	}
	return PickResult{SubConn: p.sc}, nil
}

// ──────────────────────────────────────────────
// 3. round_robin 밸런서 (roundrobin.go 참조)
// ──────────────────────────────────────────────

// roundRobinBalancer는 활성 SubConn을 순환하며 선택한다.
// 실제: balancer/roundrobin/roundrobin.go
type roundRobinBalancer struct {
	cc       *ClientConn
	subConns []*SubConn
}

func newRoundRobin(cc *ClientConn) *roundRobinBalancer {
	return &roundRobinBalancer{cc: cc}
}

func (b *roundRobinBalancer) Name() string { return "round_robin" }

func (b *roundRobinBalancer) UpdateClientConnState(state ClientConnState) error {
	addrs := state.ResolverState.Addresses
	if len(addrs) == 0 {
		return fmt.Errorf("주소 목록이 비어있음")
	}

	// 기존 SubConn 정리
	for _, sc := range b.subConns {
		b.cc.RemoveSubConn(sc)
	}
	b.subConns = nil

	// 모든 주소에 SubConn 생성
	var readyConns []*SubConn
	for _, addr := range addrs {
		sc := b.cc.NewSubConn(addr)
		b.subConns = append(b.subConns, sc)

		// 연결 시뮬레이션 (일부 실패 가능)
		sc.state = Connecting
		if rand.Intn(10) < 8 { // 80% 성공률
			sc.state = Ready
			readyConns = append(readyConns, sc)
			fmt.Printf("  [round_robin] %s → READY\n", sc.addr.Addr)
		} else {
			sc.state = TransientFailure
			fmt.Printf("  [round_robin] %s → TRANSIENT_FAILURE\n", sc.addr.Addr)
		}
	}

	if len(readyConns) == 0 {
		return fmt.Errorf("연결 가능한 서버 없음")
	}

	// Picker 업데이트: Ready SubConn들을 순환
	b.cc.UpdatePicker(&roundRobinPicker{subConns: readyConns})
	return nil
}

func (b *roundRobinBalancer) Close() {
	for _, sc := range b.subConns {
		b.cc.RemoveSubConn(sc)
	}
}

// roundRobinPicker는 Ready SubConn들을 순환하며 선택한다.
// 실제: atomic 카운터로 인덱스를 순환 (락 없는 구현)
type roundRobinPicker struct {
	subConns []*SubConn
	next     atomic.Uint64
}

func (p *roundRobinPicker) Pick() (PickResult, error) {
	if len(p.subConns) == 0 {
		return PickResult{}, fmt.Errorf("사용 가능한 SubConn 없음")
	}
	idx := p.next.Add(1) - 1
	sc := p.subConns[idx%uint64(len(p.subConns))]
	return PickResult{SubConn: sc}, nil
}

// ──────────────────────────────────────────────
// 4. main
// ──────────────────────────────────────────────

func main() {
	fmt.Println("=== pick_first / round_robin 밸런서 시뮬레이션 ===")
	fmt.Println()

	addresses := []Address{
		{Addr: "10.0.0.1:8080"},
		{Addr: "10.0.0.2:8080"},
		{Addr: "10.0.0.3:8080"},
		{Addr: "10.0.0.4:8080"},
	}

	// === pick_first ===
	fmt.Println("── 1. pick_first 밸런서 ──")
	cc1 := &ClientConn{}
	pf := newPickFirst(cc1)

	pf.UpdateClientConnState(ClientConnState{
		ResolverState: ResolverState{Addresses: addresses},
	})

	fmt.Println()
	fmt.Println("  10번 Pick 결과:")
	for i := 0; i < 10; i++ {
		result, err := cc1.Pick()
		if err != nil {
			fmt.Printf("  Pick #%d: 에러 - %v\n", i+1, err)
			continue
		}
		fmt.Printf("    Pick #%d → %s\n", i+1, result.SubConn.addr.Addr)
	}

	// 주소 변경 시뮬레이션
	fmt.Println()
	fmt.Println("  주소 목록 변경:")
	pf.UpdateClientConnState(ClientConnState{
		ResolverState: ResolverState{Addresses: []Address{
			{Addr: "10.0.0.5:8080"},
			{Addr: "10.0.0.6:8080"},
		}},
	})
	result, _ := cc1.Pick()
	fmt.Printf("  변경 후 Pick → %s\n", result.SubConn.addr.Addr)
	pf.Close()

	// === round_robin ===
	fmt.Println()
	fmt.Println("── 2. round_robin 밸런서 ──")
	cc2 := &ClientConn{}
	rr := newRoundRobin(cc2)

	rr.UpdateClientConnState(ClientConnState{
		ResolverState: ResolverState{Addresses: addresses},
	})

	fmt.Println()
	fmt.Println("  12번 Pick 결과 (순환 확인):")
	distribution := make(map[string]int)
	for i := 0; i < 12; i++ {
		result, err := cc2.Pick()
		if err != nil {
			fmt.Printf("    Pick #%d: 에러 - %v\n", i+1, err)
			continue
		}
		addr := result.SubConn.addr.Addr
		distribution[addr]++
		fmt.Printf("    Pick #%02d → %s\n", i+1, addr)
	}

	fmt.Println()
	fmt.Println("  분배 통계:")
	for addr, count := range distribution {
		fmt.Printf("    %s: %d회\n", addr, count)
	}

	// 주소 추가 시뮬레이션
	fmt.Println()
	fmt.Println("  서버 추가 (주소 목록 업데이트):")
	rr.UpdateClientConnState(ClientConnState{
		ResolverState: ResolverState{Addresses: append(addresses,
			Address{Addr: "10.0.0.5:8080"},
			Address{Addr: "10.0.0.6:8080"},
		)},
	})

	fmt.Println()
	fmt.Println("  업데이트 후 6번 Pick:")
	for i := 0; i < 6; i++ {
		result, err := cc2.Pick()
		if err != nil {
			fmt.Printf("    Pick #%d: 에러 - %v\n", i+1, err)
			continue
		}
		fmt.Printf("    Pick #%d → %s\n", i+1, result.SubConn.addr.Addr)
	}

	rr.Close()
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
