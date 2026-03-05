// Helm v4 스토리지 드라이버 PoC: Driver 인터페이스, Memory/File 드라이버
//
// 이 PoC는 Helm v4의 릴리스 저장소 패턴을 시뮬레이션합니다:
//   1. Driver 인터페이스 (pkg/storage/driver/driver.go) - CRUD + Query
//   2. Memory 드라이버 (pkg/storage/driver/memory.go) - 인메모리 저장소
//   3. File 드라이버 (Secrets/ConfigMaps 대신 파일 기반 구현)
//   4. Storage 래퍼 (pkg/storage/storage.go) - 키 생성, MaxHistory, 이력 관리
//   5. 레이블 기반 쿼리 (labels.go)
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Release: 간소화된 릴리스 구조체
// =============================================================================

type Status string

const (
	StatusDeployed    Status = "deployed"
	StatusSuperseded  Status = "superseded"
	StatusUninstalled Status = "uninstalled"
	StatusFailed      Status = "failed"
)

type Release struct {
	Name      string         `json:"name"`
	Version   int            `json:"version"`
	Namespace string         `json:"namespace"`
	Status    Status         `json:"status"`
	Chart     string         `json:"chart"`
	Config    map[string]any `json:"config,omitempty"`
	Manifest  string         `json:"manifest,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// =============================================================================
// 에러 정의: Helm의 pkg/storage/driver/driver.go
// =============================================================================

var (
	ErrReleaseNotFound = fmt.Errorf("release: not found")
	ErrReleaseExists   = fmt.Errorf("release: already exists")
	ErrInvalidKey      = fmt.Errorf("release: invalid key")
)

// =============================================================================
// Driver 인터페이스: Helm의 pkg/storage/driver/driver.go
// Creator + Updator + Deletor + Queryor 인터페이스 합성.
// 실제 구현체: Memory, Secrets, ConfigMaps, SQL
// =============================================================================

// Driver는 릴리스 저장소의 인터페이스이다.
// 실제 Helm: driver.Driver = Creator + Updator + Deletor + Queryor + Name()
type Driver interface {
	// Name은 드라이버 이름을 반환한다
	Name() string
	// Create는 새 릴리스를 저장한다 (이미 존재하면 ErrReleaseExists)
	Create(key string, rls *Release) error
	// Get은 키로 릴리스를 조회한다 (없으면 ErrReleaseNotFound)
	Get(key string) (*Release, error)
	// Update는 기존 릴리스를 갱신한다 (없으면 ErrReleaseNotFound)
	Update(key string, rls *Release) error
	// Delete는 릴리스를 삭제한다 (없으면 ErrReleaseNotFound)
	Delete(key string) (*Release, error)
	// List는 필터 조건에 맞는 릴리스 목록을 반환한다
	List(filter func(*Release) bool) ([]*Release, error)
	// Query는 레이블(키-값 쌍)으로 릴리스를 검색한다
	Query(labels map[string]string) ([]*Release, error)
}

// =============================================================================
// Memory 드라이버: Helm의 pkg/storage/driver/memory.go
// sync.RWMutex로 동시성 보호, namespace→name→records 3레벨 맵 구조.
// =============================================================================

// record는 릴리스 + 레이블을 묶는 내부 구조체
// 실제 Helm: driver.record{key, lbs, rls}
type record struct {
	key    string
	labels map[string]string
	rls    *Release
}

// Memory는 인메모리 스토리지 드라이버이다.
// 실제 Helm: driver.Memory{RWMutex, namespace, cache}
type Memory struct {
	mu        sync.RWMutex
	namespace string
	cache     map[string]map[string][]*record // namespace → name → records
}

func NewMemory() *Memory {
	return &Memory{
		namespace: "default",
		cache:     make(map[string]map[string][]*record),
	}
}

func (m *Memory) Name() string { return "Memory" }

func (m *Memory) SetNamespace(ns string) { m.namespace = ns }

func (m *Memory) Create(key string, rls *Release) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns := rls.Namespace
	if ns == "" {
		ns = "default"
	}

	if _, ok := m.cache[ns]; !ok {
		m.cache[ns] = make(map[string][]*record)
	}

	// 중복 검사
	for _, rec := range m.cache[ns][rls.Name] {
		if rec.key == key {
			return ErrReleaseExists
		}
	}

	rec := &record{
		key: key,
		labels: map[string]string{
			"name":   rls.Name,
			"owner":  "helm",
			"status": string(rls.Status),
		},
		rls: rls,
	}

	m.cache[ns][rls.Name] = append(m.cache[ns][rls.Name], rec)
	return nil
}

func (m *Memory) Get(key string) (*Release, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if nsCache, ok := m.cache[m.namespace]; ok {
		for _, recs := range nsCache {
			for _, rec := range recs {
				if rec.key == key {
					return rec.rls, nil
				}
			}
		}
	}
	return nil, ErrReleaseNotFound
}

func (m *Memory) Update(key string, rls *Release) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns := rls.Namespace
	if ns == "" {
		ns = "default"
	}

	if nsCache, ok := m.cache[ns]; ok {
		if recs, ok := nsCache[rls.Name]; ok {
			for i, rec := range recs {
				if rec.key == key {
					recs[i] = &record{
						key: key,
						labels: map[string]string{
							"name":   rls.Name,
							"owner":  "helm",
							"status": string(rls.Status),
						},
						rls: rls,
					}
					return nil
				}
			}
		}
	}
	return ErrReleaseNotFound
}

func (m *Memory) Delete(key string) (*Release, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if nsCache, ok := m.cache[m.namespace]; ok {
		for name, recs := range nsCache {
			for i, rec := range recs {
				if rec.key == key {
					nsCache[name] = append(recs[:i], recs[i+1:]...)
					return rec.rls, nil
				}
			}
		}
	}
	return nil, ErrReleaseNotFound
}

func (m *Memory) List(filter func(*Release) bool) ([]*Release, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*Release
	if nsCache, ok := m.cache[m.namespace]; ok {
		for _, recs := range nsCache {
			for _, rec := range recs {
				if filter(rec.rls) {
					result = append(result, rec.rls)
				}
			}
		}
	}
	return result, nil
}

func (m *Memory) Query(labels map[string]string) ([]*Release, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*Release
	if nsCache, ok := m.cache[m.namespace]; ok {
		for _, recs := range nsCache {
			for _, rec := range recs {
				if matchLabels(rec.labels, labels) {
					result = append(result, rec.rls)
				}
			}
		}
	}

	if len(result) == 0 {
		return nil, ErrReleaseNotFound
	}
	return result, nil
}

func matchLabels(stored, query map[string]string) bool {
	for k, v := range query {
		if stored[k] != v {
			return false
		}
	}
	return true
}

// =============================================================================
// FileDriver: Secrets/ConfigMaps 대신 파일 기반 드라이버 (PoC 구현)
// 실제 Helm에서는 Kubernetes Secrets 또는 ConfigMaps에 저장한다.
// 여기서는 파일시스템에 JSON으로 저장하여 영속성을 시뮬레이션.
// =============================================================================

type FileDriver struct {
	mu      sync.RWMutex
	baseDir string
}

func NewFileDriver(baseDir string) (*FileDriver, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, err
	}
	return &FileDriver{baseDir: baseDir}, nil
}

func (f *FileDriver) Name() string { return "File" }

func (f *FileDriver) keyToPath(key string) string {
	// 안전한 파일명으로 변환
	safe := strings.ReplaceAll(key, "/", "_")
	return filepath.Join(f.baseDir, safe+".json")
}

func (f *FileDriver) Create(key string, rls *Release) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := f.keyToPath(key)
	if _, err := os.Stat(path); err == nil {
		return ErrReleaseExists
	}

	return f.writeRelease(path, rls)
}

func (f *FileDriver) Get(key string) (*Release, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	path := f.keyToPath(key)
	return f.readRelease(path)
}

func (f *FileDriver) Update(key string, rls *Release) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := f.keyToPath(key)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ErrReleaseNotFound
	}

	return f.writeRelease(path, rls)
}

func (f *FileDriver) Delete(key string) (*Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := f.keyToPath(key)
	rls, err := f.readRelease(path)
	if err != nil {
		return nil, ErrReleaseNotFound
	}

	if err := os.Remove(path); err != nil {
		return nil, err
	}
	return rls, nil
}

func (f *FileDriver) List(filter func(*Release) bool) ([]*Release, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	var result []*Release
	entries, err := os.ReadDir(f.baseDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rls, err := f.readRelease(filepath.Join(f.baseDir, entry.Name()))
		if err != nil {
			continue
		}
		if filter(rls) {
			result = append(result, rls)
		}
	}
	return result, nil
}

func (f *FileDriver) Query(labels map[string]string) ([]*Release, error) {
	// 파일 드라이버에서는 모든 릴리스를 읽고 필터링
	return f.List(func(rls *Release) bool {
		for k, v := range labels {
			switch k {
			case "name":
				if rls.Name != v {
					return false
				}
			case "status":
				if string(rls.Status) != v {
					return false
				}
			}
		}
		return true
	})
}

func (f *FileDriver) writeRelease(path string, rls *Release) error {
	data, err := json.MarshalIndent(rls, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (f *FileDriver) readRelease(path string) (*Release, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrReleaseNotFound
	}
	var rls Release
	if err := json.Unmarshal(data, &rls); err != nil {
		return nil, err
	}
	return &rls, nil
}

// =============================================================================
// Storage: Helm의 pkg/storage/storage.go
// Driver를 감싸는 상위 레이어. 키 생성, MaxHistory, 이력 관리 담당.
// =============================================================================

const HelmStorageType = "sh.helm.release.v1"

// Storage는 릴리스 저장소 래퍼이다.
// 실제 Helm: storage.Storage{Driver, MaxHistory}
type Storage struct {
	Driver     Driver
	MaxHistory int
}

func NewStorage(d Driver) *Storage {
	if d == nil {
		d = NewMemory()
	}
	return &Storage{Driver: d}
}

// makeKey는 릴리스 키를 생성한다.
// 형식: sh.helm.release.v1.<name>.v<version>
// 실제 Helm: storage.makeKey(rlsname, version)
func makeKey(name string, version int) string {
	return fmt.Sprintf("%s.%s.v%d", HelmStorageType, name, version)
}

func (s *Storage) Create(rls *Release) error {
	key := makeKey(rls.Name, rls.Version)
	fmt.Printf("    [Storage] Create: key=%s\n", key)

	// MaxHistory 적용: 오래된 릴리스 정리
	if s.MaxHistory > 0 {
		s.removeLeastRecent(rls.Name, s.MaxHistory-1)
	}

	return s.Driver.Create(key, rls)
}

func (s *Storage) Get(name string, version int) (*Release, error) {
	key := makeKey(name, version)
	return s.Driver.Get(key)
}

func (s *Storage) Update(rls *Release) error {
	key := makeKey(rls.Name, rls.Version)
	return s.Driver.Update(key, rls)
}

func (s *Storage) Delete(name string, version int) (*Release, error) {
	key := makeKey(name, version)
	return s.Driver.Delete(key)
}

func (s *Storage) History(name string) ([]*Release, error) {
	return s.Driver.Query(map[string]string{"name": name})
}

func (s *Storage) Last(name string) (*Release, error) {
	history, err := s.History(name)
	if err != nil {
		return nil, err
	}
	if len(history) == 0 {
		return nil, fmt.Errorf("릴리스 %q 이력이 없습니다", name)
	}

	// 리비전 역순 정렬 → 최신 반환
	sort.Slice(history, func(i, j int) bool {
		return history[i].Version > history[j].Version
	})
	return history[0], nil
}

func (s *Storage) Deployed(name string) (*Release, error) {
	releases, err := s.Driver.Query(map[string]string{"name": name, "status": "deployed"})
	if err != nil {
		return nil, err
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("배포된 릴리스 %q 없음", name)
	}
	sort.Slice(releases, func(i, j int) bool {
		return releases[i].Version > releases[j].Version
	})
	return releases[0], nil
}

func (s *Storage) ListReleases() ([]*Release, error) {
	return s.Driver.List(func(_ *Release) bool { return true })
}

// removeLeastRecent는 가장 오래된 릴리스를 삭제하여 최대 이력을 유지한다.
// 실제 Helm: storage.removeLeastRecent(name, maximum)
func (s *Storage) removeLeastRecent(name string, maximum int) {
	history, err := s.History(name)
	if err != nil || len(history) <= maximum {
		return
	}

	// 오래된 순 정렬
	sort.Slice(history, func(i, j int) bool {
		return history[i].Version < history[j].Version
	})

	toDelete := len(history) - maximum
	for i := 0; i < toDelete; i++ {
		key := makeKey(name, history[i].Version)
		s.Driver.Delete(key)
		fmt.Printf("    [Storage] MaxHistory 정리: %s 삭제\n", key)
	}
}

// =============================================================================
// main: 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Helm v4 스토리지 드라이버 PoC ===")
	fmt.Println()

	// 1) Memory 드라이버 CRUD
	demoMemoryDriver()

	// 2) File 드라이버 CRUD
	demoFileDriver()

	// 3) Storage 래퍼 + 키 생성
	demoStorage()

	// 4) MaxHistory 기능
	demoMaxHistory()

	// 5) 레이블 기반 쿼리
	demoLabelQuery()
}

func demoMemoryDriver() {
	fmt.Println("--- 1. Memory 드라이버 CRUD ---")

	mem := NewMemory()
	fmt.Printf("  드라이버: %s\n", mem.Name())

	// Create
	rls := &Release{Name: "myapp", Version: 1, Namespace: "default", Status: StatusDeployed, Chart: "myapp-1.0.0", CreatedAt: time.Now()}
	err := mem.Create("sh.helm.release.v1.myapp.v1", rls)
	fmt.Printf("  Create: %v\n", err)

	// 중복 Create → 에러
	err = mem.Create("sh.helm.release.v1.myapp.v1", rls)
	fmt.Printf("  Create (중복): %v\n", err)

	// Get
	got, err := mem.Get("sh.helm.release.v1.myapp.v1")
	fmt.Printf("  Get: name=%s, version=%d, err=%v\n", got.Name, got.Version, err)

	// Update
	rls.Status = StatusSuperseded
	err = mem.Update("sh.helm.release.v1.myapp.v1", rls)
	fmt.Printf("  Update: %v\n", err)

	got, _ = mem.Get("sh.helm.release.v1.myapp.v1")
	fmt.Printf("  Get 후 상태: %s\n", got.Status)

	// Delete
	deleted, err := mem.Delete("sh.helm.release.v1.myapp.v1")
	fmt.Printf("  Delete: name=%s, err=%v\n", deleted.Name, err)

	// Get (삭제 후)
	_, err = mem.Get("sh.helm.release.v1.myapp.v1")
	fmt.Printf("  Get (삭제 후): %v\n", err)

	fmt.Println()
}

func demoFileDriver() {
	fmt.Println("--- 2. File 드라이버 CRUD ---")

	tmpDir, err := os.MkdirTemp("", "helm-storage-poc")
	if err != nil {
		fmt.Printf("  임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	fd, err := NewFileDriver(tmpDir)
	if err != nil {
		fmt.Printf("  드라이버 생성 실패: %v\n", err)
		return
	}
	fmt.Printf("  드라이버: %s (경로: %s)\n", fd.Name(), tmpDir)

	// Create
	rls := &Release{Name: "webapp", Version: 1, Namespace: "prod", Status: StatusDeployed, Chart: "webapp-2.0.0", CreatedAt: time.Now()}
	err = fd.Create("sh.helm.release.v1.webapp.v1", rls)
	fmt.Printf("  Create: %v\n", err)

	// Get
	got, err := fd.Get("sh.helm.release.v1.webapp.v1")
	fmt.Printf("  Get: name=%s, chart=%s, err=%v\n", got.Name, got.Chart, err)

	// 파일 확인
	entries, _ := os.ReadDir(tmpDir)
	fmt.Printf("  저장된 파일: ")
	for _, e := range entries {
		fmt.Printf("%s ", e.Name())
	}
	fmt.Println()

	// Update
	rls.Status = StatusSuperseded
	err = fd.Update("sh.helm.release.v1.webapp.v1", rls)
	fmt.Printf("  Update: %v\n", err)

	// Delete
	_, err = fd.Delete("sh.helm.release.v1.webapp.v1")
	fmt.Printf("  Delete: %v\n", err)

	fmt.Println()
}

func demoStorage() {
	fmt.Println("--- 3. Storage 래퍼 + 키 생성 ---")

	store := NewStorage(NewMemory())

	// 키 형식 시연
	fmt.Printf("  키 형식: %s\n", makeKey("myapp", 1))
	fmt.Printf("  키 형식: %s\n", makeKey("myapp", 5))

	// Create
	for i := 1; i <= 3; i++ {
		rls := &Release{
			Name: "myapp", Version: i, Namespace: "default",
			Status: StatusDeployed, Chart: fmt.Sprintf("myapp-%d.0.0", i),
			CreatedAt: time.Now(),
		}
		if i < 3 {
			rls.Status = StatusSuperseded
		}
		store.Create(rls)
	}

	// History
	history, _ := store.History("myapp")
	fmt.Printf("  History (myapp): %d개 리비전\n", len(history))
	for _, r := range history {
		fmt.Printf("    v%d: %s (%s)\n", r.Version, r.Status, r.Chart)
	}

	// Last
	last, _ := store.Last("myapp")
	fmt.Printf("  Last: v%d (%s)\n", last.Version, last.Status)

	// Deployed
	deployed, _ := store.Deployed("myapp")
	fmt.Printf("  Deployed: v%d (%s)\n", deployed.Version, deployed.Status)

	fmt.Println()
}

func demoMaxHistory() {
	fmt.Println("--- 4. MaxHistory 기능 ---")

	mem := NewMemory()
	store := NewStorage(mem)
	store.MaxHistory = 3

	fmt.Printf("  MaxHistory = %d\n", store.MaxHistory)

	// 5개 리비전 생성
	for i := 1; i <= 5; i++ {
		rls := &Release{
			Name: "webapp", Version: i, Namespace: "default",
			Status: StatusSuperseded, Chart: fmt.Sprintf("webapp-%d.0.0", i),
			CreatedAt: time.Now(),
		}
		if i == 5 {
			rls.Status = StatusDeployed
		}
		store.Create(rls)
	}

	// 이력 확인 (MaxHistory=3이므로 최신 3개만 남음)
	history, _ := store.History("webapp")
	fmt.Printf("  남은 이력: %d개\n", len(history))
	for _, r := range history {
		fmt.Printf("    v%d: %s\n", r.Version, r.Status)
	}

	fmt.Println()
}

func demoLabelQuery() {
	fmt.Println("--- 5. 레이블 기반 쿼리 ---")

	mem := NewMemory()
	store := NewStorage(mem)

	// 여러 릴리스 생성
	releases := []struct {
		name, chart string
		status      Status
	}{
		{"app-a", "chart-a-1.0.0", StatusDeployed},
		{"app-b", "chart-b-2.0.0", StatusDeployed},
		{"app-c", "chart-c-1.0.0", StatusFailed},
	}

	for _, r := range releases {
		store.Create(&Release{
			Name: r.name, Version: 1, Namespace: "default",
			Status: r.status, Chart: r.chart, CreatedAt: time.Now(),
		})
	}

	// 전체 조회
	all, _ := store.ListReleases()
	fmt.Printf("  전체 릴리스: %d개\n", len(all))

	// deployed 필터
	deployed, _ := mem.Query(map[string]string{"status": "deployed"})
	fmt.Printf("  deployed 릴리스: %d개\n", len(deployed))
	for _, r := range deployed {
		fmt.Printf("    %s (%s)\n", r.Name, r.Chart)
	}

	// 이름으로 쿼리
	found, _ := mem.Query(map[string]string{"name": "app-a"})
	fmt.Printf("  name=app-a: %d개\n", len(found))

	fmt.Println()
	fmt.Println("=== 스토리지 드라이버 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Driver 인터페이스: Create/Get/Update/Delete + List/Query (인터페이스 합성)")
	fmt.Println("  2. Memory 드라이버: sync.RWMutex 동시성 보호, namespace→name→records 맵")
	fmt.Println("  3. File 드라이버: JSON 직렬화로 파일시스템에 영속 저장 (Secrets/ConfigMaps 대체)")
	fmt.Println("  4. Storage 래퍼: 키 생성(sh.helm.release.v1.<name>.v<ver>), MaxHistory, History/Last/Deployed")
	fmt.Println("  5. 레이블 쿼리: name/owner/status 레이블로 릴리스 검색")
}
