// Alertmanager Notification Log (nflog) PoC
//
// Alertmanager의 Notification Log를 시뮬레이션한다.
// nflog/nflog.go의 Log/Query 및 DedupStage 연동을 재현한다.
//
// 핵심 개념:
//   - groupKey + receiver로 발송 기록 저장
//   - DedupStage에서 중복 알림 방지
//   - TTL 기반 만료 및 GC
//   - 클러스터 간 동기화 (Merge)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"sync"
	"time"
)

// Receiver는 알림 수신자이다.
type Receiver struct {
	GroupName   string
	Integration string
	Idx         int
}

func (r Receiver) Key() string {
	return fmt.Sprintf("%s/%s/%d", r.GroupName, r.Integration, r.Idx)
}

// Entry는 nflog의 발송 기록 항목이다.
type Entry struct {
	GroupKey       string
	Receiver      Receiver
	FiringAlerts  []uint64 // firing alert fingerprints
	ResolvedAlerts []uint64 // resolved alert fingerprints
	Timestamp     time.Time
	ExpiresAt     time.Time
}

// NFLog는 Notification Log 저장소이다.
type NFLog struct {
	mu  sync.RWMutex
	st  map[string]*Entry // key = groupKey + receiverKey
	ttl time.Duration     // 만료 시간
}

// NewNFLog는 새 NFLog를 생성한다.
func NewNFLog(ttl time.Duration) *NFLog {
	return &NFLog{
		st:  make(map[string]*Entry),
		ttl: ttl,
	}
}

// stateKey는 고유 키를 생성한다.
func stateKey(groupKey string, recv Receiver) string {
	return groupKey + ":" + recv.Key()
}

// Log는 발송 기록을 저장한다.
func (n *NFLog) Log(groupKey string, recv Receiver, firing, resolved []uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := time.Now()
	key := stateKey(groupKey, recv)

	n.st[key] = &Entry{
		GroupKey:        groupKey,
		Receiver:        recv,
		FiringAlerts:    firing,
		ResolvedAlerts:  resolved,
		Timestamp:       now,
		ExpiresAt:       now.Add(n.ttl),
	}

	fmt.Printf("  [Log] key=%q, firing=%v, resolved=%v, expires=%v\n",
		key, firing, resolved, n.ttl)
}

// Query는 groupKey와 receiver로 발송 기록을 조회한다.
func (n *NFLog) Query(groupKey string, recv Receiver) (*Entry, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	key := stateKey(groupKey, recv)
	entry, ok := n.st[key]

	if !ok {
		fmt.Printf("  [Query] key=%q → 기록 없음\n", key)
		return nil, false
	}

	// 만료 확인
	if time.Now().After(entry.ExpiresAt) {
		fmt.Printf("  [Query] key=%q → 만료됨\n", key)
		return nil, false
	}

	fmt.Printf("  [Query] key=%q → firing=%v, resolved=%v\n",
		key, entry.FiringAlerts, entry.ResolvedAlerts)
	return entry, true
}

// GC는 만료된 항목을 삭제한다.
func (n *NFLog) GC() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := time.Now()
	deleted := 0
	for key, entry := range n.st {
		if now.After(entry.ExpiresAt) {
			delete(n.st, key)
			deleted++
			fmt.Printf("  [GC] key=%q 삭제\n", key)
		}
	}
	return deleted
}

// Merge는 원격 노드의 항목을 병합한다 (CRDT).
func (n *NFLog) Merge(remote *Entry) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	key := stateKey(remote.GroupKey, remote.Receiver)
	existing, ok := n.st[key]

	if !ok || remote.Timestamp.After(existing.Timestamp) {
		n.st[key] = remote
		return true
	}
	return false
}

// DedupStage는 중복 알림을 방지하는 파이프라인 단계이다.
type DedupStage struct {
	nflog *NFLog
}

// NewDedupStage는 새 DedupStage를 생성한다.
func NewDedupStage(nflog *NFLog) *DedupStage {
	return &DedupStage{nflog: nflog}
}

// NeedsNotify는 알림이 필요한지 판단한다.
func (ds *DedupStage) NeedsNotify(groupKey string, recv Receiver, currentFiring, currentResolved []uint64) bool {
	fmt.Printf("  [Dedup] 중복 확인: groupKey=%q, receiver=%s\n", groupKey, recv.Key())

	entry, ok := ds.nflog.Query(groupKey, recv)
	if !ok {
		// 이전 기록 없음 → 알림 필요
		fmt.Println("  [Dedup] → 이전 기록 없음, 알림 필요")
		return true
	}

	// firing 목록 비교
	prevFiringSet := make(map[uint64]bool)
	for _, fp := range entry.FiringAlerts {
		prevFiringSet[fp] = true
	}

	// 새로운 firing alert 확인
	for _, fp := range currentFiring {
		if !prevFiringSet[fp] {
			fmt.Printf("  [Dedup] → 새 firing alert fp=%d 발견, 알림 필요\n", fp)
			return true
		}
	}

	// resolved alert 확인
	if len(currentResolved) > 0 {
		fmt.Printf("  [Dedup] → resolved alert %v 발견, 알림 필요\n", currentResolved)
		return true
	}

	fmt.Println("  [Dedup] → 변경 없음, 알림 불필요 (중복)")
	return false
}

func main() {
	fmt.Println("=== Alertmanager Notification Log (nflog) PoC ===")
	fmt.Println()

	nflog := NewNFLog(500 * time.Millisecond) // TTL=500ms (데모용)
	dedup := NewDedupStage(nflog)

	recv := Receiver{
		GroupName:   "default",
		Integration: "slack",
		Idx:         0,
	}
	groupKey := "alertname=HighCPU"

	// 시나리오 1: 첫 알림 → 전송 필요
	fmt.Println("--- 시나리오 1: 첫 알림 ---")
	firing := []uint64{1001, 1002}
	if dedup.NeedsNotify(groupKey, recv, firing, nil) {
		fmt.Println("  → 알림 전송!")
		nflog.Log(groupKey, recv, firing, nil)
	}
	fmt.Println()

	// 시나리오 2: 동일 alert → 중복 제거
	fmt.Println("--- 시나리오 2: 동일 Alert 재전송 (중복) ---")
	if dedup.NeedsNotify(groupKey, recv, firing, nil) {
		fmt.Println("  → 알림 전송!")
		nflog.Log(groupKey, recv, firing, nil)
	} else {
		fmt.Println("  → 알림 스킵 (중복)")
	}
	fmt.Println()

	// 시나리오 3: 새 alert 추가 → 전송 필요
	fmt.Println("--- 시나리오 3: 새 Alert 추가 ---")
	newFiring := []uint64{1001, 1002, 1003} // 1003 추가
	if dedup.NeedsNotify(groupKey, recv, newFiring, nil) {
		fmt.Println("  → 알림 전송! (새 alert 포함)")
		nflog.Log(groupKey, recv, newFiring, nil)
	}
	fmt.Println()

	// 시나리오 4: resolved alert → 전송 필요
	fmt.Println("--- 시나리오 4: Alert 해결 ---")
	resolved := []uint64{1001}
	remainFiring := []uint64{1002, 1003}
	if dedup.NeedsNotify(groupKey, recv, remainFiring, resolved) {
		fmt.Println("  → 알림 전송! (resolved 포함)")
		nflog.Log(groupKey, recv, remainFiring, resolved)
	}
	fmt.Println()

	// 시나리오 5: TTL 만료 후 → 전송 필요
	fmt.Println("--- 시나리오 5: TTL 만료 후 ---")
	fmt.Println("  500ms 대기 중...")
	time.Sleep(600 * time.Millisecond)

	if dedup.NeedsNotify(groupKey, recv, remainFiring, nil) {
		fmt.Println("  → 알림 전송! (TTL 만료로 기록 소실)")
		nflog.Log(groupKey, recv, remainFiring, nil)
	}
	fmt.Println()

	// GC 실행
	fmt.Println("--- GC 실행 ---")
	// 짧은 TTL nflog에 새 항목 추가 후 만료시킴
	shortNflog := NewNFLog(1 * time.Millisecond)
	shortNflog.Log("group1", recv, []uint64{1}, nil)
	shortNflog.Log("group2", recv, []uint64{2}, nil)
	time.Sleep(10 * time.Millisecond)
	shortNflog.Log("group3", recv, []uint64{3}, nil) // 아직 유효
	deleted := shortNflog.GC()
	fmt.Printf("GC 결과: %d개 삭제\n", deleted)

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. nflog는 groupKey + receiver 조합으로 발송 기록 저장")
	fmt.Println("2. DedupStage에서 이전 발송 기록과 현재 Alert 비교")
	fmt.Println("3. 새 firing/resolved Alert가 있을 때만 알림 전송")
	fmt.Println("4. TTL로 기록 만료 → RepeatInterval 후 재전송 가능")
	fmt.Println("5. 클러스터 간 Merge로 중복 알림 방지 (CRDT)")
}
