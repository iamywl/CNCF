package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Loki PoC #15: 압축(Compaction) - 인덱스/청크 압축 및 보존 정책
// =============================================================================
//
// Loki의 Compactor는 여러 작은 인덱스 파일을 하나로 합치고,
// 보존 정책(Retention)에 따라 만료된 데이터를 삭제한다.
//
// 핵심 개념:
// 1. 인덱스 압축: 여러 작은 인덱스 파일 → 하나의 큰 파일로 병합
// 2. 보존 정책: TTL 기반 만료 + Mark-Sweep 삭제
// 3. Mark-Sweep: Phase 1(마킹) → Phase 2(스위핑) 2단계 삭제
// 4. 리더 선출: Ring 기반 단일 Compactor 실행 보장
//
// 참조: pkg/compactor/compactor.go, table.go, tables_manager.go

// =============================================================================
// IndexEntry: 인덱스 항목
// =============================================================================

// IndexEntry 는 하나의 인덱스 항목을 나타낸다
type IndexEntry struct {
	TenantID  string
	StreamID  string
	Labels    map[string]string
	ChunkRef  string    // 청크 참조 ID
	From      time.Time // 시작 시간
	Through   time.Time // 종료 시간
	CreatedAt time.Time // 생성 시간
}

// IsExpired 는 보존 기간에 따라 만료 여부를 확인한다
func (e *IndexEntry) IsExpired(retentionPeriod time.Duration, now time.Time) bool {
	return now.Sub(e.Through) > retentionPeriod
}

// =============================================================================
// IndexFile: 인덱스 파일
// =============================================================================
// Loki에서 각 인덱스 파일은 BoltDB 등의 임베디드 DB로 구현된다.
// Ingester가 주기적으로 작은 인덱스 파일을 생성하고,
// Compactor가 이를 합쳐 큰 파일로 만든다.

// IndexFile 은 하나의 인덱스 파일을 나타낸다
type IndexFile struct {
	Name      string
	Entries   []IndexEntry
	CreatedAt time.Time
	Compacted bool // 이미 압축된 파일인지
}

// Size 는 인덱스 파일의 크기(항목 수)를 반환한다
func (f *IndexFile) Size() int {
	return len(f.Entries)
}

// =============================================================================
// Table: 시간 범위별 인덱스 테이블
// =============================================================================
// Loki 실제 코드: pkg/compactor/table.go
// 각 테이블은 특정 시간 범위(보통 24시간)에 해당하는 인덱스 파일들을 관리한다.

// Table 은 시간 범위별 인덱스 테이블을 나타낸다
type Table struct {
	Name       string
	Files      []*IndexFile
	TimeRange  TimeRange
	Compacted  bool
}

// TimeRange 는 시간 범위를 나타낸다
type TimeRange struct {
	From    time.Time
	Through time.Time
}

// =============================================================================
// DeletionMarker: 삭제 마커
// =============================================================================
// Loki 실제 코드: pkg/compactor/retention/ → Mark-Sweep 패턴
// Phase 1에서 만료된 청크에 마커를 기록하고,
// Phase 2에서 마커가 있는 청크를 실제로 삭제한다.

// DeletionMarker 는 삭제 대상 청크의 마커
type DeletionMarker struct {
	ChunkRef  string
	TenantID  string
	MarkedAt  time.Time
	Reason    string // "retention" 또는 "user_request"
}

// =============================================================================
// RetentionConfig: 보존 정책 설정
// =============================================================================

// RetentionConfig 는 테넌트별 보존 정책을 정의한다
type RetentionConfig struct {
	DefaultRetention time.Duration            // 기본 보존 기간
	PerTenant        map[string]time.Duration  // 테넌트별 보존 기간
}

// GetRetention 은 테넌트의 보존 기간을 반환한다
func (c *RetentionConfig) GetRetention(tenantID string) time.Duration {
	if d, ok := c.PerTenant[tenantID]; ok {
		return d
	}
	return c.DefaultRetention
}

// =============================================================================
// Compactor: 압축기
// =============================================================================
// Loki 실제 코드: pkg/compactor/compactor.go
//
// 동작 흐름:
// 1. Ring에서 리더 선출 (단일 Compactor만 실행)
// 2. 테이블 목록 조회
// 3. 각 테이블에 대해:
//    a. 인덱스 파일들을 하나로 합침 (Compact)
//    b. 보존 정책 적용 (Mark → Sweep)
// 4. 결과를 스토리지에 업로드

// Compactor 는 인덱스 압축기를 시뮬레이션한다
type Compactor struct {
	mu              sync.Mutex
	tables          map[string]*Table
	retentionConfig *RetentionConfig
	markers         []DeletionMarker
	isLeader        bool
	instanceID      string

	// 통계
	compactedTables int
	markedChunks    int
	sweptChunks     int
}

// NewCompactor 는 새 Compactor를 생성한다
func NewCompactor(instanceID string, retentionConfig *RetentionConfig) *Compactor {
	return &Compactor{
		tables:          make(map[string]*Table),
		retentionConfig: retentionConfig,
		instanceID:      instanceID,
	}
}

// AddTable 은 테이블을 추가한다
func (c *Compactor) AddTable(table *Table) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tables[table.Name] = table
}

// =============================================================================
// CompactTable: 인덱스 파일 병합
// =============================================================================
// Loki 실제 코드: pkg/compactor/table.go → compact()
//
// 여러 작은 인덱스 파일을 하나의 큰 파일로 병합한다.
// - 중복 제거
// - 정렬
// - 만료된 항목 필터링

func (c *Compactor) CompactTable(tableName string) (*IndexFile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	table, ok := c.tables[tableName]
	if !ok {
		return nil, fmt.Errorf("테이블 '%s'를 찾을 수 없습니다", tableName)
	}

	if len(table.Files) == 0 {
		return nil, fmt.Errorf("테이블 '%s'에 인덱스 파일이 없습니다", tableName)
	}

	// 모든 파일의 항목을 수집
	var allEntries []IndexEntry
	for _, file := range table.Files {
		allEntries = append(allEntries, file.Entries...)
	}

	// 중복 제거 (ChunkRef 기준)
	seen := make(map[string]bool)
	uniqueEntries := make([]IndexEntry, 0, len(allEntries))
	for _, entry := range allEntries {
		if !seen[entry.ChunkRef] {
			seen[entry.ChunkRef] = true
			uniqueEntries = append(uniqueEntries, entry)
		}
	}

	// 시간순 정렬
	sort.Slice(uniqueEntries, func(i, j int) bool {
		return uniqueEntries[i].From.Before(uniqueEntries[j].From)
	})

	// 압축된 파일 생성
	compactedFile := &IndexFile{
		Name:      fmt.Sprintf("%s-compacted", tableName),
		Entries:   uniqueEntries,
		CreatedAt: time.Now(),
		Compacted: true,
	}

	// 기존 파일을 압축된 파일로 교체
	table.Files = []*IndexFile{compactedFile}
	table.Compacted = true
	c.compactedTables++

	return compactedFile, nil
}

// =============================================================================
// Mark-Sweep 삭제
// =============================================================================
// Loki 실제 코드: pkg/compactor/retention/
//
// Phase 1 (Mark): 보존 정책에 따라 만료된 청크에 삭제 마커 기록
// Phase 2 (Sweep): 마커가 있는 청크를 실제로 삭제
//
// 2단계로 나누는 이유:
// - 마킹은 빠르지만, 실제 삭제는 I/O가 많이 발생
// - 장애 시 마킹부터 재시작 가능 (멱등성)
// - 삭제 확인 후 마커 제거

// MarkExpired 는 만료된 청크에 삭제 마커를 기록한다 (Phase 1)
// Loki 실제 코드: retention.Marker → MarkForDelete()
func (c *Compactor) MarkExpired(now time.Time) []DeletionMarker {
	c.mu.Lock()
	defer c.mu.Unlock()

	var newMarkers []DeletionMarker

	for _, table := range c.tables {
		for _, file := range table.Files {
			for _, entry := range file.Entries {
				retention := c.retentionConfig.GetRetention(entry.TenantID)
				if entry.IsExpired(retention, now) {
					marker := DeletionMarker{
						ChunkRef: entry.ChunkRef,
						TenantID: entry.TenantID,
						MarkedAt: now,
						Reason:   "retention",
					}
					newMarkers = append(newMarkers, marker)
					c.markedChunks++
				}
			}
		}
	}

	c.markers = append(c.markers, newMarkers...)
	return newMarkers
}

// Sweep 는 삭제 마커가 있는 청크를 실제로 제거한다 (Phase 2)
// Loki 실제 코드: retention.Sweeper → sweepTable()
func (c *Compactor) Sweep() (removed int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 마커가 있는 ChunkRef 수집
	markedRefs := make(map[string]bool)
	for _, marker := range c.markers {
		markedRefs[marker.ChunkRef] = true
	}

	// 각 테이블에서 마커가 있는 항목 제거
	for _, table := range c.tables {
		for _, file := range table.Files {
			cleanEntries := make([]IndexEntry, 0, len(file.Entries))
			for _, entry := range file.Entries {
				if !markedRefs[entry.ChunkRef] {
					cleanEntries = append(cleanEntries, entry)
				} else {
					removed++
					c.sweptChunks++
				}
			}
			file.Entries = cleanEntries
		}
	}

	// 처리된 마커 제거
	c.markers = nil

	return removed
}

// =============================================================================
// LeaderElection: Ring 기반 리더 선출
// =============================================================================
// Loki 실제 코드: pkg/compactor/compactor.go → loop()
//
// Ring을 사용하여 단일 Compactor 인스턴스만 실행되도록 보장한다.
// - 각 인스턴스는 Ring에 등록
// - ringKeyOfLeader(0)를 기준으로 리더 결정
// - 리더만 압축 작업을 수행

// CompactorRing 은 Compactor 리더 선출을 위한 Ring을 시뮬레이션한다
type CompactorRing struct {
	mu        sync.Mutex
	instances map[string]*RingInstance
}

// RingInstance 는 Ring에 등록된 인스턴스
type RingInstance struct {
	ID       string
	Addr     string
	Token    uint32
	State    string // "JOINING", "ACTIVE", "LEAVING"
	IsLeader bool
}

// NewCompactorRing 은 새 Ring을 생성한다
func NewCompactorRing() *CompactorRing {
	return &CompactorRing{
		instances: make(map[string]*RingInstance),
	}
}

// Register 는 인스턴스를 Ring에 등록한다
func (r *CompactorRing) Register(id, addr string) *RingInstance {
	r.mu.Lock()
	defer r.mu.Unlock()

	instance := &RingInstance{
		ID:    id,
		Addr:  addr,
		Token: rand.Uint32(),
		State: "ACTIVE",
	}
	r.instances[id] = instance

	// 리더 재선출
	r.electLeader()

	return instance
}

// Deregister 는 인스턴스를 Ring에서 제거한다
func (r *CompactorRing) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.instances, id)
	r.electLeader()
}

// electLeader 는 가장 작은 토큰을 가진 인스턴스를 리더로 선출한다
// Loki 실제 코드: ring.Get(ringKeyOfLeader, ring.Write, ...) → 단일 인스턴스 선출
func (r *CompactorRing) electLeader() {
	// 모든 인스턴스의 리더 플래그 초기화
	for _, inst := range r.instances {
		inst.IsLeader = false
	}

	if len(r.instances) == 0 {
		return
	}

	// 가장 작은 토큰을 가진 인스턴스가 리더
	var leader *RingInstance
	for _, inst := range r.instances {
		if inst.State == "ACTIVE" {
			if leader == nil || inst.Token < leader.Token {
				leader = inst
			}
		}
	}
	if leader != nil {
		leader.IsLeader = true
	}
}

// GetLeader 는 현재 리더를 반환한다
func (r *CompactorRing) GetLeader() *RingInstance {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, inst := range r.instances {
		if inst.IsLeader {
			return inst
		}
	}
	return nil
}

// =============================================================================
// 헬퍼 함수
// =============================================================================

// generateIndexFiles 는 테스트용 인덱스 파일들을 생성한다
func generateIndexFiles(tableName string, numFiles int, baseTime time.Time, entriesPerFile int) []*IndexFile {
	tenants := []string{"tenant-1", "tenant-2", "tenant-3"}
	files := make([]*IndexFile, numFiles)

	for i := 0; i < numFiles; i++ {
		entries := make([]IndexEntry, entriesPerFile)
		for j := 0; j < entriesPerFile; j++ {
			tenant := tenants[rand.Intn(len(tenants))]
			entryTime := baseTime.Add(time.Duration(i*entriesPerFile+j) * time.Minute)
			entries[j] = IndexEntry{
				TenantID:  tenant,
				StreamID:  fmt.Sprintf("stream-%d", rand.Intn(10)),
				Labels:    map[string]string{"app": "web", "env": "prod"},
				ChunkRef:  fmt.Sprintf("chunk-%s-%d-%d", tableName, i, j),
				From:      entryTime,
				Through:   entryTime.Add(5 * time.Minute),
				CreatedAt: entryTime,
			}
		}
		files[i] = &IndexFile{
			Name:      fmt.Sprintf("%s-file-%d", tableName, i),
			Entries:   entries,
			CreatedAt: baseTime.Add(time.Duration(i) * time.Hour),
		}
	}
	return files
}

func labelsToString(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ", ") + "}"
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== Loki 압축(Compaction) 및 보존 정책 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 1단계: 리더 선출 (Ring 기반)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [1] Ring 기반 리더 선출 ---")
	fmt.Println()

	ring := NewCompactorRing()

	// 3개의 Compactor 인스턴스 등록
	inst1 := ring.Register("compactor-1", "10.0.0.1:3100")
	inst2 := ring.Register("compactor-2", "10.0.0.2:3100")
	inst3 := ring.Register("compactor-3", "10.0.0.3:3100")

	fmt.Println("  등록된 인스턴스:")
	for _, inst := range []*RingInstance{inst1, inst2, inst3} {
		leaderStr := ""
		if inst.IsLeader {
			leaderStr = " ← 리더"
		}
		fmt.Printf("    %s (토큰: %d, 상태: %s)%s\n", inst.ID, inst.Token, inst.State, leaderStr)
	}

	leader := ring.GetLeader()
	fmt.Printf("\n  현재 리더: %s\n", leader.ID)
	fmt.Println("  → 리더만 압축 작업을 수행합니다")
	fmt.Println()

	// 리더 장애 시 재선출
	fmt.Printf("  [이벤트] %s 제거 (장애 시뮬레이션)\n", leader.ID)
	ring.Deregister(leader.ID)
	newLeader := ring.GetLeader()
	fmt.Printf("  새 리더: %s\n", newLeader.ID)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 2단계: 인덱스 파일 병합 (Compaction)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [2] 인덱스 파일 병합 (Compaction) ---")
	fmt.Println()

	// 보존 정책 설정
	retentionConfig := &RetentionConfig{
		DefaultRetention: 7 * 24 * time.Hour, // 기본 7일
		PerTenant: map[string]time.Duration{
			"tenant-1": 30 * 24 * time.Hour,   // tenant-1: 30일
			"tenant-3": 3 * 24 * time.Hour,    // tenant-3: 3일
		},
	}

	compactor := NewCompactor("compactor-1", retentionConfig)

	// 테스트용 인덱스 파일 생성
	baseTime := time.Now().Add(-10 * 24 * time.Hour) // 10일 전
	files := generateIndexFiles("table-2024-01-15", 5, baseTime, 4)

	table := &Table{
		Name:  "table-2024-01-15",
		Files: files,
		TimeRange: TimeRange{
			From:    baseTime,
			Through: baseTime.Add(24 * time.Hour),
		},
	}
	compactor.AddTable(table)

	// 압축 전 상태
	fmt.Println("  압축 전:")
	totalEntries := 0
	for _, file := range table.Files {
		fmt.Printf("    파일: %-30s | 항목 수: %d | 생성: %s\n",
			file.Name, file.Size(), file.CreatedAt.Format("2006-01-02 15:04"))
		totalEntries += file.Size()
	}
	fmt.Printf("    총 파일 수: %d, 총 항목 수: %d\n", len(table.Files), totalEntries)
	fmt.Println()

	// 압축 실행
	compactedFile, err := compactor.CompactTable("table-2024-01-15")
	if err != nil {
		fmt.Printf("  압축 오류: %s\n", err)
		return
	}

	fmt.Println("  압축 후:")
	fmt.Printf("    파일: %-30s | 항목 수: %d | 압축됨: %v\n",
		compactedFile.Name, compactedFile.Size(), compactedFile.Compacted)
	fmt.Printf("    중복 제거: %d → %d 항목\n", totalEntries, compactedFile.Size())
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 3단계: 보존 정책 (Retention)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [3] 보존 정책 (Retention) ---")
	fmt.Println()

	fmt.Println("  보존 정책 설정:")
	fmt.Printf("    기본 보존 기간: %s\n", retentionConfig.DefaultRetention)
	for tenant, duration := range retentionConfig.PerTenant {
		fmt.Printf("    %s: %s\n", tenant, duration)
	}
	fmt.Println()

	// 현재 시간 기준으로 만료 여부 확인
	now := time.Now()
	fmt.Println("  테넌트별 만료 상태:")
	tenantExpired := make(map[string]int)
	tenantTotal := make(map[string]int)
	for _, entry := range compactedFile.Entries {
		tenantTotal[entry.TenantID]++
		retention := retentionConfig.GetRetention(entry.TenantID)
		if entry.IsExpired(retention, now) {
			tenantExpired[entry.TenantID]++
		}
	}

	tenantIDs := make([]string, 0, len(tenantTotal))
	for t := range tenantTotal {
		tenantIDs = append(tenantIDs, t)
	}
	sort.Strings(tenantIDs)

	for _, tid := range tenantIDs {
		retention := retentionConfig.GetRetention(tid)
		expired := tenantExpired[tid]
		total := tenantTotal[tid]
		fmt.Printf("    %s: 보존=%s, 만료=%d/%d\n", tid, retention, expired, total)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 4단계: Mark-Sweep 삭제
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [4] Mark-Sweep 삭제 ---")
	fmt.Println()

	// Phase 1: 마킹
	fmt.Println("  Phase 1: 만료된 청크 마킹")
	markers := compactor.MarkExpired(now)
	fmt.Printf("    마킹된 청크 수: %d\n", len(markers))
	if len(markers) > 0 {
		fmt.Println("    마킹된 청크:")
		for i, marker := range markers {
			if i >= 5 {
				fmt.Printf("    ... (총 %d개)\n", len(markers))
				break
			}
			fmt.Printf("      %s (테넌트: %s, 사유: %s)\n",
				marker.ChunkRef, marker.TenantID, marker.Reason)
		}
	}
	fmt.Println()

	// Phase 2: 스위핑
	fmt.Println("  Phase 2: 마킹된 청크 삭제 (스위핑)")
	beforeSweep := compactedFile.Size()
	removed := compactor.Sweep()
	afterSweep := compactedFile.Size()

	fmt.Printf("    삭제 전: %d 항목\n", beforeSweep)
	fmt.Printf("    삭제됨: %d 항목\n", removed)
	fmt.Printf("    삭제 후: %d 항목\n", afterSweep)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 5단계: 다중 테이블 압축 시뮬레이션
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [5] 다중 테이블 압축 시뮬레이션 ---")
	fmt.Println()

	multiCompactor := NewCompactor("compactor-leader", retentionConfig)

	// 여러 날짜의 테이블 생성
	tableNames := []string{
		"index_2024-01-10",
		"index_2024-01-11",
		"index_2024-01-12",
		"index_2024-01-13",
		"index_2024-01-14",
	}

	for i, name := range tableNames {
		tableBaseTime := time.Now().Add(-time.Duration(14-i) * 24 * time.Hour)
		files := generateIndexFiles(name, 3+rand.Intn(4), tableBaseTime, 3+rand.Intn(3))
		t := &Table{
			Name:  name,
			Files: files,
			TimeRange: TimeRange{
				From:    tableBaseTime,
				Through: tableBaseTime.Add(24 * time.Hour),
			},
		}
		multiCompactor.AddTable(t)
	}

	// 최신 테이블부터 압축 (Loki 실제 동작)
	// Loki 실제 코드: SortTablesByRange() → 최신 먼저
	sort.Slice(tableNames, func(i, j int) bool {
		return tableNames[i] > tableNames[j]
	})

	fmt.Println("  테이블별 압축 결과 (최신 먼저):")
	fmt.Printf("  %-25s | 파일수 | 압축전 | 압축후\n", "테이블")
	fmt.Println("  " + strings.Repeat("-", 65))

	for _, name := range tableNames {
		table := multiCompactor.tables[name]
		beforeFiles := len(table.Files)
		beforeEntries := 0
		for _, f := range table.Files {
			beforeEntries += f.Size()
		}

		compactedFile, err := multiCompactor.CompactTable(name)
		if err != nil {
			fmt.Printf("  %-25s | 오류: %s\n", name, err)
			continue
		}
		fmt.Printf("  %-25s | %5d  | %6d | %6d\n",
			name, beforeFiles, beforeEntries, compactedFile.Size())
	}
	fmt.Println()

	// Mark-Sweep 실행
	fmt.Println("  전체 Mark-Sweep:")
	markers = multiCompactor.MarkExpired(now)
	fmt.Printf("    마킹: %d 청크\n", len(markers))
	removed = multiCompactor.Sweep()
	fmt.Printf("    삭제: %d 청크\n", removed)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 6단계: 통계 요약
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [6] 압축 통계 요약 ---")
	fmt.Println()

	fmt.Printf("  압축된 테이블 수: %d\n", multiCompactor.compactedTables)
	fmt.Printf("  마킹된 청크 수: %d\n", multiCompactor.markedChunks)
	fmt.Printf("  삭제된 청크 수: %d\n", multiCompactor.sweptChunks)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 동작 원리 요약
	// ─────────────────────────────────────────────────────────────
	fmt.Println("=== 압축(Compaction) 동작 원리 요약 ===")
	fmt.Println()
	fmt.Println("  1. 리더 선출: Ring에서 단일 Compactor만 실행")
	fmt.Println("     → ringKeyOfLeader(0)을 기준으로 리더 결정")
	fmt.Println("  2. 인덱스 압축: 여러 작은 파일 → 하나의 큰 파일로 병합")
	fmt.Println("     → 중복 제거, 시간순 정렬")
	fmt.Println("  3. 보존 정책: TTL 기반 만료 판단")
	fmt.Println("     → 테넌트별 개별 보존 기간 설정 가능")
	fmt.Println("  4. Mark-Sweep 삭제:")
	fmt.Println("     → Phase 1: 만료된 청크에 삭제 마커 기록 (멱등적)")
	fmt.Println("     → Phase 2: 마커가 있는 청크를 실제 삭제 (스위핑)")
	fmt.Println()
	fmt.Println("  Loki 핵심 코드 경로:")
	fmt.Println("  - pkg/compactor/compactor.go       → Compactor 구조체, loop(), 리더 선출")
	fmt.Println("  - pkg/compactor/table.go            → 테이블별 압축 로직")
	fmt.Println("  - pkg/compactor/tables_manager.go   → 테이블 관리 및 실행")
	fmt.Println("  - pkg/compactor/retention/           → Mark-Sweep 보존 정책")
}
