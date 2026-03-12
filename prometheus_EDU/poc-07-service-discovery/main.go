// Prometheus Service Discovery PoC
//
// Prometheus의 플러거블 서비스 디스커버리 프레임워크를 Go 표준 라이브러리만으로 재현한다.
// 핵심 구조: Discoverer 인터페이스 → DiscoveryManager(병합/중복제거/디바운싱) → ScrapeManager(타겟 소비)
//
// 실제 Prometheus 소스코드 참조:
//   - discovery/targetgroup/targetgroup.go  → Group struct
//   - discovery/discovery.go               → Discoverer interface, staticDiscoverer
//   - discovery/manager.go                 → Manager (sender, updater, triggerSend, updatert=5s)
//   - discovery/file/file.go               → File-based SD (polling + fsnotify)
//
// 실행: go run main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. TargetGroup — Prometheus의 targetgroup.Group 재현
//    실제 코드: Targets []model.LabelSet, Labels model.LabelSet, Source string
// =============================================================================

// TargetGroup은 동일한 레이블 셋을 공유하는 타겟 그룹이다.
// Source 필드가 그룹의 고유 식별자 역할을 하며, Manager에서 중복 제거에 사용된다.
type TargetGroup struct {
	Targets []map[string]string // 각 타겟의 레이블 (예: "__address__": "host:port")
	Labels  map[string]string   // 그룹 공통 레이블
	Source  string              // 그룹 식별자 — Manager의 dedup 키
}

func (tg TargetGroup) String() string {
	addrs := make([]string, 0, len(tg.Targets))
	for _, t := range tg.Targets {
		if addr, ok := t["__address__"]; ok {
			addrs = append(addrs, addr)
		}
	}
	return fmt.Sprintf("[%s] targets=%s labels=%v", tg.Source, strings.Join(addrs, ","), tg.Labels)
}

// =============================================================================
// 2. Discoverer 인터페이스 — 모든 SD 메커니즘의 공통 계약
//    실제 코드: discovery/discovery.go:35
//    - Run은 ctx가 취소될 때까지 blocking
//    - 업데이트 채널을 close하면 안 됨 (실제 코드의 staticDiscoverer는 예외적으로 close)
// =============================================================================

type Discoverer interface {
	Name() string
	Run(ctx context.Context, ch chan<- []TargetGroup)
}

// =============================================================================
// 3. StaticDiscoverer — 고정 타겟 목록 제공
//    실제 코드: discovery/discovery.go:159-168 (staticDiscoverer)
//    한 번 전송하고 ctx 대기 — 가장 단순한 SD
// =============================================================================

type StaticDiscoverer struct {
	groups []TargetGroup
}

func NewStaticDiscoverer(groups []TargetGroup) *StaticDiscoverer {
	return &StaticDiscoverer{groups: groups}
}

func (d *StaticDiscoverer) Name() string { return "static" }

func (d *StaticDiscoverer) Run(ctx context.Context, ch chan<- []TargetGroup) {
	// 초기 전송 후 ctx 취소까지 대기 — Prometheus의 실제 패턴
	select {
	case <-ctx.Done():
		return
	case ch <- d.groups:
	}
	<-ctx.Done()
}

// =============================================================================
// 4. FileDiscoverer — 파일 기반 타겟 디스커버리 (폴링 방식)
//    실제 코드: discovery/file/file.go
//    Prometheus는 fsnotify + RefreshInterval 폴링 병행.
//    여기서는 폴링만으로 시뮬레이션 (3초 주기).
//    JSON 형식: [{"targets":["host:port"], "labels":{"key":"val"}}]
// =============================================================================

type FileDiscoverer struct {
	filePath        string
	refreshInterval time.Duration
	lastSources     map[string]struct{} // 이전 폴링에서 존재했던 Source 추적
}

func NewFileDiscoverer(path string, interval time.Duration) *FileDiscoverer {
	return &FileDiscoverer{filePath: path, refreshInterval: interval, lastSources: make(map[string]struct{})}
}

func (d *FileDiscoverer) Name() string { return "file" }

func (d *FileDiscoverer) Run(ctx context.Context, ch chan<- []TargetGroup) {
	// sendWithCleanup는 현재 그룹을 전송하면서 사라진 Source에 대해 빈 그룹을 전송한다.
	// 실제 Prometheus file SD도 동일하게 사라진 그룹을 빈 Targets로 전송하여 Manager가 삭제하게 한다.
	sendWithCleanup := func(groups []TargetGroup) {
		currentSources := make(map[string]struct{})
		for _, g := range groups {
			currentSources[g.Source] = struct{}{}
		}
		// 이전에 있었지만 지금은 없는 Source → 빈 타겟 그룹 전송 (삭제 시그널)
		for src := range d.lastSources {
			if _, ok := currentSources[src]; !ok {
				groups = append(groups, TargetGroup{Source: src, Targets: nil, Labels: nil})
			}
		}
		d.lastSources = currentSources

		select {
		case ch <- groups:
		case <-ctx.Done():
		}
	}

	// 초기 로드
	if groups, err := d.readFile(); err == nil {
		sendWithCleanup(groups)
	}

	// 폴링 루프 — Prometheus의 RefreshInterval과 동일한 개념
	ticker := time.NewTicker(d.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			groups, err := d.readFile()
			if err != nil {
				fmt.Printf("  [file-sd] 파일 읽기 실패: %v\n", err)
				continue
			}
			sendWithCleanup(groups)
		}
	}
}

// readFile은 JSON 파일에서 타겟 그룹을 파싱한다.
func (d *FileDiscoverer) readFile() ([]TargetGroup, error) {
	data, err := os.ReadFile(d.filePath)
	if err != nil {
		return nil, err
	}

	var raw []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	groups := make([]TargetGroup, 0, len(raw))
	for i, r := range raw {
		tg := TargetGroup{
			Labels: r.Labels,
			Source: fmt.Sprintf("file:%s:%d", d.filePath, i),
		}
		for _, addr := range r.Targets {
			tg.Targets = append(tg.Targets, map[string]string{"__address__": addr})
		}
		groups = append(groups, tg)
	}
	return groups, nil
}

// =============================================================================
// 5. DNSDiscoverer — DNS SRV 레코드 기반 디스커버리 시뮬레이션
//    실제 코드: discovery/dns/dns.go
//    Prometheus는 net.LookupSRV로 실제 DNS 질의.
//    여기서는 고정 데이터를 주기적으로 반환하여 시뮬레이션.
// =============================================================================

type DNSDiscoverer struct {
	domain          string
	refreshInterval time.Duration
	records         []string // 시뮬레이션용 SRV 레코드 목록
}

func NewDNSDiscoverer(domain string, records []string, interval time.Duration) *DNSDiscoverer {
	return &DNSDiscoverer{domain: domain, records: records, refreshInterval: interval}
}

func (d *DNSDiscoverer) Name() string { return "dns" }

func (d *DNSDiscoverer) Run(ctx context.Context, ch chan<- []TargetGroup) {
	send := func() {
		tg := TargetGroup{
			Labels: map[string]string{"__meta_dns_name": d.domain},
			Source: fmt.Sprintf("dns:%s", d.domain),
		}
		for _, rec := range d.records {
			tg.Targets = append(tg.Targets, map[string]string{"__address__": rec})
		}
		select {
		case ch <- []TargetGroup{tg}:
		case <-ctx.Done():
		}
	}

	// 초기 전송
	send()

	// 주기적 갱신 — DNS TTL 만료 후 재질의와 동일한 개념
	ticker := time.NewTicker(d.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// =============================================================================
// 6. DiscoveryManager — 핵심 조율자
//    실제 코드: discovery/manager.go:162-198 (Manager struct)
//
//    핵심 메커니즘:
//    a) poolKey{setName, provider}로 타겟 그룹 저장 → Source 기준 dedup
//    b) triggerSend 채널로 업데이트 시그널 → sender 고루틴에서 ticker(5초)마다 체크
//    c) allGroups()로 전체 병합 결과를 syncCh로 전송
//
//    이 설계의 이유:
//    - Discoverer마다 업데이트 주기가 다름 (DNS는 30초, k8s는 실시간)
//    - 짧은 시간 내 여러 업데이트가 오면 하나로 묶어 전송 (디바운싱)
//    - ScrapeManager는 전체 스냅샷을 받아 diff 처리
// =============================================================================

// poolKey는 {scrape job 이름, provider 이름}으로 타겟을 분류한다.
type poolKey struct {
	setName  string
	provider string
}

type DiscoveryManager struct {
	mu sync.Mutex

	// targets: poolKey → (source → TargetGroup) — Source 기준 중복 제거
	targets map[poolKey]map[string]TargetGroup

	// providers: 등록된 discoverer 목록
	providers []providerEntry

	// syncCh: 병합된 전체 타겟을 소비자에게 전달
	syncCh chan map[string][]TargetGroup

	// triggerSend: updater가 새 데이터 수신 시 sender에게 시그널
	triggerSend chan struct{}

	// updatert: 디바운싱 간격 — 실제 Prometheus는 5초 (manager.go:97)
	updatert time.Duration

	ctx    context.Context
	cancel context.CancelFunc
}

type providerEntry struct {
	name    string
	setName string
	disc    Discoverer
}

func NewDiscoveryManager(updateInterval time.Duration) *DiscoveryManager {
	return &DiscoveryManager{
		targets:     make(map[poolKey]map[string]TargetGroup),
		syncCh:      make(chan map[string][]TargetGroup),
		triggerSend: make(chan struct{}, 1),
		updatert:    updateInterval,
	}
}

// SyncCh returns the channel consumers read from.
func (m *DiscoveryManager) SyncCh() <-chan map[string][]TargetGroup {
	return m.syncCh
}

// RegisterDiscoverer adds a discoverer for a given scrape job (setName).
func (m *DiscoveryManager) RegisterDiscoverer(setName string, disc Discoverer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	provName := fmt.Sprintf("%s/%d", disc.Name(), len(m.providers))
	m.providers = append(m.providers, providerEntry{
		name:    provName,
		setName: setName,
		disc:    disc,
	})
}

// Run starts all discoverers and the sender loop.
// 실제 코드: manager.go:213-218, startProvider:324-335
func (m *DiscoveryManager) Run(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)

	// 각 provider를 개별 고루틴에서 시작
	for _, p := range m.providers {
		updates := make(chan []TargetGroup)
		go p.disc.Run(m.ctx, updates)
		go m.updater(p, updates)
	}

	// sender 고루틴 — 디바운싱된 병합 결과 전송
	go m.sender()
}

// updater는 개별 discoverer의 업데이트를 수신하여 targets 맵에 반영한다.
// 실제 코드: manager.go:355-383
func (m *DiscoveryManager) updater(p providerEntry, updates chan []TargetGroup) {
	pk := poolKey{setName: p.setName, provider: p.name}
	for {
		select {
		case <-m.ctx.Done():
			return
		case tgs, ok := <-updates:
			if !ok {
				<-m.ctx.Done()
				return
			}

			m.mu.Lock()
			if _, exists := m.targets[pk]; !exists {
				m.targets[pk] = make(map[string]TargetGroup)
			}
			// Source 기준 upsert/delete — 실제 코드: manager.go:427-447
			for _, tg := range tgs {
				if len(tg.Targets) > 0 {
					m.targets[pk][tg.Source] = tg
				} else {
					// 빈 타겟 그룹은 삭제 — 리소스 누수 방지
					delete(m.targets[pk], tg.Source)
				}
			}
			m.mu.Unlock()

			// triggerSend에 시그널 — 비차단 전송 (이미 pending이면 skip)
			// 실제 코드: manager.go:377-379
			select {
			case m.triggerSend <- struct{}{}:
			default:
			}
		}
	}
}

// sender는 updatert 간격의 ticker로 디바운싱하여 병합 결과를 syncCh에 전송한다.
// 실제 코드: manager.go:385-413
// 핵심: ticker.C를 기다린 뒤 triggerSend가 있을 때만 전송 → 최대 updatert 간격으로 제한
func (m *DiscoveryManager) sender() {
	ticker := time.NewTicker(m.updatert)
	defer func() {
		ticker.Stop()
		close(m.syncCh)
	}()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// ticker가 울린 시점에 triggerSend가 있으면 전송
			select {
			case <-m.triggerSend:
				merged := m.allGroups()
				select {
				case m.syncCh <- merged:
				default:
					// 소비자가 바쁘면 다음 주기에 재시도
					select {
					case m.triggerSend <- struct{}{}:
					default:
					}
				}
			default:
				// 업데이트 없음 — 아무것도 안 함
			}
		}
	}
}

// allGroups는 모든 provider의 타겟을 setName별로 병합한다.
// 실제 코드: manager.go:449-482
func (m *DiscoveryManager) allGroups() map[string][]TargetGroup {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string][]TargetGroup)
	for pk, sources := range m.targets {
		if _, ok := result[pk.setName]; !ok {
			result[pk.setName] = []TargetGroup{}
		}
		for _, tg := range sources {
			result[pk.setName] = append(result[pk.setName], tg)
		}
	}
	return result
}

// Stop cancels all discoverers.
func (m *DiscoveryManager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

// =============================================================================
// 7. ScrapeManager — 타겟 소비자
//    실제 코드: scrape/manager.go
//    SyncCh에서 타겟 업데이트를 받아 scrape pool을 생성/갱신/제거한다.
// =============================================================================

type ScrapeTarget struct {
	Address string
	Labels  map[string]string
}

type ScrapeManager struct {
	mu      sync.Mutex
	targets map[string]map[string]ScrapeTarget // jobName → address → target
}

func NewScrapeManager() *ScrapeManager {
	return &ScrapeManager{
		targets: make(map[string]map[string]ScrapeTarget),
	}
}

// ApplyTargets는 discovery manager에서 받은 전체 스냅샷을 적용한다.
// 새 타겟 추가, 사라진 타겟 제거를 수행한다.
func (sm *ScrapeManager) ApplyTargets(allTargets map[string][]TargetGroup) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for jobName, groups := range allTargets {
		newTargets := make(map[string]ScrapeTarget)

		for _, group := range groups {
			for _, target := range group.Targets {
				addr := target["__address__"]
				if addr == "" {
					continue
				}
				// 그룹 레이블과 타겟 레이블 병합
				merged := make(map[string]string)
				for k, v := range group.Labels {
					merged[k] = v
				}
				for k, v := range target {
					merged[k] = v
				}
				newTargets[addr] = ScrapeTarget{Address: addr, Labels: merged}
			}
		}

		// 제거된 타겟 감지
		if oldTargets, exists := sm.targets[jobName]; exists {
			for addr := range oldTargets {
				if _, ok := newTargets[addr]; !ok {
					fmt.Printf("  [scrape-manager] 타겟 제거: job=%s addr=%s\n", jobName, addr)
				}
			}
		}

		// 새로 추가된 타겟 감지
		for addr := range newTargets {
			if oldTargets, exists := sm.targets[jobName]; !exists || oldTargets[addr].Address == "" {
				fmt.Printf("  [scrape-manager] 타겟 추가: job=%s addr=%s labels=%v\n",
					jobName, addr, newTargets[addr].Labels)
			}
		}

		sm.targets[jobName] = newTargets
	}

	// 사라진 job 감지
	for jobName := range sm.targets {
		if _, ok := allTargets[jobName]; !ok {
			fmt.Printf("  [scrape-manager] job 제거: %s\n", jobName)
			delete(sm.targets, jobName)
		}
	}
}

func (sm *ScrapeManager) PrintStatus() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	total := 0
	for jobName, targets := range sm.targets {
		fmt.Printf("    job=%s: %d 타겟\n", jobName, len(targets))
		for addr := range targets {
			fmt.Printf("      - %s\n", addr)
		}
		total += len(targets)
	}
	fmt.Printf("    총 활성 타겟: %d\n", total)
}

// =============================================================================
// 8. Demo — 전체 라이프사이클 시연
// =============================================================================

func main() {
	fmt.Println("=== Prometheus Service Discovery PoC ===")
	fmt.Println()

	// ---- 임시 파일 준비 (FileDiscoverer용) ----
	tmpDir, err := os.MkdirTemp("", "prom-sd-poc")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	targetFile := filepath.Join(tmpDir, "targets.json")
	initialFileTargets := `[{"targets":["file-host-1:9100","file-host-2:9100"],"labels":{"env":"staging","source":"file"}}]`
	if err := os.WriteFile(targetFile, []byte(initialFileTargets), 0644); err != nil {
		fmt.Printf("타겟 파일 쓰기 실패: %v\n", err)
		return
	}

	// ---- Discovery Manager 생성 (디바운싱 간격 5초 — Prometheus 기본값) ----
	fmt.Println("[1] DiscoveryManager 생성 (updatert=5s)")
	manager := NewDiscoveryManager(5 * time.Second)

	// ---- Discoverer 등록 ----
	fmt.Println("[2] Discoverer 등록")

	// Static SD — "prometheus" job
	fmt.Println("  - StaticDiscoverer: prometheus job (localhost:9090)")
	manager.RegisterDiscoverer("prometheus", NewStaticDiscoverer([]TargetGroup{
		{
			Targets: []map[string]string{{"__address__": "localhost:9090"}},
			Labels:  map[string]string{"job": "prometheus", "env": "production"},
			Source:  "static:prometheus:0",
		},
	}))

	// Static SD — "node-exporter" job
	fmt.Println("  - StaticDiscoverer: node-exporter job (node-1:9100, node-2:9100)")
	manager.RegisterDiscoverer("node-exporter", NewStaticDiscoverer([]TargetGroup{
		{
			Targets: []map[string]string{
				{"__address__": "node-1:9100"},
				{"__address__": "node-2:9100"},
			},
			Labels: map[string]string{"job": "node", "env": "production"},
			Source: "static:node:0",
		},
	}))

	// File SD — "file-targets" job (3초 폴링)
	fmt.Printf("  - FileDiscoverer: file-targets job (파일=%s, 폴링=3s)\n", targetFile)
	manager.RegisterDiscoverer("file-targets", NewFileDiscoverer(targetFile, 3*time.Second))

	// DNS SD — "api-service" job (4초 주기)
	fmt.Println("  - DNSDiscoverer: api-service job (도메인=api.internal.local)")
	manager.RegisterDiscoverer("api-service", NewDNSDiscoverer(
		"api.internal.local",
		[]string{"10.0.1.1:8080", "10.0.1.2:8080"},
		4*time.Second,
	))

	// ---- ScrapeManager 생성 및 소비 시작 ----
	fmt.Println("[3] ScrapeManager 시작 — SyncCh에서 타겟 업데이트 수신")
	fmt.Println()

	scrapeManager := NewScrapeManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ScrapeManager 고루틴: syncCh에서 업데이트 수신
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		updateCount := 0
		for targets := range manager.SyncCh() {
			updateCount++
			fmt.Printf("--- [업데이트 #%d] ScrapeManager가 타겟 스냅샷 수신 ---\n", updateCount)
			scrapeManager.ApplyTargets(targets)
			fmt.Println("  현재 활성 타겟:")
			scrapeManager.PrintStatus()
			fmt.Println()
		}
	}()

	// ---- Manager 시작 ----
	fmt.Println("[4] DiscoveryManager.Run() — 모든 discoverer 시작")
	fmt.Println("    (5초 디바운싱 후 첫 업데이트가 syncCh로 전송됨)")
	fmt.Println()
	manager.Run(ctx)

	// ---- 시뮬레이션: 파일 변경으로 타겟 추가 ----
	time.Sleep(7 * time.Second) // 첫 디바운싱 주기 대기

	fmt.Println("=== [시뮬레이션] 파일 변경: 새 타겟 추가 (file-host-3:9100) ===")
	updatedFileTargets := `[
  {"targets":["file-host-1:9100","file-host-2:9100","file-host-3:9100"],"labels":{"env":"staging","source":"file"}},
  {"targets":["file-host-4:9200"],"labels":{"env":"canary","source":"file"}}
]`
	if err := os.WriteFile(targetFile, []byte(updatedFileTargets), 0644); err != nil {
		fmt.Printf("파일 업데이트 실패: %v\n", err)
	}
	fmt.Println("    → FileDiscoverer가 다음 폴링에서 변경 감지 예정")
	fmt.Println()

	// 파일 변경 감지 + 디바운싱 대기
	time.Sleep(10 * time.Second)

	// ---- 시뮬레이션: 파일에서 타겟 제거 ----
	fmt.Println("=== [시뮬레이션] 파일 변경: 타겟 축소 (canary 그룹 제거) ===")
	reducedFileTargets := `[{"targets":["file-host-1:9100"],"labels":{"env":"staging","source":"file"}}]`
	if err := os.WriteFile(targetFile, []byte(reducedFileTargets), 0644); err != nil {
		fmt.Printf("파일 업데이트 실패: %v\n", err)
	}
	fmt.Println("    → 타겟 축소 시 Source 기반 그룹 제거가 동작함")
	fmt.Println()

	// 변경 감지 + 디바운싱 대기
	time.Sleep(10 * time.Second)

	// ---- 종료 ----
	fmt.Println("=== [종료] DiscoveryManager 중지 ===")
	manager.Stop()
	cancel()
	wg.Wait()

	fmt.Println()
	fmt.Println("=== PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 학습 포인트:")
	fmt.Println("  1. Discoverer 인터페이스: Run(ctx, ch) — 모든 SD 메커니즘의 공통 계약")
	fmt.Println("  2. DiscoveryManager: poolKey(setName+provider)로 타겟 저장, Source로 dedup")
	fmt.Println("  3. 디바운싱: triggerSend + ticker(5s)로 빈번한 업데이트를 묶어 전송")
	fmt.Println("  4. 전체 스냅샷: ScrapeManager는 diff가 아닌 전체 상태를 받아 처리")
	fmt.Println("  5. 타겟 라이프사이클: 파일 변경 → Discoverer 감지 → Manager 병합 → ScrapeManager 적용")
}
