// poc-23-snapshot: Grafana 대시보드 스냅샷 시스템 시뮬레이션
//
// 핵심 개념:
//   - Key/DeleteKey 분리 설계 (접근 키와 삭제 키 분리)
//   - 대시보드 데이터 암호화 저장 (XOR 기반 간이 암호화)
//   - 만료 시간 기반 자동 정리
//   - 역할 기반 검색 (Admin은 전체, 일반 사용자는 자신의 것만)
//
// 실행: go run main.go

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// --- 데이터 모델 ---

type DashboardSnapshot struct {
	ID                 int64
	Name               string
	Key                string // 접근 키 (공유용)
	DeleteKey          string // 삭제 키 (생성자만 보유)
	OrgID              int64
	UserID             int64
	External           bool
	ExternalURL        string
	Expires            time.Time
	Created            time.Time
	DashboardEncrypted []byte // 암호화된 대시보드 데이터
}

type SnapshotDTO struct {
	Name    string `json:"name"`
	Key     string `json:"key"`
	Expires string `json:"expires"`
}

type CreateSnapshotCmd struct {
	Name      string
	Dashboard map[string]interface{}
	Expires   int64 // 초 단위, 0이면 영구
	OrgID     int64
	UserID    int64
	External  bool
}

type CreateSnapshotResult struct {
	Key       string
	DeleteKey string
	URL       string
	DeleteURL string
}

// --- 간이 암호화 서비스 ---

type EncryptionService struct {
	secretKey []byte
}

func NewEncryptionService(key string) *EncryptionService {
	return &EncryptionService{secretKey: []byte(key)}
}

func (e *EncryptionService) Encrypt(data []byte) []byte {
	result := make([]byte, len(data))
	for i, b := range data {
		result[i] = b ^ e.secretKey[i%len(e.secretKey)]
	}
	return result
}

func (e *EncryptionService) Decrypt(data []byte) []byte {
	return e.Encrypt(data) // XOR 대칭 암호화
}

// --- 랜덤 키 생성 ---

func generateRandomKey(length int) string {
	b := make([]byte, length)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:length]
}

// --- Store (인메모리 데이터베이스) ---

type SnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]*DashboardSnapshot // Key -> Snapshot
	deleteIdx map[string]string             // DeleteKey -> Key
	nextID    int64
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{
		snapshots: make(map[string]*DashboardSnapshot),
		deleteIdx: make(map[string]string),
		nextID:    1,
	}
}

func (s *SnapshotStore) Create(snap *DashboardSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap.ID = s.nextID
	s.nextID++
	s.snapshots[snap.Key] = snap
	s.deleteIdx[snap.DeleteKey] = snap.Key
}

func (s *SnapshotStore) GetByKey(key string) (*DashboardSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.snapshots[key]
	return snap, ok
}

func (s *SnapshotStore) GetByDeleteKey(deleteKey string) (*DashboardSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.deleteIdx[deleteKey]
	if !ok {
		return nil, false
	}
	snap, ok := s.snapshots[key]
	return snap, ok
}

func (s *SnapshotStore) Delete(deleteKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.deleteIdx[deleteKey]
	if !ok {
		return false
	}
	delete(s.snapshots, key)
	delete(s.deleteIdx, deleteKey)
	return true
}

func (s *SnapshotStore) DeleteExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	deleted := 0
	for key, snap := range s.snapshots {
		if snap.Expires.Before(now) {
			delete(s.deleteIdx, snap.DeleteKey)
			delete(s.snapshots, key)
			deleted++
		}
	}
	return deleted
}

// Search: Admin은 조직의 전체, 일반 사용자는 자신의 것만
func (s *SnapshotStore) Search(orgID, userID int64, isAdmin bool) []SnapshotDTO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []SnapshotDTO
	for _, snap := range s.snapshots {
		if snap.OrgID != orgID {
			continue
		}
		if !isAdmin && snap.UserID != userID {
			continue
		}
		result = append(result, SnapshotDTO{
			Name:    snap.Name,
			Key:     snap.Key,
			Expires: snap.Expires.Format(time.RFC3339),
		})
	}
	return result
}

// --- Service ---

type SnapshotService struct {
	store      *SnapshotStore
	encryption *EncryptionService
	baseURL    string
}

func NewSnapshotService(store *SnapshotStore, enc *EncryptionService) *SnapshotService {
	return &SnapshotService{
		store:      store,
		encryption: enc,
		baseURL:    "http://grafana.example.com",
	}
}

func (s *SnapshotService) Create(cmd CreateSnapshotCmd) (*CreateSnapshotResult, error) {
	// 1. 대시보드 데이터 직렬화
	dashJSON, err := json.Marshal(cmd.Dashboard)
	if err != nil {
		return nil, fmt.Errorf("대시보드 직렬화 실패: %w", err)
	}

	// 2. 대시보드 데이터 암호화
	encrypted := s.encryption.Encrypt(dashJSON)

	// 3. 키 생성
	key := generateRandomKey(32)
	deleteKey := generateRandomKey(32)

	// 4. 만료 시간 계산
	var expires time.Time
	if cmd.Expires > 0 {
		expires = time.Now().Add(time.Second * time.Duration(cmd.Expires))
	} else {
		expires = time.Now().Add(time.Hour * 24 * 365 * 50) // 50년 (실질 영구)
	}

	// 5. 스냅샷 저장
	snap := &DashboardSnapshot{
		Name:               cmd.Name,
		Key:                key,
		DeleteKey:          deleteKey,
		OrgID:              cmd.OrgID,
		UserID:             cmd.UserID,
		External:           cmd.External,
		Expires:            expires,
		Created:            time.Now(),
		DashboardEncrypted: encrypted,
	}
	s.store.Create(snap)

	return &CreateSnapshotResult{
		Key:       key,
		DeleteKey: deleteKey,
		URL:       fmt.Sprintf("%s/dashboard/snapshot/%s", s.baseURL, key),
		DeleteURL: fmt.Sprintf("%s/api/snapshots-delete/%s", s.baseURL, deleteKey),
	}, nil
}

func (s *SnapshotService) Get(key string) (map[string]interface{}, error) {
	snap, ok := s.store.GetByKey(key)
	if !ok {
		return nil, fmt.Errorf("스냅샷을 찾을 수 없음")
	}

	// 만료 확인
	if snap.Expires.Before(time.Now()) {
		return nil, fmt.Errorf("스냅샷이 만료됨")
	}

	// 복호화
	decrypted := s.encryption.Decrypt(snap.DashboardEncrypted)
	var dashboard map[string]interface{}
	if err := json.Unmarshal(decrypted, &dashboard); err != nil {
		return nil, fmt.Errorf("대시보드 복호화 실패: %w", err)
	}

	return dashboard, nil
}

func (s *SnapshotService) DeleteByKey(deleteKey string) error {
	if !s.store.Delete(deleteKey) {
		return fmt.Errorf("스냅샷을 찾을 수 없음")
	}
	return nil
}

// --- 메인 ---

func main() {
	fmt.Println("=== Grafana 대시보드 스냅샷 시스템 시뮬레이션 ===")
	fmt.Println()

	store := NewSnapshotStore()
	enc := NewEncryptionService("my-secret-key-for-encryption!!")
	svc := NewSnapshotService(store, enc)

	// 1. 스냅샷 생성
	fmt.Println("--- 1. 스냅샷 생성 ---")
	result1, _ := svc.Create(CreateSnapshotCmd{
		Name: "프로덕션 서버 상태",
		Dashboard: map[string]interface{}{
			"uid":   "abc123",
			"title": "Server Dashboard",
			"panels": []map[string]interface{}{
				{"id": 1, "title": "CPU 사용량", "data": []float64{45.2, 67.8, 89.1}},
				{"id": 2, "title": "메모리", "data": []float64{72.0, 75.5, 80.2}},
			},
		},
		OrgID:  1,
		UserID: 100,
	})
	fmt.Printf("  스냅샷 URL:  %s\n", result1.URL)
	fmt.Printf("  삭제 URL:    %s\n", result1.DeleteURL)
	fmt.Printf("  Access Key:  %s...\n", result1.Key[:16])
	fmt.Printf("  Delete Key:  %s...\n", result1.DeleteKey[:16])
	fmt.Println()

	// 2. 만료 있는 스냅샷 생성
	fmt.Println("--- 2. 만료 있는 스냅샷 (1초 후 만료) ---")
	result2, _ := svc.Create(CreateSnapshotCmd{
		Name: "임시 장애 보고서",
		Dashboard: map[string]interface{}{
			"uid":   "def456",
			"title": "Incident Report",
		},
		Expires: 1, // 1초 후 만료
		OrgID:   1,
		UserID:  100,
	})
	fmt.Printf("  스냅샷 키: %s...\n", result2.Key[:16])

	// 다른 사용자의 스냅샷
	svc.Create(CreateSnapshotCmd{
		Name: "개발 환경 모니터링",
		Dashboard: map[string]interface{}{
			"uid":   "ghi789",
			"title": "Dev Dashboard",
		},
		OrgID:  1,
		UserID: 200,
	})

	// 3. 스냅샷 조회 (복호화)
	fmt.Println()
	fmt.Println("--- 3. 스냅샷 조회 (데이터 복호화) ---")
	dashboard, err := svc.Get(result1.Key)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
	} else {
		dashJSON, _ := json.MarshalIndent(dashboard, "  ", "  ")
		fmt.Printf("  복호화된 대시보드:\n  %s\n", string(dashJSON))
	}

	// 4. 역할 기반 검색
	fmt.Println()
	fmt.Println("--- 4. 역할 기반 검색 ---")

	adminResults := store.Search(1, 100, true)
	fmt.Printf("  Admin (UserID=100) 검색: %d개 발견\n", len(adminResults))
	for _, r := range adminResults {
		fmt.Printf("    - %s (key: %s...)\n", r.Name, r.Key[:8])
	}

	userResults := store.Search(1, 100, false)
	fmt.Printf("  일반 사용자 (UserID=100) 검색: %d개 발견\n", len(userResults))
	for _, r := range userResults {
		fmt.Printf("    - %s (key: %s...)\n", r.Name, r.Key[:8])
	}

	// 5. 만료된 스냅샷 정리
	fmt.Println()
	fmt.Println("--- 5. 만료된 스냅샷 정리 ---")
	time.Sleep(1100 * time.Millisecond) // 만료 대기

	deleted := store.DeleteExpired()
	fmt.Printf("  정리된 스냅샷: %d개\n", deleted)

	// 만료된 스냅샷 조회 시도
	_, err = svc.Get(result2.Key)
	fmt.Printf("  만료된 스냅샷 조회: %v\n", err)

	// 6. DeleteKey로 삭제
	fmt.Println()
	fmt.Println("--- 6. DeleteKey로 스냅샷 삭제 ---")
	err = svc.DeleteByKey(result1.DeleteKey)
	if err != nil {
		fmt.Printf("  삭제 실패: %v\n", err)
	} else {
		fmt.Println("  스냅샷 삭제 성공")
	}

	// Access Key로는 삭제 불가 (Key != DeleteKey)
	_, err = svc.Get(result1.Key)
	fmt.Printf("  삭제 후 조회: %v\n", err)

	// 7. 암호화 확인
	fmt.Println()
	fmt.Println("--- 7. 암호화 저장 검증 ---")
	result3, _ := svc.Create(CreateSnapshotCmd{
		Name:      "암호화 테스트",
		Dashboard: map[string]interface{}{"secret": "비밀 데이터"},
		OrgID:     1,
		UserID:    100,
	})
	snap, _ := store.GetByKey(result3.Key)
	encLen := len(snap.DashboardEncrypted)
	if encLen > 20 {
		encLen = 20
	}
	fmt.Printf("  암호화된 데이터: %s...\n", hex.EncodeToString(snap.DashboardEncrypted[:encLen]))
	fmt.Printf("  암호화 데이터에 'secret' 포함 여부: %v\n",
		strings.Contains(string(snap.DashboardEncrypted), "secret"))

	decrypted, _ := svc.Get(result3.Key)
	fmt.Printf("  복호화 결과: %v\n", decrypted)

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
