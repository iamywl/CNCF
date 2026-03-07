// Alertmanager Alert Store PoC
//
// Alertmanager의 메모리 기반 Alert 저장소를 시뮬레이션한다.
// store/store.go의 Alerts와 provider/mem/mem.go의 구독 패턴을 재현한다.
//
// 핵심 개념:
//   - Fingerprint 기반 중복 제거 및 업데이트
//   - 구독자(Subscriber) 패턴
//   - GC (Garbage Collection)
//   - PreStore/PostStore 콜백
//
// 실행: go run main.go

package main

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"
)

// LabelSet은 레이블 집합이다.
type LabelSet map[string]string

// Fingerprint는 LabelSet의 해시값이다.
type Fingerprint uint64

// ComputeFingerprint는 LabelSet에서 Fingerprint를 생성한다.
func ComputeFingerprint(ls LabelSet) Fingerprint {
	keys := make([]string, 0, len(ls))
	for k := range ls {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := fnv.New64a()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\x00", k, ls[k])
	}
	return Fingerprint(h.Sum64())
}

// Alert는 저장되는 Alert이다.
type Alert struct {
	Labels   LabelSet
	StartsAt time.Time
	EndsAt   time.Time
	Status   string // "firing" or "resolved"
}

func (a *Alert) Fingerprint() Fingerprint {
	return ComputeFingerprint(a.Labels)
}

func (a *Alert) String() string {
	return fmt.Sprintf("{%v status=%s}", a.Labels, a.Status)
}

// Resolved는 Alert가 해결되었는지 확인한다.
func (a *Alert) Resolved() bool {
	return a.Status == "resolved" || (!a.EndsAt.IsZero() && a.EndsAt.Before(time.Now()))
}

// AlertStore는 Fingerprint 기반 Alert 저장소이다.
type AlertStore struct {
	mu    sync.RWMutex
	data  map[Fingerprint]*Alert
	count int
}

// NewAlertStore는 새 AlertStore를 생성한다.
func NewAlertStore() *AlertStore {
	return &AlertStore{data: make(map[Fingerprint]*Alert)}
}

// Set은 Alert를 저장한다. 기존 Alert가 있으면 업데이트한다.
func (s *AlertStore) Set(alert *Alert) (isNew bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fp := alert.Fingerprint()
	_, exists := s.data[fp]
	s.data[fp] = alert

	if !exists {
		s.count++
		return true
	}
	return false
}

// Get은 Fingerprint로 Alert를 조회한다.
func (s *AlertStore) Get(fp Fingerprint) (*Alert, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.data[fp]
	return a, ok
}

// Delete는 Alert를 삭제한다.
func (s *AlertStore) Delete(fp Fingerprint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[fp]; ok {
		delete(s.data, fp)
		s.count--
	}
}

// GetAll은 모든 Alert를 반환한다.
func (s *AlertStore) GetAll() []*Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Alert, 0, len(s.data))
	for _, a := range s.data {
		result = append(result, a)
	}
	return result
}

// Provider는 Alert 저장소 상위 레이어이다. 콜백과 구독을 관리한다.
type Provider struct {
	store       *AlertStore
	subscribers []chan *Alert
	mu          sync.Mutex
}

// NewProvider는 새 Provider를 생성한다.
func NewProvider() *Provider {
	return &Provider{
		store: NewAlertStore(),
	}
}

// Subscribe는 새 Alert 구독을 등록한다.
func (p *Provider) Subscribe() chan *Alert {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan *Alert, 100)
	p.subscribers = append(p.subscribers, ch)
	return ch
}

// Put은 Alert를 저장하고 구독자들에게 알린다.
func (p *Provider) Put(alerts []*Alert) error {
	for _, alert := range alerts {
		// PreStore 콜백: 유효성 검증
		if err := p.preStore(alert); err != nil {
			fmt.Printf("  [PreStore] 거부: %v - %v\n", alert, err)
			continue
		}

		// 저장
		isNew := p.store.Set(alert)
		action := "업데이트"
		if isNew {
			action = "새 저장"
		}
		fp := alert.Fingerprint()
		fmt.Printf("  [Store] %s: fp=%d, %v\n", action, fp, alert)

		// PostStore 콜백: 구독자 알림
		p.notify(alert)
	}
	return nil
}

// preStore는 저장 전 유효성을 검증한다.
func (p *Provider) preStore(alert *Alert) error {
	if len(alert.Labels) == 0 {
		return fmt.Errorf("레이블 없음")
	}
	return nil
}

// notify는 모든 구독자에게 Alert를 전달한다.
func (p *Provider) notify(alert *Alert) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, ch := range p.subscribers {
		select {
		case ch <- alert:
		default:
			fmt.Printf("  [Notify] 구독자 %d 채널 가득 참, 건너뜀\n", i)
		}
	}
}

// GC는 해결된(resolved) Alert를 정리한다.
func (p *Provider) GC() int {
	alerts := p.store.GetAll()
	deleted := 0
	for _, a := range alerts {
		if a.Resolved() {
			p.store.Delete(a.Fingerprint())
			deleted++
		}
	}
	return deleted
}

// GetPending은 활성 Alert만 반환한다.
func (p *Provider) GetPending() []*Alert {
	all := p.store.GetAll()
	var pending []*Alert
	for _, a := range all {
		if !a.Resolved() {
			pending = append(pending, a)
		}
	}
	return pending
}

func main() {
	fmt.Println("=== Alertmanager Alert Store PoC ===")
	fmt.Println()

	provider := NewProvider()

	// 구독자 등록
	sub := provider.Subscribe()
	var received []*Alert
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for a := range sub {
			received = append(received, a)
		}
	}()

	// 1. Alert 저장
	fmt.Println("--- 1. Alert 저장 ---")
	provider.Put([]*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}, Status: "firing", StartsAt: time.Now()},
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-2"}, Status: "firing", StartsAt: time.Now()},
		{Labels: LabelSet{"alertname": "HighMemory", "instance": "node-1"}, Status: "firing", StartsAt: time.Now()},
	})
	fmt.Println()

	// 2. Fingerprint 기반 중복 제거
	fmt.Println("--- 2. 중복 Alert 업데이트 ---")
	fmt.Println("같은 Labels의 Alert를 다시 전송 (상태 업데이트):")
	provider.Put([]*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}, Status: "firing", StartsAt: time.Now()},
	})
	fmt.Println()

	// 3. Alert 조회
	fmt.Println("--- 3. 활성 Alert 조회 ---")
	pending := provider.GetPending()
	fmt.Printf("활성 Alert: %d개\n", len(pending))
	for _, a := range pending {
		fp := a.Fingerprint()
		fmt.Printf("  [%d] %v\n", fp, a)
	}
	fmt.Println()

	// 4. Alert 해결
	fmt.Println("--- 4. Alert 해결 ---")
	provider.Put([]*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}, Status: "resolved",
			StartsAt: time.Now().Add(-1 * time.Hour), EndsAt: time.Now()},
	})
	fmt.Println()

	// 5. GC
	fmt.Println("--- 5. GC (해결된 Alert 정리) ---")
	deleted := provider.GC()
	fmt.Printf("GC 결과: %d개 삭제\n", deleted)
	pending = provider.GetPending()
	fmt.Printf("남은 활성 Alert: %d개\n", len(pending))
	for _, a := range pending {
		fmt.Printf("  %v\n", a)
	}
	fmt.Println()

	// 6. Fingerprint 동작 확인
	fmt.Println("--- 6. Fingerprint 동작 ---")
	ls1 := LabelSet{"alertname": "HighCPU", "instance": "node-1"}
	ls2 := LabelSet{"alertname": "HighCPU", "instance": "node-1"}
	ls3 := LabelSet{"alertname": "HighCPU", "instance": "node-2"}

	fp1 := ComputeFingerprint(ls1)
	fp2 := ComputeFingerprint(ls2)
	fp3 := ComputeFingerprint(ls3)

	fmt.Printf("  %v → fp=%d\n", ls1, fp1)
	fmt.Printf("  %v → fp=%d\n", ls2, fp2)
	fmt.Printf("  %v → fp=%d\n", ls3, fp3)
	fmt.Printf("  동일 Labels → 동일 Fingerprint: %v\n", fp1 == fp2)
	fmt.Printf("  다른 Labels → 다른 Fingerprint: %v\n", fp1 != fp3)

	// 구독자 채널 정리
	close(sub)
	wg.Wait()

	fmt.Println()
	fmt.Printf("  구독자가 수신한 Alert: %d개\n", len(received))
	for _, a := range received {
		labels := make([]string, 0)
		for k, v := range a.Labels {
			labels = append(labels, k+"="+v)
		}
		fmt.Printf("    %s (%s)\n", strings.Join(labels, ", "), a.Status)
	}

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. Labels의 해시(Fingerprint)로 Alert를 고유 식별")
	fmt.Println("2. 같은 Fingerprint면 기존 Alert 업데이트 (중복 제거)")
	fmt.Println("3. 구독자(Subscriber) 패턴으로 Dispatcher에 실시간 알림")
	fmt.Println("4. GC로 resolved Alert를 주기적으로 정리")
}
