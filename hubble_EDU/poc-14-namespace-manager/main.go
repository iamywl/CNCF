package main

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// =============================================================================
// Hubble 네임스페이스 매니저 시뮬레이션
//
// 실제 구현 참조:
//   cilium/pkg/hubble/observer/namespace/manager.go   - namespaceManager
//   cilium/pkg/hubble/observer/namespace/defaults.go  - cleanupInterval, namespaceTTL
//   cilium/pkg/hubble/observer/local_observer.go      - trackNamespaces()
//
// 핵심 개념:
//   1. Manager 인터페이스: GetNamespaces/AddNamespace
//   2. TTL 캐시: namespaceTTL=1시간, 초과시 자동 정리
//   3. cleanupNamespaces: 5분 간격 주기적 GC
//   4. trackNamespaces: Flow에서 src/dst 네임스페이스 자동 추출
// =============================================================================

// --- Namespace 데이터 모델 ---
// 실제: observerpb.Namespace

type Namespace struct {
	Namespace string
	Cluster   string
}

func (n *Namespace) String() string {
	return fmt.Sprintf("%s/%s", n.Cluster, n.Namespace)
}

// --- NamespaceRecord ---
// 실제: namespace.namespaceRecord

type NamespaceRecord struct {
	namespace *Namespace
	added     time.Time
}

// --- Manager 인터페이스 ---
// 실제: namespace.Manager

type Manager interface {
	GetNamespaces() []*Namespace
	AddNamespace(ns *Namespace)
}

// --- Namespace Manager ---
// 실제: namespace.namespaceManager

const (
	// 실제: namespace/defaults.go
	cleanupInterval = 5 * time.Minute
	namespaceTTL    = 1 * time.Hour
)

type NamespaceManager struct {
	mu         sync.RWMutex
	namespaces map[string]NamespaceRecord
	nowFunc    func() time.Time // 테스트 가능하도록 시간 함수 주입

	// PoC 전용: TTL/cleanup 간격 오버라이드 (시뮬레이션용)
	ttl             time.Duration
	cleanupInterval time.Duration
}

// NewManager는 새 매니저 생성
// 실제: namespace.NewManager()
func NewManager() *NamespaceManager {
	return &NamespaceManager{
		namespaces:      make(map[string]NamespaceRecord),
		nowFunc:         time.Now,
		ttl:             namespaceTTL,
		cleanupInterval: cleanupInterval,
	}
}

// NewManagerWithCustomTTL은 커스텀 TTL로 매니저 생성 (시뮬레이션용)
func NewManagerWithCustomTTL(ttl, cleanup time.Duration) *NamespaceManager {
	return &NamespaceManager{
		namespaces:      make(map[string]NamespaceRecord),
		nowFunc:         time.Now,
		ttl:             ttl,
		cleanupInterval: cleanup,
	}
}

// AddNamespace는 네임스페이스를 추가/갱신
// 실제: namespaceManager.AddNamespace()
// 키: cluster/namespace 조합
func (m *NamespaceManager) AddNamespace(ns *Namespace) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := ns.Cluster + "/" + ns.Namespace
	m.namespaces[key] = NamespaceRecord{
		namespace: ns,
		added:     m.nowFunc(),
	}
}

// GetNamespaces는 현재 추적 중인 네임스페이스 목록 반환
// 실제: namespaceManager.GetNamespaces()
// 결과는 cluster → namespace 순으로 정렬
func (m *NamespaceManager) GetNamespaces() []*Namespace {
	m.mu.RLock()
	namespaces := make([]*Namespace, 0, len(m.namespaces))
	for _, ns := range m.namespaces {
		namespaces = append(namespaces, ns.namespace)
	}
	m.mu.RUnlock()

	// 실제: sort.Slice - cluster 먼저, 같으면 namespace 비교
	sort.Slice(namespaces, func(i, j int) bool {
		a := namespaces[i]
		b := namespaces[j]
		if a.Cluster != b.Cluster {
			return a.Cluster < b.Cluster
		}
		return a.Namespace < b.Namespace
	})
	return namespaces
}

// cleanupNamespaces는 TTL이 만료된 네임스페이스를 정리
// 실제: namespaceManager.cleanupNamespaces()
func (m *NamespaceManager) cleanupNamespaces() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	removed := 0
	now := m.nowFunc()
	for key, record := range m.namespaces {
		if record.added.Add(m.ttl).Before(now) {
			delete(m.namespaces, key)
			removed++
		}
	}
	return removed
}

// StartCleanup은 주기적 정리 고루틴 시작
// 실제: Hive Cell의 Start에서 실행
func (m *NamespaceManager) StartCleanup(stopCh <-chan struct{}) {
	ticker := time.NewTicker(m.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			removed := m.cleanupNamespaces()
			if removed > 0 {
				fmt.Printf("  [GC] %d개 만료된 네임스페이스 정리됨\n", removed)
			}
		}
	}
}

// Len은 현재 추적 중인 네임스페이스 수 반환
func (m *NamespaceManager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.namespaces)
}

// --- Flow 모델 ---

type Endpoint struct {
	Namespace string
	PodName   string
}

type Flow struct {
	Time        time.Time
	Source      *Endpoint
	Destination *Endpoint
	NodeName    string
}

// --- trackNamespaces ---
// 실제: LocalObserverServer.trackNamespaces()

func trackNamespaces(m Manager, flow *Flow, clusterName string) {
	if srcNs := flow.Source.Namespace; srcNs != "" {
		m.AddNamespace(&Namespace{
			Namespace: srcNs,
			Cluster:   clusterName,
		})
	}
	if dstNs := flow.Destination.Namespace; dstNs != "" {
		m.AddNamespace(&Namespace{
			Namespace: dstNs,
			Cluster:   clusterName,
		})
	}
}

// --- 테스트 데이터 ---

func generateFlows() []*Flow {
	namespaces := []string{
		"default", "kube-system", "monitoring", "prod",
		"staging", "dev", "istio-system", "cert-manager",
	}
	pods := []string{"frontend", "backend", "api", "db", "cache", "worker"}

	var flows []*Flow
	for i := 0; i < 50; i++ {
		srcNs := namespaces[rand.Intn(len(namespaces))]
		dstNs := namespaces[rand.Intn(len(namespaces))]
		flows = append(flows, &Flow{
			Time: time.Now(),
			Source: &Endpoint{
				Namespace: srcNs,
				PodName:   fmt.Sprintf("%s-%d", pods[rand.Intn(len(pods))], rand.Intn(3)),
			},
			Destination: &Endpoint{
				Namespace: dstNs,
				PodName:   fmt.Sprintf("%s-%d", pods[rand.Intn(len(pods))], rand.Intn(3)),
			},
			NodeName: fmt.Sprintf("node-%d", rand.Intn(3)),
		})
	}
	return flows
}

func printNamespaces(m *NamespaceManager) {
	nss := m.GetNamespaces()
	fmt.Printf("  추적 중인 네임스페이스 (%d개):\n", len(nss))
	for _, ns := range nss {
		fmt.Printf("    - %s/%s\n", ns.Cluster, ns.Namespace)
	}
}

func main() {
	fmt.Println("=== Hubble 네임스페이스 매니저 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: cilium/pkg/hubble/observer/namespace/manager.go   - namespaceManager")
	fmt.Println("참조: cilium/pkg/hubble/observer/namespace/defaults.go  - TTL=1h, cleanup=5m")
	fmt.Println("참조: cilium/pkg/hubble/observer/local_observer.go      - trackNamespaces()")
	fmt.Println()

	// === 테스트 1: 기본 네임스페이스 추적 ===
	fmt.Println("--- 테스트 1: Flow에서 네임스페이스 자동 추적 ---")
	fmt.Println()

	mgr := NewManager()
	clusterName := "cluster-1"

	flows := generateFlows()
	for _, flow := range flows {
		trackNamespaces(mgr, flow, clusterName)
	}

	printNamespaces(mgr)
	fmt.Println()

	// === 테스트 2: 멀티 클러스터 ===
	fmt.Println("--- 테스트 2: 멀티 클러스터 네임스페이스 추적 ---")
	fmt.Println()

	multiMgr := NewManager()

	// 클러스터 1
	multiMgr.AddNamespace(&Namespace{Namespace: "default", Cluster: "us-east"})
	multiMgr.AddNamespace(&Namespace{Namespace: "kube-system", Cluster: "us-east"})
	multiMgr.AddNamespace(&Namespace{Namespace: "prod", Cluster: "us-east"})

	// 클러스터 2
	multiMgr.AddNamespace(&Namespace{Namespace: "default", Cluster: "eu-west"})
	multiMgr.AddNamespace(&Namespace{Namespace: "kube-system", Cluster: "eu-west"})
	multiMgr.AddNamespace(&Namespace{Namespace: "staging", Cluster: "eu-west"})

	// 클러스터 3
	multiMgr.AddNamespace(&Namespace{Namespace: "default", Cluster: "ap-south"})
	multiMgr.AddNamespace(&Namespace{Namespace: "monitoring", Cluster: "ap-south"})

	nss := multiMgr.GetNamespaces()
	fmt.Printf("  멀티 클러스터 네임스페이스 (%d개, cluster/namespace 순 정렬):\n", len(nss))
	for _, ns := range nss {
		fmt.Printf("    - %s/%s\n", ns.Cluster, ns.Namespace)
	}
	fmt.Println()

	// === 테스트 3: TTL 기반 만료 (시뮬레이션) ===
	fmt.Println("--- 테스트 3: TTL 기반 네임스페이스 만료 ---")
	fmt.Println("  (실제: TTL=1시간, 여기서는 2초로 시뮬레이션)")
	fmt.Println()

	ttlMgr := NewManagerWithCustomTTL(2*time.Second, 1*time.Second)

	// 타임스탬프를 수동 조작하기 위한 설정
	currentTime := time.Now()
	ttlMgr.nowFunc = func() time.Time { return currentTime }

	// T=0: 네임스페이스 추가
	ttlMgr.AddNamespace(&Namespace{Namespace: "old-ns-1", Cluster: "test"})
	ttlMgr.AddNamespace(&Namespace{Namespace: "old-ns-2", Cluster: "test"})
	fmt.Printf("  T=0초: 추가 후 %d개 네임스페이스\n", ttlMgr.Len())

	// T=1s: 새 네임스페이스 추가
	currentTime = currentTime.Add(1 * time.Second)
	ttlMgr.AddNamespace(&Namespace{Namespace: "new-ns-1", Cluster: "test"})
	fmt.Printf("  T=1초: 추가 후 %d개 네임스페이스\n", ttlMgr.Len())

	// T=2.5s: old-ns-1, old-ns-2는 TTL(2초) 만료
	currentTime = currentTime.Add(1500 * time.Millisecond)
	removed := ttlMgr.cleanupNamespaces()
	fmt.Printf("  T=2.5초: GC 실행 → %d개 만료, 남은 %d개\n", removed, ttlMgr.Len())
	for _, ns := range ttlMgr.GetNamespaces() {
		fmt.Printf("    - %s/%s (생존)\n", ns.Cluster, ns.Namespace)
	}

	// T=3.5s: new-ns-1도 만료
	currentTime = currentTime.Add(1 * time.Second)
	removed = ttlMgr.cleanupNamespaces()
	fmt.Printf("  T=3.5초: GC 실행 → %d개 만료, 남은 %d개\n", removed, ttlMgr.Len())
	fmt.Println()

	// === 테스트 4: 동일 네임스페이스 갱신 ===
	fmt.Println("--- 테스트 4: 동일 네임스페이스의 TTL 갱신 ---")
	fmt.Println()

	renewMgr := NewManagerWithCustomTTL(3*time.Second, 1*time.Second)
	renewTime := time.Now()
	renewMgr.nowFunc = func() time.Time { return renewTime }

	// T=0: 추가
	renewMgr.AddNamespace(&Namespace{Namespace: "kube-system", Cluster: "test"})
	fmt.Printf("  T=0초: kube-system 추가\n")

	// T=2s: 동일 네임스페이스에 새 Flow → TTL 갱신
	renewTime = renewTime.Add(2 * time.Second)
	renewMgr.AddNamespace(&Namespace{Namespace: "kube-system", Cluster: "test"})
	fmt.Printf("  T=2초: kube-system 갱신 (새 Flow 도착)\n")

	// T=4s: 원래 TTL(3초)이면 만료, 갱신 후 TTL이면 생존
	renewTime = renewTime.Add(2 * time.Second)
	removed = renewMgr.cleanupNamespaces()
	fmt.Printf("  T=4초: GC 실행 → %d개 만료, 남은 %d개\n", removed, renewMgr.Len())
	fmt.Printf("    결과: 갱신 덕분에 kube-system 생존 (마지막 접근 T=2초 + TTL=3초 = T=5초까지)\n")

	// T=5.5s: 이제 만료
	renewTime = renewTime.Add(1500 * time.Millisecond)
	removed = renewMgr.cleanupNamespaces()
	fmt.Printf("  T=5.5초: GC 실행 → %d개 만료, 남은 %d개\n", removed, renewMgr.Len())
	fmt.Println()

	// === 테스트 5: 동시성 테스트 ===
	fmt.Println("--- 테스트 5: 동시성 안전성 테스트 ---")
	fmt.Println()

	concMgr := NewManager()
	var wg sync.WaitGroup

	// 동시에 AddNamespace 호출
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				concMgr.AddNamespace(&Namespace{
					Namespace: fmt.Sprintf("ns-%d", j%20),
					Cluster:   fmt.Sprintf("cluster-%d", id%3),
				})
			}
		}(i)
	}

	// 동시에 GetNamespaces 호출
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = concMgr.GetNamespaces()
			}
		}()
	}

	wg.Wait()
	fmt.Printf("  동시 접근 후 네임스페이스 수: %d (데이터 경합 없음)\n", concMgr.Len())
	fmt.Println()

	// === 테스트 6: 주기적 GC 데모 ===
	fmt.Println("--- 테스트 6: 주기적 GC (1초 간격, 2초 TTL) ---")
	fmt.Println()

	gcMgr := NewManagerWithCustomTTL(2*time.Second, 1*time.Second)
	stopCh := make(chan struct{})

	// 초기 네임스페이스 추가
	gcMgr.AddNamespace(&Namespace{Namespace: "will-expire", Cluster: "test"})
	fmt.Printf("  네임스페이스 추가: will-expire\n")

	// GC 고루틴 시작
	go gcMgr.StartCleanup(stopCh)

	// 잠시 대기 후 새 네임스페이스 추가
	time.Sleep(1 * time.Second)
	gcMgr.AddNamespace(&Namespace{Namespace: "fresh-ns", Cluster: "test"})
	fmt.Printf("  네임스페이스 추가: fresh-ns (T=1초)\n")
	fmt.Printf("  현재: %d개\n", gcMgr.Len())

	// will-expire가 만료될 때까지 대기
	time.Sleep(2 * time.Second)
	fmt.Printf("  T=3초: %d개 (will-expire 만료됨)\n", gcMgr.Len())

	// fresh-ns도 만료될 때까지 대기
	time.Sleep(2 * time.Second)
	fmt.Printf("  T=5초: %d개 (fresh-ns도 만료됨)\n", gcMgr.Len())

	close(stopCh)
	time.Sleep(100 * time.Millisecond)

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Manager 인터페이스: GetNamespaces/AddNamespace 2개 메서드")
	fmt.Println("  2. 키 구조: cluster/namespace 조합으로 유니크 식별")
	fmt.Println("  3. TTL 캐시: 1시간 미접근 시 자동 제거 (cleanupNamespaces)")
	fmt.Println("  4. 주기적 GC: 5분마다 cleanupNamespaces 실행")
	fmt.Println("  5. 동시성: RWMutex로 읽기/쓰기 분리")
	fmt.Println("  6. 정렬: GetNamespaces 결과를 cluster → namespace 순 정렬")
	fmt.Println("  7. trackNamespaces: Flow에서 src/dst 네임스페이스 자동 추출")
}
