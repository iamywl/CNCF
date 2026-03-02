// Cilium Hive DI 프레임워크의 동작 원리를 간소화하여 재현
//
// Cilium 실제 코드:
//   - Hive 코어: github.com/cilium/hive (외부 패키지)
//   - Cell 등록: daemon/cmd/cells.go
//   - 생명주기: hive.Lifecycle (Start/Stop Hook)
//
// 실행: go run main.go
package main

import (
	"fmt"
	"strings"
)

// -----------------------------------------------------------
// Lifecycle — Hive의 컴포넌트 생명주기 관리
// Cilium 실제: github.com/cilium/hive/cell.Lifecycle
// -----------------------------------------------------------
type LifecycleHook struct {
	Name    string
	OnStart func() error
	OnStop  func() error
}

type Lifecycle struct {
	hooks []LifecycleHook
}

func (l *Lifecycle) Append(hook LifecycleHook) {
	l.hooks = append(l.hooks, hook)
}

func (l *Lifecycle) Start() error {
	fmt.Println("\n[Lifecycle] Start 실행 (의존성 순서대로)")
	fmt.Println(strings.Repeat("─", 50))
	for _, h := range l.hooks {
		fmt.Printf("  ▶ Starting: %s\n", h.Name)
		if err := h.OnStart(); err != nil {
			return fmt.Errorf("%s start 실패: %w", h.Name, err)
		}
	}
	return nil
}

func (l *Lifecycle) Stop() error {
	fmt.Println("\n[Lifecycle] Stop 실행 (역순)")
	fmt.Println(strings.Repeat("─", 50))
	// 역순으로 종료 — 의존하는 컴포넌트부터 먼저 정리
	for i := len(l.hooks) - 1; i >= 0; i-- {
		h := l.hooks[i]
		fmt.Printf("  ■ Stopping: %s\n", h.Name)
		if err := h.OnStop(); err != nil {
			return fmt.Errorf("%s stop 실패: %w", h.Name, err)
		}
	}
	return nil
}

// -----------------------------------------------------------
// 컴포넌트들 — Cilium daemon의 실제 Cell을 단순화
// -----------------------------------------------------------

// KVStore — etcd 연결 (Cilium 실제: pkg/kvstore/etcd.go)
type KVStore struct {
	connected bool
}

func NewKVStore(lc *Lifecycle) *KVStore {
	kv := &KVStore{}
	lc.Append(LifecycleHook{
		Name: "KVStore (etcd)",
		OnStart: func() error {
			kv.connected = true
			fmt.Println("    → etcd 연결 완료")
			return nil
		},
		OnStop: func() error {
			kv.connected = false
			fmt.Println("    → etcd 연결 해제")
			return nil
		},
	})
	return kv
}

func (kv *KVStore) AllocateIdentity(labels string) uint32 {
	fmt.Printf("    [KVStore] Identity 할당: %s → 48312\n", labels)
	return 48312
}

// EndpointManager — Endpoint 관리 (Cilium 실제: pkg/endpointmanager/manager.go)
// 의존성: KVStore (Identity 할당에 필요)
type EndpointManager struct {
	kvstore   *KVStore
	endpoints map[uint16]string
}

func NewEndpointManager(lc *Lifecycle, kv *KVStore) *EndpointManager {
	em := &EndpointManager{kvstore: kv, endpoints: make(map[uint16]string)}
	lc.Append(LifecycleHook{
		Name: "EndpointManager",
		OnStart: func() error {
			fmt.Println("    → Endpoint 관리자 초기화")
			return nil
		},
		OnStop: func() error {
			fmt.Printf("    → %d개 Endpoint 정리 완료\n", len(em.endpoints))
			return nil
		},
	})
	return em
}

func (em *EndpointManager) CreateEndpoint(id uint16, podName string) {
	em.endpoints[id] = podName
	em.kvstore.AllocateIdentity(podName)
	fmt.Printf("    [EndpointManager] Endpoint %d 생성: %s\n", id, podName)
}

// PolicyEngine — 정책 엔진 (Cilium 실제: pkg/policy/repository.go)
// 의존성: KVStore, EndpointManager
type PolicyEngine struct {
	kvstore    *KVStore
	epManager  *EndpointManager
	ruleCount  int
}

func NewPolicyEngine(lc *Lifecycle, kv *KVStore, em *EndpointManager) *PolicyEngine {
	pe := &PolicyEngine{kvstore: kv, epManager: em}
	lc.Append(LifecycleHook{
		Name: "PolicyEngine",
		OnStart: func() error {
			fmt.Println("    → 정책 엔진 초기화, 기존 정책 로딩 중...")
			return nil
		},
		OnStop: func() error {
			fmt.Printf("    → %d개 정책 규칙 해제\n", pe.ruleCount)
			return nil
		},
	})
	return pe
}

func (pe *PolicyEngine) AddRule(name string) {
	pe.ruleCount++
	fmt.Printf("    [PolicyEngine] 규칙 추가: %s (총 %d개)\n", name, pe.ruleCount)
}

// HubbleObserver — Flow 관측 (Cilium 실제: pkg/hubble/)
// 의존성: EndpointManager (Endpoint 정보 조회에 필요)
type HubbleObserver struct {
	epManager *EndpointManager
	flowCount int
}

func NewHubbleObserver(lc *Lifecycle, em *EndpointManager) *HubbleObserver {
	ho := &HubbleObserver{epManager: em}
	lc.Append(LifecycleHook{
		Name: "HubbleObserver",
		OnStart: func() error {
			fmt.Println("    → Hubble Observer 시작, perf ring buffer 연결")
			return nil
		},
		OnStop: func() error {
			fmt.Printf("    → Hubble Observer 종료 (수집한 Flow: %d개)\n", ho.flowCount)
			return nil
		},
	})
	return ho
}

func (ho *HubbleObserver) RecordFlow(verdict string) {
	ho.flowCount++
	fmt.Printf("    [HubbleObserver] Flow 기록: %s (총 %d개)\n", verdict, ho.flowCount)
}

// -----------------------------------------------------------
// Hive — 의존성 주입 컨테이너
// -----------------------------------------------------------
func main() {
	fmt.Println("=== Cilium Hive DI 프레임워크 시뮬레이터 ===")
	fmt.Println()
	fmt.Println("실제 코드 위치:")
	fmt.Println("  Hive 코어:   github.com/cilium/hive")
	fmt.Println("  Cell 등록:   daemon/cmd/cells.go")
	fmt.Println("  Lifecycle:   hive.Lifecycle")
	fmt.Println()

	// ----- Step 1: 의존성 그래프 구성 -----
	fmt.Println("[1] 컴포넌트 등록 (의존성 주입)")
	fmt.Println(strings.Repeat("─", 50))
	fmt.Println("  의존성 그래프:")
	fmt.Println("  KVStore ──► EndpointManager ──► HubbleObserver")
	fmt.Println("      │              │")
	fmt.Println("      └──────┬───────┘")
	fmt.Println("             ▼")
	fmt.Println("       PolicyEngine")
	fmt.Println()

	lc := &Lifecycle{}

	// 생성자 호출 — Hive가 자동으로 의존성 순서를 결정
	// 실제 Cilium에서는 cell.Provide()로 등록하면 Hive가 자동 해결
	fmt.Println("  컴포넌트 생성 (Hive가 의존성 순서대로):")
	kvstore := NewKVStore(lc)
	epManager := NewEndpointManager(lc, kvstore)
	policyEngine := NewPolicyEngine(lc, kvstore, epManager)
	hubble := NewHubbleObserver(lc, epManager)

	// ----- Step 2: Start (의존성 순서대로) -----
	if err := lc.Start(); err != nil {
		fmt.Printf("Start 실패: %v\n", err)
		return
	}

	// ----- Step 3: 실제 동작 시뮬레이션 -----
	fmt.Println("\n[2] 동작 시뮬레이션")
	fmt.Println(strings.Repeat("─", 50))

	epManager.CreateEndpoint(1234, "frontend-7b4d8")
	policyEngine.AddRule("allow-frontend-to-backend")
	hubble.RecordFlow("FORWARDED")
	hubble.RecordFlow("DROPPED")

	// ----- Step 4: Stop (역순) -----
	if err := lc.Stop(); err != nil {
		fmt.Printf("Stop 실패: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("[요약]")
	fmt.Println("  - Hive는 Provide()로 등록된 생성자의 파라미터를 분석하여 의존성 그래프를 구성")
	fmt.Println("  - Start는 의존성 순서대로 (KVStore → EndpointMgr → PolicyEngine → Hubble)")
	fmt.Println("  - Stop은 역순으로 (Hubble → PolicyEngine → EndpointMgr → KVStore)")
	fmt.Println("  - 이렇게 하면 컴포넌트가 수십 개여도 순서 실수가 없다")

	// suppress unused warnings
	_ = policyEngine
	_ = hubble
}
