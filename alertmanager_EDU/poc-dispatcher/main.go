// Alertmanager Dispatcher PoC
//
// Alertmanager의 Dispatcher 핵심 동작을 시뮬레이션한다.
// Alert 수신 → Route 매칭 → AggregationGroup 할당 → flush 타이밍을 재현한다.
//
// 핵심 개념:
//   - Alert를 group_by 레이블로 그룹핑
//   - AggregationGroup별 flush 타이밍 (GroupWait, GroupInterval)
//   - 동시 워커(concurrency)를 통한 Alert 처리
//
// 실행: go run main.go

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// LabelSet은 레이블 집합이다.
type LabelSet map[string]string

// Alert는 수신된 알림이다.
type Alert struct {
	Labels   LabelSet
	StartsAt time.Time
}

// AggregationGroup은 동일 그룹 키를 가진 Alert들의 그룹이다.
type AggregationGroup struct {
	key      string
	receiver string
	alerts   []*Alert
	mu       sync.Mutex

	groupWait     time.Duration
	groupInterval time.Duration

	firstInsert time.Time    // 첫 Alert 삽입 시간
	lastFlush   time.Time    // 마지막 flush 시간
	flushCount  int          // flush 횟수
}

// insert는 AggregationGroup에 Alert를 추가한다.
func (ag *AggregationGroup) insert(alert *Alert) {
	ag.mu.Lock()
	defer ag.mu.Unlock()

	if len(ag.alerts) == 0 {
		ag.firstInsert = time.Now()
	}
	ag.alerts = append(ag.alerts, alert)
}

// flush는 그룹의 모든 Alert를 전송한다.
func (ag *AggregationGroup) flush() {
	ag.mu.Lock()
	defer ag.mu.Unlock()

	ag.flushCount++
	ag.lastFlush = time.Now()

	fmt.Printf("  [FLUSH #%d] 그룹 %q → receiver=%s, Alert %d개:\n",
		ag.flushCount, ag.key, ag.receiver, len(ag.alerts))
	for _, a := range ag.alerts {
		fmt.Printf("    - %v\n", a.Labels)
	}
}

// shouldFlush는 현재 flush해야 하는지 판정한다.
func (ag *AggregationGroup) shouldFlush() (bool, string) {
	ag.mu.Lock()
	defer ag.mu.Unlock()

	if len(ag.alerts) == 0 {
		return false, ""
	}

	now := time.Now()

	// 첫 flush: GroupWait 경과 확인
	if ag.flushCount == 0 {
		elapsed := now.Sub(ag.firstInsert)
		if elapsed >= ag.groupWait {
			return true, fmt.Sprintf("GroupWait %v 경과", ag.groupWait)
		}
		return false, fmt.Sprintf("GroupWait 대기 중 (경과: %v)", elapsed.Round(time.Millisecond))
	}

	// 후속 flush: GroupInterval 경과 확인
	elapsed := now.Sub(ag.lastFlush)
	if elapsed >= ag.groupInterval {
		return true, fmt.Sprintf("GroupInterval %v 경과", ag.groupInterval)
	}
	return false, fmt.Sprintf("GroupInterval 대기 중 (경과: %v)", elapsed.Round(time.Millisecond))
}

// Dispatcher는 Alert를 라우팅하고 그룹핑하는 컴포넌트이다.
type Dispatcher struct {
	groupBy  []string // 그룹핑 레이블
	receiver string   // 기본 receiver

	mu     sync.RWMutex
	groups map[string]*AggregationGroup

	groupWait     time.Duration
	groupInterval time.Duration
}

// NewDispatcher는 새 Dispatcher를 생성한다.
func NewDispatcher(groupBy []string, receiver string, wait, interval time.Duration) *Dispatcher {
	return &Dispatcher{
		groupBy:       groupBy,
		receiver:      receiver,
		groups:        make(map[string]*AggregationGroup),
		groupWait:     wait,
		groupInterval: interval,
	}
}

// processAlert는 Alert를 라우팅하고 그룹에 할당한다.
func (d *Dispatcher) processAlert(alert *Alert) {
	// group_by 레이블로 그룹 키 생성
	groupKey := d.makeGroupKey(alert.Labels)

	d.mu.Lock()
	ag, exists := d.groups[groupKey]
	if !exists {
		ag = &AggregationGroup{
			key:           groupKey,
			receiver:      d.receiver,
			groupWait:     d.groupWait,
			groupInterval: d.groupInterval,
		}
		d.groups[groupKey] = ag
		fmt.Printf("  [NEW GROUP] 키=%q 생성\n", groupKey)
	}
	d.mu.Unlock()

	ag.insert(alert)
	fmt.Printf("  [INSERT] Alert %v → 그룹 %q\n", alert.Labels, groupKey)
}

// makeGroupKey는 group_by 레이블로 그룹 키를 생성한다.
func (d *Dispatcher) makeGroupKey(labels LabelSet) string {
	var parts []string
	for _, key := range d.groupBy {
		val := labels[key]
		parts = append(parts, fmt.Sprintf("%s=%q", key, val))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// run은 Dispatcher를 실행한다 (Alert 수신 및 flush 루프).
func (d *Dispatcher) run(ctx context.Context, alerts <-chan *Alert, wg *sync.WaitGroup) {
	defer wg.Done()

	// Alert 수집 워커
	go func() {
		for {
			select {
			case alert, ok := <-alerts:
				if !ok {
					return
				}
				d.processAlert(alert)
			case <-ctx.Done():
				return
			}
		}
	}()

	// flush 체크 루프 (10ms 간격)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d.mu.RLock()
			for _, ag := range d.groups {
				if should, reason := ag.shouldFlush(); should {
					fmt.Printf("  [TRIGGER] %s\n", reason)
					ag.flush()
				}
			}
			d.mu.RUnlock()
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	fmt.Println("=== Alertmanager Dispatcher PoC ===")
	fmt.Println()

	// Dispatcher 설정: alertname으로 그룹핑
	// GroupWait=200ms, GroupInterval=500ms (데모용 짧은 간격)
	disp := NewDispatcher(
		[]string{"alertname"},
		"default-receiver",
		200*time.Millisecond,  // GroupWait
		500*time.Millisecond,  // GroupInterval
	)

	fmt.Println("설정:")
	fmt.Printf("  group_by: %v\n", disp.groupBy)
	fmt.Printf("  GroupWait: %v\n", disp.groupWait)
	fmt.Printf("  GroupInterval: %v\n", disp.groupInterval)
	fmt.Println()

	alertCh := make(chan *Alert, 100)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())

	wg.Add(1)
	go disp.run(ctx, alertCh, &wg)

	// 시나리오 1: 동시에 같은 alertname의 Alert 3개 수신
	fmt.Println("--- 시나리오 1: 같은 alertname 3개 동시 수신 ---")
	alertCh <- &Alert{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}}
	alertCh <- &Alert{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-2"}}
	alertCh <- &Alert{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-3"}}

	// GroupWait 대기 (200ms)
	time.Sleep(300 * time.Millisecond)
	fmt.Println()

	// 시나리오 2: 다른 alertname의 Alert 수신
	fmt.Println("--- 시나리오 2: 다른 alertname Alert 수신 ---")
	alertCh <- &Alert{Labels: LabelSet{"alertname": "HighMemory", "instance": "node-1"}}
	time.Sleep(300 * time.Millisecond)
	fmt.Println()

	// 시나리오 3: 기존 그룹에 새 Alert 추가 (GroupInterval 대기)
	fmt.Println("--- 시나리오 3: 기존 그룹에 추가 Alert (GroupInterval) ---")
	alertCh <- &Alert{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-4"}}
	time.Sleep(600 * time.Millisecond)

	cancel()
	wg.Wait()

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. Alert 수신 → group_by 레이블로 그룹 키 생성")
	fmt.Println("2. 새 그룹이면 AggregationGroup 생성")
	fmt.Println("3. 첫 flush: GroupWait(200ms) 후 그룹의 모든 Alert 일괄 전송")
	fmt.Println("4. 후속 flush: GroupInterval(500ms)마다 변경사항 전송")
	fmt.Println("5. 같은 alertname의 Alert는 하나의 그룹으로 묶임")
}
