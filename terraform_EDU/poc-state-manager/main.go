package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// =============================================================================
// Terraform State 관리 시뮬레이션
// =============================================================================
//
// Terraform State는 실제 인프라와 설정 파일 사이의 매핑 정보를 저장한다.
// State가 없으면 Terraform은 어떤 리소스가 이미 존재하는지 알 수 없다.
//
// 실제 Terraform 소스:
//   - internal/states/statefile/version4.go: State 직렬화 (v4 포맷)
//   - internal/states/sync.go: SyncState (동기화된 상태 접근)
//   - internal/states/instance_object.go: 리소스 인스턴스 객체
//   - internal/statemgr/filesystem.go: 파일 기반 상태 관리
//   - internal/statemgr/locker.go: 상태 잠금 인터페이스
//
// 이 PoC에서 구현하는 핵심 개념:
//   1. State, Module, Resource, ResourceInstance 구조체
//   2. SyncState (RWMutex 기반 동시성 안전 접근)
//   3. JSON 직렬화/역직렬화
//   4. 파일 기반 잠금 메커니즘
//   5. Deposed 인스턴스 (create_before_destroy)

// =============================================================================
// 1. 상태 데이터 구조체
// =============================================================================

// StateFile은 Terraform 상태 파일의 최상위 구조체이다.
// Terraform의 statefile.File에 대응한다.
type StateFile struct {
	Version          int       `json:"version"`
	TerraformVersion string    `json:"terraform_version"`
	Serial           uint64    `json:"serial"`
	Lineage          string    `json:"lineage"`
	State            *State    `json:"state"`
}

// State는 전체 Terraform 상태를 나타낸다.
// Terraform의 states.State에 대응한다.
type State struct {
	Modules map[string]*Module `json:"modules"`
}

// Module은 하나의 모듈 인스턴스의 상태이다.
// Terraform의 states.Module에 대응한다.
type Module struct {
	Addr      string                `json:"addr"`
	Resources map[string]*Resource  `json:"resources"`
	OutputValues map[string]string  `json:"output_values,omitempty"`
}

// Resource는 하나의 리소스의 상태이다.
// Terraform의 states.Resource에 대응한다.
type Resource struct {
	Addr      string                        `json:"addr"`
	Type      string                        `json:"type"`
	Name      string                        `json:"name"`
	Provider  string                        `json:"provider"`
	Instances map[string]*ResourceInstance   `json:"instances"`
}

// ResourceInstance는 리소스의 하나의 인스턴스이다.
// Terraform의 states.ResourceInstanceObjectSrc에 대응한다.
type ResourceInstance struct {
	// Status는 인스턴스 상태이다.
	// "current": 현재 활성 인스턴스
	// "deposed": create_before_destroy로 인해 대기 중인 이전 인스턴스
	Status     string            `json:"status"`
	Attributes map[string]string `json:"attributes"`
	DeposedKey string            `json:"deposed_key,omitempty"`
}

// =============================================================================
// 2. SyncState (동시성 안전 상태 접근)
// =============================================================================

// SyncState는 RWMutex로 보호되는 동시성 안전한 상태 접근을 제공한다.
// Terraform의 states.SyncState에 대응한다.
//
// 왜 필요한가?
// - terraform apply 중 여러 리소스가 병렬로 처리됨
// - 각 리소스 처리 완료 시 상태를 업데이트해야 함
// - 동시에 읽기/쓰기가 발생하므로 잠금이 필요
type SyncState struct {
	mu    sync.RWMutex
	state *State
}

// NewSyncState는 새로운 SyncState를 생성한다.
func NewSyncState() *SyncState {
	return &SyncState{
		state: &State{
			Modules: make(map[string]*Module),
		},
	}
}

// Lock은 쓰기 잠금을 획득한다.
func (s *SyncState) Lock() {
	s.mu.Lock()
}

// Unlock은 쓰기 잠금을 해제한다.
func (s *SyncState) Unlock() {
	s.mu.Unlock()
}

// RLock은 읽기 잠금을 획득한다.
func (s *SyncState) RLock() {
	s.mu.RLock()
}

// RUnlock은 읽기 잠금을 해제한다.
func (s *SyncState) RUnlock() {
	s.mu.RUnlock()
}

// SetResourceInstance는 리소스 인스턴스를 설정한다.
func (s *SyncState) SetResourceInstance(moduleAddr, resourceAddr, instanceKey string, instance *ResourceInstance) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 모듈 확인/생성
	mod, exists := s.state.Modules[moduleAddr]
	if !exists {
		mod = &Module{
			Addr:         moduleAddr,
			Resources:    make(map[string]*Resource),
			OutputValues: make(map[string]string),
		}
		s.state.Modules[moduleAddr] = mod
	}

	// 리소스 확인/생성
	res, exists := mod.Resources[resourceAddr]
	if !exists {
		res = &Resource{
			Addr:      resourceAddr,
			Instances: make(map[string]*ResourceInstance),
		}
		mod.Resources[resourceAddr] = res
	}

	res.Instances[instanceKey] = instance
}

// GetResourceInstance는 리소스 인스턴스를 조회한다.
func (s *SyncState) GetResourceInstance(moduleAddr, resourceAddr, instanceKey string) *ResourceInstance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	mod, exists := s.state.Modules[moduleAddr]
	if !exists {
		return nil
	}
	res, exists := mod.Resources[resourceAddr]
	if !exists {
		return nil
	}
	return res.Instances[instanceKey]
}

// RemoveResourceInstance는 리소스 인스턴스를 제거한다.
func (s *SyncState) RemoveResourceInstance(moduleAddr, resourceAddr, instanceKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mod, exists := s.state.Modules[moduleAddr]
	if !exists {
		return
	}
	res, exists := mod.Resources[resourceAddr]
	if !exists {
		return
	}
	delete(res.Instances, instanceKey)
}

// DeposeResourceInstance는 현재 인스턴스를 deposed 상태로 전환한다.
// create_before_destroy 시 사용된다.
//
// Terraform의 동작:
//   1. 현재 인스턴스를 deposed로 표시
//   2. 새 인스턴스를 current로 생성
//   3. 새 인스턴스 생성 성공 후 deposed 인스턴스 삭제
func (s *SyncState) DeposeResourceInstance(moduleAddr, resourceAddr, instanceKey, deposedKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mod, exists := s.state.Modules[moduleAddr]
	if !exists {
		return
	}
	res, exists := mod.Resources[resourceAddr]
	if !exists {
		return
	}
	instance, exists := res.Instances[instanceKey]
	if !exists {
		return
	}

	// 현재 인스턴스를 deposed로 이동
	deposedInstance := &ResourceInstance{
		Status:     "deposed",
		Attributes: instance.Attributes,
		DeposedKey: deposedKey,
	}
	res.Instances[deposedKey] = deposedInstance
	delete(res.Instances, instanceKey)
}

// GetState는 전체 상태를 반환한다 (읽기 전용 복사본이어야 하지만 여기서는 참조 반환).
func (s *SyncState) GetState() *State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// =============================================================================
// 3. 상태 직렬화/역직렬화
// =============================================================================

// SerializeState는 상태를 JSON으로 직렬화한다.
func SerializeState(state *State, serial uint64) ([]byte, error) {
	sf := &StateFile{
		Version:          4,
		TerraformVersion: "1.9.0",
		Serial:           serial,
		Lineage:          "poc-demo-lineage-12345",
		State:            state,
	}
	return json.MarshalIndent(sf, "", "  ")
}

// DeserializeState는 JSON에서 상태를 역직렬화한다.
func DeserializeState(data []byte) (*StateFile, error) {
	sf := &StateFile{}
	err := json.Unmarshal(data, sf)
	return sf, err
}

// =============================================================================
// 4. 파일 기반 잠금 (State Locking)
// =============================================================================

// StateLock은 상태 잠금 정보를 나타낸다.
// Terraform의 statemgr.LockInfo에 대응한다.
type StateLock struct {
	ID        string    `json:"ID"`
	Operation string    `json:"Operation"`
	Who       string    `json:"Who"`
	Created   time.Time `json:"Created"`
	Path      string    `json:"Path"`
}

// FileLocker는 파일 기반 상태 잠금을 구현한다.
// Terraform의 statemgr.Filesystem에서 잠금 부분에 대응한다.
//
// 실제 Terraform은 원격 백엔드(S3, GCS, Consul 등)에서도
// 잠금 메커니즘을 제공한다 (DynamoDB, GCS Object Lock 등).
type FileLocker struct {
	lockFilePath string
}

// NewFileLocker는 새로운 파일 기반 잠금을 생성한다.
func NewFileLocker(stateFilePath string) *FileLocker {
	return &FileLocker{
		lockFilePath: stateFilePath + ".lock",
	}
}

// Lock은 상태 잠금을 획득한다.
func (l *FileLocker) Lock(operation string) (*StateLock, error) {
	// 이미 잠금이 있는지 확인
	if _, err := os.Stat(l.lockFilePath); err == nil {
		// 잠금 파일이 있으면 기존 잠금 정보 읽기
		data, readErr := os.ReadFile(l.lockFilePath)
		if readErr == nil {
			var existing StateLock
			if json.Unmarshal(data, &existing) == nil {
				return nil, fmt.Errorf(
					"상태가 이미 잠겨 있습니다!\n"+
						"  ID:        %s\n"+
						"  Operation: %s\n"+
						"  Who:       %s\n"+
						"  Created:   %s\n"+
						"잠금 해제: terraform force-unlock %s",
					existing.ID, existing.Operation, existing.Who,
					existing.Created.Format(time.RFC3339), existing.ID)
			}
		}
	}

	lock := &StateLock{
		ID:        fmt.Sprintf("lock-%d", time.Now().UnixNano()),
		Operation: operation,
		Who:       "poc-demo@localhost",
		Created:   time.Now(),
		Path:      l.lockFilePath,
	}

	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(l.lockFilePath, data, 0644); err != nil {
		return nil, fmt.Errorf("잠금 파일 생성 실패: %w", err)
	}

	return lock, nil
}

// Unlock은 상태 잠금을 해제한다.
func (l *FileLocker) Unlock(lockID string) error {
	// 잠금 파일 확인
	data, err := os.ReadFile(l.lockFilePath)
	if err != nil {
		return fmt.Errorf("잠금 파일을 찾을 수 없습니다: %w", err)
	}

	var existing StateLock
	if err := json.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("잠금 파일 파싱 실패: %w", err)
	}

	if existing.ID != lockID {
		return fmt.Errorf("잠금 ID 불일치: 예상 %q, 실제 %q", lockID, existing.ID)
	}

	return os.Remove(l.lockFilePath)
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║   Terraform State 관리 시뮬레이션                         ║")
	fmt.Println("║   실제 코드: internal/states/, internal/statemgr/        ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "terraform-state-poc-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// =========================================================================
	// 데모 1: SyncState로 상태 관리
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 1: SyncState - 동시성 안전한 상태 관리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	syncState := NewSyncState()

	// 리소스 추가 (병렬 시뮬레이션)
	var wg sync.WaitGroup
	resources := []struct {
		moduleAddr   string
		resourceAddr string
		instanceKey  string
		attrs        map[string]string
	}{
		{
			moduleAddr: "root", resourceAddr: "aws_vpc.main", instanceKey: "current",
			attrs: map[string]string{"id": "vpc-abc123", "cidr_block": "10.0.0.0/16"},
		},
		{
			moduleAddr: "root", resourceAddr: "aws_subnet.public", instanceKey: "current",
			attrs: map[string]string{"id": "subnet-def456", "vpc_id": "vpc-abc123", "cidr_block": "10.0.1.0/24"},
		},
		{
			moduleAddr: "root", resourceAddr: "aws_instance.web", instanceKey: "current",
			attrs: map[string]string{"id": "i-ghi789", "ami": "ami-12345", "instance_type": "t3.micro"},
		},
		{
			moduleAddr: "module.network", resourceAddr: "aws_route_table.main", instanceKey: "current",
			attrs: map[string]string{"id": "rtb-jkl012", "vpc_id": "vpc-abc123"},
		},
	}

	fmt.Println("  병렬로 리소스 상태 추가:")
	for _, r := range resources {
		wg.Add(1)
		go func(ma, ra, ik string, attrs map[string]string) {
			defer wg.Done()
			syncState.SetResourceInstance(ma, ra, ik, &ResourceInstance{
				Status:     "current",
				Attributes: attrs,
			})
			fmt.Printf("    ✓ [%s] %s 추가 완료\n", ma, ra)
		}(r.moduleAddr, r.resourceAddr, r.instanceKey, r.attrs)
	}
	wg.Wait()
	fmt.Println()

	// 상태 조회
	fmt.Println("  상태 조회:")
	instance := syncState.GetResourceInstance("root", "aws_vpc.main", "current")
	if instance != nil {
		fmt.Printf("    aws_vpc.main: id=%s, cidr=%s\n",
			instance.Attributes["id"], instance.Attributes["cidr_block"])
	}
	instance = syncState.GetResourceInstance("module.network", "aws_route_table.main", "current")
	if instance != nil {
		fmt.Printf("    module.network/aws_route_table.main: id=%s\n", instance.Attributes["id"])
	}
	fmt.Println()

	// =========================================================================
	// 데모 2: 상태 직렬화/역직렬화
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 2: 상태 직렬화/역직렬화 (JSON)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	state := syncState.GetState()
	serialized, err := SerializeState(state, 1)
	if err != nil {
		fmt.Printf("  직렬화 실패: %v\n", err)
		return
	}

	// 파일에 저장
	stateFilePath := filepath.Join(tmpDir, "terraform.tfstate")
	if err := os.WriteFile(stateFilePath, serialized, 0644); err != nil {
		fmt.Printf("  파일 저장 실패: %v\n", err)
		return
	}
	fmt.Printf("  상태 파일 저장: %s\n", stateFilePath)
	fmt.Printf("  파일 크기: %d bytes\n", len(serialized))
	fmt.Println()

	// JSON 내용 출력 (처음 40줄)
	fmt.Println("  직렬화된 상태 (JSON):")
	lines := splitLines(string(serialized))
	maxLines := 40
	if len(lines) < maxLines {
		maxLines = len(lines)
	}
	for i := 0; i < maxLines; i++ {
		fmt.Printf("    %s\n", lines[i])
	}
	if len(lines) > maxLines {
		fmt.Printf("    ... (총 %d줄)\n", len(lines))
	}
	fmt.Println()

	// 역직렬화
	readData, _ := os.ReadFile(stateFilePath)
	deserialized, err := DeserializeState(readData)
	if err != nil {
		fmt.Printf("  역직렬화 실패: %v\n", err)
		return
	}

	fmt.Printf("  역직렬화 결과:\n")
	fmt.Printf("    Version:          %d\n", deserialized.Version)
	fmt.Printf("    TerraformVersion: %s\n", deserialized.TerraformVersion)
	fmt.Printf("    Serial:           %d\n", deserialized.Serial)
	fmt.Printf("    Lineage:          %s\n", deserialized.Lineage)
	fmt.Printf("    Module 수:        %d\n", len(deserialized.State.Modules))
	for modAddr, mod := range deserialized.State.Modules {
		fmt.Printf("    [%s] 리소스 %d개\n", modAddr, len(mod.Resources))
	}
	fmt.Println()

	// =========================================================================
	// 데모 3: 상태 잠금 (State Locking)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 3: 상태 잠금 (State Locking)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	locker := NewFileLocker(stateFilePath)

	// 잠금 획득
	fmt.Println("  1. 잠금 획득 시도 (terraform apply):")
	lock, err := locker.Lock("OperationTypeApply")
	if err != nil {
		fmt.Printf("     실패: %v\n", err)
		return
	}
	fmt.Printf("     ✓ 잠금 획득 성공\n")
	fmt.Printf("       ID:        %s\n", lock.ID)
	fmt.Printf("       Operation: %s\n", lock.Operation)
	fmt.Printf("       Who:       %s\n", lock.Who)
	fmt.Printf("       Created:   %s\n", lock.Created.Format(time.RFC3339))
	fmt.Println()

	// 다른 사용자가 잠금 시도 (실패해야 함)
	fmt.Println("  2. 다른 사용자 잠금 시도 (동시 실행 시뮬레이션):")
	_, err = locker.Lock("OperationTypePlan")
	if err != nil {
		fmt.Printf("     ✗ 예상대로 실패:\n")
		for _, line := range splitLines(err.Error()) {
			fmt.Printf("       %s\n", line)
		}
	}
	fmt.Println()

	// 잠금 해제
	fmt.Println("  3. 잠금 해제:")
	err = locker.Unlock(lock.ID)
	if err != nil {
		fmt.Printf("     실패: %v\n", err)
	} else {
		fmt.Printf("     ✓ 잠금 해제 성공 (ID: %s)\n", lock.ID)
	}
	fmt.Println()

	// 해제 후 다시 잠금 (성공해야 함)
	fmt.Println("  4. 해제 후 재잠금 시도:")
	lock2, err := locker.Lock("OperationTypePlan")
	if err != nil {
		fmt.Printf("     실패: %v\n", err)
	} else {
		fmt.Printf("     ✓ 잠금 획득 성공 (ID: %s)\n", lock2.ID)
		locker.Unlock(lock2.ID) // 정리
	}
	fmt.Println()

	// =========================================================================
	// 데모 4: Deposed 인스턴스 (create_before_destroy)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 4: Deposed 인스턴스 (create_before_destroy)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  create_before_destroy 동작 흐름:")
	fmt.Println()
	fmt.Println("    1. 현재 인스턴스를 deposed로 표시")
	fmt.Println("    2. 새 인스턴스를 current로 생성")
	fmt.Println("    3. 새 인스턴스 성공 → deposed 인스턴스 삭제")
	fmt.Println("    4. 새 인스턴스 실패 → deposed 인스턴스를 current로 복원")
	fmt.Println()

	// 현재 EC2 인스턴스 확인
	fmt.Println("  [before] aws_instance.web 상태:")
	ec2Instance := syncState.GetResourceInstance("root", "aws_instance.web", "current")
	if ec2Instance != nil {
		fmt.Printf("    current: id=%s, ami=%s\n",
			ec2Instance.Attributes["id"], ec2Instance.Attributes["ami"])
	}
	fmt.Println()

	// 1. 현재 인스턴스를 deposed로
	fmt.Println("  단계 1: 현재 인스턴스를 deposed로 전환")
	syncState.DeposeResourceInstance("root", "aws_instance.web", "current", "deposed-001")

	deposedInstance := syncState.GetResourceInstance("root", "aws_instance.web", "deposed-001")
	if deposedInstance != nil {
		fmt.Printf("    deposed-001: id=%s, ami=%s (상태: %s)\n",
			deposedInstance.Attributes["id"], deposedInstance.Attributes["ami"],
			deposedInstance.Status)
	}
	fmt.Println()

	// 2. 새 인스턴스를 current로 생성
	fmt.Println("  단계 2: 새 인스턴스를 current로 생성")
	syncState.SetResourceInstance("root", "aws_instance.web", "current", &ResourceInstance{
		Status: "current",
		Attributes: map[string]string{
			"id":            "i-new99999",
			"ami":           "ami-new67890",
			"instance_type": "t3.micro",
		},
	})

	newInstance := syncState.GetResourceInstance("root", "aws_instance.web", "current")
	if newInstance != nil {
		fmt.Printf("    current: id=%s, ami=%s (상태: %s)\n",
			newInstance.Attributes["id"], newInstance.Attributes["ami"],
			newInstance.Status)
	}
	fmt.Println()

	// 3. 새 인스턴스 성공 → deposed 삭제
	fmt.Println("  단계 3: 새 인스턴스 생성 성공 → deposed 인스턴스 삭제")
	syncState.RemoveResourceInstance("root", "aws_instance.web", "deposed-001")
	fmt.Printf("    deposed-001 삭제됨\n")
	fmt.Println()

	// 최종 상태 확인
	fmt.Println("  [after] aws_instance.web 최종 상태:")
	finalInstance := syncState.GetResourceInstance("root", "aws_instance.web", "current")
	if finalInstance != nil {
		fmt.Printf("    current: id=%s, ami=%s\n",
			finalInstance.Attributes["id"], finalInstance.Attributes["ami"])
	}

	// =========================================================================
	// 데모 5: 상태 업데이트 후 재직렬화
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 5: 상태 업데이트 후 재직렬화 (Serial 증가)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	updatedState := syncState.GetState()
	serialized2, _ := SerializeState(updatedState, 2)

	fmt.Printf("  이전 Serial: 1 → 새 Serial: 2\n")
	fmt.Printf("  업데이트된 상태 크기: %d bytes\n", len(serialized2))
	fmt.Println()

	// =========================================================================
	// 핵심 포인트
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  핵심 포인트 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. State는 실제 인프라와 설정 파일 사이의 매핑을 저장한다")
	fmt.Println("  2. SyncState는 RWMutex로 병렬 apply 중 안전한 접근을 보장한다")
	fmt.Println("  3. State는 JSON v4 포맷으로 직렬화/역직렬화된다")
	fmt.Println("  4. State Locking으로 동시 실행(apply/plan) 충돌을 방지한다")
	fmt.Println("  5. Deposed 인스턴스는 create_before_destroy의 핵심 메커니즘이다")
	fmt.Println("  6. Serial 번호로 상태 파일의 버전을 추적한다")
}

func splitLines(s string) []string {
	var lines []string
	current := ""
	for _, ch := range s {
		if ch == '\n' {
			lines = append(lines, current)
			current = ""
		} else {
			current += string(ch)
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}
