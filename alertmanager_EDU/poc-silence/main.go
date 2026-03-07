// Alertmanager Silence PoC
//
// Alertmanager의 Silence 시스템을 시뮬레이션한다.
// CRUD, 버전 기반 캐시, 상태 전이, Matcher 매칭을 재현한다.
//
// 핵심 개념:
//   - Silence 생성/만료/조회
//   - 버전 기반 캐시 (lazy invalidation)
//   - Silence 상태: Pending → Active → Expired → GC
//   - Matchers AND 매칭
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// LabelSet은 레이블 집합이다.
type LabelSet map[string]string

// Fingerprint는 LabelSet의 해시값이다 (간소화 버전).
func Fingerprint(ls LabelSet) string {
	var parts []string
	for k, v := range ls {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

// Matcher는 레이블 매칭 조건이다.
type Matcher struct {
	Name  string
	Value string
	IsRe  bool // 정규식 매칭 (간소화: prefix 매칭)
}

// Matches는 값이 Matcher 조건에 맞는지 확인한다.
func (m *Matcher) Matches(val string) bool {
	if m.IsRe {
		return strings.HasPrefix(val, m.Value) // 간소화된 정규식
	}
	return val == m.Value
}

// SilenceState는 Silence의 상태이다.
type SilenceState int

const (
	StatePending SilenceState = iota
	StateActive
	StateExpired
)

func (s SilenceState) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateActive:
		return "active"
	case StateExpired:
		return "expired"
	}
	return "unknown"
}

// Silence는 특정 조건의 Alert를 억제하는 규칙이다.
type Silence struct {
	ID        string
	Matchers  []*Matcher
	StartsAt  time.Time
	EndsAt    time.Time
	UpdatedAt time.Time
	CreatedBy string
	Comment   string
}

// State는 Silence의 현재 상태를 반환한다.
func (s *Silence) State(now time.Time) SilenceState {
	if now.Before(s.StartsAt) {
		return StatePending
	}
	if now.Before(s.EndsAt) {
		return StateActive
	}
	return StateExpired
}

// Matches는 LabelSet이 모든 Matcher와 일치하는지 확인한다 (AND).
func (s *Silence) Matches(lset LabelSet) bool {
	for _, m := range s.Matchers {
		val, ok := lset[m.Name]
		if !ok {
			return false
		}
		if !m.Matches(val) {
			return false
		}
	}
	return true
}

// cacheEntry는 캐시의 엔트리이다.
type cacheEntry struct {
	version    int
	silenceIDs []string
}

// Silences는 Silence 저장소이다.
type Silences struct {
	mu      sync.RWMutex
	st      map[string]*Silence // ID → Silence
	version int                 // 변경 시 증가
	nextID  int

	// 캐시: fingerprint → cacheEntry
	cache map[string]*cacheEntry
}

// NewSilences는 새 Silences 저장소를 생성한다.
func NewSilences() *Silences {
	return &Silences{
		st:    make(map[string]*Silence),
		cache: make(map[string]*cacheEntry),
	}
}

// Set은 새 Silence를 생성하거나 기존 Silence를 업데이트한다.
func (s *Silences) Set(sil *Silence) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 유효성 검증
	if len(sil.Matchers) == 0 {
		return "", fmt.Errorf("최소 1개의 Matcher 필요")
	}
	if !sil.StartsAt.Before(sil.EndsAt) {
		return "", fmt.Errorf("StartsAt은 EndsAt보다 이전이어야 함")
	}

	// ID 생성
	if sil.ID == "" {
		s.nextID++
		sil.ID = fmt.Sprintf("sil-%03d", s.nextID)
	}
	sil.UpdatedAt = time.Now()

	s.st[sil.ID] = sil
	s.version++ // 캐시 무효화

	return sil.ID, nil
}

// Expire는 Silence를 즉시 만료시킨다.
func (s *Silences) Expire(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sil, ok := s.st[id]
	if !ok {
		return fmt.Errorf("Silence %s not found", id)
	}

	now := time.Now()
	if sil.State(now) == StateExpired {
		return fmt.Errorf("Silence %s already expired", id)
	}

	sil.EndsAt = now
	sil.UpdatedAt = now
	s.version++

	return nil
}

// Query는 조건에 맞는 Silence를 조회한다.
func (s *Silences) Query(states ...SilenceState) []*Silence {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	stateSet := make(map[SilenceState]bool)
	for _, st := range states {
		stateSet[st] = true
	}

	var result []*Silence
	for _, sil := range s.st {
		state := sil.State(now)
		if len(stateSet) == 0 || stateSet[state] {
			result = append(result, sil)
		}
	}
	return result
}

// Mutes는 LabelSet이 활성 Silence에 의해 억제되는지 확인한다.
// 버전 기반 캐시를 사용하여 성능을 최적화한다.
func (s *Silences) Mutes(lset LabelSet) (bool, []string) {
	fp := Fingerprint(lset)

	s.mu.RLock()
	currentVersion := s.version

	// 캐시 확인
	if entry, ok := s.cache[fp]; ok && entry.version == currentVersion {
		s.mu.RUnlock()
		fmt.Printf("    [캐시 HIT] fp=%s, version=%d\n", fp, currentVersion)
		return len(entry.silenceIDs) > 0, entry.silenceIDs
	}
	s.mu.RUnlock()

	fmt.Printf("    [캐시 MISS] fp=%s, version=%d → 전체 매칭 수행\n", fp, currentVersion)

	// 캐시 미스 → 전체 활성 Silence 매칭
	s.mu.RLock()
	now := time.Now()
	var matchedIDs []string
	for _, sil := range s.st {
		if sil.State(now) == StateActive && sil.Matches(lset) {
			matchedIDs = append(matchedIDs, sil.ID)
		}
	}
	s.mu.RUnlock()

	// 캐시 저장
	s.mu.Lock()
	s.cache[fp] = &cacheEntry{
		version:    currentVersion,
		silenceIDs: matchedIDs,
	}
	s.mu.Unlock()

	return len(matchedIDs) > 0, matchedIDs
}

// GC는 만료 후 retention 기간이 지난 Silence를 삭제한다.
func (s *Silences) GC(retention time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	deleted := 0
	for id, sil := range s.st {
		if sil.State(now) == StateExpired && now.Sub(sil.EndsAt) > retention {
			delete(s.st, id)
			deleted++
			s.version++
		}
	}
	return deleted
}

func main() {
	fmt.Println("=== Alertmanager Silence PoC ===")
	fmt.Println()

	silences := NewSilences()

	// 1. Silence 생성
	fmt.Println("--- 1. Silence 생성 ---")
	now := time.Now()

	id1, _ := silences.Set(&Silence{
		Matchers:  []*Matcher{{Name: "alertname", Value: "HighCPU"}},
		StartsAt:  now.Add(-1 * time.Hour),
		EndsAt:    now.Add(1 * time.Hour),
		CreatedBy: "admin",
		Comment:   "CPU 유지보수",
	})
	fmt.Printf("Silence %s 생성: alertname=HighCPU (1시간 후 만료)\n", id1)

	id2, _ := silences.Set(&Silence{
		Matchers:  []*Matcher{{Name: "severity", Value: "warning"}},
		StartsAt:  now.Add(-30 * time.Minute),
		EndsAt:    now.Add(30 * time.Minute),
		CreatedBy: "admin",
		Comment:   "warning 일시 억제",
	})
	fmt.Printf("Silence %s 생성: severity=warning (30분 후 만료)\n", id2)

	id3, _ := silences.Set(&Silence{
		Matchers:  []*Matcher{{Name: "team", Value: "backend"}},
		StartsAt:  now.Add(1 * time.Hour), // 미래 시작
		EndsAt:    now.Add(2 * time.Hour),
		CreatedBy: "admin",
		Comment:   "백엔드 배포 예정",
	})
	fmt.Printf("Silence %s 생성: team=backend (Pending, 1시간 후 시작)\n", id3)
	fmt.Println()

	// 2. 상태 조회
	fmt.Println("--- 2. 상태별 조회 ---")
	active := silences.Query(StateActive)
	pending := silences.Query(StatePending)
	fmt.Printf("Active: %d개, Pending: %d개\n", len(active), len(pending))
	for _, s := range active {
		fmt.Printf("  [Active] %s: %v\n", s.ID, s.Comment)
	}
	for _, s := range pending {
		fmt.Printf("  [Pending] %s: %v\n", s.ID, s.Comment)
	}
	fmt.Println()

	// 3. Alert 매칭 (캐시 동작)
	fmt.Println("--- 3. Alert 매칭 (캐시 동작) ---")
	testLabels := []LabelSet{
		{"alertname": "HighCPU", "severity": "critical"},
		{"alertname": "HighMemory", "severity": "warning"},
		{"alertname": "DiskFull", "severity": "critical"},
		{"alertname": "HighCPU", "severity": "critical"}, // 캐시 HIT 테스트
	}

	for _, ls := range testLabels {
		fmt.Printf("  Alert %v:\n", ls)
		muted, ids := silences.Mutes(ls)
		if muted {
			fmt.Printf("    → MUTED by %v\n", ids)
		} else {
			fmt.Printf("    → 통과\n")
		}
	}
	fmt.Println()

	// 4. Silence 만료
	fmt.Println("--- 4. Silence 만료 ---")
	err := silences.Expire(id1)
	if err != nil {
		fmt.Printf("만료 오류: %v\n", err)
	} else {
		fmt.Printf("Silence %s 즉시 만료\n", id1)
	}
	fmt.Println()

	// 5. 만료 후 캐시 무효화 확인
	fmt.Println("--- 5. 캐시 무효화 확인 (version 변경) ---")
	fmt.Printf("  Alert {alertname: HighCPU}:\n")
	muted, ids := silences.Mutes(LabelSet{"alertname": "HighCPU", "severity": "critical"})
	if muted {
		fmt.Printf("    → MUTED by %v\n", ids)
	} else {
		fmt.Printf("    → 통과 (Silence 만료됨)\n")
	}
	fmt.Println()

	// 6. GC
	fmt.Println("--- 6. GC (retention=0, 즉시 삭제) ---")
	deleted := silences.GC(0)
	fmt.Printf("GC 결과: %d개 Silence 삭제\n", deleted)
	remaining := silences.Query()
	fmt.Printf("남은 Silence: %d개\n", len(remaining))

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. Silence 생성 시 version++ → 캐시 자동 무효화")
	fmt.Println("2. Mutes() 호출 시 캐시 version 확인 → HIT이면 즉시 반환")
	fmt.Println("3. 캐시 MISS이면 모든 활성 Silence와 매칭 후 결과 캐시")
	fmt.Println("4. Silence 만료/삭제 시 version++ → lazy invalidation")
	fmt.Println("5. GC로 retention 지난 만료 Silence 삭제")
}
