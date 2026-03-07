package main

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
)

// =============================================================================
// Consistent Hash Ring 시뮬레이션
// =============================================================================
//
// Loki는 dskit/ring 패키지를 사용하여 Ingester 인스턴스 간에 로그 스트림을
// 분산한다. Consistent Hashing은 노드가 추가/제거될 때 최소한의 키 재배치만
// 발생하도록 보장하는 분산 해싱 기법이다.
//
// 핵심 원리:
//   1. 해시 링: 0 ~ 2^32-1 범위의 원형 해시 공간
//   2. 가상 노드(토큰): 각 물리 노드가 여러 개의 토큰을 링에 배치하여 균등 분산
//   3. 키 라우팅: 키를 해시한 후 시계 방향으로 가장 가까운 토큰의 노드에 할당
//   4. 복제 인자: 시계 방향으로 N개의 서로 다른 노드에 복제
//   5. 노드 추가/제거 시 영향받는 키가 최소화됨
//
// Loki 실제 구현 참조:
//   - dskit/ring/ring.go: Ring 구조체, Get() 메서드
//   - dskit/ring/model.go: InstanceDesc (노드 정보)
//   - dskit/ring/tokens.go: 토큰 관리
//   - pkg/distributor/distributor.go: Ring을 이용한 스트림 라우팅
// =============================================================================

// Token은 해시 링 위의 하나의 지점을 나타낸다.
// 각 물리 노드는 여러 개의 토큰을 가지며, 이를 통해 링 위에 균등하게 분산된다.
type Token struct {
	Hash     uint32 // 링 위의 위치 (0 ~ 2^32-1)
	NodeID   string // 이 토큰이 속한 물리 노드
	VNodeIdx int    // 가상 노드 인덱스
}

// Node는 링에 참여하는 물리 노드(예: Ingester 인스턴스)를 나타낸다.
type Node struct {
	ID     string   // 노드 식별자 (예: "ingester-1")
	Addr   string   // 네트워크 주소
	State  string   // 상태: ACTIVE, LEAVING, JOINING
	Tokens []uint32 // 이 노드가 소유한 토큰 목록
}

// Ring은 Consistent Hash Ring을 구현한다.
// dskit/ring/ring.go의 Ring 구조체와 동일한 역할이다.
type Ring struct {
	tokens          []Token          // 정렬된 토큰 목록
	nodes           map[string]*Node // 노드 ID → Node 매핑
	virtualNodes    int              // 물리 노드당 가상 노드 수
	replicationFactor int            // 복제 인자
}

// NewRing은 새 Ring을 생성한다.
func NewRing(virtualNodes, replicationFactor int) *Ring {
	return &Ring{
		nodes:             make(map[string]*Node),
		virtualNodes:      virtualNodes,
		replicationFactor: replicationFactor,
	}
}

// hashKey는 문자열을 uint32 해시값으로 변환한다.
// Loki는 실제로 FNV32a를 사용하지만, 여기서는 MD5의 앞 4바이트를 사용한다.
func hashKey(key string) uint32 {
	h := md5.Sum([]byte(key))
	return binary.BigEndian.Uint32(h[:4])
}

// AddNode는 노드를 링에 추가한다.
// 가상 노드(토큰)를 생성하여 링에 배치한다.
func (r *Ring) AddNode(id, addr string) {
	node := &Node{
		ID:    id,
		Addr:  addr,
		State: "ACTIVE",
	}

	// 가상 노드 생성: 노드 ID와 인덱스를 조합하여 해시
	for i := 0; i < r.virtualNodes; i++ {
		tokenKey := fmt.Sprintf("%s-vnode-%d", id, i)
		hash := hashKey(tokenKey)
		node.Tokens = append(node.Tokens, hash)
		r.tokens = append(r.tokens, Token{
			Hash:     hash,
			NodeID:   id,
			VNodeIdx: i,
		})
	}

	r.nodes[id] = node

	// 토큰을 해시값 기준으로 정렬 — 링 위에서 빠른 검색을 위해
	sort.Slice(r.tokens, func(i, j int) bool {
		return r.tokens[i].Hash < r.tokens[j].Hash
	})
}

// RemoveNode는 노드를 링에서 제거한다.
func (r *Ring) RemoveNode(id string) {
	delete(r.nodes, id)

	// 해당 노드의 토큰 제거
	filtered := make([]Token, 0, len(r.tokens))
	for _, t := range r.tokens {
		if t.NodeID != id {
			filtered = append(filtered, t)
		}
	}
	r.tokens = filtered
}

// Get은 주어진 키에 대해 담당 노드를 찾는다.
// 키를 해시한 후 시계 방향으로 가장 가까운 토큰을 찾아 해당 노드를 반환한다.
// dskit/ring/ring.go의 Ring.Get() 메서드와 동일한 로직이다.
func (r *Ring) Get(key string) *Node {
	if len(r.tokens) == 0 {
		return nil
	}

	hash := hashKey(key)

	// 이진 검색으로 시계 방향 첫 번째 토큰 찾기
	idx := sort.Search(len(r.tokens), func(i int) bool {
		return r.tokens[i].Hash >= hash
	})

	// 링의 끝을 넘으면 처음으로 돌아감 (원형 구조)
	if idx >= len(r.tokens) {
		idx = 0
	}

	nodeID := r.tokens[idx].NodeID
	return r.nodes[nodeID]
}

// GetReplicaNodes는 복제 인자에 따라 여러 노드를 반환한다.
// 시계 방향으로 순회하면서 서로 다른 물리 노드를 replicationFactor 개만큼 수집한다.
// dskit/ring/ring.go의 Ring.Get()에서 ReplicationSet을 구성하는 로직이다.
func (r *Ring) GetReplicaNodes(key string) []*Node {
	if len(r.tokens) == 0 {
		return nil
	}

	hash := hashKey(key)

	// 시작 인덱스 찾기
	startIdx := sort.Search(len(r.tokens), func(i int) bool {
		return r.tokens[i].Hash >= hash
	})
	if startIdx >= len(r.tokens) {
		startIdx = 0
	}

	// 서로 다른 물리 노드를 replicationFactor 개만큼 수집
	seen := make(map[string]bool)
	var result []*Node

	for i := 0; i < len(r.tokens) && len(result) < r.replicationFactor; i++ {
		idx := (startIdx + i) % len(r.tokens)
		nodeID := r.tokens[idx].NodeID
		if !seen[nodeID] {
			seen[nodeID] = true
			result = append(result, r.nodes[nodeID])
		}
	}

	return result
}

// PrintRing은 링의 상태를 시각적으로 출력한다.
func (r *Ring) PrintRing() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    해시 링 상태                                   ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  노드 수: %d, 토큰 수: %d, 복제 인자: %d                           ║\n",
		len(r.nodes), len(r.tokens), r.replicationFactor)
	fmt.Println("╠══════════════════════════════════════════════════════════════════╣")

	for id, node := range r.nodes {
		fmt.Printf("║  [%s] 상태: %-8s 토큰 수: %-3d 주소: %-20s  ║\n",
			id, node.State, len(node.Tokens), node.Addr)
	}
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
}

// AnalyzeDistribution은 키를 분산시켜 각 노드의 부하를 분석한다.
func (r *Ring) AnalyzeDistribution(numKeys int) map[string]int {
	dist := make(map[string]int)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("stream-%d", i)
		node := r.Get(key)
		if node != nil {
			dist[node.ID]++
		}
	}
	return dist
}

// visualizeRing은 링을 ASCII 원형으로 시각화한다.
func (r *Ring) visualizeRing() {
	fmt.Println()
	fmt.Println("  해시 링 시각화 (토큰 위치, 0° = 12시 방향):")
	fmt.Println()

	// 링을 36분할하여 각 섹터에 어떤 노드가 있는지 표시
	const sectors = 36
	sectorSize := float64(math.MaxUint32+1) / float64(sectors)
	sectorNodes := make([]string, sectors)

	for _, t := range r.tokens {
		sector := int(float64(t.Hash) / sectorSize)
		if sector >= sectors {
			sector = sectors - 1
		}
		if sectorNodes[sector] == "" {
			// 노드 ID의 마지막 문자를 사용
			parts := strings.Split(t.NodeID, "-")
			sectorNodes[sector] = parts[len(parts)-1]
		}
	}

	// 간단한 원형 표시
	radius := 8
	centerX, centerY := 20, radius+1

	grid := make([][]rune, centerY*2+2)
	for i := range grid {
		grid[i] = make([]rune, centerX*2+10)
		for j := range grid[i] {
			grid[i][j] = ' '
		}
	}

	for s := 0; s < sectors; s++ {
		angle := float64(s) * 2 * math.Pi / float64(sectors)
		// 12시 방향부터 시계 방향 (Y축 반전)
		x := centerX + int(math.Round(float64(radius)*math.Sin(angle)))
		y := centerY - int(math.Round(float64(radius)*math.Cos(angle)*0.5))

		if y >= 0 && y < len(grid) && x >= 0 && x < len(grid[0]) {
			if sectorNodes[s] != "" {
				ch := rune(sectorNodes[s][0])
				grid[y][x] = ch
			} else {
				grid[y][x] = '·'
			}
		}
	}

	for _, row := range grid {
		line := strings.TrimRight(string(row), " ")
		if line != "" {
			fmt.Printf("  %s\n", line)
		}
	}

	// 범례 출력
	fmt.Println()
	fmt.Print("  범례: ")
	for id := range r.nodes {
		parts := strings.Split(id, "-")
		fmt.Printf("[%s]=%s  ", parts[len(parts)-1], id)
	}
	fmt.Println()
}

func main() {
	fmt.Println("=================================================================")
	fmt.Println("  Loki Consistent Hash Ring 시뮬레이션")
	fmt.Println("  - 가상 노드(토큰) 기반 일관된 해싱")
	fmt.Println("  - 복제 인자에 따른 다중 노드 라우팅")
	fmt.Println("  - 노드 추가/제거 시 최소 재분배")
	fmt.Println("=================================================================")
	fmt.Println()

	// =========================================================================
	// 시나리오 1: 기본 링 구성 및 키 라우팅
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 1: 기본 링 구성 (3 노드, 가상 노드 128개, 복제 인자 3)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	ring := NewRing(128, 3)

	// 3개의 Ingester 노드 추가
	ring.AddNode("ingester-1", "10.0.0.1:9095")
	ring.AddNode("ingester-2", "10.0.0.2:9095")
	ring.AddNode("ingester-3", "10.0.0.3:9095")

	ring.PrintRing()
	ring.visualizeRing()
	fmt.Println()

	// 키 분산 분석
	numKeys := 10000
	dist := ring.AnalyzeDistribution(numKeys)
	fmt.Printf("  키 분산 분석 (%d개 키):\n", numKeys)
	idealPct := 100.0 / float64(len(ring.nodes))
	for id, count := range dist {
		pct := float64(count) / float64(numKeys) * 100
		deviation := pct - idealPct
		bar := strings.Repeat("█", int(pct/2))
		fmt.Printf("    %-12s: %5d (%5.1f%%) [편차: %+5.1f%%] %s\n",
			id, count, pct, deviation, bar)
	}

	// =========================================================================
	// 시나리오 2: 스트림 라우팅 및 복제
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 2: 스트림 라우팅 및 복제")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// Loki에서 스트림 키는 테넌트 ID + 레이블 세트의 해시로 구성된다
	streams := []string{
		`{tenant="team-a", app="nginx", level="error"}`,
		`{tenant="team-a", app="api", level="info"}`,
		`{tenant="team-b", app="worker", level="warn"}`,
		`{tenant="team-b", app="db", level="error"}`,
		`{tenant="team-c", app="frontend", level="debug"}`,
	}

	fmt.Println("  스트림 → 담당 노드 (복제 인자=3):")
	fmt.Println()
	for _, stream := range streams {
		hash := hashKey(stream)
		replicas := ring.GetReplicaNodes(stream)
		nodeNames := make([]string, len(replicas))
		for i, n := range replicas {
			nodeNames[i] = n.ID
		}
		fmt.Printf("    해시: 0x%08X  스트림: %s\n", hash, stream)
		fmt.Printf("                      → 복제 노드: [%s]\n", strings.Join(nodeNames, ", "))
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 3: 노드 추가 시 재분배 분석
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 3: 노드 추가 시 재분배 분석")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 노드 추가 전 각 키의 담당 노드 기록
	beforeMap := make(map[string]string)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("stream-%d", i)
		node := ring.Get(key)
		beforeMap[key] = node.ID
	}

	// 새 노드 추가
	fmt.Println("  새 노드 추가: ingester-4 (10.0.0.4:9095)")
	ring.AddNode("ingester-4", "10.0.0.4:9095")

	// 노드 추가 후 재분배 분석
	moved := 0
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("stream-%d", i)
		node := ring.Get(key)
		if beforeMap[key] != node.ID {
			moved++
		}
	}

	fmt.Printf("  전체 키: %d, 재배치된 키: %d (%.1f%%)\n",
		numKeys, moved, float64(moved)/float64(numKeys)*100)
	fmt.Printf("  이상적인 재배치율: %.1f%% (1/N, N=%d)\n",
		100.0/float64(len(ring.nodes)), len(ring.nodes))
	fmt.Println()

	// 새로운 분산 분석
	dist2 := ring.AnalyzeDistribution(numKeys)
	fmt.Printf("  노드 추가 후 키 분산 (%d개 키):\n", numKeys)
	idealPct2 := 100.0 / float64(len(ring.nodes))
	for id, count := range dist2 {
		pct := float64(count) / float64(numKeys) * 100
		deviation := pct - idealPct2
		bar := strings.Repeat("█", int(pct/2))
		fmt.Printf("    %-12s: %5d (%5.1f%%) [편차: %+5.1f%%] %s\n",
			id, count, pct, deviation, bar)
	}

	// =========================================================================
	// 시나리오 4: 노드 제거 시 재분배 분석
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 4: 노드 제거 시 재분배 분석")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 노드 제거 전 각 키의 담당 노드 기록
	beforeMap2 := make(map[string]string)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("stream-%d", i)
		node := ring.Get(key)
		beforeMap2[key] = node.ID
	}

	// 노드 제거
	fmt.Println("  노드 제거: ingester-2")
	ring.RemoveNode("ingester-2")

	// 재분배 분석
	moved2 := 0
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("stream-%d", i)
		node := ring.Get(key)
		if beforeMap2[key] != node.ID {
			moved2++
		}
	}

	fmt.Printf("  전체 키: %d, 재배치된 키: %d (%.1f%%)\n",
		numKeys, moved2, float64(moved2)/float64(numKeys)*100)
	fmt.Println()

	dist3 := ring.AnalyzeDistribution(numKeys)
	fmt.Printf("  노드 제거 후 키 분산 (%d개 키):\n", numKeys)
	idealPct3 := 100.0 / float64(len(ring.nodes))
	for id, count := range dist3 {
		pct := float64(count) / float64(numKeys) * 100
		deviation := pct - idealPct3
		bar := strings.Repeat("█", int(pct/2))
		fmt.Printf("    %-12s: %5d (%5.1f%%) [편차: %+5.1f%%] %s\n",
			id, count, pct, deviation, bar)
	}

	// =========================================================================
	// 시나리오 5: 가상 노드 수에 따른 분산 균등도 비교
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 5: 가상 노드 수에 따른 분산 균등도 비교")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	vnodeCounts := []int{1, 4, 16, 64, 128, 256, 512}
	ideal := float64(numKeys) / 3.0

	fmt.Printf("  %-12s  %-12s  %-12s  %s\n", "가상노드 수", "최소 키 수", "최대 키 수", "표준편차")
	fmt.Println("  " + strings.Repeat("─", 56))

	for _, vn := range vnodeCounts {
		testRing := NewRing(vn, 3)
		testRing.AddNode("node-A", "10.0.0.1:9095")
		testRing.AddNode("node-B", "10.0.0.2:9095")
		testRing.AddNode("node-C", "10.0.0.3:9095")

		d := testRing.AnalyzeDistribution(numKeys)
		minVal, maxVal := numKeys, 0
		sumSqDiff := 0.0
		for _, count := range d {
			if count < minVal {
				minVal = count
			}
			if count > maxVal {
				maxVal = count
			}
			diff := float64(count) - ideal
			sumSqDiff += diff * diff
		}
		stddev := math.Sqrt(sumSqDiff / 3.0)

		fmt.Printf("  %-14d %-14d %-14d %.1f\n", vn, minVal, maxVal, stddev)
	}

	fmt.Println()
	fmt.Println("=================================================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("  1. 가상 노드가 많을수록 키 분산이 균등해짐")
	fmt.Println("  2. 노드 추가/제거 시 약 1/N의 키만 재배치됨")
	fmt.Println("  3. 복제 인자만큼 시계 방향으로 다른 노드에 복제")
	fmt.Println("  4. 이진 검색으로 O(log T) 시간에 담당 노드를 찾음 (T=토큰 수)")
	fmt.Println("  5. Loki의 Distributor가 이 방식으로 스트림을 Ingester에 분배")
	fmt.Println("=================================================================")
}
