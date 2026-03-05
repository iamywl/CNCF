// poc-16-sharding/main.go
//
// Argo CD 클러스터 샤딩 시뮬레이션
//
// 핵심 개념:
//   - 3가지 분배 알고리즘: Legacy(FNV-32a), RoundRobin, ConsistentHashing
//   - ClusterShardingCache 인터페이스: Init, Add, Delete, Update, IsManagedCluster
//   - 하트비트 프로토콜: HeartbeatDuration=10s, HeartbeatTimeout=30s
//   - ConfigMap 기반 샤드 할당
//   - 데드 샤드 감지 및 인계
//   - StatefulSet 호스트명 파싱으로 샤드 번호 추론
//   - getOrUpdateShardNumberForController()
//
// 실행: go run main.go

package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================
// 도메인 모델
// ============================================================

// Cluster 클러스터 정보
type Cluster struct {
	ID     string // 클러스터 고유 ID (서버 URL 기반)
	Name   string // 클러스터 이름
	Server string // API 서버 URL
}

// ShardAssignment 샤드 할당 정보
type ShardAssignment struct {
	ShardNumber  int
	ControllerID string // 할당된 컨트롤러 (StatefulSet Pod 이름)
	AssignedAt   time.Time
}

// ControllerShard 컨트롤러 샤드 정보 (ConfigMap에 저장)
type ControllerShard struct {
	ShardNumber int
	Hostname    string
	HeartbeatAt time.Time
}

// ============================================================
// 상수
// ============================================================

const (
	// 하트비트 설정 (실제 Argo CD와 동일)
	HeartbeatDuration = 10 * time.Second // 하트비트 주기
	HeartbeatTimeout  = 30 * time.Second // 3 * HeartbeatDuration

	// 분배 알고리즘 이름
	AlgorithmLegacy            = "legacy"
	AlgorithmRoundRobin        = "round-robin"
	AlgorithmConsistentHashing = "consistent-hashing"
)

// ============================================================
// 분배 알고리즘 1: Legacy (FNV-32a 해시)
// ============================================================
//
// 실제 Argo CD: controller/sharding/sharding.go getLegacyShardNumber()
// 클러스터 ID의 FNV-32a 해시 값을 레플리카 수로 나눈 나머지

// LegacyDistribute FNV-32a 해시 기반 분배
func LegacyDistribute(clusters []*Cluster, replicas int) map[string]int {
	assignment := make(map[string]int)
	for _, c := range clusters {
		shard := fnv32aHash(c.ID) % replicas
		assignment[c.ID] = shard
	}
	return assignment
}

// fnv32aHash FNV-32a 해시 계산
func fnv32aHash(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	return int(h.Sum32())
}

// ============================================================
// 분배 알고리즘 2: RoundRobin
// ============================================================
//
// 실제 Argo CD: controller/sharding/sharding.go getRoundRobinShardNumber()
// 클러스터를 알파벳 순 정렬 후 인덱스 % 레플리카 수

// RoundRobinDistribute 라운드 로빈 기반 분배
func RoundRobinDistribute(clusters []*Cluster, replicas int) map[string]int {
	// ID 순 정렬 (결정론적 분배)
	sorted := make([]*Cluster, len(clusters))
	copy(sorted, clusters)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})

	assignment := make(map[string]int)
	for i, c := range sorted {
		assignment[c.ID] = i % replicas
	}
	return assignment
}

// ============================================================
// 분배 알고리즘 3: ConsistentHashing (Bounded Loads)
// ============================================================
//
// 실제 Argo CD: controller/sharding/consistent_hashing.go
// 각 샤드에 최대 허용 부하를 설정하여 균등 분배 보장

// ConsistentHashRing 일관성 해시 링
type ConsistentHashRing struct {
	mu       sync.RWMutex
	replicas int
	// 샤드별 클러스터 할당
	shardLoads []int // 각 샤드의 현재 부하
	assignment map[string]int
}

// NewConsistentHashRing 일관성 해시 링 생성
func NewConsistentHashRing(replicas int) *ConsistentHashRing {
	return &ConsistentHashRing{
		replicas:   replicas,
		shardLoads: make([]int, replicas),
		assignment: make(map[string]int),
	}
}

// maxLoad 허용 최대 부하 (Bounded Loads 공식)
// ceil((total + 1) * capacityFactor / replicas)
func (r *ConsistentHashRing) maxLoad(total int) int {
	const capacityFactor = 1.25 // 25% 여유분 허용
	return int(math.Ceil(float64(total+1) * capacityFactor / float64(r.replicas)))
}

// GetLeast 가장 부하가 낮은 샤드 반환
func (r *ConsistentHashRing) GetLeast() int {
	minLoad := r.shardLoads[0]
	minShard := 0
	for i, load := range r.shardLoads {
		if load < minLoad {
			minLoad = load
			minShard = i
		}
	}
	return minShard
}

// Add 클러스터를 해시 링에 추가
func (r *ConsistentHashRing) Add(cluster *Cluster, totalClusters int) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 선호 샤드 = FNV32a(clusterID) % replicas
	preferred := fnv32aHash(cluster.ID) % r.replicas
	maxAllowed := r.maxLoad(totalClusters)

	// 선호 샤드에 공간이 있으면 사용
	if r.shardLoads[preferred] < maxAllowed {
		r.assignment[cluster.ID] = preferred
		r.shardLoads[preferred]++
		return preferred
	}

	// 선호 샤드가 꽉 찼으면 가장 낮은 부하의 샤드 선택
	leastShard := r.GetLeast()
	r.assignment[cluster.ID] = leastShard
	r.shardLoads[leastShard]++
	return leastShard
}

// ConsistentHashDistribute 일관성 해시 기반 분배
func ConsistentHashDistribute(clusters []*Cluster, replicas int) map[string]int {
	ring := NewConsistentHashRing(replicas)
	assignment := make(map[string]int)

	// ID 순으로 추가 (결정론적 결과)
	sorted := make([]*Cluster, len(clusters))
	copy(sorted, clusters)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})

	for _, c := range sorted {
		shard := ring.Add(c, len(clusters))
		assignment[c.ID] = shard
	}
	return assignment
}

// ============================================================
// ClusterShardingCache 인터페이스
// ============================================================

// ClusterShardingCache 샤드 캐시 인터페이스
// 실제 Argo CD: controller/sharding/sharding.go ClusterShardingCache
type ClusterShardingCache interface {
	Init(clusters []*Cluster, replicas int)
	Add(cluster *Cluster)
	Delete(clusterID string)
	Update(cluster *Cluster)
	IsManagedCluster(clusterID string) bool
	GetShardNumber(clusterID string) (int, bool)
}

// ShardingCache 샤드 캐시 구현
type ShardingCache struct {
	mu          sync.RWMutex
	algorithm   string
	replicas    int
	ownShard    int // 이 컨트롤러의 샤드 번호
	clusters    map[string]*Cluster
	assignments map[string]int
}

// NewShardingCache 샤드 캐시 생성
func NewShardingCache(algorithm string, replicas, ownShard int) *ShardingCache {
	return &ShardingCache{
		algorithm:   algorithm,
		replicas:    replicas,
		ownShard:    ownShard,
		clusters:    make(map[string]*Cluster),
		assignments: make(map[string]int),
	}
}

// distribute 현재 알고리즘으로 클러스터 재분배
func (sc *ShardingCache) distribute() {
	var clusters []*Cluster
	for _, c := range sc.clusters {
		clusters = append(clusters, c)
	}

	switch sc.algorithm {
	case AlgorithmLegacy:
		sc.assignments = LegacyDistribute(clusters, sc.replicas)
	case AlgorithmRoundRobin:
		sc.assignments = RoundRobinDistribute(clusters, sc.replicas)
	case AlgorithmConsistentHashing:
		sc.assignments = ConsistentHashDistribute(clusters, sc.replicas)
	default:
		sc.assignments = LegacyDistribute(clusters, sc.replicas)
	}
}

// Init 초기화 — 모든 클러스터 로드 및 분배
func (sc *ShardingCache) Init(clusters []*Cluster, replicas int) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.replicas = replicas
	sc.clusters = make(map[string]*Cluster)
	for _, c := range clusters {
		sc.clusters[c.ID] = c
	}
	sc.distribute()
}

// Add 클러스터 추가
func (sc *ShardingCache) Add(cluster *Cluster) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.clusters[cluster.ID] = cluster
	sc.distribute() // 재분배
}

// Delete 클러스터 삭제
func (sc *ShardingCache) Delete(clusterID string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	delete(sc.clusters, clusterID)
	delete(sc.assignments, clusterID)
	sc.distribute() // 재분배
}

// Update 클러스터 정보 업데이트
func (sc *ShardingCache) Update(cluster *Cluster) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.clusters[cluster.ID] = cluster
	sc.distribute()
}

// IsManagedCluster 이 컨트롤러가 관리하는 클러스터인지 확인
func (sc *ShardingCache) IsManagedCluster(clusterID string) bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	shard, ok := sc.assignments[clusterID]
	if !ok {
		return false
	}
	return shard == sc.ownShard
}

// GetShardNumber 클러스터의 샤드 번호 반환
func (sc *ShardingCache) GetShardNumber(clusterID string) (int, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	shard, ok := sc.assignments[clusterID]
	return shard, ok
}

// ============================================================
// 하트비트 프로토콜 (ConfigMap 기반)
// ============================================================
//
// 실제 Argo CD:
//   - controller/sharding/sharding.go heartbeatCurrentShard()
//   - ConfigMap: argocd-app-controller-shard-lock
//
// 구조:
//   ConfigMap.data:
//     "shard-0": {"hostname": "argocd-app-controller-0", "heartbeatAt": "2024-01-01T00:00:00Z"}
//     "shard-1": {"hostname": "argocd-app-controller-1", "heartbeatAt": "2024-01-01T00:00:00Z"}

// ShardConfigMap ConfigMap 기반 샤드 할당 저장소
type ShardConfigMap struct {
	mu     sync.RWMutex
	shards map[int]*ControllerShard
}

// NewShardConfigMap ConfigMap 저장소 생성
func NewShardConfigMap() *ShardConfigMap {
	return &ShardConfigMap{
		shards: make(map[int]*ControllerShard),
	}
}

// UpdateHeartbeat 샤드 하트비트 업데이트
func (cm *ShardConfigMap) UpdateHeartbeat(shardNumber int, hostname string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.shards[shardNumber] = &ControllerShard{
		ShardNumber: shardNumber,
		Hostname:    hostname,
		HeartbeatAt: time.Now(),
	}
}

// GetDeadShards 타임아웃된 샤드 목록 반환
func (cm *ShardConfigMap) GetDeadShards(replicas int) []int {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	var dead []int
	now := time.Now()

	for shard := 0; shard < replicas; shard++ {
		info, ok := cm.shards[shard]
		if !ok {
			// ConfigMap에 항목 없음 → 미등록 샤드 (데드로 간주)
			dead = append(dead, shard)
			continue
		}
		if now.Sub(info.HeartbeatAt) > HeartbeatTimeout {
			dead = append(dead, shard)
		}
	}
	return dead
}

// GetActiveShards 활성 샤드 목록 반환
func (cm *ShardConfigMap) GetActiveShards() []*ControllerShard {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	var active []*ControllerShard
	now := time.Now()
	for _, shard := range cm.shards {
		if now.Sub(shard.HeartbeatAt) <= HeartbeatTimeout {
			active = append(active, shard)
		}
	}
	return active
}

// PrintStatus ConfigMap 상태 출력
func (cm *ShardConfigMap) PrintStatus(replicas int) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	now := time.Now()

	fmt.Printf("  ConfigMap 상태 (레플리카=%d, 타임아웃=%v):\n", replicas, HeartbeatTimeout)
	for shard := 0; shard < replicas; shard++ {
		info, ok := cm.shards[shard]
		if !ok {
			fmt.Printf("    샤드 %d: [미등록]\n", shard)
			continue
		}
		elapsed := now.Sub(info.HeartbeatAt)
		status := "활성"
		if elapsed > HeartbeatTimeout {
			status = "데드"
		}
		fmt.Printf("    샤드 %d: hostname=%-30s 마지막하트비트=%v 전 [%s]\n",
			shard, info.Hostname, elapsed.Round(time.Millisecond), status)
	}
}

// ============================================================
// StatefulSet 샤드 추론
// ============================================================
//
// 실제 Argo CD: controller/sharding/sharding.go InferShard()
// StatefulSet Pod 이름은 "<name>-<ordinal>" 형식
// 예: argocd-application-controller-2 → 샤드 2

// InferShard StatefulSet Pod 호스트명에서 샤드 번호 추론
func InferShard(hostname string) (int, error) {
	parts := strings.Split(hostname, "-")
	if len(parts) == 0 {
		return 0, fmt.Errorf("호스트명 파싱 실패: %s", hostname)
	}
	// 마지막 '-' 뒤의 숫자를 샤드 번호로 사용
	suffix := parts[len(parts)-1]
	shard, err := strconv.Atoi(suffix)
	if err != nil {
		return 0, fmt.Errorf("샤드 번호 파싱 실패 [%s]: %w", suffix, err)
	}
	return shard, nil
}

// ============================================================
// getOrUpdateShardNumberForController
// ============================================================
//
// 실제 Argo CD: controller/sharding/sharding.go getOrUpdateShardNumberForController()
// 컨트롤러 시작 시 샤드 번호 결정:
//   1. StatefulSet 호스트명 파싱 시도
//   2. 실패하면 ConfigMap에서 빈 슬롯 찾기
//   3. 없으면 데드 샤드 인계

// getOrUpdateShardNumberForController 컨트롤러의 샤드 번호 결정
func getOrUpdateShardNumberForController(
	hostname string,
	replicas int,
	configMap *ShardConfigMap,
) (int, error) {
	// 1. StatefulSet 호스트명에서 추론 시도
	shard, err := InferShard(hostname)
	if err == nil && shard < replicas {
		fmt.Printf("  [ShardAssign] StatefulSet 호스트명 파싱 성공: %s → 샤드 %d\n",
			hostname, shard)
		configMap.UpdateHeartbeat(shard, hostname)
		return shard, nil
	}

	// 2. ConfigMap에서 빈 슬롯 찾기
	active := configMap.GetActiveShards()
	activeSet := make(map[int]bool)
	for _, s := range active {
		activeSet[s.ShardNumber] = true
	}

	for i := 0; i < replicas; i++ {
		if !activeSet[i] {
			fmt.Printf("  [ShardAssign] 빈 슬롯 발견: 샤드 %d 할당 (hostname=%s)\n",
				i, hostname)
			configMap.UpdateHeartbeat(i, hostname)
			return i, nil
		}
	}

	// 3. 데드 샤드 인계
	dead := configMap.GetDeadShards(replicas)
	if len(dead) > 0 {
		shard = dead[0]
		fmt.Printf("  [ShardAssign] 데드 샤드 인계: 샤드 %d (hostname=%s)\n",
			shard, hostname)
		configMap.UpdateHeartbeat(shard, hostname)
		return shard, nil
	}

	return 0, fmt.Errorf("사용 가능한 샤드 없음 (replicas=%d, active=%d)", replicas, len(active))
}

// ============================================================
// 분배 시각화 헬퍼
// ============================================================

// printDistribution 분배 결과를 표로 출력
func printDistribution(algorithm string, clusters []*Cluster, assignment map[string]int, replicas int) {
	// 샤드별 클러스터 목록 구성
	shardMap := make(map[int][]string)
	for id, shard := range assignment {
		shardMap[shard] = append(shardMap[shard], id)
	}
	for s := range shardMap {
		sort.Strings(shardMap[s])
	}

	fmt.Printf("\n  알고리즘: %s (클러스터=%d, 레플리카=%d)\n",
		algorithm, len(clusters), replicas)
	fmt.Printf("  %-8s %-12s %s\n", "샤드", "클러스터수", "클러스터 ID 목록")
	fmt.Printf("  %s\n", strings.Repeat("-", 60))
	for shard := 0; shard < replicas; shard++ {
		ids := shardMap[shard]
		fmt.Printf("  %-8d %-12d %s\n", shard, len(ids), strings.Join(ids, ", "))
	}

	// 분산도 계산 (표준편차)
	avg := float64(len(clusters)) / float64(replicas)
	var variance float64
	for shard := 0; shard < replicas; shard++ {
		diff := float64(len(shardMap[shard])) - avg
		variance += diff * diff
	}
	stddev := math.Sqrt(variance / float64(replicas))
	fmt.Printf("  평균=%.1f, 표준편차=%.2f (낮을수록 균등)\n", avg, stddev)
}

// ============================================================
// 시뮬레이션 실행
// ============================================================

func main() {
	fmt.Println("============================================================")
	fmt.Println("  Argo CD 클러스터 샤딩 시뮬레이션 (PoC-16)")
	fmt.Println("============================================================")

	// 테스트 클러스터 생성 (12개)
	clusters := []*Cluster{
		{ID: "cluster-001", Name: "prod-us-east-1", Server: "https://prod-us-east-1.example.com"},
		{ID: "cluster-002", Name: "prod-us-west-2", Server: "https://prod-us-west-2.example.com"},
		{ID: "cluster-003", Name: "prod-eu-west-1", Server: "https://prod-eu-west-1.example.com"},
		{ID: "cluster-004", Name: "prod-ap-east-1", Server: "https://prod-ap-east-1.example.com"},
		{ID: "cluster-005", Name: "staging-us-east-1", Server: "https://staging-us-east-1.example.com"},
		{ID: "cluster-006", Name: "staging-eu-west-1", Server: "https://staging-eu-west-1.example.com"},
		{ID: "cluster-007", Name: "dev-us-east-1", Server: "https://dev-us-east-1.example.com"},
		{ID: "cluster-008", Name: "dev-eu-west-1", Server: "https://dev-eu-west-1.example.com"},
		{ID: "cluster-009", Name: "test-us-east-1", Server: "https://test-us-east-1.example.com"},
		{ID: "cluster-010", Name: "test-eu-west-1", Server: "https://test-eu-west-1.example.com"},
		{ID: "cluster-011", Name: "canary-us-east-1", Server: "https://canary-us-east-1.example.com"},
		{ID: "cluster-012", Name: "canary-eu-west-1", Server: "https://canary-eu-west-1.example.com"},
	}
	replicas := 3

	// ----------------------------------------------------------------
	// 시나리오 1: 3가지 분배 알고리즘 비교
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 1: 3가지 분배 알고리즘 비교")
	fmt.Println("============================")

	// Legacy (FNV-32a)
	legacyAssign := LegacyDistribute(clusters, replicas)
	printDistribution(AlgorithmLegacy, clusters, legacyAssign, replicas)

	// RoundRobin
	rrAssign := RoundRobinDistribute(clusters, replicas)
	printDistribution(AlgorithmRoundRobin, clusters, rrAssign, replicas)

	// ConsistentHashing
	chAssign := ConsistentHashDistribute(clusters, replicas)
	printDistribution(AlgorithmConsistentHashing, clusters, chAssign, replicas)

	// ----------------------------------------------------------------
	// 시나리오 2: ClusterShardingCache — IsManagedCluster
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 2: ClusterShardingCache — IsManagedCluster")
	fmt.Println("============================")

	// 샤드 1을 담당하는 컨트롤러 시뮬레이션
	cache := NewShardingCache(AlgorithmRoundRobin, replicas, 1)
	cache.Init(clusters, replicas)

	fmt.Println("\n  (알고리즘=RoundRobin, 이 컨트롤러 샤드=1)")
	for _, c := range clusters[:6] {
		managed := cache.IsManagedCluster(c.ID)
		shard, _ := cache.GetShardNumber(c.ID)
		fmt.Printf("  %-15s 샤드=%-2d 관리=%v\n", c.ID, shard, managed)
	}

	// 클러스터 추가 후 재분배
	newCluster := &Cluster{
		ID: "cluster-013", Name: "new-cluster", Server: "https://new.example.com",
	}
	fmt.Printf("\n  신규 클러스터 추가: %s\n", newCluster.ID)
	cache.Add(newCluster)
	shard, _ := cache.GetShardNumber(newCluster.ID)
	fmt.Printf("  → 배정 샤드: %d\n", shard)

	// 클러스터 삭제 후 재분배
	fmt.Printf("\n  클러스터 삭제: cluster-001\n")
	cache.Delete("cluster-001")
	shard, ok := cache.GetShardNumber("cluster-001")
	fmt.Printf("  → 삭제 후 조회: shard=%d, found=%v\n", shard, ok)

	// ----------------------------------------------------------------
	// 시나리오 3: 하트비트 프로토콜 및 데드 샤드 감지
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 3: 하트비트 프로토콜 및 데드 샤드 감지")
	fmt.Println("============================")

	configMap := NewShardConfigMap()

	// 컨트롤러 시작 — 하트비트 등록
	fmt.Println("\n  [초기화] 컨트롤러 하트비트 등록:")
	configMap.UpdateHeartbeat(0, "argocd-application-controller-0")
	configMap.UpdateHeartbeat(1, "argocd-application-controller-1")
	configMap.UpdateHeartbeat(2, "argocd-application-controller-2")

	configMap.PrintStatus(replicas)

	// 샤드 2를 데드로 시뮬레이션 (시간 조작)
	fmt.Println("\n  [시뮬레이션] 샤드 2 하트비트 타임아웃 (시간 조작):")
	configMap.mu.Lock()
	configMap.shards[2].HeartbeatAt = time.Now().Add(-(HeartbeatTimeout + 5*time.Second))
	configMap.mu.Unlock()

	configMap.PrintStatus(replicas)

	deadShards := configMap.GetDeadShards(replicas)
	fmt.Printf("\n  감지된 데드 샤드: %v\n", deadShards)

	// ----------------------------------------------------------------
	// 시나리오 4: StatefulSet 호스트명 파싱 (InferShard)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 4: StatefulSet 호스트명에서 샤드 번호 추론")
	fmt.Println("============================")

	hostnames := []string{
		"argocd-application-controller-0",
		"argocd-application-controller-1",
		"argocd-application-controller-2",
		"app-controller-10",     // 큰 번호
		"mycontroller",          // 번호 없음 (실패 예상)
		"controller-abc",        // 비숫자 (실패 예상)
	}

	for _, hostname := range hostnames {
		shard, err := InferShard(hostname)
		if err != nil {
			fmt.Printf("  %-45s → 오류: %v\n", hostname, err)
		} else {
			fmt.Printf("  %-45s → 샤드 %d\n", hostname, shard)
		}
	}

	// ----------------------------------------------------------------
	// 시나리오 5: getOrUpdateShardNumberForController — 동적 할당
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 5: getOrUpdateShardNumberForController — 샤드 동적 할당")
	fmt.Println("============================")

	freshConfigMap := NewShardConfigMap()

	// Case 1: StatefulSet Pod (호스트명으로 추론)
	fmt.Println("\n  [Case 1] StatefulSet Pod 기동:")
	for i := 0; i < replicas; i++ {
		hostname := fmt.Sprintf("argocd-application-controller-%d", i)
		shard, err := getOrUpdateShardNumberForController(hostname, replicas, freshConfigMap)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
		} else {
			_ = shard
		}
	}
	freshConfigMap.PrintStatus(replicas)

	// Case 2: 일반 Deployment Pod (호스트명 추론 불가 → 빈 슬롯 할당)
	fmt.Println("\n  [Case 2] 일반 Deployment Pod (이름에 번호 없음):")
	freshConfigMap2 := NewShardConfigMap()
	// 샤드 0만 활성화
	freshConfigMap2.UpdateHeartbeat(0, "argocd-app-controller-abc123")
	replicas2 := 3
	shard2, _ := getOrUpdateShardNumberForController("argocd-app-controller-xyz789", replicas2, freshConfigMap2)
	fmt.Printf("  → 할당된 샤드: %d\n", shard2)

	// Case 3: 데드 샤드 인계
	fmt.Println("\n  [Case 3] 데드 샤드 인계:")
	freshConfigMap3 := NewShardConfigMap()
	// 모든 샤드를 활성으로 등록 후 샤드 1을 데드로 만들기
	freshConfigMap3.UpdateHeartbeat(0, "controller-0")
	freshConfigMap3.UpdateHeartbeat(1, "controller-1")
	freshConfigMap3.UpdateHeartbeat(2, "controller-2")
	freshConfigMap3.mu.Lock()
	freshConfigMap3.shards[1].HeartbeatAt = time.Now().Add(-(HeartbeatTimeout + time.Second))
	freshConfigMap3.mu.Unlock()

	newShard, err := getOrUpdateShardNumberForController("new-controller-pod", replicas, freshConfigMap3)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  → 데드 샤드 %d 인계 완료\n", newShard)
	}

	// ----------------------------------------------------------------
	// 시나리오 6: 레플리카 확장/축소 시 재분배
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 6: 레플리카 확장/축소 시 클러스터 재분배")
	fmt.Println("============================")

	fmt.Println("\n  [3 → 5 레플리카 확장]:")
	assign3 := RoundRobinDistribute(clusters, 3)
	assign5 := RoundRobinDistribute(clusters, 5)

	moved := 0
	for _, c := range clusters {
		old := assign3[c.ID]
		newShard := assign5[c.ID]
		if old != newShard {
			fmt.Printf("  %s: 샤드 %d → %d (이동)\n", c.ID, old, newShard)
			moved++
		}
	}
	fmt.Printf("  이동된 클러스터 수: %d/%d\n", moved, len(clusters))

	fmt.Println("\n  [5 → 3 레플리카 축소]:")
	moved2 := 0
	for _, c := range clusters {
		old := assign5[c.ID]
		newShard := assign3[c.ID]
		if old != newShard {
			moved2++
		}
	}
	fmt.Printf("  이동된 클러스터 수: %d/%d\n", moved2, len(clusters))

	fmt.Println("\n[완료] 클러스터 샤딩 시뮬레이션 종료")
}
