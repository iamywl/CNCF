package main

import (
	"container/list"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// =============================================================================
// Loki PoC #13: Drain 패턴 알고리즘 기반 로그 패턴 자동 감지
// =============================================================================
//
// Drain 알고리즘은 Loki의 패턴 감지 엔진(pkg/pattern/drain/)에서 사용되는
// 로그 클러스터링 알고리즘이다. 비정형 로그 메시지에서 반복되는 패턴을 자동으로
// 감지하여 변수 부분을 와일드카드(<_>)로 치환한다.
//
// 핵심 개념:
// 1. 접두사 트리(Prefix Tree): 토큰 길이 → 앞부분 토큰 → 리프 노드로 탐색
// 2. 유사도 임계값(SimTh): 0.3 — 기존 클러스터와의 매칭 기준
// 3. 와일드카드 매개변수화: 매칭되지 않는 토큰은 <_>로 치환
// 4. LRU 클러스터 캐시: 최대 클러스터 수 제한 및 오래된 패턴 제거
//
// 참조: pkg/pattern/drain/drain.go, log_cluster.go, line_tokenizer.go

const (
	// ParamString 은 변수 부분을 나타내는 와일드카드 문자열
	// Loki 실제 코드: Config.ParamString = `<_>`
	ParamString = "<_>"

	// DefaultSimTh 는 유사도 임계값 (Loki 기본값: 0.3)
	// 클러스터 토큰과 입력 토큰의 일치 비율이 이 값 이상이면 매칭
	DefaultSimTh = 0.3

	// DefaultMaxNodeDepth 는 트리 탐색 최대 깊이 (LogClusterDepth - 2)
	// Loki 기본 LogClusterDepth: 30, maxNodeDepth: 28
	DefaultMaxNodeDepth = 8

	// DefaultMaxChildren 은 트리 노드의 최대 자식 수
	// Loki 기본값: 15
	DefaultMaxChildren = 15

	// DefaultMaxClusters 는 LRU 캐시의 최대 클러스터 수
	// Loki 기본값: 300
	DefaultMaxClusters = 50
)

// =============================================================================
// LogCluster: 로그 패턴 클러스터
// =============================================================================
// Loki 실제 코드: pkg/pattern/drain/log_cluster.go
// 동일한 패턴을 가진 로그 메시지들의 그룹
type LogCluster struct {
	id     int      // 고유 클러스터 ID
	Tokens []string // 패턴 토큰 (변수 부분은 <_>)
	Size   int      // 이 패턴에 매칭된 로그 수
}

// String 은 클러스터의 패턴 문자열을 반환한다
// Loki 실제 코드: LogCluster.String() → Stringer(Tokens, TokenState)
func (c *LogCluster) String() string {
	return strings.Join(c.Tokens, " ")
}

// =============================================================================
// LRU 캐시: 클러스터 관리
// =============================================================================
// Loki 실제 코드: pkg/pattern/drain/drain.go → LogClusterCache
// simplelru.LRU를 래핑하여 클러스터 ID → LogCluster 매핑을 관리
// 최대 크기 초과 시 가장 오래 접근되지 않은 클러스터를 제거(Evict)

// LRUCache 는 클러스터 ID를 키로 하는 LRU 캐시
type LRUCache struct {
	maxSize  int
	items    map[int]*list.Element // 키 → 리스트 요소 매핑
	order    *list.List            // 접근 순서 (front = 가장 최근)
	onEvict  func(int, *LogCluster)
}

type lruEntry struct {
	key     int
	cluster *LogCluster
}

// NewLRUCache 는 지정된 최대 크기의 LRU 캐시를 생성한다
func NewLRUCache(maxSize int, onEvict func(int, *LogCluster)) *LRUCache {
	if maxSize <= 0 {
		maxSize = math.MaxInt32
	}
	return &LRUCache{
		maxSize: maxSize,
		items:   make(map[int]*list.Element),
		order:   list.New(),
		onEvict: onEvict,
	}
}

// Get 은 키에 해당하는 클러스터를 반환하고 접근 순서를 갱신한다
// Loki 실제 코드: LogClusterCache.Get() → cache.Get(key)
func (c *LRUCache) Get(key int) *LogCluster {
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem) // 최근 접근으로 이동
		return elem.Value.(*lruEntry).cluster
	}
	return nil
}

// Set 은 클러스터를 캐시에 추가한다. 최대 크기 초과 시 가장 오래된 항목을 제거한다
// Loki 실제 코드: LogClusterCache.Set() → cache.Add(key, cluster)
func (c *LRUCache) Set(key int, cluster *LogCluster) {
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		elem.Value.(*lruEntry).cluster = cluster
		return
	}
	// 최대 크기 초과 시 가장 오래된 항목 제거 (Eviction)
	for c.order.Len() >= c.maxSize {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*lruEntry)
		c.order.Remove(oldest)
		delete(c.items, entry.key)
		if c.onEvict != nil {
			c.onEvict(entry.key, entry.cluster)
		}
	}
	elem := c.order.PushFront(&lruEntry{key: key, cluster: cluster})
	c.items[key] = elem
}

// Values 는 캐시에 있는 모든 클러스터를 반환한다
func (c *LRUCache) Values() []*LogCluster {
	result := make([]*LogCluster, 0, c.order.Len())
	for elem := c.order.Front(); elem != nil; elem = elem.Next() {
		result = append(result, elem.Value.(*lruEntry).cluster)
	}
	return result
}

// Len 은 캐시에 있는 클러스터 수를 반환한다
func (c *LRUCache) Len() int {
	return c.order.Len()
}

// =============================================================================
// Node: 접두사 트리 노드
// =============================================================================
// Loki 실제 코드: pkg/pattern/drain/drain.go → Node
// 트리의 각 노드는 토큰을 키로 하는 자식 노드 맵과
// 이 노드에 연결된 클러스터 ID 목록을 가진다

type Node struct {
	keyToChildNode map[string]*Node // 토큰 → 자식 노드
	clusterIDs     []int            // 이 리프에 연결된 클러스터 ID들
}

func createNode() *Node {
	return &Node{
		keyToChildNode: make(map[string]*Node),
		clusterIDs:     make([]int, 0),
	}
}

// =============================================================================
// Drain: 핵심 알고리즘 구조체
// =============================================================================
// Loki 실제 코드: pkg/pattern/drain/drain.go → Drain
//
// 알고리즘 동작 흐름:
// 1. 입력 로그를 토큰으로 분리
// 2. 접두사 트리에서 토큰 길이 → 앞부분 토큰 순서로 탐색
// 3. 리프 노드에서 유사도 기반 최적 클러스터 매칭 (fastMatch)
// 4. 매칭 성공 → 클러스터 템플릿 갱신 (다른 토큰은 <_>로)
// 5. 매칭 실패 → 새 클러스터 생성 → 트리에 추가

type DrainConfig struct {
	MaxNodeDepth int     // 트리 탐색 최대 깊이
	SimTh        float64 // 유사도 임계값
	MaxChildren  int     // 노드당 최대 자식 수
	MaxClusters  int     // 최대 클러스터 수
	ParamString  string  // 와일드카드 문자열
}

func DefaultDrainConfig() *DrainConfig {
	return &DrainConfig{
		MaxNodeDepth: DefaultMaxNodeDepth,
		SimTh:        DefaultSimTh,
		MaxChildren:  DefaultMaxChildren,
		MaxClusters:  DefaultMaxClusters,
		ParamString:  ParamString,
	}
}

type Drain struct {
	config          *DrainConfig
	rootNode        *Node          // 접두사 트리의 루트
	idToCluster     *LRUCache      // 클러스터 ID → LogCluster LRU 캐시
	clustersCounter int            // 클러스터 ID 시퀀스
	evictedCount    int            // 제거된 클러스터 수
}

// NewDrain 은 새 Drain 인스턴스를 생성한다
// Loki 실제 코드: drain.New() → Drain 초기화
func NewDrain(config *DrainConfig) *Drain {
	d := &Drain{
		config:   config,
		rootNode: createNode(),
	}
	d.idToCluster = NewLRUCache(config.MaxClusters, func(id int, cluster *LogCluster) {
		d.evictedCount++
	})
	return d
}

// Clusters 는 현재 모든 클러스터를 반환한다
func (d *Drain) Clusters() []*LogCluster {
	return d.idToCluster.Values()
}

// Train 은 로그 메시지를 학습하여 패턴 클러스터에 추가하거나 새 클러스터를 생성한다
// Loki 실제 코드: Drain.Train() → tokenize → train()
//
// 알고리즘 핵심 흐름:
// 1. 토큰화 (공백 기준)
// 2. 트리 탐색 (treeSearch)
// 3. 매칭 클러스터 있으면 → 템플릿 갱신
// 4. 없으면 → 새 클러스터 생성 → 트리에 추가
func (d *Drain) Train(content string) *LogCluster {
	tokens := tokenize(content)

	// Loki 실제 코드: 최소 4개, 최대 80개 토큰만 처리
	if len(tokens) < 4 {
		return nil
	}

	// 트리에서 유사한 클러스터 검색
	matchCluster := d.treeSearch(d.rootNode, tokens, d.config.SimTh)

	if matchCluster == nil {
		// 매칭 실패 → 새 클러스터 생성
		d.clustersCounter++
		clusterID := d.clustersCounter
		tokensCopy := make([]string, len(tokens))
		copy(tokensCopy, tokens)

		matchCluster = &LogCluster{
			id:     clusterID,
			Tokens: tokensCopy,
			Size:   1,
		}
		d.idToCluster.Set(clusterID, matchCluster)
		d.addSeqToPrefixTree(d.rootNode, matchCluster)
	} else {
		// 매칭 성공 → 템플릿 갱신 (다른 위치를 <_>로 치환)
		// Loki 실제 코드: createTemplate(tokens, matchClusterTokens)
		matchCluster.Tokens = d.createTemplate(tokens, matchCluster.Tokens)
		matchCluster.Size++
		// LRU 캐시에서 접근 순서 갱신
		d.idToCluster.Get(matchCluster.id)
	}
	return matchCluster
}

// =============================================================================
// 토큰화 (Tokenizer)
// =============================================================================
// Loki 실제 코드: pkg/pattern/drain/line_tokenizer.go
// 실제 Loki는 punctuationTokenizer, logfmtTokenizer, jsonTokenizer 등 여러 종류를 지원
// 여기서는 기본적인 공백 기반 토큰화 + 구두점 분리를 시뮬레이션

func tokenize(line string) []string {
	// 간단한 구두점 기반 토큰화
	// Loki의 punctuationTokenizer는 공백과 구두점(=, 등)을 구분자로 사용
	tokens := make([]string, 0, 32)
	start := 0
	for i, ch := range line {
		if ch == ' ' {
			if i > start {
				tokens = append(tokens, line[start:i])
			}
			start = i + 1
		}
	}
	if start < len(line) {
		tokens = append(tokens, line[start:])
	}
	return tokens
}

// =============================================================================
// 트리 탐색 (Tree Search)
// =============================================================================
// Loki 실제 코드: Drain.treeSearch()
//
// 탐색 과정:
// 1단계: rootNode → 토큰 개수로 첫 번째 레이어 선택
// 2단계: 토큰별로 트리를 깊이 탐색 (maxNodeDepth까지)
// 3단계: 리프 노드의 clusterIDs에서 fastMatch로 최적 클러스터 선택

func (d *Drain) treeSearch(rootNode *Node, tokens []string, simTh float64) *LogCluster {
	tokenCount := len(tokens)

	// 1단계: 토큰 개수로 첫 번째 레이어 노드 선택
	// Loki 실제 코드: rootNode.keyToChildNode[strconv.Itoa(tokenCount)]
	curNode, ok := rootNode.keyToChildNode[strconv.Itoa(tokenCount)]
	if !ok {
		return nil // 같은 토큰 수의 템플릿이 없음
	}

	// 빈 문자열 처리
	if tokenCount < 2 {
		if len(curNode.clusterIDs) > 0 {
			return d.idToCluster.Get(curNode.clusterIDs[0])
		}
		return nil
	}

	// 2단계: 앞부분 토큰으로 트리를 깊이 탐색
	// Loki 실제 코드: 토큰별 keyToChildNode 또는 ParamString(와일드카드) 노드로 이동
	curNodeDepth := 1
	for _, token := range tokens {
		if curNodeDepth >= d.config.MaxNodeDepth {
			break
		}
		if curNodeDepth == tokenCount {
			break
		}

		keyToChildNode := curNode.keyToChildNode
		var nextNode *Node
		nextNode, ok = keyToChildNode[token]
		if !ok {
			// 정확한 토큰이 없으면 와일드카드 노드 시도
			nextNode, ok = keyToChildNode[d.config.ParamString]
		}
		if !ok {
			return nil // 매칭 경로 없음
		}
		curNode = nextNode
		curNodeDepth++
	}

	// 3단계: 리프 노드에서 최적 클러스터 선택
	return d.fastMatch(curNode.clusterIDs, tokens, simTh)
}

// =============================================================================
// 빠른 매칭 (Fast Match)
// =============================================================================
// Loki 실제 코드: Drain.fastMatch()
//
// 후보 클러스터들 중 유사도가 가장 높은 클러스터를 반환
// 유사도 = 일치하는 토큰 수 / 전체 토큰 수

func (d *Drain) fastMatch(clusterIDs []int, tokens []string, simTh float64) *LogCluster {
	var matchCluster *LogCluster
	maxSim := -1.0
	maxParamCount := -1

	for _, clusterID := range clusterIDs {
		cluster := d.idToCluster.Get(clusterID)
		if cluster == nil {
			continue
		}
		// 유사도 계산
		curSim, paramCount := d.getSeqDistance(cluster.Tokens, tokens)
		if paramCount < 0 {
			continue
		}
		// 더 높은 유사도 또는 같은 유사도에서 더 많은 파라미터
		// Loki 실제 코드: curSim > maxSim || (curSim == maxSim && paramCount > maxParamCount)
		if curSim > maxSim || (curSim == maxSim && paramCount > maxParamCount) {
			maxSim = curSim
			maxParamCount = paramCount
			matchCluster = cluster
		}
	}

	// 임계값 이상인 경우만 매칭
	if maxSim >= simTh {
		return matchCluster
	}
	return nil
}

// =============================================================================
// 시퀀스 거리 계산 (Sequence Distance)
// =============================================================================
// Loki 실제 코드: Drain.getSeqDistance()
//
// 두 토큰 시퀀스 사이의 유사도를 계산
// - 같은 토큰이면 simTokens 증가
// - 클러스터 토큰이 <_>이면 paramCount 증가
// - 유사도 = simTokens / len(tokens)

func (d *Drain) getSeqDistance(clusterTokens, tokens []string) (float64, int) {
	if len(clusterTokens) != len(tokens) {
		return 0, -1
	}

	simTokens := 0
	paramCount := 0
	for i := range clusterTokens {
		if clusterTokens[i] == d.config.ParamString {
			paramCount++ // 이미 와일드카드인 위치
		} else if clusterTokens[i] == tokens[i] {
			simTokens++ // 정확히 일치하는 토큰
		}
	}

	retVal := float64(simTokens) / float64(len(clusterTokens))
	return retVal, paramCount
}

// =============================================================================
// 접두사 트리에 클러스터 추가
// =============================================================================
// Loki 실제 코드: Drain.addSeqToPrefixTree()
//
// 새 클러스터를 접두사 트리에 추가하는 과정:
// 1. 토큰 개수로 첫 번째 레이어 노드 생성/조회
// 2. 각 토큰별로 자식 노드 생성/탐색 (maxNodeDepth까지)
// 3. 숫자를 포함하는 토큰은 <_> 와일드카드 노드로 라우팅
// 4. 리프 노드에 클러스터 ID 추가

func (d *Drain) addSeqToPrefixTree(rootNode *Node, cluster *LogCluster) {
	tokenCount := len(cluster.Tokens)
	tokenCountStr := strconv.Itoa(tokenCount)

	// 첫 번째 레이어: 토큰 개수로 분류
	firstLayerNode, ok := rootNode.keyToChildNode[tokenCountStr]
	if !ok {
		firstLayerNode = createNode()
		rootNode.keyToChildNode[tokenCountStr] = firstLayerNode
	}
	curNode := firstLayerNode

	if tokenCount == 0 {
		curNode.clusterIDs = append(curNode.clusterIDs, cluster.id)
		return
	}

	currentDepth := 1
	for _, token := range cluster.Tokens {
		// 최대 깊이 또는 마지막 토큰에 도달 → 리프 노드에 클러스터 추가
		if currentDepth >= d.config.MaxNodeDepth || currentDepth >= tokenCount {
			curNode.clusterIDs = append(curNode.clusterIDs, cluster.id)
			break
		}

		// 토큰에 숫자가 포함되어 있으면 와일드카드 노드로 라우팅
		// Loki 실제 코드: hasNumbers() → 숫자가 있으면 ParamString 노드로
		if hasNumbers(token) {
			if _, exists := curNode.keyToChildNode[d.config.ParamString]; !exists {
				newNode := createNode()
				curNode.keyToChildNode[d.config.ParamString] = newNode
			}
			curNode = curNode.keyToChildNode[d.config.ParamString]
		} else if _, exists := curNode.keyToChildNode[token]; exists {
			// 정확한 토큰 노드가 있으면 그쪽으로
			curNode = curNode.keyToChildNode[token]
		} else {
			// 새 노드 생성 (MaxChildren 제한 확인)
			if len(curNode.keyToChildNode) < d.config.MaxChildren {
				newNode := createNode()
				curNode.keyToChildNode[token] = newNode
				curNode = newNode
			} else {
				// 자식 수 초과 → 와일드카드 노드로
				if _, exists := curNode.keyToChildNode[d.config.ParamString]; !exists {
					newNode := createNode()
					curNode.keyToChildNode[d.config.ParamString] = newNode
				}
				curNode = curNode.keyToChildNode[d.config.ParamString]
			}
		}
		currentDepth++
	}
}

// createTemplate 은 두 토큰 시퀀스를 비교하여 템플릿을 생성한다
// 다른 위치의 토큰은 <_>로 치환
// Loki 실제 코드: Drain.createTemplate()
func (d *Drain) createTemplate(tokens, matchClusterTokens []string) []string {
	for i := range tokens {
		if tokens[i] != matchClusterTokens[i] {
			matchClusterTokens[i] = d.config.ParamString
		}
	}
	return matchClusterTokens
}

// hasNumbers 는 문자열에 숫자가 포함되어 있는지 확인한다
// Loki 실제 코드: Drain.hasNumbers()
func hasNumbers(s string) bool {
	for _, c := range s {
		if unicode.IsNumber(c) {
			return true
		}
	}
	return false
}

// printTree 는 접두사 트리 구조를 시각화한다
func printTree(node *Node, prefix string, depth int) {
	for key, child := range node.keyToChildNode {
		fmt.Printf("%s[깊이 %d] 토큰: %-20s | 자식: %d | 클러스터ID: %v\n",
			prefix, depth, key, len(child.keyToChildNode), child.clusterIDs)
		printTree(child, prefix+"  ", depth+1)
	}
}

// =============================================================================
// 메인 함수: Drain 알고리즘 시뮬레이션
// =============================================================================

func main() {
	fmt.Println("=== Loki Drain 패턴 알고리즘 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 1단계: Drain 인스턴스 생성
	// ─────────────────────────────────────────────────────────────
	config := DefaultDrainConfig()
	drain := NewDrain(config)

	fmt.Println("--- [1] Drain 설정 ---")
	fmt.Printf("  유사도 임계값 (SimTh):    %.1f\n", config.SimTh)
	fmt.Printf("  최대 트리 깊이:           %d\n", config.MaxNodeDepth)
	fmt.Printf("  최대 자식 수:             %d\n", config.MaxChildren)
	fmt.Printf("  최대 클러스터 수 (LRU):   %d\n", config.MaxClusters)
	fmt.Printf("  와일드카드 문자열:        %s\n", config.ParamString)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 2단계: 로그 메시지 학습 (Training)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [2] 로그 메시지 학습 ---")
	fmt.Println()

	// 다양한 패턴의 로그 메시지
	logs := []string{
		// 패턴 A: HTTP 요청 로그 (IP, 상태코드, 시간이 변수)
		"2024-01-15 10:23:45 INFO request from 192.168.1.100 status 200 duration 45ms",
		"2024-01-15 10:23:46 INFO request from 10.0.0.50 status 200 duration 120ms",
		"2024-01-15 10:23:47 INFO request from 172.16.0.1 status 201 duration 89ms",
		"2024-01-15 10:23:48 INFO request from 192.168.1.200 status 200 duration 33ms",

		// 패턴 B: 에러 로그 (시간, 에러 ID가 변수)
		"2024-01-15 10:24:00 ERROR connection timeout to database host db-primary-01 retry 1",
		"2024-01-15 10:24:05 ERROR connection timeout to database host db-primary-02 retry 2",
		"2024-01-15 10:24:10 ERROR connection timeout to database host db-replica-03 retry 3",

		// 패턴 C: 사용자 활동 로그 (사용자 ID, 동작이 변수)
		"user 12345 logged in from browser Chrome on platform Linux",
		"user 67890 logged in from browser Firefox on platform macOS",
		"user 11111 logged in from browser Safari on platform Windows",

		// 패턴 D: 메트릭 로그 (수치가 변수)
		"metric cpu_usage value 78.5 host worker-node-01 region us-east-1",
		"metric cpu_usage value 92.3 host worker-node-02 region us-west-2",
		"metric memory_usage value 65.1 host worker-node-01 region us-east-1",
	}

	for i, logLine := range logs {
		cluster := drain.Train(logLine)
		if cluster != nil {
			matchType := "기존 패턴 매칭"
			if cluster.Size == 1 {
				matchType = "새 패턴 생성"
			}
			fmt.Printf("  [로그 %2d] %s\n", i+1, matchType)
			fmt.Printf("           입력:  %s\n", logLine)
			fmt.Printf("           패턴:  %s\n", cluster.String())
			fmt.Printf("           매칭수: %d (클러스터 ID: %d)\n", cluster.Size, cluster.id)
			fmt.Println()
		}
	}

	// ─────────────────────────────────────────────────────────────
	// 3단계: 감지된 패턴 요약
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [3] 감지된 패턴 요약 ---")
	fmt.Println()

	clusters := drain.Clusters()
	fmt.Printf("  총 클러스터 수: %d\n", len(clusters))
	fmt.Printf("  제거된 클러스터 수: %d\n", drain.evictedCount)
	fmt.Println()

	for _, cluster := range clusters {
		fmt.Printf("  [클러스터 %d] 매칭 수: %d\n", cluster.id, cluster.Size)
		fmt.Printf("    패턴: %s\n", cluster.String())
		fmt.Println()
	}

	// ─────────────────────────────────────────────────────────────
	// 4단계: 접두사 트리 구조 시각화
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [4] 접두사 트리 구조 ---")
	fmt.Println()
	fmt.Println("  루트 노드:")
	printTree(drain.rootNode, "    ", 0)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 5단계: 유사도 계산 데모
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [5] 유사도 계산 데모 ---")
	fmt.Println()

	// 유사도 계산 예시
	pattern := []string{"INFO", "request", "from", "<_>", "status", "<_>", "duration", "<_>"}
	testCases := [][]string{
		{"INFO", "request", "from", "10.0.0.1", "status", "200", "duration", "55ms"},
		{"INFO", "request", "from", "10.0.0.2", "status", "500", "duration", "100ms"},
		{"ERROR", "request", "from", "10.0.0.3", "status", "503", "duration", "5000ms"},
		{"INFO", "response", "to", "10.0.0.4", "code", "200", "latency", "50ms"},
	}

	fmt.Printf("  패턴:    %s\n", strings.Join(pattern, " "))
	fmt.Printf("  임계값:  %.1f\n", config.SimTh)
	fmt.Println()

	for _, tc := range testCases {
		sim, params := drain.getSeqDistance(pattern, tc)
		matched := sim >= config.SimTh
		matchStr := "매칭 실패"
		if matched {
			matchStr = "매칭 성공"
		}
		fmt.Printf("  입력:    %s\n", strings.Join(tc, " "))
		fmt.Printf("  유사도:  %.3f | 파라미터: %d | 결과: %s\n", sim, params, matchStr)
		fmt.Println()
	}

	// ─────────────────────────────────────────────────────────────
	// 6단계: LRU 제거(Eviction) 시뮬레이션
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [6] LRU 제거 시뮬레이션 ---")
	fmt.Println()

	// 작은 캐시로 테스트
	smallConfig := DefaultDrainConfig()
	smallConfig.MaxClusters = 5
	smallDrain := NewDrain(smallConfig)

	// 서로 다른 패턴의 로그를 많이 생성하여 LRU 제거 유도
	for i := 0; i < 10; i++ {
		// 각각 다른 개수의 토큰을 가진 유니크 로그 생성
		parts := []string{"log", "entry", "type", fmt.Sprintf("pattern-%d", i)}
		for j := 0; j < i; j++ {
			parts = append(parts, fmt.Sprintf("extra-field-%d", j))
		}
		logLine := strings.Join(parts, " ")
		smallDrain.Train(logLine)
	}

	fmt.Printf("  최대 클러스터 수: %d\n", smallConfig.MaxClusters)
	fmt.Printf("  학습한 고유 패턴 수: 10\n")
	fmt.Printf("  남아있는 클러스터 수: %d\n", smallDrain.idToCluster.Len())
	fmt.Printf("  제거된 클러스터 수: %d\n", smallDrain.evictedCount)
	fmt.Println()

	fmt.Println("  남아있는 클러스터:")
	for _, cluster := range smallDrain.Clusters() {
		fmt.Printf("    [ID %d] %s (매칭: %d)\n", cluster.id, cluster.String(), cluster.Size)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 동작 원리 요약
	// ─────────────────────────────────────────────────────────────
	fmt.Println("=== Drain 알고리즘 동작 원리 요약 ===")
	fmt.Println()
	fmt.Println("  1. 토큰화: 로그 메시지를 토큰 단위로 분리")
	fmt.Println("  2. 트리 탐색: 토큰 수 → 앞부분 토큰 → 리프 노드")
	fmt.Println("  3. 유사도 매칭: SimTh(0.3) 이상이면 기존 클러스터에 추가")
	fmt.Println("  4. 템플릿 갱신: 일치하지 않는 토큰을 <_>로 치환")
	fmt.Println("  5. LRU 관리: 최대 클러스터 수 초과 시 가장 오래된 패턴 제거")
	fmt.Println()
	fmt.Println("  Loki 핵심 코드 경로:")
	fmt.Println("  - pkg/pattern/drain/drain.go       → Drain 구조체, Train, treeSearch")
	fmt.Println("  - pkg/pattern/drain/log_cluster.go  → LogCluster, 패턴 문자열화")
	fmt.Println("  - pkg/pattern/drain/line_tokenizer.go → 토큰화 전략")
}
