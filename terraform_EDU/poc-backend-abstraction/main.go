package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Terraform Backend 추상화 시뮬레이션
// =============================================================================
// Terraform의 백엔드 시스템은 상태(State) 저장소를 추상화합니다.
// 실제 코드: internal/backend/backend.go
//
// 백엔드는 다음 책임을 가집니다:
// 1. State 읽기/쓰기 (StateMgr 인터페이스)
// 2. 워크스페이스 관리 (생성, 목록, 삭제)
// 3. State 잠금 (Lock/Unlock)
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// State 정의
// ─────────────────────────────────────────────────────────────────────────────

// State는 Terraform 상태를 나타냅니다.
// 실제: internal/states/state.go
type State struct {
	Version   int                    `json:"version"`
	Serial    int                    `json:"serial"`
	Lineage   string                 `json:"lineage"`
	Resources map[string]*Resource   `json:"resources"`
	Outputs   map[string]interface{} `json:"outputs"`
}

// Resource는 상태에 저장된 리소스입니다.
type Resource struct {
	Type       string                 `json:"type"`
	Name       string                 `json:"name"`
	Provider   string                 `json:"provider"`
	Instances  []Instance             `json:"instances"`
	Attributes map[string]interface{} `json:"attributes"`
}

// Instance는 리소스의 인스턴스입니다.
type Instance struct {
	IndexKey   interface{}            `json:"index_key,omitempty"`
	Attributes map[string]interface{} `json:"attributes"`
}

// NewState는 새 빈 State를 생성합니다.
func NewState() *State {
	return &State{
		Version:   4,
		Serial:    0,
		Lineage:   fmt.Sprintf("lineage-%d", time.Now().UnixNano()),
		Resources: make(map[string]*Resource),
		Outputs:   make(map[string]interface{}),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Lock 정보
// ─────────────────────────────────────────────────────────────────────────────

// LockInfo는 잠금 정보를 나타냅니다.
// 실제: internal/states/statemgr/lock.go
type LockInfo struct {
	ID        string    `json:"ID"`
	Operation string    `json:"Operation"`
	Info      string    `json:"Info"`
	Who       string    `json:"Who"`
	Version   string    `json:"Version"`
	Created   time.Time `json:"Created"`
}

// LockError는 잠금 충돌 에러입니다.
type LockError struct {
	Info *LockInfo
	Err  error
}

func (e *LockError) Error() string {
	return fmt.Sprintf("잠금 충돌: 이미 %s에 의해 잠겨있습니다 (ID: %s, 작업: %s)",
		e.Info.Who, e.Info.ID, e.Info.Operation)
}

// ─────────────────────────────────────────────────────────────────────────────
// StateMgr 인터페이스
// ─────────────────────────────────────────────────────────────────────────────

// StateMgr는 상태 관리 인터페이스입니다.
// 실제: internal/states/statemgr/statemgr.go
type StateMgr interface {
	// State는 현재 상태를 반환합니다.
	State() *State
	// WriteState는 상태를 기록합니다 (아직 영속화하지 않음).
	WriteState(state *State) error
	// PersistState는 상태를 영속 저장소에 기록합니다.
	PersistState() error
	// RefreshState는 저장소에서 상태를 다시 읽습니다.
	RefreshState() error
}

// Locker는 상태 잠금을 지원하는 인터페이스입니다.
type Locker interface {
	Lock(info *LockInfo) (string, error)
	Unlock(id string) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Backend 인터페이스
// ─────────────────────────────────────────────────────────────────────────────

// Backend는 Terraform 백엔드 인터페이스입니다.
// 실제: internal/backend/backend.go
type Backend interface {
	// StateMgr는 지정된 워크스페이스의 StateMgr를 반환합니다.
	StateMgr(workspace string) (StateMgr, error)
	// Workspaces는 사용 가능한 워크스페이스 목록을 반환합니다.
	Workspaces() ([]string, error)
	// DeleteWorkspace는 워크스페이스를 삭제합니다.
	DeleteWorkspace(name string, force bool) error
}

// ─────────────────────────────────────────────────────────────────────────────
// InMemoryBackend: 테스트용 인-메모리 백엔드
// ─────────────────────────────────────────────────────────────────────────────

// InMemoryStateMgr는 인-메모리 상태 관리자입니다.
type InMemoryStateMgr struct {
	currentState *State
	savedState   *State
	lockInfo     *LockInfo
	mu           sync.Mutex
}

func (m *InMemoryStateMgr) State() *State {
	return m.currentState
}

func (m *InMemoryStateMgr) WriteState(state *State) error {
	m.currentState = state
	return nil
}

func (m *InMemoryStateMgr) PersistState() error {
	if m.currentState != nil {
		data, _ := json.Marshal(m.currentState)
		var copy State
		json.Unmarshal(data, &copy)
		m.savedState = &copy
	}
	return nil
}

func (m *InMemoryStateMgr) RefreshState() error {
	if m.savedState != nil {
		data, _ := json.Marshal(m.savedState)
		var copy State
		json.Unmarshal(data, &copy)
		m.currentState = &copy
	}
	return nil
}

func (m *InMemoryStateMgr) Lock(info *LockInfo) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lockInfo != nil {
		return "", &LockError{Info: m.lockInfo, Err: fmt.Errorf("이미 잠겨있음")}
	}

	info.ID = fmt.Sprintf("inmem-lock-%d", time.Now().UnixNano())
	info.Created = time.Now()
	m.lockInfo = info
	return info.ID, nil
}

func (m *InMemoryStateMgr) Unlock(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lockInfo == nil {
		return fmt.Errorf("잠금이 없습니다")
	}
	if m.lockInfo.ID != id {
		return fmt.Errorf("잠금 ID 불일치: 예상 %s, 실제 %s", m.lockInfo.ID, id)
	}

	m.lockInfo = nil
	return nil
}

// InMemoryBackend는 테스트용 인-메모리 백엔드입니다.
type InMemoryBackend struct {
	workspaces map[string]*InMemoryStateMgr
	mu         sync.Mutex
}

func NewInMemoryBackend() *InMemoryBackend {
	b := &InMemoryBackend{
		workspaces: make(map[string]*InMemoryStateMgr),
	}
	// default 워크스페이스는 항상 존재
	b.workspaces["default"] = &InMemoryStateMgr{}
	return b
}

func (b *InMemoryBackend) StateMgr(workspace string) (StateMgr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	mgr, exists := b.workspaces[workspace]
	if !exists {
		mgr = &InMemoryStateMgr{}
		b.workspaces[workspace] = mgr
	}
	return mgr, nil
}

func (b *InMemoryBackend) Workspaces() ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var names []string
	for name := range b.workspaces {
		names = append(names, name)
	}
	return names, nil
}

func (b *InMemoryBackend) DeleteWorkspace(name string, force bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if name == "default" {
		return fmt.Errorf("default 워크스페이스는 삭제할 수 없습니다")
	}

	mgr, exists := b.workspaces[name]
	if !exists {
		return fmt.Errorf("워크스페이스 '%s'이(가) 존재하지 않습니다", name)
	}

	if !force && mgr.currentState != nil && len(mgr.currentState.Resources) > 0 {
		return fmt.Errorf("워크스페이스 '%s'에 리소스가 있습니다. -force 플래그를 사용하세요", name)
	}

	delete(b.workspaces, name)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// LocalBackend: 파일 기반 로컬 백엔드
// ─────────────────────────────────────────────────────────────────────────────

// LocalStateMgr는 파일 기반 상태 관리자입니다.
type LocalStateMgr struct {
	stateFilePath string
	lockFilePath  string
	currentState  *State
	lockID        string
	mu            sync.Mutex
}

func NewLocalStateMgr(path string) *LocalStateMgr {
	return &LocalStateMgr{
		stateFilePath: path,
		lockFilePath:  path + ".lock",
	}
}

func (m *LocalStateMgr) State() *State {
	return m.currentState
}

func (m *LocalStateMgr) WriteState(state *State) error {
	state.Serial++
	m.currentState = state
	return nil
}

func (m *LocalStateMgr) PersistState() error {
	if m.currentState == nil {
		return nil
	}

	data, err := json.MarshalIndent(m.currentState, "", "  ")
	if err != nil {
		return fmt.Errorf("상태 직렬화 실패: %w", err)
	}

	dir := filepath.Dir(m.stateFilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("디렉토리 생성 실패: %w", err)
	}

	return os.WriteFile(m.stateFilePath, data, 0644)
}

func (m *LocalStateMgr) RefreshState() error {
	data, err := os.ReadFile(m.stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			m.currentState = nil
			return nil
		}
		return fmt.Errorf("상태 파일 읽기 실패: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("상태 역직렬화 실패: %w", err)
	}

	m.currentState = &state
	return nil
}

func (m *LocalStateMgr) Lock(info *LockInfo) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 잠금 파일이 이미 존재하는지 확인
	if data, err := os.ReadFile(m.lockFilePath); err == nil {
		var existing LockInfo
		if json.Unmarshal(data, &existing) == nil {
			return "", &LockError{Info: &existing, Err: fmt.Errorf("잠금 파일 존재")}
		}
	}

	info.ID = fmt.Sprintf("local-lock-%d", time.Now().UnixNano())
	info.Created = time.Now()

	data, _ := json.MarshalIndent(info, "", "  ")
	dir := filepath.Dir(m.lockFilePath)
	os.MkdirAll(dir, 0755)

	if err := os.WriteFile(m.lockFilePath, data, 0644); err != nil {
		return "", fmt.Errorf("잠금 파일 생성 실패: %w", err)
	}

	m.lockID = info.ID
	return info.ID, nil
}

func (m *LocalStateMgr) Unlock(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lockID != id {
		return fmt.Errorf("잠금 ID 불일치")
	}

	if err := os.Remove(m.lockFilePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("잠금 파일 삭제 실패: %w", err)
	}

	m.lockID = ""
	return nil
}

// LocalBackend는 로컬 파일 시스템 기반 백엔드입니다.
type LocalBackend struct {
	baseDir    string
	workspaces map[string]*LocalStateMgr
	mu         sync.Mutex
}

func NewLocalBackend(baseDir string) *LocalBackend {
	b := &LocalBackend{
		baseDir:    baseDir,
		workspaces: make(map[string]*LocalStateMgr),
	}
	// default 워크스페이스 생성
	statePath := filepath.Join(baseDir, "terraform.tfstate")
	b.workspaces["default"] = NewLocalStateMgr(statePath)
	return b
}

func (b *LocalBackend) StateMgr(workspace string) (StateMgr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	mgr, exists := b.workspaces[workspace]
	if !exists {
		// 새 워크스페이스 생성
		wsDir := filepath.Join(b.baseDir, "terraform.tfstate.d", workspace)
		statePath := filepath.Join(wsDir, "terraform.tfstate")
		mgr = NewLocalStateMgr(statePath)
		b.workspaces[workspace] = mgr
	}
	return mgr, nil
}

func (b *LocalBackend) Workspaces() ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var names []string
	for name := range b.workspaces {
		names = append(names, name)
	}
	return names, nil
}

func (b *LocalBackend) DeleteWorkspace(name string, force bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if name == "default" {
		return fmt.Errorf("default 워크스페이스는 삭제할 수 없습니다")
	}

	_, exists := b.workspaces[name]
	if !exists {
		return fmt.Errorf("워크스페이스 '%s'이(가) 존재하지 않습니다", name)
	}

	// 워크스페이스 디렉토리 삭제
	wsDir := filepath.Join(b.baseDir, "terraform.tfstate.d", name)
	os.RemoveAll(wsDir)

	delete(b.workspaces, name)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// S3LikeBackend: S3 유사 백엔드 시뮬레이션 (디렉토리 기반)
// ─────────────────────────────────────────────────────────────────────────────

// S3LikeBackend는 S3 백엔드를 디렉토리 기반으로 시뮬레이션합니다.
// 실제 S3 백엔드: internal/backend/remote-state/s3/backend.go
type S3LikeBackend struct {
	bucket     string // 시뮬레이션 - 베이스 디렉토리
	keyPrefix  string
	workspaces map[string]*S3LikeStateMgr
	mu         sync.Mutex
}

type S3LikeStateMgr struct {
	objectPath   string
	lockPath     string
	currentState *State
	lockInfo     *LockInfo
	mu           sync.Mutex
}

func NewS3LikeBackend(bucket, keyPrefix string) *S3LikeBackend {
	b := &S3LikeBackend{
		bucket:     bucket,
		keyPrefix:  keyPrefix,
		workspaces: make(map[string]*S3LikeStateMgr),
	}

	// default 워크스페이스
	objPath := filepath.Join(bucket, keyPrefix, "default", "terraform.tfstate")
	lockPath := filepath.Join(bucket, keyPrefix, "default", ".terraform.lock")
	b.workspaces["default"] = &S3LikeStateMgr{
		objectPath: objPath,
		lockPath:   lockPath,
	}
	return b
}

func (m *S3LikeStateMgr) State() *State {
	return m.currentState
}

func (m *S3LikeStateMgr) WriteState(state *State) error {
	state.Serial++
	m.currentState = state
	return nil
}

func (m *S3LikeStateMgr) PersistState() error {
	if m.currentState == nil {
		return nil
	}

	data, err := json.MarshalIndent(m.currentState, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(m.objectPath)
	os.MkdirAll(dir, 0755)
	return os.WriteFile(m.objectPath, data, 0644)
}

func (m *S3LikeStateMgr) RefreshState() error {
	data, err := os.ReadFile(m.objectPath)
	if err != nil {
		if os.IsNotExist(err) {
			m.currentState = nil
			return nil
		}
		return err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	m.currentState = &state
	return nil
}

func (m *S3LikeStateMgr) Lock(info *LockInfo) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lockInfo != nil {
		return "", &LockError{Info: m.lockInfo, Err: fmt.Errorf("DynamoDB 잠금 존재")}
	}

	info.ID = fmt.Sprintf("s3-lock-%d", time.Now().UnixNano())
	info.Created = time.Now()
	m.lockInfo = info

	// 잠금 정보를 파일로 기록 (DynamoDB 시뮬레이션)
	data, _ := json.MarshalIndent(info, "", "  ")
	dir := filepath.Dir(m.lockPath)
	os.MkdirAll(dir, 0755)
	os.WriteFile(m.lockPath, data, 0644)

	return info.ID, nil
}

func (m *S3LikeStateMgr) Unlock(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lockInfo == nil || m.lockInfo.ID != id {
		return fmt.Errorf("잠금 ID 불일치")
	}

	m.lockInfo = nil
	os.Remove(m.lockPath)
	return nil
}

func (b *S3LikeBackend) StateMgr(workspace string) (StateMgr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	mgr, exists := b.workspaces[workspace]
	if !exists {
		objPath := filepath.Join(b.bucket, b.keyPrefix, workspace, "terraform.tfstate")
		lockPath := filepath.Join(b.bucket, b.keyPrefix, workspace, ".terraform.lock")
		mgr = &S3LikeStateMgr{
			objectPath: objPath,
			lockPath:   lockPath,
		}
		b.workspaces[workspace] = mgr
	}
	return mgr, nil
}

func (b *S3LikeBackend) Workspaces() ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var names []string
	for name := range b.workspaces {
		names = append(names, name)
	}
	return names, nil
}

func (b *S3LikeBackend) DeleteWorkspace(name string, force bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if name == "default" {
		return fmt.Errorf("default 워크스페이스는 삭제할 수 없습니다")
	}

	_, exists := b.workspaces[name]
	if !exists {
		return fmt.Errorf("워크스페이스 '%s'이(가) 존재하지 않습니다", name)
	}

	// S3 객체 삭제 시뮬레이션
	wsDir := filepath.Join(b.bucket, b.keyPrefix, name)
	os.RemoveAll(wsDir)

	delete(b.workspaces, name)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 헬퍼 함수
// ─────────────────────────────────────────────────────────────────────────────

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
}

func printSubSection(title string) {
	fmt.Printf("\n--- %s ---\n\n", title)
}

// demoBackend는 주어진 백엔드로 표준 데모를 수행합니다.
func demoBackend(name string, backend Backend) {
	printSeparator(fmt.Sprintf("%s 데모", name))

	// 1. 워크스페이스 목록 확인
	printSubSection("1. 초기 워크스페이스 목록")
	workspaces, _ := backend.Workspaces()
	for _, ws := range workspaces {
		fmt.Printf("  - %s\n", ws)
	}

	// 2. default 워크스페이스에 상태 기록
	printSubSection("2. default 워크스페이스에 상태 기록")
	mgr, err := backend.StateMgr("default")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}

	state := NewState()
	state.Resources["aws_instance.web"] = &Resource{
		Type:     "aws_instance",
		Name:     "web",
		Provider: "registry.terraform.io/hashicorp/aws",
		Attributes: map[string]interface{}{
			"ami":           "ami-12345",
			"instance_type": "t2.micro",
		},
	}
	state.Outputs["instance_ip"] = "10.0.1.5"

	mgr.WriteState(state)
	mgr.PersistState()
	fmt.Println("  상태 기록 완료 (리소스 1개, 출력 1개)")

	// 3. State 잠금 테스트
	printSubSection("3. State 잠금 테스트")
	if locker, ok := mgr.(Locker); ok {
		lockID, err := locker.Lock(&LockInfo{
			Operation: "OperationTypeApply",
			Who:       "user@terraform-poc",
			Info:      "terraform apply",
		})
		if err != nil {
			fmt.Printf("  잠금 실패: %v\n", err)
		} else {
			fmt.Printf("  잠금 성공: ID=%s\n", lockID)

			// 이중 잠금 시도
			_, err = locker.Lock(&LockInfo{
				Operation: "OperationTypePlan",
				Who:       "other-user@ci",
				Info:      "terraform plan",
			})
			if err != nil {
				fmt.Printf("  이중 잠금 시도 → %v\n", err)
			}

			// 잠금 해제
			err = locker.Unlock(lockID)
			if err != nil {
				fmt.Printf("  잠금 해제 실패: %v\n", err)
			} else {
				fmt.Println("  잠금 해제 성공")
			}
		}
	} else {
		fmt.Println("  이 백엔드는 잠금을 지원하지 않습니다.")
	}

	// 4. 새 워크스페이스 생성
	printSubSection("4. 'staging' 워크스페이스 생성")
	stagingMgr, _ := backend.StateMgr("staging")
	stagingState := NewState()
	stagingState.Resources["aws_instance.staging"] = &Resource{
		Type:     "aws_instance",
		Name:     "staging",
		Provider: "registry.terraform.io/hashicorp/aws",
		Attributes: map[string]interface{}{
			"ami":           "ami-67890",
			"instance_type": "t2.small",
		},
	}
	stagingMgr.WriteState(stagingState)
	stagingMgr.PersistState()
	fmt.Println("  staging 워크스페이스에 상태 기록 완료")

	// 5. production 워크스페이스 생성
	printSubSection("5. 'production' 워크스페이스 생성")
	prodMgr, _ := backend.StateMgr("production")
	prodState := NewState()
	prodState.Resources["aws_instance.prod"] = &Resource{
		Type:     "aws_instance",
		Name:     "prod",
		Provider: "registry.terraform.io/hashicorp/aws",
		Attributes: map[string]interface{}{
			"ami":           "ami-prod-1",
			"instance_type": "t2.large",
		},
	}
	prodState.Resources["aws_db_instance.prod"] = &Resource{
		Type:     "aws_db_instance",
		Name:     "prod",
		Provider: "registry.terraform.io/hashicorp/aws",
		Attributes: map[string]interface{}{
			"engine":         "postgres",
			"instance_class": "db.r5.large",
		},
	}
	prodMgr.WriteState(prodState)
	prodMgr.PersistState()
	fmt.Println("  production 워크스페이스에 상태 기록 완료 (리소스 2개)")

	// 6. 전체 워크스페이스 목록
	printSubSection("6. 전체 워크스페이스 목록")
	workspaces, _ = backend.Workspaces()
	for _, ws := range workspaces {
		wsMgr, _ := backend.StateMgr(ws)
		wsMgr.RefreshState()
		s := wsMgr.State()
		resourceCount := 0
		if s != nil {
			resourceCount = len(s.Resources)
		}
		fmt.Printf("  - %-15s (리소스 %d개)\n", ws, resourceCount)
	}

	// 7. 상태 새로고침 검증
	printSubSection("7. 상태 새로고침 (RefreshState) 검증")
	freshMgr, _ := backend.StateMgr("default")
	freshMgr.RefreshState()
	refreshedState := freshMgr.State()
	if refreshedState != nil {
		fmt.Printf("  새로고침된 상태: 버전=%d, 시리얼=%d, 리소스=%d개\n",
			refreshedState.Version, refreshedState.Serial, len(refreshedState.Resources))
		for addr, res := range refreshedState.Resources {
			fmt.Printf("    - %s (type=%s, provider=%s)\n", addr, res.Type, res.Provider)
		}
	}

	// 8. 워크스페이스 삭제
	printSubSection("8. 워크스페이스 삭제 테스트")

	// force 없이 삭제 시도 (리소스가 있으면 실패)
	err = backend.DeleteWorkspace("staging", false)
	if err != nil {
		fmt.Printf("  staging 삭제 (force=false): %v\n", err)
	}

	// force로 삭제
	err = backend.DeleteWorkspace("staging", true)
	if err != nil {
		fmt.Printf("  staging 삭제 (force=true): 실패 - %v\n", err)
	} else {
		fmt.Println("  staging 삭제 (force=true): 성공")
	}

	// default 삭제 시도
	err = backend.DeleteWorkspace("default", true)
	if err != nil {
		fmt.Printf("  default 삭제 시도: %v\n", err)
	}

	// 최종 워크스페이스 목록
	printSubSection("9. 최종 워크스페이스 목록")
	workspaces, _ = backend.Workspaces()
	for _, ws := range workspaces {
		fmt.Printf("  - %s\n", ws)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Terraform Backend 추상화 시뮬레이션                        ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  백엔드 종류:                                                        ║")
	fmt.Println("║    1. InMemoryBackend - 테스트용 인-메모리 저장소                    ║")
	fmt.Println("║    2. LocalBackend    - 파일 기반 로컬 저장소                        ║")
	fmt.Println("║    3. S3LikeBackend   - S3 유사 원격 저장소 시뮬레이션               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// ─── InMemoryBackend 데모 ───
	demoBackend("InMemoryBackend", NewInMemoryBackend())

	// ─── LocalBackend 데모 ───
	tmpDir, _ := os.MkdirTemp("", "terraform-local-backend-*")
	defer os.RemoveAll(tmpDir)
	demoBackend("LocalBackend", NewLocalBackend(tmpDir))

	// ─── S3LikeBackend 데모 ───
	s3Dir, _ := os.MkdirTemp("", "terraform-s3-backend-*")
	defer os.RemoveAll(s3Dir)
	demoBackend("S3LikeBackend", NewS3LikeBackend(s3Dir, "state"))

	// ─── 백엔드 교체 데모 ───
	printSeparator("백엔드 교체 (Migration) 시뮬레이션")
	fmt.Println("Terraform에서 백엔드를 교체하면 상태가 마이그레이션됩니다.")
	fmt.Println("예: local → s3 전환 시 기존 상태를 새 백엔드로 복사")
	fmt.Println()

	// 소스 백엔드에서 상태 읽기
	srcBackend := NewInMemoryBackend()
	srcMgr, _ := srcBackend.StateMgr("default")
	srcState := NewState()
	srcState.Resources["aws_vpc.main"] = &Resource{
		Type: "aws_vpc", Name: "main",
		Provider:   "registry.terraform.io/hashicorp/aws",
		Attributes: map[string]interface{}{"cidr_block": "10.0.0.0/16"},
	}
	srcMgr.WriteState(srcState)
	srcMgr.PersistState()

	// 대상 백엔드로 마이그레이션
	dstDir, _ := os.MkdirTemp("", "terraform-migration-*")
	defer os.RemoveAll(dstDir)
	dstBackend := NewS3LikeBackend(dstDir, "state")

	// 마이그레이션 수행
	fmt.Println("  [1] 소스(InMemory) 백엔드에서 상태 읽기...")
	srcMgr.RefreshState()
	migrateState := srcMgr.State()
	fmt.Printf("      리소스: %d개\n", len(migrateState.Resources))

	fmt.Println("  [2] 대상(S3Like) 백엔드에 상태 쓰기...")
	dstMgr, _ := dstBackend.StateMgr("default")
	dstMgr.WriteState(migrateState)
	dstMgr.PersistState()
	fmt.Println("      마이그레이션 완료!")

	fmt.Println("  [3] 대상 백엔드에서 상태 확인...")
	dstMgr.RefreshState()
	verifyState := dstMgr.State()
	if verifyState != nil {
		fmt.Printf("      검증 성공: 리소스 %d개 확인\n", len(verifyState.Resources))
		for addr := range verifyState.Resources {
			fmt.Printf("        - %s\n", addr)
		}
	}

	printSeparator("아키텍처 요약")
	fmt.Print(`
  ┌──────────────────────────────────────────────────────┐
  │                  Backend 인터페이스                     │
  │  StateMgr(workspace) → StateMgr                      │
  │  Workspaces() → []string                              │
  │  DeleteWorkspace(name, force) → error                 │
  └──────────┬──────────────┬──────────────┬──────────────┘
             │              │              │
    ┌────────▼───────┐  ┌──▼──────────┐  ┌▼──────────────┐
    │  LocalBackend  │  │ S3Backend   │  │  RemoteBackend │
    │  (파일 시스템) │  │ (S3+DynDB)  │  │  (Terraform    │
    │                │  │             │  │   Cloud)       │
    └────────┬───────┘  └──┬──────────┘  └┬──────────────┘
             │              │              │
    ┌────────▼──────────────▼──────────────▼──────────────┐
    │                StateMgr 인터페이스                     │
    │  State()        → *State     // 현재 상태 읽기       │
    │  WriteState()   → error      // 메모리에 기록        │
    │  PersistState() → error      // 영속 저장소에 기록   │
    │  RefreshState() → error      // 저장소에서 다시 읽기 │
    └─────────────────────┬───────────────────────────────┘
                          │
    ┌─────────────────────▼───────────────────────────────┐
    │                  Locker 인터페이스                     │
    │  Lock(info) → (lockID, error)                        │
    │  Unlock(id) → error                                  │
    │                                                       │
    │  → 동시 접근 방지                                     │
    │  → Lock 파일 / DynamoDB 기반                          │
    └─────────────────────────────────────────────────────┘`)
}
