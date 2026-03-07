// Alertmanager Deduplication PoC
//
// Alertmanager의 Alert 중복 제거 메커니즘을 시뮬레이션한다.
// Fingerprint 기반 중복 제거와 DedupStage의 nflog 기반 중복 제거를 재현한다.
//
// 핵심 개념:
//   - Fingerprint: Labels 해시로 동일 Alert 식별
//   - Provider 레벨 중복 제거: 같은 fp → 기존 Alert 업데이트
//   - DedupStage 레벨 중복 제거: nflog로 이미 전송된 Alert 스킵
//   - 변경 감지: firing/resolved 목록 비교
//
// 실행: go run main.go

package main

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
)

// LabelSet은 레이블 집합이다.
type LabelSet map[string]string

// Fingerprint는 LabelSet의 해시값이다.
type Fingerprint uint64

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

// Alert는 수신된 알림이다.
type Alert struct {
	Labels   LabelSet
	Status   string // "firing" or "resolved"
	StartsAt time.Time
	EndsAt   time.Time
}

func (a *Alert) Fingerprint() Fingerprint {
	return ComputeFingerprint(a.Labels)
}

func (a *Alert) String() string {
	var parts []string
	for k, v := range a.Labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return fmt.Sprintf("{%s} status=%s", strings.Join(parts, ","), a.Status)
}

// ProviderDedup은 Provider 레벨의 Fingerprint 기반 중복 제거이다.
type ProviderDedup struct {
	store map[Fingerprint]*Alert
}

func NewProviderDedup() *ProviderDedup {
	return &ProviderDedup{store: make(map[Fingerprint]*Alert)}
}

// Put은 Alert를 저장한다. 같은 Fingerprint면 업데이트한다.
func (pd *ProviderDedup) Put(alert *Alert) (action string) {
	fp := alert.Fingerprint()
	existing, exists := pd.store[fp]

	if !exists {
		pd.store[fp] = alert
		return "NEW"
	}

	// 기존 Alert 업데이트 (Merge)
	if alert.StartsAt.After(existing.StartsAt) {
		existing.StartsAt = alert.StartsAt
	}
	if alert.Status == "resolved" {
		existing.Status = "resolved"
		existing.EndsAt = alert.EndsAt
	}

	return "UPDATE"
}

// GetAll은 모든 Alert를 반환한다.
func (pd *ProviderDedup) GetAll() []*Alert {
	result := make([]*Alert, 0, len(pd.store))
	for _, a := range pd.store {
		result = append(result, a)
	}
	return result
}

// NFLogEntry는 발송 기록이다.
type NFLogEntry struct {
	FiringAlerts   map[Fingerprint]bool
	ResolvedAlerts map[Fingerprint]bool
	Timestamp      time.Time
}

// DedupStage는 nflog 기반의 알림 중복 제거이다.
type DedupStage struct {
	log map[string]*NFLogEntry // groupKey → 발송 기록
}

func NewDedupStage() *DedupStage {
	return &DedupStage{log: make(map[string]*NFLogEntry)}
}

// NeedsNotify는 알림이 필요한지 판단한다.
func (ds *DedupStage) NeedsNotify(groupKey string, alerts []*Alert) (needsNotify bool, reason string, toSend []*Alert) {
	// 이전 발송 기록 조회
	prev, exists := ds.log[groupKey]
	if !exists {
		// 이전 기록 없음 → 전체 전송
		return true, "첫 알림 (기록 없음)", alerts
	}

	// 변경사항 확인
	var newFiring, newResolved []*Alert

	for _, alert := range alerts {
		fp := alert.Fingerprint()
		if alert.Status == "firing" {
			if !prev.FiringAlerts[fp] {
				newFiring = append(newFiring, alert)
			}
		} else if alert.Status == "resolved" {
			if !prev.ResolvedAlerts[fp] {
				newResolved = append(newResolved, alert)
			}
		}
	}

	if len(newFiring) > 0 || len(newResolved) > 0 {
		return true,
			fmt.Sprintf("새 firing: %d, 새 resolved: %d", len(newFiring), len(newResolved)),
			append(newFiring, newResolved...)
	}

	return false, "변경 없음 (중복)", nil
}

// RecordSent는 발송 기록을 저장한다.
func (ds *DedupStage) RecordSent(groupKey string, alerts []*Alert) {
	entry := &NFLogEntry{
		FiringAlerts:   make(map[Fingerprint]bool),
		ResolvedAlerts: make(map[Fingerprint]bool),
		Timestamp:      time.Now(),
	}

	for _, alert := range alerts {
		fp := alert.Fingerprint()
		if alert.Status == "firing" {
			entry.FiringAlerts[fp] = true
		} else {
			entry.ResolvedAlerts[fp] = true
		}
	}

	ds.log[groupKey] = entry
}

func main() {
	fmt.Println("=== Alertmanager Deduplication PoC ===")
	fmt.Println()

	// === Part 1: Provider 레벨 중복 제거 ===
	fmt.Println("=== Part 1: Provider 레벨 (Fingerprint 기반) ===")
	fmt.Println()

	provider := NewProviderDedup()

	// 1. 새 Alert 저장
	fmt.Println("--- 1. 새 Alert 저장 ---")
	alerts := []*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}, Status: "firing"},
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-2"}, Status: "firing"},
		{Labels: LabelSet{"alertname": "HighMemory", "instance": "node-1"}, Status: "firing"},
	}

	for _, a := range alerts {
		action := provider.Put(a)
		fmt.Printf("  [%s] fp=%d %s\n", action, a.Fingerprint(), a)
	}
	fmt.Println()

	// 2. 동일 Alert 재전송 (중복 → UPDATE)
	fmt.Println("--- 2. 동일 Alert 재전송 (중복) ---")
	duplicates := []*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}, Status: "firing",
			StartsAt: time.Now()},
	}

	for _, a := range duplicates {
		action := provider.Put(a)
		fmt.Printf("  [%s] fp=%d %s (같은 Labels → 같은 Fingerprint)\n", action, a.Fingerprint(), a)
	}

	fmt.Printf("  Provider 내 Alert: %d개 (3개 입력, 1개 중복)\n", len(provider.GetAll()))
	fmt.Println()

	// 3. Alert 해결 (resolved → UPDATE)
	fmt.Println("--- 3. Alert 해결 ---")
	resolved := &Alert{
		Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"},
		Status: "resolved", EndsAt: time.Now(),
	}
	action := provider.Put(resolved)
	fmt.Printf("  [%s] fp=%d %s\n", action, resolved.Fingerprint(), resolved)
	fmt.Println()

	// === Part 2: DedupStage 레벨 중복 제거 ===
	fmt.Println("=== Part 2: DedupStage 레벨 (nflog 기반) ===")
	fmt.Println()

	dedup := NewDedupStage()
	groupKey := "alertname=HighCPU"

	// 1차 전송
	fmt.Println("--- 1차 알림 ---")
	batch1 := []*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}, Status: "firing"},
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-2"}, Status: "firing"},
	}
	needs, reason, toSend := dedup.NeedsNotify(groupKey, batch1)
	fmt.Printf("  전송 필요: %v (%s)\n", needs, reason)
	if needs {
		fmt.Printf("  전송 대상: %d개\n", len(toSend))
		dedup.RecordSent(groupKey, batch1)
	}
	fmt.Println()

	// 2차 전송 (동일 Alert → 중복)
	fmt.Println("--- 2차 알림 (동일 Alert) ---")
	needs, reason, _ = dedup.NeedsNotify(groupKey, batch1)
	fmt.Printf("  전송 필요: %v (%s)\n", needs, reason)
	fmt.Println()

	// 3차 전송 (새 Alert 추가)
	fmt.Println("--- 3차 알림 (새 Alert 추가) ---")
	batch3 := []*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}, Status: "firing"},
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-2"}, Status: "firing"},
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-3"}, Status: "firing"}, // 새로운
	}
	needs, reason, toSend = dedup.NeedsNotify(groupKey, batch3)
	fmt.Printf("  전송 필요: %v (%s)\n", needs, reason)
	if needs {
		fmt.Printf("  전송 대상:\n")
		for _, a := range toSend {
			fmt.Printf("    %s\n", a)
		}
		dedup.RecordSent(groupKey, batch3)
	}
	fmt.Println()

	// 4차 전송 (resolved Alert)
	fmt.Println("--- 4차 알림 (Alert 해결) ---")
	batch4 := []*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-2"}, Status: "firing"},
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-3"}, Status: "firing"},
		{Labels: LabelSet{"alertname": "HighCPU", "instance": "node-1"}, Status: "resolved"}, // 해결됨
	}
	needs, reason, toSend = dedup.NeedsNotify(groupKey, batch4)
	fmt.Printf("  전송 필요: %v (%s)\n", needs, reason)
	if needs {
		fmt.Printf("  전송 대상:\n")
		for _, a := range toSend {
			fmt.Printf("    %s\n", a)
		}
	}

	fmt.Println()
	fmt.Println("=== 중복 제거 계층 요약 ===")
	fmt.Println("1단계 (Provider): Labels의 Fingerprint로 동일 Alert 식별/업데이트")
	fmt.Println("2단계 (DedupStage): nflog로 이미 전송된 Alert 스킵")
	fmt.Println("  - 새 firing Alert 있을 때만 전송")
	fmt.Println("  - resolved Alert는 항상 전송")
	fmt.Println("  - 변경 없으면 중복으로 판단하여 스킵")
}
