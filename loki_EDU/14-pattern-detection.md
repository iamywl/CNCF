# 14. 로그 패턴 감지 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Drain 알고리즘 이론](#2-drain-알고리즘-이론)
3. [Drain 구현: Config, Node, LogClusterCache](#3-drain-구현-config-node-logclustercache)
4. [트리 구조: 깊이 기반 매칭](#4-트리-구조-깊이-기반-매칭)
5. [유사도 계산과 클러스터 관리](#5-유사도-계산과-클러스터-관리)
6. [Line Tokenizer: 로그 형식 적응](#6-line-tokenizer-로그-형식-적응)
7. [Limiter: 과부하 방지](#7-limiter-과부하-방지)
8. [Pattern Ingester](#8-pattern-ingester)
9. [Pattern Stream: 로그 레벨별 Drain 인스턴스](#9-pattern-stream-로그-레벨별-drain-인스턴스)
10. [패턴 청크: 시간 기반 샘플링](#10-패턴-청크-시간-기반-샘플링)
11. [LogCluster와 Iterator](#11-logcluster와-iterator)
12. [볼륨 임계값 기반 필터링](#12-볼륨-임계값-기반-필터링)
13. [메트릭 생성과 모니터링](#13-메트릭-생성과-모니터링)
14. [활용 사례](#14-활용-사례)
15. [설계 결정 분석](#15-설계-결정-분석)

---

## 1. 개요

Loki의 로그 패턴 감지 시스템은 Drain 알고리즘을 기반으로 비정형 로그 메시지에서 반복적인 패턴(템플릿)을 자동으로 추출하는 기능이다. 이를 통해 로그의 구조적 이해, 이상 탐지, 로그 볼륨 분석을 지원한다.

```
소스 위치:
- pkg/pattern/drain/drain.go          -- Drain 알고리즘 핵심 구현
- pkg/pattern/drain/log_cluster.go    -- LogCluster 구조체
- pkg/pattern/drain/chunk.go          -- 패턴 청크, 시간 샘플링
- pkg/pattern/drain/line_tokenizer.go -- LineTokenizer (JSON, Logfmt, Punct)
- pkg/pattern/drain/limiter.go        -- 과부하 방지 Limiter
- pkg/pattern/drain/metrics.go        -- 메트릭, 로그 형식 감지
- pkg/pattern/ingester.go             -- Pattern Ingester Config, 라이프사이클
- pkg/pattern/stream.go               -- Pattern Stream (레벨별 Drain)
```

### 패턴 감지 예시

```
입력 로그 라인들:
  "2024-01-15 10:23:45 INFO user john logged in from 192.168.1.1"
  "2024-01-15 10:24:12 INFO user alice logged in from 10.0.0.5"
  "2024-01-15 10:25:33 INFO user bob logged in from 172.16.0.1"
  "2024-01-15 10:26:01 ERROR connection to db-primary timed out after 30s"
  "2024-01-15 10:26:45 ERROR connection to db-replica timed out after 15s"

감지된 패턴:
  패턴 1: "<_> <_> INFO user <_> logged in from <_>"        (count: 3)
  패턴 2: "<_> <_> ERROR connection to <_> timed out after <_>" (count: 2)

<_> = 가변 파라미터 (와일드카드)
```

### 아키텍처 개요

```
┌───────────────────────────────────────────────────────────┐
│                     Loki Distributor                       │
│  (로그 수집)                                               │
└──────────────────────┬────────────────────────────────────┘
                       │ 로그 Tee (분기)
                       │
          ┌────────────┴────────────┐
          │                         │
          ▼                         ▼
┌─────────────────┐       ┌──────────────────────┐
│  일반 Ingester  │       │  Pattern Ingester    │
│  (로그 저장)    │       │                      │
│                 │       │  ┌──────────────────┐ │
│                 │       │  │ Stream 1         │ │
│                 │       │  │ ├─ Drain(info)   │ │
│                 │       │  │ ├─ Drain(error)  │ │
│                 │       │  │ └─ Drain(warn)   │ │
│                 │       │  ├──────────────────┤ │
│                 │       │  │ Stream 2         │ │
│                 │       │  │ ├─ Drain(info)   │ │
│                 │       │  │ └─ ...           │ │
│                 │       │  └──────────────────┘ │
└─────────────────┘       └──────────────────────┘
```

---

## 2. Drain 알고리즘 이론

### 논문 배경

Drain은 2017년 Pinjia He 등이 발표한 "Drain: An Online Log Parsing Approach with Fixed Depth Tree"(IEEE ICWS 2017) 논문에서 제안한 알고리즘이다.

논문 참조: https://jiemingzhu.github.io/pub/pjhe_icws2017.pdf

### 핵심 아이디어

1. **고정 깊이 트리**: 로그 메시지의 첫 N개 토큰을 기반으로 고정 깊이 파싱 트리를 구성한다.
2. **온라인 학습**: 로그가 도착할 때마다 실시간으로 패턴을 갱신한다.
3. **유사도 기반 클러스터링**: 트리의 리프 노드에서 유사도 임계값을 사용하여 기존 클러스터에 매칭하거나 새 클러스터를 생성한다.

### Drain 알고리즘 3단계

```
입력: 새 로그 메시지

1단계: 토큰 수에 의한 사전 필터링
─────────────────────────────────────
   로그를 토큰화하여 토큰 수를 계산한다.
   같은 패턴의 로그는 같은 수의 토큰을 가진다고 가정한다.

   Root
   ├── "5 tokens" → ...
   ├── "8 tokens" → ...
   └── "12 tokens" → ...

2단계: 접두사 트리 탐색
─────────────────────────────────────
   첫 N개 토큰(LogClusterDepth)을 사용하여
   트리를 탐색한다. 로그의 시작 토큰은
   상수일 가능성이 높다는 가정에 기반한다.

   "8 tokens"
   ├── "INFO" → ...
   │   ├── "user" → [Cluster List]
   │   └── "request" → [Cluster List]
   └── "ERROR" → ...
       └── "connection" → [Cluster List]

3단계: 유사도 기반 클러스터 매칭
─────────────────────────────────────
   리프 노드의 클러스터 목록에서
   SimTh(유사도 임계값) 이상인 클러스터를 찾는다.

   매칭 있음 → 기존 클러스터에 추가, 템플릿 갱신
   매칭 없음 → 새 클러스터 생성
```

---

## 3. Drain 구현: Config, Node, LogClusterCache

### Config 구조체

```go
// 소스: pkg/pattern/drain/drain.go:38-50
type Config struct {
    maxNodeDepth         int
    LogClusterDepth      int
    SimTh                float64
    MaxChildren          int
    ExtraDelimiters      []string
    MaxClusters          int
    ParamString          string
    MaxEvictionRatio     float64
    MaxAllowedLineLength int
    MaxChunkAge          time.Duration
    SampleInterval       time.Duration
}
```

### 기본 설정

```go
// 소스: pkg/pattern/drain/drain.go:98-144
func DefaultConfig() *Config {
    return &Config{
        LogClusterDepth:      30,          // 트리 깊이 (접두사 매칭 토큰 수)
        SimTh:                0.3,         // 유사도 임계값 (30%)
        MaxChildren:          15,          // 노드당 최대 자식 수
        ParamString:          `<_>`,       // 와일드카드 문자열
        MaxClusters:          300,         // 최대 클러스터 수
        MaxEvictionRatio:     0.25,        // 최대 퇴거 비율 (25%)
        MaxAllowedLineLength: 3000,        // 최대 허용 라인 길이
        MaxChunkAge:          time.Hour,   // 최대 청크 지속 시간
        SampleInterval:       10 * time.Second,  // 샘플 간격
    }
}
```

### 설정 상세 설명

| 설정 | 기본값 | 역할 |
|------|--------|------|
| `LogClusterDepth` | 30 | 트리의 최대 깊이. 로그 시작 부분의 상수 토큰 수를 결정 |
| `SimTh` | 0.3 | 유사도 임계값. 낮을수록 더 관대한 매칭 (더 적은 패턴) |
| `MaxChildren` | 15 | 트리 노드당 최대 자식 수. 메모리 사용량 제어 |
| `ParamString` | `<_>` | 가변 파라미터를 나타내는 와일드카드 문자열 |
| `MaxClusters` | 300 | LRU 캐시 크기. 초과 시 가장 오래된 클러스터 퇴거 |
| `MaxEvictionRatio` | 0.25 | 퇴거 비율 25% 초과 시 새 학습 일시 중단 |
| `MaxAllowedLineLength` | 3000 | 3000자 초과 라인은 무시 |

### Node 구조체

```go
// 소스: pkg/pattern/drain/drain.go:93-96
type Node struct {
    keyToChildNode map[string]*Node
    clusterIDs     []int
}

func createNode() *Node {
    return &Node{
        keyToChildNode: make(map[string]*Node),
        clusterIDs:     make([]int, 0),
    }
}
```

### LogClusterCache (LRU 캐시)

```go
// 소스: pkg/pattern/drain/drain.go:56-64
func createLogClusterCache(maxSize int, onEvict func(int, *LogCluster)) *LogClusterCache {
    if maxSize == 0 {
        maxSize = math.MaxInt
    }
    cache, _ := simplelru.NewLRU[int, *LogCluster](maxSize, onEvict)
    return &LogClusterCache{
        cache: cache,
    }
}

type LogClusterCache struct {
    cache simplelru.LRUCache[int, *LogCluster]
}
```

### Drain 구조체

```go
// 소스: pkg/pattern/drain/drain.go:192-204
type Drain struct {
    config          *Config
    rootNode        *Node
    idToCluster     *LogClusterCache
    clustersCounter int
    metrics         *Metrics
    tokenizer       LineTokenizer
    format          string
    tokens          []string
    state           interface{}
    limiter         *limiter
    pruning         bool
}
```

### Drain 생성자

```go
// 소스: pkg/pattern/drain/drain.go:146-190
func New(tenantID string, config *Config, limits Limits, format string, metrics *Metrics) *Drain {
    if config.LogClusterDepth < 3 {
        panic("depth argument must be at least 3")
    }
    config.maxNodeDepth = config.LogClusterDepth - 2

    d := &Drain{
        config:   config,
        rootNode: createNode(),
        metrics:  metrics,
        format:   format,
    }

    limiter := newLimiter(config.MaxEvictionRatio)

    // 로그 형식에 따른 토크나이저 선택
    var tokenizer LineTokenizer
    switch format {
    case FormatJSON:
        tokenizer = newJSONTokenizer(config.ParamString, config.MaxAllowedLineLength, fieldsToTokenize)
    case FormatLogfmt:
        tokenizer = newLogfmtTokenizer(config.ParamString, config.MaxAllowedLineLength)
    default:
        tokenizer = newPunctuationTokenizer(config.MaxAllowedLineLength)
    }

    // LRU 캐시 생성 (퇴거 콜백 포함)
    d.idToCluster = createLogClusterCache(config.MaxClusters, func(int, *LogCluster) {
        if metrics != nil {
            if d.pruning {
                metrics.PatternsPrunedTotal.Inc()
            } else {
                metrics.PatternsEvictedTotal.Inc()
            }
        }
        if !d.pruning {
            limiter.Evict()
        }
    })

    d.tokenizer = &DedupingTokenizer{
        LineTokenizer: tokenizer,
        dedupParam:    config.ParamString,
    }
    d.limiter = limiter
    return d
}
```

---

## 4. 트리 구조: 깊이 기반 매칭

### 트리 구조 시각화

```
Drain 파싱 트리:

Root Node
├── "5" (토큰 수 = 5)
│   ├── "INFO" (첫 번째 토큰)
│   │   ├── "user" (두 번째 토큰)
│   │   │   └── [ClusterIDs: 1, 3, 7]
│   │   └── "<_>" (와일드카드)
│   │       └── [ClusterIDs: 5]
│   └── "ERROR"
│       ├── "connection"
│       │   └── [ClusterIDs: 2, 4]
│       └── "<_>"
│           └── [ClusterIDs: 8]
├── "8" (토큰 수 = 8)
│   └── ...
└── "12" (토큰 수 = 12)
    └── ...

1단계: 토큰 수로 분기 (Root → "5")
2단계: 첫 토큰으로 분기 ("5" → "INFO" or "ERROR")
3단계~N단계: 후속 토큰으로 분기
마지막 단계: 리프 노드의 ClusterIDs에서 유사도 매칭
```

### treeSearch: 트리 탐색

```go
// 소스: pkg/pattern/drain/drain.go:331-374
func (d *Drain) treeSearch(rootNode *Node, tokens []string, simTh float64, includeParams bool) *LogCluster {
    tokenCount := len(tokens)

    // 1단계: 토큰 수로 첫 분기
    curNode, ok := rootNode.keyToChildNode[strconv.Itoa(tokenCount)]
    if !ok {
        return nil  // 같은 토큰 수의 클러스터 없음
    }

    // 빈 로그 처리
    if tokenCount < 2 {
        return d.idToCluster.Get(curNode.clusterIDs[0])
    }

    // 2~N단계: 접두사 토큰으로 트리 탐색
    curNodeDepth := 1
    for _, token := range tokens {
        if curNodeDepth >= d.config.maxNodeDepth {
            break  // 최대 깊이 도달
        }
        if curNodeDepth == tokenCount {
            break  // 마지막 토큰
        }

        keyToChildNode := curNode.keyToChildNode
        curNode, ok = keyToChildNode[token]
        if !ok {
            // 정확한 토큰이 없으면 와일드카드 노드 시도
            curNode, ok = keyToChildNode[d.config.ParamString]
        }
        if !ok {
            return nil  // 매칭 실패
        }
        curNodeDepth++
    }

    // 마지막 단계: 유사도 기반 클러스터 매칭
    cluster := d.fastMatch(curNode.clusterIDs, tokens, simTh, includeParams)
    return cluster
}
```

### addSeqToPrefixTree: 새 클러스터 추가

```go
// 소스: pkg/pattern/drain/drain.go:433-508
func (d *Drain) addSeqToPrefixTree(rootNode *Node, cluster *LogCluster) {
    tokenCount := len(cluster.Tokens)
    tokenCountStr := strconv.Itoa(tokenCount)

    // 첫 번째 레이어: 토큰 수로 노드 생성/탐색
    firstLayerNode, ok := rootNode.keyToChildNode[tokenCountStr]
    if !ok {
        firstLayerNode = createNode()
        rootNode.keyToChildNode[tokenCountStr] = firstLayerNode
    }
    curNode := firstLayerNode

    currentDepth := 1
    for _, token := range cluster.Tokens {
        // 최대 깊이 또는 마지막 토큰에 도달하면 클러스터 ID 추가
        if (currentDepth >= d.config.maxNodeDepth) || currentDepth >= tokenCount {
            curNode.clusterIDs = append(curNode.clusterIDs, cluster.id)
            break
        }

        // 토큰에 숫자가 없으면 정확한 매칭 노드 생성
        if _, ok = curNode.keyToChildNode[token]; !ok {
            if !d.hasNumbers(token) {
                // 숫자 없음: 토큰 그대로 사용
                if len(curNode.keyToChildNode) < d.config.MaxChildren {
                    newNode := createNode()
                    curNode.keyToChildNode[token] = newNode
                    curNode = newNode
                } else {
                    // MaxChildren 초과: 와일드카드 노드로 대체
                    curNode.keyToChildNode[d.config.ParamString] = createNode()
                    curNode = curNode.keyToChildNode[d.config.ParamString]
                }
            } else {
                // 숫자 포함: 와일드카드 노드 사용
                if _, ok = curNode.keyToChildNode[d.config.ParamString]; !ok {
                    curNode.keyToChildNode[d.config.ParamString] = createNode()
                }
                curNode = curNode.keyToChildNode[d.config.ParamString]
            }
        } else {
            curNode = curNode.keyToChildNode[token]
        }
        currentDepth++
    }
}
```

### 숫자 포함 토큰의 특별 처리

```go
// 소스: pkg/pattern/drain/drain.go:510-517
func (d *Drain) hasNumbers(s string) bool {
    for _, c := range s {
        if unicode.IsNumber(c) {
            return true
        }
    }
    return false
}
```

숫자를 포함한 토큰은 IP 주소, 포트, ID 등 가변 값일 가능성이 높으므로 와일드카드 노드로 라우팅한다.

---

## 5. 유사도 계산과 클러스터 관리

### fastMatch: 최적 클러스터 찾기

```go
// 소스: pkg/pattern/drain/drain.go:377-403
func (d *Drain) fastMatch(clusterIDs []int, tokens []string, simTh float64, includeParams bool) *LogCluster {
    var matchCluster, maxCluster *LogCluster
    maxSim := -1.0
    maxParamCount := -1

    for _, clusterID := range clusterIDs {
        cluster := d.idToCluster.Get(clusterID)
        if cluster == nil {
            continue
        }
        curSim, paramCount := d.getSeqDistance(cluster.Tokens, tokens, includeParams)
        if paramCount < 0 {
            continue  // 마크된 토큰 불일치
        }
        if curSim > maxSim || (curSim == maxSim && paramCount > maxParamCount) {
            maxSim = curSim
            maxParamCount = paramCount
            maxCluster = cluster
        }
    }

    if maxSim >= simTh {
        matchCluster = maxCluster
    }
    return matchCluster
}
```

### getSeqDistance: 유사도 계산

```go
// 소스: pkg/pattern/drain/drain.go:405-431
func (d *Drain) getSeqDistance(clusterTokens, tokens []string, includeParams bool) (float64, int) {
    if len(clusterTokens) != len(tokens) {
        panic("seq1 seq2 be of same length")
    }

    simTokens := 0
    paramCount := 0
    for i := range clusterTokens {
        token1 := clusterTokens[i]
        token2 := tokens[i]

        // 마크된 토큰은 정확한 매칭 필요
        if len(token1) > 0 && token1[0] == 0 && token1 != token2 {
            return 0, -1
        }

        switch token1 {
        case d.config.ParamString:
            paramCount++     // 와일드카드 위치
        case token2:
            simTokens++      // 정확히 일치
        }
    }
    if includeParams {
        simTokens += paramCount
    }
    retVal := float64(simTokens) / float64(len(clusterTokens))
    return retVal, paramCount
}
```

### 유사도 계산 예시

```
클러스터 토큰:  "INFO"  "user"  "<_>"   "logged"  "in"  "from"  "<_>"
입력 토큰:      "INFO"  "user"  "alice" "logged"  "in"  "from"  "10.0.0.5"

비교:
  INFO  == INFO   → simTokens++  (1)
  user  == user   → simTokens++  (2)
  <_>   != alice  → paramCount++ (1)
  logged == logged → simTokens++ (3)
  in    == in     → simTokens++  (4)
  from  == from   → simTokens++  (5)
  <_>   != 10...  → paramCount++ (2)

유사도 = simTokens / totalTokens = 5 / 7 = 0.714
SimTh = 0.3이므로 0.714 >= 0.3 → 매칭 성공!
```

### createTemplate: 템플릿 갱신

```go
// 소스: pkg/pattern/drain/drain.go:519-529
func (d *Drain) createTemplate(tokens, matchClusterTokens []string) []string {
    if len(tokens) != len(matchClusterTokens) {
        panic("seq1 seq2 be of same length")
    }
    for i := range tokens {
        if tokens[i] != matchClusterTokens[i] {
            matchClusterTokens[i] = d.config.ParamString  // 다른 부분을 <_>로 대체
        }
    }
    return matchClusterTokens
}
```

```
기존 템플릿: "INFO" "user" "john" "logged" "in"
새 로그:      "INFO" "user" "alice" "logged" "in"

비교:
  INFO   == INFO   → 유지
  user   == user   → 유지
  john   != alice  → <_>로 대체
  logged == logged → 유지
  in     == in     → 유지

갱신된 템플릿: "INFO" "user" "<_>" "logged" "in"
```

### Train: 학습 메서드

```go
// 소스: pkg/pattern/drain/drain.go:210-224
func (d *Drain) Train(content string, ts int64) *LogCluster {
    if !d.limiter.Allow() {
        return nil  // 과부하 시 학습 건너뜀
    }

    d.tokens, d.state = d.tokenizer.Tokenize(content, d.tokens, d.state, linesSkipped)
    if d.tokens == nil && d.state == nil {
        return nil
    }
    return d.train(d.tokens, d.state, ts, int64(len(content)))
}
```

```go
// 소스: pkg/pattern/drain/drain.go:226-277
func (d *Drain) train(tokens []string, state any, ts int64, contentSize int64) *LogCluster {
    // 토큰 수 검증 (4~80 토큰)
    if len(tokens) < 4 {
        return nil
    }
    if len(tokens) > 80 {
        return nil
    }

    // 트리에서 기존 클러스터 탐색
    matchCluster := d.treeSearch(d.rootNode, tokens, d.config.SimTh, false)

    if matchCluster == nil {
        // 새 클러스터 생성
        d.clustersCounter++
        clusterID := d.clustersCounter
        tokens, state = d.tokenizer.Clone(tokens, state)
        matchCluster = &LogCluster{
            Tokens:      tokens,
            TokenState:  state,
            id:          clusterID,
            Size:        1,
            Stringer:    d.tokenizer.Join,
            Chunks:      Chunks{},
            Volume:      contentSize,
            SampleCount: 1,
        }
        d.idToCluster.Set(clusterID, matchCluster)
        d.addSeqToPrefixTree(d.rootNode, matchCluster)
    } else {
        // 기존 클러스터에 추가, 템플릿 갱신
        matchCluster.Tokens = d.createTemplate(tokens, matchCluster.Tokens)
        matchCluster.append(model.TimeFromUnixNano(ts), d.config.MaxChunkAge, d.config.SampleInterval)
        matchCluster.Volume += contentSize
        matchCluster.SampleCount++
        d.idToCluster.Get(matchCluster.id)  // LRU 캐시 터치
    }
    return matchCluster
}
```

---

## 6. Line Tokenizer: 로그 형식 적응

### LineTokenizer 인터페이스

```go
// 소스: pkg/pattern/drain/line_tokenizer.go:16-20
type LineTokenizer interface {
    Tokenize(line string, tokens []string, state interface{}, linesDropped *prometheus.CounterVec) ([]string, interface{})
    Join(tokens []string, state interface{}) string
    Clone(tokens []string, state interface{}) ([]string, interface{})
}
```

### 로그 형식 자동 감지

```go
// 소스: pkg/pattern/drain/metrics.go:20-31
func DetectLogFormat(line string) string {
    if len(line) < 2 {
        return FormatUnknown
    } else if line[0] == '{' && line[len(line)-1] == '}' {
        return FormatJSON
    } else if logfmtRegex.MatchString(line) {
        return FormatLogfmt
    }
    return FormatUnknown
}
```

### 토크나이저 종류

```
형식별 토크나이저:

1. punctuationTokenizer (FormatUnknown, 기본)
   ─────────────────────────────────────
   구두점을 구분자로 사용하여 토큰화
   '=', 공백, 구두점에서 분리
   '_', '-', '.', ':', '/'는 구분자에서 제외

   입력: "INFO user=john status:ok"
   토큰: ["INFO", "user", "=", "john", "status", ":", "ok"]
   상태: [공백 위치 배열]

2. logfmtTokenizer (FormatLogfmt)
   ─────────────────────────────────────
   key=value 쌍으로 파싱
   값 부분을 <_>로 자동 대체

   입력: "level=info msg=\"user logged in\" user=john"
   토큰: ["level", "=", "info", "msg", "=", "<_>", "user", "=", "<_>"]

3. jsonTokenizer (FormatJSON)
   ─────────────────────────────────────
   JSON 키를 추출하고 값을 <_>로 대체
   설정된 필드만 토큰화 대상

   입력: {"level":"info","msg":"user logged in","user":"john"}
   토큰: ["level", "=", "info", "msg", "=", "<_>", "user", "=", "<_>"]
```

### punctuationTokenizer 상세

```go
// 소스: pkg/pattern/drain/line_tokenizer.go:38-58
type punctuationTokenizer struct {
    includeDelimiters [128]rune
    excludeDelimiters [128]rune
    maxLineLength     int
}

func newPunctuationTokenizer(maxLineLength int) *punctuationTokenizer {
    var included [128]rune
    var excluded [128]rune
    included['='] = 1         // '='는 구분자로 포함
    excluded['_'] = 1         // '_'는 구분자에서 제외
    excluded['-'] = 1
    excluded['.'] = 1
    excluded[':'] = 1
    excluded['/'] = 1
    return &punctuationTokenizer{
        includeDelimiters: included,
        excludeDelimiters: excluded,
        maxLineLength:     maxLineLength,
    }
}
```

### DedupingTokenizer

```go
// Drain 생성 시 토크나이저를 DedupingTokenizer로 래핑
d.tokenizer = &DedupingTokenizer{
    LineTokenizer: tokenizer,
    dedupParam:    config.ParamString,
}
```

`DedupingTokenizer`는 연속된 `<_>` 와일드카드를 하나로 병합한다. 예를 들어 `<_><_><_>`를 `<_>`로 축소한다.

---

## 7. Limiter: 과부하 방지

### limiter 구조체

```go
// 소스: pkg/pattern/drain/limiter.go:7-12
type limiter struct {
    added         int64
    evicted       int64
    maxPercentage float64
    blockedUntil  time.Time
}
```

### Allow 메서드

```go
// 소스: pkg/pattern/drain/limiter.go:20-37
func (l *limiter) Allow() bool {
    if !l.blockedUntil.IsZero() {
        if time.Now().Before(l.blockedUntil) {
            return false  // 차단 기간 중
        }
        l.reset()  // 차단 해제
    }
    if l.added == 0 {
        l.added++
        return true
    }
    if float64(l.evicted)/float64(l.added) > l.maxPercentage {
        l.block()  // 퇴거 비율 초과 → 10분간 차단
        return false
    }
    l.added++
    return true
}
```

### 차단 로직

```go
// 소스: pkg/pattern/drain/limiter.go:49-51
func (l *limiter) block() {
    l.blockedUntil = time.Now().Add(10 * time.Minute)
}
```

### Limiter 동작 시각화

```
Limiter 상태 전이:

                  evicted/added <= 25%
            ┌──────────────────────┐
            │                      │
            ▼                      │
     ┌──────────┐          ┌──────┴──────┐
     │  Allow   │──────────│  counting   │
     │  = true  │          │ (추적 중)   │
     └────┬─────┘          └─────────────┘
          │
          │ evicted/added > 25%
          ▼
     ┌──────────┐
     │  Block   │
     │  = false │
     │ (10분)   │
     └────┬─────┘
          │
          │ 10분 경과
          ▼
     ┌──────────┐
     │  Reset   │
     │  → Allow │
     └──────────┘

의미: 클러스터 퇴거율이 25%를 넘으면 10분간 학습을 중단한다.
이는 너무 다양한 로그가 들어와 패턴이 안정되지 못하는 상황을
방지한다.
```

---

## 8. Pattern Ingester

### Config 구조체

```go
// 소스: pkg/pattern/ingester.go:36-55
type Config struct {
    Enabled               bool                  `yaml:"enabled,omitempty"`
    LifecyclerConfig      ring.LifecyclerConfig `yaml:"lifecycler,omitempty"`
    ClientConfig          clientpool.Config     `yaml:"client_config,omitempty"`
    ConcurrentFlushes     int                   `yaml:"concurrent_flushes"`
    FlushCheckPeriod      time.Duration         `yaml:"flush_check_period"`
    MaxClusters           int                   `yaml:"max_clusters,omitempty"`
    MaxEvictionRatio      float64               `yaml:"max_eviction_ratio,omitempty"`
    MetricAggregation     aggregation.Config    `yaml:"metric_aggregation,omitempty"`
    PatternPersistence    PersistenceConfig     `yaml:"pattern_persistence,omitempty"`
    TeeConfig             TeeConfig             `yaml:"tee_config,omitempty"`
    ConnectionTimeout     time.Duration         `yaml:"connection_timeout"`
    MaxAllowedLineLength  int                   `yaml:"max_allowed_line_length,omitempty"`
    RetainFor             time.Duration         `yaml:"retain_for,omitempty"`
    MaxChunkAge           time.Duration         `yaml:"max_chunk_age,omitempty"`
    PatternSampleInterval time.Duration         `yaml:"pattern_sample_interval,omitempty"`
    VolumeThreshold       float64               `yaml:"volume_threshold,omitempty"`
}
```

### 주요 설정

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `Enabled` | false | 패턴 Ingester 활성화 |
| `ConcurrentFlushes` | 32 | 스트림당 동시 플러시 수 |
| `FlushCheckPeriod` | 1m | 플러시 확인 주기 |
| `MaxClusters` | 300 | 최대 패턴 클러스터 수 |
| `MaxEvictionRatio` | 0.25 | 최대 퇴거 비율 |
| `ConnectionTimeout` | 2s | Ingester 연결 타임아웃 |
| `MaxAllowedLineLength` | 3000 | 최대 라인 길이 |
| `RetainFor` | 3h | 패턴 보존 기간 |
| `MaxChunkAge` | 1h | 최대 청크 수명 |
| `PatternSampleInterval` | 10s | 패턴 샘플 간격 |
| `VolumeThreshold` | 0.99 | 상위 99% 볼륨 패턴만 유지 |

### 검증 규칙

```go
// 소스: pkg/pattern/ingester.go:174-195
func (cfg *Config) Validate() error {
    if cfg.LifecyclerConfig.RingConfig.ReplicationFactor != 1 {
        return errors.New("pattern ingester replication factor must be 1")
    }
    if cfg.RetainFor < cfg.MaxChunkAge {
        return fmt.Errorf("retain-for (%v) must be >= chunk-duration (%v)", cfg.RetainFor, cfg.MaxChunkAge)
    }
    if cfg.MaxChunkAge < cfg.PatternSampleInterval {
        return fmt.Errorf("chunk-duration (%v) must be >= sample-interval (%v)", cfg.MaxChunkAge, cfg.PatternSampleInterval)
    }
    if cfg.VolumeThreshold < 0 || cfg.VolumeThreshold > 1 {
        return fmt.Errorf("volume_threshold (%v) must be between 0 and 1", cfg.VolumeThreshold)
    }
    return cfg.LifecyclerConfig.Validate()
}
```

### Ring 통합

Pattern Ingester는 Ring을 사용하여 스트림을 인스턴스에 분산한다. 단, `ReplicationFactor`는 반드시 1이어야 한다. 이는 패턴 감지가 상태 유지(stateful)이므로, 같은 스트림의 패턴 학습이 하나의 인스턴스에서만 이루어져야 일관된 결과를 보장하기 때문이다.

### TeeConfig: 로그 분기 설정

```go
// 소스: pkg/pattern/ingester.go:133-139
type TeeConfig struct {
    BatchSize          int           `yaml:"batch_size"`           // 기본 5000
    BatchFlushInterval time.Duration `yaml:"batch_flush_interval"` // 기본 1s
    FlushQueueSize     int           `yaml:"flush_queue_size"`     // 기본 1000
    FlushWorkerCount   int           `yaml:"flush_worker_count"`   // 기본 100
    StopFlushTimeout   time.Duration `yaml:"stop_flush_timeout"`   // 기본 30s
}
```

---

## 9. Pattern Stream: 로그 레벨별 Drain 인스턴스

### stream 구조체

```go
// 소스: pkg/pattern/stream.go:24-41
type stream struct {
    fp                 model.Fingerprint
    labels             labels.Labels
    labelsString       string
    labelHash          uint64
    patterns           map[string]*drain.Drain    // 로그 레벨별 Drain
    mtx                sync.Mutex
    logger             log.Logger
    patternWriter      aggregation.EntryWriter
    aggregationMetrics *aggregation.Metrics
    instanceID         string

    lastTS                 int64
    persistenceGranularity time.Duration
    sampleInterval         time.Duration
    patternRateThreshold   float64
    volumeThreshold        float64
}
```

### 레벨별 Drain 생성

```go
// 소스: pkg/pattern/stream.go:43-94
func newStream(...) (*stream, error) {
    patterns := make(map[string]*drain.Drain, len(constants.LogLevels))
    for _, lvl := range constants.LogLevels {
        patterns[lvl] = drain.New(instanceID, drainCfg, limits, guessedFormat, &drain.Metrics{
            PatternsEvictedTotal:  metrics.patternsDiscardedTotal.WithLabelValues(instanceID, guessedFormat, "false"),
            PatternsPrunedTotal:   metrics.patternsDiscardedTotal.WithLabelValues(instanceID, guessedFormat, "true"),
            PatternsDetectedTotal: metrics.patternsDetectedTotal.WithLabelValues(instanceID, guessedFormat),
            LinesSkipped:          linesSkipped,
            TokensPerLine:         metrics.tokensPerLine.WithLabelValues(instanceID, guessedFormat),
            StatePerLine:          metrics.statePerLine.WithLabelValues(instanceID, guessedFormat),
        })
    }

    return &stream{
        patterns:        patterns,
        volumeThreshold: volumeThreshold,
        // ...
    }, nil
}
```

### 레벨별 분리 구조

```
stream {app="myapp"}
├── patterns["info"]    → Drain 인스턴스 (INFO 로그 패턴)
├── patterns["error"]   → Drain 인스턴스 (ERROR 로그 패턴)
├── patterns["warn"]    → Drain 인스턴스 (WARN 로그 패턴)
├── patterns["debug"]   → Drain 인스턴스 (DEBUG 로그 패턴)
├── patterns["trace"]   → Drain 인스턴스 (TRACE 로그 패턴)
└── patterns["unknown"] → Drain 인스턴스 (레벨 미감지)

각 Drain 인스턴스:
  - 독립적인 파싱 트리
  - 독립적인 LRU 클러스터 캐시
  - 독립적인 Limiter
```

### Push: 로그 레벨 라우팅

```go
// 소스: pkg/pattern/stream.go:96-124
func (s *stream) Push(_ context.Context, entries []logproto.Entry) error {
    s.mtx.Lock()
    defer s.mtx.Unlock()

    for _, entry := range entries {
        if entry.Timestamp.UnixNano() < s.lastTS {
            continue  // 시간순 보장 (역순 거부)
        }

        // StructuredMetadata에서 로그 레벨 추출
        metadata := logproto.FromLabelAdaptersToLabels(entry.StructuredMetadata)
        lvl := constants.LogLevelUnknown
        if metadata.Has(constants.LevelLabel) {
            lvl = strings.ToLower(metadata.Get(constants.LevelLabel))
        }
        s.lastTS = entry.Timestamp.UnixNano()

        // 해당 레벨의 Drain에 학습
        if pattern, ok := s.patterns[lvl]; ok {
            pattern.Train(entry.Line, entry.Timestamp.UnixNano())
        } else {
            s.patterns[constants.LogLevelUnknown].Train(entry.Line, entry.Timestamp.UnixNano())
        }
    }
    return nil
}
```

### Iterator: 패턴 쿼리 응답

```go
// 소스: pkg/pattern/stream.go:127-144
func (s *stream) Iterator(_ context.Context, from, through, step model.Time) (iter.Iterator, error) {
    s.mtx.Lock()
    defer s.mtx.Unlock()

    iters := []iter.Iterator{}
    for lvl, pattern := range s.patterns {
        clusters := pattern.Clusters()
        for _, cluster := range clusters {
            if cluster.String() == "" {
                continue
            }
            iters = append(iters, cluster.Iterator(lvl, from, through, step, model.Time(s.sampleInterval.Milliseconds())))
        }
    }
    return iter.NewMerge(iters...), nil
}
```

---

## 10. 패턴 청크: 시간 기반 샘플링

### Chunks 타입

```go
// 소스: pkg/pattern/drain/chunk.go:19-23
type Chunks []Chunk

type Chunk struct {
    Samples []logproto.PatternSample
}
```

### 청크 생성

```go
// 소스: pkg/pattern/drain/chunk.go:25-33
func newChunk(ts model.Time, maxChunkAge time.Duration, sampleInterval time.Duration) Chunk {
    maxSize := int(maxChunkAge.Nanoseconds()/sampleInterval.Nanoseconds()) + 1
    v := Chunk{Samples: make([]logproto.PatternSample, 1, maxSize)}
    v.Samples[0] = logproto.PatternSample{
        Timestamp: ts,
        Value:     1,
    }
    return v
}
```

### 청크 용량 계산

```
기본 설정 기준:
  MaxChunkAge     = 1시간 = 3600초
  SampleInterval  = 10초

  maxSize = 3600 / 10 + 1 = 361개 샘플

  즉, 하나의 청크는 최대 1시간 동안의 패턴 발생 빈도를
  10초 간격으로 361개 샘플에 기록한다.
```

### Chunks.Add: 샘플 추가

```go
// 소스: pkg/pattern/drain/chunk.go:104-128
func (c *Chunks) Add(ts model.Time, maxChunkAge time.Duration, sampleInterval time.Duration) *logproto.PatternSample {
    t := TruncateTimestamp(ts, model.Time(sampleInterval.Milliseconds()))

    if len(*c) == 0 {
        *c = append(*c, newChunk(t, maxChunkAge, sampleInterval))
        return nil
    }

    last := &(*c)[len(*c)-1]

    // 같은 시간 구간이면 값 증가
    if last.Samples[len(last.Samples)-1].Timestamp == t {
        last.Samples[len(last.Samples)-1].Value++
        return nil
    }

    // 청크 용량 초과 시 새 청크 생성
    if !last.spaceFor(t, maxChunkAge) {
        *c = append(*c, newChunk(t, maxChunkAge, sampleInterval))
        return &last.Samples[len(last.Samples)-1]
    }

    // 역순 타임스탬프 거부
    if ts.Before(last.Samples[len(last.Samples)-1].Timestamp) {
        return nil
    }

    // 새 시간 구간 추가
    last.Samples = append(last.Samples, logproto.PatternSample{
        Timestamp: t,
        Value:     1,
    })
    return &last.Samples[len(last.Samples)-2]
}
```

### TruncateTimestamp: 시간 정렬

```go
// 소스: pkg/pattern/drain/chunk.go:218
func TruncateTimestamp(ts, step model.Time) model.Time { return ts - ts%step }
```

### 청크 동작 시각화

```
패턴 "INFO user <_> logged in from <_>"에 대한 청크:

시간          샘플
10:00:00      ┌─────────────────────┐
              │ ts: 10:00:00, val: 3│  3번 발생
              │ ts: 10:00:10, val: 1│  1번 발생
              │ ts: 10:00:20, val: 5│  5번 발생
              │ ...                 │
              │ ts: 10:59:50, val: 2│
11:00:00      └─────────────────────┘  ← 1시간 경과, 청크 종료
              ┌─────────────────────┐  ← 새 청크 시작
              │ ts: 11:00:00, val: 4│
              │ ...                 │
              └─────────────────────┘
```

### ForRange: 시간 범위 쿼리

```go
// 소스: pkg/pattern/drain/chunk.go:47-99
func (c Chunk) ForRange(start, end, step, sampleInterval model.Time) []logproto.PatternSample {
    // 범위 밖 체크
    if start >= end || first >= end || last < start {
        return nil
    }

    // 이진 탐색으로 시작 위치 찾기
    lo := sort.Search(len(c.Samples), func(i int) bool {
        return c.Samples[i].Timestamp >= start
    })

    // step == sampleInterval이면 그대로 반환
    if step == sampleInterval {
        return c.Samples[lo:hi]
    }

    // step이 다르면 리샘플링 (집계)
    currentStep := TruncateTimestamp(c.Samples[lo].Timestamp, step)
    aggregatedSamples := make([]logproto.PatternSample, 0, ...)
    for _, sample := range c.Samples[lo:hi] {
        // step 경계를 넘으면 새 버킷 생성
        // 같은 step 내 값은 누적
    }
    return aggregatedSamples
}
```

---

## 11. LogCluster와 Iterator

### LogCluster 구조체

```go
// 소스: pkg/pattern/drain/log_cluster.go:13-23
type LogCluster struct {
    id          int
    Size        int
    Tokens      []string
    TokenState  interface{}
    Stringer    func([]string, interface{}) string
    Volume      int64
    SampleCount int64
    Chunks      Chunks
}
```

| 필드 | 설명 |
|------|------|
| `id` | 클러스터 고유 ID (LRU 캐시 키) |
| `Size` | 이 패턴에 매칭된 총 로그 수 |
| `Tokens` | 패턴 템플릿 토큰 배열 |
| `TokenState` | 토크나이저 상태 (공백 위치 등) |
| `Stringer` | 토큰을 문자열로 합치는 함수 |
| `Volume` | 총 로그 바이트 수 |
| `SampleCount` | 샘플 수 |
| `Chunks` | 시간별 발생 빈도 청크 |

### LogCluster.String()

```go
// 소스: pkg/pattern/drain/log_cluster.go:25-30
func (c *LogCluster) String() string {
    if c.Stringer != nil {
        return c.Stringer(c.Tokens, c.TokenState)
    }
    return strings.Join(c.Tokens, " ")
}
```

### LogCluster.Prune

```go
// 소스: pkg/pattern/drain/log_cluster.go:50-54
func (c *LogCluster) Prune(olderThan time.Duration) []*logproto.PatternSample {
    prunedSamples := c.Chunks.prune(olderThan)
    c.Size = c.Chunks.size()
    return prunedSamples
}
```

### Chunks.prune: 오래된 청크 제거

```go
// 소스: pkg/pattern/drain/chunk.go:189-206
func (c *Chunks) prune(olderThan time.Duration) []*logproto.PatternSample {
    if len(*c) == 0 {
        return nil
    }
    var prunedSamples []*logproto.PatternSample
    for i := 0; i < len(*c); i++ {
        if time.Since((*c)[i].Samples[len((*c)[i].Samples)-1].Timestamp.Time()) > olderThan {
            // 오래된 청크의 모든 샘플 수집
            for j := range (*c)[i].Samples {
                prunedSamples = append(prunedSamples, &(*c)[i].Samples[j])
            }
            // 청크 제거
            *c = append((*c)[:i], (*c)[i+1:]...)
            i--
        }
    }
    return prunedSamples
}
```

---

## 12. 볼륨 임계값 기반 필터링

### filterClustersByVolume

```go
// 소스: pkg/pattern/stream.go:358-401
func filterClustersByVolume(clusters []clusterWithMeta, threshold float64) []clusterWithMeta {
    if len(clusters) == 0 || threshold == 0 {
        return []clusterWithMeta{}
    }

    // 총 볼륨 계산
    var totalVolume int64
    for _, cluster := range clusters {
        totalVolume += cluster.cluster.Volume
    }

    // 볼륨 내림차순 정렬 (in-place)
    slices.SortFunc(clusters, func(i, j clusterWithMeta) int {
        if i.cluster.Volume > j.cluster.Volume {
            return -1
        }
        if i.cluster.Volume < j.cluster.Volume {
            return 1
        }
        return 0
    })

    // 임계값까지의 클러스터만 유지
    targetVolume := int64(float64(totalVolume) * threshold)
    var cumulativeVolume int64

    var i int
    for ; i < len(clusters); i++ {
        cumulativeVolume += clusters[i].cluster.Volume
        if cumulativeVolume >= targetVolume {
            i++
            break
        }
    }

    return clusters[0:i]
}
```

### 볼륨 필터링 시각화

```
VolumeThreshold = 0.99 (상위 99%)

클러스터별 볼륨:
┌───────────────────────────────────────────┐
│ Cluster A: volume = 50000 (50%)          │ ← 유지
│ Cluster B: volume = 30000 (30%)          │ ← 유지
│ Cluster C: volume = 15000 (15%)          │ ← 유지
│ Cluster D: volume =  4000 (4%)           │ ← 유지 (누적 99%)
│ Cluster E: volume =   800 (0.8%)         │ ← 제거 (상위 99% 초과)
│ Cluster F: volume =   200 (0.2%)         │ ← 제거
└───────────────────────────────────────────┘

총 볼륨 = 100000
목표 볼륨 = 100000 * 0.99 = 99000
누적: A(50000) + B(80000) + C(95000) + D(99000) ← 여기서 커트오프

결과: 클러스터 A, B, C, D만 유지 (로그 볼륨의 99%)
클러스터 E, F는 "노이즈"로 간주하여 제거
```

### prune 메서드에서의 활용

```go
// 소스: pkg/pattern/stream.go:154-203
func (s *stream) prune(olderThan time.Duration) bool {
    s.mtx.Lock()
    defer s.mtx.Unlock()

    var allClusters []clusterWithMeta

    // 1단계: 모든 클러스터에서 오래된 샘플 정리
    for lvl, pattern := range s.patterns {
        clusters := pattern.Clusters()
        for _, cluster := range clusters {
            prunedSamples := cluster.Prune(olderThan)
            if len(prunedSamples) > 0 {
                allClusters = append(allClusters, clusterWithMeta{
                    cluster:       cluster,
                    level:         lvl,
                    drainInstance: pattern,
                    prunedSamples: prunedSamples,
                })
            }
            if cluster.Size == 0 {
                pattern.Delete(cluster)
            }
            pattern.Prune()
        }
    }

    // 2단계: 볼륨 필터링
    var clustersToWrite []clusterWithMeta
    if s.volumeThreshold > 0 && s.volumeThreshold < 1.0 && len(allClusters) > 0 {
        clustersToWrite = filterClustersByVolume(allClusters, s.volumeThreshold)
    } else {
        clustersToWrite = allClusters
    }

    // 3단계: 필터링된 클러스터만 기록
    for _, cm := range clustersToWrite {
        s.writePatternsBucketed(cm.prunedSamples, s.labels, cm.cluster.String(), cm.level)
    }

    return totalClusters == 0
}
```

---

## 13. 메트릭 생성과 모니터링

### Drain 메트릭

```go
// 소스: pkg/pattern/drain/metrics.go:33-40
type Metrics struct {
    PatternsEvictedTotal  prometheus.Counter    // LRU에서 퇴거된 패턴 수
    PatternsPrunedTotal   prometheus.Counter    // 명시적으로 정리된 패턴 수
    PatternsDetectedTotal prometheus.Counter    // 감지된 새 패턴 수
    LinesSkipped          *prometheus.CounterVec // 건너뛴 라인 수 (이유별)
    TokensPerLine         prometheus.Observer    // 라인당 토큰 수 분포
    StatePerLine          prometheus.Observer    // 라인당 상태 크기 분포
}
```

### 건너뛴 라인 이유

```go
// 소스: pkg/pattern/drain/metrics.go:13-15
const (
    TooFewTokens  = "too_few_tokens"   // 4개 미만
    TooManyTokens = "too_many_tokens"  // 80개 초과
    LineTooLong   = "line_too_long"    // 3000자 초과
)
```

### 핵심 모니터링 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `loki_pattern_ingester_patterns_detected_total` | Counter | 감지된 새 패턴 수 |
| `loki_pattern_ingester_patterns_discarded_total` | Counter | 퇴거/정리된 패턴 수 |
| `loki_pattern_ingester_lines_skipped` | Counter | 건너뛴 라인 수 (이유별) |
| `loki_pattern_ingester_tokens_per_line` | Histogram | 라인당 토큰 수 분포 |
| `loki_pattern_ingester_patterns_active` | Gauge | 현재 활성 패턴 수 |
| `loki_pattern_ingester_pattern_writes_total` | Counter | 패턴 기록 횟수 |
| `loki_pattern_ingester_pattern_bytes_written_total` | Counter | 기록된 패턴 바이트 수 |

### 패턴 시계열 샘플 생성

```go
// 소스: pkg/pattern/stream.go:226-264
func (s *stream) writePattern(ts model.Time, streamLbls labels.Labels, pattern string, count int64, lvl string) {
    service := streamLbls.Get(push.LabelServiceName)
    if service == "" {
        service = push.ServiceUnknown
    }

    newLbls := labels.FromStrings(constants.PatternLabel, service)
    newStructuredMetadata := []logproto.LabelAdapter{
        {Name: constants.LevelLabel, Value: lvl},
    }

    if s.patternWriter != nil {
        patternEntry := aggregation.PatternEntry(ts.Time(), count, pattern, streamLbls)
        s.patternWriter.WriteEntry(ts.Time(), patternEntry, newLbls, newStructuredMetadata)
    }
}
```

---

## 14. 활용 사례

### 14.1 로그 이상 탐지

```
정상 상태의 패턴 분포:
  "INFO user <_> logged in from <_>"       → 100 events/min (평균)
  "INFO request processed in <_>ms"        → 500 events/min (평균)
  "ERROR connection to <_> timed out"      →   2 events/min (평균)

이상 상태 감지:
  "ERROR connection to <_> timed out"      → 200 events/min  ← 100배 증가!
  "INFO request processed in <_>ms"        →  50 events/min  ← 90% 감소!

패턴 샘플의 시계열 데이터를 사용하여:
1. 갑작스러운 패턴 빈도 변화 감지
2. 새로운 에러 패턴 출현 감지
3. 기존 패턴의 사라짐 감지
```

### 14.2 패턴 기반 대시보드

```
Grafana 대시보드 패널 예시:

1. 패턴별 로그 볼륨 (Top 10)
   ┌─────────────────────────────────────────┐
   │ ████████████████████  50%  INFO user <_>│
   │ ████████████         30%  INFO request  │
   │ ████████            20%  ERROR conn     │
   └─────────────────────────────────────────┘

2. 시간별 패턴 발생 추이
   ┌─────────────────────────────────────────┐
   │    ╱╲    ╱╲                             │
   │   ╱  ╲  ╱  ╲   ← INFO user <_>        │
   │  ╱    ╲╱    ╲                           │
   │ ╱              ╲                        │
   │ ─────────────────── ← ERROR conn        │
   └─────────────────────────────────────────┘
   09:00  10:00  11:00  12:00

3. 새로운 패턴 알림
   "새 패턴 감지: ERROR database <_> replication lag exceeded <_>"
```

### 14.3 서비스 레벨 로그 구조 분석

```
서비스: payment-service

패턴 분석 결과:
┌──────┬──────────────────────────────────────────┬────────┐
│ 레벨 │ 패턴                                      │ 비율   │
├──────┼──────────────────────────────────────────┼────────┤
│ INFO │ "payment <_> processed for user <_>"     │ 45%    │
│ INFO │ "refund <_> issued for order <_>"        │ 15%    │
│ WARN │ "payment <_> retry attempt <_> of <_>"   │ 20%    │
│ ERROR│ "payment <_> failed: <_>"                │ 10%    │
│ ERROR│ "gateway timeout for transaction <_>"    │ 10%    │
└──────┴──────────────────────────────────────────┴────────┘

인사이트:
- 결제 실패(ERROR)가 전체의 20% → 조치 필요
- 재시도(WARN)가 20% → 안정성 문제 의심
```

---

## 15. 설계 결정 분석

### 왜 Drain 알고리즘인가?

**비교 대상**:
| 알고리즘 | 정확도 | 속도 | 온라인 | 구현 복잡도 |
|----------|--------|------|--------|-------------|
| AEL | 높음 | 느림 | 아니오 | 높음 |
| IPLoM | 중간 | 중간 | 아니오 | 중간 |
| LenMa | 중간 | 빠름 | 예 | 낮음 |
| **Drain** | **높음** | **빠름** | **예** | **중간** |

**선택 이유**:
1. 온라인 학습이 가능하여 스트리밍 로그에 적합
2. 고정 깊이 트리로 예측 가능한 메모리 사용
3. 높은 정확도와 빠른 처리 속도의 균형

### 왜 로그 레벨별 Drain 인스턴스인가?

**문제**: 같은 스트림 내에서도 INFO와 ERROR 로그는 구조가 매우 다르다. 하나의 Drain 인스턴스로 모든 레벨을 처리하면 클러스터 수가 급증하고 패턴 품질이 저하된다.

**설계 결정**: 로그 레벨별로 별도의 Drain 인스턴스를 유지하여:
- 각 레벨의 클러스터 공간을 독립적으로 관리
- 레벨별 패턴 빈도를 별도로 추적
- 레벨 간 패턴 간섭 방지

### 왜 LRU 캐시를 사용하는가?

**문제**: 무한히 많은 패턴이 생성될 수 있으므로 메모리 제한이 필요하다.

**설계 결정**: `MaxClusters = 300`의 LRU 캐시 사용
- 최근에 매칭된 패턴은 유지 (자주 나타나는 패턴은 항상 캐시에)
- 오래 매칭되지 않은 패턴은 자동 퇴거
- 퇴거 비율이 25%를 넘으면 Limiter가 학습을 10분간 중단하여 안정화

### 왜 VolumeThreshold를 사용하는가?

**문제**: 소량의 노이즈 패턴까지 영구 저장하면 저장 비용이 증가한다.

**설계 결정**: `VolumeThreshold = 0.99`로 상위 99% 볼륨의 패턴만 저장
- 전체 로그 볼륨의 99%를 차지하는 주요 패턴만 유지
- 나머지 1%의 "롱테일" 패턴은 노이즈로 간주하여 제거
- 저장 비용을 크게 절감하면서도 주요 패턴 정보를 보존

### ReplicationFactor = 1의 이유

**문제**: 패턴 감지는 상태 유지(stateful) 연산이다. 같은 스트림의 패턴을 여러 인스턴스에서 학습하면 각 인스턴스의 패턴 트리가 달라져 일관되지 않은 결과를 생성한다.

**설계 결정**: `ReplicationFactor = 1`로 강제하여 각 스트림이 정확히 하나의 Pattern Ingester에서만 학습되도록 보장한다. 이는 가용성을 다소 희생하지만, 패턴 일관성을 보장한다.

---

## 요약

Loki의 로그 패턴 감지 시스템은 다음 핵심 원칙으로 설계되었다:

1. **Drain 알고리즘**: 고정 깊이 트리와 유사도 기반 클러스터링으로 실시간 로그 패턴을 추출한다.
2. **로그 형식 적응**: JSON, Logfmt, 일반 텍스트 각각에 최적화된 토크나이저를 자동 선택한다.
3. **레벨별 분리**: 로그 레벨별로 독립적인 Drain 인스턴스를 유지하여 패턴 품질을 보장한다.
4. **과부하 방지**: LRU 캐시의 퇴거 비율 기반 Limiter로 학습 과부하를 방지한다.
5. **볼륨 기반 필터링**: 상위 99% 볼륨의 패턴만 영구 저장하여 노이즈를 제거한다.
6. **시간 기반 샘플링**: 10초 간격의 청크 샘플링으로 패턴 빈도의 시계열 데이터를 생성한다.
